// Package item answers "what IS this item?" from the catalog the Python side emits.
//
// WHY A DATA FILE: a script names items ("Vis", "FireStone") and only the client's
// item tables know the number behind the name, the stack rule and the per-slot cap.
// `ge_item_index` already reads those tables, so `src\emit_item_catalog.py` writes
// the answer once (`data\item_catalog.json`, 24,976 names, 840 KB, 0.1 s) and this
// package loads it. A second `.xml` reader here would be a second implementation of
// work already proved - the same rule as the script corpus and the NPC spawns.
package item

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Entry is what the game's own item table says about one item NAME.
type Entry struct {
	ClassID int64
	// Mergeable is false for equipment and unstackable items: the shipped server
	// never adds to an existing row for them, it mints a new item id.
	Mergeable bool
	// MaxStack is the client's per-slot cap. A give that would pass it is a FULL
	// INVENTORY and fails the transaction; it never opens a second stack.
	MaxStack int64
}

// Catalog is the name -> item table, loaded once at zone start.
type Catalog struct {
	names map[string]Entry
	lower map[string]Entry // name lowercased, for the corpus's inconsistent case
	ids   map[int64]string
	rows  map[int64]Entry // class id -> entry, for a caller that holds the NUMBER
}

// fileDoc is the emitted JSON. One array per name keeps the file small.
type fileDoc struct {
	Count int                `json:"count"`
	Items map[string][]int64 `json:"items"`
}

// Load reads the emitted catalog. A missing file is an ERROR, not an empty
// catalog: an empty one would answer "unknown item" for every name, which looks
// exactly like "this script asks for items that do not exist".
func Load(path string) (*Catalog, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read item catalog: %w", err)
	}
	var doc fileDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse item catalog %s: %w", path, err)
	}
	return FromRows(doc.Items)
}

// FromRows builds a catalog from the decoded rows, so a test can hand it a table
// of three items instead of a file on disk.
func FromRows(rows map[string][]int64) (*Catalog, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("item catalog holds no items")
	}
	c := &Catalog{
		names: make(map[string]Entry, len(rows)),
		lower: make(map[string]Entry, len(rows)),
		ids:   make(map[int64]string, len(rows)),
		rows:  make(map[int64]Entry, len(rows)),
	}
	for name, row := range rows {
		if len(row) < 3 {
			continue
		}
		entry := Entry{ClassID: row[0], Mergeable: row[1] != 0, MaxStack: row[2]}
		c.names[name] = entry
		c.lower[strings.ToLower(name)] = entry
		// First name wins for an id. Several names may share one class id (the
		// same item in two tables), and the reverse lookup is only used for
		// messages, never for a decision.
		if _, taken := c.ids[entry.ClassID]; !taken {
			c.ids[entry.ClassID] = name
			c.rows[entry.ClassID] = entry
		}
	}
	if len(c.names) == 0 {
		return nil, fmt.Errorf("item catalog parsed to nothing")
	}
	return c, nil
}

// Len is how many names the catalog knows.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.names)
}

// Lookup finds an item by the name a script uses.
//
// The exact name is tried first, then a case-insensitive one: the corpus is not
// consistent about case, and the Python side accepts the same near miss, so both
// servers answer the same way.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	if entry, ok := c.names[name]; ok {
		return entry, true
	}
	if entry, ok := c.lower[strings.ToLower(name)]; ok {
		return entry, true
	}
	return Entry{}, false
}

// NameOf is the name a class id belongs to, or "" when the catalog does not know
// it. For messages only: two names can share one id.
func (c *Catalog) NameOf(classID int64) string {
	if c == nil {
		return ""
	}
	return c.ids[classID]
}

// ClassOf is Lookup narrowed to the number, which is what an inventory row holds.
func (c *Catalog) ClassOf(name string) (int64, bool) {
	entry, ok := c.Lookup(name)
	return entry.ClassID, ok
}

// EntryOf is the reverse lookup for a caller that holds the CLASS: the stack rule
// and the per-slot cap of one number, with no name in between. `false` means the
// catalog does not know the class; there is no fallback here, because a cap nobody
// wrote down is a cap nobody measured.
func (c *Catalog) EntryOf(classID int64) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	entry, ok := c.rows[classID]
	return entry, ok
}
