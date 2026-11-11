package zone

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// realEngine is the game's own path helper, or nil when this machine has none.
// Its absence is not a failure: the chase has a straight-line fallback and the
// Linux target will use build-time route data instead.
func realEngine(t *testing.T) *PathEngine {
	t.Helper()
	engine := &PathEngine{
		Tool:   filepath.Join("..", "..", "navmesh", "pathtool.exe"),
		Mesh:   filepath.Join("..", "..", "navmesh", "mesh", "reb_basic.tok"),
		Radius: 50,
	}
	if !engine.Available() {
		t.Skipf("no path engine at %s / %s", engine.Tool, engine.Mesh)
	}
	// The engine keeps ONE pathtool.exe running until it is closed (nav.go,
	// navWorker), so every test that builds one gives it back: a test binary that
	// walked away from it would leave a live helper behind on this machine.
	t.Cleanup(func() { engine.Close() })
	return engine
}

// TestPathEnginePlanOnRealMap runs the real helper against the real mesh: a route
// from the spawn to a town NPC, with the stand-off applied.
func TestPathEnginePlanOnRealMap(t *testing.T) {
	engine := realEngine(t)
	from := [3]int32{5779, 303, 149} // the spawn cell
	npc := [3]int32{7566, 1425, 165} // AUCH_LEON_SUM/LEO_SERVICE, measured 505 units out

	goal, _, routeLen, overlap, ok, err := engine.Plan(from, npc, DialogStandoff(NPCDefaultRadius))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !ok {
		t.Fatal("the clamp refused the player's own ground at the spawn")
	}
	if overlap {
		// Legal, but the spawn is in the open plaza, so expect a real stand-off here.
		t.Errorf("the spawn clamped as an OVERLAP - unexpected for open ground")
	}

	// The goal must be a stand-off: DISTANCE_TO_CHAR outside the NPC's own body (piece 2d
	// took the number from the target's shape, PlayerChar_FSM.cpp:355 + FieldObject.h:12),
	// and with a cell of its own (the clamp answered it, it is not the NPC's own triangle).
	want := DialogStandoff(NPCDefaultRadius)
	dist := hypot2(goal, npc)
	if dist < want-40 || dist > want+40 {
		t.Errorf("stand-off is %.0f units from the NPC, want about %.0f", dist, want)
	}
	if goal == npc {
		t.Error("the stand-off is the NPC's own spot without being an overlap")
	}
	if routeLen <= 0 {
		t.Error("no route length - the clock would fall back to the straight line")
	}
	t.Logf("goal %v, %.0f units from the NPC, route %.0f units", goal, dist, routeLen)
}

// TestPathEngineNoRouteOnBadPoint: a destination off the mesh must be refused, not
// guessed at. This is the "unreachable NPC" answer - send nothing and say why.
func TestPathEngineNoRouteOnBadPoint(t *testing.T) {
	engine := realEngine(t)
	// Far outside the map's walkable area.
	from := [3]int32{5779, 303, 149}
	npc := [3]int32{900000, 900000, 1}

	_, _, _, _, ok, err := engine.Plan(from, npc, DialogStandoff(NPCDefaultRadius))
	if err != nil {
		t.Skipf("the helper refused the query itself: %v", err)
	}
	if ok {
		t.Fatal("a point 900k units off the map was accepted as a walk destination")
	}
}

// TestPathEngineRouteIsTheClientLegsPlanner runs Route - the function that plans a
// CLIENT's own reported leg (move.go, Handler.clientRoute) - against the real helper and
// the real mesh. It is not the chase's Plan: there is no stand-off and no NPC, only "how
// does the ground go between these two points", and its two ends must come back exactly
// as they went in.
func TestPathEngineRouteIsTheClientLegsPlanner(t *testing.T) {
	engine := realEngine(t)
	from := [3]int32{5779, 303, 149} // the spawn cell
	npc := [3]int32{7566, 1425, 165} // AUCH_LEON_SUM/LEO_SERVICE

	route, ok := engine.Route(from, npc)
	if !ok {
		t.Fatalf("no route from %v to %v on the real mesh", from, npc)
	}
	if route[0] != from {
		t.Errorf("the route starts at %v, want the point it was asked from %v", route[0], from)
	}
	if last := route[len(route)-1]; last != npc {
		t.Errorf("the route ends at %v, want the point it was asked to %v", last, npc)
	}
	t.Logf("route: %d points, %.0f units of ground", len(route), polylineLength(route))

	// AND A POINT OFF THE MAP HAS NO ROUTE - the caller's answer is the straight chord,
	// never a guess at geometry the mesh does not have.
	if _, ok := engine.Route(from, [3]int32{900000, 900000, 1}); ok {
		t.Fatal("a point 900k units off the map was given a route")
	}
}

