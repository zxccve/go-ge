package zone

// The caption a town NPC's spawn frame carries (zone\npcs.go).
//
// The three pass conditions this file exists for, in its own words: (1) the built
// 3543's 65-byte name field holds the ROW'S ENGLISH NAME, NUL-padded, for a row
// that has one; (2) a row WITHOUT a name sends an empty field - no token, no class
// name - and says why once; (3) a name longer than the field is truncated on a
// UTF-8 boundary and the packet's length is still exactly 109 bytes.
//
// The rows are the SHIPPED data (`data\zone_npcs\map10.json`, Reboldoeux) and the
// words come from the SHIPPED dictionary, so a test cannot pass on an invented
// name. The expected strings are the ones the live v32 capture carries in this
// same field (captures\animref\20261028-221827, 42 records, see the lane note).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"geserver/text"
)

// npcNameTestBook loads the dictionaries the zone loads. The client's data folders
// are not in git, so a machine without them SKIPS rather than fails - but the
// resolution itself is never faked: every expected name below is asserted against
// the dictionary as well, so a book that answers nothing fails instead of skipping.
func npcNameTestBook(t *testing.T) *text.Book {
	t.Helper()
	book, err := text.Load(
		filepath.Join("..", "..", "data", "locale_id_map.json"),
		filepath.Join("..", "..", "..", "ge", "dictionary", "dictionary_local.xml"),
		// The pack sits in the same folder; Load's default path is relative to
		// goserver\, not to this package's directory.
		filepath.Join("..", "..", "..", "ge", "dictionary",
			"dictionary_local_serverpack.xml"),
	)
	if err != nil {
		t.Skipf("no dictionary on this machine (%v) - the caption tests need the "+
			"client's dictionary_local.xml", err)
	}
	return book
}

// npcByGenID loads one row of the shipped NPC data for a map.
func npcByGenID(t *testing.T, mapID int, genID uint32) NPC {
	t.Helper()
	doc, err := LoadNPCs(filepath.Join("..", "..", "data", "zone_npcs",
		fmt.Sprintf("map%d.json", mapID)))
	if err != nil {
		t.Skipf("no NPC data for map %d (%v) - build it with: cd src && "+
			"python emit_zone_npcs.py %d", mapID, err, mapID)
	}
	for _, npc := range doc.NPCs {
		if npc.GenID == genID {
			return npc
		}
	}
	t.Fatalf("map %d has no NPCGen row %d in the shipped data", mapID, genID)
	return NPC{}
}

