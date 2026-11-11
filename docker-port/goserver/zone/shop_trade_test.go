package zone

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"testing"

	"geserver/item"
	"geserver/protocol"
)

// A fake bag + write path, so the shop paths are tested WITHOUT a database: the
// real `CommitTx` needs PostgreSQL and a commander, and neither is what these tests
// are about. It applies the ops to an in-memory bag using the same two rules the
// real write path does - an op that names an ItemID touches THAT row, one that does
// not is resolved by class (oldest first) - so a wrong row is visible here too.
type fakeShop struct {
	stacks []item.Stack
	nextID int64
	ops    []item.Op
	fail   error
}

func newFakeShop(stacks ...item.Stack) *fakeShop {
	f := &fakeShop{stacks: append([]item.Stack(nil), stacks...)}
	for _, st := range f.stacks {
		if st.ItemID > f.nextID {
			f.nextID = st.ItemID
		}
	}
	return f
}

func (f *fakeShop) commit(_ context.Context, _ int32, _ map[string]int64, ops []item.Op) ([]item.Change, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.ops = append(f.ops, ops...)
	var changes []item.Change
	for _, op := range ops {
		i := f.row(op)
		if op.Give && i < 0 {
			if op.MaxStack > 0 && op.Count > op.MaxStack {
				return nil, fmt.Errorf("give %d in one slot, cap %d (FULL INVENTORY)",
					op.Count, op.MaxStack)
			}
			f.nextID++
			f.stacks = append(f.stacks, item.Stack{
				ItemID: f.nextID, ClassID: op.ClassID, Count: op.Count,
			})
			changes = append(changes, item.Change{
				Give: true, ClassID: op.ClassID, ItemID: f.nextID, Total: op.Count,
			})
			continue
		}
		if i < 0 {
			return nil, fmt.Errorf("no bag row for op %+v", op)
		}
		st := &f.stacks[i]
		if op.Give {
			// The cap is enforced HERE as well, because the real write path refuses a
			// give that would pass it: a test that skipped the rule would pass while
			// the server refused the same op.
			if op.MaxStack > 0 && st.Count+op.Count > op.MaxStack {
				return nil, fmt.Errorf("give %d to item %d: cap %d (FULL INVENTORY)",
					op.Count, st.ItemID, op.MaxStack)
			}
			st.Count += op.Count
			changes = append(changes, item.Change{
				Give: true, ClassID: st.ClassID, ItemID: st.ItemID, Total: st.Count,
			})
			continue
		}
		st.Count -= op.Count
		ch := item.Change{
			Give: false, ClassID: st.ClassID, ItemID: st.ItemID, Total: st.Count,
		}
		if st.Count <= 0 {
			f.stacks = append(f.stacks[:i], f.stacks[i+1:]...)
		}
		changes = append(changes, ch)
	}
	return changes, nil
}

// row is the row an op names: by ItemID when it has one, else the oldest of its
// class, which is the rule the real take uses.
func (f *fakeShop) row(op item.Op) int {
	for i, st := range f.stacks {
		if op.ItemID != 0 {
			if st.ItemID == op.ItemID {
				return i
			}
			continue
		}
		if st.ClassID == op.ClassID {
			return i
		}
	}
	return -1
}

func (f *fakeShop) held(itemID int64) int64 {
	for _, st := range f.stacks {
		if st.ItemID == itemID {
			return st.Count
		}
	}
	return 0
}

// testPrices is four items: a stackable one (Fire stone), an unstackable one, one
// the shop refuses to buy back, and the VIS class itself.
//
// The vis row is copied from data\item_prices.json - `[0, 0, 0, 1, "GigaDeath"]`,
// price 0, sell 0, NOT venderable, max_stack 1 - because that cap of 1 is a trap: a
// sale that carried it as a MaxStack would be refused as a FULL INVENTORY every
// time. Money is never capped, and these tests would fail if it became capped.
func testPrices(t *testing.T) *item.Prices {
	t.Helper()
	prices, err := item.FromPriceRows(map[string][]any{
		"21613": {float64(124200), float64(1100), float64(1), float64(999), "Fire stone"},
		"20022": {float64(500), float64(250), float64(1), float64(1), "Novice Sword"},
		"20023": {float64(700), float64(700), float64(0), float64(1), "Bound Sword"},
		"30000": {float64(0), float64(0), float64(0), float64(1), "GigaDeath"},
		// The classes the SELL tests use, one property each so a refusal names
		// exactly which rule it tripped: an ordinary item, a zero Feso price, a row
		// whose Feso venderable flag is 0, one priced past INT_MAX per unit, and one
		// that is vis-sellable but has NO Feso row at all.
		"21006": {float64(5000), float64(900), float64(1), float64(999), "Sale sword"},
		"21007": {float64(5000), float64(500), float64(1), float64(999), "Zero Feso sword"},
		"21008": {float64(5000), float64(500), float64(1), float64(999), "No Feso vendor"},
		"21009": {float64(5000), float64(500), float64(1), float64(999), "Cap sword"},
		"21010": {float64(5000), float64(500), float64(1), float64(999), "Vis only sword"},
	})
	if err != nil {
		t.Fatalf("FromPriceRows: %v", err)
	}
	return prices
}

