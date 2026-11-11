package zone

import (
	"encoding/binary"
	"testing"
)

// THE WORN-EQUIPMENT TESTS (R32's leftover).
//
// A remote player's spawn is built from the same appearance block START_INFO
// carries, so these tests look INSIDE the spawn packet at
// spAppearance + apEquipment + spot*equipEntrySize - the very bytes the other
// client's model builder reads.
//
// appearanceEquip is that offset for one slot.
func appearanceEquip(spot int) int {
	return spAppearance + apEquipment + spot*equipEntrySize
}

// TestSpawnCarriesTheWornWeaponInTheAppearanceOnly: a character wearing a shield and
// a sword is announced with BOTH of them in the appearance slots the client draws the
// hands from - and with ZERO in the WORD at +11.
//
// +11 IS NOT "THE SPARE COPY OF THE WEAPON". It used to carry the weapon's class id,
// and armed characters then T-posed and kept their walk animation in a town in game
// (owner report 2026-10-29). The live server sends 0 there for its own armed Fighter
// (shield 7215 in slot 4, sword 7201 in slot 5) - captures\pcref2\20261028-232622,
// six spawns, +11 = 0 in all six. The model gets its weapon from the appearance block
// and from nowhere else.
//
// The length word is checked too: it must still equal the packet's own length. This
// project has shipped a spawn whose length word disagreed with its bytes twice, and
// when they disagree the client reads the rest of the stream as option records.
func TestSpawnCarriesTheWornWeaponInTheAppearanceOnly(t *testing.T) {
	// The real pair: 7215 = CrecementoShield in the off hand (4), 7201 =
	// CrecementoSword in the main hand (5) - the same two slots and the same two ids
	// the live capture's Fighter carries.
	c := Character{Handle: 40, Name: "Fighter", Job: 1, Stance: 10110}
	c.Equip[4] = 7215
	c.Equip[5] = 7201

	pkt := BuildSpawnStandingPC(c.Handle, c, [3]int32{100, 200, 5}, DefaultPosture)

	if len(pkt) != SpawnStandingPCSize {
		t.Fatalf("%d bytes, want %d", len(pkt), SpawnStandingPCSize)
	}
	if n := binary.LittleEndian.Uint16(pkt[2:]); int(n) != len(pkt) {
		t.Fatalf("length word = %d, packet = %d bytes - they MUST agree", n, len(pkt))
	}
	if got := binary.LittleEndian.Uint16(pkt[spWeapon:]); got != 0 {
		t.Fatalf("weapon word at +11 = %d, want 0 - an ARMED character gets the same 0 "+
			"the live server sends, or the client goes looking for a battle stance a "+
			"town does not play", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[appearanceEquip(4):]); got != 7215 {
		t.Fatalf("appearance slot 4 = %d, want the worn shield 7215", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[appearanceEquip(5):]); got != 7201 {
		t.Fatalf("appearance slot 5 = %d, want the worn sword 7201", got)
	}
	// Every other slot keeps the client's own None id, never 0.
	for _, spot := range []int{0, 1, 2, 3, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20} {
		if got := binary.LittleEndian.Uint32(pkt[appearanceEquip(spot):]); got != EquipNone[spot] {
			t.Fatalf("appearance slot %d = %d, want the None id %d", spot, got, EquipNone[spot])
		}
	}
}

// TestSpawnWithNothingWornSendsNoneNotZero: a character wearing nothing is announced
// with the client's own per-slot None class id in all 21 slots and 0 in the hand
// word. ZERO IS NOT "NOTHING WORN" - it makes the client dereference a null item
// property object, which crashed the first two live logins.
func TestSpawnWithNothingWornSendsNoneNotZero(t *testing.T) {
	c := Character{Handle: 7, Name: "Jin", Job: 3}

	pkt := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	for spot := 0; spot < equipSpotLast; spot++ {
		got := binary.LittleEndian.Uint32(pkt[appearanceEquip(spot):])
		if got == 0 {
			t.Fatalf("slot %d is 0 - the client would crash on this", spot)
		}
		if got != EquipNone[spot] {
			t.Fatalf("slot %d = %d, want the None id %d", spot, got, EquipNone[spot])
		}
	}
	if got := binary.LittleEndian.Uint16(pkt[spWeapon:]); got != 0 {
		t.Fatalf("weapon class id at +11 = %d, want 0 for bare hands", got)
	}
}

// TestABareHandedCharacterHasTheLiveBareHandIDs: with nothing in either hand, both
// hand slots carry 10000 - the client's own bare-hand class id, and the value the
// live server's bare-handed Scout carries in slots 4 and 5
// (captures\pcref2\20261028-232622). The armed case is in
// TestSpawnCarriesTheWornWeaponInTheAppearanceOnly; this one is the control, and it
// is the case that has always looked right in game.
func TestABareHandedCharacterHasTheLiveBareHandIDs(t *testing.T) {
	c := Character{Handle: 34, Name: "Scout", Job: 3, Stance: 30110}

	pkt := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	for _, spot := range []int{4, 5} {
		if got := binary.LittleEndian.Uint32(pkt[appearanceEquip(spot):]); got != 10000 {
			t.Fatalf("appearance slot %d = %d, want 10000 - the bare hand id the live "+
				"server's own unarmed Scout sends", spot, got)
		}
	}
	if got := binary.LittleEndian.Uint16(pkt[spWeapon:]); got != 0 {
		t.Fatalf("weapon word at +11 = %d, want 0", got)
	}
}

// TestAClassIDAboveAWordIsStillCarriedInFull: the appearance slot is a DWORD, so a
// class id the WORD at +11 could never hold (125349 = CraftedEmpire_Burst, a real id
// on real wear rows) is still carried in full where the client reads it. Nothing is
// truncated onto the wire, and nothing is invented at +11 either.
func TestAClassIDAboveAWordIsStillCarriedInFull(t *testing.T) {
	c := Character{Handle: 455, Name: "Grace", Job: 6}
	c.Equip[5] = 125349

	pkt := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	if got := binary.LittleEndian.Uint16(pkt[spWeapon:]); got != 0 {
		t.Fatalf("weapon word at +11 = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[appearanceEquip(5):]); got != 125349 {
		t.Fatalf("appearance slot 5 = %d, want the worn weapon 125349", got)
	}
}

// TestWeaponHandsAreTheRightOneFirst guards the PAIR of hand slots itself and the
// order the log names them in. The live capture settles the numbering: its armed
// Fighter carries the shield 7215 in slot 4 and the sword 7201 in slot 5, and this
// server's own DB rows agree (item classes 72xx sit at spot 5, 125349 at spot 5).
// If the numbering moved, a sword in the main hand would be read as a ring - the
// client would still draw something, which is why this is pinned rather than left to
// the call site.
func TestWeaponHandsAreTheRightOneFirst(t *testing.T) {
	if equipWeaponSpots != [2]int{5, 4} {
		t.Fatalf("weapon hands = %v, want {5, 4} (right hand first)", equipWeaponSpots)
	}
	// The client's own "bare hand" ids: both weapon hands share one None class.
	if EquipNone[4] != EquipNone[5] {
		t.Fatalf("slot 4 None id %d != slot 5 None id %d", EquipNone[4], EquipNone[5])
	}
	// Character 40's real pair: a SHIELD in the left hand must not be mistaken for
	// the weapon beside it.
	var c Character
	c.Equip[4] = 7215 // shield
	c.Equip[5] = 7201 // sword
	if got := weaponClassOf(c); got != 7201 {
		t.Fatalf("weaponClassOf = %d, want the right-hand sword 7201", got)
	}
	// A character holding only something in the left hand still gets it.
	c.Equip[5] = 0
	if got := weaponClassOf(c); got != 7215 {
		t.Fatalf("weaponClassOf = %d, want the left-hand item 7215", got)
	}
}
