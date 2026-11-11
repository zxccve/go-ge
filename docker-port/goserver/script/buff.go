// THE BUFF BUILTINS - the nine names the corpus applies a buff with, plus the two
// runners that call a buff's own ENTER script.
//
// WHY THIS FILE REPLACES TWO STUBS. `script\status.go` used to bind `IsBuffApplied`
// to a constant "NO" and `GetAppliedBuffLevel` to 0, with a comment saying so: "this
// server has no buff system". That was true and it is the finding the buff slice
// exists to delete: 870 call sites in the buff corpus ask `GetAppliedBuffLevel` and
// every one of them got 0, which is the level of a buff that is NOT APPLIED
// (`CBuffList::GetAppliedBuffLevel` answers -1 for "no such buff", BuffList.cpp:527).
//
// THE ENGINE THESE SIT ON IS THE ZONE'S. A buff list belongs to a character, and a
// character belongs to a session, so this package cannot own one: it asks a
// BuffWorld (implemented by `zone\buff.go`) and this file never invents a duration,
// a level or a mask. With NO world wired every one of the nine answers its EMPTY
// value and says so ONCE per name - never a made-up "YES", because a script that
// believes a buff is up takes the wrong branch for the rest of its run.
//
// THE REFERENCE'S BODIES, quoted so the argument order cannot drift:
//
//	AddBuff(self, name, arg1, arg2, arg3)      FieldCharFunc.cpp:394-410
//	    -> to->AddBuff(name,arg1,arg2,arg3); to->SendChangeBuff();
//	AddBuffWithTime(..., arg3, additionalTime)  FieldCharFunc.cpp:426-...
//	AddBuffImm   - the SAME body as AddBuff     FieldCharFunc.cpp:503-517
//	RemoveBuff(self, name)                      FieldCharFunc.cpp:520-...
//	RemoveBuffDB(self, name) -> RemoveBuff(name, true)   site above
//	AddBuffToTeam(self, name, a1, a2, a3)       PlayerCharFunc.cpp:5931-5949
//	    -> commander->AddBuffToAllPC(name, a1, a2, a3)
//	RemoveBuffToTeam(self, name)                PlayerCharFunc.cpp:5951-...
//	IsBuffApplied(self, name) -> 'YES' / 'NO'   FieldCharFunc.cpp:2609
//	GetAppliedBuffLevel(self, name) -> level    FieldCharFunc.cpp:2610
//
// `AddBuffImm` and `AddBuff` are bound to the same behaviour ON PURPOSE: in THIS
// generation of the reference they are byte-identical (both call `to->AddBuff`), and
// the difference the names suggest lives in a later client. Binding a difference
// nobody measured would be an invented rule.
package script

