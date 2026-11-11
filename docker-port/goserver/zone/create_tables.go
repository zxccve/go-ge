package zone

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// THE TWO TABLES A RUNTIME SPAWN NEEDS.
//
// `CreateTNPC` names an NPC by its table ClassName and `CreateTmonster` names a
// monster by its ClassID, and in both cases the client needs NUMBERS the script
// never passes: the body's class id, the level, the EXP, the tactics. The
// original reads them from the client's own IES tables (`geNPCTable::Get`,
// `imcIES::GetClass("Monster", id)`); so does this.
//
// Both loaders read the SAME files the rest of the zone already reads
// (`Handler.Stances`, `LoadWarpTypes` take the same `-xml` folder):
//
//	datatable_npc.xml         1,751 NPC rows     ClassName -> ClassID
//	datatable_monster*.xml    7,725 monster rows ClassID -> level, EXP, tactics
//
// A MISSING FILE IS NOT FATAL. A zone without these tables still serves the map,
// its town NPCs and its clicks; it simply REFUSES a runtime spawn with a log line
// naming the table. That is the same rule the warp table follows, and it is
// better than a start-up failure on a map that never creates anything.

// NPCTypeTable is `datatable_npc.xml` reduced to what a spawn needs: the ClassID
// behind a ClassName.
//
// The row's `Size` is deliberately NOT here: it selects the collision shape the
// original's path engine uses for the actor (`geShapeTable::Get(npcProperty->size)`).
// This lane places an NPC where the script asked and draws it; nothing walks it.
type NPCTypeTable struct {
	byName map[string]uint32
}

// ClassID answers the ClassID of one NPC ClassName ('npc_tbk_contbox_off' ->
// 91024), and false when the table has no such row.
func (t *NPCTypeTable) ClassID(name string) (uint32, bool) {
	if t == nil {
		return 0, false
	}
	id, ok := t.byName[name]
	return id, ok
}

// Len is how many rows the table holds, for the startup line.
func (t *NPCTypeTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byName)
}

