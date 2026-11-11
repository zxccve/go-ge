package zone

import (
	"bytes"
	"encoding/binary"
	"log"
	"strings"
	"testing"
)

// jobStatsFile is the emitted class base stats, relative to this package's test
// working directory (goserver\zone -> docker-port\data).
const jobStatsFile = "../../data/job_stats.json"

// loadTestJobStats installs data\job_stats.json for one test and puts the
// package state back afterwards, so a later test cannot inherit it.
func loadTestJobStats(t *testing.T) *JobStats {
	t.Helper()
	table, err := LoadJobStats(jobStatsFile)
	if err != nil {
		// FATAL, not a skip: the file is committed data, so a miss is the repo's
		// fault, and a skipped test here would report green having checked nothing.
		t.Fatalf("class base stats are missing or unreadable at %s: %v", jobStatsFile, err)
	}
	old := jobStats
	SetJobStats(table)
	t.Cleanup(func() { SetJobStats(old) })
	return table
}

// TestJobStatsFileCarriesTheClientsSixNames pins the FILE, not the code: the six
// JSON keys are the six column names the client's own job table uses, and they
// are what statNames reads them by. A renamed column would decode as 0 and the
// sheet would show a plausible-but-wrong zero.
func TestJobStatsFileCarriesTheClientsSixNames(t *testing.T) {
	table := loadTestJobStats(t)

	if table.Len() < 400 {
		t.Fatalf("table holds %d jobs, want the client's 413 - a short file means "+
			"the extractor dropped classes", table.Len())
	}

	// Job 72 is Grandies, the class the Python server confirmed against the DB.
	got, ok := table.For(72)
	if !ok {
		t.Fatalf("job 72 (Grandies) is not in %s", jobStatsFile)
	}
	want := JobStat{STR: 80, CON: 90, DEX: 50, INT: 50, AGI: 60, CHA: 30}
	if got != want {
		t.Fatalf("job 72 = %+v, want %+v", got, want)
	}
}

// TestSheetUsesTheClientsStatOrder is the order test. It reads the SIX WORDS out
// of a real record and compares them one at a time, so swapping two stats - the
// failure that shows plausible numbers to the owner and is never doubted - fails
// here by name.
func TestSheetUsesTheClientsStatOrder(t *testing.T) {
	loadTestJobStats(t)

	c := Character{Name: "Grandies", Job: 72, Level: 60, HP: 3750, SP: 795, Exp: 22133655}
	refreshForTest(t, &c)
	b := BuildStartInfo(c)

	// The client's own order at the basic-stat block's offset 37.
	want := []struct {
		name string
		word uint16
	}{
		{"STR", 80}, {"CON", 90}, {"DEX", 50}, {"INT", 50}, {"AGI", 60}, {"CHA", 30},
	}
	for i, w := range want {
		off := siBasicStat + bsStats + i*2
		if got := binary.LittleEndian.Uint16(b[off:]); got != w.word {
			t.Fatalf("%s (+%d) = %d, want %d - the six words are in the WRONG "+
				"order or the wrong job", w.name, off, got, w.word)
		}
	}
	// The name list and the writer must not drift apart either.
	for i, name := range statNames {
		if name != want[i].name {
			t.Fatalf("statNames[%d] = %q, want %q", i, name, want[i].name)
		}
	}

	// Width: six WORDs occupy exactly 12 bytes, and the stance count follows at
	// once. A DWORD write would land on the counts and lose the stance list.
	if siStanceCount != siBasicStat+bsStats+statCount*2 {
		t.Fatalf("the stats block does not end where the stance count begins: "+
			"%d + %d + %d != %d", siBasicStat, bsStats, statCount*2, siStanceCount)
	}

	// MaxHP is the game's own formula for this character: SCR_Get_MHP, which answers
	// CON*10 + Lv*CON for an unpromoted character - and the zone puts it on the
	// character (Handler.refreshSheet) before this record is built.
	if got := mathFloat64(b[siBasicStat+bsMaxHP:]); got != 6300 {
		t.Fatalf("maxhp = %v, want 6300 (CON 90 * 10 + level 60 * CON 90)", got)
	}
	// Stored HP below the maximum is written unchanged.
	if got := mathFloat64(b[siBasicStat+bsHP:]); got != 3750 {
		t.Fatalf("hp = %v, want the stored 3750 (it is below the maximum)", got)
	}
}

// TestHPAboveTheMaximumIsClamped pins the clamp: Grace is stored at 17300 with a
// formula maximum of 5640, and a bar past its own end is what an unclamped write
// draws. Same rule as the Python builder (hp = min(hp, max_hp)).
func TestHPAboveTheMaximumIsClamped(t *testing.T) {
	loadTestJobStats(t)

	// Grace: job 6, level 131, CON 40 -> SCR_Get_MHP answers 40*10 + 131*40 = 5640
	// for a character with no promotion, which is what refreshForTest puts on her.
	c := Character{Name: "Grace", Job: 6, Level: 131, HP: 17300}
	refreshForTest(t, &c)
	b := BuildStartInfo(c)

	if got := mathFloat64(b[siBasicStat+bsMaxHP:]); got != 5640 {
		t.Fatalf("maxhp = %v, want 5640", got)
	}
	if got := mathFloat64(b[siBasicStat+bsHP:]); got != 5640 {
		t.Fatalf("hp = %v, want it clamped to the maximum 5640", got)
	}
}

