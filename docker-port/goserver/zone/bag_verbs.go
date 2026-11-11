package zone

// THE TWO BAG VERBS THE CLIENT OFFERS AND THIS ZONE NEVER ANSWERED: drop (3612) and
// use-with-target (3651).
//
// WHY THIS FILE EXISTS. The inventory window's own buttons send these two frames, and
// both used to fall into `Reply`'s default branch - `cmd 3612 not handled yet (first
// seen)` - so the player pressed Drop or double-clicked a potion and the game did
// nothing at all. Neither frame is optional: the client's own table registers them
// (`data\cmd_table.txt`: 3612 19 B `PKS_CZ_DROP`, 3651 20 B `PKS_CZ_USEITEM_TARGET`,
// 3613 6 B `PKS_ZC_USEITEM`), so the bytes arrive and the answer is this server's job.
//
// THE LAYOUTS, from the client's own structs (geZCPacket.h:1202-1207, :1412-1418), all
// after the 10-byte PKS_SECURITY_DEFAULT:
//
//	CZ_DROP              +10 charIndex(1)  +11 invIndex(4)  +15 count(4)
//	CZ_USEITEM_TARGET    +10 charIndex(1)  +11 invIndex(4)  +15 targetID(4)  +19 overwriteBuff(1)
//
// `count` on the CZ_DROP server side is a WORD (`CPlayerChar::RequestDropItem(DWORD,
// WORD)`, PlayerChar_Request.cpp:169) and the client's own sender packs a WORD
// (`packet::CZ_Drop(int, DWORD, WORD)`, GENormalSendPacket.cpp:181-190), so the DWORD
// at +15 is a zero-extended u16 - the high half is always 0 in a real frame.
//
// WHAT THE BAG BECOMES, AND THE ONE THING THAT MUST NOT BE INVENTED HERE. There is NO
// bag move, swap, split, discard or repair message in the client's registry (1,588
// names, `data\cmd_table.txt`): the ONLY way a player moves a stack is this drop, and a
// drop of less than the stack IS the client's split. So a drop of N takes N out of the
// row and puts ONE ground item of N on the floor - it is not a second row in the bag.
//
// THE BAG HALF GOES THROUGH `h.Commit`, the same one-transaction write path a PURCHASE
// and a PICKUP use (zone\items.go -> item.writeItem), and it names the ROW (`op.ItemID` ->
// takeRow), not the class: a player can hold the same class in two rows, and the row the
// client pointed at is the row that must shrink. A refusal returns before the first
// write, so the bag is untouched and nothing at all is sent.
//
// THE REFUSALS ARE LOG-ONLY. The client already has its own message for every case below
// (it refuses most of them before sending at all - GENormalItem.cpp:61-95), and a chat line
// in this server's words would be a second, wrong voice. One line, nothing sent, is the
// answer; the same rule the pickup and the shop paths follow.
//
// THE CURRENCY RULE IS THE REFERENCE'S OWN LINE, not a guess: `CTxDropItem::Do` refuses
// `item->GetType() == ITEM_ID_VIS || item->GetType() == ITEM_ID_FESO` (TxDropItem.cpp:115)
// and those two constants are geShared.h:369-370 (30000 / 30001). The item table agrees:
// class 30000 is ClassName "Vis", GroupName "Money" (`ge\xml\datatable_item.xml`).
//
// WHAT IS DELIBERATELY NOT HERE, each because it would be an invented rule:
//   - the reference's `item->IsTradable()` refusal (TxDropItem.cpp:118) needs a Tradable
//     column this zone does not read; a drop of an untradable item is not refused today
//     and that is a named GAP, not a silent rule.
//   - the reference SCATTERS a drop within 200 units of the character
//     (`GetRandomPosLocaly(shape, dropPos, pos, 200)`, TxDropItem.cpp:210-216). This
//     places it at the character's own drawn point, which is 0 units from the character
//     and therefore inside that same radius - a legal position of the reference's own
//     rule, chosen so the drop is where the player is looking.
//   - the 60 s ground-item lifetime and the 3537 a later arrival needs (see pickup.go).
//   - `ZoneItem` items (their use does not consume, TxUseItem.cpp:145-147) and every
//     use SCRIPT except three: the 3613 announces the class and the client draws its
//     message, `SCP_USE_ITEM_HPUP` is carried by the HP half below, and the two buff
//     scripts (`SCP_USE_ITEM_AddBuff`, `SCP_USE_ITEM_TeamAddBuff`) now run the row's
//     own script through the buff slice (zone\buff.go). An exp, a resurrection or a
//     scroll item is still only announced and consumed, because this server has no
//     exp-on-use path, no resurrection
//     flow and no stance unlock - and eating the item for no effect is worse than
//     saying so in one line (the non-heal line in useItemTarget).
//
// THE HP HALF (the potion bug, owner 2026-11-08 17:12 "hp does not update realtime?").
// A use that the item's OWN TABLE says heals runs the same three steps the reference's
// `CFieldChar::AddHP` does (FieldChar.cpp:132-165), in this order:
//
//	1. clamp:  after = hp + arg1, then `if (mhp <= expectHP) expectHP = mhp`;
//	2. write the character's ledger (Character.HP, the float64 zone\zone.go:98 that
//	   the monster's 3564 damage also writes - this zone persists no HP to the
//	   database, see the note on the store below);
//	3. send ONE 23-byte ZC_ADD_HP 3557 built by the EXISTING builder
//	   (zone\skill_area.go:275), +6 = the new HP and +14 = the heal delta.
//
// NO CHANGE, NO PACKET: `if (0 == add || hp == expectHP) return 0;` (FieldChar.cpp:156-157)
// - a potion swallowed at full HP is consumed (the item transaction is already done by
// then in the reference too, TxUseItem.cpp:238) and no 3557 goes out. The oracle does
// the same (`if HEAL_ENABLED and delta:`, src\zone_server.py:747).
//
// THE +14 FIELD IS THE DELTA, not 0: the client's own AddHP replaces the delta with
// `nowHP - prevHP` for YOUR character (GENormalObject.cpp:2376-2377) but does
// `m_HP += addHP` for every OTHER viewer's copy of it (:2388-2389), and `m_HP` is what
// their bar draws (GeBaseObject.h:527). A 0 there would move the actor's bar and leave
// every onlooker's bar stale. The reference sends `expectHP - hp` (FieldChar.cpp:163).
// +22 is then 0 by the builder's own rule (a positive delta), which is what the 81
// measured live 3557s carry.

