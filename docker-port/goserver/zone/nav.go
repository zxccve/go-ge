package zone

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NAVIGATION: the route a click on an NPC should walk.
//
// The client treats ZC_MOVE_PC as a PATH move (`CActor::MoveTo` sets `CMS_PATH_MOVE`),
// so a destination its own engine cannot route animates NOTHING - the actor just
// stands there. And a long leg with a wall across it leaves the character shuffling
// against the wall. Both were measured on the Python oracle, which therefore
// path-checks every chase before sending it.
//
// The engine is the game's own: `navmesh\pathtool.exe`, a 32-bit Windows helper
// around `pathengine.dll`. It answers two questions this side needs:
//
//   - **clamp**: the last unobstructed spot along a line (`GetValidPos`). Used to
//     turn "140 units from the NPC towards the player" into a point with a REAL
//     navmesh cell - the cell is a triangle index and a wrong one drops the actor
//     through the floor.
//   - **path**: the shortest walkable polyline from A to B, WITH a cell per corner.
//     Its length is also the honest arrival time, and its corners are the legs the
//     client must be handed one at a time.
//
// WHY THIS IS A SEPARATE, OPTIONAL PIECE: the helper is Windows only, and the target
// is a Linux VPS. A zone started without a path engine keeps the older behaviour (a
// straight leg to the NPC's own spot, capped at MaxChase) and says so. The portable
// answer - a per-map walk graph in JSON, built the way the NPC data already is - is
// the next step, not a rewrite of this one.

// THE STAND-OFF IS THE TARGET'S OWN SHAPE PLUS THE PURPOSE'S OFFSET (piece 2d).
//
// One number for everything was wrong, and the reference shows why. Each of its approach
// states asks the TARGET for a point outside the target's own collision body:
//
//	dialog    target->GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_CHAR)      = 100
//	          source/geServer/ZServerLib/PlayerChar_FSM.cpp:355
//	trigger   target->GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_CHAR-10)   = 90
//	          PlayerChar_FSM.cpp:568
//	pick item target->GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)    = 15
//	          PlayerChar_FSM.cpp:486
//	attack    GetAttackMinPos / GetAttackMaxPos - already implemented, and already taken
//	          from the target's row (zone\attack_approach.go)
//
// and `GetDomainOuterPos` puts the point at `target radius + offset` from the target's
// CENTRE (FieldObject.cpp:84-104, FieldChar.cpp:594-616). DISTANCE_TO_CHAR and
// DISTANCE_TO_ITEM are the client's own two numbers, `#define`d beside the class:
// DISTANCE_TO_ITEM 30, DISTANCE_TO_CHAR 100 (source/geServer/ZServerLib/FieldObject.h:12-13).
//
// The near-tests that fire the ACTION are the same kind of number, and they are wider
// than the stand-off on purpose: `IsNearObject(target, DISTANCE_TO_CHAR*2)` (200 for the
// dialog, PlayerChar_FSM.cpp:353) measures edge to edge - the centre distance minus BOTH
// bodies' radii (FieldObject.cpp:76-82).
const (
	// DistanceToChar and DistanceToItem are the client's own two interaction ranges.
	DistanceToChar = 100.0
	DistanceToItem = 30.0

	// DialogNear is the dialog state's own near-test, edge to edge: IsNearObject folds both
	// 50-unit S radii into the number it is handed, and the reference hands it
	// m_npcClickDistance = DISTANCE_TO_CHAR = 100, so the gate is 200 between CENTRES -
	// NOT 300. It was 2x here until 2026-11-09, when the owner played it: "the dialog is
	// now too early and sometimes overlap". The extra 100 units is the gap he saw.
	DialogNear = DistanceToChar
)

// NPCDefaultRadius is the collision radius this side gives a town NPC, because the
// emitted NPC data carries no size at all (data\zone_npcs\*.json: handle, x, y, cell,
// direction, npc_type, gen_id, name_token, class_type, script). 50 is the reference's
// S body (geShared\Source\ShapeTable.cpp:47-51), which is the same body the old
// `StandoffDistance = 140` assumed when it wrote "50 for an S body".
const NPCDefaultRadius = 50.0

// DialogStandoff is where a click's walk ends: DISTANCE_TO_CHAR outside the NPC's own
// collision body, i.e. `radius + 100` from its centre.
func DialogStandoff(targetRadius float64) float64 { return targetRadius + DistanceToChar }

