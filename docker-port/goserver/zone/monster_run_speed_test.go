package zone

// THE RUN SPEED - the speed a body carries while it is ENGAGED.
//
// The rule, measured 2026-11-08 on five live bodies (history\combat.md, "A monster's RUN
// speed is the row's SPDBM, not RSPD"):
//
//	runSpeed = floor(MSPD * (MSPDFix*0.01 + 1) * (100 + SPDBM) / 100)
//
// A body with NO engagement keeps `SCR_Get_MON_MSPD` (MSPD with `MSPD_BM`), which is
// what a script `Move` and the walk home after a chase gave up carry. A body with one -
// a chase following a player, or an attack order walking it into reach - carries the
// run speed, and every change is ONE `zone: MONSTER RUN` line.
//
// The two rows the card names are used through the REAL table (a hand-built `MonsterRow`
// with the XML's own columns): 11483 Monster_RionHalfmoonBear (131 / SPDBM 198 -> 390)
// and 10093 Monster_Boar (91 / SPDBM 298 -> 362). The XML test at the bottom reads the
// same columns off the client's file, so the fixture cannot drift from the data silently.

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const (
	runClassBear = uint32(11483) // Monster_RionHalfmoonBear: MSPD 131, SPDBM 198 -> 390
	runClassBoar = uint32(10093) // Monster_Boar:           MSPD 91,  SPDBM 298 -> 362
)

// runBearRow is class 11483's row, copied from ge\xml\datatable_monster.xml: MSPD 131,
// MSPDFix 0, MSPD_BM 0, SPDBM 198, MinR 1 / MaxR 135, Tactics MON_AGGRESSIVERUN
// (AIType AGGRESSIVE, SeekRange 1500). The XML test below reads the file itself.
func runBearRow() MonsterRow {
	return MonsterRow{
		ID: runClassBear, Name: "Monster_RionHalfmoonBear", Level: 64, Size: "M",
		RaceType: "Creature", SizeType: "LARGE",
		Wpn: 51, STR: 11.9, MinR: 1, MaxR: 135, ASPD: 3400, Hits: 1,
		MSPD: 131, MSPDFix: 0, MSPDBM: 0, SPDBM: 198,
		AIType: "AGGRESSIVE", AIKnown: true, AIAttack: true, SeekRange: 1500,
	}
}

// runBoarRow is class 10093's row: MSPD 91, SPDBM 298, MinR 0 / MaxR 100, Tactics
// MON_ELITE (AIType ELITE - it does NOT chase on its own, so its run is driven through
// the script's attack order, which is the OTHER engagement the card names).
func runBoarRow() MonsterRow {
	return MonsterRow{
		ID: runClassBoar, Name: "Monster_Boar", Level: 4, Size: "S",
		RaceType: "Creature", SizeType: "SMALL",
		Wpn: 31, STR: 3.9, MinR: 0, MaxR: 100, ASPD: 2067, Hits: 1,
		MSPD: 91, MSPDFix: 0, MSPDBM: 0, SPDBM: 298,
		AIType: "ELITE", AIKnown: true, AIAttack: true, SeekRange: 900,
	}
}

// runZone is one body of `row` and one player, in the roster, with the sockets cleared
// of the join burst. It is `chaseZone` with the row and class given, because the fixture
// here is the ROW's numbers and not Bigfoot's.
func runZone(t *testing.T, row MonsterRow, classID uint32, victimAt [3]int32) (*Handler, *bytes.Buffer, *player, Monster) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Plan = directPlan
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{pt[0], pt[1], 29}, 26, true
	}
	h.Monsters = newMonsters(map[uint32]MonsterRow{classID: row})

	victim := newPlayer(0x40000101, "Victim", victimAt)
	victim.sess.Team[0].HP = swingVictimHP
	victim.sess.Team[0].MaxHP = swingVictimHP
	victim.sess.Team[0].Stance = 10560

	h.joinZone(victim.sess)
	cullPlace(h, victim, victimAt)
	victim.conn.writes = nil

	mon := Monster{
		Handle: MonsterHandle(9),
		X:      0, Y: 0, Cell: 29,
		ClassID: classID, Level: row.Level,
		ATK: monsterATK(row), MinR: row.MinR, MaxR: row.MaxR, ASPD: monsterASPD(row),
		AssaultRange:    monsterAssaultRange(row.AI()),
		KeepReturnRange: monsterKeepReturnRange(row.AIType),
		ChaseSpeed:      monsterChaseSpeed(row.AIType, row),
	}
	h.spawnedMonster = []Monster{mon}
	h.monsterHP = map[uint32]float64{mon.Handle: swingMonsterHP}
	return h, logs, victim, mon
}

