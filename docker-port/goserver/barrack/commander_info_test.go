package barrack

import (
	"encoding/binary"
	"testing"
)

// TestCommanderInfoLayout pins the offsets that were ported from the working
// Python builder. If any field moves, the client reads the rest of the barrack
// wrong - so each offset is asserted, not just the total size.
func TestCommanderInfoLayout(t *testing.T) {
	var names [maxBarrackChannel]string
	names[0] = "Barrack 1"
	b := BuildCommanderInfo(CommanderInfo{
		Name:         "Arcanine",
		Rank:         3,
		CharSlot:     4,
		SaveMapID:    100,
		BarrackNames: names,
	})

	if len(b) != CommanderInfoSize {
		t.Fatalf("packet = %d bytes, want %d", len(b), CommanderInfoSize)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 10 {
		t.Fatalf("command = %d, want 10 (BC_COMMANDER_INFO)", cmd)
	}
	if got := string(b[2:2+commanderNameLen]); got[:len("Arcanine")] != "Arcanine" {
		t.Fatalf("name = %q, want Arcanine", got)
	}

	// Offsets, by hand from the builder: name(17) party(17) profile(251)
	// title(9) = 296.
	const rankAt = 296
	if got := binary.LittleEndian.Uint16(b[rankAt:]); got != 3 {
		t.Fatalf("rank = %d, want 3", got)
	}
	if got := binary.LittleEndian.Uint16(b[rankAt+2:]); got != 4 {
		t.Fatalf("charSlot = %d, want 4", got)
	}
	// rank(2) charSlot(2) commanderPoint(4) ladderPoint(2) = 10
	const newMapAt = rankAt + 10
	if got := binary.LittleEndian.Uint16(b[newMapAt:]); got != 100 {
		t.Fatalf("newMapID = %d, want 100 - the client dereferences the map name", got)
	}
	// No save point: the last point's map is used for both, which is the single-map
	// barrack of before the save point existed.
	if got := binary.LittleEndian.Uint16(b[newMapAt+2:]); got != 100 {
		t.Fatalf("contMapID = %d, want 100", got)
	}

	// names(24 x 17): the first slot is set, the rest default to "None".
	const namesAt = rankAt + 10 + 2 + 2 + 2 + 2 + 1 + 2 + 2 + 4 + 4
	if got := b[namesAt : namesAt+barrackNameLen]; string(got[:9]) != "Barrack 1" {
		t.Fatalf("barrackName[0] = %q, want Barrack 1", got)
	}
	second := namesAt + barrackNameLen
	if got := b[second : second+barrackNameLen]; string(got[:4]) != "None" {
		t.Fatalf("barrackName[1] = %q, want the default None", got)
	}
}

// TestCommanderInfoTruncatesLongNames proves a long name cannot shift every
// later field, which is the classic way this layout breaks.
func TestCommanderInfoTruncatesLongNames(t *testing.T) {
	long := "0123456789abcdefghijklmnop"
	b := BuildCommanderInfo(CommanderInfo{Name: long, SaveMapID: 7})
	if len(b) != CommanderInfoSize {
		t.Fatalf("packet = %d bytes, want %d", len(b), CommanderInfoSize)
	}
	if got := binary.LittleEndian.Uint16(b[296:]); got != 0 {
		t.Fatalf("rank = %d after a long name - the field shifted", got)
	}
	// The name field keeps 16 bytes and a terminating zero.
	if b[2+commanderNameLen-1] != 0 {
		t.Fatal("the name field must end in a NUL")
	}
}
