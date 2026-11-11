// THE ORDER THE BATTLE SCRIPT GIVES THE ENGINE: WALK, ATTACK, STOP.
//
// WHY THIS FILE EXISTS. `ge\script\monster\mon_ts_battle.scp` is the game's whole
// monster battle AI, and the five calls below are the half of it that puts bytes on
// the wire: a walk becomes ONE `ZC_MOVE_MONSTER` (3550, 26 bytes), an attack request
// arms the body's swing, and a stop parks it again. The owner's ruling (2026-11-07) is
// that the script decides and Go obeys - "whatever the scp is doing it should do by
// itself, nothing gets coded in native Go" - so this file stores what the script says
// and performs the engine's half of it. It chooses no target, no route, no distance
// and no speed: every number is the script's or the body's own row.
//
// WHAT EACH CALL DOES, and the reference line it copies:
//
//	Move(self, x, y, z)      resolve the point through the map's own mesh
//	                         (`Get3DPointToPosLocaly(shape, point, pos, 1000, 300)`,
//	                         FieldCharFunc.cpp:57; our `meshCell`, the same call and
//	                         the same two ranges), stamp the body's `GetMON_MSPD` as
//	                         the move speed (`m_moveSpeed = GetMoveSPD()`,
//	                         FieldChar_FSM.cpp:599) and broadcast ONE 3550 through the
//	                         ORDINARY fan-out (`BroadcastStartMovement`,
//	                         MonsterNPC.cpp:592-601; `BuildMoveMonster` + `fanOut`, the
//	                         same pair `planMonsterChase` uses - no second builder and
//	                         no second fan-out). The FSM's own dispatch refuses a
//	                         point equal to the one the body is already walking to
//	                         (`IsEqualPos(moveTo, m_moveTo)`, FieldChar.cpp:946-949),
//	                         so a repeat order sends nothing.
//	MoveToOwner(self, r)     the same walk, with a point `r` from the body's OWNER as
//	                         the goal (`GetBossPos`, MonsterNPCFunc.cpp:656-695). No
//	                         owner, no order: `GetOwner` is always nil in this server
//	                         (script\status.go:223-228), so this answers "refused" for
//	                         every body that exists today and the owner state is here
//	                         for the minion card.
//	StartAttack(self, t)     STORE the target (`OnStartAttack` -> `RequestAttack`,
//	                         MonsterNPC.cpp:175-179, which refuses a self-target,
//	                         FieldChar.cpp:882-890) and mark the body attacking, which
//	                         is what `IsAttacking` reads. It does NOT swing.
//	StartGroundAttack(self)  store a ground-attack request (`RequestGroundAttack`,
//	                         FieldChar.cpp:896-899). This engine has no ground attack,
//	                         so nothing is sent.
//	StopAttack(self)         clear the action state when it is an attack state
//	                         (`OnStopAttack`, MonsterNPC.cpp:186-199) and park the
//	                         body's swing until a later StartAttack.
//
// THE ONE THING THIS FILE ADDS TO THE SWING, AND WHY IT IS NOT A SECOND IMPLEMENTATION:
// a body's swing is `monsterSwingTick` -> `monsterSwing` (zone\monster_swing.go) and
// stays that. The tick now asks `Handler.monsterMaySwing` before it swings a body, and
// that predicate answers TRUE unless the script has explicitly stopped that body. So
// the pre-script engine is byte-identical (no body ever has a stop recorded), while a
// `StopAttack` really stops and a later `StartAttack` really resumes - which is the
// pass condition this card is measured by.
//
// THE OTHER THING THIS FILE DOES TO THE SWING - AN ATTACK ORDER IS A TARGET, NOT A WALK.
// `mon_ts_battle.scp` decides WHO and calls `StartAttack`; it contains no `Move` toward
// the target (its only `Move`, `:253`, is a home/return). The approach between deciding
// and swinging is the ENGINE's in the reference too: `OnStartAttack` raises
// `RequestAttack(handle)` (MonsterNPC.cpp:175-179) and the attack state walks the body
// into reach before each swing (FieldChar_FSM.cpp:121-160). Ours lived in
// `monsterChaseTick`, which `-monai script` switches off (zone\chase.go:710) - so a
// scripted body held an order it could not reach with, and stood at its spawn point
// while the player walked up to it (the owner's report, 2026-11-08). The fix is the
// engine's half and nothing else: the DECISION stays the script's, and the walk is the
// chase's own machinery (`planMonsterWalk` / `advanceMonsterLeg` / the 3550). It steps
// from `monsterApproachTick`, the approach's OWN clock on `tickIfDue`, and - for the
// swing's own ordering - from `monsterMaySwing` as well; the step is idempotent, so the
// two callers cannot walk a body twice. See the section on `monsterApproachStep`, and
// `monsterApproachTick` for why riding the SWING's clock was not enough.
//
// THE STATE IS THE BODY'S SCRIPT STATE, not a second per-handle map: the order lives in
// `monsterEngineState.order` (zone\monster_engine.go), guarded by `spawnMu` and pruned
// against the roster with the rest of that state, so it dies with the body. That is the
// one structural edit this card makes outside its own new files, and it is the placement
// the card's own handoff asks for ("add the move order and the attack request TO THAT
// STATE ... a second per-handle map is the failure mode").
//
// WHAT IS DELIBERATELY NOT HERE:
//
//   - no walk ADVANCE FROM A BINDING. `Move` records from / goal / speed / began, and
//     the walk IT ordered is drawn by the client from the 3550 (the server's point
//     lags it - a seam stage 3 owns). The ENGINE's approach below does advance a body,
//     because it is a tick of the zone's clock and not a call: one implementation
//     (`advanceMonsterLeg`), shared with the chase.
//   - no collision query beyond the mesh. The reference also refuses a point the body's
//     shape collides at (`m_world->IsCollision(m_shape, moveTo)`, FieldChar.cpp:947-949);
//     this zone has no shape-collision query, and the mesh point `meshCell` answers is
//     walkable ground, so the mesh IS the guard here.
//   - no chase hand-off. A script `Move` on a body the chase is driving does not cancel
//     the chase: the chase's own order and cancel are its own (and out of scope for this
//     card). The two cannot both run until stage 3 hands the driving to the script, and
//     then the chase's autonomous half is what gets retired.
package zone

import (
	"fmt"
	"math"
	"time"

	"geserver/script"
)

