package script

import (
	"os"
	"path/filepath"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// THE INDEX IMAGE CARRIES ONE PLATFORM'S PATHS TO THE OTHER.
//
// The image is built by whichever build ran the corpus walk and is then copied into a container,
// so a zone on Linux reads a table of paths a Windows walk produced:
//
//	..\out\lua_corpus\skill\attack.lua
//
// On Linux a backslash is an ordinary character in a file name, so every load failed with
// `no such file or directory` while the zone still started and players still entered - monster AI,
// skill damage, monster HP and DEF silently dead (docker-port\deploy\README.md, 2026-11-10).
// These tests feed that exact entry in, and require the resolved file to BE FOUND.

// writeScriptFile writes one translated-looking script and returns its path.
func writeScriptFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// imageFrom writes an image under corpus and reads it back, so a test goes through the REAL writer
// and reader rather than a hand-built struct.
func imageFrom(t *testing.T, corpus string, idx *packedIndex) *packedIndex {
	t.Helper()
	img := IndexImagePath(corpus)
	if err := SaveIndexImage(img, idx); err != nil {
		t.Fatalf("save image: %v", err)
	}
	read, err := OpenIndexImage(img)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	return read
}

// TestAnImageWrittenOnWindowsLoadsItsScriptsHere is pass condition 2: an entry containing `\` is
// fed through the image, and the file it names must be found and armed (not merely "the string
// looks better"). The paths are the SHAPE THE SHIPPED IMAGE CARRIES, measured with a parser over
// out\lua_corpus\.index_image.bin - all 4,239 of its entries look like the one below.
func TestAnImageWrittenOnWindowsLoadsItsScriptsHere(t *testing.T) {
	corpus := filepath.Join(t.TempDir(), "lua_corpus")
	const name = "SCR_SKILL_ATTACK"
	writeScriptFile(t, filepath.Join(corpus, "skill", "attack.lua"),
		"function "+name+"()\n  return 41\nend\n")

	// The image as the WINDOWS walk wrote it: a relative corpus root and native separators.
	legacy := `..\out\lua_corpus\skill\attack.lua`
	read := imageFrom(t, corpus, packIndex(map[string]uint32{name: 0}, []string{legacy}))
	if got := read.paths[0]; got != legacy {
		t.Fatalf("the image did not store the entry as written: %q", got)
	}

	h := New()
	defer h.Close()
	if n := h.IndexFromImage(corpus, read); n != 1 {
		t.Fatalf("IndexFromImage answered %d names, want 1", n)
	}

	got := h.ScriptFile(name)
	if got == "" {
		t.Fatalf("%s has no file after the image was installed", name)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the image's entry %q resolved to %q, which is not on disk: %v", legacy, got, err)
	}
	if want := filepath.Join(corpus, "skill", "attack.lua"); filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("the entry resolved to %q, want %q", got, want)
	}

	loaded, err := h.EnsureScript(name)
	if err != nil {
		t.Fatalf("the script the image names did not load: %v", err)
	}
	if !loaded {
		t.Fatal("EnsureScript reported the file was already in - nothing was loaded")
	}
	if h.L.GetGlobal(name).Type() != lua.LTFunction {
		t.Fatalf("%s is %s after the load, want a function", name, h.L.GetGlobal(name).Type())
	}
}

