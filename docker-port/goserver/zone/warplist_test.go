package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"geserver/protocol"
)

// THE WARP LIST (`warplist`): the player's own saved points, DRAWN by 3634 and
// WRITTEN by 3635 SAVE / 3636 REMOVE / 3638 RENAME.
//
// The layout pinned here comes from the SHIPPED BINARY, and it disagrees with
// source2 in two places. source2 says a 45-byte record with a 31-byte memo, reached
// through `struct PKS_ZC_COMMWARP_LIST{ BYTE teltype; BYTE curCount; }`; the client
// says:
//
//	0x4b6d65 PUSH 0x2e               ; the copy size, 46
//	0x4b6d74 ADD ESI,0x2e            ; the stride, 46
//	records at +6, x i32 @+0, y i32 @+4, cell i32 @+8, mapId u16 @+12, memo char[32] @+14
//
// and `len` is the TOTAL packet, header included.
//
// THERE IS NO REPLY PACKET for the three writes: the zone dispatcher FUN_004d76fd
// has no case for 3635-3639 and the client has no receive handler. Confirmation is a
// REFRESH - 3634 for all four types - so every successful write test below asserts the
// four 3634 packets.
//
// A SUCCESSFUL SAVE ALSO SAYS SO IN WORDS (R76 slice 5, 2026-10-30). The live reference
// server prints "The warp location has been saved successfully", and a duplicate save
// prints "This warp point has already been saved." - the owner's screenshots. Both
// strings are sent here VERBATIM as one ZC_MESSAGE (3596). That is not a new reply
// packet: a system line is the ordinary channel. A remove, rename or swap still answers
// with the lists alone, because only the save was seen on the reference.
//
// A REFUSAL is the other half, and since 2026-10-30 it is no longer silent: it sends
// ONE ZC_MESSAGE (3596) line naming the reason, so a refused click cannot read as a
// dead button (the owner's own words: "the eleventh is silent fail? is it intended?").
// Every refusal test below therefore asserts EXACTLY ONE 3596 packet with the exact
// text, plus the ONE log line that stays the record.

// wantOneWarpRefusalMessage is the player-facing half of a refusal: EXACTLY one packet,
// cmd 3596, a length word that describes the bytes, a general type/level (a chat line,
// never a modal box), and the expected ASCII text at +45 with one NUL after it.
//
// "Exactly one" is the rule being pinned: a refusal must SAY SO on screen, and two
// signals for one event is noise.
func wantOneWarpRefusalMessage(t *testing.T, out [][]byte, wantText string) {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("%d packet(s) sent on a refusal, want exactly 1 (one ZC_MESSAGE line)",
			len(out))
	}
	msg := out[0]
	if cmd := cmdOf(msg); cmd != CmdZCMessage {
		t.Fatalf("the refusal packet is cmd %d, want %d (ZC_MESSAGE)", cmd, CmdZCMessage)
	}
	if length := int(binary.LittleEndian.Uint16(msg[2:])); length != len(msg) {
		t.Fatalf("length word = %d, the packet is %d bytes (it must be the WHOLE packet)",
			length, len(msg))
	}
	if v := binary.LittleEndian.Uint32(msg[4:]); v != MessageTypeGeneral {
		t.Fatalf("type = %d, want %d (a general line, not a modal box)", v, MessageTypeGeneral)
	}
	if msg[8] != MessageLevelGeneral {
		t.Fatalf("level = %d, want %d (level>0 pops a modal box)", msg[8],
			MessageLevelGeneral)
	}
	if msg[len(msg)-1] != 0 {
		t.Fatalf("the message has no NUL terminator")
	}
	text := string(msg[45 : len(msg)-1])
	if text != wantText {
		t.Fatalf("the message reads %q, want %q", text, wantText)
	}
	// ASCII only, and short: the client copies the line into `char txt[256]`
	// (GEPacket.cpp, packet::ServerMsg) and draws it in the system font.
	if len(text) > 200 {
		t.Fatalf("the message is %d bytes; the client's own buffer is char[256]", len(text))
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			t.Fatalf("the message has a non-ASCII byte at %d: %q", i, text)
		}
	}
	t.Logf("refusal message: %s", text)
}

// warpRow is one saved point in the shape the window draws: slot, map, position, memo.
// It is a row of the FIRST list (teltype 0), which is what most tests want.
func warpRow(slot, mapID int, x, y, cell int32, memo string) SavedPoint {
	return SavedPoint{Slot: slot, MapID: mapID, Pos: [3]int32{x, y, cell}, Name: memo}
}

// warpRowIn is a saved point in ONE named list. Since R76 slice 6 the four panels are
// four lists, and each numbers its own slots and its own display order from 0, so a test
// that puts a row in "Warp Extend(2)" has to say so.
func warpRowIn(telType, slot, mapID int, x, y, cell int32, memo string) SavedPoint {
	return SavedPoint{TelType: telType, Slot: slot, MapID: mapID, Pos: [3]int32{x, y, cell},
		Name: memo}
}

// TestWarpListExactBytesForOneRow pins the WHOLE 52-byte packet for one row, byte for
// byte. Why exact bytes: the count the client draws with is the byte at +5 and the
// records it walks are 46 bytes from +6, so a length word written as "records only"
// or a 45-byte stride draws the wrong rows with no error anywhere.
func TestWarpListExactBytesForOneRow(t *testing.T) {
	// Map 15 (Coimbra), standing at (3175, 4203, navmesh cell 77), memo "Bank".
	got := BuildCommWarpListRows(commWarpOwnList,
		[]SavedPoint{warpRow(2, 15, 3175, 4203, 77, "Bank")})

	// cmd(2) + len(2) + teltype(1) + count(1) + one 46-byte record
	if want := 6 + 46; len(got) != want {
		t.Fatalf("packet = %d bytes, want %d (6 header + one 46-byte record)", len(got), want)
	}
	// "320e" cmd 3634, "3400" len 52 = 0x0034, "00" teltype, "01" count,
	// "670c0000" x 3175, "6b100000" y 4203, "4d000000" cell 77, "0f00" map 15,
	// "42616e6b" the memo "Bank", then NULs to fill the 32-byte memo field.
	want := "320e34000001" + "670c0000" + "6b100000" + "4d000000" + "0f00" +
		"42616e6b" + strings.Repeat("00", 28)
	if now := hex.EncodeToString(got); now != want {
		t.Fatalf("packet hex:\n got %s\nwant %s", now, want)
	}

	// The length word is the TOTAL, and it agrees with the bytes on the wire.
	if length := int(binary.LittleEndian.Uint16(got[2:])); length != len(got) {
		t.Fatalf("len word = %d, packet is %d bytes (it must be the TOTAL)", length, len(got))
	}
	// The map id sits at +12 of the RECORD, which is +18 of the packet.
	if id := binary.LittleEndian.Uint16(got[commWarpRecordsAt+commWarpMapID:]); id != 15 {
		t.Fatalf("map id at packet +%d = %d, want 15",
			commWarpRecordsAt+commWarpMapID, id)
	}
	// The cell is the record's THIRD int and it is a navmesh triangle, never a height.
	if cell := int32(binary.LittleEndian.Uint32(got[commWarpRecordsAt+8:])); cell != 77 {
		t.Fatalf("cell at record +8 = %d, want 77", cell)
	}
	// The memo field is 32 bytes with the NUL INSIDE it: source2's 31 would leave one
	// byte of every record unwritten and mis-segment the next row.
	memo := got[commWarpRecordsAt+commWarpMemo:]
	if len(memo) != 32 {
		t.Fatalf("memo field = %d bytes, want 32", len(memo))
	}
	if string(memo[:4]) != "Bank" || !bytes.Equal(memo[4:], make([]byte, 28)) {
		t.Fatalf("memo field = %x, want \"Bank\" then 28 NULs", memo)
	}
}

// TestWarpListLengthIsTheTotal walks every count this server can produce: the length
// word is 6 + 46*count, the count byte agrees with it, and a type with no rows is
// exactly the 6-byte packet it has always been.
func TestWarpListLengthIsTheTotal(t *testing.T) {
	for n := 0; n <= MaxSavedTeleports; n++ {
		rows := make([]SavedPoint, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, warpRow(i, 15+i, int32(100+i), int32(200+i), int32(i)+1,
				fmt.Sprintf("p%d", i)))
		}
		got := BuildCommWarpListRows(commWarpOwnList, rows)

		if want := commWarpRecordsAt + commWarpRecord*n; len(got) != want {
			t.Fatalf("count %d: packet = %d bytes, want %d (6 + 46*count)",
				n, len(got), want)
		}
		if length := int(got[2]) | int(got[3])<<8; length != len(got) {
			t.Fatalf("count %d: len word = %d, packet is %d bytes (it must be the TOTAL)",
				n, length, len(got))
		}
		if got[4] != commWarpOwnList {
			t.Fatalf("count %d: teltype = %d, want %d", n, got[4], commWarpOwnList)
		}
		if int(got[5]) != n {
			t.Fatalf("count %d: count byte = %d, want %d", n, got[5], n)
		}
		if n == 0 && hex.EncodeToString(got) != "320e06000000" {
			t.Fatalf("empty type 0 = %x, want the 6-byte 320e06000000", got)
		}
	}
}

// TestWarpListEmptyListIsUnchanged is the "do not break the entry burst" line: with no
// saved points, the four 3634s must be the SAME BYTES the burst has always sent, so a
// player with an empty list cannot tell this change happened.
func TestWarpListEmptyListIsUnchanged(t *testing.T) {
	before, _ := EntryBurst(nil, nil)
	after, _ := EntryBurstWithSavedTeleports(nil, nil, nil)

	if len(before) != len(after) {
		t.Fatalf("%d packet(s) before, %d after - an empty list must not add or drop any",
			len(before), len(after))
	}
	for i := range before {
		if !bytes.Equal(before[i], after[i]) {
			t.Fatalf("packet %d changed: %x -> %x", i, before[i], after[i])
		}
	}
	warps := 0
	for _, p := range after {
		if cmdOf(p) != CmdZCCommWarpList {
			continue
		}
		warps++
		if hex.EncodeToString(p) != hex.EncodeToString(BuildCommWarpList(int(p[4]))) {
			t.Fatalf("type %d is no longer the 6-byte empty list: %x", p[4], p)
		}
	}
	if warps != commWarpTypes {
		t.Fatalf("%d commwarp packet(s), want %d", warps, commWarpTypes)
	}
}

// TestWarpListSendsFourTypesInOrder is the entry send: type 0 carries the player's
// points, each of the four lists carries its own, and all four go out in type order - the
// client replaces `COMM_WARP_LIST[teltype]` on each one, so a type left out of a send
// keeps whatever it held before.

// TestWarpListSendsFourTypesInOrder is the entry send: EACH list carries its own points
// in its own order, all four go out in type order, and a list with no rows is still the
// 6-byte empty packet - the client replaces `COMM_WARP_LIST[teltype]` on each one, so a
// type left out of a send keeps whatever it held before.
func TestWarpListSendsFourTypesInOrder(t *testing.T) {
	rows := []SavedPoint{
		warpRow(0, 15, 3175, 4203, 77, "Bank"),
		warpRow(1, 10, 100, 200, 5, ""),
		warpRowIn(2, 0, 15, 1200, 1300, 9, "extend one"),
		warpRowIn(2, 1, 15, 1400, 1500, 11, "extend two"),
	}
	packets, _ := EntryBurstWithSavedTeleports(nil, nil, rows)

	var warps [][]byte
	for _, p := range packets {
		if cmdOf(p) == CmdZCCommWarpList {
			warps = append(warps, p)
		}
	}
	wantWarpCounts(t, warps, [commWarpTypes]int{2, 0, 2, 0})

	// An empty memo is not a blank row: the client draws the map name instead, so the
	// record still has to be there with a NUL-only memo field.
	if memo := warps[0][commWarpRecordsAt+commWarpRecord+commWarpMemo:]; len(memo) != 32 ||
		!bytes.Equal(memo, make([]byte, 32)) {
		t.Fatalf("the second row's memo = %x, want 32 NULs", memo)
	}
	// Type 2's two rows are ITS rows, in ITS order, with ITS memos.
	if memo := string(warps[2][commWarpRecordsAt+commWarpMemo:][:10]); memo != "extend one" {
		t.Fatalf("type 2 row 0 memo = %q, want %q", memo, "extend one")
	}
	if memo := string(warps[2][commWarpRecordsAt+commWarpRecord+commWarpMemo:][:10]); memo != "extend two" {
		t.Fatalf("type 2 row 1 memo = %q, want %q", memo, "extend two")
	}
	for _, i := range []int{1, 3} {
		if length := int(warps[i][2]) | int(warps[i][3])<<8; length != 6 {
			t.Fatalf("type %d len word = %d, want 6 (the byte-identical empty list)", i,
				length)
		}
	}
}

