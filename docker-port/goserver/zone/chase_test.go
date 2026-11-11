package zone

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// chaseHandler is a zone with one NPC 500 units from where the player stands, and a
// script engine that always says one line.
func chaseHandler(t *testing.T) (*Handler, *int) {
	calls := 0
	npc := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77,
		ClassType: "REB_SERM", Script: "NPC_TAXMAN"}
	h := &Handler{
		Log:  log.New(io.Discard, "", 0),
		NPCs: []NPC{npc},
		// THE ENGINE, wired as the product does: a plan with a real polyline. A click with
		// NO planner is REFUSED since 2026-11-09 ("the path engine is the only planner"),
		// so a test that wants "no engine at all" must set h.Plan = nil itself. The fake
		// answers the NPC's OWN spot (the old chord's destination) so the assertions these
		// tests were written around (goal == the NPC) keep their meaning.
		Plan: func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
			return npc, [][3]int32{from, npc}, flatDist(from, npc), true, true, nil
		},
		Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
			calls++
			return ClickReply{DialogKind: ClickDialogOK, Text: "hello", Fade: 1}, nil
		},
	}
	return h, &calls
}

func chaseSession() *Session {
	return &Session{
		Pos:  [3]int32{1000, 1000, 5},
		Team: []Character{{Handle: 7, Name: "Tester", Level: 3}},
	}
}

// TestChaseLogsTheSectorAndTheApproach is the evidence line the next in-game test reads
// out of logs\zone15_live.log: the sector the stand-off sits in, and the side the player
// came from, in degrees and in words.
func TestChaseLogsTheSectorAndTheApproach(t *testing.T) {
	var buf bytes.Buffer
	npc := NPC{Handle: 0x40000006, X: 3065, Y: 17062, Cell: 233,
		ClassType: "WAYPOINT_ONROAD/WAYPOINT_AUCH_BASIC"}
	h := &Handler{
		Log:  log.New(&buf, "", 0),
		NPCs: []NPC{npc},
		Plan: func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
			return [3]int32{2966, 17161, 216}, nil, 414, false, true, nil // NW, the player's side
		},
	}
	sess := &Session{
		Pos:  [3]int32{2833, 17244, 216}, // the player, NORTH-WEST of the waypoint
		Team: []Character{{Handle: 7, Name: "Tester", Level: 3}},
	}
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000006)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"is sector 135 deg (NW side", "the approach is from 135 deg (NW side"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log is missing %q:\n%s", want, out)
		}
	}
}

// TestChaseAimsFromWalkedPosNotTheClickDestination: Pos is the DESTINATION of the
// player's own click (geZCPacket.h:1133), not where the character stands. A click on an
// NPC made MID-WALK must therefore aim the stand-off from walkedPos, or the character is
// sent to a side chosen for a place it has not reached yet (R58).
func TestChaseAimsFromWalkedPosNotTheClickDestination(t *testing.T) {
	h, _ := chaseHandler(t)
	var got [3]int32
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		got = from
		return [3]int32{1370, 1010, 70}, nil, 620, false, true, nil
	}
	sess := chaseSession()
	// A walk segment is running: it started at (1000,1000), the player clicked
	// (2000,1000), and at 400 u/s the character is half way, at (1500,1000).
	sess.moveFrom = [3]int32{1000, 1000, 5}
	sess.Pos = [3]int32{2000, 1000, 6}
	sess.MoveSpeed = 400
	sess.moveSamples = 4
	sess.moveAt = time.Now().Add(-1250 * time.Millisecond) // 500 units travelled

	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if got[0] == sess.Pos[0] {
		t.Fatalf("the stand-off was aimed from the click DESTINATION (%d) instead of "+
			"where the character stands", got[0])
	}
	if got[0] < 1480 || got[0] > 1520 || got[1] != 1000 {
		t.Fatalf("approach point = %v, want about (1500,1000): where the character "+
			"actually is half way along its own walk", got)
	}
}

// TestChaseAimsFromPosWhenStandingStill: with no walk segment running, Pos IS the truth,
// so the approach point must be Pos - the walkedPos rule must not invent motion.
func TestChaseAimsFromPosWhenStandingStill(t *testing.T) {
	h, _ := chaseHandler(t)
	var got [3]int32
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		got = from
		return [3]int32{1370, 1010, 70}, nil, 620, false, true, nil
	}
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if got != sess.Pos {
		t.Fatalf("approach point = %v, want the standing position %v", got, sess.Pos)
	}
}

// TestChaseSendsExactlyOneMove is the rule the live server taught us
// (history\zone-npc-click.md): ONE move packet for the whole walk. Legs and
// repeats make the client turn and restart, which reads as stop-go jitter.
func TestChaseSendsExactlyOneMove(t *testing.T) {
	h, calls := chaseHandler(t)
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return [3]int32{1370, 1010, 70}, nil, 620, false, true, nil
	}
	sess := chaseSession()

	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want exactly 1", len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCMovePC {
		t.Fatalf("cmd = %d, want ZC_MOVE_PC", cmd)
	}
	if reason := uint32(out[0][26]); reason != ReasonDialog {
		t.Fatalf("reason = %d, want MR_DIALOG %d", reason, ReasonDialog)
	}
	if x := int32(le32(out[0][10:])); x != 1370 {
		t.Fatalf("destination x = %d, want the planned goal 1370", x)
	}

	// Mid-walk: NOTHING may be sent - a second packet re-targets the actor.
	if mid := h.chaseTick(sess); len(mid) != 0 {
		t.Fatalf("%d packet(s) mid-walk, want 0", len(mid))
	}
	if *calls != 0 {
		t.Fatal("the dialog opened before the walk finished")
	}
}

