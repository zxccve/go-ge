package zone

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// THE SIGHT RING - who a session is told about, and when.
//
// WHAT THIS REPLACES. Until now the zone told EVERY session about EVERY player
// (`sendOthers`, zone\players.go) and every monster (`broadcastWorld`,
// zone\create.go): two players 8,000 units apart in one field saw each other.
// The shipped server does not do that; it keeps, PER PLAYER, the set of objects
// it has announced, and on its own update it compares that set with a CIRCLE
// around the player's leader (`CView::Update`, source2\geServer\ZServerLib\
// View.cpp:139 `circleChecker(pc->GetPos(), m_viewRange)`), and then
//
//	never announced      -> SendEntered   (View.cpp:190-200)
//	announced, still in  -> nothing but the relation/update bookkeeping (:223-232)
//	announced, now out   -> SendLeave(OBJ_OUTOFSIGHT) + remember it was seen (:212-222)
//	back after a leave   -> SendReEnter (:196-201), plus SendAppearance only if the
//	                        appearance changed - which is NOT this lane
//
// and the ring NEVER leaves the player's own team (:212-217 `IsTeam` check), even
// when a follower is further away than the radius, and never announces an object
// that is hidden (:107 `IsHide`). Our client has no hidden state for a player or a
// monster, and the town NPCs' hide list is applied when the town is built, so
// there is nothing here to skip for :107 - the rule is kept where it belongs.
//
// THE RADIUS IS THE MAP'S OWN `ViewRange`, not a number chosen here. It is read
// from data\map_table.xml (the same client table the client itself reads,
// source2\geShared\Source\MapTable.cpp:218) once per process - at the zone's START-UP,
// in its own MEMSTAGE stage (Handler.PrimeSight, card t_a593b9a2), never in a broadcast -
// and the row is the one whose ClassID is this zone's map id. `PcViewDist` / `MonViewDist`
// are in the
// SAME rows and are NOT the ring's radius: they are CLIENT DRAW distances and the
// shipped server never reads them (`World.cpp:1886` returns
// `m_mapProperty->viewRange`, which is the `ViewRange` column).
//
// WHAT IS NOT FILTERED HERE, on purpose: the item / trigger / warp / NPC entry
// burst (`BuildEnterLegacyObjects` and the town NPCs). They are objects a client
// needs to draw a town at all, and ring-culling them is a later slice.

// SightRow is ONE map row, reduced to the four numbers the sight ring needs.
//
// THEY ARE TWO RULES, NOT TWO NUMBERS FOR ONE (research\Q6-view-middle-range.md).
// ViewRange is the VISIT circle: what the server tracks, and what it LEAVES an actor by
// (`View.cpp:139`, reason 3). ViewMiddleRange is the SEND radius inside CView::Broadcast()
// (`int cullDistance = m_viewMiddleRange;`, `View.cpp:269`): an actor past it stays in the
// client's world and stops being updated - the freeze between the two. A row with no
// middle column (0) simply has no freeze band, and the ring then behaves as it did before
// the cull (zone\sight.go, sightCulls).
//
// PcViewDist / MonViewDist are the CLIENT's own draw distances - the shipped server never
// reads them (`World.cpp:1886` returns `viewRange`), but they are what the CLIENT draws, and
// that is what decides whether a ring radius is felt as a POP. See sightRadiiFor.
type SightRow struct {
	ViewRange       int32
	ViewMiddleRange int32
	PcViewDist      int32
	MonViewDist     int32
}

// sightMargin is how far outside the CLIENT's draw distance the ring's announce/leave circle
// must sit, so an actor is out of sight BEFORE the server tells the client to drop it.
//
// WHY THIS EXISTS (owner, 2026-11-10: "the actor popping in and out/missing actor etc is the
// biggest issue... let the server ring process just outside that, 5000/6000 so that the actor
// transition is not buggy"). On a map whose ViewRange equals the client's draw distance - the
// towns, map 10/15: ViewRange 4000, PcViewDist 4000 - the server says LEAVE at exactly the
// pixel where the client is still drawing the actor, and with a home link's ~234 ms RTT the
// leave lands while the actor is still on screen: it pops. One second of margin at a run
// (~300 units/s) is ~300 units; 1,000 covers the latency four times over and costs 1.6x the
// area only on maps that were already too tight.
const sightMargin int32 = 1000

// sightUpdateMargin is the lead the UPDATE circle keeps OUTSIDE the client's draw distance.
//
// WHY IT IS SMALLER THAN sightMargin (owner, 2026-11-10: "500 instead of 1000 for the margin
// good or bad?"). The two margins do different jobs. The leave margin must outrun the client's
// own removal by a round trip, so it is generous. The update margin only has to make sure an
// actor is ALREADY MOVING when it becomes drawable - the client needs a few move frames before
// the edge, not a second of them. 500 units is ~1.1-1.7 s of a run at 300-470 u/s, which is
// five to eight reports, and it costs HALF the extra band of 1,000: for a 4,000 draw the band
// grows 12.5% of the circle's area instead of 25%, and the band is the expensive part (one
// packet per step per observer in it). A town's event crowd is all INSIDE both circles anyway,
// so no radius value helps there - what the margin buys is the ordinary case: somebody walking
// into view at the edge, who must arrive walking instead of snapping.
const sightUpdateMargin int32 = 500

// sightRadiiFor is the DECISION, pure: what the ring's two radii must be for one map row.
//
// THE RULE, in the owner's terms: the server's circles must sit OUTSIDE what the client draws.
//
//	leave  = max(ViewRange, clientDraw + sightMargin)
//	update = max(ViewMiddleRange, clientDraw + sightUpdateMargin), never above leave
//
// and a row with no client-draw columns keeps the reference's own numbers exactly (draw 0),
// which is what every existing test and every map without those columns gets.
// sightCapRadius, when > 0, caps BOTH circles at a fixed number of units. It exists to MEASURE
// the distance at which the CLIENT starts accepting an actor at all (2026-11-10: the owner's
// report that a returning player "did not know at all" - announced at 6,000, invisible, and it
// only appeared when it stopped beside the observer). Set it from the zone's environment,
// `GE_SIGHT_CAP=3000`, and every zone the manager starts inherits it; 0 keeps the table's rule.
var sightCapRadius int32

// SetSightCap sets the measurement cap (0 = off). Call it once at startup, before any handler
// exists; the zone's sight state is per handler but the cap is a run-level experiment switch.
func SetSightCap(n int32) {
	if n < 0 {
		n = 0
	}
	sightCapRadius = n
}

func sightRadiiFor(row SightRow) (view, middle int32, ok bool) {
	if row.ViewRange <= 0 {
		return 0, 0, false
	}
	draw := row.PcViewDist
	if row.MonViewDist > draw {
		draw = row.MonViewDist
	}
	view = row.ViewRange
	if draw > 0 && draw+sightMargin > view {
		view = draw + sightMargin
	}
	middle = row.ViewMiddleRange
	if draw > 0 && draw+sightUpdateMargin > middle {
		// NEVER FREEZE SOMETHING THE CLIENT CAN SEE, and never let it SNAP into view either:
		// the update circle starts before the draw edge, so the first frame the client is
		// allowed to draw is already a moving body. ViewMiddleRange 2,500 with a 4,000 draw
		// (map 10, 15) was a band the player could watch: drawn and frozen.
		middle = draw + sightUpdateMargin
	}
	if middle > view {
		middle = view
	}
	// THE MEASUREMENT CAP (GE_SIGHT_CAP): both circles shrink to one number of units, so an
	// announcement lands when the client can accept it. See sightCapRadius below.
	if sightCapRadius > 0 && view > sightCapRadius {
		view = sightCapRadius
		if middle > view {
			middle = view
		}
	}
	return view, middle, true
}