// npcNameField cuts the 65-byte caption out of a built 3543 by the offsets the
// packet documents, so a moved field fails here rather than in game.
func npcNameField(t *testing.T, pkt []byte) []byte {
	t.Helper()
	if len(pkt) != trSize {
		t.Fatalf("the spawn is %d bytes, want %d (the packet is FIXED)", len(pkt), trSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCSpawnTrigger {
		t.Fatalf("cmd = %d, want %d (ZC_SPAWN_STANDINGTRIGGER)", cmd, CmdZCSpawnTrigger)
	}
	start := trAppearance + apTrName
	return pkt[start : start+npcNameLen]
}

// TestTheSpawnFrameCarriesTheRowsOwnEnglishName is pass condition 1, on a real row:
// Reboldoeux's Sir Lyndon (NPCGen row 2, Name `<$>10008848</>`).
func TestTheSpawnFrameCarriesTheRowsOwnEnglishName(t *testing.T) {
	book := npcNameTestBook(t)
	if got, _ := book.Dict.Get("10008848"); got != "Sir Lyndon" {
		t.Fatalf("the dictionary answers %q for <$>10008848</>, want \"Sir Lyndon\" "+
			"- this test is pinned to the game's own text", got)
	}

	npc := npcByGenID(t, 10, 2)
	if npc.NameToken != "<$>10008848</>" {
		t.Fatalf("map 10's NPCGen row 2 carries Name %q, want <$>10008848</>",
			npc.NameToken)
	}

	h := &Handler{Log: log.New(&bytes.Buffer{}, "", 0), Text: book}
	pkts := h.npcSpawnPackets([]NPC{npc})
	if len(pkts) != 1 {
		t.Fatalf("%d packet(s), want exactly 1", len(pkts))
	}

	field := npcNameField(t, pkts[0])
	const want = "Sir Lyndon"
	if got := string(field[:len(want)]); got != want {
		t.Fatalf("the caption field carries %q, want %q", got, want)
	}
	if field[len(want)] != 0 {
		t.Fatalf("the caption is not NUL-terminated: byte %d is 0x%02x",
			len(want), field[len(want)])
	}
	for i := len(want) + 1; i < len(field); i++ {
		if field[i] != 0 {
			t.Fatalf("the caption is not NUL-PADDED: byte %d of the field is 0x%02x",
				i, field[i])
		}
	}
	if strings.Contains(string(field), "<$>") {
		t.Fatalf("a raw locale token reached the wire: %q", field)
	}
	// The rest of the appearance block is untouched by naming.
	if got := binary.LittleEndian.Uint32(pkts[0][trAppearance+apTrGenID:]); got != npc.GenID {
		t.Errorf("genId = %d, want %d", got, npc.GenID)
	}
	if got := binary.LittleEndian.Uint32(pkts[0][trAppearance+apTrType:]); got != npc.NPCType {
		t.Errorf("type = %d, want %d", got, npc.NPCType)
	}
	if got := binary.LittleEndian.Uint32(pkts[0][trHandle:]); got != npc.Handle {
		t.Errorf("handle = 0x%08x, want 0x%08x", got, npc.Handle)
	}
}

// TestARowWithNoNameSendsAnEmptyFieldAndSaysWhy is pass condition 2, twice over:
// a REAL row whose NPCGen Name is the ies word "None" (Reboldoeux's NPCGen row 54,
// which the live capture also shows empty), and a token the dictionary does not
// know. Neither may fall back to a class name, a script name or the token itself.
func TestARowWithNoNameSendsAnEmptyFieldAndSaysWhy(t *testing.T) {
	book := npcNameTestBook(t)

	none := npcByGenID(t, 10, 54)
	if none.NameToken != "None" {
		t.Fatalf("map 10's NPCGen row 54 carries Name %q, want the ies word None",
			none.NameToken)
	}
	unknown := NPC{Handle: 0x400000fe, GenID: 9999, NPCType: 10011,
		ClassType: "npc_nosuchrow", NameToken: "<$>99999999</>"}
	both := []NPC{none, unknown}

	var logs bytes.Buffer
	h := &Handler{Log: log.New(&logs, "", 0), Text: book}
	pkts := h.npcSpawnPackets(both)
	if len(pkts) != 2 {
		t.Fatalf("%d packet(s), want 2", len(pkts))
	}

	for i, pkt := range pkts {
		field := npcNameField(t, pkt)
		for j, b := range field {
			if b != 0 {
				t.Fatalf("row %d sends %q in the caption field (byte %d), want EMPTY",
					i, field, j)
			}
		}
		if strings.Contains(string(field), both[i].ClassType) {
			t.Fatalf("row %d put its CLASS NAME (%q) in the caption field",
				i, both[i].ClassType)
		}
	}

	// One line per row, and the reason on it - not one per packet: a second player
	// entering the same world must not repeat it.
	if got := strings.Count(logs.String(), "carries NO caption"); got != 2 {
		t.Fatalf("%d \"carries NO caption\" line(s) for 2 rows, want 2:\n%s",
			got, logs.String())
	}
	if !strings.Contains(logs.String(), `Name is the ies word "None" (no name)`) {
		t.Errorf("the log does not name the None reason:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `the dictionary has no text for "<$>99999999</>"`) {
		t.Errorf("the log does not name the unresolvable token:\n%s", logs.String())
	}
	logs.Reset()
	h.npcSpawnPackets(both)
	if logs.Len() != 0 {
		t.Errorf("the same two rows wrote again on a second build (dropSayOnce "+
			"should keep a re-entering player silent):\n%s", logs.String())
	}
}

// TestALongNameIsTruncatedOnAUTF8BoundaryAndThePacketKeepsItsLength is pass
// condition 3: 64 bytes is the ceiling (a char[65] keeps its NUL), a multi-byte
// character straddling byte 64 is dropped whole, and the packet is STILL 109 bytes.
func TestALongNameIsTruncatedOnAUTF8BoundaryAndThePacketKeepsItsLength(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantLen int
	}{
		// 63 ASCII + "é" (2 bytes at 63..64) + "tail": the rune does not fit.
		{"a rune straddling the ceiling", strings.Repeat("A", 63) + "é" + "tail", 63},
		{"exactly 64 bytes", strings.Repeat("B", 64), 64},
		{"65 bytes", strings.Repeat("C", 65), 64},
		{"a rune ending exactly at 64", strings.Repeat("D", 62) + "é", 64},
		{"a rune starting exactly at 64", strings.Repeat("E", 64) + "é", 64},
	}
	for _, c := range cases {
		npc := NPC{Handle: 0x40000001, GenID: 7, NPCType: 10011, Name: c.text}
		pkt := BuildSpawnTrigger(npc)
		field := npcNameField(t, pkt)
		if got := len(field); got != 65 {
			t.Fatalf("%s: caption field is %d bytes, want 65", c.name, got)
		}
		if !utf8.Valid(field) {
			t.Fatalf("%s: the caption field is not valid UTF-8 after truncation: %q",
				c.name, field)
		}
		got := string(field[:c.wantLen])
		if got != c.text[:c.wantLen] {
			t.Fatalf("%s: caption = %q, want %q", c.name, got, c.text[:c.wantLen])
		}
		if field[c.wantLen] != 0 {
			t.Fatalf("%s: byte %d is 0x%02x, want the NUL", c.name, c.wantLen,
				field[c.wantLen])
		}
		if len(pkt) != trSize {
			t.Fatalf("%s: the packet is %d bytes, want the fixed %d", c.name,
				len(pkt), trSize)
		}
	}
}

// TestTheNameIsTheTodaysIdTextNotTheIDMapsRewrite guards the one row in the shipped
// data where the id map and the dictionary disagree: map 10's NPCGen row 1215
// carries `<$>184538</>`, the dictionary answers "Crack" and the LIVE WIRE says
// "Crack", while the id map points that id at a dialog line. Book.Resolve applies
// the id map first, so this test is what keeps the caption path off it.
func TestTheNameIsTheTodaysIdTextNotTheIDMapsRewrite(t *testing.T) {
	book := npcNameTestBook(t)
	if got, _ := book.Dict.Get("184538"); got != "Crack" {
		t.Fatalf("the dictionary answers %q for 184538, want \"Crack\"",
			got)
	}
	if got, ok := book.IDMap.Get("184538"); !ok {
		t.Skip("this build's id map no longer rewrites 184538 - the guard is moot")
	} else if text, _ := book.Dict.Get(got); !strings.Contains(text, "Kurt") {
		t.Skipf("the id map now points 184538 at %q, which no longer disagrees", text)
	}

	npc := npcByGenID(t, 10, 1215)
	h := &Handler{Log: log.New(&bytes.Buffer{}, "", 0), Text: book}
	pkts := h.npcSpawnPackets([]NPC{npc})
	field := npcNameField(t, pkts[0])
	if got := string(field[:len("Crack")]); got != "Crack" {
		t.Fatalf("the caption is %q, want \"Crack\": Book.Resolve would have sent "+
			"the id map's dialog line instead", got)
	}
	if !strings.HasPrefix(string(field), "Crack") || field[5] != 0 {
		t.Fatalf("the caption field is %q, want \"Crack\" + NUL", field)
	}
}

// TestEveryShippedTownNPCSendsNoToken is the data-health half: EVERY row of EVERY
// emitted map is built through the production path and no field may carry a token.
// It prints how many rows are named and how many are not, which is the number the
// lane note quotes.
func TestEveryShippedTownNPCSendsNoToken(t *testing.T) {
	book := npcNameTestBook(t)
	paths, err := filepath.Glob(filepath.Join("..", "..", "data", "zone_npcs", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Skipf("no emitted NPC data (%v)", err)
	}
	h := &Handler{Log: log.New(&bytes.Buffer{}, "", 0), Text: book}
	named, total := 0, 0
	for _, path := range paths {
		doc, err := LoadNPCs(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, pkt := range h.npcSpawnPackets(doc.NPCs) {
			total++
			field := npcNameField(t, pkt)
			if strings.Contains(string(field), "<$>") {
				t.Fatalf("%s: a token reached the wire: %q", path, field)
			}
			if field[0] != 0 {
				named++
			}
		}
	}
	t.Logf("captions: %d of %d shipped town NPC row(s) carry a name, %d have none",
		named, total, total-named)
	if named == 0 {
		t.Fatalf("no shipped row carries a name - the dictionary or the data is wrong")
	}
}
