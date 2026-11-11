package zone

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"geserver/protocol"
)

// THE PLAYER'S OWN SAVED POINTS: save, list, remove, travel.
//
// Four requests and one answer, all of them one-window traffic - the client's
// `teleportation` frame, whose THREE buttons are the only way these ids are ever
// sent. The shipped client's own layout file is the whole UI:
//
//	GEAndromida\tools\andro\ui\uixml\teleport.xml:6-10
//	  <listbox name="select warp" ... LListDblClickScp="TELEPORT_EXECUTE"/>
//	  <text name="count" caption="<$>128067</>" param1="used" param2="total" .../>
//	  <button name="warp"   ... LBtnUpScp="TELEPORT_EXECUTE"/>
//	  <button name="save"   ... LBtnUpScp="TELEPORT_SAVE"/>
//	  <button name="delete" ... LBtnUpScp="TELEPORT_REMOVE"/>
//	ge\ui\uiscp\game.scp:116-137
//	  UI_TELEPORT_EXECUTE -> TeleportSelect(uiIndex, GetNumber(uiFrame,"select warp"))
//	  UI_TELEPORT_REMOVE  -> TeleportRemove(uiIndex, GetNumber(uiFrame,"select warp"))
//	  UI_TELEPORT_SAVE    -> TeleportSave(uiIndex)
//
// So the SAVE names NO point at all: it asks the server to remember where the
// character stands (saved_teleport_db.go has the sender, its caller and the
// original server's own behaviour). The OTHER TWO name a point by its row in the
// listbox, and that number is an INDEX INTO THE LIST THE SERVER LAST SENT - never a
// slot number. Those two differ the moment one point is removed from the middle, so
// every read of it goes through listAt() below.
//
// WIRE SIZES, from the client's own table (docker-port\data\cmd_table_live.txt:
// 1307-1311) and matching PKS_SECURITY_DEFAULT byte for byte:
//
//	3640 CZ_TELEPORT_SAVE    11 = 10 + BYTE charIndex
//	3641 CZ_TELEPORTLIST     11 = 10 + BYTE charIndex
//	3643 CZ_TELEPORT_REMOVE  12 = 10 + BYTE charIndex + BYTE index
//	3644 CZ_TELEPORT         12 = 10 + BYTE charIndex + BYTE index
//
//	geZCPacket.h:2338-2358
//	  PKS_CZ_TELEPORTLIST     { BYTE charIndex; }
//	  PKS_CZ_TELEPORT_REMOVE  { BYTE charIndex; BYTE index; }
//	  PKS_CZ_TELEPORT_SAVE    { BYTE charIndex; }
//	  PKS_CZ_TELEPORT         { BYTE charIndex; BYTE index; }
//	gePacketBase.h:52  PKS_SECURITY_DEFAULT { WORD command; DWORD seq; int checksum; }
//
// 10 + 1 = 11 and 10 + 2 = 12, so the payload starts at offset 10 and nothing here
// is guessed. `charIndex` is the PC the window belongs to (the frame is INDEXED:
// datatable_ui.xml:82 `ClassName="teleportation" ... UseIndex="YES"`), and it is
// used for the log line only - a commander has ONE list, not one per character
// (the original kept it on CCommander, Commander.cpp:925).
//
// The shipped client's own builders agree with those offsets, and they also settle
// that a SAVE names NO SLOT:
//
//	FUN_00506dcd  3640, writes cmd 0xe38, 8 zero bytes (seq + checksum), then ONE
//	              byte = the PC index. No second byte exists to name a point with.
//	FUN_00506d7d  3643, two bytes: the PC index at +10, the list row at +11.
//	FUN_00506ce3  3644, same two bytes, cmd 0xe3c.
//	FUN_00506d... all three ZERO seq and checksum, so a reader must not require them.
//
// LAYOUT OF THE ANSWER, ZC_TELEPORTLIST (3642) - PROVEN FROM THE SHIPPED BINARY, not
// from source2's struct. source2 says `(WORLDPOINT + MAP_ID) * curCount` under a
// 4-byte header; the binary agrees on the RECORD and disagrees on the header, because
// it also carries the two bytes source2's comment names but its own size does not:
//
//	dispatch  switch(param_1 - 0xd7d), case 0xbd (0xe3a) -> FUN_004e5052
//	0x4e5059  MOVZX EAX, byte ptr [ESI+0x5]      count = packet[5]  (len NOT read)
//	0x4e5066  MOVZX EAX, byte ptr [ESI+0x4]      charIndex = packet[4]
//	0x4e1238  ADD EAX, 0x6                       records START AT +6
//	0x4e5072  CALL 0x005de3cc                    the parser, below
//	0x4e507c  PUSH 0x9d3318                      "teleportation" -> ui::UpdateData
//
//	FUN_005de3cc (the parser)
//	0x5de400  CALL 0x004b77c3                    CLEARS the list first: each 3642
//	                                             replaces, it never appends
//	0x5de420  PUSH 0xc / PUSH ESI                copies 12 bytes of position
//	0x5de429  MOV AX, word ptr [ESI+0xc]         map id = the WORD at record +12
//	0x5de439  ADD ESI, 0xe                       STRIDE = 14
//	0x5de405  CMP [EBP+0x10], 0 / DEC EDI        the loop is bounded by curCount
//	                                             and CLAMPED NOWHERE ELSE
//
// so: len(2) covers the WHOLE packet, charIndex at +4, curCount at +5, then one
// 14-byte record per point - x int32, y int32, z int32, map id WORD. The 46-byte
// element the client keeps in memory is 12 + 2 + 32 bytes the packet never writes:
// the window's NAME comes from the map table (`UIListFrame.cpp:844-846` builds
// "%s : %d %d" from the record's position and the map's name), which is why this
// server has nothing to put on the wire for it.
//
// AND THE RECORD'S THIRD INT IS THE NAVMESH CELL, not a height. The original server
// wrote `info->pos.cell = teleport->point.z` into exactly this field
// (TeleportInfo.cpp:131), and this project's whole position convention is
// {x, y, cell} - see zone\gate.go.
const (
	CmdCZTeleportSave   = 3640
	CmdCZTeleportList   = 3641
	CmdZCTeleportList   = 3642
	CmdCZTeleportRemove = 3643
	CmdCZTeleport       = 3644

	// teleportBody is where PKS_SECURITY_DEFAULT ends and the payload begins.
	teleportBody = 10
	// teleportCharIndex is the PC the request is for, at +0 of the body.
	teleportCharIndex = teleportBody
	// teleportIndexAt is the listbox row, at +1 of the body: present on the two
	// requests that name a point, absent on save and on the list request.
	teleportIndexAt = teleportBody + 1

	// teleportOwnList is teltype 0, and it is the ONLY list the `teleportation` window
	// (3640..3644, answered by 3642) has ever drawn. That window names no list at all,
	// so this family always passes 0 EXPLICITLY since the Warp List learned about four
	// lists (R76 slice 6): the store now takes a teltype on every call, and passing
	// anything but 0 here would change 3642's wire behaviour, which is not this
	// window's to change.
	teleportOwnList = 0

	// teleportListHeader is PKS_VARSIZE: cmd(2) + len(2), and `len` is the WHOLE
	// packet including these four bytes (the client's extract computes its count
	// from it, the same way 3742 does - see commwarp.go).
	teleportListHeader = 4
	// teleportListPrefix is PKS_ZC_TELEPORTLIST's own two fields: charIndex and
	// curCount, one byte each.
	teleportListPrefix = 2
	// teleportListRecord is one saved point on the wire: x, y, cell (12 bytes) then
	// the map id as a WORD. PROVEN stride, from `ADD ESI, 0xe` at 0x5de439.
	teleportListRecord = 14
)

