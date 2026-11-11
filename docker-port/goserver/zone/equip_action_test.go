package zone

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"testing"

	"geserver/item"
	"geserver/protocol"
)

// The equip/unequip ACTION: which packets a committed change turns into, in which
// order, and what a refused one sends (nothing) and writes (nothing).
//
// A fake store stands in for the database here. It reports the same thing
// PGStore.EquipItem reports - one changed slot and the character's whole worn set -
// so these tests fail on a wrong packet or a wrong order, not on a missing
// database.

// errFakeRefused is what a transaction that rolled back reports.
var errFakeRefused = errors.New("refused by the fake store")

// fakeEquip is the write path: it records the call and answers a canned result.
type fakeEquip struct {
	equipCalls   int
	unequipCalls int
	res          EquipResult
	unequipRes   EquipResult
	err          error

	gotCommander int32
	gotCharacter int32
	gotItem      int64
	gotSpot      int
}

func (f *fakeEquip) equip(_ context.Context, commanderID, characterID int32, itemID int64, spot int) (EquipResult, error) {
	f.equipCalls++
	f.gotCommander, f.gotCharacter, f.gotItem, f.gotSpot = commanderID, characterID, itemID, spot
	return f.res, f.err
}

func (f *fakeEquip) unequip(_ context.Context, commanderID, characterID int32, spot int) (EquipResult, error) {
	f.unequipCalls++
	f.gotCommander, f.gotCharacter, f.gotSpot = commanderID, characterID, spot
	return f.unequipRes, f.err
}

// equipHandler wires the fake into the handler the way main.go wires the store.
func equipHandler(f *fakeEquip) *Handler {
	return &Handler{
		Log:     log.New(io.Discard, "", 0),
		Equip:   f.equip,
		Unequip: f.unequip,
	}
}

// equipped is the worn set a test asks for: one slot filled, the rest empty (the
// builder turns an empty slot into EquipNone).
func equipped(spot int, class uint32) [equipSpotLast]uint32 {
	var e [equipSpotLast]uint32
	e[spot] = class
	return e
}

// frameEquip builds a CZ_EQUIP exactly as the client sends it: 16 bytes,
// cmd(2) seq(4) checksum(4) charIndex(1) invIndex(4) spot(1).
func frameEquip(charIndex byte, invIndex int32, spot byte) protocol.Frame {
	pt := make([]byte, czEquipSize)
	binary.LittleEndian.PutUint16(pt[0:], CmdCZEquip)
	binary.LittleEndian.PutUint32(pt[2:], 1) // seq: nothing reads it
	pt[10] = charIndex
	binary.LittleEndian.PutUint32(pt[11:], uint32(invIndex))
	pt[15] = spot
	return protocol.Frame{Cmd: CmdCZEquip, Size: len(pt), Data: pt}
}

// frameUnequip builds a CZ_UNEQUIP: 12 bytes, cmd(2) seq(4) checksum(4)
// charIndex(1) spot(1).
func frameUnequip(charIndex, spot byte) protocol.Frame {
	pt := make([]byte, czUnequipSize)
	binary.LittleEndian.PutUint16(pt[0:], CmdCZUnequip)
	binary.LittleEndian.PutUint32(pt[2:], 1)
	pt[10] = charIndex
	pt[11] = spot
	return protocol.Frame{Cmd: CmdCZUnequip, Size: len(pt), Data: pt}
}

