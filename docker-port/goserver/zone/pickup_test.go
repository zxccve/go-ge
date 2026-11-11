package zone

// PICKUP (R98 slice 4): WHAT MUST HOLD.
//
// THE SPEC IS `history\monster-death.md` §4 and the reference's pick path
// (zone\pickup.go's header lists it line by line). The tests are the task's own pass
// conditions, one per claim:
//
//	1  a valid pickup of a REAL ground item puts ONE bag row in (quantity unchanged),
//	   takes the item off the ground, and sends 3539 + 3555 with both handles right
//	2  an unknown handle sends nothing and writes ONE line
//	3  a body out of reach is refused and the item stays where it was
//	4  a second pickup of the same handle is refused - the bag cannot receive it twice
//	5  a bag insert that the write path refuses leaves the item on the ground and
//	   invents no bag row
//	6  a watcher inside gets the 3539, one past ViewMiddleRange does not (R135e)
//	7  a click at 124 units reaches the item - the owner's own refused distance, and
//	   the old 80-unit reach would have refused it
//	8  a click at 131 units is refused and the item stays
//	9  a click at exactly 130 units reaches it - the reference's `<=`, not `<`
//
// NOTHING IS SEEDED GLOBALLY AND NOTHING SLEEPS. The item on the ground is made by the
// REAL drop roll through the REAL kill path (damageMonsterBy -> takeHP -> the corpse's
// ZC_DEAD pass -> rollMonsterDrop -> sendDropPackets), so the handle a test picks up is
// the handle a 3536 actually carried - not one a test wrote into the registry by hand.
// The bag is the fake write path shop_trade_test.go already uses, which applies the same
// two rules the real one does.

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"testing"

	"geserver/item"
	"geserver/protocol"
)

// pickupFixture is one real drop, ready to be picked up.
//
// THE COUNT IS 3 ON PURPOSE. The trophy is Per 1 / Quant 3 and the scripted draw is 2,
// so the ground item holds 3 units (zone\drop_test.go's scripted sequence, the same
// arithmetic). A pickup that bagged 1 - or 4 - fails pass condition 1 instead of
// passing on a 1-unit drop where every wrong answer looks right.
type pickupFixture struct {
	h      *Handler
	logs   *logBuf
	picker *player
	bag    *fakeShop
	// item is the registry entry the fixture read back AFTER the kill, and its Handle
	// is the one the picker's own 3536 carried - the fixture fails if those two ever
	// disagree, because every test below then names the wrong item.
	item groundItem
}