// PathEngine runs the shipped helper.
//
// ONE PROCESS, MANY QUESTIONS (2026-11-02). The helper's whole cost is its STARTUP:
// loading the .tok mesh and generating the collision/pathfind preprocess. Measured on
// this machine against `navmesh\mesh\fld_auch.tok`, twenty `path` commands through
// TWENTY separate processes took 1.015 s, while ONE process fed the same twenty
// commands answered in 0.053 s - so a spawn is ~51 ms and each further `path` is 1-3 ms.
// The zone plans a route for every client move report (move.go, clientRoute), and that
// planning sits in front of the echo packet the client is waiting for; at one spawn per
// leg it put 50-120 ms there for nothing. The reference's own engine is IN-PROCESS
// (history\movement.md, "How the reference walks to a THING"), so a slow answer here is
// OUR cost and not the design's.
//
// So the engine keeps ONE helper alive: it is started on the first command, its
// stdin/stdout stay open, and every command is written, answered and matched before the
// next one goes out (mu). All of them go through run(): `path` for Plan / Route /
// PathLength, `valid` for Height, `near` for Near, and `clamp` for Plan's stand-off.
//
// A helper that dies or stops answering is retired ONCE and logged, and the NEXT command
// starts a fresh one; the call that found it broken gets the failure it already had,
// which every caller already reads as "no route" / "no height". There is no retry loop,
// because a helper that cannot start must not be spawned once per command.
//
// ONE LIFECYCLE TRAP, now that the process is long-lived: pathtool generates the
// preprocess ONCE PER PROCESS and REFUSES a second radius in the same run
// (pathtool.cpp:80-95, "preprocess-already-generated-for-radius"). Spawning per command
// hid that. This engine therefore always asks with the ONE radius from radius(), and any
// future caller that wants a second radius needs a second engine - not a second radius
// on this one.
//
// The zero value is unusable; see Available. Close it when the zone stops.
type PathEngine struct {
	Tool string // navmesh\pathtool.exe - the helper FILE, and what Available stats

	// Launcher is an optional program that runs Tool (e.g. `wine` on Linux).
	// Empty = run Tool directly, which is every pre-config call. Args are the
	// launcher's arguments between Tool and the mesh, so the full line is:
	//
	//	Launcher Tool Args... <mesh>
	//
	// They are DATA (server.json engine.command + engine.args), never a
	// runtime.GOOS branch: the operator writes the platform's own line.
	Launcher string
	Args     []string

	Mesh    string // the map's .tok
	Radius  int    // the actor's half-size from the shape table (S = 50)
	Timeout int    // seconds, 0 = 5

	// mu serialises commands, and it is what matches an answer to its command: a
	// second command is never written while the first one's answer is being read.
	mu   sync.Mutex
	proc *navWorker // nil until the first command, and after a failure

	// seenCell holds the (client cell > mesh cell) pairs already reported, so the
	// adoption line is once per pair and not once per leg (cellAt).
	seenCell sync.Map
}

// Available reports whether both files are there. A missing helper is NOT an error
// at startup: a zone without it still plays, it just walks straighter.
func (p *PathEngine) Available() bool {
	if p == nil || p.Tool == "" || p.Mesh == "" {
		return false
	}
	if _, err := os.Stat(p.Tool); err != nil {
		return false
	}
	if _, err := os.Stat(p.Mesh); err != nil {
		return false
	}
	return true
}

