package script

// THE RADIUS SCAN AND `SendMsg` - the Scarecrow Creator's second half.
//
// The game's own click script decides between "create a Scarecrow" and "remove the
// one already here" by LOOKING at the monsters around the NPC it was clicked on:
//
//	local count1 = GetNearObjectCount(self, 800, "Monster", 0)     -- :31
//	for i=0, count1 - 1 do
//	  local obj = GetNearObject(i)                                 -- :33
//	  ... if (obj ~= nil) and (obj.ClassID == HitDummyID[t])        -- :35
//
// and the remove branch kills what it found with
//
//	SendMsg(self, obj, 'OM_KILL', 0, 0)                            -- :45
//
// (out\lua_corpus\npc\evenpc\auch\npc_hitdummy_creater.lua). None of the three
// names existed here, so the stub at host.go answered 0, the loop never ran, and
// every click offered Create again - exactly what the owner saw.
//
// THREE THINGS THE ORIGINAL DOES THAT ARE NOT OBVIOUS, each copied rather than
// "improved" (`source2\geServer\ZServerLib\BindFunction\SystemFunc.cpp:776-849`):
//
//   - `GetNearObject` is **0-BASED**, and that is the script's own loop, not a
//     slip: `for i=0, count1-1`. It indexes the list the COUNT call left behind,
//     not a fresh search (:838-841 reads `g_AllObjectChecker`, a global).
//   - an index outside that list answers **nil**, never an error (:838-841
//     `IsValidIndex` -> `PushNull`), because the script guards with `obj ~= nil`.
//   - a scan that finds nothing is a COUNT OF 0 and an empty list. Nothing in this
//     file may raise: a Lua error inside a click aborts the whole dialog, so a
//     missing world, a wrong argument or an empty field must all end in 0/nil.
//
// WHAT THIS FILE DOES NOT DECIDE: which objects exist, how far 800 is, or what a
// kill does. That is the zone's, behind the NearWorld interface below - the same
// one-way split Spawner uses (script -> nothing, zone -> nothing, main -> both).

