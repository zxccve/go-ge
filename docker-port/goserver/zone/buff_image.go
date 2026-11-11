package zone

// THE BUFF TABLE AS ITS OWN IMAGE, so every zone stops parsing 1,962 rows x 54 attributes
// into 106,000 Go map entries (26 MB, 2.2 s) on every start.
//
// The owner's rule for shared data (2026-11-09): ONE IMAGE PER TABLE - "if we edit item and not
// buff we dont have to rebuild the whole thing". So this file knows only the buff table; the item,
// text and script images are separate files with separate staleness.
//
// Shape: the attribute NAMES are interned once, each row keeps (nameIdx, len) pairs, and every
// value lives back to back in one blob. A row's Attrs is a view over that blob, so a buff costs
// bytes in one shared buffer instead of a map with 54 string keys.
//
// A FIRST BOOT MAY BE SLOW: a missing image is built from the XML and written; every start after
// that reads one file.

import (
	"encoding/binary"
	"fmt"
	"os"
)

const buffImageVersion = 1

var buffImageMagic = [8]byte{'G', 'E', 'B', 'U', 'F', 'F', '0', '1'}

// BuffImagePath is where a zone keeps the buff image: next to the other emitted tables.
func BuffImagePath(dir string) string { return dir + string(os.PathSeparator) + "buff_image.bin" }

// BuffImageStale answers whether the image is older than the XML it was built from. A missing
// image is stale, so the caller builds one (a first boot may be slow - the owner's rule).
func BuffImageStale(imagePath, xmlPath string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	src, err := os.Stat(xmlPath)
	if err != nil {
		return false // no source to compare against: trust the image rather than refuse it
	}
	return src.ModTime().After(img.ModTime())
}

// SaveBuffsImage writes the table to path (atomic: temp file then rename).
func SaveBuffsImage(path string, t *BuffTable) error {
	if t == nil || len(t.rows) == 0 {
		return fmt.Errorf("zone: no buff rows to write for %s", path)
	}
	// Names, in first-seen order.
	nameIdx := map[string]int{}
	var names []string
	for i := range t.rows {
		a := t.rows[i].Attrs
		for k := 0; k < a.Len(); k++ {
			if _, ok := nameIdx[a.names[k]]; !ok {
				nameIdx[a.names[k]] = len(names)
				names = append(names, a.names[k])
			}
		}
	}

	var rowBuf, valBuf []byte
	for i := range t.rows {
		r := &t.rows[i]
		a := r.Attrs
		rowBuf = binary.LittleEndian.AppendUint32(rowBuf, r.ID)
		rowBuf = binary.LittleEndian.AppendUint16(rowBuf, uint16(a.Len()))
		for k := 0; k < a.Len(); k++ {
			v := a.vals[k]
			if len(v) > 0xFFFF {
				return fmt.Errorf("zone: buff %s attribute %s is too long for the image", r.Name, a.names[k])
			}
			rowBuf = binary.LittleEndian.AppendUint16(rowBuf, uint16(nameIdx[a.names[k]]))
			rowBuf = binary.LittleEndian.AppendUint16(rowBuf, uint16(len(v)))
			valBuf = append(valBuf, v...)
		}
	}

	var buf []byte
	buf = append(buf, buffImageMagic[:]...)
	buf = binary.LittleEndian.AppendUint32(buf, buffImageVersion)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(t.rows)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(names)))
	for _, n := range names {
		if len(n) > 0xFFFF {
			return fmt.Errorf("zone: attribute name too long: %s", n)
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(n)))
		buf = append(buf, n...)
	}
	buf = append(buf, rowBuf...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(valBuf)))
	buf = append(buf, valBuf...)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadBuffsImage reads the image back. The values are slices of the one buffer the file was read