// TestParseEquipAndUnequipLayout pins the byte offsets: a field moved by one byte
// here equips the wrong item, and a short frame must be refused rather than read
// past.
func TestParseEquipAndUnequipLayout(t *testing.T) {
	req, ok := ParseEquip(frameEquip(2, 351, 5).Data)
	if !ok {
		t.Fatal("a full 16-byte CZ_EQUIP was refused")
	}
	if req.CharIndex != 2 {
		t.Fatalf("charIndex = %d, want the byte at +10 (2)", req.CharIndex)
	}
	if req.InvIndex != 351 {
		t.Fatalf("invIndex = %d, want the DWORD at +11 (351)", req.InvIndex)
	}
	if req.Spot != 5 {
		t.Fatalf("spot = %d, want the byte at +15 (5)", req.Spot)
	}
	if _, ok := ParseEquip(make([]byte, czEquipSize-1)); ok {
		t.Fatal("a 15-byte CZ_EQUIP was accepted - the parser read past the buffer")
	}

	un, ok := ParseUnequip(frameUnequip(1, 4).Data)
	if !ok {
		t.Fatal("a full 12-byte CZ_UNEQUIP was refused")
	}
	if un.CharIndex != 1 {
		t.Fatalf("charIndex = %d, want the byte at +10 (1)", un.CharIndex)
	}
	if un.Spot != 4 {
		t.Fatalf("spot = %d, want the byte at +11 (4)", un.Spot)
	}
	if _, ok := ParseUnequip(make([]byte, czUnequipSize-1)); ok {
		t.Fatal("an 11-byte CZ_UNEQUIP was accepted - the parser read past the buffer")
	}
}

