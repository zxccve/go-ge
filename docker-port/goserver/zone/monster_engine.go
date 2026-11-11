// THE STATE BEHIND THE BATTLE SCRIPT'S READ-ONLY CALLS: a body's tactic state, the
// AI row its `Tactics` names, and the point it spawned at.
//
// WHY THIS FILE EXISTS. `script\monster_engine.go` binds the five questions
// `ge\script\monster\mon_ts_battle.scp` asks before it decides anything - what AI row
// this body runs (:11-18), how far a body is (:185, :197), where it spawned (:250-251),
// and which tactics state it is in (ChangeTactics, :158..:217). This file is the
// WORLD those answers come from: it stores the one thing the script TELLS the engine
// (the tactics state) and reads the three it ASKS for. It decides nothing - no tactic
// is chosen here, no range is compared, and nothing is sent.
//
// THE TWO TACTICS STRINGS, AND WHY CONFUSING THEM WOULD BE A REAL BUG:
//
//	`Monster.Tactics`        the IES row's `Tactics` COLUMN - the AI PROFILE NAME
//	                         (`MON_AGGRESSIVE`), assigned at spawn from
//	                         `datatable_monster.xml` (create.go) exactly as the
//	                         reference does (`pIesObj->Set(StrID::Tactics, tactics)`,
//	                         MonsterGen.cpp:289, World.cpp:840/980/1280)
//	`monsterEngineState.tactic` the FSM TACTICS STATE - `TS_NONE`, `TS_BATTLE`, ...
//	                         (`PrevTacticsState = TS_NONE; SetTacticsState(TS_NONE)` at
//	                         creation, MonsterNPC.cpp:251-256), which `ChangeTactics`
//	                         moves
//
// They are two different fields in the reference, used in ONE statement there
// (`TS_NONE == GetTacticsState()` beside `imcIES::GetPropertyString(MonsterAi,
// GetTactics(), ...)`, MonsterNPC.cpp:691-699), which is the proof. `GetMonAiOption`
// reads the ROW NAME (`imcIES::GetClass(MonsterAi, obj->GetTactics())`,
// MonsterNPCFunc.cpp:843), so a `ChangeTactics` does NOT change which AI row answers.
//
// THE TACTICS STATE NAMES ARE THE STATE TABLE'S, NOT A LIST THIS FILE INVENTED.
// `ChangeTactics` sends `OM_SET_TACTICS_STATE` and the handler asks
// `GetStateTable()->GetState(name)`; an undeclared name is `INVALID_STATE` and the
// call RETURNS without setting anything (FieldChar_Message.cpp:608-616). The names
// the table declares for a monster's Tactics group are exactly the TS_ entries of
// `enum STATE` (State.h:10-23) - CFieldChar declares NONE/BATTLE/FLEE/DIALOG
// (FieldChar.h:61-67) and CMonsterNPC adds RALLY1..4 and SIEGE..SIEGE6
// (MonsterNPC.h:29-45). Nothing declares TS_RALLY5..TS_RALLY23, and the shipped
// scripts DO pass those (`ge\script\npc\genpc\monnpc_viron_basic.scp:132` asks for
// `TS_RALLY9`): in the reference they are silent no-ops, and this file refuses them
// the same way, with one log line that names the reason.
//
// THE SPAWN POINT IS `m_createdPos`, SET ONCE WHEN THE BODY ENTERS THE WORLD:
//
//	bool CMonsterNPC::OnEnter(CWorld* world, const WORLDPOS& pos)
//	{
//	    m_createdPos = pos;                 // MonsterNPC.cpp:1409-1414
//	    ...
//	}
//
// and read back by `int CMonsterNPC::GetMyHomeDistance() { return GetDistance(
// m_createdPos, GetPos()); }` and `const WORLDPOS& GetMyHomePosition()`
// (MonsterNPC.cpp:1906-1915). This zone does not have an OnEnter hook a lane may
// call, so the point is captured when the engine first hears about a handle
// (`engineLocked`) and `SetMonsterHome` is the exact hook for the spawn path to call
// when the script is wired. The difference is bounded and named: a body that had
// already chased before the engine first asked would have its LATER point recorded,
// which is why the capture says nothing at spawn and `SetMonsterHome` exists.
//
// THE LOCK IS `spawnMu`, the same short lock `monster_hate.go` takes: the roster, the
// HP ledger, the hate table and this state are one world. A body's state dies with
// the body (`pruneEngineLocked`), and a handle is never reused, so nothing is
// inherited by the next body at the same point.
package zone

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"geserver/script"
)