// MonsterRow is one row of the Monster table, reduced to what a spawn and its log
// line use.
type MonsterRow struct {
	ID   uint32
	Name string
	// Level and Exp are the row's own values. The Scarecrow rows (11600-11614) are
	// Lv 130, EXP 0 - a monster that pays nothing, which is what a damage dummy
	// must be.
	Level int
	Exp   float64
	// Tactics is the row's AI name. The script overrides it; no AI is built in this
	// lane either way.
	Tactics string
	// MHP is the row's fixed HP when the table carries one (`MHP` column), which is
	// 0 for the Scarecrow: its HP is the SCRIPT's `mhpFix` (the player's HP x 100).
	MHP int64
	// Size is the row's collision body ("S", "M", "L", "XL"), and Radius is that name
	// put through the reference's own four shapes (shapeBodyRadius). Both are kept
	// because an attack's reach is measured EDGE TO EDGE (R91 round 5): a reach test
	// that ignores the target's body refuses hits the game lands.
	Size   string
	Radius float64

	// THE DEFENCE COLUMNS (R84 slice 2): exactly the row attributes the game's own
	// `SCR_Get_MON_DEF` (monster\get_status_monster.lua:481-505) and the damage rule
	// (common_monster.lua:2884-2896) read. They are copied verbatim - no arithmetic
	// happens at load time, because the SCRIPT is what owns the formula and a second
	// implementation here would drift from it.
	//
	//	RaceType   'Home' answers DEF 9999 before any arithmetic
	//	Arm        the multiplier in floor((Lv * 0.05 + 1) * Arm)
	//	DEFBM      added after that floor
	//	DEFPERBM   the percent applied after the add: * (100 + DEFPERBM)/100
	//	IMPBM      SCR_Get_MON_IMP = floor(IMP_BM), the immunity the rule subtracts
	//	SizeType   'SMALL'/'MEDIUM'/'LARGE', read by the rule's SmallBane branch
	//	           (NOT `Size`, which is the collision body the reach test uses)
	RaceType string
	Arm      float64
	DEFBM    float64
	DEFPERBM float64
	IMPBM    float64
	SizeType string

	// THE RANK / LAYER / RESIST COLUMNS (this slice): exactly the row attributes the
	// game's own computed properties read, which the buff message script then reads
	// back off the defender:
	//
	//	MobRank     SCR_Get_MON_DR      = Lv/2 + MobRank - 1 + DR_BM   (:93-101)
	//	DRBM        the DR_BM term of the same line
	//	DefLayer    SCR_Get_MON_DefLayer = DefLayer + DefLayer_BM, clamped (:263-274)
	//	DefLayerBM  the DefLayer_BM term of the same line
	//	RSTAT       SCR_Get_MON_RSTAT   = RSTAT + RSTAT_BM, clamped 0..100 (:385-397)
	//	RSTATBM     the RSTAT_BM term of the same line
	//
	// THE COLUMNS ARE NAMES AS OFTEN AS NUMBERS: 3,074 of the 7,725 rows carry
	// `DefLayer="SCR_Get_MON_DefLayer"`, which is the ENGINE's own computed-property
	// marker (MonsterNPC.h:143). Those rows are read as 0 and the entry point is run
	// for them like every other - that is why the value comes from the script and
	// never from this column directly.
	MobRank   float64
	DRBM      float64
	DefLayer  float64
	DefLayerBM float64
	RSTAT     float64
	RSTATBM   float64

	// THE MAX-HP COLUMNS: exactly the row attributes the game's own
	// `SCR_Get_MON_MHP` (monster\get_status_monster.lua:13-64) reads from the ROW.
	// Copied verbatim for the same reason as the defence block above - the script
	// owns the formula.
	//
	//	CON        the body's constitution: the `(self.CON / 5)` factor of the
	//	           ordinary branch, and `CON * 1000` in the fixed-class branch
	//	MHPFix     a percent added AFTER the branch: * (MHPFix * 0.01 + 1)
	//	MHPBM      a percent applied inside the branch: * (100 + MHP_BM)/100
	//	ADDMHPBM   a flat amount added at the END, outside every branch
	//
	// The table has NO `MHP` column at all (`HP`, `RHP`, `MHPFix`, `MHP_BM`,
	// `CP_MON_MHP` exist; `MHP` does not), which is why `MonsterRow.MHP` is 0 for
	// all 7,725 rows and a spawn that passes no MHP must ask the script.
	CON      float64
	MHPFix   float64
	MHPBM    float64
	ADDMHPBM float64

	// THE ATTACK COLUMNS (the monster swing), read from the same row and never
	// invented. The game's own `SCR_Get_MON_DAM` (get_status_monster.lua:398-471)
	// computes the monster's ATK from `Wpn`, `STR`, `Lv` and `ATK_BM`; its reach is
	// `MinR`/`MaxR` (MonsterNPC.cpp:1035-1046) and its cadence is `ASPD` adjusted by
	// `ASPD_BM` (SCR_Get_MON_ASPD, get_status_monster.lua:496-521).
	//
	// `Hits` is NOT a column of the Monster table: it is a column of
	// `datatable_monster_shape.xml`, the row the monster's own `Shape` names, and the
	// ATK formula divides by it. It is copied here at load time so the swing has the
	// divisor the game's formula uses; a row whose shape names no Hits keeps 0 and
	// then has no ATK (monsterATK), rather than a divisor this server guessed.
	Wpn    float64
	STR    float64
	ATKBM  float64
	MinR   float64
	MaxR   float64
	ASPD   float64
	ASPDBM float64
	Hits   float64

	// THE MOVE SPEED (the monster chase, zone\monster_chase.go): the three columns
	// the game's own `SCR_Get_MON_MSPD` reads, copied verbatim for the same reason
	// as every other script input here - the formula lives in one place
	// (`monsterMSPD`), the row supplies its numbers.
	//
	// THIS IS THE CHASE'S SPEED, and it is NOT WSPD or RSPD. `CMonsterNPC::
	// GetMoveSPD` answers `GetMON_MSPD()` (MonsterNPC.cpp:1929-1932), the move FSM
	// stamps that into `m_moveSpeed` before it broadcasts ZC_MOVE_MONSTER
	// (FieldChar_FSM.cpp:599-603). WSPD/RSPD are animation-side columns the client
	// reads; they never reach the packet.
	MSPD    float64
	MSPDFix float64
	MSPDBM  float64

	// SPDBM is the row's own RUN-SPEED column, and it is a DIFFERENT column from
	// `MSPD_BM` above (class 10093 Boar: MSPD_BM 0, SPDBM 298). It is the whole
	// percent a body's run adds to MSPD:
	//
	//	runSpeed = floor(MSPD * (MSPDFix*0.01 + 1) * (100 + SPDBM) / 100)
	//
	// measured 2026-11-08 on five live bodies (history\combat.md, "A monster's RUN
	// speed is the row's SPDBM"). A body with an engagement - a chase or an attack
	// order in flight - moves at that number; a body with none keeps MSPD.
	SPDBM float64

	// AIType is the monster-AI FAMILY this row's `Tactics` names, resolved at load
	// time through `datatable_monsterai.xml` (`MON_AGGRESSIVE` -> `AGGRESSIVE`). It
	// is what decides whether the row's body engages at all: only the AGGRESSIVE
	// family chases (zone\monster_chase.go). A tactics name the table does not hold
	// answers "", and then the body never chases.
	//
	// AIKnown says the tactics row RESOLVED. It separates "the file says this
	// monster does not attack" from "there is no tactics row to ask", which are two
	// different answers: the first forbids the swing (AIAttack false), the second
	// leaves the body's own ATK/MaxR to decide, which is the behaviour before the
	// AI table was read.
	//
	// AIATTACK and SEEKRANGE are the same tactics row's `Attack` and `SeekRange`
	// columns - the two the reference's own AI script reads to decide whether the
	// body swings and how far it looks for a target (monster\mon_ts_none.scp:7-13,
	// 223-240: `GetMonAiOption(self,'SeekRange')` -> `GetObjectLocaly`, and
	// `'TRUE' == Attack` -> `ChangeTactics TS_BATTLE`).
	AIType    string
	AIKnown   bool
	AIAttack  bool
	SeekRange float64

	// DyingTime is the SHAPE row's `DyingTime`, in milliseconds - the length the
	// client's own death animation runs for that shape (`DIE1` / `DN` in the same
	// row; `Monster_Bigfoot` says 0, a shape that lingers says e.g. 3500). It is
	// read from `datatable_monster_shape.xml` through the monster row's `Shape`,
	// exactly like `Hits` above, because the Monster table does not carry it.
	//
	// IT IS THE SHAPE'S NUMBER, NOT A GUESS: the client plays that animation itself
	// and needs the body to still EXIST while it runs, so the corpse's leave waits
	// for it (`zone/respawn.go`, CorpseLeaveFloor). 0 is a real answer - the shape
	// has no lingering death - and the zone then keeps the body for its floor.
	//
	// ShapeKnown says the join RESOLVED at all. False means there is no shape row to
	// ask: the leave falls back to the floor and the zone says so once, because that
	// is a missing input and not a shape that states 0.
	DyingTime  time.Duration
	ShapeKnown bool

	// THE DROP COLUMNS (R98 slice 3, `history\monster-death.md` §4): the eight
	// basic-rule frequencies, the crystal count, and the six unique trophies. They
	// are the row's own numbers, copied verbatim - the 1-in-N arithmetic lives in
	// zone\drop.go, next to the roll, not here.
	//
	// THE VALUES ARE FLOATS BECAUSE THE REFERENCE MULTIPLIES THEM BY THE PENALTY
	// BEFORE ROUNDING (`(float)monProp->dropOldEqpFreq * m_dropPenalty`,
	// MonsterDropItem.cpp:228-262); rounding at load time would move every
	// 0.8-penalty roll on a small N by one.
	DropOldEqpFreq   float64
	DropNormEqpFreq  float64
	DropEliteEqpFreq float64
	DropAccFreq      float64
	DropEtcFreq      float64
	DropInstFreq     float64
	DropRecipeFreq   float64
	DropCrystalFreq  float64
	DropCrystalCount int
	Trophies         [6]DropTrophy
}

