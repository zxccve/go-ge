package item

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadPrices is the three questions a buy/sell asks: what it costs, what a
// shop pays back, and whether a shop takes it at all.
func TestLoadPrices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	blob := `{"count":3,"items":{
		"30000":[1,0,0,2147483647,"Vis"],
		"21613":[5000,2500,1,32767,"FireStone"],
		"1234":[90000,45000,1,1,"Sword"]}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadPrices(path)
	if err != nil {
		t.Fatalf("LoadPrices: %v", err)
	}
	if p.Len() != 3 {
		t.Fatalf("Len = %d, want 3", p.Len())
	}

	stone, ok := p.Get(21613)
	if !ok {
		t.Fatal("class id 21613 is not in the table")
	}
	if stone.Price != 5000 || stone.SellPrice != 2500 {
		t.Fatalf("FireStone price/sell = %d/%d, want 5000/2500",
			stone.Price, stone.SellPrice)
	}
	if !stone.Venderable || stone.MaxStack != 32767 {
		t.Fatalf("FireStone = %+v, want venderable with a 32767 stack", stone)
	}
	if stone.Name != "FireStone" {
		t.Fatalf("Name = %q, want FireStone", stone.Name)
	}

	// Vis has a zero sell price and a zero venderable flag: a shop will not buy
	// it back, and that is the flag the client's own shop reads.
	vis, _ := p.Get(30000)
	if vis.Venderable || vis.SellPrice != 0 {
		t.Fatalf("Vis = %+v, want not venderable and a zero sell price", vis)
	}

	if _, ok := p.Get(999999); ok {
		t.Fatal("an unknown class id resolved")
	}
}

// TestLoadPricesErrors: a missing or empty file must be an ERROR, not an empty
// table - an empty table prices everything at 0, which reads as "everything is
// free" and would let a buy take any item.
func TestLoadPricesErrors(t *testing.T) {
	if _, err := LoadPrices(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file loaded")
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"count":0,"items":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrices(empty); err == nil {
		t.Fatal("an empty price table was accepted")
	}

	// A nil receiver is answered, not panicked on: a zone started without the
	// file holds a nil table and every buy path still has to ask.
	var none *Prices
	if _, ok := none.Get(21613); ok {
		t.Fatal("a nil table priced an item")
	}
	if none.Len() != 0 {
		t.Fatal("a nil table reported rows")
	}
}

// TestFromPriceRowsTolerance: one short row costs that one item, not the table.
func TestFromPriceRowsTolerance(t *testing.T) {
	p, err := FromPriceRows(map[string][]any{
		"21613": {5000, 2500, 1, 32767, "FireStone"},
		"1234":  {90000},          // short: skipped
		"x":     {1, 1, 1, 1, ""}, // not a class id: skipped
	})
	if err != nil {
		t.Fatalf("FromPriceRows: %v", err)
	}
	if p.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (only the complete row)", p.Len())
	}
}

// TestLoadFesoPrices is the Feso table's own three cells. It is a SEPARATE table
// from the vis one on purpose: an item can be sold for one coin and not the other,
// and a Feso buy must never fall back to the vis price.
func TestLoadFesoPrices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feso.json")
	blob := `{"count":2,"items":{"21613":[700,350,1],"1234":[9,0,0]}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := LoadFesoPrices(path)
	if err != nil {
		t.Fatalf("LoadFesoPrices: %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("Len = %d, want 2", p.Len())
	}
	stone, ok := p.Get(21613)
	if !ok {
		t.Fatal("class id 21613 is not in the Feso table")
	}
	if stone.Price != 700 || stone.SellPrice != 350 || !stone.Venderable {
		t.Fatalf("FireStone Feso = %+v, want 700/350 and venderable", stone)
	}
	// A class id that is ABSENT is not sold for Feso. The extractor dropped the
	// sentinel rows, so absence is the whole rule.
	if _, ok := p.Get(20022); ok {
		t.Fatal("a class id the file does not carry was priced for Feso")
	}

	// A missing file is an ERROR: an empty table would make every Feso purchase
	// "unknown price", and a path that read unknown as 0 would give goods away.
	if _, err := LoadFesoPrices(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing Feso price file loaded")
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"count":0,"items":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFesoPrices(empty); err == nil {
		t.Fatal("an empty Feso price table was accepted")
	}

	// A nil receiver is answered, not panicked on, like Prices.
	var none *FesoPrices
	if _, ok := none.Get(21613); ok || none.Len() != 0 {
		t.Fatal("a nil Feso table answered with rows")
	}
}
