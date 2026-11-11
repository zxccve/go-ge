package zone

// THE PICKUP CHASE (2026-11-08): a CZ_PICKITEM from out of reach WALKS to the item.
//
// THE OWNER'S SYMPTOM, in his zone log: four `PICKITEM refused ... stands 486 units ...
// the pickup reach is 130` in half a second, and the character did not move. The client
// was doing its job - it re-asks while the player holds the click - and the zone waited
// for a walk nobody had ordered. The reference walks: `RequestPickItem` records the
// request (PlayerChar_Request.cpp:187-191), `DispatchPickItem` puts the character into
// AS_PICK_ITEM_CHASE (:443-491), and that state issues ONE move to
// `GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)` and picks the item up when the
// body is near it and the walk is over (PlayerChar_FSM.cpp:157-190).
//
// EACH TEST IS ONE OF THE CARD'S PASS CONDITIONS, and each drives the REAL Reply
// dispatch and the REAL chase tick - never a helper that calls approachPick directly.

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"
)

// idleTicks runs the session's own chase tick n times without moving any clock, and
// answers every packet those ticks produced. It is the "unchanged stand point" half of
// the re-order condition: the body walks, the item does not move, and a tick must have
// nothing to say.
func idleTicks(h *Handler, sess *Session, n int) [][]byte {
	var out [][]byte
	for i := 0; i < n; i++ {
		out = append(out, h.chaseTick(sess)...)
	}
	return out
}

// bringTheWalkHome says the body has covered its route, without waiting: the walk's own
// clock is moved back by its eta, which is exactly the point walk.arrived answers yes -
// the same trick skill_approach_test.go's walkFor uses, on the approach record.
func bringTheWalkHome(t *testing.T, sess *Session, idx int) *walk {
	t.Helper()
	w := sess.approachOf(idx)
	if w == nil {
		t.Fatalf("character %d owns no approach walk, so there is no chase to bring home", idx)
	}
	w.began = time.Now().Add(-(w.eta + 50*time.Millisecond))
	return w
}

// goalOf reads the destination out of a ZC_MOVE_PC (30 bytes, cmd/handle/tick/x/y/cell/
// speed/reason).
func goalOf(t *testing.T, pkt []byte) [3]int32 {
	t.Helper()
	if len(pkt) != 30 {
		t.Fatalf("the move is %d bytes, want 30 (ZC_MOVE_PC)", len(pkt))
	}
	return [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[10:])),
		int32(binary.LittleEndian.Uint32(pkt[14:])),
		int32(binary.LittleEndian.Uint32(pkt[18:])),
	}
}

