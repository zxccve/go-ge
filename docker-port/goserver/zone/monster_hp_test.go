package zone

// THE SPAWNED MONSTER'S MAX HP - the tests the card names.
//
// THE DEFECT THESE PIN. A monster spawned WITHOUT an explicit `MHP` was given hp 0:
// `SpawnMonsterObject` read `row.MHP` from an MHP attribute the Monster table does not
// have (`datatable_monster.xml` carries `HP`, `RHP`, `MHPFix`, `MHP_BM`, `CP_MON_MHP`
// and never `MHP`, so all 7,725 rows read 0). Two symptoms followed in game: the
// client drew the body but no target/HP frame (the 3527's HP fields were 0), and the
// body never swung (zone\monster_swing.go skips a monster whose ledger entry is
// missing or <= 0).
//
// THE FIX IS THE GAME'S OWN ENTRY POINT, `SCR_Get_MON_MHP` (zone\monster_hp.go), run
// on the same host that answers `SCR_Get_MON_DEF`. These tests read a REAL Monster row
// (class 11591, Monster_Bigfoot - the class the owner tested) out of the shipped XML,
// so the numbers are the table's, not typed in here.
//
// WHAT IS PROVED, and nothing wider:
//
//	A. a real row through the script answers MHP > 0 and a spawn with no MHP seeds the
//	   HP ledger with it;
//	B. an explicit spec.MHP still wins;
//	C. a script that cannot answer keeps 0, writes NO ledger entry, and logs ONCE;
//	D. `monsterSwingArmed` is true for a body made through the REAL spawn path and
//	   `monsterSwingTick` swings at a player in reach - the symptom the owner waits on;
//	E. the HP the 3527 carries equals the ledger, including a value wider than int32
//	   (the reference's ~1.0e12 Scarecrow HP).

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"geserver/script"
)

// bigfootClass is the row the owner's in-game report names: class 11591,
// "Monster_Bigfoot", Lv 111, CON 83.3, RaceType Creature - so the ordinary branch of
// SCR_Get_MON_MHP, and a max HP the table itself cannot state.
const bigfootClass = uint32(11591)

// realMonstersOnce shares ONE load of the shipped Monster table across the file: it
// is a 30 MB XML split across five files and every case here needs the same numbers.
var (
	realMonstersOnce  sync.Once
	realMonstersTable *MonsterTable
	realMonstersErr   error
)

func realMonsters(t *testing.T) *MonsterTable {
	t.Helper()
	realMonstersOnce.Do(func() {
		realMonstersTable, realMonstersErr = LoadMonsters(XMLDir())
	})
	if realMonstersErr != nil {
		t.Skipf("the Monster table is not readable at %s (%v) - these tests need a "+
			"real row, and a hand-built one would not prove the spawn path",
			XMLDir(), realMonstersErr)
	}
	return realMonstersTable
}

