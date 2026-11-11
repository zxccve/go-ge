package zone

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// errQueueFull is what enqueue answers after it has closed this session for
// overflow. The session is already closed and the line already written by then, so
// a caller must not treat it as news - see sendOthers.
var errQueueFull = errors.New("session closed: outbound queue full")

// errWriterGone is what enqueue answers once this session's writer has stopped on a
// write error. It is NOT errQueueFull on purpose: a full queue has already written
// its own line and sendOthers stays quiet about it, while a dead socket is a forward
// failure and sendOthers reports it through the rate limiter, exactly as it did
// when the write error came back to the caller.
var errWriterGone = errors.New("session closed: writer stopped")

// ONE WRITER PER SESSION. EVERY outbound packet - a session's own reply AND every
// packet another player's goroutine fans out to it - is handed to a per-session
// queue and written by ONE goroutine that belongs to that session.
//
// WHY IT IS BUILT THIS WAY (measured 2026-11-06, 50 fake players in one zone, 45 s,
// map 10): zone\players.go sendOthers wrote each copy SYNCHRONOUSLY to every other
// session in the sender's own goroutine - 49 sequential Conn.Write calls per move
// report. The zone burned 2.0-2.8% of one core, but a handler's WALL time was
// 1.9-2.3 ms per frame against ~0.17 ms of CPU: almost all of it was that wait, and
// a player feels it as fan-out tail latency (p99 8.9-11 ms at 50 players, 20-25 ms
// extrapolated at 100). Logging was measured and was NOT the cause.
//
// WHAT CHANGED: fan-out is now an ENQUEUE. The sender never touches another
// session's socket, so one player's move report costs one channel send per
// recipient instead of one syscall per recipient.
//
// ORDER IS THE SUBTLE PART, SO IT IS SAID ONCE, HERE. Before this file a session's
// socket had ONE writer in effect because every write took Session.wmu: the
// session's own batch could not be split by a fan-out packet from another
// goroutine. That guarantee is now kept by THREE rules, and breaking any one of
// them reorders the wire:
//  1. one queue per session, and exactly ONE goroutine reads it - so the order
//     things were enqueued in is the order they leave on the socket;
//  2. a session's OWN batch (Handler.writeSession, an entry burst or a gate
//     handshake) is enqueued as ONE item, never one item per packet - so no other
//     goroutine's packet can land in the middle of it, exactly as wmu allowed;
//  3. the writer drains in queue order and concatenates. Concatenating is legal on
//     the wire because every frame is already length-prefixed; it is not a new
//     packet, just several packets in one syscall.
//
// THE QUEUE IS BOUNDED, AND A CLIENT THAT CANNOT KEEP UP IS DISCONNECTED. A queue
// that could not be bounded would let one stalled client grow this process's memory
// without end; a queue that blocked its producer would put the stall straight back
// into the sender, which is the thing this file exists to remove. So the limit is
// checked on the way in, the check never blocks, and on overflow this session is
// closed and said so in ONE line (rate limited, like the forward-failure line).
//
// THERE IS NO BATCHING TIMER, ON PURPOSE. The writer runs when it has something to
// write and writes it at once; it never holds a packet back to wait for company.
// Waiting for company would trade the one thing this task must not lose - latency -
// for syscalls. The coalescing falls out of the queue being non-empty: 49 packets
// that arrive while the writer is busy are written together without anybody waiting
// for them on purpose.
//
// `-simlatency` KEEPS WORKING AND KEEPS ITS OWN QUEUE. It is now a DELAY APPLIED
// INSIDE THIS WRITER (a packet is not written before `sentAt + simLatency`), not a
// second queue beside it: one session has one writer, always, and the flag only
// changes when that writer is allowed to write. With the flag off (the default and
// every normal run) the delay is zero and that comparison is always already true,
// so the writer writes immediately.

