package zone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WHICH STANCE a character is put into, and why it matters.
//
// The remote player stood in a T-pose on the other client (2026-10-28, two real
// clients). The client draws a standing character with the motion NAMED by the
// tables: the job row's STD for the standing pose, and the stance row's ASTD for the
// stance's idle (GetPropertyString("Job", job, "STD") / ("Stance", stance, "ASTD"),
// PC.cpp:476-521). A name the client cannot resolve makes PlaySTDAnim return false
// and NOTHING plays, so the actor keeps whatever it had.
//
// Two separate faults were possible and this file separates them:
//   - the NAME does not resolve (rejected by the chooser, tested here), and
//   - the WRONG stance was chosen: the old rule ignored Character.Equip, so a
//     Fighter holding a rapier and a shield stood in "Bareknuckle".
//
// MEASURED ON THE SHIPPED DATA, 2026-10-28: all 413 job rows have a usable STD and
// all 661 stance rows a usable ASTD, so the name hypothesis is DEAD for this data -
// see TestNoShippedMotionNameIsUnusable. What was really wrong for a weapon holder
// is the choice, which is what TestPickStancePrefersTheWornWeapon pins.

// TestPickStanceChoosesAStanceWhoseIdleResolves: for a job with SEVERAL candidate
// stances, the one chosen is a real stance of that job and its ASTD resolves.
//
// TABLE-DRIVEN FROM THE REAL XML, not a hand-written fixture: the ids below are the
// jobs of the owner's own characters (database `character`: Jin 34 = 3, Fighter 40 =
// 1, Warlock 38 = 5, Grandies 1066 = 72).
func TestPickStanceChoosesAStanceWhoseIdleResolves(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable: %v", err)
	}
	if len(table.rows) < 100 {
		t.Fatalf("only %d stance row(s) loaded - the fixture is not the shipped table",
			len(table.rows))
	}

	for _, tc := range []struct {
		name string
		job  int
	}{
		{"Fighter", 1},   // 30 stances over 19 conditions
		{"Scout", 3},     // 12 stances over 9 conditions
		{"Warlock", 5},   // 15
		{"Grandies", 72}, // 7
	} {
		t.Run(tc.name, func(t *testing.T) {
			pick, ok := table.PickStance(tc.job, [2]string{"None", "None"})
			if !ok {
				t.Fatalf("job %d answered no stance at all", tc.job)
			}
			if !pick.Usable() || !MotionUsable(pick.ASTD) {
				t.Fatalf("stance %d (%s) was chosen with an unusable idle %q - that is "+
					"the T-pose by construction", pick.Stance, pick.Name, pick.ASTD)
			}
			// The id must be one the job's own stance list carries, or the client
			// cannot resolve it from the STANCE_INFO block either.
			found := false
			for _, sid := range table.JobStances(tc.job) {
				if uint32(sid) == pick.Stance {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("stance %d is not in job %d's own stance list", pick.Stance, tc.job)
			}
			// The name and the ASTD must be that stance's OWN row, not a neighbour's.
			if pick.ASTD != table.StanceASTD(pick.Stance) || pick.Name != table.StanceName(pick.Stance) {
				t.Fatalf("pick describes stance %d as (%q, %q), but its row says (%q, %q)",
					pick.Stance, pick.Name, pick.ASTD,
					table.StanceName(pick.Stance), table.StanceASTD(pick.Stance))
			}
			// And for a character holding nothing it must still be the bare-hand one,
			// which is what every character got before this change.
			if pick.LHand != "None" || pick.RHand != "None" {
				t.Fatalf("a bare-handed character got the condition needing %s/%s",
					pick.LHand, pick.RHand)
			}
		})
	}
}

