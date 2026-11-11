package zone

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE NORMAL ATTACK'S REACH IS THE STANCE'S MinR/MaxR (R90).
//
// The owner found this in game, twice: "again normal skill also need stance range
// check. Fighter melee attack is attacking from insane distance lol." and "normal
// attack is stance based. read the xml/ies." The numbers were in
// `datatable_stance.xml` all along, on the same row as the ASPD the zone already
// reads:
//
//	BackGuard      10110  MinR 0   MaxR 150   ASPD 1000
//	PossessionFire 20210  MinR 50  MaxR 900   ASPD 1700
//	Peltast        10560  MinR 1   MaxR 125   ASPD 1067   (the capture's stance)
//
// These tests hold the handler to ONE rule, read off that row and never baked in:
//
//	inside the range   the hit goes out at once
//	past MaxR          the character walks to a stand point inside the range and the
//	                   hit fires when it arrives - the skill approach's own machinery
//	nearer than MinR   the character steps BACK out to the min ring and the hit fires
//	                   when it arrives (R101 - the far case's mirror; the reference's
//	                   IsAttackArea -1 -> GetAttackMinPos)
//	no row / MaxR 0    nothing is checked, and one line says so
//
// THE NUMBERS ARE READ FROM THE TABLE, never written into the test as a range: (f)
// is the test that would go red if 150 (or any other number) were baked in, and the
// "prove a test can fail" run in the report is exactly that change.

// attackZoneAt is attackZone with the character moved and its stance chosen, so a
// test can ask about ONE stance's row. Everything else - the real stance table, the
// real item numbers, the Scarecrow, its 5000 HP - is the shared fixture.
func attackZoneAt(t *testing.T, stance uint32, pos [3]int32) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZone(t)
	p.sess.Team[0].Stance = stance
	p.sess.Team[0].Position = pos
	p.sess.Pos = pos
	return h, logs, p
}

// stanceReach reads the reach the handler must use straight out of the table, and
// fails the test when the row it is written against is not there: a missing row would
// make every assertion below vacuous.
func stanceReach(t *testing.T, h *Handler, stance uint32) (minR, maxR float64) {
	t.Helper()
	if h.Stances == nil {
		t.Fatal("this zone has no stance table, so no reach can be asked about")
	}
	row := h.Stances.Combat(stance)
	if row.MaxR <= 0 {
		t.Fatalf("stance %d has MaxR %v in datatable_stance.xml - the row this test is "+
			"written against is missing", stance, row.MaxR)
	}
	return row.MinR, row.MaxR
}

