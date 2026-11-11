// The Character Info sheet's SOURCE: the game's own status script.
//
// WHAT THIS IS. `zone\status.go` builds the 3684 (ZC_UPDATE_ALL_STATUS) - the
// packet the Character Info panel draws. The fifteen numbers in it are NOT
// computed here any more: this file runs the game's OWN
// `ge\script\pc\get_status_pc.scp` for one character (through
// `script\status.go`'s host) and answers what those entry points return, cached
// per character-and-loadout.
//
// WHY IT IS NOT A HAND PORT. Until 2026-11-04 the numbers came from Go rules
// transcribed from that same script, validated by running the script beside them
// and logging `script X / go Y` per field. That instrument did its job - it found
// a dead line in our RPSY port and a missing multiplicative identity in empty
// slots - and the owner's ruling was that transcription is not the design: "i
// dont want switching, i want the full formula working. a shortcut is a defered
// work." The Go rules are DELETED; agreeing by construction replaced agreeing by
// transcription (DECISIONS.md, 2026-11-03 / 2026-11-04, R126).
//
// A BROKEN SCRIPT MUST NEVER BREAK THE ZONE. Every entry point is called under
// `pcall` (gopher-lua's Protect) and a raise is reported per field; Sheet also
// answers ok=false so a caller can keep the last good value instead of building a
// packet out of zeros. The whole run is panic-guarded, because the zone calls it
// from inside a reply.

package zone

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"geserver/script"
)

// DefaultItemProps is where `src\emit_item_props.py` writes the 75 properties of
// every item class, relative to goserver\ (the same convention the command table
// and the script corpus use).
const DefaultItemProps = "../data/item_props.json"

// ItemProps is `data\item_props.json`: every item class's 75 columns, plus the
// growing curves. It is loaded LAZILY per class - the file is 4.7 MB and only the
// worn items are ever asked for - so the rows stay as the decoder's raw bytes
// until one is needed.
//
// WHY A SECOND FILE NEXT TO item_stats.json: that file carries 18 columns, the
// ones the GO rules read. The script reads 75 (`armor.IncSTR`, `rweapon.AddATK`,
// `stance.StnBLK`, ...), and a formula cannot be compared against a table that is
// missing four fifths of its inputs. The two emitters are cross-checked by
// `src\check_item_props.py`, which compares those 18 columns class by class.
type ItemProps struct {
	Columns []string
	Growing map[int]GrowCurve

	classes map[int]json.RawMessage
	mu      sync.Mutex
	missing map[int]bool
}

