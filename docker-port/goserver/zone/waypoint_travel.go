package zone

import (
	"encoding/binary"
	"fmt"

	"geserver/protocol"
)

// TRAVELLING BETWEEN TOWNS: CZ_WAYPOINT_SELECT (3743), a click on one thumbnail in the
// waypoint grid, and the map change it starts.
//
// The grid itself is commwarp.go's job (3742 the rows, 3753 the window). This file is
// only the CLICK, and it deliberately does NOT build a map change of its own: it resolves
// a town to a destination and calls Handler.startMoveFor, the same sender
// Handler.gateTick uses for a doorway. One 3593 goes out, the client answers 3686, and
// Handler.gateReady -> Handler.acceptMove builds the 3594 and writes the handoff, exactly
// as it does for a gate (see gate.go's file comment for the whole handshake).
//
// ---------------------------------------------------------------------------
// THE WIRE LAYOUT of 3743, from the CLIENT'S OWN CODE
// ---------------------------------------------------------------------------
//
// The sender is `packet::CZ_Waypoint_Select`, called from the grid's script binding:
//
//	source2\geClient\Source\script.cpp:1655-1663
//	  void WaypointSelect(int index)
//	  {
//	      WAYPOINT_INFO * pWpInfo = Session().WAYPOINT_LIST.Element(index);
//	      int myVis               = UISession()->GetVisInItem();
//	      if (pWpInfo->price > myVis)
//	          ui::MsgBox(... UI_ALERT_MSG_220, UI_ALERT_MSG_24);   // "not enough Vis"
//	      else
//	          packet::CZ_Waypoint_Select(pWpInfo->mapID);
//	  }
//
// Two facts come out of those nine lines and both matter here:
//
//  1. WHAT IS SENT IS THE MAP ID, not the grid index. The body is the record's own
//     `mapID`, so the zone never has to know which thumbnail is which, and a re-ordered
//     ZC_WAYPOINT_LIST cannot make a click travel to the wrong town.
//  2. THE PRICE CHECK IS ALREADY DONE BY THE CLIENT (line 1660). A player who cannot
//     afford the trip never sends 3743 at all, which is why this slice charges nothing -
//     see the FARE note at the bottom of this comment.
//
// The builder itself:
//
//	source2\geClient\Source\GENormalSendPacket.cpp:772-779
//	  void packet::CZ_Waypoint_Select(int mapId)
//	  {
//	      CHECK_SEND_CONDITION PKT_DECL (CZ_WAYPOINT_SELECT)
//	          packet.mapID = mapId;
//	      SEND_PKT
//	  }
//
// and the struct it fills:
//
//	source2\geShared\include\geZCPacket.h:2525-2527
//	  struct PKS_CZ_WAYPOINT_SELECT : public PKS_SECURITY_DEFAULT {
//	      MAP_ID  mapID;
//	  };
//	source2\geShared\include\geSharedTypes.h:45
//	  typedef WORD MAP_ID;
//	source2\geShared\include\gePacketBase.h:52-56
//	  struct PKS_SECURITY_DEFAULT { WORD command; DWORD seq; int checksum; };
//
// So: a 10-byte security header, then the map id as a WORD at offset 10.
//
// ONE DISAGREEMENT, LEFT VISIBLE ON PURPOSE. source2's struct is 12 bytes
// (10 + 2), and the SHIPPED client's own registered size for this command is 13
// (data\cmd_table_live_z.txt: 3743  13  CZ_WAYPOINT_SELECT - the same table that
// makes 3743 a FIX packet at all). The header size is not in doubt: CZ_ROTATE is
// 12 = 10 + BYTE + BYTE and CZ_SAVE_ITEM_SHORTCUT is 19 = 10 + BYTE + DWORD + DWORD,
// both exactly source2's packed struct. So this build of the client has ONE byte in
// this packet that source2's struct does not name, and it sits after the map id.
// The map id is still the first body field, so it is read at offset 10 as a WORD and
// the extra byte is ignored - and EVERY 3743's bytes are printed in hex in the log
// line below, so if that guess is ever wrong the next session sees it as a wrong
// number in the log instead of a player reporting "the wrong town".
const (
	// waypointSelectSize is the whole packet, from the client's table.
	waypointSelectSize = 13

	// waypointSelectMapID is where the map id sits: right after the security
	// header, as a WORD.
	waypointSelectMapID = 10

	// waypointSelectMapIDLen is sizeof(MAP_ID) = sizeof(WORD).
	waypointSelectMapIDLen = 2
)

