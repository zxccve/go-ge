package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/db"
	"geserver/protocol"
)

// The saved-point family, tested with NO CLIENT: the bytes on the wire, the list
// row -> slot translation, and the four answers.
//
// The one test that touches PostgreSQL is off by default and says so
// (GE_SAVED_TELEPORT_DB_TEST=1): it writes to commander 25, the throwaway account,
// and deletes every row it made. Everything else runs against an in-memory store
// whose behaviour is the CONTRACT Handler's hooks promise, so a handler that forgets
// one case fails here without a database.

// savedTeleportFake is the store a handler test wires in.
//
// Its save follows the same contract as PGStore.SaveSavedTeleport: slot -1 means any
// free slot, and the new row goes to the BOTTOM of the list. That rule is DUPLICATED
// here on purpose - the handler's decision (what position and map it offers) and the
// store's decision (which slot, which display position) are two different mistakes, and
// the real store has its own test below.
//
// The ORDER is what load returns: `Order` first, then `Slot` as a tiebreak, which is
// exactly what PGStore.SavedTeleports' `ORDER BY display_order, slot` does. Before R76
// slice 2 the read was slot order, and re-creating that here is what lets a test catch
// the bug the owner saw (a deleted row refilled in the middle of the window).
type savedTeleportFake struct {
	rows    []SavedPoint
	err     error
	saves   int
	removes []int
	saved   []SavedPoint
}

func (f *savedTeleportFake) load(_ context.Context, _ int32) ([]SavedPoint, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := append([]SavedPoint(nil), f.rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TelType != out[j].TelType {
			return out[i].TelType < out[j].TelType
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Slot < out[j].Slot
	})
	return out, nil
}

func (f *savedTeleportFake) save(_ context.Context, _ int32, telType, slot int, p SavedPoint) (SavedPoint, error) {
	f.saves++
	if f.err != nil {
		return SavedPoint{}, f.err
	}
	// A SPOT ALREADY IN THE LIST IS REFUSED, by map + x + y + cell and NEVER by name -
	// and now NEVER by list either: the same spot in any of the four lists is the same
	// place (R76 slice 6). sameSpot is the store's own predicate
	// (saved_teleport_db.go), so this copy cannot drift from the rule the SQL enforces;
	// the handler's mapping of the refusal is what a handler test exercises. A different
	// map is a different place, and that is allowed.
	for _, r := range f.rows {
		if sameSpot(r, p) {
			return SavedPoint{}, errDuplicatePoint
		}
	}
	// THE CAP COUNTS ROWS OF ONE LIST: ten rows fill THAT list, whatever slots they
	// hold, and the other three lists are untouched.
	inList := 0
	for _, r := range f.rows {
		if r.TelType == telType {
			inList++
		}
	}
	if inList >= MaxSavedTeleports {
		return SavedPoint{}, errNoFreeSlot
	}
	if slot < 0 {
		slot = -1
		for i := 0; i < MaxSavedTeleports; i++ {
			taken := false
			for _, r := range f.rows {
				if r.TelType == telType && r.Slot == i {
					taken = true
				}
			}
			if !taken {
				slot = i
				break
			}
		}
		if slot < 0 {
			return SavedPoint{}, errNoFreeSlot
		}
	}
	// A NEW ROW GOES TO THE BOTTOM OF ITS OWN LIST, one past that list's highest
	// display position. Another list's numbers are a different sequence.
	maxOrder := -1
	for _, r := range f.rows {
		if r.TelType == telType && r.Order > maxOrder {
			maxOrder = r.Order
		}
	}
	p.TelType = telType
	p.Order = maxOrder + 1
	p.Slot = slot
	f.rows = append(f.rows, p)
	f.saved = append(f.saved, p)
	return p, nil
}

func (f *savedTeleportFake) remove(_ context.Context, _ int32, telType, slot int) (SavedPoint, bool, error) {
	f.removes = append(f.removes, slot)
	if f.err != nil {
		return SavedPoint{}, false, f.err
	}
	for i, r := range f.rows {
		if r.TelType == telType && r.Slot == slot {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return r, true, nil
		}
	}
	return SavedPoint{}, false, nil
}

// teleportPacket builds one of the four requests the way the client's own sender
// does: a 10-byte security header, then the body.
func teleportPacket(cmd uint16, body ...byte) []byte { return pack(cmd, body...) }

// teleportHandler is the same travelling zone waypointHandler builds (Coimbra, with
// Reboldoeux live), plus a saved-point store.
func teleportHandler(t *testing.T, rows ...SavedPoint) (*Handler, *savedTeleportFake, *bytes.Buffer) {
	t.Helper()
	h, _, logs := waypointHandler(t)
	f := &savedTeleportFake{rows: rows}
	h.UseSavedTeleports(f.load, f.save, f.remove)
	return h, f, logs
}

// teleportSession is a session whose commander is resolved and whose leader stands
// somewhere real, so a save has a position to write.
func teleportSession(pos [3]int32) *Session {
	s := newTeam([]uint32{0x40000031, 0x40000032}, [][3]int32{pos, pos}).sess
	s.CommanderID = 25 // the throwaway test commander
	return s
}

