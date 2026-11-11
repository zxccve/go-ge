package zone

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// THE SKILL TABLE - what a skill id IS, by name and by its own ATK factor.
//
// WHY IT EXISTS. The cast lane (`zone\skill_cast.go`) has to name the skill in its
// one log line and has to turn an ATK into a damage number. Both facts live in the
// game's own `datatable_skill.xml`, one row per skill, and this zone had never read
// that file: the stance tables (`zone\stance.go`) carry skill IDS only.
//
// WHAT IS READ, AND WHY ONLY THESE FOUR COLUMNS. The row is 12 KB of attributes per
// skill (5,626 rows, 14.5 MB) and this lane needs almost none of it:
//
//	ClassID   the id the wire uses (`+11` of CZ_SKILL_TARGET, `+30` of both answers)
//	EngName   the name a player would recognise - "Rapid Shot" for 10561
//	ClassName the internal name ("RapidShot"), used when EngName is empty
//	SklATK    the skill's own ATK factor, a PERCENTAGE of the character's ATK
//	          (7 for Rapid Shot, 187.5 for HealingAuto). It is what makes a hard
//	          skill hit harder than a weak one with the same character.
//	UseType / ValueType / MaxR / MinR / SplashRange / SplLimit / SpendSP
//	          what the GROUND cast (`zone\skill_area.go`, R85/R83) routes on: which
//	          family the skill belongs to, how far the caster may reach, how wide the
//	          area is, how many bodies it may hit, and what it costs. These are read,
//	          never chosen: Fireball 20211's own row says AREA/Attack/150..800.
//	CastTime  the wind-up in milliseconds - the pending cast's clock
//	          (`zone\skill_cast_clock.go`). It is the honest starting point and not a
//	          measured truth: see the field's own comment.
//
// NOTHING HERE IS A GUESS ABOUT THE WIRE. No offset, no size and no field of any
// packet comes from this file - it answers "what is this id" and nothing else.
//
// A ROW IS SKIPPED, NOT FATAL, when its ClassID is not a number: the file is the
// client's and one bad row must not cost the zone every skill name. A MISSING or
// unreadable file IS an error, and the caller (`main.go`) logs it and starts
// anyway - a zone without this table refuses a cast with a line saying so rather
// than guessing a damage number.
type SkillRow struct {
	ID     uint32
	Name   string  // EngName: "Rapid Shot"
	Code   string  // ClassName: "RapidShot"
	SklATK float64 // percent of the caster's ATK; 0 means the row has none

	// SklLv, SklLvBM and SplDam are the three columns the game's own damage
	// formula reads OUT OF THE SKILL ROW and multiplies SklATK by
	// (`attack.lua:359-365`):
	//
	//	SklATK * ((SklLv + ADDSklLv + SklLv_BM) * 10 + 100) / 100   [* SplDam]
	//
	// so the factor the script computes is NOT the row's SklATK - it is that
	// number scaled by the skill's own level. They were added for R84 slice 1,
	// which runs that formula beside the placeholder (`zone\skill_script.go`), and
	// the placeholder itself still uses SklATK alone.
	//
	// SklLv is the row's own level, 1 for a fresh skill. It is an APPROXIMATION
	// and the comment says so rather than hiding it: the reference passes the
	// character's LEARNED skill object, and this server keeps no per-character
	// skill levels (ROADMAP, the CHARACTER_SKILL research lane), so the row's
	// value is the only one we hold.
	SklLv   float64
	SklLvBM float64
	SplDam  float64

	// The routing columns, read verbatim from the same `<Class>` element.
	UseType     string  // "AREA", "MELEE", "PLACEMENT" - HOW the skill is aimed
	ValueType   string  // "Attack", "Buff", "Heal"    - WHAT it does when it lands
	MaxR        float64 // the furthest the caster may reach, in map units (cm)
	MinR        float64 // the closest, so a skill cannot be cast in someone's face
	SplashRange float64 // the radius of the area around the point; 0 = a point
	SplLimit    int     // how many bodies one cast may record; 0 = the row states none
	SpendSP     int     // the SP the cast costs, cut from the caster before the 3571

	// CastTime is the wind-up the row states, in MILLISECONDS: how long the cast
	// takes, and therefore WHEN THE RESULT IS SENT (zone\skill_cast_clock.go).
	//
	// It is the column the reference server itself reads - `GetSkillCastingTime()`
	// is `m_skill->GetCastTime() * GetCAST()` (source2\geServer\ZServerLib\
	// PlayerChar.cpp:2942-2958) - scaled by the caster's cast speed. We model no
	// cast-speed stat, so the row's own number is used unscaled.
	//
	// 0 is what the table says for the fast skills (Implode Shock 10562, Deep
	// Straight 10111) and it means exactly what it says: the reference then SKIPS
	// the cast state entirely (`FieldChar_FSM.cpp:363-367`) - no 3571, the result at
	// the press - and only the HoldTime below locks the body.
	CastTime int

	// HoldTime is how long the body is HELD after the result, in MILLISECONDS: the
	// second half of a cast, and the whole lock for an instant one.
	//
	// `FieldChar_FSM.cpp:523` sets `m_useSkillEndTime = now + m_skill->GetHoldTime()`
	// at the end of the state that sends the result, and `IsHoldAction()`
	// (`FieldChar.cpp:1023-1049`) is true in that state - so a move is queued, never
	// echoed and never a cancel (`PlayerChar_Request.cpp:343-348`). The client holds
	// ITSELF for the same column plus 0.5 s on top (`GeActor.cpp:590-607`), which is
	// why the server must not also wait it before the result: that would double the
	// lock. The SCP script only carries the value across (`skill\attack.lua:222`).
	HoldTime int

	// THE DAMAGE RULE'S TWO SKILL COLUMNS (R84 slice 2). Both are read by the
	// reference's `SCR_MSG_MON_SKILL_PHYSICAL` (quoted in zone\monster_def.go):
	//
	//	IgnoreDEF   the skill's own ignore-DEF points, part of the `immune` term
	//	            `max(0, arg.IgnoreDEF + arg.DefIP - MON_IMP)`
	//	SpecialType the row's special class ("none", "Pierce", "ShieldBreak", ...).
	//	            'Pierce' has a determinate term; 'IgnoreDef' and 'SmallBane' change
	//	            the defence branches. Every OTHER value gates a buff, a skill-id
	//	            table or a random roll this build does not model, and the log line
	//	            names it rather than silently applying nothing.
	IgnoreDEF   float64
	SpecialType string

	// THE DISPATCH COLUMN, AND THE FOUR BUFF COLUMNS (this slice). `Script` is the
	// entry point the reference RUNS for a cast - `CFieldChar::ProcessSkill` hands
	// the row to the engine and calls `mainThread->Run(skill->GetScript())`
	// (FieldChar_Skill.cpp:65-67) - so 714 rows dispatch to `SCR_SKILL_BUFF` and the
	// rest to the attack family. It is the dispatch: a row's number is what decides
	// which script runs, never a table of ours.
	//
	// `Buff` / `BuffType` / `BuffFreq` / `BuffID` are the four the skill script copies
	// into the magical-argument bag (`magicalskillarg.Buff = skill.Buff`,
	// ge\script\skill\buff.scp:36-40); `SCR_RESULT_BATTLEBUFF_SKILL` decides from
	// them whether the buff lands (common_pc.scp:6727). Nothing in Go reads BuffType
	// as a rule - it is carried into the bag and the script rules on it.
	Script string

	Buff string
	// BuffType is 'FORCE' / 'Normal' / 'SklLv' / ... as the row spells it.
	BuffType string
	BuffFreq float64
	BuffID   float64

	// THE REST OF THE BAG `SCR_SKILL_BUFF` FILLS from the row. They are columns the
	// buff message script reads back off the bag (`arg.AtkLayer` gates the whole rule,
	// `arg.ClassID` picks its branches, `arg.SklLv` is the level chance and the level
	// applied), so they travel even though no Go code decides anything from them.
	ClassType string
	AtkLayer  float64
	HateAmnt  float64
	RemBuff1  string
	RemBuff2  string
	RemBuff3  string
	RemBuff4  string
	RemBuff5  string
}

