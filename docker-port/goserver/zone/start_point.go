package zone

import (
	"context"
	"encoding/binary"
	"fmt"

	"geserver/protocol"
)

// THE WAYPOINT GRID'S SAVE BUTTON - "Save as Start Point".
//
// The owner's own words (2026-10-29): "the waypoint save function is to change your
// saved location/position/zone/map when u enter from barrack." That is the whole
// requirement, and this file is the two halves of it:
//
//   - savedStartPoint, below: the ENTRY side. Read once per START_GAME and put the
//     team on the saved town's own point.
//   - saveRestartWarp: the BUTTON side. Write the map and the warp id, then answer
//     the client so its window can show the new start point.
//
// THE SAVE POINT AND THE LAST POINT ARE TWO DIFFERENT FACTS AND ARE STORED TWICE.
// This file owns the save point (`commander.point`, a restart warp id);
// zone\last_point.go owns the last point (`commander.zone_id` + `character_position`).
// They used to share `commander.zone_id`, and on 2026-10-29 a log-out in Reboldoeux
// overwrote the owner's Coimbra save that way. Nothing here writes `commander.zone_id`
// and nothing here touches `character_position`.
//
// The database half (which column, why the map is not stored beside the warp id, and
// why there is no position column at all) is zone\start_point_db.go.
//
// UseStartPoint wires the store behind the Save button: where a save is written,
// and where an entry reads it back.
//
// It is a method and not two exported fields because the hooks belong together: a
// handler with the read hook and no write hook would show a start point it cannot
// change, which is the exact failure this task exists to fix.
func (h *Handler) UseStartPoint(
	load func(ctx context.Context, commanderID int32) (StartPoint, bool, error),
	save func(ctx context.Context, commanderID int32, warpID int) (StartPoint, error),
) {
	h.StartPoint = load
	h.saveStartPoint = save
}

