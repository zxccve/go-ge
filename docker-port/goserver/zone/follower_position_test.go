package zone

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"testing"
	"time"

	"geserver/protocol"
)

// A FOLLOWER'S LIVE POSITION MUST SURVIVE EVERY SEGMENT DROP (R91 round 7).
//
// THE MEASUREMENT these tests are written against is the owner's own session
// (logs\go_zone1331.log, 2026-11-01 14:43, ms-stamped): of 7 `SWING POS` lines, 6
// carried a point more than 40 units from the last destination the client had sent
// for that character, the worst 479 units, and every one of them was a FOLLOWER
// (char 1). Six of the seven also printed a ZERO-LENGTH segment - `from (X, Y) ->
// (X, Y)` - which is the signature of the bug: both ends of that line came from ONE
// stored record, so the model was reading a point no walk had written recently.
//
// WHY THE ZERO-LENGTH LINE IS THE BUG AND NOT THE CURE: `openWalk`'s `from == dest`
// branch drops the segment (a report naming the place the character already stands is
// not a walk) and, before this round, wrote the position nowhere. drawnPos/charPos
// then fell back to Team[idx].Position, which a walk only writes when it ENDS
// (endWalks) - so a follower mid-journey was read at the point an EARLIER walk last
// wrote. The client obeys the 3564's position and hard-sets its own actor to it
// (source2\geClientShared\source\GeActor.cpp:309-311), so a 479-unit gap in that
// number is a 479-unit teleport on screen: "not laggy per say, but
// twitchy/jittery/snapping" (owner, in game).
//
// THE POINTS. `followerStand` is the stand point these tests use for a swing: 100
// units from `scarecrow()` along y, inside stance 10560's own MaxR 125. `staleStore`
// is 479 units behind it - the distance the owner's worst swing carried.

var (
	followerStand = attackReachStand            // (-919, 2211, cell 136)
	staleStore    = [3]int32{-919, 1732, 136}   // 479 units short of followerStand
	leaderStore   = [3]int32{-919, 2000, 136}   // the leader's own placed point
	walkSpeedTest = 469.0                       // packetSpeed with no gear, the log's value
)

// followerSession is one leader (slot 0) and one follower (slot 1) whose STORED point
// is `staleStore` - the state after a walk that never ended on the server's side.
func followerSession() *Session {
	s := &Session{
		Pos: leaderStore,
		Team: []Character{
			{Handle: 0x40000031, Name: "Leader", Position: leaderStore},
			{Handle: 0x40000032, Name: "Follower", Position: staleStore},
		},
		skipped: map[uint16]int{},
	}
	return s
}

// TestFollowerReportNamingWhereItStandsIsRecorded is (1): a follower's CZ_MOVE_PC that
// names the place it already stands - from == dest, the zero-length segment of the log -
// must leave the model at THAT point, not at an older stored one.
//
// The setup is the measured shape exactly: a segment that has been walked OUT (opened
// two seconds ago, 479 units at 469 u/s is 1.02 s, so the body is at its end), and then
// the client's next report naming that same end. Before the fix drawnPos answered
// `staleStore`, 479 units behind the body.
func TestFollowerReportNamingWhereItStandsIsRecorded(t *testing.T) {
	s := followerSession()
	now := time.Now()

	// The walk that took the follower from its stale point to where it stands now.
	s.openWalk(1, staleStore, followerStand, now.Add(-2*time.Second), walkSpeedTest)
	if got := s.drawnPos(1, now, walkSpeedTest); got != followerStand {
		t.Fatalf("the walk has run out, so the body should be at %v: drawnPos = %v",
			followerStand, got)
	}

	// FROM == DEST: the client's last report of a walk names the place it has reached.
	s.openWalk(1, followerStand, followerStand, now, walkSpeedTest)

	got := s.drawnPos(1, now, walkSpeedTest)
	if got != followerStand {
		t.Errorf("after a report naming the place the follower already stands, drawnPos "+
			"= %v, want %v - the report's point is %.0f units from the stored one, and that "+
			"gap is what the swing packet hands to the client (GeActor.cpp:309-311)",
			got, followerStand, hypot2(got, followerStand))
	}
	if got := s.charPos(1, now); got != followerStand {
		t.Errorf("charPos = %v, want %v - the same fallback, read by the move guard and "+
			"the click range", got, followerStand)
	}
	if got := s.Team[1].Position; got != followerStand {
		t.Errorf("the stored position is %v, want %v - with the segment gone it is the "+
			"ONLY record of where this character is", got, followerStand)
	}
}

