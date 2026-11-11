package zone

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The stance and skill blocks inside START_INFO.
//
// THE CLIENT CRASHES WITHOUT THEM. Measured 2026-10-25: a record with
// stanceCount=0 was accepted, the client then took an access violation reading
// 0x1DC a millisecond after the packet, log `age_dll_crash.log`. The Python server
// always sends them: 4,559 bytes for the same three characters where the empty
// version was 1,491. The client picks its default stance FROM this list
// (CStanceList::Init + GetStanceByCondition), so an empty list leaves it with no
// stance object at all.
//
// The data is the game's own XML, not a database: three tables the client also
// reads. Only NUMERIC attributes are used here (ClassID, SkillID1..5,
// EqpWeaponSet, Stance1..6), which is why Go can parse them directly.

// StanceRow is one stance's own numbers, out of datatable_stance.xml.
//
// RSPD and WSPD are NOT the speed on the wire: the client uses them only to scale
// the run/walk ANIMATION (`CPC::PlayMoveAnim` divides the packet's speed by the
// stance's RSPD, geclientlib\geScene\PC.cpp:1175-1180). The number the server puts
// in a move packet is MSPD, which the original computes in a script from the
// 469 base and the stance's StnMSPD - see zone\speed.go.
type StanceRow struct {
	Name      string
	ClassType string  // MELEE / HYBRID / MAGIC ... - SCR_Get_MSPD caps MELEE at 750
	RSPD      float64 // run animation speed, not the wire value
	WSPD      float64 // walk animation speed
	StnMSPD   float64 // the multiplier the move-speed formula applies to 469
	// ASTD is the NAME of the stance's idle motion - the string the client hands
	// to PlayASTDAnim when a 3554 (or a spawn) applies this stance to an actor:
	// GetPropertyString("Stance", stance, "ASTD") (PC.cpp:501-521, then :1219).
	// It is a NAME, not a number, and it can be unusable - see MotionUsable.
	ASTD string
	// AI is the stance's AI profile NAME out of the SAME row ("ATTACKER",
	// "HEALER", "PUPPET", "BYSTANDER"). It is the column the reference compares
	// when it decides whether a stance change leaves the attack state:
	//
	//	IMC_DECLARE_STRINGID(ATTACKER);
	//	if (AS_ATTACK == actionState && ATTACKER != m_stance->GetAI())
	//		SetActionState(AS_NONE);      // PlayerChar_Request.cpp:1364-1368
	//
	// The value IS a name in this table, not a number - measured on the shipped
	// datatable_stance.xml, all 661 rows carry it: 618 "ATTACKER", 25 "HEALER",
	// 12 "PUPPET", 6 "BYSTANDER". The NUMBER the rest of this file speaks in is
	// the AI profile's own ClassID in datatable_ai.xml (1 ATTACKER, 2 HEALER,
	// 3 PUPPET, 4 BYSTANDER, 99 LIB) - see AI and IsAttackerAI.
	AI string
}

// NoneMotion is the client's own "no such motion" value. CPC::PlaySTDAnim treats a
// filename of exactly `PC\None` as "there is nothing to play" and returns false
// WITHOUT an error (PC.cpp:476-492), so the actor silently keeps the animation it
// already had. A stance or job row that points here is therefore REJECTED by the
// chooser instead of being sent.
const NoneMotion = `PC\None`

// MotionUsable says whether a motion NAME out of the tables can actually be played:
// empty or the client's own `PC\None` is not a name, everything else is.
//
// This is the check the client performs itself before it plays anything
// (PC.cpp:476-492: `if (FileName == "PC\\None") return false;`). Doing it here means
// a stance whose idle would play nothing is never chosen, so the actor gets a stance
// that animates instead of one that leaves it in bind pose.
func MotionUsable(name string) bool {
	s := strings.TrimSpace(name)
	if s == "" {
		return false
	}
	if strings.EqualFold(s, NoneMotion) || strings.EqualFold(s, "None") {
		return false
	}
	return true
}

