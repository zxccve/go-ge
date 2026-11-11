package zone

import (
	"testing"
	"time"
)

// THE WORKER'S OWN TESTS (2026-11-02, movement piece 2f).
//
// Before this round every question to the game's path helper was a PROCESS: spawn
// pathtool.exe, load the .tok mesh, generate the preprocess, ask, exit. A zone now plans
// a route for every client move report (move.go, clientRoute), and that plan sits in
// front of the echo packet the client is waiting for - so the process start was on the
// press path. Measured on this machine, fld_auch.tok: twenty `path` commands through
// twenty processes took 1.015 s; one process fed the same twenty took 0.053 s.
//
// These tests hold the three things that make the worker safe to rely on:
//
//  1. TWENTY COMMANDS, ONE PROCESS - the whole point, and a wall-clock bound on it.
//  2. A DEAD HELPER IS REPLACED - a crash costs one answer, not the zone.
//  3. NO ROUTE IS AN ANSWER - a bad point comes back quickly, and the SAME process is
//     still usable afterwards (the preprocess must not be re-generated, and nothing may
//     be left waiting on a line that will never come).
//
// All of them skip on a machine without pathtool.exe (the Linux target has none).

// workerPoints are two real, connected points on the map the zone serves: the field
// spawn and the NPC the chase tests walk to (nav_test.go). They are used together so a
// route between them is a real route, with a length.
var (
	workerFrom = [3]int32{5779, 303, 149}
	workerTo   = [3]int32{7566, 1425, 165}
)

// TestPathEngineWorkerAnswersTwentyCommandsInOneProcess is the measurement this round
// exists for: twenty route/height commands through ONE PathEngine, timed.
func TestPathEngineWorkerAnswersTwentyCommandsInOneProcess(t *testing.T) {
	engine := realEngine(t)

	// What ONE process start costs here, measured the same way the old engine did it:
	// a fresh engine, one command, gone. This is the number every leg used to pay.
	one := realEngine(t)
	t0 := time.Now()
	if _, _, err := one.PathLength(workerFrom, workerTo); err != nil {
		t.Fatalf("the first command on a fresh engine failed: %v", err)
	}
	spawn := time.Since(t0)
	one.Close()

	const n = 20
	t0 = time.Now()
	var load time.Duration // the FIRST command only: it pays the mesh load and preprocess
	for i := 0; i < n; i++ {
		if i == 1 {
			load = time.Since(t0)
		}
		if i%2 == 0 {
			length, ok, err := engine.PathLength(workerFrom, workerTo)
			if err != nil {
				t.Fatalf("route %d of %d: %v", i+1, n, err)
			}
			if !ok || length < 100 {
				t.Fatalf("route %d of %d answered ok=%v length=%.0f, want a real route",
					i+1, n, ok, length)
			}
			continue
		}
		height, ok, err := engine.Height(workerFrom)
		if err != nil {
			t.Fatalf("height %d of %d: %v", i+1, n, err)
		}
		if !ok || height == 0 {
			t.Fatalf("height %d of %d answered ok=%v height=%d, want the mesh's height",
				i+1, n, ok, height)
		}
	}
	total := time.Since(t0)

	t.Logf("%d commands through ONE pathtool process: %v total (%v each). "+
		"One process start on this machine measures %v, so %d starts would be ~%v - "+
		"the worker is %.0fx cheaper per command.",
		n, total.Round(time.Millisecond), (total / n).Round(10*time.Microsecond),
		spawn.Round(time.Millisecond), n, (spawn * n).Round(10*time.Millisecond),
		float64(spawn)/(float64(total)/n))
	// WHAT A LIVE MOVE REPORT PAYS: the first command is the mesh load, so the 19 after
	// it are the honest per-leg cost - the route planned inside one CZ_MOVE_PC.
	t.Logf("of that, the first command (mesh load) is %v and the other %d average %v each",
		load.Round(time.Millisecond), n-1, ((total - load) / time.Duration(n-1)).Round(10*time.Microsecond))

	// The bound this round was written to: well under half a second, where the old
	// engine spent ~2 s on the same twenty commands (20 x ~100 ms).
	if total > 500*time.Millisecond {
		t.Fatalf("%d commands took %v - the helper is not being reused (a process start "+
			"is %v on this machine)", n, total.Round(time.Millisecond),
			spawn.Round(time.Millisecond))
	}
	// AND IT MUST BEAT THE PROCESS START IT REPLACED, by a wide margin. A machine so
	// slow that the two are close would mean the reuse is not happening at all.
	if total > n*spawn/4 {
		t.Fatalf("%d commands took %v, which is not far below %d process starts (%v)",
			n, total.Round(time.Millisecond), n, (n * spawn).Round(time.Millisecond))
	}
}

