package zone

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strconv"

	"geserver/item"
	"geserver/protocol"
)

// BUY and SELL: the two shop transactions.
//
// PORTED FROM src\zone_server.py (CZ_BUYITEM ~line 4729, CZ_SELLITEM ~line 4845)
// because it is the only implementation that has been run against the live client.
// The behaviour is copied, not reinvented: the same refusals, the same reply order,
// and the same price sources (the item's own row for both directions).
//
// The two packets are NOT the plain "header then records" shape the other zone
// packets have. One unexplained FLAG BYTE sits between the count and the first
// record, and that byte is the trap this file exists to respect: DECISIONS.md
// (2026-10-17) records that reading 13-byte BUY records instead of 12-byte ones
// parses a ONE-item cart correctly and everything after the first item wrongly -
// the flag byte gets counted once per record. The proof is a multi-item cart:
// six items fit in len 93 = 20 + 1 + 6*12, and never in 6*13.
const (
	// BUY: security header (12) + shopType (4) + n (4) + FLAG (1) + n x 12.
	// A record is {type(4) IGNORE, count(4), index(4)}; the first word is a CLIENT
	// POINTER, not an item id, which is why reading it as one finds nothing.
	buyHeader     = 21
	buyRecordSize = 12
	// SELL: security header (12) + n (4) + FLAG (1) + n x 8.
	// A record is {invIndex(4), count(4)}, and invIndex is the 1-based position
	// the client draws - the same key ZC_INVENTORY_ITEM_LIST handed out.
	sellHeader     = 17
	sellRecordSize = 8

	// ShopVis is the shopType a NORMAL shop sends. Its item list and prices live
	// on the CLIENT (datatable_Shop.ies), so the record only names an index and
	// this side prices it. The COIN is NOT in this packet: shopType is 0 for a vis
	// shop and for a Feso shop alike, so the window send is where this side learnt
	// it (ShopState.Coin). shopCustom (1) is the other window: the SERVER sent the
	// prices inside the window itself and the player pays with ITEMS - see
	// buyCustomItem.
	ShopVis    uint32 = 0
	shopCustom uint32 = 1

	// VisClass is g_visItem: money IS an item row (class 30000), never a property.
	// Every vis price is paid out of and into that stack, and the sell path must
	// never sell it back to itself.
	VisClass int64 = 30000
	// FesoClass is the Feso coin item (class 30001). A Feso shop charges it at the
	// item's FESO price, and it is a different row from vis: charging vis for a
	// Feso window would take the wrong coin, and paying vis out of one would be a
	// fee the player never agreed to.
	FesoClass int64 = 30001

	// maxCartRecords is the biggest cart either packet may carry, and maxLineCount
	// the biggest order for one item. Both are the Python oracle's limits
	// (zone_server.py: `n <= 0 or n > 32`, `count <= 0 or count > 99`); the client
	// cannot send more, so a bigger number is a malformed packet, not a big order.
	maxCartRecords = 32
	maxLineCount   = 99
)

// BuyRecord is one cart line: how many of the item at Index.
type BuyRecord struct {
	Count int32
	Index int32 // 0-based position in the shop window's item list
}

// BuyCart is one CZ_BUYITEM, decoded.
type BuyCart struct {
	ShopType uint32
	Count    int // the record count the packet claims
	Records  []BuyRecord
}

// SellRecord is one sale line: how many of the row at the 1-based InvIndex.
type SellRecord struct {
	InvIndex int32 // 1-based position in the list the client was sent
	Count    int32
}

// SellCart is one CZ_SELLITEM, decoded.
type SellCart struct {
	Count   int
	Records []SellRecord
}

// ParseBuyItem reads CZ_BUYITEM:
//
//	cmd(2) len(2) security(8) shopType(4) n(4) flag(1) n x {type(4) count(4) index(4)}
//
// ok is false when the packet is shorter than the records it declares - that is a
// malformed client, never a crash, and the caller answers NOTHING (the player stays
// in the shop rather than losing the cart to a silent half-purchase).
func ParseBuyItem(pt []byte) (BuyCart, bool) {
	var cart BuyCart
	if len(pt) < buyHeader {
		return cart, false
	}
	cart.ShopType = binary.LittleEndian.Uint32(pt[12:])
	cart.Count = int(binary.LittleEndian.Uint32(pt[16:]))
	// pt[20] is the flag byte: nothing reads it, but it is why the records start at
	// 21 and not at 20. Skipped deliberately, not by accident.
	if cart.Count < 0 || buyHeader+cart.Count*buyRecordSize > len(pt) {
		return cart, false
	}
	cart.Records = make([]BuyRecord, 0, cart.Count)
	for i := 0; i < cart.Count; i++ {
		off := buyHeader + i*buyRecordSize
		// +0 is the client's own pointer; it is read by nobody.
		cart.Records = append(cart.Records, BuyRecord{
			Count: int32(binary.LittleEndian.Uint32(pt[off+4:])),
			Index: int32(binary.LittleEndian.Uint32(pt[off+8:])),
		})
	}
	return cart, true
}