// DropTrophy is ONE of a monster row's six static/unique drop slots
// (`Drop1ID`/`Drop1Per`/`Drop1Quant` .. `Drop6*`, MonsterDropItem.cpp:288-305).
//
// Per is 1-in-N, never a percent, and Quant is the MAXIMUM count: the reference
// rolls `RandomInt(1, Quant)` and only a Quant of 1 skips that roll.
type DropTrophy struct {
	ID    uint32
	Per   float64
	Quant int
}

// BodyRadius is the collision radius this row's body gives the edge-to-edge reach. A
// row that names no size the shape table knows answers 0 - "unknown", never a guess.
func (r MonsterRow) BodyRadius() float64 {
	if r.Radius > 0 {
		return r.Radius
	}
	return shapeBodyRadius(r.Size)
}

// THE BODIES' RADII, AND WHERE THE NUMBERS COME FROM.
//
// The reference measures an attack's reach edge to edge, not centre to centre:
//
//	float len = GetDistance(m_pos, pos) - (GetDomainRadius() + domainSize);
//	                                    source/geServer/ZServerLib/FieldChar.cpp:118
//
// so both radii are part of the arithmetic and must be real numbers. `GetDomainRadius()`
// is the actor's own collision body (`FieldChar.cpp:562-565` returns `m_shapeSize`, set
// from `geShapeTable::GetInfo(GetSize())->size` at `FieldChar.cpp:181-185`), and that
// table is four fixed shapes:
//
//	S 50   M 100   L 200   XL 400     source/geShared/Source/ShapeTable.cpp:47-63
//
// A player IS the S body: `geShapeTable::Get(ST_SMALL)` is what both of the reference's
// own attack stand points fall back to when an actor's shape is missing
// (`PlayerChar.cpp:1371-1374` and `:1394-1397`), i.e. 50.
const PCBodyRadius = 50.0

