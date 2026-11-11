// The Character Info script, run for real.
//
// WHY THIS FILE EXISTS: the numbers on the Character Info panel (ATK, DEF, A.R.,
// D.R., HR, CSPD, the five resistances) are computed by the game's OWN script -
// `ge\script\pc\get_status_pc.scp`, one entry point per panel line - and our Go
// code applies the same rules by hand (zone\status.go). Hand-porting a formula is
// how a port drifts, so this file makes the game's script RUN and lets the zone
// compare the two answers (`zone\status_shadow.go`).
//
// IT IS SHADOW: the script's answer is logged and then thrown away. The packet
// still carries the Go value, because the Go sheet is the one the owner's screen
// already shows and a changed number there is a regression. The script proves the
// Go rule; it does not replace it.
//
// THE BUILTINS ARE BOUND, NOT RECORDED. Every name the status script calls is
// installed here, so the `__on_missing` stub never answers 0 for a call whose
// answer the FORMULA depends on - a 0 there is not "missing", it is a wrong
// number in the middle of an arithmetic expression. A name this file answers
// with an empty value also says so ONCE in the log (the same contract as the
// missing recorder), so "the term is 0 because we have no data" is a line a
// reader can find rather than a silence.
package script

import (
	"context"
	"fmt"
	"sort"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// StatusTimeout bounds ONE call of one entry point. The status script is pure
// arithmetic over tables - no loop, no tick - so three seconds is generous; the
// point of a deadline is that a script under a `while(1)` of its own cannot
// freeze a zone before the shadow has even logged.
const StatusTimeout = 3 * time.Second

// StatusSelf is the `self` table the game's status script reads: ONE character,
// as plain data. The zone fills it; this package turns it into a Lua table with
// the property fallback, so a field the script reads and we did not set answers
// 0 (never nil - `nil >= 11` is a Lua ERROR that would abort the whole run).
//
// Handle is the only FIELD the builtins trust: `GetEquipItem(self, slot)` answers
// the worn item only when `self` was built for the same handle as the installed
// StatusSource. A click script that calls GetEquipItem with its own `self` gets a
// zero table of the right shape instead of another character's gear.
type StatusSelf struct {
	Handle uint32
	// Props are the property names the script reads with a MEANING: Job, Lv,
	// ClassName ('PC') and the six base stats as the rules see them. Everything
	// unspecified reads 0, which is the truth for every buff/promotion term this
	// server has no data for (ATK_BM, PromotionSTR, WeaponPF, ...).
	Props map[string]float64
	// Strings are the properties the script reads as NAMES, not numbers
	// (`self.RaceType`, `self.SizeType` in monster\get_status_monster.lua and in
	// the damage rule). A property absent from both maps reads 0, so a name that
	// matters has to be listed here or the comparison silently answers false.
	Strings map[string]string
	// ClassName is 'PC' for a player character, exactly as the live server has it
	// (`self.ClassName == 'PC' and self.Job == 272` appears eight times in the
	// corpus, once per class-specific branch).
	ClassName string
}

// StatusSource answers what the status builtins read. It is implemented by the
// ZONE (`zone\status_shadow.go`), because the data behind it - the worn items,
// the emitted item properties, the stance table - is the zone's. NIL is legal and
// means "nothing known": every item answer is a zero table of the right shape,
// every stance answer is an empty row, every data-table read answers 0 / "", and
// each of those says so once in the log.
type StatusSource interface {
	// Handle is the character this source describes.
	Handle() uint32
	// EquipItem answers one worn slot's properties by SLOT name ('RH', 'BODY',
	// 'BOOTS', ...). An EMPTY slot answers a table of the same shape with zeros -
	// never nil, because the script indexes it without a guard.
	EquipItem(slot string) map[string]any
	// Stance answers the row of the stance the character stands in, as the raw
	// attributes of datatable_stance.xml.
	Stance() map[string]any
	// NumberValue, StringValue and NumberValueByClassName are the
	// GetDataTable*Value builtins. ok=false means "this build has no such table"
	// and is logged once by name.
	NumberValue(table string, id uint32, column string) (float64, bool)
	StringValue(table string, id uint32, column string) (string, bool)
	NumberValueByClassName(table, className, column string) (float64, bool)
	// ZoneMapClassID is the map class id this zone runs - the REAL id, not a 0.
	ZoneMapClassID() uint32
	// Promotion is the character's promotion level AFTER the game's band for its
	// level - min(PromotionStep, ge_tables.promotion_band). SCR_Get_MSP adds
	// promotionLv*10 to the sheet; SCR_Get_RHP and SCR_Get_RSP read it too.
	// CashPromotion is the raw cash step (GetCashPromotionLevel), uncapped.
	Promotion() int
	CashPromotion() int
}

// StatusResult is one entry point's answer.
type StatusResult struct {
	Name  string
	Value float64
	Err   error // non-nil when the script RAISED: Value is then 0 and unused
}

// StatusEntrySpec is one panel line: the label the log uses and the script
// function that produces it.
type StatusEntrySpec struct {
	Label  string
	Script string
}

// StatusEntry is the fifteen panel lines, in the order the shadow prints them.
//
// SCR_Get_CATK is the panel's ATK and SCR_Get_CHR its HR (the packet's field is
// called HR, the script's name is CHR - see zone\status.go's offsets).
var StatusEntry = []StatusEntrySpec{
	{"ATK", "SCR_Get_CATK"},
	{"DEF", "SCR_Get_DEF"},
	{"AR", "SCR_Get_AR"},
	{"DR", "SCR_Get_DR"},
	{"CHR", "SCR_Get_CHR"},
	{"CSPD", "SCR_Get_CSPD"},
	{"ASPD", "SCR_Get_ASPD"},
	{"MSPD", "SCR_Get_MSPD"},
	{"MHP", "SCR_Get_MHP"},
	{"MSP", "SCR_Get_MSP"},
	{"RFIRE", "SCR_Get_RFIRE"},
	{"RICE", "SCR_Get_RICE"},
	{"RLGHT", "SCR_Get_RLGHT"},
	{"RSTAT", "SCR_Get_RSTAT"},
	{"RPSY", "SCR_Get_RPSY"},
}

// installStatus binds the Character Info builtins. Called once, from New(),
// beside installClick: installClick answers the DIALOG names, this answers the
// ones the stat formula needs, and the two sets do not overlap except for
// GetCommander/GetMasteryObject, which are re-bound here only to say once in the
// log that their object reads answer 0.
func (h *Host) installStatus() {
	L := h.L

	// The worn item, by slot. Answered from the installed source; with no source
	// (or a `self` that is not the source's character) it is a zero table of the
	// SAME shape, because `armor.DEF` on nil raises.
	L.SetGlobal("GetEquipItem", L.NewFunction(func(L *lua.LState) int {
		slot := argString(L, 2)
		src, mine := h.statusSourceFor(argSelf(L, 1))
		if src == nil {
			h.reportOnce("status:GetEquipItem", "script: GetEquipItem(%q) has no item "+
				"source wired - answering a zero table (every item column 0)", slot)
			L.Push(bag(L))
			return 1
		}
		if !mine {
			h.reportOnce("status:GetEquipItem:foreign", "script: GetEquipItem(%q) was "+
				"called with a `self` that is not the status character - answering a "+
				"zero table", slot)
			L.Push(bag(L))
			return 1
		}
		L.Push(statusTable(L, src.EquipItem(slot)))
		return 1
	}))

	// The stance row, raw. 32 of its attributes are read by name; StnATK and
	// StnMSPD are the two whose absence must read 1, never 0 (a 0 StnATK is a
	// character who cannot hit, a 0 StnMSPD one who cannot move) - the source
	// applies that, because it owns the row.
	L.SetGlobal("GetCurrentStance", L.NewFunction(func(L *lua.LState) int {
		src, mine := h.statusSourceFor(argSelf(L, 1))
		if src == nil || !mine {
			h.reportOnce("status:GetCurrentStance", "script: GetCurrentStance has no "+
				"stance table wired - answering an empty row (StnATK 1, StnMSPD 1)")
			t := bag(L)
			t.RawSetString("StnATK", lua.LNumber(1))
			t.RawSetString("StnMSPD", lua.LNumber(1))
			L.Push(t)
			return 1
		}
		L.Push(statusTable(L, src.Stance()))
		return 1
	}))

	// Buffs. THE TWO "NO BUFF SYSTEM" STUBS ARE DELETED (buff slice 1): IsBuffApplied
	// and GetAppliedBuffLevel are bound for real in script\buff.go, against the zone's
	// buff list, and answering 'NO' / 0 here would shadow the real binding - this
	// function runs after installBuffs in New, so a stub here would silently win.
	// GetCommander stays the identity stub installClick installed (the commander
	// bag IS the character bag, and its every field reads 0 through the property
	// fallback). It is re-bound only so the 13 Lock_* reads have one log line.
	L.SetGlobal("GetCommander", L.NewFunction(func(L *lua.LState) int {
		h.reportOnce("status:GetCommander", "script: GetCommander answered self - the "+
			"13 commander Lock_* fields and BurlonPoint read 0 (no mastery data)")
		L.Push(L.Get(1))
		return 1
	}))
	// The mastery object: 37 CM_* fields, all 0. A bag, not a number, because
	// `masteryObj.CM_AttackUp1` on a number raises.
	L.SetGlobal("GetMasteryObject", L.NewFunction(func(L *lua.LState) int {
		h.reportOnce("status:GetMasteryObject", "script: GetMasteryObject answered an "+
			"empty bag - its 37 CM_* mastery bonuses read 0 (no mastery table)")
		L.Push(bag(L))
		return 1
	}))

	// GetOwner is the MONSTER owner lookup: `SCR_Get_MON_DEF`
	// (monster\get_status_monster.lua:492) asks it and then only compares the
	// owner's ClassName, so `nil` is the reference's own "no owner" answer and the
	// adjust branch is skipped. Unbound it would RAISE, and a raise here would cost
	// every cast its target DEF.
	L.SetGlobal("GetOwner", L.NewFunction(func(L *lua.LState) int {
		h.reportOnce("status:GetOwner", "script: GetOwner answered nil - this server "+
			"builds no monster ownership, so the reference's owner-adjust branch is skipped")
		L.Push(lua.LNil)
		return 1
	}))
	// IsWorldPVPServer is asked by the same file (:502) to scale a monster's DEF by
	// 0.75 on a PVP server. This is a PVE zone, so the answer is the reference's own
	// "not a PVP server" value.
	L.SetGlobal("IsWorldPVPServer", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(0))
		return 1
	}))

	// The data tables. Stance and Job come from the tables the zone already
	// loaded; anything else is 0 / "" and one line, because a table this build
	// never loaded would otherwise silently zero a formula term.
	L.SetGlobal("GetDataTableNumberValue", L.NewFunction(func(L *lua.LState) int {
		table, id, column := argString(L, 1), argNumber(L, 2), argString(L, 3)
		if src, mine := h.statusSourceFor(nil); src != nil && mine {
			if v, ok := src.NumberValue(table, uint32(id), column); ok {
				L.Push(lua.LNumber(v))
				return 1
			}
		}
		h.reportOnce("status:GetDataTableNumberValue:"+table,
			"script: GetDataTableNumberValue(%q, ...) - this build has not loaded that "+
				"table, answering 0", table)
		L.Push(lua.LNumber(0))
		return 1
	}))
	L.SetGlobal("GetDataTableNumberValueByClassName", L.NewFunction(func(L *lua.LState) int {
		table, name, column := argString(L, 1), argString(L, 2), argString(L, 3)
		if src, mine := h.statusSourceFor(nil); src != nil && mine {
			if v, ok := src.NumberValueByClassName(table, name, column); ok {
				L.Push(lua.LNumber(v))
				return 1
			}
		}
		h.reportOnce("status:GetDataTableNumberValueByClassName:"+table,
			"script: GetDataTableNumberValueByClassName(%q, ...) - that table is not "+
				"loaded, answering 0", table)
		L.Push(lua.LNumber(0))
		return 1
	}))
	L.SetGlobal("GetDataTableStringValue", L.NewFunction(func(L *lua.LState) int {
		table, id, column := argString(L, 1), argNumber(L, 2), argString(L, 3)
		if src, mine := h.statusSourceFor(nil); src != nil && mine {
			if v, ok := src.StringValue(table, uint32(id), column); ok {
				L.Push(lua.LString(v))
				return 1
			}
		}
		h.reportOnce("status:GetDataTableStringValue:"+table,
			"script: GetDataTableStringValue(%q, ...) - that table is not loaded, "+
				"answering an empty string", table)
		L.Push(lua.LString(""))
		return 1
	}))

	// The one-field reads that have no data behind them yet. Each answers the
	// script's own "not set" value and says so once: an option roll, a set-item
	// count and a service nation are all things this server's item model does not
	// carry (see the shadow's report line).
	zeroOnce := func(luaName, why string) lua.LGFunction {
		return func(L *lua.LState) int {
			h.reportOnce("status:"+luaName, "script: %s - %s, answering 0", luaName, why)
			L.Push(lua.LNumber(0))
			return 1
		}
	}
	L.SetGlobal("GetItemOptionSocketATK", L.NewFunction(zeroOnce("GetItemOptionSocketATK",
		"no per-instance socket/option roll is stored")))
	L.SetGlobal("GetCurEquipSetItemCount", L.NewFunction(zeroOnce("GetCurEquipSetItemCount",
		"no set-item membership is stored")))
	L.SetGlobal("GetServiceNation", L.NewFunction(zeroOnce("GetServiceNation",
		"no service nation is configured")))

	// PROMOTION IS REAL DATA, NOT A STUB. Both of these used to answer 0 with
	// "no promotion level is stored" - true of the zone's LOADER, false of the
	// database: `character_property` carries PromotionStep / CashPromotionStep
	// (character 304 "Amy" has 4), and PGStore.loadPromotion puts them on the
	// character, banded by barrack.PromotionBand exactly as the barrack packet is.
	//
	// WHY IT MATTERS: SCR_Get_MSP is `195 + Lv*5 + promotionLv*10` before gear, so
	// a 0 here gave every promoted character the unpromoted max SP (695 at level
	// 100) while the live server's sheet reads 995 at level 150 - which is
	// promotion 5, and 10 points per level of it. SCR_Get_RHP and SCR_Get_RSP read
	// the same value.
	L.SetGlobal("GetPromotionLevel", L.NewFunction(func(L *lua.LState) int {
		src, mine := h.statusSourceFor(argSelf(L, 1))
		if src == nil || !mine {
			h.reportOnce("status:GetPromotionLevel", "script: GetPromotionLevel - no "+
				"status source is wired for this `self`, answering 0")
			L.Push(lua.LNumber(0))
			return 1
		}
		L.Push(lua.LNumber(float64(src.Promotion())))
		return 1
	}))
	L.SetGlobal("GetCashPromotionLevel", L.NewFunction(func(L *lua.LState) int {
		src, mine := h.statusSourceFor(argSelf(L, 1))
		if src == nil || !mine {
			h.reportOnce("status:GetCashPromotionLevel", "script: GetCashPromotionLevel - "+
				"no status source is wired for this `self`, answering 0")
			L.Push(lua.LNumber(0))
			return 1
		}
		L.Push(lua.LNumber(float64(src.CashPromotion())))
		return 1
	}))

	// The zone INSTANCE id is 0 (one instance per process), but the map CLASS id
	// is real: GetZoneMapClassID is the only rule in the file that needs it, and
	// answering 0 there would make every map read as "map 0".
	L.SetGlobal("GetCurrentZoneInstID", L.NewFunction(func(L *lua.LState) int {
		h.reportOnce("status:GetCurrentZoneInstID", "script: GetCurrentZoneInstID - this "+
			"zone runs ONE instance, answering 0 (the map class id is answered by "+
			"GetZoneMapClassID)")
		L.Push(lua.LNumber(0))
		return 1
	}))
	L.SetGlobal("GetZoneMapClassID", L.NewFunction(func(L *lua.LState) int {
		if src, mine := h.statusSourceFor(nil); src != nil && mine {
			L.Push(lua.LNumber(float64(src.ZoneMapClassID())))
			return 1
		}
		h.reportOnce("status:GetZoneMapClassID", "script: GetZoneMapClassID has no "+
			"source wired - answering 0")
		L.Push(lua.LNumber(0))
		return 1
	}))

	// SCR_Get_HoldTime is ABSENT from the whole corpus: there is no file that
	// defines it, so a script that calls it would hit the missing recorder, get a
	// NUMBER back and raise "attempt to call a number". It is stubbed to 0 here
	// and every call is logged, because a hold time of 0 is a decision this build
	// makes, not a number the game gave us. Named in the report.
	L.SetGlobal("SCR_Get_HoldTime", L.NewFunction(func(L *lua.LState) int {
		h.logf("script: SCR_Get_HoldTime called - the corpus does not define it, " +
			"answering 0 (no hold time), see the status shadow report")
		L.Push(lua.LNumber(0))
		return 1
	}))
	// NewPVPRule is absent for the same reason (SCR_Get_MHP calls it). 0 is the
	// "not a PvP rule" arm, which is the only rule this server applies.
	L.SetGlobal("NewPVPRule", L.NewFunction(func(L *lua.LState) int {
		h.logf("script: NewPVPRule called - the corpus does not define it, answering " +
			"0 (no PvP rule), see the status shadow report")
		L.Push(lua.LNumber(0))
		return 1
	}))
}