// TestChaseClampHitOverlapsTheNPC: when the clamp is shortened, something is pressed
// against the NPC, the client refuses those poses, and the live server sends the NPC's
// OWN position instead. The character then touches the NPC - that overlap is normal.
func TestChaseClampHitOverlapsTheNPC(t *testing.T) {
	h, _ := chaseHandler(t)
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return npc, nil, 500, true, true, nil // overlap on purpose
	}
	sess := chaseSession()
	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want 1", len(out))
	}
	if x, y := int32(le32(out[0][10:])), int32(le32(out[0][14:])); x != 1500 || y != 1000 {
		t.Fatalf("destination = (%d,%d), want the NPC's own spot (1500,1000)", x, y)
	}
	if sess.Walk == nil || !sess.Walk.overlap {
		t.Fatal("the walk was not marked as an overlap")
	}
}

// TestChaseRefusedWhenPlayerGroundInvalid: the live sends NOTHING when the clamp says
// the player is not standing on valid ground.
func TestChaseRefusedWhenPlayerGroundInvalid(t *testing.T) {
	h, calls := chaseHandler(t)
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return npc, nil, 0, true, false, nil
	}
	sess := chaseSession()
	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) when the player's ground is invalid, want 0", len(out))
	}
	if sess.Walk != nil {
		t.Fatal("a refused click left a walk behind")
	}
	if *calls != 0 {
		t.Fatal("the script ran for a refused click")
	}
}

// TestChaseOpensDialogOnArrival (WAS TestChaseOpensDialogOnTheClock): the client says
// nothing during the walk, so the box opens when the BODY'S DISTANCE says the character
// is there - and not before (piece 2d: there is no clock left to open it).
func TestChaseOpensDialogOnArrival(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}

	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) before the clock, want 0", len(out))
	}

	// Walk the body to its goal: one tick past the time its own route needs. The arrival
	// is a distance, so this is the only thing that decides it.
	sess.Walk.began = time.Now().Add(-(bodyAge(sess.Walk) + 2*time.Millisecond))
	out := h.chaseTick(sess)
	if len(out) != 2 {
		t.Fatalf("%d packet(s) on arrival, want 2 (dialog + click voice)", len(out))
	}
	if cmdOf(out[0]) != CmdZCDialogOK {
		t.Fatalf("cmd = %d, want DIALOG_OK", cmdOf(out[0]))
	}
	if sess.Walk != nil {
		t.Fatal("the walk was not cleared on arrival")
	}
	if sess.Pos != [3]int32{1500, 1000, 77} {
		t.Fatalf("position = %v, want the packet's destination", sess.Pos)
	}
	if *calls != 1 {
		t.Fatalf("script calls = %d, want 1", *calls)
	}
}

// TestClickWhileWalkingStartsAtOnce (WAS TestClickWhileWalkingWaits, which asserted the
// opposite): a click on an out-of-reach NPC no longer waits for the client to go quiet.
// The reference starts AS_DIALOG_CHASE on the click itself (PlayerChar_Request.cpp:436-444
// -> PlayerChar_FSM.cpp:340-372), and PlayerIdleWindow - the 250 ms wait - is deleted
// with piece 2d. The tail of the player's own walk is handled by ChaseGrace instead.
func TestClickWhileWalkingStartsAtOnce(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	sess.LastMoveAt = time.Now() // the client reported a move a moment ago

	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("the click answered %d packet(s) (%v), want the one ZC_MOVE_PC at once",
			len(out), kinds(out))
	}
	if sess.Walk == nil {
		t.Fatal("the click did not start a walk")
	}
	if *calls != 0 {
		t.Fatal("the dialog opened at the start of the walk")
	}
}

// TestChaseCancelIgnoredInsideGrace: the tail of the player's own walk arrives right
// after our move and must NOT cancel it.
func TestChaseCancelIgnoredInsideGrace(t *testing.T) {
	h, _ := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}

	pt := pack(CmdCZMovePC, 0, 0x11, 0x22, 0, 0, 0x0a, 0, 0, 0, 0x01, 0, 0, 0)
	if _, err := h.movePC(sess, protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt}); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("a move report inside the grace window cancelled the walk")
	}
}

// moveReport is one CZ_MOVE_PC from a character at (x, y).
func moveReport(idx int, x, y int32) protocol.Frame {
	pt := pack(CmdCZMovePC, byte(idx),
		byte(x), byte(x>>8), byte(x>>16), byte(x>>24),
		byte(y), byte(y>>8), byte(y>>16), byte(y>>24),
		5, 0, 0, 0)
	return protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt}
}

// TestAClickAfterAPlayersMoveReplacesTheWalk is the owner's own sequence (2026-11-09):
// click 1 walks and he cancels it; the stale walk must not swallow click 2 ("repeats the
// click this walk is already serving - absorbed: no packet"). The walk is MARKED
// takenOver by the order and the next click REPLACES it, so a packet goes out.
func TestAClickAfterAPlayersMoveReplacesTheWalk(t *testing.T) {
	h, _ := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("the click did not start a walk")
	}
	// A report far off the walk's line is the player's order: it marks the walk, it does
	// not cancel it (a report inside ChaseGrace may still be the tail of his own walk).
	if _, err := h.movePC(sess, moveReport(0, 900, 900)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil || !sess.Walk.takenOver {
		t.Fatalf("the player's move order did not mark the click walk: %+v", sess.Walk)
	}
	// The next click on the SAME NPC must REPLACE it and send a packet.
	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the click after the player's move was absorbed - no packet went out")
	}
}

