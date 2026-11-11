// THE MONSTER'S OWN CHASE. ACQUIRE, WALK, STOP AND SWING, GIVE UP.
//
// WHAT THIS FILE DOES. A monster this zone runs (a body a script created, with a
// live HP ledger and a row behind it) that belongs to the AGGRESSIVE monster-AI
// family does four things on the zone's own clock:
//
//  1. ACQUIRE the nearest player character within the MONSTER'S OWN AI ROW's
//     `SeekRange` (datatable_monsterai.xml; MON_AGGRESSIVE says 2500). It is a
//     CENTRE distance: the row's radius is the range at which the body starts
//     caring, and the edge-to-edge arithmetic belongs to the reach, not to this
//     question. A row with `SeekRange="0"` - or `Attack="FALSE"` - never acquires.
//  2. WALK toward it while the edge gap is beyond the row's own `MaxR`, through
//     `Handler.Plan` (the map's own path machinery), and send ONE
//     `ZC_MOVE_MONSTER` (3550, 26 bytes) per CHANGED stand point - never one per
//     tick. The reference does exactly this in `CFieldChar::AS_ATTACK_CHASE`
//     (source2\geServer\ZServerLib\FieldChar_FSM.cpp:28-95): it re-plans every
//     300 ms, and only calls `SetMoveState(MS_MOVE)` when `GetAttackMaxPos`
//     returned a point that differs from `m_moveTo` (`IsEqualPos(moveTo,
//     m_moveTo)`).
//  3. STOP inside `MaxR`. The swing (zone\monster_swing.go) owns the cadence and
//     the damage; this file sends no attack packet and changes no HP.
//  4. GIVE UP when the target dies, leaves the zone, or gets farther than
//     `KeepReturnRange` (2000) from the point where the engagement STARTED, and
//     walk home to that point, then idle.
//
// A DEAD BODY NEVER CHASES. It is skipped the moment its live HP is 0, which is
// the same test the swing uses, and its state is dropped when the body leaves the
// roster.
//
// THE RULE'S OWN EVIDENCE, all verified in the tree before this file was written:
//
//	acquisition  the game's own AI script, ge\script\monster\mon_ts_none.scp:7 and
//	             :223-240 - `seekRange = GetMonAiOption(self,'SeekRange')`, then
//	             `target = GetObjectLocaly(self, seekRange, filter)`, and
//	             `if('TRUE' == attackable)` gates `ChangeTactics(TS_BATTLE)`.
//	             GetMonAiOption answers a column of datatable_monsterai.xml for the
//	             tactics the body runs, so the SEEK RADIUS IS THE ROW'S OWN - not
//	             the ATTACKER profile's AssaultRange (1000) this file first used.
//	             MON_AGGRESSIVE carries SeekRange="2500"; MON_MISSION_84 carries
//	             SeekRange="0" and Attack="FALSE", a mission prop that never looks.
//	family       the Monster table has NO per-monster AI column; its `Tactics`
//	             names a row of ge\xml\datatable_monsterai.xml (MON_AGGRESSIVE ->
//	             AIType="AGGRESSIVE"), and the aggressive family is the one this
//	             chase serves (AGGRESSIVE is the attack family)
//	speed        the row's own `CP_MON_MSPD="SCR_Get_MON_MSPD"` makes MON_MSPD a
//	             COMPUTED property; CMonsterNPC::GetMoveSPD answers GetMON_MSPD()
//	             (MonsterNPC.cpp:1929-1932); the move FSM stamps
//	             `m_moveSpeed = GetMoveSPD()` before BroadcastStartMovement
//	             (FieldChar_FSM.cpp:599-603); SCR_Get_MON_MSPD is
//	             `MSPD * (MSPDFix*0.01+1) * (100+MSPD_BM)/100` floored
//	             (monster\get_status_monster.lua:535-547). Class 11591
//	             (Monster_Bigfoot) is MSPD 143, so an IDLE body walks at 143 - NOT
//	             WSPD 130 and NOT RSPD 856. A body WITH an engagement (a chase, or
//	             an attack order in flight) RUNS at the row's SPDBM rule instead -
//	             `monsterRunSpeed`, the same formula with `SPDBM` in place of
//	             `MSPD_BM` - measured 2026-11-08 on five live bodies
//	             (history\combat.md, "A monster's RUN speed is the row's SPDBM");
//	             the reference's own run lever is `SetMoveSpeed` = SetBMValue
//	             "MSPD_BM" (FieldChar.cpp:1405-1412), which is that same read
//	packet       PKS_ZC_MOVE_MONSTER : public PKS_ZC_MOVE
//	             (source2\geShared\include\geZCPacket.h:1665): 26 bytes, built by
//	             BuildMoveMonster (zone\move.go); CMonsterNPC::
//	             BroadcastStartMovement fills handle, tick, pos, spd
//	             (MonsterNPC.cpp:592-601)
//
// WHAT IS DELIBERATELY NOT HERE:
//
//   - no ZC_STOP_MONSTER (3551). The card scopes the wire to the 3550 and the
//     existing 3564, and a monster's 3564 names its CURRENT position, which the
//     client hard-sets its actor to (GeActor.cpp:300-311, the same rule the
//     port's own swing comment records). Inventing a stop packet is a second
//     layout nobody measured. When the body reaches reach the leg stops
//     advancing, so the next swing names the place it stopped at.
//   - no hate table, no random walk, no flee, no summons. The row's `Tactics`
//     only decides WHETHER this body chases; the chase itself is one target at
//     a time.
//   - no fallback walk when the zone has no path engine. The card says the walk
//     goes through the map's own path machinery; without it the monster stands
//     and swings at what comes into reach, and one log line says so.
package zone

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// aiAttackerKeepReturnRange is the ATTACKER profile's `KeepReturnRange="2000"`
	// (ge\xml\datatable_ai.xml:3): how far the target may get from the point the
	// engagement started before the body gives up and walks home.
	//
	// IT IS STILL A PROFILE NUMBER, NOT A ROW ONE: datatable_monsterai.xml has no
	// leash column, and the reference's own tactics scripts ask the game for the
	// return range through the character rather than the AI row. What the AI row
	// owns is the SEEK, and that is now read from it.
	aiAttackerKeepReturnRange = 2000.0

	// monsterChaseReplan is the reference's OWN re-plan interval: the AS_ATTACK_CHASE
	// update re-asks GetAttackMaxPos at most once per 300 ms
	// (`m_nextAttackChaseTime = timeGetTime() + 300`, FieldChar_FSM.cpp:62-63).
	// The tick may run ten times in that window; the stand point is asked three.
	monsterChaseReplan = 300 * time.Millisecond
)