// shapeBodyRadius turns a table's `Size` name into the collision radius the reference
// gives that body. 0 means "this row does not say" - the caller must treat that as an
// unknown half, never as a body of no width.
func shapeBodyRadius(size string) float64 {
	switch strings.ToUpper(strings.TrimSpace(size)) {
	case "S":
		return 50
	case "M":
		return 100
	case "L":
		return 200
	case "XL":
		return 400
	}
	return 0
}

// MonsterAIRow is one row of `datatable_monsterai.xml`: the three columns the zone
// acts on, out of the file's twenty-six. It is the monster's TACTICS, keyed by the
// name a Monster row's `Tactics` carries.
//
// THE REFERENCE'S OWN AI SCRIPT READS THE SAME THREE (ge\script\monster\
// mon_ts_none.scp): `SeekRange` is the radius it hands `GetObjectLocaly` to find a
// target, `Attack` gates `ChangeTactics TS_BATTLE`, and `AIType` names the family
// (the script's custom hooks and the turret branch key off it).
type MonsterAIRow struct {
	AIType    string
	Attack    bool
	SeekRange float64
}

// MonsterTable is every `datatable_monster*.xml` row, by ClassID.
type MonsterTable struct {
	byID map[uint32]MonsterRow

	// aiRows is `datatable_monsterai.xml` by tactics NAME: `MON_AGGRESSIVE` ->
	// {AGGRESSIVE, attack true, seek 2500}. It is loaded best effort; a nil map
	// means the file was not readable, and then no row resolves an AI row and no
	// monster chases.
	//
	// WHY THE TACTICS TABLE AND NOT A PER-MONSTER COLUMN: the Monster table has no
	// AI-profile column (the brief's own evidence), so the tactics NAME is the only
	// key the row carries. The row it names is what the chase and the swing ask
	// about.
	aiRows map[string]MonsterAIRow
}

// AI answers the tactics row a tactics name names, and false when the table does
// not know it.
func (t *MonsterTable) AI(tactics string) (MonsterAIRow, bool) {
	if t == nil || t.aiRows == nil {
		return MonsterAIRow{}, false
	}
	ai, ok := t.aiRows[strings.TrimSpace(tactics)]
	return ai, ok
}

// AI answers the tactics row this monster row resolved, as one value the chase can
// ask about. It is the same three columns `LoadMonsters` copied onto the row, so a
// caller never has to remember to pass them separately.
func (r MonsterRow) AI() MonsterAIRow {
	return MonsterAIRow{AIType: r.AIType, Attack: r.AIAttack, SeekRange: r.SeekRange}
}

// Row answers one monster row, and false when the table has no such id.
func (t *MonsterTable) Row(id uint32) (MonsterRow, bool) {
	if t == nil {
		return MonsterRow{}, false
	}
	r, ok := t.byID[id]
	return r, ok
}

// Len is how many rows the table holds, for the startup line.
func (t *MonsterTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byID)
}

