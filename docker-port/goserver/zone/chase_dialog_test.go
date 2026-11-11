package zone

import (
	"bytes"
	"context"
	"log"
	"math"
	"strings"
	"testing"
	"time"
)

// ---- PIECE 2d: THE NPC BOX OPENS ON ARRIVAL, NOT ON A CLOCK -----------------
//
// The history this file used to pin, and the numbers that ended it:
//
//	2026-10-29  "NPC dialog kinda slow? a slight delay of few seconds" - the box waited on
//	            walkDone, eta x 1.4 + 250 ms, so a 6.412 s walk opened at 9.231 s
//	            (logs\zone10_live.log: `finished after 9.231s (eta 6.412s)`)
//	R64         the box was moved to its own clock, eta + 250 ms - faster, still a clock
//	piece 2d    the clock goes. The body's own DISTANCE decides, on every tick.
//
// WHAT THE REFERENCE DOES, which is why there is nothing to pad:
//
//	if (MS_MOVE == GetMoveState()) return 0;                       // still walking
//	if (false == IsNearObject(target, DISTANCE_TO_CHAR*2))         // not there yet
//	        target->GetDomainOuterPos(m_pos, moveTo, 100);         // walk again
//	else    SetActionState(AS_DIALOG);                             // the box opens
//	        source/geServer/ZServerLib/PlayerChar_FSM.cpp:340-372
//
// There is no timer in that state at all. Our port's body is the point the walk has
// covered along its own route (move.go, walkedRouteAt), at the speed the move on the
// wire told the client to animate - so "has it arrived" is a distance this side can
// answer on every tick, which is what chaseTick does now (chase.go, walk.arrived).
//
// WHY THE BOX IS STILL NOT WOKEN BY A CLIENT REPORT: this client sends no leader report
// during a SERVER-driven walk. Measured, same log: 13 chases, 393 `MOVE_PC char 0`
// reports in the whole file, and ZERO of them inside a chase - every report in those
// windows is char 1 or char 2. observeWalk still answers a report if one ever arrives
// (TestLeaderArrivalOpensTheDialog, unchanged).

// bodyAge is how long this walk's body needs to cover its own route: the route's length
// divided by the speed the move named. It is NOT the eta field - that is what these
// tests deliberately disagree with.
func bodyAge(w *walk) time.Duration {
	route := flatDist(w.from, w.goal)
	if len(w.route) >= 2 {
		route = polylineLength(w.route)
	}
	if w.speed <= 0 {
		return 0
	}
	return time.Duration(route / w.speed * float64(time.Second))
}

// TestTheBoxOpensTheMomentTheBodyIsThere is the whole rule in one test: silence while the
// body is short, and the box on the very tick the distance says it is there - with no
// pad, whichever clock the old tree would have used (eta x 1.4 + 250 ms here).
func TestTheBoxOpensTheMomentTheBodyIsThere(t *testing.T) {
	h, a, b := chasePair(t)
	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if a.sess.Walk == nil {
		t.Fatal("no walk started")
	}
	w := a.sess.Walk
	due := bodyAge(w)
	if due <= 0 {
		t.Fatalf("this fixture's body needs %s to walk its route - no test here", due)
	}

	// ONE TICK SHORT: nothing for the owner, nothing new for the other player.
	w.began = time.Now().Add(-(due - 100*time.Millisecond))
	if out := h.chaseTick(a.sess); len(out) != 0 {
		t.Fatalf("the owner got %d packet(s) 100 ms short of a %s walk, want 0:\n%v",
			len(out), due.Round(time.Millisecond), kinds(out))
	}
	if got := len(movesAndStops(b.conn.sent())); got != 1 {
		t.Fatalf("the other player got %d packet(s) before the arrival, want just the "+
			"move: something ended the walk early", got)
	}

	// AT THE ARRIVAL: the box, once, and the stop on the same tick.
	w.began = time.Now().Add(-(due + 2*time.Millisecond))
	out := h.chaseTick(a.sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("the owner got %d packet(s) at the arrival, want the dialog:\n%v",
			len(out), kinds(out))
	}
	seen := movesAndStops(b.conn.sent())
	if len(seen) != 2 || cmdOf(seen[1]) != CmdZCStopPC {
		t.Fatalf("the other player got %d packet(s) at the arrival, want the move then one "+
			"stop:\n%v", len(seen), seen)
	}
	if a.sess.Walk != nil {
		t.Fatal("the walk survived the tick it arrived on")
	}
	if a.sess.Pos != [3]int32{1500, 1000, 77} {
		t.Fatalf("position = %v, want the goal the move named", a.sess.Pos)
	}
}

