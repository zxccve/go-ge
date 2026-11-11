// ONE LIVE LOGIN PER ACCOUNT - the barrack half (card t_c38359e7).
//
// WHY. `zone\live.go` stopped two ZONE sessions from running the same characters
// (card t_487ea31c), but the BARRACK still let two connections on one account both
// press Start Game: each wrote its own `pending_zone_entry` row and each was told a
// zone address. Only the zone's own claim refused the second, and the older
// connection's handoff row was left in the table for whatever read it next.
//
// The reference keeps ONE session per account and closes the older one (offlinespada
// docs\kb\accounts-security.md:32; PROVEN on the stock binaries - see
// research\offlinespada-adopt-2026-11-09.md A1). The zone's half can only act on a
// connection that has ARRIVED; this half acts where the player asks to LEAVE.
//
// WHAT IS STAMPED. Every CB_LOGIN takes the account's next login sequence
// (`account_info.login_seq`, migration 00021) and the connection keeps it. The number
// lives in the DATABASE and not in a map in this process, because a restart forgets a
// map: after a restart the older client's socket is the only witness of its own login,
// and the row is what both that connection and the newer one can read.
//
// WHAT IS REFUSED. A CB_STARTGAME (22) or CB_CONTINUEGAME (23) whose stamped sequence
// is BELOW the account's latest login came from a superseded connection. It is refused
// BEFORE anything is written - no pending_zone_entry, no team on the character rows, no
// cmd 25 - with ONE log line naming both sequences, so the refusal is diagnosable from
// the log alone. (The card names the request CZ_START_GAME; in this server it is the
// client's CB_STARTGAME / CB_CONTINUEGAME, answered by BC_STARTGAMEOK.)
//
// WHAT IS NOT REFUSED, and the three cases are the whole "a normal player must never
// see a refusal" rule:
//
//   - a SINGLE login: its sequence EQUALS the latest one, and the test is a strict
//     "<", so the one connection an ordinary player has is answered exactly as before;
//   - a connection with no sequence (0): no source is wired, or the stamp failed. A
//     database hiccup must not lock a player out - the same rule the usage log follows
//     ("a record is never a reason to refuse a login");
//   - a Start Game whose latest login CANNOT BE READ: it is answered and the failure is
//     said once, because the zone's own claim is still the final authority.
//
// WHO OWNS THE ROW: PGStore (the login store, which already reads account_info). That
// is why main.go needs no new wiring - see Handler.loginSeqStore.
package barrack

import (
	"context"
	"fmt"
)

// LoginSeqStore is the account's login sequence: one number per login, held in
// account_info so a restart and a second connection read the same value.
//
// PGStore implements it; a test (or a deployment with another backend) supplies its
// own. The two calls are deliberately separate: the stamp happens at login, the read
// happens at Start Game, and a refusal must never depend on a write succeeding then.
type LoginSeqStore interface {
	// NextLoginSeq stamps a NEW login on this account and answers the sequence that
	// login owns. It must be ATOMIC: two clients logging in at the same moment get two
	// different numbers, never the same one (otherwise neither is "older").
	NextLoginSeq(ctx context.Context, accountID uint32) (int64, error)

	// LatestLoginSeq answers the account's latest stamped login. 0 means none has ever
	// been stamped, which is every account from before migration 00021 - and 0 refuses
	// nobody.
	LatestLoginSeq(ctx context.Context, accountID uint32) (int64, error)
}

// THE COMPILER PROVES THE PRODUCTION WIRING: main.go builds the Handler with
// Store = NewPGStore(pool) and never sets LoginSeqs, so `loginSeqStore`'s type assertion
// is the whole reason the rule is live in a deployment. If PGStore ever stops satisfying
// this, the build fails here instead of the rule quietly becoming dead code.
var _ LoginSeqStore = (*PGStore)(nil)

// loginSeqStore answers WHERE the sequence lives: the explicitly wired field when
// there is one, otherwise the login Store itself when it can do the job.
//
// THE FALLBACK IS WHY main.go DID NOT CHANGE. PGStore is the real implementation and it
// is already the Handler's Store (it owns the account row), so a deployment gets the
// sequence by construction. The field exists for a test - and for a backend that wants
// a different source - without either one having to grow the base Store interface,
// which every fake in this package would then have to implement for nothing.
//
// false means "no sequence source": nothing is stamped, and no Start Game is refused.
func (h *Handler) loginSeqStore() (LoginSeqStore, bool) {
	if h.LoginSeqs != nil {
		return h.LoginSeqs, true
	}
	if s, ok := h.Store.(LoginSeqStore); ok {
		return s, true
	}
	return nil, false
}