// xmlRow is the shared shape of both tables' rows: the client's files are all
// `<idspace><Class ClassID=".." ClassName=".." Attr=".." .../></idspace>`.
type xmlRow struct {
	XMLName   xml.Name `xml:"Class"`
	ClassID   string   `xml:"ClassID,attr"`
	ClassName string   `xml:"ClassName,attr"`
	Lv        string   `xml:"Lv,attr"`
	EXP       string   `xml:"EXP,attr"`
	Tactics   string   `xml:"Tactics,attr"`
	MHP       string   `xml:"MHP,attr"`
	Size      string   `xml:"Size,attr"`
	// The defence columns (see MonsterRow). Absent on a row that does not state
	// them, which reads "" - the same tolerant column read every other attribute
	// here uses.
	RaceType string `xml:"RaceType,attr"`
	SizeType string `xml:"SizeType,attr"`
	Arm      string `xml:"Arm,attr"`
	DEFBM    string `xml:"DEF_BM,attr"`
	DEFPERBM string `xml:"DEFPER_BM,attr"`
	IMPBM    string `xml:"IMP_BM,attr"`
	// THE RANK / LAYER / RESIST COLUMNS (see MonsterRow). `DefLayer` may be the word
	// `SCR_Get_MON_DefLayer` rather than a number - the engine's computed-property
	// marker - which reads 0 here and is answered by the entry point instead.
	MobRank    string `xml:"MobRank,attr"`
	DRBM       string `xml:"DR_BM,attr"`
	DefLayer   string `xml:"DefLayer,attr"`
	DefLayerBM string `xml:"DefLayer_BM,attr"`
	RSTAT      string `xml:"RSTAT,attr"`
	RSTATBM    string `xml:"RSTAT_BM,attr"`
	// The max-HP columns (see MonsterRow). Absent on a row that does not state
	// them, which reads "" - the same tolerant read every other attribute uses.
	CON      string `xml:"CON,attr"`
	MHPFix   string `xml:"MHPFix,attr"`
	MHPBM    string `xml:"MHP_BM,attr"`
	ADDMHPBM string `xml:"ADDMHP_BM,attr"`

	// The attack columns (see MonsterRow). `Shape` is the row's shape NAME, the key
	// into datatable_monster_shape.xml where `Hits` lives.
	Wpn    string `xml:"Wpn,attr"`
	STR    string `xml:"STR,attr"`
	ATKBM  string `xml:"ATK_BM,attr"`
	MinR   string `xml:"MinR,attr"`
	MaxR   string `xml:"MaxR,attr"`
	ASPD   string `xml:"ASPD,attr"`
	ASPDBM string `xml:"ASPD_BM,attr"`
	Shape  string `xml:"Shape,attr"`
	Hits   string `xml:"Hits,attr"`

	// THE MOVE-SPEED COLUMNS (the monster chase, zone\monster_chase.go): exactly
	// the three attributes SCR_Get_MON_MSPD reads (monster\get_status_monster.lua:
	// 535-547). MSPD is the body's movement speed; MSPDFix and MSPD_BM are the two
	// modifiers the script applies to it.
	MSPD    string `xml:"MSPD,attr"`
	MSPDFix string `xml:"MSPDFix,attr"`
	MSPDBM  string `xml:"MSPD_BM,attr"`
	// SPDBM is the RUN-speed column (see MonsterRow). It is the Monster table's own
	// attribute of exactly that name; the AI rows this decoder also reads do not
	// carry it, and there it reads "" like every other absent attribute.
	SPDBM string `xml:"SPDBM,attr"`

	// AIType is `datatable_monsterai.xml`'s column (see MonsterTable.AI): the
	// monster-AI FAMILY a tactics name belongs to (`MON_AGGRESSIVE` answers
	// `AGGRESSIVE`). Attack and SeekRange are the same file's other two columns the
	// zone acts on. They are read off that file's rows with the same decoder.
	AIType    string `xml:"AIType,attr"`
	Attack    string `xml:"Attack,attr"`
	SeekRange string `xml:"SeekRange,attr"`

	// DyingTime is the shape table's own column (see MonsterRow.DyingTime); it is
	// read off the SAME `<Class>` row `Hits` comes from, so one decode serves both.
	DyingTime string `xml:"DyingTime,attr"`

	// THE DROP COLUMNS (R98 slice 3, `history\monster-death.md` §4). Read here
	// because they are columns of the SAME row `CMonsterTable::AddProperty` reads
	// (`MonsterTable.cpp:80-108`); a second parser would be a second answer.
	//
	// THE EIGHT FREQUENCIES ARE 1-IN-N, NOT PERCENT. `GetItemRandom` divides N by
	// the world's item reward BM and the server's item ratio, floors it at 1, and
	// then asks `RandomInt(1, N) == 1` (`MonsterDropItem.cpp:344-358`); with no
	// reward BM or item ratio configured in this build both divisors are 1 and the
	// row's number IS N. See zone\drop.go.
	DropOldEqpFreq   string `xml:"DropOldEqpFreq,attr"`
	DropNormEqpFreq  string `xml:"DropNormEqpFreq,attr"`
	DropEliteEqpFreq string `xml:"DropEliteEqpFreq,attr"`
	DropAccFreq      string `xml:"DropAccFreq,attr"`
	DropEtcFreq      string `xml:"DropEtcFreq,attr"`
	DropInstFreq     string `xml:"DropInstFreq,attr"`
	DropRecipeFreq   string `xml:"DropRecipeFreq,attr"`
	DropCrystalFreq  string `xml:"DropCrystalFreq,attr"`
	DropCrystalCount string `xml:"DropCrystalCount,attr"`

	// The six unique / static trophies: an item id, its 1-in-N and its quantity
	// (`DropItemByUniqueRule`, `MonsterDropItem.cpp:288-305`).
	Drop1ID    string `xml:"Drop1ID,attr"`
	Drop1Per   string `xml:"Drop1Per,attr"`
	Drop1Quant string `xml:"Drop1Quant,attr"`
	Drop2ID    string `xml:"Drop2ID,attr"`
	Drop2Per   string `xml:"Drop2Per,attr"`
	Drop2Quant string `xml:"Drop2Quant,attr"`
	Drop3ID    string `xml:"Drop3ID,attr"`
	Drop3Per   string `xml:"Drop3Per,attr"`
	Drop3Quant string `xml:"Drop3Quant,attr"`
	Drop4ID    string `xml:"Drop4ID,attr"`
	Drop4Per   string `xml:"Drop4Per,attr"`
	Drop4Quant string `xml:"Drop4Quant,attr"`
	Drop5ID    string `xml:"Drop5ID,attr"`
	Drop5Per   string `xml:"Drop5Per,attr"`
	Drop5Quant string `xml:"Drop5Quant,attr"`
	Drop6ID    string `xml:"Drop6ID,attr"`
	Drop6Per   string `xml:"Drop6Per,attr"`
	Drop6Quant string `xml:"Drop6Quant,attr"`
}

