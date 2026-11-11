package zone

// THE STANCE TABLES AS ONE IMAGE (card t_88e45fd8, 2026-11-11).
//
// WHY. `LoadStances` was the biggest single table left on a map start:
//
//	MEMSTAGE monsters        283 ms
//	MEMSTAGE monster exp     562 ms   (+279 - LoadStances; the exp table is a 1.8 KB
//	                                   embedded CSV, and the two lines used to print the
//	                                   same number because the exp stage came AFTER it)
//
// MEASURED per file over the shipped data (eachClass with an empty callback, so nothing but
// the parse):
//
//	datatable_item_weapon.xml   3431 rows in 219 ms   <- 12 MB for 3,431 rows
//	datatable_stance.xml         661 rows in  26 ms
//	datatable_job.xml            413 rows in  17 ms
//	datatable_stancecondition.xml 1117 rows in  6 ms
//	datatable_ai.xml               5 rows in   0 ms
//
// encoding/xml builds an attribute map per row and that is the cost; the data never changes
// between client patches. Same treatment as the item, map, monster and skill tables.
//
// NINE STRUCTURES PLUS FOUR ROW MAPS, and this is the only image whose VALUES ARE SLICES.
// THE ORDER OF EVERY LIST IS DATA: `JobStances` walks a job's conditions in table order,
// `PickStance` walks a condition's stances in table order and takes the FIRST whose motion
// resolves, and `JobSkills` walks the stance's own SkillID1..5 - a set or a sorted rebuild
// would silently change which stance and which skills a character gets. The image therefore
// stores every list element by element, and the round-trip test compares every map's every
// list with reflect.DeepEqual (the item image's `byRule` precedent).
//
// rawStance/rawJob are the rows' OWN attributes, kept whole for the game's status script
// (`GetCurrentStance`, `GetDataTableNumberValue('Job', ...)`). They are stored as name/value
// pairs sorted by name: a map has no order to lose, and sorting keeps the image byte-stable.
//
// A STALE, MISSING OR CORRUPT IMAGE IS NOT AN ERROR: the caller parses the XML and writes a
// new image. Staleness is checked against EVERY file the loader reads, not only
// datatable_stance.xml - a job, weapon, condition or AI row that changed is a table that
// changed.

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

const stanceImageVersion = 1

var stanceImageMagic = [8]byte{'G', 'E', 'S', 'T', 'A', 'N', 'C', '1'}

// stanceSourceFiles are the files LoadStances reads. datatable_ai.xml is optional there
// (a missing one is not an error), and here a missing one is simply not compared - but if it
// APPEARS it is newer than an older image and forces the rebuild.
var stanceSourceFiles = []string{
	"datatable_stance.xml",
	"datatable_job.xml",
	"datatable_stancecondition.xml",
	"datatable_item_weapon.xml",
	"datatable_ai.xml",
}

// StanceImagePath is where a zone keeps the stance image: next to the other emitted tables.
func StanceImagePath(textImagePath string) string {
	return filepath.Join(filepath.Dir(textImagePath), "stance_image.bin")
}

// StanceImageStale answers whether the image is older than any of the files LoadStances
// reads. A missing image is stale; no source at all means the image is trusted (the same
// rule the item and map images follow).
func StanceImageStale(imagePath, xmlDir string) bool {
	img, err := os.Stat(imagePath)
	if err != nil {
		return true
	}
	seen := 0
	for _, name := range stanceSourceFiles {
		src, err := os.Stat(filepath.Join(xmlDir, name))
		if err != nil {
			continue
		}
		seen++
		if src.ModTime().After(img.ModTime()) {
			return true
		}
	}
	return seen == 0
}

// --- the writer / reader this image uses ------------------------------------------------
//
// One method per primitive, so a section is written and read in the SAME order and a field
// cannot quietly move: the round-trip test compares all thirteen maps, not a sample.

type stanceImageWriter struct {
	b    bytes.Buffer      // the body, in section order
	strs []string          // every distinct string, in first-seen order
	idx  map[string]uint32 // and where each one sits
}

func (w *stanceImageWriter) u32(v uint32) {
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], v)
	w.b.Write(b4[:])
}