// shopHandler wires a handler whose bag and write path are the fake.
func shopHandler(f *fakeShop, prices *item.Prices) *Handler {
	return &Handler{
		Log:    log.New(io.Discard, "", 0),
		Prices: prices,
		Bag: func(context.Context, int32) ([]item.Stack, error) {
			return f.stacks, nil
		},
		Commit: f.commit,
	}
}

// testFesoPrices is the FESO table for the two classes the Feso tests buy: Fire
// stone at 700 Feso, the sword at 9000. 20023 (Bound Sword) is deliberately ABSENT -
// a class the table does not carry is not sold for Feso, and a buy must refuse it
// rather than fall back to the vis price.
func testFesoPrices(t *testing.T) *item.FesoPrices {
	t.Helper()
	prices, err := item.FromFesoRows(map[string][]any{
		"21613": {float64(700), float64(350), float64(1)},
		"20022": {float64(9000), float64(4000), float64(1)},
		// The SELL side. 21008 has a Feso price but a 0 Feso venderable flag, 21007
		// a 0 sell price, and 21009 a sell price that passes INT_MAX at count 2.
		// 21010 is deliberately ABSENT: it is vis-sellable and NOT Feso-sellable.
		"21006": {float64(100), float64(40), float64(1)},
		"21007": {float64(100), float64(0), float64(1)},
		"21008": {float64(100), float64(60), float64(0)},
		"21009": {float64(100), float64(2000000000), float64(1)},
	})
	if err != nil {
		t.Fatalf("FromFesoRows: %v", err)
	}
	return prices
}

// TestBuyItemFesoChargesFeso is a Feso shop: the same window, the same buy packet
// (shopType 0), the same reply types - but the money is the FESO row (class 30001)
// at the item's FESO price, and the vis row is not touched at all.
func TestBuyItemFesoChargesFeso(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 1_000_000},
		item.Stack{ItemID: 301, ClassID: FesoClass, Count: 10_000},
	)
	h := shopHandler(f, testPrices(t))
	h.FesoPrices = testFesoPrices(t)
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop: &ShopState{Group: "FESO_ITEMSHOP_POT", Kind: WindowShop,
			Coin: CoinFeso, Items: []int64{21613}},
	}

	out, err := h.buyItem(context.Background(), sess, frameBuy(ShopVis, 0,
		BuyRecord{Count: 2, Index: 0})) // 2 x 700 Feso, not 2 x 124200 vis
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}

	const cost = 2 * 700
	if len(f.ops) != 2 {
		t.Fatalf("%d op(s), want 2 (payment + goods): %+v", len(f.ops), f.ops)
	}
	if f.ops[0].Give || f.ops[0].ClassID != FesoClass || f.ops[0].ItemID != 301 ||
		f.ops[0].Count != cost {
		t.Fatalf("op 0 = %+v, want %d out of the Feso row 301", f.ops[0], cost)
	}
	if !f.ops[1].Give || f.ops[1].ClassID != 21613 || f.ops[1].Count != 2 {
		t.Fatalf("op 1 = %+v, want 2 x 21613 given", f.ops[1])
	}

	// The reply types are the NORMAL ones: 1 for the goods, 2 for the payment.
	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2: %d", len(out), len(out))
	}
	if out[0][25] != AddItemBuy {
		t.Fatalf("addType = %d, want ADD_ITEM_BUY (1)", out[0][25])
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCRemoveItem {
		t.Fatalf("packet 1 cmd = %d, want ZC_REMOVEITEM (the Feso payment)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[1][2:]); got != 2 {
		t.Fatalf("the Feso row was announced at position %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(out[1][6:]); got != 10_000-cost {
		t.Fatalf("the Feso total = %d, want %d", got, 10_000-cost)
	}
	if out[1][10] != RemoveItemBuy {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_BUY (2)", out[1][10])
	}

	// The vis row must be untouched: taking vis for a Feso window is free money.
	if f.held(101) != 1_000_000 {
		t.Fatalf("the vis row holds %d, want 1000000 untouched", f.held(101))
	}
	if f.held(301) != 10_000-cost {
		t.Fatalf("the Feso row holds %d, want %d", f.held(301), 10_000-cost)
	}
}

// TestBuyItemFesoRefusals: a Feso cart is refused when it cannot be priced or paid
// for, and a refusal writes NOTHING - the same rule the vis path follows.
func TestBuyItemFesoRefusals(t *testing.T) {
	cases := []struct {
		name   string
		stacks []item.Stack
		feso   bool // load the Feso table?
		items  []int64
	}{
		{
			// 20023 is in the VIS table (700 vis) but absent from the Feso one. Falling
			// back to the vis price would charge the wrong coin; the answer is a refusal.
			name:   "the class has no Feso price",
			stacks: []item.Stack{{ItemID: 301, ClassID: FesoClass, Count: 10_000}},
			feso:   true,
			items:  []int64{20023},
		},
		{
			name:   "the Feso row is short",
			stacks: []item.Stack{{ItemID: 301, ClassID: FesoClass, Count: 100}},
			feso:   true,
			items:  []int64{21613},
		},
		{
			name:   "no Feso table is loaded",
			stacks: []item.Stack{{ItemID: 301, ClassID: FesoClass, Count: 10_000}},
			feso:   false,
			items:  []int64{21613},
		},
		{
			// A Feso window the player cannot pay because the bag holds no Feso at all,
			// even though it is full of vis.
			name:   "no Feso row in the bag",
			stacks: []item.Stack{{ItemID: 101, ClassID: VisClass, Count: 1_000_000}},
			feso:   true,
			items:  []int64{21613},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeShop(c.stacks...)
			h := shopHandler(f, testPrices(t))
			if c.feso {
				h.FesoPrices = testFesoPrices(t)
			}
			sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks),
				LastShop: &ShopState{Group: "FESO_ITEMSHOP_POT", Kind: WindowShop,
					Coin: CoinFeso, Items: c.items}}

			out, err := h.buyItem(context.Background(), sess, frameBuy(ShopVis, 0,
				BuyRecord{Count: 1, Index: 0}))
			if err != nil {
				t.Fatalf("buyItem: %v", err)
			}
			if len(out) != 0 || len(f.ops) != 0 {
				t.Fatalf("%d op(s) and %d packet(s) for a refused Feso cart, want 0 and 0",
					len(f.ops), len(out))
			}
		})
	}
}

