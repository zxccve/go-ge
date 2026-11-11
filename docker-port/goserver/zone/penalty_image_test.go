// THE DROP-PENALTY IMAGE MUST CARRY EVERY LEVEL, AND REMEMBER WHICH EXPORT IT CAME FROM
// (card t_88e45fd8).
//
// This table has one rule the other images do not: its source is NOT in the client's `ge\xml`
// folder, so "is the image stale" means "would `dropPenaltyPaths()` choose the same file
// now, and is that file newer". The tests below pin all three answers - same file, a
// different file, and no file at all.
package zone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestThePenaltyImageRoundTripsTheRealExport(t *testing.T) {
	src := firstExistingPenaltySource(dropPenaltyPaths())
	if src == "" {
		t.Skipf("no datatable_droppenalty.xml on any of %v", dropPenaltyPaths())
	}
	startXML := time.Now()
	fromXML, err := LoadDropPenalty(src)
	if err != nil {
		t.Fatalf("LoadDropPenalty(%q): %v", src, err)
	}
	xmlTook := time.Since(startXML)
	if fromXML.Len() == 0 {
		t.Fatal("the XML loader answered no levels")
	}

	path := filepath.Join(t.TempDir(), "penalty_image.bin")
	if err := SavePenaltyImage(path, fromXML); err != nil {
		t.Fatalf("SavePenaltyImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadPenaltyImage(path)
	if err != nil {
		t.Fatalf("LoadPenaltyImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if fromImage.Len() != fromXML.Len() {
		t.Fatalf("the image holds %d level(s), the export %d", fromImage.Len(), fromXML.Len())
	}
	for lv, want := range fromXML.rows {
		got, ok := fromImage.rows[lv]
		if !ok {
			t.Fatalf("killer level %d is missing from the image", lv)
		}
		if got != want {
			t.Fatalf("killer level %d came back different:\n export %+v\n image  %+v", lv, want, got)
		}
	}
	// The recorded source is what staleness reads, so it must survive the round trip.
	if fromImage.source != fromXML.source {
		t.Fatalf("the image records source %q, the export was %q", fromImage.source, fromXML.source)
	}
	// And the ANSWER the table gives must be identical: every level's six ranges, as the
	// penalised kill reads them.
	checked := 0
	for lv := 1; lv <= 130; lv++ {
		for gap := -100; gap <= 100; gap++ {
			wantP, wantOK := fromXML.Penalty(lv, lv+gap)
			gotP, gotOK := fromImage.Penalty(lv, lv+gap)
			if wantP != gotP || wantOK != gotOK {
				t.Fatalf("Penalty(%d, %d): image answers %v/%v, export %v/%v",
					lv, lv+gap, gotP, gotOK, wantP, wantOK)
			}
			checked++
		}
	}
	t.Logf("%d level(s), %d lookups identical: export %s, image %s",
		fromXML.Len(), checked, xmlTook, imageTook)
}

func TestAPenaltyImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not a drop-penalty image at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadPenaltyImage(path); err == nil {
		t.Fatal("a junk file was accepted as a drop-penalty image")
	}
}

// TestThePenaltyImageTracksTheExportItWasBuiltFrom is pass condition 2 for this half: the
// staleness rule is the CHOSEN path, not a fixed one.
func TestThePenaltyImageTracksTheExportItWasBuiltFrom(t *testing.T) {
	src := firstExistingPenaltySource(dropPenaltyPaths())
	if src == "" {
		t.Skipf("no datatable_droppenalty.xml on any of %v", dropPenaltyPaths())
	}
	dir := t.TempDir()
	// A COPY, so touching a mtime cannot disturb the repository's own export.
	own := filepath.Join(dir, "datatable_droppenalty.xml")
	copyFile(t, src, own)
	tbl, err := LoadDropPenalty(own)
	if err != nil {
		t.Fatalf("LoadDropPenalty(%q): %v", own, err)
	}
	img := filepath.Join(dir, "penalty_image.bin")
	if err := SavePenaltyImage(img, tbl); err != nil {
		t.Fatalf("SavePenaltyImage: %v", err)
	}

	// (a) the same file, untouched: fresh.
	if PenaltyImageStale(img, []string{own}) {
		t.Fatal("an image built from this exact file was called stale")
	}

	// (b) a DIFFERENT file is what would load now: stale, even though the image's own
	// source is still there and still older.
	other := filepath.Join(dir, "other.xml")
	copyFile(t, src, other)
	if !PenaltyImageStale(img, []string{other}) {
		t.Fatal("an image built from another export was called fresh")
	}
	// The candidate ORDER matters too: the first existing one is what the loader reads.
	if !PenaltyImageStale(img, []string{other, own}) {
		t.Fatal("a candidate list whose FIRST existing file differs was called fresh")
	}

	// (c) the source changed after the image: stale.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(own, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !PenaltyImageStale(img, []string{own}) {
		t.Fatal("an export newer than the image was called fresh")
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(own, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if PenaltyImageStale(img, []string{own}) {
		t.Fatal("the same export, older than the image, was called stale")
	}

	// (d) NO export anywhere: the image is all there is, so it is served (the rule that
	// makes a baked image useful in a container with no server_xml\ copy).
	if PenaltyImageStale(img, []string{filepath.Join(dir, "gone.xml")}) {
		t.Fatal("an image with no source on this host was called stale")
	}
	// (e) the SAME file reached by a different spelling is NOT a move: this is the container
	// case, where the dev box's "../../ge/server_xml/..." and "/ge/server_xml/..." are the
	// same bytes. A relative spelling is used when the temp dir shares the working
	// directory's volume; a ".." hop otherwise (both must pass through os.SameFile).
	alt := ""
	if cwd, err := os.Getwd(); err == nil {
		if rel, rerr := filepath.Rel(cwd, own); rerr == nil {
			alt = rel
		}
	}
	if alt == "" {
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		alt = filepath.Join(sub, "..", filepath.Base(own))
	}
	if alt != own && PenaltyImageStale(img, []string{alt}) {
		t.Fatalf("the same export spelled %q was called a different file", alt)
	}

	// (f) a missing image is always stale.
	if !PenaltyImageStale(filepath.Join(dir, "absent.bin"), []string{own}) {
		t.Fatal("a missing image was called fresh")
	}
}

// TestACorruptPenaltyImageFallsBackToTheExport is pass condition 2's other half: a junk
// image must not stop a start, and the next one must be readable again.
func TestACorruptPenaltyImageFallsBackToTheExport(t *testing.T) {
	src := firstExistingPenaltySource(dropPenaltyPaths())
	if src == "" {
		t.Skipf("no datatable_droppenalty.xml on any of %v", dropPenaltyPaths())
	}
	dir := t.TempDir()
	own := filepath.Join(dir, "datatable_droppenalty.xml")
	copyFile(t, src, own)
	img := filepath.Join(dir, "penalty_image.bin")
	if err := os.WriteFile(img, []byte("this is not an image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tbl, fromImage, err := LoadDropPenaltyCached([]string{own}, img)
	if err != nil {
		t.Fatalf("a corrupt image was not survivable: %v", err)
	}
	if fromImage {
		t.Fatal("a corrupt image was reported as the source")
	}
	if tbl.Len() == 0 {
		t.Fatal("the fallback answered an empty table")
	}
	if _, err := LoadPenaltyImage(img); err != nil {
		t.Fatalf("the broken image was not rewritten: %v", err)
	}
	// The rewritten image is what the NEXT start reads.
	tbl2, fromImage2, err := LoadDropPenaltyCached([]string{own}, img)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if !fromImage2 {
		t.Fatal("the rewritten image was not used on the next start")
	}
	if !reflect.DeepEqual(tbl.rows, tbl2.rows) {
		t.Fatal("the rewritten image answers a different table")
	}
}

// copyFile (stance_pick_test.go) is the one-file copy these tests use: the real export is
// 55 KB and the image must be built from a file whose mtime this test may touch.
