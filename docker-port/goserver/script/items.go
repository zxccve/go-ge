package script

import (
	"context"

	lua "github.com/yuin/gopher-lua"

	"geserver/item"
)

// The item reads a click script asks for, and the bag a transaction writes.
//
// A script counts what the player holds (`GetItemCountInInventory`, 1,364 call
// sites; `GetInvItemCount`, 872) and how full the bag is
// (`GetInventoryItemCount`). Both answered 0 before this existed, which made every
// "do you have the fee?" and "is the bag full?" branch take the false side.
//
// MONEY IS NOT A SEPARATE VALUE: the client draws it from the vis pseudo-item
// (class 30000/30001), so the count of that class IS the money.

// ItemStore is the player's bag. It is read ONCE per click, not once per call: a
// script checking twenty items would otherwise cost twenty round trips, and the
// bag cannot change while a click runs (a transaction's give/take commits at the
// end of it).
//
// It hands back the ROWS, not totals, because a give must know how much room the
// oldest stack of a class has: the shipped server never opens a second stack, it
// refuses the give when the full amount does not fit one.
type ItemStore interface {
	Stacks(ctx context.Context, commanderID int32) ([]item.Stack, error)
}

// ItemCatalog answers what an item NAME is. NIL means "no catalog": every count
// then answers 0, which is the same answer an unknown name gets - and the engine
// says so once, because "we do not know the item table" and "you own none" must
// not look alike in a log.
type ItemCatalog interface {
	Lookup(name string) (item.Entry, bool)
	NameOf(classID int64) string
}

// installItems registers the bag reads. Called once per runtime, from New.
func (h *Host) installItems() {
	L := h.L

	// GetItemCountInInventory(pc, name) and GetInvItemCount(pc, name) are the
	// same question in the corpus; the second name is an alias, not a variant.
	count := func(L *lua.LState) int {
		L.Push(lua.LNumber(h.itemCountOf(L.Get(2))))
		return 1
	}
	L.SetGlobal("GetItemCountInInventory", L.NewFunction(count))
	L.SetGlobal("GetInvItemCount", L.NewFunction(count))

	// Slots in use - `inventory->GetCount()` in the original.
	L.SetGlobal("GetInventoryItemCount", L.NewFunction(func(L *lua.LState) int {
		h.loadBag()
		if h.click == nil {
			L.Push(lua.LNumber(0))
			return 1
		}
		L.Push(lua.LNumber(h.click.slots))
		return 1
	}))
}

// itemEntry resolves an item argument: a number is a class id already (the corpus
// does that), a string is a NAME and needs the catalog.
func (h *Host) itemEntry(value lua.LValue) (int64, bool) {
	if number, ok := value.(lua.LNumber); ok {
		return int64(number), true
	}
	name, ok := value.(lua.LString)
	if !ok || name == "" || h.Catalog == nil {
		return 0, false
	}
	entry, known := h.Catalog.Lookup(string(name))
	return entry.ClassID, known
}

// itemCountOf answers one count: the class the script asked about, or 0.
func (h *Host) itemCountOf(value lua.LValue) int64 {
	classID, known := h.itemEntry(value)
	if !known {
		return 0
	}
	h.loadBag()
	if h.click == nil {
		return 0
	}
	return h.click.bag[classID]
}

// loadBag reads the bag once per click and remembers a failure, so a script that
// asks ten times does not write ten identical log lines.
func (h *Host) loadBag() {
	c := h.click
	if c == nil || c.bagLoaded {
		return
	}
	c.bagLoaded = true
	if h.Items == nil || c.commander == 0 {
		return
	}
	ctx := h.callCtx
	if ctx == nil {
		ctx = context.Background()
	}
	stacks, err := h.Items.Stacks(ctx, c.commander)
	if err != nil {
		// Not fatal: the click still runs, and every count is 0 - which is what it
		// was before this layer existed. The log line is what stops "the shop says
		// I have nothing" from looking like a data problem.
		h.logf("script: could not read commander %d's bag (%v) - every item count "+
			"this click answers 0", c.commander, err)
		return
	}
	c.bag = make(map[int64]int64, len(stacks))
	c.first = make(map[int64]int64, len(stacks))
	for _, st := range stacks {
		c.bag[st.ClassID] += st.Count
		// The FIRST stack of a class is the only one a give may join, so its size
		// is what decides a FULL INVENTORY.
		if _, seen := c.first[st.ClassID]; !seen {
			c.first[st.ClassID] = st.Count
		}
	}
	c.slots = len(stacks)
}
