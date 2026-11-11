// Package gatedb holds the guard that proves the GATE does not change the
// owner's database.
//
// It is not a test of a product path: it is the measurement the card
// "the gate writes to the owner's live database" (2026-11-08) asks for. The
// Extend-button test had raised commander 990401's Lock_TeamGroup to 20 and
// never put it back, so the next login refused the owner's Extend button at the
// cap. Per-test restores fix the seven writers; this guard is what proves the
// whole set leaves the database exactly as it found it.
package gatedb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/db"
)

// gateTables is every table the seven writing test files can touch, plus the
// commander and character rows they read and rewrite. A table added to those
// tests belongs here.
var gateTables = []string{
	"commander",
	"commander_property",
	"character",
	"character_position",
	"item",
	"storage",
	"wear_item",
	"saved_teleport",
	// The barrack's one-shot handoff (t_0cda12ac): the entry consume writes
	// consumed_at and deletes a stale row, so the guard must see it put back.
	"pending_zone_entry",
}

// writerTests are the DB-writing tests, by name. The flags that normally guard
// them are set by the guard itself: the point is to exercise EVERY writer, not
// just the ones a default `go test ./...` happens to run.
const writerTests = "TestExtendTeamGroupWritesThePropertyAndTakesTheStones" +
	"|TestTheTeamGroupRankIsTheCommandersProperty" +
	"|TestEquipDBMovesExactlyOneRow" +
	"|TestItemWriteRoundTrip" +
	"|TestTakeFromAPartlyDrainedStack" +
	"|TestTakeTheExactRow" +
	"|TestGiveWithMergeAddsToTheHeldStack" +
	"|TestGivePastTheHeldStackIsRefusedNotReplaced" +
	"|TestEquipmentGetsItsOwnSlot" +
	"|TestFullInventoryIsRefusedAtWriteTime" +
	"|TestLastPointRowInPostgreSQL" +
	"|TestStartPointDBRoundTrip" +
	"|TestSavedTeleportRowsInPostgreSQL" +
	"|TestSavedTeleportOrderInPostgreSQL" +
	"|TestPropertyRoundTripInPostgres" +
	// The handoff token (t_0cda12ac): both writers, consumed_at and the stale delete.
	"|TestAFreshHandoffIsServedThenTheSameSessionMoves" +
	"|TestAStaleHandoffIsRefusedWithItsAge"

// TestTheGateLeavesTheOwnersDatabaseAlone fingerprints the tables the writing
// tests can touch, runs those tests with their write flags ON, and fails if any
// table's checksum changed.
//
// It is SKIPPED unless GE_DB_CLEAN_GUARD=1, because it starts a nested `go test`
// and must not race a `go test ./...` that is already running those packages.
// The gate's command is:
//
//	GE_DB_CLEAN_GUARD=1 go test ./gatedb -run TestTheGateLeavesTheOwnersDatabaseAlone -v
//
// Run it with the game closed. A player looting or logging out writes item,
// storage and character_position rows, and this guard cannot tell that apart
// from a test that leaked.
func TestTheGateLeavesTheOwnersDatabaseAlone(t *testing.T) {
	if os.Getenv("GE_DB_CLEAN_GUARD") == "" {
		t.Skip("set GE_DB_CLEAN_GUARD=1 to run the nested writer run (it writes, then " +
			"checks the database is byte-identical)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	defer pool.Close()

	before := fingerprint(t, ctx, pool)

	run := exec.CommandContext(ctx, "go", "test", "./barrack", "./zone",
		"-run", "^("+writerTests+")$", "-count=1", "-v")
	// Tests run with their package folder as the working directory; gatedb's
	// parent is goserver, which is where `go test ./barrack ./zone` is valid.
	run.Dir = ".."
	run.Env = append(os.Environ(),
		"GE_EQUIP_DB_TEST=1",
		"GE_ITEM_DB_TEST=1",
		"GE_LAST_POINT_DB_TEST=1",
		"GE_SAVED_TELEPORT_DB_TEST=1",
		"GE_TX_DB_TEST=1",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the writer tests themselves failed, so the checksum proves nothing: %v\n%s",
			err, out)
	}

	after := fingerprint(t, ctx, pool)

	var changed []string
	for _, table := range gateTables {
		if before[table] != after[table] {
			changed = append(changed, fmt.Sprintf("  %-20s before %s  after %s",
				table, before[table], after[table]))
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		t.Fatalf("THE WRITER TESTS CHANGED THE DATABASE:\n%s\n"+
			"Every DB test must restore exactly what it changed (t.Cleanup runs even "+
			"after t.Fatalf), or run inside a transaction it rolls back.\n"+
			"NOTE: run this with the game closed - a live player's loot, log-out or "+
			"warp save changes these tables too.", strings.Join(changed, "\n"))
	}

	t.Logf("GATE DB CLEAN: %d table(s) byte-identical after the %d writer tests "+
		"(%s)", len(gateTables), strings.Count(writerTests, "|")+1,
		strings.Join(gateTables, ", "))
}

// fingerprint is one md5 per table over every row's text, in sorted order. It
// is a row checksum, not a schema checksum: a migration is a deliberate change
// and is not what this guard is about.
func fingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	out := make(map[string]string, len(gateTables))
	for _, table := range gateTables {
		q := fmt.Sprintf(
			"SELECT COALESCE(md5(string_agg(x::text, chr(10) ORDER BY x::text)), '') FROM %s x",
			table)
		var sum string
		if err := pool.QueryRow(ctx, q).Scan(&sum); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		out[table] = sum
	}
	return out
}
