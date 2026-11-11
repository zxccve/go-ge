package zone

// THE TWO BAG VERBS: WHAT MUST HOLD.
//
// The spec is the card's pass condition plus the reference's two transactions
// (TxDropItem.cpp:101-258, TxUseItem.cpp:102-240), and every test drives the REAL
// dispatch (`h.Reply`) so a test cannot pass with no case in the switch. The bag and the
// write path are the fake shop_trade_test.go already uses - it applies the same two rules
// the real one does (a named row, delete at zero, refuse a missing row) - and NOTHING
// here sleeps, seeds a global or starts a server.
//
//	1  a partial drop: the ground item holds the dropped count, the bag row keeps the
//	   remainder, the 3608 carries the remainder, ONE log line names all four
//	2  a second drop of the remainder removes the row, and the emptied position is a hole
//	3  every refusal (count 0, count > stack, Vis, Feso, a slot the client never had)
//	   leaves the bag BYTE-IDENTICAL and sends nothing
//	4  a use takes exactly ONE, sends 3613 with the class and then the 3608 with the new
//	   total, and accepts BOTH self spellings (0 and the character's own handle)
//	5  every use refusal sends nothing: a non-Consume group, a Scroll, a UseLv above the
//	   character, an absent slot, a foreign target, a class with no Item row
//	6  the dropped stack is a REAL ground item: the pickup handler resolves its handle and
//	   puts the same count back in the bag through its own path
//	7  the four ids and sizes are the CLIENT TABLE's, not this file's memory
//	8  a healing potion raises HP by exactly the Item row's arg1 and the 3557's +6
//	   BYTES decode to the new HP as a double (class 20001, arg1 750)
//	9  the same use at 99% of the maximum clamps EXACTLY onto it, +14 carries the real
//	   delta (100) and never the arg1 (750)
//	10 a use at full HP is CONSUMED and sends no 3557 - the reference's AddHP returns 0
//	   when hp == expectHP (FieldChar.cpp:156-157) and its transaction already took the
//	   item (TxUseItem.cpp:238)
//	11 the other players get the SAME 3557 bytes (their copy of the bar moves too)
//	12 a row that names SCP_USE_ITEM_HPUP and has no arg1 is REFUSED - nothing consumed,
//	   nothing sent (a GAP, not a 0-point heal); the same for a character with no maximum
//	13 a Consume item with no heal script still sends 3613 + 3608 and says so in ONE line

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"geserver/item"
	"geserver/protocol"
)

// bagVerbFixture is one commander's bag, a fake write path, and a socket.
type bagVerbFixture struct {
	h    *Handler
	logs *logBuf
	p    *player
	bag  *fakeShop
}

// bagRows are the classes this fixture's item table knows, one per rule a test names:
// two real healing potions (Consume + SCP_USE_ITEM_HPUP, arg1 180 and 750), a real Consume
// item whose effect this build does not carry (SoulFiller, an SP potion), a synthetic
// heal row with no arg1, a plain Etc item, a stance Scroll, the two money classes, and a
// potion above the fixture character's level (newPlayer is 9).
func bagVerbRows() []ItemRow {
	return []ItemRow{
		{ID: 21613, Name: "Fire stone", GroupName: "Etc"},
		{ID: 75000, Name: "Stance Scroll", GroupName: "Scroll"},
		{ID: 30000, Name: "Vis", GroupName: "Money", UseLv: 1},
		{ID: 30001, Name: "Feso", GroupName: "Money", UseLv: 1},
		{ID: 21099, Name: "High potion", GroupName: "Consume", UseLv: 50},
		// THE HEAL ROWS, exactly as the client's own table states them
		// (cache\datatable_item_consume.xml): class 20001 HealingPotion is
		// `ScpType="SCP_USE_ITEM_HPUP"` with `arg1="750"`, and class 20000 HealingAmpul is
		// the same script at arg1 180. 30 Consume rows carry that script in the shipped
		// table, and arg1 is THE number the client's own batch-use script reads as
		// `GetPropertyNumber('Item', itemType, 'arg1')` (useitem.scp:14).
		{ID: 20000, Name: "HealingAmpul", GroupName: "Consume", UseLv: 1,
			ScpType: "SCP_USE_ITEM_HPUP", Arg1: 180, Arg1Known: true},
		{ID: 20001, Name: "HealingPotion", GroupName: "Consume", UseLv: 1,
			ScpType: "SCP_USE_ITEM_HPUP", Arg1: 750, Arg1Known: true},
		// A REAL Consume item whose use this build does NOT carry: class 20009 SoulFiller
		// runs `SCP_USE_ITEM_SPUP` (an SP potion, arg1 600) - the row the "consumed and
		// announced, no effect, one line" tests use.
		{ID: 20009, Name: "SoulFiller", GroupName: "Consume", UseLv: 1,
			ScpType: "SCP_USE_ITEM_SPUP", Arg1: 600, Arg1Known: true},
		// A SYNTHETIC row - no shipped class is like this - that names the heal script and
		// carries no arg1: the DATA GAP the use verb must refuse instead of healing 0
		// (no fallback numbers). Its id is outside the shipped 20xxx range for that reason.
		{ID: 29999, Name: "HealingPotionNoArg", GroupName: "Consume", UseLv: 1,
			ScpType: "SCP_USE_ITEM_HPUP"},
	}
}

