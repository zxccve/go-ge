package main

// WHY THIS TEST EXISTS. The address a player's client dials used to be two
// compiled-in constants (`presenceIP` in main.go and `DefaultZoneAddr` in
// barrack\handler.go). Both said loopback, so a player on a SECOND PC could not
// connect at all and NOTHING logged an error. The address now comes from the
// address book (`machine` in PostgreSQL, migrations\00013_machine.sql), and this
// file holds the three promises the owner asked for, against the record the ZONE
// really publishes on disk:
//
//	(a) the row's ip is what the presence file carries;
//	(b) -advertise WINS over the row;
//	(c) a nil, erroring or empty source falls back to the documented default and
//	    logs exactly ONE line saying which value was used and why.
//
// Run it alone:  go test . -run TestAdvertisedAddress -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"geserver/address"
	"geserver/world"
)

const (
	// storeIP and flagIP are TEST-NET addresses (RFC 5737): never a real machine,
	// so a passing test cannot be explained by the network this box is on.
	storeIP = "192.0.2.7"
	flagIP  = "198.51.100.9"
)

// logSink collects the resolution line so the test can count the lines, not just
// read one.
type logSink struct{ lines []string }

func (l *logSink) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// only fails unless exactly one line was logged, and returns it.
func (l *logSink) only(t *testing.T) string {
	t.Helper()
	if len(l.lines) != 1 {
		t.Fatalf("logged %d line(s), want exactly 1:\n%s",
			len(l.lines), strings.Join(l.lines, "\n"))
	}
	return l.lines[0]
}

// fakeStore answers the address book without a database.
func fakeStore(ip string) address.Store {
	return address.StoreFunc(func(context.Context, string) (string, error) {
		return ip, nil
	})
}

// brokenStore is a database that answers with an error - the unreachable case.
func brokenStore() address.Store {
	return address.StoreFunc(func(context.Context, string) (string, error) {
		return "", errors.New(`ERROR: relation "machine" does not exist (SQLSTATE 42P01)`)
	})
}

