package script

import (
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// TestATacticsEntryThatLoopsStartsInMilliseconds is R141's RED PROOF. Before
// script\trigger.go, an NPC tactics entry was a one-shot call with a 3 s deadline, so
// the arm managers' `while(1) ... sleep(2000)` (btl_arm01_manager.lua,
// SCR_TR_ARM_MON_BLOCK_TS_NONE) blocked the caller for the full deadline and then
// failed - eight of them were 24.1 s of map 4063's 33.4 s start. A thread starts, runs
// its first turn, and parks on its own sleep.
func TestATacticsEntryThatLoopsStartsInMilliseconds(t *testing.T) {
	h := newTestHost(t, `
runs = 0
function SCR_TEST_LOOP_TS_NONE(self)
  while(1) do
    runs = runs + 1
    sleep(2000)
  end
end`)

	start := time.Now()
	found, err := h.StartTacticsThread("SCR_TEST_LOOP_TS_NONE", Obj{Handle: 0x11})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("StartTacticsThread: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true: the entry is defined")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("the entry took %s to start; a loop must PARK on its first sleep, "+
			"not run until a deadline", elapsed)
	}
	if got := h.TriggerThreadCount(); got != 1 {
		t.Fatalf("threads = %d, want 1 (the loop is alive, parked)", got)
	}
	if got := runs(h); got != 1 {
		t.Fatalf("the loop ran %d time(s) before parking, want 1", got)
	}
}

// TestAParkedTriggerWakesOnItsOwnSleep pins the period: the script's own `sleep(2000)`
// is what decides when the next turn happens - not the tick, and not a constant here.
func TestAParkedTriggerWakesOnItsOwnSleep(t *testing.T) {
	h := newTestHost(t, `
runs = 0
function SCR_TEST_WAKE_TS_NONE(self)
  while(1) do
    runs = runs + 1
    sleep(2000)
  end
end`)
	if _, err := h.StartTacticsThread("SCR_TEST_WAKE_TS_NONE", Obj{Handle: 0x12}); err != nil {
		t.Fatalf("StartTacticsThread: %v", err)
	}

	h.TickTriggers(time.Now().Add(500 * time.Millisecond))
	if got := runs(h); got != 1 {
		t.Fatalf("ran %d time(s) 0.5 s into a 2 s sleep, want 1 (it must stay parked)", got)
	}
	h.TickTriggers(time.Now().Add(3 * time.Second))
	if got := runs(h); got != 2 {
		t.Fatalf("ran %d time(s) after the sleep expired, want 2", got)
	}
}

// TestATriggerThatReturnsIsSpent keeps the other half of the reference's semantics:
// most entries (the Scarecrow's manager) run once and end, and must not be restarted
// by the clock.
func TestATriggerThatReturnsIsSpent(t *testing.T) {
	h := newTestHost(t, `
runs = 0
function SCR_TEST_ONCE_TS_NONE(self) runs = runs + 1 end`)
	if _, err := h.StartTacticsThread("SCR_TEST_ONCE_TS_NONE", Obj{Handle: 0x13}); err != nil {
		t.Fatalf("StartTacticsThread: %v", err)
	}
	if got := h.TriggerThreadCount(); got != 0 {
		t.Fatalf("threads = %d, want 0: the script returned, so the thread is spent", got)
	}
	h.TickTriggers(time.Now().Add(time.Minute))
	if got := runs(h); got != 1 {
		t.Fatalf("ran %d time(s), want 1: a spent entry must not be restarted", got)
	}
}

// TestAMissingTacticsEntryIsNotAThread is the ordinary case - most NPCs have no
// tactics script - and must stay silent and harmless.
func TestAMissingTacticsEntryIsNotAThread(t *testing.T) {
	h := newTestHost(t, "")
	found, err := h.StartTacticsThread("SCR_TEST_NOTHING_TS_NONE", Obj{Handle: 0x14})
	if err != nil {
		t.Fatalf("a missing entry is not an error: %v", err)
	}
	if found {
		t.Fatal("found = true for an entry the corpus does not define")
	}
	if got := h.TriggerThreadCount(); got != 0 {
		t.Fatalf("threads = %d, want 0", got)
	}
}