// TestParseTeleportIndexFromTheClientLayout pins the offsets: the PC index is the
// first body byte and the list row is the second, so a parser that read the index
// from offset 10 would travel to the wrong point on every click.
func TestParseTeleportIndexFromTheClientLayout(t *testing.T) {
	for _, index := range []int{0, 1, 4, 255} {
		pt := teleportPacket(CmdCZTeleport, 2, byte(index))
		if len(pt) != 12 {
			t.Fatalf("test packet is %d bytes, want 12 (the client's table size)", len(pt))
		}
		got, ok := ParseTeleportIndex(pt)
		if !ok {
			t.Fatalf("row %d: the parser refused a full-size packet", index)
		}
		if got != index {
			t.Fatalf("row = %d, want %d - it is the byte at offset 11", got, index)
		}
		if pc := ParseTeleportCharIndex(pt); pc != 2 {
			t.Fatalf("pc index = %d, want 2 - it is the byte at offset 10", pc)
		}
	}

	// A packet too short to hold the row: a malformed client, not a crash and not
	// row 0 (deleting row 0 on a short packet would destroy the wrong point).
	for _, n := range []int{0, 4, 10, 11} {
		pt := make([]byte, n)
		if _, ok := ParseTeleportIndex(pt); ok {
			t.Fatalf("%d bytes parsed as a list row; the byte needs 12", n)
		}
	}
}

// TestTeleportListLayout pins EVERY byte of ZC_TELEPORTLIST for one saved point.
//
// This is the layout evidence, in a form a machine can re-check: the header, the two
// one-byte fields, and the 14-byte record - x, y, the navmesh CELL, then the map id.
// A change to the stride, the field order or the length word fails here.
func TestTeleportListLayout(t *testing.T) {
	rows := []SavedPoint{{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}, Name: "reb"}}
	out, err := BuildTeleportList(1, rows)
	if err != nil {
		t.Fatalf("BuildTeleportList: %v", err)
	}
	if len(out) != 20 {
		t.Fatalf("packet is %d bytes, want 20 = 4 header + 2 prefix + 14 record", len(out))
	}

	want := "3a0e14000101" + // cmd 3642, len 20, charIndex 1, curCount 1
		"6a0f0000" + //         x = 3946
		"c1000000" + //         y = 193
		"92000000" + //         cell = 146
		"0a00" //              map id = 10
	got := hex.EncodeToString(out)
	if got != want {
		t.Fatalf("ZC_TELEPORTLIST bytes:\n got %s\nwant %s", got, want)
	}

	// The length word covers the WHOLE packet, header included: the client divides
	// what is left, so an off-by-header here draws the wrong number of points with no
	// error anywhere.
	if got := binary.LittleEndian.Uint16(out[2:]); int(got) != len(out) {
		t.Fatalf("len word = %d, want %d (the whole packet)", got, len(out))
	}
	if got := int(out[5]); got != len(rows) {
		t.Fatalf("curCount = %d, want %d", got, len(rows))
	}

	// An empty list is still a packet: header and the two zero bytes, which is what
	// the window must be told when it opens on a commander with no points.
	empty, err := BuildTeleportList(0, nil)
	if err != nil {
		t.Fatalf("BuildTeleportList(nil): %v", err)
	}
	if got := hex.EncodeToString(empty); got != "3a0e06000000" {
		t.Fatalf("empty list = %s, want 3a0e06000000", got)
	}
}

// TestTeleportListAtMapsRowToSlot is the trap this slice is most likely to ship: the
// client names a LIST ROW, the database is keyed by SLOT, and after one remove in the
// middle the two disagree. Removing row 1 of slots {0, 2} must delete slot 2.
func TestTeleportListAtMapsRowToSlot(t *testing.T) {
	rows := []SavedPoint{{Slot: 0, MapID: 10}, {Slot: 2, MapID: 15}, {Slot: 4, MapID: 149}}
	if p, ok := listAt(rows, 1); !ok || p.Slot != 2 {
		t.Fatalf("row 1 = slot %d (ok %v), want slot 2 - rows are positions, slots are "+
			"keys", p.Slot, ok)
	}
	for _, bad := range []int{-1, 3, 99} {
		if _, ok := listAt(rows, bad); ok {
			t.Fatalf("row %d resolved against a 3-point list", bad)
		}
	}
}

