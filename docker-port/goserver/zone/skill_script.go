// R84 SLICE 2 - THE GAME'S OWN DAMAGE SCRIPT DECIDES THE NUMBER.
//
// WHAT THIS IS. The game's own formula is the corpus script `skill\attack.lua`'s
// `SCR_SKILL_ATTACK(self, target, skill, splash, ret, targetcount)`, which 5,621
// of the 5,626 rows in `datatable_skill.xml` name. Slice 1 ran it beside the
// placeholder and threw the answer away. This slice USES the answer: the factor
// the script computes is multiplied by OUR OWN attack, and that product is what
// the 3573 / the 3581 record carries.
//
// THE NUMBER IS THE FACTOR MULTIPLIED INTO OUR OWN ATTACK:
//
//	deal = ATK x factor        (floored at 1)
//
// NOT `ATK x factor / 100`. `arg.ATKRatio` is a MULTIPLIER: the game multiplies
// the base damage by it in both copies of the final hit
// (script\monster\common_monster.scp:7864, script\pc\common_pc.scp:4933), and the
// `/100` in `skill\attack.scp:359-362` is only the `SklATK` percent -> ratio
// conversion inside the factor itself. Dividing again was 100x too small.
//
// THE TARGET'S DEFENCE IS APPLIED FROM THE GAME'S OWN SCRIPT. The `(100 - defence)/100`
// multiply the reference performs next (`common_monster.scp:7864`, `common_pc.scp:4933`)
// is computed by the TARGET's own script message handler, `SCR_MSG_MON_SKILL_PHYSICAL`
// (`script\monster\common_monster.scp:2884-2896`), from `SCR_Get_MON_DEF` and the
// attacker's AR - so it is read and applied here (zone\monster_def.go, which quotes the
// rule and every input). What is still NOT modelled, and is named rather than faked:
// evasion, the AR-vs-DR level fix, the race/size/skin tables, and the live server's own
// model (its billion-scale multipliers and its misses when the gap is too big). The
// reference server's model is not copied: the owner asked for OUR formula to drive the
// attack, not the reference server's (owner, 2026-11-04). The two live captures under
// `captures\scaredef1\` and `captures\scaredef2\` are evidence that the two models
// differ, not a specification.
//
// FALLING BACK IS NORMAL, NOT AN ERROR. With no script armed the placeholder is
// returned untouched and nothing is logged - a checkout with no corpus is a
// legitimate state and stays byte-identical to what it was. A script that raises
// costs the cast nothing either: the placeholder goes out and one line says so.
//
// THE MISSING-BUILTIN LIST IS THE DELIVERABLE. The host records every builtin a
// script asks for and nothing answers (`__on_missing`, script\host.go:196 ->
// `Host.Missing()`, host.go:505). The script runs on the SAME host as the
// Character Info source and the clicks, so the raw list is the whole process's,
// not this cast's - so the first cast SNAPSHOTS it before and diffs it after, and
// the line names exactly the names THAT cast asked for. That diff is what decides
// the next slice.
package zone

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"geserver/script"
)

// SkillAttackEntry is the entry point every skill row names in its own `Script`
// column, and the only one this file calls.
const SkillAttackEntry = "SCR_SKILL_ATTACK"

// skillHelperEntries are the corpus entry points SCR_SKILL_ATTACK CALLS but that
// no eager library defines, so a run would reach the missing recorder for names
// the corpus really does implement - a false entry in the one list this slice
// exists to produce.
//
//	SCR_PC_LIB_BROAD_HATE  pc\pc_lib.lua:2       (attack.lua:145 calls it)
//	SCR_Get_CAST           pc\get_status_pc.lua:3515 (attack.lua:268, the cast time)
//
// They are loaded the same way the status source loads SCR_SKILL_BUFF
// (script\status.go:439): by NAME, through the index, never by a hardcoded path.
var skillHelperEntries = []string{"SCR_PC_LIB_BROAD_HATE", "SCR_Get_CAST"}

// skillMissingReportCap is how many missing names one log line prints. The
// measured list for ONE cast is ~50 names (history\script-engine.md measured 58
// for the file as a whole), so a cap keeps the line readable and the count at the
// end keeps it complete.
const skillMissingReportCap = 40

