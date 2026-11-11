package zone

// THE THREE TABLES A DROP ROLL READS (R98 slice 3).
//
// THE SPEC IS `history\monster-death.md` §4 and the reference is
// `source2\geServer\ZServerLib\MonsterDropItem.cpp`. Only three of the five rules
// are implemented in this slice, and these are the tables they need:
//
//	basic   the MONSTER row's Drop*Freq + DropCrystalCount (loaded with the row,
//	        zone\create_tables.go) and the ITEM table's DropRule / ItemLv / Unstack
//	        columns, which pick the item from the rule's level window
//	        (`geItemTable::GetItemsByDropRule`, ItemTable.cpp:1350-1381)
//	unique  the MONSTER row's six trophies (same row, nothing extra)
//	zone    the MAP row's DropItem1..DropItem7 + their frequencies
//	        (`geMapTable::MAP_PROPERTY::MAXDROPITEM` is 7, geMapTable.h:85 - the
//	        doc says 1..5; the header and the shipped row carry 7)
//
// THE ITEM-GROUP AND SPECIAL-GROUP RULES ARE DELIBERATELY ABSENT. They read
// `datatable_itemgroup.ies` (a RANDOM_TABLE decode this server has not done yet)
// and `datatable_specialitem_drop.ies`; without that decode a group drop would be
// an invented item. They are a later slice (zone\drop.go prints the same sentence
// once at the first kill).
//
// THE ITEM TABLE IS THE CLIENT'S OWN, SPLIT ACROSS FILES. `datatable_item.xml`
// itself holds only 2 rows (the Item idspace's shared items); the 25,000+ real
// rows live in `datatable_item_weapon.xml`, `_armor`, `_etc`, `_consume`, ... and
// every one of them is the SAME idspace (`<idspace id="Item">`). The reference
// reads ONE merged `Item` class list (`CItemTable::Init`, ItemTable.cpp:1095-1112);
// so does this loader, and the idspace test is what keeps `ItemOpt`, `ItemGroup`
// and `itembless` out of it.

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ItemRow is one row of the client's Item idspace, reduced to the columns a drop
// roll and the 3536 appearance need.
type ItemRow struct {
	ID   uint32
	Name string

	// Lv is `ItemLv` - the number `GetItemsByDropRule` filters the level window on
	// (`property->Lv`, ItemTable.cpp:1371). NOT `UseLv`/`WLv`.
	Lv int
	// DropRule is the row's `DropRule` string ("OldEqp", "Etc", ...). A row with
	// "None" is never selected by a rule.
	DropRule string

	// Unstack is the reference's `isUnStack`: TRUE means each unit is its own
	// field item; FALSE means one field item of N units (ItemTable.cpp:654-663,
	// MonsterDropItem.cpp:415-421).
	Unstack bool

	// MaxStack is the row's `MaxStack` - the client's per-slot cap. The give path
	// passes it into `item.Op.MaxStack` so `//item <Name> <Count>` cannot write a
	// stack the client will not draw (zone\items.go). It is READ, never guessed:
	// a row with no column answers 0, which the write path reads as "no cap
	// known" rather than inventing one.
	MaxStack int64

	// OptionGroup / Option1ID are the two facts that decide whether a dropped item
	// could carry a RANDOM OPTION. The reference's option generator only attaches
	// an option whose `ReqGroupName` equals this row's `OptionGroup` and whose
	// `Rarity` is non-zero (OptionGen.cpp:14-40, :152-187); a unique item
	// (Option1ID != 0) ALWAYS gets one from its own columns. The shipped ItemOpt
	// table has NO row with ReqGroupName "None" and Rarity > 0, so an item in the
	// None group can never carry an option - which is what makes the 3536's
	// `isOptionItem` byte CERTAIN for those items (zone\drop.go,
	// itemAppearanceCertain).
	OptionGroup string
	Option1ID   uint32

	// DropRareFreq is the row's own rare-drop frequency. When it is non-zero the
	// reference rolls sockets and a reinforcement value for that drop
	// (`GetDropRareVal`, ItemTable.cpp:211-260), and THIS SERVER HAS NO SOURCE
	// for either: the 3536's socketCount/reinforceCount would be invented. Such a
	// drop still rolls and logs; only its packet is withheld, with the reason.
	DropRareFreq float64

	// GroupName is the row's `GroupName` - "Consume", "Scroll", "Period", "Etc", ...
	// It is the column the client's own use gate reads (IsUseableItem,
	// GENormalItem.cpp:63-65: Consume / Scroll / Period are the three it lets a
	// player click), so the zone's use verb (zone\bag_verbs.go) reads the SAME
	// column instead of inventing a class range. Nothing on the DROP path reads it.
	GroupName string
	// UseLv is the row's `UseLv`: the level a character must have to use it. The
	// client refuses below it (GENormalItem.cpp:86-90) and the reference's
	// DispatchUseItem does too (PlayerChar_Request.cpp:560-566). NOT `ItemLv`.
	UseLv int

	// ScpType is the row's `ScpType`: the name of the GAME'S OWN use script, run by
	// the reference's `CPlayerChar::UseItem` as `context->Run(itemProperty->scpType)`
	// with `buffName`, `arg1` and `arg2` pushed as the script's arguments
	// (PlayerChar.cpp:2237-2250; the signature is
	// `func SCP_USE_ITEM_HPUP(self, BuffName, arg1, arg2)`,
	// ge\script\item\item.scp:1529). "None" or empty means the row names no effect.
	//
	// THIS IS THE COLUMN THAT DECIDES WHETHER A USE HEALS, so the use verb does not
	// need a table of "potion classes" - the item's own script name says what it is.
	// HealingPotion 20001 carries "SCP_USE_ITEM_HPUP"
	// (cache\datatable_item_consume.xml, Class ClassID="20001").
	ScpType string

	// Arg1 is the row's `arg1` column: the FIRST argument that script receives
	// (`PushFloat(itemProperty->arg1)`, PlayerChar.cpp:2247). For a healing potion it
	// is the heal amount - the same number the client's own batch-use script reads as
	// `GetPropertyNumber('Item', itemType, 'arg1')`
	// (GEAndromida/tools/andro/ui/uiscp/useitem.scp:14) and the oracle reads as
	// `effect.get("arg1")` (src\zone_server.py:4754). HealingPotion 20001 = 750.
	//
	// Arg1Known separates "the row says 0" from "the row has no arg1 at all": the
	// second is a DATA GAP and a heal must not read it as a 0-point heal.
	Arg1      float64
	Arg1Known bool

	// Arg2 is the row's `arg2`: the THIRD argument the use script receives
	// (`PushFloat(itemProperty->arg2)`, PlayerChar.cpp:2248) and the number a buff
	// script passes on to `AddBuff` as its own arg2. MEASURED on the 360 buff-item
	// rows: all 360 carry the attribute and 119 carry a non-zero one (20046
	// WhiteHouse_1 reads arg1 12 / arg2 600000), so leaving it at 0 would hand a
	// script a number the table does not say.
	Arg2      float64
	Arg2Known bool

	// BuffName is the row's `BuffName`: the buff the item grants, and the SECOND
	// argument the reference pushes into the row's own use script
	// (`context->PushStringID(itemProperty->buffName)`, PlayerChar.cpp:2246; the
	// signature is `func SCP_USE_ITEM_AddBuff(self, BuffName, arg1, arg2)`,
	// ge\script\item\item.scp:1567). "None" means the row grants no buff - and it is
	// the same column `CPlayerChar::UseItem` tests before it sends `SendChangeBuff`
	// (PlayerChar.cpp:2258-2260), which is why the buff path reads it rather than a
	// list of class ids.
	//
	// MEASURED (cache\datatable_item_consume.xml, 3,129 rows): 400 rows carry a real
	// BuffName - 313 of them name SCP_USE_ITEM_TeamAddBuff and 30 SCP_USE_ITEM_AddBuff,
	// the two scripts this build runs, and the other 57 name a script that is not a
	// plain add (SongPyeon, TotalStatA, TransPotion, ...) and are NOT run here.
	BuffName string
}

