// THE BATTLE SCRIPT'S MOVEMENT AND ATTACK CALLS - the half that puts bytes on the
// wire. Bound here, decided NOWHERE here.
//
// WHY THIS FILE EXISTS. `ge\script\monster\mon_ts_battle.scp` is the game's whole
// monster battle AI, and after it has chosen a target (stage 1's hate table) and a
// tactic (stage 2a) it does three things: it walks the body somewhere, it starts an
// attack, and it stops one. Those are the calls below. The owner's ruling (2026-11-07)
// is that the script DECIDES and Go only obeys - "whatever the scp is doing it should
// do by itself, nothing gets coded in native Go" - so no call here chooses a target,
// a distance, a route or a speed: each one performs exactly what its reference
// counterpart performs and answers nothing.
//
// WHAT IS BOUND, with the arity the script itself calls it with (every call site in
// `mon_ts_battle.scp` and `mon_ai_lib.scp` read before this file was written):
//
//	Move(self, x, y, z)          mon_ts_battle.scp:253; mon_ai_lib.scp:1219, :1229,
//	                             :1263, :1273, :1317, :1362, :1370, :1378, :1380,
//	                             :1394, :1396, :1404, :1472, :1486, :1517, :1527,
//	                             :1530, :1536, :1545     - FOUR args, always
//	MoveToOwner(self, nearRange) mon_ts_battle.scp:190 (100); mon_ts_none.scp:292
//	                             (300); mon_tactics_*.scp    - TWO args, always
//	RandomMove(self, min, max)   mon_ts_none.scp:276, :302, :311; mon_ai_lib.scp:732,
//	                             :1314, :1359; mon_ai_event.scp:20, :25, :62, :67;
//	                             mon_tactics_*.scp - THREE args, always
//	RandomMoveCreatedLocaly(self, max)
//	                             mon_ts_none.scp:280; mon_tactics_artifact.scp:55,
//	                             :120, :186              - TWO args, always
//	StartAttack(self, target)    mon_ts_battle.scp:163, :172 - TWO args, always
//	StartGroundAttack(self)      mon_ts_battle.scp:68 - ONE arg, always
//	StopAttack(self)             mon_ts_battle.scp:148, :156, :171, :187, :200, :207,
//	                             :214, :254; mon_ai_lib.scp:733 - ONE arg, always
//
// THE TWO RANDOM WALKS ARE THE IDLE BODY'S OWN MOVEMENT, and they are why a body that
// lost its target does not stand still. `mon_ts_none.scp`'s Move block (`:274-296`)
// picks a walk every pass:
//
//	RandomMove(self, moveMin, moveMax)          moveType 'RANDOM'      (:276)
//	RandomMoveCreatedLocaly(self, moveMax)      moveType 'RANDOMLOCAL' (:280)
//
// with `moveMin = self.MON_MSPD * 1.5` and `moveMax = self.MON_MSPD * 3.0` (:9-10). The
// reference's two handlers are `CFieldChar::OnRandomMove` (FieldChar_Message.cpp:565-577)
// and `CMonsterNPC::OnRandomMoveCreatedLocaly` (MonsterNPC.cpp:120-133); both draw the
// point with `CWorld::GetRandomPosLocaly(shape, randomPos, centre, range)`
// (World.cpp:1448-1462), i.e. a RANDOM WALKABLE POINT WITHIN `range` OF A CENTRE, and
// then `RequestMove` it. The centres are the whole difference between the two names:
//
//	RandomMove                the body's CURRENT point (`m_pos`)
//	RandomMoveCreatedLocaly   the body's BIRTH point (`m_createdPos`)
//
// so "wander" on live is really "go somewhere near home", and a body dragged 4,000
// units from its spawn walks all the way back at walk speed instead of standing where
// it stopped and hitting the NONE entry's home-sick kill (mon_ts_none.scp:107-112).
//
// TWO THINGS THE REFERENCE DOES THAT THIS FILE DOES NOT INVENT. (1) `OnRandomMove`
// reads `minRange = arg1` and then NEVER USES IT - only `maxRange` reaches the world
// (`:567-572`, with a TODO at `:570`). The scripts pass a minimum anyway (500..800,
// 200..500, 75..200), so this side honours it: a point at a distance in [min, max].
// (2) `OnRandomMoveCreatedLocaly` retries the draw for up to 100 ms (`:122-132`) because
// a random mesh point can collide; the retry is the ZONE's walk path here (a point the
// mesh cannot resolve sends nothing and says so once), and the script's own 1 s loop
// asks again - this side cannot see the world's answer, so it does not pretend to.
//
// THE CENTRES COME FROM THE WORLD, NEVER FROM A SECOND COPY. The current point is the
// `self` table's own X/Y - the zone refreshes them on every think turn (script\
// monster_ai.go, `refreshMonsterSelf`), which is the script's own view of the body. The
// birth point is `MonsterWorld.Home`, the SAME point `GetMyHomeDistance` /
// `GetMyHomePosition` answer from (`zone\monster_engine.go`, `engineLocked` ->
// `monsterHomeAt`). The height hint the walk path is given is that birth point's own
// `Z` - the only height this world keeps for a body; the reference samples the mesh
// directly and needs none.
//
// THE REFERENCE'S BINDINGS, each read before this file was written:
// `FieldCharFunc.cpp:40-71` (Move), `:163-179` (StartAttack), `:181-188`
// (StartGroundAttack), `:198-207` (StopAttack), `:273-297` (RandomMove),
// `MonsterNPCFunc.cpp:118-139` (RandomMoveCreatedLocaly), `:896-911`
// (MoveToOwner) with `:656-695` (GetBossPos); the message handlers are
// `FieldChar_Message.cpp:246-250` (OnMove -> RequestMove), `:565-577` (OnRandomMove),
// `MonsterNPC.cpp:120-133` (OnRandomMoveCreatedLocaly), `:175-199`
// (OnStartAttack / OnStartGroundAttack / OnStopAttack); the move state machine is
// `FieldChar_FSM.cpp:579-627` (MS_MOVE), the dispatch `FieldChar.cpp:932-953`
// (DispatchMove), and the attack request `FieldChar.cpp:882-898` (RequestAttack /
// RequestGroundAttack).
//
// FOUR THINGS THE CENSUS GOT WRONG, and they change the signatures:
//
//   - `Move` is `(self, x, y, z)` - THREE coordinates, not two. The reference reads
//     `GetFloat(2..4)` (FieldCharFunc.cpp:44-46) and every call site in the corpus
//     passes three. The point is a REQUEST, not the walk: the world resolves it
//     (`Get3DPointToPosLocaly(shape, point, pos, 1000, 300)`, :56) and a point that
//     does not resolve sends NOTHING (:57-58).
//   - `MoveToOwner` is `(self, nearRange)` - TWO args (mon_ts_battle.scp:190 passes
//     100). `GetBossPos` with a NON-ZERO range answers a point within `nearRange` of
//     the OWNER (a random walkable one, World.cpp:GetRandomPosLocaly), not the
//     owner's own point; the zero-range branch is the one that answers the owner's
//     point (MonsterNPCFunc.cpp:669-677).
//   - `StartAttack` has NO one-argument form: the reference dereferences argument 2
//     unconditionally (`context->GetObject(2)->GetOwner()`, FieldCharFunc.cpp:167)
//     and the corpus has zero one-argument calls. A one-argument call is therefore
//     REFUSED here with a line, instead of guessing a target.
//   - NONE of the three attack calls SWINGS. `StartAttack` stores the target
//     (`OnStartAttack` -> `RequestAttack(handle)`, MonsterNPC.cpp:175-179),
//     `StartGroundAttack` stores a ground-attack request (`RequestGroundAttack`,
//     FieldChar.cpp:896-899), and `StopAttack` only clears the action state when it
//     is one of the attack states (`OnStopAttack`, MonsterNPC.cpp:186-199). The
//     swing itself is the FSM's, later, and on this server it is
//     `monsterSwingTick` -> `monsterSwing` (zone\monster_swing.go:236, :270).
//
// WHICH ENGINE FUNCTION EACH NAME ENDS AT, so there is no second implementation:
//
//	Move / MoveToOwner   zone\monster_order.go MoveMonster / MoveMonsterToOwner ->
//	                     BuildMoveMonster + Handler.fanOut, the SAME pair
//	                     `planMonsterChase` uses (zone\monster_chase.go:400-401)
//	                     - one 26-byte 3550 through the ordinary per-viewer fan-out
//	RandomMove /         the SAME MoveMonster, with the point drawn here and the
//	RandomMoveCreatedLocaly  centre answered by the world (the body's live point /
//	                     its birth point). ONE walk implementation, not two
//	StartAttack          zone\monster_order.go StartMonsterAttack: stores the
//	                     request. The swing is `monsterSwing` (zone\monster_swing.go:
//	                     270), the SAME function a monster's tick has always used
//	StartGroundAttack    zone\monster_order.go StartMonsterGroundAttack: stores the
//	                     request. This engine has NO ground-attack path, so nothing
//	                     is sent - inventing a 3564 at the ground would be a second
//	                     attack implementation with a layout nobody measured
//	StopAttack           zone\monster_order.go StopMonsterAttack: clears the action
//	                     state - the field `markMonsterAttacking` writes
//	                     (zone\monster_hate.go:392, the writer stage 1 left for this
//	                     card; the zone writes it under its own lock) - and parks the
//	                     body's swing, which `monsterSwingTick` reads through
//	                     `Handler.monsterMaySwing`
//
// WHAT DOES NOT CHANGE, SAID OUT LOUD: nothing here is called by the zone's own
// clock. Until stage 3 runs the script, a body receives no order, `monsterMaySwing`
// answers true for it, and the chase and the swing tick do exactly what they did
// before this card. A binding never advances a walk and never lands a hit.
//
// THE WORLD IS AN INTERFACE, NOT A MAP - the one-way split `HateWorld` and
// `MonsterWorld` keep: this package knows Lua, the zone knows the field. NIL means
// "no world wired"; every name then does NOTHING and says so ONCE (never a made-up
// packet: a wrong 3550 moves a body, and a wrong 3564 takes HP).
package script

