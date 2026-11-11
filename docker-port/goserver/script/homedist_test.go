// THE HOME-SICK SELF-KILL NEVER FIRES ON LIVE, SO IT MUST NOT FIRE HERE.
//
// `mon_ts_none.scp:107-112` asks `GetMyHomeDistance(self)` at the top of every idle
// loop and kills the body when the answer is over 4,000:
//
//	if('TRUE' == homeSick){
//	    var homedist = GetMyHomeDistance(self);
//	    if(homedist > 4000){ SendMsg(self, self, 'OM_KILL', 0, 0); }
//	}
//
// The owner measured the live server on 2026-11-08, at the Bigfoot's own map: a Bigfoot
// kited far from its spawn, lost him, and did NOT kill itself - it turned around and
// walked home, exactly like the bear before it. Both rows are `HomeSick TRUE`
// (`MON_AGGRESSIVERUN` 11483, `MON_AGGRESSIVE` 11591), so the branch is dead on live and
// the only reading that fits is that this call answers small there. OUR build fired it
// as written: `zone: OM_KILL` at 7,940 and 8,038 units from spawn on 2026-11-10.
//
// The two quantities are therefore split: the WANDER anchor stays the birth point
// (`Zone.Home`, untouched - that is what walks a body home), and this ANSWER is live's
// 0, with the true number kept in ONE log line per body so the record shows what the
// engine measured. These tests pin both halves.
package script

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// homeSickHandle is the handle the owner's Bigfoot carried in the 2026-11-10 log
// (`0x20001234`), so the HOMEDIST line's own assertion reads like the live one.
const homeSickHandle = 536875572

// homeSickDist is a body kited 8,000 units from its birth point - past the script's
// 4,000 twice over, and the distance our own build killed at.
const homeSickDist = 8000

// fakeHomeWorld is the ZONE's half of `MonsterWorld`: it answers the TRUE home distance
// the zone computes (`Handler.HomeDistance`) and counts every ask, so a test can tell
// "the builtin stopped asking the zone" from "the builtin stopped answering the script".
type fakeHomeWorld struct {
	home    MonsterHome
	dist    float64
	known   bool
	asked   int
	options map[string]MonsterOption
	tactic  string
}

func (f *fakeHomeWorld) ChangeTactics(_ uint32, tactic string)   { f.tactic = tactic }
func (f *fakeHomeWorld) Home(uint32) (MonsterHome, bool)         { return f.home, f.known }
func (f *fakeHomeWorld) Distance(uint32, uint32) (float64, bool) { return 0, true }

func (f *fakeHomeWorld) HomeDistance(uint32) (float64, bool) {
	f.asked++
	return f.dist, f.known
}

func (f *fakeHomeWorld) AiOption(_ uint32, key string) (MonsterOption, bool) {
	opt, ok := f.options[key]
	return opt, ok
}

// fakeFarField is the rest of the field the real idle entry needs: no target, no
// neighbour, every walk accepted, and the two things a test reads back - the kills it
// was asked for and the walks it was handed.
type fakeFarField struct {
	kills []uint32
	moves []fakeMove
}

type fakeMove struct {
	mon     uint32
	x, y, z float64
}

// NearWorld
func (f *fakeFarField) NearObjects(int32, int32, float64, string) ([]NearObject, error) {
	return nil, nil
}
func (f *fakeFarField) KillMonster(handle uint32) error {
	f.kills = append(f.kills, handle)
	return nil
}

// HateWorld
func (f *fakeFarField) InsertHate(uint32, uint32, int) {}
func (f *fakeFarField) RemoveHate(uint32, uint32)      {}
func (f *fakeFarField) TopHate(uint32) (HateTarget, bool) {
	return HateTarget{}, false
}
func (f *fakeFarField) SetTacticsTarget(uint32, uint32) {}
func (f *fakeFarField) TacticsTarget(uint32) (HateTarget, bool) {
	return HateTarget{}, false
}
func (f *fakeFarField) IsAttacking(uint32) bool { return false }