// pressAttack sends one real CZ_ATTACK through the dispatch, exactly as the client
// does, so a test that passes cannot pass with no case in the switch.
func pressAttack(t *testing.T, h *Handler, sess *Session, target uint32) [][]byte {
	t.Helper()
	f := attackFrame(0, target)
	out, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestNormalAttackInsideTheStanceReachHitsAtOnce is (a): a press inside the stance
// row's own MinR..MaxR answers 3564 then 3557 immediately - no walk, no 3548, no
// waiting.
func TestNormalAttackInsideTheStanceReachHitsAtOnce(t *testing.T) {
	const stance = uint32(10560) // Peltast, the captured stance
	h, logs, p := attackZoneAt(t, stance, attackReachStand)
	minR, maxR := stanceReach(t, h, stance)

	// THE FIXTURE'S OWN GEOMETRY: attackReachStand is 100 units from the Scarecrow.
	dist := math.Hypot(float64(attackReachStand[0]-scarecrow().X),
		float64(attackReachStand[1]-scarecrow().Y))
	// The target's half is 0 here because this fixture loads no monster table, which is
	// exactly the answer the handler gives for it (monsterBodyRadius, one log line).
	if _, in := attackRangeAllowed(attackReachStand,
		[3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}, minR, maxR,
		PCBodyRadius, 0); !in {
		t.Fatalf("the fixture stands %g from the target, outside stance %d's %g..%g - "+
			"this test cannot ask about an in-reach press", dist, stance, minR, maxR)
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 {
		t.Fatalf("an in-reach press produced %v, want exactly %d then %d:\n%s",
			kinds(out), CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if got := recvs(out); got[0].cmd != CmdZCAttackSingle || got[1].cmd != CmdZCAddHP {
		t.Fatalf("the answer is %v, want %d then %d", kinds(out), CmdZCAttackSingle,
			CmdZCAddHP)
	}
	if p.sess.Walk != nil {
		t.Errorf("an in-reach press started a walk (%s) - it must hit where it stands",
			p.sess.Walk.describe())
	}
	if strings.Contains(logs.String(), "APPROACH") {
		t.Errorf("an in-reach press logged an approach:\n%s", logs.String())
	}
	t.Logf("in reach (%g of MaxR %g): %s", dist, maxR, lastLine(logs.String()))
}

// TestNormalAttackPastMaxRSendsTheRingWalkAtOnce is (b): a press beyond MaxR sends the
// ONE ZC_MOVE_PC AT ONCE (R91 round 6). The client's own AI never walks the character to
// a clicked enemy - SCR_ATTACKER_TS_BATTLE_NOMINATE calls SCR_AI_LIB_ATTACKTARGET, which
// only attacks or sleeps (attacker.scp:135-170, lib.scp) - so the server walks, exactly
// as the reference does from the press itself (FieldChar_FSM.cpp:28-80): the same plan,
// stand point, reason and packet a skill approach uses. NOTHING SLEEPS and NO TICK is
// needed to start it.
func TestNormalAttackPastMaxRSendsTheRingWalkAtOnce(t *testing.T) {
	const stance = uint32(10560)
	// 400 units from the Scarecrow, along y: past Peltast's MaxR whichever way the
	// table's number moves, as long as it is under 400.
	far := [3]int32{-919, 1911, 136}
	h, logs, p := attackZoneAt(t, stance, far)
	minR, maxR := stanceReach(t, h, stance)
	if 400 <= maxR {
		t.Fatalf("stance %d reaches %g, so %g away is NOT out of reach - pick a farther "+
			"stand point", stance, maxR, 400.0)
	}

	var calls int
	var gotFrom, gotTarget [3]int32
	var gotStandoff float64
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		calls++
		gotFrom, gotTarget, gotStandoff = from, target, standoff
		return directPlan(from, target, standoff)
	}

	// THE PRESS ITSELF WALKS: one move packet, and it is the answer to this press.
	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 1 {
		t.Fatalf("an out-of-reach press produced %v, want exactly one %d sent AT ONCE - "+
			"the client's own AI never walks to a clicked enemy, so this zone does:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	move := recvs(out)[0]
	if move.cmd != CmdZCMovePC {
		t.Fatalf("the only packet is cmd %d, want %d ZC_MOVE_PC", move.cmd, CmdZCMovePC)
	}
	if len(move.b) != 30 {
		t.Fatalf("ZC_MOVE_PC is %d bytes, want 30", len(move.b))
	}
	if got := binary.LittleEndian.Uint32(move.b[2:]); got != capturedCaster {
		t.Errorf("move +2 handle = %d, want the character that pressed (%d)", got,
			capturedCaster)
	}
	if got := binary.LittleEndian.Uint32(move.b[26:]); got != ReasonSkill {
		t.Errorf("move +26 reason = %d, want %d (MR_SKILL: the reason a capture shows "+
			"for a SERVER-driven walk)", got, ReasonSkill)
	}

	// THE DESTINATION IS INSIDE THE STANCE'S REACH, and it is the stand point the
	// table's own number asked for.
	dest := [3]int32{
		int32(binary.LittleEndian.Uint32(move.b[10:])),
		int32(binary.LittleEndian.Uint32(move.b[14:])),
		int32(binary.LittleEndian.Uint32(move.b[18:])),
	}
	if d, in := attackRangeAllowed(dest, [3]int32{scarecrow().X, scarecrow().Y,
		scarecrow().Cell}, minR, maxR, PCBodyRadius, 0); !in {
		t.Errorf("the stand point %v is %g from the target, outside MinR %g .. MaxR %g",
			dest, d, minR, maxR)
	}
	if calls != 1 {
		t.Errorf("the path engine was called %d time(s), want 1", calls)
	}
	if gotFrom != far {
		t.Errorf("planned from %v, want the character's own position %v", gotFrom, far)
	}
	if want := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}; gotTarget != want {
		t.Errorf("planned towards %v, want the target %v", gotTarget, want)
	}
	// THE REFERENCE'S RING, NO MARGIN (R132): MaxR plus the target's own body, from its
	// centre (`maxR + obj->GetDomainRadius()`, PlayerChar.cpp:1393). This fixture's target
	// has no table row, so its half is 0 - and the 10% margin the 90% rule applied is gone.
	if want := maxR; math.Abs(gotStandoff-want) > 0.001 {
		t.Errorf("asked for a stand-off of %g, want %g (MaxR %g + the target's body 0, "+
			"the reference's ring)", gotStandoff, want, maxR)
	}

	// THE PENDING ANSWER IS THE CLIENT'S OWN FRAME, and the walk knows what it walks for.
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatal("no walk with a pending attack was left on the session")
	}
	if w.caster != 0 {
		t.Errorf("walk caster = %d, want 0 (the character that pressed)", w.caster)
	}
	if w.atk.target != capturedTarget {
		t.Errorf("pending attack targets %d, want %d", w.atk.target, capturedTarget)
	}
	if w.cast != nil {
		t.Error("the normal attack's walk also carries a pending CAST - one walk answers " +
			"one request")
	}

	text := logs.String()
	for _, want := range []string{"ATTACK APPROACH", "reaches MinR", "MaxR",
		"the hit fires when the walk ends"} {
		if !strings.Contains(text, want) {
			t.Errorf("the approach line does not mention %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "3564") {
		t.Errorf("the walk sent or named a hit:\n%s", text)
	}
	t.Logf("approach: %s", lineWith(text, "ATTACK APPROACH"))
}

// TestNormalAttackLandsWhenTheWalkEnds is (c): once the approach's clock says the
// character is at the stand point, the hit goes out in the measured order - 3564,
// then the 3557 that carries the same double - and only ONCE, however many ticks run.
func TestNormalAttackLandsWhenTheWalkEnds(t *testing.T) {
	const stance = uint32(10560)
	far := [3]int32{-919, 1911, 136}
	h, logs, p := attackZoneAt(t, stance, far)
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, target, standoff)
	}

	// THE WALK IS IN FLIGHT FROM THE PRESS ITSELF (R91 round 6), not from a tick.
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1 - the walk starts at the press:\n%s",
			moves(out), logs.String())
	}

	walkFor(t, p.sess)
	goal := p.sess.Walk.goal
	out := h.chaseTick(p.sess)
	if len(out) != 2 {
		t.Fatalf("the end of the walk produced %v, want %d then %d:\n%s", kinds(out),
			CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	hit, add := recvs(out)[0], recvs(out)[1]
	if hit.cmd != CmdZCAttackSingle || add.cmd != CmdZCAddHP {
		t.Fatalf("the order is %v, want %d then %d (the order the capture measured)",
			kinds(out), CmdZCAttackSingle, CmdZCAddHP)
	}
	if len(hit.b) != AttackSingleSize || len(add.b) != AddHPSize {
		t.Errorf("the packets are %d and %d bytes, want %d and %d", len(hit.b), len(add.b),
			AttackSingleSize, AddHPSize)
	}
	// THE SAME DOUBLE TWICE, and the HP before/after on the right fields.
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit.b[asDamage:]))
	before := math.Float64frombits(binary.LittleEndian.Uint64(hit.b[asTargetHP:]))
	after := math.Float64frombits(binary.LittleEndian.Uint64(add.b[ahNowHP:]))
	if !bytesEqual(add.b[ahAddHP:ahAddHP+8], hit.b[asDamage:asDamage+8]) {
		t.Errorf("the 3557 delta is not the 3564's damage double")
	}
	if before != 5000 || after != 5000-damage {
		t.Errorf("HP %g -> %g, want 5000 -> %g", before, after, 5000-damage)
	}
	// The hit names the stand point the WALK reached - the body's own walked point, which
	// is what the fire measures (R104). It is not read back off the session: a fire does
	// not place the body, so sess.Pos is not a stand-in for the goal any more.
	if got := worldPos(hit.b[asAtkPos:]); got != goal {
		t.Errorf("3564 +8 = %v, want the stand point the walk reached %v", got, goal)
	}

	// ONE PRESS, ONE HIT: the walk ended on its own arrival (piece 2d), and a later tick
	// must not fire the attack a second time.
	walkPastArrival(t, p.sess)
	for _, pkt := range h.chaseTick(p.sess) {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd == CmdZCAttackSingle {
			t.Fatalf("a later tick sent a SECOND %d for one press (%s)",
				CmdZCAttackSingle, logs.String())
		}
	}
	t.Logf("landed: %s", lineWith(logs.String(), "ATTACK APPROACH: the character can be"))
}

// TestNormalAttackTooCloseForMinRStepsBackToTheMinRing is (d), REWRITTEN IN R101. It used
// to assert the opposite: a target nearer than the stance's own MinR was REFUSED with one
// line and nothing sent. The reference does not refuse it - IsAttackArea answers -1 and
// that case walks the character OUT to the min ring (FieldChar_FSM.cpp:63-79 ->
// GetAttackMinPos) - and the owner's session showed what the refusal cost in game: 6 of
// 27 presses ended on a body standing on top of the monster with nothing sent. So the
// answer here is now one move to the min ring, the press held, and NO refusal line.
func TestNormalAttackTooCloseForMinRStepsBackToTheMinRing(t *testing.T) {
	const stance = uint32(20210) // PossessionFire: MinR 50 .. MaxR 900
	close := [3]int32{-919, 2301, 136}
	h, logs, p := attackZoneAt(t, stance, close)
	minR, _ := stanceReach(t, h, stance)

	dist := math.Hypot(float64(close[0]-scarecrow().X), float64(close[1]-scarecrow().Y))
	if dist >= minR {
		t.Fatalf("the fixture stands %g from the target, not under MinR %g", dist, minR)
	}
	h.Plan = directPlan

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("a too-close press produced %v (%d move(s)), want exactly ONE move out "+
			"to the min ring:\n%s", kinds(out), moves(out), logs.String())
	}
	if n := attackCmds(out); n != 0 {
		t.Fatalf("a too-close press sent %d attack packet(s) without stepping back:\n%s",
			n, logs.String())
	}
	if p.sess.Walk == nil || p.sess.Walk.atk == nil {
		t.Fatalf("a too-close press left no walk with a held press:\n%s", logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK refused"); line != "" {
		t.Errorf("a too-close press is still refused: %s", line)
	}
	if hp, ok := h.monsterLiveHP(capturedTarget); !ok || hp != 5000 {
		t.Errorf("a step-back press moved the monster's HP to %v (known %v)", hp, ok)
	}
	t.Logf("step back: %s", lineWith(logs.String(), "STEPPING BACK"))
}

// TestSecondNormalAttackInsideTheStanceASPDIsDropped is (e): two presses in the same
// instant answer ONE hit. The second produces no second 3564 (and no 3557, no 3568),
// and one log line names the time remaining - a number that comes from the stance
// row's own ASPD through the sheet, not from a constant here.
func TestSecondNormalAttackInsideTheStanceASPDIsDropped(t *testing.T) {
	h, logs, p := attackZoneAt(t, 10560, attackReachStand)
	interval := attackASPD(t, h, p)

	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 2 {
		t.Fatalf("the first press produced %v, want the hit and the HP:\n%s", kinds(out),
			logs.String())
	}
	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 0 {
		t.Fatalf("a second press %s into a %g ms interval produced %v, want NOTHING:\n%s",
			time.Since(p.sess.lastAttack(0)).Round(time.Millisecond), interval,
			kinds(out), logs.String())
	}
	line := lineWith(logs.String(), "ATTACK held")
	if line == "" {
		t.Fatalf("the held press logged no line:\n%s", logs.String())
	}
	if !strings.Contains(line, "attack delay") || !strings.Contains(line, "ASPD") {
		t.Errorf("the held line does not name the interval or where it came from: %s", line)
	}
	// The clock is the character's own sheet, which starts at the stance row's ASPD.
	if got := h.stanceASPD(p.sess.Team[0].Stance); got <= 0 {
		t.Fatalf("stance 10560 states ASPD %v, so this test cannot ask about a cadence", got)
	}
	t.Logf("held: %s", line)
}

// TestNormalAttackReachFromTheStanceRowChangesWithTheStance is (f), THE ONE THAT
// PROVES NO NUMBER IS BAKED IN: the same character, the same distance, the same
// monster - and only the STANCE changes. BackGuard reaches 150 and the press walks;
// PossessionFire reaches 900 and the same press hits at once. Bake any constant in and
// this test goes red.
func TestNormalAttackReachFromTheStanceRowChangesWithTheStance(t *testing.T) {
	// 300 units from the Scarecrow: past BackGuard's MaxR, inside PossessionFire's.
	stand := [3]int32{-919, 2611, 136}
	const distance = 300.0

	h, logs, p := attackZoneAt(t, 10110, stand) // BackGuard, MinR 0 .. MaxR 150
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, target, standoff)
	}
	_, maxMR := stanceReach(t, h, 10110)
	if distance <= maxMR {
		t.Fatalf("stance 10110 reaches %g, so %g away is inside it - this test needs a "+
			"distance between the two stances' reaches", maxMR, distance)
	}
	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("with stance 10110 (MaxR %g) a press %g away produced %v, want one "+
			"%d - out of reach, so this zone walks:\n%s", maxMR, distance, kinds(out),
			CmdZCMovePC, logs.String())
	}
	if w := p.sess.Walk; w == nil || w.atk == nil {
		t.Fatalf("with stance 10110 (MaxR %g) a press %g away left no walk with a pending "+
			"attack:\n%s", maxMR, distance, logs.String())
	}

	// THE SAME PRESS, ONE FIELD CHANGED: the stance. PossessionFire reaches 900.
	p.sess.Team[0].Stance = 20210
	minPF, maxPF := stanceReach(t, h, 20210)
	if distance <= minPF || distance > maxPF {
		t.Fatalf("stance 20210 reaches %g..%g, so %g away is not inside it", minPF, maxPF,
			distance)
	}
	out = pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 || recvs(out)[0].cmd != CmdZCAttackSingle {
		t.Fatalf("with stance 20210 (MaxR %g) the SAME press %g away produced %v, want the "+
			"hit at once - the reach did not follow the stance row:\n%s", maxPF, distance,
			kinds(out), logs.String())
	}
	if recvs(out)[1].cmd != CmdZCAddHP {
		t.Fatalf("the hit was not followed by %d: %v", CmdZCAddHP, kinds(out))
	}

	// AND THE TWO ROWS REALLY ARE DIFFERENT NUMBERS, read from the table itself: this is
	// what makes the change above evidence and not a coincidence of geometry.
	if maxMR == maxPF {
		t.Fatalf("stances 10110 and 20210 both reach %g - the test proves nothing", maxMR)
	}
	t.Logf("stance 10110 MaxR %g -> walks (%s); stance 20210 MaxR %g -> hit at %g", maxMR,
		lineWith(logs.String(), "ATTACK APPROACH"), maxPF, distance)
}

