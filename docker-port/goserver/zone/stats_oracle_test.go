package zone

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"geserver/barrack"
)

// The Python server's own bytes for one real character, written by the WORKING
// builder (src\zone_server.py build_start_info), not by hand:
//
//	cmd> C:\Python313\python.exe  (run from docker-port\src, so ..\data resolves)
//	  import zone_server
//	  c = {"character_id": 1066, "name": "Grandies", "gender": 1, "job": 72,
//	       "headtype": 0, "exp": 22133655, "life": 99, "hp": 3750, "sp": 795,
//	       "equip": {0: 17740, 1: 15001, 11: 50009, 16: 26012},
//	       "stance_bytes": b"", "skill_bytes": b""}
//	  rec = zone_server.build_start_info(c, handle=1066, pos=(0, 0, 0))
//	  open(r"goserver\zone\testdata\start_info_oracle.bin", "wb").write(rec)
//
// The character is real: character_id 1066 in the CHARACTERS table (job 72,
// exp 22133655 -> level 60, hp 3750, sp 795, four worn items). The two empty
// blocks make the fixture the 485-byte record alone, so a difference can only
// be a FIELD, never a block.
const startInfoOracle = "testdata/start_info_oracle.bin"

// oracleCharacter is the same character, as the Go side sees it. MaxHP is left 0
// on purpose: the record's maximum comes from the game's formula and the job
// table, so a fixture that supplied it would test nothing.
func oracleCharacter() Character {
	const exp = 22133655
	return Character{
		Handle:   1066,
		Name:     "Grandies",
		Gender:   1,
		Job:      72,
		HeadType: 0,
		// The level the ZONE uses: barrack.LevelOf(exp), never the DB's LIFE column.
		Level:    uint8(barrack.LevelOf(exp)),
		HP:       3750,
		SP:       795,
		MaxSP:    795, // the Python builder writes the stored SP into both slots
		Exp:      exp,
		Position: [3]int32{0, 0, 0},
		Equip: [equipSpotLast]uint32{
			0: 17740, 1: 15001, 11: 50009, 16: 26012,
		},
	}
}

// TestStartInfoMatchesThePythonOracle is the test that ends the guessing for the
// basic-stat block: it compares the Go record against the bytes the WORKING
// Python server sends for the same character.
//
// It fails on any drift in the six stats' order, their width, the offsets, the
// level byte or the HP pair - all of which the owner would see as a wrong or
// empty Character Info panel rather than as an error.
func TestStartInfoMatchesThePythonOracle(t *testing.T) {
	want, err := os.ReadFile(startInfoOracle)
	if err != nil {
		// FATAL, not a skip: the fixture is committed, so a miss is the repo's
		// fault and this test must never report green having compared nothing.
		t.Fatalf("oracle fixture is missing: %v (the recipe is in the comment above)", err)
	}
	if len(want) != startInfoSize {
		t.Fatalf("the fixture is %d bytes, want %d - it must be the record alone",
			len(want), startInfoSize)
	}
	loadTestJobStats(t)

	c := oracleCharacter()
	// The record's maximum now comes from the game's own SCR_Get_MHP, which the zone
	// puts on the character before building anything (Handler.refreshSheet).
	refreshForTest(t, &c)
	if c.Level != 60 {
		t.Fatalf("barrack.LevelOf(%d) = %d, want 60 - the Go exp table and the "+
			"Python ge_tables.level_of disagree, and the sheet's max HP depends on it",
			c.Exp, c.Level)
	}

	got := BuildStartInfo(c)
	if bytes.Equal(got, want) {
		return
	}
	// TWO BYTES THE PYTHON ORACLE IS BEHIND ON, and the LIVE capture is the authority
	// for them: appearance +383 / +384 are the character's level and promotion level,
	// which the Go builder now writes (the spawn's only level for a REMOTE character,
	// and the pair the party card's rank comes from). src\zone_server.py's
	// build_start_info never wrote them, so the fixture has 0 there. Proven instead by
	// TestSpawnMatchesLiveCapture, whose fixture is the OFFICIAL server's own bytes for
	// a level-150 / promotion-5 character (u16 0x0596 there).
	oracleBehind := map[int]bool{
		siAppearance + apLevel:          true,
		siAppearance + apPromotionLevel: true,
	}
	// MAX SP is the third byte pair the oracle is behind on, for the same reason as
	// the promotion level: src\zone_server.py sends the STORED sp as the maximum,
	// while the Go side now asks SCR_Get_MSP (495 at level 60 unpromoted, against the
	// fixture's stored 795). The live captures are the authority here - 695 and 995 -
	// and TestStatusMSPReproducesTheLiveCaptures pins them.
	for o := siBasicStat + bsMaxSP; o < siBasicStat+bsMaxSP+2; o++ {
		oracleBehind[o] = true
	}
	only := true
	for i := range got {
		if got[i] != want[i] && !oracleBehind[i] {
			only = false
		}
	}
	if only {
		t.Logf("the record differs from the Python oracle ONLY at %d (+%d level) and %d "+
			"(+%d promotion): the oracle never wrote them, the live capture does",
			siAppearance+apLevel, siAppearance+apLevel,
			siAppearance+apPromotionLevel, siAppearance+apPromotionLevel)
		return
	}
	// Name the first difference in the client's own words, not as an index.
	off := 0
	for off < len(got) && off < len(want) && got[off] == want[off] {
		off++
	}
	t.Fatalf("record differs at +%d (%s): go=%#02x python=%#02x\n%s",
		off, startInfoField(off), got[off], want[off], startInfoDump(got, want))
}

