package zone

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
)

// The move speed that goes in a ZC_MOVE_PC - and it is NOT the stance's RSPD.
//
// WHERE THE NUMBER COMES FROM (measured 2026-10-28, live capture
// captures\warp_ref\20261027-200136\s2c_7054_64343.bin, all 62 ZC_MOVE_PC records:
// 1090.0 x55, 469.0 x4, 675.0 x2, 658.0 x1):
//
//   - The original server sends `packet.spd = m_moveSpeed` and nothing else
//     (PlayerChar.cpp:401-408). m_moveSpeed is set from GetMoveSPD()
//     (FieldChar_FSM.cpp:599 for an ordinary move, PlayerChar_FSM.cpp:224 for the
//     walk to a clicked NPC), which is a one-line getter
//     for the MSPD property (FieldChar.cpp:1297).
//   - MSPD on a PC is NOT a table column: datatable_pc.xml declares it as a
//     calculated property, `CP_MSPD="SCR_Get_MSPD"`, and the engine runs that
//     script once and caches the result (PropertySys.cpp:8-21).
//   - The script is ge\script\pc\get_status_pc.scp:1764-2013. Its walled-off rule
//     is: `value = floor(469 * stance.StnMSPD * <every equipped item's MSPD>)`,
//     then `value = floor(value * (100 + MSPD_BM + achieve) / 100)`, then a cap.
//     The constant is 469 and the stance's column is StnMSPD - the RSPD column is
//     never read by the server at all (zero hits for "RSPD" in geServer).
//   - The CLIENT reads RSPD only to scale the run ANIMATION:
//     `CPC::PlayMoveAnim` divides the packet speed by the stance's RSPD
//     (geclientlib\geScene\PC.cpp:1175-1180 -> Actor.cpp:318-328).
//
// Why this matters here: 415 was CFieldChar's constructor placeholder
// (FieldChar.cpp:67), so every move we sent animated 415/469 = 0.88 of the run
// cycle while the live server's bare character moves at exactly 469. The capture's
// 469 group IS that bare case; its 1090/675/658 records are the same formula with
// gear and buffs on, which cannot be reproduced from a table (and are therefore
// NOT hardcoded - our characters wear nothing).
//
// What this side can supply: the stance's StnMSPD and - since the item columns
// were emitted - the eight worn categories' MSPD MULTIPLIERS (BODY, BOOTS,
// COSTUME, FACE, BACK, BELT, ACHIEVE, ARTIFACT; the script's own category
// names). No buffs run here, so MSPD_BM stays 0 as a named term. Every
// bare-handed stance in the game has StnMSPD = 1 and every grandfather item is
// neutral, so a character in nothing still walks at 469 today.
//
// WHY THE ITEM TERM IS NOT A PERCENTAGE, and why this matters HERE: the swing's
// position is interpolated along the walk segment at this speed. Measured
// 2026-11-01: all 235 swing positions sit exactly on that segment at the
// fraction this speed implies, so the MODEL is right and only the SPEED can be
// wrong. A body that really walks 5% faster than we assume is 5% ahead of every
// position we report, and every swing pulls it back. The owner's test boots
// (class 19533 "SettlerShoes") carry MSPD 1.05, so that is 492, not 469.
const (
	// MoveBaseSpeed is the constant inside SCR_Get_MSPD (get_status_pc.scp:1808),
	// in cm per second. NOT a guess and not a stance's RSPD.
	MoveBaseSpeed = 469.0

	// MoveMSPDBM is `self.MSPD_BM` in the script's SECOND line,
	// `value = floor(value * (100 + self.MSPD_BM + achievefix) / 100)`
	// (get_status_pc.scp:1809). It is the buff/BM term, in whole percent, and no
	// buff script runs on this side yet - so it is 0. It stays a named term
	// rather than being folded away, so the next reader can see it was not
	// forgotten. The achievement special fix is not implemented either.
	MoveMSPDBM = 0.0

	// DefaultSpeedCap is `var mspdlimit = 650` (get_status_pc.scp:1783).
	DefaultSpeedCap = 650.0

	// MeleeSpeedCap is what a MELEE or HYBRID stance is capped at
	// (get_status_pc.scp:1830-1832). It OVERRIDES the stance-name table above it,
	// which looks wrong but is the original's order.
	MeleeSpeedCap = 750.0

	// NoSpeedCap is the "uncapped" value for jobs 200/201/213 (the GameQueen test
	// jobs, get_status_pc.scp:1800) and for a character under Sprint.
	NoSpeedCap = 99999.0
)