func (w *stanceImageWriter) i32(v int) { w.u32(uint32(int32(v))) }

func (w *stanceImageWriter) f64(v float64) {
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], math.Float64bits(v))
	w.b.Write(b8[:])
}

// str writes an INDEX into the image's string table, not the bytes. The tables are full of
// repeats ("Rapier" on hundreds of weapon rows, "ClassID"/"ClassName" on every raw row), so
// the table is what keeps a 3 MB image down AND keeps the read to one allocation per distinct
// string instead of one per use - the difference between ~16 ms and single-digit ones.
func (w *stanceImageWriter) str(s string) {
	i, ok := w.idx[s]
	if !ok {
		i = uint32(len(w.strs))
		w.strs = append(w.strs, s)
		w.idx[s] = i
	}
	w.u32(i)
}

func (w *stanceImageWriter) boolean(v bool) {
	if v {
		w.b.WriteByte(1)
		return
	}
	w.b.WriteByte(0)
}

func (w *stanceImageWriter) ints(list []int) {
	w.u32(uint32(len(list)))
	for _, v := range list {
		w.i32(v)
	}
}

func (w *stanceImageWriter) attrs(row map[string]string) {
	names := make([]string, 0, len(row))
	for name := range row {
		names = append(names, name)
	}
	sort.Strings(names)
	w.u32(uint32(len(names)))
	for _, name := range names {
		w.str(name)
		w.str(row[name])
	}
}

// stanceImageReader walks the image's bytes by INDEX rather than through a bytes.Reader:
// there are ~100,000 primitives in this image (one per attribute name and value) and
// io.ReadFull per primitive was measured at ~10 ms of the read on its own.
type stanceImageReader struct {
	data []byte
	pos  int
	strs []string
}

func (r *stanceImageReader) u32() (uint32, error) {
	if r.pos+4 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *stanceImageReader) i32() (int, error) {
	v, err := r.u32()
	return int(int32(v)), err
}

func (r *stanceImageReader) f64() (float64, error) {
	if r.pos+8 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(r.data[r.pos : r.pos+8]))
	r.pos += 8
	return v, nil
}

// str answers the string the image's table holds at this index: NO allocation, and the same
// string value the XML loader would have built.
func (r *stanceImageReader) str() (string, error) {
	i, err := r.u32()
	if err != nil {
		return "", err
	}
	if int(i) >= len(r.strs) {
		return "", fmt.Errorf("string index %d of %d", i, len(r.strs))
	}
	return r.strs[i], nil
}

func (r *stanceImageReader) boolean() (bool, error) {
	if r.pos >= len(r.data) {
		return false, io.ErrUnexpectedEOF
	}
	b := r.data[r.pos]
	r.pos++
	return b != 0, nil
}

