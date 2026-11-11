package zone

// THE DROP PENALTY TABLE AS AN IMAGE (card t_88e45fd8, 2026-11-11).
//
// WHY. It is the last table inside the drop block that still parsed XML on every start:
//
//	MEMSTAGE before drop tables        717 ms
//	MEMSTAGE drop tables               740 ms   (item image, map image, and this parse)
//
// MEASURED, not assumed: `LoadDropPenalty` over the shipped export is 2.1-3.1 ms per call
// (five timed parses, 204 levels), NOT the ~111 ms the card quoted - that number did not
// reproduce on this build. The image still takes it to a file read, and it is one table
// fewer whose cost is a function of XML: the same rule the item, map, monster, skill, buff
// and text tables already follow.
//
// THE ONE RULE THIS IMAGE NEEDS THAT THE OTHERS DO NOT: its source is NOT in the client's
// `ge\xml` folder. `dropPenaltyPaths()` picks a path (GE_DROP_PENALTY first, then four
// directory guesses) and the table may be absent entirely - a zone without it multiplies
// every frequency by 1.0 and says so. So the image REMEMBERS the path it was built from,
// and staleness asks two questions:
//
//	1. is the path `dropPenaltyPaths()` would choose NOW the one the image was built from?
//	   A different answer (GE_DROP_PENALTY moved, another copy appeared earlier in the
//	   list, the export was deleted) means the image no longer describes what would load;
//	2. is that file newer than the image?
//
// WHEN NO CANDIDATE EXISTS AT ALL the image is TRUSTED, which is the same rule the item and
// map images follow ("no source to compare against: trust the image"). It is what makes the
// baked image useful in a container that carries no `server_xml\` export.
//
// A STALE, MISSING OR CORRUPT IMAGE IS NOT AN ERROR: the caller parses the export and writes
// a new image.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const penaltyImageVersion = 1

var penaltyImageMagic = [8]byte{'G', 'E', 'P', 'E', 'N', 'L', 'T', '1'}

// PenaltyImagePath is where a zone keeps the drop-penalty image: next to the other
// emitted tables.
func PenaltyImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "penalty_image.bin")
}

// SavePenaltyImage writes the table (and the path it was read from) to path, atomically.
func SavePenaltyImage(path string, t *DropPenaltyTable) error {
	if t == nil || len(t.rows) == 0 {
		return fmt.Errorf("zone: no drop-penalty levels to write for %s", path)
	}
	levels := make([]int, 0, len(t.rows))
	for lv := range t.rows {
		levels = append(levels, lv)
	}
	sort.Ints(levels)

	var buf bytes.Buffer
	buf.Write(penaltyImageMagic[:])
	var b4 [4]byte
	var b8 [8]byte
	binary.LittleEndian.PutUint32(b4[:], penaltyImageVersion)
	buf.Write(b4[:])
	// THE SOURCE PATH IS PART OF THE IMAGE: see the staleness note at the top.
	putString(&buf, t.source)
	binary.LittleEndian.PutUint32(b4[:], uint32(len(levels)))
	buf.Write(b4[:])
	for _, lv := range levels {
		row := t.rows[lv]
		binary.LittleEndian.PutUint32(b4[:], uint32(int32(lv)))
		buf.Write(b4[:])
		for _, r := range row {
			binary.LittleEndian.PutUint32(b4[:], uint32(int32(r.Min)))
			buf.Write(b4[:])
			binary.LittleEndian.PutUint32(b4[:], uint32(int32(r.Max)))
			buf.Write(b4[:])
			binary.LittleEndian.PutUint64(b8[:], math.Float64bits(r.Penalty))
			buf.Write(b8[:])
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the drop-penalty image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the drop-penalty image into place: %w", err)
	}
	return nil
}

// LoadPenaltyImage reads the image and rebuilds the table.
func LoadPenaltyImage(path string) (*DropPenaltyTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a drop-penalty image",
			path, len(data))
	}
	if !bytes.Equal(data[:8], penaltyImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a drop-penalty image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != penaltyImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, penaltyImageVersion)
	}
	r := bytes.NewReader(data[12:])
	source, err := readString(r)
	if err != nil {
		return nil, fmt.Errorf("zone: %s source path: %w", path, err)
	}
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, fmt.Errorf("zone: %s level count: %w", path, err)
	}
	t := &DropPenaltyTable{rows: make(map[int][6]dropPenaltyRange, count), source: source}
	for i := uint32(0); i < count; i++ {
		var lv uint32
		if err := binary.Read(r, binary.LittleEndian, &lv); err != nil {
			return nil, fmt.Errorf("zone: %s level %d: %w", path, i, err)
		}
		var row [6]dropPenaltyRange
		for j := range row {
			var min, max uint32
			if err := binary.Read(r, binary.LittleEndian, &min); err != nil {
				return nil, fmt.Errorf("zone: %s level %d range %d min: %w", path, i, j, err)
			}
			if err := binary.Read(r, binary.LittleEndian, &max); err != nil {
				return nil, fmt.Errorf("zone: %s level %d range %d max: %w", path, i, j, err)
			}
			var bits uint64
			if err := binary.Read(r, binary.LittleEndian, &bits); err != nil {
				return nil, fmt.Errorf("zone: %s level %d range %d penalty: %w", path, i, j, err)
			}
			row[j] = dropPenaltyRange{
				Min: int(int32(min)), Max: int(int32(max)),
				Penalty: math.Float64frombits(bits),
			}
		}
		t.rows[int(int32(lv))] = row
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, r.Len())
	}
	if len(t.rows) == 0 {
		return nil, fmt.Errorf("zone: %s holds no drop-penalty level", path)
	}
	return t, nil
}

