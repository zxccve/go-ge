package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/item"
	"geserver/protocol"
	"geserver/world"
)

// THE GO BUTTON: CZ_COMMWARP_SELECT (3637) - travel to a saved point for ONE Warp Scroll.
//
// The layout pinned here is the client's own struct AND its own registered size:
//
//	source2\geShared\include\geZCPacket.h:3254
//	  struct PKS_CZ_COMMWARP_SELECT : public PKS_SECURITY_DEFAULT {
//	      BYTE index; BYTE teltype; bool isEvent; };
//	source2\geShared\include\gePacketBase.h:52  (#pragma pack(1))
//	  struct PKS_SECURITY_DEFAULT { WORD command; DWORD seq; int checksum; };
//	docker-port\data\cmd_table.txt:1112          3637  13  PKS_CZ_COMMWARP_SELECT
//
// so 10 + 1 + 1 + 1 = 13 bytes: index @+10, teltype @+11, isEvent @+12.
//
// WHAT THE CLIENT CHECKS BEFORE IT EVER SENDS THIS (ge\ui\uiscp\game.scp:174-196): a
// `WarpScroll` or a `WarpScroll_Event` in the bag, else MsgBox("<$>79767</>") and no
// packet at all. So from the client's side this request cannot be sent without a scroll;
// the SERVER's own check is a backstop, and these tests pin it.
//
// THE TWO CLASS IDS are the client's item table's own (ge\xml\datatable_item_etc.xml):
// 21006 "WarpScroll" and 21446 "WarpScroll_Event". `isEvent` is the client saying which
// one it means to spend (game.scp:188-191).
//
// NO REPLY PACKET EXISTS for 3637 and none is invented: the client re-enables its own
// button (GENormalCommand.cpp:49-53, CCmdCreateWarp::Do). What a SUCCESS sends is the
// movement (3549 stops on the same map, or one 3593 for a map change) plus the ONE 3608
// item line - and NOT a 3634 refresh, because a jump does not change the list.

// warpTravelHandler is a zone that can serve the GO button: Coimbra (map 15), a saved
// point store with the rename and move hooks, a bag with two scrolls in it, and a live
// Reboldoeux (map 10) in the registry so a CROSS-MAP jump has somewhere to go.
//
// THE "LIVE" REBOLDOEUX IS A REAL LISTENER, not just a presence file: the jump DIALS its
// destination before it charges, so a town that only has a file would be refused here for
// the wrong reason (see liveDestination).
//
// The bag is fakeShop, the same in-memory write path the shop tests use: it applies a
// take to the named row, deletes a row that reaches 0, and records every op it was
// asked for - which is exactly what "the charge happened once" means.
func warpTravelHandler(t *testing.T, rows ...SavedPoint) (*Handler, *warpStore, *fakeShop, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	// The listener is held open by the test's own cleanup (liveDestination), so the
	// cross-map jumps below really accept a connection.
	_, reg := liveDestination(t, 10, "reb_basic")
	h := &Handler{
		Log:        log.New(logs, "", 0),
		MapID:      15,
		MapName:    "coim_basic",
		ZonePoints: zoneTable(t),
		Zones:      reg,
		Transfers:  t.TempDir(),
	}
	f := &warpStore{savedTeleportFake: &savedTeleportFake{rows: rows}}
	h.UseSavedTeleports(f.load, f.save, f.remove)
	h.UseSavedPointRename(f.rename)
	h.UseSavedPointMove(f.move)

	bag := newFakeShop(
		item.Stack{ItemID: 11, ClassID: warpScrollClass, Count: 2},
		item.Stack{ItemID: 12, ClassID: warpScrollEventClass, Count: 1},
	)
	h.Bag = func(context.Context, int32) ([]item.Stack, error) { return bag.stacks, nil }
	h.Commit = bag.commit
	return h, f, bag, logs
}

// warpTravelSession is teleportSession WITH the client's bag mirror, so the item line
// the handler builds addresses a row the client really holds. Both members start at
// (100, 200, cell 5) - a spot that is NOT any saved point, so "did the team move" is a
// question with an obvious answer.
func warpTravelSession(t *testing.T, bag *fakeShop) *Session {
	t.Helper()
	sess := teleportSession([3]int32{100, 200, 5})
	sess.Inv = NewInvMirror(bag.stacks)
	return sess
}

// warpSelectPacket is one CZ_COMMWARP_SELECT the way the client writes it: a 10-byte
// security header, the row, the list, and the event flag as ONE byte.
func warpSelectPacket(row, telType byte, isEvent bool) []byte {
	var event byte
	if isEvent {
		event = 1
	}
	return pack(CmdCZCommWarpSelect, row, telType, event)
}

// bagCount is how many of a class the fake bag holds, summed over its rows.
func bagCount(bag *fakeShop, classID int64) int64 {
	var n int64
	for _, st := range bag.stacks {
		if st.ClassID == classID {
			n += st.Count
		}
	}
	return n
}

// liveDestination is a REAL listening socket registered as mapID: the jump dials the
// destination before it charges, so a "live town" in a test has to be one that accepts a
// connection, not just a fresh presence file. The listener is closed by the test's own
// cleanup; a test that wants a DEAD town closes it early (see TestWarpRefundWhenTheTown
// DiesBeforeTheAck).
func liveDestination(t *testing.T, mapID int, mapName string) (net.Listener, *world.Registry) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, presence(t, mapID, port, mapName)
}