// TestPathEngineResolvesAForeignCell is the 2026-11-09 fault and its fix: a cell that is
// not the mesh's triangle for a point used to make the engine answer "no route" - which
// the zone drew as a straight chord, the chase's hit-and-miss. Both ends are now resolved
// on the mesh, with x and y kept exactly.
func TestPathEngineResolvesAForeignCell(t *testing.T) {
	engine := realEngine(t)
	// The spawn and the NPC, both asking with cell 1 - not the mesh's triangle there.
	from := [3]int32{5779, 303, 1}
	to := [3]int32{7566, 1425, 1}

	route, ok := engine.Route(from, to)
	if !ok {
		t.Fatal("a foreign cell was refused instead of resolved - the leg would be a chord")
	}
	if route[0][0] != from[0] || route[0][1] != from[1] {
		t.Errorf("the route starts at %v, want x/y of %v kept exactly", route[0], from)
	}
	if last := route[len(route)-1]; last[0] != to[0] || last[1] != to[1] {
		t.Errorf("the route ends at %v, want x/y of %v kept exactly", last, to)
	}
	if route[0][2] == 1 || route[len(route)-1][2] == 1 {
		t.Errorf("the route still carries the foreign cell 1: %v .. %v",
			route[0], route[len(route)-1])
	}
}

func hypot2(a, b [3]int32) float64 {
	return math.Hypot(float64(a[0]-b[0]), float64(a[1]-b[1]))
}

// TestAClientLegIsPlannedByTheRealEngine is the wiring, end to end: one CZ_MOVE_PC through
// a real Handler whose Route is the real path helper, on the real mesh. It proves the three
// pieces find each other - movePC asks h.clientRoute, that asks PathEngine.Route, and the
// answer is stored on the leg - and that every sampled position is on the polyline the
// engine returned.
//
// The two points are the ones TestPathEngineRouteIsTheClientLegsPlanner already proved
// routable on this mesh, so a machine WITH the helper tests the real answer and a machine
// without it skips (realEngine).
func TestAClientLegIsPlannedByTheRealEngine(t *testing.T) {
	engine := realEngine(t)
	stand := [3]int32{5779, 303, 149} // the spawn cell
	dest := [3]int32{7566, 1425, 165} // AUCH_LEON_SUM/LEO_SERVICE

	h, sess := driftSession(t, []uint32{0x40000031}, [][3]int32{stand})
	h.Route = engine.Route
	driftMove(t, h, sess, 0, dest)

	if sess.moveFrom != stand {
		t.Fatalf("the leg starts at %v, want the body's own point %v", sess.moveFrom, stand)
	}
	if !legRouteMatches(sess.moveRoute, stand, dest) {
		t.Fatalf("the leg carries %v, which is not a route from %v to %v - the engine's "+
			"answer did not reach the leg", sess.moveRoute, stand, dest)
	}
	legLen := legLength(stand, dest, sess.moveRoute)
	if legLen < flatDist(stand, dest)-1 {
		t.Fatalf("the leg measures %.0f units, less than the straight %.0f: a route can "+
			"never be shorter than the line between its ends", legLen, flatDist(stand, dest))
	}
	spd := h.packetSpeed(&sess.Team[0])
	for step := 1; step < 10; step++ {
		at := sess.moveAt.Add(time.Duration(
			legLen * float64(step) / 10 / spd * float64(time.Second)))
		p := sess.drawnPos(0, at, spd)
		if d := distanceToPolyline(p, sess.moveRoute); d > 1.5 {
			t.Fatalf("at %.0f of %.0f units the body reads %v, %.1f units off the route "+
				"the engine returned %v", legLen*float64(step)/10, legLen, p, d, sess.moveRoute)
		}
	}
	if late := sess.drawnPos(0, sess.moveAt.Add(time.Minute), spd); late != dest {
		t.Fatalf("the walked-out leg reads %v, want the destination %v", late, dest)
	}
	t.Logf("leg %.0f units of ground over %d route point(s), ends at %v",
		legLen, len(sess.moveRoute), dest)
}
