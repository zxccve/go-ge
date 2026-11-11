package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"geserver/item"
	"geserver/protocol"
	"geserver/world"
)

// The town-teleport window, and the two packets that are NOT it.
//
// WHAT THE CLIENT DOES WITH EACH ID (quote: the shipped client's own sources)
//
//	3634 ZC_COMMWARP_LIST   -> `CModeNet::CommWarpList` fills
//	                           `Session().COMM_WARP_LIST[teltype]`, and
//	                           `CNormalNet::CommWarpList` then calls
//	                           `ui::UpdateData("warplist")` - the SAVED-TELEPORT
//	                           window, whose rows are your own saved points with
//	                           their memo. It does NOT open anything.
//	                           (source2\geClient\Source\GENormalNet_Session.cpp:959,
//	                            GEModeNet.cpp:282)
//	3742 ZC_WAYPOINT_LIST   -> `CNormalNet::WaypointList` fills
//	                           `Session().WAYPOINT_LIST` and updates
//	                           `"waypointlist"`. (GENormalNet_Session.cpp:401)
//	3753 ZC_UI_OPEN         -> `CCommander::OpenUi("waypointlist")` - this is what
//	                           OPENS the window. (Commander.cpp:2310, 658)
//
// "waypointlist" is the window the player calls the town teleport: the shipped
// client's own layout is `GEAndromida\tools\andro\ui\uixml\waypointlist.xml`, a
// 3x5 grid of `thumbnail_town0..14` with `button_town0..14`, `tax_town0..14`, a
// second tab `field` of 9 slots, and one `save` button whose script is
// `WAYPOINT_SAVE`. That grid is FIFTEEN towns, which is exactly how many rows in
// datatable_waypoint carry a restart warp id.
//
// So the empty 3634 that EntryBurst sends four times is honest when the player has
// no saved points yet, and it is also not the reason the structure did nothing:
// nothing sent 3742 and nothing sent 3753. Since the Warp List slice, all FOUR 3634
// lists carry the player's OWN saved points (BuildCommWarpListRows in burst.go, one
// list per teltype) and 3635 / 3636 / 3638 / 3639 are answered - see the handlers at
// the bottom of this file.
//
// WIRE LAYOUT, from source2 first and then CORRECTED BY THE BINARY (the binary
// wins, and here it does not agree):
//
//	geShared\include\geZCPacket.h:3239
//	  struct PKS_ZC_COMMWARP_LIST : public PKS_VARSIZE { BYTE teltype; BYTE curCount; }
//	geShared\include\geZCPacket.h:2522
//	  struct PKS_ZC_WAYPOINT_LIST : public PKS_VARSIZE {}   // (WAYPOINT_INFO) * count
//	geShared\include\geZCTypes.h:870
//	  struct WAYPOINT_INFO { MAP_ID mapID; WORD price; }     // 4 bytes - STALE
//	geShared\include\geSharedTypes.h:45
//	  typedef WORD MAP_ID;
//
// PKS_VARSIZE is cmd(2) + len(2), so source2's struct implies a 4-byte record and
// the packet carries NO count of its own: the client divides. The sender is
// `CCommander::SendWayPointList` (Commander.cpp:2182) and it sets
// `len = bin.GetPosition()`, the TOTAL written, header included.
//
// THE SHIPPED CLIENT DOES NOT MATCH THAT STRUCT. Its handler for 3742 is
// FUN_004b64ea (reached from the dispatch at FUN_005251e5, `param_1 == 0xe9e`):
//
//	0x4b64f6  CALL 0x004b5b3f              ; -> packet + 4
//	0x4b64fb  MOVZX ESI, word ptr [ESI+2]  ; the len word
//	0x4b6501  SUB ESI, 0x4
//	0x4b6504  SHR ESI, 0x1                 ; count = (len - 4) / 2   <-- /2, not /4
//	0x4b6512  CALL 0x009006b0              ; read 2 bytes
//	0x4b6522  ADD EDI, 0x2                 ; STRIDE = 2
//	0x4b651e  AND dword ptr [EBP-0x4], 0   ; the second field is forced to 0
//
// So the record on the wire is TWO bytes - the map id alone - and this client has
// no fare field at all: it zeroes what source2 called `price`. Sending 4 bytes
// makes it read twice as many towns and mis-segment every id after the first, so
// the stride here is 2, on the binary's authority and not on source2's.
const (
	// CmdZCWaypointList is ZC_WAYPOINT_LIST: the town grid's rows.
	CmdZCWaypointList = 3742
	// CmdZCUIOpen is ZC_UI_OPEN: cmd(2) + frame[32], no length word.
	CmdZCUIOpen = 3753

	// waypointHeader is PKS_ZC_WAYPOINT_LIST: cmd(2) + len(2).
	waypointHeader = 4
	// waypointRecord is the shipped client's stride: one WORD, the map id.
	// source2's WAYPOINT_INFO is 4 bytes and the client does not use it.
	waypointRecord = 2
	// uiOpenFrame is frameNameSize (geZCPacket.h:2558).
	uiOpenFrame = 32

	// WaypointListFrame is the frame name the client's own layout is filed
	// under: ge\ui\uixml\waypointlist.xml, `class="waypointlist"`.
	WaypointListFrame = "waypointlist"
)

// BuildWaypointList is ZC_WAYPOINT_LIST (3742): cmd, len, then one 2-byte map id
// per town, in the order given.
//
// The ORDER is the client's own datatable_waypoint row order, because the client
// fills grid slot i from record i. A sorted order would still draw fifteen towns,
// but not the arrangement the live server shows.
//
// len is the WHOLE packet including these four bytes: the client computes the
// record count as (len - 4)/2 (see the disassembly above), so an off-by-header or
// a 4-byte stride here draws the wrong number of towns with no error anywhere.
func BuildWaypointList(towns []WaypointTown) []byte {
	out := make([]byte, waypointHeader+waypointRecord*len(towns))
	binary.LittleEndian.PutUint16(out[0:], CmdZCWaypointList)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	for i, town := range towns {
		binary.LittleEndian.PutUint16(out[waypointHeader+i*waypointRecord:], uint16(town.MapID))
	}
	return out
}

// BuildUIOpen is ZC_UI_OPEN (3753): cmd, then the frame name in a fixed 32-byte
// field, zero padded, no length word.
//
// A frame name of 32 bytes or more would be cut, and a cut name opens nothing -
// so this refuses to guess: it truncates and lets the caller's log line name the
// frame it asked for.
func BuildUIOpen(frame string) []byte {
	out := make([]byte, 2+uiOpenFrame)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUIOpen)
	copy(out[2:], frame)
	return out
}

// describeTowns names the rows that did NOT reach the grid, as "location(id)".
func describeTowns(towns []WaypointTown) string {
	var b strings.Builder
	for i, t := range towns {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s(%d)", t.Location, t.ID)
	}
	return b.String()
}

// The town grid's two requests, plus the saved-teleport window's five. 3743 and
// 3468 are answered (waypoint_travel.go, start_point.go); 3635, 3636, 3638 and 3639
// are answered at the bottom of this file; 3637 (travel) is still logged and NOT
// answered - see zone.go's Reply.
//
// 3639 SWAP **IS** SENT BY THIS CLIENT, and the note that said otherwise was wrong
// (corrected 2026-10-30). The owner pressed Up and Down in the window in game and the
// zone logged the request six times; logs\zone15_live.log 15:07:46 has the first one,
// 19 bytes:
//
//	37 0e 76 00 00 00 34 00 00 00 04 00 00 00 03 00 00 00 00
//	cmd 3639 | seq 0x76 | checksum 0x34 | from 4 | to 3 | teltype 0
//
// so `from` and `to` are u32 DISPLAY indices at +10 and +14, and the teltype is the
// byte at +18. He pressed Up on the last row of five, which is index 4 -> 3.
const (
	// CmdCZWaypointSelect is CZ_WAYPOINT_SELECT: a click on a town thumbnail.
	// `WaypointSelect` sends the record's own mapID (script.cpp:1655-1663).
	CmdCZWaypointSelect = 3743
	// CmdCZSaveRestartWarpID is CZ_SAVE_RESTART_WARPID: the grid's "Save as Start
	// Point" button, whose script is `SaveRestartWarpID()` (game.scp:381). It is
	// ANSWERED now - but not in this file: the request's body carries no field it
	// could be read from, and its answer is its own window's business, so both live
	// in zone\start_point.go.
	CmdCZSaveRestartWarpID = 3468
	// The four ids below belong to the SAVED-teleport window (`warplist`, fed by
	// 3634), which is NOT the town grid. SAVE / REMOVE / RENAME / SWAP are answered
	// by the handlers at the bottom of this file; SELECT (travel) is logged only.
	CmdCZCommWarpSelect = 3637
	CmdCZCommWarpSave   = 3635
	CmdCZCommWarpRemove = 3636
	CmdCZCommWarpRename = 3638
	// CmdCZCommWarpSwap is the window's Up / Down: it names two DISPLAY rows and
	// means "move the first one to where the second one is". Proven on the wire, not
	// from source2 - see the note above this block.
	CmdCZCommWarpSwap = 3639
)