// Plan answers the live server's OWN decision: where to stand, and how long the walk
// to that point should take.
//
//	ideal   = `standoff` units out from the NPC, on the player's side (the caller
//	           passes the purpose's own number: zone\nav.go, DialogStandoff)
//	clamp   = walk that line BACK towards the NPC until the point is clear
//	free    -> the clamped point is the destination            (the normal gap)
//	hit     -> the ideal ray is BLOCKED: the destination is the NPC's OWN position,
//	           `overlap` is true, and the route is the engine's path to it. The client
//	           walks at the body and stops at collision distance (the agreed clamp rule,
//	           2026-11-09 16:25: measured 15:56 - arrived, gap -100, box opened). This
//	           is a WALK, not a refusal: the click path and the attack approach both
//	           take it, and the attack's hit fires on the first tick inside the band.
//	start invalid -> ok=false: the player is not on valid ground, and the live sends
//	           nothing at all in that case
//
// routeLen is the walkable route's length, used only for the CLOCK. The packet carries
// the destination alone - the client paths it - so a missing route is not a reason to
// refuse the click, just a reason to time it on the straight distance.
//
// route is the route ITSELF, and it is handed back for one reason: the body must walk
// the way it was timed (R110, move.go walkedRouteAt). The clock already counted these
// corners; before this the geometry was the straight chord between the two ends, so a
// bent walk timed itself on the polyline and then drew the body on a line the body
// never walked - the owner's log shows 539 units between the two
// (logs\go_zone1331_20261101_night.log, 2026/11/02 00:00:59.521). Its first point is
// `from` and its last is `goal`, exactly as pathtool answered.
//
// A route that could not be found is nil with a 0 length, which the callers read as
// "time it on the straight distance" and "walk it straight", and nothing else changes.
func (p *PathEngine) Plan(from, npc [3]int32, standoff float64) (goal [3]int32, route [][3]int32, routeLen float64, overlap bool, ok bool, err error) {
	// THE MESH OWNS THE CELL (2026-11-09). Both ends are resolved before anything is
	// asked: pathtool refuses a point whose cell is not the triangle at its (x, y), and
	// that refusal used to become a straight chord. x and y are kept exactly as given.
	if resolved, okFrom := p.cellAt(from); okFrom {
		from = resolved
	} else {
		return npc, nil, 0, true, false, nil
	}
	// The TARGET end must be ground: cellAt's 50-unit guard means a point with no meshed
	// surface within it is nowhere to walk to, and that IS a refusal (TestPathEngineNoRoute-
	// OnBadPoint: 900k units off the map). A real NPC always resolves.
	if resolved, okNpc := p.cellAt(npc); okNpc {
		npc = resolved
	} else {
		return npc, nil, 0, true, false, nil
	}

	ideal := pointTowards(npc, from, standoff)
	clamped, hit, valid, cerr := p.clamp(npc, ideal)
	if cerr != nil {
		return npc, nil, 0, true, false, cerr
	}
	if valid && !hit {
		// THE IDEAL RAY IS CLEAR: use it, as the reference does.
		if route, rerr := p.path(from, clamped); rerr == nil && len(route) >= 2 {
			return clamped, route, polylineLength(route), false, true, nil
		}
	}

	// THE AGREED CLAMP RULE (2026-11-09 16:25, owner: "we agree on the clamp"). A clamp
	// hit means the ideal stand-off is blocked, and the clamp's edge point - where a body
	// TOUCHES the NPC - is refused by the client: it stands still, our clock walks the body
	// on paper, and the box opens "a few moments later" with nothing having moved
	// (logs\zone_go_10.log 16:21-16:22). The destination is therefore the NPC's OWN
	// position: the client walks at it and stops at collision distance (measured 15:56:
	// arrived, gap -100, box opened). NO ring sweep and no other bearing - that was tried
	// and the owner rejected it the same evening ("i thought we agree on the clamp?").
	goal, overlap = npc, true
	route, rerr := p.path(from, goal)
	if rerr != nil || len(route) < 2 {
		// A ROUTE THAT CANNOT BE FOUND IS NOT A REFUSAL (RESTORED 2026-11-09). The
		// engine-only rework (76c20070) turned this into ok=false, and the owner's verdict
		// on it is plain: "check git history before the pathengine-only rework - that is
		// the working state". A crowded NPC has no route to its stand-off, and refusing the
		// click there is what made the NPCs unclickable ("cant path at all now"). The walk
		// goes out and is timed on the straight line, and noteChordWalk calls that out.
		return goal, nil, 0, overlap, true, nil
	}
	return goal, route, polylineLength(route), overlap, true, nil
}

// Route is the walkable polyline between two arbitrary points, with a real cell per
// corner: `Plan` without the stand-off question, for a leg the CLIENT named.
//
// This is what gives a player's own walk a route (move.go, Handler.clientRoute): the
// destination in CZ_MOVE_PC is the client's, so the only question the engine can answer
// is "how does the ground go between here and there". It is the same `path` command Plan
// uses on the same mesh, so a client's leg and a server's walk can never disagree about
// the geometry of a map.
//
// ok=false means "walk the straight line" - no route between the two points, or a route
// with no room to bend. A broken helper is NOT logged here: this runs once per leg, and a
// log line per leg would bury the session; the engine's own availability is announced once
// at startup (main.go, pathEngine).
func (p *PathEngine) Route(from, to [3]int32) ([][3]int32, bool) {
	if !p.Available() {
		return nil, false
	}
	// THE MESH OWNS THE CELL (2026-11-09): both ends are resolved before the path is
	// asked for, and x/y are kept exactly as the client named them.
	var okFrom, okTo bool
	if from, okFrom = p.cellAt(from); !okFrom {
		return nil, false
	}
	if to, okTo = p.cellAt(to); !okTo {
		return nil, false
	}
	route, err := p.path(from, to)
	if err != nil || len(route) < 2 {
		return nil, false
	}
	return route, true
}

