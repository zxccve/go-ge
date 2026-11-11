package script

import (
	"context"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"geserver/item"
)

// The `Tx` layer: how a script changes the world.
//
// A script does not write to the database; it opens a transaction, QUEUES
// property writes, and ends it. Everything the transaction queued lands or none
// of it does, and `TxEnd` answers 1 or 0 - which the scripts branch on
// (`if (ret == 0) { return; }`).
//
// Stage 1 is PROPERTIES only. Money is NOT a property: the client shows it from
// the vis pseudo-item (class 30000/30001), so moving money needs the item layer
// (Stage 2). See ROADMAP.md R46.

// TxStore is the database behind a script transaction.
//
// It is an interface, not an import, for the same reason Handler.Click is a
// function field: the script engine must be testable with a fake, and the
// dependency stays one-way (main wires it). The zone's PGStore satisfies it
// structurally, so neither package imports the other.
type TxStore interface {
	// CommanderProps returns every NAMED property of a commander. A property
	// that has no row is absent from the map, and reads as 0 - which is what
	// the shipped IES object answers for an unset integer.
	CommanderProps(ctx context.Context, commanderID int32) (map[string]int64, error)

	// CommitTx writes the properties AND the item ops in ONE database
	// transaction. A property-only transaction passes no ops: the same method is
	// the whole write path, because a script that pays a fee and moves a counter
	// must not have one land and the other not.
	CommitTx(ctx context.Context, commanderID int32, props map[string]int64, ops []item.Op) ([]item.Change, error)
}

// PropValue is one property a COMMITTED transaction moved.
//
// The zone pushes the Lock_* ones back to the client: the client CACHES them
// (bag capacity is `Lock_InventoryCount`), so a database write alone is
// invisible on screen.
type PropValue struct {
	Name  string
	Value int64
}

// numericItemName accepts a class id that reached the call as text ("30000"), the
// same leniency the oracle has for a number instead of a name.
func numericItemName(name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// tx is one OPEN transaction.
//
// The commander is remembered AT BEGIN, not looked up at commit: a later resume
// (a menu the player answered) must not write to a different character.
type tx struct {
	commander int32
	values    map[string]int64
	items     []item.Op
	failed    bool
}

// installTx registers the transaction builtins. Called once per runtime, from
// New, so a script never reaches the "unimplemented builtin answers 0" stub for
// them.
func (h *Host) installTx() {
	L := h.L

	L.SetGlobal("TxBegin", L.NewFunction(func(L *lua.LState) int {
		h.txSeq++
		id := h.txSeq
		h.txs[id] = &tx{commander: h.clickCommander(), values: map[string]int64{}}
		h.txOrder = append(h.txOrder, id)
		L.Push(lua.LNumber(id))
		return 1
	}))

	// TxSetCommProp(tx, name, value) queues a write. It answers 0, like the
	// shipped builtin: the RESULT the script guards on is TxEnd's.
	L.SetGlobal("TxSetCommProp", L.NewFunction(func(L *lua.LState) int {
		id := int64(L.CheckNumber(1))
		name := strings.TrimSpace(argString(L, 2))
		value := int64(L.CheckNumber(3))
		t, known := h.txs[id]
		switch {
		case !known:
			// A set on a transaction that was never begun (or already ended)
			// writes nowhere. Saying so beats inventing a transaction.
			h.logf("script: TxSetCommProp(%d, %q) - no open transaction %d, ignored",
				id, name, id)
		case !validPropName(name):
			// A property name that is a number is a helper that answered 0, the
			// same crash class as `ShowOkDlg(self, 0)`. The original server
			// refuses an unknown property and fails the whole transaction
			// (`TransactionFunc.cpp`), so this does too.
			t.failed = true
			h.logf("script: TxSetCommProp(%d, %q, %d) - %q is not a property name, "+
				"the transaction will fail", id, name, value, name)
		default:
			t.values[name] = value
		}
		L.Push(lua.LNumber(0))
		return 1
	}))

	// TxEnd(tx) answers 1 when everything landed, 0 when it did not - and the
	// scripts branch on exactly that.
	L.SetGlobal("TxEnd", L.NewFunction(func(L *lua.LState) int {
		n := lua.LNumber(0)
		if h.txEnd(int64(L.CheckNumber(1))) {
			n = 1
		}
		L.Push(n)
		return 1
	}))

	// The original's "what went wrong" code. 0 means success, and no script in
	// the corpus reads the failure codes, so 0 is the honest answer.
	L.SetGlobal("TxGetLastResultCode", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(0))
		return 1
	}))

	// Stage 2: items. The original decides AT CALL TIME: an unknown item, a
	// non-positive count, a stack the player does not have, or a give that would
	// overfill a stack answers 0 AND fails the whole transaction
	// (`TransactionFunc.cpp:294`, `TxScript.cpp:1170`). Getting this wrong is what
	// makes a script take the wrong branch - so the answer here is the same rule,
	// checked against the bag this click loaded.
	itemOp := func(give bool) lua.LGFunction {
		return func(L *lua.LState) int {
			id := int64(L.CheckNumber(1))
			name := argString(L, 2)
			count := int64(L.OptNumber(3, 1))
			t, known := h.txs[id]
			refuse := func(why string) int {
				if known {
					t.failed = true
				}
				if h.click != nil && h.click.refused == 0 {
					h.click.refused++
					h.logf("script: %s(%q, %d) refused - %s, so the transaction fails "+
						"and nothing is written", itemBuiltinName(give), name, count, why)
				}
				L.Push(lua.LNumber(0))
				return 1
			}
			if !known {
				L.Push(lua.LNumber(0))
				return 1
			}
			classID, why := h.checkItemOp(give, name, count)
			if why != "" {
				return refuse(why)
			}
			op := item.Op{Give: give, ClassID: classID, Count: count, Name: name}
			// The item table's word on stacking travels with the op: the write path
			// must NOT merge equipment into an existing slot, and it must refuse a
			// give that would pass the cap even if this check somehow passed. A bare
			// class id the table does not carry keeps the zero value: no merge, no
			// cap - one slot per give, rather than merging into something we cannot
			// describe.
			if entry, known := h.Catalog.Lookup(name); known {
				op.Merge = entry.Mergeable
				op.MaxStack = entry.MaxStack
			}
			t.items = append(t.items, op)
			L.Push(lua.LNumber(1))
			return 1
		}
	}
	L.SetGlobal("TxGiveItem", L.NewFunction(itemOp(true)))
	L.SetGlobal("TxTakeItem", L.NewFunction(itemOp(false)))
}

