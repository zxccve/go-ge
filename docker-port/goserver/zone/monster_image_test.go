// THE MONSTER IMAGE MUST CARRY EVERY FIELD (card t_96ee949e).
//
// The image is a fixed-order byte layout, so a field added to MonsterRow without a line in
// encodeMonsterRow/decodeMonsterRow would be silently dropped - a monster would start with
// a zero where the table said a number, and nothing would say so. This test saves the REAL
// table to an image, loads it back, and compares every row, field by field.
package zone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheMonsterImageRoundTripsTheRealTable(t *testing.T) {
	dir := XMLDir()
	if _, err := os.Stat(filepath.Join(dir, "datatable_monster.xml")); err != nil {
		t.Skipf("no monster XML at %s (%v)", dir, err)
	}

	startXML := time.Now()
	fromXML, err := LoadMonsters(dir)
	if err != nil {
		t.Fatalf("LoadMonsters: %v", err)
	}
	xmlTook := time.Since(startXML)
	if fromXML.Len() == 0 {
		t.Fatal("the XML loader answered no rows")
	}

	path := filepath.Join(t.TempDir(), "monster_image.bin")
	if err := SaveMonstersImage(path, fromXML); err != nil {
		t.Fatalf("SaveMonstersImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadMonstersImage(path)
	if err != nil {
		t.Fatalf("LoadMonstersImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if fromImage.Len() != fromXML.Len() {
		t.Fatalf("the image holds %d row(s), the XML %d", fromImage.Len(), fromXML.Len())
	}
	checked := 0
	for id, want := range fromXML.byID {
		got, ok := fromImage.byID[id]
		if !ok {
			t.Fatalf("class %d is missing from the image", id)
		}
		if got != want {
			t.Fatalf("class %d came back different:\n xml   %+v\n image %+v", id, want, got)
		}
		checked++
	}
	t.Logf("%d row(s) identical: XML parse %s, image read %s", checked, xmlTook, imageTook)

	// THE IMAGE IS ONLY GOOD WHILE THE SOURCES ARE OLDER THAN IT. Tested in a THROWAWAY
	// directory, never against the real xml folder: touching a real source file would
	// invalidate the owner's image and make his next start pay the rebuild.
	srcDir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for _, name := range monsterSourceFiles(srcDir) {
		if err := os.WriteFile(name, []byte("<x/>"), 0o644); err != nil {
			t.Fatalf("make a source stub: %v", err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatalf("age a source stub: %v", err)
		}
	}
	if MonsterImageStale(path, srcDir) {
		t.Fatal("sources OLDER than the image were called stale")
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(monsterSourceFiles(srcDir)[0], future, future); err != nil {
		t.Fatalf("touch a source stub: %v", err)
	}
	if !MonsterImageStale(path, srcDir) {
		t.Fatal("MonsterImageStale answered false with a source newer than the image")
	}
}

// TestAMonsterImageThatIsNotOneIsRefused keeps the fallback honest: a corrupt or foreign
// file must be an ERROR the caller falls back from, never a half-read table.
func TestAMonsterImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not a monster image, not even close"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadMonstersImage(path); err == nil {
		t.Fatal("a junk file was accepted as a monster image")
	}
}