// commWarpRequestName names the requests above, for the log line.
func commWarpRequestName(cmd uint16) string {
	switch cmd {
	case CmdCZWaypointSelect:
		return "CZ_WAYPOINT_SELECT"
	case CmdCZSaveRestartWarpID:
		return "CZ_SAVE_RESTART_WARPID"
	case CmdCZCommWarpSelect:
		return "CZ_COMMWARP_SELECT"
	case CmdCZCommWarpSave:
		return "CZ_COMMWARP_SAVE"
	case CmdCZCommWarpRemove:
		return "CZ_COMMWARP_REMOVE"
	case CmdCZCommWarpRename:
		return "CZ_COMMWARP_RENAME"
	case CmdCZCommWarpSwap:
		return "CZ_COMMWARP_SWAP"
	}
	return "unknown-request"
}

// THE WARP LIST'S FOUR WRITES: SAVE (3635), REMOVE (3636), RENAME (3638) and the
// Up/Down SWAP (3639).
//
// LAYOUT, measured from the shipped binary's own senders (FUN_00500640 for the
// save, FUN_00500576 for the remove, FUN_005005c6 for the rename) and, for the swap,
// from the LIVE CAPTURE below - the binary's sender for 3639 was never found, and the
// captured bytes are better evidence than a sender anyway. All four carry
// PKS_SECURITY_DEFAULT first - cmd u16, seq u32, checksum u32 - so the payload starts
// at +10:
//
//	3635 SAVE    teltype u8 @+10, memo char[31] @+11                        42 bytes
//	3636 REMOVE  index   u8 @+10, teltype u8 @+11                           12 bytes
//	3638 RENAME  index   u8 @+10, teltype u8 @+11, memo char[31] @+12       43 bytes
//	3639 SWAP    from u32 @+10, to u32 @+14, teltype u8 @+18                19 bytes
//
// `teltype` NAMES THE LIST, and there are four of them. 0 is the basic "Warp List",
// 1-3 are "Warp Extend(1)".."(3)", and every one of the four is served from
// `saved_teleport` (migration 00012). The client's own COMMWARP_SAVE picks the first
// panel holding fewer than ten rows - `for(telType = 0, 3) { if( CommWarpCount(telType)
// < 10 ) { CommWarpSave(telType, memo); return; } }` in ge\ui\uiscp\game.scp - checks no
// item and charges nothing, so nothing here refuses a save, remove, rename or swap for
// naming a list. That is the owner's ruling: "nothing was paid lol. just serve all 40."
//
// The swap is the odd one: its two indices are FOUR bytes each, while the remove's and
// the rename's are one. That is not a guess - the captured packet is 19 bytes and its
// bytes read `04 00 00 00` then `03 00 00 00` then `00`.
//
// THERE IS NO REPLY PACKET FOR ANY OF THEM. The zone dispatcher FUN_004d76fd has no
// case for 3635-3639 (they fall to the default), and the client has no receive
// handler either, so a "saved OK" reply would be an invention nobody reads. The ONLY
// confirmation this window understands is a REFRESH: 3634 for all four teleport
// types, exactly as the entry burst already sends them. That is what a successful
// save, remove or rename answers with - and nothing else.
//
// `index` is the row's POSITION IN THE LIST THE CLIENT WAS LAST SENT, never a
// dataase slot: a remove leaves a gap in the slot numbers and does not renumber, so
// row 2 can well be slot 3. Since R76 slice 6 it is also a position WITHIN the list the
// packet names (`teltype`), not within a global forty-row list: each of the four panels
// is its own list with its own numbering, so row 2 of "Warp Extend(2)" is that panel's
// third row and has nothing to do with the basic list's row 2.
//
// THEN there is the human line. Since R76 slice 5 (2026-10-30) a successful SAVE also
// sends ONE ZC_MESSAGE (3596) - "The warp location has been saved successfully" - the
// LIVE REFERENCE SERVER's own wording, photographed by the owner. The refresh confirms
// the LIST; the message confirms the EVENT. A remove, rename or swap still answers with
// the four lists alone, because only the save was seen on the reference.
//
// REFUSALS change nothing, so they never re-send the list: the list the client holds
// is already true, and re-sending it on every click of a full list would say "nothing
// happened" in a packet. They DO send ONE ZC_MESSAGE (3596) line and print exactly
// ONE log line - see the note above commWarpRefuse.
const (
	// commWarpReqHeader is PKS_SECURITY_DEFAULT: cmd(2) + seq(4) + checksum(4).
	commWarpReqHeader = 10
	// The save: the list selector, then the text the player typed.
	commWarpSaveTelType = commWarpReqHeader
	commWarpSaveMemo    = commWarpReqHeader + 1
	commWarpMemoField   = 31
	commWarpSaveSize    = commWarpSaveMemo + commWarpMemoField
	// The remove and the rename both name a row first, then the list.
	commWarpRowIndex   = commWarpReqHeader
	commWarpRowTelType = commWarpReqHeader + 1
	commWarpRowSize    = commWarpRowTelType + 1
	// The rename's new text, after the row and the list.
	commWarpRenameMemo = commWarpReqHeader + 2
	commWarpRenameSize = commWarpRenameMemo + commWarpMemoField
	// The swap names TWO rows, each a u32, then the list. Its three payload fields
	// and its size are pinned to the captured 19-byte packet (see the note above).
	commWarpSwapFrom    = commWarpReqHeader
	commWarpSwapTo      = commWarpReqHeader + 4
	commWarpSwapTelType = commWarpReqHeader + 8
	commWarpSwapSize    = commWarpSwapTelType + 1
	// commWarpMemoChars is the client's own cap: `strncpy_s(dst, 0x1f, ...)` and
	// `InputString(..., 30)` in UIWarpListFrame.cpp, i.e. 30 characters plus a NUL.
	commWarpMemoChars = 30
)

// cutCString cuts a client STRING FIELD at its FIRST NUL byte.
//
// This is the trap in this file, and it cost the owner every save he tried in game.
// A `char[N]` field written by this client is a C string: the text the player typed,
// a NUL terminator, and then whatever was left on the client's own stack behind it.
// Trimming NULs from the END keeps everything AFTER an interior NUL, so a
// four-character memo arrived as
//
//	"lala\x00\xd4\xff\x19\x00!\x12X\x00..."
//
// and PostgreSQL refused the row outright: `invalid byte sequence for encoding
// "UTF8": 0x00 (SQLSTATE 22021)`. Every save failed that way on 2026-10-30
// (logs\zone15_live.log, lines 19135-19142), and from the client it looked like
// "the save button does nothing".
//
// So: everything from the first NUL onward is NOT part of the memo. It must be
// dropped, never kept and never trimmed off the ends.
func cutCString(raw []byte) string {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		return string(raw[:i])
	}
	return string(raw)
}

// ParseCommWarpSave reads teltype and the memo out of CZ_COMMWARP_SAVE (3635).
//
// ok is false only when the teltype byte is not even there - a malformed client,
// not a crash. The memo is taken as far as the packet goes, up to its 31 bytes, and
// CUT AT ITS FIRST NUL: a client that sends only the text describes the same memo as
// one that pads the whole field (see cutCString for why the ends must not be
// trimmed).
func ParseCommWarpSave(pt []byte) (telType int, memo string, ok bool) {
	if len(pt) < commWarpSaveTelType+1 {
		return 0, "", false
	}
	end := commWarpSaveMemo + commWarpMemoField
	if end > len(pt) {
		end = len(pt)
	}
	return int(pt[commWarpSaveTelType]), cutCString(pt[commWarpSaveMemo:end]), true
}

// ParseCommWarpRow reads the list index and the teltype out of CZ_COMMWARP_REMOVE
// (3636) or CZ_COMMWARP_RENAME (3638): the same two bytes at +10 and +11.
//
// The index is NOT range-checked here: the refusal belongs where it is compared
// against the list, so the log line can say what number arrived.
func ParseCommWarpRow(pt []byte) (index, telType int, ok bool) {
	if len(pt) < commWarpRowSize {
		return 0, 0, false
	}
	return int(pt[commWarpRowIndex]), int(pt[commWarpRowTelType]), true
}

// ParseCommWarpRename reads CZ_COMMWARP_RENAME (3638) whole: the row, the list, and
// the new text. The memo is cut at its first NUL, the same way the save's is.
func ParseCommWarpRename(pt []byte) (index, telType int, memo string, ok bool) {
	index, telType, ok = ParseCommWarpRow(pt)
	if !ok {
		return 0, 0, "", false
	}
	end := commWarpRenameMemo + commWarpMemoField
	if end > len(pt) {
		end = len(pt)
	}
	return index, telType, cutCString(pt[commWarpRenameMemo:end]), true
}

// ParseCommWarpSwap reads CZ_COMMWARP_SWAP (3639) whole: the row that moves, the row it
// moves to, and the teleport list it belongs to.
//
// Both indices are u32 - that is what the captured 19-byte packet carries, and it is the
// one place this window's layout differs from 3636 / 3638, which use one byte each. They
// are DISPLAY indices: the row numbers the client drew, which is the only order it knows.
//
// Neither is range-checked here: the refusal belongs where it is compared against the
// list, so the log line can say which number was wrong.
func ParseCommWarpSwap(pt []byte) (from, to, telType int, ok bool) {
	if len(pt) < commWarpSwapSize {
		return 0, 0, 0, false
	}
	return int(binary.LittleEndian.Uint32(pt[commWarpSwapFrom:])),
		int(binary.LittleEndian.Uint32(pt[commWarpSwapTo:])),
		int(pt[commWarpSwapTelType]), true
}