// testMonsterHPScript is ONE MonsterHPScript per call, on the suite's own runtime.
func testMonsterHPScript(t *testing.T) *MonsterHPScript {
	t.Helper()
	requireStatus(t)
	if testScriptHost == nil {
		t.Skip("no script host in this checkout: build the corpus with " +
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus")
	}
	hs, err := NewMonsterHPScript(testScriptHost, t.Logf)
	if err != nil {
		t.Fatalf("NewMonsterHPScript: %v (SCR_Get_MON_MHP must be in the corpus - "+
			"rebuild it with cd src && python scp_to_lua.py --dump ../out/lua_corpus)", err)
	}
	return hs
}

// hpSpawnZone is a zone whose mesh answers ONE fixed spot, so a real Monster row can
// go through the REAL SpawnMonsterObject without pathtool.exe. The stance/skill
// tables and the log come from castZone (the same fixture the swing tests use).
func hpSpawnZone(t *testing.T, spot [3]int32) (*Handler, *bytes.Buffer) {
	t.Helper()
	h, logs := castZone(t)
	h.Monsters = realMonsters(t)
	h.MaxHP = testMonsterHPScript(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		if hRange != spawnHRange || vRange != spawnVRange {
			// The span ranges are the reference's own 1000/300; a caller that changed
			// them is not the spawn path this test is about.
			return [3]int32{}, 0, false
		}
		return spot, 10, true
	}
	return h, logs
}

// monHPByHand is `SCR_Get_MON_MHP` written out in Go, line for line
// (monster\get_status_monster.lua:13-64). It is the TEST's own copy on purpose:
// checking the script against the file that produced the same numbers would prove
// only that one function called itself.
//
// IsWorldPVPServer() is 0 in this build, so the 0.45 branch is not taken; the passive
// owner branch is not taken because this zone builds no monster ownership
// (GetOwner answers nil); the two are the same zero terms the script sees.
func monHPByHand(m Monster) float64 {
	var v float64
	switch {
	case m.RaceType == "Home" || m.RaceType == "Colony":
		v = float64(m.Level)
		v = v * (m.MHPFix*0.01 + 1)
	case m.ClassID == 15041:
		v = 666666666
		v = v * (100 + m.MHPBM) / 100
	case m.ClassID == 16664 || m.ClassID == 16665 || m.ClassID == 12973 ||
		m.ClassID == 12974 || (m.ClassID >= 40345 && m.ClassID <= 40359):
		v = ((m.CON * 1000) + m.MHPBM) * float64(m.Level)
	default:
		levelfix := (float64(m.Level)/2 + 50) / 100
		v = (150 + float64(m.Level)*15) * (m.CON / 5) * levelfix * 0.8
		v = v * (100 + m.MHPBM) / 100
		v = v * (m.MHPFix*0.01 + 1)
		if v < 1 {
			v = 1
		}
	}
	// ADDMHP_BM is added AFTER every branch (the corpus' `end` closes the if before
	// this line), and the answer is floored once, at the end.
	v += m.ADDMHPBM
	return math.Floor(v)
}

// TestMonsterHPScriptMatchesTheRowsOwnFormula is the first half of A: the game's own
// entry point, run through the zone's host for the owner's own row (11591), answers
// the quoted formula. 25,520 is a PIN: it is what the shipped row computes, so a
// moved table row fails here instead of silently changing every spawn.
func TestMonsterHPScriptMatchesTheRowsOwnFormula(t *testing.T) {
	hs := testMonsterHPScript(t)
	table := realMonsters(t)
	row, ok := table.Row(bigfootClass)
	if !ok {
		t.Fatalf("class %d is not a row of the Monster table - the row this fix is "+
			"proved on is gone", bigfootClass)
	}
	m := Monster{
		ClassID: bigfootClass, Level: row.Level,
		RaceType: row.RaceType, Arm: row.Arm, DEFBM: row.DEFBM, DEFPERBM: row.DEFPERBM,
		IMPBM: row.IMPBM, SizeType: row.SizeType,
		CON: row.CON, MHPFix: row.MHPFix, MHPBM: row.MHPBM, ADDMHPBM: row.ADDMHPBM,
	}
	got, ok, err := hs.MaxHP(m)
	if err != nil || !ok {
		t.Fatalf("SCR_Get_MON_MHP did not answer for class %d: ok=%v err=%v",
			bigfootClass, ok, err)
	}
	want := monHPByHand(m)
	if got != want {
		t.Fatalf("class %d: the script answers MHP %g, the quoted formula gives %g "+
			"(Lv %d, CON %g, MHPFix %g, MHP_BM %g)", bigfootClass, got, want,
			m.Level, m.CON, m.MHPFix, m.MHPBM)
	}
	if want != 25520 {
		t.Fatalf("class %d's max HP is %g, the shipped row computes 25520 - the row "+
			"moved, so the -spawndummy card and this pin must be updated together",
			bigfootClass, want)
	}
	if got <= 0 {
		t.Fatalf("the script answered MHP %g - a body with no max HP is the bug", got)
	}
	t.Logf("class %d (%q): Lv %d CON %g -> MHP %g from %s", bigfootClass, row.Name,
		m.Level, m.CON, got, hs.File)
}

// TestSpawnWithoutMHPSeedsTheLedgerFromTheScript is A's second half: the REAL spawn
// path, no explicit MHP, and the ledger - the number every later hit and swing reads -
// holds the script's answer.
func TestSpawnWithoutMHPSeedsTheLedgerFromTheScript(t *testing.T) {
	spot := [3]int32{3160, -1800, 55}
	h, logs := hpSpawnZone(t, spot)

	handle, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: int32(bigfootClass), X: float64(spot[0]), Y: float64(spot[1]),
		Count: 1, Direction: 270, Tactics: "MON_MISSION_84",
	})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	made := h.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) stored, want 1", len(made))
	}
	m := made[0]
	want := monHPByHand(m)
	if m.MaxHP != want {
		t.Fatalf("the body carries MaxHP %g, the script's formula gives %g", m.MaxHP, want)
	}
	hp, known := h.monsterLiveHP(handle)
	if !known {
		t.Fatalf("the spawn wrote NO ledger entry for handle %d - that is the bug "+
			"this card fixes\n%s", handle, logs.String())
	}
	if hp != want {
		t.Fatalf("the ledger holds %g, the script answered %g", hp, want)
	}
	if m.HP != int32(want) {
		t.Fatalf("the legacy int32 HP copy is %d, want %d", m.HP, int32(want))
	}
	if !strings.Contains(logs.String(), "CREATE_TMONSTER") {
		t.Fatalf("no CREATE_TMONSTER line\n%s", logs.String())
	}
	t.Logf("class %d with no MHP: ledger hp %g, the script's own answer (want %g)",
		bigfootClass, hp, want)
}