// DamageScript is ONE source: the game's damage formula, run for a cast.
//
// NIL MEANS OFF, and the zone then behaves exactly as it did before R84 slice 1 -
// the placeholder, no line. That is the state a checkout with no corpus, or a
// zone started with scripts disabled, is in, and the startup line says so rather
// than leaving a silent gap.
type DamageScript struct {
	Host *script.Host
	// File is the corpus file the entry point was loaded from, for the startup
	// line - the real path, asked of the index rather than assembled here.
	File string
	// Sheet is the zone's Character Info source when one is armed. The damage
	// formula calls the SAME builtins the sheet does (`GetEquipItem`,
	// `GetCurrentStance`, `GetCommander`), so it is handed the same answers; with
	// no sheet every item answers a zero table and the formula reads a naked
	// character, which is honest but wrong and says so once in the log.
	Sheet *StatusScript
	Log   func(format string, args ...any)

	// runMu serialises the run: it installs a source and a `self` on the shared
	// host for the length of the call, exactly as StatusScript.runMu does, and a
	// second cast must not see them.
	runMu sync.Mutex
	// missingOnce prints the worklist after the FIRST cast that ran the script.
	missingOnce sync.Once
}

// NewDamageScript builds the source ONCE, at zone startup: it loads
// `skill\attack.lua` (and the two helpers above) and remembers where it came
// from. An error means the zone runs on the placeholder alone and says so.
func NewDamageScript(host *script.Host, logf func(string, ...any)) (*DamageScript, error) {
	if host == nil {
		return nil, errors.New("no script host (this zone was started without one)")
	}
	if _, err := host.EnsureScript(SkillAttackEntry); err != nil {
		return nil, err
	}
	d := &DamageScript{Host: host, File: host.ScriptFile(SkillAttackEntry), Log: logf}
	for _, name := range skillHelperEntries {
		if _, err := host.EnsureScript(name); err != nil {
			d.logf("zone: skill damage script: %s is NOT loadable (%v) - a call to it "+
				"will be recorded as a missing builtin and the formula will read a 0 "+
				"where the game's own helper answers", name, err)
		}
	}
	return d, nil
}