// TestOracleFixtureDecodesToTheExpectedStats is a guard on the FIXTURE itself:
// if a future re-emit silently produced zeros, the comparison above would still
// pass and prove nothing.
func TestOracleFixtureDecodesToTheExpectedStats(t *testing.T) {
	want, err := os.ReadFile(startInfoOracle)
	if err != nil {
		t.Fatalf("oracle fixture is missing: %v", err)
	}
	// The client's order, for Grandies (job 72).
	expected := []uint16{80, 90, 50, 50, 60, 30} // STR CON DEX INT AGI CHA
	for i, w := range expected {
		off := siBasicStat + bsStats + i*2
		if got := binary.LittleEndian.Uint16(want[off:]); got != w {
			t.Fatalf("fixture %s (+%d) = %d, want %d", statNames[i], off, got, w)
		}
	}
	if got := want[siBasicStat+bsLevel]; got != 60 {
		t.Fatalf("fixture level = %d, want 60", got)
	}
	if got := mathFloat64(want[siBasicStat+bsMaxHP:]); got != 6300 {
		t.Fatalf("fixture maxhp = %v, want 6300", got)
	}
	if got := int64(binary.LittleEndian.Uint64(want[siBasicStat+bsNextExp:])); got != 0 {
		t.Fatalf("fixture nextexp = %d, want 0 (the Python sends 0 too)", got)
	}
}

// startInfoField names the field a byte offset falls in, so a failure reads as
// "the CON word" rather than "byte 467".
func startInfoField(off int) string {
	switch {
	case off < siName:
		return "handle"
	case off < siAppearance:
		return "name"
	case off < siInitPos:
		return fmt.Sprintf("appearance +%d", off-siAppearance)
	case off < siBasicStat:
		return fmt.Sprintf("initPos +%d", off-siInitPos)
	case off < siStanceCount:
		rel := off - siBasicStat
		switch {
		case rel == bsLevel:
			return "basicStat level"
		case rel >= bsHP && rel < bsHP+8:
			return "basicStat hp"
		case rel >= bsMaxHP && rel < bsMaxHP+8:
			return "basicStat maxhp"
		case rel >= bsSP && rel < bsSP+2:
			return "basicStat sp"
		case rel >= bsMaxSP && rel < bsMaxSP+2:
			return "basicStat maxsp"
		case rel >= bsExp && rel < bsExp+8:
			return "basicStat exp"
		case rel >= bsNextExp && rel < bsNextExp+8:
			return "basicStat nextexp"
		case rel >= bsStats && rel < bsStats+statCount*2:
			return "basicStat " + statNames[(rel-bsStats)/2]
		}
		return fmt.Sprintf("basicStat +%d", rel)
	}
	return fmt.Sprintf("counts/blocks +%d", off-siStanceCount)
}

// startInfoDump shows the two records side by side at the interesting offsets.
func startInfoDump(got, want []byte) string {
	offsets := []struct {
		name string
		off  int
		n    int
	}{
		{"level", siBasicStat + bsLevel, 1},
		{"hp", siBasicStat + bsHP, 8},
		{"maxhp", siBasicStat + bsMaxHP, 8},
		{"sp", siBasicStat + bsSP, 2},
		{"maxsp", siBasicStat + bsMaxSP, 2},
		{"exp", siBasicStat + bsExp, 8},
		{"stats", siBasicStat + bsStats, statCount * 2},
	}
	out := ""
	for _, f := range offsets {
		end := f.off + f.n
		if end > len(got) || end > len(want) {
			continue
		}
		out += fmt.Sprintf("  %-6s go=% x  python=% x\n", f.name, got[f.off:end], want[f.off:end])
	}
	return out
}
