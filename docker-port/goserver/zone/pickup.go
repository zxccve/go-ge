package zone

// PICKUP: an item on the ground goes into the bag (R98 slice 4).
//
// THE SPEC IS the reference's own pick path, read end to end:
//
//	CCommander::OnPickItem         Commander_Packet.cpp:904-921   the frame arrives
//	CPlayerChar::RequestPickItem   PlayerChar_Request.cpp:187-191 the request
//	CPlayerChar::DispatchPickItem  PlayerChar_Request.cpp:443-491 reach, owner, quest
//	CTxPickItem::Do / Update       TxPickItem.cpp:107-258         bag, packets, log
//
// AND THE ONE DETAIL THAT DECIDES THE PACKET ORDER, taken from the CLIENT's own
// handler (GENormalNet_Action.cpp:86-118): ZC_PICKITEM (3555) is answered by looking
// up the PICKING ACTOR by `handle` and the item by `itemHandle`, and if the item is
// already gone the client plays NO pickup animation at all - while if the actor is
// not one of its own characters a FINAL_RELEASE build does nothing whatsoever (not
// even the item's removal). So the 3555 carries the PICKING CHARACTER's handle, and
// it goes out BEFORE the 3539 that takes the item off the ground.
//
// THE REACH IS THE REFERENCE'S OWN ARITHMETIC, not a number chosen here:
//
//	IsNearObject(item, DISTANCE_TO_ITEM)     FieldObject.cpp:120-126
//	= GetDistance(myPos, itemPos) - (myRadius + itemRadius) <= 30
//
// `GetDistance` is X and Y only, no height (Common.cpp:39-45), and
// DISTANCE_TO_ITEM is 30 (FieldObject.h:17). The ITEM's radius is certain -
// `CFieldItem::GetDomainRadius` answers `geShapeTable::ST_SMALL` (FieldItem.cpp:41-44)
// which is 50 (geShapeTable.h:10). The CHARACTER's radius is `m_shapeSize`, the shape
// table's own `size` column (FieldChar.cpp:225), and a player character IS the S body:
// 50 (`PCBodyRadius`, create_tables.go, read from the same shape rows the death path
// already loads). The three terms are named in the pickupReach constant below and the
// sum is 30 + 50 + 50 = 130 units.
//
// THE 80 THIS FILE USED TO CARRY WAS WRONG, AND THE OWNER'S OWN LOG PROVED IT: nine
// PICKITEMs were refused at 124, 107 and 96 units on 2026-11-08 16:04 because the
// character's own 50 was left out of the sum. The client does not only pick at its
// feet - the frame arrives from where the body stands - so the reference's own 130 is
// the number, not a tighter guess.
//
// THE BODY POINT, NOT THE DESTINATION (R135d). The reach is measured from
// `sessionBodyAt`, the same reader the sight ring and the attack swing use: a body
// mid-walk is measured where it is DRAWN, never at the click target it is walking
// towards.
//
// OUT OF REACH IS A WALK, NOT A REFUSAL (2026-11-08). The owner clicked an item from
// far away and the character did not move; the zone log held four refusals in half a
// second, at 486 and 456 units, and then a linchpin take at 69. The CLIENT was doing
// its job - it re-asks while the player holds the click - and this zone was waiting for
// a walk nobody had ordered. The reference does not wait:
//
//	CCommander::OnPickItem         Commander_Packet.cpp:904-921   the frame arrives
//	RequestPickItem                PlayerChar_Request.cpp:187-191 RECORDS the request
//	                               (a QUEUED action, not a transaction)
//	DispatchPickItem               PlayerChar_Request.cpp:443-491 IsPickable, then
//	                                 m_pickItemTarget = target;
//	                                 SetMoveState(MS_STOP);
//	                                 SetActionState(AS_PICK_ITEM_CHASE);
//	AS_PICK_ITEM_CHASE             PlayerChar_FSM.cpp:157-190     ONE walk to
//	                               GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)
//	                               (65 units from the item's centre), then, once the
//	                               move state is no longer MS_MOVE, IsNearObject ->
//	                               PickItem.
//
// THIS FILE IS THAT PATH IN OUR TERMS: an out-of-reach 3611 becomes a pending pickup on
// the character (a walk record, the same one an attack's approach uses), the walk is
// ordered ONCE at the reference's own stand point with MR_PICK, and the arrival replays
// the client's own 3611 through THIS handler - so the pickup transaction, its ownership
// rule, its bag write and its packets are the same code, not a copy. The reference's
// `MS_MOVE` gate is what keeps the tick from ordering a walk per pass: an unchanged
// stand point is never re-ordered (the item cannot move), and a second request for the
// SAME item is absorbed.
//
// OWNERSHIP (slice 2, DELIBERATE AND NAMED): ANYONE IN REACH. The reference gives the
// drop to the highest-damage commander: the 3536 carries that commander's name and
// `IsPickable` refuses everybody else (MonsterDropItem.cpp:88-102,
// FieldItem.cpp:236-245). THIS ZONE'S 3536 CARRIES AN EMPTY commanderName AND squadID
// 0 (zone\drop.go, BuildSpawnStandingItem: there is no per-attacker damage list), and
// for exactly that state the reference's `IsPickable` answers TRUE for every commander
// - the client's own `IsEnablePickItem` does too (GeBaseObject.cpp:1375-1389). So a
// priority check on this side would refuse a pickup the client was told it may make,
// with no window and no message to explain it. "Anyone in reach" is therefore what
// this side's own wire already says; the first successful pickup prints that sentence
// once. `groundItem.Killer` is RECORDED AND NOT ENFORCED - it is what a later priority
// slice needs, nothing more.
//
// WHAT IS NOT HERE, ON PURPOSE: the instant-item branch (an `Instant` class is used
// on the spot instead of bagged, TxPickItem.cpp:131-142), the quest-item unstack
// guard (:474-481), the squad routing (:930-941), the 60 s ground-item lifetime
// (FieldItem.cpp:169) and ZC_ENTER_STANDINGITEM (3537) for a player who arrives after
// the drop. Each is a later slice, and none of them is faked here.

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"geserver/item"
	"geserver/protocol"
)

