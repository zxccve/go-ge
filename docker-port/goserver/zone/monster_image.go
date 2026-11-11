package zone

// THE MONSTER TABLE AS ITS OWN IMAGE, so every zone stops parsing 7,720 rows x 253
// attributes of XML on every start.
//
// WHY (2026-11-10, card t_96ee949e). A cold map start was 6.7 s and the monster table
// alone was 1.94 s of it - measured with the new MEMSTAGE lines on map 204:
//
//	MEMSTAGE npc types    1381 ms
//	MEMSTAGE monsters     3318 ms   <- +1937 ms for this one table
//	MEMSTAGE monster exp  3624 ms
//
// encoding/xml walks ALL 253 attributes of a row and matches them against the struct's
// 52 fields, for each of 7,720 rows; the parse is the cost, not the data. The table is
// immutable between client patches, so it becomes an image - the same rule the dialog
// text, the script index and the buff table already follow (owner, 2026-11-09: "ONE
// IMAGE PER TABLE").
//
// SHAPE: magic + version + count, then each row's 52 fields in a FIXED order. Strings are
// u32 length + bytes; numbers are little-endian. The order is written once in encodeRow
// and read once in decodeRow, and TestTheMonsterImageRoundTripsTheRealTable compares every
// field of every row after a real save/load, so a drift between the two cannot be silent.
//
// A STALE OR MISSING IMAGE IS NOT AN ERROR: the caller parses the XML and writes a new
// image (a first boot may be slow - the owner's rule). A stale check compares the image's
// mtime with `datatable_monster.xml` AND its `_1.._4` parts, because the table is split.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const monsterImageVersion = 1

var monsterImageMagic = [8]byte{'G', 'E', 'M', 'O', 'N', '0', '0', '1'}

// MonsterImagePath is where a zone keeps the monster image: next to the other emitted
// tables (the same directory as the buff and text images).
func MonsterImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "monster_image.bin")
}

// MonsterImageStale answers whether the image is older than any file the monster table is
// built from. A missing image is stale.
func MonsterImageStale(imagePath, xmlDir string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	for _, name := range monsterSourceFiles(xmlDir) {
		src, err := os.Stat(name)
		if err != nil {
			continue
		}
		if src.ModTime().After(img.ModTime()) {
			return true
		}
	}
	return false
}

// monsterSourceFiles is every file LoadMonsters reads: the base table plus its numeric
// parts (datatable_monster.xml + _1.xml .. _4.xml, see create_tables.go).
func monsterSourceFiles(dir string) []string {
	out := []string{filepath.Join(dir, "datatable_monster.xml")}
	for i := 1; i <= 4; i++ {
		out = append(out, filepath.Join(dir, fmt.Sprintf("datatable_monster_%d.xml", i)))
	}
	return out
}

// SaveMonstersImage writes the table to path, atomically (temp file then rename).
func SaveMonstersImage(path string, t *MonsterTable) error {
	if t == nil || len(t.byID) == 0 {
		return fmt.Errorf("zone: no monster rows to write for %s", path)
	}
	ids := make([]uint32, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var buf bytes.Buffer
	buf.Write(monsterImageMagic[:])
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], monsterImageVersion)
	buf.Write(scratch[:])
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(ids)))
	buf.Write(scratch[:])
	for _, id := range ids {
		row := t.byID[id]
		encodeMonsterRow(&buf, row)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the monster image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the monster image into place: %w", err)
	}
	return nil
}

// LoadMonstersImage reads the image and rebuilds the table (the AI rows are NOT in here -
// they come from their own small XML, see LoadMonsters).
func LoadMonstersImage(path string) (*MonsterTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a monster image",
			path, len(data))
	}
	if !bytes.Equal(data[:8], monsterImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a monster image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != monsterImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, monsterImageVersion)
	}
	count := int(binary.LittleEndian.Uint32(data[12:16]))
	r := bytes.NewReader(data[16:])
	t := &MonsterTable{byID: make(map[uint32]MonsterRow, count)}
	for i := 0; i < count; i++ {
		row, err := decodeMonsterRow(r)
		if err != nil {
			return nil, fmt.Errorf("zone: %s row %d: %w", path, i, err)
		}
		t.byID[row.ID] = row
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s) after %d rows",
			path, r.Len(), count)
	}
	return t, nil
}

