// THE BATTLE SCRIPT'S ENGINE CALLS: the hate list, the tactics target, the attack
// state - bound here, decided NOWHERE here.
//
// WHY THIS FILE EXISTS. `ge\script\monster\mon_ts_battle.scp` is the game's whole
// monster battle AI in ONE function (`SCR_MON_AI_LIB_TS_BATTLE`, 277 lines): who a
// body hates, when it switches target, when it skills, when it runs, when it calls
// friends. The owner's ruling (2026-11-07) is that this script RUNS - "whatever the
// scp is doing it should do by itself, nothing gets coded in native Go" - so the Go
// side is the ENGINE: it stores what the script tells it and answers what the script
// asks. Every name below is a question, not a decision.
//
// WHAT IS BOUND HERE, with the arity the script itself calls it with (grep of
// `ge\script\monster\mon_ts_battle.scp`; the translated corpus calls the same way):
//
//	GetTopHatePointChar(self)             :5, :100, :150, :240
//	GetTacticsTarget(self)                :101, :241
//	SetTacticsTarget(self [, target])     :164, :173 (2 args) / :157, :189, :202 (1 = clear)
//	IsSameTarget(target, lastTarget)      :167
//	InsertHate(self, target, 20)          :177
//	RemoveHate(self, lastTarget)          :149, :188, :201, :208
//	IsAttacking(self)                     :67, :162
//
// The reference's own bindings, each read before this file was written:
// `MonsterNPCFunc.cpp:41` (GetTopHatePointChar), `:81` (InsertHate), `:100`
// (RemoveHate), `:249` (SetTacticsTarget), `:294` (GetTacticsTarget), `:321`
// (IsSameTarget), and `FieldCharFunc.cpp:310` (IsAttacking).
//
// TWO THINGS THE CENSUS GOT WRONG, and they change the signatures:
//
//   - `InsertHate` has NO `force` argument. It is `(to, obj, point)` and the point is
//     ADDED to the standing (`info->m_point += point`, HateList.cpp:42). The `force`
//     word in the script belongs to a DIFFERENT call: `SCR_MON_AI_LIB_HATE(self,
//     range, force, hateamnt, target)` hands it to `BroadMsg(self, range, force,
//     'OM_HATE', hatearg, target)` (mon_ai_lib.scp:58) - it is the BROADCAST SCOPE
//     ('Friend'), not a hate priority, and a hate table cannot be "forced".
//   - `GetTopHatePointChar` answers the OBJECT (the reference `PushObject`s it,
//     MonsterNPCFunc.cpp:45-49) and nothing else. The point TOTAL is a DIFFERENT
//     builtin, `GetHate(mon, char)` (MonsterNPCFunc.cpp:1295). A caller that wants
//     both asks twice, so the object table here carries no point field - and reading
//     `target.Points` off it answers 0 through the property fallback, exactly as the
//     reference's object would.
//
// THE WORLD IS AN INTERFACE, NOT A MAP. The table itself belongs to the zone
// (`zone\monster_hate.go`), the same one-way split NearWorld keeps: this package
// knows Lua, the zone knows the field. NIL means "no world wired", and every name
// then answers its EMPTY value (nil / nothing / 'NO') and says so ONCE - never a
// made-up score, because a wrong hate total moves a monster.
package script

import (
	lua "github.com/yuin/gopher-lua"
)

// HateTarget is one character the hate table names, as the script sees it.
//
// The original pushes the character's IES object (`PushObject(target->GetIESObject())`,
// MonsterNPCFunc.cpp:45-49), so every declared property answers. The two files this
// card binds read exactly two of them: `target.ClassName` (mon_ts_battle.scp:148,
// compared against 'Structure_PartyColony') and `target.Lv` (mon_ai_lib.scp:55, the
// hate formula's level term). Handle is this side's identity for the object, and
// X/Y/Cell are carried so a later `GetDistance(self, target)` has the body's point
// without a second lookup.
//
// Points is the standing this object was chosen with. It is NOT pushed into the Lua
// table (see the file header: the reference's object has no such property); the zone
// uses it for its own `zone: HATE` line.
type HateTarget struct {
	Handle    uint32
	ClassID   uint32
	ClassName string
	Lv        int
	X, Y      int32
	Cell      int32
	Points    int
}

