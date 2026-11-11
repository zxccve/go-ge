package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile is the test's only fixture helper: a server.json in a temp folder,
// returned as an absolute path so an error message can be matched against it.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadOverlaysTheFileOnTheDefaults is the whole precedence rule in one
// place: a field the file names wins, a field it does not name keeps the
// value the server had before there was a file at all.
func TestLoadOverlaysTheFileOnTheDefaults(t *testing.T) {
	path := writeFile(t, "server.json", `{
		"listen": {"barrack_addr": ":7101"},
		"mode":   {"role": "zone", "map": 1331},
		"engine": {"command": "wine", "args": ["./pathtool.exe"]}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.BarrackAddr != ":7101" {
		t.Fatalf("barrack_addr = %q, want :7101", cfg.Listen.BarrackAddr)
	}
	if cfg.Mode.Role != "zone" || cfg.Mode.Map != 1331 {
		t.Fatalf("mode = %q map %d, want zone 1331", cfg.Mode.Role, cfg.Mode.Map)
	}
	if cfg.Engine.Command != "wine" || len(cfg.Engine.Args) != 1 || cfg.Engine.Args[0] != "./pathtool.exe" {
		t.Fatalf("engine = %q %v, want wine [./pathtool.exe]", cfg.Engine.Command, cfg.Engine.Args)
	}
	// Absent from the file: the built-in defaults, unchanged.
	if cfg.Paths.BarrackTable != "../data/cmd_table_barrack.txt" {
		t.Fatalf("barrack_table = %q, want the default", cfg.Paths.BarrackTable)
	}
	if cfg.Diagnostics.MetricsEvery != Duration(10*time.Second) {
		t.Fatalf("metrics_every = %v, want 10s", time.Duration(cfg.Diagnostics.MetricsEvery))
	}
	if cfg.Source != path {
		t.Fatalf("source = %q, want %q", cfg.Source, path)
	}
}

// TestAMissingFileNamesItsPath is the operator-facing half of requirement 2:
// naming a file and getting built-in defaults instead would be the same silent
// failure the flag layer already cost us.
func TestAMissingFileNamesItsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no_such_server.json")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the path %q", err, path)
	}
}

// TestAnUnknownKeyIsAnError pins the typo guard: DisallowUnknownFields is on
// purpose, and the message must name the key so the fix is obvious.
func TestAnUnknownKeyIsAnError(t *testing.T) {
	path := writeFile(t, "server.json", `{"listen": {"barrack_adr": ":7001"}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a misspelled key was silently ignored")
	}
	if !strings.Contains(err.Error(), "barrack_adr") {
		t.Fatalf("error %q does not name the key", err)
	}
}

// TestATrailingObjectIsAnError: half-read JSON must not look like a config.
func TestATrailingObjectIsAnError(t *testing.T) {
	path := writeFile(t, "server.json", `{"mode": {"map": 10}} {"mode": {"map": 11}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("trailing data was accepted")
	}
}

// TestDurationAcceptsBothForms: strings are the documented form, numbers are
// SECONDS. A number is never nanoseconds in a file a human edits.
func TestDurationAcceptsBothForms(t *testing.T) {
	path := writeFile(t, "server.json", `{"mode": {"respawn": "2s"}, "diagnostics": {"metrics_every": 3}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.Mode.Respawn); got != 2*time.Second {
		t.Fatalf("respawn = %v, want 2s", got)
	}
	if got := time.Duration(cfg.Diagnostics.MetricsEvery); got != 3*time.Second {
		t.Fatalf("metrics_every = %v, want 3s", got)
	}
}

// TestPreloadFindsServerJSONOrNothing is the no-flag route an operator gets:
// `server.json` in the working directory is used when it is there, and its
// absence is not an error - it is the built-in defaults every existing command
// line has always run on.
func TestPreloadFindsServerJSONOrNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if got := Preload(); got != "" {
		t.Fatalf("Preload = %q with no file present", got)
	}
	if err := os.WriteFile("server.json", []byte(`{"mode": {"map": 6004}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if got := Preload(); got != "server.json" {
		t.Fatalf("Preload = %q, want server.json", got)
	}
	cfg, err := Load(Preload())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode.Map != 6004 {
		t.Fatalf("discovered map %d, want 6004", cfg.Mode.Map)
	}
}

// TestEngineSplitIsTheOnlyPlatformDifference is requirement 1's engine line:
// Windows names the exe as the command, Linux names wine and the exe as the
// first arg. No GOOS branch exists anywhere; this test is the contract.
func TestEngineSplitIsTheOnlyPlatformDifference(t *testing.T) {
	tool, launcher, args := Engine{Command: "../navmesh/pathtool.exe"}.Split()
	if tool != "../navmesh/pathtool.exe" || launcher != "" || args != nil {
		t.Fatalf("windows split = %q %q %v", tool, launcher, args)
	}
	tool, launcher, args = Engine{Command: "wine", Args: []string{"./pathtool.exe"}}.Split()
	if tool != "./pathtool.exe" || launcher != "wine" || len(args) != 0 {
		t.Fatalf("linux split = %q %q %v", tool, launcher, args)
	}
	if line := (Engine{Command: "wine", Args: []string{"./pathtool.exe"}}).Line(); line != "wine ./pathtool.exe" {
		t.Fatalf("engine line = %q", line)
	}
}

// TestDescribeNamesTheFileRoleMapAndEngine pins the one startup line the rail
// asks for, in both the file and the defaults case.
func TestDescribeNamesTheFileRoleMapAndEngine(t *testing.T) {
	cfg := Defaults()
	cfg.Source = "server.json"
	cfg.Mode.Role, cfg.Mode.Map = "zone", 1331
	cfg.Engine = Engine{Command: "wine", Args: []string{"./pathtool.exe"}}
	got := cfg.Describe()
	want := `read server.json (role zone, map 1331, engine "wine ./pathtool.exe")`
	if got != want {
		t.Fatalf("describe = %q, want %q", got, want)
	}
	plain := Defaults().Describe()
	if !strings.HasPrefix(plain, "defaults") || !strings.Contains(plain, `engine "../navmesh/pathtool.exe"`) {
		t.Fatalf("defaults describe = %q", plain)
	}
}

// TestTheManagerSectionIsReadAndTheZoneAcceptsIt is the trap this section created,
// pinned: server.json is read by BOTH binaries with DisallowUnknownFields on, so a
// "manager" object is legal ONLY because Config carries the field. A zone start with
// that section present must still succeed - that is the regression test for the day
// the manager's settings were added (before that, the key was an error).
func TestTheManagerSectionIsReadAndTheZoneAcceptsIt(t *testing.T) {
	path := writeFile(t, "server.json", `{
		"mode": {"role": "zone", "map": 4063},
		"manager": {"bin": "/app/out/goserver", "max": 60, "idle": "5m", "linger": "15m",
		            "prepare": false, "warm": "10,15"}
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a manager section made the file unreadable (the trap): %v", err)
	}
	if cfg.Manager.Max != 60 || cfg.Manager.Warm != "10,15" {
		t.Fatalf("manager.max/warm = %d/%q, want 60/10,15", cfg.Manager.Max, cfg.Manager.Warm)
	}
	if time.Duration(cfg.Manager.Idle) != 5*time.Minute || time.Duration(cfg.Manager.Linger) != 15*time.Minute {
		t.Fatalf("manager idle/linger = %v/%v, want 5m/15m",
			time.Duration(cfg.Manager.Idle), time.Duration(cfg.Manager.Linger))
	}
	if cfg.Manager.Prepare {
		t.Fatal("manager.prepare = true, want false")
	}
	// The ZONE's own fields are untouched by the section.
	if cfg.Mode.Map != 4063 || cfg.Paths.BarrackTable != "../data/cmd_table_barrack.txt" {
		t.Fatalf("the manager section disturbed the zone: map %d, table %q",
			cfg.Mode.Map, cfg.Paths.BarrackTable)
	}
	// A manager field the file does not name keeps the manager's HISTORIC default.
	if time.Duration(cfg.Manager.StartWait) != 180*time.Second || cfg.Manager.MaxTries != 3 {
		t.Fatalf("manager defaults changed: start_wait %v, max_tries %d",
			time.Duration(cfg.Manager.StartWait), cfg.Manager.MaxTries)
	}
	if cfg.Manager.MonAI != "script" {
		t.Fatalf("manager.monai = %q, want script (its own default, not Mode.Monai's go)", cfg.Manager.MonAI)
	}
}
