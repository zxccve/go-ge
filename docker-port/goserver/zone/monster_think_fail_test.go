package zone

// The think turn's TWO remaining pass conditions: a script that FAILS (c), and the
// startup line that names the mode (d). Kept in their own file so the happy path in
// monster_think_test.go reads as one story.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/script"
)

// TestMonsterAIScriptFailsOnceAndDoesNotHang is pass condition (c): a script that raises
// is reported ONCE, with the script's OWN FILE AND LINE, and the think pass keeps going -
// a broken body must not cost a log line per tick and must not stop the tick.
//
// The fixture is a two-entry body in the reference's own shape: the NONE entry moves it
// to TS_BATTLE (what the real one does when it has a target) and the BATTLE entry raises.
func TestMonsterAIScriptFailsOnceAndDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	blob := "function SCR_MON_FAIL_TS_NONE(self)\n" +
		"\tChangeTactics(self, 'TS_BATTLE')\n" +
		"\treturn\n" +
		"end\n" +
		"function SCR_MON_FAIL_TS_BATTLE(self)\n" +
		"\terror('boom from the fixture')\n" +
		"end\n"
	if err := os.WriteFile(filepath.Join(dir, "mon_fail.lua"), []byte(blob), 0o644); err != nil {
		t.Fatalf("write the fixture script: %v", err)
	}

	h, logs, scriptLogs, _, mon, host := monaiZoneIn(t, [3]int32{0, 150, 29}, true, dir, "MON_FAIL")

	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		h.monsterAITick(t0.Add(time.Duration(i) * 100 * time.Millisecond))
	}

	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the fixture's NONE entry did not run (state %q) - the failure under "+
			"test is not the one being proved:\n%s", got, logs.String())
	}

	failed := failedLines(scriptLogs.String())
	if len(failed) != 1 {
		t.Fatalf("%d FAILED line(s) after five passes, want exactly one:\n%s",
			len(failed), strings.Join(failed, "\n"))
	}
	if !strings.Contains(failed[0], "mon_fail.lua:6") {
		t.Fatalf("the FAILED line does not name the script's own place (mon_fail.lua:6):\n%s",
			failed[0])
	}
	if !strings.Contains(failed[0], "boom from the fixture") {
		t.Fatalf("the FAILED line does not carry the script's own error:\n%s", failed[0])
	}
	if _, running := host.MonsterThreadEntry(mon.Handle); running {
		t.Fatalf("a thread is still running after the failure - the tick would resume a " +
			"broken script forever")
	}

	// AND THE TICK ITSELF SURVIVED: a sixth pass costs nothing and adds no line.
	h.monsterAITick(t0.Add(time.Second))
	if n := len(failedLines(scriptLogs.String())); n != 1 {
		t.Fatalf("a later pass wrote another FAILED line (%d now) - the failure is not "+
			"once-only", n)
	}
}

// failedLines is the runner's FAILED lines out of the host's log text.
func failedLines(logText string) []string {
	var out []string
	for _, line := range strings.Split(logText, "\n") {
		if strings.Contains(line, "FAILED") {
			out = append(out, line)
		}
	}
	return out
}

// TestMonsterAIStartupLineNamesTheMode is pass condition (d).
func TestMonsterAIStartupLineNamesTheMode(t *testing.T) {
	goLine := MonsterAIStartupLine(false)
	if !strings.Contains(goLine, "zone: MONSTER AI go") || !strings.Contains(goLine, "default") {
		t.Fatalf("the go-mode line does not name the mode and its default: %q", goLine)
	}
	scriptLine := MonsterAIStartupLine(true)
	for _, want := range []string{"zone: MONSTER AI script", "SCR_MON_AI_LIB_TS_BATTLE",
		"SCR_<Tactics>_TS_<state>", "GO CHASE is"} {
		if !strings.Contains(scriptLine, want) {
			t.Fatalf("the script-mode line is missing %q: %q", want, scriptLine)
		}
	}
}

// TestMonsterAIEntryIsTheReferencesDispatch pins the sprintf + _strupr_s contract
// (MonsterNPC.cpp:1116-1118) the zone's entry name depends on.
func TestMonsterAIEntryIsTheReferencesDispatch(t *testing.T) {
	cases := []struct{ tactics, state, want string }{
		{"MON_AGGRESSIVE", "TS_NONE", "SCR_MON_AGGRESSIVE_TS_NONE"},
		{"mon_aggressive", "ts_battle", "SCR_MON_AGGRESSIVE_TS_BATTLE"},
		{"  MON_ELITE  ", " TS_RALLY1 ", "SCR_MON_ELITE_TS_RALLY1"},
		{"", "TS_NONE", ""},
		{"MON_AGGRESSIVE", "", ""},
	}
	for _, c := range cases {
		if got := script.MonsterAIEntry(c.tactics, c.state); got != c.want {
			t.Errorf("MonsterAIEntry(%q, %q) = %q, want %q", c.tactics, c.state, got, c.want)
		}
	}
}