// PathLength measures a route the engine would walk, in world units. ok=false means the
// engine found no route at all. Exported so a test can check the PRE-COMPUTED walk graph
// against the engine's own answer.
func (p *PathEngine) PathLength(from, to [3]int32) (float64, bool, error) {
	route, err := p.path(from, to)
	if err != nil {
		return 0, false, err
	}
	if len(route) < 2 {
		return 0, false, nil
	}
	return polylineLength(route), true, nil
}

// Height answers the world HEIGHT of a position on the mesh.
//
// This is the game's own `CWorld::GetHeight(pos)`, and it exists because a POSITION
// and a HEIGHT are two different numbers and the oracle stores both:
//
//	WORLDPOS  { x, y, CELL }   what the client walks on - a navmesh triangle index
//	WORLDPOINT{ x, y, z    }   the same spot as a WORLD HEIGHT
//
// The original converts one into the other at both ends of the Last Point
// (Commander_FSM.cpp:644-650 on the way out, `point.Set(pos.x, pos.y,
// m_world->GetHeight(pos))`; Commander_Load.cpp:575-580 on the way in,
// `Get3DPointToPosLocaly(point, leaderPos, 1000, 300)`). `character_position.ZPOS` is
// the HEIGHT, never the cell - writing the cell there is what put the owner's team
// under the floor on 2026-10-29.
//
// ok=false means the mesh has no valid ground at that (x, y, cell). The caller must
// then write NOTHING: the only other number it has is the cell, and a cell in that
// column is the bug.
func (p *PathEngine) Height(pos [3]int32) (int32, bool, error) {
	out, err := p.run(fmt.Sprintf("valid %d %d %d", pos[0], pos[1], pos[2]), answerSingle)
	if err != nil {
		return 0, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "valid" {
			continue
		}
		// `valid <x> <y> <cell> <ok> <height>` (navmesh\pathtool.cpp:171-179).
		if len(f) < 6 {
			return 0, false, fmt.Errorf("pathtool valid line unreadable: %q", line)
		}
		ok, err := strconv.Atoi(f[4])
		if err != nil {
			return 0, false, fmt.Errorf("pathtool valid line unreadable: %q", line)
		}
		if ok == 0 {
			return 0, false, nil
		}
		h, err := strconv.Atoi(f[5])
		if err != nil {
			return 0, false, fmt.Errorf("pathtool valid line has no height: %q", line)
		}
		return int32(h), true, nil
	}
	return 0, false, fmt.Errorf("pathtool gave no valid answer")
}

// Near is the game's own `Get3DPointToPosLocaly(shape, point, out, hRange, vRange)`:
// the navmesh POSITION nearest a 3D point. It is the ONLY way a stored HEIGHT becomes
// the CELL the client needs, and the ranges are the original's own (1000, 300 - see
// Commander_Load.cpp:580). A point outside them answers ok=false, which the original
// turns into the map's start position; so does the caller here.
//
// It answers the resolved position (x, y, CELL), the height the mesh has THERE, and
// ok. The height is what lets the caller notice a row whose z is really a cell: the
// mesh's own height at that spot is a different number.
func (p *PathEngine) Near(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool, error) {
	out, err := p.run(fmt.Sprintf("near %d %d %d %d %d",
		pt[0], pt[1], pt[2], hRange, vRange), answerSingle)
	if err != nil {
		return [3]int32{}, 0, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "near" {
			continue
		}
		// `near <x> <y> <z> <ok> <px> <py> <cell> <height>` (pathtool.cpp:161-170).
		if len(f) < 9 {
			return [3]int32{}, 0, false, fmt.Errorf("pathtool near line unreadable: %q", line)
		}
		ok, err := strconv.Atoi(f[4])
		if err != nil {
			return [3]int32{}, 0, false, fmt.Errorf("pathtool near line unreadable: %q", line)
		}
		if ok == 0 {
			return [3]int32{}, 0, false, nil
		}
		var n [4]int
		for i := 0; i < 4; i++ {
			n[i], err = strconv.Atoi(f[5+i])
			if err != nil {
				return [3]int32{}, 0, false,
					fmt.Errorf("pathtool near line unreadable: %q", line)
			}
		}
		return [3]int32{int32(n[0]), int32(n[1]), int32(n[2])}, int32(n[3]), true, nil
	}
	return [3]int32{}, 0, false, fmt.Errorf("pathtool gave no near answer")
}

