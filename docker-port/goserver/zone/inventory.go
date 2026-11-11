package zone

import (
	"encoding/binary"

	"geserver/item"
)

// What the client is told when ONE bag row changes.
//
// These are the INCREMENTAL packets, not a fresh list: a bulk list re-lays every
// slot (the bag visibly reshuffles) and does not refresh an open shop, while these
// two do. They are also the only way the client prints its own "you received X"
// line - we never write that text, the client builds it.
const (
	CmdZCAddItem    = 3607 // server -> client: VAR (length word = 44)
	CmdZCRemoveItem = 3608 // server -> client: FIXED 15, NO length word

	// addType picks WHICH line the client prints, from ITS OWN string table - the
	// server never writes that text. The full list is in
	// `source2\geShared\include\geZCTypes.h`, and the client's switch over it is
	// `GENormalItem.cpp`:
	//
	//	ADD_ITEM(0) / ADD_ITEM_PICK(3)     -> UI_ALERT_MSG_177  "obtained X N"
	//	ADD_ITEM_BUY(1) / USERSHOP_BUY(6)  -> UI_ALERT_MSG_102  "... purchased"
	//	CUSTOM_SHOP_BUY(12)                -> 102 + UpdateData("customshop")
	//	ADD_ITEM_UNEQUIP(4)                -> 103
	//	ADD_ITEM_EXCHANGE(5)               -> 104
	//	ADD_ITEM_SELL(2) / USERSHOP_SELL(7)-> no message at all
	//
	// A script's reward is NOT a purchase, so it is ADD_ITEM. Sending 12 here made
	// the client say "purchased" and refresh a shop panel that had nothing to do
	// with it (seen live 2026-10-25).
	AddItemPlain uint8 = 0
	// AddItemPick is ADD_ITEM_PICK (3), the type a PICKUP sends. The client's switch
	// gives it UI_ALERT_MSG_177 ("obtained X N") - the same line ADD_ITEM prints, and
	// the reason a pickup must not ride on ADD_ITEM_BUY, which says "purchased". The
	// reference sends exactly this value from CTxPickItem::Update (TxPickItem.cpp:217).
	AddItemPick uint8 = 3
	// The shop lines, from the same client switch and the same enum. A buy takes
	// the item INTO the bag (ADD_ITEM_BUY), a sale takes it OUT (ADD_ITEM_SELL,
	// which the client answers silently), and a custom-shop buy is 12 - the same
	// number as the buy line plus a panel refresh.
	AddItemBuy  uint8 = 1
	AddItemSell uint8 = 2
	// AddItemExchange makes the client print "You obtained N <item>." in the
	// YELLOW get-item font (UI_ALERT_MSG_212). For the Money class (Vis/Feso)
	// this is the ONLY add type that prints: the Money branch handles EXCHANGE
	// and WAREHOUSE and skips the generic "You got" line (GENormalItem.cpp).
	AddItemExchange      uint8 = 5
	AddItemCustomShopBuy uint8 = 12
	// removeType: 0 is the unnamed first value, which is what a script's take uses
	// (SELL=1, BUY=2 and CUSTOM_SHOP=16 belong to the shop paths).
	RemoveItemTake uint8 = 0
	// The shop removes. The enum starts with an UNNAMED 0 (geZCTypes.h), so SELL
	// is 1 and not 0; CUSTOM_SHOP is 16 (MARKET_BUY is 15, and sending that was a
	// silent no-op in the Python server).
	RemoveItemSell       uint8 = 1
	RemoveItemBuy        uint8 = 2
	RemoveItemCustomShop uint8 = 16
	// RemoveItemUse is REMOVE_ITEM_USE: "consumed by USING the item" (geZCTypes.h's
	// enum, the 9th value after the unnamed 0, so 8). It is what a Warp Scroll charge
	// sends, and the type matters to the client: GENormalItem.cpp:568 turns it into
	// its own "<name> x N used." line, where REMOVE_ITEM (0) prints nothing at all.
	RemoveItemUse uint8 = 8
)

// BuildAddItem is 44 bytes:
//
//	cmd(2) len(2) ItemType(4) InvIndex(4) Count(4) PType(1) GUID(8) addType(1)
//	expireTime(4) optionCount(4) socketCount(4) isReinforce(1) WaitSecond(1) autoRule(4)
//
// LAYOUT PROVEN against the reference capture AND the decompiled handler
// (`CNormalNet::AddItem` reads expireTime at +0x1a and WaitSecond at +0x27 - see
// progress\2026-10.md, 2026-10-17). NOTE the two SINGLE bytes near the end: the
// Python oracle packs them as 4-byte ints, which makes its packet 50 bytes while
// its length word still says 44. That is the discrepancy that kept this packet out
// of the Go zone; the capture decides it, and the capture says 44.
//
// `count` is the NEW TOTAL the row should show, not the amount gained.
func BuildAddItem(classID int64, invIndex, count int32, addType uint8) []byte {
	out := make([]byte, 44)
	binary.LittleEndian.PutUint16(out[0:], CmdZCAddItem)
	binary.LittleEndian.PutUint16(out[2:], 44)
	binary.LittleEndian.PutUint32(out[4:], uint32(classID))
	binary.LittleEndian.PutUint32(out[8:], uint32(invIndex))
	binary.LittleEndian.PutUint32(out[12:], uint32(count))
	// PType at 16 stays 0 (no protection). The GUID at 17..24 is never read by the
	// handler, so it stays 0 rather than inventing a value the client might key on.
	out[25] = addType
	binary.LittleEndian.PutUint32(out[26:], 0xFFFFFFFF) // expireTime: -1 = permanent
	// optionCount (30), socketCount (34), isReinforce (38), WaitSecond (39) stay 0:
	// this server carries no options, sockets or reinforcement yet.
	binary.LittleEndian.PutUint32(out[40:], 0) // autoRule: 0 = none
	return out
}