// newBagVerbFixture builds a session that has entered the world with `stacks` in its bag,
// and the mirror built from exactly those rows - the client's key space IS that order.
func newBagVerbFixture(t *testing.T, stacks ...item.Stack) *bagVerbFixture {
	t.Helper()
	logs := &logBuf{}
	bag := newFakeShop(stacks...)
	h := &Handler{
		Log:   log.New(logs, "", 0),
		MapID: 1331,
		Items: newItemTable(bagVerbRows()...),
		Bag: func(context.Context, int32) ([]item.Stack, error) {
			return bag.stacks, nil
		},
		Commit: bag.commit,
	}
	p := newPlayer(1792, "Dropper", [3]int32{100, 200, 7})
	p.sess.CommanderID = 24
	p.sess.Inv = NewInvMirror(bag.stacks)
	h.Players.Add(p.sess)
	return &bagVerbFixture{h: h, logs: logs, p: p, bag: bag}
}

// dropFrame builds one CZ_DROP from the struct's own offsets. The security header is
// cmd(2) seq(4) checksum(4); neither verb validates the checksum, so the seq is the value
// the other fixtures carry and the checksum stays 0.
func dropFrame(charIndex uint8, slot, count uint32) []byte {
	b := make([]byte, czDropSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZDrop)
	binary.LittleEndian.PutUint32(b[2:], 0x1c)
	b[czdCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[czdSlot:], slot)
	binary.LittleEndian.PutUint32(b[czdCount:], count)
	return b
}

// useFrame builds one CZ_USEITEM_TARGET (20 bytes) - `last` is the overwriteBuff byte.
func useFrame(charIndex uint8, slot, target uint32, last uint8) []byte {
	b := make([]byte, czUseItemTargetSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZUseItemTarget)
	binary.LittleEndian.PutUint32(b[2:], 0x1c)
	b[czuCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[czuSlot:], slot)
	binary.LittleEndian.PutUint32(b[czuTarget:], target)
	b[czuLast] = last
	return b
}

// dropOnce sends one CZ_DROP through the REAL dispatch.
func dropOnce(t *testing.T, f *bagVerbFixture, slot, count uint32) [][]byte {
	t.Helper()
	b := dropFrame(0, slot, count)
	out, err := f.h.Reply(context.Background(), f.p.sess,
		protocol.Frame{Cmd: CmdCZDrop, Size: len(b), Data: b})
	if err != nil {
		t.Fatalf("Reply(CZ_DROP): %v", err)
	}
	return out
}

// useOnce sends one CZ_USEITEM_TARGET through the REAL dispatch.
func useOnce(t *testing.T, f *bagVerbFixture, slot, target uint32) [][]byte {
	t.Helper()
	b := useFrame(0, slot, target, 0)
	out, err := f.h.Reply(context.Background(), f.p.sess,
		protocol.Frame{Cmd: CmdCZUseItemTarget, Size: len(b), Data: b})
	if err != nil {
		t.Fatalf("Reply(CZ_USEITEM_TARGET): %v", err)
	}
	return out
}