// ParseSellItem reads CZ_SELLITEM:
//
//	cmd(2) len(2) security(8) n(4) flag(1) n x {invIndex(4) count(4)}
//
// Then the same flag-byte trap as a buy: records start at 17, not at 16.
func ParseSellItem(pt []byte) (SellCart, bool) {
	var cart SellCart
	if len(pt) < sellHeader {
		return cart, false
	}
	cart.Count = int(binary.LittleEndian.Uint32(pt[12:]))
	// pt[16] is the flag byte (see ParseBuyItem).
	if cart.Count < 0 || sellHeader+cart.Count*sellRecordSize > len(pt) {
		return cart, false
	}
	cart.Records = make([]SellRecord, 0, cart.Count)
	for i := 0; i < cart.Count; i++ {
		off := sellHeader + i*sellRecordSize
		cart.Records = append(cart.Records, SellRecord{
			InvIndex: int32(binary.LittleEndian.Uint32(pt[off:])),
			Count:    int32(binary.LittleEndian.Uint32(pt[off+4:])),
		})
	}
	return cart, true
}

// buyLine is one priced cart line, resolved against the shop window and the price
// table before anything is written. `unit` is what ONE costs in the window's own
// coin (vis or Feso), and `price` is the item's vis row, which is where the stack
// cap and the name live - those are properties of the item, not of the coin.
type buyLine struct {
	classID int64
	count   int64
	unit    int64
	price   item.Price
}

// sellLine is one validated sale line: the bag row it names, and what it pays.
// `price` is the item's VIS row, kept for its NAME only - the amount paid comes
// from whichever table the shop's coin uses (see sellItem), so it is not read for
// money here.
type sellLine struct {
	stack item.Stack
	count int64
	gain  int64
	price item.Price
}

