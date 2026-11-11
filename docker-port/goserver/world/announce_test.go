package world

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readRecord loads one presence file as a reader would.
func readRecord(t *testing.T, path string) Record {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rec Record
	if err := json.Unmarshal(blob, &rec); err != nil {
		t.Fatalf("%s is not a record: %v", path, err)
	}
	return rec
}

// TestAnnounceWritesWhatTheRegistryReads is the contract between the two halves: the
// file name and the field names come out exactly as Registry expects them, so a zone
// that writes its own presence is findable without a second protocol.
func TestAnnounceWritesWhatTheRegistryReads(t *testing.T) {
	dir := t.TempDir()
	ann := &Announcer{
		Dir: dir,
		Record: Record{
			MapID: 10, Channel: 0, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic",
		},
	}
	if err := ann.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ann.Close()

	// The name is src\world.py's _zone_path: zone_<port>.json, keyed by port.
	want := filepath.Join(dir, "zone_7002.json")
	if got := ann.Path(); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the announced file is not there: %v", err)
	}

	rec := readRecord(t, want)
	if blob, err := os.ReadFile(want); err == nil {
		t.Logf("%s: %s", filepath.Base(want), blob)
	}
	if rec.MapID != 10 || rec.Port != 7002 || rec.MapName != "reb_basic" {
		t.Fatalf("record = %+v, want map 10 / port 7002 / reb_basic", rec)
	}
	// last_seen must be unix SECONDS as a float, the same scale Python writes.
	if age := time.Since(unixFloat(rec.LastSeen)); age < 0 || age > time.Minute {
		t.Fatalf("last_seen = %v is not a recent unix time (age %s)", rec.LastSeen, age)
	}

	// And the reader agrees, which is the only thing that actually matters.
	r := &Registry{Dir: dir}
	recs, err := r.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 1 || recs[0].MapID != 10 || recs[0].Port != 7002 ||
		recs[0].MapName != "reb_basic" {
		t.Fatalf("Records = %+v, want the announced zone 10", recs)
	}
	if rec, ok := r.Address(10, AnyChannel); !ok || rec.Port != 7002 {
		t.Fatalf("Address(10) = %+v %v, want port 7002", rec, ok)
	}
	if name, ok := r.MapName(15); ok {
		t.Fatalf("MapName(15) = %q although this zone serves 10", name)
	}
}

// TestWithdrawRemovesTheFile: a zone that stopped must stop being handed out at once,
// not after AliveTimeout. This is the half of the deal the killed-zone case cannot do.
func TestWithdrawRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	ann := &Announcer{Dir: dir, Record: Record{
		MapID: 15, IP: "127.0.0.1", Port: 7005, MapName: "coim_basic",
	}}
	if err := ann.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := &Registry{Dir: dir}
	if recs, _ := r.Records(); len(recs) != 1 {
		t.Fatalf("before the shutdown Records = %d, want 1", len(recs))
	}

	ann.Close()
	if _, err := os.Stat(ann.Path()); !os.IsNotExist(err) {
		t.Fatalf("the presence file survived Close: %v", err)
	}
	recs, err := r.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("after the shutdown Records = %+v, want none", recs)
	}
	// Close twice is a normal shutdown path, not an error.
	ann.Close()
}

// TestStaleAnnouncementIsIgnored: a killed zone leaves its file behind, so the AGE is
// what decides. The clock is the registry's own injection point, so this is exact.
func TestStaleAnnouncementIsIgnored(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_792_489_511, 0)

	ann := &Announcer{
		Dir:    dir,
		Record: Record{MapID: 10, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic"},
		Now:    fixedClock(now.Add(-AliveTimeout - time.Second)),
	}
	defer ann.Close()
	if err := ann.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	r := &Registry{Dir: dir, Now: fixedClock(now)}
	if rec, ok := r.Address(10, AnyChannel); ok {
		t.Fatalf("a zone silent for %s was still handed out: %+v", AliveTimeout, rec)
	}
	if recs, err := r.Records(); err != nil || len(recs) != 0 {
		t.Fatalf("Records = %v, %v; want none", recs, err)
	}
}

// TestHeartbeatKeepsTheFileFresh is the "refreshes every 5 s" promise: the beat is a
// goroutine, so the test shortens it and waits for a second write.
func TestHeartbeatKeepsTheFileFresh(t *testing.T) {
	dir := t.TempDir()
	ann := &Announcer{
		Dir:    dir,
		Record: Record{MapID: 10, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic"},
		Every:  20 * time.Millisecond,
	}
	if err := ann.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ann.Close()

	first := readRecord(t, ann.Path()).LastSeen
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if readRecord(t, ann.Path()).LastSeen > first {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the heartbeat never refreshed the presence file")
}

// TestAnnounceRefusesAPortlessRecord: the reader skips a record with no port, so
// writing one would announce a zone nobody can find - the bug this file fixes.
func TestAnnounceRefusesAPortlessRecord(t *testing.T) {
	ann := &Announcer{Dir: t.TempDir(), Record: Record{MapID: 10}}
	if err := ann.Start(); err == nil {
		t.Fatal("Start accepted a record with no port")
	}
	if _, err := os.Stat(ann.Path()); !os.IsNotExist(err) {
		t.Fatal("a portless record still left a file behind")
	}
}

// TestAnnounceFailureIsNotFatal: a directory that cannot be created is reported, not
// fatal, and Close on that announcer is harmless - a failed announcement must never
// stop a zone.
func TestAnnounceFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	// A FILE where the directory should be: MkdirAll cannot make anything under it.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ann := &Announcer{
		Dir:    filepath.Join(blocker, "zones"),
		Record: Record{MapID: 10, IP: "127.0.0.1", Port: 7002, MapName: "reb_basic"},
	}
	if err := ann.Start(); err == nil {
		t.Fatal("Start succeeded on a directory it cannot create")
	}
	ann.Close() // must not panic, must not need a successful write first
}
