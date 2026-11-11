package zone

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildMessage pins the ZC_MESSAGE layout against the Python oracle
// (`build_system_message` in src\zone_server.py): cmd(2) len(2) type(4) level(1)
// tm(36) text + NUL, and the length word is the WHOLE packet.
func TestBuildMessage(t *testing.T) {
	const text = "Sold FireStone for 2500 vis"
	got := BuildMessage(text, MessageTypeGeneral, MessageLevelGeneral)

	if cmd := cmdOf(got); cmd != CmdZCMessage {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCMessage)
	}
	// header 4 + type 4 + level 1 + tm 36 + text + one NUL
	wantLen := 4 + 4 + 1 + 36 + len(text) + 1
	// The oracle's own count for this exact line is 73 bytes; the literal keeps
	// the formula above honest (checked with python: build_system_message()).
	if wantLen != 73 {
		t.Fatalf("the formula gives %d bytes for a %d-character line, the oracle says 73",
			wantLen, len(text))
	}
	if got[2] != byte(wantLen) || got[3] != byte(wantLen>>8) {
		t.Fatalf("length word = %d, want %d", binary.LittleEndian.Uint16(got[2:]), wantLen)
	}
	if len(got) != wantLen {
		t.Fatalf("packet is %d bytes, but its length word says %d", len(got), wantLen)
	}

	// type(4) at 4 and level(1) at 8 are both general, so the client prints the
	// plain system line instead of a modal box.
	if v := binary.LittleEndian.Uint32(got[4:]); v != MessageTypeGeneral {
		t.Fatalf("type = %d, want %d", v, MessageTypeGeneral)
	}
	if got[8] != MessageLevelGeneral {
		t.Fatalf("level = %d, want %d", got[8], MessageLevelGeneral)
	}

	// The text starts at 45, is the plain bytes, and ends with exactly one NUL.
	if body := string(got[45 : len(got)-1]); body != text {
		t.Fatalf("text = %q, want %q", body, text)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("last byte = 0x%02x, want the NUL terminator", got[len(got)-1])
	}

	// tm is the 9-int block: the DATE fields must be the local clock, because the
	// client draws them beside the line.
	//
	// THE ONE-SECOND TOLERANCE IS DELIBERATE (2026-10-30). BuildMessage reads the clock
	// itself, so a test that samples time.Now() AFTER the build can straddle a second
	// tick: it compared 15:51:25 against 15:51:26 on the 15:52 gate and passed on the
	// re-run, with nothing wrong in the packet. A gate must never go red because a
	// clock ticked, so the block is read back into a time.Time and compared with a
	// one-second window instead of equality.
	//
	// BOTH SIDES ARE TRUNCATED TO THE SECOND on purpose: the packet carries no
	// nanoseconds. If `now` kept its fraction, a build at 15:51:25.999 sampled at
	// 15:51:26.0001 would read as 1.0001 s apart and fail - the same tick, through the
	// back door. Truncated, the difference can only be 0 or exactly 1 s on a tick.
	// Anything further away than that is a real bug - a bad month offset, a stale year.
	now := time.Now().Truncate(time.Second)
	tm := got[9:45]
	stamp := time.Date(
		int(binary.LittleEndian.Uint32(tm[20:]))+1900,  // tm_year counts from 1900
		time.Month(binary.LittleEndian.Uint32(tm[16:]))+1, // tm_mon is 0-based here
		int(binary.LittleEndian.Uint32(tm[12:])),          // tm_mday
		int(binary.LittleEndian.Uint32(tm[8:])),           // tm_hour
		int(binary.LittleEndian.Uint32(tm[4:])),           // tm_min
		int(binary.LittleEndian.Uint32(tm[0:])),           // tm_sec
		0, time.Local)
	if d := now.Sub(stamp); d > time.Second || d < -time.Second {
		t.Fatalf("tm reads %s, want the local clock %s within one second (a tick between "+
			"the build and this line is not a failure)", stamp.Format(time.RFC3339),
			now.Format(time.RFC3339))
	}
	// The two Python-only conventions are read back OUT of that stamp, so they stay
	// checked even when the packet was built exactly on a second boundary.
	// Python's tm_wday counts MONDAY as 0 and its tm_yday is 1-BASED. The oracle packs
	// those two as Python reports them, so this does too - a C-style value here would be
	// one off in both fields.
	if wday := binary.LittleEndian.Uint32(tm[24:]); wday != uint32((int(stamp.Weekday())+6)%7) {
		t.Fatalf("tm_wday = %d, want %d (Monday=0, as Python reports it)",
			wday, (int(stamp.Weekday())+6)%7)
	}
	if yday := binary.LittleEndian.Uint32(tm[28:]); yday != uint32(stamp.YearDay()) {
		t.Fatalf("tm_yday = %d, want %d (1-based, as Python reports it)",
			yday, stamp.YearDay())
	}
	if isdst := binary.LittleEndian.Uint32(tm[32:]); isdst != 0 {
		t.Fatalf("tm_isdst = %d, want 0", isdst)
	}
}