// TestAPickupFromFarAwayWalksThenBagsTheItem is the card's first pass condition: a
// request at 486 units answers ONE move order toward the item and NO bag change; when
// the character arrives, the bag row appears and the ground item is gone.
func TestAPickupFromFarAwayWalksThenBagsTheItem(t *testing.T) {
	f := newPickupFixture(t)
	f.h.Plan = directPlan
	it := f.item
	charHandle := f.picker.sess.Team[0].Handle

	// 486 UNITS: the owner's own refused distance, a literal so a shrunken reach cannot
	// quietly make this test measure the in-reach path.
	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	// ONE MOVE ORDER, AND NOTHING ELSE.
	if len(out) != 1 || cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("a pickup at 486 units answered %v, want exactly one %d (ZC_MOVE_PC) - "+
			"the character WALKS to the item:\n%s", cmdsOf(out), CmdZCMovePC, f.logs.String())
	}
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != charHandle {
		t.Fatalf("the move names handle %d, want the PICKING CHARACTER %d", got, charHandle)
	}
	if got := binary.LittleEndian.Uint32(out[0][26:]); got != ReasonPick {
		t.Fatalf("the move's reason is %d, want %d (MR_PICK - the reference's own word "+
			"for a pickup chase, PlayerChar.cpp:418-419)", got, ReasonPick)
	}
	goal := goalOf(t, out[0])
	d := math.Hypot(float64(goal[0]-it.Pos[0]), float64(goal[1]-it.Pos[1]))
	if math.Abs(d-pickChaseStandoff) > 2 {
		t.Fatalf("the walk's stand point %v is %.1f units from the item, want %.0f "+
			"(GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2) = the item's 50 + 15)",
			goal, d, float64(pickChaseStandoff))
	}
	if d >= 486 {
		t.Fatalf("the stand point is %.1f units from the item - it is not closer than the "+
			"press was (486)", d)
	}

	// NO BAG CHANGE, AND THE ITEM STAYS: the walk is not a pickup.
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("a walk-only request wrote to the bag: ops %+v stacks %+v", f.bag.ops,
			f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("the item left the ground before the character arrived")
	}
	if sent := f.picker.conn.sent(); len(sent) != 0 {
		t.Fatalf("the walk sent the picker %v, want nothing (the move is returned, the "+
			"world news waits for the arrival)", cmdsOf(sent))
	}
	text := f.logs.String()
	if !strings.Contains(text, "PICKITEM CHASE: char 0") {
		t.Fatalf("no chase line:\n%s", text)
	}
	if !strings.Contains(text, "at 486 units") {
		t.Fatalf("the chase line does not name the press distance:\n%s", text)
	}
	if strings.Contains(text, "PICKITEM refused") {
		t.Fatalf("a walkable item was REFUSED:\n%s", text)
	}

	// THE ARRIVAL: the body has covered its route, so the chase fires the pickup.
	w := bringTheWalkHome(t, f.picker.sess, 0)
	if w.pick == nil {
		t.Fatalf("the walk carries no pending pickup:\n%s", f.logs.String())
	}
	out = f.h.chaseTick(f.picker.sess)

	if cmdCount(out, CmdZCAddItem) != 1 {
		t.Fatalf("the arrival returned %v, want the one 3607 (ZC_ADD_ITEM):\n%s",
			cmdsOf(out), f.logs.String())
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].ClassID != 30000 || f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag holds %+v, want one row of class 30000 x3", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); still {
		t.Fatal("the item is still on the ground after the chase arrived")
	}
	sent := f.picker.conn.sent()
	if len(sent) != 2 || cmdOf(sent[0]) != CmdZCPickItem || cmdOf(sent[1]) != CmdZCLeaveItem {
		t.Fatalf("the picker's socket got %v, want the 3555 then the 3539:\n%s",
			cmdsOf(sent), f.logs.String())
	}
	text = f.logs.String()
	if !strings.Contains(text, "PICKITEM CHASE LANDED") {
		t.Fatalf("the landing wrote no line:\n%s", text)
	}
	if !strings.Contains(text, "zone: PICKITEM commander 24 char 0") {
		t.Fatalf("the pickup transaction did not run:\n%s", text)
	}
	// AND THE WALK IS OVER: no second answer on the next tick.
	if again := f.h.chaseTick(f.picker.sess); cmdCount(again, CmdZCAddItem) != 0 {
		t.Fatalf("the tick after the arrival picked the item up AGAIN: %v", cmdsOf(again))
	}
	if len(f.bag.stacks) != 1 || f.bag.stacks[0].Count != 3 {
		t.Fatalf("the bag was written twice: %+v", f.bag.stacks)
	}
}

// TestThePickupChaseOrdersOneWalkForTenTicks is the card's second pass condition: ten
// ticks with an unchanged stand point produce exactly ONE move order.
//
// THE COMPARISON IS THE TOTAL: the press's own order plus every tick's. The item cannot
// move and the walk is in flight, so the reference's chase returns on
// `MS_MOVE == GetMoveState()` (PlayerChar_FSM.cpp:166-167) and this side has nothing to
// order - a re-order per tick is the storm that made an attack walk "teleport" the body.
func TestThePickupChaseOrdersOneWalkForTenTicks(t *testing.T) {
	f := newPickupFixture(t)
	f.h.Plan = directPlan
	it := f.item

	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)

	orders := cmdCount(pickupOnce(t, f.h, f.picker.sess, it.Handle), CmdZCMovePC)
	if orders != 1 {
		t.Fatalf("the press ordered %d walk(s), want 1", orders)
	}

	const ticks = 10
	orders += cmdCount(idleTicks(f.h, f.picker.sess, ticks), CmdZCMovePC)
	if orders != 1 {
		t.Fatalf("%d ticks with an unchanged stand point produced %d walk order(s) in "+
			"total, want exactly 1:\n%s", ticks, orders, f.logs.String())
	}
	// THE CHASE IS STILL ALIVE, so the silence is "nothing to do", not "the chase died".
	w := f.picker.sess.approachOf(0)
	if w == nil || w.pick == nil || w.castDone {
		t.Fatalf("the chase ended during the ticks:\n%s", f.logs.String())
	}
	// AND THE SECOND REQUEST THE CLIENT SENDS (four in half a second in the owner's log)
	// IS ABSORBED: nothing is sent and the walk keeps its arrival.
	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); len(out) != 0 {
		t.Fatalf("a repeat request for the item already being walked to answered %v, "+
			"want nothing", cmdsOf(out))
	}
	if w := f.picker.sess.approachOf(0); w == nil || w.pick == nil {
		t.Fatalf("the repeat request dropped the chase:\n%s", f.logs.String())
	}
}