// monsterAggressiveFamily is the monster-AI family this slice's profile serves. The
// tactics table names it `AGGRESSIVE` (datatable_monsterai.xml: MON_AGGRESSIVE,
// MON_AGGRESSIVERUN, ... all carry AIType="AGGRESSIVE").
func monsterAggressiveFamily(aiType string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(aiType)), "AGGRESSIVE")
}

// monsterAssaultRange answers where this body starts caring, or 0 when it never
// does. 0 is "never acquires", and the chase skips it.
//
// THE RADIUS IS THE TACTICS ROW'S OWN `SeekRange`, NOT A PROFILE CONSTANT. The
// game's AI script asks `GetMonAiOption(self,'SeekRange')` and hands that number to
// `GetObjectLocaly` (monster\mon_ts_none.scp:7, :223-240), so a MON_AGGRESSIVE body
// looks 2500 units; the ATTACKER profile's AssaultRange 1000 was this file's first
// answer and was WRONG for a monster (that column is the PLAYER-side AI profile).
//
// `Attack="FALSE"` also answers 0: the script may find a target but never enters
// TS_BATTLE for a body that does not attack, so there is nothing to chase for.
func monsterAssaultRange(ai MonsterAIRow) float64 {
	if !monsterAggressiveFamily(ai.AIType) || !ai.Attack || ai.SeekRange <= 0 {
		return 0
	}
	return ai.SeekRange
}

// monsterKeepReturnRange answers how far this body will follow, or 0 for a body
// that does not chase at all.
func monsterKeepReturnRange(aiType string) float64 {
	if !monsterAggressiveFamily(aiType) {
		return 0
	}
	return aiAttackerKeepReturnRange
}