import (
	"context"
	"encoding/binary"
	"strings"
	"time"

	"geserver/item"
	"geserver/protocol"
)

const (
	// CmdCZDrop is PKS_CZ_DROP, client -> server, FIXED 19 bytes
	// (`data\cmd_table.txt`:1085, geZCPacket.h:1202-1207).
	CmdCZDrop  = 3612
	czDropSize = 19

	// The three fields after the security header, at the struct's own offsets.
	czdCharIndex = 10 // BYTE   - the dropping character's slot in THIS session's team
	czdSlot      = 11 // DWORD  - the bag position the client is showing (1-based)
	czdCount     = 15 // DWORD  - how many to drop; a WORD in the client's sender, zero-extended

	// CmdCZUseItemTarget is PKS_CZ_USEITEM_TARGET, client -> server, FIXED 20 bytes
	// (`data\cmd_table.txt`:1127, geZCPacket.h:1412-1418). THERE IS NO PLAIN SELF-USE
	// REQUEST: the bag window's use IS this frame.
	CmdCZUseItemTarget  = 3651
	czUseItemTargetSize = 20

	czuCharIndex = 10 // BYTE   - the using character's slot in this session's team
	czuSlot      = 11 // DWORD  - the bag position (same key space CZ_EQUIP names)
	czuTarget    = 15 // OBJHANDLE - the object the item is used ON
	czuLast      = 19 // BYTE   - `overWriteBuff`: the buff overwrite ask, carried and unused

	// CmdZCUseItem is PKS_ZC_USEITEM: cmd(2) + type(4) = 6 bytes
	// (`data\cmd_table.txt`:1086, geZCPacket.h:2119-2121). It ANNOUNCES the effect: the
	// class, nothing else. A use is this packet PLUS the ordinary 3608 that changes the
	// stack - the client's handler prints its own line from its own string table
	// (GENormalNet_Session.cpp:81-87).
	CmdZCUseItem  = 3613
	ZCUseItemSize = 6

	// removeItemDrop is REMOVE_ITEM_DROP, the removeType a drop carries: the enum starts
	// with an unnamed 0, so SELL=1, BUY=2, DROP=3 (geZCTypes.h:455-459; the same enum and
	// the same counting zone\inventory.go already documents for USE=8).
	removeItemDrop uint8 = 3

	// VisClassID and FesoClassID are ITEM_ID_VIS / ITEM_ID_FESO (geShared.h:369-370).
	// CTxDropItem::Do refuses both by name (TxDropItem.cpp:115): money is not a field item.
	VisClassID  = 30000
	FesoClassID = 30001

	// usableGroup is the ONE item-table GroupName whose use this server carries end to
	// end: `CTxUseItem` is the reference's Consume branch (PlayerChar_Request.cpp:653-690).
	// The client's own gate allows three groups - "Consume", "Scroll", "Period"
	// (GENormalItem.cpp:63-65) - and the other two are REFUSED rather than eaten: a Scroll
	// unlocks a stance (CTxUseScrollItem) and a Period item is a timed cash item, and this
	// server has neither path, so consuming the item would destroy it for no effect.
	usableGroup = "Consume"

	// useItemHealScript is the ONE use effect this zone carries: the item's own
	// `ScpType` names `SCP_USE_ITEM_HPUP` (`ge\script\item\item.scp:1529`), and that
	// script is `AddHP(self, arg1)` with `arg1` pushed as the script's first argument
	// (`PushFloat(itemProperty->arg1)`, PlayerChar.cpp:2247). The reference runs it
	// from the use TRANSACTION, after the item change and before the 3613:
	// `m_pc->UseConsumeItem(property->type)` (TxUseItem.cpp:218-221).
	//
	// IT IS MATCHED BY NAME, never by a class range: the item table says which script
	// a class runs, so there is no list of "potion classes" here to rot.
	useItemHealScript = "SCP_USE_ITEM_HPUP"

	// The two use scripts that GRANT a buff, and the only ones this build runs.
	//
	//	SCP_USE_ITEM_AddBuff      ge\script\item\item.scp:1567  -> AddBuffImm(self, BuffName, arg1, arg2)
	//	SCP_USE_ITEM_TeamAddBuff  ge\script\item\item.scp:1762  -> AddBuffToTeam(self, BuffName, arg1, arg2)
	//
	// 360 of the 3,129 Consume rows name one of them. They are matched BY NAME for the
	// same reason the heal script is: the item table says which script a class runs, so
	// there is no list of \"buff potion classes\" here to rot. The 57 BuffName rows that
	// name a different script are NOT run - each needs builtins this server does not
	// bind yet, and that is the item lane's next card, named in the refusal line.
	useItemBuffScript     = "SCP_USE_ITEM_AddBuff"
	useItemTeamBuffScript = "SCP_USE_ITEM_TeamAddBuff"
)