// lastMoveSpeed answers the speed of the most recent 3550 on a connection.
func lastMoveSpeed(t *testing.T, conn *recordConn) float32 {
	t.Helper()
	moves := movesOf(conn)
	if len(moves) == 0 {
		t.Fatalf("no 3550 on the wire")
	}
	_, _, speed := decodeMonsterMove(t, moves[len(moves)-1])
	return speed
}

// TestAChasingMonsterRunsAtTheRowsSPDBM is pass condition (a) for the GO CHASE: class
// 11483 (MSPD 131, SPDBM 198) acquires a player and its first 3550 carries 390, not
// 131 - floor(131 * (100+198) / 100) = floor(390.38) - and ONE MONSTER RUN line names
// the change.
func TestAChasingMonsterRunsAtTheRowsSPDBM(t *testing.T) {
	h, logs, victim, _ := runZone(t, runBearRow(), runClassBear, [3]int32{900, 0, 29})

	start := time.Now()
	h.monsterChaseTick(start)

	if got := lastMoveSpeed(t, victim.conn); got != 390 {
		t.Fatalf("the chasing body's 3550 carries %g u/s, want 390 - floor(131 * "+
			"(100+198) / 100) = floor(390.38), the row's SPDBM run speed:\n%s", got, logs.String())
	}
	line := "zone: MONSTER RUN 0x20000009 class 11483 MSPD 131 -> 390 (SPDBM 198)"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("%d MONSTER RUN line(s), want exactly 1 %q:\n%s", n, line, logs.String())
	}
}

// TestAnApproachingMonsterRunsAtTheRowsSPDBM is pass condition (a) for the SCRIPT'S
// APPROACH: class 10093 (MSPD 91, SPDBM 298) does not chase on its own (MON_ELITE is
// not the AGGRESSIVE family), so the script orders an attack and the engine's approach
// walks at floor(91 * 3.98) = 362 - ONE rule, the same helper the go chase asks.
func TestAnApproachingMonsterRunsAtTheRowsSPDBM(t *testing.T) {
	h, logs, victim, mon := runZone(t, runBoarRow(), runClassBoar, [3]int32{600, 0, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
	h.monsterApproachTick(time.Now())

	if got := lastMoveSpeed(t, victim.conn); got != 362 {
		t.Fatalf("the approaching body's 3550 carries %g u/s, want 362 - floor(91 * "+
			"(100+298) / 100) = floor(362.18), the row's SPDBM run speed:\n%s", got, logs.String())
	}
	line := "zone: MONSTER RUN 0x20000009 class 10093 MSPD 91 -> 362 (SPDBM 298)"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("%d MONSTER RUN line(s), want exactly 1 %q:\n%s", n, line, logs.String())
	}
}

// TestAMonsterWithNoEngagementKeepsItsMSPD is pass condition (b): the SAME row as the
// chase test, told to walk by the script's own `Move` - no engagement, so the 3550
// carries the row's MSPD 131 and no MONSTER RUN line is written at all.
func TestAMonsterWithNoEngagementKeepsItsMSPD(t *testing.T) {
	h, logs, victim, mon := runZone(t, runBearRow(), runClassBear, [3]int32{900, 0, 29})
	host := orderHost(t, h)

	src := `Move({ Handle = ` + luaNum(mon.Handle) + ` }, 500, 800, 26)`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}
	if got := lastMoveSpeed(t, victim.conn); got != 131 {
		t.Fatalf("a script Move with no engagement carries %g u/s, want the row's MSPD 131 "+
			"(SPDBM 198 applies only to an engaged body):\n%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "MONSTER RUN") {
		t.Fatalf("an idle body wrote a MONSTER RUN line - there was no change:\n%s", logs.String())
	}
}

// TestTheChaseDropsBackToMSPDWhenTheEngagementEnds is pass condition (c) for the chase:
// the same body runs at 390 while it follows the player, the player dies, and the walk
// HOME is the no-engagement MSPD 131 - with the MONSTER RUN line closed out, numbers
// reversed, exactly once.
func TestTheChaseDropsBackToMSPDWhenTheEngagementEnds(t *testing.T) {
	h, logs, victim, _ := runZone(t, runBearRow(), runClassBear, [3]int32{900, 0, 29})

	start := time.Now()
	h.monsterChaseTick(start)
	if got := lastMoveSpeed(t, victim.conn); got != 390 {
		t.Fatalf("the chase did not start at 390 (got %g):\n%s", got, logs.String())
	}
	// One step of the leg, so the body is off its home point and the walk back is real.
	h.monsterChaseTick(start.Add(pollInterval))
	victim.sess.Team[0].HP = 0
	h.monsterChaseTick(start.Add(2 * pollInterval))

	moves := movesOf(victim.conn)
	if len(moves) != 2 {
		t.Fatalf("%d x 3550, want 2 (the chase, then the walk home):\n%s", len(moves), logs.String())
	}
	_, goal, speed := decodeMonsterMove(t, moves[1])
	if speed != 131 {
		t.Fatalf("the walk home carries %g u/s, want the row's MSPD 131:\n%s", speed, logs.String())
	}
	if goal != [3]int32{0, 0, 29} {
		t.Fatalf("the walk home goes to %v, want the engagement's start (0, 0, 29)", goal)
	}
	line := "zone: MONSTER RUN 0x20000009 class 11483 MSPD 390 -> 131 (SPDBM 198)"
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("%d back-to-walk line(s), want exactly 1 %q:\n%s", n, line, logs.String())
	}
}