// statusSourceFor answers the installed source and whether `self` belongs to it.
// A nil self (the GetDataTable* builtins take no self) is accepted as "the same
// character", because those reads name a TABLE, not a character.
func (h *Host) statusSourceFor(self *lua.LTable) (StatusSource, bool) {
	if self == nil {
		return h.status, h.status != nil
	}
	n, ok := self.RawGetString(statusHandleKey).(lua.LNumber)
	if !ok {
		return h.status, false
	}
	id := uint32(n)
	if h.status != nil && h.status.Handle() == id {
		return h.status, true
	}
	// THE OTHER ACTOR of a two-character run: a script message is `from` and `self`,
	// and the rule inside it asks for the SENDER's stance (`GetCurrentStance(from)`,
	// pc\common_pc.scp:6744). Without this the sender's table is "foreign" and every
	// stance read answers the empty row.
	if h.statusFrom != nil && h.statusFrom.Handle() == id {
		return h.statusFrom, true
	}
	return h.status, false
}

// statusHandleKey is the hidden field that ties a `self` table to the character
// it was built for. It is deliberately NOT a property name the corpus reads.
const statusHandleKey = "__status_id"

// statusTable turns a plain map into a Lua table whose UNKNOWN fields read 0, so
// the script's unguarded `armor.DEF` can never meet a nil.
func statusTable(L *lua.LState, m map[string]any) *lua.LTable {
	t := bag(L)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic; the table is read by name, so only tidiness
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			t.RawSetString(k, lua.LNumber(v))
		case int:
			t.RawSetString(k, lua.LNumber(float64(v)))
		case string:
			t.RawSetString(k, lua.LString(v))
		case bool:
			if v {
				t.RawSetString(k, lua.LNumber(1))
			} else {
				t.RawSetString(k, lua.LNumber(0))
			}
		default:
			t.RawSetString(k, lua.LNumber(0))
		}
	}
	return t
}