// savedStartPoint answers "where does this team enter" from the commander's saved
// town, and says in one log line what it did.
//
// IT IS ONE OF TWO CHOICES, AND THE BARRACK SAYS WHICH COMES FIRST. New Game (cmd 22)
// and the mission button (cmd 24) enter at the SAVE POINT first, then the Last Point;
// Continue Game (cmd 23) is the other way round. The order lives in one place,
// Handler.placeTeam (zone\zone.go), because it now depends on what the player pressed.
//
// IT NEVER INVENTS A POINT: every refusal keeps the map's own spawn (the `base` it
// was handed) and prints why. The refusals are different problems and are named
// differently:
//
//   - the store is not wired, or the read failed: the server cannot know;
//   - there is no commander on this session: there is no row to read;
//   - nothing is saved yet: the normal first login;
//   - NO MAP CARRIES THE SAVED WARP ID: the warp id alone does not answer which town
//     it is. That is a data error (a hand edit, or a spawn table that changed under an
//     old save), refused and named - this server never enters on a guessed map;
//   - the saved warp belongs to ANOTHER map: this zone serves one map, so the save
//     point cannot place the team here. NOT a data error: the warp is perfectly good,
//     it is just not this map;
//   - the warp row answers but cannot place a team (no resolved cell): refused too,
//     because a cell that is not on the mesh crashes the client inside PathEngine.
//
// The formation is moved with the point, by arrivalFormation - the same function a
// gate's arrival uses. Reusing it means the followers cannot end up at the map's
// spawn while the leader stands in the saved town. `placed` is returned for the
// caller's log line and is "the saved start point (map N, warp M)" when it moved the
// team.
func (h *Handler) savedStartPoint(ctx context.Context, sess *Session, base [3]int32,
	slots []FormationSlot) ([3]int32, []FormationSlot, string) {
	if h.StartPoint == nil {
		h.Log.Printf("zone: no start-point store is wired in, so this team has no saved "+
			"start point to enter at on map %d", h.MapID)
		return base, slots, ""
	}
	if sess.CommanderID <= 0 {
		h.Log.Printf("zone: account %d has no commander, so it has no saved start "+
			"point", sess.AccountID)
		return base, slots, ""
	}
	if h.ZonePoints == nil {
		// The warp id means nothing without the spawn table: it is the table that
		// says which map owns a restart warp, and where its row lands. NIL is a
		// startup mistake (a zone started without -points), and the line names it.
		h.Log.Printf("zone: no spawn table is loaded, so the saved start point of "+
			"commander %d cannot be resolved to a map - falling through to the Last "+
			"Point; start the zone with -points ../data/zone_points.json",
			sess.CommanderID)
		return base, slots, ""
	}

	sp, found, err := h.StartPoint(ctx, sess.CommanderID)
	switch {
	case err != nil:
		h.Log.Printf("zone: the saved start point of commander %d could not be read "+
			"(%v) - falling through to the Last Point", sess.CommanderID, err)
		return base, slots, ""
	case !found:
		// The normal first login: nothing saved yet.
		h.Log.Printf("zone: commander %d has no saved start point yet - falling "+
			"through to the Last Point", sess.CommanderID)
		return base, slots, ""
	}

	// WHICH MAP OWNS THIS WARP ID. One line, because the answer decides everything
	// below, and a warp id alone is not a town.
	mapID, ok := h.ZonePoints.RestartWarpMap(sp.WarpID)
	if !ok {
		// The warp id is not on ANY map's restart row, so there is no way to know
		// which town was meant. Refused and named as a data error: guessing a map is
		// a team in the wrong town.
		h.Log.Printf("zone: the saved start point of commander %d is a DATA ERROR - it "+
			"names restart warp %d and NO map's restart row in the spawn table carries "+
			"that warp id (%d map(s) checked). The point is REFUSED, not guessed, and "+
			"the entry falls through to the Last Point", sess.CommanderID, sp.WarpID,
			len(h.ZonePoints.Maps))
		return base, slots, ""
	}
	if mapID != h.MapID {
		// The saved point belongs to ANOTHER map. A zone serves one map, so this is
		// not a place it can put the team. NOT a data error: the warp is fine, it is
		// just not this map.
		h.Log.Printf("zone: the saved start point of commander %d is map %d (warp %d) "+
			"and this zone serves map %d, so the save point cannot place this team - "+
			"falling through to the Last Point", sess.CommanderID, mapID, sp.WarpID,
			h.MapID)
		return base, slots, ""
	}

	landing, why := h.ZonePoints.RestartLanding(mapID, sp.WarpID)
	if why != nil {
		// The warp has a map but cannot be resolved to a point: refused as a DATA
		// ERROR, and the entry falls through. Entering anyway would mean inventing a
		// spot on the map.
		h.Log.Printf("zone: the saved start point of commander %d is a DATA ERROR - it "+
			"names warp %d on map %d and that warp does not answer on that map's "+
			"restart row (%v). The point is REFUSED, not guessed, and the entry falls "+
			"through to the Last Point", sess.CommanderID, sp.WarpID, mapID, why)
		return base, slots, ""
	}
	moved := landing.Pos
	h.Log.Printf("zone: SAVED START POINT of commander %d: map %d (%s), warp %d, "+
		"landing (%d, %d, cell %d) from %s - the team enters HERE, not at the map's "+
		"own spawn", sess.CommanderID, mapID, h.MapName, sp.WarpID, moved[0],
		moved[1], moved[2], landing.Source)
	return moved, arrivalFormation(h.SpawnFormation, h.Spawn, moved),
		fmt.Sprintf("the saved start point (map %d, warp %d)", mapID, sp.WarpID)
}

// The two ids, and exactly where the bytes sit. BOTH LAYOUTS WERE PROVEN FROM THE
// SHIPPED CLIENT, not from source2 (whose tree predates this button and has neither
// id anywhere: `rg RestartWarp source2\geClient` is empty).
const (
	// saveRestartWarpSize is the whole CZ_SAVE_RESTART_WARPID packet, from the
	// client's own registered table: data\cmd_table.txt:952 `3468 14
	// PKS_CZ_SAVE_RESTART_WARPID`, agreeing with data\cmd_table_live_z.txt:1135.
	saveRestartWarpSize = 14

	// updateRestartWarpSize is 3467 ZC_UPDATE_RESTART_WARP: data\cmd_table.txt:951
	// says 6, and that is cmd(2) + one DWORD. THERE IS NO LENGTH WORD - the client's
	// handler reads the value from offset 2, not 4:
	//
	//   ge.exe FUN_004cbc78 (the 3467 handler, reached from the dispatch at
	//   0x4d76fd; 3467 - 0xd7d = 0xe -> the jump table)
	//     0x4cbc7b  MOV EAX, dword ptr [EBP + 0x8]   ; the packet
	//     0x4cbc80  PUSH dword ptr [EAX + 0x2]       ; the id, a DWORD at +2
	//     0x4cbc8a  CALL 0x005e6898                  ; store it in the client's cache
	//     0x4cbc98  PUSH 0x009d2d0c                  ; ..._SAVE_RESTART_WARP_SUCCESS
	updateRestartWarpSize = 6
	// updateRestartWarpIDAt is where that DWORD starts: right after the command word.
	updateRestartWarpIDAt = 2
)