// groundItemsOf is every field item this zone holds, in handle order.
func groundItemsOf(h *Handler) []groundItem {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	out := make([]groundItem, 0, len(h.groundItems))
	for _, it := range h.groundItems {
		out = append(out, it)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Handle > out[j].Handle; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// TestAPartialDropPutsTheDroppedCountOnTheGround is pass condition 1: a drop of 3 from a
// stack of 5 puts a THREE-unit field item on the floor, leaves TWO in the row, and tells
// the client the row's new total.
func TestAPartialDropPutsTheDroppedCountOnTheGround(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20000, Count: 5})

	out := dropOnce(t, f, 1, 3)

	// THE BAG: the row still there, two left, and the op named THAT row (not the class).
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 2 {
		t.Fatalf("the bag is %+v, want one row of 2 (5 dropped 3)", f.bag.stacks)
	}
	if len(f.bag.ops) != 1 || f.bag.ops[0].Give || f.bag.ops[0].ItemID != 7001 ||
		f.bag.ops[0].Count != 3 {
		t.Fatalf("the write path was asked for %+v, want one take of 3 from item 7001",
			f.bag.ops)
	}

	// THE BAG PACKET: exactly one 3608, position 1, total 2, removeType REMOVE_ITEM_DROP.
	if len(out) != 1 {
		t.Fatalf("the dropper got %d returned packet(s) %v, want exactly one 3608",
			len(out), cmdsOf(out))
	}
	if len(out[0]) != 15 || cmdOf(out[0]) != CmdZCRemoveItem {
		t.Fatalf("returned packet cmd %d, %d bytes - want cmd 3608, the fixed 15",
			cmdOf(out[0]), len(out[0]))
	}
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != 1 {
		t.Fatalf("the 3608 names position %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 2 {
		t.Fatalf("the 3608 says %d left, want 2", got)
	}
	if out[0][10] != removeItemDrop {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_DROP (%d)", out[0][10], removeItemDrop)
	}

	// THE GROUND: ONE item, the dropped count, at the character's own point.
	ground := groundItemsOf(f.h)
	if len(ground) != 1 {
		t.Fatalf("the ground holds %d item(s), want exactly 1: %+v", len(ground), ground)
	}
	if ground[0].ItemID != 20000 || ground[0].Count != 3 {
		t.Fatalf("the ground item is class %d x%d, want class 20000 x3 (the DROPPED "+
			"count, not the stack's 5)", ground[0].ItemID, ground[0].Count)
	}
	if ground[0].Pos != [3]int32{100, 200, 7} {
		t.Fatalf("the item lies at %v, want the character's own point [100 200 7]",
			ground[0].Pos)
	}

	// THE 3536 went out to the dropper's own socket with that handle and that count.
	frames := itemFrames(f.p.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("the dropper's socket got %d 3536 frame(s), want exactly 1:\n%s",
			len(frames), f.logs.String())
	}
	if got := binary.LittleEndian.Uint32(frames[0][ssiHandle:]); got != ground[0].Handle {
		t.Fatalf("the 3536 names handle 0x%08X and the registry holds 0x%08X - the item "+
			"the client draws is not the item a pickup can resolve", got, ground[0].Handle)
	}
	if got := binary.LittleEndian.Uint32(frames[0][ssiAppCount:]); got != 3 {
		t.Fatalf("the 3536 says count %d, want 3", got)
	}

	// ONE line, naming slot, class, dropped count and the remainder.
	log := f.logs.String()
	for _, want := range []string{"dropped class 20000 x3 from slot 1", "2 left",
		"ground handle 0x"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log does not say %q:\n%s", want, log)
		}
	}
}

// TestASecondDropOfTheRemainderRemovesTheRow is pass condition 2: the drop that empties a
// row removes it, the client is told total 0, and the position becomes a hole - a third
// drop of the same position is refused, because the client's key is not reused.
func TestASecondDropOfTheRemainderRemovesTheRow(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20000, Count: 5})

	if out := dropOnce(t, f, 1, 3); len(out) != 1 {
		t.Fatalf("the first drop returned %v, want one 3608", cmdsOf(out))
	}
	out := dropOnce(t, f, 1, 2)

	if len(f.bag.stacks) != 0 {
		t.Fatalf("the bag still holds %+v after dropping the remainder, want empty",
			f.bag.stacks)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCRemoveItem {
		t.Fatalf("the second drop returned %v, want one 3608", cmdsOf(out))
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 0 {
		t.Fatalf("the second 3608 says %d left, want 0 (the row is gone)", got)
	}
	if ground := groundItemsOf(f.h); len(ground) != 2 {
		t.Fatalf("the ground holds %d item(s), want 2 (3 then 2): %+v", len(ground), ground)
	}

	// THE HOLE: position 1 is not a row any more, so a third drop of it is refused.
	before := len(f.p.conn.sent())
	if out := dropOnce(t, f, 1, 1); len(out) != 0 {
		t.Fatalf("a drop of the emptied position returned %v, want nothing", cmdsOf(out))
	}
	if got := len(f.p.conn.sent()); got != before {
		t.Fatalf("a drop of the emptied position wrote %d packet(s) to the socket, want 0",
			got-before)
	}
	if !strings.Contains(f.logs.String(), "bag position 1 is not a live row") {
		t.Fatalf("the refusal does not name the hole:\n%s", f.logs.String())
	}
}

// TestTheDropRefusalsLeaveTheBagByteIdentical is pass condition 3. Every case is a frame a
// client can really send; the answer is one line and NOTHING else - no packet, no ground
// item, and a bag that is byte-identical to what it was.
func TestTheDropRefusalsLeaveTheBagByteIdentical(t *testing.T) {
	cases := []struct {
		name  string
		stake item.Stack
		slot  uint32
		count uint32
		want  string
	}{
		{"count zero", item.Stack{ItemID: 7001, ClassID: 20000, Count: 5}, 1, 0,
			"asked for 0 of class 20000"},
		{"more than the stack", item.Stack{ItemID: 7001, ClassID: 20000, Count: 5}, 1, 6,
			"asked for 6 of class 20000"},
		{"Vis", item.Stack{ItemID: 7002, ClassID: 30000, Count: 100}, 1, 1,
			"money cannot be dropped (ITEM_ID_VIS 30000 / ITEM_ID_FESO 30001"},
		{"Feso", item.Stack{ItemID: 7003, ClassID: 30001, Count: 100}, 1, 50,
			"money cannot be dropped (ITEM_ID_VIS 30000 / ITEM_ID_FESO 30001"},
		{"a slot the client never had", item.Stack{ItemID: 7001, ClassID: 20000, Count: 5},
			7, 1, "bag position 7 is not a live row"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newBagVerbFixture(t, c.stake)
			before := append([]item.Stack(nil), f.bag.stacks...)

			out := dropOnce(t, f, c.slot, c.count)

			if len(out) != 0 {
				t.Fatalf("the refusal returned %v, want no packet", cmdsOf(out))
			}
			if len(f.p.conn.sent()) != 0 {
				t.Fatalf("the refusal wrote %d packet(s) to the socket, want 0:\n%s",
					len(f.p.conn.sent()), f.logs.String())
			}
			if !reflect.DeepEqual(before, f.bag.stacks) {
				t.Fatalf("the bag changed on a refusal: %+v -> %+v", before, f.bag.stacks)
			}
			if len(f.bag.ops) != 0 {
				t.Fatalf("a refusal reached the write path: %+v", f.bag.ops)
			}
			if ground := groundItemsOf(f.h); len(ground) != 0 {
				t.Fatalf("a refusal created a ground item: %+v", ground)
			}
			if !strings.Contains(f.logs.String(), "DROP refused") ||
				!strings.Contains(f.logs.String(), c.want) {
				t.Fatalf("the refusal line does not say %q:\n%s", c.want, f.logs.String())
			}
		})
	}
}