// currentWaypointText is the LOG NOTE sent with the waypoint "already here" box,
// copied from the original server's own argument
// (source2\geServer\ZServerLib\Commander_Packet.cpp:2028,
// SendMessage(MESSAGE_TYPE_CURRENT_WAY_POINT, MESSAGE_LEVEL_WARNING, "current way
// point")). It is NOT what the player reads: for every type except 0 the client
// overwrites the message from its own table (GEPacket.cpp:210-211), and the
// waypoint type resolves UI_ALERT_MSG_203 -> 10000205, "You have chosen the
// waypoint of the current area." (clientmessages.xml:214, dictionary_local.xml:207).
// The note is still what goes on the wire, so it says in the log what the player
// was told in the box.
//
// THE TYPE IS MEASURED (2026-10-30 evening, TYPE-HUNT -> MEASURED): 147, not the
// source's 136. See zone\message.go for the ladder the owner ran with `/typemsg`.
// This path was a cyan chat line for a few hours while the number was unknown, and
// the lobby sentences it drew at 136 and at 135 are why it is a box again now.
const currentWaypointText = "current way point"

// ParseWaypointSelect reads the town's map id out of CZ_WAYPOINT_SELECT.
//
// ok is false only when the packet cannot hold the word, which is a malformed client
// and not a crash. The value is NOT range-checked: the refusal belongs where the map id
// is resolved, so the log line can say what the number was.
func ParseWaypointSelect(pt []byte) (mapID int, ok bool) {
	if len(pt) < waypointSelectMapID+waypointSelectMapIDLen {
		return 0, ok
	}
	return int(binary.LittleEndian.Uint16(pt[waypointSelectMapID:])), true
}

// waypointLanding is where a team arrives in a town, and which row said so.
//
// Source is for the LOG, never for the wire: when a player comes out at the wrong place,
// the line that says WHICH row made the decision is what makes it fixable.
type waypointLanding struct {
	Pos      [3]int32
	Source   string
	FromWarp bool // true = the town's restart warp row; false = the spawn fallback
}

// WaypointLanding resolves a town row to the point the team lands on.
//
// THE RULE (task R57b): a town's arrival point is the RESTART WARP ROW it names, not the
// map's generic spawn. `datatable_waypoint.RestartWarpID` is a row of the client's Warp
// table (data\warp.xml), and the destination map's own `restart` row in
// data\zone_points.json carries that warp id WITH a resolved navmesh cell (warp 203 on
// map 10 -> 3946,193 cell 146; warp 213 on map 15 -> 2268,16931 cell 233).
//
// The fallback is SpawnFor(mapID), and it is never silent: the sentence it returns says
// WHY the warp row could not answer. A number invented here would put the team through
// the mesh, which is the PathEngine crash this data file exists to prevent.
//
// The original server lands a waypoint trip on the MAP table's own `WayPointX/Y/Z`
// (Commander.cpp:2873, `info.point.Set(mapProperty->wayPoint[...])`), which is a
// DIFFERENT point from the restart warp row - measured on map 10: map waypoint (4953, 0)
// against warp 203's (3946, 193). That field carries no navmesh cell, so this slice uses
// the warp row and records the difference in history\zone-town-npcs.md.
func (z *ZonePoints) WaypointLanding(town WaypointTown) (waypointLanding, error) {
	landing, why := z.restartLanding(town.MapID, town.RestartWarpID)
	if why == nil {
		return landing, nil
	}

	fallback, err := z.SpawnFor(town.MapID)
	if err != nil {
		return waypointLanding{}, fmt.Errorf("%v, and the spawn fallback cannot place "+
			"the team either: %v", why, err)
	}
	return waypointLanding{
		Pos:    fallback.Pos,
		Source: fmt.Sprintf("the spawn fallback (%s; %v)", fallback.Source, why),
	}, nil
}