const (
	// CmdCZPickItem is PKS_CZ_PICKITEM, from the CLIENT's own table
	// (data\cmd_table_live_z.txt:1278 - 15 bytes).
	CmdCZPickItem  = 3611
	czPickItemSize = 15

	// CmdZCLeaveItem is PKS_ZC_LEAVE_ITEM: `handle` ONLY (geZCPacket.h:1186-1188),
	// 6 bytes (data\cmd_table_live_z.txt:1206). Unlike ZC_LEAVE_MONSTER (3531) there
	// is no reason word and no position: the object is simply gone.
	CmdZCLeaveItem = 3539
	LeaveItemSize  = 6

	// CmdZCPickItem is PKS_ZC_PICKITEM: `handle` (the picking CHARACTER) +
	// `itemHandle` (geZCPacket.h:2004-2007), 10 bytes
	// (data\cmd_table_live_z.txt:1222).
	CmdZCPickItem = 3555
	PickItemSize  = 10

	// The three fields of CZ_PICKITEM after the security header. The header itself
	// is PKS_SECURITY_DEFAULT - cmd(2) seq(4) checksum(4) - which is the same ten
	// bytes CZ_ATTACK has, and that is why czChecksum reads this frame unchanged.
	czpChecksum  = 6  // int32 - the client's own GetChecksum() over the packet
	czpCharIndex = 10 // BYTE - the picking character's slot in THIS session's team
	czpItemID    = 11 // OBJHANDLE - the ground item (geZCPacket.h:1196-1199)

	// distanceToItem is DISTANCE_TO_ITEM - the gap `IsNearObject` allows between the
	// two BODIES' edges (FieldObject.h:17; the comparison is FieldObject.cpp:120-126).
	distanceToItem = 30.0

	// itemBodyRadius is the field item's own domain radius: `CFieldItem::GetDomainRadius`
	// answers `geShapeTable::ST_SMALL` (FieldItem.cpp:41-44), whose body radius is 50
	// (geShapeTable.h:10; the same four rows shapeBodyRadius reads in create_tables.go).
	itemBodyRadius = 50.0

	// pickupReach is the reference's own THREE-term sum, one term per line:
	//
	//	DISTANCE_TO_ITEM            30  (FieldObject.h:17)
	//	PCBodyRadius                50  (the PICKER's own S body, create_tables.go)
	//	the item's ST_SMALL radius  50  (FieldItem.cpp:41-44, geShapeTable.h:10)
	//
	// `IsNearObject` asks GetDistance(myPos, itemPos) - (myRadius + itemRadius) <= 30,
	// so a player character may pick at 130 units between centres. The picker here is
	// ALWAYS a player character - CZ_PICKITEM names a slot in this session's team - so
	// PCBodyRadius is the right half; a monster picker would measure with
	// monsterBodyRadius(mon), and no frame sends that today.
	pickupReach = distanceToItem + PCBodyRadius + itemBodyRadius

	// ReasonPick is MR_PICK (3): the reason word the reference puts on the move a
	// PICKUP CHASE starts - `case AS_PICK_ITEM_CHASE: packet.reason = MR_PICK;`
	// (PlayerChar.cpp:418-419), out of `enum MOVE_REASON { MR_MOVE, MR_ATTACK,
	// MR_SKILL, MR_PICK, MR_DIALOG }` (geShared.h:1347-1353). The client stores it and
	// its own state machine reads it (zone\move.go).
	ReasonPick = 3

	// pickChaseStandoff is the point the chase walks to, measured from the ITEM's
	// CENTRE - the reference's own `GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)`
	// (PlayerChar_FSM.cpp:170-174), whose body is
	// `int radius = GetDomainRadius() + dist` (FieldObject.cpp:128-145). The item's
	// domain is its ST_SMALL 50 and dist is DISTANCE_TO_ITEM/2 = 15, so the stand point
	// is 65 units from the item, on the character's side - well inside the 130 reach,
	// which is why the pickup fires by the time that walk ends.
	pickChaseStandoff = itemBodyRadius + distanceToItem/2
)