// THE GO BUTTON: CZ_COMMWARP_SELECT (3637), and the ONE Warp Scroll a jump costs.
//
// WHAT THE CLIENT DOES FIRST, in its own script - the only reason this request ever
// reaches us with a scroll in the bag and never without one:
//
//	ge\ui\uiscp\game.scp:174-196 (THE COPY THE RUNNING CLIENT USES:
//	 GEAndromida\tools\andro\ui\uiscp\game.scp - the extracted tree at
//	 ge\ui\uiscp\game.scp carries a Korean literal in that MsgBox instead of the
//	 token, so quote the GEAndromida copy)
//	  func UI_COMMWARP_EXECUTE(frame, control, arg1, arg2)
//	  {
//	      has       = IsItemInInventory('WarpScroll', 1);
//	      has_event = IsItemInInventory('WarpScroll_Event', 1);
//	      if ('YES' == has || 'YES' == has_event) {
//	          var TelType, SelectedIndex = GET_SELECTED_WARP_INFO(frame);
//	          if (TelType == -1) { return; }
//	          var useevent = false;
//	          if ('YES' == has_event) { useevent = true; }
//	          CommWarpSelect(SelectedIndex, TelType, useevent);
//	      } else {
//	          MsgBox("<$>79767</>");   // "you need Warp Scroll or Warp Scroll for event"
//	      }
//	  }
//
// and `GET_SELECTED_WARP_INFO` (game.scp:145-172) answers the ONE selected row and the
// panel it was selected in - `warplist` 0, `cashwarplist` 1, `_2` 2, `_3` 3 - which is
// the same `index` + `teltype` pair the four writes carry.
//
// THE SENDER: `packet::CZ_CommWarp_Select_New(index, teltype, isevent)`
// (source2\geClient\Source\GENormalSendPacket.cpp:632-641). `script.cpp:1344` may hold
// the request for a localised delay first (geLocalize::GetUseWarpDelay, a 5 s
// CCmdCreateWarp), and that command's own Do() re-enables the button and THEN sends:
//
//	GENormalCommand.cpp:49-53
//	  void CCmdCreateWarp::Do()
//	  { ui::Enable("warplist", "executewarp"); ui::UpdateData("warplist");
//	    packet::CZ_CommWarp_Select_New(m_index, m_isCash, m_isevent); }
//
// so there is STILL no reply packet to invent: the client re-enables its own button.
//
// THE STRUCT, and the client's own table agrees with it byte for byte:
//
//	source2\geShared\include\geZCPacket.h:3254
//	  struct PKS_CZ_COMMWARP_SELECT : public PKS_SECURITY_DEFAULT {
//	      BYTE index; BYTE teltype; bool isEvent; };
//	source2\geShared\include\gePacketBase.h:52-56  (#pragma pack(1) above it)
//	  struct PKS_SECURITY_DEFAULT { WORD command; DWORD seq; int checksum; };
//	docker-port\data\cmd_table.txt:1112          3637  13  PKS_CZ_COMMWARP_SELECT
//
// 10 + 1 + 1 + 1 = 13, which is exactly the registered size - so:
//
//	3637 SELECT  index u8 @+10, teltype u8 @+11, isEvent u8 @+12        13 bytes
//
// `index` is the row's position INSIDE the list `teltype` names (see the note above the
// four writes), `isEvent` is 1 when the client is about to spend the EVENT scroll.
//
// THE COST, by the owner's decision (2026-10-30, "yes" to one consumed per jump): ONE
// scroll, and which one is the client's own choice - `isEvent` picks the event scroll.
// The two class ids are the client's own table's (`ge\xml\datatable_item_etc.xml`,
// ClassID 21006 ClassName="WarpScroll" and ClassID 21446 ClassName="WarpScroll_Event";
// data\item_catalog.json agrees: [21006,1,32767] and [21446,1,32767]).
const (
	// warpScrollClass is "WarpScroll": the ordinary scroll, 1 piece = 1 jump.
	warpScrollClass int64 = 21006
	// warpScrollEventClass is "WarpScroll_Event": the event scroll, spent only when
	// the client says it means to (isEvent at +12).
	warpScrollEventClass int64 = 21446
	// warpScrollPerJump is what one jump costs. It is a CONSTANT and not a field:
	// a fare that can be edited from outside is a number nobody audited.
	warpScrollPerJump int64 = 1

	// The payload of 3637, at the offsets the struct above gives it.
	commWarpSelectIndex   = commWarpReqHeader     // +10
	commWarpSelectTelType = commWarpReqHeader + 1 // +11
	commWarpSelectIsEvent = commWarpReqHeader + 2 // +12
	commWarpSelectSize    = commWarpReqHeader + 3 // 13, the client's registered size
)

// scrollClassName is the item table's own ClassName for the two scrolls, for the log.
func scrollClassName(classID int64) string {
	if classID == warpScrollEventClass {
		return "WarpScroll_Event"
	}
	return "WarpScroll"
}

// firstStackOfClass is the bag row a scroll charge takes: the OLDEST row of that class,
// which is the bag's own order and the same rule a class-based take follows. ok is false
// when the bag holds none.
func firstStackOfClass(stacks []item.Stack, classID int64) (item.Stack, bool) {
	for _, st := range stacks {
		if st.ClassID == classID && st.Count > 0 {
			return st, true
		}
	}
	return item.Stack{}, false
}

// ParseCommWarpSelect reads CZ_COMMWARP_SELECT (3637) whole: the row, the list it is a
// row of, and which scroll the client intends to spend.
//
// ok is false only when the packet cannot hold those three bytes - a malformed client,
// not a crash. The index is NOT range-checked here: the refusal belongs where it is
// compared against the list, so the log line can say what number arrived.
func ParseCommWarpSelect(pt []byte) (index, telType int, isEvent bool, ok bool) {
	if len(pt) < commWarpSelectSize {
		return 0, 0, false, false
	}
	return int(pt[commWarpSelectIndex]), int(pt[commWarpSelectTelType]),
		pt[commWarpSelectIsEvent] != 0, true
}

// capWarpMemo applies the client's own 30-character limit to a memo the server is
// about to store. cut is true when it had to shorten the text, so the caller can log
// that once rather than silently renaming the player's point.
func capWarpMemo(memo string) (capped string, cut bool) {
	r := []rune(memo)
	if len(r) <= commWarpMemoChars {
		return memo, false
	}
	return string(r[:commWarpMemoChars]), true
}