// TestUseTakesExactlyOneAndAnnouncesTheClass is pass condition 4: the 3613 carries the
// class, the 3608 carries the new total with REMOVE_ITEM_USE, and the bag lost exactly ONE.
// BOTH self spellings are exercised, because the client's own self-use sends the
// character's HANDLE (GENormalItem.cpp:249) and 0 is the "no target" spelling.
func TestUseTakesExactlyOneAndAnnouncesTheClass(t *testing.T) {
	for _, c := range []struct {
		name   string
		target uint32
	}{
		{"target 0", 0},
		{"the character's own handle", 1792},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Class 20009 SoulFiller is a REAL Consume item whose own script
			// (`SCP_USE_ITEM_SPUP`) this build does not carry, so this test is about the
			// two bag-verb packets and not about a heal (the heal has its own tests).
			f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20009, Count: 3})

			out := useOnce(t, f, 1, c.target)

			if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 2 {
				t.Fatalf("the bag is %+v after one use, want one row of 2 (3 - 1)",
					f.bag.stacks)
			}
			if len(f.bag.ops) != 1 || f.bag.ops[0].Give || f.bag.ops[0].Count != 1 ||
				f.bag.ops[0].ItemID != 7001 {
				t.Fatalf("the write path was asked for %+v, want one take of 1 from item 7001",
					f.bag.ops)
			}

			// 3613 FIRST (the announcement), then the 3608 (the stack).
			if len(out) != 2 {
				t.Fatalf("the use returned %v, want exactly [3613 3608]", cmdsOf(out))
			}
			if cmdOf(out[0]) != CmdZCUseItem || len(out[0]) != ZCUseItemSize {
				t.Fatalf("first packet cmd %d, %d bytes - want 3613 at the fixed 6",
					cmdOf(out[0]), len(out[0]))
			}
			if got := binary.LittleEndian.Uint32(out[0][2:]); got != 20009 {
				t.Fatalf("the 3613 announces class %d, want 20009", got)
			}
			if cmdOf(out[1]) != CmdZCRemoveItem || len(out[1]) != 15 {
				t.Fatalf("second packet cmd %d, %d bytes - want the fixed 15-byte 3608",
					cmdOf(out[1]), len(out[1]))
			}
			if got := binary.LittleEndian.Uint32(out[1][2:]); got != 1 {
				t.Fatalf("the 3608 names position %d, want 1", got)
			}
			if got := binary.LittleEndian.Uint32(out[1][6:]); got != 2 {
				t.Fatalf("the 3608 says %d left, want 2", got)
			}
			if out[1][10] != RemoveItemUse {
				t.Fatalf("removeType = %d, want REMOVE_ITEM_USE (%d) - the type whose "+
					"client handler prints its own \"used\" line", out[1][10], RemoveItemUse)
			}

			if ground := groundItemsOf(f.h); len(ground) != 0 {
				t.Fatalf("a use created a ground item: %+v", ground)
			}
			log := f.logs.String()
			for _, want := range []string{"used class 20009 (SoulFiller) from slot 1",
				"2 left"} {
				if !strings.Contains(log, want) {
					t.Fatalf("the log does not say %q:\n%s", want, log)
				}
			}
		})
	}
}