// cellRangeXY / cellRangeZ bound the mesh search that turns a point's (x, y) into the
// triangle the mesh actually has there. THE CLIENT'S CELL IS NOT THE MESH'S CELL
// (2026-11-09): a report or a table row can carry a cell from another index space, and
// pathtool answers `startOk=0` for it - "no route" - which the zone then drew as a
// straight chord. See history\movement.md, "THE CLIENT'S CELL IS NOT THE MESH'S CELL".
// WHY THE WIDE VERTICAL SEARCH IS 20000, NOT 300 (2026-11-10). The third element of a
// position is a CELL, and a MID-WALK one is not even the mesh's cell: walkedRouteAt
// (move.go:566) answers route[i][2] - the triangle of the corner the body is walking
// TOWARD - and walkedToAt (:513) answers dest[2], neither of which is the ground under
// the interpolated x,y. pathtool then refuses `valid x y cell`, which is the state
// cellAt exists to resolve.
//
// The search that resolves it must not be the 300-unit band: that band is the original's
// limit for a point that ALREADY carries a HEIGHT (Commander_Load.cpp:580), and a cell
// index used as one is nowhere near the ground. Measured with the game's own helper:
//
//	pathtool auch_basic.tok near -8856 4111 140 1000 300    -> "no ground"
//	pathtool auch_basic.tok near -8856 4111   0 1000 20000  -> cell 140, height 1385
//	pathtool fld_auch.tok   near -225 1107    0 1000 20000  -> cell 29,  height 25
//
// - map 1301's ground at (-8856,4111) is 1245 units from the 140 in the cell slot, which
// is the same pair main.go's dummyHintVRange comment measured (`-dummycell` had to go to
// 20000 for exactly this reason). One number for every "which triangle is under this
// (x, y)" question, so a new caller cannot pick a band too narrow to answer.
const (
	cellRangeXY    = 1000
	cellRangeZ     = 300   // the near search used when a height is already known
	cellRangeWideZ = 20000 // the near search used when only (x, y) is known
)

// Valid asks whether the mesh accepts this point's own cell at its (x, y), and answers
// the ground height there: `valid <x> <y> <cell>` -> `valid x y cell ok height`.
func (p *PathEngine) Valid(pt [3]int32) (int32, bool, error) {
	out, err := p.run(fmt.Sprintf("valid %d %d %d", pt[0], pt[1], pt[2]), answerSingle)
	if err != nil {
		return 0, false, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] != "valid" {
			continue
		}
		ok, _ := strconv.Atoi(f[4])
		height, _ := strconv.Atoi(f[5])
		return int32(height), ok == 1, nil
	}
	return 0, false, fmt.Errorf("pathtool gave no valid answer")
}

// cellAt answers the point with the MESH's cell for its (x, y), keeping the caller's
// x and y exactly: they are facts (the client walked there, or the table put the NPC
// there), while the cell is the mesh's opinion and the engine only accepts its own.
//
// ok=false means the mesh has no ground within cellRangeXY of the point: the caller
// must refuse, never plan.
func (p *PathEngine) cellAt(pt [3]int32) ([3]int32, bool) {
	if !p.Available() {
		return pt, false
	}
	// 1. THE CHEAP EXACT ANSWER: the mesh accepts the cell the point already carries.
	if _, ok, err := p.Valid(pt); err == nil && ok {
		return pt, true
	}
	// 2. THE CELL IS FOREIGN (or the point is off the mesh). Ask the mesh for the
	// triangle at this (x, y) with a WIDE vertical search from 0 - a map's ground can sit
	// thousands of units below (btl_auch01: -6158) or above. The answer must land within
	// 50 units of the point, or the point is not on this mesh at all.
	at, _, ok, err := p.Near([3]int32{pt[0], pt[1], 0}, cellRangeXY, cellRangeWideZ)
	if err != nil || !ok || flatDist(at, pt) > 50 {
		return pt, false
	}
	if at[2] == pt[2] {
		return pt, true
	}
	// ONE line per (client cell -> mesh cell) per engine: the same disagreement repeats
	// on every leg of a walk, and the count lives in the log line, not per report.
	if _, seen := p.seenCell.LoadOrStore(fmt.Sprintf("%d>%d", pt[2], at[2]), true); !seen {
		log.Printf("zone: cell %d at (%d, %d) is not the mesh's cell %d on %s - the "+
			"mesh's cell is adopted (said once per pair)",
			pt[2], pt[0], pt[1], at[2], p.Mesh)
	}
	return [3]int32{pt[0], pt[1], at[2]}, true
}

