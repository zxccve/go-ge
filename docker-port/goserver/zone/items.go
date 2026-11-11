package zone

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"geserver/item"
)

// The player's bag: what a script counts, and what a transaction writes.

// Stack is one inventory row: the item id the client keys on, its class, and how
// many are in it.
//
// It is `item.Stack`, not a type of its own, so the script engine can ask the zone
// for the bag without either package importing the other.

// Bag is the commander's bag: how many of each item class, and how many slots are
// in use. Both come from the same rows.
type Bag struct {
	// Counts is class id -> total count across that class's stacks.
	Counts map[int64]int64
	// Slots is the number of rows in the bag - `inventory->GetCount()` in the
	// original, which is what an inventory-full check compares against capacity.
	Slots int
}

// Stacks reads the bag ROWS, oldest first, which is the order the client draws.
//
// It is one query and the only place the bag is read: `Bag` aggregates this, the
// entry burst builds the inventory list from it, and a write path walks it. The
// order matters - a new item lands at the END of the client's list, and the
// inventory index the client uses is the position in exactly this sequence.
func (s *PGStore) Stacks(ctx context.Context, commanderID int32) ([]item.Stack, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.item_id, i.class_id, i.count
		   FROM storage s
		   JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND i.class_id IS NOT NULL AND i.class_id <> 0
		  ORDER BY s.insert_date, s.item_id`, commanderID)
	if err != nil {
		return nil, fmt.Errorf("stacks of commander %d: %w", commanderID, err)
	}
	defer rows.Close()

	var out []item.Stack
	for rows.Next() {
		var st item.Stack
		if err := rows.Scan(&st.ItemID, &st.ClassID, &st.Count); err != nil {
			return nil, fmt.Errorf("scan bag row: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// Bag reads the commander's bag, grouped by class. An empty bag is NOT an error:
// a fresh character owns nothing, and every count is then honestly 0.
func (s *PGStore) Bag(ctx context.Context, commanderID int32) (Bag, error) {
	out := Bag{Counts: map[int64]int64{}}
	stacks, err := s.Stacks(ctx, commanderID)
	if err != nil {
		return out, err
	}
	for _, st := range stacks {
		out.Counts[st.ClassID] += st.Count
		out.Slots++
	}
	return out, nil
}

// NextItemID mints an item id the way the shipped server does:
//
//	ITEM_ID = (base << 32) + counter
//
// `base` is claimed ONCE per process from the item_base_seq sequence (the stand-in
// for RESOURCE.dbo.INSTANCEHISTORY), and the counter runs 1, 2, 3 inside it. Two
// processes therefore never mint the same id, which is the one thing the shape is
// for.
//
// The base and the counter live on the PGStore, so they last as long as the server
// does; the mutex is the store's, because two stores must not share a counter.
func (s *PGStore) NextItemID(ctx context.Context) (int64, error) {
	s.idMu.Lock()
	defer s.idMu.Unlock()

	if s.idBase == 0 {
		if err := s.pool.QueryRow(ctx,
			`SELECT nextval('item_base_seq')`).Scan(&s.idBase); err != nil {
			return 0, fmt.Errorf("claim item id base: %w", err)
		}
	}
	s.idCounter++
	if s.idCounter >= 1<<32 {
		// 4 billion items in one run is not a game state; refusing beats wrapping
		// into the NEXT base and colliding with ids that base already issued.
		return 0, fmt.Errorf("item id counter exhausted (%d)", s.idCounter)
	}
	return (s.idBase << 32) + s.idCounter, nil
}

// CommitTx applies a script transaction: the property writes AND the item ops, in
// ONE database transaction. Everything the script queued lands, or none of it does.
//
// The ops run IN ORDER, because a script may take what it just gave (the corpus
// does), and each one re-reads the rows it touches with FOR UPDATE so two players
// cannot spend the same stack.
func (s *PGStore) CommitTx(ctx context.Context, commanderID int32, props map[string]int64, ops []item.Op) ([]item.Change, error) {
	if len(props) == 0 && len(ops) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin commit for commander %d: %w", commanderID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := writeProps(ctx, tx, commanderID, props); err != nil {
		return nil, err
	}

	var changes []item.Change
	for _, op := range ops {
		change, err := writeItem(ctx, tx, s, commanderID, op)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change...)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit for commander %d: %w", commanderID, err)
	}
	return changes, nil
}

// writeProps is the stage-1 property upsert, factored out so CommitTx and the
// property-only path cannot drift apart.
func writeProps(ctx context.Context, tx pgx.Tx, commanderID int32, props map[string]int64) error {
	for name, value := range props {
		if _, err := tx.Exec(ctx,
			`INSERT INTO commander_property (commander_id, property, section, value)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (commander_id, property, section)
			 DO UPDATE SET value = EXCLUDED.value`,
			commanderID, name, propertySectionNamed,
			fmt.Sprintf("%d", value)); err != nil {
			return fmt.Errorf("write %s=%d for commander %d: %w", name, value, commanderID, err)
		}
	}
	return nil
}

// writeItem applies one give or take and reports what the client should now show.
func writeItem(ctx context.Context, tx pgx.Tx, s *PGStore, commanderID int32, op item.Op) ([]item.Change, error) {
	if op.Give {
		return giveItem(ctx, tx, s, commanderID, op)
	}
	return takeItem(ctx, tx, commanderID, op)
}

// giveItem merges into the oldest stack when the ITEM CLASS allows it, and mints a
// new item id otherwise.
//
// The cap is enforced HERE as well as at call time, because this is the only place
// that writes: a give that would push the stack past `MaxStack` is a FULL INVENTORY
// and fails the whole transaction. It never opens a second stack - two stacks of
// one item is a state the client cannot draw sensibly.
//
// `op.Merge` is the item table's word, not a guess: equipment and unstackable items
// must get their own slot even when a stack of the same class exists.
func giveItem(ctx context.Context, tx pgx.Tx, s *PGStore, commanderID int32, op item.Op) ([]item.Change, error) {
	// A named row wins over the merge search: see addToRow.
	if op.ItemID != 0 {
		return addToRow(ctx, tx, commanderID, op)
	}
	if !op.Merge {
		return mintItem(ctx, tx, s, commanderID, op)
	}

	var (
		stackID int64
		have    int64
	)
	err := tx.QueryRow(ctx,
		`SELECT s.item_id, i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND i.class_id = $2
		  ORDER BY s.insert_date, s.item_id
		  LIMIT 1
		  FOR UPDATE OF i`, commanderID, op.ClassID).Scan(&stackID, &have)

	switch {
	case err == pgx.ErrNoRows:
		// Nothing of this class held: a new stack is the only answer, and it is
		// exactly what a merge would have done next.
		return mintItem(ctx, tx, s, commanderID, op)
	case err != nil:
		return nil, fmt.Errorf("find a stack of %d for commander %d: %w", op.ClassID, commanderID, err)
	}

	total := have + op.Count
	if op.MaxStack > 0 && total > op.MaxStack {
		// Refusing here is the same answer the script got at call time; it can only
		// differ when the two reads disagree, which is what this guard is for.
		return nil, fmt.Errorf("give %d of %d: stack %d holds %d, cap %d (FULL INVENTORY)",
			op.Count, op.ClassID, stackID, have, op.MaxStack)
	}
	if _, err := tx.Exec(ctx, `UPDATE item SET count = $2 WHERE item_id = $1`,
		stackID, total); err != nil {
		return nil, fmt.Errorf("grow stack %d: %w", stackID, err)
	}
	return []item.Change{{Give: true, ClassID: op.ClassID, ItemID: stackID, Total: total}}, nil
}

// addToRow grows ONE named bag row.
//
// WHY IT EXISTS: a script gives by CLASS, and merging into the oldest stack is
// right there. The SALE path pays into the vis row the client is currently
// showing, and a merge search could pick a different vis row when the player
// holds two - the client would then draw a total the database does not have.
func addToRow(ctx context.Context, tx pgx.Tx, commanderID int32, op item.Op) ([]item.Change, error) {
	var have int64
	err := tx.QueryRow(ctx,
		`SELECT i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND s.item_id = $2
		  FOR UPDATE OF i`, commanderID, op.ItemID).Scan(&have)
	if err == pgx.ErrNoRows {
		// The row the client was told about is gone. Writing into some other row
		// would move money nobody asked about, so the transaction fails.
		return nil, fmt.Errorf("give %d to item %d: not in commander %d's bag",
			op.Count, op.ItemID, commanderID)
	}
	if err != nil {
		return nil, fmt.Errorf("read item %d for commander %d: %w", op.ItemID, commanderID, err)
	}

	total := have + op.Count
	if op.MaxStack > 0 && total > op.MaxStack {
		return nil, fmt.Errorf("give %d to item %d: it holds %d, cap %d (FULL INVENTORY)",
			op.Count, op.ItemID, have, op.MaxStack)
	}
	if _, err := tx.Exec(ctx, `UPDATE item SET count = $2 WHERE item_id = $1`,
		op.ItemID, total); err != nil {
		return nil, fmt.Errorf("grow item %d: %w", op.ItemID, err)
	}
	return []item.Change{{Give: true, ClassID: op.ClassID, ItemID: op.ItemID, Total: total}}, nil
}

// mintItem gives an item a NEW id and its own slot.
func mintItem(ctx context.Context, tx pgx.Tx, s *PGStore, commanderID int32, op item.Op) ([]item.Change, error) {
	if op.MaxStack > 0 && op.Count > op.MaxStack {
		return nil, fmt.Errorf("give %d of %d in one slot, cap %d (FULL INVENTORY)",
			op.Count, op.ClassID, op.MaxStack)
	}
	newID, err := s.NextItemID(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO item (item_id, class_id, count, type) VALUES ($1, $2, $3, 0)`,
		newID, op.ClassID, op.Count); err != nil {
		return nil, fmt.Errorf("mint item %d (%s): %w", op.ClassID, op.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO storage (item_id, commander_id) VALUES ($1, $2)`,
		newID, commanderID); err != nil {
		return nil, fmt.Errorf("add item %d to commander %d: %w", newID, commanderID, err)
	}
	return []item.Change{{Give: true, ClassID: op.ClassID, ItemID: newID, Total: op.Count}}, nil
}

// takeItem consumes the amount from the OLDEST stacks first and deletes a stack
// that reaches zero.
//
// It splits across stacks on purpose: the player sees a total, not piles, so
// "take 5" from two piles of 3 must work - the shipped proc took one row at a
// time, which is why the Python oracle had to refuse that case.
//
// A named row (op.ItemID) skips all of that and goes to takeRow: a SALE removes
// the row the client is pointing at, not the oldest one of its class.
func takeItem(ctx context.Context, tx pgx.Tx, commanderID int32, op item.Op) ([]item.Change, error) {
	if op.ItemID != 0 {
		return takeRow(ctx, tx, commanderID, op)
	}
	rows, err := tx.Query(ctx,
		`SELECT s.item_id, i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND i.class_id = $2
		  ORDER BY s.insert_date, s.item_id
		  FOR UPDATE OF i`, commanderID, op.ClassID)
	if err != nil {
		return nil, fmt.Errorf("read stacks of %d: %w", op.ClassID, err)
	}
	type row struct{ id, have int64 }
	var stacks []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.have); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stack: %w", err)
		}
		stacks = append(stacks, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stacks of %d: %w", op.ClassID, err)
	}

	left := op.Count
	var changes []item.Change
	for _, st := range stacks {
		if left <= 0 {
			break
		}
		spend := left
		if spend > st.have {
			spend = st.have
		}
		rest := st.have - spend
		left -= spend
		if rest == 0 {
			// A zero-count row is a SLOT the client still draws, so the row goes -
			// the legacy proc leaned on a cascade, this deletes both halves.
			if _, err := tx.Exec(ctx, `DELETE FROM storage WHERE item_id = $1 AND commander_id = $2`,
				st.id, commanderID); err != nil {
				return nil, fmt.Errorf("remove stack %d: %w", st.id, err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM item WHERE item_id = $1`, st.id); err != nil {
				return nil, fmt.Errorf("remove item %d: %w", st.id, err)
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE item SET count = $2 WHERE item_id = $1`,
				st.id, rest); err != nil {
				return nil, fmt.Errorf("shrink stack %d: %w", st.id, err)
			}
		}
		changes = append(changes, item.Change{
			Give: false, ClassID: op.ClassID, ItemID: st.id, Total: rest,
		})
	}
	if left > 0 {
		return nil, fmt.Errorf("take %d of class %d: %d short", op.Count, op.ClassID, left)
	}
	return changes, nil
}

// takeRow consumes from ONE named row and deletes it at zero.
//
// WHY IT EXISTS: takeItem resolves its row by CLASS and walks the oldest stack
// first. That is right for a script ("pay 100 vis") and wrong for a SALE: the
// client is showing a specific bag position, and if the player holds the same
// class twice the oldest row is not the one they clicked. The change reported
// here is then the row that really changed, so the mirror and the database agree.
func takeRow(ctx context.Context, tx pgx.Tx, commanderID int32, op item.Op) ([]item.Change, error) {
	var (
		classID int64
		have    int64
	)
	err := tx.QueryRow(ctx,
		`SELECT i.class_id, i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND s.item_id = $2
		  FOR UPDATE OF i`, commanderID, op.ItemID).Scan(&classID, &have)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("take %d from item %d: not in commander %d's bag",
			op.Count, op.ItemID, commanderID)
	}
	if err != nil {
		return nil, fmt.Errorf("read item %d for commander %d: %w", op.ItemID, commanderID, err)
	}
	if op.Count <= 0 || op.Count > have {
		// The caller checked this against a read a moment ago; disagreeing here
		// means another writer got there first, and the whole transaction rolls
		// back rather than selling a row twice.
		return nil, fmt.Errorf("take %d from item %d: it holds %d", op.Count, op.ItemID, have)
	}

	rest := have - op.Count
	if rest == 0 {
		// A zero-count row is a SLOT the client still draws, so both halves go.
		if _, err := tx.Exec(ctx, `DELETE FROM storage WHERE item_id = $1 AND commander_id = $2`,
			op.ItemID, commanderID); err != nil {
			return nil, fmt.Errorf("remove storage row %d: %w", op.ItemID, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM item WHERE item_id = $1`, op.ItemID); err != nil {
			return nil, fmt.Errorf("remove item %d: %w", op.ItemID, err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE item SET count = $2 WHERE item_id = $1`,
		op.ItemID, rest); err != nil {
		return nil, fmt.Errorf("shrink item %d: %w", op.ItemID, err)
	}
	return []item.Change{{Give: false, ClassID: classID, ItemID: op.ItemID, Total: rest}}, nil
}
