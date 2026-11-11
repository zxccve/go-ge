package zone

import (
	"encoding/json"
	"fmt"
	"os"
)

// ShopGroups is what a NORMAL shop sells: a group name -> the class ids the
// client draws, IN THE CLIENT'S ORDER.
//
// WHY THIS EXISTS: ZC_DIALOG_NPC_TRADE (3680) carries only the GROUP name, and
// the shipped client looks the items up itself in `datatable_Shop.ies`. Our zone
// has no client tables, and a buy record names an item by its INDEX in that list
// ("slot 3"), not by class id - so without the same list in the same order a buy
// cannot be resolved at all. `src\emit_shop_data.py` writes it
// (`data\shop_groups.json`, 101 groups).
type ShopGroups map[string][]int64

// shopGroupDoc is the emitted JSON: {"count":N,"groups":{"<GroupName>":[id,...]}}.
type shopGroupDoc struct {
	Count  int                `json:"count"`
	Groups map[string][]int64 `json:"groups"`
}

// ShopState is the shop window this session was LAST sent. A buy or a sale
// arrives as an index into it, so it is the only way to know which item the
// player pointed at.
type ShopState struct {
	Group string
	// Kind is "normal" or "custom" - the two windows are answered by different
	// handlers on the client, and their indices mean different things.
	Kind string
	// Items are the class ids in the client's order. Normal shops only: a custom
	// shop carries its own entries instead.
	Items []int64
	// Coin is the coin a NORMAL window charges, taken from the script that opened
	// it. It matters because the buy confirm does not carry the coin: shopType is
	// 0 for a vis shop and a Feso shop alike, so this field is the only thing that
	// says whether the cart is paid in vis (30000) or Feso (30001). The zero value
	// is CoinVis, which is what every shop window did before Feso existed.
	Coin Coin
	// Entries are the custom shop's lines in the order they were SENT, which is
	// the order a buy's index counts in. Never sort it: the index would then
	// name a different line than the one the player clicked.
	Entries []CustomShopEntry
}

// Items is the list a group sells, or nil when the group is unknown. A nil map
// is answered, not panicked on: a zone started without the file holds a nil
// table and still has to answer every click.
func (g ShopGroups) Items(group string) []int64 {
	if g == nil {
		return nil
	}
	return g[group]
}

// Len is how many groups are loaded.
func (g ShopGroups) Len() int {
	return len(g)
}

// LoadShopGroups reads the emitted normal-shop table. A missing file is an
// ERROR, not an empty table: an empty one would leave every shop window with no
// items to resolve a buy against, which looks exactly like a client that sends
// the wrong index.
func LoadShopGroups(path string) (ShopGroups, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shop groups: %w", err)
	}
	var doc shopGroupDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse shop groups %s: %w", path, err)
	}
	if len(doc.Groups) == 0 {
		return nil, fmt.Errorf("shop groups hold no groups; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	return ShopGroups(doc.Groups), nil
}

// customShopDoc is {"count":N,"groups":{"<GroupName>":[[sell,price1,count1,price2,count2],...]}}.
type customShopDoc struct {
	Count  int                   `json:"count"`
	Groups map[string][][5]int64 `json:"groups"`
}

// CustomShops is what a CUSTOM (barter) shop offers: a group name -> its rows in
// the client's order, straight from data\custom_shops.json.
//
// A ROW IS NOT A CustomShopEntry, and copying it by position is the trap: the
// XML's own column order is (sell, price1, count1, price2, count2) - the item
// being bought first, then each payment's item and count - while the wire record
// is (BuyItem, PriceItem0, PriceItem1, Count0, Count1). A positional copy puts
// count1 where price2 belongs and silently swaps the two prices. Entries is the
// ONE place that reorders (Python: src\zone_server.py:2626-2628).
type CustomShops map[string][][5]int64

// Entries is a group's rows as window records, or nil when the group is unknown.
// The ROW ORDER IS KEPT: a buy record's index is a position in this list, and the
// client was sent the window in this order.
func (c CustomShops) Entries(group string) []CustomShopEntry {
	rows := c[group]
	if len(rows) == 0 {
		return nil
	}
	out := make([]CustomShopEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, CustomShopEntry{
			BuyItem:    uint32(row[0]),
			PriceItem0: uint32(row[1]),
			Count0:     uint32(row[2]),
			PriceItem1: uint32(row[3]),
			Count1:     uint32(row[4]),
		})
	}
	return out
}

// Len is how many custom shop groups are loaded.
func (c CustomShops) Len() int {
	return len(c)
}

// LoadCustomShops reads the emitted CUSTOM shop table, which is the opposite
// case to a normal shop: the window packet carries the items and their prices,
// so the server must know them to build the window at all.
//
// A missing file is an ERROR, not an empty table, for the same reason as
// LoadShopGroups: an empty one draws an empty window and leaves every buy with
// nothing to resolve.
func LoadCustomShops(path string) (CustomShops, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read custom shops: %w", err)
	}
	var doc customShopDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse custom shops %s: %w", path, err)
	}
	if len(doc.Groups) == 0 {
		return nil, fmt.Errorf("custom shops hold no groups; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	return CustomShops(doc.Groups), nil
}
