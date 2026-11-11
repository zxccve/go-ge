package zone

// -diag-distance (R135c): A MEASUREMENT, NOT A FEATURE.
//
// WHY IT EXISTS. The sight ring (zone\sight.go, R135) culls at the map's own
// ViewRange - 6000 on map 1331. The owner watched a character pop out at that line
// while his camera could still draw it, so the SERVER was removing an actor the
// CLIENT could still see. The distance at which our client stops drawing a character
// on its own has never been measured: no packet carries it (the client's SetViewDist
// is a CAMERA zoom, source2\geClientShared\include\GeCamera.h:38, and
// PcViewDist/MonViewDist appear in neither tree). So it is measured the only way
// left: leave the actor on the wire (-sightoff) and write down the distances while a
// human watches the screen.
//
// WHAT IT WRITES. Once a second, one line per pair of sessions in the zone, naming
// both LEADERS at their BODIES - the same points the ring itself measures (sightCentre /
// bodyPointAt, R135d) - and their straight X/Y distance. Every stop the zone sends gains
// the distance to the nearest other player, so a character that stops at the very moment
// it vanishes leaves its number behind (see broadcastStop, zone\players.go).
//
// THE POINT IS THE BODY, NOT THE CLICK (R135d). The distance this line exists to measure
// is the distance between two bodies on a screen, and the leader's stored point is the
// destination the client last reported (fact #231). Printing the destination would put a
// character that has only just started walking at the far end of the walk - the very
// number the bug report is about - so the line reads the same leg the ring reads.
//
// IT IS LOG ONLY. No packet, no state and no game rule reads this file; the same
// bytes go on the wire with the switch on and off, and a test pins that.
//
// NOT A GOROUTINE. The lines ride the EXISTING per-session clock (tickIfDue,
// zone\chase.go), the same one the walk, the cast and the ring use. A goroutine of
// its own would be a second reader of positions with a lifetime to get wrong.

import (
	"fmt"
	"math"
	"time"
)

// distLineEvery is the gap between two DIST lines for the same pair.
//
// THE TICK IS NOT THIS RATE. tickIfDue runs a session at most once per pollInterval
// (zone\chase.go) - ten times a second - so the line needs a stamp of its own:
// Session.lastDistLog, compared with this gap and advanced only when a line is
// really written, exactly the way lastTick bounds the tick.
const distLineEvery = time.Second

// formatDistLine is the ONE shape a -diag-distance line has, and it is a function on
// purpose: the shape is the deliverable, so a test pins the string, not a clock.
//
//	zone: DIST <handleA> (x, y) <-> <handleB> (x, y) = <N> units [ring <radius>]
//	zone: DIST <handleA> (x, y) <-> <handleB> (x, y) = <N> units [ring off]
//
// handleA is the VIEWER - the session whose tick wrote the line - and the positions
// are the two LEADERS' BODIES in x, y order (R135d: the point the ring measures, not the
// destination the client last reported). `[ring ...]` is the zone's radius so the number
// can be read against the ring immediately: a line of 5,900 units `[ring 6000]` is
// inside the ring, and 7,000 `[ring 6000]` is outside it.
func formatDistLine(viewer uint32, vpos [3]int32, other uint32, opos [3]int32,
	dist int64, radius int32, armed bool) string {
	ring := "ring off"
	if armed {
		ring = fmt.Sprintf("ring %d", radius)
	}
	return fmt.Sprintf("zone: DIST %d (%d, %d) <-> %d (%d, %d) = %d units [%s]",
		viewer, vpos[0], vpos[1], other, opos[0], opos[1], dist, ring)
}

// xyDistance is the straight-line X/Y distance between two positions, rounded to the
// nearest whole unit. Z is not part of it: the ring compares X/Y only (inRing uses
// distSq on x and y), so a height difference must not make the two numbers disagree.
func xyDistance(a, b [3]int32) int64 {
	return int64(math.Round(math.Sqrt(distSq(a[0], a[1], b[0], b[1]))))
}

