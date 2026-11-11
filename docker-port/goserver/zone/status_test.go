package zone

import (
	"encoding/binary"
	"testing"
)

// itemStatsFile is the emitted item columns, relative to the package's test
// working directory (goserver\zone -> docker-port\data).
const itemStatsFile = "../../data/item_stats.json"

// loadTestItemStats installs data\item_stats.json for one test and puts the
// package state back afterwards, so a later test cannot inherit it.
func loadTestItemStats(t *testing.T) *ItemStats {
	t.Helper()
	table, err := LoadItemStats(itemStatsFile)
	if err != nil {
		// FATAL, not a skip: the file is committed data, so a miss is the repo's
		// fault and a skip would report green having checked nothing.
		t.Fatalf("item stat columns are missing or unreadable at %s: %v",
			itemStatsFile, err)
	}
	old := itemStats
	SetItemStats(table)
	t.Cleanup(func() { SetItemStats(old) })
	return table
}

// testStances loads the client's own stance tables, or fails: every test below
// is about the numbers those tables produce.
func testStances(t *testing.T) *StanceTable {
	t.Helper()
	table, err := LoadStances(XMLDir())
	if err != nil {
		t.Fatalf("stance tables are unreadable (%v) - a stance's own columns are "+
			"half of every number in this packet", err)
	}
	return table
}

// readBattleWord and readResist read the packet the way the CLIENT does - by
// offset - so a change to the layout constants cannot pass unnoticed.
func readBattleWord(p []byte, at int) uint16 {
	return binary.LittleEndian.Uint16(p[updBattleAt+at:])
}

// readBattleDword reads the 32-bit field (only ATK is one).
func readBattleDword(p []byte, at int) uint32 {
	return binary.LittleEndian.Uint32(p[updBattleAt+at:])
}

func readResist(p []byte, at int) int16 {
	return int16(binary.LittleEndian.Uint16(p[updResistAt+at:]))
}

// TestUpdateAllStatusIs125Bytes pins the SHAPE of ZC_UPDATE_ALL_STATUS: the
// client's own table declares a fixed 125 bytes (data\cmd_table_live.txt:1351)
// and its handler reads four fixed blocks. A record of any other length makes
// the client hand its parser garbage.
func TestUpdateAllStatusIs125Bytes(t *testing.T) {
	if UpdateAllStatusSize != 125 {
		t.Fatalf("UpdateAllStatusSize = %d, want the client's 125",
			UpdateAllStatusSize)
	}
	// The block offsets the handler memcpys to, and the field offsets inside
	// BATTLE_STAT the shipped client's panel reads (FUN_007820ce). If one of these
	// moves, the CATK-dword trap is back and DR/DEF/HR/CSPD land two bytes off.
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"BASIC_STAT", updBasicAt, 6},
		{"BUFF_MODIFIER_STAT", updBuffAt, 55},
		{"BATTLE_STAT", updBattleAt, 79},
		{"RESISTANCE_STAT", updResistAt, 115},
		{"BATTLE_STAT length", battleStatLen, 36},
		{"AR", battleAR, 0},
		{"CATK (32-bit)", battleCATK, 2},
		{"DR", battleDR, 6},
		{"DEF", battleDEF, 8},
		{"HR", battleHR, 10},
		{"PR", battlePR, 12},
		{"CSPD", battleCSPD, 14},
		{"MSPD (last field)", battleMSPD, 34},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s starts/measures %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	c := Character{Handle: 0x11223344, Name: "Tester", Job: 1, Level: 60}
	p := BuildUpdateAllStatus(c, StatusSheet{ATK: 453, DEF: 45})
	if len(p) != 125 {
		t.Fatalf("packet is %d bytes, want 125", len(p))
	}
	if got := binary.LittleEndian.Uint16(p); got != CmdZCUpdateAllStatus {
		t.Fatalf("cmd = %d, want %d", got, CmdZCUpdateAllStatus)
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != c.Handle {
		t.Fatalf("handle = %#x, want %#x", got, c.Handle)
	}
	// The packet has no length field of its own - the client sizes it from its
	// command table - so "length == len(packet)" is the constant above.
	if UpdateAllStatusSize != len(p) {
		t.Fatalf("UpdateAllStatusSize = %d but the packet is %d bytes",
			UpdateAllStatusSize, len(p))
	}
}

