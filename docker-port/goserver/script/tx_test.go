package script

import (
	"context"
	"errors"
	"testing"
	"time"

	"geserver/item"
)

// fakeStore is the database side of a transaction, in memory. It records what a
// commit really wrote, which is the whole question: a script that believes a
// write succeeded takes the wrong branch for the rest of the session.
type fakeStore struct {
	props     map[string]int64 // what CommanderProps reports
	committed map[string]int64 // what CommitTx was handed
	commits   int
	commander int32
	err       error
	gave      []item.Op // item ops the transaction really committed
	took      []item.Op
}

func (f *fakeStore) CommanderProps(ctx context.Context, commanderID int32) (map[string]int64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int64, len(f.props))
	for name, value := range f.props {
		out[name] = value
	}
	return out, nil
}

func (f *fakeStore) CommitTx(ctx context.Context, commanderID int32, values map[string]int64, ops []item.Op) ([]item.Change, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.commits++
	f.commander = commanderID
	if f.committed == nil {
		f.committed = map[string]int64{}
	}
	for name, value := range values {
		f.committed[name] = value
		// A real store answers the NEXT read with what it just wrote, so the
		// model must too - otherwise a second click on the same NPC reads the
		// pre-test value and the sweep under-reports.
		if f.props == nil {
			f.props = map[string]int64{}
		}
		f.props[name] = value
	}
	var changes []item.Change
	for _, op := range ops {
		if op.Give {
			f.gave = append(f.gave, op)
		} else {
			f.took = append(f.took, op)
		}
		changes = append(changes, item.Change{
			Give: op.Give, ClassID: op.ClassID, ItemID: 9000 + int64(len(f.gave)+len(f.took)),
			Total: op.Count,
		})
	}
	return changes, nil
}

// TestTxCommitWritesTheIncrement is the shape 30,182 call sites have: read a
// counter off the player, add one, queue it, commit. The read must be the STORED
// value (a bag answering 0 would make every counter restart at 1) and the write
// must be the new one.
func TestTxCommitWritesTheIncrement(t *testing.T) {
	store := &fakeStore{props: map[string]int64{"Cwar_Mining": 4}}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", pc.Cwar_Mining + 1)
  ok = TxEnd(tx)
  after = pc.Cwar_Mining
end`)
	h.Tx = store

	out, err := h.StartClick("SCR_CLICK_TX",
		Obj{Name: "Miner"}, Obj{Name: "Tester", Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}

	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if got := store.committed["Cwar_Mining"]; got != 5 {
		t.Fatalf("committed Cwar_Mining = %d, want 5 (stored 4 + 1)", got)
	}
	if store.commander != 24 {
		t.Fatalf("committed for commander %d, want 24", store.commander)
	}
	// A script may read the property back AFTER the commit (Leonardo's
	// "increased up to %d" line does), so the bag must already say 5.
	if got := h.L.GetGlobal("after").String(); got != "5" {
		t.Fatalf("read-back = %q, want \"5\"", got)
	}
	// The zone pushes the Lock_* ones to the client's cache; the engine's job is
	// to report what moved.
	if len(out.Properties) != 1 || out.Properties[0].Name != "Cwar_Mining" ||
		out.Properties[0].Value != 5 {
		t.Fatalf("Outcome.Properties = %+v, want [Cwar_Mining=5]", out.Properties)
	}
}

// TestTxPendingWriteWinsOverTheStoredOne: a script that sets and then reads in
// the SAME transaction must see its own write, not the old row - otherwise
// "add one" within one script reads stale.
func TestTxPendingWriteWinsOverTheStoredOne(t *testing.T) {
	store := &fakeStore{props: map[string]int64{"Cwar_Mining": 4}}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 40)
  seen = pc.Cwar_Mining
  TxEnd(tx)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("seen").String(); got != "40" {
		t.Fatalf("read-back inside the transaction = %q, want \"40\" (the queued value)", got)
	}
}

// TestTxItemOpRefusedWithoutAnItemTable is the call-time rule the shipped server
// has (`TransactionFunc.cpp:294`): an item op it cannot serve answers 0 AND fails
// the transaction, so the property queued beside it never lands. Pretending
// otherwise would tell the player a quest paid out while the item never appeared.
func TestTxItemOpRefusedWithoutAnItemTable(t *testing.T) {
	store := &fakeStore{}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 9)
  give = TxGiveItem(tx, "Wool", 10)
  ok = TxEnd(tx)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("give").String(); got != "0" {
		t.Fatalf("TxGiveItem answered %q, want 0", got)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q, want 0 - a failed item op fails the transaction", got)
	}
	if store.commits != 0 {
		t.Fatalf("the property was written despite the failed item op (%v)", store.committed)
	}
	if len(store.gave) != 0 {
		t.Fatalf("a refused give reached the store: %+v", store.gave)
	}
}

// TestTxNumericPropertyNameFails covers the crash class this file's own header
// warns about: a builtin that answers 0 and the 0 reaching the NEXT call. A
// property name that is a number cannot be a property, and writing a row called
// "0" would be invisible until someone read that row back.
func TestTxNumericPropertyNameFails(t *testing.T) {
	store := &fakeStore{}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, 0, 5)
  ok = TxEnd(tx)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q, want 0 (the name is not a property)", got)
	}
	if store.commits != 0 {
		t.Fatalf("a numeric property name was written: %v", store.committed)
	}
}

// TestTxRefusesWithoutACommander: a transaction with no commander has nobody to
// write to. Refusing is the whole safety property - writing to id 0 would put
// the row on some other player or none.
func TestTxRefusesWithoutACommander(t *testing.T) {
	store := &fakeStore{}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 1)
  ok = TxEnd(tx)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{}, Obj{}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q, want 0", got)
	}
	if store.commits != 0 {
		t.Fatalf("wrote %v with no commander", store.committed)
	}
}

// TestTxRefusesWithoutAStore: a zone started without the property store must
// FAIL the transaction, not silently succeed. A 1 here makes the NPC say "done"
// while nothing changed.
func TestTxRefusesWithoutAStore(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 1)
  ok = TxEnd(tx)
end`)

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q with no store, want 0", got)
	}
}

