package zone

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
)

// The LIVE SERVER's own ZC_UPDATE_ALL_STATUS, byte for byte.
//
// `testdata\live_updateallstatus.bin` is ONE real packet, cut unchanged out of
// the live reference zone's stream:
//
//	captures\charpanel2\20261029-165952\s2c_7062_60772.bin, +54046, 125 bytes
//
// It is a level-150 3rdScout (job 10003) with handle 0x000B6E06 - the same job
// whose base stats our own table carries (STR 30 CON 60 DEX 70 INT 80 AGI 70
// CHA 50), which is what makes a byte-for-byte comparison possible at all.
//
// WHY THIS FILE EXISTS, AND THE TRAP IT GUARDS. That same packet is printed by
// the project's capture tool as "123 bytes", because analyze_capture.split_packets
// reports the length of a FIXED-size packet WITHOUT its 2 command bytes
// (`total - 2`). Reading that 123 as the wire size drops rPsy (the 5th
// resistance, at +123) and makes the two BATTLE_STAT layouts below look
// interchangeable. All 83 live 3684 packets across the 13 captures in
// `captures\` are 125 bytes - the same size this package sends.
const liveStatusFile = "testdata/live_updateallstatus.bin"

// The live packet's offsets, named once. They are the constants in status.go, so
// a change to a block boundary breaks this file instead of quietly moving a
// field on the wire.
const (
	liveCmd     = 0  // 2
	liveHandle  = 2  // 4
	liveLV      = 6  // 1
	liveHP      = 7  // 8, double
	liveMaxHP   = 15 // 8, double
	liveSP      = 23 // 2
	liveMaxSP   = 25 // 2
	liveExp     = 27 // 8, int64
	liveNextExp = 35 // 8, int64
	liveStats   = 43 // 6 x WORD: STR CON DEX INT AGI CHA
	liveBM      = 55 // 6 x int32: STR CON DEX INT AGI CHA modifiers
	liveBattle  = 79 // BATTLE_STAT
	liveResist  = 115
)

// readLiveStatus loads the captured packet, or fails: the file is committed
// evidence, so a miss is the repository's fault, and a skip would report green
// having compared nothing.
func readLiveStatus(t *testing.T) []byte {
	t.Helper()
	p, err := os.ReadFile(liveStatusFile)
	if err != nil {
		t.Fatalf("the live 3684 evidence is missing or unreadable at %s: %v",
			liveStatusFile, err)
	}
	return p
}

func liveWord(p []byte, at int) uint16 {
	return binary.LittleEndian.Uint16(p[at:])
}

func liveDword(p []byte, at int) uint32 {
	return binary.LittleEndian.Uint32(p[at:])
}

func liveDouble(p []byte, at int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(p[at:]))
}

// TestLiveUpdateAllStatusIs125Bytes is the length half, against the live bytes
// rather than against a table. The client's cmd table says 125 (a fixed packet,
// so the number is the whole packet, command included), and so does the wire.
// The capture tool's "123" is that number minus the 2 command bytes.
func TestLiveUpdateAllStatusIs125Bytes(t *testing.T) {
	p := readLiveStatus(t)

	if len(p) != 125 {
		t.Fatalf("the live 3684 is %d bytes, want 125", len(p))
	}
	if UpdateAllStatusSize != len(p) {
		t.Fatalf("UpdateAllStatusSize = %d but the live server sends %d bytes - "+
			"our packet would be %d byte(s) of the wrong length",
			UpdateAllStatusSize, len(p), UpdateAllStatusSize-len(p))
	}
	if got := liveWord(p, liveCmd); got != CmdZCUpdateAllStatus {
		t.Fatalf("cmd = %d, want %d", got, CmdZCUpdateAllStatus)
	}
	// The 2 bytes a 123-byte reading throws away ARE rPsy: the block that starts
	// at +115 is ten bytes wide, so it ends at +125, not +123.
	if got := UpdateAllStatusSize - updResistAt; got != resistanceStatLen {
		t.Fatalf("the resistance block is %d bytes on the wire, want %d",
			got, resistanceStatLen)
	}
	if got := int16(liveWord(p, liveResist+8)); got != 78 {
		t.Fatalf("rPsy (the last 2 bytes, +123) = %d, want the live 78 - if this "+
			"reads as something else the packet tail is not where we think it is", got)
	}
}