// TestExplicitMHPStillWins is B: the script's `mhpFix` - and the probes' own numbers -
// arrive as spec.MHP, and the spawn must keep that number rather than overwrite it
// with the row's formula.
func TestExplicitMHPStillWins(t *testing.T) {
	spot := [3]int32{3160, -1800, 55}
	h, logs := hpSpawnZone(t, spot)

	const explicit = 20000 // what the -spawnsplashpair Scarecrow passes
	handle, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: int32(bigfootClass), X: float64(spot[0]), Y: float64(spot[1]),
		Count: 1, MHP: explicit,
	})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	hp, known := h.monsterLiveHP(handle)
	if !known || hp != explicit {
		t.Fatalf("the ledger holds %g (known=%v), want the spawn's own %d - an "+
			"explicit MHP must win over the row's formula", hp, known, explicit)
	}
	if m := h.runtimeMonsters()[0]; m.MaxHP != explicit {
		t.Fatalf("the body's MaxHP is %g, want the spawn's %d", m.MaxHP, explicit)
	}
}

// TestUnansweredHPKeepsZeroAndLogsOnce is C: the script is armed but does not answer.
// The body keeps 0, the ledger gets NO entry (so no hit can invent one), and the zone
// says why ONCE - not once per body.
func TestUnansweredHPKeepsZeroAndLogsOnce(t *testing.T) {
	spot := [3]int32{3160, -1800, 55}
	h, logs := hpSpawnZone(t, spot)

	dir := t.TempDir()
	const raising = `function SCR_Get_MON_MHP(self)
  error("the stub monster HP script always raises")
end
`
	if err := os.WriteFile(filepath.Join(dir, "monster.lua"), []byte(raising), 0o644); err != nil {
		t.Fatalf("write the stub corpus: %v", err)
	}
	host := script.New()
	defer host.Close()
	if _, err := host.Index(dir); err != nil {
		t.Fatalf("index the stub corpus: %v", err)
	}
	hs, err := NewMonsterHPScript(host, t.Logf)
	if err != nil {
		t.Fatalf("NewMonsterHPScript on the stub corpus: %v", err)
	}
	h.MaxHP = hs

	for i := 0; i < 2; i++ {
		handle, serr := h.SpawnMonsterObject(MonsterSpec{
			MonsterID: int32(bigfootClass), X: float64(spot[0]), Y: float64(spot[1]),
			Count: 1,
		})
		if serr != nil {
			t.Fatalf("spawn %d: %v", i, serr)
		}
		if hp, known := h.monsterLiveHP(handle); known {
			t.Fatalf("spawn %d wrote ledger hp %g although the script did not answer - "+
				"an unanswered body must have NO entry", i, hp)
		}
		if m := h.runtimeMonsters()[i]; m.MaxHP != 0 || m.HP != 0 {
			t.Fatalf("spawn %d kept MaxHP %g / HP %d, want 0 and 0", i, m.MaxHP, m.HP)
		}
	}

	text := logs.String()
	if got := strings.Count(text, "has NO max HP"); got != 1 {
		t.Fatalf("%d \"has NO max HP\" line(s), want exactly 1 for the zone:\n%s", got, text)
	}
	for _, want := range []string{
		"the stub monster HP script always raises",
		"cannot be hit and will not swing",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the one line does not say %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "zone: CREATE_TMONSTER \""); got != 2 {
		t.Fatalf("%d CREATE_TMONSTER line(s), want 2", got)
	}
}