// itemBuiltinName is the corpus name of an op, for the log and the worklist.
func itemBuiltinName(give bool) string {
	if give {
		return "TxGiveItem"
	}
	return "TxTakeItem"
}

// checkItemOp applies the shipped call-time rule and returns the class id, or a
// reason the op must be refused ("" means it may go in the transaction).
func (h *Host) checkItemOp(give bool, name string, count int64) (int64, string) {
	if count < 0 || (give && count == 0) {
		return 0, "a count that is not positive cannot move"
	}
	if h.Catalog == nil {
		return 0, "the item table is not loaded, so no item name can be resolved"
	}
	entry, known := h.Catalog.Lookup(name)
	if !known {
		// Class ids can be passed as numbers, but a NAME the table does not know
		// is a misspelling or a stale script - the original refuses it.
		if number, ok := numericItemName(name); ok {
			return number, ""
		}
		return 0, "the item table has no such item"
	}

	h.loadBag()
	if h.click == nil {
		return 0, "no click in progress, so there is no bag to check"
	}
	pending := h.pendingItems(entry.ClassID)
	held := h.click.bag[entry.ClassID] + pending

	if !give {
		if held < count {
			return 0, "not enough is held"
		}
		return entry.ClassID, ""
	}

	// A give joins the OLDEST stack and never opens a second one: an amount that
	// does not fit it is a FULL INVENTORY and fails the transaction. The queued
	// ops of this click land on that same stack, so they count towards its size.
	if !entry.Mergeable {
		return entry.ClassID, "" // equipment always gets its own slot
	}
	first, has := h.click.first[entry.ClassID]
	if !has {
		return entry.ClassID, "" // nothing of this class held, so a new stack
	}
	if first+pending+count > entry.MaxStack {
		return 0, "the stack is full (MaxStack)"
	}
	return entry.ClassID, ""
}

// pendingItems is the net change this click's OPEN transactions have already
// queued for one class, so a script that gives an item and then takes it sees its
// own write - the same rule the property read-back uses.
func (h *Host) pendingItems(classID int64) int64 {
	var net int64
	for _, id := range h.txOrder {
		t := h.txs[id]
		if t == nil {
			continue
		}
		for _, op := range t.items {
			if op.ClassID != classID {
				continue
			}
			if op.Give {
				net += op.Count
			} else {
				net -= op.Count
			}
		}
	}
	return net
}