// buyItem answers CZ_BUYITEM for a NORMAL shop.
//
// ONE transaction takes the money and gives the goods: `CommitTx` runs the ops in
// order with row locks, so a cart that cannot be paid for cannot be half-given
// either. Every refusal logs ONE line naming the reason and answers NOTHING - the
// client stays in its shop with its cart intact, which is what the Python oracle
// does (`REJECTED: not enough vis` and friends).
func (h *Handler) buyItem(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	cart, ok := ParseBuyItem(f.Data)
	if !ok {
		h.Log.Printf("zone: BUYITEM refused: %d record(s) do not fit in a %d-byte packet",
			cart.Count, len(f.Data))
		return nil, nil
	}
	if sess.LastShop == nil {
		h.Log.Printf("zone: BUYITEM refused: no shop window was sent this session, "+
			"so index %d names nothing", firstBuyIndex(cart))
		return nil, nil
	}
	if cart.Count < 1 || cart.Count > maxCartRecords {
		h.Log.Printf("zone: BUYITEM refused: %d record(s), a cart carries 1..%d",
			cart.Count, maxCartRecords)
		return nil, nil
	}
	if cart.ShopType == shopCustom {
		// A custom shop is a BARTER: its prices came from the window we sent and
		// it is paid with ITEMS, never with vis, so it is a different transaction
		// with its own packet types (see buyCustomItem).
		return h.buyCustomItem(ctx, sess, cart)
	}
	if cart.ShopType != ShopVis {
		h.Log.Printf("zone: BUYITEM refused: unknown shopType %d (only %d = normal shop "+
			"and %d = custom shop exist)", cart.ShopType, ShopVis, shopCustom)
		return nil, nil
	}
	if sess.Inv == nil {
		h.Log.Printf("zone: BUYITEM refused: no inventory list was sent this session, " +
			"so there is no position to put the goods at")
		return nil, nil
	}

	// WHICH COIN the window takes: the buy packet does not say, so the session's
	// record of the window decides. The coin class and the price table follow from
	// it - and each coin needs ITS OWN table, because a Feso cart priced from the
	// vis table would take the wrong number.
	coin := sess.LastShop.Coin
	coinClass := VisClass
	if coin == CoinFeso {
		coinClass = FesoClass
		if h.FesoPrices == nil {
			// A missing Feso table must not read as "free": without it no line can
			// be priced at all, so the whole cart is refused.
			h.Log.Printf("zone: BUYITEM refused: this window takes Feso but no Feso " +
				"price table is loaded (build it with: cd src && python emit_shop_data.py)")
			return nil, nil
		}
	} else if h.Prices == nil {
		h.Log.Printf("zone: BUYITEM refused: no price table is loaded, so the cart " +
			"cannot be charged (build it with: cd src && python emit_shop_data.py)")
		return nil, nil
	}

	// Price the WHOLE cart before writing anything: a single unresolvable line
	// refuses the purchase, because a partial cart is a different purchase from the
	// one the player confirmed.
	var (
		lines []buyLine
		cost  int64
	)
	for i, rec := range cart.Records {
		if rec.Index < 0 || int(rec.Index) >= len(sess.LastShop.Items) {
			h.Log.Printf("zone: BUYITEM refused: record %d names index %d, and the %q "+
				"window has %d item(s)", i, rec.Index, sess.LastShop.Group,
				len(sess.LastShop.Items))
			return nil, nil
		}
		if rec.Count < 1 || rec.Count > maxLineCount {
			h.Log.Printf("zone: BUYITEM refused: record %d asks for %d of one item, "+
				"and a line is 1..%d", i, rec.Count, maxLineCount)
			return nil, nil
		}
		classID := sess.LastShop.Items[rec.Index]
		unit, meta, known := h.shopUnitPrice(coin, classID)
		if !known {
			h.Log.Printf("zone: BUYITEM refused: class %d (record %d) has no %s price, "+
				"so it cannot be charged", classID, i, coin)
			return nil, nil
		}
		lines = append(lines, buyLine{classID: classID, count: int64(rec.Count),
			unit: unit, price: meta})
		cost += unit * int64(rec.Count)
	}

	stacks, ok := h.shopBag(ctx, sess, "BUYITEM")
	if !ok {
		return nil, nil
	}
	vis, ok := coinRow(stacks, coinClass)
	if !ok {
		h.Log.Printf("zone: BUYITEM refused: commander %d's bag holds no %s row "+
			"(class %d), so the cart cannot be paid for", sess.CommanderID, coin,
			coinClass)
		return nil, nil
	}
	if vis.Count < cost {
		h.Log.Printf("zone: BUYITEM refused: %d item(s) cost %d %s and the bag holds %d",
			len(lines), cost, coin, vis.Count)
		return nil, nil
	}

	ops := make([]item.Op, 0, len(lines)+1)
	// The payment goes FIRST in the transaction: its row is locked before the goods
	// are added, and every op after it is part of the same all-or-nothing commit.
	// ItemID names that exact row, so two vis stacks cannot be confused. The CLASS
	// is the window's coin - 30000 for vis, 30001 for Feso.
	//
	// A price row of 0 makes the item free, and a take of 0 is not a write at all
	// (the oracle subtracts 0 and sends a no-op remove), so it is skipped.
	if cost > 0 {
		ops = append(ops, item.Op{
			Give: false, ClassID: coinClass, Count: cost, ItemID: vis.ItemID,
			Name: coin.String(),
		})
	}
	for _, ln := range lines {
		// Merge/MaxStack are the ITEM TABLE's word (carried by the price row): a
		// stackable item joins its stack, equipment gets its own slot, and a give
		// that would pass the cap is refused by the write path as a FULL INVENTORY.
		ops = append(ops, item.Op{
			Give: true, ClassID: ln.classID, Count: ln.count, Name: ln.price.Name,
			Merge: ln.price.MaxStack > 1, MaxStack: int64(ln.price.MaxStack),
		})
	}
	changes, err := h.Commit(ctx, sess.CommanderID, nil, ops)
	if err != nil {
		// The whole cart rolled back: nothing was paid and nothing was given, so the
		// client is told nothing and its shop stays open.
		h.Log.Printf("zone: BUYITEM by commander %d failed (nothing written): %v",
			sess.CommanderID, err)
		return nil, nil
	}

	var out [][]byte
	// The client hears about the GOODS first and the PAYMENT last, which is the
	// Python oracle's order and the reverse of the transaction's. ADD_ITEM_BUY is
	// what makes the client print its own "... purchased" line, so no ZC_MESSAGE is
	// needed here at all.
	for _, c := range changes {
		if !c.Give {
			continue
		}
		if p := sess.Inv.PacketFor(c, AddItemBuy, RemoveItemBuy); p != nil {
			out = append(out, p)
		}
	}
	for _, c := range changes {
		if c.Give {
			continue
		}
		p := sess.Inv.PacketFor(c, AddItemBuy, RemoveItemBuy)
		if p == nil {
			// Money was taken but the client cannot be told which row: say so, because
			// a silently shrinking purse looks like a client bug from the outside.
			h.Log.Printf("zone: BUYITEM: the client's list has no row for item %d, so "+
				"its new total was NOT sent (the payment still happened)", c.ItemID)
			continue
		}
		out = append(out, p)
	}
	h.Log.Printf("zone: BUYITEM commander %d bought %d item(s) for %d %s, %d packet(s)",
		sess.CommanderID, len(lines), cost, coin, len(out))
	return out, nil
}