// monsterMSPD is `SCR_Get_MON_MSPD` (monster\get_status_monster.lua:535-547),
// quoted:
//
//	value = self.MSPD * (self.MSPDFix * 0.01 + 1)
//	value = value * (100 + self.MSPD_BM) / 100
//	if value < 0 then value = 0 elseif value > 2500 then value = 2500
//	return math.floor(value)
//
// It is the number the reference's move state uses for the walk (GetMoveSPD ->
// GetMON_MSPD, MonsterNPC.cpp:1929-1932) and the number the 3550 carries.
func monsterMSPD(row MonsterRow) float64 {
	value := row.MSPD * (row.MSPDFix*0.01 + 1)
	value = value * (100 + row.MSPDBM) / 100
	if value < 0 {
		value = 0
	} else if value > 2500 {
		value = 2500
	}
	return math.Floor(value)
}

// monsterChaseSpeed is the speed this body walks at with NO engagement, or 0 when it
// does not chase. It is the row's `SCR_Get_MON_MSPD`; a body that engaged with a
// target RUNS at `monsterRunSpeed` instead - `monsterWalkAndRun` answers both.
func monsterChaseSpeed(aiType string, row MonsterRow) float64 {
	if !monsterAggressiveFamily(aiType) {
		return 0
	}
	return monsterMSPD(row)
}

// monsterRunSpeed is the speed a body moves at while it is ENGAGED - a chase following
// a player, or an attack order the script gave walking the body into reach. It is the
// row's SPDBM rule, measured 2026-11-08 on five live bodies and pinned in
// history\combat.md ("A monster's RUN speed is the row's SPDBM, not RSPD"):
//
//	runSpeed = floor(MSPD * (MSPDFix*0.01 + 1) * (100 + SPDBM) / 100)
//
// The five bodies it was measured on, wire value against row: Boar 10093 91 -> 362
// (SPDBM 298), Jaquin 10281 121 -> 145 (20), Milhou 11212 202 -> 339 (68), Rion Bear
// 11483 131 -> 390 (198), Hornet 10071 150 -> 480 (220). RSPD never appears on the
// wire - it is the client's animation column. Nothing is clamped here: the 0..2500
// bound belongs to `SCR_Get_MON_MSPD` (get_status_monster.lua:535-547), and this is
// the measured run rule, not that script.
func monsterRunSpeed(row MonsterRow) float64 {
	value := row.MSPD * (row.MSPDFix*0.01 + 1)
	value = value * (100 + row.SPDBM) / 100
	return math.Floor(value)
}

// monsterWalkAndRun answers a body's TWO walk speeds from its row - the WALK speed
// `SCR_Get_MON_MSPD` (what a body with no engagement carries: a script `Move`, the walk
// home after a chase gave up) and the RUN speed `monsterRunSpeed` (what a chase or an
// attack order in flight carries) - plus the row's SPDBM for the log line.
//
// ONE RULE, TWO READERS: the go chase's `planMonsterChase` and the script approach's
// `monsterApproachStep` both ask HERE, so the two engagements cannot drift into two
// speeds. A body with no row behind it (a hand-built probe) answers its ChaseSpeed copy
// for both: the copy IS the row's walk speed, and there is no SPDBM to apply.
func (h *Handler) monsterWalkAndRun(m Monster) (walk, run, spdbm float64, known bool) {
	if h.Monsters != nil {
		if row, ok := h.Monsters.Row(m.ClassID); ok {
			return monsterMSPD(row), monsterRunSpeed(row), row.SPDBM, true
		}
	}
	if m.ChaseSpeed > 0 {
		return m.ChaseSpeed, m.ChaseSpeed, 0, true
	}
	return 0, 0, 0, false
}

// logMonsterRun writes the ONE line a body's walk-speed change is allowed: the
// engagement's start prints `MSPD a -> b`, its end prints the same line with the
// numbers reversed. A row whose SPDBM leaves the number alone writes no line - there
// was no change to announce.
//
//	zone: MONSTER RUN 0x%08x class %d MSPD <a> -> <b> (SPDBM <n>)
func (h *Handler) logMonsterRun(m Monster, from, to, spdbm float64) {
	if from == to {
		return
	}
	h.Log.Printf("zone: MONSTER RUN 0x%08x class %d MSPD %g -> %g (SPDBM %g)",
		m.Handle, m.ClassID, from, to, spdbm)
}