// TestTheUseRefusalsSendNothing is pass condition 5: the game's own data refuses before
// anything is written or sent, and the reason is ONE line.
func TestTheUseRefusalsSendNothing(t *testing.T) {
	cases := []struct {
		name   string
		stacks []item.Stack
		slot   uint32
		target uint32
		want   string
	}{
		{"an Etc item", []item.Stack{{ItemID: 7001, ClassID: 21613, Count: 4}}, 1, 0,
			`GroupName "Etc"`},
		{"a stance Scroll", []item.Stack{{ItemID: 7001, ClassID: 75000, Count: 1}}, 1, 0,
			`GroupName "Scroll"`},
		{"UseLv above the character",
			[]item.Stack{{ItemID: 7001, ClassID: 21099, Count: 2}}, 1, 0,
			"whose UseLv is 50"},
		{"an absent slot", []item.Stack{{ItemID: 7001, ClassID: 20000, Count: 2}}, 9, 0,
			"bag position 9 is not a live row"},
		{"a foreign target", []item.Stack{{ItemID: 7001, ClassID: 20000, Count: 2}}, 1,
			0x00ABC123, "which is not this character"},
		{"a class with no Item row",
			[]item.Stack{{ItemID: 7001, ClassID: 99999, Count: 2}}, 1, 0,
			"the Item table has no row for it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newBagVerbFixture(t, c.stacks...)
			before := append([]item.Stack(nil), f.bag.stacks...)

			out := useOnce(t, f, c.slot, c.target)

			if len(out) != 0 {
				t.Fatalf("the refusal returned %v, want no packet", cmdsOf(out))
			}
			if len(f.p.conn.sent()) != 0 {
				t.Fatalf("the refusal wrote %d packet(s) to the socket, want 0:\n%s",
					len(f.p.conn.sent()), f.logs.String())
			}
			if !reflect.DeepEqual(before, f.bag.stacks) {
				t.Fatalf("the bag changed on a refusal: %+v -> %+v", before, f.bag.stacks)
			}
			if len(f.bag.ops) != 0 {
				t.Fatalf("a refusal reached the write path: %+v", f.bag.ops)
			}
			if !strings.Contains(f.logs.String(), "USEITEM refused") ||
				!strings.Contains(f.logs.String(), c.want) {
				t.Fatalf("the refusal line does not say %q:\n%s", c.want, f.logs.String())
			}
		})
	}
}