// TestTxEmptyTransactionIsSuccess: 8,385 begins in the corpus never queue
// anything. Doing nothing successfully is a success, and it must not cost a
// database round trip.
func TestTxEmptyTransactionIsSuccess(t *testing.T) {
	store := &fakeStore{}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  ok = TxEnd(tx)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "1" {
		t.Fatalf("TxEnd answered %q, want 1", got)
	}
	if store.commits != 0 {
		t.Fatalf("an empty transaction wrote %d time(s)", store.commits)
	}
}

// TestTxUnknownIdIsRefused: a stale or invented transaction id must answer 0.
// The alternative - creating a transaction on demand - writes a property the
// script never meant to move.
func TestTxUnknownIdIsRefused(t *testing.T) {
	store := &fakeStore{}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  ok = TxEnd(424242)
  TxSetCommProp(424242, "Cwar_Mining", 7)
  ok2 = TxEnd(424242)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	for _, name := range []string{"ok", "ok2"} {
		if got := h.L.GetGlobal(name).String(); got != "0" {
			t.Fatalf("%s = %q, want 0", name, got)
		}
	}
	if store.commits != 0 {
		t.Fatalf("an unknown transaction wrote %v", store.committed)
	}
}

// TestTxCommitFailureAnswersZero: the database refusing the write must reach the
// script as 0, so it does NOT tell the player the job paid out.
func TestTxCommitFailureAnswersZero(t *testing.T) {
	store := &fakeStore{err: errors.New("connection refused")}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  local tx = TxBegin(pc)
  TxSetCommProp(tx, "Cwar_Mining", 1)
  ok = TxEnd(tx)
end`)
	h.Tx = store

	out, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("a failed commit must not become a Lua error: %v", err)
	}
	if got := h.L.GetGlobal("ok").String(); got != "0" {
		t.Fatalf("TxEnd answered %q after a failed commit, want 0", got)
	}
	if len(out.Properties) != 0 {
		t.Fatalf("a failed commit reported %+v as moved", out.Properties)
	}
}

// TestPropertyLoadFailureIsNotFatal: the click still runs when the property read
// fails, on zeros - which is what it did before this layer existed.
func TestPropertyLoadFailureIsNotFatal(t *testing.T) {
	store := &fakeStore{err: errors.New("no database")}
	h := newTestHost(t, `
function SCR_CLICK_TX(self, pc)
  seen = pc.Cwar_Mining
  ShowOkDlg(self, "still here")
end`)
	h.Tx = store

	out, err := h.StartClick("SCR_CLICK_TX", Obj{},
		Obj{Commander: 24}, time.Second)
	if err != nil {
		t.Fatalf("a failed property read killed the click: %v", err)
	}
	if out.Dialog.Kind != DialogOK {
		t.Fatalf("kind = %q, want the NPC's line", out.Dialog.Kind)
	}
	if got := h.L.GetGlobal("seen").String(); got != "0" {
		t.Fatalf("property read = %q, want 0", got)
	}
}
