package zone

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
	"geserver/text"
)

// pack builds a client packet for the parser tests: the security header is
// opaque here, so it is filled with markers that make a wrong offset obvious.
func pack(cmd uint16, body ...byte) []byte {
	out := []byte{byte(cmd), byte(cmd >> 8)}
	out = append(out, 1, 2, 3, 4, 5, 6, 7, 8) // security header: 10 bytes total
	return append(out, body...)
}

func TestParseClickTrigger(t *testing.T) {
	// charIndex 1, longdistance 0, handle 0x40000031 - the shape the live server
	// logs for a click (`handle=0x40000031`).
	pt := pack(CmdCZClickTrigger, 1, 0, 0x31, 0x00, 0x00, 0x40)

	idx, long, handle, ok := ParseClickTrigger(pt)
	if !ok {
		t.Fatal("a 16-byte CLICK_TRIGGER was rejected")
	}
	if idx != 1 || long || handle != 0x40000031 {
		t.Fatalf("got idx=%d long=%v handle=0x%08x, want 1/false/0x40000031",
			idx, long, handle)
	}

	// The long-distance flag is a byte at 11, not a field of its own.
	pt[11] = 1
	if _, long, _, _ = ParseClickTrigger(pt); !long {
		t.Fatal("longdistance byte at 11 was not read")
	}

	if _, _, _, ok := ParseClickTrigger(pt[:15]); ok {
		t.Fatal("a 15-byte packet was accepted - the handle would be read from a byte that is not there")
	}
}

