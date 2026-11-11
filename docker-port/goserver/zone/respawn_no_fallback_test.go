package zone

// NO RESPAWN FALLBACK - the owner's ruling, 2026-11-08 11:42:
//
//	"i dont want fallback, it can confuse or mislead us"
//
// (DECISIONS.md, "THE ORDER OF AUTHORITY" / no fallback numbers). The map spawner's
// `mongentype_<map>` row is the ONLY clock a return has, and `-respawn` is the operator's
// own override. There is no third arm: a body with no spawner row and no flag does not
// come back, and the zone says so once per class instead of inventing an interval.
//
// THE FOUR PASS CONDITIONS, one test each:
//
//	(a) TestABodyWithASpawnerRowComesBackOnThatRowsTime - two different RespawnTime
//	    values, each answered exactly; a constant in the code cannot pass both.
//	(b) TestABodyWithNoSpawnerRowNeverComesBack - no row, no flag: the corpse leaves,
//	    the body never returns however far the clock is driven, the corpse entry is
//	    dropped, and the "will not come back" line is written exactly ONCE for the class.
//	(c) TestTheRespawnFlagBringsBackABodyWithNoRow - the DEV flag still wins over both
//	    the row and the missing row.
//	(d) TestNoDecisionPathReturnsTheRecordingFallback - the deleted number (23.568 s,
//	    one recording, history\monster-death.md §5) survives only in COMMENTS that
//	    explain why it is gone. This reads the package's own source, so a later session
//	    cannot quietly put the default back.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// armSpawnerBody spawns one body through the REAL creation path with a spawner row's own
// interval on the spec, then kills it with one hit and answers the world, the player, the
// body and the instant of its death.
func armSpawnerBody(t *testing.T, row time.Duration) (*fakeWorld, *player, Monster, time.Time) {
	t.Helper()
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	spec := scarecrowSpawn()
	spec.Respawn = row
	spec.HasSpawner = true
	spec.SpawnerClassID = 1
	if _, err := w.SpawnMonsterObject(spec); err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}
	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) in the roster after one spawn, want 1", len(made))
	}
	at := killAndDead(t, w.Handler, made[0], p.sess.Team[0].Handle)
	return w, p, made[0], at
}

// (a) THE ROW'S OWN NUMBER, EXACTLY - two values, so a constant cannot pass.
func TestABodyWithASpawnerRowComesBackOnThatRowsTime(t *testing.T) {
	for _, row := range []time.Duration{2500 * time.Millisecond, 7 * time.Second} {
		// 2500 ms, not 1250: a row SHORTER than the corpse's own leave (the attack
		// branch's 2 s) is due before the body has left, and takeDueRespawns refuses a
		// corpse that has not left - the return would then ride the leave's tick and
		// this test could not stand one millisecond before it. Two values, both longer
		// than the leave, still make a constant in the code fail.
		t.Run(row.String(), func(t *testing.T) {
			w, p, mon, at := armSpawnerBody(t, row)
			c := corpseFor(t, w.Handler, mon.Handle)
			if c.noReturn {
				t.Fatalf("a body with a spawner row of %s was armed with NO return", row)
			}
			if got := c.backAt.Sub(at); got != row {
				t.Fatalf("the return is armed %s after the kill, want the row's own %s",
					got, row)
			}

			p.conn.writes = nil
			w.corpseTick(c.leaveAt) // the corpse leaves first
			w.corpseTick(c.backAt.Add(-time.Millisecond))
			if n := len(enterFrames(p.conn.sent())); n != 0 {
				t.Fatalf("%d 3527(s) went out one millisecond before the row's %s:\n%s",
					n, row, w.logText())
			}
			w.corpseTick(c.backAt)
			if n := len(enterFrames(p.conn.sent())); n != 1 {
				t.Fatalf("the socket has %d 3527(s) at the row's %s, want exactly 1:\n%s",
					n, row, w.logText())
			}
			if !strings.Contains(w.logText(), "this body's SPAWNER row (mongentype RespawnTime "+
				row.String()+")") {
				t.Errorf("the leave line does not name the spawner row and its %s:\n%s",
					row, w.logText())
			}
		})
	}
}

