// zoneman - the zone manager: it watches the zone registry directory for
// `request_<map>.json` files and starts a zone for that map, so a player can walk through
// any gate without someone starting that map's zone by hand.
//
// WHY IT EXISTS. Every gate, warp NPC and waypoint answers with the address of the zone
// that serves the destination map, and refused the moment no zone was live for it - which
// on this server means "ask the operator to start the map". The owner's words: "hard to
// test each time asking u to start certain map". The zone that wants a map writes a
// request (world.Registry.Request); this process starts the zone and the registry answers
// on the next tick.
//
// POLICY. It starts a zone ON DEMAND and never more than -max at once: over the cap it
// stops the zone IT started least recently (a map the player has not asked for in a while).
// It only ever stops zones it started itself, so a hand-started zone is never killed.
//
// USAGE (from docker-port\out, the deployed folder):
//
//	goserver_zoneman.exe -bin goserver_zone.exe -zones zones -base-port 7010 -max 8 \
//	    -jobstats ../data/job_stats.json -equipslots ../data/item_equip.json \
//	    -itemstats ../data/item_stats.json -logdir ../logs
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gecfg "geserver/config"
	"geserver/db"
	"geserver/world"

	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	bin       string
	zonesDir  string
	basePort  int
	maxZones  int
	jobstats  string
	equipslot string
	itemstats string
	logdir    string
	maps      string
	memlimit  string
	warm      string
	idle      time.Duration
	linger    time.Duration
	retain    time.Duration
	dbDSN     string
	monai     string
	startWait time.Duration
	maxTries  int
	mapTable  string
	preload   bool
	prepare   bool
}

type managed struct {
	mapID int
	port  int
	start time.Time
	cmd   *exec.Cmd
}

func main() {
	cfg, source, err := newManagerConfig(os.Args[1:], os.Stderr)
	if err != nil {
		log.Fatalf("zoneman: %v", err)
	}
	if source == "" {
		log.Printf("zoneman: config: the built-in defaults (no server.json found; -config names one)")
	} else {
		log.Printf("zoneman: config: %s", source)
	}
	// The resolved policy, on one line: the answer to "did it read the file I think it
	// read", which is the whole point of moving these out of the command line.
	log.Printf("zoneman: policy: max %d, idle %s, linger %s, retain %s, memlimit %q, base-port %d, prepare %v, preload %v",
		cfg.maxZones, cfg.idle, cfg.linger, cfg.retain, cfg.memlimit, cfg.basePort, cfg.prepare, cfg.preload)

	allowed := map[int]bool{}
	for _, s := range strings.Split(cfg.maps, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if id, err := strconv.Atoi(s); err == nil {
			allowed[id] = true
		}
	}

	// ABSOLUTE, because Go refuses a relative path for a program it runs (and the manager
	// runs from the deployed folder, so "goserver_zone.exe" is a real path but not one
	// exec will accept).
	if abs, err := filepath.Abs(cfg.bin); err == nil {
		cfg.bin = abs
	}
	warmSet := map[int]bool{}
	for _, s := range strings.Split(cfg.warm, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if id, err := strconv.Atoi(s); err == nil {
			warmSet[id] = true
		}
	}

	// THE DOOR GRAPH IS ONLY THE PRELOAD'S, so it is read only when preload is on: with the
	// plan dropped (owner, 2026-11-10) this is a 2,381-row table read nobody uses.
	var graph *zoneGraph
	if cfg.preload {
		var gerr error
		graph, gerr = loadZoneGraph(cfg.mapTable)
		if gerr != nil {
			log.Printf("zoneman: no door graph (%v) - preload is off", gerr)
			graph = nil
		} else {
			log.Printf("zoneman: door graph: %d map(s) with a way out (preload ON)", len(graph.doors))
		}
	} else {
		log.Printf("zoneman: preload is OFF - a gate starts its map on request, which is ~1.5 s")
	}

	reg := &world.Registry{Dir: cfg.zonesDir}
	// THE PLAN COMES FROM THE DATABASE WHEN IT CAN (2026-11-09, the owner: "go with the
	// database then. much cleaner and better"). The shipped server kept the same thing in
	// RESOURCE.DAEMON + ZONE_DAEMON; ours is one row per map process. The flags stay as the
	// fallback so a manager with no database still runs.
	ports := map[int]int{}
	var usagePool *pgxpool.Pool
	if cfg.dbDSN != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		pool, err := db.Open(ctx, cfg.dbDSN)
		if err != nil {
			log.Printf("zoneman: no database (%v) - using -warm/-maps from the command line", err)
		} else {
			// A FRESH DEPLOYMENT HAS AN EMPTY DATABASE, and the plan lives in one of its
			// tables - so the manager MIGRATES BEFORE IT READS. On the VPS the manager is the
			// first thing that runs (it is the container's CMD) and the barrack, the other
			// migrator, is not even in that stack. Without this a new install warmed nothing
			// and said "the zone plan could not be read". goose is idempotent: a database
			// that is already current pays one no-op.
			mctx, mcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if merr := db.Migrate(mctx, pool); merr != nil {
				log.Printf("zoneman: migrations failed (%v) - the plan may not be readable", merr)
			}
			mcancel()
			// The connect budget above may be spent by now; the plan read gets its own.
			pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
			rows, rerr := db.LoadZoneDaemons(pctx, pool)
			pcancel()
			if rerr != nil {
				log.Printf("zoneman: the zone plan could not be read (%v) - using the flags", rerr)
			} else {
				// THE TABLE DECIDES WHAT IS WARM AND ON WHICH PORT - NOT WHAT IS ALLOWED.
				// A map with no row is exactly the on-demand case (missions, 1,999 of them),
				// and refusing it because nobody wrote a row would undo "started when a gate
				// asks". The -maps allowlist still applies when it is given explicitly.
				warmSet = map[int]bool{} // the OUTER set: startWarm reads it
				explicitAllowed := len(allowed) > 0
				for _, d := range rows {
					if !d.ManagementOn || d.Kind != "Z" {
						continue
					}
					if d.Port > 0 {
						ports[d.MapID] = d.Port
					}
					if d.Warm {
						warmSet[d.MapID] = true
					}
				}
				if !explicitAllowed {
					allowed = map[int]bool{} // empty means any map
				}
				allow := "any map on request"
				if len(allowed) > 0 {
					allow = fmt.Sprintf("%d map(s) allowlisted", len(allowed))
				}
				log.Printf("zoneman: plan from zone_daemon: %d row(s), %d warm, %s",
					len(rows), len(warmSet), allow)
				usagePool = pool // kept open: the session-count writer owns it
			}
		}
		cancel()
	}

	m := &manager{cfg: cfg, reg: reg, allowed: allowed, live: map[int]*managed{},
		starting: map[int]bool{},
		failed:   map[int]time.Time{}, warm: warmSet, zeroSince: map[int]time.Time{},
		tries: map[int]int{}, inUse: map[int]bool{}, preloaded: map[int]time.Time{}, graph: graph,
		ports: ports, pool: usagePool}
	log.Printf("zoneman: watching %s for requests; bin=%s base-port=%d max=%d warm=%d idle=%s",
		cfg.zonesDir, cfg.bin, cfg.basePort, cfg.maxZones, len(warmSet), cfg.idle)
	// THE WARM SET GOES UP AT BOOT, in its own goroutine so requests are still answered
	// while the towns load (~4 s each). A warm map is started once and then never stopped.
	if m.cfg.prepare {
		ignition(preparePaths{
			preparePy:  "../src/prepare.py",
			prepareDir: "../src",
			corpus:     "../out/lua_corpus",
			xmlDir:     "../../ge/xml",
			textImage:  "../out/text_image.bin",
			buffImage:  "../out/buff_image.bin",
			timeout:    120 * time.Second,
		})
		// THE ZONES ARE TOLD THE CORPUS IS READY: they must not run prepare.py again each.
		os.Setenv("GE_CORPUS_READY", "1")
	} else {
		// NOT SILENT. The container runs `-prepare=false` because it is a DEPLOYMENT image with
		// no Python in it (owner, 2026-11-10): everything derived is baked on the dev machine.
		// A stale corpus is then invisible here, so the skip is named in the log with the one
		// action that fixes it.
		log.Printf("zoneman: ignition OFF (-prepare=false) - the corpus and the table images are " +
			"taken as BUILT; a stale one is invisible here: run src\\prepare.py on the dev " +
			"machine and rebuild the image")
	}
	// TAKE BACK WHAT A PREVIOUS MANAGER STARTED, before the warm set goes up: an orphan is
	// otherwise invisible to the idle clock and never stops (adoptManaged).
	m.adoptManaged()
	go m.startWarm()
	go m.planLoop()
	go m.preloadLoop()
	go m.sessionCountLoop()
	m.run()
}

