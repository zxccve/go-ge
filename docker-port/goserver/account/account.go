// Package account creates accounts on the PostgreSQL stack: the login row the barrack
// reads at CB_LOGIN, and the password row beside it.
//
// WHY A PACKAGE: two callers need the same rules - the `mkaccount` command (a deployment
// makes one account from a shell) and the operator console's registration page (a player
// signs up from the client's own New Account button). A second copy of the transaction
// would drift the moment one of them learned something new.
//
// WHAT IT DOES NOT DO: it does not create a family (commander) or a character. The CLIENT
// asks for both - CB_CREATE_COMMANDER (11) and CB_CREATE_CHAR (18) in the barrack's own
// command table - so an account made here logs in to an EMPTY barrack, which is the screen
// where the player types their family name.
//
// THE PASSWORD IS UNSALTED MD5 HEX, the original engine's own scheme: its RESOURCE
// database keeps it in ACCOUNT_INFO.PASS, and 504 of its 507 rows are exactly 32 lowercase
// hex characters, md5("123456") among them (measured 2026-11-11). Nothing VERIFIES it yet:
// the client's CB_LOGIN carries a 16-byte digest that CHANGES between logins of the same
// account (protocol\login.go), so the column is correct data waiting for a check.
package account

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Create writes one account in ONE transaction: account_info (commander_id 0 on purpose -
// the client creates the family) and account_pass. It refuses an id that already exists
// CASE-INSENSITIVELY, because the login lookup does.
func Create(ctx context.Context, pool *pgxpool.Pool, member, pass string) (uint32, error) {
	if err := ValidateMemberID(member); err != nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// ONE WRITER AT A TIME. account_id is MAX+1 (the legacy table has no sequence), so two
	// simultaneous runs would pick the same id; the advisory lock serialises them and is
	// released by the transaction either way.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(71001)"); err != nil {
		return 0, fmt.Errorf("lock: %w", err)
	}

	var existing uint32
	err = tx.QueryRow(ctx,
		"SELECT account_id FROM account_info WHERE lower(member_id) = lower($1)", member,
	).Scan(&existing)
	switch {
	case err == nil:
		return 0, fmt.Errorf("account %q already exists (account_id %d)", member, existing)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("look up %q: %w", member, err)
	}

	var id uint32
	if err := tx.QueryRow(ctx,
		"INSERT INTO account_info (account_id, commander_id, member_id, class, special_ip) "+
			"VALUES ((SELECT COALESCE(MAX(account_id),0)+1 FROM account_info), 0, $1, 0, 0) "+
			"RETURNING account_id", member,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert account %q: %w", member, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO account_pass (member_id, pass_md5, cumulate_comm) VALUES ($1, $2, 0)",
		member, PassMD5(pass),
	); err != nil {
		return 0, fmt.Errorf("insert password row for %q: %w", member, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// PassMD5 is the column's own format: 32 lowercase hex characters, unsalted - measured
// from the original engine's RESOURCE.ACCOUNT_INFO (504 of 507 rows are exactly that,
// including md5("123456")). An empty password writes '-', which is "no password set".
func PassMD5(password string) string {
	if password == "" {
		return "-"
	}
	sum := md5.Sum([]byte(password))
	return hex.EncodeToString(sum[:])
}

// ValidateMemberID refuses what the login path and the launcher's accounts.ini cannot
// carry. A ':' or '=' would split the ini line, whitespace would be trimmed by one side and
// not the other, and an empty name is not an account.
func ValidateMemberID(member string) error {
	if member == "" {
		return errors.New("the member id is empty")
	}
	if strings.TrimSpace(member) != member {
		return fmt.Errorf("member id %q has leading or trailing whitespace", member)
	}
	if len(member) > 64 {
		return fmt.Errorf("member id is %d bytes; the limit is 64", len(member))
	}
	if strings.ContainsAny(member, ":=") {
		return fmt.Errorf("member id %q contains ':' or '=' - those split accounts.ini", member)
	}
	for _, r := range member {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("member id %q contains a space or a non-printable character", member)
		}
	}
	return nil
}
