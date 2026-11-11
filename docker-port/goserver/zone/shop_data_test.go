package zone

import (
	"os"
	"testing"

	"geserver/item"
)

// The three files the buy/sell paths read, as `src\emit_shop_data.py` writes
// them. Tests in this repo already read `../../data` directly (see
// protocol\fixture_test.go), so this is the same arrangement.
const (
	realPricesPath      = "../../data/item_prices.json"
	realShopGroupsPath  = "../../data/shop_groups.json"
	realCustomShopsPath = "../../data/custom_shops.json"
)

// TestRealShopData loads the emitted files themselves. It is the check the loaders
// exist for: a file that parses but has lost rows still "works" - the shop opens,
// and the buy that would have named an item finds nothing. The counts are the ones
// emit_shop_data.py reported when these files were written (2026-10-26).
//
// SKIPPED when the files are absent: they are derived data, and a checkout without
// a game tree is expected to lack them.
func TestRealShopData(t *testing.T) {
	if _, err := os.Stat(realPricesPath); err != nil {
		t.Skipf("no emitted shop data (%v); run: cd src && python emit_shop_data.py", err)
	}

	prices, err := item.LoadPrices(realPricesPath)
	if err != nil {
		t.Fatalf("LoadPrices: %v", err)
	}
	if prices.Len() != 27922 {
		t.Fatalf("prices hold %d rows, the emitter wrote 27922 (base + DEU overlay)", prices.Len())
	}
	// Fire stone is the item a shop purchase is most likely to be tried with: a
	// real price, a real sell price, and a stack cap. Its SELL price is 1, not the
	// base table's 1100: the client applies the DEU locale overlay, and the overlay
	// wins (measured in game 2026-10-26 - the shop showed 1 Vis while we paid 1100).
	stone, ok := prices.Get(21613)
	if !ok {
		t.Fatal("Fire stone (21613) has no price row")
	}
	if stone.Price != 124200 || stone.SellPrice != 1 || !stone.Venderable {
		t.Fatalf("Fire stone = %+v, want 124200/1 and venderable (the DEU overlay value)", stone)
	}
	if stone.Name != "Fire stone" {
		t.Fatalf("Fire stone name = %q", stone.Name)
	}

	groups, err := LoadShopGroups(realShopGroupsPath)
	if err != nil {
		t.Fatalf("LoadShopGroups: %v", err)
	}
	if groups.Len() != 101 {
		t.Fatalf("shop groups hold %d groups, the emitter wrote 101", groups.Len())
	}
	// The order is what a buy record indexes into, so it must be the SOURCE
	// TABLE's order, never a sorted one. Sorting it bought the wrong item in game
	// (2026-10-26): index 0 became 20022 instead of the client's 21006.
	leonardo := groups.Items("ETC_LEONARDO")
	want := []int64{21006, 20022, 20023, 20024, 23234, 89088, 89089, 89090,
		495502, 26009, 27901, 27902, 495501}
	if len(leonardo) != len(want) {
		t.Fatalf("ETC_LEONARDO has %d ids, want %d: %v", len(leonardo), len(want), leonardo)
	}
	for i, id := range want {
		if leonardo[i] != id {
			t.Fatalf("ETC_LEONARDO = %v, want %v (in that order)", leonardo, want)
		}
	}

	custom, err := LoadCustomShops(realCustomShopsPath)
	if err != nil {
		t.Fatalf("LoadCustomShops: %v", err)
	}
	if custom.Len() != 194 {
		t.Fatalf("custom shops hold %d groups, the emitter wrote 194", custom.Len())
	}
	rows := custom["13YEAR_SHOP"]
	if len(rows) == 0 || rows[0] != [5]int64{27380, 24187, 200, 0, 0} {
		t.Fatalf("13YEAR_SHOP = %v, want the first entry 27380/24187/200/0/0", rows)
	}
	// CLAUD is the group a live reference capture drew: 21 rows and a 773-byte
	// window. Its first row is the one the capture shows (7201 bought, 21450
	// paid, once), so the count and the first row are pinned together.
	claud := custom.Entries("CLAUD")
	if len(claud) != 21 {
		t.Fatalf("CLAUD has %d row(s), the capture's window held 21", len(claud))
	}
	if want := (CustomShopEntry{BuyItem: 7201, PriceItem0: 21450, Count0: 1}); claud[0] != want {
		t.Fatalf("CLAUD row 0 = %+v, want %+v", claud[0], want)
	}
}

// TestCustomShopEntryRowOrder is the reorder that a positional copy gets wrong:
// a JSON row is [sell, price1, count1, price2, count2] while a window record is
// (BuyItem, PriceItem0, PriceItem1, Count0, Count1). Copied by position, count1
// lands where price2 belongs and the two prices swap - the window then shows a
// price of 1 item of class 1, and the barter charges the wrong stack.
func TestCustomShopEntryRowOrder(t *testing.T) {
	shops := CustomShops{"X": {{7201, 21450, 1, 0, 0}}}
	got := shops.Entries("X")
	if len(got) != 1 {
		t.Fatalf("%d entries, want 1", len(got))
	}
	want := CustomShopEntry{BuyItem: 7201, PriceItem0: 21450, PriceItem1: 0, Count0: 1, Count1: 0}
	if got[0] != want {
		t.Fatalf("entry = %+v, want %+v", got[0], want)
	}
	// A row that fills BOTH price pairs proves the two middle columns do not swap.
	two := CustomShops{"Y": {{89359, 89364, 15, 24109, 250}}}.Entries("Y")
	wantTwo := CustomShopEntry{BuyItem: 89359, PriceItem0: 89364, PriceItem1: 24109,
		Count0: 15, Count1: 250}
	if len(two) != 1 || two[0] != wantTwo {
		t.Fatalf("entry = %+v, want %+v", two, wantTwo)
	}
	// The ORDER is the index a buy counts in, so it must not be sorted.
	order := CustomShops{"Z": {{200, 1, 1, 0, 0}, {100, 1, 1, 0, 0}}}.Entries("Z")
	if len(order) != 2 || order[0].BuyItem != 200 || order[1].BuyItem != 100 {
		t.Fatalf("entries = %+v, want 200 then 100 (the file's order, never sorted)", order)
	}
	if got := (CustomShops{}).Entries("MISSING"); got != nil {
		t.Fatalf("a missing group answered %+v, want nil", got)
	}
}
