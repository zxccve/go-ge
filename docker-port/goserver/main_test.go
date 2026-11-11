package main

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"geserver/zone"
)

// captureLog runs fn with the standard logger redirected into a buffer, so a
// test can quote the EXACT line the starter prints. The zone is not started by
// these tests: they exercise the two loader functions main.go calls at startup.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})
	fn()
	return buf.String()
}

// TestLoadItemStatsLogsTheStartupLine pins the two lines the zone prints for
// data\item_stats.json: the one that says how many item classes were loaded, and
// the one that says the file is missing and the DERIVED half of the sheet will
// be sent as 0. The second is NOT fatal, so it must be a log line and not an
// exit - a zone that refuses to start over a missing data file is worse than a
// sheet with zeros.
func TestLoadItemStatsLogsTheStartupLine(t *testing.T) {
	// The file the zone loads by default, relative to goserver\.
	got := captureLog(t, func() {
		if table := loadItemStats("../data/item_stats.json"); table == nil {
			t.Fatalf("data\\item_stats.json is missing - run " +
				"`python src\\emit_item_stats.py`")
		}
	})
	if !strings.Contains(got, "item class(es) of stat columns loaded") {
		t.Fatalf("startup line = %q", got)
	}
	t.Logf("loaded: %s", strings.TrimSpace(got))

	got = captureLog(t, func() {
		if table := loadItemStats("../data/no_such_item_stats.json"); table != nil {
			t.Fatal("a missing file answered a table")
		}
	})
	if !strings.Contains(got, "sent as 0") {
		t.Fatalf("missing-file line = %q", got)
	}
	t.Logf("missing: %s", strings.TrimSpace(got))
}

// TestSpawnSplashPairOffTouchesNothing pins the default: with the flag off the
// helper returns BEFORE it looks at the handler, so a normal run creates nothing
// and logs nothing. The handler is nil on purpose - a call that reached
// SpawnMonsterObject would panic on it, which is exactly the failure this guards.
// The ON path needs a live mesh and the game's Monster table, so it is not tested
// here; a fake table would prove nothing about the wire.
func TestSpawnSplashPairOffTouchesNothing(t *testing.T) {
	var lines []string
	got := captureLog(t, func() {
		spawnSplashPair(nil, false, func(format string, args ...any) {
			lines = append(lines, format)
		})
	})
	if len(lines) != 0 {
		t.Fatalf("flag OFF logged %d line(s): %q", len(lines), lines)
	}
	if got != "" {
		t.Fatalf("flag OFF wrote to the standard log: %q", got)
	}
}

// TestLogScriptBalanceNamesTheProbeAndCaps is the boot-log contract for a
// TRUNCATED script: prepare.py printed these lines after translating exactly the
// files this run touched, and each one must reach the log with the file's path
// and counts - capped at 5 plus a "... and N more", so a corpus-wide rebuild
// cannot flood the start.
//
// The input is copied verbatim from a real `python prepare.py` run (2026-11-03)
// with a deliberately truncated probe in place: the source had 3 `{` and 2 `}`,
// and the translator synthesised the one `end` that was missing.
func TestLogScriptBalanceNamesTheProbeAndCaps(t *testing.T) {
	out := "TRANSLATED 1 script(s)\n" +
		"UNBALANCED aa_probe\\truncated_probe.scp (openers 3, ends 2, " +
		"1 closer(s) added automatically)\n" +
		"BALANCE 1 of 1 translated script(s) unbalanced\n"

	got := captureLog(t, func() { logScriptBalance(out) })
	if !strings.Contains(got, "zone: WARNING: script aa_probe\\truncated_probe.scp looks unbalanced "+
		"(openers 3, ends 2, 1 closer(s) added automatically)") {
		t.Fatalf("warning line missing or wrong shape: %q", got)
	}
	if !strings.Contains(got, "zone: script corpus balance-check: 1 of 1 translated script(s) unbalanced") {
		t.Fatalf("total line missing: %q", got)
	}

	// The older shape (no closer count) must still be read: a stale prepare.py
	// must not be able to silence the warning.
	legacy := captureLog(t, func() {
		logScriptBalance("UNBALANCED aa_probe\\truncated_probe.scp (openers 12, ends 11) - cut off\n")
	})
	if !strings.Contains(legacy, "looks unbalanced (openers 12, ends 11, 0 closer(s)") {
		t.Fatalf("legacy shape not parsed: %q", legacy)
	}

	// Seven unbalanced files: five named, the rest counted.
	var many strings.Builder
	for i := 0; i < 7; i++ {
		fmt.Fprintf(&many, "UNBALANCED f%d.scp (openers 2, ends 1, 1 closer(s) added automatically)\n", i)
	}
	many.WriteString("BALANCE 7 of 7 translated script(s) unbalanced\n")
	capped := captureLog(t, func() { logScriptBalance(many.String()) })
	if n := strings.Count(capped, " looks unbalanced (openers "); n != scriptBalanceShown {
		t.Fatalf("named %d file(s), want %d: %q", n, scriptBalanceShown, capped)
	}
	if !strings.Contains(capped, "... and 2 more file(s) look unbalanced") {
		t.Fatalf("cap line missing: %q", capped)
	}

	// A boot that translated nothing says nothing about balance.
	if quiet := captureLog(t, func() { logScriptBalance("nothing to do\n") }); quiet != "" {
		t.Fatalf("clean boot logged balance lines: %q", quiet)
	}
}