// shopUnitPrice is what ONE of a class costs in the window's own coin, plus the
// item's vis row, which is where the stack cap and the name live - those are
// properties of the ITEM, not of the coin, so they come from the vis table even
// in a Feso shop (and that table may be missing; then the cap is simply unknown).
//
// A class the Feso table does not carry is NOT sold for Feso: the extractor
// already dropped the not-for-sale sentinel rows, so absence is the whole rule and
// the answer is false. A buy must treat false as a REFUSAL, never as free.
func (h *Handler) shopUnitPrice(coin Coin, classID int64) (int64, item.Price, bool) {
	meta, _ := h.Prices.Get(classID)
	if coin == CoinFeso {
		feso, known := h.FesoPrices.Get(classID)
		if !known {
			return 0, item.Price{}, false
		}
		return feso.Price, meta, true
	}
	vis, known := h.Prices.Get(classID)
	if !known {
		return 0, item.Price{}, false
	}
	return vis.Price, vis, true
}

// customSlot is one (price item, count per bought item) pair a custom shop row
// offers. The wire record carries two of them.
type customSlot struct {
	classID int64
	unit    int64
}

// customLine is one BARTER cart line that survived resolution: the window row it
// names, how many of the bought item, and which price pair it is paid with.
type customLine struct {
	entry CustomShopEntry
	count int64
	slot  customSlot
}

// customCost is one price class and how many of it the whole cart charges.
type customCost struct {
	classID int64
	count   int64
}

// pickPriceSlot decides which of a row's two (item, count) pairs one barter line
// is paid with.
//
// TODAY'S DATA CARRIES ONE PAIR: every row of data\custom_shops.json has Count1
// = 0 (checked 2026-10-26), so slot 0 is the only answer the game needs. The
// record still has two, and a row that fills both charges DIFFERENT items whose
// vis prices are unrelated, so the pairs cannot be ranked by price. The rule here
// is therefore what the player can actually COVER, cheapest first (the smaller
// requirement). It is deliberately NOT the Python oracle's, which charges BOTH
// pairs at once (src\zone_server.py:4766-4769). When a second pair starts being
// used, this is the line to revisit.
//
// A row with neither pair defined answers false: it names free goods, and
// handing those over for nothing is not what the window offered.
func pickPriceSlot(e CustomShopEntry, per int64, held func(classID int64) int64) (customSlot, bool) {
	var cands []customSlot
	if e.Count0 > 0 && e.PriceItem0 > 0 {
		cands = append(cands, customSlot{classID: int64(e.PriceItem0), unit: int64(e.Count0)})
	}
	if e.Count1 > 0 && e.PriceItem1 > 0 {
		cands = append(cands, customSlot{classID: int64(e.PriceItem1), unit: int64(e.Count1)})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].unit < cands[j].unit })
	for _, c := range cands {
		if held(c.classID) >= c.unit*per {
			return c, true
		}
	}
	return customSlot{}, false
}