// TestUpdateAllStatusATKFromWeaponStanceAndBaseStats is the NAMED ATK case. It
// runs the REAL code path (the client's own item, job and stance tables) and
// writes the arithmetic out, with the script line that produced each factor:
//
//	weapon       class 101 IceSword: ATK 89, ClassType "Sword"
//	base stats   job 1 Fighter: STR 80, AGI 60 (datatable_job.ies row)
//	stance       job 1 holding a Sword in RH and nothing in LH -> the game's own
//	             condition 10122 -> stance 10110 "BackGuard"
//	             AtkType Single, RHits 1, LHits 0, StnATK 3
//
// SCR_GET_STATANDWEAPON (get_status_pc.scp:3760, stage 1 = melee):
//
//	statfix = (STR/10 + (AGI/10 - 5)/2) / 5 = (80/10 + (60/10 - 5)/2)/5
//	        = (8 + 0.5)/5 = 1.7
//
// SCR_Get_CATK (:984, :1000, :1005):
//
//	ratk = 89 * 1.7 * RHits(1) * StnATK(3) = 453.9
//	no percent terms are worn, so ratk stays 453.9
//	Single, so atk = (ratk + RH.AddATK 0) / RHits(1) = 453.9
//	math.floor -> 453
func TestUpdateAllStatusATKFromWeaponStanceAndBaseStats(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	pick, ok := stances.PickStance(1, [2]string{"None", "Sword"})
	if !ok {
		t.Fatal("job 1 holding a Sword has no stance at all")
	}
	if pick.Stance != 10110 {
		t.Fatalf("job 1 with RH Sword picked stance %d, want the game's own 10110 "+
			"(condition 10122)", pick.Stance)
	}

	c := Character{Handle: 7, Name: "Fighter", Job: 1, Level: 60, Stance: pick.Stance}
	c.Equip[HandSpotRight] = 101 // IceSword

	sheet := sheetOf(t, c)
	if sheet.ATK != 453 {
		t.Fatalf("ATK = %v, want 453: 89 * (80/10 + (60/10-5)/2)/5 * 1 * 3 / 1",
			sheet.ATK)
	}
	p := BuildUpdateAllStatus(c, sheet)
	if got := readBattleDword(p, battleCATK); got != 453 {
		t.Fatalf("the ATK word on the wire = %d, want 453", got)
	}
}

// TestUpdateAllStatusDEFIsWornDEFOnly is the DEF case. SCR_Get_DEF
// (`get_status_pc.scp:1126-1156`) is the sum of the WORN ITEMS' DEF columns -
// no base stat and no level enters it:
//
//	BODY  4000 LeatherArmor1Sol  DEF 44
//	BOOTS 19533 SettlerShoes     DEF  1
//	everything else empty        DEF  0
//	                            = 45                      (:1156)
//
// The test proves the "no base stats" half too: the same gear on two different
// jobs must give the same DEF.
func TestUpdateAllStatusDEFIsWornDEFOnly(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	build := func(job uint32) float64 {
		c := Character{Handle: 1, Name: "x", Job: job, Level: 60, Stance: 10110}
		c.Equip[spotBody] = 4000   // DEF 44
		c.Equip[spotBoots] = 19533 // DEF +1
		return sheetOf(t, c).DEF
	}
	for _, job := range []uint32{1, 3, 72} {
		if got := build(job); got != 45 {
			t.Fatalf("job %d: DEF = %v, want 45 (44 + 1, worn DEF only)", job, got)
		}
	}

	c := Character{Handle: 1, Name: "x", Job: 1, Level: 60, Stance: 10110}
	c.Equip[spotBody] = 4000
	c.Equip[spotBoots] = 19533
	p := BuildUpdateAllStatus(c, sheetOf(t, c))
	if got := readBattleWord(p, battleDEF); got != 45 {
		t.Fatalf("the DEF word on the wire = %d, want 45", got)
	}
}