// ItemTable is the merged Item idspace: by ClassID, by ClassName, and by DropRule
// in FILE ORDER (the reference picks uniformly from the class list's own order).
type ItemTable struct {
	byID   map[uint32]ItemRow
	byName map[string]uint32
	byRule map[string][]uint32
}

// Row answers one item, and false when the table has no such id.
func (t *ItemTable) Row(id uint32) (ItemRow, bool) {
	if t == nil {
		return ItemRow{}, false
	}
	r, ok := t.byID[id]
	return r, ok
}

// IDByName resolves a ClassName ("ShinyCrystal") the way the reference's
// `geItemTable::Get("ShinyCrystal")` does for the crystal drop.
func (t *ItemTable) IDByName(name string) (uint32, bool) {
	if t == nil {
		return 0, false
	}
	id, ok := t.byName[strings.ToLower(strings.TrimSpace(name))]
	return id, ok
}

// ByRule answers the ids whose DropRule is exactly `rule` and whose ItemLv is in
// [minLv, maxLv], in the order the client's files list them. This is
// `CItemTable::GetItemsByDropRule` minus the classType filter, which the drop
// callers always pass as 0 (`MonsterDropItem.cpp:360-372`).
func (t *ItemTable) ByRule(rule string, minLv, maxLv int) []uint32 {
	if t == nil {
		return nil
	}
	all := t.byRule[rule]
	if len(all) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(all))
	for _, id := range all {
		r := t.byID[id]
		if r.Lv < minLv || r.Lv > maxLv {
			continue
		}
		out = append(out, id)
	}
	return out
}