// groundItem is ONE field item this zone created and nobody has taken yet.
//
// IT IS THE REGISTRY THE 3536 NEVER HAD. Until this slice the drop built the frame
// and forgot the item, so a 3611 had no handle to resolve and 3537 (the frame a LATER
// arrival needs) had no source. Nothing here is persisted: a ground item lives as long
// as this process, exactly as the reference's field objects are world state and not
// database rows.
type groundItem struct {
	// Handle is the 3536's handle - what a CZ_PICKITEM names and what the 3539 and
	// 3555 carry back.
	Handle uint32
	// ItemID is the dropped CLASS id: the 3536's appearance.Type, and the class the
	// bag row is written with.
	ItemID uint32
	// Count is how many units THIS one field item holds - the 3536's
	// appearance.Count after the drop's UNSTACK split, never the total of a stack
	// that was split into pieces.
	Count int
	// Pos is where it lies: the corpse's point, and the point the reach is measured
	// to. All three words are kept because a later slice's 3537 needs the whole
	// WORLDPOS.
	Pos [3]int32
	// Parent is the body it came out of - the 3536's appearance.parent.
	Parent uint32
	// Killer is the CHARACTER handle whose damage took the body's last HP. It is
	// RECORDED AND NOT ENFORCED: the rule this slice applies is "anyone in reach"
	// (see the file header), which is what this zone's own 3536 tells the client. A
	// later priority slice reads this; nothing refuses on it today.
	Killer uint32
}

// recordGroundItem puts one field item in the registry, keyed by its handle.
//
// It shares spawnMu with the handle counter (nextItemHandle) and the monster roster,
// because a kill arms a drop on one session's goroutine while another session can
// send CZ_PICKITEM at the same instant.
func (h *Handler) recordGroundItem(it groundItem) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if h.groundItems == nil {
		h.groundItems = make(map[uint32]groundItem, 8)
	}
	h.groundItems[it.Handle] = it
}

// groundItemAt answers the item a handle names, WITHOUT taking it.
//
// The refusals (not here, out of reach) must leave the item exactly where it is, so
// they read through this and never through claimGroundItem.
func (h *Handler) groundItemAt(handle uint32) (groundItem, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	it, ok := h.groundItems[handle]
	return it, ok
}

// claimGroundItem removes the item and answers it: the FIRST caller for a handle owns
// it and every later one is told it is not there.
//
// THAT IS WHAT MAKES THE PICKUP IDEMPOTENT. A double click, a second client or a
// forged frame that repeats a handle cannot put the item in two bags, because the
// item IS the claim - there is no "was it taken?" flag to fall out of step with the
// world. A claim that is followed by a FAILED bag insert is restored by
// restoreGroundItem, so a refusal still leaves the item on the floor.
func (h *Handler) claimGroundItem(handle uint32) (groundItem, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	it, ok := h.groundItems[handle]
	if !ok {
		return groundItem{}, false
	}
	delete(h.groundItems, handle)
	return it, true
}

// restoreGroundItem puts back an item whose bag insert failed, under its ORIGINAL
// handle: the client never heard anything about it (no 3555, no 3539), so the item it
// still draws is the item this puts back.
func (h *Handler) restoreGroundItem(it groundItem) {
	h.recordGroundItem(it)
}

// BuildLeaveItem packs PKS_ZC_LEAVE_ITEM (3539), 6 bytes: cmd(2) + handle(4).
//
// The client's handler takes the object out of its world by that handle alone - there
// is no code word (the one ZC_LEAVE_MONSTER carries) and no position.
func BuildLeaveItem(handle uint32) []byte {
	out := make([]byte, LeaveItemSize)
	binary.LittleEndian.PutUint16(out[0:], uint16(CmdZCLeaveItem))
	binary.LittleEndian.PutUint32(out[2:], handle)
	return out
}

// BuildPickItem packs PKS_ZC_PICKITEM (3555), 10 bytes:
//
//	cmd(2) + handle(4) + itemHandle(4)
//
// `charHandle` is the PICKING CHARACTER, never the commander and never the session:
// the client looks the actor up by it and plays that actor's pickup animation
// (GENormalNet_Action.cpp:93-98). `itemHandle` is the ground item, and the client
// wants it still in the world - see the file header on the packet order.
func BuildPickItem(charHandle, itemHandle uint32) []byte {
	out := make([]byte, PickItemSize)
	binary.LittleEndian.PutUint16(out[0:], uint16(CmdZCPickItem))
	binary.LittleEndian.PutUint32(out[2:], charHandle)
	binary.LittleEndian.PutUint32(out[6:], itemHandle)
	return out
}