// TestTeleportRequestSendsTheWholeList is the window opening: 3641 in, one 3642 out
// carrying every saved point in slot order.
func TestTeleportRequestSendsTheWholeList(t *testing.T) {
	h, _, logs := teleportHandler(t,
		SavedPoint{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}},
		SavedPoint{Slot: 3, MapID: 15, Pos: [3]int32{2209, 16830, 233}})
	sess := teleportSession([3]int32{2209, 16830, 233})

	pt := teleportPacket(CmdCZTeleportList, 0)
	out, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleportList, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want exactly 1: %s", len(out), logs)
	}
	if got := cmdOf(out[0]); got != CmdZCTeleportList {
		t.Fatalf("cmd = %d, want %d (ZC_TELEPORTLIST)", got, CmdZCTeleportList)
	}
	if want := teleportListHeader + teleportListPrefix + 2*teleportListRecord; len(out[0]) != want {
		t.Fatalf("%d bytes, want %d for two points", len(out[0]), want)
	}
	// Slot order, and the slot-3 point is the SECOND record because the list is
	// positional: slot 3 is row 1.
	if got := binary.LittleEndian.Uint16(out[0][6+12:]); got != 10 {
		t.Fatalf("first record's map id = %d, want 10", got)
	}
	if got := binary.LittleEndian.Uint16(out[0][6+14+12:]); got != 15 {
		t.Fatalf("second record's map id = %d, want 15 (slot 3 is drawn second)", got)
	}
	if line := logs.String(); !strings.Contains(line, "2 saved point(s)") {
		t.Fatalf("the 3641 log line does not say how many points were sent:\n%s", line)
	}
}

// TestTeleportSaveWritesWhereTheLeaderStands is a save with no client: one row, the
// leader's own map and position, and the window told what the list is now.
func TestTeleportSaveWritesWhereTheLeaderStands(t *testing.T) {
	h, f, logs := teleportHandler(t)
	sess := teleportSession([3]int32{3946, 193, 146}) // Coimbra, standing on a real cell

	pt := teleportPacket(CmdCZTeleportSave, 0)
	out, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleportSave, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if f.saves != 1 || len(f.saved) != 1 {
		t.Fatalf("%d save(s) reached the store, want 1: %s", f.saves, logs)
	}
	got := f.saved[0]
	if got.Slot != 0 {
		t.Fatalf("slot = %d, want 0 - the first free slot", got.Slot)
	}
	if got.MapID != 15 {
		t.Fatalf("map id = %d, want 15 (the zone the leader stands in)", got.MapID)
	}
	if got.Pos != [3]int32{3946, 193, 146} {
		t.Fatalf("position = %v, want the leader's own {3946 193 146}", got.Pos)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCTeleportList {
		t.Fatalf("%d packet(s) (%v), want one ZC_TELEPORTLIST so the window redraws",
			len(out), out)
	}
	// THE LINE A READER COMPARES WITH THE WINDOW: slot, map, position.
	line := logs.String()
	for _, want := range []string{"SAVED slot 0", "map 15 (coim_basic)",
		"at (3946, 193, cell 146)"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the 3640 log line does not name %q:\n%s", want, line)
		}
	}
	t.Logf("a save logs:\n%s", line)
}

// TestTeleportSaveRefusesAnUnusablePosition: an empty/height position is refused
// BEFORE the write, and nothing is sent. A stored cell that is not a navmesh triangle
// is the crash this check exists for - the player would fall through the world on
// travel, with no error anywhere.
func TestTeleportSaveRefusesAnUnusablePosition(t *testing.T) {
	for _, pos := range [][3]int32{{0, 0, 0}, {3946, 193, -1}, {3946, 193, 2000000}} {
		h, f, logs := teleportHandler(t)
		sess := teleportSession(pos)
		pt := teleportPacket(CmdCZTeleportSave, 0)
		out, err := h.Reply(context.Background(), sess,
			protocol.Frame{Cmd: CmdCZTeleportSave, Size: len(pt), Data: pt})
		if err != nil {
			t.Fatalf("%v: Reply: %v", pos, err)
		}
		if f.saves != 0 || len(f.rows) != 0 {
			t.Fatalf("%v: %d save(s) reached the store, want 0 - the position is not "+
				"on any mesh", pos, f.saves)
		}
		if len(out) != 0 {
			t.Fatalf("%v: %d packet(s) sent, want none", pos, len(out))
		}
		if !strings.Contains(logs.String(), "REFUSED") {
			t.Fatalf("%v: nothing was logged:\n%s", pos, logs)
		}
	}
}

// TestTeleportSaveOnAFullListWritesNothing: ten slots taken, an eleventh save refused.
// The ORIGINAL server deleted the oldest point here (Commander.cpp:4939) - this one
// does not, so the refusal must be visible and nothing may be sent.
func TestTeleportSaveOnAFullListWritesNothing(t *testing.T) {
	full := make([]SavedPoint, 0, MaxSavedTeleports)
	for i := 0; i < MaxSavedTeleports; i++ {
		full = append(full, SavedPoint{Slot: i, MapID: 10, Pos: [3]int32{3946, 193, 146}})
	}
	h, f, logs := teleportHandler(t, full...)
	sess := teleportSession([3]int32{3946, 193, 146})

	pt := teleportPacket(CmdCZTeleportSave, 0)
	out, _ := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleportSave, Size: len(pt), Data: pt})
	if len(f.rows) != MaxSavedTeleports {
		t.Fatalf("%d rows after a refused save, want %d - no point may be lost",
			len(f.rows), MaxSavedTeleports)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent on a full list, want none", len(out))
	}
	if !strings.Contains(logs.String(), "slot(s) are taken") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}

