package zone

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"geserver/world"
)

// TestAMissingMapIsRequestedAndTheDoorOpens: with AskForZones on, a door whose map has no
// live zone writes a request for the manager and waits; the moment the manager's zone
// announces itself, the door opens with that address. This is the whole on-demand path
// without a real process: the goroutine stands in for cmd/zoneman.
func TestAMissingMapIsRequestedAndTheDoorOpens(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{
		Log:         log.New(io.Discard, "", 0),
		Zones:       &world.Registry{Dir: dir},
		AskForZones: true,
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		rec := world.Record{MapID: 15, IP: "127.0.0.1", Port: 7009, MapName: "coim_basic",
			LastSeen: float64(time.Now().Unix())}
		blob, _ := json.Marshal(rec)
		_ = os.WriteFile(filepath.Join(dir, "zone_7009.json"), blob, 0o644)
	}()

	rec, ok := h.ensureZone(15)
	if !ok {
		t.Fatal("ensureZone did not open the door though the zone announced")
	}
	if rec.Port != 7009 || rec.IP != "127.0.0.1" {
		t.Fatalf("answered %s:%d, want 127.0.0.1:7009", rec.IP, rec.Port)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests", "request_15.json")); err != nil {
		t.Fatalf("no request was written for the manager: %v", err)
	}
}

// TestWithoutAskForZonesAMissingMapIsRefusedAtOnce: the default for a handler with no
// manager is the old behaviour - no request, no wait.
func TestWithoutAskForZonesAMissingMapIsRefusedAtOnce(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{Log: log.New(io.Discard, "", 0), Zones: &world.Registry{Dir: dir}}
	start := time.Now()
	if _, ok := h.ensureZone(15); ok {
		t.Fatal("a map with no zone answered live")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the refusal took %s - it must not wait without a manager", elapsed)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests", "request_15.json")); err == nil {
		t.Fatal("a request was written though the handler has no manager")
	}
}
