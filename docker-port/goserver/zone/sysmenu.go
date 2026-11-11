package zone

// THE SYSTEM OPTION MENU'S OTHER TWO BUTTONS (2026-11-09).
//
// The client's System Option window sends ONE packet for all three entries - CZ_SYSMENU 3595
// with a selection byte - and the reference answers them in `CCommander::OnSysMenu`
// (source2\geServer\ZServerLib\Commander_Packet.cpp:734-765):
//
//	case SYSMENU_BARRACK:   RequestDestroy(true)                     // "To Barrack"
//	case SYSMENU_LOGOUT:    RequestDestroy()                         // "Logout"  [already done]
//	case SYSMENU_LOBBYZONE: TransferRandomLobbyZone()                 // "To the Save Point"
//	                        else TransferBarrack()
//
// `TransferRandomLobbyZone` (Commander.cpp:4022-4050) is the save-point move and it is NOT a
// warp table of its own: it reads the commander's `RestartWarpID`, takes the WARP ROW's target
// map, and transfers the team to that map with `TransferZone`. If the target is the map the
// player is already on it answers "can't warp to the same zone" and moves nobody. `TransferBarrack`
// (Commander.cpp:1225-1240) is the fallback: it sends ZC_MOVE_BARRACK (3597) and marks the
// session as logging out.
//
// OUR THREE PIECES, all of them already in this project:
//
//	the saved point     `commander.point` - a WARP ID, read through Handler.SaveWarpIDOf
//	the warp -> map     ZonePoints.RestartWarpMap (zone\mapdata.go), the reverse lookup the
//	                    barrack already uses for the same number
//	the move            BuildMoveZoneOK + the destination zone's own spawn (ZonePoints.SpawnFor)

import (
	"context"
	"strconv"

	"geserver/protocol"
)

const (
	// CmdZCMoveBarrack is ZC_MOVE_BARRACK: cmd(2) + a 2-byte pad, the client's own
	// registered size. It tells the client to switch to the barrack (character select).
	CmdZCMoveBarrack   = 3597
	sysMenuToBarrack   = 0
	sysMenuToSavePoint = 2
)

// BuildMoveBarrack is ZC_MOVE_BARRACK: two bytes, the command and nothing else. The
// reference sends exactly this from CCommander::TransferBarrack (Commander.cpp:1236).
func BuildMoveBarrack() []byte {
	return []byte{byte(CmdZCMoveBarrack & 0xFF), byte(CmdZCMoveBarrack >> 8)}
}

// sysMenuBarrack answers selection 0 ("To Barrack").
//
// The client has ALREADY switched itself to barrack mode by the time this arrives
// (GENormalCommand.cpp:73-83: CZ_SysMenu, then SetSwitchTarget(MST_SWITCH_BARRACK_MODE)),
// so this is not what makes it leave - it is what lets the client finish the switch without
// waiting to be disconnected. The LAST POINT has already been saved by the caller.
func (h *Handler) sysMenuBarrack(sess *Session) [][]byte {
	// TWO PACKETS, AND THE ORDER IS THE CLIENT'S OWN RULE. The Python oracle had already
	// written it down (src\zone_server.py, build_logout_ok): the client only acts on the
	// ZC_MOVE_BARRACK switch target once `CNormalMode::m_isAbleToLogout` is true, and ONLY
	// the ZC_LOGOUT_OK handler sets it (GENormalNet::LogoutOK -> NormalMode()->EnableLogout()).
	// Sending 3597 alone made the client wait for ever and then drop at the login screen -
	// which is what the owner saw twice.
	logout := BuildLogoutOK()
	out := BuildMoveBarrack()
	h.Log.Printf("zone: CZ_SYSMENU selection %d (BARRACK) from account %d on map %d: "+
		"answering ZC_LOGOUT_OK (cmd %d) then ZC_MOVE_BARRACK (cmd %d, %d bytes) - the "+
		"client switches to character select and closes this socket", sysMenuToBarrack,
		sess.AccountID, h.MapID, CmdZCLogoutOK, CmdZCMoveBarrack, len(out))
	sess.CloseAfterAnswer = true
	return [][]byte{logout, out}
}

