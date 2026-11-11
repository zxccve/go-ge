package item

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCatalogLookup covers the three questions a give/take asks of the table, and
// the case-insensitivity the corpus needs: the same item appears as `FireStone`
// and `firestone` in shipped scripts, and the Python side accepts both.
func TestCatalogLookup(t *testing.T) {
	c, err := FromRows(map[string][]int64{
		"Vis":       {30000, 1, 2147483647},
		"FireStone": {21613, 1, 32767},
		"Sword":     {1234, 0, 1},
	})
	if err != nil {
		t.Fatalf("FromRows: %v", err)
	}
	if c.Len() != 3 {
		t.Fatalf("Len = %d, want 3", c.Len())
	}

	if id, ok := c.ClassOf("Vis"); !ok || id != 30000 {
		t.Fatalf("Vis -> %d, %v; want 30000", id, ok)
	}
	if id, ok := c.ClassOf("firestone"); !ok || id != 21613 {
		t.Fatalf("lower-case name did not resolve: %d, %v", id, ok)
	}
	if _, ok := c.ClassOf("NoSuchItem"); ok {
		t.Fatal("an unknown name resolved")
	}

	// The stack rule is what decides merge-or-mint and the full-inventory check.
	entry, _ := c.Lookup("Sword")
	if entry.Mergeable {
		t.Fatal("Sword is marked mergeable; equipment is one per slot")
	}
	if entry.MaxStack != 1 {
		t.Fatalf("Sword MaxStack = %d, want 1", entry.MaxStack)
	}
	stone, _ := c.Lookup("FireStone")
	if !stone.Mergeable || stone.MaxStack != 32767 {
		t.Fatalf("FireStone = %+v, want mergeable with a 32767 cap", stone)
	}

	if name := c.NameOf(21613); name != "FireStone" {
		t.Fatalf("NameOf(21613) = %q", name)
	}
}

// TestCatalogLoadErrors: a missing or empty table must be an ERROR, not an empty
// catalog - an empty one answers "unknown item" for every name, which looks
// exactly like "this script asks for items that do not exist".
func TestCatalogLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file loaded")
	}
	if _, err := FromRows(map[string][]int64{}); err == nil {
		t.Fatal("an empty catalog was accepted")
	}

	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"count":1,"items":{"Vis":[30000,1,99]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if id, ok := c.ClassOf("Vis"); !ok || id != 30000 {
		t.Fatalf("Vis -> %d, %v", id, ok)
	}
	// A nil receiver is answered, not panicked on: a zone started without the file
	// holds a nil catalog and still has to answer every count.
	var none *Catalog
	if _, ok := none.ClassOf("Vis"); ok {
		t.Fatal("a nil catalog resolved a name")
	}
	if none.Len() != 0 || none.NameOf(1) != "" {
		t.Fatal("a nil catalog answered a value")
	}
}
