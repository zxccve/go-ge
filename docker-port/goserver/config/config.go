// Package config is the ONE place a goserver setting is named.
//
// Three layers decide every value, in this order: the built-in default below,
// then the server.json file, then a command-line flag. The flag layer lives in
// main.go and writes straight into this struct, so "a flag overrides the file"
// is not a rule anyone has to remember - it is the only place a flag can write.
//
// WHY THE FILE EXISTS (owner, 2026-11-08): a role used to be started with a
// 56-flag command line. `-jobstats ../data/job_stats.json` and four flags like
// it take a FILE argument, so one bare `-jobstats` silently ate the next flag's
// name and the zone ran with no stats - ATK 0 and "damage 1" on every hit
// (durable fact #217). A file cannot swallow its neighbour. The long flag line
// also differs between Windows and Linux in exactly one place, the path engine,
// so that line is DATA (engine.command + engine.args) and never a runtime.GOOS
// branch.
//
// All built-in paths use forward slashes: Go accepts `/` on Windows, so one
// form serves both platforms.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"geserver/address"
	"geserver/script"
	"geserver/text"
	"geserver/world"
)

// Config is server.json, section by section. Every field is also a flag; the
// JSON name says what the value IS, the flag keeps the name it always had.
type Config struct {
	Listen      Listen      `json:"listen"`
	Paths       Paths       `json:"paths"`
	Engine      Engine      `json:"engine"`
	Mode        Mode        `json:"mode"`
	Scripts     Scripts     `json:"scripts"`
	Diagnostics Diagnostics `json:"diagnostics"`
	Probe       Probe       `json:"probe"`

	// Manager is cmd/zoneman's own settings. The zone and barrack ACCEPT this
	// section and ignore it; the manager's loader reads it. It is a separate
	// section rather than shared fields on purpose: several of these defaults
	// differ from the zone's (monai "script" here, "go" in Mode; zones_dir
	// "zones" here, "../out/zones" in Paths), so sharing one field would
	// change someone's behaviour the day the other side moved.
	Manager Manager `json:"manager"`

	// Source is the file the values came from, or "" when the built-in
	// defaults were used. It is not part of server.json: it is the answer to
	// "did the server read the file I think it read", printed at startup.
	Source string `json:"-"`
}

// Listen is where each role accepts connections, and what address the CLIENT
// is told to dial.
type Listen struct {
	BarrackAddr string `json:"barrack_addr"` // -addr, barrack role
	ZoneAddr    string `json:"zone_addr"`    // -zone, overrides the announced address
	Advertise   string `json:"advertise"`    // -advertise, ip the client is told to dial
	Machine     string `json:"machine"`      // -machine, address-book row for this machine
}

// Paths is every file and folder a role reads or writes. Bare names are
// relative to the working directory, which the service recipes set to the
// folder holding the binary.
type Paths struct {
	Log          string `json:"log"`           // -log, empty = console only
	BarrackTable string `json:"barrack_table"` // -table
	ZoneTable    string `json:"zone_table"`    // -zone-table
	ReplayDir    string `json:"replay_dir"`    // -replay
	ZonesDir     string `json:"zones_dir"`     // -zones, zone registry
	PresenceDir  string `json:"presence_dir"`  // -presence
	Points       string `json:"points"`        // -points, zone_points.json
	XMLDir       string `json:"xml_dir"`       // -xml, empty = GE_XML_DIR then the repo layout
	NPCDir       string `json:"npc_dir"`       // -npcs
	NPCHide      string `json:"npc_hide"`      // -npchide
	MeshDir      string `json:"mesh_dir"`      // -meshdir
	NavDir       string `json:"nav_dir"`       // -nav
	LuaCorpus    string `json:"lua_corpus"`    // -lua
	Dictionary   string `json:"dictionary"`    // -dict
	IDMap        string `json:"id_map"`        // -idmap
	TextImage    string `json:"text_image"`    // -textimage
	ItemCatalog  string `json:"item_catalog"`  // -items
	JobStats     string `json:"job_stats"`     // -jobstats
	ItemStats    string `json:"item_stats"`    // -itemstats
	Prices       string `json:"prices"`        // -prices
	FesoPrices   string `json:"feso_prices"`   // -fesoprices
	EquipSlots   string `json:"equip_slots"`   // -equipslots
	ShopGroups   string `json:"shop_groups"`   // -shops
	CustomShops  string `json:"custom_shops"`  // -customshops
}