// Len is how many item rows the table holds, for the one log line.
func (t *ItemTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byID)
}

// itemXMLRow is the subset of an Item Class row this loader reads. Decoding a
// struct (rather than a map of every attribute) matters: the merged table is
// 25,353 rows at 153 columns each.
type itemXMLRow struct {
	ClassID      string `xml:"ClassID,attr"`
	ClassName    string `xml:"ClassName,attr"`
	ItemLv       string `xml:"ItemLv,attr"`
	DropRule     string `xml:"DropRule,attr"`
	Unstack      string `xml:"Unstack,attr"`
	MaxStack     string `xml:"MaxStack,attr"`
	OptionGroup  string `xml:"OptionGroup,attr"`
	Option1ID    string `xml:"Option1ID,attr"`
	DropRareFreq string `xml:"DropRareFreq,attr"`
	GroupName    string `xml:"GroupName,attr"`
	UseLv        string `xml:"UseLv,attr"`
	// ScpType and Arg1 are the use effect: the game's own script name and its first
	// argument (see ItemRow.ScpType / ItemRow.Arg1). The `arg1` attribute is
	// lowercase in the client's files, so the tag is exact-case.
	ScpType string `xml:"ScpType,attr"`
	Arg1    string `xml:"arg1,attr"`
	Arg2    string `xml:"arg2,attr"`
	// BuffName is the buff this item grants - the use script's second argument and
	// the column `CPlayerChar::UseItem` tests before it resends the bar.
	BuffName string `xml:"BuffName,attr"`
}

// LoadItemTables reads every `datatable_item*.xml` in `dir` whose idspace is the
// Item idspace.
//
// A file that cannot be opened is an ERROR (the zone then says it has no item
// table and drops only what needs no item row - the trophies are ids, not item
// lookups). A file that IS readable but belongs to another idspace is skipped, not
// an error: `datatable_itemopt.xml` sits in the same folder and is a different
// table.
func LoadItemTables(dir string) (*ItemTable, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "datatable_item*.xml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths) // base file first ("datatable_item.xml" sorts before "_...")
	t := &ItemTable{
		byID:   make(map[uint32]ItemRow, 26000),
		byName: make(map[string]uint32, 26000),
		byRule: make(map[string][]uint32, 16),
	}
	read := 0
	for _, path := range paths {
		n, err := t.loadFile(path)
		if err != nil {
			return nil, err
		}
		read += n
	}
	if read == 0 {
		return nil, fmt.Errorf("%s holds no Item row (no datatable_item*.xml with "+
			"<idspace id=\"Item\">)", dir)
	}
	return t, nil
}

