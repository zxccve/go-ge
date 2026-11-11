package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeServerJSON puts a server.json in a temp folder and returns its path. The
// tests never call main(): they resolve the SAME config main() resolves, which
// is the part that can start a barrack or a zone on the wrong values.
func writeServerJSON(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestABarrackAndAZoneResolveFromAConfigFileAlone is the rail's first pass
// condition. Each role is named by its OWN file and NOTHING else: only the one
// -config flag reaches the parser (NFlag proves it), and every resolved value -
// listen address, role, map, engine - comes from the file.
func TestABarrackAndAZoneResolveFromAConfigFileAlone(t *testing.T) {
	barrackFile := writeServerJSON(t, `{
		"listen": {"barrack_addr": ":7201"},
		"paths":  {"log": "../logs/test_barrack.log"},
		"mode":   {"role": "barrack"}
	}`)
	zoneFile := writeServerJSON(t, `{
		"listen": {"barrack_addr": ":7202"},
		"paths":  {"log": "../logs/test_zone.log", "mesh_dir": "../navmesh/mesh"},
		"engine": {"command": "wine", "args": ["./pathtool.exe"]},
		"mode":   {"role": "zone", "map": 1331}
	}`)

	barrack, bf, err := bindConfig([]string{"-config", barrackFile}, io.Discard)
	if err != nil {
		t.Fatalf("barrack: %v", err)
	}
	if barrack.Mode.Role != "barrack" || barrack.Listen.BarrackAddr != ":7201" {
		t.Fatalf("barrack resolved role %q addr %q", barrack.Mode.Role, barrack.Listen.BarrackAddr)
	}
	if barrack.Paths.Log != "../logs/test_barrack.log" {
		t.Fatalf("barrack log = %q", barrack.Paths.Log)
	}
	if got := bf.NFlag(); got != 1 {
		t.Fatalf("barrack: %d flags were set, want only -config", got)
	}

	zone, zf, err := bindConfig([]string{"-config", zoneFile}, io.Discard)
	if err != nil {
		t.Fatalf("zone: %v", err)
	}
	if zone.Mode.Role != "zone" || zone.Mode.Map != 1331 || zone.Listen.BarrackAddr != ":7202" {
		t.Fatalf("zone resolved role %q map %d addr %q", zone.Mode.Role, zone.Mode.Map, zone.Listen.BarrackAddr)
	}
	tool, launcher, extra := zone.Engine.Split()
	if tool != "./pathtool.exe" || launcher != "wine" || len(extra) != 0 {
		t.Fatalf("zone engine split = %q %q %v", tool, launcher, extra)
	}
	if got := zf.NFlag(); got != 1 {
		t.Fatalf("zone: %d flags were set, want only -config", got)
	}
	// The one line an operator reads first names the file and the three values
	// that make a wrong file obvious.
	if got := zone.Describe(); !strings.Contains(got, "read "+zoneFile) ||
		!strings.Contains(got, "role zone, map 1331") ||
		!strings.Contains(got, `engine "wine ./pathtool.exe"`) {
		t.Fatalf("startup line = %q", got)
	}
}

// TestAFlagOverridesTheFileForItsOwnField - and ONLY its own field.
func TestAFlagOverridesTheFileForItsOwnField(t *testing.T) {
	file := writeServerJSON(t, `{
		"listen": {"barrack_addr": ":7201"},
		"mode":   {"role": "zone", "map": 1331}
	}`)
	cfg, _, err := bindConfig([]string{"-config", file, "-addr", ":7299", "-map", "6004"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.BarrackAddr != ":7299" || cfg.Mode.Map != 6004 {
		t.Fatalf("flags did not win: addr %q map %d", cfg.Listen.BarrackAddr, cfg.Mode.Map)
	}
	if cfg.Mode.Role != "zone" {
		t.Fatalf("a flag the operator did not pass changed: role %q", cfg.Mode.Role)
	}
	if cfg.Paths.JobStats != "../data/job_stats.json" {
		t.Fatalf("untouched field changed: job_stats %q", cfg.Paths.JobStats)
	}
}

// TestAMissingConfigFileFailsWithThePath is the rail's second pass condition:
// an operator who names a file gets that file, or one line naming it.
func TestAMissingConfigFileFailsWithThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no_such_server.json")
	cfg, _, err := bindConfig([]string{"-config", missing}, io.Discard)
	if err == nil {
		t.Fatal("a missing server.json was accepted")
	}
	if cfg != nil {
		t.Fatal("a missing file still produced a config")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name %q", err, missing)
	}
}

// TestEveryFlagResolvesTheSameAsTheFile walks EVERY registered flag, feeds it
// back as a command-line flag, and requires the result to equal the file's
// config field for field. A flag wired to the wrong field, or a config field
// with no flag, fails here without a second table to maintain.
func TestEveryFlagResolvesTheSameAsTheFile(t *testing.T) {
	// HERMETIC ON PURPOSE. The second half of this test resolves from FLAGS with no
	// -config, and bindConfig then falls back to config.Preload() - a server.json in the
	// working directory. The repo's own goserver/server.json now carries a "manager"
	// section (the Windows fleet's policy), so without a temp working directory this test
	// compared the fixture file against the repo's file. 2026-11-11.
	t.Chdir(t.TempDir())
	file := writeServerJSON(t, `{
		"listen": {"barrack_addr": ":7301", "zone_addr": "10.0.0.5:7002", "advertise": "10.0.0.9", "machine": "vps"},
		"paths":  {"log": "../logs/x.log", "barrack_table": "../data/bt.txt", "zone_table": "../data/zt.txt",
		           "replay_dir": "../data/rp", "zones_dir": "../out/z1", "presence_dir": "../out/z2",
		           "points": "../data/p.json", "xml_dir": "../xml", "npc_dir": "../data/npcs",
		           "npc_hide": "../config/hide.txt", "mesh_dir": "../navmesh/m", "nav_dir": "../data/nav",
		           "lua_corpus": "../out/lua", "dictionary": "../ge/d.xml", "id_map": "../data/id.json",
		           "item_catalog": "../data/ic.json", "job_stats": "../data/js.json",
		           "item_stats": "../data/is.json", "prices": "../data/pr.json",
		           "feso_prices": "../data/fp.json", "equip_slots": "../data/eq.json",
		           "shop_groups": "../data/sg.json", "custom_shops": "../data/cs.json"},
		"engine": {"command": "wine", "args": ["./pathtool.exe", "--quiet"]},
		"mode":   {"role": "zone", "map": 1331, "spawn": "1 2 3", "monai": "script",
		           "respawn": "2s", "spawn_dummy": "11600", "dummy_cell": "1,2",
		           "spawn_monster": true, "spawn_splash_pair": true},
		"scripts": {"no_load": true, "no_refresh": true},
		"diagnostics": {"raw_log": true, "stop_offset": 7, "hex": true, "events": true,
		                "distance": true, "strict_range": true, "sight_off": true,
		                "sp_lock": true, "sim_latency_ms": 150, "metrics_every": "3s"},
		"probe": {"replay_cmds": "16,17", "jp_barrack": true, "jp_list16": true,
		          "jp_subset": "16", "jp_one": true, "jp_bare": true, "login_ok_capture": "x"}
	}`)
	fromFile, fs, err := bindConfig([]string{"-config", file}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	var args []string
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" {
			return // the file itself, not a value inside it
		}
		args = append(args, "-"+f.Name+"="+f.Value.String())
	})
	// 56 pre-config flags, the new -pathtool-args, -textimage (the shared dialog-text
	// image), -prewarm (ask the manager for this map's neighbours) and -quietspawn
	// (the startup spawner's two per-body lines, counted instead of written). If a
	// flag is dropped, this number and the equivalence below both fail.
	if len(args) != 60 {
		t.Fatalf("%d flags registered besides -config, want 60", len(args))
	}

	fromFlags, flagFS, err := bindConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Source is provenance, not a setting; the flag run may have discovered the
	// repo's own server.json before the flags overwrote it.
	fromFile.Source, fromFlags.Source = "", ""
	if !reflect.DeepEqual(fromFile, fromFlags) {
		t.Fatalf("the file and the flags resolved differently:\n file: %+v\nflags: %+v", *fromFile, *fromFlags)
	}

	// The 56 pre-config names are still ALL here: the file path and the flag
	// path cover the same setting set, which is what "flags-covered 56/56" means.
	known := strings.Fields(`addr log table replay replay-cmds jpbarrack jplist16 jpsubset textimage
		jpone jpbare loginok-capture zone advertise machine zones role zone-table xml spawn
		points map presence npcs npchide rawlog diag-stop-offset diag-hex diag-events
		spawnmonster spawnsplashpair spawndummy dummycell strictrange sightoff diag-distance
		splock simlatency respawn monai metrics pathtool meshdir nav lua no-scripts norefresh
		dict idmap items jobstats itemstats prices fesoprices equipslots shops customshops prewarm
		quietspawn`)
	if len(known) != 59 {
		t.Fatalf("the known-flag list has %d names, want 59", len(known))
	}
	for _, name := range known {
		if flagFS.Lookup(name) == nil {
			t.Errorf("flag -%s is gone from bindConfig", name)
		}
	}
}

