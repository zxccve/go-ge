package zone

// THE TACTIC THREAD'S LIFE - the card's four pass conditions (a)-(d).
//
// THE BUG THESE TESTS PIN. `logs\zone_go_script.log` (map 1331, `-monai script`, zone up
// 09:03:36) has four bodies scanning at 09:04:12.368 and ONE at 09:04:13.438 - then not
// one GetObjectLocaly, HATE, TACTICS or ATTACK line for the rest of the session, while the
// swing still landed its 3564 at 09:04:21. The clock was alive; the THREADS had stopped.
//
// The stop was not the runner's: a plain `while(1){ count=count+1; sleep(1000) }` loops
// for as long as it is driven. It was a WAIT the script sits in and we could never satisfy:
// `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` is
//
//	while(1) do if('YES' == IsEnableMove(self)) then return 'YES' end sleep(1000) end
//
// (mon_ai_lib.scp:72-83) and `IsEnableMove` had never been bound, so the missing-builtin
// stub answered 0 - never 'YES'. The NONE loop reaches that helper from its Move block on
// every second it loses `math.random(0,99) > 50` (mon_ts_none.scp:275-296), BEFORE its seek
// at :230, so a body parked there for good with a ~50% chance per second. That is the
// measured decay, and that is why the owner had to stand on top of a body to be hit: only
// `monsterSwingTick`'s own in-reach swing still worked.
//
// THE CORPUS IS THE REAL ONE, as in monster_think_test.go - a green test here that ran no
// script would be worse than none.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/script"
)

// countingSeek stands between the host and the zone and counts the scans, so (a) can assert
// HOW MANY times a body actually asked the field "who is near me" instead of inferring it
// from a log line. It answers everything the zone answers.
type countingSeek struct {
	inner script.MonsterSeek

	scans int
	// tick is the think pass the test is on, and lastScanTick the one the last scan
	// happened in: a count alone cannot tell "scanned forty times and then stopped" from
	// "still scanning at the end".
	tick         int
	lastScanTick int
}

func (c *countingSeek) SeekTarget(mon uint32, rng float64, filter string) (script.HateTarget, bool) {
	c.scans++
	c.lastScanTick = c.tick
	return c.inner.SeekTarget(mon, rng, filter)
}

func (c *countingSeek) ResetHate(mon uint32) { c.inner.ResetHate(mon) }

func (c *countingSeek) EnableMove(mon uint32) bool { return c.inner.EnableMove(mon) }

// tacticThreadLines is every start/end line the runner wrote for `entry`, in order.
func tacticThreadLines(logText, handleHex, entry, what string) []string {
	var out []string
	for _, line := range strings.Split(logText, "\n") {
		if strings.Contains(line, "TACTIC THREAD "+handleHex) &&
			strings.Contains(line, entry+" "+what) {
			out = append(out, line)
		}
	}
	return out
}

// drive runs `n` think passes at the zone's own pollInterval (100 ms), which is the clock
// `tickIfDue` gives the think turn (zone\chase.go:87).
func drive(h *Handler, cs *countingSeek, t0 time.Time, n int) {
	for i := 0; i < n; i++ {
		if cs != nil {
			cs.tick = i
		}
		h.monsterAITick(t0.Add(time.Duration(i) * 100 * time.Millisecond))
	}
}