// TestLiveUpdateAllStatusFieldMap decodes the captured packet at OUR offsets and
// checks every value against the rule that produced it, so the map in status.go
// is pinned to real bytes instead of to a comment.
//
// It also proves the 34-byte BATTLE_STAT is WRONG for the live wire. That layout
// puts CATK in TWO bytes, so DR/DEF/HR land 2 bytes earlier and HR would read the
// live DEF (205). SCR_Get_CHR caps HR at 100 (`get_status_pc.scp:1343`) and all
// 83 live packets carry exactly 100 there, so a 205 reading is not a style
// difference, it is an impossible number.
func TestLiveUpdateAllStatusFieldMap(t *testing.T) {
	p := readLiveStatus(t)

	if got := liveDword(p, liveHandle); got != 0x000B6E06 {
		t.Fatalf("handle = %#x, want 0x000B6E06", got)
	}
	if got := p[liveLV]; got != 150 {
		t.Fatalf("LV = %d, want the live 150", got)
	}
	if got, want := liveDouble(p, liveHP), 135400.0; got != want {
		t.Fatalf("HP = %v, want the live %v (a double at +7)", got, want)
	}
	if got, want := liveDouble(p, liveMaxHP), 124030.0; got != want {
		t.Fatalf("maxHP = %v, want the live %v (a double at +15)", got, want)
	}
	if got, want := liveWord(p, liveSP), uint16(995); got != want {
		t.Fatalf("SP = %d, want the live %d", got, want)
	}
	if got, want := liveWord(p, liveMaxSP), uint16(995); got != want {
		t.Fatalf("maxSP = %d, want the live %d", got, want)
	}
	if got, want := int64(binary.LittleEndian.Uint64(p[liveExp:])), int64(59487183514); got != want {
		t.Fatalf("EXP = %d, want the live %d", got, want)
	}
	// The six base stats are job 10003's own row in data\job_stats.json.
	wantStats := [6]uint16{30, 60, 70, 80, 70, 50}
	for i, want := range wantStats {
		if got := liveWord(p, liveStats+i*2); got != want {
			t.Fatalf("base stat %d at +%d = %d, want the live %d",
				i, liveStats+i*2, got, want)
		}
	}
	// The BM/WP block is GEAR: it moved 16/16/16/16/36/36 -> 19/19/19/21/39/39
	// when the owner changed equipment during this very capture, which is how we
	// know it is not a constant.
	for i, want := range []uint32{16, 16, 16, 16, 36, 36} {
		if got := liveDword(p, liveBM+i*4); got != want {
			t.Fatalf("bmStat[%d] at +%d = %d, want the live %d", i, liveBM+i*4, got, want)
		}
	}

	// BATTLE_STAT, field by field, in the game's own units.
	words := []struct {
		name string
		at   int
		want uint16
	}{
		{"AR", battleAR, 33},
		{"DR", battleDR, 61},
		{"DEF", battleDEF, 205},
		{"HR", battleHR, 100},
		{"PR", battlePR, 16},
		{"CSPD", battleCSPD, 35},
		{"BLK", battleBLK, 0},
		{"CRT", battleCRT, 15},
		{"IMP", battleIMP, 0},
		{"IgnoreDEFPer", battleIgnoreDEFPer, 10},
		{"DefIP", battleDefIP, 2},
		{"FireIP", battleFireIP, 2},
		{"IceIP", battleIceIP, 2},
		{"LgtIP", battleLgtIP, 2},
		{"PsyIP", battlePsyIP, 2},
		{"MSPD", battleMSPD, 562},
	}
	for _, f := range words {
		if got := liveWord(p, updBattleAt+f.at); got != f.want {
			t.Fatalf("%s at +%d = %d, want the live %d", f.name, updBattleAt+f.at, got, f.want)
		}
	}
	if got := liveDword(p, updBattleAt+battleCATK); got != 0 {
		t.Fatalf("CATK = %d, want the live 0 (this character holds nothing)", got)
	}

	// The rules that make those values legal, which is what proves the offsets.
	if hr := liveWord(p, updBattleAt+battleHR); hr > 100 {
		t.Fatalf("HR = %d: SCR_Get_CHR cannot return more than 100", hr)
	}
	if mspd := liveWord(p, updBattleAt+battleMSPD); mspd < 400 || mspd > 1450 {
		t.Fatalf("MSPD = %d: SCR_Get_MSPD is 469 x the stance, capped at 650/750 "+
			"or 1450, so it cannot be outside 400..1450", mspd)
	}

	// THE REFUTED LAYOUT: BATTLE_STAT with a 16-bit CATK (source2's header,
	// geZCTypes.h:235). Every field from DR on moves 2 bytes earlier, so HR would
	// read this packet's DEF (205) - and SCR_Get_CHR cannot return more than 100.
	if got := liveWord(p, updBattleAt+8); got != 205 {
		t.Fatalf("the 34-byte reading of HR = %d, expected 205 - the trap this "+
			"test documents has moved, so re-derive the map before trusting it", got)
	}

	// RESISTANCE_STAT: five SIGNED shorts, each inside the script's own clamp.
	resists := []struct {
		name string
		at   int
		want int16
	}{
		{"rFire", 0, 67},
		{"rIce", 2, 67},
		{"rLight", 4, 67},
		{"rState", 6, 51},
		{"rPsy", 8, 78},
	}
	for _, r := range resists {
		got := int16(liveWord(p, updResistAt+r.at))
		if got != r.want {
			t.Fatalf("%s at +%d = %d, want the live %d", r.name, updResistAt+r.at, got, r.want)
		}
		if got > 100 || got < -100 {
			t.Fatalf("%s = %d: every SCR_Get_R* clamps to -100..100", r.name, got)
		}
	}
}

