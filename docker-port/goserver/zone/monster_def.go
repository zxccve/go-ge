// R84 SLICE 2 (CONTINUED) - THE TARGET'S DEFENCE, READ FROM THE GAME'S OWN SCRIPT.
//
// WHAT THIS FILE IS. R84 slice 2 switched the packet from the placeholder to the
// game's own damage factor (`zone\skill_script.go`): `damage = ATK x ATKRatio`. That
// is the TOP of the reference's roll. This file supplies the term the reference
// applies to it next, and it is the reference's own rule, not a guess:
//
//	damage = damage * (100 - defense) / 100      common_monster.lua:2936-2937
//
// `defense` is a PERCENTAGE computed by the target's own script message handler,
// `SCR_MSG_MON_SKILL_PHYSICAL` (common_monster.lua:2884-2896):
//
//	defense = (self.MON_DEF - pierce - DefPierce - immune) / 2 - arg.AR / 2
//	if ( arg.Attacker == 'player' )  then
//	  defense = defense * ( (100 - from.IgnoreDEFPer) / 100 )
//	end
//	if(defense < 0  or  arg.SpecialType == 'IgnoreDef') then
//	  defense = 0
//	elseif ( IsBuffApplied(self, 'ProtectionField') == 'NO'  and  defense > 95 ) then
//	  defense = 95
//	elseif ( arg.SpecialType == 'SmallBane' ) then
//	  if(self.SizeType == 'SMALL'  or  self.SizeType == 'MEDIUM') then
//	    defense = 0
//	  elseif( self.SizeType == 'LARGE' ) then
//	    defense = 100
//	  end
//	end
//
// WHY A MONSTER TARGET RUNS THAT FUNCTION AT ALL: `CFieldChar::OnSkillPhysical`
// (source2\geServer\ZServerLib\FieldChar_Message.cpp:262-268) calls
// `ProcessScriptMsg(from, "SKILL_PHYSICAL", ...)`, and `CObject::ProcessScriptMsg`
// (source2\geServer\ZServerLib\Object.cpp:37-43) builds the entry name as
// `SCR_MSG_<GetObjTypeName()>_SKILL_PHYSICAL` - so a monster's is
// `SCR_MSG_MON_SKILL_PHYSICAL`, and the rule lives there in script, not in C++.
//
// THE INPUTS THE RULE NEEDS, AND WHERE EACH ONE COMES FROM TODAY:
//
//	MON_DEF         SCR_Get_MON_DEF(self), monster\get_status_monster.lua:481-505:
//	                  RaceType == 'Home' -> 9999, else
//	                  floor((Lv * 0.05 + 1) * Arm) + DEF_BM, then * (100 + DEFPER_BM)/100
//	                RUN for real through the script host (MonsterDefScript.Def), from
//	                the columns zone\create_tables.go now carries. AVAILABLE.
//	arg.AR          the ATTACKER's AR = SCR_Get_AR, which the Character Info sheet
//	                already computes (Handler.sheetFor). AVAILABLE.
//	immune          max(0, arg.IgnoreDEF + arg.DefIP - MON_IMP)
//	                  arg.IgnoreDEF  skill.IgnoreDEF   - a skill column. AVAILABLE
//	                  arg.DefIP      attacker's DefIP - NOT COMPUTED (below)
//	                  MON_IMP        SCR_Get_MON_IMP = floor(IMP_BM). AVAILABLE
//	DefPierce       from.IGN_DEF (the attacker's ignore-DEF stat) - NOT MODELLED
//	pierce          the skill's own piercing term. 'Pierce' is determinate
//	                (floor(MON_DEF * 0.1), common_monster.lua:2470-2473) and is
//	                applied; every other branch of that chain (:2489-2879) is gated on
//	                a buff, a skill-id table, or an IMCRandom roll, so it is NOT
//	                applied and the line names the special type when one is set.
//	from.IgnoreDEFPer  the attacker's ignore-DEF percent: the STANCE row's
//	                `StnIgnoreDEFPer` (geStanceTable.h:62). AVAILABLE; its buff part
//	                is 0 because this server has no buff system.
//	ProtectionField IsBuffApplied answers 'NO' (script\status.go:182), so the rule's
//	                >95 clamp applies exactly as the reference's no-buff path does.
//	SizeType        the monster row's SizeType - AVAILABLE (SmallBane branch).
//
// WHAT IS NOT SUPPLIED, AND IS THEREFORE 0, SAID OUT LOUD RATHER THAN GUESSED: the
// attacker's DefIP and IGN_DEF. Both are C++ character properties filled from
// equipment, artifacts, achievements and wardrobe items; this build holds none of
// that data, so it sends 0 for both and the log line PRINTS both, so a reader cannot
// mistake the number for a complete one. With no buff the reference's own immune term
// is then max(0, IgnoreDEF - MON_IMP), which is what this file computes.
//
// NOT IN THIS SLICE, and deliberately. The terms that FOLLOW the defence factor in
// the same function stay out, and are named here so the next lane knows what is still
// missing (all quoted from common_monster.lua, SCR_MSG_MON_SKILL_PHYSICAL):
//
//	:3056-3058  a FLOOR, not a multiplier: if(damage < arg.Hits) then damage = arg.Hits
//	:3060-3066  the AR/DR level fix: levelfix = clamp(arg.AR - self.MON_DR, -9, 5),
//	            damage = damage * (1 + levelfix/10) - it reads the attacker's AR a
//	            SECOND time, against the monster's own DR (SCR_Get_MON_DR), so a
//	            DEF-only number is not the finished hit
//	:3068       damage = damage * (arg.MSATK / 100)
//	:7864       lastdamage = floor(damage * (racefix + defiance - 1) * skinfix *
//	            arg.ATKRatio * sizefix * SkillCri * adddam + hitdam) * coldam
//
// Every one of those is a mastery/artifact/buff term, a race/size/skin table this
// lane has not read, or (the ATKRatio this slice already applies) a second use of the
// factor. None of them is guessed at here.
package zone

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"geserver/script"
)

