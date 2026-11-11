package zone

// THE THREE MOVEMENT LINES ARE OFF BY DEFAULT, AND THE DEFAULT IS SILENT BUT NOT
// BLIND.
//
// A line per echoed move, per stop and per walk leg is a format plus a write on the
// PER-PACKET path: at 50 bots that is ~100 lines/s, at 300 players in one town ~1000,
// and a log that busy buries the one line a reading is looking for. So the line went
// behind `-diag-events` (off in every normal run) and the EVENT stayed, as one atomic
// add counted in the window's METRICS line (moves=, stops=, walks=).
//
// What this file pins is exactly that bargain, and it has to be pinned or the bargain
// can rot in either direction:
//
//  1. OFF, no line is written AND the count is still incremented - a "quiet" default
//     that also stopped counting would not be quieter, it would be blind;
//  2. ON, the line comes back looking the way the readings that rely on it expect;
//  3. the stop and the walk count too, not only the move (they are the other two of
//     the three);
//  4. a dead client's forward failure cannot flood the log: the first few lines print
//     in full, then one per window carries the CURRENT error and a suppressed count -
//     and the full lines keep their OLD wording, so a short run's log is unchanged;
//  5. the METRICS line really carries the three counts.
//
// Everything here goes through the real broadcastMove / broadcastStop / legMoveFor /
// logForwardFailure. What it does NOT prove is a real client's behaviour or the rate a
// real town produces - that stays a live run.

