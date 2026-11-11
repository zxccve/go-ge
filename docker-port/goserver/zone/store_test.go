package zone

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/barrack"
	"geserver/db"
)

// TestTeamOfReadsTheRealDatabase runs the SELECTs against the live PostgreSQL.
// It SKIPS when the database is not reachable, so `go test` still works on a
// machine without the containers - but on this machine it is the proof that the
// queries match the schema, which a mock would not give.
func TestTeamOfReadsTheRealDatabase(t *testing.T) {
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	defer pool.Close()

	var accountID int32
	if err := pool.QueryRow(ctx,
		`SELECT account_id FROM account_info ORDER BY account_id LIMIT 1`).
		Scan(&accountID); err != nil {
		t.Skipf("account_info is empty or unreadable (%v)", err)
	}

	store := NewPGStore(pool)
	team, err := store.TeamOf(ctx, uint32(accountID))
	if err != nil {
		t.Fatalf("TeamOf(%d): %v", accountID, err)
	}
	if len(team) == 0 {
		t.Fatalf("account %d has no characters - the client would sit on the "+
			"loading screen", accountID)
	}
	if len(team) > 3 {
		t.Fatalf("account %d got %d characters, a team is at most 3", accountID, len(team))
	}

	for i, c := range team {
		if c.Handle == 0 {
			t.Fatalf("character %d has handle 0 - the client keys everything on it", i)
		}
		if c.Name == "" {
			t.Fatalf("character %d has no name", i)
		}
		if c.Job == 0 {
			t.Fatalf("character %q has job 0, which draws the wrong class", c.Name)
		}
		t.Logf("%d: handle=%d name=%q job=%d level=%d hp=%.0f pos=%v",
			i, c.Handle, c.Name, c.Job, c.Level, c.HP, c.Position)
	}

	// The chosen order must follow the barrack's slots, not the slot column
	// order: the player picked that order and the client trusts it.
	var commanderID int32
	if err := pool.QueryRow(ctx,
		`SELECT commander_id FROM account_info WHERE account_id = $1`,
		accountID).Scan(&commanderID); err != nil {
		t.Fatalf("commander of account %d: %v", accountID, err)
	}
	t.Logf("barrack picked slots %v -> %d character(s)",
		store.pickedSlots(ctx, commanderID), len(team))

	// PROMOTION IS READ FROM THE ROW, BANDED BY THE LEVEL. The status script reads
	// GetPromotionLevel for SCR_Get_MSP (195 + Lv*5 + promotionLv*10), so a team
	// that spawns with 0 there computes the unpromoted max SP for a promoted
	// character - the one DIFF the sheet carried until this was wired. Checked
	// against the DATABASE rather than a constant: the expected answer is read
	// here, independently, and compared with what TeamOf produced.
	assertPromotion(t, ctx, pool, team)
}

