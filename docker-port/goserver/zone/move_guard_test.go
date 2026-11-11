package zone

// THE MOVE GUARD IS AN ALARM, NOT A CLAMP.
//
// The rule lives in zone\move_guard.go. A CZ_MOVE_PC carries a DESTINATION chosen by
// the client's own pathfinding, not a position, so two consecutive destinations can be
// any distance apart while the body walks smoothly - which is why the old speed-based
// clamp fired 50 times in 100 seconds of ordinary play and moved the model to points the
// client never reported (logs\go_zone1331.log, 2026-11-01 15:23).
//
// These three tests pin the replacement:
//
//  1. the exact report from that log (641 units) is used byte for byte, however long the
//     gap was;
//  2. one 5000-unit jump writes ONE line naming both points and the distance, and the
//     jump is still used as sent;
//  3. no arithmetic can change x, y or the cell - forward, backwards along the body's own
//     walk, or across the sign of both axes.
//
// Every test drives the REAL packet path (CZ_MOVE_PC in, the stored position and the
// echoed point out), because the corruption this file stops happened downstream of the
// guard, not inside it.

import (
	"bytes"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// guardStand is where these characters stand: an ordinary map point with a cell.
var guardStand = [3]int32{1000, 1000, 1}

// guardSession builds a zone holding ONE team at stand, standing in Bareknuckle
// (10010), whose packet speed is MoveBaseSpeed 469, plus the log the caller reads.
func guardSession(t *testing.T, stand [3]int32) (*Handler, *Session, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), Stances: speedTable(t)}
	a := newTeam([]uint32{0x40000031}, [][3]int32{stand})
	for i := range a.sess.Team {
		a.sess.Team[i].Job = 1
		a.sess.Team[i].Stance = 10010
	}
	h.joinZone(a.sess)
	if got := h.packetSpeed(&a.sess.Team[0]); got != MoveBaseSpeed {
		t.Fatalf("packet speed = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
	return h, a.sess, &buf
}

// guardReport sends one CZ_MOVE_PC for team slot idx, exactly as the client does.
func guardReport(t *testing.T, h *Handler, sess *Session, idx int, pos [3]int32) {
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

// guardAlarmLines counts the alarm lines, NOT every MOVE_PC line: the ordinary echo says
// "MOVE_PC char ...", the alarm says "MOVE_PC ALARM char ...".
func guardAlarmLines(buf *bytes.Buffer) int { return strings.Count(buf.String(), "MOVE_PC ALARM") }

// guardDistance is the ground-plane distance between two world points, as the guard
// measures it.
func guardDistance(a, b [3]int32) float64 {
	return math.Hypot(float64(a[0]-b[0]), float64(a[1]-b[1]))
}

// TestMoveReportSixFortyOneUnitsIsUsedAsSent: the report the clamp rewrote in the
// owner's own log - 641 units, 409 ms after the previous one - is now stored and echoed
// EXACTLY as sent, all three ints.
//
// The old rule allowed 469 u/s x1.5 x0.409 s + 50 = 337 units and shortened this report
// to (1337, 1000, cell 55): a point the client never sent, 304 units short of its own
// path. The sleep is there so the 409 ms is REAL - with the clamp gone the clock cannot
// enter the answer, and this test fails if anyone puts a timer back in.
func TestMoveReportSixFortyOneUnitsIsUsedAsSent(t *testing.T) {
	h, sess, buf := guardSession(t, guardStand)

	// A previous report at the body's own point, so there is a "previous report" at all.
	guardReport(t, h, sess, 0, guardStand)
	time.Sleep(409 * time.Millisecond)

	want := [3]int32{guardStand[0] + 641, guardStand[1], 55}
	if d := guardDistance(want, guardStand); d != 641 {
		t.Fatalf("this test means to send a 641 unit re-path, built one of %.0f", d)
	}
	guardReport(t, h, sess, 0, want)

	if sess.Pos != want {
		t.Fatalf("stored position = %v, want the report's own three ints %v - a 641 unit "+
			"re-path was rewritten (the old clamp shortened it to 337 units along the "+
			"same direction)", sess.Pos, want)
	}
	if sess.LastPos != want {
		t.Fatalf("last reported position = %v, want %v - the echo other players get "+
			"must carry the same point", sess.LastPos, want)
	}
	if n := guardAlarmLines(buf); n != 0 {
		t.Fatalf("%d alarm line(s) for a 641 unit re-path, want none (the alarm is at "+
			"3000 units):\n%s", n, buf.String())
	}
}

// TestMoveReportFiveThousandUnitsLogsOneAlarmAndIsUsedAsSent: a jump no walk could make
// is now VISIBLE - one line naming both points and the distance - and it is still stored
// and echoed as sent.
//
// A previous report is sent first, exactly as a real client streams them, so the jump is
// NOT a character's first report after entry (which any version of this file accepted
// unchanged).
//
// FAILURE THIS PINS, in both directions: no alarm at all hides a teleporting client, and
// any shortening (the old clamp stored a bounded step instead) is the corruption this
// change removes.
func TestMoveReportFiveThousandUnitsLogsOneAlarmAndIsUsedAsSent(t *testing.T) {
	h, sess, buf := guardSession(t, guardStand)

	guardReport(t, h, sess, 0, guardStand) // the report before the jump
	far := [3]int32{guardStand[0] + 5000, guardStand[1], 5}
	guardReport(t, h, sess, 0, far)

	if sess.Pos != far {
		t.Fatalf("stored position = %v, want the jump's own three ints %v - the alarm "+
			"reports, it does not rewrite", sess.Pos, far)
	}
	if sess.LastPos != far {
		t.Fatalf("last reported position = %v, want %v", sess.LastPos, far)
	}
	if n := guardAlarmLines(buf); n != 1 {
		t.Fatalf("%d alarm line(s) for one 5000 unit jump, want exactly one:\n%s",
			n, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"char 0",
		"(6000, 1000, cell 5)", // the destination, as reported
		"(1000, 1000, cell 1)", // the last known point, as the model had it
		"5000 units",           // the distance between them
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the alarm line does not name %q:\n%s", want, out)
		}
	}
}

// TestMoveReportKeepsAllThreeIntsInEveryDirection: nothing in the move path may do
// arithmetic on the point. x, y and the cell survive untouched whichever way the report
// points - forward, BACKWARDS along the walk the body is already on, or across the sign
// of both axes.
//
// FAILURE THIS PINS: the old guard shortened a step along its own direction and REFUSED
// a step that went backwards (the body was left where the model had it) - so the
// direction of a report decided what the server believed, and a re-path behind the body
// was silently discarded.
//
// Each case sends the report a real client would have sent just before it, so the tested
// report is never a character's first after entry - the one case every version accepted
// unchanged, and the reason this test would otherwise pass on the clamp too.
func TestMoveReportKeepsAllThreeIntsInEveryDirection(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		h, sess, buf := guardSession(t, guardStand)
		guardReport(t, h, sess, 0, guardStand)
		want := [3]int32{guardStand[0] + 2900, guardStand[1] + 400, 21}
		guardReport(t, h, sess, 0, want)
		if sess.Pos != want || sess.LastPos != want {
			t.Fatalf("stored %v / last reported %v, want %v untouched",
				sess.Pos, sess.LastPos, want)
		}
		if n := guardAlarmLines(buf); n != 0 {
			t.Fatalf("%d alarm line(s) under the 3000 unit alarm:\n%s",
				n, buf.String())
		}
	})

	t.Run("backwards along the body's own walk", func(t *testing.T) {
		h, sess, buf := guardSession(t, [3]int32{0, 0, 1})
		guardReport(t, h, sess, 0, [3]int32{0, 0, 1})
		// Then one second into a walk from (0,0) toward (2000,0): the model draws the
		// body about 469 units along, and the walk's direction is +x. The report asks
		// for the walk's START, i.e. 469 units BEHIND the body.
		sess.moveFrom = [3]int32{0, 0, 1}
		sess.Pos = [3]int32{2000, 0, 1}
		sess.moveAt = time.Now().Add(-time.Second)

		want := [3]int32{0, 0, 1}
		guardReport(t, h, sess, 0, want)
		if sess.Pos != want || sess.LastPos != want {
			t.Fatalf("stored %v / last reported %v, want %v untouched - a destination "+
				"behind the body is still a destination the client chose",
				sess.Pos, sess.LastPos, want)
		}
		if n := guardAlarmLines(buf); n != 0 {
			t.Fatalf("%d alarm line(s) for a 469 unit re-path:\n%s", n, buf.String())
		}
	})

	t.Run("across the sign of both axes", func(t *testing.T) {
		h, sess, buf := guardSession(t, guardStand)
		guardReport(t, h, sess, 0, guardStand)
		want := [3]int32{guardStand[0] - 1500, guardStand[1] - 1600, 12}
		guardReport(t, h, sess, 0, want)
		if sess.Pos != want || sess.LastPos != want {
			t.Fatalf("stored %v / last reported %v, want %v untouched - negative x and "+
				"y must survive the round trip", sess.Pos, sess.LastPos, want)
		}
		if n := guardAlarmLines(buf); n != 0 {
			t.Fatalf("%d alarm line(s) under the 3000 unit alarm:\n%s",
				n, buf.String())
		}
	})
}