// The queue's limits. Both are checked; whichever is reached first closes the
// session. They are package variables rather than constants for ONE reason: a test
// has to reach the limit in milliseconds instead of pushing 1024 packets through a
// socket, exactly as forwardFailLines is a variable for the same reason. Nothing
// outside a test writes them.
//
// 1024 packets / 256 KiB is a backlog of many seconds at the rate a real client
// reads - a client that has not read 1024 packets is not behind, it is gone. It is
// also small enough that 300 sessions all stuck cannot cost more than ~75 MB.
var (
	outQueueDepth = 1024        // packets
	outQueueBytes = 256 * 1024  // bytes
)

// outBatch is ONE enqueue: the packets that must be written together and in this
// order, and the instant they were handed over.
//
// It is a BATCH and not a packet because Handler.writeSession hands over a session's
// own run of packets (an entry burst, a gate handshake) as one item - see rule 2 at
// the top of this file. A single fan-out packet is a batch of one.
type outBatch struct {
	pkts   [][]byte
	bytes  int
	sentAt time.Time
}

// startWriter gives ONE session its writer goroutine and returns false when there
// is nothing to write to. It is called where the session is created (zone\zone.go
// Serve) BEFORE anything can be sent to it, and the caller defers stopWriter.
func (h *Handler) startWriter(sess *Session) bool {
	if sess == nil || sess.Conn == nil {
		return false
	}
	sess.simLatency = h.SimLatency
	sess.outLog = h.Log
	sess.outq = make(chan *outBatch, outQueueDepth)
	sess.outStop = make(chan struct{})
	sess.outLimited = func(msg string) { h.logLimited(&h.queueFail, "queue-overflow", msg) }

	// SAID ONCE PER SESSION, before anything is written, so a reader who picks the
	// log up halfway knows the run is simulated and by how much - a late reply in a
	// log that says nothing about the flag reads as a server fault.
	if h.SimLatency > 0 && h.Log != nil {
		h.Log.Printf("zone: SIMULATED LATENCY ON for %s: every packet this session is "+
			"sent is delayed by %d ms on the way out (applied inside the session's own "+
			"writer, queue %d packets / %d KiB, a session that falls behind it is "+
			"closed). Read side untouched; restart WITHOUT -simlatency for a normal run.",
			sess.Conn.RemoteAddr(), h.SimLatency.Milliseconds(), outQueueDepth, outQueueBytes>>10)
	}
	go h.drainWriter(sess)
	return true
}

// stopWriter ends this session's writer goroutine. Whatever is still queued is
// dropped: the session is over, and a packet delivered after the socket is closed
// belongs to nobody. Safe to call more than once and from any goroutine.
func (s *Session) stopWriter() {
	if s == nil || s.outStop == nil {
		return
	}
	s.outStopOnce.Do(func() { close(s.outStop) })
}

