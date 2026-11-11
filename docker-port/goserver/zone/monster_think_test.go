package zone

// THE THINK TURN'S TESTS - the card's pass conditions (b), (c) and (d).
//
//   (b) with -monai script a monster standing near a player ACQUIRES it and SWINGS,
//       proved through the SCRIPT'S OWN CALLS: the NONE entry's `GetObjectLocaly` ->
//       `InsertHate` -> `ChangeTactics(TS_BATTLE)`, then the BATTLE entry's
//       `StartAttack`, then the existing swing path's 3564.
//   (c) a body whose script FAILS logs ONCE and does not hang the tick.
//   (d) the startup line names the mode.
//
// (a) - "with -monai go the existing chase/swing suites pass UNCHANGED" - is not a test
// in this file: it is the chase and swing suites themselves, untouched, and the default
// they run on (`Handler.MonsterAIScript` false).
//
// THE CORPUS IS THE REAL ONE (`out/lua_corpus`, built by `src\scp_to_lua.py --dump`), so
// these tests run the game's own mon_tactics_normal.scp / mon_ts_none.scp / mon_ts_battle.scp
// and not a hand-written stand-in. A corpus that is missing is a SKIP with the rebuild
// command, never a pass: a green test here that ran no script would be worse than none.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/script"
)

// monaiCorpus is the translated corpus, relative to zone\ - the same folder the zone
// indexes at startup and the one `-lua` defaults to.
const monaiCorpus = "../../out/lua_corpus"

// monaiZone builds a zone with ONE Bigfoot, ONE player, and the REAL script host wired
// the way main.go wires it.
//
// The monster carries the columns the reference's dispatch needs and nothing invented:
// `Tactics="MON_AGGRESSIVE"` (datatable_monster.xml), the AI row the chase fixture
// already pins (AIType AGGRESSIVE, Attack TRUE, SeekRange 2500), a live HP ledger, and
// the reach/cadence/power the swing suite uses.
func monaiZone(t *testing.T, victimAt [3]int32, scriptMode bool) (*Handler, *bytes.Buffer, *bytes.Buffer, *player, Monster, *script.Host) {
	t.Helper()
	return monaiZoneIn(t, victimAt, scriptMode, monaiCorpus, "MON_AGGRESSIVE")
}

// monaiZoneIn is monaiZone with the corpus and the body's `Tactics` column chosen by the
// caller, so the failure case can run a fixture script instead of the game's own.
func monaiZoneIn(t *testing.T, victimAt [3]int32, scriptMode bool, corpus, tactics string) (*Handler, *bytes.Buffer, *bytes.Buffer, *player, Monster, *script.Host) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID

	row := chaseRow()
	row.Tactics = tactics
	h.Monsters = newMonsters(map[uint32]MonsterRow{chaseClassID: row})

	victim := newPlayer(0x40000101, "Victim", victimAt)
	victim.sess.Team[0].HP = swingVictimHP
	victim.sess.Team[0].MaxHP = swingVictimHP
	victim.sess.Team[0].Stance = 10560
	h.joinZone(victim.sess)
	cullPlace(h, victim, victimAt)
	victim.conn.writes = nil

	mon := Monster{
		Handle:  MonsterHandle(7),
		X:       0, Y: 0, Cell: 29,
		ClassID: chaseClassID, Level: 111, MaxHP: swingMonsterHP,
		Tactics:  tactics,
		RaceType: "Creature",
		ATK:      swingMonsterATK, MinR: 1, MaxR: 306, ASPD: swingCadenceMS,
		ChaseSpeed: 143,
	}
	h.spawnedMonster = []Monster{mon}
	h.monsterHP = map[uint32]float64{mon.Handle: swingMonsterHP}

	// THE HOST IS BUILT THE WAY clickEngine BUILDS IT: index the corpus, load the shared
	// libraries, and point the log at a buffer this test reads.
	scriptLogs := &bytes.Buffer{}
	host := script.New()
	host.Log = func(format string, args ...any) {
		fmt.Fprintf(scriptLogs, format+"\n", args...)
	}
	if _, err := host.Index(corpus); err != nil {
		t.Skipf("the translated corpus is not at %s (%v) - rebuild it with: "+
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus", corpus, err)
	}
	if err := host.LoadLibraries(); err != nil {
		t.Logf("script libraries did not load (%v) - the AI entries do not need them", err)
	}

	// THE WIRE-UP main.go does, name for name.
	host.Hate = h
	host.Monster = h
	host.Order = h
	host.Seek = h
	h.MonsterBrain = host
	h.MonsterAIScript = scriptMode
	return h, logs, scriptLogs, victim, mon, host
}