// BuildRemoveItem is 15 bytes and FIXED - it has NO length word:
//
//	cmd(2) invIndex(4) count(4) removeType(1) autoRule(4)
//
// `count` is the NEW TOTAL left in that row; 0 means the row is gone.
func BuildRemoveItem(invIndex, count int32, removeType uint8) []byte {
	out := make([]byte, 15)
	binary.LittleEndian.PutUint16(out[0:], CmdZCRemoveItem)
	binary.LittleEndian.PutUint32(out[2:], uint32(invIndex))
	binary.LittleEndian.PutUint32(out[6:], uint32(count))
	out[10] = removeType
	binary.LittleEndian.PutUint32(out[11:], 0) // autoRule: 0 = none
	return out
}

// InvMirror is the client's inventory KEY SPACE, kept on this side.
//
// The client addresses a bag row by its 1-based POSITION, and when a row empties it
// keeps that position as a HOLE - the key is not handed to the next item. So once
// anything has been taken, positions are NOT the database's row order: a take
// deletes the row, and recomputing an index from `storage` would shift every later
// row by one and address the wrong item. That is the failure the earlier full-list
// refresh was covering for.
//
// One mirror per connection: loaded at world entry in bag order, updated by the
// same changes the client is told about.
type InvMirror struct {
	ids []int64 // the item id at each position; 0 is a hole the client still knows
}

// NewInvMirror builds the mirror from the bag rows, oldest first (bag order).
func NewInvMirror(stacks []item.Stack) *InvMirror {
	m := &InvMirror{ids: make([]int64, 0, len(stacks))}
	for _, st := range stacks {
		m.ids = append(m.ids, st.ItemID)
	}
	return m
}

// Len is how many positions the client knows, holes included.
func (m *InvMirror) Len() int {
	if m == nil {
		return 0
	}
	return len(m.ids)
}

// Index is the 1-based position of an item, or false when the client does not
// know it. A hole answers false: its row is gone.
func (m *InvMirror) Index(itemID int64) (int32, bool) {
	if m == nil {
		return 0, false
	}
	for i, id := range m.ids {
		if id == itemID && id != 0 {
			return int32(i + 1), true
		}
	}
	return 0, false
}

// Append is the 1-based position of an item the client must learn about. A known
// item keeps its position; a new one goes at the END, which is what the client's
// own list does.
func (m *InvMirror) Append(itemID int64) int32 {
	if m == nil {
		// A session that read no bag has no mirror, and a script that gives an
		// item still has to be told a position. Answering 0 would be a position
		// the client cannot use, so the caller sees "no mirror" as item 0 - but
		// it does not crash the process.
		return 0
	}
	if idx, ok := m.Index(itemID); ok {
		return idx
	}
	m.ids = append(m.ids, itemID)
	return int32(len(m.ids))
}

// Clear turns a position into a hole. The position is NOT reused, because the
// client keeps its key and would otherwise show the new item in the old slot.
func (m *InvMirror) Clear(itemID int64) {
	if m == nil {
		return
	}
	for i, id := range m.ids {
		if id == itemID && id != 0 {
			m.ids[i] = 0
			return
		}
	}
}

// IDAt is the item id at a 1-based position, which is the reverse of Index: a
// SALE names its row by POSITION, and the transaction needs the item id the row
// really is. A hole (or a position the client never had) answers false - the
// caller must refuse rather than sell something it cannot name.
func (m *InvMirror) IDAt(pos int32) (int64, bool) {
	if m == nil || pos < 1 || int(pos) > len(m.ids) {
		return 0, false
	}
	id := m.ids[pos-1]
	if id == 0 {
		return 0, false
	}
	return id, true
}

// PacketsFor turns the committed changes into the packets that tell the client.
//
// The mirror is updated as it goes, in the order the transaction applied them, so
// two changes to one row address the same position.
func (m *InvMirror) PacketsFor(changes []item.Change) [][]byte {
	out := make([][]byte, 0, len(changes))
	for _, c := range changes {
		if p := m.PacketFor(c, AddItemPlain, RemoveItemTake); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// PacketFor turns ONE committed change into the packet that announces it, with the
// caller choosing the shop types, and updates the mirror as it does.
//
// WHY THE SPLIT FROM PacketsFor: the shop paths do not send the changes in the
// transaction's order. A buy PAYS first and hands the goods over second, while the
// client must see the goods first and the payment last (python zone_server.py,
// CZ_BUYITEM), so the caller has to decide the order. Which change gets which
// packet TYPE is the other half: a sale's remove must carry REMOVE_ITEM_SELL,
// because that is the one whose handler refreshes the open shop panel.
//
// nil means "no packet": the mirror never had this row, and pointing a remove at a
// guessed position would edit a different item.
func (m *InvMirror) PacketFor(c item.Change, addType, removeType uint8) []byte {
	if c.Give {
		idx := m.Append(c.ItemID)
		return BuildAddItem(c.ClassID, idx, int32(c.Total), addType)
	}
	idx, known := m.Index(c.ItemID)
	if !known {
		// The client never had this row, so there is nothing to decrement. That
		// is a real inconsistency between the mirror and the database - skip the
		// packet rather than point at a row that is not the right item.
		return nil
	}
	pkt := BuildRemoveItem(idx, int32(c.Total), removeType)
	if c.Total == 0 {
		m.Clear(c.ItemID)
	}
	return pkt
}
