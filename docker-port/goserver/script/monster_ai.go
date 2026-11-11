// THE THINK TURN: the game's own monster tactics script, run as a THREAD.
//
// WHY THIS FILE EXISTS. Every name the battle script calls is bound (stage 1: the hate
// table; stage 2a: the reads and `ChangeTactics`; stage 2b: the five movement/attack
// orders) but nothing has ever RUN the script. The owner's ruling is that it must:
// "whatever the scp is doing it should do by itself, nothing gets coded in native Go".
// So this file is the RUNNER, and it decides nothing about a fight.
//
// THE MODEL IS THE REFERENCE'S IES THREAD, NOT A CALL PER TICK, and that distinction
// is the whole design:
//
//	CMonsterNPC::RunTacticsStateThread(name)            MonsterNPC.cpp:1108-1136
//	    sprintf_s(funcName, "SCR_%s_%s", GetTactics(), name);   // :1116-1117
//	    _strupr_s(funcName);                                     // :1118
//	    m_tacticsStateThread = imcIES::CreateThread(...);        // :1120
//	    m_tacticsStateThread->PushObject(GetIESObject());        // :1129
//	    m_tacticsStateThread->Resume();                          // :1130
//
//	CMonsterNPC::SleepThread(ms)                        MonsterNPC.cpp:2073-2076
//	    => m_tacticsStateThread->SleepThread(ms)
//
// and the state machine starts one on ENTER and destroys it on LEAVE
// (IMPL_STATE(TS_BATTLE) -> RunTacticsStateThread("TS_BATTLE"), MonsterNPC.cpp:2433-2438).
//
// THE PERIOD IS THEREFORE THE SCRIPT'S OWN `sleep`, NOT A TICK THIS SIDE INVENTED:
// `mon_ts_battle.scp:3` sleeps 100 ms once when the thread starts and the main loop
// sleeps 1000 ms at `:276`; the NONE state's loop sleeps 1000 ms at `mon_ts_none.scp:322`.
// A zone tick that CALLED the function every 100 ms would run the script's body thousands
// of times a second and never reach its second line - the script is a coroutine, and
// `sleep` is where it yields. gopher-lua's threads give exactly that: one LState per
// body, resumed when its sleep is up, yielding again at the next one.
//
// `Tactics` IS THE IES COLUMN, NOT THE FSM STATE. `SCR_<Tactics>_<STATE>` is built from
// the body's `Tactics` column (`MON_AGGRESSIVE`, datatable_monster.xml) and the FSM
// state name (`TS_BATTLE`) - which is why a MON_AGGRESSIVE body runs
// `SCR_MON_AGGRESSIVE_TS_BATTLE` (`mon_tactics_normal.scp:29`), and that wrapper calls
// `SCR_MON_AI_LIB_TS_BATTLE` (`mon_tactics_normal.scp:31` -> `mon_ts_battle.scp:1`).
// Both names are in the FAILED line so the card's function is visible through the
// reference's dispatch.
//
// A THREAD RUNS ONCE PER (BODY, ENTRY). The reference starts it on state ENTER and
// nothing restarts it, so a script that RETURNS (or raises) is not re-run until the
// body's tactics STATE changes - `monsterStop` below is that memory, and it is why a
// broken script costs one line and one refused start instead of a line per tick.
//
// WHAT IS DELIBERATELY NOT HERE: no target choice, no distance rule, no tactic, no
// timer of this side's own, and no re-implementation of anything the script does.
// If the script asks for a name we have not bound, the host's own `__on_missing`
// recorder counts it (Host.Missing) and answers 0 - the same contract every other
// script runs under.
package script