// buyCustomItem answers CZ_BUYITEM for a CUSTOM (barter) shop: the player pays
// with ITEMS, never with vis, at the prices our own 3681 window carried.
//
// PORTED FROM src\zone_server.py:4745-4792 (the `shop_type == SHOP_TYPE_CUSTOM`
// branch). ONE transaction takes every price stack and gives every bought stack,
// so a cart the bag cannot pay for changes NOTHING. The reply is ZC_REMOVEITEM
// with removeType 16 (CUSTOM_SHOP) per price row, then ZC_ADDITEM with addType 12
// (CUSTOM_SHOP_BUY) per bought stack; every `count` in those packets is the row's
// NEW TOTAL, which is what the client prints and what its bag then shows.
//
// A custom shop has NO sell side: the client sends no CZ_SELLITEM from this
// window, so nothing here prices a sale.
//
// FESO IS A LATER JOB. A Feso shop (class 30001) is a separate currency with its
// own rules; it is not handled here and must not be treated as vis.
// TODO: implement Feso shops separately, with their own class and price source.
func (h *Handler) buyCustomItem(ctx context.Context, sess *Session, cart BuyCart) ([][]byte, error) {
	shop := sess.LastShop
	if shop.Kind != WindowShopCustom || len(shop.Entries) == 0 {
		// shopType=1 against a normal window (or one whose rows never loaded): the
		// index would name nothing, and pricing it as a normal buy would charge vis
		// the player never agreed to.
		h.Log.Printf("zone: BUYITEM refused: shopType=%d says custom, but the last "+
			"window was %q with %d custom row(s)", cart.ShopType, shop.Kind,
			len(shop.Entries))
		return nil, nil
	}
	if sess.Inv == nil {
		h.Log.Printf("zone: BUYITEM refused: no inventory list was sent this session, " +
			"so there is no position to put the goods at")
		return nil, nil
	}

	stacks, ok := h.shopBag(ctx, sess, "BUYITEM")
	if !ok {
		return nil, nil
	}
	// held is how many of a class the bag really has, summed over its stacks: a
	// player can hold the same price item in two rows.
	held := func(classID int64) int64 {
		var n int64
		for _, st := range stacks {
			if st.ClassID == classID {
				n += st.Count
			}
		}
		return n
	}

	// Resolve the WHOLE cart before writing anything: one line that names nothing,
	// or that the bag cannot pay for, refuses the purchase - a partial barter is a
	// different purchase from the one the player confirmed.
	var (
		lines []customLine
		costs []customCost
	)
	addCost := func(classID, n int64) {
		if n <= 0 {
			return
		}
		for i := range costs {
			if costs[i].classID == classID {
				costs[i].count += n
				return
			}
		}
		costs = append(costs, customCost{classID: classID, count: n})
	}
	for i, rec := range cart.Records {
		if rec.Index < 0 || int(rec.Index) >= len(shop.Entries) {
			h.Log.Printf("zone: BUYITEM refused: record %d names index %d, and the %q "+
				"custom window has %d row(s)", i, rec.Index, shop.Group, len(shop.Entries))
			return nil, nil
		}
		if rec.Count < 1 || rec.Count > maxLineCount {
			h.Log.Printf("zone: BUYITEM refused: record %d asks for %d of one item, "+
				"and a line is 1..%d", i, rec.Count, maxLineCount)
			return nil, nil
		}
		entry := shop.Entries[rec.Index]
		slot, payable := pickPriceSlot(entry, int64(rec.Count), held)
		if !payable {
			h.Log.Printf("zone: BUYITEM refused: record %d (row %d, item %d) charges "+
				"%d x item %d or %d x item %d, and the bag holds %d and %d", i,
				rec.Index, entry.BuyItem, entry.Count0, entry.PriceItem0,
				entry.Count1, entry.PriceItem1, held(int64(entry.PriceItem0)),
				held(int64(entry.PriceItem1)))
			return nil, nil
		}
		lines = append(lines, customLine{entry: entry, count: int64(rec.Count), slot: slot})
		addCost(slot.classID, slot.unit*int64(rec.Count))
	}

	ops := make([]item.Op, 0, len(costs)+len(lines))
	// The price goes FIRST in the transaction, the same way the vis path pays
	// first: the rows being spent are locked before anything is handed over. A
	// price that spans two stacks of one class becomes one take per ROW, named by
	// ItemID, so the client is told about the exact rows that shrank.
	for _, c := range costs {
		name := ""
		if price, known := h.Prices.Get(c.classID); known {
			name = price.Name
		}
		need := c.count
		for _, st := range stacks {
			if need <= 0 {
				break
			}
			if st.ClassID != c.classID || st.Count <= 0 {
				continue
			}
			n := st.Count
			if n > need {
				n = need
			}
			ops = append(ops, item.Op{
				Give: false, ClassID: c.classID, Count: n, ItemID: st.ItemID, Name: name,
			})
			need -= n
		}
		if need > 0 {
			// The affordability check above already covered this, so only a bag
			// that changed under us gets here. Take nothing rather than take less.
			h.Log.Printf("zone: BUYITEM refused: class %d is short by %d after the bag "+
				"read - nothing written", c.classID, need)
			return nil, nil
		}
	}
	for _, ln := range lines {
		// Merge/MaxStack are the ITEM TABLE's word, carried by the price row: a
		// stackable item joins its stack, equipment gets its own slot, and a give
		// that would pass the cap is refused by the write path as a FULL INVENTORY.
		// A class with no price row is still given - the window named it - but it is
		// logged: an unmerged give of something stackable is two slots on the client.
		price, known := h.Prices.Get(int64(ln.entry.BuyItem))
		if !known {
			h.Log.Printf("zone: BUYITEM: item class %d has no price row, so it is given "+
				"as its own slot (its stack rule is unknown)", ln.entry.BuyItem)
		}
		ops = append(ops, item.Op{
			Give: true, ClassID: int64(ln.entry.BuyItem), Count: ln.count,
			Name: price.Name, Merge: price.MaxStack > 1, MaxStack: int64(price.MaxStack),
		})
	}
	changes, err := h.Commit(ctx, sess.CommanderID, nil, ops)
	if err != nil {
		// The whole barter rolled back: nothing was paid and nothing was given, so
		// the client is told nothing and its window stays open.
		h.Log.Printf("zone: BUYITEM barter by commander %d failed (nothing written): %v",
			sess.CommanderID, err)
		return nil, nil
	}

	var (
		out  [][]byte
		adds []item.Change
	)
	// The PRICE ROWS go first, with removeType 16 (CUSTOM_SHOP): that is the type
	// whose handler refreshes the open custom shop's counters. This is the Python
	// oracle's order for a custom buy (removes, then adds) and the reverse of its
	// vis buy, which sends the goods first - copied, not guessed.
	for _, c := range changes {
		if c.Give {
			adds = append(adds, c)
			continue
		}
		p := sess.Inv.PacketFor(c, AddItemCustomShopBuy, RemoveItemCustomShop)
		if p == nil {
			// The price was taken but the client cannot be told which row: say so,
			// because a silently shrinking stock looks like a client bug.
			h.Log.Printf("zone: BUYITEM: the client's list has no row for item %d, so "+
				"its new total was NOT sent (the price was still taken)", c.ItemID)
			continue
		}
		out = append(out, p)
	}
	// Then the GOODS, with addType 12 (CUSTOM_SHOP_BUY): that type makes the client
	// print its own "you received X" line, so no ZC_MESSAGE is needed here.
	for _, c := range adds {
		out = append(out, sess.Inv.PacketFor(c, AddItemCustomShopBuy, RemoveItemCustomShop))
	}
	h.Log.Printf("zone: BUYITEM commander %d bartered %d line(s) for %d price row(s), "+
		"%d packet(s)", sess.CommanderID, len(lines), len(changes)-len(adds), len(out))
	return out, nil
}