// LoadNPCTypes reads `datatable_npc.xml` from the client's xml folder.
//
// A row whose ClassID is not a number is SKIPPED and counted, not fatal: the file
// is the client's, one bad row must not cost the zone its NPCs, and the count is
// in the startup line so a lower-than-expected number is visible.
func LoadNPCTypes(dir string) (*NPCTypeTable, error) {
	path := filepath.Join(dir, "datatable_npc.xml")
	rows, err := readClassRows(path)
	if err != nil {
		return nil, err
	}
	table := &NPCTypeTable{byName: make(map[string]uint32, len(rows))}
	var bad int
	for _, r := range rows {
		if r.ClassName == "" {
			continue
		}
		id, perr := strconv.ParseUint(strings.TrimSpace(r.ClassID), 10, 32)
		if perr != nil {
			bad++
			continue
		}
		table.byName[r.ClassName] = uint32(id)
	}
	if table.Len() == 0 {
		return nil, fmt.Errorf("%s holds no readable NPC row", path)
	}
	if bad > 0 {
		return table, fmt.Errorf("%s: %d row(s) skipped, their ClassID is not a "+
			"number", path, bad)
	}
	return table, nil
}

// LoadMonsters reads every `datatable_monster*.xml` in the client's xml folder.
//
// It reads the base file and the numbered ones (`_1` .. `_4`), which is how the
// client splits one table across files; miss one and a class id resolves to
// nothing for no visible reason. A missing base file is an error; a missing
// numbered part is not (a client with one file is still a client).
func LoadMonsters(dir string) (*MonsterTable, error) {
	base := filepath.Join(dir, "datatable_monster.xml")
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("%s: %w", base, err)
	}
	files := []string{base}
	for i := 1; i <= monsterTableParts; i++ {
		part := filepath.Join(dir, fmt.Sprintf("datatable_monster_%d.xml", i))
		if _, err := os.Stat(part); err != nil {
			continue
		}
		files = append(files, part)
	}

	table := &MonsterTable{
		byID:   make(map[uint32]MonsterRow, 8000),
		aiRows: loadMonsterAIRows(dir),
	}
	// THE SHAPE TABLE, for TWO columns: `Hits`, which the game's own ATK formula divides
	// by (`SCR_Get_MON_DAM`: `Wpn * (STR/5) * (Lv*0.05+1) / Hits * 1.8`), and `DyingTime`,
	// which is how long the client's own death animation runs for that shape - the corpse
	// stays in the world that long (zone\respawn.go). Neither is a Monster column: the
	// Monster table's 253 columns have no `Hits` and no `DyingTime`; the shape row the
	// monster's `Shape` names does. Best effort: without the file a monster keeps Hits 0
	// (no ATK, the honest answer for a missing divisor) and DyingTime unknown (the leave
	// falls back to its floor and says so once).
	shapes := loadMonsterShapes(dir)
	for _, path := range files {
		rows, err := readClassRows(path)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			id, perr := strconv.ParseUint(strings.TrimSpace(r.ClassID), 10, 32)
			if perr != nil {
				continue
			}
			row := MonsterRow{
				ID:      uint32(id),
				Name:    r.ClassName,
				Level:   atoiOr(r.Lv, 0),
				Tactics: r.Tactics,
				MHP:     int64(atofOr(r.MHP, 0)),
				Size:    r.Size,
				Radius:  shapeBodyRadius(r.Size),

				RaceType: r.RaceType,
				SizeType: r.SizeType,
				Arm:      atofOr(r.Arm, 0),
				DEFBM:    atofOr(r.DEFBM, 0),
				DEFPERBM: atofOr(r.DEFPERBM, 0),
				IMPBM:    atofOr(r.IMPBM, 0),

				MobRank:    atofOr(r.MobRank, 0),
				DRBM:       atofOr(r.DRBM, 0),
				DefLayer:   atofOr(r.DefLayer, 0),
				DefLayerBM: atofOr(r.DefLayerBM, 0),
				RSTAT:      atofOr(r.RSTAT, 0),
				RSTATBM:    atofOr(r.RSTATBM, 0),

				CON:      atofOr(r.CON, 0),
				MHPFix:   atofOr(r.MHPFix, 0),
				MHPBM:    atofOr(r.MHPBM, 0),
				ADDMHPBM: atofOr(r.ADDMHPBM, 0),

				Wpn:    atofOr(r.Wpn, 0),
				STR:    atofOr(r.STR, 0),
				ATKBM:  atofOr(r.ATKBM, 0),
				MinR:   atofOr(r.MinR, 0),
				MaxR:   atofOr(r.MaxR, 0),
				ASPD:   atofOr(r.ASPD, 0),
				ASPDBM: atofOr(r.ASPDBM, 0),

				// THE CHASE'S SPEED INPUTS and the tactics row, both read off
				// this same row / its tactics row. See MonsterRow.
				MSPD:    atofOr(r.MSPD, 0),
				MSPDFix: atofOr(r.MSPDFix, 0),
				MSPDBM:  atofOr(r.MSPDBM, 0),
				SPDBM:   atofOr(r.SPDBM, 0),
			}
			// THE TACTICS ROW: `Tactics` names a row of datatable_monsterai.xml, and
			// that row - not this one - says which family the body belongs to, whether
			// it attacks at all, and how far it looks for a target. A name the file
			// does not hold leaves AIKnown false and every field zero.
			if ai, ok := table.AI(r.Tactics); ok {
				row.AIKnown = true
				row.AIType = ai.AIType
				row.AIAttack = ai.Attack
				row.SeekRange = ai.SeekRange
			}
			// THE SHAPE JOIN: one lookup through the row's `Shape` feeds BOTH shape
			// columns - `Hits` for the swing and `DyingTime` for the corpse's leave.
			// A shape row that names no Hits still answers DyingTime, which is why the
			// join is recorded even when Hits comes back 0.
			if sh, ok := shapes[strings.TrimSpace(r.Shape)]; ok {
				row.ShapeKnown = true
				row.Hits = sh.hits
				row.DyingTime = sh.dying
			}
			if row.Hits == 0 {
				// The shape's own ClassID is the same idspace as the Monster's in the rows
				// checked (19763 -> 19763), so a Monster row whose `Shape` name is misspelt
				// can still find its shape by id.
				if sh, ok := shapes[strings.TrimSpace(r.ClassID)]; ok {
					row.Hits = sh.hits
					if !row.ShapeKnown {
						row.ShapeKnown = true
						row.DyingTime = sh.dying
					}
				}
			}
			row.Exp = atofOr(r.EXP, 0)

			// THE DROP COLUMNS, copied straight off the row. A missing column
			// reads 0, which is the reference's own answer for an absent
			// property and means "this rule never fires" - never a guessed
			// frequency.
			row.DropOldEqpFreq = atofOr(r.DropOldEqpFreq, 0)
			row.DropNormEqpFreq = atofOr(r.DropNormEqpFreq, 0)
			row.DropEliteEqpFreq = atofOr(r.DropEliteEqpFreq, 0)
			row.DropAccFreq = atofOr(r.DropAccFreq, 0)
			row.DropEtcFreq = atofOr(r.DropEtcFreq, 0)
			row.DropInstFreq = atofOr(r.DropInstFreq, 0)
			row.DropRecipeFreq = atofOr(r.DropRecipeFreq, 0)
			row.DropCrystalFreq = atofOr(r.DropCrystalFreq, 0)
			row.DropCrystalCount = atoiOr(r.DropCrystalCount, 0)
			trophy := func(id, per, quant string) DropTrophy {
				n, _ := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
				return DropTrophy{
					ID:    uint32(n),
					Per:   atofOr(per, 0),
					Quant: atoiOr(quant, 0),
				}
			}
			row.Trophies = [6]DropTrophy{
				trophy(r.Drop1ID, r.Drop1Per, r.Drop1Quant),
				trophy(r.Drop2ID, r.Drop2Per, r.Drop2Quant),
				trophy(r.Drop3ID, r.Drop3Per, r.Drop3Quant),
				trophy(r.Drop4ID, r.Drop4Per, r.Drop4Quant),
				trophy(r.Drop5ID, r.Drop5Per, r.Drop5Quant),
				trophy(r.Drop6ID, r.Drop6Per, r.Drop6Quant),
			}
			table.byID[row.ID] = row
		}
	}
	if table.Len() == 0 {
		return nil, fmt.Errorf("%s holds no readable Monster row", base)
	}
	return table, nil
}