// StanceCombat is the columns of a stance row that the game's own status
// scripts read when they build the Character Info sheet (R61b). They are loaded
// next to the fields the stance chooser needs, from the SAME row, so a stance
// cannot be described two different ways.
//
// Named by the script that reads each one (ge\script\pc\get_status_pc.scp):
//
//	AtkType/DefType  the branch of SCR_Get_CATK :1002 and SCR_Get_DEF :670
//	RHits/LHits      hits per swing - SCR_Get_CATK :984, SCR_Get_AR :603
//	StnATK           the stance's attack multiplier - SCR_Get_CATK :984
//	StnHR/IncHR/StnLv  SCR_Get_CHR :1318 `stance.StnHR + stance.IncHR*stance.StnLv`
//	ASPD/IncASPD     SCR_Get_CSPD :1686 and :1705
//	StnRFIRE..RSTAT  the stance term of each resistance, :2135 / :2277
//	MinR/MaxR        THE NORMAL ATTACK'S REACH (R90) - see the two fields below
type StanceCombat struct {
	AtkType   string
	DefType   string
	ClassType string
	RHits     float64
	LHits     float64
	StnATK    float64
	StnHR     float64
	IncHR     float64
	StnLv     float64
	ASPD      float64
	IncASPD   float64
	StnRFIRE  float64
	StnRICE   float64
	StnRLGHT  float64
	StnRSTAT  float64

	// MinR and MaxR are THE NORMAL ATTACK'S REACH, in map units (the same unit the
	// world positions use), read off the SAME row as ASPD.
	//
	// WHY HERE (R90). The owner, twice: "again normal skill also need stance range
	// check. Fighter melee attack is attacking from insane distance lol." and
	// "normal attack is stance based. read the xml/ies." He is right, and the columns
	// were in this table all along: a plain attack has NO skill row, so the only row
	// that can say how far it reaches is the stance's own -
	//
	//	BackGuard      10110  MinR 0   MaxR 150   ASPD 1000
	//	HighGuard      10120  MinR 0   MaxR 150   ASPD 1300
	//	PossessionFire 20210  MinR 50  MaxR 900   ASPD 1700
	//	FirstAid       30110  MinR 1   MaxR 100   ASPD 1500
	//	Peltast        10560  MinR 1   MaxR 125   ASPD 1067
	//
	// MaxR is the far bound and MinR the near one, exactly as a skill's row reads
	// them (zone\skill_area.go). 0 is the table's own "no such bound": MaxR 0 means
	// the row states no maximum (so no reach check is invented - see
	// zone\attack.go), and a MinR of 0 is a stance that may hit a body it stands on.
	MinR float64
	MaxR float64

	// SplRange and SplType are the SPLASH SHAPE of the SAME row: how far past the
	// MAIN TARGET a splash stance's normal attack reaches, and what shape that area
	// is. They sit on the row that AtkType already comes from, which is the whole
	// point - a stance is splash because ITS OWN row says so, never because of a
	// character id, a name or a handle.
	//
	// MEASURED, on the shipped table and on one live capture: stance 120039
	// (ClassName "Disablering") is AtkType="Splash", SplType="Circle",
	// SplRange="220", on the same row as MinR 0 .. MaxR 1050. The capture's own 171
	// byte sample shows what the area means -
	//
	//	the extra victim stands 109.5 units from the MAIN TARGET
	//	and                 303.9 units from the CASTER
	//
	// - so the circle is centred on the main target, not on the character that
	// swings, and a 220-unit circle around the caster would not have contained it
	// (`docker-port\history\splash-attack.md`, built by zone\attack_splash.go).
	//
	// SplType reads "Circle" on the 180 rows that state a shape and "None" on every
	// other one; nothing else has ever been measured, so only the circle is built.
	// A missing attribute reads as 0 / "" and is NOT an error: it is the table's own
	// "no area", meaning a plain attack that touches one body - the shape every
	// stance had before this column was read.
	SplRange float64
	SplType  string
}

// StanceTable is the three tables flattened to what START_INFO needs.
type StanceTable struct {
	// stanceSkills: stance id -> the skills it grants (SkillID1..5).
	stanceSkills map[int][]int
	// rows: stance id -> its name, class type and speeds, for the move-speed
	// formula (zone\speed.go) and for nothing else.
	rows map[int]StanceRow
	// combat: stance id -> the columns the status scripts read. Loaded from the
	// SAME datatable_stance.xml row as rows, in the same pass; a missing attribute
	// reads as 0 (a stance with no stance bonus), never as a guess.
	combat map[int]StanceCombat
	// jobs: job id -> its stance-condition ids (EqpWeaponSet).
	jobs map[int][]int
	// conds: condition id -> its stance ids (Stance1..6).
	conds map[int][]int
	// hands: condition id -> the LHand/RHand weapon class TYPES the condition
	// needs ("None" = bare). It is what decides WHICH condition applies to a
	// character, which is how the active stance is picked - see PickStance.
	hands map[int][2]string
	// jobSTD: job id -> the NAME of its standing motion ("PC_STD_GRANDIES"). The
	// client reads this column itself when it draws a standing character
	// (GetPropertyString("Job", job, "STD"), PC.cpp:476-492). It is read here only
	// to be able to SAY whether that name is usable - see MotionUsable.
	jobSTD map[int]string
	// jobSkill: job id -> the job's OWN skill (`JobSkill` in datatable_job.xml,
	// "10415" for the Fighter). It is the one skill no stance row names, and the
	// live ZC_SKILLLIST carries it as its last entry - see zone\skill_list.go
	// JobSkillList and history\stance-skill-lists.md §2. Read from the same job row
	// as EqpWeaponSet and STD, so it cannot drift from the job's own data.
	jobSkill map[int]int
	// weaponTypes: item class id -> its weapon TYPE ("Rapier", "Shield", ...), out
	// of datatable_item_weapon.xml. This is the missing half of the hands check:
	// a stance condition asks for a weapon TYPE, and this is how the type of the
	// item a character is actually holding is found.
	//
	// The vocabulary is the SAME list in both tables - measured on the shipped data,
	// all 28 distinct Hand values ("Sword", "THSword", "MainG", "Rosario", ...)
	// appear as a weapon ClassType and no weapon row lacks one - which is why this
	// one column is enough and nothing is invented.
	weaponTypes map[int]string
	// twoHanded: item class id -> true when that weapon is held in BOTH hands. The
	// client needs this because a two-handed weapon frees no left hand: it blanks
	// the left hand before it looks the stance condition up
	// (CSession::GetStanceByCondition, GeSession.cpp:487-520), and without the same
	// override a character holding a two-hander would be validated against a
	// "weapon + shield" condition it can never actually be in.
	//
	// The columns are the table's own: DblAttach is "0"/"1" and DBLHand is
	// "NO"/"YES" (measured: 49 DblAttach="1" and 32 DBLHand="YES" rows out of 3431).
	// The client's third test, `equipType == "DH"`, never fires on this data at all -
	// EqpType is "SH" on all 3431 rows - so it is not read here.
	twoHanded map[int]bool
	// aiIDs: AI profile NAME -> its ClassID in datatable_ai.xml ("ATTACKER" -> 1).
	// It is what turns the stance row's AI name into the number the rest of the
	// server can compare, and it is read from the client's own file rather than
	// written down here. A zone that cannot read datatable_ai.xml still knows the
	// attacker profile: see AttackerAI.
	aiIDs map[string]int

	// rawStance: stance id -> the row's OWN attributes, exactly as
	// datatable_stance.xml writes them. It is what `GetCurrentStance(self)`
	// answers to the game's own status script, which reads 32 of them by name
	// (AtkLayer, AtkType, ClassID, ClassName, ClassType, DefLayer, DefType,
	// IncASPD, IncBLK, IncCRT, IncHR, IncIMP, IncIP, IncPR, IncRSP, LHits, RHits,
	// RSPType, StnATK, StnBLK, StnHR, StnIMP, StnIP, StnIgnoreDEFPer, StnLv,
	// StnMSPD, StnPR, StnRFIRE, StnRICE, StnRLGHT, StnRSTAT and ASPD).
	//
	// WHY THE WHOLE ROW AND NOT A TYPED STRUCT: StanceCombat already carries the
	// columns the GO rules read, and a second typed struct would be a second
	// place for a column to go missing. The script reads the row itself, so the
	// row itself is kept - one copy, straight out of the parser, with no
	// conversion that could shift a name.
	rawStance map[int]map[string]string
	// rawJob: job id -> the same, out of datatable_job.xml. The status script's
	// `GetDataTableNumberValue('Job', id, column)` /
	// `...ByClassName('Job', name, 'ClassID')` read it, and the job file is
	// already parsed here - so the same rule applies: keep the row, do not
	// re-read the file for one column.
	rawJob map[int]map[string]string
}