// TestLoadPromotionCarriesAPromotedCharacter is the case the account above does
// not reach: its characters are three level-100 testers with no
// `character_property` row, so promotion 0 is correct there and proves little.
// This one takes a character that IS promoted (Amy, PromotionStep 4, band 4 at
// level 131) and checks the loader answers what the sheet needs: the step, banded,
// plus the raw cash step. Without this, a loader that quietly ignored the table
// would pass the team test above.
func TestLoadPromotionCarriesAPromotedCharacter(t *testing.T) {
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	defer pool.Close()

	var (
		commanderID int32
		charID      int32
		name        string
		step        int
	)
	err = pool.QueryRow(ctx,
		`SELECT c.commander_id, c.character_id, c.name, p.value::int
		   FROM character c
		   JOIN character_property p ON p.character_id = c.character_id
		  WHERE p.property = 'PromotionStep' AND p.value::int > 0
		  ORDER BY c.character_id LIMIT 1`).
		Scan(&commanderID, &charID, &name, &step)
	if err != nil {
		t.Skipf("no promoted character in this database (%v)", err)
	}

	rows, err := NewPGStore(pool).loadCharacters(ctx, commanderID)
	if err != nil {
		t.Fatalf("loadCharacters(commander %d): %v", commanderID, err)
	}
	for _, r := range rows {
		if int32(r.ID) != charID {
			continue
		}
		level := barrack.LevelOf(r.Exp)
		want := step
		if band := barrack.PromotionBand(level); want > band {
			want = band
		}
		if want == 0 {
			t.Fatalf("character %d %q at level %d has band 0, so step %d cannot be "+
				"checked - pick another character", charID, name, level, step)
		}
		if r.Promotion != want {
			t.Errorf("character %d %q (level %d): promotion %d, want %d "+
				"(row %d, band %d)", charID, name, level, r.Promotion, want,
				step, barrack.PromotionBand(level))
		}
		if r.CashPromotion == 0 {
			t.Errorf("character %d %q: cash promotion 0 - the raw row was not read",
				charID, name)
		}
		t.Logf("%d %q level %d: promotion %d (row %d, band %d), cash %d",
			charID, name, level, r.Promotion, step,
			barrack.PromotionBand(level), r.CashPromotion)
		return
	}
	t.Fatalf("character %d %q is not among the %d row(s) of commander %d",
		charID, name, len(rows), commanderID)
}

