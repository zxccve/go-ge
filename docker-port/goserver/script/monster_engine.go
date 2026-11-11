// THE BATTLE SCRIPT'S READ-ONLY AND TACTIC ENGINE CALLS - bound here, decided
// NOWHERE here.
//
// WHY THIS FILE EXISTS. `ge\script\monster\mon_ts_battle.scp` runs as a script and
// asks the ENGINE five questions before it does anything: which AI row this body
// runs, how far away something is, where this body spawned, and what its tactics
// state is. The owner's ruling (2026-11-07) is that the script decides and Go only
// stores and answers - "whatever the scp is doing it should do by itself, nothing
// gets coded in native Go" - so every name below is the reference's own question
// with the reference's own answer, and not one of them picks a target, a tactic or
// a distance rule.
//
// WHAT IS BOUND, with the arity the script itself calls it with (grep of
// `ge\script\monster\mon_ts_battle.scp` and `mon_ai_lib.scp`; the translated corpus
// calls the same way):
//
//	ChangeTactics(self, 'TS_X')          mon_ts_battle.scp:158,:192,:203,:210,:217
//	GetDistance(a, b)                    mon_ts_battle.scp:185,:197; mon_ai_lib.scp:96...
//	GetMonAiOption(self, 'AIType')       mon_ts_battle.scp:11-18 (eight keys)
//	GetMyHomeDistance(self)              mon_ts_battle.scp:250
//	GetMyHomePosition(self)              mon_ts_battle.scp:251 (three returns)
//
// THE REFERENCE'S BINDINGS, each read before this file was written:
// MonsterNPCFunc.cpp:24-30 (ChangeTactics), :727-742 (GetMyHomeDistance),
// :752-766 (GetMyHomePosition), :830-862 (GetMonAiOption) and
// FieldCharFunc.cpp:235-262 (GetDistance). The two state readers are
// MonsterNPC.cpp:1906-1915, the distance itself FieldChar.cpp's `GetDomainRadius`
// (:660-663) with Common.cpp:39-45.
//
// FIVE THINGS A CENSUS OF THE NAMES WOULD HAVE GOT WRONG, and they change the
// answers:
//
//   - `ChangeTactics` sets the TACTICS STATE, and `GetMonAiOption` reads the AI ROW.
//     They are two different strings on one body. `SetTacticsState(TS_BATTLE)` moves
//     the state machine (MonsterNPC.cpp:255, `IMPL_STATE(TS_RALLY1)` -> ":2453"), while
//     `GetMonAiOption` asks `imcIES::GetClass(MonsterAi, obj->GetTactics())`
//     (:843) - and `GetTactics()` is the IES row's `Tactics` COLUMN (a row name such
//     as `MON_AGGRESSIVE`), declared `DECLARE_STRING_PROPERTY(Tactics)` at
//     FieldChar.h:107. The same file uses BOTH in one statement
//     (`TS_NONE == GetTacticsState()` and `GetMonAiOption`'s row, MonsterNPC.cpp:696),
//     which is the proof they are not one field. So changing the tactics state
//     changes no AI option - and the AI option is read from the row the monster SPAWNED
//     with unless a script overrides `Tactics` at spawn.
//   - `ChangeTactics` with a name the state table does not hold DOES NOTHING. The
//     binding sends `OM_SET_TACTICS_STATE` (MonsterNPCFunc.cpp:29) and the handler
//     looks the name up: `state = GetStateTable()->GetState(name); if (INVALID_STATE
//     == state) return;` (FieldChar_Message.cpp:608-616). A body keeps its previous
//     state. The shipped scripts do call undeclared names (`TS_RALLY9`..
//     `TS_RALLY23`, monnpc_viron_basic.scp:132-314) - those are silent no-ops in the
//     reference, and the zone refuses them the same way.
//   - `GetDistance` is EDGE TO EDGE and can be NEGATIVE:
//     `sqrt(dx*dx+dy*dy) - (from->GetDomainRadius() + to->GetDomainRadius())`
//     (FieldCharFunc.cpp:254-258). The two radii are the collision bodies
//     (FieldChar.cpp:660-663, `m_shapeSize`), so two bodies standing on each other
//     answer a negative number. Z is NOT in it. A null object answers NULL
//     (:238-243), which is why the script guards `dist != nil` in
//     mon_ai_lib.scp:97 before comparing.
//   - `GetMyHomeDistance` is NOT the same arithmetic as `GetDistance`: it is
//     `int distance = GetDistance(m_createdPos, GetPos());` (MonsterNPC.cpp:1906-1910)
//   - the FREE function at Common.cpp:39-45, centre to centre, NO radii - and the
//     int is pushed as a float. So the script's `homedist > 3500` is a
//     centre-to-centre comparison while a skill's `dist` is an edge gap.
//   - `GetMonAiOption` is not optional and not a monster-table read: it needs the
//     KEY (`context->GetStringID(2)`, :835) and it reads the `MonsterAi` idspace's
//     row, answering a STRING property as a string ('TRUE'/'FALSE'/'AGGRESSIVE') and
//     any other as a float. A body that is not a monster or pet answers NULL
//     (:838-841), and so does a row or a property the table does not hold.
//
// THE WORLD IS AN INTERFACE, NOT A MAP, and it is the same one-way split
// `HateWorld` and `NearWorld` keep: this package knows Lua, the zone knows the
// field. NIL means "no world wired" and every name then answers its EMPTY value
// (nothing / nil / 0) and says so ONCE - never a made-up distance, because a wrong
// distance moves a monster.
package script

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// MonsterHome is one body's spawn point - the reference's `m_createdPos`
// (`WORLDPOS m_createdPos;`, MonsterNPC.h:373), set once when the body enters the
// world (`m_createdPos = pos;`, MonsterNPC.cpp:1409-1414, inside OnEnter) and never
// touched again. X/Y are the world point; Z is NOT stored by the reference either -
// `GetMyHomePosition` answers `GetWorld()->GetHeight(createdPos)` (MonsterNPCFunc.cpp:
// 764-765) - but this side carries it so a home answers its height without a mesh
// lookup per call.
type MonsterHome struct {
	X, Y, Z int32
}

