package zone

// THE IDLE WANDER OF A BODY WHOSE TACTICS FAMILY DOES NOT CHASE - card t_c873ad60.
//
// THE MEASURED STOP. `fld_gig`, `-monai script`, zone up 2026-11-09: the Palm King Tree
// Crab (class 12981, tactics MON_GIGANTE, `Move="RANDOMLOCAL"`, SeekRange 0) opened
// `SCR_MON_GIGANTE_TS_NONE` and stood still. Its thread was alive (`TACTIC THREAD ... start`
// and no end) and NO move order was ever issued for its handle; the Giant Seastars
// (MON_GIGANTE4, SeekRange 2000) walked because their thread reached TS_BATTLE.
//
// THE GATE, and it is inside the game's own script. The NONE entry's move block is
//
//	if (math.random(0,99) > 50) then
//	    ... else if('RANDOMLOCAL' == moveType) then
//	        if('YES' == SCR_MON_AI_LIB_CHECK_ENABLEMOVE(self)) then
//	            RandomMoveCreatedLocaly(self, moveMax)
//
// (mon_ts_none.scp:275-296) and that helper's FIRST line is
//
//	if (self.MSPD == 0) { return 'NO'; }            // mon_ai_lib.scp:74-76
//
// - answered BEFORE `IsEnableMove` (this server's binding) is ever called, so the refusal
// could not appear in any Go-side log. `self.MSPD` was `Monster.ChaseSpeed`, and
// `monsterChaseSpeed` answers 0 for every tactics family that is not AGGRESSIVE*:
// MON_GIGANTE is `GIGANTE`, so the script was told the crab could not move at all while
// its own row says MSPD 217.
//
// THE TWO TESTS BELOW are the two halves of the card's pass condition: the body IS ordered
// to walk once the script is handed its row's numbers, and when it is genuinely refused the
// log says WHICH gate did it, once per body.

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// crabClassID is Monster_PamlTreeKingCrab, the body the owner watched stand still
// (datatable_monster.xml: ClassID 12981, Tactics="MON_GIGANTE", MSPD="217", MSPDFix="0",
// MSPD_BM="0").
const crabClassID = 12981

// crabRow is that row's numbers, with the AI row's own columns (datatable_monsterai.xml,
// MON_GIGANTE: AIType="GIGANTE" Attack="TRUE" SeekRange="0" Move="RANDOMLOCAL"
// MoveMin="0" MoveMax="500"). The AI row is NOT copied here - `aiMoveType` and
// `GetMonAiOption` read the shipped XML.
func crabRow() MonsterRow {
	return MonsterRow{
		ID: crabClassID, Name: "Monster_PamlTreeKingCrab", Level: 65, Size: "M", Radius: 50,
		RaceType: "Creature", SizeType: "SMALL",
		Wpn: 49, STR: 9.8, ATKBM: 0, MinR: 1, MaxR: 63, ASPD: 3267, ASPDBM: 0, Hits: 4,
		// MSPD 217, MSPDFix 0, MSPD_BM 0 -> SCR_Get_MON_MSPD = 217.
		MSPD: 217, MSPDFix: 0, MSPDBM: 0,
		AIType: "GIGANTE", AIKnown: true, AIAttack: true, SeekRange: 0,
	}
}

// crabZone is the think turn's own fixture (monaiZoneIn: the REAL corpus, one joined
// player, the host wired the way main.go wires it) with the crab's body and row in place
// of the Bigfoot's, and a mesh that answers any point - the same stand-in `orderZone` uses,
// so a wander is a wander and a refusal is a refusal.
//
// THE BODY CARRIES ChaseSpeed 0, which is what `monsterChaseSpeed("GIGANTE", row)` answers
// at spawn (zone\create.go:505) and the whole reason the script saw MSPD 0.
func crabZone(t *testing.T, row MonsterRow) (*Handler, *bytes.Buffer, *bytes.Buffer, *player, Monster) {
	t.Helper()
	h, logs, scriptLogs, victim, _, _ := monaiZoneIn(t, [3]int32{0, 150, 29}, true,
		monaiCorpus, "MON_GIGANTE")
	h.Monsters = newMonsters(map[uint32]MonsterRow{row.ID: row})
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{pt[0], pt[1], 29}, 26, true
	}
	body := &h.spawnedMonster[0]
	body.ClassID = row.ID
	body.Tactics = "MON_GIGANTE"
	body.ChaseSpeed = monsterChaseSpeed(row.AIType, row)
	body.AssaultRange = 0
	body.KeepReturnRange = 0
	victim.conn.writes = nil
	return h, logs, scriptLogs, victim, *body
}

