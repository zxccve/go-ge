// Command goserver is the Go barrack server, in progress.
//
// It answers the login (CB_LOGIN -> BC_LOGINOK) with the account read from
// PostgreSQL, then the barrack entry (CB_STARTBARRACK -> the captured barrack
// burst). Everything else is logged and left unanswered on purpose.
//
// Run from this folder:
//
//	go run . -addr :7001
//
// or double-click run_barrack.cmd, which frees the port first.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"geserver/address"
	"geserver/barrack"
	"geserver/config"
	"geserver/db"
	"geserver/item"
	"geserver/protocol"
	"geserver/script"
	"geserver/text"
	"geserver/world"
	"geserver/zone"
)

// cmdHotkey is the script-binding table the live barrack sends right after
// LOGINOK; everything else in the capture follows when the client asks to
// enter the barrack.
const (
	cmdHotkey      = 56 // the script-binding table
	cmdBarrackInfo = 16 // BC_BARRACK_INFO, the answer to CB_REQ_BARRACK_INFO
)

// registerFlags names every setting ONCE: the flag keeps the long-standing name
// and usage text, and writes straight into the config field it belongs to.
// bindConfig calls it twice (defaults, then the file), which is why it is its
// own function rather than 230 lines inside it. See package config for the file
// format.
func registerFlags(fs *flag.FlagSet, cfg *config.Config) *string {
	fs.StringVar(&cfg.Source, "config", cfg.Source,
		"server.json to read (empty = the file beside the binary, then the built-in "+
			"defaults); every flag below overrides the same-named value in it")
	pathToolArgs := fs.String("pathtool-args", strings.Join(cfg.Engine.Args, " "),
		"arguments the path-engine COMMAND needs before the mesh, space separated "+
			"(server.json: engine.args; Linux: ``-pathtool wine -pathtool-args "+
			"./pathtool.exe``, Windows: leave empty and point -pathtool at the exe)")
	fs.StringVar(&cfg.Listen.BarrackAddr, "addr", cfg.Listen.BarrackAddr, "TCP address to listen on")
	fs.StringVar(&cfg.Paths.Log, "log", cfg.Paths.Log, "also write the log to this file")
	fs.StringVar(&cfg.Paths.BarrackTable, "table", cfg.Paths.BarrackTable,
		"barrack command table (id size name shape)")
	fs.StringVar(&cfg.Paths.ReplayDir, "replay", cfg.Paths.ReplayDir,
		"directory of captured packets to send after LOGINOK")
	fs.StringVar(&cfg.Probe.ReplayCmds, "replay-cmds", cfg.Probe.ReplayCmds,
		"only load these commands from the replay, comma separated (empty = all)")
	fs.BoolVar(&cfg.Probe.JPBarrack, "jpbarrack", cfg.Probe.JPBarrack,
		"JP 375639 barrack: send the built barrack-info set (16/17/26/35/41/57/65) in the "+
			"STARTBARRACK burst; the JP client never sends CB_REQ_BARRACK_INFO (15)")
	fs.BoolVar(&cfg.Probe.JPList16, "jplist16", cfg.Probe.JPList16,
		"JP diagnostic: answer CB_REQ_BARRACK_INFO with cmd 16 only (drop 17/35)")
	fs.StringVar(&cfg.Probe.JPSubset, "jpsubset", cfg.Probe.JPSubset,
		"JP diagnostic: comma-separated command ids to send from the deferred barrack set (empty = all)")
	fs.BoolVar(&cfg.Probe.JPOne, "jpone", cfg.Probe.JPOne,
		"JP diagnostic: send only the first barrack character")
	fs.BoolVar(&cfg.Probe.JPBare, "jpbare", cfg.Probe.JPBare,
		"JP diagnostic: ignore worn items and fill every equipment slot with the client's None id")
	fs.StringVar(&cfg.Probe.LoginOKCapture, "loginok-capture", cfg.Probe.LoginOKCapture,
		"PROBE: send this captured login answer instead of a built one (empty = build it)")
	fs.StringVar(&cfg.Listen.ZoneAddr, "zone", cfg.Listen.ZoneAddr,
		"override the WHOLE host:port the client is sent to in BC_STARTGAMEOK when no "+
			"live zone announces the map; empty = the advertised address (see -advertise "+
			"and -machine) and the project's own zone port")
	fs.StringVar(&cfg.Listen.Advertise, "advertise", cfg.Listen.Advertise,
		"ip the CLIENT is told to dial, OVERRIDING the address book in PostgreSQL "+
			"(empty = read the machine row; if it cannot be read the documented default "+
			"is used and one startup line says why). Both roles take it. No port comes "+
			"from here: a town announces the port it actually bound")
	fs.StringVar(&cfg.Listen.Machine, "machine", cfg.Listen.Machine,
		"which row of the address book (table machine) names THIS machine's address")
	fs.StringVar(&cfg.Paths.ZonesDir, "zones", cfg.Paths.ZonesDir,
		"zone registry: the presence files src\\world.py writes, one per zone - the "+
			"barrack finds a map's zone here, and so does the gate out of a zone (it "+
			"also holds the handoff files a map change leaves)")
	fs.StringVar(&cfg.Mode.Role, "role", cfg.Mode.Role,
		"which server to run: barrack (login + barrack) or zone (in progress)")
	fs.StringVar(&cfg.Paths.ZoneTable, "zone-table", cfg.Paths.ZoneTable,
		"zone command table (id size name shape), used by -role zone")
	fs.StringVar(&cfg.Paths.XMLDir, "xml", cfg.Paths.XMLDir,
		`folder holding datatable_stance/job/stancecondition.xml (empty = GE_XML_DIR, then the repo layout), used by -role zone`)
	fs.StringVar(&cfg.Mode.Spawn, "spawn", cfg.Mode.Spawn,
		`where the team enters, as "x y CELL" (a navmesh triangle index, not a height); empty = the map's own point from -points, used by -role zone`)
	fs.StringVar(&cfg.Paths.Points, "points", cfg.Paths.Points,
		"per-map spawn points and map names written by src\\emit_zone_points.py (used when -spawn is empty, and by the barrack for a map's name)")
	fs.IntVar(&cfg.Mode.Map, "map", cfg.Mode.Map, "map id this zone serves, used by -role zone")
	fs.StringVar(&cfg.Paths.PresenceDir, "presence", cfg.Paths.PresenceDir,
		"folder this zone announces itself in, one file per zone, read by the barrack - the same folder -zones reads")
	fs.StringVar(&cfg.Paths.NPCDir, "npcs", cfg.Paths.NPCDir,
		"folder of per-map town NPC data written by src\\emit_zone_npcs.py")
	fs.StringVar(&cfg.Paths.NPCHide, "npchide", cfg.Paths.NPCHide,
		"hand-written list of town NPCs NOT to spawn: SCRIPTNAME, or map<id>:SCRIPTNAME for one map")
	fs.BoolVar(&cfg.Diagnostics.RawLog, "rawlog", cfg.Diagnostics.RawLog,
		"diagnosis: log every decrypted client chunk and the frames cut from it (noisy; off by default)")
	fs.IntVar(&cfg.Diagnostics.StopOffset, "diag-stop-offset", cfg.Diagnostics.StopOffset,
		"DIAGNOSTIC, zone only, default OFF: move the x of every ZC_STOP_PC this zone "+
			"broadcasts east by this many units, so the other client's model jumps "+
			"sideways if it found the handle (0 = off)")
	fs.BoolVar(&cfg.Diagnostics.Hex, "diag-hex", cfg.Diagnostics.Hex,
		"DIAGNOSTIC, zone only, default OFF: dump the full bytes of every announced "+
			"character's 3520 spawn and 3554 stance as DIAGHEX lines, so a leader and a "+
			"follower can be diffed byte for byte (0 = off)")
	fs.BoolVar(&cfg.Diagnostics.Events, "diag-events", cfg.Diagnostics.Events,
		"DIAGNOSTIC, zone only, default OFF: write one log line per echoed move, one "+
			"per stop and one per walk leg, as this zone did before they were counted "+
			"instead. OFF they are not written at all, but the METRICS line counts them "+
			"(moves=/stops=/walks=), so a run still says how many happened. They are off "+
			"because they sit on the PER-PACKET path: at 50 bots they are ~100 lines/s "+
			"and at 300 players in one town ~1000, which buries every other line in the "+
			"log. Turn this on only to read a single event's line")
	fs.BoolVar(&cfg.Diagnostics.QuietSpawn, "quietspawn", cfg.Diagnostics.QuietSpawn,
		"DEVELOPMENT FLAG, zone only, default OFF: the startup spawner does NOT write "+
			"its two per-body lines (`zone: MONSTER AI class ...` and `zone: CREATE_TMONSTER "+
			"...`) and prints ONE line per distinct tactics decision when it finishes. "+
			"MEASURED: those 858 lines on Katovic cost 3-8 ms of a ~178 ms spawner, so this "+
			"is a LOG switch, not a speed one - it exists because they are 84% of a start's "+
			"lines. NOTHING IS LOST: a body whose tactics row does not resolve still prints "+
			"both lines, and every body created outside the startup burst (a script's "+
			"CreateTmonster, a probe, a respawn) prints in full. See zone\\spawn_quiet.go")
	fs.BoolVar(&cfg.Mode.SpawnMonster, "spawnmonster", cfg.Mode.SpawnMonster,
		"DEVELOPMENT FLAG, zone only, default OFF: on the first GAME_READY send ONE "+
			"ZC_ENTER_STANDINGMONSTER (3527, 360 bytes) that draws a single monster at a "+
			"fixed spot on Thueringen Lakeside (map 1331). Off, the wire is unchanged "+
			"for every map. See zone\\monster.go - slice S1")
	fs.BoolVar(&cfg.Mode.SpawnSplashPair, "spawnsplashpair", cfg.Mode.SpawnSplashPair,
		"DEVELOPMENT FLAG, zone only, default OFF: right after the map's own startup "+
			"tactics have run, create TWO Scarecrows (class 11600) 80 units apart at "+
			"(3000,-1800) and (3080,-1800), so one area skill can be tested against "+
			"two bodies. Off, nothing is created and the wire is unchanged for every "+
			"map. See main.go spawnSplashPair")
	fs.StringVar(&cfg.Mode.SpawnDummy, "spawndummy", cfg.Mode.SpawnDummy,
		"DEVELOPMENT FLAG, zone only, default OFF (empty): a comma-separated list of "+
			"monster class ids, e.g. 11600,10232,100212,153538. Right after the map's "+
			"own startup tactics, ONE real Monster row of each class is created at a "+
			"fixed walkable cell on the map, and the DEF the game's own SCR_Get_MON_DEF "+
			"computes for it is logged - the target-defence probe of R84 slice 2. Off, "+
			"nothing is created and the wire is unchanged for every map. See main.go "+
			"spawnDummies")
	fs.StringVar(&cfg.Mode.DummyCell, "dummycell", cfg.Mode.DummyCell,
		"DEVELOPMENT FLAG, zone only, default EMPTY, and only read when -spawndummy "+
			"is set: the cells -spawndummy puts its monsters on, as x,y pairs, e.g. "+
			"\x22-8856,4111\x22 or \x22-8856,4111,-8756,4111\x22. It exists because the "+
			"four default cells were measured on map 1331 and an off-mesh spawn is "+
			"REFUSED on every other map, so a probe on a map that allows drops (1301, "+
			"208, 6004) has to give its own ground. Empty keeps the measured strip; a "+
			"pair that does not parse refuses the whole probe with ONE log line and "+
			"creates nothing. See main.go parseDummyCells")
	fs.BoolVar(&cfg.Diagnostics.StrictRange, "strictrange", cfg.Diagnostics.StrictRange,
		"DEVELOPMENT FLAG, zone only, default OFF: refuse a skill whose target or point "+
			"is outside the skill's own MinR..MaxR (nothing sent), instead of logging the "+
			"distance and applying the cast anyway. OFF is the normal value because the "+
			"reference's own rule for an OUT-OF-RANGE press is not measured yet - the one "+
			"capture (captures\\skillcast20261031, Rapid Shot at 919 of MaxR 1200) is "+
			"inside its range. See zone\\skill_cast.go and zone\\skill_area.go - slice R84")
	fs.BoolVar(&cfg.Diagnostics.SightOff, "sightoff", cfg.Diagnostics.SightOff,
		"DEVELOPMENT FLAG, zone only, default OFF: the sight RING is INERT for the whole "+
			"run - sightArmed is false, sightTick answers nil, and every session is told "+
			"about every player and monster exactly as before R135. Nothing is ever "+
			"entered or left by range; the entry burst, the spawn/kill broadcasts and the "+
			"leave on logout are the whole announcement. It exists so the CLIENT's own "+
			"draw distance can be measured (beside -diag-distance); OFF the ring is "+
			"unchanged. See zone\\sight.go - R135c")
	fs.BoolVar(&cfg.Diagnostics.Distance, "diag-distance", cfg.Diagnostics.Distance,
		"MEASUREMENT SWITCH, zone only, default OFF: once a second, one log line per "+
			"pair of sessions naming both LEADERS' positions and their rounded X/Y "+
			"distance, plus `| DIST to <handle> = <N> units` on every STOP the zone "+
			"sends. Log only - no packet, state or rule changes. Off, no line is written. "+
			"See zone\\diag_distance.go - R135c")

	fs.BoolVar(&cfg.Diagnostics.SPLock, "splock", cfg.Diagnostics.SPLock,
		"DEVELOPMENT FLAG, zone only, default OFF: never spend SP - a skill's own SpendSP "+
			"is cut and then put back, so the caster's bar stays full and a skill costing "+
			"hundreds of SP can be pressed all session. OFF is the reference's own behaviour; "+
			"nothing else about the cast changes. Exists because this server has no SP "+
			"regeneration, so a formula test would otherwise stop on the client's side. "+
			"See zone\\skill_area.go and zone\\zone.go (Handler.SPLock)")
	fs.IntVar(&cfg.Diagnostics.SimLatencyMS, "simlatency", cfg.Diagnostics.SimLatencyMS,
		"DEVELOPMENT FLAG, zone only, default 0 = OFF: hold every packet this zone "+
			"writes to a client back by this many milliseconds, so a run on 127.0.0.1 "+
			"costs something like a real network's latency. Order is preserved, the read "+
			"loop never sleeps, and 0 is byte for byte today's behaviour. No game rule "+
			"changes. See zone\\writer.go")
	fs.DurationVar((*time.Duration)(&cfg.Mode.Respawn), "respawn", time.Duration(cfg.Mode.Respawn),
		"DEVELOPMENT FLAG, zone only, default 0 = the reference's 23.568 s: how long "+
			"after a kill a monster this zone created comes back, e.g. -respawn 2s. It "+
			"replaces the reference's measured interval for EVERY body in the run, so "+
			"one kill can be followed through its corpse, its leave and its return "+
			"without a 23.6 s wait; 0 (or any value that is not positive) keeps "+
			"23.568 s and changes nothing. The startup log says RESPAWN OVERRIDE when it "+
			"is on. See zone/respawn.go in the goserver tree")
	fs.StringVar(&cfg.Mode.MonAI, "monai", cfg.Mode.MonAI,
		"DEV SWITCH, zone only, default go: WHICH ENGINE ANSWERS A MONSTER'S "+
			"DECISIONS. go = today's Go chase and swing, byte-identical. script = a "+
			"living body thinks through the game's OWN tactics script "+
			"(SCR_<Tactics>_TS_<state> -> SCR_MON_AI_LIB_TS_BATTLE in "+
			"ge\\script\\monster\\mon_ts_battle.scp), run as a thread and woken by the "+
			"script's own sleep; the Go chase is OFF in that mode and stays in the tree "+
			"as the fallback. The startup line says MONSTER AI <mode>. See "+
			"zone/monster_ai.go and script/monster_ai.go")
	fs.DurationVar((*time.Duration)(&cfg.Diagnostics.MetricsEvery), "metrics", time.Duration(cfg.Diagnostics.MetricsEvery),
		"zone only, default 10s: log one METRICS line this often (packets and bytes in "+
			"and out, broadcasts and their recipient count, handler microseconds); "+
			"0 disables it. Nothing is sent to a client and no game rule reads it. "+
			"See zone\\metrics.go")
	fs.StringVar(&cfg.Engine.Command, "pathtool", cfg.Engine.Command,
		"the game's path helper; without it a click walks a straight line, used by -role zone")
	fs.StringVar(&cfg.Paths.MeshDir, "meshdir", cfg.Paths.MeshDir,
		"folder of per-map .tok walk-meshes, used with -pathtool by -role zone")
	fs.StringVar(&cfg.Paths.NavDir, "nav", cfg.Paths.NavDir,
		"folder of per-map walk graphs written by src\\emit_zone_nav.py (used when there is no -pathtool)")
	fs.StringVar(&cfg.Paths.LuaCorpus, "lua", cfg.Paths.LuaCorpus,
		"translated script corpus written by `src\\scp_to_lua.py --dump`, used by -role zone")
	fs.BoolVar(&cfg.Scripts.NoLoad, "no-scripts", cfg.Scripts.NoLoad,
		"do not load the script corpus: clicks are then logged and dropped, used by -role zone")
	fs.BoolVar(&cfg.Mode.Prewarm, "prewarm", cfg.Mode.Prewarm,
		"ask the zone manager for every map this one has a gate to, when a player enters "+
			"(off by default: a warm map costs ~142 MB until the manager's idle stop takes "+
			"it). Used by -role zone")
	fs.BoolVar(&cfg.Scripts.NoRefresh, "norefresh", cfg.Scripts.NoRefresh,
		"do not refresh the Lua script corpus at startup: the zone then indexes "+
			"whatever src\\scp_to_lua.py last produced. Off by default, so a restart "+
			"picks up an edited .scp the way the reference server does; implied by "+
			"-no-scripts. Used by -role zone")
	fs.StringVar(&cfg.Paths.Dictionary, "dict", cfg.Paths.Dictionary,
		"the client's dictionary_local.xml, used to turn dialog tokens into text")
	fs.StringVar(&cfg.Paths.IDMap, "idmap", cfg.Paths.IDMap,
		"old-id -> client-id map written by src\\make_locale_map.py")
	fs.StringVar(&cfg.Paths.TextImage, "textimage", cfg.Paths.TextImage,
		"the shared dialog-text image (go run ./cmd/mktext): when it maps, every zone "+
			"shares ONE physical copy of the 73 MB of text instead of loading it per process")
	fs.StringVar(&cfg.Paths.ItemCatalog, "items", cfg.Paths.ItemCatalog,
		"item NAME -> class id + stack rule, written by src\\emit_item_catalog.py")
	fs.StringVar(&cfg.Paths.JobStats, "jobstats", cfg.Paths.JobStats,
		"job id -> the six class base stats (STR, CON, DEX, INT, AGI, CHA), copied "+
			"from the client's own datatable_job.ies into data\\job_stats.json; "+
			"without it Character Info shows 0 for every stat")
	fs.StringVar(&cfg.Paths.ItemStats, "itemstats", cfg.Paths.ItemStats,
		"item class id -> the stat columns the status rules read (ATK, DEF, AR, "+
			"WLv, ShldDR, HR, ASPD, the resistances ...), copied from the client's "+
			"own datatable_item_*.xml into data\\item_stats.json; without it the "+
			"DERIVED half of Character Info (ATK, DEF, A.R. ...) is sent as 0")
	fs.StringVar(&cfg.Paths.Prices, "prices", cfg.Paths.Prices,
		"class id -> price / sell price / venderable, written by src\\emit_shop_data.py")
	fs.StringVar(&cfg.Paths.FesoPrices, "fesoprices", cfg.Paths.FesoPrices,
		"class id -> FESO price / sell price / venderable, written by src\\emit_shop_data.py")
	fs.StringVar(&cfg.Paths.EquipSlots, "equipslots", cfg.Paths.EquipSlots,
		"class id -> wearable slot(s), written by src\\emit_item_equip.py")
	fs.StringVar(&cfg.Paths.ShopGroups, "shops", cfg.Paths.ShopGroups,
		"normal shop GROUP -> item class ids, written by src\\emit_shop_data.py")
	fs.StringVar(&cfg.Paths.CustomShops, "customshops", cfg.Paths.CustomShops,
		"custom shop GROUP -> entries, written by src\\emit_shop_data.py")
	return pathToolArgs
}

