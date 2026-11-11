package item

import "testing"

// TestEquipSlotsAllows is the whole rule in one table: a one-handed item fits
// either hand, a ring either finger, a single-slot item only its own spot, an
// unknown class nothing at all, and a nil table nothing at all (never a panic).
func TestEquipSlotsAllows(t *testing.T) {
	slots, err := FromEquipRows(map[string]int{
		"7201":  EqpEitherHand, // Crecemento, a sword: LHAND or RHAND
		"7202":  EqpEitherHand,
		"9050":  EqpEitherRing, // a ring: RING1 or RING2
		"15001": 1,             // BODY armour: slot 1 and nowhere else
		"17000": 0,             // a HEAD hair row: slot 0
	})
	if err != nil {
		t.Fatalf("FromEquipRows: %v", err)
	}
	if slots.Len() != 5 {
		t.Fatalf("Len() = %d, want 5", slots.Len())
	}

	cases := []struct {
		name    string
		classID int64
		spot    int
		want    bool
	}{
		{"either hand, left", 7201, 4, true},
		{"either hand, right", 7201, 5, true},
		{"either hand, not the body", 7201, 1, false},
		{"either hand, not a ring", 7201, 6, false},
		{"either ring, ring 1", 9050, 6, true},
		{"either ring, ring 2", 9050, 7, true},
		{"either ring, not a hand", 9050, 5, false},
		{"single slot, its own spot", 15001, 1, true},
		{"single slot, a hand", 15001, 5, false},
		{"single slot, the head", 15001, 0, false},
		{"single slot, spot 0 item", 17000, 0, true},
		{"single slot, hatted elsewhere", 17000, 12, false},
		{"unknown class", 999999, 5, false},
		{"spot below the block", 7201, -1, false},
		{"spot past the block", 7201, equipSpotLast, false},
	}
	for _, c := range cases {
		if got := slots.Allows(c.classID, c.spot); got != c.want {
			t.Errorf("%s: Allows(%d, %d) = %v, want %v",
				c.name, c.classID, c.spot, got, c.want)
		}
	}

	// A nil table must answer false, not panic: a zone started without
	// data\item_equip.json has exactly this value.
	var none *EquipSlots
	if none.Allows(7201, 5) {
		t.Fatal("a nil table allowed a class - a missing file would read as 'wearable'")
	}
	if none.Len() != 0 {
		t.Fatalf("a nil table reported %d rows", none.Len())
	}
}

// TestFromEquipRowsDropsWhatItCannotPlace: a bad key or an out-of-range slot is
// one row lost, and the table still loads. A file that parses to nothing is an
// error, because an empty table refuses every equip and looks like a client bug.
func TestFromEquipRowsDropsWhatItCannotPlace(t *testing.T) {
	slots, err := FromEquipRows(map[string]int{
		"not-a-class": 5,
		"0":           5,
		"7201":        99, // not a spot and not one of the two markers
		"15001":       1,
	})
	if err != nil {
		t.Fatalf("FromEquipRows: %v", err)
	}
	if slots.Len() != 1 {
		t.Fatalf("Len() = %d, want the 1 usable row", slots.Len())
	}
	if !slots.Allows(15001, 1) {
		t.Fatal("the one usable row was lost")
	}

	if _, err := FromEquipRows(map[string]int{"7201": 99}); err == nil {
		t.Fatal("a table of nothing but unusable rows was accepted")
	}
	if _, err := FromEquipRows(nil); err == nil {
		t.Fatal("an empty table was accepted")
	}
}
