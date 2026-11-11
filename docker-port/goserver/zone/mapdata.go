package zone

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// The shared spawn table: docker-port\data\zone_points.json.
//
// ONE file for every map, written by `src\emit_zone_points.py` from the client's own
// tables (warp rows, town points, map waypoints). It exists because the landing point
// belongs to the MAP, not to the server: reading Reboldoeux's numbers on another map
// puts the team through the floor, and the client then dies inside PathEngine.DLL on
// the first walk - the crash this whole file is arranged around.
//
// The file is DERIVED data with a fixed schema. Version 2 adds the arrival FORMATION
// (see FormationSlot) and is still readable as version 1, where no formation means
// "everybody on the point". Version 3 adds the gate's own fields (see MapGate): the
// trigger radius, the arrival point on the TARGET map's mesh, the direction byte and
// the five camera floats. Version 4 adds `waypoints`: the town grid the waypoint NPC
// opens (see WaypointTown). Regenerate it; never hand-edit it. And unlike the item or
// text tables, a missing value here is NOT a warning: a map with no landing cell
// cannot be served at all (see SpawnFor).
//
// The zone reads `spawn`, `points`, their `formation`, `gates` and `waypoints`.
// `restart` is kept because it is the same map's rows and the next step (respawn)
// reads it from here rather than from a second file.

// ZonePointsVersion is the newest schema this loader understands. An OLDER version is
// accepted: version 1 has no formation and version 2 no gate fields, and both must
// behave exactly as they did, not fail. A NEWER one is refused, because guessing at a
// shape is how a cell ends up read out of the wrong field.
const ZonePointsVersion = 4

// ZonePointsVersionMin is the oldest schema this loader still understands: the one
// without formations.
const ZonePointsVersionMin = 1

// ZonePoints is the whole file.
type ZonePoints struct {
	GeneratedBy string `json:"generated_by"`
	Version     int    `json:"version"`
	Built       string `json:"built"`
	Counts      struct {
		Maps      int `json:"maps"`
		WithSpawn int `json:"with_spawn"`
		WithTown  int `json:"with_town"`
		Gates     int `json:"gates"`
	} `json:"counts"`
	ByName map[string]int      `json:"by_name"` // map NAME -> map id
	Maps   map[string]MapEntry `json:"maps"`    // map id (as text) -> the map's rows
	// Waypoints is the TOWN-TELEPORT grid, in the client's own datatable_waypoint
	// row order. Version 4+. Absent (an older file) means "no grid to open", which
	// the waypoint NPC's click reports instead of drawing an empty window.
	Waypoints []WaypointTown `json:"waypoints"`
	Note      string         `json:"note"`
}

// WaypointTown is one town in the waypoint window: a MAP ID, nothing else the
// client needs. It looks the town's name and its thumbnail up in its own
// datatable_map, so a name sent from here would be ignored at best and wrong at
// worst.
//
// The row's own table id and restart warp id are carried for the LOG, not for the
// wire: when this list is wrong, the line that says which row it came from is what
// makes it fixable.
type WaypointTown struct {
	ID            int    `json:"id"`       // datatable_waypoint ClassID
	Location      string `json:"location"` // the zone name, e.g. coim_basic
	MapID         int    `json:"map_id"`   // datatable_map id the client draws
	RestartWarpID int    `json:"restart_warp_id"`
	Tab           int    `json:"tab"` // 0 = town grid, 1 = field grid
}

// MapEntry is one map's rows.
type MapEntry struct {
	Name string `json:"name"`
	Mesh string `json:"mesh"` // the .tok walk-mesh, empty when the map has none
	// Town is a 0/1 flag, NOT a table row id: 1 means town points exist for the map.
	Town    int           `json:"town"`
	Spawn   *MapPoint     `json:"spawn"` // nil when the map has no position at all
	Points  []TownPoint   `json:"points"`
	Restart *RestartPoint `json:"restart"`
	Gates   []MapGate     `json:"gates"`
}

// FormationSlot is one character's place in an arrival formation: slot 0 is the
// LEADER and is the point itself, slots 1.. are the followers, each with its own
// resolved cell.
//
// Cell is a POINTER for the same reason MapPoint's is: the emitter writes null when
// its path engine found no ground at the offset, and a follower must then stand on
// the leader's point rather than be sent a coordinate nobody resolved.
type FormationSlot struct {
	X    int32  `json:"x"`
	Y    int32  `json:"y"`
	Cell *int32 `json:"cell"`
}