// ParseTeleportIndex reads the listbox row out of CZ_TELEPORT (3644) or
// CZ_TELEPORT_REMOVE (3643).
//
// ok is false only when the packet is too short to hold the byte - a malformed
// client, not a crash. The value is NOT range-checked here: the refusal belongs
// where it is compared against the list, so the log can say what the number was.
func ParseTeleportIndex(pt []byte) (index int, ok bool) {
	if len(pt) < teleportIndexAt+1 {
		return 0, false
	}
	return int(pt[teleportIndexAt]), true
}

// ParseTeleportCharIndex reads the PC index out of any of the four requests. It is
// 0 when the packet is too short, which is also what the frame's index 0 is - and
// it is only ever logged.
func ParseTeleportCharIndex(pt []byte) int {
	if len(pt) < teleportCharIndex+1 {
		return 0
	}
	return int(pt[teleportCharIndex])
}

// teleportSettings are the handler's three store hooks, kept together so a test can
// wire all three or none.
//
// NIL means "no saved-point store here", and every request is then REFUSED with a
// log line: a window that claims a point was saved when nothing was written is
// worse than one that does nothing, the same rule Handler.Commit follows.
type teleportSettings struct {
	Load func(ctx context.Context, commanderID int32) ([]SavedPoint, error)
	// Save, Remove, Rename and Move all carry a `telType`: `saved_teleport` holds FOUR
	// lists of ten now, and the Warp List's 3635 / 3636 / 3638 / 3639 each name which
	// one they mean. The 3640 family always passes teleportOwnList (0).
	Save   func(ctx context.Context, commanderID int32, telType, slot int, p SavedPoint) (SavedPoint, error)
	Remove func(ctx context.Context, commanderID int32, telType, slot int) (SavedPoint, bool, error)
	// Rename is the UPDATE half of the Warp List's 3638: it changes a row's `name`
	// and nothing else. It is optional because the 3640 family never needs it - a
	// nil hook refuses a rename with a log line rather than dropping it silently.
	Rename func(ctx context.Context, commanderID int32, telType, slot int, name string) (SavedPoint, bool, error)
	// Move is the Warp List's 3639 SWAP, done as a move: the row at one DISPLAY
	// index of ONE list is taken out and spliced in at another, and the commander's
	// whole row set comes back in its new order. Like Rename it is optional, and a nil
	// hook refuses a swap with a log line: a window that claims a row moved when the
	// store was not reordered would draw the old order on the very next refresh.
	Move func(ctx context.Context, commanderID int32, telType, from, to int) ([]SavedPoint, error)
}