// AttackerAI is datatable_ai.xml's own ClassID for the attacker profile:
//
//	<Class ClassID="1" ClassName="ATTACKER" AssaultRange="1000" ... />
//
// It is the value in `ATTACKER != m_stance->GetAI()`
// (PlayerChar_Request.cpp:1367): the one AI that KEEPS the attack state across a
// stance change. The number is a fallback, used only to NAME the profile in a log
// line and when datatable_ai.xml could not be read - the mapping itself comes from
// the file (see StanceTable.AI).
const AttackerAI = 1

// AttackerAIName is the same profile's name in datatable_ai.xml and in the AI
// column of every attacker stance row.
const AttackerAIName = "ATTACKER"

// XMLDir finds the folder holding the game's datatable_*.xml files.
//
// It is NOT a hardcoded Windows path, on purpose: this server has to run on a
// Linux VPS, and a path that starts with D:\ is a deploy failure waiting to
// happen. Order: GE_XML_DIR if set, then the places the folder actually sits
// relative to the server's working directory (goserver\, so ../../ge/xml is the
// repository layout), then the same name one level up for a deployed bundle.
func XMLDir() string {
	if v := os.Getenv("GE_XML_DIR"); v != "" {
		return v
	}
	candidates := []string{
		filepath.Join("..", "..", "ge", "xml"),       // server run from goserver\
		filepath.Join("..", "..", "..", "ge", "xml"), // tests run from goserver\zone\
		filepath.Join("..", "ge", "xml"),
		filepath.Join("ge", "xml"),
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "datatable_stance.xml")); err == nil {
			return dir
		}
	}
	return candidates[0] // let LoadStances report the miss, with the path it tried
}

