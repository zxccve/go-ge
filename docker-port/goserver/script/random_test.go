// Tests for the RNG binding (script\random.go).
//
// These call through the LUA state, not the Go handlers, on purpose: the thing
// being pinned is what a script sees. The corpus is not needed - every chunk
// here defines its own function - so these run on a machine with no translated
// scripts.
package script

import (
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// luaValues runs chunk and returns the values it `return`s, having reset the
// stack so one test cannot see another's leftovers.
func luaValues(t *testing.T, h *Host, chunk string) []lua.LValue {
	t.Helper()
	top := h.L.GetTop()
	if err := h.L.DoString(chunk); err != nil {
		t.Fatalf("chunk raised: %v\n%s", err, chunk)
	}
	n := h.L.GetTop() - top
	out := make([]lua.LValue, n)
	for i := 0; i < n; i++ {
		out[i] = h.L.Get(top + i + 1)
	}
	h.L.SetTop(top)
	return out
}

// luaNumber runs an expression that returns one number.
func luaNumber(t *testing.T, h *Host, chunk string) float64 {
	t.Helper()
	vals := luaValues(t, h, chunk)
	if len(vals) != 1 {
		t.Fatalf("chunk returned %d values, want 1", len(vals))
	}
	n, ok := vals[0].(lua.LNumber)
	if !ok {
		t.Fatalf("chunk returned %s, want a number", vals[0].Type())
	}
	return float64(n)
}

// TestIMCRandomCoversRangeInclusive is the whole fidelity requirement: 100,000
// draws stay inside [1,100], every value appears, and both ends do.
func TestIMCRandomCoversRangeInclusive(t *testing.T) {
	h := New()
	defer h.Close()
	h.Seed(12345)

	// One Lua loop rather than 100,000 Go round trips; it still calls the real
	// binding 100,000 times.
	vals := luaValues(t, h, `
		local bad, count = 0, {}
		for i = 1, 100000 do
			local v = IMCRandom(1, 100)
			if v < 1 or v > 100 or v ~= math.floor(v) then bad = bad + 1 end
			local k = tostring(v)
			count[k] = (count[k] or 0) + 1
		end
		local distinct = 0
		for _ in pairs(count) do distinct = distinct + 1 end
		return bad, distinct, count["1"] or 0, count["100"] or 0`)
	if len(vals) != 4 {
		t.Fatalf("returned %d values, want 4", len(vals))
	}
	bad, distinct := int(vals[0].(lua.LNumber)), int(vals[1].(lua.LNumber))
	low, high := int(vals[2].(lua.LNumber)), int(vals[3].(lua.LNumber))
	if bad != 0 {
		t.Fatalf("%d draw(s) were outside [1,100] or not whole numbers", bad)
	}
	if distinct != 100 {
		t.Fatalf("only %d distinct values in 100,000 draws, want 100", distinct)
	}
	if low == 0 || high == 0 {
		t.Fatalf("an end is unreachable: count(1)=%d count(100)=%d", low, high)
	}
	t.Logf("100000 draws: distinct=%d count(1)=%d count(100)=%d", distinct, low, high)
}

// TestIMCRandomEmptyRangeAnswersStart pins the original's rule: start == end and
// start > end both answer start - no error, no redraw.
func TestIMCRandomEmptyRangeAnswersStart(t *testing.T) {
	h := New()
	defer h.Close()

	if got := luaNumber(t, h, `return IMCRandom(7, 7)`); got != 7 {
		t.Fatalf("IMCRandom(7,7) = %v, want 7", got)
	}
	if got := luaNumber(t, h, `return IMCRandom(9, 3)`); got != 9 {
		t.Fatalf("IMCRandom(9,3) = %v, want 9", got)
	}
	// Repeat a few hundred times: an empty range must never be redrawn into a
	// different answer.
	for i := 0; i < 500; i++ {
		if got := luaNumber(t, h, `return IMCRandom(9, 3)`); got != 9 {
			t.Fatalf("draw %d: IMCRandom(9,3) = %v, want 9", i, got)
		}
	}
}

// TestIMCRandomTruncatesTowardZero proves the CVTTSD2SI cast: 1.9 -> 1 and
// 100.9 -> 100, so the float call is the integer call.
func TestIMCRandomTruncatesTowardZero(t *testing.T) {
	h := New()
	defer h.Close()

	// Same seed, same stream: the two call shapes must produce equal sequences.
	h.Seed(99)
	floats := luaValues(t, h, `
		local out = {}
		for i = 1, 500 do out[i] = tostring(IMCRandom(1.9, 100.9)) end
		return table.concat(out, ",")`)[0].String()

	h.Seed(99)
	ints := luaValues(t, h, `
		local out = {}
		for i = 1, 500 do out[i] = tostring(IMCRandom(1, 100)) end
		return table.concat(out, ",")`)[0].String()

	if floats != ints {
		t.Fatalf("IMCRandom(1.9,100.9) and IMCRandom(1,100) diverged:\n float=%s\n int  =%s",
			floats, ints)
	}

	// And the truncation reaches BOTH ends over a long run.
	h.Seed(4321)
	vals := luaValues(t, h, `
		local count, bad = {}, 0
		for i = 1, 100000 do
			local v = IMCRandom(1.9, 100.9)
			if v < 1 or v > 100 then bad = bad + 1 end
			count[tostring(v)] = (count[tostring(v)] or 0) + 1
		end
		local distinct = 0
		for _ in pairs(count) do distinct = distinct + 1 end
		return bad, distinct, count["1"] or 0, count["100"] or 0`)
	bad, distinct := int(vals[0].(lua.LNumber)), int(vals[1].(lua.LNumber))
	if bad != 0 || distinct != 100 {
		t.Fatalf("float args: bad=%d distinct=%d, want 0 and 100", bad, distinct)
	}
	if vals[2].(lua.LNumber) == 0 || vals[3].(lua.LNumber) == 0 {
		t.Fatalf("float args: an end is unreachable (1=%v, 100=%v)", vals[2], vals[3])
	}
}

// TestIMCRandomIsBoundNotRecorded proves the name left the missing list while
// every other unknown name still lands on it.
func TestIMCRandomIsBoundNotRecorded(t *testing.T) {
	h := New()
	defer h.Close()

	if err := h.L.DoString(`
		function SCR_CLICK__RNG_PROBE()
			local a = IMCRandom(1, 10)
			local b = ThisBuiltinWasNeverWritten_XYZ()
			return a + b
		end`); err != nil {
		t.Fatalf("define: %v", err)
	}
	if err := h.Call("SCR_CLICK__RNG_PROBE", 2*time.Second); err != nil {
		t.Fatalf("call: %v", err)
	}

	missing := h.Missing()
	if len(missing) != 1 {
		t.Fatalf("missing list = %+v, want exactly ThisBuiltinWasNeverWritten_XYZ", missing)
	}
	if missing[0].Name != "ThisBuiltinWasNeverWritten_XYZ" || missing[0].Calls != 1 {
		t.Fatalf("recorded %+v, want ThisBuiltinWasNeverWritten_XYZ x1", missing[0])
	}
}

// TestMathRandomLua51Semantics pins the three call shapes the corpus uses.
func TestMathRandomLua51Semantics(t *testing.T) {
	h := New()
	defer h.Close()
	h.Seed(2024)

	// math.random(1,100): whole numbers, both ends reachable.
	vals := luaValues(t, h, `
		local bad, count = 0, {}
		for i = 1, 100000 do
			local v = math.random(1, 100)
			if v < 1 or v > 100 or v ~= math.floor(v) then bad = bad + 1 end
			count[tostring(v)] = (count[tostring(v)] or 0) + 1
		end
		local distinct = 0
		for _ in pairs(count) do distinct = distinct + 1 end
		return bad, distinct, count["1"] or 0, count["100"] or 0`)
	if int(vals[0].(lua.LNumber)) != 0 || int(vals[1].(lua.LNumber)) != 100 {
		t.Fatalf("math.random(1,100): bad=%v distinct=%v, want 0 and 100", vals[0], vals[1])
	}
	if vals[2].(lua.LNumber) == 0 || vals[3].(lua.LNumber) == 0 {
		t.Fatalf("math.random(1,100): an end is unreachable (1=%v, 100=%v)", vals[2], vals[3])
	}

	// math.random(100): Lua 5.1 is 1-based, so the answer is 1..100.
	vals = luaValues(t, h, `
		local bad, count = 0, {}
		for i = 1, 100000 do
			local v = math.random(100)
			if v < 1 or v > 100 or v ~= math.floor(v) then bad = bad + 1 end
			count[tostring(v)] = (count[tostring(v)] or 0) + 1
		end
		local distinct = 0
		for _ in pairs(count) do distinct = distinct + 1 end
		return bad, distinct, count["1"] or 0, count["100"] or 0`)
	if int(vals[0].(lua.LNumber)) != 0 || int(vals[1].(lua.LNumber)) != 100 {
		t.Fatalf("math.random(100): bad=%v distinct=%v, want 0 and 100", vals[0], vals[1])
	}
	if vals[2].(lua.LNumber) == 0 || vals[3].(lua.LNumber) == 0 {
		t.Fatalf("math.random(100): an end is unreachable (1=%v, 100=%v)", vals[2], vals[3])
	}

	// math.random(): a float in [0,1), and not a constant.
	vals = luaValues(t, h, `
		local bad, first, distinct = 0, nil, {}
		for i = 1, 1000 do
			local v = math.random()
			if v < 0 or v >= 1 then bad = bad + 1 end
			if first == nil then first = v end
			distinct[tostring(v)] = true
		end
		local n = 0
		for _ in pairs(distinct) do n = n + 1 end
		return bad, n, first`)
	if int(vals[0].(lua.LNumber)) != 0 {
		t.Fatalf("math.random(): %v draw(s) outside [0,1)", vals[0])
	}
	if int(vals[1].(lua.LNumber)) < 900 {
		t.Fatalf("math.random() produced only %v distinct floats in 1000 draws", vals[1])
	}

	// Lua 5.1 rejects an empty interval instead of inventing an answer.
	for _, bad := range []string{`return math.random(0)`, `return math.random(5, 4)`} {
		if err := h.L.DoString(bad); err == nil {
			t.Fatalf("%s did not raise; Lua 5.1 rejects an empty interval", bad)
		} else if !strings.Contains(err.Error(), "interval is empty") {
			t.Fatalf("%s raised %q, want an 'interval is empty' error", bad, err)
		}
	}
}

// TestMathRandomSeededPerHost proves the two requirements a test needs: the
// same explicit seed reproduces a sequence, and different seeds do not.
func TestMathRandomSeededPerHost(t *testing.T) {
	draw := func(h *Host) string {
		return luaValues(t, h, `
			local out = {}
			for i = 1, 50 do out[i] = tostring(math.random(1, 1000000)) end
			return table.concat(out, ",")`)[0].String()
	}

	a, b := New(), New()
	defer a.Close()
	defer b.Close()

	a.Seed(111)
	b.Seed(111)
	if draw(a) != draw(b) {
		t.Fatal("two hosts with the same explicit seed produced different sequences")
	}

	a.Seed(111)
	b.Seed(222)
	if draw(a) == draw(b) {
		t.Fatal("two hosts with different seeds produced the same 50-draw sequence")
	}
}

// TestMathRandomseedReseedsTheSameStream pins that randomseed exists, returns
// nothing, and drives the SAME generator IMCRandom uses.
func TestMathRandomseedReseedsTheSameStream(t *testing.T) {
	h := New()
	defer h.Close()

	vals := luaValues(t, h, `
		math.randomseed(424242)
		local a = { IMCRandom(1, 1000000), math.random(1, 1000000), math.random() }
		math.randomseed(424242)
		local b = { IMCRandom(1, 1000000), math.random(1, 1000000), math.random() }
		return tostring(math.randomseed ~= nil),
			tostring(a[1] == b[1] and a[2] == b[2] and a[3] == b[3]),
			tostring(select("#", math.randomseed(1)))`)
	if len(vals) != 3 {
		t.Fatalf("returned %d values, want 3", len(vals))
	}
	if vals[0].String() != "true" {
		t.Fatal("math.randomseed does not exist")
	}
	if vals[1].String() != "true" {
		t.Fatal("randomseed did not reproduce the mixed IMCRandom/math.random sequence")
	}
	if vals[2].String() != "0" {
		t.Fatalf("math.randomseed returned %s values, want 0 (Lua 5.1)", vals[2])
	}
}

// TestScriptUsingMathRandomInArithmetic is the integration shape: the corpus
// writes `math.random(a,b) + math.random(c)` inside a formula, and that must run
// and answer a usable number.
func TestScriptUsingMathRandomInArithmetic(t *testing.T) {
	h := New()
	defer h.Close()

	if err := h.L.DoString(`
		function SCR_CLICK__RNG_ARITH()
			local jitter = math.random(10, 20) + math.random(5)
			local roll = IMCRandom(1, 100)
			return jitter * roll / 100
		end`); err != nil {
		t.Fatalf("define: %v", err)
	}

	fn, ok := h.L.GetGlobal("SCR_CLICK__RNG_ARITH").(*lua.LFunction)
	if !ok {
		t.Fatal("SCR_CLICK__RNG_ARITH is not a function")
	}
	if err := h.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		t.Fatalf("a formula using math.random raised: %v", err)
	}
	v := h.L.Get(-1)
	h.L.Pop(1)
	n, ok := v.(lua.LNumber)
	if !ok {
		t.Fatalf("the formula returned %s, want a number", v.Type())
	}
	if n < 0.11 || n > 25 {
		t.Fatalf("the formula returned %v, outside its possible range [0.11, 25]", n)
	}
}