// polylineLength is the walked distance of a route.
func polylineLength(route [][3]int32) float64 {
	var total float64
	for i := 1; i < len(route); i++ {
		dx := float64(route[i][0] - route[i-1][0])
		dy := float64(route[i][1] - route[i-1][1])
		total += math.Hypot(dx, dy)
	}
	return total
}

// pointTowards is `standoff` units from base, in the direction of towards.
// The cell is left as the base's, because this is only a REQUEST for the clamp:
// pathtool answers with the real cell of the point it accepts.
func pointTowards(base, towards [3]int32, distance float64) [3]int32 {
	dx := float64(towards[0] - base[0])
	dy := float64(towards[1] - base[1])
	length := math.Hypot(dx, dy)
	if length == 0 {
		return base
	}
	return [3]int32{
		base[0] + int32(dx/length*distance),
		base[1] + int32(dy/length*distance),
		base[2],
	}
}

// clamp asks for the last unobstructed spot from start towards point, and whether the
// clamp was SHORTENED (hit) by something in the way.
func (p *PathEngine) clamp(start, point [3]int32) (out [3]int32, hit bool, valid bool, err error) {
	stdout, err := p.run(fmt.Sprintf("clamp %d %d %d %d %d %d",
		start[0], start[1], start[2], point[0], point[1], p.radius()), answerSingle)
	if err != nil {
		return start, false, false, err
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "clamp none") {
			// The START is not valid ground. Not an error: the caller refuses the
			// click, exactly as the live server sends nothing.
			return start, false, false, nil
		}
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == "clamp" && fields[1] == "ok" {
			x, err1 := strconv.Atoi(fields[2])
			y, err2 := strconv.Atoi(fields[3])
			cell, err3 := strconv.Atoi(fields[4])
			if err1 != nil || err2 != nil || err3 != nil {
				return start, false, false, fmt.Errorf("pathtool clamp line unreadable: %q", line)
			}
			return [3]int32{int32(x), int32(y), int32(cell)}, fields[5] == "hit", true, nil
		}
	}
	return start, false, false, fmt.Errorf("pathtool gave no clamp answer")
}

// path asks for the walkable polyline from -> to.
func (p *PathEngine) path(from, to [3]int32) ([][3]int32, error) {
	out, err := p.run(fmt.Sprintf("path %d %d %d %d %d %d %d",
		from[0], from[1], from[2], to[0], to[1], to[2], p.radius()), answerPath)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "path none"), strings.HasPrefix(line, "path err"):
			return nil, nil // no route: a legitimate answer, not a failure
		case strings.HasPrefix(line, "path ok "):
			// `path ok <n> radius=.. length=..`, then one `pt <i> x y cell` per point.
			fields := strings.Fields(line)
			count, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("pathtool path line unreadable: %q", line)
			}
			points := make([][3]int32, 0, count)
			for j := 1; j <= count && i+j < len(lines); j++ {
				f := strings.Fields(lines[i+j])
				if len(f) < 5 || f[0] != "pt" {
					return nil, fmt.Errorf("pathtool point %d unreadable: %q", j, lines[i+j])
				}
				x, _ := strconv.Atoi(f[2])
				y, _ := strconv.Atoi(f[3])
				cell, _ := strconv.Atoi(f[4])
				points = append(points, [3]int32{int32(x), int32(y), int32(cell)})
			}
			return points, nil
		}
	}
	return nil, fmt.Errorf("pathtool gave no path answer")
}

func (p *PathEngine) radius() int {
	if p.Radius <= 0 {
		return 50 // S body, the shape table's smallest
	}
	return p.Radius
}

// answerKind says how many lines ONE command's answer is.
//
// The helper has NO end-of-answer marker - it prints the answer and flushes - so the
// reader has to know the shape of what it is waiting for. The shapes are the CLI's own
// (navmesh\pathtool.cpp): `valid`, `near`, `clamp`, `faces`, `sections` and `section`
// answer with exactly one line, while `path` answers with ONE header line and then one
// `  pt <i> x y cell` line per point, counted in the header (`path ok <n> ...`).
type answerKind int