// adoptManaged takes over every zone a PREVIOUS manager started, BEFORE the warm set goes up.
//
// WHY (owner, 2026-11-10: "i dont really want to restart any map myself if thats possible. let
// the manager do its own thing... the manager should take over back as soon as possible").
// A manager restart used to leave every on-demand zone it had started running and INVISIBLE:
// m.live is empty at boot, a request for a map that is already served is answered "somebody
// already serves it" and the request file dropped, and start() - the only adopter - is reached
// for WARM maps only. Those orphans never idled out and piled up until somebody stopped them
// by hand.
//
// THE MARKER DECIDES IT: GE_MANAGED=1 is passed to every zone this manager starts (see
// start), and the zone writes `managed` into its presence record. A record WITHOUT it is a
// hand-started zone - the owner's rule is that it is never touched - so it is left alone.
func (m *manager) adoptManaged() int {
	recs, err := m.reg.Records()
	if err != nil {
		return 0
	}
	n := 0
	for _, rec := range recs {
		if !rec.Managed || rec.MapID <= 0 || rec.Port <= 0 {
			continue
		}
		m.mu.Lock()
		_, have := m.live[rec.MapID]
		if !have {
			m.live[rec.MapID] = &managed{mapID: rec.MapID, port: rec.Port, start: time.Now()}
			m.inUse[rec.Port] = true
		}
		m.mu.Unlock()
		if have {
			continue
		}
		log.Printf("zoneman: adopted map %d on :%d (pid %d) - a previous manager started it", rec.MapID, rec.Port, rec.PID)
		n++
	}
	if n > 0 {
		log.Printf("zoneman: %d zone(s) adopted from a previous manager - the idle clock covers them again", n)
	}
	return n
}

type manager struct {
	cfg     config
	reg     *world.Registry
	allowed map[int]bool

	mu        sync.Mutex
	live      map[int]*managed  // map id -> the zone this manager started for it
	starting  map[int]bool      // map id -> a start() call holds it right now (one copy, see start)
	failed    map[int]time.Time // map id -> when a start last failed (no retry storm)
	warm      map[int]bool      // maps that stay up: never stopped by the cap or by idling
	tries     map[int]int       // map id -> how many starts failed this run (0 = never failed)
	inUse     map[int]bool      // port -> handed to a start that has not announced yet
	graph     *zoneGraph        // map id -> the maps its doors lead to (preload)
	preloaded map[int]time.Time // map id -> when the preload asked for it (no repeat requests)
	recent    map[int]time.Time // map id -> when a player was last counted there (24 h record)
	recentAt  time.Time         // when `recent` was read
	zeroSince map[int]time.Time // map id -> since when its zone reports 0 players
	ports     map[int]int       // map id -> the fixed port its row names (empty = next free)
	pool      *pgxpool.Pool     // the usage-log pool (db.RecordSessionCount); nil = no database
}