// encodeMonsterRow writes one row's 52 fields IN ORDER. decodeMonsterRow reads the same
// order; the round-trip test is what keeps the two honest.
func encodeMonsterRow(buf *bytes.Buffer, r MonsterRow) {
	var b4 [4]byte
	var b8 [8]byte
	putU32 := func(v uint32) {
		binary.LittleEndian.PutUint32(b4[:], v)
		buf.Write(b4[:])
	}
	putStr := func(s string) {
		putU32(uint32(len(s)))
		buf.WriteString(s)
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

	putU32(r.ID)
	putStr(r.Name)
	putU32(uint32(int32(r.Level)))
	putF(r.Exp)
	putStr(r.Tactics)
	binary.LittleEndian.PutUint64(b8[:], uint64(r.MHP))
	buf.Write(b8[:])
	putStr(r.Size)
	putF(r.Radius)
	putStr(r.RaceType)
	putF(r.Arm)
	putF(r.DEFBM)
	putF(r.DEFPERBM)
	putF(r.IMPBM)
	putStr(r.SizeType)
	putF(r.MobRank)
	putF(r.DRBM)
	putF(r.DefLayer)
	putF(r.DefLayerBM)
	putF(r.RSTAT)
	putF(r.RSTATBM)
	putF(r.CON)
	putF(r.MHPFix)
	putF(r.MHPBM)
	putF(r.ADDMHPBM)
	putF(r.Wpn)
	putF(r.STR)
	putF(r.ATKBM)
	putF(r.MinR)
	putF(r.MaxR)
	putF(r.ASPD)
	putF(r.ASPDBM)
	putF(r.Hits)
	putF(r.MSPD)
	putF(r.MSPDFix)
	putF(r.MSPDBM)
	putF(r.SPDBM)
	putStr(r.AIType)
	putBool(r.AIKnown)
	putBool(r.AIAttack)
	putF(r.SeekRange)
	binary.LittleEndian.PutUint64(b8[:], uint64(int64(r.DyingTime)))
	buf.Write(b8[:])
	putBool(r.ShapeKnown)
	putF(r.DropOldEqpFreq)
	putF(r.DropNormEqpFreq)
	putF(r.DropEliteEqpFreq)
	putF(r.DropAccFreq)
	putF(r.DropEtcFreq)
	putF(r.DropInstFreq)
	putF(r.DropRecipeFreq)
	putF(r.DropCrystalFreq)
	putU32(uint32(int32(r.DropCrystalCount)))
	for _, tr := range r.Trophies {
		putU32(tr.ID)
		putF(tr.Per)
		putU32(uint32(int32(tr.Quant)))
	}
}

// decodeMonsterRow reads one row written by encodeMonsterRow.
func decodeMonsterRow(r *bytes.Reader) (MonsterRow, error) {
	var row MonsterRow
	var b4 [4]byte
	var b8 [8]byte
	getU32 := func() (uint32, error) {
		if _, err := io.ReadFull(r, b4[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(b4[:]), nil
	}
	getStr := func() (string, error) {
		n, err := getU32()
		if err != nil {
			return "", err
		}
		s := make([]byte, n)
		if _, err := io.ReadFull(r, s); err != nil {
			return "", err
		}
		return string(s), nil
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
	if row.Name, err = getStr(); err != nil {
		return row, err
	}
	lv, err := getU32()
	if err != nil {
		return row, err
	}
	row.Level = int(int32(lv))
	if row.Exp, err = getF(); err != nil {
		return row, err
	}
	if row.Tactics, err = getStr(); err != nil {
		return row, err
	}
	if _, err := io.ReadFull(r, b8[:]); err != nil {
		return row, err
	}
	row.MHP = int64(binary.LittleEndian.Uint64(b8[:]))
	if row.Size, err = getStr(); err != nil {
		return row, err
	}
	if row.Radius, err = getF(); err != nil {
		return row, err
	}
	if row.RaceType, err = getStr(); err != nil {
		return row, err
	}
	floats := []*float64{
		&row.Arm, &row.DEFBM, &row.DEFPERBM, &row.IMPBM,
	}
	for _, p := range floats {
		if *p, err = getF(); err != nil {
			return row, err
		}
	}
	if row.SizeType, err = getStr(); err != nil {
		return row, err
	}
	rest := []*float64{
		&row.MobRank, &row.DRBM, &row.DefLayer, &row.DefLayerBM,
		&row.RSTAT, &row.RSTATBM, &row.CON, &row.MHPFix, &row.MHPBM,
		&row.ADDMHPBM, &row.Wpn, &row.STR, &row.ATKBM, &row.MinR, &row.MaxR,
		&row.ASPD, &row.ASPDBM, &row.Hits, &row.MSPD, &row.MSPDFix,
		&row.MSPDBM, &row.SPDBM,
	}
	for _, p := range rest {
		if *p, err = getF(); err != nil {
			return row, err
		}
	}
	if row.AIType, err = getStr(); err != nil {
		return row, err
	}
	if row.AIKnown, err = getBool(); err != nil {
		return row, err
	}
	if row.AIAttack, err = getBool(); err != nil {
		return row, err
	}
	if row.SeekRange, err = getF(); err != nil {
		return row, err
	}
	if _, err := io.ReadFull(r, b8[:]); err != nil {
		return row, err
	}
	row.DyingTime = time.Duration(int64(binary.LittleEndian.Uint64(b8[:])))
	if row.ShapeKnown, err = getBool(); err != nil {
		return row, err
	}
	drops := []*float64{
		&row.DropOldEqpFreq, &row.DropNormEqpFreq, &row.DropEliteEqpFreq,
		&row.DropAccFreq, &row.DropEtcFreq, &row.DropInstFreq,
		&row.DropRecipeFreq, &row.DropCrystalFreq,
	}
	for _, p := range drops {
		if *p, err = getF(); err != nil {
			return row, err
		}
	}
	cc, err := getU32()
	if err != nil {
		return row, err
	}
	row.DropCrystalCount = int(int32(cc))
	for i := range row.Trophies {
		if row.Trophies[i].ID, err = getU32(); err != nil {
			return row, err
		}
		if row.Trophies[i].Per, err = getF(); err != nil {
			return row, err
		}
		q, err := getU32()
		if err != nil {
			return row, err
		}
		row.Trophies[i].Quant = int(int32(q))
	}
	return row, nil
}

// LoadMonstersCached answers the monster table from the image when it is fresh, and
// otherwise parses the XML, writes the image for the next start, and answers the table.
// `fromImage` is what the caller logs: a start that read the image did no XML parse.
//
// THE AI ROWS ARE NOT IN THE IMAGE: `datatable_monsterai.xml` is a few dozen rows and is
// re-read either way, so a stale image can never hide a changed AI profile.
func LoadMonstersCached(dir, imagePath string) (table *MonsterTable, fromImage bool, err error) {
	if !MonsterImageStale(imagePath, dir) {
		if t, ierr := LoadMonstersImage(imagePath); ierr == nil {
			t.aiRows = loadMonsterAIRows(dir)
			return t, true, nil
		}
	}
	t, err := LoadMonsters(dir)
	if err != nil {
		return nil, false, err
	}
	// A failed WRITE is not a failed start: the table in hand is correct, and the next
	// start simply pays the parse again.
	_ = SaveMonstersImage(imagePath, t)
	return t, false, nil
}