// TestSellItemFesoPaysFeso is a sale inside a FESO shop: the item leaves, and the
// money is paid into the FESO row (class 30001) at the item's FESO sell price -
// never into vis. The reply packets are the ones a vis sale already sends, because
// TxSellItem.cpp runs the same code for both coins.
func TestSellItemFesoPaysFeso(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 1000},
		item.Stack{ItemID: 301, ClassID: FesoClass, Count: 5000},
		item.Stack{ItemID: 201, ClassID: 21006, Count: 5},
	)
	h := shopHandler(f, testPrices(t))
	h.FesoPrices = testFesoPrices(t)
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks),
		LastShop: &ShopState{Group: "FESO_ITEMSHOP", Kind: WindowShop, Coin: CoinFeso}}

	out, err := h.sellItem(context.Background(), sess, frameSell(0,
		SellRecord{InvIndex: 3, Count: 3})) // 3 x 40 Feso
	if err != nil {
		t.Fatalf("sellItem: %v", err)
	}

	const gain = 3 * 40
	if len(f.ops) != 2 {
		t.Fatalf("%d op(s), want 2 (money in, item out): %+v", len(f.ops), f.ops)
	}
	if !f.ops[0].Give || f.ops[0].ClassID != FesoClass || f.ops[0].ItemID != 301 ||
		f.ops[0].Count != gain {
		t.Fatalf("op 0 = %+v, want %d into the exact Feso row 301", f.ops[0], gain)
	}
	if f.ops[1].Give || f.ops[1].ItemID != 201 || f.ops[1].Count != 3 {
		t.Fatalf("op 1 = %+v, want 3 out of the exact sold row 201", f.ops[1])
	}

	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (the money, then the sold row)", len(out))
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != uint32(FesoClass) {
		t.Fatalf("packet 0 class = %d, want %d", got, FesoClass)
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 2 {
		t.Fatalf("packet 0 position = %d, want 2 (the Feso row)", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 5000+gain {
		t.Fatalf("packet 0 count = %d, want the NEW TOTAL %d", got, 5000+gain)
	}
	if out[0][25] != AddItemExchange {
		t.Fatalf("addType = %d, want ADD_ITEM_EXCHANGE", out[0][25])
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCRemoveItem || out[1][10] != RemoveItemSell {
		t.Fatalf("packet 1 = cmd %d type %d, want ZC_REMOVEITEM with REMOVE_ITEM_SELL",
			cmdOf(out[1]), out[1][10])
	}
	if f.held(101) != 1000 {
		t.Fatalf("the vis row holds %d, want 1000: a Feso sale must not pay vis",
			f.held(101))
	}
	if f.held(201) != 2 {
		t.Fatalf("the sold row holds %d, want 2", f.held(201))
	}
}

// TestSellItemFesoMintsThePurse: a player with NO Feso row at all can still sell.
// The give names no row (ItemID 0) and merges by class, so giveItem mints one, and
// ZC_ADDITEM's appended position is how the client learns where it went.
func TestSellItemFesoMintsThePurse(t *testing.T) {
	f := newFakeShop(item.Stack{ItemID: 201, ClassID: 21006, Count: 5})
	h := shopHandler(f, testPrices(t))
	h.FesoPrices = testFesoPrices(t)
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks),
		LastShop: &ShopState{Group: "FESO_ITEMSHOP", Kind: WindowShop, Coin: CoinFeso}}

	out, err := h.sellItem(context.Background(), sess, frameSell(0,
		SellRecord{InvIndex: 1, Count: 2}))
	if err != nil {
		t.Fatalf("sellItem: %v", err)
	}
	if len(f.ops) != 2 || !f.ops[0].Give || f.ops[0].ClassID != FesoClass {
		t.Fatalf("ops = %+v, want the Feso give first", f.ops)
	}
	if f.ops[0].ItemID != 0 || !f.ops[0].Merge {
		t.Fatalf("op 0 = %+v, want a merging give with no row named (it mints one)",
			f.ops[0])
	}
	if f.held(202) != 2*40 {
		t.Fatalf("the minted Feso row holds %d, want %d", f.held(202), 2*40)
	}
	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2", len(out))
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 2 {
		t.Fatalf("the minted row was announced at position %d, want 2", got)
	}
}

// TestSellItemRefusalsBothCoins is TxSellItem.cpp:179-216 line by line: the coin
// items are never merchandise, a Feso shop pays only Feso-sellable things at a
// non-zero Feso price, and a total past INT_MAX is refused. Every case writes
// NOTHING.
func TestSellItemRefusalsBothCoins(t *testing.T) {
	// bag builds a sale-sized bag: the shop's coin row first, then the item.
	bag := func(coinClass, class int64, count int64) []item.Stack {
		return []item.Stack{
			{ItemID: 301, ClassID: coinClass, Count: 10_000},
			{ItemID: 201, ClassID: class, Count: count},
		}
	}
	cases := []struct {
		name  string
		coin  Coin
		row   int32 // which bag position the sale names
		class int64 // the class at that position
		count int32
	}{
		// The flag and the price come from the coin's OWN table.
		{"a non-Feso-vendorable item", CoinFeso, 2, 21008, 1},
		{"an item with no Feso row", CoinFeso, 2, 21010, 1},
		{"a zero Feso sell price", CoinFeso, 2, 21007, 1},
		// Money is not merchandise, in either shop.
		{"the Feso item in a Feso shop", CoinFeso, 1, FesoClass, 1},
		{"the Feso item in a vis shop", CoinVis, 1, FesoClass, 1},
		{"the vis item in a vis shop", CoinVis, 1, VisClass, 1},
		{"the vis item in a Feso shop", CoinFeso, 1, VisClass, 1},
		// 2 x 2,000,000,000 passes INT_MAX, which the reference refuses.
		{"a total past INT_MAX", CoinFeso, 2, 21009, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stacks := bag(VisClass, c.class, 5)
			if c.coin == CoinFeso {
				stacks[0].ClassID = FesoClass
			}
			f := newFakeShop(stacks...)
			h := shopHandler(f, testPrices(t))
			h.FesoPrices = testFesoPrices(t)
			sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks),
				LastShop: &ShopState{Group: "SHOP", Kind: WindowShop, Coin: c.coin}}

			out, err := h.sellItem(context.Background(), sess, frameSell(0,
				SellRecord{InvIndex: c.row, Count: c.count}))
			if err != nil {
				t.Fatalf("sellItem: %v", err)
			}
			if len(out) != 0 || len(f.ops) != 0 {
				t.Fatalf("%d op(s) and %d packet(s) for a refused sale, want 0 and 0",
					len(f.ops), len(out))
			}
		})
	}
}