import (
	"context"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// BuffTimeout bounds ONE buff ENTER run. These are four-line scripts that set
// ApplyTime and two `*_BM` values (`buff.scp:22-26` is the whole shape), so a
// deadline is only there to stop a runaway loop in a script nobody wrote by hand.
const BuffTimeout = 3 * time.Second

// BuffWorld is the ZONE behind the nine buff builtins: the per-character buff
// lists, their ranks, their mask and their packets. Every method takes the
// character's HANDLE, because a script names its target with `self` and this
// package must not know what a Character is.
//
// ok=false plus a reason is a REFUSAL the zone has already logged; the builtins are
// void in the reference, so a script cannot tell the difference and must not be able
// to. `Team` answers the handles of the session's own team, self first - what
// `AddBuffToTeam` walks (`commander->AddBuffToAllPC`).
type BuffWorld interface {
	AddBuff(handle uint32, name string, arg1, arg2, arg3, addTime float64) (ok bool, why string)
	RemoveBuff(handle uint32, name string, inDB bool) (ok bool, why string)
	IsBuffApplied(handle uint32, name string) bool
	AppliedBuffLevel(handle uint32, name string) float64
	TeamBuffTargets(handle uint32) []uint32
}

// BuffEnterCall is ONE `SCR_BUFF_ENTER_<ClassName>` run: the owner, the buff's own
// row as the script's `buff` object, and the three numbers the reference pushes
// after it (Buff.cpp:71-76).
type BuffEnterCall struct {
	// Entry is `SCR_BUFF_ENTER_<ClassName>`.
	Entry string
	// Source is the status source the script's data-table and item builtins read,
	// exactly as a skill cast installs one. NIL is legal: every read then answers
	// its empty value and says why once.
	Source StatusSource
	// Self is the character the buff lands on - `self` inside the script.
	Self StatusSelf
	// Row is the buff's own table row (zone\buff_table.go, BuffRow.Lua): the
	// `buff` object the script writes `ApplyTime` and the `*_BM` values into.
	Row map[string]any
	// Arg1/Arg2/Arg3 are the reference's three floats. `skilllv` - the name the
	// corpus gives the third argument (`SCR_BUFF_ENTER_Positions(self, buff,
	// skilllv, arg2, AddAppTime)`) - is Arg1.
	Arg1, Arg2, Arg3 float64

	// World is the buff list the ENTER's own buff builtins read and write. The zone
	// passes it so a script that asks `IsBuffApplied(self, 'X')` gets the TRUE answer
	// and one that adds a second buff lands (an ENTER may do both - the reference runs
	// OnEnter with the list already in place). NIL is legal: the builtins then answer
	// their empty value and say so once.
	World BuffWorld
}

// BuffEnterResult is what the script CHANGED about the buff - read back off the
// `buff` table after the run, which is the only place the duration exists.
type BuffEnterResult struct {
	// Ran is false when the corpus defines no ENTER for this buff (546 of the 1,962
	// rows measured against `ge\script\**`). A scriptless buff is a NORMAL buff with
	// no duration, and the zone REFUSES it rather than granting a 0.
	Ran bool
	// ApplyTime is `buff.ApplyTime` after the run, in milliseconds, as the script
	// left it - seeded from the row's own column and overwritten by the script
	// (`buff.ApplyTime = 30000`). It is NOT defaulted here: 0 after the run means
	// the script did not set a duration, and the zone's rule decides what that means.
	ApplyTime float64
	// BM is every `*_BM` property the script set to a non-zero number, by name
	// (`DR_BM`, `IH_BM`, `ATK_BM`, ...). They are the stat terms the reference's
	// `ApplyBMProperties` adds to the owner (Buff.cpp:231-254); this slice carries
	// them on the buff and does NOT put them in the stat sheet - the sheet is shadow
	// (zone\status.go) and applying them there is its own card.
	BM map[string]float64
}

// RunBuffEnter runs one buff's ENTER script and reads the buff back out of it.
//
// A buff with no ENTER is NOT an error: it answers Ran=false and no error, because
// "the corpus does not define this one" is a property of the data (546 rows) and the
// caller has a rule for it. An ENTER that RAISES is an error, because a duration
// nobody computed must not be mistaken for a duration of 0.
func (h *Host) RunBuffEnter(c BuffEnterCall) (BuffEnterResult, error) {
	var res BuffEnterResult
	if h == nil || h.L == nil {
		return res, fmt.Errorf("%s: no script host", c.Entry)
	}
	if c.Entry == "" {
		return res, nil
	}
	// NOT DEFINED BY ANY FILE = SCRIPTLESS, and that is answered from the INDEX, not
	// from a count in a comment: the zone asks once per add and the answer is the
	// corpus's own.
	if !h.HasScript(c.Entry) {
		return res, nil
	}
	if _, err := h.EnsureScript(c.Entry); err != nil {
		return res, fmt.Errorf("%s: %w", c.Entry, err)
	}

	defer h.withStatus(c.Source, c.Self)()
	if c.World != nil {
		defer h.withBuffWorld(c.World)()
	}
	L := h.L
	self := h.statusSelfTable(c.Self)
	buff := statusTable(L, c.Row)

	h.ticks = 0
	if err := h.callBuffEntry(c.Entry, self, buff, c.Arg1, c.Arg2, c.Arg3); err != nil {
		return res, err
	}
	res.Ran = true

	// READ THE BUFF BACK. `ApplyTime` is the duration; every `*_BM` the script set is
	// a stat term. The walk is over the TABLE the script was handed, so a property
	// this build has never heard of is caught by its own suffix rather than by a list
	// that would fall behind the corpus.
	res.BM = map[string]float64{}
	buff.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		n, ok := v.(lua.LNumber)
		if !ok {
			return
		}
		switch {
		case string(name) == "ApplyTime":
			res.ApplyTime = float64(n)
		case len(name) > 3 && string(name)[len(name)-3:] == "_BM":
			if n != 0 {
				res.BM[string(name)] = float64(n)
			}
		}
	})
	return res, nil
}

