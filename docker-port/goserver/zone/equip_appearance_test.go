package zone

import (
	"context"
	"encoding/binary"
	"testing"

	"geserver/item"
)

// THE APPEARANCE ANSWER (R59c): after an equip or an unequip the client must be
// told to REDRAW THE BODY, not only to update the numbers.
//
// The bug this pins, seen in game 2026-10-28 19:48: the armour read "equipped" in
// the compare window (3616 does that) and was invisible on the character, because
// 3616's handler never touches the model (GENormalNet_Action.cpp:1402). 3546 is
// the packet whose handler calls ChangeAppearance (same file:125-136), and the
// reference sends it through BroadcastUpdateAppearance on every worn-set change
// (PlayerChar.cpp:458-467).
//
// Every test below is written so it FAILS on the code before this lane: no 3546
// was sent at all, and no equip info list either.

// cmdsOf answers the command id of every packet, in order - the sequence is what
// "the order is right" means, so it is compared as a sequence and not a set.
func cmdsOf(pkts [][]byte) []uint16 {
	out := make([]uint16, len(pkts))
	for i, p := range pkts {
		out[i] = cmdOf(p)
	}
	return out
}

// wantCmds fails with the two sequences side by side, which is the only readable
// way to see a wrong order.
func wantCmds(t *testing.T, got []uint16, want ...uint16) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("packets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("packets = %v, want %v (position %d differs)", got, want, i)
		}
	}
}

// TestEquipSendsAppearanceAndListsInOrder is the lane's order, by command id:
// 3608 (the bag row goes) -> 3616 (the worn set) -> 3546 (the body) -> the three
// equip info lists.
//
// WHY EACH POSITION:
//   - 3608 first: the item must leave the bag before anything claims it is worn;
//     the reference does the same (TxEquipItem.cpp:224) and the row is already
//     gone from the database by then.
//   - 3616 second: the NUMBERS. It is what the client's compare window reads, and
//     it is built from the worn set the transaction read back.
//   - 3546 third: the BODY, redrawn from the same team copy the 3616 came from, so
//     the model and the numbers can never describe different sets.
//   - the three lists last: they are per-item detail (reinforcement, sockets,
//     properties), and this server has none to report - they go out empty, as the
//     reference sends them, so the client's equip windows are filled and not left
//     waiting.
func TestEquipSendsAppearanceAndListsInOrder(t *testing.T) {
	f := &fakeEquip{res: EquipResult{
		Spot: 5, ClassID: 7201, Removed: 9001, Equip: equipped(5, 7201),
	}}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester", Job: 1}},
		Inv:         NewInvMirror([]item.Stack{{ItemID: 9001, ClassID: 7201, Count: 1}}),
	}

	out, err := h.Reply(context.Background(), sess, frameEquip(0, 1, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wantCmds(t, cmdsOf(out),
		CmdZCRemoveItem, CmdZCUpdateEquipment, CmdZCUpdatedPCAppearance,
		CmdZCEquipReinforceInfoList, CmdZCEquipSocketInfoList, CmdZCEquipAdditionPropList,
		CmdZCUpdateAllStatus)

	// The 3684 comes LAST, and it is the real 125-byte packet, not a placeholder:
	// the reference server ends its equipment path with SendCurStance, which sends
	// ZC_CHANGE_STANCE and then SendAllStatus (PlayerChar.cpp:1043). Without it the
	// panel keeps the ATK/DEF/A.R. it drew at entry.
	last := out[len(out)-1]
	if cmd := cmdOf(last); cmd != CmdZCUpdateAllStatus {
		t.Fatalf("the last packet is %d, want the re-sent 3684", cmd)
	}
	if len(last) != UpdateAllStatusSize {
		t.Fatalf("the re-sent 3684 is %d bytes, want %d", len(last), UpdateAllStatusSize)
	}
	if got, want := binary.LittleEndian.Uint32(last[2:]), uint32(1792); got != want {
		t.Fatalf("the re-sent 3684 carries handle %d, want the character's %d", got, want)
	}
}

// TestUnequipSendsAppearanceWithTheSlotBackToNone: the mirror image. The item goes
// back to the bag (3607, at its BAG index), the worn set loses it (3616), the body
// is redrawn (3546) - and the slot that held the item now carries the client's own
// None class id, never 0.
func TestUnequipSendsAppearanceWithTheSlotBackToNone(t *testing.T) {
	f := &fakeEquip{unequipRes: EquipResult{
		Spot: 5, ClassID: 7201,
		Returned: &item.Stack{ItemID: 9001, ClassID: 7201, Count: 1},
		Equip:    equipped(5, 0),
	}}
	h := equipHandler(f)
	sess := &Session{
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "Tester", Job: 1}},
		Inv:         NewInvMirror([]item.Stack{{ItemID: 8001, ClassID: 14000, Count: 1}}),
	}

	out, err := h.Reply(context.Background(), sess, frameUnequip(0, 5))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wantCmds(t, cmdsOf(out),
		CmdZCAddItem, CmdZCUpdateEquipment, CmdZCUpdatedPCAppearance,
		CmdZCEquipReinforceInfoList, CmdZCEquipSocketInfoList, CmdZCEquipAdditionPropList,
		CmdZCUpdateAllStatus)

	app := out[2]
	if got := binary.LittleEndian.Uint32(app[upcAppearance+apEquipment+5*equipEntrySize:]); got != EquipNone[5] {
		t.Fatalf("the 3546's slot 5 = %d, want the client's None id %d (never 0)",
			got, EquipNone[5])
	}
}

