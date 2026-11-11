package script

// THE SCAN LINE IS SAID ONCE PER BODY, NOT ONCE PER TICK.
//
// `GetObjectLocaly` is the NONE loop's question - `mon_ts_none.scp:230`, inside a loop whose
// only other statement is `sleep(1000)` - so a zone holding 185 bodies wrote ~1,000 debug lines
// a minute and buried the lines the owner was reading (measured 2026-11-08 on
// `logs\zone_btlauch01.log`: 996 lines in the 23:23 minute, 689 in 23:24).
//
// What a reader needs is that a body's loop STARTED, and the one occasion the question changed
// (a different radius or filter). This file proves both, and proves the gate did not swallow an
// error: a bad range or a missing handle is a failure and still prints on every call.

import (
	"fmt"
	"strings"
	"testing"
)

// fakeSeek is a seek world that answers "nobody" and records what it was asked, so a test can
// tell "the host stopped calling" from "the host stopped writing about the calls".
type fakeSeek struct {
	asked []seekAsk
}

type seekAsk struct {
	mon    uint32
	rng    float64
	filter string
}

func (f *fakeSeek) SeekTarget(mon uint32, rng float64, filter string) (HateTarget, bool) {
	f.asked = append(f.asked, seekAsk{mon: mon, rng: rng, filter: filter})
	return HateTarget{}, false
}

func (f *fakeSeek) ResetHate(mon uint32) {}

func (f *fakeSeek) EnableMove(mon uint32) bool { return true }

// newScanHost is a runtime with the fake seek world and a log that keeps its lines.
func newScanHost(t *testing.T) (*Host, *fakeSeek, *[]string) {
	t.Helper()
	h := New()
	world := &fakeSeek{}
	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	h.Seek = world
	t.Cleanup(h.Close)
	return h, world, &lines
}

// luaDo runs one script and fails the test if it raises - a Lua error would mean the builtin
// answered something that is not nil, which is not what the corpus' guard expects.
func luaDo(t *testing.T, h *Host, src string) {
	t.Helper()
	if err := h.L.DoString(src); err != nil {
		t.Fatalf("the script raised: %v", err)
	}
}

