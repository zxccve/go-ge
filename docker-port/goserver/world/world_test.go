package world

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// write drops one presence file into dir, as src\world.py does.
func write(t *testing.T, dir, name string, rec Record) {
	t.Helper()
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), blob, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// fixedClock pins "now" so a record's age is exact, not approximate.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// TestLiveZoneIsFoundByItsMap is the whole point: a zone announced on map 20 is
// answered for map 20, and NOT for the map the constants used to hardcode.
func TestLiveZoneIsFoundByItsMap(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, Channel: 0, IP: "127.0.0.1", Port: 7002,
		MapName: "reb_basic", LastSeen: float64(now.Unix()),
	})
	write(t, dir, "zone_7003.json", Record{
		MapID: 20, Channel: 0, IP: "10.0.0.5", Port: 7003,
		MapName: "mission_sales1", LastSeen: float64(now.Unix()),
	})
	r := &Registry{Dir: dir, Now: fixedClock(now)}

	rec, ok := r.Address(20, AnyChannel)
	if !ok {
		t.Fatal("map 20 has a live zone but Address said no")
	}
	if rec.IP != "10.0.0.5" || rec.Port != 7003 {
		t.Fatalf("map 20 -> %s:%d, want 10.0.0.5:7003", rec.IP, rec.Port)
	}

	rec, ok = r.Address(10, AnyChannel)
	if !ok || rec.Port != 7002 {
		t.Fatalf("map 10 -> %+v %v, want port 7002", rec, ok)
	}

	// The map NAME travels with the announcement, which is what unblocks a map
	// other than 10 without decoding the map table.
	if name, ok := r.MapName(20); !ok || name != "mission_sales1" {
		t.Fatalf("MapName(20) = %q %v, want mission_sales1", name, ok)
	}
	if _, ok := r.Address(999, AnyChannel); ok {
		t.Fatal("Address(999) answered although no zone serves map 999")
	}
}

// TestStaleHeartbeatIsNotHandedOut covers the killed-zone case: taskkill leaves
// the file behind, so the file's AGE is what decides, not its existence.
func TestStaleHeartbeatIsNotHandedOut(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic",
		LastSeen: float64(now.Add(-AliveTimeout - time.Second).Unix()),
	})
	r := &Registry{Dir: dir, Now: fixedClock(now)}

	if rec, ok := r.Address(10, AnyChannel); ok {
		t.Fatalf("a zone silent for %s was still handed out: %+v", AliveTimeout, rec)
	}
	if recs, err := r.Records(); err != nil || len(recs) != 0 {
		t.Fatalf("Records = %v, %v; want none", recs, err)
	}

	// One second inside the timeout is still alive - the boundary is not off by
	// a unit, which matters because the refresh runs every 5 s.
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic",
		LastSeen: float64(now.Add(-AliveTimeout + time.Second).Unix()),
	})
	if _, ok := r.Address(10, AnyChannel); !ok {
		t.Fatal("a zone refreshed inside the timeout was treated as dead")
	}
}

// TestLowestPortWinsAndChannelPrefersMatch is the stability rule: two zones on
// one map must not answer differently on two calls, and a player asking for
// channel 2 should get channel 2 when it exists.
func TestLowestPortWinsAndChannelPrefersMatch(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)
	seen := float64(now.Unix())
	write(t, dir, "zone_7010.json", Record{
		MapID: 10, Channel: 2, IP: "127.0.0.1", Port: 7010, LastSeen: seen,
	})
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, Channel: 0, IP: "127.0.0.1", Port: 7002, LastSeen: seen,
	})
	r := &Registry{Dir: dir, Now: fixedClock(now)}

	if rec, _ := r.Address(10, AnyChannel); rec.Port != 7002 {
		t.Fatalf("lowest port = %d, want 7002", rec.Port)
	}
	if rec, _ := r.Address(10, 2); rec.Port != 7010 {
		t.Fatalf("channel 2 -> port %d, want 7010", rec.Port)
	}
	// A channel nobody serves must still get an answer rather than "no zone".
	if rec, ok := r.Address(10, 7); !ok || rec.Port != 7002 {
		t.Fatalf("channel 7 -> %+v %v, want a fallback to port 7002", rec, ok)
	}
}

// TestBrokenRegistryAnswersNothing is the fallback discipline: these are the
// shapes a broken registry takes, and every one of them must be a quiet "I do
// not know" so the caller keeps its old behaviour.
func TestBrokenRegistryAnswersNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *Registry
	}{
		{"nil registry", nil},
		{"missing directory", &Registry{Dir: filepath.Join(t.TempDir(), "nope")}},
		{"empty directory", &Registry{Dir: t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.reg.Address(10, AnyChannel); ok {
				t.Fatal("Address answered")
			}
			if _, ok := tc.reg.MapName(10); ok {
				t.Fatal("MapName answered")
			}
		})
	}
}

// TestUnreadableAndForeignFilesAreSkipped: one bad file must not hide a good
// one, and a non-JSON file in the folder is not the registry's business.
func TestUnreadableAndForeignFilesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)
	seen := float64(now.Unix())
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, IP: "127.0.0.1", Port: 7002, LastSeen: seen,
	})
	if err := os.WriteFile(filepath.Join(dir, "zone_7003.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Dir: dir, Now: fixedClock(now)}

	recs, err := r.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 1 || recs[0].Port != 7002 {
		t.Fatalf("Records = %+v, want only the readable zone 7002", recs)
	}
}

// TestEmptyIPFallsBackToLoopback: a record without an address is still usable;
// handing the client an empty IP string would be worse than the default.
func TestEmptyIPFallsBackToLoopback(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)
	write(t, dir, "zone_7002.json", Record{
		MapID: 10, Port: 7002, LastSeen: float64(now.Unix()),
	})
	r := &Registry{Dir: dir, Now: fixedClock(now)}
	if rec, ok := r.Address(10, AnyChannel); !ok || rec.IP != "127.0.0.1" {
		t.Fatalf("Address = %+v %v, want 127.0.0.1", rec, ok)
	}
}

// TestRequestWritesARequestTheManagerCanRead: the request goes into its own directory
// (a *.json beside the zone records would decode as a half-made zone) and names the map.
func TestRequestWritesARequestTheManagerCanRead(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}
	if err := r.Request(15); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "requests", "request_15.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the request file is missing: %v", err)
	}
	if !strings.Contains(string(body), `"map_id":15`) {
		t.Fatalf("request body = %s, want map_id 15", body)
	}
	if recs, _ := r.Records(); len(recs) != 0 {
		t.Fatalf("the request became %d zone record(s) - it must not be read as one", len(recs))
	}
}

// TestWaitForAnswersWhenTheZoneAnnounces: the other half - the caller asks, then waits for
// the manager's zone to write its presence file.
func TestWaitForAnswersWhenTheZoneAnnounces(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{Dir: dir}
	go func() {
		time.Sleep(150 * time.Millisecond)
		rec := `{"map_id":15,"channel":0,"ip":"127.0.0.1","port":7009,"map_name":"coim_basic","last_seen":` +
			strconv.FormatInt(time.Now().Unix(), 10) + `}`
		_ = os.WriteFile(filepath.Join(dir, "zone_7009.json"), []byte(rec), 0o644)
	}()
	got, ok := r.WaitFor(15, 3*time.Second)
	if !ok {
		t.Fatal("WaitFor timed out though the zone announced")
	}
	if got.Port != 7009 {
		t.Fatalf("port = %d, want 7009", got.Port)
	}
}