// statusSelfTable builds the `self` table: the named fields plus the property
// fallback that answers 0 for everything else.
func (h *Host) statusSelfTable(self StatusSelf) *lua.LTable {
	m := make(map[string]any, len(self.Props)+len(self.Strings)+3)
	for k, v := range self.Props {
		m[k] = v
	}
	for k, v := range self.Strings {
		m[k] = v
	}
	if self.ClassName != "" {
		m["ClassName"] = self.ClassName
	}
	t := statusTable(h.L, m)
	t.RawSetString(statusHandleKey, lua.LNumber(self.Handle))
	return t
}

// RunStatus calls every entry point ONCE for one character and answers what each
// returned. It is the only way the zone runs the status script, so the source and
// the `self` table are installed for exactly the length of the run and taken back
// out afterwards - a click script that runs in between cannot see them.
//
// A script that RAISES is reported per entry and the run continues: one broken
// entry point must not cost the other fourteen, and the caller keeps the Go value
// either way.
func (h *Host) RunStatus(src StatusSource, self StatusSelf, entries []StatusEntrySpec) []StatusResult {
	defer h.withStatus(src, self)()

	// scr_skill_buff: the corpus defines it in skill\buff.lua, which is NOT one of
	// the eagerly loaded libraries. `SCR_SKILL_BUFF_ORNO` in get_status_pc.lua
	// calls it, so it is loaded up front - by NAME, through the index, exactly
	// like every other script file (never by a hardcoded corpus path).
	if _, err := h.EnsureScript("SCR_SKILL_BUFF"); err != nil {
		h.reportOnce("status:SCR_SKILL_BUFF", "script: SCR_SKILL_BUFF is not loadable "+
			"(%v) - a buff call will record as missing", err)
	}

	selfTable := h.statusSelfTable(self)
	out := make([]StatusResult, 0, len(entries))
	for _, e := range entries {
		res := StatusResult{Name: e.Label}
		if _, err := h.EnsureScript(e.Script); err != nil {
			res.Err = err
			out = append(out, res)
			continue
		}
		h.ticks = 0
		res.Value, res.Err = h.callStatus(selfTable, e.Script)
		out = append(out, res)
	}
	return out
}