// (b) NO ROW, NO FLAG: the body never comes back, and the zone says so ONCE.
func TestABodyWithNoSpawnerRowNeverComesBack(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	const line = "has no spawner row and no -respawn flag - it will not come back"

	// THE FIRST PROBE BODY: no spawner row on the spec at all, exactly what
	// `-spawnmonster`, `-spawndummy` and a script's `CreateTmonster` pass.
	if _, err := w.SpawnMonsterObject(scarecrowSpawn()); err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}
	first := w.runtimeMonsters()[0]
	killAndDead(t, w.Handler, first, p.sess.Team[0].Handle)

	if got := strings.Count(w.logText(), line); got != 1 {
		t.Fatalf("%d \"%s\" line(s) after the first kill, want exactly 1:\n%s",
			got, line, w.logText())
	}
	if !strings.Contains(w.logText(), "class 11600 (Monster_Hersu01)") {
		t.Errorf("the line does not name the class and its row's name:\n%s", w.logText())
	}

	// The corpse still owes its LEAVE: a dead body must be taken off the client's world
	// even when no successor will be made. Its return is not armed at all.
	c := corpseFor(t, w.Handler, first.Handle)
	if !c.noReturn {
		t.Fatalf("a body with no spawner row was armed WITH a return")
	}
	if !c.backAt.IsZero() {
		t.Fatalf("a body with no spawner row has backAt %s, want the zero instant (no "+
			"return is armed)", c.backAt)
	}

	p.conn.writes = nil
	w.corpseTick(c.leaveAt)
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("%d 3531(s) at the corpse's leave, want exactly 1:\n%s", n, w.logText())
	}
	if got := len(w.runtimeMonsters()); got != 0 {
		t.Fatalf("%d body(ies) in the roster after the leave, want 0", got)
	}

	// DRIVE THE CLOCK FAR PAST EVERY NUMBER THAT USED TO STAND IN: the old fallback's
	// interval, then an hour. Nothing may come back, and the corpse entry must be gone
	// (so corpsePending cannot keep a session looping for a body that will never exist).
	p.conn.writes = nil
	for _, when := range []time.Time{
		c.leaveAt.Add(time.Millisecond),
		c.deadAt.Add(23568 * time.Millisecond),
		c.deadAt.Add(time.Hour),
	} {
		w.corpseTick(when)
	}
	if n := len(enterFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d 3527(s) came back for a body with no spawner row:\n%s", n, w.logText())
	}
	if got := len(w.runtimeMonsters()); got != 0 {
		t.Fatalf("%d body(ies) in the roster after the clock ran out, want 0", got)
	}
	if owed := corpsesOwed(t, w.Handler); len(owed) != 0 {
		t.Fatalf("a corpse that can never return is still owed after its leave: %+v", owed)
	}

	// A SECOND BODY OF THE SAME CLASS: the sentence is per class, so it must NOT repeat.
	if _, err := w.SpawnMonsterObject(scarecrowSpawn()); err != nil {
		t.Fatalf("SpawnMonsterObject (second body): %v", err)
	}
	second := w.runtimeMonsters()[0]
	killAndDead(t, w.Handler, second, p.sess.Team[0].Handle)
	if got := strings.Count(w.logText(), line); got != 1 {
		t.Fatalf("%d \"%s\" line(s) after a second kill of the same class, want exactly "+
			"1 (once per class, not once per body):\n%s", got, line, w.logText())
	}
	if c2 := corpseFor(t, w.Handler, second.Handle); !c2.noReturn {
		t.Fatalf("the second body of the class was armed WITH a return")
	}
}

// (c) THE FLAG OVERRIDES BOTH - a row and a missing row.
func TestTheRespawnFlagBringsBackABodyWithNoRow(t *testing.T) {
	t.Cleanup(func() { SetRespawnDelay(0) })
	SetRespawnDelay(1500 * time.Millisecond)

	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	if _, err := w.SpawnMonsterObject(scarecrowSpawn()); err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}
	mon := w.runtimeMonsters()[0]
	at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle)

	c := corpseFor(t, w.Handler, mon.Handle)
	if c.noReturn {
		t.Fatalf("with -respawn 1500ms a body with no spawner row was armed with NO return")
	}
	if got := c.backAt.Sub(at); got != 1500*time.Millisecond {
		t.Fatalf("the return is armed %s after the kill, want the flag's 1.5s", got)
	}
	if strings.Contains(w.logText(), "will not come back") {
		t.Errorf("the flag was on but the zone said the body will not come back:\n%s",
			w.logText())
	}

	w.corpseTick(c.leaveAt)
	w.corpseTick(c.backAt)
	if got := w.runtimeMonsters(); len(got) != 1 || got[0].Handle == mon.Handle {
		t.Fatalf("the body did not come back under the flag: roster %+v", got)
	}
	if !strings.Contains(w.logText(), "the -respawn DEV FLAG's number") {
		t.Errorf("the leave line does not name the DEV flag as the interval's source:\n%s",
			w.logText())
	}
}

// (d) THE DELETED NUMBER SURVIVES ONLY IN COMMENTS. Parsed, not grepped, so a comment
// that merely MENTIONS the value cannot be mistaken for code that uses it.
func TestNoDecisionPathReturnsTheRecordingFallback(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue // decision paths are the product files; tests only quote the value
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s does not parse: %v", path, err)
		}
		tf := fset.File(f.Pos())
		if tf == nil {
			t.Fatalf("%s has no token file", path)
		}
		var spans [][2]int
		for _, group := range f.Comments {
			for _, c := range group.List {
				spans = append(spans, [2]int{tf.Offset(c.Pos()), tf.Offset(c.End())})
			}
		}
		inComment := func(off int) bool {
			for _, s := range spans {
				if off >= s[0] && off < s[1] {
					return true
				}
			}
			return false
		}
		text := string(src)
		for _, needle := range []string{"23568", "23.568", "23,568"} {
			for at := 0; ; {
				i := strings.Index(text[at:], needle)
				if i < 0 {
					break
				}
				off := at + i
				at = off + len(needle)
				seen++
				if !inComment(off) {
					t.Errorf("%s:%d carries %q OUTSIDE a comment - no row states it, so it "+
						"must never decide a return (the owner: \"i dont want fallback, it "+
						"can confuse or mislead us\")", path, tf.Position(tf.Pos(off)).Line,
						needle)
				}
			}
		}
	}
	if seen == 0 {
		t.Errorf("no comment in the package explains why the recording's number is gone - " +
			"the explanation is part of the change, not a casualty of it")
	}
}
