package zone

// THE SIGHT RING'S MAP TABLE AS AN IMAGE (card t_38b6216f, 2026-11-11).
//
// WHY. `LoadSightTable` is the last start-up table still parsed from XML as `eachClass`
// rows: 4.2 MB of `data\map_table.xml`, 2,144 `<Class>` elements, an `encoding/xml` walk
// that maps every attribute of every row to read FIVE of them. Measured on map 4063, the
// `MEMSTAGE sight ring` stage was 87/85 ms with the parse (92/86 ms on a rebuilt pair) and
// the card that moved the read to start-up (t_a593b9a2) named imaging as the next slice.
// The table is a pure function of one file, so it becomes an image - the same rule the
// monster, skill, stance, item, map, buff, penalty and text tables already follow.
//
// A STALE, MISSING OR CORRUPT IMAGE IS NOT AN ERROR: the caller parses the XML and writes
// a new image, so a zone without one is a slow start, never a wrong one.
//
// THE FIVE COLUMNS ARE THE WHOLE ROW, and they are all `LoadSightTable` reads: ClassID (the
// key), ViewRange (the visit circle, and the row is SKIPPED when it is absent or not
// positive), ViewMiddleRange (the send/freeze circle) and the two CLIENT draw distances
// PcViewDist / MonViewDist (sightRadiiFor's margin). Nothing else in a 108-attribute row is
// read, so nothing else is stored.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
)

const sightImageVersion = 1

var sightImageMagic = [8]byte{'G', 'E', 'S', 'I', 'G', 'H', 'T', '1'}

// SightImagePath is where a zone keeps the sight-ring map-table image: beside the other
// emitted tables, so `out\` and the container's `/app/out` carry it the same way.
func SightImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "sight_image.bin")
}

// SaveSightImage writes the map table (and the file it was read from) to path, atomically.
//
// THE ROWS ARE WRITTEN IN ID ORDER, so two builds of the same XML produce the same bytes and
// a diff of two images says what changed. The order is not data here - the ring looks one map
// up by id and nothing iterates the table - but a stable file is cheaper to trust.
func SaveSightImage(path string, t *SightTable) error {
	if t == nil || len(t.rows) == 0 {
		return fmt.Errorf("zone: no sight map rows to write for %s", path)
	}
	ids := make([]int, 0, len(t.rows))
	for id := range t.rows {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	var buf bytes.Buffer
	buf.Write(sightImageMagic[:])
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], sightImageVersion)
	buf.Write(b4[:])
	// THE SOURCE PATH IS PART OF THE IMAGE: staleness asks whether the loader would read the
	// same FILE now, and a container may reach the dev box's spelling by another path.
	putString(&buf, t.source)
	binary.LittleEndian.PutUint32(b4[:], uint32(len(ids)))
	buf.Write(b4[:])
	for _, id := range ids {
		row := t.rows[id]
		for _, v := range [5]int32{int32(id), row.ViewRange, row.ViewMiddleRange,
			row.PcViewDist, row.MonViewDist} {
			binary.LittleEndian.PutUint32(b4[:], uint32(v))
			buf.Write(b4[:])
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the sight image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the sight image into place: %w", err)
	}
	return nil
}

// LoadSightImage reads the image and rebuilds the table.
func LoadSightImage(path string) (*SightTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a sight image", path, len(data))
	}
	if !bytes.Equal(data[:8], sightImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a sight image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != sightImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, sightImageVersion)
	}
	r := bytes.NewReader(data[12:])
	source, err := readString(r)
	if err != nil {
		return nil, fmt.Errorf("zone: %s source path: %w", path, err)
	}
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("zone: %s row count: %w", path, err)
	}
	if count == 0 {
		// A LEGITIMATE TABLE IS NEVER EMPTY: an image with no row would silently disarm the
		// ring on every map, which is the one failure this loader must not invent.
		return nil, fmt.Errorf("zone: %s holds no sight map row", path)
	}
	t := &SightTable{rows: make(map[int]SightRow, count), source: source}
	for i := uint32(0); i < count; i++ {
		var f [5]int32
		for j := range f {
			var v uint32
			if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
				return nil, fmt.Errorf("zone: %s row %d column %d: %w", path, i, j, err)
			}
			f[j] = int32(v)
		}
		if f[0] <= 0 {
			return nil, fmt.Errorf("zone: %s row %d has map id %d", path, i, f[0])
		}
		t.rows[int(f[0])] = SightRow{ViewRange: f[1], ViewMiddleRange: f[2],
			PcViewDist: f[3], MonViewDist: f[4]}
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, r.Len())
	}
	return t, nil
}