// TestNormalAttackWithNoMaxRChecksNothing says out loud what happens when the row is
// missing or states no far bound: the reach is NOT checked, the hit goes out where the
// character stands, and ONE line per run says so. Inventing a reach here is exactly
// the mistake R84 made with skills, which refused the owner's Fighter.
func TestNormalAttackWithNoMaxRChecksNothing(t *testing.T) {
	// A stance id with no row in datatable_stance.xml: the table answers its neutral
	// row, MinR 0 / MaxR 0.
	const noRow = uint32(999999)
	far := [3]int32{-919, 1911, 136}
	h, logs, p := attackZoneAt(t, noRow, far)

	if row := h.Stances.Combat(noRow); row.MaxR != 0 {
		t.Fatalf("stance %d has a row with MaxR %v - this test needs a missing row",
			noRow, row.MaxR)
	}
	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 {
		t.Fatalf("a press with no stated reach produced %v, want the hit and the HP:\n%s",
			kinds(out), logs.String())
	}
	if p.sess.Walk != nil {
		t.Errorf("a press with no stated reach started a walk (%s)", p.sess.Walk.describe())
	}
	line := lineWith(logs.String(), "ATTACK REACH NOT CHECKED")
	if line == "" {
		t.Fatalf("nothing said the reach was not checked:\n%s", logs.String())
	}
	// ONCE per run, not once per press.
	before := strings.Count(logs.String(), "ATTACK REACH NOT CHECKED")
	p.sess.noteAttack(0, time.Now().Add(-time.Hour))
	pressAttack(t, h, p.sess, capturedTarget)
	if after := strings.Count(logs.String(), "ATTACK REACH NOT CHECKED"); after != before {
		t.Errorf("the line appeared %d time(s) then %d - it is meant to be once per run",
			before, after)
	}
	t.Logf("no reach: %s", line)
}

