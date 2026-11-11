package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MachineIP reads ONE row of the address book: the ip this machine tells the game
// client to dial (migrations\00013_machine.sql, the original server's MACHINE
// table in spirit).
//
// A MISSING ROW IS NOT AN ERROR THE CALLER CANNOT SURVIVE. It comes back as
// pgx.ErrNoRows (the caller can test it with errors.Is), and the address package
// turns every failure here - missing row, empty ip, database unreachable - into
// the documented default plus ONE log line, because a server that cannot read its
// address book must still serve the players already on it.
//
// The row is read ONCE at startup and never again: a running role keeps its
// startup state (history\agent-traps.md), so an edit here takes effect at the next
// restart, which is also when the listener's port is chosen.
func MachineIP(ctx context.Context, pool *pgxpool.Pool, machine string) (string, error) {
	var ip string
	err := pool.QueryRow(ctx,
		`SELECT machine_ip FROM machine WHERE machine_name = $1`, machine).Scan(&ip)
	if err != nil {
		return "", err
	}
	return ip, nil
}
