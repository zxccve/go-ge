package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"geserver/db"
	"geserver/protocol"
)

// The barrack half of "one live login per account" (card t_c38359e7): the zone closes
// the older SESSION (zone\live.go), and these tests prove the older BARRACK connection
// can no longer hand off at all.
//
// The rule is one comparison, so the tests are built to be READ as that comparison: the
// first test fails at "the superseded connection was sent 0..." when it is removed, and
// the others pin the three cases that must never refuse an ordinary player.

// fakeLoginSeq is an account's login sequence in memory - the same two operations
// PGStore performs, so the handler's rule is tested without a database and a test can
// never write the owner's account_info.
type fakeLoginSeq struct {
	mu     sync.Mutex
	latest map[uint32]int64
	// readErr makes LatestLoginSeq fail: the "the number cannot be read" case, which
	// must ANSWER the Start Game rather than strand the player.
	readErr error
	// writeErr makes NextLoginSeq fail: a login that could not be stamped.
	writeErr error
}

func newFakeLoginSeq() *fakeLoginSeq { return &fakeLoginSeq{latest: make(map[uint32]int64)} }

func (f *fakeLoginSeq) NextLoginSeq(_ context.Context, accountID uint32) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.latest[accountID]++
	return f.latest[accountID], nil
}

func (f *fakeLoginSeq) LatestLoginSeq(_ context.Context, accountID uint32) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return 0, f.readErr
	}
	return f.latest[accountID], nil
}

// seqHandler is quietHandler with the sequence wired and the LOG KEPT: the refusal is a
// log line as much as it is a missing packet, so the test has to read it.
func seqHandler(t *testing.T, s Store, seqs LoginSeqStore) (*Handler, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := quietHandler(t, s)
	h.LoginSeqs = seqs
	h.Log = log.New(&buf, "", 0)
	return h, &buf
}

// startGameWith sends one CB_STARTGAME for commander 24 and answers the packets.
func startGameWith(t *testing.T, h *Handler, sess *Session) [][]byte {
	t.Helper()
	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), sess,
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply(CB_STARTGAME): %v", err)
	}
	return out
}