// TestTheDroppedStackIsTheStackThePickupReads proves the drop lands in the SAME ground
// registry the pickup resolves: the handle the 3536 carried is picked up by the REAL
// pickup handler, off the floor, and the three units come back into the row that dropped
// them (2 + 3 = 5).
func TestTheDroppedStackIsTheStackThePickupReads(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20000, Count: 5})

	if out := dropOnce(t, f, 1, 3); len(out) != 1 {
		t.Fatalf("the drop returned %v, want one 3608", cmdsOf(out))
	}
	frames := itemFrames(f.p.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("the drop sent %d 3536 frame(s), want 1:\n%s", len(frames), f.logs.String())
	}
	handle := binary.LittleEndian.Uint32(frames[0][ssiHandle:])

	// THE PICKUP, through its own dispatch and its own reach/claim rules.
	picked := pickupOnce(t, f.h, f.p.sess, handle)

	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 5 ||
		f.bag.stacks[0].ItemID != 7001 {
		t.Fatalf("the bag is %+v after drop 3 + pick 3, want one row of 5 (the same row: "+
			"2 + 3)", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(handle); still {
		t.Fatalf("the picked-up item 0x%08X is still on the ground", handle)
	}
	if len(picked) != 1 || cmdOf(picked[0]) != CmdZCAddItem {
		t.Fatalf("the pickup returned %v, want the one 3607", cmdsOf(picked))
	}
	if got := binary.LittleEndian.Uint32(picked[0][12:]); got != 5 {
		t.Fatalf("the 3607 says the row holds %d, want 5", got)
	}
}

// TestTheBagVerbLayoutsAreTheClientTable's pins the four ids and sizes against the client's
// OWN registry (`data\cmd_table.txt`), and the field offsets against a hand-built frame -
// so a later edit cannot move a field and still pass.
func TestTheBagVerbLayoutsAreTheClientTables(t *testing.T) {
	blob, err := os.ReadFile("../../data/cmd_table.txt")
	if err != nil {
		t.Fatalf("the client's command table is not readable: %v", err)
	}
	want := map[string]struct {
		size int
		name string
	}{
		"3612": {19, "PKS_CZ_DROP"},
		"3613": {6, "PKS_ZC_USEITEM"},
		"3651": {20, "PKS_CZ_USEITEM_TARGET"},
		"3608": {15, "PKS_ZC_REMOVEITEM"},
		// THE HEAL'S PACKET: the pass condition asserts 23 bytes, and the client's own
		// registry is what makes 23 the right number (it has NO length field).
		"3557": {23, "PKS_ZC_ADD_HP"},
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(blob), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		w, ok := want[fields[0]]
		if !ok {
			continue
		}
		if got := fields[1]; got != fmt.Sprint(w.size) {
			t.Errorf("cmd %s is %s bytes in the client table, want %d", fields[0], got,
				w.size)
		}
		if fields[2] != w.name {
			t.Errorf("cmd %s is %s in the client table, want %s", fields[0], fields[2],
				w.name)
		}
		seen[fields[0]] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("cmd %s is not in the client table - the id is this file's memory, "+
				"not the client's", id)
		}
	}

	// THE OFFSETS: one frame each, read back through the parsers.
	d := dropFrame(2, 0x01020304, 0x00000005)
	req, ok := ParseDrop(d)
	if !ok || req.CharIndex != 2 || req.Slot != 0x01020304 || req.Count != 5 {
		t.Fatalf("ParseDrop read %+v (ok=%v), want char 2 slot 0x01020304 count 5", req, ok)
	}
	if _, ok := ParseDrop(d[:18]); ok {
		t.Fatal("ParseDrop accepted an 18-byte frame - CZ_DROP is a FIXED 19")
	}
	u := useFrame(1, 0x0A0B0C0D, 0x00FFEE01, 1)
	ureq, ok := ParseUseItemTarget(u)
	if !ok || ureq.CharIndex != 1 || ureq.Slot != 0x0A0B0C0D ||
		ureq.Target != 0x00FFEE01 || ureq.Last != 1 {
		t.Fatalf("ParseUseItemTarget read %+v (ok=%v), want char 1 slot 0x0A0B0C0D "+
			"target 0x00FFEE01 last 1", ureq, ok)
	}
	if _, ok := ParseUseItemTarget(u[:19]); ok {
		t.Fatal("ParseUseItemTarget accepted a 19-byte frame - it is a FIXED 20")
	}
	built := BuildUseItem(20000)
	if len(built) != 6 || cmdOf(built) != CmdZCUseItem ||
		binary.LittleEndian.Uint32(built[2:]) != 20000 {
		t.Fatalf("BuildUseItem built %v, want 6 bytes: cmd 3613 + class 20000", built)
	}
}

// THE HEAL: the potion bug the owner reported (2026-11-08 17:12, "hp does not update
// realtime?"). The rule is `CFieldChar::AddHP` (FieldChar.cpp:132-165): clamp at the
// computed maximum, write the ledger, send ONE 23-byte 3557 - and send NOTHING when the
// value did not move. The numbers are the Item row's own `arg1`, never a constant here.

// healPackets splits a use's reply into its three parts and checks the ORDER once, for
// every heal test: the 3613, the 3608 and then the 3557 (see useItemTarget on why the
// heal is last - the reference's item change precedes its effect, TxUseItem.cpp:238/220).
func healPackets(t *testing.T, out [][]byte) (hp []byte) {
	t.Helper()
	if len(out) != 3 {
		t.Fatalf("the use returned %v (%d packet(s)), want [3613 3608 3557]",
			cmdsOf(out), len(out))
	}
	if got := cmdsOf(out); got[0] != CmdZCUseItem || got[1] != CmdZCRemoveItem ||
		got[2] != CmdZCAddHP {
		t.Fatalf("the use returned %v, want [%d %d %d] - the item change first, then "+
			"the heal", got, CmdZCUseItem, CmdZCRemoveItem, CmdZCAddHP)
	}
	return out[2]
}