// monsterLeg is ONE move this file put on the wire: where the body was, where it
// was told to go, the route it walks, and the clock and speed the body is drawn
// with. It is the monster's answer to `walk` (chase.go), with no session and no
// purpose: the only reader is the body's own position.
type monsterLeg struct {
	from  [3]int32
	goal  [3]int32
	route [][3]int32
	began time.Time
	speed float64
	// active is true while the body is still walking. It goes false when the route
	// is walked out, when the retreat starts, or when the monster reaches its own
	// reach and stops to swing.
	active bool
}

// posAt answers where this leg's body is at `now`, and whether it is still walking.
func (l *monsterLeg) posAt(now time.Time) ([3]int32, bool) {
	if !l.active {
		return l.goal, false
	}
	length := legLength(l.from, l.goal, l.route)
	walked := l.speed * now.Sub(l.began).Seconds()
	if l.speed <= 0 || walked >= length {
		return l.goal, false
	}
	return walkedRouteAt(l.route, l.from, l.goal, l.began, now, l.speed), true
}

// monsterChaseState is one body's engagement: who it is following, where the
// engagement started (and therefore where home is), where the body is, and the
// move in flight.
type monsterChaseState struct {
	target uint32 // the player CHARACTER handle being chased; 0 = nobody
	home   [3]int32
	body   [3]int32
	leg    monsterLeg

	// lastStand/hasStand are the "one order per CHANGED stand point" record: the
	// point the last 3550 named, and whether there is one. A re-plan that answers
	// the same point sends nothing.
	lastStand [3]int32
	hasStand  bool

	// nextPlan is when this body may ask the path engine again (300 ms, the
	// reference's own AS_ATTACK_CHASE interval).
	nextPlan time.Time

	// returning is true once the engagement is over: the body walks home and
	// ignores players until it is there.
	returning bool

	// run is true while the MONSTER RUN line is in force for this engagement: the
	// body is walking at the row's SPDBM speed, and the line back to MSPD is owed
	// when the engagement ends.
	run bool
}

// monsterChaseArmed reports whether ANY monster in this zone could chase, which is
// what keeps a session's loop ticking while a body is walking (zone\chase.go
// tickIfDue) - the sibling of monsterSwingArmed. A chase that gave up still walks
// home, so the body being alive is the whole question.
func (h *Handler) monsterChaseArmed() bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for _, m := range h.spawnedMonster {
		if m.AssaultRange <= 0 || m.ChaseSpeed <= 0 {
			continue
		}
		if hp, ok := h.monsterHP[m.Handle]; ok && hp > 0 {
			return true
		}
	}
	return false
}

