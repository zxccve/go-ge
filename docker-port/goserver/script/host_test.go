// Test the Go script host against the REAL corpus.
//
// The corpus is generated, git-ignored and not present on every machine, so the
// tests SKIP rather than fail when it is missing - the instruction in the skip
// message is the way to build it:
//
//	cd src && python scp_to_lua.py --dump ..\out\lua_corpus
//
// `-short` skips the two full-corpus runs: they take a few seconds and the fast
// tests are enough to catch a broken host.
package script

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// corpusCandidates is the corpus location, in the order to try it.
//
// It cannot be ONE constant: a test runs with its working directory inside
// goserver\script, while the server binary runs from goserver\ (where
// DefaultCorpus is correct). Rather than make the caller pass a path, the test
// tries the shapes the corpus actually lives in.
func corpusCandidates() []string {
	if env := os.Getenv("GE_LUA_CORPUS"); env != "" {
		return []string{env}
	}
	return []string{DefaultCorpus, "../../out/lua_corpus", "../../../out/lua_corpus"}
}

// corpusDir returns the corpus folder, or skips the test with the build recipe.
func corpusDir(t *testing.T) string {
	t.Helper()
	for _, dir := range corpusCandidates() {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	t.Skipf("no translated corpus (tried %s) - build it with: "+
		"cd src && python scp_to_lua.py --dump ..\\out\\lua_corpus",
		strings.Join(corpusCandidates(), ", "))
	return ""
}

// TestPreludeRestoresLua50Names is the cheap test that runs everywhere: the
// corpus was written for Lua 5.0, so the names 5.1 dropped must exist BEFORE a
// script is loaded, or every script using them fails at load time.
func TestPreludeRestoresLua50Names(t *testing.T) {
	h := New()
	defer h.Close()

	for _, probe := range []struct{ expr, want string }{
		{`return tostring(table.getn ~= nil)`, "true"},
		{`return tostring(table.foreach ~= nil)`, "true"},
		{`return tostring(unpack ~= nil)`, "true"},
		{`return tostring(string.gfind ~= nil)`, "true"},
		{`return tostring(math.mod ~= nil)`, "true"},
		{`return tostring(#("abc"))`, "3"},
	} {
		if err := h.L.DoString(probe.expr); err != nil {
			t.Fatalf("%s: %v", probe.expr, err)
		}
		got := h.L.Get(-1).String()
		h.L.Pop(1)
		if got != probe.want {
			t.Fatalf("%s = %s, want %s", probe.expr, got, probe.want)
		}
	}
}

// TestUnknownBuiltinAnswersZeroAndIsRecorded pins the contract the whole design
// rests on: a name we have not written must not raise, and must be counted.
func TestUnknownBuiltinAnswersZeroAndIsRecorded(t *testing.T) {
	h := New()
	defer h.Close()

	if err := h.L.DoString(`
		function SCR_CLICK__PROBE()
			local a = SomeBuiltinWeNeverWrote(1, "two")
			local b = AnotherOne()
			return a + b
		end`); err != nil {
		t.Fatalf("define: %v", err)
	}
	if err := h.Call("SCR_CLICK__PROBE", 2*time.Second); err != nil {
		t.Fatalf("an unimplemented builtin must not raise: %v", err)
	}

	missing := h.Missing()
	if len(missing) != 2 {
		t.Fatalf("recorded %d missing builtin(s), want 2: %+v", len(missing), missing)
	}
	for _, m := range missing {
		if m.Calls != 1 {
			t.Fatalf("%s counted %d time(s), want 1", m.Name, m.Calls)
		}
	}
}

// TestRunawayLoopIsStopped proves the loop guard: a `while(1)` must raise
// instead of hanging the goroutine, because 662 corpus files contain one.
func TestRunawayLoopIsStopped(t *testing.T) {
	h := New()
	defer h.Close()
	h.TickBudget = 1000 // keep the test instant; the real budget is 5,000,000

	if err := h.L.DoString(`
		function SCR_CLICK__SPIN()
			while (1) do _vtick() end
		end`); err != nil {
		t.Fatalf("define: %v", err)
	}
	err := h.Call("SCR_CLICK__SPIN", 5*time.Second)
	if err == nil {
		t.Fatal("an endless loop returned; the tick guard did not fire")
	}
	if !strings.Contains(err.Error(), "ran too long") {
		t.Fatalf("error was %v, want the tick-guard message", err)
	}
}

// TestCorpusLoads is the gate: every translated script must load into ONE
// runtime. Measured 2026-10-25: 4,238 / 4,238.
func TestCorpusLoads(t *testing.T) {
	if testing.Short() {
		t.Skip("full corpus load is skipped with -short")
	}
	dir := corpusDir(t)

	h := New()
	defer h.Close()

	// Memory BEFORE the corpus, so the corpus cost is a delta and not a guess:
	// an empty gopher-lua state is not free either.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	loaded, err := h.LoadCorpus(dir)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	_, failed, _ := h.Stats()
	if len(failed) > 0 {
		t.Fatalf("%d script(s) failed to load, first: %s", len(failed), failed[0])
	}
	if loaded < 4000 {
		t.Fatalf("loaded %d scripts, expected the whole corpus (~4,238)", loaded)
	}

	clicks := h.Names("SCR_CLICK_")
	if len(clicks) < 4000 {
		t.Fatalf("%d SCR_CLICK_ functions, expected ~4,300", len(clicks))
	}
	// What one loaded runtime costs, so "how many can we run?" is a number and
	// not a guess. GC first: HeapAlloc before a collection counts parse garbage,
	// which would make one runtime look far more expensive than it is.
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	corpusMB := float64(mem.HeapAlloc-before.HeapAlloc) / (1 << 20)
	t.Logf("loaded %d file(s), %d click script(s); heap %.1f MB total, "+
		"%.1f MB of it the corpus, %.1f MB the empty runtime",
		loaded, len(clicks), float64(mem.HeapAlloc)/(1<<20), corpusMB,
		float64(before.HeapAlloc)/(1<<20))

	// Name collisions matter: one runtime means the LAST definition wins, which
	// is also what the Python host does. Report the count so a surprise is visible.
	seen := map[string]int{}
	for _, n := range clicks {
		seen[n]++
	}
	dupes := 0
	for _, c := range seen {
		if c > 1 {
			dupes++
		}
	}
	if dupes > 0 {
		t.Logf("WARNING: %d click name(s) defined more than once", dupes)
	}
}

// TestClickScriptsRun executes every click script with the builtins stubbed and
// reports the profile. It is NOT a correctness test: with every unknown builtin
// answering 0, a script that needs a real OBJECT (`commander.X`) stops at the
// first index. What it proves is that the ENGINE carries the corpus - which is
// the question the Lua choice was made on.
func TestClickScriptsRun(t *testing.T) {
	if testing.Short() {
		t.Skip("full corpus run is skipped with -short")
	}
	dir := corpusDir(t)

	h := New()
	defer h.Close()
	if _, err := h.LoadCorpus(dir); err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}

	clicks := h.Names("SCR_CLICK_")
	start := time.Now()
	ran, finished := 0, 0
	for _, name := range clicks {
		ran++
		if err := h.Call(name, 2*time.Second); err == nil {
			finished++
		}
	}
	elapsed := time.Since(start)

	t.Logf("ran %d click script(s), %d finished, %d stopped, in %s",
		ran, finished, ran-finished, elapsed.Round(time.Millisecond))
	t.Logf("distinct missing builtins: %d", len(h.Missing()))

	// The floor is deliberately loose. The measured rate with a full stub
	// environment was 2,345 / 4,335; the point of the assertion is that the
	// engine is not silently failing on the whole corpus.
	if finished < ran/2 {
		t.Fatalf("only %d of %d click scripts finished - the engine, not the "+
			"stubs, may be the cause", finished, ran)
	}
}