import (
	"context"
	"fmt"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// monsterThinkTimeout bounds ONE resume, so a script that loops without sleeping
// cannot hold the zone's tick. It is a runaway guard, not a game timer: the reference
// has the same protection ("Too long script execution time") and `TickBudget` is the
// other half of it.
const monsterThinkTimeout = 2 * time.Second

// MonsterSelf is what `self` answers inside a tactics script - the fields the
// reference's IES object carries that these scripts read.
//
// IT IS REFRESHED ON EVERY RESUME, not copied once when the thread starts: the
// reference's `self` is a live object, and `mon_ts_battle.scp:244` reads `self.HP`
// INSIDE the loop (the flee check). Between two turns the value is the one at the last
// turn, which is exactly the script's own granularity.
//
// The names are the IES properties, spelled as the corpus spells them: `HP`,
// `MON_MHP`, `MSPD`, `MON_MSPD`, `RaceType`, `ClassID`, `Lv`.
type MonsterSelf struct {
	Handle    uint32
	ClassID   uint32
	ClassName string
	Name      string
	Lv        int
	X, Y, Z   int32
	HP        float64
	MHP       float64

	// MSPD is `self.MSPD`, the Monster row's OWN `MSPD` column, and MONMSPD is
	// `self.MON_MSPD`, the row's COMPUTED move speed - the datatable's
	// `CP_MON_MSPD="SCR_Get_MON_MSPD"`, which is
	// `MSPD*(MSPDFix*0.01+1)*(100+MSPD_BM)/100` floored (monster\get_status_monster.lua:
	// 535-547), the same number the 3550 carries.
	//
	// THEY ARE TWO PROPERTIES, NOT ONE NUMBER READ TWICE, and telling them apart is the
	// whole of card t_c873ad60: `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` refuses a body whose
	// `self.MSPD` is 0 BEFORE it asks `IsEnableMove` (mon_ai_lib.scp:74-76), while the
	// NONE entry draws its wander range from `self.MON_MSPD`
	// (`moveMin = self.MON_MSPD*1.5`, `moveMax = self.MON_MSPD*3.0`, mon_ts_none.scp:9-10).
	// A host that answers one number for both hands the script a wander of range 0 (or,
	// worse, refuses the move outright) for every body whose two differ - and hands it 0
	// for both when the only copy it carries is a chase capability.
	MSPD    float64
	MONMSPD float64

	RaceType string
}

// MonsterThink is ONE body's turn: who it is, the entry the reference's dispatch names
// for the state it is in, and the fields `self` answers now.
type MonsterThink struct {
	Handle uint32
	// Entry is `SCR_<Tactics>_<STATE>` - the name RunTacticsStateThread builds
	// (MonsterNPC.cpp:1116-1118).
	Entry string
	Self  MonsterSelf
}

// MonsterAIEntry is the dispatch name for a body whose `Tactics` column is `tactics` and
// whose FSM tactics state is `state` - the zone's copy of the reference's sprintf +
// _strupr_s (MonsterNPC.cpp:1116-1118), kept here beside the runner that consumes it.
//
// "" means the name cannot be built (`Tactics` or the state is empty), and the zone then
// says why instead of running a half-name.
func MonsterAIEntry(tactics, state string) string {
	t, s := strings.TrimSpace(tactics), strings.TrimSpace(state)
	if t == "" || s == "" {
		return ""
	}
	return "SCR_" + strings.ToUpper(t) + "_" + strings.ToUpper(s)
}

// MonsterSeek is the world behind the two names the think turn needs beyond stages 1-2:
// the seek the NONE state acquires a target with (`GetObjectLocaly`) and the list reset
// it begins with (`ResetHate`).
//
// It is a SEPARATE interface on purpose: the stage 1/2 interfaces are the script's
// questions about a fight, and both of these are still questions - one asks the field
// "who is near me", the other tells the body's own list to empty. NIL means "no world
// wired": both then answer their empty value and say so once.
type MonsterSeek interface {
	// SeekTarget answers the nearest live character within `rng` CENTRE units of the
	// body, or false for nobody. `filter` is the script's own third argument
	// (`self.SelectFilter`, e.g. 'PC' or 'MonAndPC'); this zone's field holds players,
	// so both answer a player and 'Monster' answers nobody - see zone\monster_ai.go.
	SeekTarget(mon uint32, rng float64, filter string) (HateTarget, bool)
	// ResetHate empties the body's hate list - `m_hates->RemoveAll()`
	// (MonsterNPC.cpp:132-134).
	ResetHate(mon uint32)
	// EnableMove answers `IsEnableMove(self)`: true while the body may START a move,
	// false while one is already in flight.
	//
	// IT BELONGS TO THE THINK TURN AND NOT TO THE BATTLE SCRIPT'S READS, because the
	// NONE entry does not use it to judge a fight - it WAITS on it, in a loop that has
	// no exit but the answer: `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` is
	//
	//	while(1) do if('YES' == IsEnableMove(self)) then return 'YES' end sleep(1000) end
	//
	// (mon_ai_lib.scp:72-83). An answer outside the game's domain ('YES'/'NO') does not
	// make the AI wrong - it parks the BODY's thread where it can never reach its next
	// seek. That is this file's subject; the measured stop is quoted at enableMove.
	EnableMove(mon uint32) bool
}

// monsterThread is ONE body's running tactics script - the reference's
// `m_tacticsStateThread` (FieldChar.h:404).
type monsterThread struct {
	entry  string
	fn     *lua.LFunction
	co     *lua.LState
	self   *lua.LTable
	at     time.Time // the instant of the resume in progress; `sleep` counts from it
	wake   time.Time // when `sleep` lets this thread continue
	cancel context.CancelFunc

	// started says the entry function has been handed to the coroutine. gopher-lua
	// takes it on the FIRST Resume only (state.go:2158-2172 builds the call frame from
	// it); passing nil on a continuation is correct and passing nil on the first call
	// is a nil dereference.
	started bool
}

// monsterStop is why a (body, entry) has no thread: it ran to its own `return`, or it
// raised. The reference has the same dead end - a thread that returns is not resumed
// and nothing starts a new one until the tactics STATE changes - and keying it by the
// ENTRY is what lets a body go NONE -> BATTLE -> NONE and get a fresh NONE thread.
type monsterStop struct {
	entry string
	why   string
}

// ThinkMonster is the whole runner for one body: start the thread for `t.Entry` if
// there is none, drop a thread whose entry no longer matches (the state's ON_LEAVE),
// and resume it when its own sleep is up.
//
// The caller owns time: `now` is the zone's tick clock, so a test drives a whole
// engagement without sleeping.
func (h *Host) ThinkMonster(t MonsterThink, now time.Time) {
	if h == nil || h.L == nil || t.Handle == 0 || t.Entry == "" {
		return
	}
	if h.monsterThreads == nil {
		h.monsterThreads = map[uint32]*monsterThread{}
	}
	if h.monsterStops == nil {
		h.monsterStops = map[uint32]monsterStop{}
	}

	th := h.monsterThreads[t.Handle]
	if th != nil && th.entry != t.Entry {
		// ON_LEAVE -> ReleaseTacticsStateThread (MonsterNPC.cpp:2433-2438): the
		// script for the OLD state is gone. Its stop mark belonged to that entry
		// and is dropped with it.
		h.dropMonsterThread(t.Handle, "its tactics state changed to "+t.Entry+
			"; the old state's script is released before the new one starts")
		th = nil
	}
	if stop, was := h.monsterStops[t.Handle]; was && stop.entry != t.Entry {
		delete(h.monsterStops, t.Handle)
	}

	if th == nil {
		if stop, was := h.monsterStops[t.Handle]; was && stop.entry == t.Entry {
			return // this entry already ended; only a state CHANGE starts a new one
		}
		h.startMonsterThread(t, now)
		return
	}
	if now.Before(th.wake) {
		return // parked in its own sleep()
	}
	h.resumeMonsterThread(th, t, now)
}

// ForgetMonster drops everything this runner holds for a handle - called when the body
// leaves the field. Handles are never reused, so this is about memory, not identity.
func (h *Host) ForgetMonster(handle uint32) {
	if h == nil {
		return
	}
	h.dropMonsterThread(handle, "the body left the field")
	delete(h.monsterStops, handle)
}

// MonsterThreadEntry answers the entry a handle is running, and false when it is not
// running one - so a test (and a diagnosis line) can see the dispatch without reading
// this file's maps.
func (h *Host) MonsterThreadEntry(handle uint32) (string, bool) {
	if h == nil {
		return "", false
	}
	if th := h.monsterThreads[handle]; th != nil {
		return th.entry, true
	}
	return "", false
}

// startMonsterThread loads the entry, builds `self` and makes the FIRST resume. The
// script runs until its first `sleep` (mon_ts_battle.scp:3) and parks there.
func (h *Host) startMonsterThread(t MonsterThink, now time.Time) {
	path, ok := h.index.Lookup(t.Entry)
	if !ok {
		h.monsterGiveUp(t, "the corpus defines no such entry under "+h.root+
			" - the reference's CreateThread answers NULL and it asserts "+
			"(MonsterNPC.cpp:1120-1124)")
		return
	}
	if !h.loadedFiles[path] {
		if err := h.LoadFile(path); err != nil {
			h.monsterGiveUp(t, err.Error())
			return
		}
	}
	fn, isFn := h.L.GetGlobal(t.Entry).(*lua.LFunction)
	if !isFn {
		h.monsterGiveUp(t, t.Entry+" is not a function after its file loaded")
		return
	}
	co, coCancel := h.L.NewThread()
	th := &monsterThread{
		entry:  t.Entry,
		fn:     fn,
		co:     co,
		self:   monsterSelfTable(h.L, t.Self),
		cancel: coCancel,
	}
	h.monsterThreads[t.Handle] = th
	h.tacticThreadLog(t.Handle, t.Entry, "start", "")
	h.resumeMonsterThread(th, t, now)
}

// resumeMonsterThread runs the thread until its next yield or its end.
func (h *Host) resumeMonsterThread(th *monsterThread, t MonsterThink, now time.Time) {
	// `self` is a live object in the reference, so it is refreshed before every turn.
	refreshMonsterSelf(th.self, t.Self)

	th.at, th.wake = now, now
	h.ticks = 0
	h.think = th
	ctx, cancel := context.WithTimeout(context.Background(), monsterThinkTimeout)
	th.co.SetContext(ctx)

	// The ENTRY and `self` are handed over on the first resume only; a continuation
	// resumes where the yield left off (gopher-lua's Resume reads `fn` only when the
	// thread has not started, state.go:2158-2172). The reference pushes the object once
	// (`PushObject(GetIESObject())`, MonsterNPC.cpp:1129) and it is the SAME table every
	// turn, which is why a value the script stores on `self` survives.
	var state lua.ResumeState
	var err error
	if !th.started {
		th.started = true
		state, err, _ = h.L.Resume(th.co, th.fn, th.self)
	} else {
		state, err, _ = h.L.Resume(th.co, nil)
	}
	th.co.RemoveContext()
	h.think = nil
	cancel()

	if err != nil {
		h.monsterGiveUp(t, err.Error())
		return
	}
	switch state {
	case lua.ResumeYield:
		return // parked in sleep(); ThinkMonster resumes it when `wake` passes
	default:
		// The script ran to its own `return` (mon_ts_battle.scp:159/:193/:203/:210, or
		// the NONE state's acquire-and-return). The thread is spent.
		h.dropMonsterThread(t.Handle, "the script ran to its own return; only a "+
			"tactics state change starts another")
		h.monsterStops[t.Handle] = monsterStop{entry: th.entry,
			why: "the script returned - the reference's thread is spent and only a " +
				"tactics state change starts another (MonsterNPC.cpp:2433-2438)"}
	}
}

// monsterGiveUp reports ONE failure for this body and entry, then refuses to start it
// again until the state changes.
//
// THE LINE NAMES THE SCRIPT'S OWN PLACE. Every corpus file is loaded with its PATH as
// the Lua chunk name (LoadFile), so an error carries `<corpus path>:<line>:` - and the
// translator is line-for-line, so that line is the `.scp` line too.
func (h *Host) monsterGiveUp(t MonsterThink, why string) {
	h.dropMonsterThread(t.Handle, "it raised: "+why)
	if stop, was := h.monsterStops[t.Handle]; was && stop.entry == t.Entry {
		return // this body already said why; a broken script must not flood the log
	}
	h.monsterStops[t.Handle] = monsterStop{entry: t.Entry, why: why}
	h.logf("zone: MONSTER AI 0x%08x %s FAILED and is not retried until its tactics "+
		"state changes: %s", t.Handle, t.Entry, why)
}

// dropMonsterThread destroys a body's thread - DestroyThread + the ref-count release
// (MonsterNPC.cpp:1138-1146) - and writes the ONE end line, with `why`.
//
// A HANDLE WITH NO THREAD SAYS NOTHING. The same function is the give-up path's, and a
// body whose corpus entry does not exist never had a thread to end: its report is the
// FAILED line, not a start/end pair around nothing.
func (h *Host) dropMonsterThread(handle uint32, why string) {
	th := h.monsterThreads[handle]
	if th == nil {
		return
	}
	if th.cancel != nil {
		th.cancel()
	}
	delete(h.monsterThreads, handle)
	h.tacticThreadLog(handle, th.entry, "end", why)
}

// tacticThreadLog writes the ONE line a body's thread life gets: `start` when the entry
// is handed to a coroutine, `end` with the reason when it stops.
//
// IT EXISTS BECAUSE THE STOP WAS INVISIBLE. In the measured run (`logs\zone_go_script.log`,
// map 1331, `-monai script`, 09:03:36) four bodies' threads scanned at 09:04:12.368 and
// ONE scanned again at 09:04:13.438 - then not one GetObjectLocaly, HATE, TACTICS or
// ATTACK line for the rest of the session, while the swing kept landing its 3564 (09:04:21).
// A tick that still swings proves the CLOCK was alive, so the threads had stopped and the
// log said nothing about it. These two lines are that missing sentence.
func (h *Host) tacticThreadLog(handle uint32, entry, what, why string) {
	if what == "start" {
		h.logf("zone: TACTIC THREAD 0x%08x %s start", handle, entry)
		return
	}
	h.logf("zone: TACTIC THREAD 0x%08x %s end (%s)", handle, entry, why)
}

// monsterSleep is the script's own `sleep(ms)` - the IES thread's SleepThread
// (CMonsterNPC::SleepThread, MonsterNPC.cpp:2073-2076), which suspends the thread for
// that many milliseconds.
//
// IT ONLY YIELDS INSIDE A MONSTER THREAD. A click script runs in a coroutine too
// (StartClickCoroutine) and the corpus has NPC scripts that call `sleep`; for those the
// answer must stay what it was before this file existed - the missing-builtin stub's 0,
// no suspension - or a click would be left looking like it was waiting for a dialog.
// `h.think` is set for exactly the length of a monster resume.
func (h *Host) monsterSleep(L *lua.LState) int {
	// A TRIGGER THREAD PARKS THE SAME WAY (script	rigger.go): the arm managers'
	// `while(1) ... sleep(2000)` is the loop R141 was about, and without this branch
	// the loop would spin inside one resume until its deadline.
	if h.trigger != nil && L == h.trigger.co {
		th := h.trigger
		return h.parkSleep(L, th.at, func(wake time.Time) { th.wake = wake })
	}
	if h.think == nil || L == h.L {
		return 0
	}
	th := h.think
	return h.parkSleep(L, th.at, func(wake time.Time) { th.wake = wake })
}

// parkSleep is the body of `sleep(ms)` for ONE thread: read the argument, mark the
// thread's wake, and yield. `setWake` writes the mark onto whichever thread is
// running - a monster's or a trigger's - so both runners share one argument check.
func (h *Host) parkSleep(L *lua.LState, at time.Time, setWake func(time.Time)) int {
	ms, err := numArg(L, 1, "milliseconds")
	if err != nil {
		h.logf("script: sleep: %v - the thread was not parked", err)
		return 0
	}
	if ms < 0 {
		ms = 0
	}
	setWake(at.Add(time.Duration(ms) * time.Millisecond))
	return L.Yield(lua.LNumber(ms))
}

// monsterSelfTable builds the `self` table once per thread. Every name it does not set
// answers 0 through the property fallback, which is what keeps `self.ATK_BM + 1` from
// raising on a property this server does not model.
func monsterSelfTable(L *lua.LState, s MonsterSelf) *lua.LTable {
	t := L.NewTable()
	propertyFallback(L, t)
	refreshMonsterSelf(t, s)
	return t
}

// refreshMonsterSelf writes the live fields onto the SAME table, so a value the script
// stored (`self.DLv_BM = self.DLv_BM + 1`, mon_ts_battle.scp:33) survives the refresh.
func refreshMonsterSelf(t *lua.LTable, s MonsterSelf) {
	t.RawSetString("Handle", lua.LNumber(s.Handle))
	t.RawSetString("ClassID", lua.LNumber(s.ClassID))
	t.RawSetString("ClassName", lua.LString(s.ClassName))
	t.RawSetString("Name", lua.LString(s.Name))
	t.RawSetString("Lv", lua.LNumber(s.Lv))
	t.RawSetString("X", lua.LNumber(s.X))
	t.RawSetString("Y", lua.LNumber(s.Y))
	t.RawSetString("Z", lua.LNumber(s.Z))
	t.RawSetString("HP", lua.LNumber(s.HP))
	t.RawSetString("MHP", lua.LNumber(s.MHP))
	t.RawSetString("MON_MHP", lua.LNumber(s.MHP))
	t.RawSetString("MSPD", lua.LNumber(s.MSPD))
	t.RawSetString("MON_MSPD", lua.LNumber(s.MONMSPD))
	t.RawSetString("RaceType", lua.LString(s.RaceType))
}

// installMonsterAI registers the names the think turn needs that stages 1-2 did not
// bind. Called once, from New().
func (h *Host) installMonsterAI() {
	h.L.SetGlobal("sleep", h.L.NewFunction(h.monsterSleep))
	h.L.SetGlobal("ResetHate", h.L.NewFunction(h.resetHate))
	h.L.SetGlobal("GetObjectLocaly", h.L.NewFunction(h.getObjectLocaly))
	h.L.SetGlobal("IsEnableMove", h.L.NewFunction(h.enableMove))
}

// enableMove is `IsEnableMove(self)` - the reference's MOVE-STATE predicate
// (MonsterNPCFunc.cpp:515-530) and the reason a body's thread used to stop.
//
// THE STOP, MEASURED. `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` (mon_ai_lib.scp:72-83) is not a
// question the script branches on and forgets - it is a WAIT LOOP:
//
//	while(1) do
//	    if('YES' == IsEnableMove(self)) then return 'YES' end
//	    sleep(1000)
//	end
//
// `IsEnableMove` was never bound, so the prelude's missing-builtin stub answered 0, which
// can never equal 'YES'. The NONE loop calls that helper from its Move block
//
//	if (math.random(0,99) > 50) then if('RANDOM' == moveType) then
//	    if('YES' == SCR_MON_AI_LIB_CHECK_ENABLEMOVE(self)) then RandomMove(...)
//
// (mon_ts_none.scp:275-296), so every body had a ~50% chance PER SECOND of parking in that
// loop for the rest of its life, before the seek at `:230`. That is the decay the owner's
// log shows: four bodies scanned at 09:04:12.368, one at 09:04:13.438, none after - and
// the swing still landing at 09:04:21 because the SWING has a clock of its own. No scan →
// no InsertHate → no TS_BATTLE → no StartAttack → no walk: "still frozen, need to be
// extremely close to trigger the attack, and no chase either".
//
// THE ANSWER IS THE REFERENCE'S, not a convenience: 'NO' for an object that is not a
// monster/pet (every handle here is one), 'YES' when the move state is MS_STOP or
// MS_INITIAL, 'NO' for any other state - i.e. a body standing still may start a move, a
// body already walking may not, and the script's own loop then waits a second and asks
// again. That deferral is the point: a second walk while the first is in flight would
// steer one body twice.
func (h *Host) enableMove(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "IsEnableMove", "the body (self)")
	if !ok {
		L.Push(lua.LString("NO"))
		return 1
	}
	if h.Seek == nil {
		h.reportOnce("seek:no-world",
			"script: IsEnableMove answered 'NO' - this host has no seek world wired, so "+
				"no body's move state is known (further calls are not logged). A tactics "+
				"thread that WAITS on this name (mon_ai_lib.scp:72-83) will never pass")
		L.Push(lua.LString("NO"))
		return 1
	}
	if h.Seek.EnableMove(mon) {
		L.Push(lua.LString("YES"))
		return 1
	}
	L.Push(lua.LString("NO"))
	return 1
}