// monsterTableParts is how many numbered parts the shipped Monster table has
// (datatable_monster.xml + _1 .. _4 = 7,725 rows). Reading more than exist is
// harmless - each one is stat'ed first.
const monsterTableParts = 4

// monsterShapeFile is where the monster's SHAPE row lives. The game's own
// `SCR_Get_MON_DAM` divides by `self.Hits`, and `Hits` is a column of THIS table,
// never of the Monster table (the Monster IES has 253 columns and no `Hits`).
const monsterShapeFile = "datatable_monster_shape.xml"

// loadMonsterShapes answers shape name (and ClassID) -> the row's `Hits` and
// `DyingTime`.
//
// BEST EFFORT ON PURPOSE: a client without the file, or a row without the column,
// leaves that monster's Hits 0 (and `monsterATK` then answers 0 - a monster that
// cannot attack, the honest answer for a missing divisor) and its DyingTime
// unknown, so the corpse's leave uses its floor and the zone says so once. The
// shape row's own `ClassID` is registered beside its name because the Monster
// rows and the shape rows share one idspace (the same fallback `Hits` has always
// had, for a misspelt `Shape`).
func loadMonsterShapes(dir string) map[string]monsterShape {
	path := filepath.Join(dir, monsterShapeFile)
	rows, err := readClassRows(path)
	if err != nil {
		return nil
	}
	out := make(map[string]monsterShape, len(rows))
	for _, r := range rows {
		var sh monsterShape
		if hits, err := strconv.ParseFloat(strings.TrimSpace(r.Hits), 64); err == nil && hits > 0 {
			sh.hits = hits
		}
		// DyingTime is milliseconds. A row that states 0 states it on purpose (the
		// client has no lingering death for that shape); a row that cannot be parsed
		// leaves the zero value, which the corpse treats as "use the floor".
		if ms, err := strconv.Atoi(strings.TrimSpace(r.DyingTime)); err == nil && ms >= 0 {
			sh.dying = time.Duration(ms) * time.Millisecond
		}
		if name := strings.TrimSpace(r.ClassName); name != "" {
			out[name] = sh
		}
		if id := strings.TrimSpace(r.ClassID); id != "" {
			out[id] = sh
		}
	}
	return out
}

