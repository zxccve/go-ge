package db

import (
	"context"
	"testing"
	"time"
)

// TestUsageRecordsLand proves the three record tables exist and accept a row, against the
// real PostgreSQL - the same style as TestMigrateCreatesAccountTables, and it SKIPS when no
// database answers (a skip is not a proof; run it with the container up).
//
// It is the RED proof for db/migrations/00018_usage_log.sql: the columns are the imported
// server's own (RESOURCE_LOG.SESSION_COUNT_ZONE and GE_LOG.ACCOUNT_LOGIN/ACCOUNT_LOGOUT), so a
// query written against the original's tables runs here unchanged.
func TestUsageRecordsLand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := Open(ctx, DSN())
	if err != nil {
		t.Skipf("no PostgreSQL reachable (%s): %v", EnvDSN, err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A marker no other row can carry, so the reads below see exactly this test's rows.
	at := time.Now().UTC().Truncate(time.Millisecond)
	const member = "usage-test-member"

	if err := RecordSessionCount(ctx, pool, at, 15, 0, 7); err != nil {
		t.Fatalf("RecordSessionCount: %v", err)
	}
	if err := RecordLogin(ctx, pool, at, member, "127.0.0.1"); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
	if err := RecordLogout(ctx, pool, at, member, "127.0.0.1"); err != nil {
		t.Fatalf("RecordLogout: %v", err)
	}

	// SESSION_COUNT_ZONE: the original's five columns, read by the original's names.
	var zoneID, channel, count int
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, channel_id, conn_count FROM session_count_zone
		 WHERE log_date = $1 AND group_id = $2`, at, UsageGroupID).
		Scan(&zoneID, &channel, &count); err != nil {
		t.Fatalf("session_count_zone read: %v", err)
	}
	if zoneID != 15 || channel != 0 || count != 7 {
		t.Fatalf("session_count_zone = (zone %d, channel %d, count %d), want (15, 0, 7)",
			zoneID, channel, count)
	}

	// The two account tables: one row each, with the address kept as CONN_IP.
	for _, table := range []string{"account_login", "account_logout"} {
		var ip string
		if err := pool.QueryRow(ctx,
			`SELECT conn_ip FROM `+table+` WHERE log_date = $1 AND member_id = $2`,
			at, member).Scan(&ip); err != nil {
			t.Fatalf("%s read: %v", table, err)
		}
		if ip != "127.0.0.1" {
			t.Fatalf("%s conn_ip = %q, want 127.0.0.1", table, ip)
		}
	}

	// The instance lock: a second use of the same id must not duplicate the row.
	if err := RecordInstanceUse(ctx, pool, 424242, at); err != nil {
		t.Fatalf("RecordInstanceUse: %v", err)
	}
	if err := RecordInstanceUse(ctx, pool, 424242, at); err != nil {
		t.Fatalf("RecordInstanceUse twice: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instance_history WHERE instance_id = 424242`).Scan(&n); err != nil {
		t.Fatalf("instance_history read: %v", err)
	}
	if n != 1 {
		t.Fatalf("instance_history has %d rows for one id, want 1 (it is a LOCK)", n)
	}

	// Clean up this test's rows so a second run starts from the same place.
	if _, err := pool.Exec(ctx, `DELETE FROM session_count_zone WHERE log_date = $1`, at); err != nil {
		t.Fatalf("cleanup session_count_zone: %v", err)
	}
	for _, table := range []string{"account_login", "account_logout"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table+` WHERE member_id = $1`, member); err != nil {
			t.Fatalf("cleanup %s: %v", table, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM instance_history WHERE instance_id = 424242`); err != nil {
		t.Fatalf("cleanup instance_history: %v", err)
	}
}