// preloadLoop loads the maps a player could walk into NEXT, while they are still standing on the
// map before them. The owner asked for it: "we need something like a preload, both for the warm
// map and on demand map."
//
// The trigger is a LIVE map WITH A PLAYER: its doors' destinations (the client's own MovableZone)
// are started in the background, two at a time, so the gate click finds the zone already up. A map
// that nobody enters still falls to the idle stop like any other on-demand map - a preload is a
// head start, not a promotion to warm.
func (m *manager) preloadLoop() {
	if !m.cfg.preload || m.graph == nil {
		return
	}
	const parallel = 2
	sem := make(chan struct{}, parallel)
	for {
		time.Sleep(10 * time.Second)
		recs, err := m.reg.Records()
		if err != nil {
			continue
		}
		occupied := map[int]bool{}
		for _, r := range recs {
			if r.Players > 0 {
				occupied[r.MapID] = true
			}
		}
		if len(occupied) == 0 {
			continue
		}
		for from := range occupied {
			for _, next := range m.graph.neighbours(from) {
				if occupied[next] {
					continue
				}
				if _, live := m.reg.Address(next, world.AnyChannel); live {
					continue
				}
				m.mu.Lock()
				_, ours := m.live[next]
				when, asked := m.preloaded[next]
				m.mu.Unlock()
				if ours || (asked && time.Since(when) < 2*time.Minute) {
					continue
				}
				if m.cfg.maxTries > 0 && m.tries[next] >= m.cfg.maxTries {
					continue
				}
				m.mu.Lock()
				m.preloaded[next] = time.Now()
				m.mu.Unlock()
				go func(id int) {
					sem <- struct{}{}
					defer func() { <-sem }()
					log.Printf("zoneman: preloading map %d - a player is on a map whose doors lead to it", id)
					m.start(id)
				}(next)
			}
		}
	}
}

// planLoop FOLLOWS THE TABLE WHILE IT RUNS. The plan used to be read once at boot, so an
// Unwarm pressed in the console did nothing until the next restart and a Warm never started the
// map at all (the owner edits the list in the console - "im editing the list").
//
// The rules it keeps:
//   - a map that became warm starts now;
//   - a map that stopped being warm is stopped NOW when nobody is inside, and otherwise the
//     moment the last player leaves (an open zone has no timer - the player stays);
//   - a fixed port in the row is used on the next start.
func (m *manager) planLoop() {
	if m.pool == nil {
		return
	}
	ctx := context.Background()
	for {
		time.Sleep(15 * time.Second)
		rows, err := db.LoadZoneDaemons(ctx, m.pool)
		if err != nil {
			continue
		}
		warm := map[int]bool{}
		ports := map[int]int{}
		for _, d := range rows {
			if !d.ManagementOn || d.Kind != "Z" {
				continue
			}
			if d.Warm {
				warm[d.MapID] = true
			}
			if d.Port > 0 {
				ports[d.MapID] = d.Port
			}
		}
		m.mu.Lock()
		old := m.warm
		m.warm, m.ports = warm, ports
		m.mu.Unlock()

		for id := range warm {
			if old[id] {
				continue
			}
			if _, live := m.reg.Address(id, world.AnyChannel); live {
				continue
			}
			log.Printf("zoneman: map %d is warm in the plan - starting it", id)
			go m.start(id)
		}
		for id := range old {
			if warm[id] {
				continue
			}
			log.Printf("zoneman: map %d is no longer warm in the plan", id)
			if players := m.playersOn(id); players == 0 {
				m.stopZone(id, "unwarmed in the plan, nobody inside")
			} else {
				log.Printf("zoneman: map %d has %d player(s) inside - it stops when they leave", id, players)
			}
		}
	}
}

// recentlyUsed reads the 24 h activity record: for every map, when a player was last counted in
// it (session_count_zone, one row per live zone per minute with CONN_COUNT > 0). Cached for a
// minute so the idle sweep does not query per zone per tick.
func (m *manager) recentlyUsed() map[int]time.Time {
	m.mu.Lock()
	if time.Since(m.recentAt) < time.Minute && m.recent != nil {
		out := m.recent
		m.mu.Unlock()
		return out
	}
	m.mu.Unlock()

	out := map[int]time.Time{}
	if m.pool != nil {
		rows, err := m.pool.Query(context.Background(),
			`SELECT zone_id, MAX(log_date) FROM session_count_zone
			 WHERE log_date > now() - $1::interval AND conn_count > 0 GROUP BY zone_id`,
			m.cfg.retain.String())
		if err == nil {
			for rows.Next() {
				var id int
				var at time.Time
				if err := rows.Scan(&id, &at); err == nil {
					out[id] = at
				}
			}
			rows.Close()
		}
	}
	m.mu.Lock()
	m.recent, m.recentAt = out, time.Now()
	m.mu.Unlock()
	return out
}

// playersOn is the count the zone last reported, or 0 when it is not running.
func (m *manager) playersOn(mapID int) int {
	recs, err := m.reg.Records()
	if err != nil {
		return 0
	}
	for _, r := range recs {
		if r.MapID == mapID {
			return r.Players
		}
	}
	return 0
}