// TestParseCommWarpRequestsFromTheClientLayout pins all three request layouts: the
// payload starts at +10 because every one of them carries PKS_SECURITY_DEFAULT.
func TestParseCommWarpRequestsFromTheClientLayout(t *testing.T) {
	// 3635 SAVE: teltype @+10, memo[31] @+11, 42 bytes.
	save := pack(CmdCZCommWarpSave, append([]byte{2},
		append([]byte("Reboldoeux gate"), make([]byte, commWarpMemoField-15)...)...)...)
	if len(save) != commWarpSaveSize {
		t.Fatalf("save packet is %d bytes, want %d (the client's own size)", len(save),
			commWarpSaveSize)
	}
	telType, memo, ok := ParseCommWarpSave(save)
	if !ok || telType != 2 || memo != "Reboldoeux gate" {
		t.Fatalf("save parsed as teltype %d memo %q ok=%v, want 2 and %q", telType, memo,
			ok, "Reboldoeux gate")
	}

	// 3636 REMOVE: index @+10, teltype @+11, 12 bytes.
	remove := pack(CmdCZCommWarpRemove, 3, 0)
	if len(remove) != commWarpRowSize {
		t.Fatalf("remove packet is %d bytes, want %d", len(remove), commWarpRowSize)
	}
	index, telType, ok := ParseCommWarpRow(remove)
	if !ok || index != 3 || telType != commWarpOwnList {
		t.Fatalf("remove parsed as row %d teltype %d ok=%v, want 3 and 0", index, telType, ok)
	}

	// 3638 RENAME: index @+10, teltype @+11, memo[31] @+12, 43 bytes.
	rename := pack(CmdCZCommWarpRename,
		append([]byte{1, 0}, append([]byte("Home"), make([]byte, commWarpMemoField-4)...)...)...)
	if len(rename) != commWarpRenameSize {
		t.Fatalf("rename packet is %d bytes, want %d", len(rename), commWarpRenameSize)
	}
	index, telType, memo, ok = ParseCommWarpRename(rename)
	if !ok || index != 1 || telType != commWarpOwnList || memo != "Home" {
		t.Fatalf("rename parsed as row %d teltype %d memo %q ok=%v, want 1, 0, %q",
			index, telType, memo, ok, "Home")
	}

	// A packet too short to hold the payload is refused, never read out of bounds.
	for _, n := range []int{0, 4, 10} {
		if _, _, ok := ParseCommWarpSave(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a save; the teltype needs 11", n)
		}
		if _, _, ok := ParseCommWarpRow(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a row; the teltype needs 12", n)
		}
		if _, _, _, ok := ParseCommWarpRename(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a rename; the row needs 12", n)
		}
	}
}

// warpStore is savedTeleportFake plus the rename and move hooks, so a Warp List test can
// drive the real 3638 and 3639 handlers. Embedding keeps the three existing hooks
// untouched: the teleportation window's own tests still wire exactly those three.
type warpStore struct {
	*savedTeleportFake
	renames []string
	// moves records every (from, to) pair the handler asked for, so "a refusal never
	// reaches the store" is assertable.
	moves [][2]int
}

func (w *warpStore) rename(_ context.Context, _ int32, telType, slot int, name string) (SavedPoint, bool, error) {
	if w.err != nil {
		return SavedPoint{}, false, w.err
	}
	for i, r := range w.rows {
		if r.TelType == telType && r.Slot == slot {
			w.rows[i].Name = name
			w.renames = append(w.renames, name)
			return w.rows[i], true, nil
		}
	}
	return SavedPoint{}, false, nil
}

// move is PGStore.MoveSavedTeleport's contract in memory: take the row at `from` out of
// the NAMED list's DISPLAY order and splice it in at `to`, renumbering that list's rows
// and storing the result. The splice is written out the long way rather than swapped so
// the fake proves the same thing the store does: the rows in between shift by one and
// keep their relative order, and the other lists are untouched. It returns the whole row
// set, like the store.
func (w *warpStore) move(ctx context.Context, _ int32, telType, from, to int) ([]SavedPoint, error) {
	if w.err != nil {
		return nil, w.err
	}
	all, err := w.load(ctx, 0)
	if err != nil {
		return nil, err
	}
	list := rowsOfType(all, telType)
	if from < 0 || from >= len(list) || to < 0 || to >= len(list) {
		return nil, fmt.Errorf("rows %d -> %d of a %d-row list %d: %w", from, to, len(list),
			telType, errNoSuchRow)
	}
	moved := list[from]
	spliced := make([]SavedPoint, 0, len(list))
	spliced = append(spliced, list[:from]...)
	spliced = append(spliced, list[from+1:]...)
	spliced = append(spliced, SavedPoint{})
	copy(spliced[to+1:], spliced[to:])
	spliced[to] = moved
	// The renumbered list replaces the old one inside the fake's row set; the other
	// lists keep their own rows and their own order.
	kept := make([]SavedPoint, 0, len(all)-len(list))
	for _, r := range all {
		if r.TelType != telType {
			kept = append(kept, r)
		}
	}
	for i := range spliced {
		spliced[i].Order = i
	}
	w.moves = append(w.moves, [2]int{from, to})
	w.rows = append(kept, spliced...)
	out := append([]SavedPoint(nil), w.rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TelType != out[j].TelType {
			return out[i].TelType < out[j].TelType
		}
		return out[i].Order < out[j].Order
	})
	return out, nil
}

// warpListHandler is a zone that can serve the Warp List: Coimbra, a saved-point store,
// the rename and move hooks, and a log the test can read.
func warpListHandler(t *testing.T, rows ...SavedPoint) (*Handler, *warpStore, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	h := &Handler{
		Log:     log.New(logs, "", 0),
		MapID:   15,
		MapName: "coim_basic",
	}
	f := &warpStore{savedTeleportFake: &savedTeleportFake{rows: rows}}
	h.UseSavedTeleports(f.load, f.save, f.remove)
	h.UseSavedPointRename(f.rename)
	h.UseSavedPointMove(f.move)
	return h, f, logs
}

// warpSavePacket is one CZ_COMMWARP_SAVE the way the client writes it: a 10-byte
// security header, the teltype, then the memo in its 31-byte field.
func warpSavePacket(telType byte, memo string) []byte {
	body := make([]byte, 1+commWarpMemoField)
	body[0] = telType
	copy(body[1:], memo)
	return pack(CmdCZCommWarpSave, body...)
}

// warpRowPacket is one 3636 or 3638: the list row, the teltype, and (for a rename)
// the new memo.
func warpRowPacket(cmd uint16, row, telType byte, memo string) []byte {
	body := []byte{row, telType}
	if cmd == CmdCZCommWarpRename {
		body = append(body, make([]byte, commWarpMemoField)...)
		copy(body[2:], memo)
	}
	return pack(cmd, body...)
}

// warpReply hands one request to the zone through the REAL dispatch, so a lost case
// in zone.go fails here.
func warpReply(t *testing.T, h *Handler, sess *Session, cmd uint16, pt []byte) [][]byte {
	t.Helper()
	out, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: cmd, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("cmd %d: %v", cmd, err)
	}
	return out
}

// logLines is the number of lines a handler printed.
func logLines(buf *bytes.Buffer) []string {
	s := strings.TrimSuffix(buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// wantFourWarpLists is the confirmation check: four 3634 packets, types 0..3 in
// order, and - when rows are expected - they are on the wire in type 0.

// wantWarpCounts pins ONE answer's four 3634 lists: types 0..3 in that order, each
// carrying EXACTLY the given number of rows, a count byte that agrees with the records,
// a length word that describes the whole packet, and - for an empty list - the
// byte-identical 6-byte packet the entry burst has always sent.
//
// Per-list counts are the point since R76 slice 6: the four panels are four independent
// lists of ten, so "how many rows" is only a complete question with a list number.
func wantWarpCounts(t *testing.T, out [][]byte, want [commWarpTypes]int) {
	t.Helper()
	if len(out) != commWarpTypes {
		t.Fatalf("%d packet(s), want %d (one 3634 per list)", len(out), commWarpTypes)
	}
	for i, p := range out {
		if cmdOf(p) != CmdZCCommWarpList {
			t.Fatalf("packet %d is cmd %d, want %d", i, cmdOf(p), CmdZCCommWarpList)
		}
		if int(p[4]) != i {
			t.Fatalf("packet %d carries teltype %d, want %d in this position", i, p[4], i)
		}
		if n := int(p[5]); n != want[i] {
			t.Fatalf("type %d count byte = %d, want %d", i, n, want[i])
		}
		if got, wantLen := len(p), commWarpRecordsAt+commWarpRecord*want[i]; got != wantLen {
			t.Fatalf("type %d = %d bytes, want %d (%d row(s))", i, got, wantLen, want[i])
		}
		if length := int(binary.LittleEndian.Uint16(p[2:])); length != len(p) {
			t.Fatalf("type %d len word = %d, the packet is %d bytes - it must be the "+
				"WHOLE packet", i, length, len(p))
		}
	}
}

// wantFourWarpLists is wantWarpCounts for the common case: rows in the FIRST list only.
func wantFourWarpLists(t *testing.T, out [][]byte, rows int) {
	t.Helper()
	wantWarpCounts(t, out, [commWarpTypes]int{rows})
}

// wantMessageText pins ONE ZC_MESSAGE packet on its bytes: cmd 3596, a length word
// that describes the whole packet, a general type/level (a chat line, never a modal
// box), and the text at +45 with one NUL after it - compared BYTE FOR BYTE.
//
// "Byte for byte" is the point of it. Two of these lines are the LIVE REFERENCE
// SERVER's own words, photographed by the owner (R76 slice 5), and a paraphrase that
// reads better is a failed test.
func wantMessageText(t *testing.T, msg []byte, wantText string) {
	t.Helper()
	if cmd := cmdOf(msg); cmd != CmdZCMessage {
		t.Fatalf("the packet is cmd %d, want %d (ZC_MESSAGE)", cmd, CmdZCMessage)
	}
	if length := int(binary.LittleEndian.Uint16(msg[2:])); length != len(msg) {
		t.Fatalf("length word = %d, the packet is %d bytes (it must be the WHOLE "+
			"packet)", length, len(msg))
	}
	if v := binary.LittleEndian.Uint32(msg[4:]); v != MessageTypeGeneral {
		t.Fatalf("type = %d, want %d (a general line, not a modal box)", v,
			MessageTypeGeneral)
	}
	if msg[8] != MessageLevelGeneral {
		t.Fatalf("level = %d, want %d (level>0 pops a modal box)", msg[8],
			MessageLevelGeneral)
	}
	if msg[len(msg)-1] != 0 {
		t.Fatalf("the message has no NUL terminator")
	}
	text := string(msg[45 : len(msg)-1])
	if text != wantText {
		t.Fatalf("the message reads %q, want %q", text, wantText)
	}
	// ASCII only, and short: the client copies the line into `char txt[256]`
	// (GEPacket.cpp, packet::ServerMsg) and draws it in the system font.
	if len(text) > 200 {
		t.Fatalf("the message is %d bytes; the client's own buffer is char[256]", len(text))
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			t.Fatalf("the message has a non-ASCII byte at %d: %q", i, text)
		}
	}
}

// wantSavedConfirmation is what a SUCCESSFUL SAVE answers with since R76 slice 5: the
// four 3634 lists AND exactly one ZC_MESSAGE whose text is the reference server's own
// "The warp location has been saved successfully", byte for byte.
//
// Both halves are asserted. The list is what redraws the window; the line is what tells
// the player the click landed, and the reference says it in words - so a server that
// only redrew the list would look different from the reference while every byte of the
// 3634 was correct.

// wantSavedConfirmationCounts is what a SUCCESSFUL SAVE answers with since R76 slice 5:
// the four 3634 lists (each with the row count given for it) AND exactly one ZC_MESSAGE
// whose text is the reference server's own "The warp location has been saved
// successfully", byte for byte.
//
// Both halves are asserted. The list is what redraws the window; the line is what tells
// the player the click landed, and the reference says it in words - so a server that
// only redrew the list would look different from the reference while every byte of the
// 3634 was correct.
func wantSavedConfirmationCounts(t *testing.T, out [][]byte, want [commWarpTypes]int) {
	t.Helper()
	if len(out) != commWarpTypes+1 {
		t.Fatalf("%d packet(s) after a save, want %d: the four 3634 lists plus ONE "+
			"ZC_MESSAGE (%q)", len(out), commWarpTypes+1, warpSavedText)
	}
	wantWarpCounts(t, out[:commWarpTypes], want)
	wantMessageText(t, out[commWarpTypes], warpSavedText)
	t.Logf("save confirmation: %v + %q", want, warpSavedText)
}

// wantSavedConfirmation is wantSavedConfirmationCounts for the common case: the rows are
// in the FIRST list and the other three are empty.
func wantSavedConfirmation(t *testing.T, out [][]byte, rows int) {
	t.Helper()
	wantSavedConfirmationCounts(t, out, [commWarpTypes]int{rows})
}

// TestWarpListSaveWritesOneRowAndRefreshes is the feature: a save at an empty list
// writes ONE row at the leader's own map and position, with the memo the player typed,
// and answers with the four 3634 lists - the only confirmation there is.
func TestWarpListSaveWritesOneRowAndRefreshes(t *testing.T) {
	h, f, logs := warpListHandler(t)
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Bank"))

	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) written, want exactly 1", len(f.saved))
	}
	got := f.saved[0]
	if got.Slot != 0 {
		t.Fatalf("slot = %d, want 0 (the first free slot)", got.Slot)
	}
	if got.MapID != 15 {
		t.Fatalf("map = %d, want 15 - the map the leader stands on", got.MapID)
	}
	if got.Pos != [3]int32{3175, 4203, 77} {
		t.Fatalf("position = %v, want the leader's own (3175, 4203, cell 77)", got.Pos)
	}
	if got.Name != "Bank" {
		t.Fatalf("memo/name = %q, want %q - it is the text the player typed", got.Name, "Bank")
	}

	wantSavedConfirmation(t, out, 1)
	// The memo of the saved row must be ON the wire, or the window draws the map name
	// instead of what the player typed.
	if memo := string(out[0][commWarpRecordsAt+commWarpMemo:][:4]); memo != "Bank" {
		t.Fatalf("memo on the wire = %q, want %q", memo, "Bank")
	}

	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"SAVED type0", "slot 0", "map 15", `memo "Bank"`,
		"type0 1 row(s)"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the save line does not name %q:\n%s", want, lines[0])
		}
	}
}

