package zone

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// WalkGraph is a map's PRE-COMPUTED walk data: where to stand for each NPC, and a
// graph of walkable points so a route can be measured without the path engine.
//
// It exists so the server can serve a map on a machine with no `pathtool.exe` (the
// Linux target) and get the same answers as the Windows build, because the answers are
// geometry - fixed in the map - and are computed once by `src\emit_zone_nav.py`.
//
// What it is NOT: a port of PathEngine. It stores the ANSWERS (a stand-off point with
// its real cell, and legs the engine confirmed are walkable), which is all the chase
// needs. The client still does its own pathing.
type WalkGraph struct {
	Mesh    string
	Spacing int
	Nodes   [][3]int32
	Adj     [][]graphEdge
	spots   map[[2]int32]WalkSpot
}

// graphEdge is one adjacency entry. Its length is the ROUTE's, not the straight
// distance: a leg that needs a detour is a long edge, never a missing one.
type graphEdge struct {
	to     int
	length float64
}

// WalkSpot is where the character should stand to talk to one NPC.
type WalkSpot struct {
	Pos [3]int32

	// Overlap is true when no clear stand-off existed and the destination is the NPC's
	// own position - the client stops at collision distance, so the character touches
	// the NPC. That is the live server's behaviour for those NPCs, not a defect.
	Overlap bool

	// Sectors is one clear stand-off per compass direction, built by the emitter. Empty
	// for a graph written before schema 2, and then Spot's single point is all there is.
	Sectors []Sector
}

// Sector is one clear stand-off point and the direction it sits in, measured from the
// NPC: dir 0 is +x, 90 is +y, 22.5 apart, the same convention the emitter used.
type Sector struct {
	Dir float64
	Pos [3]int32
}

// The file's shape. Flat arrays keep a town's graph to tens of kilobytes.
type walkGraphFile struct {
	Mesh      string `json:"mesh"`
	Spacing   int    `json:"spacing"`
	NPCCount  int    `json:"npc_count"`
	NodeCount int    `json:"node_count"`
	Spots     []struct {
		Handle  uint32 `json:"handle"`
		AnchorX int32  `json:"anchor_x"`
		AnchorY int32  `json:"anchor_y"`
		X       int32  `json:"x"`
		Y       int32  `json:"y"`
		Cell    int32  `json:"cell"`
		Overlap bool   `json:"overlap"`
		Sectors []struct {
			Dir  float64 `json:"dir"`
			X    int32   `json:"x"`
			Y    int32   `json:"y"`
			Cell int32   `json:"cell"`
		} `json:"sectors"` // schema 2; absent in an older file
	} `json:"spots"`
	Nodes []int32 `json:"nodes"` // x, y, cell triples
	Edges []int32 `json:"edges"` // a, b, length triples, undirected
}

// LoadWalkGraph reads one map's walk data.
//
// A missing file is not an error here by itself - a map with no graph falls back to a
// straight line - but the caller should say so once, because "no graph" and "graph
// empty" look the same in game.
func LoadWalkGraph(path string) (*WalkGraph, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc walkGraphFile
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Nodes)%3 != 0 || len(doc.Edges)%3 != 0 {
		return nil, fmt.Errorf("%s: nodes/edges are not flat triples", path)
	}

	g := &WalkGraph{
		Mesh:    doc.Mesh,
		Spacing: doc.Spacing,
		Nodes:   make([][3]int32, len(doc.Nodes)/3),
		Adj:     make([][]graphEdge, len(doc.Nodes)/3),
		spots:   make(map[[2]int32]WalkSpot, len(doc.Spots)),
	}
	for i := range g.Nodes {
		g.Nodes[i] = [3]int32{doc.Nodes[i*3], doc.Nodes[i*3+1], doc.Nodes[i*3+2]}
	}
	for i := 0; i+2 < len(doc.Edges); i += 3 {
		a, b := int(doc.Edges[i]), int(doc.Edges[i+1])
		length := float64(doc.Edges[i+2])
		if a < 0 || b < 0 || a >= len(g.Nodes) || b >= len(g.Nodes) {
			return nil, fmt.Errorf("%s: edge %d-%d is out of range", path, a, b)
		}
		g.Adj[a] = append(g.Adj[a], graphEdge{to: b, length: length})
		g.Adj[b] = append(g.Adj[b], graphEdge{to: a, length: length})
	}
	for _, s := range doc.Spots {
		spot := WalkSpot{
			Pos:     [3]int32{s.X, s.Y, s.Cell},
			Overlap: s.Overlap,
		}
		for _, sec := range s.Sectors {
			spot.Sectors = append(spot.Sectors, Sector{
				Dir: sec.Dir,
				Pos: [3]int32{sec.X, sec.Y, sec.Cell},
			})
		}
		g.spots[[2]int32{s.AnchorX, s.AnchorY}] = spot
	}
	return g, nil
}

