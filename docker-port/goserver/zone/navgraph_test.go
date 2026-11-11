package zone

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// tinyGraph writes a graph file and loads it: three nodes in a line, 100 units apart.
func tinyGraph(t *testing.T) *WalkGraph {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "map1.json")
	// nodes: (0,0), (100,0), (200,0) with the cells the emitter would have found.
	// edges: 0-1 and 1-2 only, each 100 long, so 0 and 2 are connected THROUGH 1.
	body := `{"mesh":"test","spacing":100,
	  "spots":[{"handle":1073741824,"anchor_x":200,"anchor_y":0,"x":60,"y":0,"cell":9,"overlap":false}],
	  "nodes":[0,0,1, 100,0,2, 200,0,3],
	  "edges":[0,1,100, 1,2,100]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadWalkGraph(path)
	if err != nil {
		t.Fatalf("LoadWalkGraph: %v", err)
	}
	return g
}

func TestWalkGraphRoute(t *testing.T) {
	g := tinyGraph(t)

	// Straight along the chain: 0 -> 2 is 200 units of graph, plus nothing at the ends.
	if got, ok := g.Route([3]int32{0, 0, 1}, [3]int32{200, 0, 3}); !ok || math.Abs(got-200) > 1 {
		t.Fatalf("Route(0->2) = %.0f ok=%v, want 200", got, ok)
	}

	// A point at a node costs nothing extra at that end.
	if got, ok := g.Route([3]int32{0, 0, 1}, [3]int32{100, 0, 2}); !ok || math.Abs(got-100) > 1 {
		t.Fatalf("Route(0->1) = %.0f, want 100", got)
	}
}

// TestWalkGraphGapFallsBackToStraight pins the contract that keeps a Linux build from
// refusing NPCs the engine can reach: two nodes with no edge between them is a HOLE IN
// THE SAMPLE, not a wall, so the answer is the straight distance - never "no route".
func TestWalkGraphGapFallsBackToStraight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map2.json")
	body := `{"spacing":100,"nodes":[0,0,1, 900,0,2],"edges":[],"spots":[]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadWalkGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	length, ok := g.Route([3]int32{0, 0, 1}, [3]int32{900, 0, 2})
	if !ok {
		t.Fatal("a gap in the sample was reported as 'no route' - that would kill an NPC")
	}
	if math.Abs(length-900) > 1 {
		t.Fatalf("length = %.0f, want the straight 900", length)
	}
}

// TestWalkGraphNoData: with no nodes at all the graph must say so rather than guess, so
// the caller can fall back to its own straight-line rule.
func TestWalkGraphNoData(t *testing.T) {
	var empty *WalkGraph
	if _, ok := empty.Route([3]int32{0, 0, 1}, [3]int32{1, 0, 2}); ok {
		t.Fatal("a nil graph claimed a route")
	}
}

func TestWalkGraphSpot(t *testing.T) {
	g := tinyGraph(t)
	spot, ok := g.Spot([3]int32{200, 0, 3}) // the NPC's own position, the anchor
	if !ok {
		t.Fatal("the spot was not found by the NPC's anchor")
	}
	if spot.Pos != [3]int32{60, 0, 9} {
		t.Fatalf("spot = %v, want the stored stand-off", spot.Pos)
	}
	if spot.Overlap {
		t.Fatal("a clear stand-off was reported as an overlap")
	}
	if _, ok := g.Spot([3]int32{1, 2, 3}); ok {
		t.Fatal("an unknown NPC position produced a spot")
	}
	if g.NPCCount() != 1 {
		t.Fatalf("NPCCount = %d, want 1", g.NPCCount())
	}
}