// sysMenuSavePoint answers selection 2 - the client's "To the Save Point".
//
// It moves the team to the map of the character's SAVED START POINT - the same warp id the
// barrack shows as the character's save point - and lands on that map's own spawn row. A
// player already standing on that map is told so and moved nowhere, which is the reference's
// behaviour (MESSAGE_TYPE_CANT_WARP_SAME_ZONE) rather than a silent no-op.
func (h *Handler) sysMenuSavePoint(ctx context.Context, sess *Session) ([][]byte, error) {
	if h.SaveWarpIDOf == nil {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from account %d: this zone "+
			"has no save-point reader wired (Handler.SaveWarpIDOf is nil), so nothing was "+
			"done", sysMenuToSavePoint, sess.AccountID)
		return nil, nil
	}
	warpID, found, err := h.SaveWarpIDOf(ctx, sess.CommanderID)
	if err != nil {
		return nil, err
	}
	if !found || warpID == 0 {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: no saved "+
			"start point (commander.point is 0) - there is no save point to move to, and "+
			"nothing was sent", sysMenuToSavePoint, sess.CommanderID)
		return nil, nil
	}
	mapID, ok := h.ZonePoints.RestartWarpMap(warpID)
	if !ok {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: saved "+
			"warp id %d names no map's restart row - nothing was sent", sysMenuToSavePoint,
			sess.CommanderID, warpID)
		return nil, nil
	}
	if mapID == h.MapID {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: the saved "+
			"warp %d IS this map (%d) - the reference refuses the same-zone warp and so does "+
			"this: nothing was sent", sysMenuToSavePoint, sess.CommanderID, warpID, mapID)
		return nil, nil
	}
	landing, err := h.ZonePoints.SpawnFor(mapID)
	if err != nil {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: warp %d "+
			"names map %d but that map has no landing: %v", sysMenuToSavePoint, sess.CommanderID,
			warpID, mapID, err)
		return nil, nil
	}
	dest, live := h.Zones.Address(mapID, 0)
	if !live {
		h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: map %d "+
			"(%s) has no live zone yet - asking the manager for it; this press is refused "+
			"rather than dropping the player on a dead address", sysMenuToSavePoint,
			sess.CommanderID, mapID, landing.Map)
		if err := h.Zones.Request(mapID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	// The LAST POINT belongs to the map being LEFT, exactly as a gate crossing writes it.
	h.noteLastPoint(sess, lastPointMapChange)

	// THE DESTINATION'S OWN CAMERA, from its restart warp row: a move that carries zeros
	// collapses the client's zoom (the owner: "it mess my camera zoom"). A table emitted
	// before 2026-11-09 has no camera on the restart row, and then these stay zero - the
	// client keeps its own, which is the lesser evil to inventing numbers.
	var cam RestartPoint
	if entry, ok := h.ZonePoints.Maps[strconv.Itoa(mapID)]; ok && entry.Restart != nil {
		cam = *entry.Restart
	}
	out := BuildMoveZoneOK(MoveZoneDest{
		InstID: uint32(mapID), IP: dest.IP, Port: dest.Port, Map: landing.Map,
		Lat: cam.Lat, Long: cam.Long, ZoomMin: cam.ZoomMin, ZoomMax: cam.ZoomMax,
		InitZoom: cam.InitZoom, Pos: landing.Pos, Dir: 0, Channel: channelByte(dest.Channel),
	})
	h.Log.Printf("zone: CZ_SYSMENU selection %d (SAVE POINT) from commander %d: saved warp "+
		"%d -> map %d (%s), sending MOVE_ZONE_OK (cmd %d, %d bytes) to %s:%d at (%d, %d, "+
		"cell %d)", sysMenuToSavePoint, sess.CommanderID, warpID, mapID, landing.Map,
		CmdZCMoveZoneOK, len(out), dest.IP, dest.Port, landing.Pos[0], landing.Pos[1],
		landing.Pos[2])
	return [][]byte{out}, nil
}

// sysMenuFrame is the dispatcher-facing half: it keeps last_point.go's sysMenu readable and
// gives the two new selections one entry point.
func (h *Handler) sysMenuSelection(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	selection, ok := ParseSysMenu(f.Data)
	if !ok {
		return nil, nil
	}
	switch selection {
	case sysMenuToBarrack:
		return h.sysMenuBarrack(sess), nil
	case sysMenuToSavePoint:
		return h.sysMenuSavePoint(ctx, sess)
	}
	return nil, nil
}
