// Package barrack answers the client's login and (later) the barrack screens.
//
// It is deliberately small: protocol layout lives in the protocol package, and
// this package only holds the glue between a decoded packet, the database, and
// the reply. Anything that needs a byte offset belongs in protocol instead.
package barrack

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Account is the part of a login the server needs to answer.
type Account struct {
	AccountID   uint32
	CommanderID uint32
	MemberID    string // as stored in the database, which may be lower case
}

// Store looks an account up by the name the client sent.
//
// ok is false when the account does not exist - that is a normal answer, not an
// error, and the caller must not confuse the two: an error means the database is
// unhappy, ok=false means "this person is not registered".
type Store interface {
	Account(ctx context.Context, memberID string) (acc Account, ok bool, err error)
}

// PGStore is the PostgreSQL implementation. PostgreSQL is the only database
// this server knows; there is no SQL Server path and there never will be.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a connection pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Account looks the account up case-insensitively.
//
// THIS IS NOT LAZINESS. The legacy database stores the name as `arcanine` while
// the client sends `Arcanine`; Microsoft SQL Server's default collation treats
// those as equal, PostgreSQL does not, so an equality lookup finds nothing and
// the login fails for an account that plainly exists. See
// docker-port\history\postgres-migration.md.
func (s *PGStore) Account(ctx context.Context, memberID string) (Account, bool, error) {
	var acc Account
	err := s.pool.QueryRow(ctx,
		"SELECT account_id, commander_id, member_id FROM account_info "+
			"WHERE lower(member_id) = lower($1)", memberID,
	).Scan(&acc.AccountID, &acc.CommanderID, &acc.MemberID)
	if err != nil {
		if isNoRows(err) {
			return Account{}, false, nil
		}
		return Account{}, false, fmt.Errorf("look up account %q: %w", memberID, err)
	}
	return acc, true, nil
}