// assertPromotion reads `character_property` for the spawned characters and
// checks that each one carries min(PromotionStep, promotion_band(level)), with the
// cash step carried raw. Characters with no row must read 0 - "not promoted" is a
// real answer, not a missing one.
func assertPromotion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, team []Character) {
	t.Helper()
	ids := make([]int32, 0, len(team))
	for _, c := range team {
		ids = append(ids, int32(c.Handle))
	}
	rows, err := pool.Query(ctx,
		`SELECT character_id, property, value FROM character_property
		  WHERE character_id = ANY($1)
		    AND property IN ('PromotionStep', 'CashPromotionStep')`, ids)
	if err != nil {
		t.Fatalf("promotion steps of %v: %v", ids, err)
	}
	defer rows.Close()

	steps := make(map[uint32]int, len(ids))
	cash := make(map[uint32]int, len(ids))
	for rows.Next() {
		var (
			charID int32
			prop   string
			val    string
		)
		if err := rows.Scan(&charID, &prop, &val); err != nil {
			t.Fatalf("scan promotion step: %v", err)
		}
		n, convErr := strconv.Atoi(val)
		if convErr != nil {
			t.Fatalf("character %d %s = %q is not a number", charID, prop, val)
		}
		if prop == "PromotionStep" {
			steps[uint32(charID)] = n
		} else {
			cash[uint32(charID)] = n
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("promotion steps: %v", err)
	}

	for _, c := range team {
		want := steps[c.Handle]
		if band := barrack.PromotionBand(int(c.Level)); want > band {
			want = band
		}
		if int(c.Promotion) != want {
			t.Errorf("character %d %q (level %d): promotion %d, want %d "+
				"(row %d, band %d)", c.Handle, c.Name, c.Level, c.Promotion, want,
				steps[c.Handle], barrack.PromotionBand(int(c.Level)))
		}
		if int(c.CashPromotion) != cash[c.Handle] {
			t.Errorf("character %d %q: cash promotion %d, want %d (the raw row)",
				c.Handle, c.Name, c.CashPromotion, cash[c.Handle])
		}
		t.Logf("%d %q level %d -> promotion %d (row %d, band %d), cash %d",
			c.Handle, c.Name, c.Level, c.Promotion, steps[c.Handle],
			barrack.PromotionBand(int(c.Level)), c.CashPromotion)
	}
}

// TestGmLevelOfReadsTheRealDatabase proves migration 00014 and the query match the
// live schema, and that the OPERATOR seed is the one the owner's login gets. It
// SKIPS when PostgreSQL is not reachable, like the other store tests.
//
// The absent row is the important half: `account_level` carries GRANTS, and a member
// with no row must answer 0 - not an error, and not a default of 1.
func TestGmLevelOfReadsTheRealDatabase(t *testing.T) {
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	defer pool.Close()

	store := NewPGStore(pool)
	cases := []struct {
		member string
		want   int
	}{
		{"arcanine", 1},      // the operator's seed, not a rule
		{"Arcanine", 1},      // the client's own spelling (START_GAME account 35)
		{"test01", 0},        // seeded at 0, and 0 is what the session must see
		{"nobody-at-all", 0}, // absent row
	}
	for _, c := range cases {
		got, err := store.GmLevelOf(ctx, c.member)
		if err != nil {
			t.Fatalf("GmLevelOf(%q): %v", c.member, err)
		}
		if got != c.want {
			t.Fatalf("GmLevelOf(%q) = %d, want %d", c.member, got, c.want)
		}
	}
}

// accountLevelGrant is one `account_level` row as the GM tests snapshot it.
type accountLevelGrant struct {
	Member string
	Level  int16
}

// gmLevelTestStore opens the live PostgreSQL for a GM test, or SKIPS when it is
// not reachable - the same contract as the other store tests. The pool and the
// context are closed by t.Cleanup; a grant snapshot registered AFTER this call
// is restored BEFORE the pool closes, because cleanups run last-in-first-out.
func gmLevelTestStore(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	if os.Getenv("GE_SKIP_DB") != "" {
		t.Skip("GE_SKIP_DB is set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v) - this test needs the granado_pg container", err)
	}
	t.Cleanup(pool.Close)
	return NewPGStore(pool), pool, ctx
}

// clearGmProbeGrants removes every `account_level` row whose member_id starts
// with prefix (case-insensitively) and returns what it removed, so the test's
// cleanup can put exactly those rows back. The GM tests use synthetic names, so
// the owner's grants are never in this set - the gate's rule is that a test
// leaves the database as it found it (`gatedb`).
func clearGmProbeGrants(t *testing.T, pool *pgxpool.Pool, prefix string) []accountLevelGrant {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx,
		`SELECT member_id, level FROM account_level WHERE lower(member_id) LIKE lower($1)`,
		prefix+"%")
	if err != nil {
		t.Fatalf("snapshot account_level rows like %q: %v", prefix, err)
	}
	var old []accountLevelGrant
	for rows.Next() {
		var g accountLevelGrant
		if err := rows.Scan(&g.Member, &g.Level); err != nil {
			rows.Close()
			t.Fatalf("scan account_level row: %v", err)
		}
		old = append(old, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read account_level rows like %q: %v", prefix, err)
	}

	if _, err := pool.Exec(ctx,
		`DELETE FROM account_level WHERE lower(member_id) LIKE lower($1)`,
		prefix+"%"); err != nil {
		t.Fatalf("clear account_level rows like %q: %v", prefix, err)
	}
	return old
}

// restoreGmProbeGrants puts the snapshot back on a FRESH context: the test's own
// context may already be cancelled when the cleanup runs.
func restoreGmProbeGrants(t *testing.T, pool *pgxpool.Pool, prefix string, old []accountLevelGrant) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx,
		`DELETE FROM account_level WHERE lower(member_id) LIKE lower($1)`,
		prefix+"%"); err != nil {
		t.Errorf("cleanup: clear account_level rows like %q: %v", prefix, err)
		return
	}
	for _, g := range old {
		if _, err := pool.Exec(ctx,
			`INSERT INTO account_level (member_id, level) VALUES ($1, $2)`,
			g.Member, g.Level); err != nil {
			t.Errorf("cleanup: restore account_level %q = %d: %v", g.Member, g.Level, err)
		}
	}
}

