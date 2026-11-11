// THE MONSTER'S THINK TURN: `-monai script`, the game's own tactics script driving a body.
//
// WHY THIS FILE EXISTS. `script\monster_ai.go` is the RUNNER (one coroutine per body,
// resumed at the script's own `sleep`) and stages 1-2 bound every name the battle script
// calls. This file is the ZONE's half: it walks the bodies this zone owns, works out
// which entry the reference's dispatch would start for each (`SCR_<Tactics>_TS_<state>`,
// MonsterNPC.cpp:1116-1118), and hands the body to the runner. It DECIDES NOTHING -
// no target, no tactic, no distance, no timer. The one choice it makes is a documented
// translation of a visit order, in SeekTarget.
//
// WHAT CHANGES ON THE WIRE WHEN IT IS ON: a body walks, attacks and stops because a
// script said so, through the SAME `Move`/`StartAttack`/`StopAttack` builtins stage 2b
// bound - so the packets are the ones the earlier cards measured. What is switched OFF
// is the GO CHASE (`monsterChaseTick`), which is the fallback, not a second opinion: two
// things steering one body would fight. The SWING stays on exactly as it is, because in
// the reference the swing is the FSM's action (`AS_ATTACK`), not a script's - the script
// chooses the target, the engine lands the hit.
//
// WITH THE FLAG OFF (the default) `monsterAITick` returns at its first line and every
// other line in this file is unreachable: the chase, the swing, the packets and the
// tests they already have are byte for byte what they were.
//
// THE STATE A BODY STARTS IN IS TS_NONE, and that is not a convenience: a monster is
// born in TS_NONE (`SetTacticsState(TS_NONE)`, MonsterNPC.cpp:251-256) and the NONE
// entry is the one that ACQUIRES - `GetObjectLocaly(seekRange)` -> `InsertHate` ->
// `ChangeTactics(TS_BATTLE)` (mon_ts_none.scp:219-240). Running only the battle entry
// would need a Go acquisition rule to feed it hate, which is exactly what the owner has
// ruled out; the script's own NONE entry is the acquisition, and the state machine then
// starts the BATTLE thread on its own.
package zone

import (
	"fmt"
	"strings"
	"time"

	"geserver/script"
)

// The zone IS the world behind the think turn's two builtins, and the runner's host is
// what it drives. These two lines fail the BUILD, not a run, if a signature drifts.
var _ script.MonsterSeek = (*Handler)(nil)

// MonsterBrain is the runner side of the think turn (script\Host): run this body's
// tactics thread for the instant `now`, and forget a body that has left the field.
//
// It is an interface for the same reason HateWorld is: this package knows the field and
// that one knows Lua, and a test can stand a recorder in its place.
type MonsterBrain interface {
	ThinkMonster(t script.MonsterThink, now time.Time)
	ForgetMonster(handle uint32)
}

// MonsterAIStartupLine is the ONE line the zone prints about `-monai`, in the house
// style of the other dev flags: it names the mode and says which engine answers a
// body's decisions.
//
// script is the corpus entry shape as well as the switch, so a reader of the log can
// follow one body from the line to the file that ran it.
func MonsterAIStartupLine(scriptMode bool) string {
	if scriptMode {
		return "zone: MONSTER AI script - a living body thinks through the game's OWN " +
			"tactics script: SCR_<Tactics>_TS_<state> (MonsterNPC.cpp:1116-1118), whose " +
			"TS_BATTLE entry is the card's SCR_MON_AI_LIB_TS_BATTLE, run as a thread and " +
			"woken by the script's own sleep (mon_ts_battle.scp:3, :276). The GO CHASE is " +
			"OFF in this mode (the script walks the body); the swing and the packets are the " +
			"same engine. -monai go is the default and is byte-identical to the pre-script run"
	}
	return "zone: MONSTER AI go - the Go chase and the swing decide, and NO monster AI " +
		"script is run (the default; -monai script runs the game's own SCR_MON_AI_LIB_TS_BATTLE)"
}

