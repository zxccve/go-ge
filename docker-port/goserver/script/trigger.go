// TRIGGER THREADS: an NPC's tactics entry is a THREAD, not a call.
//
// WHY THIS FILE EXISTS (R141). The original starts one script thread per trigger
// object when its tactics state machine enters TS_NONE, and that thread then runs
// for as long as the object lives:
//
//	CTriggerObject's FSM, source2\geServer\ZServerLib\TriggerObject.cpp:49-57
//	IMPL_STATE(TS_NONE) ON_ENTER RunTacticsStateThread("TS_NONE")
//
// Most tactics entries return at once (the Scarecrow's manager creates its dummies
// and is done). Some do NOT: the arm managers on the battle maps are
//
//	while(1) do
//	    ... kill the wolves and bears within 500 units ...
//	    sleep(2000)
//	end
//
// (npc\genpc\btl_arm01_manager.lua, SCR_TR_ARM_MON_BLOCK_TS_NONE). Run as a ONE-SHOT
// call with a deadline - which is what RunTacticsState does, and what a click needs -
// such an entry never runs at all AND costs its whole deadline. Measured on map 4063,
// 2026-11-10: eight scripted NPCs, each one a 3 s timeout, 24.1 s of a 33.4 s start,
// and the block rule itself never executed (logs\spawncost_4063c.log).
//
// THE RUNNER IS THE MONSTER ONE (script\monster_ai.go): gopher-lua coroutines, one per
// object, parked on the script's own `sleep` and resumed by a clock this side drives.
// So the semantics are the reference's - a thread that loops lives, a thread that
// returns is spent - and no deadline is applied to a script that is doing its job.
//
// SCOPE, stated plainly:
//   - a trigger thread is woken by the ZONE's tick (zone\chase.go tickIfDue), so it
//     runs while the zone has a session to drive the clock. With nobody in the map
//     nothing ticks; the reference's threads would keep running. That is a limitation
//     of this wiring, not of the model, and it is what R141's follow-up is for.
//   - a thread is cancelled when the zone is torn down with StopTriggers (main.go
//     calls it on shutdown) - "flagged and joined", never killed mid-statement.
package script

