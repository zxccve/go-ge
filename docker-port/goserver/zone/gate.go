package zone

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"geserver/protocol"
	"geserver/world"
)

// The GATE: walking onto a town gate moves the player to another map.
//
// The shape is the original server's (source2\geServer\ZServerLib\WarpPoint.cpp,
// geZCPacket.h), and it is a HANDOFF between two ZONE PROCESSES, not a move inside
// one: every map here is its own process on its own port, and the client is told to
// reconnect to the destination's port. So a map change is three packets and one file:
//
//  1. NOTHING is sent by the client to start it. The ZONE notices, on its own tick,
//     that the leader stands within a gate's `range` of its x/y
//     (WarpPoint.cpp:38-47,60-104).
//  2. zone -> client ZC_MOVE_ZONE (3593, 3 bytes: cmd + one bool byte).
//  3. client -> zone CZ_MOVE_ZONE_OK (3686, the 10-byte security header and no body).
//  4. zone -> client ZC_MOVE_ZONE_OK (3594, 101 bytes, packed - the layout is in
//     BuildMoveZoneOK), naming the ip, port and map CLASS name to reconnect to, and
//     the WORLDPOS the team appears on there.
//  5. The client reconnects to that ip/port and sends CZ_START_GAME THERE. It re-auths
//     from scratch: the cipher is a fixed key with per-chunk framing, so nothing
//     carries over a socket.
//
// WHERE THE ARRIVAL POINT COMES FROM: step 4 already carries it, in the packet, so it
// would be tempting to trust the client to land there. It does not - the arrival point
// is what the SERVER says at START_GAME (the spawn record), and the destination zone
// knows nothing about the gate. That is what the handoff file is for: the OLD zone
// leaves the exit point in `out\zones\transfer_<account>.json` and the NEW zone
// consumes it ONCE, as the base for the arrival formation. A stale, foreign or
// other-map handoff is never used (see ReadTransfer).
//
// The gate data itself is in data\zone_points.json version 3, loaded by mapdata.go.
const (
	// CmdZCMoveZone is zone -> client: the map change is starting.
	CmdZCMoveZone = 3593

	// CmdCZMoveZoneOK is client -> zone: it accepted, and the zone may name the
	// server to reconnect to. It carries NO body - only the security header.
	CmdCZMoveZoneOK = 3686

	// CmdZCMoveZoneOK is zone -> client: where to reconnect, and where on that map
	// the team appears.
	CmdZCMoveZoneOK = 3594

	// MoveZoneSize and MoveZoneOKSize are the WHOLE packets; there is no length word
	// to read, the size belongs to the command in the client's own table
	// (data\cmd_table_live_z.txt: 3593 is 3, 3594 is 101, 3686 is 10).
	MoveZoneSize   = 3
	MoveZoneOKSize = 101

	// moveZoneAcceptSize is the whole client packet: the security header alone.
	moveZoneAcceptSize = 10

	// moveZoneFlag is the one bool byte after 3593's command word, and it must be FALSE
	// on a normal map change. MEASURED, not guessed: the live server sends command 3593
	// as the three bytes `09 0e 00` - 0x0e09 = 3593, then 0x00
	// (D:\ge_server\captures\warp_ref\20261027-200136\capture.txt line 593; line 594 is the client
	// answering CZ_MOVE_ZONE_OK 3686 in the same millisecond, line 595 is the server's
	// 3594).
	//
	// The byte is `ToWorldPVPServer`: TRUE means "this is a jump to the WORLD PvP server",
	// and the live server leaves it FALSE for an ordinary map change. Sending 0x01 here
	// was the gate blocker: the shipped client (ge.exe, durable analysis project) then
	// answers nothing AND stops writing to its own socket - its heartbeat (3688) stops in
	// the same second and the screen freezes (docker-port\logs\zone15_err.txt, 23:13:27:
	// 3593 out, then zero bytes).
	moveZoneFlag = 0
)

// The packed layout of ZC_MOVE_ZONE_OK (3594). Every offset starts at the command
// word, so they are the packet's own byte positions - there is no 10-byte security
// header on a server -> client packet.
const (
	mzInstID  = 2 // DWORD: the destination map id (only meaningful with ip+port)
	mzIP      = 6 // char[16]: the destination zone's ip, NUL padded
	mzIPLen   = 16
	mzPort    = 22 // DWORD: the destination zone's port
	mzMap     = 26 // char[41]: the destination map's CLASS name, NUL padded
	mzMapLen  = 41
	mzCamera  = 67 // float32 x5, consecutive: lat, long, zoomMin, zoomMax, initZoom
	mzCameras = 5
	mzPos     = 87  // int32 x3: x, y, CELL (a navmesh triangle index, never a height)
	mzDir     = 99  // BYTE: the arrival facing, 360/255 degrees
	mzChannel = 100 // BYTE: the destination channel
)

// MoveZoneDest is everything ZC_MOVE_ZONE_OK tells the client.
//
// InstID is the DESTINATION MAP ID. It only matters together with ip and port - the
// client keeps its socket only when all three match, and a handoff always names a
// different port, so a reconnect really happens.
type MoveZoneDest struct {
	InstID   uint32
	IP       string
	Port     int
	Map      string // the map CLASS name ("fld_coim"), which the client feeds to SwitchMode
	Lat      float32
	Long     float32
	ZoomMin  float32
	ZoomMax  float32
	InitZoom float32
	Pos      [3]int32 // x, y, CELL on the destination map
	Dir      uint8    // the direction BYTE, not degrees
	Channel  uint8
}

// gateMove is one map change in progress: set when ZC_MOVE_ZONE (3593) goes out,
// cleared when its answer (3594) has been sent. It lives on the Session and on nothing
// else, because it is one player's handshake.
type gateMove struct {
	gate  MapGate
	dest  world.Record
	pos   [3]int32 // where the player arrives on the destination map
	since time.Time

	// warpClass is the scroll class a Warp List jump PAID WITH, and 0 for every move
	// that costs the player nothing (a door, a warp NPC, the town grid, the
	// teleportation window). It is what lets gateReady give the fare back when the
	// destination has stopped answering by the time the client acks.
	warpClass int64
}

// BuildMoveZone builds ZC_MOVE_ZONE (3593): command(2) and one bool byte.
func BuildMoveZone() []byte {
	out := make([]byte, MoveZoneSize)
	binary.LittleEndian.PutUint16(out, CmdZCMoveZone)
	out[2] = moveZoneFlag
	return out
}

// BuildMoveZoneOK builds ZC_MOVE_ZONE_OK (3594), 101 bytes packed:
//
//	+0   command(2)
//	+2   zoneInstID(4)      the destination map id
//	+6   zoneIP[16]         NUL padded
//	+22  zonePort(4)
//	+26  map[41]            the map CLASS name, NUL padded
//	+67  lat(4, float32)
//	+71  long(4, float32)
//	+75  zoomMin(4, float32)
//	+79  zoomMax(4, float32)
//	+83  initZoom(4, float32)
//	+87  startPos[3] int32  x, y, CELL
//	+99  startDir(1)        BYTE
//	+100 channel(1)
//
// An ip that is not an IPv4 literal leaves the field zeroed instead of inventing one:
// the caller REFUSES such a destination before this is reached (see Handler.gateTick),
// so that cannot reach a client by accident.
//
// zoneIP is a dotted-decimal STRING, not a packed address: the original fills it with
// GetStrIP (imclib\Include\ImcCore.h:42-53) and the client parses it back. 16 bytes is
// exactly "255.255.255.255" plus its NUL, which is why nothing longer may be put there.
func BuildMoveZoneOK(d MoveZoneDest) []byte {
	out := make([]byte, MoveZoneOKSize)
	binary.LittleEndian.PutUint16(out, CmdZCMoveZoneOK)
	binary.LittleEndian.PutUint32(out[mzInstID:], d.InstID)
	copy(out[mzIP:mzIP+mzIPLen], d.IP)
	binary.LittleEndian.PutUint32(out[mzPort:], uint32(d.Port))
	copy(out[mzMap:mzMap+mzMapLen], d.Map)
	for i, v := range [mzCameras]float32{d.Lat, d.Long, d.ZoomMin, d.ZoomMax, d.InitZoom} {
		binary.LittleEndian.PutUint32(out[mzCamera+4*i:], math.Float32bits(v))
	}
	for i, v := range d.Pos {
		binary.LittleEndian.PutUint32(out[mzPos+4*i:], uint32(v))
	}
	out[mzDir] = d.Dir
	out[mzChannel] = d.Channel
	return out
}