// Damage runs the game's own formula for ONE cast and answers the damage factor
// it computed.
//
// ok=false plus an error means the script did not answer - a raise, a missing
// entry point, a bag it never filled - and the caller logs the error and keeps
// the placeholder. NOTHING HERE CONSUMES THE ERROR: a cast is never failed,
// delayed or reordered because the instrument could not answer.
func (d *DamageScript) Damage(c Character, row SkillRow, target *Monster,
	splash bool, targetCount int) (damage float64, ok bool, err error) {

	if d == nil || d.Host == nil {
		return 0, false, errors.New("no damage script is armed")
	}

	call := script.SkillAttackCall{
		Source:      d.source(c),
		Self:        statusSelfFor(c),
		Skill:       skillScriptRow(row),
		Target:      skillScriptTarget(target),
		Splash:      splash,
		TargetCount: targetCount,
	}

	// The recorder's state BEFORE this cast, so the worklist this slice prints is
	// the names THIS cast asked for and not the whole process's.
	before := d.Host.Missing()

	var raised any
	d.runMu.Lock()
	func() {
		// A panic in the Lua binding must not take the zone's reply down with it.
		defer func() { raised = recover() }()
		damage, err = d.Host.RunSkillAttack(call)
	}()
	d.runMu.Unlock()

	d.reportMissingOnce(before, d.Host.Missing())

	if raised != nil {
		return 0, false, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	if err != nil {
		return 0, false, err
	}
	return damage, true, nil
}

// source is the status source the formula's builtins read, for THIS character.
// A nil Sheet answers nil, which the status builtins treat as "no source wired":
// a zero item table, an empty stance row, one line each.
func (d *DamageScript) source(c Character) script.StatusSource {
	if d.Sheet == nil {
		return nil
	}
	return &statusRun{script: d.Sheet, c: c}
}

// logf writes one line, or nothing when no logger is wired (a unit test).
func (d *DamageScript) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// reportMissingOnce prints the worklist ONCE per process, after the first cast
// that ran the script.
//
// THE DIFF IS THE POINT. The host is shared with the Character Info source and
// every click, so `Host.Missing()` holds whatever the whole process has asked
// for; subtracting the snapshot taken before this cast leaves exactly what THIS
// cast needed and nothing answered. Most-called first, like the recorder's own
// order, so the top of the list is the first thing to write.
func (d *DamageScript) reportMissingOnce(before, after []script.Missing) {
	d.missingOnce.Do(func() {
		asked := missingSince(before, after)
		if len(asked) == 0 {
			d.logf("zone: SKILL SCRIPT host: 0 missing builtin(s) - every name THIS cast " +
				"asked for had a binding")
			return
		}
		shown := asked
		left := 0
		if len(shown) > skillMissingReportCap {
			left = len(shown) - skillMissingReportCap
			shown = shown[:skillMissingReportCap]
		}
		parts := make([]string, 0, len(shown))
		for _, m := range shown {
			parts = append(parts, fmt.Sprintf("%s x%d", m.Name, m.Calls))
		}
		tail := ""
		if left > 0 {
			tail = fmt.Sprintf(", ... and %d more (not printed)", left)
		}
		d.logf("zone: SKILL SCRIPT host: %d missing builtin(s) that THIS cast asked for: %s%s",
			len(asked), strings.Join(parts, ", "), tail)
	})
}

// missingSince subtracts the `before` snapshot from `after`: the names whose
// count went UP, most-called-in-this-run first.
func missingSince(before, after []script.Missing) []script.Missing {
	was := make(map[string]int, len(before))
	for _, m := range before {
		was[m.Name] = m.Calls
	}
	out := make([]script.Missing, 0, len(after))
	for _, m := range after {
		if m.Calls > was[m.Name] {
			out = append(out, script.Missing{Name: m.Name, Calls: m.Calls - was[m.Name]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// skillScriptRow is the `skill` argument: the row the reference passes as its
// skill object, as plain data.
//
// EVERY NAME HERE IS ONE THE CORPUS READS (`attack.lua`): `ClassID` picks the
// branches, `SklATK`/`SklLv`/`SklLv_BM`/`SplDam` are the damage factor,
// `UseType`/`ValueType` route the last test, and the rest are carried into the
// argument bag. Anything NOT set here answers 0 through the table's property
// fallback, which is the honest value for a column this build does not hold
// (`Hits`, `KDRank`, `PerentSkillID`, ...): 0 is what an unset integer reads in
// the reference's own IES layer too.
func skillScriptRow(row SkillRow) map[string]any {
	return map[string]any{
		"ClassID":     float64(row.ID),
		"ClassName":   row.Code,
		"EngName":     row.Name,
		"SklATK":      row.SklATK,
		"SklLv":       row.SklLv,
		"SklLv_BM":    row.SklLvBM,
		"SplDam":      row.SplDam,
		"UseType":     row.UseType,
		"ValueType":   row.ValueType,
		"MaxR":        row.MaxR,
		"MinR":        row.MinR,
		"SplashRange": row.SplashRange,
		"SplLimit":    float64(row.SplLimit),
		"SpendSP":     float64(row.SpendSP),
		"CastTime":    float64(row.CastTime),
		"HoldTime":    float64(row.HoldTime),
		// THE BUFF COLUMNS the skill's own script copies into the magical bag
		// (`magicalskillarg.Buff = skill.Buff`, ge\script\skill\buff.scp:36-40) and
		// the message script rules on (`arg.AtkLayer`, `arg.ClassID`, `arg.SklLv`,
		// `arg.RemoveBuff1..5`). Carried verbatim from the row: no rule is applied to
		// any of them here.
		"Buff":        row.Buff,
		"BuffType":    row.BuffType,
		"BuffFreq":    row.BuffFreq,
		"BuffID":      row.BuffID,
		"ClassType":   row.ClassType,
		"AtkLayer":    row.AtkLayer,
		"HateAmnt":    row.HateAmnt,
		"RemBuff1":    row.RemBuff1,
		"RemBuff2":    row.RemBuff2,
		"RemBuff3":    row.RemBuff3,
		"RemBuff4":    row.RemBuff4,
		"RemBuff5":    row.RemBuff5,
		// The script spells the removal columns `RemoveBuff1..5` on the BAG
		// (`magicalskillarg.RemoveBuff1 = skill.RemBuff1`, buff.scp:26-30); carrying
		// this spelling too keeps a script that reads `skill.RemoveBuff1` honest
		// without renaming the table's own column.
		"RemoveBuff1": row.RemBuff1,
		"RemoveBuff2": row.RemBuff2,
		"RemoveBuff3": row.RemBuff3,
		"RemoveBuff4": row.RemBuff4,
		"RemoveBuff5": row.RemBuff5,
	}
}

// skillScriptTarget is the `target` argument, from the monster this zone is
// running - or an EMPTY table when the cast hit nobody.
//
// WHY EMPTY AND NOT NIL: the script indexes `target` before it ever tests whether
// anything was hit (`target.ClassName`, attack.lua:43, against the
// `UseType == 'AREA' and targetCount == 0` early return at :1067), so a nil here
// is an "attempt to index a nil value" and the ground cast at empty ground - the
// one the empty-ground capture measured - would answer a raise instead of a
// number. `SendMsg` then refuses a handle-less target with its own line, which is
// the same answer the original gives for a body that is not there.
func skillScriptTarget(m *Monster) map[string]any {
	out := map[string]any{}
	if m == nil {
		return out
	}
	out["Handle"] = float64(m.Handle)
	out["ClassID"] = float64(m.ClassID)
	out["ClassName"] = "Monster"
	out["Lv"] = float64(m.Level)
	out["HP"] = float64(m.HP)
	out["MHP"] = float64(m.HP)
	out["X"] = float64(m.X)
	out["Y"] = float64(m.Y)
	out["Z"] = float64(m.Cell)
	return out
}

// skillScriptDamage decides the damage ONE cast puts on the wire, and it is the
// ONLY place that decides it: both damage sites call this and use what it returns.
//
//	no script armed (h.Damage == nil) -> `placeholder`, silently. A zone with no
//	  corpus is a legitimate state and must stay exactly what it was before R84.
//	script raises / does not answer   -> `placeholder`, plus one line naming the
//	  error. A broken script never costs the cast its hit.
//	target DEF unreadable             -> `placeholder`, plus one line naming the
//	  reason. 0 is a REAL DEF (the Scarecrow's), so it is never substituted for
//	  "could not read" - that would price a defence nobody measured.
//	everything answers                -> `ATK x factor`, then the reference's own
//	  `x (100 - defence)/100`, plus one line naming the factor, its damage, the
//	  placeholder, the target's DEF and the branch of the rule that decided the
//	  percentage - so a reader can re-derive the number instead of trusting it.
//
// THE FACTOR IS A MULTIPLIER, NOT A PERCENT, AND IT IS NEVER DIVIDED. The script's
// last number is `ATKRatio` (`skill\attack.scp:359-362`):
//
//	physicalskillarg.ATKRatio = skill.SklATK * ((skill.SklLv + ADDSklLv +
//	                            skill.SklLv_BM) * 10 + 100) / 100
//
// and the game then MULTIPLIES the base damage by it - both copies of the final
// hit, monster and player:
//
//	script\monster\common_monster.scp:7864  lastdamage = math.floor((damage * (...)
//	    * arg.ATKRatio * sizefix * SkillCri * adddam + hitdam) * coldam)
//	script\pc\common_pc.scp:4933            lastdamage = math.floor(damage * (...)
//	    * arg.ATKRatio * ... )
//
// The `/100` above belongs to the `SklATK` percent -> ratio conversion inside the
// factor, and to nothing else: dividing the product again would make every cast
// 100x too small. `Stun shock` (63011: SklATK 12, SklLv 1) is ratio 13.2, so at
// ATK 3736 the game's arithmetic is 49,315 - not 493.
//
// OUR ATTACK IS THE BASE: `sheet.ATK`, the panel number, which is `RATK`/`LATK`
// (`attack.scp:214-215`, spent at `pc\common_pc.scp:3234-3270`) up to
// `AddATK x (1 - 1/RHits)`. It is passed IN by the caller, so it is read once and
// never re-read here, and it is NOT multiplied by the stance's hits - the hits
// term is already inside that difference, so multiplying would double it.
//
// WHAT THIS NUMBER IS, EXACTLY: the TOP of the game's `RHR` roll with the reference's
// defence factor applied - `ATK x factor x (100 - defence)/100` - and NOT its evasion
// or hit roll. The defence percentage is the target's own
// `SCR_MSG_MON_SKILL_PHYSICAL` expression, quoted in `zone\monster_def.go`; the terms
// that follow it in the same function (evasion, the AR-vs-DR level fix, race/size/skin
// tables, mastery) are named there and NOT applied. `target` is handed to the script
// because the script indexes it (attack.lua:43), and it is read for its DEF through
// `SCR_Get_MON_DEF` - the game's own entry point, never our arithmetic.
//
// The two shapes:
//
//	zone: SKILL_SCRIPT slice 2: skill 63011 ("Stun shock") char 455 "Grace" - the
//	  script says factor 13.2 (= 49315.2 damage at ATK 3736), the placeholder says
//	  448.32, the packet carries 49315.2
//	zone: SKILL_SCRIPT slice 2: skill 63011 ("Stun shock") char 455 "Grace"
//	  RAISED: <err> - the packet carries the placeholder 448.32
//
// `%g` for every number and never `%d`: the factor and the damage are float64 and
// an integer format hides the fraction that is really there (the same rule the
// SKILL_CAST lines follow).
func (h *Handler) skillScriptDamage(c Character, row SkillRow, target *Monster,
	splash bool, targetCount int, placeholder float64, sheet StatusSheet) float64 {

	if h == nil || h.Damage == nil {
		return placeholder
	}
	factor, ok, err := h.Damage.Damage(c, row, target, splash, targetCount)
	name := h.SkillName(row.ID)
	if err != nil || !ok {
		h.Log.Printf("zone: SKILL_SCRIPT slice 2: skill %d (%q) char %d %q RAISED: %v - "+
			"the packet carries the placeholder %g",
			row.ID, name, c.Handle, c.Name, err, placeholder)
		return placeholder
	}
	// THE FACTOR MULTIPLIES THE ATTACK - it is not a percentage (see the header):
	// `arg.ATKRatio` meets the base damage by multiplication in
	// common_monster.scp:7864 / common_pc.scp:4933, so there is no `/100` here.
	// The floor at 1 is the placeholder's own rule (a landed cast must show a
	// number), kept without borrowing `castDamage`, which would divide by 100.
	deal := sheet.ATK * factor
	if deal < 1 {
		deal = 1
	}

	// NO TARGET, NO DEFENCE. A ground cast at empty ground records no victim, so
	// there is nothing to read a DEF from and no body on the wire sees the number.
	if target == nil {
		h.Log.Printf("zone: SKILL_SCRIPT slice 2: skill %d (%q) char %d %q - the script "+
			"says factor %g (= %g damage at ATK %g), the placeholder says %g, but this "+
			"cast recorded no target, so there is no DEF to apply - the packet carries %g",
			row.ID, name, c.Handle, c.Name, factor, deal, sheet.ATK, placeholder, deal)
		return deal
	}

	// THE TARGET'S OWN DEF, through the game's own SCR_Get_MON_DEF. A source that
	// cannot answer is NOT a zero: 0 is a real DEF (the Scarecrow's), so the
	// placeholder stays and the line names the failure.
	def, defOK, defErr := h.monsterDefence(*target)
	if defErr != nil || !defOK {
		h.Log.Printf("zone: SKILL_SCRIPT slice 2: skill %d (%q) char %d %q - the script "+
			"says factor %g (= %g damage at ATK %g), but the target's DEF could not be "+
			"read: %v - the packet carries the placeholder %g",
			row.ID, name, c.Handle, c.Name, factor, deal, sheet.ATK, defErr, placeholder)
		return placeholder
	}

	in := h.defenceInputsFor(sheet, c, row, *target, def)
	pierce, _ := pierceFor(row.SpecialType, def)
	in.Pierce = pierce
	defense, applied := in.percent()

	// A special type that names one of the reference's OTHER piercing branches is not
	// applied (no buff system, no skill-id table, no random roll this lane can honour)
	// and the line says so - a silent skip would read as "applied and 0". The table's
	// own "none" is the ABSENCE of a special type, not an unmodelled one.
	note := ""
	if special := strings.ToUpper(strings.TrimSpace(row.SpecialType)); special != "" &&
		special != "NONE" && special != "PIERCE" && special != "IGNOREDEF" &&
		special != "SMALLBANE" {
		note = fmt.Sprintf(", special type %q NOT modelled - not applied", row.SpecialType)
	}

	carries := applyDefence(deal, defense)
	h.Log.Printf("zone: SKILL_SCRIPT slice 2: skill %d (%q) char %d %q - the script says "+
		"factor %g (= %g damage at ATK %g), the placeholder says %g, target %d (%q) "+
		"DEF %g -> defence %g%% by %q (AR %g, IMP %g, IgnoreDEF %g, IgnoreDEFPer %g, "+
		"pierce %g%s), the packet carries %g",
		row.ID, name, c.Handle, c.Name, factor, deal, sheet.ATK, placeholder,
		target.Handle, h.MonsterName(*target), def, defense, applied,
		in.AttackerAR, in.MonsterIMP, in.SkillIgnoreDEF, in.IgnoreDEFPer, pierce, note,
		carries)
	return carries
}