// TestABlockedWalkGivesUpAndOpensNothing is the ONE honest fallback the piece keeps: a
// walk that cannot arrive never satisfies the distance, so it is written off on its
// budget - the walk's own route time plus ChaseGiveUp - with the existing log line, and
// NOTHING fires.
func TestABlockedWalkGivesUpAndOpensNothing(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}
	w := sess.Walk
	// THE BODY CANNOT GET THERE. The plan estimated the walk at w.eta, but the body is
	// crawling - what a client walking into a wall this side cannot see looks like. Nothing
	// about the estimate changes, which is the point: the distance never says "arrived".
	w.speed = 1.0

	// Inside the budget the walk is still alive: a body that is late is not a body that
	// has failed.
	w.began = time.Now().Add(-(w.eta + ChaseGiveUp/2))
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) inside the give-up budget, want 0", len(out))
	}
	if sess.Walk == nil {
		t.Fatal("the walk was dropped before its budget ran out")
	}

	// Past it: given up, nothing fired, the walk is over.
	w.began = time.Now().Add(-(w.eta + ChaseGiveUp + 100*time.Millisecond))
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) after giving up, want 0 - a box from the wrong place is "+
			"worse than none", len(out))
	}
	if sess.Walk != nil {
		t.Fatal("the walk was not dropped")
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0", *calls)
	}
}

// TestTheBoxAndTheStopShareOneTick is the log-ordering promise the in-game check reads:
// the arrival line and the stop's own line must be the same event - tens of milliseconds
// apart at most, never seconds. Before piece 2d the box was on one clock and the stop on
// another, 1.4x + 250 ms later.
func TestTheBoxAndTheStopShareOneTick(t *testing.T) {
	var buf strings.Builder
	h, a, _ := chasePair(t) // a second player, so the STOP is actually written
	h.Log = log.New(&buf, "", 0)
	// The STOP_PC line is a `-diag-events` diagnostic now, and this test reads its
	// position in the log against the ARRIVED line.
	h.DiagEvents = true
	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := a.sess.Walk
	if w == nil {
		t.Fatal("no walk started")
	}
	w.began = time.Now().Add(-(bodyAge(w) + 2*time.Millisecond))
	h.chaseTick(a.sess)

	lines := strings.Split(buf.String(), "\n")
	arrival, stop := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "ARRIVED at ") {
			arrival = i
		}
		if strings.Contains(ln, "STOP_PC handle") {
			stop = i
		}
	}
	if arrival < 0 {
		t.Fatalf("no ARRIVED line: the in-game check reads that line:\n%s", buf.String())
	}
	if stop < 0 {
		t.Fatalf("no STOP_PC line: the other players were not told:\n%s", buf.String())
	}
	if stop < arrival || stop-arrival > 2 {
		t.Fatalf("the arrival line is at %d and the stop's at %d - they must be the same "+
			"event, adjacent lines:\n%s", arrival, stop, buf.String())
	}
	t.Logf("arrival: %s", lines[arrival])
	t.Logf("stop:    %s", lines[stop])
}