// LoadStances reads datatable_stance.xml, datatable_job.xml and
// datatable_stancecondition.xml from dir. A missing or unreadable file is an
// error: a zone that cannot describe a character must not pretend it can.
func LoadStances(dir string) (*StanceTable, error) {
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

	_, err := eachClass(filepath.Join(dir, "datatable_stance.xml"),
		func(attrs map[string]string) {
			id, ok := atoi(attrs["ClassID"])
			if !ok {
				return
			}
			var skills []int
			for n := 1; n <= 5; n++ {
				if v, ok := atoi(attrs["SkillID"+strconv.Itoa(n)]); ok && v != 0 {
					skills = append(skills, v)
				}
			}
			t.stanceSkills[id] = skills
			// The SAME row, kept whole for the status script's GetCurrentStance.
			t.rawStance[id] = attrs
			// StnMSPD defaults to 1 in the table and in the original script, so a
			// missing attribute must read as 1, never as 0 - 0 is a stance that
			// FREEZES the character (get_status_pc.scp:1808 multiplies by it).
			t.rows[id] = StanceRow{
				Name:      attrs["ClassName"],
				ClassType: attrs["ClassType"],
				RSPD:      atof(attrs["RSPD"]),
				WSPD:      atof(attrs["WSPD"]),
				StnMSPD:   atofDefault(attrs["StnMSPD"], 1),
				ASTD:      attrs["ASTD"],
				AI:        attrs["AI"],
			}
			// The status-script columns of the SAME row. A stance with no
			// StnATK attribute must not multiply a weapon by 0 (that is a
			// character who cannot hit at all), so StnATK reads 1 when absent -
			// the table's own neutral value, which is what `StnATK="1"` says on
			// every row that has it.
			t.combat[id] = StanceCombat{
				AtkType:   attrs["AtkType"],
				DefType:   attrs["DefType"],
				ClassType: attrs["ClassType"],
				RHits:     atof(attrs["RHits"]),
				LHits:     atof(attrs["LHits"]),
				StnATK:    atofDefault(attrs["StnATK"], 1),
				StnHR:     atof(attrs["StnHR"]),
				IncHR:     atof(attrs["IncHR"]),
				StnLv:     atofDefault(attrs["StnLv"], 1),
				ASPD:      atof(attrs["ASPD"]),
				IncASPD:   atof(attrs["IncASPD"]),
				StnRFIRE:  atof(attrs["StnRFIRE"]),
				StnRICE:   atof(attrs["StnRICE"]),
				StnRLGHT:  atof(attrs["StnRLGHT"]),
				StnRSTAT:  atof(attrs["StnRSTAT"]),
				// THE NORMAL ATTACK'S REACH, from the same row (R90). No default: a
				// missing attribute reads as 0, which is the table's own "no bound"
				// and not a guess at one.
				MinR: atof(attrs["MinR"]),
				MaxR: atof(attrs["MaxR"]),
				// AND THE SPLASH SHAPE, from the SAME row. No default either, and a
				// missing attribute is not an error: SplRange 0 / SplType "" is the
				// table's own "no area", which is a stance whose plain attack hits one
				// body (§2 of history\splash-attack.md measured the shape of the one
				// row a capture shows: Circle, 220, centred on the main target).
				SplRange: atof(attrs["SplRange"]),
				SplType:  attrs["SplType"],
			}
		})
	if err != nil {
		return nil, fmt.Errorf("stance table: %w", err)
	}

	// The AI table, for one column: the NAME of a stance's AI profile -> its
	// ClassID. datatable_stance.xml states the profile by NAME ("AI=ATTACKER"), and
	// the reference compares exactly that name (`IMC_DECLARE_STRINGID(ATTACKER)` vs
	// `GetAI()`), so this map is only about being able to SAY "AI 1" and to answer a
	// stance's AI as a number.
	//
	// A MISSING FILE IS NOT AN ERROR HERE. Nothing else in this zone reads it, and
	// every rule that uses it has a fallback that already knows the attacker row
	// (AttackerAI/AttackerAIName), so a bundle without datatable_ai.xml keeps
	// swinging instead of silently refusing every stance change.
	_, _ = eachClass(filepath.Join(dir, "datatable_ai.xml"),
		func(attrs map[string]string) {
			id, ok := atoi(attrs["ClassID"])
			if !ok {
				return
			}
			if name := attrs["ClassName"]; name != "" {
				t.aiIDs[name] = id
			}
		})

	// The weapon table, for one column: the TYPE of what a character holds. Read
	// from the client's own file in the same folder, with the same reader - the
	// alternative is a hand-written id-to-type list, which drifts from the data on
	// the first item the game adds.
	_, err = eachClass(filepath.Join(dir, "datatable_item_weapon.xml"),
		func(attrs map[string]string) {
			id, ok := atoi(attrs["ClassID"])
			if !ok {
				return
			}
			t.weaponTypes[id] = attrs["ClassType"]
			// Two-handedness, from the SAME row. A weapon is two-handed when the
			// table says so in either of its two columns - see StanceHands for why
			// the left hand is then treated as empty.
			t.twoHanded[id] = attrs["DblAttach"] == "1" || attrs["DBLHand"] == "YES"
		})
	if err != nil {
		return nil, fmt.Errorf("weapon table: %w", err)
	}

	_, err = eachClass(filepath.Join(dir, "datatable_job.xml"),
		func(attrs map[string]string) {
			id, ok := atoi(attrs["ClassID"])
			if !ok {
				return
			}
			t.jobs[id] = parseIntList(attrs["EqpWeaponSet"], ",")
			t.jobSTD[id] = attrs["STD"]
			// The SAME row, kept whole for the status script's Job table reads.
			t.rawJob[id] = attrs
			if v, ok := atoi(attrs["JobSkill"]); ok {
				t.jobSkill[id] = v
			}
		})
	if err != nil {
		return nil, fmt.Errorf("job table: %w", err)
	}

	_, err = eachClass(filepath.Join(dir, "datatable_stancecondition.xml"),
		func(attrs map[string]string) {
			id, ok := atoi(attrs["ClassID"])
			if !ok {
				return
			}
			var stances []int
			for n := 1; n <= 6; n++ {
				if v, ok := atoi(attrs["Stance"+strconv.Itoa(n)]); ok && v != 0 {
					stances = append(stances, v)
				}
			}
			t.conds[id] = stances
			// The hands the condition needs. A missing attribute means BARE, which is
			// how the reference reads it too (src\stance_data.py:169-170).
			l, r := attrs["LHand"], attrs["RHand"]
			if l == "" {
				l = "None"
			}
			if r == "" {
				r = "None"
			}
			t.hands[id] = [2]string{l, r}
		})
	if err != nil {
		return nil, fmt.Errorf("stance-condition table: %w", err)
	}

	if len(t.stanceSkills) == 0 || len(t.jobs) == 0 || len(t.conds) == 0 {
		return nil, fmt.Errorf("stance tables under %s are empty - wrong folder?", dir)
	}
	if len(t.weaponTypes) == 0 {
		return nil, fmt.Errorf("weapon table under %s is empty - the stance chooser cannot "+
			"tell a sword from a shield, and a wrong stance is what leaves a remote "+
			"character in a T-pose", dir)
	}
	return t, nil
}