// resetHate is `ResetHate(self)` - mon_ts_none.scp:33, the list reset a body does when
// it enters its idle state (CMonsterNPC::OnResetHates -> `m_hates->RemoveAll()`,
// MonsterNPC.cpp:132-134). It clears the LIST only; the tactics target is another field
// in the reference and the script clears that itself (`SetTacticsTarget(self)`).
func (h *Host) resetHate(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "ResetHate", "the body (self)")
	if !ok {
		return 0
	}
	if h.Seek == nil {
		h.reportOnce("seek:no-world",
			"script: ResetHate answered nothing - this host has no seek world wired, so "+
				"no body's hate list can be emptied (further calls are not logged)")
		return 0
	}
	h.Seek.ResetHate(mon)
	return 0
}

// getObjectLocaly is `GetObjectLocaly(self, range [, type [, name]])` -
// mon_ts_none.scp:230-235, the seek that ACQUIRES a target.
//
// THE REFERENCE'S SHAPE, read before this was written (SystemFunc.cpp:270-330): the
// body, a radius, an object-type name and an optional ClassName; the world visits a
// circle of `range` around the body with `CEnemySelector` (geVisitor.h:33-84) and the
// FIRST visitor that is a live, valid enemy of the body is answered as an OBJECT, or
// NULL when the circle holds none. The visit ORDER is the sector grid's, not a rule, so
// this zone answers the NEAREST (zone\monster_ai.go says so in one line); the answers
// agree whenever one player is in range, which is the case the script acts on.
//
// `type` is optional here although the reference reads it as a StrID: the corpus calls
// the three-argument form and the four-argument form, and a missing argument resolves an
// empty filter, which is `OLT_ALL` there.
func (h *Host) getObjectLocaly(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "GetObjectLocaly", "the body (self)")
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	rng, rerr := numArg(L, 2, "range")
	if rerr != nil {
		h.logf("script: GetObjectLocaly: %v - nil was answered (a circle has no radius "+
			"when the number is missing)", rerr)
		L.Push(lua.LNil)
		return 1
	}
	filter, _ := strArg(L, 3, "type") // a missing third argument is the empty filter
	h.reportScan(mon, rng, filter)
	if h.Seek == nil {
		h.reportOnce("seek:no-world",
			"script: GetObjectLocaly answered nil - this host has no seek world wired, so "+
				"no body can find a target (further calls are not logged)")
		L.Push(lua.LNil)
		return 1
	}
	target, found := h.Seek.SeekTarget(mon, rng, filter)
	if !found {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(hateObject(L, target))
	return 1
}

