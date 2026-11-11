// THE SKILL IMAGE MUST CARRY EVERY FIELD (card t_53f1a6d5).
package zone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheSkillImageRoundTripsTheRealTable(t *testing.T) {
	dir := XMLDir()
	if _, err := os.Stat(filepath.Join(dir, skillTableFile)); err != nil {
		t.Skipf("no %s under %s", skillTableFile, dir)
	}
	startXML := time.Now()
	fromXML, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	xmlTook := time.Since(startXML)
	if fromXML.Len() == 0 {
		t.Fatal("the XML loader answered no rows")
	}

	path := filepath.Join(t.TempDir(), "skill_image.bin")
	if err := SaveSkillImage(path, fromXML); err != nil {
		t.Fatalf("SaveSkillImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadSkillImage(path)
	if err != nil {
		t.Fatalf("LoadSkillImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if fromImage.Len() != fromXML.Len() {
		t.Fatalf("the image holds %d row(s), the XML %d", fromImage.Len(), fromXML.Len())
	}
	checked := 0
	for id, want := range fromXML.byID {
		got, ok := fromImage.byID[id]
		if !ok {
			t.Fatalf("skill %d is missing from the image", id)
		}
		if got != want {
			t.Fatalf("skill %d came back different:\n xml   %+v\n image %+v", id, want, got)
		}
		checked++
	}
	t.Logf("%d row(s) identical: XML parse %s, image read %s", checked, xmlTook, imageTook)
}

func TestASkillImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not a skill image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSkillImage(path); err == nil {
		t.Fatal("a junk file was accepted as a skill image")
	}
}
