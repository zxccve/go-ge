// THE STANCE IMAGE MUST CARRY NINE STRUCTURES PLUS FOUR ROW MAPS, AND EVERY LIST IN ORDER
// (card t_88e45fd8).
//
// The lists are DATA, not sets: `JobStances` walks a job's conditions in table order,
// `PickStance` takes the FIRST stance of a condition whose idle motion resolves, and
// `JobSkills` walks the stance's own SkillID1..5. A rebuild from sorted ids would change
// which stance and which skills a character gets, silently. So this test compares every map
// of the real tables with reflect.DeepEqual - the item image's `byRule` precedent - and
// refuses to pass unless at least one list is long enough for order to mean something.
package zone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTheStanceImageRoundTripsTheRealTablesInOrder(t *testing.T) {
	dir := XMLDir()
	for _, name := range stanceSourceFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			if name == "datatable_ai.xml" {
				continue // optional in the loader too
			}
			t.Skipf("no %s under %s", name, dir)
		}
	}

	startXML := time.Now()
	fromXML, err := LoadStances(dir)
	if err != nil {
		t.Fatalf("LoadStances(%q): %v", dir, err)
	}
	xmlTook := time.Since(startXML)

	path := filepath.Join(t.TempDir(), "stance_image.bin")
	if err := SaveStanceImage(path, fromXML); err != nil {
		t.Fatalf("SaveStanceImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadStanceImage(path)
	if err != nil {
		t.Fatalf("LoadStanceImage: %v", err)
	}
	imageTook := time.Since(startImage)

	// EVERY map, every key, every value. Lists go through DeepEqual, which is also what
	// catches a nil list coming back as an empty one.
	pairs := []struct {
		name string
		xml  any
		img  any
	}{
		{"stanceSkills", fromXML.stanceSkills, fromImage.stanceSkills},
		{"rows", fromXML.rows, fromImage.rows},
		{"combat", fromXML.combat, fromImage.combat},
		{"jobs", fromXML.jobs, fromImage.jobs},
		{"conds", fromXML.conds, fromImage.conds},
		{"hands", fromXML.hands, fromImage.hands},
		{"jobSTD", fromXML.jobSTD, fromImage.jobSTD},
		{"jobSkill", fromXML.jobSkill, fromImage.jobSkill},
		{"weaponTypes", fromXML.weaponTypes, fromImage.weaponTypes},
		{"twoHanded", fromXML.twoHanded, fromImage.twoHanded},
		{"aiIDs", fromXML.aiIDs, fromImage.aiIDs},
		{"rawStance", fromXML.rawStance, fromImage.rawStance},
		{"rawJob", fromXML.rawJob, fromImage.rawJob},
	}
	for _, p := range pairs {
		if !reflect.DeepEqual(p.xml, p.img) {
			t.Fatalf("%s came back different after the round trip (%s)", p.name, diffHint(p.name, p.xml, p.img))
		}
	}

	// The order proof: at least one list long enough that a set would lose information.
	longest := 0
	for _, list := range fromXML.stanceSkills {
		if len(list) > longest {
			longest = len(list)
		}
	}
	for _, list := range fromXML.conds {
		if len(list) > longest {
			longest = len(list)
		}
	}
	for _, list := range fromXML.jobs {
		if len(list) > longest {
			longest = len(list)
		}
	}
	if longest < 2 {
		t.Fatalf("no list holds two ids - the order check proved nothing on this data")
	}

	// And the ANSWERS, for every job the table knows: the same stance order and the same
	// skill list, which is what actually reaches a character.
	for job := range fromXML.jobs {
		if got, want := fromImage.JobStances(job), fromXML.JobStances(job); !reflect.DeepEqual(got, want) {
			t.Fatalf("job %d: image answers stances %v, XML %v", job, got, want)
		}
		if got, want := fromImage.JobSkills(job), fromXML.JobSkills(job); !reflect.DeepEqual(got, want) {
			t.Fatalf("job %d: image answers skills %v, XML %v", job, got, want)
		}
	}
	t.Logf("%d stance row(s), %d job(s), %d condition(s), %d weapon(s), %d AI profile(s), "+
		"longest list %d: XML %s, image %s",
		len(fromXML.stanceSkills), len(fromXML.jobs), len(fromXML.conds),
		len(fromXML.weaponTypes), len(fromXML.aiIDs), longest, xmlTook, imageTook)
}

// diffHint names the first key that differs, so a failure is readable without a dump of
// 3,431 weapon rows.
func diffHint(name string, xml, img any) string {
	switch name {
	case "stanceSkills", "jobs", "conds":
		a, b := xml.(map[int][]int), img.(map[int][]int)
		for k, v := range a {
			if !reflect.DeepEqual(v, b[k]) {
				return "first difference at key " + itoa(k)
			}
		}
	case "rows":
		a, b := xml.(map[int]StanceRow), img.(map[int]StanceRow)
		for k, v := range a {
			if b[k] != v {
				return "first difference at key " + itoa(k)
			}
		}
	case "combat":
		a, b := xml.(map[int]StanceCombat), img.(map[int]StanceCombat)
		for k, v := range a {
			if b[k] != v {
				return "first difference at key " + itoa(k)
			}
		}
	case "hands":
		a, b := xml.(map[int][2]string), img.(map[int][2]string)
		for k, v := range a {
			if b[k] != v {
				return "first difference at key " + itoa(k)
			}
		}
	}
	return "map sizes differ or the first key does not"
}

func TestAStanceImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not a stance image at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadStanceImage(path); err == nil {
		t.Fatal("a junk file was accepted as a stance image")
	}
}