// stanceSpeedCap is the stnchk table, verbatim from get_status_pc.scp:1784-1798.
// A stance named here may run faster than 650.
var stanceSpeedCap = map[string]float64{
	"Levitation":         1450,
	"ThePowerofDarkness": 1450,
	"Madness":            1100,
	"Trickinesys":        1100,
	"Pythonis":           800,
	"Zodiac":             1450,
	"Lama":               1450,
	"AcSpirit":           1450,
	"Aristocratic":       1450,
	"Hasaraahab":         1450,
	"Fatesentence":       1450,
	"AcSpiritCustom":     1450,
	"Cupiditas":          1450,
}

// flat750 are the three stances get_status_pc.scp:1840 also caps at 750.
var flat750 = map[string]bool{
	"DemolitionRoar": true,
	"FreezeOver":     true,
	"BlackCat":       true,
}

// jobSpeedUncapped is `var jobchk = { 200, 201, 213 }` (get_status_pc.scp:1800).
func jobSpeedUncapped(job uint32) bool {
	return job == 200 || job == 201 || job == 213
}

// MSPDTerms are the worn items' walk-speed MULTIPLIERS - one per category
// SCR_Get_MSPD multiplies into the base:
//
//	armor * boots * costume * face * back * belt * Achieve * artifact
//
// (get_status_pc.scp:1808). Each is a MULTIPLIER, not a percentage: 1 is
// neutral, 0.95 is slower, 1.05 is faster. The client's own item tables carry
// the column on every row of all eight families, so a 0 here means OUR file has
// no value for that slot, and Product reads it as the neutral 1 - a missing
// column must not stop the character dead.
type MSPDTerms struct {
	Body, Boots, Costume, Face, Back, Belt, Achieve, Artifact float64
}

// mspdFactor is one item term with "we have no value" (0) read as neutral.
func mspdFactor(term float64) float64 {
	if term == 0 {
		return 1
	}
	return term
}

// Product is the item factor of the whole body: every term multiplied, in the
// script's own order, each absent one counting as the neutral 1.
func (m MSPDTerms) Product() float64 {
	return mspdFactor(m.Body) * mspdFactor(m.Boots) * mspdFactor(m.Costume) *
		mspdFactor(m.Face) * mspdFactor(m.Back) * mspdFactor(m.Belt) *
		mspdFactor(m.Achieve) * mspdFactor(m.Artifact)
}

// gearMSPD picks the eight multipliers SCR_Get_MSPD reads off the WORN items.
// The slots come from wornGear, the one reader of the equipment array, so the
// walk speed and the Character Info sheet cannot disagree about what is worn.
func gearMSPD(g gear) MSPDTerms {
	return MSPDTerms{
		Body:     g.body.MSPD,
		Boots:    g.boots.MSPD,
		Costume:  g.costume.MSPD,
		Face:     g.face.MSPD,
		Back:     g.back.MSPD,
		Belt:     g.belt.MSPD,
		Achieve:  g.achieve.MSPD,
		Artifact: g.artifact.MSPD,
	}
}