// MonsterDefEntry is the script entry point that answers a monster's DEF,
// `SCR_Get_MON_DEF` - named by every monster row's own `CP_MON_DEF` column in
// `datatable_monster.xml` (7,725 rows, all of them).
const MonsterDefEntry = "SCR_Get_MON_DEF"

// MonsterImpEntry is the sibling that answers the immunity the damage rule
// subtracts. It is a one-line formula (`floor(self.IMP_BM)`,
// monster\get_status_monster.lua:558-562), so it is read from the row directly and
// quoted where it is used (`monsterIMP`), rather than paid for as a second script run
// per cast.
const MonsterImpEntry = "SCR_Get_MON_IMP"

// MonsterDefScript is ONE source: the game's own `SCR_Get_MON_DEF`, run for a
// monster. NIL MEANS OFF, and the cast then keeps the placeholder - the same rule
// DamageScript follows.
//
// It is the SAME host the damage script and the Character Info source run on, so its
// `self` is installed for exactly the length of one call (`script.Host.RunStatus` ->
// `withStatus`), and a raise is RETURNED, never raised through the zone's stack.
type MonsterDefScript struct {
	Host *script.Host
	// File is the corpus file the entry point was loaded from, for the startup line -
	// asked of the index, never assembled here.
	File string
	Log  func(format string, args ...any)

	// runMu serialises the run on the shared host, the same rule DamageScript
	// follows: a second cast must never see another monster's `self`.
	runMu sync.Mutex
}

// NewMonsterDefScript loads the entry point ONCE, at zone startup. An error means
// the zone keeps the placeholder and says so.
func NewMonsterDefScript(host *script.Host, logf func(string, ...any)) (*MonsterDefScript, error) {
	if host == nil {
		return nil, errors.New("no script host (this zone was started without one)")
	}
	if _, err := host.EnsureScript(MonsterDefEntry); err != nil {
		return nil, err
	}
	return &MonsterDefScript{Host: host, File: host.ScriptFile(MonsterDefEntry), Log: logf}, nil
}