// CmdZCUpdateRestartWarp is ZC_UPDATE_RESTART_WARP: the answer that makes the
// window show the new start point (and, on the client, silences a second Save with
// "you have already saved this point" - see BuildUpdateRestartWarp).
const CmdZCUpdateRestartWarp = 3467

// WHY THE 3468 BODY IS NOT A FIELD, AND WHY THE SERVER DECIDES THE POINT.
//
// The 14 bytes are a 10-byte security header (PKS_SECURITY_DEFAULT: cmd, seq,
// checksum - gePacketBase.h:52-56) and FOUR BYTES THE CLIENT NEVER WRITES. Its only
// builder allocates a 16-byte buffer, zeroes the seq and the checksum, writes the
// command word, and sends 14 - leaving offsets 10..13 as whatever the stack held:
//
//	ge.exe FUN_005062b2 (the 3468 builder; the only caller is FUN_0053c2e1)
//	  0x5062b5  SUB ESP, 0x10                  ; the 16-byte buffer
//	  0x5062c1  AND dword ptr [EBP + -0xe], 0  ; seq = 0        (buffer + 2)
//	  0x5062ca  AND dword ptr [EBP + -0xa], 0  ; checksum = 0   (buffer + 6)
//	  0x5062ce  MOV word ptr [EBP + -0x10], AX ; command = 0xd8c (buffer + 0)
//	  0x5062ef  CALL dword ptr [EDX + 0x20]    ; Send(buffer, 14)
//
// There is NO store to buffer + 0xa .. + 0xd anywhere in the function. A real packet
// that arrived from the shipped client proves it: logs\zone15_live.log:14879
//
//	CZ_SAVE_RESTART_WARPID (3468, 14 bytes): 8c0d 17000000 d1010000 e47d3cf6
//	                                         ^cmd  ^seq 23   ^cksum 465 ^stale stack
//
// so the tail is not a warp id, a map id or an index - and a server that READ one
// would sometimes write a random warp id into the player's start point.
//
// The client also never tells the server which point it means, even though it knows
// one: its own copy of the saved id is local state only (cache at [ECX+0x18304],
// written by FUN_005e6898 and read by FUN_005e00aa - two references in the whole
// .text, neither of them a packet builder). The button's code is
//
//	ge.exe 0x549d59  CALL 0x0068127b   ; the SELECTED point's own id (record +0x14)
//	0x549d60  CMP  ESI, -1             ; -1 = nothing selected
//	0x549d97  CMP  ESI, EAX            ; vs FUN_005e00aa = the saved id it was TOLD
//	0x549d99  JNZ  0x00549dc2          ; different -> carry on and ask the server
//	0x549da0  PUSH 0x009d0168 / 0x9def5c ; equal -> "already saved", nothing is sent
//
// So the client's only question is "is the point I am looking at the one I saved",
// and everything else is the SERVER's answer. That is the rule this handler follows:
// the point is resolved from WHERE THE CHARACTER STANDS (this zone's map), never from
// a number in the request.

// BuildUpdateRestartWarp is ZC_UPDATE_RESTART_WARP (3467): cmd, then the saved warp
// id as a DWORD, and nothing else.
//
// The id is the RESTART WARP ID (203 for Reboldoeux, 213 for Coimbra, ...), not the
// map id: it is the same number the client compares against the selected town's own
// restart warp field (0x68127b reads record +0x14), which is the column the client's
// own datatable_waypoint calls RestartWarpID - the column this server already uses to
// resolve a town's landing point. It is also the number the original server saved
// (source2\geServer\BarrackServer\Commander.cpp:589 GetRestartWarpID, and
// GetNewGameZoneInfo:378 resolves the zone and the start position from that warp row).
func BuildUpdateRestartWarp(warpID int) []byte {
	out := make([]byte, updateRestartWarpSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUpdateRestartWarp)
	binary.LittleEndian.PutUint32(out[updateRestartWarpIDAt:], uint32(warpID))
	return out
}

// ParseUpdateRestartWarp reads the id back out of a 3467 packet. It exists so the
// layout above is checkable in a test rather than only in a comment; the server never
// receives this packet.
func ParseUpdateRestartWarp(pt []byte) (warpID int, ok bool) {
	if len(pt) < updateRestartWarpSize {
		return 0, false
	}
	return int(binary.LittleEndian.Uint32(pt[updateRestartWarpIDAt:])), true
}