// TestAnOlderBarrackLoginCannotStartTheGame IS the card's pass condition: two barrack
// logins on ONE account, the first one's Start Game refused once the second has logged
// in, both sequence numbers in ONE log line, and the handoff row untouched.
//
// RED when the comparison in supersededStartGame is removed: the first Start Game then
// gets a BC_STARTGAMEOK and writes pending_zone_entry, and the first two assertions here
// fail ("sent 1 packet(s), want 0").
func TestAnOlderBarrackLoginCannotStartTheGame(t *testing.T) {
	seqs := newFakeLoginSeq()
	h, logs := seqHandler(t,
		fakeStore{acc: Account{AccountID: 35, CommanderID: 24, MemberID: "arcanine"}, ok: true}, seqs)

	// saved is the pending_zone_entry writer, teamSaved the character-row writer: the
	// refusal must leave BOTH empty, which is the card's "pending_zone_entry is
	// untouched by the refusal".
	var saved []ZoneEntry
	var team [][3]uint16
	h.Barrack = fakeBarrack{
		found:     true,
		cmdr:      CommanderRow{ID: 24, Name: "Arcis", ZoneID: 10},
		saved:     &saved,
		teamSaved: &team,
	}

	// TWO CONNECTIONS, TWO LOGINS, ONE ACCOUNT - the frame is the real captured CB_LOGIN.
	first, second := &Session{}, &Session{}
	for i, sess := range []*Session{first, second} {
		if _, err := h.Reply(context.Background(), sess, capturedFrame(t)); err != nil {
			t.Fatalf("login %d: %v", i+1, err)
		}
	}
	if first.LoginSeq != 1 || second.LoginSeq != 2 {
		t.Fatalf("the logins were stamped %d and %d, want 1 and 2 (the second login is a "+
			"NEWER sequence on the same account)", first.LoginSeq, second.LoginSeq)
	}

	// THE OLDER CONNECTION ASKS TO LEAVE, after the newer login exists.
	logs.Reset()
	if out := startGameWith(t, h, first); len(out) != 0 {
		t.Fatalf("the superseded connection was sent %d packet(s), want 0: a Start Game "+
			"older than the account's latest login must be REFUSED, with no BC_STARTGAMEOK "+
			"for the client to follow", len(out))
	}
	if len(saved) != 0 {
		t.Fatalf("the refusal WROTE pending_zone_entry (%+v): the row must be left exactly "+
			"as the newer login finds it", saved)
	}
	if len(team) != 0 {
		t.Fatalf("the refusal wrote the character rows (%v): nothing may be written before "+
			"the refusal answers", team)
	}

	// ONE line, naming BOTH sequences: the refusal has to be diagnosable from the log.
	line := logs.String()
	if strings.Count(line, "REFUSED") != 1 || strings.Count(strings.TrimRight(line, "\n"), "\n") != 0 {
		t.Fatalf("the refusal is not ONE log line:\n%s", line)
	}
	for _, want := range []string{"sequence 1", "sequence 2"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the refusal line does not name %q:\n%s", want, line)
		}
	}

	// LAST POINT (23) IS THE SAME REQUEST and the same handoff row, so it is refused by
	// the same rule - a superseded connection cannot leave by either button.
	cont := startGameFrame(23, 1, [3]uint16{0, 3, EmptyCharSlot})
	if out, err := h.Reply(context.Background(), first,
		protocol.Frame{Cmd: 23, Size: len(cont), Data: cont}); err != nil || len(out) != 0 {
		t.Fatalf("CB_CONTINUEGAME on the superseded connection answered %d packet(s) (%v), "+
			"want 0: both buttons write the SAME handoff row", len(out), err)
	}
	if len(saved) != 0 || len(team) != 0 {
		t.Fatalf("CB_CONTINUEGAME on the superseded connection wrote %d handoff row(s) and "+
			"%d team row(s), want none", len(saved), len(team))
	}

	// AND THE LIVE CONNECTION STILL HANDS OFF: its sequence EQUALS the account's latest,
	// which is the live login - the comparison is a strict "<", so this never refuses a
	// player who has only one connection.
	out := startGameWith(t, h, second)
	if len(out) != 1 {
		t.Fatalf("the live connection was sent %d packet(s), want 1", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 25 {
		t.Fatalf("the live connection was answered with cmd %d, want 25 (BC_STARTGAMEOK)", cmd)
	}
	if len(saved) != 1 {
		t.Fatalf("pending_zone_entry holds %d row write(s) after the live Start Game, want 1",
			len(saved))
	}
	if saved[0].CommanderID != 24 || saved[0].Channel != 1 || saved[0].Continue {
		t.Fatalf("recorded %+v, want commander 24 channel 1 start", saved[0])
	}
	if len(team) != 1 || team[0] != [3]uint16{0, 3, EmptyCharSlot} {
		t.Fatalf("the live Start Game saved the team as %v, want [[0 3 255]]", team)
	}
}

// TestASingleLoginIsNeverRefused is the ordinary player: ONE login, one Start Game, and
// the answer and the writes are exactly what this barrack did before the rule existed.
func TestASingleLoginIsNeverRefused(t *testing.T) {
	var saved []ZoneEntry
	h, logs := seqHandler(t,
		fakeStore{acc: Account{AccountID: 35, CommanderID: 24, MemberID: "arcanine"}, ok: true},
		newFakeLoginSeq())
	h.Barrack = fakeBarrack{
		found: true,
		cmdr:  CommanderRow{ID: 24, Name: "Arcis", ZoneID: 10},
		saved: &saved,
	}

	sess := &Session{}
	if _, err := h.Reply(context.Background(), sess, capturedFrame(t)); err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.LoginSeq != 1 {
		t.Fatalf("a first login was stamped %d, want 1", sess.LoginSeq)
	}

	out := startGameWith(t, h, sess)
	if len(out) != 1 || binary.LittleEndian.Uint16(out[0]) != 25 {
		t.Fatalf("a single login's Start Game was answered with %d packet(s) (%v), want one "+
			"cmd 25", len(out), out)
	}
	if len(saved) != 1 {
		t.Fatalf("pending_zone_entry writes = %d, want 1", len(saved))
	}
	if strings.Contains(logs.String(), "REFUSED") {
		t.Fatalf("a single login was refused:\n%s", logs.String())
	}
}

// TestStartGameWithoutASequenceIsAnswered covers the two "unstamped" cases: a barrack
// with no sequence source at all (nil field and a Store that is not one - tests, and any
// caller from before this card), and an account whose latest login is well ahead while
// THIS connection carries 0 because its stamp failed. Neither may refuse: the rule is
// the barrack's hardening, and the zone's own claim is still the final authority.
func TestStartGameWithoutASequenceIsAnswered(t *testing.T) {
	t.Run("no sequence source", func(t *testing.T) {
		var saved []ZoneEntry
		// fakeStore does not implement LoginSeqStore, and LoginSeqs is nil: the barrack
		// cannot order logins, and must behave exactly as it did before.
		h := quietHandler(t, fakeStore{acc: Account{AccountID: 35, CommanderID: 24}, ok: true})
		h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{ID: 24, ZoneID: 10}, saved: &saved}

		sess := &Session{}
		if _, err := h.Reply(context.Background(), sess, capturedFrame(t)); err != nil {
			t.Fatalf("login: %v", err)
		}
		if sess.LoginSeq != 0 {
			t.Fatalf("session was stamped %d with no sequence source, want 0", sess.LoginSeq)
		}
		if out := startGameWith(t, h, sess); len(out) != 1 {
			t.Fatalf("Start Game answered with %d packet(s), want 1", len(out))
		}
		if len(saved) != 1 {
			t.Fatalf("pending_zone_entry writes = %d, want 1", len(saved))
		}
	})

	t.Run("stamp failed at login", func(t *testing.T) {
		seqs := newFakeLoginSeq()
		seqs.latest[35] = 7 // a newer login exists...
		seqs.writeErr = errors.New("database is down")
		var saved []ZoneEntry
		h, logs := seqHandler(t,
			fakeStore{acc: Account{AccountID: 35, CommanderID: 24}, ok: true}, seqs)
		h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{ID: 24, ZoneID: 10}, saved: &saved}

		sess := &Session{}
		if _, err := h.Reply(context.Background(), sess, capturedFrame(t)); err != nil {
			t.Fatalf("login: %v", err)
		}
		if sess.LoginSeq != 0 {
			t.Fatalf("a failed stamp left sequence %d, want 0", sess.LoginSeq)
		}
		if !strings.Contains(logs.String(), "could not stamp the login sequence") {
			t.Fatalf("a failed stamp was not said:\n%s", logs.String())
		}
		if out := startGameWith(t, h, sess); len(out) != 1 {
			t.Fatalf("an UNSTAMPED connection was sent %d packet(s), want 1: it cannot be "+
				"proved superseded, and a database failure must not refuse a player", len(out))
		}
	})
}

