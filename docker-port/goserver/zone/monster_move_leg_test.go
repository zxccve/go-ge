package zone

// THE LEG A SCRIPT MOVE ORDER CARRIES - card t_16ab5afc.
//
// THE MEASURED GAP. `fld_gig`, `-monai script`, zone up 2026-11-11: a wandering body was
// ordered to walk about once a second (1,245 `MONSTER MOVE` lines in 40 s for 61 bodies,
// every one at the row's 217 u/s) and the SERVER's copy of the body never left its birth
// point. Three consequences, and the three tests below are those three:
//
//	(a) the body's point must advance at the row's MSPD  -> TestScriptMoveLegAdvancesTheBodyAtTheRowsMSPD
//	(b) the script must see the walk IN FLIGHT           -> TestIsEnableMoveSaysNoWhileAScriptWalkIsInFlight
//	    (`IsEnableMove` 'NO' while it walks, 'YES' once it arrives - the wait loop in
//	    mon_ai_lib.scp:72-83 is what turns that answer into the script's own cadence)
//	(c) an unfinished leg is not replaced                -> TestScriptMoveLegIsNotRedrawnWhileItIsInFlight
//	    and the field itself stops flooding              -> TestMonsterAIWanderFloodStopsAndTheBodyWalks
//
// All four set `MonsterNow` (zone\zone.go) so the leg's clock and the tick's clock are the
// same simulated instant: the think turn's own `sleep`s are simulated seconds, and a leg
// stamped with the wall clock would begin in the future and never advance.

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// monsterLegZone is the order fixture with the script mode ON (the only mode a script move
// order can exist in) and a simulated clock the test moves by hand.
func monsterLegZone(t *testing.T, clock *time.Time) (*Handler, *player, Monster, *bytes.Buffer) {
	t.Helper()
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.MonsterNow = func() time.Time { return *clock }
	return h, victim, mon, logs
}

// rosterPointOf answers where the zone's roster says this handle stands. It is the point
// every reader of `runtimeMonsters` sees - the swing's reach, the sight ring, the next 3564
// and the script's own `self.X`/`self.Y`.
func rosterPointOf(t *testing.T, h *Handler, handle uint32) [3]int32 {
	t.Helper()
	for _, m := range h.runtimeMonsters() {
		if m.Handle == handle {
			return [3]int32{m.X, m.Y, m.Cell}
		}
	}
	t.Fatalf("handle 0x%08x is not on the roster", handle)
	return [3]int32{}
}

// TestScriptMoveLegAdvancesTheBodyAtTheRowsMSPD is pass condition (a): after ONE script
// `Move`, the body's own point advances along the leg at its row's `SCR_Get_MON_MSPD`
// (143 for class 11591 Monster_Bigfoot), and it stops on the goal.
//
// RED WITHOUT THE LEG: the roster stays on the point the body was spawned at for the whole
// walk - which is exactly what the live run showed, and why the 3550 flood was invisible
// from this side.
func TestScriptMoveLegAdvancesTheBodyAtTheRowsMSPD(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h, victim, mon, logs := monsterLegZone(t, &clock)

	// 1,000 units due north: the row's speed is the only thing that can make y advance.
	h.MoveMonster(mon.Handle, 0, 1000, 26)
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 for one order, want 1:\n%s", n, logs.String())
	}
	start := rosterPointOf(t, h, mon.Handle)
	if start != [3]int32{0, 0, 29} {
		t.Fatalf("the body stood at %v when the order was given, want (0, 0, 29) - a "+
			"binding must not move it", start)
	}

	// 143 u/s: 143, 286 and 429 units after 1, 2 and 3 seconds.
	for _, sec := range []float64{1, 2, 3} {
		clock = clock.Add(time.Second)
		h.monsterMoveTick(clock)
		at := rosterPointOf(t, h, mon.Handle)
		want := 143 * sec
		if math.Abs(float64(at[1])-want) > 1.5 || at[0] != 0 || at[2] != 29 {
			t.Fatalf("after %g s the body stands at %v, want y %g (x 0, cell 29) - the "+
				"leg is drawn at the order's own SCR_Get_MON_MSPD 143 u/s:\n%s",
				sec, at, want, logs.String())
		}
	}
	// The walk is 1,000 units: 6.99 s. One second before that it is still short of the
	// goal; one second after, it is ON it and stands still.
	clock = time.Unix(1_700_000_000, 0).Add(5999 * time.Millisecond)
	h.monsterMoveTick(clock)
	if at := rosterPointOf(t, h, mon.Handle); at[1] >= 1000 {
		t.Fatalf("the body reached y %d before its 6.99 s walk was up (%v)", at[1], at)
	}
	clock = time.Unix(1_700_000_000, 0).Add(8 * time.Second)
	h.monsterMoveTick(clock)
	if at := rosterPointOf(t, h, mon.Handle); at != [3]int32{0, 1000, 29} {
		t.Fatalf("after the walk the body stands at %v, want the goal (0, 1000, 29)", at)
	}
	// AND IT STAYS THERE: the leg is walked out, so a later pass writes nothing.
	h.monsterMoveTick(clock.Add(time.Second))
	if at := rosterPointOf(t, h, mon.Handle); at != [3]int32{0, 1000, 29} {
		t.Fatalf("the body moved past its goal to %v", at)
	}
}