// TestWarpListSaveRefusedWhenFull is the refusal the owner asked for: at TEN rows (the
// size of the basic Warp List) the save writes NOTHING, the list still holds ten, and
// ONE log line names the reason and the memo it threw away. The player is no longer
// left guessing either: ONE ZC_MESSAGE names the cap - the number read from
// MaxSavedTeleports, never typed in twice.
func TestWarpListSaveRefusedWhenFull(t *testing.T) {
	full := make([]SavedPoint, 0, MaxSavedTeleports)
	for i := 0; i < MaxSavedTeleports; i++ {
		full = append(full, warpRow(i, 15, int32(100+i), int32(200+i), int32(i)+1, "old"))
	}
	h, f, logs := warpListHandler(t, full...)
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Sixth point"))

	if len(f.saved) != 0 {
		t.Fatalf("%d row(s) written at a full list, want 0", len(f.saved))
	}
	if len(f.rows) != MaxSavedTeleports {
		t.Fatalf("the list now holds %d row(s), want %d - nothing may be replaced",
			len(f.rows), MaxSavedTeleports)
	}
	for _, r := range f.rows {
		if r.Name != "old" {
			t.Fatalf("a stored row changed to %q; a refused save must write nothing", r.Name)
		}
	}
	// The player is told, once, and the line NAMES THE CAP the server enforces.
	wantOneWarpRefusalMessage(t, out, warpFullText())
	if !strings.Contains(warpFullText(), fmt.Sprintf("%d", MaxSavedTeleports)) {
		t.Fatalf("the full-list message does not name the cap %d: %q",
			MaxSavedTeleports, warpFullText())
	}
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"REFUSED", "already holds 10 row(s)", `"Sixth point"`,
		"DISCARDED"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, lines[0])
		}
	}
}

// TestWarpListSaveRefusedForCashType: this server has ONE list. A save into the extra
// lists (teltype 1-3) would land in the player's own list and draw under the wrong
// heading, so it is refused the same way - nothing written, one log line, and ONE
// message.
//
// WHAT THE MESSAGE MUST SAY (R76 slice 5). The client's own COMMWARP_SAVE walks
// telType 0..3 and takes the first list under ten (ge\ui\uiscp\game.scp:247-253), so
// teltype 1-3 only ever arrives when the player's OWN list already holds ten. The text
// therefore tells that story: your own list is full, the extra lists need a "Warp
// Extend" item this server does not sell. It must NOT say the list is merely
// "unavailable" any more, because that sentence left a player who had just filled ten
// points with no idea what the next save was asking for.

// TestWarpListSaveIntoAnExtendPanelLandsInThatList is DoD 1 of R76 slice 6: the save
// carries the list the client chose (teltype at +10) and the point lands THERE.
//
// THE RULING BEHIND IT, the owner's own words: "nothing was paid lol. just serve all 40.
// it was supposed to be free to add to the entry. u only need those cash item to ENABLED
// that slot extension." The client's own COMMWARP_SAVE walks telType 0..3 and takes the
// first panel under ten rows, so by the time it sends "2" the basic list already holds
// ten; there is nothing to buy.
//
// The other half of the test is that type 0 is UNCHANGED, byte for byte: a save into a
// panel must not draw its row in the basic list.
func TestWarpListSaveIntoAnExtendPanelLandsInThatList(t *testing.T) {
	h, f, logs := warpListHandler(t, warpRow(0, 15, 100, 200, 5, "Bank"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(2, "Extend point"))

	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) written, want exactly 1", len(f.saved))
	}
	got := f.saved[0]
	if got.TelType != 2 {
		t.Fatalf("the row was written into list %d, want 2 - the list the packet named",
			got.TelType)
	}
	// The slot and the display position are the FIRST of THAT list, which is empty.
	if got.Slot != 0 || got.Order != 0 {
		t.Fatalf("type2 slot %d order %d, want slot 0 order 0 (its own list is empty)",
			got.Slot, got.Order)
	}
	if got.Name != "Extend point" || got.Pos != [3]int32{3175, 4203, 77} {
		t.Fatalf("written row = %+v, want the memo the player typed at the leader's spot",
			got)
	}

	wantSavedConfirmationCounts(t, out, [commWarpTypes]int{1, 0, 1, 0})
	// TYPE 0 IS UNCHANGED: still one row, still "Bank".
	if memo := string(out[0][commWarpRecordsAt+commWarpMemo:][:4]); memo != "Bank" {
		t.Fatalf("type 0 row 0 memo = %q, want %q - a save into type 2 must not touch it",
			memo, "Bank")
	}
	// TYPE 2 CARRIES THE NEW ROW.
	if memo := string(out[2][commWarpRecordsAt+commWarpMemo:][:12]); memo != "Extend point" {
		t.Fatalf("type 2 row 0 memo = %q, want %q", memo, "Extend point")
	}

	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"SAVED type2", "Warp Extend(2)", "slot 0",
		`memo "Extend point"`, "type0 1 row(s)", "type2 1 row(s)"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the save line does not name %q:\n%s", want, lines[0])
		}
	}
}

// TestWarpListCapIsTenPerList is DoD 1's second half: each list caps at ten
// INDEPENDENTLY. A full basic list refuses its eleventh and still accepts a save into
// "Warp Extend(1)" - which is what "serve all 40" means - and a full extra list refuses
// its own eleventh, with a message that NAMES it.
func TestWarpListCapIsTenPerList(t *testing.T) {
	full := make([]SavedPoint, 0, MaxSavedTeleports)
	for i := 0; i < MaxSavedTeleports; i++ {
		// Distinct cells: a repeated map+x+y+cell would be a DUPLICATE, a different
		// refusal, and this test is about the cap.
		full = append(full, warpRow(i, 15, int32(100+i), int32(200+i), int32(i)+1, "old"))
	}
	h, f, logs := warpListHandler(t, full...)
	sess := teleportSession([3]int32{3175, 4203, 77})

	// (1) The basic list is full: its eleventh is refused, naming the basic list.
	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(0, "eleventh"))
	wantOneWarpRefusalMessage(t, out, warpFullTextFor(0))
	if len(f.saved) != 0 {
		t.Fatalf("%d row(s) written into a full list, want 0", len(f.saved))
	}
	if !strings.Contains(logs.String(), "Warp List") {
		t.Fatalf("the refusal does not name the basic list:\n%s", logs.String())
	}

	// (2) ... and "Warp Extend(1)" still takes the save.
	out = warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(1, "extend one"))
	wantSavedConfirmationCounts(t, out, [commWarpTypes]int{MaxSavedTeleports, 1, 0, 0})
	if len(f.saved) != 1 || f.saved[0].TelType != 1 {
		t.Fatalf("written rows = %+v, want one row in list 1", f.saved)
	}

	// (3) A FULL extra list refuses its own eleventh, and the message NAMES it: with
	// four lists, "your Warp List is full" would be ambiguous.
	extendFull := make([]SavedPoint, 0, MaxSavedTeleports)
	for i := 0; i < MaxSavedTeleports; i++ {
		extendFull = append(extendFull, warpRowIn(1, i, 15, int32(900+i), int32(800+i),
			int32(i)+1, "old extend"))
	}
	h2, f2, logs2 := warpListHandler(t, extendFull...)
	out = warpReply(t, h2, sess, CmdCZCommWarpSave, warpSavePacket(1, "twelfth"))
	wantOneWarpRefusalMessage(t, out, warpFullTextFor(1))
	if !strings.Contains(warpFullTextFor(1), "Warp Extend(1)") {
		t.Fatalf("the full-list message for type 1 does not name the list: %q",
			warpFullTextFor(1))
	}
	if len(f2.saved) != 0 {
		t.Fatalf("%d row(s) written into a full extra list, want 0", len(f2.saved))
	}
	for _, want := range []string{"Warp Extend(1)", "other lists are unaffected"} {
		if !strings.Contains(logs2.String(), want) {
			t.Fatalf("the type-1 refusal does not say %q:\n%s", want, logs2.String())
		}
	}
}