// HateWorld is the live world behind the seven builtins: ONE body's hate list, its
// tactics target and its attack state, all keyed by the MONSTER's handle.
//
// It is implemented by the zone, because the data behind it - which bodies are on
// the field, who is alive, what a character's level is - is the zone's. Nothing here
// returns an error: a name that cannot answer answers its empty value and the ZONE
// logs why, which is the contract `SendMsg` keeps (a Lua error inside a script run
// aborts the whole run; a missing answer does not).
type HateWorld interface {
	// InsertHate ADDS `points` to `target`'s standing with the body `mon`. A
	// negative number subtracts, because the reference's Update adds whatever it is
	// given (`info->m_point += point`, HateList.cpp:42) - clamping at zero here would
	// be inventing a rule the game does not have.
	InsertHate(mon, target uint32, points int)
	// RemoveHate drops `target` from `mon`'s list entirely - it is a delete, not a
	// subtraction (`CHateList::Remove`, HateList.cpp:129-146).
	RemoveHate(mon, target uint32)
	// TopHate answers the highest-standing LIVE target, with the points it was
	// chosen by. false means "nobody" - the reference answers NULL.
	TopHate(mon uint32) (HateTarget, bool)
	// SetTacticsTarget stores `target` as the body's tactics target; 0 clears it -
	// the script calls it with one argument to clear (mon_ts_battle.scp:157).
	SetTacticsTarget(mon, target uint32)
	// TacticsTarget answers the stored tactics target, or false when there is none
	// (or when the actor behind it has left).
	TacticsTarget(mon uint32) (HateTarget, bool)
	// IsAttacking is `GetActionState()` in the reference (FieldCharFunc.cpp:310):
	// true while the body is in one of the attack/skill states.
	IsAttacking(mon uint32) bool
}

// installHate registers the seven names. Called once, from New().
//
// They are SET, not left to the `__on_missing` stub, for the reason this package
// exists: the stub answers 0, and here 0 is a MEANINGFUL answer - `nil` means "no
// target" and `'NO'` means "not attacking", so a stub would make the battle script
// believe a monster has nobody to hate and is already swinging, forever, with the
// missing name counted in a log line nobody is reading during a fight.
func (h *Host) installHate() {
	h.L.SetGlobal("InsertHate", h.L.NewFunction(h.insertHate))
	h.L.SetGlobal("RemoveHate", h.L.NewFunction(h.removeHate))
	h.L.SetGlobal("GetTopHatePointChar", h.L.NewFunction(h.getTopHatePointChar))
	h.L.SetGlobal("SetTacticsTarget", h.L.NewFunction(h.setTacticsTarget))
	h.L.SetGlobal("GetTacticsTarget", h.L.NewFunction(h.getTacticsTarget))
	h.L.SetGlobal("IsSameTarget", h.L.NewFunction(h.isSameTarget))
	h.L.SetGlobal("IsAttacking", h.L.NewFunction(h.isAttacking))
}

// reportNoHateWorld is the ONCE line every name in this file writes when the host
// has no zone behind it. One key for all seven: the reader needs to know that the
// battle AI's answers are empty, not which name asked first.
func (h *Host) reportNoHateWorld(name string) {
	h.reportOnce("hate:no-world",
		"script: %s answered its EMPTY value - this host has no monster world wired, "+
			"so every hate answer is nil/0/'NO' (further calls are not logged)", name)
}

