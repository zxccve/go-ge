// THE HANDOFF IS A ONE-SHOT TOKEN (card t_0cda12ac) - proven against the live PostgreSQL,
// because the rule IS one SQL statement: a mock would only test the mock.
//
// The tests take one real commander's row, put it back exactly as they found it, and are
// listed in gatedb's gateTables so the GATE proves it.
package zone

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/db"
)

// entryFixture is one commander's pending_zone_entry row, kept so the test can put it
// back byte for byte.
type entryFixture struct {
	commanderID                int32
	channel                    int16
	slot0, slot1, slot2        int16
	isContinue                 bool
	pickedAt                   time.Time
	kind                       int16
	version                    int64
	consumedAt                 *time.Time
	had                        bool
}

func openEntryTestDB(t *testing.T) (*pgxpool.Pool, *PGStore) {
	t.Helper()
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	t.Cleanup(pool.Close)
	// The columns this card adds (version, consumed_at) come from migration 00020; the
	// server applies it at start, a test that runs alone must too. Idempotent by goose.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, NewPGStore(pool)
}

// saveAndRestoreEntry snapshots one commander's row and puts it back when the test ends.
func saveAndRestoreEntry(t *testing.T, pool *pgxpool.Pool, commanderID int32) {
	t.Helper()
	ctx := context.Background()
	var f entryFixture
	f.commanderID = commanderID
	err := pool.QueryRow(ctx,
		`SELECT channel, slot0, slot1, slot2, is_continue, picked_at, kind, version, consumed_at
		   FROM pending_zone_entry WHERE commander_id = $1`, commanderID).
		Scan(&f.channel, &f.slot0, &f.slot1, &f.slot2, &f.isContinue, &f.pickedAt, &f.kind,
			&f.version, &f.consumedAt)
	switch {
	case err == nil:
		f.had = true
	case errors.Is(err, pgx.ErrNoRows):
		f.had = false
	default:
		t.Fatalf("snapshot the handoff row: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if !f.had {
			_, _ = pool.Exec(ctx, `DELETE FROM pending_zone_entry WHERE commander_id = $1`, commanderID)
			return
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO pending_zone_entry
			   (commander_id, channel, slot0, slot1, slot2, is_continue, picked_at, kind, version, consumed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 ON CONFLICT (commander_id) DO UPDATE SET
			   channel = EXCLUDED.channel, slot0 = EXCLUDED.slot0, slot1 = EXCLUDED.slot1,
			   slot2 = EXCLUDED.slot2, is_continue = EXCLUDED.is_continue,
			   picked_at = EXCLUDED.picked_at, kind = EXCLUDED.kind,
			   version = EXCLUDED.version, consumed_at = EXCLUDED.consumed_at`,
			f.commanderID, f.channel, f.slot0, f.slot1, f.slot2, f.isContinue, f.pickedAt,
			f.kind, f.version, f.consumedAt)
		if err != nil {
			t.Errorf("RESTORE the handoff row of commander %d: %v", commanderID, err)
		}
	})
}

// entryAccount finds one account and its commander, and snapshots the row.
func entryAccount(t *testing.T, pool *pgxpool.Pool) (uint32, int32) {
	t.Helper()
	var accountID int32
	var commanderID int32
	err := pool.QueryRow(context.Background(),
		`SELECT a.account_id, a.commander_id FROM account_info a
		  WHERE a.commander_id IS NOT NULL ORDER BY a.account_id LIMIT 1`).
		Scan(&accountID, &commanderID)
	if err != nil {
		t.Skipf("account_info has no usable row (%v)", err)
	}
	saveAndRestoreEntry(t, pool, commanderID)
	return uint32(accountID), commanderID
}

// putEntry writes a handoff row the way the barrack does, with a chosen age and a chosen
// consume mark.
func putEntry(t *testing.T, pool *pgxpool.Pool, commanderID int32, age time.Duration, consumed bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO pending_zone_entry
		   (commander_id, channel, slot0, slot1, slot2, is_continue, picked_at, kind, version, consumed_at)
		 VALUES ($1, 0, 0, 1, 2, false, now() - ($2 * interval '1 second'), 0, 7, $3)
		 ON CONFLICT (commander_id) DO UPDATE SET
		   picked_at = EXCLUDED.picked_at, consumed_at = EXCLUDED.consumed_at,
		   slot0 = EXCLUDED.slot0, slot1 = EXCLUDED.slot1, slot2 = EXCLUDED.slot2,
		   version = EXCLUDED.version`,
		commanderID, int(age/time.Second), consumedAtOrNil(consumed))
	if err != nil {
		t.Fatalf("put the handoff row: %v", err)
	}
}

func consumedAtOrNil(consumed bool) *time.Time {
	if !consumed {
		return nil
	}
	now := time.Now()
	return &now
}

// TestAFreshHandoffIsServedThenTheSameSessionMoves is the card's pass condition, corrected in
// game on 2026-11-10: the token is consumed ONCE, but a GATE/WARP move re-uses the login's token
// (walking through a door writes no new pending_zone_entry), so a re-entry from the same session
// must be allowed - refusing it sat the owner on the loading screen ("stuck trying to move to
// Bahama"). A token consumed LONG ago is still a replay and is still refused.
func TestAFreshHandoffIsServedThenTheSameSessionMoves(t *testing.T) {
	pool, store := openEntryTestDB(t)
	accountID, commanderID := entryAccount(t, pool)
	putEntry(t, pool, commanderID, 0, false)

	ctx := context.Background()
	if err := store.ConsumeZoneEntry(ctx, accountID); err != nil {
		t.Fatalf("the first entry was refused: %v", err)
	}

	var consumed *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT consumed_at FROM pending_zone_entry WHERE commander_id = $1`, commanderID).
		Scan(&consumed); err != nil {
		t.Fatalf("read the consume mark: %v", err)
	}
	if consumed == nil {
		t.Fatal("consumed_at is still NULL after a whole entry - the row was not taken")
	}

	// A MAP MOVE moments later is the same session: allowed, and it does NOT need a new token.
	if err := store.ConsumeZoneEntry(ctx, accountID); err != nil {
		t.Fatalf("a map move was refused as a replay: %v", err)
	}

	// A token consumed LONG ago IS a replay: refused.
	if _, err := pool.Exec(ctx,
		`UPDATE pending_zone_entry SET consumed_at = now() - interval '3 hours'
		  WHERE commander_id = $1`, commanderID); err != nil {
		t.Fatalf("age the consume mark: %v", err)
	}
	err := store.ConsumeZoneEntry(ctx, accountID)
	var refused *EntryRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a 3-hour-old consumed token answered %v, want an *EntryRefused", err)
	}
	if refused.Age != 0 {
		t.Fatalf("the replay was reported as stale (%s) instead of already used", refused.Age)
	}
	if got := refused.Error(); got == "" {
		t.Fatal("the refusal has no sentence to log")
	}
}

// TestAStaleHandoffIsRefusedWithItsAge is the second pass condition: a row older than the
// window is refused, names its age, and is DELETED so it can never be used later.
func TestAStaleHandoffIsRefusedWithItsAge(t *testing.T) {
	pool, store := openEntryTestDB(t)
	accountID, commanderID := entryAccount(t, pool)
	putEntry(t, pool, commanderID, 120*time.Second, false)

	ctx := context.Background()
	err := store.ConsumeZoneEntry(ctx, accountID)
	var refused *EntryRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a 120 s old handoff answered %v, want an *EntryRefused", err)
	}
	if refused.Age < 100*time.Second || refused.Age > 140*time.Second {
		t.Fatalf("the refusal named an age of %s, want about 120 s", refused.Age)
	}
	if msg := refused.Error(); msg == "" || !contains(msg, "old") {
		t.Fatalf("the refusal does not name the age: %q", msg)
	}

	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pending_zone_entry WHERE commander_id = $1`, commanderID).
		Scan(&left); err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if left != 0 {
		t.Fatal("the stale row survived: a deleted token cannot be replayed later")
	}
}

// TestNoHandoffIsADirectEntry keeps the behaviour every commander had before this card:
// an account with no row (a direct zone login) enters normally.
func TestNoHandoffIsADirectEntry(t *testing.T) {
	pool, store := openEntryTestDB(t)
	accountID, commanderID := entryAccount(t, pool)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM pending_zone_entry WHERE commander_id = $1`, commanderID); err != nil {
		t.Fatalf("clear the row: %v", err)
	}
	if err := store.ConsumeZoneEntry(context.Background(), accountID); err != nil {
		t.Fatalf("a direct entry was refused: %v", err)
	}
}