// bindConfig resolves the ONE place every setting is named, in the order the
// rail asks for: built-in default, then server.json, then the command line.
//
// TWO PASSES, not a hand-rolled scan. The first parse runs against the built-in
// defaults just to learn what -config says; the flag package itself decides,
// so `-log a -config x` is read exactly as the operator means it (a scanner
// that does not know -log takes a value stops at "a" and misses x). The file is
// then loaded, and the second parse uses its values as the flags' defaults - so
// a flag overwrites exactly its own field, with no second table to keep in sync.
//
// A file NAMED by -config must exist: the error names the path. With no
// -config, server.json beside the binary is used if present, and its absence
// is the built-in defaults - the no-file run every existing command line does.
//
// It returns the FlagSet as well, so a test can walk every registered flag and
// prove the field it writes is the field the file fills.
func bindConfig(args []string, output io.Writer) (*config.Config, *flag.FlagSet, error) {
	probe := config.Defaults()
	first := flag.NewFlagSet("goserver", flag.ContinueOnError)
	first.SetOutput(output)
	registerFlags(first, probe)
	if err := first.Parse(args); err != nil {
		return nil, first, err
	}
	named := false
	first.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			named = true
		}
	})
	cfg := probe
	switch {
	case probe.Source != "":
		loaded, err := config.Load(probe.Source)
		if err != nil {
			return nil, first, err
		}
		cfg = loaded
	case !named:
		if path := config.Preload(); path != "" {
			loaded, err := config.Load(path)
			if err != nil {
				return nil, first, err
			}
			cfg = loaded
		}
	}

	fs := flag.NewFlagSet("goserver", flag.ContinueOnError)
	fs.SetOutput(output)
	pathToolArgs := registerFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return nil, fs, err
	}
	// GE_SIGHT_OFF=1 is the fleet-wide form of `-sightoff`: the manager passes its own environment
	// to every zone it starts, so one variable turns the ring off for all 18 maps at once. The
	// flag wins if it was given explicitly on the command line.
	if v := os.Getenv("GE_SIGHT_OFF"); v != "" && v != "0" {
		cfg.Diagnostics.SightOff = true
	}
	// An empty flag means "no args" and must stay the zero value: nil and an
	// empty slice mean the same thing to Engine.Split, but they are not equal
	// to reflect.DeepEqual, and a config resolved from a file and the same one
	// resolved from flags has to compare equal.
	if s := strings.Fields(*pathToolArgs); len(s) > 0 {
		cfg.Engine.Args = s
	} else {
		cfg.Engine.Args = nil
	}
	return cfg, fs, nil
}

// announceConfig prints the ONE resolved-config line. A function, not an inline
// log call, so a test can quote the exact text the starter writes (the same
// reason presenceRecord exists). It runs AFTER the log file is open, so the
// line lands in the log as well as the console.
func announceConfig(cfg *config.Config) {
	log.Printf("config: %s", cfg.Describe())
}

func main() {
	cfg, _, err := bindConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("config: %v", err)
	}

	// GE_MEMSTATS=1 prints the LIVE heap once a second (with a forced GC, so the number
	// is what the process really keeps, not garbage waiting to be collected). It exists
	// because private bytes cannot tell the two apart: the script index's +58 MB in the
	// 2026-11-09 measurement turned out to be transient read buffers, not a kept cost.
	if os.Getenv("GE_MEMSTATS") != "" {
		// memstats = 1 makes the STAGE lines print too: what each table costs once it is kept,
		// which is the only way to know what a second zone would share with the first.
		memstats = 1
		go func() {
			var m runtime.MemStats
			for {
				runtime.GC()
				runtime.ReadMemStats(&m)
				log.Printf("MEMSTATS live=%.1fMB heap=%.1fMB sys=%.1fMB objects=%d",
					float64(m.Alloc)/(1<<20), float64(m.HeapSys)/(1<<20),
					float64(m.Sys)/(1<<20), m.HeapObjects)
				time.Sleep(time.Second)
			}
		}()
	}

	// ONE POINTER PER SETTING, pointing into the resolved config. The rest of
	// main() reads exactly as it did when each flag owned its variable, and a
	// flag that was not passed is the server.json value. The two durations are
	// config.Duration in the file and time.Duration on the flag, so they are
	// converted by pointer rather than copied.
	addr := &cfg.Listen.BarrackAddr
	logPath := &cfg.Paths.Log
	tablePath := &cfg.Paths.BarrackTable
	replayDir := &cfg.Paths.ReplayDir
	replayCmds := &cfg.Probe.ReplayCmds
	jpBarrack := &cfg.Probe.JPBarrack
	jpList16 := &cfg.Probe.JPList16
	jpSubset := &cfg.Probe.JPSubset
	jpOne := &cfg.Probe.JPOne
	jpBare := &cfg.Probe.JPBare
	loginOKCapture := &cfg.Probe.LoginOKCapture
	zoneAddr := &cfg.Listen.ZoneAddr
	advertise := &cfg.Listen.Advertise
	machineName := &cfg.Listen.Machine
	zonesDir := &cfg.Paths.ZonesDir
	role := &cfg.Mode.Role
	zoneTable := &cfg.Paths.ZoneTable
	xmlDir := &cfg.Paths.XMLDir
	spawn := &cfg.Mode.Spawn
	pointsPath := &cfg.Paths.Points
	mapID := &cfg.Mode.Map
	presenceDir := &cfg.Paths.PresenceDir
	npcsDir := &cfg.Paths.NPCDir
	npcHide := &cfg.Paths.NPCHide
	rawLog := &cfg.Diagnostics.RawLog
	diagStopOffset := &cfg.Diagnostics.StopOffset
	diagHex := &cfg.Diagnostics.Hex
	diagEvents := &cfg.Diagnostics.Events
	quietSpawn := &cfg.Diagnostics.QuietSpawn
	spawnMonster := &cfg.Mode.SpawnMonster
	spawnSplashPair := &cfg.Mode.SpawnSplashPair
	spawnDummy := &cfg.Mode.SpawnDummy
	dummyCell := &cfg.Mode.DummyCell
	strictRange := &cfg.Diagnostics.StrictRange
	// GE_SIGHT_CAP caps BOTH sight circles at a fixed distance. It is the measurement switch for
	// "where does the CLIENT start accepting an actor at all" (see zone\sight.go sightCapRadius);
	// 0 or unset keeps the map table's rule.
	if v := os.Getenv("GE_SIGHT_CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			zone.SetSightCap(int32(n))
			log.Printf("zone: sight: GE_SIGHT_CAP=%d - both circles are capped at %d units for this run", n, n)
		}
	}
	sightOff := &cfg.Diagnostics.SightOff
	diagDistance := &cfg.Diagnostics.Distance
	spLock := &cfg.Diagnostics.SPLock
	simLatency := &cfg.Diagnostics.SimLatencyMS
	respawnDelay := (*time.Duration)(&cfg.Mode.Respawn)
	monai := &cfg.Mode.MonAI
	metricsEvery := (*time.Duration)(&cfg.Diagnostics.MetricsEvery)
	meshDir := &cfg.Paths.MeshDir
	navDir := &cfg.Paths.NavDir
	luaCorpus := &cfg.Paths.LuaCorpus
	noScripts := &cfg.Scripts.NoLoad
	noRefresh := &cfg.Scripts.NoRefresh
	dictPath := &cfg.Paths.Dictionary
	idMapPath := &cfg.Paths.IDMap
	textImagePath := &cfg.Paths.TextImage
	itemCatalog := &cfg.Paths.ItemCatalog
	jobStatsPath := &cfg.Paths.JobStats
	itemStatsPath := &cfg.Paths.ItemStats
	pricesPath := &cfg.Paths.Prices
	fesoPrices := &cfg.Paths.FesoPrices
	equipSlots := &cfg.Paths.EquipSlots
	shopGroups := &cfg.Paths.ShopGroups
	customShops := &cfg.Paths.CustomShops

	// MICROSECOND stamps, and only that: LstdFlags keeps the date and the clock
	// exactly as they were, Lmicroseconds appends the fraction. A press and its
	// answer land in the same second many times over, so a second-resolution stamp
	// cannot measure the delay between them at all. No line's text changes - only
	// the stamp it is prefixed with.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("open log %s: %v", *logPath, err)
		}
		defer f.Close()
		// Keep the console output: whoever watches the window sees the same
		// lines that land in the file.
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}

	// THE ONE STARTUP LINE: which file, and the three values that make a wrong
	// file obvious. A zone started with the barrack's server.json says so here
	// before it accepts a connection.
	announceConfig(cfg)

	// A zone is a different job with a different command table, so it gets its
	// own entry point instead of threading a flag through the barrack setup.
	if *role == "zone" {
		// The engine is DATA from server.json: the command that runs the helper,
		// and the args it needs before the mesh (empty = run the exe directly).
		tool, launcher, toolArgs := cfg.Engine.Split()
		runZone(zoneConfig{
			addr: *addr, tablePath: *zoneTable, xmlDir: *xmlDir, spawn: *spawn,
			points: *pointsPath, mapID: *mapID, npcsDir: *npcsDir, corpus: *luaCorpus,
			prewarm: cfg.Mode.Prewarm,
			noScripts: *noScripts, noRefresh: *noRefresh, dict: *dictPath, idMap: *idMapPath,
			textImage: *textImagePath,
			items:     *itemCatalog,
			prices:    *pricesPath, fesoPrices: *fesoPrices,
			shops: *shopGroups, customShops: *customShops,
			equipSlots: *equipSlots,
			jobStats:   *jobStatsPath,
			itemStats:  *itemStatsPath,
			pathtool:   tool, pathtoolLauncher: launcher, pathtoolArgs: toolArgs,
			meshdir: *meshDir, navdir: *navDir,
			presence: *presenceDir, npcHide: *npcHide, zones: *zonesDir,
			advertise: *advertise, machine: *machineName,
			rawLog:         *rawLog,
			diagStopOffset: *diagStopOffset, diagHex: *diagHex,
			diagEvents:      *diagEvents,
			quietSpawn:      *quietSpawn,
			spawnMonster:    *spawnMonster,
			spawnSplashPair: *spawnSplashPair,
			spawnDummy:      *spawnDummy,
			dummyCell:       *dummyCell,
			strictRange:     *strictRange,
			sightOff:        *sightOff,
			diagDistance:    *diagDistance,
			spLock:          *spLock,
			simLatencyMs:    *simLatency,
			respawnDelay:    *respawnDelay,
			monai:           *monai,
			metricsEvery:    *metricsEvery,
		})
		return
	}

	table, err := protocol.LoadSpec(*tablePath)
	if err != nil {
		log.Fatalf("load command table: %v", err)
	}

	// The captured barrack burst, split by WHEN the client is ready for it.
	// A missing directory is a warning (the server still logs); a broken
	// manifest is a failure, because silently dropping packets strands the
	// client with no explanation.
	var afterLogin, afterStart, afterBarrackInfo [][]byte
	if *replayDir != "" {
		if _, statErr := os.Stat(*replayDir); statErr == nil {
			packets, err := barrack.LoadReplay(*replayDir)
			if err != nil {
				log.Fatalf("load replay packets: %v", err)
			}
			keep := commandFilter(*replayCmds)
			for _, p := range packets {
				cmd := binary.LittleEndian.Uint16(p)
				if keep != nil && !keep[cmd] {
					continue
				}
				switch cmd {
				case cmdHotkey:
					afterLogin = append(afterLogin, p)
				case cmdBarrackInfo:
					// BOTH: keep it in the live server's position in the burst
					// (the client expects it there, before cmd 17), and serve it
					// again as the answer to cmd 15. Sending it twice is the
					// safe order; removing it from the burst broke the load.
					afterBarrackInfo = append(afterBarrackInfo, p)
					afterStart = append(afterStart, p)
				default:
					afterStart = append(afterStart, p)
				}
			}
			// THE JP BUILD MOVED THE HOTKEY TABLE TO 57. Cmd 56 is
			// CB_COMMANDER_TRANSFER (14 bytes) in the JP 375639 client; sending the
			// 7,571-byte table under 56 made it read 14 bytes and parse the rest as
			// packets (the loading-screen hang and the MSVC abort). The length word
			// at +2 stays 7,571, so only the command changes.
			if *jpBarrack {
				for i, p := range afterLogin {
					if len(p) >= 2 && binary.LittleEndian.Uint16(p) == cmdHotkey {
						q := append([]byte(nil), p...)
						binary.LittleEndian.PutUint16(q, 57)
						afterLogin[i] = q
					}
				}
			}
			log.Printf("replay: %d after LOGINOK, %d after STARTBARRACK, %d for the "+
				"barrack-info request (PROBE: per-player data is the capture's own)",
				len(afterLogin), len(afterStart), len(afterBarrackInfo))
		} else {
			log.Printf("replay: no %s - the client will wait and send nothing", *replayDir)
		}
	}

	// A barrack that cannot read accounts is useless, so a database that does
	// not answer is a startup failure, not a warning.
	ctx := context.Background()
	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// WHICH ADDRESS DO WE TELL THE CLIENT TO DIAL? It is NOT compiled in any more.
	// The machine row in PostgreSQL answers (-machine names the row), -advertise
	// overrides it, and the documented default answers when neither can - with ONE
	// log line saying which value was used and why. Read ONCE: a running role keeps
	// its startup state, so an edit in the table takes effect at the next restart.
	adv := address.Resolve(ctx, address.Config{
		Flag: *advertise, Machine: *machineName,
		Store: address.StoreFunc(func(ctx context.Context, machine string) (string, error) {
			return db.MachineIP(ctx, pool, machine)
		}),
		Log: log.Printf,
	})

	// The zone registry is the world layer's first piece. It is read PER
	// REQUEST, so a zone that starts or dies is picked up without a restart;
	// this check only says whether the folder is there at all, once, loudly.
	zones := &world.Registry{Dir: *zonesDir, FallbackIP: adv.IP}
	if recs, err := zones.Records(); err != nil {
		log.Printf("world: no zone registry at %s (%v) - cmd 25 falls back to %s",
			*zonesDir, err, barrack.FallbackAddr(adv.IP))
	} else {
		log.Printf("world: %d live zone(s) in %s", len(recs), *zonesDir)
	}

	var jpSubsetMap map[uint16]bool
	if *jpSubset != "" {
		jpSubsetMap = make(map[uint16]bool)
		for _, s := range strings.Split(*jpSubset, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				jpSubsetMap[uint16(n)] = true
			}
		}
	}
	barrack.JPOnlyFirst = *jpOne
	barrack.JPBareEquip = *jpBare

	handler := &barrack.Handler{
		Store:   barrack.NewPGStore(pool),
		Barrack: barrack.NewPGBarrackStore(pool),
		Table:   table,
		Cipher:  protocol.DefaultCipher,
		Log:     log.Default(),
		// THE USAGE RECORDS (db/migrations/00018_usage_log.sql): GE_LOG.ACCOUNT_LOGIN and
		// ACCOUNT_LOGOUT, one row per login and one per connection end. A failure is logged
		// by the recorder's caller and never refuses the login - the record is an artifact of
		// the login, not a condition for it.
		RecordLogin: func(ctx context.Context, memberID, ip string) {
			if err := db.RecordLogin(ctx, pool, time.Now(), memberID, ip); err != nil {
				log.Printf("barrack: %v", err)
			}
		},
		RecordLogout: func(ctx context.Context, memberID, ip string) {
			if err := db.RecordLogout(ctx, pool, time.Now(), memberID, ip); err != nil {
				log.Printf("barrack: %v", err)
			}
		},
		AfterLogin:        afterLogin,
		AfterStartBarrack: afterStart,
		AfterBarrackInfo:  afterBarrackInfo,
		JP:                *jpBarrack,
		JPList16:          *jpList16,
		JPSubset:          jpSubsetMap,
		ZoneAddr:          *zoneAddr,
		AdvertisedIP:      adv.IP,
		Zones:             zones,
		AskForZones:       true, // a missing map is a request to cmd/zoneman, not a refusal
		MapNames:          mapNames(*pointsPath),
		// The SAVED START POINT is stored as a restart warp id, so the barrack needs
		// the reverse lookup to name the town the New Game button enters at. Same
		// file, same loader, same "a missing table is not fatal" rule.
		WarpMaps: warpMaps(*pointsPath),
	}

	// PROBE: use the capture's own login answer so the whole session is
	// internally consistent (the replayed barrack belongs to the capture's
	// account, not the database's). Clear the flag to build it instead.
	if *loginOKCapture != "" {
		blob, err := os.ReadFile(*loginOKCapture)
		if err != nil {
			log.Printf("loginok-capture: %v (building the answer instead)", err)
		} else if len(blob) != 57 {
			log.Printf("loginok-capture: %s is %d bytes, want 57 (ignored)",
				*loginOKCapture, len(blob))
		} else {
			handler.LoginOKCapture = blob
			log.Printf("loginok-capture: PROBE - the login answer will be the capture's")
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("UP on %s - waiting for client...", *addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("ACCEPT failed: %v", err)
			continue
		}
		go handler.Serve(ctx, conn)
	}
}