// TestPickStancePrefersTheWornWeapon is the fix the awkward pose needed: a character
// holding a weapon is NOT given the bare-hand stance.
//
// The case is the owner's own Fighter (character 40, job 1), exactly as the database
// holds it: 7215 CrecementoShield in the left hand (spot 4) and 7203 CrecementoRapier
// in the right (spot 5). Both class ids are resolved through
// datatable_item_weapon.xml, the same table whose ClassType vocabulary the stance
// conditions use.
func TestPickStancePrefersTheWornWeapon(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable: %v", err)
	}
	if got := table.WeaponType(7203); got != "Rapier" {
		t.Fatalf("class 7203 reads as %q, want Rapier - the weapon table is not being read",
			got)
	}
	if got := table.WeaponType(7215); got != "Shield" {
		t.Fatalf("class 7215 reads as %q, want Shield", got)
	}

	c := Character{Handle: 40, Name: "Fighter", Job: 1}
	c.Equip[HandSpotLeft] = 7215  // shield
	c.Equip[HandSpotRight] = 7203 // rapier
	hands := table.Hands(c)
	if hands != [2]string{"Shield", "Rapier"} {
		t.Fatalf("hands = %v, want [Shield Rapier]", hands)
	}

	pick, ok := table.PickStance(1, hands)
	if !ok {
		t.Fatal("job 1 holding a shield and a rapier answered no stance")
	}
	bare, _ := table.ActiveStance(1)
	if pick.Stance == bare {
		t.Fatalf("stance %d is the BARE-HAND stance - a character holding a rapier "+
			"and a shield was given the bare-knuckle idle, which is the pose the owner "+
			"reported", pick.Stance)
	}
	if pick.RHand != "Rapier" {
		t.Fatalf("the condition chosen needs %s/%s, want the rapier in the main hand",
			pick.LHand, pick.RHand)
	}
	// Job 1's rapier condition (105) offers 10210, whose idle is the rapier guard.
	if pick.Stance != 10210 || pick.ASTD != "PC_ASTD_EPE" {
		t.Fatalf("chose %d (%s, %s), want 10210 (EpeeGarde, PC_ASTD_EPE)",
			pick.Stance, pick.Name, pick.ASTD)
	}
	if !pick.Usable() {
		t.Fatalf("the rapier stance's idle %q does not resolve", pick.ASTD)
	}

	// A two-handed case, so the rule is not "always the right hand": the Fire
	// bracelet of the owner's Warlock (38, job 5) is a single right-hand weapon.
	c2 := Character{Handle: 38, Name: "Warlock", Job: 5}
	c2.Equip[HandSpotRight] = 7222 // CrecementoFireBrc
	if got := table.Hands(c2); got != [2]string{"None", "FireBrc"} {
		t.Fatalf("Warlock's hands = %v, want [None FireBrc]", got)
	}
	p2, ok := table.PickStance(5, table.Hands(c2))
	if !ok || p2.RHand != "FireBrc" {
		t.Fatalf("Warlock got %v (ok=%v), want the FireBrc condition", p2, ok)
	}
}

// TestPickStanceRejectsAPCNoneASTD is the rejection rule: a stance whose idle motion
// is the client's own "PC\None" is SKIPPED and the next candidate is taken.
//
// THE FIXTURE IS THE REAL TABLE with ONE attribute changed: datatable_stance.xml is
// copied beside the other three files and stance 30110's ASTD is set to `PC\None`.
// Nothing else is hand-written, so the test measures the chooser, not a toy table.
// Job 3's bare-hand condition (300) offers 30110 FirstAid, 30120 Fortitudo, 30210
// InstallTrap and 30140 TacticalAssistance in that order - so the next candidate is
// 30120.
func TestPickStanceRejectsAPCNoneASTD(t *testing.T) {
	dir := stanceFixtureWithNoneASTD(t, 30110)

	table, err := LoadStances(dir)
	if err != nil {
		t.Fatalf("LoadStances(%q): %v", dir, err)
	}
	if got := table.StanceASTD(30110); got != NoneMotion {
		t.Fatalf("the fixture did not take: stance 30110's ASTD reads %q, want %q",
			got, NoneMotion)
	}
	// The poisoned row is the one the OLD rule would have taken (it is element [0] of
	// the first bare-hand condition), so this test fails on the old choice.
	if bare := firstBareStance(t, dir, 3); bare != 30110 {
		t.Fatalf("the fixture's first bare-hand stance for job 3 is %d, want 30110 - "+
			"this test would then prove nothing", bare)
	}

	pick, ok := table.PickStance(3, [2]string{"None", "None"})
	if !ok {
		t.Fatal("job 3 answered no stance once 30110 was poisoned - it should have " +
			"fallen through to the next candidate")
	}
	if pick.Stance == 30110 {
		t.Fatalf("stance 30110 was chosen although its idle is %q - the client would "+
			"play nothing and the actor would keep its bind pose", NoneMotion)
	}
	if pick.Stance != 30120 {
		t.Fatalf("chose %d (%s), want the NEXT candidate 30120 Fortitudo",
			pick.Stance, pick.Name)
	}
	if !pick.Usable() {
		t.Fatalf("the fallback's idle %q does not resolve either", pick.ASTD)
	}

	// And a JOB whose whole bare-hand condition is poisoned falls through to another
	// condition instead of answering nothing: job 72's bare-hand condition 6901 has a
	// single stance (10010 Bareknuckle).
	dir72 := stanceFixtureWithNoneASTD(t, 10010)
	table72, err := LoadStances(dir72)
	if err != nil {
		t.Fatalf("LoadStances(%q): %v", dir72, err)
	}
	p72, ok := table72.PickStance(72, [2]string{"None", "None"})
	if !ok {
		t.Fatal("job 72 answered no stance once its only bare-hand stance was poisoned")
	}
	if p72.Stance == 10010 {
		t.Fatalf("stance 10010 was chosen with an unusable idle %q", p72.ASTD)
	}
	if !p72.Usable() {
		t.Fatalf("the fallback for job 72 has an unusable idle %q", p72.ASTD)
	}
}