// newPickupFixture kills the fixture monster through the real ledger, reads the 3536
// the picker received, and checks the registry holds exactly that handle.
func newPickupFixture(t *testing.T) *pickupFixture {
	t.Helper()
	logs := &logBuf{}
	bag := newFakeShop()
	h := &Handler{
		Log:      log.New(logs, "", 0),
		MapID:    1331,
		Monsters: newMonsters(map[uint32]MonsterRow{11600: trophyRow(1, 3)}),
		Items:    newItemTable(dropItem(30000, "Vis", "", 0, false)),
		// THE MAP ROW IS NOT BLOCKED, AND IT IS SPELLED OUT ON PURPOSE. The SHIPPED
		// 1331 row says MonsterDropBlock="TRUE", so on the field this zone actually
		// serves the trophy rule is suppressed and no ground item ever appears - maps
		// 1301, 208 and 6004 are the shipped FALSE rows (zone\drop_test.go owns that
		// rule, not this fixture). A pickup test needs ONE item on the ground before
		// the pickup runs, so the fixture's own row says Blocked=false and carries no
		// zone items: the only ground item is the monster row's single trophy slot,
		// and the check below fails if the kill creates more than one 3536.
		MapDrops:    &MapDropTable{rows: map[int]MapDropRow{1331: {Blocked: false}}},
		DropPenalty: flatPenalty(1),
		DropRand:    &scriptedDropRand{t: t, vals: []int{2}}, // quantity 3 of 3
		Bag: func(context.Context, int32) ([]item.Stack, error) {
			return bag.stacks, nil
		},
		Commit: bag.commit,
	}

	m := monsterAtLevel(10)
	picker := newPlayer(1792, "Picker", [3]int32{m.X, m.Y, m.Cell})
	picker.sess.CommanderID = 24
	picker.sess.Inv = NewInvMirror(nil)
	h.Players.Add(picker.sess)

	killOnce(t, h, m, picker.sess.Team[0].Handle)

	frames := itemFrames(picker.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("%d 3536 frame(s) reached the picker, want exactly 1 - the fixture "+
			"has no ground item:\n%s", len(frames), logs.String())
	}
	handle := binary.LittleEndian.Uint32(frames[0][ssiHandle:])
	it, ok := h.groundItemAt(handle)
	if !ok {
		t.Fatalf("the 3536 carried handle 0x%08X and the registry does not hold it - "+
			"the item a client can click is not the item a pickup can resolve:\n%s",
			handle, logs.String())
	}
	if it.ItemID != 30000 || it.Count != 3 {
		t.Fatalf("the registry holds class %d x%d, want the frame's class 30000 x3",
			it.ItemID, it.Count)
	}
	if it.Pos != [3]int32{m.X, m.Y, m.Cell} {
		t.Fatalf("the item lies at %v, want the corpse's own cell %v", it.Pos,
			[3]int32{m.X, m.Y, m.Cell})
	}
	// Everything above is fixture bookkeeping; the socket resets so an assertion is
	// about the PICKUP and not about the drop that built the fixture.
	picker.conn.writes = nil
	return &pickupFixture{h: h, logs: logs, picker: picker, bag: bag, item: it}
}

// pickFrame builds one CZ_PICKITEM from the struct's own offsets, checksum included -
// the client computes its own over the frame, and the zone logs a mismatch rather than
// refusing it, so a fixture without one would never exercise that path.
func pickFrame(charIndex uint8, itemHandle uint32) []byte {
	b := make([]byte, czPickItemSize)
	binary.LittleEndian.PutUint16(b[0:], uint16(CmdCZPickItem))
	binary.LittleEndian.PutUint32(b[2:], 0x1c) // seq, the value the attack fixtures carry
	b[czpCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[czpItemID:], itemHandle)
	binary.LittleEndian.PutUint32(b[czpChecksum:], czChecksum(b))
	return b
}