// isAreaAttack is the ONE family this lane builds: a skill aimed at a POINT whose
// landing damages the bodies inside its area. Fireball 20211 is the measured case.
// Anything else - AREA+Buff (Burning Hands), PLACEMENT (Flame Ground), a heal, a
// melee skill - is NOT this lane's, and the caller refuses it with one line naming
// the family instead of answering half of it.
func (r SkillRow) isAreaAttack() bool {
	return r.UseType == "AREA" && r.ValueType == "Attack"
}

// familyWord is the word a refusal line uses for a skill this lane does not build:
// the VALUE type is what a player sees the effect of ("Buff"), and when the value is
// Attack the USE type is what is missing ("PLACEMENT"). It exists so the log says
// "the PLACEMENT family is not built yet" rather than quoting two raw columns and
// leaving the reader to work it out.
func (r SkillRow) familyWord() string {
	if r.ValueType != "" && r.ValueType != "Attack" {
		return strings.ToUpper(r.ValueType)
	}
	if r.UseType != "" {
		return strings.ToUpper(r.UseType)
	}
	return "(no UseType/ValueType in its row)"
}

// SkillTable answers a skill id. A nil table answers nothing and no zero value is
// ever used: Row reports `ok`, so a caller cannot mistake "not loaded" for a skill.
type SkillTable struct {
	byID map[uint32]SkillRow
}