// presence writes one presence file exactly as world.Announcer does - fresh last_seen,
// so world.Registry calls it alive - and answers the registry that reads that folder.
func presence(t *testing.T, mapID, port int, mapName string) *world.Registry {
	t.Helper()
	dir := t.TempDir()
	rec := world.Record{
		MapID: mapID, Channel: 0, IP: "127.0.0.1", Port: port, MapName: mapName,
		LastSeen: float64(time.Now().UnixNano()) / 1e9,
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("zone_%d.json", port)), blob, 0o644); err != nil {
		t.Fatalf("write presence: %v", err)
	}
	return &world.Registry{Dir: dir}
}

// countItemPackets is how many bag-change packets (3607 ADD / 3608 REMOVE) one reply
// carries. It is what turns "the client was told ONCE" into a testable claim: two would
// count the scroll down twice, and none is the stale-bag bug R76 is closing.
func countItemPackets(out [][]byte) int {
	n := 0
	for _, p := range out {
		switch cmdOf(p) {
		case CmdZCAddItem, CmdZCRemoveItem:
			n++
		}
	}
	return n
}

// wantStop is one ZC_STOP_PC checked against the spot it should put that character on.
func wantStop(t *testing.T, p []byte, handle uint32, pos [3]int32, at string) {
	t.Helper()
	if pktLen := len(p); pktLen != 22 {
		t.Fatalf("%s: the stop is %d bytes, want 22", at, pktLen)
	}
	if cmd := cmdOf(p); cmd != CmdZCStopPC {
		t.Fatalf("%s: cmd = %d, want %d (ZC_STOP_PC)", at, cmd, CmdZCStopPC)
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != handle {
		t.Fatalf("%s: handle = %d, want %d", at, got, handle)
	}
	got := [3]int32{
		int32(binary.LittleEndian.Uint32(p[10:])),
		int32(binary.LittleEndian.Uint32(p[14:])),
		int32(binary.LittleEndian.Uint32(p[18:])),
	}
	if got != pos {
		t.Fatalf("%s: stop position = %v, want %v", at, got, pos)
	}
}

// TestParseCommWarpSelectFromTheClientStruct pins the three offsets and the size, and
// that a short packet is refused rather than read out of bounds.
func TestParseCommWarpSelectFromTheClientStruct(t *testing.T) {
	pt := warpSelectPacket(7, 2, true)
	if len(pt) != commWarpSelectSize {
		t.Fatalf("test packet is %d bytes, want %d (the client's own registered size)",
			len(pt), commWarpSelectSize)
	}
	index, telType, isEvent, ok := ParseCommWarpSelect(pt)
	if !ok {
		t.Fatalf("the parser refused a full-size packet")
	}
	if index != 7 || telType != 2 || !isEvent {
		t.Fatalf("parsed row %d list %d event %v, want 7, 2, true", index, telType, isEvent)
	}
	// A clean request: the flag at +12 is 0, and it must read as "the normal scroll".
	index, telType, isEvent, ok = ParseCommWarpSelect(warpSelectPacket(0, 0, false))
	if !ok || index != 0 || telType != 0 || isEvent {
		t.Fatalf("parsed row %d list %d event %v ok=%v, want 0, 0, false", index, telType,
			isEvent, ok)
	}
	// Short packets: not a crash, and not row 0 either - row 0 would jump the player.
	for _, n := range []int{0, 4, 10, 12} {
		if _, _, _, ok := ParseCommWarpSelect(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a select; the three bytes need 13", n)
		}
	}
}

// TestWarpSelectSameMapChargesOneScrollAndLands is the feature, on the map the player is
// already on: ONE Warp Scroll of class 21006 is taken, the team is put on the saved spot
// with the in-zone teleport (one 3549 per member, leader LAST, one tick), and no 3634
// list is re-sent - the list did not change.
func TestWarpSelectSameMapChargesOneScrollAndLands(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(3, 15, spot[0], spot[1], spot[2], "Bank"))
	sess := warpTravelSession(t, bag)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	// ONE item line, then ONE stop per character. The team is two, so three packets -
	// and NOT a fourth 3634.
	if len(out) != 1+len(sess.Team) {
		t.Fatalf("%d packet(s), want %d (one 3608 + %d stops): %s", len(out),
			1+len(sess.Team), len(sess.Team), logs)
	}
	line := out[0]
	if cmd := cmdOf(line); cmd != CmdZCRemoveItem {
		t.Fatalf("the first packet is cmd %d, want %d (ZC_REMOVE_ITEM)", cmd,
			CmdZCRemoveItem)
	}
	if n := len(line); n != 15 {
		t.Fatalf("the item line is %d bytes, want 15 (cmd, invIndex u32, count u32, "+
			"removeType, autoRule)", n)
	}
	if pos := binary.LittleEndian.Uint32(line[2:]); pos != 1 {
		t.Fatalf("the item line names position %d, want 1 - the scroll is the FIRST row "+
			"in the client's bag", pos)
	}
	if left := int32(binary.LittleEndian.Uint32(line[6:])); left != 1 {
		t.Fatalf("the item line says %d left, want 1 (the bag held 2) - the count field "+
			"is the NEW TOTAL of that row, never the amount taken (GENormalItem.cpp:521 "+
			"ChangeItemCount(invIndex, count))", left)
	}
	if kind := line[10]; kind != RemoveItemUse {
		t.Fatalf("removeType = %d, want %d (REMOVE_ITEM_USE: the client prints its own "+
			"\"used\" line for it)", kind, RemoveItemUse)
	}
	if rule := binary.LittleEndian.Uint32(line[11:]); rule != 0 {
		t.Fatalf("autoRule = %d, want 0 (this server has no auto-folder rule)", rule)
	}
	t.Logf("the item line on the wire: % x", line)
	// EXACTLY ONE item packet for the ONE row the charge touched: two would make the
	// client count the scroll down twice, and none is the bug this path exists to close.
	if n := countItemPackets(out); n != 1 {
		t.Fatalf("%d item packet(s) in the reply, want exactly 1 (one 3608 for the "+
			"scroll): %s", n, logs)
	}

	// The LEADER LAST: iteration runs backwards, so the leader's own stop is the last.
	wantStop(t, out[1], sess.Team[1].Handle, spot, "the follower")
	wantStop(t, out[2], sess.Team[0].Handle, spot, "the leader, sent LAST")
	tick := binary.LittleEndian.Uint32(out[1][6:])
	if other := binary.LittleEndian.Uint32(out[2][6:]); other != tick {
		t.Fatalf("stops carry ticks %d and %d; a warp is ONE tick", tick, other)
	}

	// The session and every member stand on the saved spot.
	if sess.Pos != spot {
		t.Fatalf("the session stands at %v, want the saved spot %v", sess.Pos, spot)
	}
	for i := range sess.Team {
		if sess.Team[i].Position != spot {
			t.Fatalf("member %d is at %v, want %v", i, sess.Team[i].Position, spot)
		}
	}

	// EXACTLY ONE scroll, of the ordinary class, and the event scroll untouched.
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1 (2 minus one jump)", n)
	}
	if n := bagCount(bag, warpScrollEventClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll_Event, want 1 - a normal jump must not "+
			"spend the event scroll", n)
	}
	if len(bag.ops) != 1 {
		t.Fatalf("%d item op(s), want exactly 1: %+v", len(bag.ops), bag.ops)
	}
	if op := bag.ops[0]; op.Give || op.ClassID != warpScrollClass ||
		op.Count != warpScrollPerJump || op.ItemID != 11 {
		t.Fatalf("the op = %+v, want a take of %d of class %d from row 11", op,
			warpScrollPerJump, warpScrollClass)
	}

	// ONE log line, and it PROVES the charge and names where the team went.
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"CZ_COMMWARP_SELECT (type0 row 0)", "CHARGED 1 WarpScroll",
		"class 21006", "1 left", "same map", "no 3634 refresh"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the jump line does not name %q:\n%s", want, lines[0])
		}
	}
	t.Logf("jump line: %s", lines[0])
}