// monsterChaseTick advances every chaser by one step. It is called from
// `tickIfDue` beside the swing, on the session poll clock - but unlike the swing it
// is a ZONE clock, so ONE step is taken per `pollInterval`, not one per session:
// the first session's tick in the window advances the bodies and the others see
// `monsterChaseRan` and return. Without that, three sessions would walk every
// monster three times as far.
//
// The lock is the step's own, not a per-monster claim: a body's position, its
// target choice and its leg must move together, or a second session could re-plan
// from a position the first one has already left.
func (h *Handler) monsterChaseTick(now time.Time) {
	h.chaseMu.Lock()
	defer h.chaseMu.Unlock()
	if !h.monsterChaseRan.IsZero() && now.Sub(h.monsterChaseRan) < pollInterval {
		return
	}
	h.monsterChaseRan = now

	live := make(map[uint32]bool, len(h.monsterChases)+4)
	for _, m := range h.runtimeMonsters() {
		hp, ok := h.monsterLiveHP(m.Handle)
		if !ok || hp <= 0 {
			continue // dead, leaving, or not a body this zone keeps HP for
		}
		if m.AssaultRange <= 0 || m.ChaseSpeed <= 0 {
			continue // this row's tactics family does not chase
		}
		live[m.Handle] = true

		st, known := h.monsterChases[m.Handle]
		if !known {
			home := [3]int32{m.X, m.Y, m.Cell}
			st = &monsterChaseState{home: home, body: home}
		} else {
			// THE BODY FIRST: this step reads where the body has walked TO, never
			// where the last order sent it.
			h.advanceMonsterLeg(m.Handle, st, now)
		}

		if st.returning {
			if !st.leg.active && flatDist(st.body, st.home) <= ArrivalEpsilon {
				delete(h.monsterChases, m.Handle) // home: idle, and a new engagement may begin
			}
			continue
		}

		if st.target == 0 {
			v, taken := h.nearestPlayerWithin(m, now, m.AssaultRange)
			if !taken {
				continue // no engagement, and no state kept for an idle body
			}
			st.target = v.handle
			st.home = st.body
			st.hasStand = false
			st.leg = monsterLeg{}
			if h.monsterChases == nil {
				h.monsterChases = make(map[uint32]*monsterChaseState, 4)
			}
			h.monsterChases[m.Handle] = st
			h.Log.Printf("zone: MONSTER CHASE %d %q ACQUIRES player %d (%q) at (%d, %d): "+
				"%g units away, inside the monster-AI row's SeekRange %g - home is "+
				"(%d, %d, cell %d), it will follow up to %g units from there",
				m.Handle, h.MonsterName(m), v.handle, v.sess.Team[v.idx].Name,
				v.pos[0], v.pos[1], flatDist(st.body, v.pos), m.AssaultRange,
				st.home[0], st.home[1], st.home[2], m.KeepReturnRange)
		}

		v, held := h.playerBodyByHandle(st.target, now)
		if !held {
			h.giveUpChase(m, st, now, "the target is gone (dead or out of the zone)")
			continue
		}
		insideKeepReturn := m.KeepReturnRange <= 0 ||
			flatDist(v.pos, st.home) <= m.KeepReturnRange
		if !insideKeepReturn {
			h.giveUpChase(m, st, now, "the target is beyond KeepReturnRange "+
				strconv.FormatFloat(m.KeepReturnRange, 'f', 0, 64)+" from where the "+
				"engagement started")
			continue
		}

		monR := h.monsterBodyRadius(m)
		gap := flatDist(st.body, v.pos) - (monR + PCBodyRadius)
		if gap <= m.MaxR {
			// STOP AND SWING. The swing's own tick owns the cadence and the damage;
			// this file only stops walking. The leg is frozen at the point the body
			// has reached, so the next 3564 names that point and the client hard-sets
			// its actor there.
			if st.leg.active {
				st.leg.active = false
			}
			st.hasStand = false
			continue
		}

		if now.Before(st.nextPlan) {
			continue
		}
		st.nextPlan = now.Add(monsterChaseReplan)
		h.planMonsterChase(m, st, v.pos, monR, now)
	}

	for handle := range h.monsterChases {
		if !live[handle] {
			delete(h.monsterChases, handle)
		}
	}
}

// advanceMonsterLeg draws the body along its leg at the leg's own speed and writes
// the point into the roster, so the reach test, the sight ring and the next 3564
// all read one position.
//
// BOTH ENGAGEMENTS ADVANCE THROUGH THIS: the go chase's step and the script's attack
// approach (zone\monster_order.go) hold the same `monsterLeg` and the same reader, so a
// body is drawn identically whether a Go rule or the game's own script ordered the walk.
func (h *Handler) advanceMonsterLeg(handle uint32, st *monsterChaseState, now time.Time) {
	if !st.leg.active {
		return
	}
	pos, walking := st.leg.posAt(now)
	if pos != st.body {
		st.body = pos
		h.setMonsterPos(handle, pos)
	}
	if !walking {
		st.leg.active = false
	}
}

// monsterPlan is ONE walk the plan core ordered: its goal, the route it walks,
// whether the engine had to answer the target's own point, and the 3550 it put out.
type monsterPlan struct {
	goal      [3]int32
	routeLen  float64
	waypoints int // corners the engine answered, 0 when it gave no route
	overlap   bool
	sent      int
	size      int
}

