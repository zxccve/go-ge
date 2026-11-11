package barrack

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/db"
	"geserver/protocol"
)

// This is the SQL half of the Extend button: the handler tests prove the packets,
// and this one proves the WRITE - the stones leave the bag, Lock_TeamGroup gains
// one, and a refusal leaves both exactly as they were.
//
// THE GATE MUST NOT CHANGE THE OWNER'S DATABASE. This file used to update
// commander 990401's Lock_TeamGroup to 20 and never put it back, and the owner's
// barrack then refused the Extend button at the cap (2026-11-08). Every row this
// file can touch is therefore SNAPSHOT FIRST and restored exactly - property
// rows AND bag rows, with their insert dates - by a t.Cleanup that runs even
// after t.Fatalf. The test also asserts the restoration itself, so a re-run is
// byte-identical and leaves nothing behind.
//
// It is not behind an env switch, because it never touches a real player: it
// refuses to run against a commander that is not its own (see
// ensureExtendCommander), and it creates only rows it deletes again. With no
// database it skips, exactly like oracle_test.go.
func TestExtendTeamGroupWritesThePropertyAndTakesTheStones(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	// t.Cleanup runs AFTER the function's own defers, so the pool must be closed
	// from a cleanup registered BEFORE the restore: LIFO closes it last.
	t.Cleanup(pool.Close)

	const (
		commanderID = extendTestCommander
		classID     = MoonStoneClassID
	)
	// Fixed, test-only item ids. They are deleted BY ID, never by class: a delete
	// by class would take a real player's Moon Stones with it.
	first, second, third := int64(9904010001), int64(9904010002), int64(9904010003)

	created := ensureExtendCommander(t, ctx, pool, commanderID)
	snap := snapshotCommanderRows(t, ctx, pool, commanderID)
	t.Cleanup(func() { restoreCommanderRows(t, pool, commanderID, snap, created) })

	// A CLEAN BAG AND A MISSING PROPERTY are the preconditions the phases below
	// read: phase (1) must see exactly the 6 stones it inserts at slot 5 (the
	// default), and phase (3) must be able to reach the cap. Both removals are
	// put back by the snapshot restore above.
	for _, id := range append(snapshotItemIDs(snap), first, second, third) {
		if _, err := pool.Exec(ctx, `DELETE FROM item WHERE item_id = $1`, id); err != nil {
			t.Fatalf("clear test item %d: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM storage WHERE commander_id = $1`, commanderID); err != nil {
		t.Fatalf("clear the bag of commander %d: %v", commanderID, err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM commander_property
		  WHERE commander_id = $1 AND property = 'Lock_TeamGroup' AND section = 0`,
		commanderID); err != nil {
		t.Fatalf("clear Lock_TeamGroup of commander %d: %v", commanderID, err)
	}

	// TWO stacks (2 + 4), so the price has to cross a stack boundary and the
	// emptied one must be deleted, not left at 0.
	if _, err := pool.Exec(ctx,
		`INSERT INTO item (item_id, class_id, count, type) VALUES ($1, $2, 2, 0)`,
		first, int32(classID)); err != nil {
		t.Fatalf("insert item %d: %v", first, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO item (item_id, class_id, count, type) VALUES ($1, $2, 4, 0)`,
		second, int32(classID)); err != nil {
		t.Fatalf("insert item %d: %v", second, err)
	}
	for _, id := range []int64{first, second} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage (item_id, commander_id) VALUES ($1, $2)`, id, commanderID); err != nil {
			t.Fatalf("insert storage for %d: %v", id, err)
		}
	}

	store := NewPGBarrackStore(pool)

	// (1) slot 5 (no property row: the default), 6 stones -> 5 taken, slot 6.
	res, err := store.ExtendTeamGroup(ctx, uint32(commanderID), classID)
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if res.Outcome != ExtendDone || res.Slot != 5 || res.NewSlot != 6 || res.Need != 5 ||
		res.Held != 6 || res.Taken != 5 {
		t.Fatalf("extend answered %+v, want Done 5->6 need 5 held 6 taken 5", res)
	}
	if got := propertyValue(t, ctx, pool, commanderID); got != "6" {
		t.Fatalf("Lock_TeamGroup = %q after the extension, want \"6\"", got)
	}
	if got := bagCount(t, ctx, pool, commanderID, classID); got != 1 {
		t.Fatalf("the bag holds %d stone(s) after paying 5 of 6, want 1", got)
	}
	if got := rowCount(t, ctx, pool, `SELECT count(*) FROM item WHERE item_id = $1`, first); got != 0 {
		t.Fatalf("the emptied stack %d still has an item row, want it deleted", first)
	}
	if got := rowCount(t, ctx, pool, `SELECT count(*) FROM storage WHERE item_id = $1`, first); got != 0 {
		t.Fatalf("the emptied stack %d still has a storage row, want it deleted", first)
	}
	if got := rowCount(t, ctx, pool,
		`SELECT count FROM item WHERE item_id = $1`, second); got != 1 {
		t.Fatalf("the second stack holds %d, want the 1 left after the take", got)
	}

	// (2) slot 6 now costs 5 and the bag holds 1: refused, NOTHING written.
	res, err = store.ExtendTeamGroup(ctx, uint32(commanderID), classID)
	if err != nil {
		t.Fatalf("second extend: %v", err)
	}
	if res.Outcome != ExtendNoStones || res.Slot != 6 || res.Need != 5 || res.Held != 1 {
		t.Fatalf("the short-bag call answered %+v, want NoStones at slot 6 (need 5, held 1)", res)
	}
	if got := propertyValue(t, ctx, pool, commanderID); got != "6" {
		t.Fatalf("Lock_TeamGroup = %q after a refusal, want it left at \"6\"", got)
	}
	if got := rowCount(t, ctx, pool,
		`SELECT count FROM item WHERE item_id = $1`, second); got != 1 {
		t.Fatalf("the second stack holds %d after a refusal, want the 1 untouched", got)
	}

	// (3) the cap: slot 20 with a full bag is refused, NOTHING written.
	if _, err := pool.Exec(ctx,
		`UPDATE commander_property SET value = '20'
		  WHERE commander_id = $1 AND property = 'Lock_TeamGroup'`, commanderID); err != nil {
		t.Fatalf("raise the slot to the cap: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO item (item_id, class_id, count, type) VALUES ($1, $2, 100, 0)`,
		third, int32(classID)); err != nil {
		t.Fatalf("insert the cap-test stack: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage (item_id, commander_id) VALUES ($1, $2)`, third, commanderID); err != nil {
		t.Fatalf("insert the cap-test storage row: %v", err)
	}
	res, err = store.ExtendTeamGroup(ctx, uint32(commanderID), classID)
	if err != nil {
		t.Fatalf("cap extend: %v", err)
	}
	if res.Outcome != ExtendAtCap || res.Slot != TeamGroupMaxSlot {
		t.Fatalf("the cap call answered %+v, want AtCap at slot %d", res, TeamGroupMaxSlot)
	}
	if got := propertyValue(t, ctx, pool, commanderID); got != "20" {
		t.Fatalf("Lock_TeamGroup = %q after the cap refusal, want \"20\"", got)
	}
	if got := rowCount(t, ctx, pool,
		`SELECT count FROM item WHERE item_id = $1`, third); got != 100 {
		t.Fatalf("the cap-test stack holds %d after the refusal, want the 100 untouched", got)
	}

	// THE CARD'S ASSERTION: put every row back NOW, and prove it. The property
	// must be at the value it held before the test (5 on this database, the game
	// data's Lock_TeamGroup), not the 20 the cap phase left, and the bag must be
	// the rows that were there before this file ran - 990401's own stacks
	// included, because "restore" means the snapshot, not "empty".
	restoreCommanderRows(t, pool, commanderID, snap, created)
	assertCommanderRows(t, ctx, pool, commanderID, snap, created)
	want := snapshotTeamGroup(snap)
	had, _ := propertyValueIfAny(t, ctx, pool, commanderID)
	if had != want {
		t.Fatalf("Lock_TeamGroup after the restore = %q, want the starting %q", had, want)
	}
	t.Logf("RESTORED: commander %d Lock_TeamGroup = %q (the starting value), "+
		"%d bag row(s) back", commanderID, had, len(snap.storage))
}

// TestTheLockTableCarriesTheCountAndCmd10TheCap REPLACES the 2026-11-08 test that
// asserted rank == the commander's Lock_TeamGroup. That rule was the bug, not the
// fix: the client's Extend guard is `Lock_TeamGroup < rank` (client 0x005426c1,
// history\barrack.md "2026-11-08 - the Extend gate"), so cmd 10's rank is the CAP
// (GET_TEAMGROUP_MAXSLOT) and cmd 32's Lock table is the CURRENT count. Sending
// the same number on both sides made the guard `n < n`.
//
// It drives the REAL handler against the REAL database, so both numbers are the
// ones a login would send: with the property at 7 the Lock table says 7 and rank
// stays the cap; with no property row the Lock table falls back to the game data's
// 5 - the same fallback ExtendTeamGroup charges its price from - and rank still
// does not move. The snapshot restore puts the row back.
func TestTheLockTableCarriesTheCountAndCmd10TheCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	// t.Cleanup runs AFTER the function's own defers, so the pool must be closed
	// from a cleanup registered BEFORE the restore: LIFO closes it last.
	t.Cleanup(pool.Close)

	const commanderID = extendTestCommander
	created := ensureExtendCommander(t, ctx, pool, commanderID)
	snap := snapshotCommanderRows(t, ctx, pool, commanderID)
	t.Cleanup(func() { restoreCommanderRows(t, pool, commanderID, snap, created) })

	store := NewPGBarrackStore(pool)

	// The handler composes BOTH packets, so the test asks it - not the builders -
	// what a login would carry. The two 2-byte stubs are replaced by the built
	// cmd 10 and cmd 32 inside startBarrack.
	h := quietHandler(t, fakeStore{ok: true})
	h.Barrack = store
	h.AfterStartBarrack = [][]byte{{10, 0}, {32, 0}}

	read := func(t *testing.T, wantLock int) {
		t.Helper()
		row, ok, err := store.Commander(ctx, uint32(commanderID))
		if err != nil {
			t.Fatalf("load commander %d: %v", commanderID, err)
		}
		if !ok {
			t.Fatalf("commander %d is not in the database", commanderID)
		}
		if int(row.TeamMax) != wantLock {
			t.Fatalf("Commander().TeamMax = %d, want %d (the commander's Lock_TeamGroup)",
				row.TeamMax, wantLock)
		}
		out, err := h.Reply(ctx, &Session{CommanderID: uint32(commanderID)},
			protocol.Frame{Cmd: 9, Size: 11})
		if err != nil {
			t.Fatalf("STARTBARRACK: %v", err)
		}
		var p10, p32 []byte
		for _, p := range out {
			if len(p) < 2 {
				continue
			}
			switch binary.LittleEndian.Uint16(p) {
			case 10:
				p10 = p
			case 32:
				p32 = p
			}
		}
		if p10 == nil || p32 == nil {
			t.Fatalf("the answer carries cmd 10 present=%v and cmd 32 present=%v, want both",
				p10 != nil, p32 != nil)
		}
		if got := int(binary.LittleEndian.Uint16(p10[rankAt:])); got != int(TeamGroupMaxSlot) {
			t.Fatalf("cmd 10's rank at +%d = %d, want the CAP %d (GET_TEAMGROUP_MAXSLOT), "+
				"not the commander's count %d", rankAt, got, TeamGroupMaxSlot, wantLock)
		}
		if got := lockTeamGroupOnWire(t, p32); got != wantLock {
			t.Fatalf("cmd 32's Lock_TeamGroup = %d, want the commander's Lock_TeamGroup %d",
				got, wantLock)
		}
	}

	// The property ROW wins on the COUNT side: 7 in the Lock table, while cmd 10
	// keeps saying the cap.
	if _, err := pool.Exec(ctx,
		`INSERT INTO commander_property (commander_id, property, section, value)
		 VALUES ($1, 'Lock_TeamGroup', 0, '7')
		 ON CONFLICT (commander_id, property, section)
		 DO UPDATE SET value = EXCLUDED.value`, commanderID); err != nil {
		t.Fatalf("set Lock_TeamGroup = 7: %v", err)
	}
	read(t, 7)

	// NO ROW: the game data's default (5) is the fallback, and it is the SAME
	// fallback ExtendTeamGroup charges its price from. cmd 10's rank still does not
	// move - it is the cap, not a copy of this value.
	if _, err := pool.Exec(ctx,
		`DELETE FROM commander_property
		  WHERE commander_id = $1 AND property = 'Lock_TeamGroup'`, commanderID); err != nil {
		t.Fatalf("clear Lock_TeamGroup: %v", err)
	}
	read(t, int(defaultTeamMax))

	restoreCommanderRows(t, pool, commanderID, snap, created)
	assertCommanderRows(t, ctx, pool, commanderID, snap, created)
	want := snapshotTeamGroup(snap)
	had, _ := propertyValueIfAny(t, ctx, pool, commanderID)
	if had != want {
		t.Fatalf("Lock_TeamGroup after the restore = %q, want the starting %q", had, want)
	}
}

// extendTestCommander is the commander these tests own. It is not a player: it
// was created by this file's own first version with account_id 0 and member_id
// "extendtest", and ensureExtendCommander REFUSES to touch it if that ever
// stops being true.
const extendTestCommander int32 = 990401

// ensureExtendCommander makes sure the test commander exists, and answers
// whether THIS RUN created it (so the restore can delete it again).
//
// It will not adopt a commander that belongs to somebody: the whole point of
// the card is that the gate must never write a player's row, and a test that
// "cleans" a real commander to get its fixture is exactly that bug.
func ensureExtendCommander(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32) (created bool) {
	t.Helper()
	var (
		accountID int32
		memberID  string
	)
	err := pool.QueryRow(ctx,
		`SELECT account_id, member_id FROM commander WHERE commander_id = $1`,
		commanderID).Scan(&accountID, &memberID)
	switch {
	case err == nil:
		if accountID != 0 || memberID != "extendtest" {
			t.Fatalf("commander %d belongs to account %d / member %q - this test only "+
				"runs against its OWN commander (account 0, member \"extendtest\"); it "+
				"will not overwrite a player's rows", commanderID, accountID, memberID)
		}
		return false
	case isNoRows(err):
		if _, err := pool.Exec(ctx,
			`INSERT INTO commander (commander_id, account_id, member_id, name)
			 VALUES ($1, 0, 'extendtest', 'Extend Test')`, commanderID); err != nil {
			t.Fatalf("insert the test commander %d: %v", commanderID, err)
		}
		return true
	default:
		t.Fatalf("read commander %d: %v", commanderID, err)
		return false
	}
}

// commanderPropertyRow, commanderItemRow and commanderStorageRow are every
// column of the rows this file changes. The insert date is carried because a
// "restored" bag row with a fresh insert_date is not the row that was there.
type commanderPropertyRow struct {
	Property string
	Section  int16
	Value    string
}

type commanderItemRow struct {
	ItemID  int64
	ClassID int32
	Count   int32
	Type    int16
}

type commanderStorageRow struct {
	ItemID     int64
	Type       int16
	Trash      int16
	InsertDate time.Time
}

type commanderRows struct {
	props   []commanderPropertyRow
	items   []commanderItemRow
	storage []commanderStorageRow
}

func snapshotItemIDs(snap commanderRows) []int64 {
	ids := make([]int64, 0, len(snap.storage))
	for _, st := range snap.storage {
		ids = append(ids, st.ItemID)
	}
	return ids
}

// snapshotTeamGroup is the starting Lock_TeamGroup value as TEXT, or "" when the
// commander has no such row (the game data's default applies then).
func snapshotTeamGroup(snap commanderRows) string {
	for _, p := range snap.props {
		if p.Property == "Lock_TeamGroup" && p.Section == 0 {
			return p.Value
		}
	}
	return ""
}

func snapshotCommanderRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32) commanderRows {
	t.Helper()
	var snap commanderRows

	rows, err := pool.Query(ctx,
		`SELECT property, section, value FROM commander_property
		  WHERE commander_id = $1 ORDER BY property, section`, commanderID)
	if err != nil {
		t.Fatalf("snapshot the properties of commander %d: %v", commanderID, err)
	}
	for rows.Next() {
		var p commanderPropertyRow
		if err := rows.Scan(&p.Property, &p.Section, &p.Value); err != nil {
			rows.Close()
			t.Fatalf("scan the property snapshot: %v", err)
		}
		snap.props = append(snap.props, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("properties of commander %d: %v", commanderID, err)
	}

	rows, err = pool.Query(ctx,
		`SELECT i.item_id, i.class_id, i.count, i.type, s.item_id, s.type, s.trash,
		        s.insert_date
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1
		  ORDER BY s.insert_date, s.item_id`, commanderID)
	if err != nil {
		t.Fatalf("snapshot the bag of commander %d: %v", commanderID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			it commanderItemRow
			st commanderStorageRow
		)
		if err := rows.Scan(&it.ItemID, &it.ClassID, &it.Count, &it.Type,
			&st.ItemID, &st.Type, &st.Trash, &st.InsertDate); err != nil {
			t.Fatalf("scan the bag snapshot: %v", err)
		}
		snap.items = append(snap.items, it)
		snap.storage = append(snap.storage, st)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("bag of commander %d: %v", commanderID, err)
	}
	return snap
}

// restoreCommanderRows puts the snapshot back EXACTLY: every property row, every
// bag row and its item row, with the insert dates they had. It deletes only
// rows of this commander, and it is idempotent, so it can run at the end of the
// test body and again from t.Cleanup.
func restoreCommanderRows(t *testing.T, pool *pgxpool.Pool, commanderID int32, snap commanderRows, created bool) {
	t.Helper()
	// A FRESH context: the test's own may be cancelled by the time a fatal
	// failure unwinds into this cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	current, err := pool.Query(ctx, `SELECT item_id FROM storage WHERE commander_id = $1`, commanderID)
	if err != nil {
		t.Errorf("restore: read the bag of commander %d: %v", commanderID, err)
		return
	}
	var currentIDs []int64
	for current.Next() {
		var id int64
		if err := current.Scan(&id); err == nil {
			currentIDs = append(currentIDs, id)
		}
	}
	current.Close()

	if _, err := pool.Exec(ctx, `DELETE FROM storage WHERE commander_id = $1`, commanderID); err != nil {
		t.Errorf("restore: clear the bag of commander %d: %v", commanderID, err)
		return
	}
	for _, id := range currentIDs {
		if _, err := pool.Exec(ctx, `DELETE FROM item WHERE item_id = $1`, id); err != nil {
			t.Errorf("restore: remove item %d: %v", id, err)
		}
	}
	for _, it := range snap.items {
		if _, err := pool.Exec(ctx,
			`INSERT INTO item (item_id, class_id, count, type) VALUES ($1, $2, $3, $4)`,
			it.ItemID, it.ClassID, it.Count, it.Type); err != nil {
			t.Errorf("restore: item %d: %v", it.ItemID, err)
		}
	}
	for _, st := range snap.storage {
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage (item_id, commander_id, type, trash, insert_date)
			 VALUES ($1, $2, $3, $4, $5)`,
			st.ItemID, commanderID, st.Type, st.Trash, st.InsertDate); err != nil {
			t.Errorf("restore: storage row %d: %v", st.ItemID, err)
		}
	}

	if _, err := pool.Exec(ctx,
		`DELETE FROM commander_property WHERE commander_id = $1`, commanderID); err != nil {
		t.Errorf("restore: clear the properties of commander %d: %v", commanderID, err)
		return
	}
	for _, p := range snap.props {
		if _, err := pool.Exec(ctx,
			`INSERT INTO commander_property (commander_id, property, section, value)
			 VALUES ($1, $2, $3, $4)`,
			commanderID, p.Property, p.Section, p.Value); err != nil {
			t.Errorf("restore: property %s/%d: %v", p.Property, p.Section, err)
		}
	}

	if created {
		if _, err := pool.Exec(ctx, `DELETE FROM commander WHERE commander_id = $1`, commanderID); err != nil {
			t.Errorf("restore: delete the test commander %d: %v", commanderID, err)
		}
	}
}

// assertCommanderRows fails unless the database now holds exactly the snapshot.
// This is the "back to its starting value" half of the card, measured rather
// than assumed: DeepEqual on every column of every row.
func assertCommanderRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32, snap commanderRows, created bool) {
	t.Helper()
	got := snapshotCommanderRows(t, ctx, pool, commanderID)
	if created {
		if len(got.props) != 0 || len(got.storage) != 0 || len(got.items) != 0 {
			t.Fatalf("commander %d was created by this run but its rows are still there: "+
				"%d propert(ies), %d bag row(s)", commanderID, len(got.props), len(got.storage))
		}
		return
	}
	if !reflect.DeepEqual(got.props, snap.props) {
		t.Fatalf("properties of commander %d were not restored:\n before %+v\n after  %+v",
			commanderID, snap.props, got.props)
	}
	if !reflect.DeepEqual(got.items, snap.items) {
		t.Fatalf("item rows of commander %d were not restored:\n before %+v\n after  %+v",
			commanderID, snap.items, got.items)
	}
	if !reflect.DeepEqual(got.storage, snap.storage) {
		t.Fatalf("bag rows of commander %d were not restored:\n before %+v\n after  %+v",
			commanderID, snap.storage, got.storage)
	}
}

// propertyValue reads Lock_TeamGroup as TEXT, so a wrong NUMBER cannot pass as a
// right one through a cast.
func propertyValue(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32) string {
	t.Helper()
	v, ok := propertyValueIfAny(t, ctx, pool, commanderID)
	if !ok {
		t.Fatalf("commander %d has no Lock_TeamGroup row", commanderID)
	}
	return v
}

// propertyValueIfAny is propertyValue for a row that may legitimately be absent,
// which is what the game data's default is: absent means 5.
func propertyValueIfAny(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32) (string, bool) {
	t.Helper()
	var v string
	err := pool.QueryRow(ctx,
		`SELECT value FROM commander_property
		  WHERE commander_id = $1 AND property = 'Lock_TeamGroup' AND section = 0`,
		commanderID).Scan(&v)
	if err != nil {
		if isNoRows(err) {
			return "", false
		}
		t.Fatalf("read Lock_TeamGroup: %v", err)
	}
	return v, true
}

// bagCount is every Moon Stone in the commander's bag, which is the number the
// price is checked against.
func bagCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, commanderID int32, classID uint32) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(sum(i.count), 0)
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND i.class_id = $2`, commanderID, int32(classID)).Scan(&n); err != nil {
		t.Fatalf("read the bag: %v", err)
	}
	return n
}

func rowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}