// TestTeleportRemoveDeletesTheSlotBehindTheRow: the client's row 1 names slot 2, and
// that is what must be deleted - then the refreshed list goes out.
func TestTeleportRemoveDeletesTheSlotBehindTheRow(t *testing.T) {
	h, f, logs := teleportHandler(t,
		SavedPoint{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}},
		SavedPoint{Slot: 2, MapID: 15, Pos: [3]int32{2209, 16830, 233}})
	sess := teleportSession([3]int32{2209, 16830, 233})

	pt := teleportPacket(CmdCZTeleportRemove, 0, 1) // row 1
	out, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleportRemove, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(f.removes) != 1 || f.removes[0] != 2 {
		t.Fatalf("removes = %v, want [2] - row 1 of slots {0, 2} is slot 2",
			f.removes)
	}
	if len(f.rows) != 1 || f.rows[0].Slot != 0 {
		t.Fatalf("rows after the remove = %v, want only slot 0", f.rows)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCTeleportList {
		t.Fatalf("%d packet(s), want one refreshed ZC_TELEPORTLIST", len(out))
	}
	line := logs.String()
	for _, want := range []string{"slot 2 map 15", "map 15", "deleted", "1 point(s)"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the 3643 log line does not name %q:\n%s", want, line)
		}
	}
}

// TestTeleportRemoveOutOfRangeWritesNothing: a row the list does not have deletes
// nothing and sends nothing. The store is never even asked.
func TestTeleportRemoveOutOfRangeWritesNothing(t *testing.T) {
	h, f, logs := teleportHandler(t,
		SavedPoint{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}})
	sess := teleportSession([3]int32{3946, 193, 146})

	for _, row := range []byte{1, 9, 255} {
		pt := teleportPacket(CmdCZTeleportRemove, 0, row)
		out, _ := h.Reply(context.Background(), sess,
			protocol.Frame{Cmd: CmdCZTeleportRemove, Size: len(pt), Data: pt})
		if len(out) != 0 {
			t.Fatalf("row %d: %d packet(s) sent, want none", row, len(out))
		}
	}
	if len(f.removes) != 0 {
		t.Fatalf("store asked to remove %v, want nothing", f.removes)
	}
	if len(f.rows) != 1 {
		t.Fatalf("%d rows left, want the one that was there", len(f.rows))
	}
	if !strings.Contains(logs.String(), "the list has 1 point(s)") {
		t.Fatalf("the refusal does not say what the list had:\n%s", logs)
	}
}

// TestTeleportTravelSendsTheSamePacketsAsWaypointTravel is the requirement that this
// slice must NOT grow a second travel path: a saved point on Reboldoeux's own arrival
// spot must produce the identical 3593 and the identical pending map change that a
// click on the town grid produces.
func TestTeleportTravelSendsTheSamePacketsAsWaypointTravel(t *testing.T) {
	// Reboldoeux's arrival point, from its restart warp row (map 10, warp 203).
	const wantPos = "[3946 193 146]"

	h, f, logs := teleportHandler(t,
		SavedPoint{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}})
	sess := teleportSession([3]int32{2209, 16830, 233})

	pt := teleportPacket(CmdCZTeleport, 0, 0) // row 0
	got, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleport, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(f.removes) != 0 || f.saves != 0 {
		t.Fatalf("travelling wrote to the store: saves %d, removes %v", f.saves, f.removes)
	}

	// The same trip, the way it was done before this slice existed: a click on the
	// town thumbnail in the 3743 grid.
	h2, _, _ := teleportHandler(t)
	h2.teleports = teleportSettings{} // the town path needs no store
	sess2 := teleportSession([3]int32{2209, 16830, 233})
	town := teleportPacket(CmdCZWaypointSelect, 10, 0, 0xBE)
	byWaypoint, err := h2.Reply(context.Background(), sess2,
		protocol.Frame{Cmd: CmdCZWaypointSelect, Size: len(town), Data: town})
	if err != nil {
		t.Fatalf("waypoint Reply: %v", err)
	}

	if len(got) != 1 || len(byWaypoint) != 1 {
		t.Fatalf("packets: teleport %d, waypoint %d - both must send exactly the 3593",
			len(got), len(byWaypoint))
	}
	if !bytes.Equal(got[0], byWaypoint[0]) {
		t.Fatalf("the 3593 differs:\n teleport %x\n waypoint %x", got[0], byWaypoint[0])
	}
	if cmdOf(got[0]) != CmdZCMoveZone {
		t.Fatalf("cmd = %d, want %d (ZC_MOVE_ZONE)", cmdOf(got[0]), CmdZCMoveZone)
	}

	// The pending map change must be the same one, field for field.
	g1, g2 := sess.Gate, sess2.Gate
	if g1 == nil || g2 == nil {
		t.Fatalf("pending map change: teleport %v, waypoint %v - both must exist", g1, g2)
	}
	if g1.gate.ToID != g2.gate.ToID || g1.gate.To != g2.gate.To {
		t.Fatalf("destination: teleport %d (%s), waypoint %d (%s)",
			g1.gate.ToID, g1.gate.To, g2.gate.ToID, g2.gate.To)
	}
	if g1.gate.TargetX != g2.gate.TargetX || g1.gate.TargetY != g2.gate.TargetY ||
		*g1.gate.TargetCell != *g2.gate.TargetCell {
		t.Fatalf("landing: teleport (%d, %d, cell %d), waypoint (%d, %d, cell %d)",
			g1.gate.TargetX, g1.gate.TargetY, *g1.gate.TargetCell,
			g2.gate.TargetX, g2.gate.TargetY, *g2.gate.TargetCell)
	}
	// The announcement's own LastSeen differs by a few milliseconds between the two
	// handlers, so the ADDRESS is compared and not the whole record.
	if g1.dest.IP != g2.dest.IP || g1.dest.Port != g2.dest.Port ||
		g1.dest.MapID != g2.dest.MapID || g1.dest.Channel != g2.dest.Channel {
		t.Fatalf("destination zone: teleport %v, waypoint %v", g1.dest, g2.dest)
	}
	line := logs.String()
	for _, want := range []string{"CZ_TELEPORT", "slot 0 map 10 (reb_basic)",
		"(3946, 193, cell 146)", "MOVE_ZONE sent"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the 3644 log line does not name %q:\n%s", want, line)
		}
	}
	t.Logf("a travel logs:\n%s", line)
}