// The zone IS the world behind the five builtins (script\monster_engine.go). This
// line fails the build, not a script run, if a signature ever drifts.
var _ script.MonsterWorld = (*Handler)(nil)

// tacticsStates is the Tactics group of the reference's state table - the names
// `ChangeTactics` may set, and the ONLY names it may set (see the file header).
//
// QUOTED, name for name: `enum STATE` (State.h:10-23) declares TS_NONE, TS_BATTLE,
// TS_FLEE, TS_DIALOG, TS_RALLY1..TS_RALLY4, TS_SIEGE, TS_SIEGE1..TS_SIEGE6, TS_DEAD.
// `GetState` searches that ONE global table, so a name declared for another group
// (`BS_DEAD`, `AS_ATTACK`) also resolves there - but no shipped monster script passes
// one, and a tactics state that is a body state would be a script bug this file
// refuses to store.
var tacticsStates = map[string]bool{
	"TS_NONE":   true,
	"TS_BATTLE": true,
	"TS_FLEE":   true,
	"TS_DIALOG": true,
	"TS_RALLY1": true,
	"TS_RALLY2": true,
	"TS_RALLY3": true,
	"TS_RALLY4": true,
	"TS_SIEGE":  true,
	"TS_SIEGE1": true,
	"TS_SIEGE2": true,
	"TS_SIEGE3": true,
	"TS_SIEGE4": true,
	"TS_SIEGE5": true,
	"TS_SIEGE6": true,
	"TS_DEAD":   true,
}

// monsterEngineState is what the engine keeps about ONE body for these five calls:
// the tactics state `ChangeTactics` sets and the spawn point `GetMyHome*` reads.
type monsterEngineState struct {
	// tactic is the FSM tactics state name. A body starts at TS_NONE, exactly as
	// `CMonsterNPC::InitFSM` sets it (`SetTacticsState(TS_NONE)`, MonsterNPC.cpp:255).
	tactic string

	// home is `m_createdPos` (see the file header) and homeKnown says the body's
	// spawn point was recorded at all.
	home      script.MonsterHome
	homeKnown bool

	// order is what the battle script last TOLD this body to do (zone\monster_order.go):
	// the walk it was ordered to make, the attack request it was given, and its owner.
	// It starts EMPTY for every body, which is why binding a call changed no live
	// behaviour: an empty order is "the script has not spoken", not "stop".
	order monsterOrder
}

// pruneEngineLocked forgets the state of bodies that are no longer on this field.
// Caller holds spawnMu.
func (h *Handler) pruneEngineLocked() {
	for handle := range h.monsterEngines {
		if !h.bodyOnFieldLocked(handle) {
			delete(h.monsterEngines, handle)
		}
	}
}

// engineLocked answers one body's state, creating it when the body is on the field.
// It answers nil for a handle this zone never spawned, so no call can invent a spawn
// point. Caller holds spawnMu.
func (h *Handler) engineLocked(handle uint32) *monsterEngineState {
	st := h.monsterEngines[handle]
	if st != nil {
		return st
	}
	idx := h.monsterIndexLocked(handle)
	if idx < 0 {
		return nil
	}
	if h.monsterEngines == nil {
		h.monsterEngines = make(map[uint32]*monsterEngineState, 4)
	}
	m := h.spawnedMonster[idx]
	st = &monsterEngineState{tactic: "TS_NONE"}
	st.home = h.monsterHomeAt(m)
	st.homeKnown = true
	h.monsterEngines[handle] = st
	return st
}

// monsterHomeAt is the spawn point this file records when it first hears about a
// body: the roster's point (where the body was put) and the map's own height there.
//
// The HEIGHT is the reference's `obj->GetWorld()->GetHeight(createdPos)`
// (MonsterNPCFunc.cpp:765). It is asked of the same mesh a spawn is checked against.
//
// THE z HINT IS THE BODY'S OWN GROUND HEIGHT, NOT 0 (measured 2026-11-09, btl_auch01):
// `NavNear` needs a 3D point, and a query at z=0 on a map whose ground sits at -6,027
// finds nothing within the 300-unit vertical band, so this function answered "unknown"
// and every later script walk that named the home z (RandomMove, RandomMoveCreatedLocaly)
// was refused - 93 `MONSTER MOVE ... refused` lines in one short run. `Monster.GroundZ`
// is the height the spawn already resolved, so the hint is the same point the body was
// placed at. A zone with no path engine, or a body built by hand (GroundZ 0), still
// answers honestly and says so ONCE.
func (h *Handler) monsterHomeAt(m Monster) script.MonsterHome {
	home := script.MonsterHome{X: m.X, Y: m.Y}
	pos, height, err := h.meshCell(float64(m.X), float64(m.Y), float64(m.GroundZ))
	if err != nil {
		h.reportOnceEngine("home:no-height",
			"zone: TACTICS home height for body 0x%08x at (%d, %d) is unknown (%v) - "+
				"GetMyHomePosition answers z 0 (further bodies are not logged)", m.Handle, m.X, m.Y, err)
		return home
	}
	home.X, home.Y = pos[0], pos[1]
	home.Z = height
	return home
}

