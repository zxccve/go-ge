package zone

import (
	"encoding/binary"
	"testing"
)

// stanceDir is resolved the same way the server resolves it: GE_XML_DIR first,
// then the repository layout. No hardcoded Windows path - the server is meant to
// move to a Linux VPS, and a test that needs D:\ would not run there.
func stanceDir() string { return XMLDir() }

// TestActiveStanceIsBareHanded is where a remote player's spawn stance comes from:
// the FIRST stance of the job's first BARE-HANDED condition in
// datatable_stancecondition.xml (the rule the client uses itself, and what
// src\stance_data.py:264-298 sends). No weapon is ever equipped on this side, so
// bare hands is the only case that can be decided here.
//
// job 1 Fighter -> condition 100 -> stance 10010 "Bareknuckle"
// job 3 Scout   -> condition 300 -> stance 30110 "FirstAid"
// job 5 Warlock -> condition 500 -> stance 20210 "PossessionFire"
func TestActiveStanceIsBareHanded(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	for _, tc := range []struct {
		name string
		job  int
		want uint32
	}{
		{"Fighter", 1, 10010},
		{"Scout", 3, 30110},
		{"Warlock", 5, 20210},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := table.ActiveStance(tc.job)
			if !ok {
				t.Fatalf("job %d has no bare-handed stance condition", tc.job)
			}
			if got != tc.want {
				t.Fatalf("active stance = %d, want %d", got, tc.want)
			}
			// The id must also be one the job's own stance list carries, or the
			// client cannot resolve it at all.
			found := false
			for _, sid := range table.JobStances(tc.job) {
				if uint32(sid) == got {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("stance %d is not in job %d's stance list", got, tc.job)
			}
		})
	}
	if _, ok := table.ActiveStance(99999); ok {
		t.Fatal("an unknown job answered a stance")
	}
}

// TestStanceCountsMatchTheOracle pins the counts the PYTHON server sent for the
// three characters that crashed the Go zone on 2026-10-25 (zone_console.log,
// 10:58): job 3 -> 12 stances / 54 skills, job 1 -> 30 / 118, job 5 -> 15 / 55.
//
// Matching the oracle exactly is the point: a count that is merely "non-zero"
// would still leave the client with a stance list it cannot use.
func TestStanceCountsMatchTheOracle(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}

	for _, tc := range []struct {
		name            string
		job             int
		stances, skills int
	}{
		{"Jin (job 3)", 3, 12, 54},
		{"Fighter (job 1)", 1, 30, 118},
		{"Warlock (job 5)", 5, 15, 55},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stances := table.JobStances(tc.job)
			skills := table.JobSkills(tc.job)
			if len(stances) != tc.stances {
				t.Fatalf("%d stance(s), want %d: %v", len(stances), tc.stances, stances)
			}
			if len(skills) != tc.skills {
				t.Fatalf("%d skill(s), want %d", len(skills), tc.skills)
			}

			// The record sizes the client reads must match the counts in the
			// header, or the packet that follows misaligns.
			blk := table.BuildStanceRecords(tc.job)
			if len(blk) != tc.stances*14 {
				t.Fatalf("stance block is %d bytes, want %d (14 per record)",
					len(blk), tc.stances*14)
			}
			skl := table.BuildSkillRecords(tc.job)
			if len(skl) != tc.skills*10 {
				t.Fatalf("skill block is %d bytes, want %d (10 per record)",
					len(skl), tc.skills*10)
			}
		})
	}
}

// TestStartInfoCarriesTheBlocks is the regression test for the crash: the record
// must be longer than 485 when stances exist, and the two counts must agree with
// the number of records actually appended.
func TestStartInfoCarriesTheBlocks(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}

	c := Character{Handle: 34, Name: "Jin", Job: 3, Level: 100}
	c.Stances = table.BuildStanceRecords(3)
	c.Skills = table.BuildSkillRecords(3)
	rec := BuildStartInfo(c)

	want := startInfoSize + len(c.Stances) + len(c.Skills)
	if len(rec) != want {
		t.Fatalf("record is %d bytes, want %d (485 + %d stance bytes + %d skill bytes)",
			len(rec), want, len(c.Stances), len(c.Skills))
	}
	if n := int(binary.LittleEndian.Uint16(rec[siStanceCount:])); n != len(c.Stances)/14 {
		t.Fatalf("stance count = %d, want %d", n, len(c.Stances)/14)
	}
	if n := int(binary.LittleEndian.Uint16(rec[siSkillCount:])); n != len(c.Skills)/10 {
		t.Fatalf("skill count = %d, want %d", n, len(c.Skills)/10)
	}

	// And the whole packet must grow with them: the Python oracle sent 4,559
	// bytes for three characters, not 1,491.
	pkt := BuildStartGameOK([][]byte{rec})
	if len(pkt) != headerSize+want {
		t.Fatalf("packet is %d bytes, want %d", len(pkt), headerSize+want)
	}
}

// TestLoadStancesRejectsAWrongFolder stops a silent empty table: a zone with no
// stances crashes the client, so a bad path must be an error, not zero rows.
func TestLoadStancesRejectsAWrongFolder(t *testing.T) {
	if _, err := LoadStances(t.TempDir()); err == nil {
		t.Fatal("an empty folder was accepted as a stance table")
	}
}
