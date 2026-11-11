// THE BUFF TABLE - `ge\xml\datatable_buff.xml`, 1,962 rows.
//
// WHY IT IS A FILE OF ITS OWN: a buff is a ROW first and a script second. Every
// name the corpus applies (`AddBuff(self,'Revenge',...)`) is looked up here, and
// the row is what says whether the buff expires, in which group it competes, and
// what its five BuffValue columns are. Nothing has to be decoded: the client's XML
// already carries the column names this server reads (`ClassID`, `ClassName`,
// `ApplyTime`, `UpdateType`, `BuffRank`, `GroupIndex`, `ExGroupIndex`,
// `HoldMove/Attack/Skill/Item`, the four `Ignore*`, `Range`, `Save`, `Category`,
// `BuffValue1..5`), so the loader reads attributes rather than a packed record.
//
// THE ONE FACT THAT SHAPES THE WHOLE SLICE: the duration is NOT in this table.
// `ApplyTime="0"` on 1,954 of the 1,962 rows; the buff's own ENTER script writes
// it (`buff.ApplyTime = 30000`, ge\script\buff\buff.scp:24; `= skilllv * 20000`,
// :44). The 8 rows that DO carry a non-zero ApplyTime all have an ENTER script as
// well (Aroma 817, P_BloodMark 825, E_BloodMark 826, Fear2 827, Haste3 828,
// Coma 887, MagicPlague 955, OldShock 1107), so the table value is never the
// source of truth - it is the SEED the ENTER script may overwrite, which is
// exactly what `CBuff::OnEnter` does (Buff.cpp:66-99: run the script, then
// `m_buff.time = GetApplyTime()`).
//
// `UpdateType` values in the shipped table: NORMAL 1,093 / SCRIPT 406 /
// INFINITY 463. The three strings are the reference's own `StrID::NORMAL`,
// `StrID::SCRIPT`, `StrID::INFINITY` (used at Buff.cpp:155-186).
package zone

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The three `UpdateType` words the table uses, spelled exactly as the XML spells
// them. They are the reference's own enumeration, not names invented here.
const (
	BuffUpdateNormal   = "NORMAL"
	BuffUpdateScript   = "SCRIPT"
	BuffUpdateInfinity = "INFINITY"
)

// BuffEnterPrefix is the entry-point prefix a buff row's script carries. The
// reference stores the whole name in its table (`enterFuncName`,
// geBuffTable.h:11-25); the client's XML does not, so the name is the ClassName
// under this prefix - the same rule `SCR_BUFF_ENTER_Revenge` follows in the corpus.
const BuffEnterPrefix = "SCR_BUFF_ENTER_"

// BuffTableFile is the client file this table lives in, under the XML directory.
const BuffTableFile = "datatable_buff.xml"

// BuffRow is one row of the client's Buff idspace, with the columns a buff list
// acts on typed and EVERY attribute kept raw.
type BuffRow struct {
	ID   uint32
	Name string

	// ApplyTime is the row's own duration in MILLISECONDS. It is 0 on 1,954 of the
	// 1,962 rows and the ENTER script is what writes the real number - see the file
	// header. It is read here so a row that DOES carry one is not lost.
	ApplyTime float64

	// UpdateType is NORMAL, SCRIPT or INFINITY. NORMAL expires when the elapsed
	// time passes ApplyTime, INFINITY never expires, and SCRIPT expires when its
	// own UPDATE script says so (Buff.cpp:155-186).
	UpdateType string

	// BuffRank is the row's rank inside its group. A LOWER rank is refused while a
	// HIGHER rank of an overlapping group is applied (`CBuffList::ExistHigherRank`,
	// BuffList.cpp:62-99).
	BuffRank int

	// GroupIndex / ExGroupIndex are the group(s) the rank rule compares. The shipped
	// rows name one value each ("None" when unset), which is the single-group case of
	// the reference's GROUPINDEX_LIST.
	GroupIndex   string
	ExGroupIndex string

	HoldMove   bool
	HoldAttack bool
	HoldSkill  bool
	HoldItem   bool

	IgnoreMagicalSkill  bool
	IgnoreNormalHit     bool
	IgnorePhysicalSkill bool
	IgnoreTargetting    bool
	// Invisible is the row's `Invisible` column: it is not a movement hold, but it is
	// the one other flag the buff scripts ask about by name, so it is carried.
	Invisible bool

	// Range is the row's radius: a non-zero value makes the reference walk the
	// nearby objects and fire the buff's EVENT entry on enter and leave
	// (Buff.cpp:145-156, :212). This slice does not run events and does not report
	// the range, but the number is read so a later card does not have to re-open the
	// loader.
	Range float64

	// Save is the row's `Save` flag: TRUE means the reference WRITES the buff to the
	// database (`OM_SAVE_BUFF_UPDATE`, Buff.cpp:96-98). This slice does not persist
	// buffs, and the flag is carried so the omission is a named one - 388 rows say
	// TRUE.
	Save bool

	// Category is "buff", "debuff" or "cash".
	Category string

	// BuffValue1..5 are the row's five free values, in column order.
	BuffValue [5]float64

	// Attrs is EVERY attribute the row carries, by its XML name, exactly as the file
	// spells it. It is what the ENTER script's `buff` table is seeded from: the
	// reference hands the script its own IES object (`Buff.cpp:71`), and the IES
	// object's properties are these columns. Keeping them all means a script that
	// reads a column this struct did not name still gets the real value instead of 0.
	Attrs *AttrSet
}