// useItemGrantsBuff answers whether this row's OWN script is one this build runs for
// its buff half. The BuffName must be real too: the reference only resends the bar
// when `itemProperty->buffName != StrID::None` (PlayerChar.cpp:2258-2260), and a row
// with no name has nothing to grant.
func useItemGrantsBuff(row ItemRow) bool {
	if !buffNameIsReal(row.BuffName) {
		return false
	}
	switch row.ScpType {
	case useItemBuffScript, useItemTeamBuffScript:
		return true
	}
	return false
}

// buffNameIsReal answers whether a name column carries a buff. The client spells "no
// buff" as the word None, and an empty attribute is the same thing - never a name.
func buffNameIsReal(name string) bool {
	n := strings.TrimSpace(name)
	return n != "" && !strings.EqualFold(n, "None")
}

// DropRequest is one decoded CZ_DROP (3612).
type DropRequest struct {
	// CharIndex is the dropping character's slot in this session's team - the same field
	// CZ_ATTACK, CZ_PICKITEM and CZ_CHANGE_STANCE carry.
	CharIndex int
	// Slot is the client's 1-based bag position. It is resolved through Session.Inv, the
	// mirror of the client's own key space, never by counting database rows: a row that
	// emptied keeps its position as a HOLE (zone\inventory.go).
	Slot int32
	// Count is how many units leave the row. A count BELOW the stack is the client's only
	// split; the server decides the remainder and the new ground GUID.
	Count int64
}

// ParseDrop reads CZ_DROP. It insists on the EXACT 19 bytes the client's table registers,
// for the reason ParsePickItem gives: 3612 is a fixed-size packet, the splitter cuts it at
// the declared size, and a frame of another length means the table and the bytes disagree.
func ParseDrop(pt []byte) (DropRequest, bool) {
	var req DropRequest
	if len(pt) != czDropSize {
		return req, false
	}
	req.CharIndex = int(pt[czdCharIndex])
	req.Slot = int32(binary.LittleEndian.Uint32(pt[czdSlot:]))
	req.Count = int64(binary.LittleEndian.Uint32(pt[czdCount:]))
	return req, true
}

// UseItemRequest is one decoded CZ_USEITEM_TARGET (3651).
type UseItemRequest struct {
	// CharIndex is the using character's slot in this session's team.
	CharIndex int
	// Slot is the client's 1-based bag position.
	Slot int32
	// Target is the object the item is used on. THE CLIENT'S OWN SELF-USE IS NOT ZERO:
	// `geitem::ReqUseItem` sends `Session().GetMyPcHandle(myPcIndex)` - the character's
	// OWN HANDLE (GENormalItem.cpp:249, GENormalCommand.cpp:36) - and the reference's
	// `DispatchUseItem` requires exactly that (`this != target` -> return,
	// PlayerChar_Request.cpp:546-551). So BOTH 0 (the "no target" spelling) and the
	// acting character's own handle are self-use here, and anything else is refused.
	Target uint32
	// Last is `overWriteBuff`: 0 at the client's self-use sites and 1 from the shortcut
	// path (script.cpp:3790). It is the `overwrite existing buff` ask; the zone carries
	// a buff LIST now (buff slice 1) but does not honour this flag - a buff already up
	// is replaced by its own group/rank rule, not by this byte. Carried and unused.
	Last uint8
}

// ParseUseItemTarget reads CZ_USEITEM_TARGET, EXACT 20 bytes.
func ParseUseItemTarget(pt []byte) (UseItemRequest, bool) {
	var req UseItemRequest
	if len(pt) != czUseItemTargetSize {
		return req, false
	}
	req.CharIndex = int(pt[czuCharIndex])
	req.Slot = int32(binary.LittleEndian.Uint32(pt[czuSlot:]))
	req.Target = binary.LittleEndian.Uint32(pt[czuTarget:])
	req.Last = pt[czuLast]
	return req, true
}

