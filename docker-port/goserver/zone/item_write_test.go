package zone

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/db"
	"geserver/item"
)

// TestItemWriteRoundTrip proves the WRITE path against the real schema: the minted
// id's shape, the merge into an existing stack, the delete at zero, and that a take
// empties the bag rather than leaving a zero row the client would still draw.
//
// It is GUARDED because it WRITES (GE_ITEM_DB_TEST=1), and it removes what it made.
// A class id of 900001 cannot collide with a real item, so a failed cleanup leaves
// one row that the next run deletes again.
func TestItemWriteRoundTrip(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const classID int64 = 900001
	store := NewPGStore(pool)
	// item rows are the ones this test mints; storage rows go with them by id.
	// Scoped cleanup: only THIS commander's rows of the test class.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	// --- give 3: a new stack, and the id has the game's shape.
	changes, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 3, Merge: true, Name: "RoundTrip"}})
	if err != nil {
		t.Fatalf("give 3: %v", err)
	}
	if len(changes) != 1 || changes[0].Total != 3 {
		t.Fatalf("give 3 reported %+v", changes)
	}
	minted := changes[0].ItemID
	if base, counter := minted>>32, minted&0xffffffff; base < 100000 || counter == 0 {
		t.Fatalf("minted id %d does not have the (base<<32)+counter shape", minted)
	}
	stacks, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks: %v", err)
	}
	if n := countClass(stacks, classID); n != 3 {
		t.Fatalf("the bag holds %d, want 3", n)
	}

	// --- give 2 more: it MERGES into that same stack, never a second one.
	changes, err = store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 2, Merge: true, Name: "RoundTrip"}})
	if err != nil {
		t.Fatalf("give 2 more: %v", err)
	}
	if changes[0].ItemID != minted {
		t.Fatalf("the second give minted %d instead of joining %d", changes[0].ItemID, minted)
	}
	if changes[0].Total != 5 {
		t.Fatalf("merged total = %d, want 5", changes[0].Total)
	}

	// --- take all 5: the row must be GONE, not left at zero.
	changes, err = store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: false, ClassID: classID, Count: 5, Name: "RoundTrip"}})
	if err != nil {
		t.Fatalf("take 5: %v", err)
	}
	if len(changes) != 1 || changes[0].Total != 0 {
		t.Fatalf("take 5 reported %+v, want one change with total 0", changes)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM storage WHERE item_id = $1 AND commander_id = $2`,
		minted, commanderID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d storage row(s) left after an emptied take, want 0", rows)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM item WHERE item_id = $1`, minted).Scan(&rows); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d item row(s) left after an emptied take, want 0", rows)
	}
}

// TestTakeFromAPartlyDrainedStack: a take smaller than the stack leaves the
// remainder, which is the common case (a fee out of a big pile).
func TestTakeFromAPartlyDrainedStack(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const classID int64 = 900002
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	if _, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 10, Merge: true}}); err != nil {
		t.Fatalf("give 10: %v", err)
	}
	changes, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: false, ClassID: classID, Count: 4}})
	if err != nil {
		t.Fatalf("take 4: %v", err)
	}
	if len(changes) != 1 || changes[0].Total != 6 {
		t.Fatalf("take 4 reported %+v, want a remainder of 6", changes)
	}
	stacks, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks: %v", err)
	}
	if n := countClass(stacks, classID); n != 6 {
		t.Fatalf("the bag holds %d, want 6", n)
	}
}

// TestTakeTheExactRow is why `item.Op.ItemID` exists. Two rows of ONE class, and a
// SALE must take the row the player pointed at, not the oldest one of that class -
// otherwise the client is told to decrement a row that still holds its count.
//
// Guarded like the rest of this file (it writes): GE_ITEM_DB_TEST=1.
func TestTakeTheExactRow(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const classID int64 = 900005
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	// Two rows of the same class, each in its own slot: equipment never merges.
	var ids []int64
	for _, count := range []int64{3, 7} {
		changes, err := store.CommitTx(ctx, commanderID, nil,
			[]item.Op{{Give: true, ClassID: classID, Count: count, Merge: false}})
		if err != nil {
			t.Fatalf("give %d: %v", count, err)
		}
		ids = append(ids, changes[0].ItemID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both gives minted item %d - they must be two rows", ids[0])
	}

	// Take 2 from the SECOND row by item id: the class-based take would have gone
	// to ids[0].
	changes, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: false, ClassID: classID, Count: 2, ItemID: ids[1]}})
	if err != nil {
		t.Fatalf("take 2 from item %d: %v", ids[1], err)
	}
	if len(changes) != 1 || changes[0].ItemID != ids[1] || changes[0].Total != 5 {
		t.Fatalf("take reported %+v, want item %d with 5 left", changes, ids[1])
	}

	// Give 1 back into the FIRST row by item id: no merge search may move it.
	changes, err = store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 1, ItemID: ids[0], Merge: true}})
	if err != nil {
		t.Fatalf("give 1 to item %d: %v", ids[0], err)
	}
	if changes[0].ItemID != ids[0] || changes[0].Total != 4 {
		t.Fatalf("give reported %+v, want item %d with 4", changes, ids[0])
	}

	stacks, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks: %v", err)
	}
	byID := map[int64]int64{}
	for _, st := range stacks {
		byID[st.ItemID] = st.Count
	}
	if byID[ids[0]] != 4 || byID[ids[1]] != 5 {
		t.Fatalf("rows after the exact-row write = %d and %d, want 4 and 5",
			byID[ids[0]], byID[ids[1]])
	}
}

