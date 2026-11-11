// THE DROP ITEM IMAGE MUST CARRY THE ROWS **AND THE ORDER** (card t_9779e2fd).
//
// Two indexes are rebuilt from the rows (byID, byName), but byRule is stored: the reference picks
// a drop uniformly from the class list's own FILE order (`CItemTable::GetItemsByDropRule`), so a
// set would silently change every drop roll's odds. This test therefore compares every row AND
// every rule list ELEMENT BY ELEMENT after a real save/load.
package zone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTheItemImageRoundTripsTheRealTableInOrder(t *testing.T) {
	dir := XMLDir()
	paths, _ := filepath.Glob(filepath.Join(dir, "datatable_item*.xml"))
	if len(paths) == 0 {
		t.Skipf("no item XML under %s", dir)
	}

	startXML := time.Now()
	fromXML, err := LoadItemTables(dir)
	if err != nil {
		t.Fatalf("LoadItemTables: %v", err)
	}
	xmlTook := time.Since(startXML)
	if fromXML.Len() == 0 {
		t.Fatal("the XML loader answered no rows")
	}

	path := filepath.Join(t.TempDir(), "item_image.bin")
	if err := SaveItemImage(path, fromXML); err != nil {
		t.Fatalf("SaveItemImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadItemImage(path)
	if err != nil {
		t.Fatalf("LoadItemImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if fromImage.Len() != fromXML.Len() {
		t.Fatalf("the image holds %d row(s), the XML %d", fromImage.Len(), fromXML.Len())
	}
	rows := 0
	for id, want := range fromXML.byID {
		got, ok := fromImage.byID[id]
		if !ok {
			t.Fatalf("class %d is missing from the image", id)
		}
		if got != want {
			t.Fatalf("class %d came back different:\n xml   %+v\n image %+v", id, want, got)
		}
		rows++
	}
	if len(fromImage.byName) != len(fromXML.byName) {
		t.Fatalf("byName has %d entries, the XML %d", len(fromImage.byName), len(fromXML.byName))
	}
	for name, id := range fromXML.byName {
		if got, ok := fromImage.byName[name]; !ok || got != id {
			t.Fatalf("byName[%q] = %d, %v; want %d, true", name, got, ok, id)
		}
	}

	// THE ORDER IS THE DATA.
	if len(fromImage.byRule) != len(fromXML.byRule) {
		t.Fatalf("byRule has %d rule(s), the XML %d", len(fromImage.byRule), len(fromXML.byRule))
	}
	longest := 0
	for rule, want := range fromXML.byRule {
		got, ok := fromImage.byRule[rule]
		if !ok {
			t.Fatalf("rule %q is missing from the image", rule)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rule %q came back in a different ORDER (or with different ids):\n"+
				" xml   %v\n image %v", rule, want, got)
		}
		if len(want) > longest {
			longest = len(want)
		}
	}
	if longest < 2 {
		t.Fatal("no rule holds two ids - the order check proved nothing on this data")
	}
	t.Logf("%d row(s), %d rule(s) identical in order (longest rule %d ids): XML %s, image %s",
		rows, len(fromXML.byRule), longest, xmlTook, imageTook)
}

// TestAnItemImageThatIsNotOneIsRefused keeps the fallback honest.
func TestAnItemImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not an item image at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadItemImage(path); err == nil {
		t.Fatal("a junk file was accepted as an item image")
	}
}
