// THE SIGHT RING'S MAP TABLE MUST SURVIVE ITS IMAGE, COLUMN FOR COLUMN (card t_38b6216f).
//
// WHY THIS FILE EXISTS. `LoadSightTable` was the last start-up table still parsed as
// `eachClass` rows - 4.2 MB of `data\map_table.xml` and an 85-92 ms `MEMSTAGE sight ring`
// stage on map 4063. It is now `out\sight_image.bin` (zone\sight_image.go). An image is only
// worth having if it answers EXACTLY what the XML answers, so these tests pin four things:
//
//  1. every row of the real `data\map_table.xml` comes back field for field (all five
//     columns the loader reads, and the row count);
//  2. staleness is measured against the file the loader would read - and an image with no
//     source on this host is served, which is what makes a container bake work;
//  3. a missing or corrupt image falls back to the XML and the zone STILL STARTS with the
//     same `zone: sight: map N ... leave X update Y ... the ring is ON` line;
//  4. the image and the XML answer the SAME radii for a sample of maps, INCLUDING a map with
//     no row, which must keep the `no map_table row for map N - sending to everyone` line.
//
// RED when the image is wrong: blanking ViewMiddleRange in the writer makes test 1 fail at
// "map 4063 ... middle 4000, image 0", and reversing two columns fails naming the column.
package zone

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realMapTable is the shipped `data\map_table.xml`, found the way the loader finds it.
func realMapTable(t *testing.T) string {
	t.Helper()
	src := firstExistingSightSource(sightTablePaths())
	if src == "" {
		t.Fatalf("no data\\map_table.xml on any of %v", sightTablePaths())
	}
	return src
}

// sightImageFresh is sightStartFresh for the image arms: it clears BOTH caches AND points
// the process at `image` ("" = parse the XML), restoring everything afterwards.
func sightImageFresh(t *testing.T, image string) {
	t.Helper()
	sightStartFresh(t)
	old := sightImagePath
	sightImagePath = image
	t.Cleanup(func() { sightImagePath = old })
}