// monsterAITick is the think pass. It rides `tickIfDue` (chase.go) beside the corpse,
// chase and swing clocks, so there is no second timer and no goroutine.
//
// IT IS OFF BY DEFAULT AND COSTS NOTHING WHEN OFF: one boolean read.
func (h *Handler) monsterAITick(now time.Time) {
	if !h.MonsterAIScript {
		return
	}
	bodies := h.runtimeMonsters()
	if len(bodies) == 0 {
		return
	}
	if h.MonsterBrain == nil {
		h.reportOnceEngine("monai:no-brain",
			"zone: MONSTER AI script is ON but this zone has no script host wired "+
				"(-no-scripts?): no body's tactics script can run, and the Go chase is "+
				"OFF in this mode - nothing will move or attack (further ticks are silent)")
		return
	}

	// ONE pass at a time: the host is a single Lua runtime, and every session's loop
	// can reach this line in the same instant.
	h.monsterAIMu.Lock()
	defer h.monsterAIMu.Unlock()

	live := make(map[uint32]bool, len(bodies))
	for _, m := range bodies {
		live[m.Handle] = true
	}
	for handle := range h.monsterAIBodies {
		if !live[handle] {
			h.MonsterBrain.ForgetMonster(handle)
		}
	}
	h.monsterAIBodies = live

	for _, m := range bodies {
		hp, ok := h.monsterLiveHP(m.Handle)
		if !ok || hp <= 0 {
			continue // a dead body thinks nothing, exactly as it swings nothing
		}
		think, ok := h.monsterThinkFor(m, hp)
		if !ok {
			continue
		}
		h.MonsterBrain.ThinkMonster(think, now)
	}
}

// monsterThinkFor answers the runner's one input for a body: the entry its tactics state
// names, and the fields `self` answers.
//
// THE ENTRY IS THE REFERENCE'S DISPATCH, not a name this file chose: the body's `Tactics`
// COLUMN (`MON_AGGRESSIVE`) and its FSM tactics STATE (`TS_NONE`/`TS_BATTLE`), sprintf'd
// and uppercased - `SCR_%s_%s` + `_strupr_s` (MonsterNPC.cpp:1116-1118). A body whose
// corpus file does not define that name is reported ONCE by the runner, which is also
// what the reference does (`CreateThread` NULL -> Assert, MonsterNPC.cpp:1120-1124).
func (h *Handler) monsterThinkFor(m Monster, hp float64) (script.MonsterThink, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.engineLocked(m.Handle)
	if st == nil {
		return script.MonsterThink{}, false
	}
	entry := script.MonsterAIEntry(m.Tactics, st.tactic)
	if entry == "" {
		h.reportOnceEngine("monai:no-name:"+hexHandle(m.Handle),
			"zone: MONSTER AI 0x%08x %q cannot be given a tactics entry: its Tactics "+
				"column is %q and its state is %q, so the reference's SCR_%%s_%%s would be a "+
				"half-name (MonsterNPC.cpp:1116-1118) - nothing runs for this body",
			m.Handle, h.MonsterName(m), m.Tactics, st.tactic)
		return script.MonsterThink{}, false
	}
	mspd, monMSPD := h.monsterScriptSpeed(m)
	self := script.MonsterSelf{
		Handle:    m.Handle,
		ClassID:   m.ClassID,
		ClassName: h.MonsterName(m),
		Name:      h.MonsterName(m),
		Lv:        m.Level,
		X:         m.X,
		Y:         m.Y,
		Z:         m.Cell,
		HP:        hp,
		MHP:       m.MaxHP,
		MSPD:      mspd,
		MONMSPD:   monMSPD,
		RaceType:  m.RaceType,
	}
	h.reportWanderGate(m, st.tactic, self)
	return script.MonsterThink{
		Handle: m.Handle,
		Entry:  entry,
		Self:   self,
	}, true
}