// TestSwingAfterThatReportCarriesTheReportedPoint is (2): the same follower then swings,
// and the position IN THE PACKET is the point the client reported.
//
// It goes through h.Reply with a real CZ_ATTACK for char index 1, not through drawnPos,
// for the reason attack_position_test.go gives: the bug is what the player SEES, and the
// 3564's +8 is what the client obeys. Before the fix the press did not even swing - the
// model had the body 579 units from the Scarecrow, so the reach check walked the
// character instead - which is the same stale point, one step earlier.
func TestSwingAfterThatReportCarriesTheReportedPoint(t *testing.T) {
	h, logs, p := attackZone(t)

	// A follower that is the leader's twin in job, stance and gear: the swing path reads
	// all three, and the point of this test is the POSITION, not the character.
	f := p.sess.Team[0]
	f.Handle = 0x40000032
	f.Name = "Follower"
	f.Position = staleStore
	p.sess.Team = append(p.sess.Team, f)

	now := time.Now()
	p.sess.openWalk(1, staleStore, followerStand, now.Add(-2*time.Second), walkSpeedTest)
	p.sess.openWalk(1, followerStand, followerStand, now, walkSpeedTest)

	out := pressAttackFor(t, h, p.sess, 1)

	var hit []byte
	for _, pkt := range out {
		if len(pkt) >= asAtkPos+12 && binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			hit = pkt
			break
		}
	}
	if hit == nil {
		t.Fatalf("char 1 pressed a swing from %v (the point the client reported) and no "+
			"%d came back - the model still has the body at %v, %.0f units away, so the "+
			"press was answered with a walk instead of a hit. Packages: %d. Log:\n%s",
			followerStand, CmdZCAttackSingle, staleStore,
			hypot2(staleStore, followerStand), len(out), logs.String())
	}
	got := [3]int32{
		int32(binary.LittleEndian.Uint32(hit[asAtkPos:])),
		int32(binary.LittleEndian.Uint32(hit[asAtkPos+4:])),
		int32(binary.LittleEndian.Uint32(hit[asAtkPos+8:])),
	}
	if got != followerStand {
		t.Errorf("the 3564 carries %v, want the point the client reported %v (%.0f units "+
			"of snap on the owner's screen)", got, followerStand, hypot2(got, followerStand))
	}
}