// ---- PIECE 2d: ARRIVAL BY DISTANCE, AND THE ARRIVAL PADS ARE GONE ----------
//
// The owner, in game on 2026-11-01, after the walk was restored: "the transition
// between starting to attack and moving is quite noticeable especially on melee
// attack since they require shorter distance." The number was in his own log: a walk
// with eta 48 ms fired `after 311ms (eta 48ms)` - that is 48 x 1.4 + 250, the STOP's
// pad paid on a hit that teleports nobody. R96 shrank the hit's own pad to 50 ms;
// piece 2d deletes the clock a pad belongs to.
//
// WHAT THESE TESTS PIN NOW:
//
//	this file   the hit fires on the tick the body is inside the stance's reach, and the
//	            walk is OVER on that same tick - no pad in either direction
//	the box     opens on the tick the body reaches its stand point (chase_dialog_test.go)
//	the stop    leaves with the arrival and names the goal the body provably reached
//
// The pads that REMAIN in the tree are the PLAYER-DRIVEN leg's own (move.go,
// charWalk.walkedOut): that leg has no body model - only a report and a speed - so its
// stop keeps StopSafety and StopSlack. See walkend_test.go.

// attackApproachAt starts a real approach with a real press, then states the walk's
// eta so the test can name exact numbers instead of measuring a route: the fixture is
// what is under test here, not the path engine.
func attackApproachAt(t *testing.T, h *Handler, sess *Session, eta time.Duration) *walk {
	t.Helper()
	if out := pressAttack(t, h, sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1 - no approach to time", moves(out))
	}
	w := sess.Walk
	if w == nil || w.atk == nil {
		t.Fatal("no walk with a pending attack was left on the session")
	}
	w.eta = eta
	return w
}