// loadFile reads ONE XML file if it is the Item idspace, and answers how many rows
// it added.
func (t *ItemTable) loadFile(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	dec := xml.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	added := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return added, nil
		}
		if err != nil {
			return added, fmt.Errorf("%s: %w", path, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "idspace":
			id := ""
			for _, a := range start.Attr {
				if a.Name.Local == "id" {
					id = a.Value
				}
			}
			if id != "Item" {
				return 0, nil // another table that happens to share the prefix
			}
		case "Class":
			var r itemXMLRow
			if err := dec.DecodeElement(&r, &start); err != nil {
				return added, fmt.Errorf("%s: %w", path, err)
			}
			id, ok := atoi(r.ClassID)
			if !ok || id <= 0 {
				continue
			}
			row := ItemRow{
				ID:           uint32(id),
				Name:         strings.TrimSpace(r.ClassName),
				Lv:           atoiOr(r.ItemLv, 0),
				DropRule:     strings.TrimSpace(r.DropRule),
				Unstack:      strings.EqualFold(strings.TrimSpace(r.Unstack), "TRUE"),
				MaxStack:     int64(atoiOr(r.MaxStack, 0)),
				OptionGroup:  strings.TrimSpace(r.OptionGroup),
				DropRareFreq: atofOr(r.DropRareFreq, 0),
				GroupName:    strings.TrimSpace(r.GroupName),
				UseLv:        atoiOr(r.UseLv, 0),
				ScpType:      strings.TrimSpace(r.ScpType),
				BuffName:     strings.TrimSpace(r.BuffName),
			}
			// arg1 is optional (only the rows with a use script carry it), so it is
			// read with its own "present" flag: a row that names a script and has no
			// arg1 is a data gap the use verb must REPORT, not a 0-point effect.
			if s := strings.TrimSpace(r.Arg1); s != "" {
				if v, perr := strconv.ParseFloat(s, 64); perr == nil {
					row.Arg1 = v
					row.Arg1Known = true
				}
			}
			// arg2 is read the same way, for the same reason: a buff script passes it
			// straight on to `AddBuff`, so a dropped column would change the buff.
			if s := strings.TrimSpace(r.Arg2); s != "" {
				if v, perr := strconv.ParseFloat(s, 64); perr == nil {
					row.Arg2 = v
					row.Arg2Known = true
				}
			}
			if n, ok := atoi(r.Option1ID); ok && n > 0 {
				row.Option1ID = uint32(n)
			}
			t.byID[row.ID] = row
			if row.Name != "" {
				t.byName[strings.ToLower(row.Name)] = row.ID
			}
			if row.DropRule != "" {
				t.byRule[row.DropRule] = append(t.byRule[row.DropRule], row.ID)
			}
			added++
		}
	}
}

// mapXMLRow is the subset of a Map row the zone rule reads.
type mapXMLRow struct {
	ClassID     string `xml:"ClassID,attr"`
	Type        string `xml:"Type,attr"`
	DropItem1ID string `xml:"DropItem1ID,attr"`
	DropItem1F  string `xml:"DropItem1Freq,attr"`
	DropItem2ID string `xml:"DropItem2ID,attr"`
	DropItem2F  string `xml:"DropItem2Freq,attr"`
	DropItem3ID string `xml:"DropItem3ID,attr"`
	DropItem3F  string `xml:"DropItem3Freq,attr"`
	DropItem4ID string `xml:"DropItem4ID,attr"`
	DropItem4F  string `xml:"DropItem4Freq,attr"`
	DropItem5ID string `xml:"DropItem5ID,attr"`
	DropItem5F  string `xml:"DropItem5Freq,attr"`
	DropItem6ID string `xml:"DropItem6ID,attr"`
	DropItem6F  string `xml:"DropItem6Freq,attr"`
	DropItem7ID string `xml:"DropItem7ID,attr"`
	DropItem7F  string `xml:"DropItem7Freq,attr"`
	// MonsterDropBlock is the map's `isMonsterDropBlocked` (MapTable.cpp:266-269).
	// It suppresses the BASIC and UNIQUE rules - not the zone rule.
	MonsterDropBlock string `xml:"MonsterDropBlock,attr"`
}

// MapDropRow is ONE map's zone-rule drops. The count is 7 because the reference's
// own `MAXDROPITEM` is 7 (geMapTable.h:85) and the shipped row carries seven.
type MapDropRow struct {
	Item    [7]uint32
	Freq    [7]float64
	Blocked bool // MonsterDropBlock="TRUE": basic and unique rules are suppressed
	// Type is the row's own `Type` column ("CITY", "MISSION", "BATTLEFIELD", ...).
	// It is read for the SP rule, not for a drop: SCR_Get_RSP forces ApplyRSPType to
	// 'regenerate' when mapType == 'CITY' (ge\script\pc\get_status_pc.scp:508,526-528),
	// and the zone already has this file loaded. One loader, so the drop rules and the
	// SP rule cannot disagree about which map they are on.
	Type string
}