// TestTeleportTravelRefusesAnUnknownRow: a row the list does not have starts no map
// change at all - the worst outcome here is a 3593 the client answers with nothing.
func TestTeleportTravelRefusesAnUnknownRow(t *testing.T) {
	h, _, logs := teleportHandler(t,
		SavedPoint{Slot: 0, MapID: 10, Pos: [3]int32{3946, 193, 146}})
	sess := teleportSession([3]int32{2209, 16830, 233})

	pt := teleportPacket(CmdCZTeleport, 0, 3)
	out, _ := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleport, Size: len(pt), Data: pt})
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent, want none", len(out))
	}
	if sess.Gate != nil {
		t.Fatalf("a pending map change was left behind: %v", sess.Gate.gate)
	}
	if !strings.Contains(logs.String(), "REFUSED") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}

// TestTeleportRefusesWithoutAStore: with nothing wired in, every request is refused
// with a line and NOTHING is sent - a window that says "saved" over no database is
// worse than one that does nothing.
func TestTeleportRefusesWithoutAStore(t *testing.T) {
	h, _, logs := waypointHandler(t)
	sess := teleportSession([3]int32{3946, 193, 146})

	for _, c := range []struct {
		cmd  uint16
		body []byte
	}{
		{CmdCZTeleportList, []byte{0}},
		{CmdCZTeleportSave, []byte{0}},
		{CmdCZTeleportRemove, []byte{0, 0}},
		{CmdCZTeleport, []byte{0, 0}},
	} {
		pt := teleportPacket(c.cmd, c.body...)
		out, err := h.Reply(context.Background(), sess,
			protocol.Frame{Cmd: c.cmd, Size: len(pt), Data: pt})
		if err != nil {
			t.Fatalf("cmd %d: Reply: %v", c.cmd, err)
		}
		if len(out) != 0 {
			t.Fatalf("cmd %d: %d packet(s) sent with no store, want none", c.cmd, len(out))
		}
	}
	if n := strings.Count(logs.String(), "no saved-point store is wired in"); n != 4 {
		t.Fatalf("%d refusal line(s), want 4 - one per request:\n%s", n, logs)
	}
}

// TestTeleportRefusesWithoutACommander: a session whose commander was never resolved
// cannot own a list, so nothing is read, written or sent.
func TestTeleportRefusesWithoutACommander(t *testing.T) {
	h, f, logs := teleportHandler(t)
	sess := teleportSession([3]int32{3946, 193, 146})
	sess.CommanderID = 0

	pt := teleportPacket(CmdCZTeleportSave, 0)
	out, _ := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: CmdCZTeleportSave, Size: len(pt), Data: pt})
	if len(out) != 0 || f.saves != 0 {
		t.Fatalf("sent %d packet(s) and wrote %d row(s) with no commander",
			len(out), f.saves)
	}
	if !strings.Contains(logs.String(), "the commander was never resolved") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}

