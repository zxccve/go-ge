package zone

// THE DROP ITEM TABLE AS ITS OWN IMAGE (card t_9779e2fd, 2026-11-10).
//
// WHY. With the new MEMSTAGE lines, map 204's start-up table put this ONE loader at the top:
//
//	MEMSTAGE before drop tables  4327 ms
//	zone: DROP item table parsed in 2081 ms     <- datatable_item*.xml, 25,353 rows at 153 columns
//	MEMSTAGE drop tables         6533 ms
//
// encoding/xml decodes the WHOLE row and matches every attribute against the struct's 15 fields,
// for each of 25,353 rows; the parse is the cost, not the data. The table is immutable between
// client patches, so it becomes an image - the same rule the dialog text, the script index, the
// buff table and the monster table already follow (one image per table).
//
// WHAT IT CARRIES, exactly the three indexes ItemTable answers from:
//   - byID    every ItemRow, in id order, 17 fields each;
//   - byName  stored as written (lowercased ClassName -> id). It is NOT rebuilt from the rows:
//             two rows can share a ClassID, so byID keeps one row while byName kept a name from
//             each - the round-trip test caught exactly that (25,348 vs 25,349 entries);
//   - byRule  THE ORDER IS DATA. The reference picks a drop uniformly from the class list's own
//             FILE order (`CItemTable::GetItemsByDropRule`), so the image stores each rule's id
//             list element by element - a set would silently change every drop roll's odds.
//
// A STALE OR MISSING IMAGE IS NOT AN ERROR: the caller parses the XML and writes a new image. The
// stale check is "any `datatable_item*.xml` in the folder is newer than the image", which is
// broader than the loader's own idspace test on purpose - a file that is not the Item idspace
// rebuilding the image costs a second, while missing a real change would serve wrong prices.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const dropImageVersion = 1

var dropImageMagic = [8]byte{'G', 'E', 'D', 'R', 'O', 'P', '0', '1'}

// ItemImagePath is where a zone keeps the drop item image: next to the other emitted tables.
func ItemImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "item_image.bin")
}

// ItemImageStale answers whether the image is older than any `datatable_item*.xml` it is built
// from. A missing image is stale.
func ItemImageStale(imagePath, xmlDir string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	paths, err := filepath.Glob(filepath.Join(xmlDir, "datatable_item*.xml"))
	if err != nil || len(paths) == 0 {
		return false // no sources to compare against: trust the image rather than refuse it
	}
	for _, p := range paths {
		src, err := os.Stat(p)
		if err != nil {
			continue
		}
		if src.ModTime().After(img.ModTime()) {
			return true
		}
	}
	return false
}

// SaveItemImage writes the table to path, atomically.
func SaveItemImage(path string, t *ItemTable) error {
	if t == nil || len(t.byID) == 0 {
		return fmt.Errorf("zone: no item rows to write for %s", path)
	}
	ids := make([]uint32, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var buf bytes.Buffer
	buf.Write(dropImageMagic[:])
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], dropImageVersion)
	buf.Write(b4[:])
	binary.LittleEndian.PutUint32(b4[:], uint32(len(ids)))
	buf.Write(b4[:])
	for _, id := range ids {
		encodeItemRow(&buf, t.byID[id])
	}

	// THE RULE LISTS, in a stable order (sorted by rule name) with their ids in FILE ORDER.
	rules := make([]string, 0, len(t.byRule))
	for rule := range t.byRule {
		rules = append(rules, rule)
	}
	sort.Strings(rules)
	binary.LittleEndian.PutUint32(b4[:], uint32(len(rules)))
	buf.Write(b4[:])
	for _, rule := range rules {
		putString(&buf, rule)
		list := t.byRule[rule]
		binary.LittleEndian.PutUint32(b4[:], uint32(len(list)))
		buf.Write(b4[:])
		for _, id := range list {
			binary.LittleEndian.PutUint32(b4[:], id)
			buf.Write(b4[:])
		}
	}

	// BYNAME IS STORED TOO, and the round-trip test is why: two rows can share a ClassID (the
	// table is five files), so byID keeps the last row while byName kept a name from each - a
	// rebuild from the rows would lose one alias. Identical means identical.
	binary.LittleEndian.PutUint32(b4[:], uint32(len(t.byName)))
	buf.Write(b4[:])
	for name, id := range t.byName {
		putString(&buf, name)
		binary.LittleEndian.PutUint32(b4[:], id)
		buf.Write(b4[:])
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the item image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the item image into place: %w", err)
	}
	return nil
}