// zoneConfig is what -role zone needs to start. It is a struct rather than eight
// more positional strings: the flags are independent of each other and a reader
// should not have to count order to know which is which.
type zoneConfig struct {
	addr      string
	tablePath string
	xmlDir    string
	spawn     string
	// points is the shared spawn table: docker-port\data\zone_points.json, one file
	// for every map. It answers -spawn when that flag is EMPTY, which is the default
	// and the only way the zone can serve a map other than the one it was written on.
	points    string
	mapID     int
	npcsDir   string
	corpus    string
	noScripts bool
	prewarm   bool
	// noRefresh is the -norefresh flag: skip the startup translate of the .scp
	// sources into the corpus above, so the zone indexes whatever is on disk. The
	// refresh is fail-soft and off-by-nothing (it runs by default), so this is only
	// for a run that must not start a second process - and for -no-scripts, where
	// the corpus is never read at all.
	noRefresh bool
	dict      string
	idMap     string
	textImage string
	items     string
	// prices, fesoPrices, shops and customShops are the BUY/SELL data: what an item
	// costs in vis and in Feso, which items a normal shop group holds (in the
	// client's order), and the custom shop entries. All four come from
	// src\emit_shop_data.py.
	prices      string
	fesoPrices  string
	shops       string
	customShops string
	// equipSlots is which slot(s) each item class may be worn in, from
	// data\item_equip.json (src\emit_item_equip.py). Without it an equip is
	// REFUSED: the client's requested spot cannot be checked against the class.
	equipSlots string
	// jobStats is the class base stats: job id -> the six stats the client's
	// basic-stat block carries, data\job_stats.json (413 classes, copied from the
	// client's own datatable_job.ies). Missing is NOT fatal: every stat is then
	// sent as 0 - exactly the state Character Info was in before this table - and
	// the startup log names the file.
	jobStats string
	// itemStats is the item columns the status rules read: class id -> ATK, DEF,
	// AR, WLv, ShldDR, HR, ASPD, the resistances, ClassType. data\item_stats.json,
	// copied from the client's own datatable_item_*.xml. Missing is NOT fatal:
	// every item then answers zeros and the DERIVED half of Character Info is
	// sent as 0 - exactly the state before this table - and the log names the file.
	itemStats string
	// pathtool is the path-engine COMMAND as server.json names it (win: the exe
	// itself, Linux: the program that runs it - `wine`), and pathtoolArgs are
	// the args it needs before the mesh. main.go splits them from
	// config.Engine so a platform change is a file edit, not a GOOS branch.
	// Empty launcher = run the tool directly, which is the pre-config shape.
	pathtool         string
	pathtoolLauncher string
	pathtoolArgs     []string
	meshdir          string
	navdir           string
	// presence is the folder this zone announces itself in: one file per zone,
	// world.ZonePath(c.presence, port). The barrack's -zones flag reads the same
	// folder, which is how a map other than 10 becomes findable at all.
	presence string
	// zones is the zone registry the GATE path reads: a map change names the ip and
	// port of the zone serving the destination, and refusing to name one is the
	// difference between "that gate does not work" and a client stranded in a
	// handshake. It is also the folder the handoff files live in.
	zones string
	// npcHide is the hand-written list of town NPCs NOT to spawn
	// (config\zone_npc_hide.txt). Missing or empty hides nothing.
	npcHide string
	// advertise is the ip the CLIENT is told to dial, from the -advertise flag:
	// a test or a one-off run that beats the address book. Empty means "read the
	// machine row" (machine), which is what a normal start does.
	advertise string
	// machine is which row of the address book names this machine's address
	// (table machine, migrations\00013_machine.sql). Empty means the default row.
	machine string
	// rawLog dumps every decrypted client chunk and the frames cut from it, for
	// the case a known packet seems never to arrive: the splitter drops what it
	// cannot size, silently. Diagnosis only; a busy town would flood the log.
	rawLog bool
	// diagStopOffset moves the x of every ZC_STOP_PC this zone broadcasts by this
	// many units, and diagHex dumps the bytes of every announced character's spawn
	// and stance. Both are DIAGNOSTIC switches for one probe, both off in every
	// normal run; see Handler.DiagStopOffset / Handler.DiagHex for what an answer
	// means. The barrack never sees them.
	diagStopOffset int
	diagHex        bool
	// diagEvents writes one log line per echoed move, per stop and per walk leg
	// again (`-diag-events`). OFF is the normal value: those three sit on the
	// per-packet path, and at 50 bots they are ~100 lines/s and at 300 players in
	// one town ~1000. The METRICS line counts the same three events (moves=,
	// stops=, walks=), so OFF loses the line and not the information.
	// See Handler.DiagEvents.
	diagEvents bool

	// quietSpawn is the `-quietspawn` DEVELOPMENT FLAG (default OFF): the startup
	// spawner counts its two per-body lines into one line per tactics decision instead
	// of writing 858 of them. It changes NO packet and NO state - the bodies are placed
	// exactly as before - so it is announced as a LOG switch, not as a broken run.
	// See Handler.QuietSpawn and zone\spawn_quiet.go.
	quietSpawn bool

	// spawnMonster is the S1 development flag (-spawnmonster, default OFF): one
	// ZC_ENTER_STANDINGMONSTER at a fixed spot on the first GAME_READY of a
	// session. It is NOT a diagnostic - nothing is moved or faked - but it changes
	// the wire, so it is announced at startup the way the diag switches are, and it
	// is off in every normal run. See zone\monster.go.
	spawnMonster bool

	// spawnSplashPair is the -spawnsplashpair development flag (default OFF): after
	// the map's own startup tactics have run, create two Scarecrows 80 units apart
	// so one area skill can be tested against two bodies in one cast. Like
	// spawnMonster it changes the world rather than the wire alone, it is announced
	// at startup, and it is off in every normal run. See spawnSplashPair below.
	spawnSplashPair bool

	// spawnDummy is the `-spawndummy` development flag (default "" = OFF): a
	// comma-separated list of monster class ids. After the map's own startup tactics
	// have run, one real Monster row of each class is created at a fixed walkable
	// cell, and the DEF the game's own SCR_Get_MON_DEF computes for it is logged.
	// It is R84 slice 2's probe: the damage rule can only be checked against a target
	// whose DEF is not 0, and the map's own dummies are all Scarecrows. Off, nothing
	// is created. See spawnDummies below.
	spawnDummy string

	// dummyCell is the `-dummycell` development flag (default "" = the four cells
	// measured on map 1331): the spots -spawndummy puts its monsters on, as x,y
	// pairs. Only read when spawnDummy is set. The default strip is off the mesh
	// on every map but 1331 - a spawn there is refused by SpawnMonsterObject's own
	// meshCell - so a probe on a map that allows drops (1301, 208, 6004) must give
	// its own ground. See parseDummyCells.
	dummyCell string

	// strictRange is R84's development flag (-strictrange, default OFF): refuse a
	// cast whose target or point is outside the skill's own MinR..MaxR. Off, the
	// distance and the range are still logged and the cast is applied, because the
	// reference's own rule for an out-of-range press has not been measured.
	// See zone\skill_cast.go and zone\skill_area.go.
	strictRange bool

	// sightOff is the `-sightoff` development flag (default OFF): the sight ring is
	// inert for the whole run, so every session is told about everybody, exactly as
	// before R135. It exists to measure where the CLIENT stops drawing an actor -
	// our own ring must not remove it first. Off, nothing about the ring changes.
	// See zone\sight.go and Handler.SightOff.
	sightOff bool

	// diagDistance is the `-diag-distance` measurement switch (default OFF): one log
	// line per pair of sessions per second, and a distance suffix on every stop the
	// zone sends. Log only; no packet changes. See zone\diag_distance.go and
	// Handler.DiagDistance.
	diagDistance bool

	// spLock is the `-splock` development flag (default OFF): a skill's own SpendSP is
	// cut and then put back, so the caster's bar stays full for a whole test session.
	// Off, the wire is exactly today's. It exists because nothing in this server
	// regenerates SP, so a formula test would stop on the CLIENT's side after a few
	// casts of a 300-750 SP skill. No game rule reads it.
	// See zone\skill_area.go and Handler.SPLock.
	spLock bool

	// simLatencyMs is the `-simlatency <ms>` development flag (default 0 = OFF):
	// how long every outbound packet is held back, so a run on 127.0.0.1 costs
	// something like a real network's latency. It is a delay INSIDE each session's
	// one writer goroutine (zone\writer.go), never a queue of its own. Zero is the
	// normal value, and the writer then writes as soon as it has something - the path
	// every real run takes. No game rule reads it.
	simLatencyMs int

	// respawnDelay is the `-respawn <duration>` development flag (default 0 = the
	// reference's 23.568 s): how long after a kill a body this zone created comes
	// back. It replaces zone.RespawnDelay for EVERY body in the run, so a probe of
	// the death or of the return does not wait 23.6 s per kill; 0 (or a value that is
	// not positive) leaves the reference's number in place and the run is what it
	// always was. The flag is applied where the other dev flags are announced, by
	// zone.SetRespawnDelay, and that line says RESPAWN OVERRIDE.
	// See zone/respawn.go.
	respawnDelay time.Duration

	// monai is the `-monai go|script` DEV SWITCH (default go): which engine answers a
	// monster's decisions. "go" is today's chase and swing, byte for byte; "script"
	// runs the game's own tactics script per body (zone\monster_ai.go). The flag is
	// validated in runZone - an unknown value is refused with a line naming the two,
	// not silently treated as the default - and announced by the MONSTER AI line.
	monai string

	// metricsEvery is the `-metrics <interval>` flag (default 10s, 0 = OFF): how
	// often the zone logs ONE line of its own counters - packets and bytes each
	// way, broadcasts and their recipients, handler microseconds. Nothing is sent
	// to a client and no game rule reads it. See zone\metrics.go.
	metricsEvery time.Duration
}

// clickTimeout bounds one click script. The corpus has 662 files containing
// `while(1)` and every loop calls _vtick() for the budget, but a script that
// blocks another way would still freeze the zone - so it also has a deadline.
const clickTimeout = 3 * time.Second

// clickEngine builds the click side of the zone: one Lua runtime for the WHOLE
// server, its corpus INDEXED at startup (names only - a file is parsed when a
// click first needs it, which is what keeps a zone at 23 MB instead of 828 MB).
//
// The mutex is not decoration: the zone serves each connection in its own
// goroutine and a Lua state is not safe for concurrent use. One runtime also
// means one definition of each name, which is what the game itself had.
//
// It returns nil when scripts are switched off: the zone then logs every click as
// dropped instead of inventing a line.
//
// The second return value resumes a script that yielded at ShowSelDlg. It is nil
// when scripts are off.
//
// The third return value releases a click left suspended when a connection ends,
// so a player who disconnects mid-dialog does not lock every NPC on the server -
// the suspension is ONE slot for the whole runtime, not one per player.
func clickEngine(c zoneConfig, book *text.Book, store *zone.PGStore, catalog *item.Catalog, spawner clickWorld) (func(string, zone.ClickObject, zone.ClickObject) (zone.ClickReply, error), func(int) (zone.ClickReply, bool, error), func(), func(string, zone.ClickObject) (bool, error), func(time.Time), func() bool, *script.Host) {
	if c.noScripts {
		log.Printf("zone: scripts OFF (-no-scripts) - clicks are logged and dropped")
		return nil, nil, nil, nil, nil, nil, nil
	}

	host := script.New()
	host.Text = book
	// The transaction layer's two dependencies: where a `TxEnd` writes, and where
	// a refused transaction is reported. Without the store a transaction still
	// FAILS honestly (TxEnd answers 0); it does not pretend the write landed.
	host.Tx = store
	host.Log = log.Printf
	// The bag and the item table: what the player holds (money is the vis item)
	// and what a script's item NAME means. PGStore satisfies both interfaces
	// structurally, so neither package imports the other.
	host.Items = store
	host.Catalog = catalog
	// The world behind CreateTNPC/CreateTmonster: the Scarecrow lane. NIL is a
	// legitimate test state, and both builtins then REFUSE with a log line instead
	// of creating nothing silently.
	host.Spawner = spawner
	// The SAME world behind the radius scan and SendMsg: "is a Scarecrow already
	// here?" and "take it away". One bridge, two interfaces - PGStore satisfies both
	// Items and Catalog the same way.
	host.Near = spawner
	// THE CORPUS INDEX COMES FROM ITS OWN IMAGE when one exists - one image per table, so editing
	// items never rebuilds the script index (the owner's rule, 2026-11-09). A missing image is the
	// FIRST BOOT: the walk happens then, and the result is written for every start after it.
	var n int
	var err error
	scriptCorpusDir = c.corpus
	imgPath := script.IndexImagePath(c.corpus)
	if idx, ierr := script.OpenIndexImage(imgPath); ierr == nil {
		n = host.IndexFromImage(c.corpus, idx)
		log.Printf("zone: script index from the IMAGE %s (%d names, no corpus walk)", imgPath, n)
	} else {
		n, err = host.Index(c.corpus)
		if err == nil {
			if werr := script.SaveIndexImage(imgPath, host.IndexValue()); werr != nil {
				log.Printf("zone: the script index could not be saved for the next start (%v)", werr)
			} else {
				log.Printf("zone: script index built and saved to %s (the next start reads it)", imgPath)
			}
		}
	}
	if err != nil {
		// Not fatal: a zone with no scripts still lets the player walk the map,
		// and a fatal start would hide that the town NPCs themselves are fine.
		log.Printf("zone: no script corpus at %s (%v) - clicks will be dropped; "+
			"build it with: cd src && python scp_to_lua.py --dump ../out/lua_corpus",
			c.corpus, err)
		return nil, nil, nil, nil, nil, nil, nil
	}
	if err := host.LoadLibraries(); err != nil {
		log.Printf("zone: script libraries did not load (%v) - a click that calls "+
			"them will fail; the error names the file", err)
	}
	log.Printf("zone: %d script name(s) indexed from %s (click AND tactics entries; "+
		"files load on first use)", n, c.corpus)
	memStage("script index")

	// The SAME engine parses what the refresh just translated. A script with a
	// real syntax error is otherwise invisible until somebody clicks that NPC,
	// because indexing only reads text and a file loads on first use.
	compileCheckRefreshedCorpus(host, c.corpus)
	memStage("corpus compile check")

	var mu sync.Mutex
	clickFunc := func(entry string, self, pc zone.ClickObject) (zone.ClickReply, error) {
		mu.Lock()
		defer mu.Unlock()

		// EnsureScript reports whether the file was new, which is the one moment
		// worth a log line: the first click on an NPC pays for its script.
		if loaded, err := host.EnsureScript(entry); err != nil {
			return zone.ClickReply{}, err
		} else if loaded {
			log.Printf("zone: first click on %s - its script file is now loaded", entry)
		}
		out, suspended, err := host.StartClickCoroutine(entry, toScriptObj(self), toScriptObj(pc), clickTimeout)
		return zone.ClickReply{
			DialogKind:  clickKind(out.Dialog.Kind),
			Text:        out.Dialog.Text,
			Prompt:      out.Dialog.Prompt,
			Options:     out.Dialog.Options,
			Fade:        out.Dialog.Fade,
			WindowKind:  out.Window.Kind,
			WindowGroup: out.Window.Group,
			WindowCoin:  coinKind(out.Window.Coin),
			Superseded:  out.Superseded,
			BadText:     out.BadText,
			Properties:  toZoneProps(out.Properties),
			Items:       out.Items,
			Suspended:   suspended,
		}, err
	}

	resumeFunc := func(chosenIndex int) (zone.ClickReply, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		out, suspended, err := host.ResumeClick(chosenIndex, clickTimeout)
		return zone.ClickReply{
			DialogKind:  clickKind(out.Dialog.Kind),
			Text:        out.Dialog.Text,
			Prompt:      out.Dialog.Prompt,
			Options:     out.Dialog.Options,
			Fade:        out.Dialog.Fade,
			WindowKind:  out.Window.Kind,
			WindowGroup: out.Window.Group,
			WindowCoin:  coinKind(out.Window.Coin),
			Superseded:  out.Superseded,
			BadText:     out.BadText,
			Properties:  toZoneProps(out.Properties),
			Items:       out.Items,
			Suspended:   suspended,
		}, suspended, err
	}

	// releaseFunc drops a click left suspended when a connection ends. The
	// mutex is the same one the clicks take: one Lua runtime, one suspended slot.
	releaseFunc := func() {
		mu.Lock()
		defer mu.Unlock()
		if host.CancelSuspended() {
			log.Printf("zone: connection ended - released a suspended click script")
		}
	}

	// tacticsFunc starts one actor's tactics entry (`SCR_<script>_TS_NONE`) as a
	// THREAD - the thread the original starts when a trigger object enters the world
	// (`TriggerObject.cpp:49-57`), which for most entries returns at once and for the
	// arm managers loops on `sleep` for as long as the map lives (script	rigger.go,
	// R141). It takes the SAME mutex a click does, for the same reason: one Lua
	// runtime. It USED to run the entry as a one-shot call with a 3 s deadline, which
	// aborted every looping entry at its full deadline - 24.1 s of map 4063's 33.4 s
	// start, and the block rule never ran at all.
	tacticsFunc := func(entry string, self zone.ClickObject) (bool, error) {
		mu.Lock()
		defer mu.Unlock()

		// An entry the corpus does not define is the NORMAL case - most NPCs have
		// no tactics script - so it answers found=false without a line. Without this
		// every script-less NPC on the map reads as a failure in the log.
		if !host.HasScript(entry) {
			return false, nil
		}
		if loaded, err := host.EnsureScript(entry); err != nil {
			return true, err
		} else if loaded {
			log.Printf("zone: %s - its script file is now loaded", entry)
		}
		return host.StartTacticsThread(entry, toScriptObj(self))
	}

	// tacticsTick wakes every parked trigger thread whose `sleep` has expired, on the
	// zone's own clock (zone\chase.go tickIfDue). Armed is the guard that keeps that
	// clock awake while a thread is alive, and it reads the host's thread map under
	// the same mutex.
	tacticsTick := func(now time.Time) {
		mu.Lock()
		defer mu.Unlock()
		host.TickTriggers(now)
	}
	tacticsArmed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return host.TriggerThreadsArmed()
	}

	// ONE LINE PER THREAD LIFE, start and end with the reason - the same shape the
	// monster runner writes, so a log reader can follow an object's script from the
	// first wake to the map's teardown.
	host.TriggerLog = func(handle uint32, entry, what, why string) {
		switch what {
		case "start":
			log.Printf("zone: TACTIC THREAD 0x%08x %s start", handle, entry)
		case "failed":
			log.Printf("zone: TACTIC THREAD 0x%08x %s FAILED and is not retried: %s",
				handle, entry, why)
		default:
			log.Printf("zone: TACTIC THREAD 0x%08x %s end (%s)", handle, entry, why)
		}
	}

	return clickFunc, resumeFunc, releaseFunc, tacticsFunc, tacticsTick, tacticsArmed, host
}

// worldSpawner is the ONE bridge between the script host's `CreateTNPC` /
// `CreateTmonster` / `GetNearObjectCount` / `SendMsg` and this zone.
//
// It exists because the dependency must stay one-way (script -> nothing, zone ->
// nothing, main -> both), the same reason Handler.Click is a function field. `h` is
// set right after the Handler is built and before the listener opens.
type worldSpawner struct {
	h *zone.Handler
}

// clickWorld is the whole world a click script may touch: create an actor, look
// around it, and send it a message. It is one interface at the call site because one
// object (this bridge) answers all of it - the script host still takes it as two, so
// a test can wire the creation half without the scan half.
type clickWorld interface {
	script.Spawner
	script.NearWorld
}

