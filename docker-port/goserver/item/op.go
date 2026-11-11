package item

// A give or a take inside a script transaction.
//
// It lives in this package, not in `script` or `zone`, because it is ITEM-shaped:
// the script engine queues ops, the zone writes them, and both can name the same
// type without either importing the other.

// Op is one queued change. The script has already VALIDATED it at call time (the
// shipped rule: an unknown item or too small a stack answers 0 and fails the whole
// transaction), so a write path that meets a surprise here is a bug, not a game
// condition - it rolls the transaction back.
type Op struct {
	Give    bool
	ClassID int64
	Count   int64
	// ItemID names ONE bag ROW, and 0 means "resolve the row by class". It is set
	// by the SHOP paths, which is where it matters: a sale must take the exact row
	// the client was told to decrement, and a class-based take walks the oldest
	// stack first - two stacks of one class would put the change on the wrong row
	// and the client would show a count that is not what the database holds.
	ItemID int64
	// Merge says whether this give should join the stack the player already holds.
	// FALSE means it gets its own slot - equipment and anything the item table
	// marks unstackable. The write path cannot guess this: it is a property of the
	// item class, and only the item table knows it.
	Merge bool
	// MaxStack is the client's per-slot cap for that class (0 = not known). A give
	// that would pass it is a FULL INVENTORY and fails the transaction; it never
	// opens a second stack.
	MaxStack int64
	// Name is the script's spelling, for the log only. Two names can share one
	// class id, so the id is the truth and this is the courtesy.
	Name string
}

// Stack is one inventory row: the item id the client keys on, its class, and how
// many are in it. The bag is a list of these, oldest first - that order IS the
// client's inventory index.
type Stack struct {
	ItemID  int64
	ClassID int64
	Count   int64
}

// Change is what a COMMITTED op did, which is what the client has to be told:
// which stack (item id), and the NEW TOTAL it should now show - not the delta.
// A take that emptied a stack reports Total 0 and the row is gone.
type Change struct {
	Give    bool
	ClassID int64
	ItemID  int64
	Total   int64
}