func (m *manager) run() {
	var lastReconcile time.Time
	for {
		m.sweep()
		if time.Since(lastReconcile) >= 30*time.Second {
			lastReconcile = time.Now()
			m.reconcileWarm()
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// lastLine is the last non-empty line of a file, trimmed - the zone's own reason for exiting.
func lastLine(path string) string {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "(no log)"
	}
	nl := "\n"
	lines := strings.Split(strings.TrimRight(string(blob), nl), nl)
	for k := len(lines) - 1; k >= 0; k-- {
		if line := strings.TrimSpace(lines[k]); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return line
		}
	}
	return "(empty log)"
}

// reconcileWarm puts a WARM map back up when it is down.
//
// WHY: a warm map is "always on" (DECISIONS.md 2026-11-09) - but until now the warm set was
// started once, at boot. A zone that crashed, or that the operator stopped from the console,
// stayed down until the manager itself restarted, and the first player to walk in met a gate
// that answered nothing. This is the supervisor half of the job the name promises.
//
// IT RESPECTS THE SAME RULES AS START: only maps in the plan with warm = true, never more than
// -max, and a map whose start failed is left to the retry floor (15 s) rather than hammered.
func (m *manager) reconcileWarm() {
	for mapID := range m.warm {
		if _, live := m.reg.Address(mapID, world.AnyChannel); live {
			delete(m.failed, mapID)
			continue
		}
		if when, bad := m.failed[mapID]; bad && time.Since(when) < 15*time.Second {
			continue
		}
		if m.cfg.maxTries > 0 && m.tries[mapID] >= m.cfg.maxTries {
			if m.tries[mapID] == m.cfg.maxTries {
				m.tries[mapID]++ // log once, then stay quiet
				log.Printf("zoneman: map %d failed %d times - leaving it down (unwarm it in "+
					"the console, or fix what its log names)", mapID, m.cfg.maxTries)
			}
			continue
		}
		m.mu.Lock()
		_, ours := m.live[mapID]
		m.mu.Unlock()
		if ours {
			continue // its process is alive, it just has not announced yet
		}
		log.Printf("zoneman: warm map %d is down - bringing it back", mapID)
		if !m.start(mapID) {
			m.tries[mapID]++
			m.failed[mapID] = time.Now()
		}
	}
}

// sweep answers every pending request, then keeps the cap.
func (m *manager) sweep() {
	files, _ := filepath.Glob(filepath.Join(m.cfg.zonesDir, "requests", "request_*.json"))
	for _, f := range files {
		mapID, action, ok := readRequest(f)
		if !ok {
			os.Remove(f)
			continue
		}
		if action == "stop" {
			m.stopZone(mapID, "operator asked")
			os.Remove(f)
			continue
		}
		if _, live := m.reg.Address(mapID, world.AnyChannel); live {
			// Somebody (a hand-started zone) already serves it.
			os.Remove(f)
			continue
		}
		if len(m.allowed) > 0 && !m.allowed[mapID] {
			log.Printf("zoneman: map %d asked for but not on the allowlist - ignored", mapID)
			os.Remove(f)
			continue
		}
		if when, bad := m.failed[mapID]; bad && time.Since(when) < 15*time.Second {
			continue // do not hammer a map that just failed; the request stays
		}
		if !m.start(mapID) {
			m.tries[mapID]++
			m.failed[mapID] = time.Now()
			// Leave the request in place: the player's next try asks again.
			continue
		}
		delete(m.failed, mapID)
		os.Remove(f)
	}
	m.stopIdle()
	m.enforceCap()
}

// startWarm brings the warm set up, one map at a time. It runs beside the request loop:
// the first player must not wait for twenty towns to load.
func (m *manager) startWarm() {
	// LOAD SEVERAL AT A TIME, not one after another: a zone takes 4-35 s and a warm set of 78
	// maps is then an hour of serial waiting (measured 2026-11-09).
	const parallel = 8
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for id := range m.warm {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.start(id)
		}(id)
	}
	wg.Wait()
}

// stopIdle stops a NON-warm zone that has reported 0 players for longer than -idle. A warm
// map is never stopped here, and neither is a zone this manager did not start.
func (m *manager) stopIdle() {
	if m.cfg.idle <= 0 {
		return
	}
	recs, err := m.reg.Records()
	if err != nil {
		return
	}
	now := time.Now()
	for _, s := range m.idleStops(now, recs, m.recentlyUsed()) {
		// ONE PATH FOR EVERY STOP. stopZone knows a zone THIS manager started (its cmd) AND a
		// zone it ADOPTED after a restart (through the pid in the presence record). The old code
		// killed cmd.Process itself, so an adopted zone - which is what every zone becomes after
		// the manager restarts - was silently exempt from the idle clock, and the fleet never
		// shrank back to the warm set.
		m.stopZone(s.mapID, fmt.Sprintf("idle: 0 players for %s (grace %s)",
			now.Sub(s.since).Round(time.Minute), s.grace))
	}
}

// idleStop is one zone the idle clock says is due: which map, since when it has been empty,
// and the grace it was given.
type idleStop struct {
	mapID int
	since time.Time
	grace time.Duration
}