// TestLeaderWalkingAwayCancels: the player clicked the ground, so the leader's reports
// move AWAY from the goal and the walk must stop - otherwise the box opens later anyway,
// which is what "the dialog fired even though I cancelled" was.
func TestLeaderWalkingAwayCancels(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	sess.Walk.sentAt = time.Now().Add(-2 * ChaseGrace) // past the tail window

	// First report: 500 units from the goal (1500,1000). Records the distance only.
	if _, err := h.movePC(sess, moveReport(0, 1000, 1000)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("one report cancelled the walk - jitter would cancel every real walk")
	}

	// Second report: 200 units FARTHER. That is the player going elsewhere.
	//
	// NO GUARD STAMP IS NEEDED any more: the move guard (zone\move_guard.go) is an
	// alarm at MoveGuardAlarmDistance, not a clamp, so it bounds nothing and this report
	// is honoured exactly as sent however soon it follows the previous one. Every
	// assertion below is unchanged.
	if _, err := h.movePC(sess, moveReport(0, 800, 1000)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk != nil {
		t.Fatal("walking away did not cancel the walk")
	}
	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatal("a cancelled walk still produced a dialog")
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d after cancel, want 0", *calls)
	}
}

// TestFollowerMoveDoesNotCancel is the bug that made every walk stop 2-3 seconds in:
// the followers stream move reports the whole time, and one of those (`char 2`) was
// being read as "the player took control".
func TestFollowerMoveDoesNotCancel(t *testing.T) {
	h, _ := chaseHandler(t)
	sess := chaseSession()
	sess.Team = []Character{{Handle: 7}, {Handle: 40}, {Handle: 38}} // leader + 2 followers
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	sess.Walk.sentAt = time.Now().Add(-2 * ChaseGrace) // well past the tail window

	// A follower walks away: ignored entirely.
	if _, err := h.movePC(sess, moveReport(2, 1000, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.movePC(sess, moveReport(2, 800, 1000)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("a FOLLOWER's move report cancelled the leader's walk")
	}

	// The leader, walking away from the same reports: that one cancels.
	if _, err := h.movePC(sess, moveReport(0, 1000, 1000)); err != nil {
		t.Fatal(err)
	}
	// The move guard (zone\move_guard.go) is an alarm, not a clamp, so this 200-unit
	// report is honoured as sent; the assertions are unchanged.
	if _, err := h.movePC(sess, moveReport(0, 800, 1000)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk != nil {
		t.Fatal("the LEADER walking away did not cancel the walk")
	}
}

// TestAnotherNpcIsArmedFirstAndRedirectsOnTheSecondClick: the reference's CLICK_LOCK
// (history\zone-npc-click.md, 2026-10-16). A click on a SECOND NPC while the walk to the
// first is in flight does NOT redirect the actor - it arms the new NPC, drops the box the
// current walk would open, and sends nothing. The second click on that same NPC stops the
// walk where the body is and redirects it there.
func TestAnotherNpcIsArmedFirstAndRedirectsOnTheSecondClick(t *testing.T) {
	h, calls := chaseHandler(t)
	first := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	second := NPC{Handle: 0x40000032, X: 1000, Y: 1800, Cell: 90, ClassType: "B", Script: "TWO"}
	h.NPCs = []NPC{first, second}

	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, first.Handle)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != first.Handle {
		t.Fatal("the first walk did not start")
	}

	// THE FIRST CLICK ON B: armed, no packet, the walk to A keeps going, A's box is dropped.
	out, err := h.clickTrigger(sess, frameClick(0, second.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) for the first click on B, want 0 - it only arms B", len(out))
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != first.Handle {
		t.Fatal("the walk to A was replaced instead of being kept")
	}
	if !sess.Walk.dialogCleared {
		t.Fatal("A's box was not dropped when B was armed")
	}
	if sess.ClickLockArmed != second.Handle {
		t.Fatalf("armed handle = 0x%08x, want B 0x%08x", sess.ClickLockArmed, second.Handle)
	}

	// THE SECOND CLICK ON B: the walk stops where the body is and B is chased.
	out, err = h.clickTrigger(sess, frameClick(0, second.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s) for the second click on B, want 1 (the redirect)", len(out))
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != second.Handle {
		t.Fatal("the second click on B did not redirect the walk")
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0 - no dialog opens on the way", *calls)
	}
}

// TestARepeatClickOnTheSameNPCIsAbsorbed: the client re-sends a click several times a
// second while the character walks (measured 2026-11-03 20:36: 40 packets in 3.76 s).
// The second one is the SAME order, so nothing is sent and the walk keeps its own clock -
// re-planning used to move the stand-off around the NPC and restart the arrival clock,
// which was the owner's "dialog delay".
func TestARepeatClickOnTheSameNPCIsAbsorbed(t *testing.T) {
	h, _ := chaseHandler(t)
	var buf bytes.Buffer
	h.Log = log.New(&buf, "", 0)
	npc := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	h.NPCs = []NPC{npc}

	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, npc.Handle)); err != nil {
		t.Fatal(err)
	}
	first := sess.Walk
	if first == nil {
		t.Fatal("the first click did not start a walk")
	}
	goal, began := first.goal, first.began

	for i := 0; i < 5; i++ {
		out, err := h.clickTrigger(sess, frameClick(0, npc.Handle))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 0 {
			t.Fatalf("repeat click %d sent %d packet(s), want 0 - the walk already "+
				"serves this click", i+2, len(out))
		}
	}
	if sess.Walk != first {
		t.Fatal("the repeat clicks replaced the walk instead of leaving it alone")
	}
	if sess.Walk.goal != goal || !sess.Walk.began.Equal(began) {
		t.Fatalf("the walk was re-planned: goal %v -> %v, began %v -> %v",
			goal, sess.Walk.goal, began, sess.Walk.began)
	}
	if !strings.Contains(buf.String(), "repeats the click this walk is already serving") {
		t.Fatalf("the absorb was not logged; log:\n%s", buf.String())
	}
}

// TestARepeatClickReArmsTheBoxALeaderSwitchCleared: the switch cancels the DIALOG, not
// the walk. If the player clicks the same NPC again while the walk is still going, the
// box must come back on arrival WITHOUT a second walk - otherwise the fix for the repeat
// click would leave a switched-away click unable to open anything at all.
func TestARepeatClickReArmsTheBoxALeaderSwitchCleared(t *testing.T) {
	h, calls := chaseHandler(t)
	npc := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	h.NPCs = []NPC{npc}

	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, npc.Handle)); err != nil {
		t.Fatal(err)
	}
	h.cancelWalkDialog(sess)
	if !sess.Walk.dialogCleared {
		t.Fatal("the switch did not clear the box")
	}
	began := sess.Walk.began

	out, err := h.clickTrigger(sess, frameClick(0, npc.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("the re-arming click sent %d packet(s), want 0 - the walk is still "+
			"in flight and keeps its clock", len(out))
	}
	if sess.Walk == nil || sess.Walk.dialogCleared {
		t.Fatal("the click did not re-arm the box")
	}
	if !sess.Walk.began.Equal(began) {
		t.Fatal("re-arming restarted the arrival clock")
	}
	// And the box really does open: the body arrives on its own clock.
	sess.Walk.began = time.Now().Add(-(bodyAge(sess.Walk) + 2*time.Millisecond))
	if out := h.chaseTick(sess); len(out) == 0 {
		t.Fatal("no packet on arrival though the box was re-armed")
	}
	if *calls != 1 {
		t.Fatalf("script calls = %d, want the dialog on arrival", *calls)
	}
}

// TestLeaderArrivalOpensTheDialog: a report INSIDE the arrival radius opens the box at
// once. The client's own position is a better arrival signal than any timer.
func TestLeaderArrivalOpensTheDialog(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// 40 units from the goal (1500,1000): the client's own stop distance.
	out, err := h.movePC(sess, moveReport(0, 1500, 1040))
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("script calls = %d, want the dialog on arrival", *calls)
	}
	if sess.Walk != nil {
		t.Fatal("the walk was not cleared on arrival")
	}
	// The echo of the move comes first, then the dialog and the click voice.
	if len(out) != 3 || cmdOf(out[1]) != CmdZCDialogOK {
		t.Fatalf("packets = %d, want echo + dialog + voice", len(out))
	}
}