// MonsterOption is one column of the body's AI row, as the script reads it.
//
// IsText is the reference's own split: `if (imcIES::PROPERTY_ENTRY::STRING ==
// propertyEntry->type) PushStringID(...) else PushFloat(...)` (MonsterNPCFunc.cpp:
// 856-860). The client's XML has no per-attribute type, so the zone decides by the
// column's VALUE (a TRUE/FALSE or a name is text, a number is a number) - and every
// key the battle script reads lands on the correct side: AIType/Attack/Hatred/
// LtdMinion/RunAway/SelfBomb are strings there, HateRange/HateCount are floats.
type MonsterOption struct {
	Text   string
	Num    float64
	IsText bool
}

// MonsterWorld is the live world behind the five builtins: one body's tactics state
// and spawn point, the AI row it runs, and the distance between two bodies - keyed
// by the MONSTER's handle for the first two and by both handles for the last.
//
// Nothing here returns an error. A question that cannot be answered answers its
// empty value and the ZONE logs why, which is the contract `SendMsg` keeps: a Lua
// error inside a script run aborts the whole run, a missing answer does not.
type MonsterWorld interface {
	// ChangeTactics stores `tactic` as the body's tactics STATE, and answers
	// nothing. A name the state table does not declare leaves the standing state
	// alone (FieldChar_Message.cpp:609-612).
	ChangeTactics(mon uint32, tactic string)
	// Home answers the body's spawn point, and false for an actor this field never
	// spawned - the reference answers NULL for a NULL object (:734-738).
	Home(mon uint32) (MonsterHome, bool)
	// HomeDistance answers `int GetDistance(m_createdPos, GetPos())` as the float
	// the binding pushed (MonsterNPC.cpp:1906-1910): CENTRE TO CENTRE from the spawn
	// point, no radii, truncated to an int. false means "no spawn point for this
	// handle", which the reference answers as 0 (:734-738).
	HomeDistance(mon uint32) (float64, bool)
	// Distance answers the EDGE-TO-EDGE distance between two live bodies
	// (FieldCharFunc.cpp:254-258); false when either handle is not a body on this
	// field, which is the reference's NULL.
	Distance(a, b uint32) (float64, bool)
	// AiOption answers one column of the AI row the body's `Tactics` names
	// (`MonsterAi`, MonsterNPCFunc.cpp:843-862); false when there is no body, no
	// row, or no such column.
	AiOption(mon uint32, key string) (MonsterOption, bool)
}