// ---- the warp POINT: the arrow the client draws -----------------------------
//
// A Warp row is not only a trigger. The original spawns an OBJECT for it
// (CWorld::InitWarpNPC, World.cpp:1314-1356) and that object has packets of its own
// (WarpPoint.cpp:117-144):
//
//	3540 ZC_SPAWN_WARPPOINT  the arrow appears       - on entry, one per warp point
//	3541 ZC_ENTER_WARPPOINT  "you are inside it"     - when the leader walks into its radius
//	3542 ZC_LEAVE_WARPPOINT  "you are out of it"     - when the leader walks out
//	3593 ZC_MOVE_ZONE        the map change          - AFTER 3541, never instead of it
//
// 3541 GOES OUT BEFORE 3593 AND THE CLIENT STILL DID NOT ANSWER. Measured in game
// 2026-10-27 (docker-port\logs\zone15_err.txt, 22:22:51): walking onto Coimbra's office
// arrow logged "SENT cmd 3541, 32 bytes" and then "SENT cmd 3593, 3 bytes", and no
// CZ_MOVE_ZONE_OK (3686) ever came back.
//
// WHY IT DID NOT ANSWER - FOUND. The order was right; the CONTENT of 3593 was wrong. Its
// one bool byte was sent as 0x01, and the live server sends 0x00
// (D:\ge_server\captures\warp_ref\20261027-200136\capture.txt line 593: `09 0e 00`). With 0x01 the
// shipped client answers nothing and stops writing to its own socket. See moveZoneFlag.
//
// RULED OUT - do not re-check it: "the client answered nothing because it had never been
// told it was standing in a warp point". It WAS told, and in the right order. The order is
// now pinned by TestWarpPointEnterPrecedesMoveZone, so a regression there fails the build
// rather than being blamed on the client.
//
// NO COUNTDOWN. The reference has none: entering the radius sends OM_TRANSFER_ZONE
// (WarpPoint.cpp:104), CCommander::OnMsgTransferZone hands it straight to
// CCommander::TransferZone (Commander_Message.cpp:204-213, Commander.cpp:1153-1166),
// which sets SS_MOVE, and that state's ON_ENTER saves the commander and sends
// ZC_MOVE_ZONE (Commander_FSM.cpp:797-916). Same tick, no timer - the DB save in
// between is the only delay. The one timer there is `m_moveTimeOut = now + 32s`
// (Commander_FSM.cpp:916), which is how long the SERVER waits for CZ_MOVE_ZONE_OK;
// nothing is sent to rush the client, so none is invented here.
const (
	// CmdZCSpawnWarpPoint is zone -> client: the arrow of a warp point exists.
	CmdZCSpawnWarpPoint = 3540

	// CmdZCEnterWarpPoint is zone -> client: the leader is inside its radius.
	CmdZCEnterWarpPoint = 3541

	// CmdZCLeaveWarpPoint is zone -> client: the leader is out of it again.
	CmdZCLeaveWarpPoint = 3542

	// WarpPointSize is the WHOLE 3540 and 3541. The client's own table says 32 for both
	// (data\cmd_table.txt), they are the same struct with another command word.
	WarpPointSize = 32

	// WarpLeaveSize is the whole 3542: command(2) + handle(4) + code(2).
	WarpLeaveSize = 8

	// warpLeaveCode is the code 3542 carries. CWarpPoint::SendLeave takes the WORD the
	// caller passes; the visitor's leave path has no sight code of its own, and the only
	// code the client treats specially is OBJ_OUTOFSIGHT, which is not this case.
	warpLeaveCode = 0
)

// The offsets of 3540/3541, MEASURED off the shipped client, not read off source2:
//
//	+0   command(2)
//	+2   handle(4)
//	+6   9 bytes the handler ignores
//	+15  x(4) y(4) cell(4)   WORLDPOS; cell is a navmesh triangle index, never a height
//	+27  direction(1)
//	+28  type(4)             the Warp row's ClassID
//
// WHY +15 AND NOT +6: source2's geZCPacket.h is from 2014, older than the shipped client.
// Its PKS_ZC_SPAWN_STANDING {handle, WORLDPOS, direction, relation} is 18 bytes, which
// would make 3540 twenty-four - but the client's own table says 32, and the SIBLING packet
// ZC_SPAWN_STANDINGTRIGGER (3543, 109) is read out of the binary at exactly these offsets:
// handle +2, pos +15, direction +27, appearance +28 (src\zone_server.py:2158-2180, its
// handlers FUN_004f424e / FUN_004f35a3). So the shipped struct carries 9 more bytes before
// the position, and 3518 (ZC_RE_ENTER_STANDING, 28) and 3540 (32) are exactly
// 28 + {0, 4}: `type` lands where 3543's appearance lands, at +28.
//
// The `relation` byte source2 shows between direction and type has NO room in the shipped
// client (28+1+1+4 would be 34) and the 3543 handler reads direction straight into its own
// copy, so nothing is written for it. The reference does not set direction either:
// CWarpPoint::SendSpawned fills command, handle, pos and type and nothing else, which is
// why the caller passes 0 there and this server matches it.
const (
	wpHandle    = 2
	wpPos       = 15
	wpDirection = 27
	wpType      = 28
)

// BuildSpawnWarpPoint builds ZC_SPAWN_WARPPOINT (3540): the arrow of one warp point.
func BuildSpawnWarpPoint(handle uint32, pos [3]int32, direction uint8, typ uint32) []byte {
	return buildWarpPoint(CmdZCSpawnWarpPoint, handle, pos, direction, typ)
}

// BuildEnterWarpPoint builds ZC_ENTER_WARPPOINT (3541): the SAME packet with the other
// command word. The client's two handlers (CNormalNet::SpawnWarpPoint and
// ::EnterWarpPoint, GENormalNet_World.cpp:402-418) call the same geactor::WarpPointEnter
// with the same three arguments, which is why one builder body serves both.
func BuildEnterWarpPoint(handle uint32, pos [3]int32, direction uint8, typ uint32) []byte {
	return buildWarpPoint(CmdZCEnterWarpPoint, handle, pos, direction, typ)
}

func buildWarpPoint(cmd uint16, handle uint32, pos [3]int32, direction uint8, typ uint32) []byte {
	out := make([]byte, WarpPointSize)
	binary.LittleEndian.PutUint16(out[0:], cmd)
	binary.LittleEndian.PutUint32(out[wpHandle:], handle)
	for i, v := range pos {
		binary.LittleEndian.PutUint32(out[wpPos+4*i:], uint32(v))
	}
	out[wpDirection] = direction
	binary.LittleEndian.PutUint32(out[wpType:], typ)
	// +6..+15 stay zero: 9 bytes the client's handler never reads.
	return out
}

// BuildLeaveWarpPoint builds ZC_LEAVE_WARPPOINT (3542), 8 bytes:
//
//	+0 command(2) +2 handle(4) +6 code(2)
//
// The client's handler is CNormalNet::LeaveWarpPoint (GENormalNet_World.cpp:420-429): it
// leaves the handle locally, and only OBJ_OUTOFSIGHT takes the "gone for good" path.
func BuildLeaveWarpPoint(handle uint32, code uint16) []byte {
	out := make([]byte, WarpLeaveSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCLeaveWarpPoint)
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint16(out[6:], code)
	return out
}

// WarpPointHandleBase is where a warp point's handle starts.
//
// A handle lives in ONE id space per client view: town NPCs are 0x40000000 and up
// (data\zone_npcs\map*.json), a character's handle is its own DB id (store.go), and
// 0x7F000000 is far above both. A warp point's handle has to be STABLE for the session -
// 3541 and 3542 must name the point 3540 named - so it is derived from the trigger key
// and never allocated.
const WarpPointHandleBase = 0x7F000000

// WarpPointHandle answers the handle of the trigger whose key this is (gates are index+1,
// warp NPCs are len(Gates)+index+1 - see triggerAt).
func WarpPointHandle(key int) uint32 { return WarpPointHandleBase + uint32(key) }

// WarpTypeTable answers the `type` a warp point packet carries: the ClassID of the Warp
// row the point belongs to.
//
// In the original that is WARP_PROPERTY.type, an imcIES::ClassID (geWarpTable.h:11) read
// from the Warp IES and passed to CWarpPoint::Init(type, range) (World.cpp:1350). The
// client uses it AS A ROW ID: it looks the row up in its own copy of the same table
// (CBaseObject::WarpPointEnterWorld -> imcIES::GetPropertyString("Warp", type, "Name"),
// GeBaseObject.cpp:405-414) and hands it to the scene as the appearance. It is not an
// enum of arrow styles, so an invented number names no row - which is the "no arrow at
// all" the owner sees today. A zero falls back to "no row named" and says so in the log.
type WarpTypeTable struct {
	byName map[string]int // Warp ClassName (upper) -> ClassID: this is a gate's own row
	byDoor map[string]int // "create>target" (lower) -> ClassID, unambiguous rows only
	rows   int
}