import (
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// diagEventPair is two joined players whose sockets are real loopback pairs, so a
// fan-out reaches somebody (sent > 0) and the line's own gate is what is under test.
// It answers the handler, the mover and the other player.
func diagEventPair(t *testing.T) (*Handler, *Session, *Session, *strings.Builder) {
	t.Helper()
	// latencyPair registers its own t.Cleanup for BOTH ends, so neither client is
	// closed here: closing the far end early would make the server's write fail and
	// turn a "did the line print" test into a forward-failure test.
	serverA, _ := latencyPair(t)
	serverB, _ := latencyPair(t)

	var buf strings.Builder
	h := &Handler{Log: log.New(&buf, "", 0)}
	a := &Session{Conn: serverA, Team: []Character{{Handle: 11}}}
	b := &Session{Conn: serverB, Team: []Character{{Handle: 22}}}
	h.Players.Add(a)
	h.Players.Add(b)
	return h, a, b, &buf
}

// TestMoveLineIsOffByDefaultAndCountedInstead is pin 1: with DiagEvents false the
// echoed move writes NO line, and the window still counts it as one move. The second
// half is the part that matters - silence alone would pass on a server that had lost
// the event entirely.
func TestMoveLineIsOffByDefaultAndCountedInstead(t *testing.T) {
	h, a, _, buf := diagEventPair(t)

	// Other tests in this package write packets too, so the window is zeroed
	// immediately before the measurement, not at the start of the test.
	metrics.take()

	pkt := simPacket(3541)
	h.broadcastMove(a, 0, 11, [3]int32{1, 2, 3}, pkt)

	if text := buf.String(); strings.Contains(text, "MOVE_PC handle") {
		t.Fatalf("the move line is a `-diag-events` diagnostic and DiagEvents is false, "+
			"but it was written:\n%s", text)
	}
	if got := metrics.take().Moves; got != 1 {
		t.Fatalf("the window counted moves = %d, want 1 - OFF must drop the LINE and "+
			"keep the event, or the default is blind and not quiet", got)
	}
}

// TestMoveLineComesBackWithDiagEvents is pin 2: ON, the line names the handle, the
// character slot and the destination, which is what the readings that read it expect -
// and the count stays, because the event did happen.
func TestMoveLineComesBackWithDiagEvents(t *testing.T) {
	h, a, _, buf := diagEventPair(t)
	h.DiagEvents = true

	metrics.take()

	h.broadcastMove(a, 0, 11, [3]int32{1, 2, 3}, simPacket(3541))

	if text := buf.String(); !strings.Contains(text, "MOVE_PC handle 11") {
		t.Fatalf("DiagEvents is true, so the move line is back, but it is not on the log:\n%s",
			text)
	}
	if got := metrics.take().Moves; got != 1 {
		t.Fatalf("the window counted moves = %d, want 1 - turning the line on must not "+
			"switch the counter off", got)
	}
}

// TestStopAndWalkCountersIncrement is pin 3: the OTHER two of the three events are
// counted as well, and with the line off neither writes anything. A counter set that
// counted only the move would have made two thirds of the movement path silent with no
// number against it.
//
// The stop goes through a two-player handler so the fan-out really reaches somebody -
// the line's own gate is `sent > 0`, and a zero-recipient stop would prove nothing.
// The walk goes through guardSession because a walk needs the stance table that gives
// it a speed.
func TestStopAndWalkCountersIncrement(t *testing.T) {
	h, sess, logs := guardSession(t, guardStand)
	stopHandler, a, _, _ := diagEventPair(t)
	stopHandler.Log = log.New(logs, "", 0)

	metrics.take()

	pos := [3]int32{1, 2, 3}
	stopHandler.broadcastStop(a, 0, 11, pos, BuildStopPC(11, pos))

	goal := [3]int32{1100, 1000, 1}
	pkt, _ := h.legMoveFor(sess, 0, goal, ReasonMove)
	if pkt == nil {
		t.Fatalf("the walk leg was refused, so this test has no walk to count:\n%s",
			logs.String())
	}

	snap := metrics.take()
	if snap.Stops != 1 {
		t.Errorf("the window counted stops = %d, want 1", snap.Stops)
	}
	if snap.Walks != 1 {
		t.Errorf("the window counted walks = %d, want 1", snap.Walks)
	}
	text := logs.String()
	if strings.Contains(text, "STOP_PC handle") {
		t.Errorf("the stop line is a `-diag-events` diagnostic and DiagEvents is false, "+
			"but it was written:\n%s", text)
	}
	if strings.Contains(text, "SENT walk char") {
		t.Errorf("the walk line is a `-diag-events` diagnostic and DiagEvents is false, "+
			"but it was written:\n%s", text)
	}
}

// TestForwardFailureIsRateLimited is pin 4. ONE dead client fails once per recipient
// per packet, so a crowded town would write the same error hundreds of times per
// forward. The first forwardFailLines lines print IN FULL and with the OLD wording; the
// rest collapse into one line per window, which carries the CURRENT error and how many
// were held back. The window is shrunk here so the second line arrives in milliseconds
// instead of ten seconds.
func TestForwardFailureIsRateLimited(t *testing.T) {
	oldLines, oldWindow := forwardFailLines, forwardFailWindow
	forwardFailLines, forwardFailWindow = 3, 1*time.Millisecond
	defer func() {
		forwardFailLines, forwardFailWindow = oldLines, oldWindow
	}()

	var buf strings.Builder
	h := &Handler{Log: log.New(&buf, "", 0)}
	errSend := errors.New("session has no connection")

	// TEN failures in a burst: exactly the first three, verbatim.
	for i := 0; i < 10; i++ {
		h.logForwardFailure(1, 2, errSend)
	}
	if n := strings.Count(buf.String(), "zone: could not forward"); n != 3 {
		t.Fatalf("%d forward-failure line(s) printed for a burst of 10, want exactly 3:\n%s",
			n, buf.String())
	}

	// A whole window later, ONE more line carries the current error plus the count of
	// the seven that were held back.
	time.Sleep(3 * time.Millisecond)
	h.logForwardFailure(1, 2, errSend)

	text := buf.String()
	if n := strings.Count(text, "zone: could not forward"); n != 4 {
		t.Fatalf("%d forward-failure line(s) after the window, want exactly 4 - one per "+
			"window after the first three:\n%s", n, text)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	// THE OLD WORDING IS UNCHANGED: every line, suppressed ones included, starts with
	// the same sentence a short run has always printed, so an existing reading of the
	// log still works and only the long-run line gains its tail.
	const prefix = "zone: could not forward handle 1's packet to handle 2: "
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("line %d does not keep the forward-failure wording:\n got: %s\nwant prefix: %s",
				i+1, line, prefix)
		}
	}
	if !strings.Contains(lines[len(lines)-1], "suppressed") {
		t.Errorf("the line after the window does not say how many were suppressed: %s",
			lines[len(lines)-1])
	}
}

// TestMetricsLineCarriesTheMovementCounts is pin 5: the door the information escaped
// through. If the three counts were not on the METRICS line, turning the lines off
// would have thrown the events away, and every test above would be pinning a loss.
func TestMetricsLineCarriesTheMovementCounts(t *testing.T) {
	s := Snapshot{Moves: 7, Stops: 8, Walks: 9, PktsIn: 1}
	line := s.Line(10 * time.Second)
	if !strings.Contains(line, "moves=7 stops=8 walks=9") {
		t.Fatalf("the METRICS line does not carry the movement counts:\n got: %s\nwant it "+
			"to hold: moves=7 stops=8 walks=9", line)
	}
}