// TestWarpListDuplicateRefusedAcrossLists is DoD 1's duplicate half: the same
// map + x + y + cell is ONE place, whichever of the forty points holds it. A point
// already saved in "Warp Extend(2)" refuses the same spot saved into the basic list, with
// the reference server's own line - which names no panel, because it does not care.
func TestWarpListDuplicateRefusedAcrossLists(t *testing.T) {
	h, f, logs := warpListHandler(t, warpRowIn(2, 0, 15, 3175, 4203, 77, "already in Extend 2"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(0, "same spot, basic"))

	wantOneWarpRefusalMessage(t, out, warpDuplicateText)
	if len(f.saved) != 0 {
		t.Fatalf("%d row(s) written for a duplicate spot, want 0", len(f.saved))
	}
	if len(f.rows) != 1 {
		t.Fatalf("the store now holds %d row(s), want the one it started with", len(f.rows))
	}
	for _, p := range out {
		if cmdOf(p) == CmdZCCommWarpList {
			t.Fatalf("a 3634 list was re-sent on a refusal")
		}
	}
	if !strings.Contains(logs.String(), "ANYWHERE") {
		t.Fatalf("the refusal does not say the duplicate is across all the lists:\n%s",
			logs.String())
	}
}

// TestWarpListSaveRefusesADuplicateSpot is DoD (b) of R76 slice 5: the SAME
// map + x + y + cell saved TWICE under TWO DIFFERENT NAMES is one point, and the second
// save is refused - nothing written (one row, not two), ONE log line, ONE ZC_MESSAGE,
// and NO 3634 list, because nothing changed and the list the client holds is still true.
//
// The two names are the whole point of the test. The owner asked for this rule in his
// own words: "im talking about coordinates and not names. the names does not matter if
// duplicate." A check that compared the memo would pass this test's first save and then
// happily write the second row under "Same spot, other name".
func TestWarpListSaveRefusesADuplicateSpot(t *testing.T) {
	// One point already saved in Coimbra at (3175, 4203, cell 77), called "Bank".
	h, f, logs := warpListHandler(t, warpRow(0, 15, 3175, 4203, 77, "Bank"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave,
		warpSavePacket(commWarpOwnList, "Same spot, other name"))

	// ONE ROW, still under its first name: the refused save wrote nothing.
	if len(f.rows) != 1 {
		t.Fatalf("%d row(s) in the list, want 1 - the duplicate must not be written",
			len(f.rows))
	}
	if f.rows[0].Name != "Bank" {
		t.Fatalf("the stored row is now called %q, want \"Bank\" (nothing may change)",
			f.rows[0].Name)
	}
	if len(f.saved) != 0 {
		t.Fatalf("%d row(s) reached the store as written, want 0", len(f.saved))
	}
	// The store WAS asked - the refusal is the store's, not a handler shortcut - so a
	// store that forgot the rule fails this test rather than passing it by accident.
	if f.saves != 1 {
		t.Fatalf("the store was asked %d time(s), want 1 (the handler must not decide "+
			"this before the store's locked read)", f.saves)
	}
	// ONE message, and no 3634: a re-sent list would claim something happened.
	wantOneWarpRefusalMessage(t, out, warpDuplicateText)
	for _, p := range out {
		if cmdOf(p) == CmdZCCommWarpList {
			t.Fatalf("a 3634 list was re-sent on the duplicate refusal")
		}
	}
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"REFUSED", "already saved at map 15", "3175",
		"4203", "cell 77", `"Same spot, other name"`, "DISCARDED"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, lines[0])
		}
	}
	t.Logf("duplicate-refusal line: %s / message: %s", lines[0], warpDuplicateText)
}

// TestSameSpotIsCoordinatesNeverTheName pins the duplicate rule itself, with no store
// and no client: map + x + y + cell decide, the name does not, and a different map is a
// different place. This is the test to break when the rule is questioned - sameSpot is
// what both the PostgreSQL store and the in-memory test store call.
func TestSameSpotIsCoordinatesNeverTheName(t *testing.T) {
	base := warpRow(0, 15, 3175, 4203, 77, "Bank")

	type check struct {
		name  string
		other SavedPoint
		want  bool
		// why is the sentence a reader needs when this row fails.
		why string
	}
	cases := []check{
		{
			name:  "the same map and the same x, y, cell under ANOTHER NAME",
			other: warpRow(9, 15, 3175, 4203, 77, "Same spot, other name"),
			want:  true,
			why:   "the name is irrelevant; this is the owner's own case",
		},
		{
			name:  "the same spot with an EMPTY name",
			other: warpRow(9, 15, 3175, 4203, 77, ""),
			want:  true,
			why:   "an unnamed point on the same spot is still the same point",
		},
		{
			name:  "the same x, y and cell on ANOTHER MAP",
			other: warpRow(9, 10, 3175, 4203, 77, "Bank"),
			want:  false,
			why:   "a different map is a different place, and a player may hold both",
		},
		{
			name:  "one step away in x",
			other: warpRow(9, 15, 3176, 4203, 77, "Bank"),
			want:  false,
			why:   "x is part of the spot",
		},
		{
			name:  "one step away in y",
			other: warpRow(9, 15, 3175, 4204, 77, "Bank"),
			want:  false,
			why:   "y is part of the spot",
		},
		{
			name:  "a different navmesh cell at the same x and y",
			other: warpRow(9, 15, 3175, 4203, 78, "Bank"),
			want:  false,
			why:   "the cell is part of the spot: travel takes the cell, not x/y",
		},
	}
	for _, tc := range cases {
		if got := sameSpot(base, tc.other); got != tc.want {
			t.Fatalf("%s: sameSpot = %v, want %v - %s", tc.name, got, tc.want, tc.why)
		}
	}
	// The slot is NOT part of the rule: a duplicate is a duplicate whatever row it would
	// have taken, which is what lets the store answer before it picks a slot.
	if !sameSpot(warpRow(0, 15, 1, 2, 3, "a"), warpRow(7, 15, 1, 2, 3, "b")) {
		t.Fatalf("sameSpot says two rows on one spot with different slots are different " +
			"places; the slot is a storage key, not part of a point")
	}
}

// TestWarpListSaveAllowsTheSameSpotOnAnotherMap is DoD (c): x/y/cell equal to a stored
// point, on a DIFFERENT map, is a DIFFERENT place and must be saved. This is why the
// rule is a check and not a unique index - the coordinates are not a key.
func TestWarpListSaveAllowsTheSameSpotOnAnotherMap(t *testing.T) {
	// A point on map 10 (reb_basic) at the very coordinates the zone will offer, while
	// the zone is serving map 15 (coim_basic).
	h, f, logs := warpListHandler(t, warpRow(0, 10, 3175, 4203, 77, "Reboldoeux bank"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Coimbra"))

	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) written, want 1 - the same x/y/cell on another map is "+
			"allowed", len(f.saved))
	}
	if len(f.rows) != 2 {
		t.Fatalf("the list holds %d row(s), want 2", len(f.rows))
	}
	if got := f.rows[1]; got.MapID != 15 || got.Pos != [3]int32{3175, 4203, 77} {
		t.Fatalf("the new row is map %d %v, want map 15 (3175, 4203, cell 77)",
			got.MapID, got.Pos)
	}
	// A SUCCESS is the four 3634 lists plus the reference's one success line.
	wantSavedConfirmation(t, out, 2)
	if strings.Contains(logs.String(), "REFUSED") {
		t.Fatalf("a legal save logged a refusal:\n%s", logs.String())
	}
	// ONE line, and it names the row it wrote - the record the docs point at.
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s) for one save, want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"SAVED type0", "slot 1 map 15", "3634 re-sent 4"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the save line does not name %q:\n%s", want, lines[0])
		}
	}
}

// TestWarpListSaveReadsTheLeaderPosition is the "not a zero/default" check: the row
// that lands in the store carries where the LEADER stands RIGHT NOW, and it follows
// the character - the second save stores the second position, not the first.
func TestWarpListSaveReadsTheLeaderPosition(t *testing.T) {
	h, f, _ := warpListHandler(t)
	sess := teleportSession([3]int32{3175, 4203, 77})

	warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "first"))

	// The character walks: this is the live session state, and the save must read it.
	sess.Pos = [3]int32{-1088, 9339, 128}
	warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "second"))

	if len(f.saved) != 2 {
		t.Fatalf("%d row(s) written, want 2", len(f.saved))
	}
	if first := f.saved[0]; first.Pos != [3]int32{3175, 4203, 77} {
		t.Fatalf("the first row holds %v, want (3175, 4203, cell 77)", first.Pos)
	}
	if second := f.saved[1]; second.Pos != [3]int32{-1088, 9339, 128} {
		t.Fatalf("the second row holds %v, want the leader's NEW position "+
			"(-1088, 9339, cell 128)", second.Pos)
	}
	// A session that was never placed has no position at all, and that is refused
	// rather than stored as the world origin - and the player is told why.
	h2, f2, logs := warpListHandler(t)
	bare := &Session{CommanderID: 25}
	out := warpReply(t, h2, bare, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "nowhere"))
	if len(f2.saved) != 0 {
		t.Fatalf("a save with no position wrote %d row(s), want none", len(f2.saved))
	}
	wantOneWarpRefusalMessage(t, out, warpNoPositionText)
	if !strings.Contains(logs.String(), "no position yet") {
		t.Fatalf("the refusal does not say why:\n%s", logs.String())
	}
}

// TestWarpListMemoCappedAtThirtyChars: the wire field is 32 bytes but the client's own
// limit is 30 characters, so a longer memo is cut and the cut is LOGGED once instead of
// silently renaming the player's point.
func TestWarpListMemoCappedAtThirtyChars(t *testing.T) {
	h, f, logs := warpListHandler(t)
	sess := teleportSession([3]int32{3175, 4203, 77})
	// 40 characters, which the client's own InputString limit cannot even send.
	long := strings.Repeat("A", 40)

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, long))

	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) written, want 1", len(f.saved))
	}
	if want := strings.Repeat("A", commWarpMemoChars); f.saved[0].Name != want {
		t.Fatalf("stored memo = %q (%d chars), want %d characters",
			f.saved[0].Name, len(f.saved[0].Name), commWarpMemoChars)
	}
	if strings.Count(logs.String(), "character(s) and this client accepts") != 1 {
		t.Fatalf("the cut was not logged exactly once:\n%s", logs.String())
	}
	wantSavedConfirmation(t, out, 1)
}

// TestWarpListRemoveByListIndex is the index rule: the client names a ROW, and a row
// is a position in the list it was last sent - NOT a database slot. Slot 2 is gone
// here, so row 1 IS slot 3, and a handler that deleted row 1 as slot 1 would delete
// the wrong point.
func TestWarpListRemoveByListIndex(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(1, 15, 4, 5, 6, "b"),
		warpRow(3, 15, 7, 8, 9, "c"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRemove, warpRowPacket(CmdCZCommWarpRemove, 2, 0, ""))

	if len(f.removes) != 1 || f.removes[0] != 3 {
		t.Fatalf("removes = %v, want [3]: row 2 is slot 3, because slot 2 is a gap",
			f.removes)
	}
	// The other slots are NOT renumbered: 0 and 1 stay where they were.
	if len(f.rows) != 2 || f.rows[0].Slot != 0 || f.rows[1].Slot != 1 {
		t.Fatalf("rows after the remove = %v, want slots 0 and 1 untouched", f.rows)
	}
	wantFourWarpLists(t, out, 2)
	for _, want := range []string{"CZ_COMMWARP_REMOVE (type0 row 2)", "slot 3", "deleted"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the remove line does not name %q:\n%s", want, logs.String())
		}
	}
}

// TestWarpListRemoveOutOfRangeRefuses: an index naming no row refuses with one log
// line and one message, and changes nothing - the store is never even asked.
func TestWarpListRemoveOutOfRangeRefuses(t *testing.T) {
	h, f, logs := warpListHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRemove, warpRowPacket(CmdCZCommWarpRemove, 5, 0, ""))

	wantOneWarpRefusalMessage(t, out, warpNoSuchRowText)
	if len(f.removes) != 0 {
		t.Fatalf("the store was asked to remove %v; an out-of-range row must not reach it",
			f.removes)
	}
	if len(f.rows) != 1 {
		t.Fatalf("the list now holds %d row(s), want 1", len(f.rows))
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
}