// BuildUseItem packs PKS_ZC_USEITEM (3613), 6 bytes: cmd(2) + type(4).
//
// `classID` is the ITEM CLASS the use announced - the same id the bag row holds. The
// client looks the class up in its own item table and prints its own "used" line; it does
// not read the bag row from this packet, which is why the 3608 still has to follow.
func BuildUseItem(classID int64) []byte {
	out := make([]byte, ZCUseItemSize)
	binary.LittleEndian.PutUint16(out[0:], uint16(CmdZCUseItem))
	binary.LittleEndian.PutUint32(out[2:], uint32(classID))
	return out
}

// bagRowAt resolves a 1-based client position to the LIVE bag row behind it.
//
// IT IS THE ONE PLACE THE TWO VERBS AGREE, and the one place "the slot does not own a
// stack" is answered: no mirror (the session was never sent a bag list), a position the
// client never had, a hole the mirror remembers, no write path, an unresolved commander,
// or a row that left the bag since the mirror was built - each is ONE log line and
// nothing sent. `verb` is the frame's own name ("DROP" / "USEITEM"), so the line is
// greppable by the command that failed. A row with count <= 0 cannot come out of `Stacks`
// (the query is the same one the client's list is built from), so "empty" and "absent" are
// the same answer here.
func (h *Handler) bagRowAt(ctx context.Context, sess *Session, slot int32, verb string) (item.Stack, bool) {
	if sess.Inv == nil {
		h.Log.Printf("zone: %s refused: commander %d has no inventory mirror - this "+
			"session was never sent a bag list, so position %d names nothing - nothing sent",
			verb, sess.CommanderID, slot)
		return item.Stack{}, false
	}
	itemID, known := sess.Inv.IDAt(slot)
	if !known {
		h.Log.Printf("zone: %s refused: bag position %d is not a live row this session "+
			"was told about (%d position(s), holes included) for commander %d - nothing sent",
			verb, slot, sess.Inv.Len(), sess.CommanderID)
		return item.Stack{}, false
	}
	if h.Bag == nil || h.Commit == nil {
		why := "this zone has no bag write path wired"
		if h.Bag != nil {
			why = "this zone has no item transaction wired"
		}
		h.Log.Printf("zone: %s refused: %s for commander %d (position %d names item %d) "+
			"- nothing sent", verb, why, sess.CommanderID, slot, itemID)
		return item.Stack{}, false
	}
	if sess.CommanderID == 0 {
		h.Log.Printf("zone: %s refused: the session's commander is not resolved (0), so "+
			"position %d cannot name a bag row - nothing sent", verb, slot)
		return item.Stack{}, false
	}
	stacks, err := h.Bag(ctx, sess.CommanderID)
	if err != nil {
		h.Log.Printf("zone: %s refused: commander %d's bag could not be read (%v) - "+
			"nothing sent", verb, sess.CommanderID, err)
		return item.Stack{}, false
	}
	for _, st := range stacks {
		if st.ItemID == itemID {
			if st.Count <= 0 {
				break
			}
			return st, true
		}
	}
	h.Log.Printf("zone: %s refused: position %d named item %d and commander %d's bag no "+
		"longer holds it (or holds it empty) - nothing sent", verb, slot, itemID,
		sess.CommanderID)
	return item.Stack{}, false
}