// CreateTNPC is script.Spawner's first half: the parsed call, in the zone's terms.
func (w *worldSpawner) CreateTNPC(s script.TNPCSpawn) (uint32, error) {
	if w.h == nil {
		return 0, errNoWorldYet
	}
	return w.h.SpawnTNPCObject(zone.TNPCSpec{
		Name: s.Name, TypeName: s.TypeName,
		X: s.X, Y: s.Y, Z: s.Z,
		Direction: s.Direction, Occupation: s.Occupation, Possession: s.Possession,
	})
}

// CreateTmonster is script.Spawner's second half.
func (w *worldSpawner) CreateTmonster(s script.MonsterSpawn) (uint32, error) {
	if w.h == nil {
		return 0, errNoWorldYet
	}
	return w.h.SpawnMonsterObject(zone.MonsterSpec{
		MonsterID: s.MonsterID,
		X:         s.X, Y: s.Y, Z: s.Z,
		Count: s.Count, Direction: s.Direction,
		Level: s.Level, MHP: s.MHP, Tactics: s.Tactics,
	})
}

// errNoWorldYet is a real programming state, not a player-facing one: a script ran
// before the zone it writes into existed. It is refused, not guessed at.
var errNoWorldYet = fmt.Errorf("the zone handler is not built yet, so there is no " +
	"world to create an object in")

// NearObjects is script.NearWorld's first half: the radius scan, in the zone's terms.
//
// The conversion is per FIELD and not a cast, so a field added to one side is a
// visible decision rather than a silent zero.
func (w *worldSpawner) NearObjects(x, y int32, radius float64, kind string) ([]script.NearObject, error) {
	if w.h == nil {
		return nil, errNoWorldYet
	}
	found, err := w.h.NearObjects(x, y, radius, kind)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	out := make([]script.NearObject, len(found))
	for i, o := range found {
		out[i] = script.NearObject{Handle: o.Handle, ClassID: o.ClassID, X: o.X, Y: o.Y}
	}
	return out, nil
}

// KillMonster is script.NearWorld's second half: `OM_KILL` on a monster.
func (w *worldSpawner) KillMonster(handle uint32) error {
	if w.h == nil {
		return errNoWorldYet
	}
	return w.h.KillMonsterObject(handle)
}

// toScriptObj is the one place the zone's actor and the script engine's actor are
// said to be the same thing. Kept explicit so that adding a field to one of them
// is a visible decision, not a silent difference.
func toScriptObj(o zone.ClickObject) script.Obj {
	return script.Obj{
		Handle: o.Handle, Name: o.Name, Script: o.Script, Level: o.Level,
		Commander: o.Commander,
		X:         o.X, Y: o.Y, Z: o.Z, HP: o.HP,
	}
}

// toZoneProps narrows a script transaction's property values to the 4 bytes the
// client's own cache holds. A value outside that range cannot be shown, so it is
// clamped rather than wrapped: a counter reading 0 because it overflowed is a
// worse lie than one that stopped at the ceiling.
func toZoneProps(in []script.PropValue) []zone.Property {
	if len(in) == 0 {
		return nil
	}
	out := make([]zone.Property, 0, len(in))
	for _, p := range in {
		value := p.Value
		if value < 0 {
			value = 0
		} else if value > math.MaxUint32 {
			value = math.MaxUint32
		}
		out = append(out, zone.Property{Name: p.Name, Value: uint32(value)})
	}
	return out
}

// clickKind maps the script engine's dialog kinds onto the zone's. They are the
// same three strings today; the function exists so they cannot drift apart by
// accident.
func clickKind(kind string) string {
	switch kind {
	case script.DialogOK:
		return zone.ClickDialogOK
	case script.DialogSelect:
		return zone.ClickDialogSelect
	default:
		return zone.ClickDialogNone
	}
}

// coinKind maps a script's coin name onto the zone's wire coin type.
//
// Anything that is not the Feso name is VIS. That is what every shop did before
// Feso existed, and defaulting the other way would take Feso out of the player's
// bag for a shop whose script said nothing.
func coinKind(coin string) zone.Coin {
	if coin == script.CoinFeso {
		return zone.CoinFeso
	}
	return zone.CoinVis
}

// loadItemCatalog reads the emitted item table. A missing file is NOT fatal: the
// zone still runs, every item count answers 0, and the log line names the tool
// that writes the file - the same rule as the text book and the script corpus.
func loadItemCatalog(path string) *item.Catalog {
	catalog, err := item.Load(path)
	if err != nil {
		log.Printf("zone: item counts will answer 0 for every name (%v); build it "+
			"with: cd src && python emit_item_catalog.py", err)
		return nil
	}
	log.Printf("zone: %d item name(s) loaded for script item counts", catalog.Len())
	memStage("item names")
	return catalog
}

// loadPrices reads the emitted price table. Missing is NOT fatal: a shop window
// still opens, and a buy then has NO price - which a buy handler must treat as a
// refusal, not as "free" (see zone.Handler.Prices).
// loadJobStats reads the class base stats: job id -> the six stats the client's
// basic-stat block carries, data\job_stats.json.
//
// Missing is NOT fatal, and neither is unreadable - the same rule the Python
// server applies with its `except`: every job then answers six zeros, which is
// the state Character Info was in before this table existed, and the log line
// names the file. A zone that cannot describe a character must not refuse to
// serve it.
func loadJobStats(path string) *zone.JobStats {
	stats, err := zone.LoadJobStats(path)
	if err != nil {
		log.Printf("zone: no class base stats, so Character Info shows 0 for all six "+
			"(%v); the file is data\\job_stats.json, copied from the client's own "+
			"datatable_job.ies", err)
		return nil
	}
	log.Printf("zone: %d class(es) of base stats loaded for the Character Info sheet",
		stats.Len())
	return stats
}

// loadItemStats reads the emitted item columns (data\item_stats.json). A missing
// file is a WARNING, not a refusal: the zone still starts, every item answers
// zeros, and the DERIVED half of the Character Info sheet is sent as 0 - the
// same state it was in before ZC_UPDATE_ALL_STATUS existed. The log names the
// file and the command that writes it.
func loadItemStats(path string) *zone.ItemStats {
	stats, err := zone.LoadItemStats(path)
	if err != nil {
		log.Printf("zone: no item stat columns, so ATK/DEF/A.R./D.R./HR/CSPD and the "+
			"resistances are sent as 0 (%v); the file is data\\item_stats.json, "+
			"written by: cd src && python emit_item_stats.py", err)
		return nil
	}
	log.Printf("zone: %d item class(es) of stat columns loaded for the Character Info "+
		"sheet, %d of them growing items that take their numbers from the owner's "+
		"level", stats.Len(), stats.GrowingLen())
	memStage("item stats")
	return stats
}

func loadPrices(path string) *item.Prices {
	prices, err := item.LoadPrices(path)
	if err != nil {
		log.Printf("zone: BUY/SELL have no prices, so a purchase cannot be "+
			"charged (%v); build them with: cd src && python emit_shop_data.py", err)
		return nil
	}
	log.Printf("zone: %d item price(s) loaded for shop buy/sell", prices.Len())
	return prices
}

// loadFesoPrices reads the emitted FESO price table. It is a second table, not a
// column of the first: a Feso shop charges class 30001 at THESE numbers, and a
// class missing here is simply not sold for Feso.
//
// Missing is NOT fatal, the same as the vis table: a Feso window still opens (the
// client draws it from the group name alone), and a Feso buy is then REFUSED - a
// buy handler must never read "no Feso price" as free (see zone.Handler.FesoPrices).
func loadFesoPrices(path string) *item.FesoPrices {
	prices, err := item.LoadFesoPrices(path)
	if err != nil {
		log.Printf("zone: FESO prices are missing, so a Feso shop's purchase cannot "+
			"be charged (%v); build them with: cd src && python emit_shop_data.py", err)
		return nil
	}
	log.Printf("zone: %d item Feso price(s) loaded for Feso shops", prices.Len())
	return prices
}

// loadEquipSlots reads the emitted class id -> wearable slot table.
//
// It is what turns the client's requested equip slot into a CHECK: the class must
// really be wearable there (item.EquipSlots.Allows). Missing is NOT fatal - the
// zone still runs - but every equip is then REFUSED, because a missing table must
// never read as "this class fits anywhere". The log line names the emitter.
func loadEquipSlots(path string) *item.EquipSlots {
	slots, err := item.LoadEquipSlots(path)
	if err != nil {
		log.Printf("zone: no wearable-slot table, so EVERY equip will be REFUSED "+
			"(%v); build it with: cd src && python emit_item_equip.py", err)
		return nil
	}
	log.Printf("zone: %d wearable item class(es) loaded for equip/unequip", slots.Len())
	return slots
}

// loadShopGroups reads what each normal shop GROUP sells, in the client's order.
//
// A buy record names its item by INDEX into that list, so a missing file means a
// buy cannot be resolved at all - the window still opens, because the client
// draws it from the group name alone.
func loadShopGroups(path string) zone.ShopGroups {
	groups, err := zone.LoadShopGroups(path)
	if err != nil {
		log.Printf("zone: no normal shop groups, so a shop buy cannot name its "+
			"item (%v); build them with: cd src && python emit_shop_data.py", err)
		return nil
	}
	log.Printf("zone: %d normal shop group(s) loaded", groups.Len())
	return groups
}

// loadCustomShops reads the CUSTOM shop entries: what each barter group offers
// and what it charges.
//
// The table is KEPT, not just parsed: unlike a normal shop, a custom window
// carries its own rows, so without this the window cannot be drawn at all and a
// barter buy has no price to charge. A missing file is not fatal - the zone then
// answers a custom window request with ZC_DIALOG_CLOSE only - but it says so
// here, once, instead of on the first click that needs it.
func loadCustomShops(path string) zone.CustomShops {
	groups, err := zone.LoadCustomShops(path)
	if err != nil {
		log.Printf("zone: no custom shop data, so a custom (barter) trade window "+
			"cannot be drawn (%v); build it with: cd src && python emit_shop_data.py", err)
		return nil
	}
	log.Printf("zone: %d custom shop group(s) loaded for barter windows", groups.Len())
	return groups
}

// textBook loads the client's word tables. A missing file is NOT fatal: the zone
// still answers clicks, with the raw locale token in the box, which is visibly
// wrong and points straight at the missing file. It says so once, here.
func textBook(dict, idMap, image string) *text.Book {
	// THE SHARED IMAGE FIRST (2026-11-09). One mapped file, shared by every zone, costs the
	// machine ~50 MB in total instead of 73 MB of private heap per process. A missing or
	// broken image is NOT an error: the tables load exactly as before.
	if image != "" {
		// REBUILD THE IMAGE WHEN ITS SOURCES CHANGED (2026-11-09): the same "refresh the
		// generated data at startup" rule the Lua corpus already follows, so nobody has to
		// remember a second command after regenerating the dictionary or the id map.
		if stale, why := text.ImageStale(image, idMap, dict, text.DefaultServerPack); stale {
			if size, serr := text.SaveImage(image, idMap, dict, text.DefaultServerPack); serr != nil {
				log.Printf("zone: the text image %s needs rebuilding (%s) but the build "+
					"failed (%v) - the tables load instead", image, why, serr)
			} else {
				log.Printf("zone: rebuilt the SHARED text image %s (%.1f MB) - %s",
					image, float64(size)/(1<<20), why)
			}
		}
		if book, err := text.LoadImage(image); err == nil {
			log.Printf("zone: dialog text from the SHARED image %s - %d entries mapped "+
				"read-only, one copy for every zone", image, book.Dict.Len())
			return book
		} else {
			log.Printf("zone: text image %s not usable (%v) - loading the tables instead",
				image, err)
		}
	}
	book, err := text.Load(idMap, dict)
	if err != nil {
		log.Printf("zone: dialog text will NOT be translated (%v); a box will read "+
			"`<$>1234</>` until -dict and -idmap point at real files", err)
		return nil
	}
	log.Printf("zone: %d dictionary entries + %d id-map rows loaded for dialog text",
		book.Dict.Len(), book.IDMap.Len())
	memStage("dialog text")
	return book
}

// pathEngine wires the game's own path engine, when it is there.
//
// The helper is a 32-bit Windows binary around `pathengine.dll`, so a zone on the
// Linux VPS cannot have one. WITHOUT it the chase still works on open ground: one
// straight leg to the NPC, capped at MaxChase. WITH it the walk follows the mesh and
// a destination that has no route is REFUSED instead of animating nothing.
//
// The engine itself is returned as well, because the Last Point needs TWO more answers
// from the same mesh (R73): the HEIGHT of a position, to store it, and the POSITION
// nearest a 3D point, to turn that height back into a cell on entry. Building it twice
// would load the same mesh twice and print two startup lines.
func pathEngine(c zoneConfig, meshName string) *zone.PathEngine {
	if meshName == "" {
		log.Printf("zone: the NPC data names no walk-mesh - the chase will walk straight "+
			"lines (rebuild it with: cd src && python emit_zone_npcs.py %d)", c.mapID)
		return nil
	}
	engine := &zone.PathEngine{
		Tool:     c.pathtool,
		Launcher: c.pathtoolLauncher,
		Args:     c.pathtoolArgs,
		Mesh:     filepath.Join(c.meshdir, meshName+".tok"),
		Radius:   50, // S body: the shape table's smallest half-size
	}
	if !engine.Available() {
		// TWO different gaps, and the next session must not have to guess which: the
		// helper is a file this machine either has or does not, while the MESH is per
		// map and comes from the client's `ge\bg\` tree (R74). Saying "no path engine
		// (tool on mesh)" for both sent a reader looking at the wrong file.
		missing := ""
		if _, err := os.Stat(engine.Tool); err != nil {
			missing = fmt.Sprintf("the tool %s is not there", engine.Tool)
		} else {
			missing = fmt.Sprintf("the tool %s is here but the map's walk-mesh %s is missing - "+
				"copy it from the client's ge\\bg\\%s.tok into %s",
				engine.Tool, engine.Mesh, meshName, c.meshdir)
		}
		log.Printf("zone: no path engine (%s) - every server-planned walk is REFUSED and "+
			"nothing is sent (a player's own leg is drawn on the chord, counted and named); "+
			"the LAST POINT will not be saved (its height needs the mesh)", missing)
		return nil
	}
	log.Printf("zone: path engine: %s on %s (routes are checked before every walk, and "+
		"the Last Point's height comes from it)", engine.Tool, engine.Mesh)
	return engine
}

// navHeight and navNear adapt the engine to the two Handler hooks, and answer nil when
// there is no engine: a nil hook is how the handler says "no mesh here", and it then
// refuses the Last Point with a log line instead of writing a cell into a height column.
func navHeight(e *zone.PathEngine) func(pos [3]int32) (int32, bool) {
	if e == nil {
		return nil
	}
	return func(pos [3]int32) (int32, bool) {
		h, ok, err := e.Height(pos)
		if err != nil {
			log.Printf("zone: the mesh could not answer the height of (%d, %d, cell %d): %v",
				pos[0], pos[1], pos[2], err)
			return 0, false
		}
		return h, ok
	}
}

func navNear(e *zone.PathEngine) func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
	if e == nil {
		return nil
	}
	return func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		at, height, ok, err := e.Near(pt, hRange, vRange)
		if err != nil {
			log.Printf("zone: the mesh could not resolve (%d, %d, z %d): %v",
				pt[0], pt[1], pt[2], err)
			return [3]int32{}, 0, false
		}
		return at, height, ok
	}
}

// walkGraph loads the map's pre-computed walk data, when it has been built.
//
// This is what lets a machine with no `pathtool.exe` (the Linux target) answer the same
// two questions the helper answers: where to stand, and how far the walk is. A missing
// file is not fatal - the chase then walks a straight line and says so.
func walkGraph(c zoneConfig) *zone.WalkGraph {
	path := filepath.Join(c.navdir, fmt.Sprintf("map%d.json", c.mapID))
	graph, err := zone.LoadWalkGraph(path)
	if err != nil {
		log.Printf("zone: no walk graph at %s (%v) - without it, and without a path "+
			"engine, walks are straight lines", path, err)
		return nil
	}
	log.Printf("zone: walk graph: %d node(s), %d spot(s) from %s", len(graph.Nodes),
		graph.NPCCount(), path)
	return graph
}

// graphPlan answers the chase's question from the walk graph alone: a stand-off on the
// side the PLAYER is approaching from, and the route length over the graph.
//
// `from` used to be ignored here, because a spot was one point fixed at build time; that
// put the character on the NPC's far side (R58). zone.WalkGraph.Stand uses it now.
func graphPlan(g *zone.WalkGraph) func([3]int32, [3]int32, float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
	return func(from, npc [3]int32, _ float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		stand := g.Stand(npc, from)
		if !stand.Found {
			// No data for this NPC: the honest answer is its own position.
			return npc, nil, 0, true, true, nil
		}
		return stand.Pos, nil, stand.Route, stand.Overlap, stand.Routable, nil
	}
}

// loadNPCHide reads the town NPC hide list (config\zone_npc_hide.txt).
//
// Everything about it is non-fatal: a missing file hides nothing, a malformed line is
// one warning that names the line, and the server starts either way. What must NOT
// happen silently is nothing being hidden when the operator expected otherwise, so the
// rule count is always logged.
func loadNPCHide(path string) *zone.NPCHide {
	hide, err := zone.LoadNPCHide(path)
	if err != nil {
		log.Printf("zone: NPC hide list %s could not be read (%v) - every town NPC "+
			"will be spawned", path, err)
		return nil
	}
	for _, bad := range hide.Bad {
		log.Printf("zone: NPC hide list %s %s (that line hides nothing)", path, bad)
	}
	log.Printf("zone: %d NPC hide rule(s) from %s", hide.Rules(), path)
	return hide
}

// loadMapRows reads the map's own rows out of the shared spawn table: its quick-move
// destinations (one per panel button) and its gates out (walking into one starts a map
// change).
//
// A missing file is NOT fatal: the zone serves the map, the panel's buttons are refused
// one at a time, and there is simply no gate to walk into. Empty points are the normal
// state for a field map, so that is a log line, not a complaint - only a file that will
// not load says anything louder.
func loadMapRows(path string, mapID int) ([]zone.TownPoint, []zone.MapGate, *zone.ZonePoints) {
	points, err := zone.LoadZonePoints(path)
	if err != nil {
		log.Printf("zone: town quick-move (Alt+F3) and gate travel have no data (%v); "+
			"build the table with: cd src && python emit_zone_points.py", err)
		return nil, nil, nil
	}
	town := points.TownPoints(mapID)
	if len(town) == 0 {
		log.Printf("zone: map %d has no town points - the quick-move panel is refused", mapID)
	} else {
		log.Printf("zone: %d town quick-move point(s) for map %d (button 1 = %d, %d, cell %s)",
			len(town), mapID, town[0].X, town[0].Y, cellOf(town[0]))
	}
	gates := points.Gates(mapID)
	if len(gates) == 0 {
		log.Printf("zone: map %d has no gates out - nothing to walk into", mapID)
		return town, nil, points
	}
	// One line per gate that CANNOT be used, never one per gate that can: a map with
	// twenty exits must not print twenty lines at every start.
	usable := 0
	for _, g := range gates {
		switch {
		case g.ToID <= 0 || g.To == "":
			log.Printf("zone: gate %s names no destination (to %q, to_id %d) - it is "+
				"refused when a player stands in it", g.Name, g.To, g.ToID)
		case g.TargetCell == nil:
			log.Printf("zone: gate %s -> map %d (%s) has NO arrival cell - it is "+
				"refused when a player stands in it", g.Name, g.ToID, g.To)
		default:
			usable++
		}
	}
	log.Printf("zone: %d gate(s) out of map %d, %d of them usable "+
		"(walk within a gate's own range to leave)", len(gates), mapID, usable)
	return town, gates, points
}