// planMonsterWalk is THE one walk plan for a monster body: the reach ring, the map's
// own path engine, the "one order per CHANGED stand point" record, and the 3550 through
// the ordinary per-viewer fan-out.
//
// IT IS SHARED BY BOTH ENGAGEMENTS ON PURPOSE - one walk implementation, not two. The
// GO CHASE calls it through `planMonsterChase` below and keeps its own line; the
// SCRIPT'S ATTACK ORDER calls it through `monsterApproachStep` (zone\monster_order.go),
// which walks the body into reach and stops where the swing's own check says in-range.
// `speed` is the caller's answer to "how fast does this body walk": both end at the
// row's RUN speed (`monsterRunSpeed`, the SPDBM rule) because BOTH callers are an
// ENGAGEMENT - the go chase walking a body it has acquired and the script's attack order
// walking a body into reach. A body with no engagement never reaches this function; its
// walk (a script `Move`, the walk home) is built with `monsterMSPD` directly.
// `what` is only the label of the failure line ("CHASE" / "APPROACH"), so a log reader
// can tell which engagement could not plan.
func (h *Handler) planMonsterWalk(m Monster, st *monsterChaseState, target [3]int32, monR, speed float64, now time.Time, what string) (monsterPlan, bool) {
	if h.Plan == nil {
		if h.chaseNoPlanLogged.Swap(1) == 0 {
			h.Log.Printf("zone: MONSTER CHASE has no path engine (Handler.Plan is nil), so " +
				"no monster can name a stand point that is real ground - every body stands " +
				"where it was put and swings at what comes into reach")
		}
		return monsterPlan{}, false
	}
	// A WALK AT SPEED 0 IS NOT A WALK (2026-11-10): the client's CActor::MoveTo
	// asserts the speed is not 0 (GeActor.cpp:250) and its path state advances by
	// m_updateTime*m_moveSpeed (GeActor.cpp:2254), so a 0-speed 3550 puts the body in
	// a move state that never advances - a frozen walk pose, not a refusal. No live
	// 3550 carries 0 (0 of 494 in captures\kill01, kite4000c and chasec20261108: the
	// range is 67..1419), so this zone must not send one either.
	if speed <= 0 {
		h.reportOnceEngine("walk:zero-speed:"+hexHandle(m.Handle),
			"zone: MONSTER %s 0x%08x %q: its walk speed is 0 or unknown, so NO 3550 is "+
				"sent - a 0-speed move is one the client starts and can never advance",
			what, m.Handle, h.MonsterName(m))
		return monsterPlan{}, false
	}
	standoff := attackStandoff(m.MinR, m.MaxR, monR, PCBodyRadius)
	goal, route, routeLen, overlap, ok, err := h.Plan(st.body, target, standoff)
	switch {
	case err != nil:
		h.Log.Printf("zone: MONSTER %s %d %q: the path engine failed (%v) - no order",
			what, m.Handle, h.MonsterName(m), err)
		return monsterPlan{}, false
	case !ok:
		// The body's own point is not on the mesh: a walk from it would be a guess.
		// It still swings at what comes into reach, so nothing is lost but the walk.
		return monsterPlan{}, false
	}
	if st.hasStand && goal == st.lastStand {
		return monsterPlan{}, false // THE SAME STAND POINT: one order, never one per tick
	}
	if routeLen <= 0 {
		routeLen = flatDist(st.body, goal)
	}
	st.lastStand, st.hasStand = goal, true
	st.leg = monsterLeg{from: st.body, goal: goal, route: route, began: now,
		speed: speed, active: true}

	pkt := BuildMoveMonster(m.Handle, goal, uint32(nowMillis()), float32(speed))
	sent := h.fanOut(nil, m.Handle, pkt)
	return monsterPlan{goal: goal, routeLen: routeLen, waypoints: len(route),
		overlap: overlap, sent: sent, size: len(pkt)}, true
}