// sellItem answers CZ_SELLITEM.
//
// The mirror of a buy: the price is the item's own (never anything the client
// sends), the money is paid into the coin's row, and the goods leave the exact row
// the player pointed at. ONE transaction, so a refused line means nothing at all
// happened.
//
// TWO COINS, ONE CODE PATH - ported from TxSellItem.cpp:151-215, which runs the
// same routine for both and only changes the coin and the price source:
//
//	SHOP_VIS  -> class 30000 (vis)  at the item's ordinary sell price
//	SHOP_FESO -> class 30001 (Feso) at the item's FESO sell price
//
// The coin comes from the SHOP THE SERVER OPENED (Session.LastShop.Coin), never
// from the packet: CZ_SELLITEM carries no shop type at all.
func (h *Handler) sellItem(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	sale, ok := ParseSellItem(f.Data)
	if !ok {
		h.Log.Printf("zone: SELLITEM refused: %d record(s) do not fit in a %d-byte packet",
			sale.Count, len(f.Data))
		return nil, nil
	}
	if sess.Inv == nil {
		h.Log.Printf("zone: SELLITEM refused: no inventory list was sent this session, " +
			"so a position names nothing")
		return nil, nil
	}
	if sale.Count < 1 || sale.Count > maxCartRecords {
		h.Log.Printf("zone: SELLITEM refused: %d record(s), a sale carries 1..%d",
			sale.Count, maxCartRecords)
		return nil, nil
	}

	// WHICH COIN the shop pays in. TxSellItem.cpp:160-169 refuses any shop type
	// that is neither vis nor Feso, and so does this: a coin this side cannot name
	// is a coin it cannot pay.
	coin := CoinVis
	if sess.LastShop != nil {
		coin = sess.LastShop.Coin
	}
	coinClass := VisClass
	switch coin {
	case CoinVis:
		if h.Prices == nil {
			h.Log.Printf("zone: SELLITEM refused: no price table is loaded, so no item " +
				"has a sell price (build it with: cd src && python emit_shop_data.py)")
			return nil, nil
		}
	case CoinFeso:
		coinClass = FesoClass
		if h.FesoPrices == nil {
			h.Log.Printf("zone: SELLITEM refused: this window takes Feso but no Feso " +
				"price table is loaded (build it with: cd src && python emit_shop_data.py)")
			return nil, nil
		}
	default:
		h.Log.Printf("zone: SELLITEM refused: shop coin %d is not one this side can pay "+
			"(only vis and Feso exist)", coin)
		return nil, nil
	}

	stacks, ok := h.shopBag(ctx, sess, "SELLITEM")
	if !ok {
		return nil, nil
	}
	// The mirror holds item IDS only, so the row's class and count come from this
	// one bag read - and a row the bag no longer has is a refusal, not a guess.
	byID := make(map[int64]item.Stack, len(stacks))
	for _, st := range stacks {
		byID[st.ItemID] = st
	}
	purse, hasPurse := coinRow(stacks, coinClass)
	if !hasPurse && coin == CoinVis {
		// A VIS sale has nowhere to pay: the vis row is what the player already owns.
		// (A missing FESO row is different - the give below mints one.)
		h.Log.Printf("zone: SELLITEM refused: commander %d's bag holds no %s row "+
			"(class %d) to pay into", sess.CommanderID, coin, coinClass)
		return nil, nil
	}
	// Where the client draws the money. A row that exists but is not in the client's
	// list could be written and never announced, so the sale is refused rather than
	// paid invisibly. A Feso row that does not exist yet is the other case: the give
	// mints it and ZC_ADDITEM appends it, which is how the client learns its position.
	if hasPurse {
		if _, known := sess.Inv.Index(purse.ItemID); !known {
			h.Log.Printf("zone: SELLITEM refused: the client's list has no row for the "+
				"%s stack, so the gain cannot be shown", coin)
			return nil, nil
		}
	}

	var (
		lines []sellLine
		gain  int64
	)
	for i, rec := range sale.Records {
		id, ok := sess.Inv.IDAt(rec.InvIndex)
		if !ok {
			h.Log.Printf("zone: SELLITEM refused: record %d names position %d, which the "+
				"client's list does not have", i, rec.InvIndex)
			return nil, nil
		}
		st, held := byID[id]
		if !held {
			h.Log.Printf("zone: SELLITEM refused: record %d (position %d, item %d) is not "+
				"in the bag any more", i, rec.InvIndex, id)
			return nil, nil
		}
		if rec.Count < 1 || int64(rec.Count) > st.Count {
			h.Log.Printf("zone: SELLITEM refused: record %d sells %d of item %d, which "+
				"holds %d", i, rec.Count, id, st.Count)
			return nil, nil
		}
		if st.ClassID == VisClass || st.ClassID == FesoClass {
			// Money is not merchandise, in EITHER coin: a vis item and a Feso item
			// can never be sold back to a shop (TxSellItem.cpp:179-183).
			which := "vis"
			if st.ClassID == FesoClass {
				which = "Feso"
			}
			h.Log.Printf("zone: SELLITEM refused: record %d points at the %s item itself",
				i, which)
			return nil, nil
		}
		// CASH ITEMS are refused by the reference too (TxSellItem.cpp:186-190, a
		// lookup in geItemCashTable). This side has NO cash-item table, so there is
		// no check to make here: the honest state is "not enforced", not a rule
		// invented from a table we do not have.

		// The price AND the vendorable flag come from the coin's own table: a Feso
		// shop pays the FESO sell price and reads the FESO venderable flag
		// (TxSellItem.cpp:192-210). A class with no Feso row at all is simply not
		// Feso-sellable - it must never be paid at the vis price.
		var (
			unit int64
			meta item.Price // for the NAME in the log only; see sellLine
		)
		if coin == CoinFeso {
			feso, known := h.FesoPrices.Get(st.ClassID)
			if !known {
				h.Log.Printf("zone: SELLITEM refused: class %d (record %d) has no Feso "+
					"price row, so it is not Feso-sellable", st.ClassID, i)
				return nil, nil
			}
			meta, _ = h.Prices.Get(st.ClassID)
			if feso.SellPrice <= 0 || !feso.Venderable {
				// The reference charges the line and only later, when the TOTAL is 0,
				// leaves the coin return empty and fails the transaction
				// (TxSellItem.cpp:257-272). Refusing the zero-priced LINE here is the
				// same answer for the player, decided before anything is written.
				h.Log.Printf("zone: SELLITEM refused: %s (class %d) is not Feso-sellable "+
					"(Feso sell price %d, Feso venderable %v)",
					priceName(meta, st.ClassID), st.ClassID, feso.SellPrice, feso.Venderable)
				return nil, nil
			}
			unit = feso.SellPrice
		} else {
			price, priced := h.Prices.Get(st.ClassID)
			if !priced {
				h.Log.Printf("zone: SELLITEM refused: class %d (record %d) has no price row, "+
					"so it has no sell price", st.ClassID, i)
				return nil, nil
			}
			if price.SellPrice <= 0 || !price.Venderable {
				// Both halves matter: the tables carry rows with a price and a zero
				// Venderable flag, and the client's own shop refuses those too.
				h.Log.Printf("zone: SELLITEM refused: %s (class %d) is not sellable "+
					"(sell price %d, venderable %v)", priceName(price, st.ClassID),
					st.ClassID, price.SellPrice, price.Venderable)
				return nil, nil
			}
			meta, unit = price, price.SellPrice
		}

		lineGain := unit * int64(rec.Count)
		lines = append(lines, sellLine{
			stack: st, count: int64(rec.Count), gain: lineGain, price: meta,
		})
		gain += lineGain
		// The reference keeps the running total in a DWORD and refuses once it passes
		// INT_MAX (TxSellItem.cpp:212-216): a bigger sum cannot be paid into one stack.
		if gain > math.MaxInt32 {
			h.Log.Printf("zone: SELLITEM refused: the sale totals %d %s, past the %d a "+
				"shop can pay", gain, coin, int64(math.MaxInt32))
			return nil, nil
		}
	}

	ops := make([]item.Op, 0, len(lines)+1)
	// Money FIRST, into the COIN's row (class 30000 for vis, 30001 for Feso): a
	// merge search could grow a different row of that class, and the packet tells
	// the client its own row's new total.
	//
	// A player who holds NO Feso row yet gets one minted: the give names no ItemID
	// when purse is empty and merges by class, and giveItem mints the row when the
	// class is not held (items.go). So a first Feso sale works on an empty purse.
	//
	// NO MaxStack on this op, on purpose. The price table's row for the vis class
	// carries max_stack 1 (checked against data\item_prices.json, 2026-10-26), and a
	// cap of 1 would refuse EVERY sale as a full inventory. A purse is not a stack
	// the client draws: the oracle adds the gain with no cap either.
	ops = append(ops, item.Op{
		Give: true, ClassID: coinClass, Count: gain, Merge: true,
		ItemID: purse.ItemID, Name: coin.String(),
	})
	for _, ln := range lines {
		// Take the EXACT row by item id. A class-based take walks the oldest stack
		// first, and when the player holds the same class twice that is not the row
		// they clicked - see takeRow in items.go.
		ops = append(ops, item.Op{
			Give: false, ClassID: ln.stack.ClassID, Count: ln.count,
			ItemID: ln.stack.ItemID, Name: ln.price.Name,
		})
	}
	changes, err := h.Commit(ctx, sess.CommanderID, nil, ops)
	if err != nil {
		h.Log.Printf("zone: SELLITEM by commander %d failed (nothing written): %v",
			sess.CommanderID, err)
		return nil, nil
	}

	var (
		out   [][]byte
		takes []item.Change
	)
	// The money line goes FIRST as ADD_ITEM_EXCHANGE (5): the Money class prints
	// only for that type, and it prints its own YELLOW "You obtained N Vis."
	// (UI_ALERT_MSG_212, FONT_GET_ITEM). ADD_ITEM_SELL (2) is silent, which is
	// why an earlier version wrote a cyan ZC_MESSAGE instead - the owner asked
	// for the yellow line and no cyan one.
	for _, c := range changes {
		if !c.Give {
			takes = append(takes, c)
			continue
		}
		if p := sess.Inv.PacketFor(c, AddItemExchange, RemoveItemSell); p != nil {
			out = append(out, p)
		}
	}
	if len(takes) != len(lines) {
		// Should be one take per line (each names ONE row). If it ever is not, the
		// messages would be attached to the wrong items, so say so in the log.
		h.Log.Printf("zone: SELLITEM: %d row(s) changed for %d sale line(s) - "+
			"the sold lines are not matched up", len(takes), len(lines))
	}
	// Removes LAST, with removeType=SELL (1). The client prints its own YELLOW
	// "<item> N have been sold." for that type (UI_ALERT_MSG_105), so we send no
	// text of our own. That handler also ends with ui::UpdateData("shop"), so
	// the shop panel rebuilds once the money has changed.
	for _, c := range takes {
		p := sess.Inv.PacketFor(c, AddItemExchange, RemoveItemSell)
		if p == nil {
			h.Log.Printf("zone: SELLITEM: the client's list has no row for item %d, so "+
				"its new total was NOT sent (the item was still taken)", c.ItemID)
			continue
		}
		out = append(out, p)
	}
	h.Log.Printf("zone: SELLITEM commander %d sold %d line(s) for %d %s, %d packet(s)",
		sess.CommanderID, len(lines), gain, coin, len(out))
	return out, nil
}