// ints answers nil for a zero-length list, which is what the XML loader builds: an empty
// (non-nil) slice would make reflect.DeepEqual say the two tables differ.
func (r *stanceImageReader) ints() ([]int, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	list := make([]int, n)
	for i := range list {
		if list[i], err = r.i32(); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (r *stanceImageReader) attrs() (map[string]string, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	row := make(map[string]string, n)
	for i := uint32(0); i < n; i++ {
		name, err := r.str()
		if err != nil {
			return nil, err
		}
		value, err := r.str()
		if err != nil {
			return nil, err
		}
		row[name] = value
	}
	return row, nil
}

// sortedIntKeys / sortedStringKeys give every section a stable order, so the same table
// always writes the same bytes.
func sortedIntKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// SaveStanceImage writes the table to path, atomically.
func SaveStanceImage(path string, t *StanceTable) error {
	if t == nil || len(t.stanceSkills) == 0 || len(t.rows) == 0 || len(t.conds) == 0 {
		return fmt.Errorf("zone: no stance rows to write for %s", path)
	}
	var w stanceImageWriter
	w.idx = map[string]uint32{}

	// 1. stance id -> the skills it grants, in the row's own order.
	w.u32(uint32(len(t.stanceSkills)))
	for _, id := range sortedIntKeys(t.stanceSkills) {
		w.i32(id)
		w.ints(t.stanceSkills[id])
	}

	// 2. stance id -> its name, class type and speeds.
	w.u32(uint32(len(t.rows)))
	for _, id := range sortedIntKeys(t.rows) {
		row := t.rows[id]
		w.i32(id)
		w.str(row.Name)
		w.str(row.ClassType)
		w.f64(row.RSPD)
		w.f64(row.WSPD)
		w.f64(row.StnMSPD)
		w.str(row.ASTD)
		w.str(row.AI)
	}

	// 3. stance id -> the status-script combat columns.
	w.u32(uint32(len(t.combat)))
	for _, id := range sortedIntKeys(t.combat) {
		row := t.combat[id]
		w.i32(id)
		w.str(row.AtkType)
		w.str(row.DefType)
		w.str(row.ClassType)
		w.f64(row.RHits)
		w.f64(row.LHits)
		w.f64(row.StnATK)
		w.f64(row.StnHR)
		w.f64(row.IncHR)
		w.f64(row.StnLv)
		w.f64(row.ASPD)
		w.f64(row.IncASPD)
		w.f64(row.StnRFIRE)
		w.f64(row.StnRICE)
		w.f64(row.StnRLGHT)
		w.f64(row.StnRSTAT)
		w.f64(row.MinR)
		w.f64(row.MaxR)
		w.f64(row.SplRange)
		w.str(row.SplType)
	}

	// 4. job id -> its stance-condition ids, in the job row's order.
	w.u32(uint32(len(t.jobs)))
	for _, id := range sortedIntKeys(t.jobs) {
		w.i32(id)
		w.ints(t.jobs[id])
	}

	// 5. condition id -> its stance ids, in the condition row's order.
	w.u32(uint32(len(t.conds)))
	for _, id := range sortedIntKeys(t.conds) {
		w.i32(id)
		w.ints(t.conds[id])
	}

	// 6. condition id -> the two weapon types it needs.
	w.u32(uint32(len(t.hands)))
	for _, id := range sortedIntKeys(t.hands) {
		w.i32(id)
		w.str(t.hands[id][0])
		w.str(t.hands[id][1])
	}

	// 7. job id -> the name of its standing motion.
	w.u32(uint32(len(t.jobSTD)))
	for _, id := range sortedIntKeys(t.jobSTD) {
		w.i32(id)
		w.str(t.jobSTD[id])
	}

	// 8. job id -> the job's own skill.
	w.u32(uint32(len(t.jobSkill)))
	for _, id := range sortedIntKeys(t.jobSkill) {
		w.i32(id)
		w.i32(t.jobSkill[id])
	}

	// 9. item class id -> its weapon type.
	w.u32(uint32(len(t.weaponTypes)))
	for _, id := range sortedIntKeys(t.weaponTypes) {
		w.i32(id)
		w.str(t.weaponTypes[id])
	}

	// 10. item class id -> held in both hands.
	w.u32(uint32(len(t.twoHanded)))
	for _, id := range sortedIntKeys(t.twoHanded) {
		w.i32(id)
		w.boolean(t.twoHanded[id])
	}

	// 11. AI profile name -> its ClassID.
	aiNames := make([]string, 0, len(t.aiIDs))
	for name := range t.aiIDs {
		aiNames = append(aiNames, name)
	}
	sort.Strings(aiNames)
	w.u32(uint32(len(aiNames)))
	for _, name := range aiNames {
		w.str(name)
		w.i32(t.aiIDs[name])
	}

	// 12/13. the rows' OWN attributes, for the game's status script.
	w.u32(uint32(len(t.rawStance)))
	for _, id := range sortedIntKeys(t.rawStance) {
		w.i32(id)
		w.attrs(t.rawStance[id])
	}
	w.u32(uint32(len(t.rawJob)))
	for _, id := range sortedIntKeys(t.rawJob) {
		w.i32(id)
		w.attrs(t.rawJob[id])
	}

	tmp := path + ".tmp"
	// THE FILE IS magic, version, the STRING TABLE, then the body: the body refers to the
	// table by index, so it is written second even though it is built first.
	var out bytes.Buffer
	out.Write(stanceImageMagic[:])
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], stanceImageVersion)
	out.Write(b4[:])
	binary.LittleEndian.PutUint32(b4[:], uint32(len(w.strs)))
	out.Write(b4[:])
	for _, s := range w.strs {
		putString(&out, s)
	}
	out.Write(w.b.Bytes())
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("zone: write the stance image %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("zone: move the stance image into place: %w", err)
	}
	return nil
}