// stampLoginSeq gives this connection its place in the account's login order.
//
// A failure is NOT fatal and never refuses the login (see the file comment): the session
// keeps sequence 0, which supersededStartGame reads as "unstamped", and one line says
// exactly what protection is missing.
func (h *Handler) stampLoginSeq(ctx context.Context, sess *Session) {
	store, ok := h.loginSeqStore()
	if !ok || sess == nil || sess.AccountID == 0 {
		return
	}
	seq, err := store.NextLoginSeq(ctx, sess.AccountID)
	if err != nil {
		h.Log.Printf("LOGIN: could not stamp the login sequence for account %d (%v) - this "+
			"connection is not ordered against a later login, so a SECOND barrack login on "+
			"this account will not have this one's Start Game refused", sess.AccountID, err)
		return
	}
	sess.LoginSeq = seq
	h.Log.Printf("LOGIN: account %d is login sequence %d (one live login per account: a "+
		"Start Game stamped below this is refused)", sess.AccountID, seq)
}

// supersededStartGame reports whether this connection's Start Game is older than the
// account's latest login, and says so ONCE with both numbers.
//
// The three "no" answers are the design, not an oversight: an unstamped connection, a
// barrack with no sequence source and a failed read all leave the old behaviour in
// place. Only a READ SUCCESS that shows a newer login can refuse anything, and the
// comparison is strict - a connection whose sequence EQUALS the latest one is the live
// login and is answered.
func (h *Handler) supersededStartGame(ctx context.Context, sess *Session) bool {
	if sess == nil || sess.LoginSeq == 0 || sess.AccountID == 0 {
		return false
	}
	store, ok := h.loginSeqStore()
	if !ok {
		return false
	}
	latest, err := store.LatestLoginSeq(ctx, sess.AccountID)
	if err != nil {
		h.Log.Printf("STARTGAME: account %d's latest login sequence could not be read (%v) - "+
			"this Start Game is ANSWERED, because a failed read must not strand a player "+
			"in the barrack", sess.AccountID, err)
		return false
	}
	if sess.LoginSeq >= latest {
		return false
	}
	h.Log.Printf("STARTGAME: REFUSED - this connection logged in at sequence %d but account "+
		"%d has a NEWER login at sequence %d, so this Start Game came from a superseded "+
		"connection; no zone handoff was written (one live login per account)",
		sess.LoginSeq, sess.AccountID, latest)
	return true
}

// NextLoginSeq bumps the account's login sequence and answers the value this login owns.
//
// ONE STATEMENT, so the row lock it takes is what serialises two simultaneous logins:
// each UPDATE sees the other's committed value, and neither can hand out the number the
// other already holds. A read-then-write here would be exactly the race this rule exists
// to survive.
func (s *PGStore) NextLoginSeq(ctx context.Context, accountID uint32) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		"UPDATE account_info SET login_seq = login_seq + 1 WHERE account_id = $1 "+
			"RETURNING login_seq", accountID).Scan(&seq)
	if err != nil {
		if isNoRows(err) {
			// Cannot happen after a successful login lookup, and still not a reason to
			// pretend a sequence exists.
			return 0, fmt.Errorf("stamp a login on account %d: no such account", accountID)
		}
		return 0, fmt.Errorf("stamp a login on account %d: %w", accountID, err)
	}
	return seq, nil
}

// LatestLoginSeq answers the account's latest stamped login.
//
// A MISSING ROW ANSWERS 0, not an error: the account was deleted while this connection
// was open, and 0 refuses nobody - a deleted account has no newer login to lose to, and
// the login would be refused a moment later by the account lookup anyway. 0 is also
// "never stamped", which is every account from before migration 00021.
func (s *PGStore) LatestLoginSeq(ctx context.Context, accountID uint32) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		"SELECT login_seq FROM account_info WHERE account_id = $1", accountID).Scan(&seq)
	if err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read account %d's latest login: %w", accountID, err)
	}
	return seq, nil
}
