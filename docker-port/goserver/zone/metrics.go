package zone

// SERVER-SIDE COUNTERS (R17 / S3). One window, one log line, no locks.
//
// WHY: a load test has to answer three questions and the zone could answer none
// of them. How many bytes does it read and write (what a bandwidth-capped VPS
// pays for)? How many recipients does ONE broadcast touch (the 300-in-a-town
// wall - sendOthers fans every event out to EVERY session)? How long does one
// frame take to handle (is the wall per-packet work, or fan-out)?
//
// THE RULE THAT KEEPS THIS HONEST: nothing here allocates, locks, or looks
// anything up. Every field is an atomic.Uint64, the histogram is a fixed array,
// and the only arithmetic is an add. Instrumentation that changes the thing it
// measures is worse than no instrumentation.
//
// THE READER OF THIS FILE SHOULD KNOW where the two halves are called:
//   - packets/bytes in:  zone\zone.go Serve, once per frame the splitter yields;
//   - packets/bytes out: zone\writer.go writeFrames, once per PACKET on the wire,
//     whoever supplied it - a session's own batch and a fan-out packet both reach
//     the socket through the session's one writer now. Counting per packet (not
//     per syscall) is what keeps out=pkt comparable across that change;
//   - recipients:        zone\players.go sendOthers, once per call;
//   - queue depth:       zone\writer.go enqueue, one atomic max per enqueue - the
//     deepest a session's outbound queue got in the window (qmax=);
//   - queue closes:      zone\writer.go overflow, once per session closed because
//     its client stopped reading (qclose=);
//   - moves/stops/walks: the three movement events that used to be one log line
//     each - zone\players.go broadcastMove and broadcastStop, and zone\chase.go
//     legMove;
//   - sight_enter/leave/reenter: the sight ring's packets, counted per packet
//     (zone\sight.go) - the first sight of an actor, its range-culling leave, and
//     on a RETURN the packets of the same spawned burst (a player's 3520+3572+3554
//     is 3, a monster's 3527 is 1), NOT one 3522/3528;
//   - cull: zone\players.go sendOthers, one atomic add per (viewer, packet) pair the
//     INNER radius (zone\sight.go sightCulls) suppressed - an actor frozen between
//     the map's ViewMiddleRange and ViewRange still announced, its updates dropped;
//   - handler time:      zone\zone.go Reply, in a defer, so a reply that
//     returns an error is timed too.

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// handlerHistEdges are the UPPER edges, in MICROSECONDS, of the handler-time
// histogram. Bucket i covers [lo, hi) with hi = handlerHistEdges[i] and
// lo = the previous edge (0 for the first), and the last bucket is everything
// at or above the final edge. Fixed at compile time: no map, no sort, no slice
// grown per event - a percentile has to cost one atomic add per packet.
//
// The edges are dense where a healthy reply lives (0..500 us) and coarse above
// it, because the interesting question is "is a frame cheap", not "exactly how
// expensive was the expensive one".
var handlerHistEdges = [...]uint64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000}

// handlerHistBuckets is the edge count PLUS the overflow bucket.
const handlerHistBuckets = len(handlerHistEdges) + 1

// histIndex is the bucket a duration in microseconds falls in.
func histIndex(us uint64) int {
	for i, edge := range handlerHistEdges {
		if us < edge {
			return i
		}
	}
	return handlerHistBuckets - 1
}

// histEdge is the number a bucket REPORTS: its own upper edge, so a percentile
// reads as "at most this", never as a false precision. The overflow bucket has
// no upper edge and reports the last real one, i.e. "at least this".
func histEdge(i int) uint64 {
	if i >= len(handlerHistEdges) {
		return handlerHistEdges[len(handlerHistEdges)-1]
	}
	return handlerHistEdges[i]
}

