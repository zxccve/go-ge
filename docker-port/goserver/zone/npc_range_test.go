package zone

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// loadTownNPCs reads one emitted map file, skipping when the data is absent.
func loadTownNPCs(t *testing.T, name string) *NPCFile {
	t.Helper()
	dir := os.Getenv("GE_NPCS_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "data", "zone_npcs")
	}
	doc, err := LoadNPCs(filepath.Join(dir, name))
	if err != nil {
		t.Skipf("no emitted NPC data (%v) - build it with: cd src && python emit_zone_npcs.py", err)
	}
	return doc
}

// TestEmittedNPCCarriesRange pins the emitter: every row carries `range`, and a
// named real row carries the table's own value.
//
// The column is `datatable_npc.Range` - the radius of the NPC's ENTER/LEAVE
// trigger area (see the NPC.Range comment for why it is not a click reach).
// map 10's ETC_Board (the marketplace board) is Range=200 in the shipped table,
// so a wrong column order in the emitter shows up here rather than in game.
func TestEmittedNPCCarriesRange(t *testing.T) {
	doc := loadTownNPCs(t, "map10.json")

	// Presence, not just a zero: an absent key decodes to 0 and would hide a
	// column the emitter forgot to write.
	dir := os.Getenv("GE_NPCS_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "data", "zone_npcs")
	}
	blob, err := os.ReadFile(filepath.Join(dir, "map10.json"))
	if err != nil {
		t.Fatalf("read map10.json: %v", err)
	}
	var raw struct {
		NPCs []map[string]json.RawMessage `json:"npcs"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("map10.json: %v", err)
	}
	for i, row := range raw.NPCs {
		if _, ok := row["range"]; !ok {
			t.Fatalf("npc %d (%s) has no `range` key - the emitter dropped the column",
				i, doc.NPCs[i].ClassType)
		}
	}

	var withRange, without int
	board := false
	for _, npc := range doc.NPCs {
		switch {
		case npc.Range > 0:
			withRange++
		case npc.Range == 0:
			without++
		default:
			t.Fatalf("npc %s: range %d is negative", npc.ClassType, npc.Range)
		}
		if npc.ClassType == "ETC_Board" {
			board = true
			if npc.Range != 200 {
				t.Fatalf("ETC_Board range = %d, want 200 (datatable_npc Range)", npc.Range)
			}
		}
	}
	if withRange == 0 {
		t.Fatal("no map-10 NPC carries range > 0 - the column did not reach the emitter")
	}
	if without == 0 {
		t.Fatal("no map-10 NPC carries range 0 - the ordinary townsfolk lost their row value")
	}
	if !board {
		t.Fatal("map 10 has no ETC_Board - the pin needs a named row from the real town")
	}
}

// TestSpawnTriggerCarriesRange pins the appearance slot the reference fills from
// the NPC table: APPEARANCE_TRIGGER.range (ZServerLib\TriggerObject.cpp:415,
// GetAppearance -> packet offset apTrRange). It is a carried value, not a rule:
// the client stores it and the click gate never reads it.
func TestSpawnTriggerCarriesRange(t *testing.T) {
	cases := []struct {
		name string
		rng  int32
		want uint32
	}{
		{"declared", 300, 300},
		{"none", 0, 0},
		{"large", 2000, 2000},
	}
	for _, c := range cases {
		pkt := BuildSpawnTrigger(NPC{Handle: 0x40000000, NPCType: 99, Range: c.rng})
		got := binary.LittleEndian.Uint32(pkt[trAppearance+apTrRange:])
		if got != c.want {
			t.Errorf("%s: apTrRange = %d, want %d", c.name, got, c.want)
		}
		// The rest of the appearance must not shift: the value has its own slot.
		if typ := binary.LittleEndian.Uint32(pkt[trAppearance+apTrType:]); typ != 99 {
			t.Errorf("%s: apTrType = %d, want 99", c.name, typ)
		}
		if len(pkt) != trSize {
			t.Errorf("%s: packet is %d bytes, want %d", c.name, len(pkt), trSize)
		}
	}
}

// TestClickReachDoesNotUseTheNPCTableRange is the rule this card asked for,
// pinned so a later lane cannot "fix" it wrongly.
//
// The reference has NO per-NPC click reach. The click path is a constant:
// DISTANCE_TO_CHAR (100, ZServerLib\FieldObject.h:18), x5 when the client flags
// longdistance, folded with both actors' shape radii (FieldObject.cpp:120-128) -
// 200/600 between centres, which is exactly zone\click.go's ClickRange/
// ClickRangeLong. The client sends the click whatever the distance
// (GENormalView.cpp:952,988: no distance test on either sender).
//
// The NPC table's Range is the ENTER/LEAVE trigger radius instead (see
// NPC.Range). The data says so itself: ColonyTrigger=10000,
// SubBoss_BanTrigger_50m=5000, TutorialTrigger_Black_30m=3000, and the 1,549
// ordinary townsfolk are 0 - a 0 click reach would make every shopkeeper
// unclickable.
//
// So two real map-10 rows - one that declares a range (TutorialTrigger_White_3m,
// 300) and one that declares none (REB_SERM, 0) - must answer a click
// identically: out at 201, in at 199.
func TestClickReachDoesNotUseTheNPCTableRange(t *testing.T) {
	doc := loadTownNPCs(t, "map10.json")

	pick := func(class string) *NPC {
		for i := range doc.NPCs {
			if doc.NPCs[i].ClassType == class {
				return &doc.NPCs[i]
			}
		}
		return nil
	}
	declared, plain := pick("TutorialTrigger_White_3m"), pick("REB_SERM")
	if declared == nil || plain == nil {
		t.Skipf("map 10 lost %q or %q - the pin needs both real rows", "TutorialTrigger_White_3m", "REB_SERM")
	}
	if declared.Range == 0 || plain.Range != 0 {
		t.Fatalf("rows moved: %s range=%d, %s range=%d - want one declared and one 0",
			declared.ClassType, declared.Range, plain.ClassType, plain.Range)
	}

	for _, npc := range []*NPC{declared, plain} {
		at := [2]int32{npc.X, npc.Y}
		out := [2]int32{npc.X + 201, npc.Y}
		in := [2]int32{npc.X + 199, npc.Y}
		if InClickRange(at, out, false) {
			t.Errorf("%s (range %d): a click 201 away is IN reach - the gate grew a per-NPC term",
				npc.ClassType, npc.Range)
		}
		if !InClickRange(at, in, false) {
			t.Errorf("%s (range %d): a click 199 away is OUT of reach - the gate shrank",
				npc.ClassType, npc.Range)
		}
	}
	// And the constants themselves, so the reason above stays true.
	if ClickRangeFor(false) != 200 || ClickRangeFor(true) != 600 {
		t.Fatalf("ClickRangeFor = %v/%v, want 200/600 (DISTANCE_TO_CHAR folded with both S radii)",
			ClickRangeFor(false), ClickRangeFor(true))
	}
}