// enqueue hands ONE batch to this session's writer and returns at once. It is the
// only way a packet reaches a socket while a writer is running.
//
// It answers errNoSessionConn when there is no socket, and an error when the queue
// is full - but by then the session has already been closed and the line written,
// so a caller that logs its error must not treat this as news (see players.go
// sendOthers). It NEVER blocks: the sender's goroutine is the goroutine that reads
// a client's frames, and stalling it here is the failure this file exists to remove.
func (s *Session) enqueue(pkts [][]byte) error {
	if s == nil || s.Conn == nil {
		return errNoSessionConn
	}
	if len(pkts) == 0 {
		return nil
	}
	// No writer (a Session built by hand in a test, or any future path that does not
	// go through Serve): write inline, exactly as every session did before this file.
	// The queue is the normal path, not a mandatory one - a caller that builds its
	// own Session gets the old behaviour rather than a queue nobody drains.
	if s.outq == nil {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		for _, p := range pkts {
			if _, err := s.Conn.Write(p); err != nil {
				return err
			}
			metrics.packetOut(len(p))
		}
		return nil
	}
	// CLOSED IS CLOSED. The queue would empty itself after the close (the writer
	// stops, and a batch already in its hands is counted out before it dies), so
	// without this a closed session could quietly accept a few more packets into a
	// queue nobody will ever drain. The sender is told at once instead.
	if s.outOverflowed.Load() {
		return errQueueFull
	}
	if s.outDead.Load() {
		return errWriterGone
	}

	n := 0
	for _, p := range pkts {
		n += len(p)
	}
	// COUNT FIRST, THEN CHECK, and undo the count on refusal. Two producers can
	// overshoot by one batch between them; a bound that is a few packets soft is
	// still a bound, and the alternative is a mutex on the enqueue path - which is
	// the wait this file removes.
	if s.outBytes.Add(int64(n)) > int64(outQueueBytes) {
		s.outBytes.Add(-int64(n))
		s.overflow()
		return errQueueFull
	}
	if s.outPkts.Add(int64(len(pkts))) > int64(outQueueDepth) {
		s.outPkts.Add(-int64(len(pkts)))
		s.outBytes.Add(-int64(n))
		s.overflow()
		return errQueueFull
	}

	b := &outBatch{pkts: pkts, bytes: n, sentAt: time.Now()}
	select {
	case s.outq <- b:
		metrics.queueDepth(int(s.outPkts.Load()))
		return nil
	default:
		// Cannot happen while outPkts is under the limit (the queue holds
		// outQueueDepth batches and outPkts counts at least one packet per batch),
		// but the enqueue must be non-blocking by construction, not by argument.
		s.outPkts.Add(-int64(len(pkts)))
		s.outBytes.Add(-int64(n))
		s.overflow()
		return errQueueFull
	}
}

// overflow closes this session because its client is not reading. It is idempotent:
// the session is closed once, counted once, and the line is written once per
// session however many packets hit the wall afterwards.
func (s *Session) overflow() {
	if !s.outOverflowed.CompareAndSwap(false, true) {
		return
	}
	msg := fmt.Sprintf("zone: %s is not reading: its outbound queue is full (limit %d "+
		"packets / %d KiB), so the session is closed - the sender was never blocked, "+
		"which is what keeps one stalled client from slowing the zone",
		s.Conn.RemoteAddr(), outQueueDepth, outQueueBytes>>10)
	metrics.queueClosed()
	// ONE line per session, and the limiter collapses a whole town of them, so a
	// crowd of stalled clients cannot bury the rest of the log.
	if s.outLimited != nil {
		s.outLimited(msg)
	} else if s.outLog != nil {
		s.outLog.Print(msg)
	}
	// Closing the socket is what makes the session actually go: Serve's read loop
	// wakes with an error and runs the ordinary disconnect path (leaveZone and the
	// last-point save), so an overflow leaves the zone in the same state a killed
	// client does. The writer's own goroutine stops on outStop, closed by Serve.
	if s.Conn != nil {
		s.Conn.Close()
	}
}