// cellOf is the printable cell of a town point, for a log line. The file stores null
// when the emitter found no ground, and printing that as a number would be a lie.
func cellOf(p zone.TownPoint) string {
	if p.Cell == nil {
		return "null"
	}
	return strconv.Itoa(int(*p.Cell))
}

// mapNames is the barrack's map table: map id -> CLASS name, from the shared spawn
// table (data\zone_points.json, written by src\emit_zone_points.py).
//
// A missing or broken file is NOT fatal here. The barrack still answers with the
// built-in table and then reb_basic, which is exactly what it did before this table
// existed: a missing name must never stop a login.
func mapNames(path string) func(mapID uint16) (string, bool) {
	points, err := zone.LoadZonePoints(path)
	if err != nil {
		log.Printf("barrack: map names fall back to the built-in table (%v); build the "+
			"table with: cd src && python emit_zone_points.py", err)
		return nil
	}
	return func(mapID uint16) (string, bool) { return points.Name(int(mapID)) }
}

// warpMaps is the barrack's REVERSE of mapNames: a RESTART WARP ID -> the map that
// owns it, from the same shared spawn table.
//
// It exists because the SAVED START POINT is stored the way the oracle stores it - as
// a restart warp id in `commander.point` (213 = Coimbra, 203 = Reboldoeux) - while
// BC_COMMANDER_INFO's newMapID needs the MAP. There is no global warp table in this
// project, so a warp id resolves exactly when some map's `restart` row names it
// (zone\ZonePoints.RestartWarpMap).
//
// A missing or broken file is NOT fatal here, for the same reason as mapNames: the
// barrack then falls back to the last point's map, which is what it did before the
// save point existed. A missing name must never stop a login.
func warpMaps(path string) func(warpID uint16) (uint16, bool) {
	points, err := zone.LoadZonePoints(path)
	if err != nil {
		log.Printf("barrack: saved start points cannot be resolved to a map (%v); build "+
			"the table with: cd src && python emit_zone_points.py", err)
		return nil
	}
	return func(warpID uint16) (uint16, bool) {
		mapID, ok := points.RestartWarpMap(int(warpID))
		if !ok {
			return 0, false
		}
		return uint16(mapID), true
	}
}

// zoneSpawn answers where the team enters the map this zone serves.
//
// An EMPTY -spawn means "ask the shared spawn table for -map". That is the default,
// because the landing point belongs to the map: Reboldoeux's numbers on Coimbra put
// the team through the floor, and the client then dies inside PathEngine.DLL on the
// first walk. A NON-EMPTY -spawn is an override and keeps the old behaviour exactly.
//
// Both failures are FATAL and both name the fix: without a landing point the client
// is never placed at all, and a wrong one is the crash above.
func zoneSpawn(c zoneConfig) (zone.Landing, error) {
	if strings.TrimSpace(c.spawn) != "" {
		pos, err := zone.ParseSpawn(c.spawn)
		if err != nil {
			return zone.Landing{}, fmt.Errorf("bad -spawn %q: %w", c.spawn, err)
		}
		landing := zone.Landing{Pos: pos, MapID: c.mapID, Source: "-spawn"}
		// The map's NAME still comes from the table when it is there: a hand-picked
		// point must not make the map unnameable to the barrack. A table that will
		// not load is not worth a second complaint here - the override is a probe.
		if points, err := zone.LoadZonePoints(c.points); err == nil {
			if name, ok := points.Name(c.mapID); ok {
				landing.Map = name
			}
		}
		return landing, nil
	}

	points, err := zone.LoadZonePoints(c.points)
	if err != nil {
		return zone.Landing{}, fmt.Errorf("read the map spawn points: %w; build them with: "+
			"cd src && python emit_zone_points.py", err)
	}
	return points.SpawnFor(c.mapID)
}

// spawnSplashPair is the -spawnsplashpair probe: TWO Scarecrows 80 units apart, on
// the map's own ground, right after StartupTactics has placed the map's own actors.
// Two bodies and not one because the thing under test is an AREA skill: a single
// monster cannot say whether the second body was reached too. It rides the same
// lane a script's CreateTmonster does (SpawnMonsterObject), so what is drawn here is
// what a script would have drawn - nothing is faked.
//
// A failure is LOGGED AND LET GO, never fatal: this is a probe, and a missing
// Monster row or a point off the mesh must cost two absences, not the whole zone.
// Off, it returns before touching the handler, so a normal run is unchanged.
func spawnSplashPair(h *zone.Handler, on bool, logf func(string, ...any)) {
	if !on {
		return
	}
	spec := zone.MonsterSpec{
		MonsterID: 11600, X: 3000, Y: -1800, Count: 1,
		Direction: 270, Level: 130, MHP: 20000,
		// NO Tactics OVERRIDE: the row decides. Class 11600 (Monster_Hersu01, the
		// Scarecrow) carries Tactics="MON_MISSION_84" - the passive prop this probe
		// wants for a target that never fights back. Naming it here is what made the
		// probe unable to spawn anything aggressive (-spawndummy hit the same wall).
	}
	h1, err1 := h.SpawnMonsterObject(spec)
	if err1 != nil {
		logf("zone: -spawnsplashpair is ON: could not create class %d at (%g,%g): %v",
			spec.MonsterID, spec.X, spec.Y, err1)
	}
	spec.X = 3080
	h2, err2 := h.SpawnMonsterObject(spec)
	if err2 != nil {
		logf("zone: -spawnsplashpair is ON: could not create class %d at (%g,%g): %v",
			spec.MonsterID, spec.X, spec.Y, err2)
	}
	// ONE line, and only when both are there: a handle of 0 in a success line would
	// read as a monster that exists, and the failure lines above already say why.
	if err1 == nil && err2 == nil {
		logf("zone: -spawnsplashpair is ON: spawned class %d at (3000,-1800) handle %d "+
			"and (3080,-1800) handle %d", spec.MonsterID, h1, h2)
	}
}

// dummyCells are the spots -spawndummy puts its monsters on: four points on the SAME
// strip of ground the -spawnsplashpair pair stands on (x 3000/3080 at y -1800), so one
// area skill can reach the old pair and the new dummies together.
//
// They were MEASURED, not guessed: `docker-port\navmesh\pathtool.exe near fld_auch.tok`
// answers ground at x 3160/3240/3320/3400, y -1800, all on cell 55, heights 9/9/9/10.
// A point off the mesh is refused by SpawnMonsterObject's own meshCell, which says why,
// so a wrong spot costs a log line and not a silent actor.
//
// THEY ARE THE DEFAULT, NOT THE ONLY SPOTS: -dummycell replaces them for a probe on
// a map whose ground is somewhere else (1331 blocks monster drops; 1301/208/6004 do
// not). An empty -dummycell means these four, exactly.
var dummyCells = [][2]float64{
	{3160, -1800}, {3240, -1800}, {3320, -1800}, {3400, -1800},
}

// parseDummyCells turns `-dummycell` into the spots spawnDummies cycles.
//
// AN EMPTY FLAG IS TODAY'S STRIP, exactly: the four cells above were measured for
// map 1331 and every run before this flag used them, so an untouched command line
// must keep putting the probe on the same ground. nil is never returned for a
// blank flag - "no cells" would be a probe that silently creates nothing.
//
// A PAIR THAT DOES NOT PARSE REFUSES THE WHOLE LIST, and the caller creates
// nothing. Half a list is the one outcome that must not happen: the first class
// would land on the new map's ground and the second back on 1331's y -1800 strip,
// where the mesh refuses it - one working dummy and one log line that reads like a
// spawn bug. The error NAMES the exact text, because this is fixed from the log
// line alone, without opening the code.
//
// NaN and the infinities are refused with everything else: strconv.ParseFloat
// accepts them, and int32(math.Round(NaN)) is a number the language does not
// define - a coordinate is finite or it is a typo.
func parseDummyCells(s string) ([][2]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return dummyCells, nil
	}
	fields := strings.Split(s, ",")
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("%q has no partner - %d number(s) is not a whole "+
			"number of x,y pairs (a pair is written -8856,4111)", s, len(fields))
	}
	out := make([][2]float64, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		var pair [2]float64
		texts := [2]string{strings.TrimSpace(fields[i]), strings.TrimSpace(fields[i+1])}
		for k, text := range texts {
			v, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("%q is not a coordinate (pair %d of %d is %q,%q)",
					text, i/2+1, len(fields)/2, texts[0], texts[1])
			}
			pair[k] = v
		}
		out = append(out, pair)
	}
	return out, nil
}

// dummyCellFor is the cycle: class i takes spot i modulo the number of spots, so
// a list longer than the cell list reuses them in order (the old single-class run
// takes the first cell, as it always did). ok=false is a refusal, never a guess:
// an empty spot list would be an index out of range on a probe run.
func dummyCellFor(spots [][2]float64, i int) ([2]float64, bool) {
	if len(spots) == 0 || i < 0 {
		return [2]float64{}, false
	}
	return spots[i%len(spots)], true
}

// dummyHintVRange is how far above or below the hint the mesh is asked for ground
// under a -dummycell pair. It is NOT the spawn's own 300 (zone\create.go
// spawnVRange): 300 is the original's limit for a point that ALREADY carries a
// height, and a -dummycell pair carries none. Measured with the game's own path
// engine, `pathtool auch_basic.tok near -8856 4111 0 1000 20000` answers cell 140 at
// height 1385 (map 1301's own spawn point is (-9056,4111), the same cell 140),
// while the same query with 300 answers "no ground" - so the 0 that map 1331 wants
// is 1085 units outside the band on 1301 and every spawn there is refused.
const dummyHintVRange = 20000

// dummyGround resolves ONE -dummycell pair against the map's own mesh and answers
// the position the mesh names plus the HEIGHT it has there, which is the hint
// SpawnMonsterObject needs (see dummyHintVRange for why 0 cannot stand in for it).
//
// ok=false is a refusal and never a guess: it means the mesh found no ground under
// that pair within the original's 1000-unit HORIZONTAL range. The caller then hands
// the pair to SpawnMonsterObject unchanged (z 0), so that ITS refusal line - the
// one that names the point and the rule - is the one the operator reads. One
// refusal path, not two.
func dummyGround(h *zone.Handler, x, y float64) (gx, gy, gz float64, ok bool) {
	if h == nil || h.NavNear == nil {
		return x, y, 0, false
	}
	// The horizontal range is the original's own (zone\create.go spawnHRange,
	// unexported there): it is the number that stops a probe jumping across a wall.
	pt := [3]int32{int32(math.Round(x)), int32(math.Round(y)), 0}
	near, height, found := h.NavNear(pt, 1000, dummyHintVRange)
	if !found {
		return x, y, 0, false
	}
	return float64(near[0]), float64(near[1]), float64(height), true
}

// spawnDummies is the -spawndummy probe: ONE real Monster row per listed class id,
// on the map's own ground, right after StartupTactics has placed the map's own
// actors. It rides the same lane a script's CreateTmonster does
// (SpawnMonsterObject), so what is drawn is what a script would have drawn and
// every defence column comes from the row - nothing is typed in here.
//
// THE CELLS COME FROM -dummycell, and its empty value is the four cells measured
// on map 1331. Those cells are ground on 1331 and nowhere else, which is why the
// flag exists: on map 208 the default strip is refused by the mesh, and the maps
// where a drop can actually appear (1301/208/6004 - 1331's map row has
// MonsterDropBlock="TRUE") were unreachable by this probe. A point near the map's
// own spawn point is enough - SpawnMonsterObject snaps it to the nearest ground
// within 1000 units.
//
// WHY THE LOG NAMES THE DEF. This probe exists because the map's own dummies are all
// Scarecrows (DEF 0), and a damage rule that multiplies by `(100 - defence)/100`
// cannot be checked against a 0: every wrong implementation agrees at 0. So each
// dummy's DEF is read back through the SAME source a cast uses
// (MonsterDefByHandle -> SCR_Get_MON_DEF) and printed beside the row values it
// came from, which lets a reader check the number against the row instead of
// trusting it.
//
// A FAILURE IS LOGGED AND LET GO, never fatal: a bad id or a point off the mesh
// must cost one absence, not the whole zone. The ONE exception is a -dummycell
// that does not parse: that refuses the whole probe before any class is looked at,
// because the cells are shared by every dummy and a guessed one is an actor
// standing off the map. Off (the default), it returns before touching the
// handler, so a normal run is unchanged.
func spawnDummies(h *zone.Handler, list, cellList string, logf func(string, ...any)) {
	list = strings.TrimSpace(list)
	cellList = strings.TrimSpace(cellList)
	if list == "" {
		if cellList != "" {
			logf("zone: -dummycell %q is set but -spawndummy is empty, so NOTHING is "+
				"created: -dummycell only moves the cells -spawndummy uses", cellList)
		}
		return
	}
	spots, cerr := parseDummyCells(cellList)
	if cerr != nil {
		logf("zone: -dummycell %q is not a list of x,y pairs (%v) - the -spawndummy "+
			"probe is REFUSED and NO monster is created", cellList, cerr)
		return
	}
	// WHERE THE CELLS CAME FROM, once, and only when the flag was given: with the
	// default strip this run logs exactly what it logged before the flag existed.
	if cellList != "" {
		logf("zone: -dummycell is set: %d cell(s) parsed from %q, used in order and "+
			"cycled", len(spots), cellList)
	}
	ids := strings.Split(list, ",")
	placed := 0
	for i, field := range ids {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.Atoi(field)
		if err != nil || id <= 0 || id > math.MaxInt32 {
			logf("zone: -spawndummy is ON: %q is not a monster class id - skipped", field)
			continue
		}
		spot, ok := dummyCellFor(spots, i)
		if !ok {
			logf("zone: -spawndummy is ON: class %d has no cell to stand on - skipped", id)
			continue
		}
		spec := zone.MonsterSpec{
			MonsterID: int32(id), X: spot[0], Y: spot[1], Count: 1,
			Direction: 270,
			// NO Tactics OVERRIDE: the ROW decides, which is the whole point of
			// -spawndummy. A hardcoded "MON_MISSION_84" here (a mission prop with
			// AIType="None" Attack="FALSE" SeekRange="0") turned every id into a
			// passive body - including the Bigfoot, whose own row says
			// Tactics="MON_AGGRESSIVE" (AGGRESSIVE, attack, seek 2500).
		}
		// A -dummycell pair carries x,y and NO height, and the height is not
		// optional: meshCell searches only 300 units above and below the hint. So
		// when the flag is set the MAP answers it, and the point the mesh names is
		// the one spawned. NOT done for the default strip on purpose - map 1331's
		// run has to stay the run it was, down to the z it passes.
		if cellList != "" {
			if gx, gy, gz, gok := dummyGround(h, spot[0], spot[1]); gok {
				spec.X, spec.Y, spec.Z = gx, gy, gz
			}
		}
		handle, serr := h.SpawnMonsterObject(spec)
		if serr != nil {
			logf("zone: -spawndummy is ON: could not create class %d at (%g,%g): %v",
				id, spec.X, spec.Y, serr)
			continue
		}
		row, _ := h.Monsters.Row(uint32(id))
		def, defOK, defErr := h.MonsterDefByHandle(handle)
		if defErr != nil || !defOK {
			logf("zone: -spawndummy is ON: class %d (%q) is at (%g,%g) handle %d, but its "+
				"DEF could not be read: %v", id, row.Name, spec.X, spec.Y, handle, defErr)
			continue
		}
		hp, hpKnown := h.MonsterHPByHandle(handle)
		if !hpKnown {
			hp = 0
		}
		// spec.X/spec.Y ARE THE RESOLVED CELL: this is the line that says where the
		// body actually went, so a -dummycell run can be checked against its flag
		// without reading the spawn code.
		//
		// THE HEIGHT IS SAID TOO, but only when it came from the mesh: a spawn that
		// succeeds where z 0 is refused should not look like z 0 worked. On the
		// default strip the suffix is empty and this line is the line it always was.
		zNote := ""
		if spec.Z != 0 {
			zNote = fmt.Sprintf(", ground z %g from the map's mesh", spec.Z)
		}
		logf("zone: -spawndummy is ON: class %d (%q) at (%g,%g) handle %d - Lv %d, "+
			"Arm %g, DEF_BM %g, DEFPER_BM %g, IMP_BM %g, RaceType %q, SizeType %q; "+
			"hp %g (the zone's own ledger, written at spawn), "+
			"SCR_Get_MON_DEF says DEF %g%s",
			id, row.Name, spec.X, spec.Y, handle, row.Level, row.Arm, row.DEFBM,
			row.DEFPERBM, row.IMPBM, row.RaceType, row.SizeType, hp, def, zNote)
		placed++
	}
	where := " beside the map's own dummies"
	if cellList != "" {
		where = fmt.Sprintf(" at the -dummycell spot(s) %q", cellList)
	}
	logf("zone: -spawndummy is ON: %d of %d listed class(es) placed%s",
		placed, len(ids), where)
}

// Where the startup corpus refresh finds its tool. The roles are launched from
// docker-port\out and every data path in this file is written the same way
// (..\data\..., ..\config\...), so docker-port\src - one level up - is where
// prepare.py and its translator live. The script must run with ITS OWN folder as
// the working directory: it imports ge_paths and resolves its inputs from there.
const (
	prepareScript  = `../src/prepare.py`
	prepareDir     = `../src`
	prepareTimeout = 120 * time.Second
)

// scriptCorpusDir is where the .lua corpus lives, set once at startup: the corpus refresh needs it
// to drop an index image that the refresh itself has just made stale.
var scriptCorpusDir string

const ()

// corpusRefresh is the evidence the startup refresh leaves for the compile check
// that runs once the script host exists: WHEN it began and WHETHER the
// translator was actually run. clickEngine needs both, and it runs after
// runZone called refreshScriptCorpus - threading two values through a function
// that already returns five would be worse than one package-level record, and
// startup is a single goroutine, so there is nothing to synchronise.
var corpusRefresh struct {
	started time.Time
	ran     bool
}