// txEnd commits one transaction and reports whether it succeeded.
func (h *Host) txEnd(id int64) bool {
	t, known := h.txs[id]
	if !known {
		h.logf("script: TxEnd(%d) - unknown transaction, answering 0", id)
		return false
	}
	h.forgetTx(id)

	if t.failed {
		h.logf("script: TxEnd(%d) refused - an op failed earlier in this "+
			"transaction, so nothing is written", id)
		return false
	}
	if len(t.values) == 0 && len(t.items) == 0 {
		// A transaction that queued nothing succeeded in doing nothing. No
		// database round trip: the commonest case in the corpus (8,385 begins).
		return true
	}
	if t.commander == 0 {
		h.logf("script: TxEnd(%d) refused - this click has no commander id, so "+
			"there is nobody to write to", id)
		return false
	}
	if h.Tx == nil {
		h.logf("script: TxEnd(%d) refused - no store is wired into the script engine "+
			"(%d value(s), %d item op(s) would have been written)",
			id, len(t.values), len(t.items))
		return false
	}

	ctx := h.callCtx
	if ctx == nil {
		ctx = context.Background()
	}
	changes, err := h.Tx.CommitTx(ctx, t.commander, t.values, t.items)
	if err != nil {
		h.logf("script: TxEnd(%d) failed - the database refused the write, so the "+
			"transaction is rolled back: %v", id, err)
		return false
	}

	// Committed. Keep the values in the click's own cache so a script that reads
	// a property back AFTER TxEnd is told the new number, not the old one -
	// Leonardo's "increased up to %d" line does exactly that.
	if h.click != nil {
		for name, value := range t.values {
			h.click.props[name] = value
			h.click.outcome.Properties = append(h.click.outcome.Properties,
				PropValue{Name: name, Value: value})
		}
		if len(t.items) > 0 {
			// The bag changed under the cache, so the next count must come from the
			// database rather than from what the click started with.
			h.click.bag, h.click.first = nil, nil
			h.click.bagLoaded = false
			h.click.outcome.Items = append(h.click.outcome.Items, changes...)
		}
	}
	h.logf("script: TxEnd(%d) committed %d propertie(s) and %d item change(s) for "+
		"commander %d", id, len(t.values), len(changes), t.commander)
	return true
}

// forgetTx removes a transaction from both the map and the order slice.
func (h *Host) forgetTx(id int64) {
	delete(h.txs, id)
	for i, other := range h.txOrder {
		if other == id {
			h.txOrder = append(h.txOrder[:i], h.txOrder[i+1:]...)
			return
		}
	}
}

// pendingProp is the value an OPEN transaction has queued for this commander,
// latest transaction last - so a script that sets a property and then reads it
// back sees its own write, exactly like the shipped object. `ok` is false when
// nothing is queued; 0 is a real value a guard can compare.
func (h *Host) pendingProp(commander int32, name string) (int64, bool) {
	for i := len(h.txOrder) - 1; i >= 0; i-- {
		t := h.txs[h.txOrder[i]]
		if t == nil || t.commander != commander {
			continue
		}
		if value, queued := t.values[name]; queued {
			return value, true
		}
	}
	return 0, false
}

// propValue answers `pc.<name>`: the running transaction wins, then what the
// click loaded from the database, then 0.
func (h *Host) propValue(commander int32, name string) int64 {
	if value, queued := h.pendingProp(commander, name); queued {
		return value
	}
	if h.click != nil {
		if value, ok := h.click.props[name]; ok {
			return value
		}
	}
	return 0
}

// clickCommander is whose transaction this is. 0 means "unknown", and TxEnd
// then refuses rather than writing to nobody.
func (h *Host) clickCommander() int32 {
	if h.click == nil {
		return 0
	}
	return h.click.commander
}

// validPropName rejects what cannot be a property: an empty name, or one that
// starts with a digit (a number that reached the call as text).
func validPropName(name string) bool {
	if name == "" {
		return false
	}
	if name[0] >= '0' && name[0] <= '9' {
		return false
	}
	return true
}

// logf is a nil-safe log line: a Host built without a logger (every unit test)
// must not panic on a refused transaction.
func (h *Host) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}