// NPCCount is how many NPCs the graph has a stand-off for.
func (g *WalkGraph) NPCCount() int {
	if g == nil {
		return 0
	}
	return len(g.spots)
}

// Spot answers where to stand for the NPC standing at (x, y).
//
// The lookup is by the NPC's OWN position, which is what the zone already has from the
// NPC data file, so no handle has to be threaded through.
func (g *WalkGraph) Spot(npc [3]int32) (WalkSpot, bool) {
	if g == nil {
		return WalkSpot{}, false
	}
	spot, ok := g.spots[[2]int32{npc[0], npc[1]}]
	return spot, ok
}

// StandSpot is Stand's answer.
type StandSpot struct {
	Pos     [3]int32
	Overlap bool

	// Dir is the sector used, in degrees from the NPC (-1 when no sector decided it).
	Dir float64

	// Route is the walk length from the approach point, 0 when it was not measured.
	Route float64

	// Routable is false when the graph cannot judge the PLAYER's own ground - the same
	// refusal the single-spot lookup gave, so the chase sends nothing.
	Routable bool

	// Found is false when this NPC has no data at all.
	Found bool
}

// Stand answers where to stand for the NPC at npc when the player is coming from
// `from`: the side the PLAYER is on, not a side fixed at build time.
//
// A single stored stand-off put the character past the NPC and on its far side, because
// the emitter had no player to aim at when it ran (R58, history\zone-entry.md). The
// sectors it now stores are the clear points on every side; this picks the one whose
// direction is closest to the direction NPC -> from, and prefers one the graph can
// actually walk to within the half-circle facing the player.
//
// Two fallbacks, both of them the previous behaviour:
//   - no sector data (an older graph file, or an NPC with no clear side at all - that is
//     the `overlap` spot) -> exactly Spot's single answer, which is the NPC's own
//     position with Overlap true when an obstacle is pressed against it;
//   - the graph has no data for the NPC -> Found false, and the caller answers with the
//     NPC's own position.
func (g *WalkGraph) Stand(npc, from [3]int32) StandSpot {
	if g == nil {
		return StandSpot{}
	}
	spot, ok := g.Spot(npc)
	if !ok {
		return StandSpot{}
	}
	if len(spot.Sectors) == 0 {
		length, routable := g.Route(from, spot.Pos)
		return StandSpot{Pos: spot.Pos, Overlap: spot.Overlap, Dir: -1,
			Route: length, Routable: routable, Found: true}
	}

	want := bearing(npc, from)
	ordered := sectorsByCloseness(spot.Sectors, want)

	// First choice: a sector on the side the player is coming from that the graph can
	// actually walk to - the answer this side is surest of. The search is capped to the
	// half-circle facing the player, because a routable point on the FAR side is exactly
	// the bug this lookup exists to remove.
	for _, sec := range ordered {
		if angleBetween(sec.Dir, want) > standSectorArc {
			break // ordered by closeness: everything left is on the far side
		}
		if g.linked(from, sec.Pos) {
			length, _ := g.Route(from, sec.Pos)
			return StandSpot{Pos: sec.Pos, Dir: sec.Dir, Route: length, Routable: true,
				Found: true}
		}
	}
	// Second: the nearest-facing side, whether or not the graph's SAMPLE covers it. A
	// point the emitter got from the path engine carries a real cell - the character can
	// stand there - and a hole in the sample is not a wall (Route's own contract). On a
	// map like Coimbra the grid reaches nothing near some NPCs, and refusing those sides
	// would put the character back on the stored far-side point.
	sec := ordered[0]
	length, _ := g.Route(from, sec.Pos)
	return StandSpot{Pos: sec.Pos, Dir: sec.Dir, Route: length, Routable: true, Found: true}
}

// standSectorArc is how far round the NPC the routable-search may look: a quarter circle
// either way still faces the player.
const standSectorArc = 90.0

// onGround reports whether p sits within two grid steps of a sampled node, which is how
// far Route trusts itself to speak about a point.
func (g *WalkGraph) onGround(p [3]int32) bool {
	if g == nil || len(g.Nodes) == 0 {
		return false
	}
	n := g.nearest(p)
	return n >= 0 && g.dist(p, g.Nodes[n]) <= float64(g.Spacing)*2
}

// linked reports whether the graph can walk from -> to over stored legs: both ends on the
// ground, and A* finding a chain between them.
//
// This is deliberately NOT Route's answer: Route returns the straight distance for a
// point beyond the grid (right for a length, wrong for "can the character stand there"),
// so a stand-off chosen on Route alone would accept a point nobody sampled.
func (g *WalkGraph) linked(from, to [3]int32) bool {
	if !g.onGround(from) || !g.onGround(to) {
		return false
	}
	start, goal := g.nearest(from), g.nearest(to)
	if start == goal {
		return true
	}
	_, found := g.astar(start, goal)
	return found
}

