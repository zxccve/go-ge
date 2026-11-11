package zone

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// These five tests are the owner's report as arithmetic: he clicked his SECOND
// character as leader and the FIRST one moved (2026-11-01), because the server
// called slot 0 "the player's character" everywhere and the client's own
// CZ_SELECTCHANGE_PC (3652) said otherwise. See zone\control.go.

// controlZone is driftSession with a log the test can read: the switch is proved by
// ONE line, so the handler needs somewhere to put it. The fixture is reused, not
// copied - a second builder for the same three-member team would be a fork.
func controlZone(t *testing.T, stand [][3]int32) (*Handler, *Session, *bytes.Buffer) {
	t.Helper()
	h, sess := driftSession(t, []uint32{0x40000011, 0x40000012, 0x40000013}, stand)
	var buf bytes.Buffer
	h.Log = log.New(&buf, "", 0)
	return h, sess, &buf
}

// selectFrame builds CZ_SELECTCHANGE_PC the way the client does: 10 bytes of
// security header, then the ONE payload byte naming the team slot. 11 bytes, the
// width docker-port\data\cmd_table.txt:1128 gives for 3652.
func selectFrame(slot byte) protocol.Frame {
	return protocol.Frame{
		Cmd: CmdCZSelectChangePC, Size: 11, Data: pack(CmdCZSelectChangePC, slot),
	}
}

// switchControl sends the real frame through the real dispatch and returns the log,
// so a test that passes cannot pass with no case in the switch.
func switchControl(t *testing.T, h *Handler, sess *Session, buf *bytes.Buffer, slot byte) string {
	t.Helper()
	buf.Reset()
	out, err := h.Reply(context.Background(), sess, selectFrame(slot))
	if err != nil {
		t.Fatalf("Reply(SELECTCHANGE_PC %d): %v", slot, err)
	}
	if len(out) != 0 {
		// The client has already switched its camera and input before it sends this;
		// it reads no reply, so anything sent here would be a packet it cannot parse.
		t.Fatalf("SELECTCHANGE_PC %d produced %d packet(s), want none - the client "+
			"expects no answer", slot, len(out))
	}
	return buf.String()
}

// TestControlSlotFollowsThePlayersSwitch is the owner's bug, in one run: after the
// client says "slot 1", a move report for slot 1 is the one that opens the session's
// walk segment, and a report for slot 0 no longer does.
//
// It fails with the old code in the way the game fails: slot 0's report opens the
// segment, slot 1's is ignored, so the server tracks the character the player is NOT
// driving.
func TestControlSlotFollowsThePlayersSwitch(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, buf := controlZone(t, stand)
	if sess.Control != 0 {
		t.Fatalf("a fresh session drives slot %d, want 0 (the default before any 3652)",
			sess.Control)
	}

	// The bytes the client really sent are the shape this frame is built to:
	// 44 0e 18 00 00 00 4b 00 00 00 01 - 0x0e44 = 3652, then ten bytes of header and
	// the payload byte 01 (docker-port\logs\go_zone1331.log, 10:28:02).
	logged := switchControl(t, h, sess, buf, 1)
	if sess.Control != 1 {
		t.Fatalf("after a 3652 naming slot 1 the session drives slot %d, want 1",
			sess.Control)
	}
	const want = "zone: CONTROL slot 1 - the player now drives this character (cmd 3652)"
	if !strings.Contains(logged, want) {
		t.Fatalf("the switch did not print its one line.\nwant: %s\ngot: %q", want, logged)
	}

	// A report from the character the player is NOT driving is a FOLLOWER's step. It
	// must keep its own record and leave the session's segment alone.
	if !sess.moveAt.IsZero() {
		t.Fatal("the fixture already has an open segment - the assertions below would " +
			"prove nothing")
	}
	// THE POSITION THE SESSION KEEPS IS THE DRIVEN CHARACTER'S OWN (R109). It used to
	// be slot 0's point whatever the switch said, which is how the warlock ended up
	// opening its first leg on the other character's feet and teleporting on its first
	// swing (2026-11-01 23:31:53). So after a 3652 naming slot 1, Pos is slot 1's stand
	// point - and slot 0's report below must leave it exactly there.
	if sess.Pos != stand[1] {
		t.Fatalf("after the switch Pos is %v, want the driven character's own point %v "+
			"- Pos is the point the driven character's next leg starts from",
			sess.Pos, stand[1])
	}
	driftMove(t, h, sess, 0, [3]int32{1200, 1000, 5})
	if !sess.moveAt.IsZero() {
		t.Fatalf("a MOVE_PC for slot 0 opened the session's walk segment (moveAt = %v) "+
			"while the player drives slot 1 - this is the owner's bug: the character he "+
			"is not driving is the one being tracked", sess.moveAt)
	}
	if sess.Pos != stand[1] {
		t.Fatalf("a MOVE_PC for slot 0 moved the session position to %v, want it left "+
			"at %v - Pos is the driven character's point", sess.Pos, stand[1])
	}
	if sess.walks[0].dest != [3]int32{1200, 1000, 5} {
		t.Fatalf("slot 0's own walk record says dest %v, want its report (1200, 1000, "+
			"5): a follower still walks, it is just not the player's intent",
			sess.walks[0].dest)
	}

	// A report from the character the player IS driving opens the segment, and that
	// one segment is what the gate, the chase and the swing then read.
	driftMove(t, h, sess, 1, [3]int32{1900, 1000, 5})
	if sess.moveAt.IsZero() {
		t.Fatal("a MOVE_PC for slot 1 did NOT open the session's walk segment after " +
			"the client said the player drives slot 1 - the walk bookkeeping is still " +
			"stuck on slot 0")
	}
	if sess.Pos != [3]int32{1900, 1000, 5} {
		t.Fatalf("the session position is %v, want the destination slot 1 reported "+
			"(1900, 1000, 5)", sess.Pos)
	}
	if !sess.LastMoveAt.Equal(sess.moveAt) {
		t.Fatalf("LastMoveAt = %v but the segment opened at %v: the report that opens "+
			"the segment is also the player's own move clock", sess.LastMoveAt, sess.moveAt)
	}
}