func appendU32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

// frameBuy is CZ_BUYITEM as the client sends it: the command and security header
// (10 bytes) plus the two bytes that make the header 12, then shopType, the record
// count, ONE FLAG BYTE, and the records. The flag is a real argument so a test can
// prove the parser SKIPS it: a 13-byte record reading only works while it is 0.
func frameBuy(shopType uint32, flag byte, recs ...BuyRecord) protocol.Frame {
	body := []byte{0, 0} // offsets 10..11: the rest of the 12-byte header
	body = appendU32(body, shopType)
	body = appendU32(body, uint32(len(recs)))
	body = append(body, flag)
	for _, r := range recs {
		body = appendU32(body, 0xdeadbeef) // the client pointer: read by nobody
		body = appendU32(body, uint32(r.Count))
		body = appendU32(body, uint32(r.Index))
	}
	pt := pack(CmdCZBuyItem, body...)
	return protocol.Frame{Cmd: CmdCZBuyItem, Size: len(pt), Data: pt}
}

// frameSell is CZ_SELLITEM: the same 12-byte header, the count, ONE FLAG BYTE, then
// {invIndex, count} records.
func frameSell(flag byte, recs ...SellRecord) protocol.Frame {
	body := []byte{0, 0}
	body = appendU32(body, uint32(len(recs)))
	body = append(body, flag)
	for _, r := range recs {
		body = appendU32(body, uint32(r.InvIndex))
		body = appendU32(body, uint32(r.Count))
	}
	pt := pack(CmdCZSellItem, body...)
	return protocol.Frame{Cmd: CmdCZSellItem, Size: len(pt), Data: pt}
}