// TestIsEnableMoveSaysNoWhileAScriptWalkIsInFlight is pass condition (b), and it is the
// card's flood in one assertion: `IsEnableMove` is `MS_STOP`/`MS_INITIAL` vs anything else,
// and a walk the ORDER armed is MS_MOVE - so 'NO' while it is in flight and 'YES' once the
// body arrives. The script's helper WAITS on that answer
// (`while(1){ if IsEnableMove=='YES' return 'YES'; sleep(1000) }`, mon_ai_lib.scp:72-83),
// so 'NO' is what stops the NONE loop drawing a fresh goal every pass.
//
// RED WITHOUT THE LEG: the answer is 'YES' one instant after the order (the walk has no
// clock, so the move state reads as stopped) and the wait loop returns immediately.
func TestIsEnableMoveSaysNoWhileAScriptWalkIsInFlight(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h, _, mon, logs := monsterLegZone(t, &clock)

	if !h.EnableMove(mon.Handle) {
		t.Fatalf("a body that has never been ordered anywhere answered 'NO' - the only "+
			"thing that may say 'NO' is a leg in flight:\n%s", logs.String())
	}
	h.MoveMonster(mon.Handle, 0, 500, 26) // 500 units at 143 u/s = 3.5 s
	if h.EnableMove(mon.Handle) {
		t.Fatalf("IsEnableMove answered 'YES' with a script walk in flight (the body has "+
			"walked %v of its 500 units) - the script's wait loop then returns at once and "+
			"draws a new goal, which is the 3550 flood:\n%s",
			rosterPointOf(t, h, mon.Handle), logs.String())
	}
	// MID-WALK, after the body has really moved.
	clock = clock.Add(2 * time.Second)
	h.monsterMoveTick(clock)
	if at := rosterPointOf(t, h, mon.Handle); at[1] != 286 {
		t.Fatalf("after 2 s the body stands at %v, want y 286", at)
	}
	if h.EnableMove(mon.Handle) {
		t.Fatalf("IsEnableMove answered 'YES' 2 s into a 3.5 s walk:\n%s", logs.String())
	}
	// ARRIVED: 3.5 s. The next order may start now.
	clock = clock.Add(2 * time.Second)
	h.monsterMoveTick(clock)
	if at := rosterPointOf(t, h, mon.Handle); at[1] != 500 {
		t.Fatalf("the walk ended at %v, want y 500", at)
	}
	if !h.EnableMove(mon.Handle) {
		t.Fatalf("IsEnableMove still answers 'NO' after the body arrived - the script's "+
			"wait loop would park for ever:\n%s", logs.String())
	}
}

