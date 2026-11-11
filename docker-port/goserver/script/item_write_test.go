package script

import (
	"testing"
	"time"
)

// The item WRITE path, from the script's side: what a give/take does at CALL time
// (the shipped rule) and what reaches the store at TxEnd.

func itemHost(t *testing.T, body string, bag *fakeBag) (*Host, *fakeStore) {
	t.Helper()
	store := &fakeStore{}
	h := newTestHost(t, body)
	h.Tx = store
	h.Items = bag
	h.Catalog = fakeCatalog{"Vis": 30000, "FireStone": 21613, "Sword": 1234}
	return h, store
}

// TestGiveReachesTheStore: a give that the bag allows is queued and committed, and
// the store sees it as a give of that class and count.
func TestGiveReachesTheStore(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 100}}
	h, store := itemHost(t, `
function SCR_CLICK_GIVE(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 1)
  give = TxGiveItem(tx, "FireStone", 5)
  ok = TxEnd(tx)
end`, bag)

	out, err := h.StartClick("SCR_CLICK_GIVE", Obj{},
		Obj{Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("give").String(); got != "1" {
		t.Fatalf("TxGiveItem answered %q, want 1", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if len(store.gave) != 1 || store.gave[0].ClassID != 21613 || store.gave[0].Count != 5 {
		t.Fatalf("store saw %+v, want one give of 21613 x5", store.gave)
	}
	// The property beside it landed in the SAME transaction.
	if store.committed["Cwar_Mining"] != 1 {
		t.Fatalf("the property did not commit with the item: %v", store.committed)
	}
	// And the zone is told what moved, which is what the client draws.
	if len(out.Items) != 1 || out.Items[0].ClassID != 21613 {
		t.Fatalf("Outcome.Items = %+v", out.Items)
	}
}

// TestGiveOverMaxStackIsAFullInventory: the shipped server never opens a second
// stack. A give that does not fit the OLDEST stack answers 0 and fails the whole
// transaction, which is what makes a script tell the player "your bag is full".
func TestGiveOverMaxStackIsAFullInventory(t *testing.T) {
	// Vis joins "Vis", cap 32767: the oldest stack already holds 32760.
	bag := &fakeBag{counts: map[int64]int64{30000: 32760}, first: map[int64]int64{30000: 32760}}
	h, store := itemHost(t, `
function SCR_CLICK_FULL(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 1)
  give = TxGiveItem(tx, "Vis", 10)
  ok = TxEnd(tx)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_FULL", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("give").String(); got != "0" {
		t.Fatalf("an over-cap give answered %q, want 0", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q, want 0", got)
	}
	if store.commits != 0 || len(store.gave) != 0 {
		t.Fatalf("a refused give still wrote: commits=%d gave=%+v", store.commits, store.gave)
	}
}

// TestTakeMoreThanHeldIsRefused: paying a fee you cannot afford answers 0 and the
// transaction fails, so a script does not hand out the reward anyway.
func TestTakeMoreThanHeldIsRefused(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 100}}
	h, store := itemHost(t, `
function SCR_CLICK_POOR(self, pc)
  local tx = TxBegin(pc)
  take = TxTakeItem(tx, "Vis", 1000)
  ok = TxEnd(tx)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_POOR", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("take").String(); got != "0" {
		t.Fatalf("an unaffordable take answered %q, want 0", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q, want 0", got)
	}
	if len(store.took) != 0 {
		t.Fatalf("a refused take reached the store: %+v", store.took)
	}
}

// TestTakeReachesTheStore: the happy path, and money is just the vis item.
func TestTakeReachesTheStore(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{30000: 5000}}
	h, store := itemHost(t, `
function SCR_CLICK_FEE(self, pc)
  local tx = TxBegin(pc)
  ok1 = TxTakeItem(tx, "Vis", 1000)
  ok = TxEnd(tx)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_FEE", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if len(store.took) != 1 || store.took[0].ClassID != 30000 || store.took[0].Count != 1000 {
		t.Fatalf("store saw %+v, want one take of 30000 x1000", store.took)
	}
}

// TestAScriptSeesItsOwnQueuedItemOps: a script that gives and then takes the same
// thing must not be refused for "not enough held" - the give is already in its own
// transaction.
func TestAScriptSeesItsOwnQueuedItemOps(t *testing.T) {
	bag := &fakeBag{}
	h, store := itemHost(t, `
function SCR_CLICK_NET(self, pc)
  local tx = TxBegin(pc)
  g = TxGiveItem(tx, "FireStone", 5)
  t = TxTakeItem(tx, "FireStone", 5)
  ok = TxEnd(tx)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_NET", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("t").String(); got != "1" {
		t.Fatalf("the take answered %q, want 1 (the give is in the same transaction)", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if len(store.gave) != 1 || len(store.took) != 1 {
		t.Fatalf("store saw gave=%+v took=%+v", store.gave, store.took)
	}
}

// TestEquipmentAlwaysMintsItsOwnStack: an unstackable item is never merged, so a
// give of one is allowed even when a stack of the same class is at the cap.
func TestEquipmentAlwaysMintsItsOwnStack(t *testing.T) {
	bag := &fakeBag{counts: map[int64]int64{1234: 1}, first: map[int64]int64{1234: 1}}
	h, store := itemHost(t, `
function SCR_CLICK_SWORD(self, pc)
  local tx = TxBegin(pc)
  give = TxGiveItem(tx, "Sword", 1)
  ok = TxEnd(tx)
end`, bag)

	if _, err := h.StartClick("SCR_CLICK_SWORD", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if len(store.gave) != 1 || store.gave[0].ClassID != 1234 {
		t.Fatalf("store saw %+v", store.gave)
	}
}