// TestOurUpdateAllStatusMatchesTheLiveOne builds OUR packet for a character
// shaped like the captured one and compares it with the live bytes offset by
// offset.
//
// The comparison is byte-for-byte, with ONE allowance: the offsets below name the
// bytes that MUST differ and say why. Those are the ones this server cannot
// answer from its own data - the live character's gear, its HP, the live server's
// exp table - so an honest test lists them instead of hiding them behind a
// "shape" check. Everything not named is required to be identical, which is what
// pins the layout: a single moved field fails with the offset printed.
func TestOurUpdateAllStatusMatchesTheLiveOne(t *testing.T) {
	loadTestJobStats(t)
	loadTestItemStats(t)
	_ = testStances(t) // the sheet source carries its own; kept for XMLDir() side effects

	// Character 0x000B6E06 as the live packet describes it. Level, job, SP and EXP
	// are the live values; the stance and the gear are NOT in the packet, so every
	// field they feed is listed in the allow list below.
	c := Character{
		Handle: 0x000B6E06,
		Name:   "live",
		Job:    10003, // 3rdScout: STR 30 CON 60 DEX 70 INT 80 AGI 70 CHA 50
		Level:  150,
		SP:     995,
		MaxSP:  995,
		Exp:    59487183514,
	}

	live := readLiveStatus(t)
	requireStatus(t)
	ours, sheet := StatusPacketFor(c, defaultStatusForTests)
	if len(ours) != len(live) {
		t.Fatalf("our 3684 is %d bytes, the live one is %d", len(ours), len(live))
	}

	// allowed names the bytes that may differ, by the input we do not have.
	allowed := []struct {
		name string
		from int
		to   int // exclusive
	}{
		{"HP and maxHP: the live character's HP and its gear's max-HP terms, " +
			"which our SCR_Get_MHP does not carry (zone\\stats.go)", liveHP, liveMaxHP + 8},
		{"nextEXP: the live server reads gePCLevelTable::GetExp(LV+1); we send 0 " +
			"(basicStatBlock says so)", liveNextExp, liveNextExp + 8},
		{"BUFF_MODIFIER_STAT: GetBMValue(.._BM) + Get.._WP(), both from the worn " +
			"items, and this server has no BM/mastery path", liveBM, liveBM + 24},
		{"BATTLE_STAT: AR, CATK, DR, DEF, HR, PR, CSPD, CRT, IgnoreDEFPer, DefIP, " +
			"the four element IPs and MSPD all come from the live character's gear " +
			"and stance, which the packet does not reveal. MSPD is the closest miss: " +
			"ours is 469 (SCR_Get_MSPD's own constant, zone\\speed.go) and the live " +
			"one is 562 = floor(469 * 120 / 100), i.e. the same rule with a +20% " +
			"MSPD_BM/achieve term this server has no input for", liveBattle, liveBattle + 36},
		{"RESISTANCE_STAT: the five resists are stance + worn-item columns + INT",
			liveResist, liveResist + 10},
	}

	inAllowed := func(at int) string {
		for _, a := range allowed {
			if at >= a.from && at < a.to {
				return a.name
			}
		}
		return ""
	}

	var unexpected []string
	for at := 0; at < len(live); at++ {
		if ours[at] == live[at] {
			continue
		}
		if inAllowed(at) == "" {
			unexpected = append(unexpected, fmt.Sprintf("%s@%d (ours %#02x, live %#02x)",
				fieldAt(at), at, ours[at], live[at]))
			if len(unexpected) == 8 {
				break
			}
		}
	}
	if len(unexpected) > 0 {
		t.Fatalf("our packet differs from the live one at offsets we do NOT allow "+
			"to differ: %v", unexpected)
	}

	// The bytes that must be identical, stated one by one so the test says WHY
	// each one matters.
	if got := liveWord(ours, 0); got != liveWord(live, 0) {
		t.Fatalf("cmd = %d, want %d", got, liveWord(live, 0))
	}
	if got := liveDword(ours, liveHandle); got != liveDword(live, liveHandle) {
		t.Fatalf("handle = %#x, want %#x", got, liveDword(live, liveHandle))
	}
	if ours[liveLV] != live[liveLV] {
		t.Fatalf("LV = %d, want the live %d", ours[liveLV], live[liveLV])
	}
	for i := 0; i < 6; i++ {
		at := liveStats + i*2
		if got, want := liveWord(ours, at), liveWord(live, at); got != want {
			t.Fatalf("base stat %d at +%d = %d, want the live %d - the six base "+
				"stats and the level 3684 carries must be the ones START_INFO sent",
				i, at, got, want)
		}
	}
	for _, at := range []int{liveSP, liveMaxSP} {
		if got, want := liveWord(ours, at), liveWord(live, at); got != want {
			t.Fatalf("word at +%d = %d, want the live %d", at, got, want)
		}
	}
	if got, want := int64(binary.LittleEndian.Uint64(ours[liveExp:])), int64(binary.LittleEndian.Uint64(live[liveExp:])); got != want {
		t.Fatalf("EXP = %d, want the live %d", got, want)
	}
	// BLK and IMP are 0 in the live packet and 0 in ours: the two fields where
	// "we have no rule" and "the live server has no bonus" agree.
	for _, f := range []struct {
		name string
		at   int
	}{{"BLK", battleBLK}, {"IMP", battleIMP}} {
		if got, want := liveWord(ours, updBattleAt+f.at), liveWord(live, updBattleAt+f.at); got != want {
			t.Fatalf("%s = %d, want the live %d", f.name, got, want)
		}
	}

	t.Logf("identical bytes: %d of %d; our sheet: ATK %d DEF %d AR %d DR %d HR %d "+
		"CSPD %d MSPD %d; live sheet: AR %d CATK %d DR %d DEF %d HR %d CSPD %d MSPD %d",
		countEqual(ours, live), len(live),
		int(sheet.ATK), int(sheet.DEF), int(sheet.AR), int(sheet.DR),
		int(sheet.HR), int(sheet.CSPD), int(sheet.MSPD),
		liveWord(live, updBattleAt+battleAR), liveDword(live, updBattleAt+battleCATK),
		liveWord(live, updBattleAt+battleDR), liveWord(live, updBattleAt+battleDEF),
		liveWord(live, updBattleAt+battleHR), liveWord(live, updBattleAt+battleCSPD),
		liveWord(live, updBattleAt+battleMSPD))
}