// TestSpawnedBodySwingsAtAPlayerInReach is D, the symptom the owner is waiting on. The
// body is made by the REAL SpawnMonsterObject - no `spawnWithHP` helper, no hand-built
// ledger - and then the zone's own clock must find it armed and swing at a player
// inside the row's reach.
func TestSpawnedBodySwingsAtAPlayerInReach(t *testing.T) {
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Monsters = realMonsters(t)
	h.MaxHP = testMonsterHPScript(t)

	// The mesh answers the monster's spot: the body stands at (0,0,cell 29) and the
	// victim 150 units away - edge to edge 50 (150 - the monster's 50 - the PC's 50),
	// inside row 19763's own MinR 1 .. MaxR 100.
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{0, 0, 29}, 10, true
	}
	victim := newPlayer(0x40000101, "Victim", [3]int32{0, 150, 29})
	victim.sess.Team[0].HP = 1e12
	victim.sess.Team[0].MaxHP = 1e12
	victim.sess.Team[0].Stance = 10560 // Peltast: a real stance row for sheetFor
	h.joinZone(victim.sess)
	cullPlace(h, victim, [3]int32{0, 150, 29})
	victim.conn.writes = nil

	handle, err := h.SpawnMonsterObject(MonsterSpec{MonsterID: int32(swingClassID),
		X: 0, Y: 0, Count: 1, Direction: 270})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	if hp, known := h.monsterLiveHP(handle); !known || hp <= 0 {
		t.Fatalf("the spawned body has no HP in the ledger (known=%v hp=%g), so it "+
			"cannot swing\n%s", known, hp, logs.String())
	}
	if !h.monsterSwingArmed() {
		t.Fatalf("monsterSwingArmed is FALSE with a spawned, HP-bearing body in the "+
			"zone - the session clock would never run for it\n%s", logs.String())
	}

	victim.conn.writes = nil // the spawn's own 3527 is not the swing
	before := victim.sess.Team[0].HP
	h.monsterSwingTick(time.Unix(1_700_000_000, 0))
	sent := victim.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("one cadence produced %d packet(s) on the victim's socket, want "+
			"exactly 1:\n%s", len(sent), logs.String())
	}
	if cmd := binary.LittleEndian.Uint16(sent[0]); cmd != CmdZCAttackSingle {
		t.Fatalf("the swing's packet is cmd %d, want %d", cmd, CmdZCAttackSingle)
	}
	if victim.sess.Team[0].HP >= before {
		t.Fatalf("the victim's HP did not fall (%g -> %g)", before, victim.sess.Team[0].HP)
	}
	if !strings.Contains(logs.String(), "MONSTER SWING") {
		t.Fatalf("the swing happened without its line:\n%s", logs.String())
	}
}