// TestControlSlotDefaultsToZero is the "nothing changed until the client speaks"
// half: before any 3652, slot 0 gets exactly the treatment it got before this lane
// existed, and slot 1 does not.
//
// It fails if the default is anything but slot 0 - which would break every session
// whose client never sends 3652.
func TestControlSlotDefaultsToZero(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, _ := controlZone(t, stand)

	driftMove(t, h, sess, 0, [3]int32{1200, 1000, 5})
	if sess.moveAt.IsZero() {
		t.Fatal("with no 3652 the session must still treat slot 0 as the player's " +
			"character, and its report did not open the walk segment")
	}
	if sess.Pos != [3]int32{1200, 1000, 5} {
		t.Fatalf("slot 0's report left the session position at %v, want its "+
			"destination (1200, 1000, 5)", sess.Pos)
	}
	opened := sess.moveAt

	// The follower is a follower until the client says otherwise, so its report must
	// not touch the segment.
	driftMove(t, h, sess, 1, [3]int32{1900, 1000, 5})
	if !sess.moveAt.Equal(opened) || sess.Pos != [3]int32{1200, 1000, 5} {
		t.Fatalf("a report for slot 1 changed the session segment (moveAt %v -> %v, "+
			"Pos %v) with no 3652 ever sent - followers must stay followers",
			opened, sess.moveAt, sess.Pos)
	}
}