// usableWord is the log's word for MotionUsable: a "NOT USABLE" in the spawn line is
// the one thing that explains a T-pose on its own.
func usableWord(name string) string {
	if MotionUsable(name) {
		return "usable"
	}
	return "NOT USABLE"
}

// JobStances is every stance the job knows, in condition order, no repeats -
// CStanceList::Init.
func (t *StanceTable) JobStances(job int) []int {
	var out []int
	seen := map[int]bool{}
	for _, condID := range t.jobs[job] {
		for _, sid := range t.conds[condID] {
			if !seen[sid] {
				seen[sid] = true
				out = append(out, sid)
			}
		}
	}
	return out
}

// stanceCond is one of the job's stance conditions, in table order.
type stanceCond struct {
	ID   int
	L, R string // the weapon TYPES the condition needs; "None" = bare
	IDs  []int  // the stances it offers, in its own order
}

// jobConds is the job's stance conditions in the table's own order, with the hands
// each one needs and the stances it offers.
func (t *StanceTable) jobConds(job int) []stanceCond {
	var out []stanceCond
	for _, condID := range t.jobs[job] {
		h, ok := t.hands[condID]
		if !ok {
			continue
		}
		out = append(out, stanceCond{ID: condID, L: h[0], R: h[1], IDs: t.conds[condID]})
	}
	return out
}

// normHand maps "no weapon" to the word the tables use, so a missing id and a bare
// hand are the same thing to every comparison below.
func normHand(s string) string {
	if strings.TrimSpace(s) == "" {
		return "None"
	}
	return s
}

// HandSpotLeft and HandSpotRight are the two appearance slots that hold a weapon.
//
// 4 is the LEFT hand and 5 the RIGHT one, which is the client's own numbering (see
// equipWeaponSpots in players.go, and EQUIP_NONE[4] == EQUIP_NONE[5] == 10000, the
// bare hand). Main hand first is the other order, for the spawn's +11 field; a
// stance condition names BOTH hands, so here each slot is looked at on its own.
const (
	HandSpotLeft  = 4
	HandSpotRight = 5
)

// WeaponType is the weapon TYPE of an item class ("Rapier", "Shield", ...), or "" for
// anything the weapon table does not hold - an armour piece, a costume, or an empty
// slot.
func (t *StanceTable) WeaponType(classID uint32) string {
	if classID == 0 {
		return ""
	}
	return t.weaponTypes[int(classID)]
}

// Hands is what a character is HOLDING, as [left, right] weapon TYPES, with "None"
// for a hand that holds nothing or holds something that is not a weapon.
func (t *StanceTable) Hands(c Character) [2]string {
	return [2]string{
		normHand(t.WeaponType(c.Equip[HandSpotLeft])),
		normHand(t.WeaponType(c.Equip[HandSpotRight])),
	}
}

// StanceHands is Hands plus the client's own TWO-HANDED OVERRIDE: a two-handed
// weapon in the right hand means the character does not also hold anything in its
// left hand, so the left hand reads "None".
//
// This is CSession::GetStanceByCondition (GeSession.cpp:487-520) read literally: it
// takes the two hand items' class types, and `if (right is two-handed) leftID =
// "None"` before it asks geStanceConditionTable::GetStances. A greatsword row in
// datatable_stancecondition.xml therefore names the RIGHT hand alone, and reading
// the left slot as well would make every two-hander look like no condition at all.
//
// Hands is left UNCHANGED on purpose: PickStance depends on the raw pair to tell
// "hands match what it holds" from its fallbacks.
func (t *StanceTable) StanceHands(c Character) [2]string {
	if t == nil {
		// A nil table holds no weapon rows, so both hands are bare - the same
		// answer Hands gives for an empty slot, without the nil dereference.
		return [2]string{"None", "None"}
	}
	hands := t.Hands(c)
	if t.twoHanded[int(c.Equip[HandSpotRight])] {
		hands[0] = "None"
	}
	return hands
}