// Def runs the game's own `SCR_Get_MON_DEF` for ONE monster and answers the DEF it
// computed.
//
// ok=false plus an error means the script did not answer - a raise, a missing entry
// point - and the caller keeps the placeholder and says so. NOTHING HERE CONSUMES THE
// ERROR: a cast is never failed because a defence could not be read.
func (d *MonsterDefScript) Def(m Monster) (def float64, ok bool, err error) {
	if d == nil || d.Host == nil {
		return 0, false, errors.New("no monster defence script is armed")
	}

	var raised any
	d.runMu.Lock()
	func() {
		// A panic in the Lua binding must not take the zone's reply down with it.
		defer func() { raised = recover() }()
		results := d.Host.RunStatus(nil, monsterStatusSelf(m),
			[]script.StatusEntrySpec{{Label: "DEF", Script: MonsterDefEntry}})
		if len(results) != 1 {
			err = fmt.Errorf("%s answered %d result(s), want 1", MonsterDefEntry, len(results))
			return
		}
		if results[0].Err != nil {
			err = results[0].Err
			return
		}
		def = results[0].Value
	}()
	d.runMu.Unlock()

	if raised != nil {
		return 0, false, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	if err != nil {
		return 0, false, err
	}
	return def, true, nil
}

// MonsterRankEntry is one of the three computed properties a debuff rule reads off
// the DEFENDER: `SCR_RESULT_BATTLEBUFF_SKILL` asks for `self.MON_DR`,
// `self.MON_RSTAT` and `self.MON_DefLayer` (pc\common_pc.scp:6752-6760), and the
// monster's own row names the entry point for each of them in its `CP_MON_*` columns
// (`CP_MON_DR="SCR_Get_MON_DR"`, ...). The reference CACHES the answer on the body
// (MonsterNPC.h:143); this build does the same, once, at spawn.
type MonsterRankEntry struct {
	// Prop is the property name the rule reads.
	Prop string
	// Entry is the corpus function that computes it.
	Entry string
}

// MonsterRankEntries is the three, in the order a spawn computes them.
var MonsterRankEntries = []MonsterRankEntry{
	{"MON_DR", "SCR_Get_MON_DR"},
	{"MON_DefLayer", "SCR_Get_MON_DefLayer"},
	{"MON_RSTAT", "SCR_Get_MON_RSTAT"},
}

// Ranks runs the three computed-property entry points for ONE body and answers their
// values by property name. It is called at SPAWN, so a later script message reads a
// cached number instead of paying for three Lua calls in the middle of a cast - the
// same shape the reference has.
//
// ok=false plus an error means the entries could not be read; the caller then leaves
// the three at 0 (which is a GAP, and the caller says so once).
func (d *MonsterDefScript) Ranks(m Monster) (map[string]float64, bool, error) {
	if d == nil || d.Host == nil {
		return nil, false, errors.New("no monster defence script is armed")
	}
	out := make(map[string]float64, len(MonsterRankEntries))
	for _, e := range MonsterRankEntries {
		if _, err := d.Host.EnsureScript(e.Entry); err != nil {
			return nil, false, fmt.Errorf("%s: %w", e.Entry, err)
		}
	}
	var raised any
	var err error
	d.runMu.Lock()
	func() {
		defer func() { raised = recover() }()
		for _, e := range MonsterRankEntries {
			var results []script.StatusResult
			results = d.Host.RunStatus(nil, monsterStatusSelf(m),
				[]script.StatusEntrySpec{{Label: e.Prop, Script: e.Entry}})
			if len(results) != 1 {
				err = fmt.Errorf("%s answered %d result(s), want 1", e.Entry, len(results))
				return
			}
			if results[0].Err != nil {
				err = fmt.Errorf("%s: %w", e.Entry, results[0].Err)
				return
			}
			out[e.Prop] = results[0].Value
		}
	}()
	d.runMu.Unlock()

	if raised != nil {
		return nil, false, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// monsterStatusSelf is the `self` the monster status script reads: the properties
// `SCR_Get_MON_DEF` (and the rest of the file) ask for, by the names the SCRIPT uses.
// Every number here is a column of the monster's own row - nothing is derived, and a
// row that does not state one answers 0 through the property fallback, which is the
// reference's own answer for an absent column.
//
// THE NAMES ARE THE SCRIPT'S, NOT OURS: `MON_DEF` and `MON_IMP` are NOT here on
// purpose. In the reference those are CACHED, COMPUTED properties (MonsterNPC.h:143):
// the engine runs `SCR_Get_MON_DEF` itself and stores the answer as `MON_DEF`, so the
// formula's input is the row's `Arm`/`DEF_BM`/`DEFPER_BM`, never `MON_DEF`.
func monsterStatusSelf(m Monster) script.StatusSelf {
	return script.StatusSelf{
		ClassName: "MON",
		Props: map[string]float64{
			"ClassID":   float64(m.ClassID),
			"Lv":        float64(m.Level),
			"Arm":       m.Arm,
			"DEF_BM":    m.DEFBM,
			"DEFPER_BM": m.DEFPERBM,
			"IMP_BM":    m.IMPBM,
			// THE RANK / LAYER / RESIST INPUTS. `SCR_Get_MON_DR` reads MobRank and
			// DR_BM, `SCR_Get_MON_DefLayer` reads DefLayer and DefLayer_BM, and
			// `SCR_Get_MON_RSTAT` reads RSTAT and RSTAT_BM - the three numbers a
			// script message about a debuff asks the defender for
			// (zone\skillmsg.go). The COMPUTED answers (MON_DR, MON_DefLayer,
			// MON_RSTAT) are NOT here: the reference caches them from those entry
			// points, and so does this build.
			"MobRank":     m.MobRank,
			"DR_BM":       m.DRBM,
			"DefLayer":    m.DefLayer,
			"DefLayer_BM": m.DefLayerBM,
			"RSTAT":       m.RSTAT,
			"RSTAT_BM":    m.RSTATBM,
			// THE COMPUTED ANSWERS, cached on the body at spawn exactly as the
			// reference caches them (MonsterNPC.h:143): `SCR_RESULT_BATTLEBUFF_SKILL`
			// reads `self.MON_DR` / `self.MON_RSTAT` / `self.MON_DefLayer` directly.
			"MON_DR":       m.MONDR,
			"MON_RSTAT":    m.MONRSTAT,
			"MON_DefLayer": m.MONDefLayer,
			// THE MAX-HP TERMS, for the sibling entry point `SCR_Get_MON_MHP`
			// (zone\monster_hp.go). `SCR_Get_MON_DEF` does not read them, and the
			// property fallback would answer 0 for each if they were missing - which
			// is the correct answer for a column the row does not state.
			"CON":       m.CON,
			"MHPFix":    m.MHPFix,
			"MHP_BM":    m.MHPBM,
			"ADDMHP_BM": m.ADDMHPBM,
		},
		Strings: map[string]string{
			"RaceType": m.RaceType,
			"SizeType": m.SizeType,
		},
	}
}

// monsterIMP is `SCR_Get_MON_IMP(self)` (monster\get_status_monster.lua:558-562):
//
//	local value = 0
//	value = value + self.IMP_BM
//	return math.floor(value)
//
// It is a floor of one column, so it is read here rather than run as a second script
// per cast. `IMP_BM` is the monster's own immunity to defence-ignoring points, and it
// is the term `immune` subtracts.
func monsterIMP(m Monster) float64 { return math.Floor(m.IMPBM) }

// defenceInputs is every term of the rule quoted at the top of this file, as plain
// numbers, so the arithmetic can be checked on its own without a script host.
type defenceInputs struct {
	MonsterDEF     float64 // SCR_Get_MON_DEF(self)
	MonsterIMP     float64 // SCR_Get_MON_IMP(self)
	AttackerAR     float64 // arg.AR    (self.AR of the attacker)
	SkillIgnoreDEF float64 // arg.IgnoreDEF (skill row)
	AttackerDefIP  float64 // arg.DefIP  (0 today - see the file comment)
	AttackerIGNDEF float64 // DefPierce = from.IGN_DEF (0 today)
	IgnoreDEFPer   float64 // from.IgnoreDEFPer (stance StnIgnoreDEFPer; buffs 0)
	Pierce         float64 // the special type's own pierce term ('Pierce' only today)
	SpecialType    string
	SizeType       string
	// ProtectionField is the target's own buff. A MONSTER has no buff list in this
	// slice - the list is per player character (zone\buff.go), so the reference's
	// `IsBuffApplied(monster,...) == 'NO'` is still the answer here; the field is
	// here so the expression can be tested both ways.
	ProtectionField bool
}

// percent is the quoted expression, line for line, and answers the defence
// PERCENTAGE plus the short name of the branch that decided it - so the log line can
// say WHICH rule was applied instead of only its result.
//
// The order of the branches is the reference's, and it matters: `SmallBane` is an
// `elseif` AFTER the >95 clamp, so it only runs for a defence that was neither
// negative nor above 95.
func (in defenceInputs) percent() (defense float64, applied string) {
	immune := math.Max(0, in.SkillIgnoreDEF+in.AttackerDefIP-in.MonsterIMP)
	defense = (in.MonsterDEF-in.Pierce-in.AttackerIGNDEF-immune)/2 - in.AttackerAR/2
	// `arg.Attacker == 'player'`: every cast this zone prices has a player as the
	// attacker (a monster's own attack never reaches this file - the monster lanes
	// build no skill packet here).
	defense = defense * (100 - in.IgnoreDEFPer) / 100
	switch {
	case defense < 0 || strings.EqualFold(in.SpecialType, "IgnoreDef"):
		return 0, "defense<0 (or IgnoreDef) -> 0"
	case !in.ProtectionField && defense > 95:
		return 95, "clamped to 95"
	case strings.EqualFold(in.SpecialType, "SmallBane"):
		switch strings.ToUpper(in.SizeType) {
		case "SMALL", "MEDIUM":
			return 0, "SmallBane on " + strings.ToUpper(in.SizeType) + " -> 0"
		case "LARGE":
			return 100, "SmallBane on LARGE -> 100"
		}
		return defense, "SmallBane, size " + in.SizeType + " not one it names"
	}
	return defense, "the formula"
}

// pierceFor is the ONE determinate branch of the reference's piercing chain that
// needs no buff, no random roll and no skill-id table
// (common_monster.lua:2470-2473):
//
//	if(arg.SpecialType == 'Pierce' ) then
//		pierce = math.floor(self.MON_DEF * 0.1)
//
// Every other branch is gated on `IsBuffApplied` (always 'NO' here), on a random
// roll, or on a specific skill id. `pierced` is false for those, and the caller's log
// line names the special type so the gap is visible.
func pierceFor(specialType string, def float64) (pierce float64, pierced bool) {
	if strings.EqualFold(specialType, "Pierce") {
		return math.Floor(def * 0.1), true
	}
	return 0, false
}

// applyDefence is the reference's own multiply (common_monster.lua:2936-2937):
//
//	damage = damage * (100 - defense) / 100
//
// It floors at 1 like every other damage this server sends: a landed cast must put a
// number on screen, and 0 reads as a miss the client animates as a death. The floor
// is the LAST step, which is the order the reference uses - its own `lastdamage` is
// floored once, at common_monster.lua:7864, after every factor.
func applyDefence(damage, defense float64) float64 {
	d := damage * (100 - defense) / 100
	if d < 1 {
		return 1
	}
	return d
}

// monsterDefence reads the target's DEF through the game's own SCR_Get_MON_DEF, as
// armed at zone startup (Handler.Defence).
//
// AN UNARMED SOURCE IS AN ERROR, NOT A ZERO. 0 is a real DEF - it is the Scarecrow's
// own - so answering 0 for "could not read" would price a defence nobody measured,
// and the cast would look right for the wrong reason. The caller keeps the
// placeholder and logs the reason.
func (h *Handler) monsterDefence(m Monster) (float64, bool, error) {
	if h == nil || h.Defence == nil {
		return 0, false, errors.New("no monster defence source is armed (Handler.Defence is nil)")
	}
	return h.Defence.Def(m)
}

// MonsterDefByHandle is the same read for a monster named by HANDLE, and it exists
// for the -spawndummy probe: a dummy is only worth spawning if the log names the DEF
// the game's own script computes for it, so the probe can check the row it landed on
// rather than a number written down by hand. A handle this zone is not running is an
// error, not a zero - see monsterDefence.
func (h *Handler) MonsterDefByHandle(handle uint32) (float64, bool, error) {
	m, ok := h.monsterByHandle(handle)
	if !ok {
		return 0, false, fmt.Errorf("handle %d is not a monster this zone is running", handle)
	}
	return h.monsterDefence(m)
}

// defenceInputsFor fills every term of the rule quoted at the top of this file for
// ONE cast.
//
// `sheet` is the caster's sheet the caller ALREADY read to build the placeholder, so
// `AR` here and `ATK` there come from ONE run of the status script and cannot
// disagree. `IgnoreDEFPer` is the CURRENT stance row's own `StnIgnoreDEFPer` - the
// reference's stance term (geStanceTable.h:62); its buff part is 0 because this
// server has no buff system, which is the reference's own value with no buff on.
//
// `AttackerDefIP` and `AttackerIGNDEF` are 0 and are NOT guessed: this build holds no
// DefIP and no wardrobe/artifact IGN_DEF (see the file comment). The log line prints
// them, so a reader sees the two zeroes instead of assuming a full rule.
func (h *Handler) defenceInputsFor(sheet StatusSheet, c Character, row SkillRow,
	m Monster, def float64) defenceInputs {

	ignorePer := 0.0
	if h != nil && h.Stances != nil {
		if raw, ok := h.Stances.RawStance(c.Stance); ok {
			ignorePer = atof(raw["StnIgnoreDEFPer"])
		}
	}
	return defenceInputs{
		MonsterDEF:      def,
		MonsterIMP:      monsterIMP(m),
		AttackerAR:      sheet.AR,
		SkillIgnoreDEF:  row.IgnoreDEF,
		AttackerDefIP:   0, // no DefIP source in this build - see the file comment
		AttackerIGNDEF:  0, // no wardrobe/artifact IGN_DEF in this build
		IgnoreDEFPer:    ignorePer,
		SpecialType:     row.SpecialType,
		SizeType:        m.SizeType,
		ProtectionField: false, // a monster holds no buff list (zone\buff.go), so 'NO'
	}
}
