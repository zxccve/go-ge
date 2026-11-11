package zone

import (
	"context"
	"encoding/binary"
	"time"

	"geserver/protocol"
)

// THE LAST POINT: where the leader stood when the session LEFT the zone.
//
// This is SERVER-KEPT STATE, and that is the whole reason the file exists. There is
// no client request for it anywhere in the protocol: the shipped client never tells
// the server "remember where I am" on the way out, and the original server did not
// wait to be asked. It saved the character's position on the way out and read it back
// on the next entry. Four separate facts make that work here, and each one was taken
// from a shipped artifact rather than guessed:
//
//  1. CZ_SYSMENU 3595 IS the log-out / system-menu request, so a clean log-out is
//     visible to this server after all. Its builder is
//     `packet::CZ_SysMenu` (source2\geClient\Source\GENormalSendPacket.cpp:261-274),
//     its struct is
//     `PKS_CZ_SYSMENU : PKS_SECURITY_DEFAULT { BYTE selection;
//      bool NotifyMyInfoToPartyMembers; }` (source2\geShared\include\geZCPacket.h:994-997),
//     and its enum is SYSMENU_BARRACK=0, SYSMENU_LOGOUT=1, SYSMENU_LOBBYZONE=2
//     (source2\geShared\include\geZCTypes.h:415-418). A live packet was 12 bytes with
//     `01 00` at +10 - selection 1, LOGOUT. See data\cmd_table_live.txt:1262 for the
//     client's own registered size (`3595 12 CZ_SYSMENU`).
//
//  2. The answer ZC_LOGOUT_OK 3784 (2 bytes, command only) is NOT required for the
//     socket to close - the CLIENT always closes it - but it is what makes the close
//     immediate, and it is what the original server sends. Two paths, both in the
//     shipped binary:
//       WITH the packet: `CNormalNet::LogoutOK` -> `NormalMode()->EnableLogout()`
//       (source2\geClient\Source\GENormalNet.cpp:1804-1808) sets a flag that
//       `CNormalMode::ProcessModeSwitch` (GENormalMode.cpp:379-397) reads to call
//       `Net()->Disconnect()` at once and switch to the login mode.
//       WITHOUT it: the client waits out its own reserve timer (~10 s, 15 s outside a
//       town - `CCmdReqLogout` reserves it, GENormalCommand.cpp:55-61, and
//       `CCmdLogout::Do` then disconnects by itself, GECommonCommand.cpp:7-13).
//     Measured in docker-port\logs\zone10_err.txt: 19:38:20 the request, 19:38:30 the
//     EOF - the ten seconds the owner spent looking at a session that was already over.
//     So this zone ANSWERS, then the client closes. It is never us that ends the
//     session, which is why the EOF that follows is logged as a plain disconnect.
//
//  3. The DISPLAY half needs no new packet at all. The barrack's Continue button
//     ("이전 시작", caption 81166) is captioned from BC_COMMANDER_INFO (cmd 10)
//     contMapID, which barrack\commander_info.go fills with THIS MAP -
//     `commander.zone_id` - because that is the map a continue resumes
//     (source2\geClient\Source\GEBarrackNet.cpp:234-242; tooltip 81171 "start the game
//     at the last EXIT point"). The SAVED START POINT is the OTHER word of that same
//     struct (newMapID, tooltip 81172 "start the game at the last SAVED point"), and
//     the two were the wrong way round until R70 - which is why a Coimbra save showed
//     as Cite de Reboldoeux. So WRITING `commander.zone_id` IS the display: there is
//     nothing extra to send, and nothing to invent.
//
//  4. The ENTRY half reads the map from `commander.zone_id` and the position from
//     `character_position` (xpos/ypos/zpos/map_id - the oracle's column names, one row
//     per character; db\migrations\00010_point_and_position.sql). zone\last_point_db.go
//     owns which columns and why, and why this file never touches `character.pos_*`
//     (the barrack's own copy) nor `commander.point` (the save point).
//
// THREE TRIGGERS, ONE WRITER. A session ends in one of three ways and all of them mean
// "the leader is leaving this map": a log-out (CZ_SYSMENU), a map change
// (Handler.acceptMove, which closes this session and opens one on the destination),
// and a broken connection (the socket's own EOF, caught only by Serve's defer). They
// all call Handler.noteLastPoint, and Session.lastPointTried makes that exactly ONE
// row and ONE log line per session however many of the three fire.

