// The COMBAT skill script: `ge\script\skill\attack.scp`, run for real.
//
// WHY THIS FILE EXISTS. R84 is the damage number a cast puts on the wire, and it
// is OURS today - `zone\skill_cast.go`'s `castDamage` is one line of arithmetic
// (ATK x SklATK / 100 - DEF). The game's own formula is not Go at all: 5,621 of
// the 5,626 rows in `datatable_skill.xml` name `SCR_SKILL_ATTACK`, which builds an
// argument bag (`GetPhysicalSkillArg`) out of the caster, the skill row, the
// stance and the worn gear and hands it to the engine with
// `SendMsg(self, target, 'OM_SKILL_PHYSICAL', arg, ret)`
// (out\lua_corpus\skill\attack.lua:141-145).
//
// WHAT ONE CALL ANSWERS, and why it is the ratio and not a damage number: the
// FINAL hit is computed in C++, not in Lua - `OM_SKILL_PHYSICAL` lands in
// `CFieldChar::OnMessage` (source2\geServer\ZServerLib\FieldChar_Message.cpp:46)
// -> `OnSkillPhysical`, which is where the ratio meets the attack and the
// defence. The last number that exists on the SCRIPT side is the one the script
// writes in its own "Skill Attack damage calculate" block:
//
//	physicalskillarg.ATKRatio = skill.SklATK * ((skill.SklLv + ADDSklLv +
//	                            skill.SklLv_BM) * 10 + 100) / 100 [* skill.SplDam]
//
// (attack.lua:359-365). That is what RunSkillAttack returns: the script's own
// damage factor, comparable against the row's SklATK that the placeholder uses.
//
// ON THE PACKET'S PATH AS OF R84 SLICE 2. The zone multiplies this factor by the
// caster's own ATK (`castDamage`) and that product is what the packet carries; a
// zone with no corpus, or a script that raises, still carries the placeholder.
// Nothing in THIS package decides a damage - it answers the factor and nothing more.
//
// A BROKEN SCRIPT MUST NEVER BREAK A CAST. The one entry point is called under
// `Protect` (gopher-lua's pcall) with a deadline, and every failure - a missing
// function, a raise, a bag the script never asked for - is RETURNED as an error
// for the caller to log. Nothing here panics into the zone's reply.
package script