// planMonsterChase plans the go chase's next leg: the walk core above, then the CHASE
// line this mode has always written. The line keeps its shape and now names the run
// rule its speed came from.
func (h *Handler) planMonsterChase(m Monster, st *monsterChaseState, target [3]int32, monR float64, now time.Time) {
	// AN ENGAGED CHASE RUNS: the row's SPDBM speed, from the ONE helper the script
	// approach asks as well. m.ChaseSpeed is the no-engagement copy (the row's MSPD)
	// and stays the fallback for a hand-built body with no table behind it.
	walk, run, spdbm, known := h.monsterWalkAndRun(m)
	speed := m.ChaseSpeed
	if known && run > 0 {
		speed = run
	}
	p, ok := h.planMonsterWalk(m, st, target, monR, speed, now, "CHASE")
	if !ok {
		return
	}
	if !st.run && speed != walk {
		// THE FIRST 3550 AT THE RUN SPEED OPENS THE LINE: once per engagement, and only
		// when the row's SPDBM actually changes the number. A body that never walks (no
		// path engine, no route) has no speed change to announce.
		h.logMonsterRun(m, walk, speed, spdbm)
		st.run = true
	}
	where := "the stand point"
	if p.overlap {
		where = "the target's own point (the engine found no clear stand-off, so the " +
			"client stops at collision distance)"
	}
	h.Log.Printf("zone: MONSTER CHASE %d %q walks to %s (%d, %d, cell %d): %g units of "+
		"route at %.0f u/s, eta %s, speed from the row's SPDBM run rule (MSPD %g, "+
		"SPDBM %g) - cmd %d (%d B) to %d session(s)",
		m.Handle, h.MonsterName(m), where, p.goal[0], p.goal[1], p.goal[2], p.routeLen,
		speed, time.Duration(p.routeLen/speed*float64(time.Second)).Round(time.Millisecond),
		walk, spdbm, CmdZCMoveMonster, p.size, p.sent)
}

// giveUpChase ends an engagement and orders the walk home, or clears the state when
// the body is already there.
//
// HOME IS WHERE THE ENGAGEMENT STARTED, not the row's spawn point: the body may
// have been pulled a long way by an earlier fight, and the reference's own leash
// is measured from the point it decided to chase (m_pos at AS_ATTACK_CHASE entry).
func (h *Handler) giveUpChase(m Monster, st *monsterChaseState, now time.Time, why string) {
	target := st.target
	st.target = 0
	st.returning = true
	st.hasStand = false
	if st.leg.active {
		st.leg.active = false // freeze the body where it got to
	}
	h.Log.Printf("zone: MONSTER CHASE %d %q GIVES UP on player %d: %s - walking home to "+
		"(%d, %d, cell %d)", m.Handle, h.MonsterName(m), target, why,
		st.home[0], st.home[1], st.home[2])
	// THE ENGAGEMENT IS OVER: the walk home is the no-engagement speed again, and the
	// MONSTER RUN line is closed out once (the same line, numbers reversed).
	walk, run, spdbm, known := h.monsterWalkAndRun(m)
	if !known || walk <= 0 {
		walk = m.ChaseSpeed
	}
	if from := run; st.run {
		if from <= 0 {
			from = m.ChaseSpeed
		}
		h.logMonsterRun(m, from, walk, spdbm)
		st.run = false
	}
	if flatDist(st.body, st.home) <= ArrivalEpsilon {
		delete(h.monsterChases, m.Handle)
		return
	}
	var route [][3]int32
	if h.Route != nil {
		if r, ok := h.Route(st.body, st.home); ok {
			route = r
		}
	}
	st.leg = monsterLeg{from: st.body, goal: st.home, route: route, began: now,
		speed: walk, active: true}
	pkt := BuildMoveMonster(m.Handle, st.home, uint32(nowMillis()), float32(walk))
	sent := h.fanOut(nil, m.Handle, pkt)
	h.Log.Printf("zone: MONSTER CHASE %d %q walks HOME to (%d, %d, cell %d) at %.0f u/s - "+
		"cmd %d (%d B) to %d session(s); it will idle there until a player comes inside "+
		"its SeekRange %g again", m.Handle, h.MonsterName(m), st.home[0], st.home[1],
		st.home[2], walk, CmdZCMoveMonster, len(pkt), sent, m.AssaultRange)
}

