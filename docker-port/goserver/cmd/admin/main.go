// goserver_admin - the operator console: one page that shows every map and lets the owner
// start it, stop it, mark it warm, or read its log.
//
// WHY IT EXISTS. The owner: "i need some sort of control panel to operate the whole server.
// a webview app or web console is lightweight and good enough." Until now the only console was
// a log file and me reading it, and every map he wanted to test cost a message. The original
// server shipped a Windows GUI for this (source2\geManager\ManagerClient: a tree of
// machine -> daemon with start/stop, a state table and the daemon's log over the wire); ours is
// one page on localhost, which needs no install and works from any browser on this machine.
//
// IT OWNS NO STATE AND STARTS NOTHING ITSELF. Start writes the SAME request file a gate writes
// (world.Registry.Request), stop writes that file with "action":"stop" (cmd/zoneman answers it
// for zones it started), and warm is one UPDATE of `zone_daemon`. So the console cannot drift
// from the manager: both read the same table, the same presence files and the same requests.
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"geserver/account"
	"geserver/db"
	"geserver/world"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7099", "where the console listens (localhost only by default)")
	zonesDir := flag.String("zones", "../out/zones", "the zone registry directory the manager and barrack share")
	logDir := flag.String("logdir", "../logs", "folder holding zone_go_<map>.log")
	dsn := flag.String("db", db.DSN(), "PostgreSQL DSN holding zone_daemon")
	mapXML := flag.String("maps", "../../ge/xml/datatable_map.xml",
		"the client's own map table, for the names the player reads (Cite de Reboldoeux, not reb_basic)")
	meshDir := flag.String("meshes", "../navmesh/mesh",
		"walk meshes, to mark the maps a zone could actually serve")
	dictXML := flag.String("dictionary", "../../ge/dictionary/dictionary_local.xml",
		"the client's text dictionary: a map row's Name is a TEXT ID and this is what the client draws")
	flag.Parse()

	ctx := context.Background()
	var pool *pgxpool.Pool
	if *dsn != "" {
		p, err := db.Open(ctx, *dsn)
		if err != nil {
			log.Printf("console: no database (%v) - the warm flags and map names will be missing", err)
		} else {
			pool = p
			defer pool.Close()
			// SIGN-UP WRITES ROWS, so the console migrates like every other writer: on a
			// database that has never run the server, /api/register would otherwise answer
			// "relation account_info does not exist". goose is idempotent.
			if err := db.Migrate(ctx, pool); err != nil {
				log.Printf("console: migrations failed (%v) - registration will not work", err)
			}
		}
	}

	dict := loadDictionary(*dictXML)
	catalogue, titles := loadMapTable(*mapXML, dict)
	log.Printf("console: %d map(s) in %s (%d dictionary entries in %s)",
		len(catalogue), *mapXML, len(dict), *dictXML)

	c := &console{reg: &world.Registry{Dir: *zonesDir}, pool: pool, logDir: *logDir,
		zonesDir: *zonesDir, titles: titles, catalogue: catalogue, meshDir: *meshDir,
		byClass: indexByClass(catalogue), byID: indexByID(catalogue)}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", c.stateJSON)
	mux.HandleFunc("/api/start", c.start)
	mux.HandleFunc("/api/stop", c.stop)
	mux.HandleFunc("/api/warm", c.warm)
	mux.HandleFunc("/api/restart", c.restart)
	mux.HandleFunc("/api/log", c.logTail)
	// SIGN-UP LIVES HERE because this is already an HTTP server on loopback and it already
	// holds the pool. /api/register is the LAUNCHER's route (JSON); /register is the same
	// thing as a page, for the operator. Neither is a website: they ride on the process that
	// already runs.
	mux.HandleFunc("/api/register", c.registerAPI)
	mux.HandleFunc("/register", c.register)
	mux.HandleFunc("/", c.page)

	log.Printf("console: http://%s - the map table refreshes every 5 s; nothing is started until you press a button", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("console: %v", err)
	}
}

type console struct {
	reg      *world.Registry
	pool     *pgxpool.Pool
	logDir   string
	zonesDir string
	// titles is map CLASS name -> the name the player reads ("reb_basic" -> "Cite de Reboldoeux"),
	// straight out of the client's own map table. The console shows that; the internal name stays
	// beside it in small type, because the operator also greps the logs by it.
	titles map[string]string
	// catalogue is EVERY map the client knows, so the operator can find a place that has no
	// zone_daemon row yet (the owner: "we are missing some maps, Katovic area") and start it.
	catalogue []mapInfo
	byClass   map[string]*mapInfo
	byID      map[int]*mapInfo
	meshDir   string
}