// TestUnknownJobSendsZerosAndSaysSoTheOnce is the no-panic test: a job id that
// is not in the file must answer six zeros, keep the character's HP (a zero
// maximum would draw an EMPTY bar), and name the id in the log exactly once.
func TestUnknownJobSendsZerosAndSaysSoTheOnce(t *testing.T) {
	table := loadTestJobStats(t)

	var logged bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	const unknown = 424242
	c := Character{Name: "Nobody", Job: unknown, Level: 40, HP: 1000, MaxHP: 1000}

	// Twice, so the "once" part of the log rule is tested too.
	first := BuildStartInfo(c)
	second := BuildStartInfo(c)

	for i := 0; i < statCount; i++ {
		off := siBasicStat + bsStats + i*2
		if got := binary.LittleEndian.Uint16(first[off:]); got != 0 {
			t.Fatalf("stat %d (+%d) = %d for an unknown job, want 0", i, off, got)
		}
	}
	// The HP pair falls back to the store's stand-in, never to zero.
	if got := mathFloat64(first[siBasicStat+bsMaxHP:]); got != 1000 {
		t.Fatalf("maxhp = %v for an unknown job, want the stored 1000 (a zero "+
			"maximum draws an empty bar)", got)
	}
	if got := mathFloat64(first[siBasicStat+bsHP:]); got != 1000 {
		t.Fatalf("hp = %v for an unknown job, want the stored 1000", got)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the same character built twice differs - the table is not stable")
	}

	msg := logged.String()
	if !strings.Contains(msg, "424242") || !strings.Contains(msg, "no entry in job_stats.json") {
		t.Fatalf("the log does not name the missing job:\n%s", msg)
	}
	if n := strings.Count(msg, "424242"); n != 1 {
		t.Fatalf("the missing job was logged %d times, want once:\n%s", n, msg)
	}
	// And the same table keeps answering zeros after the log line.
	if stat, ok := table.For(unknown); ok || stat != (JobStat{}) {
		t.Fatalf("For(unknown) = %+v, %v; want zeros, false", stat, ok)
	}
}

// TestNoTableAtAllIsQuietAndKeepsTheStoredHP covers the missing FILE, which is
// the state the zone starts in when job_stats.json is not there: no panic, six
// zeros, and the HP pair still the store's.
func TestNoTableAtAllIsQuietAndKeepsTheStoredHP(t *testing.T) {
	old := jobStats
	SetJobStats(nil)
	t.Cleanup(func() { SetJobStats(old) })

	c := Character{Name: "Arcis", Job: 72, Level: 60, HP: 500, MaxHP: 700}
	b := BuildStartInfo(c)

	for i := 0; i < statCount; i++ {
		if got := binary.LittleEndian.Uint16(b[siBasicStat+bsStats+i*2:]); got != 0 {
			t.Fatalf("stat %d = %d with no table, want 0", i, got)
		}
	}
	if got := mathFloat64(b[siBasicStat+bsMaxHP:]); got != 700 {
		t.Fatalf("maxhp = %v with no table, want the stored 700", got)
	}
	if got := mathFloat64(b[siBasicStat+bsHP:]); got != 500 {
		t.Fatalf("hp = %v with no table, want the stored 500", got)
	}
}

// TestRecordStillCarriesTheStanceAndSkillBlocks is the length guard: the six
// stats must not have grown the fixed body or moved the blocks, and the packet's
// length word must still equal the packet length.
func TestRecordStillCarriesTheStanceAndSkillBlocks(t *testing.T) {
	loadTestJobStats(t)

	stances := make([]byte, 14*2)
	for i := range stances {
		stances[i] = byte(i + 1)
	}
	skills := make([]byte, 10)
	for i := range skills {
		skills[i] = byte(0xA0 + i)
	}
	c := Character{
		Name: "Grandies", Job: 72, Level: 60, HP: 3750, SP: 795, Exp: 22133655,
		Stances: stances, Skills: skills,
	}
	rec := BuildStartInfo(c)

	if want := startInfoSize + len(stances) + len(skills); len(rec) != want {
		t.Fatalf("record is %d bytes, want %d (485 + %d stance + %d skill)",
			len(rec), want, len(stances), len(skills))
	}
	if got := binary.LittleEndian.Uint16(rec[siStanceCount:]); got != 2 {
		t.Fatalf("stance count = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(rec[siSkillCount:]); got != 1 {
		t.Fatalf("skill count = %d, want 1", got)
	}
	// The blocks really moved: the first stance byte is the first appended byte.
	if rec[startInfoSize] != 1 {
		t.Fatalf("the stance block does not start at %d", startInfoSize)
	}

	pkt := BuildStartGameOK([][]byte{rec})
	if got := binary.LittleEndian.Uint16(pkt[2:]); int(got) != len(pkt) {
		t.Fatalf("length word = %d, want %d (it counts the whole packet)", got, len(pkt))
	}
	if want := headerSize + len(rec); len(pkt) != want {
		t.Fatalf("packet is %d bytes, want %d", len(pkt), want)
	}
}
