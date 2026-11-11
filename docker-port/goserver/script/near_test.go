package script

import (
	"fmt"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// THE RADIUS SCAN AND `SendMsg`, WITH A FAKE WORLD.
//
// What is tested here is the SCRIPT HOST's half: that the count and the index calls
// agree on ONE list, that the index is 0-based (the script's own `for i=0,
// count1-1`), that an empty or impossible scan is 0/nil and never a Lua error, and
// that `SendMsg(self, obj, 'OM_KILL', 0, 0)` reaches the world with the handle of
// the object the scan handed back.
//
// Whether a monster 800 units away counts as near is the ZONE's rule and is tested
// there (zone\near_test.go) against the real registry - a fake here would only test
// the fake.

// fakeNear is a world that answers a fixed list and records what it was asked.
type fakeNear struct {
	objects  []NearObject
	err      error
	asked    []nearAsk
	killed   []uint32
	killErr  error
	killDone int
}

type nearAsk struct {
	x, y   int32
	radius float64
	kind   string
}

func (f *fakeNear) NearObjects(x, y int32, radius float64, kind string) ([]NearObject, error) {
	f.asked = append(f.asked, nearAsk{x: x, y: y, radius: radius, kind: kind})
	if f.err != nil {
		return nil, f.err
	}
	return f.objects, nil
}

func (f *fakeNear) KillMonster(handle uint32) error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, handle)
	f.killDone++
	return nil
}

// newNearHost is a runtime with the fake world and a log that keeps its lines.
func newNearHost(t *testing.T, world NearWorld) (*Host, *[]string) {
	t.Helper()
	h := New()
	h.TickBudget = DefaultTickBudget
	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	h.Near = world
	t.Cleanup(h.Close)
	return h, &lines
}

// scatterSelf is the `self` the Scarecrow's own script passes: the NPC, with the
// position GetPos reads back.
const scatterSelf = `local self = { Handle = 0x40000000, X = 2240, Y = -590, Z = 7,
	Name = 'NPC_HITDUMMY01' }`

// luaList reads a global Lua table's array part as strings, in order. It is how a
// test reads back what the script collected without asserting on Lua internals.
func luaList(h *Host, name string) []string {
	t, ok := h.L.GetGlobal(name).(*lua.LTable)
	if !ok {
		return nil
	}
	out := make([]string, 0, t.Len())
	for i := 1; i <= t.Len(); i++ {
		out = append(out, t.RawGetInt(i).String())
	}
	return out
}