// diagDistanceTick writes the pair lines this session's tick owes, and nothing else.
//
// ONE SIDE OF A PAIR WRITES IT. Both sessions tick, so a pair logged from both ends
// would put two lines a second on the record saying the same distance with the two
// positions swapped - a reader counting lines would double every count. The session
// with the LOWER leader handle writes it, which is the same side on every tick, so a
// pair appears exactly once and always with the same viewer.
//
// THE POSITIONS COME FROM ONE SNAPSHOT AND ONE CLOCK. Players.Snapshot copies the roster
// and its stored points under the roster's own read lock and releases it before this loop
// runs: another session may join, leave or move while the lines are built, and NOTHING is
// held while they are written to the log. The BODIES are then read off each session's own
// leg at `now` (the tick's instant), so both ends of one line describe the same moment. A
// session that is not in the world yet has no pairs to write.
func (h *Handler) diagDistanceTick(sess *Session, now time.Time) {
	if !h.DiagDistance || sess == nil || len(sess.Team) == 0 {
		return
	}
	// THE 1/s GATE. tickIfDue already bounds how often this can run (pollInterval);
	// this is what turns 10 ticks a second into one line a second per pair.
	if !sess.lastDistLog.IsZero() && now.Sub(sess.lastDistLog) < distLineEvery {
		return
	}
	members := h.Players.Snapshot()
	me := -1
	for i := range members {
		if members[i].Sess == sess {
			me = i
			break
		}
	}
	if me < 0 {
		return // not in the world yet: no leader position of its own to measure from
	}
	mine := members[me]
	if len(mine.Chars) == 0 || len(mine.Pos) == 0 {
		return
	}
	radius, armed := h.sightRadius()
	myHandle := mine.Chars[0].Handle
	// THE VIEWER'S SIDE IS ITS LEADER'S BODY TOO, read through the ring's own helper so
	// the two halves of one line cannot state two different clocks.
	minePos := h.sightCentre(sess, now)

	var lines []string
	for i := range members {
		m := &members[i]
		if m.Sess == sess || len(m.Chars) == 0 || len(m.Pos) == 0 {
			continue
		}
		other := m.Chars[0].Handle
		if other < myHandle {
			continue // the other side of this pair writes it
		}
		// THE OTHER SIDE IS A BODY AT THE SAME `now` (fact #229): a destination would put
		// a player who just clicked 3,000 units away on this line immediately.
		opos := h.bodyPointAt(m, 0, now)
		lines = append(lines, formatDistLine(myHandle, minePos, other, opos,
			xyDistance(minePos, opos), radius, armed))
	}
	if len(lines) == 0 {
		return // alone in the zone: nothing to measure, and the stamp stays back
	}
	sess.lastDistLog = now
	for _, line := range lines {
		h.Log.Print(line)
	}
}

// distToNearest is the distance from `pos` to the NEAREST other session's leader
// BODY, and that session's leader handle - the suffix broadcastStop appends when
// -diag-distance is on.
//
// NEAREST, NOT EVERYBODY. The stop line is one line per stop, and a stop is
// broadcast to the whole zone; a distance per recipient would turn one line into a
// crowd and could not be read beside a single client's observation. The nearest
// other player is the one most likely to have the character on screen, and in the
// measurement run (two clients) it is the only one - so the line says exactly the
// number the reader is looking at, and no more.
//
// THE OTHER SIDE IS THE BODY (R135d), at ONE clock for the whole scan: `pos` is the
// walk's own goal, which IS where the stopping body stands, so measuring it against
// another player's destination would be the same two-kinds-of-point mistake the ring
// had. A body that is walking away is at its body, not at the end of its walk.
func (h *Handler) distToNearest(sess *Session, pos [3]int32) (uint32, int64, bool) {
	var (
		handle uint32
		best   int64
		found  bool
	)
	now := time.Now()
	for _, m := range h.Players.Snapshot() {
		if m.Sess == sess || len(m.Chars) == 0 || len(m.Pos) == 0 {
			continue
		}
		d := xyDistance(pos, h.bodyPointAt(&m, 0, now))
		if !found || d < best {
			handle, best, found = m.Chars[0].Handle, d, true
		}
	}
	return handle, best, found
}

// stopDistSuffix is the text broadcastStop appends when -diag-distance is on, or ""
// when it is off, nobody else is in the zone, or the flag is off entirely.
//
// WHY ON THE STOP. The interesting moment in the measurement is the one where the
// actor leaves the screen while it is still standing - and a character that stops
// produces exactly one line. With the suffix, the number is on the record at that
// moment without a second look at the log.
func (h *Handler) stopDistSuffix(sess *Session, pos [3]int32) string {
	if !h.DiagDistance {
		return ""
	}
	if handle, d, ok := h.distToNearest(sess, pos); ok {
		return fmt.Sprintf(" | DIST to %d = %d units", handle, d)
	}
	return ""
}