// TestThe3527CarriesTheLedgersHP is E. The packet the client is sent must carry the
// SAME number the ledger holds - in BOTH HP fields (current HP at +28, max at +298) -
// and the field is 8 bytes wide, so the reference's ~1.0e12 Scarecrow HP goes out
// EXACTLY. The only clamp left is the int32 legacy copy, which must saturate instead
// of wrapping to a negative number.
func TestThe3527CarriesTheLedgersHP(t *testing.T) {
	spot := [3]int32{3160, -1800, 55}
	h, logs := castZone(t)
	h.Monsters = realMonsters(t)
	h.MaxHP = testMonsterHPScript(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return spot, 10, true
	}
	p := newPlayer(0x40000101, "Watcher", spot)
	h.joinZone(p.sess)
	cullPlace(h, p, spot)
	p.conn.writes = nil

	// A body whose max HP is far wider than an int32: the reference's own Scarecrow
	// sat near 1.0e12, which is the case a wrapped field would show as a full bar
	// over a dead body.
	const huge = 1.0e12
	handle, err := h.SpawnMonsterObject(MonsterSpec{MonsterID: int32(swingClassID),
		X: float64(spot[0]), Y: float64(spot[1]), Count: 1, MHP: huge})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v\n%s", err, logs.String())
	}
	ledger, known := h.monsterLiveHP(handle)
	if !known || ledger != huge {
		t.Fatalf("the ledger holds %g (known=%v), want the spawn's %g", ledger, known, huge)
	}

	var pkt []byte
	for _, w := range p.conn.sent() {
		if len(w) == EnterStandingMonsterSize && binary.LittleEndian.Uint16(w) == CmdZCEnterStandingMonster {
			pkt = w
		}
	}
	if pkt == nil {
		t.Fatalf("the watcher never received the spawn's 3527\n%s", logs.String())
	}
	hp := math.Float64frombits(binary.LittleEndian.Uint64(pkt[emHP:]))
	mhp := math.Float64frombits(binary.LittleEndian.Uint64(pkt[emMaxHP:]))
	if hp != ledger {
		t.Fatalf("the 3527 carries HP %g at +%d, the ledger holds %g", hp, emHP, ledger)
	}
	if mhp != ledger {
		t.Fatalf("the 3527 carries max HP %g at +%d, the ledger holds %g", mhp, emMaxHP, ledger)
	}

	// THE LEGACY INT32 COPY SATURATES, it does not wrap: a negative int32 here is
	// exactly how "1.0e12" becomes a full bar over a dead body in an older reader.
	m := h.runtimeMonsters()[0]
	if m.HP != math.MaxInt32 {
		t.Fatalf("the int32 HP copy is %d, want the saturation value %d - 1.0e12 "+
			"must not wrap", m.HP, int32(math.MaxInt32))
	}
	if got := hpFieldValue(-1); got != 0 {
		t.Fatalf("hpFieldValue(-1) = %g, want 0 - a negative HP is the client's own clamp", got)
	}
	t.Logf("ledger %g: 3527 hp %g at +%d, mhp %g at +%d, int32 copy %d",
		ledger, hp, emHP, mhp, emMaxHP, m.HP)
}

// TestThe3527sHPMatchesTheFixture pins the field's WIDTH against the live capture: the
// fixture's own 8 bytes at +0x1c decode to its current HP, and the bytes at appearance+6
// to the same double. If a future reader "fixes" HP to an int32, the fixture's value
// (1,023,356.0) would no longer be reproduced by the same 8 bytes and this fails.
func TestThe3527sHPMatchesTheFixture(t *testing.T) {
	live := liveEnterMonsterBytes(t)
	hp := math.Float64frombits(binary.LittleEndian.Uint64(live[emHP:]))
	mhp := math.Float64frombits(binary.LittleEndian.Uint64(live[emMaxHP:]))
	if hp != capturedMaxHP || mhp != capturedMaxHP {
		t.Fatalf("the fixture's HP fields decode to %g / %g, want the named "+
			"capturedMaxHP %g", hp, mhp, capturedMaxHP)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(
		BuildEnterStandingMonster(DevStandingMonster())[emHP:])); got != capturedMaxHP {
		t.Fatalf("the S1 builder writes HP %g, the capture has %g", got, capturedMaxHP)
	}
}