// drainWriter is the ONE goroutine per session. It takes the oldest batch, waits
// until that batch is allowed to be written (only ever waits while `-simlatency` is
// on), drains everything else that is already allowed, concatenates them, and
// issues ONE Conn.Write.
func (h *Handler) drainWriter(sess *Session) {
	// held is a batch taken off the queue that was not yet due. Only `-simlatency`
	// produces one: without a delay everything taken is written straight away, so
	// the writer's queue and the local variable are never both non-empty.
	var held *outBatch

	for {
		head := held
		held = nil
		if head == nil {
			select {
			case <-sess.outStop:
				return
			case head = <-sess.outq:
			}
		}
		// Counted OUT here, once, whichever way it was taken: a batch taken off the
		// queue, or the one held from an earlier pass because it was not due yet (it
		// stayed counted while it waited, and this is the pass that consumes it).
		sess.outPkts.Add(-int64(len(head.pkts)))
		sess.outBytes.Add(-int64(head.bytes))

		if wait := time.Until(head.sentAt.Add(sess.simLatency)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-sess.outStop:
				timer.Stop()
				return
			}
		}

		// COALESCE: take every batch that is due right now. In a normal run (no
		// delay) everything queued is due, so the whole backlog leaves in one write -
		// this is where 49 syscalls become one. Under `-simlatency` a batch that is
		// not due yet is held for the next pass, which is what keeps each packet's
		// own delay without stretching the pair.
		frames := make([][]byte, 0, 16)
		frames = append(frames, head.pkts...)
		for {
			select {
			case next := <-sess.outq:
				if time.Until(next.sentAt.Add(sess.simLatency)) > 0 {
					// NOT DUE YET: keep it, and KEEP IT COUNTED, because it is still
					// waiting to go out and the queue's bound is about what is waiting.
					held = next
				} else {
					sess.outPkts.Add(-int64(len(next.pkts)))
					sess.outBytes.Add(-int64(next.bytes))
					frames = append(frames, next.pkts...)
					continue
				}
			case <-sess.outStop:
				return
			default:
			}
			break
		}

		if err := h.writeFrames(sess, frames); err != nil {
			// A write error closes the session, exactly as it always did: nothing more
			// can go out, the reader notices, and the ordinary disconnect path runs.
			// outDead is what stops anything else being queued for a writer that is
			// gone - without it a dead session would swallow packets into a queue
			// nobody drains until the bound closed it for the wrong reason.
			sess.outDead.Store(true)
			// SAID THROUGH THE LIMITER, not once per session: a zone restarting with
			// 300 players disconnects 300 sockets, and 300 identical lines is the flood
			// the limiter exists to stop. A session already closed for overflow has had
			// its own line, so this one stays quiet there.
			if h.Log != nil && !sess.outOverflowed.Load() {
				h.logLimited(&h.writeFail, "write-failure", fmt.Sprintf(
					"zone: write to %s failed, this session's writer stops: %v",
					sess.Conn.RemoteAddr(), err))
			}
			sess.Conn.Close()
			return
		}
	}
}

// writeFrames puts a run of packets on the wire in ONE Conn.Write and then records
// and logs each one, exactly as the old per-packet writer recorded and logged them.
//
// The concatenation is safe because every frame is already length-prefixed: the
// client's splitter reads the first frame's length, skips it, and finds the next
// header there. The bytes on the wire are the concatenation of packets that would
// otherwise have been written one at a time, in the same order.
func (h *Handler) writeFrames(sess *Session, frames [][]byte) error {
	total := 0
	for _, p := range frames {
		total += len(p)
	}
	buf := make([]byte, 0, total)
	for _, p := range frames {
		buf = append(buf, p...)
	}
	if _, err := sess.Conn.Write(buf); err != nil {
		return err
	}
	// S3 counters: ONE record per PACKET, not per syscall, so out=pkt and the byte
	// count mean exactly what they meant before the writer existed and a reading can
	// compare windows across the change.
	for _, p := range frames {
		metrics.packetOut(len(p))
		// A ZC_MOVE_PC IS NOT LOGGED HERE (R105). It is logged where it is BUILT, with
		// the goal on it: "SENT walk char N -> (x, y) cell C speed S" for a walk this
		// side started (zone\chase.go, legMoveFor) and "MOVE_PC char N handle H -> ..."
		// for the echo of the client's own report (zone\zone.go, movePC). A bare
		// "SENT cmd 3548, 30 bytes" said nothing about WHERE the actor was told to go,
		// so a reading could not tell two walks that agree from two that contradict -
		// and a duplicate 3548 is exactly what re-anchors an actor mid-stride.
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCMovePC {
			continue
		}
		if h.Log != nil {
			h.Log.Printf("zone: SENT cmd %d, %d bytes",
				binary.LittleEndian.Uint16(p), len(p))
		}
	}
	return nil
}