// TestMonsterAICorpusCarriesTheDispatchChain proves the corpus the runner needs is really
// there: the zone asks for `SCR_MON_AGGRESSIVE_TS_BATTLE` (the reference's sprintf,
// MonsterNPC.cpp:1116-1118), that wrapper is in mon_tactics_normal, and it calls the
// card's function.
func TestMonsterAICorpusCarriesTheDispatchChain(t *testing.T) {
	_, _, _, _, _, host := monaiZone(t, [3]int32{0, 150, 29}, true)

	for _, name := range []string{
		"SCR_MON_AGGRESSIVE_TS_NONE",
		"SCR_MON_AGGRESSIVE_TS_BATTLE",
		"SCR_MON_AI_LIB_TS_NONE",
		"SCR_MON_AI_LIB_TS_BATTLE",
	} {
		if !host.HasScript(name) {
			t.Fatalf("%s is not defined by any corpus file - the runner cannot dispatch "+
				"a MON_AGGRESSIVE body's state", name)
		}
	}

	blob, err := os.ReadFile(filepath.Join(monaiCorpus, "monster", "mon_tactics_normal.lua"))
	if err != nil {
		t.Fatalf("read mon_tactics_normal.lua: %v", err)
	}
	if !bytes.Contains(blob, []byte("SCR_MON_AI_LIB_TS_BATTLE(self)")) {
		t.Fatalf("mon_tactics_normal.lua no longer calls SCR_MON_AI_LIB_TS_BATTLE - the " +
			"card's function is not reachable through the reference's dispatch")
	}
}

// TestMonsterAIGoModeRunsNoScript is the switch's own proof: with the flag OFF (the
// default) three think passes change NOTHING - no thread, no tactic, no hate, no attack
// and no packet. The chase/swing suites cover the rest of go mode, untouched.
func TestMonsterAIGoModeRunsNoScript(t *testing.T) {
	h, logs, scriptLogs, victim, mon, host := monaiZone(t, [3]int32{0, 150, 29}, false)

	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3; i++ {
		h.monsterAITick(t0.Add(time.Duration(i) * 200 * time.Millisecond))
	}

	if _, running := host.MonsterThreadEntry(mon.Handle); running {
		t.Fatalf("a tactics thread is running with -monai go - the switch is not OFF")
	}
	if got := h.MonsterTactic(mon.Handle); got != "TS_NONE" {
		t.Fatalf("the body's tactics state is %q after three go-mode passes, want the "+
			"TS_NONE it was born in", got)
	}
	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("%d packet(s) reached the victim with -monai go, want 0", n)
	}
	for _, line := range []string{"zone: TACTICS 0x", "-> TS_", "zone: HATE", "zone: MONSTER ATTACK"} {
		if strings.Contains(logs.String(), line) {
			t.Fatalf("go mode wrote %q - nothing but the Go chase and swing may act:\n%s",
				line, logs.String())
		}
	}
	if strings.Contains(scriptLogs.String(), "MONSTER AI") {
		t.Fatalf("go mode ran a script:\n%s", scriptLogs.String())
	}
}

