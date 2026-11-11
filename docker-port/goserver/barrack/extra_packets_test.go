package barrack

import (
	"encoding/binary"
	"testing"
)

// TestEquipExtraHeader pins the 16-byte header, which is the one packet whose
// wrong size cost 58 seconds of client grinding.
func TestEquipExtraHeader(t *testing.T) {
	b := BuildEquipExtra()
	if len(b) != EquipExtraHeaderLen {
		t.Fatalf("packet = %d bytes, want %d", len(b), EquipExtraHeaderLen)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 17 {
		t.Fatalf("command = %d, want 17", cmd)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != len(b) {
		t.Fatalf("length word = %d, want %d", got, len(b))
	}
	for _, off := range []int{4, 8, 12} {
		if got := binary.LittleEndian.Uint32(b[off:]); got != 0 {
			t.Fatalf("count at +%d = %d, want 0", off, got)
		}
	}
}

// TestEmptyItemList pins the documented empty inventory shape.
func TestEmptyItemList(t *testing.T) {
	b := BuildEmptyItemList()
	if len(b) != EmptyItemListLen {
		t.Fatalf("packet = %d bytes, want %d", len(b), EmptyItemListLen)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 26 {
		t.Fatalf("command = %d, want 26", cmd)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != len(b) {
		t.Fatalf("length word = %d, want %d", got, len(b))
	}
	if got := binary.LittleEndian.Uint32(b[4:]); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
}