// TestConfigAfterAFlagThatTakesAValue: -log consumes its FILE before -config is
// read. A hand-rolled pre-scan cannot know that (it stopped at the log path),
// which is exactly why bindConfig asks the real flag package twice.
func TestConfigAfterAFlagThatTakesAValue(t *testing.T) {
	file := writeServerJSON(t, `{"mode": {"role": "zone", "map": 208}}`)
	cfg, _, err := bindConfig([]string{"-log", "../logs/x.log", "-config", file}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode.Map != 208 || cfg.Paths.Log != "../logs/x.log" {
		t.Fatalf("map %d log %q, want 208 and ../logs/x.log", cfg.Mode.Map, cfg.Paths.Log)
	}
	// -config=FILE is the same flag.
	cfg, _, err = bindConfig([]string{"-config=" + file}, io.Discard)
	if err != nil || cfg.Mode.Map != 208 {
		t.Fatalf("-config=FILE: map %d err %v", cfg.Mode.Map, err)
	}
}

// TestTheStartupLineNamesTheResolvedFile pins the one line an operator reads
// first: the file, the role, the map and the engine, so a zone started with the
// barrack's file is visible before it accepts a connection.
func TestTheStartupLineNamesTheResolvedFile(t *testing.T) {
	file := writeServerJSON(t, `{
		"engine": {"command": "wine", "args": ["./pathtool.exe"]},
		"mode":   {"role": "zone", "map": 1331}
	}`)
	cfg, _, err := bindConfig([]string{"-config", file}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := captureLog(t, func() { announceConfig(cfg) })
	want := "config: read " + file + ` (role zone, map 1331, engine "wine ./pathtool.exe")` + "\n"
	if got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
}