// TestMonsterAIGigantWandersAroundItsBirthPoint is the card's walk half: a MON_GIGANTE
// body with nobody to fight is ORDERED to walk, at the row's own MON_MSPD, to a point
// within `moveMax = MON_MSPD*3` of its BIRTH point.
//
// RED WITHOUT THE FIX. With `self.MSPD` answered from `Monster.ChaseSpeed` this test fails
// at its first assertion: 0 x 3550 in 60 simulated seconds (measured on the zone itself:
// 0 `MONSTER MOVE` lines for 61 bodies and one `gets NO wander` line each).
func TestMonsterAIGigantWandersAroundItsBirthPoint(t *testing.T) {
	row := crabRow()
	if got := monsterChaseSpeed(row.AIType, row); got != 0 {
		t.Fatalf("monsterChaseSpeed(GIGANTE) = %g, want 0 - the premise of the card is that "+
			"the GO CHASE's speed copy is 0 for this family, so it cannot be the number the "+
			"script reads as self.MSPD", got)
	}
	h, logs, scriptLogs, victim, mon := crabZone(t, row)

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, nil, t0, 600) // 60 s at the zone's 100 ms poll

	moves := movesOf(victim.conn)
	if len(moves) == 0 {
		t.Fatalf("the crab was ordered to walk 0 times in 60 simulated seconds - the NONE "+
			"entry's move block refuses when `self.MSPD == 0` (mon_ai_lib.scp:74-76), so "+
			"the script must have been handed 0.\nscript log:\n%s\nzone log:\n%s",
			scriptLogs.String(), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != mon.Handle {
		t.Fatalf("the walk names handle 0x%08x, want the crab's 0x%08x", handle, mon.Handle)
	}
	if speed != 217 {
		t.Fatalf("the walk carries speed %g, want 217 - SCR_Get_MON_MSPD of the crab's row "+
			"(MSPD 217, no fix, no BM), the same number the 3550 always carries",
			speed)
	}
	// THE CENTRE IS THE BIRTH POINT and the ring is MON_MSPD*3.0 = 651 (mon_ts_none.scp:10),
	// not ChaseSpeed*3.0 = 0 and not 143*3.
	const moveMax = 217 * 3.0
	d := math.Hypot(float64(goal[0]), float64(goal[1]))
	if d > moveMax+1 {
		t.Fatalf("the walk's goal (%d, %d) is %.1f units from the birth point, want at "+
			"most %g - RANDOMLOCAL draws around m_createdPos within moveMax = MON_MSPD*3.0",
			goal[0], goal[1], d, moveMax)
	}
	// AND THE GATE LINE DID NOT FIRE: this body's row can walk, so no refusal is owed.
	if strings.Contains(logs.String(), "gets NO wander") {
		t.Fatalf("a body whose row says MSPD 217 was reported as refused a wander:\n%s",
			logs.String())
	}
	if !strings.Contains(logs.String(), "MONSTER MOVE 0x") {
		t.Fatalf("the zone stored a walk but wrote no MONSTER MOVE line:\n%s", logs.String())
	}
	t.Logf("%d walk(s) ordered in 60 s, the first to (%d, %d) at %g u/s, %.0f units from home",
		len(moves), goal[0], goal[1], speed, d)
}

// TestMonsterAIWanderRefusalNamesTheGate is the card's other half: when the script really
// cannot move a body, ONE line per body says WHICH gate refused it and WITH WHAT NUMBERS.
//
// THE BODY HERE GENUINELY CANNOT WALK - its row says MSPD 0, so `SCR_MON_AI_LIB_CHECK_
// ENABLEMOVE` is right to answer 'NO' on its first line - and the line has to say so
// without sending the reader to a debugger: the gate, its file:line, the number the script
// was handed, and the number the row carries.
func TestMonsterAIWanderRefusalNamesTheGate(t *testing.T) {
	row := crabRow()
	row.MSPD = 0 // the row itself says this body does not move
	row.Name = "Monster_PamlTreeKingCrab_Immovable"
	h, logs, _, victim, mon := crabZone(t, row)

	t0 := time.Unix(1_700_000_000, 0)
	drive(h, nil, t0, 40)

	lines := refusalLines(logs.String(), mon.Handle)
	if len(lines) != 1 {
		t.Fatalf("%d refusal line(s) for one body over 40 passes, want exactly 1 - the "+
			"gate is named once per body, never per pass:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		"gets NO wander",
		`Move="RANDOMLOCAL"`,
		"mon_ai_lib.scp:74-76",
		"self.MSPD == 0",
		"mon_ts_none.scp:275-296",
		"self.MSPD 0",
		"Monster row says MSPD 0",
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the refusal line does not name %q:\n%s", want, lines[0])
		}
	}
	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 for a body whose MSPD is 0, want 0", n)
	}
}

// refusalLines is every wander-refusal line the zone wrote for one handle.
func refusalLines(logText string, handle uint32) []string {
	needle := "MONSTER AI 0x" + hexHandle(handle)
	var out []string
	for _, line := range strings.Split(logText, "\n") {
		if strings.Contains(line, needle) && strings.Contains(line, "gets NO wander") {
			out = append(out, line)
		}
	}
	return out
}