// TestControlSlotRefusesAnIndexOutsideTheTeam is the safety half: a 3652 naming a
// slot this session never spawned is refused, said so ONCE, and the previous answer
// stands. So is a frame that is not the 11 bytes this build sends.
//
// It fails if the frame is obeyed without a bounds check (Control = 7, and every
// "the player's character" decision then points at a slot with no character), if the
// refusal is silent, or if a wrong-length frame is read as if it were ours.
func TestControlSlotRefusesAnIndexOutsideTheTeam(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, buf := controlZone(t, stand)

	switchControl(t, h, sess, buf, 1) // the answer that must survive the refusals

	logged := switchControl(t, h, sess, buf, 7) // 7 is outside a three-character team
	if sess.Control != 1 {
		t.Fatalf("a 3652 naming slot 7 changed the driven slot to %d, want it left at "+
			"1: an index outside the team has no character, no handle and no position",
			sess.Control)
	}
	if !strings.Contains(logged, "REFUSED") || !strings.Contains(logged, "slot 7") {
		t.Fatalf("the out-of-range switch was not logged as refused.\ngot: %q", logged)
	}
	if n := strings.Count(logged, "\n"); n > 1 {
		t.Fatalf("the refusal printed %d lines, want ONE\ngot: %q", n, logged)
	}

	// A different build's length is not guessed at: it is refused with the bytes.
	long := make([]byte, 0, 13)
	long = append(long, selectFrame(1).Data...)
	long = append(long, 0xAA, 0xBB)
	buf.Reset()
	if _, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZSelectChangePC, Size: len(long), Data: long,
	}); err != nil {
		t.Fatalf("Reply(13-byte SELECTCHANGE_PC): %v", err)
	}
	logged = buf.String()
	if sess.Control != 1 {
		t.Fatalf("a 13-byte 3652 changed the driven slot to %d, want it left at 1",
			sess.Control)
	}
	if !strings.Contains(logged, "REFUSED") || !strings.Contains(logged, "bytes:") {
		t.Fatalf("a wrong-length 3652 was refused without naming the bytes it "+
			"received\ngot: %q", logged)
	}

	// The old answer still WORKS, not merely survives: slot 1's report opens the
	// segment and slot 0's does not.
	driftMove(t, h, sess, 0, [3]int32{1200, 1000, 5})
	if !sess.moveAt.IsZero() {
		t.Fatal("after the refused frames a report for slot 0 opened the segment - the " +
			"driven slot was not really left at 1")
	}
	driftMove(t, h, sess, 1, [3]int32{1900, 1000, 5})
	if sess.moveAt.IsZero() {
		t.Fatal("after the refused frames a report for slot 1 did not open the segment")
	}
}

// TestControlSlotResetsOnEntryAndDisconnect: the driven slot is per visit. A fresh
// entry starts at slot 0 (the barrack lets the player pick a different leader, and
// the client names it again after the entry), and a session that leaves the zone
// puts the session back to the default.
//
// It fails if Control is carried over: the next entry would drive a slot the client
// never named, and the character the player sees himself on would not be the one the
// server tracks.
func TestControlSlotResetsOnEntryAndDisconnect(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, buf := controlZone(t, stand)

	// ENTRY. The real startGame, with an empty START_GAME body: account 0 keeps the
	// team this session already has and touches no database or handoff file, so the
	// only thing under test is the reset.
	switchControl(t, h, sess, buf, 2)
	if sess.Control != 2 {
		t.Fatalf("the fixture drives slot %d, want 2 before the entry", sess.Control)
	}
	if _, err := h.startGame(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZStartGame}); err != nil {
		t.Fatalf("startGame: %v", err)
	}
	if sess.Control != 0 {
		t.Fatalf("after a fresh entry the session drives slot %d, want 0 - a new entry "+
			"must not inherit the previous character's slot", sess.Control)
	}

	// DISCONNECT.
	switchControl(t, h, sess, buf, 1)
	if sess.Control != 1 {
		t.Fatalf("the fixture drives slot %d, want 1 before the leave", sess.Control)
	}
	h.leaveZone(sess)
	if sess.Control != 0 {
		t.Fatalf("after leaving the zone the session still drives slot %d, want 0",
			sess.Control)
	}
}