// monsterShape is the two columns of `datatable_monster_shape.xml` this server
// reads, as one decoded row.
type monsterShape struct {
	hits  float64
	dying time.Duration
}

// monsterAIFile is the monster TACTICS table: one row per `Tactics` name the
// Monster table uses, and the `AIType` family it belongs to. The chase asks it
// because the Monster table has no AI column of its own.
const monsterAIFile = "datatable_monsterai.xml"

// loadMonsterAIRows answers tactics name -> its row (`MON_AGGRESSIVE` ->
// {AGGRESSIVE, true, 2500}).
//
// BEST EFFORT, exactly like the shape table: a client without the file leaves the
// map nil, every row resolves no AI row, and NO monster chases - a body that
// stands and swings, which is the behaviour this server had before the chase and
// is the honest answer for a missing tactics table. It also leaves every body's
// Attack question unanswered, so the swing keeps its own ATK/MaxR gate rather than
// reading a missing file as `Attack="FALSE"`.
func loadMonsterAIRows(dir string) map[string]MonsterAIRow {
	rows, err := readClassRows(filepath.Join(dir, monsterAIFile))
	if err != nil {
		return nil
	}
	out := make(map[string]MonsterAIRow, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.ClassName)
		if name == "" {
			continue
		}
		out[name] = MonsterAIRow{
			AIType:    strings.TrimSpace(r.AIType),
			Attack:    strings.EqualFold(strings.TrimSpace(r.Attack), "TRUE"),
			SeekRange: atofOr(r.SeekRange, 0),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readClassRows decodes one table file into its `<Class>` rows.
//
// The files are large (13 MB for the Monster base) and are decoded as a STREAM, not
// read whole: `xml.Unmarshal` on a 13 MB document holds two copies of it, and this
// runs at start-up beside everything else the zone loads.
func readClassRows(path string) ([]xmlRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rows := make([]xmlRow, 0, 1024)
	dec := xml.NewDecoder(f)
	// The files carry the client's own encoding declaration; UTF-8 is what every
	// other loader here reads them as (`LoadStances`, `LoadWarpTypes`), and the
	// bytes of the attributes we want are ASCII.
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
		var row xmlRow
		if derr := dec.DecodeElement(&row, &start); derr != nil {
			return nil, fmt.Errorf("%s: %w", path, derr)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// atoiOr and atofOr are the table readers' tolerant number parsing: the client's
// files hold "-", "None" and empty strings where a number belongs.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func atofOr(s string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return v
}