// TestArrivalRefusesWhenClientIsElsewhere (WAS TestClockRefusesWhenClientIsElsewhere): if
// the body arrives but the client's last report puts the character far from the goal, NO
// dialog - the player cancelled.
func TestArrivalRefusesWhenClientIsElsewhere(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// One report 500 units out, then let the body reach its goal.
	if _, err := h.movePC(sess, moveReport(0, 1000, 1000)); err != nil {
		t.Fatal(err)
	}
	sess.Walk.began = time.Now().Add(-(bodyAge(sess.Walk) + 2*time.Millisecond))

	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) though the client says it is elsewhere, want 0", len(out))
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0", *calls)
	}
	if sess.Walk != nil {
		t.Fatal("the stale walk was not dropped")
	}
}

// TestChaseGivesUp: the client can stop early against something this side cannot see.
// A box opened from the wrong place is worse than no box, so the click is dropped.
func TestChaseGivesUp(t *testing.T) {
	h, calls := chaseHandler(t)
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// THE BODY CANNOT ARRIVE: the plan estimated the walk, but the body is crawling, so
	// the distance never says it got there. That is the only walk that may be given up.
	sess.Walk.speed = 1.0
	sess.Walk.began = time.Now().Add(-(sess.Walk.eta + ChaseGiveUp + 100*time.Millisecond))

	if out := h.chaseTick(sess); len(out) != 0 {
		t.Fatalf("%d packet(s) after giving up, want 0", len(out))
	}
	if sess.Walk != nil {
		t.Fatal("the walk was not dropped")
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0", *calls)
	}
}

// TestChaseRefusesLongWalkWithoutEngine: no path engine and a long distance means the
// destination is a guess, so nothing is sent.
func TestChaseRefusesLongWalkWithoutEngine(t *testing.T) {
	h, calls := chaseHandler(t)
	h.Plan = nil // NO engine at all: since 2026-11-09 that refuses EVERY click, not just a long one
	sess := chaseSession()
	sess.Pos = [3]int32{-5000, 1000, 5} // 6500 units away

	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || sess.Walk != nil || *calls != 0 {
		t.Fatalf("a %.0f-unit walk without an engine was sent", MaxChase)
	}
}