// TestParseBuyItemFlagByte is the trap DECISIONS.md records as settled: the records
// start at 21, NOT at 20, so a 13-byte per-record read parses one item and then
// slides. The flag byte is non-zero here on purpose - the live client's value is not
// proven, and the parser must ignore it whatever it is.
func TestParseBuyItemFlagByte(t *testing.T) {
	cart, ok := ParseBuyItem(frameBuy(ShopVis, 0x7f,
		BuyRecord{Count: 2, Index: 0},
		BuyRecord{Count: 1, Index: 3},
	).Data)
	if !ok {
		t.Fatal("a 2-record cart was refused")
	}
	if cart.ShopType != ShopVis || cart.Count != 2 || len(cart.Records) != 2 {
		t.Fatalf("cart = %+v, want 2 records of a normal shop", cart)
	}
	if cart.Records[1] != (BuyRecord{Count: 1, Index: 3}) {
		t.Fatalf("record 1 = %+v, want {1 3}: the flag byte was not skipped",
			cart.Records[1])
	}
}

// TestBuyItemChargesAndDelivers: a two-line cart is priced from the price table,
// paid out of the vis row, and the client is told about the GOODS first and the
// PAYMENT last, with the shop packet types.
func TestBuyItemChargesAndDelivers(t *testing.T) {
	// The bag: the vis row (item 101) and nothing else.
	f := newFakeShop(item.Stack{ItemID: 101, ClassID: VisClass, Count: 1_000_000})
	h := shopHandler(f, testPrices(t))
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop:    &ShopState{Group: "ETC_LEONARDO", Kind: WindowShop, Items: []int64{21613, 20022}},
	}

	out, err := h.buyItem(context.Background(), sess, frameBuy(ShopVis, 0,
		BuyRecord{Count: 2, Index: 0}, // 2 x 124200
		BuyRecord{Count: 1, Index: 1}, // 1 x 500
	))
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}

	const cost = 2*124200 + 500
	// The transaction paid first and gave second.
	if len(f.ops) != 3 {
		t.Fatalf("%d op(s), want 3 (payment + 2 goods): %+v", len(f.ops), f.ops)
	}
	if f.ops[0].Give || f.ops[0].ItemID != 101 || f.ops[0].Count != cost {
		t.Fatalf("op 0 = %+v, want the payment of %d out of item 101", f.ops[0], cost)
	}
	if !f.ops[1].Give || !f.ops[1].Merge || f.ops[1].MaxStack != 999 {
		t.Fatalf("op 1 = %+v, want a merging give with the price row's cap", f.ops[1])
	}

	// The reply: two ADD_ITEM_BUY, then ONE REMOVE_ITEM_BUY for the money.
	if len(out) != 3 {
		t.Fatalf("%d packet(s), want 3: %v", len(out), len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCAddItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_ADDITEM", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != 21613 {
		t.Fatalf("packet 0 class = %d, want 21613", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 2 {
		t.Fatalf("packet 0 position = %d, want 2 (appended after the vis row)", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 2 {
		t.Fatalf("packet 0 count = %d, want the NEW TOTAL 2", got)
	}
	if out[0][25] != AddItemBuy {
		t.Fatalf("packet 0 addType = %d, want ADD_ITEM_BUY", out[0][25])
	}
	if got := binary.LittleEndian.Uint32(out[1][8:]); got != 3 {
		t.Fatalf("packet 1 position = %d, want 3", got)
	}
	if cmd := cmdOf(out[2]); cmd != CmdZCRemoveItem {
		t.Fatalf("last packet cmd = %d, want ZC_REMOVEITEM (the payment)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[2][2:]); got != 1 {
		t.Fatalf("the payment was announced at position %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(out[2][6:]); got != 1_000_000-cost {
		t.Fatalf("the vis total = %d, want %d", got, 1_000_000-cost)
	}
	if out[2][10] != RemoveItemBuy {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_BUY", out[2][10])
	}

	// And the mirror now knows the two new rows, so a later sale can name them.
	// The fake mints from the highest id it holds (101), so the two new rows are
	// 102 and 103 - the POSITIONS are what the client keys on.
	if id, ok := sess.Inv.IDAt(2); !ok || id != 102 {
		t.Fatalf("mirror position 2 = %d, %v; want the first minted id 102", id, ok)
	}
	if id, ok := sess.Inv.IDAt(1); !ok || id != 101 {
		t.Fatalf("mirror position 1 = %d, %v; want the vis row 101", id, ok)
	}
}

// TestBuyItemRefusesWhenVisIsShort: a cart the purse cannot cover writes NOTHING and
// answers NOTHING - the client keeps its shop and its cart.
func TestBuyItemRefusesWhenVisIsShort(t *testing.T) {
	f := newFakeShop(item.Stack{ItemID: 101, ClassID: VisClass, Count: 1000})
	h := shopHandler(f, testPrices(t))
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop:    &ShopState{Group: "ETC_LEONARDO", Kind: WindowShop, Items: []int64{21613}},
	}

	out, err := h.buyItem(context.Background(), sess, frameBuy(ShopVis, 0,
		BuyRecord{Count: 1, Index: 0})) // 124200 vis, the bag holds 1000
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) for an unaffordable cart, want 0", len(out))
	}
	if len(f.ops) != 0 {
		t.Fatalf("%d op(s) reached the write path, want 0: %+v", len(f.ops), f.ops)
	}
	if f.held(101) != 1000 {
		t.Fatalf("the vis row holds %d, want 1000 untouched", f.held(101))
	}
}

// TestBuyCustomItemBartersItems: a CUSTOM shop is paid with ITEMS, never with vis.
// The price row and the goods move in the SAME transaction, and the reply is
// ZC_REMOVEITEM with removeType 16 (CUSTOM_SHOP) for the price, then ZC_ADDITEM
// with addType 12 (CUSTOM_SHOP_BUY) for the goods - each carrying its NEW TOTAL.
// The vis row is asserted untouched: a barter that reached the purse would be a
// free item.
func TestBuyCustomItemBartersItems(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 1000},
		item.Stack{ItemID: 201, ClassID: 21450, Count: 10},
	)
	h := shopHandler(f, testPrices(t))
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop: &ShopState{Group: "CLAUD", Kind: WindowShopCustom,
			Entries: []CustomShopEntry{{BuyItem: 21613, PriceItem0: 21450, Count0: 2}}},
	}

	out, err := h.buyItem(context.Background(), sess, frameBuy(shopCustom, 0,
		BuyRecord{Count: 1, Index: 0}))
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}

	// One take of the price, then one give of the goods.
	if len(f.ops) != 2 {
		t.Fatalf("%d op(s), want 2 (price then goods): %+v", len(f.ops), f.ops)
	}
	if f.ops[0].Give || f.ops[0].ItemID != 201 || f.ops[0].Count != 2 {
		t.Fatalf("op 0 = %+v, want 2 out of the exact price row 201", f.ops[0])
	}
	if !f.ops[1].Give || f.ops[1].ClassID != 21613 || f.ops[1].Count != 1 {
		t.Fatalf("op 1 = %+v, want 1 x 21613 given", f.ops[1])
	}

	// The price row is announced first, then the goods.
	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (price, then goods): %d", len(out), len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCRemoveItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_REMOVEITEM (the price)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != 2 {
		t.Fatalf("the price row was announced at position %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 8 {
		t.Fatalf("the price remainder = %d, want the NEW TOTAL 8", got)
	}
	if out[0][10] != RemoveItemCustomShop {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_CUSTOM_SHOP (16)", out[0][10])
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCAddItem {
		t.Fatalf("packet 1 cmd = %d, want ZC_ADDITEM (the goods)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[1][4:]); got != 21613 {
		t.Fatalf("packet 1 class = %d, want 21613", got)
	}
	if got := binary.LittleEndian.Uint32(out[1][8:]); got != 3 {
		t.Fatalf("packet 1 position = %d, want 3 (appended after the two bag rows)", got)
	}
	if got := binary.LittleEndian.Uint32(out[1][12:]); got != 1 {
		t.Fatalf("packet 1 count = %d, want the NEW TOTAL 1", got)
	}
	if out[1][25] != AddItemCustomShopBuy {
		t.Fatalf("addType = %d, want ADD_ITEM_CUSTOM_SHOP_BUY (12)", out[1][25])
	}

	if f.held(101) != 1000 {
		t.Fatalf("the vis row holds %d, want 1000 - a barter never pays with vis",
			f.held(101))
	}
	if f.held(201) != 8 {
		t.Fatalf("the price row holds %d, want 8", f.held(201))
	}
}

// TestBuyCustomItemRefusals: every one of these changes NOTHING and answers
// NOTHING, so the window stays open with the player's cart intact.
func TestBuyCustomItemRefusals(t *testing.T) {
	paid := []item.Stack{{ItemID: 201, ClassID: 21450, Count: 10}}
	single := func() *ShopState {
		return &ShopState{Group: "CLAUD", Kind: WindowShopCustom,
			Entries: []CustomShopEntry{{BuyItem: 21613, PriceItem0: 21450, Count0: 2}}}
	}
	cases := []struct {
		name   string
		stacks []item.Stack
		shop   *ShopState
		rec    BuyRecord
	}{
		{
			name:   "the price item is short",
			stacks: []item.Stack{{ItemID: 201, ClassID: 21450, Count: 1}},
			shop:   single(),
			rec:    BuyRecord{Count: 1, Index: 0},
		},
		{
			name:   "the index names no row",
			stacks: paid,
			shop:   single(),
			rec:    BuyRecord{Count: 1, Index: 4},
		},
		{
			name:   "shopType 1 after a normal window",
			stacks: paid,
			shop: &ShopState{Group: "ETC_LEONARDO", Kind: WindowShop,
				Items: []int64{21613}},
			rec: BuyRecord{Count: 1, Index: 0},
		},
		{
			name:   "the row charges nothing",
			stacks: paid,
			shop: &ShopState{Group: "CLAUD", Kind: WindowShopCustom,
				Entries: []CustomShopEntry{{BuyItem: 21613}}},
			rec: BuyRecord{Count: 1, Index: 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeShop(c.stacks...)
			h := shopHandler(f, testPrices(t))
			sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks),
				LastShop: c.shop}

			out, err := h.buyItem(context.Background(), sess, frameBuy(shopCustom, 0, c.rec))
			if err != nil {
				t.Fatalf("buyItem: %v", err)
			}
			if len(out) != 0 || len(f.ops) != 0 {
				t.Fatalf("%d op(s) and %d packet(s) for a refused barter, want 0 and 0",
					len(f.ops), len(out))
			}
		})
	}
}