// StancesForHands is the client's geStanceConditionTable::GetStances: every stance
// the JOB offers for exactly these two hands, in the table's own order, no repeats.
//
// It is the permission list for a stance change (zone\stance_change.go) and the
// reason a change request can be checked at all: whatever this returns is what the
// character could have chosen for itself. A nil table or a job with no rows answers
// nil, which refuses every request instead of allowing one.
func (t *StanceTable) StancesForHands(job int, hands [2]string) []int {
	if t == nil {
		return nil
	}
	wantL, wantR := normHand(hands[0]), normHand(hands[1])
	var out []int
	seen := map[int]bool{}
	for _, condID := range t.jobs[job] {
		h, ok := t.hands[condID]
		if !ok {
			continue
		}
		if normHand(h[0]) != wantL || normHand(h[1]) != wantR {
			continue
		}
		for _, sid := range t.conds[condID] {
			if !seen[sid] {
				seen[sid] = true
				out = append(out, sid)
			}
		}
	}
	return out
}

// JobSTD is the NAME of the job's standing motion - the column the client reads
// itself in PlaySTDAnim. Empty when the job has no row.
func (t *StanceTable) JobSTD(job int) string { return t.jobSTD[job] }

// JobSkill is the job's OWN skill (`JobSkill` in datatable_job.xml), or 0 when the
// job has no row or names none. It is the one skill no stance row lists - 10415
// HighlanderSoul for the Fighter, 1005 ManaActivation for the Warlock - and the
// live ZC_SKILLLIST ends with it (history\stance-skill-lists.md §2).
func (t *StanceTable) JobSkill(job int) int {
	if t == nil {
		return 0
	}
	return t.jobSkill[job]
}

// StanceASTD is the NAME of a stance's idle motion, and StanceName its class name.
func (t *StanceTable) StanceASTD(stance uint32) string {
	return t.rows[int(stance)].ASTD
}

func (t *StanceTable) StanceName(stance uint32) string {
	return t.rows[int(stance)].Name
}

// AI answers the ID of the stance's AI profile - the number of the profile in
// datatable_ai.xml, so 1 is the attacker, 2 the healer, 3 the puppet and 4 the
// bystander. It is the number behind the NAME datatable_stance.xml states on the
// stance's own row (StanceRow.AI), and it comes from that row plus datatable_ai.xml;
// nothing here is a list of stance ids.
//
// TWO CASES ANSWER 0, DELIBERATELY, because 0 is "no profile this zone can name":
//
//	a stance the table does not know  no row -> nothing to read. 0 is treated as NOT
//	                                  the attacker AI by IsAttackerAI, which is the
//	                                  reference's own default: `ATTACKER != GetAI()`
//	                                  is true for a NULL stance too, so the attack
//	                                  state ends.
//	a row with no AI attribute      the same answer for the same reason: an empty
//	                                  name is not the attacker profile.
//
// A table that could not read datatable_ai.xml still answers 1 for an "ATTACKER"
// row (see AttackerAIName), so a bundle missing that one file does not turn every
// stance change into an ending.
func (t *StanceTable) AI(stanceID int) int {
	if t == nil {
		return 0
	}
	name := t.rows[stanceID].AI
	if id, ok := t.aiIDs[name]; ok {
		return id
	}
	if strings.EqualFold(strings.TrimSpace(name), AttackerAIName) {
		return AttackerAI
	}
	return 0
}

// IsAttackerAI answers whether a stance stands in the ATTACKER AI - the one profile
// whose stance change does NOT end the attack state (PlayerChar_Request.cpp:1364-1368,
// and see StanceRow.AI). A stance this table does not know, or one whose row states
// no AI, is NOT the attacker AI: the answer is false, and the caller ends the repeat
// and says so.
func (t *StanceTable) IsAttackerAI(stanceID int) bool {
	return t.AI(stanceID) == AttackerAI
}

// Combat answers one stance's status-script columns. A nil table or a stance
// with no row answers NEUTRAL values (StnATK 1, StnLv 1 and everything else 0),
// so a character whose stance is unknown still gets an ATK for the weapon it
// holds instead of zero, and no panic.
func (t *StanceTable) Combat(stance uint32) StanceCombat {
	if t == nil {
		return StanceCombat{StnATK: 1, StnLv: 1}
	}
	if row, ok := t.combat[int(stance)]; ok {
		return row
	}
	return StanceCombat{StnATK: 1, StnLv: 1}
}

// RawStance answers one stance row's own attributes, exactly as
// datatable_stance.xml writes them: the map `GetCurrentStance(self)` hands the
// game's status script. A nil table or an unknown stance answers nil, false -
// the caller decides what an empty row means (see zone\status_shadow.go, which
// answers a zero table so the script's unguarded reads cannot raise).
//
// The map is the parser's own and is NOT copied: it is read-only after load.
func (t *StanceTable) RawStance(stance uint32) (map[string]string, bool) {
	if t == nil {
		return nil, false
	}
	row, ok := t.rawStance[int(stance)]
	return row, ok
}

// RawJob is the same for one job row out of datatable_job.xml, which the status
// script reaches through GetDataTableNumberValue('Job', ...) and its
// ByClassName twin.
func (t *StanceTable) RawJob(job uint32) (map[string]string, bool) {
	if t == nil {
		return nil, false
	}
	row, ok := t.rawJob[int(job)]
	return row, ok
}

// JobIDByClassName answers a job's ClassID from its ClassName ("Fighter"), which
// is what `GetDataTableNumberValueByClassName('Job', name, 'ClassID')` asks.
// 0, false means no job row carries that name.
func (t *StanceTable) JobIDByClassName(name string) (uint32, bool) {
	if t == nil || name == "" {
		return 0, false
	}
	for id, row := range t.rawJob {
		if row["ClassName"] == name {
			return uint32(id), true
		}
	}
	return 0, false
}

