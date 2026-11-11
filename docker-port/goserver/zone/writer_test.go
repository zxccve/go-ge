package zone

import (
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE ONE WRITER PER SESSION (zone\writer.go): what a unit test can prove without
// the client, and only that.
//
// Three claims, one test each:
//   - ORDER: packets handed over in an order leave in that order, all of them, even
//     when the writer was busy while they piled up. The wire is the oracle: the test
//     reads the real socket.
//   - COALESCING: a backlog is written with a number of Conn.Write calls that does
//     not grow with the backlog. This is the whole point of the change - 49
//     sequential syscalls per move report become about one - and a test that only
//     counted delivered packets would not notice if it silently went back.
//   - THE BOUND: a client that stops reading is CLOSED and counted (qclose) and the
//     sender is never blocked, instead of the queue growing without end.
//
// What is NOT proved here: that the game client accepts the concatenated stream.
// That is a live run against the real client, and it stays one.

// countingConn counts the Write calls that reach the socket, and can hold the FIRST
// one so a backlog builds up while the writer is busy - which is the state the
// coalescing has to handle.
type countingConn struct {
	net.Conn
	gate chan struct{} // closed to let the first write through; nil = never hold

	mu     sync.Mutex
	writes int
	bytes  int
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	first := c.writes == 1
	c.mu.Unlock()
	if first && c.gate != nil {
		<-c.gate
	}
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	c.bytes += n
	c.mu.Unlock()
	return n, err
}

func (c *countingConn) count() (writes, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.bytes
}

// TestWriterPreservesOrderAcrossABurst is the ordering claim: 500 packets handed
// over as fast as the test can hand them over arrive as 1..500 with nothing missing
// and nothing reordered, even though the writer was draining the whole time.
func TestWriterPreservesOrderAcrossABurst(t *testing.T) {
	server, client := latencyPair(t)
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	sess := &Session{Conn: server}
	if !h.startWriter(sess) {
		t.Fatalf("no writer started")
	}
	defer sess.stopWriter()

	const total = 500
	// Read CONCURRENTLY: a loopback socket buffers only so much, and the point here
	// is the order the bytes were written, not how much the socket will hold.
	got := make(chan int, total)
	go func() {
		for i := 0; i < total; i++ {
			if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				got <- -1
				return
			}
			buf := make([]byte, 6)
			if _, err := io.ReadFull(client, buf); err != nil {
				got <- -1
				return
			}
			got <- int(buf[5])<<24 | int(buf[4])<<16 | int(buf[3])<<8 | int(buf[2])
		}
	}()

	for i := 0; i < total; i++ {
		if err := sess.send(simPacket(i)); err != nil {
			t.Fatalf("hand-over %d of %d: %v", i+1, total, err)
		}
	}
	for i := 0; i < total; i++ {
		select {
		case seq := <-got:
			if seq != i {
				t.Fatalf("packet %d on the wire carried sequence %d - the order changed "+
					"or a packet was lost", i, seq)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d packets arrived", i, total)
		}
	}
}

// TestWriterCoalescesABacklogIntoFewWrites is the measured claim, in a unit test: a
// backlog of 300 packets is written with a handful of Conn.Write calls, not one per
// packet. The gate holds the first write, so every one of the 300 is queued before
// the writer can go round again - exactly the state a move report's fan-out puts a
// session's queue in.
func TestWriterCoalescesABacklogIntoFewWrites(t *testing.T) {
	server, client := latencyPair(t)
	cc := &countingConn{Conn: server, gate: make(chan struct{})}
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	sess := &Session{Conn: cc}
	if !h.startWriter(sess) {
		t.Fatalf("no writer started")
	}
	defer sess.stopWriter()

	const total = 300
	// The client has to be reading, or the socket buffer fills and the writer's own
	// writes block - which would look like coalescing that never happens.
	read := make(chan int, total)
	go func() {
		n := 0
		for n < total {
			if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				break
			}
			buf := make([]byte, 6)
			if _, err := io.ReadFull(client, buf); err != nil {
				break
			}
			n++
		}
		read <- n
	}()

	// First packet: the writer takes it and blocks in the gate.
	if err := sess.send(simPacket(0)); err != nil {
		t.Fatalf("hand-over 0: %v", err)
	}
	// Everything else piles up behind it.
	deadline := time.Now().Add(2 * time.Second)
	for i := 1; i < total; i++ {
		if err := sess.send(simPacket(i)); err != nil {
			t.Fatalf("hand-over %d: %v", i, err)
		}
		if i == total/2 && time.Now().After(deadline) {
			t.Fatalf("handing over %d packets blocked the caller - the queue is not "+
				"absorbing them", total)
		}
	}
	close(cc.gate)

	if n := <-read; n != total {
		t.Fatalf("client received %d of %d packets", n, total)
	}
	writes, bytes := cc.count()
	// 300 packets, one syscall each, would be 300. One for the head and a few more
	// for whatever arrived later is the shape we want; anything above a handful means
	// the coalescing is not happening.
	if writes > 10 {
		t.Fatalf("%d Conn.Write calls for %d packets - the backlog is NOT being "+
			"coalesced into one write per drain", writes, total)
	}
	if bytes != total*6 {
		t.Fatalf("%d bytes on the wire, want %d - packets were dropped or duplicated",
			bytes, total*6)
	}
	t.Logf("%d packets in %d Conn.Write call(s), %d bytes", total, writes, bytes)
}