// TestSpeedIsMeasured: the clock must use the speed the CLIENT actually walks at - but
// only when the measurement is trustworthy. Two reports taken a moment apart once read
// as 48 u/s, the character crawled, and the eta said 79 s for a 3817-unit walk.
func TestSpeedIsMeasured(t *testing.T) {
	sess := &Session{}
	base := time.Now()

	// 1600 units in 4 s, in 400-unit steps: 400 u/s, which is a plausible run.
	sess.observeSpeed(0, [3]int32{1000, 1000, 1}, base)
	sess.observeSpeed(0, [3]int32{1400, 1000, 1}, base.Add(time.Second))
	sess.observeSpeed(0, [3]int32{1800, 1000, 1}, base.Add(2*time.Second))
	sess.observeSpeed(0, [3]int32{2200, 1000, 1}, base.Add(3*time.Second))
	sess.observeSpeed(0, [3]int32{2600, 1000, 1}, base.Add(4*time.Second))

	if sess.MoveSpeed < 380 || sess.MoveSpeed > 420 {
		t.Fatalf("measured speed = %.0f u/s, want about 400", sess.MoveSpeed)
	}
	if sess.moveSamples < 4 {
		t.Fatalf("samples = %d, want at least 4", sess.moveSamples)
	}
	if got := sess.walkSpeed(); got != sess.MoveSpeed {
		t.Fatalf("walkSpeed = %.0f, want the measured value", got)
	}

	// A duplicated or corner report must not drag the estimate around.
	sess.observeSpeed(0, [3]int32{2600, 1000, 1}, base.Add(4100*time.Millisecond))
	if sess.MoveSpeed < 380 || sess.MoveSpeed > 420 {
		t.Fatalf("a zero-distance report moved the estimate to %.0f", sess.MoveSpeed)
	}

	// Another character's report starts a new sample instead of a cross-character jump.
	sess.observeSpeed(1, [3]int32{9000, 9000, 1}, base.Add(5*time.Second))
	if sess.MoveSpeed < 380 || sess.MoveSpeed > 420 {
		t.Fatalf("another character's report changed the estimate to %.0f", sess.MoveSpeed)
	}
}

// TestLowMeasuredSpeedIsIgnored is the crawl of 2026-10-25: two reports a moment apart
// read as 48 u/s, and the clock believed it.
func TestLowMeasuredSpeedIsIgnored(t *testing.T) {
	sess := &Session{}
	base := time.Now()
	sess.observeSpeed(0, [3]int32{1000, 1000, 1}, base)
	sess.observeSpeed(0, [3]int32{1048, 1000, 1}, base.Add(time.Second)) // 48 u/s

	if sess.MoveSpeed == 0 {
		t.Fatal("the sample was not even recorded - this test would prove nothing")
	}
	if got := sess.walkSpeed(); got != MoveBaseSpeed {
		t.Fatalf("walkSpeed = %.0f for a 48 u/s measurement, want the base %.0f",
			got, MoveBaseSpeed)
	}

	// And the packet never carries a measurement at all: it is the character's own
	// MSPD, which for a character with no stance on this side is the base 469.
	if got := (&Handler{}).packetSpeed(nil); got != MoveBaseSpeed {
		t.Fatalf("packetSpeed = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
}

// TestWalkSpeedNeedsEnoughSamples: one good sample is still a guess.
func TestWalkSpeedNeedsEnoughSamples(t *testing.T) {
	sess := &Session{}
	base := time.Now()
	sess.observeSpeed(0, [3]int32{1000, 1000, 1}, base)
	sess.observeSpeed(0, [3]int32{1400, 1000, 1}, base.Add(time.Second))
	sess.observeSpeed(0, [3]int32{1800, 1000, 1}, base.Add(2*time.Second))

	if sess.moveSamples != 2 {
		t.Fatalf("samples = %d, want 2", sess.moveSamples)
	}
	if got := sess.walkSpeed(); got != MoveBaseSpeed {
		t.Fatalf("walkSpeed = %.0f with only 2 samples, want the base", got)
	}
}

// le32 reads the little-endian word a move packet carries.
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// ---- R63: the chase the OTHER player can see ---------------------------------
//
// These four tests are the observer's side of a chase. Before R63 the chase was the
// owner's alone: `startChase` returned one ZC_MOVE_PC for the clicker, and the other
// clients only ever heard the ZC_STOP_PC at the end - so their copy of the actor
// stood still and then TELEPORTED to the NPC.

// chasePair is one player who is about to click an NPC (A) and one who is watching
// (B), both already in the zone, with both connections emptied.
func chasePair(t *testing.T) (*Handler, *player, *player) {
	t.Helper()
	h, _ := chaseHandler(t)
	a := newPlayer(0x40000071, "A", [3]int32{1000, 1000, 5})
	b := newPlayer(0x40000072, "B", [3]int32{2000, 2000, 5})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil
	return h, a, b
}

// movesAndStops is what a connection was written for the walk, in order, keeping only
// the two packets this lane is about.
func movesAndStops(packets [][]byte) [][]byte {
	var out [][]byte
	for _, p := range packets {
		if len(p) >= 2 && (cmdOf(p) == CmdZCMovePC || cmdOf(p) == CmdZCStopPC) {
			out = append(out, p)
		}
	}
	return out
}

// TestChaseMoveIsBroadcastUnchanged: the click that starts a walk to an NPC puts the
// SAME ZC_MOVE_PC on the other player's wire, byte for byte, at the same moment the
// owner's copy is built - and the owner's own packet is untouched.
//
// This is symptom 1 of R63: with no move announced, the observer's actor cannot
// animate the walk at all.
func TestChaseMoveIsBroadcastUnchanged(t *testing.T) {
	h, a, b := chasePair(t)

	out, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("the owner got %d packet(s), want exactly 1 move", len(out))
	}
	if cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("the owner's packet is cmd %d, want ZC_MOVE_PC", cmdOf(out[0]))
	}

	seen := movesAndStops(b.conn.sent())
	if len(seen) != 1 {
		t.Fatalf("the other player got %d packet(s) for the chase, want exactly 1 move",
			len(seen))
	}
	if cmdOf(seen[0]) != CmdZCMovePC {
		t.Fatalf("the other player got cmd %d, want ZC_MOVE_PC", cmdOf(seen[0]))
	}
	if !bytes.Equal(seen[0], out[0]) {
		t.Fatalf("the two clients got DIFFERENT move packets:\nowner    %x\nobserver %x",
			out[0], seen[0])
	}
	// The owner's own bytes must be untouched by the fan-out: still a 30-byte
	// MR_DIALOG move to the planned goal.
	if len(out[0]) != 30 {
		t.Fatalf("the owner's move is %d bytes, want 30", len(out[0]))
	}
	if reason := le32At(out[0], 26); reason != ReasonDialog {
		t.Fatalf("the owner's reason = %d, want MR_DIALOG %d", reason, ReasonDialog)
	}
	if x := int32(le32At(out[0], 10)); x != 1500 {
		t.Fatalf("the owner's destination x = %d, want the goal 1500", x)
	}
}