// StancePick is the stance chosen for one character, with EVERYTHING needed to say
// why: the condition it came from, the hands that condition needs, and the two names
// the client resolves when it draws the character.
type StancePick struct {
	Stance uint32
	Name   string // the stance's ClassName ("FirstAid")
	ASTD   string // its idle motion name ("PC_ASTD_SCT")
	Cond   int    // the condition row it came from
	LHand  string // the hands that condition needs
	RHand  string
	Reason string // which rule chose it, in words
}

// Usable answers whether the ASTD motion of the pick can be played. A pick is only
// ever returned when it can, so this is for the log.
func (p StancePick) Usable() bool { return MotionUsable(p.ASTD) }

// SpawnLine is the ONE log line printed per character at spawn, and the line the
// owner can be asked to read back after a restart.
//
// It carries everything needed to tell "the wrong stance was chosen" from "the right
// stance whose motion does not resolve": the handle and job the client keys on, the
// stance id and its name, the condition row it came from and the hands that row
// needs, what the character actually holds, and BOTH motion names the client will
// resolve by string - the job's STD (the standing pose) and the stance's ASTD (the
// stance idle), each marked usable or NOT.
func (p StancePick) SpawnLine(handle uint32, name string, job int, jobSTD string, hands [2]string) string {
	return fmt.Sprintf("zone: spawn char %d %q: job %d, stance %d %q (condition %d "+
		"needs %s/%s, %s; this character holds %s/%s), job STD %q %s, stance ASTD %q %s",
		handle, name, job, p.Stance, p.Name, p.Cond, p.LHand, p.RHand, p.Reason,
		normHand(hands[0]), normHand(hands[1]),
		jobSTD, usableWord(jobSTD), p.ASTD, usableWord(p.ASTD))
}

// PickStance is the stance this server puts a character into: the FIRST candidate
// whose idle motion NAME actually resolves, preferring the one that matches what the
// character holds.
//
// WHY THE HANDS MATTER AT ALL: a stance names an idle motion built for a weapon
// ("PC_ASTD_EPE" is the rapier guard). Applying the bare-knuckle idle to a character
// holding a rapier is the "awkward pose" measured live 2026-10-28, and applying one
// to the wrong job is a pose the client can only approximate. The old rule ignored
// Character.Equip completely and gave EVERY character the bare-hand stance.
//
// THE RULES, in this order, all of them filtered to "the ASTD resolves" first (see
// MotionUsable) and all of them walking the job's conditions IN TABLE ORDER so the
// game's own preference decides between equals:
//
//  1. the condition whose two hands are exactly what the character holds;
//  2. the condition for the MAIN hand alone, for a character whose other hand holds
//     something no condition describes (a shield combo the job does not list);
//  3. bare hands - ONLY for a character who really is holding nothing. A character
//     with a weapon must never take this one: it is the T-pose's other half.
//  4. any condition that is not the bare-hand one, so a weapon-holder still gets an
//     animation;
//  5. any stance the job knows at all.
//
// Rule 4 and 5 are the fallback the task asks for ("if no weapon qualifies, fall back
// to any stance whose ASTD resolves") and they log their reason, so a fallback is
// visible in the log instead of looking like a correct match.
//
// NOTHING IS INVENTED: every id comes out of datatable_stancecondition.xml and every
// name out of datatable_stance.xml / datatable_job.xml, the same files the client
// reads. The only judgement made here is WHICH of the game's own rows applies.
func (t *StanceTable) PickStance(job int, hands [2]string) (StancePick, bool) {
	conds := t.jobConds(job)
	if len(conds) == 0 {
		return StancePick{}, false
	}
	wornL, wornR := normHand(hands[0]), normHand(hands[1])
	bare := wornL == "None" && wornR == "None"

	rules := []struct {
		why  string
		keep func(stanceCond) bool
	}{
		{"hands match what it holds", func(c stanceCond) bool {
			return c.L == wornL && c.R == wornR
		}},
		{"main hand " + wornR, func(c stanceCond) bool {
			return wornR != "None" && c.R == wornR && c.L == "None"
		}},
		{"bare hands", func(c stanceCond) bool {
			return bare && c.L == "None" && c.R == "None"
		}},
		{"fallback: no condition matches, so any weapon stance", func(c stanceCond) bool {
			return c.L != "None" || c.R != "None"
		}},
		{"fallback: any stance this job knows", func(c stanceCond) bool {
			return true
		}},
	}

	for _, rule := range rules {
		for _, cond := range conds {
			if !rule.keep(cond) {
				continue
			}
			for _, sid := range cond.IDs {
				row, ok := t.rows[sid]
				if !ok || !MotionUsable(row.ASTD) {
					continue // no row, or its idle would play nothing
				}
				return StancePick{
					Stance: uint32(sid),
					Name:   row.Name,
					ASTD:   row.ASTD,
					Cond:   cond.ID,
					LHand:  cond.L,
					RHand:  cond.R,
					Reason: rule.why,
				}, true
			}
		}
	}
	return StancePick{}, false
}