// TestTacticThreadKeepsScanning is pass condition (a): a body with nobody in its seek ring
// keeps asking - at least ten scans across thirty simulated seconds, and still asking at
// the end - instead of the two the measured run got.
//
// THE PLAYER IS OUT OF THE RING (9000 units, SeekRange 2500), so every scan answers nil and
// the NONE entry stays in its loop: this is exactly the state the owner's four bodies were
// in before he walked up to them.
func TestTacticThreadKeepsScanning(t *testing.T) {
	h, _, scriptLogs, _, mon, host := monaiZone(t, [3]int32{9000, 9000, 29}, true)
	cs := &countingSeek{inner: h}
	host.Seek = cs

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, cs, t0, 300) // 30 s at 100 ms

	if cs.scans < 10 {
		t.Fatalf("the body scanned %d time(s) in 30 s, want at least 10 - the loop "+
			"stopped (thread log:\n%s)", cs.scans, scriptLogs.String())
	}
	if cs.lastScanTick < 250 {
		t.Fatalf("the last scan was in pass %d of 300 (%.1f s of 30 s) - the body "+
			"stopped scanning before the end of the window (thread log:\n%s)",
			cs.lastScanTick, float64(cs.lastScanTick)/10, scriptLogs.String())
	}
	t.Logf("%d scans over 30.0 s, the last in pass %d (%.1f s)", cs.scans,
		cs.lastScanTick, float64(cs.lastScanTick)/10)

	// THE THREAD NEVER ENDED, and it is still the NONE entry.
	if ends := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_NONE", "end"); len(ends) != 0 {
		t.Fatalf("%d TACTIC THREAD end line(s) for a body with nobody in range, want 0:\n%s",
			len(ends), strings.Join(ends, "\n"))
	}
	if entry, running := host.MonsterThreadEntry(mon.Handle); !running ||
		entry != "SCR_MON_AGGRESSIVE_TS_NONE" {
		t.Fatalf("the body's thread is %q (running %v), want SCR_MON_AGGRESSIVE_TS_NONE",
			entry, running)
	}

	// AND THE NAME THE LOOP WAITS ON IS OURS, NOT THE MISSING-BUILTIN STUB.
	for _, m := range host.Missing() {
		if m.Name == "IsEnableMove" {
			t.Fatalf("IsEnableMove is still answered by the missing-builtin stub (%d "+
				"call(s)) - the wait loop in mon_ai_lib.scp:72-83 can never pass", m.Calls)
		}
	}
}

// TestTacticThreadChangeStartsTheNewOneAndEndsTheOldOnce is pass condition (b): the NONE
// entry's own `ChangeTactics('TS_BATTLE')` + `return` (mon_ts_none.scp:236-240) must leave
// ONE ended thread and ONE started one - no orphan resumed forever, no second BATTLE
// thread chasing the same body.
func TestTacticThreadChangeStartsTheNewOneAndEndsTheOldOnce(t *testing.T) {
	h, logs, scriptLogs, _, mon, host := monaiZone(t, [3]int32{0, 150, 29}, true)

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, nil, t0, 3) // NONE acquires -> ChangeTactics -> BATTLE starts

	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the body's tactics state is %q after the seek found the player, want "+
			"TS_BATTLE (script log:\n%s)\nzone log:\n%s", got, scriptLogs.String(), logs.String())
	}

	noneEnds := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_NONE", "end")
	if len(noneEnds) != 1 {
		t.Fatalf("%d end line(s) for the NONE thread, want exactly 1:\n%s",
			len(noneEnds), strings.Join(noneEnds, "\n"))
	}
	battleStarts := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_BATTLE", "start")
	if len(battleStarts) != 1 {
		t.Fatalf("%d start line(s) for the BATTLE thread, want exactly 1:\n%s",
			len(battleStarts), strings.Join(battleStarts, "\n"))
	}
	// THE ONE END AND THE ONE START ARE THE SAME HANDOVER: the NONE thread ended because
	// its script returned into the state change, and the BATTLE thread is the new state's.
	if !strings.Contains(noneEnds[0], "the script ran to its own return") {
		t.Fatalf("the NONE thread's end line does not name its reason:\n%s", noneEnds[0])
	}
	if entry, running := host.MonsterThreadEntry(mon.Handle); !running ||
		entry != "SCR_MON_AGGRESSIVE_TS_BATTLE" {
		t.Fatalf("the body's thread is %q (running %v), want the BATTLE entry - the old "+
			"thread was not replaced", entry, running)
	}

	// A SECOND PASS MUST NOT START A THIRD THREAD.
	drive(h, nil, t0.Add(3*100*time.Millisecond), 2)
	if got := len(tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_BATTLE", "start")); got != 1 {
		t.Fatalf("%d BATTLE start line(s) after two more passes, want 1 - the thread is "+
			"being restarted under itself:\n%s", got, scriptLogs.String())
	}
}