import (
	"context"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// SkillAttackScript is the entry point a skill row names in its own `Script`
// column, and the name this package calls.
const SkillAttackScript = "SCR_SKILL_ATTACK"

// SkillTimeout bounds ONE call. The script is a straight walk over arithmetic
// with a handful of table reads (no loop, no tick), so three seconds is generous;
// the deadline exists so a `while(1)` a future skill branch reaches cannot freeze
// a zone inside a reply.
const SkillTimeout = 3 * time.Second

// skillRatioField is the field of the argument bag the answer is read from. It is
// spelled exactly as the corpus spells it - Lua is case sensitive, and
// `physicalskillarg.ATKRatio` is the write (attack.lua:360).
const skillRatioField = "ATKRatio"

// installSkill binds the combat-script builtins. Called once, from New().
//
// THE BAGS ARE BUILTINS, NOT STUBS. `GetPhysicalSkillArg()` is the table the whole
// damage formula is written into, so a missing recorder answering 0 would make its
// very first field assignment (`physicalskillarg.ClassType = skill.ClassType`,
// attack.lua:188) an "attempt to index a number" - the script would raise before it
// computed anything, on every skill. `GetHateArg()` is the same container for the
// hate message (`hatearg.TargetLv = self.Lv`, pc\pc_lib.lua:6, reached from
// attack.lua:145 on EVERY cast), and it is the same failure.
//
// `GetMagicalSkillArg()` is the third bag, and the one the buff lane needs:
// `SCR_SKILL_BUFF` writes `Buff` / `BuffType` / `BuffFreq` / `BuffID` and eleven more
// fields into it (ge\script\skill\buff.scp:21-45) and sends it to the target. It was
// the ONE name of the three the reference registers that this host answered with the
// missing stub's 0, so the skill's buff died on its first assignment.
//
// The original hands out a fresh argument bag per call (SystemFunc.cpp:130
// BEGIN_FUNCTION(GetPhysicalSkillArg, 1); :6712 for the magical one); so does this.
// The PHYSICAL and the MAGICAL bag are both kept, because both are read back after
// the run - the hate bag is not, because it goes straight out with `BroadMsg`, which
// this host does not serve yet, so there is nothing on this side to read it.
func (h *Host) installSkill() {
	h.L.SetGlobal("GetPhysicalSkillArg", h.L.NewFunction(func(L *lua.LState) int {
		t := bag(L)
		h.physArg = t
		L.Push(t)
		return 1
	}))
	h.L.SetGlobal("GetMagicalSkillArg", h.L.NewFunction(func(L *lua.LState) int {
		t := bag(L)
		h.magArg = t
		L.Push(t)
		return 1
	}))
	h.L.SetGlobal("GetHateArg", h.L.NewFunction(func(L *lua.LState) int {
		L.Push(bag(L))
		return 1
	}))
}

// ResetMagicalSkillArg forgets the bag the last call handed out, for the same reason
// ResetPhysicalSkillArg exists: a stale bag from a previous cast would otherwise
// answer for a cast whose script never asked for one.
func (h *Host) ResetMagicalSkillArg() { h.magArg = nil }

// MagicalSkillArg answers the fields of the bag the LAST GetMagicalSkillArg handed
// out, as plain Go values: numbers as float64, strings as string, everything else
// skipped. nil means the script never asked for a bag.
//
// STRINGS MATTER HERE, unlike the physical bag: the four fields this lane reads back
// are `Buff` ("Coruscation"), `BuffType` ("FORCE"), `BuffFreq` (100) and `BuffID`
// (an id) - two names and two numbers - so a numbers-only reader would lose half of
// what the script decided. The map is a COPY; the Lua table stays the script's own.
func (h *Host) MagicalSkillArg() map[string]any {
	if h == nil {
		return nil
	}
	return tableFields(h.magArg)
}

// tableFields copies a Lua table into plain Go values, or nil for a nil table.
func tableFields(t *lua.LTable) map[string]any {
	if t == nil {
		return nil
	}
	out := make(map[string]any, 24)
	t.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		switch val := v.(type) {
		case lua.LNumber:
			out[string(name)] = float64(val)
		case lua.LString:
			out[string(name)] = string(val)
		}
	})
	return out
}

// SkillBuffCall is ONE cast of a skill whose own `Script` column is a BUFF script:
// `SCR_SKILL_BUFF`, run for real.
//
// ITS SHAPE IS THE REFERENCE'S `CFieldChar::ProcessSkill` (FieldChar_Skill.cpp:49-75):
// push the caster, the target, the skill, `splash`, the result table, run the row's
// `Script`. Everything the script needs to KNOW is data the zone owns; nothing here
// decides a buff, a rank or a resist.
type SkillBuffCall struct {
	// Entry is the skill row's own `Script` column - "SCR_SKILL_BUFF" for the 714
	// rows that dispatch there. It is a field, not a constant, because the column is
	// the dispatch: a row that names a different script must run that one.
	Entry string
	// Source and Self are the CASTER: the builtins it reads and its `self` table.
	Source StatusSource
	Self   StatusSelf
	// Skill is the skill's own row by name (`Buff`, `BuffType`, `BuffFreq`,
	// `BuffID`, `ClassID`, `SklLv`, ...) - the `skill` argument. Every value comes
	// from datatable_skill.xml; a field the row does not state answers 0 through the
	// table's property fallback, which is what an unset IES property reads.
	Skill map[string]any
	// Target is the body the cast was aimed at (`Handle` is what SendMsg needs).
	Target map[string]any
	// Splash and TargetCount are the `splash` and `targetcount` arguments.
	Splash      bool
	TargetCount int
	// World is the buff list the message script's own `AddBuff` / `IsBuffApplied`
	// calls reach. NIL is legal: the builtins then do nothing and say so once.
	World BuffWorld
	// Assist resolves the message target to a live actor. NIL refuses the message.
	Assist AssistWorld
}

// SkillBuffResult is what ONE buff script left behind.
type SkillBuffResult struct {
	// Ran is false when the entry point never ran (an empty Script column, a name
	// the corpus does not define).
	Ran bool
	// Arg is the magical-argument bag AFTER the script ran: `Buff`, `BuffType`,
	// `BuffFreq`, `BuffID`, `SklLv`, `ClassID`, ... nil when the script never asked
	// for one.
	Arg map[string]any
	// Ret is the `ret` table after the message script wrote into it (`BuffSucc`,
	// `Value`, ...) - the reference's CSkillResult, as plain data.
	Ret map[string]any
}