// TestGiveWithMergeAddsToTheHeldStack is the bug report of 2026-10-30: a give of
// 20 with Merge, to a commander already holding 5, was said to REPLACE the held
// stack - a new item id, the old row gone. The write path ADDS: the count goes in
// the SAME row, under the SAME item id, and the bag gains no slot.
//
// The numbers are the incident's: WarpScroll (class 21006) has cap 32,767, the
// commander held 5, and the give was 20 - so 25 in one row is the only right answer.
// A give that deletes, renumbers or re-creates the held row fails this test.
//
// Guarded like the rest of this file (it writes): GE_ITEM_DB_TEST=1.
func TestGiveWithMergeAddsToTheHeldStack(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const (
		classID int64 = 900006 // the incident's shape: a mergeable class, big cap
		cap     int64 = 32767
	)
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	// The commander already holds 5.
	changes, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 5, Merge: true, MaxStack: cap, Name: "Held"}})
	if err != nil {
		t.Fatalf("give the held 5: %v", err)
	}
	held := changes[0].ItemID

	before, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks before: %v", err)
	}

	// Exactly what `mintkit -items 21006 -count 20` sends: Merge, the class's cap.
	changes, err = store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 20, Merge: true, MaxStack: cap, Name: "Give"}})
	if err != nil {
		t.Fatalf("give 20 on top of 5: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("the give reported %d change(s), want 1", len(changes))
	}
	if changes[0].ItemID != held {
		t.Fatalf("the give created item %d instead of joining the held row %d - "+
			"a held stack must never be replaced", changes[0].ItemID, held)
	}
	if changes[0].Total != 25 {
		t.Fatalf("the give reported total %d, want 25 (5 held + 20 given)", changes[0].Total)
	}

	after, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the bag went from %d row(s) to %d - a merged give must not open a slot",
			len(before), len(after))
	}

	// The database itself: one row of this class, the SAME item id, holding 25.
	var (
		rows  int
		count int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM item WHERE class_id = $1`, classID).Scan(&rows); err != nil {
		t.Fatalf("count item rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d item row(s) of class %d, want 1 - the held row was replaced", rows, classID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT i.count FROM item i JOIN storage s ON s.item_id = i.item_id
		  WHERE i.item_id = $1 AND s.commander_id = $2`, held, commanderID).Scan(&count); err != nil {
		t.Fatalf("read the held row %d: %v", held, err)
	}
	if count != 25 {
		t.Fatalf("the held row %d holds %d, want 25", held, count)
	}
}

// TestGivePastTheHeldStackIsRefusedNotReplaced: when the held row is nearly full,
// a give that does not fit it is a FULL INVENTORY, and the write path REFUSES it -
// the transaction writes nothing. Two rules matter here and both are checked:
//
//   - the held row is not touched, not renumbered, and not deleted (nothing lost);
//   - the second stack the give does not fit into is NOT opened from this path.
//
// The second rule is the shipped one: `script\tx.go` checkItemOp refuses the same
// give at call time with "the stack is full (MaxStack)", so a script never reaches
// this write. Splitting the remainder into a new row would change that rule on
// both sides, so it is a decision for the owner, not a fix made here.
//
// Guarded like the rest of this file (it writes): GE_ITEM_DB_TEST=1.
func TestGivePastTheHeldStackIsRefusedNotReplaced(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const (
		classID int64 = 900007
		cap     int64 = 32767
	)
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	// 32,760 of a 32,767 cap: seven short of full.
	changes, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 32760, Merge: true, MaxStack: cap}})
	if err != nil {
		t.Fatalf("give the held 32760: %v", err)
	}
	held := changes[0].ItemID

	before, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks before: %v", err)
	}

	// 20 more does not fit seven: refused, and the whole transaction writes nothing.
	if _, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 20, Merge: true, MaxStack: cap}}); err == nil {
		t.Fatal("a give past the held stack was accepted - the client cannot draw that")
	}

	after, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the refused give changed the bag from %d row(s) to %d, want no change",
			len(before), len(after))
	}
	var (
		rows  int
		count int64
	)
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM item WHERE class_id = $1`, classID).Scan(&rows); err != nil {
		t.Fatalf("count item rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d item row(s) of class %d after a refused give, want 1", rows, classID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT i.count FROM item i JOIN storage s ON s.item_id = i.item_id
		  WHERE i.item_id = $1 AND s.commander_id = $2`, held, commanderID).Scan(&count); err != nil {
		t.Fatalf("read the held row %d: %v", held, err)
	}
	if count != 32760 {
		t.Fatalf("the held row %d holds %d after a refused give, want 32760 untouched", held, count)
	}
}