// TestTheSightImageRoundTripsTheRealMapTable is pass condition 1: every row of the real
// table, every column the loader reads.
func TestTheSightImageRoundTripsTheRealMapTable(t *testing.T) {
	src := realMapTable(t)

	startXML := time.Now()
	fromXML, err := LoadSightTable(src)
	if err != nil {
		t.Fatalf("LoadSightTable(%q): %v", src, err)
	}
	xmlTook := time.Since(startXML)
	if fromXML.Len() == 0 {
		t.Fatal("the XML loader answered no map rows")
	}

	path := filepath.Join(t.TempDir(), "sight_image.bin")
	if err := SaveSightImage(path, fromXML); err != nil {
		t.Fatalf("SaveSightImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadSightImage(path)
	if err != nil {
		t.Fatalf("LoadSightImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if fromImage.Len() != fromXML.Len() {
		t.Fatalf("the image holds %d map row(s), the XML %d", fromImage.Len(), fromXML.Len())
	}
	for id, want := range fromXML.rows {
		got, ok := fromImage.rows[id]
		if !ok {
			t.Fatalf("map %d is missing from the image", id)
		}
		// EVERY COLUMN THE LOADER READS, one comparison per column so a failure names it.
		if got.ViewRange != want.ViewRange {
			t.Fatalf("map %d ViewRange: image %d, XML %d", id, got.ViewRange, want.ViewRange)
		}
		if got.ViewMiddleRange != want.ViewMiddleRange {
			t.Fatalf("map %d ViewMiddleRange: image %d, XML %d",
				id, got.ViewMiddleRange, want.ViewMiddleRange)
		}
		if got.PcViewDist != want.PcViewDist {
			t.Fatalf("map %d PcViewDist: image %d, XML %d", id, got.PcViewDist, want.PcViewDist)
		}
		if got.MonViewDist != want.MonViewDist {
			t.Fatalf("map %d MonViewDist: image %d, XML %d", id, got.MonViewDist, want.MonViewDist)
		}
		if got != want {
			t.Fatalf("map %d came back different: XML %+v, image %+v", id, want, got)
		}
	}
	// The recorded source is what staleness reads, so it must survive the round trip.
	if fromImage.source != fromXML.source || fromImage.source != src {
		t.Fatalf("the image records source %q, the export was %q (wanted %q)",
			fromImage.source, fromXML.source, src)
	}
	// And the ANSWER the ring builds from each row must be identical, for every map.
	radii := 0
	for id := range fromXML.rows {
		a, b := fromImage.rows[id], fromXML.rows[id]
		wv, wm, wok := sightRadiiFor(a)
		gv, gm, gok := sightRadiiFor(b)
		if wv != gv || wm != gm || wok != gok {
			t.Fatalf("map %d radii: image %d/%d (armed %v), XML %d/%d (armed %v)",
				id, gv, gm, gok, wv, wm, wok)
		}
		radii++
	}
	t.Logf("%d map row(s) identical column by column, %d radii identical: XML %s, image %s",
		fromXML.Len(), radii, xmlTook, imageTook)
}

// TestASightImageThatIsNotOneIsRefused: junk is never mistaken for a table.
func TestASightImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("this is not a sight image at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSightImage(path); err == nil {
		t.Fatal("a junk file was accepted as a sight image")
	}
}

// TestTheSightImageTracksTheMapTableItWasBuiltFrom is pass condition 2's staleness half.
func TestTheSightImageTracksTheMapTableItWasBuiltFrom(t *testing.T) {
	src := realMapTable(t)
	dir := t.TempDir()
	own := filepath.Join(dir, "map_table.xml")
	copyFile(t, src, own)
	tbl, err := LoadSightTable(own)
	if err != nil {
		t.Fatalf("LoadSightTable(%q): %v", own, err)
	}
	img := filepath.Join(dir, "sight_image.bin")
	if err := SaveSightImage(img, tbl); err != nil {
		t.Fatalf("SaveSightImage: %v", err)
	}

	// (a) the same file, untouched: fresh.
	if SightImageStale(img, []string{own}) {
		t.Fatal("an image built from this exact file was called stale")
	}

	// (b) a DIFFERENT file is what would load now: stale, even though the image's own source
	// is still there and still older.
	other := filepath.Join(dir, "other.xml")
	copyFile(t, src, other)
	if !SightImageStale(img, []string{other}) {
		t.Fatal("an image built from another map_table.xml was called fresh")
	}
	// The candidate ORDER matters: the first existing file is what the loader reads.
	if !SightImageStale(img, []string{other, own}) {
		t.Fatal("a candidate list whose FIRST existing file differs was called fresh")
	}

	// (c) the source changed after the image: stale; older than the image: fresh.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(own, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !SightImageStale(img, []string{own}) {
		t.Fatal("a map_table.xml newer than the image was called fresh")
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(own, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if SightImageStale(img, []string{own}) {
		t.Fatal("the same map_table.xml, older than the image, was called stale")
	}

	// (d) NO map_table.xml anywhere: the image is all there is (the container bake).
	if SightImageStale(img, []string{filepath.Join(dir, "gone.xml")}) {
		t.Fatal("an image with no source on this host was called stale")
	}

	// (e) the SAME file reached by another spelling is NOT a move: `..\data\map_table.xml`
	// from `out\` and an absolute path are one file, which is how a baked image survives.
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
	if alt != own && SightImageStale(img, []string{alt}) {
		t.Fatalf("the same map_table.xml spelled %q was called a different file", alt)
	}

	// (f) a missing image is always stale.
	if !SightImageStale(filepath.Join(dir, "absent.bin"), []string{own}) {
		t.Fatal("a missing image was called fresh")
	}
}

// TestACorruptSightImageFallsBackAndTheRingLineIsIdentical is pass condition 2's other half,
// at the level the zone actually runs: the ring line must be the SAME TEXT whether the table
// came from a good image, a corrupt one, or no image at all.
func TestACorruptSightImageFallsBackAndTheRingLineIsIdentical(t *testing.T) {
	src := realMapTable(t)
	dir := t.TempDir()
	own := filepath.Join(dir, "map_table.xml")
	copyFile(t, src, own)
	old := os.Getenv("GE_MAP_TABLE")
	if err := os.Setenv("GE_MAP_TABLE", own); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if old == "" {
			os.Unsetenv("GE_MAP_TABLE")
			return
		}
		os.Setenv("GE_MAP_TABLE", old)
	})
	img := filepath.Join(dir, "sight_image.bin")

	ringLine := func(t *testing.T, image string) string {
		t.Helper()
		sightImageFresh(t, image)
		logs := &bytes.Buffer{}
		h := &Handler{MapID: 4063, MapName: "btl_ktv03", Log: log.New(logs, "", 0)}
		if _, _, armed := h.PrimeSight(); !armed {
			t.Fatalf("map 4063 did not arm the ring with image %q:\n%s", image, logs.String())
		}
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "zone: sight: map 4063") && strings.Contains(line, "the ring is ON") {
				return line[strings.Index(line, "zone: sight:"):]
			}
		}
		t.Fatalf("no ring line with image %q:\n%s", image, logs.String())
		return ""
	}

	// ARM 1: no image at all - the XML is parsed and the image is written for next time.
	fromXML := ringLine(t, img)
	if _, err := os.Stat(img); err != nil {
		t.Fatalf("the XML arm did not write the image: %v", err)
	}
	// ARM 2: the image just written - a plain file read.
	fromImage := ringLine(t, img)
	// ARM 3: a corrupt image - the start must survive it and print the same line.
	if err := os.WriteFile(img, []byte("not an image, and not even close"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fromCorrupt := ringLine(t, img)
	// ARM 4: the image gone - the same fallback.
	if err := os.Remove(img); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fromMissing := ringLine(t, img)

	for _, arm := range []struct{ name, line string }{
		{"image", fromImage}, {"corrupt", fromCorrupt}, {"missing", fromMissing},
	} {
		if arm.line != fromXML {
			t.Fatalf("the %s arm printed a DIFFERENT ring line:\n XML     %s\n %-7s %s",
				arm.name, fromXML, arm.name, arm.line)
		}
	}
	if !strings.Contains(fromXML, "leave 6000 update 4000") {
		t.Fatalf("the ring line no longer carries 6000/4000 for map 4063: %s", fromXML)
	}
	// The corrupt image was rebuilt by its arm, so the next start reads an image again.
	if _, err := LoadSightImage(img); err != nil {
		t.Fatalf("the rebuilt image is not readable: %v", err)
	}
	t.Logf("ring line identical in all four arms: %s", fromXML)
}

// TestTheImageAndTheXMLAnswerTheSameRadiiForEveryMap is pass condition 4: a sample of the
// shipped maps, through the zone's own start-up call, in both arms - plus a map with NO row,
// which must keep the `no map_table row` line and stay unarmed.
func TestTheImageAndTheXMLAnswerTheSameRadiiForEveryMap(t *testing.T) {
	src := realMapTable(t)
	xmlTable, err := LoadSightTable(src)
	if err != nil {
		t.Fatalf("LoadSightTable(%q): %v", src, err)
	}
	img := filepath.Join(t.TempDir(), "sight_image.bin")
	if err := SaveSightImage(img, xmlTable); err != nil {
		t.Fatalf("SaveSightImage: %v", err)
	}

	// THE SAMPLE: the card's map, the two towns the margin exists for, a field map, the
	// lowest id the table holds, and the highest - so a column that shifts by row is caught.
	ids := []int{4063, 1331, 208, 10, 15}
	if _, ok := xmlTable.Row(1); ok {
		ids = append(ids, 1)
	}
	lo, hi := 0, 0
	for id := range xmlTable.rows {
		if lo == 0 || id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
	}
	ids = append(ids, lo, hi)
	const noRow = 999999
	ids = append(ids, noRow)

	type answer struct {
		view, middle int32
		armed        bool
		line         string
	}
	ask := func(t *testing.T, image string) map[int]answer {
		t.Helper()
		got := map[int]answer{}
		for _, id := range ids {
			sightImageFresh(t, image)
			logs := &bytes.Buffer{}
			h := &Handler{MapID: id, MapName: "sample", Log: log.New(logs, "", 0)}
			view, middle, armed := h.PrimeSight()
			got[id] = answer{view: view, middle: middle, armed: armed,
				line: strings.TrimSpace(logs.String())}
		}
		return got
	}

	fromXML := ask(t, "")
	fromImage := ask(t, img)
	for _, id := range ids {
		x, i := fromXML[id], fromImage[id]
		if x.view != i.view || x.middle != i.middle || x.armed != i.armed {
			t.Fatalf("map %d: image answers %d/%d (armed %v), XML %d/%d (armed %v)",
				id, i.view, i.middle, i.armed, x.view, x.middle, x.armed)
		}
		if id == noRow {
			const want = "zone: sight: no map_table row for map 999999 - sending to everyone"
			if !strings.Contains(i.line, want) {
				t.Fatalf("the no-row map lost its line through the image:\n%s", i.line)
			}
			if x.armed {
				t.Fatal("a map with no row armed the ring in the XML arm")
			}
		}
	}
	// The card's map, quoted so a reader of the test log sees the numbers the live run logs.
	t.Logf("map 4063: leave/update %d/%d (armed %v) from the image, identical to the XML; "+
		"%d maps compared including the no-row map %d",
		fromImage[4063].view, fromImage[4063].middle, fromImage[4063].armed, len(ids), noRow)
}