// LoadItemImage reads the image and rebuilds the table (byName is derived from the rows).
func LoadItemImage(path string) (*ItemTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for an item image", path, len(data))
	}
	if !bytes.Equal(data[:8], dropImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not an item image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != dropImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, dropImageVersion)
	}
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	r := bytes.NewReader(data[16:])

	t := &ItemTable{
		byID:   make(map[uint32]ItemRow, count),
		byName: make(map[string]uint32, count),
		byRule: make(map[string][]uint32, 16),
	}
	for i := 0; i < count; i++ {
		row, err := decodeItemRow(r)
		if err != nil {
			return nil, fmt.Errorf("zone: %s row %d: %w", path, i, err)
		}
		t.byID[row.ID] = row
	}
	var nRules uint32
	if err := binary.Read(r, binary.LittleEndian, &nRules); err != nil {
		return nil, fmt.Errorf("zone: %s rule count: %w", path, err)
	}
	for i := uint32(0); i < nRules; i++ {
		rule, err := readString(r)
		if err != nil {
			return nil, fmt.Errorf("zone: %s rule %d name: %w", path, i, err)
		}
		var n uint32
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return nil, fmt.Errorf("zone: %s rule %q count: %w", path, rule, err)
		}
		list := make([]uint32, n)
		for j := range list {
			if err := binary.Read(r, binary.LittleEndian, &list[j]); err != nil {
				return nil, fmt.Errorf("zone: %s rule %q id %d: %w", path, rule, j, err)
			}
		}
		t.byRule[rule] = list
	}
	var nNames uint32
	if err := binary.Read(r, binary.LittleEndian, &nNames); err != nil {
		return nil, fmt.Errorf("zone: %s name count: %w", path, err)
	}
	for i := uint32(0); i < nNames; i++ {
		name, err := readString(r)
		if err != nil {
			return nil, fmt.Errorf("zone: %s name %d: %w", path, i, err)
		}
		var id uint32
		if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
			return nil, fmt.Errorf("zone: %s name %q id: %w", path, name, err)
		}
		t.byName[name] = id
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, r.Len())
	}
	return t, nil
}

// LoadItemTablesCached answers the drop item table from the image when it is fresh, otherwise
// parses the XML, writes the image for the next start, and answers the table.
func LoadItemTablesCached(dir, imagePath string) (table *ItemTable, fromImage bool, err error) {
	if !ItemImageStale(imagePath, dir) {
		if t, ierr := LoadItemImage(imagePath); ierr == nil {
			return t, true, nil
		}
	}
	t, err := LoadItemTables(dir)
	if err != nil {
		return nil, false, err
	}
	_ = SaveItemImage(imagePath, t) // a failed write is not a failed start
	return t, false, nil
}

// putString / readString are the length-prefixed string pair this image uses.
func putString(buf *bytes.Buffer, s string) {
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], uint32(len(s)))
	buf.Write(b4[:])
	buf.WriteString(s)
}

func readString(r *bytes.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	s := make([]byte, n)
	if _, err := io.ReadFull(r, s); err != nil {
		return "", err
	}
	return string(s), nil
}

