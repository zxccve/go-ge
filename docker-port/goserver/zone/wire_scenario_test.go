package zone

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// =================================================================================
// THE MOVEMENT RULES, EXERCISED BY A CLIENT OF OUR OWN OVER THE REAL SOCKET
// =================================================================================
//
// WHY THIS FILE EXISTS. Four builds in a row went to the owner for a verdict, and each
// one came back with a different defect: a walk that never stopped (R95), a swing 68
// units behind the one before it (R111), a 77-unit leg reported at `elapsed 54453ms`
// (R112), a swing drawn ~600 units off the segment its own line named (R104). Every one
// of them was found by his hands, on his screen, after a build had been handed over.
// The unit tests existed - what they could not say is that the RULES HOLD ON THE WIRE:
// through Serve, a real socket, the real clock and the real frames.
//
// So this is a CLIENT, and it drives the scenario the owner ran by hand:
//
//	(a) a GROUND WALK that needs a BENT route - a wall on the straight line, 700 units
//	    between the two ends, ~1100 units of route around it (R110);
//	(b) an ATTACK pressed from 2500 units out, so the zone walks the character in and
//	    the repeat fires five hits;
//	(c) a MOVE 1000 units AWAY from the monster in the middle of the repeat, then two
//	    seconds of nothing. SINCE 2026-11-02 THAT MOVE ENDS THE REPEAT: the report is the
//	    order of the character it names, for every slot of the team and not only the
//	    driven one (zone\zone.go's MOVE_PC block).
//
// AND THEN IT ASSERTS THE INVARIANTS OUT OF THE HANDLER'S OWN LOG, not out of the
// packets. A packet says what the client was told; a `SWING POS` line says WHICH LEG the
// number was read from and HOW OLD that leg was - and every one of the four defects
// above lived in that difference.
//
// THE NUMBERS ARE READ BACK OUT OF THE LOG, never trusted from the constants below: the
// press point, the ring the approach walked to, the walk-away destination and the walk
// speed all come from the handler's own lines, so a fixture that drifted fails on its own
// arithmetic instead of passing on a stale number.
//
// THE ONE LEG THAT BENDS IS THE GROUND WALK (a). Every `SWING POS` line must name a
// segment its point lies on, and a swing on a bent leg lies on the POLYLINE, not on the
// chord the line names - so every swing in this scenario happens on a straight leg (the
// approach, and the walk-away, whose Route answers "no route"), and the bent leg is
// asserted where the bend is visible: on the `MOVE_PC` line's own "leg N u (straight
// M u)".
//
// THE SIX RULES (the PM's card, movement piece 2e):
//
//	rule 1  every SWING POS line names a segment, and its point lies on it   (R104)
//	rule 2  consecutive swings on one leg are at most speed x dt + 1 apart,
//	        and never backwards along it                                    (R104/R111)
//	rule 3  no SWING POS REWIND line in the whole scenario                    (R111)
//	rule 4  no SWING POS elapsed larger than its leg's own walk time + 1 s    (R112)
//	rule 5  the walk-away ends the repeat with ONE move-order line, after the
//	        five hits it already made and with no sixth, and no walk back  (2026-11-02)
//	rule 6  a repeat ends exactly once: the later stance change writes no
//	        second ATTACK REPEAT ENDED line                                    (R94)
//
// NOTHING HERE CHANGES BEHAVIOUR. It presses, walks, changes stance and reads the log,
// exactly as the client does; every rule above is already written in production code and
// this file only measures it.

// The scenario's own numbers. They are the FIXTURE, not the assertions: each one is
// checked against the log before a rule is checked against it.
const (
	wireGroundLeg  = 700.0         // the ground walk's straight distance (the card asks 600+)
	wireWalkAway   = 1000.0        // the walk-away leg (the card asks 900+)
	wirePressGap   = 2500.0        // the press, from the monster's centre: past MaxR 1500
	wireStanceID   = uint32(30220) // Construction: MinR 1 .. MaxR 1500, Single, ATTACKER
	wireIdleStance = uint32(30210) // InstallTrap: job 13's login stance, NOT attacker AI
	wireStepTo     = 510.0         // where the monster steps to, from the body: edge 400
)