func TestAdvertisedAddress(t *testing.T) {
	ctx := context.Background()

	// (a) THE POINT OF THE WHOLE CHANGE: the row's ip is what the client is told.
	t.Run("the machine row's ip reaches the presence file", func(t *testing.T) {
		var sink logSink
		adv := address.Resolve(ctx, address.Config{
			Machine: "local", Store: fakeStore(storeIP), Log: sink.logf,
		})
		if adv.IP != storeIP || adv.Source != address.FromStore {
			t.Fatalf("resolved %q from %s, want %q from the machine row",
				adv.IP, adv.Source, storeIP)
		}
		if line := sink.only(t); !strings.Contains(line, storeIP) ||
			!strings.Contains(line, "machine row") {
			t.Fatalf("startup line does not name the address and its source: %q", line)
		}

		// The zone writes exactly this record; the barrack reads it back.
		dir := t.TempDir()
		ann := &world.Announcer{
			Dir:    dir,
			Record: presenceRecord(15, "coim_basic", adv.IP, 7003),
		}
		if err := ann.Start(); err != nil {
			t.Fatalf("announce: %v", err)
		}
		defer ann.Close() // also proves Close is safe after a successful Start

		blob, err := os.ReadFile(world.ZonePath(dir, 7003))
		if err != nil {
			t.Fatalf("read presence file: %v", err)
		}
		var rec world.Record
		if err := json.Unmarshal(blob, &rec); err != nil {
			t.Fatalf("parse presence file: %v", err)
		}
		if rec.IP != storeIP {
			t.Fatalf("presence ip = %q, want %q from the machine row (file: %s)",
				rec.IP, storeIP, blob)
		}
		// The PORT is not configurable and must stay the one the listener opened.
		if rec.Port != 7003 {
			t.Fatalf("presence port = %d, want the bound 7003", rec.Port)
		}

		// And the barrack's own reader answers with that same address.
		reg := &world.Registry{Dir: dir, FallbackIP: adv.IP}
		got, ok := reg.Address(15, world.AnyChannel)
		if !ok {
			t.Fatal("registry did not find the announced map 15")
		}
		if got.IP != storeIP || got.Port != 7003 {
			t.Fatalf("registry answered %s:%d, want %s:7003", got.IP, got.Port, storeIP)
		}
	})

	// (b) A FLAG VALUE WINS OVER THE DATABASE, or a one-off run could not point a
	// test client anywhere without editing the table.
	t.Run("-advertise overrides the machine row", func(t *testing.T) {
		var sink logSink
		adv := address.Resolve(ctx, address.Config{
			Flag: flagIP, Machine: "local", Store: fakeStore(storeIP), Log: sink.logf,
		})
		if adv.IP != flagIP || adv.Source != address.FromFlag {
			t.Fatalf("resolved %q from %s, want the flag's %q", adv.IP, adv.Source, flagIP)
		}
		line := sink.only(t)
		if !strings.Contains(line, flagIP) || !strings.Contains(line, "flag") {
			t.Fatalf("startup line does not say the flag decided: %q", line)
		}
	})

	// (c) EVERY FAILURE DEGRADES TO THE DOCUMENTED DEFAULT, LOUDLY BUT ONCE, and
	// never stops the server. A player already in a town must not be dropped
	// because the address book could not be read.
	fallbacks := []struct {
		name string
		cfg  address.Config
		said string // a word the ONE line must carry, so the reason is visible
	}{
		{
			name: "no address book wired in",
			cfg:  address.Config{Machine: "local"},
			said: "no address book",
		},
		{
			name: "the database does not answer",
			cfg:  address.Config{Machine: "local", Store: brokenStore()},
			said: "could not be read",
		},
		{
			name: "the row's ip is empty",
			cfg:  address.Config{Machine: "local", Store: fakeStore("   ")},
			said: "empty ip",
		},
	}
	for _, f := range fallbacks {
		t.Run(f.name, func(t *testing.T) {
			var sink logSink
			f.cfg.Log = sink.logf
			adv := address.Resolve(ctx, f.cfg)

			if adv.IP != address.DefaultIP {
				t.Fatalf("fell back to %q, want %q", adv.IP, address.DefaultIP)
			}
			if adv.Source != address.FromDefault {
				t.Fatalf("source = %q, want the built-in default", adv.Source)
			}
			line := sink.only(t)
			if !strings.Contains(line, address.DefaultIP) {
				t.Fatalf("the line does not say which value was used: %q", line)
			}
			if !strings.Contains(line, f.said) {
				t.Fatalf("the line does not say why: want %q in %q", f.said, line)
			}
		})
	}
}

// TestPresenceRecordUsesTheBoundPort pins the one thing the address book must
// NEVER decide: the port. A configured port that disagrees with the listener is a
// closed door, so the record takes whatever the listener opened.
func TestPresenceRecordUsesTheBoundPort(t *testing.T) {
	rec := presenceRecord(10, "reb_basic", storeIP, 7007)
	if rec.IP != storeIP || rec.Port != 7007 || rec.MapID != 10 || rec.MapName != "reb_basic" {
		t.Fatalf("record = %+v, want map 10 reb_basic at %s:7007", rec, storeIP)
	}
	if rec.Channel != presenceChannel {
		t.Fatalf("channel = %d, want %d", rec.Channel, presenceChannel)
	}
	if rec.LastSeen != 0 {
		t.Fatalf("LastSeen = %v - the announcer stamps it on every write", rec.LastSeen)
	}
}

// TestThePresenceRecordSaysWhetherAManagerStartedIt pins the marker the zone manager adopts its
// own orphans by (2026-11-10): GE_MANAGED in the zone's environment is the ONLY input, and a zone
// started by hand has no marker at all - the manager must never touch it.
func TestThePresenceRecordSaysWhetherAManagerStartedIt(t *testing.T) {
	t.Setenv("GE_MANAGED", "")
	if rec := presenceRecord(10, "reb_basic", storeIP, 7007); rec.Managed {
		t.Fatal("a zone started by hand announced managed=true; the manager would adopt and stop it")
	}
	t.Setenv("GE_MANAGED", "1")
	if rec := presenceRecord(10, "reb_basic", storeIP, 7007); !rec.Managed {
		t.Fatal("a manager-started zone did not announce managed=true; a restarted manager cannot adopt it")
	}
}