const (
	CmdCZSysMenu  = 3595 // client -> server: the system menu (go to barrack / log out / lobby zone)
	CmdZCLogoutOK = 3784 // server -> client: 2 bytes, command word only

	// sysMenuSize is the client's own registered size: data\cmd_table_live.txt:1262
	// `3595 12 CZ_SYSMENU`.
	sysMenuSize = 12

	// sysMenuSelectionAt is the BYTE, after the 10-byte security header that
	// PKS_SECURITY_DEFAULT carries (cmd(2) seq(4) checksum(4) - gePacketBase.h:52-56).
	// The `bool NotifyMyInfoToPartyMembers` after it is never read: nothing here
	// forwards a log-out to a party.
	sysMenuSelectionAt = 10

	// sysMenuLogout is SYSMENU_LOGOUT (geZCTypes.h:415-418). The other two are
	// SYSMENU_BARRACK = 0 and SYSMENU_LOBBYZONE = 2, and both of those leave this map
	// too - which is why the last point is saved for EVERY selection, not only this one.
	sysMenuLogout = 1
)

// The triggers, as words: they go into the log line so a reader can tell a clean
// log-out from a socket that simply died.
const (
	lastPointSysMenu    = "sysmenu"
	lastPointDisconnect = "disconnect"
	lastPointMapChange  = "map change"
)

// lastPointTimeout bounds the write on the way out. It runs while a connection is
// ending and on the reply path of a map change, so it must never inherit a cancelled
// process context or wait on a socket that is already gone: a save that hangs here
// would hold a dying connection open and stall the map change behind it.
const lastPointTimeout = 5 * time.Second

// NearRangeXY and NearRangeZ are the search box the original uses to turn a stored
// HEIGHT back into a navmesh CELL: `Get3DPointToPosLocaly(shape, point, leaderPos, 1000,
// 300)` (Commander_Load.cpp:580). They are NOT tunable knobs - the same two numbers must
// be used on both sides or a point saved here could not be found again.
const (
	NearRangeXY = 1000
	NearRangeZ  = 300
)

// LastPoint is where the leader stood when the session left: the map, and the position
// in the ORACLE's own terms - x, y and ZPOS.
//
// POS[2] IS A WORLD HEIGHT, NOT A NAVMESH CELL, and that is the whole of R73's second
// bug. The oracle's `CHARACTER_POSITION(CHARACTER_ID, XPOS, YPOS, ZPOS, MAP_ID, UPDATED)`
// stores a HEIGHT, and the original fills it with `m_world->GetHeight(pos)` on the way
// out (Commander_FSM.cpp:644-650) and turns it back into a cell with
// `Get3DPointToPosLocaly(point, leaderPos, 1000, 300)` on the way in
// (Commander_Load.cpp:575-580). A CELL written here reads as a height of a few hundred
// units on entry, is handed to the client as a triangle index, and the character stands
// under the floor - which is exactly what the owner saw on 2026-10-29.
//
// So this type carries the STORED numbers. The CELL the client needs is resolved from
// them by the map's mesh, and it is never read back out of this struct.
type LastPoint struct {
	MapID int
	Pos   [3]int32 // x, y, and ZPOS - a HEIGHT
	Name  string   // the leader's name, for the log line only
}

// BuildLogoutOK builds ZC_LOGOUT_OK (3784): the command word and nothing else.
//
// Two bytes, exactly what the client's table registers (data\cmd_table_live.txt:1451
// `3784 2 ZC_LOGOUT_OK`). The client's handler reads no field from it - its whole job
// is to set the flag that lets the client close its own socket (see fact 2 in the file
// comment), so any length word added here would be a lie about a packet that has no
// body.
func BuildLogoutOK() []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, CmdZCLogoutOK)
	return out
}

