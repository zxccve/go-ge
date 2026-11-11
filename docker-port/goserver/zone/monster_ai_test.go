package zone

// THE MONSTER-AI ROW DECIDES WHETHER A BODY CHASES AND SWINGS.
//
// The owner's report: "still not chasing" - his Bigfoot stood still with a player
// next to it. The chase code was right; the body was spawned PASSIVE. Both probes
// passed `Tactics: "MON_MISSION_84"` (AIType="None" Attack="FALSE" SeekRange="0" - a
// mission prop) over the row's own `MON_AGGRESSIVE`, and the engage radius was the
// ATTACKER profile's AssaultRange 1000 instead of the tactics row's SeekRange 2500.
//
// The four cases here are the card's pass conditions:
//
//	(a) an EMPTY Tactics takes the row's MON_AGGRESSIVE and acquires at the row's
//	    own SeekRange 2500;
//	(b) a spawn that NAMES MON_MISSION_84 never acquires and never swings;
//	(c) an Attack="FALSE" row never swings even with a player inside MaxR;
//	(d) the chase's own tests keep their meaning (zone\monster_chase_test.go).
//
// THE SPAWN IS THE REAL ONE: `SpawnMonsterObject` with the shipped
// `datatable_monster*.xml` and `datatable_monsterai.xml`, and a NavNear that answers
// one fixed mesh spot (the spawn needs a path engine; these cases are not about the
// mesh).

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// bigfootRowID is class 11591 Monster_Bigfoot: Tactics="MON_AGGRESSIVE", MaxR 306.
const bigfootRowID = int32(11591)

// aiSpawnZone is a zone that can run the REAL SpawnMonsterObject for a Bigfoot: the
// shipped Monster table, a mesh that answers (0, 0, cell 29) for any ask inside the
// spawn ranges, and the sight/stance tables castZone already builds.
func aiSpawnZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Monsters = realMonsters(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		if hRange != spawnHRange || vRange != spawnVRange {
			return [3]int32{}, 0, false
		}
		return [3]int32{0, 0, 29}, 29, true
	}
	return h, logs
}

// aiVictim puts one living player character where the case wants its BODY, with the
// same cullPlace the chase tests use so the sight ring and the reach read the point
// the case placed rather than the roster's destination.
func aiVictim(t *testing.T, h *Handler, at [3]int32) *player {
	t.Helper()
	v := newPlayer(0x40000101, "Seeker", at)
	v.sess.Team[0].HP = swingVictimHP
	v.sess.Team[0].MaxHP = swingVictimHP
	v.sess.Team[0].Stance = 10560
	h.joinZone(v.sess)
	cullPlace(h, v, at)
	v.conn.writes = nil
	return v
}

// aiSpawned answers the one body the spawn put in the roster, and fails when there
// is not exactly one.
func aiSpawned(t *testing.T, h *Handler) Monster {
	t.Helper()
	made := h.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) stored, want 1", len(made))
	}
	return made[0]
}

// TestSpawnUsesTheRowsOwnTacticsAndSeekRange is (a): an empty Tactics spawn takes
// the row's own MON_AGGRESSIVE, and the acquisition radius is that tactics row's
// SeekRange 2500 - not the ATTACKER profile's 1000 the chase first used.
//
// The behavioural half is the one that matters: a player at 1500 is outside the old
// AssaultRange 1000 AND inside the leash (KeepReturnRange 2000), so a body that still
// used the old constant would acquire nothing and send no 3550.
func TestSpawnUsesTheRowsOwnTacticsAndSeekRange(t *testing.T) {
	h, logs := aiSpawnZone(t)
	h.Plan = directPlan

	handle, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: bigfootRowID, X: 0, Y: 0, Count: 1, MHP: 30000,
	})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	m := aiSpawned(t, h)
	if m.Tactics != "MON_AGGRESSIVE" {
		t.Fatalf("handle %d runs tactics %q, want the ROW's own MON_AGGRESSIVE - an "+
			"empty override must not change the body's behaviour", handle, m.Tactics)
	}
	if m.AssaultRange != 2500 {
		t.Fatalf("the body acquires at %g, want the tactics row's SeekRange 2500 "+
			"(MON_AGGRESSIVE: AIType AGGRESSIVE Attack TRUE SeekRange 2500)",
			m.AssaultRange)
	}
	if m.AINoAttack {
		t.Fatalf("the body's tactics row says Attack=TRUE, but the spawn marked it " +
			"as never swinging")
	}
	want := `zone: MONSTER AI class 11591 tactics "MON_AGGRESSIVE" AIType ` +
		`AGGRESSIVE attack yes seek 2500`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the spawn never logged its AI decision; want a line containing\n  %s\n"+
			"got:\n%s", want, logs.String())
	}

	victim := aiVictim(t, h, [3]int32{1500, 0, 29})
	start := time.Now()
	h.monsterChaseTick(start)

	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 for a player 1500 away from an AGGRESSIVE Bigfoot, want 1: "+
			"1500 is outside the old AssaultRange 1000 and inside the tactics row's "+
			"SeekRange 2500 - log:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "inside the monster-AI row's SeekRange 2500") {
		t.Fatalf("the acquisition line did not name the tactics row's radius:\n%s",
			logs.String())
	}
}