// installMonsterEngine registers the five names. Called once, from New().
//
// They are SET rather than left to the `__on_missing` stub for the reason this
// package exists: the stub answers 0, and 0 is a MEANINGFUL answer here - `nil`
// means "no such body" and a distance of 0 is two bodies touching - so a stub would
// make the battle script believe every target is on top of it, with the missing name
// counted in a log line nobody is reading during a fight.
func (h *Host) installMonsterEngine() {
	h.L.SetGlobal("ChangeTactics", h.L.NewFunction(h.changeTactics))
	h.L.SetGlobal("GetDistance", h.L.NewFunction(h.getDistance))
	h.L.SetGlobal("GetMonAiOption", h.L.NewFunction(h.getMonAiOption))
	h.L.SetGlobal("GetMyHomeDistance", h.L.NewFunction(h.getMyHomeDistance))
	h.L.SetGlobal("GetMyHomePosition", h.L.NewFunction(h.getMyHomePosition))
}

// reportNoMonsterWorld is the ONCE line every name in this file writes when the
// host has no zone behind it. One key for all five: the reader needs to know that
// the battle AI's answers are empty, not which name asked first.
func (h *Host) reportNoMonsterWorld(name string) {
	h.reportOnce("monster:no-world",
		"script: %s answered its EMPTY value - this host has no monster world wired, "+
			"so every tactics/AI-row/home/distance answer is nil or 0 (further calls "+
			"are not logged)", name)
}

// changeTactics is `ChangeTactics(mon, 'TS_X')` - mon_ts_battle.scp:158 and the four
// lines after it. The reference's binding takes the object and a StrID and sends
// `OM_SET_TACTICS_STATE` to the body's own state machine (MonsterNPCFunc.cpp:24-30);
// it pushes NO return value, so neither does this.
func (h *Host) changeTactics(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "ChangeTactics", "the monster (self)")
	if !ok {
		return 0
	}
	tactic, terr := strArg(L, 2, "tactic")
	if terr != nil {
		h.logf("script: ChangeTactics: %v - nothing was stored", terr)
		return 0
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("ChangeTactics")
		return 0
	}
	h.Monster.ChangeTactics(mon, tactic)
	return 0
}

// getDistance is `GetDistance(a, b)` - mon_ts_battle.scp:185/:197 and the eight
// skill-condition reads in mon_ai_lib.scp.
//
// It answers ONE number (which may be negative: two bodies closer than their radii
// sum, FieldCharFunc.cpp:254-258) or nil when either side is not a live body - the
// reference's `PushNull()` for a NULL object, and the reason the script writes
// `dist != nil &&` before every comparison.
func (h *Host) getDistance(L *lua.LState) int {
	a, aok := h.hateHandle(L, 1, "GetDistance", "the first body")
	b, bok := h.hateHandle(L, 2, "GetDistance", "the second body")
	if !aok || !bok {
		L.Push(lua.LNil)
		return 1
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("GetDistance")
		L.Push(lua.LNil)
		return 1
	}
	d, ok := h.Monster.Distance(a, b)
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(lua.LNumber(d))
	return 1
}