// PickRequest is one decoded CZ_PICKITEM (3611).
type PickRequest struct {
	// CharIndex is the picking character's slot in THIS session's team - the same
	// field CZ_ATTACK and CZ_CHANGE_STANCE carry, and the same field the reference
	// indexes m_pcList with (Commander_Packet.cpp:913-917).
	CharIndex int
	// Item is the ground item's handle. It is only trusted after groundItemAt finds
	// it among the items THIS zone created.
	Item uint32
	// Checksum is the DWORD the client stored at +6 - its own GetChecksum() over
	// the packet. czChecksum is what this zone does with it, and the answer is the
	// one CZ_ATTACK already gives: log a mismatch, accept the frame.
	Checksum uint32
}

// ParsePickItem reads CZ_PICKITEM:
//
//	+0 cmd(2)=3611  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 itemID(4)
//
// ok is false when the frame is not exactly the 15 bytes the client's table
// registers. EXACT, not "at least", for the reason ParseAttack gives: 3611 is a
// FIXED-size packet, the splitter cuts it at the declared size, and a frame of
// another length means the table and the bytes disagree.
func ParsePickItem(pt []byte) (PickRequest, bool) {
	var req PickRequest
	if len(pt) != czPickItemSize {
		return req, false
	}
	req.CharIndex = int(pt[czpCharIndex])
	req.Item = binary.LittleEndian.Uint32(pt[czpItemID:])
	req.Checksum = binary.LittleEndian.Uint32(pt[czpChecksum:])
	return req, true
}