// THE PLAYER-FACING HALF OF A REFUSAL (2026-10-30).
//
// Why this exists: this window has NO reply packet at all (see the note above), so a
// SUCCESS is confirmed only by the four re-sent 3634 lists - the row appears. A REFUSAL
// sends no list, because nothing changed and the list the client holds is still true.
// From the player's side that made a refused click look like a dead button. The owner
// hit it twice in one evening and asked, in his words: "the eleventh is silent fail? is
// it intended?" (logs\zone15_live.log 15:54:59 and 15:55:03).
//
// So every refusal now ALSO sends ONE ZC_MESSAGE (3596, BuildMessage) naming the reason
// in plain words. The log line stays the record; the message is for the player.
//
// THE RULES THESE TEXTS FOLLOW, and why:
//
//   - ASCII only and no locale tokens. The client copies the line into `char txt[256]`
//     (GEPacket.cpp, packet::ServerMsg) and draws it in the system font - cyan, since
//     type/level cannot set a colour. A locale token would need the client's own table
//     and would read as the wrong sentence in another language pack.
//   - SHORT. Everything below is well under a hundred bytes, so the 256-byte buffer and
//     the packet's own length word can never disagree.
//   - No invented numbers. The only number in a text is the cap, and it is read from
//     MaxSavedTeleports - the same constant the store enforces and the entry burst tells
//     the client as Lock_Warp.
//   - ONE message per save, never more: one success line, or one refusal line. Two
//     signals for one event is noise.
//   - THE TWO SAVE LINES ARE THE REFERENCE'S OWN WORDS, VERBATIM. "The warp location
//     has been saved successfully" and "This warp point has already been saved."
//     were read off the live reference server from the owner's screenshots, so they
//     are copied, not written: no paraphrase, no translation, no full stop added or
//     dropped.
const (
	// warpSavedText: the reference's SUCCESS line. Its wording is the point of it: a
	// save on the live reference server prints exactly this as a system line.
	warpSavedText = "The warp location has been saved successfully"
	// warpDuplicateText: the reference's DUPLICATE line - the same spot saved twice. The
	// duplicate is decided by COORDINATES, never by the name and never by the list (see
	// sameSpot and the store), but the sentence is the reference's and is copied byte
	// for byte, full stop included.
	warpDuplicateText = "This warp point has already been saved."
	// warpShortText: the packet was too short to even hold the field it names.
	warpShortText = "The server could not read that request. Nothing was changed."
	// warpNoStoreText: the zone started without a saved-point store wired in.
	warpNoStoreText = "Saved points are not available on this server. Nothing was changed."
	// warpBadListText: the teltype byte names no panel. The client draws four
	// (MAX_TELEPORT_TYPE), so anything else is a broken client rather than a fifth list -
	// and it is NOT the "this server does not serve that list" refusal, which is gone:
	// types 1-3 are served now.
	warpBadListText = "That Warp List does not exist. Nothing was changed."
	// warpBadNameText: the memo is not UTF-8 text, so PostgreSQL would refuse the row.
	warpBadNameText = "That name cannot be stored here. Nothing was changed."
	// warpNoPositionText: the session was never placed, so there is nowhere to save.
	warpNoPositionText = "The server does not know where your character is. Nothing was saved."
	// warpBadSpotText: the leader's position is not a plausible navmesh cell.
	warpBadSpotText = "This spot cannot be saved. Nothing was saved."
	// warpSaveFailedText: the store's own write failed.
	warpSaveFailedText = "The server could not save the point. Nothing was changed."
	// warpLoadText: reading the list back failed.
	warpLoadText = "The server could not read your Warp List. Nothing was changed."
	// warpRemoveFailedText: the store's delete failed.
	warpRemoveFailedText = "The server could not remove the point. Nothing was changed."
	// warpRenameFailedText: the store's rename failed.
	warpRenameFailedText = "The server could not rename the point. Nothing was changed."
	// warpRenameGoneText: the row vanished between the read and the write.
	warpRenameGoneText = "That row no longer exists. Nothing was renamed."
	// warpMoveFailedText: the store's reorder failed.
	warpMoveFailedText = "The server could not move the row. Nothing was changed."
	// warpNoSuchRowText: an index that names no row (remove / rename / swap / travel).
	warpNoSuchRowText = "That row is not in your Warp List. Nothing was changed."
	// THE GO BUTTON'S OWN THREE, since a jump can be refused for reasons the four
	// writes cannot: a scroll that is not there, a scroll that could not be spent, and a
	// destination that cannot be reached right now. Each says "nothing was charged",
	// because that is the half of it a player can check in the bag.
	warpNoScrollText = "You need a Warp Scroll in your bag to travel. Nothing was charged."
	// warpScrollChargeText: the item transaction itself failed.
	warpScrollChargeText = "The server could not use the Warp Scroll. Nothing was charged."
	// warpTravelRefusedText: a point that cannot be landed on, or a destination no
	// running zone serves. Both are "not now", never "you paid and nothing happened".
	warpTravelRefusedText = "That warp point cannot be used right now. Nothing was charged."
	// warpBusyText: a map change is already running, the same one-at-a-time rule the
	// town grid and the teleportation window keep.
	warpBusyText = "A map change is already running. Nothing was charged."
	// warpNoItemsText: no bag reader or no item write path is wired in - a server that
	// could not charge, so it must not move anybody either.
	warpNoItemsText = "The server cannot use items right now. Nothing was charged."
)

// warpListName names one of the four panels the way the client's own layout does: the
// first is the "Warp List", the other three are "Warp Extend(1)".."(3)". It exists so a
// refusal can NAME the list it is about, which is the difference between "your list is
// full" and "Warp Extend(2) is full" once there are four.
func warpListName(telType int) string {
	if telType == 0 {
		return "Warp List"
	}
	return fmt.Sprintf("Warp Extend(%d)", telType)
}

// warpFullText names the cap the save hit. The number comes from MaxSavedTeleports, so
// moving the cap moves this sentence with it and can never disagree with the store.
func warpFullText() string {
	return warpFullTextFor(commWarpOwnList)
}

// warpFullTextFor is warpFullText for ONE list: the cap counts rows in that list, and
// the sentence names it, because with four lists "the Warp List is full" is ambiguous
// while "Warp Extend(2) is full" is not. The other three lists are untouched by a full
// one - that is what forty points means - so the advice stays "remove a point", from
// that list.
func warpFullTextFor(telType int) string {
	return fmt.Sprintf("%s is full: %d points is the maximum here. Nothing "+
		"was saved - remove a point first.", warpListName(telType), MaxSavedTeleports)
}

// warpBadListWhy is the refusal reason for a teltype byte naming no panel, so the log
// says which number arrived and how many lists the client draws.
func warpBadListWhy(telType int) string {
	return fmt.Sprintf("teltype %d names no Warp List - the client draws %d of them "+
		"(0..%d), and all %d are served here", telType, SavedTeleportLists,
		SavedTeleportLists-1, SavedTeleportLists)
}

// commWarpRefuse is the ONE refusal path for the Warp List's four writes: ONE log line
// naming what was refused, why, and the text that was thrown away - plus ONE
// ZC_MESSAGE line telling the PLAYER the same thing in plain words.
//
// Nothing is written, and the four 3634 lists are NOT re-sent: a refusal changed no
// row, so the list the client holds is still true. The message is the only thing that
// goes out, and it is what stops a refused click reading as a dead button.
//
// `text` must be ASCII, short and free of locale tokens (see the block above).
func (h *Handler) commWarpRefuse(name string, commanderID int32, why, note, text string) [][]byte {
	h.Log.Printf("zone: %s for commander %d REFUSED - %s%s; nothing was written and the "+
		"player was told with one cmd %d line", name, commanderID, why, note, CmdZCMessage)
	return [][]byte{BuildMessage(text, MessageTypeGeneral, MessageLevelGeneral)}
}

// discardNote is the tail of a refusal line that names the memo it dropped, so "I
// typed a name and it vanished" is answerable from the log alone.
func discardNote(memo string) string {
	if memo == "" {
		return "; no memo was sent"
	}
	return fmt.Sprintf("; the memo %q was DISCARDED", summariseMemo(memo))
}

// invalidMemoWhy is the refusal reason for a memo that is not UTF-8 text. It is
// checked BEFORE the store is asked to write, so the store's own complaint is never
// the first thing a reader sees: PostgreSQL's `invalid byte sequence for encoding
// "UTF8": 0x00 (SQLSTATE 22021)` named a slot, a commander and a raw string, and said
// nothing about which packet sent it. Naming the bytes here answers that.
func invalidMemoWhy(memo string) string {
	return fmt.Sprintf("the memo is not UTF-8 text - its bytes are % x - and PostgreSQL "+
		"refuses such a value (SQLSTATE 22021)", []byte(memo))
}