// TestTheApproachDropsBackToMSPDWhenTheEngagementEnds is pass condition (c) for the
// SCRIPT'S APPROACH, over the three endings the card names - the target dies, StopAttack
// parks the body, the body arrives inside its reach - plus the fourth the engine has: a
// script Move replaces the walk. Every one closes the line once, and the next walk is
// the row's MSPD again.
func TestTheApproachDropsBackToMSPDWhenTheEngagementEnds(t *testing.T) {
	cases := []struct {
		name string
		end  func(h *Handler, victim *player, mon Monster)
	}{
		{"the target dies", func(h *Handler, victim *player, mon Monster) {
			victim.sess.Team[0].HP = 0
		}},
		{"StopAttack", func(h *Handler, victim *player, mon Monster) {
			h.StopMonsterAttack(mon.Handle)
		}},
		{"arrival", func(h *Handler, victim *player, mon Monster) {
			// Put the player on the body itself: one unit is inside every reach.
			body := h.runtimeMonsters()[0]
			cullPlace(h, victim, [3]int32{body.X + 1, body.Y, 29})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, logs, victim, mon := runZone(t, runBoarRow(), runClassBoar, [3]int32{600, 0, 29})
			h.MonsterAIScript = true
			h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
			h.monsterApproachTick(time.Now())

			if got := lastMoveSpeed(t, victim.conn); got != 362 {
				t.Fatalf("the approach did not start at 362 (got %g):\n%s", got, logs.String())
			}

			tc.end(h, victim, mon)
			h.monsterApproachStep(mon.Handle, time.Now())

			line := "zone: MONSTER RUN 0x20000009 class 10093 MSPD 362 -> 91 (SPDBM 298)"
			if n := strings.Count(logs.String(), line); n != 1 {
				t.Fatalf("%d back-to-walk line(s), want exactly 1 %q:\n%s", n, line, logs.String())
			}

			// AND THE NUMBER IS BACK: the script's next walk is the row's MSPD 91.
			host := orderHost(t, h)
			src := `Move({ Handle = ` + luaNum(mon.Handle) + ` }, 500, 800, 26)`
			if err := host.L.DoString(src); err != nil {
				t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
			}
			if got := lastMoveSpeed(t, victim.conn); got != 91 {
				t.Fatalf("the walk after the approach carries %g u/s, want the row's MSPD 91:\n%s",
					got, logs.String())
			}
		})
	}
}

// TestTheRunRuleIsTheRowsSPDBM pins the rule against the CLIENT'S OWN FILE: the loader
// carries the `SPDBM` attribute (it is a different column from `MSPD_BM`), and the
// formula lands on the number each of the five 2026-11-08 bodies was measured at.
func TestTheRunRuleIsTheRowsSPDBM(t *testing.T) {
	table, err := LoadMonsters(XMLDir())
	if err != nil {
		t.Skipf("monster table not readable (%v)", err)
	}
	cases := []struct {
		id    uint32
		walk  float64 // SCR_Get_MON_MSPD
		spdbm float64
		run   float64 // what the wire carried
	}{
		{10093, 91, 298, 362},  // Boar
		{10281, 121, 20, 145},  // Jaquin Comodo
		{11212, 202, 68, 339},  // Milhou Elite
		{11483, 131, 198, 390}, // Rion Halfmoon Bear
		{10071, 150, 220, 480}, // Hornet
	}
	for _, want := range cases {
		row, ok := table.Row(want.id)
		if !ok {
			t.Skipf("class %d is not in this client's table", want.id)
		}
		if row.SPDBM != want.spdbm {
			t.Fatalf("class %d SPDBM = %g, want %g - the RUN column, not MSPD_BM (%g)",
				want.id, row.SPDBM, want.spdbm, row.MSPDBM)
		}
		if got := monsterMSPD(row); got != want.walk {
			t.Fatalf("class %d SCR_Get_MON_MSPD = %g, want %g", want.id, got, want.walk)
		}
		if got := monsterRunSpeed(row); got != want.run {
			t.Fatalf("class %d run speed = %g, want %g - floor(%g * (100+%g) / 100)",
				want.id, got, want.run, row.MSPD, row.SPDBM)
		}
	}
}