// reportScan says the NONE loop STARTED - once per body, and once per distinct
// question it asks.
//
// `GetObjectLocaly` is not a rare call: every body's tactics script asks it inside
// its `sleep(1000)` loop (mon_ts_none.scp:230), so on a populated map the line was
// written hundreds of times a minute. On `btl_auch01` (185 spawner-placed bodies,
// 2026-11-08) `logs\zone_btlauch01.log` gained 996 lines in the 23:23 minute and 689
// in 23:24, and the owner's real test lines - a heal, a refused command, a kill -
// drowned in them. What a reader needs from the line is that a body's loop STARTED,
// and the one occasion its question changed; the repeat says nothing. So the key is
// the body AND the whole question (radius, filter): a body's first scan prints, its
// thousandth does not, and a different radius or filter prints once more.
//
// Keyed by the handle, which this host never reuses (host.go, monsterThreads). The
// gate belongs to this diagnostic alone: refused calls and argument errors are
// reported by their own logf/reportOnce lines and are not suppressed by it.
func (h *Host) reportScan(mon uint32, rng float64, filter string) {
	h.reportOnce(fmt.Sprintf("scan:%s:%g:%q", hexMonster(mon), rng, filter),
		"zone: MONSTER AI DEBUG GetObjectLocaly(mon=%d, rng=%g, filter=%q)", mon, rng, filter)
}