// refreshScriptCorpus translates the game's own .scp scripts into the Lua corpus
// the script host indexes - BEFORE that index is built (the index is built once,
// at startup, and there is no live reload).
//
// WHY the server does this itself: the reference server reads the .scp sources
// at startup, so editing a script and restarting the zone is enough. We do not
// read .scp; the Go script host runs Lua, produced from those sources by
// src\scp_to_lua.py (through src\prepare.py, which only re-translates what is
// stale). Without this call a restart would keep serving the OLD translated
// corpus and a script edit would silently do nothing.
//
// It does NOT port the translator into Go on purpose: one translator, one
// behaviour - the one 4,239 files were validated against. A Go re-implementation
// would be a second dialect converter that drifts from it.
//
// It is bounded and FAIL-SOFT: a missing python, a missing tool, a non-zero exit
// or a run longer than prepareTimeout is ONE warning line and the zone continues
// with the corpus already on disk. A stale script is a smaller fault than a zone
// that refuses to start.
func refreshScriptCorpus(corpusDir string) {
	// The image path is resolved here, before the local `script` below shadows the package name.
	indexImage := script.IndexImagePath(corpusDir)
	// Recorded BEFORE the child process starts: the .lua files prepare.py writes
	// carry an mtime at or after this instant, and that set is what the compile
	// check reads (see compileCheckRefreshedCorpus).
	corpusRefresh.started = time.Now()
	corpusRefresh.ran = false

	python, err := exec.LookPath("python")
	if err != nil {
		log.Printf("zone: WARNING: script corpus NOT refreshed - python not found (%v). "+
			"Starting with the corpus already on disk. Refresh it by hand with: "+
			"cd src && python prepare.py", err)
		return
	}
	// Absolute paths, so the child's own working directory cannot change what
	// gets run. A failure here is not worth a warning: the relative pair below is
	// exactly how every other path in this file is written.
	script, scriptDir := prepareScript, prepareDir
	if abs, aerr := filepath.Abs(prepareScript); aerr == nil {
		script, scriptDir = abs, filepath.Dir(abs)
	}

	// The deadline is the whole point: a build tool must never hold startup
	// hostage. CommandContext kills the child when it fires.
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, script)
	cmd.Dir = scriptDir
	// From here on the translator is (or was) RUNNING: even a timeout or a
	// non-zero exit can leave rewritten .lua files behind, so those get checked
	// too. `ran` stays false only when nothing was ever launched.
	corpusRefresh.ran = true
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("zone: WARNING: script corpus NOT refreshed - `python prepare.py` did not "+
			"finish in %s and was killed. Starting with the corpus already on disk. Refresh "+
			"it by hand with: cd src && python prepare.py", prepareTimeout)
		return
	}
	if err != nil {
		log.Printf("zone: WARNING: script corpus NOT refreshed - `python prepare.py` failed "+
			"(%v): %s. Starting with the corpus already on disk. Refresh it by hand with: "+
			"cd src && python prepare.py", err, firstLine(string(out)))
		return
	}
	outcome := prepareOutcome(string(out))
	log.Printf("zone: script corpus %s", outcome)
	// THE INDEX IMAGE IS INVALID THE MOMENT THE CORPUS CHANGES. prepare.py saying anything but
	// "all fresh" means it rewrote .lua files, so the image built from the old ones must go -
	// otherwise a zone would keep serving names that point at files that no longer define them.
	if outcome != "all fresh" {
		if err := os.Remove(indexImage); err == nil {
			log.Printf("zone: script corpus changed - the index image was removed and will be rebuilt")
		}
	}
	logScriptBalance(string(out))
}

// prepareOutcome turns prepare.py's output into the ONE line the startup log
// carries: "refreshed - TRANSLATED n script(s)" when it did work, "all fresh"
// when nothing was stale. An output it does not recognise still reports the
// first line it saw rather than claiming a freshness it never observed.
func prepareOutcome(out string) string {
	var work, first string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if first == "" {
			first = line
		}
		if strings.HasPrefix(line, "nothing to do") {
			return "all fresh"
		}
		if work == "" && (strings.HasPrefix(line, "TRANSLATED") || strings.HasPrefix(line, "EMITTED")) {
			work = line
		}
	}
	if work != "" {
		return "refreshed - " + work
	}
	if first != "" {
		return "refreshed - " + first
	}
	return "refresh reported nothing"
}

// firstLine is the first non-empty line of a tool's output, for a warning that
// must stay one line. A failed translator's first line is the reason.
func firstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "(no output)"
}

// scriptBalanceShown caps the WARNING lines, not the measurement - the same
// reason as compileCheckedShown: a corpus-wide rebuild must not print four
// thousand lines nobody reads.
const scriptBalanceShown = 5

// scriptBalance is one UNBALANCED line prepare.py printed: a script this boot
// translated whose blocks did not balance.
type scriptBalance struct {
	Path    string
	Openers int
	Ends    int
	Closers int // closers the translator had to synthesise: >=1 means a block was left open
}

// logScriptBalance turns prepare.py's balance report into boot warnings.
//
// WHY (measured 2026-11-03): src\scp_to_lua.py AUTO-CLOSES an unfinished block,
// so a TRUNCATED .scp translates into VALID Lua and the compile check cannot
// see it. prepare.py counts `{` and `}` in the .scp SOURCE of exactly the files
// THIS run translated, and prints one
// `UNBALANCED <path> (openers X, ends Y, N closer(s) added automatically)` line
// each, plus a `BALANCE N of M translated script(s) unbalanced` total. The
// closer count is the better signal: it is the number of `end`s the translator
// invented, so one or more of them is exactly "the source left a block open".
//
// The shipped corpus is brace-balanced (0 of 4,239 measured 2026-11-03), so a
// start that translated nothing prints nothing here, and a file is only named
// when this run translated it. FAIL-SOFT: this only logs, the zone starts.
func logScriptBalance(out string) {
	bad, total := parseScriptBalance(out)
	for i, b := range bad {
		if i == scriptBalanceShown {
			log.Printf("zone: WARNING: script corpus balance-check: ... and %d more "+
				"file(s) look unbalanced", len(bad)-scriptBalanceShown)
			break
		}
		log.Printf("zone: WARNING: script %s looks unbalanced (openers %d, ends %d, "+
			"%d closer(s) added automatically) - if this script was edited, "+
			"check it is not cut off", b.Path, b.Openers, b.Ends, b.Closers)
	}
	if total != "" {
		log.Printf("zone: script corpus balance-check: %s", total)
	}
}

// parseScriptBalance reads prepare.py's two balance line shapes. A line it does
// not recognise is skipped: an unreadable report must not look like a clean one,
// but it must not be fatal either. The older shape without the closer count is
// still read, so a stale prepare.py cannot silence the warning.
func parseScriptBalance(out string) (bad []scriptBalance, total string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "BALANCE "); ok {
			total = strings.TrimSpace(rest)
			continue
		}
		rest, ok := strings.CutPrefix(line, "UNBALANCED ")
		if !ok {
			continue
		}
		// The counts are the LAST "(openers X, ends Y...)": the path itself never
		// contains that text, but LastIndex keeps a stray one from splitting it.
		at := strings.LastIndex(rest, " (openers ")
		if at < 0 {
			continue
		}
		var openers, ends, closers int
		if n, _ := fmt.Sscanf(rest[at:],
			" (openers %d, ends %d, %d closer(s) added automatically)",
			&openers, &ends, &closers); n == 3 {
			// the current shape
		} else if n, _ := fmt.Sscanf(rest[at:], " (openers %d, ends %d)",
			&openers, &ends); n == 2 {
			closers = 0
		} else {
			continue
		}
		bad = append(bad, scriptBalance{
			Path:    strings.TrimSpace(rest[:at]),
			Openers: openers,
			Ends:    ends,
			Closers: closers,
		})
	}
	return bad, total
}

// compileCheckedShown caps the WARNING lines, not the measurement: a translator
// that mangled the whole corpus must not print four thousand lines nobody reads.
const compileCheckedShown = 5

// compileCheckRefreshedCorpus parses the corpus files the startup refresh just
// rewrote, with the SAME engine that will run them, and NAMES every one that
// does not compile.
//
// WHY (measured 2026-11-03): prepare.py TRANSLATES text and never parses the Lua
// it writes, so a .scp holding `var x = (1 + ;` comes out as `TRANSLATED 1
// script(s)` with exit 0, and the emitted .lua does not parse. The zone then
// starts normally and the fault only shows on the first click on that NPC - the
// index is lazy - with nothing naming the file.
//
// WHICH FILES: the corpus .lua files whose mtime is at/after the moment
// refreshScriptCorpus() began (corpusRefresh.started). That is exactly the set
// the translator rewrote; a file it did not touch keeps its old mtime and is not
// re-checked, so a normal start costs one walk of names plus zero parses. After
// a whole-corpus rebuild (the output folder was missing) every file matches,
// which is correct - they were all just translated.
//
// FAIL-SOFT is the contract: nothing here stops the zone, deletes a file, or
// refuses to index the corpus. A broken script must be NAMED at boot, not made
// fatal - and the game's own .scp copies are fine; what broke is the translation
// of one file, or an edit somebody has just made.
//
// KNOWN LIMIT, not fixed on purpose: src\scp_to_lua.py AUTO-CLOSES unbalanced
// blocks (a probe func with a missing `}` came out as `end  end`), so a
// TRUNCATED script translates into VALID Lua and no compile check can catch it.
// Only genuinely unparseable Lua is caught here.
func compileCheckRefreshedCorpus(h *script.Host, corpus string) {
	if h == nil || !corpusRefresh.ran {
		return
	}
	paths := refreshedCorpusFiles(corpus, corpusRefresh.started)
	bad := h.CompileCheck(paths)
	for i, se := range bad {
		if i == compileCheckedShown {
			log.Printf("zone: WARNING: script corpus compile-check: ... and %d more "+
				"file(s) do NOT compile", len(bad)-compileCheckedShown)
			break
		}
		log.Printf("zone: WARNING: script %s does NOT compile: %s - this file was just "+
			"translated; the game's own copies are fine (the translator cannot tell a "+
			"typo from a valid script)", corpusRel(corpus, se.Path), oneLineError(se.Path, se.Err))
	}
	// ALWAYS, even when it is the zero line: a start that says nothing about the
	// check is indistinguishable from a start whose check is broken.
	log.Printf("zone: script corpus compile-check: %d of %d refreshed file(s) failed",
		len(bad), len(paths))
}