// AttrSet is a row's attributes as a VIEW rather than a map: the names are shared by every row of
// the table (interned once) and the values are slices of the row's own backing store. A row of 54
// attributes used to cost a Go map with 54 string keys - 106,000 of them for the shipped table,
// 26 MB in every zone (measured 2026-11-09). The same 54 pairs as two slices cost a fraction.
type AttrSet struct {
	names []string
	vals  []string
}

// Get answers one attribute by its XML name, or "" when the row does not carry it.
func (a *AttrSet) Get(name string) string {
	if a == nil {
		return ""
	}
	for i := range a.names {
		if a.names[i] == name {
			return a.vals[i]
		}
	}
	return ""
}

// Len answers how many attributes the row carries.
func (a *AttrSet) Len() int {
	if a == nil {
		return 0
	}
	return len(a.names)
}

// Each visits every attribute in the file's own order.
func (a *AttrSet) Each(fn func(name, value string)) {
	if a == nil {
		return
	}
	for i := range a.names {
		fn(a.names[i], a.vals[i])
	}
}

// BuffTable is the whole Buff idspace: by name (the key every script uses) and by
// class id (the mask bit index).
type BuffTable struct {
	byID   map[uint32]BuffRow
	byName map[string]uint32
	rows   []BuffRow // file order, for the image writer
}

// Len answers how many rows the table holds. The shipped file has 1,962.
func (t *BuffTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byID)
}

// Row answers one buff by its ClassName, and false when the table has no such
// name. A name the table does not carry is REFUSED by the caller - it is never
// turned into a zero row, because a zero row means "0 ms, group None, rank 0",
// which is a buff nobody wrote.
func (t *BuffTable) Row(name string) (BuffRow, bool) {
	if t == nil {
		return BuffRow{}, false
	}
	id, ok := t.byName[name]
	if !ok {
		return BuffRow{}, false
	}
	r, ok := t.byID[id]
	return r, ok
}

// RowByID answers one buff by its ClassID.
func (t *BuffTable) RowByID(id uint32) (BuffRow, bool) {
	if t == nil {
		return BuffRow{}, false
	}
	r, ok := t.byID[id]
	return r, ok
}

// EnterEntry is the script entry point this row's ENTER half is called by:
// `SCR_BUFF_ENTER_<ClassName>`. It is the name the corpus defines for 1,416 of the
// 1,962 rows (measured against `ge\script\**`); the other 546 have no ENTER at all
// and the caller must treat them as scriptless.
func (r BuffRow) EnterEntry() string { return BuffEnterPrefix + r.Name }

// Lua is the `buff` table the ENTER script receives: every raw attribute, with a
// value that parses as a number handed over as a NUMBER and everything else as a
// string.
//
// THE TYPING IS NOT COSMETIC. The corpus does `buff.ApplyTime = skilllv * 20000`
// and then compares `buff.ApplyTime < elapsedTime`; a string "0" there is a Lua
// error ("attempt to compare string with number"), not a 0. And the columns that
// are words ("NO", "YES", "None", "debuff") must stay strings, because scripts
// compare them by name. A numeric column is a number in the IES layer too, so the
// parse is the same typing the reference's object has.
func (r BuffRow) Lua() map[string]any {
	m := make(map[string]any, r.Attrs.Len()+1)
	r.Attrs.Each(func(k, v string) {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			m[k] = f
			return
		}
		m[k] = v
	})
	// ClassID/ClassName are not attributes a script should have to guess.
	m["ClassID"] = float64(r.ID)
	m["ClassName"] = r.Name
	return m
}

