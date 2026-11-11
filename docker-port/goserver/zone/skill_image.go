package zone

// THE SKILL TABLE AS ITS OWN IMAGE (card t_53f1a6d5, 2026-11-10).
//
// WHY. With the start-up table in place, `datatable_skill.xml` is the biggest loader left:
//
//	MEMSTAGE monsters      298 ms
//	MEMSTAGE monster exp   581 ms   (283)
//	MEMSTAGE skills       1031 ms   (445)
//
// The file is **14.5 MB** for 5,626 rows, and encoding/xml decodes each row through the
// struct's attributes; the parse - not the data - is the cost. Same treatment as the
// monster and drop-item tables: magic, version, count, then each row's 31 fields in a fixed
// order. The round-trip test compares every field of every row after a real save/load.
//
// A STALE OR MISSING IMAGE IS NOT AN ERROR: the caller parses the XML and writes a new one.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const skillImageVersion = 1

var skillImageMagic = [8]byte{'G', 'E', 'S', 'K', 'I', 'L', 'L', '1'}

// SkillImagePath is where a zone keeps the skill image: next to the other emitted tables.
func SkillImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "skill_image.bin")
}

// SkillImageStale answers whether the image is older than `datatable_skill.xml`.
func SkillImageStale(imagePath, xmlDir string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	src, err := os.Stat(filepath.Join(xmlDir, skillTableFile))
	if err != nil {
		return false // no source to compare against: trust the image
	}
	return src.ModTime().After(img.ModTime())
}

// SaveSkillImage writes the table to path, atomically.
func SaveSkillImage(path string, t *SkillTable) error {
	if t == nil || len(t.byID) == 0 {
		return fmt.Errorf("zone: no skill rows to write for %s", path)
	}
	ids := make([]uint32, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var buf bytes.Buffer
	buf.Write(skillImageMagic[:])
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], skillImageVersion)
	buf.Write(b4[:])
	binary.LittleEndian.PutUint32(b4[:], uint32(len(ids)))
	buf.Write(b4[:])
	for _, id := range ids {
		encodeSkillRow(&buf, t.byID[id])
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the skill image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the skill image into place: %w", err)
	}
	return nil
}

// LoadSkillImage reads the image and rebuilds the table.
func LoadSkillImage(path string) (*SkillTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a skill image", path, len(data))
	}
	if !bytes.Equal(data[:8], skillImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a skill image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != skillImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, skillImageVersion)
	}
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	r := bytes.NewReader(data[16:])
	t := &SkillTable{byID: make(map[uint32]SkillRow, count)}
	for i := 0; i < count; i++ {
		row, err := decodeSkillRow(r)
		if err != nil {
			return nil, fmt.Errorf("zone: %s row %d: %w", path, i, err)
		}
		t.byID[row.ID] = row
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, r.Len())
	}
	return t, nil
}

// LoadSkillsCached answers the skill table from the image when it is fresh, otherwise parses
// the XML, writes the image for the next start, and answers the table.
func LoadSkillsCached(dir, imagePath string) (table *SkillTable, fromImage bool, err error) {
	if !SkillImageStale(imagePath, dir) {
		if t, ierr := LoadSkillImage(imagePath); ierr == nil {
			return t, true, nil
		}
	}
	table, err = LoadSkills(dir)
	if err != nil {
		return nil, false, err
	}
	_ = SaveSkillImage(imagePath, table) // a failed write is not a failed start
	return table, false, nil
}

