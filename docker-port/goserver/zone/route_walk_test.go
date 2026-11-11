package zone

import (
	"math"
	"testing"
	"time"
)

// R110: a WALK MUST CARRY THE ROUTE IT WALKS.
//
// The clock already counts the route's own length (nav.go, Plan). Before R110 the
// geometry was the straight CHORD between the two ends, so a bent walk timed itself on
// the polyline and then drew the body on a line the body never walked - the owner's log
// shows 539 units between the two (logs\go_zone1331_20261101_night.log,
// 2026/11/02 00:00:59.521), and the swing hard-sets the client's actor to whatever that
// geometry returns (GeActor.cpp:300-309), so the disagreement is a snap on screen.
//
// These tests are all pure arithmetic on walkedRouteAt: no pathtool, no packets, no
// client. The right-angle numbers come straight from the brief.

// routeTime is `at` plus the seconds a body at `speed` u/s needs to travel `distance`.
func routeTime(at time.Time, distance, speed float64) time.Time {
	return at.Add(time.Duration(distance / speed * float64(time.Second)))
}

// TestARightAngleRouteWalksTheCorners is the brief's own route: an L from (0,0) through
// (600,0) to (600,600). Each answer is the point ON the route after that much travel,
// and the cell is the segment's end - the last leg owns the goal's cell, not the start's.
func TestARightAngleRouteWalksTheCorners(t *testing.T) {
	route := [][3]int32{{0, 0, 7}, {600, 0, 7}, {600, 600, 8}}
	at := time.Unix(1_700_000_000, 0)
	const speed = 100.0
	for _, tc := range []struct {
		travel float64
		want   [3]int32
	}{
		{300, [3]int32{300, 0, 7}},
		{900, [3]int32{600, 300, 8}},
		{5000, [3]int32{600, 600, 8}}, // walked out: the last point, never past it
	} {
		got := walkedRouteAt(route, route[0], route[len(route)-1], at,
			routeTime(at, tc.travel, speed), speed)
		if got != tc.want {
			t.Fatalf("travel %g along (%d,%d)->(%d,%d)->(%d,%d) is %v, want %v - the "+
				"body is not being walked along the route itself", tc.travel,
				route[0][0], route[0][1], route[1][0], route[1][1],
				route[2][0], route[2][1], got, tc.want)
		}
	}
}

// segDist is the distance from a point to a SEGMENT (not to its infinite line), the
// nearest-neighbour distance a point on the polyline answers 0 to.
func segDist(px, py, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	if dx == 0 && dy == 0 {
		return math.Hypot(px-ax, py-ay)
	}
	f := ((px-ax)*dx + (py-ay)*dy) / (dx*dx + dy*dy)
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return math.Hypot(px-(ax+f*dx), py-(ay+f*dy))
}

// onRoute is true when the point sits on one of the polyline's segments (within float
// dust). A chord-cutting answer is NOT on the route, and that is the assertion a chord
// model cannot pass.
func onRoute(route [][3]int32, p [3]int32) bool {
	x, y := float64(p[0]), float64(p[1])
	best := math.MaxFloat64
	for i := 1; i < len(route); i++ {
		best = math.Min(best, segDist(x, y,
			float64(route[i-1][0]), float64(route[i-1][1]),
			float64(route[i][0]), float64(route[i][1])))
	}
	// The model truncates to whole units, so a corner walk can sit a fraction of a unit
	// off the mathematical segment; a CHORD through the L is ~200 units off (60% of its
	// own segment length). 1.0 tells those apart with room to spare.
	return best < 1.0
}

// TestABentRoutePutsTheBodyOnThePolyline is the test the chord model FAILS: the same L
// route as above, four seconds in (400 units of travel, still on the first leg). The
// chord from (0,0) to (600,600) has the body at (282, 282) - mid-air, 200 units from the
// route. The route model has it at (400, 0), a point ON the polyline.
func TestABentRoutePutsTheBodyOnThePolyline(t *testing.T) {
	route := [][3]int32{{0, 0, 7}, {600, 0, 7}, {600, 600, 8}}
	at := time.Unix(1_700_000_000, 0)
	const speed = 100.0
	got := walkedRouteAt(route, route[0], route[len(route)-1], at,
		routeTime(at, 400, speed), speed)
	if want := ([3]int32{400, 0, 7}); got != want {
		t.Fatalf("travel 400 should still be on the first leg at %v, is %v", want, got)
	}
	if !onRoute(route, got) {
		t.Fatalf("the body at %v is not on the route - the chord answer the old "+
			"geometry would have sent the client instead", got)
	}
	chord := chordAt(route[0], route[len(route)-1], 400, speed)
	if got == chord {
		t.Fatalf("the route model answers the chord point %v too - it cut the corner", chord)
	}
}