// TestTacticThreadStateChangeEndsAParkedThread is the OTHER half of (b): the old thread is
// not always at its `return` when the tactic changes - a script can call `ChangeTactics` and
// then `sleep`, which leaves a PARKED thread belonging to the old state. The next think pass
// must DROP it (and say so) before starting the new state's thread: no orphan resumed
// forever, no two threads on one body.
func TestTacticThreadStateChangeEndsAParkedThread(t *testing.T) {
	dir := t.TempDir()
	blob := "function SCR_MON_PARK_TS_NONE(self)\n" +
		"\tChangeTactics(self, 'TS_BATTLE')\n" +
		"\tsleep(60000)\n" +
		"end\n" +
		"function SCR_MON_PARK_TS_BATTLE(self)\n" +
		"	sleep(1000)\n" +
		"end\n"
	if err := os.WriteFile(filepath.Join(dir, "mon_park.lua"), []byte(blob), 0o644); err != nil {
		t.Fatalf("write the fixture script: %v", err)
	}
	h, _, scriptLogs, _, mon, host := monaiZoneIn(t, [3]int32{0, 150, 29}, true, dir, "MON_PARK")

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, nil, t0, 2) // pass 0 parks the NONE thread; pass 1 sees the state changed

	ends := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_PARK_TS_NONE", "end")
	if len(ends) != 1 {
		t.Fatalf("%d end line(s) for the PARKED NONE thread, want 1 - it was left as an "+
			"orphan:\n%s", len(ends), scriptLogs.String())
	}
	if !strings.Contains(ends[0], "its tactics state changed to SCR_MON_PARK_TS_BATTLE") {
		t.Fatalf("the parked thread's end line does not name the new state:\n%s", ends[0])
	}
	if got := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_PARK_TS_BATTLE", "start"); len(got) != 1 {
		t.Fatalf("%d start line(s) for the new BATTLE thread, want 1:\n%s",
			len(got), scriptLogs.String())
	}
	if entry, running := host.MonsterThreadEntry(mon.Handle); !running ||
		entry != "SCR_MON_PARK_TS_BATTLE" {
		t.Fatalf("the body's thread is %q (running %v), want SCR_MON_PARK_TS_BATTLE - "+
			"the parked one was not replaced", entry, running)
	}
}

// TestTacticThreadErrorIsLoggedOnce is pass condition (c), and it is the second half of the
// old failure test: a script that raises must report ONCE, name its own file:line, and END
// its thread with that reason - never drop the body's loop without a word.
func TestTacticThreadErrorIsLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	blob := "function SCR_MON_FAIL_TS_NONE(self)\n" +
		"	ChangeTactics(self, 'TS_BATTLE')\n" +
		"	return\n" +
		"end\n" +
		"function SCR_MON_FAIL_TS_BATTLE(self)\n" +
		"	error('boom from the fixture')\n" +
		"end\n"
	if err := os.WriteFile(filepath.Join(dir, "mon_fail.lua"), []byte(blob), 0o644); err != nil {
		t.Fatalf("write the fixture script: %v", err)
	}
	h, _, scriptLogs, _, _, _ := monaiZoneIn(t, [3]int32{0, 150, 29}, true, dir, "MON_FAIL")

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, nil, t0, 5)
	first := scriptLogs.String()

	failed := failedLines(first)
	if len(failed) != 1 {
		t.Fatalf("%d FAILED line(s) after five passes, want 1:\n%s", len(failed), first)
	}
	if !strings.Contains(failed[0], "mon_fail.lua:6") ||
		!strings.Contains(failed[0], "boom from the fixture") {
		t.Fatalf("the FAILED line does not carry the script's own place and error:\n%s",
			failed[0])
	}

	ends := tacticThreadLines(first, "0x20000007", "SCR_MON_FAIL_TS_BATTLE", "end")
	if len(ends) != 1 {
		t.Fatalf("%d end line(s) for the failed BATTLE thread, want 1:\n%s",
			len(ends), strings.Join(ends, "\n"))
	}
	if !strings.Contains(ends[0], "it raised") || !strings.Contains(ends[0], "boom from the fixture") {
		t.Fatalf("the thread's end line does not say it raised, with the error:\n%s", ends[0])
	}

	// A LATER PASS COSTS NOTHING AND ADDS NOTHING - the stop is a fact, not a flood.
	drive(h, nil, t0.Add(time.Second), 2)
	if got := len(failedLines(scriptLogs.String())); got != 1 {
		t.Fatalf("%d FAILED line(s) after two more passes, want 1", got)
	}
	if got := len(tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_FAIL_TS_BATTLE", "end")); got != 1 {
		t.Fatalf("%d end line(s) for the failed thread after two more passes, want 1", got)
	}
}

