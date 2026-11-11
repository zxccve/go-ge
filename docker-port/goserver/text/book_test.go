package text

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newBook is the live shape in miniature: the shipped script says 14047, the
// dictionary knows 10146189, and only the id map links the two.
func newBook() *Book {
	return &Book{
		IDMap: fromMap(map[string]string{"14047": "10146189", "7": "555"}),
		Dict: fromMap(map[string]string{
			"10146189": "Pay your taxes.",
			"555":      "Balance: %d gold",
			"556":      "Rate %0.1f percent",
			"557":      "Score: %d", // placeholder, and a token: unresolvable
		}),
	}
}

func TestResolve(t *testing.T) {
	book := newBook()
	cases := map[string]string{
		"<$>14047</>":     "Pay your taxes.",
		"plain text":      "plain text",
		"a <$>14047</> b": "a Pay your taxes. b",
		"<$>557</>":       "<$>557</>", // the dictionary has it, but it needs an argument
		"<$>999999</>":    "<$>999999</>",
	}
	for in, want := range cases {
		if got := book.Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
	if book.Missing != 2 {
		t.Errorf("Missing = %d, want 2 (the placeholder text and the unknown id)", book.Missing)
	}

	// A nil book must not panic: the zone runs without one when the files are absent.
	var none *Book
	if got := none.Resolve("<$>1</>"); got != "<$>1</>" {
		t.Errorf("nil book = %q, want the token back", got)
	}
}

// TestFormat is IMCFormat's contract: resolve FIRST, then fill. Formatting the
// token itself leaves every argument unplaced and the player reads a literal %d.
func TestFormat(t *testing.T) {
	book := newBook()
	cases := []struct {
		name string
		tok  string
		args []any
		want string
	}{
		{"integer", "<$>7</>", []any{float64(50000)}, "Balance: 50000 gold"},
		{"integer truncates a fraction", "<$>7</>", []any{float64(1.25)}, "Balance: 1 gold"},
		{"resolved rate", "<$>556</>", []any{float64(12.34)}, "Rate 12.3 percent"},
		{"no args", "<$>14047</>", nil, "Pay your taxes."},
		{"C length prefix", "%I64d gold", []any{float64(7)}, "7 gold"},
		{"escaped percent", "100%% sure", nil, "100%% sure"},
		{"missing argument", "%d and %d", []any{float64(1)}, "1 and %d"},
	}
	for _, c := range cases {
		if got := book.Format(c.tok, c.args); got != c.want {
			t.Errorf("%s: Format(%q, %v) = %q, want %q", c.name, c.tok, c.args, got, c.want)
		}
	}
}

func TestFormatStringArgument(t *testing.T) {
	book := newBook()
	if got, want := book.Format("<$>7</>", []any{"nothing"}), "Balance: %d gold"; got != want {
		// %d with a string: not a number, so the C-style substitution cannot
		// happen. The text is left with its placeholder rather than printing a
		// Go type error into the player's dialog box.
		t.Errorf("Format with a string for %%d = %q, want %q", got, want)
	}
}

// dictionaryFixture writes one dictionary in the client's own shape.
func dictionaryFixture(t *testing.T, dir, name string, entries map[string]string) string {
	t.Helper()
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<table>\n")
	for _, id := range ids {
		fmt.Fprintf(&b, "  <Text ClassID=\"%s\" Text=\"%s\"/>\n", id, entries[id])
	}
	b.WriteString("</table>\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestServerPackFillsALegacyGapAndLosesEveryOverlap pins the two rules together:
// a legacy id the modern dictionary lacks comes from the server pack, and an id
// BOTH carry keeps the modern text. The ids and texts are the shipped files' own
// (measured 2026-11-08), so a failure names the exact rows it is about.
func TestServerPackFillsALegacyGapAndLosesEveryOverlap(t *testing.T) {
	dir := t.TempDir()
	modern := dictionaryFixture(t, dir, "dictionary_local.xml", map[string]string{
		"10146189": "Pay your taxes.",
		"107009":   "Psychokinesis",
	})
	pack := dictionaryFixture(t, dir, "dictionary_local_serverpack.xml", map[string]string{
		"1055":   "I want to leave..",
		"7656":   "Purchase Item",
		"107009": "{#D89EFC}Hey there, commander.{/}",
	})
	idMap := filepath.Join(dir, "locale_id_map.json")
	if err := os.WriteFile(idMap, []byte(`{"14047": "10146189"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	book, err := Load(idMap, modern, pack)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if book.Dict.Len() != 2 || book.ServerPack.Len() != 3 {
		t.Fatalf("loaded %d modern + %d pack rows, want 2 + 3",
			book.Dict.Len(), book.ServerPack.Len())
	}

	cases := []struct{ name, in, want string }{
		{"legacy id from the pack", "<$>7656</>", "Purchase Item"},
		{"legacy id 1055 from the pack", "<$>1055</>", "I want to leave.."},
		{"modern id beats the pack", "<$>107009</>", "Psychokinesis"},
		{"the id map still runs first", "<$>14047</>", "Pay your taxes."},
		{"in neither table: the token stays", "<$>424242</>", "<$>424242</>"},
	}
	for _, c := range cases {
		if got := book.Resolve(c.in); got != c.want {
			t.Errorf("%s: Resolve(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	if book.Missing != 1 {
		t.Errorf("Missing = %d, want 1 (only 424242)", book.Missing)
	}
	// IMCFormat resolves through the same lookup, so a legacy id must format too.
	if got := book.Format("<$>7656</>", nil); got != "Purchase Item" {
		t.Errorf(`Format(<$>7656</>) = %q, want "Purchase Item"`, got)
	}

	// The pack is a required read: a bad path must be an error, never a book
	// that quietly resolves 9.8% fewer tokens.
	if _, err := Load(idMap, modern, filepath.Join(dir, "absent.xml")); err == nil {
		t.Error("Load with an absent server pack returned no error")
	}
}

// TestLiveDictionariesCoverTheLegacyBand is the pass condition on the SHIPPED
// files: a legacy token resolves to English, the modern dictionary keeps its own
// text for an id both files carry, an id in neither stays a token, and neither
// file has shrunk under the loader. The client data is not in git, so this skips
// on a machine without it.
func TestLiveDictionariesCoverTheLegacyBand(t *testing.T) {
	dictDir := filepath.Join("..", "..", "..", "ge", "dictionary")
	dictPath := filepath.Join(dictDir, "dictionary_local.xml")
	packPath := filepath.Join(dictDir, "dictionary_local_serverpack.xml")
	idMapPath := filepath.Join("..", "..", "data", "locale_id_map.json")
	for _, path := range []string{dictPath, packPath, idMapPath} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("no client data at %s", path)
		}
	}

	// An EMPTY id map on purpose: the shipped map rewrites 107009 to 10154242,
	// so the dictionary-vs-pack question - the one this job settles - can only
	// be asked with that rewrite out of the way. The shipped map is exercised
	// separately at the end.
	emptyMap := filepath.Join(t.TempDir(), "empty_id_map.json")
	if err := os.WriteFile(emptyMap, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	book, err := Load(emptyMap, dictPath, packPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The sizes this pipeline was measured against. A shrunken file (or a
	// parser that stopped reading one) must be visible here, not as missing
	// dialog boxes in the game.
	t.Logf("dictionary_local.xml: %d rows; dictionary_local_serverpack.xml: %d rows",
		book.Dict.Len(), book.ServerPack.Len())
	if book.Dict.Len() < 210000 {
		t.Errorf("modern dictionary has %d rows, want >= 210000 - did the file shrink?", book.Dict.Len())
	}
	if book.ServerPack.Len() < 188000 {
		t.Errorf("server pack has %d rows, want >= 188000 - did the file shrink?", book.ServerPack.Len())
	}

	for _, c := range []struct{ in, want string }{
		{"<$>7656</>", "Purchase Item"},
		{"<$>1055</>", "I want to leave.."},
		{"<$>107009</>", "Psychokinesis"}, // the pack's NPC line must lose
		{"<$>42424242</>", "<$>42424242</>"},
	} {
		if got := book.Resolve(c.in); got != c.want {
			t.Errorf("Resolve(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The two files really do disagree on 107009 - otherwise the case above
	// would pass even if the pack overwrote the modern table.
	if pack, _ := book.ServerPack.Get("107009"); pack == "" || pack == "Psychokinesis" {
		t.Errorf("server pack 107009 = %q, want the NPC line the modern text must beat", pack)
	}

	// And the id-map step still runs first, with the map that ships in git.
	book, err = Load(idMapPath, dictPath, packPath)
	if err != nil {
		t.Fatalf("Load with the shipped id map: %v", err)
	}
	if got, want := book.Resolve("<$>14047</>"), "You traitors! {br}I will kill you!"; got != want {
		t.Errorf("Resolve(<$>14047</>) = %q, want %q (the id map: 14047 -> 10146189)", got, want)
	}
}