// legSpeedRE reads the walk speed the zone prints on its own walk lines ("speed %.0f").
// It is not a second SWING POS parser: the speed is the number the body is DRAWN at, and
// both checks that need it (rules 2 and 4) are arithmetic between the logged milliseconds
// and that number.
var legSpeedRE = regexp.MustCompile(`speed (\d+)`)

// legSpeed is `speed N` out of one of the zone's own walk lines.
func legSpeed(t *testing.T, line string) float64 {
	t.Helper()
	m := legSpeedRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("this walk line names no speed, so its swings cannot be timed: %q", line)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		t.Fatalf("this walk line names speed %q, which is not a speed: %q", m[1], line)
	}
	return v
}

// legAndStraight reads the two distances the zone prints on a client's own walk line:
// "leg %.0f u (straight %.0f u)". The first is the route it walks and times, the second
// the chord between the ends - a bent walk is the one where they differ.
// bodyWasAt reads the `body was at (x, y)` point out of a MOVE_PC line - the position the
// zone itself measured when the report arrived.
func bodyWasAt(t *testing.T, line string) [3]int32 {
	t.Helper()
	m := regexp.MustCompile(`body was at \((-?\d+), (-?\d+)\)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the MOVE_PC line carries no body point: %s", line)
	}
	x, err1 := strconv.Atoi(m[1])
	y, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		t.Fatalf("the body point is unreadable (%q, %q): %s", m[1], m[2], line)
	}
	return [3]int32{int32(x), int32(y), 0}
}

func legAndStraight(t *testing.T, line string) (leg, straight float64) {
	t.Helper()
	i := strings.Index(line, "leg ")
	if i < 0 {
		t.Fatalf("this walk line names no leg length: %q", line)
	}
	var n int
	var err error
	if n, err = fmt.Sscanf(line[i:], "leg %f u (straight %f u)", &leg, &straight); err != nil ||
		n != 2 {
		t.Fatalf("this walk line does not carry its two distances (%d field(s), %v): %q",
			n, err, line)
	}
	return leg, straight
}

// hitCount is the number a repeat's own ending line states: "after %d hit(s)".
func hitCount(t *testing.T, line string) int {
	t.Helper()
	i := strings.Index(line, "after ")
	if i < 0 {
		t.Fatalf("the repeat's ending line names no hit count: %q", line)
	}
	var hits int
	if _, err := fmt.Sscanf(line[i:], "after %d hit(s)", &hits); err != nil {
		t.Fatalf("the repeat's ending line does not carry its hit count (%v): %q", err, line)
	}
	return hits
}

// swingLine is the i-th (0-based) SWING POS line of one log, trimmed, for an error
// message that quotes the line a failing number came from.
func swingLine(t *testing.T, text string, i int) string {
	t.Helper()
	lines := swingLogLines(text)
	if i < 0 || i >= len(lines) {
		t.Fatalf("swing %d does not exist among %d SWING POS line(s)", i+1, len(lines))
	}
	return lines[i]
}

// swingLogLines is every SWING POS line in a log - the alarm's REWIND line excluded, so
// an index into this list and an index into swingLinesOf's answer are the same swing.
func swingLogLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "SWING POS") && !strings.Contains(line, "REWIND") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// waitSwings waits until the log carries n SWING POS lines and answers them, parsed by
// the tree's own parser. It answers what there is after `max` too, so the caller can say
// how many arrived instead of timing out silently.
func waitSwings(t *testing.T, logs *syncBuffer, n int, max time.Duration) []swingRead {
	t.Helper()
	deadline := time.Now().Add(max)
	for {
		if len(swingLogLines(logs.String())) >= n {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return swingLinesOf(t, logs.String())
}

// waitSwingOnLeg waits for a swing whose segment ENDS on `dest` and whose point is
// strictly between the two ends - a swing fired while the body was WALKING that leg, not
// one fired at the destination once the leg was over. It answers false when the cadence
// never landed inside the leg, which is a fact about this fixture and is reported as one.
func waitSwingOnLeg(t *testing.T, logs *syncBuffer, dest [3]int32,
	max time.Duration) (swingRead, bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for {
		for _, s := range swingLinesOf(t, logs.String()) {
			if s.dest != dest {
				continue
			}
			if cross, along := onSegment(s.pos, s.from, s.dest); cross <= 2 && along > 0 &&
				along < 1 {
				return s, true
			}
		}
		if time.Now().After(deadline) {
			return swingRead{}, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tailLines is the last n lines of a log, for a failure message that stays readable.
func tailLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestTheWireScenario is the client: login, bent ground walk, attack from out of reach,
// walk away mid-repeat, and the six rules read back out of the handler's log.
func TestTheWireScenario(t *testing.T) {
	spawn := [3]int32{1000, 1000, 1}
	groundDest := [3]int32{spawn[0], spawn[1] + int32(wireGroundLeg), spawn[2]}

	// THE WALL. The straight line from the spawn to the destination is blocked, so the
	// route runs 200 east, 700 north and 200 west back onto the destination: ~1100 units
	// for a 700-unit gap. This is the map of this test - the zone's own planner is a
	// Handler.Route, and this test hands it the geometry a map would hand it.
	wallRoute := [][3]int32{
		spawn,
		{spawn[0] + 200, spawn[1], spawn[2]},
		{spawn[0] + 200, groundDest[1], groundDest[2]},
		groundDest,
	}

	// THE MONSTER, straight out on -y: wirePressGap from the spawn, so a press there is
	// past MaxR 1500 and the zone has to walk the character in.
	monsterAt := [3]int32{spawn[0], spawn[1] - int32(wirePressGap), spawn[2]}

	// ONE LEADER AND ONE FOLLOWER, and WHICH SLOT ATTACKS IS LOAD-BEARING. Slot 0 is the
	// character the player drives; slot 1 is a follower. UNTIL 2026-11-02 only slot 0's
	// reports could end an engagement, and this scenario leaned on that: it swung as the
	// follower so a move report from the follower would be ignored and the repeat would keep
	// fighting. That was the owner's bug - moving must stop the character whose body moved,
	// whichever slot it is - so the card now expects slot 1's own order to end slot 1's
	// engagement (part (c) below).
	team := []Character{
		{Handle: 0x40000061, Name: "Lead", Job: 1, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10010, Position: spawn},
		{Handle: 0x40000062, Name: "Fist", Job: 13, Level: 100, HP: 500, MaxHP: 900,
			Stance: wireIdleStance, Position: spawn},
	}

	logs := &syncBuffer{}
	h, _, client, table := tickWalkServer(t, spawn, team, func(h *Handler) {
		h.Log = log.New(logs, "", 0)
		// THE WALK LINE IS A `-diag-events` DIAGNOSTIC NOW, AND THIS TEST READS THE LINE
		// ITSELF (the goal and the leg it names): turn it back on, or the scenario would
		// be reading a log the default run no longer writes.
		h.DiagEvents = true

		// THE REAL TABLES, because the reach, the cadence and the login stance are theirs
		// (a stance is picked from job + hands at login, which is why this scenario changes
		// it over the wire instead of writing it into the team above).
		st, err := LoadStances(stanceDir())
		if err != nil {
			t.Skipf("stance tables not readable (%v)", err)
		}
		h.Stances = st
		h.Monsters = newMonsters(map[uint32]MonsterRow{
			scarecrow().ClassID: {ID: scarecrow().ClassID, Name: "Monster_Hersu01",
				Size: "S", Radius: ringTargetBody},
		})
		// THE CHARACTER'S OWN SHEET, the same two tables the attack fixtures install
		// (zone\skill_cast_test.go, castZone): without them the job has no base stats, the
		// AGI term falls back to the 1.5 stand-in and every delay is stretched by half -
		// 2475 ms instead of this job's 1320 - which is longer than the walk-away leg and
		// would leave rule 5 with no swing to look at.
		if js, jerr := LoadJobStats("../../data/job_stats.json"); jerr == nil {
			old := jobStats
			SetJobStats(js)
			t.Cleanup(func() { jobStats = old })
		}
		if is, ierr := LoadItemStats("../../data/item_stats.json"); ierr == nil {
			old := itemStats
			SetItemStats(is)
			t.Cleanup(func() { itemStats = old })
		}

		sc := scarecrow()
		sc.X, sc.Y, sc.Cell = monsterAt[0], monsterAt[1], monsterAt[2]
		h.spawnedMonster = []Monster{sc}
		// A ledger with room for a whole fight: this scenario is about WHERE the swings
		// are, and a dead monster would end the repeat for a reason of its own.
		h.monsterHP = map[uint32]float64{capturedTarget: 1e9}

		// The approach's own planner: arithmetic, so the ring the walk aims at is the
		// stance's own stand point and not a mesh's opinion of it.
		h.Plan = directPlan
		// THE MAP. Only the ground walk bends; a client's walk-away leg plans "no route"
		// and therefore walks the straight line its `SWING POS` line names.
		h.Route = func(from, to [3]int32) ([][3]int32, bool) {
			if to == groundDest {
				return wallRoute, true
			}
			return nil, false
		}
	})

	logInAs(t, client, table)

	// ------------------------------------------------------------------- the stance
	// THE FIGHTING STANCE, over the wire. Job 13 logs in bare-handed in 30210
	// (InstallTrap, MaxR 100, AI not the attacker's); 30220 is the same job's own attacker
	// stance with MinR 1 .. MaxR 1500 - a band wide enough that a 1000-unit walk-away can
	// still end INSIDE it, which is what rule 5 needs.
	sendWire(t, client, changeStanceFrame(1, wireStanceID), table)
	text := waitForLine(t, logs, "CHANGE_STANCE commander", 3*time.Second)
	if strings.Contains(text, "CHANGE_STANCE refused") {
		t.Fatalf("the zone refused the fighting stance - this scenario cannot start:\n%s",
			lastLineWith(text, "CHANGE_STANCE"))
	}
	if line := lastLineWith(text, "CHANGE_STANCE commander"); !strings.Contains(line,
		"-> 30220") {
		t.Fatalf("the zone did not stand the fighter in 30220: %s", line)
	}

	// ------------------------------------------------------------ (a) the ground walk
	sendWire(t, client, moveTo(0, groundDest).Data, table)
	text = waitForLine(t, logs, "MOVE_PC char 0 ->", 3*time.Second)
	walk := lastLineWith(text, "MOVE_PC char 0 ->")
	leg, straight := legAndStraight(t, walk)
	// THE BEND, as the zone itself printed it: the route is materially longer than the
	// chord. A zone that walked (or timed) the straight line prints two equal numbers
	// here, which is R110's defect exactly.
	if leg < straight+300 {
		t.Fatalf("the ground walk reports leg %g u for a straight distance of %g u - the "+
			"route does not bend round the wall, so there is nothing here to prove: %s",
			leg, straight, walk)
	}
	if straight < 600 {
		t.Fatalf("the ground walk is only %g units end to end (the card asks 600+): %s",
			straight, walk)
	}
	t.Logf("ground walk: leg %g u for a straight %g u - the route bends round the wall",
		leg, straight)

	// ------------------------------------------------ (b) the attack, from out of reach
	sendWire(t, client, attackFrame(1, capturedTarget), table)
	text = waitForLine(t, logs, "SENT walk char 1 ->", 3*time.Second)
	if !strings.Contains(text, "SENT walk char 1 ->") {
		t.Fatalf("the press from %g units out did not walk the character in:\n%s",
			wirePressGap, tailLines(text, 30))
	}

	// THE APPROACH'S OWN LEG, out of the first swing's line: `from` is where the press was
	// made, `dest` is the ring the walk aimed at. Both are the zone's numbers.
	swings := waitSwings(t, logs, 1, 5*time.Second)
	approach := swings[0]
	if approach.from == approach.dest {
		t.Fatalf("the first swing names no leg at all (%v -> %v):\n%s", approach.from,
			approach.dest, tailLines(logs.String(), 30))
	}

	// THE FIXTURE'S OWN ARITHMETIC, before a rule is asserted against it.
	minR, maxR := stanceReach(t, h, wireStanceID)
	if minR != 1 || maxR != 1500 {
		t.Fatalf("stance %d is MinR %g .. MaxR %g in datatable_stance.xml, and this "+
			"scenario is written against 1 .. 1500", wireStanceID, minR, maxR)
	}
	if gap, in := attackRangeAllowed(approach.from, monsterAt, minR, maxR, PCBodyRadius,
		ringTargetBody); in {
		t.Fatalf("the press point (%d, %d) is %g of clear ground from the monster - inside "+
			"MinR %g .. MaxR %g - so it would not walk in at all", approach.from[0],
			approach.from[1], gap, minR, maxR)
	}
	ring := approach.dest
	ringGap, ringIn := attackRangeAllowed(ring, monsterAt, minR, maxR, PCBodyRadius,
		ringTargetBody)
	if !ringIn {
		t.Fatalf("the ring the approach walked to (%d, %d) is %g of clear ground, which is "+
			"NOT inside MinR %g .. MaxR %g - the arrival could not fire there", ring[0],
			ring[1], ringGap, minR, maxR)
	}

	// FIVE HITS: the press's arrival plus four swings of the repeat (the card asks four).
	swings = waitSwings(t, logs, 5, 14*time.Second)
	if len(swings) < 5 {
		t.Fatalf("%d swing(s) after the walk in - the repeat did not fire the four swings "+
			"this card needs:\n%s", len(swings), tailLines(logs.String(), 40))
	}

	// THE MONSTER STEPS IN, so the body the player is about to pull away sits INSIDE the
	// stance's band with room on both sides: at edge 400 the body can walk 1000 units away
	// from the target and still be inside MaxR 1500, which is what rule 5 needs.
	stepped := pointTowards(ring, monsterAt, wireStepTo)
	placeTargetAt(t, h, stepped)
	if gap, in := attackRangeAllowed(ring, stepped, minR, maxR, PCBodyRadius,
		ringTargetBody); !in {
		t.Fatalf("after the monster stepped in the body is %g of clear ground - not inside "+
			"MinR %g .. MaxR %g, so the repeat would step back instead of swing", gap, minR,
			maxR)
	}

	// ----------------------------------------------------- (c) the walk AWAY, mid-repeat
	away := pointTowards(stepped, ring, flatDist(stepped, ring)+wireWalkAway)
	if d := flatDist(ring, away); math.Abs(d-wireWalkAway) > 2 {
		t.Fatalf("the walk-away leg is %g units, want %g (the card asks 900+)", d,
			wireWalkAway)
	}
	awayGap, awayIn := attackRangeAllowed(away, stepped, minR, maxR, PCBodyRadius,
		ringTargetBody)
	if !awayIn {
		t.Fatalf("the walk-away destination (%d, %d) is %g of clear ground from the "+
			"monster - outside MinR %g .. MaxR %g, so the ending here would be the drag "+
			"hysteresis and not the player's order, and this scenario would prove nothing",
			away[0], away[1], awayGap, minR, maxR)
	}

	sendWire(t, client, moveTo(1, away).Data, table)
	walksBefore := countLines(logs.String(), "SENT walk char 1 ->")
	text = waitForLine(t, logs, "MOVE_PC char 1 ->", 3*time.Second)
	moved := lastLineWith(text, "MOVE_PC char 1 ->")
	movedAt := time.Now()
	walkSpeed := legSpeed(t, moved)
	// The leg the client's report opened must be the one the zone draws: its own line says
	// how long it is, and the fixture says what it should be.
	// THE LEG IS MEASURED FROM WHERE THE BODY ACTUALLY STANDS, which since R132 is the point
	// the first hit was fired from and not the ring the approach aimed at (the reference
	// STOPS the body when the attack starts, so the walk is closed short of its goal). The
	// line prints that point itself: `body was at (x, y)`.
	bodyAt := bodyWasAt(t, moved)
	if legU, straightU := legAndStraight(t, moved); math.Abs(legU-straightU) > 1 ||
		math.Abs(straightU-flatDist(bodyAt, away)) > 2 {
		t.Fatalf("the walk-away leg is %g u (%g straight) from a body at %v to %v (%g): %s",
			legU, straightU, bodyAt, away, flatDist(bodyAt, away), moved)
	}

	// RULE 5: THE WALK-AWAY IS A MOVE ORDER AND ENDS THE ENGAGEMENT (2026-11-02). The
	// report is char 1's own, so the repeat ends on the tick that follows it - the swing
	// already due would still land, and the cadence is not due here (the fifth hit was one
	// ASPD earlier), so the ending carries the five hits the card asks for and no sixth.
	//
	// THIS BLOCK USED TO ASK FOR A SWING ON THE WALK-AWAY LEG, and it got one, because the
	// zone ignored a follower's report: the repeat kept hitting a body the player had just
	// walked away ("once i start moving, 3 continue on attacking ... although i already
	// cancel that by moving"). No swing follows the player now, and that is the rule.
	endedByMove := waitForLine(t, logs, "ATTACK REPEAT ENDED", 3*time.Second)
	endedLine := lastLineWith(endedByMove, "ATTACK REPEAT ENDED")
	if !strings.Contains(endedLine, "move order") {
		t.Fatalf("the walk-away did not end the repeat on the move order: %s", endedLine)
	}
	if n := countLines(endedLine, "ATTACK REPEAT ENDED"); n != 1 {
		t.Fatalf("%d ATTACK REPEAT ENDED line(s) so far, want exactly 1: %s", n, endedLine)
	}

	// AND THE ORDER DID NOT SEND A WALK BACK TOWARD THE TARGET: the count of the zone's own
	// walk lines for char 1 is the same after the ending as it was when the order arrived -
	// the report's own leg is the last walk of this fight.
	if walks := countLines(logs.String(), "SENT walk char 1 ->"); walks != walksBefore {
		t.Fatalf("the zone sent %d walk(s) for char 1 after the move order (it had sent %d by "+
			"the time the report arrived) - walking the body back toward the target is "+
			"exactly what an order forbids:\n%s", walks-walksBefore, walksBefore,
			tailLines(logs.String(), 20))
	}

	// AND NO SWING FOLLOWED THE PLAYER ONTO THAT LEG - the old behaviour, and the bug.
	if follow, ok := waitSwingOnLeg(t, logs, away, 300*time.Millisecond); ok {
		t.Fatalf("a swing followed the player onto the walk-away leg (%v -> %v at %dms) "+
			"after the order ended the repeat:\n%s", follow.from, follow.dest,
			follow.elapsed, tailLines(logs.String(), 20))
	}

	// TWO SECONDS OF NOTHING, exactly what the card asks for after the move.
	if left := time.Until(movedAt.Add(2 * time.Second)); left > 0 {
		time.Sleep(left)
	}

	// ------------------------------- (6) a repeat that has already ended ends only once
	// THE STANCE CHANGE COMES AFTER THE ORDER ENDED THE REPEAT, so it must be a no-op for a
	// record that is no longer there: no second ENDED line, and nothing more on the wire.
	sendWire(t, client, changeStanceFrame(1, wireIdleStance), table)
	deadline := time.Now().Add(3 * time.Second)
	for countLines(logs.String(), "CHANGE_STANCE commander") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	text = logs.String()
	if n := countLines(text, "ATTACK REPEAT ENDED"); n != 1 {
		t.Fatalf("%d ATTACK REPEAT ENDED line(s), want exactly 1 - the repeat was already "+
			"over when the stance changed:\n%s", n, tailLines(text, 40))
	}

	// =========================================================== THE RULES, from the log
	text = logs.String()
	swings = swingLinesOf(t, text)

	// RULE 1: every SWING POS line names a segment, and its point lies on it.
	for i, s := range swings {
		cross, along := onSegment(s.pos, s.from, s.dest)
		if cross > 2 {
			t.Fatalf("swing %d/%d: %v is %g units OFF the segment (%v -> %v) its own line "+
				"names - the client hard-sets its actor to that point (GeActor.cpp:300-309):"+
				"\n%s", i+1, len(swings), s.pos, cross, s.from, s.dest,
				swingLine(t, text, i))
		}
		if along < 0 || along > 1 {
			t.Fatalf("swing %d/%d: %v is %g of the way along (%v -> %v), i.e. past its own "+
				"ends:\n%s", i+1, len(swings), s.pos, along, s.from, s.dest,
				swingLine(t, text, i))
		}
	}

	// RULE 2: consecutive swings on ONE leg are at most speed x dt + 1 apart, and never run
	// backwards along it. A new leg is a new record (a new walk, a new report), so it
	// starts its own pair - the same rule legDoesNotRewind applies.
	var largest float64
	var largestGap time.Duration
	var pairs int
	for i := 1; i < len(swings); i++ {
		a, b := swings[i-1], swings[i]
		if a.from != b.from || a.dest != b.dest {
			continue // a different leg: not a pair
		}
		pairs++
		if b.elapsed < a.elapsed {
			t.Fatalf("swing %d runs BACKWARDS on its own leg: %dms after %dms (%v -> %v):"+
				"\n%s", i+1, b.elapsed, a.elapsed, b.from, b.dest, swingLine(t, text, i))
		}
		dt := float64(b.elapsed-a.elapsed) / 1000
		gap := flatDist(a.pos, b.pos)
		if gap > walkSpeed*dt+1 {
			t.Fatalf("two swings %g s apart on one leg are %g units apart; at %g u/s the body "+
				"can only have walked %g:\n%s", dt, gap, walkSpeed, walkSpeed*dt,
				swingLine(t, text, i-1)+"\n"+swingLine(t, text, i))
		}
		if gap > largest {
			largest, largestGap = gap, time.Duration(b.elapsed-a.elapsed)*time.Millisecond
		}
	}
	if pairs == 0 {
		t.Fatalf("%d swing(s) and not one pair on the same leg - this scenario measured "+
			"nothing about how a body moves along a leg: %v", len(swings),
			swingLegNames(t, text))
	}

	// RULE 3: the alarm never sounded.
	if line := lastLineWith(text, "SWING POS REWIND"); line != "" {
		t.Fatalf("the rewind alarm fired - one leg was reported backwards, so some caller "+
			"handed in a time that does not belong to it: %s", line)
	}

	// RULE 4: no leg is reported older than its own walk time plus one second.
	for i, s := range swings {
		chord := flatDist(s.from, s.dest)
		own := chord / walkSpeed * 1000 // every swing leg in this scenario is straight
		if float64(s.elapsed) > own+1000 {
			t.Fatalf("swing %d/%d reports `elapsed %dms` on a %g-unit leg whose own walk "+
				"time is %gms - a finished leg is still being timed:\n%s", i+1, len(swings),
				s.elapsed, chord, own, swingLine(t, text, i))
		}
	}

	// RULE 6's numbers: the repeat's own count, and the swings that produced it.
	ended := lastLineWith(text, "ATTACK REPEAT ENDED")
	hits := hitCount(t, ended)
	if hits < 5 {
		t.Fatalf("the repeat names %d hit(s) - the card asks four swings beyond the press's "+
			"own: %s", hits, ended)
	}
	// THE ORDER ENDED IT BEFORE THE NEXT CADENCE, so the five hits it had made are all of
	// them: a sixth would mean the record outlived the order by a whole ASPD.
	if hits != 5 {
		t.Fatalf("the repeat names %d hit(s) - the order arrived one cadence after the "+
			"fifth, so it must end at 5 and swing no sixth: %s", hits, ended)
	}
	if hits != len(swings) {
		t.Fatalf("the repeat says %d hit(s) and the log carries %d SWING POS line(s) - the "+
			"count and the swings are not the same fight: %s", hits, len(swings), ended)
	}

	// -------------------------------------------------------------- the evidence, named
	t.Logf("scenario: %d corner(s) round the wall, %d swing(s) for char 1, %d hit(s) on the "+
		"repeat's own line", len(wallRoute)-1, len(swings), hits)
	t.Logf("ground walk: leg %g u round the wall for a %g u gap", leg, straight)
	t.Logf("approach: %v -> %v, and the ring is %g of clear ground from the monster",
		approach.from, ring, ringGap)
	t.Logf("the walk-away: %v -> %v (%g units, %g of clear ground from the monster) - it "+
		"ENDED the repeat and no swing followed", ring, away, flatDist(ring, away), awayGap)
	t.Logf("largest swing-to-swing gap on ONE leg: %g units (%s apart), walk speed %g u/s",
		largest, largestGap.Round(time.Millisecond), walkSpeed)
	for i, s := range swings {
		if i >= 12 {
			t.Logf("  ... %d more swing(s)", len(swings)-12)
			break
		}
		t.Logf("  swing %d/%d: pos %v on %v -> %v, elapsed %dms", i+1, len(swings), s.pos,
			s.from, s.dest, s.elapsed)
	}
	for name, n := range swingLegNames(t, text) {
		t.Logf("  leg %s: %d swing(s)", name, n)
	}
	t.Logf("repeat ended: %s", ended)
}

// swingLegNames groups the scenario's swings by the leg each line names, in the order the
// legs first appear, so a reader can see which leg every swing used. It groups the
// TREE'S OWN parse (swingLinesOf), never a second reader of the same line.
func swingLegNames(t *testing.T, text string) map[string]int {
	t.Helper()
	used := map[string]int{}
	for _, s := range swingLinesOf(t, text) {
		used[fmt.Sprintf("(%d, %d) -> (%d, %d)", s.from[0], s.from[1], s.dest[0],
			s.dest[1])]++
	}
	return used
}