// LoadItemProps reads the emitted property file.
func LoadItemProps(path string) (*ItemProps, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Columns []string                   `json:"columns"`
		Classes map[string]json.RawMessage `json:"classes"`
		Growing struct {
			Items map[string]GrowCurve `json:"items"`
		} `json:"growing"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Columns) == 0 {
		return nil, fmt.Errorf("%s: no \"columns\" list - the file came from another emitter", path)
	}
	if len(doc.Classes) == 0 {
		return nil, fmt.Errorf("%s: no \"classes\" object - the file came from another emitter", path)
	}
	p := &ItemProps{
		Columns: doc.Columns,
		Growing: make(map[int]GrowCurve, len(doc.Growing.Items)),
		classes: make(map[int]json.RawMessage, len(doc.Classes)),
		missing: map[int]bool{},
	}
	for key, raw := range doc.Classes {
		id, aerr := strconv.Atoi(key)
		if aerr != nil {
			return nil, fmt.Errorf("%s: item key %q is not a number", path, key)
		}
		p.classes[id] = raw
	}
	for key, curve := range doc.Growing.Items {
		id, aerr := strconv.Atoi(key)
		if aerr != nil {
			return nil, fmt.Errorf("%s: growing key %q is not a number", path, key)
		}
		p.Growing[id] = curve
	}
	return p, nil
}

// Len is how many item classes the file holds, for the startup log line.
func (p *ItemProps) Len() int {
	if p == nil {
		return 0
	}
	return len(p.classes)
}

// emptySlotIdentity is what an EMPTY gear slot answers for the columns the status
// script MULTIPLIES instead of adding: their identity, 1, never 0.
//
// WHY: SCR_Get_MSPD (`get_status_pc.lua:1808`) multiplies nine slots together -
// 469 * stance.StnMSPD * armor.MSPD * boots.MSPD * costume.MSPD * face.MSPD *
// back.MSPD * belt.MSPD * Achieve.MSPD * artifact.MSPD - so ONE empty slot
// answering 0 killed the whole walk speed (the shadow read `MSPD script 0`).
// 0 is not a value a worn item has: 24,799 of the 24,975 rows in
// data\item_props.json carry MSPD >= 1 (the rest are 0.95/0.9 style slows), so
// "nothing worn" is the neutral 1.
//
// THE ONE OTHER MULTIPLIED ITEM COLUMN in the whole corpus is KDRank (`:733`,
// `:753`, `stance.RHits * weapon.KDRank`) and it is deliberately NOT here:
// 23,221 of the 24,975 classes carry KDRank 0, so 0 IS that column's own neutral
// - and neither SCR_Get_RKDRank nor SCR_Get_LKDRank is one of the fifteen panel
// numbers this shadow compares.
var emptySlotIdentity = map[string]float64{"MSPD": 1}

// Table answers one item class's 75 properties by NAME, with the growing curve
// applied at the owner's level.
//
// AN EMPTY OR UNKNOWN CLASS ANSWERS A ZERO TABLE OF THE SAME SHAPE, never nil:
// the script reads `armor.DEF` with no guard, and nil there is a Lua error that
// would abort the whole formula. The ONE exception is a MULTIPLIED column, which
// answers its identity (see emptySlotIdentity) because a 0 there erases a whole
// product. A class the file does not hold is named in the log once - that is a
// fact about the data, not about the script - and keeps the plain zero table, so
// a gap in the emitted file stays visible instead of reading as "no item".
func (p *ItemProps) Table(classID uint32, level int) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p.Columns))
	for _, c := range p.Columns {
		out[c] = float64(0)
	}
	if classID == 0 {
		for col, identity := range emptySlotIdentity {
			if _, known := out[col]; known {
				out[col] = identity
			}
		}
		return out
	}
	raw, ok := p.classes[int(classID)]
	if !ok {
		p.noteMissing(classID)
		return out
	}
	var row []any
	if err := json.Unmarshal(raw, &row); err != nil {
		p.noteMissing(classID)
		return out
	}
	for i, c := range p.Columns {
		if i >= len(row) {
			break
		}
		switch v := row[i].(type) {
		case float64:
			out[c] = v
		case string:
			if v != "" { // the two name columns: "" is the table's own empty
				out[c] = v
			}
		}
	}
	// GROWING ITEMS take ATK/DEF/WLv from the OWNER'S LEVEL - THE CURVE ALWAYS WINS.
	// The row's own ATK column is the curve AT THE LEVEL CAP (item 7201 reads 225,
	// which is 1.7x100 + 55), so using it for a level-50 character would hand them a
	// level-100 weapon. That is exactly the bug the switch exposed: the item file and
	// item_stats.json disagree on the convention (this file keeps the raw column, the
	// other emits 0 and lets the curve fill it), and the script path reads THIS file.
	// A level of 0 means "unknown" and answers 0, never the cap.
	//
	// The curve code is zone\itemstats.go's own (grow, GrowCurve, growLevelCap) so the
	// script and the movement/damage paths cannot disagree about how a "Crecemento"
	// item grows.
	if curve, ok := p.Growing[int(classID)]; ok {
		lv := level
		if lv <= 0 {
			out[curve.Column] = float64(0)
			out["WLv"] = float64(0)
			return out
		}
		if lv > growLevelCap {
			lv = growLevelCap
		}
		out[curve.Column] = curve.ValueAt(lv)
		out["WLv"] = curve.RankAt(lv)
	}
	return out
}

// propNumber reads one already-decoded column as a number (0 when it is not one).
func propNumber(m map[string]any, name string) float64 {
	if v, ok := m[name].(float64); ok {
		return v
	}
	return 0
}

// noteMissing names an item class the file does not hold, once. There is no log
// line here on purpose: the class id a slot holds is already named by the shadow's
// own line when the numbers differ, and this file's missing classes are the
// emitter's business (src\check_item_props.py), not a zone fault.
func (p *ItemProps) noteMissing(classID uint32) {
	p.mu.Lock()
	p.missing[int(classID)] = true
	p.mu.Unlock()
}

// StatusScript is the CHARACTER INFO SHEET's source: it runs the game's OWN
// fifteen status entry points (`ge\script\pc\get_status_pc.scp`) for one
// character and answers their numbers. NIL means off (scripts disabled, no
// corpus, or no item file) and the zone then has no sheet - it says so, loudly,
// and does not invent one.
//
// THIS IS THE SWITCH (R126 slice B, 2026-11-04). Until this change the zone
// computed those fifteen numbers itself (`StatusOf` and the `sheet*` rules beside
// it) and ran the script only BESIDE them, to log a comparison. The comparison
// agreed on 14 of 15 on the fixture, and the owner's call was to stop having two
// implementations: "i dont want switching, i want the full formula working. a
// shortcut is a defered work." So the Go rules are gone and this is the answer -
// agreeing by construction instead of by transcription (DECISIONS.md, 2026-11-03
// and 2026-11-04).
//
// CACHED, because the sheet is event-driven (entry, equip, stance change) but
// READ per swing: `attackInterval` and the damage base ask for it on every hit.
// The key is everything the formulas read - handle, job, level, stance, the 21
// worn classes, promotion, cash promotion and the AGI bonus - so a change to any
// of them misses the cache. There is no invalidation call to forget.
type StatusScript struct {
	Host    *script.Host
	Props   *ItemProps
	Stances *StanceTable
	MapID   uint32
	Log     func(format string, args ...any)

	// runMu serialises RunStatus: it installs a source and a `self` on the shared
	// host for the length of the run, and a second session must not see them.
	runMu sync.Mutex
	// mu guards the cache only (never held across a script run).
	mu    sync.Mutex
	cache map[string]StatusSheet

	missingOnce sync.Once
}

// NewStatusScript wires the sheet's source. A nil host or props answers nil, and
// the zone then runs on whatever it was given and says so once.
func NewStatusScript(host *script.Host, props *ItemProps, stances *StanceTable, mapID uint32, logf func(string, ...any)) *StatusScript {
	if host == nil || props == nil {
		return nil
	}
	return &StatusScript{Host: host, Props: props, Stances: stances, MapID: mapID,
		Log: logf, cache: make(map[string]StatusSheet)}
}

// Sheet answers the character's fifteen panel numbers from the game's own script.
//
// ok=false means the script could not run at all (no source, a raise, a missing
// entry point): the caller keeps whatever it had and the log says which character
// and which field, so a broken script is never mistaken for a zero stat.
func (s *StatusScript) Sheet(c Character) (StatusSheet, bool) {
	if s == nil || s.Host == nil || s.Props == nil {
		return StatusSheet{}, false
	}
	key := s.cacheKey(c)
	s.mu.Lock()
	hit, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return hit, true
	}

	// A panic in the Lua binding must not take the zone's reply down with it.
	var raised any
	s.runMu.Lock()
	func() {
		defer func() { raised = recover() }()
		run := &statusRun{script: s, c: c}
		results := s.Host.RunStatus(run, run.self(), script.StatusEntry)
		// NOTE the second name: a `sheet, ok :=` here would SHADOW the outer ok and
		// every caller would be told "the script could not run" while the sheet was
		// in hand - the bug this comment is the scar of.
		sheet, good := sheetFromResults(c, results, s.logf)
		if good {
			s.mu.Lock()
			s.cache[key] = sheet
			s.mu.Unlock()
			hit = sheet
			ok = true
		}
	}()
	s.runMu.Unlock()
	s.logMissingOnce()

	if raised != nil {
		s.logf("zone: STATUS SCRIPT char %d %q RAISED and was abandoned (%v) - the "+
			"sheet keeps its last value", c.Handle, c.Name, raised)
		return StatusSheet{}, false
	}
	return hit, ok
}

// cacheKey is everything the status formulas read. A field that is NOT in here is
// a stale sheet waiting to happen, so it is spelled out rather than hashed
// loosely: handle, job, level, stance, the 21 worn classes, promotion, cash
// promotion and the AGI bonus.
func (s *StatusScript) cacheKey(c Character) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%d|%d|%d|%d|%d|%d|", c.Handle, c.Job, c.Level, c.Stance,
		c.Promotion, c.PromotionStep, c.CashPromotion)
	for _, cls := range c.Equip {
		fmt.Fprintf(&b, "%d,", cls)
	}
	fmt.Fprintf(&b, "|%d", int(c.AGIBonus*100))
	return b.String()
}

// sheetFromResults maps the fifteen entry points onto the sheet. A field that
// RAISED is reported by name and left at zero - the caller is told (ok=false)
// rather than being handed a number nobody computed.
func sheetFromResults(c Character, results []script.StatusResult, logf func(string, ...any)) (StatusSheet, bool) {
	var s StatusSheet
	ok := len(results) > 0
	for _, r := range results {
		if r.Err != nil {
			ok = false
			if logf != nil {
				logf("zone: STATUS SCRIPT char %d %q: %s RAISED (%v)", c.Handle, c.Name, r.Name, r.Err)
			}
			continue
		}
		switch r.Name {
		case "ATK":
			s.ATK = r.Value
		case "DEF":
			s.DEF = r.Value
		case "AR":
			s.AR = r.Value
		case "DR":
			s.DR = r.Value
		case "CHR":
			s.HR = r.Value
		case "CSPD":
			s.CSPD = r.Value
		case "ASPD":
			s.ASPD = r.Value
		case "MSPD":
			s.MSPD = r.Value
		case "MHP":
			s.MHP = r.Value
		case "MSP":
			s.MSP = r.Value
		case "RFIRE":
			s.RFIRE = r.Value
		case "RICE":
			s.RICE = r.Value
		case "RLGHT":
			s.RLGHT = r.Value
		case "RSTAT":
			s.RSTAT = r.Value
		case "RPSY":
			s.RPSY = r.Value
		default:
			ok = false
			if logf != nil {
				logf("zone: STATUS SCRIPT char %d %q: the script answered %q, which the "+
					"sheet has no field for", c.Handle, c.Name, r.Name)
			}
		}
	}
	if len(results) != len(script.StatusEntry) {
		ok = false
		if logf != nil {
			logf("zone: STATUS SCRIPT char %d %q: %d result(s) for the %d entry point(s)",
				c.Handle, c.Name, len(results), len(script.StatusEntry))
		}
	}
	return s, ok
}

// logf writes one line, or nothing when no logger is wired (a unit test).
func (s *StatusScript) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// logMissingOnce prints the builtin recorder ONCE per process: the names the
// status script asked for that NOTHING answered. After the bindings in
// script\status.go this list should hold script helpers rather than panel inputs,
// and if it ever holds a panel input the formula was reading a 0 nobody chose.
func (s *StatusScript) logMissingOnce() {
	s.missingOnce.Do(func() {
		missing := s.Host.Missing()
		if len(missing) == 0 {
			s.logf("zone: STATUS SCRIPT host: 0 missing builtins - every name the " +
				"status script asked for had a binding")
			return
		}
		parts := make([]string, 0, len(missing))
		for _, m := range missing {
			parts = append(parts, fmt.Sprintf("%s(%d)", m.Name, m.Calls))
		}
		s.logf("zone: STATUS SCRIPT host: %d missing builtin(s): %s",
			len(missing), strings.Join(parts, ", "))
	})
}

// -- the source the builtins read ------------------------------------------

// statusRun is ONE character's answer to the status builtins: the worn items,
// its stance row and the two data tables this build has loaded. It is created per
// run, so nothing about one character can leak into another's.
type statusRun struct {
	script *StatusScript
	c      Character
}

// equipSpotByName is the script's slot name -> the EQUIP_SPOT index the client
// and wear_item use. The names are the 19 `GetEquipItem(self, 'X')` literals in
// get_status_pc.lua, counted there: RH 54, LH 54, ACHIEVE 46, BACK 45, ARTIFACT
// 41, COSTUME 40, RCOS 37, LCOS 37, NECK 35, BODY 32, RING2 31, RING1 31,
// GLOVE 28, BOOTS 26, BELT 25, EAR 24, FACE 21, HEAD 20, ASSIST 4.
//
// ASSIST has NO spot: it is the summon/assist slot, which this server's
// wear_item model does not carry, so it answers the zero table. That is the
// honest answer and the script's own `+ assist.X` term is 0 as it is in the Go
// rules.
var equipSpotByName = map[string]int{
	"HEAD": spotHead, "BODY": spotBody, "FACE": spotFace, "NECK": spotNeck,
	"RING1": spotRing1, "RING2": spotRing2, "GLOVE": spotGlove, "BOOTS": spotBoots,
	"BACK": spotBack, "COSTUME": spotCostume, "EAR": spotEar, "BELT": spotBelt,
	"ACHIEVE": spotAchieve, "LCOS": spotLCostumeWeapon, "RCOS": spotRCostumeWeapon,
	"ARTIFACT": spotArtifact,
}

// stanceAttributes is the 32 attributes of a stance row the status script reads
// by name. The five STRING ones are listed here; every other one is a number.
var stanceAttributes = []string{
	"ASPD", "AtkLayer", "AtkType", "ClassID", "ClassName", "ClassType",
	"DefLayer", "DefType", "IncASPD", "IncBLK", "IncCRT", "IncHR", "IncIMP",
	"IncIP", "IncPR", "IncRSP", "LHits", "RHits", "RSPType", "StnATK", "StnBLK",
	"StnHR", "StnIMP", "StnIP", "StnIgnoreDEFPer", "StnLv", "StnMSPD", "StnPR",
	"StnRFIRE", "StnRICE", "StnRLGHT", "StnRSTAT",
}

// stanceStringAttributes is the subset of the 32 that is a NAME, not a number.
var stanceStringAttributes = map[string]bool{
	"AtkType": true, "ClassName": true, "ClassType": true,
	"DefType": true, "RSPType": true,
}

func (r *statusRun) Handle() uint32 { return r.c.Handle }

// EquipItem answers one worn slot's 75 properties, growing applied at the
// character's own level.
func (r *statusRun) EquipItem(slot string) map[string]any {
	name := strings.ToUpper(strings.TrimSpace(slot))
	spot, ok := equipSpotByName[name]
	// RH/LH are the two hands, which have their own constants in stance.go.
	switch name {
	case "RH":
		spot, ok = HandSpotRight, true
	case "LH":
		spot, ok = HandSpotLeft, true
	}
	if !ok {
		// ASSIST, or a slot name this build has never seen: a zero table of the
		// same shape, because the script indexes it.
		return r.script.Props.Table(0, 0)
	}
	if spot < 0 || spot >= equipSpotLast {
		return r.script.Props.Table(0, 0)
	}
	return r.script.Props.Table(r.c.Equip[spot], int(r.c.Level))
}

// Stance answers the character's stance row, raw. StnATK and StnMSPD default to 1
// when the row does not state them: 0 there is a character who cannot hit or
// cannot walk, and the table's own neutral value is 1 (see LoadStances).
func (r *statusRun) Stance() map[string]any {
	out := make(map[string]any, len(stanceAttributes))
	row, ok := r.script.Stances.RawStance(r.c.Stance)
	for _, name := range stanceAttributes {
		if stanceStringAttributes[name] {
			out[name] = row[name]
			continue
		}
		raw := ""
		if ok {
			raw = row[name]
		}
		switch name {
		case "StnATK", "StnMSPD":
			out[name] = atofDefault(raw, 1)
		default:
			out[name] = atof(raw)
		}
	}
	return out
}

// NumberValue is `GetDataTableNumberValue(table, id, column)`. Stance and Job are
// answered from the tables this zone already loaded; any other table is a 0 the
// caller logs once.
func (r *statusRun) NumberValue(table string, id uint32, column string) (float64, bool) {
	switch strings.ToLower(table) {
	case "stance":
		row, ok := r.script.Stances.RawStance(id)
		if !ok {
			return 0, true // a stance with no row: the table's own 0
		}
		return atof(row[column]), true
	case "job":
		row, ok := r.script.Stances.RawJob(id)
		if !ok {
			return 0, true
		}
		return atof(row[column]), true
	}
	return 0, false
}

// NumberValueByClassName is the same read keyed by ClassName. Only the Job table
// is looked up by name in this corpus (`...('Job', name, 'ClassID')`).
func (r *statusRun) NumberValueByClassName(table, className, column string) (float64, bool) {
	if !strings.EqualFold(table, "job") {
		return 0, false
	}
	id, ok := r.script.Stances.JobIDByClassName(className)
	if !ok {
		return 0, true
	}
	return r.NumberValue("Job", id, column)
}

// StringValue is `GetDataTableStringValue(table, id, column)`.
func (r *statusRun) StringValue(table string, id uint32, column string) (string, bool) {
	switch strings.ToLower(table) {
	case "stance":
		row, _ := r.script.Stances.RawStance(id)
		return row[column], true
	case "job":
		row, _ := r.script.Stances.RawJob(id)
		return row[column], true
	}
	return "", false
}

// ZoneMapClassID is the REAL map class id this zone runs.
func (r *statusRun) ZoneMapClassID() uint32 { return r.script.MapID }

// Promotion is the character's promotion level as the script's promotionLv:
// already banded by PGStore.loadPromotion (min(PromotionStep, promotion_band)),
// so this is the same number the barrack packet carries. SCR_Get_MSP's
// `+ promotionLv*10` is the term it feeds.
func (r *statusRun) Promotion() int { return int(r.c.Promotion) }

// CashPromotion is the raw CASH step (GetCashPromotionLevel).
func (r *statusRun) CashPromotion() int { return int(r.c.CashPromotion) }

// self builds the `self` table the script reads: the six base stats, the level,
// the job and 'PC'.
//
// WHAT IS DELIBERATELY LEFT 0: every buff/weapon-mastery term
// (ATK_BM, PromotionSTR, WeaponPF, <stat>_WP, ...) and every commander term. They
// are 0 in the Go rules too, for the same reason - this server has no such data -
// so both sides of the comparison read the same inputs and a difference can only
// come from the FORMULA.
//
// PROMOTION IS NOT ONE OF THEM ANY MORE: Promotion / CashPromotion come off the
// character row (PGStore.loadPromotion), because `character_property` really does
// carry them and SCR_Get_MSP adds promotionLv*10. A promoted character therefore
// reads its OWN max SP here, not the unpromoted floor.
//
// TWO STATS ARE NOT THE BASE STATS, and this follows the Go rules exactly:
//
//	AGI is base + AGIBonus, because the script's cast-speed rule reads the
//	    character's AGI WITH the worn items on (self.AGI), and zone\status.go's
//	    sheetCSPD takes that same pair as `agiBonus`.
//	every other stat is the job's base value, because statAndWeapon reads the
//	    base stat (there is no gear AGI/STR column in our item data at all).
func (r *statusRun) self() script.StatusSelf {
	return statusSelfFor(r.c)
}

// statusSelfFor is the same `self` table for a character with no run around it,
// so a SECOND source can read the identical inputs. The damage script
// (zone\skill_script.go) needs it: its formula reads the same six base stats and
// the same level off `self`, and two builders would be two answers to one
// question the moment either is edited.
func statusSelfFor(c Character) script.StatusSelf {
	base, _ := jobStats.For(c.Job)
	props := map[string]float64{
		"Job":   float64(c.Job),
		"Lv":    float64(c.Level),
		"STR":   float64(base.STR),
		"CON":   float64(base.CON),
		"DEX":   float64(base.DEX),
		"INT":   float64(base.INT),
		"AGI":   float64(base.AGI) + c.AGIBonus,
		"CHA":   float64(base.CHA),
		"AR_BM": 0, "ATK_BM": 0, "ATKADD_BM": 0, "ASPD_BM": 0, "BLKLimit_BM": 0,
		"CON_BM": 0, "CRT_BM": 0, "DEF_BM": 0, "DEX_BM": 0, "DR_BM": 0,
		"HR_BM": 0, "IMP_BM": 0, "INT_BM": 0, "MHP_BM": 0, "MSP_BM": 0,
		"MSPD_BM": 0, "PR_BM": 0, "STR_BM": 0, "CHA_BM": 0, "AGI_BM": 0,
		"CON_WP": 0, "DEX_WP": 0, "INT_WP": 0, "STR_WP": 0, "CHA_WP": 0,
		"AGI_WP": 0, "ArmorPF": 0, "WeaponPF": 0,
	}
	return script.StatusSelf{Handle: c.Handle, Props: props, ClassName: "PC"}
}