// le32At is the little-endian word at off, for reading a packet field in a test.
func le32At(p []byte, off int) uint32 { return le32(p[off:]) }

// TestChaseStopsTheOthersOnceOnTheClock: when the walk is over by the arrival rule,
// the other player gets exactly ONE ZC_STOP_PC naming the goal - after the move, not
// before it, and for the same handle.
func TestChaseStopsTheOthersOnceOnTheClock(t *testing.T) {
	h, a, b := chasePair(t)
	handle := a.sess.Team[0].Handle

	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// Mid-walk: nothing may be stopped.
	seen := movesAndStops(b.conn.sent())
	if len(seen) != 1 {
		t.Fatalf("%d packet(s) mid-walk, want just the move", len(seen))
	}

	// Age the walk past its ARRIVAL. This walk is 500 units: eta 1.07 s, so 3 s is a
	// body that got there, and it is well inside the give-up budget (eta + 6 s = 7.07 s).
	// A fixed age rather than a method, so the number being asserted is a real duration.
	a.sess.Walk.began = time.Now().Add(-3 * time.Second)
	h.chaseTick(a.sess)

	seen = movesAndStops(b.conn.sent())
	if len(seen) != 2 {
		t.Fatalf("%d packet(s) after arrival, want the move then one stop", len(seen))
	}
	if cmdOf(seen[0]) != CmdZCMovePC || cmdOf(seen[1]) != CmdZCStopPC {
		t.Fatalf("order is %d then %d, want MOVE then STOP", cmdOf(seen[0]), cmdOf(seen[1]))
	}
	for name, p := range map[string][]byte{"move": seen[0], "stop": seen[1]} {
		if got := le32At(p, 2); got != handle {
			t.Fatalf("the %s names handle %#x, want the walker %#x", name, got, handle)
		}
	}
	// The stop names the point the actor was walked to.
	for i, want := range [3]int32{1500, 1000, 77} {
		if got := int32(le32At(seen[1], 10+i*4)); got != want {
			t.Fatalf("stop pos[%d] = %d, want %d (the goal the move named)", i, got, want)
		}
	}
	// One stop per stop.
	h.chaseTick(a.sess)
	if got := len(movesAndStops(b.conn.sent())); got != 2 {
		t.Fatalf("a second tick added a packet: %d, want the same 2", got)
	}
}

// TestChaseReportArrivalStopsTheOthers: when the mover's OWN report is what proves the
// character arrived, the other player is stopped at the position the client named - not
// at the goal - and still exactly once.
func TestChaseReportArrivalStopsTheOthers(t *testing.T) {
	h, a, b := chasePair(t)

	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	// 40 units from the goal: the client's own collision stop.
	if _, err := h.movePC(a.sess, moveReport(0, 1500, 1040)); err != nil {
		t.Fatal(err)
	}
	seen := movesAndStops(b.conn.sent())
	// The report's own echo is a move too, so the walk is: chase move, echo, stop.
	if len(seen) != 3 {
		t.Fatalf("%d packet(s), want chase move + echo + stop", len(seen))
	}
	last := seen[len(seen)-1]
	if cmdOf(last) != CmdZCStopPC {
		t.Fatalf("the last packet is cmd %d, want the stop", cmdOf(last))
	}
	if x, y := int32(le32At(last, 10)), int32(le32At(last, 14)); x != 1500 || y != 1040 {
		t.Fatalf("stop = (%d,%d), want the reported position (1500,1040)", x, y)
	}
}

// TestChaseAbandonedWalkSendsNoStop: the player cancelled by clicking the ground. The
// clock runs out, there is no dialog - and NO stop either, because the only position
// this side could name is a destination the character never stood at. A stop there is
// the jump R63 removed.
func TestChaseAbandonedWalkSendsNoStop(t *testing.T) {
	h, a, b := chasePair(t)

	if _, err := h.clickTrigger(a.sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	a.sess.Walk.sentAt = time.Now().Add(-2 * ChaseGrace)
	// The player walked 500 units the other way.
	if _, err := h.movePC(a.sess, moveReport(0, 500, 1000)); err != nil {
		t.Fatal(err)
	}
	b.conn.writes = nil
	a.sess.Walk.began = time.Now().Add(-3 * time.Second) // past the rule, before give-up

	if out := h.chaseTick(a.sess); len(out) != 0 {
		t.Fatalf("%d packet(s) for an abandoned walk, want 0", len(out))
	}
	for _, p := range movesAndStops(b.conn.sent()) {
		if cmdOf(p) == CmdZCStopPC {
			t.Fatalf("an abandoned walk stopped the other player at (%d,%d) - a place "+
				"the character never reached",
				int32(le32At(p, 10)), int32(le32At(p, 14)))
		}
	}
}

// chordZone is a zone with NO path engine, on a map whose id and name the refusal line
// must carry, and two NPCs 500 units apart from where the player stands. Since
// 2026-11-09 every click here is REFUSED (no engine, no walk), once per (map, cell).
func chordZone(t *testing.T, buf *bytes.Buffer) (*Handler, *Session) {
	t.Helper()
	h := &Handler{
		Log:     log.New(buf, "", 0),
		MapID:   1331,
		MapName: "fld_auch",
		NPCs: []NPC{
			{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "REB_SERM", Script: "NPC_TAXMAN"},
			{Handle: 0x40000032, X: 1000, Y: 1500, Cell: 78, ClassType: "REB_SERM", Script: "NPC_TAXMAN"},
		},
		Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
			return ClickReply{DialogKind: ClickDialogOK, Text: "hello", Fade: 1}, nil
		},
	}
	return h, chaseSession()
}