// Metrics is one zone's counter set, all atomics so the packet path never waits.
type Metrics struct {
	pktsIn  atomic.Uint64
	bytesIn atomic.Uint64

	pktsOut  atomic.Uint64
	bytesOut atomic.Uint64

	broadcasts    atomic.Uint64
	recipients    atomic.Uint64
	maxRecipients atomic.Uint64

	// moves, stops and walks count the three movement events that used to be
	// logged one line each (an echoed move, a stop, and a walk leg). Counting one
	// of them is ONE atomic add; a log line is a format plus a write, on the
	// per-packet path. Off by default the line goes and these stay, so a run still
	// answers "how many happened" - see Handler.DiagEvents.
	moves atomic.Uint64
	stops atomic.Uint64
	walks atomic.Uint64

	// walksStraight is the subset of walks that went out as the STRAIGHT CHORD -
	// a walk installed with no route, because the path engine answered none (or
	// this zone has none, or the walk is a step-back, which is arithmetic and is
	// never planned). It is counted next to walks in legMoveFor, so
	// walks - walksStraight is how many walks were ROUTED: the one pair of
	// numbers that says whether a session's movement was planned at all, on a map
	// whose mesh may have a hole under the character (2026-11-09).
	walksStraight atomic.Uint64

	// sightEnter, sightLeave, sightReenter and sightCull count the SIGHT RING's packets
	// (zone\sight.go): the first sight of an actor, the leave that range culling
	// sends, and on a RETURN the packets of the same spawned burst. They are counted
	// PER PACKET SENT, not per event, because that is the number the fan-out costs:
	// a player's first sight is three packets (3520 spawn, 3572, 3554), a monster's
	// is one - and a return now costs exactly the same as a first sight, because the
	// return sends the same burst (the 3522/3528 re-enter form is parked, see
	// sightAnnounce).
	//
	// sightCull is the INNER radius (ViewMiddleRange): one actor's packet NOT sent to
	// one viewer because that viewer's body was past the middle. It is the only number
	// that moves when an actor freezes - the actor stays announced, so no enter and no
	// leave counter changes - which is what makes the freeze measurable in a run.
	sightEnterC   atomic.Uint64
	sightLeaveC   atomic.Uint64
	sightReenterC atomic.Uint64
	sightCullC    atomic.Uint64

	// queueMax and queueClosed are the outbound queue's two numbers
	// (zone\writer.go): the deepest any session's queue got in this window, and how
	// many sessions were closed for not reading. The first answers "is fan-out
	// actually being absorbed by the queues or is one client always at the wall";
	// the second must be 0 in a healthy run, and if it is not, the zone dropped a
	// player instead of stalling every other player's move report on them.
	queueMax          atomic.Uint64
	closedForOverflow atomic.Uint64

	hist [handlerHistBuckets]atomic.Uint64
	// handlerTotal is the SUM of every handled frame's microseconds. The buckets
	// say "at most this edge"; the mean is the one number that resolves INSIDE a
	// bucket, which matters when the question is 0.1 ms or 0.9 ms per move report.
	handlerTotal atomic.Uint64
}

// metrics is THE zone's counter set: one process serves one zone, so one set is
// the whole truth and no handler or session has to carry a pointer to it.
var metrics = &Metrics{}

// packetIn records one frame read from a client.
func (c *Metrics) packetIn(n int) {
	c.pktsIn.Add(1)
	c.bytesIn.Add(uint64(n))
}

// packetOut records one packet written to a client.
func (c *Metrics) packetOut(n int) {
	c.pktsOut.Add(1)
	c.bytesOut.Add(uint64(n))
}

// broadcast records ONE sendOthers call: how many sessions accepted the packet.
// A call that reached nobody still counts as a broadcast (it is work the server
// did, and a mean of recipients over broadcasts is meaningless without it).
func (c *Metrics) broadcast(recipients int) {
	c.broadcasts.Add(1)
	n := uint64(recipients)
	c.recipients.Add(n)
	// Max is a compare-and-swap loop rather than a lock: it runs once per
	// broadcast, and only when the new count is the biggest seen this window.
	for {
		cur := c.maxRecipients.Load()
		if n <= cur || c.maxRecipients.CompareAndSwap(cur, n) {
			return
		}
	}
}

// move, stop and walk record ONE movement event each. The three names are the
// three log lines this file's counters replace: the EVENT is counted even when it
// reached nobody, which is the point - a fan-out that found no one is still work
// the server did, and a count that hid it would under-report the busy minute.
func (c *Metrics) move() { c.moves.Add(1) }
func (c *Metrics) stop() { c.stops.Add(1) }
func (c *Metrics) walk() { c.walks.Add(1) }

// walkStraight records ONE walk sent with no route: the straight chord. It is
// added by the same function that adds walk() (zone\chase.go, legMoveFor), so
// walksStraight can never exceed walks and the difference is the routed count.
func (c *Metrics) walkStraight() { c.walksStraight.Add(1) }