// Engine is how the path helper is started: the COMMAND, then the args it
// needs before the mesh. This is the ONE line that differs between platforms:
//
//	Windows: {"command": "../navmesh/pathtool.exe"}
//	Linux:   {"command": "wine", "args": ["./pathtool.exe"]}
//
// When args is empty the command IS the helper and is run directly. When it is
// not, the first arg is the helper file itself (the thing os.Stat's existence
// gate checks) and the command is the program that runs it. The mesh path is
// appended last by the zone. Never a runtime.GOOS branch - the operator writes
// the line for their platform.
type Engine struct {
	Command string   `json:"command"`        // -pathtool
	Args    []string `json:"args,omitempty"` // -pathtool-args, space separated
}

// Split answers the two questions the zone asks: which FILE must exist
// (tool), and what runs it (launcher + extra args). A zero launcher means
// "run tool directly", which is exactly the pre-config behaviour.
func (e Engine) Split() (tool, launcher string, args []string) {
	if len(e.Args) == 0 {
		return e.Command, "", nil
	}
	return e.Args[0], e.Command, append([]string(nil), e.Args[1:]...)
}

// Line is the command as an operator would type it, for the startup line.
func (e Engine) Line() string {
	return strings.TrimSpace(strings.Join(append([]string{e.Command}, e.Args...), " "))
}

// Mode is WHICH server this process is and the development switches that
// change what it does. The dev fields carry omitempty on purpose: they are
// absent from a normal server.json, so a run that shows one at startup is
// visibly not a normal run.
type Mode struct {
	Role            string   `json:"role"`                        // -role, barrack|zone
	Map             int      `json:"map"`                         // -map, zone role
	Spawn           string   `json:"spawn,omitempty"`             // -spawn, "x y CELL"
	MonAI           string   `json:"monai,omitempty"`             // -monai, go|script
	Respawn         Duration `json:"respawn,omitempty"`           // -respawn, absent = the reference's 23.568 s
	SpawnDummy      string   `json:"spawn_dummy,omitempty"`       // -spawndummy
	DummyCell       string   `json:"dummy_cell,omitempty"`        // -dummycell
	SpawnMonster    bool     `json:"spawn_monster,omitempty"`     // -spawnmonster
	SpawnSplashPair bool     `json:"spawn_splash_pair,omitempty"` // -spawnsplashpair
	Prewarm         bool     `json:"prewarm,omitempty"`           // -prewarm: ask the manager for the maps this one leads to
}

// Manager is cmd/zoneman's settings: which zone binary to run, how many maps it
// may hold, how long an empty map is kept, and the paths it hands to every child
// it starts. Field names follow the flags' meanings, not their spelling, except
// where a flag name was already a good noun.
//
// The database DSN is DELIBERATELY ABSENT: it carries a password and this file is
// committed. It comes from GE_PG_DSN (or -db) only.
type Manager struct {
	Bin        string   `json:"bin"`         // -bin, the zone binary to start
	ZonesDir   string   `json:"zones_dir"`   // -zones, the zone registry
	BasePort   int      `json:"base_port"`   // -base-port, first port a zone may use
	Max        int      `json:"max"`         // -max, how many zones may run at once
	JobStats   string   `json:"job_stats"`   // -jobstats, passed to each zone
	EquipSlots string   `json:"equip_slots"` // -equipslots, passed to each zone
	ItemStats  string   `json:"item_stats"`  // -itemstats, passed to each zone
	LogDir     string   `json:"log_dir"`     // -logdir, where each zone's log goes
	Maps       string   `json:"maps"`        // -maps, allowlist "10,15" (empty = any)
	Warm       string   `json:"warm"`        // -warm, "10,15" kept warm at boot
	MemLimit   string   `json:"memlimit"`    // -memlimit, GOMEMLIMIT for each child
	Idle       Duration `json:"idle"`        // -idle, stop an unused map after this
	Linger     Duration `json:"linger"`      // -linger, keep an EMPTY used map this long
	Retain     Duration `json:"retain"`      // -retain, the activity window for "used"
	MonAI      string   `json:"monai"`       // -monai, passed to each zone
	Preload    bool     `json:"preload"`     // -preload, load maps behind a player's doors
	Prepare    bool     `json:"prepare"`     // -prepare, the world's ignition at boot
	MapTable   string   `json:"map_table"`   // -map-table, the client's map table
	MaxTries   int      `json:"max_tries"`   // -max-tries, starts before giving up
	StartWait  Duration `json:"start_wait"`  // -start-wait, longest a start may take
}