// TestStopTriggersCancelsEveryLoop is the unload path: flagged and joined, one end
// line per thread, nothing left alive.
func TestStopTriggersCancelsEveryLoop(t *testing.T) {
	h := newTestHost(t, `
function SCR_TEST_A_TS_NONE(self) while(1) do sleep(1000) end end
function SCR_TEST_B_TS_NONE(self) while(1) do sleep(1000) end end
function SCR_TEST_C_TS_NONE(self) while(1) do sleep(1000) end end`)
	var ends []string
	h.TriggerLog = func(handle uint32, entry, what, why string) {
		if what == "end" {
			ends = append(ends, entry+"|"+why)
		}
	}
	for i, entry := range []string{"SCR_TEST_A_TS_NONE", "SCR_TEST_B_TS_NONE", "SCR_TEST_C_TS_NONE"} {
		if _, err := h.StartTacticsThread(entry, Obj{Handle: uint32(0x20 + i)}); err != nil {
			t.Fatalf("StartTacticsThread(%s): %v", entry, err)
		}
	}
	if got := h.TriggerThreadCount(); got != 3 {
		t.Fatalf("threads = %d, want 3", got)
	}
	if n := h.StopTriggers(); n != 3 {
		t.Fatalf("StopTriggers = %d, want 3", n)
	}
	if got := h.TriggerThreadCount(); got != 0 {
		t.Fatalf("threads after StopTriggers = %d, want 0", got)
	}
	if len(ends) != 3 {
		t.Fatalf("end lines = %d, want 3 (%v)", len(ends), ends)
	}
	for _, line := range ends {
		if !strings.Contains(line, "the map is going away") {
			t.Fatalf("end line %q does not say why the thread ended", line)
		}
	}
}

// TestATriggerThatRaisesIsReportedOnce kills a thread that errors: one failed line,
// the thread gone, and no retry on the next tick (there is no tactics state to
// re-enter an NPC's entry, unlike a monster's).
func TestATriggerThatRaisesIsReportedOnce(t *testing.T) {
	h := newTestHost(t, `
function SCR_TEST_BOOM_TS_NONE(self) error("boom") end`)
	var failures []string
	h.TriggerLog = func(handle uint32, entry, what, why string) {
		if what == "failed" {
			failures = append(failures, entry+"|"+why)
		}
	}
	if _, err := h.StartTacticsThread("SCR_TEST_BOOM_TS_NONE", Obj{Handle: 0x31}); err != nil {
		t.Fatalf("StartTacticsThread: %v", err)
	}
	if got := h.TriggerThreadCount(); got != 0 {
		t.Fatalf("threads = %d, want 0 after a raise", got)
	}
	if len(failures) != 1 {
		t.Fatalf("failed lines = %d, want 1 (%v)", len(failures), failures)
	}
	h.TickTriggers(time.Now().Add(time.Minute))
	if len(failures) != 1 {
		t.Fatalf("a broken entry was retried: %d failed lines", len(failures))
	}
}

// TestTheOneShotPathAbortsALoopAtItsDeadline is the RED HALF of R141, kept so the
// reason the thread model exists cannot be forgotten: the SAME entry, run the way the
// zone ran it before (one call, a deadline), does no work and costs the whole budget.
// On map 4063 eight such entries were 8 x 3 s = 24.1 s of a 33.4 s start, and the
// block rule never executed (logs\spawncost_4063c.log).
func TestTheOneShotPathAbortsALoopAtItsDeadline(t *testing.T) {
	h := newTestHost(t, `
runs = 0
function SCR_TEST_LOOP_TS_NONE(self)
  while(1) do
    runs = runs + 1
    sleep(50)
  end
end`)

	start := time.Now()
	found, err := h.RunTacticsState("SCR_TEST_LOOP_TS_NONE", Obj{Handle: 0x41}, 100*time.Millisecond)
	elapsed := time.Since(start)

	if !found {
		t.Fatal("found = false, want true")
	}
	if err == nil {
		t.Fatal("the one-shot call ended without an error: a while(1) loop never returns")
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("error = %v, want the context deadline the old path hit", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Fatalf("the call gave up after %s, want the whole budget", elapsed)
	}
}

// runs reads the test scripts' counter through the global table.
func runs(h *Host) int {
	if n, ok := h.L.GetGlobal("runs").(lua.LNumber); ok {
		return int(n)
	}
	return -1
}