// sightEnter, sightLeave and sightReenter record ONE packet of the sight ring each
// (zone\sight.go). They are per PACKET, not per object, because that is what the
// wire carried: a player's first sight is a spawn plus its 3572 and 3554. A return
// sends that same burst, so sightReenter is added once per packet of it.
func (c *Metrics) sightEnter() { c.sightEnterC.Add(1) }

// sightEnterN records n packets of the enter form in one add - the create-broadcast
// path sends ONE packet to every session that can see the spot, and each of those is
// a packet on the wire like any other.
func (c *Metrics) sightEnterN(n int) { c.sightEnterC.Add(uint64(n)) }

func (c *Metrics) sightLeave()   { c.sightLeaveC.Add(1) }
func (c *Metrics) sightReenter() { c.sightReenterC.Add(1) }

// sightCull records ONE suppressed update of the inner radius (zone\sight.go,
// sightCulls; zone\players.go sendOthers): one packet that would have gone to one
// viewer and did not, because that viewer's body was past the map's ViewMiddleRange
// and still inside ViewRange. One add per (viewer, packet) pair.
func (c *Metrics) sightCull() { c.sightCullC.Add(1) }

// queueDepth records how deep ONE session's outbound queue is after an enqueue, and
// keeps the deepest seen this window. It is a compare-and-swap loop rather than a
// lock, and it only writes when the new depth is a record - the same shape as
// broadcast's maxRecipients, for the same reason: this runs on the fan-out path.
func (c *Metrics) queueDepth(n int) {
	d := uint64(n)
	for {
		cur := c.queueMax.Load()
		if d <= cur || c.queueMax.CompareAndSwap(cur, d) {
			return
		}
	}
}

// queueClosed records ONE session closed because its client stopped reading
// (zone\writer.go, overflow). It is counted per SESSION, not per dropped packet.
func (c *Metrics) queueClosed() { c.closedForOverflow.Add(1) }

// handlerMicros records how long ONE frame took to handle.
func (c *Metrics) handlerMicros(us uint64) {
	c.hist[histIndex(us)].Add(1)
	c.handlerTotal.Add(us)
}

// Snapshot is one window's counters, taken and reset in one step.
type Snapshot struct {
	PktsIn  uint64
	BytesIn uint64

	PktsOut  uint64
	BytesOut uint64

	Broadcasts    uint64
	Recipients    uint64
	MaxRecipients uint64

	// Moves, Stops and Walks are the three movement events of the window, counted
	// where the lines used to be written (see Metrics.moves/stops/walks).
	//
	// WalksStraight is the subset of Walks that went out with NO route - the
	// straight chord - so Walks-WalksStraight is how many were routed. It is the
	// number that answers "did this session's movement get planned at all" on a
	// map whose mesh may have no ground under the character.
	Moves         uint64
	Stops         uint64
	Walks         uint64
	WalksStraight uint64

	// SightEnter, SightLeave and SightReenter are the sight ring's packets in this
	// window, counted per packet (zone\sight.go). SightReenter counts the packets of
	// the burst sent on a RETURN - the same 3520+3572+3554 / 3527 a first sight
	// sends - not one re-enter frame.
	//
	// Cull is the inner radius: how many of one actor's packets were NOT sent to a
	// viewer whose body was past the map's ViewMiddleRange (zone\players.go
	// sendOthers). An actor that freezes moves this number and nothing else.
	SightEnter   uint64
	SightLeave   uint64
	SightReenter uint64
	Cull         uint64

	// QueueMax is the deepest outbound queue any session reached in the window, in
	// packets; QueueClosed is how many sessions were closed for not reading.
	QueueMax    uint64
	QueueClosed uint64

	Hist [handlerHistBuckets]uint64
	// HandlerTotal is the sum of the window's handler microseconds; the mean is
	// HandlerTotal / sum(Hist).
	HandlerTotal uint64
}

// take reads every counter and resets it, so the next window starts at zero.
// Swap instead of Load+Store: a packet that lands between the two would be lost
// from both windows, and the log would under-report the busiest moment.
func (c *Metrics) take() Snapshot {
	s := Snapshot{
		PktsIn:        c.pktsIn.Swap(0),
		BytesIn:       c.bytesIn.Swap(0),
		PktsOut:       c.pktsOut.Swap(0),
		BytesOut:      c.bytesOut.Swap(0),
		Broadcasts:    c.broadcasts.Swap(0),
		Recipients:    c.recipients.Swap(0),
		MaxRecipients: c.maxRecipients.Swap(0),
		Moves:         c.moves.Swap(0),
		Stops:         c.stops.Swap(0),
		Walks:         c.walks.Swap(0),
		WalksStraight: c.walksStraight.Swap(0),
		SightEnter:    c.sightEnterC.Swap(0),
		SightLeave:    c.sightLeaveC.Swap(0),
		SightReenter:  c.sightReenterC.Swap(0),
		Cull:          c.sightCullC.Swap(0),
		QueueMax:      c.queueMax.Swap(0),
		QueueClosed:   c.closedForOverflow.Swap(0),
		HandlerTotal:  c.handlerTotal.Swap(0),
	}
	for i := range c.hist {
		s.Hist[i] = c.hist[i].Swap(0)
	}
	return s
}

