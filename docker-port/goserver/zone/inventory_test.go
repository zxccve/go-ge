package zone

import (
	"encoding/binary"
	"testing"

	"geserver/item"
)

// TestAddItemLayout pins the 44-byte packet against the CAPTURE's layout, not
// against the Python builder: `CNormalNet::AddItem` reads expireTime at +0x1a (26)
// and WaitSecond at +0x27 (39), and those two offsets only line up when
// isReinforce and WaitSecond are SINGLE bytes. The Python oracle packs them as
// 4-byte ints, which is why its packet is 50 bytes while its length word says 44.
func TestAddItemLayout(t *testing.T) {
	pkt := BuildAddItem(21613, 3, 128, AddItemPlain)

	if len(pkt) != 44 {
		t.Fatalf("%d bytes, want 44", len(pkt))
	}
	if cmd := binary.LittleEndian.Uint16(pkt[0:]); cmd != CmdZCAddItem {
		t.Fatalf("command = %d, want %d", cmd, CmdZCAddItem)
	}
	if size := binary.LittleEndian.Uint16(pkt[2:]); int(size) != len(pkt) {
		t.Fatalf("length word = %d, but the packet is %d bytes", size, len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[4:]); got != 21613 {
		t.Fatalf("ItemType = %d, want 21613", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[8:]); got != 3 {
		t.Fatalf("InvIndex = %d, want 3", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[12:]); got != 128 {
		t.Fatalf("Count = %d, want the NEW TOTAL 128", got)
	}
	if pkt[16] != 0 {
		t.Fatalf("PType = %d, want 0", pkt[16])
	}
	// The handler never reads the GUID, so it must be 0 - not a guess.
	if got := binary.LittleEndian.Uint64(pkt[17:]); got != 0 {
		t.Fatalf("GUID = %d, want 0", got)
	}
	// The RAW value 0 is asserted, not the constant: "every grant is ADD_ITEM" is a
	// rule about the game's enum, so changing the constant must break this test.
	if pkt[25] != 0 {
		t.Fatalf("addType = %d, want 0 (ADD_ITEM): every GRANT prints 'obtained', "+
			"and only a real purchase prints 'purchased'", pkt[25])
	}
	// expireTime at +0x1a: -1 means permanent. Anything else tells the client the
	// item expires, and it draws a timer under it.
	if got := int32(binary.LittleEndian.Uint32(pkt[26:])); got != -1 {
		t.Fatalf("expireTime = %d, want -1", got)
	}
	if pkt[38] != 0 || pkt[39] != 0 {
		t.Fatalf("isReinforce=%d WaitSecond=%d, want 0/0", pkt[38], pkt[39])
	}
	if got := binary.LittleEndian.Uint32(pkt[40:]); got != 0 {
		t.Fatalf("autoRule = %d, want 0", got)
	}
}

// TestRemoveItemLayout: 15 bytes and FIXED, so it has NO length word. Reading a
// length there takes the invIndex as a size and hangs the stream.
func TestRemoveItemLayout(t *testing.T) {
	pkt := BuildRemoveItem(7, 0, RemoveItemTake)

	if len(pkt) != 15 {
		t.Fatalf("%d bytes, want 15", len(pkt))
	}
	if cmd := binary.LittleEndian.Uint16(pkt[0:]); cmd != CmdZCRemoveItem {
		t.Fatalf("command = %d, want %d", cmd, CmdZCRemoveItem)
	}
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != 7 {
		t.Fatalf("invIndex = %d, want 7", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[6:]); got != 0 {
		t.Fatalf("count = %d, want the NEW TOTAL 0", got)
	}
	if pkt[10] != RemoveItemTake {
		t.Fatalf("removeType = %d, want %d", pkt[10], RemoveItemTake)
	}
}

// TestInvMirrorKeepsHoles is the reason the mirror exists: a take deletes the
// database row, so a position recomputed from `storage` would point at the WRONG
// item for everything after it.
func TestInvMirrorKeepsHoles(t *testing.T) {
	m := NewInvMirror([]item.Stack{
		{ItemID: 101, ClassID: 30000, Count: 500},
		{ItemID: 102, ClassID: 21613, Count: 5},
		{ItemID: 103, ClassID: 1234, Count: 1},
	})
	if m.Len() != 3 {
		t.Fatalf("Len = %d, want 3", m.Len())
	}
	if idx, ok := m.Index(102); !ok || idx != 2 {
		t.Fatalf("index of 102 = %d, %v; want 2", idx, ok)
	}

	// Empty the first row: its position becomes a hole, and everything after it
	// KEEPS the position the client knows.
	m.Clear(101)
	if _, ok := m.Index(101); ok {
		t.Fatal("a hole still answers with a position")
	}
	if idx, _ := m.Index(102); idx != 2 {
		t.Fatalf("102 moved to %d - the client would decrement the wrong row", idx)
	}

	// A new item goes at the end, never into the hole.
	if idx := m.Append(104); idx != 4 {
		t.Fatalf("a new item landed at %d, want 4 (holes are not reused)", idx)
	}
	if idx := m.Append(102); idx != 2 {
		t.Fatalf("a known item moved to %d, want its own position 2", idx)
	}
}

// TestPacketsForOneChangeEach: a give is 3607 with the NEW TOTAL, a take that
// empties a row is 3608 with total 0, and the mirror is updated in the same pass.
func TestPacketsForOneChangeEach(t *testing.T) {
	m := NewInvMirror([]item.Stack{{ItemID: 101, ClassID: 30000, Count: 500}})

	pkts := m.PacketsFor([]item.Change{
		{Give: true, ClassID: 21613, ItemID: 201, Total: 5},
		{Give: false, ClassID: 30000, ItemID: 101, Total: 0},
	})
	if len(pkts) != 2 {
		t.Fatalf("%d packet(s), want 2", len(pkts))
	}
	if cmd := binary.LittleEndian.Uint16(pkts[0]); cmd != CmdZCAddItem {
		t.Fatalf("first packet is %d, want ZC_ADDITEM", cmd)
	}
	if idx := binary.LittleEndian.Uint32(pkts[0][8:]); idx != 2 {
		t.Fatalf("the new item was announced at position %d, want 2 (appended)", idx)
	}
	if cmd := binary.LittleEndian.Uint16(pkts[1]); cmd != CmdZCRemoveItem {
		t.Fatalf("second packet is %d, want ZC_REMOVEITEM", cmd)
	}
	if idx := binary.LittleEndian.Uint32(pkts[1][2:]); idx != 1 {
		t.Fatalf("the emptied row was announced at position %d, want 1", idx)
	}
	// The hole is remembered, so a later add does not take slot 1.
	if idx := m.Append(202); idx != 3 {
		t.Fatalf("the next item landed at %d, want 3", idx)
	}
}

// TestPacketsForSkipsAnUnknownRow: if the mirror never had the row there is
// nothing to decrement, and pointing at a guessed position would edit the wrong
// item - so no packet is better than a wrong one.
func TestPacketsForSkipsAnUnknownRow(t *testing.T) {
	m := NewInvMirror(nil)
	pkts := m.PacketsFor([]item.Change{
		{Give: false, ClassID: 21613, ItemID: 999, Total: 0},
	})
	if len(pkts) != 0 {
		t.Fatalf("%d packet(s) for a row the client never had", len(pkts))
	}
}