// dropItem answers CZ_DROP (3612): take N units out of ONE named bag row and put them on
// the floor as ONE field item.
//
// THE ORDER OF THE THREE ANSWERS, and why it cannot be another one:
//
//	1. the bag write (h.Commit -> takeRow) - the only step that can fail, and it fails
//	   BEFORE anything is sent or created, so a refusal leaves the bag byte-identical;
//	2. the 3608 the dropper gets - the row's NEW TOTAL (0 removes it), removeType
//	   REMOVE_ITEM_DROP, which is the client's own "dropped" line;
//	3. the 3536 to every viewer the sight ring reaches (broadcastDropAt - both radii),
//	   and the registry entry BEFORE the frame leaves, so the handle a client draws is a
//	   handle a later pickup can resolve (the rule sendDropPackets already follows).
//
// The ground item is the SAME registry entry a monster drop makes: class and count only,
// parent 0 (no body it came from), killer 0 (nobody earned it).
func (h *Handler) dropItem(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParseDrop(f.Data)
	if !ok {
		h.Log.Printf("zone: DROP refused: %d-byte frame, CZ_DROP 3612 is a fixed %d bytes - "+
			"nothing sent", len(f.Data), czDropSize)
		return nil, nil
	}
	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: DROP refused: commander %d character index %d is not in this team "+
			"(%d member(s) spawned) - nothing sent", sess.CommanderID, req.CharIndex,
			len(sess.Team))
		return nil, nil
	}
	charHandle := sess.Team[req.CharIndex].Handle

	st, ok := h.bagRowAt(ctx, sess, req.Slot, "DROP")
	if !ok {
		return nil, nil
	}
	if st.ClassID == VisClassID || st.ClassID == FesoClassID {
		h.Log.Printf("zone: DROP refused: commander %d char %d (handle %d) named class %d "+
			"(%s) at slot %d - money cannot be dropped (ITEM_ID_VIS 30000 / ITEM_ID_FESO "+
			"30001, TxDropItem.cpp:115) - nothing sent",
			sess.CommanderID, req.CharIndex, charHandle, st.ClassID,
			moneyClassName(st.ClassID), req.Slot)
		return nil, nil
	}
	if req.Count <= 0 || req.Count > st.Count {
		h.Log.Printf("zone: DROP refused: commander %d char %d (handle %d) asked for %d of "+
			"class %d at slot %d, which holds %d - a drop must be 1..the stack - nothing sent",
			sess.CommanderID, req.CharIndex, charHandle, req.Count, st.ClassID, req.Slot,
			st.Count)
		return nil, nil
	}

	// THE BAG HALF: one transaction, the row named by item id (never by class), so the row
	// the client pointed at is the row that shrinks even when the class is held twice.
	changes, err := h.Commit(ctx, sess.CommanderID, nil, []item.Op{{
		Give: false, ClassID: st.ClassID, ItemID: st.ItemID, Count: req.Count,
	}})
	if err != nil {
		h.Log.Printf("zone: DROP refused: commander %d's bag refused to give up %d of item %d "+
			"(%v) - nothing was written, nothing sent", sess.CommanderID, req.Count, st.ItemID, err)
		return nil, nil
	}

	remaining := st.Count
	var out [][]byte
	for _, c := range changes {
		if c.Give {
			continue
		}
		remaining = c.Total
		if p := sess.Inv.PacketFor(c, AddItemPlain, removeItemDrop); p != nil {
			out = append(out, p)
		}
	}

	// THE WORLD HALF. The point is the character's own DRAWN point (sessionBodyAt, the
	// reader the reach check and the ring already use) - see the file header on why the
	// reference's 200-unit scatter is answered by 0.
	body, cellFrom := h.dropPosFor(h.sessionBodyAt(sess, req.CharIndex, time.Now()))
	handle := h.nextItemHandle()
	h.recordGroundItem(groundItem{
		Handle: handle,
		ItemID: uint32(st.ClassID),
		Count:  int(req.Count),
		Pos:    body,
	})
	pkt := BuildSpawnStandingItem(handle, body,
		ItemAppearance{Type: uint32(st.ClassID), Count: uint32(req.Count)}, "", 0)
	seen := h.broadcastDropAt(pkt, body[0], body[1])

	h.Log.Printf("zone: DROP commander %d char %d (handle %d) dropped class %d x%d from slot %d "+
		"(item %d), %d left; ground handle 0x%08x at (%d, %d, cell %d from %s); cmd %d (%d B) "+
		"to %d player(s), %d bag packet(s)",
		sess.CommanderID, req.CharIndex, charHandle, st.ClassID, req.Count, req.Slot,
		st.ItemID, remaining, handle, body[0], body[1], body[2], cellFrom, CmdZCSpawnStandingItem,
		SpawnStandingItemSize, seen, len(out))
	return out, nil
}

// dropPosFor is the point ONE dropped item is laid on: the character's own drawn point,
// with its CELL taken from the map's mesh at that point.
//
// THE X/Y ARE NOT TOUCHED AND THE CELL IS, and that is the whole of the owner's
// "dropping something sometimes put the item in wrong position" (2026-11-08). A WORLDPOS
// carries `x, y, cell`, the cell is a NAVMESH TRIANGLE INDEX and never a height
// (history\zone.md:42), and the client takes the item's HEIGHT from that triangle
// (history\zone-npc-click.md:324-326: "a bad cell drops the NPC through the map"). The
// point arrives from sessionBodyAt, which INTERPOLATES x/y along the walk but answers the
// CELL OF THE LEG'S SEGMENT END: walkedToAt returns `dest[2]` (move.go:510-514) and
// walkedRouteAt returns `route[i][2]`, the corner the body is walking toward
// (move.go:563-567). A click's leg can be thousands of units long, so a drop taken
// mid-walk kept a cell from the far end - and the DROP line shows it: 2026-11-08
// 23:18:05.708 dropped at (-1008, -4049) with `cell 181`, a triangle 2,500 units away at
// (529, -1615); `pathtool fld_auch.tok valid -1008 -4049 181` answers 0 (not that
// triangle) while the ground under the point is cell 81. A body that is STANDING keeps
// the cell its own CZ_MOVE_PC reported - that is the client's own ground and it is
// right - which is exactly why the owner saw it "sometimes".
//
// THE ANSWER IS THE MAP'S OWN. `Handler.NavNear` is the game's
// `Get3DPointToPosLocaly(point, out, 1000, 300)` - the call the reference makes to land a
// point on its mesh and the one this side already uses to place an NPC or a follower
// (zone\create.go meshCell, zone\mapdata.go formationCellAt). The Z it is asked with is 0
// and the vertical band is WIDE, because a drop point carries NO height at all: 300 is
// the band for a point that ALREADY has one (zone\create.go spawnVRange) and 300 was
// measured too narrow for a point that has none (main.go dummyHintVRange, which is the
// same 20000 for the same reason). The horizontal range is the original's own 1000 - the
// number that stops the search jumping across a wall.
//
// A REFUSAL KEEPS THE WALK'S CELL AND SAYS SO. No path engine, or no ground under the
// point at all, and the item keeps the leg's cell; the caller prints which in the DROP
// line, so the next live drop is diagnosable from one line. (No ground under the drop
// point is not a cell problem: it means the body itself was drawn off the mesh, because
// this side walks a straight chord where the client walked around something.)
func (h *Handler) dropPosFor(body [3]int32) ([3]int32, string) {
	if h.NavNear == nil {
		return body, "the walk (no path engine to ask)"
	}
	at, _, ok := h.NavNear([3]int32{body[0], body[1], 0}, dropNearXY, dropNearZ)
	if !ok || !PlausibleCell(at[2]) {
		return body, "the walk (the mesh has no ground under the point)"
	}
	body[2] = at[2]
	return body, "the map's mesh"
}