// MoveSpeed is SCR_Get_MSPD for a character with NOTHING on: the stance's
// StnMSPD times the 469 base, then the original's cap. It is the same rule as
// MoveSpeedGear with every item term neutral, and it stays for the callers and
// tests that describe a bare body.
//
// An unknown stance (0, or an id the table does not have) falls back to the bare
// base 469: the formula's constant does not depend on the stance, only its
// multiplier does, so 469 is what a character with no stance really walks at.
func (t *StanceTable) MoveSpeed(stanceID, job uint32) float64 {
	value, _, _, _ := t.moveSpeedTerms(stanceID, job, MSPDTerms{})
	return value
}

// MoveSpeedGear is SCR_Get_MSPD for a dressed character: the same rule with the
// worn items' MSPD multipliers in it. Gear now MATTERS - a 1.05 boot is 492, not
// 469 - because the swing's position is interpolated along the walk at this
// speed, and being 5% off puts the body 5% ahead of every position we report.
func (t *StanceTable) MoveSpeedGear(stanceID, job uint32, items MSPDTerms) float64 {
	value, _, _, _ := t.moveSpeedTerms(stanceID, job, items)
	return value
}

// moveSpeedTerms is the whole of SCR_Get_MSPD in one place, and it also answers
// the pieces the TERMS log line needs: the stance's multiplier, the product of
// the item multipliers, and the cap that was in force.
func (t *StanceTable) moveSpeedTerms(stanceID, job uint32, items MSPDTerms) (value, stanceMSPD, itemMSPD, limit float64) {
	stanceMSPD, itemMSPD = 1.0, items.Product()
	row := StanceRow{}
	if t != nil {
		if r, ok := t.rows[int(stanceID)]; ok {
			row, stanceMSPD = r, r.StnMSPD
		}
	}

	// value = floor(469 * stance.StnMSPD * armor * boots * ... * artifact)
	value = math.Floor(MoveBaseSpeed * stanceMSPD * itemMSPD)
	// value = floor(value * (100 + MSPD_BM + achievefix) / 100)
	value = math.Floor(value * (100 + MoveMSPDBM) / 100)

	limit = DefaultSpeedCap
	if c, ok := stanceSpeedCap[row.Name]; ok {
		limit = c
	}
	if jobSpeedUncapped(job) {
		limit = NoSpeedCap
	}
	if row.ClassType == "MELEE" || row.ClassType == "HYBRID" {
		limit = MeleeSpeedCap
	}
	if flat750[row.Name] {
		limit = MeleeSpeedCap
	}
	// No Sprint and no buffs here, so the original's Sprint_BM and IsBuffApplied
	// branches (get_status_pc.scp:1843-1887) have nothing to add.

	if value > limit {
		value = limit
	}
	if row.Name == "SnipeShot" {
		value *= 0.7
	}
	return math.Floor(value), stanceMSPD, itemMSPD, limit
}

// mspdLogged remembers, per character handle, the last TERMS line printed - so
// the line appears when a term CHANGES and not once per move packet. A move
// packet is sent on every step; the answer only moves when the gear, the stance
// or the cap does, and that is what a reader wants to grep.
var (
	mspdMu     sync.Mutex
	mspdLogged = map[uint32]string{}
)

// mspdLogOnce prints the MSPD TERMS line when this character's answer changed.
//
//	zone: MSPD TERMS char N: 469 x stance StnMSPD x item a.bc -> NNN, cap CCC
//
// A nil logger (a test Handler built without one) is silent, not a panic.
func mspdLogOnce(l *log.Logger, handle uint32, stanceMSPD, itemMSPD, value, limit float64) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("zone: MSPD TERMS char %d: %s x stance %s x item %s -> %s, cap %s",
		handle, num(MoveBaseSpeed), num(stanceMSPD),
		strconv.FormatFloat(itemMSPD, 'f', -1, 64), num(value), num(limit))
	mspdMu.Lock()
	seen := mspdLogged[handle]
	mspdLogged[handle] = line
	mspdMu.Unlock()
	if seen != line {
		l.Print(line)
	}
}