// getMonAiOption is `GetMonAiOption(mon, 'AIType')` - mon_ts_battle.scp:11-18, the
// eight reads the whole battle profile hangs off, and mon_ai_lib.scp:1223/'AIType'.
//
// The key is REQUIRED by every call site; the reference reads it unconditionally
// (`context->GetStringID(2)`, MonsterNPCFunc.cpp:835) and a missing one resolves no
// property, which answers NULL - so a one-argument call answers nil here too, with
// one log line, rather than a guess at which column was meant.
func (h *Host) getMonAiOption(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetMonAiOption", "the monster (self)")
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	key, kerr := strArg(L, 2, "column name")
	if kerr != nil {
		h.reportOnce("monster:ai-key",
			"script: GetMonAiOption: %v - the reference resolves no property for a "+
				"missing name and answers NULL, so nil was answered (further calls "+
				"are not logged)", kerr)
		L.Push(lua.LNil)
		return 1
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("GetMonAiOption")
		L.Push(lua.LNil)
		return 1
	}
	opt, found := h.Monster.AiOption(mon, key)
	if !found {
		L.Push(lua.LNil)
		return 1
	}
	if opt.IsText {
		L.Push(lua.LString(opt.Text))
		return 1
	}
	L.Push(lua.LNumber(opt.Num))
	return 1
}

// homeSelfKillRange is the threshold the NONE entry tests against this answer -
// `if(homedist > 4000)` (mon_ts_none.scp:107-112). It is the script's number, quoted
// here only so the log line below can say WHEN the branch was reached; nothing in this
// file compares it to make a decision.
const homeSelfKillRange = 4000

// getMyHomeDistance is `GetMyHomeDistance(mon)` - mon_ts_battle.scp:250 (the SET_MINION
// branch's "have I wandered too far from where I spawned") and the NONE entry's
// home-sick check at mon_ts_none.scp:107-112:
//
//	if('TRUE' == homeSick){
//	    var homedist = GetMyHomeDistance(self);
//	    if(homedist > 4000){ SendMsg(self, self, 'OM_KILL', 0, 0); }
//	}
//
// THE REFERENCE'S ARITHMETIC IS `int GetDistance(m_createdPos, GetPos())`
// (MonsterNPC.cpp:1906-1910 + MonsterNPCFunc.cpp:739-740): CENTRE TO CENTRE, no radii
// subtracted. The ZONE still answers exactly that (`MonsterWorld.HomeDistance`) and this
// file still asks it on every call - but the value that reaches the SCRIPT is 0.
//
// WHY 0 IS THE LIVE ANSWER (the owner's test, 2026-11-08, on the live server). A
// Bigfoot kited far from its spawn, lost him, and did NOT kill itself: it turned and
// walked home, exactly like the bear before it. Both of those rows are `HomeSick TRUE`
// in the client table (`MON_AGGRESSIVERUN` 11483's row, `MON_AGGRESSIVE` 11591's) and
// both generations carry it - yet live never fires the branch, at ~7,000 units (bear)
// or on the Bigfoot's own map. Live's wander hugs the spawn, so its `m_createdPos` IS
// the birth point and the only reading that fits every observation is that this call
// answers small there: an unbound/stubbed builtin - the branch never taken, the rest of
// the idle loop unaffected, and the walk home still happening because the WANDER carries
// the anchor (`RandomMoveCreatedLocaly` draws within its range of `m_createdPos`,
// MonsterNPC.cpp:120-133), not this call.
//
// SO THE TWO QUANTITIES ARE SPLIT, which is the whole point of this binding: the WANDER
// ANCHOR is untouched (the zone's `Home`, still `m_createdPos`), and this ANSWER is
// live's 0. Our own build had been firing the branch as written - `zone: OM_KILL` at
// 7,940 and 8,038 units from spawn on 2026-11-10 - which is a behaviour live does not
// have.
//
// THE TRUE NUMBER IS NOT THROWN AWAY: it is read on every call and, the first time a
// body is past the script's own 4,000, written ONCE per body - so the record shows what
// the engine measured and that this build chose live's answer on purpose. The SET_MINION
// branch (`homedist > 3500` -> walk home) reads the same 0, which means "no return
// needed" - also what live shows.
func (h *Host) getMyHomeDistance(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetMyHomeDistance", "the monster (self)")
	if !ok {
		L.Push(lua.LNumber(0))
		return 1
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("GetMyHomeDistance")
		L.Push(lua.LNumber(0))
		return 1
	}
	// The distance itself is the world's answer, not this file's arithmetic: the
	// body's live point is the zone's, and a second copy here would drift from the
	// one `GetDistance` uses. A handle with no spawn point answers 0, the
	// reference's NULL-object answer (MonsterNPCFunc.cpp:734-738) - which is the same
	// 0 the script gets either way, so only a KNOWN home is worth a log line.
	d, known := h.Monster.HomeDistance(mon)
	if !known {
		L.Push(lua.LNumber(0))
		return 1
	}
	if d > homeSelfKillRange {
		h.reportOnce("homedist:"+hexMonster(mon),
			"zone: HOMEDIST 0x%08x %s is %d from its birth point - no self-kill "+
				"(live does not fire this)", mon, h.classLabel(L, 1), int(d))
	}
	L.Push(lua.LNumber(0))
	return 1
}