// The zone IS the world behind the five movement/attack builtins
// (script\monster_order.go). This line fails the build, not a script run, if a
// signature ever drifts.
var _ script.OrderWorld = (*Handler)(nil)

// monsterOrder is the order the battle script last gave ONE body.
//
// It is STATE, in the sense the card uses the word: the engine stores what the script
// said and answers questions about it. Nothing here decides anything - a target is a
// target because StartAttack named it, and a goal is a goal because Move named it.
type monsterOrder struct {
	// owner is the body's owner handle, 0 for none - the `GetOwner` this server never
	// had (script\status.go:223-228 answers nil for every body). `SetMonsterOwner` is
	// the writer; the minion card is its consumer.
	owner uint32

	// move is the walk the script last ordered.
	move monsterMoveOrder

	// attack is the attack request the script last gave.
	attack monsterAttackOrder

	// approach is the ENGINE's walk into reach for the order above: the target, the
	// leg, and the two flags that make the walk happen once per order (see the section
	// on `monsterApproachStep`). It is EMPTY for every body until a script orders an
	// attack, which is why the pre-script engine is unchanged.
	approach monsterApproach
}

// monsterMoveOrder is ONE walk: where the body was when the order was given, where it
// was told to go, the route it walks, how fast it walks, and when. Those are the whole
// of the reference's move state (from = m_moveStartPos, goal = m_moveEndPos, began =
// m_moveStartTick, speed = m_moveSpeed; FieldChar_FSM.cpp:591-603), and `route` is the
// map's own polyline between the two (PathEngine.Route), so the body is DRAWN along the
// ground it will actually walk instead of the chord across a wall.
//
// IT IS A LEG, NOT A NOTE (2026-11-11). The order used to be stored and nothing more:
// the 3550 went out, the client walked the body, and the SERVER's copy stood at the
// spawn point for ever. That made the body invisible to its own approach, to the swing's
// reach test and to the next 3564, and it left `IsEnableMove` with no clock to read - so
// the script re-drew a goal about once a second and flooded 3550s. `posAt` below is the
// clock, and `monsterMoveTick` is the one reader that advances the roster with it.
type monsterMoveOrder struct {
	from  [3]int32
	goal  [3]int32
	route [][3]int32
	speed float64
	began time.Time
	has   bool
}

// monsterNow is the ONE clock the script's order and its move state share. It is
// `MonsterNow` when a test set one (zone\zone.go says why) and `time.Now()` in every
// production run. Two readers only: `orderMonsterMove` stamps a leg's `began` with it,
// and `EnableMove` measures that leg against it.
func (h *Handler) monsterNow() time.Time {
	if h.MonsterNow != nil {
		return h.MonsterNow()
	}
	return time.Now()
}

// posAt answers where this order's body is at `now`, and whether the walk is still in
// flight. It is `monsterLeg.posAt` with the four fields this order owns - ONE walk
// model, not two: the goal when the route has run out (or there was none), the point
// along the polyline while there is ground left, and `false` the moment the leg is
// walked out. A speed of 0 or a missing clock answers "not walking", which is the
// honest answer for a walk nothing can time (a 0-speed 3550 is refused before it is
// stored, so this only covers a hand-built body).
func (o monsterMoveOrder) posAt(now time.Time) ([3]int32, bool) {
	if !o.has || o.began.IsZero() || o.speed <= 0 {
		return o.goal, false
	}
	length := legLength(o.from, o.goal, o.route)
	if walked := o.speed * now.Sub(o.began).Seconds(); walked >= length {
		return o.goal, false
	}
	return walkedRouteAt(o.route, o.from, o.goal, o.began, now, o.speed), true
}

// monsterAttackOrder is the body's attack request: who it was told to attack, whether
// the request was a ground attack, and whether the script has stopped it since.
//
// stopped is the parking brake the swing tick reads (`monsterMaySwing`). It starts
// FALSE for every body - which is exactly why the live engine is unchanged: no body
// ever has it set until a script calls `StopAttack`.
type monsterAttackOrder struct {
	target  uint32
	ground  bool
	stopped bool
	has     bool
}

// ---- the five builtins' engine half ---------------------------------------

// MoveMonster is `Move(self, x, y, z)`: walk the body to the point the script named.
//
// THE POINT IS A REQUEST, AND THE MESH ANSWERS IT. The reference resolves it with
// `world->Get3DPointToPosLocaly(shape, point, pos, 1000, 300)` and sends NOTHING when
// that fails (FieldCharFunc.cpp:57-58); `meshCell` is that call with that shape and
// those two ranges (zone\create.go:74-75, :130-146), so a point off the walkable mesh
// is refused here too, with one line.
func (h *Handler) MoveMonster(mon uint32, x, y, z float64) {
	m, ok := h.monsterOrderBody(mon, "Move")
	if !ok {
		return
	}
	goal, _, err := h.meshCell(x, y, z)
	if err != nil {
		h.reportOnceEngine("order:move:no-ground:"+hexHandle(mon),
			"zone: MONSTER MOVE 0x%08x %q refused: %v - the reference sends no OM_MOVE "+
				"for a point the world cannot resolve (FieldCharFunc.cpp:57-58, "+
				"Get3DPointToPosLocaly with 1000/300), so nothing was sent",
			mon, h.MonsterName(m), err)
		return
	}
	h.orderMonsterMove(m, goal, "the script's point")
}