// TestUpdateAllStatusCommander25Leader is a REAL character from the shipped
// database: commander 25 ("test01"), the team leader character 1792 "Tester" -
// job 3 Scout (STR 30 CON 60 DEX 70 INT 80 AGI 70 CHA 50), level 100 (its exp
// 187183514 is exactly the level-100 row of ge_tables), wearing
//
//	BODY  class 15001 CrecementoLeather  DEF 0 in the row, RFIRE/RICE/RLGHT 36,
//	                                     WLv 0 in the row - a GROWING item
//	BOOTS class 19533 SettlerShoes       DEF 1, WLv 20
//
// and holding NOTHING, so the game's own bare-hand condition 300 applies and
// the stance is 30110 "FirstAid" (Single, Parry, RHits 1, StnATK 1, StnHR 100,
// StnLv 1, IncHR 0, StnRSTAT 20, ASPD 1500, IncASPD 0).
//
// THE GROWING BODY, which is what changed in slice 2. 15001's row carries no
// DEF and no WLv, so both come from `ge\xml\datatable_growing_item.xml:36`
//
//	<Class ClassID="34" ItemID="15001" Rank="math.floor(3 + Lv/4)"
//	       Value="Lv*1.2 + 55" />
//
// at the OWNER'S level, 100 (not a fixed 100 by choice - it is what
// barrack.LevelOf(187183514) answers for character 1792):
//
//	DEF   Value = 1.2*100 + 55 = 175        (item_stats.json growing.15001)
//	WLv   Rank  = floor(3 + 100/4) = 28
//
// The numbers, each from its rule:
//
//	ATK   0   - no weapon, so ratk = latk = 0 (:984-985); bare hands have no ATK
//	DEF   176 - worn DEF only: body 175 (growing) + boots 1 (:1156)
//	A.R.  25  - floor(100/4) + RH.WLv 0 (:602, :615) - the BODY's WLv is not an
//	            A.R. term, only a D.R. one, so A.R. is unchanged by the growing body
//	D.R.  53  - floor(100/4) + body.WLv 28, DefType is Parry so no shield term (:669-677)
//	HR    100 - StnHR 100 + (-25 + DEX 70/2) = 110, clamped (:1318, :1330, :1343)
//	CSPD  132 - 1500*1.1 = 1650, * (100 - 20)/100 = 1320, / (RHits 1) / 10 (:1705-1710, :1759)
//	RFIRE 52  - armor 36 + INT 80*0.2 = 16 (:2135)
//	RICE  52  - same shape (:2182)
//	RLGHT 52  - same shape (:2229)
//	RSTAT 36  - stance StnRSTAT 20 + armor 0 + INT 16 (:2277)
//	RPSY  50  - Lv 100/2 = 50 and NOTHING else: the script computes
//	            `INT = (...) * 0.2` at :2306 and never reads it, so RPSY has no
//	            INT term (the other four do)
func TestUpdateAllStatusCommander25Leader(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	pick, ok := stances.PickStance(3, [2]string{"None", "None"})
	if !ok || pick.Stance != 30110 {
		t.Fatalf("job 3 bare-handed picked stance %d (ok=%v), want 30110 FirstAid",
			pick.Stance, ok)
	}
	c := Character{Handle: 0x0F7000, Name: "Tester", Job: 3, Level: 100, Stance: pick.Stance}
	c.Equip[spotBody] = 15001
	c.Equip[spotBoots] = 19533

	p := BuildUpdateAllStatus(c, sheetOf(t, c))

	// The evidence a human can check against the panel: the field-by-field build
	// and the raw bytes. Printed only under `go test -v`.
	t.Logf("3684 for %s (handle %#x): ATK %d DEF %d AR %d DR %d HR %d CSPD %d "+
		"RFIRE %d RICE %d RLGHT %d RSTAT %d RPSY %d, %d bytes",
		c.Name, c.Handle,
		readBattleDword(p, battleCATK), readBattleWord(p, battleDEF),
		readBattleWord(p, battleAR), readBattleWord(p, battleDR),
		readBattleWord(p, battleHR), readBattleWord(p, battleCSPD),
		readResist(p, 0), readResist(p, 2), readResist(p, 4),
		readResist(p, 6), readResist(p, 8), len(p))
	t.Logf("bytes: % x", p)

	if got := readBattleDword(p, battleCATK); got != 0 {
		t.Fatalf("ATK = %d, want 0: bare hands carry no ATK", got)
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"DEF", int(readBattleWord(p, battleDEF)), 176},
		{"A.R.", int(readBattleWord(p, battleAR)), 25},
		{"D.R.", int(readBattleWord(p, battleDR)), 53},
		{"HR", int(readBattleWord(p, battleHR)), 100},
		{"CSPD", int(readBattleWord(p, battleCSPD)), 132},
		{"RFIRE", int(readResist(p, 0)), 52},
		{"RICE", int(readResist(p, 2)), 52},
		{"RLGHT", int(readResist(p, 4)), 52},
		{"RSTAT", int(readResist(p, 6)), 36},
		{"RPSY", int(readResist(p, 8)), 50},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s = %d, want %d (see the arithmetic in this test's comment)",
				tc.name, tc.got, tc.want)
		}
	}
}