// TestUncontrolledSwingStillUsesItsOwnWalkRecord is the R91-round-7 guard: making
// the DRIVEN character's position come from the session's segment must not send a
// FOLLOWER's swing to that segment. A follower has its own walk record, opened by its
// own reports, and that is where its swing position comes from.
//
// It fails if the site-by-site change was done with a find-and-replace that left one
// branch reading the session segment for every slot - a swing would then be drawn
// where the player's character is, not where the follower is, which is the snap that
// round 7 removed.
func TestUncontrolledSwingStillUsesItsOwnWalkRecord(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, buf := controlZone(t, stand)
	switchControl(t, h, sess, buf, 1) // the player drives slot 1

	now := time.Now()
	// The DRIVEN character walks east along the session's segment.
	sess.moveFrom = [3]int32{1000, 1000, 5}
	sess.Pos = [3]int32{1800, 1000, 5}
	sess.moveAt = now.Add(-time.Second)
	// The FOLLOWER (slot 0) walks NORTH on a record of its own, deliberately
	// perpendicular so the two answers cannot be confused.
	sess.walks = make([]charWalk, len(sess.Team))
	sess.walks[0] = charWalk{
		from: [3]int32{1000, 1300, 5}, dest: [3]int32{1000, 1900, 5},
		at: now.Add(-500 * time.Millisecond), speed: MoveBaseSpeed,
	}

	from, dest, elapsed := sess.swingSegment(0, now)
	if from != sess.walks[0].from || dest != sess.walks[0].dest {
		t.Fatalf("the follower's swing segment is %v -> %v, want its OWN walk record "+
			"%v -> %v: a swing for a character the player does not drive must not come "+
			"from the session's segment (R91 round 7)",
			from, dest, sess.walks[0].from, sess.walks[0].dest)
	}
	if elapsed < 400*time.Millisecond || elapsed > 600*time.Millisecond {
		t.Fatalf("the follower's swing segment has been walking for %v, want ~500ms "+
			"from its own record", elapsed)
	}

	// The position that swing is drawn at is on the follower's own segment too: half
	// of a 600-unit north walk at the animated speed, so y is between the two ends
	// and x never moves.
	p := sess.drawnPos(0, now, MoveBaseSpeed)
	if p[0] != 1000 || p[1] <= 1300 || p[1] >= 1900 {
		t.Fatalf("the follower's drawn position is %v, want a point on its own north "+
			"walk (x = 1000, 1300 < y < 1900) - the session's segment runs EAST and "+
			"would put it at y = 1000", p)
	}

	// And the driven character still uses the session's segment, which is the half
	// this lane changed. Without this the test would pass on code that never switched.
	dFrom, dDest, _ := sess.swingSegment(1, now)
	if dFrom != sess.moveFrom || dDest != sess.Pos {
		t.Fatalf("the driven character's swing segment is %v -> %v, want the session's "+
			"segment %v -> %v after a 3652 named slot 1",
			dFrom, dDest, sess.moveFrom, sess.Pos)
	}
}

// TestASwitchDoesNotLeaveTheOldLegOnTheNewCharacter is the owner's second report of the
// same evening: "changing character broke something? warlock teleport?" (2026-11-01).
//
// The session keeps ONE walk segment for the character the player drives (moveFrom /
// moveRoute / Pos / moveAt, clientLegOf in move.go). A switch used to leave that record
// pointing at the leg the PREVIOUS character was walking, so the new character read it as
// its own - its swing was built on a phantom leg hundreds of units long and the client
// hard-set the actor there. Measured, right after switching to the warlock:
//
//	SWING POS char 2 (1793) pos (2119, -797) from segment (2560, -1207) -> (2119, -797)
//
// where (2560,-1207) was char 1's point. So the switch must CLOSE the leg and re-seat
// Pos on the new character's own last point.
func TestASwitchDoesNotLeaveTheOldLegOnTheNewCharacter(t *testing.T) {
	stand := [][3]int32{{100, 100, 108}, {900, 900, 108}, {500, 500, 108}}
	h, sess, buf := controlZone(t, stand)

	// The driven character (slot 0) walks a long leg - this is the leg that used to leak.
	if _, err := h.Reply(context.Background(), sess,
		moveFrame(0, [3]int32{1400, 1400, 108})); err != nil {
		t.Fatalf("Reply(MOVE_PC char 0): %v", err)
	}
	// THE READER THE SWING USES, not a second one: a test that asks a different question
	// from the packet's builder can pass while the packet is wrong.
	leg := sess.clientLegOf(0, MoveBaseSpeed)
	if leg.placed {
		t.Fatalf("no leg open for the driven character after its own move report")
	}
	from, dest := leg.from, leg.dest

	// The player takes over slot 1, which stands 800 units away from that leg.
	switchControl(t, h, sess, buf, 1)

	if after := sess.clientLegOf(1, MoveBaseSpeed); !after.placed {
		t.Fatalf("the new character inherited the old leg %v -> %v (its own point is %v); "+
			"a swing built on it is the teleport", after.from, after.dest,
			sess.Team[1].Position)
	}
	if sess.Pos != sess.Team[1].Position {
		t.Fatalf("session Pos is %v after the switch, the new character stands at %v: "+
			"the next leg would start on the other character's feet",
			sess.Pos, sess.Team[1].Position)
	}
	if sess.moveRoute != nil {
		t.Fatalf("the new character inherited the old leg's route %v", sess.moveRoute)
	}
	if from == sess.Team[1].Position && dest == sess.Team[1].Position {
		t.Logf("note: the two characters stood in the same place, which weakens this case")
	}
}