// MoveMonsterToOwner is `MoveToOwner(self, nearRange)`: walk to a point `nearRange`
// from the body's OWNER.
//
// THE REFERENCE'S ANSWER, read out of `GetBossPos` (MonsterNPCFunc.cpp:656-695): with a
// non-zero range the world hands back a RANDOM walkable point within `nearRange` of the
// owner (`GetRandomPosLocaly(shape, out, bossPos, nearRange)`, :684); the zero-range
// branch is the owner's own point resolved through the shape (:668-676). This zone has
// no random-point sampler, so the point is the one `nearRange` from the owner on the
// line the body would walk - a member of the same set (inside the range, walkable,
// away from the owner rather than on top of it), and it is still the mesh that says
// whether it is ground. A body with NO owner sends nothing, exactly as the reference's
// `if (owner)` does: this server has no ownership yet, so that is every body today.
func (h *Handler) MoveMonsterToOwner(mon uint32, nearRange float64) {
	m, ok := h.monsterOrderBody(mon, "MoveToOwner")
	if !ok {
		return
	}
	owner, ownerOK := h.MonsterOwnerOf(mon)
	if !ownerOK || owner == 0 {
		h.reportOnceEngine("order:move:no-owner:"+hexHandle(mon),
			"zone: MONSTER MOVE 0x%08x %q to owner refused: the body has no owner. The "+
				"reference only moves it inside `if (owner)` (MonsterNPCFunc.cpp:904), and "+
				"this server builds no monster ownership yet (GetOwner answers nil, "+
				"script\\status.go:223-228) - nothing was sent and the home point is NOT "+
				"used in its place", mon, h.MonsterName(m))
		return
	}
	now := time.Now()
	at, live := h.orderOwnerPoint(owner, now)
	if !live {
		h.reportOnceEngine("order:move:owner-gone:"+hexHandle(mon),
			"zone: MONSTER MOVE 0x%08x %q to owner refused: owner 0x%08x is not a live "+
				"body on this field - the reference's GetBossPos needs the owner's shape "+
				"and position (MonsterNPCFunc.cpp:659-663), so nothing was sent",
			mon, h.MonsterName(m), owner)
		return
	}

	patrol := nearRange
	if patrol < 0 {
		patrol = 0
	}
	goalAt := [2]float64{float64(at[0]), float64(at[1])}
	if patrol != 0 {
		// A point `nearRange` from the owner, on the line from the owner to the body.
		dx := float64(m.X) - float64(at[0])
		dy := float64(m.Y) - float64(at[1])
		d := math.Hypot(dx, dy)
		if d < 1 {
			dx, dy, d = 1, 0, 1 // the body stands on the owner: step east
		}
		goalAt = [2]float64{
			float64(at[0]) + dx/d*patrol,
			float64(at[1]) + dy/d*patrol,
		}
	}
	goal, _, err := h.meshCell(goalAt[0], goalAt[1], float64(at[2]))
	if err != nil {
		h.reportOnceEngine("order:move:owner-no-ground:"+hexHandle(mon),
			"zone: MONSTER MOVE 0x%08x %q to owner refused: %v - the reference's "+
				"GetBossPos answers false and sends nothing when the world cannot place "+
				"the point (MonsterNPCFunc.cpp:681-686)", mon, h.MonsterName(m), err)
		return
	}
	h.orderMonsterMove(m, goal, fmt.Sprintf("a point within %g of owner 0x%08x", patrol, owner))
}

// orderMonsterMove stores ONE walk and puts ONE 3550 on the wire.
//
// THE ORDER OF OPERATIONS IS THE REFERENCE'S: `DispatchMove` refuses a point equal to
// the standing one, stores it, and resets the action state (FieldChar.cpp:946-953);
// `MS_MOVE`'s ON_ENTER then stamps the speed and broadcasts
// (FieldChar_FSM.cpp:591-607). The speed is the body's own `GetMON_MSPD`, which is what
// `GetMoveSPD` answers for a monster (`CMonsterNPC::GetMoveSPD -> GetMON_MSPD()`,
// MonsterNPC.cpp:1929-1932) - 143 for class 11591, never WSPD and never RSPD.
func (h *Handler) orderMonsterMove(m Monster, goal [3]int32, how string) {
	speed, known := h.monsterMoveSpeed(m)
	if !known {
		h.reportOnceEngine("order:move:no-speed:"+hexHandle(m.Handle),
			"zone: MONSTER MOVE 0x%08x %q: its row is not in the Monster table, so "+
				"SCR_Get_MON_MSPD has no column to read - NO walk is ordered. A 3550 "+
				"whose speed is 0 is a move the client STARTS and can never advance: "+
				"CActor::MoveTo asserts the speed is not 0 (GeActor.cpp:250) and the "+
				"move state advances by m_updateTime*m_moveSpeed (GeActor.cpp:2254), so "+
				"the body would sit in a frozen walk pose for ever - the one value no "+
				"live 3550 carries (0 of 494 in captures\\kill01, kite4000c and chasec20261108)",
			m.Handle, h.MonsterName(m))
		return
	}

	// THE DEDUPE FIRST, THE ROUTE SECOND, THE ORDER THIRD. The route is a process against
	// the navmesh (PathEngine.Route) and `spawnMu` is the lock every session's tick takes,
	// so the plan happens between two SHORT lock windows instead of inside one. The first
	// window answers the two cheap questions (`st` exists, is this the same goal), and only
	// an order that will really replace the walk pays for a plan.
	h.spawnMu.Lock()
	st := h.engineLocked(m.Handle)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	if st.order.move.has && st.order.move.goal == goal {
		h.spawnMu.Unlock()
		return // THE SAME POINT: one order, never one per script pass (FieldChar.cpp:946-947)
	}
	from := [3]int32{m.X, m.Y, m.Cell}
	h.spawnMu.Unlock()

	// A route the engine does not answer means "walk the straight line" - the client is
	// told the SAME destination either way, so nothing on the wire depends on this
	// answer; only the point the server draws the body at does.
	var route [][3]int32
	if h.Route != nil {
		if r, ok := h.Route(from, goal); ok {
			route = r
		}
	}

	now := h.monsterNow()
	h.spawnMu.Lock()
	if st = h.engineLocked(m.Handle); st == nil {
		h.spawnMu.Unlock()
		return // the body left the field while the route was being planned
	}
	st.order.move = monsterMoveOrder{from: from, goal: goal, route: route,
		speed: speed, began: now, has: true}
	// `DispatchMove` ends with `SetActionState(AS_NONE)` (FieldChar.cpp:952): a walk
	// that is actually dispatched takes the body out of its attack action, which is
	// what `IsAttacking` reads, and the engine's approach for the old order is over
	// with it - otherwise two walks (the script's goal and the engine's stand point)
	// would steer one body. It is the same field `markMonsterAttacking` writes, and
	// only an EXISTING table is touched: a walk must not create hate state.
	wasRun := st.order.approach.run
	st.order.approach = monsterApproach{}
	if hs := h.hateLocked(m.Handle, false); hs != nil {
		hs.attacking = false
	}
	h.spawnMu.Unlock()

	if wasRun {
		// The approach's engagement is over with it, and this walk is the
		// no-engagement MSPD: close the MONSTER RUN line once.
		if walk, run, spdbm, known := h.monsterWalkAndRun(m); known {
			h.logMonsterRun(m, run, walk, spdbm)
		}
	}

	pkt := BuildMoveMonster(m.Handle, goal, uint32(nowMillis()), float32(speed))
	sent := h.fanOut(nil, m.Handle, pkt)
	h.Log.Printf("zone: MONSTER MOVE 0x%08x %q walks to %s (%d, %d, cell %d) from "+
		"(%d, %d, cell %d) at %.0f u/s from SCR_Get_MON_MSPD - cmd %d (%d B) to %d "+
		"session(s); the ORDER is stored WITH ITS LEG and monsterMoveTick advances the "+
		"body along it at that speed until it arrives",
		m.Handle, h.MonsterName(m), how, goal[0], goal[1], goal[2],
		from[0], from[1], from[2], speed, CmdZCMoveMonster, len(pkt), sent)
}