// callBuffEntry calls the ENTER once, with a deadline and under Protect.
func (h *Host) callBuffEntry(entry string, self, buff *lua.LTable, a1, a2, a3 float64) error {
	fn, ok := h.L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		return fmt.Errorf("%s is not a function (was its file loaded?)", entry)
	}
	ctx, cancel := context.WithTimeout(context.Background(), BuffTimeout)
	defer cancel()
	defer h.withContext(ctx)()
	return h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		self, buff, lua.LNumber(a1), lua.LNumber(a2), lua.LNumber(a3))
}

// RunItemBuffScript runs ONE item's own `ScpType` with the four arguments the
// reference pushes for it: `self`, the item's BuffName, its arg1 and its arg2
// (`CPlayerChar::UseItem`, PlayerChar.cpp:2245-2250).
//
// returned=false means the corpus does not define that name: the caller keeps what
// it did before (announce the class, consume the item) instead of pretending an
// effect ran. An error is a script that raised; the caller says so and the item is
// still consumed, which is the reference's own order (the transaction takes the item
// before `context->Run`, TxUseItem.cpp:238 -> PlayerChar.cpp:2250).
func (h *Host) RunItemBuffScript(entry string, src StatusSource, self StatusSelf,
	buffName string, arg1, arg2 float64, w BuffWorld) (bool, error) {

	if h == nil || h.L == nil {
		return false, fmt.Errorf("%s: no script host", entry)
	}
	if entry == "" || !h.HasScript(entry) {
		return false, nil
	}
	if _, err := h.EnsureScript(entry); err != nil {
		return false, fmt.Errorf("%s: %w", entry, err)
	}
	defer h.withStatus(src, self)()
	defer h.withBuffWorld(w)()

	L := h.L
	fn, ok := L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		return false, fmt.Errorf("%s is not a function (was its file loaded?)", entry)
	}
	h.ticks = 0
	ctx, cancel := context.WithTimeout(context.Background(), BuffTimeout)
	defer cancel()
	defer h.withContext(ctx)()
	return true, L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		h.statusSelfTable(self), lua.LString(buffName), lua.LNumber(arg1), lua.LNumber(arg2))
}

// withBuffWorld installs the world the nine buff builtins ask, for the length of
// one run, and puts back whatever was there.
//
// IT SAVES AND RESTORES RATHER THAN CLEARING, because a buff ENTER may add another
// buff (`AddBuff` inside an ENTER is legal in the reference - `CBuffList::Add` runs
// `OnEnter` and the script is free to call back in). A clear-on-exit would leave the
// INNER run's exit without a world for the outer script's remaining lines.
func (h *Host) withBuffWorld(w BuffWorld) func() {
	prev := h.buffs
	if w != nil || prev == nil {
		h.buffs = w
	}
	return func() { h.buffs = prev }
}

