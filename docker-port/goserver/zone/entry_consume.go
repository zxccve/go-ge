// THE BARRACK'S HANDOFF IS A ONE-SHOT TOKEN (card t_0cda12ac). 
//
// `pending_zone_entry` is what the barrack's START button writes (barrack\
// barrack_store.go SaveZoneEntry) and what the zone reads when CZ_START_GAME arrives.
// Until this file it was written as an UPSERT and only ever SELECTed: nothing consumed
// it, nothing expired it, so a replayed CZ_START_GAME could enter with an old team for
// ever - the client never has to prove it just came from the barrack.
//
// The reference consumes the row once and refuses a stale one (offlinespada
// docs\kb\zone.md, "AUTH_TIME version": -9993 stale / -9994 missing / -9995 unknown,
// PROVEN on the stock binaries). Ours is the same rule with the material we have:
//
//   - the consume is ONE compare-and-swap: `UPDATE ... WHERE consumed_at IS NULL
//     RETURNING slot0, slot1, slot2`. A second entry finds no row and is refused;
//   - a row older than entryHandoffLimit is DELETED and refused with its age, so a
//     token left behind by a crash or a closed client cannot be picked up later;
//   - a commander with NO row is not refused: that is a direct zone login (a test, a
//     hand-made entry), the behaviour every commander had before this file, and the
//     zone falls back to the first character exactly as before.
//
// GAP, stated: CZ_START_GAME carries no version/token field that this server parses
// (zone\entry.go ParseStartGame: the sequence name and two ids), so the row's identity
// cannot be pinned to the PACKET. A replay that arrives after a NEW barrack pick and
// inside the 60 s window would consume that new token. Closing that needs the field the
// reference carries (AUTH_TIME) read out of the packet first - a packet job, not this one.
package zone

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// entryHandoffLimit is how long a barrack handoff stays usable. The reference's own
// window is not readable from our data (GAP), so this is a chosen number and it is the
// ONE place it is written: the SQL below passes it in seconds.
const entryHandoffLimit = 60 * time.Second

// entryReentryWindow is how long after a token was CONSUMED a further entry is still the
// same session moving between maps, rather than a replay. It is not a session timeout -
// the zone's own roster is what knows a player is inside.
const entryReentryWindow = 2 * time.Hour

// EntryRefused is a CZ_START_GAME the handoff cannot serve. It is a typed error so the
// entry can log a sentence a reader can act on instead of "team for account N: ...".
type EntryRefused struct {
	CommanderID int32
	AccountID   uint32
	Age         time.Duration // set for a stale row; zero for an already-used one
}

func (e *EntryRefused) Error() string {
	if e.Age > 0 {
		return fmt.Sprintf("the barrack handoff for commander %d is %s old, past the %s "+
			"limit - a stale entry is refused",
			e.CommanderID, e.Age.Round(100*time.Millisecond), entryHandoffLimit)
	}
	return fmt.Sprintf("the barrack handoff for commander %d was already used by an "+
		"earlier entry - a replayed CZ_START_GAME is refused", e.CommanderID)
}

// ConsumeZoneEntry takes this account's handoff ONCE and marks it used.
//
// It is called by the zone where CZ_START_GAME is read, BEFORE the team is loaded, so
// the row the team is built from is exactly the row this call took. A nil error means
// "go ahead": either the row was consumed now, or the account has no row at all (a
// direct entry). An *EntryRefused means the entry must not happen.
func (s *PGStore) ConsumeZoneEntry(ctx context.Context, accountID uint32) error {
	commanderID, ok, err := s.commanderOfAccount(ctx, accountID)
	if err != nil || !ok {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("consume the handoff of commander %d: %w", commanderID, err)
	}
	defer tx.Rollback(ctx) // a committed transaction makes this a no-op

	var s0, s1, s2 int16
	err = tx.QueryRow(ctx,
		`UPDATE pending_zone_entry
		    SET consumed_at = now()
		  WHERE commander_id = $1
		    AND consumed_at IS NULL
		    AND picked_at > now() - ($2 * interval '1 second')
		  RETURNING slot0, slot1, slot2`,
		commanderID, int(entryHandoffLimit/time.Second)).Scan(&s0, &s1, &s2)
	if err == nil {
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("consume the handoff of commander %d: %w", commanderID, err)
	}

	// NOTHING WAS TAKEN: say WHY, because the three answers are different events.
	var consumedAt *time.Time
	var age float64
	err = tx.QueryRow(ctx,
		`SELECT consumed_at, extract(epoch FROM now() - picked_at)::float8
		   FROM pending_zone_entry WHERE commander_id = $1`, commanderID).
		Scan(&consumedAt, &age)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx) // no handoff at all: a direct entry, allowed
	}
	if err != nil {
		return fmt.Errorf("read the handoff of commander %d: %w", commanderID, err)
	}
	if consumedAt != nil {
		// A GATE/WARP MOVE RE-USES THE LOGIN'S TOKEN (2026-11-10, measured in game): walking
		// through a door does NOT write a new pending_zone_entry - only the barrack's START
		// does - so the second, third and fiftieth map move of one session all arrive with the
		// row already consumed. Refusing them sits the player on the loading screen (the
		// owner: "stuck trying to move to Bahama").
		//
		// The rule is therefore about the SESSION, not the row: a token consumed recently means
		// the commander is in the world and moving, and a re-entry is allowed; a token consumed
		// LONG ago is a replay and is refused. The window is generous because a session can
		// cross maps for hours; the live-session claim (zone\live.go) is what keeps one body
		// per character in the meantime.
		if age := time.Since(*consumedAt); age < entryReentryWindow {
			return tx.Commit(ctx)
		}
		return &EntryRefused{CommanderID: commanderID, AccountID: accountID}
	}
	// STALE: delete it so it can never be used later, then report its age.
	if _, err := tx.Exec(ctx, `DELETE FROM pending_zone_entry WHERE commander_id = $1`,
		commanderID); err != nil {
		return fmt.Errorf("delete the stale handoff of commander %d: %w", commanderID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete the stale handoff of commander %d: %w", commanderID, err)
	}
	return &EntryRefused{
		CommanderID: commanderID,
		AccountID:   accountID,
		Age:         time.Duration(age * float64(time.Second)),
	}
}

// DeleteZoneEntry drops this account's handoff - the row has been served and the session
// is over (a logout), so nothing can be replayed from it later.
func (s *PGStore) DeleteZoneEntry(ctx context.Context, accountID uint32) error {
	commanderID, ok, err := s.commanderOfAccount(ctx, accountID)
	if err != nil || !ok {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM pending_zone_entry WHERE commander_id = $1`, commanderID); err != nil {
		return fmt.Errorf("delete the handoff of commander %d: %w", commanderID, err)
	}
	return nil
}

// commanderOfAccount resolves the commander an account plays. A missing account is not
// an error here: it answers ok=false and the handoff is simply absent.
func (s *PGStore) commanderOfAccount(ctx context.Context, accountID uint32) (int32, bool, error) {
	if accountID == 0 {
		return 0, false, nil
	}
	var commanderID int32
	err := s.pool.QueryRow(ctx,
		`SELECT commander_id FROM account_info WHERE account_id = $1`, int32(accountID)).
		Scan(&commanderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("account %d: %w", accountID, err)
	}
	return commanderID, true, nil
}