// ---- the script move's LEG: an ordered walk the body actually walks ----------------
//
// WHAT WAS WRONG (card t_16ab5afc, measured on fld_gig with `-monai script`). A script
// move order stored its four fields and put ONE 3550 on the wire, and then nothing ever
// moved the SERVER's copy of the body: 1,245 `MONSTER MOVE` lines in 40 s at 217 u/s for
// 61 bodies, a ~1 s re-draw per body, and a body whose own point never left its spawn
// cell. The placement it left was observable in three ways, and all three are the card:
//
//	deaf script   `SCR_MON_AI_LIB_CHECK_ENABLEMOVE` (mon_ai_lib.scp:72-83) asks
//	              `IsEnableMove`, whose answer is the MOVE STATE - and a walk with no
//	              clock is MS_STOP for ever, so the answer was 'YES' on the very next
//	              pass and the NONE loop drew a fresh goal about once a second
//	flood         each draw sent a 3550 that REPLACED the walk in flight, so the client
//	              was told to start a new leg before the last one finished
//	blind engine  the swing's reach, the sight ring and the next 3564 read the roster,
//	              which stood still, so a body walking 500 units still looked where it
//	              was born
//
// `monsterMoveTick` is the clock. It reads the leg the order stored (`posAt`, the same
// four fields and the same polyline the chase's `monsterLeg` walks) and writes the point
// into the roster through the SAME writer every other monster walk uses
// (`advanceMonsterLeg` -> `setMonsterPos`'s field), so a script walk and a Go chase are
// one walk model with one reader.
func (h *Handler) monsterMoveTick(now time.Time) {
	if !h.MonsterAIScript {
		return
	}
	// ROSTER ORDER, not map order: two runs of the same field must log and advance in
	// the same order (the approach's clock makes the same promise).
	for _, m := range h.runtimeMonsters() {
		h.monsterMoveStep(m.Handle, now)
	}
}

// monsterMoveStep advances ONE body along the leg its script order stored, by one step.
//
// IT IS A ZONE STEP: `chaseMu` then `spawnMu`, the house order (`monsterApproachStep`
// and `monsterEnableMove` take the same pair), because a body's point and its order must
// move together and because this is reached from a session's tick while another session
// may be in the same instant. The point itself is drawn from ABSOLUTE time
// (`monsterMoveOrder.posAt`), so two sessions reaching the tick advance the body to the
// same point instead of twice as far - the same guarantee `advanceMonsterLeg` gives.
//
// IT WRITES ONLY WHEN THE POINT CHANGED, which is what keeps a body at rest free: the
// arrival writes the goal ONCE and every later pass compares equal.
func (h *Handler) monsterMoveStep(handle uint32, now time.Time) {
	h.chaseMu.Lock()
	defer h.chaseMu.Unlock()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()

	st := h.engineLocked(handle)
	if st == nil || !st.order.move.has {
		return
	}
	pos, _ := st.order.move.posAt(now)
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle != handle {
			continue
		}
		if h.spawnedMonster[i].X == pos[0] && h.spawnedMonster[i].Y == pos[1] &&
			h.spawnedMonster[i].Cell == pos[2] {
			return
		}
		h.spawnedMonster[i].X, h.spawnedMonster[i].Y, h.spawnedMonster[i].Cell =
			pos[0], pos[1], pos[2]
		return
	}
}

// StartMonsterAttack is `StartAttack(self, target)`: store the target and mark the body
// attacking. It sends nothing - the swing is the engine's (see the file header).
//
// THE TWO GUARDS ARE THE REFERENCE'S: a target that is the body itself is refused
// (`if (targetID == m_handle) return;`, FieldChar.cpp:884-888) and a target the world
// does not hold is dropped, because `DispatchAttack` resolves it with `GetWorld()->Get`
// and returns on NULL (FieldChar.cpp:963-967).
func (h *Handler) StartMonsterAttack(mon, target uint32) {
	m, ok := h.monsterOrderBody(mon, "StartAttack")
	if !ok {
		return
	}
	if target == mon {
		h.reportOnceEngine("order:attack:self:"+hexHandle(mon),
			"zone: MONSTER ATTACK 0x%08x %q refused a SELF target - the reference logs "+
				"`invalid target` and returns (FieldChar.cpp:884-888), and a body that "+
				"attacks itself is a script bug, not an order", mon, h.MonsterName(m))
		return
	}
	h.spawnMu.Lock()
	h.pruneHateLocked()
	if _, live := h.hateTargetLocked(target); !live {
		h.spawnMu.Unlock()
		h.reportOnceEngine("order:attack:ghost:"+hexHandle(mon)+":"+hexHandle(target),
			"zone: MONSTER ATTACK 0x%08x %q refused target 0x%08x: it is not a live body "+
				"on this field, and the reference's DispatchAttack drops a handle the "+
				"world does not hold (FieldChar.cpp:963-967)", mon, h.MonsterName(m), target)
		return
	}
	st := h.engineLocked(mon)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	was := st.order.attack
	st.order.attack = monsterAttackOrder{target: target, has: true}
	hs := h.hateLocked(mon, true)
	if hs != nil {
		hs.attacking = true
	}
	h.spawnMu.Unlock()

	if was.has && !was.stopped && was.target == target && !was.ground {
		return // the same request again: the line below would say nothing new
	}
	h.Log.Printf("zone: MONSTER ATTACK 0x%08x %q START on 0x%08x %q - the request is "+
		"STORED and the action state is attacking; the swing itself is "+
		"monsterSwingTick -> monsterSwing (zone\\monster_swing.go), which is the only "+
		"thing that sends a 3564",
		mon, h.MonsterName(m), target, h.hateName(target))
}