// ParseSysMenu reads the selection byte of CZ_SYSMENU (3595).
//
// It requires the client's full registered 12 bytes, because a shorter frame is not a
// system menu at all: the byte at +10 would then be past the end of the buffer, and a
// truncated packet must read as "not a system menu" rather than as selection 0
// (SYSMENU_BARRACK), which would look like a request to go to the barrack.
func ParseSysMenu(data []byte) (selection uint8, ok bool) {
	if len(data) < sysMenuSize {
		return 0, false
	}
	return data[sysMenuSelectionAt], true
}

// UseLastPoint wires the store behind the Last Point: where a save is written, and
// where an entry reads it back.
//
// The save takes the TEAM's character ids, because the oracle's `character_position`
// is one row per character and this zone has one position for the whole team (the
// leader's). See zone\last_point_db.go.
//
// It is a method and not two exported fields because the two hooks belong together: a
// handler that read the last point and could not write it would put the team back at a
// position that never changes, and one that wrote without reading would keep a row
// nobody ever uses. The same rule as UseStartPoint and UseSavedTeleports.
func (h *Handler) UseLastPoint(
	load func(ctx context.Context, commanderID int32) (LastPoint, bool, error),
	save func(ctx context.Context, commanderID int32, characterIDs []uint32, lp LastPoint) error,
) {
	h.LastPointOf = load
	h.saveLastPoint = save
}