// reportOnceEngine is this file's once-per-zone log gate.
func (h *Handler) reportOnceEngine(key, format string, args ...any) {
	if h.engineSaid == nil {
		h.engineSaid = map[string]bool{}
	}
	if h.engineSaid[key] {
		return
	}
	h.engineSaid[key] = true
	if h.Log != nil {
		h.Log.Printf(format, args...)
	}
}

// SetMonsterHome records a body's spawn point EXACTLY, for the spawn path to call as
// soon as the body is in the roster - the reference's `m_createdPos = pos` in
// OnEnter. false means the handle is not a body on this field.
//
// This is the hook that makes the capture exact: `engineLocked`'s fallback takes the
// roster's point the first time anything asks, which is only as good as "nothing has
// moved it yet". Stage 3 (the script runner) calls this at spawn.
func (h *Handler) SetMonsterHome(handle uint32, home script.MonsterHome) bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	if !h.bodyOnFieldLocked(handle) {
		return false
	}
	st := h.engineLocked(handle)
	if st == nil {
		return false
	}
	st.home, st.homeKnown = home, true
	return true
}

// MonsterTactic answers the tactics state this zone holds for a body - the store
// `ChangeTactics` writes. "" means the handle is not a body on this field.
//
// NOT a script name: `GetTactics` is not one of the five bound calls. It exists so a
// test and the stage-3 runner can read what the script set, the same way
// `Handler.TopHate` exposes the hate table.
func (h *Handler) MonsterTactic(handle uint32) string {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.engineLocked(handle)
	if st == nil {
		return ""
	}
	return st.tactic
}

// ChangeTactics is `ChangeTactics(mon, 'TS_X')`: store the body's tactics state, and
// log it once per CHANGE.
//
// THE REFERENCE'S GUARD IS COPIED: a name the state table does not declare leaves the
// standing state alone (`if (state == INVALID_STATE) return;`,
// FieldChar_Message.cpp:609-612). It is not an AI rule - it is what keeps an
// undeclared name from becoming a state nobody can answer.
//
// WHAT THIS DOES NOT DO: it does not start, stop, retarget or speed anything. The
// reference's `IMPL_STATE(TS_BATTLE)` runs the body's script thread
// (MonsterNPC.cpp:2450-2453); wiring that is the card that runs the script, not this
// one.
func (h *Handler) ChangeTactics(mon uint32, tactic string) {
	name := strings.TrimSpace(tactic)
	h.spawnMu.Lock()
	h.pruneEngineLocked()
	st := h.engineLocked(mon)
	if st == nil {
		h.spawnMu.Unlock()
		h.reportOnceEngine("tactic:refused-handle:"+strconv.FormatUint(uint64(mon), 16),
			"zone: TACTICS refused for 0x%08x: that handle is not a body on this field, "+
				"and the reference sends the message to the body's own state machine "+
				"(MonsterNPCFunc.cpp:29)", mon)
		return
	}
	if !tacticsStates[name] {
		h.spawnMu.Unlock()
		h.reportOnceEngine("tactic:refused-name:"+name,
			"zone: TACTICS refused %q: it is not a tactics state name. The reference asks "+
				"GetStateTable()->GetState(name) and RETURNS on INVALID_STATE "+
				"(FieldChar_Message.cpp:609-612); the declared Tactics names are the TS_ "+
				"entries of State.h:10-23. TS_RALLY5..TS_RALLY23 are used by shipped "+
				"scripts and declared nowhere, so they are no-ops there and here", name)
		return
	}
	was := st.tactic
	changed := was != name
	st.tactic = name
	m, _ := h.monsterLocked(mon)
	h.spawnMu.Unlock()
	if !changed {
		return
	}
	if h.Log != nil {
		h.Log.Printf("zone: TACTICS 0x%08x %q -> %s (was %s)",
			mon, h.MonsterName(m), name, was)
	}
}

// monsterLocked answers one monster's roster row. Caller holds spawnMu.
func (h *Handler) monsterLocked(handle uint32) (Monster, bool) {
	idx := h.monsterIndexLocked(handle)
	if idx < 0 {
		return Monster{}, false
	}
	return h.spawnedMonster[idx], true
}