// encodeSkillRow writes one row's 31 fields IN ORDER; decodeSkillRow reads the same order.
func encodeSkillRow(buf *bytes.Buffer, row SkillRow) {
	var b4 [4]byte
	var b8 [8]byte
	u32 := func(v uint32) {
		binary.LittleEndian.PutUint32(b4[:], v)
		buf.Write(b4[:])
	}
	i32 := func(v int) {
		binary.LittleEndian.PutUint32(b4[:], uint32(int32(v)))
		buf.Write(b4[:])
	}
	f64 := func(v float64) {
		binary.LittleEndian.PutUint64(b8[:], math.Float64bits(v))
		buf.Write(b8[:])
	}

	u32(row.ID)
	putString(buf, row.Name)
	putString(buf, row.Code)
	f64(row.SklATK)
	f64(row.SklLv)
	f64(row.SklLvBM)
	f64(row.SplDam)
	putString(buf, row.UseType)
	putString(buf, row.ValueType)
	f64(row.MaxR)
	f64(row.MinR)
	f64(row.SplashRange)
	i32(row.SplLimit)
	i32(row.SpendSP)
	i32(row.CastTime)
	i32(row.HoldTime)
	f64(row.IgnoreDEF)
	putString(buf, row.SpecialType)
	putString(buf, row.Script)
	putString(buf, row.Buff)
	putString(buf, row.BuffType)
	f64(row.BuffFreq)
	f64(row.BuffID)
	putString(buf, row.ClassType)
	f64(row.AtkLayer)
	f64(row.HateAmnt)
	putString(buf, row.RemBuff1)
	putString(buf, row.RemBuff2)
	putString(buf, row.RemBuff3)
	putString(buf, row.RemBuff4)
	putString(buf, row.RemBuff5)
}

// decodeSkillRow reads one row written by encodeSkillRow.
func decodeSkillRow(r *bytes.Reader) (SkillRow, error) {
	var row SkillRow
	var b4 [4]byte
	var b8 [8]byte
	getU32 := func() (uint32, error) {
		if _, err := io.ReadFull(r, b4[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(b4[:]), nil
	}
	getI := func() (int, error) {
		v, err := getU32()
		return int(int32(v)), err
	}
	getF := func() (float64, error) {
		if _, err := io.ReadFull(r, b8[:]); err != nil {
			return 0, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b8[:])), nil
	}

	var err error
	if row.ID, err = getU32(); err != nil {
		return row, err
	}
	if row.Name, err = readString(r); err != nil {
		return row, err
	}
	if row.Code, err = readString(r); err != nil {
		return row, err
	}
	for _, p := range []*float64{&row.SklATK, &row.SklLv, &row.SklLvBM, &row.SplDam} {
		if *p, err = getF(); err != nil {
			return row, err
		}
	}
	if row.UseType, err = readString(r); err != nil {
		return row, err
	}
	if row.ValueType, err = readString(r); err != nil {
		return row, err
	}
	for _, p := range []*float64{&row.MaxR, &row.MinR, &row.SplashRange} {
		if *p, err = getF(); err != nil {
			return row, err
		}
	}
	if row.SplLimit, err = getI(); err != nil {
		return row, err
	}
	if row.SpendSP, err = getI(); err != nil {
		return row, err
	}
	if row.CastTime, err = getI(); err != nil {
		return row, err
	}
	if row.HoldTime, err = getI(); err != nil {
		return row, err
	}
	if row.IgnoreDEF, err = getF(); err != nil {
		return row, err
	}
	for _, p := range []*string{&row.SpecialType, &row.Script, &row.Buff, &row.BuffType} {
		if *p, err = readString(r); err != nil {
			return row, err
		}
	}
	if row.BuffFreq, err = getF(); err != nil {
		return row, err
	}
	if row.BuffID, err = getF(); err != nil {
		return row, err
	}
	if row.ClassType, err = readString(r); err != nil {
		return row, err
	}
	if row.AtkLayer, err = getF(); err != nil {
		return row, err
	}
	if row.HateAmnt, err = getF(); err != nil {
		return row, err
	}
	for _, p := range []*string{&row.RemBuff1, &row.RemBuff2, &row.RemBuff3, &row.RemBuff4, &row.RemBuff5} {
		if *p, err = readString(r); err != nil {
			return row, err
		}
	}
	return row, nil
}