// hateHandle reads argument `i` as one ACTOR and answers its handle.
//
// Every argument in this group is an object table the zone built (the script's own
// `self`, or a target it got back from GetTopHatePointChar), and the only field
// trusted is `Handle` - the same contract StatusSelf.Handle keeps, and the reason a
// table built for another character cannot answer for this one.
func (h *Host) hateHandle(L *lua.LState, i int, call, what string) (uint32, bool) {
	t, ok := L.Get(i).(*lua.LTable)
	if !ok {
		h.logf("script: %s needs %s as argument %d, got %s - nothing was answered",
			call, what, i, luaTypeName(L.Get(i)))
		return 0, false
	}
	handle, ok := tableUint32(t, "Handle")
	if !ok || handle == 0 {
		h.logf("script: %s needs a Handle on %s (argument %d) - nothing was answered",
			call, what, i)
		return 0, false
	}
	return handle, true
}

// hateObject is the Lua table the script sees for one target: the IES object's four
// identity fields plus the property fallback, so a name the script reads and we do
// not set answers 0 instead of nil (`nil ~= 'Structure_PartyColony'` is true, but
// `nil >= 11` raises and would abort the battle run).
func hateObject(L *lua.LState, t HateTarget) *lua.LTable {
	obj := L.NewTable()
	obj.RawSetString("Handle", lua.LNumber(t.Handle))
	obj.RawSetString("ClassID", lua.LNumber(t.ClassID))
	obj.RawSetString("ClassName", lua.LString(t.ClassName))
	obj.RawSetString("Lv", lua.LNumber(t.Lv))
	obj.RawSetString("X", lua.LNumber(t.X))
	obj.RawSetString("Y", lua.LNumber(t.Y))
	propertyFallback(L, obj)
	return obj
}

// insertHate is `InsertHate(mon, target, point)` - mon_ts_battle.scp:177.
func (h *Host) insertHate(L *lua.LState) int {
	mon, mok := h.hateHandle(L, 1, "InsertHate", "the monster (self)")
	target, tok := h.hateHandle(L, 2, "InsertHate", "the hated target")
	points, perr := numArg(L, 3, "point")
	if !mok || !tok || perr != nil {
		return 0 // already said why
	}
	if h.Hate == nil {
		h.reportNoHateWorld("InsertHate")
		return 0
	}
	// The reference's FIRST line in Update is the self-check: a body that hates
	// itself is a script bug, and it is dropped rather than stored
	// (`if (obj == (CFieldChar*)m_owner || NULL == obj) return;`, HateList.cpp:29-30).
	if target == mon {
		return 0
	}
	h.Hate.InsertHate(mon, target, int(points))
	return 0
}

// removeHate is `RemoveHate(mon, target)` - mon_ts_battle.scp:149, :188, :201.
func (h *Host) removeHate(L *lua.LState) int {
	mon, mok := h.hateHandle(L, 1, "RemoveHate", "the monster (self)")
	target, tok := h.hateHandle(L, 2, "RemoveHate", "the target to drop")
	if !mok || !tok {
		return 0
	}
	if h.Hate == nil {
		h.reportNoHateWorld("RemoveHate")
		return 0
	}
	h.Hate.RemoveHate(mon, target)
	return 0
}