// The two ranges the ground under a DROP point is searched in. They are the original's
// 1000 for the horizontal (across a wall, no ground is the honest answer) and 20000 for
// the vertical, because the point carries no height: see dropPosFor. Not tuning knobs.
const (
	dropNearXY = 1000
	dropNearZ  = 20000
)

// moneyClassName names the two money classes the drop refusal can trip, for the log.
func moneyClassName(classID int64) string {
	switch classID {
	case VisClassID:
		return "Vis"
	case FesoClassID:
		return "Feso"
	}
	return "money"
}

// useItemTarget answers CZ_USEITEM_TARGET (3651): take ONE unit out of ONE named bag row,
// announce the class, and give the client the stack's new total.
//
// THE GATE IS THE GAME'S OWN DATA, in this order, each refusal ONE line and nothing sent:
//
//  1. the character index must be this team's;
//  2. the target must be SELF - 0, or the acting character's own handle (the two spellings
//     the client and the reference use; see UseItemRequest.Target);
//  3. the slot must own a live row (bagRowAt);
//  4. the item table must have the class AND its GroupName must be `Consume`
//     (GENormalItem.cpp:63-65 lets the player click Consume/Scroll/Period; only Consume has
//     a path here - see usableGroup);
//  5. the row's `UseLv` must not be above the character's level. The client refuses this
//     before sending (GENormalItem.cpp:86-90) and the reference answers
//     MESSAGE_TYPE_USEITEM_INVALID_LV (PlayerChar_Request.cpp:560-566) - this side refuses
//     with a line and no packet, because the client's own message is the one to show.
//
// THEN, in the rail's order: the bag takes exactly ONE (h.Commit, the named row), 3613
// carries the class, and the 3608 carries the new total with REMOVE_ITEM_USE - the type
// whose client handler prints its own "<name> x N used." line (zone\inventory.go). The
// reference's TxUseItem sends the remove first and the 3613 from Update() (TxUseItem.cpp:239
// then :2278-2280); the order is the card's and the client reads neither packet through
// the other.
//
// AND THEN THE EFFECT, IF THE CLASS NAMES ONE: the 3557 goes LAST, after the 3613 and the
// 3608. The reference's own relative order is the item change first and the effect second -
// `SendRemoveItem` at TxUseItem.cpp:238, then `UseConsumeItem` -> the script's `AddHP` at
// :218-221 - so appending it after the pair keeps the two packets this verb already sent
// exactly where they are today and puts the heal on the same side of the item change the
// reference puts it on. (The 3613 is the reference's LAST of the three, :2253-2256, inside
// `UseItem` after `Run` returned; moving ours there is a separate change and not this card.)
func (h *Handler) useItemTarget(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParseUseItemTarget(f.Data)
	if !ok {
		h.Log.Printf("zone: USEITEM refused: %d-byte frame, CZ_USEITEM_TARGET 3651 is a fixed "+
			"%d bytes - nothing sent", len(f.Data), czUseItemTargetSize)
		return nil, nil
	}
	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: USEITEM refused: commander %d character index %d is not in this "+
			"team (%d member(s) spawned) - nothing sent", sess.CommanderID, req.CharIndex,
			len(sess.Team))
		return nil, nil
	}
	char := &sess.Team[req.CharIndex]
	if req.Target != 0 && req.Target != char.Handle {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d (handle %d) named target "+
			"0x%08x, which is not this character - the reference's DispatchUseItem requires "+
			"the acting character itself (PlayerChar_Request.cpp:546-551) - nothing sent",
			sess.CommanderID, req.CharIndex, char.Handle, req.Target)
		return nil, nil
	}

	st, ok := h.bagRowAt(ctx, sess, req.Slot, "USEITEM")
	if !ok {
		return nil, nil
	}

	row, known := h.Items.Row(uint32(st.ClassID))
	if !known {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d named class %d at slot %d "+
			"and the Item table has no row for it - its usable rule cannot be read, so it is "+
			"not used - nothing sent", sess.CommanderID, req.CharIndex, st.ClassID, req.Slot)
		return nil, nil
	}
	if row.GroupName != usableGroup {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d named class %d (%s) at slot "+
			"%d, GroupName %q - only %q has a use path here (a Scroll unlocks a stance and a "+
			"Period item is a timed cash item, neither of which this server carries) - "+
			"nothing sent", sess.CommanderID, req.CharIndex, st.ClassID, row.Name, req.Slot,
			row.GroupName, usableGroup)
		return nil, nil
	}
	if row.UseLv > 0 && int(char.Level) < row.UseLv {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d (level %d) named class %d "+
			"(%s) at slot %d, whose UseLv is %d - nothing sent",
			sess.CommanderID, req.CharIndex, char.Level, st.ClassID, row.Name, req.Slot,
			row.UseLv)
		return nil, nil
	}

	// THE TWO WAYS THIS CLASS CANNOT BE APPLIED, both decided BEFORE the item is taken:
	// a heal whose amount or whose ceiling is not in this build's data is a GAP, and the
	// potion stays in the bag rather than being eaten for a number nobody can name.
	heals := row.ScpType == useItemHealScript
	if heals && !row.Arg1Known {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d named class %d (%s) at "+
			"slot %d, whose ScpType is %s but whose Item row carries no arg1 - the heal "+
			"amount is not in this build's table (a GAP, not a 0-point heal), so the item "+
			"is NOT used and nothing is sent",
			sess.CommanderID, req.CharIndex, st.ClassID, row.Name, req.Slot, row.ScpType)
		return nil, nil
	}
	if heals && char.MaxHP <= 0 {
		h.Log.Printf("zone: USEITEM refused: commander %d char %d (%s) named class %d "+
			"(%s) at slot %d, a heal of %g, but this character's maximum HP is 0 - the "+
			"status script has not answered, so the heal cannot be clamped and the item "+
			"is NOT used and nothing is sent",
			sess.CommanderID, req.CharIndex, char.Name, st.ClassID, row.Name, req.Slot,
			row.Arg1)
		return nil, nil
	}

	changes, err := h.Commit(ctx, sess.CommanderID, nil, []item.Op{{
		Give: false, ClassID: st.ClassID, ItemID: st.ItemID, Count: 1,
	}})
	if err != nil {
		h.Log.Printf("zone: USEITEM refused: commander %d's bag refused to give up one of item "+
			"%d (%v) - nothing was written, nothing sent", sess.CommanderID, st.ItemID, err)
		return nil, nil
	}

	out := [][]byte{BuildUseItem(st.ClassID)}
	remaining := st.Count
	for _, c := range changes {
		if c.Give {
			continue
		}
		remaining = c.Total
		if p := sess.Inv.PacketFor(c, AddItemPlain, RemoveItemUse); p != nil {
			out = append(out, p)
		}
	}

	// THE USE LINE: one line for the verb itself, whatever the effect turns out to be.
	h.Log.Printf("zone: USEITEM commander %d char %d (handle %d) used class %d (%s) from slot "+
		"%d (item %d), %d left; cmd %d (%d B) + cmd %d (%d B); target %d last %d",
		sess.CommanderID, req.CharIndex, char.Handle, st.ClassID, row.Name, req.Slot,
		st.ItemID, remaining, CmdZCUseItem, ZCUseItemSize, CmdZCRemoveItem, 15, req.Target,
		req.Last)

	// THE HP HALF. The three steps are `CFieldChar::AddHP`'s own (FieldChar.cpp:132-165):
	// clamp at the computed maximum, write the ledger, send the 3557 - and the ONE gate
	// that says "no change, no packet" (`hp == expectHP` -> return 0, :156-157).
	//
	// THE LEDGER IS Character.HP AND THERE IS NO DATABASE ROW TO WRITE. The zone reads
	// `character.hp` once, at entry (`SELECT character_id, ... exp, hp, sp ... FROM
	// character WHERE commander_id = $1`, zone\store.go:211-214) and no path in this
	// package ever writes it back - the monster's damage writes `sess.Team[idx].HP` the
	// same way (zone\monster_swing.go:287). So the "normal HP path" here IS the ledger:
	// inventing an UPDATE character SET hp = ... would be a new persistence path, not
	// this card. `swingMu` is the mutex that already makes a monster's read-modify-write
	// on that field one step.
	healSeen := 0
	if heals {
		h.swingMu.Lock()
		before := char.HP
		after := before + row.Arg1
		if after > char.MaxHP {
			after = char.MaxHP
		}
		char.HP = after
		h.swingMu.Unlock()

		if delta := after - before; delta != 0 {
			// +6 the new value, +14 the delta (see the file header on why the delta and
			// not the oracle's 0), +22 = 0 because the delta is positive.
			pkt := BuildAddHP(char.Handle, after, delta)
			out = append(out, pkt)
			// THE OTHER VIEWERS: the same fan-out the attack family uses, which skips
			// this session (it gets the packet in `out`) and applies the update cull.
			healSeen = h.fanOut(sess, char.Handle, pkt)
			h.Log.Printf("zone: USEITEM HEAL commander %d char %d (handle %d) used class "+
				"%d (%s) arg1 %g from the Item row's own ScpType %s: HP %g -> %g of max "+
				"%g (delta %g); cmd %d (%d B), +6 = %g, +14 = %g, +22 = 0, to %d other "+
				"player(s) beside this session",
				sess.CommanderID, req.CharIndex, char.Handle, st.ClassID, row.Name,
				row.Arg1, row.ScpType, before, after, char.MaxHP, delta,
				CmdZCAddHP, len(pkt), after, delta, healSeen)
		} else {
			h.Log.Printf("zone: USEITEM HEAL commander %d char %d (handle %d) used class "+
				"%d (%s) arg1 %g but its HP is already %g of max %g, so the heal changes "+
				"NOTHING and no %d is sent (AddHP returns 0 when hp == expectHP, "+
				"FieldChar.cpp:156-157; the item was still consumed, as the reference's "+
				"transaction consumes it first, TxUseItem.cpp:238)",
				sess.CommanderID, req.CharIndex, char.Handle, st.ClassID, row.Name,
				row.Arg1, before, char.MaxHP, CmdZCAddHP)
		}
	} else if useItemGrantsBuff(row) && h.BuffScript != nil {
		// THE ITEM'S OWN SCRIPT - the reference runs it verbatim:
		// `context->Run(itemProperty->scpType)` with (self, buffName, arg1, arg2)
		// pushed first (PlayerChar.cpp:2245-2250). The two scripts this gate admits
		// are the ones that GRANT a buff:
		//
		//	SCP_USE_ITEM_AddBuff      ge\script\item\item.scp:1567
		//	    -> AddBuffImm(self, BuffName, arg1, arg2)
		//	SCP_USE_ITEM_TeamAddBuff  ge\script\item\item.scp:1762
		//	    -> AddBuffToTeam(self, BuffName, arg1, arg2)
		//
		// 360 of the 3,129 Consume rows name one of them (30 AddBuff + 330 TeamAddBuff,
		// 343 of the 360 carrying a BuffName). The other 57 BuffName rows run a script
		// this build does not arm (SongPyeon, TotalStatA, TransPotion, ...) and keep the
		// old one-line answer below - running them would reach builtins this server does
		// not bind (`AddHP` among them), and an item eaten for a raise is worse than one
		// eaten with a line saying why.
		//
		// NOTHING IS INVENTED: the name and the two numbers come from the item row and
		// the duration from the buff's own ENTER script. The packets the script's
		// AddBuff calls build come back in `w.out` and are appended AFTER the item's
		// two, so the wire is [3613, 3608, 3556, 3125] - the reference's own relative
		// order (its 3556 follows its 3613, PlayerChar.cpp:2252-2260).
		w := &buffWorld{h: h, sess: sess, inScript: true}
		ran, err := h.BuffScript.RunItem(row.ScpType, *char, row.BuffName, row.Arg1,
			row.Arg2, w)
		out = append(out, w.out...)
		if err != nil {
			h.Log.Printf("zone: USEITEM class %d (%s) ran its own script %s and it RAISED: "+
				"%v - the item was consumed (the reference consumes first, TxUseItem.cpp:238) "+
				"and %d buff packet(s) went out before the raise",
				st.ClassID, row.Name, row.ScpType, err, len(w.out))
		} else if !ran {
			h.Log.Printf("zone: USEITEM class %d (%s) names ScpType %q and BuffName %q, but "+
				"this build's corpus does not define that script - the item is announced and "+
				"consumed and no buff is applied",
				st.ClassID, row.Name, row.ScpType, row.BuffName)
		} else {
			h.Log.Printf("zone: USEITEM BUFF class %d (%s) ran its own script %s with "+
				"BuffName %q arg1 %g arg2 %g - %d packet(s) after the item's two (%d B of "+
				"3556 + %d B of 3125 per buff that landed)",
				st.ClassID, row.Name, row.ScpType, row.BuffName, row.Arg1, row.Arg2,
				len(w.out), ChangeBuffSize, BuffRemainTimeSize)
		}
	} else {
		// ONE LINE, and it names what the row's own script is: this build carries no
		// buff/exp/rezz/scroll effect, so the item is announced and consumed and nothing
		// else happens. Saying it plainly beats a silent 0-point heal.
		h.Log.Printf("zone: USEITEM class %d (%s) names ScpType %q, not %q - this build "+
			"carries no other use effect, so only the %d and the %d go out (no HP, no "+
			"buff, no exp)",
			st.ClassID, row.Name, row.ScpType, useItemHealScript, CmdZCUseItem,
			CmdZCRemoveItem)
	}
	return out, nil
}
