package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ZoneDaemon is one row of `zone_daemon`: the PLAN for one map's process, in the shape the
// shipped v28 server kept in RESOURCE.DAEMON + ZONE_DAEMON (db/migrations/00015).
//
// The row says which map, on which port, switched on or off, and whether it is WARM. It is
// read by cmd/zoneman; nothing in a running zone depends on it, so editing it takes effect
// the next time the manager looks - not on a restart of the game.
type ZoneDaemon struct {
	DaemonID     int    `json:"daemon_id"`
	MapID        int    `json:"map_id"`
	Channel      int    `json:"channel"`
	Name         string `json:"name"`
	Port         int    `json:"port"`
	Kind         string `json:"kind"` // 'B' barrack, 'Z' zone
	ManagementOn bool   `json:"management_on"`
	Warm         bool   `json:"warm"`
	Note         string `json:"note"`
}

// LoadZoneDaemons reads every row, ordered by map id so two runs print the same plan. A
// table with no rows is not an error - it means the manager has nothing to do and says so.
func LoadZoneDaemons(ctx context.Context, pool *pgxpool.Pool) ([]ZoneDaemon, error) {
	rows, err := pool.Query(ctx, `SELECT daemon_id, map_id, channel, name, port, kind,
		management_on, warm, note FROM zone_daemon ORDER BY map_id`)
	if err != nil {
		return nil, fmt.Errorf("read zone_daemon: %w", err)
	}
	defer rows.Close()
	out := make([]ZoneDaemon, 0, 16)
	for rows.Next() {
		var d ZoneDaemon
		if err := rows.Scan(&d.DaemonID, &d.MapID, &d.Channel, &d.Name, &d.Port, &d.Kind,
			&d.ManagementOn, &d.Warm, &d.Note); err != nil {
			return nil, fmt.Errorf("scan zone_daemon: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