// idleStops answers which non-warm zones have been empty long enough to stop, and ARMS the
// clock for a zone it sees empty for the first time. It is the DECISION only - no process is
// touched - so a test can drive it without starting a zone.
//
// TWO CLOCKS, ONE RULE. A zone nobody has EVER used stops after -idle (10 min). A zone that
// had a player recently is somebody's road, so it is kept for -linger (2 h) after the last
// player leaves - and the 24 h activity record (session_count_zone, written once a minute per
// live zone) is what decides "recently": a map untouched for -retain is not lingered at all.
// The owner asked for exactly this: "detect player activity/count within 24hours and close
// that zone if inactive".
func (m *manager) idleStops(now time.Time, recs []world.Record, recent map[int]time.Time) []idleStop {
	seen := map[int]bool{}
	var out []idleStop
	for _, rec := range recs {
		seen[rec.MapID] = true
		if m.warm[rec.MapID] {
			delete(m.zeroSince, rec.MapID)
			continue
		}
		if rec.Players > 0 {
			delete(m.zeroSince, rec.MapID)
			continue
		}
		since, armed := m.zeroSince[rec.MapID]
		if !armed {
			m.zeroSince[rec.MapID] = now
			continue
		}
		grace := m.cfg.idle
		if at, ok := recent[rec.MapID]; ok && now.Sub(at) < m.cfg.retain {
			grace = m.cfg.linger
		}
		if now.Sub(since) < grace {
			continue
		}
		m.mu.Lock()
		_, ours := m.live[rec.MapID]
		m.mu.Unlock()
		if !ours {
			continue // a hand-started zone this manager never owned: not ours to stop
		}
		out = append(out, idleStop{mapID: rec.MapID, since: since, grace: grace})
	}
	for id := range m.zeroSince {
		if !seen[id] {
			delete(m.zeroSince, id)
		}
	}
	return out
}

// sessionCountLoop writes ONE SAMPLE PER LIVE ZONE PER MINUTE - the row the shipped server's
// GAIA_ZONE_SESSION_COUNT produced into RESOURCE_LOG.SESSION_COUNT_ZONE (LOG_DATE, GROUP_ID,
// ZONE_ID, CHANNEL_ID, CONN_COUNT; measured 2026-11-09 in the imported database). It is the record
// that answers "how many players were on map 15 at 21:00 last Tuesday", which no log file can.
//
// SILENT WHEN THERE IS NO DATABASE: a manager with no `-db` still runs zones, it just cannot keep
// this record. A failed INSERT is logged and the next minute tries again - a broken record must
// never stop a zone.
func (m *manager) sessionCountLoop() {
	if m.pool == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for now := range t.C {
		recs, err := m.reg.Records()
		if err != nil {
			log.Printf("zoneman: session count skipped - %v", err)
			continue
		}
		for _, r := range recs {
			if r.Port == 0 {
				continue
			}
			if err := db.RecordSessionCount(context.Background(), m.pool, now, r.MapID, r.Channel, r.Players); err != nil {
				log.Printf("zoneman: %v", err)
			}
		}
	}
}