// pickItem answers CZ_PICKITEM (3611).
//
// THE REFUSALS COME FIRST, EACH WITH ONE LINE AND NOTHING SENT, in this order: a
// frame that is not 15 bytes, a character index that is not this team, a handle that
// is not a ground item HERE, a body out of reach, and a bag insert the write path
// refused. Only then does anything leave the zone.
//
// THE ANSWER, in the order the reference sends it and the client needs it:
//
//	3555 ZC_PICKITEM   the picking character + the item, to the ring, BEFORE the leave
//	3539 ZC_LEAVE_ITEM the item, to the same ring (ViewRange outside, the R135e
//	                   middle-range cull inside - broadcastDropAt, the fan-out the
//	                   3536 itself went out through)
//	3607 ZC_ADD_ITEM   the bag row, to the PICKER ONLY, with addType ADD_ITEM_PICK
//
// THE 3607 IS RETURNED, NOT FANNED OUT: it is about one commander's bag, and the
// caller hands the returned slice to that session's writer. The 3555/3539 are world
// news and go through the fan-out, which is why the picker receives them from there
// rather than twice.
func (h *Handler) pickItem(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParsePickItem(f.Data)
	if !ok {
		h.Log.Printf("zone: PICKITEM refused: %d-byte frame, CZ_PICKITEM 3611 is a "+
			"fixed %d bytes - nothing sent", len(f.Data), czPickItemSize)
		return nil, nil
	}

	// The checksum verdict rides on EVERY line below, word for word as CZ_ATTACK
	// states it, so one line of the log answers "was it validated?" for either
	// frame. A mismatch is ACCEPTED for the reason attack.go gives: no sample shows
	// the reference refusing one, and the client computes it itself.
	stated := req.Checksum
	recomputed := czChecksum(f.Data)
	checksum := "checksum " + strconv.FormatUint(uint64(stated), 10) + " ok"
	if stated != recomputed {
		checksum = "checksum MISMATCH (stated " + strconv.FormatUint(uint64(stated), 10) +
			", recomputed " + strconv.FormatUint(uint64(recomputed), 10) +
			") ACCEPTED - no sample shows the reference refusing one"
	}

	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: PICKITEM refused: commander %d character index %d is not "+
			"in this team (%d member(s) spawned), %s - nothing sent",
			sess.CommanderID, req.CharIndex, len(sess.Team), checksum)
		return nil, nil
	}
	charHandle := sess.Team[req.CharIndex].Handle

	it, ok := h.groundItemAt(req.Item)
	if !ok {
		h.Log.Printf("zone: PICKITEM refused: handle 0x%08x is not a ground item this "+
			"zone is running (commander %d char %d handle %d), %s - nothing sent",
			req.Item, sess.CommanderID, req.CharIndex, charHandle, checksum)
		return nil, nil
	}

	// THE BODY, read through the same reader the ring and the swing use (R135d).
	now := time.Now()
	body := h.sessionBodyAt(sess, req.CharIndex, now)
	dist := math.Sqrt(distSq(body[0], body[1], it.Pos[0], it.Pos[1]))
	if dist > pickupReach {
		// OUT OF REACH IS NOT A REFUSAL ANY MORE: THE CHARACTER WALKS TO THE ITEM
		// (2026-11-08, the owner's "clicked a ground item from far away and the
		// character did not move"). The frame is the whole request - the reference
		// keeps it as a queued action and chases - so it is what the pending pickup
		// replays when the body arrives (firePickup below).
		return h.chasePickup(sess, f, req, charHandle, it, dist, checksum)
	}

	// THE OWNERSHIP RULE, said once per run (see the file header for why it is this
	// one and not AddItemPriority).
	h.dropSayOnce("pickup-owner",
		"zone: PICKITEM ownership: ANYONE IN REACH. This zone's 3536 carries an empty "+
			"commanderName and squadID 0, and the reference's own IsPickable answers "+
			"true for every commander in that state (FieldItem.cpp:236-245); the "+
			"highest-damage priority list (AddItemPriority, MonsterDropItem.cpp:88-102) "+
			"is a LATER slice and nothing is refused on it here. The killer is "+
			"recorded on each ground item and not enforced. This line is printed once.")

	if h.Commit == nil || sess.CommanderID == 0 {
		why := "this zone has no bag write path wired"
		if h.Commit != nil {
			why = "the session's commander is not resolved (0)"
		}
		h.Log.Printf("zone: PICKITEM refused: item 0x%08x cannot be bagged - %s for "+
			"commander %d; %s - the item stays",
			it.Handle, why, sess.CommanderID, checksum)
		return nil, nil
	}

	// THE CLAIM, BEFORE THE WRITE: two requests for one handle must not both reach
	// the bag. A refusal below restores it.
	claimed, ok := h.claimGroundItem(it.Handle)
	if !ok {
		h.Log.Printf("zone: PICKITEM refused: item 0x%08x was taken by another request "+
			"in the same instant, %s - nothing sent", it.Handle, checksum)
		return nil, nil
	}

	op := item.Op{Give: true, ClassID: int64(claimed.ItemID), Count: int64(claimed.Count)}
	if row, known := h.Items.Row(claimed.ItemID); known {
		op.Name = row.Name
	}
	// Merge/MaxStack are the ITEM's own word, from the same two sources the buy path
	// uses: the PRICE row carries MaxStack (a buy passes it straight into this op),
	// and the Item table carries `Unstack`. A price row wins, because that is what a
	// purchase of the same class would do; with neither, the give merges into the
	// oldest stack - the rule a script's ADD_ITEM already follows (zone\items.go).
	merge, maxStack, priced := true, int64(0), false
	if p, known := h.Prices.Get(int64(claimed.ItemID)); known {
		merge, maxStack, priced = p.MaxStack > 1, int64(p.MaxStack), true
	}
	if !priced {
		if row, known := h.Items.Row(claimed.ItemID); known {
			merge = !row.Unstack
		}
	}
	op.Merge, op.MaxStack = merge, maxStack

	// THE SAME DOOR A PURCHASE PAYS THROUGH: h.Commit applies the op in ONE database
	// transaction (item.writeItem -> giveItem/mintItem, zone\items.go). There is no
	// second insert in this file, and there must never be one.
	changes, err := h.Commit(ctx, sess.CommanderID, nil, []item.Op{op})
	if err != nil {
		h.restoreGroundItem(claimed)
		h.Log.Printf("zone: PICKITEM refused: commander %d's bag refused item %d x%d "+
			"(%v) - nothing was written, the item 0x%08x is back on the ground at "+
			"(%d, %d), nothing sent",
			sess.CommanderID, claimed.ItemID, claimed.Count, err, claimed.Handle,
			claimed.Pos[0], claimed.Pos[1])
		return nil, nil
	}

	var out [][]byte
	for _, c := range changes {
		if !c.Give {
			continue
		}
		// ADD_ITEM_PICK (3) is the reference's own type for a pickup
		// (TxPickItem.cpp:217) and the client prints its UI_ALERT_MSG_177 "obtained"
		// line for it - ADD_ITEM_BUY would say "purchased" (zone\inventory.go).
		if p := sess.Inv.PacketFor(c, AddItemPick, RemoveItemTake); p != nil {
			out = append(out, p)
		}
	}

	// THE WORLD NEWS, in this order: the pick first, so the client still HAS the item
	// when its 3555 handler looks it up (GENormalNet_Action.cpp:93-98), then the
	// leave. broadcastDropAt is the field-item ring fan-out - the outer ViewRange and
	// the R135e middle-range cull, the same two radii the 3536 went out through.
	seen := h.broadcastDropAt(BuildPickItem(charHandle, claimed.Handle),
		body[0], body[1])
	seen += h.broadcastDropAt(BuildLeaveItem(claimed.Handle),
		claimed.Pos[0], claimed.Pos[1])

	h.Log.Printf("zone: PICKITEM commander %d char %d (handle %d) took item 0x%08x "+
		"(class %d x%d) at %.0f units; cmd %d (%d B) + cmd %d (%d B) to %d "+
		"player(s), %d bag packet(s), killer %d (not enforced), %s",
		sess.CommanderID, req.CharIndex, charHandle, claimed.Handle, claimed.ItemID,
		claimed.Count, dist, CmdZCPickItem, PickItemSize, CmdZCLeaveItem,
		LeaveItemSize, seen, len(out), claimed.Killer, checksum)
	return out, nil
}