const (
	answerSingle answerKind = iota // one line: near, valid, clamp, faces, sections, section
	answerPath                     // `path ok <n> ...` then <n> point lines
)

// navDefaultTimeout is the per-command budget when PathEngine.Timeout is 0. The first
// command also pays the mesh load and preprocess, so this covers both.
const navDefaultTimeout = 5 * time.Second

// navStopGrace is how long Close waits after `quit` before it kills the helper.
const navStopGrace = 2 * time.Second

// errNavTimeout is the one failure that is the HELPER's fault rather than the caller's:
// it was asked something and never answered.
var errNavTimeout = errors.New("pathtool did not answer in time")

// run sends ONE command to the live helper and returns its answer.
//
// The first call starts the process and pays the mesh load (measured ~51 ms on
// fld_auch.tok); every call after it is 1-3 ms. A helper that fails or times out is
// retired here and nowhere else, so a broken tool costs one restart and not a spawn per
// command.
func (p *PathEngine) run(command string, kind answerKind) (string, error) {
	if p == nil || p.Tool == "" || p.Mesh == "" {
		return "", errors.New("pathtool: no path engine configured")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.proc == nil {
		started, err := startNav(p.Tool, p.Launcher, p.Args, p.Mesh)
		if err != nil {
			return "", err
		}
		p.proc = started
	}
	w := p.proc
	out, err := w.ask(command, kind, p.timeout())
	if err == nil {
		return out, nil
	}

	// Broken or stuck: the answer to THIS command is gone, so retire the process, say
	// so once, and hand back the failure the caller already knows how to read ("no
	// route", "no height"). The next command starts a fresh helper - never a retry
	// here, or a helper that cannot start would be spawned once per command.
	p.proc = nil
	w.shutdown(false)
	log.Printf("zone: pathtool on %s: %v - the helper is restarted on the next command",
		p.Mesh, err)
	return "", err
}

// timeout is the per-command budget: PathEngine.Timeout seconds, or the default.
func (p *PathEngine) timeout() time.Duration {
	if p.Timeout <= 0 {
		return navDefaultTimeout
	}
	return time.Duration(p.Timeout) * time.Second
}

// Close stops the helper. A zone serves one map, so it owns exactly one of these.
//
// It sends the CLI's own `quit` and gives it a moment, then kills whatever is left: a
// live zone that exits must not leave pathtool.exe behind. Safe on a NIL engine (that is
// how main.go calls it when this machine has no helper) and safe to call twice.
//
// The engine still WORKS after Close - the next command starts a new process - which is
// what a test needs to prove a closed engine recovers. Nothing in the server relies on
// that: Close is a shutdown call.
func (p *PathEngine) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.proc
	p.proc = nil
	if w == nil {
		return nil
	}
	w.shutdown(true)
	return nil
}

// navWorker is ONE running pathtool process: its pipes, and the goroutine that reads
// its answers.
//
// The reader is per PROCESS, never per command. It is the only reader of stdout, and
// commands are written one at a time under PathEngine.mu, so an answer can never be
// matched to the wrong command.
type navWorker struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string   // one answer line at a time; CLOSED when the reader stops
	done  chan struct{} // closed when the reader has returned
	dead  chan struct{} // closed once, to stop the reader without waiting for EOF
	errs  navStderr     // the helper's own stderr, for a failure message
	read  error         // why the reader stopped; read only after `lines` is closed
	shut  bool          // guards shutdown; both callers hold PathEngine.mu
}

// startNav starts ONE helper and leaves it running. Nothing is written to it yet: the
// mesh load is the child's own startup, and a command written now simply waits in the
// pipe until it is ready (pathtool.cpp reads stdin with fgets once the mesh is loaded).
//
// The command line is `launcher tool args... mesh`. An empty launcher (the Windows
// case, and every pre-config call) runs the tool itself, so the old line is unchanged;
// a non-empty one is how a Linux zone runs the same shipped exe under `wine`, and it
// comes from server.json - this file has no idea which platform it is on.
func startNav(tool, launcher string, args []string, mesh string) (*navWorker, error) {
	name := tool
	argv := append(append([]string(nil), args...), mesh)
	if launcher != "" {
		name = launcher
		argv = append([]string{tool}, argv...)
	}
	cmd := exec.Command(name, argv...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pathtool: %w", err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, fmt.Errorf("pathtool: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		in.Close()
		outPipe.Close()
		return nil, fmt.Errorf("pathtool: %w", err)
	}
	w := &navWorker{
		cmd:   cmd,
		in:    in,
		lines: make(chan string, 64),
		done:  make(chan struct{}),
		dead:  make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		outPipe.Close()
		errPipe.Close()
		return nil, fmt.Errorf("pathtool: %w", err)
	}
	go w.drainStderr(errPipe)
	go w.readLines(outPipe)
	return w, nil
}