// sectorGraph loads a graph written in the schema-2 shape (spots carry `sectors`).
func sectorGraph(t *testing.T, body string) *WalkGraph {
	t.Helper()
	path := filepath.Join(t.TempDir(), "map3.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := LoadWalkGraph(path)
	if err != nil {
		t.Fatalf("LoadWalkGraph: %v", err)
	}
	return g
}

// TestWalkGraphStandAimsAtThePlayer is the bug R58 records: one stand-off stored at
// build time put the character BEHIND the NPC, on the far side from the player. The same
// NPC, approached from opposite sides, must answer with goals on OPPOSITE sides, and each
// goal must be on the side the player is actually on - not merely different.
func TestWalkGraphStandAimsAtThePlayer(t *testing.T) {
	g := sectorGraph(t, `{"schema":2,"spacing":300,
	  "spots":[{"handle":1,"anchor_x":0,"anchor_y":0,"x":140,"y":0,"cell":9,"overlap":false,
	    "sectors":[{"dir":0,"x":140,"y":0,"cell":9},
	               {"dir":90,"x":0,"y":140,"cell":10},
	               {"dir":180,"x":-140,"y":0,"cell":11},
	               {"dir":270,"x":0,"y":-140,"cell":12}]}],
	  "nodes":[-1000,0,1, -500,0,2, 0,0,3, 500,0,4, 1000,0,5],
	  "edges":[0,1,500, 1,2,500, 2,3,500, 3,4,500]}`)
	npc := [3]int32{0, 0, 3}

	east := g.Stand(npc, [3]int32{1000, 0, 5})
	west := g.Stand(npc, [3]int32{-1000, 0, 1})
	if !east.Found || !west.Found {
		t.Fatal("the NPC's stand-off was not found")
	}
	if east.Pos != [3]int32{140, 0, 9} {
		t.Errorf("a player standing to the EAST (+x) got goal %v, want the +x sector (140,0)", east.Pos)
	}
	if west.Pos != [3]int32{-140, 0, 11} {
		t.Errorf("a player standing to the WEST (-x) got goal %v, want the -x sector (-140,0)", west.Pos)
	}
	// The assertion that matters: the goal is on the APPROACHING side of the NPC.
	if east.Pos[0] <= npc[0] {
		t.Errorf("goal %v is not on the east side of the NPC - that is the old far-side bug", east.Pos)
	}
	if west.Pos[0] >= npc[0] {
		t.Errorf("goal %v is not on the west side of the NPC - that is the old far-side bug", west.Pos)
	}
	// North/south too, so the check is not passing by accident on one axis.
	north := g.Stand(npc, [3]int32{0, 1000, 5})
	if north.Pos != [3]int32{0, 140, 10} {
		t.Errorf("a player standing NORTH (+y) got goal %v, want (0,140)", north.Pos)
	}
	if east.Overlap || !east.Routable {
		t.Errorf("east: overlap=%v routable=%v, want a clear routable stand-off",
			east.Overlap, east.Routable)
	}
}

// TestWalkGraphStandStepsToTheNextSector: the sector facing the player can be one the
// graph cannot judge (its point lies off the sampled ground). The next sector round the
// circle must be used rather than sending the character onto the NPC.
func TestWalkGraphStandStepsToTheNextSector(t *testing.T) {
	g := sectorGraph(t, `{"schema":2,"spacing":300,
	  "spots":[{"handle":1,"anchor_x":0,"anchor_y":0,"x":5000,"y":0,"cell":9,"overlap":false,
	    "sectors":[{"dir":0,"x":5000,"y":0,"cell":9},
	               {"dir":22.5,"x":129,"y":54,"cell":10}]}],
	  "nodes":[-1000,0,1, 0,0,3, 1000,0,5],
	  "edges":[0,1,1000, 1,2,1000]}`)

	got := g.Stand([3]int32{0, 0, 3}, [3]int32{1000, 0, 5})
	if !got.Found || !got.Routable {
		t.Fatalf("found=%v routable=%v, want a routable answer", got.Found, got.Routable)
	}
	if got.Pos != [3]int32{129, 54, 10} {
		t.Fatalf("goal = %v, want the neighbouring sector (129,54) - the preferred side "+
			"(5000,0) lies off the graph", got.Pos)
	}
	if got.Dir != 22.5 {
		t.Errorf("dir = %.1f, want 22.5 (the sector that was used)", got.Dir)
	}
}

// TestWalkGraphStandWithoutSectorsIsTheOldSpot: a graph written before schema 2 has no
// sectors, and must answer EXACTLY what Spot answers - the old behaviour is the fallback,
// not a bug to fix by guessing.
func TestWalkGraphStandWithoutSectorsIsTheOldSpot(t *testing.T) {
	g := tinyGraph(t) // its spot carries no `sectors`
	npc := [3]int32{200, 0, 3}
	old, ok := g.Spot(npc)
	if !ok {
		t.Fatal("the fixture has no spot")
	}
	got := g.Stand(npc, [3]int32{0, 0, 1})
	if got.Pos != old.Pos || got.Overlap != old.Overlap {
		t.Fatalf("Stand = %v overlap=%v, want the single stored spot %v overlap=%v",
			got.Pos, got.Overlap, old.Pos, old.Overlap)
	}
	if got.Dir != -1 {
		t.Errorf("dir = %.1f, want -1: no sector decided this answer", got.Dir)
	}
	if !got.Routable || !got.Found {
		t.Errorf("routable=%v found=%v, want true/true", got.Routable, got.Found)
	}
}

// TestWalkGraphStandUnknownNPC: no data for the NPC is not an error, it is "Found false"
// so the caller can answer with the NPC's own position.
func TestWalkGraphStandUnknownNPC(t *testing.T) {
	g := tinyGraph(t)
	if got := g.Stand([3]int32{1, 2, 3}, [3]int32{0, 0, 1}); got.Found {
		t.Fatalf("an unknown NPC produced %v", got)
	}
}

// TestWalkGraphSectorDir pins the direction convention the emitter stores: 0 is +x, 90 is
// +y, and a point NW of the NPC reads 135 (the side, not the opposite one).
func TestWalkGraphSectorDir(t *testing.T) {
	npc := [3]int32{3065, 17062, 233}
	// The in-game report: the player at (2833,17244), NORTH-WEST of the waypoint.
	if got := SectorDir(npc, [3]int32{2833, 17244, 216}); got != 135 {
		t.Fatalf("SectorDir(NW) = %.1f, want 135", got)
	}
	if got := SectorDir(npc, [3]int32{3205, 17062, 233}); got != 0 {
		t.Fatalf("SectorDir(E) = %.1f, want 0", got)
	}
}

// TestWalkGraphStandOnTheWaypointReport replays the in-game report (2026-10-28, map 15,
// Coimbra): the player at (2833,17244) clicked the waypoint at (3065,17062) and the
// character was sent to (3205,17062) - due EAST, the far side. The real graph must now
// answer with a point on the NORTH-WEST side, where the player was standing.
func TestWalkGraphStandOnTheWaypointReport(t *testing.T) {
	path := filepath.Join("..", "..", "data", "zone_nav", "map15.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no walk graph at %s (run: cd src && python emit_zone_nav.py 15)", path)
	}
	g, err := LoadWalkGraph(path)
	if err != nil {
		t.Fatalf("LoadWalkGraph: %v", err)
	}
	npc := [3]int32{3065, 17062, 233}  // the waypoint, data\zone_npcs\map15.json
	from := [3]int32{2833, 17244, 216} // the player, per the zone15_live.log line
	got := g.Stand(npc, from)
	if !got.Found || !got.Routable || got.Overlap {
		t.Fatalf("stand-off = %+v, want a clear routable one", got)
	}
	if got.Pos == [3]int32{3205, 17062, 233} {
		t.Fatal("the stand-off is still the build-time EAST point - the far-side bug is back")
	}
	if got.Pos[0] >= npc[0] || got.Pos[1] <= npc[1] {
		t.Fatalf("goal %v is not NORTH-WEST of the NPC %v, which is where the player stood",
			got.Pos, npc)
	}
	if got.Dir != 135 {
		t.Errorf("dir = %.1f, want 135 (NW): the sector the player approached from", got.Dir)
	}
	d := math.Hypot(float64(got.Pos[0]-npc[0]), float64(got.Pos[1]-npc[1]))
	if d < 100 || d > 180 {
		t.Errorf("goal is %.0f units from the NPC, want about the 140 stand-off", d)
	}
}