// TestWarpSelectEventScrollIsSpentWhenTheClientSaysSo: `isEvent` at +12 picks the EVENT
// scroll (21446), and the ordinary one is not touched. That flag is the client's own
// decision - it sets it only when the event scroll is the one it holds.
func TestWarpSelectEventScrollIsSpentWhenTheClientSaysSo(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	sess := warpTravelSession(t, bag)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, true))

	if n := bagCount(bag, warpScrollEventClass); n != 0 {
		t.Fatalf("the bag holds %d WarpScroll_Event, want 0 - isEvent spends that one", n)
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - the event flag must not touch the "+
			"ordinary scroll", n)
	}
	if len(bag.ops) != 1 || bag.ops[0].ClassID != warpScrollEventClass ||
		bag.ops[0].ItemID != 12 {
		t.Fatalf("the ops = %+v, want ONE take of class %d from row 12", bag.ops,
			warpScrollEventClass)
	}
	if sess.Pos != spot {
		t.Fatalf("the session stands at %v, want the saved spot %v", sess.Pos, spot)
	}
	wantStop(t, out[len(out)-1], sess.Team[0].Handle, spot, "the leader")
	if !strings.Contains(logs.String(), "CHARGED 1 WarpScroll_Event") ||
		!strings.Contains(logs.String(), "class 21446") {
		t.Fatalf("the jump line does not name the event scroll:\n%s", logs.String())
	}
}

// TestWarpSelectWithoutAScrollIsLogOnly: the bag holds none of the class the client named,
// so nothing is charged, NOBODY moves, and - the point of this test - NOTHING is sent:
// no item packet and no ZC_MESSAGE.
//
// WHY NO MESSAGE. The player already has the reference server's answer: game.scp:174-196
// checks the bag BEFORE it sends 3637 and, with none, shows the CLIENT's own box
// MsgBox("<$>79767</>"). Our 3596 line replaced that box, which is the owner's complaint
// of 2026-10-30 ("i dont need this line. it was supposed to show the message box like the
// live server reference"). A request that reaches here anyway is a bag that is out of
// step, and the LOG is the record of it - it must say the bag held none and that nothing
// was charged.
func TestWarpSelectWithoutAScrollIsLogOnly(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	// A bag with every scroll of the two classes taken out of it.
	bag.stacks = []item.Stack{{ItemID: 90, ClassID: 21613, Count: 5}}
	sess := warpTravelSession(t, bag)
	start := sess.Pos

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent on the no-scroll refusal, want NONE - the client's "+
			"own MsgBox 79767 is the answer, and a 3596 here overrides it: %x", len(out), out)
	}
	for _, p := range out {
		switch cmdOf(p) {
		case CmdZCMessage:
			t.Fatalf("a ZC_MESSAGE (3596) was sent for the no-scroll case; that is the " +
				"line the owner asked to drop")
		case CmdZCAddItem, CmdZCRemoveItem:
			t.Fatalf("an item packet (cmd %d) was sent although nothing was charged",
				cmdOf(p))
		}
	}
	if len(bag.ops) != 0 {
		t.Fatalf("%d item op(s) on a refusal, want none: %+v", len(bag.ops), bag.ops)
	}
	if sess.Pos != start {
		t.Fatalf("the session moved to %v; a refusal moves nobody", sess.Pos)
	}
	for i := range sess.Team {
		if sess.Team[i].Position != start {
			t.Fatalf("member %d moved to %v; a refusal moves nobody", i,
				sess.Team[i].Position)
		}
	}
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1 (the log is the ONLY record of this now):\n%s",
			len(lines), logs.String())
	}
	for _, want := range []string{"REFUSED", "holds no WarpScroll", "class 21006",
		"nothing was charged", "LOG ONLY"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, lines[0])
		}
	}
	t.Logf("no-scroll refusal: %s", lines[0])
}

