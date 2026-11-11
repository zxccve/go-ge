// Package world is the world layer: the state that crosses a map boundary.
//
// Today it owns ONE thing - the ZONE REGISTRY, which answers "which live zone
// serves this map, at what address, and what is the map's class name?". That
// question used to be answered by two hardcoded constants in the barrack
// (127.0.0.1:7002 and "reb_basic"), and the second one silently blocked
// cross-map travel: the client loads the world NAMED in cmd 25, so a character
// saved on any map other than 10 could never be entered.
//
// The Python server already solved this in src\world.py. It writes one JSON
// presence file per zone process under out\zones\, refreshed every 5 s. This
// package READS THOSE SAME FILES, so the two halves of the port agree without a
// new protocol and without a second process.
//
// THE FILES ARE TEMPORARY SCAFFOLDING. The owner's rule is that the final state
// is DATABASE-ONLY, with no file anywhere in the chain: the registry moves into
// PostgreSQL and out\zones\ is deleted, tracked as R42 in ROADMAP.md. Only the
// three lookups in this file are the contract - swap their source when that
// lands, and delete the file path rather than keep it as a fallback.
//
// The design rule, from history\world-layer.md, is the FALLBACK DISCIPLINE:
// every lookup is allowed to say "I do not know", and the caller then does
// exactly what it did before the registry existed. A broken registry must
// degrade to "one zone on one port", never to "no game".
package world

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"geserver/address"
)

const (
	// AliveTimeout is how long a presence file may go unrefreshed before its
	// zone is treated as dead. Zones refresh every 5 s (src\world.py,
	// REFRESH_SECONDS). The original server knew instantly, because its global
	// server saw the socket drop; a heartbeat approximates that.
	AliveTimeout = 30 * time.Second

	// DefaultDir is where the presence files live: docker-port\out\zones,
	// reached from goserver\ the same way the command-table path is (../data).
	DefaultDir = "../out/zones"

	// AnyChannel asks Address to ignore the channel number. The Python side
	// passes None for the same meaning.
	AnyChannel = -1

	// ProbeTimeout is how long a caller waits for a zone to ACCEPT A CONNECTION
	// before treating the registry's record as a dead zone. A few hundred ms is
	// enough on loopback or a LAN and short enough that a player's jump does not
	// visibly stall.
	ProbeTimeout = 300 * time.Millisecond
)

// Record is one zone's presence file:
//
//	{"map_id": 10, "channel": 0, "ip": "127.0.0.1", "port": 7002,
//	 "map_name": "reb_basic", "last_seen": 1792489511.37}
//
// LastSeen is a float because Python's time.time() writes one.
type Record struct {
	MapID    int     `json:"map_id"`
	Channel  int     `json:"channel"`
	IP       string  `json:"ip"`
	Port     int     `json:"port"`
	MapName  string  `json:"map_name"`
	LastSeen float64 `json:"last_seen"`

	// Players is how many player sessions the zone had when it last refreshed, so the
	// manager can stop a map nobody is standing in (cmd/zoneman, -idle).
	Players int `json:"players"`

	// PID is the zone PROCESS, so an operator stop still works when the manager that
	// started the zone has itself been restarted: the new manager owns no process for it and
	// used to answer "not mine" while the map stayed up (measured 2026-11-09, six zones).
	PID int `json:"pid"`

	// Managed is TRUE when a zone MANAGER started this process (it is passed GE_MANAGED=1
	// on the command line's environment). It is what lets a RESTARTED manager tell its own
	// orphan - which it must adopt and may stop - from a zone the operator started by hand,
	// which the owner's rule says is never touched. A record without the field is a
	// hand-started (or pre-2026-11-10) zone and is left alone.
	Managed bool `json:"managed,omitempty"`
}

// Registry reads the presence files. A NIL *Registry is usable on purpose:
// every method then answers "I do not know", so a server built without a
// registry behaves exactly like the one that had no registry at all.
type Registry struct {
	// Dir holds the presence files. Empty means DefaultDir.
	Dir string

	// Timeout overrides AliveTimeout. Zero means AliveTimeout.
	Timeout time.Duration

	// Now overrides the clock. Nil means time.Now. Tests set it; nothing else
	// should.
	Now func() time.Time

	// FallbackIP fills a presence file whose `ip` is empty or missing - a file
	// written by hand, or by a zone built before the address book existed.
	// main.go sets it to the address THIS machine advertises (address.Resolve),
	// because every zone of this project runs on the same machine; a caller that
	// leaves it empty gets address.DefaultIP, which is the rule this package had
	// before the address was configurable.
	FallbackIP string
}

// readFileShared reads one presence file, RETRYING a transient Windows sharing violation.
//
// WHY: the writer replaces the file with a temp file + rename (world/announce.go), which is
// atomic, but on Windows a rename over a file another handle is reading can fail with "The
// process cannot access the file because it is being used by another process". That surfaced
// as a flaky TestHeartbeatKeepsTheFileFresh in a full-suite run (2026-11-09: one failure in a
// 400-second gate, three clean runs alone) - and in production the same window would make a
// live zone look ABSENT for one poll, which is a gate answering "no zone" for a map that is up.
//
// The reader is the cheap side to make patient. It does not swallow an error: after the last
// attempt the real one is returned, so a file that is really gone still reads as ErrNotExist.
func readFileShared(path string) ([]byte, error) {
	const attempts = 5
	var blob []byte
	var err error
	for i := 0; i < attempts; i++ {
		blob, err = os.ReadFile(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return blob, err
		}
		time.Sleep(2 * time.Millisecond)
	}
	return blob, err
}