// TestBuyItemRefusesAnUnknownShopType pins the shopType gate: 0 is a normal shop,
// 1 is a custom (barter) shop, and anything else is a malformed client - answering
// it with the vis path would move money nobody agreed to.
func TestBuyItemRefusesAnUnknownShopType(t *testing.T) {
	f := newFakeShop(item.Stack{ItemID: 101, ClassID: VisClass, Count: 1_000_000})
	h := shopHandler(f, testPrices(t))
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop:    &ShopState{Group: "ETC_LEONARDO", Kind: WindowShop, Items: []int64{21613}},
	}

	out, err := h.buyItem(context.Background(), sess, frameBuy(2, 0,
		BuyRecord{Count: 1, Index: 0}))
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}
	if len(out) != 0 || len(f.ops) != 0 {
		t.Fatalf("shopType 2 wrote %d op(s) and sent %d packet(s), want 0 and 0",
			len(f.ops), len(out))
	}
}

// TestShortShopPacketsAreRefused: a count that does not fit the frame must not be
// indexed past. Every case answers nothing and writes nothing.
func TestShortShopPacketsAreRefused(t *testing.T) {
	f := newFakeShop(item.Stack{ItemID: 101, ClassID: VisClass, Count: 1_000_000})
	h := shopHandler(f, testPrices(t))
	sess := &Session{
		CommanderID: 42,
		Inv:         NewInvMirror(f.stacks),
		LastShop:    &ShopState{Group: "ETC_LEONARDO", Kind: WindowShop, Items: []int64{21613}},
	}

	// A BUY that claims two records but carries one, and a SELL that claims two and
	// carries none. Both are built by hand: the frame builders always agree with
	// themselves.
	buy := frameBuy(ShopVis, 0, BuyRecord{Count: 1, Index: 0})
	buy.Data[16] = 2 // the count says 2, the bytes say 1
	buy.Size = len(buy.Data)

	sell := frameSell(0)
	sell.Data[12] = 2 // the count says 2, the bytes say 0
	sell.Size = len(sell.Data)

	for _, f2 := range []struct {
		name string
		fn   func(context.Context, *Session, protocol.Frame) ([][]byte, error)
		pkt  protocol.Frame
	}{
		{"buy", h.buyItem, buy},
		{"sell", h.sellItem, sell},
	} {
		out, err := f2.fn(context.Background(), sess, f2.pkt)
		if err != nil {
			t.Fatalf("%s: %v", f2.name, err)
		}
		if len(out) != 0 {
			t.Fatalf("%s: %d packet(s) for a short packet, want 0", f2.name, len(out))
		}
	}
	if len(f.ops) != 0 {
		t.Fatalf("%d op(s) from short packets, want 0: %+v", len(f.ops), f.ops)
	}
}