// TestNoShippedMotionNameIsUnusable records the measurement that killed the "the
// motion name does not resolve" hypothesis for THIS data: every job row and every
// stance row in the shipped tables names a motion the client can play.
//
// It is here so the next person does not spend a night re-checking it, and so a data
// change that introduces a `PC\None` row is caught instead of showing up as a T-pose
// in game.
func TestNoShippedMotionNameIsUnusable(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable: %v", err)
	}

	var badJobs []string
	for job, std := range table.jobSTD {
		if !MotionUsable(std) {
			badJobs = append(badJobs, itoa(job)+"="+std)
		}
	}
	if len(badJobs) > 0 {
		t.Fatalf("%d job row(s) name no motion: %v", len(badJobs), badJobs)
	}
	var badStances []string
	for id, row := range table.rows {
		if !MotionUsable(row.ASTD) {
			badStances = append(badStances, itoa(id)+"="+row.ASTD)
		}
	}
	if len(badStances) > 0 {
		t.Fatalf("%d stance row(s) name no idle motion: %v", len(badStances), badStances)
	}
	if len(table.jobSTD) < 400 || len(table.rows) < 600 {
		t.Fatalf("only %d job(s) / %d stance(s) read - the shipped tables are bigger, so "+
			"this test is not looking at them", len(table.jobSTD), len(table.rows))
	}
}

// TestSpawnLineForTheOwnersCharacters prints the exact line the zone writes at spawn
// for the two characters the lane names - Jin (character 34, job 3 Scout) and
// Grandies (character 1066, job 72) - and pins what it says.
//
// It is the line to read back from logs\zone10_live.log after a restart: a stance id,
// and both motion names with "usable" or "NOT USABLE" after each. The test asserts the
// words, so the line cannot quietly lose the half that matters.
func TestSpawnLineForTheOwnersCharacters(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable: %v", err)
	}
	for _, tc := range []struct {
		name   string
		handle uint32
		job    int
		equip  [2]uint32 // left hand, right hand (appearance spots 4 and 5)
		stance uint32    // the stance the real data gives it
		astd   string
		std    string
	}{
		// Neither character holds a weapon in the database (no wear_item row at spot
		// 4 or 5), so both keep the bare-hand stance they had - 30110 and 10010 are
		// real rows whose idle resolves.
		{"Jin", 34, 3, [2]uint32{}, 30110, "PC_ASTD_SCT", "PC_STD_SCT"},
		{"Grandies", 1066, 72, [2]uint32{}, 10010, "PC_ASTD_BKN", "PC_STD_GRANDIES"},
		// Fighter (character 40) holds 7215 CrecementoShield and 7203
		// CrecementoRapier. It stood in the BARE-HAND stance before this change; now
		// it takes job 1's rapier condition (105), whose stance 10210 EpeeGarde is
		// the rapier guard.
		{"Fighter", 40, 1, [2]uint32{7215, 7203}, 10210, "PC_ASTD_EPE", "PC_STD_FGT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Character{Handle: tc.handle, Name: tc.name, Job: uint32(tc.job)}
			c.Equip[HandSpotLeft], c.Equip[HandSpotRight] = tc.equip[0], tc.equip[1]
			hands := table.Hands(c)
			pick, ok := table.PickStance(tc.job, hands)
			if !ok {
				t.Fatalf("job %d answered no stance", tc.job)
			}
			line := pick.SpawnLine(c.Handle, c.Name, int(c.Job),
				table.JobSTD(int(c.Job)), hands)
			t.Log(line)

			if pick.Stance != tc.stance || pick.ASTD != tc.astd {
				t.Fatalf("stance %d (%s), want %d (%s)", pick.Stance, pick.ASTD,
					tc.stance, tc.astd)
			}
			if got := table.JobSTD(tc.job); got != tc.std {
				t.Fatalf("job STD %q, want %q", got, tc.std)
			}
			for _, want := range []string{
				"job " + itoa(tc.job), "stance " + itoa(int(tc.stance)),
				`stance ASTD "` + tc.astd + `" usable`,
				`job STD "` + tc.std + `" usable`,
			} {
				if !strings.Contains(line, want) {
					t.Fatalf("the spawn line does not contain %q:\n%s", want, line)
				}
			}
		})
	}
}

