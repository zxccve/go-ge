package zone

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
)

// ItemStat is ONE item class's own numbers - the columns the game's status
// scripts read off a worn item. Copied from the client's own
// `ge\xml\datatable_item_*.xml` by `src\emit_item_stats.py` into
// `data\item_stats.json`; a column the client leaves empty is simply 0 here,
// never a defaulted "plausible" value.
//
// WHY THESE COLUMNS: every rule that produces the sheet's derived block reads
// one of them - SCR_Get_CATK (`get_status_pc.scp:944`) starts from `rweapon.ATK`,
// SCR_Get_AR (`:565`) from `WLv`/`AR`, SCR_Get_DEF (`:1126`) and SCR_Get_DR
// (`:632`) from `WLv`/`ShldDR`, SCR_Get_CHR (`:1277`) from `HR`,
// SCR_Get_CSPD (`:1613`) from `ASPD`, and the five resistances (`:2105`..`:2292`)
// from `RFIRE`..`RPSY` plus `RedMAG`. `Weight` is read by the Burlon-point rule
// and `ClassType` ("Sword", "Shield", "Rosario" ...) picks the branch.
type ItemStat struct {
	ATK       float64 `json:"ATK"`
	DEF       float64 `json:"DEF"`
	AR        float64 `json:"AR"`
	ShldDR    float64 `json:"ShldDR"`
	WLv       float64 `json:"WLv"`
	MHP       float64 `json:"MHP"`
	IncMHP    float64 `json:"IncMHP"`
	RFIRE     float64 `json:"RFIRE"`
	RICE      float64 `json:"RICE"`
	RLGHT     float64 `json:"RLGHT"`
	RSTAT     float64 `json:"RSTAT"`
	RPSY      float64 `json:"RPSY"`
	RedMAG    float64 `json:"RedMAG"`
	Weight    float64 `json:"Weight"`
	HR        float64 `json:"HR"`
	ASPD      float64 `json:"ASPD"`
	// MSPD is the walk-speed MULTIPLIER the item carries (1 neutral, 0.95
	// slower, 1.05 faster - NOT a percentage). SCR_Get_MSPD multiplies it into
	// its 469 base; see zone\speed.go. A zero means the emitted file has no
	// value for this class, which the speed rule reads as the neutral 1.
	MSPD      float64 `json:"MSPD"`
	ClassType string  `json:"ClassType"`
}

// ItemStats is item class id -> its columns: data\item_stats.json. The file is
// `{"count": N, "items": {"<class id>": {...}}, "growing": {...}}` so the count
// can be printed and checked without walking the map twice.
type ItemStats struct {
	items map[int]ItemStat

	// growing is the second half of the same file: the items that carry NO
	// ATK/DEF/WLv of their own and take them from the OWNER'S LEVEL instead
	// (the "Crecemento" gear). Keyed by item class id, like items.
	growing map[int]GrowCurve

	// mu guards reported, the class ids already named in the log - a worn item
	// that the file does not know must be said ONCE, not once per packet.
	mu       sync.Mutex
	reported map[int]bool
}

// GrowCurve is one growing item's numbers, as the client's own
// `ge\xml\datatable_growing_item.xml` writes them: two lines over the owner's
// level, `<Class ItemID="7201" Rank="math.floor(5 + Lv/4)" Value="Lv*1.7+55"/>`.
//
//	value = ValueMul*level + ValueAdd   - the item's grown number, which fills
//	                                      Column (ATK for a weapon, DEF for
//	                                      armour or a shield)
//	rank  = RankMul*level + RankAdd     - the grown WLv, floored, which is the
//	                                      A.R./D.R. term
//
// WHAT THE CLIENT ITSELF DOES WITH THE TWO, so nobody reads more into this than
// the evidence says (ge.exe, read by the packet analyzer 2026-10-29):
//
//   - the table IS read: the name `datatable_growing_item` sits at 0x9f3354 and
//     the parser is `FUN_00638604`, which keys its map by ItemID (NEVER by the
//     file's ClassID) and reads exactly Value and Rank; `ClassID` is ignored.
//   - it evaluates both expressions with only `Lv` in scope
//     (`SCR_EVALUATE_GROWING_ITEM_STAT`, 0x9f5360) and clamps Lv to 100.
//   - the ONE consumer is the item TOOLTIP (`FUN_006de50b`): it adds Value to a
//     base number and Rank to another, and prints both in `item_desc_spec`. The
//     client writes nothing back to the item and computes no damage from it. It
//     does not compute the Character Info sheet at all (that is why we send
//     3684), so the SERVER is what has to turn these two lines into stats.
//   - which row of the item the Value is added to in the tooltip is a global
//     config compare (`FUN_005ad372`), not a test on the item's ClassType, so
//     the disassembly does NOT say ATK-versus-DEF for us.
//
// So the COLUMN choice is not taken from the disassembly. It comes from the
// shipped item text and the peer items of the same level:
//
//	"7201 ... the A.R. and ATK Power according to the player's level" (10047079)
//	"15001 ... the D.R. and DEF Power according to the player's level" (10017601)
//	"7215 ... a Shield that increases DEF according to the level of character"
//	at level 100: 7201 gives 225 and 30, and the PLAIN level-100 swords 6001 /
//	6010 / 6016 carry ATK 225, WLv 30; 15001 gives 175 and 28 against a plain
//	level-100 leather body's DEF 172 and WLv 26.
//
// The Rank -> WLv half is the same kind of inference and it is the weakest link
// here: the client only PRINTS Rank. It is what makes the item text's "the A.R.
// and D.R. ... according to the player's level" true, and its magnitude lands
// on the WLv of every plain item of the same level, so it is used - and named in
// the report as an inference, not as something the binary proved.
type GrowCurve struct {
	Column   string  `json:"column"`
	ValueMul float64 `json:"value_mul"`
	ValueAdd float64 `json:"value_add"`
	RankMul  float64 `json:"rank_mul"`
	RankAdd  float64 `json:"rank_add"`
}