// commWarpSave answers CZ_COMMWARP_SAVE (3635): put where the leader stands into the
// list the packet names (teltype at +10 - one of the four panels), then answer with the
// four 3634 lists AND one system line.
//
// THAT LINE IS THE REFERENCE'S. A save on the live reference server prints "The warp
// location has been saved successfully" (R76 slice 5, the owner's screenshot), so this
// answers the same way: 3634 x4 refreshes the LIST, the 3596 line tells the PLAYER. A
// spot the commander already holds is refused instead, with the reference's own "This
// warp point has already been saved." - and the test for it is the four COORDINATES,
// never the name (see sameSpot).
//
// The position is `sess.walkedPos(now)` and not `sess.Pos`, for the reason
// saved_teleport.go gives at its own save: CZ_MOVE_PC carries the DESTINATION of a
// click, so sess.Pos is where the character is going. The saved triple is {x, y,
// CELL} - a cell, never a height.
func (h *Handler) commWarpSave(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_COMMWARP_SAVE", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil || h.teleports.Save == nil {
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID,
			"no saved-point store is wired in", "", warpNoStoreText), nil
	}

	telType, raw, ok := ParseCommWarpSave(f.Data)
	if !ok {
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, fmt.Sprintf(
			"the packet is %d bytes and a save is %d (a 10-byte security header, the "+
				"teltype at +10, a 31-byte memo at +11), so the teltype is not even there",
			len(f.Data), commWarpSaveSize), "", warpShortText), nil
	}
	if !validTelType(telType) {
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, warpBadListWhy(telType),
			discardNote(raw), warpBadListText), nil
	}
	if !utf8.ValidString(raw) {
		// Never hand a broken string to the store: its error names a slot and a raw
		// string, not the packet that sent it.
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, invalidMemoWhy(raw), "",
			warpBadNameText), nil
	}

	now := time.Now()
	pos := sess.walkedPos(now)
	if pos == [3]int32{} {
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID,
			"the leader has no position yet (0, 0, cell 0), so there is nowhere to save",
			discardNote(raw), warpNoPositionText), nil
	}
	if !PlausibleCell(pos[2]) {
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, fmt.Sprintf(
			"the leader stands at (%d, %d, cell %d) and %v", pos[0], pos[1], pos[2],
			badCellError(pos[2])), discardNote(raw), warpBadSpotText), nil
	}

	// The MEMO is what this window saves, and it goes into the row's `name` column:
	// the next 3634 sends it back as the memo the client draws.
	//
	// `raw` is ALREADY the cut memo (cutCString), so the count below is the memo's own
	// length - not the width of the 31-byte field, which made a four-character memo
	// read as "31 character(s)" in the live log.
	memo, cut := capWarpMemo(raw)
	if cut {
		h.Log.Printf("zone: CZ_COMMWARP_SAVE for commander %d: the memo was %d "+
			"character(s) and this client accepts %d, so the row is named %q",
			commanderID, len([]rune(raw)), commWarpMemoChars, memo)
	}

	// THE LIST IS THE ONE THE CLIENT PICKED (teltype at +10). A save fills that list; the
	// other three are untouched. And `want` carries it, because the store decides the
	// slot within the list and nothing else.
	want := SavedPoint{TelType: telType, Slot: -1, MapID: h.MapID, Pos: pos, Name: memo}
	got, err := h.teleports.Save(ctx, commanderID, telType, -1, want)
	switch {
	case errors.Is(err, errDuplicatePoint):
		// THE SPOT IS ALREADY SAVED, IN ONE OF THE COMMANDER'S FORTY POINTS. The store
		// compares the four coordinates it just read under its own lock, so neither the
		// name the player typed NOR the list the save names enters the test: the same
		// map+x+y+cell saved into "Warp Extend(2)" is the same place as one already in the
		// basic list, and the reference's line does not care which panel holds it.
		//
		// THE DUPLICATE IS ANSWERED FIRST, IN THE STORE - before the cap. On a full list
		// both reasons are true, and this one is the useful sentence: removing a point
		// would not make a save on an already-saved spot legal. The order lives in
		// PGStore.SaveSavedTeleport, so read it there rather than here.
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, fmt.Sprintf(
			"a point is already saved at map %d (%d, %d, cell %d); a duplicate is the "+
				"same map and the same x, y and cell ANYWHERE in the commander's lists, "+
				"never the same name", h.MapID, pos[0], pos[1], pos[2]),
			discardNote(memo), warpDuplicateText), nil
	case errors.Is(err, errNoFreeSlot):
		// The store owns the slot decision, so the full-list check lives there and
		// its refusal arrives here. Nothing was written, and the other lists are not
		// affected: the message names THIS list.
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID, fmt.Sprintf(
			"%s (teltype %d) already holds %d row(s), which is this server's cap per "+
				"list (the client was told %d on entry as Lock_Warp) and this server "+
				"does not delete the oldest point to make room; the other lists are "+
				"unaffected", warpListName(telType), telType, MaxSavedTeleports,
			MaxSavedTeleports),
			discardNote(memo), warpFullTextFor(telType)), nil
	case err != nil:
		return h.commWarpRefuse("CZ_COMMWARP_SAVE", commanderID,
			fmt.Sprintf("the write failed: %v", err), discardNote(memo), warpSaveFailedText), nil
	}

	rows, lerr := h.teleports.Load(ctx, commanderID)
	if lerr != nil {
		// The row IS written: say so, or a reader would think the save failed.
		h.Log.Printf("zone: CZ_COMMWARP_SAVE for commander %d: SAVED type%d slot %d map "+
			"%d (%s) at (%d, %d, cell %d) memo %q, but reading the list back failed: %v "+
			"- the window was NOT refreshed", commanderID, got.TelType, got.Slot,
			got.MapID, fromOr(got.Name, h.MapName), got.Pos[0], got.Pos[1], got.Pos[2],
			summariseMemo(got.Name), lerr)
		return nil, nil
	}
	out := CommWarpPackets(rows)
	// THE SUCCESS LINE, and it is the reference's own. The four 3634 lists redraw the
	// window; this one 3596 line is the human confirmation, and the reference server
	// prints it on every save. Appended LAST, so a client that draws as it reads has the
	// refreshed list behind the line rather than in front of it.
	out = append(out, BuildMessage(warpSavedText, MessageTypeGeneral, MessageLevelGeneral))
	h.Log.Printf("zone: CZ_COMMWARP_SAVE for commander %d: SAVED type%d (%s) slot %d map "+
		"%d (%s) at (%d, %d, cell %d) memo %q; 3634 re-sent %d time(s) plus one cmd %d "+
		"confirmation line (%q) - %s",
		commanderID, got.TelType, warpListName(got.TelType), got.Slot, got.MapID,
		fromOr(got.Name, h.MapName), got.Pos[0], got.Pos[1], got.Pos[2],
		summariseMemo(got.Name), len(out)-1, CmdZCMessage,
		warpSavedText, CommWarpSummary(rows))
	return out, nil
}

// commWarpRemove answers CZ_COMMWARP_REMOVE (3636): delete the point the player
// selected, then re-send 3634 four times.
//
// The number on the wire is the LIST ROW, so it goes through listAt against the list
// the client was last sent. Deleting by the number directly would delete the wrong
// point as soon as one gap exists. A remove does NOT renumber the other slots.
func (h *Handler) commWarpRemove(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_COMMWARP_REMOVE", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil || h.teleports.Remove == nil {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID,
			"no saved-point store or no remove hook is wired in", "", warpNoStoreText), nil
	}
	index, telType, ok := ParseCommWarpRow(f.Data)
	if !ok {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID, fmt.Sprintf(
			"the packet is %d bytes and a remove is %d (a 10-byte security header, the "+
				"row at +10 and the teltype at +11), so the row is not even there",
			len(f.Data), commWarpRowSize), "", warpShortText), nil
	}
	if !validTelType(telType) {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID, warpBadListWhy(telType),
			"", warpBadListText), nil
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID,
			fmt.Sprintf("reading the saved points failed: %v", err), "", warpLoadText), nil
	}
	// The row number is a position INSIDE the list the client named, not inside a flat
	// forty-row collection: each panel numbers its own rows from 0.
	rows = rowsOfType(rows, telType)
	target, found := listAt(rows, index)
	if !found {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID, fmt.Sprintf(
			"the client named row %d of type%d (%s) and that list has %d point(s) [%s]",
			index, telType, warpListName(telType), len(rows), describePoints(rows)), "",
			warpNoSuchRowText), nil
	}
	gone, deleted, err := h.teleports.Remove(ctx, commanderID, telType, target.Slot)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_REMOVE", commanderID, fmt.Sprintf(
			"deleting type%d slot %d failed: %v", telType, target.Slot, err), "",
			warpRemoveFailedText), nil
	}

	after, lerr := h.teleports.Load(ctx, commanderID)
	if lerr != nil {
		// The row IS gone, so say so - a reader must not think the delete failed.
		h.Log.Printf("zone: CZ_COMMWARP_REMOVE (type%d row %d) for commander %d: slot %d "+
			"was %s, but reading the list back failed: %v - the window was NOT refreshed",
			telType, index, commanderID, target.Slot, wasDeleted(deleted), lerr)
		return nil, nil
	}
	out := CommWarpPackets(after)
	h.Log.Printf("zone: CZ_COMMWARP_REMOVE (type%d row %d) for commander %d: slot %d map "+
		"%d (%s) at (%d, %d, cell %d) %s; 3634 re-sent %d time(s) - %s", telType, index,
		commanderID, gone.Slot, gone.MapID, fromOr(gone.Name, h.MapName), gone.Pos[0],
		gone.Pos[1], gone.Pos[2], wasDeleted(deleted), len(out), CommWarpSummary(after))
	return out, nil
}

// commWarpRename answers CZ_COMMWARP_RENAME (3638): change the selected point's
// memo, then re-send 3634 four times. The position, the map and the slot are NOT
// touched - a rename cannot move a point.
func (h *Handler) commWarpRename(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_COMMWARP_RENAME", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil || h.teleports.Rename == nil {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID,
			"no saved-point store or no rename hook is wired in", "", warpNoStoreText), nil
	}
	index, telType, raw, ok := ParseCommWarpRename(f.Data)
	if !ok {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, fmt.Sprintf(
			"the packet is %d bytes and a rename is %d (a 10-byte security header, the "+
				"row at +10, the teltype at +11 and a 31-byte memo at +12)",
			len(f.Data), commWarpRenameSize), "", warpShortText), nil
	}
	if !validTelType(telType) {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, warpBadListWhy(telType),
			discardNote(raw), warpBadListText), nil
	}
	if !utf8.ValidString(raw) {
		// Same rule as the save: a memo the store could never take is refused here,
		// with one line naming its bytes, and nothing is written.
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, invalidMemoWhy(raw), "",
			warpBadNameText), nil
	}

	// `raw` is already the cut memo, so this counts the memo and not the 31-byte field.
	memo, cut := capWarpMemo(raw)
	if cut {
		h.Log.Printf("zone: CZ_COMMWARP_RENAME for commander %d: the memo was %d "+
			"character(s) and this client accepts %d, so it is stored as %q",
			commanderID, len([]rune(raw)), commWarpMemoChars, memo)
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID,
			fmt.Sprintf("reading the saved points failed: %v", err), "", warpLoadText), nil
	}
	// The row number is a position inside the list the client named (see commWarpRemove).
	rows = rowsOfType(rows, telType)
	target, found := listAt(rows, index)
	if !found {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, fmt.Sprintf(
			"the client named row %d of type%d (%s) and that list has %d point(s) [%s]",
			index, telType, warpListName(telType), len(rows), describePoints(rows)),
			discardNote(memo), warpNoSuchRowText), nil
	}
	got, renamed, err := h.teleports.Rename(ctx, commanderID, telType, target.Slot, memo)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, fmt.Sprintf(
			"renaming type%d slot %d failed: %v", telType, target.Slot, err), "",
			warpRenameFailedText), nil
	}
	if !renamed {
		// That list came from the same store a moment ago, so this is a race or a
		// store bug - either way nothing changed.
		return h.commWarpRefuse("CZ_COMMWARP_RENAME", commanderID, fmt.Sprintf(
			"the store has no row in type%d slot %d any more, so there was nothing to "+
				"rename", telType, target.Slot), "", warpRenameGoneText), nil
	}

	after, lerr := h.teleports.Load(ctx, commanderID)
	if lerr != nil {
		h.Log.Printf("zone: CZ_COMMWARP_RENAME (type%d row %d) for commander %d: slot %d "+
			"is now named %q, but reading the list back failed: %v - the window was NOT "+
			"refreshed", telType, index, commanderID, target.Slot,
			summariseMemo(got.Name), lerr)
		return nil, nil
	}
	out := CommWarpPackets(after)
	h.Log.Printf("zone: CZ_COMMWARP_RENAME (type%d row %d) for commander %d: slot %d map "+
		"%d renamed %q -> %q; 3634 re-sent %d time(s) - %s", telType, index, commanderID,
		got.Slot, got.MapID, summariseMemo(target.Name), summariseMemo(got.Name),
		len(out), CommWarpSummary(after))
	return out, nil
}