// TestAChordWalkSaysSoOncePerMapAndCell is the card's first pass condition: a walk with
// NO route writes exactly ONE line naming the map and the cell, the walk still goes out
// unchanged, and a second chord from the same cell does not repeat the line - the bound
// is (map, cell), never per tick and never per packet.
//
// WHY IT MATTERS: before this, a chord and a client that never got the packet looked the
// same in the log - the owner's "cant really pinpoint the exact issue" on fld_auch.
// TestAClickWithNoEngineWalksStraight: no path engine still walks - the pre-rework
// behaviour the owner calls "the working state" (2026-11-09). The click sends ONE walk on
// the straight line, capped by MaxChase, and the chord is counted and named by
// noteChordWalk so it is never silent.
func TestAClickWithNoEngineWalksStraight(t *testing.T) {
	var buf bytes.Buffer
	h, sess := chordZone(t, &buf)
	metrics.take()

	// NO ENGINE IS NOT A REFUSAL (restored 2026-11-09 - the owner: the pre-rework state is
	// "the working state"). The click walks on the straight line, capped by MaxChase, and
	// the chord is counted and named by noteChordWalk so it is never silent.
	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || sess.Walk == nil {
		t.Fatalf("%d packet(s) and walk=%v for a click with no path engine, want one walk",
			len(out), sess.Walk != nil)
	}
	if strings.Contains(buf.String(), "REFUSED") {
		t.Fatalf("the click was refused with no engine: %s", buf.String())
	}
}

// TestAClientLegWithNoRouteIsCountedAndNamed is the other half of the same rule: a
// player's own reported leg cannot be refused (the client has already moved), so it is
// drawn on the chord, COUNTED in walksStraight, and named once per (map, cell pair) -
// never silent.
func TestAClientLegWithNoRouteIsCountedAndNamed(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), MapID: 1331, MapName: "fld_auch"}
	metrics.take()
	from := [3]int32{1000, 1000, 5}
	to := [3]int32{1500, 1000, 77}

	if route := h.clientRoute(from, to); route != nil {
		t.Fatalf("clientRoute without a planner answered %v, want nil", route)
	}
	if route := h.clientRoute(from, to); route != nil {
		t.Fatalf("second call answered %v, want nil", route)
	}
	text := buf.String()
	if n := strings.Count(text, "CLIENT LEG IS THE STRAIGHT CHORD"); n != 1 {
		t.Fatalf("%d chord line(s) for two identical legs, want exactly 1:\n%s", n, text)
	}
	for _, want := range []string{"map 1331 (fld_auch)", "from (1000, 1000, cell 5)",
		"to (1500, 1000, cell 77)", "counted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the client-leg line is missing %q:\n%s", want, text)
		}
	}
	if snap := metrics.take(); snap.WalksStraight != 2 {
		t.Fatalf("walksStraight = %d, want 2", snap.WalksStraight)
	}
}

// TestARoutedWalkSaysNothing is the other half of pass condition 2: the common path - a
// walk the path engine routed - must stay silent, or every ordinary click writes a line
// that means nothing.
func TestARoutedWalkSaysNothing(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{
		Log:     log.New(&buf, "", 0),
		MapID:   1331,
		MapName: "fld_auch",
		NPCs: []NPC{{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77,
			ClassType: "REB_SERM", Script: "NPC_TAXMAN"}},
		Plan: func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
			return [3]int32{1370, 1010, 70},
				[][3]int32{{1000, 1000, 5}, {1200, 1000, 5}, {1370, 1010, 70}},
				620, false, true, nil
		},
	}
	sess := chaseSession()
	metrics.take()

	out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want exactly 1", len(out))
	}
	if sess.Walk == nil || len(sess.Walk.route) == 0 {
		t.Fatalf("the routed walk was not installed with its route: %+v", sess.Walk)
	}
	if text := buf.String(); strings.Contains(text, "STRAIGHT CHORD") {
		t.Fatalf("a ROUTED walk wrote the chord line - the common path must stay silent:\n%s", text)
	}
	snap := metrics.take()
	if snap.Walks != 1 || snap.WalksStraight != 0 {
		t.Fatalf("the window counted walks=%d walksStraight=%d, want 1 and 0",
			snap.Walks, snap.WalksStraight)
	}
}

// TestMetricsLineCarriesBothWalkCounters is pass condition 3: one METRICS line answers
// "how many walks were routed and how many were chords" for a whole session.
func TestMetricsLineCarriesBothWalkCounters(t *testing.T) {
	s := Snapshot{PktsIn: 1, Walks: 9, WalksStraight: 4}
	line := s.Line(10 * time.Second)
	if !strings.Contains(line, "walks=9 walksStraight=4") {
		t.Fatalf("the METRICS line does not carry both walk counters:\n got: %s\nwant it "+
			"to hold: walks=9 walksStraight=4", line)
	}
}