import (
	"fmt"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// NearObject is one live actor a radius scan found.
//
// The fields are the ones the game's own script reads off the object the original
// pushes (`PushObject(...->GetIESObject())`, SystemFunc.cpp:843): `ClassID` is what
// the fifteen Scarecrow ids are compared against, and it must keep that exact
// spelling - Lua is case sensitive and `obj.ClassID` is the read.
//
// Handle is what `SendMsg(self, obj, 'OM_KILL', 0, 0)` needs: the original takes the
// IES object and asks it for its OWNER (`context->GetObject(2)->GetOwner()`,
// SystemFunc.cpp:219), which is the actor. The handle is that link here.
type NearObject struct {
	Handle  uint32
	ClassID uint32
	X       int32
	Y       int32
}

// NearWorld is the live world behind the three builtins in this file.
//
// NIL is a legitimate state (a unit test, or a zone with scripts and no world), and
// a scan then answers 0 with one line saying so rather than pretending the field is
// empty - "nothing is near" and "nobody can look" must not read the same.
type NearWorld interface {
	// NearObjects returns every object of `kind` within `radius` of (x, y), in the
	// world's own order. radius is in the map's stored units; see the zone's own
	// file for what that unit is and why the test is strict.
	NearObjects(x, y int32, radius float64, kind string) ([]NearObject, error)

	// KillMonster takes one monster off the field and tells every player in the
	// zone. It is the ONLY thing `OM_KILL` does here.
	KillMonster(handle uint32) error
}

// installNear registers the three names.
//
// They are set, not left to the `__on_missing` stub, for the reason this whole
// package exists: the stub answers 0, and 0 here is a real answer ("no monster is
// near"), so the script took the CREATE branch forever and the missing feature was
// invisible.
func (h *Host) installNear() {
	h.L.SetGlobal("GetNearObjectCount", h.L.NewFunction(h.getNearObjectCount))
	h.L.SetGlobal("GetNearObject", h.L.NewFunction(h.getNearObject))
	h.L.SetGlobal("SendMsg", h.L.NewFunction(h.sendMsg))
}

// getNearObjectCount is `GetNearObjectCount(obj, range, type [, notUseChaseLimit])`.
//
// Arguments, from the original (:776-826):
//
//	1 the object the circle is centred on - here the NPC itself, `self`
//	2 range, in the map's units
//	3 the type word; "Monster" is the only one the Scarecrow asks for
//	4 notUseChaseLimit - and **0 does not mean "ignore"**, it means "use the
//	  world's chase limit" (:786, :789). That limit is not a distance: it drops a
//	  target that already has `chaseLimit` attackers (:114-122), so a fresh dummy
//	  with no attackers always passes. This zone keeps NO attacker list at all, so
//	  the fourth argument cannot change a single answer here and is deliberately not
//	  read - copying it would be a filter over a field we do not have. The number is
//	  left on the stack and ignored, and the comment says so rather than the code
//	  pretending to honour it.
//
// It answers the COUNT and leaves the list behind for GetNearObject - the original
// keeps it in `g_AllObjectChecker`, a global, and a fresh search per index would
// mostly agree and occasionally not. ONE list, snapshotted.
func (h *Host) getNearObjectCount(L *lua.LState) int {
	// A scan that fails must not leave the PREVIOUS list behind: the remove branch
	// calls this again (:40) and would otherwise walk a list of objects that are
	// already gone.
	h.nearList = nil

	self, ok := L.Get(1).(*lua.LTable)
	if !ok {
		h.logf("script: GetNearObjectCount needs the object it searches around as "+
			"argument 1 (the script passes self), got %s - the scan answers 0 and "+
			"nothing was searched", luaTypeName(L.Get(1)))
		L.Push(lua.LNumber(0))
		return 1
	}
	x, xok := tableInt(self, "X")
	y, yok := tableInt(self, "Y")
	if !xok || !yok {
		h.logf("script: GetNearObjectCount: argument 1 carries no X/Y, so there is " +
			"no centre to measure from - the scan answers 0")
		L.Push(lua.LNumber(0))
		return 1
	}

	radius, err := numArg(L, 2, "range")
	if err != nil {
		h.logf("script: GetNearObjectCount refused: %v - the scan answers 0 and "+
			"nothing was searched", err)
		L.Push(lua.LNumber(0))
		return 1
	}
	// The original squares the range (:807), so a NEGATIVE range would match
	// everything within |range|. That is not an answer worth copying: a negative
	// radius is a broken call, and "everything" is the one result a script cannot
	// tell from a correct one.
	if radius <= 0 {
		h.logf("script: GetNearObjectCount: range %g is not a positive distance - the "+
			"scan answers 0 and nothing was searched", radius)
		L.Push(lua.LNumber(0))
		return 1
	}

	kind, err := strArg(L, 3, "type")
	if err != nil {
		h.logf("script: GetNearObjectCount refused: %v - the scan answers 0 and "+
			"nothing was searched", err)
		L.Push(lua.LNumber(0))
		return 1
	}

	if h.Near == nil {
		h.logf("script: GetNearObjectCount(%g, %q) around (%d, %d): no live world is "+
			"wired to this script host, so nothing can be searched - answering 0",
			radius, kind, x, y)
		L.Push(lua.LNumber(0))
		return 1
	}

	found, err := h.Near.NearObjects(x, y, radius, kind)
	if err != nil {
		h.logf("script: GetNearObjectCount(%g, %q) around (%d, %d) found nothing: %v",
			radius, kind, x, y, err)
		L.Push(lua.LNumber(0))
		return 1
	}

	h.nearList = found
	L.Push(lua.LNumber(len(found)))
	return 1
}

// getNearObject is `GetNearObject(index)`: one object out of the list the last
// count left, by a **0-based** index (the script's own `for i=0, count1-1`).
//
// Out of range answers nil, which is the original's `PushNull` and what the
// script's `(obj ~= nil)` guard is written for.
func (h *Host) getNearObject(L *lua.LState) int {
	raw, err := numArg(L, 1, "index")
	if err != nil {
		h.logf("script: GetNearObject refused: %v - answering nil", err)
		L.Push(lua.LNil)
		return 1
	}
	// int() truncates, which is what the original's `int(GetFloat(1))` does.
	index := int(raw)
	if index < 0 || index >= len(h.nearList) {
		L.Push(lua.LNil)
		return 1
	}

	o := h.nearList[index]
	t := L.NewTable()
	t.RawSetString("Handle", lua.LNumber(o.Handle))
	t.RawSetString("ClassID", lua.LNumber(o.ClassID))
	t.RawSetString("X", lua.LNumber(o.X))
	t.RawSetString("Y", lua.LNumber(o.Y))
	// Any OTHER name the script reads off it answers 0 instead of nil - an IES
	// object has every declared property, and `nil == number` raises.
	propertyFallback(L, t)
	L.Push(t)
	return 1
}

// sendMsg is `SendMsg(from, to, message, arg1, arg2)`.
//
// The original hands the message to the target actor's own `OnMessage`
// (SystemFunc.cpp:215-226 -> MonsterNPC.cpp:76 `case OM_KILL: OnKill(...)`). Two
// messages are served here: OM_KILL, because it is what the Scarecrow's click sends,
// and OM_SKILL_MAGICAL_ASSIST, which is how a skill's own script hands its argument
// bag to the buff rule (script\skillmsg.go). A message this side did not implement
// must SAY SO rather than vanish: a script that "removed" a dummy which is still
// standing is the exact class of bug this lane exists to end.
//
// A kill is not chatty: the zone writes the ONE line for it (the handle, its class
// and where it stood), so nothing is logged here on success.
func (h *Host) sendMsg(L *lua.LState) int {
	msg, err := strArg(L, 3, "message")
	if err != nil {
		h.logf("script: SendMsg refused: %v - nothing was sent", err)
		return 0
	}

	target, ok := L.Get(2).(*lua.LTable)
	if !ok {
		h.logf("script: SendMsg(%q) needs the object to send to as argument 2, got "+
			"%s - nothing was sent", msg, luaTypeName(L.Get(2)))
		return 0
	}
	handle, hok := tableUint32(target, "Handle")
	if !hok || handle == 0 {
		h.logf("script: SendMsg(%q): argument 2 carries no handle, so there is no "+
			"actor to send it to - nothing was sent", msg)
		return 0
	}

	switch msg {
	case "OM_KILL":
		if h.Near == nil {
			h.logf("script: SendMsg(%q) to handle 0x%08x: no live world is wired to "+
				"this script host, so nothing was removed", msg, handle)
			return 0
		}
		if err := h.Near.KillMonster(handle); err != nil {
			// ONCE per handle. 3,638 `OM_KILL` calls sit in the corpus (pets,
			// ambers, spinels - objects this zone does not have), and a script that
			// asks for one of them on every click would otherwise bury the log in
			// one sentence. The first line names the handle, which is what a reader
			// needs; the repeat says nothing new.
			h.reportOnce(fmt.Sprintf("kill:0x%08x", handle),
				"script: SendMsg(%q) to handle 0x%08x was refused: %v (further "+
					"refusals for this handle are not logged)", msg, handle, err)
		}
		return 0
	case "OM_SKILL_MAGICAL_ASSIST":
		// THE SKILL'S BUFF: the bag the skill script filled is handed to the target's
		// own message script (script\skillmsg.go), which is where the game decides
		// whether the buff lands and calls AddBuff itself.
		h.skillMagicalAssist(L, handle)
		return 0
	default:
		h.reportUnhandledMessage(msg, handle)
		return 0
	}
}

// reportOnce writes one line the FIRST time a problem is seen, and nothing after.
//
// Once, not once per call: a script that sends one per tick (or a click that repeats
// the same broken call) would otherwise fill the zone's log with one sentence, and
// the first line is the one a reader needs.
func (h *Host) reportOnce(key, format string, args ...any) {
	h.reportMu.Lock()
	if h.reported == nil {
		h.reported = map[string]bool{}
	}
	if h.reported[key] {
		h.reportMu.Unlock()
		return
	}
	h.reported[key] = true
	h.reportMu.Unlock()
	h.logf(format, args...)
}

// reportUnhandledMessage says ONCE per name that a message is not served. The name
// and the handle are both in it so the missing piece is findable.
func (h *Host) reportUnhandledMessage(msg string, handle uint32) {
	if msg == "" {
		h.logf("script: SendMsg was called with no message name - nothing was sent")
		return
	}
	h.reportOnce("msg:"+msg,
		"script: SendMsg message %q is NOT served by this host - nothing was done "+
			"(first call was to handle 0x%08x; further calls are not logged)",
		msg, handle)
}

// UnservedMessages lists, sorted, the message names that reached SendMsg's default
// arm - the ones this host was asked for and does not serve. It is the log's own list
// (one entry per name, because the line is once per name), turned into an answer a
// caller can print as a count: the next message slice is chosen from it instead of
// guessed.
func (h *Host) UnservedMessages() []string {
	if h == nil {
		return nil
	}
	h.reportMu.Lock()
	out := make([]string, 0, 4)
	for key := range h.reported {
		if name, ok := strings.CutPrefix(key, "msg:"); ok {
			out = append(out, name)
		}
	}
	h.reportMu.Unlock()
	sort.Strings(out)
	return out
}

// tableInt reads one numeric field of a table as an int32, and reports whether it
// was there as a number. It is used for X/Y on objects this package builds, so a
// missing field is a programming fault rather than game data - which is why the
// caller logs instead of assuming 0.
func tableInt(t *lua.LTable, name string) (int32, bool) {
	n, ok := t.RawGetString(name).(lua.LNumber)
	if !ok {
		return 0, false
	}
	return int32(n), true
}

// tableUint32 is tableInt for a HANDLE. Handles live above the sign bit of an
// int32 (the NPC block is 0x40000000, the warp block 0x7F000000), so they are read
// as unsigned - narrowing one through int32 would turn a real handle into a
// negative number and then into a different actor.
func tableUint32(t *lua.LTable, name string) (uint32, bool) {
	n, ok := t.RawGetString(name).(lua.LNumber)
	if !ok {
		return 0, false
	}
	return uint32(n), true
}