// TestUpdateAllStatusZeroWithoutTables is the missing-file case: no
// item_stats.json and no job_stats.json loaded. Every DERIVED number must be 0
// - what the panel showed before this packet existed - and nothing may panic,
// because this is the state a hand-started zone is in when the emitter has not
// been run.
//
// The level in this case is 0 on purpose: A.R. and D.R. genuinely START at
// floor(Lv/4) (`get_status_pc.scp:602`, `:669`) and need no table to say so, so
// a level-50 character would still read 12 there. That is the script's own
// number, not an item's, and the second half of this test pins it.
func TestUpdateAllStatusZeroWithoutTables(t *testing.T) {
	oldItems, oldJobs := itemStats, jobStats
	SetItemStats(nil)
	SetJobStats(nil)
	t.Cleanup(func() { SetItemStats(oldItems); SetJobStats(oldJobs) })

	c := Character{Handle: 5, Name: "Nobody", Job: 1, Level: 0, Stance: 10110}
	c.Equip[HandSpotRight] = 101 // a real weapon, but no table to describe it

	// A ZERO sheet must still build a well-formed packet: this is the writer's own
	// contract, and it is what a zone with no script source sends (zeros are visible,
	// a plausible number is not). The sheet itself now comes from the game's script
	// (zone\status_script.go), so the old "no tables loaded" premise is gone.
	p := BuildUpdateAllStatus(c, StatusSheet{})
	if len(p) != UpdateAllStatusSize {
		t.Fatalf("packet is %d bytes, want %d", len(p), UpdateAllStatusSize)
	}
	for _, at := range []int{battleAR, battleDR, battleDEF, battleHR, battleCSPD} {
		if got := readBattleWord(p, at); got != 0 {
			t.Fatalf("battle word at %d = %d, want 0 with no tables loaded", at, got)
		}
	}
	if got := readBattleDword(p, battleCATK); got != 0 {
		t.Fatalf("ATK = %d, want 0 with no tables loaded", got)
	}
	for _, at := range []int{0, 2, 4, 6, 8} {
		if got := readResist(p, at); got != 0 {
			t.Fatalf("resistance at %d = %d, want 0 with no tables loaded", at, got)
		}
	}

	// A level ALONE no longer contributes: A.R. and D.R. are the script's answers now
	// (SCR_Get_AR / SCR_Get_DR, whose level terms are floor(Lv/4) plus gear), so a
	// zero sheet stays zero at any level. What this half still pins is that the
	// writer carries a zero faithfully instead of inventing a level term.
	c.Level = 50
	p = BuildUpdateAllStatus(c, StatusSheet{})
	for _, at := range []int{battleAR, battleDR} {
		if got := readBattleWord(p, at); got != 0 {
			t.Fatalf("battle word at %d = %d, want 0: the level term belongs to the "+
				"script (SCR_Get_AR/DR), not to this writer", at, got)
		}
	}
}

// TestGrowingItem7201ATKAtTheLeadersLevel is the ATK half of the growing-item
// work: a weapon whose row has NO ATK column, so the sheet reads the curve at
// the OWNER'S level.
//
// Item 7201 "Crescemento Sword" - the sword commander 25's TesterF (character
// 1794, team position 2) actually wears in its right hand, read from
// `wear_item` (spot 5) joined to `item.class_id`. Its own row in
// `ge\xml\datatable_item_weapon.xml` has ATK="0" and WLv="0"; the numbers are
// in `ge\xml\datatable_growing_item.xml:3`
//
//	<Class ClassID="1" ItemID="7201" Rank="math.floor(5 + Lv/4)"
//	       Value="Lv*1.7 + 55" />
//
// The level is the LEADER's real one, 100: commander 25's three characters all
// carry exp 187183514 (character 1792 in `character.exp`), which is the
// level-100 row of barrack/data/exp_levels.csv (100,187183514). The leader is
// character 1792 "Tester", job 3 Scout, and job 3 holding a Sword picks the
// game's own stance 10630 "EscrimaSCT" (Single, RHits 1, StnATK 3).
//
//	ATK Value  = 1.7*100 + 55 = 225
//	WLv Rank   = floor(5 + 100/4) = 30
//
// SCR_GET_STATANDWEAPON (`get_status_pc.scp:3760`, stage 1 melee for "Sword"):
//
//	statfix = (STR/10 + (AGI/10 - 5)/2)/5 = (30/10 + (70/10 - 5)/2)/5
//	        = (3 + 1)/5 = 0.8
//
// SCR_Get_CATK (:984, :1000, :1005):
//
//	ratk = 225 * 0.8 * RHits(1) * StnATK(3) = 540
//	no percent terms are worn, so ratk stays 540
//	Single, so atk = (ratk + RH.AddATK 0) / RHits(1) = 540
//	math.floor -> 540
//
// The same curve read AGAINST A LEVEL-100 SWORD THAT IS NOT GROWING is the
// cross-check that the numbers are the game's: class 6001/6010/6016 (UseLv 100)
// carry ATK 225 and WLv 30 in datatable_item_weapon.xml - exactly what 7201's
// two expressions answer at level 100.
func TestGrowingItem7201ATKAtTheLeadersLevel(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	pick, ok := stances.PickStance(3, [2]string{"None", "Sword"})
	if !ok || pick.Stance != 10630 {
		t.Fatalf("job 3 holding a Sword picked stance %d (ok=%v), want 10630 EscrimaSCT",
			pick.Stance, ok)
	}
	// Character 1792 "Tester", level 100 - the leader of commander 25.
	c := Character{Handle: 1792, Name: "Tester", Job: 3, Level: 100, Stance: pick.Stance}
	c.Equip[HandSpotRight] = 7201

	p := BuildUpdateAllStatus(c, sheetOf(t, c))
	if got := readBattleDword(p, battleCATK); got != 540 {
		t.Fatalf("ATK = %d, want 540: floor(225 * 0.8 * 1 * 3 / 1), where 225 is "+
			"1.7*100 + 55 from datatable_growing_item.xml:3 for item 7201", got)
	}
	t.Logf("ATK for the leader holding 7201 at level 100: %d", readBattleDword(p, battleCATK))

	// The level is the whole point: the same sword on the same character at
	// level 50 answers the curve at 50 - 1.7*50 + 55 = 140, and its WLv 17.
	c.Level = 50
	sheet := sheetOf(t, c)
	if sheet.ATK != 336 {
		t.Fatalf("ATK at level 50 = %v, want 336: floor(140 * 0.8 * 1 * 3 / 1), "+
			"where 140 is 1.7*50 + 55", sheet.ATK)
	}
	if sheet.AR != 29 {
		t.Fatalf("A.R. at level 50 = %v, want 29: floor(50/4) + Rank floor(5+50/4) = "+
			"12 + 17", sheet.AR)
	}

	// The character that REALLY wears the sword: TesterF, character 1794, job 1
	// Fighter (STR 80 CON .. AGI 60), the same level 100, wearing 7201 in the
	// right hand and 15001 on the body. Its stance is 10110 "BackGuard"
	// (Single, RHits 1, StnATK 3).
	//
	//	statfix = (80/10 + (60/10 - 5)/2)/5 = (8 + 0.5)/5 = 1.7
	//	ATK     = floor(225 * 1.7 * 1 * 3 / 1) = floor(1147.5) = 1147
	//	DEF     = body 15001 growing 175 (no boots)                 = 175
	//	A.R.    = floor(100/4) + rw.WLv 30 = 25 + 30               = 55
	//	D.R.    = floor(100/4) + body.WLv 28                       = 53
	pick1, ok := stances.PickStance(1, [2]string{"None", "Sword"})
	if !ok || pick1.Stance != 10110 {
		t.Fatalf("job 1 holding a Sword picked stance %d (ok=%v), want 10110", pick1.Stance, ok)
	}
	tf := Character{Handle: 1794, Name: "TesterF", Job: 1, Level: 100, Stance: pick1.Stance}
	tf.Equip[HandSpotRight] = 7201
	tf.Equip[spotBody] = 15001
	ts := sheetOf(t, tf)
	if ts.ATK != 1147 || ts.DEF != 175 || ts.AR != 55 || ts.DR != 53 {
		t.Fatalf("TesterF sheet = ATK %v DEF %v AR %v DR %v, want 1147 / 175 / 55 / 53",
			ts.ATK, ts.DEF, ts.AR, ts.DR)
	}
	t.Logf("TesterF (character 1794, the one that wears 7201): ATK %v DEF %v AR %v DR %v",
		ts.ATK, ts.DEF, ts.AR, ts.DR)
}