// MonsterSeek
func (f *fakeFarField) SeekTarget(uint32, float64, string) (HateTarget, bool) {
	return HateTarget{}, false
}
func (f *fakeFarField) ResetHate(uint32)       {}
func (f *fakeFarField) EnableMove(uint32) bool { return true }

// OrderWorld
func (f *fakeFarField) MoveMonster(mon uint32, x, y, z float64) {
	f.moves = append(f.moves, fakeMove{mon: mon, x: x, y: y, z: z})
}
func (f *fakeFarField) MoveMonsterToOwner(uint32, float64) {}
func (f *fakeFarField) StartMonsterAttack(uint32, uint32)  {}
func (f *fakeFarField) StartMonsterGroundAttack(uint32)    {}
func (f *fakeFarField) StopMonsterAttack(uint32)           {}

// homeDistanceHost is a runtime whose log is kept and whose world answers whatever the
// caller set on `fakeHomeWorld`.
func homeDistanceHost(t *testing.T) (*Host, *fakeHomeWorld, func() []string) {
	t.Helper()
	h := New()
	t.Cleanup(h.Close)
	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	w := &fakeHomeWorld{
		home:  MonsterHome{X: 0, Y: 0, Z: 29},
		dist:  homeSickDist,
		known: true,
	}
	h.Monster = w
	return h, w, func() []string { return lines }
}

// homedistLines keeps the HOMEDIST lines, in order.
func homedistLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "HOMEDIST") {
			out = append(out, line)
		}
	}
	return out
}