// commWarpSwap answers CZ_COMMWARP_SWAP (3639): the window's Up and Down buttons.
//
// It is a MOVE, not an exchange of two rows: the row at `from` is taken out of the list
// and spliced back in at `to`, so the rows in between shift one place and keep their
// relative order. Up on the last row of five is 4 -> 3 and swaps two neighbours; a DOWN
// is 1 -> 2. Any two valid indices are accepted, in either direction - the client
// decides which button means which, and it sends row numbers, not "one up".
//
// The order it produces is PERSISTED (PGStore.MoveSavedTeleport rewrites
// `display_order`), because the very next 3634 is what the window draws: an order that
// lived only in memory would be undone by the next refresh.
//
// A refusal writes NOTHING and re-sends no list - one log line plus ONE ZC_MESSAGE line
// to the player, the same rule the save, remove and rename follow: the list the client
// holds is still true, and a silent refusal read as a dead button. The move happens
// inside the ONE list the packet names (`teltype` at +18), and both indices are positions
// in THAT list: all four panels can be reordered, because all four are served (R76 slice
// 6 - the old "types 1-3 are cash lists, refused" branch is gone).
func (h *Handler) commWarpSwap(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	commanderID, refuse := h.teleportCommander(sess, "CZ_COMMWARP_SWAP", len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil || h.teleports.Move == nil {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID,
			"no saved-point store or no move hook is wired in, so the new order could "+
				"not be kept", "", warpNoStoreText), nil
	}
	from, to, telType, ok := ParseCommWarpSwap(f.Data)
	if !ok {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID, fmt.Sprintf(
			"the packet is %d bytes and a swap is %d (a 10-byte security header, the row "+
				"that moves as a u32 at +10, the row it moves to as a u32 at +14, and the "+
				"teltype at +18)", len(f.Data), commWarpSwapSize), "", warpShortText), nil
	}
	if !validTelType(telType) {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID, warpBadListWhy(telType),
			"", warpBadListText), nil
	}

	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID,
			fmt.Sprintf("reading the saved points failed: %v", err), "", warpLoadText), nil
	}
	// Both numbers are positions inside the ONE list the client named: the four panels
	// each number their own rows from 0, so row 1 of "Warp Extend(2)" is not row 1 of
	// anything else.
	rows = rowsOfType(rows, telType)
	pick, hasFrom := listAt(rows, from)
	if _, hasTo := listAt(rows, to); !hasFrom || !hasTo {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID, fmt.Sprintf(
			"the client named rows %d -> %d of type%d (%s) and that list has %d point(s) "+
				"[%s]", from, to, telType, warpListName(telType), len(rows),
			describePoints(rows)), "", warpNoSuchRowText), nil
	}

	moved, err := h.teleports.Move(ctx, commanderID, telType, from, to)
	if err != nil {
		return h.commWarpRefuse("CZ_COMMWARP_SWAP", commanderID, fmt.Sprintf(
			"moving type%d row %d to row %d failed: %v", telType, from, to, err), "",
			warpMoveFailedText), nil
	}
	out := CommWarpPackets(moved)
	h.Log.Printf("zone: CZ_COMMWARP_SWAP (type%d rows %d -> %d) for commander %d: slot %d "+
		"map %d (%s) moved, and the order was stored; 3634 re-sent %d time(s) - %s",
		telType, from, to, commanderID, pick.Slot, pick.MapID,
		fromOr(pick.Name, h.MapName), len(out), CommWarpSummary(moved))
	return out, nil
}