// TestMonsterAIScriptAcquiresAndSwings is pass condition (b): a body standing near a
// player, with -monai script, ACQUIRES it and SWINGS - and every step of that is a SCRIPT
// call this test can see.
//
// THE SEQUENCE IS THE REFERENCE'S, not a shortcut: a monster is born in TS_NONE
// (MonsterNPC.cpp:251-256), the NONE entry ACQUIRES (`GetObjectLocaly` -> `InsertHate` ->
// `SetTacticsTarget` -> `ChangeTactics('TS_BATTLE')`, mon_ts_none.scp:219-240), the state
// change starts the BATTLE thread (MonsterNPC.cpp:2433-2438), that entry calls
// `StartAttack` (mon_ts_battle.scp:162-165), and the zone's own swing tick sends the 3564.
func TestMonsterAIScriptAcquiresAndSwings(t *testing.T) {
	h, logs, scriptLogs, victim, mon, host := monaiZone(t, [3]int32{0, 150, 29}, true)

	t0 := time.Unix(1_700_000_000, 0)
	// 1. TS_NONE: the seek finds the player and the script acquires it.
	h.monsterAITick(t0)
	// 2. TS_BATTLE starts: the entry sleeps 100 ms at its first line.
	h.monsterAITick(t0.Add(100 * time.Millisecond))
	// 3. the battle loop: GetTopHatePointChar -> IsAttacking -> StartAttack.
	h.monsterAITick(t0.Add(200 * time.Millisecond))
	// 4. the engine's swing, on the same pass the attack was ordered.
	h.monsterSwingTick(t0.Add(200 * time.Millisecond))

	// THE TACTIC THE SCRIPT SET.
	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the body's tactics state after the NONE entry is %q, want TS_BATTLE "+
			"(the script's own ChangeTactics) - script log:\n%s\nzone log:\n%s",
			got, scriptLogs.String(), logs.String())
	}
	if !strings.Contains(logs.String(), "zone: TACTICS 0x20000007 \"Monster_Bigfoot\" -> TS_BATTLE") {
		t.Fatalf("no TACTICS line for the script's ChangeTactics:\n%s", logs.String())
	}

	// THE HATE INSERT the script made (GetObjectLocaly -> InsertHate(self, target, 20)).
	top, ok := h.TopHate(mon.Handle)
	if !ok {
		t.Fatalf("the body's hate list is empty - the NONE entry's seek did not insert "+
			"anything (script log:\n%s)", scriptLogs.String())
	}
	if top.Handle != victim.sess.Team[0].Handle {
		t.Fatalf("the hated target is 0x%08x, want the player 0x%08x", top.Handle,
			victim.sess.Team[0].Handle)
	}
	if !strings.Contains(logs.String(), "zone: HATE 0x20000007") {
		t.Fatalf("no HATE line for the insert:\n%s", logs.String())
	}

	// THE STARTATTACK REACHED THE ENGINE.
	if !strings.Contains(logs.String(), "zone: MONSTER ATTACK 0x20000007 \"Monster_Bigfoot\" START on") {
		t.Fatalf("the BATTLE entry did not call StartAttack (script log:\n%s)\n%s",
			scriptLogs.String(), logs.String())
	}

	// AND THE EXISTING SWING PATH SENT THE 3564 - same builder, same 74 bytes.
	sent := victim.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("the victim's socket holds %d packet(s), want exactly one 3564:\n%s",
			len(sent), logs.String())
	}
	f := decodeSwing(t, sent[0])
	if f.cmd != CmdZCAttackSingle || f.attacker != mon.Handle ||
		f.victim != victim.sess.Team[0].Handle {
		t.Fatalf("the swing is cmd %d attacker 0x%08x victim 0x%08x, want %d / 0x%08x / 0x%08x",
			f.cmd, f.attacker, f.victim, CmdZCAttackSingle, mon.Handle,
			victim.sess.Team[0].Handle)
	}

	// THE THREAD IS WHERE THE REFERENCE PUT IT: the BATTLE entry, parked in its loop.
	if entry, running := host.MonsterThreadEntry(mon.Handle); !running ||
		entry != "SCR_MON_AGGRESSIVE_TS_BATTLE" {
		t.Fatalf("the body's thread is %q (running %v), want SCR_MON_AGGRESSIVE_TS_BATTLE",
			entry, running)
	}
}

