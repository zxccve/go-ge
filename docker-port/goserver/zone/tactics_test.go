// THE TRIGGER THREADS RIDE THE SESSION'S OWN CLOCK (R141).
//
// script\trigger.go proves the thread model in isolation; this file proves the ZONE
// half: a session's tick runs because a trigger thread is alive (so an arm manager's
// `sleep(2000)` loop keeps being woken even when the player stands still), and a zone
// with no live thread does not wake its clock for nothing.
package zone

import (
	"testing"
	"time"
)

// TestASessionTickRunsForALiveTriggerThread is the wiring the 24 s bug needed: with
// nothing else pending, the guard must still let the tick through and the call must
// reach TacticTick.
func TestASessionTickRunsForALiveTriggerThread(t *testing.T) {
	h, sess := testHandler(nil)
	ticks := 0
	h.TacticArmed = func() bool { return true }
	h.TacticTick = func(now time.Time) { ticks++ }

	if _, ran := h.tickIfDue(sess); !ran {
		t.Fatal("the tick did not run: a live trigger thread is a reason of its own")
	}
	if ticks != 1 {
		t.Fatalf("TacticTick called %d time(s), want 1", ticks)
	}
}

// TestNoTriggerClockWithoutALiveThread keeps the cost honest: every ordinary session
// poll must not reach the script host when the map's entries have all returned.
func TestNoTriggerClockWithoutALiveThread(t *testing.T) {
	h, sess := testHandler(nil)
	h.TacticArmed = func() bool { return false }
	h.TacticTick = func(now time.Time) {
		t.Fatal("TacticTick ran with no thread alive")
	}
	if _, ran := h.tickIfDue(sess); ran {
		t.Fatal("the tick ran for a zone with no work at all")
	}
}
