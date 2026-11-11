package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// This file is the WRITE half of the registry: the zone's own presence file.
//
// It is the mirror of `src\world.py` (announce + Heartbeat + withdraw) and writes
// the EXACT format `Registry` reads - same file name, same field names, `last_seen`
// as a float unix time, written to a temp file and renamed. Both servers therefore
// read each other's files without a new protocol: a Go zone is found by the Python
// barrack and by the Go barrack alike.
//
// The file stays TEMPORARY SCAFFOLDING, the same as the reader: the final state is
// database-only (ROADMAP R42), and this file is deleted with the reader when that
// lands.

// RefreshSeconds is how often a running zone rewrites its presence file. It is the
// same number as src\world.py's REFRESH_SECONDS and it is tied to AliveTimeout: a
// zone may miss five beats before the barrack gives up on it, so one slow write is
// not a dead zone.
const RefreshSeconds = 5 * time.Second

// ZonePath is the presence file of the zone listening on port: zone_<port>.json.
//
// Keyed by PORT, not by map - src\world.py's rule. A zone process serves one map at
// a time and rewrites the same file if it moves, so a port has at most one file.
func ZonePath(dir string, port int) string {
	if dir == "" {
		dir = DefaultDir
	}
	return filepath.Join(dir, fmt.Sprintf("zone_%d.json", port))
}

// Announcer keeps ONE zone's presence file fresh while the zone runs and removes it
// when the zone stops cleanly.
//
// A failed announcement must never stop the zone: a zone that cannot write its file
// still serves the players already in it, and the barrack simply falls back to its
// constants. So every failure here is a log line, never a fatal error.
//
// A nil *Announcer is usable: Start and Refresh report the failure, Close and
// Withdraw do nothing, so a caller can `defer ann.Close()` unconditionally.
type Announcer struct {
	// Dir holds the presence files. Empty means DefaultDir.
	Dir string

	// Record is what gets written. LastSeen is set on every write, so the caller
	// leaves it zero.
	Record Record

	// Every overrides RefreshSeconds. Zero means RefreshSeconds.
	Every time.Duration

	// Log reports a failed (or recovered) announcement. Nil is allowed.
	Log func(format string, args ...any)

	// Players, when set, fills Record.Players on every refresh. A zone passes its own
	// session count so the manager can tell a map nobody is on from one in use.
	Players func() int

	// Now overrides the clock. Nil means time.Now. Tests set it; nothing else
	// should.
	Now func() time.Time

	mu      sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	failing bool // the last write failed, so the next success is worth a line
}

// Path is the file this announcer writes.
func (a *Announcer) Path() string {
	if a == nil {
		return ""
	}
	return ZonePath(a.Dir, a.Record.Port)
}

// Start writes the file once, then keeps it fresh in a goroutine that stops with the
// process or with Close.
//
// The first write happens HERE, before Start returns, so a register-then-serve zone
// is findable the moment it accepts a connection. An error means the file cannot be
// written at all (a directory that cannot be created, a port of 0); the caller logs
// it and carries on.
func (a *Announcer) Start() error {
	if a == nil {
		return fmt.Errorf("no announcer")
	}
	if a.Record.Port <= 0 {
		// The reader skips a record with no port, so writing one would announce a
		// zone nobody can find - the exact bug this file exists to fix.
		return fmt.Errorf("cannot announce map %d: port %d is not a listener",
			a.Record.MapID, a.Record.Port)
	}
	if err := a.Refresh(); err != nil {
		return err
	}

	every := a.Every
	if every <= 0 {
		every = RefreshSeconds
	}
	a.mu.Lock()
	a.stop = make(chan struct{})
	a.done = make(chan struct{})
	stop, done := a.stop, a.done
	a.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// Every beat is reported through note: one line when writing
				// breaks, one when it comes back.
				a.note(a.Refresh())
			}
		}
	}()
	return nil
}

// Refresh writes the file now, atomically: a reader must never see half a file.
//
// The temp name ends in ".tmp", not ".json", so a reader that looks while a write is
// in flight skips it - the same trick src\world.py uses.
func (a *Announcer) Refresh() error {
	if a == nil {
		return fmt.Errorf("no announcer")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	rec := a.Record
	// THE PROCESS ID, once per refresh: it costs nothing and it is the only way an operator
	// stop can reach a zone this manager did not start (see Record.PID).
	rec.PID = os.Getpid()
	if rec.Port <= 0 || rec.MapID <= 0 {
		return fmt.Errorf("cannot announce %q: map %d port %d",
			rec.MapName, rec.MapID, rec.Port)
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	rec.LastSeen = float64(now.UnixNano()) / 1e9
	if a.Players != nil {
		rec.Players = a.Players()
	}

	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal presence for port %d: %w", rec.Port, err)
	}
	path := ZonePath(a.Dir, rec.Port)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// Close stops the refresh and WITHDRAWS the file: a zone that has stopped must not
// look alive for up to AliveTimeout, or the barrack sends the next player to a closed
// port. Safe twice, safe on nil, safe when Start never ran.
func (a *Announcer) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	stop, done := a.stop, a.done
	a.stop, a.done = nil, nil
	a.mu.Unlock()

	if stop != nil {
		close(stop)
		<-done
	}
	if err := a.Withdraw(); err != nil {
		a.note(err)
	}
}

// Withdraw removes the presence file. A file that is not there is not an error:
// withdrawing twice, or before the first write, is a normal shutdown.
func (a *Announcer) Withdraw() error {
	if a == nil {
		return nil
	}
	err := os.Remove(a.Path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", a.Path(), err)
	}
	return nil
}

// note reports a failing write ONCE per outage, and the recovery once. A 5 s beat
// that logged every failure would bury the reason it started.
func (a *Announcer) note(err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	was := a.failing
	a.failing = err != nil
	a.mu.Unlock()

	logf := a.Log
	if logf == nil {
		return
	}
	switch {
	case err != nil && !was:
		logf("world: presence file %s is NOT being written (%v) - the barrack will "+
			"treat this zone as dead after %s, although it still serves players",
			a.Path(), err, AliveTimeout)
	case err == nil && was:
		logf("world: presence file %s is being written again", a.Path())
	}
}