// TestGrowingItemFillsOnlyTheEmptyColumns is the "do not overwrite" half: item
// 7217 "Crescemento Leg-Guard" carries its OWN DEF 2 in
// datatable_item_weapon.xml (its text says "the A.R. and ATK Power ... [DEF +2]"),
// while its ATK and WLv are empty. The growing curve must fill ATK and WLv and
// leave DEF 2 alone.
//
//	ATK  Lv*1.0 + 38 at level 100 = 138   (datatable_growing_item.xml:18)
//	WLv  Rank floor(5 + Lv/4)     = 30
//	DEF  2, the row's own number - unchanged
func TestGrowingItemFillsOnlyTheEmptyColumns(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)

	stat, ok := itemStats.For(7217)
	if !ok {
		t.Fatal("item class 7217 is not in data\\item_stats.json")
	}
	curve, ok := itemStats.growingFor(7217)
	if !ok {
		t.Fatal("item class 7217 has no growing curve in data\\item_stats.json")
	}
	got := grow(stat, curve, 100)
	if got.ATK != 138 {
		t.Fatalf("ATK = %v, want 138 (1.0*100 + 38)", got.ATK)
	}
	if got.WLv != 30 {
		t.Fatalf("WLv = %v, want 30 (floor(5 + 100/4))", got.WLv)
	}
	if got.DEF != 2 {
		t.Fatalf("DEF = %v, want the row's own 2 - a growing item must not "+
			"overwrite a column it already carries", got.DEF)
	}
	// A plain item is untouched by the whole mechanism: class 6001 is a
	// level-100 sword with ATK 225 and WLv 30 of its own, and no growing curve.
	plain, _ := itemStats.For(6001)
	if _, isGrowing := itemStats.growingFor(6001); isGrowing {
		t.Fatal("class 6001 is not a growing item but has a growing curve")
	}
	if plain.ATK != 225 || plain.WLv != 30 {
		t.Fatalf("plain sword 6001 = ATK %v WLv %v, want 225 / 30 from its own row",
			plain.ATK, plain.WLv)
	}
	if filled := grow(plain, GrowCurve{Column: "ATK", ValueMul: 9, ValueAdd: 9}, 100); filled.ATK != 225 {
		t.Fatalf("a plain item was overwritten: ATK %v, want 225", filled.ATK)
	}
}