// Scripts controls the Lua corpus load. The fields mirror the flags' own
// negative sense so nothing is silently inverted between file and flag.
type Scripts struct {
	NoLoad    bool `json:"no_load"`    // -no-scripts
	NoRefresh bool `json:"no_refresh"` // -norefresh
}

// Diagnostics are the measurement switches. Every one is off in a normal run;
// they are here so a probe is a file edit and not a ten-flag command line.
type Diagnostics struct {
	RawLog       bool     `json:"raw_log"`        // -rawlog
	StopOffset   int      `json:"stop_offset"`    // -diag-stop-offset
	Hex          bool     `json:"hex"`            // -diag-hex
	Events       bool     `json:"events"`         // -diag-events
	Distance     bool     `json:"distance"`       // -diag-distance
	StrictRange  bool     `json:"strict_range"`   // -strictrange
	SightOff     bool     `json:"sight_off"`      // -sightoff
	SPLock       bool     `json:"sp_lock"`        // -splock
	QuietSpawn   bool     `json:"quiet_spawn"`    // -quietspawn
	SimLatencyMS int      `json:"sim_latency_ms"` // -simlatency
	MetricsEvery Duration `json:"metrics_every"`  // -metrics
}

// Probe is the barrack capture/replay instrumentation.
type Probe struct {
	ReplayCmds     string `json:"replay_cmds"`      // -replay-cmds
	JPBarrack      bool   `json:"jp_barrack"`       // -jpbarrack
	JPList16       bool   `json:"jp_list16"`        // -jplist16
	JPSubset       string `json:"jp_subset"`        // -jpsubset
	JPOne          bool   `json:"jp_one"`           // -jpone
	JPBare         bool   `json:"jp_bare"`          // -jpbare
	LoginOKCapture string `json:"login_ok_capture"` // -loginok-capture
}

// Duration is time.Duration with a JSON form a human can type: "2s", "500ms",
// "10s". A bare number is SECONDS ("respawn": 2 is 2s), because a number in a
// config file is never nanoseconds.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(b, &seconds); err != nil {
		return fmt.Errorf("duration must be a string like \"2s\" or a number of seconds: %w", err)
	}
	*d = Duration(time.Duration(seconds * float64(time.Second)))
	return nil
}