// TestAttackApproachFiresOnTheReachNotOnAClock (WAS
// TestAttackApproachFiresFiftyMillisecondsPastArrival) drives a real attack approach and
// asserts the two halves of the new rule:
//
//	nothing fires while the body is short of the reach, whatever the eta says;
//	the hit fires on the tick the body is THERE - with NO pad at all, not R96's 50 ms and
//	not the stop's 250 ms - and the walk is over on that same tick.
//
// The old test compared two clocks (approachFireDue vs walkDone). There are no clocks to
// compare any more: approachFireDue is deleted, and the parity it pinned is replaced by
// "the arrival is the body".
func TestAttackApproachFiresOnTheReachNotOnAClock(t *testing.T) {
	const eta = 48 * time.Millisecond
	far := [3]int32{-919, 1911, 136}
	h, logs, p := attackZoneAt(t, 10560, far)
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, target, standoff)
	}
	w := attackApproachAt(t, h, p.sess, eta)

	// THE BODY HAS NOT WALKED: nothing may fire, and nothing may be placed on the stand
	// point either - the eta is not an arrival.
	w.began = time.Now()
	if out := h.chaseTick(p.sess); len(out) != 0 {
		t.Fatalf("%d packet(s) went out for a body that has not walked anywhere:\n%s",
			len(out), logs.String())
	}

	// THE BODY HAS WALKED THE WHOLE SEGMENT - its REAL route time, with nothing added.
	// The hit is out exactly once, on this tick.
	route := math.Hypot(float64(w.goal[0]-w.from[0]), float64(w.goal[1]-w.from[1]))
	real := time.Duration(route / w.speed * float64(time.Second))
	w.began = time.Now().Add(-(real + 2*time.Millisecond))
	out := h.chaseTick(p.sess)
	fired := 0
	for _, pkt := range out {
		if cmdOf(pkt) == CmdZCAttackSingle {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("a %s arrival fired %d hit(s) at %s (+2 ms), want 1 - something is still "+
			"waiting for a pad:\n%s", w.eta, fired, real.Round(time.Millisecond),
			logs.String())
	}
	line := lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if line == "" {
		t.Fatalf("the hit went out with no line naming it:\n%s", logs.String())
	}

	// AND THE WALK IS OVER ON THE SAME TICK: the stop the other players are owed left with
	// it, and nothing is left waiting for a second clock.
	if p.sess.Walk != nil {
		t.Fatalf("the walk survived the arrival the hit fired on (%s into a %s route): "+
			"a second clock is still deciding when it ends\n%s",
			time.Since(p.sess.Walk.began).Round(time.Millisecond), real, logs.String())
	}
	t.Logf("eta field %s, body's own route %s: nothing while short, one hit at the "+
		"arrival, line: %s", eta, real.Round(time.Millisecond), line)
}