import (
	"fmt"
	"math"

	lua "github.com/yuin/gopher-lua"
)

// OrderWorld is the live world behind the seven movement/attack builtins: one body's
// move order and its attack request, keyed by the MONSTER's handle.
//
// It is implemented by the zone because the data behind it - where a body stands,
// which bodies are alive, where the map's ground is - is the zone's. Nothing returns
// an error: a call that cannot be performed does nothing and the ZONE logs why, the
// contract `SendMsg` keeps (a Lua error aborts the whole script run; a refused order
// does not).
type OrderWorld interface {
	// MoveMonster is `SendMsg(self, self, OM_MOVE, pos)` for a point the world
	// resolved: resolve (x, y, z) through the map's own mesh, stamp the body's
	// MON_MSPD as the move speed and broadcast ONE 3550. A point the mesh cannot
	// resolve, or the same point the body is already walking to, sends nothing
	// (FieldCharFunc.cpp:57-58, FieldChar.cpp:946-949).
	MoveMonster(mon uint32, x, y, z float64)
	// MoveMonsterToOwner is the same walk with the body's OWNER as the goal: a
	// walkable point `nearRange` from the owner, refused when the body has no owner
	// (MonsterNPCFunc.cpp:904-909).
	MoveMonsterToOwner(mon uint32, nearRange float64)
	// StartMonsterAttack stores `target` as the body's attack request and marks the
	// body attacking; a self-target and a handle that is not a live body are
	// refused (FieldChar.cpp:882-890, :955-957).
	StartMonsterAttack(mon, target uint32)
	// StartMonsterGroundAttack stores a ground-attack request. There is no
	// ground-attack path in this engine and nothing is sent.
	StartMonsterGroundAttack(mon uint32)
	// StopMonsterAttack clears the body's action state and parks its swing until a
	// later StartAttack, which is what `OnStopAttack` does (MonsterNPC.cpp:186-199).
	StopMonsterAttack(mon uint32)
}

