// THE MAP-DROP IMAGE MUST CARRY EVERY ROW (2026-11-10).
package zone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheMapDropsImageRoundTripsTheRealTable(t *testing.T) {
	dir := XMLDir()
	if _, err := os.Stat(filepath.Join(dir, "datatable_map.xml")); err != nil {
		t.Skipf("no datatable_map.xml under %s", dir)
	}
	startXML := time.Now()
	fromXML, err := LoadMapDrops(dir)
	if err != nil {
		t.Fatalf("LoadMapDrops: %v", err)
	}
	xmlTook := time.Since(startXML)
	if len(fromXML.rows) == 0 {
		t.Fatal("the XML loader answered no rows")
	}

	path := filepath.Join(t.TempDir(), "mapdrops_image.bin")
	if err := SaveMapDropsImage(path, fromXML); err != nil {
		t.Fatalf("SaveMapDropsImage: %v", err)
	}
	startImage := time.Now()
	fromImage, err := LoadMapDropsImage(path)
	if err != nil {
		t.Fatalf("LoadMapDropsImage: %v", err)
	}
	imageTook := time.Since(startImage)

	if len(fromImage.rows) != len(fromXML.rows) {
		t.Fatalf("the image holds %d row(s), the XML %d", len(fromImage.rows), len(fromXML.rows))
	}
	for id, want := range fromXML.rows {
		got, ok := fromImage.rows[id]
		if !ok {
			t.Fatalf("map %d is missing from the image", id)
		}
		if got != want {
			t.Fatalf("map %d came back different:\n xml   %+v\n image %+v", id, want, got)
		}
	}
	t.Logf("%d row(s) identical: XML parse %s, image read %s",
		len(fromXML.rows), xmlTook, imageTook)
}

func TestAMapDropsImageThatIsNotOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not a map-drop image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadMapDropsImage(path); err == nil {
		t.Fatal("a junk file was accepted as a map-drop image")
	}
}