// TestSavedTeleportRowsInPostgreSQL is the REAL write test: the migration, one row in
// `saved_teleport`, its exact contents read back, then removal. It is off by default
// because it writes; commander 25 is the throwaway account and every row it makes is
// deleted again.
//
//	GE_SAVED_TELEPORT_DB_TEST=1 go test ./zone -run TestSavedTeleportRowsInPostgreSQL -v
func TestSavedTeleportRowsInPostgreSQL(t *testing.T) {
	if os.Getenv("GE_SAVED_TELEPORT_DB_TEST") == "" {
		t.Skip("set GE_SAVED_TELEPORT_DB_TEST=1 and start granado_pg to run the write test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("PostgreSQL is not reachable, so the saved-point write was NOT "+
			"checked: %v", err)
	}
	defer pool.Close()
	// The schema the binary carries, applied the same way a start applies it.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)

	const commander = 25 // the throwaway test commander, never 24
	// The rows this commander already had, put back EXACTLY at the end: a row
	// re-inserted with a fresh saved_at is not the row that was there. The old
	// code deleted every row of commander 25 and left none, which ate the one
	// saved point the throwaway account really holds.
	hadRows := snapshotSavedTeleports(t, ctx, pool, commander)
	defer restoreSavedTeleports(t, pool, commander, hadRows)
	if _, err := pool.Exec(ctx,
		`DELETE FROM saved_teleport WHERE commander_id = $1`, commander); err != nil {
		t.Fatalf("clear commander %d before the test: %v", commander, err)
	}

	// (1) A SAVE writes the row the client asked for: the server picks slot 0.
	want := SavedPoint{MapID: 15, Pos: [3]int32{2209, 16830, 233}, Name: "coim_basic"}
	got, err := store.SaveSavedTeleport(ctx, commander, 0, -1, want)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if got.Slot != 0 {
		t.Fatalf("slot = %d, want 0 (the first free slot)", got.Slot)
	}
	// QUOTED ROW: the database's own columns, not the struct we just built.
	var (
		slot, mapID, x, y, cell int
		name                    string
	)
	if err := pool.QueryRow(ctx,
		`SELECT slot, map_id, x, y, cell, name FROM saved_teleport
		  WHERE commander_id = $1`, commander).
		Scan(&slot, &mapID, &x, &y, &cell, &name); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	t.Logf("saved_teleport row: commander_id=%d slot=%d map_id=%d x=%d y=%d cell=%d name=%q",
		commander, slot, mapID, x, y, cell, name)
	if slot != 0 || mapID != 15 || x != 2209 || y != 16830 || cell != 233 || name != "coim_basic" {
		t.Fatalf("row = {%d %d %d %d %d %q}, want {0 15 2209 16830 233 \"coim_basic\"}",
			slot, mapID, x, y, cell, name)
	}

	// (2) An out-of-range slot writes nothing.
	if _, err := store.SaveSavedTeleport(ctx, commander, 0, MaxSavedTeleports,
		SavedPoint{MapID: 10, Pos: [3]int32{1, 2, 3}}); err == nil {
		t.Fatalf("a save into slot %d was accepted, want a refusal", MaxSavedTeleports)
	}
	// (3) A position that cannot be a cell writes nothing.
	if _, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
		SavedPoint{MapID: 10, Pos: [3]int32{1, 2, -5}}); err == nil {
		t.Fatalf("a save at cell -5 was accepted, want a refusal")
	}
	// (4) The second save takes slot 1, not slot 0 again.
	second, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
		SavedPoint{MapID: 10, Pos: [3]int32{3946, 193, 146}, Name: "reb_basic"})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if second.Slot != 1 {
		t.Fatalf("second slot = %d, want 1", second.Slot)
	}
	rows, err := store.SavedTeleports(ctx, commander)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d row(s), want 2 (the refused saves must not have written)", len(rows))
	}

	// (4b) THE DUPLICATE RULE (R76 slice 5), on the REAL SQL: the row saved in step (1)
	// is map 15 at (2209, 16830, cell 233). Saving that exact spot again under a
	// DIFFERENT name is the same point, so it is refused and nothing is written - the
	// name the player typed never enters the test.
	before := len(rows)
	if _, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
		SavedPoint{MapID: 15, Pos: [3]int32{2209, 16830, 233}, Name: "coim_basic again"}); !errors.Is(err, errDuplicatePoint) {
		t.Fatalf("the same map+x+y+cell under another name returned %v, want %v",
			err, errDuplicatePoint)
	}
	if rows, err = store.SavedTeleports(ctx, commander); err != nil {
		t.Fatalf("list after the duplicate: %v", err)
	}
	if len(rows) != before {
		t.Fatalf("%d row(s) after the duplicate refusal, want %d - it must write nothing",
			len(rows), before)
	}

	// (5) A REMOVE deletes exactly that row; the other one stays.
	gone, deleted, err := store.RemoveSavedTeleport(ctx, commander, 0, 0)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !deleted || gone.Slot != 0 || gone.MapID != 15 {
		t.Fatalf("removed %+v (deleted %v), want slot 0 map 15", gone, deleted)
	}
	rows, err = store.SavedTeleports(ctx, commander)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(rows) != 1 || rows[0].Slot != 1 {
		t.Fatalf("rows after the remove = %+v, want only slot 1", rows)
	}
	// A second remove of the same slot is a no-op, not an error.
	if _, deleted, err := store.RemoveSavedTeleport(ctx, commander, 0, 0); err != nil || deleted {
		t.Fatalf("second remove of slot 0: deleted %v, err %v - want a quiet no-op",
			deleted, err)
	}

	// (6) THE OTHER HALF OF THE DUPLICATE RULE: the same x, y and cell on a DIFFERENT
	// map is a different place, so it is saved. This is why there is no unique index on
	// (commander_id, map_id, x, y, cell) - the coordinates are not a key, and a player
	// may legitimately hold one spot per map.
	if _, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
		SavedPoint{MapID: 10, Pos: [3]int32{2209, 16830, 233}, Name: "reb at coimbra coords"}); err != nil {
		t.Fatalf("the same x/y/cell on map 10 was refused (%v), want it saved", err)
	}
	if rows, err = store.SavedTeleports(ctx, commander); err != nil {
		t.Fatalf("list after the other-map save: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d row(s) after the other-map save, want 2", len(rows))
	}
	if last := rows[1]; last.MapID != 10 || last.Pos != [3]int32{2209, 16830, 233} {
		t.Fatalf("the other-map row is map %d %v, want map 10 (2209, 16830, cell 233)",
			last.MapID, last.Pos)
	}
}

