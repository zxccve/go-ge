package db

// THE RECORDS THAT OUTLIVE A PROCESS (db/migrations/00018_usage_log.sql).
//
// Three tables, copied column for column from the databases the shipped server wrote (measured
// 2026-11-09): RESOURCE_LOG.SESSION_COUNT_ZONE, GE_LOG.ACCOUNT_LOGIN and GE_LOG.ACCOUNT_LOGOUT.
// They answer questions a log file cannot - "how many players were on map 15 at 21:00 last Tuesday",
// "which account logged in from where", "when did the last player leave" - and they are the reason the
// database is the oracle rather than a build artefact.
//
// A WRITE THAT FAILS MUST NOT BREAK THE THING IT RECORDS: every recorder here takes a context with a
// short deadline and returns an error the caller LOGS, never one it acts on. A full disk must not stop
// a login, and a broken login record must not stop a character from entering.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageGroupID is the GROUP the original stamped on every row. `RESOURCE.GROUP` has 2 rows in the
// imported database and a normal server runs one of them, so 1 is the only value this build writes.
const UsageGroupID = 1

// RecordSessionCount writes ONE sample of a zone's population - the row GAIA_ZONE_SESSION_COUNT
// produced once a minute in the shipped server (RESOURCE_LOG.SESSION_COUNT_ZONE).
func RecordSessionCount(ctx context.Context, pool *pgxpool.Pool, at time.Time, zoneID, channelID, connCount int) error {
	if pool == nil {
		return fmt.Errorf("no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO session_count_zone (log_date, group_id, zone_id, channel_id, conn_count)
		 VALUES ($1, $2, $3, $4, $5)`,
		at, UsageGroupID, zoneID, channelID, connCount)
	if err != nil {
		return fmt.Errorf("session_count_zone map %d: %w", zoneID, err)
	}
	return nil
}

// RecordLogin is GE_LOG.ACCOUNT_LOGIN: one row per successful login, with the address it came from.
func RecordLogin(ctx context.Context, pool *pgxpool.Pool, at time.Time, memberID, ip string) error {
	return recordAccountEvent(ctx, pool, "account_login", at, memberID, ip)
}

// RecordLogout is GE_LOG.ACCOUNT_LOGOUT: the mirror, written when the barrack connection ends.
func RecordLogout(ctx context.Context, pool *pgxpool.Pool, at time.Time, memberID, ip string) error {
	return recordAccountEvent(ctx, pool, "account_logout", at, memberID, ip)
}

// recordAccountEvent is the shared INSERT; `table` is chosen by the two exported wrappers and never by
// a caller, so the name cannot carry user text into the statement.
func recordAccountEvent(ctx context.Context, pool *pgxpool.Pool, table string, at time.Time, memberID, ip string) error {
	if pool == nil {
		return fmt.Errorf("no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO `+table+` (log_date, group_id, member_id, conn_ip) VALUES ($1, $2, $3, $4)`,
		at, UsageGroupID, memberID, ip)
	if err != nil {
		return fmt.Errorf("%s %q: %w", table, memberID, err)
	}
	return nil
}

// RecordInstanceUse is RESOURCE.INSTANCEHISTORY: the instance LOCK. Nothing calls it yet - this
// server has no instance maps - and it exists so the lane that adds them has one place to write.
func RecordInstanceUse(ctx context.Context, pool *pgxpool.Pool, instanceID int, at time.Time) error {
	if pool == nil {
		return fmt.Errorf("no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO instance_history (instance_id, date) VALUES ($1, $2)
		 ON CONFLICT (instance_id) DO NOTHING`, instanceID, at)
	if err != nil {
		return fmt.Errorf("instance_history %d: %w", instanceID, err)
	}
	return nil
}