// Row returns the skill's row, or ok=false when this table has never seen the id.
func (t *SkillTable) Row(id uint32) (SkillRow, bool) {
	if t == nil {
		return SkillRow{}, false
	}
	r, ok := t.byID[id]
	return r, ok
}

// Len is how many rows were read, for the startup line.
func (t *SkillTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byID)
}

// Name is the skill's display name, and it NEVER returns "" for an id the table
// holds: EngName first, ClassName second, and the id itself last, so a log line
// always says SOMETHING a reader can act on.
func (t *SkillTable) Name(id uint32) string {
	r, ok := t.Row(id)
	if !ok {
		return fmt.Sprintf("%d (not in datatable_skill.xml)", id)
	}
	if r.Name != "" {
		return r.Name
	}
	if r.Code != "" {
		return r.Code
	}
	return fmt.Sprintf("%d (row has no name)", id)
}

// skillXMLRow is the slice of one `<Class>` element this table keeps. Decoding the
// element (not the whole document) is the same pattern `create_tables.go` uses for
// the Monster table, and for the same reason: a 14 MB file must not be held twice.
type skillXMLRow struct {
	ClassID     string `xml:"ClassID,attr"`
	ClassName   string `xml:"ClassName,attr"`
	EngName     string `xml:"EngName,attr"`
	SklATK      string `xml:"SklATK,attr"`
	UseType     string `xml:"UseType,attr"`
	ValueType   string `xml:"ValueType,attr"`
	MaxR        string `xml:"MaxR,attr"`
	MinR        string `xml:"MinR,attr"`
	SplashRange string `xml:"SplashRange,attr"`
	SplLimit    string `xml:"SplLimit,attr"`
	HoldTime    string `xml:"HoldTime,attr"`
	SpendSP     string `xml:"SpendSP,attr"`
	CastTime    string `xml:"CastTime,attr"`
	SklLv       string `xml:"SklLv,attr"`
	SklLvBM     string `xml:"SklLv_BM,attr"`
	SplDam      string `xml:"SplDam,attr"`
	// The two columns the DAMAGE RULE reads off the skill (R84 slice 2):
	// `arg.IgnoreDEF` (the `immune` term) and `arg.SpecialType` (the
	// IgnoreDef/SmallBane/Pierce branches). Absent reads "" / 0.
	IgnoreDEF   string `xml:"IgnoreDEF,attr"`
	SpecialType string `xml:"SpecialType,attr"`

	// THE DISPATCH AND THE BUFF COLUMNS (see SkillRow). Read verbatim - absent reads
	// "", which is what the row itself says when it has no buff.
	Script    string `xml:"Script,attr"`
	Buff      string `xml:"Buff,attr"`
	BuffType  string `xml:"BuffType,attr"`
	BuffFreq  string `xml:"BuffFreq,attr"`
	BuffID    string `xml:"BuffID,attr"`
	ClassType string `xml:"ClassType,attr"`
	AtkLayer  string `xml:"AtkLayer,attr"`
	HateAmnt  string `xml:"HateAmnt,attr"`
	RemBuff1  string `xml:"RemBuff1,attr"`
	RemBuff2  string `xml:"RemBuff2,attr"`
	RemBuff3  string `xml:"RemBuff3,attr"`
	RemBuff4  string `xml:"RemBuff4,attr"`
	RemBuff5  string `xml:"RemBuff5,attr"`
}