// MapDropTable is datatable_map.xml reduced to map id -> its zone-rule drops.
type MapDropTable struct {
	rows map[int]MapDropRow
}

// Row answers one map's drop row, and false when the table has no such map.
func (t *MapDropTable) Row(mapID int) (MapDropRow, bool) {
	if t == nil {
		return MapDropRow{}, false
	}
	r, ok := t.rows[mapID]
	return r, ok
}

// Len is how many map rows the table holds, for the one log line.
func (t *MapDropTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rows)
}

// TypeOf answers one map's `Type` column ("CITY", "MISSION", ...). ok=false means the
// file has no such row; the caller decides what an unknown map type means, and the SP
// lane treats it as "the CITY term is not applied" rather than as "not a city".
func (t *MapDropTable) TypeOf(mapID int) (string, bool) {
	if t == nil {
		return "", false
	}
	r, ok := t.rows[mapID]
	if !ok {
		return "", false
	}
	return r.Type, true
}

// LoadMapDrops reads `datatable_map.xml` from `dir`.
func LoadMapDrops(dir string) (*MapDropTable, error) {
	path := filepath.Join(dir, "datatable_map.xml")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := &MapDropTable{rows: make(map[int]MapDropRow, 2400)}
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
		var r mapXMLRow
		if err := dec.DecodeElement(&r, &start); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		id, ok := atoi(r.ClassID)
		if !ok || id <= 0 {
			continue
		}
		var row MapDropRow
		pairs := [7][2]string{
			{r.DropItem1ID, r.DropItem1F}, {r.DropItem2ID, r.DropItem2F},
			{r.DropItem3ID, r.DropItem3F}, {r.DropItem4ID, r.DropItem4F},
			{r.DropItem5ID, r.DropItem5F}, {r.DropItem6ID, r.DropItem6F},
			{r.DropItem7ID, r.DropItem7F},
		}
		for i, p := range pairs {
			if n, ok := atoi(p[0]); ok && n > 0 {
				row.Item[i] = uint32(n)
			}
			row.Freq[i] = atofOr(p[1], 0)
		}
		row.Blocked = strings.EqualFold(strings.TrimSpace(r.MonsterDropBlock), "TRUE")
		row.Type = strings.TrimSpace(r.Type)
		t.rows[id] = row
	}
	if len(t.rows) == 0 {
		return nil, fmt.Errorf("%s holds no readable Map row", path)
	}
	return t, nil
}

// dropPenaltyXMLRow is the subset of a DropPenalty row the lookup reads:
// `Lv` (the KILLER's level) and six [MinLv, MaxLv, penalty] ranges keyed by the
// level gap `killedLv - killerLv` (`DropPenaltyTable.cpp:72-121`).
type dropPenaltyXMLRow struct {
	Lv       string `xml:"Lv,attr"`
	MinLv1   string `xml:"MinLv1,attr"`
	MaxLv1   string `xml:"MaxLv1,attr"`
	Penalty1 string `xml:"penalty1,attr"`
	MinLv2   string `xml:"MinLv2,attr"`
	MaxLv2   string `xml:"MaxLv2,attr"`
	Penalty2 string `xml:"penalty2,attr"`
	MinLv3   string `xml:"MinLv3,attr"`
	MaxLv3   string `xml:"MaxLv3,attr"`
	Penalty3 string `xml:"penalty3,attr"`
	MinLv4   string `xml:"MinLv4,attr"`
	MaxLv4   string `xml:"MaxLv4,attr"`
	Penalty4 string `xml:"penalty4,attr"`
	MinLv5   string `xml:"MinLv5,attr"`
	MaxLv5   string `xml:"MaxLv5,attr"`
	Penalty5 string `xml:"penalty5,attr"`
	MinLv6   string `xml:"MinLv6,attr"`
	MaxLv6   string `xml:"MaxLv6,attr"`
	Penalty6 string `xml:"penalty6,attr"`
}