// mapInfo is one row of the client's own map table.
type mapInfo struct {
	ID        int
	Mesh      string // TokName: the .tok file a zone loads for this map
	Class     string
	Title     string
	Type      string
	Continent string
}

// row is one line of the table: a configured map, whether it is up, and who is on it.
type row struct {
	MapID   int    `json:"map_id"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Warm    bool   `json:"warm"`
	Live    bool   `json:"live"`
	Players int    `json:"players"`
	Port    int    `json:"port"`
	Type    string `json:"type,omitempty"`
	Mode    string `json:"mode,omitempty"`
	NoMesh  bool   `json:"no_mesh,omitempty"`
}

// rows merges the configured maps with the live ones: a configured map that is down still
// shows, and a map that is live but has no row (on demand, never configured) shows too - it is
// running on this machine and the operator must be able to see and stop it.
func (c *console) rows(ctx context.Context, q string, all bool) []row {
	out := map[int]*row{}
	if c.pool != nil {
		if plan, err := db.LoadZoneDaemons(ctx, c.pool); err == nil {
			for _, d := range plan {
				// THE TITLE IS RESOLVED HERE, for a map that is DOWN as well: the row exists
				// whether or not its zone is running, and the operator must read its name
				// before deciding to start it (the owner: "the name only show when the zone is
				// started").
				mode := "on demand"
				if d.Warm && d.ManagementOn {
					mode = "warm"
				}
				out[d.MapID] = &row{MapID: d.MapID, Name: d.Name, Title: c.titles[d.Name],
					Warm: d.Warm && d.ManagementOn, Mode: mode}
			}
		} else {
			log.Printf("console: zone_daemon: %v", err)
		}
	}
	if recs, err := c.reg.Records(); err == nil {
		for _, rec := range recs {
			r := out[rec.MapID]
			if r == nil {
				r = &row{MapID: rec.MapID, Name: rec.MapName, Mode: "on demand (no row)"}
				out[rec.MapID] = r
			}
			r.Live, r.Players, r.Port = true, rec.Players, rec.Port
			if rec.MapName != "" && r.Name == "" {
				r.Name = rec.MapName
			}
			if r.Title == "" {
				r.Title = c.titles[r.Name]
			}
		}
	}
	// THE SEARCH IS OVER THE WHOLE MAP TABLE (2,381 rows), not only the configured ones: the
	// owner asked for the Katovic area, which has no zone_daemon row at all. Two characters are
	// enough to search; an empty box keeps the table to the configured and live maps.
	if q = strings.ToLower(strings.TrimSpace(q)); len(q) >= 2 {
		// A SEARCH IS EXCLUSIVE: it shows what matches, not the whole warm set plus a hit at the
		// bottom (the first version did that and "katovic" answered 44 towns and one ice map).
		matches := func(id int, name, title string) bool {
			return strings.Contains(strings.ToLower(name), q) ||
				strings.Contains(strings.ToLower(title), q) ||
				strings.Contains(strconv.Itoa(id), q)
		}
		for id, r := range out {
			if !matches(id, r.Name, r.Title) {
				delete(out, id)
			}
		}
		for i := range c.catalogue {
			m := &c.catalogue[i]
			if !matches(m.ID, m.Class, m.Title) {
				continue
			}
			r := out[m.ID]
			if r == nil {
				r = &row{MapID: m.ID, Name: m.Class, Mode: "not configured"}
				out[m.ID] = r
			}
			if r.Title == "" {
				r.Title = m.Title
			}
			r.Type = m.Type
			r.NoMesh = !c.hasMesh(*m)
		}
	}
	for _, r := range out {
		if info := c.byClass[r.Name]; info != nil && r.Type == "" {
			r.Type = info.Type
			r.NoMesh = !c.hasMesh(*info)
		}
	}

	// THE PAGE SHOWS WHAT THE OPERATOR ACTS ON: the warm maps and the live ones. Every other
	// configured map (2,336 of them) is a search away, or one tick of "show every map" - drawing
	// 2,381 rows every 5 s made the page a museum, not a console.
	if !all && q == "" {
		for id, r := range out {
			if !r.Warm && !r.Live {
				delete(out, id)
			}
		}
	}

	list := make([]row, 0, len(out))
	for _, r := range out {
		list = append(list, *r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Live != list[j].Live {
			return list[i].Live
		}
		return list[i].MapID < list[j].MapID
	})
	return list
}

func (c *console) stateJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"maps": c.rows(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("all") == "1"),
		"q":    r.URL.Query().Get("q"),
		"all":  r.URL.Query().Get("all") == "1",
		"now":  time.Now().Format("15:04:05"),
	})
}

// indexByClass is the class -> catalogue row map, built once.
func indexByClass(cat []mapInfo) map[string]*mapInfo {
	out := make(map[string]*mapInfo, len(cat))
	for i := range cat {
		out[cat[i].Class] = &cat[i]
	}
	return out
}

// indexByID is the map id -> catalogue row map: a row created from the console takes the map's
// own class name, so the manager and the zone logs name it the way the client does.
func indexByID(cat []mapInfo) map[int]*mapInfo {
	out := make(map[int]*mapInfo, len(cat))
	for i := range cat {
		out[cat[i].ID] = &cat[i]
	}
	return out
}

// hasMesh answers whether a zone for this map would have a walk mesh to load. A map without one
// can still be started, but its clicks and walks are refused, so the table says so.
func (c *console) hasMesh(m mapInfo) bool {
	if c.meshDir == "" {
		return true
	}
	name := m.Mesh
	if name == "" {
		name = m.Class
	}
	if name == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(c.meshDir, name+".tok"))
	return err == nil
}

// start asks the manager for a map exactly the way a gate does.
func (c *console) start(w http.ResponseWriter, r *http.Request) {
	id, ok := mapArg(r)
	if !ok {
		http.Error(w, "map?", http.StatusBadRequest)
		return
	}
	if err := (&world.Registry{Dir: c.zonesDir}).Request(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("console: START map %d requested by the operator", id)
	writeJSON(w, map[string]any{"ok": true, "action": "start", "map_id": id})
}

// stop writes the same request file with "action":"stop"; the manager stops only a zone it
// started, and says so in its own log when it is not the owner.
//
// IT REFUSES WHILE SOMEBODY IS IN THE MAP (2026-11-09). Killing a zone under a connected client
// crashes that client (null-read at ge.exe 0x201d8f, measured six times on 2026-11-09), so the
// console will not do it by accident: the players are named and the press is refused. The owner
// can force it, because he is the one who knows whether that character is his.
func (c *console) stop(w http.ResponseWriter, r *http.Request) {
	id, ok := mapArg(r)
	if !ok {
		http.Error(w, "map?", http.StatusBadRequest)
		return
	}
	if players, live := c.population(id); live > 0 && players > 0 && r.URL.Query().Get("force") != "1" {
		msg := fmt.Sprintf("map %d has %d player(s) in it - walking them out first avoids the "+
			"client crash a killed zone causes", id, players)
		log.Printf("console: STOP map %d refused: %s", id, msg)
		http.Error(w, msg, http.StatusConflict)
		return
	}
	dir := filepath.Join(c.zonesDir, "requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	blob := fmt.Sprintf("{\"map_id\":%d,\"action\":\"stop\"}", id)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("request_%d.json", id)), []byte(blob), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("console: STOP map %d requested by the operator", id)
	writeJSON(w, map[string]any{"ok": true, "action": "stop", "map_id": id})
}

// population answers how many players are in a map right now, from the presence files.
func (c *console) population(mapID int) (int, int) {
	recs, err := c.reg.Records()
	if err != nil {
		return 0, 0
	}
	for _, rec := range recs {
		if rec.MapID == mapID {
			return rec.Players, 1
		}
	}
	return 0, 0
}

// restart is stop-then-start with a wait: the console answers once the map is either back or
// has not come back, so the operator does not have to press twice and guess.
func (c *console) restart(w http.ResponseWriter, r *http.Request) {
	id, ok := mapArg(r)
	if !ok {
		http.Error(w, "map?", http.StatusBadRequest)
		return
	}
	if players, live := c.population(id); live > 0 && players > 0 && r.URL.Query().Get("force") != "1" {
		http.Error(w, fmt.Sprintf("map %d has %d player(s) in it - they must leave first", id, players),
			http.StatusConflict)
		return
	}
	c.writeRequest(id, "stop")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, live := c.population(id); live == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := (&world.Registry{Dir: c.zonesDir}).Request(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("console: RESTART map %d requested by the operator", id)
	writeJSON(w, map[string]any{"ok": true, "action": "restart", "map_id": id})
}

// writeRequest drops one request file for the manager to answer.
func (c *console) writeRequest(mapID int, action string) {
	dir := filepath.Join(c.zonesDir, "requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("console: %v", err)
		return
	}
	blob := fmt.Sprintf("{\"map_id\":%d,\"action\":%q}", mapID, action)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("request_%d.json", mapID)), []byte(blob), 0o644); err != nil {
		log.Printf("console: %v", err)
	}
}

// warm flips the row the manager reads. A map with NO row is added by the operator: the table
// only names the maps that were configured by hand, so refusing meant the console could not warm
// anything it had just found by searching - a new area, a mission map, a dungeon nobody listed
// ("how about the not configured?"). The row takes the map's class name from the client's own
// table and port 0, which means "the next free port".
func (c *console) warm(w http.ResponseWriter, r *http.Request) {
	id, ok := mapArg(r)
	if !ok {
		http.Error(w, "map?", http.StatusBadRequest)
		return
	}
	on := r.URL.Query().Get("on") == "1"
	if c.pool == nil {
		http.Error(w, "no database", http.StatusInternalServerError)
		return
	}
	tag, err := c.pool.Exec(r.Context(), `UPDATE zone_daemon SET warm = $2 WHERE map_id = $1`, id, on)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		info := c.byID[id]
		if info == nil {
			http.Error(w, "no such map in the client's map table", http.StatusNotFound)
			return
		}
		if _, err := c.pool.Exec(r.Context(), `INSERT INTO zone_daemon
			(daemon_id, map_id, channel, name, port, kind, management_on, warm, note)
			VALUES ((SELECT COALESCE(MAX(daemon_id), 0) + 1 FROM zone_daemon), $1, 0, $2, 0, 'Z', true, $3, $4)`,
			id, info.Class, on, "added from the console"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("console: map %d (%s) had no row - added it, warm=%v", id, info.Class, on)
	}
	log.Printf("console: map %d warm=%v set by the operator", id, on)
	writeJSON(w, map[string]any{"ok": true, "map_id": id, "warm": on})
}

// logTail answers the last lines of a zone's log, which is the question the original console
// answered over the wire (ManagerClient's ReqLogList).
func (c *console) logTail(w http.ResponseWriter, r *http.Request) {
	id, ok := mapArg(r)
	if !ok {
		http.Error(w, "map?", http.StatusBadRequest)
		return
	}
	path := filepath.Join(c.logDir, fmt.Sprintf("zone_go_%d.log", id))
	blob, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	lines := strings.Split(string(blob), "\n")
	const keep = 200
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, strings.Join(lines, "\n"))
}

func (c *console) page(w http.ResponseWriter, r *http.Request) {
	// NEVER CACHED: the page carries its own drawing code, and a browser that keeps yesterday's
	// copy keeps drawing yesterday's columns even when /api/state has moved on (the owner:
	// "i did not see any changes in the browser?"). The table polls the JSON every 5 s; the
	// SHELL must be re-fetched on every load.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, nil); err != nil {
		log.Printf("console: page: %v", err)
	}
}

// loadMapTitles reads `ClassName` -> `EngName` out of the client's own datatable_map.xml.
//
// WHY THE CLIENT'S TABLE AND NOT A COLUMN OF OURS: the name a player reads is the client's, and a
// copy in `zone_daemon` would drift every time the map table is rebuilt. A missing file is not
// fatal - the console then shows the internal names, which is what it did before.
// loadMapTable reads the client's own map table twice over: the full CATALOGUE (every map, for
// the search - "we are missing some maps, Katovic area") and the class -> display name map the
// rows use.
//
// THE NAME COMES FROM THE DICTIONARY, the way the client resolves it: a map row's `Name` is a TEXT
// ID ("<$>10009647</>") and the dictionary holds the text it draws. EngName is the fallback (some
// rows carry a readable English name with no id), and the internal class name is the last resort -
// a mission instance like dun_truman01 has no id and no readable name at all.
func loadMapTable(path string, dict map[int]string) ([]mapInfo, map[string]string) {
	titles := map[string]string{}
	var out []mapInfo
	fh, err := os.Open(path)
	if err != nil {
		log.Printf("console: no map table at %s (%v) - showing internal map names only", path, err)
		return out, titles
	}
	defer fh.Close()
	dec := xml.NewDecoder(fh)
	dec.Strict = false // the client's files carry raw '&' (measured: dictionary_local.xml:16666)
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("console: reading %s: %v", path, err)
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Class" {
			continue
		}
		m := mapInfo{}
		var eng, name string
		for _, a := range start.Attr {
			switch a.Name.Local {
			case "ClassID":
				m.ID, _ = strconv.Atoi(a.Value)
			case "ClassName":
				m.Class = a.Value
			case "TokName":
				m.Mesh = a.Value
			case "Type":
				m.Type = a.Value
			case "Continent":
				m.Continent = a.Value
			case "EngName":
				eng = a.Value
			case "Name":
				name = a.Value
			}
		}
		if m.ID <= 0 || m.Class == "" {
			continue
		}
		if id, ok := textID(name); ok {
			if text := dict[id]; text != "" {
				m.Title = text
			}
		}
		if m.Title == "" && eng != "" && eng != m.Class {
			m.Title = eng
		}
		if m.Title != "" {
			titles[m.Class] = m.Title
		}
		out = append(out, m)
	}
	return out, titles
}

// loadDictionary reads the client's text dictionary: ClassID -> the text it draws. 20 MB of XML,
// read once at startup; a missing file leaves the console with EngName and the internal names.
func loadDictionary(path string) map[int]string {
	out := map[int]string{}
	fh, err := os.Open(path)
	if err != nil {
		log.Printf("console: no dictionary at %s (%v) - falling back to EngName", path, err)
		return out
	}
	defer fh.Close()
	dec := xml.NewDecoder(fh)
	dec.Strict = false // the client's files carry raw '&' (measured: dictionary_local.xml:16666)
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("console: reading %s: %v", path, err)
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Text" {
			continue
		}
		id, text := 0, ""
		for _, a := range start.Attr {
			switch a.Name.Local {
			case "ClassID":
				id, _ = strconv.Atoi(a.Value)
			case "Text":
				text = a.Value
			}
		}
		if id > 0 && text != "" {
			out[id] = text
		}
	}
	return out
}

// textID reads the id out of a map row's `Name` attribute, which the client writes as
// `<$>10009647</>`. Anything else (a bare class name, an empty string) is not an id.
func textID(s string) (int, bool) {
	const open, closeTag = "<$>", "</>"
	i := strings.Index(s, open)
	if i < 0 {
		return 0, false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		return 0, false
	}
	id, err := strconv.Atoi(rest[:j])
	return id, err == nil && id > 0
}

func mapArg(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.URL.Query().Get("map"))
	return id, err == nil && id > 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The page: a table and four buttons. No framework, no build step, no external asset - the
// only thing it needs from the browser is fetch().
var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>ge_server console</title>
<style>
 :root { color-scheme: dark; }
 body { font: 14px/1.45 system-ui, sans-serif; margin: 18px; background: #14171a; color: #e6edf3; }
 h1 { font-size: 17px; margin: 0 0 4px; font-weight: 600; }
 .hint { color: #8b949e; margin-bottom: 14px; }
 table { border-collapse: collapse; width: 100%; max-width: 1000px; }
 th, td { padding: 6px 10px; border-bottom: 1px solid #21262d; text-align: left; }
 th { color: #8b949e; font-weight: 500; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
 tr.up td { background: #0f2417; }
 .on { color: #3fb950; } .off { color: #8b949e; }
 .alias { color: #6e7681; font-size: 11px; }
 .kind { color: #8b949e; font-size: 11px; }
 input { background: #0d1117; color: #e6edf3; border: 1px solid #30363d; border-radius: 5px;
         padding: 3px 8px; font-size: 12px; }
 td.actions { white-space: nowrap; }
 button { background: #21262d; color: #e6edf3; border: 1px solid #30363d; border-radius: 5px;
          padding: 3px 9px; font-size: 12px; cursor: pointer; margin-right: 4px; }
 button:hover { background: #30363d; }
 button.warm { border-color: #9e6a03; color: #e3b341; }
 #log { margin-top: 16px; max-width: 1000px; background: #0d1117; border: 1px solid #21262d;
        border-radius: 6px; padding: 10px; white-space: pre-wrap; font: 12px/1.4 ui-monospace, monospace;
        max-height: 320px; overflow: auto; color: #c9d1d9; }
</style></head><body>
<h1>ge_server console</h1>
<div class="hint">Live maps first. <span id="clock">-</span> &middot; <span id="count">-</span> map(s) up
  &middot; <input id="q" placeholder="search every map: katovic, porto, 1331..." size="34">
  &middot; <label><input type="checkbox" id="all"> show every map (2,381)</label></div>
<table><thead><tr><th>map</th><th>name</th><th>mode</th><th>state</th><th>players</th><th>port</th><th>warm</th><th>actions</th></tr></thead>
<tbody id="rows"><tr><td colspan="7" class="hint">loading...</td></tr></tbody></table>
<div id="log" hidden></div>
<script>
function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}
function button(label, act, map, extra, cls) {
  const b = document.createElement('button');
  b.textContent = label;
  if (cls) b.className = cls;
  b.addEventListener('click', function () { action(act, map, extra); });
  return b;
}
async function action(act, map, extra) {
  await fetch('/api/' + act + '?map=' + map + (extra || ''), { method: 'POST' });
  refresh();
  if (act === 'start' || act === 'stop' || act === 'restart') setTimeout(refresh, 1500);
}
async function showLog(map) {
  const box = document.getElementById('log');
  const r = await fetch('/api/log?map=' + map);
  box.hidden = false;
  box.textContent = r.ok ? await r.text() : 'no log for map ' + map;
}
async function refresh() {
  const q = document.getElementById('q').value.trim();
  const all = document.getElementById('all').checked ? '1' : '0';
  const r = await fetch('/api/state?q=' + encodeURIComponent(q) + '&all=' + all);
  const d = await r.json();
  const tbody = document.getElementById('rows');
  tbody.replaceChildren();
  let up = 0;
  for (const m of d.maps) {
    if (m.live) up++;
    const tr = document.createElement('tr');
    if (m.live) tr.className = 'up';
    tr.appendChild(cell(m.map_id));
    const mode = document.createElement('td');
    mode.textContent = m.mode || '';
    mode.className = 'kind';

    const name = document.createElement('td');
    name.textContent = m.title || m.name || '';
    if (m.title) {
      const alias = document.createElement('span');
      alias.className = 'alias';
      alias.textContent = ' ' + m.name;
      name.appendChild(alias);
    }
    if (m.type) {
      const kind = document.createElement('span');
      kind.className = 'kind';
      kind.textContent = ' [' + m.type + (m.no_mesh ? ', no walk mesh' : '') + ']';
      name.appendChild(kind);
    }
    tr.appendChild(name);
    tr.appendChild(mode);
    tr.appendChild(cell(m.live ? 'up' : 'down', m.live ? 'on' : 'off'));
    tr.appendChild(cell(m.live ? m.players : ''));
    tr.appendChild(cell(m.live ? m.port : ''));
    tr.appendChild(cell(m.warm ? 'yes' : 'no', m.warm ? 'on' : 'off'));
    const actions = document.createElement('td');
    actions.className = 'actions';
    if (m.live) {
      actions.appendChild(button('Stop', 'stop', m.map_id));
      actions.appendChild(button('Restart', 'restart', m.map_id));
    } else {
      actions.appendChild(button('Start', 'start', m.map_id));
    }
    actions.appendChild(button(m.warm ? 'Unwarm' : 'Warm', 'warm', m.map_id, '&on=' + (m.warm ? 0 : 1), 'warm'));
    const logBtn = button('Log', 'log', m.map_id);
    logBtn.dataset.log = '1';
    logBtn.dataset.map = String(m.map_id);
    actions.appendChild(logBtn);
    tr.appendChild(actions);
    tbody.appendChild(tr);
  }
  document.getElementById('clock').textContent = d.now;
  document.getElementById('count').textContent = up;
}
document.getElementById('rows').addEventListener('click', function (ev) {
  const b = ev.target.closest('button');
  if (!b) return;
  if (b.dataset.log !== undefined) showLog(b.dataset.map);
});
document.getElementById('all').addEventListener('change', refresh);
document.getElementById('q').addEventListener('input', function () {
  clearTimeout(window.qTimer);
  window.qTimer = setTimeout(refresh, 250);
});
refresh();
setInterval(refresh, 5000);
</script></body></html>`))

