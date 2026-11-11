package script

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// THE INDEX AS A FILE, so a zone does not walk 41,573 corpus files on every start.
//
// The packed index (index.go) is already the exact shape a file wants: one sorted byte blob of
// names, three uint32 columns and the interned paths. This writes that shape straight out and maps
// it back in, so a cold zone start skips the directory walk and the 41,573 file reads.
//
// The owner's rule for shared images (2026-11-09): ONE IMAGE PER TABLE, never one giant file - "if
// we edit item and not buff we dont have to rebuild the whole thing". So this file knows nothing
// about items, buffs or the dictionary; it owns the script index alone.
//
// A FIRST BOOT MAY BE SLOW. If the image is missing or stale, the zone builds the index the old way
// and writes the image; every start after that is a read of one file.
const indexImageVersion = 1

var indexImageMagic = [8]byte{'G', 'E', 'I', 'D', 'X', '0', '0', '1'}

// IndexImagePath is where a corpus's index image lives (exported for the zone's startup log).
func IndexImagePath(corpusDir string) string { return indexImagePath(corpusDir) }

// indexImagePath is where a corpus's index image lives: inside the corpus, one per corpus, so two
// checkouts never share one.
func indexImagePath(corpusDir string) string {
	return filepath.Join(corpusDir, ".index_image.bin")
}