// installMonsterOrder registers the five names. Called once, from New().
//
// They are SET, not left to the `__on_missing` stub: the stub records a miss and
// answers 0, so a script that believed it had moved a monster would see a silent
// success, and the one line the stub writes would arrive once with no packet behind
// it. A binding that refuses says so itself, on the name it refused.
func (h *Host) installMonsterOrder() {
	h.L.SetGlobal("Move", h.L.NewFunction(h.moveMonster))
	h.L.SetGlobal("MoveToOwner", h.L.NewFunction(h.moveToOwner))
	h.L.SetGlobal("RandomMove", h.L.NewFunction(h.randomMove))
	h.L.SetGlobal("RandomMoveCreatedLocaly", h.L.NewFunction(h.randomMoveCreatedLocaly))
	h.L.SetGlobal("StartAttack", h.L.NewFunction(h.startAttack))
	h.L.SetGlobal("StartGroundAttack", h.L.NewFunction(h.startGroundAttack))
	h.L.SetGlobal("StopAttack", h.L.NewFunction(h.stopAttack))
}

// reportNoOrderWorld is the ONCE line the whole file shares when the host has no zone
// behind it: the reader needs to know that the battle script's orders go nowhere, not
// which name asked first.
func (h *Host) reportNoOrderWorld(name string) {
	h.reportOnce("order:no-world",
		"script: %s did nothing - this host has no monster world wired, so no order "+
			"reaches a body and no packet is sent (further calls are not logged)", name)
}