// monsterScriptSpeed answers the TWO movement numbers a tactics script reads on `self`:
// `MSPD` (the Monster row's own column) and `MON_MSPD` (the row's COMPUTED
// `CP_MON_MSPD`, i.e. `monsterMSPD` - SCR_Get_MON_MSPD, the number the 3550 carries).
//
// NEITHER IS `Monster.ChaseSpeed`, AND THAT WAS THE CARD'S BUG. `ChaseSpeed` is the GO
// CHASE's capability: `monsterChaseSpeed` answers 0 for every tactics family that is not
// AGGRESSIVE*, which is right for "does this body chase a player" and wrong for "how fast
// does this body walk". Feeding it to the script made every non-aggressive body answer
// `self.MSPD` 0, and `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` refuses exactly that before it
// ever asks `IsEnableMove` (mon_ai_lib.scp:74-76) - so the NONE entry's wander was
// skipped in silence on fld_gig's 61 bodies (19 of them the Palm King Tree Crab, class
// 12981, row MSPD 217) and would have been on the 894 RANDOMLOCAL and 687 RANDOM rows
// behind them.
//
// A body with no row behind it (a hand-built probe, a class the shipped table does not
// list) keeps its ChaseSpeed copy for both: that is the only movement number it owns, and
// it is what this wired before the card.
func (h *Handler) monsterScriptSpeed(m Monster) (mspd, monMSPD float64) {
	if h.Monsters != nil {
		if row, ok := h.Monsters.Row(m.ClassID); ok {
			return row.MSPD, monsterMSPD(row)
		}
	}
	return m.ChaseSpeed, m.ChaseSpeed
}

// reportWanderGate is the ONE line an idle body gets when its own script refuses to walk
// it - and it exists because that refusal was INVISIBLE from this side.
//
// THE MEASURED STOP (card t_c873ad60: fld_gig, `-monai script`, zone up 2026-11-09). A
// Palm King Tree Crab (class 12981, tactics MON_GIGANTE, `Move="RANDOMLOCAL"`, SeekRange
// 0) opened `SCR_MON_GIGANTE_TS_NONE`, its thread stayed alive (`TACTIC THREAD ... start`
// and no end), and for the whole run the log carried no move order AND no refusal. The
// branch that should have walked it refused INSIDE THE SCRIPT, on the helper's FIRST
// line:
//
//	func SCR_MON_AI_LIB_CHECK_ENABLEMOVE(self)
//	    if (self.MSPD == 0) { return 'NO'; }        // mon_ai_lib.scp:74-76
//	    while(1) do if('YES' == IsEnableMove(self)) then return 'YES' end sleep(1000) end
//
// so `IsEnableMove` - this server's binding, `zone\monster_ai.go` EnableMove - was never
// called and no Go-side line could say why. `self.MSPD` was `Monster.ChaseSpeed`, which
// `monsterChaseSpeed` answers 0 for every tactics family that does not chase, so the NONE
// loop's move block (mon_ts_none.scp:275-296) was skipped on every pass. The same path
// serves the 894 monster-AI rows whose `Move` is RANDOMLOCAL and the 687 whose is RANDOM.
//
// IT NAMES THE SCRIPT'S OWN GATE, the number the script was handed, and the row's number,
// ONCE PER BODY. It fires only for a body whose tactics row asks it to MOVE at all: a
// corpse prop with `Move="None"` is not a refusal anybody needs explained.
func (h *Handler) reportWanderGate(m Monster, tactic string, self script.MonsterSelf) {
	if self.MSPD > 0 {
		return
	}
	move := h.aiMoveType(m.Tactics)
	if move == "" || strings.EqualFold(move, "none") {
		return
	}
	rowText := fmt.Sprintf("this zone holds no Monster row for class %d, so no MSPD column "+
		"could be read", m.ClassID)
	if h.Monsters != nil {
		if row, ok := h.Monsters.Row(m.ClassID); ok {
			rowText = fmt.Sprintf("this body's Monster row says MSPD %g", row.MSPD)
		}
	}
	h.reportOnceEngine("monai:wander-refused:"+hexHandle(m.Handle),
		"zone: MONSTER AI 0x%08x %q class %d in state %s gets NO wander: its tactics row "+
			"says Move=%q, and the script's own gate SCR_MON_AI_LIB_CHECK_ENABLEMOVE answers "+
			"'NO' on its FIRST line - `if (self.MSPD == 0)` (mon_ai_lib.scp:74-76) - before "+
			"IsEnableMove is ever asked, so the NONE entry's move block "+
			"(mon_ts_none.scp:275-296) can never order a walk for it. The script is handed "+
			"self.MSPD 0; %s (one line per body)",
		m.Handle, h.MonsterName(m), m.ClassID, tactic, move, rowText)
}

