package zone

import (
	"log"
	"os"
	"testing"

	"geserver/script"
)

// defaultStatusForTests is the package's ONE status-script source for the test
// suite. Production has no such default: a Handler with no Status shows zeros and
// says so (zone\zone.go sheetFor), because a fallback that guesses is how a wrong
// number becomes believable.
//
// It exists because the Character Info numbers are now the GAME's formula
// (zone\status_script.go), so a test that wants a sheet - an attack cadence, a
// cast's damage base, a panel value - has to have the corpus and the emitted item
// properties. Building that once per process keeps the suite fast: the host is
// indexed and the libraries loaded here, and every Sheet() call after that is a
// cached script run.
//
// A MISSING CORPUS LEAVES IT NIL, and the tests that need a sheet SKIP with the
// command that builds it - they must never pass by comparing zeros.
// testCorpus is the generated Lua corpus, relative to this package's directory
// (goserver\zone -> docker-port\out\lua_corpus). It is built by src\scp_to_lua.py
// and not committed, which is why every test that needs it can skip.
const testCorpus = "../../out/lua_corpus"

// testScriptHost is the runtime TestMain built for the Character Info source. The
// damage-script tests (zone\skill_script_test.go) run on the SAME host: indexing
// the corpus is a walk over 4,239 files and a test must not pay for it twice.
// A test that needs a DIFFERENT runtime (a stub that raises) builds its own on a
// throwaway corpus instead of poisoning this one.
var testScriptHost *script.Host

func TestMain(m *testing.M) {
	if _, err := os.Stat(testCorpus); err != nil {
		log.Printf("zone tests: no script corpus at %s - tests that need the "+
			"Character Info sheet will skip (build it with: cd src && "+
			"python scp_to_lua.py --dump ../out/lua_corpus)", testCorpus)
		os.Exit(m.Run())
	}
	props, err := LoadItemProps("../../data/item_props.json")
	if err != nil {
		log.Printf("zone tests: no item properties (%v) - tests that need the sheet "+
			"will skip (rebuild with: cd src && python emit_item_props.py)", err)
		os.Exit(m.Run())
	}
	host := script.New()
	if _, err := host.Index(testCorpus); err != nil {
		log.Printf("zone tests: indexing the corpus failed: %v", err)
		os.Exit(m.Run())
	}
	if err := host.LoadLibraries(); err != nil {
		log.Printf("zone tests: loading the shared script libraries failed: %v", err)
		os.Exit(m.Run())
	}
	testScriptHost = host
	defaultStatusForTests = NewStatusScript(host, props, testStancesForMain(), 10, log.Printf)

	code := m.Run()
	host.Close()
	os.Exit(code)
}

// sheetOf answers one character's sheet for a test, and fails if the script could
// not answer: a zero sheet would make a comparison pass while proving nothing.
func sheetOf(t *testing.T, c Character) StatusSheet {
	t.Helper()
	requireStatus(t)
	sheet, ok := defaultStatusForTests.Sheet(c)
	if !ok {
		t.Fatalf("the status script did not answer for character %d %q", c.Handle, c.Name)
	}
	return sheet
}

// refreshForTest puts the game's own maxima (SCR_Get_MHP / SCR_Get_MSP) on a
// character, exactly as Handler.refreshSheet does at entry, so a test can build a
// record the way the zone does.
func refreshForTest(t *testing.T, c *Character) {
	t.Helper()
	requireStatus(t)
	sheet := sheetOf(t, *c)
	if sheet.MHP > 0 {
		c.MaxHP = sheet.MHP
	}
	if sheet.MSP > 0 {
		c.MaxSP = clampWord(sheet.MSP)
	}
	if c.HP > c.MaxHP {
		c.HP = c.MaxHP
	}
}

// requireStatus skips a test that needs the character sheet when the suite has no
// script source (no corpus, no item properties).
func requireStatus(t *testing.T) {
	t.Helper()
	if defaultStatusForTests == nil {
		t.Skip("no status script source in this checkout: build the corpus with " +
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus, and the item " +
			"properties with python emit_item_props.py")
	}
}

// testStancesForMain loads the stance table for the package-wide source. It cannot
// take a *testing.T (TestMain has none), so a failure leaves the stances nil: the
// sheet then answers with an empty stance row, which the tests that assert stance
// behaviour would catch.
func testStancesForMain() *StanceTable {
	st, err := LoadStances(XMLDir())
	if err != nil {
		log.Printf("zone tests: stance table not loaded for the sheet source: %v", err)
		return nil
	}
	return st
}