// TestTheStanceImageIsStaleAgainstEverySourceFile is pass condition 2 for this half: the
// loader reads FIVE files and a change in any of them is a change in the table.
func TestTheStanceImageIsStaleAgainstEverySourceFile(t *testing.T) {
	dir := t.TempDir()
	writeStanceFixtures(t, dir)
	tbl, err := LoadStances(dir)
	if err != nil {
		t.Fatalf("LoadStances on the fixtures: %v", err)
	}
	img := filepath.Join(dir, "stance_image.bin")
	if err := SaveStanceImage(img, tbl); err != nil {
		t.Fatalf("SaveStanceImage: %v", err)
	}
	if StanceImageStale(img, dir) {
		t.Fatal("an image just built from these files was called stale")
	}

	future := time.Now().Add(2 * time.Hour)
	past := time.Now().Add(-2 * time.Hour)
	for _, name := range stanceSourceFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		if !StanceImageStale(img, dir) {
			t.Fatalf("%s newer than the image was called fresh", name)
		}
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		if StanceImageStale(img, dir) {
			t.Fatalf("%s older than the image was called stale", name)
		}
	}

	// A source that is GONE is skipped, not fatal: a bundle without datatable_ai.xml still
	// uses the image (the loader itself treats that file as optional).
	if err := os.Remove(filepath.Join(dir, "datatable_ai.xml")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if StanceImageStale(img, dir) {
		t.Fatal("a missing optional source was called stale")
	}
	// A missing image is always stale.
	if !StanceImageStale(filepath.Join(dir, "absent.bin"), dir) {
		t.Fatal("a missing image was called fresh")
	}
}