// moveMonster is `Move(self, x, y, z)` - mon_ts_battle.scp:253 and the seventeen other
// call sites named in the file header.
//
// A missing or non-numeric coordinate is REFUSED rather than read as 0: the reference
// reads `GetFloat(2..4)` and every call site passes three numbers, so a two-argument
// call is a broken script, and a silent 0 would walk a body to (x, 0, z) instead of
// saying so.
func (h *Host) moveMonster(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "Move", "the monster (self)")
	if !ok {
		return 0
	}
	x, xerr := numArg(L, 2, "x")
	y, yerr := numArg(L, 3, "y")
	z, zerr := numArg(L, 4, "z")
	if xerr != nil || yerr != nil || zerr != nil {
		for _, err := range []error{xerr, yerr, zerr} {
			if err != nil {
				h.logf("script: Move: %v - the reference reads three coordinates "+
					"(GetFloat(2..4), FieldCharFunc.cpp:44-46) and nothing was sent", err)
				break
			}
		}
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("Move")
		return 0
	}
	h.Order.MoveMonster(mon, x, y, z)
	return 0
}

// moveToOwner is `MoveToOwner(self, nearRange)` - mon_ts_battle.scp:190, where a body
// that has wandered more than 1000 units from its owner is sent back to within 100.
//
// The range is REQUIRED by every call site (100 or 300 in the corpus) and the
// reference reads it unconditionally (`context->GetFloat(2)`, MonsterNPCFunc.cpp:901),
// so a one-argument call is refused rather than read as 0 - and 0 would be the
// reference's OTHER branch, the owner's exact point.
func (h *Host) moveToOwner(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "MoveToOwner", "the monster (self)")
	if !ok {
		return 0
	}
	nearRange, err := numArg(L, 2, "near range")
	if err != nil {
		h.logf("script: MoveToOwner: %v - the reference reads the range "+
			"unconditionally (MonsterNPCFunc.cpp:901) and nothing was sent", err)
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("MoveToOwner")
		return 0
	}
	h.Order.MoveMonsterToOwner(mon, nearRange)
	return 0
}

// randomMove is `RandomMove(self, minRange, maxRange)` - mon_ts_none.scp:276, :302,
// :311 and the other call sites named in the file header: the NONE entry's 'RANDOM'
// wander and the two FOLLOWOWNER fallbacks.
//
// THE CENTRE IS THE BODY'S CURRENT POINT, read from the `self` table the zone refreshes
// on every think turn (script\monster_ai.go, `refreshMonsterSelf`) - the script's own
// view of the body, which is what the reference's `m_pos` is at the moment of the call
// (`OnRandomMove`, FieldChar_Message.cpp:565-577). A point at a distance in
// [minRange, maxRange] is drawn, and the walk goes through `MoveMonster`: the same mesh
// check, the same row's MSPD and the same 3550 fan-out as every other script walk.
//
// THE MINIMUM IS HONOURED HERE although the reference reads it and drops it
// (`float minRange = arg1;` and then only `maxRange` reaches the world, :567-572). The
// scripts pass one at every call site, so a walk that ignored it would be a different
// walk from the one the script asked for.
func (h *Host) randomMove(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "RandomMove", "the monster (self)")
	if !ok {
		return 0
	}
	minRange, minErr := numArg(L, 2, "minimum range")
	maxRange, maxErr := numArg(L, 3, "maximum range")
	if minErr != nil || maxErr != nil {
		for _, err := range []error{minErr, maxErr} {
			if err != nil {
				h.logf("script: RandomMove: %v - the reference reads BOTH ranges "+
					"(GetFloat(2), GetFloat(3); FieldCharFunc.cpp:290-291) and nothing "+
					"was sent", err)
				break
			}
		}
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("RandomMove")
		return 0
	}
	cx, cy, ok := h.selfPoint(L, 1, "RandomMove")
	if !ok {
		return 0
	}
	height, ok := h.birthHeight(mon, "RandomMove")
	if !ok {
		return 0
	}
	x, y, ok := h.wanderPoint(cx, cy, minRange, maxRange, "RandomMove")
	if !ok {
		return 0
	}
	h.Order.MoveMonster(mon, x, y, height)
	return 0
}