// TestAPlayerMoveOrderCancelsThePickupChase is the card's third pass condition: a player
// move order during the chase clears it, and no pickup happens later.
func TestAPlayerMoveOrderCancelsThePickupChase(t *testing.T) {
	f := newPickupFixture(t)
	f.h.Plan = directPlan
	it := f.item

	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)
	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); cmdCount(out, CmdZCMovePC) != 1 {
		t.Fatalf("the chase did not start: %v\n%s", cmdsOf(out), f.logs.String())
	}
	w := f.picker.sess.approachOf(0)
	if w == nil || w.pick == nil {
		t.Fatalf("no pending pickup to cancel:\n%s", f.logs.String())
	}

	// THE PLAYER'S OWN ORDER: a CZ_MOVE_PC for the walking character, pointing somewhere
	// else - the same click that cancels an attack's approach.
	elsewhere := [3]int32{it.Pos[0] + 900, it.Pos[1] + 300, it.Pos[2]}
	if _, err := f.h.movePC(f.picker.sess, moveFrame(0, elsewhere)); err != nil {
		t.Fatalf("movePC: %v", err)
	}

	if cur := f.picker.sess.approachOf(0); cur != nil {
		t.Fatalf("the pickup chase survived the player's move order (%+v):\n%s",
			cur.describe(), f.logs.String())
	}
	if !strings.Contains(f.logs.String(), "PICKITEM CHASE CANCELLED") {
		t.Fatalf("the cancel wrote no line:\n%s", f.logs.String())
	}

	// NOTHING IS PICKED UP LATER, however long the character walks now.
	idleTicks(f.h, f.picker.sess, 10)
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("a cancelled chase still wrote to the bag: %+v", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("a cancelled chase took the item off the ground")
	}
	if got := strings.Count(f.logs.String(), "PICKITEM commander 24"); got != 0 {
		t.Fatalf("the cancelled chase ran the pickup transaction %d time(s):\n%s", got,
			f.logs.String())
	}
}

// TestAPickupChaseDropsTheItemThatVanishes: the item leaves the ground mid-walk, so the
// pending pickup is dropped with ONE line, no packet, and the walk's arrival opens
// nothing at all - a pickup walk must never fall through to the click's dialog path.
func TestAPickupChaseDropsTheItemThatVanishes(t *testing.T) {
	f := newPickupFixture(t)
	f.h.Plan = directPlan
	it := f.item

	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)
	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); cmdCount(out, CmdZCMovePC) != 1 {
		t.Fatalf("the chase did not start: %v", cmdsOf(out))
	}

	if _, ok := f.h.claimGroundItem(it.Handle); !ok {
		t.Fatal("the fixture's item was not in the registry")
	}
	before := len(f.picker.conn.sent())
	if out := idleTicks(f.h, f.picker.sess, 1); len(out) != 0 {
		t.Fatalf("the vanished item produced %v, want nothing", cmdsOf(out))
	}
	if n := strings.Count(f.logs.String(), "PICKITEM CHASE DROPPED"); n != 1 {
		t.Fatalf("%d drop line(s), want exactly 1:\n%s", n, f.logs.String())
	}
	if w := f.picker.sess.approachOf(0); w == nil || w.pick == nil || !w.castDone {
		t.Fatalf("the pending pickup was not dropped:\n%s", f.logs.String())
	}

	// THE WALK STILL ARRIVES, AND NOTHING COMES OF IT: no bag row, no pickup, and above
	// all no NPC dialog - the walk record is not a click.
	bringTheWalkHome(t, f.picker.sess, 0)
	out := f.h.chaseTick(f.picker.sess)
	if cmdCount(out, CmdZCAddItem) != 0 {
		t.Fatalf("the arrival of a dropped chase answered %v", cmdsOf(out))
	}
	if len(f.bag.stacks) != 0 {
		t.Fatalf("the bag holds %+v after a dropped chase", f.bag.stacks)
	}
	for _, p := range f.picker.conn.sent()[before:] {
		if cmdOf(p) == CmdZCDialogOK || cmdOf(p) == CmdZCDialogSel {
			t.Fatalf("a pickup walk opened an NPC dialog:\n%s", f.logs.String())
		}
	}
}