// ActiveStance is PickStance for a character holding NOTHING.
//
// It is kept because the bare-hand case is the one the client's own default-stance
// pick reduces to on this side (CSession::GetStanceByCondition), and because every
// caller before 2026-10-28 meant exactly that. Anything that knows what the character
// is wearing must call PickStance with StanceTable.Hands instead - a bare-hand stance
// on a character holding a rapier is the awkward pose this replaces.
func (t *StanceTable) ActiveStance(job int) (uint32, bool) {
	p, ok := t.PickStance(job, [2]string{"None", "None"})
	return p.Stance, ok
}

// StanceSkills is the skills ONE stance grants: SkillID1..SkillID5 of its row in
// datatable_stance.xml, in the row's order, with the 0 slots already dropped at load
// time. This is what the ZC_SKILLLIST for a character standing in that stance is
// built from (zone\skill_list.go).
//
// The slice is COPIED before it leaves: t.stanceSkills is the table's own state, and
// a caller that sorted or edited the returned slice would be editing the answer every
// later character gets as well.
func (t *StanceTable) StanceSkills(stance uint32) []int {
	if t == nil {
		return nil
	}
	skills := t.stanceSkills[int(stance)]
	if len(skills) == 0 {
		return nil
	}
	return append([]int(nil), skills...)
}

// JobSkills is every skill those stances refer to - CStanceList::GetUseSkillID.
func (t *StanceTable) JobSkills(job int) []int {
	var out []int
	seen := map[int]bool{}
	for _, sid := range t.JobStances(job) {
		for _, skill := range t.stanceSkills[sid] {
			if !seen[skill] {
				seen[skill] = true
				out = append(out, skill)
			}
		}
	}
	return out
}

// BuildStanceRecords packs STANCE_INFO records, 14 bytes each:
//
//	classID(4) level(2) exp(4) skillPoint(4)
//
// Level and exp come from the character's own STANCE rows, which live in the
// legacy database and are NOT read here yet, so every stance is sent at level 1
// with 0 exp - exactly what the Python server sends for a stance with no row.
// The client unlocks the stances we name and shows level 1.
func (t *StanceTable) BuildStanceRecords(job int) []byte {
	ids := t.JobStances(job)
	out := make([]byte, 0, len(ids)*14)
	for _, sid := range ids {
		rec := make([]byte, 14)
		binary.LittleEndian.PutUint32(rec[0:], uint32(sid))
		binary.LittleEndian.PutUint16(rec[4:], 1) // level
		binary.LittleEndian.PutUint32(rec[6:], 0) // exp
		binary.LittleEndian.PutUint32(rec[10:], 0)
		out = append(out, rec...)
	}
	return out
}

// BuildSkillRecords packs SKILL_INFO records, 10 bytes each:
//
//	classID(4) level(2) remainingExp(4)
//
// Same gap as the stances: skill level and exp come from the character's rows in
// the legacy database, so everything is level 1 with 0 exp.
func (t *StanceTable) BuildSkillRecords(job int) []byte {
	ids := t.JobSkills(job)
	out := make([]byte, 0, len(ids)*10)
	for _, sid := range ids {
		rec := make([]byte, 10)
		binary.LittleEndian.PutUint32(rec[0:], uint32(sid))
		binary.LittleEndian.PutUint16(rec[4:], 1) // level
		binary.LittleEndian.PutUint32(rec[6:], 0) // remaining exp
		out = append(out, rec...)
	}
	return out
}

// Describe is for the boot log: how many rows each table gave us.
func (t *StanceTable) Describe() string {
	return fmt.Sprintf("%d stance row(s), %d job(s), %d condition(s)",
		len(t.stanceSkills), len(t.jobs), len(t.conds))
}

// eachClass calls fn for every <Class .../> element in the file, whatever the
// nesting under <idspace>. Attributes are handed over as a plain map because
// these tables carry dozens of fields and only a few matter here.
func eachClass(path string, fn func(attrs map[string]string)) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Class" {
			continue
		}
		attrs := make(map[string]string, len(start.Attr))
		for _, a := range start.Attr {
			attrs[a.Name.Local] = a.Value
		}
		fn(attrs)
		count++
	}
	return count, nil
}

func atoi(s string) (int, bool) {
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// atof reads one of the table's numeric attributes. A table cell can carry a
// trailing space ("1.10 "), which ParseFloat rejects, so it is trimmed first.
func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// atofDefault is atof with the table's own default when the attribute is missing
// or unreadable. StnMSPD needs it: absent means 1 (no change), and 0 would be a
// stance that cannot move.
func atofDefault(s string, def float64) float64 {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return atof(s)
}

// parseIntList reads "1,2,3" or "1 2 3" into ids, skipping anything that is not
// a positive number. The tables use a comma; the space form is tolerated.
func parseIntList(s, sep string) []int {
	var out []int
	for _, part := range splitAny(s, sep) {
		if v, ok := atoi(part); ok && v != 0 {
			out = append(out, v)
		}
	}
	return out
}

func splitAny(s string, seps string) []string {
	fields := []string{}
	cur := ""
	for _, r := range s {
		isSep := false
		for _, sep := range seps {
			if r == sep {
				isSep = true
				break
			}
		}
		if isSep {
			if cur != "" {
				fields = append(fields, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		fields = append(fields, cur)
	}
	return fields
}