// TestStartGameIsAnsweredWhenTheLatestSequenceCannotBeRead is the read-failure half:
// the connection IS stamped (so the rule is armed), the read fails, and the player is
// answered anyway - with the reason in the log.
func TestStartGameIsAnsweredWhenTheLatestSequenceCannotBeRead(t *testing.T) {
	seqs := newFakeLoginSeq()
	seqs.latest[35] = 9
	seqs.readErr = errors.New("connection reset")
	var saved []ZoneEntry
	h, logs := seqHandler(t,
		fakeStore{acc: Account{AccountID: 35, CommanderID: 24}, ok: true}, seqs)
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{ID: 24, ZoneID: 10}, saved: &saved}

	sess := &Session{AccountID: 35, CommanderID: 24, LoginSeq: 3}
	if out := startGameWith(t, h, sess); len(out) != 1 {
		t.Fatalf("Start Game answered with %d packet(s), want 1: a failed READ must not "+
			"refuse a player", len(out))
	}
	if len(saved) != 1 {
		t.Fatalf("pending_zone_entry writes = %d, want 1", len(saved))
	}
	if !strings.Contains(logs.String(), "could not be read") {
		t.Fatalf("the failed read was not said:\n%s", logs.String())
	}
}

// TestPGStoreIsTheLoginSequenceSource pins the PRODUCTION wiring: main.go builds the
// Handler with `Store: barrack.NewPGStore(pool)` and does NOT set LoginSeqs, so the rule
// lives or dies on that Store answering the sequence interface. A compile-time assertion
// in login_seq.go covers the type; this proves the handler actually finds it through the
// field that is nil in production.
func TestPGStoreIsTheLoginSequenceSource(t *testing.T) {
	h := &Handler{Store: NewPGStore(nil)} // no pool: nothing here touches the database
	store, ok := h.loginSeqStore()
	if !ok || store == nil {
		t.Fatal("a Handler wired exactly like main.go (Store = PGStore, LoginSeqs nil) has " +
			"NO login sequence source: the rule would be dead code in production")
	}

	// And the explicit field still WINS when it is set - that is what the tests and any
	// other backend use.
	fake := newFakeLoginSeq()
	h.LoginSeqs = fake
	if got, ok := h.loginSeqStore(); !ok || got != fake {
		t.Fatal("LoginSeqs was set but loginSeqStore did not answer with it")
	}
}

