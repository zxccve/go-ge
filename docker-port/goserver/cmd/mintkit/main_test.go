// Tests for the ONE decision that made the owner's warp scrolls eleven separate
// slots: how many bag rows a give needs, and what count each row gets.
//
// They do NOT drive main(): that opens the database and writes. The seam is
// planGive, which is pure - a catalog, a class, a count.
package main

import (
	"path/filepath"
	"strings"
	"testing"

	"geserver/item"
)

// testCatalog is three rows shaped like the real emitted file:
// [class id, mergeable (1/0), per-slot cap].
func testCatalog(t *testing.T) *item.Catalog {
	t.Helper()
	catalog, err := item.FromRows(map[string][]int64{
		"WarpScroll":      {21006, 1, 32767},
		"CrecementoSword": {7201, 0, 1},
	})
	if err != nil {
		t.Fatalf("build test catalog: %v", err)
	}
	return catalog
}

// TestStackableMintsOneRow is the owner's case: 20 warp scrolls must be ONE row of
// 20, because the class's own cap is 32,767. Hardcoding cap 1 produced twenty rows
// and read to him as "the warp scroll is not stacking".
func TestStackableMintsOneRow(t *testing.T) {
	plan, err := planGive(testCatalog(t), 21006, 20)
	if err != nil {
		t.Fatalf("planGive(21006, 20): %v", err)
	}
	if got := plan.RowCount(); got != 1 {
		t.Fatalf("20 warp scrolls = %d row(s), want 1 (rows: %v)", got, plan.Rows)
	}
	if plan.Rows[0].Count != 20 {
		t.Fatalf("the row holds %d, want 20", plan.Rows[0].Count)
	}
	if !plan.Rows[0].Merge {
		t.Fatal("a stackable class must merge into an existing stack, not open a slot")
	}
	if plan.MaxStack != 32767 {
		t.Fatalf("cap = %d, want the catalog's 32767", plan.MaxStack)
	}
}

// TestEquipmentMintsOneRowPerItem: equipment is MaxStack 1 in the catalog, so its
// behaviour must not change - N items are N slots of one.
func TestEquipmentMintsOneRowPerItem(t *testing.T) {
	plan, err := planGive(testCatalog(t), 7201, 3)
	if err != nil {
		t.Fatalf("planGive(7201, 3): %v", err)
	}
	if got := plan.RowCount(); got != 3 {
		t.Fatalf("3 swords = %d row(s), want 3", got)
	}
	for i, row := range plan.Rows {
		if row.Count != 1 {
			t.Fatalf("row %d holds %d, want 1", i, row.Count)
		}
		if row.Merge {
			t.Fatalf("row %d merges; equipment never does", i)
		}
	}
}

// TestUnknownClassRefuses: no catalog row means no measured cap, so nothing is
// written and the message says what the fallback would have been.
func TestUnknownClassRefuses(t *testing.T) {
	plan, err := planGive(testCatalog(t), 999999, 1)
	if err == nil {
		t.Fatalf("unknown class was accepted: %+v", plan)
	}
	if !strings.Contains(err.Error(), "not in the item catalog") {
		t.Fatalf("error does not name the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "cap 1") {
		t.Fatalf("error does not say what an unknown class would have become: %v", err)
	}
	if plan.RowCount() != 0 {
		t.Fatalf("a refused class still planned %d row(s)", plan.RowCount())
	}
}

// TestMoreThanOneStackSplits: a request bigger than one slot fills full rows plus a
// remainder instead of being refused as "cap 1".
func TestMoreThanOneStackSplits(t *testing.T) {
	plan, err := planGive(testCatalog(t), 21006, 70000)
	if err != nil {
		t.Fatalf("planGive(21006, 70000): %v", err)
	}
	want := []int64{32767, 32767, 4466}
	if len(plan.Rows) != len(want) {
		t.Fatalf("70000 scrolls = %d row(s), want %d", len(plan.Rows), len(want))
	}
	var total int64
	for i, row := range plan.Rows {
		if row.Count != want[i] {
			t.Fatalf("row %d holds %d, want %d", i, row.Count, want[i])
		}
		total += row.Count
	}
	if total != 70000 {
		t.Fatalf("rows add up to %d, want 70000", total)
	}
	if !plan.Rows[0].Merge || plan.Rows[1].Merge || plan.Rows[2].Merge {
		t.Fatalf("only the first row may merge: %v", plan.Rows)
	}
}

// TestZeroCountRefuses: a give of nothing is a typo, not a request.
func TestZeroCountRefuses(t *testing.T) {
	if _, err := planGive(testCatalog(t), 21006, 0); err == nil {
		t.Fatal("count 0 was accepted")
	}
}

// TestRealCatalogWarpScrollCap is the emitted file itself: the class the owner
// minted must carry 32,767, not 1.
func TestRealCatalogWarpScrollCap(t *testing.T) {
	catalog, err := item.Load(filepath.Join("..", "..", "..", "data", "item_catalog.json"))
	if err != nil {
		t.Skipf("item catalog not built: %v", err)
	}
	entry, ok := catalog.EntryOf(21006)
	if !ok {
		t.Fatal("class 21006 is not in data\\item_catalog.json")
	}
	if entry.MaxStack != 32767 {
		t.Fatalf("class 21006 cap = %d, want 32767", entry.MaxStack)
	}
}
