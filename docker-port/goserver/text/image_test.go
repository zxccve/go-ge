package text

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTheImageRoundTripsEveryTable: the shared image is only worth anything if a mapped
// book answers exactly what the map-built one does - every key, the missing ones, and the
// table sizes.
func TestTheImageRoundTripsEveryTable(t *testing.T) {
	src := &Book{
		Dict: fromMap(map[string]string{
			"10114047": "Hello, I'm Leonardo Expreso.",
			"184538":   "Crack",
			"":         "an empty key is not a key",
		}),
		ServerPack: fromMap(map[string]string{"107009": "Psychokinesis"}),
		IDMap:      fromMap(map[string]string{"184538": "10008848", "14047": "10114047"}),
	}
	path := filepath.Join(t.TempDir(), "text_image.bin")
	if _, err := writeImage(path, src); err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	got, err := LoadImage(path)
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	defer got.Close()

	if got.Dict.Len() != src.Dict.Len() {
		t.Fatalf("dict has %d entries, want %d", got.Dict.Len(), src.Dict.Len())
	}
	for _, id := range []string{"10114047", "184538"} {
		want, _ := src.Dict.Get(id)
		have, ok := got.Dict.Get(id)
		if !ok || have != want {
			t.Fatalf("Dict[%s] = %q, %v; want %q", id, have, ok, want)
		}
	}
	if v, ok := got.Dict.Get("no-such-id"); ok || v != "" {
		t.Fatalf("a missing id answered %q, %v", v, ok)
	}
	if v, ok := got.ServerPack.Get("107009"); !ok || v != "Psychokinesis" {
		t.Fatalf("ServerPack[107009] = %q, %v", v, ok)
	}
	if v, ok := got.IDMap.Get("14047"); !ok || v != "10114047" {
		t.Fatalf("IDMap[14047] = %q, %v", v, ok)
	}
	// The resolved path a click takes: token -> id map -> dictionary.
	if text, ok := got.lookup("<$>14047</>"); !ok || text != "Hello, I'm Leonardo Expreso." {
		t.Fatalf("lookup(<$>14047</>) = %q, %v", text, ok)
	}
}

// TestAMissingImageIsAnErrorNotAPanic: the zone falls back to the tables when the image is
// absent, so LoadImage must fail cleanly.
func TestAMissingImageIsAnErrorNotAPanic(t *testing.T) {
	if _, err := LoadImage(filepath.Join(t.TempDir(), "absent.bin")); err == nil {
		t.Fatal("a missing image loaded")
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "absent.bin")); err == nil {
		t.Fatal("LoadImage created a file")
	}
}

// TestImageStaleFollowsTheSources: the zone rebuilds the image by itself when a dictionary
// or the id map is newer, so "how do we update the dictionary" is answered by a restart -
// the same rule the Lua corpus already follows.
func TestImageStaleFollowsTheSources(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "image.bin")
	dict := filepath.Join(dir, "dict.xml")
	idmap := filepath.Join(dir, "idmap.json")
	pack := filepath.Join(dir, "pack.xml")
	for _, f := range []string{dict, idmap, pack} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if stale, _ := ImageStale(img, idmap, dict, pack); !stale {
		t.Fatal("a missing image must be stale")
	}
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale, why := ImageStale(img, idmap, dict, pack); stale {
		t.Fatalf("a fresh image was called stale: %s", why)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dict, future, future); err != nil {
		t.Fatal(err)
	}
	if stale, _ := ImageStale(img, idmap, dict, pack); !stale {
		t.Fatal("a newer dictionary must make the image stale")
	}
}