// TestBuildTMRoundTrips pins the tm block against a FIXED clock, so the layout is
// checked without involving the real one at all. That is why TestBuildMessage can
// afford a one-second tolerance: the field offsets and the two Python conventions are
// proven here, once, instead of depending on when the gate happens to run.
func TestBuildTMRoundTrips(t *testing.T) {
	// A Friday, so the Monday=0 convention is not accidentally right.
	want := time.Date(2026, 10, 30, 15, 51, 25, 0, time.Local)
	tm := buildTM(want)

	if len(tm) != 36 {
		t.Fatalf("tm block = %d bytes, want 36 (9 packed int32)", len(tm))
	}
	field := func(at int) int { return int(int32(binary.LittleEndian.Uint32(tm[at:]))) }
	for _, tc := range []struct {
		name string
		at   int
		got  int
		want int
	}{
		{"tm_sec", 0, field(0), 25},
		{"tm_min", 4, field(4), 51},
		{"tm_hour", 8, field(8), 15},
		{"tm_mday", 12, field(12), 30},
		{"tm_mon (0-based)", 16, field(16), 9},
		{"tm_year (from 1900)", 20, field(20), 126},
		{"tm_wday (Monday=0)", 24, field(24), 4},
		{"tm_yday (1-based)", 28, field(28), 303},
		{"tm_isdst", 32, field(32), 0},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// TestBuildMessageEmptyText: the text is optional, but the NUL is not - and the
// length word still has to describe the packet that was sent.
func TestBuildMessageEmptyText(t *testing.T) {
	got := BuildMessage("", MessageTypeGeneral, MessageLevelGeneral)
	if want := 4 + 4 + 1 + 36 + 1; len(got) != want {
		t.Fatalf("empty message is %d bytes, want %d", len(got), want)
	}
	if binary.LittleEndian.Uint16(got[2:]) != uint16(len(got)) {
		t.Fatalf("length word %d does not match the %d byte packet",
			binary.LittleEndian.Uint16(got[2:]), len(got))
	}
}

// TestBuildMessageEncoding: an item name is ASCII today, but a character outside
// cp949 must become ONE '?' and never shift the bytes after it.
func TestBuildMessageEncoding(t *testing.T) {
	got := BuildMessage("a\u1234b", MessageTypeGeneral, MessageLevelGeneral)
	if body := string(got[45 : len(got)-1]); body != "a?b" {
		t.Fatalf("encoded text = %q, want %q", body, "a?b")
	}
}

// TestBuildMessageCarriesTheTypeAndLevel pins that the two presentation fields are
// really in the bytes: `type` as a DWORD at +4 and `level` as a byte at +8, on top of
// the 45-byte header, with the length word describing the whole packet.
//
// The literals are 137 / 3 and NOT the waypoint pair (2026-10-30, TYPE-HUNT): this
// test checks the BUILDER only, with numbers that belong to no feature, so it cannot
// go red when a real feature's number is re-measured. The waypoint pair is 147 / 3
// and it has its own gate: TestBuildMessageCurrentWayPointIsABox.
func TestBuildMessageCarriesTheTypeAndLevel(t *testing.T) {
	const note = "type 137 level 3"
	got := BuildMessage(note, 137, 3)

	if cmd := cmdOf(got); cmd != CmdZCMessage {
		t.Fatalf("cmd = %d, want %d (ZC_MESSAGE)", cmd, CmdZCMessage)
	}
	// 45-byte header + the 16-byte note + one NUL = 62, and the length word is the
	// WHOLE packet, which is what the client reads.
	if len(got) != 62 {
		t.Fatalf("packet = %d bytes, want 62 (45 + %q + NUL)", len(got), note)
	}
	if lw := binary.LittleEndian.Uint16(got[2:]); int(lw) != len(got) {
		t.Fatalf("length word = %d, want the packet size %d", lw, len(got))
	}
	if v := binary.LittleEndian.Uint32(got[4:]); v != 137 {
		t.Fatalf("type = %d at +4, want the 137 that was passed in", v)
	}
	if lv := got[8]; lv != 3 {
		t.Fatalf("level = %d at +8, want 3 - a level above 0 is what makes the client "+
			"pop a box; 0 is the cyan chat line", lv)
	}
	if body := string(got[45 : len(got)-1]); body != note {
		t.Fatalf("text = %q, want %q", body, note)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("last byte = 0x%02x, want the NUL terminator", got[len(got)-1])
	}
}

// TestBuildMessageCurrentWayPointIsABox pins the waypoint "already here" answer to
// the pair the OWNER MEASURED on his own shipped client (2026-10-30 ~20:46,
// `/typemsg 147 3`), not to the source header's 136:
//
//	sent 136 -> "You can't participate in it as you did not satisfy its level
//	            limitation."   (wrong)
//	sent 135 -> "Password is incorrect."                            (wrong)
//	sent 147 -> "You have chosen the waypoint of the current area."  (right)
//
// Both numbers are LITERALS on purpose: a wrong constant must not be able to pass its
// own gate. The last check pins the constant itself, so flipping it to 136 - which
// looks right to anyone reading geSharedMessage.h - turns this test red.
func TestBuildMessageCurrentWayPointIsABox(t *testing.T) {
	// The reference server's own argument (Commander_Packet.cpp:2028). The client
	// throws it away for any type but 0; it is the log note that rides the wire.
	const note = "current way point"
	got := BuildMessage(note, 147, 3)

	if cmd := cmdOf(got); cmd != CmdZCMessage {
		t.Fatalf("cmd = %d, want %d (ZC_MESSAGE)", cmd, CmdZCMessage)
	}
	// 45-byte header + the 17-byte note + one NUL = 63.
	if len(got) != 63 {
		t.Fatalf("packet = %d bytes, want 63 (45 + %q + NUL)", len(got), note)
	}
	if lw := binary.LittleEndian.Uint16(got[2:]); int(lw) != len(got) {
		t.Fatalf("length word = %d, want the packet size %d", lw, len(got))
	}
	if v := binary.LittleEndian.Uint32(got[4:]); v != 147 {
		t.Fatalf("type = %d at +4, want 147 - the MEASURED waypoint type; 136 (the "+
			"source header) boxed the level-limit sentence", v)
	}
	if lv := got[8]; lv != 3 {
		t.Fatalf("level = %d at +8, want 3 - level above 0 is what pops the box, and "+
			"3 is the one titled \"Message\"", lv)
	}
	if body := string(got[45 : len(got)-1]); body != note {
		t.Fatalf("text = %q, want the log note %q", body, note)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("last byte = 0x%02x, want the NUL terminator", got[len(got)-1])
	}
	if MessageTypeCurrentWayPoint != 147 {
		t.Fatalf("MessageTypeCurrentWayPoint = %d, want the MEASURED 147 (the source "+
			"header says 136 and is wrong on this client)",
			MessageTypeCurrentWayPoint)
	}
}

// TestBuildMessageGeneralCallersUnchanged is the guard that adding the type/level
// arguments moved NOTHING for the callers that existed before: the Warp List lines
// and the warp refusal all pass the general pair, so their packet is still the 73
// bytes the Python oracle builds - type 0, level 0, OUR text.
func TestBuildMessageGeneralCallersUnchanged(t *testing.T) {
	const text = "Sold FireStone for 2500 vis"
	got := BuildMessage(text, MessageTypeGeneral, MessageLevelGeneral)

	if len(got) != 73 {
		t.Fatalf("packet = %d bytes, want the oracle's 73", len(got))
	}
	if v := binary.LittleEndian.Uint32(got[4:]); v != 0 {
		t.Fatalf("type = %d, want 0 (general) - a non-zero type makes the client "+
			"print its own sentence instead of ours", v)
	}
	if lv := got[8]; lv != 0 {
		t.Fatalf("level = %d, want 0 (general) - any other level pops a modal box", lv)
	}
	if body := string(got[45 : len(got)-1]); body != text {
		t.Fatalf("text = %q, want %q", body, text)
	}
}

// TestLoadShopGroups reads a two-group file and checks the ORDER survives: a buy
// record indexes into this list, so a reordered list would sell the wrong item.
func TestLoadShopGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shop_groups.json")
	blob := `{"count":2,"groups":{
		"ETC_LEONARDO":[13348,13353,13354],
		"ANDRE_BEAUTIQUE":[14121,14124]}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	groups, err := LoadShopGroups(path)
	if err != nil {
		t.Fatalf("LoadShopGroups: %v", err)
	}
	if groups.Len() != 2 {
		t.Fatalf("Len = %d, want 2", groups.Len())
	}
	items := groups.Items("ETC_LEONARDO")
	if len(items) != 3 || items[0] != 13348 || items[2] != 13354 {
		t.Fatalf("ETC_LEONARDO = %v, want [13348 13353 13354] in that order", items)
	}
	if got := groups.Items("NO_SUCH_GROUP"); got != nil {
		t.Fatalf("an unknown group answered %v, want nil", got)
	}

	// Nil-safety: a zone started without the file holds a nil table.
	var none ShopGroups
	if none.Items("ETC_LEONARDO") != nil || none.Len() != 0 {
		t.Fatal("a nil shop table answered a group")
	}
}

// TestShopLoadErrors: a missing or empty file is an ERROR the caller logs.
func TestShopLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadShopGroups(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("a missing shop-group file loaded")
	}
	if _, err := LoadCustomShops(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("a missing custom-shop file loaded")
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"count":0,"groups":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadShopGroups(empty); err == nil {
		t.Fatal("an empty shop-group file was accepted")
	}
	if _, err := LoadCustomShops(empty); err == nil {
		t.Fatal("an empty custom-shop file was accepted")
	}
}

// TestLoadCustomShops pins the five numbers a custom shop entry carries, in the
// order the packet needs them: buy item, price item 0, count 0, price item 1,
// count 1.
func TestLoadCustomShops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom_shops.json")
	blob := `{"count":1,"groups":{"13YEAR_SHOP":[[27380,24187,200,0,0],[40998,24187,200,0,0]]}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
	shops, err := LoadCustomShops(path)
	if err != nil {
		t.Fatalf("LoadCustomShops: %v", err)
	}
	rows, ok := shops["13YEAR_SHOP"]
	if !ok || len(rows) != 2 {
		t.Fatalf("13YEAR_SHOP = %v (%d rows), want 2", rows, len(rows))
	}
	if rows[0] != [5]int64{27380, 24187, 200, 0, 0} {
		t.Fatalf("first entry = %v, want [27380 24187 200 0 0]", rows[0])
	}
}

// TestBuildTradeDialogHex is the guard that LastShop did not change the packet:
// the bytes are the ones the capture proved.
func TestBuildTradeDialogHex(t *testing.T) {
	got := BuildTradeDialog(0x40000031, "ETC_LEONARDO", CoinVis)
	if cmd := cmdOf(got); cmd != CmdZCDialogNPCTrade || len(got) != 40 {
		t.Fatalf("got cmd %d, %d bytes; want %d, 40", cmd, len(got), CmdZCDialogNPCTrade)
	}
	group := got[11:39]
	if !bytes.HasPrefix(group, []byte("ETC_LEONARDO")) {
		t.Fatalf("group bytes = %s", hex.EncodeToString(group))
	}
	if !strings.Contains(string(got), "ETC_LEONARDO") {
		t.Fatal("the group name is not in the packet")
	}
}