// TestSavedTeleportOrderInPostgreSQL is the REAL check of the order rules, because that
// is where the bug lived: the SQL, not the handler. It is off by default for the same
// reason the test above is - it writes - and it uses commander 25, the throwaway
// account, clearing every row it makes.
//
//	GE_SAVED_TELEPORT_DB_TEST=1 go test ./zone -run TestSavedTeleportOrderInPostgreSQL -v
func TestSavedTeleportOrderInPostgreSQL(t *testing.T) {
	if os.Getenv("GE_SAVED_TELEPORT_DB_TEST") == "" {
		t.Skip("set GE_SAVED_TELEPORT_DB_TEST=1 and start granado_pg to run the write test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("PostgreSQL is not reachable, so the saved-point ORDER was NOT "+
			"checked: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)

	const commander = 25 // the throwaway test commander, never 24
	// The rows this commander already had, put back EXACTLY at the end.
	hadRows := snapshotSavedTeleports(t, ctx, pool, commander)
	defer restoreSavedTeleports(t, pool, commander, hadRows)
	if _, err := pool.Exec(ctx,
		`DELETE FROM saved_teleport WHERE commander_id = $1`, commander); err != nil {
		t.Fatalf("clear commander %d: %v", commander, err)
	}

	save := func(name string, cell int32) SavedPoint {
		got, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
			SavedPoint{MapID: 15, Pos: [3]int32{2209, 16830, cell}, Name: name})
		if err != nil {
			t.Fatalf("save %q: %v", name, err)
		}
		return got
	}
	names := func() string {
		rows, err := store.SavedTeleports(ctx, commander)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var b []byte
		for i, r := range rows {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, r.Name...)
		}
		return string(b)
	}

	// Four saves, in order. The list is the save order.
	for i, name := range []string{"a", "b", "c", "d"} {
		save(name, int32(100+i))
	}
	if got := names(); got != "a,b,c,d" {
		t.Fatalf("after four saves the list is %s, want a,b,c,d", got)
	}

	// The owner's defect: delete the SECOND row, then save. The new row must be LAST,
	// and it may reuse the free slot - the slot is not the order any more.
	rows, err := store.SavedTeleports(ctx, commander)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, deleted, err := store.RemoveSavedTeleport(ctx, commander, 0, rows[1].Slot); err != nil ||
		!deleted {
		t.Fatalf("remove the second row: deleted %v, err %v", deleted, err)
	}
	fresh := save("new", 200)
	if fresh.Slot != rows[1].Slot {
		t.Logf("the new row took slot %d (the freed one was %d) - either is fine, the "+
			"slot is only the storage key", fresh.Slot, rows[1].Slot)
	}
	if got := names(); got != "a,c,d,new" {
		t.Fatalf("after a delete + a save the list is %s, want a,c,d,new - a new point "+
			"goes to the BOTTOM", got)
	}

	// A move, and the order must survive a re-read: row 0 -> row 1.
	moved, err := store.MoveSavedTeleport(ctx, commander, 0, 0, 1)
	if err != nil {
		t.Fatalf("move 0 -> 1: %v", err)
	}
	if got := names(); got != "c,a,d,new" {
		t.Fatalf("after moving row 0 to row 1 the list is %s, want c,a,d,new", got)
	}
	var returned []byte
	for i, r := range moved {
		if i > 0 {
			returned = append(returned, ',')
		}
		returned = append(returned, r.Name...)
	}
	if string(returned) != "c,a,d,new" {
		t.Fatalf("the move returned %s, want c,a,d,new - the same order a re-read gives",
			returned)
	}

	// An index that is not a row changes nothing.
	if _, err := store.MoveSavedTeleport(ctx, commander, 0, 3, 9); err == nil {
		t.Fatalf("a move to row 9 of a 4-row list was accepted, want a refusal")
	}
	if got := names(); got != "c,a,d,new" {
		t.Fatalf("a refused move changed the list to %s, want c,a,d,new", got)
	}

	// The cap counts ROWS IN THIS LIST and it is MaxSavedTeleports (ten): fill the list
	// to ten, then prove the eleventh is refused. Every cell is distinct on purpose -
	// since R76 slice 5 a repeated map+x+y+cell is a DUPLICATE, which is a different
	// refusal and would make this a different test.
	save("e", 201)
	for i, name := range []string{"f", "g", "h", "i", "j"} {
		save(name, int32(202+i))
	}
	if got := names(); got != "c,a,d,new,e,f,g,h,i,j" {
		t.Fatalf("the list is %s, want ten rows c,a,d,new,e,f,g,h,i,j", got)
	}
	if _, err := store.SaveSavedTeleport(ctx, commander, 0, -1,
		SavedPoint{MapID: 15, Pos: [3]int32{1, 2, 400}, Name: "eleventh"}); !errors.Is(err,
		errNoFreeSlot) {
		t.Fatalf("the eleventh save returned %v, want errNoFreeSlot", err)
	}
	if got := names(); got != "c,a,d,new,e,f,g,h,i,j" {
		t.Fatalf("the list is %s, want ten rows - a refused save writes nothing", got)
	}

	// A MOVE RETURNS THE WHOLE ROW SET (R76 slice 6). The caller sends that answer
	// straight out as the four 3634 lists, so a move inside list 0 that returned only
	// list 0 would draw the other three panels EMPTY on the client. This is the check
	// that pins it, and it is the one the in-memory fake cannot make: the fake returns
	// everything by construction, the SQL has to do it on purpose.
	if _, err := store.SaveSavedTeleport(ctx, commander, 2, -1,
		SavedPoint{TelType: 2, MapID: 15, Pos: [3]int32{3, 4, 500},
			Name: "extend"}); err != nil {
		t.Fatalf("save into list 2: %v", err)
	}
	movedAll, err := store.MoveSavedTeleport(ctx, commander, 0, 0, 1)
	if err != nil {
		t.Fatalf("move 0 -> 1 in list 0: %v", err)
	}
	if len(movedAll) != MaxSavedTeleports+1 {
		t.Fatalf("the move returned %d row(s), want %d - every list, not just the one "+
			"that moved", len(movedAll), MaxSavedTeleports+1)
	}
	var perList [SavedTeleportLists][]string
	for _, r := range movedAll {
		perList[r.TelType] = append(perList[r.TelType], r.Name)
	}
	if got := strings.Join(perList[0], ","); got != "a,c,d,new,e,f,g,h,i,j" {
		t.Fatalf("list 0 after the move = %s, want a,c,d,new,e,f,g,h,i,j", got)
	}
	if got := strings.Join(perList[2], ","); got != "extend" {
		t.Fatalf("list 2 after a move in list 0 = %s, want extend - the other lists must "+
			"survive the move", got)
	}
}