// countClass adds up what the bag holds of one class.
func countClass(stacks []item.Stack, classID int64) int64 {
	var total int64
	for _, st := range stacks {
		if st.ClassID == classID {
			total += st.Count
		}
	}
	return total
}

// throwawayCommander is commander 25, the throwaway test account. The item
// tests used to take the FIRST commander in the table, which is commander 24 -
// the owner's. A test that mints stacks in the owner's bag and deletes items by
// class has no business running in a gate.
const throwawayCommander int32 = 25

// testItemCommander is the commander the item write tests use. It skips, rather
// than adopting another commander, when the throwaway account is absent.
func testItemCommander(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int32 {
	t.Helper()
	var id int32
	err := pool.QueryRow(ctx,
		`SELECT commander_id FROM commander WHERE commander_id = $1`,
		throwawayCommander).Scan(&id)
	if err != nil {
		t.Skipf("the throwaway test commander %d is not in the database (%v)",
			throwawayCommander, err)
	}
	return id
}

// clearTestClass removes the rows these tests mint, and only those: this
// commander's bag rows of the test class, and item rows of that class that no
// bag or worn row holds any more.
//
// It replaces a DELETE by class across the whole item table. A class id in the
// 900000s is test-only by convention, but a delete that is only correct while
// that convention holds is exactly the kind of "the gate cannot touch the
// owner's data" hole this card exists to close.
func clearTestClass(t *testing.T, pool *pgxpool.Pool, commanderID int32, classID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`DELETE FROM storage
		  WHERE commander_id = $1
		    AND item_id IN (SELECT item_id FROM item WHERE class_id = $2)`,
		commanderID, classID); err != nil {
		t.Errorf("clear class %d from commander %d's bag: %v", classID, commanderID, err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM item i
		  WHERE i.class_id = $1
		    AND NOT EXISTS (SELECT 1 FROM storage s WHERE s.item_id = i.item_id)
		    AND NOT EXISTS (SELECT 1 FROM wear_item w WHERE w.item_id = i.item_id)`,
		classID); err != nil {
		t.Errorf("clear the orphaned item rows of class %d: %v", classID, err)
	}
}

// TestEquipmentGetsItsOwnSlot is the bug this test was written for: the write path
// used to MERGE every give into the oldest stack of the class, so two swords would
// have shared one slot. `op.Merge` is the item table's word, and equipment says no.
func TestEquipmentGetsItsOwnSlot(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const classID int64 = 900003 // a sword: one per slot
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	for i := 0; i < 2; i++ {
		if _, err := store.CommitTx(ctx, commanderID, nil,
			[]item.Op{{Give: true, ClassID: classID, Count: 1, Merge: false}}); err != nil {
			t.Fatalf("give sword %d: %v", i+1, err)
		}
	}
	stacks, err := store.Stacks(ctx, commanderID)
	if err != nil {
		t.Fatalf("Stacks: %v", err)
	}
	n := 0
	for _, st := range stacks {
		if st.ClassID == classID {
			n++
			if st.Count != 1 {
				t.Fatalf("a sword stack holds %d, want 1 per slot", st.Count)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d sword slot(s), want 2 (equipment never merges)", n)
	}
}

// TestFullInventoryIsRefusedAtWriteTime: the cap is enforced where the write
// happens, not only in the script. Without this a direct call could push a stack
// past what the client can draw.
func TestFullInventoryIsRefusedAtWriteTime(t *testing.T) {
	if os.Getenv("GE_ITEM_DB_TEST") == "" {
		t.Skip("set GE_ITEM_DB_TEST=1 to run the item write round trip (it writes rows)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	const classID int64 = 900004
	store := NewPGStore(pool)
	// Scoped cleanup: only THIS commander's rows of the test class, and only the
	// item rows nobody else holds. The old delete was by class alone, which is a
	// delete across every commander in the database.
	defer clearTestClass(t, pool, commanderID, classID)
	clearTestClass(t, pool, commanderID, classID)

	if _, err := store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 3, Merge: true, MaxStack: 5}}); err != nil {
		t.Fatalf("first give: %v", err)
	}
	_, err = store.CommitTx(ctx, commanderID, nil,
		[]item.Op{{Give: true, ClassID: classID, Count: 3, Merge: true, MaxStack: 5}})
	if err == nil {
		t.Fatal("a give past MaxStack was accepted - the client cannot draw that")
	}
	// The refused transaction wrote NOTHING: the stack still holds 3.
	stacks, sErr := store.Stacks(ctx, commanderID)
	if sErr != nil {
		t.Fatalf("Stacks: %v", sErr)
	}
	if got := countClass(stacks, classID); got != 3 {
		t.Fatalf("the bag holds %d after a refused give, want 3", got)
	}
}