// aiMoveType answers the tactics row's own `Move` column - `RANDOM`, `RANDOMLOCAL`,
// `FOLLOWBOSS`, or `None` - or "" when this zone holds no row under that name. It is the
// column mon_ts_none.scp compares (`moveType`, :17, :275-296), read from the same table
// `GetMonAiOption` answers from, so no second copy of it exists.
func (h *Handler) aiMoveType(tactics string) string {
	table := h.monsterAiTable()
	if table == nil {
		return ""
	}
	row, ok := table.rows[strings.TrimSpace(tactics)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(row["Move"])
}

// SeekTarget is `GetObjectLocaly(self, range, type)`: the nearest live player character
// within `range` CENTRE units, or nobody.
//
// THE RULE, QUOTED. The reference visits a circle of `range` around the body with
// `CEnemySelector` and answers the first visitor that `IsValidTarget` accepts
// (SystemFunc.cpp:283-330; geVisitor.h:41-69): a character, ALIVE, not a zombie, not in
// a peace state, not invincible, and an enemy of the body. This field's characters are
// the players, and every one of them is an enemy of a monster - so the test collapses to
// "alive, within the circle".
//
// THE ONE TRANSLATION: the reference's answer depends on the sector grid's visit order,
// which is not a rule (the selector simply stops at the first valid visitor); this side
// answers the NEAREST. With one player in the circle the two agree exactly, and with
// several the choice is the one a human would name.
//
// THE FILTER IS THE SCRIPT'S THIRD ARGUMENT and it is accepted, not obeyed: `PC` and
// `MonAndPC` both answer a player here, because this zone's field holds no monster a
// monster may attack, and `Monster` answers nobody for the same reason. The `SelectFilter`
// COLUMN the corpus passes is not carried on Monster yet (create_tables.go does not read
// it), so the argument arrives as the empty string today - which the reference also treats
// as `OLT_ALL`. Naming the gap is the point: a filter that is silently ignored would look
// like a rule.
func (h *Handler) SeekTarget(mon uint32, rng float64, filter string) (script.HateTarget, bool) {
	if rng <= 0 {
		return script.HateTarget{}, false
	}
	h.spawnMu.Lock()
	h.pruneEngineLocked()
	m, ok := h.monsterLocked(mon)
	h.spawnMu.Unlock()
	if !ok {
		return script.HateTarget{}, false
	}
	switch strings.ToUpper(strings.TrimSpace(filter)) {
	case "MONSTER":
		return script.HateTarget{}, false // no monster is a target on this field yet
	case "ITEM":
		return script.HateTarget{}, false // no ground item is a target either
	}
	now := time.Now()
	// THE ONE ANSWER, and the line: the nearest PC is measured even when it is outside
	// the circle, so a nil is never reported as "nobody on the field" when the truth is
	// "somebody at 7,000 units". See reportSeekAnswer for the gate.
	v, dist, onField := h.nearestPlayer(m, now)
	if !onField {
		// AN EMPTY FIELD IS NOT A SEEK FAILURE, and it must not consume the line: the
		// first think pass can run while a client is connected but before its characters
		// are in the zone roster (measured: the threads started at 07:08:01.7, ENTRY KIND
		// 07:08:02.6, GAME_READY 07:08:04.1). A line there would answer "no player" for
		// ever and hide the distance that matters.
		return script.HateTarget{}, false
	}
	if dist > rng {
		h.reportSeekAnswer(m, rng, seekOutOfRange, v.handle, dist)
		return script.HateTarget{}, false
	}
	h.spawnMu.Lock()
	target, live := h.hateTargetLocked(v.handle)
	h.spawnMu.Unlock()
	if !live {
		h.reportSeekAnswer(m, rng, seekTargetGone, v.handle, dist)
		return script.HateTarget{}, false
	}
	h.reportSeekAnswer(m, rng, seekFound, v.handle, dist)
	return target, true
}

// The three answers `reportSeekAnswer` knows: a live PC inside the circle, the nearest PC
// measured outside it, and a nearest PC that is no longer a live target.
type seekAnswer int

const (
	seekFound seekAnswer = iota
	seekOutOfRange
	seekTargetGone
)

// reportSeekAnswer is the ONE line a body writes about what its seek answered.
//
// WHY IT EXISTS (the measured 2026-11-09 run on btl_auch01): the log showed
// `MONSTER AI DEBUG GetObjectLocaly(mon=..., rng=2500)` for every body and ZERO
// `MONSTER APPROACH`/`MONSTER RUN` lines, and nothing in the log could say whether
// the selector had answered nil because no player was inside the circle or because
// the selector/world was broken. This line prints the second number that separates
// the two cases: the distance to the NEAREST player on the field, even when that
// distance is far outside `rng`. A nil with the nearest PC at 400 units is a broken
// selector; a nil with the nearest PC at 7,315 units is the correct answer (that was
// the measured btl_auch01 run: the owner stood among MON_PASSIVERUN buffaloes whose
// tactics row has SeekRange 0, and the nearest AGGRESSIVE body was 7,315 units from
// the whole sampled route).
//
// THE GATE IS TWO KEYS PER BODY, NOT ONE PER SCAN: a NIL answer is printed once, the
// first time a body looks with a player actually on the field, and a FOUND answer is
// printed once when the body acquires. A body that sees the player at 7,000 units and
// then has it walk into the circle writes exactly two lines, in that order - which is
// the sequence a reader needs to tell "not in range yet" from "never in range". A
// repeat of the SAME answer writes nothing (the flood commit 83bcf77c removed), and a
// body on a field with no player writes nothing at all: the first think pass can run
// before the client's characters join (measured 2026-11-09: threads 07:08:01.7, ENTRY
// KIND 07:08:02.6, GAME_READY 07:08:04.1), and an "empty field" line there would
// consume the line that matters.
func (h *Handler) reportSeekAnswer(m Monster, rng float64, answer seekAnswer, pc uint32, dist float64) {
	key := "seek:answer:" + hexHandle(m.Handle)
	if answer == seekFound {
		key = "seek:found:" + hexHandle(m.Handle)
	}
	switch answer {
	case seekFound:
		h.reportOnceEngine(key,
			"zone: MONSTER AI SEEK 0x%08x %q rng=%g -> FOUND PC 0x%08x at %.0f units "+
				"(the acquire; one line per body)",
			m.Handle, h.MonsterName(m), rng, pc, dist)
	case seekOutOfRange:
		h.reportOnceEngine(key,
			"zone: MONSTER AI SEEK 0x%08x %q rng=%g -> nobody; the nearest PC 0x%08x is "+
				"%.0f units away, OUTSIDE the circle (one line per body)",
			m.Handle, h.MonsterName(m), rng, pc, dist)
	case seekTargetGone:
		h.reportOnceEngine(key,
			"zone: MONSTER AI SEEK 0x%08x %q rng=%g -> the nearest PC 0x%08x at %.0f "+
				"units is not a live target any more (one line per body)",
			m.Handle, h.MonsterName(m), rng, pc, dist)
	}
}

// ResetHate is `ResetHate(self)` - `m_hates->RemoveAll()` (MonsterNPC.cpp:132-134), the
// list reset the NONE entry opens with (mon_ts_none.scp:33).
//
// The TACTICS TARGET is left alone: the reference's `RemoveAll` empties the list only,
// and the script clears the target itself with `SetTacticsTarget(self)` (mon_ts_battle.scp:157).
func (h *Handler) ResetHate(mon uint32) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneHateLocked()
	if st := h.hateLocked(mon, false); st != nil {
		st.entries = nil
	}
}