// listAt maps a LISTBOX ROW to the saved point it draws.
//
// This is the whole reason the remove and travel handlers do not simply use the
// number they were sent: slots are stable (a remove leaves a gap) and list rows are
// not, so `rows[3]` is "the fourth point the player can see", which may well be
// slot 7. ok is false when the client named a row the list does not have.
func listAt(rows []SavedPoint, index int) (SavedPoint, bool) {
	if index < 0 || index >= len(rows) {
		return SavedPoint{}, false
	}
	return rows[index], true
}

// UseSavedTeleports wires the saved-point store: the three methods of PGStore that
// read, write and delete a `saved_teleport` row.
//
// It is a method and not an exported field because the three hooks have to arrive
// together: a handler with Load but no Save could draw the window and then refuse
// the save button, which is a state nobody wants to debug.
func (h *Handler) UseSavedTeleports(
	load func(ctx context.Context, commanderID int32) ([]SavedPoint, error),
	save func(ctx context.Context, commanderID int32, telType, slot int, p SavedPoint) (SavedPoint, error),
	remove func(ctx context.Context, commanderID int32, telType, slot int) (SavedPoint, bool, error),
) {
	h.teleports = teleportSettings{Load: load, Save: save, Remove: remove}
}

// UseSavedPointRename wires the ONE extra store method the Warp List needs: 3638
// RENAME changes a row's `name` in place (the memo the 3634 window draws) and moves
// nothing else. It is separate from UseSavedTeleports on purpose - the 3640 family
// has no rename and must keep working with the three hooks it has always had.
//
// A store wired with UseSavedTeleports and not this one still serves the
// teleportation window completely; a 3638 then refuses with a log line.
func (h *Handler) UseSavedPointRename(
	rename func(ctx context.Context, commanderID int32, telType, slot int, name string) (SavedPoint, bool, error),
) {
	h.teleports.Rename = rename
}