// TestEveryShapeOfStoredEntryResolvesToTheSameFile pins the three rules of resolveIndexPath. All
// seven entry shapes name the SAME file inside the corpus, so every one of them must answer a path
// that exists - which is the contract: the build machine's prefix never survives, the
// corpus-relative tail always does.
func TestEveryShapeOfStoredEntryResolvesToTheSameFile(t *testing.T) {
	root := filepath.Join("somewhere", "out", "lua_corpus")
	want := filepath.Join(root, "skill", "attack.lua")

	for _, tc := range []struct{ what, stored string }{
		{"windows walk, relative root", `..\out\lua_corpus\skill\attack.lua`},
		{"windows walk, absolute", `D:\ge_server\docker-port\out\lua_corpus\skill\attack.lua`},
		{"windows walk, forward slashes", `../out/lua_corpus/skill/attack.lua`},
		{"linux walk, absolute", `/app/out/lua_corpus/skill/attack.lua`},
		{"relative to the corpus's parent", `lua_corpus/skill/attack.lua`},
		{"already corpus-relative (portable)", `skill/attack.lua`},
		{"corpus-relative with a ./ prefix", `./skill/attack.lua`},
	} {
		if got := resolveIndexPath(root, tc.stored); filepath.Clean(got) != filepath.Clean(want) {
			t.Errorf("%s: %q resolved to %q, want %q", tc.what, tc.stored, got, want)
		}
	}

	// Rule 3: a path that cannot be repaired is NOT quietly re-pointed at something else.
	for _, stored := range []string{`..\elsewhere\x.lua`, `/opt/other/x.lua`, `D:\other\x.lua`} {
		if got := resolveIndexPath(root, stored); filepath.Clean(got) == filepath.Clean(want) {
			t.Errorf("%q was re-pointed into the corpus (%q); an unrepairable path must stay as it is", stored, got)
		}
	}

	// An empty entry stays empty: the caller reports "not defined by any file" rather than a
	// path that is really the corpus directory.
	if got := resolveIndexPath(root, ""); got != "" {
		t.Errorf("an empty entry resolved to %q, want the empty string", got)
	}
}

// TestAPortableEntryIsPreferredOverALegacyOne keeps the second half honest: an image built on
// Linux (or by a future writer storing relative names) must work as well as the legacy one, and
// the entry the image ACTUALLY stores for a name must be the one resolved.
func TestAPortableEntryIsPreferredOverALegacyOne(t *testing.T) {
	corpus := filepath.Join(t.TempDir(), "lua_corpus")
	const name = "SCP_USE_ITEM_AddBuff"
	writeScriptFile(t, filepath.Join(corpus, "item", "legacy.lua"), "function SCP_LEGACY() end\n")
	writeScriptFile(t, filepath.Join(corpus, "item", "item.lua"),
		"function "+name+"()\n  return 1\nend\n")

	// Both forms in one image; the name points at the PORTABLE entry (file id 1).
	read := imageFrom(t, corpus, packIndex(map[string]uint32{name: 1},
		[]string{`..\out\lua_corpus\item\legacy.lua`, "item/item.lua"}))

	h := New()
	defer h.Close()
	h.IndexFromImage(corpus, read)
	got := h.ScriptFile(name)
	if want := filepath.Join(corpus, "item", "item.lua"); filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("the portable entry resolved to %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("the portable entry resolved to %q, which is not on disk: %v", got, err)
	}
	if _, err := h.EnsureScript(name); err != nil {
		t.Fatalf("the script the portable entry names did not load: %v", err)
	}
}

// TestTheShippedImageResolvesEveryPath runs the resolver over the REAL image when it is on this
// machine (it is generated and git-ignored, so the test skips elsewhere). This is the measurement
// the deploy\README row was written from: 4,239 interned paths, every one of them a Windows path.
func TestTheShippedImageResolvesEveryPath(t *testing.T) {
	dir := corpusDir(t)
	img := IndexImagePath(dir)
	if _, err := os.Stat(img); err != nil {
		t.Skipf("no index image at %s - the zone builds one on its first start", img)
	}
	idx, err := OpenIndexImage(img)
	if err != nil {
		t.Fatalf("open %s: %v", img, err)
	}
	h := New()
	defer h.Close()
	h.IndexFromImage(dir, idx)

	var checked, missing int
	for _, p := range idx.paths {
		checked++
		if _, err := os.Stat(p); err != nil {
			missing++
			if missing <= 3 {
				t.Logf("missing: %s", p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the image carried no paths")
	}
	if missing != 0 {
		t.Fatalf("%d of %d interned paths from %s do not resolve", missing, checked, img)
	}
	t.Logf("%d interned paths in %s all resolve", checked, img)
}