// TestACorruptStanceImageFallsBackToTheXML is the other half of pass condition 2: a junk
// image must not stop a start, and the next one must be readable again.
func TestACorruptStanceImageFallsBackToTheXML(t *testing.T) {
	dir := t.TempDir()
	writeStanceFixtures(t, dir)
	img := filepath.Join(dir, "stance_image.bin")
	if err := os.WriteFile(img, []byte("this is not an image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tbl, fromImage, err := LoadStancesCached(dir, img)
	if err != nil {
		t.Fatalf("a corrupt image was not survivable: %v", err)
	}
	if fromImage {
		t.Fatal("a corrupt image was reported as the source")
	}
	if len(tbl.stanceSkills) == 0 || len(tbl.jobs) == 0 || len(tbl.conds) == 0 {
		t.Fatal("the fallback answered an empty table")
	}
	good, err := LoadStanceImage(img)
	if err != nil {
		t.Fatalf("the broken image was not rewritten: %v", err)
	}
	if !reflect.DeepEqual(good.stanceSkills, tbl.stanceSkills) {
		t.Fatal("the rewritten image answers a different table")
	}

	// And a MISSING image is built on the first start, then used on the next.
	if err := os.Remove(img); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, fromImage, err = LoadStancesCached(dir, img); err != nil || fromImage {
		t.Fatalf("a missing image: fromImage=%v err=%v, want false/nil", fromImage, err)
	}
	if _, fromImage, err = LoadStancesCached(dir, img); err != nil || !fromImage {
		t.Fatalf("the built image: fromImage=%v err=%v, want true/nil", fromImage, err)
	}
}

// writeStanceFixtures writes the five smallest files LoadStances can load, so the staleness
// and fallback tests do not depend on 14 MB of the real client data (and cannot touch it).
func writeStanceFixtures(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"datatable_stance.xml": `<?xml version="1.0" encoding="utf-8"?>
<idspace id="datatable_stance">
	<Category>
		<Name>stance</Name>
		<Class ClassID="1" ClassName="BackGuard" ClassType="MELEE" RSPD="1.0" WSPD="1.0"
			StnMSPD="1.2" ASTD="PC_ASTD_EPE" AI="ATTACKER" SkillID1="100" SkillID2="0"
			SkillID3="101" AtkType="None" DefType="None" RHits="1" LHits="0" StnATK="1.5"
			StnLv="1" ASPD="1000" MinR="0" MaxR="150" SplType="None"/>
		<Class ClassID="2" ClassName="FirstAid" ClassType="MAGIC" RSPD="0.9" WSPD="0.9"
			StnMSPD="1" ASTD="PC_ASTD_NONE" AI="HEALER" SkillID1="200" AtkType="None"
			DefType="None" RHits="1" LHits="0" StnATK="1" StnLv="1" ASPD="1500"
			MinR="1" MaxR="100" SplRange="220" SplType="Circle"/>
	</Category>
</idspace>
`,
		"datatable_job.xml": `<?xml version="1.0" encoding="utf-8"?>
<idspace id="datatable_job">
	<Category>
		<Name>job</Name>
		<Class ClassID="1" ClassName="Fighter" EqpWeaponSet="1,3" STD="PC_STD_GRANDIES"
			JobSkill="10415"/>
		<Class ClassID="2" ClassName="Wizard" EqpWeaponSet="2" STD="PC_STD_WIZARD"
			JobSkill="1005"/>
	</Category>
</idspace>
`,
		"datatable_stancecondition.xml": `<?xml version="1.0" encoding="utf-8"?>
<idspace id="datatable_stancecondition">
	<Category>
		<Name>stancecondition</Name>
		<Class ClassID="1" Stance1="1" Stance2="2" LHand="Shield" RHand="Rapier"/>
		<Class ClassID="2" Stance1="2" LHand="None" RHand="None"/>
		<Class ClassID="3" Stance1="1" Stance2="0" LHand="None" RHand="THSword"/>
	</Category>
</idspace>
`,
		"datatable_item_weapon.xml": `<?xml version="1.0" encoding="utf-8"?>
<idspace id="datatable_item_weapon">
	<Category>
		<Name>weapon</Name>
		<Class ClassID="500" ClassName="Rapier01" ClassType="Rapier" DblAttach="0" DBLHand="NO"/>
		<Class ClassID="501" ClassName="Shield01" ClassType="Shield" DblAttach="0" DBLHand="NO"/>
		<Class ClassID="502" ClassName="THSword01" ClassType="THSword" DblAttach="1" DBLHand="YES"/>
	</Category>
</idspace>
`,
		"datatable_ai.xml": `<?xml version="1.0" encoding="utf-8"?>
<idspace id="datatable_ai">
	<Category>
		<Name>ai</Name>
		<Class ClassID="1" ClassName="ATTACKER"/>
		<Class ClassID="2" ClassName="HEALER"/>
	</Category>
</idspace>
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
}