// RestartLanding is the same resolution WITHOUT the spawn fallback: the map's own
// restart row for `warpID`, or a sentence saying why it cannot answer.
//
// It exists for the SAVED START POINT (zone\start_point_db.go). The entry path
// already has a point of its own to fall back on (the map's spawn it is about to
// enter at), so a fallback INSIDE the resolver would hide the reason it did not use
// the saved town - and "the save did nothing" is exactly the sentence this task is
// about. The caller decides what to do with the refusal, and logs it.
func (z *ZonePoints) RestartLanding(mapID, warpID int) (waypointLanding, error) {
	landing, why := z.restartLanding(mapID, warpID)
	if why != nil {
		return waypointLanding{}, why
	}
	return landing, nil
}

// restartLanding is the ONE place the "does this map's restart row answer this warp
// id" question is answered. Both callers above go through it, so a town click and a
// saved start point can never land a team on two different spots.
//
// The second return is a SENTENCE rather than an error because the fallback's own
// wording quotes it; nil means "the row answered".
func (z *ZonePoints) restartLanding(mapID, warpID int) (waypointLanding, error) {
	if z == nil {
		return waypointLanding{}, fmt.Errorf("no spawn table is loaded, so map %d has "+
			"no arrival point; start the zone with -points ../data/zone_points.json",
			mapID)
	}
	entry, ok := z.Maps[fmt.Sprint(mapID)]
	if !ok {
		return waypointLanding{}, fmt.Errorf("map %d is not in the spawn table, so "+
			"there is no arrival point for it", mapID)
	}

	// Why the warp row could not answer. One sentence per case, because each one is
	// fixed a different way.
	r := entry.Restart
	switch {
	case r == nil:
		return waypointLanding{}, fmt.Errorf("map %d has no restart row in the spawn "+
			"table", mapID)
	case r.WarpID != warpID:
		return waypointLanding{}, fmt.Errorf("map %d's restart row is warp %d, not warp %d",
			mapID, r.WarpID, warpID)
	case r.Cell == nil:
		return waypointLanding{}, fmt.Errorf("warp %d's restart row on map %d has NO "+
			"resolved cell, so there is no ground to land on", r.WarpID, mapID)
	case !PlausibleCell(*r.Cell):
		return waypointLanding{}, fmt.Errorf("warp %d's restart row on map %d: %v",
			r.WarpID, mapID, badCellError(*r.Cell))
	}
	return waypointLanding{
		Pos:      [3]int32{r.X, r.Y, *r.Cell},
		Source:   fmt.Sprintf("the restart warp row %d", r.WarpID),
		FromWarp: true,
	}, nil
}

// waypointRow answers the table row for one map id, town grid or not.
//
// The lookup is by MAP ID and not by grid slot, because that is what the client sends.
func (z *ZonePoints) waypointRow(mapID int) (WaypointTown, bool) {
	if z == nil {
		return WaypointTown{}, false
	}
	for _, w := range z.Waypoints {
		if w.MapID == mapID {
			return w, true
		}
	}
	return WaypointTown{}, false
}