// chordAt is the OLD geometry, spelled out: the straight line between the two ends taken
// at `travel` units of elapsed walking. It is what the body was drawn on before R110,
// kept here so the test above fails loudly if that model comes back.
func chordAt(from, dest [3]int32, travel, speed float64) [3]int32 {
	return walkedToAt(from, dest, time.Unix(1_700_000_000, 0),
		routeTime(time.Unix(1_700_000_000, 0), travel, speed), speed)
}

// TestAStraightRouteWalksExactlyWhereTheChordDid is the regression that says a straight
// walk did not change: for a two-point route, EVERY sampled time must answer EXACTLY what
// walkedToAt answers, to the unit. Equality, not closeness.
func TestAStraightRouteWalksExactlyWhereTheChordDid(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, speed := range []float64{100, 469, 800} {
		for _, tc := range []struct{ from, dest [3]int32 }{
			{[3]int32{1000, 2000, 108}, [3]int32{1540, 1735, 108}}, // a diagonal leg
			{[3]int32{0, 0, 3}, [3]int32{600, 0, 7}},               // level, new cell at the end
			{[3]int32{2281, 861, 108}, [3]int32{2540, 565, 108}},   // the night log's own leg
			{[3]int32{500, 500, 1}, [3]int32{500, 500, 1}},         // zero length: arrived
		} {
			route := [][3]int32{tc.from, tc.dest}
			for i := 0; i <= 20; i++ {
				// Sampled from t=0 to past the end of the leg (the last ones answer the
				// destination on both models; the sampling includes that boundary).
				elapsed := time.Duration(float64(i) / 20 * 3.0 * float64(time.Second))
				now := at.Add(elapsed)
				got := walkedRouteAt(route, tc.from, tc.dest, at, now, speed)
				want := walkedToAt(tc.from, tc.dest, at, now, speed)
				if got != want {
					t.Fatalf("speed %g, leg %v -> %v, %s in: the route model answers %v, "+
						"the chord answers %v - a straight walk changed", speed, tc.from,
						tc.dest, elapsed, got, want)
				}
			}
		}
	}
}

// TestNoRouteFallsBackToTheChord: an empty route, a one-point route, and a non-positive
// speed are all NOT routes to walk - each must answer exactly what walkedToAt answers,
// because those are the walks this side sends without a plan and they were correct.
func TestNoRouteFallsBackToTheChord(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	now := at.Add(900 * time.Millisecond)
	from, dest := [3]int32{1000, 2000, 108}, [3]int32{1540, 1735, 108}
	for name, route := range map[string][][3]int32{
		"nil route":  nil,
		"one-point":  {[3]int32{1540, 1735, 108}},
		"zero-point": {},
	} {
		if got, want := walkedRouteAt(route, from, dest, at, now, 469),
			walkedToAt(from, dest, at, now, 469); got != want {
			t.Fatalf("%s at 900 ms answers %v, the chord answers %v - the fallback moved",
				name, got, want)
		}
	}
	for name, speed := range map[string]float64{"zero speed": 0, "negative speed": -1} {
		if got, want := walkedRouteAt(nil, from, dest, at, now, speed),
			walkedToAt(from, dest, at, now, speed); got != want {
			t.Fatalf("%s answers %v, the chord answers %v - the fallback moved",
				name, got, want)
		}
	}
}

// TestTheChaseCarriesTheRouteItTimes: a PLAN'S own corners must reach the walk, not stop
// at the length. The fake plans an L through (2000,1000) and the walk must hold all three
// points, first = the walk's start, last = its goal.
func TestTheChaseCarriesTheRouteItTimes(t *testing.T) {
	h, _ := chaseHandler(t)
	middle := [3]int32{2000, 1000, 70}
	var planned [][3]int32
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64,
		bool, bool, error) {
		planned = [][3]int32{from, middle, {1370, 1010, 70}}
		return planned[2], planned, polylineLength(planned), false, true, nil
	}
	sess := chaseSession()
	if _, err := h.clickTrigger(sess, frameClick(0, 0x40000031)); err != nil {
		t.Fatal(err)
	}
	w := sess.Walk
	if w == nil {
		t.Fatal("the click started no walk, so there is no route to carry")
	}
	if len(w.route) != 3 {
		t.Fatalf("the walk carries %d route point(s), want the planned 3: %v",
			len(w.route), w.route)
	}
	if w.route[0] != planned[0] || w.route[1] != planned[1] || w.route[2] != w.goal {
		t.Fatalf("the walk's route is not the planned polyline: walk %v, plan %v",
			w.route, planned)
	}
	// AND THE BODY WALKS THE CORNER, not the chord: a quarter of the way through the
	// walk's own clock the body is on the first leg, past the corner the chord would
	// have ignored.
	quarter := w.began.Add(time.Duration(w.eta) / 4)
	if got := walkedRouteAt(w.route, w.from, w.goal, w.began, quarter, w.speed); got[0] > planned[1][0] {
		t.Fatalf("a quarter into the L the body is at %v - past x=%d, the corner the "+
			"chord would skip", got, planned[1][0])
	}
}