// TestChaseWithoutAnyClientReportStillOpensOnArrival: the same path with no report of any
// kind - which is what a real chase looks like on this client - opens its box on the
// body's distance. The follower-report guard of 2026-11-09 was REVERTED the same day: the
// followers only report when they lag behind (the owner: "they only move if the distance
// is too far"), so a legitimate short walk would hold its box for ever.
func TestChaseWithoutAnyClientReportStillOpensOnArrival(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}
	due := bodyAge(sess.Walk)

	// Before the body is there: nothing, and the script is not run.
	sess.Walk.began = time.Now().Add(-(due - 100*time.Millisecond))
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) before the body arrived, want 0", len(out))
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d before the body arrived, want 0", *calls)
	}

	// At the arrival: the box, on its own.
	sess.Walk.began = time.Now().Add(-(due + 2*time.Millisecond))
	out := h.chaseTick(sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("%d packet(s) at the arrival, want the dialog - a walk with no report "+
			"still has to open its box", len(out))
	}
	if *calls != 1 {
		t.Fatalf("script calls = %d, want 1", *calls)
	}
}

// TestChaseAbandonedWalkOpensNoBox: the player clicked the ground and walked away. The
// client's own report says so, and the box must not appear for a character standing 500
// units from the NPC - the check that used to live on the box's clock lives on the
// arrival now.
func TestChaseAbandonedWalkOpensNoBox(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// The client says the character is 500 units the other way. Sent inside ChaseGrace,
	// so this is recorded as "where the leader is", not read as a new instruction.
	if _, err := h.movePC(sess, moveReport(0, 500, 1000)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil || !sess.Walk.hasReport {
		t.Fatal("the walk did not record the report - this test would prove nothing")
	}

	sess.Walk.began = time.Now().Add(-(bodyAge(sess.Walk) + 2*time.Millisecond))
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) for a walk the client says went elsewhere, want 0 - a box "+
			"opened for a character 500 units from the NPC", len(out))
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0", *calls)
	}
}

// TestAStaleEtaDoesNotDelayTheBox is THE REPRODUCTION the task names, kept next to the
// rule it pins: a walk whose eta FIELD is 10 s while the body covers its route in about a
// second. The box opens on the body, so the eta cannot delay it - and on the old tree it
// fired at 10 s, or at 14.25 s with the pads.
func TestAStaleEtaDoesNotDelayTheBox(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("no walk started")
	}
	body := bodyAge(w)
	if body >= 2*time.Second {
		t.Fatalf("this fixture's real walk is %s - the point of this test is a stale eta "+
			"much larger than the body's walk", body.Round(time.Millisecond))
	}
	w.eta = 10 * time.Second // stale: the plan's estimate, not the body

	// Three seconds in: the old tree is still 11 s away from its box; the body has been
	// there since ~1 s.
	w.began = time.Now().Add(-3 * time.Second)
	out := h.chaseTick(sess)
	if len(out) == 0 || cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("%d packet(s) 3 s into a walk whose body needs %s, want the dialog - the "+
			"box is still reading the eta field (%s)", len(out),
			body.Round(time.Millisecond), w.eta)
	}
	if *calls != 1 {
		t.Fatalf("script calls = %d, want 1", *calls)
	}
	t.Logf("body %s, eta field %s, box opened 3 s in", body.Round(time.Millisecond), w.eta)
}

// ---- A SWITCH IN FLIGHT CANCELS THE BOX, NOT THE WALK -----------------------
//
// The owner's capture, decoded (captures\leaderdlg\20261103-200921, src\
// decode_zone_capture.py, timestamps from capture.txt):
//
//	12.43 s  ZC_DIALOG_SELECT, 13.87 s ZC_DIALOG_CLOSE   - the first click, no switch:
//	         the box opened normally
//	then     CZ_CLICK_TRIGGER (3679) again, and a CZ_SELECTCHANGE_PC (3652)
//	after    NO ZC_DIALOG_SELECT (3663) anywhere in the rest of the stream, while the
//	         walk itself ran (a 3548 echo at 23.57 s)
//	owner    "walk complete when change leader but no dialog"
//
// So the switch cancels the DIALOG REQUEST and nothing else: the body walks to the goal,
// the stop and the echoes are unchanged, and the arrival opens nothing.

// switchClickSession is a TWO-character session: a 3652 can only name a slot the team
// has, and the whole rule is about switching to one of them.
func switchClickSession(t *testing.T) (*Handler, *int, *player, *player) {
	t.Helper()
	h, calls := chaseHandler(t)
	a := newTeam([]uint32{0x40000071, 0x40000073},
		[][3]int32{{1000, 1000, 5}, {1700, 1000, 5}})
	b := newPlayer(0x40000072, "B", [3]int32{2000, 2000, 5})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil
	return h, calls, a, b
}

