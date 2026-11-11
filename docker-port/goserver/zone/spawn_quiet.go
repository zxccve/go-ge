package zone

// WHAT `-quietspawn` SILENCES, AND WHAT IT KEEPS.
//
// THE MEASUREMENT FIRST (card t_1d63a658, map 4063 btl_ktv03, 429 bodies, the
// build of 2026-11-11, the SAME binary in both arms, GE_CORPUS_READY=1, scratch DB):
//
//	MEMSTAGE spawner stage, -quietspawn OFF, 4 runs   176, 180, 182, 185 ms
//	MEMSTAGE spawner stage, -quietspawn ON,  4 runs   163, 171, 178, 182 ms
//	the 858 lines' own cost, timed around them         3, 4, 5, 6, 8 ms
//
// The two arms overlap, which is the answer: the lines are ~5 ms of the ~180 ms this
// stage costs on that map - NOT the ~400 ms the card suspected, and the card's 511 ms
// is not reproducible on this build at all (three runs of the untouched tree: 172,
// 181, 182 ms of stage). What the stage IS measured to be (one instrumented build, not
// committed; the numbers are in lanes\build.md): ~85 ms the sight ring's map_table
// load charged to the first body's broadcast, ~48 ms of path-engine queries for the
// 237 distinct spawn points (cached per point), ~21 ms of the MHP/DEF script calls,
// ~5 ms these lines, ~10 ms listen/announce/FreeOSMemory, and under a millisecond
// for the 429 bodies themselves.
//
// SO THE FLAG IS ABOUT THE LOG, NOT THE CLOCK. 858 of the ~1,046 lines a Katovic
// start writes are these two, and they bury everything else in the file. ON, the
// burst is COUNTED instead of written, and NOTHING is lost:
//
//   - one line per DISTINCT decision - class, tactics, AIType, attack, seek - with
//     its body count and how many cells it landed on, printed when the spawner
//     finishes. That is exactly what the per-body MONSTER AI line carried, 429 lines
//     of it collapsed into one line per row of the table.
//   - a body whose tactics row did NOT resolve, or whose AIType is empty, still
//     prints BOTH full lines: that unresolved tactics row is the failure this line
//     was used to find (`zone: MONSTER AI class ... tactics "" AIType - ...`), and a
//     flag that hid it would be a diagnostic deleted.
//   - bodies created at any OTHER moment - a script's `CreateTmonster`, a probe, a
//     respawn - always print in full. The silencing is scoped to `StartSpawner`
//     (`Handler.spawnerRunning`), because a burst is the only place the volume is.
//
// OFF is the default and it is byte for byte what the zone wrote before this file
// existed.

import (
	"sort"
	"sync"
)

// spawnQuietKey is ONE decision the two per-body lines carried, as a value the
// aggregate can count. Class AND name, because a reader checks the id against the
// name the CREATE line used to print; tactics/AIType/attack/seek are the MONSTER AI
// line's own five numbers.
type spawnQuietKey struct {
	Class   int32
	Name    string
	Tactics string
	AIType  string
	Attack  bool
	Seek    float64
}

// spawnQuietRow is what one key accumulated: how many bodies and where they landed.
// The cell set is bounded by the map's own spawn points (the spawner cycles a
// cursor per division over the flag rows), so it is a handful of numbers, not a list
// that grows with the body count.
type spawnQuietRow struct {
	Bodies int
	Cells  map[int32]struct{}
}

// spawnQuietEntry is one finished aggregate row, sorted for printing.
type spawnQuietEntry struct {
	Key    spawnQuietKey
	Bodies int
	Cells  int
}

// spawnQuietLog is the Handler's counter for the silenced burst. Its own mutex, not
// `spawnMu`: a log line must never be held behind the roster lock (and the reverse).
type spawnQuietLog struct {
	mu    sync.Mutex
	rows  map[spawnQuietKey]*spawnQuietRow
	lines int // how many per-body lines were NOT written (two per body)
}

// add counts one silenced body.
func (q *spawnQuietLog) add(key spawnQuietKey, cell int32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.rows == nil {
		q.rows = make(map[spawnQuietKey]*spawnQuietRow, 8)
	}
	row := q.rows[key]
	if row == nil {
		row = &spawnQuietRow{Cells: map[int32]struct{}{}}
		q.rows[key] = row
	}
	row.Bodies++
	row.Cells[cell] = struct{}{}
	q.lines += 2 // the MONSTER AI line and the CREATE_TMONSTER line
}

// take answers the aggregate - sorted by class, then tactics - and the number of
// lines that were not written, and empties it.
func (q *spawnQuietLog) take() ([]spawnQuietEntry, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]spawnQuietEntry, 0, len(q.rows))
	for key, row := range q.rows {
		out = append(out, spawnQuietEntry{Key: key, Bodies: row.Bodies, Cells: len(row.Cells)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Class != out[j].Key.Class {
			return out[i].Key.Class < out[j].Key.Class
		}
		if out[i].Key.Tactics != out[j].Key.Tactics {
			return out[i].Key.Tactics < out[j].Key.Tactics
		}
		return out[i].Key.Seek < out[j].Key.Seek
	})
	lines := q.lines
	q.rows, q.lines = nil, 0
	return out, lines
}

// quietSpawnFor answers whether THIS body's two lines are to be counted instead of
// written: the flag is on, the call is inside `StartSpawner`'s burst, and the body's
// tactics row RESOLVED. Everything else prints in full (see the file comment).
func (h *Handler) quietSpawnFor(aiKnown bool, aiLabel string) bool {
	if !h.QuietSpawn || !h.spawnerRunning.Load() {
		return false
	}
	return aiKnown && aiLabel != "-"
}

// flushQuietSpawn prints what the flag counted, once, at the end of a spawner run.
// Silence when nothing was counted: a map with no bodies, or a run with the flag
// off, writes nothing.
func (h *Handler) flushQuietSpawn() {
	entries, lines := h.spawnQuiet.take()
	if lines == 0 {
		return
	}
	bodies := 0
	for _, e := range entries {
		bodies += e.Bodies
	}
	h.Log.Printf("zone: spawner: -quietspawn counted %d body(ies) into %d tactics "+
		"decision(s) and wrote none of their %d per-body line(s); a body whose tactics "+
		"row does not resolve, and every body created after this burst, still prints "+
		"in full", bodies, len(entries), lines)
	for _, e := range entries {
		attack := "yes"
		if !e.Key.Attack {
			attack = "no"
		}
		h.Log.Printf("zone: spawner: -quietspawn class %d %q tactics %q AIType %s "+
			"attack %s seek %g: %d body(ies) on %d cell(s)",
			e.Key.Class, e.Key.Name, e.Key.Tactics, e.Key.AIType, attack, e.Key.Seek,
			e.Bodies, e.Cells)
	}
}