// pickupOnce sends one CZ_PICKITEM through the REAL dispatch, so a test that passes
// cannot pass with no case in Reply's switch.
func pickupOnce(t *testing.T, h *Handler, sess *Session, itemHandle uint32) [][]byte {
	t.Helper()
	f := pickFrame(0, itemHandle)
	out, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZPickItem, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestPickupPutsTheItemInTheBagAndClearsTheGround is pass condition 1: the item a
// 3536 announced goes into the bag through the bag's ONE write path, comes off the
// ground, and the client is told the two things it needs - the pickup (3555, both
// handles) and the leave (3539).
func TestPickupPutsTheItemInTheBagAndClearsTheGround(t *testing.T) {
	f := newPickupFixture(t)
	handle := f.item.Handle
	charHandle := f.picker.sess.Team[0].Handle

	out := pickupOnce(t, f.h, f.picker.sess, handle)

	// THE BAG: exactly ONE row, of the class and the COUNT the ground item held.
	if len(f.bag.stacks) != 1 {
		t.Fatalf("the bag holds %d row(s), want exactly 1: %+v", len(f.bag.stacks),
			f.bag.stacks)
	}
	if got := f.bag.stacks[0]; got.ClassID != 30000 || got.Count != 3 {
		t.Fatalf("the bag row is class %d x%d, want class 30000 x3 (the ground "+
			"item's own quantity, unchanged)", got.ClassID, got.Count)
	}
	if len(f.bag.ops) != 1 || !f.bag.ops[0].Give || f.bag.ops[0].ClassID != 30000 ||
		f.bag.ops[0].Count != 3 {
		t.Fatalf("the write path was asked for %+v, want one give of 3 x 30000",
			f.bag.ops)
	}

	// THE BAG PACKET: one 3607, ADD_ITEM_PICK, the row's new total, position 1.
	if len(out) != 1 {
		t.Fatalf("the picker got %d returned packet(s) (%v), want exactly the 3607",
			len(out), cmdsOf(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCAddItem {
		t.Fatalf("returned cmd = %d, want %d (ZC_ADD_ITEM)", cmd, CmdZCAddItem)
	}
	if out[0][25] != AddItemPick {
		t.Fatalf("addType = %d, want ADD_ITEM_PICK (3) - a pickup is not a purchase",
			out[0][25])
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != 30000 {
		t.Fatalf("the bag row's class = %d, want 30000", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 1 {
		t.Fatalf("the bag row's position = %d, want 1 (a fresh bag's first row)", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 3 {
		t.Fatalf("the bag row's total = %d, want 3", got)
	}

	// THE WORLD NEWS, in the order the client needs it: the 3555 (with the item still
	// in its world) and then the 3539.
	sent := f.picker.conn.sent()
	if len(sent) != 2 {
		t.Fatalf("the picker's socket got %d packet(s) %v, want 2 (3555 then 3539):\n%s",
			len(sent), cmdsOf(sent), f.logs.String())
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCPickItem {
		t.Fatalf("first packet cmd = %d, want %d (ZC_PICKITEM) - it must precede the "+
			"leave, or the client has no item left to play the pickup on", cmd,
			CmdZCPickItem)
	}
	if len(sent[0]) != PickItemSize {
		t.Fatalf("ZC_PICKITEM is %d bytes, want %d", len(sent[0]), PickItemSize)
	}
	if got := binary.LittleEndian.Uint32(sent[0][2:]); got != charHandle {
		t.Fatalf("ZC_PICKITEM handle = %d, want the PICKING CHARACTER %d (the client "+
			"looks its actor up by it)", got, charHandle)
	}
	if got := binary.LittleEndian.Uint32(sent[0][6:]); got != handle {
		t.Fatalf("ZC_PICKITEM itemHandle = 0x%08X, want the ground item 0x%08X",
			got, handle)
	}
	if cmd := cmdOf(sent[1]); cmd != CmdZCLeaveItem {
		t.Fatalf("second packet cmd = %d, want %d (ZC_LEAVE_ITEM)", cmd, CmdZCLeaveItem)
	}
	if len(sent[1]) != LeaveItemSize {
		t.Fatalf("ZC_LEAVE_ITEM is %d bytes, want %d", len(sent[1]), LeaveItemSize)
	}
	if got := binary.LittleEndian.Uint32(sent[1][2:]); got != handle {
		t.Fatalf("ZC_LEAVE_ITEM handle = 0x%08X, want 0x%08X", got, handle)
	}

	// AND THE ITEM IS GONE FROM THIS ZONE'S WORLD.
	if _, still := f.h.groundItemAt(handle); still {
		t.Fatal("the item is still in the registry after a successful pickup")
	}
	if line := lineWith(f.logs.String(), "zone: PICKITEM commander 24"); !strings.Contains(
		line, "class 30000 x3") {
		t.Fatalf("the take line does not name the item and its quantity:\n%s", line)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(f.logs.String()))
}

// TestPickupUnknownHandleSendsNothingAndLogsOnce is pass condition 2: a handle this
// zone never created is refused with ONE line, no packet and no write.
func TestPickupUnknownHandleSendsNothingAndLogsOnce(t *testing.T) {
	f := newPickupFixture(t)

	out := pickupOnce(t, f.h, f.picker.sess, ItemHandleBase+9999)

	if len(out) != 0 {
		t.Fatalf("an unknown handle returned %v, want nothing", cmdsOf(out))
	}
	if sent := f.picker.conn.sent(); len(sent) != 0 {
		t.Fatalf("an unknown handle sent %v, want nothing", cmdsOf(sent))
	}
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("an unknown handle wrote to the bag: ops %+v, stacks %+v",
			f.bag.ops, f.bag.stacks)
	}
	text := f.logs.String()
	if got := strings.Count(text, "PICKITEM refused"); got != 1 {
		t.Fatalf("%d refusal line(s), want exactly 1:\n%s", got, text)
	}
	if !strings.Contains(text, "is not a ground item") {
		t.Fatalf("the refusal does not say the handle is not a ground item:\n%s", text)
	}
	// AND THE REAL ITEM IS UNTOUCHED: a bogus request must not shadow the registry.
	if _, ok := f.h.groundItemAt(f.item.Handle); !ok {
		t.Fatal("the fixture's ground item disappeared after an unknown handle was refused")
	}
}

// TestPickupOutOfReachIsRefusedAndTheItemStays is pass condition 3: the reach is the
// reference's own 130 units (DISTANCE_TO_ITEM 30 + the picker's ST_SMALL body 50 + the
// item's ST_SMALL radius 50), and a body past it changes NOTHING.
func TestPickupOutOfReachIsRefusedAndTheItemStays(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item

	// 200 units east: past the 130-unit reach, still inside the sight ring.
	far := [3]int32{it.Pos[0] + 200, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	if len(out) != 0 || len(f.picker.conn.sent()) != 0 {
		t.Fatalf("an out-of-reach pickup answered %v / %v, want nothing",
			cmdsOf(out), cmdsOf(f.picker.conn.sent()))
	}
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("an out-of-reach pickup wrote to the bag: %+v", f.bag.ops)
	}
	if !strings.Contains(f.logs.String(), "stands 200 units") {
		t.Fatalf("the refusal does not name the distance:\n%s", f.logs.String())
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("the item left the ground for a pickup that was refused")
	}

	// THE PROOF THAT IT STAYED: walk the body onto it and the SAME handle works.
	cullPlace(f.h, f.picker, it.Pos)
	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); len(out) != 1 {
		t.Fatalf("the item could not be picked up after being refused once: %v",
			cmdsOf(out))
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag holds %+v, want one row of 3", f.bag.stacks)
	}
}

// TestPickupAt124UnitsReachesTheItem is pass condition 7 and the OWNER'S OWN CASE: the
// 2026-11-08 16:04 log refused a click at 124 units nine times against the old 80. With
// the picker's own ST_SMALL body in the sum the reach is 130, so 124 is inside it and
// the item goes into the bag. The distance is a literal, not `pickupReach - 6`, so a
// constant that shrinks back to 80 fails HERE and not only in the log.
func TestPickupAt124UnitsReachesTheItem(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item

	near := [3]int32{it.Pos[0] + 124, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, near)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	if len(out) != 1 || cmdOf(out[0]) != CmdZCAddItem {
		t.Fatalf("a click at 124 units answered %v, want the 3607 - 124 is inside the "+
			"reference's 130:\n%s", cmdsOf(out), f.logs.String())
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].ClassID != 30000 ||
		f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag holds %+v, want one row of class 30000 x3", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); still {
		t.Fatal("the item is still on the ground after a pickup at 124 units")
	}
	if !strings.Contains(f.logs.String(), "at 124 units") {
		t.Fatalf("the take line does not name the 124-unit distance:\n%s",
			f.logs.String())
	}
}

// TestPickupAt131UnitsIsRefused is pass condition 8: one unit past the reference's sum
// changes nothing, and the refusal line quotes the arithmetic that produced the reach.
func TestPickupAt131UnitsIsRefused(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item

	far := [3]int32{it.Pos[0] + 131, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	if len(out) != 0 || len(f.picker.conn.sent()) != 0 {
		t.Fatalf("a click at 131 units answered %v / %v, want nothing",
			cmdsOf(out), cmdsOf(f.picker.conn.sent()))
	}
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("a refused 131-unit pickup wrote to the bag: %+v", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("the item left the ground for a click at 131 units")
	}
	text := f.logs.String()
	if !strings.Contains(text, "stands 131 units") {
		t.Fatalf("the refusal does not name the distance:\n%s", text)
	}
	// THE ARITHMETIC, ALL THREE TERMS: the next short reach is one grep away.
	if !strings.Contains(text, "the pickup reach is 130 (DISTANCE_TO_ITEM 30 + "+
		"PCBodyRadius 50 + the item's ST_SMALL radius 50)") {
		t.Fatalf("the refusal does not quote the three-term reach:\n%s", text)
	}
}

// TestPickupAtExactly130UnitsReaches is pass condition 9, the boundary: the reference
// compares with `<=` (FieldObject.cpp:120-126), so exactly 130 is IN reach.
func TestPickupAtExactly130UnitsReaches(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item

	if pickupReach != 130 {
		t.Fatalf("pickupReach = %g, want 130 = DISTANCE_TO_ITEM 30 + PCBodyRadius 50 "+
			"+ the item's ST_SMALL radius 50", float64(pickupReach))
	}
	at := [3]int32{it.Pos[0] + 130, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, at)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	if len(out) != 1 || cmdOf(out[0]) != CmdZCAddItem {
		t.Fatalf("a click at exactly 130 units answered %v, want the 3607 - the "+
			"reference's comparison is <=, not <:\n%s", cmdsOf(out), f.logs.String())
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag holds %+v, want one row of 3", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); still {
		t.Fatal("the item is still on the ground after a pickup at exactly 130 units")
	}
}

// TestSecondPickupOfTheSameHandleIsRefused is pass condition 4: the item IS the claim,
// so a repeated 3611 for a handle already taken cannot put a second row in the bag.
func TestSecondPickupOfTheSameHandleIsRefused(t *testing.T) {
	f := newPickupFixture(t)
	handle := f.item.Handle

	if out := pickupOnce(t, f.h, f.picker.sess, handle); len(out) != 1 {
		t.Fatalf("the first pickup returned %v, want the 3607", cmdsOf(out))
	}
	afterFirst := len(f.picker.conn.sent())

	out := pickupOnce(t, f.h, f.picker.sess, handle)

	if len(out) != 0 {
		t.Fatalf("the second pickup of the same handle returned %v, want nothing",
			cmdsOf(out))
	}
	if sent := f.picker.conn.sent(); len(sent) != afterFirst {
		t.Fatalf("the second pickup sent %v, want nothing new", cmdsOf(sent[afterFirst:]))
	}
	if len(f.bag.stacks) != 1 {
		t.Fatalf("the bag holds %d row(s) %+v, want the ONE the first pickup made",
			len(f.bag.stacks), f.bag.stacks)
	}
	if f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag row was written twice: count %d, want 3", f.bag.stacks[0].Count)
	}
	if got := strings.Count(f.logs.String(), "is not a ground item"); got != 1 {
		t.Fatalf("%d \"not a ground item\" line(s), want exactly 1:\n%s", got,
			f.logs.String())
	}
}

// TestPickupWithARefusedBagInsertLeavesTheItemOnTheGround is pass condition 5: the
// claim happens BEFORE the write, so a write the bag refuses must put the item back
// and must not invent a row.
func TestPickupWithARefusedBagInsertLeavesTheItemOnTheGround(t *testing.T) {
	f := newPickupFixture(t)
	handle := f.item.Handle

	f.bag.fail = errors.New("FULL INVENTORY: commander 24 has no free slot")
	out := pickupOnce(t, f.h, f.picker.sess, handle)

	if len(out) != 0 || len(f.picker.conn.sent()) != 0 {
		t.Fatalf("a refused insert answered %v / %v, want nothing",
			cmdsOf(out), cmdsOf(f.picker.conn.sent()))
	}
	if len(f.bag.stacks) != 0 {
		t.Fatalf("a refused insert left %+v in the bag", f.bag.stacks)
	}
	if !strings.Contains(f.logs.String(), "bag refused") {
		t.Fatalf("the refusal does not say the bag refused the item:\n%s",
			f.logs.String())
	}
	it, ok := f.h.groundItemAt(handle)
	if !ok {
		t.Fatal("the item was not put back after the bag refused it")
	}

	// AND THE ITEM IS REALLY USABLE AGAIN, at its ORIGINAL handle and place.
	f.bag.fail = nil
	if out := pickupOnce(t, f.h, f.picker.sess, handle); len(out) != 1 {
		t.Fatalf("the item could not be picked up after a refused insert: %v",
			cmdsOf(out))
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].ClassID != int64(it.ItemID) ||
		f.bag.stacks[0].Count != int64(it.Count) {
		t.Fatalf("the bag holds %+v, want one row of %d x%d", f.bag.stacks, it.ItemID,
			it.Count)
	}
}

// TestPickupTellsOnlyTheViewersInsideTheMiddleRange is pass condition 6: the leave
// travels the field-item ring, which is BOTH radii - inside ViewMiddleRange hears it,
// the band between the middle and ViewRange does not (R135e), and past ViewRange does
// not either.
func TestPickupTellsOnlyTheViewersInsideTheMiddleRange(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item
	sightUseTable(t, map[int]SightRow{1331: {ViewRange: 6000, ViewMiddleRange: 4000}})
	f.h.MapID = 1331

	// THREE WATCHERS, each placed by the same helper the cull tests use, so the ring
	// reads a BODY and not a click target.
	nearPos := [3]int32{it.Pos[0] + 100, it.Pos[1], it.Pos[2]}
	near := newPlayer(0x40000041, "Near", nearPos)
	f.h.Players.Add(near.sess)
	cullPlace(f.h, near, nearPos)

	middlePos := [3]int32{it.Pos[0] + 4500, it.Pos[1], it.Pos[2]}
	middle := newPlayer(0x40000042, "Middle", middlePos)
	f.h.Players.Add(middle.sess)
	cullPlace(f.h, middle, middlePos)

	outsidePos := [3]int32{it.Pos[0] + 7000, it.Pos[1], it.Pos[2]}
	outside := newPlayer(0x40000043, "Outside", outsidePos)
	f.h.Players.Add(outside.sess)
	cullPlace(f.h, outside, outsidePos)

	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); len(out) != 1 {
		t.Fatalf("the pickup returned %v, want the 3607 - the cull must not stop the "+
			"pickup itself:\n%s", cmdsOf(out), f.logs.String())
	}

	sent := near.conn.sent()
	if len(sent) != 2 || cmdOf(sent[0]) != CmdZCPickItem || cmdOf(sent[1]) != CmdZCLeaveItem {
		t.Fatalf("the watcher 100 units away got %v, want 3555 then 3539:\n%s",
			cmdsOf(sent), f.logs.String())
	}
	if got := binary.LittleEndian.Uint32(sent[1][2:]); got != it.Handle {
		t.Fatalf("the watcher's 3539 names 0x%08X, want 0x%08X", got, it.Handle)
	}
	if sent := middle.conn.sent(); len(sent) != 0 {
		t.Fatalf("the watcher 4,500 away - past the 4,000 middle range - got %v, "+
			"want nothing", cmdsOf(sent))
	}
	if sent := outside.conn.sent(); len(sent) != 0 {
		t.Fatalf("the watcher 7,000 away - past the 6,000 ring - got %v, want nothing",
			cmdsOf(sent))
	}
}