// fillFromAttrs fills the typed columns from the row's own attributes. It is the ONLY place that
// decides what a column means, so the XML path and the image path cannot drift apart.
func (r *BuffRow) fillFromAttrs() {
	a := r.Attrs
	r.Name = a.Get("ClassName")
	r.ApplyTime = atofOr(a.Get("ApplyTime"), 0)
	r.UpdateType = strings.ToUpper(a.Get("UpdateType"))
	r.BuffRank = int(atofOr(a.Get("BuffRank"), 0))
	r.GroupIndex = a.Get("GroupIndex")
	r.ExGroupIndex = a.Get("ExGroupIndex")
	r.HoldMove = yesNo(a.Get("HoldMove"))
	r.HoldAttack = yesNo(a.Get("HoldAttack"))
	r.HoldSkill = yesNo(a.Get("HoldSkill"))
	r.HoldItem = yesNo(a.Get("HoldItem"))
	r.IgnoreMagicalSkill = yesNo(a.Get("IgnoreMagicalSkill"))
	r.IgnoreNormalHit = yesNo(a.Get("IgnoreNormalHit"))
	r.IgnorePhysicalSkill = yesNo(a.Get("IgnorePhysicalSkill"))
	r.IgnoreTargetting = yesNo(a.Get("IgnoreTargetting"))
	r.Invisible = yesNo(a.Get("Invisible"))
	r.Range = atofOr(a.Get("Range"), 0)
	r.Save = yesNo(a.Get("Save"))
	r.Category = a.Get("Category")
	for i := 0; i < 5; i++ {
		r.BuffValue[i] = atofOr(a.Get("BuffValue"+strconv.Itoa(i+1)), 0)
	}
}

// LoadBuffs reads datatable_buff.xml from `dir` (the client XML folder the zone
// already uses for the stance, skill and item tables).
//
// A missing or unreadable file is an ERROR, and the caller must refuse every buff
// rather than serve a table of zeros: a buff with an invented row is a mask bit
// with no state behind it, which is the failure shape this slice exists to avoid.
func LoadBuffs(dir string) (*BuffTable, error) {
	path := filepath.Join(dir, BuffTableFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := &BuffTable{
		byID:   make(map[uint32]BuffRow, 2048),
		byName: make(map[string]uint32, 2048),
	}
	dec := xml.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Class" {
			continue
		}
		names := make([]string, 0, len(start.Attr))
		vals := make([]string, 0, len(start.Attr))
		for _, a := range start.Attr {
			names = append(names, a.Name.Local)
			vals = append(vals, a.Value)
		}
		attrs := &AttrSet{names: names, vals: vals}
		id64, err := strconv.ParseUint(strings.TrimSpace(attrs.Get("ClassID")), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%s: row with no readable ClassID (%q)", path, attrs.Get("ClassID"))
		}
		name := attrs.Get("ClassName")
		if name == "" {
			return nil, fmt.Errorf("%s: row %d has no ClassName", path, id64)
		}
		row := BuffRow{ID: uint32(id64), Attrs: attrs}
		row.fillFromAttrs()
		if _, dup := t.byID[row.ID]; dup {
			return nil, fmt.Errorf("%s: ClassID %d appears twice", path, row.ID)
		}
		t.byID[row.ID] = row
		t.rows = append(t.rows, row)
		t.byName[row.Name] = row.ID
	}
	if len(t.byID) == 0 {
		return nil, fmt.Errorf("%s holds no Buff row", path)
	}
	return t, nil
}

// yesNo reads one of the client's flag columns: "TRUE" / "YES" are true, anything
// else (including an absent attribute) is false. The table uses both spellings -
// `Save="TRUE"` and `HoldMove="NO"` - so a single reader that accepts either is
// the only one that cannot get it wrong.
func yesNo(v string) bool {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "TRUE", "YES", "1":
		return true
	}
	return false
}