// MapPoint is a position with a cell. Cell is a POINTER because the emitter writes
// null when its path engine found no ground near the point: that is a real answer
// ("do not enter this map here"), not a missing number to guess at.
type MapPoint struct {
	X         int32           `json:"x"`
	Y         int32           `json:"y"`
	Cell      *int32          `json:"cell"`
	Source    string          `json:"source"`    // which table the position came from, e.g. townpoint:1
	Formation []FormationSlot `json:"formation"` // versions 2+; empty means "everybody here"
}

// TownPoint is one of a town's arrival points: n=1 is the one the emitter chose as
// the map's spawn.
type TownPoint struct {
	N         int             `json:"n"`
	X         int32           `json:"x"`
	Y         int32           `json:"y"`
	Cell      *int32          `json:"cell"`
	Name      string          `json:"name"`      // a locale token until the text book resolves it
	Formation []FormationSlot `json:"formation"` // versions 2+; empty means "everybody here"
}

// RestartPoint is where the map puts a character who respawns on it.
type RestartPoint struct {
	WarpID int    `json:"warp_id"`
	X      int32  `json:"x"`
	Y      int32  `json:"y"`
	Cell   *int32 `json:"cell"`

	// THE CAMERA of the destination, from the same warp row (2026-11-09). A zone move carries
	// it in MOVE_ZONE_OK, and zeros collapse the client's zoom - the owner saw that after the
	// first save-point move. A row emitted before this change has no values here, and the
	// mover then leaves the camera to the client rather than sending zeros.
	Lat      float32 `json:"lat"`
	Long     float32 `json:"long"`
	ZoomMin  float32 `json:"zoom_min"`
	ZoomMax  float32 `json:"zoom_max"`
	InitZoom float32 `json:"init_zoom"`
}

// MapGate is one doorway out of the map: where the player TRIGGERS it in this map,
// and where they ARRIVE on the map it leads to.
//
// The two sides are different maps, so they are different points on different meshes:
// X/Y/Cell is the trigger here, TargetX/TargetY/TargetCell is the exit there, and
// TargetCell is a triangle index into the TARGET map's own mesh (resolved by the
// emitter). TargetZ is the row's HEIGHT and is never a cell - a height in the cell
// slot drops a character through the map.
//
// Range is this gate's own trigger radius in world units (measured 90..1000, not a
// constant): the original asks IsNearObject(leader, range) on the point
// (WarpPoint.cpp:71), so a gate is a circle, not a coordinate.
type MapGate struct {
	Name string `json:"name"`
	To   string `json:"to"`    // destination map CLASS name, e.g. "fld_coim"
	ToID int    `json:"to_id"` // destination map id; 0 when the row names an unknown map
	X    int32  `json:"x"`
	Y    int32  `json:"y"`
	Cell *int32 `json:"cell"` // the trigger's own cell, not the exit's

	// Range is the trigger radius. 0 means "the table measured none", and then the
	// gate never fires: a guessed radius warps a player who only walked past.
	Range int32 `json:"range"`

	TargetX    int32  `json:"target_x"`
	TargetY    int32  `json:"target_y"`
	TargetCell *int32 `json:"target_cell"` // null = the exit is not on ground: refuse
	TargetZ    int32  `json:"target_z"`    // a HEIGHT hint, never a cell
	// TargetDir is the row value in DEGREES and TargetDirByte is what the packet
	// sends: (BYTE)(deg/360*255 + 0.5) - GetByteAngle(), gePacketBase.h:11, used at
	// Commander.cpp:1213. A plain & 0xFF turns 278 into 22, a silent 256-degree error.
	TargetDir     int32 `json:"target_dir"`
	TargetDirByte uint8 `json:"target_dir_byte"`

	// The five camera floats the client is TOLD to use on arrival, copied straight
	// from the row (WarpPoint.cpp:99-103): latitude, longitude, zoom min, zoom max,
	// initial zoom. float32 because that is what the packet carries.
	Lat      float32 `json:"lat"`
	Long     float32 `json:"long"`
	ZoomMin  float32 `json:"zoom_min"`
	ZoomMax  float32 `json:"zoom_max"`
	InitZoom float32 `json:"init_zoom"`
}