// UseSavedPointMove wires the ONE extra store method 3639 SWAP needs: moving a row
// from one display position to another and keeping it moved. It is separate from
// UseSavedTeleports for the same reason UseSavedPointRename is - the 3640 family has no
// reorder and must keep working with the three hooks it has always had.
//
// A store wired with UseSavedTeleports and not this one still serves the teleportation
// window completely; a 3639 then refuses with a log line.
func (h *Handler) UseSavedPointMove(
	move func(ctx context.Context, commanderID int32, telType, from, to int) ([]SavedPoint, error),
) {
	h.teleports.Move = move
}

// teleportRequest answers CZ_TELEPORTLIST (3641): the window has just opened and
// wants its rows.
func (h *Handler) teleportRequest(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_TELEPORTLIST", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil {
		h.Log.Printf("zone: CZ_TELEPORTLIST from account %d REFUSED - no saved-point "+
			"store is wired in, so the window would be answered with a list this "+
			"server could not read; nothing was sent", sess.AccountID)
		return nil, nil
	}
	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORTLIST for commander %d REFUSED - reading the "+
			"saved points failed: %v; nothing was sent", commanderID, err)
		return nil, nil
	}
	// The `teleportation` window draws ONE list and it is teltype 0. The store returns
	// all four lists now, so this family filters explicitly - a 3642 must never grow to
	// forty rows because the Warp List learned about panels.
	rows = rowsOfType(rows, teleportOwnList)
	pc := ParseTeleportCharIndex(f.Data)

	// Why the packet was cut before it went ON the wire is only in a test when the
	// list does not fit: the client's byte for a record count is one byte, so a
	// commander with more than 255 points could not be described at all.
	out, err := BuildTeleportList(pc, rows)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORTLIST for commander %d REFUSED - %v; nothing "+
			"was sent", commanderID, err)
		return nil, nil
	}
	h.Log.Printf("zone: CZ_TELEPORTLIST (pc %d) for commander %d: %d saved point(s), "+
		"%d byte(s) sent: %s", pc, commanderID, len(rows), len(out), describePoints(rows))
	return [][]byte{out}, nil
}