// TestGrowingItemWithoutALevelAnswersZero is the "the table cannot serve this
// level" case, and the "no row at all" case. Neither may panic, and neither may
// invent a number: a level of 0 leaves the item's own columns exactly as the
// file wrote them, which for a growing item means 0.
func TestGrowingItemWithoutALevelAnswersZero(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	curve, ok := itemStats.growingFor(7201)
	if !ok {
		t.Fatal("item class 7201 has no growing curve")
	}
	// Level 0: no curve is applied, so the row's own (empty) columns stand.
	if got := grow(itemStats.items[7201], curve, 0); got.ATK != 0 || got.WLv != 0 {
		t.Fatalf("level 0 gave ATK %v WLv %v, want 0 and 0", got.ATK, got.WLv)
	}
	// Negative is not a level either, and must not flip a sign in.
	if got := grow(itemStats.items[7201], curve, -5); got.ATK != 0 {
		t.Fatalf("level -5 gave ATK %v, want 0", got.ATK)
	}
	// A level above the cap is asked at 100, because that is the cap the client
	// itself applies before it evaluates the curve (`FUN_00637151`, 0x637151).
	if got := grow(itemStats.items[7201], curve, 150); got.ATK != 225 {
		t.Fatalf("level 150 gave ATK %v, want 225 (1.7*100 + 55 - the client "+
			"clamps the level to 100)", got.ATK)
	}
	// No growing row for the class: the item answers only its own columns.
	if _, ok := itemStats.growingFor(6001); ok {
		t.Fatal("class 6001 answered a growing curve")
	}
	// And through the whole sheet, a level-0 character wearing 7201 reads 0 and
	// the builder still produces a 125-byte packet.
	c := Character{Handle: 9, Name: "Newborn", Job: 3, Level: 0, Stance: 10630}
	c.Equip[HandSpotRight] = 7201
	c.Equip[spotBody] = 15001
	p := BuildUpdateAllStatus(c, sheetOf(t, c))
	if len(p) != UpdateAllStatusSize {
		t.Fatalf("packet is %d bytes, want %d", len(p), UpdateAllStatusSize)
	}
	if got := readBattleDword(p, battleCATK); got != 0 {
		t.Fatalf("ATK at level 0 = %d, want 0", got)
	}
	if got := readBattleWord(p, battleDEF); got != 0 {
		t.Fatalf("DEF at level 0 = %d, want 0", got)
	}
}