// TestWarpSelectBadRowRefuses: an index that names no row of THAT list refuses - no
// charge, no move, one message. The store is never asked to write anything.
func TestWarpSelectBadRowRefuses(t *testing.T) {
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, 2222, 16900, 233, "Bank"))
	sess := warpTravelSession(t, bag)
	start := sess.Pos

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(5, commWarpOwnList, false))

	wantOneWarpRefusalMessage(t, out, warpNoSuchRowText)
	if len(bag.ops) != 0 {
		t.Fatalf("%d item op(s) for a bad row, want none", len(bag.ops))
	}
	if sess.Pos != start {
		t.Fatalf("the session moved to %v; a bad row must move nobody", sess.Pos)
	}
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"REFUSED", "named row 5", "1 point(s)"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, lines[0])
		}
	}
	t.Logf("bad-row refusal: %s", lines[0])
}

// TestWarpSelectCrossMapWithoutAZoneRefuses: a point on a map nobody is serving is a jump
// that cannot finish, so it is refused BEFORE the scroll is spent - one message, one line,
// no charge, no 3593. The check has to happen here and not only inside startMoveFor,
// because startMoveFor runs AFTER the charge.
func TestWarpSelectCrossMapWithoutAZoneRefuses(t *testing.T) {
	// Map 5 (test_zone_bot) is in the spawn table and no test serves it.
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 5, 2000, 0, 1, "nowhere"))
	sess := warpTravelSession(t, bag)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	wantOneWarpRefusalMessage(t, out, warpTravelRefusedText)
	if len(bag.ops) != 0 {
		t.Fatalf("%d item op(s) for a destination with no zone, want none: %+v",
			len(bag.ops), bag.ops)
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - nothing may be charged", n)
	}
	if sess.Gate != nil {
		t.Fatalf("a map change was started to a map with no zone")
	}
	for _, p := range out {
		if cmdOf(p) == CmdZCMoveZone {
			t.Fatalf("a refusal sent a 3593")
		}
	}
	if !strings.Contains(logs.String(), "no live zone serves map 5") {
		t.Fatalf("the refusal does not say why:\n%s", logs.String())
	}
	t.Logf("no-zone refusal: %s", strings.TrimSpace(logs.String()))
}

// TestWarpSelectCrossMapSpendsAScrollAndStartsTheMove: the other branch. A point on
// Reboldoeux (map 10, running here) charges ONE scroll and starts the map change with the
// SAME one 3593 the town grid's travel sends - and still sends no 3634.
func TestWarpSelectCrossMapSpendsAScrollAndStartsTheMove(t *testing.T) {
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 10, 3946, 193, 146, "Reboldoeux"))
	sess := warpTravelSession(t, bag)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (one 3608 item line + one 3593): %s", len(out), logs)
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCRemoveItem {
		t.Fatalf("the first packet is cmd %d, want the item line %d", cmd, CmdZCRemoveItem)
	}
	move := out[1]
	if cmd := cmdOf(move); cmd != CmdZCMoveZone {
		t.Fatalf("the second packet is cmd %d, want %d (ZC_MOVE_ZONE)", cmd, CmdZCMoveZone)
	}
	if move[2] != moveZoneFlag {
		t.Fatalf("the 3593 flag byte = %d, want %d", move[2], moveZoneFlag)
	}
	if sess.Gate == nil || sess.Gate.gate.ToID != 10 {
		t.Fatalf("the jump left no pending map change to map 10")
	}
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1", n)
	}
	for _, want := range []string{"CHARGED 1 WarpScroll", "started a map change to map 10",
		"reb_basic", "no 3634 refresh"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the jump line does not name %q:\n%s", want, logs.String())
		}
	}
	t.Logf("cross-map jump line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpSelectRowIsResolvedInsideTheNamedList: 3637 names a list AND a row, and the row
// is a position INSIDE that list - not inside a flat forty-row collection. The point is
// SAVED into "Warp Extend(1)" here, then selected as that panel's row 0, and it must be
// THAT spot that the team lands on while the basic list's own row 0 is ignored.
func TestWarpSelectRowIsResolvedInsideTheNamedList(t *testing.T) {
	basicSpot := [3]int32{100, 200, 5}
	h, f, bag, logs := warpTravelHandler(t,
		warpRow(0, 15, basicSpot[0], basicSpot[1], basicSpot[2], "basic first"))
	sess := warpTravelSession(t, bag)

	// SAVE into list 1, at the leader's own position - the real 3635 handler does it.
	sess.Pos = [3]int32{2222, 16900, 233}
	warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(1, "extend point"))
	if len(f.saved) != 1 || f.saved[0].TelType != 1 {
		t.Fatalf("the save wrote %+v, want one row in list 1", f.saved)
	}
	extendSpot := f.saved[0].Pos

	// Now the GO button, naming list 1 row 0.
	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, 1, false))

	if sess.Pos != extendSpot {
		t.Fatalf("the team landed on %v, want the point saved in Warp Extend(1) at %v - "+
			"the row number is a position inside the list the packet names",
			sess.Pos, extendSpot)
	}
	if sess.Pos == basicSpot {
		t.Fatalf("the jump used row 0 of the BASIC list (%v) although the packet named "+
			"list 1", basicSpot)
	}
	// The leader's stop is the last packet, and it is the extend point.
	wantStop(t, out[len(out)-1], sess.Team[0].Handle, extendSpot, "the leader")
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1", n)
	}
	if !strings.Contains(logs.String(), "CZ_COMMWARP_SELECT (type1 row 0)") {
		t.Fatalf("the jump line does not name the list and the row:\n%s", logs.String())
	}
}

