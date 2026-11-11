package zone

// THE SERVER'S COPY OF A CHARACTER'S POSITION MUST NOT WALK AWAY FROM THE BODY ON
// SCREEN, AND SINCE 2026-11-02 IT CANNOT (movement piece 2b).
//
// The measured symptom this file was born from (logs\go_zone1331.log, 2026-11-01): one
// character's consecutive swings carried from-positions 760 units apart three seconds
// apart, and the client hard-sets its own actor to that position on every swing
// (geClientShared\source\GeActor.cpp:309-311: SetGroundPos(pAtkParm->srcPos);
// SetMoveState(CMS_STOP)), so the number in our packet IS the lurch.
//
// THE RULE NOW IS THE REFERENCE'S OWN. A leg starts on the point this zone has ALREADY
// DRAWN the body on - for a character the player drives that is `stand`, read before the
// report is applied - and it ends on the destination the report named. The reference does
// the same with its own body: MS_MOVE's ON_ENTER sets m_moveStartPos = m_pos
// (FieldChar_FSM.cpp:246-270) and ON_UPDATE advances its path agent by
// m_moveSpeed x dt (FieldChar.cpp:240-260). Nothing is inferred from the packet, so
// there is no gap that a correction would have to pay back.
//
// Which is why the twenty-report scenario below is the ORIGINAL measurement again: every
// report re-anchors the model on the body, so the model tracks the screen. The interim
// rules made it stand on a point the CLIENT declared (R106) and then absorbed the gap at
// walking pace (R107); both are gone, and so is the 963-unit snap the owner's own log
// shows at 2026-11-02 18:22:04.412745 (`model was at (2621, -816) 963 u off`).

import (
	"io"
	"log"
	"testing"
	"time"

	"geserver/protocol"
)

const (
	// driftMeasured is the speed the measured reports imply, inside the 250..800 band
	// walkSpeed trusted. It is the number that caused the original bug: the model used to
	// be drawn at THIS speed while the client animates at the one we sent.
	driftMeasured = 800.0
	// driftPacketSpeed is the speed the client was TOLD to walk at, i.e. the speed it
	// animates at. That is the one the server's copy must advance at.
	driftPacketSpeed = MoveBaseSpeed
	// driftInterval is the gap between two rapid reports. It is under the
	// 1200 u/s plausibility ceiling only because the destinations below are far
	// apart, so observeSpeed REJECTS every sample and the measured speed stays
	// the 800 this test set - a chain of clicks, not a walk being measured.
	driftInterval = 100 * time.Millisecond
	// driftSlack is how far AHEAD of the drawn body the server's copy may be. It is
	// this small on purpose: the position in our packet is hard-set onto the client's
	// actor (GeActor.cpp:309-311), so a unit ahead is a unit of lurch on screen.
	driftSlack = 2.0
)