// PenaltyImageStale answers whether the image no longer describes what the loader would
// read: a different path than the one it was built from, or a newer source. A missing
// image is stale; an image with no source anywhere on this host is NOT (nothing newer
// exists to compare against - see the note at the top of this file).
func PenaltyImageStale(imagePath string, candidates []string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	t, err := LoadPenaltyImage(imagePath)
	if err != nil {
		return true // corrupt: rebuild rather than serve something unreadable
	}
	chosen := firstExistingPenaltySource(candidates)
	if chosen == "" {
		return false // no export on this host: the image is all there is
	}
	if t.source != "" {
		cs, cerr := os.Stat(chosen)
		ss, serr := os.Stat(t.source)
		if cerr == nil && serr == nil {
			// THE SAME FILE, however it is spelled. A baked image records the path the dev
			// box used ("../../ge/server_xml/..."), which in a container may be reached as
			// "/ge/server_xml/..." - string equality would call that "moved" and rebuild a
			// correct image on every start. Identity is the question being asked.
			if !os.SameFile(cs, ss) {
				return true // the loader would read a DIFFERENT file than the image was built from
			}
			return cs.ModTime().After(img.ModTime())
		}
		if serr != nil {
			return true // the export the image was built from is gone; another one is here
		}
	}
	src, err := os.Stat(chosen)
	if err != nil {
		return false // it vanished between the two stats: nothing newer is on offer
	}
	return src.ModTime().After(img.ModTime())
}

// firstExistingPenaltySource is the path `dropPenaltyPaths()` would actually load: the
// first candidate that exists. Empty when none does.
func firstExistingPenaltySource(candidates []string) string {
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

// LoadDropPenaltyFrom reads the first candidate export that parses, and records which one
// it was (the image carries that path - see PenaltyImageStale).
func LoadDropPenaltyFrom(candidates []string) (*DropPenaltyTable, error) {
	for _, p := range candidates {
		t, err := LoadDropPenalty(p)
		if err != nil {
			continue
		}
		return t, nil
	}
	return nil, fmt.Errorf("zone: no drop-penalty export readable from %v", candidates)
}

// LoadDropPenaltyCached answers the penalty table from the image when it is fresh,
// otherwise parses the export, writes the image for the next start, and answers the table.
func LoadDropPenaltyCached(candidates []string, imagePath string) (table *DropPenaltyTable, fromImage bool, err error) {
	if !PenaltyImageStale(imagePath, candidates) {
		if t, ierr := LoadPenaltyImage(imagePath); ierr == nil {
			return t, true, nil
		}
	}
	table, err = LoadDropPenaltyFrom(candidates)
	if err != nil {
		return nil, false, err
	}
	_ = SavePenaltyImage(imagePath, table) // a failed write is not a failed start
	return table, false, nil
}

// penaltyImagePath is where this process keeps the drop-penalty image; SetPenaltyImagePath
// sets it once at start-up. EMPTY means "no image": the export is parsed, which is what
// every unit test wants.
var penaltyImagePath string

// SetPenaltyImagePath tells the penalty loader where its image lives (main.go, from the
// text image's directory). Call it before ArmDropTables.
func SetPenaltyImagePath(path string) { penaltyImagePath = path }

// loadDropPenaltyWithImage is the production reader behind dropTables: the image when it
// is fresh, the export otherwise (and then the image is written for the next start).
func loadDropPenaltyWithImage() (*DropPenaltyTable, error) {
	candidates := dropPenaltyPaths()
	if penaltyImagePath == "" {
		return LoadDropPenaltyFrom(candidates)
	}
	t, fromImage, err := LoadDropPenaltyCached(candidates, penaltyImagePath)
	if err != nil {
		return nil, err
	}
	if fromImage {
		log.Printf("zone: DROP penalty table from the IMAGE %s - %d level(s) (built from %s), "+
			"no XML parse", penaltyImagePath, t.Len(), t.source)
	}
	return t, nil
}