// skillTableFile is the one file this table reads. There are also
// `datatable_skill_ex.xml`, `_jobspecialize` and `_worldpvp` beside it: those are
// separate id spaces for content this lane does not touch, and folding them in
// would claim a stance skill could be one of them.
const skillTableFile = "datatable_skill.xml"

// THE CACHE - WHAT SHAPE, AND WHY IT LIVES HERE.
//
// `LoadSkills` is the ONE name every caller already uses (`main.go` at startup,
// and the zone suites' `castZone` for every test that needs a caster), and the
// measured cost is exactly this call: the 14.5 MB file is opened, read and
// RE-PARSED every time, and `go test ./zone/` makes that call ~40 times. So the
// cache lives in `LoadSkills` itself, not behind a second name
// (`LoadSkillsCached`) that every caller would have to be edited to adopt - the
// callers ARE the cost, and a cache nobody calls saves nothing.
//
// WHAT IS CACHED IS THE DECODED ROWS, never a `*SkillTable`. A table is what a
// caller MUTATES (`zone\skill_area_test.go` swaps a row into `h.Skills.byID`), so
// a hit builds a FRESH map and copies the rows into it: a mutation of one
// caller's table cannot reach the next caller, and it cannot reach the cache.
//
// THE KEY is the file's absolute path plus its size and modification time. A
// rewritten file - the client patched, a fixture replaced - misses and is read
// again; nothing expires on a timer. A FAILED read is NOT cached: a missing or
// unreadable file is reported in exactly the old words, and the next call tries
// again instead of remembering a transient failure.
//
// `LoadSkillsUncached` below is the original reader, unchanged in behaviour: the
// cache calls it on a miss, and the tests compare against it.
func LoadSkills(dir string) (*SkillTable, error) {
	path := filepath.Join(dir, skillTableFile)
	key := path
	if abs, aerr := filepath.Abs(path); aerr == nil {
		key = abs
	}
	fi, serr := os.Stat(key)
	if serr != nil {
		// No stat means no key. Let the reader report the failure with the words it
		// always has (its os.Open error names the path).
		return LoadSkillsUncached(dir)
	}

	// The lock is held across the parse on purpose: a second caller that arrives
	// while the first is parsing WAITS for it and then takes the hit, so the file
	// is parsed once even when callers overlap. `LoadSkillsUncached` takes no lock,
	// so there is nothing here to deadlock against.
	skillCacheMu.Lock()
	defer skillCacheMu.Unlock()
	if e, ok := skillCache[key]; ok && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
		return &SkillTable{byID: copySkillRows(e.rows)}, nil
	}
	table, err := LoadSkillsUncached(dir)
	if err != nil {
		return table, err
	}
	// The cache keeps its OWN copy: the caller keeps the table this call parsed
	// and may mutate it freely.
	skillCache[key] = skillCacheEntry{
		size:    fi.Size(),
		modTime: fi.ModTime(),
		rows:    copySkillRows(table.byID),
	}
	return table, nil
}

// skillCacheEntry is one file's decoded rows plus the key that makes them valid.
type skillCacheEntry struct {
	size    int64
	modTime time.Time
	rows    map[uint32]SkillRow
}

// skillCache is process-wide, one entry per skill-table path. Small by
// construction: production reads one path, and the tests read that same one.
var (
	skillCacheMu sync.Mutex
	skillCache   = make(map[string]skillCacheEntry)
)

// copySkillRows is the fresh table a caller receives. `SkillRow` holds only
// numbers and strings, so copying the map's values is a complete copy.
func copySkillRows(rows map[uint32]SkillRow) map[uint32]SkillRow {
	out := make(map[uint32]SkillRow, len(rows))
	for id, r := range rows {
		out[id] = r
	}
	return out
}