// TestPathEngineWorkerIsReplacedAfterItDies: the helper is killed behind the engine's
// back, the way a crash would. The call that finds it dead answers with the failure it
// already had ("no route" to every caller), the process is retired, and the NEXT call
// starts a fresh one and answers normally.
func TestPathEngineWorkerIsReplacedAfterItDies(t *testing.T) {
	engine := realEngine(t)

	if _, ok, err := engine.Height(workerFrom); err != nil || !ok {
		t.Fatalf("the engine did not answer before it was killed: ok=%v err=%v", ok, err)
	}

	engine.mu.Lock()
	w := engine.proc
	pid := 0
	if w != nil && w.cmd.Process != nil {
		pid = w.cmd.Process.Pid
	}
	engine.mu.Unlock()
	if pid == 0 {
		t.Fatal("no helper process is running after one command - nothing was started")
	}

	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatalf("could not kill the helper (pid %d): %v", pid, err)
	}
	// Wait for the READER to notice, so "the helper is dead" is a fact when the next
	// command is written - not a race this test could lose.
	select {
	case <-w.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the dead helper (pid %d) was never noticed by its reader", pid)
	}

	// The call that finds it dead: an error, which every caller already reads as "no
	// route" / "no height". It may be a write to a closed pipe or the end of stdout;
	// which one is not this test's business.
	if _, _, err := engine.Height(workerFrom); err == nil {
		t.Fatal("a command sent to a killed helper answered as if nothing had happened")
	}

	// ... AND THE NEXT ONE WORKS, on a NEW process.
	height, ok, err := engine.Height(workerFrom)
	if err != nil {
		t.Fatalf("the engine did not restart after its helper was killed: %v", err)
	}
	if !ok || height == 0 {
		t.Fatalf("the restarted engine answered ok=%v height=%d", ok, height)
	}
	engine.mu.Lock()
	newPID := 0
	if engine.proc != nil && engine.proc.cmd.Process != nil {
		newPID = engine.proc.cmd.Process.Pid
	}
	engine.mu.Unlock()
	if newPID == 0 {
		t.Fatal("the restarted engine has no helper process")
	}
	if newPID == pid {
		t.Fatalf("the engine is still using the dead helper (pid %d)", pid)
	}
	t.Logf("killed pid %d, replaced by pid %d, which answered height %d", pid, newPID, height)
}

// TestPathEngineNoRouteIsAnAnswerNotAHang: a point off the mesh is a legitimate "no
// route" (pathtool's own `path none`), and it must come back quickly - and leave the
// SAME process able to answer the next question. A worker that read one line too few, or
// one too many, would hang here instead.
func TestPathEngineNoRouteIsAnAnswerNotAHang(t *testing.T) {
	engine := realEngine(t)

	// A route first: the process is warm, exactly as it is in a live zone.
	if _, ok, err := engine.PathLength(workerFrom, workerTo); err != nil || !ok {
		t.Fatalf("the warm-up route failed: ok=%v err=%v", ok, err)
	}

	t0 := time.Now()
	route, ok := engine.Route(workerFrom, [3]int32{900000, 900000, 1})
	took := time.Since(t0)
	if ok {
		t.Fatalf("a point far off the mesh answered a route: %v", route)
	}
	if took > time.Second {
		t.Fatalf("a point off the mesh took %v to answer - the helper was waited on, "+
			"not answered", took)
	}

	// THE SAME PROCESS MUST STILL ANSWER. If `path none` left a line unread, the next
	// command would read the wrong answer; if the engine retired the process for it, the
	// route below would still work but the pid would change. Both are checked.
	engine.mu.Lock()
	pid := 0
	if engine.proc != nil && engine.proc.cmd.Process != nil {
		pid = engine.proc.cmd.Process.Pid
	}
	engine.mu.Unlock()

	length, ok, err := engine.PathLength(workerFrom, workerTo)
	if err != nil {
		t.Fatalf("the engine broke after answering `path none`: %v", err)
	}
	if !ok || length < 100 {
		t.Fatalf("after `path none` the engine answered ok=%v length=%.0f, want a route",
			ok, length)
	}
	engine.mu.Lock()
	samePID := engine.proc != nil && engine.proc.cmd.Process != nil &&
		engine.proc.cmd.Process.Pid == pid
	engine.mu.Unlock()
	if pid != 0 && !samePID {
		t.Fatal("`path none` replaced the helper - a legitimate answer must not restart it")
	}
	t.Logf("no route answered in %v, and the same helper (pid %d) then planned %.0f u",
		took.Round(10*time.Microsecond), pid, length)
}

// TestPathEngineCloseStopsTheHelperAndTheEngineStillWorks: a zone closes its engine on
// the way out (main.go), and a closed engine must leave NO process behind - while still
// being usable if something asks again, which is how a test proves the close happened.
func TestPathEngineCloseStopsTheHelperAndTheEngineStillWorks(t *testing.T) {
	engine := realEngine(t)

	if _, ok, err := engine.Height(workerFrom); err != nil || !ok {
		t.Fatalf("the engine did not answer before it was closed: ok=%v err=%v", ok, err)
	}
	engine.mu.Lock()
	first, pid := engine.proc, 0
	if first != nil && first.cmd.Process != nil {
		pid = first.cmd.Process.Pid
	}
	engine.mu.Unlock()
	if pid == 0 {
		t.Fatal("no helper process is running after one command")
	}

	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if first.cmd.ProcessState == nil {
		t.Fatalf("Close returned without reaping the helper (pid %d) - that is a zombie",
			pid)
	}
	if !first.cmd.ProcessState.Exited() {
		t.Fatalf("the helper (pid %d) is still running after Close", pid)
	}

	// Usable again: a new process, a real answer.
	height, ok, err := engine.Height(workerFrom)
	if err != nil || !ok || height == 0 {
		t.Fatalf("a closed engine could not answer again: ok=%v height=%d err=%v",
			ok, height, err)
	}
	engine.mu.Lock()
	second := 0
	if engine.proc != nil && engine.proc.cmd.Process != nil {
		second = engine.proc.cmd.Process.Pid
	}
	engine.mu.Unlock()
	if second == 0 || second == pid {
		t.Fatalf("no new helper after Close (old pid %d, now %d)", pid, second)
	}
	t.Logf("closed pid %d (exited), reopened as pid %d", pid, second)
}