// savedTeleportFull is every column of a saved_teleport row, so a restore is a
// restore and not a re-creation with a fresh saved_at.
type savedTeleportFull struct {
	CommanderID  int32
	Slot         int32
	TelType      int16
	MapID        int32
	X, Y, Cell   int32
	Name         string
	SavedAt      time.Time
	DisplayOrder int32
}

// snapshotSavedTeleports reads every list of one commander, in a stable order.
func snapshotSavedTeleports(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32) []savedTeleportFull {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT commander_id, slot, teltype, map_id, x, y, cell, name, saved_at,
		        display_order
		   FROM saved_teleport WHERE commander_id = $1
		  ORDER BY teltype, slot`, commanderID)
	if err != nil {
		t.Fatalf("snapshot the saved points of commander %d: %v", commanderID, err)
	}
	defer rows.Close()
	var out []savedTeleportFull
	for rows.Next() {
		var r savedTeleportFull
		if err := rows.Scan(&r.CommanderID, &r.Slot, &r.TelType, &r.MapID, &r.X, &r.Y,
			&r.Cell, &r.Name, &r.SavedAt, &r.DisplayOrder); err != nil {
			t.Fatalf("scan the saved-point snapshot: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("saved points of commander %d: %v", commanderID, err)
	}
	return out
}

// restoreSavedTeleports puts the snapshot back EXACTLY, including saved_at and
// the display order. A fresh context, because the test's own may have been
// cancelled when a fatal failure unwinds into the defer.
func restoreSavedTeleports(t *testing.T, pool *pgxpool.Pool, commanderID int32, rows []savedTeleportFull) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`DELETE FROM saved_teleport WHERE commander_id = $1`, commanderID); err != nil {
		t.Errorf("restore: clear the saved points of commander %d: %v", commanderID, err)
		return
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx,
			`INSERT INTO saved_teleport
			        (commander_id, slot, teltype, map_id, x, y, cell, name, saved_at,
			         display_order)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			commanderID, r.Slot, r.TelType, r.MapID, r.X, r.Y, r.Cell, r.Name,
			r.SavedAt, r.DisplayOrder); err != nil {
			t.Errorf("restore: saved point slot %d teltype %d of commander %d: %v",
				r.Slot, r.TelType, commanderID, err)
		}
	}
}