// Home is `GetMyHomePosition`'s two ends: the recorded spawn point, and whether this
// body has one.
func (h *Handler) Home(mon uint32) (script.MonsterHome, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.engineLocked(mon)
	if st == nil || !st.homeKnown {
		return script.MonsterHome{}, false
	}
	return st.home, true
}

// HomeDistance is `CMonsterNPC::GetMyHomeDistance()`:
//
//	int distance = GetDistance(m_createdPos, GetPos());   MonsterNPC.cpp:1906-1910
//	                                    -> PushFloat(distance)   MonsterNPCFunc.cpp:739-740
//
// The FREE `GetDistance(const WORLDPOS&, const WORLDPOS&)` (Common.cpp:39-45) is
// CENTRE TO CENTRE with NO radii, and the result is an INT before it is pushed. That
// is deliberately NOT the same arithmetic as the `GetDistance` builtin, which
// subtracts both collision bodies (FieldCharFunc.cpp:254-258).
func (h *Handler) HomeDistance(mon uint32) (float64, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.engineLocked(mon)
	if st == nil || !st.homeKnown {
		return 0, false
	}
	m, ok := h.monsterLocked(mon)
	if !ok {
		return 0, false
	}
	dx := float64(st.home.X - m.X)
	dy := float64(st.home.Y - m.Y)
	return float64(int(math.Sqrt(dx*dx + dy*dy))), true
}

// bodyPointLocked answers one actor's live point and the HALF-WIDTH the reference's
// GetDistance subtracts for it. Caller holds spawnMu.
//
// The half-width is `GetDomainRadius()` = `m_shapeSize` (FieldChar.cpp:660-663): a
// player is the S body (PCBodyRadius, the same 50 the attack reach uses) and a
// monster is its row's `Size` through shapeBodyRadius (create_tables.go).
func (h *Handler) bodyPointLocked(handle uint32, now time.Time) (x, y, radius float64, ok bool) {
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			c := &mem.Sess.Team[j]
			if c.Handle != handle {
				continue
			}
			pos := h.sessionBodyAt(mem.Sess, j, now)
			return float64(pos[0]), float64(pos[1]), PCBodyRadius, true
		}
	}
	idx := h.monsterIndexLocked(handle)
	if idx < 0 {
		return 0, 0, 0, false
	}
	m := h.spawnedMonster[idx]
	r := h.monsterBodyRadius(m)
	if r <= 0 {
		// A row that names no body the shape table knows has no half-width to
		// subtract. The reference would use 0 here; so does this, and the distance
		// is then the centre distance - visibly the "unknown body" case rather than a
		// guessed width.
		r = 0
	}
	return float64(m.X), float64(m.Y), r, true
}

// Distance is `GetDistance(a, b)` - EDGE TO EDGE, negative when two bodies overlap:
//
//	float vX = float(fromPos.x - toPos.x);
//	float vY = float(fromPos.y - toPos.y);
//	float len = sqrt(vX*vX + vY*vY) - (from->GetDomainRadius() + to->GetDomainRadius());
//
// (FieldCharFunc.cpp:252-258.) Z is not part of it. false means either handle is not
// a body on this field, which is the reference's `PushNull` (:238-243).
func (h *Handler) Distance(a, b uint32) (float64, bool) {
	now := time.Now()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	ax, ay, ar, aok := h.bodyPointLocked(a, now)
	bx, by, br, bok := h.bodyPointLocked(b, now)
	if !aok || !bok {
		return 0, false
	}
	dx, dy := ax-bx, ay-by
	return math.Sqrt(dx*dx+dy*dy) - (ar + br), true
}

