// THE DROP MAP TABLE AS AN IMAGE (2026-11-10, continuing cards t_96ee949e / t_9779e2fd).
//
// WHY. After the item table's image, the start-up table still showed one 121 ms loader inside the drop
// block:
//
//	zone: DROP map table parsed in 121 ms   <- datatable_map.xml, 2,381 rows x 108 attributes
//
// The zone wants NINE things from each row: seven DropItem ids, their seven frequencies, a class TYPE
// and a drop-BLOCK flag. encoding/xml walks all 108 attributes of all 2,381 rows to answer that, and the
// answer never changes between client patches, so it becomes an image - the same rule the item, monster,
// skill, text and buff tables already follow.
//
// A STALE OR MISSING IMAGE IS NOT AN ERROR: the caller parses the XML and writes a new image, and a
// corrupt one falls back the same way.
//
// THE PENALTY TABLE IS NOT IN HERE: it is ~111 ms, read from an EXPORT rather than the client's xml
// folder, and its own staleness would be a second rule in this file. Carded separately.
package zone

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const mapDropsImageVersion = 1

var mapDropsImageMagic = [8]byte{'G', 'E', 'M', 'A', 'P', 'D', 'R', '1'}

// MapDropsImagePath is where a zone keeps the map-drop image.
func MapDropsImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "mapdrops_image.bin")
}

// MapDropsImageStale answers whether the image is older than `datatable_map.xml`.
func MapDropsImageStale(imagePath, xmlDir string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	src, err := os.Stat(filepath.Join(xmlDir, "datatable_map.xml"))
	if err != nil {
		return false // no source to compare against: trust the image
	}
	return src.ModTime().After(img.ModTime())
}

// SaveMapDropsImage writes the table to path, atomically.
func SaveMapDropsImage(path string, t *MapDropTable) error {
	if t == nil || len(t.rows) == 0 {
		return fmt.Errorf("zone: no map-drop rows to write for %s", path)
	}
	ids := make([]int, 0, len(t.rows))
	for id := range t.rows {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	var buf bytes.Buffer
	buf.Write(mapDropsImageMagic[:])
	var b4 [4]byte
	var b8 [8]byte
	binary.LittleEndian.PutUint32(b4[:], mapDropsImageVersion)
	buf.Write(b4[:])
	binary.LittleEndian.PutUint32(b4[:], uint32(len(ids)))
	buf.Write(b4[:])
	for _, id := range ids {
		row := t.rows[id]
		binary.LittleEndian.PutUint32(b4[:], uint32(int32(id)))
		buf.Write(b4[:])
		for _, v := range row.Item {
			binary.LittleEndian.PutUint32(b4[:], v)
			buf.Write(b4[:])
		}
		for _, v := range row.Freq {
			binary.LittleEndian.PutUint64(b8[:], math.Float64bits(v))
			buf.Write(b8[:])
		}
		if row.Blocked {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		putString(&buf, row.Type)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the map-drop image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the map-drop image into place: %w", err)
	}
	return nil
}

// LoadMapDropsImage reads the image and rebuilds the table.
func LoadMapDropsImage(path string) (*MapDropTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a map-drop image",
			path, len(data))
	}
	if !bytes.Equal(data[:8], mapDropsImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a map-drop image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != mapDropsImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, mapDropsImageVersion)
	}
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	r := bytes.NewReader(data[16:])
	t := &MapDropTable{rows: make(map[int]MapDropRow, count)}
	for i := 0; i < count; i++ {
		var id uint32
		if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
			return nil, fmt.Errorf("zone: %s row %d id: %w", path, i, err)
		}
		var row MapDropRow
		for j := range row.Item {
			if err := binary.Read(r, binary.LittleEndian, &row.Item[j]); err != nil {
				return nil, fmt.Errorf("zone: %s row %d item %d: %w", path, i, j, err)
			}
		}
		for j := range row.Freq {
			if err := binary.Read(r, binary.LittleEndian, &row.Freq[j]); err != nil {
				return nil, fmt.Errorf("zone: %s row %d freq %d: %w", path, i, j, err)
			}
		}
		blocked, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("zone: %s row %d blocked: %w", path, i, err)
		}
		row.Blocked = blocked != 0
		if row.Type, err = readString(r); err != nil {
			return nil, fmt.Errorf("zone: %s row %d type: %w", path, i, err)
		}
		t.rows[int(int32(id))] = row
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, r.Len())
	}
	return t, nil
}

// mapDropsImagePath is where this process keeps the image; SetMapDropsImagePath sets it once.
var mapDropsImagePath string

// SetMapDropsImagePath tells the map-drop loader where its image lives (main.go, from the text
// image's directory). Call it before ArmDropTables.
func SetMapDropsImagePath(path string) { mapDropsImagePath = path }

// loadMapDropsWithImage is the production reader behind dropTables: the image when it is fresh, the
// XML otherwise (and then the image is written for the next start).
func loadMapDropsWithImage(dir string) (*MapDropTable, error) {
	if mapDropsImagePath != "" && !MapDropsImageStale(mapDropsImagePath, dir) {
		if t, ierr := LoadMapDropsImage(mapDropsImagePath); ierr == nil {
			return t, nil
		}
	}
	t, err := LoadMapDrops(dir)
	if err != nil {
		return nil, err
	}
	if mapDropsImagePath != "" {
		_ = SaveMapDropsImage(mapDropsImagePath, t) // a failed write is not a failed start
	}
	return t, nil
}