// refreshedCorpusFiles lists the corpus .lua files written at or after `since`,
// sorted, which is what the compile check parses.
//
// A folder it cannot read is skipped: the index walk is the one that reports a
// corpus it cannot reach, and a fault there is already a log line.
func refreshedCorpusFiles(corpus string, since time.Time) []string {
	var files []string
	_ = filepath.Walk(corpus, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(p, ".lua") && !info.ModTime().Before(since) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// corpusRel names a corpus file the way the log should: relative to the corpus
// root, so the line stays short and the path can be pasted into Explorer.
func corpusRel(corpus, path string) string {
	rel, err := filepath.Rel(corpus, path)
	if err != nil {
		return path
	}
	return rel
}

// oneLineError is gopher-lua's parse message as ONE short line. The engine's
// text is `<file> line:N(column:M) near '<token>':   <why>` plus a trailing
// newline, and it repeats the path the warning already names, so that prefix is
// dropped and the runs of spaces are collapsed.
func oneLineError(path string, err error) string {
	msg := strings.TrimSpace(err.Error())
	msg = strings.TrimSpace(strings.TrimPrefix(msg, path))
	if msg == "" {
		return err.Error()
	}
	return strings.Join(strings.Fields(msg), " ")
}

// runZone starts the Go zone server: it answers the world-entry handshake, spawns
// the town's NPCs, echoes movement, and answers a click on an NPC with that NPC's
// own script. Everything else is logged with its count so the gap stays visible.
func runZone(c zoneConfig) {
	memStage("entry (config parsed)")
	// The two DIAGNOSTIC switches, checked before anything else runs: a nonsense value
	// is a typo, and a typo acted on is a corrupted probe (a shifted position the size
	// of a map is not a measurement). Refuse it here, where the run has not started.
	if c.diagStopOffset < -100000 || c.diagStopOffset > 100000 {
		log.Fatalf("zone: -diag-stop-offset=%d is out of range: it is a diagnostic that moves a "+
			"character a few units, so anything past 100000 is a typo, not a probe", c.diagStopOffset)
	}
	// SAY IT LOUDLY, ON EVERY START. A probe that is left on poisons every later
	// observation in the same run (a sideways-jumping model "proves" the stop works
	// even after the real fault is found), so the run announces that it is not normal
	// before it accepts a single connection.
	var diagOn, diagWhy []string
	if c.diagStopOffset != 0 {
		diagOn = append(diagOn, fmt.Sprintf("-diag-stop-offset=%d", c.diagStopOffset))
		diagWhy = append(diagWhy, fmt.Sprintf("the x of every ZC_STOP_PC it broadcasts is "+
			"moved %d east", c.diagStopOffset))
	}
	if c.diagHex {
		diagOn = append(diagOn, "-diag-hex")
		diagWhy = append(diagWhy, "every announced character is dumped as DIAGHEX lines "+
			"of raw packet bytes")
	}
	if c.diagEvents {
		diagOn = append(diagOn, "-diag-events")
		diagWhy = append(diagWhy, "every echoed move, stop and walk leg is written as its "+
			"own log line again, one line per event on the movement path")
	}
	if len(diagOn) > 0 {
		log.Printf("zone: *** WARNING: DIAGNOSTIC SWITCH ON: %s. This zone is NOT behaving "+
			"normally: %s. Every such switch is OFF unless passed on the command line; "+
			"restart WITHOUT it before believing anything else this run says.",
			strings.Join(diagOn, ", "), strings.Join(diagWhy, ", and "))
	}
	// A LOG SWITCH, SAID OUT LOUD AND NOT AS A FAULT: -quietspawn places every body
	// exactly as before and changes no packet - it only stops writing two lines per
	// body. A reader who does not know that would look for a spawner that never ran,
	// which is the one failure a missing line can fake.
	if c.quietSpawn {
		// THE TWO LINE PREFIXES ARE NOT QUOTED HERE ON PURPOSE: an announcement that
		// spelled them out would answer `grep "zone: MONSTER AI class"` with itself, and
		// that grep is exactly how a reader checks the flag did its job.
		log.Printf("zone: -quietspawn is ON: the startup spawner writes ONE line per " +
			"tactics decision instead of two per body. Every body is still placed and " +
			"the wire is unchanged; a body whose tactics row does NOT resolve still " +
			"writes both of its per-body lines, and so does every body created after " +
			"this burst. Restart without the flag for all of them.")
	}

	// REFRESH THE SCRIPT CORPUS FIRST, before anything reads it: the script host
	// indexes it ONCE (clickEngine below) and the status shadow reads the derived
	// item tables (LoadItemProps further down), so this is the only moment an
	// edited .scp can become visible. It sits after the guard above on purpose,
	// so a typo'd diagnostic dies immediately instead of after up to 120 s of
	// translating. Skipped by -no-scripts (the corpus is never read then) and by
	// -norefresh (the run must not start a second process).
	if !c.noScripts && !c.noRefresh {
		if os.Getenv("GE_CORPUS_READY") != "" {
			// THE WORLD ALREADY TURNED THE KEY (cmd/zoneman -prepare): the corpus was refreshed
			// once for every zone, so a zone must not run prepare.py again - 1.1 s each, 18 times.
			log.Printf("zone: script corpus refreshed by the world manager - not checking it again")
		} else {
			refreshScriptCorpus(c.corpus)
		}
	}

	table, err := protocol.LoadSpec(c.tablePath)
	if err != nil {
		log.Fatalf("zone: load command table %s: %v", c.tablePath, err)
	}

	// SAY THE DEVELOPMENT FLAG IS ON, before a client connects. With it off - the
	// default - this line does not exist and the wire is what it always was; with it
	// on, the run is a probe and the log says so, because a monster that appears on a
	// map nobody briefed would otherwise read as a data bug.
	if c.spawnMonster {
		log.Printf("zone: -spawnmonster is ON: one ZC_ENTER_STANDINGMONSTER (%d, %d bytes) "+
			"will introduce ONE monster at the fixed spot on each session's GAME_READY. "+
			"Every other map's wire is unaffected; restart WITHOUT this flag for a normal run.",
			zone.CmdZCEnterStandingMonster, zone.EnterStandingMonsterSize)
	}
	if c.spawnSplashPair {
		log.Printf("zone: -spawnsplashpair is ON: TWO monsters of class 11600 will be " +
			"created at (3000,-1800) and (3080,-1800) right after the map's own startup " +
			"tactics run, so one area skill can be tested against two bodies. Restart " +
			"WITHOUT this flag for a normal run.")
	}
	if strings.TrimSpace(c.spawnDummy) != "" {
		log.Printf("zone: -spawndummy %q is ON: one real Monster row per listed class "+
			"will be created at fixed walkable cells right after the map's own startup "+
			"tactics run, and the DEF the game's own SCR_Get_MON_DEF computes for each "+
			"will be logged. Restart WITHOUT this flag for a normal run.", c.spawnDummy)
	}
	if strings.TrimSpace(c.dummyCell) != "" {
		log.Printf("zone: -dummycell %q is set: -spawndummy will place its monsters on "+
			"THOSE cells, not on the four measured on map 1331. Restart WITHOUT both "+
			"flags for a normal run.", c.dummyCell)
	}
	// SAY THE SP LOCK IS ON, before a client connects. A cast that costs nothing would
	// otherwise read as the SP rule being broken, so a locked run announces itself once
	// here and once per cast it puts a bar back. With it off - the default - neither
	// line exists and the cut is the reference's own.
	if c.spLock {
		log.Printf("zone: -splock is ON: a skill's SP cost is cut and then put back, so the " +
			"caster's bar stays full for the whole session (a formula test needs the damage " +
			"number, not the SP). Restart WITHOUT this flag for a normal run.")
	}
	// SAY THE RING IS OFF AND WHAT THAT MEANS, before a client connects. With the ring
	// inert every session is told about everybody - the pre-R135 behaviour - so a
	// crowded zone in this run is NOT a ring fault, and a reader who was not told
	// would spend the run looking for one.
	if c.sightOff {
		log.Printf("zone: sight: -sightoff - the ring is OFF, this run announces everybody")
	}
	// SAY THE DISTANCE RECORD IS BEING WRITTEN, and that it is only a record: these
	// lines are the measurement, nothing on the wire changes.
	if c.diagDistance {
		log.Printf("zone: -diag-distance is ON: one DIST line per pair of sessions per " +
			"second (both leaders' positions and their X/Y distance), and every STOP the " +
			"zone sends gains the distance to the nearest other player. Log only - no " +
			"packet, state or rule changes. Restart WITHOUT this flag for a normal run.")
	}
	// SAY THE SIMULATED LATENCY AND ITS VALUE, before a client connects. A late
	// reply in a log that never mentions the flag reads as a server fault, so the
	// number is on the record. With it 0 - the default - this line does not exist.
	if c.simLatencyMs > 0 {
		log.Printf("zone: -simlatency %d is ON: every packet this zone writes to a "+
			"client is held back %d ms on the way out, in order, so the run costs "+
			"something like a real network's latency. The read side and every game rule "+
			"are unchanged; restart WITHOUT this flag for a normal run.",
			c.simLatencyMs, c.simLatencyMs)
	}
	// APPLY AND ANNOUNCE THE RESPAWN OVERRIDE, before a client connects. Every body
	// this zone created comes back in seconds instead of the reference's 23.568 s,
	// which is what makes one kill's whole chain observable in a probe; but a body
	// that returns far earlier than its species should is exactly what a broken timer
	// looks like too, so the line says the flag did it. Unset - the default - nothing
	// is applied and this line does not exist.
	if c.respawnDelay > 0 {
		zone.SetRespawnDelay(c.respawnDelay)
		log.Printf("zone: RESPAWN OVERRIDE %s - a DEV FLAG, the reference's 23.568 s is "+
			"replaced for testing", c.respawnDelay)
	}
	// THE MONSTER AI SWITCH, validated and announced in ONE place, before a client
	// connects. `go` is the pre-script engine and the default; `script` runs the game's
	// own tactics script for every living body. An unknown value is REFUSED instead of
	// being treated as the default: a run that silently did something other than what
	// was typed is the one thing the log could not then explain.
	switch strings.ToLower(strings.TrimSpace(c.monai)) {
	case "", "go":
		c.monai = "go"
	case "script":
		c.monai = "script"
	default:
		log.Fatalf("zone: -monai %q is not a mode - the two are `go` (today's Go chase "+
			"and swing, the default) and `script` (the game's own "+
			"SCR_MON_AI_LIB_TS_BATTLE)", c.monai)
	}
	log.Printf("%s", zone.MonsterAIStartupLine(c.monai == "script"))

	// The landing point is a startup failure when the map has none: a zone that
	// cannot place the team is not a zone, and guessing is what crashed the client.
	landing, err := zoneSpawn(c)
	if err != nil {
		log.Fatalf("zone: %v", err)
	}
	mapDesc := "map " + strconv.Itoa(landing.MapID)
	if landing.Map != "" {
		mapDesc += " (" + landing.Map + ")"
	}
	log.Printf("zone: team spawns at %v on %s, from %s (a navmesh CELL, not a height)",
		landing.Pos, mapDesc, landing.Source)

	// Town NPCs come from the emitted data file. Missing is a WARNING, not a
	// failure: a map can legitimately have no town, and an empty plaza is better
	// than a server that will not start.
	npcPath := filepath.Join(c.npcsDir, fmt.Sprintf("map%d.json", c.mapID))
	var npcs []zone.NPC
	meshName := ""
	loaded := 0
	npcDoc, err := zone.LoadNPCs(npcPath)
	if err != nil {
		log.Printf("zone: no town NPCs (%v) - the town will be empty; build the data "+
			"with: cd src && python emit_zone_npcs.py %d", err, c.mapID)
	} else {
		loaded = len(npcDoc.NPCs)
		npcs = npcDoc.NPCs
		meshName = npcDoc.Mesh
		log.Printf("zone: %d town NPC(s) for map %d (data built from client revision %s)",
			len(npcs), c.mapID, npcDoc.ClientRevision)
	}

	// The hide list: the town NPCs the owner does not want spawned. A missing or empty
	// file means "hide nothing" and is NOT an error, so the summary below always says
	// how many rules there were and how many NPCs went - a "0 hidden" is a fact, not a
	// silence.
	hide := loadNPCHide(c.npcHide)
	hidden := 0
	npcs, hidden = hide.Apply(c.mapID, npcs)
	log.Printf("zone: NPC hide list: %d town NPC(s) hidden on map %d (%d -> %d)",
		hidden, c.mapID, loaded, len(npcs))
	// The spawn table names the map's walk-mesh as well, so a map with no NPC file
	// still gets the RIGHT mesh for the path engine instead of a straight line.
	if meshName == "" {
		meshName = landing.Mesh
	}

	// The stance tables are NOT optional: without them the entry record carries no
	// stances and the client crashes a millisecond later. A boot failure is far
	// cheaper than a crash the player has to report.
	if c.xmlDir == "" {
		c.xmlDir = zone.XMLDir()
	}
	// The two tables a RUNTIME spawn needs (zone\create_tables.go): the NPC table
	// resolves `CreateTNPC`'s type NAME to the ClassID that draws the body, and the
	// Monster table is what makes an unknown id a REFUSAL instead of a bodiless
	// actor. Neither is fatal - a zone without them still serves the map and its
	// town NPCs; it refuses only the scripted spawn, with a line saying which table
	// is missing.
	npcTypes, npcTypeErr := zone.LoadNPCTypes(c.xmlDir)
	if npcTypeErr != nil {
		log.Printf("zone: NPC table from %s: %v - a CreateTNPC is REFUSED with a log "+
			"line until this loads", c.xmlDir, npcTypeErr)
	}
	if npcTypes != nil {
		log.Printf("zone: NPC table: %d type(s) by name (a script's "+
			"CreateTNPC type name resolves here)", npcTypes.Len())
	}
	memStage("npc types")
	// THE MONSTER TABLE, IMAGED (card t_96ee949e): parsing 7,720 rows x 253 attributes was
	// 1.94 s of every cold start. The image is rebuilt when any of the table's own XML
	// files is newer, and a missing or unreadable image is not an error - the XML answers.
	monsterImage := zone.MonsterImagePath(c.textImage)
	// The drop ITEM table's image (card t_9779e2fd): 25,353 rows of XML were 2.08 s of a start.
	zone.SetItemImagePath(zone.ItemImagePath(c.textImage))
	// The drop MAP table's image (121 ms of the same block).
	zone.SetMapDropsImagePath(zone.MapDropsImagePath(c.textImage))
	// The drop PENALTY table's image (card t_88e45fd8): the export is not in ge\xml, so the
	// image remembers the path it was built from - see zone\penalty_image.go.
	zone.SetPenaltyImagePath(zone.PenaltyImagePath(c.textImage))
	// The SIGHT RING's map_table image (card t_38b6216f): the ring's table was the last
	// start-up table still parsed as `eachClass` rows - an 85-92 ms `MEMSTAGE sight ring`
	// stage on map 4063, now a 43 KB file read. Set here because PrimeSight runs far below.
	zone.SetSightImagePath(zone.SightImagePath(c.textImage))
	monsterRows, monsterFromImage, monsterRowErr := zone.LoadMonstersCached(c.xmlDir, monsterImage)
	if monsterRowErr == nil && monsterFromImage {
		log.Printf("zone: Monster table from the IMAGE %s - %d row(s), no XML parse",
			monsterImage, monsterRows.Len())
	}
	if monsterRowErr != nil {
		log.Printf("zone: Monster table from %s: %v - a CreateTmonster is REFUSED "+
			"with a log line until this loads", c.xmlDir, monsterRowErr)
	}
	if monsterRows != nil {
		log.Printf("zone: Monster table: %d row(s) by id (a script's "+
			"CreateTmonster id resolves here)", monsterRows.Len())
		log.Printf("%s", zone.MonsterSwingStartupLine())
		log.Printf("%s", zone.MonsterChaseStartupLine())
	}
	memStage("monsters")

	// The experience a kill pays, per level: the game's own `Xp` idspace column
	// `MonXp`, embedded at build time (zone\monster_exp.go, emitted by
	// src\export_exp_table.py). NOT fatal - a zone that cannot read it awards 0 on
	// every kill and says so once, which is better than a number nobody printed.
	monsterExp, monsterExpErr := zone.LoadMonsterExpTable()
	if monsterExpErr != nil {
		log.Printf("zone: monster exp table: %v - every kill awards 0 and no "+
			"ZC_EXP_UP goes out until this loads", monsterExpErr)
	} else {
		log.Printf("zone: monster exp table: %s (per-level, before the monster's "+
			"own factor)", monsterExp.Describe())
	}
	// The exp stage ends HERE, so the next line measures the stance tables alone: the two
	// MEMSTAGE lines used to print the same number because LoadStances sat between them
	// (2026-11-11, card t_88e45fd8).
	memStage("monster exp")
	// THE STANCE TABLES, IMAGED (card t_88e45fd8): 279 ms of every start, 219 of it
	// datatable_item_weapon.xml alone. The image is rebuilt when ANY of the five files the
	// loader reads is newer, and a missing or unreadable image is not an error - the XML answers.
	stances, err := zone.StancesForZone(c.xmlDir, zone.StanceImagePath(c.textImage))
	if err != nil {
		log.Fatalf("zone: stance tables from %s: %v", c.xmlDir, err)
	}
	log.Printf("zone: stance tables: %s", stances.Describe())
	memStage("stances")

	// The Skill table: the NAME of a skill id and its SklATK factor, both read from
	// the client's own `datatable_skill.xml` (zone\skill_table.go). NOT fatal - a zone
	// without it still serves the map and refuses a cast with one line saying so,
	// which is better than firing a hit it cannot name or price.
	// THE SKILL TABLE, IMAGED (card t_53f1a6d5): datatable_skill.xml is 14.5 MB and parsing
	// its 5,626 rows cost 471 ms of every start. The image is rebuilt when the XML is newer.
	skillImage := zone.SkillImagePath(c.textImage)
	skills, skillFromImage, skillErr := zone.LoadSkillsCached(c.xmlDir, skillImage)
	if skillErr != nil {
		log.Printf("zone: skill table from %s: %v - a skill cast is REFUSED with a "+
			"log line until this loads", c.xmlDir, skillErr)
	} else {
		if skillFromImage {
			log.Printf("zone: skill table from the IMAGE %s - %d row(s), no XML parse",
				skillImage, skills.Len())
		}
		log.Printf("zone: skill table: %s", skills.Describe())
	}
	memStage("skills")

	// The Warp table's ClassIDs: the `type` a warp point packet carries, and the row the
	// client looks up to draw its arrow (see zone.LoadWarpTypes). NOT fatal: a zone that
	// cannot read it still serves players, and every warp point it spawns then says its
	// type is 0 - a guess, in the log, instead of a silent wrong number.
	warps, werr := zone.LoadWarpTypes(c.xmlDir)
	if werr != nil {
		log.Printf("zone: warp table from %s: %v - warp points will carry type 0 "+
			"(a guess: A/B it in game)", c.xmlDir, werr)
	} else {
		log.Printf("zone: warp table: %s", warps.Describe())
	}
	memStage("warps")

	// The map's own TYPE is NOT read here on purpose: a stance change is allowed on
	// every map, towns included (the owner, 2026-10-31: "u can change stance in town
	// now ... no combat in town so nothing complex there"). The old
	// `World()->IsVillageMap()` branch in source2\geClient\Source\GENormalObject.cpp:2231
	// is a pre-release reference and the shipped client does not do it, so there is
	// no village rule anywhere in this server - see zone\stance_change.go.

	// Ctrl+C is the only clean shutdown this server has, and a clean shutdown must
	// WITHDRAW the presence file: a zone that has stopped must not look alive for up
	// to world.AliveTimeout, or the barrack sends the next player to a closed port.
	// The signal context is the base for the pool and every connection, so it also
	// ends the handlers below.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// THE METRICS REPORTER (`-metrics`, default 10s, 0 = off). One goroutine for
	// the life of the zone, logging ONE line per interval and resetting the window
	// behind it. Silent on an interval where no packet arrived, so an idle map does
	// not fill the log with zeroes. It starts before the listener, and it dies with
	// ctx on Ctrl+C. See zone\metrics.go.
	if c.metricsEvery > 0 {
		log.Printf("zone: METRICS every %s - one line per interval with packets and "+
			"bytes each way, broadcasts and their recipients, handler microseconds",
			c.metricsEvery)
		zone.StartReporter(log.Writer(), c.metricsEvery, ctx.Done())
	}

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		log.Fatalf("zone: postgres: %v", err)
	}
	defer pool.Close()

	// The binary carries its own schema (db\migrations, embedded), so the ZONE
	// must apply it too: started on its own - which is how it is run - it would
	// otherwise read whatever the barrack last applied, and a new migration would
	// make its queries fail with no hint. goose is idempotent, so this costs a
	// version check per start.
	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("zone: migrate: %v", err)
	}

	// WHICH ADDRESS DO WE TELL THE CLIENT TO DIAL? A zone publishes the address the
	// barrack hands a player who enters this map, so this is the value that made a
	// second PC unable to connect. It is NOT compiled in: the machine row in
	// PostgreSQL answers (-machine names the row), -advertise overrides it, and the
	// documented default answers when neither can - address.Resolve logs ONE line
	// saying which and why. Read ONCE, here: a running role keeps its startup state.
	adv := address.Resolve(ctx, address.Config{
		Flag: c.advertise, Machine: c.machine,
		Store: address.StoreFunc(func(ctx context.Context, machine string) (string, error) {
			return db.MachineIP(ctx, pool, machine)
		}),
		Log: log.Printf,
	})

	// THE BASELINE: everything loaded before the shared tables start (config, the map table,
	// the quest and warp tables). Without this line the first stage's number was the baseline
	// and read as if the text book cost it (corrected 2026-11-09).
	memStage("config and map tables")

	// One text book, shared by the zone (dialog text) and the script engine
	// (IMCFormat). Loaded once, before either uses it.
	book := textBook(c.dict, c.idMap, c.textImage)
	memStage("text book")

	// The item table: a script's item NAME -> class id + stack rule. Not fatal to
	// be missing - every item count then answers 0, which is visibly "we do not
	// know the item table" rather than a wrong number.
	catalog := loadItemCatalog(c.items)

	// The class base stats: what the six WORDs of the basic-stat block carry. They
	// are what makes the Character Info panel show numbers instead of zeros, and
	// they are also the input to the game's own maximum HP formula (zone\stats.go
	// Sheet). NOT fatal to be missing - the sheet is then all zeros, as before.
	zone.SetJobStats(loadJobStats(c.jobStats))
	memStage("job stats")

	// The item columns the status rules read. Without them the DERIVED half of
	// Character Info (ATK, DEF, A.R., D.R., HR, CSPD, the resistances) can only be
	// sent as 0 - which is what it was before 3684 existed.
	zone.SetItemStats(loadItemStats(c.itemStats))

	// The BUY/SELL data: what an item costs in vis and in Feso, what each normal
	// shop group holds in the client's order, and each CUSTOM (barter) group's rows
	// and prices. All of them are needed the moment a shop can be bought from, and
	// all of them say so at startup when they are missing.
	prices := loadPrices(c.prices)
	fesoPrices := loadFesoPrices(c.fesoPrices)
	shopGroups := loadShopGroups(c.shops)
	memStage("prices and shops")
	customShops := loadCustomShops(c.customShops)

	// The walk plan: the live path engine when this machine has it, else the map's
	// PRE-COMPUTED walk graph, else nothing (a straight line, capped at MaxChase).
	// The engine is kept as well: the Last Point takes its HEIGHT and its near-point
	// answers from the same mesh (R73).
	nav := pathEngine(c, meshName)
	// ONE HELPER PER ZONE, STOPPED ON THE WAY OUT: it is a real process holding the
	// map's mesh, and a zone that exits must not leave pathtool.exe behind. Safe when
	// there is no engine at all (nil), which is the normal case on the Linux target.
	defer nav.Close()
	var plan func(from, npc [3]int32, standoff float64) (goal [3]int32,
		route [][3]int32, routeLen float64, overlap bool, ok bool, err error)
	// AND THE CLIENT'S OWN LEGS GET A ROUTE FROM THE SAME MESH (2026-11-02): a player's
	// reported walk is planned once per leg, so the body this side draws walks the ground
	// the client walks instead of the straight chord across it. The walk graph is NOT
	// wired here on purpose - it stores walk LENGTHS between sampled nodes, not a
	// polyline, so a route from it would have corners the map does not have.
	var routeOf func(from, to [3]int32) (route [][3]int32, ok bool)
	if nav != nil {
		plan = nav.Plan
		routeOf = nav.Route
	}
	// THE ENGINE IS THE ONLY PLANNER (2026-11-09, DECISIONS.md). No walk graph is wired as
	// a plan source: without an engine `plan` stays nil and every server-planned walk is
	// REFUSED with a line - the owner's rule ("i want the pathengine to handle everything"),
	// and the reason the helper now runs under Wine too (fact #445).

	store := zone.NewPGStore(pool)
	// Which slot(s) each class may be worn in. Set on the STORE, because the slot
	// check happens inside the equip transaction (a check outside it could race a
	// class change of the same row).
	store.Wearable = loadEquipSlots(c.equipSlots)
	// The world a script can CHANGE: the bridge below is what `CreateTNPC` and
	// `CreateTmonster` reach. Its Handler is filled in after the Handler is built,
	// because the zone package must not be imported by the script package - the
	// same one-way rule Handler.Click already follows.
	spawnWorld := &worldSpawner{}
	clickFunc, resumeFunc, releaseFunc, tacticsFunc, tacticsTick, tacticsArmed, scriptHost := clickEngine(c, book, store,
		catalog, spawnWorld)

	// The town quick-move panel's destinations and the map's gates out, from the same
	// file the spawn came from. A field map has no points, and a map with no exits has
	// no gates; both are normal, not errors. The table itself is kept for the warp NPCs:
	// an NPC's Script names the map it leads to, and that map's own rows say where the
	// team arrives.
	townMoves, gates, points := loadMapRows(c.points, c.mapID)

	// The registry a gate reads the destination's ip and port from. Read per gate, so
	// a zone that starts or dies is picked up without a restart; this check only says
	// once whether the folder is there at all.
	zones := &world.Registry{Dir: c.zones, FallbackIP: adv.IP}
	if len(gates) > 0 {
		if recs, rerr := zones.Records(); rerr != nil {
			log.Printf("zone: no zone registry at %s (%v) - a gate will be REFUSED "+
				"because there is no address to send the player to", c.zones, rerr)
		} else {
			log.Printf("zone: %d live zone(s) in %s - gate travel can only go to one "+
				"of them", len(recs), c.zones)
		}
	}

	h := &zone.Handler{
		Table:          table,
		Cipher:         protocol.DefaultCipher,
		Log:            log.Default(),
		RawLog:         c.rawLog,
		DiagStopOffset: int32(c.diagStopOffset),
		DiagHex:        c.diagHex,
		DiagEvents:     c.diagEvents,
		QuietSpawn:     c.quietSpawn,
		SpawnMonster:   c.spawnMonster,
		StrictRange:    c.strictRange,
		SightOff:       c.sightOff,
		DiagDistance:   c.diagDistance,
		SPLock:         c.spLock,
		SimLatency:     time.Duration(c.simLatencyMs) * time.Millisecond,
		TeamOf:         store.TeamOf,
		ConsumeEntry:   store.ConsumeZoneEntry,
		DropEntry:      store.DeleteZoneEntry,
		Properties:     store.LockProperties,
		CommanderOf:    store.CommanderOf,
		GmLevelOf:      store.GmLevelOf,
		Bag:            store.Stacks,
		Stances:        stances,
		Skills:         skills,
		Spawn:          landing.Pos,
		// The same map's arrival formation: each member's own slot, or everybody on
		// the spawn point when the file predates the formation.
		SpawnFormation: landing.Formation,
		NPCs:           npcs,
		MapID:          c.mapID,
		MapName:        landing.Map,
		Warps:          warps,
		Points:         townMoves,
		Gates:          gates,
		ZonePoints:     points,
		Zones:          zones,
		AskForZones:    true,
		Prewarm:        c.prewarm,
		RequestZone:    zones.Request,
		Transfers:      c.zones,
		Click:          clickFunc,
		ResumeClick:    resumeFunc,
		ReleaseClick:   releaseFunc,
		Text:           book,
		Plan:           plan,
		Route:          routeOf,
		NavHeight:      navHeight(nav),
		NavNear:        navNear(nav),
		NPCTypes:       npcTypes,
		Monsters:       monsterRows,
		MonsterExp:     monsterExp,
		Tactics:        tacticsFunc,
		TacticTick:     tacticsTick,
		TacticArmed:    tacticsArmed,
		ShopGroups:     shopGroups,
		CustomShops:    customShops,
		Prices:         prices,
		FesoPrices:     fesoPrices,
		// The same transaction a script uses: a buy and a sale write their money
		// and their items together, or not at all.
		Commit: store.CommitTx,
		// The equip pair: the same transaction layer, one bag row moving to the
		// paper doll and back. Without them an equip is refused with a log line
		// (Handler.Equip / Handler.Unequip are nil).
		Equip:   store.EquipItem,
		Unequip: store.UnequipItem,
	}

	// THE BATTLE AI'S WORLD, WIRED AT LAST (2026-11-08). Stage 1's hate table, stage
	// 2a's reads and tactics, stage 2b's orders and this card's seek were all bound on
	// the host and none of them had a world behind it, so every answer was the empty
	// value. One bridge, four interfaces: the zone IS the world those builtins ask, so
	// the same `h` is assigned to each.
	//
	// IT CHANGES NOTHING UNTIL A SCRIPT CALLS THEM, which is why it is safe beside
	// -no-scripts: with no host there is nothing to wire, and with `-monai go` nothing
	// runs a monster's tactics script. A CLICK script that called one of these names
	// used to get the missing-builtin stub's 0; that was never a correct answer either.
	if scriptHost != nil {
		scriptHost.Hate = h
		scriptHost.Monster = h
		scriptHost.Order = h
		scriptHost.Seek = h
		h.MonsterBrain = scriptHost
	}
	h.MonsterAIScript = c.monai == "script"

	// THE DROP TABLES, ARMED AT START (2026-11-07). They used to load inside the first
	// kill: the owner's log has the first `DROP rules` line 1.85 s after that kill's
	// `DEAD` line, and everything the kill owed waited behind 75 MB of Item XML. This
	// is a start-up cost, so it is paid here, beside the item and status tables, and
	// the one line says how much a roll has to work with.
	memStage("before drop tables")
	h.ArmDropTables()
	memStage("drop tables")

	// The Character Info SHEET'S SOURCE (zone\status_script.go): the game's own
	// status script, answering the fifteen numbers every 3684 carries - and the max
	// HP/SP the bars are drawn against. It is not an instrument any more; without it
	// the panel reads zeros and says so.
	props, perr := zone.LoadItemProps(zone.DefaultItemProps)
	if perr != nil {
		log.Printf("zone: NO STATUS SHEET - %v; the file is data\\item_props.json "+
			"(rebuild it with: cd src && python emit_item_props.py). The Character "+
			"Info panel and the HP/SP maxima have no source.", perr)
		props = nil
	}
	if st := zone.NewStatusScript(scriptHost, props, stances, uint32(c.mapID), log.Printf); st != nil {
		h.Status = st
		log.Printf("zone: status sheet source armed - %d item class(es), %d growing curve(s), "+
			"the fifteen numbers come from pc\\get_status_pc.scp",
			props.Len(), len(props.Growing))
	}
	memStage("status script")

	// R84 SLICE 1 AND 2: THE GAME'S OWN DAMAGE SCRIPT. Slice 1 ran it BESIDE the
	// placeholder; slice 2 makes its factor the packet's number, with the target's own
	// defence applied. Armed here, beside the sheet, because it reads the same host
	// and the same character data.
	if ds, derr := zone.NewDamageScript(scriptHost, log.Printf); derr != nil {
		log.Printf("zone: skill damage script NOT armed: %v - the packet keeps the "+
			"placeholder", derr)
	} else {
		ds.Sheet = h.Status
		h.Damage = ds
		log.Printf("zone: skill damage script armed - %s from %s", zone.SkillAttackEntry, ds.File)
	}
	memStage("damage script")

	// R84 SLICE 2: THE TARGET'S OWN DEF, the game's `SCR_Get_MON_DEF`, on the same
	// host and from the monster row's own Arm/DEF_BM/DEFPER_BM/RaceType columns. It is
	// the input the damage rule's `(100 - defence)/100` divides by; without it a cast
	// carries the placeholder and says the DEF could not be read.
	if mds, merr := zone.NewMonsterDefScript(scriptHost, log.Printf); merr != nil {
		log.Printf("zone: monster defence script NOT armed: %v - every cast keeps the "+
			"placeholder and says the target's DEF could not be read", merr)
	} else {
		h.Defence = mds
		log.Printf("zone: monster defence script armed - %s from %s",
			zone.MonsterDefEntry, mds.File)
	}
	memStage("monster def script")

	// THE SPAWNED MONSTER'S MAX HP, the game's `SCR_Get_MON_MHP`, on the same host and
	// from the same row columns as the DEF above (zone\monster_hp.go). It answers the
	// one number a `CreateTmonster` that passed no `mhpFix` cannot get from the table:
	// the Monster table has no MHP column at all. Without it a spawn is drawn with
	// hp 0 - no target frame, no ledger entry, and no swing.
	if mhs, herr := zone.NewMonsterHPScript(scriptHost, log.Printf); herr != nil {
		log.Printf("zone: monster HP script NOT armed: %v - a spawn that passes no "+
			"MHP is drawn with hp 0 and the reason is logged once", herr)
	} else {
		h.MaxHP = mhs
		log.Printf("zone: monster HP script armed - %s from %s",
			zone.MonsterHPEntry, mhs.File)
	}
	memStage("monster hp script")

	// THE BUFF TABLE AND ITS ENTER SCRIPTS (buff slice 1, zone\buff_table.go and
	// zone\buff_script.go). The ROW says what a buff is - its UpdateType, its group and
	// rank - and the buff's OWN ENTER script says how long it lasts, because
	// `ApplyTime` is 0 on 1,954 of the 1,962 rows and the script writes the real number
	// (`buff.ApplyTime = 30000`, ge\script\buff\buff.scp:24).
	//
	// A MISSING TABLE IS NOT FATAL AND IS NOT SILENT: every buff application is then
	// refused with one line naming the file, which is the only answer that does not put
	// an invented row behind a mask bit. A missing HOST is the same shape one step down:
	// every buff is scriptless and a NORMAL one is refused by name instead of being
	// granted with 0.
	// THE BUFF TABLE COMES FROM ITS OWN IMAGE when one is present and newer than the XML: 1,962
	// rows x 54 attributes parsed into maps cost 26 MB and 2.2 s in every zone (measured
	// 2026-11-09). One image per table, the owner's rule, so this cannot invalidate the text or
	// script images.
	buffImage := filepath.Join(filepath.Dir(c.textImage), "buff_image.bin")
	buffXML := filepath.Join(c.xmlDir, zone.BuffTableFile)
	if !zone.BuffImageStale(buffImage, buffXML) {
		if b, ierr := zone.LoadBuffsImage(buffImage); ierr == nil {
			h.Buffs = b
			log.Printf("zone: buff table from the IMAGE %s - %d row(s), no XML parse", buffImage, b.Len())
			memStage("buffs")
		} else {
			log.Printf("zone: buff image unusable (%v) - reading the XML", ierr)
		}
	}
	if h.Buffs == nil {
		if buffs, berr := zone.LoadBuffs(c.xmlDir); berr != nil {
			log.Printf("zone: NO BUFF TABLE - %v; every buff application will be REFUSED with a "+
				"line naming the file (%s)", berr, filepath.Join(c.xmlDir, zone.BuffTableFile))
		} else {
			h.Buffs = buffs
			log.Printf("zone: buff table loaded - %d row(s) from %s; a buff's duration is its "+
				"own ENTER script's", buffs.Len(),
				filepath.Join(c.xmlDir, zone.BuffTableFile))
			memStage("buffs")
			if serr := zone.SaveBuffsImage(buffImage, buffs); serr != nil {
				log.Printf("zone: the buff image could not be saved for the next start (%v)", serr)
			} else {
				log.Printf("zone: buff image saved to %s - the next start reads it", buffImage)
			}
		}
	}
	memStage("scripts armed")
	if bs, serr := zone.NewBuffScript(scriptHost, log.Printf); serr != nil {
		log.Printf("zone: buff ENTER scripts NOT armed: %v - every buff is treated as "+
			"scriptless, so a NORMAL buff is refused by name and never granted with 0", serr)
	} else {
		bs.Sheet = h.Status
		h.BuffScript = bs
		log.Printf("zone: buff ENTER scripts armed on the corpus host - " +
			"SCR_BUFF_ENTER_<ClassName> is run on add and its ApplyTime is what the list counts")
	}
	memStage("buff script arming")

	// The saved-point list behind the teleportation window (3640/3641/3642/3643/3644):
	// one row per slot per commander in `saved_teleport`. Wired here rather than in
	// the literal above because the three hooks belong together.
	h.UseSavedTeleports(store.SavedTeleports, store.SaveSavedTeleport,
		store.RemoveSavedTeleport)
	// The ONE extra method the Warp List needs: 3638 RENAME changes the memo a point
	// is drawn with. Same table, same slots, no position change.
	h.UseSavedPointRename(store.RenameSavedTeleport)
	// The Warp List's Up/Down (3639): a move of one row to another display position.
	// It rewrites the row order in `saved_teleport.display_order`, so the order the
	// player set is what the next 3634 draws.
	h.UseSavedPointMove(store.MoveSavedTeleport)

	// The SAVED START POINT: the town whose waypoint grid Save button was pressed
	// last. Read once per entry (Handler.startGame), written by the 3468 handler.
	h.UseStartPoint(store.StartPointOf, store.SaveStartPoint)

	// THE LAST POINT: where the leader stood when the session left. Written on the way
	// out (log-out, disconnect, map change) and read once per entry. `commander.zone_id`
	// plus one `character_position` row per character; it never touches
	// `commander.point`, which is the save point below.
	h.UseLastPoint(store.LastPointOf, store.SaveLastPoint)
	// "To the Save Point": the commander's saved WARP ID, the input TransferRandomLobbyZone reads.
	h.SaveWarpIDOf = store.SaveWarpIDOf

	// WHICH BARRACK BUTTON was pressed - New Game (22), Continue Game (23) or the
	// mission button (24). It decides the ENTRY ORDER of the two points above, so the
	// player's own choice decides which one wins.
	h.UseEntryKind(store.EntryKindOf)

	// The bridge again, this time with the zone it writes into: from here on a
	// script's CreateTNPC/CreateTmonster lands in THIS zone. It is set before the
	// listener opens and never changed afterwards.
	spawnWorld.h = h

	// THE SIGHT RING'S TABLE, AT START (card t_a593b9a2). `sightRadii` used to read
	// data\map_table.xml the first time ANY caller needed a radius, which is the first
	// body's broadcast: an 81-87 ms XML parse on map 4063, one call, paid in the middle
	// of the spawn burst and charged to `MEMSTAGE spawner` (a measured 176-185 ms of
	// stage, ~118 of them this read plus the broadcast lines). It is a fact about the
	// START, so it is paid HERE, in its own stage, before anything can create a body -
	// and `MEMSTAGE sight ring` is what carries it from now on, so the spawner's number
	// is the spawner's. `-sightoff` returns before the table is touched, so a
	// measurement run still skips the read whole. Since card t_38b6216f the read is an
	// IMAGE (`out\sight_image.bin`, set above with the other table images), so this stage is
	// a file read and the XML is parsed only when the image is missing, stale or corrupt.
	h.PrimeSight()
	memStage("sight ring")

	// THE MAP'S OWN ACTORS RUN THEIR TACTICS ENTRY NOW, once, before anybody can
	// connect. This is the original's behaviour when a trigger object enters the
	// world (`TriggerObject.cpp:49-57`) and it is what creates Thueringen Lakeside's
	// four Scarecrow dummies: AUCH_HITDUMMY01's script is NPC_HITDUMMYMANAGER, its
	// entry is SCR_NPC_HITDUMMYMANAGER_TS_NONE, and that function's four CreateTNPC
	// calls are the dummies. Running it here and not per player is deliberate - a
	// second player entering must not create a second set.
	h.StartupTactics()
	memStage("startup tactics")

	// THE MAP'S OWN SPAWNER (zone\spawner.go, R98): the `mongenflag_<map>` points and
	// the `mongentype_<map>` rows that name each point's monster, class and
	// `RespawnTime`. One body per flag row, through the SAME creation path a probe
	// uses. A map with no type table (map 1331 fld_auch is one) places nothing and
	// says so once, keeping the 23.568 s fallback; the probes below are unchanged.
	h.StartSpawner(zone.LoadSpawner(c.xmlDir, landing.Map))

	// The -spawnsplashpair probe, when it was asked for: two bodies to aim one
	// area skill at. Off by default, and it does nothing at all in that case.
	spawnSplashPair(h, c.spawnSplashPair, log.Printf)

	// The -spawndummy probe, when it was asked for: one real Monster row per listed
	// class, on the same strip of ground, each with its own DEF logged. Off by
	// default, and it does nothing at all in that case.
	spawnDummies(h, c.spawnDummy, c.dummyCell, log.Printf)

	ln, err := net.Listen("tcp", c.addr)
	if err != nil {
		log.Fatalf("zone: listen %s: %v", c.addr, err)
	}

	// The presence file is what makes this map FINDABLE. Without one the barrack
	// answers from its own fallback address, which knows the machine but is the wrong
	// WORLD (one map name and one port) - the whole reason this announcement exists.
	// Written AFTER the bind, because a file that promises a port must never exist for
	// a port that did not open: `port` is the port this listener BOUND, never a
	// configured one, while the ip is the address the client can reach (adv.IP).
	port := 0
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}
	ann := &world.Announcer{
		Dir:     c.presence,
		Record:  presenceRecord(c.mapID, landing.Map, adv.IP, port),
		Log:     log.Printf,
		Players: h.Online, // so cmd/zoneman can stop a map nobody is standing in
	}
	if err := ann.Start(); err != nil {
		// NOT fatal: the zone serves the players already in it either way. What it
		// costs is that the barrack cannot find the map by itself.
		log.Printf("zone: could not announce this zone (%v) - the barrack will not "+
			"find map %d on its own and falls back to %s; serving players is "+
			"unaffected", err, c.mapID, barrack.FallbackAddr(adv.IP))
	} else {
		log.Printf("zone: announced map %d (%s) at %s:%d in %s - the barrack can find "+
			"this zone by map id", c.mapID, landing.Map, adv.IP, port, ann.Path())
	}
	// Withdraws on the way out, so a stopped zone does not look alive. Safe when the
	// announcement failed, and safe when the process is killed outright (the file
	// then goes stale and the registry ignores it after AliveTimeout).
	defer ann.Close()

	// A blocked Accept does not notice ctx by itself, so the shutdown closes the
	// listener underneath it. That is the only way this loop ends.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	// GIVE THE LOAD BACK BEFORE TAKING PLAYERS (2026-11-09). Startup builds every table
	// with the default GOGC, so the heap it keeps is about twice what the zone really
	// needs: measured 136.6 MB alive and 317 MB private. One FreeOSMemory after the last
	// load (and the zone's own data is stable from here) hands the free pages back to the
	// OS, which is the difference between 97 warm maps costing ~31 GB and ~19 GB.
	// Nothing below this line allocates in proportion to the tables.
	debug.FreeOSMemory()

	memStage("spawner")
	log.Printf("zone: UP on %s - waiting for client...", c.addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("zone: shutting down - presence withdrawn for map %d", c.mapID)
				return
			}
			log.Printf("zone: ACCEPT failed: %v", err)
			continue
		}
		go h.Serve(ctx, conn)
	}
}