// Defaults is the SERVER AS IT RUNS TODAY with no flags: the pre-config
// values, path for path, except that the two Windows-separator defaults are
// written with forward slashes. Nothing else moved.
func Defaults() *Config {
	return &Config{
		Listen: Listen{
			BarrackAddr: ":7001",
			ZoneAddr:    "",
			Advertise:   "",
			Machine:     address.DefaultMachine,
		},
		Paths: Paths{
			Log:          "",
			BarrackTable: "../data/cmd_table_barrack.txt",
			ZoneTable:    "../data/cmd_table_live_z.txt",
			ReplayDir:    "../data/replay_live",
			ZonesDir:     world.DefaultDir,
			PresenceDir:  world.DefaultDir,
			Points:       "../data/zone_points.json",
			XMLDir:       "",
			NPCDir:       "../data/zone_npcs",
			NPCHide:      "../config/zone_npc_hide.txt",
			MeshDir:      "../navmesh/mesh",
			NavDir:       "../data/zone_nav",
			LuaCorpus:    script.DefaultCorpus,
			Dictionary:   text.DefaultDict,
			IDMap:        text.DefaultIDMap,
			TextImage:    text.DefaultImage,
			ItemCatalog:  "../data/item_catalog.json",
			JobStats:     "../data/job_stats.json",
			ItemStats:    "../data/item_stats.json",
			Prices:       "../data/item_prices.json",
			FesoPrices:   "../data/item_feso_prices.json",
			EquipSlots:   "../data/item_equip.json",
			ShopGroups:   "../data/shop_groups.json",
			CustomShops:  "../data/custom_shops.json",
		},
		Engine: Engine{
			Command: "../navmesh/pathtool.exe",
		},
		Mode: Mode{
			Role:  "barrack",
			Map:   10,
			MonAI: "go",
		},
		Diagnostics: Diagnostics{
			MetricsEvery: Duration(10 * time.Second),
		},
		// cmd/zoneman's own defaults, kept EXACTLY as its flags had them so this
		// change moves where a value lives and re-tunes nothing.
		Manager: Manager{
			Bin:        "goserver_zone.exe",
			ZonesDir:   "zones",
			BasePort:   7010,
			Max:        8,
			JobStats:   "../data/job_stats.json",
			EquipSlots: "../data/item_equip.json",
			ItemStats:  "../data/item_stats.json",
			LogDir:     "../logs",
			Maps:       "",
			Warm:       "",
			MemLimit:   "200MiB",
			Idle:       Duration(10 * time.Minute),
			Linger:     Duration(2 * time.Hour),
			Retain:     Duration(24 * time.Hour),
			MonAI:      "script",
			Preload:    false,
			Prepare:    true,
			MapTable:   "../../ge/xml/datatable_map.xml",
			MaxTries:   3,
			StartWait:  Duration(180 * time.Second),
		},
	}
}

// Load reads ONE server.json onto the built-in defaults. A field absent from
// the file keeps its default; a field present replaces it. An unknown key is
// an ERROR, not a silent no-op: a typo in "barrack_addr" must not look like a
// server that ignored it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Defaults()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// One JSON object, nothing after it. Trailing text is a broken file, and
	// a half-read config is exactly the class of silent failure this file
	// exists to remove.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse config %s: trailing data after the JSON object", path)
	}
	cfg.Source = path
	// nil and [] mean the same thing here (no extra args), and one form is
	// easier to compare; see bindConfig's matching normalisation.
	if len(cfg.Engine.Args) == 0 {
		cfg.Engine.Args = nil
	}
	return cfg, nil
}

// Preload resolves WHICH file a run uses, on its own: `server.json` in the
// working directory, then beside the executable (for a service, which runs
// from the binary's folder), then none. It exists for a caller that has not
// registered the flags yet; bindConfig does the real work with the flag
// package itself, so `-log a -config x` is read the way the operator means it.
func Preload() string {
	return findDefaultFile()
}

// findDefaultFile is step 2 of Preload. The working directory wins because
// `go run .` and a test run from the goserver folder have no useful executable
// directory (it is a temp path); a service recipe sets WorkingDirectory to the
// binary's folder, so both routes reach the same file there.
func findDefaultFile() string {
	const name = "server.json"
	if st, err := os.Stat(name); err == nil && !st.IsDir() {
		return name
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	near := filepath.Join(filepath.Dir(exe), name)
	if st, err := os.Stat(near); err == nil && !st.IsDir() {
		return near
	}
	return ""
}

// Describe is the ONE startup line: which file, and the three values that
// make a wrong file obvious in a glance - the role, the map and the path
// engine. A barrack has no map of its own, so it prints the map anyway (10,
// its default) rather than a blank.
func (c *Config) Describe() string {
	from := "defaults (no server.json found)"
	if c.Source != "" {
		from = "read " + c.Source
	}
	return fmt.Sprintf("%s (role %s, map %d, engine %q)", from, c.Mode.Role, c.Mode.Map, c.Engine.Line())
}