// StartMonsterGroundAttack is `StartGroundAttack(self)`: store the ground-attack request
// and mark the body attacking.
//
// NOTHING IS SENT, AND THAT IS THE HONEST ANSWER: the reference raises a ground-attack
// request (FieldChar.cpp:896-899) that its own FSM turns into an attack at the body's
// feet. This engine has no ground-attack path, so it stores the request and says so
// ONCE - a 3564 aimed at the ground would be a second attack implementation with a
// layout nobody measured.
func (h *Handler) StartMonsterGroundAttack(mon uint32) {
	m, ok := h.monsterOrderBody(mon, "StartGroundAttack")
	if !ok {
		return
	}
	h.spawnMu.Lock()
	st := h.engineLocked(mon)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	st.order.attack = monsterAttackOrder{ground: true, has: true}
	if hs := h.hateLocked(mon, true); hs != nil {
		hs.attacking = true
	}
	h.spawnMu.Unlock()

	h.reportOnceEngine("order:attack:ground",
		"zone: MONSTER ATTACK 0x%08x %q asked for a GROUND attack: the request is "+
			"stored and IsAttacking answers YES, but this engine has no ground-attack "+
			"path and sends nothing (FieldCharFunc.cpp:181-188 raises the request; the "+
			"FSM's ground attack is not ported). Further ground requests are not logged",
		mon, h.MonsterName(m))
}

// StopMonsterAttack is `StopAttack(self)`: clear the action state and park the swing.
//
// THE STATE CLEAR IS THE REFERENCE'S `OnStopAttack` (MonsterNPC.cpp:186-199): it only
// acts on an attack state and never starts anything. The parking is this engine's half
// of the same meaning - a stopped body does not swing until something starts it again -
// and it is what makes the stop observable (the swing tick reads `monsterMaySwing`).
// The stored attack TARGET is kept: `OnStopAttack` does not clear it either.
func (h *Handler) StopMonsterAttack(mon uint32) {
	m, ok := h.monsterOrderBody(mon, "StopAttack")
	if !ok {
		return
	}
	h.spawnMu.Lock()
	st := h.engineLocked(mon)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	// READ THE ACTION STATE WITHOUT `IsAttacking`: that method takes spawnMu itself,
	// and this is the same field it reads (zone\monster_hate.go:377-384).
	hs := h.hateLocked(mon, false)
	wasAttacking := hs != nil && hs.attacking
	wasTarget := st.order.attack.target
	wasGround := st.order.attack.ground
	// THE REFERENCE ONLY ACTS ON AN ATTACK STATE (`OnStopAttack` checks the state and
	// returns otherwise, MonsterNPC.cpp:188-198): a body that was not attacking has
	// nothing to stop, so this is a no-op and writes no line.
	if !wasAttacking && wasTarget == 0 && !wasGround {
		h.spawnMu.Unlock()
		return
	}
	st.order.attack.stopped = true
	st.order.attack.ground = false
	st.order.attack.has = true
	if hs != nil {
		hs.attacking = false
	}
	h.spawnMu.Unlock()

	h.Log.Printf("zone: MONSTER ATTACK 0x%08x %q STOP (target 0x%08x %q): the action "+
		"state is cleared and the swing is PARKED - monsterSwingTick will not swing "+
		"this body again until a later StartAttack (OnStopAttack, MonsterNPC.cpp:186-199)",
		mon, h.MonsterName(m), wasTarget, h.hateName(wasTarget))
}

// monsterMaySwing is the ONE question `monsterSwingTick` asks this file: may this body
// swing now? TRUE unless the script has explicitly stopped it.
//
// THE DEFAULT IS TRUE, AND THAT IS THE WHOLE "NO LIVE CHANGE" PROOF: no body has a stop
// recorded until a script records one, and no script is run until stage 3, so every body
// the tick sees today answers exactly as it did before this card.
//
// IT ALSO STEPS THE APPROACH: an attack order is a target, not a walk, and this is the
// per-body call the SWING's clock makes into this file. The approach's own clock is
// `monsterApproachTick` (called from `tickIfDue`); this one is kept because the swing's
// reach test and the approach's step must land on the same pass, and because the order
// tests drive the approach through this tick. Both call the SAME step, which is
// idempotent (absolute-time leg, `nextPlan` gate), so a pass that reaches both plans
// once. ONE STALENESS IS NAMED, NOT HIDDEN: the swing's reach test in this same tick uses
// the roster point the tick read BEFORE this call, so a body that arrives on this step is
// first swung at on the next one (100 ms); the point this step writes is what every later
// reader, and the 3564 that names the body's position, sees.
func (h *Handler) monsterMaySwing(handle uint32) bool {
	if h.MonsterAIScript {
		h.monsterApproachStep(handle, time.Now())
	}
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if st := h.monsterEngines[handle]; st != nil && st.order.attack.stopped {
		return false
	}
	return true
}

// ---- the ENGINE's approach: an attack order walks into reach ----------------------
//
// WHAT THIS SECTION IS FOR. `mon_ts_battle.scp` decides WHO to attack and calls
// `StartAttack` (":163, :172); it never walks toward the target (its only `Move`,
// ":253", is a home/return). The approach between deciding and swinging is the
// ENGINE's: the reference's `OnStartAttack` raises `RequestAttack(handle)`
// (MonsterNPC.cpp:175-179) and the attack state walks the body into reach before each
// swing (FieldChar_FSM.cpp:121-160). This is that half, and nothing else: the DECISION
// is still the script's.
//
// IT ACQUIRES NOTHING. The target is the one the order stored; the script's own
// `GetObjectLocaly` -> `InsertHate` -> `ChangeTactics(TS_BATTLE)` is the acquisition
// (mon_ts_none.scp:219-240), and `monsterChaseTick`'s acquisition branch stays
// unreachable in this mode.
//
// ONE WALK IMPLEMENTATION. The leg is the chase's `monsterChaseState` and the walk is
// `planMonsterWalk` + `advanceMonsterLeg` (zone\monster_chase.go) - the same reach ring
// (MaxR + both body radii), the same path engine, the same one-order-per-stand-point
// rule, the same 3550 through the ordinary fan-out. The only thing this file adds is
// WHEN to ask: the reference's own 300 ms AS_ATTACK_CHASE cadence
// (`m_nextAttackChaseTime = timeGetTime() + 300`, FieldChar_FSM.cpp:62-63).
//
// THE APPROACH ENDS on the target dying or leaving (`playerBodyByHandle`), on
// `StopAttack` (the leg freezes where the body got to), on a different target ordered
// (the state is rebuilt from the body's current point), or on the body arriving - the
// swing's own reach check (`attackRangeAllowed`, zone\monster_swing.go:176) says
// in-range and the walk stops, leaving the 3564 to the swing exactly as today.