// noteLastPoint is the ONE writer of the last point. All three ways out of this map
// call it, so the row and the log line cannot disagree about which one happened.
//
// WHAT POSITION IT SAVES: `sess.walkedPos(now)` and never `sess.Pos`. Session.Pos is
// the click TARGET the player pointed at (see walkedPos in gate.go), so the saved spot
// is the WALKED one - the same rule the gate uses, for the same reason: a save taken
// from Pos would store a point the character may never have reached.
//
// AND IT SAVES A HEIGHT, NOT THE CELL (R73). `character_position.ZPOS` is a world
// height, so the walked WORLDPOS is handed to the map's own mesh (Handler.NavHeight,
// the game's `CWorld::GetHeight`) and THAT number is what is written. Without a mesh
// there is no height to write and the row is left alone: writing the cell is the bug
// this exists to fix, and a cell in that column puts the team under the floor on the
// next entry.
//
// IT REFUSES A POSITION THAT WAS NEVER REPORTED. A zero would overwrite a good row with
// a spot in the void, and the next entry would drop the team there - so "nothing was
// ever reported" writes NOTHING and says so. That is the case a disconnect during the
// loading screen hits: there is no position yet, and the row on file is still the best
// answer the server has.
//
// ONE WRITE PER SESSION (Session.lastPointTried). A log-out is followed by the socket's
// own EOF a moment later, and a map change is followed by this session closing too:
// without the flag each of those would write a second time and log a second line, and
// a reader comparing the window with the database would have two rows to explain.
func (h *Handler) noteLastPoint(sess *Session, trigger string) {
	if sess.lastPointTried {
		return
	}
	if sess.CommanderID <= 0 {
		if sess.AccountID != 0 {
			h.Log.Printf("zone: LAST POINT not saved for account %d (%s): this session "+
				"has no commander", sess.AccountID, sess.LoginID)
		}
		sess.lastPointTried = true
		return
	}
	if len(sess.Team) == 0 {
		h.Log.Printf("zone: LAST POINT not saved for account %d (%s): this session has "+
			"no characters", sess.AccountID, sess.LoginID)
		sess.lastPointTried = true
		return
	}

	pos := sess.walkedPos(time.Now())
	leader := sess.Team[0]
	if (pos[0] == 0 && pos[1] == 0) || !PlausibleCell(pos[2]) {
		h.Log.Printf("zone: LAST POINT not saved for commander %d (%s): no position was "+
			"ever reported on map %d, and a zero would overwrite a good row - nothing "+
			"written", sess.CommanderID, leader.Name, h.MapID)
		sess.lastPointTried = true
		return
	}
	if h.saveLastPoint == nil {
		h.Log.Printf("zone: LAST POINT not saved for commander %d (%s): no last-point "+
			"store is wired in", sess.CommanderID, leader.Name)
		sess.lastPointTried = true
		return
	}

	// THE HEIGHT, FROM THE MAP'S OWN MESH. `character_position.ZPOS` holds a world
	// height (Commander_FSM.cpp:648), so the cell the character stands on is converted
	// here and ONLY here. No mesh, or a spot the mesh does not cover, means there is no
	// honest number to write: the row keeps what it had and the log says why.
	if h.NavHeight == nil {
		h.Log.Printf("zone: LAST POINT not saved for commander %d (%s): no path engine is "+
			"wired in, so the height of (%d, %d, cell %d) is unknown - writing the cell "+
			"into zpos is the bug R73 fixes, so nothing is written",
			sess.CommanderID, leader.Name, pos[0], pos[1], pos[2])
		sess.lastPointTried = true
		return
	}
	height, ok := h.NavHeight(pos)
	if !ok {
		h.Log.Printf("zone: LAST POINT not saved for commander %d (%s): the map %d mesh has "+
			"no valid ground at (%d, %d, cell %d), so there is no height to store - "+
			"nothing is written", sess.CommanderID, leader.Name, h.MapID,
			pos[0], pos[1], pos[2])
		sess.lastPointTried = true
		return
	}

	// Its OWN context, on purpose: this runs on the way out of a connection and on the
	// reply path of a map change, so it must never hang on the process context or on a
	// dead socket.
	ctx, cancel := context.WithTimeout(context.Background(), lastPointTimeout)
	defer cancel()

	// ONE ROW PER CHARACTER, at the LEADER's walked spot. The oracle's table is keyed
	// by character, and this zone has one live position per session (the leader's), so
	// the whole team is written at the spot the team actually stood: the followers are
	// re-formed around the leader on entry, and inventing a per-character position here
	// would be inventing a fact this side never had. A slot with no handle names no row
	// and is skipped rather than written as character 0.
	ids := make([]uint32, 0, len(sess.Team))
	for _, c := range sess.Team {
		if c.Handle != 0 {
			ids = append(ids, c.Handle)
		}
	}

	// Pos[2] is the HEIGHT here, not the cell: LastPoint is the oracle's own shape.
	stored := [3]int32{pos[0], pos[1], height}
	err := h.saveLastPoint(ctx, sess.CommanderID, ids,
		LastPoint{MapID: h.MapID, Pos: stored, Name: leader.Name})
	// Set AFTER the call, success or error: a failed write must not be retried by the
	// socket's own EOF a moment later, or one session would log twice and a database
	// that is down would be hit twice for the same nothing.
	sess.lastPointTried = true
	if err != nil {
		h.Log.Printf("zone: LAST POINT could not be saved for commander %d (%s): %v - "+
			"the row is unchanged", sess.CommanderID, leader.Name, err)
		return
	}
	h.Log.Printf("zone: LAST POINT saved for commander %d (leader %q, character %d): "+
		"map %d, (%d, %d, cell %d = height %d), trigger %s - wrote commander.zone_id "+
		"and character_position rows for %d character(s) with ZPOS = the HEIGHT %d; "+
		"commander.point (the SAVED START POINT) was NOT touched",
		sess.CommanderID, leader.Name, leader.Handle, h.MapID,
		pos[0], pos[1], pos[2], height, trigger, len(ids), height)
}

