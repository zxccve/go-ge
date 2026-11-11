// The RNG: `IMCRandom` and a real `math.random`.
//
// WHY THIS FILE EXISTS: both names are used by the corpus, and until this
// binding `IMCRandom` went to the missing-builtin stub, which answered 0 - so
// every chance roll took its low branch. (The damage formula's
// `random(RATK * RHR/100, RATK)` therefore always answered its first argument.)
// gopher-lua's own `math.random` was worse than a constant: it ran, but was
// never seeded, so every process produced the same sequence.
//
// THE SEMANTICS ARE THE v28 ZONE SERVER'S, read from the machine code and not
// re-derived. `imc::RandomInt(start,end)` (0x0046E0CA) is inclusive at BOTH
// ends, returns `start` when `end <= start`, and draws uniformly (rejection
// sampling). The binding (`FUN_005F02C0`) reads both arguments as floats and
// casts them with CVTTSD2SI, i.e. truncation toward zero. Evidence:
// docker-port\history\reference-damage-system.md, "The RNG itself".
//
// WHAT WE DO NOT REPRODUCE: the original seeds once from `srand(timeGetTime())`
// through `rand()`, which gives only 32,768 possible seeds per process start.
// That is a seeding weakness, not a RESULT semantic, so our seed is the
// nanosecond clock and any int64 is accepted. The distribution and the
// inclusive bounds are the part that matters, and those are exact.
package script

import (
	"math"
	"math/rand"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// installRandom binds `IMCRandom` and replaces the two `math` functions with
// versions over ONE generator owned by this host. It is called from New, so a
// host is seeded once at construction.
func (h *Host) installRandom() {
	h.Seed(time.Now().UnixNano())

	h.L.SetGlobal("IMCRandom", h.L.NewFunction(h.imcRandom))

	// math.random and math.randomseed, Lua 5.1 shape, same stream. The table is
	// opened by lua.NewState's OpenLibs, so a missing one is a broken runtime,
	// not game data - the same contract as the prelude.
	mathTable, ok := h.L.GetGlobal("math").(*lua.LTable)
	if !ok {
		panic("script: math library is missing from the Lua state")
	}
	mathTable.RawSetString("random", h.L.NewFunction(h.mathRandom))
	mathTable.RawSetString("randomseed", h.L.NewFunction(h.mathRandomSeed))
}

// Seed reseeds the host's generator, so a test can make a run reproducible.
// The original's seed space is 32,768 values (millisecond clock through rand());
// ours is the whole int64 range - a deliberate, stated difference.
func (h *Host) Seed(seed int64) {
	h.rng = rand.New(rand.NewSource(seed))
}

// random is the generator, created on demand for a zero-value Host. New always
// seeds one, so this only covers a Host built by hand.
func (h *Host) random() *rand.Rand {
	if h.rng == nil {
		h.Seed(time.Now().UnixNano())
	}
	return h.rng
}

// imcRandom is `IMCRandom(a, b)`: a uniform integer in [a, b] with BOTH ends
// reachable, the original's truncation toward zero, and its rule that an empty
// range answers `start` rather than raising.
func (h *Host) imcRandom(L *lua.LState) int {
	start := int64(math.Trunc(float64(L.OptNumber(1, 0))))
	end := int64(math.Trunc(float64(L.OptNumber(2, 0))))
	if end <= start {
		L.Push(lua.LNumber(start))
		return 1
	}
	span := end - start + 1
	if span <= 0 { // overflowed: the original answers start here too
		L.Push(lua.LNumber(start))
		return 1
	}
	L.Push(lua.LNumber(start + int64(h.random().Intn(int(span)))))
	return 1
}

// mathRandom is Lua 5.1's `math.random`:
//
//	math.random()     -> a float in [0, 1)
//	math.random(m)    -> an integer in [1, m]
//	math.random(m, n) -> an integer in [m, n], both ends inclusive
//
// The integer cases use the generator's Intn rather than scaling a float, so
// the bounds are exact instead of merely likely.
func (h *Host) mathRandom(L *lua.LState) int {
	switch L.GetTop() {
	case 0:
		L.Push(lua.LNumber(h.random().Float64()))
	case 1:
		m := L.CheckInt(1)
		if m < 1 {
			L.ArgError(1, "interval is empty")
			return 0
		}
		L.Push(lua.LNumber(h.random().Intn(m) + 1))
	case 2:
		m := L.CheckInt(1)
		n := L.CheckInt(2)
		if n < m {
			L.ArgError(2, "interval is empty")
			return 0
		}
		span := int64(n) - int64(m) + 1
		if span <= 0 { // overflowed: answer the low end, as IMCRandom does
			L.Push(lua.LNumber(m))
			return 1
		}
		L.Push(lua.LNumber(m + h.random().Intn(int(span))))
	default:
		L.RaiseError("wrong number of arguments")
	}
	return 1
}

// mathRandomSeed is Lua 5.1's `math.randomseed`: it reseeds the SAME generator
// IMCRandom draws from and returns nothing (5.2 changed that).
func (h *Host) mathRandomSeed(L *lua.LState) int {
	h.Seed(int64(math.Trunc(float64(L.CheckNumber(1)))))
	return 0
}