// percentile answers the bucket edge pct percent of the window's handler times
// fell at or below.
func (s Snapshot) percentile(pct uint64) uint64 {
	var total uint64
	for _, n := range s.Hist {
		total += n
	}
	if total == 0 {
		return 0
	}
	target := (pct*total + 99) / 100 // round UP: the percentile must cover pct
	var cum uint64
	for i, n := range s.Hist {
		cum += n
		if cum >= target {
			return histEdge(i)
		}
	}
	return histEdge(handlerHistBuckets - 1)
}

// Line renders the window as the one METRICS line, or "" when nothing arrived:
// an idle map must not write a line per interval, or the log of a real server
// is a wall of zeroes with the interesting minutes buried in it.
func (s Snapshot) Line(interval time.Duration) string {
	if s.PktsIn == 0 {
		return ""
	}
	mean := 0.0
	if s.Broadcasts > 0 {
		mean = float64(s.Recipients) / float64(s.Broadcasts)
	}
	// The mean is the only figure that resolves INSIDE a bucket: the percentiles
	// report their bucket's upper edge, so p50=1000us means "at most 1 ms", while
	// avg says whether the typical frame is 0.1 ms or 0.9 ms.
	var handled uint64
	for _, n := range s.Hist {
		handled += n
	}
	var avg uint64
	if handled > 0 {
		avg = s.HandlerTotal / handled
	}
	// The three movement counts ride at the END of the line, after the timing.
	// They are the same three events whose per-event lines are off by default
	// (Handler.DiagEvents): a reader who asks "did the walk happen" reads walks=,
	// and only turns the line itself back on for a single event's details.
	//
	// qmax= and qclose= close the line (zone\writer.go). qmax is the deepest outbound
	// queue any one session reached, in packets, against the 1024 that closes a
	// session; qclose counts the sessions that were closed for not reading. A run
	// where qmax climbs towards the limit is a run where fan-out is outrunning the
	// slowest client - and qclose above 0 says it already happened.
	//
	// cull= is the INNER radius (zone\players.go sendOthers, zone\sight.go sightCulls):
	// the updates dropped because the viewer's body was past the map's ViewMiddleRange.
	// The actor stays announced, so enter/leave stay flat while a freeze is happening -
	// this is the one number that shows it.
	//
	// walksStraight= rides directly after walks= (zone\chase.go legMoveFor): the
	// walks of this window that went out with NO route, i.e. the straight chord.
	// walks - walksStraight is the routed count, so one line answers "was this
	// session's movement planned at all" without turning any per-walk line on.
	return fmt.Sprintf("METRICS %s in=%dpkt/%dB out=%dpkt/%dB bcast=%d recipients mean=%.2f max=%d "+
		"handler avg=%dus p50=%dus p95=%dus p99=%dus moves=%d stops=%d walks=%d walksStraight=%d "+
		"qmax=%d qclose=%d sight_enter=%d sight_leave=%d sight_reenter=%d cull=%d",
		interval, s.PktsIn, s.BytesIn, s.PktsOut, s.BytesOut, s.Broadcasts,
		mean, s.MaxRecipients, avg, s.percentile(50), s.percentile(95), s.percentile(99),
		s.Moves, s.Stops, s.Walks, s.WalksStraight, s.QueueMax, s.QueueClosed,
		s.SightEnter, s.SightLeave, s.SightReenter, s.Cull)
}

// StartReporter runs the ONE goroutine that logs a METRICS line every interval
// and stops when stop closes. An interval of 0 or less, or no writer, does
// nothing at all - the flag's "off" value.
func StartReporter(w io.Writer, interval time.Duration, stop <-chan struct{}) {
	if w == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if line := metrics.take().Line(interval); line != "" {
					fmt.Fprintln(w, line)
				}
			}
		}
	}()
}