// readLines turns the helper's stdout into one line at a time. It is the ONLY reader.
func (w *navWorker) readLines(r io.ReadCloser) {
	defer close(w.done)
	defer close(w.lines)
	defer r.Close()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			select {
			case w.lines <- strings.TrimRight(line, "\r\n"):
			case <-w.dead:
				return
			}
		}
		if err != nil {
			w.read = err
			return
		}
	}
}

// drainStderr keeps the child's stderr pipe empty, so the child can never block on it,
// and keeps the last few lines for the message a failure prints.
func (w *navWorker) drainStderr(r io.ReadCloser) {
	defer r.Close()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			w.errs.write(line)
		}
		if err != nil {
			return
		}
	}
}

// ask writes ONE command and reads its answer back.
func (w *navWorker) ask(command string, kind answerKind, timeout time.Duration) (string, error) {
	if _, err := io.WriteString(w.in, command+"\n"); err != nil {
		return "", fmt.Errorf("pathtool: write: %w (%s)",
			err, strings.TrimSpace(w.errs.String()))
	}
	return w.readAnswer(kind, timeout)
}

// readAnswer reads the lines this answer is made of (answerKind), on ONE budget of
// `timeout` for the whole answer: a helper that stops halfway is as broken as one that
// never starts.
func (w *navWorker) readAnswer(kind answerKind, timeout time.Duration) (string, error) {
	budget := time.NewTimer(timeout)
	defer budget.Stop()

	read := func() (string, error) {
		select {
		case line, ok := <-w.lines:
			if !ok {
				if w.read != nil {
					return "", fmt.Errorf("pathtool: %w (%s)",
						w.read, strings.TrimSpace(w.errs.String()))
				}
				return "", fmt.Errorf("pathtool: the helper closed its output")
			}
			return line, nil
		case <-budget.C:
			return "", fmt.Errorf("pathtool: %w", errNavTimeout)
		}
	}

	first, err := read()
	if err != nil {
		return "", err
	}
	out := []string{first}

	if kind == answerPath && strings.HasPrefix(first, "path ok ") {
		// `path ok <n> radius=.. length=.. points=<n>`, then <n> point lines. The count
		// is in the header, so the answer's END is known without a marker.
		fields := strings.Fields(first)
		if len(fields) < 3 {
			return strings.Join(out, "\n"), nil // left to path()'s own message
		}
		count, cerr := strconv.Atoi(fields[2])
		if cerr != nil {
			return strings.Join(out, "\n"), nil
		}
		for i := 0; i < count; i++ {
			line, rerr := read()
			if rerr != nil {
				return "", rerr
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n"), nil
}

// shutdown ends the helper and REAPS it.
//
// graceful sends the CLI's own `quit` first; anything else kills it. Either way the
// reader is waited for BEFORE Wait(), because Wait() closes the pipes the reader is
// using - and Wait() itself is what stops the process being a zombie.
func (w *navWorker) shutdown(graceful bool) {
	if w.shut {
		return
	}
	w.shut = true
	close(w.dead)
	if w.cmd.Process != nil {
		if graceful {
			io.WriteString(w.in, "quit\n")
		}
		w.in.Close()
		if graceful {
			select {
			case <-w.done:
			case <-time.After(navStopGrace):
				w.cmd.Process.Kill()
			}
		} else {
			w.cmd.Process.Kill()
		}
	} else {
		w.in.Close()
	}
	<-w.done
	w.cmd.Wait()
}

// navStderr keeps the last few lines the helper wrote on stderr. A live zone prints one
// failure message, not a transcript, but the reason has to be IN it: "mesh loaded,
// 881 3D faces" is how a reader tells a bad mesh from a bad path.
type navStderr struct {
	mu  sync.Mutex
	buf []byte
}

func (s *navStderr) write(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, line...)
	if len(s.buf) > 4096 {
		s.buf = s.buf[len(s.buf)-4096:]
	}
}

func (s *navStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}
