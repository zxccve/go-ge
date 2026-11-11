package zone

import (
	"encoding/binary"
	"testing"
)

// TestEveryEquipmentSlotIsARealItemClass is the regression test for the crash that
// took two live logins: a record whose equipment ids are all zero made the client
// dereference a null item property object (`CMP [EDI+0x1dc]`, EDI=0). Zero is not
// "nothing worn" - the client has an explicit "None" class id per slot.
func TestEveryEquipmentSlotIsARealItemClass(t *testing.T) {
	rec := BuildStartInfo(Character{Handle: 1, Name: "Jin", Job: 3})

	for spot := 0; spot < equipSpotLast; spot++ {
		off := siAppearance + apEquipment + spot*equipEntrySize
		got := binary.LittleEndian.Uint32(rec[off:])
		if got == 0 {
			t.Fatalf("slot %d is 0 - the client would crash on this", spot)
		}
		if got != EquipNone[spot] {
			t.Fatalf("slot %d = %d, want the None id %d", spot, got, EquipNone[spot])
		}
	}
}

// TestWornEquipmentWins: a real item must not be overwritten by the default.
func TestWornEquipmentWins(t *testing.T) {
	c := Character{Handle: 1, Name: "Jin", Job: 3}
	c.Equip[5] = 123456 // right hand
	rec := BuildStartInfo(c)

	off := siAppearance + apEquipment + 5*equipEntrySize
	if got := binary.LittleEndian.Uint32(rec[off:]); got != 123456 {
		t.Fatalf("worn item = %d, want 123456", got)
	}
	// Its neighbours must still be defaults, not shifted.
	off4 := siAppearance + apEquipment + 4*equipEntrySize
	if got := binary.LittleEndian.Uint32(rec[off4:]); got != EquipNone[4] {
		t.Fatalf("slot 4 = %d, want the None id %d", got, EquipNone[4])
	}
}

// TestEquipNoneIsTwentyOneEntries guards the table itself: a short table would
// read past its end, and a changed length means the client's stride changed.
func TestEquipNoneIsTwentyOneEntries(t *testing.T) {
	if len(EquipNone) != equipSpotLast {
		t.Fatalf("EquipNone has %d entries, want %d", len(EquipNone), equipSpotLast)
	}
	for i, v := range EquipNone {
		if v == 0 {
			t.Fatalf("EquipNone[%d] is 0", i)
		}
	}
}