// THE -dummycell CONTRACT, in three parts: an empty flag is EXACTLY the four cells
// measured on map 1331, a list is parsed and cycled in order, and a list that does
// not parse refuses the whole probe without creating anything.
//
// The four numbers are written out again here on purpose. A test that only compared
// the empty flag against `dummyCells` would pass after somebody edited `dummyCells`,
// and map 1331's probe - the one run that works today - would move without a word.
// These are the cells `pathtool near fld_auch.tok` answered (cell 55, heights
// 9/9/9/10), and they are what the owner's existing runs put on screen.
func TestParseDummyCellsEmptyKeepsTheMeasuredStrip(t *testing.T) {
	want := [][2]float64{{3160, -1800}, {3240, -1800}, {3320, -1800}, {3400, -1800}}

	for _, blank := range []string{"", " ", "\t", "\r\n"} {
		got, err := parseDummyCells(blank)
		if err != nil {
			t.Fatalf("empty flag %q was refused: %v", blank, err)
		}
		if len(got) != len(want) {
			t.Fatalf("empty flag %q gave %d cell(s), want %d: %v", blank, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("empty flag %q cell %d = %v, want %v (the measured strip)", blank, i, got[i], want[i])
			}
		}
		if len(dummyCells) != len(want) {
			t.Fatalf("dummyCells now holds %d cell(s), want the %d measured ones", len(dummyCells), len(want))
		}
		for i := range want {
			if dummyCells[i] != want[i] {
				t.Fatalf("dummyCells[%d] = %v, want the measured %v", i, dummyCells[i], want[i])
			}
		}
	}
}

// A valid list is parsed as x,y pairs regardless of the spaces around them, and the
// cycle is the existing `i % len` rule: four classes over two cells take 0,1,0,1.
// The -dummycell list is the ONLY thing that changes - map 1301's probe is
// "-8856,4111", a few hundred units from that map's own spawn point.
func TestParseDummyCellsListAndCycle(t *testing.T) {
	got, err := parseDummyCells(" -8856,4111 , -8756,4200 ")
	if err != nil {
		t.Fatalf("a valid list was refused: %v", err)
	}
	want := [][2]float64{{-8856, 4111}, {-8756, 4200}}
	if len(got) != len(want) {
		t.Fatalf("parsed %d cell(s), want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cell %d = %v, want %v", i, got[i], want[i])
		}
	}

	for i, want := range []int{0, 1, 0, 1, 0} {
		spot, ok := dummyCellFor(got, i)
		if !ok {
			t.Fatalf("class %d got no cell", i)
		}
		if spot != got[want] {
			t.Fatalf("class %d took %v, want %v (cycle 0,1,0,1)", i, spot, got[want])
		}
	}

	// One pair is a legal list: every class lands on it.
	one, err := parseDummyCells("-8856,4111")
	if err != nil || len(one) != 1 {
		t.Fatalf("one pair parsed to %v, err %v - want one cell", one, err)
	}
	if spot, ok := dummyCellFor(one, 3); !ok || spot != ([2]float64{-8856, 4111}) {
		t.Fatalf("class 3 on a one-cell list took %v (ok %v)", spot, ok)
	}

	// No cells is a refusal, never an index out of range: a probe must not panic.
	if _, ok := dummyCellFor(nil, 0); ok {
		t.Fatal("an empty spot list answered a cell")
	}
}

// A pair that does not parse is refused WHOLE, and the error carries the offending
// text - the operator fixes the flag from the log line alone. Every case here would
// otherwise become a guess: an odd count means a halved pair, a word is a typo, and
// NaN/Inf are the two numbers ParseFloat accepts that the mesh cannot take.
func TestParseDummyCellsRefusesAMalformedList(t *testing.T) {
	cases := []struct{ in, names string }{
		{"-8856", "-8856"},             // one number, no y
		{"1,2,3", "1,2,3"},             // three numbers: two pairs cannot be made
		{"abc,4111", "abc"},            // x is a word
		{"-8856,", ""},                 // y is missing
		{",", ""},                      // nothing but a comma
		{"-8856,4111,NaN,4111", "NaN"}, // a number, but not a coordinate
		{"-8856,4111,Inf,4111", "Inf"}, // the other one
	}
	for _, c := range cases {
		got, err := parseDummyCells(c.in)
		if err == nil {
			t.Fatalf("%q was ACCEPTED: %v", c.in, got)
		}
		if got != nil {
			t.Fatalf("%q was refused but still answered %v", c.in, got)
		}
		if !strings.Contains(err.Error(), c.names) {
			t.Fatalf("%q: error %q does not name %q", c.in, err.Error(), c.names)
		}
	}
}