// teleportSave answers CZ_TELEPORT_SAVE (3640): write where the leader stands into
// the first free slot, then hand the window the new list.
//
// The position is `sess.walkedPos(now)` and not `sess.Pos`, for the reason gate.go
// spells out at its own call: CZ_MOVE_PC carries the DESTINATION of a click, so
// sess.Pos is where the player is going, while walkedPos is where the character has
// actually got to. A save that used sess.Pos would store a point the player may not
// have reached.
func (h *Handler) teleportSave(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_TELEPORT_SAVE", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Save == nil || h.teleports.Load == nil {
		h.Log.Printf("zone: CZ_TELEPORT_SAVE from account %d REFUSED - no saved-point "+
			"store is wired in; nothing was written and nothing was sent", sess.AccountID)
		return nil, nil
	}

	now := time.Now()
	pos := sess.walkedPos(now)
	pc := ParseTeleportCharIndex(f.Data)

	// A save needs a position, and (0, 0, cell 0) is not one: it is the world origin,
	// which no playable spot on any map sits on, and it is what a session that has
	// not been placed yet carries. Cell 0 is a REAL triangle index, so the cell check
	// below cannot catch this on its own - which is why the three zeros are refused
	// by name here.
	if pos == [3]int32{} {
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d REFUSED - the "+
			"leader has no position yet (0, 0, cell 0), so there is nowhere to save; "+
			"nothing was written, nothing was sent", pc, commanderID)
		return nil, nil
	}

	// The position is checked BEFORE the write, and a cell that cannot be a navmesh
	// triangle is refused rather than stored: travelling to it later would drop the
	// character through the world (see saved_teleport_db.go).
	if !PlausibleCell(pos[2]) {
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d REFUSED - the "+
			"leader stands at (%d, %d, cell %d) and %v; nothing was written, nothing "+
			"was sent", pc, commanderID, pos[0], pos[1], pos[2],
			badCellError(pos[2]))
		return nil, nil
	}

	// `teleportOwnList` (0) is the list this window has always drawn; see the constant.
	want := SavedPoint{TelType: teleportOwnList, Slot: -1, MapID: h.MapID, Pos: pos,
		Name: h.MapName}
	got, err := h.teleports.Save(ctx, commanderID, teleportOwnList, -1, want)
	switch {
	case errors.Is(err, errNoFreeSlot):
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d REFUSED - all %d "+
			"slot(s) are taken; the original deleted the oldest point to make room and "+
			"this one does not, so nothing was written and nothing was sent",
			pc, commanderID, MaxSavedTeleports)
		return nil, nil
	case err != nil:
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d REFUSED - the "+
			"write failed: %v; nothing was sent", pc, commanderID, err)
		return nil, nil
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		// The row IS written: say so, or a reader would think the save failed.
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d: SAVED slot %d "+
			"map %d (%s) at (%d, %d, cell %d), but reading the list back failed: %v - "+
			"the window was NOT refreshed", pc, commanderID, got.Slot, got.MapID,
			got.Name, got.Pos[0], got.Pos[1], got.Pos[2], err)
		return nil, nil
	}
	rows = rowsOfType(rows, teleportOwnList)
	out, err := BuildTeleportList(pc, rows)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d: SAVED slot %d "+
			"map %d (%s) at (%d, %d, cell %d), but the list could not be built: %v - "+
			"the window was NOT refreshed", pc, commanderID, got.Slot, got.MapID,
			got.Name, got.Pos[0], got.Pos[1], got.Pos[2], err)
		return nil, nil
	}
	h.Log.Printf("zone: CZ_TELEPORT_SAVE (pc %d) for commander %d: SAVED slot %d "+
		"map %d (%s) at (%d, %d, cell %d); the refreshed list (%d point(s), %d bytes) "+
		"was sent: %s", pc, commanderID, got.Slot, got.MapID, got.Name, got.Pos[0],
		got.Pos[1], got.Pos[2], len(rows), len(out), describePoints(rows))
	return [][]byte{out}, nil
}