// classLabel is the `<class>` field of the HOMEDIST line: the body's own `ClassName` off
// the `self` table the runner refreshed (`refreshMonsterSelf`), then `Name`, then the
// numeric ClassID, then "?" - a reader needs to know WHICH body, and a log line must not
// be the thing that raises.
func (h *Host) classLabel(L *lua.LState, i int) string {
	t, ok := L.Get(i).(*lua.LTable)
	if !ok {
		return "?"
	}
	for _, key := range []string{"ClassName", "Name"} {
		if v := t.RawGetString(key); v.Type() == lua.LTString && lua.LVAsString(v) != "" {
			return lua.LVAsString(v)
		}
	}
	if id, ok := tableUint32(t, "ClassID"); ok && id != 0 {
		return fmt.Sprintf("class %d", id)
	}
	return "?"
}

// getMyHomePosition is `GetMyHomePosition(mon)` - mon_ts_battle.scp:251, read as
// `var x, y, z = ...` and handed straight to `Move(self, x, y, z)`:
//
//	const WORLDPOS& createdPos = obj->GetMyHomePosition();
//	context->PushFloat(createdPos.x);
//	context->PushFloat(createdPos.y);
//	context->PushFloat(obj->GetWorld()->GetHeight(createdPos));
//
// (MonsterNPCFunc.cpp:762-766.) X and Y are the spawn point; Z is the map's OWN
// height at that point, never a number this side picked.
//
// THE REFERENCE'S NULL PATH PUSHES ONE VALUE, NOT THREE (`context->PushFloat(0)`,
// :756-759): with no object, `y` and `z` would read nil. That path is unreachable
// from a running script - `self` is always a pushed object - and it is not copied,
// because a nil y would raise inside `Move(self, x, y, z)` instead of moving. A
// handle this field never spawned answers 0, 0, 0 and says so once.
func (h *Host) getMyHomePosition(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetMyHomePosition", "the monster (self)")
	if !ok {
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		return 3
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("GetMyHomePosition")
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		return 3
	}
	home, known := h.Monster.Home(mon)
	if !known {
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		return 3
	}
	L.Push(lua.LNumber(home.X))
	L.Push(lua.LNumber(home.Y))
	L.Push(lua.LNumber(home.Z))
	return 3
}