import (
	"context"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// triggerThread is one NPC's live tactics entry: the same shape as monsterThread,
// keyed by the object's handle instead of a body's.
type triggerThread struct {
	entry   string
	fn      *lua.LFunction
	co      *lua.LState
	self    *lua.LTable
	handle  uint32
	at      time.Time // the instant of the resume in progress; `sleep` counts from it
	wake    time.Time // when `sleep` lets this thread continue
	started bool      // the entry function has been handed to the coroutine
	cancel  context.CancelFunc
}

// StartTacticsThread starts one actor's tactics entry as a live script thread and
// answers whether the corpus defines it. The FIRST resume happens here, so an entry
// that returns at once (the Scarecrow's manager) has done its work before this call
// returns, exactly as the old one-shot call did; an entry that loops parks on its
// first `sleep` and is woken by TickTriggers.
//
// The entry is looked up in the GLOBALS, raw: a missing entry is the ordinary case
// (most NPCs have no tactics script), so it answers found=false without a line.
func (h *Host) StartTacticsThread(entry string, self Obj) (found bool, err error) {
	globals, ok := h.L.Get(lua.GlobalsIndex).(*lua.LTable)
	if !ok {
		return false, fmt.Errorf("script: the globals table is not a table")
	}
	fn, ok := globals.RawGetString(entry).(*lua.LFunction)
	if !ok {
		return false, nil
	}
	if h.triggers == nil {
		h.triggers = make(map[uint32]*triggerThread)
	}
	if h.triggerStops == nil {
		h.triggerStops = make(map[uint32]monsterStop)
	}

	co, cancel := h.L.NewThread()
	th := &triggerThread{
		entry:  entry,
		fn:     fn,
		co:     co,
		self:   selfTable(h.L, self),
		handle: self.Handle,
		cancel: cancel,
	}
	// A handle that already runs a thread is the same object re-armed (a zone reload
	// of the same map, or a caller repeating StartupTactics): the OLD thread's state
	// belongs to the old entry and is dropped, the way a monster's tactics-state
	// change drops its own (MonsterNPC.cpp:2433-2438).
	if old := h.triggers[th.handle]; old != nil {
		h.dropTriggerThread(th.handle, "the same object started "+entry+
			" while it was running "+old.entry)
	}
	h.triggers[th.handle] = th
	if h.TriggerLog != nil {
		h.TriggerLog(th.handle, entry, "start", "")
	}
	h.resumeTriggerThread(th, time.Now())
	return true, nil
}

// TickTriggers wakes every trigger thread whose `sleep` has expired and resumes it
// for one turn. It is the zone's clock (zone\chase.go tickIfDue -> Handler.TacticTick).
func (h *Host) TickTriggers(now time.Time) {
	for handle, th := range h.triggers {
		if now.Before(th.wake) {
			continue // parked in its own sleep()
		}
		h.resumeTriggerThread(th, now)
		_ = handle
	}
}

// TriggerThreadsArmed says whether any thread is still alive, so a zone with none
// keeps the tick off (the same boolean read monsterAIScript is).
func (h *Host) TriggerThreadsArmed() bool { return h != nil && len(h.triggers) > 0 }

// StopTriggers cancels and joins every trigger thread - the map is going away.
func (h *Host) StopTriggers() int {
	n := 0
	for handle := range h.triggers {
		h.dropTriggerThread(handle, "the map is going away - the thread is cancelled")
		n++
	}
	return n
}

// TriggerThreadCount answers how many threads are alive (0 when the map's scripts
// have all returned) - a test and a diagnosis line see the same number.
func (h *Host) TriggerThreadCount() int {
	if h == nil {
		return 0
	}
	return len(h.triggers)
}

// resumeTriggerThread runs the thread until its next yield or its end.
func (h *Host) resumeTriggerThread(th *triggerThread, now time.Time) {
	th.at, th.wake = now, now
	h.ticks = 0
	h.trigger = th
	ctx, cancel := context.WithTimeout(context.Background(), monsterThinkTimeout)
	th.co.SetContext(ctx)

	// The ENTRY and `self` are handed over on the first resume only; `self` is the
	// SAME table every turn, which is why a value the script stores on it survives.
	var state lua.ResumeState
	var err error
	if !th.started {
		th.started = true
		state, err, _ = h.L.Resume(th.co, th.fn, th.self)
	} else {
		state, err, _ = h.L.Resume(th.co, nil)
	}
	th.co.RemoveContext()
	cancel()
	h.trigger = nil

	if err != nil {
		h.triggerGiveUp(th, err.Error())
		return
	}
	if state == lua.ResumeYield {
		return // parked in sleep(); TickTriggers resumes it when `wake` passes
	}
	// The script ran to its own `return`: the thread is spent, exactly as the
	// reference's DestroyThread after the entry returns.
	h.dropTriggerThread(th.handle, "the script ran to its own return")
}

// triggerGiveUp reports ONE failure for this object and entry, then stops the thread.
// A tactics entry has no state machine to re-enter it (unlike a monster's TS_BATTLE),
// so a broken script is reported once and not retried.
func (h *Host) triggerGiveUp(th *triggerThread, why string) {
	handle := th.handle
	entry := th.entry
	h.dropTriggerThread(handle, "it raised: "+why)
	if stop, was := h.triggerStops[handle]; was && stop.entry == entry {
		return
	}
	h.triggerStops[handle] = monsterStop{entry: entry, why: why}
	if h.TriggerLog != nil {
		h.TriggerLog(handle, entry, "failed", why)
	}
}

// dropTriggerThread destroys one object's thread and writes the ONE end line, with
// `why` (the same accounting as dropMonsterThread).
func (h *Host) dropTriggerThread(handle uint32, why string) {
	th := h.triggers[handle]
	if th == nil {
		return
	}
	if th.cancel != nil {
		th.cancel()
	}
	delete(h.triggers, handle)
	if h.TriggerLog != nil {
		h.TriggerLog(handle, th.entry, "end", why)
	}
}