// randomMoveCreatedLocaly is `RandomMoveCreatedLocaly(self, maxRange)` -
// mon_ts_none.scp:280, the 'RANDOMLOCAL' wander a body whose tactics row says
// `Move="RANDOMLOCAL"` makes, and the three `mon_tactics_artifact.scp` calls.
//
// THE CENTRE IS THE BODY'S BIRTH POINT, and it is the world's own: `MonsterWorld.Home`
// is the same `m_createdPos` the NONE entry's home-sick check reads through
// `GetMyHomeDistance` (mon_ts_none.scp:107-112) - one point, not a second copy. The
// reference's handler draws within `arg1` of `m_createdPos`
// (`GetRandomPosLocaly(shape, randomPos, m_createdPos, float(arg1))`,
// MonsterNPC.cpp:120-133); a body dragged 4,000 units from its spawn therefore walks
// all the way back, in steps of at most `maxRange`, instead of standing where it stopped.
func (h *Host) randomMoveCreatedLocaly(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "RandomMoveCreatedLocaly", "the monster (self)")
	if !ok {
		return 0
	}
	maxRange, err := numArg(L, 2, "range")
	if err != nil {
		h.logf("script: RandomMoveCreatedLocaly: %v - the reference reads the range "+
			"unconditionally (context->GetFloat(2), MonsterNPCFunc.cpp:121) and nothing "+
			"was sent", err)
		return 0
	}
	if h.Monster == nil {
		h.reportNoMonsterWorld("RandomMoveCreatedLocaly")
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("RandomMoveCreatedLocaly")
		return 0
	}
	home, known := h.Monster.Home(mon)
	if !known {
		h.reportOnce("random:no-home:"+hexMonster(mon),
			"script: RandomMoveCreatedLocaly answered nothing - body 0x%08x has no "+
				"birth point on this field (the reference's m_createdPos is set once in "+
				"OnEnter, MonsterNPC.cpp:1409-1414), so there is no centre to wander "+
				"around and nothing was sent", mon)
		return 0
	}
	x, y, ok := h.wanderPoint(home.X, home.Y, 0, maxRange, "RandomMoveCreatedLocaly")
	if !ok {
		return 0
	}
	// The height hint is the birth point's own Z: the only height this world keeps for a
	// body. The reference samples the mesh directly (`generateRandomPositionLocally`)
	// and needs no hint; our walk path resolves (x, y, z) through the mesh with the
	// original's 1000/300 search (zone\create.go:131-148).
	h.Order.MoveMonster(mon, x, y, float64(home.Z))
	return 0
}

// selfPoint reads the body's own point out of the `self` table - the X/Y the zone
// refreshes on every think turn. A table without them is REFUSED rather than read as
// (0, 0): a wander to the map's origin is the kind of silent zero this package exists
// to avoid.
func (h *Host) selfPoint(L *lua.LState, i int, call string) (int32, int32, bool) {
	t, ok := L.Get(i).(*lua.LTable)
	if !ok {
		h.logf("script: %s needs the body (self) as argument %d, got %s - nothing was sent",
			call, i, luaTypeName(L.Get(i)))
		return 0, 0, false
	}
	x, xok := tableInt(t, "X")
	y, yok := tableInt(t, "Y")
	if !xok || !yok {
		h.reportOnce("random:no-point:"+call,
			"script: %s answered nothing - the `self` table carries no X/Y, so the "+
				"body's current point (the reference's m_pos, FieldChar_Message.cpp:572) "+
				"is unknown and nothing was sent (further calls are not logged)", call)
		return 0, 0, false
	}
	return x, y, true
}

// birthHeight answers the birth point's Z for a walk's height hint, or refuses with the
// one line that says why. A body with no home is a handle this field never spawned (the
// reference's NULL object), and the zone's walk path would refuse it anyway.
func (h *Host) birthHeight(mon uint32, call string) (float64, bool) {
	if h.Monster == nil {
		h.reportNoMonsterWorld(call)
		return 0, false
	}
	home, known := h.Monster.Home(mon)
	if !known {
		h.reportOnce("random:no-home:"+hexMonster(mon),
			"script: %s answered nothing - body 0x%08x has no birth point on this field "+
				"(the reference's m_createdPos is set once in OnEnter, "+
				"MonsterNPC.cpp:1409-1414) and nothing was sent", call, mon)
		return 0, false
	}
	return float64(home.Z), true
}