// TestAClickRightAfterAReportPlansFromTheReport: the client's own step has just landed
// beside the NPC (a report inside ChaseGrace), so the plan must start where the report put
// the body - not at the beginning of the leg the interpolation is still timing. The
// owner's Leonardo click: the gate said 239 units away, the plan started 2,078 units back,
// and the box opened 4.5 s late (logs\zone_go_10.log, 17:08:34-17:08:44).
func TestAClickRightAfterAReportPlansFromTheReport(t *testing.T) {
	h, _ := chaseHandler(t)
	npc := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	h.NPCs = []NPC{npc}

	sess := chaseSession()
	// The step ended 240 units east of the NPC: outside the 200 gate, so the click walks.
	sess.Pos = [3]int32{npc.X + 240, npc.Y, npc.Cell}
	sess.LastMoveAt = time.Now()

	if _, err := h.clickTrigger(sess, frameClick(0, npc.Handle)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk started")
	}
	if sess.Walk.from != sess.Pos {
		t.Fatalf("planned from %v, want the reported position %v", sess.Walk.from, sess.Pos)
	}
}

// TestARedirectPlansFromWhereTheBodyHasWalkedTo: a redirect must time the new walk from
// the body's DRAWN position, not from the session's last client report - during a server
// walk the client reports nothing, so the report predates the walk and the clock starts too
// early. That is the owner's "the arrival time is still being calculated on my initial
// position rather than current click position".
func TestARedirectPlansFromWhereTheBodyHasWalkedTo(t *testing.T) {
	h, _ := chaseHandler(t)
	first := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	second := NPC{Handle: 0x40000032, X: 3000, Y: 1000, Cell: 90, ClassType: "B", Script: "TWO"}
	h.NPCs = []NPC{first, second}

	sess := chaseSession()
	sess.LastMoveAt = time.Time{} // no recent report: the walk model is all there is
	if _, err := h.clickTrigger(sess, frameClick(0, first.Handle)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil {
		t.Fatal("no walk to the first NPC")
	}
	// Half the walk to the first NPC has been walked.
	sess.Walk.began = sess.Walk.began.Add(-sess.Walk.eta / 2)
	mid := sess.Walk.bodyAt(time.Now())
	if mid == sess.Pos {
		t.Fatal("the fixture did not advance the body")
	}

	if _, err := h.clickTrigger(sess, frameClick(0, second.Handle)); err != nil { // arm
		t.Fatal(err)
	}
	if _, err := h.clickTrigger(sess, frameClick(0, second.Handle)); err != nil { // redirect
		t.Fatal(err)
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != second.Handle {
		t.Fatal("the second click did not redirect")
	}
	if d := hypot2(sess.Walk.from, mid); d > 20 {
		t.Fatalf("the redirect planned from %v, want the body's drawn point %v (%.0f away)",
			sess.Walk.from, mid, d)
	}
}

// TestAClientStepReAnchorsTheArrivalClock: the client walks the player to a clicked NPC
// by itself and reports every step, so each report is a real position - the walk must be
// re-anchored on it, or the clock keeps running from where the move was sent and the box
// comes out late (owner: "the arrival time is still being calculated on my initial
// position rather than current click position").
func TestAClientStepReAnchorsTheArrivalClock(t *testing.T) {
	h, _ := chaseHandler(t)
	npc := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	h.NPCs = []NPC{npc}

	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, npc.Handle)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("no walk started")
	}
	before := w.eta
	// A step report half way between the click and the goal.
	mid := [3]int32{(sess.Pos[0] + w.goal[0]) / 2, (sess.Pos[1] + w.goal[1]) / 2, w.goal[2]}
	h.observeWalk(sess, w.caster, mid)
	if w.from != mid {
		t.Fatalf("the clock is anchored at %v, want the reported step %v", w.from, mid)
	}
	if w.eta >= before {
		t.Fatalf("eta %s did not shrink from %s after the step", w.eta, before)
	}
}

// TestAnInRangeClickOnAnotherNpcIsArmedNotOpened: the client re-sends a click on the NPC
// that was under the cursor, so a click on an NPC the player is ALREADY beside can land
// mid-walk to another one. It must obey the lock like any other click on a different NPC -
// ARM, drop the box, move nothing - or its window pops open in the middle of the walk
// (owner: "the box still opens mid walk"; logs\zone_go_10.log 17:51:17.704 click GRANMA
// 888 away -> 17:51:18.387 SCR_CLICK_LEO_SERVICE sent Leonardo's box).
func TestAnInRangeClickOnAnotherNpcIsArmedNotOpened(t *testing.T) {
	h, calls := chaseHandler(t)
	far := NPC{Handle: 0x40000031, X: 1500, Y: 1000, Cell: 77, ClassType: "A", Script: "ONE"}
	closeBy := NPC{Handle: 0x40000032, X: 1100, Y: 1000, Cell: 90, ClassType: "B", Script: "TWO"}
	h.NPCs = []NPC{far, closeBy}

	sess := chaseSession() // Pos (1000,1000,5): far is 500 away, closeBy is 100
	if _, err := h.clickTrigger(sess, frameClick(0, far.Handle)); err != nil {
		t.Fatal(err)
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != far.Handle {
		t.Fatal("the walk to the far NPC did not start")
	}

	out, err := h.clickTrigger(sess, frameClick(0, closeBy.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) for an in-range click on another NPC, want 0", len(out))
	}
	if *calls != 0 {
		t.Fatalf("script calls = %d, want 0 - the box must not open mid-walk", *calls)
	}
	if sess.ClickLockArmed != closeBy.Handle {
		t.Fatalf("armed handle = 0x%08x, want the in-range NPC 0x%08x",
			sess.ClickLockArmed, closeBy.Handle)
	}
	if sess.Walk == nil || sess.Walk.npc.Handle != far.Handle {
		t.Fatal("the walk to the far NPC was replaced by the arming click")
	}
}