// TestASwitchCancelsTheBoxButKeepsTheWalk is the whole rule in one test: an out-of-range
// click starts a walk, a real 3652 arrives while it is in flight, the walk still ends on
// its own arrival and brings the other player its stop - and NO 3663 is handed back.
func TestASwitchCancelsTheBoxButKeepsTheWalk(t *testing.T) {
	var buf bytes.Buffer
	h, calls, a, b := switchClickSession(t)
	h.Log = log.New(&buf, "", 0)
	sess := a.sess

	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("no walk started - this test would prove nothing")
	}
	if w.caster != 0 {
		t.Fatalf("the click walks slot %d, want the driven slot 0", w.caster)
	}

	// THE SWITCH, through the real dispatch, exactly as the client sends it.
	out, err := h.Reply(context.Background(), sess, selectFrame(1))
	if err != nil {
		t.Fatalf("Reply(SELECTCHANGE_PC 1): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("the switch answered %d packet(s), want none", len(out))
	}
	if sess.Control != 1 {
		t.Fatalf("the session drives slot %d after the 3652, want 1", sess.Control)
	}
	const want = "CANCELLED by the switch"
	if line := lineWith(buf.String(), want); line == "" {
		t.Fatalf("the cancellation printed no line containing %q:\n%s", want, buf.String())
	} else {
		t.Logf("cancel line: %s", line)
	}
	if sess.Walk != w {
		t.Fatal("the switch DROPPED the click's walk - the character has to finish it")
	}

	// THE ARRIVAL: the body is there, the walk ends, no box. Both box packets are checked:
	// this town NPC's script answers with ZC_DIALOG_OK (3660), and the capture's was the
	// selection list ZC_DIALOG_SELECT (3663) - neither may appear.
	w.began = time.Now().Add(-(bodyAge(w) + 2*time.Millisecond))
	got := h.chaseTick(sess)
	for _, p := range got {
		if cmdOf(p) == CmdZCDialogOK || cmdOf(p) == CmdZCDialogSel {
			t.Fatalf("a dialog opened after the switch: %v", kinds(got))
		}
	}
	if sess.Walk != nil {
		t.Fatalf("the walk survived its own arrival (%s)", sess.Walk.describe())
	}
	if *calls != 0 {
		t.Fatalf("the click script ran %d time(s) for a cancelled click, want 0", *calls)
	}

	// THE STOP IS UNCHANGED: the other player got the move and then exactly one stop.
	seen := movesAndStops(b.conn.sent())
	if len(seen) != 2 || cmdOf(seen[0]) != CmdZCMovePC || cmdOf(seen[1]) != CmdZCStopPC {
		t.Fatalf("the other player got %d packet(s), want the move then one stop:\n%v",
			len(seen), seen)
	}
}

// TestASwitchLeavesAnAttackApproachAlone is the boundary of the rule: the same 3652 while
// a NORMAL ATTACK's walk is in flight must not cancel the hit. The switch names the
// walker's own slot (a player taking control of the character that pressed), so the walk
// is not an approach "belonging to somebody else" either - it is exactly the state the
// attack lane owns, and this rule must not touch it.
func TestASwitchLeavesAnAttackApproachAlone(t *testing.T) {
	h, logs, p := attackZoneAt(t, 10560, [3]int32{-919, 1911, 136})
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, target, standoff)
	}
	// A second character, so the 3652 has somewhere to point.
	p.sess.Team = append(p.sess.Team, Character{
		Handle: 0x40000091, Name: "Second", Job: 1, Level: 9, HP: 120, MaxHP: 200,
		Stance: 10560, Position: [3]int32{-919, 1911, 136},
	})
	p.sess.Team[1].Equip[5] = 7201

	// THE FOLLOWER PRESSES, and the player then takes control of that follower.
	if out := pressAttackAs(t, h, p.sess, 1, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1 - no approach to time:\n%s",
			moves(out), logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil || w.caster != 1 {
		t.Fatal("no attack walk for slot 1 was left on the session")
	}

	if _, err := h.Reply(context.Background(), p.sess, selectFrame(1)); err != nil {
		t.Fatalf("Reply(SELECTCHANGE_PC 1): %v", err)
	}
	if p.sess.Walk != w {
		t.Fatal("the switch dropped the attack's walk - this rule is about the NPC box")
	}

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
		t.Fatalf("the arrival after a 3652 fired %d hit(s), want 1 - the switch touched "+
			"the attack:\n%s", fired, logs.String())
	}
	if line := lineWith(logs.String(), "CANCELLED by the switch"); line != "" {
		t.Fatalf("the attack's approach was cancelled as if it were a click: %s", line)
	}
}