// TestLoginSequenceInPostgreSQL proves the SQL half: `account_info.login_seq` really is
// a counter (one stamp per login, strictly increasing, latest reads back the last one)
// and a missing account reads as 0 rather than as an error.
//
// SKIPPED unless GE_LOGIN_SEQ_DB_TEST=1, because it WRITES a real account row. Point
// GE_PG_DSN at a SCRATCH database (ge_loadtest) when running it; the row's exact value
// is restored on the way out either way, in the style of extend_group_db_test.go.
func TestLoginSequenceInPostgreSQL(t *testing.T) {
	if os.Getenv("GE_LOGIN_SEQ_DB_TEST") == "" {
		t.Skip("set GE_LOGIN_SEQ_DB_TEST=1 (and GE_PG_DSN at a scratch database) to write " +
			"account_info.login_seq and read it back")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close) // LIFO: closes AFTER the restore below
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The LOWEST account id, whatever database this runs against, and its value is put
	// back exactly - a stamp is a deliberate write, but a test must leave no trace.
	var accountID uint32
	var before int64
	if err := pool.QueryRow(ctx,
		"SELECT account_id, login_seq FROM account_info ORDER BY account_id LIMIT 1").
		Scan(&accountID, &before); err != nil {
		t.Fatalf("read an account: %v", err)
	}
	t.Cleanup(func() {
		// The test's own ctx is cancelled by then, so the restore carries its own.
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		if _, err := pool.Exec(rctx,
			"UPDATE account_info SET login_seq = $2 WHERE account_id = $1",
			accountID, before); err != nil {
			t.Errorf("RESTORE account %d login_seq to %d: %v", accountID, before, err)
		}
	})

	store := NewPGStore(pool)
	first, err := store.NextLoginSeq(ctx, accountID)
	if err != nil {
		t.Fatalf("NextLoginSeq: %v", err)
	}
	second, err := store.NextLoginSeq(ctx, accountID)
	if err != nil {
		t.Fatalf("NextLoginSeq (second): %v", err)
	}
	if second != first+1 {
		t.Fatalf("stamps %d then %d, want the second to be exactly one higher", first, second)
	}
	latest, err := store.LatestLoginSeq(ctx, accountID)
	if err != nil {
		t.Fatalf("LatestLoginSeq: %v", err)
	}
	if latest != second {
		t.Fatalf("latest = %d after stamping %d, want the stamp to be the latest", latest, second)
	}

	// An account with no row answers 0 and no error: a deleted account has no newer
	// login to lose to, and 0 refuses nobody.
	if got, err := store.LatestLoginSeq(ctx, 2147483600); err != nil || got != 0 {
		t.Fatalf("LatestLoginSeq(missing) = %d, %v; want 0, nil", got, err)
	}
}