// TestWarpSelectTellsTheClientTheRowIsGone: the LAST scroll in the row. The count field
// of a ZC_REMOVE_ITEM is the NEW TOTAL, so "the row is gone" is the number 0 and nothing
// else - `InvItemList::SetItemCount` removes the row when the count is 0
// (UISession.cpp:1993). The mirror must forget the position in the same breath, or the
// next charge would name a slot the client no longer has.
func TestWarpSelectTellsTheClientTheRowIsGone(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	// ONE scroll, on the row the client knows as position 1.
	bag.stacks = []item.Stack{{ItemID: 11, ClassID: warpScrollClass, Count: 1}}
	sess := warpTravelSession(t, bag)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	if n := countItemPackets(out); n != 1 {
		t.Fatalf("%d item packet(s), want exactly 1: %s", n, logs)
	}
	line := out[0]
	if cmd := cmdOf(line); cmd != CmdZCRemoveItem {
		t.Fatalf("the first packet is cmd %d, want %d (ZC_REMOVE_ITEM)", cmd,
			CmdZCRemoveItem)
	}
	if n := len(line); n != 15 {
		t.Fatalf("the item line is %d bytes, want 15", n)
	}
	if pos := binary.LittleEndian.Uint32(line[2:]); pos != 1 {
		t.Fatalf("the item line names position %d, want 1", pos)
	}
	if left := binary.LittleEndian.Uint32(line[6:]); left != 0 {
		t.Fatalf("the item line says %d left, want 0 - a count of 0 is how the client is "+
			"told the row is GONE", left)
	}
	if kind := line[10]; kind != RemoveItemUse {
		t.Fatalf("removeType = %d, want %d (REMOVE_ITEM_USE)", kind, RemoveItemUse)
	}
	t.Logf("the last-scroll item line on the wire: % x", line)

	// The mirror forgot it, so a later packet can never point at that position again.
	if pos, known := sess.Inv.Index(11); known {
		t.Fatalf("the mirror still answers position %d for the spent row", pos)
	}
	if n := bagCount(bag, warpScrollClass); n != 0 {
		t.Fatalf("the bag holds %d WarpScroll, want 0", n)
	}
	for _, want := range []string{"CHARGED 1 WarpScroll", "0 left",
		"position 1 is GONE", "removeType 8"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the jump line does not name %q:\n%s", want, logs.String())
		}
	}
	t.Logf("last-scroll jump line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpSelectSaysSoWhenTheClientWasNotTold is the trap, pinned: a bag row this session
// was never told about has NO position in the client's list, so no incremental packet can
// address it. Pointing at a guessed position would decrement a different item, so the
// honest answer is silence on the wire and a line in the log that says the client was not
// told - which is exactly what the log used to claim it had done.
func TestWarpSelectSaysSoWhenTheClientWasNotTold(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	// A mirror built from an EMPTY bag: the scroll reached the database behind this
	// session's back (the mint path does not talk to the zone).
	sess := teleportSession([3]int32{100, 200, 5})
	sess.Inv = NewInvMirror(nil)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	if n := countItemPackets(out); n != 0 {
		t.Fatalf("%d item packet(s), want none: a position the client never had must not "+
			"be named", n)
	}
	// It still MOVED (the charge was real and the jump paid for), leader last.
	wantStop(t, out[len(out)-1], sess.Team[0].Handle, spot, "the leader")
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1 (2 minus one jump)", n)
	}
	for _, want := range []string{"CHARGED 1 WarpScroll", "the client was NOT told",
		"no cmd 3608 could be built"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the jump line does not name %q:\n%s", want, logs.String())
		}
	}
	t.Logf("untold charge line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpRefundAfterChargeTellsTheClient: the after-charge refusal gives the scroll back
// AND tells the client, or the bag would sit one lower on the client's side forever.
//
// Reaching it takes a session with NO team: every reason a CROSS-MAP move can fail is
// decided before the charge, so the only reachable "charged, then the move cannot start"
// on this map is "there was nobody to place". It should not happen in play - the leader is
// always in the team - and this test keeps the path honest rather than dead.
func TestWarpRefundAfterChargeTellsTheClient(t *testing.T) {
	spot := [3]int32{2222, 16900, 233}
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	sess := warpTravelSession(t, bag)
	sess.Team = nil

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	if n := countItemPackets(out); n != 1 {
		t.Fatalf("%d item packet(s) on a refund, want exactly 1 (the scroll coming back): "+
			"%s", n, logs)
	}
	line := out[0]
	if cmd := cmdOf(line); cmd != CmdZCAddItem {
		t.Fatalf("the refund packet is cmd %d, want %d (ZC_ADD_ITEM)", cmd, CmdZCAddItem)
	}
	if n := len(line); n != 44 {
		t.Fatalf("the refund line is %d bytes, want 44", n)
	}
	if class := int64(binary.LittleEndian.Uint32(line[4:])); class != warpScrollClass {
		t.Fatalf("the refund line names class %d, want %d (WarpScroll)", class,
			warpScrollClass)
	}
	if got := int32(binary.LittleEndian.Uint32(line[12:])); got != 2 {
		t.Fatalf("the refund line says the row holds %d, want 2 (1 was taken, it is back)",
			got)
	}
	if kind := line[25]; kind != AddItemPlain {
		t.Fatalf("addType = %d, want %d - a refund is not a purchase", kind, AddItemPlain)
	}
	// ONE message, and it is the "cannot be used right now" line, not a stale-bag line:
	// this refusal is OUR failure, and the player is owed a word about it.
	if cmdOf(out[len(out)-1]) != CmdZCMessage {
		t.Fatalf("the last packet is cmd %d, want %d (ZC_MESSAGE)", cmdOf(out[len(out)-1]),
			CmdZCMessage)
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - a failed jump costs nothing", n)
	}
	for _, want := range []string{"the scroll was given back and the client was told",
		"one cmd 3607 item line", "nothing was charged"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refund line does not name %q:\n%s", want, logs.String())
		}
	}
	t.Logf("refund line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpSelectDeadDestinationIsRefusedBeforeTheCharge is the owner's bug, pinned: a
// town whose server is DOWN still has a fresh presence file (a killed zone leaves it
// behind), so the registry calls map 10 alive. Before the jump charges anything it DIALS
// that address; nothing answers, so the jump is refused with ONE log line and ONE
// ZC_MESSAGE - no item op, no 3593, no 3608, and the player keeps both scrolls.
func TestWarpSelectDeadDestinationIsRefusedBeforeTheCharge(t *testing.T) {
	// A port that was free a moment ago and has nothing listening on it now.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 10, 3946, 193, 146, "Reboldoeux"))
	h.Zones = presence(t, 10, port, "reb_basic")
	sess := warpTravelSession(t, bag)
	start := sess.Pos

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))

	wantOneWarpRefusalMessage(t, out, warpTravelRefusedText) // one 3596, nothing else
	if len(bag.ops) != 0 {
		t.Fatalf("%d item op(s) for a dead destination, want none: %+v", len(bag.ops), bag.ops)
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - a dead town must cost nothing", n)
	}
	if sess.Gate != nil {
		t.Fatalf("a map change was started to a dead town")
	}
	for _, p := range out {
		if cmdOf(p) == CmdZCMoveZone || cmdOf(p) == CmdZCRemoveItem {
			t.Fatalf("a refusal to a dead town sent cmd %d", cmdOf(p))
		}
	}
	if sess.Pos != start {
		t.Fatalf("the session moved to %v; a refusal moves nobody", sess.Pos)
	}
	for _, want := range []string{"REFUSED", "NOTHING ANSWERS", "nothing was charged"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "CHARGED") {
		t.Fatalf("the dead-town refusal still charged a scroll:\n%s", logs.String())
	}
	t.Logf("dead-town refusal: %s", strings.TrimSpace(logs.String()))
}