// TestArrivalSendsTheWalkersOwnPointToTheTeamRecord is the owner's own report, in game
// 2026-11-03 20:52: "character 1 is near the NPC, but if i change leader to character 2
// and then change back to 1 and click the NPC, there will be a visible delay".
//
// The cause was one record not written. A click by the DRIVEN character landed `Pos` and
// left Team[caster].Position on the pre-walk spot; a CZ_SELECTCHANGE_PC re-seats `Pos`
// from that record (move.go, closeLegForControlSwitch), so switching away and back put
// the session 2,636 units behind the body and the next click walked the whole leg again.
// This test walks, arrives, switches away AND back, and clicks again: the second click
// must be answered from where the character stands.
func TestArrivalSendsTheWalkersOwnPointToTheTeamRecord(t *testing.T) {
	h, calls, a, _ := switchClickSession(t)
	sess := a.sess
	const handle = 0x40000031 // chaseHandler's NPC, 500 units from slot 0's stand point

	// 1. The click from out of reach starts the walk, as in the owner's session.
	if _, err := h.clickTrigger(sess, frameClick(0, handle)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("the click did not start a walk - this test would prove nothing")
	}
	goal := w.goal

	// 2. The body arrives on its own clock: the box opens and BOTH records must follow.
	w.began = time.Now().Add(-(bodyAge(w) + 2*time.Millisecond))
	got := h.chaseTick(sess)
	if *calls != 1 {
		t.Fatalf("the arrival ran the click script %d time(s), want 1", *calls)
	}
	found := false
	for _, p := range got {
		if cmdOf(p) == CmdZCDialogOK || cmdOf(p) == CmdZCDialogSel {
			found = true
		}
	}
	if !found {
		t.Fatalf("the arrival opened no box: %v", kinds(got))
	}
	if sess.Pos != goal {
		t.Fatalf("after the arrival Pos is %v, want the goal %v", sess.Pos, goal)
	}
	if sess.Team[0].Position != goal {
		t.Fatalf("after the arrival Team[0].Position is %v, want the goal %v - a switch "+
			"re-seats Pos from this record, so a stale one puts the character back where "+
			"it started and the next click walks the whole leg again",
			sess.Team[0].Position, goal)
	}

	// 3. The owner's sequence: drive character 2, then come back to character 1.
	for _, slot := range []byte{1, 0} {
		if _, err := h.Reply(context.Background(), sess, selectFrame(slot)); err != nil {
			t.Fatalf("Reply(SELECTCHANGE_PC %d): %v", slot, err)
		}
	}
	if sess.Pos != goal {
		t.Fatalf("the round trip through another leader left Pos at %v, want %v",
			sess.Pos, goal)
	}

	// 4. The click again, standing next to the NPC: NO walk, a box at once.
	out, err := h.clickTrigger(sess, frameClick(0, handle))
	if err != nil {
		t.Fatal(err)
	}
	if sess.Walk != nil {
		t.Fatalf("the click after the switch started a walk to %s - the character is "+
			"already standing at the NPC", sess.Walk.describe())
	}
	found = false
	for _, p := range out {
		if cmdOf(p) == CmdZCDialogOK || cmdOf(p) == CmdZCDialogSel {
			found = true
		}
	}
	if !found {
		t.Fatalf("the click after the switch opened no box: %v", kinds(out))
	}
}
