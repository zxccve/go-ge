package text

import "sort"

// Table is one id -> string table: the modern dictionary, the server pack, or the id map.
//
// WHY IT IS NOT A MAP (2026-11-09). The three tables together cost 73 MB of LIVE heap in
// every zone (history\world-layer.md, "WHERE A ZONE'S 320 MB GOES"), and every zone loads
// the same bytes. A Go map also pays a string header and a bucket on every entry. So a
// Table is EITHER the old map (the JSON/XML path, unchanged) OR a set of byte slices that
// point into a shared, read-only image file - in which case N zones share ONE physical
// copy and a lookup copies nothing.
type Table struct {
	// The map path: built by Load when no image is in use.
	m map[string]string

	// The image path: all three slices are views into the mapped file (or into the file's
	// bytes when it was read, never copied per entry).
	keys   []byte   // every key, back to back, SORTED
	vals   []byte   // every value, back to back
	keyOff []uint32 // keyOff[i]..keyOff[i+1] bounds key i in keys
	valOff []uint32 // valOff[i]..valOff[i+1] bounds value i in vals
}

// fromMap wraps the old representation.
func fromMap(m map[string]string) *Table { return &Table{m: m} }

// NewTable wraps a plain map, for callers outside this package (tests build a Book this
// way). Production code gets its tables from Load or LoadImage.
func NewTable(m map[string]string) *Table { return fromMap(m) }

// Len answers how many entries the table holds.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	if t.m != nil {
		return len(t.m)
	}
	return len(t.keyOff) - 1
}

// Get answers the value for id.
//
// The image path is a binary search over the sorted key blob and returns a string that
// SHARES the mapped bytes - no allocation, and nothing is copied. The caller must not keep
// it past the process's life, which for this server is "never" (the mapping lives as long
// as the zone does).
func (t *Table) Get(id string) (string, bool) {
	if t == nil {
		return "", false
	}
	if t.m != nil {
		v, ok := t.m[id]
		return v, ok
	}
	lo, hi := 0, len(t.keyOff)-1
	for lo < hi {
		mid := (lo + hi) / 2
		k := t.keys[t.keyOff[mid]:t.keyOff[mid+1]]
		if string(k) < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(t.keyOff)-1 {
		return "", false
	}
	if string(t.keys[t.keyOff[lo]:t.keyOff[lo+1]]) != id {
		return "", false
	}
	return string(t.vals[t.valOff[lo]:t.valOff[lo+1]]), true
}

// Keys lists every key, sorted. Used by tools and tests, not on a packet path.
func (t *Table) Keys() []string {
	if t == nil {
		return nil
	}
	if t.m != nil {
		out := make([]string, 0, len(t.m))
		for k := range t.m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(t.keyOff)-1)
	for i := 0; i+1 < len(t.keyOff); i++ {
		out = append(out, string(t.keys[t.keyOff[i]:t.keyOff[i+1]]))
	}
	return out
}