// TestTheBoxOpensOnArrivalNotOnAClock is THE REPRODUCTION the piece is judged on, and it
// REPLACES TestNPCDialogStillWaitsTheStopSlack.
//
// The fixture is the shape the task names: a walk whose ETA IS LARGE (10 s) while the body
// covers its own route in well under a second - the plan's estimate and the body's
// distance disagree, which is exactly what a stale eta field is. On the old tree the box
// waited on that eta (10 s, then 14.25 s with the pads); now it opens the moment the
// body's distance says the character is there.
func TestTheBoxOpensOnArrivalNotOnAClock(t *testing.T) {
	h, calls := chaseHandler(t)
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, npc, standoff)
	}
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}
	w := sess.Walk
	if got := w.eta; got < 500*time.Millisecond || got > 1500*time.Millisecond {
		t.Fatalf("this fixture's real eta is %s, so the 10 s below really is a stale "+
			"estimate - check the fixture", got.Round(time.Millisecond))
	}
	w.eta = 10 * time.Second // the stale estimate: the body does not care
	bodyRoute := flatDist(w.from, w.goal)
	bodyTime := time.Duration(bodyRoute / w.speed * float64(time.Second))

	// HALF WAY: nothing. The box must not open at the click, and must not open on the eta.
	w.began = time.Now().Add(-bodyTime / 2)
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) half way along the walk, want 0 - the box opened before "+
			"the body was there", len(out))
	}
	if *calls != 0 {
		t.Fatalf("the click script ran %d time(s) half way along the walk, want 0", *calls)
	}

	// AT THE BODY'S OWN ARRIVAL, and 9 s BEFORE the eta the walk claims: the box opens,
	// once. This is the assertion that fails on a clock of any kind.
	w.began = time.Now().Add(-(bodyTime + 2*time.Millisecond))
	out := h.chaseTick(sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("%d packet(s) at the body's own arrival (%s of a 10 s eta), want the "+
			"dialog - the box is still waiting on the eta",
			len(out), (bodyTime + 2*time.Millisecond).Round(time.Millisecond))
	}
	if *calls != 1 {
		t.Fatalf("the click script ran %d time(s), want 1", *calls)
	}
	if sess.Walk != nil {
		t.Fatal("the walk survived the arrival the box opened on")
	}
	t.Logf("body route %g at %g u/s = %s, the walk's eta field %s: the box opened on the "+
		"body", bodyRoute, w.speed, bodyTime.Round(time.Millisecond), w.eta)
}

