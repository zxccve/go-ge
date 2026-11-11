package text

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

// The shared text image.
//
// WHY (measured 2026-11-09): the three tables are 73 MB of LIVE heap in every zone, the
// same bytes in every process. As a memory-mapped file the operating system keeps ONE
// physical copy for the whole machine and every zone's private memory loses the 73 MB.
//
// FORMAT (little endian, one small header, then three sorted tables):
//
//	magic   "GETEXT01"           8 bytes
//	tables  u32                  3 (dict, server pack, id map)
//	then per table:
//	  count   u32
//	  keys    count+1 u32 offsets, then the key bytes, back to back, SORTED
//	  vals    count+1 u32 offsets, then the value bytes, back to back
//
// The offsets are absolute within their own blob. Sorting by key is what makes a lookup a
// binary search with no per-entry allocation (text\table.go, Table.Get).
const (
	imageMagic = "GETEXT01"

	// ImageVersion is bumped when the layout above changes; LoadImage refuses a file with a
	// different magic, so an old image is a clean error, never a misread.
	ImageVersion = 1
)

// SaveImage writes the three tables as ONE file every zone can map.
//
// The sources are the same three the JSON/XML loader reads, so the image can never be a
// different generation from the files beside it: the caller passes the paths it would have
// passed to Load.
func SaveImage(path, idMapPath, dictPath, serverPackPath string) (int64, error) {
	book, err := Load(idMapPath, dictPath, serverPackPath)
	if err != nil {
		return 0, err
	}
	return writeImage(path, book)
}

// writeImage is SaveImage from an already-loaded Book, so a test can round-trip a small
// table set without the game's files.
func writeImage(path string, book *Book) (int64, error) {
	buf := make([]byte, 0, 64<<20)
	buf = append(buf, imageMagic...)
	buf = binary.LittleEndian.AppendUint32(buf, 3)
	for _, t := range []*Table{book.Dict, book.ServerPack, book.IDMap} {
		buf = appendTable(buf, t)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return int64(len(buf)), nil
}

// appendTable writes one table in the format above, sorted by key.
func appendTable(buf []byte, t *Table) []byte {
	keys := t.Keys()
	sort.Strings(keys)
	var keysBlob, valsBlob []byte
	keyOff := make([]uint32, 0, len(keys)+1)
	valOff := make([]uint32, 0, len(keys)+1)
	for _, k := range keys {
		v, _ := t.Get(k)
		keyOff = append(keyOff, uint32(len(keysBlob)))
		keysBlob = append(keysBlob, k...)
		valOff = append(valOff, uint32(len(valsBlob)))
		valsBlob = append(valsBlob, v...)
	}
	keyOff = append(keyOff, uint32(len(keysBlob)))
	valOff = append(valOff, uint32(len(valsBlob)))

	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(keys)))
	for _, o := range keyOff {
		buf = binary.LittleEndian.AppendUint32(buf, o)
	}
	buf = append(buf, keysBlob...)
	for _, o := range valOff {
		buf = binary.LittleEndian.AppendUint32(buf, o)
	}
	buf = append(buf, valsBlob...)
	return buf
}

// LoadImage maps the shared image and answers a Book whose tables are views into it.
//
// A missing or unreadable image is an error the caller can ignore: it then calls Load and
// gets today's behaviour, so an image is an optimisation and never a dependency.
func LoadImage(path string) (*Book, error) {
	blob, closer, err := mapFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) < 12 || string(blob[:8]) != imageMagic {
		closer()
		return nil, fmt.Errorf("%s is not a text image (magic mismatch)", path)
	}
	n := binary.LittleEndian.Uint32(blob[8:12])
	if n != 3 {
		closer()
		return nil, fmt.Errorf("%s holds %d table(s), want 3", path, n)
	}
	at := 12
	read := func() (*Table, error) {
		if at+4 > len(blob) {
			return nil, fmt.Errorf("image truncated at a table header")
		}
		count := int(binary.LittleEndian.Uint32(blob[at:]))
		at += 4
		need := (count + 1) * 4
		if at+need > len(blob) {
			return nil, fmt.Errorf("image truncated in key offsets")
		}
		keyOff := make([]uint32, count+1)
		for i := range keyOff {
			keyOff[i] = binary.LittleEndian.Uint32(blob[at:])
			at += 4
		}
		keyLen := int(keyOff[count])
		if at+keyLen > len(blob) {
			return nil, fmt.Errorf("image truncated in key bytes")
		}
		keys := blob[at : at+keyLen]
		at += keyLen

		if at+need > len(blob) {
			return nil, fmt.Errorf("image truncated in value offsets")
		}
		valOff := make([]uint32, count+1)
		for i := range valOff {
			valOff[i] = binary.LittleEndian.Uint32(blob[at:])
			at += 4
		}
		valLen := int(valOff[count])
		if at+valLen > len(blob) {
			return nil, fmt.Errorf("image truncated in value bytes")
		}
		vals := blob[at : at+valLen]
		at += valLen
		return &Table{keys: keys, vals: vals, keyOff: keyOff, valOff: valOff}, nil
	}

	dict, err := read()
	if err != nil {
		closer()
		return nil, err
	}
	pack, err := read()
	if err != nil {
		closer()
		return nil, err
	}
	idMap, err := read()
	if err != nil {
		closer()
		return nil, err
	}
	if at != len(blob) {
		closer()
		return nil, fmt.Errorf("image holds %d trailing byte(s)", len(blob)-at)
	}
	return &Book{Dict: dict, ServerPack: pack, IDMap: idMap, unmapped: closer}, nil
}

// ImageStale answers whether the image needs rebuilding: missing, unreadable, or older
// than any of the three sources it is built from. The reason is for the log line.
//
// This is the automation the owner asked for ("how do we update the dictionary then? like
// how the scp/xml update works"): the zone rebuilds the image itself when a dictionary or
// the id map is newer, exactly as it already refreshes the Lua corpus at startup.
func ImageStale(path, idMapPath, dictPath, serverPackPath string) (bool, string) {
	fi, err := os.Stat(path)
	if err != nil {
		return true, "it does not exist yet"
	}
	for _, src := range []struct{ name, path string }{
		{"the id map", idMapPath},
		{"the dictionary", dictPath},
		{"the server pack dictionary", serverPackPath},
	} {
		si, serr := os.Stat(src.path)
		if serr != nil {
			continue // a missing source is the loader's problem, not a rebuild trigger
		}
		if si.ModTime().After(fi.ModTime()) {
			return true, src.name + " is newer than the image"
		}
	}
	return false, ""
}
