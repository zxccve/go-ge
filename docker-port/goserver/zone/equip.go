package zone

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5"

	"geserver/item"
	"geserver/protocol"
)

// Equipping and unequipping: the two client requests, the answer, and the
// transaction behind them.
//
// WHY THIS IS NOT JUST A PACKET: the bag row for a worn item is not deleted, it
// MOVES - `storage` -> `wear_item` - and the item keeps its id. So an equip is
// three writes that must land together or not at all (see PGStore.EquipItem),
// and the client is told with three different packets that have to arrive in the
// shipped server's order (see EquipResult's users below).
const (
	// CmdCZEquip is CZ_EQUIP, client -> server, 16 bytes FIXED:
	// cmd(2) seq(4) checksum(4) charIndex(1) invIndex(4) spot(1).
	// source2\geShared\include\geZCPacket.h:1352.
	CmdCZEquip = 3614

	// CmdCZUnequip is CZ_UNEQUIP, client -> server, 12 bytes FIXED:
	// cmd(2) seq(4) checksum(4) charIndex(1) spot(1).
	// source2\geShared\include\geZCPacket.h:1359.
	CmdCZUnequip = 3615

	// CmdZCUpdateEquipment is ZC_UPDATE_EQUIPMENT, server -> client, VARIABLE
	// (264 bytes as this server builds it): cmd(2) len(2) handle(4)
	// EQUIPMENT_PC(21 slots x 12 bytes) optionCount(4).
	// source2\geShared\include\geZCPacket.h:1375. The 12-byte slot is MEASURED
	// in the client, not read from that header - see the note on
	// zcUpdateEquipItemSize.
	CmdZCUpdateEquipment = 3616

	// czEquipSize and czUnequipSize are the client's fixed frame sizes with the
	// 10-byte PKS_SECURITY_DEFAULT header (cmd(2) seq(4) checksum(4),
	// gePacketBase.h:52) included: 10+1+4+1 and 10+1+1. Both are the size the
	// command table carries (data\cmd_table_live.txt:1281-1282), so a short
	// frame is a broken client, and it is refused rather than read past.
	czEquipSize   = 16
	czUnequipSize = 12

	// AddItemUnequip is ADD_ITEM_UNEQUIP, the addType an unequip sends: the
	// client prints UI_ALERT_MSG_103 from its own table
	// (source2\geShared\include\geZCTypes.h).
	AddItemUnequip uint8 = 4

	// RemoveItemEquip is REMOVE_ITEM_EQUIP, the removeType an equip sends: the
	// client's 3608 handler prints "%s equipped" for it
	// (source2\geClient\Source\GENormalItem.cpp:459, :560-565), and it is the
	// reason the worn row visibly LEAVES the bag. The enum is in
	// source2\geShared\include\geZCTypes.h.
	//
	// It is NOT ZC_REMOVEEQUIPITEM (3609): that packet is about a WORN item being
	// destroyed (a drop or a death), it carries invType and settlerItem, and it
	// has no part in an equip.
	RemoveItemEquip uint8 = 5
)

// EquipRequest is one decoded CZ_EQUIP.
type EquipRequest struct {
	// CharIndex is the client's "multi character index": the position in THIS
	// connection's own team list, in the order this server spawned it - the same
	// order as Session.Team. The shipped server indexes m_pcList by it after a
	// CanControlPC check (Commander_Packet.cpp:804-816).
	CharIndex int
	// InvIndex is the BAG POSITION the client is pointing at (1-based), i.e. the
	// key space Session.Inv mirrors. It is resolved through InvMirror.IDAt, never
	// turned into a database row offset.
	InvIndex int32
	// Spot is the EQUIP SLOT the client asked for, taken FROM THE PACKET. The
	// shipped server accepts it and, when it disagrees with the item, moves the
	// item to the item's own default slot (EquipSlotControl.cpp:276-280); this
	// zone REFUSES the disagreement instead, which is what item.EquipSlots.Allows
	// checks in the transaction.
	Spot int
}

// UnequipRequest is one decoded CZ_UNEQUIP.
type UnequipRequest struct {
	CharIndex int
	Spot      int
}