// SightImageStale answers whether the image no longer describes what `LoadSightTable` would
// read: a missing or unreadable image, a different FILE than the one it was built from, or a
// source newer than the image.
//
// THE FOURTH ANSWER IS "NOT STALE", and it is the container case: with no `map_table.xml`
// anywhere on this host there is nothing newer to compare against, so the baked image is
// served. That is the same rule the item and map images follow.
func SightImageStale(imagePath string, candidates []string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	t, err := LoadSightImage(imagePath)
	if err != nil {
		return true // corrupt: rebuild rather than serve something unreadable
	}
	chosen := firstExistingSightSource(candidates)
	if chosen == "" {
		return false // no source on this host: the image is all there is
	}
	if t.source != "" {
		cs, cerr := os.Stat(chosen)
		ss, serr := os.Stat(t.source)
		if cerr == nil && serr == nil {
			// THE SAME FILE, however it is spelled: `..\data\map_table.xml` from `out\` and
			// an absolute path are one file, and string equality would call that a move and
			// rebuild a correct image on every start.
			if !os.SameFile(cs, ss) {
				return true // the loader would read a DIFFERENT file than the image was built from
			}
			return cs.ModTime().After(img.ModTime())
		}
		if serr != nil {
			return true // the file the image was built from is gone; another one is here
		}
	}
	src, err := os.Stat(chosen)
	if err != nil {
		return false // it vanished between the two stats: nothing newer is on offer
	}
	return src.ModTime().After(img.ModTime())
}

// firstExistingSightSource is the path `sightTablePaths()` would actually load: the first
// candidate that exists. Empty when none does.
func firstExistingSightSource(candidates []string) string {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// LoadSightTableFrom reads the first candidate that parses, with the SAME rules the lazy
// loader had: a path that does not exist is skipped, any other error stops the search (a
// `GE_MAP_TABLE` that is unreadable must say so, not fall through to a stale copy).
func LoadSightTableFrom(candidates []string) (*SightTable, error) {
	for _, p := range candidates {
		t, err := LoadSightTable(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		return t, nil
	}
	return nil, os.ErrNotExist
}

// LoadSightTableCached answers the map table from the image when it is fresh, otherwise
// parses the XML, writes the image for the next start, and answers the table.
func LoadSightTableCached(candidates []string, imagePath string) (table *SightTable, fromImage bool, err error) {
	if !SightImageStale(imagePath, candidates) {
		if t, ierr := LoadSightImage(imagePath); ierr == nil {
			return t, true, nil
		}
	}
	table, err = LoadSightTableFrom(candidates)
	if err != nil {
		return nil, false, err
	}
	_ = SaveSightImage(imagePath, table) // a failed write is not a failed start
	return table, false, nil
}

// sightImagePath is where this process keeps the sight image; SetSightImagePath sets it once
// at start-up. EMPTY means "no image": the XML is parsed, which is what every unit test that
// is not about the image wants.
var sightImagePath string

// SetSightImagePath tells the sight loader where its image lives (main.go, from the text
// image's directory). Call it before PrimeSight.
func SetSightImagePath(path string) { sightImagePath = path }

// loadSightTableWithImage is the production reader behind sightTableFor: the image when it is
// fresh, the XML otherwise (and then the image is written for the next start).
func loadSightTableWithImage(candidates []string) (*SightTable, error) {
	if sightImagePath == "" {
		return LoadSightTableFrom(candidates)
	}
	if !SightImageStale(sightImagePath, candidates) {
		if t, ierr := LoadSightImage(sightImagePath); ierr == nil {
			log.Printf("zone: sight: map table from the IMAGE %s - %d row(s) (built from %s), "+
				"no XML parse", sightImagePath, t.Len(), t.source)
			return t, nil
		}
	}
	t, err := LoadSightTableFrom(candidates)
	if err != nil {
		return nil, err
	}
	_ = SaveSightImage(sightImagePath, t) // a failed write is not a failed start
	return t, nil
}