// RunSkillBuff runs ONE skill row's own buff script with a live target behind it.
//
// THE WORLD IS INSTALLED FOR EXACTLY THE LENGTH OF THE RUN: the buff list
// (`World`) and the message world (`Assist`) are the zone's, handed in per call so a
// script that runs outside a cast cannot reach them. Both are save-and-restore, the
// same discipline withBuffWorld already uses for a buff ENTER.
//
// A RAISE IS AN ERROR, not a zero: the caller says so and no buff lands. The
// `GetMagicalSkillArg` bag is recorded on the host as `magArg`, so the caller can
// read what the script decided even when the message script refused afterwards.
func (h *Host) RunSkillBuff(c SkillBuffCall) (SkillBuffResult, error) {
	var res SkillBuffResult
	if h == nil || h.L == nil {
		return res, fmt.Errorf("a skill buff script needs a script host and this zone has none")
	}
	if c.Entry == "" {
		return res, fmt.Errorf("the skill row names no Script column, so no buff script can run")
	}
	if !h.HasScript(c.Entry) {
		return res, fmt.Errorf("%s is not defined by any file under %s", c.Entry, h.root)
	}
	if _, err := h.EnsureScript(c.Entry); err != nil {
		return res, fmt.Errorf("%s: %w", c.Entry, err)
	}

	defer h.withStatus(c.Source, c.Self)()
	if c.World != nil {
		defer h.withBuffWorld(c.World)()
	}
	prevAssist := h.assist
	h.assist = c.Assist
	defer func() { h.assist = prevAssist }()

	L := h.L
	self := h.statusSelfTable(c.Self)
	target := statusTable(L, c.Target)
	skill := statusTable(L, c.Skill)
	// `ret` is the result table the reference passes in (ProcessSkill pushes
	// `ret->GetIESObject()`); it must be a table because SendMsg hands it on and the
	// message script writes `ret.BuffSucc` into it.
	ret := bag(L)
	splash := lua.LNumber(0)
	if c.Splash {
		splash = lua.LNumber(1)
	}

	h.ResetMagicalSkillArg()
	h.ticks = 0
	if err := h.callSkillScript(c.Entry, self, target, skill, ret, splash, float64(c.TargetCount)); err != nil {
		return res, err
	}
	res.Ran = true
	res.Arg = h.MagicalSkillArg()
	res.Ret = tableFields(ret)
	return res, nil
}

// callSkillScript calls one skill entry point once, with a deadline and under
// Protect. The arguments are ProcessSkill's own order (FieldChar_Skill.cpp:54-58):
// self, target, skill, splash, ret, targetcount.
func (h *Host) callSkillScript(entry string, self, target, skill, ret *lua.LTable,
	splash lua.LValue, targetCount float64) error {

	fn, ok := h.L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		return fmt.Errorf("%s is not a function (was its file loaded?)", entry)
	}
	ctx, cancel := context.WithTimeout(context.Background(), SkillTimeout)
	defer cancel()
	defer h.withContext(ctx)()
	return h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		self, target, skill, splash, ret, lua.LNumber(targetCount))
}

// ResetPhysicalSkillArg forgets the bag the last call handed out, so a caller can
// tell "the script filled a bag" from "the script never asked for one" - a stale
// bag from a previous cast would otherwise answer for a cast that computed
// nothing.
func (h *Host) ResetPhysicalSkillArg() { h.physArg = nil }

// PhysicalSkillArg answers the numeric fields of the bag the LAST
// GetPhysicalSkillArg handed out, or nil when the script never asked for one.
//
// Numbers only: the bag carries names too (`Attacker = 'player'`,
// `RaceType = 'Human'`), and none of them is a number a caller can compare. The
// map is a COPY - the Lua table is the script's own working memory and must not
// be handed out live.
func (h *Host) PhysicalSkillArg() map[string]float64 {
	if h == nil || h.physArg == nil {
		return nil
	}
	out := make(map[string]float64, 8)
	h.physArg.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		if n, ok := v.(lua.LNumber); ok {
			out[string(name)] = float64(n)
		}
	})
	return out
}