// THE FAILURE THIS FLAG EXISTS FOR IS A REFUSAL, NOT A GUESS: with a -dummycell
// that does not parse, the probe logs ONE line and creates nothing at all - not
// "create what parses", which would put the second class on map 1331's strip again.
//
// The handler is nil on purpose, exactly as TestSpawnSplashPairOffTouchesNothing
// does it: any call that reached SpawnMonsterObject would panic on it, so a green
// run here is a run that spawned NOTHING, and the one-line count is checked to the
// character - two lines would mean the refusal was logged and the spawn still tried.
func TestSpawnDummiesMalformedCellsCreatesNothing(t *testing.T) {
	var lines []string
	spawnDummies(nil, "11591", "-8856", func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	if len(lines) != 1 {
		t.Fatalf("malformed -dummycell logged %d line(s), want exactly 1: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "-dummycell") || !strings.Contains(lines[0], "REFUSED") {
		t.Fatalf("refusal line = %q", lines[0])
	}
	if !strings.Contains(lines[0], `"-8856"`) {
		t.Fatalf("refusal line does not name the bad text: %q", lines[0])
	}
	if strings.Contains(lines[0], "is ON: class") {
		t.Fatalf("a spawn line was printed for a refused probe: %q", lines[0])
	}
}

// The default is untouched: no -spawndummy means no line and no handler access, and
// a -dummycell with nothing to place is said out loud ONCE instead of being silently
// dropped (that silent drop is how a probe run ends with no dummy and no clue).
func TestSpawnDummiesOffTouchesNothing(t *testing.T) {
	for _, cells := range []string{"", "-8856,4111"} {
		var lines []string
		spawnDummies(nil, "", cells, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
		if cells == "" {
			if len(lines) != 0 {
				t.Fatalf("flag OFF logged %d line(s): %q", len(lines), lines)
			}
			continue
		}
		if len(lines) != 1 {
			t.Fatalf("-dummycell without -spawndummy logged %d line(s), want 1: %q", len(lines), lines)
		}
		if !strings.Contains(lines[0], "-dummycell") || !strings.Contains(lines[0], "NOTHING is created") {
			t.Fatalf("ignored-flag line = %q", lines[0])
		}
	}
}

// THE HEIGHT HINT. A -dummycell pair carries x,y and no z, and meshCell searches
// only 300 units above and below the hint it is given. Map 1301's ground under
// (-8856,4111) is at 1385 - measured, and the reason the first live :7007 probe was
// REFUSED with z 0 at 12:27 on 2026-11-07 - so the map has to answer the height.
//
// This pins both halves of that: the query keeps the original's 1000-unit
// HORIZONTAL limit (the one that stops a probe jumping across a wall) and widens
// only the VERTICAL one, and the answer is the mesh's own point and height.
func TestDummyGroundAsksTheMeshForTheHeight(t *testing.T) {
	var gotPt [3]int32
	var gotH, gotV int
	h := &zone.Handler{NavNear: func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		gotPt, gotH, gotV = pt, hRange, vRange
		return [3]int32{-8856, 4111, 140}, 1385, true
	}}
	gx, gy, gz, ok := dummyGround(h, -8856, 4111)
	if !ok {
		t.Fatal("the mesh answered ground and dummyGround refused it")
	}
	if gx != -8856 || gy != 4111 || gz != 1385 {
		t.Fatalf("resolved (%g,%g,%g), want the mesh's (-8856,4111,1385)", gx, gy, gz)
	}
	if gotPt != [3]int32{-8856, 4111, 0} {
		t.Fatalf("asked the mesh for %v, want the flag's x,y with the hint z 0", gotPt)
	}
	if gotH != 1000 {
		t.Fatalf("horizontal range %d, want the original's 1000", gotH)
	}
	if gotV != dummyHintVRange {
		t.Fatalf("vertical range %d, want the wide hint range %d", gotV, dummyHintVRange)
	}

	// No ground under the pair: refused, and the pair comes back UNCHANGED, so the
	// refusal the operator reads is SpawnMonsterObject's own line about the point.
	miss := &zone.Handler{NavNear: func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{}, 0, false
	}}
	x2, y2, z2, ok2 := dummyGround(miss, 5, 6)
	if ok2 || x2 != 5 || y2 != 6 || z2 != 0 {
		t.Fatalf("a miss answered (%g,%g,%g) ok=%v, want the pair back with z 0", x2, y2, z2, ok2)
	}

	// A zone with no path engine wired, and a nil handler, must not panic.
	if _, _, _, ok3 := dummyGround(nil, 1, 2); ok3 {
		t.Fatal("a nil handler answered a ground height")
	}
	if _, _, _, ok4 := dummyGround(&zone.Handler{}, 1, 2); ok4 {
		t.Fatal("a zone with no NavNear answered a ground height")
	}
}