// TestMissionPropOverrideNeverAcquiresOrSwings is (b): a spawn that NAMES
// MON_MISSION_84 - what both probes did - is a passive mission prop. MON_MISSION_84
// says AIType="None" Attack="FALSE" SeekRange="0", so the body has no seek radius,
// no pace, and no swing.
//
// THE PLAYER IS INSIDE MaxR on purpose: at (100, 0) with the Bigfoot's own MaxR 306
// and both body radii 50, the edge gap is 0. A body that still swung would fire; the
// tactics row says it must not.
func TestMissionPropOverrideNeverAcquiresOrSwings(t *testing.T) {
	h, logs := aiSpawnZone(t)
	h.Plan = directPlan

	handle, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: bigfootRowID, X: 0, Y: 0, Count: 1, MHP: 30000,
		Tactics: "MON_MISSION_84",
	})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	m := aiSpawned(t, h)
	if m.Tactics != "MON_MISSION_84" {
		t.Fatalf("handle %d runs tactics %q, want the script's override "+
			"MON_MISSION_84 - a named tactics must win over the row", handle, m.Tactics)
	}
	if m.AssaultRange != 0 || m.ChaseSpeed != 0 {
		t.Fatalf("the mission prop acquired at %g and walks at %g, want 0 and 0 "+
			"(SeekRange 0, AIType None)", m.AssaultRange, m.ChaseSpeed)
	}
	if !m.AINoAttack {
		t.Fatalf("MON_MISSION_84 says Attack=\"FALSE\" but the spawn did not bar the " +
			"body from swinging")
	}
	if m.MaxR <= 0 || m.ATK <= 0 || m.ASPD <= 0 {
		t.Fatalf("the fixture is not a body that COULD swing: MaxR %g, ATK %g, "+
			"ASPD %g - the test would pass for the wrong reason", m.MaxR, m.ATK, m.ASPD)
	}
	want := `zone: MONSTER AI class 11591 tactics "MON_MISSION_84" AIType None ` +
		`attack no seek 0`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the spawn never logged the passive decision; want a line containing\n"+
			"  %s\ngot:\n%s", want, logs.String())
	}

	victim := aiVictim(t, h, [3]int32{100, 0, 29})
	start := time.Now()
	for i := int64(1); i*int64(pollInterval) <= int64(3*time.Second); i++ {
		at := start.Add(time.Duration(i) * pollInterval)
		h.monsterChaseTick(at)
		h.monsterSwingTick(at)
	}

	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 from a MON_MISSION_84 body, want 0 - a mission prop "+
			"does not chase\n%s", n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 from a MON_MISSION_84 body with a player inside MaxR %g, "+
			"want 0 - Attack=\"FALSE\"\n%s", n, m.MaxR, logs.String())
	}
	if len(h.monsterChases) != 0 {
		t.Fatalf("the chase holds %d engagement(s) for a body that may not acquire",
			len(h.monsterChases))
	}
}

// TestAttackFalseRowNeverSwings is (c), with its own control: the SAME body, inside
// the same reach, swings the moment the tactics gate is lifted. That is what makes
// the refusal the flag's doing rather than the reach's.
func TestAttackFalseRowNeverSwings(t *testing.T) {
	victimAt := [3]int32{0, 150, 29} // centre 150: 50 of clear ground inside MaxR 100
	h, logs, victim, _, mon := swingZone(t, victimAt, [3]int32{0, 3000, 29})

	h.spawnedMonster[0].AINoAttack = true
	start := time.Now()
	h.monsterSwingTick(start)
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 from a body whose tactics row says Attack=\"FALSE\", "+
			"want 0 - the player is inside MaxR %g on purpose\n%s",
			n, mon.MaxR, logs.String())
	}

	h.spawnedMonster[0].AINoAttack = false
	h.monsterSwingTick(start.Add(2 * pollInterval))
	if n := len(swingsOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3564 after lifting the tactics gate, want 1 - the control "+
			"proves the refusal above was the gate, not the reach\n%s",
			n, logs.String())
	}
}