// readRequest reads one request file: which map, and WHAT to do with it.
//
// `action` is "" or "start" for the ordinary case - a gate (or the operator console) asking
// for a map - and "stop" for the console's Stop button (cmd/admin, 2026-11-09). A stop is
// answered only for a zone THIS manager started: a hand-started zone is never killed.
func readRequest(path string) (mapID int, action string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	var req struct {
		MapID  int    `json:"map_id"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(b, &req); err != nil || req.MapID <= 0 {
		return 0, "", false
	}
	return req.MapID, strings.ToLower(strings.TrimSpace(req.Action)), true
}

// stopZone kills one zone THIS manager started. It answers false for a zone it does not
// own (a hand-started process), and a log line says which case it was.
func (m *manager) stopZone(mapID int, why string) bool {
	m.mu.Lock()
	z := m.live[mapID]
	m.mu.Unlock()
	if z == nil || z.cmd == nil || z.cmd.Process == nil {
		// NOT OURS, BUT THE OPERATOR ASKED. A zone started by a manager that has since been
		// restarted (or by hand) is not in m.live; refusing left the map up with no way to
		// stop it from the console (measured 2026-11-09, six zones). The presence record now
		// carries the process id, so the request can still be honoured - and the line says
		// that is what happened.
		if rec, live := m.reg.Address(mapID, world.AnyChannel); live && rec.PID > 0 {
			log.Printf("zoneman: map %d was not started by this manager, but the registry says "+
				"pid %d (%s) - stopping it as asked", mapID, rec.PID, why)
			if p, err := os.FindProcess(rec.PID); err == nil {
				_ = p.Kill()
			}
			_ = os.Remove(filepath.Join(m.cfg.zonesDir, fmt.Sprintf("zone_%d.json", rec.Port)))
			m.releasePort(rec.Port)
			// THE ENTRY GOES TOO. Leaving it made the manager believe the map was still up,
			// so start() answered "already served" for a process it had just killed.
			m.mu.Lock()
			delete(m.live, mapID)
			m.mu.Unlock()
			delete(m.zeroSince, mapID)
			return true
		}
		log.Printf("zoneman: map %d asked to stop (%s) but this manager does not own it", mapID, why)
		return false
	}
	log.Printf("zoneman: map %d stopping on :%d (%s)", mapID, z.port, why)
	_ = z.cmd.Process.Kill()
	// THE PRESENCE FILE GOES WITH THE PROCESS. A killed zone cannot withdraw its own file, and
	// the registry only stops counting a file once its heartbeat is older than AliveTimeout - so
	// without this the console would show a stopped map as live for that whole window.
	_ = os.Remove(filepath.Join(m.cfg.zonesDir, fmt.Sprintf("zone_%d.json", z.port)))
	m.releasePort(z.port)
	m.mu.Lock()
	delete(m.live, mapID)
	m.mu.Unlock()
	delete(m.zeroSince, mapID)
	return true
}

// start launches one zone for mapID on a free port and waits for it to announce itself.
// It answers false when the map could not be started, and the caller keeps the request.
func (m *manager) start(mapID int) bool {
	m.mu.Lock()
	if _, already := m.live[mapID]; already {
		m.mu.Unlock()
		return true
	}
	// ONE START PER MAP, CLAIMED BEFORE ANYTHING SLOW HAPPENS. Two callers race at boot:
	// startWarm brings the warm set up while run()'s first pass calls reconcileWarm, which
	// sees the map "down" because it is not in m.live until its process has actually started.
	// Measured in the container (2026-11-10): map 10 came up TWICE, on :7010 and :7012, and
	// the registry then held two presence files for one map. The claim is taken under the same
	// lock as m.live and released when this call returns, so the loser waits for nothing and
	// reports the same answer the winner would.
	if m.starting[mapID] {
		m.mu.Unlock()
		return true
	}
	m.starting[mapID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.starting, mapID)
		m.mu.Unlock()
	}()

	// ALREADY SERVED: a zone from a previous manager run (or started by hand) is ADOPTED, not
	// duplicated. Without this a manager restart started a second copy of every warm map on a
	// fresh port while the first copies kept running. stopZone reaches an adopted zone through
	// the pid in its presence record.
	if rec, live := m.reg.Address(mapID, world.AnyChannel); live {
		log.Printf("zoneman: map %d is already served on :%d (pid %d) - adopting it",
			mapID, rec.Port, rec.PID)
		m.mu.Lock()
		m.live[mapID] = &managed{mapID: mapID, port: rec.Port, start: time.Now()}
		m.mu.Unlock()
		return true
	}

	// AT THE CAP: only a zone with NOBODY inside may be stopped to make room. A warm map never
	// is; a non-warm map with a player in it is somebody's open zone and has no timer - the owner
	// ("zone have no such timer so if player stays for long time, we also have to accommodate
	// that"). When every candidate has a player, the new map simply stays down and says so.
	if m.atCap() {
		if id := m.evictable(); id > 0 {
			m.stopZone(id, "cap: needed the slot")
		} else {
			log.Printf("zoneman: map %d asked for but every zone is at the cap with a player "+
				"inside - not starting it", mapID)
			return false
		}
	}

	port, ok := m.reservePort(mapID)
	if !ok {
		log.Printf("zoneman: map %d asked for but no free port from %d", mapID, m.cfg.basePort)
		return false
	}
	name := fmt.Sprintf("zone_go_%d.log", mapID)
	logPath, err := filepath.Abs(filepath.Join(m.cfg.logdir, name))
	if err != nil {
		logPath = filepath.Join(m.cfg.logdir, name)
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("zoneman: cannot open %s: %v", logPath, err)
		return false
	}
	cmd := exec.Command(m.cfg.bin,
		"-role", "zone",
		"-addr", fmt.Sprintf(":%d", port),
		"-map", strconv.Itoa(mapID),
		"-jobstats", m.cfg.jobstats,
		"-equipslots", m.cfg.equipslot,
		"-itemstats", m.cfg.itemstats,
		"-monai", m.cfg.monai,
		"-log", logPath,
	)
	// THE MEMORY CEILING (measured 2026-11-09): a zone at the Go defaults settles at ~329 MB
	// private; GOMEMLIMIT=200MiB brings it to ~233 MB and holds it there for minutes. The
	// live heap is 136 MB, so the limit is headroom, not a squeeze - and with a warm world
	// it is the difference between 20 towns costing 6.6 GB and 4.7 GB.
	//
	// AND THE MANAGER'S MARK (2026-11-10): GE_MANAGED=1 makes the zone say `managed` in its
	// presence record, so a manager that restarts can ADOPT its own orphans and idle them
	// out. A zone started by hand never carries it and is never touched.
	env := append(os.Environ(), "GE_MANAGED=1")
	if m.cfg.memlimit != "" {
		env = append(env, "GOMEMLIMIT="+m.cfg.memlimit)
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		log.Printf("zoneman: starting map %d failed: %v", mapID, err)
		return false
	}
	log.Printf("zoneman: started map %d on :%d (pid %d), waiting for it to announce",
		mapID, port, cmd.Process.Pid)

	// IN FLIGHT COUNTS AS LIVE: the slot is taken before the wait, because a zone that is still
	// loading is a zone somebody asked for. Without this the warm-map check saw "warm but not
	// up" one second into a 33 s load and started a second copy (measured 2026-11-09).
	m.mu.Lock()
	m.live[mapID] = &managed{mapID: mapID, port: port, start: time.Now(), cmd: cmd}
	m.mu.Unlock()

	// THE REGISTRY IS THE PROOF: the zone writes its presence file once it is up. The wait ends
	// the moment it does, or the moment the process exits, or at -start-wait (a ceiling, not a
	// guess). A town takes ~4 s, but a battleground with 429 monsters and a cold script corpus
	// took 33 s (measured 2026-11-09, Katovic Snowfield) - the old fixed 25 s then started a
	// SECOND copy that lost the port race and exited, and the log called it a failure.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	announced := true
	if !m.waitAnnounce(mapID, done, m.cfg.startWait) {
		select {
		case <-done:
			// IT EXITED: free the slot (otherwise the supervisor thinks it is alive) and quote
			// the zone's own last line, which already says why ("has no spawn row in the table",
			// "no walk mesh", ...). This is the line the operator needs, not a timeout.
			m.mu.Lock()
			delete(m.live, mapID)
			m.mu.Unlock()
			m.releasePort(port)
			m.tries[mapID]++
			m.failed[mapID] = time.Now()
			log.Printf("zoneman: map %d exited before announcing: %s", mapID, lastLine(logPath))
			// THE START FAILED, SO SAY SO. It used to return true here, and startWarm counted a
			// dead child as a success - reconcileWarm then started the same warm map again and
			// again at boot (measured in the container, 2026-11-10).
			announced = false
		default:
			log.Printf("zoneman: map %d did not announce within %s", mapID, m.cfg.startWait)
		}
	}
	logf.Close()
	return announced
}

// waitAnnounce blocks until the zone's presence file appears, the process exits, or the ceiling
// is reached. Announcing first is the normal case by a wide margin.
func (m *manager) waitAnnounce(mapID int, done <-chan struct{}, ceiling time.Duration) bool {
	deadline := time.Now().Add(ceiling)
	for {
		if _, live := m.reg.Address(mapID, world.AnyChannel); live {
			return true
		}
		select {
		case <-done:
			// It exited. Give the file one last look: a zone that announced and then died still
			// counts as failed, but one that announced microseconds before exiting is the same
			// answer either way.
			_, live := m.reg.Address(mapID, world.AnyChannel)
			return live
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// reservePort hands out a port under the lock, so two starts running at the same time cannot
// pick the same one. It prefers the port the map's own row names and never takes a port that
// another row has claimed - a fixed port belongs to its map even before that map starts.
func (m *manager) reservePort(mapID int) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.ports[mapID]; p > 0 && !m.inUse[p] {
		m.inUse[p] = true
		return p, true
	}
	fixed := make(map[int]bool, len(m.ports))
	for _, p := range m.ports {
		fixed[p] = true
	}
	for p := m.cfg.basePort; p < m.cfg.basePort+200; p++ {
		if m.inUse[p] || fixed[p] {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		ln.Close()
		m.inUse[p] = true
		return p, true
	}
	return 0, false
}

// releasePort returns a port to the pool when the start that held it went nowhere.
func (m *manager) releasePort(p int) {
	if p <= 0 {
		return
	}
	m.mu.Lock()
	delete(m.inUse, p)
	m.mu.Unlock()
}

// atCap answers whether this manager already runs as many zones as it may.
func (m *manager) atCap() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live) >= m.cfg.maxZones
}

// evictable returns the map to stop to make room, or 0 when there is none: a zone with a player
// inside is never a candidate (an open zone has no timer, so a long stay must survive the cap),
// and neither is a warm map. Among the rest the longest-running goes first.
func (m *manager) evictable() int {
	players := map[int]int{}
	if recs, err := m.reg.Records(); err == nil {
		for _, r := range recs {
			players[r.MapID] = r.Players
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	best, bestStart := 0, time.Time{}
	for id, z := range m.live {
		if m.warm[id] || players[id] > 0 {
			continue
		}
		if best == 0 || z.start.Before(bestStart) {
			best, bestStart = id, z.start
		}
	}
	return best
}

// enforceCap stops the zone this manager started longest ago while over the cap. Only its
// own zones are candidates: a hand-started zone is never touched, and a zone with a player
// inside stays up - the cap is a memory guard, not an eviction of somebody's game.
//
// THE KILL GOES THROUGH stopZone, outside the lock: the old path killed cmd.Process itself, so
// an ADOPTED zone (no cmd, only the pid in its presence record) was dropped from m.live without
// its process being stopped - the manager forgot it while it kept holding its port.
func (m *manager) enforceCap() {
	m.mu.Lock()
	if len(m.live) <= m.cfg.maxZones {
		m.mu.Unlock()
		return
	}
	players := map[int]int{}
	if recs, err := m.reg.Records(); err == nil {
		for _, r := range recs {
			players[r.MapID] = r.Players
		}
	}
	type entry struct {
		mapID int
		start time.Time
		warm  bool
		used  bool
	}
	var started []entry
	for id, z := range m.live {
		if _, live := m.reg.Address(id, world.AnyChannel); !live {
			delete(m.live, id) // it died on its own
			m.releasePort(z.port)
			continue
		}
		started = append(started, entry{id, z.start, m.warm[id], players[id] > 0})
	}
	// A WARM MAP IS NEVER EVICTED (2026-11-09): the owner's rule is that the towns people
	// use are always up, so the cap only ever costs a map nobody asked to keep.
	// ORDER: a zone nobody is in first, then the one that has been up longest. A warm map and a
	// map with a player inside are only ever reached when nothing else is left.
	sort.Slice(started, func(i, j int) bool {
		if started[i].warm != started[j].warm {
			return !started[i].warm
		}
		if started[i].used != started[j].used {
			return !started[i].used
		}
		return started[i].start.Before(started[j].start)
	})
	var victims []int
	for len(started) > m.cfg.maxZones {
		oldest := started[0]
		if oldest.warm || oldest.used {
			log.Printf("zoneman: over the cap (%d) but every remaining zone has a player "+
				"inside or is warm - leaving map %d up", m.cfg.maxZones, oldest.mapID)
			break
		}
		started = started[1:]
		victims = append(victims, oldest.mapID)
	}
	m.mu.Unlock()
	for _, id := range victims {
		m.stopZone(id, fmt.Sprintf("cap: over -max %d and nobody inside", m.cfg.maxZones))
	}
}

// registerManagerFlags binds cmd/zoneman's settings. The values it is handed as
// DEFAULTS are the ones from server.json (or the built-in ones), so the file works
// with no flags at all and a flag still overrides the file - the same rule the zone
// follows. Every help text here is the original flag's, carried over verbatim.
func registerManagerFlags(fs *flag.FlagSet, cfg *config, configPath *string) {
	fs.StringVar(configPath, "config", "",
		"server.json to read (empty = the file beside the binary, then the built-in defaults); "+
			"every flag below overrides the same-named value in it")
	fs.StringVar(&cfg.bin, "bin", cfg.bin, "the zone binary to start")
	fs.StringVar(&cfg.zonesDir, "zones", cfg.zonesDir, "the zone registry directory")
	fs.IntVar(&cfg.basePort, "base-port", cfg.basePort, "first port a started zone may use")
	fs.IntVar(&cfg.maxZones, "max", cfg.maxZones, "how many zones this manager may keep running")
	fs.StringVar(&cfg.jobstats, "jobstats", cfg.jobstats, "passed to each zone")
	fs.StringVar(&cfg.equipslot, "equipslots", cfg.equipslot, "passed to each zone")
	fs.StringVar(&cfg.itemstats, "itemstats", cfg.itemstats, "passed to each zone")
	fs.StringVar(&cfg.logdir, "logdir", cfg.logdir, "where each zone's log goes")
	fs.StringVar(&cfg.maps, "maps", cfg.maps, "comma-separated allowlist of map ids (empty = any)")
	fs.StringVar(&cfg.memlimit, "memlimit", cfg.memlimit,
		"GOMEMLIMIT given to every zone this manager starts (empty = the Go default)")
	fs.StringVar(&cfg.warm, "warm", cfg.warm,
		"comma-separated map ids to keep WARM: started at boot, never stopped by the cap or by idling")
	fs.DurationVar(&cfg.linger, "linger", cfg.linger,
		"how long an EMPTY zone is kept after a player used it recently (so the next visit is instant)")
	fs.DurationVar(&cfg.retain, "retain", cfg.retain,
		"the activity window: a map with no player inside it for this long is not lingered - it closes")
	fs.DurationVar(&cfg.idle, "idle", cfg.idle,
		"stop a NON-warm zone after this long with 0 players (0 = never stop for idling)")
	fs.StringVar(&cfg.monai, "monai", cfg.monai,
		"which engine steers a monster: `script` runs the game's OWN tactics script "+
			"(SCR_<Tactics>_TS_NONE: RandomMove to wander, GetObjectLocaly -> TS_BATTLE to "+
			"aggro - what the live server does), `go` is the reduced fallback")
	fs.StringVar(&cfg.dbDSN, "db", cfg.dbDSN,
		"PostgreSQL DSN holding the zone plan (table zone_daemon); empty = the flags only "+
			"(NOT in server.json: it carries a password and the file is committed)")
	fs.BoolVar(&cfg.preload, "preload", cfg.preload,
		"OFF by default (owner, 2026-11-10: \"the preload plan was to load adjacent map right? ... we can drop "+
			"that plan since most quest/player movement rarely involve running around map. most of them just warp "+
			"around\"). ON = load the maps a player's doors lead to before they click the gate")
	fs.BoolVar(&cfg.prepare, "prepare", cfg.prepare,
		"the world's ignition: refresh the script corpus and rebuild stale table images ONCE, before any zone starts")
	fs.StringVar(&cfg.mapTable, "map-table", cfg.mapTable,
		"the client's map table, for the door graph the preload walks")
	fs.IntVar(&cfg.maxTries, "max-tries", cfg.maxTries,
		"how many times a start may fail before the manager gives up on that map for this run")
	fs.DurationVar(&cfg.startWait, "start-wait", cfg.startWait,
		"the longest a start may take before it counts as failed (it ends at once when the zone announces or the process exits)")
}

// managerToSettings maps the JSON section onto the struct the rest of this file uses.
// The database DSN is deliberately NOT part of it: it comes from GE_PG_DSN / -db only.
func managerToSettings(m gecfg.Manager) config {
	return config{
		bin:       m.Bin,
		zonesDir:  m.ZonesDir,
		basePort:  m.BasePort,
		maxZones:  m.Max,
		jobstats:  m.JobStats,
		equipslot: m.EquipSlots,
		itemstats: m.ItemStats,
		logdir:    m.LogDir,
		maps:      m.Maps,
		memlimit:  m.MemLimit,
		warm:      m.Warm,
		idle:      time.Duration(m.Idle),
		linger:    time.Duration(m.Linger),
		retain:    time.Duration(m.Retain),
		dbDSN:     db.DSN(),
		monai:     m.MonAI,
		startWait: time.Duration(m.StartWait),
		maxTries:  m.MaxTries,
		mapTable:  m.MapTable,
		preload:   m.Preload,
		prepare:   m.Prepare,
	}
}

// newManagerConfig resolves the manager's settings: server.json first, the flags OVER
// it. It parses twice - once with the built-in defaults to learn whether -config was
// named, then with the file's values as the defaults - which is exactly how the zone
// resolves its own file, so a flag and a file disagree the same way in both binaries.
func newManagerConfig(args []string, output io.Writer) (config, string, error) {
	var path string
	draft := managerToSettings(gecfg.Defaults().Manager)
	first := flag.NewFlagSet("zoneman", flag.ContinueOnError)
	first.SetOutput(output)
	registerManagerFlags(first, &draft, &path)
	if err := first.Parse(args); err != nil {
		return config{}, "", err
	}
	named := false
	first.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			named = true
		}
	})

	base := gecfg.Defaults().Manager
	source := ""
	switch {
	case named && path != "":
		loaded, err := gecfg.Load(path)
		if err != nil {
			return config{}, "", err
		}
		base, source = loaded.Manager, path
	case !named:
		if found := gecfg.Preload(); found != "" {
			loaded, err := gecfg.Load(found)
			if err != nil {
				return config{}, "", err
			}
			base, source = loaded.Manager, found
		}
	}

	cfg := managerToSettings(base)
	fs := flag.NewFlagSet("zoneman", flag.ContinueOnError)
	fs.SetOutput(output)
	registerManagerFlags(fs, &cfg, &path)
	if err := fs.Parse(args); err != nil {
		return config{}, "", err
	}
	return cfg, source, nil
}