// TestALongWalkDoesNotFireBeforeItsDistanceDoes: the other direction. A LONG walk with an
// HONEST eta must not fire one tick early - the rule is a distance, not a budget - and it
// must fire as soon as the body is there, with no 1.4x and no 250 ms added.
func TestALongWalkDoesNotFireBeforeItsDistanceDoes(t *testing.T) {
	h, calls := chaseHandler(t)
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, npc, standoff)
	}
	sess := chaseSession()
	sess.Pos = [3]int32{1000, 1000, 5}
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("no walk started")
	}
	bodyRoute := flatDist(w.from, w.goal)
	bodyTime := time.Duration(bodyRoute / w.speed * float64(time.Second))
	if bodyTime < 500*time.Millisecond {
		t.Fatalf("this fixture's walk is only %s long - it cannot show 'not before'",
			bodyTime.Round(time.Millisecond))
	}

	// ONE TICK SHORT OF THE BODY'S OWN ARRIVAL: nothing.
	w.began = time.Now().Add(-(bodyTime - 100*time.Millisecond))
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) 100 ms short of a %s walk, want 0 - the arrival is early",
			len(out), bodyTime.Round(time.Millisecond))
	}
	if *calls != 0 {
		t.Fatalf("the click script ran %d time(s) before the body arrived, want 0", *calls)
	}

	// AT IT: the box, once.
	w.began = time.Now().Add(-(bodyTime + 2*time.Millisecond))
	out := h.chaseTick(sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("%d packet(s) at the body's own arrival, want the dialog", len(out))
	}
	if *calls != 1 {
		t.Fatalf("the click script ran %d time(s), want 1", *calls)
	}
	t.Logf("a %s walk (%g units at %g u/s): silent 100 ms short, the box at the distance",
		bodyTime.Round(time.Millisecond), bodyRoute, w.speed)
}