// TestWriterClosesAQueueThatOverflows is the bound: with the queue shrunk to a few
// packets, a client that is not reading gets its session CLOSED, the close is
// counted once (qclose) and logged once, and the sender is never blocked.
//
// net.Pipe is used on purpose: it has no buffer, so the writer's first Write blocks
// until somebody reads, which is exactly a client that has stopped reading.
func TestWriterClosesAQueueThatOverflows(t *testing.T) {
	oldDepth, oldBytes := outQueueDepth, outQueueBytes
	outQueueDepth, outQueueBytes = 4, 1<<20 // bytes out of the way: this test is the packet bound
	defer func() { outQueueDepth, outQueueBytes = oldDepth, oldBytes }()

	metrics.take() // zero this test's window; other tests may have counted first

	server, client := net.Pipe()
	defer client.Close()
	lines := &syncBuffer{}
	h := &Handler{Log: log.New(lines, "", 0)}
	sess := &Session{Conn: server}
	if !h.startWriter(sess) {
		t.Fatalf("no writer started")
	}
	defer sess.stopWriter()

	start := time.Now()
	var lastErr error
	for i := 0; i < 50; i++ {
		lastErr = sess.send(simPacket(i))
		if lastErr != nil {
			break
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("handing packets to a session that is not reading took %v - the "+
			"sender was blocked", elapsed)
	}
	if lastErr == nil {
		t.Fatalf("50 packets went into a queue capped at %d and the session was not "+
			"closed", outQueueDepth)
	}
	if !strings.Contains(lastErr.Error(), "queue") {
		t.Fatalf("the error the sender saw does not say why: %v", lastErr)
	}
	if !sess.outOverflowed.Load() {
		t.Fatalf("the session was not marked overflowed")
	}

	// THE SESSION IS CLOSED: a further hand-over is refused at once, and the socket
	// the client holds is finished.
	if err := sess.send(simPacket(999)); err == nil {
		t.Fatalf("the session kept accepting packets after it was closed for overflow")
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatalf("the client's socket is still open")
	}

	// COUNTED AND SAID ONCE. One session, one line, however many packets hit the wall.
	if got := metrics.take().QueueClosed; got != 1 {
		t.Fatalf("qclose counted %d, want 1", got)
	}
	if n := strings.Count(lines.String(), "is not reading"); n != 1 {
		t.Fatalf("the close was logged %d time(s), want exactly 1:\n%s", n, lines.String())
	}
}

// TestWriterStopsAcceptingAfterAWriteError is the other way a session ends: the
// socket fails under the writer. The writer must stop, the session must not keep
// swallowing packets into a queue nobody will drain, and the failure must be said
// through the limiter rather than once per packet.
func TestWriterStopsAcceptingAfterAWriteError(t *testing.T) {
	server, client := net.Pipe()
	lines := &syncBuffer{}
	h := &Handler{Log: log.New(lines, "", 0)}
	sess := &Session{Conn: server}
	if !h.startWriter(sess) {
		t.Fatalf("no writer started")
	}
	defer sess.stopWriter()

	// The client is gone: the writer's next write fails.
	client.Close()
	if err := sess.send(simPacket(1)); err != nil {
		t.Fatalf("the first hand-over must be accepted (the failure happens in the "+
			"writer, not in the caller): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !sess.outDead.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("the writer did not notice the socket was gone")
		}
		time.Sleep(time.Millisecond)
	}

	// FROM HERE the session refuses, and says why. The wording names the writer, so a
	// caller reading the error knows the socket died rather than the queue filling.
	err := sess.send(simPacket(2))
	if err == nil {
		t.Fatalf("a session with a dead writer accepted another packet")
	}
	if !strings.Contains(err.Error(), "writer") {
		t.Fatalf("the refusal does not say the writer stopped: %v", err)
	}
	if n := strings.Count(lines.String(), "writer stops"); n != 1 {
		t.Fatalf("the write failure was logged %d time(s), want exactly 1:\n%s",
			n, lines.String())
	}
}
