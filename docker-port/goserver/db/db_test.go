package db

import (
	"context"
	"testing"
	"time"
)

// TestMigrateCreatesAccountTables proves the schema is built against
// PostgreSQL, not described in a comment: it connects, migrates, migrates
// again (which must be a no-op), and then looks for the tables.
//
// The test SKIPS when no PostgreSQL answers, so the suite still runs on a
// machine without the container - but skip is not proof. Run it with the
// granado_pg container up before trusting the schema.
func TestMigrateCreatesAccountTables(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dsn := DSN()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("no PostgreSQL reachable (%s): %v", EnvDSN, err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// A second run must change nothing: goose records what already ran, and a
	// migration that is not idempotent breaks every restart.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate twice: %v", err)
	}

	for _, table := range []string{"account_info", "account_pass"} {
		var n int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.tables "+
				"WHERE table_schema = current_schema() AND table_name = $1",
			table).Scan(&n)
		if err != nil {
			t.Fatalf("look for %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s not found after migrating", table)
		}
	}
}

// TestDSNFromEnv proves the connection string can be overridden, because a
// deployment must not depend on the hard-coded development database.
func TestDSNFromEnv(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://someone@elsewhere:6543/other")
	if got := DSN(); got != "postgres://someone@elsewhere:6543/other" {
		t.Fatalf("DSN() = %q, want the value from %s", got, EnvDSN)
	}
}