// TestALeaderSwitchLeavesAnotherCharactersApproachAlone is R131, the owner's rule: "you want
// each of the character to be independent unless u use the group mode ... changing leader
// should not affect anything." A keep-mode approach belongs to the character it was pressed
// on, so a switch must change only WHICH body the player's input drives.
//
// THE MEASUREMENT THIS REPLACES (2026-11-04 17:12:15): a 419-unit approach was sent for char
// 0, the player switched to slot 2 48 ms later, and the walk was dropped silently - no
// packet, no log, the stored point left at the leg's START. The next press re-planned the
// same leg with the distance still 1375.6490097580793 to thirteen decimals: two seconds of a
// character standing still, and a press 0.9 s from landing that took 2.65 s.
func TestALeaderSwitchLeavesAnotherCharactersApproachAlone(t *testing.T) {
	stand := [][3]int32{{1000, 1000, 5}, {1600, 1000, 5}, {2200, 1000, 5}}
	h, sess, buf := controlZone(t, stand)

	// THE WALK THIS SERVER SENT for slot 0 - an attack approach, built the way the zone
	// builds one (caster, goal, clock, the press it answers).
	goal := [3]int32{1200, 1000, 5}
	sess.Walk = &walk{
		from:  [3]int32{1000, 1000, 5},
		goal:  goal,
		began: time.Now(),
		speed: MoveBaseSpeed,
		caster: 0,
		atk:   &pendingAttack{target: 1234},
	}

	switchControl(t, h, sess, buf, 1)

	if sess.Walk == nil {
		t.Fatalf("the switch DROPPED char 0's approach - a keep-mode walk ended because the "+
			"player looked at another character:\n%s", buf.String())
	}
	if sess.Walk.caster != 0 {
		t.Fatalf("the walk now belongs to char %d, want char 0 - a switch must not re-home "+
			"another character's approach:\n%s", sess.Walk.caster, buf.String())
	}
	if sess.Walk.goal != goal {
		t.Fatalf("the walk's goal became %v, want the one it was sent with %v",
			sess.Walk.goal, goal)
	}
	if sess.Walk.atk == nil || sess.Walk.atk.target != 1234 {
		t.Fatalf("the press the walk is carrying was lost on the switch")
	}

	// AND THE OTHER HALF OF THE SWITCH STILL HAPPENS: the session's client leg is re-seated
	// on the newly driven character, which is what stops its swing from being drawn on the
	// previous character's leg (R109).
	if sess.Control != 1 {
		t.Fatalf("the player drives slot %d after a switch to slot 1", sess.Control)
	}
	if !sess.moveAt.IsZero() {
		t.Fatalf("the switch left the previous character's client leg open (moveAt %v)",
			sess.moveAt)
	}
}

