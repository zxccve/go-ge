package script

import (
	"context"
	"errors"
	"testing"
	"time"

	"geserver/item"
)

// fakeBag is the player's inventory, in memory: class id -> total count, plus a
// first-stack size per class (what a give may join).
type fakeBag struct {
	counts map[int64]int64
	first  map[int64]int64
	slots  int
	err    error
	reads  int
}

func (f *fakeBag) Stacks(ctx context.Context, commanderID int32) ([]item.Stack, error) {
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]item.Stack, 0, len(f.counts))
	var id int64 = 9000
	for classID, total := range f.counts {
		one := total
		if v, ok := f.first[classID]; ok {
			one = v
		}
		id++
		out = append(out, item.Stack{ItemID: id, ClassID: classID, Count: one})
	}
	return out, nil
}

// fakeCatalog is the item table, three names deep.
type fakeCatalog map[string]int64

func (f fakeCatalog) Lookup(name string) (item.Entry, bool) {
	id, ok := f[name]
	if !ok {
		return item.Entry{}, false
	}
	// Vis/FireStone stack; a Sword is equipment (one per slot).
	return item.Entry{ClassID: id, Mergeable: name != "Sword", MaxStack: 32767}, true
}

func (f fakeCatalog) NameOf(classID int64) string { return "" }

func bagHost(t *testing.T, body string, bag *fakeBag) *Host {
	t.Helper()
	h := newTestHost(t, body)
	h.Items = bag
	h.Catalog = fakeCatalog{"Vis": 30000, "FireStone": 21613, "Sword": 1234}
	return h
}

// TestMoneyIsTheVisItemCount is the one that matters for "can I afford it?".
//
// The client draws money from the vis pseudo-item (class 30000), so the count of
// that class IS the money. A script asking whether it can pay a fee reads this.
func TestMoneyIsTheVisItemCount(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 12500, 21613: 3}, slots: 2}
	h := bagHost(t, `
function SCR_CLICK_BUY(self, pc)
  money = GetItemCountInInventory(pc, "Vis")
  stones = GetInvItemCount(pc, "FireStone")
  swords = GetItemCountInInventory(pc, "Sword")
  if (money >= 1000) then ShowOkDlg(self, "yes") else ShowOkDlg(self, "no") end
end`, bag)

	out, err := h.StartClick("SCR_CLICK_BUY", Obj{},
		Obj{Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("money").String(); got != "12500" {
		t.Fatalf("money = %q, want 12500 (the vis item's count)", got)
	}
	if got := h.L.GetGlobal("stones").String(); got != "3" {
		t.Fatalf("FireStone = %q, want 3", got)
	}
	// A known item the player owns none of is 0, not nil.
	if got := h.L.GetGlobal("swords").String(); got != "0" {
		t.Fatalf("Sword = %q, want 0", got)
	}
	if out.Dialog.Text != "yes" {
		t.Fatalf("an affordable fee said %q", out.Dialog.Text)
	}
}

// TestBagIsReadOncePerClick: a script checking twenty items must not cost twenty
// round trips, and the bag cannot change while the click runs - a transaction's
// give/take commits at the end of it.
func TestBagIsReadOncePerClick(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 1}, slots: 1}
	h := bagHost(t, `
function SCR_CLICK_MANY(self, pc)
  for i = 1, 20 do
    local n = GetItemCountInInventory(pc, "Vis")
  end
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_MANY", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if bag.reads != 1 {
		t.Fatalf("the bag was read %d time(s), want 1", bag.reads)
	}
}

// TestUnknownItemNameIsZeroNotNil: a name the item table does not know reads 0.
// nil would raise on the first comparison, and `nil >= 1000` kills the click.
func TestUnknownItemNameIsZeroNotNil(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{}}
	h := bagHost(t, `
function SCR_CLICK_UNKNOWN(self, pc)
  n = GetItemCountInInventory(pc, "NoSuchItemAtAll")
  ok = (n < 1)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_UNKNOWN", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("n").String(); got != "0" {
		t.Fatalf("unknown item = %q, want 0", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "true" {
		t.Fatalf("the comparison did not see a number (%q)", got)
	}
}

// TestClassIDArgumentWorks: the corpus sometimes passes the NUMBER instead of the
// name (`GetInvItemCount(pc, 30000)`), and the original accepts both.
func TestClassIDArgumentWorks(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 42}}
	h := bagHost(t, `
function SCR_CLICK_NUM(self, pc)
  n = GetItemCountInInventory(pc, 30000)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_NUM", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("n").String(); got != "42" {
		t.Fatalf("count by class id = %q, want 42", got)
	}
}

// TestInventoryItemCountIsSlots: the third builtin is how full the bag is, which
// is what a "the bag is full" branch compares with capacity.
func TestInventoryItemCountIsSlots(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 1, 21613: 3, 21614: 2}}
	h := bagHost(t, `
function SCR_CLICK_SLOTS(self, pc)
  used = GetInventoryItemCount()
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_SLOTS", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	// A slot is a ROW: three classes in the bag are three slots, whatever the
	// counts add up to.
	if got := h.L.GetGlobal("used").String(); got != "3" {
		t.Fatalf("slots = %q, want 3", got)
	}
}

// TestBagReadFailureAnswersZero: the database refusing the read must not kill the
// click. Every count is 0, which is what it was before this layer existed.
func TestBagReadFailureAnswersZero(t *testing.T) {
	bag := &fakeBag{err: errors.New("connection refused")}
	h := bagHost(t, `
function SCR_CLICK_BAGERR(self, pc)
  n = GetItemCountInInventory(pc, "Vis")
  ShowOkDlg(self, "still talking")
end`, bag)

	out, err := h.StartClick("SCR_CLICK_BAGERR", Obj{},
		Obj{Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("a failed bag read killed the click: %v", err)
	}
	if got := h.L.GetGlobal("n").String(); got != "0" {
		t.Fatalf("count after a failed read = %q, want 0", got)
	}
	if out.Dialog.Kind != DialogOK {
		t.Fatal("the NPC stopped talking after a failed bag read")
	}
}

// TestNoCatalogMeansNoItems: with no item table loaded a NAME cannot be resolved,
// and the honest answer is 0 - the engine logs why once.
func TestNoCatalogMeansNoItems(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 99}}
	h := newTestHost(t, `
function SCR_CLICK_NOCAT(self, pc)
  n = GetItemCountInInventory(pc, "Vis")
  byId = GetItemCountInInventory(pc, 30000)
end`)
	h.Items = bag
	// deliberately no catalog

	if _, err := h.StartClick("SCR_CLICK_NOCAT", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("n").String(); got != "0" {
		t.Fatalf("name without a catalog = %q, want 0", got)
	}
	// A NUMBER still works: it needs no catalog to be a class id.
	if got := h.L.GetGlobal("byId").String(); got != "99" {
		t.Fatalf("class id without a catalog = %q, want 99", got)
	}
}