// LoadSkillsUncached reads `datatable_skill.xml` from the client's xml folder,
// exactly as the first version of this loader did: every call opens, reads and
// parses the file, and every error is the one that read produced. `LoadSkills`
// is the cached entry point; this is the read it caches (and what the cache
// tests compare against).
func LoadSkillsUncached(dir string) (*SkillTable, error) {
	path := filepath.Join(dir, skillTableFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	table := &SkillTable{byID: make(map[uint32]SkillRow, 6000)}
	var bad int
	dec := xml.NewDecoder(f)
	// The file carries the client's own encoding declaration; UTF-8 is what every
	// other loader here reads it as (`LoadStances`, `LoadMonsters`), and the bytes
	// of the four attributes below are ASCII.
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, fmt.Errorf("%s: %w", path, terr)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Class" {
			continue
		}
		var row skillXMLRow
		if derr := dec.DecodeElement(&row, &start); derr != nil {
			return nil, fmt.Errorf("%s: %w", path, derr)
		}
		id, perr := strconv.ParseUint(strings.TrimSpace(row.ClassID), 10, 32)
		if perr != nil {
			bad++
			continue
		}
		table.byID[uint32(id)] = SkillRow{
			ID:          uint32(id),
			Name:        strings.TrimSpace(row.EngName),
			Code:        strings.TrimSpace(row.ClassName),
			SklATK:      atofDefault(row.SklATK, 0),
			UseType:     strings.TrimSpace(row.UseType),
			ValueType:   strings.TrimSpace(row.ValueType),
			MaxR:        atofDefault(row.MaxR, 0),
			MinR:        atofDefault(row.MinR, 0),
			SplashRange: atofDefault(row.SplashRange, 0),
			SplLimit:    intAttr(row.SplLimit),
			SpendSP:     intAttr(row.SpendSP),
			CastTime:    intAttr(row.CastTime),
			HoldTime:    intAttr(row.HoldTime),
			SklLv:       atofDefault(row.SklLv, 0),
			SklLvBM:     atofDefault(row.SklLvBM, 0),
			SplDam:      atofDefault(row.SplDam, 0),
			IgnoreDEF:   atofDefault(row.IgnoreDEF, 0),
			SpecialType: strings.TrimSpace(row.SpecialType),
			Script:      strings.TrimSpace(row.Script),
			Buff:        strings.TrimSpace(row.Buff),
			BuffType:    strings.TrimSpace(row.BuffType),
			BuffFreq:    atofDefault(row.BuffFreq, 0),
			BuffID:      atofDefault(row.BuffID, 0),
			ClassType:   strings.TrimSpace(row.ClassType),
			AtkLayer:    atofDefault(row.AtkLayer, 0),
			HateAmnt:    atofDefault(row.HateAmnt, 0),
			RemBuff1:    strings.TrimSpace(row.RemBuff1),
			RemBuff2:    strings.TrimSpace(row.RemBuff2),
			RemBuff3:    strings.TrimSpace(row.RemBuff3),
			RemBuff4:    strings.TrimSpace(row.RemBuff4),
			RemBuff5:    strings.TrimSpace(row.RemBuff5),
		}
	}
	if table.Len() == 0 {
		return nil, fmt.Errorf("%s holds no readable Skill row", path)
	}
	if bad > 0 {
		return table, fmt.Errorf("%s: %d row(s) skipped, their ClassID is not a "+
			"number", path, bad)
	}
	return table, nil
}

// intAttr reads a whole-number attribute, 0 when it is missing or not a number.
//
// 0 is the honest default for the two columns this feeds: SplLimit 0 means "the row
// states no cap" (Flame Ground's own row says 0), and SpendSP 0 means "this skill
// costs nothing" - both are things the table itself says, not a guess this loader
// invents. A row that mis-spells either one is no worse off than a row that leaves it
// out, and neither can cost the zone its skill names.
func intAttr(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// Describe is the one line `main.go` prints about this table at startup.
func (t *SkillTable) Describe() string {
	if t == nil {
		return "no skill table (a cast is REFUSED with a log line until one loads)"
	}
	return fmt.Sprintf("%d row(s) by id from %s (name + SklATK, used by the skill cast)",
		t.Len(), skillTableFile)
}
