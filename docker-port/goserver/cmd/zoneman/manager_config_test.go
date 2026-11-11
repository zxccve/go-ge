package main

// The manager's settings come from server.json, and the flags are only the
// exception. These tests are the owner's rule written down: "no flags, no code -
// a clean config file", with the file as the base and a flag as the override,
// exactly the way the zone and barrack already resolve theirs.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeServerJSON is this file's only fixture: a server.json in a temp folder.
func writeServerJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheManagerReadsServerJSON: every value comes from the file, and a field the
// file does not name keeps the manager's HISTORIC default - so this change moved
// where a value lives and re-tuned nothing.
func TestTheManagerReadsServerJSON(t *testing.T) {
	path := writeServerJSON(t, `{
		"manager": {
			"bin": "/app/out/goserver",
			"zones_dir": "/app/out/zones",
			"base_port": 7020,
			"max": 60,
			"log_dir": "/app/out/logs",
			"idle": "5m",
			"linger": "15m",
			"prepare": false,
			"warm": "10,15"
		}
	}`)
	cfg, source, err := newManagerConfig([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if source != path {
		t.Fatalf("source = %q, want %q", source, path)
	}
	if cfg.bin != "/app/out/goserver" || cfg.zonesDir != "/app/out/zones" || cfg.logdir != "/app/out/logs" {
		t.Fatalf("paths came from somewhere else: bin %q zones %q logdir %q", cfg.bin, cfg.zonesDir, cfg.logdir)
	}
	if cfg.basePort != 7020 || cfg.maxZones != 60 {
		t.Fatalf("base-port/max = %d/%d, want 7020/60", cfg.basePort, cfg.maxZones)
	}
	if cfg.idle != 5*time.Minute || cfg.linger != 15*time.Minute {
		t.Fatalf("idle/linger = %v/%v, want 5m/15m from the file", cfg.idle, cfg.linger)
	}
	if cfg.prepare {
		t.Fatal("prepare = true, want false from the file")
	}
	if cfg.warm != "10,15" {
		t.Fatalf("warm = %q, want 10,15", cfg.warm)
	}
	// Absent from the file: the historic defaults must be untouched.
	if cfg.maxTries != 3 || cfg.retain != 24*time.Hour || cfg.startWait != 180*time.Second || cfg.memlimit != "200MiB" {
		t.Fatalf("defaults moved: max-tries %d, retain %v, start-wait %v, memlimit %q",
			cfg.maxTries, cfg.retain, cfg.startWait, cfg.memlimit)
	}
	// monai is the manager's own default ("script"), never the zone's Mode.Monai ("go").
	if cfg.monai != "script" {
		t.Fatalf("monai = %q, want script", cfg.monai)
	}
}

// TestAFlagOverridesTheFile is the second half of the rule: the file is the base,
// the command line is the exception.
func TestAFlagOverridesTheFile(t *testing.T) {
	path := writeServerJSON(t, `{"manager": {"max": 60, "idle": "5m"}}`)
	cfg, _, err := newManagerConfig([]string{"-config", path, "-max", "12", "-linger", "30m"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.maxZones != 12 {
		t.Fatalf("max = %d, want the flag's 12", cfg.maxZones)
	}
	if cfg.linger != 30*time.Minute {
		t.Fatalf("linger = %v, want the flag's 30m", cfg.linger)
	}
	if cfg.idle != 5*time.Minute {
		t.Fatalf("idle = %v, want the FILE's 5m - the flag must not reset what it did not name", cfg.idle)
	}
}

// TestAnUnknownManagerKeyIsRefused keeps the typo guard that server.json already
// has: a misspelled key must not look like a manager that ignored it.
func TestAnUnknownManagerKeyIsRefused(t *testing.T) {
	path := writeServerJSON(t, `{"manager": {"maxx": 60}}`)
	_, _, err := newManagerConfig([]string{"-config", path}, io.Discard)
	if err == nil {
		t.Fatal("a misspelled manager key was accepted")
	}
	if !strings.Contains(err.Error(), "maxx") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// TestAMissingManagerConfigNamesItsPath: naming a file that is not there must not
// fall back to the defaults silently.
func TestAMissingManagerConfigNamesItsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, _, err := newManagerConfig([]string{"-config", path}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error %v does not name %q", err, path)
	}
}