// TestSellItemReplyOrder: money first (ADD_ITEM_EXCHANGE, which is the one add
// type the Money class prints - a YELLOW "You obtained N Vis."), then the sold row
// (REMOVE_ITEM_SELL, which the client prints as a YELLOW "<item> N have been sold.").
// There is NO ZC_MESSAGE of ours: the owner asked for the cyan line to go and the
// client's own yellow lines to speak instead.
// The remove is deliberately LAST because its handler rebuilds the shop panel, so
// the panel is redrawn after the money has changed.
func TestSellItemReplyOrder(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 1000},
		item.Stack{ItemID: 201, ClassID: 21613, Count: 5},
	)
	h := shopHandler(f, testPrices(t))
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks)}

	out, err := h.sellItem(context.Background(), sess, frameSell(0,
		SellRecord{InvIndex: 2, Count: 3}))
	if err != nil {
		t.Fatalf("sellItem: %v", err)
	}

	const gain = 3 * 1100
	if len(f.ops) != 2 {
		t.Fatalf("%d op(s), want 2 (money in, item out): %+v", len(f.ops), f.ops)
	}
	if !f.ops[0].Give || f.ops[0].ItemID != 101 || f.ops[0].Count != gain {
		t.Fatalf("op 0 = %+v, want %d vis into the exact vis row 101", f.ops[0], gain)
	}
	if f.ops[1].Give || f.ops[1].ItemID != 201 || f.ops[1].Count != 3 {
		t.Fatalf("op 1 = %+v, want 3 out of the exact sold row 201", f.ops[1])
	}

	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (money, then the sold row; no message): %d",
			len(out), len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCAddItem {
		t.Fatalf("packet 0 cmd = %d, want ZC_ADDITEM (the money)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != uint32(VisClass) {
		t.Fatalf("packet 0 class = %d, want %d", got, VisClass)
	}
	if got := binary.LittleEndian.Uint32(out[0][8:]); got != 1 {
		t.Fatalf("packet 0 position = %d, want 1 (the vis row)", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 1000+gain {
		t.Fatalf("packet 0 count = %d, want the NEW TOTAL %d", got, 1000+gain)
	}
	if out[0][25] != AddItemExchange {
		t.Fatalf("addType = %d, want ADD_ITEM_EXCHANGE (the Money class prints only for it)",
			out[0][25])
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCRemoveItem {
		t.Fatalf("packet 1 cmd = %d, want ZC_REMOVEITEM (the sold row)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[1][2:]); got != 2 {
		t.Fatalf("the sold row was announced at position %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(out[1][6:]); got != 2 {
		t.Fatalf("the remainder = %d, want 2", got)
	}
	if out[1][10] != RemoveItemSell {
		t.Fatalf("removeType = %d, want REMOVE_ITEM_SELL", out[1][10])
	}

	// The mirror follows the packets: the sold row kept its position and its
	// remainder, and the vis row is still where the client draws it.
	if id, ok := sess.Inv.IDAt(2); !ok || id != 201 {
		t.Fatalf("mirror position 2 = %d, %v; want the sold row 201 with 2 left", id, ok)
	}
	if f.held(201) != 2 {
		t.Fatalf("the bag row holds %d, want 2", f.held(201))
	}
}

// TestSellItemTakesTheExactRow is why the by-item-id take exists: the client sells
// the row it shows, and `takeItem` by CLASS would have walked to the oldest stack of
// that class instead - leaving the wrong count on screen.
func TestSellItemTakesTheExactRow(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 0},
		item.Stack{ItemID: 201, ClassID: 21613, Count: 5},
		item.Stack{ItemID: 202, ClassID: 21613, Count: 2},
	)
	f.stacks[0].Count = 1000
	h := shopHandler(f, testPrices(t))
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks)}

	out, err := h.sellItem(context.Background(), sess, frameSell(0,
		SellRecord{InvIndex: 3, Count: 2})) // the SECOND Fire stone row
	if err != nil {
		t.Fatalf("sellItem: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (money, then the sold row)", len(out))
	}
	if f.held(201) != 5 {
		t.Fatalf("the other Fire stone row holds %d, want 5 untouched", f.held(201))
	}
	if f.held(202) != 0 {
		t.Fatalf("the sold row holds %d, want 0 (emptied and removed)", f.held(202))
	}
	if _, ok := sess.Inv.IDAt(3); ok {
		t.Fatal("the emptied row is still a live position in the mirror")
	}
	// An emptied row is a HOLE the client keeps: position 2 must not move.
	if id, ok := sess.Inv.IDAt(2); !ok || id != 201 {
		t.Fatalf("mirror position 2 = %d, %v; want 201", id, ok)
	}
}

// TestSellItemRefusesUnsellableItems: the two things a normal shop must never take -
// an item with no sell price or no Venderable flag, and the vis row itself.
func TestSellItemRefusesUnsellableItems(t *testing.T) {
	cases := []struct {
		name   string
		class  int64
		invIdx int32
	}{
		{"not venderable", 20023, 2},
		{"the vis row itself", VisClass, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeShop(
				item.Stack{ItemID: 101, ClassID: VisClass, Count: 1000},
				item.Stack{ItemID: 201, ClassID: c.class, Count: 1},
			)
			h := shopHandler(f, testPrices(t))
			sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks)}

			out, err := h.sellItem(context.Background(), sess, frameSell(0,
				SellRecord{InvIndex: c.invIdx, Count: 1}))
			if err != nil {
				t.Fatalf("sellItem: %v", err)
			}
			if len(out) != 0 {
				t.Fatalf("%d packet(s) for an unsellable row, want 0", len(out))
			}
			if len(f.ops) != 0 {
				t.Fatalf("%d op(s) reached the write path, want 0", len(f.ops))
			}
		})
	}
}

// TestSellItemRefusesWithoutVis: with no vis row there is nowhere to pay the gain, so
// the sale is refused instead of giving the item away.
func TestSellItemRefusesWithoutVis(t *testing.T) {
	f := newFakeShop(item.Stack{ItemID: 201, ClassID: 21613, Count: 5})
	h := shopHandler(f, testPrices(t))
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks)}

	out, err := h.sellItem(context.Background(), sess, frameSell(0,
		SellRecord{InvIndex: 1, Count: 1}))
	if err != nil {
		t.Fatalf("sellItem: %v", err)
	}
	if len(out) != 0 || len(f.ops) != 0 {
		t.Fatalf("a sale with no vis row wrote %d op(s) and sent %d packet(s), want 0 and 0",
			len(f.ops), len(out))
	}
	if f.held(201) != 5 {
		t.Fatalf("the bag row holds %d, want 5 untouched", f.held(201))
	}
}

// TestGroupDigits is the sale line's money: the oracle formats it with Python's
// "{:,}", and the client prints the text verbatim.
func TestGroupDigits(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{3300, "3,300"},
		{124200, "124,200"},
		{1234567, "1,234,567"},
	} {
		if got := groupDigits(c.in); got != c.want {
			t.Fatalf("groupDigits(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