// saveRestartWarp answers CZ_SAVE_RESTART_WARPID (3468): the town grid's "Save as
// Start Point" button.
//
// WHAT IT WRITES, and it is the whole requirement: ONE column - `commander.point` -
// holding THIS town's restart warp id. The barrack then names this town on the New
// Game button (barrack\commander_info.go, newMapID) and the entry lands on this
// town's own point instead of the map's generic spawn (Handler.savedStartPoint).
//
// THE MAP IS NOT WRITTEN ANYWHERE, and neither is the position: the warp id alone is
// the fact, and the map and the landing spot are resolved from it (zone\start_point_db.go).
//
// IT DOES NOT WRITE `commander.zone_id` AND DOES NOT TOUCH `character_position`. Those
// belong to the LAST POINT and are written on the way out of every session; a save
// click writing them too is exactly how the owner's Coimbra save was replaced by
// Reboldoeux on 2026-10-29.
//
// ONE LINE PER SAVE names the map, the warp id and the ONE column written, so the
// window can be compared with the database without a debugger.
func (h *Handler) saveRestartWarp(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	refuse := func(what string) ([][]byte, error) {
		h.Log.Printf("zone: CZ_SAVE_RESTART_WARPID (3468, %d bytes) from account %d on "+
			"map %d: REFUSED - %s - nothing was written and nothing was sent: %x",
			len(f.Data), sess.AccountID, h.MapID, what, f.Data)
		return nil, nil
	}

	// The size is checked only to catch a client that packs something else: no VALUE
	// is read from the packet at all (see the file comment above - the tail is stale
	// stack).
	if len(f.Data) != saveRestartWarpSize {
		return refuse(fmt.Sprintf("%d bytes, want the client's registered %d (10-byte "+
			"security header plus 4 bytes it never fills); there is no field to read "+
			"either way", len(f.Data), saveRestartWarpSize))
	}
	if sess.CommanderID <= 0 {
		return refuse("the commander was never resolved for this session, so there is " +
			"no row to write")
	}
	if h.saveStartPoint == nil {
		return refuse("no start-point store is wired in")
	}
	if h.ZonePoints == nil {
		return refuse("no spawn table is loaded, so this map cannot be resolved to a " +
			"town row; start the zone with -points ../data/zone_points.json")
	}

	// This zone serves ONE map, and the reserved point is where the player stands -
	// so the map id is h.MapID and the warp id is that map's own waypoint row.
	town, found := h.ZonePoints.waypointRow(h.MapID)
	if !found {
		return refuse(fmt.Sprintf("map %d (%s) is not a row in the waypoint grid "+
			"(data\\zone_points.json's waypoints list, %d row(s)), so it has no "+
			"restart warp to save", h.MapID, h.MapName, len(h.ZonePoints.Waypoints)))
	}
	if town.Tab != 0 {
		return refuse(fmt.Sprintf("waypoint row %d (%s) is Tab %d, the FIELD grid, "+
			"and carries no restart warp", town.ID, town.Location, town.Tab))
	}
	if town.RestartWarpID <= 0 {
		return refuse(fmt.Sprintf("waypoint row %d (%s) carries no restart warp id, "+
			"so there is no point to save", town.ID, town.Location))
	}

	// The SAME resolution a town click uses (waypoint_travel.go). Without the spawn
	// fallback, on purpose: a fallback would save a point this map's warp row cannot
	// answer, and the next entry would quietly land somewhere else.
	landing, why := h.ZonePoints.RestartLanding(town.MapID, town.RestartWarpID)
	if why != nil {
		return refuse(fmt.Sprintf("map %d's own restart row cannot place a team: %v",
			town.MapID, why))
	}

	saved, err := h.saveStartPoint(ctx, sess.CommanderID, town.RestartWarpID)
	if err != nil {
		return refuse(fmt.Sprintf("the write failed: %v", err))
	}

	out := BuildUpdateRestartWarp(saved.WarpID)
	h.Log.Printf("zone: CZ_SAVE_RESTART_WARPID (3468) for commander %d: START POINT "+
		"SAVED - map %d (%s) via waypoint row %d (%s), restart warp %d, landing "+
		"(%d, %d, cell %d) from %s. WROTE commander.point = %d (map %d) and NOTHING "+
		"ELSE: commander.zone_id and character_position belong to the Last Point and "+
		"are NOT touched. Answered cmd %d, %d bytes (the warp id as a DWORD at +%d): %x",
		sess.CommanderID, town.MapID, h.MapName, town.ID, town.Location, saved.WarpID,
		landing.Pos[0], landing.Pos[1], landing.Pos[2], landing.Source, saved.WarpID,
		town.MapID, CmdZCUpdateRestartWarp, len(out), updateRestartWarpIDAt, out)
	return [][]byte{out}, nil
}