// registerAPI is what a LAUNCHER calls: POST JSON {"member_id":"...","password":"..."} and
// get {"account_id":N} back, or {"error":"..."} with a 4xx. No page, no session, no
// cookies - one route, so a desktop launcher needs no browser and the deployment needs no
// website.
//
// NO GUARD YET, ON PURPOSE, AND THAT IS A LOCALHOST DECISION: the console binds 127.0.0.1 by
// default, so only this machine can reach it. The moment it is exposed for a remote
// launcher it needs the alpha guards the owner asked for (rate limit, suspicious-row check)
// and an authentication of its own - a public POST that creates accounts is a spam target.
func (c *console) registerAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, body any) {
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Printf("console: register api: %v", err)
		}
	}
	if r.Method != http.MethodPost {
		reply(http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var in struct {
		MemberID string `json:"member_id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		reply(http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	in.MemberID = strings.TrimSpace(in.MemberID)
	if c.pool == nil {
		reply(http.StatusServiceUnavailable, map[string]string{"error": "this console started without a database"})
		return
	}
	id, err := account.Create(r.Context(), c.pool, in.MemberID, in.Password)
	if err != nil {
		// A REFUSAL IS THE PLAYER'S FAULT OR THE NAME'S, never a 500: the launcher shows the
		// text as-is ("account ... already exists", "member id ... contains ':'").
		reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("console: REGISTER %q -> account %d (launcher)", in.MemberID, id)
	reply(http.StatusOK, map[string]any{"account_id": id, "member_id": in.MemberID,
		"note": "log in with the client; the family name is created in the game"})
}

// register is the optional HTML face of the same thing: a form for the operator to add a
// friend from a browser. It calls the same account.Create - it is not a second
// implementation and it is not required for the launcher.
func (c *console) register(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct{ Result, Failed, Member string }{}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			data.Failed = err.Error()
		} else {
			member := strings.TrimSpace(r.PostFormValue("member_id"))
			password := r.PostFormValue("password")
			data.Member = member
			switch {
			case c.pool == nil:
				data.Failed = "this console started without a database, so nothing can be created"
			default:
				id, err := account.Create(r.Context(), c.pool, member, password)
				if err != nil {
					data.Failed = err.Error()
					break
				}
				data.Result = fmt.Sprintf("account %q created (id %d). Log in with the client now - it will ask you for the family name.", member, id)
				data.Member = ""
				log.Printf("console: REGISTER %q -> account %d", member, id)
			}
		}
	}
	if err := registerTmpl.Execute(w, data); err != nil {
		log.Printf("console: register page: %v", err)
	}
}

var registerTmpl = template.Must(template.New("register").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>create an account</title>
<style>
  body { font: 14px system-ui, sans-serif; background:#111; color:#ddd; margin:0; padding:32px; }
  h1 { font-size:18px; font-weight:600; margin:0 0 6px; }
  p.note { color:#999; margin:0 0 22px; max-width:560px; }
  form { display:grid; gap:14px; max-width:360px; }
  label { display:grid; gap:6px; font-size:13px; color:#aaa; }
  input { font: inherit; padding:9px 10px; border-radius:6px; border:1px solid #333; background:#1a1a1a; color:#eee; }
  button { font: inherit; padding:10px 14px; border-radius:6px; border:1px solid #3a6ea5; background:#2a4d70; color:#fff; cursor:pointer; }
  button:hover { background:#33597f; }
  .ok { border-left:3px solid #3f8f4f; background:#16241a; padding:10px 12px; margin:0 0 18px; max-width:560px; }
  .bad { border-left:3px solid #b4453c; background:#2a1614; padding:10px 12px; margin:0 0 18px; max-width:560px; }
</style></head><body>
<h1>create an account</h1>
<p class="note">The account is the login only. Your family name and your first character are
created in the game's barrack, on the screen the client shows you after logging in.</p>
{{if .Result}}<div class="ok">{{.Result}}</div>{{end}}
{{if .Failed}}<div class="bad">{{.Failed}}</div>{{end}}
<form method="post" action="/register">
  <label>account name
    <input name="member_id" value="{{.Member}}" maxlength="64" autofocus required>
  </label>
  <label>password
    <input name="password" type="password" required>
  </label>
  <button type="submit">create account</button>
</form>
</body></html>`))