// teleportRemove answers CZ_TELEPORT_REMOVE (3643): delete the point the player
// selected, then hand the window the new list.
//
// The number on the wire is the LISTBOX ROW, so it is translated through the list
// the client was last sent (listAt). Deleting by the number directly would delete
// the wrong point as soon as one gap exists.
func (h *Handler) teleportRemove(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_TELEPORT_REMOVE", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Remove == nil || h.teleports.Load == nil {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE from account %d REFUSED - no "+
			"saved-point store is wired in; nothing was deleted and nothing was sent",
			sess.AccountID)
		return nil, nil
	}
	pc := ParseTeleportCharIndex(f.Data)
	index, ok := ParseTeleportIndex(f.Data)
	if !ok {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d) for commander %d REFUSED - %d "+
			"bytes, want %d (10-byte header, the PC, then the list row); nothing was "+
			"deleted, nothing was sent: %x", pc, commanderID, len(f.Data), 12, f.Data)
		return nil, nil
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d, row %d) for commander %d "+
			"REFUSED - reading the saved points failed: %v; nothing was deleted",
			pc, index, commanderID, err)
		return nil, nil
	}
	// The named row is a row of the FIRST list only: 3643 has no teltype byte at all,
	// so this window can name nothing else (see teleportOwnList).
	rows = rowsOfType(rows, teleportOwnList)
	target, found := listAt(rows, index)
	if !found {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d) for commander %d REFUSED - the "+
			"client named row %d and the list has %d point(s) [%s]; nothing was "+
			"deleted, nothing was sent", pc, commanderID, index, len(rows),
			describePoints(rows))
		return nil, nil
	}
	gone, deleted, err := h.teleports.Remove(ctx, commanderID, teleportOwnList, target.Slot)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d) for commander %d REFUSED - "+
			"deleting slot %d failed: %v; nothing was sent", pc, commanderID,
			target.Slot, err)
		return nil, nil
	}

	rows, err = h.teleports.Load(ctx, commanderID)
	if err != nil {
		// The row IS gone (or was already), but the window still has to be told what
		// the list is now - and a stale list is worse than none, so say so instead.
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d) for commander %d: slot %d was "+
			"%s, but the list could not be read back (%v); the window was NOT refreshed",
			pc, commanderID, target.Slot, wasDeleted(deleted), err)
		return nil, nil
	}
	rows = rowsOfType(rows, teleportOwnList)
	out, err := BuildTeleportList(pc, rows)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d) for commander %d: slot %d "+
			"deleted, but the list could not be built: %v; the window was NOT refreshed",
			pc, commanderID, target.Slot, err)
		return nil, nil
	}
	h.Log.Printf("zone: CZ_TELEPORT_REMOVE (pc %d, row %d) for commander %d: slot %d "+
		"map %d (%s) at (%d, %d, cell %d) %s; the refreshed list (%d point(s), "+
		"%d bytes) was sent: %s", pc, index, commanderID, gone.Slot, gone.MapID,
		fromOr(gone.Name, "unnamed"), gone.Pos[0], gone.Pos[1], gone.Pos[2],
		wasDeleted(deleted),
		len(rows), len(out), describePoints(rows))
	return [][]byte{out}, nil
}

// teleportTravel answers CZ_TELEPORT (3644): go to the point the player selected.
//
// It does NOT build a map change of its own. The destination is resolved to a
// MapGate and handed to Handler.startMoveFor, which is the SAME sender a doorway and
// a town click use: one 3593 goes out, the client answers 3686, and
// Handler.gateReady -> Handler.acceptMove builds the 3594 and writes the handoff.
// A second travel path here would be a second place for the handshake to drift.
func (h *Handler) teleportTravel(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_TELEPORT", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil {
		h.Log.Printf("zone: CZ_TELEPORT from account %d REFUSED - no saved-point store "+
			"is wired in; nothing was sent", sess.AccountID)
		return nil, nil
	}
	pc := ParseTeleportCharIndex(f.Data)
	index, ok := ParseTeleportIndex(f.Data)
	if !ok {
		h.Log.Printf("zone: CZ_TELEPORT (pc %d) for commander %d REFUSED - %d bytes, "+
			"want %d (10-byte header, the PC, then the list row); nothing was sent: %x",
			pc, commanderID, len(f.Data), 12, f.Data)
		return nil, nil
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		h.Log.Printf("zone: CZ_TELEPORT (pc %d, row %d) for commander %d REFUSED - "+
			"reading the saved points failed: %v; nothing was sent", pc, index,
			commanderID, err)
		return nil, nil
	}
	// 3644 names a row of the FIRST list: it carries no teltype byte (teleportOwnList).
	rows = rowsOfType(rows, teleportOwnList)
	target, found := listAt(rows, index)
	if !found {
		h.Log.Printf("zone: CZ_TELEPORT (pc %d) for commander %d REFUSED - the client "+
			"named row %d and the list has %d point(s) [%s]; nothing was sent",
			pc, commanderID, index, len(rows), describePoints(rows))
		return nil, nil
	}

	// One map change at a time - the same rule gateTick and the town grid keep.
	if sess.Gate != nil {
		h.Log.Printf("zone: CZ_TELEPORT (pc %d) for commander %d REFUSED - a map "+
			"change to map %d (%s) is already running; nothing was sent", pc,
			commanderID, sess.Gate.gate.ToID, sess.Gate.gate.To)
		return nil, nil
	}

	gate, camFrom, why := h.travelGate(
		fmt.Sprintf("saved point %d (map %d)", target.Slot, target.MapID),
		target.MapID, target.Pos, "")
	if why != "" {
		h.Log.Printf("zone: CZ_TELEPORT (pc %d, row %d) for commander %d -> slot %d map "+
			"%d at (%d, %d, cell %d) REFUSED - %s; nothing was sent", pc, index,
			commanderID, target.Slot, target.MapID, target.Pos[0], target.Pos[1],
			target.Pos[2], why)
		return nil, nil
	}

	out := h.startMoveFor(sess, "TELEPORT", "the player picked a saved point", gate)
	result := "the map change was REFUSED above - check the line before this one"
	if len(out) > 0 {
		result = fmt.Sprintf("MOVE_ZONE sent (cmd %d, %d bytes), waiting for cmd %d",
			CmdZCMoveZone, MoveZoneSize, CmdCZMoveZoneOK)
	}
	h.Log.Printf("zone: CZ_TELEPORT (pc %d, row %d) for commander %d -> slot %d map %d "+
		"(%s) at (%d, %d, cell %d), camera from %s - %s", pc, index, commanderID,
		target.Slot, target.MapID, gate.To, target.Pos[0], target.Pos[1],
		target.Pos[2], fromOr(camFrom, "nowhere (zeros)"), result)
	return out, nil
}