// installBuffs binds the nine names. Called once, from New().
//
// EVERY ONE OF THEM IS A NO-OP WITHOUT A WORLD, with one line, and that is the
// difference from the stub it replaces: the stub answered a VALUE ("NO", 0) that a
// script could act on, while a no-op changes nothing and says why.
func (h *Host) installBuffs() {
	L := h.L

	// buffTarget reads `self` and answers the handle it belongs to. A `self` without
	// the hidden handle field is not a character this host built, and answering for
	// it would silently apply a buff to nobody - so it is refused by name.
	buffTarget := func(L *lua.LState, luaName string) (uint32, bool) {
		self := argSelf(L, 1)
		if self == nil {
			h.reportOnce("buff:"+luaName+":noself", "script: %s called with no self "+
				"table - nothing done (a buff needs a character to land on)", luaName)
			return 0, false
		}
		v, ok := self.RawGetString(statusHandleKey).(lua.LNumber)
		if !ok {
			h.reportOnce("buff:"+luaName+":nohandle", "script: %s called with a self "+
				"this host did not build (no handle) - nothing done", luaName)
			return 0, false
		}
		if h.buffs == nil {
			h.reportOnce("buff:"+luaName+":noworld", "script: %s - no buff world is "+
				"wired for this run, so nothing is applied and no packet is sent", luaName)
			return 0, false
		}
		return uint32(v), true
	}

	addOne := func(L *lua.LState, luaName string, addTime float64) int {
		handle, ok := buffTarget(L, luaName)
		if !ok {
			return 0
		}
		h.buffs.AddBuff(handle, argString(L, 2), argNumber(L, 3), argNumber(L, 4),
			argNumber(L, 5), addTime)
		return 0
	}

	L.SetGlobal("AddBuff", L.NewFunction(func(L *lua.LState) int {
		return addOne(L, "AddBuff", 0)
	}))
	L.SetGlobal("AddBuffImm", L.NewFunction(func(L *lua.LState) int {
		// The same body as AddBuff on purpose: see the file header.
		return addOne(L, "AddBuffImm", 0)
	}))
	L.SetGlobal("AddBuffWithTime", L.NewFunction(func(L *lua.LState) int {
		// The sixth argument is the reference's `AdditionalTime`, added to the
		// duration AFTER the ENTER script has run (`SetApplyTime(GetApplyTime() +
		// additionalTime)`, Buff.cpp:83-86).
		return addOne(L, "AddBuffWithTime", argNumber(L, 6))
	}))

	removeOne := func(L *lua.LState, luaName string, inDB bool) int {
		handle, ok := buffTarget(L, luaName)
		if !ok {
			return 0
		}
		h.buffs.RemoveBuff(handle, argString(L, 2), inDB)
		return 0
	}
	L.SetGlobal("RemoveBuff", L.NewFunction(func(L *lua.LState) int {
		return removeOne(L, "RemoveBuff", false)
	}))
	L.SetGlobal("RemoveBuffDB", L.NewFunction(func(L *lua.LState) int {
		// The DB half is not built (this slice persists no buff), and the in-memory
		// removal is the same one - so the call is honoured and says once that nothing
		// was written.
		h.reportOnce("buff:RemoveBuffDB", "script: RemoveBuffDB - this build persists "+
			"no buff, so the buff is removed from the list only (no database row exists)")
		return removeOne(L, "RemoveBuffDB", true)
	}))

	L.SetGlobal("AddBuffToTeam", L.NewFunction(func(L *lua.LState) int {
		handle, ok := buffTarget(L, "AddBuffToTeam")
		if !ok {
			return 0
		}
		name := argString(L, 2)
		for _, h2 := range h.buffs.TeamBuffTargets(handle) {
			h.buffs.AddBuff(h2, name, argNumber(L, 3), argNumber(L, 4), argNumber(L, 5), 0)
		}
		return 0
	}))
	L.SetGlobal("RemoveBuffToTeam", L.NewFunction(func(L *lua.LState) int {
		handle, ok := buffTarget(L, "RemoveBuffToTeam")
		if !ok {
			return 0
		}
		name := argString(L, 2)
		for _, h2 := range h.buffs.TeamBuffTargets(handle) {
			h.buffs.RemoveBuff(h2, name, false)
		}
		return 0
	}))

	L.SetGlobal("IsBuffApplied", L.NewFunction(func(L *lua.LState) int {
		handle, ok := buffTarget(L, "IsBuffApplied")
		if !ok {
			// NO WORLD: the honest answer is "no", and it is the one the deleted stub
			// gave - but now it is given only when there is no world, not always.
			L.Push(lua.LString("NO"))
			return 1
		}
		if h.buffs.IsBuffApplied(handle, argString(L, 2)) {
			L.Push(lua.LString("YES"))
		} else {
			L.Push(lua.LString("NO"))
		}
		return 1
	}))
	L.SetGlobal("GetAppliedBuffLevel", L.NewFunction(func(L *lua.LState) int {
		handle, ok := buffTarget(L, "GetAppliedBuffLevel")
		if !ok {
			L.Push(lua.LNumber(-1))
			return 1
		}
		// -1 IS THE REFERENCE'S "NOT APPLIED" (`CBuffList::GetAppliedBuffLevel`
		// returns -1, BuffList.cpp:527-548), and the corpus is written against it.
		L.Push(lua.LNumber(h.buffs.AppliedBuffLevel(handle, argString(L, 2))))
		return 1
	}))
}