// presenceChannel is the channel a Go zone announces. There is no per-channel zone
// process yet (the PORT is the instance), so this matches src\world.py's default of 0.
// The registry prefers an exact channel and accepts any otherwise, so a player on
// another channel still finds this zone.
const presenceChannel = 0

// presenceIP used to live here, as a `const presenceIP = "127.0.0.1"`: the address
// every zone wrote into its presence file. It is gone on purpose. The address now
// comes from the address book - the machine row in PostgreSQL, or -advertise - and
// resolving it is address.Resolve's only job (package geserver/address), so a
// player on a second PC is fixed by one UPDATE instead of a rebuild.

// presenceRecord is what a zone announces: the map it serves, the map's class name,
// the ip the client can reach it on, and the port it BOUND. A function rather than
// four inline lines so a test can hold the record the zone really writes
// (advertise_test.go).
func presenceRecord(mapID int, mapName, ip string, port int) world.Record {
	return world.Record{
		MapID: mapID, Channel: presenceChannel, IP: ip,
		Port: port, MapName: mapName,
		// A manager-started zone says so, so the manager can adopt its own orphans after a
		// restart without ever touching a zone the operator started by hand (cmd/zoneman).
		Managed: os.Getenv("GE_MANAGED") != "",
	}
}

// commandFilter parses "10,16,35" into a set. An empty string returns nil,
// meaning "keep everything".
func commandFilter(spec string) map[uint16]bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	keep := map[uint16]bool{}
	for _, s := range strings.Split(spec, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			keep[uint16(v)] = true
		}
	}
	return keep
}

// memstats is 1 when GE_MEMSTATS is set: memStage then prints what each table costs.
var memstats int

// processBegan is the process's own clock for memStage: the first line it writes is the
// offset from THIS, so a stage's number is directly comparable with the log's timestamps.
var processBegan = time.Now()

// memStage marks one load stage: its name and the milliseconds since the process started,
// and - under GE_MEMSTATS - the LIVE heap after it.
//
// THE TIME IS ALWAYS PRINTED (2026-11-10, card t_96ee949e). A cold map start is ~6.2 s and
// the phase breakdown was being read off log timestamps that only bracketed some loaders;
// the 2.05 s between "live zone(s)" and "DROP rules" had no line at all inside it. One
// line per stage, every start, makes the start-up table a thing a reader can add up.
// The heap costs a forced GC, so that half still only runs under GE_MEMSTATS.
func memStage(name string) {
	ms := float64(time.Since(processBegan).Microseconds()) / 1000
	if memstats == 0 {
		log.Printf("MEMSTAGE %-22s %7.0f ms", name, ms)
		return
	}
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	log.Printf("MEMSTAGE %-22s %7.0f ms live=%.1fMB", name, ms,
		float64(m.Alloc)/(1<<20))
}