// wanderPoint draws ONE point at a distance in [minRange, maxRange] of (cx, cy): the
// distance uniform in the band, the direction uniform around the circle - the same
// "somewhere in this range" the reference's `GetRandomPosLocaly` answers.
//
// A RANGE OF ZERO IS A REFUSAL, not a point on the body: the reference's
// `CGround::GetRandomPosLocaly` returns false for range 0 (`if (0 == range) return
// false;`, Ground.cpp:109-110). A negative maximum is refused for the same reason, and a
// minimum outside the band is clamped into it - the scripts' own numbers are always
// inside (moveMin = MSPD*1.5, moveMax = MSPD*3.0, mon_ts_none.scp:9-10).
func (h *Host) wanderPoint(cx, cy int32, minRange, maxRange float64, call string) (float64, float64, bool) {
	if maxRange <= 0 {
		h.reportOnce("random:range:"+call,
			"script: %s answered nothing - the maximum range is %g, and the reference's "+
				"GetRandomPosLocaly refuses range 0 (Ground.cpp:109-110); nothing was "+
				"sent (further calls are not logged)", call, maxRange)
		return 0, 0, false
	}
	if minRange < 0 {
		minRange = 0
	}
	if minRange > maxRange {
		minRange = maxRange
	}
	d := minRange + h.random().Float64()*(maxRange-minRange)
	angle := h.random().Float64() * 2 * math.Pi
	return float64(cx) + d*math.Cos(angle), float64(cy) + d*math.Sin(angle), true
}

// hexMonster writes a handle the way the zone's log lines do.
func hexMonster(handle uint32) string {
	return fmt.Sprintf("%08x", handle)
}

// startAttack is `StartAttack(self, target)` - mon_ts_battle.scp:163 and :172, the
// call behind `if('NO' == IsAttacking(self))`.
//
// It does NOT swing, and nothing here sends a packet: the reference's binding hands the
// target to the body's own state machine (`OM_START_ATTACK`, FieldCharFunc.cpp:172)
// and `CMonsterNPC::OnStartAttack` stores it (`RequestAttack(handle)`,
// MonsterNPC.cpp:175-179). The swing is `monsterSwingTick` -> `monsterSwing`.
func (h *Host) startAttack(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "StartAttack", "the monster (self)")
	if !ok {
		return 0
	}
	target, tok := h.hateHandle(L, 2, "StartAttack", "the target")
	if !tok {
		h.logf("script: StartAttack needs a target as argument 2 - the reference " +
			"dereferences it unconditionally (FieldCharFunc.cpp:167) and has no " +
			"one-argument form, so nothing was started")
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("StartAttack")
		return 0
	}
	h.Order.StartMonsterAttack(mon, target)
	return 0
}

// startGroundAttack is `StartGroundAttack(self)` - mon_ts_battle.scp:68, the
// PLACEMENT branch, called with ONE argument and no target.
//
// The reference sends `OM_START_GROUND_ATTACK` (FieldCharFunc.cpp:187) and
// `OnStartGroundAttack` raises a ground-attack request (MonsterNPC.cpp:181-184). This
// engine has no ground-attack path, so the request is STORED and nothing is sent - the
// honest half, rather than a 3564 aimed at the ground with a layout nobody measured.
func (h *Host) startGroundAttack(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "StartGroundAttack", "the monster (self)")
	if !ok {
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("StartGroundAttack")
		return 0
	}
	h.Order.StartMonsterGroundAttack(mon)
	return 0
}

// stopAttack is `StopAttack(self)` - mon_ts_battle.scp:148 and the seven lines after
// it, plus mon_ai_lib.scp:733. The reference clears the action state when it is one of
// the attack states (MonsterNPC.cpp:186-199); it never starts anything.
func (h *Host) stopAttack(L *lua.LState) int {
	mon, ok := h.hateHandle(L, 1, "StopAttack", "the monster (self)")
	if !ok {
		return 0
	}
	if h.Order == nil {
		h.reportNoOrderWorld("StopAttack")
		return 0
	}
	h.Order.StopMonsterAttack(mon)
	return 0
}