// ParseEquip reads CZ_EQUIP (3614):
//
//	+0 cmd(2)=3614  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)
//	+11 invIndex(4, DWORD)  +15 spot(1)
//
// ok is false when the frame is shorter than the client's fixed 16 bytes. The
// caller answers NOTHING in that case: a truncated frame is a broken client, and
// reading past it is worse than refusing.
func ParseEquip(pt []byte) (EquipRequest, bool) {
	var req EquipRequest
	if len(pt) < czEquipSize {
		return req, false
	}
	req.CharIndex = int(pt[10])
	req.InvIndex = int32(binary.LittleEndian.Uint32(pt[11:]))
	req.Spot = int(pt[15])
	return req, true
}

// ParseUnequip reads CZ_UNEQUIP (3615):
//
//	+0 cmd(2)=3615  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 spot(1)
//
// ok is false for a frame shorter than the client's fixed 12 bytes.
func ParseUnequip(pt []byte) (UnequipRequest, bool) {
	var req UnequipRequest
	if len(pt) < czUnequipSize {
		return req, false
	}
	req.CharIndex = int(pt[10])
	req.Spot = int(pt[11])
	return req, true
}

// BuildUpdateEquipment makes ZC_UPDATE_EQUIPMENT (3616): the character's whole
// worn set, 264 bytes:
//
//	+0   cmd(2)=3616  +2 len(2)=WHOLE packet  +4 handle(4)
//	+8   EQUIPMENT_PC: 21 slots of 12 bytes, class id FIRST, two zero words  -> 260
//	+260 optionCount(4, 0)                                                  -> 264
//
// ONE SLOT IS 12 BYTES, NOT 4. This is the field that crashed the client on
// 2026-10-28 at ge.exe+0x3AA80B (see history\agent-traps.md): the client's own
// equipment array is 21 x 12 with the class id at the front, and 3616 is copied
// straight over it. A 4-byte-per-slot packet is read as class ids at byte
// offsets 0, 12, 24 ... 72 - which by luck still land on real "None" ids - and
// then at 84, the first byte of the reinforcement block, which is 0. A zero
// class id has no item property, and the client dereferences it anyway.
//
// Two independent measurements in the shipped client say 12, and both agree
// with our own START_INFO, which fills the same struct and works:
//
//   - the initializer at 0x0064B4E6 stores the 21 "None" class ids at
//     ECX+0, ECX+0xC, ECX+0x18 ... ECX+0xF0 (stride 0xC, 21 entries) and then
//     zeroes the second word of each entry with `LEA EAX,[EAX+0xC]`;
//   - the walker that crashed loops a byte offset from 0 to 0xFC (252 = 21*12)
//     and reads a DWORD at `[EAX+ESI+0x59]`, ESI being that byte offset.
//
// The live server builds it once per equip with `len = bin.GetPosition()`
// (PlayerChar.cpp:2341-2352) and sends it to the owner, and the client applies it
// only when `Session().IsMyPc(packet->handle)` (GENormalNet_Action.cpp:1404-1415)
// - so `handle` must be the CHARACTER id, which is Character.Handle here (the
// database's character_id; the client and the server agree on it).
//
// A slot with nothing worn is written as EquipNone[spot], NEVER 0: a zero class
// id made the client dereference a null item property and crash - the same rule
// BuildStartInfo follows, and the reason this function reuses the same table.
func BuildUpdateEquipment(handle uint32, equip [equipSpotLast]uint32) []byte {
	out := make([]byte, zcUpdateEquipmentSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUpdateEquipment)
	binary.LittleEndian.PutUint32(out[4:], handle)
	for spot := 0; spot < equipSpotLast; spot++ {
		cls := EquipNone[spot]
		if equip[spot] != 0 {
			cls = equip[spot]
		}
		binary.LittleEndian.PutUint32(out[zcUpdateEquipmentItems+spot*zcUpdateEquipItemSize:], cls)
	}
	// The 8 bytes after each class id stay zero: the client's own initializer
	// zeroes the word at +4 of every slot, and this server carries no
	// reinforcement and no item options yet. optionCount stays zero too - the
	// client reads it as a count, not as an optional field.
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// The offsets inside ZC_UPDATE_EQUIPMENT.
const (
	zcUpdateEquipmentItems = 8 // +8, the first of the 21 slots
	// zcUpdateEquipItemSize is ONE slot of the client's equipment array, and it
	// is 12 bytes because the CLIENT's array is (initializer 0x0064B4E6 and the
	// walker at 0x007AA80B, stride 0xC, 21 entries). It is the same 12 the
	// START_INFO appearance block uses (equipEntrySize) - both packets carry the
	// same struct, so they cannot disagree.
	zcUpdateEquipItemSize   = 12
	zcUpdateEquipmentOption = 260 // +260, the option count: 8 + 21*12
	zcUpdateEquipmentSize   = 264 // 8 + 21*12 + 4
)

// EquipResult is what ONE equip or unequip did, in the terms the client has to be
// told about. It is returned by the store (PGStore.EquipItem / UnequipItem) so the
// handler can build the packets without a second read.
type EquipResult struct {
	// Spot is the slot that changed, 0..20.
	Spot int
	// ClassID is the class now worn there (0 when the slot is now empty).
	ClassID int64
	// Removed is the item id that LEFT THE BAG - the one that was equipped. It is
	// 0 for an unequip, where nothing leaves the bag.
	Removed int64
	// Returned is the item that went BACK to the bag: the displaced item on an
	// equip (the SWAP), and the item that came off on an unequip. nil means the
	// slot was empty and nothing was returned.
	Returned *item.Stack
	// Equip is the character's FULL worn set AFTER the change, read back inside
	// the same transaction. It is what ZC_UPDATE_EQUIPMENT carries, so it is the
	// database's answer, not this side's prediction.
	Equip [equipSpotLast]uint32
}

// equipTarget runs the checks both verbs share: a commander to write to, a
// character this session actually spawned, and a spot the appearance block can
// hold. It answers the TEAM INDEX, and false means one line has been logged and
// nothing may be written or sent.
//
// `verb` is only the word in the log line ("EQUIP" / "UNEQUIP"), so the two
// refusals read the same way in a log a player has to send in.
func (h *Handler) equipTarget(sess *Session, charIndex, spot int, verb string) (int, bool) {
	if sess.CommanderID == 0 {
		h.Log.Printf("zone: %s refused: this session has no commander, and an item "+
			"transaction writes to a commander (not to account 0)", verb)
		return 0, false
	}
	if charIndex < 0 || charIndex >= len(sess.Team) {
		h.Log.Printf("zone: %s refused: character index %d is not in this team "+
			"(%d member(s) spawned)", verb, charIndex, len(sess.Team))
		return 0, false
	}
	if spot < 0 || spot >= equipSpotLast {
		h.Log.Printf("zone: %s refused: spot %d is outside 0..%d, the appearance "+
			"block's own slots", verb, spot, equipSpotLast-1)
		return 0, false
	}
	return charIndex, true
}

// equip answers CZ_EQUIP (3614): move one bag row into a slot.
//
// The reply is the shipped server's own three-step order (TxEquipItem.cpp,
// Update()): the displaced item back into the bag (3607, ADD_ITEM_UNEQUIP), then
// the worn item out of it (3608, REMOVE_ITEM_EQUIP), and only THEN the whole worn
// set (3616). That order is what makes the bag row vanish and the paper doll
// update in the same breath.
//
// EVERY refusal logs one line naming its numbers and answers NOTHING - the item
// stays where it is and the client is not told a story the database does not
// hold.
func (h *Handler) equip(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParseEquip(f.Data)
	if !ok {
		h.Log.Printf("zone: EQUIP refused: %d-byte frame, CZ_EQUIP is a fixed %d bytes",
			len(f.Data), czEquipSize)
		return nil, nil
	}
	i, ok := h.equipTarget(sess, req.CharIndex, req.Spot, "EQUIP")
	if !ok {
		return nil, nil
	}
	if sess.Inv == nil {
		h.Log.Printf("zone: EQUIP refused: no inventory list was sent this session, "+
			"so bag index %d names nothing", req.InvIndex)
		return nil, nil
	}
	itemID, known := sess.Inv.IDAt(req.InvIndex)
	if !known {
		h.Log.Printf("zone: EQUIP refused: bag index %d is a hole, or past the %d "+
			"position(s) this session was sent", req.InvIndex, sess.Inv.Len())
		return nil, nil
	}
	if h.Equip == nil {
		h.Log.Printf("zone: EQUIP refused: this zone has no equip write path "+
			"(Handler.Equip is nil), so item %d stays in the bag", itemID)
		return nil, nil
	}

	handle := sess.Team[i].Handle
	res, err := h.Equip(ctx, sess.CommanderID, int32(handle), itemID, req.Spot)
	if err != nil {
		// The transaction rolled back: the item is where it was, so the client
		// is told nothing at all.
		h.Log.Printf("zone: EQUIP of item %d at spot %d for commander %d failed "+
			"(nothing written): %v", itemID, req.Spot, sess.CommanderID, err)
		return nil, nil
	}

	var out [][]byte
	// (a) The displaced item is back in the bag - the SWAP half. Its 3607 goes
	// FIRST because the shipped server sends the history entries in that order.
	if res.Returned != nil {
		pkt := sess.Inv.PacketFor(item.Change{
			Give: true, ClassID: res.Returned.ClassID,
			ItemID: res.Returned.ItemID, Total: res.Returned.Count,
		}, AddItemUnequip, RemoveItemEquip)
		if pkt == nil {
			h.Log.Printf("zone: EQUIP: item %d went back to the bag but the client's "+
				"list has no position for it, so that was NOT announced "+
				"(the row did move)", res.Returned.ItemID)
		} else {
			out = append(out, pkt)
		}
	}
	// (b) The worn item leaves the bag. count 0 removes the position, and
	// REMOVE_ITEM_EQUIP is what makes the client print "equipped".
	pkt := sess.Inv.PacketFor(item.Change{
		Give: false, ClassID: res.ClassID, ItemID: res.Removed, Total: 0,
	}, AddItemUnequip, RemoveItemEquip)
	if pkt == nil {
		h.Log.Printf("zone: EQUIP: the client's list has no row for item %d, so it "+
			"was NOT told the item left the bag (it did leave)", res.Removed)
	} else {
		out = append(out, pkt)
	}
	// (c) The whole worn set.
	out = append(out, BuildUpdateEquipment(handle, res.Equip))

	// The session's copy follows the database, so a later spawn from this same
	// session (another player joining after the swap) carries the new appearance.
	// It moves BEFORE the two packets below, which are built from it.
	sess.Team[i].Equip = res.Equip

	// (d) The BODY, then the three lists that describe its items.
	//
	// 3608 only takes the row out of the bag and 3616 only updates the numbers:
	// the model is redrawn by 3546 alone (see appearance.go). The three lists come
	// last, empty, because this server stores no reinforcement, sockets or extra
	// properties - 0 is what the reference sends for a character with none.
	out = append(out, h.sendEquipAppearance(sess, i, handle)...)

	// (e) The DERIVED half of the sheet, re-sent. The client computes none of
	// ATK, DEF, A.R., D.R., HR, CSPD, the resists or MSPD: those words change only
	// when a 3684 arrives. The reference server re-sends the whole block after an
	// equipment change - OnChangedEquipment ends with SendCurStance
	// (PlayerChar.cpp:2382), which calls SendAllStatus (PlayerChar.cpp:1043) - and
	// the live capture shows it doing exactly that: the same character's 3684
	// appears three times in captures\charpanel2\20261029-165952, once at entry and
	// again after each of the owner's equip changes. Without this line the panel
	// keeps the numbers it drew at entry until the next zone.
	//
	// The STANCE is re-picked first, or the sheet answers for the weapon the
	// character held at zone entry: the reference server's OnChangedEquipment ->
	// SendCurStance pair does exactly this, and without it a gear change can never
	// move CSPD (BackGuard 10110 and Bareknuckle 10010 are different numbers).
	c := &sess.Team[i]
	if h.Stances != nil {
		if pick, ok := h.Stances.PickStance(int(c.Job), h.Stances.Hands(*c)); ok {
			c.Stance = pick.Stance
		}
	}
	// THE MAXIMA FIRST: max HP and max SP are BASIC_STAT, so they have to be on the
	// character before the packet that draws their bars is built.
	h.refreshSheet(c)
	statPkt, statSheet := StatusPacketFor(sess.Team[i], h.Status)
	out = append(out, statPkt)
	h.Log.Printf("zone: EQUIP: 3684 re-sent for char %d (%d): ATK %d DEF %d AR %d "+
		"DR %d HR %d CSPD %d MSPD %d (%d bytes)", i, handle,
		int(statSheet.ATK), int(statSheet.DEF), int(statSheet.AR),
		int(statSheet.DR), int(statSheet.HR), int(statSheet.CSPD),
		int(statSheet.MSPD), len(statPkt))

	h.Log.Printf("zone: EQUIP commander %d char %d (%d): item %d (class %d) -> spot "+
		"%d, %d back to the bag, %d packet(s)",
		sess.CommanderID, i, handle, res.Removed, res.ClassID, res.Spot,
		returnedCount(res.Returned), len(out))
	return out, nil
}

// sendEquipAppearance answers ONE character's worn-set change with the packets
// that redraw the body and describe its items, and forwards the redraw to every
// OTHER player in the zone.
//
// It returns the packets for this player's own connection, in the order they must
// be written: the appearance (3546) FIRST, then the three lists (3871, 3859,
// 3861).
//
// WHY THE OTHER PLAYERS: "broadcast" is what the reference does -
// CPlayerChar::BroadcastUpdateAppearance (PlayerChar.cpp:458-467) fills one packet
// and calls BroadcastPacket, i.e. the owner AND everyone standing nearby. Without
// the fan-out the armour appears only on the wearer's screen, which is the other
// half of the same bug. It reuses sendOthers, the one fan-out movement already
// uses, so a dead socket costs a log line and not the packet.
//
// ORDER, AND THE ONE PLACE THIS DIFFERS FROM THE REFERENCE: OnChangedEquipment
// (PlayerChar.cpp:2353-2355, :2379) sends reinforce, socket and property BEFORE
// BroadcastUpdateAppearance. The lane that asked for this fixes the order the
// other way round, appearance first, so the body redraws as early as possible.
// Both are safe because the client's handlers are independent: 3546 goes to the
// world actor (ChangeAppearance) and the three lists go to UISession's store,
// keyed by charIndex (GEModeNet.cpp:245-268). Neither reads the other.
//
// `index` is the character's place in the session's team: it is what the three
// lists carry as charIndex, and what the roster needs to remember the new worn set
// so a player who joins LATER is spawned wearing the armour.
func (h *Handler) sendEquipAppearance(sess *Session, index int, handle uint32) [][]byte {
	c := sess.Team[index]
	pkt := BuildUpdatedPCAppearance(handle, c)
	lists := BuildEquipInfoLists(uint8(index))

	others := h.sendOthers(sess, handle, pkt)
	// The roster holds COPIES of the team (players.go), so it must be told too or
	// the next joiner sees the character in the armour they wore at zone entry.
	h.Players.SetEquip(sess, index, c.Equip)
	h.Log.Printf("zone: APPEARANCE handle %d (char %d, %d bytes) -> %d other player(s), "+
		"then equip lists %d/%d/%d with count 0",
		handle, index, len(pkt), others, CmdZCEquipReinforceInfoList,
		CmdZCEquipSocketInfoList, CmdZCEquipAdditionPropList)
	return append([][]byte{pkt}, lists...)
}

// unequip answers CZ_UNEQUIP (3615): move one worn item back into the bag.
//
// The reply is the SAME item id and count that was worn - the row moves, it is
// not minted again - so the client's 3607 must carry the item id it already knew
// about, at the position the mirror gives it. There is no 3608 here: nothing
// leaves the bag.
func (h *Handler) unequip(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParseUnequip(f.Data)
	if !ok {
		h.Log.Printf("zone: UNEQUIP refused: %d-byte frame, CZ_UNEQUIP is a fixed %d bytes",
			len(f.Data), czUnequipSize)
		return nil, nil
	}
	i, ok := h.equipTarget(sess, req.CharIndex, req.Spot, "UNEQUIP")
	if !ok {
		return nil, nil
	}
	if sess.Inv == nil {
		h.Log.Printf("zone: UNEQUIP refused: no inventory list was sent this session, " +
			"so the returned item has no position to land at")
		return nil, nil
	}
	if h.Unequip == nil {
		h.Log.Printf("zone: UNEQUIP refused: this zone has no unequip write path "+
			"(Handler.Unequip is nil), so spot %d keeps its item", req.Spot)
		return nil, nil
	}

	handle := sess.Team[i].Handle
	res, err := h.Unequip(ctx, sess.CommanderID, int32(handle), req.Spot)
	if err != nil {
		h.Log.Printf("zone: UNEQUIP of spot %d for commander %d failed "+
			"(nothing written): %v", req.Spot, sess.CommanderID, err)
		return nil, nil
	}
	if res.Returned == nil {
		// The store answers with the item that came off. Without it the client
		// would be shown an empty slot and no row to put the item in, so half the
		// answer is worse than none.
		h.Log.Printf("zone: UNEQUIP spot %d for commander %d reported no item, so "+
			"nothing is sent", req.Spot, sess.CommanderID)
		return nil, nil
	}

	pkt := sess.Inv.PacketFor(item.Change{
		Give: true, ClassID: res.Returned.ClassID,
		ItemID: res.Returned.ItemID, Total: res.Returned.Count,
	}, AddItemUnequip, RemoveItemEquip)
	var out [][]byte
	if pkt == nil {
		h.Log.Printf("zone: UNEQUIP: item %d went back to the bag but the client's "+
			"list has no position for it, so that was NOT announced "+
			"(the row did move)", res.Returned.ItemID)
	} else {
		out = append(out, pkt)
	}
	out = append(out, BuildUpdateEquipment(handle, res.Equip))

	// The session's copy first: the appearance below is built from it.
	sess.Team[i].Equip = res.Equip

	// The body, then the three lists - the same answer as an equip, and for the
	// same reason: the item leaves the paper doll's slot, so the model has to be
	// rebuilt or the client keeps drawing the armour that is now in the bag.
	out = append(out, h.sendEquipAppearance(sess, i, handle)...)

	// The same re-send the equip path does, for the same reason: taking an item
	// OFF changes DEF, AR, DR, HR, CSPD and the resists, and none of those is
	// recomputed by the client. The reference server answers an unequip through the
	// same OnChangedEquipment -> SendAllStatus pair (PlayerChar.cpp:2382, :1043).
	//
	// The stance is re-picked for the hands the character has NOW, exactly as the
	// equip path does: the reference answers a gear change with OnChangedEquipment
	// -> SendCurStance, and without it the sheet keeps the stance from zone entry -
	// which is why a weapon removal could never move CSPD before.
	c := &sess.Team[i]
	if h.Stances != nil {
		if pick, ok := h.Stances.PickStance(int(c.Job), h.Stances.Hands(*c)); ok {
			c.Stance = pick.Stance
		}
	}
	// The maxima first, for the same reason the equip path does it: HP and SP maxima
	// are BASIC_STAT, and the bar is drawn against them.
	h.refreshSheet(c)
	statPkt, statSheet := StatusPacketFor(sess.Team[i], h.Status)
	out = append(out, statPkt)
	h.Log.Printf("zone: UNEQUIP: 3684 re-sent for char %d (%d): ATK %d DEF %d AR %d "+
		"DR %d HR %d CSPD %d MSPD %d (%d bytes)", i, handle,
		int(statSheet.ATK), int(statSheet.DEF), int(statSheet.AR),
		int(statSheet.DR), int(statSheet.HR), int(statSheet.CSPD),
		int(statSheet.MSPD), len(statPkt))

	h.Log.Printf("zone: UNEQUIP commander %d char %d (%d): item %d (class %d) from "+
		"spot %d back to the bag, %d packet(s)",
		sess.CommanderID, i, handle, res.Returned.ItemID, res.Returned.ClassID,
		res.Spot, len(out))
	return out, nil
}

// returnedCount is the count put in the log line: an item that went back to the
// bag, or 0 when the slot was empty.
func returnedCount(st *item.Stack) int64 {
	if st == nil {
		return 0
	}
	return st.Count
}

// ---- the transaction ------------------------------------------------------

// EquipItem moves ONE bag row into ONE wear slot, in ONE transaction.
//
// The steps are the shipped server's own order (TxEquipItem.cpp), and each one
// re-reads with a lock so two clicks cannot both take the same stack:
//
//  1. the character belongs to the commander (and is not a card);
//  2. the bag row, FOR UPDATE;
//  3. the SLOT CHECK, before anything is written - a class with no slot, or a
//     slot the class does not take, is refused and the rows stay untouched;
//  4. the slot's current item goes back to `storage` (the SWAP);
//  5. the bag row goes;
//  6. the wear row is written;
//  7. the whole worn set is read back for the packet.
//
// The `item` row is never deleted or re-minted: an item keeps its id for its
// whole life, which is the only reason the client can follow it from the bag to
// the paper doll and back.
func (s *PGStore) EquipItem(ctx context.Context, commanderID, characterID int32, itemID int64, spot int) (EquipResult, error) {
	var res EquipResult
	// A missing table must never read as "wearable": without it no class can be
	// placed anywhere, so every equip is refused and the error names the emitter.
	if s.Wearable == nil {
		return res, fmt.Errorf("no wearable-slot table is loaded, so item %d cannot be "+
			"checked against spot %d; build it with: "+
			"cd src && python emit_item_equip.py", itemID, spot)
	}
	if spot < 0 || spot >= equipSpotLast {
		return res, fmt.Errorf("spot %d is outside 0..%d", spot, equipSpotLast-1)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin equip of item %d for character %d: %w",
			itemID, characterID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkCharacterOwner(ctx, tx, commanderID, characterID); err != nil {
		return res, err
	}

	// (2) The bag row, locked: the class decides the slot, and the delete below
	// must not race a sale of the same row.
	var (
		classID int64
		count   int64
	)
	err = tx.QueryRow(ctx,
		`SELECT i.class_id, i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.item_id = $1 AND s.commander_id = $2
		    AND s.type = 0 AND s.trash = 0
		  FOR UPDATE OF i`, itemID, commanderID).Scan(&classID, &count)
	switch {
	case err == pgx.ErrNoRows:
		return res, fmt.Errorf("item %d is not in commander %d's bag", itemID, commanderID)
	case err != nil:
		return res, fmt.Errorf("read item %d for commander %d: %w", itemID, commanderID, err)
	}

	// (3) The slot check. BEFORE any write, so a wrong request leaves both tables
	// exactly as they were - the shipped server would instead move the item to its
	// own default slot, silently.
	if !s.Wearable.Allows(classID, spot) {
		return res, fmt.Errorf("class %d cannot be worn at spot %d", classID, spot)
	}

	// (4) The swap: whatever was worn there goes back to the bag, with its own id
	// and count. No row here means the slot was empty.
	var displaced int64
	err = tx.QueryRow(ctx,
		`DELETE FROM wear_item WHERE character_id = $1 AND spot = $2
		 RETURNING item_id`, characterID, spot).Scan(&displaced)
	switch {
	case err == pgx.ErrNoRows:
		// Nothing worn: the slot is now filled from empty.
	case err != nil:
		return res, fmt.Errorf("clear spot %d of character %d: %w", spot, characterID, err)
	default:
		st, err := readStack(ctx, tx, displaced)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO storage (item_id, commander_id) VALUES ($1, $2)`,
			displaced, commanderID); err != nil {
			return res, fmt.Errorf("return item %d to commander %d's bag: %w",
				displaced, commanderID, err)
		}
		res.Returned = &st
	}

	// (5) The bag row goes - the item itself stays.
	if _, err := tx.Exec(ctx,
		`DELETE FROM storage WHERE item_id = $1 AND commander_id = $2`,
		itemID, commanderID); err != nil {
		return res, fmt.Errorf("remove item %d from commander %d's bag: %w",
			itemID, commanderID, err)
	}
	// (6) ...and it is worn.
	if _, err := tx.Exec(ctx,
		`INSERT INTO wear_item (character_id, spot, item_id) VALUES ($1, $2, $3)`,
		characterID, spot, itemID); err != nil {
		return res, fmt.Errorf("wear item %d at spot %d of character %d: %w",
			itemID, spot, characterID, err)
	}
	// (7) The whole worn set, read inside this transaction: the packet carries
	// what the database now holds.
	if err := readWornSet(ctx, tx, characterID, &res); err != nil {
		return res, err
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit equip of item %d: %w", itemID, err)
	}
	res.Spot = spot
	res.ClassID = classID
	res.Removed = itemID
	return res, nil
}

// UnequipItem moves ONE worn item back into the bag, in ONE transaction. The SAME
// item id and count come back: the row moves, it is not minted again.
func (s *PGStore) UnequipItem(ctx context.Context, commanderID, characterID int32, spot int) (EquipResult, error) {
	var res EquipResult
	if spot < 0 || spot >= equipSpotLast {
		return res, fmt.Errorf("spot %d is outside 0..%d", spot, equipSpotLast-1)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin unequip of spot %d for character %d: %w",
			spot, characterID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkCharacterOwner(ctx, tx, commanderID, characterID); err != nil {
		return res, err
	}

	var itemID int64
	err = tx.QueryRow(ctx,
		`SELECT item_id FROM wear_item
		  WHERE character_id = $1 AND spot = $2
		  FOR UPDATE`, characterID, spot).Scan(&itemID)
	switch {
	case err == pgx.ErrNoRows:
		return res, fmt.Errorf("character %d wears nothing at spot %d", characterID, spot)
	case err != nil:
		return res, fmt.Errorf("read spot %d of character %d: %w", spot, characterID, err)
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM wear_item WHERE character_id = $1 AND spot = $2`,
		characterID, spot); err != nil {
		return res, fmt.Errorf("clear spot %d of character %d: %w", spot, characterID, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO storage (item_id, commander_id) VALUES ($1, $2)`,
		itemID, commanderID); err != nil {
		return res, fmt.Errorf("return item %d to commander %d's bag: %w",
			itemID, commanderID, err)
	}

	st, err := readStack(ctx, tx, itemID)
	if err != nil {
		return res, err
	}
	res.Returned = &st
	res.ClassID = st.ClassID

	if err := readWornSet(ctx, tx, characterID, &res); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit unequip of item %d: %w", itemID, err)
	}
	res.Spot = spot
	return res, nil
}

// checkCharacterOwner refuses a character that is not the commander's, or is a
// card. It is the first write-path check in both verbs: without it a forged
// character id would let a player dress somebody else's character.
func checkCharacterOwner(ctx context.Context, tx pgx.Tx, commanderID, characterID int32) error {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM character
		  WHERE character_id = $1 AND commander_id = $2 AND iscard = 0`,
		characterID, commanderID).Scan(&one)
	switch {
	case err == pgx.ErrNoRows:
		return fmt.Errorf("character %d is not commander %d's (or is a card)",
			characterID, commanderID)
	case err != nil:
		return fmt.Errorf("check character %d of commander %d: %w",
			characterID, commanderID, err)
	}
	return nil
}

// readStack reads one item row's class and count, inside the caller's
// transaction.
func readStack(ctx context.Context, tx pgx.Tx, itemID int64) (item.Stack, error) {
	var st item.Stack
	err := tx.QueryRow(ctx,
		`SELECT class_id, count FROM item WHERE item_id = $1`, itemID).
		Scan(&st.ClassID, &st.Count)
	if err == pgx.ErrNoRows {
		return st, fmt.Errorf("item %d has no row, so its class and count are unknown",
			itemID)
	}
	if err != nil {
		return st, fmt.Errorf("read item %d: %w", itemID, err)
	}
	st.ItemID = itemID
	return st, nil
}

// readWornSet fills res.Equip from the character's wear rows, inside the
// transaction that just changed them.
//
// A spot outside 0..20 is skipped rather than written past the end of Equip - the
// same guard loadWorn uses, because a bad row must not corrupt the packet.
func readWornSet(ctx context.Context, tx pgx.Tx, characterID int32, res *EquipResult) error {
	rows, err := tx.Query(ctx,
		`SELECT w.spot, i.class_id
		   FROM wear_item w JOIN item i ON i.item_id = w.item_id
		  WHERE w.character_id = $1`, characterID)
	if err != nil {
		return fmt.Errorf("worn set of character %d: %w", characterID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			spot  int
			class int64
		)
		if err := rows.Scan(&spot, &class); err != nil {
			return fmt.Errorf("scan worn row: %w", err)
		}
		if spot < 0 || spot >= equipSpotLast {
			continue
		}
		res.Equip[spot] = uint32(class)
	}
	return rows.Err()
}