// sysMenu answers CZ_SYSMENU (3595): the system menu's three selections.
//
// The LAST POINT is saved for EVERY selection, because all three of them leave this
// map: 0 goes to the barrack, 1 logs out, 2 goes to the lobby zone. What is ANSWERED
// differs, and the difference is the client's own:
//
//   - selection 1 (LOGOUT): ZC_LOGOUT_OK (3784) goes out, and the client then closes
//     the socket itself. Without it the client waits out its 10 s reserve timer (see
//     fact 2 in the file comment), so the answer is not a courtesy - it is what ends
//     the session now instead of ten seconds from now.
//   - the other two: NOTHING is sent. Selection 0 is the barrack hand-off and that
//     whole path is R8, not this task; selection 2 has no lobby zone in this server at
//     all. Both are named in the log so the next slice starts from what really arrived.
//
// BEFORE THIS, 3595 fell into Reply's default branch and printed "not handled yet
// (first seen)" (docker-port\logs\zone10_err.txt:513-514) - the log-out request was
// being dropped on the floor.
func (h *Handler) sysMenu(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	selection, ok := ParseSysMenu(f.Data)
	if !ok {
		h.Log.Printf("zone: CZ_SYSMENU is %d bytes, want the client's registered %d - "+
			"ignored", len(f.Data), sysMenuSize)
		return nil, nil
	}

	h.noteLastPoint(sess, lastPointSysMenu)

	if selection == sysMenuLogout {
		out := BuildLogoutOK()
		// THE HANDOFF IS GONE WITH THE SESSION (t_0cda12ac): the row has been served
		// and the player is leaving, so nothing can be replayed from it later. A
		// failure is not fatal here - the row is already marked consumed at entry,
		// this only removes it.
		if h.DropEntry != nil && sess.AccountID != 0 {
			if err := h.DropEntry(ctx, sess.AccountID); err != nil {
				h.Log.Printf("zone: logout of account %d: %v (the handoff row stays, "+
					"already consumed, and is refused by its consume mark)", sess.AccountID, err)
			}
		}
		h.Log.Printf("zone: CZ_SYSMENU selection %d (LOGOUT) from account %d on map %d: "+
			"answering ZC_LOGOUT_OK (cmd %d, %d bytes) - the client closes this socket "+
			"itself once it reads it, so the close that follows is not an error",
			selection, sess.AccountID, h.MapID, CmdZCLogoutOK, len(out))
		return [][]byte{out}, nil
	}

	// SELECTIONS 0 AND 2 ANSWER NOW (2026-11-09): 0 sends ZC_MOVE_BARRACK so the client
	// finishes its switch to character select, and 2 is the save-point move (the same
	// warp id the barrack shows). Both live in zone\sysmenu.go, beside the reference
	// lines they copy (Commander_Packet.cpp:734-765).
	if out, err := h.sysMenuSelection(ctx, sess, f); err != nil || len(out) > 0 {
		return out, err
	}
	h.Log.Printf("zone: CZ_SYSMENU selection %d from account %d on map %d: the LAST "+
		"POINT was saved on the way out, and this selection answered nothing (see the "+
		"reason above)", selection, sess.AccountID, h.MapID)
	return nil, nil
}