// monsterApproach is one body's approach for the order it holds now: the walk state
// (the chase's own, so the walk code is the chase's) and the flags that make it happen
// once per order instead of once per tick.
type monsterApproach struct {
	chase  monsterChaseState // target, body, leg, lastStand, nextPlan - the chase's own walk state
	logged bool              // the ONE line has been written for this order's target
	ended  bool              // the target is gone; the order stays, the walk does not

	// run is true while the MONSTER RUN line is in force for this approach: the body
	// is walking at the row's SPDBM speed, and the line back to its MSPD is owed when
	// the approach ends (target gone, StopAttack, arrival, or a script Move).
	run bool

	// targetAt/hasTarget are the point the CURRENT leg was planned for. One order per
	// TARGET POINT, not one per 300 ms: see `monsterApproachStep`.
	targetAt  [3]int32
	hasTarget bool
}

// monsterApproachTick is the approach's OWN clock, and it is the thing that turns an
// armed order into a 3550.
//
// WHY IT EXISTS. `-monai script` switches `monsterChaseTick` off (zone\chase.go:710), so
// the Go chase's clock is not the one that walks an ordered body. The approach's step
// used to ride `monsterMaySwing` - the swing tick's per-body question - which is a CLOCK
// OF THE SWING, not of the order: it skips every body whose row states no attack
// (`m.ATK <= 0 || m.MaxR <= 0 || m.ASPD <= 0`), so a body ordered to attack by the
// script with a row that names no attack never took a step. This loop has no such
// filter: an order is an order.
//
// THE PERIOD IS `pollInterval` (100 ms, zone\chase.go:87), the same clock the chase,
// the swing and the sight ring ride, and the same one `FieldChar_FSM.cpp:62` re-asks on
// (300 ms). What the period was NOT, before this card, is GUARANTEED: a session's read
// only woke for a tick when the SESSION had work (zone\zone.go's read deadline), so the
// approach advanced as often as the CLIENT sent a frame. That is the measured stall in
// `logs\zone_go_script.log`: an order at 08:19:26.906, the first 3550 at 08:19:29.021,
// and no tick at all for 5 s after 08:19:41. `Handler.sessionClockAwake` now wakes the
// read in script mode, so the period is the tick's own.
//
// GO MODE IS UNTOUCHED: the first line returns, and `monsterMaySwing` keeps its step
// exactly as stage 3b left it. Two callers, one step, and the step is idempotent by
// construction (the leg is drawn from absolute time and `nextPlan` gates the plan), so a
// pass that reaches both costs one no-op.
func (h *Handler) monsterApproachTick(now time.Time) {
	if !h.MonsterAIScript {
		return
	}
	// ROSTER ORDER, not map order: two runs of the same fight must log the same order.
	for _, m := range h.runtimeMonsters() {
		h.monsterApproachStep(m.Handle, now)
	}
}