// SkillAttackCall is ONE cast, as plain data: what the script needs to see.
//
// The zone fills it (zone\skill_script.go), because the data behind it - the
// character's worn classes, its stance row, the skill's own row, the body that was
// hit - is the zone's. This package turns it into Lua tables with the property
// fallback, so a field the script reads and nobody set answers 0 instead of nil
// (`nil >= 11` is a Lua ERROR, attack.lua is full of unguarded reads).
type SkillAttackCall struct {
	// Source answers the status builtins the formula calls - GetEquipItem,
	// GetCurrentStance, GetCommander. NIL is legal: every item then answers a zero
	// table of the right shape and says so once, which is honest but wrong - the
	// formula then reads a naked character.
	Source StatusSource
	// Self is the caster, exactly as the Character Info script takes it.
	Self StatusSelf

	// Skill is the skill's own ROW by name (`ClassID`, `SklATK`, `SklLv`,
	// `SklLv_BM`, `SplDam`, `UseType`, ...). The reference passes its skill object;
	// this is the same data as a plain map.
	Skill map[string]any
	// Target is the body that was hit - `Handle` is what `SendMsg` needs and
	// `ClassName` is read by the branches (`target.ClassName ~= 'PC'`). A ground
	// cast that hit nobody passes an EMPTY table, never nil: the script indexes
	// `target` before it tests `targetCount` (attack.lua:43 against :1067), so nil
	// raises instead of answering "no victims".
	Target map[string]any

	// Splash says whether the cast was aimed at a POINT (`splash == 1` in the
	// script picks the `* SplDam` arm) and TargetCount is how many bodies it
	// recorded - the value the script calls `targetcount`.
	Splash      bool
	TargetCount int
}

// RunSkillAttack runs the game's own SCR_SKILL_ATTACK once and answers the damage
// factor it computed.
//
// The status source and the `self` table are installed for exactly the length of
// the run and taken back out afterwards - the same scratch discipline RunStatus
// uses, and for the same reason: one runtime, one call at a time, and a click
// script that runs in between must not be able to see them.
func (h *Host) RunSkillAttack(c SkillAttackCall) (float64, error) {
	if h == nil || h.L == nil {
		return 0, fmt.Errorf("%s: no script host", SkillAttackScript)
	}
	if _, err := h.EnsureScript(SkillAttackScript); err != nil {
		return 0, err
	}
	defer h.withStatus(c.Source, c.Self)()

	L := h.L
	self := h.statusSelfTable(c.Self)
	skill := statusTable(L, c.Skill)
	target := statusTable(L, c.Target)
	splash := lua.LNumber(0)
	if c.Splash {
		splash = lua.LNumber(1)
	}
	// `ret` is the out-parameter the script passes through to SendMsg
	// (attack.lua:144). The engine fills it in C++; on this side it only has to
	// exist, because `SendMsg(..., ret)` hands it to a builtin that would otherwise
	// index a nil.
	ret := bag(L)

	h.ResetPhysicalSkillArg()
	h.ticks = 0
	if err := h.callSkillAttack(self, target, skill, ret, splash, float64(c.TargetCount)); err != nil {
		return 0, err
	}

	arg := h.PhysicalSkillArg()
	if arg == nil {
		return 0, fmt.Errorf("%s ran but never asked for an argument bag "+
			"(GetPhysicalSkillArg), so it computed no damage factor", SkillAttackScript)
	}
	ratio, ok := arg[skillRatioField]
	if !ok {
		return 0, fmt.Errorf("%s ran but wrote no %s into the argument bag, so it "+
			"computed no damage factor", SkillAttackScript, skillRatioField)
	}
	return ratio, nil
}

// callSkillAttack calls the entry point once, with a deadline and under Protect:
// an error is RETURNED, not raised through the zone's stack.
func (h *Host) callSkillAttack(self, target, skill, ret *lua.LTable, splash lua.LValue, targetCount float64) error {
	fn, ok := h.L.GetGlobal(SkillAttackScript).(*lua.LFunction)
	if !ok {
		return fmt.Errorf("%s is not a function (was its file loaded?)", SkillAttackScript)
	}
	ctx, cancel := context.WithTimeout(context.Background(), SkillTimeout)
	defer cancel()
	// The RAW pair for the same reason callStatus uses it: this is the OUTERMOST run of
	// one buff script, started from Go, and the pair also clears anything a previous run
	// left behind. The guard is for a call that runs INSIDE another (skillMagicalAssist).
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	return h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		self, target, skill, splash, ret, lua.LNumber(targetCount))
}