// fieldAt names a packet offset for a failure message, so a diff says "CATK@81"
// rather than "byte 81".
func fieldAt(at int) string {
	switch {
	case at < 2:
		return "cmd"
	case at < 6:
		return "handle"
	case at == liveLV:
		return "LV"
	case at >= liveHP && at < liveMaxHP:
		return "HP"
	case at >= liveMaxHP && at < liveSP:
		return "maxHP"
	case at >= liveSP && at < liveMaxSP:
		return "SP"
	case at >= liveMaxSP && at < liveExp:
		return "maxSP"
	case at >= liveExp && at < liveNextExp:
		return "EXP"
	case at >= liveNextExp && at < liveStats:
		return "nextEXP"
	case at >= liveStats && at < liveBM:
		return "baseStats"
	case at >= liveBM && at < liveBattle:
		return "bmStat"
	case at >= liveBattle && at < liveResist:
		return battleFieldName(at - updBattleAt)
	case at >= liveResist:
		return "resist"
	}
	return "?"
}

// battleFieldName is the BATTLE_STAT field that starts at `at`, for the same
// reason: an offset is not a name.
func battleFieldName(at int) string {
	for _, f := range []struct {
		name string
		at   int
		w    int
	}{
		{"AR", battleAR, 2}, {"CATK", battleCATK, 4}, {"DR", battleDR, 2},
		{"DEF", battleDEF, 2}, {"HR", battleHR, 2}, {"PR", battlePR, 2},
		{"CSPD", battleCSPD, 2}, {"BLK", battleBLK, 2}, {"CRT", battleCRT, 2},
		{"IMP", battleIMP, 2}, {"IgnoreDEFPer", battleIgnoreDEFPer, 2},
		{"DefIP", battleDefIP, 2}, {"FireIP", battleFireIP, 2},
		{"IceIP", battleIceIP, 2}, {"LgtIP", battleLgtIP, 2},
		{"PsyIP", battlePsyIP, 2}, {"MSPD", battleMSPD, 2},
	} {
		if at >= f.at && at < f.at+f.w {
			return f.name
		}
	}
	return "battle"
}

func countEqual(a, b []byte) int {
	n := 0
	for i := range a {
		if i < len(b) && a[i] == b[i] {
			n++
		}
	}
	return n
}