// withStatus installs the character ONE script run is about and returns the
// function that takes it back out. It is the shared half of RunStatus and
// RunSkillAttack (script\skill.go): both install a source and a `self` for
// exactly the length of their run, and both must clear them afterwards so a
// click that happens next cannot see another character's gear.
//
// IT SAVES AND RESTORES THE PREVIOUS PAIR, not just nil: a buff's ENTER script may
// apply another buff (`AddBuff` inside an ENTER), and that inner run installs its own
// character and source. Clearing on exit would leave the OUTER script running with no
// source for the rest of its lines, which reads as a table this build has not loaded.
// For every existing caller the previous pair is the zero value, so the clear is
// exactly what it was.
func (h *Host) withStatus(src StatusSource, self StatusSelf) func() {
	prevSrc, prevSelf := h.status, h.statusSelf
	h.status = src
	h.statusSelf = self
	return func() {
		h.status = prevSrc
		h.statusSelf = prevSelf
	}
}

// callStatus calls one entry point with the `self` table under a deadline and
// answers its number.
func (h *Host) callStatus(self *lua.LTable, name string) (float64, error) {
	fn, ok := h.L.GetGlobal(name).(*lua.LFunction)
	if !ok {
		return 0, fmt.Errorf("%s is not a function (was its file loaded?)", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), StatusTimeout)
	defer cancel()
	// THE RAW PAIR, NOT THE GUARD. This call is the OUTERMOST run of a status entry: it
	// starts from Go, never from inside another script, and Set/Remove is what also
	// CLEARS a context a previous run may have left behind. The guard
	// (`withContext`) belongs to a call that runs INSIDE one - measured here: guarding
	// this one let a leftover context survive and every later run inherited it.
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	// Protect: an error is RETURNED, not raised through the zone's stack.
	if err := h.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, self); err != nil {
		return 0, err
	}
	v := h.L.Get(-1)
	h.L.Pop(1)
	n, ok := v.(lua.LNumber)
	if !ok {
		return 0, fmt.Errorf("%s answered %s, not a number", name, v.Type())
	}
	return float64(n), nil
}

// argNumber reads one argument as a number without raising on a string.
func argNumber(L *lua.LState, i int) float64 {
	switch v := L.Get(i).(type) {
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		var f float64
		if _, err := fmt.Sscanf(string(v), "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

// argSelf reads the `self` argument as a table, or nil when it is not one.
func argSelf(L *lua.LState, i int) *lua.LTable {
	t, _ := L.Get(i).(*lua.LTable)
	return t
}