// SectorDir is the 22.5-degree sector a point sits in, measured from the NPC, using the
// emitter's convention (0 = +x, 90 = +y). It is what the chase logs, so the next in-game
// test can be read straight out of the log.
func SectorDir(npc, p [3]int32) float64 {
	return math.Mod(math.Round(bearing(npc, p)/22.5)*22.5, 360)
}

// bearing is the direction from a to b in degrees, 0 = +x and 90 = +y - the convention
// the emitter stores in each sector's `dir`.
func bearing(a, b [3]int32) float64 {
	deg := math.Atan2(float64(b[1]-a[1]), float64(b[0]-a[0])) * 180 / math.Pi
	if deg < 0 {
		deg += 360
	}
	return deg
}

// sectorsByCloseness returns a copy of sectors ordered by how near each direction is to
// want, so the side the player is on is tried first and then its neighbours outwards.
func sectorsByCloseness(sectors []Sector, want float64) []Sector {
	ordered := make([]Sector, len(sectors))
	copy(ordered, sectors)
	sort.SliceStable(ordered, func(i, j int) bool {
		return angleBetween(ordered[i].Dir, want) < angleBetween(ordered[j].Dir, want)
	})
	return ordered
}

// angleBetween is the smaller of the two ways round the circle: 350 and 10 are 20 apart.
func angleBetween(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

// Route measures the walk from -> to along the graph, in world units.
//
// ok=false is reserved for "this graph has no data near one of the points". A pair of
// nodes with NO path between them is NOT a refusal: the graph is a SAMPLE of the ground,
// so a missing edge means a gap in the sampling - measured 2026-10-25, four town NPCs
// were cut off from the rest only because no node had been sampled between their cluster
// and the plaza, while the engine walked to them happily. Absence of an edge therefore
// falls back to the straight distance, and only the engine may say "no route".
func (g *WalkGraph) Route(from, to [3]int32) (length float64, ok bool) {
	if g == nil || len(g.Nodes) == 0 {
		return 0, false
	}
	start := g.nearest(from)
	goal := g.nearest(to)

	// A point beyond the grid's reach cannot be judged by this graph.
	if start < 0 || goal < 0 || g.dist(from, g.Nodes[start]) > float64(g.Spacing)*2 ||
		g.dist(to, g.Nodes[goal]) > float64(g.Spacing)*2 {
		return g.dist(from, to), true
	}

	walked, found := g.astar(start, goal)
	if !found {
		// A hole in the SAMPLE, not a wall: the straight line is the best this graph can
		// say, and the client will path it itself.
		return g.dist(from, to), true
	}
	// The two ends outside the grid are straight by construction.
	return walked + g.dist(from, g.Nodes[start]) + g.dist(g.Nodes[goal], to), true
}

// nearest is the closest node to p, or -1 when the graph is empty.
func (g *WalkGraph) nearest(p [3]int32) int {
	best, bestDist := -1, math.Inf(1)
	for i, n := range g.Nodes {
		d := g.dist(p, n)
		if d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

func (g *WalkGraph) dist(a, b [3]int32) float64 {
	return math.Hypot(float64(a[0]-b[0]), float64(a[1]-b[1]))
}

// astar is the route search. The graph is small (a town is ~1,300 nodes) and every
// weight is a real distance, so A* with a straight-line heuristic is both exact and
// instant.
func (g *WalkGraph) astar(start, goal int) (float64, bool) {
	gScore := make(map[int]float64, len(g.Nodes))
	open := &nodeQueue{}
	heap.Init(open)

	gScore[start] = 0
	heap.Push(open, queueItem{node: start, f: g.dist(g.Nodes[start], g.Nodes[goal])})

	for open.Len() > 0 {
		cur := heap.Pop(open).(queueItem)
		if cur.node == goal {
			return gScore[goal], true
		}
		for _, next := range g.Adj[cur.node] {
			tentative := gScore[cur.node] + next.length
			if known, ok := gScore[next.to]; ok && tentative >= known {
				continue
			}
			gScore[next.to] = tentative
			heap.Push(open, queueItem{node: next.to,
				f: tentative + g.dist(g.Nodes[next.to], g.Nodes[goal])})
		}
	}
	return 0, false
}

type queueItem struct {
	node int
	f    float64
}

type nodeQueue []queueItem

func (q nodeQueue) Len() int            { return len(q) }
func (q nodeQueue) Less(i, j int) bool  { return q[i].f < q[j].f }
func (q nodeQueue) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *nodeQueue) Push(x interface{}) { *q = append(*q, x.(queueItem)) }
func (q *nodeQueue) Pop() interface{} {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}