// TestGetMyHomeDistanceAnswersLivesZeroAndKeepsTheTrueNumberForTheZone is pass
// conditions (c) and the log line, on a body 8,000 units from its birth point: the
// SCRIPT reads 0 - the answer live behaves like - while the zone is still asked for the
// true distance on every call and the one line records it.
func TestGetMyHomeDistanceAnswersLivesZeroAndKeepsTheTrueNumberForTheZone(t *testing.T) {
	h, world, lines := homeDistanceHost(t)

	src := `
		local mon = { Handle = ` + fmt.Sprint(homeSickHandle) + `, ClassName = 'Monster_Bigfoot' }
		local first = GetMyHomeDistance(mon)
		assert(first == 0, 'a body 8000 from its birth point answered ' .. tostring(first) ..
			' - live never fires the home-sick branch, so the script must read 0')
		local second = GetMyHomeDistance(mon)
		assert(second == 0, 'the second read answered ' .. tostring(second))
	`
	if err := h.L.DoString(src); err != nil {
		t.Fatalf("the script raised: %v\nengine log:\n%s", err, strings.Join(lines(), "\n"))
	}

	// (c) THE ZONE IS STILL ASKED EVERY TIME. The answer to the script is live's 0, not
	// an arithmetic this file invented: the true distance is read off the zone on each
	// call, which is what makes the log line the zone's own number and not a copy.
	if world.asked != 2 {
		t.Fatalf("the zone was asked for the home distance %d time(s), want 2 - the "+
			"answer to the script is 0, but the TRUE number must still come from the zone",
			world.asked)
	}

	want := "zone: HOMEDIST 0x20001234 Monster_Bigfoot is 8000 from its birth point - " +
		"no self-kill (live does not fire this)"
	got := homedistLines(lines())
	if len(got) != 1 {
		t.Fatalf("%d HOMEDIST line(s), want exactly one per body:\n%s",
			len(got), strings.Join(lines(), "\n"))
	}
	if got[0] != want {
		t.Fatalf("the HOMEDIST line reads\n  %s\nwant\n  %s", got[0], want)
	}

	// ONCE PER BODY, not once per call: the entry asks every second, so a repeat would be
	// one line a second forever. And a body still INSIDE the script's own 4,000 is not
	// the branch being reached - it says nothing, and still reads 0.
	world.dist = 3999
	if err := h.L.DoString(src); err != nil {
		t.Fatalf("the near-body read raised: %v", err)
	}
	if n := len(homedistLines(lines())); n != 1 {
		t.Fatalf("a body 3999 from its birth point added a line (%d now) - the branch is "+
			"only reached past 4000", n)
	}

	// A SECOND body is its own one line; a handle the field never spawned answers 0 and
	// says nothing (it is also the reference's NULL-object answer).
	world.dist = homeSickDist
	src = `
		local other = { Handle = ` + fmt.Sprint(homeSickHandle+1) + `, Name = 'Monster_Bear' }
		assert(0 == GetMyHomeDistance(other), 'the second body did not answer 0')
		assert(0 == GetMyHomeDistance(other), 'the second body did not answer 0 twice')
	`
	if err := h.L.DoString(src); err != nil {
		t.Fatalf("the second-body read raised: %v", err)
	}
	got = homedistLines(lines())
	if len(got) != 2 {
		t.Fatalf("%d HOMEDIST line(s) after a second body, want 2:\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[1], "0x20001235 Monster_Bear is 8000") {
		t.Fatalf("the second body's line reads %q - it must name its own handle and class",
			got[1])
	}

	// A handle with no spawn point is the reference's NULL object: 0, and no line.
	world.known = false
	if err := h.L.DoString(`assert(0 == GetMyHomeDistance({ Handle = 12345 }), ` +
		`'a ghost answered a home distance')`); err != nil {
		t.Fatalf("the ghost read raised: %v", err)
	}
	if n := len(homedistLines(lines())); n != 2 {
		t.Fatalf("a handle the field never spawned added a HOMEDIST line (%d now)", n)
	}
}

// TestAFarIdleBodyNeverKillsItselfAndWalksHomeInstead is pass conditions (a) and (b) on
// the GAME'S OWN SCRIPT: the real `SCR_MON_AGGRESSIVE_TS_NONE` entry (mon_tactics_normal
// .scp:3 -> mon_ts_none.scp:1) is run for 60 s of its own time - the 1 s idle loop - on
// a body 8,000 units from its birth point with a `HomeSick TRUE` row.
//
//	(a) it never sends OM_KILL and its thread never ends: the body stays on the field.
//	(b) it still WANDERS HOME: every walk lands within the row's own move range of the
//	    birth point, which is the anchor the fix deliberately left alone.
func TestAFarIdleBodyNeverKillsItselfAndWalksHomeInstead(t *testing.T) {
	dir := corpusDir(t)

	h := New()
	t.Cleanup(h.Close)
	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if _, err := h.Index(dir); err != nil {
		t.Fatalf("indexing %s: %v", dir, err)
	}
	if err := h.LoadLibraries(); err != nil {
		// The AI entries do not need the PC libraries; a missing one is not this card.
		t.Logf("script libraries did not load (%v) - the AI entry is what is under test", err)
	}

	// The body's AI row, as the client table has it for `MON_AGGRESSIVE` + HomeSick:
	// an idle, aggressive body that wanders LOCALLY around its birth point.
	world := &fakeHomeWorld{
		home:  MonsterHome{X: 0, Y: 0, Z: 29},
		dist:  homeSickDist,
		known: true,
		options: map[string]MonsterOption{
			"SeekRange":  {Num: 0},
			"Filter":     {Text: "None", IsText: true},
			"LifeTime":   {Num: 0},
			"AIType":     {Text: "NORMAL", IsText: true},
			"Attack":     {Text: "TRUE", IsText: true},
			"AttackHome": {Text: "FALSE", IsText: true},
			"Move":       {Text: "RANDOMLOCAL", IsText: true},
			"HomeSick":   {Text: "TRUE", IsText: true},
			"Rally":      {Text: "FALSE", IsText: true},
			"AutoLock":   {Text: "FALSE", IsText: true},
		},
	}
	field := &fakeFarField{}
	h.Monster = world
	h.Near = field
	h.Hate = field
	h.Seek = field
	h.Order = field

	think := MonsterThink{
		Handle: homeSickHandle,
		Entry:  MonsterAIEntry("MON_AGGRESSIVE", "TS_NONE"),
		Self: MonsterSelf{
			Handle: homeSickHandle, ClassID: 11600, ClassName: "Monster_Bigfoot",
			Name: "Bigfoot", Lv: 10,
			X: homeSickDist, Y: 0, Z: 29,
			HP: 100, MHP: 100, MSPD: 100, MONMSPD: 100, RaceType: "NONE",
			// MSPD and MON_MSPD are two properties (`self.MSPD` the row's column,
			// `self.MON_MSPD` the computed SCR_Get_MON_MSPD): the wander range below is
			// drawn from MON_MSPD, and this fixture's row has no MSPDFix and no MSPD_BM,
			// so both are 100.
		},
	}
	if think.Entry != "SCR_MON_AGGRESSIVE_TS_NONE" {
		t.Fatalf("the dispatch name is %q", think.Entry)
	}

	// 60 resumes one second apart: the entry's own `sleep(1000)` sets the period, so this
	// is 60 passes of the loop that carries the home-sick check.
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 60; i++ {
		h.ThinkMonster(think, t0.Add(time.Duration(i)*time.Second))
	}

	// (a) THE SELF-KILL NEVER FIRED, and the body never left: no OM_KILL reached the
	// field, and the thread is still the entry it started - a body the server killed, or
	// a script that raised, would have ended it.
	if len(field.kills) != 0 {
		t.Fatalf("%d OM_KILL(s) from a body 8000 from its birth point: live does not fire "+
			"this branch, so neither may we (handles %v)\n%s",
			len(field.kills), field.kills, strings.Join(lines, "\n"))
	}
	if entry, running := h.MonsterThreadEntry(homeSickHandle); !running || entry != think.Entry {
		t.Fatalf("the body's idle thread is not running its entry any more (entry %q, "+
			"running %v) - 60 s of idling must not take the body off the field:\n%s",
			entry, running, strings.Join(lines, "\n"))
	}
	if failed := strings.Join(lines, "\n"); strings.Contains(failed, "FAILED") {
		t.Fatalf("the game's own idle entry raised:\n%s", failed)
	}

	// (b) IT WALKS HOME. `RANDOMLOCAL` draws within the row's own `moveMax` of
	// `m_createdPos` (MonsterNPC.cpp:120-133) - MSPD*3.0 = 300 here - so every walk has
	// to land within 300 of the BIRTH POINT, 8,000 units from where the body stands.
	moveMax := 100 * 3.0
	if len(field.moves) == 0 {
		t.Fatalf("the body never wandered in 60 idle seconds - the wander anchor is gone:\n%s",
			strings.Join(lines, "\n"))
	}
	for _, mv := range field.moves {
		if mv.mon != homeSickHandle {
			t.Fatalf("a walk was ordered for handle 0x%08x, want 0x%08x", mv.mon, homeSickHandle)
		}
		if d := math.Hypot(mv.x, mv.y); d > moveMax+0.001 {
			t.Fatalf("a walk landed %g units from the birth point, want at most %g "+
				"(the birth point is the wander's centre) - point (%g, %g)",
				d, moveMax, mv.x, mv.y)
		}
		// The counterfactual, asserted: a wander centred on the BODY would land within
		// 300 of where it stands. Every walk here is thousands of units from it, which is
		// what "the anchor is still the birth point" means.
		if d := math.Hypot(mv.x-homeSickDist, mv.y); d < 5000 {
			t.Fatalf("a walk landed %g units from the BODY, want thousands - the wander "+
				"is anchored at the body, not at its birth point - point (%g, %g)",
				d, mv.x, mv.y)
		}
		if mv.z != 29 {
			t.Fatalf("a walk was ordered at height %g, want the birth point's own 29", mv.z)
		}
	}

	// (c) THE RECORD SHOWS WHAT WAS MEASURED. One line, the true 8,000, and the choice
	// named - proof the branch WAS reached and answered live's way, not merely missed.
	got := homedistLines(lines)
	if len(got) != 1 {
		t.Fatalf("%d HOMEDIST line(s) from 60 idles, want 1:\n%s",
			len(got), strings.Join(lines, "\n"))
	}
	if !strings.Contains(got[0], "0x20001234 Monster_Bigfoot is 8000") ||
		!strings.Contains(got[0], "no self-kill (live does not fire this)") {
		t.Fatalf("the HOMEDIST line reads %q", got[0])
	}
}