// TestWarpListRenameByListIndex is 3638: the row is resolved the same way a remove
// resolves it, the memo changes, and the position, map and slot do NOT.
func TestWarpListRenameByListIndex(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(4, 15, 7, 8, 9, "c"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRename,
		warpRowPacket(CmdCZCommWarpRename, 1, 0, "Bank"))

	if len(f.renames) != 1 || f.renames[0] != "Bank" {
		t.Fatalf("renames = %v, want [Bank]", f.renames)
	}
	if got := f.rows[1]; got.Slot != 4 || got.Name != "Bank" ||
		got.Pos != [3]int32{7, 8, 9} || got.MapID != 15 {
		t.Fatalf("the renamed row = %+v, want slot 4 map 15 at (7, 8, cell 9) named Bank",
			got)
	}
	if f.rows[0].Name != "a" {
		t.Fatalf("row 0 changed to %q; a rename touches ONE row", f.rows[0].Name)
	}
	wantFourWarpLists(t, out, 2)
	// The new memo is on the wire, or the window would draw the old one.
	if memo := string(out[0][commWarpRecordsAt+commWarpRecord+commWarpMemo:][:4]); memo != "Bank" {
		t.Fatalf("memo on the wire = %q, want %q", memo, "Bank")
	}
	for _, want := range []string{"CZ_COMMWARP_RENAME (type0 row 1)", "slot 4", `"Bank"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the rename line does not name %q:\n%s", want, logs.String())
		}
	}
}

// TestWarpListRenameOutOfRangeRefuses mirrors the remove: no row, no write, one log
// line and one message.
func TestWarpListRenameOutOfRangeRefuses(t *testing.T) {
	h, f, logs := warpListHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRename,
		warpRowPacket(CmdCZCommWarpRename, 9, 0, "Bank"))

	wantOneWarpRefusalMessage(t, out, warpNoSuchRowText)
	if len(f.renames) != 0 {
		t.Fatalf("the store was asked to rename %v, want none", f.renames)
	}
	if f.rows[0].Name != "a" {
		t.Fatalf("row 0 is now %q; a refused rename writes nothing", f.rows[0].Name)
	}
	if !strings.Contains(logs.String(), "REFUSED") || !strings.Contains(logs.String(), "named row 9") {
		t.Fatalf("the refusal does not name the row:\n%s", logs.String())
	}
}

// TestWarpListWritesRefusedForCashTypes: remove and rename name a teltype too, and a
// wrong one is refused exactly like a save is - one log line and ONE message saying the
// list is not available here.

// TestWarpListRemoveActsInsideTheNamedList: 3636 names a list AND a row, and the row is
// a position INSIDE that list - not inside some flat forty-row collection. Each panel
// numbers its own rows from 0, so row 0 of "Warp Extend(2)" is that panel's first point.
func TestWarpListRemoveActsInsideTheNamedList(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 100, 200, 5, "basic first"),
		warpRow(1, 15, 300, 400, 7, "basic second"),
		warpRowIn(2, 0, 15, 500, 600, 9, "extend first"),
		warpRowIn(2, 1, 15, 700, 800, 11, "extend second"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRemove,
		warpRowPacket(CmdCZCommWarpRemove, 0, 2, ""))

	wantWarpCounts(t, out, [commWarpTypes]int{2, 0, 1, 0})
	if len(f.rows) != 3 {
		t.Fatalf("the store holds %d row(s), want 3 - exactly one row may be deleted",
			len(f.rows))
	}
	// The basic list still has BOTH of its rows, in order.
	for i, want := range []string{"basic first", "basic second"} {
		at := commWarpRecordsAt + i*commWarpRecord + commWarpMemo
		if memo := string(out[0][at:][:len(want)]); memo != want {
			t.Fatalf("type 0 row %d memo = %q, want %q - it must not be touched", i, memo,
				want)
		}
	}
	// Type 2 kept its SECOND row, which is now its first.
	if memo := string(out[2][commWarpRecordsAt+commWarpMemo:][:13]); memo != "extend second" {
		t.Fatalf("type 2 row 0 memo = %q, want %q", memo, "extend second")
	}
	if !strings.Contains(logs.String(), "type2 row 0") {
		t.Fatalf("the log does not name the list and the row:\n%s", logs.String())
	}
}

// TestWarpListRenameActsInsideTheNamedList: 3638 names a list and a row too, and the
// rename lands on THAT row of THAT list. The basic list keeps its own name.
func TestWarpListRenameActsInsideTheNamedList(t *testing.T) {
	h, f, _ := warpListHandler(t,
		warpRow(0, 15, 100, 200, 5, "basic first"),
		warpRow(1, 15, 300, 400, 7, "basic second"),
		warpRowIn(2, 0, 15, 500, 600, 9, "extend first"),
		warpRowIn(2, 1, 15, 700, 800, 11, "extend second"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpRename,
		warpRowPacket(CmdCZCommWarpRename, 1, 2, "renamed"))

	wantWarpCounts(t, out, [commWarpTypes]int{2, 0, 2, 0})
	names := func(telType int) []string {
		var got []string
		for _, r := range f.rows {
			if r.TelType == telType {
				got = append(got, r.Name)
			}
		}
		return got
	}
	if got := strings.Join(names(0), ","); got != "basic first,basic second" {
		t.Fatalf("type 0 names = %s, want basic first,basic second - untouched", got)
	}
	if got := strings.Join(names(2), ","); got != "extend first,renamed" {
		t.Fatalf("type 2 names = %s, want extend first,renamed", got)
	}
	if len(f.renames) != 1 || f.renames[0] != "renamed" {
		t.Fatalf("the store was asked to rename %v, want one %q", f.renames, "renamed")
	}
	// The renamed row is the SECOND of type 2, and it is what went back on the wire.
	if memo := string(out[2][commWarpRecordsAt+commWarpRecord+commWarpMemo:][:7]); memo != "renamed" {
		t.Fatalf("type 2 row 1 memo on the wire = %q, want %q", memo, "renamed")
	}
}

// THE MEMO IS A C STRING.
//
// The client writes its `char[31]` memo as a C string: the text the player typed, a
// NUL terminator, and then whatever was behind it on its own stack. Trimming NULs from
// the END (the first version of this file) kept the junk AFTER an interior NUL, and
// PostgreSQL refused the row every time:
//
//	invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//
// That is logs\zone15_live.log, 2026-10-30 14:54:45, commander 24, memo "lala" - every
// save the owner tried, refused, with nothing written and nothing sent. The tests below
// rebuild that exact shape and pin the rule: CUT AT THE FIRST NUL, and refuse (one line,
// nothing written, nothing sent) a memo that is not UTF-8 text.

// liveMemoJunk is the tail the live client left behind "lala" (zone15_live.log lines
// 19135-19142): low stack bytes, including NULs of their own. It is not text and it must
// never reach a row.
var liveMemoJunk = []byte{
	0xd4, 0xff, 0x19, 0x00, 0x21, 0x12, 0x58, 0x00, 0x8a, 0x6a, 0x47, 0x23,
	0x02, 0x00, 0x00, 0x00, 0x8a, 0x6a, 0x47, 0x23, 0x00, 0x00, 0x00, 0x00, 0x8a,
}

// warpCString writes text into the client's 31-byte field the way the client's own
// `strncpy_s(dst, 0x1f, ...)` does: the text, a NUL terminator, then junk. Text that
// fills the field gets no terminator at all.
func warpCString(text string, junk []byte) []byte {
	field := make([]byte, commWarpMemoField)
	if n := copy(field, text); n < len(field) {
		copy(field[n+1:], junk)
	}
	return field
}

// warpSaveCString is one 3635 in the client's own shape: cmd(2) + the 8-byte security
// header, teltype at +10, then the 31-byte C-string memo at +11.
func warpSaveCString(telType byte, memo []byte) []byte {
	return pack(CmdCZCommWarpSave, append([]byte{telType}, memo...)...)
}

// warpRenameCString is one 3638: the row at +10, the teltype at +11, then the 31-byte
// C-string memo at +12.
func warpRenameCString(row, telType byte, memo []byte) []byte {
	return pack(CmdCZCommWarpRename, append([]byte{row, telType}, memo...)...)
}

// pgStringStore is warpStore plus PostgreSQL's own text rule, so a test can see the
// failure the live zone saw instead of a fake that accepts anything. PG refuses a text
// value holding a 0x00 byte, and refuses bytes that are not UTF-8, with SQLSTATE 22021.
type pgStringStore struct {
	*warpStore
	refused []string
}

func (s *pgStringStore) save(ctx context.Context, commander int32, telType, slot int, p SavedPoint) (SavedPoint, error) {
	if !utf8.ValidString(p.Name) {
		s.refused = append(s.refused, p.Name)
		return SavedPoint{}, fmt.Errorf("write list %d slot %d of commander %d: ERROR: "+
			`invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)`, telType,
			slot, commander)
	}
	return s.warpStore.save(ctx, commander, telType, slot, p)
}

func (s *pgStringStore) rename(ctx context.Context, commander int32, telType, slot int, name string) (SavedPoint, bool, error) {
	if !utf8.ValidString(name) {
		s.refused = append(s.refused, name)
		return SavedPoint{}, false, fmt.Errorf("rename list %d slot %d of commander %d: "+
			`ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)`,
			telType, slot, commander)
	}
	return s.warpStore.rename(ctx, commander, telType, slot, name)
}

// pgStringHandler is warpListHandler over pgStringStore: Coimbra, the rename hook, a log
// the test can read, and a store that refuses what PostgreSQL refuses.
func pgStringHandler(t *testing.T, rows ...SavedPoint) (*Handler, *pgStringStore, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), MapID: 15, MapName: "coim_basic"}
	s := &pgStringStore{warpStore: &warpStore{savedTeleportFake: &savedTeleportFake{rows: rows}}}
	h.UseSavedTeleports(s.load, s.save, s.remove)
	h.UseSavedPointRename(s.rename)
	return h, s, logs
}

// TestCommWarpSaveCutsTheMemoAtItsFirstNul is the outage rebuilt from the live bytes:
// "lala" + NUL + the client's stack junk must parse as EXACTLY "lala" and must write ONE
// row named "lala". On the pre-fix parser the memo was "lala\x00<junk>", PostgreSQL
// refused it, and the owner's save did nothing three times in a row.
func TestCommWarpSaveCutsTheMemoAtItsFirstNul(t *testing.T) {
	field := warpCString("lala", liveMemoJunk)
	pt := warpSaveCString(commWarpOwnList, field)

	// The offsets are pinned to the bytes, not just to the constants: the teltype is the
	// byte at +10 and the 31-byte memo field starts at +11.
	if len(pt) != commWarpSaveSize {
		t.Fatalf("the packet is %d bytes, want %d (the client's own save size)",
			len(pt), commWarpSaveSize)
	}
	if pt[commWarpSaveTelType] != commWarpOwnList || !bytes.Equal(pt[commWarpSaveMemo:], field) {
		t.Fatalf("the packet does not carry the teltype at +%d and the memo at +%d",
			commWarpSaveTelType, commWarpSaveMemo)
	}

	telType, memo, ok := ParseCommWarpSave(pt)
	if !ok || telType != commWarpOwnList {
		t.Fatalf("parsed teltype %d ok=%v, want 0 and ok", telType, ok)
	}
	if memo != "lala" {
		t.Errorf("the parsed memo is %q, want exactly \"lala\": everything from the first "+
			"NUL onward is not part of the memo", memo)
	}

	h, f, logs := pgStringHandler(t)
	sess := teleportSession([3]int32{3175, 4203, 77})
	out := warpReply(t, h, sess, CmdCZCommWarpSave, pt)

	if len(f.refused) != 0 {
		t.Errorf("the store refused the write with %q; a memo cut at its first NUL is plain "+
			"UTF-8 text and must never reach the store as a broken string", f.refused)
	}
	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) written, want exactly 1 named \"lala\" (refusals: %q, log:\n%s)",
			len(f.saved), f.refused, logs.String())
	}
	if got := f.saved[0].Name; got != "lala" {
		t.Errorf("the row is named %q, want %q", got, "lala")
	}
	wantSavedConfirmation(t, out, 1)
	// Four characters is not "too long": the old line counted the 31-byte FIELD, which is
	// what made a 4-character memo read as "the memo was 31 character(s)".
	if strings.Contains(logs.String(), "character(s) and this client accepts") {
		t.Errorf("a four-character memo was reported as too long - the count must be the cut "+
			"memo, not the field width:\n%s", logs.String())
	}
	if lines := logLines(logs); len(lines) != 1 {
		t.Errorf("%d log line(s), want 1 (the SAVED line):\n%s", len(lines), logs.String())
	}
}

// TestCommWarpRenameCutsTheMemoAtItsFirstNul is the same rule at the rename's offsets:
// row @+10, teltype @+11, memo @+12.
func TestCommWarpRenameCutsTheMemoAtItsFirstNul(t *testing.T) {
	field := warpCString("Coimbra", liveMemoJunk)
	pt := warpRenameCString(1, commWarpOwnList, field)

	if len(pt) != commWarpRenameSize {
		t.Fatalf("the packet is %d bytes, want %d (the client's own rename size)",
			len(pt), commWarpRenameSize)
	}
	if !bytes.Equal(pt[commWarpRenameMemo:], field) {
		t.Fatalf("the rename does not carry the memo at +%d", commWarpRenameMemo)
	}

	index, telType, memo, ok := ParseCommWarpRename(pt)
	if !ok || index != 1 || telType != commWarpOwnList {
		t.Fatalf("parsed row %d teltype %d ok=%v, want 1, 0, ok", index, telType, ok)
	}
	if memo != "Coimbra" {
		t.Errorf("the parsed memo is %q, want exactly %q: everything from the first NUL "+
			"onward is not part of the memo", memo, "Coimbra")
	}

	h, f, logs := pgStringHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(1, 15, 4, 5, 6, "b"))
	sess := teleportSession([3]int32{3175, 4203, 77})
	out := warpReply(t, h, sess, CmdCZCommWarpRename, pt)

	if len(f.refused) != 0 {
		t.Errorf("the store refused the rename with %q; a cut memo is plain UTF-8 text",
			f.refused)
	}
	if len(f.renames) != 1 || f.renames[0] != "Coimbra" {
		t.Fatalf("renames = %q, want [Coimbra] (refusals: %q, log:\n%s)",
			f.renames, f.refused, logs.String())
	}
	if f.rows[1].Name != "Coimbra" || f.rows[0].Name != "a" {
		t.Fatalf("rows after the rename are %q and %q, want \"a\" and \"Coimbra\"",
			f.rows[0].Name, f.rows[1].Name)
	}
	wantFourWarpLists(t, out, 2)
}

// R76 SLICE 2: THE LIST BEHAVES LIKE A LIST.
//
// Two defects came from the owner's hands in the real client on 2026-10-30, and both are
// about the same thing: which row the window draws where.
//
//	(a) A new point must go to the BOTTOM. He deleted row 3 of five and saved, and the
//	    new point appeared in FOURTH place. His words: "deleting and inserting new entry
//	    replace that entry number? normally it would just insert the new entry to the
//	    bottom of the list."
//	(b) Up and Down DO send 3639. An earlier note said the client had no up/down control;
//	    his clicks produced six of them, and the zone logged them as unknown.

// warpSwapPacket is one CZ_COMMWARP_SWAP (3639) in the exact shape the live client sent:
// a 10-byte security header, the row that moves as a u32 at +10, the row it moves to as a
// u32 at +14, and the teltype at +18 - 19 bytes in all.
func warpSwapPacket(from, to int, telType byte) []byte {
	body := make([]byte, commWarpSwapSize-commWarpReqHeader)
	binary.LittleEndian.PutUint32(body[commWarpSwapFrom-commWarpReqHeader:], uint32(from))
	binary.LittleEndian.PutUint32(body[commWarpSwapTo-commWarpReqHeader:], uint32(to))
	body[commWarpSwapTelType-commWarpReqHeader] = telType
	return pack(CmdCZCommWarpSwap, body...)
}

// warpMemos reads the row memos out of the type-0 list of a four-packet 3634 send, which
// is the order the window draws. The memo is the only field that names a row, so this is
// how "the list the player sees" is asserted in bytes.
func warpMemos(t *testing.T, out [][]byte) []string {
	t.Helper()
	if len(out) == 0 {
		t.Fatalf("no packets sent, so there is no list to read")
	}
	p := out[0]
	count := int(p[5])
	if want := commWarpRecordsAt + commWarpRecord*count; len(p) != want {
		t.Fatalf("type 0 = %d bytes, want %d for %d row(s)", len(p), want, count)
	}
	memos := make([]string, 0, count)
	for i := 0; i < count; i++ {
		field := p[commWarpRecordsAt+i*commWarpRecord+commWarpMemo:][:commWarpMemoSize]
		if n := bytes.IndexByte(field, 0); n >= 0 {
			field = field[:n]
		}
		memos = append(memos, string(field))
	}
	return memos
}

// storeMemos is the same order read back from the STORE, which is what proves a move
// survives: the next refresh draws the store, not the answer to this request.
func storeMemos(t *testing.T, f *warpStore) []string {
	t.Helper()
	rows, err := f.load(context.Background(), 25)
	if err != nil {
		t.Fatalf("read the list back from the store: %v", err)
	}
	memos := make([]string, 0, len(rows))
	for _, r := range rows {
		memos = append(memos, r.Name)
	}
	return memos
}

// TestWarpListSaveAfterMiddleDeleteGoesToTheBottom is defect (a), rebuilt from the live
// log (logs\zone15_live.log 15:09:57 / 15:10:00 / 15:10:07): five rows, row 3 deleted,
// then a save. The saved point must be the LAST row, and the rows that were above the
// hole must keep their order.
//
// It also pins that the storage key is still allowed to reuse the hole: the new row lands
// in slot 3, which is what "the slot is internal" means. The order on screen is the
// `display_order` column, never the slot.
func TestWarpListSaveAfterMiddleDeleteGoesToTheBottom(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "111"),
		warpRow(1, 15, 4, 5, 6, "222"),
		warpRow(2, 15, 7, 8, 9, "333"),
		warpRow(3, 15, 10, 11, 12, "444"),
		warpRow(4, 15, 13, 14, 15, "sdfsdfs"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	// The delete he did: row 3 of five is slot 3.
	warpReply(t, h, sess, CmdCZCommWarpRemove, warpRowPacket(CmdCZCommWarpRemove, 3, 0, ""))
	// Then the save he did, seven seconds later.
	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "56565"))

	if len(f.saved) != 1 {
		t.Fatalf("%d row(s) saved, want 1:\n%s", len(f.saved), logs.String())
	}
	// The hole is refilled as a storage key - and that is allowed, because nothing the
	// player sees is driven by it any more.
	if got := f.saved[0].Slot; got != 3 {
		t.Fatalf("the save took slot %d, want the free slot 3 - the slot is the internal "+
			"key, so reusing the hole is fine", got)
	}

	want := []string{"111", "222", "333", "sdfsdfs", "56565"}
	got := warpMemos(t, out)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the window draws %v, want %v - a new point goes to the BOTTOM, and the "+
			"rows above the hole keep their order", got, want)
	}
	if back := storeMemos(t, f); strings.Join(back, ",") != strings.Join(want, ",") {
		t.Fatalf("the store re-reads as %v, want %v - the order must be stored, not just "+
			"drawn once", back, want)
	}
}

// TestWarpListSwapMovesARowUp is the Up button: on a five-row list it is row 4 -> 3, the
// exact packet the owner's click produced (`04 00 00 00` then `03 00 00 00`). The two
// neighbours change places and nothing else moves, and the move is still there when the
// list is read back from the store.
func TestWarpListSwapMovesARowUp(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(1, 15, 4, 5, 6, "b"),
		warpRow(2, 15, 7, 8, 9, "c"),
		warpRow(3, 15, 10, 11, 12, "d"),
		warpRow(4, 15, 13, 14, 15, "e"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSwap, warpSwapPacket(4, 3, commWarpOwnList))

	if len(f.moves) != 1 || f.moves[0] != [2]int{4, 3} {
		t.Fatalf("the store was asked to move %v, want one move 4 -> 3", f.moves)
	}
	want := "a,b,c,e,d"
	if got := strings.Join(warpMemos(t, out), ","); got != want {
		t.Fatalf("the window draws %s, want %s (row 4 moved UP into row 3)", got, want)
	}
	if back := strings.Join(storeMemos(t, f), ","); back != want {
		t.Fatalf("the store re-reads as %s, want %s - a move that is not stored is undone "+
			"by the next refresh", back, want)
	}
	wantFourWarpLists(t, out, 5)
	line := logs.String()
	for _, w := range []string{"CZ_COMMWARP_SWAP (type0 rows 4 -> 3)", "slot 4", "moved"} {
		if !strings.Contains(line, w) {
			t.Fatalf("the swap line does not name %q:\n%s", w, line)
		}
	}
}

// TestWarpListSwapMovesARowDown is the Down button: row 1 -> 2. It is the same MOVE in
// the other direction, so the row lands one place lower and the rest keep their order.
func TestWarpListSwapMovesARowDown(t *testing.T) {
	h, f, _ := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(1, 15, 4, 5, 6, "b"),
		warpRow(2, 15, 7, 8, 9, "c"),
		warpRow(3, 15, 10, 11, 12, "d"),
		// Slot 4 is missing on purpose: the display order must not be the slot numbers.
		warpRow(5, 15, 13, 14, 15, "e"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSwap, warpSwapPacket(1, 2, commWarpOwnList))

	if len(f.moves) != 1 || f.moves[0] != [2]int{1, 2} {
		t.Fatalf("the store was asked to move %v, want one move 1 -> 2", f.moves)
	}
	want := "a,c,b,d,e"
	if got := strings.Join(warpMemos(t, out), ","); got != want {
		t.Fatalf("the window draws %s, want %s (row 1 moved DOWN into row 2)", got, want)
	}
	if back := strings.Join(storeMemos(t, f), ","); back != want {
		t.Fatalf("the store re-reads as %s, want %s", back, want)
	}
}

// TestWarpListSwapOutOfRangeRefuses: a swap naming a row the list does not have writes
// nothing, re-sends no list, prints ONE log line and sends ONE message - the store is
// never even asked.
func TestWarpListSwapOutOfRangeRefuses(t *testing.T) {
	for _, tc := range []struct{ from, to int }{{0, 5}, {5, 0}, {2, 2 + 9}} {
		h, f, logs := warpListHandler(t,
			warpRow(0, 15, 1, 2, 3, "a"),
			warpRow(1, 15, 4, 5, 6, "b"))
		sess := teleportSession([3]int32{3175, 4203, 77})

		out := warpReply(t, h, sess, CmdCZCommWarpSwap,
			warpSwapPacket(tc.from, tc.to, commWarpOwnList))

		wantOneWarpRefusalMessage(t, out, warpNoSuchRowText)
		if len(f.moves) != 0 {
			t.Fatalf("%d -> %d: the store was asked to move %v; an out-of-range row must "+
				"not reach it", tc.from, tc.to, f.moves)
		}
		if back := strings.Join(storeMemos(t, f), ","); back != "a,b" {
			t.Fatalf("%d -> %d: the list is now %s, want a,b", tc.from, tc.to, back)
		}
		lines := logLines(logs)
		if len(lines) != 1 {
			t.Fatalf("%d -> %d: %d log line(s), want exactly 1:\n%s", tc.from, tc.to,
				len(lines), logs.String())
		}
		for _, want := range []string{"REFUSED", fmt.Sprintf("rows %d -> %d", tc.from, tc.to),
			"2 point(s)"} {
			if !strings.Contains(lines[0], want) {
				t.Fatalf("%d -> %d: the refusal does not name %q:\n%s", tc.from, tc.to,
					want, lines[0])
			}
		}
		t.Logf("an out-of-range swap refusal: %s", lines[0])
	}
}

// TestWarpListSwapRefusedForCashType: types 1-3 are the cash lists, which this server
// keeps empty, so a swap there can reorder nothing. Refused with one log line and ONE
// message, and no list is re-sent.

// TestWarpListSwapActsInsideTheNamedList: 3639's two indices are positions inside the ONE
// list the packet names (teltype at +18). A move in "Warp Extend(2)" reorders that panel
// and leaves the basic list exactly as it was.
func TestWarpListSwapActsInsideTheNamedList(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 100, 200, 5, "basic first"),
		warpRow(1, 15, 300, 400, 7, "basic second"),
		warpRowIn(2, 0, 15, 500, 600, 9, "c"),
		warpRowIn(2, 1, 15, 700, 800, 11, "d"),
		warpRowIn(2, 2, 15, 900, 1000, 13, "e"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSwap, warpSwapPacket(0, 2, 2))

	wantWarpCounts(t, out, [commWarpTypes]int{2, 0, 3, 0})
	if len(f.moves) != 1 || f.moves[0] != [2]int{0, 2} {
		t.Fatalf("the store was asked to move %v, want one 0 -> 2 inside type 2", f.moves)
	}
	memos := func(p []byte) []string {
		var got []string
		for i := 0; i < int(p[5]); i++ {
			at := commWarpRecordsAt + i*commWarpRecord + commWarpMemo
			s := string(p[at:])
			if j := strings.IndexByte(s, 0); j >= 0 {
				s = s[:j]
			}
			got = append(got, s)
		}
		return got
	}
	if got := strings.Join(memos(out[0]), ","); got != "basic first,basic second" {
		t.Fatalf("type 0 order = %s, want basic first,basic second - untouched", got)
	}
	if got := strings.Join(memos(out[2]), ","); got != "d,e,c" {
		t.Fatalf("type 2 order = %s, want d,e,c (row 0 spliced in at row 2)", got)
	}
	if !strings.Contains(logs.String(), "type2 rows 0 -> 2") {
		t.Fatalf("the log does not name the list and the move:\n%s", logs.String())
	}
}

// TestParseCommWarpSwapFromTheLiveBytes pins the 19 bytes the owner's Up click really
// sent, byte for byte (logs\zone15_live.log 2026-10-30 15:07:46). The two indices are
// FOUR bytes each here, unlike the remove's and the rename's single bytes, and a parser
// that read them as bytes would move row 4 to row 0 instead of 4 to 3.
func TestParseCommWarpSwapFromTheLiveBytes(t *testing.T) {
	live := []byte{
		0x37, 0x0e, // cmd 3639
		0x76, 0x00, 0x00, 0x00, // seq
		0x34, 0x00, 0x00, 0x00, // checksum
		0x04, 0x00, 0x00, 0x00, // from = 4
		0x03, 0x00, 0x00, 0x00, // to = 3
		0x00, //                    teltype 0
	}
	if len(live) != commWarpSwapSize {
		t.Fatalf("the captured packet is %d bytes and commWarpSwapSize is %d",
			len(live), commWarpSwapSize)
	}
	from, to, telType, ok := ParseCommWarpSwap(live)
	if !ok || from != 4 || to != 3 || telType != commWarpOwnList {
		t.Fatalf("parsed from=%d to=%d teltype=%d ok=%v, want 4, 3, 0, ok", from, to,
			telType, ok)
	}
	if cmd := binary.LittleEndian.Uint16(live[0:]); cmd != CmdCZCommWarpSwap {
		t.Fatalf("the captured command is %d, want %d", cmd, CmdCZCommWarpSwap)
	}

	// Anything shorter than the 19 bytes has no teltype, so it is refused rather than
	// read as a swap of row 0 to row 0.
	for _, n := range []int{0, 10, 14, 18} {
		if _, _, _, ok := ParseCommWarpSwap(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a swap; it needs %d", n, commWarpSwapSize)
		}
	}
}

// TestCommWarpMemoFullFieldStoresAllThirty is the other half of the rule: a memo that
// fills the field is NOT cut short and is NOT mistaken for junk. Both shapes the client
// can send must store all 30 accepted characters.
func TestCommWarpMemoFullFieldStoresAllThirty(t *testing.T) {
	terminated := strings.Repeat("C", commWarpMemoChars) // 30 characters, then the NUL

	for _, tc := range []struct {
		name  string
		text  string
		field []byte
	}{
		{"30 characters and a NUL terminator", terminated, warpCString(terminated, nil)},
		// A field with NO NUL in it at all: 31 bytes of text. The client cannot send this,
		// but the parser must not lose the 30 characters it accepts either.
		{"31 bytes and no NUL", strings.Repeat("C", commWarpMemoField),
			[]byte(strings.Repeat("C", commWarpMemoField))},
	} {
		h, f, logs := pgStringHandler(t)
		sess := teleportSession([3]int32{3175, 4203, 77})

		out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSaveCString(commWarpOwnList, tc.field))

		if len(f.refused) != 0 || len(f.saved) != 1 {
			t.Fatalf("%s: %d row(s) written and %d refusal(s), want 1 and 0:\n%s",
				tc.name, len(f.saved), len(f.refused), logs.String())
		}
		if want := strings.Repeat("C", commWarpMemoChars); f.saved[0].Name != want {
			t.Fatalf("%s: stored memo is %d character(s), want all %d",
				tc.name, len(f.saved[0].Name), commWarpMemoChars)
		}
		// The 30-character case is not "cut", so it must not claim to be.
		if strings.Contains(logs.String(), "character(s) and this client accepts") &&
			tc.text == terminated {
			t.Fatalf("%s: a 30-character memo was reported as cut:\n%s", tc.name, logs.String())
		}
		wantSavedConfirmation(t, out, 1)
	}
}

// TestCommWarpInvalidUTF8MemoRefusedBeforeTheStore: a memo that is not UTF-8 text is
// refused HERE, with ONE line naming its bytes - nothing written, nothing sent, and the
// store never asked. That keeps PostgreSQL's SQLSTATE 22021 from being the first thing a
// reader sees.
func TestCommWarpInvalidUTF8MemoRefusedBeforeTheStore(t *testing.T) {
	// "caf\xe9": Latin-1 e-acute, which is not UTF-8 on its own. No NUL appears before it,
	// so cutting cannot rescue it.
	field := warpCString("caf\xe9", nil)

	for _, tc := range []struct {
		name string
		cmd  uint16
		pt   []byte
	}{
		{"save", CmdCZCommWarpSave, warpSaveCString(commWarpOwnList, field)},
		{"rename", CmdCZCommWarpRename, warpRenameCString(0, commWarpOwnList, field)},
	} {
		h, f, logs := pgStringHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
		sess := teleportSession([3]int32{3175, 4203, 77})

		out := warpReply(t, h, sess, tc.cmd, tc.pt)

		wantOneWarpRefusalMessage(t, out, warpBadNameText)
		if len(f.refused) != 0 || f.saves != 0 || len(f.renames) != 0 {
			t.Fatalf("%s: the store was reached (%d save call(s), %d refusal(s)) - the check "+
				"must come first", tc.name, f.saves, len(f.refused))
		}
		if len(f.rows) != 1 || f.rows[0].Name != "a" {
			t.Fatalf("%s: the list changed to %v; a refusal writes nothing", tc.name, f.rows)
		}
		lines := logLines(logs)
		if len(lines) != 1 {
			t.Fatalf("%s: %d log line(s), want exactly 1:\n%s", tc.name, len(lines), logs.String())
		}
		for _, want := range []string{"REFUSED", "not UTF-8", "63 61 66 e9",
			"nothing was written"} {
			if !strings.Contains(lines[0], want) {
				t.Fatalf("%s: the refusal does not name %q:\n%s", tc.name, want, lines[0])
			}
		}
		// Printed so the exact line a refusal writes is on record, not just asserted.
		t.Logf("%s refusal: %s", tc.name, lines[0])
	}
}


// ---- R76 slice 3: the basic Warp List holds TEN, and the client is TOLD so --------

// TestWarpListTenthSaveSucceedsAndEleventhRefused is the size of the window the owner
// asked for. The reference server's basic "Warp List" panel holds ten points (his
// screenshot: "10 is the basic list"), so with nine rows the TENTH save must land, and
// only the ELEVENTH is refused - nothing written, nothing sent, one line, list still ten.
func TestWarpListTenthSaveSucceedsAndEleventhRefused(t *testing.T) {
	// Ten is the number this test is about, so it is written out rather than hidden
	// behind the constant: if someone moves the cap again, this test says so.
	if MaxSavedTeleports != 10 {
		t.Fatalf("MaxSavedTeleports = %d, want 10 - the basic Warp List holds ten "+
			"points on the reference server", MaxSavedTeleports)
	}
	nine := make([]SavedPoint, 0, MaxSavedTeleports-1)
	for i := 0; i < MaxSavedTeleports-1; i++ {
		nine = append(nine, warpRow(i, 15, int32(100+i), int32(200+i), int32(i)+1, "old"))
	}
	h, f, logs := warpListHandler(t, nine...)
	sess := teleportSession([3]int32{3175, 4203, 77})

	out := warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Tenth point"))

	if len(f.saved) != 1 {
		t.Fatalf("the tenth save wrote %d row(s), want 1 - ten is the cap, not the "+
			"refusal point", len(f.saved))
	}
	if len(f.rows) != MaxSavedTeleports {
		t.Fatalf("the list holds %d row(s) after the tenth save, want %d",
			len(f.rows), MaxSavedTeleports)
	}
	// A save answers with the four lists AND the reference's success line.
	wantSavedConfirmation(t, out, MaxSavedTeleports)
	// The client draws the tenth point because the count byte says ten and the tenth
	// 46-byte record carries the memo it was given.
	if got := int(out[0][5]); got != MaxSavedTeleports {
		t.Fatalf("count byte after the tenth save = %d, want %d", got, MaxSavedTeleports)
	}
	lastAt := commWarpRecordsAt + (MaxSavedTeleports-1)*commWarpRecord
	if memo := out[0][lastAt+commWarpMemo : lastAt+commWarpMemo+commWarpMemoSize]; !bytes.
		HasPrefix(memo, []byte("Tenth point")) {
		t.Fatalf("the tenth record's memo = %q, want \"Tenth point\"", memo)
	}

	// THE ELEVENTH: refused, nothing written, nothing sent, one line naming the memo.
	//
	// The leader WALKS FIRST, and that matters since R76 slice 5: a duplicate spot is
	// refused by coordinates, and it is answered BEFORE the cap. Saving again on the
	// tenth point's own spot would therefore be a duplicate refusal, which is a
	// different rule and a different message - this test is about the CAP, so the
	// eleventh click is on a spot that holds nothing.
	sess.Pos = [3]int32{-1088, 9339, 128}
	out = warpReply(t, h, sess, CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Eleventh point"))
	if len(f.saved) != 1 {
		t.Fatalf("the eleventh save wrote %d row(s) in total, want the 1 from the tenth",
			len(f.saved))
	}
	if len(f.rows) != MaxSavedTeleports {
		t.Fatalf("the list holds %d row(s) after the refusal, want %d - nothing may be "+
			"replaced", len(f.rows), MaxSavedTeleports)
	}
	for _, r := range f.rows {
		if r.Name != "old" && r.Name != "Tenth point" {
			t.Fatalf("a stored row changed to %q; a refused save must write nothing", r.Name)
		}
	}
	// Nothing was sent that could claim the list changed: no 3634 list is re-sent on a
	// refusal. The refusal's ONE ZC_MESSAGE line is not a list update, so it is allowed
	// here - a 3634 would be a lie.
	for _, p := range out {
		if cmdOf(p) == CmdZCCommWarpList {
			t.Fatalf("a 3634 list was re-sent on the refusal: the client's list is "+
				"already true and a re-send would say something happened")
		}
	}
	wantOneWarpRefusalMessage(t, out, warpFullText())
	lines := logLines(logs)
	refusal := lines[len(lines)-1]
	for _, want := range []string{"REFUSED", "already holds 10 row(s)", `"Eleventh point"`,
		"DISCARDED"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, refusal)
		}
	}
	t.Logf("eleventh-refusal line: %s / message: %s", refusal, warpFullText())
}

// TestUpdatePropertyNumberLockWarpExactBytes pins the whole 46-byte 3748 packet that
// tells the client its Warp List holds ten. Why exact bytes: this packet has NO length
// word, so a length word written where the property name starts shifts every field and
// the client reads a property called "Lock\0..." - which it then ignores, leaving the
// window on its XML default of 5 with nothing in any log.
func TestUpdatePropertyNumberLockWarpExactBytes(t *testing.T) {
	got := BuildUpdatePropertyNumber(lockWarpProperty, MaxSavedTeleports)

	// cmd(2) + idSpace[20] + property[20] + value(4) = 46, and no length word.
	if len(got) != 46 {
		t.Fatalf("packet = %d bytes, want 46 (cmd + 20 + 20 + 4, no length word)", len(got))
	}
	// "a40e" cmd 3748 = 0x0EA4, "Lock" then 16 NULs, "Lock_Warp" then 11 NULs,
	// "0a000000" value 10.
	want := "a40e" +
		hex.EncodeToString([]byte("Lock")) + strings.Repeat("00", 16) +
		hex.EncodeToString([]byte("Lock_Warp")) + strings.Repeat("00", 11) +
		"0a000000"
	if now := hex.EncodeToString(got); now != want {
		t.Fatalf("packet hex:\n got %s\nwant %s", now, want)
	}
	// The three fields, read back the way the client reads them.
	if idSpace := cutCString(got[2:22]); idSpace != "Lock" {
		t.Fatalf("idSpace = %q, want \"Lock\"", idSpace)
	}
	if name := cutCString(got[22:42]); name != "Lock_Warp" {
		t.Fatalf("property = %q, want \"Lock_Warp\"", name)
	}
	if v := binary.LittleEndian.Uint32(got[42:]); v != MaxSavedTeleports {
		t.Fatalf("value = %d, want %d (the server's own cap)", v, MaxSavedTeleports)
	}
}

// TestEntryBurstSendsOneLockWarp is the rule "the cap the server enforces and the value
// the client is told come from ONE place". Whatever `commander_property` holds - nothing,
// a stale 5, even two rows - the burst carries exactly ONE Lock_Warp packet and its
// value is the server's cap. The other Lock_* rows are still sent, unchanged.
func TestEntryBurstSendsOneLockWarp(t *testing.T) {
	cases := []struct {
		name  string
		props []Property
		// stale is the row that must be reported as disagreeing, or 0 for none.
		stale uint32
	}{
		{name: "no row at all", props: nil},
		{
			name: "the owner's commander today",
			props: []Property{
				{Name: "Lock_InventoryCount", Value: 248},
			},
		},
		{
			name: "a stale row of 5",
			props: []Property{
				{Name: "Lock_InventoryCount", Value: 248},
				{Name: lockWarpProperty, Value: 5},
			},
			stale: 5,
		},
		{
			name: "two rows, the cap still wins",
			props: []Property{
				{Name: lockWarpProperty, Value: 5},
				{Name: lockWarpProperty, Value: 7},
			},
			stale: 5,
		},
	}
	wantLockWarp := BuildUpdatePropertyNumber(lockWarpProperty, MaxSavedTeleports)
	for _, tc := range cases {
		packets, _ := EntryBurst(tc.props, nil)

		warpAt := -1
		sent := 0
		for i, p := range packets {
			if cmdOf(p) != CmdZCUpdatePropertyNum {
				continue
			}
			sent++
			if cutCString(p[22:42]) == lockWarpProperty {
				if warpAt >= 0 {
					t.Fatalf("%s: more than one Lock_Warp packet went out", tc.name)
				}
				warpAt = i
			}
		}
		if warpAt < 0 {
			t.Fatalf("%s: the burst carried no Lock_Warp packet, so the client keeps "+
				"drawing its own 5", tc.name)
		}
		if !bytes.Equal(packets[warpAt], wantLockWarp) {
			t.Fatalf("%s: Lock_Warp packet = %x, want %x (value %d)", tc.name,
				packets[warpAt], wantLockWarp, MaxSavedTeleports)
		}
		// The rows the burst sent BEFORE this slice are still there, untouched.
		if len(tc.props) > 0 && tc.props[0].Name == "Lock_InventoryCount" {
			found := false
			for _, p := range packets {
				if cmdOf(p) == CmdZCUpdatePropertyNum &&
					cutCString(p[22:42]) == "Lock_InventoryCount" &&
					binary.LittleEndian.Uint32(p[42:]) == 248 {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: the Lock_InventoryCount=248 packet is gone", tc.name)
			}
			if sent != 2 {
				t.Fatalf("%s: %d property packet(s), want 2 (Lock_InventoryCount and "+
					"Lock_Warp)", tc.name, sent)
			}
		}

		// The pure function also hands the disagreement back, which is what the zone
		// logs once - the CAP still wins.
		_, stale := PropsWithWarpCap(tc.props)
		switch {
		case tc.stale == 0 && stale != nil:
			t.Fatalf("%s: reported a disagreement with %+v, want none", tc.name, *stale)
		case tc.stale != 0 && stale == nil:
			t.Fatalf("%s: no disagreement reported, want the %d row", tc.name, tc.stale)
		case stale != nil && stale.Value != tc.stale:
			t.Fatalf("%s: reported %d as the stale value, want %d", tc.name, stale.Value,
				tc.stale)
		}
	}
}

// TestWarpCapWarningNamesBothNumbers pins the ONE line a disagreement logs. It is
// tested here because the entry path that prints it needs a live login, and a warning
// nobody has read is a warning that drifts: the two numbers and "the CAP wins" are the
// whole point of it.
func TestWarpCapWarningNamesBothNumbers(t *testing.T) {
	got := WarpCapWarning(Property{Name: lockWarpProperty, Value: 5})
	want := "zone: commander_property says Lock_Warp=5 but this server's Warp List " +
		"holds 10, so the CAP wins and 10 is what the client was told (the row itself " +
		"was not changed)"
	if got != want {
		t.Fatalf("warning:\n got %s\nwant %s", got, want)
	}
	t.Logf("%s", got)
}

// ---- R76 slice 4: a REFUSAL says so on screen, and a SUCCESS still says it once ----

// TestWarpListRefusalMessagesNameTheRightReason is the whole "not a dead button" rule in
// ONE place: the three refusals this window has, each asserted ON THE BYTES of the one
// 3596 packet it sends.
//
//	the full list   -> names the cap, and the cap is read from MaxSavedTeleports
//	a cash list     -> says the list is NOT AVAILABLE (never "full": nothing is filled)
//	an index with
//	no such row     -> says that row is not in the list
//
// A refusal must never send the 3634 list as well: nothing changed, so re-sending it
// would claim something did.
func TestWarpListRefusalMessagesNameTheRightReason(t *testing.T) {
	full := make([]SavedPoint, 0, MaxSavedTeleports)
	for i := 0; i < MaxSavedTeleports; i++ {
		full = append(full, warpRow(i, 15, int32(100+i), int32(200+i), int32(i)+1, "old"))
	}

	type check struct {
		name string
		run  func(t *testing.T) [][]byte
		want string
		// notContain is a word the text must NOT use, when a wrong word would mislead.
		notContain string
	}
	cases := []check{
		{
			name: "a full list names the cap of ten",
			want: warpFullText(),
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t, full...)
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "eleventh"))
			},
		},
		{
			name: "a duplicate spot says it is already saved, by coordinates",
			want: warpDuplicateText,
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t, warpRow(0, 15, 3175, 4203, 77, "Bank"))
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "another name"))
			},
		},
		{
			name: "a full extra list names THAT panel, not the basic list",
			want: warpFullTextFor(2),
			run: func(t *testing.T) [][]byte {
				rows := make([]SavedPoint, 0, MaxSavedTeleports)
				for i := 0; i < MaxSavedTeleports; i++ {
					rows = append(rows, warpRowIn(2, i, 15, int32(800+i),
						int32(900+i), int32(i)+1, "old extend"))
				}
				h, _, _ := warpListHandler(t, rows...)
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpSave, warpSavePacket(2, "full"))
			},
		},
		{
			name:       "a teltype that names no panel is refused as broken, not as unserved",
			want:       warpBadListText,
			notContain: "not available",
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t)
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpSave, warpSavePacket(9, "no such list"))
			},
		},
		{
			name: "a row that does not exist says so (remove)",
			want: warpNoSuchRowText,
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpRemove, warpRowPacket(CmdCZCommWarpRemove, 7, 0, ""))
			},
		},
		{
			name: "a row that does not exist says so (rename)",
			want: warpNoSuchRowText,
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpRename, warpRowPacket(CmdCZCommWarpRename, 7, 0, "x"))
			},
		},
		{
			name: "a row that does not exist says so (swap)",
			want: warpNoSuchRowText,
			run: func(t *testing.T) [][]byte {
				h, _, _ := warpListHandler(t, warpRow(0, 15, 1, 2, 3, "a"))
				return warpReply(t, h, teleportSession([3]int32{3175, 4203, 77}),
					CmdCZCommWarpSwap, warpSwapPacket(0, 7, commWarpOwnList))
			},
		},
	}

	for _, tc := range cases {
		out := tc.run(t)
		wantOneWarpRefusalMessage(t, out, tc.want)
		for _, p := range out {
			if cmdOf(p) == CmdZCCommWarpList {
				t.Fatalf("%s: a 3634 list was re-sent on a refusal", tc.name)
			}
		}
		if tc.notContain != "" &&
			strings.Contains(strings.ToLower(tc.want), strings.ToLower(tc.notContain)) {
			t.Fatalf("%s: the message reads %q, which uses the word %q", tc.name, tc.want,
				tc.notContain)
		}
		t.Logf("%s: %q", tc.name, tc.want)
	}

	// The cap is the constant, not a typed-in ten: the sentence follows the store.
	if want := fmt.Sprintf("%d", MaxSavedTeleports); !strings.Contains(warpFullText(), want) {
		t.Fatalf("the full-list message %q does not name the cap %d", warpFullText(),
			MaxSavedTeleports)
	}
}

// TestWarpListOnlyTheSaveCarriesASuccessMessage is the rule as the REFERENCE has it
// (R76 slice 5). Slice 4 sent no text on a success, on the reasoning that "the row
// appearing is the confirmation". The live reference server disagrees for a SAVE: it
// prints "The warp location has been saved successfully" as a system line, and the owner
// photographed it. So a save answers with four 3634 lists PLUS that one line.
//
// A REMOVE, RENAME or SWAP still answers with the lists alone. Nothing was photographed
// for those, and a message "for symmetry" would be a sentence the reference may not say.
// That half is what this test keeps from slice 4.
//
// It counts the packets rather than reading the log, because the mistake it guards
// against is a packet that should not be there at all.
func TestWarpListOnlyTheSaveCarriesASuccessMessage(t *testing.T) {
	h, f, logs := warpListHandler(t,
		warpRow(0, 15, 1, 2, 3, "a"),
		warpRow(1, 15, 4, 5, 6, "b"))
	sess := teleportSession([3]int32{3175, 4203, 77})

	for _, tc := range []struct {
		name string
		cmd  uint16
		pt   []byte
		// wantText is the ONE message this answer must carry, or "" for none at all.
		wantText string
	}{
		{"save", CmdCZCommWarpSave, warpSavePacket(commWarpOwnList, "Bank"), warpSavedText},
		{"remove", CmdCZCommWarpRemove, warpRowPacket(CmdCZCommWarpRemove, 0, 0, ""), ""},
		{"rename", CmdCZCommWarpRename, warpRowPacket(CmdCZCommWarpRename, 0, 0, "Home"), ""},
		{"swap", CmdCZCommWarpSwap, warpSwapPacket(1, 0, commWarpOwnList), ""},
	} {
		out := warpReply(t, h, sess, tc.cmd, tc.pt)

		wantPackets := commWarpTypes
		extra := ""
		if tc.wantText != "" {
			wantPackets++
			extra = " plus one ZC_MESSAGE"
		}
		if len(out) != wantPackets {
			t.Fatalf("%s: %d packet(s), want %d (four 3634 lists%s)", tc.name, len(out),
				wantPackets, extra)
		}
		lists, messages := 0, 0
		for _, p := range out {
			switch cmdOf(p) {
			case CmdZCCommWarpList:
				lists++
			case CmdZCMessage:
				messages++
				if tc.wantText == "" {
					t.Fatalf("%s: a ZC_MESSAGE was sent on a success that has no reference "+
						"wording - the lists are the whole answer", tc.name)
				}
				wantMessageText(t, p, tc.wantText)
			default:
				t.Fatalf("%s: packet cmd %d, want only %d or %d", tc.name, cmdOf(p),
					CmdZCCommWarpList, CmdZCMessage)
			}
		}
		if lists != commWarpTypes {
			t.Fatalf("%s: %d 3634 list(s), want %d (one per teleport type)", tc.name,
				lists, commWarpTypes)
		}
		wantMessages := 0
		if tc.wantText != "" {
			wantMessages = 1
		}
		if messages != wantMessages {
			t.Fatalf("%s: %d ZC_MESSAGE packet(s), want %d", tc.name, messages, wantMessages)
		}
	}
	if saved := len(f.saved); saved != 1 {
		t.Fatalf("%d row(s) written, want 1 - the saves above must have worked", saved)
	}
	if strings.Contains(logs.String(), "REFUSED") {
		t.Fatalf("a success logged a refusal:\n%s", logs.String())
	}
}