// TestUpdatedPCAppearanceLayout pins the 401-byte packet field by field:
//
//	+0   cmd(2) = 3546        +2   handle(4)      +6   APPEARANCE_PC(395)
//
// THERE IS NO LENGTH WORD, and the handle assertion is what proves it: the struct
// is PKS_DEFAULT + handle + appearance (geZCPacket.h:1365), and PKS_DEFAULT is
// `WORD command` only (gePacketBase.h:40). A builder that wrote a length word at
// +2 would put 0x0191 (401) in the low half of the handle, so this test checks the
// handle is intact AND is not the size value.
func TestUpdatedPCAppearanceLayout(t *testing.T) {
	c := Character{Handle: 1792, Name: "Tester", Job: 1}
	c.Equip[1] = 15001 // the armour from the game run of 2026-10-28

	pkt := BuildUpdatedPCAppearance(c.Handle, c)

	if len(pkt) != 401 {
		t.Fatalf("packet is %d bytes, want the command table's fixed 401", len(pkt))
	}
	if cmd := cmdOf(pkt); cmd != CmdZCUpdatedPCAppearance {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCUpdatedPCAppearance)
	}
	handle := binary.LittleEndian.Uint32(pkt[upcHandle:])
	if handle != c.Handle {
		t.Fatalf("handle at +2 = %d, want the character's %d (there is no length word: "+
			"PKS_DEFAULT carries only the command)", handle, c.Handle)
	}
	if handle == UpdatedPCAppearanceSize {
		t.Fatalf("the handle reads %d, which is this packet's size - a length word was "+
			"written into the handle field", handle)
	}

	// The slot that changed carries the worn class id...
	if got := binary.LittleEndian.Uint32(pkt[upcAppearance+apEquipment+1*equipEntrySize:]); got != 15001 {
		t.Fatalf("appearance slot 1 = %d, want the worn armour 15001", got)
	}
	// ...and every untouched slot carries the client's own None id, never 0.
	for spot := 0; spot < equipSpotLast; spot++ {
		got := binary.LittleEndian.Uint32(pkt[upcAppearance+apEquipment+spot*equipEntrySize:])
		want := EquipNone[spot]
		if spot == 1 {
			want = 15001
		}
		if got == 0 {
			t.Fatalf("appearance slot %d is 0 - the client dereferences it and crashes", spot)
		}
		if got != want {
			t.Fatalf("appearance slot %d = %d, want %d", spot, got, want)
		}
	}
}

// TestAppearanceBlockIsTheSpawnBlock is the anti-drift test the lane asked for: the
// appearance 3546 carries and the appearance a spawn carries are the SAME BYTES for
// the same character, because both come from appearanceBlock -> BuildStartInfo.
//
// A second, hand-built copy would pass every other test here and still let a
// remote player's model and the wearer's own model describe different clothes.
func TestAppearanceBlockIsTheSpawnBlock(t *testing.T) {
	c := Character{Handle: 40, Name: "Fighter", Job: 1, Stance: 30110, Level: 20}
	c.Equip[4] = 7215 // shield
	c.Equip[5] = 7201 // sword

	spawn := BuildSpawnStandingPC(c.Handle, c, [3]int32{10, 20, 30}, DefaultPosture)
	app := BuildUpdatedPCAppearance(c.Handle, c)

	spawnBlock := spawn[spAppearance : spAppearance+spAppearanceLen]
	appBlock := app[upcAppearance : upcAppearance+AppearanceLen]
	if len(spawnBlock) != AppearanceLen || len(appBlock) != AppearanceLen {
		t.Fatalf("block sizes: spawn %d, appearance %d, want %d",
			len(spawnBlock), len(appBlock), AppearanceLen)
	}
	for i := range spawnBlock {
		if spawnBlock[i] != appBlock[i] {
			t.Fatalf("the two appearance blocks differ at byte %d: spawn %#x, "+
				"appearance %#x", i, spawnBlock[i], appBlock[i])
		}
	}

	// The block ends exactly where the START_INFO record's position begins - the
	// reason 395 is the right length and not a number someone typed.
	if siAppearance+AppearanceLen != siInitPos {
		t.Fatalf("the appearance block is %d..%d and the position starts at %d - the "+
			"block must end where the position begins",
			siAppearance, siAppearance+AppearanceLen, siInitPos)
	}
}