// continuePoint answers "does the barrack want this team put back where it left" - the
// ENTRY half of the Last Point, called once per START_GAME by Handler.startGame.
//
// WHERE IT SITS IN THE ORDER DEPENDS ON THE BUTTON. Continue Game (CB_CONTINUEGAME 23,
// `pending_zone_entry.kind = 1`) asks for this point FIRST, because that is what the
// player pressed. New Game (22, kind 0) and the mission button (24, kind 2) ask for the
// SAVED START POINT first (Handler.savedStartPoint) and only then try this one. The
// order lives in Handler.placeTeam (zone\zone.go).
//
// IT ANSWERS TWO NUMBERS, AND THEY ARE NOT THE SAME NUMBER:
//
//	at   WORLDPOS{x, y, CELL}  where the team enters - the client needs the CELL
//	lp   the row as stored    x, y and ZPOS, which is a HEIGHT
//
// The row's height is turned into a cell by the map's own mesh, exactly as the original
// does (`Get3DPointToPosLocaly(point, leaderPos, 1000, 300)`, Commander_Load.cpp:580).
// Handing the stored number to the client as a cell is R73's "under the terrain": a
// height of 255 is a perfectly plausible triangle index, so nothing downstream catches
// it and the client drops the character through the floor.
//
// It answers TRUE only when all of these hold, and every refusal is a log line rather
// than a silence:
//
//   - a store is wired (silently false when it is not: that is the normal "no store"
//     state, and startGame already says nothing about it);
//   - the session has a commander (no commander, no row);
//   - the read worked;
//   - the player came through the barrack's CONTINUE button - that is
//     `pending_zone_entry.kind = 1`, read by PGStore.LastPointOf, with `is_continue`
//     as the fallback for a row written before `kind` existed;
//   - the saved map IS this zone's map. A zone serves one map, so a point from another
//     map means nothing here;
//   - a path engine is wired, because without one a stored HEIGHT cannot be turned into
//     a cell at all (the old behaviour - sending the stored number as a cell - is the
//     bug, not a fallback);
//   - the mesh HAS ground within NearRangeXY / NearRangeZ of the stored point;
//   - the stored z is not the very cell the mesh resolves it to: that is the signature
//     of the buggy writer (it stored the cell), and it is refused rather than repaired,
//     because a row that lies about its own units cannot be trusted on any map.
func (h *Handler) continuePoint(ctx context.Context, sess *Session) (at [3]int32,
	lp LastPoint, ok bool) {
	if h.LastPointOf == nil {
		return [3]int32{}, LastPoint{}, false
	}
	if sess.CommanderID <= 0 {
		return [3]int32{}, LastPoint{}, false
	}
	lp, found, err := h.LastPointOf(ctx, sess.CommanderID)
	if err != nil {
		h.Log.Printf("zone: the last point of commander %d could not be read (%v) - "+
			"falling through to the map's own spawn", sess.CommanderID, err)
		return [3]int32{}, LastPoint{}, false
	}
	if !found {
		h.Log.Printf("zone: commander %d did not come through the barrack's Last Point "+
			"(no pending continue entry) - falling through to the map's own spawn",
			sess.CommanderID)
		return [3]int32{}, LastPoint{}, false
	}
	if lp.MapID != h.MapID {
		h.Log.Printf("zone: the LAST POINT of commander %d is map %d and this zone "+
			"serves map %d - the saved cell means nothing here, entering at the saved "+
			"start point", sess.CommanderID, lp.MapID, h.MapID)
		return [3]int32{}, LastPoint{}, false
	}
	if lp.Pos[0] == 0 && lp.Pos[1] == 0 {
		// No position was ever saved on this map: a row written before this feature
		// existed carries the column defaults (0, 0, 0), and x/y of 0 is off the map.
		h.Log.Printf("zone: the LAST POINT of commander %d has no position on map %d "+
			"(x and y are both 0) - falling through to the map's own spawn",
			sess.CommanderID, h.MapID)
		return [3]int32{}, LastPoint{}, false
	}
	if h.NavNear == nil {
		h.Log.Printf("zone: the LAST POINT of commander %d is (%d, %d, z %d) on map %d "+
			"and no path engine is wired in, so that z cannot be turned into a navmesh "+
			"cell - falling through to the map's own spawn",
			sess.CommanderID, lp.Pos[0], lp.Pos[1], lp.Pos[2], lp.MapID)
		return [3]int32{}, LastPoint{}, false
	}
	ground, height, onMesh := h.NavNear(lp.Pos, NearRangeXY, NearRangeZ)
	if !onMesh {
		h.Log.Printf("zone: the LAST POINT of commander %d is (%d, %d, z %d) on map %d "+
			"and the mesh has no ground within %d/%d of it - falling through to the "+
			"map's own spawn", sess.CommanderID, lp.Pos[0], lp.Pos[1], lp.Pos[2],
			lp.MapID, NearRangeXY, NearRangeZ)
		return [3]int32{}, LastPoint{}, false
	}
	if lp.Pos[2] == ground[2] {
		// THE BUGGY WRITER'S SIGNATURE: zpos holds the CELL, so the stored number is the
		// triangle index the mesh just resolved. It is not a height, and the row cannot
		// be trusted on any map - refuse it rather than place the team from it.
		h.Log.Printf("zone: the LAST POINT of commander %d is (%d, %d, z %d) on map %d "+
			"and z EQUALS the navmesh cell the mesh resolves there (cell %d) - that row "+
			"holds a CELL, not a height, so it is refused and the map's own spawn is "+
			"used instead", sess.CommanderID, lp.Pos[0], lp.Pos[1], lp.Pos[2],
			lp.MapID, ground[2])
		return [3]int32{}, LastPoint{}, false
	}
	h.Log.Printf("zone: LAST POINT of commander %d: the leader %q stood at (%d, %d, "+
		"z %d) on map %d (%s); the mesh puts that on cell %d at height %d - the team "+
		"enters THERE, not at the town's restart point", sess.CommanderID, lp.Name,
		lp.Pos[0], lp.Pos[1], lp.Pos[2], lp.MapID, h.MapName, ground[2], height)
	return ground, lp, true
}
