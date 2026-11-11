package zone

// The S3 counters (zone\metrics.go): two things the log line is only worth
// reading if they are true.
//
//  1. A two-session broadcast records recipients == 1. sendOthers fans ONE packet
//     out to every OTHER session, so with two players in the zone the count is
//     one - that is the whole basis of the crowded-town arithmetic, and if it were
//     off by one (counting the sender, or counting players instead of packets) the
//     300-player estimate would be wrong by exactly that factor.
//  2. The histogram's p50 lands in the bucket the measured values went to. A
//     percentile read out of the wrong bucket silently moves the CPU wall.
//
// Both go through the REAL code: a real loopback socket pair, the real
// Handler.sendOthers, the real Session.send. What a unit test cannot prove is a
// real client's behaviour - that stays a live run.

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"geserver/protocol"
)

// metricsQuiet is a Handler that can broadcast and log without writing a wall of
// text into the test output.
func metricsQuiet() *Handler {
	return &Handler{Log: log.New(io.Discard, "", 0)}
}

// TestMetricsBroadcastRecordsRecipients is pass condition 1: two sessions, one
// broadcast, recipients == 1 - and the packet really left on the OTHER session's
// socket, so a zero would not be "it counted nothing, but it also sent nothing".
func TestMetricsBroadcastRecordsRecipients(t *testing.T) {
	serverA, clientA := latencyPair(t)
	defer clientA.Close()
	serverB, clientB := latencyPair(t)
	defer clientB.Close()

	h := metricsQuiet()
	a := &Session{Conn: serverA, Team: []Character{{Handle: 11}}}
	b := &Session{Conn: serverB, Team: []Character{{Handle: 22}}}
	h.Players.Add(a)
	h.Players.Add(b)

	// Other tests in this package write packets too, so the window is zeroed
	// immediately before the measurement, not at the start of the test.
	metrics.take()

	pkt := simPacket(9001)
	if n := h.sendOthers(a, 11, pkt); n != 1 {
		t.Fatalf("sendOthers to a two-session zone answered %d recipients, want 1", n)
	}
	// It must have gone to B, and B must be able to read it - otherwise the
	// recipient count describes a packet nobody received.
	if got := readSimPacket(t, clientB, 2*time.Second); got != 9001 {
		t.Fatalf("the other session read packet %d, want 9001", got)
	}

	s := metrics.take()
	if s.Broadcasts != 1 {
		t.Fatalf("one sendOthers call recorded %d broadcast(s), want 1", s.Broadcasts)
	}
	if s.Recipients != 1 {
		t.Fatalf("a two-session broadcast recorded %d recipient(s), want 1 (the OTHER "+
			"session only: the sender is not a recipient)", s.Recipients)
	}
	if s.MaxRecipients != 1 {
		t.Fatalf("max recipients recorded %d, want 1", s.MaxRecipients)
	}
	// The fan-out write is counted on the out side, once: this is the Session.send
	// path, which does not go through writePackets.
	if s.PktsOut != 1 || s.BytesOut != uint64(len(pkt)) {
		t.Fatalf("one fan-out packet recorded out=%dpkt/%dB, want 1pkt/%dB",
			s.PktsOut, s.BytesOut, len(pkt))
	}
}

// TestMetricsHistogramP50LandsInMeasuredBucket is pass condition 2: every handler
// time is recorded in ONE bucket, and p50 comes back as that bucket's upper edge -
// inside the bucket's own range, and nowhere else.
func TestMetricsHistogramP50LandsInMeasuredBucket(t *testing.T) {
	// 120 us is comfortably inside a bucket rather than on an edge, so the answer
	// cannot depend on which side of a compare a rounding lands.
	const measured, n = 120, 100
	metrics.take()
	for i := 0; i < n; i++ {
		metrics.handlerMicros(measured)
	}
	s := metrics.take()

	want := histIndex(measured)
	if s.Hist[want] != n {
		t.Fatalf("%d values of %d us landed in bucket %d with count %d, want %d",
			n, measured, want, s.Hist[want], n)
	}
	lo := uint64(0)
	if want > 0 {
		lo = handlerHistEdges[want-1]
	}
	hi := histEdge(want)
	if measured < lo || measured >= hi {
		t.Fatalf("bucket %d covers [%d,%d) but the measured %d us is not in it",
			want, lo, hi, measured)
	}
	p50 := s.percentile(50)
	if p50 != hi {
		t.Fatalf("p50 = %d us, want %d (the upper edge of the bucket every value "+
			"went to), bucket covers [%d,%d)", p50, hi, lo, hi)
	}
	if p50 < lo || p50 > hi {
		t.Fatalf("p50 = %d us is outside bucket %d's range [%d,%d]", p50, want, lo, hi)
	}
	if s.Hist[0] != 0 {
		t.Fatalf("a bucket that received nothing reports %d, want 0", s.Hist[0])
	}
}

// TestMetricsReplyIsTimedAndIdleWindowIsSilent checks the two things around the
// edges: Handler.Reply really records its own duration (the defer, not a manual
// call), and a window with no packets produces NO line - an idle map stays quiet.
func TestMetricsReplyIsTimedAndIdleWindowIsSilent(t *testing.T) {
	if line := metrics.take().Line(10 * time.Second); line != "" {
		t.Fatalf("an empty window logged %q, want no line at all", line)
	}

	h := metricsQuiet()
	sess := &Session{skipped: map[uint16]int{}}
	// An unhandled command: Reply takes its default branch, logs one line (to
	// io.Discard here) and returns nothing. What matters is that the call is
	// TIMED, which is the only thing this test asserts.
	if _, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: 9900, Size: 2, Data: []byte{0xac, 0x26}}); err != nil {
		t.Fatalf("Reply on an unhandled command returned an error: %v", err)
	}
	s := metrics.take()
	var total uint64
	for _, c := range s.Hist {
		total += c
	}
	if total != 1 {
		t.Fatalf("one Reply call recorded %d handler time(s), want 1 - the timing is "+
			"not on the Reply path", total)
	}
	if s.PktsIn != 0 {
		t.Fatalf("a Reply call with no frame read recorded %d packet(s) in, want 0 - "+
			"the inbound counter belongs to Serve, not to Reply", s.PktsIn)
	}
}