// TestANewPickupReplacesThePendingTarget: the reference keeps ONE pending pickup
// (m_pickItemTarget), so a request for a DIFFERENT item replaces the chase instead of
// queueing behind it.
func TestANewPickupReplacesThePendingTarget(t *testing.T) {
	f := newPickupFixture(t)
	f.h.Plan = directPlan
	it := f.item

	// A SECOND item, 300 units north of the first: far enough that the stand point it
	// asks for is a different point, near enough that one click resolves it.
	second := groundItem{
		Handle: it.Handle + 1, ItemID: it.ItemID, Count: 1,
		Pos: [3]int32{it.Pos[0], it.Pos[1] + 300, it.Pos[2]},
	}
	f.h.recordGroundItem(second)

	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)
	if out := pickupOnce(t, f.h, f.picker.sess, it.Handle); cmdCount(out, CmdZCMovePC) != 1 {
		t.Fatalf("the first chase did not start: %v", cmdsOf(out))
	}

	out := pickupOnce(t, f.h, f.picker.sess, second.Handle)
	if cmdCount(out, CmdZCMovePC) != 1 {
		t.Fatalf("the second item answered %v, want ONE new walk:\n%s", cmdsOf(out),
			f.logs.String())
	}
	w := f.picker.sess.approachOf(0)
	if w == nil || w.pick == nil || w.pick.item != second.Handle {
		t.Fatalf("the pending pickup names %+v, want item 0x%08x (the NEW one)",
			w, second.Handle)
	}
	if !strings.Contains(f.logs.String(), "REPLACES the chase") {
		t.Fatalf("the replacement wrote no line:\n%s", f.logs.String())
	}

	// AND THE NEW CHASE FINISHES ON THE NEW ITEM.
	bringTheWalkHome(t, f.picker.sess, 0)
	if out := f.h.chaseTick(f.picker.sess); cmdCount(out, CmdZCAddItem) != 1 {
		t.Fatalf("the replaced chase did not pick the new item up: %v\n%s", cmdsOf(out),
			f.logs.String())
	}
	if _, still := f.h.groundItemAt(second.Handle); still {
		t.Fatal("the new item is still on the ground")
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("the FIRST item was taken by a chase that had been replaced")
	}
}

// TestAPickupWithNoPathStillRefuses is the one case the card keeps as a refusal: an item
// no walk can be planned to. Nothing is ordered, nothing is bagged, and the line is the
// reach refusal this file has always written - with the reason no walk exists on it.
func TestAPickupWithNoPathStillRefuses(t *testing.T) {
	f := newPickupFixture(t)
	it := f.item
	// NO path engine on this handler: pickChaseStandoff cannot be proved to be ground.
	far := [3]int32{it.Pos[0] + 486, it.Pos[1], it.Pos[2]}
	cullPlace(f.h, f.picker, far)

	out := pickupOnce(t, f.h, f.picker.sess, it.Handle)

	if len(out) != 0 || len(f.picker.conn.sent()) != 0 {
		t.Fatalf("a pickup with no path engine answered %v / %v, want nothing",
			cmdsOf(out), cmdsOf(f.picker.conn.sent()))
	}
	if len(f.bag.ops) != 0 || len(f.bag.stacks) != 0 {
		t.Fatalf("a refused pickup wrote to the bag: %+v", f.bag.stacks)
	}
	if _, still := f.h.groundItemAt(it.Handle); !still {
		t.Fatal("the item left the ground for a pickup that could not be walked")
	}
	text := f.logs.String()
	if !strings.Contains(text, "stands 486 units") ||
		!strings.Contains(text, "NO WALK to it can be planned") {
		t.Fatalf("the refusal does not name the distance and the reason:\n%s", text)
	}
	if strings.Contains(text, "PICKITEM CHASE char") {
		t.Fatalf("a chase line was written for a walk that does not exist:\n%s", text)
	}
}

// TestThePickupChaseIsTheReferenceLayout pins the numbers the chase puts on the wire
// against their sources, so a later edit cannot drift them one at a time.
func TestThePickupChaseIsTheReferenceLayout(t *testing.T) {
	if pickupReach != 130 {
		t.Fatalf("pickupReach = %g, want 130", float64(pickupReach))
	}
	if pickChaseStandoff != 65 {
		t.Fatalf("pickChaseStandoff = %g, want 65 = the item's ST_SMALL 50 + "+
			"DISTANCE_TO_ITEM/2 15 (FieldObject.cpp:128-145)", float64(pickChaseStandoff))
	}
	if ReasonPick != 3 {
		t.Fatalf("ReasonPick = %d, want 3 = MR_PICK in enum MOVE_REASON "+
			"(geShared.h:1347-1353)", ReasonPick)
	}
	// AND THE FRAME THE CHASE REPLAYS IS THE ONE THE CLIENT SENT: a pending pickup built
	// from a real request carries the same 15 bytes through the arrival.
	frame := pickFrame(0, 0x30000002)
	if len(frame) != czPickItemSize {
		t.Fatalf("the fixture's frame is %d bytes, want %d", len(frame), czPickItemSize)
	}
	req, ok := ParsePickItem(frame)
	if !ok || req.Item != 0x30000002 {
		t.Fatalf("ParsePickItem answered %+v, ok=%v", req, ok)
	}
}