// TestScriptMoveLegIsNotRedrawnWhileItIsInFlight is pass condition (c): while a leg is in
// flight, the SAME goal is not ordered again (one 3550, never one per pass), and the body
// keeps walking through those passes. A DIFFERENT goal does replace it - that is the
// script asking for somewhere else, and the new leg starts from the point the body has
// actually reached.
func TestScriptMoveLegIsNotRedrawnWhileItIsInFlight(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h, victim, mon, logs := monsterLegZone(t, &clock)

	h.MoveMonster(mon.Handle, 2000, 0, 26) // 2000 units east at 143 u/s = 14 s
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 for one order, want 1", n)
	}
	// 40 passes (4 s) of the script's own flood: the same goal, re-asked on every pass.
	for i := 0; i < 40; i++ {
		clock = clock.Add(100 * time.Millisecond)
		h.monsterMoveTick(clock)
		h.MoveMonster(mon.Handle, 2000, 0, 26)
	}
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 for ONE goal asked 41 times during its walk, want 1 - a walk "+
			"in flight is not replaced by the same point (FieldChar.cpp:946-947):\n%s",
			n, logs.String())
	}
	mid := rosterPointOf(t, h, mon.Handle)
	if mid[0] != 572 { // 143 u/s for 4 s
		t.Fatalf("the body stands at %v after 4 s of a 2000-unit walk, want x 572 - the "+
			"same-goal orders must not restart the leg:\n%s", mid, logs.String())
	}

	// A DIFFERENT GOAL REPLACES IT, from where the body really is.
	h.MoveMonster(mon.Handle, 572, -1000, 26)
	if n := len(movesOf(victim.conn)); n != 2 {
		t.Fatalf("%d x 3550 after a DIFFERENT goal, want 2 - a new destination is a new "+
			"order:\n%s", n, logs.String())
	}
	order, ok := h.MonsterOrderOf(mon.Handle)
	if !ok || order.move.goal != [3]int32{572, -1000, 29} {
		t.Fatalf("the stored order is %+v, want the new goal (572, -1000, 29)", order.move)
	}
	if order.move.from != mid {
		t.Fatalf("the new leg starts at %v, want the point the body has walked to, %v - "+
			"a replacing order must not teleport the body back", order.move.from, mid)
	}
}

// moveLegLine is one `MONSTER MOVE` line's two points: where the body stood when the
// order was given, and the point it was told to walk to.
type moveLegLine struct {
	from [3]int32
	goal [3]int32
}

// moveLegLines parses every `MONSTER MOVE` line the zone wrote for one handle.
//
// WHY THE LOG AND NOT THE PACKET: the 3550 carries ONE point (the destination, move.go
// `putMoveFields`), so the start of each leg is only ever in this line - which is exactly
// why the line prints it.
func moveLegLines(logText string, handle uint32) []moveLegLine {
	needle := "MONSTER MOVE 0x" + hexHandle(handle)
	var out []moveLegLine
	for _, line := range strings.Split(logText, "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		// ... walks to <where> (x, y, cell c) from (x, y, cell c) at N u/s ...
		// `<where>` is free text ("the script's point", "a point within 300 of owner
		// 0x..."), so the two points are taken from their own parentheses: the last one
		// before " from (" is the goal, and the one right after it is the start.
		at := strings.LastIndex(line, " from (")
		if at < 0 {
			continue
		}
		var l moveLegLine
		if _, err := fmt.Sscanf(line[strings.LastIndex(line[:at], "("):at],
			"(%d, %d, cell %d)", &l.goal[0], &l.goal[1], &l.goal[2]); err != nil {
			continue
		}
		rest := line[at+len(" from "):]
		if _, err := fmt.Sscanf(rest, "(%d, %d, cell %d)",
			&l.from[0], &l.from[1], &l.from[2]); err != nil {
			continue
		}
		out = append(out, l)
	}
	return out
}

// TestTheZoneClockWalksAScriptOrderedLeg is the WIRING, not the step: the zone's own clock
// (`tickIfDue`, the function every session's read loop calls) must call the walk's step. A
// step that exists but is not on the clock is the card's defect with extra code.
//
// IT RUNS ON THE REAL CLOCK on purpose - `tickIfDue` takes `time.Now()` - so the leg is
// 5,000 units long (35 s at 143 u/s): a slow machine makes the body walk FURTHER, never
// past the goal, so the assertion cannot flake on a busy box.
func TestTheZoneClockWalksAScriptOrderedLeg(t *testing.T) {
	h, _, victim, _, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.MoveMonster(mon.Handle, 5000, 0, 26)
	if at := rosterPointOf(t, h, mon.Handle); at != [3]int32{0, 0, 29} {
		t.Fatalf("the body stands at %v before its walk, want (0, 0, 29)", at)
	}

	time.Sleep(150 * time.Millisecond)
	ran := false
	for i := 0; i < 3 && !ran; i++ {
		if _, ran = h.tickIfDue(victim.sess); !ran {
			time.Sleep(pollInterval) // the tick's own cadence: lastTick is the session's
		}
	}
	if !ran {
		t.Fatalf("the zone's clock did not run (tickIfDue answered false three times)")
	}
	if at := rosterPointOf(t, h, mon.Handle); at[0] <= 0 || at[0] >= 5000 {
		t.Fatalf("after one zone tick the body stands at %v, want a point on its leg "+
			"(the ordered walk is not on the zone's clock)", at)
	}
}