// teleportCommander resolves the commander a request belongs to, or returns the
// packets the handler should send instead (always none) and a line saying why.
//
// Every refusal prints ONE line with the raw bytes: these ids are new, so "the
// button did nothing" must be answerable from the log alone.
func (h *Handler) teleportCommander(sess *Session, name string, size int, data []byte) (int32, [][]byte) {
	if sess.CommanderID <= 0 {
		h.Log.Printf("zone: %s from account %d REFUSED - the commander was never "+
			"resolved for this session, so there is no list to read or write; nothing "+
			"was sent (%d bytes): %x", name, sess.AccountID, size, data)
		return 0, [][]byte{}
	}
	return sess.CommanderID, nil
}

// describePoints renders saved points for a log line: "slot0 map10@(1,2,cell3)".
func describePoints(rows []SavedPoint) string {
	if len(rows) == 0 {
		return "none"
	}
	var b []byte
	for i, p := range rows {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, fmt.Sprintf("slot%d map%d@(%d,%d,cell%d)", p.Slot, p.MapID,
			p.Pos[0], p.Pos[1], p.Pos[2])...)
	}
	return string(b)
}

// wasDeleted says what a remove that read no row means, for the log.
func wasDeleted(deleted bool) string {
	if deleted {
		return "deleted"
	}
	return "already gone"
}

// BuildTeleportList is ZC_TELEPORTLIST (3642): the answer to CZ_TELEPORTLIST, and
// the refresh sent after a save or a remove.
//
// LAYOUT - see the file comment above the constants and the note in
// saved_teleport_test.go, which pins every byte of it.
func BuildTeleportList(pc int, rows []SavedPoint) ([]byte, error) {
	if len(rows) > 255 {
		return nil, fmt.Errorf("%d saved points cannot be described: the record "+
			"count is one byte", len(rows))
	}
	out := make([]byte, teleportListHeader+teleportListPrefix+teleportListRecord*len(rows))
	binary.LittleEndian.PutUint16(out[0:], CmdZCTeleportList)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	out[teleportListHeader] = byte(pc)
	out[teleportListHeader+1] = byte(len(rows))
	for i, p := range rows {
		at := teleportListHeader + teleportListPrefix + i*teleportListRecord
		binary.LittleEndian.PutUint32(out[at:], uint32(p.Pos[0]))
		binary.LittleEndian.PutUint32(out[at+4:], uint32(p.Pos[1]))
		binary.LittleEndian.PutUint32(out[at+8:], uint32(p.Pos[2]))
		binary.LittleEndian.PutUint16(out[at+12:], uint16(p.MapID))
	}
	return out, nil
}