// travelGate builds the map change for a trip to `mapID`, landing on `pos`.
//
// ONE builder for every trip this zone starts by request: a click on a town in the
// grid (waypointSelect) and a click on a saved point (teleportTravel). The two
// differ only in WHERE the destination point came from, so the class name the
// client's SwitchMode needs, the camera the destination map's warp rows carry, and
// the point itself are resolved here once - a second copy would be a second place
// for a landing to go wrong.
//
// `fallbackClass` is the name to use when the spawn table cannot name the map; the
// table's own name wins when it has one. A trip with NO class name returns a
// `why` sentence instead of a gate, because the 3594 could not say which world to
// load.
func (h *Handler) travelGate(name string, mapID int, pos [3]int32, fallbackClass string) (gate MapGate, camFrom, why string) {
	class := fallbackClass
	if h.ZonePoints != nil {
		if n, ok := h.ZonePoints.Name(mapID); ok && n != "" {
			class = n
		}
	}
	if class == "" {
		return MapGate{}, "", fmt.Sprintf("map %d has no CLASS name in the spawn table, "+
			"so the 3594 could not say which world to load", mapID)
	}

	// The camera: the five floats the client's own Warp rows hold for arriving on
	// this map (ZonePoints.WarpCamera, the same call a warp NPC's door uses). The
	// facing stays 0 for the same reason it does there - the angle lives in the
	// Warp table, which this shape does not carry, and a copied angle is worse than
	// the one the map's own spawn arrivals already use.
	cam, camFrom := h.ZonePoints.WarpCamera(mapID)
	cell := pos[2]
	return MapGate{
		Name: name, To: class, ToID: mapID,
		TargetX: pos[0], TargetY: pos[1], TargetCell: &cell,
		TargetDir: 0, TargetDirByte: 0,
		Lat: cam[0], Long: cam[1], ZoomMin: cam[2], ZoomMax: cam[3], InitZoom: cam[4],
	}, camFrom, ""
}