// TestUpdateAllStatusCSPDFollowsTheStance is the NAMED CSPD case: the attack
// speed the sheet carries must follow the STANCE the character is standing in,
// which is what changes when he picks a weapon up. It runs the REAL code path
// (the client's own job, item and stance tables) and writes the arithmetic out,
// with the script line that produced each factor:
//
//	base stats  job 1 Fighter: AGI 60, the term statfix uses
//	bare hands  stance 10010 Bareknuckle (condition 100, "None"/"None"):
//	            ASPD 1667, IncASPD 0, StnLv 1, AtkType Single, RHits 2, LHits 0
//	sword       stance 10110 BackGuard (condition 10122, "None"/"Sword"):
//	            ASPD 1000, IncASPD 0.12, StnLv 1, AtkType Single, RHits 1, LHits 0
//
// SCR_Get_CSPD (`get_status_pc.scp:1613`, :1686-1708, :1739, :1759-1760), with
// neither item in hand carrying an ASPD column (7201 and the bare hand both
// answer 0), so the worn-item factor is 1:
//
//	bare hands (10010)
//	  value = 1667*1.1                        = 1833.7
//	  value = value * (100 - 1*0)/100         = 1833.7
//	  value = value * (100 - (60/10-5)*10)/100 = 1833.7 * 0.9 = 1650.33
//	  value = value * (100 - 0)/100           = 1650.33
//	  >= the 400 floor, so the floor changes nothing
//	  floor(1650.33 / (RHits 2 + LHits 0))    = floor(825.16) = 825
//	  floor(825 / 10)                         = 82
//
//	sword (10110)
//	  value = 1000*1.1                        = 1100
//	  value = value * (100 - 1*0.12)/100      = 1098.68
//	  value = value * 0.9                     = 988.81
//	  value = value * (100 - 0)/100           = 988.81
//	  floor(988.81 / (1 + 0))                 = 988
//	  floor(988 / 10)                         = 98
//
// THE THIRD CHECK PINS THE AGI TERM. Character.AGIBonus is the item-derived AGI
// the live 3684 carries as AGI_BM + AGI_WP (0x000B6E07: 36 -> 39 -> 44 in
// captures\charpanel2\20261029-165952). With 44 supplied, AGI is 104 and the
// factor becomes (100 - (104/10-5)*10)/100 = 0.46:
//
//	sword  floor(1098.68 * 0.46) -> 505 -> floor(505/10) = 50
//	bare   floor(1833.7 * 0.46) = 843.5 -> floor(843.5/2) = 421 -> 42
//
// That is what the live server would carry for a character whose items add 44
// AGI and NO item ASPD. The live 29/25 additionally needs the worn items' own
// ASPD (roughly 43-50% for a full level-150 set), which is gear this capture
// cannot name - so it is NOT asserted here.
func TestUpdateAllStatusCSPDFollowsTheStance(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	// Job 1 Fighter, level 100, nothing worn at all.
	bare, ok := stances.PickStance(1, [2]string{"None", "None"})
	if !ok {
		t.Fatal("job 1 holding nothing has no bare-hand stance")
	}
	if bare.Stance != 10010 {
		t.Fatalf("job 1 with empty hands picked stance %d, want the game's own "+
			"10010 Bareknuckle (condition 100)", bare.Stance)
	}
	c := Character{Handle: 1794, Name: "TesterF", Job: 1, Level: 100, Stance: bare.Stance}
	p := BuildUpdateAllStatus(c, sheetOf(t, c))
	bareCSPD := readBattleWord(p, battleCSPD)
	if bareCSPD != 82 {
		t.Fatalf("CSPD bare-handed = %d, want 82: floor(floor(1667*1.1*0.9/2)/10)", bareCSPD)
	}

	// The same character with a sword in the right hand.
	held, ok := stances.PickStance(1, [2]string{"None", "Sword"})
	if !ok {
		t.Fatal("job 1 holding a Sword has no stance")
	}
	if held.Stance != 10110 {
		t.Fatalf("job 1 with a RH Sword picked stance %d, want the game's own "+
			"10110 BackGuard (condition 10122)", held.Stance)
	}
	c.Stance = held.Stance
	c.Equip[HandSpotRight] = 7201 // growing sword, no own ASPD column
	p = BuildUpdateAllStatus(c, sheetOf(t, c))
	swordCSPD := readBattleWord(p, battleCSPD)
	if swordCSPD != 98 {
		t.Fatalf("CSPD with a sword = %d, want 98: floor(floor(1000*1.1*0.9988*0.9/1)/10)",
			swordCSPD)
	}
	if bareCSPD == swordCSPD {
		t.Fatalf("bare hands and a sword both carry CSPD %d: the sheet is not "+
			"following the stance", bareCSPD)
	}
	t.Logf("CSPD: stance %d bare-handed = %d, stance %d with sword 7201 = %d",
		bare.Stance, bareCSPD, c.Stance, swordCSPD)

	// The AGI term: 44 item-derived AGI, no item ASPD.
	c.AGIBonus = 44
	p = BuildUpdateAllStatus(c, sheetOf(t, c))
	if got := readBattleWord(p, battleCSPD); got != 50 {
		t.Fatalf("CSPD with a sword and AGIBonus 44 = %d, want 50: "+
			"floor(floor(1100*0.9988*0.46/1)/10)", got)
	}
	c.Equip[HandSpotRight] = 0
	c.Stance = bare.Stance
	p = BuildUpdateAllStatus(c, sheetOf(t, c))
	if got := readBattleWord(p, battleCSPD); got != 42 {
		t.Fatalf("CSPD bare-handed with AGIBonus 44 = %d, want 42: "+
			"floor(floor(1833.7*0.46/2)/10)", got)
	}
	t.Logf("CSPD with AGIBonus 44: stance %d bare-handed = 42, stance %d with "+
		"sword = 50", bare.Stance, held.Stance)

	// THE LIVE NUMBERS, AND THE ONE THING THAT STILL DISAGREES.
	// Handle 0x000B6E07 in captures\charpanel2\20261029-165952 sent CSPD 29 with
	// his first gear set and 25 after the change, both in stance 10110, and the
	// packet's own AGI_BM + AGI_WP block carried 36 then 44. Feeding those back in
	// with a level-150 set's worth of ASPD, the rule arrives at the live value
	// BEFORE its own floor:
	//
	//	AGI 60+36 = 96:  statfix 46 -> 0.54,  1100*0.9988*0.54 = 593.29
	//	                 five items at ASPD 10 each = 50% -> 296.64  (= 29)
	//	AGI 60+44 = 104: statfix 54 -> 0.46,  1100*0.9988*0.46 = 505.39
	//	                 same 50%                        -> 252.69  (= 25)
	//
	// ... and then the shipped `if (value < limit) value = limit` with limit 400
	// (get_status_pc.scp:1739-1741) lifts BOTH back to 400, so the packet carries
	// 40 where the live server carried 29 and 25. Every low live word is below
	// that floor: 29, 27, 25, 25, 17, 35 - and 40 is the smallest a one-hit stance
	// can print. So the capture proves the live server's floor is lower than the
	// 400 in our copy of the script; the capture cannot say what it is (anything
	// at or below 170 fits all eleven packets). THIS ASSERTION IS THE TRIPWIRE:
	// when the floor is settled, the two numbers below must become 29 and 25.
	//
	// The five items are STAND-INS for the worn ASPD of a full set, not the loadout
	// he had on - the capture never says which items he wore. data\item_stats.json
	// has no AGI column either, so AGIBonus is a stand-in too.
	liveDress := func(agiBonus float64) uint16 {
		c.Equip[HandSpotRight] = 7201
		c.Stance = held.Stance
		c.AGIBonus = agiBonus
		c.Equip[spotBack] = 41031     // Back, ASPD 10
		c.Equip[spotAchieve] = 26402  // Achieve, ASPD 10
		c.Equip[spotArtifact] = 48077 // Artifact, ASPD 10
		c.Equip[spotGlove] = 19900    // Glove, ASPD 10
		c.Equip[spotHead] = 47126     // Hat, ASPD 10
		p := BuildUpdateAllStatus(c, sheetOf(t, c))
		return readBattleWord(p, battleCSPD)
	}
	if got := liveDress(36); got != 40 {
		t.Fatalf("CSPD with the packet's first gear set (AGI 96, 50%% ASPD) = %d, "+
			"want 40: the rule computes 296, and the 400 floor lifts it to 400. "+
			"The live server sent 29 for this gear, so if this number moved the "+
			"floor changed - see the comment above", got)
	}
	if got := liveDress(44); got != 40 {
		t.Fatalf("CSPD after the gear change (AGI 104, 50%% ASPD) = %d, want 40: "+
			"the rule computes 252, and the 400 floor lifts it to 400. The live "+
			"server sent 25 for this gear", got)
	}
	t.Logf("CSPD with the live AGI and a 50%% set: 296 and 252 before the floor, "+
		"40 after it - the live server sent 29 and 25, which is how we know its "+
		"floor is lower than the script's 400")
}