// AiOption is `GetMonAiOption(mon, 'AIType')`: one column of the AI row the body's
// `Tactics` NAME resolves, as a string or a number.
//
// THE REFERENCE, line for line (MonsterNPCFunc.cpp:830-862):
//
//	if (NULL == obj || (OT_MONSTERNPC != obj->GetObjType() && OT_PETNPC != obj->GetObjType()))
//	    PushNull();                                        // not a monster or pet
//	iclass = imcIES::GetClass(MonsterAi, obj->GetTactics());   // the ROW, by tactics NAME
//	if (NULL == iclass) PushNull();                        // no such row
//	propertyEntry = iclass->GetPropertyEntry(propertyName);
//	if (NULL == propertyEntry) PushNull();                 // no such column
//	STRING == propertyEntry->type ? PushStringID(...) : PushFloat(...)
//
// so every failure is NULL and the column's own type decides the push. The client's
// XML carries no per-attribute type, so the VALUE decides here: text that parses as
// a number is a float, anything else is a string. Every key the battle script reads
// lands on the right side (`AIType`/`Attack`/`Hatred`/`LtdMinion`/`RunAway`/
// `SelfBomb` are words there; `HateRange`/`HateCount` are numbers).
func (h *Handler) AiOption(mon uint32, key string) (script.MonsterOption, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	m, ok := h.monsterLocked(mon)
	if !ok {
		return script.MonsterOption{}, false
	}
	table := h.monsterAiTable()
	if table == nil {
		return script.MonsterOption{}, false
	}
	tactics := strings.TrimSpace(m.Tactics)
	row, ok := table.rows[tactics]
	if !ok {
		// Logged outside the lock would be tidier, but a missing row is answered
		// ONCE per zone and the line is one write; the lock is held for it here
		// rather than released and re-taken around a log call.
		h.reportOnceEngine("ai:norow:"+tactics,
			"zone: GetMonAiOption answered NULL for body 0x%08x: its Tactics %q names no "+
				"row of datatable_monsterai.xml, and the reference answers NULL for a "+
				"class it cannot get (MonsterNPCFunc.cpp:843-847)", mon, m.Tactics)
		return script.MonsterOption{}, false
	}
	text, ok := row[key]
	if !ok {
		return script.MonsterOption{}, false
	}
	if num, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
		return script.MonsterOption{Num: num}, true
	}
	return script.MonsterOption{Text: text, IsText: true}, true
}

// monsterAiTable answers the AI-row table, loading it once from the client's own xml
// folder.
//
// WHY IT IS LOADED HERE AND NOT IN LoadMonsters: `create_tables.go` keeps the THREE
// columns the chase acts on (AIType, Attack, SeekRange) and is shared with spawn and
// the chase; the battle script asks for five more, and this card's lane owns only
// this file. The file is read with its own decoder because the columns are the
// TABLE's 26 attributes and `readClassRows`' struct names a fixed few.
//
// THE DIR is `XMLDir()` - what main.go uses when `-xml` is not given, which is every
// normal start. A run that overrides `-xml` would read the default folder here, and
// the load is logged with the path it read so that is visible rather than silent.
func (h *Handler) monsterAiTable() *monsterAiTable {
	if h.monsterAi != nil {
		return h.monsterAi
	}
	dir := XMLDir()
	table, err := LoadMonsterAiOptions(dir)
	if err != nil {
		h.monsterAi = nil
		h.reportOnceEngine("ai:load",
			"zone: GetMonAiOption answers NULL: datatable_monsterai.xml in %s is not "+
				"readable (%v) - the reference answers NULL for a class it cannot get "+
				"(MonsterNPCFunc.cpp:843-847)", dir, err)
		return nil
	}
	h.monsterAi = table
	return table
}

// monsterAiTable is `datatable_monsterai.xml` as the script reads it: every row by
// its ClassName (`MON_AGGRESSIVE`), every attribute by the name the script passes.
type monsterAiTable struct {
	rows map[string]map[string]string
	path string
}

// Len is how many rows the table holds, for a test.
func (t *monsterAiTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.rows)
}

// aiXmlRow keeps EVERY attribute of one `<Class>` row, because the columns are the
// script's keys and the script names eight of the file's twenty-six.
type aiXmlRow struct {
	XMLName   xml.Name   `xml:"Class"`
	ClassName string     `xml:"ClassName,attr"`
	Attrs     []xml.Attr `xml:",any,attr"`
}

// LoadMonsterAiOptions reads `datatable_monsterai.xml` from `dir`.
//
// A missing or unreadable file is an error the CALLER logs once: a zone without the
// table answers NULL for every AI option, which is what the reference does when
// `imcIES::GetClass` finds no class.
func LoadMonsterAiOptions(dir string) (*monsterAiTable, error) {
	path := filepath.Join(dir, "datatable_monsterai.xml")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	table := &monsterAiTable{rows: make(map[string]map[string]string, 4096), path: path}
	dec := xml.NewDecoder(f)
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, fmt.Errorf("%s: %w", path, terr)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Class" {
			continue
		}
		var row aiXmlRow
		if derr := dec.DecodeElement(&row, &start); derr != nil {
			return nil, fmt.Errorf("%s: %w", path, derr)
		}
		name := strings.TrimSpace(row.ClassName)
		if name == "" {
			continue
		}
		attrs := make(map[string]string, len(row.Attrs))
		for _, a := range row.Attrs {
			attrs[a.Name.Local] = a.Value
		}
		table.rows[name] = attrs
	}
	if len(table.rows) == 0 {
		return nil, fmt.Errorf("%s holds no readable MonsterAi row", path)
	}
	return table, nil
}