// TestAHealingPotionRaisesHPByItsArg1AndTheBarIsTold is the pass condition's first half:
// HP goes up by EXACTLY the row's arg1 (750 from class 20001's own row), and the 3557's
// bytes at +6 are the new HP AS A DOUBLE (the bits are asserted, not just the value).
func TestAHealingPotionRaisesHPByItsArg1AndTheBarIsTold(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20001, Count: 3})
	f.p.sess.Team[0].HP = 1000
	f.p.sess.Team[0].MaxHP = 5000

	out := useOnce(t, f, 1, 0)
	pkt := healPackets(t, out)

	// THE PACKET: 23 bytes, fixed, no length field (PKS_ZC_ADD_HP, cmd_table 3557 23 B).
	if AddHPSize != 23 {
		t.Fatalf("AddHPSize = %d, want the client's fixed 23", AddHPSize)
	}
	if len(pkt) != 23 {
		t.Fatalf("the 3557 is %d bytes, want the fixed 23", len(pkt))
	}
	if cmdOf(pkt) != CmdZCAddHP {
		t.Fatalf("the heal packet's cmd is %d, want %d", cmdOf(pkt), CmdZCAddHP)
	}
	if got := binary.LittleEndian.Uint32(pkt[ahHandle:]); got != 1792 {
		t.Fatalf("the 3557 names handle %d, want the using character's 1792", got)
	}
	// THE BYTES, not the value: +6 IS the current HP as a float64, and 1000 + 750 = 1750.
	if bits := binary.LittleEndian.Uint64(pkt[ahNowHP:]); bits != math.Float64bits(1750) {
		t.Fatalf("the 3557's +6 bytes are %#016x, want the double 1750 (%#016x)",
			bits, math.Float64bits(1750))
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahNowHP:])); got != 1750 {
		t.Fatalf("+6 decodes to %v, want 1750 (1000 + the row's arg1 750)", got)
	}
	// +14 IS THE DELTA (a heal is positive, so +22 stays 0): the client adds it to every
	// OTHER viewer's bar (GENormalObject.cpp:2388-2389).
	if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:])); got != 750 {
		t.Fatalf("+14 decodes to %v, want the 750 delta", got)
	}
	if pkt[ahFlag] != 0 {
		t.Fatalf("+22 = %d, want 0 for a positive delta (61 of 61 measured live 3557s)",
			pkt[ahFlag])
	}

	// THE LEDGER, and the bag: exactly one unit left the row.
	if got := f.p.sess.Team[0].HP; got != 1750 {
		t.Fatalf("the character's HP is %v, want 1750", got)
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 2 {
		t.Fatalf("the bag is %+v after one use, want one row of 2", f.bag.stacks)
	}

	log := f.logs.String()
	for _, want := range []string{"arg1 750", "HP 1000 -> 1750 of max 5000", "delta 750"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the heal line does not say %q:\n%s", want, log)
		}
	}
}

// TestAHealingPotionClampsAtTheMaximum is the pass condition's second half: 99% of the
// maximum plus a much larger arg1 lands EXACTLY on the maximum, never above it
// (`if (mhp <= expectHP) expectHP = mhp`, FieldChar.cpp:141-142).
func TestAHealingPotionClampsAtTheMaximum(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20001, Count: 1})
	f.p.sess.Team[0].HP = 9900 // 99% of 10000
	f.p.sess.Team[0].MaxHP = 10000

	out := useOnce(t, f, 1, 0)
	pkt := healPackets(t, out)

	// 9900 + 750 = 10650, so the answer is the ceiling: 10000.
	if bits := binary.LittleEndian.Uint64(pkt[ahNowHP:]); bits != math.Float64bits(10000) {
		t.Fatalf("the 3557's +6 bytes are %#016x (%v), want the double 10000 - the "+
			"clamped maximum", bits,
			math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahNowHP:])))
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:])); got != 100 {
		t.Fatalf("+14 = %v, want the REAL delta 100 (9900 -> 10000), not the arg1 750",
			got)
	}
	if got := f.p.sess.Team[0].HP; got != 10000 {
		t.Fatalf("the character's HP is %v, want exactly the maximum 10000", got)
	}
}

// TestAUseAtFullHPIsConsumedAndSendsNoHPPacket answers the card's question from the
// reference, not from convenience: `if (0 == add || hp == expectHP) return 0;`
// (FieldChar.cpp:156-157) skips the PACKET, while the item was already taken by the
// transaction (`SendRemoveItem`, TxUseItem.cpp:238 - before the use script runs at
// :218-221). The oracle agrees: `if HEAL_ENABLED and delta:` (src\zone_server.py:747).
func TestAUseAtFullHPIsConsumedAndSendsNoHPPacket(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20001, Count: 2})
	f.p.sess.Team[0].HP = 5000
	f.p.sess.Team[0].MaxHP = 5000

	out := useOnce(t, f, 1, 0)

	if got := cmdsOf(out); len(out) != 2 || got[0] != CmdZCUseItem ||
		got[1] != CmdZCRemoveItem {
		t.Fatalf("the use returned %v, want [3613 3608] and NO 3557", got)
	}
	if len(f.p.conn.sent()) != 0 {
		t.Fatalf("the use wrote %d packet(s) to the socket besides the reply, want 0",
			len(f.p.conn.sent()))
	}
	// THE ITEM IS STILL CONSUMED: the reference's transaction runs first, and a potion
	// drunk at full HP is gone (the oracle's heal_pc does not put it back either).
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 1 {
		t.Fatalf("the bag is %+v, want one row of 1 - the item is consumed even when "+
			"the heal changes nothing", f.bag.stacks)
	}
	if got := f.p.sess.Team[0].HP; got != 5000 {
		t.Fatalf("the character's HP is %v, want the unchanged 5000", got)
	}
	if log := f.logs.String(); !strings.Contains(log, "changes NOTHING and no 3557") {
		t.Fatalf("the log does not say why no packet went out:\n%s", log)
	}
}