// driftSession builds a zone holding one team at `stand`, with the stance's own
// move speed as the packet speed: 10010 (Bareknuckle) is MoveBaseSpeed, 469.
func driftSession(t *testing.T, handles []uint32, stand [][3]int32) (*Handler, *Session) {
	t.Helper()
	h := &Handler{Log: log.New(io.Discard, "", 0), Stances: speedTable(t)}
	a := newTeam(handles, stand)
	for i := range a.sess.Team {
		a.sess.Team[i].Job = 1
		a.sess.Team[i].Stance = 10010
	}
	h.joinZone(a.sess)
	if got := h.packetSpeed(&a.sess.Team[0]); got != MoveBaseSpeed {
		t.Fatalf("packet speed = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
	return h, a.sess
}

// driftMove sends one CZ_MOVE_PC for team slot idx, exactly as the client does.
func driftMove(t *testing.T, h *Handler, sess *Session, idx int, pos [3]int32) {
	t.Helper()
	body := []byte{byte(idx)}
	body = appendU32(body, uint32(pos[0]))
	body = appendU32(body, uint32(pos[1]))
	body = appendU32(body, uint32(pos[2]))
	pt := pack(CmdCZMovePC, body...)
	if _, err := h.movePC(sess, protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt}); err != nil {
		t.Fatalf("movePC(char %d -> %v): %v", idx, pos, err)
	}
}

// TestMoveDriftTwentyRapidReports is the measurement of the original bug, re-aimed at the
// new rule: twenty rapid reports, each naming a point 1200 units further than the last,
// and the model must stay on the body the client is drawing rather than on any point the
// client declared.
//
// THE BODY IS WHERE THE SCREEN SAYS IT IS. It left `stand` when the first report arrived
// and it has walked at the speed we sent ever since - nothing else can move it, because
// the only packet that hard-sets its position is our own attack. So the number this test
// compares against is `stand + packetSpeed x elapsed`, and the measured 800 u/s that
// used to make the model arrive early is now only a speed SAMPLE, read by nobody on this
// path (the leg is drawn at the speed the client animates).
//
// What the two assertions pin:
//   - the model is never AHEAD of the drawn body (ahead is the lurch on screen);
//   - the last leg starts on the body's own point, not on the destination the report
//     before it named - 1200 units away, and that is exactly the number that used to
//     appear in the move line as `963 u off`.
func TestMoveDriftTwentyRapidReports(t *testing.T) {
	stand := [3]int32{1000, 1000, 1}
	h, sess := driftSession(t, []uint32{0x40000031}, [][3]int32{stand})

	// A measured 800 u/s: the number walkSpeed() would trust, and the number that made
	// every segment start too far along under the old rule.
	sess.MoveSpeed = driftMeasured
	sess.moveSamples = 4
	if got := sess.walkSpeed(); got != driftMeasured {
		t.Fatalf("walkSpeed = %.0f, want the measured %.0f", got, driftMeasured)
	}

	const reports = 20
	var first time.Time
	var beforeLast, lastDest [3]int32
	for i := 0; i < reports; i++ {
		if i > 0 {
			time.Sleep(driftInterval) // rapid: a player clicking, not walking
		}
		// 1200 units per step, so the band's own ceiling rejects the sample and
		// neither the measured nor the animation speed can arrive inside a step.
		dest := [3]int32{stand[0] + int32(1200*(i+1)), stand[1], stand[2]}
		driftMove(t, h, sess, 0, dest)
		beforeLast, lastDest = lastDest, dest
		if i == 0 {
			// THE HANDLER'S OWN CLOCK, not this test's: the walk began when IT read
			// the first report, and the two clocks differing by a millisecond on a
			// loaded machine is the difference between 0 and 470 units here.
			first = sess.moveAt
		}
	}
	last := sess.moveAt

	written := float64(stand[0]) + driftPacketSpeed*last.Sub(first).Seconds()
	// WHERE THE SERVER SAYS IT IS, read exactly as attack.go reads it for the
	// swing's +8, at the same instant the handler last spoke about this character.
	held := sess.drawnPos(0, last, h.packetSpeed(&sess.Team[0]))
	heldLater := sess.drawnPos(0, last.Add(driftInterval), h.packetSpeed(&sess.Team[0]))

	t.Logf("after %d rapid reports over %s: server holds x=%d (and %d one interval "+
		"later), the client has drawn x=%.0f - %.0f units apart at the report; the leg "+
		"the last report opened is %d -> %d",
		reports, last.Sub(first).Round(time.Millisecond), held[0], heldLater[0], written,
		float64(held[0])-written, beforeLast[0], lastDest[0])

	// THE MODEL IS ON THE BODY, and standing exactly where the report found it: the leg
	// it opened began at the drawn point, so the reading at its own stamp is that point.
	if sess.moveFrom != held {
		t.Fatalf("the leg starts at %v, want the body's own point %v", sess.moveFrom, held)
	}
	if behind := written - float64(held[0]); behind > driftPacketSpeed*driftInterval.Seconds()+driftSlack {
		t.Fatalf("the model reads x=%d, %.0f units behind the drawn body (x=%.0f): more "+
			"than the one report interval it can be behind", held[0], behind, written)
	}
	if ahead := float64(held[0]) - written; ahead > driftSlack {
		t.Fatalf("the model reads x=%d, %.0f units AHEAD of the drawn body (x=%.0f): the "+
			"number in our packet is hard-set onto the client's actor, so being ahead is "+
			"the lurch", held[0], ahead, written)
	}
	// AND IT IS NOT ON A POINT THE CLIENT DECLARED. Under the old rule this leg started
	// 1200 units ahead of the body - the destination named one report earlier.
	if flatDist(sess.moveFrom, beforeLast) < 1000 {
		t.Fatalf("the leg starts at %v, near the destination the previous report named "+
			"(%v): the model is anchored on the report again, not on the body",
			sess.moveFrom, beforeLast)
	}
	// AND AN INTERVAL LATER IT IS STILL INSIDE THAT LEG - not past its destination,
	// whatever the clock says.
	if heldLater[0] < sess.moveFrom[0] || float64(heldLater[0]) > float64(lastDest[0])+driftSlack {
		t.Fatalf("one interval into the leg the model reads x=%d, outside the leg "+
			"%d -> %d: the drawn position may never leave the leg it is walking",
			heldLater[0], sess.moveFrom[0], lastDest[0])
	}
	if held[1] != stand[1] {
		t.Fatalf("the copy left the walked line: y = %d, want %d", held[1], stand[1])
	}
}

// TestMoveDriftStandingStartUnchanged pins the other end: a character that has not moved
// yet opens its leg where it STANDS, not at the destination the player just clicked. That
// is the rule the reference's MS_MOVE runs on (`m_moveStartPos = m_pos`), and the one this
// lane makes universal - it is what makes a fight that opens from standing still carry the
// standing place.
func TestMoveDriftStandingStartUnchanged(t *testing.T) {
	stand := [3]int32{2500, 1700, 3}
	h, sess := driftSession(t, []uint32{0x40000031}, [][3]int32{stand})
	if !sess.moveAt.IsZero() {
		t.Fatalf("the character was PLACED with an open segment (%v), so this test "+
			"would not be testing a standing start", sess.moveFrom)
	}

	dest := [3]int32{2600, 1700, 3}
	driftMove(t, h, sess, 0, dest)

	if sess.moveFrom != stand {
		t.Fatalf("segment start = %v, want the standing place %v (a report names a "+
			"DESTINATION; the leg must open where the body stands)",
			sess.moveFrom, stand)
	}
	if got := sess.drawnPos(0, time.Now(), h.packetSpeed(&sess.Team[0])); hypot2(got, stand) > 3 {
		t.Fatalf("just after the report the body reads %.1f units from the standing "+
			"place %v (got %v) - it cannot have moved yet", hypot2(got, stand), stand, got)
	}
}

// TestMoveDriftFollowerStartIsItsOwn pins that the two records stay separate: a
// follower's segment opens at ITS OWN standing place and a follower's report
// leaves the leader's segment alone. The leader's copy is the session's
// (moveFrom/Pos/moveAt) and a follower's is walks[idx]; folding them into one is
// how the whole team would be dragged to the leader's position.
func TestMoveDriftFollowerStartIsItsOwn(t *testing.T) {
	lead := [3]int32{1000, 1000, 1}
	follow := [3]int32{1400, 1000, 1}
	h, sess := driftSession(t, []uint32{0x40000031, 0x40000032}, [][3]int32{lead, follow})

	// The leader walks out first: an open segment of its own, well ahead.
	leadDest := [3]int32{3000, 1000, 1}
	driftMove(t, h, sess, 0, leadDest)
	leadFrom := sess.moveFrom
	if leadFrom != lead {
		t.Fatalf("the leader's segment start = %v, want its standing place %v", leadFrom, lead)
	}

	// Then the follower reports its own walk, from where IT stands.
	followDest := [3]int32{1600, 1000, 1}
	driftMove(t, h, sess, 1, followDest)

	if sess.moveFrom != leadFrom || sess.Pos != leadDest {
		t.Fatalf("the follower's report moved the LEADER's segment: start %v -> %v, "+
			"destination %v (want %v and %v)",
			leadFrom, sess.moveFrom, sess.Pos, leadFrom, leadDest)
	}
	if len(sess.walks) <= 1 || sess.walks[1].at.IsZero() {
		t.Fatalf("the follower opened no segment of its own")
	}
	if w := sess.walks[1]; w.from != follow {
		t.Fatalf("follower segment start = %v, want its own standing place %v",
			w.from, follow)
	}
	if sess.walks[1].from == leadFrom {
		t.Fatalf("follower and leader share one segment start (%v)", leadFrom)
	}
	if got := sess.drawnPos(1, time.Now(), h.packetSpeed(&sess.Team[1])); hypot2(got, follow) > 3 {
		t.Fatalf("the follower reads %.1f units from its own standing place (%v) just "+
			"after its report (got %v)", hypot2(got, follow), follow, got)
	}
}