// commWarpSelect answers CZ_COMMWARP_SELECT (3637): the Warp List's GO button. It moves
// the player to the row's saved point and takes ONE Warp Scroll for it.
//
// THE ORDER OF THE CHECKS IS THE FEATURE. Everything that can refuse is decided BEFORE
// the scroll is spent, so a refusal charges nothing and moves nobody - a player who is
// told "no" and then finds one scroll fewer has been robbed by a log line. Then the
// charge and the move itself: the item op is ONE Commit (one database transaction, the
// row named by its item id), and the placement it pays for cannot fail -
//
//   - SAME MAP (`target.MapID == h.MapID`): the in-zone teleport the town grid already
//     runs - see placeTeamAt in town.go, which this calls rather than copying. ONE
//     ZC_STOP_PC (3549, 22 B) per character, the LEADER LAST, all on ONE TICK, and the
//     team SPREAD IN THIS MAP'S OWN ARRIVAL FORMATION around the row's point - a saved
//     point carries no formation of its own (see the same-map branch below). The
//     archived trap "a warp is three stops, leader last" is that rule.
//   - OTHER MAP: the map change the town grid's travel already starts - see
//     Handler.startMoveFor in gate.go, the SAME sender waypointSelect and teleportTravel
//     use: one 3593, the client's 3686, our 3594 and the handoff. The destination zone
//     must already be running (checked here, before the charge, not only inside
//     startMoveFor after it).
//
// If the move still fails to start - the only way is a zone that stopped between those
// two lines - the scroll is GIVEN BACK in a second transaction and the jump is refused,
// so the race costs the player nothing but a log line.
//
// NOTHING ELSE IS SENT ON SUCCESS: no 3634 refresh. The list did not change, and a list
// that is redrawn on a jump is a packet per jump nobody asked for. The ONE item line
// (3608) is not optional: the scroll really did leave the bag, and a client still
// drawing it would offer a jump it cannot pay for.
func (h *Handler) commWarpSelect(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	const name = "CZ_COMMWARP_SELECT"

	commanderID, refuse := h.teleportCommander(sess, name, len(f.Data), f.Data)
	if refuse != nil {
		return refuse, nil
	}
	if h.teleports.Load == nil {
		return h.warpJumpRefuse(commanderID,
			"no saved-point store is wired in, so the row the packet names cannot be "+
				"resolved", warpNoStoreText), nil
	}
	index, telType, isEvent, ok := ParseCommWarpSelect(f.Data)
	if !ok {
		return h.warpJumpRefuse(commanderID, fmt.Sprintf(
			"the packet is %d bytes and a select is %d (a 10-byte security header, the "+
				"row at +10, the list at +11 and the event flag at +12), so the row is "+
				"not even there", len(f.Data), commWarpSelectSize), warpShortText), nil
	}
	if !validTelType(telType) {
		return h.warpJumpRefuse(commanderID, warpBadListWhy(telType),
			warpBadListText), nil
	}
	if h.Bag == nil || h.Commit == nil {
		return h.warpJumpRefuse(commanderID,
			"no bag reader or no item write path is wired in, so a scroll could not be "+
				"charged and the jump would have been free", warpNoItemsText), nil
	}

	// THE ROW: a position inside the list the packet names, exactly as remove, rename
	// and swap resolve theirs. Each of the four panels numbers its own rows from 0.
	rows, err := h.teleports.Load(ctx, commanderID)
	if err != nil {
		return h.warpJumpRefuse(commanderID,
			fmt.Sprintf("reading the saved points failed: %v", err), warpLoadText), nil
	}
	rows = rowsOfType(rows, telType)
	target, found := listAt(rows, index)
	if !found {
		return h.warpJumpRefuse(commanderID, fmt.Sprintf(
			"the client named row %d of type%d (%s) and that list has %d point(s) [%s]",
			index, telType, warpListName(telType), len(rows), describePoints(rows)),
			warpNoSuchRowText), nil
	}

	// THE SCROLL: `isEvent` picks which one, and it is the client's own choice - it
	// sets the flag only when the EVENT scroll is the one it holds (game.scp:188-191).
	class := warpScrollClass
	if isEvent {
		class = warpScrollEventClass
	}
	stacks, err := h.Bag(ctx, commanderID)
	if err != nil {
		return h.warpJumpRefuse(commanderID,
			fmt.Sprintf("reading commander %d's bag failed: %v", commanderID, err),
			warpScrollChargeText), nil
	}
	scroll, held := firstStackOfClass(stacks, class)
	if !held {
		// LOG ONLY, no ZC_MESSAGE: the CLIENT's own box is the answer to this, and it
		// already has one (`MsgBox("<$>79767</>")` in game.scp:174-196). Our line used
		// to replace it, and the owner saw ours instead of the reference server's - see
		// warpNoScrollRefuse for the whole rule.
		return h.warpNoScrollRefuse(commanderID, class), nil
	}

	// THE DESTINATION, decided before the charge. sameMap is the branch that matters:
	// a point on the map the session already stands on is a TELEPORT, anything else is
	// a MAP CHANGE, and the two use different existing paths (see the function note).
	sameMap := target.MapID == h.MapID
	var gate MapGate
	var camFrom string
	if sameMap {
		if !PlausibleCell(target.Pos[2]) {
			return h.warpJumpRefuse(commanderID, fmt.Sprintf(
				"warp point %d of type%d is on this map at (%d, %d, cell %d) and %v, so "+
					"there is no ground to land on", target.Slot, telType, target.Pos[0],
				target.Pos[1], target.Pos[2], badCellError(target.Pos[2])),
				warpTravelRefusedText), nil
		}
	} else {
		// One map change at a time, the same rule gateTick, the town grid and the
		// teleportation window keep.
		if sess.Gate != nil {
			return h.warpJumpRefuse(commanderID, fmt.Sprintf(
				"a map change to map %d (%s) is already running", sess.Gate.gate.ToID,
				sess.Gate.gate.To), warpBusyText), nil
		}
		var why string
		gate, camFrom, why = h.travelGate(
			fmt.Sprintf("warp point %d (map %d)", target.Slot, target.MapID),
			target.MapID, target.Pos, "")
		if why != "" {
			return h.warpJumpRefuse(commanderID, why, warpTravelRefusedText), nil
		}
		// The destination zone must be RUNNING, and it is checked HERE and not only
		// inside startMoveFor: that call comes after the charge, so a missing zone found
		// there would have taken a scroll for a jump nobody can make. The address is
		// looked up twice on purpose - the second lookup is startMoveFor's own.
		dest, live := h.ensureZone(target.MapID)
		if !live {
			return h.warpJumpRefuse(commanderID, fmt.Sprintf(
				"no live zone serves map %d (%s) - looked in %s; start the zone for that "+
					"map first", target.MapID, gate.To, registryDir(h.Zones)),
				warpTravelRefusedText), nil
		}
		if ip := net.ParseIP(dest.IP).To4(); ip == nil {
			return h.warpJumpRefuse(commanderID, fmt.Sprintf(
				"the zone serving map %d announced ip %q, which is not an IPv4 address",
				target.MapID, dest.IP), warpTravelRefusedText), nil
		}
		// THE REGISTRY FILE IS A HINT, NOT PROOF. A zone that is KILLED leaves its
		// presence file behind, so map 10 can still look alive at 127.0.0.1:7002 long
		// after nothing answers there. On 2026-10-30 a jump to that dead town took the
		// scroll, sent 3593, wrote a handoff and told the client to reconnect to a
		// closed port - the owner's "eats my warp scroll lol". So the destination is
		// DIALED here, before the charge: a few hundred ms, and nothing has been spent
		// yet, so a failure costs the player nothing.
		if derr := world.Probe(dest, world.ProbeTimeout); derr != nil {
			return h.warpJumpRefuse(commanderID, fmt.Sprintf(
				"map %d (%s) is registered at %s:%d (looked in %s) but NOTHING ANSWERS "+
					"there (%v) - the presence file outlives the process that wrote it, "+
					"so it is a hint and not proof; start the zone for that map",
				target.MapID, gate.To, dest.IP, dest.Port, registryDir(h.Zones), derr),
				warpTravelRefusedText), nil
		}
	}

	// THE CHARGE, in ONE transaction, and it names the exact bag ROW (ItemID) - two
	// stacks of one class would otherwise put the change on the wrong row and the client
	// would show a count the database does not hold.
	ops := []item.Op{{
		Give: false, ClassID: class, Count: warpScrollPerJump, ItemID: scroll.ItemID,
		Name: scrollClassName(class),
	}}
	changes, err := h.Commit(ctx, commanderID, nil, ops)
	if err != nil {
		return h.warpJumpRefuse(commanderID, fmt.Sprintf(
			"taking %d %s (class %d) from item %d failed: %v", warpScrollPerJump,
			scrollClassName(class), class, scroll.ItemID, err),
			warpScrollChargeText), nil
	}
	left := scroll.Count - warpScrollPerJump
	if len(changes) == 1 {
		left = changes[0].Total
	}

	var out [][]byte
	// THE CLIENT IS TOLD THAT THE BAG CHANGED - in the SAME packet the shops and the
	// equip/unequip path use: `sess.Inv.PacketFor` -> `BuildRemoveItem` (ZC_REMOVE_ITEM
	// 3608, 15 bytes: cmd, `invIndex` u32 @+2, the NEW TOTAL u32 @+6, `removeType`
	// @+10), and removeType 8 (`RemoveItemUse`) because that is the one whose handler
	// prints the client's own "<name> x N used." line (GENormalItem.cpp:541-568).
	// ONE row changes here, so this is ONE packet: N -> N-1 is that same position with
	// the lower total, and N -> 0 drops the row, because `PacketFor` clears the mirror
	// entry as it builds it when `Total` is 0 (inventory.go).
	//
	// WHY IT MUST GO OUT AT ALL (the bug this closes): the scroll leaves PostgreSQL, so
	// without this line the client keeps drawing a scroll it no longer has, its own
	// `IsItemInInventory('WarpScroll', 1)` guard keeps passing, and it asks for a jump
	// this server has to refuse - `logs\zone15_live.log`, 2026-10-30 19:03:07.
	told := ""
	for _, c := range changes {
		pos, _ := sess.Inv.Index(c.ItemID)
		if p := sess.Inv.PacketFor(c, AddItemPlain, RemoveItemUse); p != nil {
			out = append(out, p)
			if c.Total == 0 {
				told = fmt.Sprintf("the client was told with one cmd %d item line "+
					"(position %d is GONE, 0 left, removeType %d)", CmdZCRemoveItem, pos,
					RemoveItemUse)
			} else {
				told = fmt.Sprintf("the client was told with one cmd %d item line "+
					"(position %d now shows %d, removeType %d)", CmdZCRemoveItem, pos,
					c.Total, RemoveItemUse)
			}
			continue
		}
		// The mirror has no position for this row, so an incremental packet cannot
		// address it - naming a guessed position would decrement a DIFFERENT item.
		// Say so LOUDLY: a silent skip here is exactly how a stale bag goes unnoticed.
		told = fmt.Sprintf("the client was NOT told: the bag mirror has no position for "+
			"item %d, so no cmd %d could be built - that row reached the bag without "+
			"this session being told, and the two sides now disagree",
			c.ItemID, CmdZCRemoveItem)
	}

	var moved string
	if sameMap {
		// THE EXISTING IN-ZONE TELEPORT (town.go's placeTeamAt), not a second copy: one
		// 3549 per character, leader last, one tick.
		//
		// A SAVED POINT HAS NO FORMATION - the row is (x, y, cell) and nothing else - and
		// "everybody on the spot" is NOT a formation. That was this branch's rule, and the
		// owner saw it in game: "when u are in the same zone, warping will bundled your
		// character together" (2026-10-30). So the team is spread with the SAME shape every
		// other arrival uses: this map's own arrival formation, laid around the row's point
		// by formationAround - the function the gate's exit, the saved start point and the
		// Last Point already go through. Each follower's CELL is its own, resolved on this
		// map's mesh (formationCellAt); the offsets are the reference's (-75,+43 / -75,-44).
		cell := target.Pos[2]
		spot := TownPoint{X: target.Pos[0], Y: target.Pos[1], Cell: &cell}
		// Slot 0 IS the point: an empty formation answers it unchanged, so the leader
		// stands exactly where the row says.
		leader := spot.Slot(0)
		slots := formationAround(h.SpawnFormation, h.Spawn, leader, h.formationCellAt(leader))
		// ONE rule for every member, including slot 0 and any slot past the shape: a member
		// with no cell of its own stands on the leader's spot.
		place := func(index int) [3]int32 {
			return formationSlot(slots, leader, index)
		}
		stops, tick, placed := h.placeTeamAt(sess, leader, place)
		if len(stops) == 0 {
			return h.refuseAfterCharge(ctx, sess, name, commanderID, class, target,
				"the session has no team, so there was nobody to place"), nil
		}
		out = append(out, stops...)
		refused := 0
		for i := 1; i < len(slots); i++ {
			if slots[i].Cell == nil {
				refused++
			}
		}
		moved = fmt.Sprintf("moved on map %d (%s) to (%d, %d, cell %d) - same map, cmd "+
			"%d x%d stop only, reverse order, one tick %d, no 3634 refresh, spread by "+
			"this map's own arrival formation (%d slot(s), %d the mesh refused and left "+
			"with the leader) [%s]",
			h.MapID, h.MapName, target.Pos[0], target.Pos[1], target.Pos[2],
			CmdZCStopPC, len(stops), tick, len(slots), refused, strings.Join(placed, ", "))
	} else {
		// THE EXISTING MAP CHANGE (gate.go's startMoveFor): one 3593, the client's 3686,
		// our 3594 and the handoff.
		zoneOut := h.startMoveFor(sess, "WARP",
			"the player picked a saved point in the Warp List", gate)
		if len(zoneOut) == 0 {
			return h.refuseAfterCharge(ctx, sess, name, commanderID, class, target,
				"the map change to map "+fmt.Sprint(target.MapID)+" was refused by "+
					"startMoveFor after the charge"), nil
		}
		// This map change was PAID FOR: mark it so gateReady can give the fare back if
		// the destination stops answering before the client's 3686 (see gateMove.
		// warpClass and Handler.gateReady).
		if sess.Gate != nil {
			sess.Gate.warpClass = class
		}
		out = append(out, zoneOut...)
		moved = fmt.Sprintf("started a map change to map %d (%s) at (%d, %d, cell %d), "+
			"camera from %s - cmd %d sent (%d bytes), waiting for cmd %d, no 3634 refresh",
			target.MapID, gate.To, target.Pos[0], target.Pos[1], target.Pos[2],
			fromOr(camFrom, "nowhere (zeros)"), CmdZCMoveZone, MoveZoneSize,
			CmdCZMoveZoneOK)
	}

	// ONE line that PROVES the charge and names the row, so "I lost a scroll and went
	// nowhere" is answerable from the log alone - and it says what the CLIENT was told,
	// because a charge the client never hears about is the bug this path exists to
	// avoid.
	h.Log.Printf("zone: %s (type%d row %d) for commander %d: CHARGED %d %s (class %d, "+
		"item %d, %d left); %s; %s", name, telType, index, commanderID,
		warpScrollPerJump, scrollClassName(class), class, scroll.ItemID, left, told, moved)
	return out, nil
}