type dropPenaltyRange struct {
	Min, Max int
	Penalty  float64
}

// DropPenaltyTable is datatable_droppenalty.xml: killer level -> six ranges.
//
// THE FILE IS A SERVER-SIDE EXPORT, not a client table: the client's `ge\ies\`
// folder has no datatable_droppenalty at all (`history\monster-death.md` §4). It is
// therefore OPTIONAL here: a build without it multiplies every frequency by 1.0 and
// the kill line says "penalty 1.00 (no table)".
type DropPenaltyTable struct {
	rows map[int][6]dropPenaltyRange
	// source is the export this table was read from, recorded so the image can remember
	// which of dropPenaltyPaths()'s candidates it was built from (zone\penalty_image.go).
	source string
}

// Penalty answers the multiplier for one kill. ok is false when the table has no
// row for the killer's level or no range holds the gap; the caller then uses 1.0,
// which is the reference's own fallback (`DropPenaltyTable.cpp:127-135`).
func (t *DropPenaltyTable) Penalty(killerLv, killedLv int) (float64, bool) {
	if t == nil {
		return 1, false
	}
	row, ok := t.rows[killerLv]
	if !ok {
		return 1, false
	}
	gap := killedLv - killerLv
	for _, r := range row {
		if r.Min <= gap && r.Max >= gap {
			return r.Penalty, true
		}
	}
	return 1, false
}

// Len is how many killer levels the table holds.
func (t *DropPenaltyTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rows)
}

// LoadDropPenalty reads one datatable_droppenalty.xml.
func LoadDropPenalty(path string) (*DropPenaltyTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	t := &DropPenaltyTable{rows: make(map[int][6]dropPenaltyRange, 128), source: path}
	dec := xml.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	// THE SHIPPED FILE DECLARES `encoding="EUC-KR"` (an XMLSpy export). Go's decoder
	// refuses any non-UTF-8 declaration unless a CharsetReader is installed, and the
	// cells this loader reads are ASCII numbers and tag names - the EUC-KR bytes are
	// in comments/annotations. Passing the bytes through unchanged is therefore
	// exact for everything read here; a real transcoder would need a dependency.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
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
		var r dropPenaltyXMLRow
		if err := dec.DecodeElement(&r, &start); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		lv, ok := atoi(r.Lv)
		if !ok || lv <= 0 {
			continue
		}
		var row [6]dropPenaltyRange
		mins := [6]string{r.MinLv1, r.MinLv2, r.MinLv3, r.MinLv4, r.MinLv5, r.MinLv6}
		maxs := [6]string{r.MaxLv1, r.MaxLv2, r.MaxLv3, r.MaxLv4, r.MaxLv5, r.MaxLv6}
		pens := [6]string{r.Penalty1, r.Penalty2, r.Penalty3, r.Penalty4, r.Penalty5, r.Penalty6}
		for i := 0; i < 6; i++ {
			row[i] = dropPenaltyRange{
				Min:     atoiOr(mins[i], 0),
				Max:     atoiOr(maxs[i], 0),
				Penalty: atofOr(pens[i], 1),
			}
		}
		t.rows[lv] = row
	}
	if len(t.rows) == 0 {
		return nil, fmt.Errorf("%s holds no readable DropPenalty row", path)
	}
	return t, nil
}

// dropPenaltyPaths is where the penalty export may be, in the order tried.
//
// THE FIRST ENTRY IS AN OVERRIDE ON PURPOSE: the file is not part of the client, so
// a deployed bundle has to be able to point at its own copy. The repository paths
// are the two working directories this code runs from (goserver\ and goserver\zone\),
// the same shape XMLDir uses.
func dropPenaltyPaths() []string {
	if v := os.Getenv("GE_DROP_PENALTY"); v != "" {
		return []string{v}
	}
	return []string{
		filepath.Join("..", "..", "ge", "server_xml", "datatable_droppenalty.xml"),
		filepath.Join("..", "..", "..", "ge", "server_xml", "datatable_droppenalty.xml"),
		filepath.Join("..", "ge", "server_xml", "datatable_droppenalty.xml"),
		filepath.Join("ge", "server_xml", "datatable_droppenalty.xml"),
	}
}

// atoiOr already exists in create_tables.go; this file reuses it.