// =================================================================================
// THE PICKUP CHASE: AN OUT-OF-REACH CZ_PICKITEM WALKS TO THE ITEM
// =================================================================================
//
// THE REFERENCE'S OWN SHAPE, quoted in the file header: `RequestPickItem` only records
// the request (PlayerChar_Request.cpp:187-191), `DispatchPickItem` resolves the item and
// puts the character into AS_PICK_ITEM_CHASE (:443-491), and that state walks ONCE to
// `GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)` and picks the item up when the
// body is near it and the move is over (PlayerChar_FSM.cpp:157-190).
//
// EVERY PIECE BELOW IS THE APPROACH MACHINERY AN ATTACK ALREADY USES - a `walk` record
// with the request on it, `Handler.Plan`, `legMoveFor`'s one-walk-per-destination rule,
// `stepWalk`'s tick and `finishWalk`'s arrival - because the item chase and the attack
// chase are the same kind of thing and this package keeps one of those, not two.

// pendingPickup is one CZ_PICKITEM whose item was out of reach, kept while the character
// walks to it.
//
// THE FRAME IS THE REQUEST. The reference does not re-send anything to pick the item up:
// it holds the target and calls `PickItem` when the body gets there. This side replays
// the client's own 3611 through `pickItem` at that moment, so the transaction, the
// ownership say-once, the bag write and the three packets are the code that already
// exists - a replayed frame cannot drift from a live one.
type pendingPickup struct {
	frame    protocol.Frame // replayed verbatim at arrival: the handler parses it again
	item     uint32         // the ground item's handle, also in the frame
	pos      [3]int32       // where the item lay when the chase started (the landing line)
	dist     float64        // how far the press was made from, in units (the landing line)
	checksum string         // the press's checksum verdict, quoted by every line
	since    time.Time      // when the chase started

	// presses counts the requests this chase swallowed for the SAME item, and pressedAt
	// is when the last of them was written: the client re-asks while the player holds
	// the click (four in half a second in the owner's log), so one line per second
	// carries the count instead of four lines a second carrying nothing.
	presses   int
	pressedAt time.Time
}