// SaveIndexImage writes the packed index. The file is written beside its target and renamed, so a
// zone that starts while another writes never reads half an image.
func SaveIndexImage(path string, idx *packedIndex) error {
	if idx == nil || len(idx.files) == 0 {
		return fmt.Errorf("script: nothing to write for %s", path)
	}
	blobLen := 0
	if n := len(idx.start); n > 0 {
		blobLen = int(idx.end[n-1])
	}
	size := 8 + 4*5 + len(idx.start)*4 + len(idx.end)*4 + len(idx.files)*4 + blobLen
	for _, p := range idx.paths {
		size += 2 + len(p)
	}
	buf := make([]byte, 0, size)

	buf = append(buf, indexImageMagic[:]...)
	var word [4]byte
	put := func(v uint32) {
		binary.LittleEndian.PutUint32(word[:], v)
		buf = append(buf, word[:]...)
	}
	put(indexImageVersion)
	put(uint32(len(idx.files)))
	put(uint32(len(idx.paths)))
	put(uint32(blobLen))
	put(indexImageChecksum(idx))
	for i := range idx.start {
		put(idx.start[i])
	}
	for i := range idx.end {
		put(idx.end[i])
	}
	for i := range idx.files {
		put(idx.files[i])
	}
	buf = append(buf, idx.blob[:blobLen]...)
	for _, p := range idx.paths {
		if len(p) > 0xFFFF {
			return fmt.Errorf("script: path too long for the image: %s", p)
		}
		var l [2]byte
		binary.LittleEndian.PutUint16(l[:], uint16(len(p)))
		buf = append(buf, l[:]...)
		buf = append(buf, p...)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("script: write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// OpenIndexImage reads an image back. The name blob is NOT copied: it is a slice of the one buffer
// the file was read into, so a zone holds the index once.
func OpenIndexImage(path string) (*packedIndex, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) < 8+4*5 {
		return nil, fmt.Errorf("script: %s is not an index image (too short)", path)
	}
	if string(blob[:8]) != string(indexImageMagic[:]) {
		return nil, fmt.Errorf("script: %s is not an index image (bad magic)", path)
	}
	at := 8
	word := func() uint32 {
		v := binary.LittleEndian.Uint32(blob[at:])
		at += 4
		return v
	}
	if v := word(); v != indexImageVersion {
		return nil, fmt.Errorf("script: %s is version %d, this build reads %d", path, v, indexImageVersion)
	}
	names := int(word())
	paths := int(word())
	blobLen := int(word())
	want := word()

	need := 8 + 4*5 + names*4*3 + blobLen
	if names < 0 || blobLen < 0 || len(blob) < need {
		return nil, fmt.Errorf("script: %s is truncated", path)
	}
	idx := &packedIndex{
		start:  make([]uint32, names),
		end:    make([]uint32, names),
		files:  make([]uint32, names),
		paths:  make([]string, 0, paths),
		sorted: true,
	}
	for i := 0; i < names; i++ {
		idx.start[i] = word()
	}
	for i := 0; i < names; i++ {
		idx.end[i] = word()
	}
	for i := 0; i < names; i++ {
		idx.files[i] = word()
	}
	idx.blob = blob[at : at+blobLen]
	at += blobLen
	for i := 0; i < paths; i++ {
		if at+2 > len(blob) {
			return nil, fmt.Errorf("script: %s is truncated (paths)", path)
		}
		n := int(binary.LittleEndian.Uint16(blob[at:]))
		at += 2
		if at+n > len(blob) {
			return nil, fmt.Errorf("script: %s is truncated (path %d)", path, i)
		}
		idx.paths = append(idx.paths, string(blob[at:at+n]))
		at += n
	}
	if indexImageChecksum(idx) != want {
		return nil, fmt.Errorf("script: %s fails its checksum - rebuild the corpus index", path)
	}
	return idx, nil
}

// resolveIndexPath turns one path as an index IMAGE stores it into a path that opens on THIS
// machine, under THIS corpus root.
//
// WHY IT EXISTS (measured 2026-11-10, docker-port\deploy\README.md "What is BROKEN"): an image
// records the path the WALK produced, so an image built on Windows stores
// `..\out\lua_corpus\skill\attack.lua`. Copied into a Linux checkout that string is not a path at
// all - a backslash is an ordinary character in a file name there - so every script load failed
// with `open ..\out\lua_corpus\skill\attack.lua: no such file or directory` while the zone kept
// starting and players kept entering: monster AI (-monai script), skill damage, monster HP and
// monster DEF were silently dead. The image is one artifact carried between platforms, so the
// SEPARATOR and the build machine's PREFIX are both normalised HERE, on the way in, and the
// writer stays exactly what it was (the Windows zone still writes and reads what it always did).
//
// The three rules, in order:
//
//  1. LEGACY entry that names the corpus folder in its prefix (`..\out\lua_corpus\...`,
//     `/app/out/lua_corpus/...`, `D:/ge_server/docker-port/out/lua_corpus/...`): cut at the LAST
//     component equal to the corpus folder's own name and join the remainder to root. The build
//     machine's prefix is dropped, the corpus-relative tail is kept - which is what makes ONE
//     image work on both platforms.
//  2. PORTABLE entry (already corpus-relative, the form any future writer may store): joined to
//     root as it stands.
//  3. Anything else is handed back slash-normalised and otherwise UNCHANGED - exactly what this
//     code did before the fix, so a path that already worked still works, and a path that cannot
//     be repaired still fails with the file named (never silently pointing at another file).
//
// The one shape this cannot disambiguate: an entry that is ALSO a clean relative path yet happens
// to contain a folder named exactly like the corpus root (`npc/lua_corpus/x.lua`). No writer in
// this tree produces that - `Index` stores root-prefixed paths, not corpus-relative ones - and a
// corpus with a directory named after itself is not a shape the game's tree has.
func resolveIndexPath(root, stored string) string {
	if stored == "" {
		return stored
	}
	p := strings.ReplaceAll(stored, `\`, "/")

	// Rule 1: drop the build machine's prefix, keep the corpus-relative tail.
	if base := filepath.Base(filepath.Clean(root)); base != "." && base != "/" && base != string(filepath.Separator) {
		if i := strings.LastIndex(p, "/"+base+"/"); i >= 0 {
			return filepath.Join(root, filepath.FromSlash(p[i+len(base)+2:]))
		}
		if strings.HasPrefix(p, base+"/") {
			return filepath.Join(root, filepath.FromSlash(p[len(base)+1:]))
		}
	}

	// Rule 2: already relative to the corpus. `..` is NOT portable (it is somebody else's
	// prefix), and neither is a drive letter, so both fall through to rule 3.
	p = strings.TrimPrefix(p, "./")
	if !filepath.IsAbs(filepath.FromSlash(p)) && !strings.HasPrefix(p, "..") && !hasDrivePrefix(p) {
		return filepath.Join(root, filepath.FromSlash(p))
	}

	// Rule 3: unchanged, but with the separator this platform understands.
	return filepath.FromSlash(p)
}

// hasDrivePrefix reports a Windows drive-letter path (`D:/...`), which is absolute to Windows and
// not to Linux - so it must not be joined to root on the wrong platform.
func hasDrivePrefix(p string) bool { return len(p) >= 2 && p[1] == ':' }

// indexImageChecksum is a cheap FNV-1a over the columns and the name blob: an image that was
// written by a half-finished run is refused rather than half-used.
func indexImageChecksum(idx *packedIndex) uint32 {
	const prime = 16777619
	h := uint32(2166136261)
	mix := func(b byte) {
		h ^= uint32(b)
		h *= prime
	}
	for _, v := range idx.files {
		for s := 0; s < 32; s += 8 {
			mix(byte(v >> uint(s)))
		}
	}
	for i := 0; i < len(idx.blob); i++ {
		mix(idx.blob[i])
	}
	return h
}