// TestTheStopLeavesWithTheArrivalAndNamesTheGoal REPLACES
// TestTheStopClockIsUntouchedByTheHitClock, and it reverses what that test defended.
//
// A stop still has to name a place the body PROVABLY reached - that rule does not move.
// What moved is WHEN: the walk ends on its distance, so the stop leaves on the same tick
// the box does, naming the goal the body has just walked to. The old tree sent it at
// eta x 1.4 + 250 ms, seconds later.
func TestTheStopLeavesWithTheArrivalAndNamesTheGoal(t *testing.T) {
	h, a, b := chasePair(t)
	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := a.sess.Walk
	if w == nil {
		t.Fatal("no walk started")
	}
	goal := w.goal
	bodyTime := time.Duration(flatDist(w.from, w.goal) / w.speed * float64(time.Second))

	// Mid-walk: the other player has the move and nothing else.
	if got := len(movesAndStops(b.conn.sent())); got != 1 {
		t.Fatalf("the other player has %d packet(s) mid-walk, want just the move", got)
	}

	// AT THE ARRIVAL: the box for the owner, and ONE stop for the other player, naming the
	// goal - on the same tick, because it is the same event.
	w.began = time.Now().Add(-(bodyTime + 2*time.Millisecond))
	out := h.chaseTick(a.sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("%d packet(s) for the owner at the arrival, want the dialog:\n%v",
			len(out), kinds(out))
	}
	seen := movesAndStops(b.conn.sent())
	if len(seen) != 2 {
		t.Fatalf("the other player got %d packet(s) at the arrival, want the move then one "+
			"stop - the stop and the box are the same event now", len(seen))
	}
	if cmdOf(seen[1]) != CmdZCStopPC {
		t.Fatalf("the second packet is cmd %d, want the stop", cmdOf(seen[1]))
	}
	for i, want := range [3]int32{goal[0], goal[1], goal[2]} {
		if got := int32(le32At(seen[1], 10+i*4)); got != want {
			t.Fatalf("stop pos[%d] = %d, want %d (the goal the body reached)", i, got, want)
		}
	}
	// One stop per stop: a later tick adds nothing.
	h.chaseTick(a.sess)
	if got := len(movesAndStops(b.conn.sent())); got != 2 {
		t.Fatalf("a second tick added a packet: %d, want the same 2", got)
	}
	t.Logf("a %s walk: the box and the stop leave on the arrival tick, naming (%d, %d, %d)",
		bodyTime.Round(time.Millisecond), goal[0], goal[1], goal[2])
}
