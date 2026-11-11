package script

import (
	"sort"
	"strings"
)

// packedIndex is the corpus name -> file map in a COMPACT form.
//
// WHY IT EXISTS (measured 2026-11-09, history\world-layer.md "WHERE A ZONE'S 320 MB GOES"):
// the plain map[string]string of 41,573 names cost **58 MB of private bytes in every
// zone** - a Go map header, a 16-byte string header and a copied string per entry, plus
// the key bytes. Every zone loads the same names, so that 58 MB was paid once per map.
//
// Layout: all names in ONE byte blob, sorted, with a uint32 offset per entry and a uint32
// file id; the file paths are interned once (a few thousand distinct paths for 41,573
// names). A lookup is a binary search over the blob. Cost for the same corpus: ~2 MB.
type packedIndex struct {
	blob   []byte   // every name, back to back
	start  []uint32 // start[i] and end[i] bound name i in blob (end[i] == start[i+1])
	end    []uint32
	files  []uint32 // file id per name
	paths  []string // interned file paths, indexed by file id
	sorted bool
}

// Len answers how many names the index knows.
func (p *packedIndex) Len() int {
	if p == nil {
		return 0
	}
	return len(p.files)
}

// Lookup answers the corpus file that defines name.
//
// The LAST definition wins, which is what one runtime does anyway and what the old map
// build did: files are walked in sorted order, so a later file overwrites an earlier one.
func (p *packedIndex) Lookup(name string) (string, bool) {
	if p == nil || len(p.files) == 0 || name == "" {
		return "", false
	}
	i := sort.Search(len(p.files), func(i int) bool {
		return strings.Compare(string(p.blob[p.start[i]:p.end[i]]), name) >= 0
	})
	if i >= len(p.files) {
		return "", false
	}
	if string(p.blob[p.start[i]:p.end[i]]) != name {
		return "", false
	}
	return p.paths[p.files[i]], true
}

// Names lists every name the index knows, sorted (the blob is already sorted).
func (p *packedIndex) Names() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.files))
	for i := range p.files {
		out[i] = string(p.blob[p.start[i]:p.end[i]])
	}
	return out
}

// packIndex builds the compact form from a name -> file-id map. It is used once at
// startup; the map is freed afterwards, so the peak is the build's, not the zone's.
func packIndex(byName map[string]uint32, paths []string) *packedIndex {
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	p := &packedIndex{paths: paths, sorted: true}
	p.start = make([]uint32, 0, len(names))
	p.end = make([]uint32, 0, len(names))
	p.files = make([]uint32, 0, len(names))
	var at uint32
	for _, n := range names {
		p.start = append(p.start, at)
		p.blob = append(p.blob, n...)
		at += uint32(len(n))
		p.end = append(p.end, at)
		p.files = append(p.files, byName[n])
	}
	return p
}