// TestWarpRefundWhenTheTownDiesBeforeTheAck: the destination was answering when the
// scroll was spent, so the jump legitimately started - and then the town died before the
// client answered 3686. The fare is given back in the database AND on the wire (ONE
// ZC_ADD_ITEM whose count is the row's new total), the player gets ONE message, and no
// 3594 and no handoff name a dead address.
func TestWarpRefundWhenTheTownDiesBeforeTheAck(t *testing.T) {
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 10, 3946, 193, 146, "Reboldoeux"))
	ln, reg := liveDestination(t, 10, "reb_basic")
	h.Zones = reg
	sess := warpTravelSession(t, bag)

	// The jump: the dial passes, the scroll is spent, one 3593 goes out.
	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))
	if len(out) != 2 || cmdOf(out[0]) != CmdZCRemoveItem || cmdOf(out[1]) != CmdZCMoveZone {
		t.Fatalf("the jump did not start as expected (%d packet(s)): %s", len(out), logs)
	}
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll before the ack, want 1", n)
	}

	// The town dies between the charge and the client's answer.
	ln.Close()

	back := warpReply(t, h, sess, CmdCZMoveZoneOK, pack(CmdCZMoveZoneOK))

	if n := countItemPackets(back); n != 1 {
		t.Fatalf("%d item packet(s) on the refund, want exactly 1: %s", n, logs)
	}
	if cmd := cmdOf(back[0]); cmd != CmdZCAddItem {
		t.Fatalf("the refund packet is cmd %d, want %d (ZC_ADD_ITEM)", cmd, CmdZCAddItem)
	}
	if got := int32(binary.LittleEndian.Uint32(back[0][12:])); got != 2 {
		t.Fatalf("the refund line says the row holds %d, want 2 (one was taken, it is back)", got)
	}
	if cmdOf(back[len(back)-1]) != CmdZCMessage {
		t.Fatalf("the last packet is cmd %d, want %d (the player is told)", cmdOf(back[len(back)-1]), CmdZCMessage)
	}
	for _, p := range back {
		if cmdOf(p) == CmdZCMoveZoneOK {
			t.Fatalf("a 3594 was sent naming a dead address")
		}
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - a failed move costs nothing", n)
	}
	if sess.Gate != nil {
		t.Fatalf("the dead handshake was left pending")
	}
	// No handoff file was written for this account.
	if _, err := os.Stat(TransferPath(h.transferDir(), sess.AccountID)); err == nil {
		t.Fatalf("a handoff file was left for a move that never happened")
	}
	for _, want := range []string{"STILL DOES NOT ANSWER", "given back", "no cmd 3594"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refund line does not name %q:\n%s", want, logs.String())
		}
	}
	t.Logf("refund line: %s", strings.TrimSpace(logs.String()))
}