// encodeItemRow writes one row's 17 fields IN ORDER; decodeItemRow reads the same order.
func encodeItemRow(buf *bytes.Buffer, row ItemRow) {
	var b4 [4]byte
	var b8 [8]byte
	putU32 := func(v uint32) {
		binary.LittleEndian.PutUint32(b4[:], v)
		buf.Write(b4[:])
	}
	putI64 := func(v int64) {
		binary.LittleEndian.PutUint64(b8[:], uint64(v))
		buf.Write(b8[:])
	}
	putF := func(v float64) {
		binary.LittleEndian.PutUint64(b8[:], math.Float64bits(v))
		buf.Write(b8[:])
	}
	putBool := func(v bool) {
		if v {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	}

	putU32(row.ID)
	putString(buf, row.Name)
	putU32(uint32(int32(row.Lv)))
	putString(buf, row.DropRule)
	putBool(row.Unstack)
	putI64(row.MaxStack)
	putString(buf, row.OptionGroup)
	putU32(row.Option1ID)
	putF(row.DropRareFreq)
	putString(buf, row.GroupName)
	putU32(uint32(int32(row.UseLv)))
	putString(buf, row.ScpType)
	putF(row.Arg1)
	putBool(row.Arg1Known)
	putF(row.Arg2)
	putBool(row.Arg2Known)
	putString(buf, row.BuffName)
}

// decodeItemRow reads one row written by encodeItemRow.
func decodeItemRow(r *bytes.Reader) (ItemRow, error) {
	var row ItemRow
	var b4 [4]byte
	var b8 [8]byte
	getU32 := func() (uint32, error) {
		if _, err := io.ReadFull(r, b4[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(b4[:]), nil
	}
	getI64 := func() (int64, error) {
		if _, err := io.ReadFull(r, b8[:]); err != nil {
			return 0, err
		}
		return int64(binary.LittleEndian.Uint64(b8[:])), nil
	}
	getF := func() (float64, error) {
		if _, err := io.ReadFull(r, b8[:]); err != nil {
			return 0, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b8[:])), nil
	}
	getBool := func() (bool, error) {
		var one [1]byte
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return false, err
		}
		return one[0] != 0, nil
	}

	var err error
	if row.ID, err = getU32(); err != nil {
		return row, err
	}
	if row.Name, err = readString(r); err != nil {
		return row, err
	}
	lv, err := getU32()
	if err != nil {
		return row, err
	}
	row.Lv = int(int32(lv))
	if row.DropRule, err = readString(r); err != nil {
		return row, err
	}
	if row.Unstack, err = getBool(); err != nil {
		return row, err
	}
	if row.MaxStack, err = getI64(); err != nil {
		return row, err
	}
	if row.OptionGroup, err = readString(r); err != nil {
		return row, err
	}
	if row.Option1ID, err = getU32(); err != nil {
		return row, err
	}
	if row.DropRareFreq, err = getF(); err != nil {
		return row, err
	}
	if row.GroupName, err = readString(r); err != nil {
		return row, err
	}
	ul, err := getU32()
	if err != nil {
		return row, err
	}
	row.UseLv = int(int32(ul))
	if row.ScpType, err = readString(r); err != nil {
		return row, err
	}
	if row.Arg1, err = getF(); err != nil {
		return row, err
	}
	if row.Arg1Known, err = getBool(); err != nil {
		return row, err
	}
	if row.Arg2, err = getF(); err != nil {
		return row, err
	}
	if row.Arg2Known, err = getBool(); err != nil {
		return row, err
	}
	if row.BuffName, err = readString(r); err != nil {
		return row, err
	}
	return row, nil
}

// itemImagePath is where this process keeps the drop item image; SetItemImagePath sets it once at
// start-up. EMPTY means "no image": the XML is parsed, which is what every unit test wants.
var itemImagePath string

// SetItemImagePath tells the drop loader where its image lives (main.go, from the text image's
// directory). Call it before ArmDropTables.
func SetItemImagePath(path string) { itemImagePath = path }

// loadItemTableWithImage is the production reader behind dropTables: the image when it is fresh,
// the XML otherwise (and then the image is written for the next start).
func loadItemTableWithImage(dir string) (*ItemTable, error) {
	if itemImagePath == "" {
		return LoadItemTables(dir)
	}
	t, fromImage, err := LoadItemTablesCached(dir, itemImagePath)
	if err != nil {
		return nil, err
	}
	if fromImage {
		log.Printf("zone: DROP item table from the IMAGE %s - %d row(s), no XML parse",
			itemImagePath, t.Len())
	}
	return t, nil
}