// TestMonsterAIWanderFloodStopsAndTheBodyWalks is the card's field-level half, run on the
// REAL corpus and the crab's own row: a MON_GIGANTE body with nobody to fight is walked by
// `SCR_MON_GIGANTE_TS_NONE` -> `RandomMoveCreatedLocaly`, and over 60 simulated seconds it
//
//	(a) ends up somewhere other than its birth point (it WALKED), and
//	(b) is ordered a handful of times, not once a second: the script's own
//	    `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` wait loop (1 s) plus a walk of up to
//	    `moveMax = MON_MSPD*3 = 651` units at 217 u/s is the cadence, and it is the
//	    SCRIPT's, not the tick's.
//
// THE RED NUMBER IS THE CARD'S: without the leg every pass answered 'YES' and the body was
// re-ordered about once a second - the measured live run was 1,245 lines in 40 s for 61
// bodies, i.e. ~0.5 per body per second.
func TestMonsterAIWanderFloodStopsAndTheBodyWalks(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	h, logs, scriptLogs, victim, mon := crabZone(t, crabRow())
	h.MonsterNow = func() time.Time { return clock }
	birth := rosterPointOf(t, h, mon.Handle)

	// 600 passes at the zone's 100 ms poll = 60 simulated seconds, in tickIfDue's own
	// order: the ordered walk first (it writes the body's point), then the think turn,
	// then the approach.
	for i := 0; i < 600; i++ {
		clock = clock.Add(100 * time.Millisecond)
		h.monsterMoveTick(clock)
		h.monsterAITick(clock)
		h.monsterApproachTick(clock)
	}

	moves := movesOf(victim.conn)
	if len(moves) == 0 {
		t.Fatalf("the crab was ordered to walk 0 times in 60 s:\nscript log:\n%s\nzone log:\n%s",
			scriptLogs.String(), logs.String())
	}
	if len(moves) > 40 {
		t.Fatalf("%d x 3550 in 60 s for one body, want at most 40 - a walk with a leg is "+
			"not re-ordered while it is in flight (the flood was ~1 order/s):\n%s",
			len(moves), logs.String())
	}
	end := rosterPointOf(t, h, mon.Handle)
	if end == birth {
		t.Fatalf("the crab stands exactly where it was born (%v) after %d order(s): the "+
			"walk has no leg on the server's side\n%s", birth, len(moves), logs.String())
	}
	// THE CHAIN IS THE PROOF, and it is read from the LOG LINES because the 3550 carries
	// only the destination: each order's `from` point (monster_order.go prints it) must be
	// the point the previous order named as its goal. A body that did not walk could only
	// be re-ordered from where it stands (its birth point); a body whose leg was replaced
	// mid-walk would be re-ordered from somewhere between two goals.
	orders := moveLegLines(logs.String(), mon.Handle)
	if len(orders) != len(moves) {
		t.Fatalf("%d MONSTER MOVE line(s) for %d 3550(s) - every walk has one line",
			len(orders), len(moves))
	}
	for i := 1; i < len(orders); i++ {
		if orders[i].from != orders[i-1].goal {
			t.Fatalf("order %d starts from %v but order %d's goal was %v - the body was "+
				"re-ordered before it walked the leg it had:\n%s",
				i, orders[i].from, i-1, orders[i-1].goal, logs.String())
		}
		if orders[i].goal == orders[i-1].goal {
			t.Fatalf("order %d asks for the goal order %d already gave it (%v):\n%s",
				i, i-1, orders[i].goal, logs.String())
		}
	}
	// THE LAST LEG IS THE ONE IN FLIGHT at the end of the window (the script drew it a few
	// tenths of a second before the last pass), so the body must sit on the straight line
	// between that order's start and its goal - the point it has walked to.
	last := orders[len(orders)-1]
	if walked, left := flatDist(last.from, end), flatDist(end, last.goal); walked <= 0 ||
		math.Abs(walked+left-flatDist(last.from, last.goal)) > 3 {
		t.Fatalf("the crab stands at %v, which is not on its last leg %v -> %v:\n%s",
			end, last.from, last.goal, logs.String())
	}
	t.Logf("%d order(s) in 60 s (the flood was ~60), birth %v -> %v, last leg %v -> %v",
		len(orders), birth, end, last.from, last.goal)

	// THE LOG SAYS WHAT HAPPENS TO THE ORDER NOW, so a reader of a live log is not left
	// with the old sentence ("the body's own point is not moved by a binding").
	if !strings.Contains(logs.String(), "WITH ITS LEG") {
		t.Fatalf("the MONSTER MOVE line does not say the order carries a leg:\n%s",
			logs.String())
	}
}