func TestClickRange(t *testing.T) {
	npc := [2]int32{1000, 1000}
	cases := []struct {
		name string
		pc   [2]int32
		long bool
		want bool
	}{
		{"same cell", [2]int32{1000, 1000}, false, true},
		{"199 away", [2]int32{1199, 1000}, false, true},
		{"201 away", [2]int32{1201, 1000}, false, false},
		{"201 away, long reach", [2]int32{1201, 1000}, true, true},
		{"599 away, long reach", [2]int32{1599, 1000}, true, true},
		{"601 away, long reach", [2]int32{1601, 1000}, true, false},
		// 3-4-5: the distance is EUCLIDEAN, not the sum of the axes.
		{"140,140 away is 198", [2]int32{1140, 1140}, false, true},
	}
	for _, c := range cases {
		if got := InClickRange(c.pc, npc, c.long); got != c.want {
			t.Errorf("%s: InClickRange = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBuildDialogOK pins the byte layout: command(2) length(2) type(1)
// handle(4) text+NUL, text at 9, length = the WHOLE packet.
func TestBuildDialogOK(t *testing.T) {
	got := BuildDialogOK(0x40000000, "Hi", 1)
	want := mustHex(t, "4c0e"+"0c00"+"01"+"00000040"+"4869"+"00")
	if !bytes.Equal(got, want) {
		t.Fatalf("Z C_DIALOG_OK\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if int(got[2])|int(got[3])<<8 != len(got) {
		t.Fatalf("length field %d != packet size %d", got[2], len(got))
	}
}

// TestBuildDialogSelect pins the select layout: the FIRST string is the prompt
// and count is 1 + the number of options.
func TestBuildDialogSelect(t *testing.T) {
	got := BuildDialogSelect(0x40000000, "Q", []string{"A"}, 1)
	want := mustHex(t, "4f0e"+"0e00"+"01"+"02"+"00000040"+"5100"+"4100")
	if !bytes.Equal(got, want) {
		t.Fatalf("ZC_DIALOG_SELECT\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if got[5] != 2 {
		t.Fatalf("count byte = %d, want 2 (prompt + 1 option)", got[5])
	}
	// Three options must grow the count, not the header.
	if got := BuildDialogSelect(1, "p", []string{"a", "b", "c"}, 1); got[5] != 4 {
		t.Fatalf("count byte = %d with 3 options, want 4", got[5])
	}
}

func TestBuildVoiceAndClose(t *testing.T) {
	if got, want := BuildNPCClickVoice(0x40000000), mustHex(t, "bb0d"+"00000040"); !bytes.Equal(got, want) {
		t.Fatalf("voice = %s, want %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if got, want := BuildDialogClose(), mustHex(t, "510e"); !bytes.Equal(got, want) {
		t.Fatalf("close = %s, want %s - it is a bare command, 2 bytes", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

// TestBuildTradeDialog pins the 3680 layout, measured from the client's reader:
// isCashItem at +4, +5 unused, +6 must stay 1, the coin dword at +7, the group
// name at +11 (24 characters plus a NUL), sellCount 0 at +36, 40 bytes total.
func TestBuildTradeDialog(t *testing.T) {
	got := BuildTradeDialog(0x40000000, "ETC_LEONARDO", CoinVis)
	if len(got) != 40 {
		t.Fatalf("%d bytes, want 40", len(got))
	}
	if cmd := cmdOf(got); cmd != CmdZCDialogNPCTrade {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCDialogNPCTrade)
	}
	if n := binary.LittleEndian.Uint16(got[2:]); n != 40 {
		t.Fatalf("length = %d, want 40", n)
	}
	// A vis window: isCashItem=0 at +4, +5 unused, +6 must stay 1, coin dword 0.
	wantPrefix := mustHex(t, "00000100000000")
	if !bytes.Equal(got[4:11], wantPrefix) {
		t.Fatalf("prefix = %s, want %s", hex.EncodeToString(got[4:11]), hex.EncodeToString(wantPrefix))
	}
	// group name "ETC_LEONARDO" + NUL at offset 11
	wantGroup := mustHex(t, "4554435f4c454f4e4152444f00")
	if !bytes.Equal(got[11:11+len(wantGroup)], wantGroup) {
		t.Fatalf("group = %s, want %s", hex.EncodeToString(got[11:24]), hex.EncodeToString(wantGroup))
	}
	// everything from end of group+NUL through byte 39 is zero
	for i := 11 + len(wantGroup); i < 40; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d = %d, want 0", i, got[i])
		}
	}
}

// TestBuildTradeDialogCoin is the difference between a vis shop and a Feso shop
// on the wire: the coin dword at +7, AND the shop-mode byte at +6, which is not a
// constant. An NPC shop window (Leonardo) carries 0x01 and the Feso window the
// client's own toolbar opens carries 0x00; the client passes that byte on as the
// 5th argument of its shop-mode call and never compares it to 1, so both work.
func TestBuildTradeDialogCoin(t *testing.T) {
	vis := BuildTradeDialog(0x40000031, "FESO_ITEMSHOP", CoinVis)
	feso := BuildTradeDialog(0x40000031, "FESO_ITEMSHOP", CoinFeso)
	if len(vis) != 40 || len(feso) != 40 {
		t.Fatalf("%d and %d bytes, want 40 and 40", len(vis), len(feso))
	}
	if w := binary.LittleEndian.Uint32(feso[7:]); w != 1 {
		t.Fatalf("coin dword = %d, want 1 (SHOP_FESO)", w)
	}
	if w := binary.LittleEndian.Uint32(vis[7:]); w != 0 {
		t.Fatalf("vis coin dword = %d, want 0 (SHOP_VIS)", w)
	}
	// +6 is the shop mode: 0x01 for vis (what has always worked in game), 0x00 for
	// Feso (what the retail server sent). +4 is isCashItem, 0 for both.
	if vis[6] != 0x01 {
		t.Fatalf("a vis window's +6 = %d, want 1", vis[6])
	}
	if feso[6] != 0x00 {
		t.Fatalf("a Feso window's +6 = %d, want 0", feso[6])
	}
	if feso[4] != 0 {
		t.Fatalf("isCashItem = %d, want 0", feso[4])
	}
	if s := binary.LittleEndian.Uint32(feso[36:]); s != 0 {
		t.Fatalf("sellCount = %d, want 0 (the client draws the list itself)", s)
	}
	// Prove nothing ELSE drifted with the coin: the only bytes that may differ are
	// the shop mode at +6 and the coin dword at +7..+10.
	for i := 0; i < 40; i++ {
		if i == 6 || (i >= 7 && i < 11) {
			continue
		}
		if vis[i] != feso[i] {
			t.Fatalf("byte %d differs between a vis and a Feso window: %d vs %d",
				i, vis[i], feso[i])
		}
	}
}

// TestBuildTradeDialogNameLimit: the client copies 25 bytes from +11, so a longer
// group name must be CUT to 24 characters and still end in a NUL - an unterminated
// name runs on into sellCount and the client draws a torn string.
func TestBuildTradeDialogNameLimit(t *testing.T) {
	const long = "FESO_ITEMSHOP_MATERIAL_TOO_LONG" // 31 characters
	if len(long) <= TradeGroupNameLimit {
		t.Fatalf("the test name is %d characters, it must exceed the limit %d",
			len(long), TradeGroupNameLimit)
	}
	got := BuildTradeDialog(0x40000031, long, CoinVis)
	if want := long[:TradeGroupNameLimit]; string(got[11:11+TradeGroupNameLimit]) != want {
		t.Fatalf("name = %q, want the first %d characters %q",
			string(got[11:11+TradeGroupNameLimit]), TradeGroupNameLimit, want)
	}
	if got[11+TradeGroupNameLimit] != 0 {
		t.Fatalf("byte %d = %d, want the NUL terminator", 11+TradeGroupNameLimit,
			got[11+TradeGroupNameLimit])
	}
	if s := binary.LittleEndian.Uint32(got[36:]); s != 0 {
		t.Fatalf("sellCount = %d, want 0: the name ran into it", s)
	}
	// A name that fits keeps its own trailing NUL, and the rest stays zero.
	fit := BuildTradeDialog(0x40000031, "FESO_ITEMSHOP_POT", CoinFeso)
	if name := "FESO_ITEMSHOP_POT"; string(fit[11:11+len(name)]) != name || fit[11+len(name)] != 0 {
		t.Fatalf("name = %q, want %q followed by a NUL", string(fit[11:35]), name)
	}
}

// TestBuildCustomTradeDialog pins the 3681 layout: 80-byte header plus one
// 33-byte record, with the record wire order
// (BuyItem, PriceItem[0], PriceItem[1], Count[0], Count[1], 13 pad).
func TestBuildCustomTradeDialog(t *testing.T) {
	entries := []CustomShopEntry{
		{BuyItem: 0x12345678, PriceItem0: 0x11, PriceItem1: 0x22, Count0: 0x33, Count1: 0x44},
	}
	got := BuildCustomTradeDialog(0x40000000, entries)
	wantLen := 80 + 33
	if len(got) != wantLen {
		t.Fatalf("%d bytes, want %d", len(got), wantLen)
	}
	if cmd := cmdOf(got); cmd != CmdZCDialogNPCCustomTrade {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCDialogNPCCustomTrade)
	}
	// The length word is the TOTAL packet length, header included. The Python
	// oracle writes 4+76+33*n, which is the same number; writing 4+80+33*n is
	// four bytes too long and that is what this assertion exists to stop.
	if n := binary.LittleEndian.Uint16(got[2:]); n != 80+33 {
		t.Fatalf("length = %d, want %d", n, 80+33)
	}
	if n := binary.LittleEndian.Uint16(got[2:]); int(n) != len(got) {
		t.Fatalf("length word = %d but the packet is %d bytes - the word is the TOTAL",
			n, len(got))
	}
	if v := binary.LittleEndian.Uint32(got[4:]); v != 0 {
		t.Fatalf("normalTradeType = %d, want 0", v)
	}
	if v := binary.LittleEndian.Uint32(got[8:]); v != 1 {
		t.Fatalf("sellCount = %d, want 1", v)
	}
	if v := binary.LittleEndian.Uint32(got[12:]); v != 0 {
		t.Fatalf("CustomShopType = %d, want 0", v)
	}
	// PropName at 16..80: all zero bytes
	for i := 16; i < 80; i++ {
		if got[i] != 0 {
			t.Fatalf("PropName byte %d = %d, want 0", i, got[i])
		}
	}
	// the single record at offset 80
	rec := got[80 : 80+33]
	if v := binary.LittleEndian.Uint32(rec[0:]); v != 0x12345678 {
		t.Fatalf("BuyItem = %x, want 0x12345678", v)
	}
	if v := binary.LittleEndian.Uint32(rec[4:]); v != 0x11 {
		t.Fatalf("PriceItem[0] = %x, want 0x11", v)
	}
	if v := binary.LittleEndian.Uint32(rec[8:]); v != 0x22 {
		t.Fatalf("PriceItem[1] = %x, want 0x22", v)
	}
	if v := binary.LittleEndian.Uint32(rec[12:]); v != 0x33 {
		t.Fatalf("Count[0] = %x, want 0x33", v)
	}
	if v := binary.LittleEndian.Uint32(rec[16:]); v != 0x44 {
		t.Fatalf("Count[1] = %x, want 0x44", v)
	}
	for i := 20; i < 33; i++ {
		if rec[i] != 0 {
			t.Fatalf("record pad byte %d = %d, want 0", i, rec[i])
		}
	}
}

// TestBuildCustomTradeDialogStride is the number a live reference capture proved:
// CLAUD's window was 773 bytes = 80 + 21*33, first record 7201/21450. A 20-byte
// stride (source2's) is what crashed the client, because every record after the
// first landed 13 bytes away from where it was read.
func TestBuildCustomTradeDialogStride(t *testing.T) {
	const rows = 21 // CLAUD's row count in data\custom_shops.json
	entries := make([]CustomShopEntry, rows)
	for i := range entries {
		entries[i] = CustomShopEntry{BuyItem: uint32(7201 + i), PriceItem0: 21450, Count0: 1}
	}
	got := BuildCustomTradeDialog(0x40000000, entries)
	if len(got) != 773 {
		t.Fatalf("%d bytes, want 773 (80 + 21*33)", len(got))
	}
	if n := binary.LittleEndian.Uint16(got[2:]); int(n) != len(got) {
		t.Fatalf("length word = %d, want the packet length %d", n, len(got))
	}
	if v := binary.LittleEndian.Uint32(got[8:]); v != rows {
		t.Fatalf("sellCount = %d, want %d", v, rows)
	}
	// Every record sits 33 bytes after the last, so a wrong stride shows up on the
	// LAST row as well as the first.
	for i, e := range entries {
		off := 80 + i*33
		if v := binary.LittleEndian.Uint32(got[off:]); v != e.BuyItem {
			t.Fatalf("record %d BuyItem = %d, want %d (stride is 33, not 20)", i, v, e.BuyItem)
		}
	}
}

// TestLatin1StopShift is the whole reason latin1 exists: one character outside
// single-byte range must become ONE '?' and not shift every byte after it.
func TestLatin1StopShift(t *testing.T) {
	if got := latin1("a\u1234b"); string(got) != "a?b" {
		t.Fatalf("latin1 = %q, want %q", got, "a?b")
	}
}

// TestDialogCarriesTheScriptsTextUnchanged: the client resolves its own
// `<$>N</>` tokens and parses its own `<?>` / `<|>` / `</?>` markup, so both
// builders must copy the script's bytes and change NOTHING - not the token, not
// the markup, not a literal CP949 line.
func TestDialogCarriesTheScriptsTextUnchanged(t *testing.T) {
	// The live server's own select line, byte for byte:
	// captures\exitroom2\20261024-195918\s2c_7061_49726.bin @69071.
	const markup = "<?><$>10138965</><|><$>10138960</><|>500</?>;" +
		"<$>10102683</>;<$>10138963</>;<$>10138964</>;"
	const token = "<$>10193351</>"
	// A LITERAL line: the corpus is CP949, and these are the first two syllables
	// of the one non-ASCII dialog line in it (util_define.scp). latin1() would
	// turn each byte into '?', which is the bug this pins.
	korean := string([]byte{0xB0, 0xA1, 0xB3, 0xAA})

	for _, want := range []string{token, markup, korean} {
		pkt := BuildDialogOK(0x40000000, want, 1)
		if got := string(pkt[9 : len(pkt)-1]); got != want {
			t.Fatalf("DIALOG_OK text = %q, want %q (the builder rewrote it)", got, want)
		}
		if pkt[len(pkt)-1] != 0 {
			t.Fatal("DIALOG_OK lost its NUL terminator")
		}
	}

	pkt := BuildDialogSelect(0x40000000, markup, []string{token, korean, markup}, 1)
	body := pkt[4:]
	if body[1] != 4 {
		t.Fatalf("count byte = %d, want 4 (prompt + 3 options)", body[1])
	}
	parts := bytes.Split(body[6:], []byte{0})
	want := []string{markup, token, korean, markup}
	for i, w := range want {
		if string(parts[i]) != w {
			t.Fatalf("DIALOG_SELECT string %d = %q, want %q", i, parts[i], w)
		}
	}
}

// TestClickDialogDoesNotResolveTheToken is the ruling in a test: the dictionary
// is wired into the handler and WOULD answer this exact id, and the packet still
// has to carry the script's token. A dialog is the client's text, not ours.
func TestClickDialogDoesNotResolveTheToken(t *testing.T) {
	const token = "<$>10193351</>"
	book := &text.Book{Dict: text.NewTable(map[string]string{"10193351": "Pay your taxes."})}
	if got := book.Resolve(token); got != "Pay your taxes." {
		t.Fatalf("the fixture is wrong: the book answers %q, so this test proves nothing", got)
	}

	npc := NPC{Handle: 0x40000031, X: 1000, Y: 1000, Cell: 1,
		ClassType: "REB_SERM", Script: "NPC_TAXMAN"}
	clickAt := &Session{Pos: [3]int32{1000, 1000, 1}}
	clickAt.Team = []Character{{Handle: 7, Name: "Tester", Level: 5}}

	for _, reply := range []ClickReply{
		{DialogKind: ClickDialogOK, Text: token, Fade: 1},
		{DialogKind: ClickDialogSelect, Prompt: token,
			Options: []string{"<$>10138960</>", token}, Fade: 1},
	} {
		h := &Handler{
			Log:  log.New(io.Discard, "", 0),
			Text: book,
			NPCs: []NPC{npc},
			Click: func(string, ClickObject, ClickObject) (ClickReply, error) {
				return reply, nil
			},
		}
		out, err := h.clickTrigger(clickAt, frameClick(0, 0x40000031))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) == 0 {
			t.Fatal("the click sent nothing")
		}
		if !bytes.Contains(out[0], []byte(token)) {
			t.Fatalf("%s: the packet does not carry the token %q: %q",
				reply.DialogKind, token, out[0])
		}
		if bytes.Contains(out[0], []byte("Pay your taxes.")) {
			t.Fatalf("%s: the packet carries the DICTIONARY's words - the dialog was resolved",
				reply.DialogKind)
		}
	}
}

// TestClickTriggerHandler is the behaviour: a click within reach runs the NPC's
// script and sends its dialog plus the click voice; a click that is too far, or
// on an NPC that is not ours, sends NOTHING.
func TestClickTriggerHandler(t *testing.T) {
	npc := NPC{Handle: 0x40000031, X: 1000, Y: 1000, Cell: 1,
		ClassType: "REB_SERM", Script: "NPC_TAXMAN"}
	newHandler := func(reply ClickReply) (*Handler, *[]string) {
		calls := &[]string{}
		h := &Handler{
			Log:  log.New(io.Discard, "", 0),
			NPCs: []NPC{npc},
			Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
				*calls = append(*calls, entry)
				if self.Script != "NPC_TAXMAN" || self.Handle != npc.Handle {
					t.Errorf("self bag = %+v, want the clicked NPC", self)
				}
				return reply, nil
			},
		}
		return h, calls
	}
	clickAt := func(h *Handler, x int32) *Session {
		s := &Session{Pos: [3]int32{x, 1000, 1}}
		s.Team = []Character{{Handle: 7, Name: "Tester", Level: 5}}
		return s
	}

	t.Run("in reach sends dialog and voice", func(t *testing.T) {
		h, calls := newHandler(ClickReply{DialogKind: ClickDialogOK, Text: "Pay your taxes.", Fade: 1})
		out, err := h.clickTrigger(clickAt(h, 1100), frameClick(0, 0x40000031))
		if err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 || (*calls)[0] != "SCR_CLICK_NPC_TAXMAN" {
			t.Fatalf("script calls = %v, want [SCR_CLICK_NPC_TAXMAN]", *calls)
		}
		if len(out) != 2 {
			t.Fatalf("%d packet(s), want 2 (dialog + click voice)", len(out))
		}
		if cmd := cmdOf(out[0]); cmd != CmdZCDialogOK {
			t.Fatalf("first packet cmd = %d, want %d", cmd, CmdZCDialogOK)
		}
		if !strings.Contains(string(out[0]), "Pay your taxes.") {
			t.Fatal("the dialog text is not in the packet")
		}
		if cmd := cmdOf(out[1]); cmd != CmdZCNPCClickVox {
			t.Fatalf("second packet cmd = %d, want the click voice", cmd)
		}
	})

	t.Run("select sends the choices", func(t *testing.T) {
		h, _ := newHandler(ClickReply{DialogKind: ClickDialogSelect, Prompt: "Which?",
			Options: []string{"Yes", "No"}, Fade: 1})
		out, _ := h.clickTrigger(clickAt(h, 1000), frameClick(0, 0x40000031))
		if len(out) != 2 || cmdOf(out[0]) != CmdZCDialogSel {
			t.Fatalf("want a DIALOG_SELECT first, got %d packet(s)", len(out))
		}
		if out[0][5] != 3 {
			t.Fatalf("count byte = %d, want 3 (prompt + 2 options)", out[0][5])
		}
	})

	// A normal shop is the ONE thing a later buy needs from this side: the buy
	// record names its item by INDEX in the list, so the session has to remember
	// which list the client is looking at.
	t.Run("a normal shop is remembered for a buy", func(t *testing.T) {
		h, _ := newHandler(ClickReply{WindowKind: WindowShop, WindowGroup: "ETC_LEONARDO"})
		h.ShopGroups = ShopGroups{"ETC_LEONARDO": {13348, 13353, 13354}}
		sess := clickAt(h, 1000)

		out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
			t.Fatalf("%d packet(s), want only the %d trade window", len(out), CmdZCDialogNPCTrade)
		}
		if sess.LastShop == nil {
			t.Fatal("the shop window was sent but LastShop is nil - a buy cannot be resolved")
		}
		if sess.LastShop.Group != "ETC_LEONARDO" || sess.LastShop.Kind != WindowShop {
			t.Fatalf("LastShop = %+v, want group ETC_LEONARDO and kind %q",
				sess.LastShop, WindowShop)
		}
		want := []int64{13348, 13353, 13354}
		if len(sess.LastShop.Items) != len(want) {
			t.Fatalf("LastShop.Items = %v, want %v", sess.LastShop.Items, want)
		}
		for i, id := range want {
			if sess.LastShop.Items[i] != id {
				t.Fatalf("LastShop.Items = %v, want %v (order decides the index)",
					sess.LastShop.Items, want)
			}
		}
	})

	// A shop table that was never loaded must not stop the window opening: the
	// client still has to see the shop, it just cannot be bought from yet.
	t.Run("a shop with no group data still opens", func(t *testing.T) {
		h, _ := newHandler(ClickReply{WindowKind: WindowShop, WindowGroup: "UNLOADED"})
		sess := clickAt(h, 1000)
		out, _ := h.clickTrigger(sess, frameClick(0, 0x40000031))
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
			t.Fatalf("a shop with no group data did not open: %d packet(s)", len(out))
		}
		if sess.LastShop == nil || len(sess.LastShop.Items) != 0 {
			t.Fatalf("LastShop = %+v, want the group with no items", sess.LastShop)
		}
	})

	// A CUSTOM shop window is drawn from OUR table, not from the group name: the
	// packet carries every row and its prices, and the buy index counts in the
	// order those rows were sent.
	t.Run("a custom shop window is sent and remembered", func(t *testing.T) {
		h, _ := newHandler(ClickReply{WindowKind: WindowShopCustom, WindowGroup: "CLAUD"})
		h.CustomShops = CustomShops{"CLAUD": {
			{7201, 21450, 1, 0, 0},
			{7202, 21450, 2, 0, 0},
		}}
		sess := clickAt(h, 1000)

		out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCCustomTrade {
			t.Fatalf("%d packet(s), want only the %d custom trade window",
				len(out), CmdZCDialogNPCCustomTrade)
		}
		if got, want := len(out[0]), 80+2*33; got != want {
			t.Fatalf("window = %d bytes, want %d (80 + 2*33)", got, want)
		}
		if n := binary.LittleEndian.Uint16(out[0][2:]); int(n) != len(out[0]) {
			t.Fatalf("length word = %d, want the packet length %d", n, len(out[0]))
		}
		if sess.LastShop == nil {
			t.Fatal("the custom window was sent but LastShop is nil - a barter cannot be resolved")
		}
		if sess.LastShop.Kind != WindowShopCustom || sess.LastShop.Group != "CLAUD" {
			t.Fatalf("LastShop = %+v, want a custom CLAUD window", sess.LastShop)
		}
		// The ROW ORDER is the index a buy uses, so it must survive untouched and
		// the record fields must come from the right columns, not from position.
		want := []CustomShopEntry{
			{BuyItem: 7201, PriceItem0: 21450, Count0: 1},
			{BuyItem: 7202, PriceItem0: 21450, Count0: 2},
		}
		if len(sess.LastShop.Entries) != len(want) {
			t.Fatalf("LastShop.Entries = %+v, want %+v", sess.LastShop.Entries, want)
		}
		for i, e := range want {
			if sess.LastShop.Entries[i] != e {
				t.Fatalf("LastShop.Entries = %+v, want %+v", sess.LastShop.Entries, want)
			}
		}
	})

	// A FESO shop is the same 3680 window with the coin dword set to 1, and the
	// session has to remember which coin it charges: the buy confirm does not
	// carry the coin (shopType is 0 for both), so a window that is not remembered
	// would make every Feso purchase unpriceable.
	t.Run("a Feso shop is sent with its coin and remembered", func(t *testing.T) {
		h, _ := newHandler(ClickReply{WindowKind: WindowShop,
			WindowGroup: "FESO_ITEMSHOP_POT", WindowCoin: CoinFeso})
		h.ShopGroups = ShopGroups{"FESO_ITEMSHOP_POT": {60081}}
		sess := clickAt(h, 1000)

		out, err := h.clickTrigger(sess, frameClick(0, 0x40000031))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade || len(out[0]) != 40 {
			t.Fatalf("want one 40-byte %d window, got %d packet(s)",
				CmdZCDialogNPCTrade, len(out))
		}
		if w := binary.LittleEndian.Uint32(out[0][7:]); w != uint32(CoinFeso) {
			t.Fatalf("coin dword = %d, want %d (SHOP_FESO)", w, CoinFeso)
		}
		if sess.LastShop == nil || sess.LastShop.Coin != CoinFeso ||
			sess.LastShop.Group != "FESO_ITEMSHOP_POT" {
			t.Fatalf("LastShop = %+v, want a Feso FESO_ITEMSHOP_POT window", sess.LastShop)
		}
	})

	t.Run("out of reach sends nothing", func(t *testing.T) {
		h, calls := newHandler(ClickReply{DialogKind: ClickDialogOK, Text: "never"})
		out, _ := h.clickTrigger(clickAt(h, 2000), frameClick(0, 0x40000031))
		if len(out) != 0 {
			t.Fatalf("%d packet(s) for a click 1000 units away, want 0", len(out))
		}
		if len(*calls) != 0 {
			t.Fatal("the script ran for a click that is out of reach")
		}
	})

	t.Run("unknown handle sends nothing", func(t *testing.T) {
		h, calls := newHandler(ClickReply{DialogKind: ClickDialogOK, Text: "never"})
		out, _ := h.clickTrigger(clickAt(h, 1000), frameClick(0, 0x40009999))
		if len(out) != 0 || len(*calls) != 0 {
			t.Fatal("a click on a handle we never spawned was answered")
		}
	})

	t.Run("no script engine drops the click", func(t *testing.T) {
		h, _ := newHandler(ClickReply{})
		h.Click = nil
		out, _ := h.clickTrigger(clickAt(h, 1000), frameClick(0, 0x40000031))
		if len(out) != 0 {
			t.Fatal("a click was answered with no script engine wired")
		}
	})
}

// frameClick is a client frame carrying a click on handle.
func frameClick(charIndex int, handle uint32) protocol.Frame {
	pt := pack(CmdCZClickTrigger, byte(charIndex), 0,
		byte(handle), byte(handle>>8), byte(handle>>16), byte(handle>>24))
	return protocol.Frame{Cmd: CmdCZClickTrigger, Size: len(pt), Data: pt}
}

// frameDialogAnswer is CZ_DIALOG_SELECT / CZ_ACK_DIALOG: the command, the
// 10-byte security header, then the character index and the chosen value.
func frameDialogAnswer(cmd uint16, value byte) protocol.Frame {
	pt := pack(cmd, 0, value)
	return protocol.Frame{Cmd: cmd, Size: len(pt), Data: pt}
}

// TestEndDialog is the escape hatch: the client is MODAL while a box is open, and
// a player left staring at an NPC with no way out is what happens without this.
func TestEndDialog(t *testing.T) {
	newHandler := func() *Handler {
		return &Handler{Log: log.New(io.Discard, "", 0)}
	}

	t.Run("a picked option closes the box", func(t *testing.T) {
		h := newHandler()
		sess := &Session{DialogHandle: 0x40000031}
		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZDialogSelect, 2))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 {
			t.Fatalf("%d packet(s), want 1 (the close)", len(out))
		}
		if cmdOf(out[0]) != CmdZCDialogClose || len(out[0]) != 2 {
			t.Fatalf("got cmd %d, %d bytes; want the bare %d, 2 bytes",
				cmdOf(out[0]), len(out[0]), CmdZCDialogClose)
		}
		if sess.DialogHandle != 0 {
			t.Fatalf("DialogHandle = 0x%08x after the close, want 0", sess.DialogHandle)
		}
	})

	t.Run("OK closes the box too", func(t *testing.T) {
		h := newHandler()
		sess := &Session{DialogHandle: 0x40000031}
		out, _ := h.endDialog(sess, frameDialogAnswer(CmdCZAckDialog, 0))
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogClose {
			t.Fatal("an ACK did not close the dialog")
		}
	})

	t.Run("an answer with no dialog still closes", func(t *testing.T) {
		// The bookkeeping and the client can disagree (a reloaded server). A
		// stray close is harmless; a client stuck in modal mode is not.
		h := newHandler()
		out, _ := h.endDialog(&Session{}, frameDialogAnswer(CmdCZAckDialog, 0))
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogClose {
			t.Fatal("a stray answer did not send the close")
		}
	})

	t.Run("a short answer sends nothing", func(t *testing.T) {
		h := newHandler()
		pt := pack(CmdCZDialogSelect)
		out, _ := h.endDialog(&Session{}, protocol.Frame{Cmd: CmdCZDialogSelect, Data: pt})
		if len(out) != 0 {
			t.Fatal("a 10-byte answer was answered")
		}
	})

	// A resumed custom shop whose group has no rows loaded has no window to send,
	// but the client is MODAL from the box that was just answered. Zero packets
	// leaves the player unable to move, so the close must go out on its own.
	t.Run("a resumed window we cannot draw still closes", func(t *testing.T) {
		h := newHandler()
		h.ResumeClick = func(int) (ClickReply, bool, error) {
			return ClickReply{WindowKind: WindowShopCustom, WindowGroup: "CUSTOM_A"}, false, nil
		}
		sess := &Session{DialogHandle: 0x40000031,
			Suspended: &suspendedClick{entry: "SCR_CLICK_TEST", handle: 0x40000031}}

		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZAckDialog, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) == 0 {
			t.Fatal("a resumed custom window sent NOTHING - the client stays modal")
		}
		if cmdOf(out[0]) != CmdZCDialogClose {
			t.Fatalf("first packet cmd = %d, want the close (%d)", cmdOf(out[0]), CmdZCDialogClose)
		}
		if sess.DialogHandle != 0 || sess.Suspended != nil {
			t.Fatalf("DialogHandle = 0x%08x, Suspended = %v; want both cleared",
				sess.DialogHandle, sess.Suspended)
		}
	})
	// The RESUME path sends a shop too (a script that showed a dialog first), and
	// it must remember the shop the same way - a buy after a menu choice is the
	// normal case, not the exception.
	t.Run("a resumed shop is remembered", func(t *testing.T) {
		h := newHandler()
		h.ShopGroups = ShopGroups{"ANDRE_BEAUTIQUE": {14121, 14124}}
		h.ResumeClick = func(int) (ClickReply, bool, error) {
			return ClickReply{WindowKind: WindowShop, WindowGroup: "ANDRE_BEAUTIQUE"}, false, nil
		}
		sess := &Session{DialogHandle: 0x40000031,
			Suspended: &suspendedClick{entry: "SCR_CLICK_TEST", handle: 0x40000031}}

		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZDialogSelect, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
			t.Fatalf("%d packet(s), want only the trade window", len(out))
		}
		if sess.LastShop == nil || sess.LastShop.Group != "ANDRE_BEAUTIQUE" ||
			len(sess.LastShop.Items) != 2 || sess.LastShop.Items[1] != 14124 {
			t.Fatalf("LastShop = %+v, want ANDRE_BEAUTIQUE with [14121 14124]", sess.LastShop)
		}
		if sess.DialogHandle != 0x40000031 {
			t.Fatalf("DialogHandle = 0x%08x, want the shop's handle kept", sess.DialogHandle)
		}
	})

	// The RESUMED branch is the one a MENU NPC uses - the window comes after the
	// player picks an option - so it must draw and remember a custom shop too.
	t.Run("a resumed custom shop is drawn and remembered", func(t *testing.T) {
		h := newHandler()
		h.CustomShops = CustomShops{"CLAUD": {{7201, 21450, 1, 0, 0}}}
		h.ResumeClick = func(int) (ClickReply, bool, error) {
			return ClickReply{WindowKind: WindowShopCustom, WindowGroup: "CLAUD"}, false, nil
		}
		sess := &Session{DialogHandle: 0x40000031,
			Suspended: &suspendedClick{entry: "SCR_CLICK_TEST", handle: 0x40000031}}

		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZDialogSelect, 3))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCCustomTrade {
			t.Fatalf("%d packet(s), want only the custom trade window", len(out))
		}
		if n := binary.LittleEndian.Uint16(out[0][2:]); int(n) != len(out[0]) {
			t.Fatalf("length word = %d, want the packet length %d", n, len(out[0]))
		}
		if sess.LastShop == nil || sess.LastShop.Kind != WindowShopCustom ||
			len(sess.LastShop.Entries) != 1 || sess.LastShop.Entries[0].BuyItem != 7201 {
			t.Fatalf("LastShop = %+v, want the one-row CLAUD window", sess.LastShop)
		}
		if sess.DialogHandle != 0x40000031 {
			t.Fatalf("DialogHandle = 0x%08x, want the window's handle kept", sess.DialogHandle)
		}
	})

	// A menu NPC that opens a FESO shop on an option must reach the same place: the
	// coin travels with the group, so the resumed branch remembers it too.
	t.Run("a resumed Feso shop is remembered with its coin", func(t *testing.T) {
		h := newHandler()
		h.ShopGroups = ShopGroups{"FESO_ITEMSHOP_POT": {60081}}
		h.ResumeClick = func(int) (ClickReply, bool, error) {
			return ClickReply{WindowKind: WindowShop, WindowGroup: "FESO_ITEMSHOP_POT",
				WindowCoin: CoinFeso}, false, nil
		}
		sess := &Session{DialogHandle: 0x40000031,
			Suspended: &suspendedClick{entry: "SCR_CLICK_TEST", handle: 0x40000031}}

		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZDialogSelect, 2))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
			t.Fatalf("%d packet(s), want only the trade window", len(out))
		}
		if w := binary.LittleEndian.Uint32(out[0][7:]); w != uint32(CoinFeso) {
			t.Fatalf("coin dword = %d, want %d (SHOP_FESO)", w, CoinFeso)
		}
		if sess.LastShop == nil || sess.LastShop.Coin != CoinFeso {
			t.Fatalf("LastShop = %+v, want the Feso coin remembered", sess.LastShop)
		}
	})
	// A click on another NPC REPLACES a suspended one in the script host, so a
	// session can still believe a dialog is open when the host has nothing to
	// resume. Resuming then fails with "no suspended click"; the session must clear
	// its own flag and close the box, or the next answer reports a false failure.
	t.Run("a suspension the host no longer holds is cleared", func(t *testing.T) {
		h := newHandler()
		h.ResumeClick = func(int) (ClickReply, bool, error) {
			return ClickReply{}, false, errors.New("no suspended click to resume")
		}
		sess := &Session{DialogHandle: 0x40000031,
			Suspended: &suspendedClick{entry: "SCR_CLICK_TEST", handle: 0x40000031}}

		out, err := h.endDialog(sess, frameDialogAnswer(CmdCZAckDialog, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCDialogClose {
			t.Fatalf("%d packet(s), want the single close: the client is still modal", len(out))
		}
		if sess.DialogHandle != 0 || sess.Suspended != nil {
			t.Fatalf("DialogHandle = 0x%08x, Suspended = %v; want both cleared",
				sess.DialogHandle, sess.Suspended)
		}
	})
}

func cmdOf(packet []byte) uint16 { return uint16(packet[0]) | uint16(packet[1])<<8 }

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex in the test itself: %v", err)
	}
	return b
}