// nearestPlayer answers the NEAREST living player character on this field and its
// flat distance from the monster, with no radius applied.
//
// THE BODY, NOT THE DESTINATION: the candidate point is `sessionBodyAt`, the reader
// the sight ring and the swing use, so a player who is mid-walk is measured where
// it is DRAWN.
//
// IT IS THE ONE WALK: `nearestPlayerWithin` is this answer with the radius applied
// after the fact, and the seek's diagnostic prints the same `d` when a body found
// nobody - one definition of "nearest", so the line cannot contradict the answer.
func (h *Handler) nearestPlayer(m Monster, now time.Time) (swingVictim, float64, bool) {
	monPos := [3]int32{m.X, m.Y, m.Cell}
	var best swingVictim
	bestD := -1.0
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			c := &mem.Sess.Team[j]
			if c.HP <= 0 {
				continue
			}
			pos := h.sessionBodyAt(mem.Sess, j, now)
			d := flatDist(monPos, pos)
			if bestD < 0 || d < bestD {
				best = swingVictim{sess: mem.Sess, idx: j, handle: c.Handle, pos: pos}
				bestD = d
			}
		}
	}
	return best, bestD, bestD >= 0
}

// nearestPlayerWithin answers the NEAREST living player character whose centre is
// within `radius` of this monster, or false when none is.
func (h *Handler) nearestPlayerWithin(m Monster, now time.Time, radius float64) (swingVictim, bool) {
	v, d, ok := h.nearestPlayer(m, now)
	if !ok || d > radius {
		return swingVictim{}, false
	}
	return v, true
}

// playerBodyByHandle answers where ONE player character's body stands right now,
// or false when it is dead or no longer in the zone. It is how a chase keeps its
// target: the handle it acquired, re-read every step.
func (h *Handler) playerBodyByHandle(handle uint32, now time.Time) (swingVictim, bool) {
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			c := &mem.Sess.Team[j]
			if c.Handle != handle || c.HP <= 0 {
				continue
			}
			return swingVictim{sess: mem.Sess, idx: j, handle: c.Handle,
				pos: h.sessionBodyAt(mem.Sess, j, now)}, true
		}
	}
	return swingVictim{}, false
}

// setMonsterPos writes a walking body's position back into the roster, so every
// reader of `runtimeMonsters` (the swing, the sight ring, the next chase step) sees
// the point it has walked to.
func (h *Handler) setMonsterPos(handle uint32, pos [3]int32) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == handle {
			h.spawnedMonster[i].X = pos[0]
			h.spawnedMonster[i].Y = pos[1]
			h.spawnedMonster[i].Cell = pos[2]
			return
		}
	}
}

// MonsterChaseStartupLine is the ONE startup line about the chase, printed by main
// beside the swing's line. It says where the engage radius comes from, and it names
// the speed it will use AND the two columns it does not: the brief asked for the
// number to be read out of the reference rather than guessed between MSPD, WSPD and
// RSPD.
func MonsterChaseStartupLine() string {
	return "zone: monster chase: engage = the MONSTER-AI ROW's own SeekRange " +
		"(datatable_monsterai.xml, read by the game's own AI script with " +
		"GetMonAiOption(self,'SeekRange') - monster\\mon_ts_none.scp:7,223 - e.g. " +
		"MON_AGGRESSIVE 2500), never the ATTACKER profile's AssaultRange 1000; " +
		"Attack=\"FALSE\" or SeekRange 0 never acquires. Give up at " +
		strconv.FormatFloat(aiAttackerKeepReturnRange, 'f', 0, 64) +
		" (the profile's KeepReturnRange, from the encounter's start point), re-plan every " +
		strconv.FormatFloat(monsterChaseReplan.Seconds(), 'f', 1, 64) + " s (FieldChar_FSM.cpp:62). " +
		"SPEED is SCR_Get_MON_MSPD = MSPD*(MSPDFix*0.01+1)*(100+MSPD_BM)/100 floored - " +
		"the value CMonsterNPC::GetMoveSPD answers (source2\\geServer\\ZServerLib\\" +
		"MonsterNPC.cpp:1929, GetMoveSPD -> GetMON_MSPD) and the move FSM puts in the " +
		"3550 (FieldChar_FSM.cpp:599, m_moveSpeed = GetMoveSPD) - e.g. class 11591 " +
		"Monster_Bigfoot MSPD 143 walks at 143 with NO engagement, NOT WSPD 130 and NOT " +
		"RSPD 856. A body WITH one (a chase or an attack order in flight) RUNS at the " +
		"row's SPDBM rule, floor(MSPD*(MSPDFix*0.01+1)*(100+SPDBM)/100) - 11591 SPDBM " +
		"391 is 702 - and drops back to its MSPD when the engagement ends; every such " +
		"change writes one zone: MONSTER RUN line."
}