// TestPickStanceUnknownJob: a job with no row answers nothing rather than a number
// nobody chose.
func TestPickStanceUnknownJob(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable: %v", err)
	}
	if pick, ok := table.PickStance(99999, [2]string{"None", "None"}); ok {
		t.Fatalf("an unknown job answered stance %d", pick.Stance)
	}
	if pick, ok := table.PickStance(0, [2]string{"Sword", "None"}); ok {
		t.Fatalf("job 0 answered stance %d", pick.Stance)
	}
}

// ---- fixture helpers ----------------------------------------------------

// stanceFixtureWithNoneASTD copies the four shipped tables into a temp folder and
// rewrites ONE stance row's ASTD to the client's `PC\None`. It answers the folder.
func stanceFixtureWithNoneASTD(t *testing.T, stanceID int) string {
	t.Helper()
	src := stanceDir()
	dir := t.TempDir()
	for _, name := range []string{
		"datatable_job.xml",
		"datatable_stancecondition.xml",
		"datatable_item_weapon.xml",
	} {
		copyFile(t, filepath.Join(src, name), filepath.Join(dir, name))
	}

	raw, err := os.ReadFile(filepath.Join(src, "datatable_stance.xml"))
	if err != nil {
		t.Fatalf("read the real stance table: %v", err)
	}
	patched, err := poisonASTD(string(raw), stanceID)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "datatable_stance.xml"), []byte(patched), 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	return dir
}

// poisonASTD sets the ASTD attribute of exactly ONE <Class ClassID="id" ...> element
// to `PC\None`, and refuses when it cannot do it exactly once - a fixture that
// silently changed nothing would make the test pass for the wrong reason.
func poisonASTD(doc string, stanceID int) (string, error) {
	head := `ClassID="` + itoa(stanceID) + `"`
	i := strings.Index(doc, head)
	if i < 0 {
		return "", errString("class " + itoa(stanceID) + " is not in the stance table")
	}
	// The element ends at the first "/>" after its ClassID.
	end := strings.Index(doc[i:], "/>")
	if end < 0 {
		return "", errString("class " + itoa(stanceID) + " has no end of element")
	}
	element := doc[i : i+end]
	if strings.Contains(element, `ASTD="`) == false {
		return "", errString("class " + itoa(stanceID) + " has no ASTD to replace")
	}
	astd := strings.Index(element, `ASTD="`)
	close := strings.Index(element[astd:], `"`)
	close2 := strings.Index(element[astd+close+1:], `"`)
	if astd < 0 || close < 0 || close2 < 0 {
		return "", errString("class " + itoa(stanceID) + "'s ASTD is not a quoted value")
	}
	fixed := element[:astd] + `ASTD="` + NoneMotion + `"` + element[astd+close+1+close2+1:]
	return doc[:i] + fixed + doc[i+end:], nil
}

// firstBareStance is the id the OLD rule would have chosen: element [0] of the job's
// first condition whose two hands are None, with no check on the idle name at all.
func firstBareStance(t *testing.T, dir string, job int) uint32 {
	t.Helper()
	table, err := LoadStances(dir)
	if err != nil {
		t.Fatalf("LoadStances(%q): %v", dir, err)
	}
	for _, cond := range table.jobConds(job) {
		if cond.L == "None" && cond.R == "None" && len(cond.IDs) > 0 {
			return uint32(cond.IDs[0])
		}
	}
	return 0
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

// itoa and errString keep these helpers free of two more imports for one use each.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

type errString string

func (e errString) Error() string { return string(e) }
