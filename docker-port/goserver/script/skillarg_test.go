package script

// THE MAGICAL ARGUMENT BAG AND THE MESSAGE THAT CARRIES IT.
//
// These are the two names this lane added to the host, and each one is tested for
// the property that makes it usable rather than for "it did not crash":
//
//	GetMagicalSkillArg  a FRESH table per call, and what the script wrote survives to
//	                    the Go side as real values (numbers AND the two names).
//	OM_SKILL_MAGICAL_ASSIST  a message the host has no handler for still says so ONCE
//	                    and changes nothing - the old behaviour, unbroken.

import (
	"fmt"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// TestMagicalSkillArgIsFreshPerCallAndKeepsWhatTheScriptWrote is the first pass
// condition. A single shared bag would let one cast's `Buff` answer for the next
// cast's message, which is exactly the silent wrong buff this lane exists to prevent.
func TestMagicalSkillArgIsFreshPerCallAndKeepsWhatTheScriptWrote(t *testing.T) {
	h := New()
	defer h.Close()

	if err := h.L.DoString(`
		first = GetMagicalSkillArg()
		first.Buff = 'Coruscation'
		first.BuffType = 'FORCE'
		first.BuffFreq = 100
		first.BuffID = 63
		first.SklLv = 2

		second = GetMagicalSkillArg()
		same_table = rawequal(first, second)
		second.Buff = 'Sprint'
	`); err != nil {
		t.Fatalf("the probe chunk raised: %v", err)
	}

	if v := h.L.GetGlobal("same_table"); v != lua.LFalse {
		t.Fatalf("two GetMagicalSkillArg calls answered the SAME table (%v) - a cast's "+
			"bag would answer for the next cast", v)
	}

	// THE LAST BAG, as the Go side reads it after a run.
	arg := h.MagicalSkillArg()
	if arg == nil {
		t.Fatal("MagicalSkillArg answered nil after a script asked for a bag")
	}
	if got, _ := arg["Buff"].(string); got != "Sprint" {
		t.Fatalf("the bag's Buff reads %q, want %q (the second call's own write)", got, "Sprint")
	}
	if _, ok := arg["BuffType"]; ok {
		t.Fatalf("the SECOND bag carries BuffType %v, but nothing wrote it - the two "+
			"bags are shared state", arg["BuffType"])
	}

	// THE FIRST BAG IS STILL ITS OWN: the second call's write did not reach it.
	first, ok := h.L.GetGlobal("first").(*lua.LTable)
	if !ok {
		t.Fatal("the first bag is not a table any more")
	}
	if got := first.RawGetString("Buff"); got != lua.LString("Coruscation") {
		t.Fatalf("the first bag's Buff reads %v after the second call wrote 'Sprint' - "+
			"the bags alias each other", got)
	}

	// EVERY FIELD THE PASS CONDITION NAMES, as the Go side sees them.
	if err := h.L.DoString(`
		full = GetMagicalSkillArg()
		full.Buff = 'Coruscation'
		full.BuffType = 'FORCE'
		full.BuffFreq = 100
		full.BuffID = 63
	`); err != nil {
		t.Fatalf("the field chunk raised: %v", err)
	}
	full := h.MagicalSkillArg()
	for name, want := range map[string]any{
		"Buff": "Coruscation", "BuffType": "FORCE", "BuffFreq": float64(100),
		"BuffID": float64(63),
	} {
		if got := full[name]; got != want {
			t.Fatalf("the bag's %s reads %v (%T), want %v (%T)", name, got, got, want, want)
		}
	}

	// AND A RESET MAKES "NEVER ASKED" DISTINGUISHABLE FROM "ASKED AND LEFT EMPTY".
	h.ResetMagicalSkillArg()
	if arg := h.MagicalSkillArg(); arg != nil {
		t.Fatalf("after ResetMagicalSkillArg the reader still answers %v - a stale bag "+
			"would answer for a cast that computed nothing", arg)
	}
}

// TestAnUnknownMessageTypeStillLogsOnceAndChangesNothing is the "do not break what
// works" condition: SendMsg served exactly one message before this lane, and a name
// it still does not serve must produce ONE line (not a stream) and no effect.
func TestAnUnknownMessageTypeStillLogsOnceAndChangesNothing(t *testing.T) {
	h := New()
	defer h.Close()

	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	if err := h.L.DoString(`
		SendMsg({Handle = 11}, {Handle = 22}, 'OM_SOMETHING_NOBODY_SERVES', 0, 0)
		SendMsg({Handle = 11}, {Handle = 22}, 'OM_SOMETHING_NOBODY_SERVES', 0, 0)
		SendMsg({Handle = 11}, {Handle = 23}, 'OM_SOMETHING_NOBODY_SERVES', 0, 0)
	`); err != nil {
		t.Fatalf("the SendMsg chunk raised: %v", err)
	}

	said := 0
	for _, line := range lines {
		if strings.Contains(line, "OM_SOMETHING_NOBODY_SERVES") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the unserved message was logged %d time(s), want 1:\n%s",
			said, strings.Join(lines, "\n"))
	}
	if got := h.UnservedMessages(); len(got) != 1 || got[0] != "OM_SOMETHING_NOBODY_SERVES" {
		t.Fatalf("UnservedMessages answers %v, want the one name that reached the "+
			"default arm", got)
	}
}

// TestMagicalAssistWithoutAWorldSaysSoOnce covers the refused shape of the new
// message: with no live world installed, nothing may land and ONE line must name it.
func TestMagicalAssistWithoutAWorldSaysSoOnce(t *testing.T) {
	h := New()
	defer h.Close()

	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	if err := h.L.DoString(`
		arg = { Buff = 'Coruscation', BuffType = 'FORCE' }
		SendMsg({Handle = 11}, {Handle = 22}, 'OM_SKILL_MAGICAL_ASSIST', arg, {})
		SendMsg({Handle = 11}, {Handle = 22}, 'OM_SKILL_MAGICAL_ASSIST', arg, {})
	`); err != nil {
		t.Fatalf("the SendMsg chunk raised: %v", err)
	}

	said := 0
	for _, line := range lines {
		if strings.Contains(line, "no live world") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the world-less assist message was logged %d time(s), want 1:\n%s",
			said, strings.Join(lines, "\n"))
	}
}