// TestGmLevelOfMatchesAMemberInEitherCase is the live failure as a test. The
// client sends the login id as the player typed it (`START_GAME account 35
// ("Arcanine")`) while this database stored the grant as `arcanine`; PostgreSQL's
// `=` on varchar is case-sensitive, so the grant was silently read as level 0 and
// `//hpup` was refused (2026-11-08 23:09). A permission decision must not depend
// on capitalisation, in BOTH directions.
func TestGmLevelOfMatchesAMemberInEitherCase(t *testing.T) {
	store, pool, ctx := gmLevelTestStore(t)
	old := clearGmProbeGrants(t, pool, "GmCase")
	t.Cleanup(func() { restoreGmProbeGrants(t, pool, "GmCase", old) })

	if _, err := pool.Exec(ctx,
		`INSERT INTO account_level (member_id, level) VALUES
		   ('GmCaseStoredMixed', 3),
		   ('gmcasestoredlower', 4)`); err != nil {
		t.Fatalf("seed grants: %v", err)
	}

	cases := []struct {
		why    string
		member string
		want   int
	}{
		{"grant stored mixed case, client sends lower", "gmcasestoredmixed", 3},
		{"grant stored lower case, client sends mixed", "GmCaseStoredLower", 4},
	}
	for _, c := range cases {
		got, err := store.GmLevelOf(ctx, c.member)
		if err != nil {
			t.Fatalf("%s: GmLevelOf(%q): %v", c.why, c.member, err)
		}
		if got != c.want {
			t.Errorf("%s: GmLevelOf(%q) = %d, want %d", c.why, c.member, got, c.want)
		}
	}
}

// TestGmLevelOfTwoCaseVariantRowsAnswersOne pins the rule for the workaround that
// exists in this database today (`arcanine` AND `Arcanine`): the HIGHEST level
// wins, with the member_id bytes as the tie-break - never "whatever the planner
// returns". Every spelling must get the same answer on every call.
func TestGmLevelOfTwoCaseVariantRowsAnswersOne(t *testing.T) {
	store, pool, ctx := gmLevelTestStore(t)
	old := clearGmProbeGrants(t, pool, "GmCaseTwin")
	t.Cleanup(func() { restoreGmProbeGrants(t, pool, "GmCaseTwin", old) })

	if _, err := pool.Exec(ctx,
		`INSERT INTO account_level (member_id, level) VALUES
		   ('GmCaseTwin', 1),
		   ('gmcasetwin', 4)`); err != nil {
		t.Fatalf("seed case-variant grants: %v", err)
	}

	for _, spelling := range []string{"GmCaseTwin", "gmcasetwin", "GMCASETWIN", "gMcAsEtWiN"} {
		for i := 0; i < 10; i++ {
			got, err := store.GmLevelOf(ctx, spelling)
			if err != nil {
				t.Fatalf("GmLevelOf(%q): %v", spelling, err)
			}
			if got != 4 {
				t.Fatalf("GmLevelOf(%q) = %d, want 4 (highest of the two case-variant "+
					"rows); a plan-dependent answer is the bug this test exists for",
					spelling, got)
			}
		}
	}
}

// TestGmLevelOfAnAbsentMemberIsStillZero keeps the closed answer: `account_level`
// carries GRANTS, so a member with no row is level 0 and NOT an error - the
// caller must not log a database failure for an ordinary player.
func TestGmLevelOfAnAbsentMemberIsStillZero(t *testing.T) {
	store, pool, ctx := gmLevelTestStore(t)
	old := clearGmProbeGrants(t, pool, "GmCaseAbsent")
	t.Cleanup(func() { restoreGmProbeGrants(t, pool, "GmCaseAbsent", old) })

	got, err := store.GmLevelOf(ctx, "GmCaseAbsentNobody")
	if err != nil {
		t.Fatalf("GmLevelOf for an absent member returned an error: %v", err)
	}
	if got != 0 {
		t.Fatalf("GmLevelOf for an absent member = %d, want 0", got)
	}

	// The empty login id is the same closed answer, without asking the database.
	if got, err := store.GmLevelOf(ctx, ""); err != nil || got != 0 {
		t.Fatalf("GmLevelOf(\"\") = %d, %v; want 0, nil", got, err)
	}
}