// TestTacticThreadResumesSeekingWhenTheTargetLeaves is pass condition (d): the BATTLE
// entry's own NULL-target branch (`StopAttack` -> `SetTacticsTarget(self)` ->
// `ChangeTactics('TS_NONE')` -> return, mon_ts_battle.scp:155-159) must hand the body BACK
// to a fresh NONE thread that scans again - not park it.
func TestTacticThreadResumesSeekingWhenTheTargetLeaves(t *testing.T) {
	h, _, scriptLogs, victim, mon, host := monaiZone(t, [3]int32{0, 150, 29}, true)
	cs := &countingSeek{inner: h}
	host.Seek = cs

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, cs, t0, 4) // acquire, BATTLE, attack
	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the body is %q before the target leaves, want TS_BATTLE", got)
	}
	before := cs.scans

	// THE PLAYER LEAVES THE ZONE: the script's loop finds no target on its next pass.
	h.leaveZone(victim.sess)
	drive(h, cs, t0.Add(time.Second), 20)

	if got := h.MonsterTactic(mon.Handle); got != "TS_NONE" {
		t.Fatalf("the body is %q after its target left, want TS_NONE (the BATTLE entry's "+
			"NULL-target branch) - script log:\n%s", got, scriptLogs.String())
	}
	if cs.scans <= before {
		t.Fatalf("the body made %d scan(s) before the target left and %d after - it "+
			"stopped seeking (script log:\n%s)", before, cs.scans-before, scriptLogs.String())
	}
	if entry, running := host.MonsterThreadEntry(mon.Handle); !running ||
		entry != "SCR_MON_AGGRESSIVE_TS_NONE" {
		t.Fatalf("the body's thread is %q (running %v), want a fresh NONE thread",
			entry, running)
	}

	// TWO NONE STARTS: the one it was born with, and the one the lost target bought.
	starts := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_NONE", "start")
	if len(starts) != 2 {
		t.Fatalf("%d NONE start line(s), want 2 (born + after the target left):\n%s",
			len(starts), strings.Join(starts, "\n"))
	}
	// AND THE BATTLE THREAD ENDED ONCE, naming the return.
	battleEnds := tacticThreadLines(scriptLogs.String(), "0x20000007",
		"SCR_MON_AGGRESSIVE_TS_BATTLE", "end")
	if len(battleEnds) != 1 {
		t.Fatalf("%d BATTLE end line(s), want 1:\n%s", len(battleEnds),
			strings.Join(battleEnds, "\n"))
	}
}

// TestEnableMoveAnswersTheOnceAndOnlyMoveState pins the rule this card turned on: a body
// standing still may start a move, a body whose leg is still in flight may not.
//
// THE REFERENCE (MonsterNPCFunc.cpp:515-530) answers 'YES' for MS_STOP/MS_INITIAL and 'NO'
// for any other move state; this engine's move state IS its leg (zone\monster_ai.go).
func TestEnableMoveAnswersTheOnceAndOnlyMoveState(t *testing.T) {
	h, _, _, _, mon, _ := monaiZone(t, [3]int32{9000, 9000, 29}, true)

	if !h.EnableMove(mon.Handle) {
		t.Fatalf("a body with no leg in flight answered 'NO' to IsEnableMove - the " +
			"script's wait loop would park on it forever")
	}

	// ONE LEG IN FLIGHT: the body is walking 500 units at 143, so it may not start another.
	now := time.Now()
	h.spawnMu.Lock()
	st := h.engineLocked(mon.Handle)
	if st == nil {
		h.spawnMu.Unlock()
		t.Fatalf("the body has no engine state")
	}
	ap := st.order.approach
	ap.chase.leg = monsterLeg{
		from: [3]int32{0, 0, 29}, goal: [3]int32{500, 0, 29},
		route: [][3]int32{{0, 0, 29}, {500, 0, 29}},
		began: now, speed: 143, active: true,
	}
	st.order.approach = ap
	h.spawnMu.Unlock()

	if h.EnableMove(mon.Handle) {
		t.Fatalf("a WALKING body answered 'YES' to IsEnableMove - a second walk could be " +
			"started while the first is in flight")
	}

	// THE SAME LEG, WALKED OUT: back to 'YES' without any other edit.
	h.spawnMu.Lock()
	st = h.engineLocked(mon.Handle)
	ap = st.order.approach
	ap.chase.leg.active = false
	st.order.approach = ap
	h.spawnMu.Unlock()
	if !h.EnableMove(mon.Handle) {
		t.Fatalf("a body whose leg has run out still answered 'NO' to IsEnableMove")
	}
}

