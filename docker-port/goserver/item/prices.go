package item

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Price is what the game's own price table says about one item CLASS.
//
// WHY A CLASS ID -> PRICE TABLE: the client resolves a NORMAL shop's prices
// itself, so the server only has to send the group name. Our zone has no client
// tables, and a buy confirms money and a sale writes a "Sold X for N vis" line,
// so both numbers have to come from somewhere: `src\emit_shop_data.py` writes
// them once (`data\item_prices.json`, 25,448 rows) and this file loads them.
// Same arrangement as the name -> class id catalog, and for the same reason.
type Price struct {
	// Price is what the client charges for one of this item.
	Price int64
	// SellPrice is what a shop pays for one. Items with SellPrice 0 cannot be
	// sold at all.
	SellPrice int64
	// Venderable is the game's own "a shop will buy this back" flag. It is
	// separate from SellPrice: the shipped tables have rows with a price and a
	// zero flag, and the client's shop refuses those.
	Venderable bool
	// MaxStack is the per-slot cap, the same number the catalog carries. It is
	// here because a purchase checks it against the bag row it is filling.
	MaxStack int
	// Name is the item's English name, for the system line a sale prints.
	Name string
}

// Prices is the class id -> price table, loaded once at zone start.
type Prices struct {
	items map[int64]Price
}

// priceDoc is the emitted JSON: one row per class id, a fixed 5-cell array.
//
//	{"count":N,"items":{"<classid>":[price, sell_price, venderable, max_stack, name]}}
//
// Rows are decoded into `any` and read cell by cell, so a row that is short (an
// older file) costs that one item and not the whole table.
type priceDoc struct {
	Count int              `json:"count"`
	Items map[string][]any `json:"items"`
}

// LoadPrices reads the emitted price table. A missing file is an ERROR, not an
// empty table: an empty one prices every item at 0, which reads as "everything
// is free" and would let a buy take any item - the same trap the catalog's
// loader exists to avoid.
func LoadPrices(path string) (*Prices, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read item prices: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(blob))
	// Prices are money: decode numbers as json.Number so a value beyond 2^53
	// cannot arrive as a rounded float.
	dec.UseNumber()
	var doc priceDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse item prices %s: %w", path, err)
	}
	return FromPriceRows(doc.Items)
}