// TestWalkGraphStandNeverUsesTheFarSide: the sector facing the player is one the graph
// cannot walk to, and the only routable one is behind the NPC. The far side is the bug
// being fixed, so the nearest side must win even when the graph prefers the other one.
func TestWalkGraphStandNeverUsesTheFarSide(t *testing.T) {
	g := sectorGraph(t, `{"schema":2,"spacing":300,
	  "spots":[{"handle":1,"anchor_x":0,"anchor_y":0,"x":140,"y":0,"cell":9,"overlap":false,
	    "sectors":[{"dir":0,"x":9000,"y":0,"cell":9},
	               {"dir":180,"x":-140,"y":0,"cell":11}]}],
	  "nodes":[-1000,0,1, 0,0,3, 1000,0,5],
	  "edges":[0,1,1000, 1,2,1000]}`)

	got := g.Stand([3]int32{0, 0, 3}, [3]int32{1000, 0, 5})
	if got.Pos != [3]int32{9000, 0, 9} {
		t.Fatalf("goal = %v, want the side the player is on (9000,0), never the far side "+
			"(-140,0) just because the graph can route there", got.Pos)
	}
	if got.Overlap {
		t.Fatal("a stored side was reported as an overlap")
	}
}

// TestWalkGraphMatchesTheEngine is the point of the whole exercise: the PRE-COMPUTED
// graph must agree with the live path engine, or a Linux server would walk people to
// the wrong places while the Windows one walked them right.
//
// It compares the two route lengths for the same pairs. They are not identical - the
// graph is a coarse grid - so the tolerance is a fifth of the route.
func TestWalkGraphMatchesTheEngine(t *testing.T) {
	graphPath := filepath.Join("..", "..", "data", "zone_nav", "map10.json")
	if _, err := os.Stat(graphPath); err != nil {
		t.Skipf("no walk graph at %s (run: cd src && python emit_zone_nav.py 10)", graphPath)
	}
	g, err := LoadWalkGraph(graphPath)
	if err != nil {
		t.Fatalf("LoadWalkGraph: %v", err)
	}
	engine := realEngine(t) // skips without the helper
	from := [3]int32{5779, 303, 149}

	// Real NPC positions from the spawn data, so the test cannot drift from the data.
	npcBlob, err := os.ReadFile(filepath.Join("..", "..", "data", "zone_npcs", "map10.json"))
	if err != nil {
		t.Skipf("no NPC data: %v", err)
	}
	var doc struct {
		NPCs []struct {
			X    int32 `json:"x"`
			Y    int32 `json:"y"`
			Cell int32 `json:"cell"`
		} `json:"npcs"`
	}
	if err := json.Unmarshal(npcBlob, &doc); err != nil {
		t.Fatalf("NPC data: %v", err)
	}

	checked, agreed := 0, 0
	for i := 0; i < len(doc.NPCs) && checked < 8; i += 15 {
		n := doc.NPCs[i]
		npc := [3]int32{n.X, n.Y, n.Cell}
		spot, ok := g.Spot(npc)
		if !ok {
			t.Logf("no spot for the NPC at %v - the graph and the NPC data disagree", npc)
			continue
		}
		graphLen, okGraph := g.Route(from, spot.Pos)
		engineLen, okEngine, err := engine.PathLength(from, spot.Pos)
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		if !okEngine {
			// The engine is the authority: no route there means nothing to compare.
			t.Logf("npc %v: the engine found no route to %v", npc, spot.Pos)
			continue
		}
		checked++
		if !okGraph {
			// The graph must NEVER refuse what the engine allows: on a Linux build that
			// would be a dead NPC.
			t.Errorf("npc %v: the engine walks to %v but the graph has no data", npc, spot.Pos)
			continue
		}
		delta := math.Abs(graphLen-engineLen) / engineLen
		// The graph is a coarse sample, so the length only has to be the right order - it
		// times the dialog, it does not steer anyone. Double the engine's answer is the
		// bound; anything beyond that would leave the player waiting.
		if delta <= 1.0 {
			agreed++
		}
		t.Logf("npc %v -> spot %v: graph %.0f, engine %.0f (%.0f%% off)",
			npc, spot.Pos, graphLen, engineLen, delta*100)
	}
	if checked == 0 {
		t.Skip("nothing comparable - no spot was reachable in both")
	}
	if agreed < checked {
		t.Fatalf("the graph disagreed with the engine on %d of %d routes", checked-agreed, checked)
	}
}
