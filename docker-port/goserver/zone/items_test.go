package zone

import (
	"context"
	"os"
	"testing"
	"time"

	"geserver/db"
)

// TestBagReadsTheRealDatabase proves the bag query against the live schema, which
// a mock would not: the join, the `count` column name, the trash/type filter and
// the GROUP BY all have to be right for a count to be right.
//
// It is READ-ONLY, so it runs with the rest of the suite. It SKIPS when the
// `storage` table is not there yet, because that means the zone has not been
// started since the migration was added.
func TestBagReadsTheRealDatabase(t *testing.T) {
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	defer pool.Close()

	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_name = 'storage')`).Scan(&present); err != nil {
		t.Skipf("cannot look up the schema (%v)", err)
	}
	if !present {
		t.Skip("no storage table yet - start the zone once so the migration runs")
	}

	var commanderID int32
	if err := pool.QueryRow(ctx,
		`SELECT commander_id FROM commander ORDER BY commander_id LIMIT 1`).
		Scan(&commanderID); err != nil {
		t.Skipf("commander is empty (%v)", err)
	}

	bag, err := NewPGStore(pool).Bag(ctx, commanderID)
	if err != nil {
		t.Fatalf("Bag(%d): %v", commanderID, err)
	}
	// A fresh character owns nothing, so an empty bag is the EXPECTED answer; what
	// this proves is that the query runs and groups.
	if bag.Counts == nil {
		t.Fatal("Bag returned a nil Counts map - every lookup would need a nil check")
	}
	for classID, total := range bag.Counts {
		if total <= 0 {
			t.Fatalf("class %d counted %d, want a positive total", classID, total)
		}
	}
	if bag.Slots < len(bag.Counts) {
		t.Fatalf("%d slots for %d class(es) - every class needs at least one row",
			bag.Slots, len(bag.Counts))
	}
}
