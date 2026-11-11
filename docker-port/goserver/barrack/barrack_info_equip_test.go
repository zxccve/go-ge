package barrack

import (
	"encoding/binary"
	"testing"
)

// TestEmptyEquipmentUsesClientSentinelIds is the regression test for the crash
// that history\barrack.md records as #4: an equipment spot must carry the
// client's own "None" class id, never 0. A zero reaches FUN_00636a9a(0) (null
// item) and aborts the client.
//
// With only 42 worn items across 12 characters, MOST spots are empty, so this
// is the common case, not an edge case.
func TestEmptyEquipmentUsesClientSentinelIds(t *testing.T) {
	b := BuildBarrackInfo([]CharInfo{{Name: "Vex", Job: 7}})
	rec := b[BarrackInfoHeaderLen:]

	for spot := 0; spot < equipSpotLast; spot++ {
		got := binary.LittleEndian.Uint32(rec[pcInfoOffEquip+spot*equipEntryStride:])
		if got == 0 {
			t.Fatalf("spot %d is 0: the client crashes on an empty equipment slot", spot)
		}
		if got != equipNone[spot] {
			t.Fatalf("spot %d = %d, want the client's own None id %d", spot, got, equipNone[spot])
		}
	}
	// The documented traps: glove and boots have their own ids.
	if equipNone[8] != 19000 || equipNone[9] != 19500 {
		t.Fatalf("glove/boots sentinels = %d/%d, want 19000/19500", equipNone[8], equipNone[9])
	}
}

// TestWornItemOverridesTheSentinel proves a real item wins over the default.
func TestWornItemOverridesTheSentinel(t *testing.T) {
	var c CharInfo
	c.Equip[0] = 12345
	b := BuildBarrackInfo([]CharInfo{c})
	rec := b[BarrackInfoHeaderLen:]
	if got := binary.LittleEndian.Uint32(rec[pcInfoOffEquip:]); got != 12345 {
		t.Fatalf("worn item at spot 0 = %d, want 12345", got)
	}
	if got := binary.LittleEndian.Uint32(rec[pcInfoOffEquip+equipEntryStride:]); got != equipNone[1] {
		t.Fatalf("empty spot 1 = %d, want the sentinel %d", got, equipNone[1])
	}
}