// chasePickup turns an out-of-reach CZ_PICKITEM into a pending pickup and ONE walk to
// the reference's stand point. It answers that walk's packet, or nothing with one log
// line saying why nothing could be ordered.
//
// THE ONE CASE THAT IS STILL A REFUSAL is an item no walk can be planned to: no path
// engine, a position the mesh refuses, or an engine error. The line is the reach
// refusal this file has always written, so a genuinely unreachable item reads exactly as
// it did before this change.
func (h *Handler) chasePickup(sess *Session, f protocol.Frame, req PickRequest,
	charHandle uint32, it groundItem, dist float64, checksum string) ([][]byte, error) {
	const tag = "zone: PICKITEM CHASE"

	now := time.Now()

	// A CHASE ALREADY RUNNING FOR THIS CHARACTER IS NOT REPLACED BY ITS OWN REPEAT.
	// The reference keeps ONE pending pickup - `m_pickItemTarget = target`, assigned,
	// never queued (PlayerChar_Request.cpp:482) - and one walk at a time
	// (`if (MS_MOVE == GetMoveState()) return 0;`, PlayerChar_FSM.cpp:166-167). So a
	// repeat request for the item already being walked to sends nothing and keeps the
	// walk and its arrival.
	if cur := sess.approachOf(req.CharIndex); cur != nil && cur.pick != nil && !cur.castDone {
		if cur.pick.item == it.Handle {
			cur.pick.presses++
			if cur.pick.pressedAt.IsZero() || now.Sub(cur.pick.pressedAt) >= attackAbsorbPace {
				h.Log.Printf("%s: char %d (%d) asked for item 0x%08x again while it is "+
					"ALREADY being walked to (%d, %d, cell %d) - %d request(s) absorbed "+
					"since the last line, NOTHING sent: the reference keeps ONE pending "+
					"pickup (m_pickItemTarget, PlayerChar_Request.cpp:482) and one walk",
					tag, req.CharIndex, charHandle, it.Handle, cur.goal[0], cur.goal[1],
					cur.goal[2], cur.pick.presses)
				cur.pick.presses, cur.pick.pressedAt = 0, now
			}
			return nil, nil
		}
		h.Log.Printf("%s: char %d (%d): a request for item 0x%08x REPLACES the chase "+
			"for 0x%08x - the reference keeps one pending pickup, so the new item wins",
			tag, req.CharIndex, charHandle, it.Handle, cur.pick.item)
	}

	body := h.sessionBodyAt(sess, req.CharIndex, now)

	// THE STAND POINT NEEDS THE PATH ENGINE, for the same reason an attack's approach
	// does: a point this side can prove is ground, or an honest line. The goal itself is
	// the reference's `GetDomainOuterPos(m_pos, moveTo, DISTANCE_TO_ITEM/2)` - 65 units
	// from the item's centre on the character's side (pickChaseStandoff).
	if h.Plan == nil {
		h.refusePickup(req.CharIndex, charHandle, it, dist, checksum,
			"this zone has no path engine (Handler.Plan is nil)")
		return nil, nil
	}
	goal, route, routeLen, _, ok, err := h.Plan(body, it.Pos, pickChaseStandoff)
	switch {
	case err != nil:
		h.refusePickup(req.CharIndex, charHandle, it, dist, checksum,
			"the path engine failed ("+err.Error()+")")
		return nil, nil
	case !ok:
		h.refusePickup(req.CharIndex, charHandle, it, dist, checksum,
			"the character's own position is not valid ground to walk from")
		return nil, nil
	}

	speed := h.packetSpeed(&sess.Team[req.CharIndex])
	if speed <= 0 {
		speed = MoveBaseSpeed
	}
	if len(route) > 0 {
		routeLen = polylineLength(route)
	}
	walked := routeLen
	if walked <= 0 {
		walked = math.Hypot(float64(goal[0]-body[0]), float64(goal[1]-body[1]))
	}
	eta := time.Duration(walked / speed * float64(time.Second))

	pp := &pendingPickup{
		frame: f, item: it.Handle, pos: it.Pos, dist: dist,
		checksum: checksum, since: now, pressedAt: now,
	}
	installed := &walk{
		from: body, goal: goal, eta: eta, began: now, sentAt: now,
		speed: speed, caster: req.CharIndex, clamped: true, route: route,
		// THE TARGET AND ITS OWN BODY, so the walk record names what it is walking
		// towards. `near` stays 0: the reach this chase is judged by is the pickup's own
		// 130, asked on the body when the walk ends (firePickup).
		pick: pp, target: it.Pos, targetR: itemBodyRadius,
	}
	if cur := sess.approachOf(req.CharIndex); cur != nil {
		h.Log.Printf("%s: a new pickup replaces the walk to %s (%d, %d, cell %d) - this "+
			"character's own walk, and only its own", tag, cur.describe(), cur.goal[0],
			cur.goal[1], cur.goal[2])
	}
	sess.addApproach(installed)
	sess.rememberWalk(req.CharIndex, now)
	sess.closeWalk(req.CharIndex)

	pkt, handle := h.legMoveFor(sess, req.CharIndex, goal, ReasonPick)
	if pkt == nil {
		// ONE WALK PER DESTINATION IS ALREADY IN FLIGHT (legMoveFor's own rule): the
		// pending pickup still rides it, because the walk record is what the arrival is
		// read from. Nothing is sent for the duplicate.
		h.Log.Printf("%s: char %d (%d) -> item 0x%08x at %.0f units: the walk to (%d, "+
			"%d, cell %d) was ALREADY in flight, so nothing is sent - the pending pickup "+
			"rides that walk and runs when it arrives", tag, req.CharIndex, charHandle,
			it.Handle, dist, goal[0], goal[1], goal[2])
		return nil, nil
	}
	h.broadcastMove(sess, req.CharIndex, handle, goal, pkt)

	h.Log.Printf("%s: char %d (handle %d) -> item 0x%08x at %.0f units, past the "+
		"pickup reach %.0f, so the character WALKS to it - %.0f units to (%d, %d, cell "+
		"%d), eta %.1fs at %.0f u/s (cmd %d, %d B, reason %d MR_PICK, "+
		"PlayerChar.cpp:419); the pickup runs when the body arrives", tag,
		req.CharIndex, charHandle, it.Handle, dist, pickupReach, walked, goal[0], goal[1],
		goal[2], eta.Seconds(), speed, CmdZCMovePC, len(pkt), ReasonPick)
	return [][]byte{pkt}, nil
}

// pickChaseTick is the ONE thing a tick does for a pending pickup: it drops it the
// moment the item leaves the ground, silently (one line, no packet).
//
// IT ORDERS NOTHING, AND THAT IS THE REFERENCE'S OWN RULE: AS_PICK_ITEM_CHASE returns at
// once while `MS_MOVE == GetMoveState()` (PlayerChar_FSM.cpp:166-167), and the item
// cannot move, so the stand point cannot change and there is nothing to re-order. The
// walk the press ordered is the whole chase.
func (h *Handler) pickChaseTick(w *walk, now time.Time) [][]byte {
	if w.pick == nil || w.castDone {
		return nil
	}
	if _, ok := h.groundItemAt(w.pick.item); ok {
		return nil
	}
	w.castDone = true
	h.Log.Printf("zone: PICKITEM CHASE DROPPED: item 0x%08x left the ground %s into the "+
		"walk to it (another commander took it, or it left the field) - the pending "+
		"pickup is dropped and nothing is sent; the walk keeps only its stop clock",
		w.pick.item, now.Sub(w.pick.since).Round(time.Millisecond))
	return nil
}