// LoadStanceImage reads the image and rebuilds the table.
func LoadStanceImage(path string) (*StanceTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("zone: %s is %d bytes, too short for a stance image", path, len(data))
	}
	if !bytes.Equal(data[:8], stanceImageMagic[:]) {
		return nil, fmt.Errorf("zone: %s is not a stance image (magic %q)", path, data[:8])
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != stanceImageVersion {
		return nil, fmt.Errorf("zone: %s is version %d, this build reads %d",
			path, v, stanceImageVersion)
	}
	// The string table, then the body.
	pos := 12
	if pos+4 > len(data) {
		return nil, fmt.Errorf("zone: %s ends before its string table", path)
	}
	nStrs := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4
	strs := make([]string, nStrs)
	for i := range strs {
		if pos+4 > len(data) {
			return nil, fmt.Errorf("zone: %s string %d: %w", path, i, io.ErrUnexpectedEOF)
		}
		n := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if n < 0 || pos+n > len(data) {
			return nil, fmt.Errorf("zone: %s string %d is %d bytes past the end", path, i, n)
		}
		strs[i] = string(data[pos : pos+n])
		pos += n
	}
	r := &stanceImageReader{data: data[pos:], strs: strs}

	t := &StanceTable{
		stanceSkills: map[int][]int{},
		rows:         map[int]StanceRow{},
		combat:       map[int]StanceCombat{},
		jobs:         map[int][]int{},
		conds:        map[int][]int{},
		hands:        map[int][2]string{},
		jobSTD:       map[int]string{},
		jobSkill:     map[int]int{},
		weaponTypes:  map[int]string{},
		twoHanded:    map[int]bool{},
		aiIDs:        map[string]int{},
		rawStance:    map[int]map[string]string{},
		rawJob:       map[int]map[string]string{},
	}

	// 1.
	n, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("zone: %s stance-skill count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s stance-skill %d id: %w", path, i, err)
		}
		if t.stanceSkills[id], err = r.ints(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d skills: %w", path, id, err)
		}
	}

	// 2.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s stance-row count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s stance-row %d id: %w", path, i, err)
		}
		var row StanceRow
		if row.Name, err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d name: %w", path, id, err)
		}
		if row.ClassType, err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d class type: %w", path, id, err)
		}
		if row.RSPD, err = r.f64(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d RSPD: %w", path, id, err)
		}
		if row.WSPD, err = r.f64(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d WSPD: %w", path, id, err)
		}
		if row.StnMSPD, err = r.f64(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d StnMSPD: %w", path, id, err)
		}
		if row.ASTD, err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d ASTD: %w", path, id, err)
		}
		if row.AI, err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d AI: %w", path, id, err)
		}
		t.rows[id] = row
	}

	// 3.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s combat count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s combat %d id: %w", path, i, err)
		}
		var row StanceCombat
		for _, p := range []*string{&row.AtkType, &row.DefType, &row.ClassType} {
			if *p, err = r.str(); err != nil {
				return nil, fmt.Errorf("zone: %s stance %d combat string: %w", path, id, err)
			}
		}
		for _, p := range []*float64{&row.RHits, &row.LHits, &row.StnATK, &row.StnHR,
			&row.IncHR, &row.StnLv, &row.ASPD, &row.IncASPD, &row.StnRFIRE, &row.StnRICE,
			&row.StnRLGHT, &row.StnRSTAT, &row.MinR, &row.MaxR, &row.SplRange} {
			if *p, err = r.f64(); err != nil {
				return nil, fmt.Errorf("zone: %s stance %d combat number: %w", path, id, err)
			}
		}
		if row.SplType, err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s stance %d SplType: %w", path, id, err)
		}
		t.combat[id] = row
	}

	// 4.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s job count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s job %d id: %w", path, i, err)
		}
		if t.jobs[id], err = r.ints(); err != nil {
			return nil, fmt.Errorf("zone: %s job %d conditions: %w", path, id, err)
		}
	}

	// 5.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s condition count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s condition %d id: %w", path, i, err)
		}
		if t.conds[id], err = r.ints(); err != nil {
			return nil, fmt.Errorf("zone: %s condition %d stances: %w", path, id, err)
		}
	}

	// 6.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s hands count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s hands %d id: %w", path, i, err)
		}
		var h [2]string
		for j := range h {
			if h[j], err = r.str(); err != nil {
				return nil, fmt.Errorf("zone: %s condition %d hand %d: %w", path, id, j, err)
			}
		}
		t.hands[id] = h
	}

	// 7.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s job-STD count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s job STD %d id: %w", path, i, err)
		}
		if t.jobSTD[id], err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s job %d STD: %w", path, id, err)
		}
	}

	// 8.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s job-skill count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s job skill %d id: %w", path, i, err)
		}
		if t.jobSkill[id], err = r.i32(); err != nil {
			return nil, fmt.Errorf("zone: %s job %d skill: %w", path, id, err)
		}
	}

	// 9.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s weapon-type count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s weapon type %d id: %w", path, i, err)
		}
		if t.weaponTypes[id], err = r.str(); err != nil {
			return nil, fmt.Errorf("zone: %s weapon %d type: %w", path, id, err)
		}
	}

	// 10.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s two-handed count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s two-handed %d id: %w", path, i, err)
		}
		if t.twoHanded[id], err = r.boolean(); err != nil {
			return nil, fmt.Errorf("zone: %s weapon %d two-handed: %w", path, id, err)
		}
	}

	// 11.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s AI count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		name, err := r.str()
		if err != nil {
			return nil, fmt.Errorf("zone: %s AI %d name: %w", path, i, err)
		}
		if t.aiIDs[name], err = r.i32(); err != nil {
			return nil, fmt.Errorf("zone: %s AI %q id: %w", path, name, err)
		}
	}

	// 12/13.
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s raw-stance count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s raw stance %d id: %w", path, i, err)
		}
		if t.rawStance[id], err = r.attrs(); err != nil {
			return nil, fmt.Errorf("zone: %s raw stance %d row: %w", path, id, err)
		}
	}
	if n, err = r.u32(); err != nil {
		return nil, fmt.Errorf("zone: %s raw-job count: %w", path, err)
	}
	for i := uint32(0); i < n; i++ {
		id, err := r.i32()
		if err != nil {
			return nil, fmt.Errorf("zone: %s raw job %d id: %w", path, i, err)
		}
		if t.rawJob[id], err = r.attrs(); err != nil {
			return nil, fmt.Errorf("zone: %s raw job %d row: %w", path, id, err)
		}
	}

	if r.pos != len(r.data) {
		return nil, fmt.Errorf("zone: %s has %d trailing byte(s)", path, len(r.data)-r.pos)
	}
	return t, nil
}

// LoadStancesCached answers the stance tables from the image when it is fresh, otherwise
// parses the XML, writes the image for the next start, and answers the table.
func LoadStancesCached(dir, imagePath string) (table *StanceTable, fromImage bool, err error) {
	if !StanceImageStale(imagePath, dir) {
		if t, ierr := LoadStanceImage(imagePath); ierr == nil {
			return t, true, nil
		}
	}
	table, err = LoadStances(dir)
	if err != nil {
		return nil, false, err
	}
	_ = SaveStanceImage(imagePath, table) // a failed write is not a failed start
	return table, false, nil
}

// StancesForZone is the production reader main.go calls: the image when it is fresh, the XML
// otherwise, and ONE line when the image answered so a start is auditable.
//
// It exists beside LoadStances (which stays the XML parser every test wants) so the choice
// between the two is made in ONE place, next to the image's own staleness rule.
func StancesForZone(dir, imagePath string) (*StanceTable, error) {
	t, fromImage, err := LoadStancesCached(dir, imagePath)
	if err != nil {
		return nil, err
	}
	if fromImage {
		log.Printf("zone: stance tables from the IMAGE %s - %s, no XML parse",
			imagePath, t.Describe())
	}
	return t, nil
}