// waypointSelect answers one click in the town grid.
//
// It sends NOTHING on every refusal, and one 3593 on success. The single exception is
// "you are already there", where the player is told so in the client's own words: they
// are looking at the grid, they clicked a town, and silence there reads as a broken UI.
// That notice is 3596, not a map change, and nothing travels.
//
// ONE log line per 3743 names, in this order: the map id the client sent, its town row,
// whether a live zone serves it, the landing cell, and whether that cell came from the
// warp row or from the spawn fallback. The line is printed AFTER the handshake so it can
// say whether anything actually went out - a log that claims a map change the code
// refused is worse than no log.
func (h *Handler) waypointSelect(sess *Session, f protocol.Frame) ([][]byte, error) {
	mapID, ok := ParseWaypointSelect(f.Data)
	if !ok {
		h.Log.Printf("zone: WAYPOINT_SELECT is %d bytes, want %d (10-byte security "+
			"header, then the map id as a WORD) - nothing to read, nothing sent: %x",
			len(f.Data), waypointSelectSize, f.Data)
		return nil, nil
	}

	// Every refusal below prints ONE line and sends nothing.
	refuse := func(what string) ([][]byte, error) {
		h.Log.Printf("zone: WAYPOINT_SELECT map %d from %d bytes %x: REFUSED - %s - "+
			"nothing was sent", mapID, len(f.Data), f.Data, what)
		return nil, nil
	}

	// One map change at a time, the same rule gateTick keeps: a second 3593 would make
	// the client's answer carry the wrong destination.
	if sess.Gate != nil {
		return refuse(fmt.Sprintf("a map change to map %d (%s) is already running",
			sess.Gate.gate.ToID, sess.Gate.gate.To))
	}
	if h.ZonePoints == nil {
		return refuse("no spawn table is loaded, so no town can be resolved; start the " +
			"zone with -points ../data/zone_points.json")
	}
	town, found := h.ZonePoints.waypointRow(mapID)
	if !found {
		// The extra byte this build has (see the file comment) could in principle sit
		// BEFORE the map id instead of after it, and then this offset reads garbage.
		// The raw bytes are in the line either way; naming what the next offset would
		// have said turns "the click does nothing" into one number to look at.
		shifted := ""
		if len(f.Data) >= waypointSelectMapID+1+waypointSelectMapIDLen {
			if alt := int(binary.LittleEndian.Uint16(f.Data[waypointSelectMapID+1:])); alt != mapID {
				if _, ok := h.ZonePoints.waypointRow(alt); ok {
					shifted = fmt.Sprintf(" - the WORD one byte later reads map %d, "+
						"which IS a town row: if that is the one clicked, this client "+
						"lays the packet out differently and the offset is the fix", alt)
				}
			}
		}
		return refuse(fmt.Sprintf("map %d is not a row in data\\zone_points.json's "+
			"waypoints list (%d row(s) there) - it is not one of the grid's towns%s",
			mapID, len(h.ZonePoints.Waypoints), shifted))
	}
	if town.Tab != 0 {
		// The window's second grid. Those rows carry no restart warp, so there is
		// nothing to resolve and no town to arrive in.
		return refuse(fmt.Sprintf("waypoint row %d (%s) is Tab %d, the FIELD grid, not "+
			"a town row - its restart warp id is %d", town.ID, town.Location, town.Tab,
			town.RestartWarpID))
	}
	if town.RestartWarpID <= 0 {
		return refuse(fmt.Sprintf("waypoint row %d (%s) carries no restart warp id, so "+
			"there is no arrival point to resolve", town.ID, town.Location))
	}
	if mapID == h.MapID {
		// The original's own answer: a MODAL box, and the trip does not happen
		// (Commander_Packet.cpp:2027-2029). The box comes from `level` (3 =
		// warning, title "Message"), not from a 3753 frame - so a system line at
		// level 0 is NOT the same thing, which is what this path used to send.
		// The type is MessageTypeCurrentWayPoint = 147: MEASURED on the shipped
		// client with `/typemsg 147 3`, which boxed "You have chosen the waypoint of
		// the current area." The source header says 136 and is wrong. Read that
		// constant before touching this.
		h.Log.Printf("zone: WAYPOINT_SELECT map %d (town %d %s) from %d bytes %x: "+
			"REFUSED - the player is already on map %d (%s); the client is told with "+
			"cmd %d type %d level %d and nothing else is sent", mapID, town.ID,
			town.Location, len(f.Data), f.Data, h.MapID, h.MapName, CmdZCMessage,
			MessageTypeCurrentWayPoint, MessageLevelWarning)
		return [][]byte{BuildMessage(currentWaypointText, MessageTypeCurrentWayPoint,
			MessageLevelWarning)}, nil
	}

	landing, err := h.ZonePoints.WaypointLanding(town)
	if err != nil {
		return refuse(fmt.Sprintf("map %d cannot be landed on: %v", mapID, err))
	}

	// The map's CLASS name is what the client feeds to SwitchMode on arrival. The
	// town row's own Location IS that name; the table's canonical row wins when it
	// disagrees, so the 3594 can never name a world the barrack does not know.
	gate, camFrom, why := h.travelGate(
		fmt.Sprintf("town %d (%s)", town.ID, town.Location),
		mapID, landing.Pos, town.Location)
	if why != "" {
		return refuse(why)
	}

	dest, live := h.ensureZone(mapID)
	if !live {
		// The one case that would strand the player: 3593 goes out, the client
		// answers, and there is no address to name in the 3594.
		return refuse(fmt.Sprintf("no live zone serves map %d (%s) - looked in %s; "+
			"start the zone for that map first", mapID, gate.To,
			registryDir(h.Zones)))
	}

	out := h.startMoveFor(sess, "WAYPOINT", "the player picked this town in the grid",
		gate)

	landed := "the spawn fallback"
	if landing.FromWarp {
		landed = "the warp row"
	}
	result := "the map change was REFUSED above - check the line before this one"
	if len(out) > 0 {
		result = fmt.Sprintf("MOVE_ZONE sent (cmd %d, %d bytes), waiting for cmd %d",
			CmdZCMoveZone, MoveZoneSize, CmdCZMoveZoneOK)
	}
	h.Log.Printf("zone: WAYPOINT_SELECT map %d (town %d %s) from %d bytes %x: live zone "+
		"%s:%d, landing (%d, %d, cell %d) from %s [%s], camera from %s - %s",
		mapID, town.ID, town.Location, len(f.Data), f.Data, dest.IP, dest.Port,
		landing.Pos[0], landing.Pos[1], landing.Pos[2], landed, landing.Source,
		fromOr(camFrom, "nowhere (zeros)"), result)
	return out, nil
}