// into, so the whole table is one allocation plus the typed rows.
func LoadBuffsImage(path string) (*BuffTable, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) < 8+4*3 || string(blob[:8]) != string(buffImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a buff image", path)
	}
	at := 8
	word := func() uint32 {
		v := binary.LittleEndian.Uint32(blob[at:])
		at += 4
		return v
	}
	if v := word(); v != buffImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d", path, v, buffImageVersion)
	}
	rows := int(word())
	nNames := int(word())
	names := make([]string, 0, nNames)
	for i := 0; i < nNames; i++ {
		n := int(binary.LittleEndian.Uint16(blob[at:]))
		at += 2
		names = append(names, string(blob[at:at+n]))
		at += n
	}

	// PASS 1: the row table carries (nameIdx, valueLen) pairs only. The value BYTES live in a blob
	// after the rows, so the two cursors are separate - the first version read them interleaved
	// and walked off the end of the file (measured 2026-11-09: the zone hung on load).
	type pending struct {
		id    uint32
		names []string
		lens  []int
	}
	pendingRows := make([]pending, 0, rows)
	for i := 0; i < rows; i++ {
		if at+6 > len(blob) {
			return nil, fmt.Errorf("zone: %s is truncated at row %d", path, i)
		}
		id := binary.LittleEndian.Uint32(blob[at:])
		count := int(binary.LittleEndian.Uint16(blob[at+4:]))
		at += 6
		p := pending{id: id, names: make([]string, 0, count), lens: make([]int, 0, count)}
		if at+count*4 > len(blob) {
			return nil, fmt.Errorf("zone: %s is truncated in row %d", path, i)
		}
		for k := 0; k < count; k++ {
			ni := int(binary.LittleEndian.Uint16(blob[at:]))
			n := int(binary.LittleEndian.Uint16(blob[at+2:]))
			at += 4
			if ni < 0 || ni >= len(names) {
				return nil, fmt.Errorf("zone: %s names attribute %d out of range", path, ni)
			}
			p.names = append(p.names, names[ni])
			p.lens = append(p.lens, n)
		}
		pendingRows = append(pendingRows, p)
	}

	if at+4 > len(blob) {
		return nil, fmt.Errorf("zone: %s is truncated before its value blob", path)
	}
	valLen := int(binary.LittleEndian.Uint32(blob[at:]))
	at += 4
	vals := blobOfLen(blob, at, valLen)
	if vals == nil {
		return nil, fmt.Errorf("zone: %s is truncated in its value blob", path)
	}

	t := &BuffTable{
		byID:   make(map[uint32]BuffRow, rows),
		byName: make(map[string]uint32, rows),
		rows:   make([]BuffRow, 0, rows),
	}
	cursor := 0
	for i := range pendingRows {
		p := &pendingRows[i]
		rowVals := make([]string, 0, len(p.lens))
		for _, n := range p.lens {
			if cursor+n > len(vals) {
				return nil, fmt.Errorf("zone: %s value blob ends early at row %d", path, i)
			}
			rowVals = append(rowVals, string(vals[cursor:cursor+n]))
			cursor += n
		}
		row := BuffRow{ID: p.id, Attrs: &AttrSet{names: p.names, vals: rowVals}}
		row.fillFromAttrs()
		if row.Name == "" {
			return nil, fmt.Errorf("zone: %s row %d has no ClassName", path, i)
		}
		if _, dup := t.byID[row.ID]; dup {
			return nil, fmt.Errorf("zone: %s: ClassID %d appears twice", path, row.ID)
		}
		t.byID[row.ID] = row
		t.rows = append(t.rows, row)
		t.byName[row.Name] = row.ID
	}
	if len(t.byID) == 0 {
		return nil, fmt.Errorf("zone: %s holds no Buff row", path)
	}
	return t, nil
}

// blobOfLen is a bounds-checked slice: a truncated image must not panic a server.
func blobOfLen(blob []byte, at, n int) []byte {
	if at < 0 || n < 0 || at+n > len(blob) {
		return nil
	}
	return blob[at : at+n]
}