// TestEquipAppearanceReachesOtherPlayers is the "broadcast" half: a second player
// in the zone must see the armour too, so the 3546 is forwarded to every OTHER
// session - and the roster keeps the new worn set, or a player who joins later is
// spawned in the old one.
func TestEquipAppearanceReachesOtherPlayers(t *testing.T) {
	f := &fakeEquip{res: EquipResult{
		Spot: 1, ClassID: 15001, Removed: 100017, Equip: equipped(1, 15001),
	}}
	h := equipHandler(f)

	me := newPlayer(1792, "Tester", [3]int32{100, 200, 5})
	other := newPlayer(4096, "Friend", [3]int32{300, 400, 6})
	me.sess.CommanderID = 25
	me.sess.Inv = NewInvMirror([]item.Stack{{ItemID: 100017, ClassID: 15001, Count: 1}})
	h.Players.Add(me.sess)
	h.Players.Add(other.sess)

	if _, err := h.Reply(context.Background(), me.sess, frameEquip(0, 1, 1)); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// What the other client received: exactly one packet, the 3546.
	got := other.conn.sent()
	if len(got) != 1 {
		t.Fatalf("the other player received %d packet(s), want 1 (the appearance)", len(got))
	}
	pkt := got[0]
	if cmd := cmdOf(pkt); cmd != CmdZCUpdatedPCAppearance {
		t.Fatalf("the other player received cmd %d, want 3546", cmd)
	}
	if len(pkt) != UpdatedPCAppearanceSize {
		t.Fatalf("the forwarded packet is %d bytes, want %d", len(pkt), UpdatedPCAppearanceSize)
	}
	if handle := binary.LittleEndian.Uint32(pkt[upcHandle:]); handle != 1792 {
		t.Fatalf("the forwarded appearance names handle %d, want the wearer's 1792", handle)
	}
	if got := binary.LittleEndian.Uint32(pkt[upcAppearance+apEquipment+1*equipEntrySize:]); got != 15001 {
		t.Fatalf("the forwarded appearance's slot 1 = %d, want the new armour 15001", got)
	}

	// The roster must carry it, or the next joiner is spawned in the old body.
	var rosterEquip uint32
	for _, m := range h.Players.Snapshot() {
		if m.Sess == me.sess {
			rosterEquip = m.Chars[0].Equip[1]
		}
	}
	if rosterEquip != 15001 {
		t.Fatalf("the roster still holds slot 1 = %d, want the worn 15001", rosterEquip)
	}

	// The wearer's OWN connection did not get a second copy through the fan-out:
	// its 3546 is in the reply the caller writes, and sending it twice would redraw
	// the body twice for no reason.
	if n := len(me.conn.sent()); n != 0 {
		t.Fatalf("the wearer's own socket was written %d time(s) by the fan-out, want 0", n)
	}
}

// TestEquipInfoListsAreEmptyAndWellFormed pins the three lists:
//
//	+0 cmd(2)  +2 len(2)=9  +4 charIndex(1)  +5 count(4)=0
//
// An empty list is NOT a stub: this server has no reinforcement, sockets or
// additional properties, so 0 is exactly what the reference's GetItemReinforces /
// GetItemSockets / GetItemProperties answer for a worn set with none. Declaring a
// count without writing the records - the Python server's old bug - makes the
// client read the next packet as option data.
func TestEquipInfoListsAreEmptyAndWellFormed(t *testing.T) {
	lists := BuildEquipInfoLists(2)
	wantCmds(t, cmdsOf(lists),
		CmdZCEquipReinforceInfoList, CmdZCEquipSocketInfoList, CmdZCEquipAdditionPropList)

	for _, p := range lists {
		if len(p) != equipInfoListSize {
			t.Fatalf("cmd %d is %d bytes, want %d (the header alone)",
				cmdOf(p), len(p), equipInfoListSize)
		}
		if got := binary.LittleEndian.Uint16(p[2:]); int(got) != len(p) {
			t.Fatalf("cmd %d length word = %d, want the whole packet %d",
				cmdOf(p), got, len(p))
		}
		if p[equipInfoListCharIdx] != 2 {
			t.Fatalf("cmd %d charIndex = %d, want the team index 2",
				cmdOf(p), p[equipInfoListCharIdx])
		}
		if got := binary.LittleEndian.Uint32(p[equipInfoListCountOff:]); got != 0 {
			t.Fatalf("cmd %d count = %d, want 0 - this server has no records to write "+
				"and a count without records desyncs the client", cmdOf(p), got)
		}
	}
}
