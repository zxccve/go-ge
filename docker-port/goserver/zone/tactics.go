// THE NPC TRIGGER THREADS' CLOCK (R141).
//
// A scripted town NPC's tactics entry used to be run ONCE at zone start as a call
// with a 3 s deadline. The reference runs it as a THREAD per object
// (`TriggerObject.cpp:49-57`), and the battle maps' arm managers are `while(1) ...
// sleep(2000)` loops: as a one-shot they never ran, and each one cost the full
// deadline - eight of them made 24.1 s of map 4063's 33.4 s start (2026-11-10).
//
// The thread itself lives in script/trigger.go. This file is the zone end of it:
// the boolean read that keeps the tick awake while a thread is alive, and the call
// that wakes the threads whose own `sleep` has expired. It rides `tickIfDue`
// (zone\chase.go) beside the corpse, chase, swing, sight and monster-AI clocks, so
// there is no second timer and no goroutine of ours.
package zone

import "time"

// tacticThreadsArmed answers whether any NPC trigger thread is still alive. NIL (a
// zone with -no-scripts, or a unit test) is false, so the guard costs one nil check.
func (h *Handler) tacticThreadsArmed() bool {
	if h.TacticArmed == nil {
		return false
	}
	return h.TacticArmed()
}

// tacticTick wakes every trigger thread whose `sleep` has expired. The call takes
// the script host's own mutex (main.go wires it that way) because one Lua runtime
// serves clicks, monsters and triggers alike.
func (h *Handler) tacticTick(now time.Time) {
	if h.TacticTick == nil {
		return
	}
	h.TacticTick(now)
}