// TestEquipSendsRemoveItemThenUpdateEquipment is the whole equip answer: the worn
// row leaves the bag with 3608 (count 0, REMOVE_ITEM_EQUIP), and the LAST packet is
// the 3616 for the character, carrying the class now worn.
func TestEquipSendsRemoveItemThenUpdateEquipment(t *testing.T) {
	f := &fakeEquip{res: EquipResult{
		Spot: 5, ClassID: 7201, Removed: 9001, Equip: equipped(5, 7201),
	}}
	h := equipHandler(f)
	const handle = 1792
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: handle, Name: "Tester"}},
		Inv: NewInvMirror([]item.Stack{
			{ItemID: 9001, ClassID: 7201, Count: 1},
		}),
	}

	out, err := h.Reply(context.Background(), sess, frameEquip(0, 1, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if f.equipCalls != 1 {
		t.Fatalf("%d store call(s), want exactly 1", f.equipCalls)
	}
	if f.gotItem != 9001 || f.gotSpot != 5 || f.gotCharacter != handle ||
		f.gotCommander != 25 {
		t.Fatalf("store called with commander %d char %d item %d spot %d",
			f.gotCommander, f.gotCharacter, f.gotItem, f.gotSpot)
	}
	if len(out) != 7 {
		t.Fatalf("%d packet(s), want 7 (3608, 3616, 3546, 3871, 3859, 3861, then "+
			"the re-sent 3684)", len(out))
	}

	// First: the item leaves the bag.
	if cmd := cmdOf(out[0]); cmd != CmdZCRemoveItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_REMOVEITEM (3608)", cmd)
	}
	if len(out[0]) != 15 {
		t.Fatalf("the 3608 is %d bytes, want the fixed 15", len(out[0]))
	}
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != 1 {
		t.Fatalf("the 3608 names bag index %d, want the item's mirror position 1", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 0 {
		t.Fatalf("count = %d, want 0 (the row is gone)", got)
	}
	if out[0][10] != RemoveItemEquip {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_EQUIP (5)", out[0][10])
	}

	// Second: the whole worn set.
	last := out[1]
	if cmd := cmdOf(last); cmd != CmdZCUpdateEquipment {
		t.Fatalf("the last packet is cmd %d, want ZC_UPDATE_EQUIPMENT (3616)", cmd)
	}
	if got := binary.LittleEndian.Uint32(last[4:]); got != handle {
		t.Fatalf("handle = %d, want the CHARACTER id %d (the client applies it only "+
			"for its own pc)", got, handle)
	}
	if got := binary.LittleEndian.Uint32(last[zcUpdateEquipmentItems+5*zcUpdateEquipItemSize:]); got != 7201 {
		t.Fatalf("slot 5 = %d, want the class now worn (7201)", got)
	}
	// The session's copy follows, so a later spawn from this session carries it.
	if sess.Team[0].Equip[5] != 7201 {
		t.Fatalf("session team slot 5 = %d, want 7201", sess.Team[0].Equip[5])
	}
	// And the mirror no longer knows the item: its position is a hole.
	if _, known := sess.Inv.IDAt(1); known {
		t.Fatal("the mirror still has the equipped row at position 1")
	}
}

// TestEquipSwapPutsTheOldItemBackInTheBag: with something already worn, the SWAP
// half comes FIRST - a 3607 for the displaced item at a NEW bag position - and the
// 3608 still names the OLD position of the item that went on (hole keys are not
// reused).
func TestEquipSwapPutsTheOldItemBackInTheBag(t *testing.T) {
	f := &fakeEquip{res: EquipResult{
		Spot: 5, ClassID: 7202, Removed: 9002,
		Returned: &item.Stack{ItemID: 9001, ClassID: 7201, Count: 1},
		Equip:    equipped(5, 7202),
	}}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester"}},
		// The sword that is being equipped sits at position 1. The one coming off
		// (9001) is NOT in the bag, which is the real state: it was worn.
		Inv: NewInvMirror([]item.Stack{{ItemID: 9002, ClassID: 7202, Count: 1}}),
	}

	out, err := h.Reply(context.Background(), sess, frameEquip(0, 1, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 8 {
		t.Fatalf("%d packet(s), want 8 (3607, 3608, 3616, 3546, 3871, 3859, 3861, "+
			"then the re-sent 3684)", len(out))
	}

	if cmd := cmdOf(out[0]); cmd != CmdZCAddItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_ADDITEM (3607) for the displaced item", cmd)
	}
	if len(out[0]) != 44 {
		t.Fatalf("the 3607 is %d bytes, want 44", len(out[0]))
	}
	if out[0][25] != AddItemUnequip {
		t.Fatalf("addType = %d, want ADD_ITEM_UNEQUIP (4)", out[0][25])
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != 7201 {
		t.Fatalf("the 3607 carries class %d, want the displaced 7201", got)
	}
	pos := binary.LittleEndian.Uint32(out[0][8:])
	if pos != 2 {
		t.Fatalf("the displaced item landed at position %d, want the appended 2", pos)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 1 {
		t.Fatalf("the 3607 says count %d, want 1 (the item's own count)", got)
	}

	if cmd := cmdOf(out[1]); cmd != CmdZCRemoveItem {
		t.Fatalf("packet 1 cmd = %d, want ZC_REMOVEITEM (3608)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[1][2:]); got != 1 {
		t.Fatalf("the 3608 names bag index %d, want the OLD position 1 - the hole "+
			"the returned item's 3607 just used is not the same key", got)
	}
	if out[1][10] != RemoveItemEquip {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_EQUIP (5)", out[1][10])
	}
	if cmd := cmdOf(out[2]); cmd != CmdZCUpdateEquipment {
		t.Fatalf("packet 2 cmd = %d, want ZC_UPDATE_EQUIPMENT (3616)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[2][zcUpdateEquipmentItems+5*zcUpdateEquipItemSize:]); got != 7202 {
		t.Fatalf("slot 5 = %d, want the new class 7202", got)
	}
	// The mirror grew by the returned item only: position 1 is now a hole.
	if sess.Inv.Len() != 2 {
		t.Fatalf("the mirror holds %d position(s), want 2", sess.Inv.Len())
	}
	if _, known := sess.Inv.IDAt(1); known {
		t.Fatal("position 1 still names an item; the equipped row must be a hole")
	}
	if id, known := sess.Inv.IDAt(2); !known || id != 9001 {
		t.Fatalf("position 2 = %d (known %v), want the returned item 9001", id, known)
	}
}

// TestUnequipSendsAddItemThenUpdateEquipment: an unequip puts the item BACK in the
// bag (3607, ADD_ITEM_UNEQUIP, at the appended position), sends no 3608 at all,
// and the 3616 no longer carries the class.
func TestUnequipSendsAddItemThenUpdateEquipment(t *testing.T) {
	f := &fakeEquip{unequipRes: EquipResult{
		Spot: 5, ClassID: 7201,
		Returned: &item.Stack{ItemID: 9001, ClassID: 7201, Count: 1},
		Equip:    equipped(5, 0),
	}}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester"}},
		// The bag holds one unrelated row, so "appended" is position 2 and not 1.
		Inv: NewInvMirror([]item.Stack{{ItemID: 8001, ClassID: 14000, Count: 1}}),
	}

	out, err := h.Reply(context.Background(), sess, frameUnequip(0, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if f.unequipCalls != 1 {
		t.Fatalf("%d unequip call(s), want exactly 1", f.unequipCalls)
	}
	if len(out) != 7 {
		t.Fatalf("%d packet(s), want 7 (3607, 3616, 3546, then the three lists, "+
			"then the re-sent 3684): %d", len(out), len(out))
	}
	for _, p := range out {
		if cmdOf(p) == CmdZCRemoveItem {
			t.Fatal("an unequip sent a 3608 - nothing leaves the bag on an unequip")
		}
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCAddItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_ADDITEM (3607)", cmd)
	}
	if out[0][25] != AddItemUnequip {
		t.Fatalf("addType = %d, want ADD_ITEM_UNEQUIP (4)", out[0][25])
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 2 {
		t.Fatalf("the returned item landed at position %d, want the appended 2", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != 7201 {
		t.Fatalf("the 3607 carries class %d, want 7201", got)
	}

	last := out[1]
	if cmd := cmdOf(last); cmd != CmdZCUpdateEquipment {
		t.Fatalf("the last packet is cmd %d, want ZC_UPDATE_EQUIPMENT (3616)", cmd)
	}
	got := binary.LittleEndian.Uint32(last[zcUpdateEquipmentItems+5*zcUpdateEquipItemSize:])
	if got == 7201 {
		t.Fatal("slot 5 still carries the unequipped class 7201")
	}
	if got != EquipNone[5] {
		t.Fatalf("slot 5 = %d, want the client's own None id %d (never 0)",
			got, EquipNone[5])
	}
	if sess.Team[0].Equip[5] != 0 {
		t.Fatalf("session team slot 5 = %d, want 0 (nothing worn)",
			sess.Team[0].Equip[5])
	}
}

// TestEquipRefusalsSendNothingAndWriteNothing: every bad request is answered with
// NOTHING and reaches the store not at all. A reply here would tell the client the
// item moved when the database says it did not.
func TestEquipRefusalsSendNothingAndWriteNothing(t *testing.T) {
	// goodSession is a session that WOULD succeed, so each case differs in exactly
	// the one thing it refuses.
	goodSession := func() *Session {
		return &Session{
			CommanderID: 25,
			Team:        []Character{{Handle: 1792, Name: "Tester"}},
			Inv:         NewInvMirror([]item.Stack{{ItemID: 9001, ClassID: 7201, Count: 1}}),
		}
	}
	// holedSession has a HOLE at position 2: the client knows the position, but its
	// row is gone, so naming it must match no item.
	holedSession := func() *Session {
		s := goodSession()
		s.Inv = NewInvMirror([]item.Stack{
			{ItemID: 9001, ClassID: 7201, Count: 1},
			{ItemID: 9002, ClassID: 7202, Count: 1},
		})
		s.Inv.Clear(9002)
		return s
	}
	noCommander := func() *Session { s := goodSession(); s.CommanderID = 0; return s }
	noMirror := func() *Session { s := goodSession(); s.Inv = nil; return s }

	cases := []struct {
		name     string
		sess     func() *Session
		frame    protocol.Frame
		nilStore bool
	}{
		{"short frame", goodSession,
			protocol.Frame{Cmd: CmdCZEquip, Size: 15, Data: make([]byte, 15)}, false},
		{"no commander", noCommander, frameEquip(0, 1, 5), false},
		{"character index outside the team", goodSession, frameEquip(3, 1, 5), false},
		{"spot outside the block", goodSession, frameEquip(0, 1, equipSpotLast), false},
		{"no inventory mirror", noMirror, frameEquip(0, 1, 5), false},
		{"bag index is a hole", holedSession, frameEquip(0, 2, 5), false},
		{"bag index past the end", goodSession, frameEquip(0, 9, 5), false},
		{"no store function", goodSession, frameEquip(0, 1, 5), true},
	}
	for _, c := range cases {
		f := &fakeEquip{res: EquipResult{Spot: 5, ClassID: 7201, Removed: 9001}}
		h := equipHandler(f)
		if c.nilStore {
			h.Equip = nil
		}
		out, err := h.Reply(context.Background(), c.sess(), c.frame)
		if err != nil {
			t.Fatalf("%s: Reply returned an error: %v", c.name, err)
		}
		if len(out) != 0 {
			t.Fatalf("%s: %d packet(s) sent, want none", c.name, len(out))
		}
		if f.equipCalls != 0 {
			t.Fatalf("%s: the store was called %d time(s), want 0", c.name, f.equipCalls)
		}
	}
}

// TestEquipRefusalAfterAStoreErrorSendsNothing: a transaction that rolled back
// leaves both tables as they were, so the client must not be told anything either.
func TestEquipRefusalAfterAStoreErrorSendsNothing(t *testing.T) {
	f := &fakeEquip{err: errFakeRefused}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester"}},
		Inv:         NewInvMirror([]item.Stack{{ItemID: 9001, ClassID: 7201, Count: 1}}),
	}
	out, err := h.Reply(context.Background(), sess, frameEquip(0, 1, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent after a failed transaction, want none", len(out))
	}
	// The bag is untouched: the mirror still has the row where it was.
	if id, known := sess.Inv.IDAt(1); !known || id != 9001 {
		t.Fatalf("position 1 = %d (known %v) after a failed equip, want 9001", id, known)
	}
	if sess.Team[0].Equip[5] != 0 {
		t.Fatalf("the session thinks it wears %d after a failed equip",
			sess.Team[0].Equip[5])
	}
}

// TestUpdateEquipmentUsesEquipNoneForEmptySlots: a zero slot must go out as the
// client's own None class id. Zero made the client dereference a null item
// property and crash - the reason EquipNone exists at all.
func TestUpdateEquipmentUsesEquipNoneForEmptySlots(t *testing.T) {
	equip := equipped(5, 7201)
	equip[0] = 17000
	pkt := BuildUpdateEquipment(1792, equip)

	if len(pkt) != zcUpdateEquipmentSize {
		t.Fatalf("packet is %d bytes, want %d", len(pkt), zcUpdateEquipmentSize)
	}
	for spot := 0; spot < equipSpotLast; spot++ {
		got := binary.LittleEndian.Uint32(pkt[zcUpdateEquipmentItems+spot*zcUpdateEquipItemSize:])
		if got == 0 {
			t.Fatalf("slot %d is 0 - the client would crash on this", spot)
		}
		want := EquipNone[spot]
		if spot == 5 {
			want = 7201
		} else if spot == 0 {
			want = 17000
		}
		if got != want {
			t.Fatalf("slot %d = %d, want %d", spot, got, want)
		}
	}
}

// TestEquipLengthWordIsTheWholePacket: the length word at +2 counts the WHOLE
// packet, not the bytes after the header. Asserting only the size would agree with
// a wrong length word, which is the documented trap.
func TestEquipLengthWordIsTheWholePacket(t *testing.T) {
	pkt := BuildUpdateEquipment(1792, equipped(5, 7201))
	lengthWord := binary.LittleEndian.Uint16(pkt[2:])
	if int(lengthWord) != len(pkt) {
		t.Fatalf("length word = %d, len(packet) = %d - the client sizes the packet "+
			"from the word", lengthWord, len(pkt))
	}
	if len(pkt) != 264 {
		t.Fatalf("packet is %d bytes, want the layout's 264 (8 + 21*12 + 4)", len(pkt))
	}
	if cmd := cmdOf(pkt); cmd != CmdZCUpdateEquipment {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCUpdateEquipment)
	}
}

// TestUpdateEquipmentSlotsAreTwelveBytesApart pins the fix for the crash of
// 2026-10-28 19:20:42 (ge.exe+0x3AA80B, `CMP [EDI+0x1dc]` with EDI=0, slot byte
// offset ESI=0x54): ONE EQUIPMENT SLOT IS 12 BYTES IN THE CLIENT'S OWN ARRAY.
//
// The old builder packed 21 class ids 4 bytes apart and called the 21 bytes after
// them a reinforcement block. The client copies 3616's equipment straight over an
// array it walks 12 bytes at a time, so those packed ids landed on slots 0,1,2...
// at byte 0, 12, 24 - which happened to still be real "None" ids - and slot 7 (byte
// 84) landed on the first reinforcement byte, a 0. Zero has no item property and
// the client dereferences it anyway.
//
// This test asserts the shape, not a number in a comment: the class id of slot n
// sits at 8+n*12, the 8 bytes after it are zero, the last slot ends at 260, and the
// option count is the last 4 bytes.
func TestUpdateEquipmentSlotsAreTwelveBytesApart(t *testing.T) {
	pkt := BuildUpdateEquipment(1792, equipped(5, 7201))

	if len(pkt) != 264 {
		t.Fatalf("packet is %d bytes, want 264 (4 header + 4 handle + 21*12 + 4)", len(pkt))
	}
	for spot := 0; spot < equipSpotLast; spot++ {
		off := 8 + spot*zcUpdateEquipItemSize
		got := binary.LittleEndian.Uint32(pkt[off:])
		want := EquipNone[spot]
		if spot == 5 {
			want = 7201
		}
		if got != want {
			t.Fatalf("slot %d at byte %d = %d, want %d", spot, off, got, want)
		}
		// The two words after the class id: the client's own initializer zeroes
		// the one at +4, and this server has no reinforcement or options to put
		// there. A non-zero here would be a field we cannot name.
		for _, b := range pkt[off+4 : off+12] {
			if b != 0 {
				t.Fatalf("slot %d has %#x in its second word (bytes %d..%d), want 0",
					spot, pkt[off+4:off+12], off+4, off+11)
			}
		}
	}
	// Slot 7 starts at 8+84 = 92, the byte the client died on. It must be the
	// "None ring" class id, never a zero.
	if got := binary.LittleEndian.Uint32(pkt[92:]); got != EquipNone[7] {
		t.Fatalf("byte 92 (slot 7) = %d, want the None id %d - this is the byte "+
			"that crashed the client", got, EquipNone[7])
	}
	if got := binary.LittleEndian.Uint32(pkt[zcUpdateEquipmentOption:]); got != 0 {
		t.Fatalf("option count at +%d = %d, want 0", zcUpdateEquipmentOption, got)
	}
}

// TestEquipResendCarriesTheNewGear is the half of R65 that can be proven without
// a client: the 3684 an equip change sends must describe the set the change just
// wrote, not the one the character wore at zone entry.
//
// The assertion is one number with one source: class 4000 (LeatherArmor1Sol) has
// DEF 44 in data\item_stats.json, and SCR_Get_DEF is the sum of the WORN items'
// DEF columns. Before the change the body slot is empty, so DEF is 0; if the
// packet were built from the stale team copy it would still say 0.
func TestEquipResendCarriesTheNewGear(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)

	f := &fakeEquip{res: EquipResult{
		Spot: spotBody, ClassID: 4000, Removed: 9001, Equip: equipped(spotBody, 4000),
	}}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester", Job: 1}},
		Inv:         NewInvMirror([]item.Stack{{ItemID: 9001, ClassID: 4000, Count: 1}}),
	}

	out, err := h.Reply(context.Background(), sess, frameEquip(0, 1, byte(spotBody)))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	last := out[len(out)-1]
	if cmd := cmdOf(last); cmd != CmdZCUpdateAllStatus {
		t.Fatalf("the last packet is %d, want the re-sent 3684", cmd)
	}
	if got := readBattleWord(last, battleDEF); got != 44 {
		t.Fatalf("the re-sent 3684's DEF = %d, want 44 (class 4000's own DEF "+
			"column): the block must be built AFTER the worn set was updated", got)
	}
	// And the slot really moved in the session copy the packet was built from.
	if got := sess.Team[0].Equip[spotBody]; got != 4000 {
		t.Fatalf("session body slot = %d, want 4000", got)
	}
}