// SightTable is data\map_table.xml reduced to map id -> its two ranges.
type SightTable struct {
	rows map[int]SightRow

	// source is the file the rows came from (an image remembers it: staleness asks whether
	// the loader would read the same file now - see zone\sight_image.go).
	source string
}

// Row answers one map's row, and false when the table has no such map.
func (t *SightTable) Row(mapID int) (SightRow, bool) {
	if t == nil {
		return SightRow{}, false
	}
	r, ok := t.rows[mapID]
	return r, ok
}

// Len is how many map rows the table holds, for the one startup line.
func (t *SightTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rows)
}

// LoadSightTable reads map_table.xml from `path`.
//
// A row whose ViewRange is missing or not a positive number is SKIPPED rather than
// stored as 0: a 0 radius would hide every actor on that map, and "no row" is
// already handled (the ring is then off and the log says so). Guessing a radius is
// the one thing this loader must not do.
func LoadSightTable(path string) (*SightTable, error) {
	t := &SightTable{rows: map[int]SightRow{}, source: path}
	_, err := eachClass(path, func(attrs map[string]string) {
		id, ok := atoi(attrs["ClassID"])
		if !ok || id <= 0 {
			return
		}
		view, ok := atoi(attrs["ViewRange"])
		if !ok || view <= 0 {
			return
		}
		middle, _ := atoi(attrs["ViewMiddleRange"])
		pc, _ := atoi(attrs["PcViewDist"])
		mon, _ := atoi(attrs["MonViewDist"])
		t.rows[id] = SightRow{ViewRange: int32(view), ViewMiddleRange: int32(middle),
			PcViewDist: int32(pc), MonViewDist: int32(mon)}
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// sightTablePaths are where map_table.xml may be, in the order tried.
//
// THE LIST, NOT A FLAG, is what a zone started from `goserver\` and a test started from
// `goserver\zone\` have in common: "..\data" is a zone's own working directory and
// "..\..\data" a test's, and GE_MAP_TABLE overrides both when the file lives elsewhere.
// The load itself happens at the zone's START-UP (Handler.PrimeSight), not on first use.
func sightTablePaths() []string {
	if v := os.Getenv("GE_MAP_TABLE"); v != "" {
		return []string{v}
	}
	return []string{
		filepath.Join("..", "data", "map_table.xml"),
		filepath.Join("..", "..", "data", "map_table.xml"),
		filepath.Join("..", "..", "docker-port", "data", "map_table.xml"),
		filepath.Join("docker-port", "data", "map_table.xml"),
	}
}

// The table is loaded ONCE per process (one process serves one zone, the same rule
// `metrics` follows). sightTableFor is the only reader.
var (
	sightTableMu   sync.Mutex // guards the load, NOT the ring
	sightTableDone bool
	sightTableData *SightTable
	sightTableErr  error

	// sightRadiusOnce / sightRadiusRow are the resolved answer for THIS zone's map,
	// so the tick does not look the row up on every pass. The middle radius is cached
	// beside the ring's radius because sendOthers asks for it on every fan-out.
	sightRadiusMu     sync.Mutex
	sightRadiusDone   bool
	sightRadiusValue  int32
	sightRadiusMiddle int32
	sightRadiusOK     bool
	sightRadiusLogged map[int]bool // maps a missing row was already reported for
)

// sightTableFor answers the table, READING IT on the first call.
//
// THE FIRST CALL IS THE ZONE'S START-UP NOW (card t_a593b9a2): Handler.PrimeSight runs
// it in its own MEMSTAGE stage, before anything can create a body. The lazy branch stays
// because it is the same code and a caller that never primes - a test, or a zone whose
// start-up was skipped - must get an answer rather than a blind ring; what it must not be
// is the normal road. An 81-87 ms `map_table.xml` parse inside the first body's broadcast
// was charged to MEMSTAGE spawner and read by the last two cards as the spawner's cost.
//
// IT IS AN IMAGE NOW (card t_38b6216f): loadSightTableWithImage reads `out\sight_image.bin`
// when it is fresh and parses the XML only when it is missing, stale or corrupt - see
// zone\sight_image.go. The `sightTablePaths()` list is still the list of XML candidates the
// image is checked against, so `GE_MAP_TABLE` keeps working.
func sightTableFor() (*SightTable, error) {
	sightTableMu.Lock()
	defer sightTableMu.Unlock()
	if sightTableDone {
		return sightTableData, sightTableErr
	}
	sightTableDone = true
	t, err := loadSightTableWithImage(sightTablePaths())
	if err != nil {
		sightTableErr = err
		return nil, err
	}
	sightTableData = t
	return t, nil
}

// sightTableLoaded answers whether this process has READ the map table already, without
// reading it. It exists for the start-up test (sight_start_test.go): the rule this card
// pins is about WHEN the load happens, and that is a fact about this flag, not about a
// radius - a radius alone cannot tell a table read at start from one read at the first
// broadcast, because both answer 6000 on map 4063.
func sightTableLoaded() bool {
	sightTableMu.Lock()
	defer sightTableMu.Unlock()
	return sightTableDone
}

// PrimeSight LOADS THE MAP TABLE AND RESOLVES THIS ZONE'S RADII AT START-UP, and it is the
// zone's first call into the ring (main.go, before the start-up tactics and the spawner).
//
// WHY IT EXISTS (card t_a593b9a2). The table used to be read by the first caller that
// needed a radius, which is the first body's broadcast: a full `map_table.xml` parse of
// 81-87 ms on map 4063, ONE call, charged to whichever actor happened to be created first.
// That put it inside `MEMSTAGE spawner` (a measured 176-185 ms of stage, 118 ms of which
// was the parse and the two broadcast log lines), made that line blame the spawner for
// 80+ ms it does not cost, and left every future caller that touches the ring before a
// body exists paying an 85 ms surprise. The read is a fact about the START, so it is paid
// in its own stage there:
//
//	h.StartupTactics(); memStage("startup tactics")
//	h.PrimeSight()        <- the table read, once, and the "the ring is ON" line
//	memStage("sight ring")<- the stage carries it
//	h.StartSpawner(...)
//
// IT IS AN IMAGE NOW (card t_38b6216f). The stage used to be an 85-92 ms `encoding/xml`
// walk of `data\map_table.xml`; it reads `out\sight_image.bin` instead - a 43 KB file of
// five int32 columns per row - and parses the XML only when the image is missing, stale or
// corrupt, so a zone without one is a slow start and never a wrong one. The ring's own
// numbers, the once-per-map line and the unarmed fallbacks are untouched by that: this IS
// sightRadii.
//
// NOTHING ELSE CHANGES: this IS sightRadii, so the radii, the once-per-map log line, the
// unarmed fallbacks for a map with no row and `-sightoff` are untouched. `-sightoff`
// returns BEFORE the table is touched, so a measurement run still skips the read whole.
func (h *Handler) PrimeSight() (view, middle int32, armed bool) {
	return h.sightRadii()
}

// sightRadii answers BOTH radii of this zone's map - the ring's `view` (what it tracks
// and leaves by) and the update cull's `middle` - and whether the ring is armed at all.
//
// NOT ARMED is a legitimate answer and it means TODAY'S BEHAVIOUR: everybody is
// told about everybody. That is the case when the map has no row (an instanced or
// brand-new map), when the table cannot be read, or when the row's ViewRange is 0.
// Hiding actors because a table row is missing would be a server that looks broken
// on a map nobody has data for, which is worse than a wide ring. The cull is off for
// the same reasons, plus one of its own: a row whose middle range is 0 has no band to
// freeze in (sightCulls).
//
// THE LOG LINE IS WRITTEN ONCE per map id, not once per tick: a missing row is a
// fact about this build's data, not about a pass.
func (h *Handler) sightRadii() (view, middle int32, armed bool) {
	// -sightoff (R135c): the ring is INERT for this whole run, so no radius exists
	// and every caller takes its own "not armed" path - the one that was already
	// there for a map with no row (joinZone announces everyone, broadcastWorldAt
	// sends to everyone, sightTick answers nil, sendOthers culls nobody). It is
	// checked BEFORE the cache on purpose: the cache is package-level, and a run
	// that poisoned it with "off" would leave the next zone on this process blind.
	if h.SightOff {
		return 0, 0, false
	}
	sightRadiusMu.Lock()
	defer sightRadiusMu.Unlock()
	if sightRadiusDone {
		return sightRadiusValue, sightRadiusMiddle, sightRadiusOK
	}
	sightRadiusDone = true
	if sightRadiusLogged == nil {
		sightRadiusLogged = map[int]bool{}
	}

	t, err := sightTableFor()
	if err != nil {
		h.Log.Printf("zone: sight: map table not readable (%v) - sending to everyone", err)
		sightRadiusOK = false
		return 0, 0, false
	}
	row, ok := t.Row(h.MapID)
	if !ok || row.ViewRange <= 0 {
		if !sightRadiusLogged[h.MapID] {
			sightRadiusLogged[h.MapID] = true
			h.Log.Printf("zone: sight: no map_table row for map %d - sending to everyone", h.MapID)
		}
		sightRadiusOK = false
		return 0, 0, false
	}
	v, m, good := sightRadiiFor(row)
	if !good {
		if !sightRadiusLogged[h.MapID] {
			sightRadiusLogged[h.MapID] = true
			h.Log.Printf("zone: sight: no map_table row for map %d - sending to everyone", h.MapID)
		}
		sightRadiusOK = false
		return 0, 0, false
	}
	view, middle = v, m
	sightRadiusValue = view
	sightRadiusMiddle = middle
	sightRadiusOK = true
	h.Log.Printf("zone: sight: map %d %s leave %d update %d (table ViewRange %d ViewMiddleRange %d, "+
		"client draws %d/%d - the ring sits %d outside it) - the ring is ON",
		h.MapID, h.MapName, view, middle, row.ViewRange, row.ViewMiddleRange,
		row.PcViewDist, row.MonViewDist, sightMargin)
	return sightRadiusValue, sightRadiusMiddle, true
}

// sightRadius is the ring's own reader: the leave radius and whether the ring is armed.
// It is sightRadii without the middle number, kept because most callers ask only this.
func (h *Handler) sightRadius() (int32, bool) {
	view, _, armed := h.sightRadii()
	return view, armed
}

// sightArmed answers whether this zone's map has a ring at all, WITHOUT the log
// line's cost on every call (sightRadius caches, so this is two loads and a mutex).
//
// IT IS A REASON TO TICK. A session standing still with nothing to walk and nothing
// to cast would otherwise let tickIfDue answer false for ever, and the ring would
// only run when the client happened to send a frame - which is exactly the starvation
// R95 item 4 removed for the swing/cast clocks. See zone\zone.go's read deadline and
// zone\chase.go's tickIfDue.
func (h *Handler) sightArmed() bool {
	_, ok := h.sightRadius()
	return ok
}

// sightMaxTracksPerTick caps how many announcements ONE session's tick may build.
//
// IT IS A SAFETY VALVE, NOT A RULE, and 1e9 turns it off. A session standing in the
// middle of 300 players with a 6,000-unit ring builds a few hundred packets in one
// pass; the cap exists so a future registry bug (a monster list that never shrinks)
// cannot turn one tick into a wall of packets. It is a package variable so a test
// can shrink it.
var sightMaxTracksPerTick = 1000000

// sightKind is the OBJECT TYPE of one ring entry, and it selects BOTH the packet
// and the frame size. It is a closed enum on purpose: a new object type in the
// registry must FAIL LOUDLY (sightAnnounce's default branch) rather than fall
// through to a player's packet, which the client would read as a character made of
// a monster's bytes.
type sightKind uint8

const (
	sightKindPC      sightKind = 1 // a player character: 3520 spawn on FIRST sight, 3522/3524 on a RETURN
	sightKindMonster sightKind = 2 // a monster: 3527 enter on FIRST sight, 3528 on a RETURN
)

func (k sightKind) String() string {
	switch k {
	case sightKindPC:
		return "PC"
	case sightKindMonster:
		return "monster"
	}
	return "UNKNOWN"
}

// sightState is ONE session's memory of what it has been told about.
//
// seen is "announced and still inside the circle"; gone is "announced, then left by
// RANGE" - which is what makes the next sight of that object the RETURN path, and that
// path is the client's own re-enter frame (3522 standing / 3524 moving for a player,
// 3528 for a monster), never the spawn burst. An object that leaves the ZONE (a logout, a
// death, a killed monster) is dropped from BOTH maps by the tick that finds it missing
// from the zone, so its own leave packet is the only one the client ever sees.
type sightState struct {
	mu   sync.Mutex
	seen map[uint32]sightKind
	gone map[uint32]sightKind

	// visible, vis and leaving are REUSED by every pass, because a pass runs
	// pollInterval times per second for every session in the zone. A fresh map per
	// pass at 300 players is ~3,000 maps a second of ~300 entries each, which is
	// garbage the collector then has to walk - and the walk shows up in the client's
	// tail, not in this file. They are only ever touched under mu.
	visible []sightItem
	vis     map[uint32]struct{}
	leaving []uint32
}

// sightStates is the per-session state, keyed by the session.
//
// WHY A PACKAGE MAP AND NOT A Session FIELD: the seen set is written by ANOTHER
// session's goroutine too (joinZone announces the newcomer to the players already
// there, and CreateTmonster announces a new body to everybody), so it needs its own
// lock and an owner that outlives any one call. The entry is dropped when the
// session leaves the zone (dropSight).
var sightStates struct {
	mu sync.Mutex
	m  map[*Session]*sightState
	// departed holds the handles that LEFT THE ZONE (a logout, a death a later slice
	// removes, a killed monster). A ring pass that still lists one of them must
	// FORGET it in silence: the leave packet for that handle has already gone out
	// with its own reason (0 - "gone", not "out of sight"), and a second 3525 for a
	// handle the client has removed is a message about an actor that no longer
	// exists. Entries are written and read while the per-session state's own mutex is
	// held (see leaveZone and sightTick), so the check and the forget cannot straddle
	// the departure.
	departed map[uint32]bool
}

func sightStateOf(sess *Session) *sightState {
	sightStates.mu.Lock()
	defer sightStates.mu.Unlock()
	if sightStates.m == nil {
		sightStates.m = map[*Session]*sightState{}
	}
	st := sightStates.m[sess]
	if st == nil {
		st = &sightState{seen: map[uint32]sightKind{}, gone: map[uint32]sightKind{}}
		sightStates.m[sess] = st
	}
	return st
}

// dropSight forgets everything ONE session had been told, and is called when that
// session leaves the zone. Nothing else has to happen: the client's world died with
// the connection, and a session that comes back is a NEW session with a new set.
func dropSight(sess *Session) {
	sightStates.mu.Lock()
	delete(sightStates.m, sess)
	sightStates.mu.Unlock()
}

// sightDeparted records that `handle` has left the ZONE, so no ring pass sends a
// range leave for it after the leave that already went out. It is taken under the
// per-session state's mutex by every caller, which is what makes the check in
// sightTick race-free against leaveZone.
func sightDeparted(handle uint32) {
	if handle == 0 {
		return
	}
	sightStates.mu.Lock()
	if sightStates.departed == nil {
		sightStates.departed = map[uint32]bool{}
	}
	sightStates.departed[handle] = true
	sightStates.mu.Unlock()
}

// sightHasDeparted answers whether `handle` left the zone. Read while the caller
// holds a per-session state's mutex (the same one sightDeparted is called under).
func sightHasDeparted(handle uint32) bool {
	sightStates.mu.Lock()
	defer sightStates.mu.Unlock()
	return sightStates.departed[handle]
}

// sightClearDeparted drops the departure note for a handle that is BACK (a fresh
// enter has just been sent, so the old leave no longer describes the world).
func sightClearDeparted(handle uint32) {
	sightStates.mu.Lock()
	delete(sightStates.departed, handle)
	sightStates.mu.Unlock()
}

// sightMarkSeen records that `handle` has been announced to `sess` by SOMETHING
// OTHER than the ring (the entry burst, a CreateTmonster broadcast). Without it the
// ring's next pass would send a second enter form for an actor the client already
// has.
func sightMarkSeen(sess *Session, handle uint32, kind sightKind) {
	if sess == nil || handle == 0 {
		return
	}
	st := sightStateOf(sess)
	st.mu.Lock()
	if _, ok := st.seen[handle]; !ok {
		st.seen[handle] = kind
	}
	delete(st.gone, handle)
	st.mu.Unlock()
	// A fresh announcement of this handle means the old departure note is history.
	sightClearDeparted(handle)
}

// sightItem is one actor the ring can see, with everything needed to announce it.
type sightItem struct {
	handle uint32
	kind   sightKind
	x, y   int32

	// PC
	owner *Session
	char  Character
	pos   [3]int32

	// leg is the leg the character's body was drawn on when this pass measured it,
	// and at is THAT pass's clock (one `now` for the whole pass, fact #229). A return
	// needs both: a body mid-walk is re-entered with 3524, and its start tick is the
	// leg's own clock, not the moment the packet is built.
	leg swingLeg
	at  time.Time

	// monster
	mon Monster
}

// inRing is the ONE distance test of this file, and it is `distSq` from
// zone\near.go (X and Y only, no height) with the STRICT "<" the shipped checker
// uses (geCircleChecker.cpp:19-23: `distanceSq < m_fSqRange`, so an object standing
// EXACTLY on the radius is NOT in it).
func inRing(from [3]int32, toX, toY int32, radius int32) bool {
	if radius <= 0 {
		return true // ring off
	}
	r := float64(radius)
	return distSq(from[0], from[1], toX, toY) < r*r
}

// THE INNER RADIUS: BETWEEN ViewMiddleRange AND ViewRange AN ACTOR IS FROZEN, NOT GONE.
//
// THE REFERENCE HAS TWO RADII AND WE HAD DEPLOYED ONE (research\Q6-view-middle-range.md).
// CView::Update visits inside m_viewRange (`View.cpp:139`), so an actor past 6,000 on map
// 1331 is LEFT with reason 3 - that is the ring, and it is unchanged. Inside
// CView::Broadcast() a SECOND check (`int cullDistance = m_viewMiddleRange;`, `View.cpp:269`)
// sets the object's culled flag, which suppresses its pending broadcasts. So between 4,000
// and 6,000 the actor is still in the client's world and simply stops being updated: no
// 3548, no 3549, no swing - and NO 3525, because nothing was left.
//
// THE COMPARISON IS TWO HALVES, both the shipped ones: past the middle is STRICTLY greater
// (an actor exactly `ViewMiddleRange` away is still updated), and "still inside" is the same
// strict "<" as inRing - past `ViewRange` the LEAVE, not this, is the rule, so the cull
// never claims a case the leave owns.
//
// NO BAND ANSWERS "DO NOT CULL": a row with no middle column (0), a map with no row, and
// -sightoff all fall through to today's fan-out. Guessing a band would freeze a map nobody
// has data for.
func sightCulls(centre [3]int32, x, y, middle, view int32) bool {
	if middle <= 0 || view <= middle {
		return false // no band to freeze in
	}
	d2 := distSq(centre[0], centre[1], x, y)
	m, v := float64(middle), float64(view)
	return d2 > m*m && d2 < v*v
}

// cullActorAt answers where the actor a fan-out is about is DRAWN at `now`, and false when
// the session does not own `handle`.
//
// THE HANDLE IS RESOLVED THROUGH THE ACTOR'S OWN SESSION because that session is the one
// that just reported the move, so its leg is the freshest answer - the same rule and the
// same reader (sessionBodyAt) the ring's pass uses for every other body.
//
// A MONSTER HAS NO OWNING SESSION, and since the monster swing slice its attacks travel
// through the SAME fan-out. Its body is the standing point this zone spawned it at (a
// monster does not move in this slice), so that is what the cull measures - without this
// branch `sendOthers` would answer "no cull" for every monster packet and a viewer past
// ViewMiddleRange would receive a swing the reference drops (history\monster-attack.md
// "the trap").
//
// A FALSE ANSWER STILL MEANS "NO CULL", i.e. today's fan-out byte for byte: a handle that
// is neither in the actor's team nor a monster this zone runs is a wiring change, and
// inventing a body for it would be a silent freeze on the wrong point.
func (h *Handler) cullActorAt(sess *Session, handle uint32, now time.Time) ([3]int32, bool) {
	if handle == 0 {
		return [3]int32{}, false
	}
	if sess != nil {
		for j := range sess.Team {
			if sess.Team[j].Handle == handle {
				return h.sessionBodyAt(sess, j, now), true
			}
		}
	}
	if m, ok := h.monsterByHandle(handle); ok {
		return [3]int32{m.X, m.Y, m.Cell}, true
	}
	return [3]int32{}, false
}

// THE RING MEASURES THE BODY, NEVER THE DESTINATION (R135d, the owner's own report
// 2026-11-06): "1 family with 3 character is off the screen from another family. A will
// instantly [vanish from] B when B started moving although B havent reach the
// destination yet."
//
// WHY IT HAPPENED. The roster's Pos entry is the DESTINATION a CZ_MOVE_PC named
// (zone\move.go, Players.SetPos; fact #231), not where the body stands. The pass used
// to centre on `sess.Pos` and compare against `m.Pos[j]`, so the tick AFTER B ordered a
// walk past the ring already measured B at the far end of it, and A was told "gone from
// sight" while B's body was still standing beside A. The viewer had the same fault: A's
// own centre jumped to its destination the moment A clicked.
//
// THE BODY'S LIVE POINT IS ALREADY MODELLED, and these two helpers are the whole fix.
// A leg is where the body is drawn (zone\attack_position.go: swingLegOf's comment on
// the two segments, and legPoint as THE ONE arithmetic): the walk THIS SERVER sent, or
// the segment the CLIENT's own last report opened, read at `now`. legPoint is a one-line
// reader of walkedRouteAt (zone\move.go:504) - the ONE function that walks a body along
// its leg's planned route, or along the chord when the leg has none, clamped at both
// ends - and that is why every character here is measured through it: a second
// arithmetic would be a second answer to "where is this body". A leg with
// `placed: true` has no clock and its point is the stored one, exactly as before.
//
// WHAT IS UNCHANGED: the radius, the strict "<", the leave reason 3, the return burst,
// the own-team rule, the seen/gone sets, the entry burst, -sightoff and -diag-distance.
// Only the two POINTS a pass compares changed - and both are read at ONE instant.
//
// bodyPointAt answers where member m's character j is DRAWN at `now`.
//
// THE SPEED IS THE CHARACTER'S OWN, and it is the source the swing already passes for
// that character: the MSPD the move packet carried (Handler.packetSpeed), which is what
// the client animates at. It is NOT Session.walkSpeed: that is a per-SESSION measurement
// of how fast the player re-clicks destinations, and move.go's own header records it
// reading 800 while the actor animated at 469 - a body interpolated at 800 is a body
// drawn up to 331 units ahead of the screen after one second. `speed` is passed in only
// for a CLIENT leg (clientLegOf takes it as an argument); a walk this server sent
// carries its own clock already, which swingLegOf prefers.
func (h *Handler) bodyPointAt(m *Member, j int, now time.Time) [3]int32 {
	_, pos := h.bodyLegAt(m, j, now)
	return pos
}

// bodyLegAt is bodyPointAt's ONE reader, and it answers the LEG as well as the point:
// the return announcement needs the leg itself (its start, its end, its own start
// clock and its speed) at the SAME instant the ring measured the body, and asking
// swingLegOf a second time later in the pass could answer a leg that changed in
// between. One leg, one clock, one reading (R111) - the same rule the swing follows.
//
// A body with no leg answers `placed`, which is exactly "no leg" for every caller:
// the point is the stored one and there is nothing to animate from.
func (h *Handler) bodyLegAt(m *Member, j int, now time.Time) (swingLeg, [3]int32) {
	if m == nil || j < 0 || j >= len(m.Pos) {
		return swingLeg{placed: true}, [3]int32{}
	}
	if m.Sess == nil {
		return swingLeg{from: m.Pos[j], dest: m.Pos[j], placed: true}, m.Pos[j]
	}
	leg := m.Sess.swingLegOf(j, 0)
	if leg.placed {
		// NO LEG, NO CLOCK: the body STANDS on the stored point, and the roster's entry
		// for it is that point - exactly what this pass read before R135d.
		return leg, m.Pos[j]
	}
	if leg.speed <= 0 {
		leg = m.Sess.swingLegOf(j, h.packetSpeed(m.charAt(j)))
	}
	return leg, m.Sess.legPoint(leg, now)
}

// charAt is the roster's copy of character j of this member, or nil (packetSpeed
// answers the formula's base for nil).
func (m *Member) charAt(j int) *Character {
	if m == nil || j < 0 || j >= len(m.Chars) {
		return nil
	}
	return &m.Chars[j]
}

// sightCentre answers the point a session's ring is drawn around at `now`: the BODY of
// the character that session drives, through the same swingLegOf/legPoint pair every
// other character is measured by.
//
// IT IS NOT `sess.Pos` ANY MORE. Pos is the click target the client last reported, so
// centring on it moved the viewer's own circle to its destination the moment it clicked
// - the mirror of the reported bug.
//
// IT IS NOT `Team[0]` EITHER: the character the player drives is Session.Control, and
// reading slot 0 here is how a follower-as-leader would disagree with the swing about
// where the same body is.
func (h *Handler) sightCentre(sess *Session, now time.Time) [3]int32 {
	idx := sess.Control
	if idx < 0 || idx >= len(sess.Team) {
		idx = 0 // a session that never switched drives slot 0 (zone\control.go)
	}
	return h.sessionBodyAt(sess, idx, now)
}

// sessionBodyAt is where character `idx` of a session is DRAWN at `now`, and it is the
// ONE reader of swingLegOf + legPoint that both the ring's centre and the update cull's
// actor go through (zone\sight.go, sightCentre and cullActorAt).
//
// IT IS NOT `sess.Pos`: Pos is the DESTINATION the last CZ_MOVE_PC named (fact #231), and
// a body read from it sits at the far end of its walk the instant the client clicks - the
// pop R135d removed. A placed body answers its stored point (Session.Pos for the driven
// character, Team[i].Position for a follower with no leg), exactly as clientLegOf does.
func (h *Handler) sessionBodyAt(sess *Session, idx int, now time.Time) [3]int32 {
	if sess == nil {
		return [3]int32{}
	}
	if idx < 0 || idx >= len(sess.Team) {
		idx = 0
	}
	leg := sess.swingLegOf(idx, 0)
	if leg.placed {
		// A placed body answers its stored point - Session.Pos for the driven character,
		// which is what the old `leader := sess.Pos` read. Nothing moves.
		return sess.legPoint(leg, now)
	}
	if leg.speed <= 0 {
		leg = sess.swingLegOf(idx, h.packetSpeed(&sess.Team[idx]))
	}
	return sess.legPoint(leg, now)
}

// sightTick is ONE session's pass of the ring, and it answers the packets that pass
// owes that session.
//
// IT RIDES THE SESSION'S OWN CLOCK (tickIfDue, zone\chase.go) - there is no second
// timer and no goroutine per session. The packets are RETURNED, not written: the
// caller hands them to the session's own writer as one batch, so nothing here writes
// to a socket and nothing here is sent while a lock is held.
func (h *Handler) sightTick(sess *Session) [][]byte {
	// -sightoff (R135c): the ring is INERT. Nil at the first line means nothing is
	// ever entered and nothing is ever left by range this run - the entry burst in
	// joinZone and the leave on logout are the whole announcement, as before R135.
	if h.SightOff {
		return nil
	}
	radius, armed := h.sightRadius()
	if !armed {
		return nil
	}
	if sess == nil || len(sess.Team) == 0 {
		return nil
	}

	// THE TWO SNAPSHOTS ARE TAKEN BEFORE ANY RING LOCK, and they are COPIES:
	// Players.Snapshot takes the roster's RLock and releases it, runtimeMonsters
	// takes spawnMu and releases it. Session B may be joining, leaving or spawning
	// while session A's tick runs - those are the locks that make that safe, and
	// neither is held while a packet is built or sent.
	members := h.Players.Snapshot()
	mons := h.runtimeMonsters()

	me := -1
	for i := range members {
		if members[i].Sess == sess {
			me = i
			break
		}
	}
	if me < 0 {
		return nil // not in the world yet: nothing was announced, nothing to cull
	}
	// ONE CLOCK FOR THE WHOLE PASS (fact #229): the centre and every body it is compared
	// against are read at THIS instant, so one pass is one measurement of one world
	// state. A second time.Now() further down would compare a body from one moment with
	// a centre from another, which is the drift this fact exists to forbid.
	now := time.Now()
	centre := h.sightCentre(sess, now)

	st := sightStateOf(sess)
	st.mu.Lock()
	if st.vis == nil {
		st.vis = map[uint32]struct{}{}
	}
	st.visible = st.visible[:0]
	for handle := range st.vis {
		delete(st.vis, handle)
	}

	for i := range members {
		m := &members[i]
		for j := range m.Chars {
			c := m.Chars[j]
			if m.Sess == sess {
				// THE SESSION'S OWN CHARACTERS ARE NEVER RINGED (View.cpp:212-217).
				// They are drawn from START_INFO, not from an announcement, so they
				// are neither entered nor left by this pass - even a follower left
				// 5,000 units behind.
				continue
			}
			// THE POINT IS THE BODY'S, and the entry burst is told the same point: a
			// character announced while walking must be SPAWNED where it is drawn, not
			// at the destination it has not reached (the same trap, one packet later).
			// THE LEG TRAVELS WITH IT, read at this pass's `now`, because a RETURN of a
			// walking body is the moving re-enter frame and it carries that leg.
			leg, body := h.bodyLegAt(m, j, now)
			if !inRing(centre, body[0], body[1], radius) {
				continue
			}
			st.visible = append(st.visible, sightItem{
				handle: c.Handle, kind: sightKindPC,
				x: body[0], y: body[1],
				owner: m.Sess, char: c, pos: body,
				leg: leg, at: now,
			})
			st.vis[c.Handle] = struct{}{}
		}
	}
	for i := range mons {
		m := mons[i]
		if !inRing(centre, m.X, m.Y, radius) {
			continue
		}
		st.visible = append(st.visible, sightItem{
			handle: m.Handle, kind: sightKindMonster,
			x: m.X, y: m.Y, mon: m,
		})
		st.vis[m.Handle] = struct{}{}
	}

	var out [][]byte
	for _, it := range st.visible {
		if _, ok := st.seen[it.handle]; ok {
			continue // inside and already announced: nothing this lane owes
		}
		_, back := st.gone[it.handle]
		pkts := h.sightAnnounce(it, back)
		if len(pkts) == 0 {
			continue // a refusal has already been logged by sightAnnounce
		}
		for _, p := range pkts {
			if len(out) >= sightMaxTracksPerTick {
				break
			}
			out = append(out, p)
		}
		st.seen[it.handle] = it.kind
		delete(st.gone, it.handle)
	}

	// THE LEAVES, in handle order so a log or a test reads the same twice. A handle
	// that is no longer in the ZONE is dropped in silence - see the comment on
	// sightState.gone, and leaveZone (zone\players.go) which has already sent the
	// only leave that client should see.
	st.leaving = st.leaving[:0]
	for handle := range st.seen {
		if _, ok := st.vis[handle]; ok {
			continue
		}
		st.leaving = append(st.leaving, handle)
	}
	sort.Slice(st.leaving, func(i, j int) bool { return st.leaving[i] < st.leaving[j] })
	for _, handle := range st.leaving {
		// A HANDLE THAT IS NO LONGER IN THE ZONE is forgotten in silence: its own
		// leave (reason 0) has gone out already. The liveness scan only runs for a
		// handle that has gone out of RANGE, which is the rare case - a crowd within
		// the ring never reaches it.
		if !sightInZone(members, mons, handle) || sightHasDeparted(handle) {
			delete(st.seen, handle)
			delete(st.gone, handle)
			continue
		}
		kind := st.seen[handle]
		if pkt := h.sightLeave(kind, handle); pkt != nil && len(out) < sightMaxTracksPerTick {
			out = append(out, pkt)
		}
		delete(st.seen, handle)
		st.gone[handle] = kind
	}
	st.mu.Unlock()
	return out
}

// sightInZone answers whether `handle` is an object this zone is running, from the
// snapshots a pass already took. It is only called for a handle that left the RING,
// so a crowd inside the ring never pays for it.
func sightInZone(members []Member, mons []Monster, handle uint32) bool {
	for i := range members {
		for j := range members[i].Chars {
			if members[i].Chars[j].Handle == handle {
				return true
			}
		}
	}
	for i := range mons {
		if mons[i].Handle == handle {
			return true
		}
	}
	return false
}

// sightLegLive answers whether a body is STILL WALKING on the leg the pass measured: it
// has a leg at all, and that leg's own clock has not run out at `now`.
//
// BOTH HALVES ARE THE EXISTING ARITHMETIC - legElapsed and legOwnTime
// (zone\attack_position.go), the pair every other reader of a leg uses. A second
// "has it arrived?" would be a second answer to the same question, and the two would
// disagree on exactly the frame the client is told to animate. A leg whose own walk time
// is zero (from == dest) is answered as arrived, which is what it is.
func sightLegLive(leg swingLeg, now time.Time) bool {
	if leg.placed || leg.at.IsZero() {
		return false
	}
	return legElapsed(leg, now) < legOwnTime(leg)
}

// sightAnnounce builds the announcement for one object: the spawn/enter form for a FIRST
// sight, the client's OWN re-enter form for a RETURN.
//
// WHY THE RETURN IS NOT THE SPAWN ANY MORE (the owner's report, two clients, 2026-11-06
// 21:50): "the character wont show up on the observer client until the other character
// moves once more ... like the character is spawning from barrack/teleport/warping". A
// 3520 for a handle the client already knows is IGNORED - the actor is re-attached only on
// its next 3548, i.e. the moment it moves. The client's own answer to "this character is
// in sight again" is its re-enter frame, and both byte tables are MEASURED from the 2020
// client: docker-port\research\reenter-layout.md sections 2 and 3, read at
// out\ge_decompiled.txt:181119-181148 (3522) and :180946-180985 (3524).
//
// SO:
//
//	first sight, PC       3520 spawn + 3572 skill-disable + 3554 stance (announceChars)
//	return, PC walking    3524 (ZC_RE_ENTER_MOVINGPC) - start/end/tick/speed of its leg
//	return, PC standing   3522 (ZC_RE_ENTER_STANDINGPC) - the body's point, posture 0
//	first sight, monster  3527 enter
//	return, monster       3528 (ZC_RE_ENTER_STANDINGMONSTER) - monsters do not walk yet
//
// A RETURN NEVER SENDS THE 3520 BURST, and that is the one behaviour this function exists
// for. The leave keeps its own path (sightLeave, reason 3).
//
// THE FORM IS CHOSEN BY THE LEG THE PASS MEASURED, not by a second look at the session:
// `it.leg` and `it.at` are the leg and the clock the ring read when it decided this body
// was inside the circle (sightItem). A leg read again here could be a different leg, and
// the frame would then animate a segment the client was never told about.
func (h *Handler) sightAnnounce(it sightItem, back bool) [][]byte {
	switch it.kind {
	case sightKindPC:
		// FIRST SIGHT IS A SPAWN; A RETURN IS THE RE-ENTER FORM (2026-11-10, measured in the
		// owner's own game twice). A handle the client still holds is NOT created by a spawn -
		// the client ignores a 3520 for a handle it already has (the owner's 2026-11-06 21:50
		// rule), so a returning actor stayed invisible until it stopped: "the issue is when
		// Arcanine move back into range, the observer client did not draw it". The RE-ENTER
		// form is what re-adds an actor the leave removed.
		//
		// THE MOVING RE-ENTER (3524) IS NOT SENT: its leg is the CLICK's destination (a
		// 9,900-unit leg measured on map 1331) and its tick counts from the server's process
		// start, so the client placed the actor at the destination instead of where it walks.
		// The STANDING form carries the BODY's own point; the actor's next move report (the
		// client re-sends a destination while it walks, ~200 ms) animates it on with 3548.
		if back {
			metrics.sightReenter()
			// THE RE-ENTER IS THREE PACKETS (measured on the live server 2026-11-10, capture
			// ring1): every 3522 it sends is immediately followed by an 11-byte 3572 for the SAME
			// handle (`f4 0d 1a 62 30 00 00 00 00 00 00`). Our first sight also sends a 3554
			// (ZC_CHANGE_STANCE) right after the spawn, and the client builds the model from the
			// stance - so a re-enter that carries only the 3522's stance field leaves it nothing
			// to load when that field is zero. Send the stance too; the log line prints the values
			// we are handing over so a zero is visible instead of guessed at.
			walking := sightLegLive(it.leg, it.at)
			form := "standing"
			if walking {
				form = "moving"
			}
			h.Log.Printf("zone: sight: return handle 0x%08x form=%s+stance+skilldisable stance=%d hp=%.0f maxhp=%.0f job=%d walking=%v at (%d, %d, %d)",
				it.handle, form, it.char.Stance, it.char.HP, it.char.MaxHP, it.char.Job,
				walking, it.pos[0], it.pos[1], it.pos[2])
			// A WALKING BODY IS RE-ENTERED WITH 3524, not 3522 - the live server does exactly
			// that (capture ring1: 3524 for the movers, 3522 only for the standers). The leg
			// it carries is start = the BODY's own point, end = where it is going, tick = the
			// leg's start on the 3548 clock: the three values the client needs to animate the
			// remainder. Sent with the standing form, the client holds a static body and only
			// places it when the walk's STOP lands - the owner's "draws only at the destination".
			if walking {
				return [][]byte{
					BuildReEnterMovingPC(it.char, it.pos, it.leg.dest, millisOf(it.leg.at), it.leg.speed),
					BuildSkillDisable(it.handle),
				}
			}
			return [][]byte{
				BuildReEnterPC(it.char, it.pos),
				BuildChangeStance(it.handle, it.char.Stance),
				BuildSkillDisable(it.handle),
			}
		}
		pkts := h.announceChars(it.owner, []Character{it.char}, [][3]int32{it.pos})
		for range pkts {
			metrics.sightEnter()
		}
		return pkts
	case sightKindMonster:
		if back {
			metrics.sightReenter()
			h.Log.Printf("zone: sight: return handle 0x%08x form=standing monster "+
				"walking=false at (%d, %d, %d)", it.handle, it.mon.X, it.mon.Y, it.mon.Cell)
			return [][]byte{BuildReEnterStandingMonster(it.mon)}
		}
		metrics.sightEnter()
		return [][]byte{BuildEnterStandingMonster(it.mon)}
	}
	// A NEW OBJECT TYPE MUST NOT FALL THROUGH TO A PLAYER PACKET. If this ever fires
	// it is a wiring bug in the registry, not data: say so, name the kind, and send
	// nothing rather than bytes the client will key as the wrong actor.
	h.Log.Printf("zone: sight: BUG: unknown object kind %d for handle 0x%08x - "+
		"nothing is announced (add it to sightKind and sightAnnounce)",
		uint8(it.kind), it.handle)
	return nil
}

// sightLeave builds the LEAVE form for one handle - reason 3, OBJ_OUTOFSIGHT, which
// is the instant-vanish branch the client takes for range culling
// (GENormalNet_World.cpp:188-193). A logout keeps reason 0 and never comes here.
func (h *Handler) sightLeave(kind sightKind, handle uint32) []byte {
	switch kind {
	case sightKindPC:
		metrics.sightLeave()
		return BuildLeavePC(handle, LeaveCodeOutOfSight)
	case sightKindMonster:
		metrics.sightLeave()
		return BuildLeaveMonster(handle, LeaveCodeOutOfSight)
	}
	h.Log.Printf("zone: sight: BUG: unknown object kind %d for handle 0x%08x - "+
		"no leave is sent", uint8(kind), handle)
	return nil
}

// broadcastWorldAt sends ONE world-object packet to the sessions whose OWN ring
// holds (x, y) - the create/kill half of the ring (CreateTNPC, CreateTmonster,
// KillMonsterObject).
//
// markHandle/markKind, when set, record the object as announced in every recipient,
// so the next ring pass does not send it a second enter form. It is 0 for an NPC:
// NPCs are not ringed in this lane (see the file comment), so there is nothing to
// record.
func (h *Handler) broadcastWorldAt(pkt []byte, x, y int32, markHandle uint32, markKind sightKind) int {
	radius, armed := h.sightRadius()
	sent := 0
	for _, m := range h.Players.Snapshot() {
		if armed && !inRing(m.Pos[0], x, y, radius) {
			continue
		}
		if err := m.Sess.send(pkt); err != nil {
			h.Log.Printf("zone: could not tell handle %d about a world object: %v",
				m.Chars[0].Handle, err)
			continue
		}
		if markHandle != 0 {
			sightMarkSeen(m.Sess, markHandle, markKind)
		}
		sent++
	}
	return sent
}

// ---------------- the two RE-ENTER frames ----------------

// millisAt puts a leg's START time on the clock a 3548 carries: nowMillis,
// milliseconds since this zone process started (zone\move.go). The re-enter frame
// carries the moment the leg STARTED, not the moment the packet was built, so the
// client can order the body from the point it began walking.
func millisAt(t time.Time) uint32 {
	ms := t.Sub(zoneStart).Milliseconds()
	if ms < 0 {
		return 0
	}
	return uint32(ms)
}

// CmdZCReEnterPC is ZC_RE_ENTER_STANDINGPC: "this character is in sight again".
//
// IT IS NOT THE SPAWN FRAME WITH THE OPTION COUNT ZEROED. The client's own table
// lists it as a FIXED 305 bytes (out\ge_decompiled.txt:712724 registers 0x131 = 305),
// while the enter form is variable and 705 bytes before its option records - so the
// re-enter carries NO appearance block and NO equipment options.
//
// EVERY OFFSET BELOW IS THE 2020 CLIENT'S OWN READ, not the enter frame shifted:
// docker-port\research\reenter-layout.md section 2, and the handler FUN_004f3d80 at
// out\ge_decompiled.txt:181119-181148. The 20:05 build got this wrong by ONE BYTE
// from +28 on (no posture byte), and the client read our stance's low byte as a
// posture code - a T-posed actor (research doc section 4).
const (
	CmdZCReEnterPC = 3522
	// ReEnterPCSize is the WHOLE frame, from the client's own table.
	ReEnterPCSize = 305

	rpHandle    = 2  // DWORD the handle
	rpRelation  = 6  // BYTE  relation (0 = neutral; the client logs it as "Relation")
	rpClass     = 7  // WORD  the character's job
	rpWeapon    = 9  // WORD  ZERO, the measured value
	rpAnimation = 11 // DWORD the animation/posture dword: ZERO (the measured spawn value)
	rpPos       = 15 // INT32 x, +19 y, +23 the navmesh cell
	rpDirection = 27 // BYTE  direction 0..255 (compass; 0 = ours, see below)
	rpPosture   = 28 // BYTE  posture: 0 = standing. THE BYTE THAT WAS MISSING.
	rpStance    = 29 // DWORD stance
	rpHP        = 33 // FLOAT64 hp
	rpMaxHP     = 41 // FLOAT64 max hp
	rpBuffMask  = 49 // 256 bytes, all zero
	// 49 + 256 = 305: the mask is the last field and the frame ends on the
	// client's registered size. rpLast is that last byte, kept so the sum is written.
	rpLast = 304
)

// BuildReEnterPC builds ZC_RE_ENTER_STANDINGPC, 305 bytes - the body is STANDING, or
// it has arrived, or this zone has no leg for it.
//
//	+0   WORD   3522
//	+2   DWORD  the handle
//	+6   BYTE   relation (0 = neutral)
//	+7   WORD   the character's class/job
//	+9   WORD   the weapon word - ZERO, the measured value
//	+11  DWORD  the animation/posture dword - ZERO, the measured live value
//	+15  INT32  x, +19 y, +23 the navmesh cell
//	+27  BYTE   direction (see DIRECTION below)
//	+28  BYTE   posture: 0 = standing  <- the byte the 20:05 frame did not have
//	+29  DWORD  stance
//	+33  FLOAT64 hp
//	+41  FLOAT64 max hp
//	+49  256 bytes of buff mask, zero
//
// DIRECTION IS 0 AND IT IS NOT A GUESS TO CHANGE: Character has no facing field, and
// this lane may not invent one. 0 is what the measured live spawn carries in the same
// slot (zone\players.go:240); the client keeps the actor's own orientation when it
// already has one (research doc section 8).
//
// POSTURE IS 0 = DefaultPosture (zone\players.go:271): standing. This frame DOES carry
// the posture byte (the moving one does not) - that is the whole fix.
func BuildReEnterPC(c Character, pos [3]int32) []byte {
	b := make([]byte, ReEnterPCSize)
	binary.LittleEndian.PutUint16(b[0:], CmdZCReEnterPC)

	binary.LittleEndian.PutUint32(b[rpHandle:], c.Handle)
	b[rpRelation] = 0
	binary.LittleEndian.PutUint16(b[rpClass:], uint16(c.Job))
	binary.LittleEndian.PutUint16(b[rpWeapon:], 0)
	binary.LittleEndian.PutUint32(b[rpAnimation:], 0)
	binary.LittleEndian.PutUint32(b[rpPos:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(b[rpPos+4:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(b[rpPos+8:], uint32(pos[2]))
	b[rpDirection] = 0
	b[rpPosture] = byte(DefaultPosture)
	binary.LittleEndian.PutUint32(b[rpStance:], c.Stance)
	putFloat64(b[rpHP:], c.HP)
	putFloat64(b[rpMaxHP:], c.MaxHP)
	// b[rpBuffMask:rpLast+1] stays zero. The last byte of the mask IS rpLast.
	return b
}

// CmdZCReEnterMovingPC is ZC_RE_ENTER_MOVINGPC: "this character is in sight again and
// is walking RIGHT NOW".
//
// IT CARRIES NO POSTURE AND NO DIRECTION BYTE, and the handler says so: line 180979 is
// `local_110 = 0;` (posture forced to standing) and nothing loads the direction slot
// (research doc section 3, last paragraph; out\ge_decompiled.txt:180946-180985).
// Writing a byte there would SHIFT stance/hp/mhp/mask and destroy the frame - so do
// not add one.
const (
	CmdZCReEnterMovingPC = 3524
	// ReEnterMovingPCSize is the WHOLE frame, from the client's own table
	// (out\ge_decompiled.txt:712744 registers 0x143 = 323).
	ReEnterMovingPCSize = 323

	rmHandle   = 2  // DWORD the handle
	rmRelation = 6  // BYTE  relation (0 = neutral)
	rmClass    = 7  // WORD  the character's job
	rmWeapon   = 9  // WORD  ZERO, the measured value
	rmAnim     = 11 // DWORD the animation/posture dword: ZERO
	rmTick     = 15 // DWORD the leg's START tick on the 3548 clock (live, capture ring1)
	rmStart    = 19 // INT32 x, +23 y, +27 the navmesh cell: WHERE THE BODY IS
	rmEnd      = 31 // INT32 x, +35 y, +39 the navmesh cell: WHERE IT IS GOING
	rmSpeed    = 43 // FLOAT32 the leg's speed (live, capture ring1)
	rmStance   = 47 // DWORD stance
	rmHP       = 51 // FLOAT64 hp
	rmMaxHP    = 59 // FLOAT64 max hp
	rmBuffMask = 67 // 256 bytes, all zero
	// 67 + 256 = 323 - again the mask is the last field and the frame ends on the
	// client's registered size.
	rmLast = 322
)

// BuildReEnterMovingPC builds ZC_RE_ENTER_MOVINGPC, 323 bytes, for a body that is
// STILL WALKING when it comes back into sight.
//
// start is where the body was when its leg opened (the ring's leg.from) and end is
// where the leg is going (leg.dest) - the order the client itself passes them, and a
// swap walks the actor backwards (research doc section 3). tick is the leg's OWN start
// (millisAt), the same clock every 3548 in this zone carries.
//
// SPEED AT +15 IS THE LEG'S SPEED, WRITTEN AS AN INT. The brief's byte table calls the
// slot "unused, write 0"; the measurement calls it the speed-like scalar the client
// reads as an int and widens to a float (line 180980). Both are true of the CLIENT -
// it stores the value and the movement call it is passed to never reads it - but a
// slot that means "how fast this body is moving" should not say ZERO for a walking
// body, so the leg's own speed goes there and research doc section 8's "0 reproduces
// not moving" stays the meaning of a zero.
func BuildReEnterMovingPC(c Character, start, end [3]int32, tick uint32, speed float64) []byte {
	b := make([]byte, ReEnterMovingPCSize)
	binary.LittleEndian.PutUint16(b[0:], CmdZCReEnterMovingPC)

	binary.LittleEndian.PutUint32(b[rmHandle:], c.Handle)
	b[rmRelation] = 0
	binary.LittleEndian.PutUint16(b[rmClass:], uint16(c.Job))
	binary.LittleEndian.PutUint16(b[rmWeapon:], 0)
	binary.LittleEndian.PutUint32(b[rmAnim:], 0)
	// THE LIVE LAYOUT, MEASURED (capture ring1, 2026-11-10): the TICK sits at +15 and the
	// SPEED is a FLOAT32 at +43. Ours had them the other way round, so the client read our
	// speed as a tick and our tick as a speed - a leg it could not animate, which is why a
	// walking re-entry stayed static until its stop landed.
	binary.LittleEndian.PutUint32(b[rmTick:], tick)
	binary.LittleEndian.PutUint32(b[rmSpeed:], math.Float32bits(float32(speed)))
	binary.LittleEndian.PutUint32(b[rmStart:], uint32(start[0]))
	binary.LittleEndian.PutUint32(b[rmStart+4:], uint32(start[1]))
	binary.LittleEndian.PutUint32(b[rmStart+8:], uint32(start[2]))
	binary.LittleEndian.PutUint32(b[rmEnd:], uint32(end[0]))
	binary.LittleEndian.PutUint32(b[rmEnd+4:], uint32(end[1]))
	binary.LittleEndian.PutUint32(b[rmEnd+8:], uint32(end[2]))
	binary.LittleEndian.PutUint32(b[rmStance:], c.Stance)
	putFloat64(b[rmHP:], c.HP)
	putFloat64(b[rmMaxHP:], c.MaxHP)
	// b[rmBuffMask:rmLast+1] stays zero - the mask is the tail of the frame.
	return b
}

// CmdZCReEnterStandingMonster is ZC_RE_ENTER_STANDINGMONSTER.
//
// The client's table sizes it at 360 - the SAME number as its enter form, 3527
// (data\cmd_table_live_z.txt: `3527 360`, `3528 360`), because both frames carry the
// same field set: handle, pos, direction, relation, hp, buff mask, appearance
// (PKS_ZC_RE_ENTER_STANDINGMONSTER, geZCPacket.h:1106 is `PKS_ZC_ENTER_STANDING` plus
// exactly those).
const (
	CmdZCReEnterStandingMonster = 3528
	ReEnterStandingMonsterSize  = 360
)

// BuildReEnterStandingMonster builds ZC_RE_ENTER_STANDINGMONSTER, 360 bytes - the
// RETURN form for a monster.
//
// ITS BODY IS STILL THE 3527 ENTER BODY, and that is the one honest caveat on this path:
// no 3528 exists in any capture, so where its hp and buff-mask fields sit is NOT measured.
// The two frames are the same size in the client's table (data\cmd_table_live_z.txt:
// `3527 360`, `3528 360`) and geZCPacket.h:1106 defines the re-enter body as
// `PKS_ZC_ENTER_STANDING` plus exactly the handle/pos/direction/relation/hp/mask/appearance
// set, so the fields this lane cannot place are left at the values the captured 3527
// carries (zero) and the one byte that MUST differ - the command id - is the only byte
// written on top.
//
// IT IS SENT NOW (2026-11-06, the sight-return lane): a monster that comes back into the
// ring gets this, because 3527 for a handle the client already has is ignored the same way
// a repeated 3520 is. Monsters do not walk in this server yet, so there is no moving
// monster re-enter (3530) and none must be invented.
func BuildReEnterStandingMonster(m Monster) []byte {
	out := BuildEnterStandingMonster(m)
	binary.LittleEndian.PutUint16(out[0:], CmdZCReEnterStandingMonster)
	return out
}