// TestTickBudgetCostIsSmall measures the WORST case for a shared runtime: how
// long one runaway script can hold it before the guard fires. One runtime serves
// every player, so this number is how long everyone else waits.
func TestTickBudgetCostIsSmall(t *testing.T) {
	h := New()
	defer h.Close()

	const ticks = 1_000_000
	start := time.Now()
	if err := h.L.DoString(`for i = 1, 1000000 do _vtick() end`); err != nil {
		t.Fatalf("loop: %v", err)
	}
	elapsed := time.Since(start)
	perTick := elapsed / ticks
	t.Logf("1e6 ticks in %s (%.0f ns each) -> the full %d-tick budget is about %s "+
		"of blocked runtime", elapsed.Round(time.Millisecond), float64(perTick),
		DefaultTickBudget, (perTick * DefaultTickBudget).Round(time.Millisecond))

	if perTick*DefaultTickBudget > 5*time.Second {
		t.Fatalf("a runaway script could block the shared runtime for %s - "+
			"the tick budget is too high for one runtime serving every player",
			(perTick * DefaultTickBudget).Round(time.Millisecond))
	}
}

// BenchmarkLoadCorpus measures startup cost: the zone loads the corpus once.
func BenchmarkLoadCorpus(b *testing.B) {
	dir := ""
	for _, candidate := range corpusCandidates() {
		if _, err := os.Stat(candidate); err == nil {
			dir = candidate
			break
		}
	}
	if dir == "" {
		b.Skip("no translated corpus - build it with scp_to_lua.py --dump")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.lua"))
	if len(files) == 0 {
		b.Skip("corpus has no .lua at the top level")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := New()
		if _, err := h.LoadCorpus(dir); err != nil {
			b.Fatal(err)
		}
		h.Close()
	}
}

// TestLazyLoadPaysOnlyForWhatItUses is the shape a ZONE needs: index the corpus,
// load the shared libraries, then load only the scripts a map actually uses.
//
// The number it guards is memory. Loading the whole corpus measured 828 MB; a
// zone serving one map needs about 90 scripts, so the ceiling here is set far
// below that - if it ever climbs back towards the whole-corpus figure, the lazy
// path has silently become eager.
func TestLazyLoadPaysOnlyForWhatItUses(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus indexing is skipped with -short")
	}
	dir := corpusDir(t)

	h := New()
	defer h.Close()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	indexed, err := h.Index(dir)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if indexed < 4000 {
		t.Fatalf("indexed %d script(s), expected the whole corpus (~4,300)", indexed)
	}
	if err := h.LoadLibraries(); err != nil {
		t.Fatalf("LoadLibraries: %v", err)
	}
	runtime.GC()
	var afterLibs runtime.MemStats
	runtime.ReadMemStats(&afterLibs)

	// One map's worth: a town map carries 83-88 NPC rows. A FILE may define more
	// than one click name (variants of the same NPC), so the count of loaded FILES
	// is what matters, not the count of names.
	names := h.Scripts()
	used := names[:90]
	filesLoaded := 0
	for _, name := range used {
		loaded, loadErr := h.EnsureScript(name)
		if loadErr != nil {
			t.Fatalf("EnsureScript(%s): %v", name, loadErr)
		}
		if loaded {
			filesLoaded++
		}
	}
	if filesLoaded == 0 {
		t.Fatal("no file was loaded - the lazy path did nothing")
	}
	// Second call must be free, or a player clicking twice grows memory forever.
	if loaded, _ := h.EnsureScript(used[0]); loaded {
		t.Fatalf("%s was loaded twice", used[0])
	}

	// The loaded function must actually be callable. A script error is fine (the
	// builtins are stubs); "is not a function" is not.
	if callErr := h.Call(used[0], 2*time.Second); callErr != nil &&
		strings.Contains(callErr.Error(), "is not a function") {
		t.Fatalf("%s did not become callable: %v", used[0], callErr)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	libsMB := float64(afterLibs.HeapAlloc-before.HeapAlloc) / (1 << 20)
	mapMB := float64(after.HeapAlloc-afterLibs.HeapAlloc) / (1 << 20)
	totalMB := float64(after.HeapAlloc-before.HeapAlloc) / (1 << 20)
	t.Logf("indexed %d script(s); libraries %.1f MB; %d map scripts %.1f MB; "+
		"total %.1f MB (whole corpus measured 828 MB)",
		indexed, libsMB, len(used), mapMB, totalMB)

	if totalMB > 150 {
		t.Fatalf("lazy load used %.1f MB for %d scripts - the whole corpus is "+
			"828 MB, so this is no longer lazy", totalMB, len(used))
	}
}
