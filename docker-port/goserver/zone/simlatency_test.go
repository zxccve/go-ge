package zone

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// `-simlatency`: the DEVELOPMENT FLAG that holds every outbound packet back by N
// milliseconds so a run on 127.0.0.1 costs something like a real network's latency
// (zone\writer.go).
//
// The flag is no longer a path of its own: every session has ONE writer goroutine
// and ONE queue, and the flag only says how long that writer must wait before it
// writes. So these tests are about the DELAY, and zone\writer_test.go is about the
// queue itself (order across a burst, and the close on overflow).
//
// WHAT THESE TESTS CAN PROVE WITHOUT THE CLIENT: a real loopback socket, the real
// Handler.writeSession and Session.send entry points, and the arrival TIME of the
// bytes on the other end. What they cannot prove is how the game client reacts -
// that is a live run, and it stays a live run.
//
// The two things that matter here, one test each:
//   - 50 ms delays without stretching: two packets handed over 0 ms apart arrive
//     in ORDER, the first no earlier than 45 ms after the send;
//   - the delay is APPLIED PER HAND-OVER and the writer never sleeps in the caller:
//     handing over is instant.

// simPacket builds a small distinguishable packet: cmd 9901 then a sequence
// number. Not a real command id, so nothing in writeFrames' logging rules
// (which special-case 3548) hides it.
func simPacket(seq int) []byte {
	p := make([]byte, 6)
	binary.LittleEndian.PutUint16(p, 9901)
	binary.LittleEndian.PutUint32(p[2:], uint32(seq))
	return p
}

// latencyPair is a real loopback connection pair: the server end is what the zone
// writes to, the client end is what the test reads. TCP rather than net.Pipe so
// the OS buffers the way a real client's socket does - a pipe's Write blocks until
// its Read, which would hide a queue bug behind socket back-pressure.
func latencyPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("dial loopback: %v", err)
	}
	server = <-accepted
	ln.Close()
	if server == nil {
		client.Close()
		t.Fatalf("accept on loopback failed")
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return server, client
}

// readSimPacket reads one whole simPacket from the client end and returns its
// sequence number. A read that does not finish within max fails the test: a
// delayed packet must still ARRIVE, and "it never came" is the failure that a
// silent deadlock would otherwise hide.
func readSimPacket(t *testing.T, client net.Conn, max time.Duration) int {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(max)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("a packet that should have been delivered never arrived "+
			"within %v: %v", max, err)
	}
	return int(binary.LittleEndian.Uint32(buf[2:]))
}

// TestSimLatencyDelaysInOrderAndStartsTheWriter is the main half of the pass
// condition: with 50 ms, two packets handed over at the same instant arrive IN
// ORDER and the FIRST arrives no earlier than 45 ms after the send.
//
// It goes through BOTH write entry points on purpose: writeSession is a session's
// own replies, Session.send is a fan-out packet from another player's goroutine.
// If either of them slept, the goroutine that reads a client's frames would stall -
// a stalled server, not a slow link - and this test's hand-over timing would show it.
func TestSimLatencyDelaysInOrderAndStartsTheWriter(t *testing.T) {
	server, client := latencyPair(t)
	lines := &syncBuffer{}
	h := &Handler{Log: log.New(lines, "", 0), SimLatency: 50 * time.Millisecond}
	sess := &Session{Conn: server}

	if !h.startWriter(sess) {
		t.Fatalf("-simlatency 50 started no writer")
	}
	defer sess.stopWriter()

	// The reader must say the run is simulated, and by how much, BEFORE any packet.
	startLine := waitForLine(t, lines, "SIMULATED LATENCY ON", 2*time.Second)
	if !strings.Contains(startLine, "50 ms") {
		t.Fatalf("the writer's start line does not name the delay:\n%s", startLine)
	}

	first := simPacket(1)
	second := simPacket(2)
	start := time.Now()
	if err := h.writeSession(sess, [][]byte{first}); err != nil {
		t.Fatalf("writeSession to a simulated session returned an error: %v", err)
	}
	// The hand-over itself must be instant: this is the goroutine that reads the
	// client's frames, and a write that BLOCKS here is a stalled server, not a
	// slow link.
	if err := sess.send(second); err != nil {
		t.Fatalf("Session.send to a simulated session returned an error: %v", err)
	}
	handOver := time.Since(start)
	if handOver > 20*time.Millisecond {
		t.Fatalf("handing two packets to the writer took %v - the write path "+
			"is sleeping instead of queueing", handOver)
	}

	got1 := readSimPacket(t, client, 3*time.Second)
	firstArrived := time.Since(start)
	got2 := readSimPacket(t, client, 3*time.Second)
	secondArrived := time.Since(start)

	if got1 != 1 || got2 != 2 {
		t.Fatalf("order was NOT preserved: the client received %d then %d, want 1 then 2",
			got1, got2)
	}
	if firstArrived < 45*time.Millisecond {
		t.Fatalf("the first packet arrived after %v, want no earlier than 45 ms - "+
			"the delay was not applied", firstArrived)
	}
	// A DELAY, not a stretch: the second packet was already due when the first was
	// written, so the pair leaves together. Stretching would mean one sleep per
	// packet, which is a different (and wrong) simulation.
	if gap := secondArrived - firstArrived; gap > 40*time.Millisecond {
		t.Fatalf("the second packet arrived %v after the first: the delay is being "+
			"applied per packet instead of per hand-over", gap)
	}
	t.Logf("50 ms: first arrived %v after the send, second %v, order 1 then 2",
		firstArrived, secondArrived)
}

// TestSimLatencyIsAppliedInsideTheOneWriter pins the thing that would be easy to
// get wrong twice over: with `-simlatency` ON there is still exactly ONE queue and
// ONE writer per session, and the flag is a delay that writer waits, not a second
// goroutine beside it. It also pins that a session with NO delay writes as soon as
// it is handed a packet, which is the normal path every real run takes.
func TestSimLatencyIsAppliedInsideTheOneWriter(t *testing.T) {
	server, client := latencyPair(t)
	lines := &syncBuffer{}
	h := &Handler{Log: log.New(lines, "", 0)}
	sess := &Session{Conn: server}

	if !h.startWriter(sess) {
		t.Fatalf("a Handler with SimLatency 0 started no writer")
	}
	defer sess.stopWriter()
	if sess.outq == nil || sess.outStop == nil {
		t.Fatalf("the writer did not build its queue: outq=%v outStop=%v",
			sess.outq != nil, sess.outStop != nil)
	}
	if sess.simLatency != 0 {
		t.Fatalf("SimLatency 0 became %v on the session", sess.simLatency)
	}

	start := time.Now()
	if err := h.writeSession(sess, [][]byte{simPacket(7)}); err != nil {
		t.Fatalf("writeSession at SimLatency 0: %v", err)
	}
	if got := readSimPacket(t, client, time.Second); got != 7 {
		t.Fatalf("client received sequence %d, want 7", got)
	}
	if elapsed := time.Since(start); elapsed > 45*time.Millisecond {
		t.Fatalf("with -simlatency 0 the packet took %v to arrive - the writer is "+
			"holding packets back that nothing told it to hold", elapsed)
	}
	if strings.Contains(lines.String(), "SIMULATED LATENCY") {
		t.Fatalf("a run without -simlatency logged something new:\n%s", lines.String())
	}
}