// TestUpdateAllStatusCSPDRespondsToTheWeaponClass is the "the number moves with
// the WEAPON, not just with a flag" case. Three real item classes of three
// different weapon types, all job 1 Fighter, level 100 and nothing else worn:
//
//	101 Ice sword       ClassType Sword    -> condition 10122 -> stance 10110
//	                       BackGuard,   ASPD 1000, IncASPD 0.12, RHits 1
//	201 Lightning epee  ClassType Rapier   -> condition 105   -> stance 10210
//	                       EpeeGarde,   ASPD 1700, IncASPD 0.64, RHits 2
//	501 Javelin         ClassType Polearm  -> condition 108   -> stance 10510
//	                       BlandirCruz, ASPD 1700, IncASPD 1,    RHits 1
//
// None of the three carries its own ASPD column, so the whole difference comes
// from the stance the game picks for that weapon type - which is exactly what
// PickStance must be asked with the hands of EACH case, as zone\equip.go now
// does after a gear change.
func TestUpdateAllStatusCSPDRespondsToTheWeaponClass(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	stances := testStances(t)
	_ = stances

	// What the character carries with NOTHING in either hand.
	bare, ok := stances.PickStance(1, [2]string{"None", "None"})
	if !ok {
		t.Fatal("job 1 holding nothing has no bare-hand stance")
	}
	bareChar := Character{Handle: 1794, Name: "TesterF", Job: 1, Level: 100, Stance: bare.Stance}
	p := BuildUpdateAllStatus(bareChar, sheetOf(t, bareChar))
	emptyCSPD := readBattleWord(p, battleCSPD)
	t.Logf("CSPD with empty hands: stance %d %q, CSPD %d",
		bare.Stance, bare.Name, emptyCSPD)

	weapons := []struct {
		class uint32
		name  string
	}{
		{101, "Ice sword (Sword)"},
		{201, "Lightning epee (Rapier)"},
		{501, "Javelin (Polearm)"},
	}
	got := map[uint16]string{}
	for _, w := range weapons {
		it, ok := itemStats.For(w.class)
		if !ok {
			t.Fatalf("item class %d (%s) is not in data\\item_stats.json", w.class, w.name)
		}
		c := Character{Handle: 1794, Name: "TesterF", Job: 1, Level: 100}
		c.Equip[HandSpotRight] = w.class
		// The hands of THIS case, exactly as the equip fix asks: without this the
		// stance stays whatever it was and the test proves nothing.
		pick, ok := stances.PickStance(int(c.Job), stances.Hands(c))
		if !ok {
			t.Fatalf("job 1 holding %s (class %d) has no stance", it.ClassType, w.class)
		}
		c.Stance = pick.Stance
		pkt := BuildUpdateAllStatus(c, sheetOf(t, c))
		cspd := readBattleWord(pkt, battleCSPD)
		t.Logf("CSPD for %s: class %d ClassType %q, stance %d %q, CSPD %d",
			w.name, w.class, it.ClassType, pick.Stance, pick.Name, cspd)
		if cspd == emptyCSPD {
			t.Fatalf("%s (class %d, stance %d) reads CSPD %d, the same as empty "+
				"hands: the weapon class does not reach the number",
				w.name, w.class, pick.Stance, cspd)
		}
		got[cspd] = w.name
	}
	if len(got) != len(weapons) {
		t.Fatalf("three different weapon classes answered only %d distinct CSPD "+
			"value(s): the sheet is not following the weapon", len(got))
	}
}