// monsterApproachStep advances ONE body's script-ordered approach by one step.
//
// It is a ZONE step, not a session's: `chaseMu` is the lock the chase's step holds for
// the same reason (a body's point, its target and its leg must move together), and the
// walk itself is drawn from absolute time (`monsterLeg.posAt`), so two sessions reaching
// the tick in the same instant advance the body to the same point instead of twice as
// far. The re-plan is held to one per 300 ms per body (`nextPlan`).
//
// ONE ORDER PER TARGET POINT. `planMonsterWalk` decides "the same stand point, send
// nothing" by comparing the GOAL, and the goal is computed FROM THE BODY'S OWN POINT
// (`PathEngine.Plan` -> `pointTowards(target, body, standoff)`), so a body that walks a
// bent route gets a slightly different goal every re-plan and the dedup never fires: the
// measured run sent 4 x 3550 every 300 ms for 12 s (`logs\zone_go_script.log`,
// 08:19:29-41) while the target stood or ran. A client told to walk to a moving point
// every 300 ms cannot draw a walk. So a leg that is still walking is KEPT while the
// target's point is unchanged, and only a target that MOVED re-plans - which is the same
// gate the reference puts on its own chase (`m_lastOptimalChaseTargetPos == targetDest`
// -> no request, MonsterNPC.cpp:1069-1071) and its `IsEqualPos(moveTo, m_moveTo)` send.
// A body that is NOT walking (its leg ran out short of the ring, or the engine answered
// no route) is re-planned on the 300 ms cadence, so a failed plan is not permanent.
func (h *Handler) monsterApproachStep(handle uint32, now time.Time) {
	if !h.MonsterAIScript {
		return
	}
	h.chaseMu.Lock()
	defer h.chaseMu.Unlock()

	h.spawnMu.Lock()
	m, onField := h.monsterLocked(handle)
	if !onField {
		h.spawnMu.Unlock()
		return
	}
	st := h.engineLocked(handle)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	ord := st.order.attack
	if !ord.has || ord.target == 0 || ord.ground {
		// THE APPROACH IS OVER (no order, a cleared target, or a ground attack that
		// replaces it): the body goes back to its no-engagement MSPD. The line is
		// written outside the lock, like every other line this step writes.
		wasRun := st.order.approach.run
		st.order.approach = monsterApproach{} // no attack order: no approach to keep
		h.spawnMu.Unlock()
		if wasRun {
			if walk, run, spdbm, known := h.monsterWalkAndRun(m); known {
				h.logMonsterRun(m, run, walk, spdbm)
			}
		}
		return
	}
	ap := st.order.approach
	if ap.chase.target != ord.target {
		// A DIFFERENT TARGET than the walk was for (or the order's first step): the
		// body walks from where it stands now, and the ONE line is owed again. The
		// body stays ENGAGED across the change, so an open MONSTER RUN line stays open.
		ap = monsterApproach{chase: monsterChaseState{target: ord.target,
			body: [3]int32{m.X, m.Y, m.Cell}}, run: ap.run}
	}
	stopped := ord.stopped
	h.spawnMu.Unlock()

	if ap.ended {
		return // the target is gone: nothing to walk for, and nothing to say twice
	}

	v, held := h.playerBodyByHandle(ord.target, now)
	if !held {
		ap.ended = true
		ap.chase.leg.active = false
		ap.chase.hasStand = false
		h.endApproachRun(m, &ap)
		h.storeMonsterApproach(handle, ap)
		h.logApproachEnd(handle, ord.target, "the target is no longer a live body on this "+
			"field (dead or gone) - the reference clears the chase on a NULL target "+
			"(FieldChar_FSM.cpp:42-45)")
		return
	}

	// THE BODY FIRST, exactly as the chase's step does it: this reads where the body has
	// walked TO, never where the last order sent it.
	h.advanceMonsterLeg(handle, &ap.chase, now)

	if stopped {
		// `StopAttack` ends the walk where the body got to. The parking itself is this
		// file's answer above (`monsterMaySwing`), and a later StartAttack resumes the
		// walk from this point.
		ap.chase.leg.active = false
		ap.chase.hasStand = false
		h.endApproachRun(m, &ap)
		h.storeMonsterApproach(handle, ap)
		return
	}

	monR := h.monsterBodyRadius(m)
	if _, in := attackRangeAllowed(ap.chase.body, v.pos, m.MinR, m.MaxR, monR, PCBodyRadius); in {
		// ARRIVED: the swing's own reach check is in charge from here. The stand point
		// is ON the ring, so the body stops inside its reach and the next tick swings.
		ap.chase.leg.active = false
		ap.chase.hasStand = false
		h.endApproachRun(m, &ap)
		h.storeMonsterApproach(handle, ap)
		return
	}

	// THE LEG IN FLIGHT IS KEPT, unless the target has moved since it was planned.
	if ap.hasTarget && ap.targetAt == v.pos && ap.chase.leg.active {
		h.storeMonsterApproach(handle, ap)
		return
	}
	if now.Before(ap.chase.nextPlan) {
		h.storeMonsterApproach(handle, ap)
		return
	}
	// AN APPROACH IS AN ENGAGEMENT: it walks the body into reach at the row's RUN
	// speed (`monsterRunSpeed`), the same rule the go chase uses - both read it through
	// `monsterWalkAndRun`, so there is one rule and not two. A body with no engagement
	// (a script `Move`, a walk home) keeps the row's MSPD.
	walk, run, spdbm, known := h.monsterWalkAndRun(m)
	if !known || run <= 0 {
		h.reportOnceEngine("order:approach:no-speed:"+hexHandle(handle),
			"zone: MONSTER APPROACH 0x%08x %q: its row is not in the Monster table, so "+
				"neither SCR_Get_MON_MSPD nor the row's SPDBM has a column to read and the body "+
				"cannot walk into reach - it will stand where it is and swing at what comes to it",
			handle, h.MonsterName(m))
		return
	}
	ap.chase.nextPlan = now.Add(monsterChaseReplan)
	ap.targetAt, ap.hasTarget = v.pos, true

	// THE CLEAR GROUND IS EDGE TO EDGE, the same arithmetic the reach test uses:
	// centre distance minus both bodies (FieldCharFunc.cpp:254-258).
	gap := flatDist(ap.chase.body, v.pos) - (monR + PCBodyRadius)
	started := time.Now()
	p, ok := h.planMonsterWalk(m, &ap.chase, v.pos, monR, run, now, "APPROACH")
	// THE PLAN'S OWN COST, ALWAYS ON IN SCRIPT MODE: the next run answers whether a late
	// walk is the CLOCK or the path engine, with a number instead of a guess. `h.Plan` is
	// one long-lived `pathtool.exe` per zone (zone\nav.go:100-108), and the first call of
	// a run pays the mesh load, so the first line of a run is expected to be the slow one.
	took := time.Since(started)
	h.Log.Printf("zone: PLAN 0x%08x took %s for %d waypoint(s), goal (%d, %d, cell %d), "+
		"%d B to %d session(s) - the map's own path engine", m.Handle, took.Round(time.Microsecond),
		p.waypoints, p.goal[0], p.goal[1], p.goal[2], p.size, p.sent)
	if ok && !ap.run && run != walk {
		// THE FIRST 3550 AT THE RUN SPEED OPENS THE LINE: once per engagement, and only
		// when the row's SPDBM actually changes the number.
		h.logMonsterRun(m, walk, run, spdbm)
		ap.run = true
	}
	if ok && !ap.logged {
		// ONE LINE PER APPROACH, and it is the whole observable: the body, the target,
		// the gap it starts from, the route it walks and the stand point it walks to.
		// A later re-plan (the target moved) sends its 3550 without a second line.
		ap.logged = true
		h.Log.Printf("zone: MONSTER APPROACH 0x%08x -> 0x%08x at %d units of clear ground, "+
			"walking %d to (%d, %d)", m.Handle, ord.target, int(gap), int(p.routeLen),
			p.goal[0], p.goal[1])
	}
	h.storeMonsterApproach(handle, ap)
}

// endApproachRun closes the MONSTER RUN line for an approach that is ENDING - the
// target died or left, StopAttack parked the body, the body arrived inside its reach,
// or a script Move replaced the walk. It writes the one line back to the body's
// no-engagement MSPD and clears the flag, so a later approach opens it again. Caller
// holds no spawnMu (the line is written outside it, like every other line here).
func (h *Handler) endApproachRun(m Monster, ap *monsterApproach) {
	if !ap.run {
		return
	}
	if walk, run, spdbm, known := h.monsterWalkAndRun(m); known {
		h.logMonsterRun(m, run, walk, spdbm)
	}
	ap.run = false
}

// logApproachEnd writes the ONE line about an approach that ends because the target is
// gone, and says where the body stands and what happens to the order. The point is read
// from the ROSTER (not from the copy the caller loaded), because the body may have walked
// since that copy was taken.
func (h *Handler) logApproachEnd(handle, target uint32, why string) {
	h.spawnMu.Lock()
	m, onField := h.monsterLocked(handle)
	h.spawnMu.Unlock()
	if !onField {
		return // the body left the field with the target: nothing to walk any more
	}
	h.Log.Printf("zone: MONSTER APPROACH 0x%08x %q -> 0x%08x ENDS: %s - the walk stops "+
		"where the body stands (%d, %d, cell %d) and nothing swings at it; the ORDER is "+
		"still stored, so a later StartAttack on a live target rebuilds the walk",
		m.Handle, h.MonsterName(m), target, why, m.X, m.Y, m.Cell)
}