// TestNearObjectCountAndGetNearObjectAgree is the shape the game's script uses,
// written the way the script writes it: one count, then a 0-based walk that stops
// at count-1.
//
// The two objects are the fifteen Scarecrow ids' first two, so `obj.ClassID` is
// compared against the same numbers the real click compares against.
func TestNearObjectCountAndGetNearObjectAgree(t *testing.T) {
	world := &fakeNear{objects: []NearObject{
		{Handle: 0x20000002, ClassID: 11600, X: 2623, Y: -665},
		{Handle: 0x20000003, ClassID: 11605, X: 2700, Y: -700},
	}}
	h, lines := newNearHost(t, world)

	if err := h.L.DoString(scatterSelf + `
		local HitDummyID = { 11600, 11601, 11602, 11603, 11604, 11605, 11606,
		                     11607, 11608, 11609, 11610, 11611, 11612, 11613, 11614 }
		local count = GetNearObjectCount(self, 800, "Monster", 0)
		SEEN = count
		IDS = {}
		for i = 0, count - 1 do
			local obj = GetNearObject(i)
			if (obj ~= nil) then
				local hit = 0
				for t = 1, table.getn(HitDummyID) do
					if (obj.ClassID == HitDummyID[t]) then hit = 1 end
				end
				IDS[#IDS + 1] = obj.ClassID .. "/" .. hit
			end
		end
	`); err != nil {
		t.Fatalf("the scan raised inside Lua: %v", err)
	}

	if got := h.L.GetGlobal("SEEN").String(); got != "2" {
		t.Fatalf("the count answered %s, want 2", got)
	}
	if len(world.asked) != 1 {
		t.Fatalf("the world was asked %d time(s), want 1", len(world.asked))
	}
	ask := world.asked[0]
	if ask.x != 2240 || ask.y != -590 {
		t.Errorf("the circle was centred on (%d, %d), want the NPC's own (2240, -590)",
			ask.x, ask.y)
	}
	if ask.radius != 800 {
		t.Errorf("radius %g, want 800 - the number the game's script passes", ask.radius)
	}
	if ask.kind != "Monster" {
		t.Errorf("kind %q, want Monster", ask.kind)
	}
	if len(*lines) != 0 {
		t.Errorf("a working scan logged %q, want nothing", *lines)
	}

	// The 0-based walk found BOTH, in the world's order, and each ClassID matched
	// one of the fifteen ids the click compares against.
	got := luaList(h, "IDS")
	want := []string{"11600/1", "11605/1"}
	if len(got) != len(want) {
		t.Fatalf("%d object(s) reached the script, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("object %d read as %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGetNearObjectIsZeroBasedAndAnswersNilOutside is the trap the brief names: the
// script counts from 0, and an index past the end is nil, not an error. A 1-based
// table here would look right in Lua and skip every second object.
func TestGetNearObjectIsZeroBasedAndAnswersNilOutside(t *testing.T) {
	world := &fakeNear{objects: []NearObject{
		{Handle: 0x20000002, ClassID: 11600},
		{Handle: 0x20000003, ClassID: 11601},
	}}
	h, lines := newNearHost(t, world)

	if err := h.L.DoString(scatterSelf + `
		ANSWERED = GetNearObjectCount(self, 800, "Monster", 0)
		FIRST = GetNearObject(0).ClassID
		SECOND = GetNearObject(1).ClassID
		PAST = (GetNearObject(2) == nil)
		NEGATIVE = (GetNearObject(-1) == nil)
	`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	for name, want := range map[string]string{
		"ANSWERED": "2", "FIRST": "11600", "SECOND": "11601",
		"PAST": "true", "NEGATIVE": "true",
	} {
		if got := h.L.GetGlobal(name).String(); got != want {
			t.Errorf("%s = %s, want %s (0-based, nil past the end)", name, got, want)
		}
	}
	if len(*lines) != 0 {
		t.Errorf("an out-of-range index logged %q - it is an ordinary answer", *lines)
	}
}

// TestNearScanSeesOnlyTheObjectItActuallyHolds: a scan must return what the world
// says, not what a previous scan said. The second count here FAILS, and the old
// list must be dropped - otherwise the remove branch walks monsters that are gone.
func TestNearScanDropsTheOldListWhenItFails(t *testing.T) {
	world := &fakeNear{objects: []NearObject{{Handle: 0x20000002, ClassID: 11600}}}
	h, lines := newNearHost(t, world)

	if err := h.L.DoString(scatterSelf + `
		FIRST = GetNearObjectCount(self, 800, "Monster", 0)
		FIRST_ID = GetNearObject(0).ClassID
	`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if got := h.L.GetGlobal("FIRST").String(); got != "1" {
		t.Fatalf("the first scan counted %s, want 1", got)
	}

	world.err = fmt.Errorf("this zone only serves the \"Monster\" type word")
	if err := h.L.DoString(scatterSelf + `
		SECOND = GetNearObjectCount(self, 800, "Monster", 0)
		STALE = (GetNearObject(0) == nil)
	`); err != nil {
		t.Fatalf("a failed scan must not raise: %v", err)
	}
	if got := h.L.GetGlobal("SECOND").String(); got != "0" {
		t.Errorf("a failed scan counted %s, want 0", got)
	}
	if got := h.L.GetGlobal("STALE").String(); got != "true" {
		t.Errorf("GetNearObject(0) still answers the previous list - it must be nil")
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "only serves") {
		t.Fatalf("the world's reason must be in the log: %q", *lines)
	}
}

// TestNearScanRefusesBadArgumentsWithoutRaising: every wrong call is 0 plus one
// line, because a raise inside a click kills the whole dialog.
func TestNearScanRefusesBadArgumentsWithoutRaising(t *testing.T) {
	cases := []struct {
		name   string
		prefix string // what the script sets up first
		call   string
		want   string
	}{
		{
			name: "no object to search around",
			call: `GetNearObjectCount(nil, 800, "Monster", 0)`,
			want: "argument 1",
		},
		{
			name:   "range as a string",
			prefix: scatterSelf,
			call:   `GetNearObjectCount(self, "800", "Monster", 0)`,
			want:   "argument 2 (range) is a string",
		},
		{
			name:   "a zero range",
			prefix: scatterSelf,
			call:   `GetNearObjectCount(self, 0, "Monster", 0)`,
			want:   "not a positive distance",
		},
		{
			name:   "a negative range",
			prefix: scatterSelf,
			call:   `GetNearObjectCount(self, -800, "Monster", 0)`,
			want:   "not a positive distance",
		},
		{
			name:   "the type as a number",
			prefix: scatterSelf,
			call:   `GetNearObjectCount(self, 800, 11600, 0)`,
			want:   "argument 3 (type) is a number",
		},
		{
			name: "an object with no position",
			call: `GetNearObjectCount({}, 800, "Monster", 0)`,
			want: "no X/Y",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			world := &fakeNear{}
			h, lines := newNearHost(t, world)
			if err := h.L.DoString(c.prefix + "\nANSWER = " + c.call); err != nil {
				t.Fatalf("a refused call must not raise inside Lua: %v", err)
			}
			if got := h.L.GetGlobal("ANSWER").String(); got != "0" {
				t.Errorf("a refused scan answered %s, want 0", got)
			}
			if len(world.asked) != 0 {
				t.Fatalf("a refused call still searched the world %d time(s)", len(world.asked))
			}
			if len(*lines) != 1 || !strings.Contains((*lines)[0], c.want) {
				t.Fatalf("want one line carrying %q, got %q", c.want, *lines)
			}
		})
	}
}

// TestNearScanWithNoWorldSaysSo: 0 with a reason, never a silent empty field -
// "nothing is near" and "nobody can look" must not read the same.
func TestNearScanWithNoWorldSaysSo(t *testing.T) {
	h, lines := newNearHost(t, nil)
	if err := h.L.DoString(scatterSelf +
		`ANSWER = GetNearObjectCount(self, 800, "Monster", 0)`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if got := h.L.GetGlobal("ANSWER").String(); got != "0" {
		t.Errorf("the count answered %s, want 0", got)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "no live world is wired") {
		t.Fatalf("the refusal does not say there is no world: %q", *lines)
	}
}

// TestSendMsgKillReachesTheWorldWithTheScansHandle is the remove path, end to end
// through the host: the handle comes from the object GetNearObject handed back, not
// from the script and not from the NPC.
func TestSendMsgKillReachesTheWorldWithTheScansHandle(t *testing.T) {
	world := &fakeNear{objects: []NearObject{{Handle: 0x20000002, ClassID: 11600}}}
	h, lines := newNearHost(t, world)

	if err := h.L.DoString(scatterSelf + `
		local count = GetNearObjectCount(self, 800, "Monster", 0)
		for o = 0, count - 1 do
			local obj = GetNearObject(o)
			if (obj ~= nil) and (obj.ClassID == 11600) then
				SendMsg(self, obj, 'OM_KILL', 0, 0)
			end
		end
	`); err != nil {
		t.Fatalf("the remove branch raised: %v", err)
	}
	if len(world.killed) != 1 {
		t.Fatalf("%d kill(s) reached the world, want 1", len(world.killed))
	}
	if world.killed[0] != 0x20000002 {
		t.Errorf("killed handle 0x%08x, want the scanned object's 0x20000002",
			world.killed[0])
	}
	// One line is the ZONE's for a kill (the handle, its class and where it stood);
	// the host must not add a second.
	if len(*lines) != 0 {
		t.Errorf("a working kill logged %q, want nothing from the host", *lines)
	}
}

// TestSendMsgWithoutAHandleOrAWorldIsRefused: a message to something that is not an
// object, or with no world behind it, is one line and no call.
func TestSendMsgWithoutAHandleOrAWorldIsRefused(t *testing.T) {
	t.Run("target is not an object", func(t *testing.T) {
		world := &fakeNear{}
		h, lines := newNearHost(t, world)
		if err := h.L.DoString(`SendMsg({}, 7, 'OM_KILL', 0, 0)`); err != nil {
			t.Fatalf("unexpected raise: %v", err)
		}
		if len(world.killed) != 0 {
			t.Fatal("a kill was sent to something that is not an object")
		}
		if len(*lines) != 1 || !strings.Contains((*lines)[0], "argument 2") {
			t.Fatalf("want one line naming argument 2, got %q", *lines)
		}
	})

	t.Run("no world wired", func(t *testing.T) {
		h, lines := newNearHost(t, nil)
		if err := h.L.DoString(`SendMsg({}, { Handle = 0x20000002 }, 'OM_KILL', 0, 0)`); err != nil {
			t.Fatalf("unexpected raise: %v", err)
		}
		if len(*lines) != 1 || !strings.Contains((*lines)[0], "no live world is wired") {
			t.Fatalf("want one line saying there is no world: %q", *lines)
		}
	})

	t.Run("the world refuses", func(t *testing.T) {
		world := &fakeNear{killErr: fmt.Errorf("handle 0x20000009 is not a monster this zone is running")}
		h, lines := newNearHost(t, world)
		if err := h.L.DoString(`SendMsg({}, { Handle = 0x20000009 }, 'OM_KILL', 0, 0)`); err != nil {
			t.Fatalf("unexpected raise: %v", err)
		}
		if len(*lines) != 1 || !strings.Contains((*lines)[0], "not a monster this zone is running") {
			t.Fatalf("the world's reason is not in the log: %q", *lines)
		}
	})
}

// TestSendMsgReportsAnUnservedMessageONCE: a message this build does not deliver
// must be visible, but a script that sends one per tick must not flood the log.
func TestSendMsgReportsAnUnservedMessageONCE(t *testing.T) {
	world := &fakeNear{}
	h, lines := newNearHost(t, world)
	if err := h.L.DoString(`
		for i = 1, 5 do
			SendMsg({}, { Handle = 0x20000002 }, 'OM_NOTIFY_END_ATTACK', 0, 0)
		end
	`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if len(world.killed) != 0 {
		t.Fatal("an unserved message was delivered as a kill")
	}
	if len(*lines) != 1 {
		t.Fatalf("%d line(s) for one repeated message, want 1: %q", len(*lines), *lines)
	}
	if !strings.Contains((*lines)[0], "OM_NOTIFY_END_ATTACK") ||
		!strings.Contains((*lines)[0], "NOT served") {
		t.Fatalf("the line does not name the message and that it is not served: %q", (*lines)[0])
	}
}