// FromPriceRows builds the table from the decoded rows, so a test can hand it
// three items instead of a file on disk.
func FromPriceRows(rows map[string][]any) (*Prices, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("item prices hold no rows; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	p := &Prices{items: make(map[int64]Price, len(rows))}
	for key, row := range rows {
		classID, err := strconv.ParseInt(key, 10, 64)
		if err != nil || classID == 0 {
			// A key that is not a class id is not something to guess at: the
			// file is keyed by class id, so a bad key means the file is not the
			// one this loader was written for.
			continue
		}
		if len(row) < 3 {
			continue
		}
		price, _ := cellInt(row[0])
		sell, _ := cellInt(row[1])
		flag, _ := cellInt(row[2])
		stacks := 1
		if len(row) > 3 {
			if ms, ok := cellInt(row[3]); ok {
				stacks = int(ms)
			}
		}
		name := ""
		if len(row) > 4 {
			name, _ = row[4].(string)
		}
		p.items[classID] = Price{
			Price:      price,
			SellPrice:  sell,
			Venderable: flag != 0,
			MaxStack:   stacks,
			Name:       name,
		}
	}
	if len(p.items) == 0 {
		return nil, fmt.Errorf("item prices parsed to nothing; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	return p, nil
}

// cellInt reads one number cell. The decoder used json.Number, but a table
// written by another tool may carry a real number, so both are accepted.
func cellInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// Len is how many class ids the table prices.
func (p *Prices) Len() int {
	if p == nil {
		return 0
	}
	return len(p.items)
}

// Get answers what the table says about one class id.
//
// A nil receiver answers false rather than panicking: a zone started without the
// file holds a nil table, and every buy path still has to ask.
func (p *Prices) Get(classID int64) (Price, bool) {
	if p == nil {
		return Price{}, false
	}
	price, ok := p.items[classID]
	return price, ok
}

// FesoPrice is what the game's FESO table says about one item CLASS: what a Feso
// shop charges, what it pays back, and whether it buys the item at all.
//
// WHY A SECOND TABLE: a Feso shop is the same 3680 window as a vis shop, but the
// player pays the Feso item (class 30001) instead of vis (class 30000), and the
// numbers are NOT the vis prices - an item can be sold for one coin and not the
// other at all. So the two tables are kept apart rather than merged: a single Get
// that answered "which coin's price is this" would be the wrong question.
type FesoPrice struct {
	// Price is what a Feso shop charges for one of this item.
	Price int64
	// SellPrice is what a Feso shop pays for one. 0 means it pays nothing.
	SellPrice int64
	// Venderable is the game's own "a Feso shop will buy this back" flag, which is
	// separate from SellPrice - the same split the vis table has.
	Venderable bool
}

// FesoPrices is the class id -> Feso price table, loaded once at zone start.
type FesoPrices struct {
	items map[int64]FesoPrice
}

// fesoPriceDoc is the emitted JSON: one row per class id, a fixed 3-cell array.
//
//	{"count":N,"items":{"<classid>":[feso_price, feso_sell_price, feso_venderable]}}
//
// A class id that is ABSENT is not sold for Feso: the extractor already dropped
// the table's not-for-sale sentinel rows, so absence is the whole rule and there
// is no sentinel value to special-case here. Rows are decoded into `any` and read
// cell by cell, so a short row costs that one item and not the whole table.
type fesoPriceDoc struct {
	Count int              `json:"count"`
	Items map[string][]any `json:"items"`
}

// LoadFesoPrices reads the emitted Feso price table.
//
// A missing file is an ERROR, not an empty table, for the same reason Prices has
// one: an empty table says "no class is sold for Feso", which is indistinguishable
// from a shop whose every item was dropped, and a buy path that read it as free
// would hand the goods over for nothing.
func LoadFesoPrices(path string) (*FesoPrices, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Feso prices: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(blob))
	// These are prices in a coin: decode as json.Number for the same reason the
	// vis table does - a value beyond 2^53 must not arrive rounded.
	dec.UseNumber()
	var doc fesoPriceDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse Feso prices %s: %w", path, err)
	}
	return FromFesoRows(doc.Items)
}

// FromFesoRows builds the table from the decoded rows, so a test can hand it a
// couple of items instead of a file on disk.
func FromFesoRows(rows map[string][]any) (*FesoPrices, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("Feso prices hold no rows; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	p := &FesoPrices{items: make(map[int64]FesoPrice, len(rows))}
	for key, row := range rows {
		classID, err := strconv.ParseInt(key, 10, 64)
		if err != nil || classID == 0 {
			continue
		}
		if len(row) < 3 {
			continue
		}
		price, _ := cellInt(row[0])
		sell, _ := cellInt(row[1])
		flag, _ := cellInt(row[2])
		p.items[classID] = FesoPrice{
			Price:      price,
			SellPrice:  sell,
			Venderable: flag != 0,
		}
	}
	if len(p.items) == 0 {
		return nil, fmt.Errorf("Feso prices parsed to nothing; write them with: " +
			"cd src && python emit_shop_data.py")
	}
	return p, nil
}

// Len is how many class ids the table prices for Feso.
func (p *FesoPrices) Len() int {
	if p == nil {
		return 0
	}
	return len(p.items)
}

// Get answers what the table says about one class id. False means the class is
// NOT sold for Feso, which a buy must treat as a refusal - never as free.
//
// A nil receiver answers false rather than panicking, like Prices.Get.
func (p *FesoPrices) Get(classID int64) (FesoPrice, bool) {
	if p == nil {
		return FesoPrice{}, false
	}
	price, ok := p.items[classID]
	return price, ok
}