// Gates answers a map's doorways out, in file order. Empty for a map with none (most
// field maps end in a wall) and for a map the table does not carry - the same answer
// on purpose: there is nowhere to go either way.
func (z *ZonePoints) Gates(mapID int) []MapGate {
	if z == nil {
		return nil
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if !ok {
		return nil
	}
	return entry.Gates
}

// Landing is where a team enters one map, once its cell has been checked. It carries
// the map's name and the source of the position so the startup line can say WHERE the
// number came from - "the spawn is odd" is unanswerable without it.
type Landing struct {
	Pos [3]int32 // x, y, CELL (a navmesh triangle index)
	// Formation is where each member stands, slot 0 = the leader (= Pos). Empty on a
	// version-1 file and on the -spawn override, and then everybody stands on Pos -
	// which is what this zone did before the file carried a formation, and what stacked
	// the team on one spot in game.
	Formation []FormationSlot
	MapID     int
	Map       string
	Mesh      string
	Source    string // "townpoint:1", "map:waypoint", or "-spawn" for an override
}

// Slot answers where member `index` of a team stands when it lands on this point.
func (l Landing) Slot(index int) [3]int32 {
	return formationSlot(l.Formation, l.Pos, index)
}

// Slot answers where member `index` of a team stands on this town point.
func (p TownPoint) Slot(index int) [3]int32 {
	return formationSlot(p.Formation, [3]int32{p.X, p.Y, cellOrZero(p.Cell)}, index)
}

// formationAround layers this map's own arrival SHAPE around a point somebody arrives at.
//
// It is THE rule that turns "a bare (x, y, cell) destination" into a team formation, and
// it exists for one reason: a gate's exit, the saved start point, the Last Point and a
// Warp List jump must not each hold their own copy of it. The offsets are the map's own
// (the reference's `CCommander::GetTeamPos` V - leader + (-75,+43) and (-75,-44)), moved
// from the map's spawn onto the destination, exactly as the emitter lays them out.
//
// `cellAt` answers a slot's OWN navmesh cell from its x and y - the archived trap: a
// formation slot's cell is NOT the leader's, and copying it makes the client walk that
// character back to her own triangle on every warp, turning while she walks. NIL means
// "nothing here can resolve a cell" (a version-1 file, a -spawn override, or no path
// engine), and then every slot carries the DESTINATION's own cell - the gate exit's rule.
//
// A slot the mesh REFUSES keeps no cell, and formationSlot then stands that member on the
// point itself. One character beside the leader is the honest answer; refusing the whole
// move because one triangle is missing is not.
func formationAround(shape []FormationSlot, spawn, at [3]int32,
	cellAt func(x, y int32) (int32, bool)) []FormationSlot {
	if len(shape) == 0 || spawn == ([3]int32{}) {
		return nil
	}
	out := make([]FormationSlot, len(shape))
	for i, s := range shape {
		if i == 0 {
			// Slot 0 IS the point, so its cell is the destination's own and it is never
			// asked for twice - the emitter makes the same choice (`_plan_formation`).
			cell := at[2]
			out[i] = FormationSlot{X: at[0], Y: at[1], Cell: &cell}
			continue
		}
		x, y := at[0]+(s.X-spawn[0]), at[1]+(s.Y-spawn[1])
		c := at[2]
		if cellAt != nil {
			own, ok := cellAt(x, y)
			if !ok {
				// No ground under this slot: no cell, and the point's own x/y, which is
				// what the emitter writes for the same case (finish_formations drops the
				// offset rather than inventing a triangle).
				out[i] = FormationSlot{X: at[0], Y: at[1]}
				continue
			}
			c = own
		}
		cell := c
		out[i] = FormationSlot{X: x, Y: y, Cell: &cell}
	}
	return out
}

// formationCellAt builds the cell resolver formationAround takes, for a team arriving at
// `leader` (x, y, CELL) on THIS map's mesh.
//
// WHY THE LEADER'S HEIGHT IS THE QUESTION'S z. A slot's cell is "the ground under that
// x and y", and the mesh only answers a 3D point: `Handler.NavNear` is the game's own
// `Get3DPointToPosLocaly(point, out, 1000, 300)`, the call the reference makes once per
// follower (`GetValidPos` -> `GetWORLDPOS`, World.cpp:1676-1687). A saved warp row stores
// a CELL and no height, so the height to search around is taken from the leader's own
// ground - that is exactly what the emitter does for the same two offsets
// (`emit_zone_points.py`, `_plan_formation` hands each follower slot the point's z).
//
// NIL means "no per-slot cell can be answered here": no path engine, or no ground under
// the leader itself. formationAround then gives every slot the leader's own cell - the
// team still spreads, which is what the owner asked for, and no slot is refused.
//
// A slot the mesh cannot resolve answers false, and that slot alone falls back to the
// leader's spot (see formationAround).
func (h *Handler) formationCellAt(leader [3]int32) func(x, y int32) (int32, bool) {
	if h.NavHeight == nil || h.NavNear == nil {
		return nil
	}
	height, ok := h.NavHeight(leader)
	if !ok {
		return nil
	}
	return func(x, y int32) (int32, bool) {
		ground, _, onMesh := h.NavNear([3]int32{x, y, height}, NearRangeXY, NearRangeZ)
		if !onMesh {
			return 0, false
		}
		return ground[2], true
	}
}

// formationSlot is THE rule for one member's place, shared by zone entry and the town
// warp so the two cannot disagree about where a team stands.
//
// It answers pos for: the leader (slot 0 IS the point), any member past the slots the
// file carries, a file with no formation at all, a slot with no resolved cell (the
// emitter only writes that beside a leader that is itself on the edge - the leader's
// point is the one place known to be walkable), and a slot whose cell is not a
// plausible triangle index.
func formationSlot(slots []FormationSlot, pos [3]int32, index int) [3]int32 {
	if index <= 0 || index >= len(slots) {
		return pos
	}
	slot := slots[index]
	if slot.Cell == nil || !PlausibleCell(*slot.Cell) {
		return pos
	}
	return [3]int32{slot.X, slot.Y, *slot.Cell}
}

// cellOrZero is a point's cell as a plain number, for the Slot above. The caller has
// already refused a point with no cell, so a missing one can only reach here from a
// hand-built value in a test.
func cellOrZero(cell *int32) int32 {
	if cell == nil {
		return 0
	}
	return *cell
}

// LoadZonePoints reads the shared spawn table.
//
// Every failure is an error, and the caller makes it fatal: there is no honest
// fallback, because the only one available is a hardcoded point that is right for
// exactly one map.
func LoadZonePoints(path string) (*ZonePoints, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc ZonePoints
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Version < ZonePointsVersionMin || doc.Version > ZonePointsVersion {
		return nil, fmt.Errorf("%s: spawn table version %d, want %d..%d - the file came "+
			"from another emitter; rebuild it with: cd src && python emit_zone_points.py",
			path, doc.Version, ZonePointsVersionMin, ZonePointsVersion)
	}
	if len(doc.Maps) == 0 {
		return nil, fmt.Errorf("%s: no map at all; rebuild it with: "+
			"cd src && python emit_zone_points.py", path)
	}
	return &doc, nil
}

// WarpCamera answers the five camera floats the client is told to use when it arrives on
// mapID, for a destination that has no gate row of its own.
//
// A gate row carries the DESTINATION's camera - the five floats ARE what WarpPoint.cpp
// fills 3594 with - so the honest sources are, in this order:
//
//  1. a gate row that ARRIVES at mapID: exactly the numbers that row already uses for
//     this map;
//  2. a gate row OUT of mapID: the numbers authored for that map's own world (this is
//     what answers office_coim today, and its row carries 25/-297/200/2200/1000 - the
//     same five numbers the client's own Warp(pc, 5005) row holds for the same map);
//  3. nothing, and then all five are sent as 0 - the client keeps its own camera - and
//     the caller says so in the log.
//
// The walk is in sorted map order, so two runs of the same table pick the same row.
func (z *ZonePoints) WarpCamera(mapID int) (cam [5]float32, from string) {
	if z == nil {
		return cam, ""
	}
	keys := make([]string, 0, len(z.Maps))
	for k := range z.Maps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, g := range z.Maps[k].Gates {
			if g.ToID == mapID {
				return [5]float32{g.Lat, g.Long, g.ZoomMin, g.ZoomMax, g.InitZoom},
					fmt.Sprintf("gate %s (arriving from map %s)", g.Name, k)
			}
		}
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if ok && len(entry.Gates) > 0 {
		g := entry.Gates[0]
		return [5]float32{g.Lat, g.Long, g.ZoomMin, g.ZoomMax, g.InitZoom},
			fmt.Sprintf("gate %s (out of map %d itself)", g.Name, mapID)
	}
	return cam, ""
}

// Name answers a map id's CLASS name ("coim_basic" for 15), which the client feeds
// straight into SwitchMode.
//
// False means "this map is not in the table". The barrack then keeps whatever it used
// before the table existed - a name invented here would be a wrong world, and a wrong
// world is the bug the registry was built to end.
func (z *ZonePoints) Name(mapID int) (string, bool) {
	if z == nil {
		return "", false
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if !ok || entry.Name == "" {
		return "", false
	}
	return entry.Name, true
}

// TownPoints answers a map's town arrival points, in file order: n=1 is the button
// the quick-move panel numbers 1.
//
// Empty for a map with no town (most maps) and for a map the table does not carry,
// and the two are the same answer on purpose: there is no destination either way.
func (z *ZonePoints) TownPoints(mapID int) []TownPoint {
	if z == nil {
		return nil
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if !ok {
		return nil
	}
	return entry.Points
}

// RestartWarpMap answers which map owns this restart warp id - the reverse of
// RestartLanding, and the question the SAVED START POINT asks.
//
// A saved start point is stored as a WARP ID alone (`commander.point`, see
// db\migrations\00010_point_and_position.sql), because that is what the original
// server stored (Commander.cpp:589 GetRestartWarpID) and what the client's 3467
// answer carries. A zone serves ONE map, so before it can use a saved warp it has to
// know which map that warp belongs to - and there is NO global warp table in this
// project, only each map's own `restart` row in data\zone_points.json. So a warp id
// resolves exactly when some map's restart row names it; that is the whole lookup.
//
// DETERMINISTIC ON PURPOSE. z.Maps is a Go map, so a plain range would answer a
// different map on different runs if two maps ever named the same warp id. The ids
// are unique in the emitted data; should that ever change, this returns the LOWEST
// map id so the answer is repeatable and a log line is worth trusting.
//
// False means "no map's restart row carries this warp id", and the caller must then
// refuse the save point rather than guess a map: a team in the wrong town is the one
// failure this project never accepts.
func (z *ZonePoints) RestartWarpMap(warpID int) (int, bool) {
	if z == nil || warpID <= 0 {
		return 0, false
	}
	mapID, found := 0, false
	for key, entry := range z.Maps {
		if entry.Restart == nil || entry.Restart.WarpID != warpID {
			continue
		}
		id, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		if !found || id < mapID {
			mapID, found = id, true
		}
	}
	return mapID, found
}

// SpawnFor answers where a team enters mapID, and REFUSES to answer when the file
// cannot support it.
//
// The refusals are deliberate, and every one of them happens at STARTUP - one command
// to fix - instead of in game, where the same mistake looks like a client bug and
// costs a login to rediscover:
//
//   - the map is not in the file: the id is wrong, or the map has no warp row;
//   - the map has no spawn row: the client cannot be placed on it;
//   - the spawn's cell is null: the emitter's path engine found no ground near the
//     point, so there is no place to stand. That is "this map cannot be served", not
//     a number to fill in - and it is never answered with another map's point, nor
//     with the HEIGHT, which is what crashed PathEngine.
func (z *ZonePoints) SpawnFor(mapID int) (Landing, error) {
	if z == nil {
		return Landing{}, fmt.Errorf("no spawn table loaded; build it with: " +
			"cd src && python emit_zone_points.py")
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if !ok {
		return Landing{}, fmt.Errorf("map %d is not in the spawn table, which carries "+
			"%d map(s) - pass the map's own id with -map, or rebuild the table with: "+
			"cd src && python emit_zone_points.py", mapID, len(z.Maps))
	}
	if entry.Spawn == nil {
		return Landing{}, fmt.Errorf("map %d (%s) has no spawn row in the table, so the "+
			"client cannot be placed on it - serve a map that has one, or rebuild the "+
			"table with: cd src && python emit_zone_points.py", mapID, entry.Name)
	}
	spawn := entry.Spawn
	if spawn.Cell == nil {
		return Landing{}, fmt.Errorf("map %d (%s) has NO resolved landing cell: the path "+
			"engine found no ground near (%d, %d). This map cannot be served - the client "+
			"needs a navmesh triangle index, and a height there crashes it inside "+
			"PathEngine. Serve another map, or rebuild the table with: cd src && "+
			"python emit_zone_points.py", mapID, entry.Name, spawn.X, spawn.Y)
	}
	if !PlausibleCell(*spawn.Cell) {
		return Landing{}, fmt.Errorf("map %d (%s): %w", mapID, entry.Name, badCellError(*spawn.Cell))
	}
	return Landing{
		Pos:       [3]int32{spawn.X, spawn.Y, *spawn.Cell},
		Formation: spawn.Formation,
		MapID:     mapID,
		Map:       entry.Name,
		Mesh:      entry.Mesh,
		Source:    spawn.Source,
	}, nil
}