// endApproachesTo ends every approach whose TARGET is one of these character handles -
// the rule the zone's clock cannot apply when the last player leaves, because a zone
// with no sessions has no tick at all.
//
// WHO CALLS IT: `leaveZone` (zone\players.go), with the handles of the team that is
// going. Without it the measured run dropped four engagements only when the player
// RECONNECTED as a new handle (logs\zone_go_script.log, 08:19:46 logout -> 08:22:32
// stop), because the script's own thread was not resumed either. The reference ends the
// chase on the tick that finds the target NULL (`FieldChar_FSM.cpp:42-45`); with no tick
// left to run, this is that tick.
func (h *Handler) endApproachesTo(gone []uint32) {
	if !h.MonsterAIScript || len(gone) == 0 {
		return
	}
	targets := make(map[uint32]bool, len(gone))
	for _, handle := range gone {
		targets[handle] = true
	}
	h.chaseMu.Lock()
	defer h.chaseMu.Unlock()

	type ended struct {
		handle uint32
		target uint32
	}
	var ends []ended
	h.spawnMu.Lock()
	for _, m := range h.spawnedMonster {
		st := h.monsterEngines[m.Handle]
		if st == nil {
			continue
		}
		ord := st.order.attack
		if !ord.has || ord.ground || ord.target == 0 || !targets[ord.target] {
			continue
		}
		if st.order.approach.ended {
			continue
		}
		ap := st.order.approach
		ap.ended = true
		ap.chase.leg.active = false
		ap.chase.hasStand = false
		st.order.approach = ap
		ends = append(ends, ended{handle: m.Handle, target: ord.target})
	}
	h.spawnMu.Unlock()

	for _, e := range ends {
		h.logApproachEnd(e.handle, e.target, "the target left the field (LEAVE_PC)")
	}
}

// storeMonsterApproach writes the walk back to the body's state - unless a NEW order
// landed while this step was walking, in which case that order's own step owns the
// state and this one must not overwrite it.
func (h *Handler) storeMonsterApproach(handle uint32, ap monsterApproach) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	st := h.monsterEngines[handle]
	if st == nil || st.order.attack.target != ap.chase.target {
		return
	}
	st.order.approach = ap
}

// ---- the owner state (the minion card's consumer) -------------------------

// SetMonsterOwner records a body's owner - the `GetOwner` this server has never had.
//
// WHO CALLS IT: the minion card, at `CreateSmonster`, whose last argument is already the
// owner (`CreateSmonster(curZoneID, self.MinionID, ..., self)`, mon_ts_battle.scp:232).
// Until then every body answers "no owner" and `MoveToOwner` refuses, which is the state
// this server is honestly in. false means the handle is not a body on this field.
func (h *Handler) SetMonsterOwner(mon, owner uint32) bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.engineLocked(mon)
	if st == nil {
		return false
	}
	st.order.owner = owner
	return true
}

// MonsterOwnerOf answers a body's stored owner, and false when the handle is not a body
// on this field.
func (h *Handler) MonsterOwnerOf(mon uint32) (uint32, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.monsterEngines[mon]
	if st == nil {
		return 0, false
	}
	return st.order.owner, true
}

// MonsterOrderOf answers the order stored for one body, for a test and for the stage-3
// runner. false means the handle is not a body on this field.
func (h *Handler) MonsterOrderOf(mon uint32) (monsterOrder, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneEngineLocked()
	st := h.monsterEngines[mon]
	if st == nil {
		return monsterOrder{}, false
	}
	return st.order, true
}

// ---- helpers -------------------------------------------------------------

// monsterOrderBody answers the roster row for a handle a binding named, saying ONCE per
// handle why an order went nowhere. The body must be on the field: a handle this zone
// never spawned has no state to steer (the same rule `hateLocked` keeps).
func (h *Handler) monsterOrderBody(mon uint32, call string) (Monster, bool) {
	h.spawnMu.Lock()
	h.pruneEngineLocked()
	m, ok := h.monsterLocked(mon)
	if ok {
		h.engineLocked(mon) // create the body's state while we are here
	}
	h.spawnMu.Unlock()
	if !ok {
		h.reportOnceEngine("order:refused-handle:"+hexHandle(mon),
			"zone: MONSTER ORDER %s(0x%08x) refused - that handle is not a body on this "+
				"field, so no order was stored and no packet was sent (further refusals "+
				"are not logged)", call, mon)
		return Monster{}, false
	}
	return m, true
}

// orderOwnerPoint answers where one body's OWNER stands, and whether it is a live body
// on this field. The reference's `GetBossPos` asks the owner's shape and position
// (MonsterNPCFunc.cpp:660-665); this answers the position for the two kinds of owner
// this server can have - a player character (drawn where the sight ring draws it) and
// another monster (its roster point, while its HP is above 0).
func (h *Handler) orderOwnerPoint(owner uint32, now time.Time) ([3]int32, bool) {
	if v, ok := h.playerBodyByHandle(owner, now); ok {
		return v.pos, true
	}
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	m, ok := h.monsterLocked(owner)
	if !ok {
		return [3]int32{}, false
	}
	if hp, live := h.monsterHP[owner]; live && hp <= 0 {
		return [3]int32{}, false
	}
	return [3]int32{m.X, m.Y, m.Cell}, true
}

// monsterMoveSpeed answers `GetMoveSPD` for a body with NO engagement: `SCR_Get_MON_MSPD`
// of its row (`MSPD * (MSPDFix*0.01+1) * (100+MSPD_BM)/100`, floored,
// monster\get_status_monster.lua:535-547), which is what `CMonsterNPC::GetMoveSPD` hands
// the move state (MonsterNPC.cpp:1929-1932). An ENGAGED body runs at `monsterRunSpeed`
// instead - `monsterWalkAndRun` answers both from the same row.
//
// THE ROW IS THE SOURCE, NOT `Monster.ChaseSpeed`: that copy is 0 for a body whose
// tactics family does not chase, and a mission prop still walks. `ChaseSpeed` is only
// the fallback for a hand-built body with no table behind it.
func (h *Handler) monsterMoveSpeed(m Monster) (float64, bool) {
	walk, _, _, known := h.monsterWalkAndRun(m)
	return walk, known
}

// hexHandle writes a handle the way the zone's log lines do.
func hexHandle(handle uint32) string {
	return fmt.Sprintf("%08x", handle)
}