// THE HANDOVER DEADLINE (zone\gate.go, HandoffDeadline / Handler.armHandoff).
//
// The dead-town refusal above proves a dial that FAILS. These two tests are about a town
// that ACCEPTS the connection and never finishes the handover - the last hole in a paid
// map change, and the reason the deadline exists:
//
//  1. the dial passes, so the scroll is spent;
//  2. the 3593 goes out and the client answers 3686;
//  3. the handoff is written and the 3594 names the destination;
//  4. the destination accepts and then never takes the character.
//
// Without a deadline step 4 is invisible: the handoff file stays on disk, the scroll stays
// spent and the player stands where he was. With it, one refund, one message, and the file
// is taken off its name so a LATE arrival cannot use the point the refund paid for.
//
// The deadline is five seconds (HandoffDeadline) and the tests wait it out rather than
// shorten it, so what is tested is the number that ships. The two of them run in parallel,
// so the suite grows by about six seconds and not twelve.

// waitPastTheHandoverDeadline sleeps past the deadline. Six seconds, spelled out, because
// the point is that the number in the product is the number here.
func waitPastTheHandoverDeadline() { time.Sleep(6 * time.Second) }

// TestWarpHandoffDeadlineRefundsWhenTheHandoverNeverCompletes is hole 4, pinned: a
// destination that answers the dial and never takes the character costs the player
// NOTHING - one ZC_ADD_ITEM (the scroll back), ONE ZC_MESSAGE, no second 3594, no handoff
// left behind - and the refund fires exactly once even though two wakers (the session's
// tick and the deadline's own goroutine) both ask the question.
//
// It also pins the LATE SUCCESS: after that refund, a START_GAME that arrives with the
// same account must land on the map's own spawn, not on the gate's exit, so nobody is ever
// moved to the place a refunded scroll paid for.
func TestWarpHandoffDeadlineRefundsWhenTheHandoverNeverCompletes(t *testing.T) {
	t.Parallel()
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 10, 3946, 193, 146, "Reboldoeux"))
	sess := warpTravelSession(t, bag)
	sess.AccountID = 7000001
	start := sess.Pos

	// Steps 1 and 2: charged, then 3593.
	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))
	if len(out) != 2 || cmdOf(out[0]) != CmdZCRemoveItem || cmdOf(out[1]) != CmdZCMoveZone {
		t.Fatalf("the jump did not start as expected (%d packet(s)): %s", len(out), logs)
	}
	// Step 3: the client accepted, so the 3594 and the handoff exist.
	back := warpReply(t, h, sess, CmdCZMoveZoneOK, pack(CmdCZMoveZoneOK))
	if len(back) != 1 || cmdOf(back[0]) != CmdZCMoveZoneOK {
		t.Fatalf("the ack sent %d packet(s), want the one 3594: %s", len(back), logs)
	}
	path := TransferPath(h.transferDir(), sess.AccountID)
	if _, err := ReadTransfer(path, sess.AccountID, 10, time.Now()); err != nil {
		t.Fatalf("no usable handoff after the ack: %v", err)
	}

	// Step 4: nothing ever takes it. Wait out the deadline itself.
	waitPastTheHandoverDeadline()

	// The one message and the refund land on the socket that is still open. They come
	// from the deadline goroutine, because that is the waker that survives the session.
	conn := sess.Conn.(*recordConn)
	sent := conn.sent()
	if len(sent) != 2 {
		t.Fatalf("the deadline wrote %d packet(s), want exactly 2 (the refund, then one "+
			"message): %s", len(sent), logs)
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCAddItem {
		t.Fatalf("the first packet past the deadline is cmd %d, want %d (the refund)",
			cmd, CmdZCAddItem)
	}
	if class := int64(binary.LittleEndian.Uint32(sent[0][4:])); class != warpScrollClass {
		t.Fatalf("the refund names class %d, want %d (WarpScroll)", class, warpScrollClass)
	}
	if got := int32(binary.LittleEndian.Uint32(sent[0][12:])); got != 2 {
		t.Fatalf("the refund line says the row holds %d, want 2 (one was taken, it is "+
			"back)", got)
	}
	if cmd := cmdOf(sent[1]); cmd != CmdZCMessage {
		t.Fatalf("the second packet is cmd %d, want %d (the player is told)", cmd,
			CmdZCMessage)
	}

	// The money question: the bag is whole again, and the charge is the only other write.
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll, want 2 - a handover nobody completed must "+
			"cost nothing: %s", n, logs)
	}
	if n := len(bag.ops); n != 2 {
		t.Fatalf("%d item op(s) for the jump and its refund, want 2 (one take, one give): "+
			"%+v", n, bag.ops)
	}
	if !bag.ops[1].Give {
		t.Fatalf("the second item op is not a give: %+v", bag.ops[1])
	}

	// Nobody moved: the player is still where he stood, and the point is gone from disk.
	if sess.Pos != start {
		t.Fatalf("the session stands at %v, want %v - a refunded jump moves nobody",
			sess.Pos, start)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the refunded handoff is still on disk (%v) - a late arrival could use "+
			"the point the scroll was given back for", err)
	}

	// ONCE, even though two wakers ask: the tick now finds nothing to do, and it never
	// refunds a second time.
	for i := 0; i < 3; i++ {
		if again := h.gateTick(sess); len(again) != 0 {
			t.Fatalf("tick %d after the refund sent %d packet(s), want 0 - the refund is "+
				"single-shot", i+1, len(again))
		}
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll after more ticks, want 2 - the refund fired "+
			"twice", n)
	}
	if n := len(bag.ops); n != 2 {
		t.Fatalf("%d item op(s) after more ticks, want 2: %+v", n, bag.ops)
	}

	// THE ONE LINE: it names the deadline, the destination and the refund, so "did I lose
	// a scroll on a town that never took me?" is answerable from the log alone.
	for _, want := range []string{"HANDOVER DEADLINE", "5s", "map 10 (reb_basic)",
		"127.0.0.1", "given back", "nothing was charged"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the deadline line does not name %q:\n%s", want, logs.String())
		}
	}

	// LATE SUCCESS: the destination now starts serving and the client's START_GAME
	// arrives after the refund. The team enters at the map's OWN SPAWN - never at the gate
	// exit the refunded handoff named.
	points := zoneTable(t)
	landing, err := points.SpawnFor(10)
	if err != nil {
		t.Fatalf("SpawnFor(10): %v", err)
	}
	dst := &Handler{
		Log: log.New(io.Discard, "", 0), MapID: 10,
		Spawn: landing.Pos, SpawnFormation: landing.Formation,
		Transfers: h.transferDir(),
	}
	b := newTeam([]uint32{0x40000071}, [][3]int32{{1, 2, 3}})
	startPkt := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	if _, err := dst.Reply(context.Background(), b.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(startPkt), Data: startPkt}); err != nil {
		t.Fatalf("a late START_GAME on the destination: %v", err)
	}
	if got := b.sess.Team[0].Position; got != landing.Pos {
		t.Fatalf("a late arrival stands at %v, want the map's own spawn %v - the gate "+
			"exit of a REFUNDED handoff must not be usable", got, landing.Pos)
	}
	if n := bagCount(bag, warpScrollClass); n != 2 {
		t.Fatalf("the bag holds %d WarpScroll after the late arrival, want 2 - the late "+
			"entry must not refund a second time", n)
	}
	t.Logf("deadline line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpHandoffTakenBeforeTheDeadlineCostsNothingMore is the other half: the SAME jump
// with a destination that takes the handoff at once is unchanged by the deadline. The
// existing end-to-end proof of that path is TestHandoffArrivesAtTheGateExit (gate_test.go,
// the destination's START_GAME puts the team on the gate's exit), and
// TestMoveZoneAckIsTheOnlyFinish pins the 3594 and the one file; this one pins what the
// NEW timer must not do - fire a refund on a handover that already finished.
func TestWarpHandoffTakenBeforeTheDeadlineCostsNothingMore(t *testing.T) {
	t.Parallel()
	h, _, bag, logs := warpTravelHandler(t, warpRow(0, 10, 3946, 193, 146, "Reboldoeux"))
	sess := warpTravelSession(t, bag)
	sess.AccountID = 7000001

	out := warpReply(t, h, sess, CmdCZCommWarpSelect, warpSelectPacket(0, commWarpOwnList, false))
	if len(out) != 2 || cmdOf(out[0]) != CmdZCRemoveItem {
		t.Fatalf("the jump did not charge (%d packet(s)): %s", len(out), logs)
	}
	if back := warpReply(t, h, sess, CmdCZMoveZoneOK, pack(CmdCZMoveZoneOK)); len(back) != 1 {
		t.Fatalf("the ack sent %d packet(s), want the one 3594: %s", len(back), logs)
	}

	// The destination takes it AT ONCE, which is what placeTeam does - by the same atomic
	// claim the deadline uses, so only one of the two can win.
	path := TransferPath(h.transferDir(), sess.AccountID)
	tf, err := ClaimTransfer(path, sess.AccountID, 10, time.Now())
	if err != nil {
		t.Fatalf("the destination could not take a fresh handoff: %v", err)
	}
	if tf.X != 3946 || tf.Y != 193 {
		t.Fatalf("the claimed point is (%d, %d), want the gate's exit (3946, 193)", tf.X, tf.Y)
	}

	// Past the deadline the timer must do NOTHING: the file is gone, so the claim it
	// makes fails and no scroll goes back.
	waitPastTheHandoverDeadline()
	if n := len(sess.Conn.(*recordConn).sent()); n != 0 {
		t.Fatalf("a completed handover was written %d packet(s) past the deadline, want 0 "+
			"- a successful jump must not look like a failure: %s", n, logs)
	}
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1 - a jump that was taken is PAID for: %s",
			n, logs)
	}
	if n := len(bag.ops); n != 1 {
		t.Fatalf("%d item op(s), want 1 (the charge) - nothing is given back for a "+
			"handover that finished: %+v", n, bag.ops)
	}
	if strings.Contains(logs.String(), "HANDOVER DEADLINE") {
		t.Fatalf("a completed handover printed the deadline line:\n%s", logs.String())
	}
}