// pressAttackFor is pressAttack with the character slot spelled out: the bug is a
// FOLLOWER's position, so the frame must name char 1.
func pressAttackFor(t *testing.T, h *Handler, sess *Session, idx uint8) [][]byte {
	t.Helper()
	f := attackFrame(idx, capturedTarget)
	out, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestLeaderThroughTheSameBranchIsUnchanged is (3): the leader's path through the
// from == dest branch answers byte-for-byte what it answered before the fix.
//
// The leader keeps its live point in s.moveFrom / s.moveAt / s.Pos, written by movePC,
// and drawnPos reads those - never Team[0].Position. So the fix must not write Team[0]
// at all, and must still drop the segment exactly as before. This test is what fails if
// the recording is ever made unconditional (the `idx > 0` guard removed).
func TestLeaderThroughTheSameBranchIsUnchanged(t *testing.T) {
	at := time.Now().Add(-2 * time.Second)
	s := &Session{
		Pos:      [3]int32{-919, 2211, 136},
		moveFrom: [3]int32{-919, 2000, 136},
		moveAt:   at,
		Team: []Character{
			{Handle: 0x40000031, Position: [3]int32{-919, 2000, 136}},
			{Handle: 0x40000032, Position: staleStore},
		},
		skipped: map[uint16]int{},
	}
	now := time.Now()
	before := s.drawnPos(0, now, walkSpeedTest)
	beforeStored := s.Team[0].Position

	// The leader reports the place it has walked out to: from == dest, as movePC hands it
	// (segmentStart for slot 0 is the walked point, and pos is the report's destination).
	s.openWalk(0, s.Pos, s.Pos, now, walkSpeedTest)

	if got := s.drawnPos(0, now, walkSpeedTest); got != before {
		t.Errorf("the leader reads %v after the report, and read %v before it - the "+
			"leader's answer comes from s.moveFrom/s.Pos/s.moveAt and cannot depend on "+
			"this branch", got, before)
	}
	if s.Pos != ([3]int32{-919, 2211, 136}) || s.moveAt != at {
		t.Errorf("the leader's own segment moved: Pos %v, moveAt %v - movePC wrote those "+
			"and this branch must not touch them", s.Pos, s.moveAt)
	}
	if s.Team[0].Position != beforeStored {
		t.Errorf("Team[0].Position became %v, want %v untouched: writing it here would "+
			"give the leader a SECOND source of truth for where it stands",
			s.Team[0].Position, beforeStored)
	}
	if !s.walks[0].at.IsZero() {
		t.Errorf("the segment was not dropped: %+v - from == dest still opens no walk",
			s.walks[0])
	}
}

// TestFollowerWalkEndingIsUnchanged is (4): endWalks already records the body (the stop
// it sends NAMES the walked-out destination), so the fix must leave that site alone.
//
// It pins both halves: a walk that HAS ended leaves Team[idx].Position at that walk's
// destination - the point the stop names - and a walk still running writes NOTHING. A
// future edit that "rounded" the end position with an interpolated point, or that wrote
// on every tick, fails here.
func TestFollowerWalkEndingIsUnchanged(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0), Players: Players{}}
	p := newTeam([]uint32{0x40000031, 0x40000032}, [][3]int32{leaderStore, staleStore})
	sess := p.sess
	now := time.Now()

	// A walk that has been walked out: 479 units at 469 u/s, x1.4 + 250 ms of pad.
	sess.openWalk(1, staleStore, followerStand, now.Add(-2*time.Second), walkSpeedTest)
	if !sess.walkEnded(1, now) {
		t.Fatalf("the test's own walk has not ended after 2 s: %+v", sess.walks[1])
	}
	h.endWalks(sess, now)
	if got := sess.Team[1].Position; got != followerStand {
		t.Errorf("a walk that ended leaves the stored position at %v, want its "+
			"destination %v - that is the point the stop named", got, followerStand)
	}
	if !sess.walks[1].at.IsZero() {
		t.Errorf("the ended segment was not cleared: %+v", sess.walks[1])
	}

	// AND A WALK STILL RUNNING WRITES NOTHING: half a second into a 479-unit walk the
	// body is ~230 units along it - not at its destination, and not at the point it
	// started from either, so a write here would be visible rather than accidentally
	// equal to the answer. (1.68 s of pad is what the stop's own clock needs, so this
	// walk is nowhere near ended.)
	far := [3]int32{-919, 2690, 136}
	running := now.Add(-500 * time.Millisecond)
	sess.openWalk(1, followerStand, far, running, walkSpeedTest)
	if sess.walkEnded(1, now) {
		t.Fatalf("the test's own walk already ended after 0.5 s: %+v", sess.walks[1])
	}
	h.endWalks(sess, now)
	if got := sess.Team[1].Position; got != followerStand {
		t.Errorf("endWalks moved a body whose walk is still running to %v, want %v "+
			"untouched", got, followerStand)
	}
}