// warpNoScrollRefuse is the NO-SCROLL refusal of CZ_COMMWARP_SELECT: ONE log line, and
// NOTHING on the wire - no item packet, no ZC_MESSAGE.
//
// WHY NO MESSAGE. The player already has the reference server's own answer to this:
// `func UI_COMMWARP_EXECUTE` checks `IsItemInInventory('WarpScroll', 1)` BEFORE it sends
// 3637 and, with none, shows the client's own box `MsgBox("<$>79767</>")` - "You need
// Warp Scroll or Warp Scroll for event" (ge\ui\uiscp\game.scp:174-196). Our 3596 line
// said almost the same thing in our own words, and the owner's report is exactly that:
// "You need a Warp Scroll in your bag to travel. Nothing was charged." i dont need this
// line. it was supposed to show the message box like the live server reference"
// (2026-10-30).
//
// WHY THE LINE STAYS. A request for a jump with an empty bag is a bag that is out of
// step (or a client that is not the shipped one), and the log is the only record of it.
// It says the bag held none and that nothing was charged, so "did I lose a scroll?" is
// answerable from the log alone.
func (h *Handler) warpNoScrollRefuse(commanderID int32, class int64) [][]byte {
	h.Log.Printf("zone: CZ_COMMWARP_SELECT for commander %d REFUSED - the bag holds no "+
		"%s (class %d) and a jump costs %d of them; nothing was charged and nobody "+
		"moved. LOG ONLY: no cmd %d is sent for this case, because the client's own "+
		"box (MsgBox 79767, game.scp:174-196) is what the player sees on the reference "+
		"server, and a client that reaches here has a bag out of step - see the charge "+
		"line of the previous jump", commanderID, scrollClassName(class), class,
		warpScrollPerJump, CmdZCMessage)
	return nil
}

// warpJumpRefuse is the refusal path for CZ_COMMWARP_SELECT when the PLAYER must be told
// why: ONE log line naming what was refused and why, plus ONE ZC_MESSAGE saying the same
// thing in plain words.
//
// It is NOT the no-scroll case - that one is log-only, because the client has its own box
// for it (see warpNoScrollRefuse). Every other refusal here names something the player
// cannot see for themselves: a row that is not in the list, a destination no zone serves,
// a map change already running.
//
// It is commWarpRefuse's wording for the GO button, and the difference is the sentence
// that matters to a player here: a refused jump cost NOTHING. The four writes say
// "nothing was written" because a save, remove, rename or swap is a write; a jump is a
// charge and a movement, so this one says "nothing was charged, nobody moved". Same rules
// as the texts above: ASCII only, short, no locale token, no invented number.
func (h *Handler) warpJumpRefuse(commanderID int32, why, text string) [][]byte {
	h.Log.Printf("zone: CZ_COMMWARP_SELECT for commander %d REFUSED - %s; nothing was "+
		"charged, nobody moved, and the player was told with one cmd %d line",
		commanderID, why, CmdZCMessage)
	return [][]byte{BuildMessage(text, MessageTypeGeneral, MessageLevelGeneral)}
}

// refuseAfterCharge is the ONE path for "the scroll is spent and the move cannot start".
//
// It should be unreachable: every reason a move can fail is decided BEFORE the charge
// (see commWarpSelect). It exists because the two are not one instruction, and the
// difference between them is a zone that stopped in between - so the honest answer is to
// give the scroll back, tell the CLIENT that it is back (refundWarpScroll), say so in one
// line, and send the player ONE message. Taking a scroll without moving anybody is exactly
// what the owner's decision ("one scroll per jump") does not allow.
func (h *Handler) refuseAfterCharge(ctx context.Context, sess *Session, name string, commanderID int32, class int64, target SavedPoint, why string) [][]byte {
	given, note := h.refundWarpScroll(ctx, sess, class)
	h.Log.Printf("zone: %s (row %d, map %d) for commander %d: the %s (class %d) was "+
		"CHARGED and then the move could not start - %s; %s, and the player was told "+
		"with one cmd %d line", name, target.Slot, target.MapID, commanderID,
		scrollClassName(class), class, why, note, CmdZCMessage)
	out := [][]byte{}
	if given != nil {
		out = append(out, given)
	}
	return append(out, BuildMessage(warpTravelRefusedText, MessageTypeGeneral, MessageLevelGeneral))
}

// refundWarpScroll gives back the ONE scroll a jump took, in its own transaction, and
// answers the packet that tells the client plus a sentence for the log.
//
// THE CLIENT IS TOLD HERE TOO, for the same reason it is told about the charge: the
// database now holds one scroll more, and a client left on the lower count would draw a
// bag the server does not have. The packet is the item layer's own -
// `sess.Inv.PacketFor` -> `BuildAddItem` (ZC_ADD_ITEM 3607, whose `count` is the NEW
// TOTAL of that row) - and `AddItemPlain` is the right addType because the refund is not
// a purchase: the client prints its own "you obtained N <name>" line for it. A give always
// produces a position (an existing row keeps its slot, a new one goes at the end), so this
// packet exists whenever the refund itself succeeded.
func (h *Handler) refundWarpScroll(ctx context.Context, sess *Session, class int64) ([]byte, string) {
	changes, err := h.Commit(ctx, sess.CommanderID, nil, []item.Op{{
		Give: true, ClassID: class, Count: warpScrollPerJump, Merge: true,
		Name: scrollClassName(class),
	}})
	if err != nil {
		return nil, fmt.Sprintf("the refund FAILED (%v) - the player is one scroll short "+
			"and it must be given back by hand", err)
	}
	var pkt []byte
	note := "the scroll was given back, so nothing was charged"
	if len(changes) > 0 {
		pos, _ := sess.Inv.Index(changes[0].ItemID)
		pkt = sess.Inv.PacketFor(changes[0], AddItemPlain, RemoveItemUse)
		if pkt == nil {
			note = fmt.Sprintf("the scroll was given back in the database, but the client "+
				"was NOT told (no cmd %d could be built for item %d %d)", CmdZCAddItem,
				changes[0].ItemID, pos)
		} else {
			note = fmt.Sprintf("the scroll was given back and the client was told with "+
				"one cmd %d item line (position %d now %d), so nothing was charged",
				CmdZCAddItem, pos, changes[0].Total)
		}
	}
	return pkt, note
}

// summariseMemo shortens a memo for a log line: 24 characters, then "...". A memo is
// 30 characters at most, so this only ever cuts the longest ones.
func summariseMemo(memo string) string {
	r := []rune(memo)
	if len(r) <= 24 {
		return memo
	}
	return string(r[:24]) + "..."
}

// TownGrid is the rows for the TOWN tab: every waypoint row the client's table
// marks as a town (Tab 0) that also resolves to a map id this file knows.
//
// Rows left out are not silently dropped: the caller logs how many the table had
// and how many survived, because "the window shows 12 towns" must be traceable to
// a row that failed to resolve.
func (z *ZonePoints) TownGrid() (towns []WaypointTown, skipped []WaypointTown) {
	if z == nil {
		return nil, nil
	}
	for _, w := range z.Waypoints {
		if w.Tab != 0 || w.MapID <= 0 {
			skipped = append(skipped, w)
			continue
		}
		towns = append(towns, w)
	}
	return towns, skipped
}