// TestTheHealPacketReachesTheOtherPlayers: the 3557 is not only the actor's - the other
// sessions get the SAME bytes (the fan-out the attack family uses), or their copy of the
// character keeps a stale bar (GENormalObject.cpp:2388-2389 on `m_HP`).
func TestTheHealPacketReachesTheOtherPlayers(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20001, Count: 2})
	f.p.sess.Team[0].HP = 1000
	f.p.sess.Team[0].MaxHP = 5000
	other := newPlayer(0x40000099, "Watcher", [3]int32{140, 260, 7})
	f.h.Players.Add(other.sess)

	out := useOnce(t, f, 1, 0)
	pkt := healPackets(t, out)

	watcher := other.conn.sent()
	if len(watcher) != 1 || cmdOf(watcher[0]) != CmdZCAddHP {
		t.Fatalf("the other player got %v, want exactly one 3557", cmdsOf(watcher))
	}
	if !reflect.DeepEqual(watcher[0], pkt) {
		t.Fatalf("the other player's 3557 is not the actor's bytes:\n%x\n%x",
			watcher[0], pkt)
	}
}

// TestAHealWithNoArg1IsRefusedInsteadOfHealingZero is the rail's NO FALLBACK NUMBERS: a
// row that names SCP_USE_ITEM_HPUP and carries no arg1 is a GAP, and the potion stays in
// the bag. A 0-point heal would look exactly like a working one in the log.
func TestAHealWithNoArg1IsRefusedInsteadOfHealingZero(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 29999, Count: 2})
	f.p.sess.Team[0].HP = 100
	f.p.sess.Team[0].MaxHP = 5000
	before := append([]item.Stack(nil), f.bag.stacks...)

	out := useOnce(t, f, 1, 0)

	if len(out) != 0 {
		t.Fatalf("the use returned %v, want no packet", cmdsOf(out))
	}
	if !reflect.DeepEqual(before, f.bag.stacks) {
		t.Fatalf("the bag changed on a GAP: %+v -> %+v", before, f.bag.stacks)
	}
	if got := f.p.sess.Team[0].HP; got != 100 {
		t.Fatalf("the character's HP is %v, want the untouched 100", got)
	}
	log := f.logs.String()
	for _, want := range []string{"carries no arg1", "GAP"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the refusal line does not say %q:\n%s", want, log)
		}
	}
}

// TestAHealWithoutAMaximumIsRefused: the clamp needs a ceiling. A character whose
// maximum is 0 (the status script has not answered) cannot have a heal applied to it -
// no number is guessed and the item is not eaten.
func TestAHealWithoutAMaximumIsRefused(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20001, Count: 2})
	f.p.sess.Team[0].HP = 100
	f.p.sess.Team[0].MaxHP = 0

	out := useOnce(t, f, 1, 0)

	if len(out) != 0 || len(f.p.conn.sent()) != 0 {
		t.Fatalf("the use returned %v, want no packet", cmdsOf(out))
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 2 {
		t.Fatalf("the bag is %+v, want the untouched 2", f.bag.stacks)
	}
	if log := f.logs.String(); !strings.Contains(log, "maximum HP is 0") {
		t.Fatalf("the refusal line does not name the missing maximum:\n%s", log)
	}
}

// TestAConsumeItemWithNoHealScriptSaysSoInOneLine: the OTHER Consume items are still
// announced and consumed, and the log names the row's own script rather than pretending
// the effect was a 0-point heal (buff / exp / rezz / scroll effects are not this card).
func TestAConsumeItemWithNoHealScriptSaysSoInOneLine(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 20009, Count: 2})
	f.p.sess.Team[0].HP = 100
	f.p.sess.Team[0].MaxHP = 5000

	out := useOnce(t, f, 1, 0)

	if got := cmdsOf(out); len(out) != 2 || got[0] != CmdZCUseItem ||
		got[1] != CmdZCRemoveItem {
		t.Fatalf("the use returned %v, want [3613 3608] and no 3557", got)
	}
	if got := f.p.sess.Team[0].HP; got != 100 {
		t.Fatalf("a non-heal item changed HP to %v", got)
	}
	log := f.logs.String()
	for _, want := range []string{`names ScpType "SCP_USE_ITEM_SPUP"`,
		"carries no other use effect", "no HP, no buff, no exp"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the no-effect line does not say %q:\n%s", want, log)
		}
	}
}