// growLevelCap is the highest level a growing item's curve is asked at: the
// client's own tooltip path `FUN_00637151` (ge.exe 0x637151) clamps the owner
// level to 100 before it evaluates Value and Rank.
const growLevelCap = 100

// ValueAt is the grown number at one owner level.
func (g GrowCurve) ValueAt(level int) float64 {
	return g.ValueMul*float64(level) + g.ValueAdd
}

// RankAt is the grown WLv at one owner level. The table's Rank is written as
// `math.floor(...)`, so the result is floored here too.
func (g GrowCurve) RankAt(level int) float64 {
	return math.Floor(g.RankMul*float64(level) + g.RankAdd)
}

// LoadItemStats reads the emitted item columns.
//
// A failure is returned, never swallowed here: the CALLER decides whether a
// missing file is fatal (in the zone it is not - see main.go loadItemStats).
func LoadItemStats(path string) (*ItemStats, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Count   int                 `json:"count"`
		Items   map[string]ItemStat `json:"items"`
		Growing struct {
			Count int                  `json:"count"`
			Items map[string]GrowCurve `json:"items"`
		} `json:"growing"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Items == nil {
		return nil, fmt.Errorf("%s: no \"items\" object - the file came from "+
			"another emitter", path)
	}
	t := &ItemStats{
		items:    make(map[int]ItemStat, len(doc.Items)),
		growing:  make(map[int]GrowCurve, len(doc.Growing.Items)),
		reported: map[int]bool{},
	}
	for key, stat := range doc.Items {
		id, aerr := strconv.Atoi(key)
		if aerr != nil {
			return nil, fmt.Errorf("%s: item key %q is not a number - the file "+
				"came from another emitter", path, key)
		}
		t.items[id] = stat
	}
	if len(t.items) == 0 {
		return nil, fmt.Errorf("%s: no item at all", path)
	}
	// The growing block is OPTIONAL on purpose: a file written before it existed
	// still loads, and every item then answers only the columns of its own row.
	// That is the pre-slice behaviour, not a broken sheet, so it is not an error.
	for key, curve := range doc.Growing.Items {
		id, aerr := strconv.Atoi(key)
		if aerr != nil {
			return nil, fmt.Errorf("%s: growing key %q is not a number - the file "+
				"came from another emitter", path, key)
		}
		t.growing[id] = curve
	}
	return t, nil
}

// Len is how many item classes the file holds, for the startup log line.
func (t *ItemStats) Len() int {
	if t == nil {
		return 0
	}
	return len(t.items)
}

// GrowingLen is how many growing items the file holds, for the startup log line.
func (t *ItemStats) GrowingLen() int {
	if t == nil {
		return 0
	}
	return len(t.growing)
}

// For answers one item class's columns.
//
// An item that is not in the file answers a zero ItemStat and is named in the
// log once: a worn class the emitter did not see is a fact a reader of the log
// deserves, and the packet must still go out. A nil table (the file was missing
// at startup) answers zeros silently - main.go has already said so.
func (t *ItemStats) For(classID uint32) (ItemStat, bool) {
	if t == nil || classID == 0 {
		return ItemStat{}, false
	}
	if stat, ok := t.items[int(classID)]; ok {
		return stat, true
	}
	t.noteMissing(int(classID))
	return ItemStat{}, false
}

// noteMissing logs an unknown item class exactly once.
func (t *ItemStats) noteMissing(classID int) {
	t.mu.Lock()
	seen := t.reported[classID]
	t.reported[classID] = true
	t.mu.Unlock()
	if !seen {
		log.Printf("zone: item class %d has no row in item_stats.json - its stat columns "+
			"count as 0 in the Character Info sheet", classID)
	}
}

// itemStats is the table the status block reads. Package state, like jobStats:
// one copy, so two builders cannot answer differently for the same item.
var itemStats *ItemStats

// SetItemStats installs the item columns. Called once at startup, before any
// connection; nil is legal and means "no table" (every item answers zeros).
func SetItemStats(t *ItemStats) { itemStats = t }

// wornItem is the item in one equipment slot, or a zero ItemStat for an empty
// slot (class 0) or a class the file does not hold.
//
// GROWING ITEMS: a "Crecemento" item leaves ATK/DEF/WLv empty on its own row
// and takes them from the OWNER'S LEVEL (see GrowCurve). The level is the one
// the session already carries, `Character.Level`, which PGStore filled from
// barrack.LevelOf(exp) - the game's own exp table, never a fixed 100. A column
// the row DOES carry (7217 Leguard's DEF 2, for one) is left alone: the growing
// curve only fills what is empty.
func wornItem(c Character, spot int) ItemStat {
	if spot < 0 || spot >= equipSpotLast {
		return ItemStat{}
	}
	classID := c.Equip[spot]
	stat, _ := itemStats.For(classID)
	if curve, ok := itemStats.growingFor(classID); ok {
		stat = grow(stat, curve, int(c.Level))
	}
	return stat
}

// growingFor answers one item class's growing curve, if it is a growing item.
func (t *ItemStats) growingFor(classID uint32) (GrowCurve, bool) {
	if t == nil || classID == 0 {
		return GrowCurve{}, false
	}
	curve, ok := t.growing[int(classID)]
	return curve, ok
}

// weaponATKPath says WHERE the right-hand item's ATK came from and the number it
// produced, for the per-swing ASPD TERMS line (R92).
//
// The two paths are the two wornItem already takes: the item's OWN ATK column,
// or - when that column is empty and the item has a growing curve whose column is
// ATK - the curve evaluated at the owner's level, capped at 100 exactly as the
// client caps it. It reads the SAME tables through the SAME helpers, so the
// number printed is the number sheetCATK multiplied.
//
// A weapon whose own row says ATK 0 and that has no ATK curve (a bare hand, or a
// weapon the game really gives no attack) reports 0 from its own column, which is
// the truth and not a silence.
func weaponATKPath(classID uint32, level int) (float64, string) {
	if classID == 0 {
		return 0, "no item in the right hand"
	}
	own, known := itemStats.For(classID)
	curve, growing := itemStats.growingFor(classID)
	if own.ATK == 0 && growing && curve.Column == "ATK" && level > 0 {
		lv := level
		if lv > growLevelCap {
			lv = growLevelCap
		}
		return curve.ValueAt(lv), "its growing curve (" + num(curve.ValueMul) + " x Lv" +
			strconv.Itoa(lv) + " + " + num(curve.ValueAdd) +
			"; class " + strconv.FormatUint(uint64(classID), 10) +
			"'s own ATK column is 0)"
	}
	if !known {
		return 0, "class " + strconv.FormatUint(uint64(classID), 10) +
			", which item_stats.json does not hold"
	}
	return own.ATK, "its own ATK column (class " +
		strconv.FormatUint(uint64(classID), 10) + ")"
}

// grow fills in the columns a growing item does not carry.
//
// A level the table cannot serve (0, or a table with no row for the item) leaves
// the item's own columns alone, so the sheet answers 0 rather than a made-up
// number, and nothing divides by the level so there is nothing to panic on.
//
// The level is CAPPED AT 100, because the client caps it: its own tooltip path
// `FUN_00637151` (ge.exe 0x637151) does `if (100 < lv) lv = 100` before it
// evaluates the two expressions. The exp table reaches level 200, so without the
// cap a level-110 character would read numbers the client itself would never
// show. This is the same cap SCR_Get_AR and SCR_Get_DR already put on floor(Lv/4).
//
// The grown value is NOT floored here: the rules that consume it (SCR_Get_CATK,
// SCR_Get_DEF, SCR_Get_AR, SCR_Get_DR) each floor their own total, exactly as
// the script does, so flooring twice would drop a fraction the game keeps. The
// Rank IS floored, because the table writes `math.floor(...)` around it.
func grow(stat ItemStat, curve GrowCurve, level int) ItemStat {
	if level <= 0 {
		return stat
	}
	if level > growLevelCap {
		level = growLevelCap
	}
	value := curve.ValueAt(level)
	switch curve.Column {
	case "ATK":
		if stat.ATK == 0 {
			stat.ATK = value
		}
	case "DEF":
		if stat.DEF == 0 {
			stat.DEF = value
		}
	}
	if stat.WLv == 0 {
		stat.WLv = curve.RankAt(level)
	}
	return stat
}