// EnableMove is `IsEnableMove(self)`: can this body START a move now?
//
// THE RULE IS THE REFERENCE'S (MonsterNPCFunc.cpp:515-530): 'YES' while the move state is
// MS_STOP or MS_INITIAL, 'NO' for any other state - and 'NO' for an object that is not a
// monster or pet, which every handle on this field is.
//
// OUR MOVE STATE IS THE LEG. This engine has no separate MS_* field: a body walks exactly
// while a leg it was given is still in flight (`monsterLeg.active`, zone\monster_chase.go),
// and three places write one - the GO CHASE's engagement (`h.monsterChases`), in script
// mode the attack ORDER's approach (`monsterEngineState.order.approach.chase.leg`, the
// chase's own state again), and the script's own `Move`/`RandomMove` ORDER
// (`monsterEngineState.order.move`, walked by `monsterMoveTick`). So the answer is "no leg
// is walking", read from the three places a leg can live, and nothing else: no timer, no
// distance, no AI rule.
//
// WHY IT MATTERS (the card's bug, in one line): `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` is a
// WAIT LOOP - `while(1){ if IsEnableMove=='YES' return 'YES'; sleep(1000) }`
// (mon_ai_lib.scp:72-83) - and the NONE entry's Move block calls it on every second it
// loses `math.random(0,99) > 50` (mon_ts_none.scp:275-296), BEFORE the seek at `:230`. An
// answer the script can never match parks that body's whole tactics thread there for the
// rest of its life; the measured run is quoted at script\monster_ai.go's enableMove.
//
// THE LOCKS are taken in the house order (chaseMu, then spawnMu: `monsterApproachStep`
// does the same, zone\monster_order.go:559), because this is answered from INSIDE a Lua
// resume, i.e. under the script lock, and it must not become a third order.
func (h *Handler) EnableMove(mon uint32) bool {
	now := h.monsterNow()
	h.chaseMu.Lock()
	defer h.chaseMu.Unlock()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()

	// A leg on the walk the ORDER armed (script mode) or on the engagement the Go chase
	// runs (go mode). Either one means MS_MOVE/MoveEnd, i.e. 'NO'.
	if st := h.engineLocked(mon); st != nil {
		if _, walking := st.order.approach.chase.leg.posAt(now); walking {
			return false
		}
		// THE SCRIPT'S OWN MOVE IS THE THIRD PLACE A LEG LIVES (card t_16ab5afc): a
		// `Move`/`RandomMove` order stores a leg and `monsterMoveTick` walks the body
		// along it, so while that leg is in flight the body IS moving and the reference's
		// answer is 'NO' - which is what parks the script's own wait loop
		// (mon_ai_lib.scp:72-83) until the body arrives instead of drawing a fresh goal
		// every pass. Without this line the walk has no clock here, the answer is 'YES'
		// one tick after the order, and the 3550 flood is the script doing exactly what
		// it was told.
		if _, walking := st.order.move.posAt(now); walking {
			return false
		}
	}
	if st := h.monsterChases[mon]; st != nil {
		if _, walking := st.leg.posAt(now); walking {
			return false
		}
	}
	// Standing still - or a handle this zone holds no walk for, which is the same answer
	// a body that has not been ordered anywhere gives: it may start one.
	return true
}