// Records returns every zone that announced itself and is still alive, sorted
// by map id then port. A missing directory is an error so a caller can say so
// once at startup; a single unreadable or half-written file is skipped, because
// one bad file must not blind the registry to the others.
func (r *Registry) Records() ([]Record, error) {
	if r == nil {
		return nil, nil
	}
	dir := r.Dir
	if dir == "" {
		dir = DefaultDir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = AliveTimeout
	}

	out := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		blob, err := readFileShared(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(blob, &rec); err != nil {
			continue
		}
		if rec.Port <= 0 || rec.MapID <= 0 {
			continue
		}
		// A stale file is what a killed zone leaves behind: it is skipped, not
		// deleted, so the port is never handed out after the zone died.
		if rec.LastSeen == 0 || now.Sub(unixFloat(rec.LastSeen)) > timeout {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MapID != out[j].MapID {
			return out[i].MapID < out[j].MapID
		}
		return out[i].Port < out[j].Port
	})
	return out, nil
}

// Address answers where mapID is served. channel may be AnyChannel to ignore
// channels; an exact channel is preferred, but a zone on another channel is
// better than no answer at all.
//
// With several zones on one map the LOWEST PORT wins, so two calls agree. False
// means "no live zone for that map" and the caller falls back.
func (r *Registry) Address(mapID, channel int) (Record, bool) {
	recs, err := r.Records()
	if err != nil {
		return Record{}, false
	}
	cands := make([]Record, 0, len(recs))
	for _, rec := range recs {
		if rec.MapID == mapID {
			cands = append(cands, rec)
		}
	}
	if channel != AnyChannel {
		same := make([]Record, 0, len(cands))
		for _, rec := range cands {
			if rec.Channel == channel {
				same = append(same, rec)
			}
		}
		if len(same) > 0 {
			cands = same
		}
	}
	if len(cands) == 0 {
		return Record{}, false
	}
	rec := cands[0]
	if rec.IP == "" {
		// A record with no address is still usable, but the address must never be a
		// literal chosen here: it is this machine's advertised address, and the
		// documented default only when the caller has none (address.Fill).
		rec.IP = address.Fill(r.FallbackIP)
	}
	return rec, true
}

// Probe dials the address a record names, and answers nil only when something is
// LISTENING there.
//
// WHY THE FILE IS NOT ENOUGH. A presence file is a note that a zone STARTED: it is
// refreshed while the zone runs and withdrawn on a clean shutdown, but a zone that is
// KILLED - or whose console window is closed - leaves the file behind, fresh for up to
// AliveTimeout. Reading it can therefore say "map 10 is at 127.0.0.1:7002" about a port
// nobody listens on, which is what a Warp List jump did on 2026-10-30: it took the
// scroll and handed the player to nothing. The file is a HINT; a caller that must not
// strand a player calls this too.
//
// A record with no port is an error, so "we do not know where it is" can never pass for
// "it is alive".
func Probe(rec Record, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	if rec.Port <= 0 {
		return fmt.Errorf("no port to dial")
	}
	// A record with no address can only mean "something on THIS machine", because
	// the only way an address goes missing is a file written here by hand: dial the
	// documented default rather than the empty string, and never a literal.
	addr := net.JoinHostPort(address.Fill(rec.IP), strconv.Itoa(rec.Port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// MapName answers the map's CLASS name ("reb_basic" for 10), which the client
// feeds straight into SwitchMode.
//
// A live zone's own announcement wins over the map table: the zone knows which
// world it is actually serving. False means "not known here" - see the handler
// for the table lookup that follows.
func (r *Registry) MapName(mapID int) (string, bool) {
	recs, err := r.Records()
	if err != nil {
		return "", false
	}
	for _, rec := range recs {
		if rec.MapID == mapID && rec.MapName != "" {
			return rec.MapName, true
		}
	}
	return "", false
}

// unixFloat turns Python's time.time() float into a time.Time.
func unixFloat(f float64) time.Time {
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// Request asks the zone manager for a map: it writes `<Dir>/request_<map>.json`, which
// the manager (cmd/zoneman) watches. A missing zone is then started ON DEMAND instead of
// the player being refused at a door - the owner's "hard to test each time asking u to
// start certain map".
//
// The file is written atomically (temp + rename) so the manager can never read half of
// it. A request for a map that already has a zone is harmless: the manager skips it.
func (r *Registry) Request(mapID int) error {
	if r == nil || r.Dir == "" {
		return fmt.Errorf("zone registry has no directory")
	}
	body, err := json.Marshal(map[string]any{
		"map_id": mapID,
		"at":     float64(r.clock().Unix()),
	})
	if err != nil {
		return err
	}
	// The requests live in their OWN directory: Records() reads every *.json in the
	// registry dir, and a request file there would decode as a half-made zone record.
	reqDir := filepath.Join(r.Dir, "requests")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(reqDir, fmt.Sprintf("request_%d.json", mapID))
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// WaitFor polls the registry until a live zone serves mapID, or the wait runs out. It is
// the other half of Request: the caller asks, then waits for the manager to have started
// the zone and for that zone to have announced itself.
func (r *Registry) WaitFor(mapID int, wait time.Duration) (Record, bool) {
	deadline := r.clock().Add(wait)
	for {
		if rec, ok := r.Address(mapID, AnyChannel); ok {
			return rec, true
		}
		if !r.clock().Before(deadline) {
			return Record{}, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// clock is Now with the package default, so tests can drive WaitFor without sleeping.
func (r *Registry) clock() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