// firePickup answers a pickup chase whose body has ARRIVED: it re-measures the reach on
// the drawn body and, when the body is inside it, replays the client's own 3611 through
// `pickItem` - THE transaction, unchanged, packets and all.
//
// A BODY THAT ARRIVED OUTSIDE THE REACH IS THE ONE HONEST REFUSAL LEFT: the walk ended
// and the item is still too far, which is a stand point this side got wrong or ground
// the client would not cross. The line is the same reach refusal a press gets, and the
// item stays where it is.
func (h *Handler) firePickup(sess *Session, w *walk, now time.Time) [][]byte {
	pp := w.pick
	if pp == nil || w.caster < 0 || w.caster >= len(sess.Team) {
		return nil
	}
	charHandle := sess.Team[w.caster].Handle
	it, ok := h.groundItemAt(pp.item)
	if !ok {
		h.Log.Printf("zone: PICKITEM CHASE DROPPED: item 0x%08x left the ground before the "+
			"character arrived (%s into the walk) - the pending pickup is dropped and "+
			"nothing is sent", pp.item, now.Sub(pp.since).Round(time.Millisecond))
		return nil
	}
	body := h.sessionBodyAt(sess, w.caster, now)
	dist := math.Sqrt(distSq(body[0], body[1], it.Pos[0], it.Pos[1]))
	if dist > pickupReach {
		h.refusePickup(w.caster, charHandle, it, dist, pp.checksum,
			"the walk ended at ("+strconv.Itoa(int(body[0]))+", "+
				strconv.Itoa(int(body[1]))+") and the item is still out of reach")
		return nil
	}
	h.Log.Printf("zone: PICKITEM CHASE LANDED: char %d (handle %d) left the press point "+
		"%.0f units from item 0x%08x at (%d, %d, cell %d) and walked %s to within %.0f "+
		"units of it, inside the pickup reach %.0f - the pickup runs now",
		w.caster, charHandle, pp.dist, it.Handle, pp.pos[0], pp.pos[1], pp.pos[2],
		now.Sub(pp.since).Round(time.Millisecond), dist, pickupReach)
	// THE CLIENT'S OWN FRAME, through the SAME handler: the reach this replay measures
	// is the one this function just measured (the body is at its goal, and legPoint
	// never draws past it), so it takes the in-reach branch and the transaction runs
	// exactly as a click's does.
	out, err := h.pickItem(context.Background(), sess, pp.frame)
	if err != nil {
		h.Log.Printf("zone: PICKITEM CHASE: the pickup of item 0x%08x failed after the "+
			"walk (%v) - nothing was written for it", it.Handle, err)
		return nil
	}
	return out
}

// cancelApproachPick is the pickup chase's half of the CANCEL WINDOW: a CZ_MOVE_PC from
// the character that is still walking to its item cancels the whole pending pickup,
// exactly as the same click cancels a normal attack's approach
// (zone\attack_approach.go, cancelApproachAttack).
//
// It answers true when it cancelled something, and the caller then carries on with the
// ordinary move: the character walks where the player pointed and no pickup follows.
func (h *Handler) cancelApproachPick(sess *Session, idx int) bool {
	w := sess.approachOf(idx)
	if w == nil || w.pick == nil || w.castDone {
		return false
	}
	if idx < 0 || idx >= len(sess.Team) {
		return false
	}
	elapsed := time.Since(w.began)
	h.Log.Printf("zone: PICKITEM CHASE CANCELLED by a move click (nothing sent) - char "+
		"%d (%d) was %s into the walk to (%d, %d, cell %d) for item 0x%08x, and the "+
		"click for the new point cancels it: no pickup follows",
		idx, sess.Team[idx].Handle, elapsed.Round(time.Millisecond), w.goal[0],
		w.goal[1], w.goal[2], w.pick.item)
	sess.clearApproach(w)
	// The click that cancelled this carries on as an ordinary move, and it opens its own
	// segment from where the body really stopped walking - recorded FIRST, exactly as
	// the attack's cancel and the cast's do (R91 round 7).
	sess.rememberWalk(idx, time.Now())
	sess.closeWalk(idx)
	return true
}

// refusePickup writes the ONE refusal line an item that cannot be walked to earns: the
// reach arithmetic this file has always quoted, plus the reason no walk exists when
// there is one. `cannot` empty is the plain out-of-reach case, byte for byte.
func (h *Handler) refusePickup(charIndex int, charHandle uint32, it groundItem,
	dist float64, checksum, cannot string) {
	tail := "- the item stays"
	if cannot != "" {
		tail = "- and NO WALK to it can be planned (" + cannot + "), so the item stays"
	}
	h.Log.Printf("zone: PICKITEM refused: char %d (handle %d) stands %.0f units from "+
		"item 0x%08x and the pickup reach is %.0f (DISTANCE_TO_ITEM %.0f + "+
		"PCBodyRadius %.0f + the item's ST_SMALL radius %.0f), %s %s",
		charIndex, charHandle, dist, it.Handle, pickupReach, distanceToItem,
		PCBodyRadius, itemBodyRadius, checksum, tail)
}