// getTopHatePointChar is `GetTopHatePointChar(mon)` - mon_ts_battle.scp:5 and the
// line the whole loop starts from. It answers an OBJECT or nil, never a number: the
// script's very next use is `target.ClassName` (:148) after a nil guard.
func (h *Host) getTopHatePointChar(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetTopHatePointChar", "the monster (self)")
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	if h.Hate == nil {
		h.reportNoHateWorld("GetTopHatePointChar")
		L.Push(lua.LNil)
		return 1
	}
	t, found := h.Hate.TopHate(mon)
	if !found {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(hateObject(L, t))
	return 1
}

// setTacticsTarget is `SetTacticsTarget(mon [, target])` - two args to store a
// target (mon_ts_battle.scp:164), ONE argument to clear it (:157).
//
// The reference's guard is copied: the same target is a no-op and a ZOMBIE target is
// refused (MonsterNPCFunc.cpp:264-270). Its two `SendMsg` calls (OM_NOTIFY_BEGIN_ATTACK
// / OM_NOTIFY_END_ATTACK) are NOT sent here - they are the attack engine's, and
// nothing in this card starts or stops an attack.
func (h *Host) setTacticsTarget(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "SetTacticsTarget", "the monster (self)")
	if !ok {
		return 0
	}
	// `if (1 != GetNumFunctionArgs())` is the reference's own test for "the second
	// argument was passed at all" (:258-260); nil is the same as absent.
	var target uint32
	if v := L.Get(2); v != lua.LNil && v != nil {
		hated, tok := h.hateHandle(L, 2, "SetTacticsTarget", "the new target")
		if !tok {
			return 0
		}
		target = hated
	}
	if h.Hate == nil {
		h.reportNoHateWorld("SetTacticsTarget")
		return 0
	}
	h.Hate.SetTacticsTarget(mon, target)
	return 0
}

// getTacticsTarget is `GetTacticsTarget(mon)` - mon_ts_battle.scp:101, :241, right
// beside GetTopHatePointChar because the loop compares them (IsSameTarget, :167).
func (h *Host) getTacticsTarget(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetTacticsTarget", "the monster (self)")
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	if h.Hate == nil {
		h.reportNoHateWorld("GetTacticsTarget")
		L.Push(lua.LNil)
		return 1
	}
	t, found := h.Hate.TacticsTarget(mon)
	if !found {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(hateObject(L, t))
	return 1
}

// isSameTarget is `IsSameTarget(obj1, obj2)` - mon_ts_battle.scp:167, where the
// script asks whether the target it just chose is the one it was already fighting.
//
// It answers a NUMBER (1/0): the reference pushes `PushFloat(1)` (:330) and the
// script compares `0 == IsSameTarget(...)`. Two objects that both resolve to no
// actor are the SAME actor to the reference - `char1 == char2` with both NULL is
// true (:340) - so nil/nil answers 1 here too.
func (h *Host) isSameTarget(L *lua.LState) int {
	a, aok := isSameTargetHandle(L.Get(1))
	b, bok := isSameTargetHandle(L.Get(2))
	if aok != bok {
		L.Push(lua.LNumber(0))
		return 1
	}
	if !aok && !bok {
		L.Push(lua.LNumber(1))
		return 1
	}
	if a == b {
		L.Push(lua.LNumber(1))
		return 1
	}
	L.Push(lua.LNumber(0))
	return 1
}

// isSameTargetHandle answers the actor behind one IsSameTarget argument. `nil` is a
// legitimate value there (the script passes a variable that may still be nil), so it
// is "no actor" rather than a fault; anything else must carry a Handle.
func isSameTargetHandle(v lua.LValue) (uint32, bool) {
	if v == nil || v == lua.LNil {
		return 0, false
	}
	t, ok := v.(*lua.LTable)
	if !ok {
		return 0, false
	}
	handle, ok := tableUint32(t, "Handle")
	if !ok || handle == 0 {
		return 0, false
	}
	return handle, true
}

// isAttacking is `IsAttacking(mon)` - mon_ts_battle.scp:67 and :162, the gate in
// front of StartAttack. The reference answers a StrID, which the corpus compares as
// the string 'YES'/'NO' (`if('NO' == IsAttacking(self))`), never as a boolean.
func (h *Host) isAttacking(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "IsAttacking", "the monster (self)")
	if !ok {
		L.Push(lua.LString("NO"))
		return 1
	}
	if h.Hate == nil {
		h.reportNoHateWorld("IsAttacking")
		L.Push(lua.LString("NO"))
		return 1
	}
	if h.Hate.IsAttacking(mon) {
		L.Push(lua.LString("YES"))
		return 1
	}
	L.Push(lua.LString("NO"))
	return 1
}