// withPrefix keeps the lines that carry `prefix`, in order.
func withPrefix(lines []string, prefix string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// scanLines keeps the GetObjectLocaly DEBUG lines - the ones this card bounds.
func scanLines(lines []string) []string {
	return withPrefix(lines, "zone: MONSTER AI DEBUG GetObjectLocaly(")
}

const (
	scanBodyA = 0x40000007 // 1073741831
	scanBodyB = 0x40000008 // 1073741832
)

// TestMonsterSeekScanLineSaidOncePerBody is the card's first pass condition: THREE scans of one
// body with the same range write ONE line, and a FOURTH with a different range writes a second.
// The same file also pins the two neighbours of that rule: a different FILTER on the same range is
// news too (the question changed), and a second BODY gets its own first line (the key is the
// handle, not the line's text - which is what `reportOnce` would have done).
func TestMonsterSeekScanLineSaidOncePerBody(t *testing.T) {
	h, world, lines := newScanHost(t)

	luaDo(t, h, `
		local self = { Handle = 0x40000007 }
		for i = 1, 3 do GetObjectLocaly(self, 1000, 'PC') end
		FOUR = GetObjectLocaly(self, 1500, 'PC')
		FILTER = GetObjectLocaly(self, 1500, 'Monster')
		local other = { Handle = 0x40000008 }
		OTHER = GetObjectLocaly(other, 1000, 'PC')
	`)

	// Six scans really happened - the host is not skipping the call, only the line.
	if len(world.asked) != 6 {
		t.Fatalf("the seek world was asked %d time(s), want 6 (3 same-range + 1 new range + "+
			"1 new filter + 1 other body): %+v", len(world.asked), world.asked)
	}

	want := []string{
		`zone: MONSTER AI DEBUG GetObjectLocaly(mon=1073741831, rng=1000, filter="PC")`,
		`zone: MONSTER AI DEBUG GetObjectLocaly(mon=1073741831, rng=1500, filter="PC")`,
		`zone: MONSTER AI DEBUG GetObjectLocaly(mon=1073741831, rng=1500, filter="Monster")`,
		`zone: MONSTER AI DEBUG GetObjectLocaly(mon=1073741832, rng=1000, filter="PC")`,
	}
	got := scanLines(*lines)
	if len(got) != len(want) {
		t.Fatalf("%d scan line(s) for 6 scans, want %d - a repeat is not news:\n%s",
			len(got), len(want), strings.Join(*lines, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scan line %d is\n  %s\nwant\n  %s\n(all lines:\n%s)",
				i+1, got[i], want[i], strings.Join(*lines, "\n"))
		}
	}
}

// TestMonsterSeekScanErrorIsSaidOnEveryCall is the card's second pass condition: the gate is on
// the DIAGNOSTIC only. A scan with a range that is not a number, and a scan with no body at all,
// are failures - each still prints on every call, and none of them is counted as a scan line.
func TestMonsterSeekScanErrorIsSaidOnEveryCall(t *testing.T) {
	h, world, lines := newScanHost(t)

	luaDo(t, h, `
		local self = { Handle = 0x40000007 }
		local a = GetObjectLocaly(self, 'not a number', 'PC')
		local b = GetObjectLocaly(self, 'still not a number', 'PC')
		local c = GetObjectLocaly(nil, 1000, 'PC')
		local d = GetObjectLocaly(nil, 1000, 'PC')
		SELF = tostring(a) .. '/' .. tostring(b) .. '/' .. tostring(c) .. '/' .. tostring(d)
	`)

	// The refused calls never reached the world: the range is checked first.
	if len(world.asked) != 0 {
		t.Fatalf("a refused scan reached the seek world (%+v) - the argument check must come first",
			world.asked)
	}

	badRange := withPrefix(*lines, "script: GetObjectLocaly: ")
	if len(badRange) != 2 {
		t.Fatalf("%d bad-range line(s) for 2 bad-range calls, want 2 - the gate must not "+
			"swallow a failure:\n%s", len(badRange), strings.Join(*lines, "\n"))
	}
	noBody := withPrefix(*lines, "script: GetObjectLocaly needs the body (self)")
	if len(noBody) != 2 {
		t.Fatalf("%d missing-body line(s) for 2 such calls, want 2 - the gate must not "+
			"swallow a failure:\n%s", len(noBody), strings.Join(*lines, "\n"))
	}
	if got := scanLines(*lines); len(got) != 0 {
		t.Fatalf("a refused scan wrote a scan line (%q) - a call that never happened is not "+
			"a start:\n%s", got, strings.Join(*lines, "\n"))
	}
	if got := h.L.GetGlobal("SELF").String(); got != "nil/nil/nil/nil" {
		t.Fatalf("a refused scan answered %q, want nil four times (the corpus guards `~= nil`)", got)
	}
}

// TestMonsterSeekScanLineStaysBoundedAtZoneScale drives one measured minute of
// btl_auch01: 185 bodies, each asking the same question 60 times (its one-second NONE
// loop). Before the gate this test counted 11,100 lines; the bound must make it 185 -
// one per body - which is the difference between a log a reader can use and one that
// buries the heal, the refused command and the kill behind a thousand identical lines.
func TestMonsterSeekScanLineStaysBoundedAtZoneScale(t *testing.T) {
	const bodies, turns = 185, 60
	h, world, lines := newScanHost(t)

	luaDo(t, h, `
		for b = 0, 184 do
			local self = { Handle = 0x00010000 + b }
			for i = 1, 60 do GetObjectLocaly(self, 2500, 'PC') end
		end`)

	// The scans really happened; only the writing is bounded.
	if len(world.asked) != bodies*turns {
		t.Fatalf("the seek world was asked %d time(s), want %d - the host must not stop "+
			"scanning, only stop repeating itself", len(world.asked), bodies*turns)
	}
	if got := scanLines(*lines); len(got) != bodies {
		t.Fatalf("%d bodies x %d scans wrote %d scan line(s), want %d (one per body) - "+
			"the line is not bounded:\n%s", bodies, turns, len(got), bodies,
			strings.Join(got, "\n"))
	}
}