// shopBag reads the commander's bag rows for a shop transaction. An unwired Bag,
// a session with no commander, or a read error all mean the same thing to the
// caller - nothing may be bought or sold - and the line says which it was. `what`
// is the packet name, so the log points at the refusal's caller.
func (h *Handler) shopBag(ctx context.Context, sess *Session, what string) ([]item.Stack, bool) {
	if h.Bag == nil {
		h.Log.Printf("zone: %s refused: no bag reader is wired", what)
		return nil, false
	}
	if sess.CommanderID == 0 {
		h.Log.Printf("zone: %s refused: account %d has no commander resolved",
			what, sess.AccountID)
		return nil, false
	}
	if h.Commit == nil {
		h.Log.Printf("zone: %s refused: no item write path is wired", what)
		return nil, false
	}
	stacks, err := h.Bag(ctx, sess.CommanderID)
	if err != nil {
		h.Log.Printf("zone: %s refused: could not read commander %d's bag: %v",
			what, sess.CommanderID, err)
		return nil, false
	}
	return stacks, true
}

// coinRow is the bag row a window's money lives in: class 30000 for vis, 30001
// for Feso. The FIRST one wins, which is also the one the entry burst drew first
// on the client.
func coinRow(stacks []item.Stack, classID int64) (item.Stack, bool) {
	for _, st := range stacks {
		if st.ClassID == classID {
			return st, true
		}
	}
	return item.Stack{}, false
}

// priceName is the readable name a system line uses. Two names can share a class
// id, and a row can have no name at all - the oracle falls back to the number
// ("item_prices.name(cls) or str(cls)"), and so does this.
func priceName(price item.Price, classID int64) string {
	if price.Name != "" {
		return price.Name
	}
	return strconv.FormatInt(classID, 10)
}

// groupDigits writes a number with a comma every three digits, the way Python's
// "{:,}" does, because the sale line quotes the money and the oracle prints
// "for 1,100 vis". The client shows this text verbatim.
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)
	neg := len(digits) > 0 && digits[0] == '-'
	if neg {
		digits = digits[1:]
	}
	out := make([]byte, 0, len(digits)+len(digits)/3+1)
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// firstBuyIndex is the first record's index, for a log line written before the
// records could be read. It answers 0 when there is no record at all.
func firstBuyIndex(cart BuyCart) int32 {
	if len(cart.Records) == 0 {
		return 0
	}
	return cart.Records[0].Index
}