// LoadWarpTypes reads datatable_warp.xml (idspace "Warp") from dir, the same folder the
// stance tables come from. A missing or unreadable file is an error the CALLER may ignore:
// the zone then sends type 0, which is a guess and is logged as one.
func LoadWarpTypes(dir string) (*WarpTypeTable, error) {
	t := &WarpTypeTable{byName: map[string]int{}, byDoor: map[string]int{}}
	type door struct {
		id       int
		from, to string
	}
	var rows []door
	_, err := eachClass(filepath.Join(dir, "datatable_warp.xml"), func(attrs map[string]string) {
		id, ok := atoi(attrs["ClassID"])
		if !ok {
			return
		}
		t.rows++
		if name := strings.TrimSpace(attrs["ClassName"]); name != "" {
			t.byName[strings.ToUpper(name)] = id
		}
		rows = append(rows, door{
			id:   id,
			from: strings.ToLower(strings.TrimSpace(attrs["CreateZone"])),
			to:   strings.ToLower(strings.TrimSpace(attrs["TargetZone"])),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("warp table: %w", err)
	}

	// Only ONE row may answer a doorway. 807 of the 1140 rows name no create zone
	// ("None"): they are the script-driven doors, and several of them can target the
	// same map. A key that more than one row claims is left OUT, so an ambiguous door
	// answers "no row" instead of naming the wrong arrow.
	seen := make(map[string]int, len(rows))
	for _, r := range rows {
		seen[r.from+">"+r.to]++
	}
	for _, r := range rows {
		k := r.from + ">" + r.to
		if seen[k] == 1 {
			t.byDoor[k] = r.id
		}
	}
	return t, nil
}

// TypeOf answers the ClassID of the Warp row with this ClassName. A gate IS such a row and
// data\zone_points.json keeps the row's ClassName as the gate's name
// ("COIM_BASIC-FLD_COIM"), so this is an exact match and not a guess.
func (t *WarpTypeTable) TypeOf(name string) (int, bool) {
	if t == nil {
		return 0, false
	}
	id, ok := t.byName[strings.ToUpper(strings.TrimSpace(name))]
	return id, ok
}

// TypeForArrow answers the row that describes an arrow NPC's door: the row that leads FROM
// this map's class TO the arrow's destination, or - for the script-driven rows that name
// no create zone - the row that targets the destination.
//
// Coimbra's office arrow is the second form: `WARP_OFFICE_COIM` stands in coim_basic and
// leads to office_coim, and the only row that targets office_coim is 5005
// COIM-BASIC_OFFICE_COIM with CreateZone "None".
func (t *WarpTypeTable) TypeForArrow(from, to string) (int, bool) {
	if t == nil || to == "" {
		return 0, false
	}
	if from != "" {
		if id, ok := t.byDoor[strings.ToLower(from)+">"+strings.ToLower(to)]; ok {
			return id, true
		}
	}
	id, ok := t.byDoor["none>"+strings.ToLower(to)]
	return id, ok
}

// Describe is the one-line summary the zone logs at start.
func (t *WarpTypeTable) Describe() string {
	if t == nil {
		return "not loaded - warp point packets will carry type 0"
	}
	return fmt.Sprintf("%d warp rows, %d gate names, %d doorways", t.rows,
		len(t.byName), len(t.byDoor))
}

// warpPoint is one drawn arrow: the handle the client will see, where it stands, and the
// Warp row it is (the `type` of its packets).
type warpPoint struct {
	handle uint32
	pos    [3]int32
	typ    uint32
	label  string // what the log calls it: a gate's row name or an NPC's script
}

// warpPointFor turns a trigger key into the warp point the client must be told about.
//
// `ok` false means "there is no arrow here", and there are two such cases:
//
//   - a gate whose trigger CELL was never resolved (the table writes null). A position
//     without a cell is not ground, and the client would put the arrow inside the mesh;
//   - a scripted trigger that is not a door, or a door with no Warp row. warpScriptName's
//     prefixes match 18 of map 15's NPCs and only some name a map (TR_BOOK_15 is a quest
//     dialog), so the SCRIPT must resolve before the NPC counts at all - and when it does
//     resolve, the row must exist, because `type` IS the row: without one there is no
//     number to send and the client would be told to draw an unnamed arrow.
func (h *Handler) warpPointFor(key int, gate MapGate, npc NPC, isWarp bool) (warpPoint, bool) {
	wp := warpPoint{handle: WarpPointHandle(key)}
	if isWarp {
		class, _, ok := h.warpMapName(npc)
		if !ok {
			return warpPoint{}, false
		}
		typ, ok := h.Warps.TypeForArrow(h.MapName, class)
		if !ok {
			return warpPoint{}, false
		}
		wp.pos = [3]int32{npc.X, npc.Y, npc.Cell}
		wp.label = fmt.Sprintf("warp NPC %s", npc.Script)
		wp.typ = uint32(typ)
		return wp, true
	}
	if gate.Cell == nil {
		return warpPoint{}, false
	}
	wp.pos = [3]int32{gate.X, gate.Y, *gate.Cell}
	wp.label = "gate " + gate.Name
	if typ, ok := h.Warps.TypeOf(gate.Name); ok {
		wp.typ = uint32(typ)
	}
	return wp, true
}

// isWarpDoor reports whether this NPC is a DRAWABLE door: a warp script that resolves to a
// map. The seed for the entry burst and for the enter notice, so both agree.
func (h *Handler) isWarpDoor(npc NPC) bool {
	if !isWarpScript(npc.Script) {
		return false
	}
	_, _, ok := h.warpMapName(npc)
	return ok
}

// spawnWarpPoints is the entry burst's warp points: one ZC_SPAWN_WARPPOINT (3540) for every
// gate of this map, then one for every warp-script NPC. Both are DOORS the client draws -
// the client cannot draw what it was never told exists, which is the second half of the
// 2026-10-27 report ("the owner sees no arrow at the exit").
//
// It runs once per session, at GAME_READY and after the town NPCs, because the client only
// draws actors once it has the world.
func (h *Handler) spawnWarpPoints() [][]byte {
	var out [][]byte
	add := func(key int, gate MapGate, npc NPC, isWarp bool) {
		wp, ok := h.warpPointFor(key, gate, npc, isWarp)
		if !ok {
			if isWarp {
				h.Log.Printf("zone: no arrow for warp NPC %s - no Warp row leads from "+
					"this map to its destination, so there is no `type` to draw it with",
					npc.Script)
			} else {
				h.Log.Printf("zone: no arrow for gate %s - its trigger cell is not in "+
					"the table, so there is no ground under it to draw on", gate.Name)
			}
			return
		}
		out = append(out, BuildSpawnWarpPoint(wp.handle, wp.pos, 0, wp.typ))
		h.Log.Printf("zone: warp point spawned %s: handle 0x%08x at (%d, %d, cell %d), "+
			"direction 0, type %d (%s)", wp.label, wp.handle, wp.pos[0], wp.pos[1],
			wp.pos[2], wp.typ, h.warpTypeSource(wp))
	}
	for i, g := range h.Gates {
		add(i+1, g, NPC{}, false)
	}
	for i, n := range h.NPCs {
		if !h.isWarpDoor(n) {
			continue
		}
		add(len(h.Gates)+i+1, MapGate{}, n, true)
	}
	return out
}

// warpTypeSource names where a type came from, because 0 is a guess and must read as one
// in a log that a game test is going to be read from.
func (h *Handler) warpTypeSource(wp warpPoint) string {
	if wp.typ == 0 {
		if h.Warps == nil {
			return "NO WARP TABLE - type 0 is a guess, A/B it in game"
		}
		return "NO WARP ROW NAMED - type 0 is a guess, A/B it in game"
	}
	return "Warp row ClassID"
}

// leaveWarpPoint is the one sender of ZC_LEAVE_WARPPOINT (3542). It clears the record, so
// walking out and back in sends the enter notice again.
func (h *Handler) leaveWarpPoint(sess *Session) []byte {
	handle := WarpPointHandle(sess.warpInside)
	sess.warpInside = 0
	h.Log.Printf("zone: LEFT warp point handle 0x%08x (code %d) - the arrow is out of range",
		handle, warpLeaveCode)
	return BuildLeaveWarpPoint(handle, warpLeaveCode)
}

// gateAt answers the gate whose trigger circle the leader is standing in.
//
// The NEAREST one wins when two circles overlap, so two doorways a few units apart
// cannot swap places between ticks. The distance is measured on x and y only: the
// original's IsNearObject is a ground-plane test, and the third number is a triangle
// index, not a coordinate. A gate with no measured `range` is skipped - a guessed
// radius warps a player who only walked past.
func (h *Handler) gateAt(pos [3]int32) (index int, gate MapGate, dist float64, ok bool) {
	for i, g := range h.Gates {
		if g.Range <= 0 {
			continue
		}
		d := math.Hypot(float64(pos[0]-g.X), float64(pos[1]-g.Y))
		if d > float64(g.Range) {
			continue
		}
		if !ok || d < dist {
			index, gate, dist, ok = i, g, d, true
		}
	}
	return index, gate, dist, ok
}

// ---- warps carried by an NPC's own script ---------------------------------
//
// Not every doorway is a Warp row. Coimbra's two green arrows are ordinary town NPCs
// spawned from data\zone_npcs\map<id>.json - `EntranceArrow` with script
// TR_COIM_EMILIA_LABORATORY at (-7801, 7295) and `EntranceArrow5` with script
// WARP_OFFICE_COIM at (-8771, 5529) - and the client treats walking onto one as the
// start of a map change, the same UI state 3593 puts it in. Nothing in the Warp table
// describes them, so before this existed the zone never answered, and the client sat
// waiting with every key dead ("the hotkeys broke").
//
// So the SCRIPT decides: an NPC whose Script column can be resolved to a map is a DOOR,
// and it is served by the SAME handshake as a gate. There is exactly one sender of
// ZC_MOVE_ZONE (Handler.startMove) and one place that names the destination
// (Handler.gateReady), so a warp NPC cannot drift away from a gate.

// WarpNPCRange is the radius, in world units, at which an NPC whose script is a zone
// warp is triggered.
//
// The NPC rows carry NO range: data\zone_npcs\map<id>.json is the client's own spawn
// table plus its Script column, and only a Warp row has a measured `Range` (the gates
// use theirs, 90..1000). One number therefore has to be chosen, and 150 is chosen for
// two reasons:
//
//   - a CHASE stops `DialogStandoff(NPCDefaultRadius)` (150, nav.go) short of the NPC it
//     walked to, so a player who CLICKS the arrow has to end up INSIDE it - the radius
//     must stay ABOVE the stand-off, or the walk lands exactly on the edge and the arrow
//     does nothing. The margin is the same 10 units it always had (140 + 10 before piece
//     2d took the stand-off to the reference's DISTANCE_TO_CHAR outside the shape);
//   - it must not catch somebody walking PAST the arrow. The arrows are one-body-wide
//     objects (`EntranceArrow`, `TutorialTrigger_Black_3m` - a 3 metre trigger) and one
//     walk report moves a character tens of units, so 160 is about "standing on it" and
//     not "in the same street".
const WarpNPCRange = 160

// warpScriptPrefixes are the leading tokens the client puts on a trigger's Script
// column. ONE of them is stripped, and the rest is read as a map name:
// `WARP_OFFICE_COIM` -> `office_coim`, `TR_COIM_EMILIA_LABORATORY` ->
// `coim_emilia_laboratory`.
//
// TR_ is the generic trigger prefix - 25 of the 26 prefixed NPC scripts in the shipped
// data, and most of them are quest dialogs, not doors (`TR_BOOK_15`, `TR_COIM_OBSERV`).
// So a prefix only says "look at this one"; what makes an NPC a WARP is that one of the
// candidate names below EXISTS in the spawn table. That is also why the refusal for a
// prefixed script that names no map is worth a log line: it is a door the data forgot.
var warpScriptPrefixes = []string{"WARP_", "TR_", "SO_"}

// warpScriptName strips one warp prefix and answers the rest, lowercased, which is the
// spelling zone_points.json's `by_name` uses. False means the script cannot be a zone
// warp at all (`C1_COIM_EMILIA`, `SHOP_MARKET`, an empty script).
//
// EqualFold and not ToUpper: this runs for every town NPC on every tick (see
// hasWarpNPCs), and a per-NPC allocation there is 119 strings a tick for nothing.
func warpScriptName(script string) (string, bool) {
	s := strings.TrimSpace(script)
	for _, prefix := range warpScriptPrefixes {
		if len(s) <= len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
			continue
		}
		return strings.ToLower(s[len(prefix):]), true
	}
	return "", false
}

// isWarpScript is warpScriptName without the lowercased tail: "is this NPC tested at
// all?", which the tick asks of every town NPC.
func isWarpScript(script string) bool {
	s := strings.TrimSpace(script)
	for _, prefix := range warpScriptPrefixes {
		if len(s) > len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// warpCandidates answers the map CLASS names a stripped warp script may name, in the
// order they are tried. It is a short EXPLICIT list and not a fuzzy match, so a wrong
// door says which name it took instead of quietly matching something close:
//
//	script                     candidates, in order                                    wins
//	WARP_OFFICE_COIM           office_coim                                            6004 (1st)
//	TR_COIM_EMILIA_LABORATORY  coim_emilia_laboratory, emilia_laboratory,             19225 (4th)
//	                           mission_coim_emilia_laboratory, mission_emilia_laboratory
//	TR_BOOK_15                 book_15, 15, mission_book_15, mission_15                none
//
// The middle two forms exist because the client prefixes the MAP onto the name
// (`coim_` in `coim_emilia_laboratory`) while the map itself is called
// `mission_emilia_laboratory`.
func warpCandidates(name string) []string {
	out := make([]string, 0, 4)
	add := func(c string) {
		if c == "" {
			return
		}
		for _, have := range out {
			if have == c {
				return
			}
		}
		out = append(out, c)
	}
	add(name)
	tail := ""
	if i := strings.IndexByte(name, '_'); i > 0 && i+1 < len(name) {
		tail = name[i+1:]
		add(tail)
	}
	add("mission_" + name)
	if tail != "" {
		add("mission_" + tail)
	}
	return out
}

// warpAt answers the nearest warp NPC whose circle the leader is standing in. `ok` means
// "inside a warp NPC's radius", NOT "it resolves": a door that names no map still has to
// be reported once when the player steps on it, and that is the caller's job.
//
// Nearest wins, exactly as gateAt does it, so two arrows a few units apart cannot swap
// places between ticks.
func (h *Handler) warpAt(pos [3]int32) (index int, npc NPC, dist float64, ok bool) {
	for i, n := range h.NPCs {
		if !isWarpScript(n.Script) {
			continue
		}
		d := math.Hypot(float64(pos[0]-n.X), float64(pos[1]-n.Y))
		if d > WarpNPCRange {
			continue
		}
		if !ok || d < dist {
			index, npc, dist, ok = i, n, d, true
		}
	}
	return index, npc, dist, ok
}

// hasWarpNPCs reports whether this map carries any NPC that could be a door, so an idle
// tick on a map with neither gates nor arrows costs one loop over Script strings.
func (h *Handler) hasWarpNPCs() bool {
	for _, n := range h.NPCs {
		if isWarpScript(n.Script) {
			return true
		}
	}
	return false
}

// triggerAt answers the nearest trigger the leader stands in, gates and warp NPCs in ONE
// key space: a gate is index+1 and a warp NPC is len(h.Gates)+index+1.
//
// One key space is what lets Session.gateInside stay a single record for both, so the
// re-arm rule ("walk out and back in") is the same code for a gate and an arrow, and so
// the arrival guard at START_GAME cannot know about one and not the other. A tie goes to
// the datatable gate: it is the measured one.
func (h *Handler) triggerAt(pos [3]int32) (key int, gate MapGate, warpNPC NPC, dist float64, isWarp bool, ok bool) {
	gi, g, gd, gin := h.gateAt(pos)
	wi, n, wd, win := h.warpAt(pos)
	switch {
	case gin && win && wd < gd:
		return len(h.Gates) + wi + 1, MapGate{}, n, wd, true, true
	case gin:
		return gi + 1, g, NPC{}, gd, false, true
	case win:
		return len(h.Gates) + wi + 1, MapGate{}, n, wd, true, true
	}
	return 0, MapGate{}, NPC{}, 0, false, false
}

// warpMapName resolves an NPC's Script to the map it leads to, and answers the CLASS name
// that won - the by_name key, which is the same name the packet carries and the client
// feeds to SwitchMode.
func (h *Handler) warpMapName(npc NPC) (class string, mapID int, ok bool) {
	name, isWarp := warpScriptName(npc.Script)
	if !isWarp || h.ZonePoints == nil {
		return "", 0, false
	}
	for _, cand := range warpCandidates(name) {
		if id, found := h.ZonePoints.ByName[cand]; found {
			return cand, id, true
		}
	}
	return "", 0, false
}

// warpGate turns a warp NPC into the SAME MapGate a datatable row is, so the handshake
// below runs unchanged: one code path sends a map change.
//
// Every refusal answers a REASON and happens HERE - before 3593 - so a door the data or
// the running zones cannot serve leaves the player standing where they are, instead of
// inside a handshake that will never be answered. The caller logs the reason once per
// entry (see gateTick).
func (h *Handler) warpGate(npc NPC) (MapGate, string, bool) {
	who := fmt.Sprintf("warp NPC %s (%s, 0x%08x)", npc.Script, npc.ClassType, npc.Handle)
	name, isWarp := warpScriptName(npc.Script)
	if !isWarp {
		return MapGate{}, fmt.Sprintf("zone: %s REFUSED - %q carries none of the warp "+
			"prefixes (%s), so its script names no map", who, npc.Script,
			strings.Join(warpScriptPrefixes, ", ")), false
	}
	if h.ZonePoints == nil {
		return MapGate{}, fmt.Sprintf("zone: %s REFUSED - no spawn table is loaded, so "+
			"%q cannot be read as a map name; start the zone with -points "+
			"../data/zone_points.json", who, npc.Script), false
	}
	class, mapID, ok := h.warpMapName(npc)
	if !ok {
		return MapGate{}, fmt.Sprintf("zone: %s REFUSED - its script names no map: "+
			"tried %s in data\\zone_points.json's by_name, so it is not a zone warp and "+
			"the zone sends nothing. The client's own entry for a trigger like this is "+
			"SCR_ENTER_<Script>, which this zone does not run yet - if walking onto it "+
			"freezes the client, THAT is the missing piece", who,
			strings.Join(warpCandidates(name), ", ")), false
	}
	// A MISSION map is created per party as an INSTANCE (data\map_table.xml: map 19225
	// mission_emilia_laboratory, Type="MISSION", Mission="TRUE"). This server runs one
	// process per STATIC map, and no registry can announce an instance, so there is
	// nowhere to send the player - the script's own answer here is DirectMission, which
	// is not implemented.
	if strings.HasPrefix(class, "mission_") {
		return MapGate{}, fmt.Sprintf("zone: %s REFUSED - its script resolves to map %d "+
			"(%s), which is an INSTANCED mission map (data\\map_table.xml: "+
			"Type=\"MISSION\"): it is created per party, and this server serves one "+
			"process per static map. The original server's answer here is the script's "+
			"DirectMission, which is not implemented - the door stays shut",
			who, mapID, class), false
	}
	// The destination's SPAWN POINT is where the team arrives: slot 0 of the map's own
	// formation, which is the same point SpawnFor hands the zone at START_GAME. The
	// arrow itself carries no arrival point - unlike a Warp row it has no TargetX/Y.
	landing, err := h.ZonePoints.SpawnFor(mapID)
	if err != nil {
		return MapGate{}, fmt.Sprintf("zone: %s REFUSED - it leads to map %d (%s), which "+
			"cannot be entered: %v", who, mapID, class, err), false
	}
	cam, from := h.ZonePoints.WarpCamera(mapID)
	if from == "" {
		h.Log.Printf("zone: %s leads to map %d (%s), which no gate row names, so 3594 "+
			"carries a zero camera and the client keeps its own", who, mapID, class)
	}
	triggerCell := npc.Cell
	targetCell := landing.Pos[2]
	gate := MapGate{
		Name: npc.Script, To: class, ToID: mapID,
		X: npc.X, Y: npc.Y, Cell: &triggerCell,
		Range: WarpNPCRange,
		// startPos is the destination's spawn, and startDir is 0: the spawn row carries
		// no facing, and every slot of a formation stands in the leader's cell, so 0 is
		// the honest answer rather than a copied angle.
		TargetX: landing.Pos[0], TargetY: landing.Pos[1], TargetCell: &targetCell,
		TargetDir: 0, TargetDirByte: 0,
		Lat: cam[0], Long: cam[1], ZoomMin: cam[2], ZoomMax: cam[3], InitZoom: cam[4],
	}
	h.Log.Printf("zone: %s resolves to map %d (%s) - that name is the candidate that "+
		"won; the team arrives at (%d, %d, cell %d); camera from %s", who, mapID, class,
		landing.Pos[0], landing.Pos[1], landing.Pos[2], fromOr(from, "nowhere (zeros)"))
	return gate, "", true
}

// fromOr is a log-sized "this or that" for the camera source, which is empty when
// nothing in the table names the destination.
func fromOr(from, fallback string) string {
	if from == "" {
		return fallback
	}
	return from
}

// walkedPos is where the character has actually got to, not where the player clicked.
//
// CZ_MOVE_PC carries the DESTINATION of the click (geZCPacket.h:1133), so sess.Pos is
// where the player pointed, not where the character stands. Gating on that fired a
// warp the instant the arrow was clicked, whatever the distance: the character never
// walked there. The walk is simulated from the start of the latest segment at the
// speed the character really walks at (walkSpeed: the measured value when it is
// trustworthy, else the formula's own base), and it never overshoots the destination -
// the segment's own length is the clamp, so a click the character has already covered
// reads as arrived.
//
// AND IT FOLLOWS THE LEG'S ROUTE WHEN IT HAS ONE (2026-11-02): a bent leg is walked
// along its polyline, so the gate fires at the bend's far end and not at the point the
// chord would have reached first.
func (s *Session) walkedPos(now time.Time) [3]int32 {
	if s.moveAt.IsZero() {
		// No segment: this character was PLACED here (a spawn, a town warp, a finished
		// chase), so Pos is the truth.
		return s.Pos
	}
	return walkedRouteAt(s.moveRoute, s.moveFrom, s.Pos, s.moveAt, now, s.walkSpeed())
}

// gateTick is the zone's own tick: it notices the leader standing in a gate OR in a warp
// NPC's circle and starts the map change.
//
// It runs on every tick (Serve calls it after each client packet and on every poll),
// which is why it must be cheap and why it must be able to fire ONCE per entry: the
// session remembers the trigger the leader is standing in (Session.gateInside) and that
// record is only cleared when they walk out of it. Without it, a player who stands in the
// door after a refused handshake would be warped the moment they moved a step, and a door
// would fire again on every 100 ms tick.
//
// It answers nil for "nothing to send", which is the normal case, and it never answers
// half a handshake: a door that cannot be served is refused HERE, before 3593, so the
// client is never left waiting for a 3594 that will not come.
func (h *Handler) gateTick(sess *Session) [][]byte {
	// THE HANDOVER DEADLINE COMES FIRST, and it is checked before every other question
	// this tick asks: it is about a map change that has already been ACCEPTED and whose
	// scroll has already been spent, so it must not be skipped because this map happens
	// to have no gates, or because the team is being torn down. See Handler.armHandoff.
	if out := h.handoffExpired(sess); len(out) > 0 {
		return out
	}
	if len(sess.Team) == 0 {
		return nil
	}
	if len(h.Gates) == 0 && !h.hasWarpNPCs() {
		return nil
	}
	// A map change is already running, so no second 3593: a second one would make the
	// next answer carry the wrong destination. Nothing else is owed here - the move
	// finishes on the client's own ack (3686) and on nothing else. There is deliberately
	// NO timeout that COMPLETES a handshake: a timer which finished one the client
	// refused would hide the very bug that blocked this gate (the 3593 flag byte, see
	// moveZoneFlag). The handover deadline at the top of this function is not that
	// timer: it never finishes a handshake, it GIVES ONE UP and gives the scroll back
	// (see HandoffDeadline).
	if sess.Gate != nil {
		return nil
	}
	// The position is the WALKED one, never sess.Pos: Pos is the click TARGET (see
	// walkedPos), and gating on it warped the player the moment the arrow was clicked.
	now := time.Now()
	key, gate, npc, dist, isWarp, inside := h.triggerAt(sess.walkedPos(now))

	// Walking out of a warp point is the leave notice (3542), and it goes out BEFORE
	// anything the new state sends: the client must not still think it stands in the
	// old arrow when the next packet talks about another one. Walking straight from one
	// arrow into another is the same event - leave the old, then enter the new.
	var out [][]byte
	if sess.warpInside != 0 && (!inside || key != sess.warpInside) {
		out = append(out, h.leaveWarpPoint(sess))
	}
	if !inside {
		// Out of every door: the one the player was standing in re-arms, so walking out
		// and back in fires again.
		sess.gateInside = 0
		return out
	}
	if sess.gateInside == key {
		return out // still standing in the door that already fired
	}
	sess.gateInside = key

	// From here on every refusal is logged ONCE per entry (gateInside is set above): a
	// player standing in a door they cannot use must not fill the log every tick.
	//
	// The ENTER notice goes first, and it is not conditional on the door being usable:
	// standing inside a warp point is a fact about the world, and the reference sends it
	// from the range visitor (CWarpPoint::SendEntered), not from the transfer. That is
	// also what this client needs BEFORE it is asked to change zone.
	if wp, ok := h.warpPointFor(key, gate, npc, isWarp); ok {
		out = append(out, BuildEnterWarpPoint(wp.handle, wp.pos, 0, wp.typ))
		sess.warpInside = key
		h.Log.Printf("zone: ENTERED warp point %s: handle 0x%08x at (%d, %d, cell %d), "+
			"type %d - the enter notice is sent before any move-zone request",
			wp.label, wp.handle, wp.pos[0], wp.pos[1], wp.pos[2], wp.typ)
	}

	label := "gate"
	if isWarp {
		label = "warp NPC"
		built, reason, ok := h.warpGate(npc)
		if !ok {
			h.Log.Print(reason)
			return out
		}
		gate = built
	}
	return append(out, h.startMove(sess, label, gate, dist)...)
}

// startMove is the ONE sender of ZC_MOVE_ZONE (3593): the refusal checks, then the
// handshake. A gate row, an NPC's script and a waypoint click all arrive here with a
// MapGate, so there is one handshake and not two - the moment there were two, one of
// them would be the one with the bug.
//
// The refusals are the cases that would strand the player, and each is a separate
// sentence because each is fixed a different way. None of them has sent anything yet.
func (h *Handler) startMove(sess *Session, label string, gate MapGate, dist float64) [][]byte {
	return h.startMoveFor(sess, label, fmt.Sprintf("the leader is %.0f units from "+
		"(%d, %d), inside its radius %d", dist, gate.X, gate.Y, gate.Range), gate)
}

// startMoveFor is startMove with the REASON already in words.
//
// It exists because a map change has three triggers now - walking into a door (which
// knows a distance and a radius), an NPC's warp script, and a click on a town in the
// waypoint grid (which has neither). Only the sentence in the log differs; the refusals,
// the handshake and the ONE 3593 stay here, so a door and a town click cannot drift.
func (h *Handler) startMoveFor(sess *Session, label, because string, gate MapGate) [][]byte {
	if gate.ToID <= 0 || gate.To == "" {
		h.Log.Printf("zone: %s %s REFUSED - it names no destination map "+
			"(to %q, to_id %d); the row is in data\\zone_points.json and is rebuilt by: "+
			"cd src && python emit_zone_points.py", label, gate.Name, gate.To, gate.ToID)
		return nil
	}
	if gate.TargetCell == nil {
		h.Log.Printf("zone: %s %s -> map %d (%s) REFUSED - its arrival point (%d, %d) "+
			"has NO resolved cell on the destination's mesh, so there is nowhere to "+
			"land; rebuild the table with: cd src && python emit_zone_points.py",
			label, gate.Name, gate.ToID, gate.To, gate.TargetX, gate.TargetY)
		return nil
	}
	if !PlausibleCell(*gate.TargetCell) {
		h.Log.Printf("zone: %s %s -> map %d (%s) REFUSED - %v", label, gate.Name,
			gate.ToID, gate.To, badCellError(*gate.TargetCell))
		return nil
	}
	dest, live := h.ensureZone(gate.ToID)
	if !live {
		// The one case that would strand the player: 3593 goes out, the client answers,
		// and there is no address to name. Refusing BEFORE 3593 leaves a player who
		// simply cannot use that door, not one stuck in a handshake.
		h.Log.Printf("zone: %s %s -> map %d (%s) REFUSED - no live zone serves map %d "+
			"(looked in %s): start the zone for that map first",
			label, gate.Name, gate.ToID, gate.To, gate.ToID, registryDir(h.Zones))
		return nil
	}
	if ip := net.ParseIP(dest.IP).To4(); ip == nil {
		h.Log.Printf("zone: %s %s -> map %d (%s) REFUSED - the zone serving it "+
			"announced ip %q, which is not an IPv4 address", label, gate.Name, gate.ToID,
			gate.To, dest.IP)
		return nil
	}

	// A door walk is not a walk: drop a chase, or its tick keeps sending moves while the
	// client is tearing the session down.
	h.cancelChase(sess)
	sess.Gate = &gateMove{
		gate: gate, dest: dest,
		pos:   [3]int32{gate.TargetX, gate.TargetY, *gate.TargetCell},
		since: time.Now(),
	}
	h.Log.Printf("zone: %s %s: %s - starting a map change to map %d (%s)",
		strings.ToUpper(label), gate.Name, because, gate.ToID, gate.To)
	h.Log.Printf("zone: MOVE_ZONE sent (cmd %d, %d bytes) to account %d - waiting for "+
		"MOVE_ZONE_OK (cmd %d)", CmdZCMoveZone, MoveZoneSize, sess.AccountID,
		CmdCZMoveZoneOK)
	return [][]byte{BuildMoveZone()}
}

// acceptMove finishes a map change: it leaves the handoff for the destination zone,
// then builds ZC_MOVE_ZONE_OK (3594) naming it.
//
// Its ONE caller is Handler.gateReady - the client's own CZ_MOVE_ZONE_OK (3686). One
// 3593 gets one 3594 and one handoff, and only after the ack: the handshake is the
// client's decision, so nothing here finishes it on the server's own initiative.
//
// Kept as its own function because it is the ONE place a 3594 is ever built and a
// handoff ever written, so the packet layout and the file cannot drift apart.
//
// Order matters. The handoff is written BEFORE 3594 is built, because the client
// reconnects the moment it reads 3594 and the destination zone reads the file on
// START_GAME - a file written afterwards would be a race the player loses as "I came
// out at the town spawn".
//
// The caller clears Session.Gate first: one 3593 gets one 3594, and a repeated answer
// must not leave a second handoff for the same account.
func (h *Handler) acceptMove(sess *Session, m *gateMove) [][]byte {
	// THE LAST POINT OF THE MAP BEING LEFT, and it must be saved BEFORE the handoff:
	// h.MapID is the map this session is on now, and m.pos is the ARRIVAL point on the
	// OTHER map - saving that would put the player on a cell of a map they are not on,
	// which is why the position comes from sess.walkedPos (where the character really
	// stands) and not from m.pos.
	h.noteLastPoint(sess, lastPointMapChange)

	if path, err := h.writeHandoff(sess.AccountID, m); err != nil {
		// NOT fatal: the player still travels, and lands on the map's own spawn
		// instead of the gate's exit. Stranding them would be worse, and the
		// destination zone says which of the two happened. There is nothing to put a
		// deadline on either: no file was written, so no town can be slow to take it.
		h.Log.Printf("zone: HANDOFF NOT written (%v) - the player still enters map %d, "+
			"but at that map's own spawn instead of gate %s", err, m.gate.ToID, m.gate.Name)
	} else if m.warpClass != 0 {
		// A PAID map change starts the handover deadline HERE: the scroll is spent,
		// the client has been told to reconnect, and from this instant the ONE question
		// that can still cost the player something is whether the destination takes the
		// character. A free move (a door, a warp NPC) cannot eat a scroll, so it is not
		// armed - see Handler.armHandoff.
		h.armHandoff(sess, m, path)
	}

	out := BuildMoveZoneOK(MoveZoneDest{
		InstID: uint32(m.gate.ToID), IP: m.dest.IP, Port: m.dest.Port,
		Map: m.gate.To,
		Lat: m.gate.Lat, Long: m.gate.Long,
		ZoomMin: m.gate.ZoomMin, ZoomMax: m.gate.ZoomMax, InitZoom: m.gate.InitZoom,
		Pos: m.pos, Dir: m.gate.TargetDirByte, Channel: channelByte(m.dest.Channel),
	})
	h.Log.Printf("zone: MOVE_ZONE_OK sent (cmd %d, %d bytes): reconnect to %s:%d for "+
		"map %d (%s), startPos (%d, %d, cell %d), dir byte %d, channel %d, camera "+
		"%.0f/%.0f/%.0f/%.0f/%.0f",
		CmdZCMoveZoneOK, len(out), m.dest.IP, m.dest.Port, m.gate.ToID, m.gate.To,
		m.pos[0], m.pos[1], m.pos[2], m.gate.TargetDirByte, channelByte(m.dest.Channel),
		m.gate.Lat, m.gate.Long, m.gate.ZoomMin, m.gate.ZoomMax, m.gate.InitZoom)
	return [][]byte{out}
}

// gateReady answers CZ_MOVE_ZONE_OK (3686): it clears the pending move, then finishes it
// with Handler.acceptMove.
//
// This is the ONLY path that finishes a map change. There is no timeout beside it, on
// purpose: with the 3593 flag byte right the client acks at once, exactly like the live
// server (D:\ge_server\captures\warp_ref\20261027-200136\capture.txt lines 593-595: 3593, then the
// ack, then 3594 in the same millisecond). A timeout would have completed the handshake
// the client refused and hidden the real bug.
//
// The HANDOVER DEADLINE (Handler.armHandoff) starts AFTER this function, on the handoff
// the 3594 names, and it is the opposite kind of timer: it never completes anything, it
// gives a move up and refunds the scroll when the destination never takes the character.
func (h *Handler) gateReady(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	if len(f.Data) < moveZoneAcceptSize {
		h.Log.Printf("zone: MOVE_ZONE_OK is %d bytes, want %d - ignored",
			len(f.Data), moveZoneAcceptSize)
		return nil, nil
	}
	m := sess.Gate
	if m == nil {
		h.Log.Printf("zone: MOVE_ZONE_OK with no map change pending - ignored")
		return nil, nil
	}
	// A PAID-FOR MOVE IS RE-CHECKED AT THE ACK. The destination answered a few
	// milliseconds ago, before the charge; if it has stopped answering since, the next
	// thing this zone would do is tell the client to reconnect to a closed port. This is
	// the last moment the scroll can still be given back to a player who is still here,
	// so a jump whose town died in that window costs nothing.
	if m.warpClass != 0 {
		if derr := world.Probe(m.dest, world.ProbeTimeout); derr != nil {
			sess.Gate = nil
			return h.refundAfterAck(ctx, sess, m, derr), nil
		}
	}
	// Cleared here, not after the write: one 3593 gets one 3594, and a repeated 3686
	// must not leave a second handoff for the same account.
	sess.Gate = nil
	h.Log.Printf("zone: MOVE_ZONE_OK (cmd %d, %d bytes) from account %d after %s - "+
		"answering for %s -> map %d (%s)", CmdCZMoveZoneOK, len(f.Data),
		sess.AccountID, time.Since(m.since).Round(time.Millisecond), m.gate.Name,
		m.gate.ToID, m.gate.To)
	return h.acceptMove(sess, m), nil
}

// refundAfterAck is the Warp List's "paid, the client was ready, and the destination
// died anyway" path: the ONE 3686 arrived, and a fresh dial says the town that was
// answering at the charge no longer answers.
//
// WHAT IT DOES. The scroll goes back in its own transaction (refundWarpScroll, ONE
// ZC_ADD_ITEM 3607 whose count is the row's NEW TOTAL - the database row alone is not
// enough, the client would keep drawing the lower count), then ONE ZC_MESSAGE says so,
// and NOTHING else is sent: no 3594 and no handoff file, because naming a dead address
// is exactly the bug this path exists to refuse.
//
// WHAT IT CANNOT DO. The 3593 -> 3686 gap has no timer anywhere (see gateTick and
// gateReady): a client that never sends 3686 at all still sits with its 3593, and that
// case is NOT covered here - this one starts from the ack. The handover deadline
// (Handler.armHandoff) does not cover it either: that one starts after the 3594, on the
// handoff the destination never took. The client that did ack is
// waiting for a 3594 it will not get; the message and the bag line are all this side can
// say, and stopping short of the move is better than sending the player to a closed
// port.
func (h *Handler) refundAfterAck(ctx context.Context, sess *Session, m *gateMove, why error) [][]byte {
	given, note := h.refundWarpScroll(ctx, sess, m.warpClass)
	h.Log.Printf("zone: WARP -> map %d (%s) for account %d: the client answered cmd %d but "+
		"%s:%d STILL DOES NOT ANSWER (%v) - the destination died between the charge and "+
		"the handoff; %s; the player was told with one cmd %d line; no cmd %d and no "+
		"handoff were sent", m.gate.ToID, m.gate.To, sess.AccountID, CmdCZMoveZoneOK,
		m.dest.IP, m.dest.Port, why, note, CmdZCMessage, CmdZCMoveZoneOK)
	out := [][]byte{}
	if given != nil {
		out = append(out, given)
	}
	return append(out, BuildMessage(warpTravelRefusedText, MessageTypeGeneral, MessageLevelGeneral))
}

// channelByte narrows a registry channel to the one byte the packet has. A channel
// is 0 today (one zone process per map), and a copy of the record's own number is
// still better than a constant: it cannot disagree with the announcement.
func channelByte(channel int) uint8 {
	if channel < 0 {
		return 0
	}
	if channel > 255 {
		return 255
	}
	return uint8(channel)
}

// arrivalFormation puts this map's own arrival SHAPE on a gate's exit point.
//
// The gate data carries the exit POINT and its cell, not a formation (332 gates, and
// no per-gate slots): the shape is the map's own, the same three offsets the emitter
// writes for every spawn and town point. So it is taken from the map's spawn
// formation and moved onto the exit by formationAround (mapdata.go), which is the ONE
// place that does that - a Warp List jump to a saved point on this map uses the same
// function and differs only in its cell rule.
//
// NO cell resolver is passed, so every slot carries the EXIT's own cell: a gate row has
// one cell and no per-slot ground, and a slot left without one would stand that member on
// the exit point instead of beside it.
//
// An empty shape (a version-1 file, or a -spawn override) answers nil, and formationSlot
// then stands everybody on the exit point - which is what this server did before the
// formation existed, and the honest answer when the data says nothing.
func arrivalFormation(shape []FormationSlot, spawn, exit [3]int32) []FormationSlot {
	return formationAround(shape, spawn, exit, nil)
}

// TransferMaxAge is how long a handoff file may be used. It bounds the damage of a
// file left behind by a crash: a player who logs in an hour later must get the map's
// own spawn, not a gate they walked through before the server died.
const TransferMaxAge = 60 * time.Second

// Transfer is what the OLD zone leaves for the NEW one: where this account appears on
// the destination map.
//
// It is keyed by ACCOUNT, because the destination zone knows the account from
// CZ_START_GAME and nothing else - the client carries no "I came from a gate" flag.
// At is Python-style unix seconds as a float, the same as the presence files, so the
// format reads the same way as the rest of out\zones.
type Transfer struct {
	AccountID uint32  `json:"account_id"`
	MapID     int     `json:"map_id"`
	X         int32   `json:"x"`
	Y         int32   `json:"y"`
	Cell      int32   `json:"cell"` // a navmesh triangle index, never a height
	Dir       uint8   `json:"dir"`  // the direction BYTE the arrival uses
	At        float64 `json:"at"`
}

// TransferPath is the handoff file of one account. Empty dir means world.DefaultDir -
// the folder the zone registry lives in, because both zones must agree on it without a
// new flag.
//
// The name does not collide with a presence file (zone_<port>.json): a reader looking
// for zones finds a record with no port and skips it, in Go and in Python alike.
func TransferPath(dir string, accountID uint32) string {
	if dir == "" {
		dir = world.DefaultDir
	}
	return filepath.Join(dir, fmt.Sprintf("transfer_%d.json", accountID))
}

// transferDir is the folder this handler writes handoffs to and reads them from.
func (h *Handler) transferDir() string {
	if h.Transfers != "" {
		return h.Transfers
	}
	return world.DefaultDir
}

// registryDir is the folder the registry reads, for a log line. It applies the same
// default world.Registry itself does, so the line names the folder that was really
// looked in.
func registryDir(r *world.Registry) string {
	if r == nil || r.Dir == "" {
		return world.DefaultDir
	}
	return r.Dir
}

// writeHandoff leaves the arrival point for the destination zone, and says so. It
// answers the file it wrote, because the handover deadline (Handler.armHandoff) is a
// deadline on THAT file.
func (h *Handler) writeHandoff(accountID uint32, m *gateMove) (string, error) {
	path := TransferPath(h.transferDir(), accountID)
	t := Transfer{
		AccountID: accountID, MapID: m.gate.ToID,
		X: m.pos[0], Y: m.pos[1], Cell: m.pos[2], Dir: m.gate.TargetDirByte,
		At: float64(time.Now().UnixNano()) / 1e9,
	}
	if err := WriteTransfer(path, t); err != nil {
		return "", err
	}
	h.Log.Printf("zone: HANDOFF written to %s: account %d arrives on map %d at "+
		"(%d, %d, cell %d), dir byte %d, from %s",
		path, t.AccountID, t.MapID, t.X, t.Y, t.Cell, t.Dir, m.gate.Name)
	return path, nil
}

// WriteTransfer writes one handoff atomically: temp file, then rename, so the
// destination zone can never read half a file. The temp name ends in ".tmp" and not in
// ".json" - the same trick as the presence file, and the reason a reader looking while
// a write is in flight skips it.
func WriteTransfer(path string, t Transfer) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	blob, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal handoff for account %d: %w", t.AccountID, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// ReadTransfer answers the arrival point a previous zone left for this account on this
// map, and reports WHY when it will not use one.
//
// The four conditions are the whole safety of the handoff, and each is a separate
// sentence in the log because each is fixed a different way:
//
//   - the file exists (no file is the normal entry from the barrack);
//   - it names THIS account (a second player's file must never place mine);
//   - it names THIS map (the gate you walked through is not the map you asked for);
//   - it is younger than TransferMaxAge (a file from a crashed session must not
//     teleport anyone).
//
// A cell that is not a plausible triangle index is refused by the same guard as
// ParseSpawn: a hand-written or corrupted file must not be able to send a height.
//
// It does NOT delete the file: the caller uses the point first and then calls
// RemoveTransfer, so a crash between the two costs a re-use of a fresh point, never a
// silent arrival at the spawn.
func ReadTransfer(path string, accountID uint32, mapID int, now time.Time) (Transfer, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Transfer{}, fmt.Errorf("no handoff file for account %d at %s - this "+
				"entry did not come through a gate", accountID, path)
		}
		return Transfer{}, fmt.Errorf("read %s: %w", path, err)
	}
	var t Transfer
	if err := json.Unmarshal(blob, &t); err != nil {
		return Transfer{}, fmt.Errorf("%s is not a handoff file: %w", path, err)
	}
	if t.AccountID != accountID {
		return Transfer{}, fmt.Errorf("%s was written for account %d, not %d - ignored",
			path, t.AccountID, accountID)
	}
	if t.MapID != mapID {
		return Transfer{}, fmt.Errorf("%s leads to map %d, but this zone serves map %d - "+
			"ignored", path, t.MapID, mapID)
	}
	if t.At == 0 {
		return Transfer{}, fmt.Errorf("%s carries no timestamp - ignored", path)
	}
	age := float64(now.UnixNano())/1e9 - t.At
	if age > TransferMaxAge.Seconds() {
		return Transfer{}, fmt.Errorf("%s is %.0fs old (the limit is %.0fs) - ignored",
			path, age, TransferMaxAge.Seconds())
	}
	if age < -TransferMaxAge.Seconds() {
		return Transfer{}, fmt.Errorf("%s is dated %.0fs in the FUTURE - ignored", path, -age)
	}
	if !PlausibleCell(t.Cell) {
		return Transfer{}, fmt.Errorf("%s: %w", path, badCellError(t.Cell))
	}
	return t, nil
}

// RemoveTransfer deletes a handoff that has been used, so the point cannot be applied
// twice. A file that is already gone is not an error: the caller has the answer.
func RemoveTransfer(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HandoffDeadline is how long a PAID map change's handoff may sit unconsumed before the
// jump is given back (see Handler.armHandoff).
//
// WHY 5 SECONDS. The reference finishes the whole handover in one millisecond
// (captures\warp_ref\20261027-200136\capture.txt lines 593-595: 3593, the client's
// 3686, then 3594), and our client's own next step is a TCP reconnect plus ONE
// START_GAME - one connect and one packet on loopback or a LAN, well under 100 ms. Five
// seconds is fifty times that, so a merely SLOW handover is never refunded behind the
// player's back, and it is twelve times BELOW TransferMaxAge (60 s), so the two windows
// cannot overlap: a handoff this deadline refunds can never be picked up later as a
// "fresh" one. A longer deadline would hold a spent scroll longer than needed; a
// shorter one risks refunding a jump that was only slow.
const HandoffDeadline = 5 * time.Second

// handoffWait is ONE paid map change in the seconds after its 3594 went out: the
// handoff file this zone left, and the single decision "did the destination TAKE the
// character?".
//
// TWO WAKERS, ONE DECISION. The session's own tick asks the question while the client
// is still on this socket (gateTick -> Handler.handoffExpired); one goroutine asks it
// after the deadline for the case that matters most - the client read the 3594,
// reconnected, and THIS session is gone. They must never both act, so `done` is the
// single-shot latch and the file claim (claimTransfer) is the second, filesystem-level
// one. See Handler.expireHandoff.
type handoffWait struct {
	sess    *Session
	path    string
	written time.Time
	dest    world.Record
	gate    MapGate
	class   int64
	done    atomic.Bool
}

// armHandoff starts the deadline on one handoff this zone has just written. Its only
// caller is Handler.acceptMove, and only for a move that was PAID FOR: a free move
// cannot eat a scroll, so there is nothing to give back.
//
// THE TRAP THIS EXISTS FOR. A successful dial only proves the port ANSWERS. A town whose
// process accepts the connection and then never finishes the handover used to be
// indistinguishable from a working one: the scroll was spent, the 3594 went out, the
// handoff stayed on disk, and NOTHING timed out - the player stood where he was with one
// scroll fewer. The deadline is measured from the handoff file's own `At` (writeHandoff
// writes it), so the file and the timer cannot disagree about when the jump started.
func (h *Handler) armHandoff(sess *Session, m *gateMove, path string) {
	w := &handoffWait{
		sess: sess, path: path, written: time.Now(), dest: m.dest, gate: m.gate,
		class: m.warpClass,
	}
	sess.Handoff = w
	go h.watchHandoff(w)
}

// watchHandoff is the deadline itself: one timer, one decision, then it ends.
//
// WHY A GOROUTINE AND NOT ONLY THE SESSION'S TICK. The usual failure this covers is this
// session CLOSING: the client reads the 3594, reconnects to the destination, and this
// socket ends - the tick goes with it, and a scroll spent on a town that never took the
// character would have nobody left to notice. The timer outlives the socket.
func (h *Handler) watchHandoff(w *handoffWait) {
	wait := HandoffDeadline - time.Since(w.written)
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	<-timer.C

	pkts := h.expireHandoff(w)
	if len(pkts) == 0 {
		return
	}
	if err := h.writeSession(w.sess, pkts); err != nil {
		// The expected case here: the player is not on this socket any more, so the one
		// message cannot be delivered. The refund is already in the database; say that
		// instead of pretending the line was seen.
		h.Log.Printf("zone: WARP -> map %d (%s): the refund for account %d is in the "+
			"database, but the one cmd %d line could not be sent (%v) - the player "+
			"sees the scroll again the next time the bag is loaded",
			w.gate.ToID, w.gate.To, w.sess.AccountID, CmdZCMessage, err)
	}
}

// expireHandoff answers the ONE question the deadline asks: did the destination TAKE the
// handoff by now? It answers with the packets owed to the player, or nil for "nothing is
// owed".
//
// THE FILE IS THE ANSWER, AND THE CLAIM IS THE ARBITRATION. The destination removes the
// handoff while placing the team (placeTeam -> ClaimTransfer), so "the file is still
// there" means "the town never took the character". This side then RENAMES the file away
// (claimTransfer) before touching the scroll - it does not read it and delete it later -
// and the destination claims it by the SAME rename. One rename wins, so the two outcomes
// cannot both happen:
//
//   - the destination renamed first: the claim here fails with "does not exist", nothing
//     is refunded, and the jump stands;
//   - this side renamed first: the destination can no longer find the point, so a
//     START_GAME that arrives LATE lands on the map's own spawn and not on the gate's
//     exit - the character is never moved to the place a refunded scroll paid for;
//   - both at once: exactly one rename succeeds, hence at most one refund.
//
// `done` closes the other half. The tick and the timer goroutine may both call this for
// one jump; the loser returns nil at once, with no second refund, no second message and
// no second log line.
//
// WHAT A REFUNDED JUMP SENDS, AND WHAT IT MUST NOT. One ZC_ADD_ITEM (the scroll, whose
// count is the row's NEW total - the client would otherwise keep drawing the lower one),
// then ONE ZC_MESSAGE saying the move failed and nothing was charged. It sends NO 3594:
// the one 3594 went out before this deadline began (it is what tells the client to
// reconnect - there is nothing left to withhold), and a SECOND one would name a
// destination for a move that has just been refused. It also leaves NO handoff for a
// later arrival: the file it refunded is gone from its own name.
func (h *Handler) expireHandoff(w *handoffWait) [][]byte {
	if !w.done.CompareAndSwap(false, true) {
		return nil
	}
	claimed, err := claimTransfer(w.path, ".expired")
	if err != nil {
		if os.IsNotExist(err) {
			// The destination took it: the normal, successful handover. Nothing to say.
			return nil
		}
		// The file is there but cannot be taken off its name (a locked or read-only
		// folder). Do NOT refund: the destination may still read it, and a refund plus a
		// gate exit would be a free jump. The line is explicit so the case is fixable.
		h.Log.Printf("zone: WARP -> map %d (%s) for account %d: the handoff %s could not "+
			"be claimed at the %s handover deadline (%v), so NOTHING was refunded - the "+
			"destination may still use it; check the folder's permissions",
			w.gate.ToID, w.gate.To, w.sess.AccountID, w.path, HandoffDeadline, err)
		return nil
	}
	_ = os.Remove(claimed)

	given, note := h.refundWarpScroll(context.Background(), w.sess, w.class)
	h.Log.Printf("zone: WARP -> map %d (%s) for account %d: the handoff %s was NOT taken "+
		"by %s:%d within the %s HANDOVER DEADLINE (written %s ago) - the destination "+
		"answered the dial but never finished the handover; %s; the player was told "+
		"with one cmd %d line, no second cmd %d was sent and no handoff was left",
		w.gate.ToID, w.gate.To, w.sess.AccountID, w.path, w.dest.IP, w.dest.Port,
		HandoffDeadline, time.Since(w.written).Round(time.Millisecond), note,
		CmdZCMessage, CmdZCMoveZoneOK)

	out := [][]byte{}
	if given != nil {
		out = append(out, given)
	}
	return append(out, BuildMessage(warpTravelRefusedText, MessageTypeGeneral, MessageLevelGeneral))
}

// handoffExpired is the SESSION-side waker: the deadline checked from gateTick, on the
// session's own goroutine, while the client is still on this socket. That is the case
// where the one message really reaches the player.
//
// It also clears the session's pending handoff, so the tick stops asking (and
// Session.wantsTick stops waking the read loop) once the one decision has been made -
// whatever the answer was.
func (h *Handler) handoffExpired(sess *Session) [][]byte {
	w := sess.Handoff
	if w == nil {
		return nil
	}
	if time.Since(w.written) < HandoffDeadline {
		return nil
	}
	sess.Handoff = nil
	return h.expireHandoff(w)
}

// claimTransfer renames a handoff file away from its own name and answers the new name:
// the ONE atomic step two processes can race on (see Handler.expireHandoff).
func claimTransfer(path, suffix string) (string, error) {
	claim := path + suffix
	if err := os.Rename(path, claim); err != nil {
		return "", err
	}
	return claim, nil
}

// ClaimTransfer is ReadTransfer plus the one thing the handover deadline needs: the file
// is TAKEN OFF ITS NAME as the point is read, so an arrival and the deadline's refund
// cannot both act on the same handoff (see Handler.expireHandoff).
//
// The rename happens FIRST, before any of ReadTransfer's checks, on purpose: a check
// followed by a delete is exactly the window this closes. It is safe to claim a file that
// then FAILS a check, because a handoff is named `transfer_<account>.json` - the file this
// reader is looking at can only be this account's own, so a wrong map or an old timestamp
// means it can never be used here, and dropping it is the correct answer (a foreign
// player's file has a different name and is never touched).
func ClaimTransfer(path string, accountID uint32, mapID int, now time.Time) (Transfer, error) {
	claim, err := claimTransfer(path, ".claimed")
	if err != nil {
		if os.IsNotExist(err) {
			return Transfer{}, fmt.Errorf("no handoff file for account %d at %s - this "+
				"entry did not come through a gate, or its jump was REFUNDED at the "+
				"handover deadline (zone\\gate.go, HandoffDeadline)", accountID, path)
		}
		return Transfer{}, fmt.Errorf("take %s: %w", path, err)
	}
	defer func() { _ = os.Remove(claim) }()
	return ReadTransfer(claim, accountID, mapID, now)
}
