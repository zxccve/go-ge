package zone

// THE RING MUST SIT OUTSIDE WHAT THE CLIENT DRAWS (owner, 2026-11-10: "the actor popping in and
// out/missing actor etc is the biggest issue... let the server ring process just outside that,
// 5000/6000 so that the actor transition is not buggy").
//
// The numbers in these tests are the CLIENT's own map table, not invented: on the towns
// (map 10 reb_basic, map 15 coim_basic) PcViewDist = MonViewDist = 4000 and ViewRange = 4000,
// so the old ring said LEAVE at exactly the radius the client still draws the actor at, and
// stopped UPDATES at 2500 - inside the client's draw distance. That is a pop at the edge and
// a frozen actor in the band. Most maps were already wide (ViewRange 6000, draw 3000/3200).

import (
	"path/filepath"
	"testing"
)

// A TOWN ROW: the server's circle must move outside the client's 4000.
func TestTheRingSitsOutsideTheClientsDrawDistance(t *testing.T) {
	view, middle, ok := sightRadiiFor(SightRow{ViewRange: 4000, ViewMiddleRange: 2500, PcViewDist: 4000, MonViewDist: 4000})
	if !ok {
		t.Fatal("the row has a ViewRange, so the ring must be armed")
	}
	if view != 5000 {
		t.Fatalf("leave = %d, want 5000 (client draws 4000 + a 1000 margin)", view)
	}
	if middle != 4500 {
		t.Fatalf("update = %d, want 4500 (client draws 4000 + a 500 update lead)", middle)
	}
}

// A FIELD ROW THAT WAS ALREADY WIDE: no change, no extra fan-out.
func TestAMapWhoseRingIsAlreadyWideIsUnchanged(t *testing.T) {
	view, middle, ok := sightRadiiFor(SightRow{ViewRange: 6000, ViewMiddleRange: 4000, PcViewDist: 3000, MonViewDist: 3200})
	if !ok || view != 6000 || middle != 4000 {
		t.Fatalf("leave/update = %d/%d (ok %v), want 6000/4000 - a 6000 ring is already 2800 past the draw",
			view, middle, ok)
	}
}

// THE FREEZE BAND IS KEPT, but only where it is INVISIBLE: a wide ring with a middle smaller
// than the client's draw distance is widened to the draw distance.
func TestTheUpdateCircleNeverFreezesSomethingTheClientDraws(t *testing.T) {
	view, middle, ok := sightRadiiFor(SightRow{ViewRange: 6000, ViewMiddleRange: 2500, PcViewDist: 4000, MonViewDist: 4000})
	if !ok || view != 6000 || middle != 4500 {
		t.Fatalf("leave/update = %d/%d (ok %v), want 6000/4500", view, middle, ok)
	}
}

// A ROW WITHOUT THE CLIENT COLUMNS KEEPS THE REFERENCE'S OWN NUMBERS - which is what every
// existing test fixture and every map without those columns gets.
func TestARowWithoutClientDrawColumnsIsUnchanged(t *testing.T) {
	for _, row := range []SightRow{
		{ViewRange: 6000, ViewMiddleRange: 4000},
		{ViewRange: 4000, ViewMiddleRange: 2500},
		{ViewRange: 6000},
	} {
		view, middle, ok := sightRadiiFor(row)
		if !ok || view != row.ViewRange || middle != row.ViewMiddleRange {
			t.Fatalf("row %+v -> %d/%d (ok %v), want the table's own %d/%d",
				row, view, middle, ok, row.ViewRange, row.ViewMiddleRange)
		}
	}
}

// A ROW WITH NO ViewRange IS NOT A RING: the caller sends to everyone.
func TestARowWithoutAViewRangeIsUnarmed(t *testing.T) {
	if _, _, ok := sightRadiiFor(SightRow{PcViewDist: 4000, MonViewDist: 4000}); ok {
		t.Fatal("a row with ViewRange 0 must not arm the ring")
	}
}

// THE SHIPPED TABLE, through the real loader: the two town rows the owner sees pop, and one
// field row that must not move.
func TestTheShippedTownRowsGetTheirMargin(t *testing.T) {
	tab, err := LoadSightTable(filepath.Join("..", "..", "data", "map_table.xml"))
	if err != nil {
		t.Fatalf("load the shipped map table: %v", err)
	}
	towns := map[int]SightRow{10: {}, 15: {}}
	for id := range towns {
		row, ok := tab.Row(id)
		if !ok {
			t.Fatalf("map %d has no row in data\\map_table.xml", id)
		}
		view, middle, armed := sightRadiiFor(row)
		if !armed || view != 5000 || middle != 4500 {
			t.Fatalf("map %d: leave/update = %d/%d (armed %v), want 5000/4500 (row ViewRange %d middle %d draw %d/%d)",
				id, view, middle, armed, row.ViewRange, row.ViewMiddleRange, row.PcViewDist, row.MonViewDist)
		}
	}
	row, ok := tab.Row(208) // fld_coim: ViewRange 6000, draw 3000/3200 - unchanged
	if !ok {
		t.Fatal("map 208 has no row")
	}
	if view, middle, armed := sightRadiiFor(row); !armed || view != 6000 || middle != 4000 {
		t.Fatalf("map 208: leave/update = %d/%d (armed %v), want 6000/4000", view, middle, armed)
	}
}