// TestTwoCharactersKeepTheirOwnApproaches is R131's concurrency half: each character owns its
// approach, so a press by character 2 must not take character 0's walk away.
//
// THE DEFECT IT REPLACES. One `sess.Walk` for the team meant the second press REPLACED the
// first character's walk - the log said `a new press replaces the walk to ...` - so with
// three characters in keep mode only ONE of them could ever be walking to its target. The
// owner's rule: "you want each of the character to be independent unless u use the group
// mode". Group mode has no packet in the reference at all (the client drags the followers),
// so independence is the server's whole job.
func TestTwoCharactersKeepTheirOwnApproaches(t *testing.T) {
	s := &Session{Team: []Character{{}, {}, {}}}

	a0 := &walk{from: [3]int32{0, 0, 1}, goal: [3]int32{0, 100, 1}, caster: 0,
		atk: &pendingAttack{target: 11}}
	a2 := &walk{from: [3]int32{0, 0, 1}, goal: [3]int32{0, 200, 1}, caster: 2,
		atk: &pendingAttack{target: 22}}

	// The first approach lands in Walk (the single-walk case keeps its old shape), the
	// second must go somewhere of its own.
	s.addApproach(a0)
	s.addApproach(a2)

	if got := s.approachOf(0); got != a0 {
		t.Fatalf("character 0's approach is %v, want the one it installed - character 2's "+
			"press took it away", got)
	}
	if got := s.approachOf(2); got != a2 {
		t.Fatalf("character 2's approach is %v, want the one it installed", got)
	}
	if got := s.teamApproaches(); len(got) != 1 || got[0] != a2 {
		t.Fatalf("teamApproaches = %v, want just character 2's walk (character 0's is in "+
			"Walk and is stepped separately)", got)
	}

	// A SECOND press by character 2 replaces ONLY character 2's walk.
	a2b := &walk{from: [3]int32{0, 0, 1}, goal: [3]int32{0, 300, 1}, caster: 2,
		atk: &pendingAttack{target: 22}}
	if replaced := s.addApproach(a2b); replaced != a2 {
		t.Fatalf("the second press replaced %v, want character 2's own walk %v", replaced, a2)
	}
	if got := s.approachOf(0); got != a0 {
		t.Fatalf("character 2 pressing again took character 0's approach away")
	}

	// Clearing one leaves the other standing.
	s.clearApproach(a0)
	if s.approachOf(0) != nil {
		t.Fatalf("clearing character 0's walk left it installed")
	}
	if got := s.approachOf(2); got != a2b {
		t.Fatalf("clearing character 0's walk removed character 2's (%v)", got)
	}
	s.clearApproach(a2b)
	if s.approachOf(2) != nil || len(s.teamApproaches()) != 0 || s.Walk != nil {
		t.Fatalf("after clearing both: approachOf(2) %v, teamApproaches %v, Walk %v",
			s.approachOf(2), s.teamApproaches(), s.Walk)
	}
}

// TestAClickDoesNotEndAnotherCharactersApproach: a click is the player's own order and owns
// Walk (the click chase). It must take that record WITHOUT ending an approach that happens to
// be parked there - parkApproach moves the approach into teamWalks first.
func TestAClickDoesNotEndAnotherCharactersApproach(t *testing.T) {
	s := &Session{Team: []Character{{}, {}, {}}}
	approach := &walk{from: [3]int32{0, 0, 1}, goal: [3]int32{0, 100, 1}, caster: 0,
		atk: &pendingAttack{target: 11}}
	s.addApproach(approach)
	if s.Walk != approach {
		t.Fatalf("fixture: the first approach should sit in Walk")
	}
	s.parkApproach()
	if s.Walk != nil {
		t.Fatalf("parkApproach left Walk holding %v", s.Walk)
	}
	if got := s.approachOf(0); got != approach {
		t.Fatalf("parkApproach lost the approach: approachOf(0) = %v", got)
	}
}

// TestASessionWithOnlyATeamWalkStillTicks is R131's own consequence: an approach can live in
// teamWalks with Walk holding something else - or nothing - and a session that stopped
// ticking would never advance it, fire it, or give it up.
func TestASessionWithOnlyATeamWalkStillTicks(t *testing.T) {
	s := &Session{Team: []Character{{}, {}}}
	if s.wantsTick() {
		t.Fatalf("an idle session wants a tick")
	}
	// THE STATE R131 CREATES: character 1's approach in the per-character record while Walk
	// holds nothing (a click chase and an approach are never the same character's).
	s.teamWalks = []*walk{{from: [3]int32{0, 0, 1}, goal: [3]int32{0, 100, 1}, caster: 1,
		atk: &pendingAttack{target: 7}}}
	if s.Walk != nil {
		t.Fatalf("fixture: Walk is not empty")
	}
	if !s.wantsTick() {
		t.Fatalf("a session with character 1's approach in flight does not want a tick - "+
			"the walk would never advance")
	}
}
