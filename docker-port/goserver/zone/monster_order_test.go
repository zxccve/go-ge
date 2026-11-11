package zone

// THE SCRIPT'S MOVEMENT AND ATTACK CALLS - the tests for zone\monster_order.go and
// script\monster_order.go.
//
// One test per rule the binding file's header quotes, and the card's pass condition
// last: a real gopher-lua runtime calling EVERY bound name with the arity
// `ge\script\monster\mon_ts_battle.scp` gives it, with the 3550s on the wire decoded
// (`movesOf` / `decodeMonsterMove`) and the swing proved through the engine's own tick
// (`swingsOf`, and the swing suite's own assertions on the packet).
//
// THE FIXTURE IS THE REAL PATH WHERE IT MATTERS: `chaseZone` puts class 11591
// (Monster_Bigfoot, MSPD 143) in a real Monster table, joins two real sessions and
// clears their sockets; the packets go through the ordinary per-viewer fan-out
// (`h.fanOut`), which is the same one the chase and the swing use. The only stub is
// `h.NavNear` - the map's mesh - and it answers the point it was asked about with
// height 26, so a goal the test names is the goal the packet carries.

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"geserver/script"
)

// orderZone is the chase fixture plus a mesh: one Bigfoot at (0, 0, cell 29), the
// victim and a watcher, and `NavNear` answering any point on cell 29.
func orderZone(t *testing.T, victimAt, watcherAt [3]int32) (*Handler, *bytes.Buffer, *player, *player, Monster) {
	t.Helper()
	h, logs, victim, watcher, mon := chaseZone(t, victimAt, watcherAt)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{pt[0], pt[1], 29}, 26, true
	}
	return h, logs, victim, watcher, mon
}

// orderHost wires one Lua runtime to the zone's order world. The host's Log is the
// zone's, so a REFUSAL a binding writes (a missing target, a bad argument) lands in the
// same buffer the zone's own lines do and a test can assert on it.
func orderHost(t *testing.T, h *Handler) *script.Host {
	t.Helper()
	host := script.New()
	t.Cleanup(host.Close)
	host.Order = h
	host.Log = h.Log.Printf
	return host
}

// TestMoveSendsOne3550WithTheScriptsGoalAndMSPD is pass condition 1 for `Move`: FOUR
// arguments (self, x, y, z), ONE 26-byte 3550 through the ordinary fan-out, carrying
// the script's own goal and the body's SCR_Get_MON_MSPD speed - and the SAME point a
// second time sends nothing, because the reference's DispatchMove refuses a point equal
// to the one the body is already walking to (`IsEqualPos(moveTo, m_moveTo)`,
// FieldChar.cpp:945-951).
func TestMoveSendsOne3550WithTheScriptsGoalAndMSPD(t *testing.T) {
	h, logs, victim, watcher, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)

	src := `
		local mon = { Handle = ` + luaNum(mon.Handle) + ` }
		Move(mon, 500, 800, 26)
		Move(mon, 500, 800, 26)
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for one goal ordered twice, want exactly 1 - the FSM refuses "+
			"the same point (FieldChar.cpp:945-951):\n%s", len(moves), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != mon.Handle {
		t.Fatalf("the move names handle 0x%08x, want the monster's 0x%08x", handle, mon.Handle)
	}
	if goal != [3]int32{500, 800, 29} {
		t.Fatalf("the move walks to %v, want the script's own goal (500, 800, 29)", goal)
	}
	if speed != 143 {
		t.Fatalf("the move carries speed %g, want 143 - SCR_Get_MON_MSPD of class %d "+
			"(MSPD 143, no fix and no BM), which is what GetMoveSPD answers for a monster "+
			"(MonsterNPC.cpp:1929-1932)", speed, mon.ClassID)
	}
	// THE ORDINARY PER-VIEWER FAN-OUT: a second session in the ring receives the SAME
	// bytes, not a copy built for it.
	theirs := movesOf(watcher.conn)
	if len(theirs) != 1 || !bytes.Equal(moves[0], theirs[0]) {
		t.Fatalf("the second viewer got %d x 3550 (equal bytes: %v), want the same one "+
			"packet the victim got", len(theirs), len(theirs) == 1 && bytes.Equal(moves[0], theirs[0]))
	}
	if !strings.Contains(logs.String(), "MONSTER MOVE") {
		t.Fatalf("the walk was sent without a line:\n%s", logs.String())
	}
	// The order is STORED and the BINDING did not move the body: `Move` stores a leg and
	// `monsterMoveTick` is the only thing that walks it (no tick has run here).
	order, ok := h.MonsterOrderOf(mon.Handle)
	if !ok || !order.move.has {
		t.Fatalf("the move order was not stored (order %+v, ok %v)", order, ok)
	}
	if order.move.goal != [3]int32{500, 800, 29} || order.move.speed != 143 {
		t.Fatalf("the stored order is goal %v speed %g, want (500, 800, 29) at 143",
			order.move.goal, order.move.speed)
	}
	if body := h.runtimeMonsters()[0]; body.X != 0 || body.Y != 0 {
		t.Fatalf("the body stands at (%d, %d) after a move ORDER - a binding must not "+
			"move it", body.X, body.Y)
	}
}

// TestMoveRefusesAPointOffTheMesh pins the reference's first guard: the point is a
// REQUEST the world resolves, and a point it cannot resolve sends NOTHING
// (`world->Get3DPointToPosLocaly(...)`, false -> EXIT_FUNCTION, FieldCharFunc.cpp:56-58).
func TestMoveRefusesAPointOffTheMesh(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{}, 0, false
	}
	host := orderHost(t, h)

	src := `Move({ Handle = ` + luaNum(mon.Handle) + ` }, 500, 800, 26)`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v", err)
	}
	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 for a point with no ground under it, want 0:\n%s",
			n, logs.String())
	}
	if !strings.Contains(logs.String(), "MONSTER MOVE") ||
		!strings.Contains(logs.String(), "no ground") {
		t.Fatalf("an unresolvable point was refused without a line naming the ground:\n%s",
			logs.String())
	}
	if order, _ := h.MonsterOrderOf(mon.Handle); order.move.has {
		t.Fatalf("a refused point was stored as an order: %+v", order.move)
	}
}

// TestMoveToOwnerRefusesWithoutAnOwner pins the honest half of MoveToOwner: this server
// builds no ownership, so `GetOwner` answers nil for every body (script\status.go:223-228)
// and the reference only moves a body inside `if (owner)`. Nothing is sent, and the
// home point is NOT used in the owner's place.
func TestMoveToOwnerRefusesWithoutAnOwner(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)

	src := `MoveToOwner({ Handle = ` + luaNum(mon.Handle) + ` }, 100)`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v", err)
	}
	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 for a body with no owner, want 0:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "has no owner") {
		t.Fatalf("a refused MoveToOwner wrote no line:\n%s", logs.String())
	}
}

// TestMoveToOwnerWalksWithinRangeOfTheOwner is the other half: with the owner state
// recorded (the hook the minion card will call), `MoveToOwner(self, 100)` sends ONE 3550
// naming a walkable point 100 from the owner - the reference's `GetBossPos` with a
// non-zero range (MonsterNPCFunc.cpp:679-687) - at the body's own MSPD.
func TestMoveToOwnerWalksWithinRangeOfTheOwner(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	owner := victim.sess.Team[0].Handle
	if !h.SetMonsterOwner(mon.Handle, owner) {
		t.Fatalf("SetMonsterOwner refused the fixture's own body")
	}
	if got, ok := h.MonsterOwnerOf(mon.Handle); !ok || got != owner {
		t.Fatalf("the stored owner is 0x%08x (ok %v), want 0x%08x", got, ok, owner)
	}
	host := orderHost(t, h)

	src := `MoveToOwner({ Handle = ` + luaNum(mon.Handle) + ` }, 100)`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for MoveToOwner(100) with a live owner, want 1:\n%s",
			len(moves), logs.String())
	}
	_, goal, speed := decodeMonsterMove(t, moves[0])
	// The owner stands at (900, 0) and the body at (0, 0): 100 units towards the body
	// is (800, 0).
	if goal != [3]int32{800, 0, 29} {
		t.Fatalf("the walk goes to %v, want (800, 0, 29) - a point 100 from owner "+
			"(900, 0) on the line to the body (0, 0)", goal)
	}
	if d := math.Hypot(float64(goal[0])-900, float64(goal[1])-0); d > 100 {
		t.Fatalf("the goal is %g units from the owner, want at most the 100 the script "+
			"asked for (GetBossPos with a non-zero range)", d)
	}
	if speed != 143 {
		t.Fatalf("the walk carries speed %g, want the body's MON_MSPD 143", speed)
	}
}

// TestStartAttackStartsAndStopAttackStopsTheSwing is pass condition 2: the request is
// STORED by the binding (nothing is sent by the binding itself) and the SWING that
// follows is the engine's own - `monsterSwingTick` -> `monsterSwing`, asserted with the
// same fields the swing suite asserts. `StopAttack` then parks the body: the same tick
// that swung a moment ago sends nothing until a later `StartAttack`.
func TestStartAttackStartsAndStopAttackStopsTheSwing(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{0, 150, 29}, // edge gap 50: inside the Bigfoot's own MinR..MaxR 2..306
		[3]int32{0, 3000, 29})
	host := orderHost(t, h)
	c := &victim.sess.Team[0]
	victimHandle := c.Handle
	sheet := h.sheetFor(*c)
	wantDamage := monsterSwingDamage(mon.ATK, sheet.DEF)
	if wantDamage < 1 {
		t.Fatalf("the fixture's damage is %g - this test cannot tell a hit from the floor", wantDamage)
	}

	// 1. THE BINDING SENDS NOTHING. `OnStartAttack` stores the target and lets the FSM
	//    swing later (MonsterNPC.cpp:175-179).
	src := `StartAttack({ Handle = ` + luaNum(mon.Handle) + ` }, { Handle = ` + luaNum(victimHandle) + ` })`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("StartAttack sent %d packet(s) by itself, want 0 - the swing is the "+
			"engine's, not a binding's:\n%s", n, logs.String())
	}
	if !h.IsAttacking(mon.Handle) {
		t.Fatalf("StartAttack left IsAttacking answering 'NO' - the script gates " +
			"StartAttack on it (mon_ts_battle.scp:162) and would call it forever")
	}
	order, _ := h.MonsterOrderOf(mon.Handle)
	if order.attack.target != victimHandle || order.attack.stopped {
		t.Fatalf("the stored request is %+v, want target 0x%08x and not stopped",
			order.attack, victimHandle)
	}

	// 2. THE ENGINE'S OWN TICK SWINGS: one 3564, the swing suite's own fields.
	t0 := time.Unix(1_700_000_000, 0)
	h.monsterSwingTick(t0)
	swings := swingsOf(victim.conn)
	if len(swings) != 1 {
		t.Fatalf("%d x 3564 after StartAttack, want 1 - the swing is the existing "+
			"engine's:\n%s", len(swings), logs.String())
	}
	f := decodeSwing(t, swings[0])
	if f.size != AttackSingleSize || f.cmd != CmdZCAttackSingle {
		t.Fatalf("the swing is cmd %d (%d B), want %d (%d B)", f.cmd, f.size,
			CmdZCAttackSingle, AttackSingleSize)
	}
	if f.attacker != mon.Handle || f.atkPos != [3]int32{mon.X, mon.Y, mon.Cell} {
		t.Errorf("the swing names attacker %d at %v, want %d at (%d, %d, %d)",
			f.attacker, f.atkPos, mon.Handle, mon.X, mon.Y, mon.Cell)
	}
	if f.victim != victimHandle {
		t.Errorf("the swing names victim 0x%08x, want 0x%08x", f.victim, victimHandle)
	}
	if math.Abs(f.damage-wantDamage) > 1e-9 {
		t.Errorf("+42 damage = %g, want %g (the swing suite's own rule)", f.damage, wantDamage)
	}
	if f.hp != c.HP {
		t.Errorf("+50 HP = %g, want the stored post-hit HP %g", f.hp, c.HP)
	}
	if c.HP >= swingVictimHP {
		t.Errorf("the victim's ledger did not fall")
	}

	// 3. StopAttack PARKS THE BODY: the same tick at the next cadence sends nothing.
	src = `StopAttack({ Handle = ` + luaNum(mon.Handle) + ` })`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	if h.IsAttacking(mon.Handle) {
		t.Fatalf("StopAttack left IsAttacking answering 'YES' - OnStopAttack clears the " +
			"action state (MonsterNPC.cpp:186-199)")
	}
	h.monsterSwingTick(t0.Add(5 * time.Second))
	if n := len(swingsOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3564 after StopAttack, want still 1 - a stopped body must not "+
			"swing:\n%s", n, logs.String())
	}

	// 4. StartAttack AGAIN RESUMES IT, through the same tick and the same path.
	src = `StartAttack({ Handle = ` + luaNum(mon.Handle) + ` }, { Handle = ` + luaNum(victimHandle) + ` })`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	h.monsterSwingTick(t0.Add(10 * time.Second))
	swings = swingsOf(victim.conn)
	if len(swings) != 2 {
		t.Fatalf("%d x 3564 after the second StartAttack, want 2 - the stop must not be "+
			"permanent:\n%s", len(swings), logs.String())
	}
	if last := decodeSwing(t, swings[1]); last.hp != c.HP || last.victim != victimHandle {
		t.Fatalf("the resumed swing is victim 0x%08x at HP %g, want 0x%08x at %g",
			last.victim, last.hp, victimHandle, c.HP)
	}
}

// TestStartAttackRefusesWhatTheReferenceRefuses pins the two guards and the missing
// one-argument form: a self-target (`if (targetID == m_handle) return;`,
// FieldChar.cpp:884-888), a handle the world does not hold (`DispatchAttack` drops it,
// :955-957), and a call with no target at all (the binding dereferences argument 2,
// FieldCharFunc.cpp:167, and the corpus has no one-argument call).
func TestStartAttackRefusesWhatTheReferenceRefuses(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)

	// A SELF target, a GHOST target, and the one-argument form - none of them stores
	// anything and none sends a packet.
	src := `
		StartAttack({ Handle = ` + luaNum(mon.Handle) + ` }, { Handle = ` + luaNum(mon.Handle) + ` })
		StartAttack({ Handle = ` + luaNum(mon.Handle) + ` }, { Handle = 12345 })
		StartAttack({ Handle = ` + luaNum(mon.Handle) + ` })
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	if h.IsAttacking(mon.Handle) {
		t.Fatalf("a refused StartAttack still marked the body attacking")
	}
	if order, _ := h.MonsterOrderOf(mon.Handle); order.attack.has {
		t.Fatalf("a refused StartAttack stored a request: %+v", order.attack)
	}
	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("the refused attacks sent %d packet(s), want 0", n)
	}
	for _, want := range []string{"SELF target", "not a live body", "needs a target"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the refusal %q was not written:\n%s", want, logs.String())
		}
	}
}

// TestStartGroundAttackStoresTheRequestAndSendsNothing pins the honest limit: the
// reference raises a ground-attack request (FieldChar.cpp:896-899) and this engine has
// no ground-attack path, so the request is STORED and no packet is invented.
func TestStartGroundAttackStoresTheRequestAndSendsNothing(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)

	src := `StartGroundAttack({ Handle = ` + luaNum(mon.Handle) + ` })`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	order, ok := h.MonsterOrderOf(mon.Handle)
	if !ok || !order.attack.ground || order.attack.stopped {
		t.Fatalf("the ground request is %+v (ok %v), want ground true and not stopped",
			order.attack, ok)
	}
	if !h.IsAttacking(mon.Handle) {
		t.Fatalf("a ground attack left IsAttacking answering 'NO'")
	}
	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("StartGroundAttack sent %d packet(s), want 0 - this engine has no "+
			"ground-attack path:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "GROUND attack"); n != 1 {
		t.Fatalf("the ground request was logged %d time(s), want once:\n%s", n, logs.String())
	}
}

// TestMonsterOrderBuiltinsRunFromLuaWithTheScriptsArity is the card's pass condition: a
// real gopher-lua runtime, a Handler behind it, and a snippet that calls EVERY bound
// name with the arity `mon_ts_battle.scp` gives it - `Move` with four arguments,
// `MoveToOwner` and `StartAttack` with two, `StartGroundAttack` and `StopAttack` with
// one - and then the assertions on the BYTES: two 3550s (the script's point and the
// owner's ring), each carrying the body's MON_MSPD, and no 3564 at all.
func TestMonsterOrderBuiltinsRunFromLuaWithTheScriptsArity(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t,
		[3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	owner := victim.sess.Team[0].Handle
	if !h.SetMonsterOwner(mon.Handle, owner) {
		t.Fatalf("SetMonsterOwner refused the fixture's own body")
	}
	host := orderHost(t, h)

	src := `
		local mon    = { Handle = ` + luaNum(mon.Handle) + ` }
		local target = { Handle = ` + luaNum(owner) + ` }

		-- :253 Move(self, x, y, z) - four arguments, and the same point twice.
		Move(mon, 500, 800, 26)
		Move(mon, 500, 800, 26)

		-- :190 MoveToOwner(self, 100) - two arguments.
		MoveToOwner(mon, 100)

		-- :163 StartAttack(self, target) - two arguments, no packet.
		StartAttack(mon, target)

		-- :68 StartGroundAttack(self) - one argument, no packet.
		StartGroundAttack(mon)

		-- :148 StopAttack(self) - one argument, no packet.
		StopAttack(mon)
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}

	// EVERY NAME IS BOUND: no fall-through to the missing-builtin stub.
	bound := map[string]bool{"Move": true, "MoveToOwner": true, "StartAttack": true,
		"StartGroundAttack": true, "StopAttack": true}
	for _, miss := range host.Missing() {
		if bound[miss.Name] {
			t.Fatalf("%s fell through to the missing-builtin stub %d time(s) - it is not "+
				"bound on the host", miss.Name, miss.Calls)
		}
	}

	// THE BYTES: two 3550s, in the script's own order.
	moves := movesOf(victim.conn)
	if len(moves) != 2 {
		t.Fatalf("%d x 3550 from the snippet, want 2 (the script's point and the "+
			"owner's ring; the repeated point sends nothing):\n%s", len(moves), logs.String())
	}
	_, firstGoal, firstSpeed := decodeMonsterMove(t, moves[0])
	if firstGoal != [3]int32{500, 800, 29} || firstSpeed != 143 {
		t.Fatalf("the first 3550 is goal %v at %g, want (500, 800, 29) at 143 "+
			"(SCR_Get_MON_MSPD of class %d)", firstGoal, firstSpeed, mon.ClassID)
	}
	_, secondGoal, secondSpeed := decodeMonsterMove(t, moves[1])
	// The owner stands at (0, 150) and the body at (0, 0): 100 units towards the body
	// is (0, 50).
	if secondGoal != [3]int32{0, 50, 29} || secondSpeed != 143 {
		t.Fatalf("the MoveToOwner 3550 is goal %v at %g, want (0, 50, 29) at 143",
			secondGoal, secondSpeed)
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("the snippet produced %d x 3564, want 0 - none of these five calls "+
			"swings:\n%s", n, logs.String())
	}

	// THE STATE the script left behind: the walk order, a stopped attack, and the owner.
	order, ok := h.MonsterOrderOf(mon.Handle)
	if !ok || !order.move.has || order.move.goal != [3]int32{0, 50, 29} {
		t.Fatalf("the stored move order is %+v (ok %v), want the LAST goal (0, 50, 29)",
			order.move, ok)
	}
	if !order.attack.stopped {
		t.Fatalf("StopAttack did not park the body: %+v", order.attack)
	}
	if h.IsAttacking(mon.Handle) {
		t.Fatalf("the snippet ended with IsAttacking answering 'YES' after StopAttack")
	}
	if body := h.runtimeMonsters()[0]; body.X != 0 || body.Y != 0 {
		t.Fatalf("a binding moved the body to (%d, %d) - orders are stored, not walked",
			body.X, body.Y)
	}
}

// TestMonsterOrderDiesWithTheBody pins the lifetime: the order hangs off the same
// per-handle state the tactics and home do, so the body's leave takes it with them, and
// a handle that was never a body has no order to read.
func TestMonsterOrderDiesWithTheBody(t *testing.T) {
	h, _, victim, _, mon := orderZone(t,
		[3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)

	src := `
		Move({ Handle = ` + luaNum(mon.Handle) + ` }, 500, 800, 26)
		StartAttack({ Handle = ` + luaNum(mon.Handle) + ` }, { Handle = ` + luaNum(victim.sess.Team[0].Handle) + ` })
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v", err)
	}
	if _, ok := h.MonsterOrderOf(mon.Handle); !ok {
		t.Fatalf("the order was not stored before the leave")
	}
	if !h.monsterMaySwing(mon.Handle) {
		t.Fatalf("a body with a stored request must still swing - the swing is the " +
			"tick's, and only a StopAttack parks it")
	}

	// THE LEAVE, exactly as takeDueLeaves does it.
	h.spawnMu.Lock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == mon.Handle {
			h.spawnedMonster = append(h.spawnedMonster[:i], h.spawnedMonster[i+1:]...)
			break
		}
	}
	delete(h.monsterHP, mon.Handle)
	h.spawnMu.Unlock()

	if _, ok := h.MonsterOrderOf(mon.Handle); ok {
		t.Fatalf("a body that left the field still answers an order")
	}
	if !h.monsterMaySwing(MonsterHandle(4242)) {
		t.Fatalf("a handle that was never a body answered 'may not swing' - the default " +
			"must be TRUE, or the pre-script engine would stop swinging")
	}
}

// ---- the approach: an attack order walks into reach (zone\monster_order.go) ---------
//
// THE CLOCK IS THE SCRIPT-MODE ONE. `-monai script` does not run `monsterChaseTick`
// (zone\chase.go:710); it runs `monsterSwingTick`, which asks `monsterMaySwing` per
// body, and that is where `monsterApproachStep` steps. So `approachDrive` below is the
// harness's `chaseStep` for this mode, with ONE difference: the approach's walk is drawn
// from the body's own clock (speed x elapsed, `monsterLeg.posAt`), not from a tick
// number, so the drive has to be REAL time - a synthetic tick time would move nothing
// and prove nothing.

// approachDrive runs the script-mode clock at the zone's poll interval for at most
// `limit`, stopping as soon as `until` answers true (nil = run the whole span).
func approachDrive(h *Handler, limit time.Duration, until func() bool) {
	deadline := time.Now().Add(limit)
	for {
		h.monsterSwingTick(time.Now())
		if until != nil && until() {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(pollInterval)
	}
}

// TestScriptedApproachWalksIntoReachThenSwings is pass condition (a): a body 600 units
// from the target, after `StartMonsterAttack`, sends ONE 3550 whose goal is the reach
// ring - and then a 3564 when it arrives, with the player never moving.
//
// THE NUMBERS ARE THE FIXTURE'S: Bigfoot at (0, 0), target at (600, 0), the row's
// MaxR 306 and both 50-unit bodies, so the ring is 356 from the target's centre
// (`attackStandoff`) and the stand point is (244, 0). The clear ground at the order is
// 600 - 100 = 500 and the route is 244 units, which is exactly what the ONE line must
// say.
func TestScriptedApproachWalksIntoReachThenSwings(t *testing.T) {
	victimAt := [3]int32{600, 0, 29}
	h, logs, victim, _, mon := orderZone(t, victimAt, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	target := victim.sess.Team[0].Handle

	// 1. THE ORDER ITSELF SENDS NOTHING.
	h.StartMonsterAttack(mon.Handle, target)
	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("StartMonsterAttack sent %d packet(s) by itself, want 0 - the approach "+
			"is the engine's tick:\n%s", n, logs.String())
	}
	if body := h.runtimeMonsters()[0]; body.X != 0 || body.Y != 0 {
		t.Fatalf("the body stands at (%d, %d) right after the order - a binding must not "+
			"move it", body.X, body.Y)
	}

	// 2. THE ENGINE WALKS IT INTO REACH AND THE SWING FOLLOWS.
	approachDrive(h, 20*time.Second, func() bool { return len(swingsOf(victim.conn)) >= 1 })

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for the approach, want exactly 1 - one order per stand "+
			"point (FieldChar.cpp:946-947):\n%s", len(moves), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != mon.Handle {
		t.Fatalf("the move names handle 0x%08x, want the monster's 0x%08x", handle, mon.Handle)
	}
	if goal != [3]int32{244, 0, 29} {
		t.Fatalf("the walk goes to %v, want the stand point (244, 0, 29): 356 from the "+
			"target's centre (MaxR 306 + the target's body 50) on the line to the body - "+
			"the same ring the go chase walks to", goal)
	}
	if speed != 143 {
		t.Fatalf("the approach walks at %g, want 143 - SCR_Get_MON_MSPD of the Bigfoot "+
			"row (MSPD 143), the value GetMoveSPD answers (MonsterNPC.cpp:1929-1932)",
			speed)
	}
	gap := flatDist(goal, victimAt) - (h.monsterBodyRadius(mon) + PCBodyRadius)
	if gap > mon.MaxR || gap < mon.MinR {
		t.Fatalf("the stand point leaves an edge gap of %g, outside the row's own "+
			"MinR..MaxR %g..%g - the swing would never land there", gap, mon.MinR, mon.MaxR)
	}

	// 3. THE SWING ARRIVED, and the victim never moved (a moving player would make the
	// reach a different question).
	swings := swingsOf(victim.conn)
	if len(swings) != 1 {
		t.Fatalf("%d x 3564 after the walk, want 1 - the approach must hand the body to "+
			"the swing:\n%s", len(swings), logs.String())
	}
	if f := decodeSwing(t, swings[0]); f.victim != target {
		t.Fatalf("the swing names victim 0x%08x, want the ordered target 0x%08x",
			f.victim, target)
	}
	body := h.runtimeMonsters()[0]
	bodyAt := [3]int32{body.X, body.Y, body.Cell}
	if flatDist(bodyAt, victimAt)-(h.monsterBodyRadius(mon)+PCBodyRadius) > mon.MaxR {
		t.Fatalf("the body stopped at (%d, %d), still outside its reach %g - the swing "+
			"above could not have landed", bodyAt[0], bodyAt[1], mon.MaxR)
	}
	if n := cmdCount(victim.conn.sent(), CmdZCMovePC); n != 0 {
		t.Fatalf("%d x 3548 on the victim's socket, want 0 - nothing moved the player", n)
	}

	// 4. THE ONE LINE, exactly as the card words it.
	if n := strings.Count(logs.String(), "MONSTER APPROACH"); n != 1 {
		t.Fatalf("the approach was logged %d time(s), want exactly 1:\n%s", n, logs.String())
	}
	want := fmt.Sprintf("zone: MONSTER APPROACH 0x%08x -> 0x%08x at 500 units of clear ground, "+
		"walking 244 to (244, 0)", mon.Handle, target)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the approach line is not the card's:\nwant: %s\ngot:\n%s", want, logs.String())
	}
}

// TestScriptedApproachStopsOnStopAttack is pass condition (b): a `StopAttack` in the
// middle of the walk ends it where the body got to - the same tick's step freezes the
// leg, and nothing walks or swings afterwards until something starts it again.
func TestScriptedApproachStopsOnStopAttack(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)

	approachDrive(h, 5*time.Second, func() bool { return len(movesOf(victim.conn)) >= 1 })
	if len(movesOf(victim.conn)) != 1 {
		t.Fatalf("the approach never ordered a walk:\n%s", logs.String())
	}
	time.Sleep(250 * time.Millisecond) // let it get well into the route, not to the ring
	h.monsterSwingTick(time.Now())
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 mid-approach, want 0 - the body is still walking", n)
	}

	// THE STOP, then the tick that must freeze the leg on the point it reached.
	h.StopMonsterAttack(mon.Handle)
	h.monsterSwingTick(time.Now())
	body := h.runtimeMonsters()[0]
	frozen := [3]int32{body.X, body.Y, body.Cell}
	order, _ := h.MonsterOrderOf(mon.Handle)
	if order.approach.chase.leg.active {
		t.Fatalf("the leg is still walking after StopAttack: %+v", order.approach.chase.leg)
	}

	approachDrive(h, 700*time.Millisecond, nil)
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 after StopAttack, want still 1 - a stopped body must not "+
			"order a walk:\n%s", n, logs.String())
	}
	body = h.runtimeMonsters()[0]
	if got := [3]int32{body.X, body.Y, body.Cell}; got != frozen {
		t.Fatalf("the body walked from %v to %v after StopAttack", frozen, got)
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 after StopAttack, want 0 - the swing is parked too", n)
	}
}

// TestScriptedApproachEndsWhenTheTargetDies is pass condition (c): the target's ledger
// reaching 0 ends the walk. The order is still stored (the script clears it itself), but
// nothing walks or swings at the emptied body.
func TestScriptedApproachEndsWhenTheTargetDies(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)

	approachDrive(h, 5*time.Second, func() bool { return len(movesOf(victim.conn)) >= 1 })
	time.Sleep(250 * time.Millisecond)
	h.monsterSwingTick(time.Now())

	victim.sess.Team[0].HP = 0 // the kill the swing/ledger would record
	h.monsterSwingTick(time.Now())
	body := h.runtimeMonsters()[0]
	frozen := [3]int32{body.X, body.Y, body.Cell}
	order, _ := h.MonsterOrderOf(mon.Handle)
	if !order.approach.ended {
		t.Fatalf("the approach did not end on a dead target: %+v", order.approach)
	}

	approachDrive(h, 700*time.Millisecond, nil)
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 after the target died, want still 1:\n%s", n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 at a dead target, want 0", n)
	}
	body = h.runtimeMonsters()[0]
	if got := [3]int32{body.X, body.Y, body.Cell}; got != frozen {
		t.Fatalf("the body walked from %v to %v after the target died", frozen, got)
	}
}

// TestScriptModeAcquiresNothingWithoutAnOrder is pass condition (d), with its own
// counterproof: in script mode a body beside a player does NOT acquire (no order, no
// walk, no line), and the SAME fixture in go mode walks - so the difference is the
// order, not the fixture.
func TestScriptModeAcquiresNothingWithoutAnOrder(t *testing.T) {
	h, logs, victim, _, _ := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true

	approachDrive(h, 1*time.Second, nil)
	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 with no attack order in script mode, want 0 - the target in "+
			"this mode comes from the script's own StartAttack and nowhere else:\n%s",
			n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 with no attack order, want 0", n)
	}
	if strings.Contains(logs.String(), "MONSTER APPROACH") {
		t.Fatalf("an approach was announced with no order:\n%s", logs.String())
	}

	// THE COUNTERPROOF: the go chase, on the same fixture, does acquire and walk.
	h.MonsterAIScript = false
	h.monsterChaseTick(time.Now())
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 from the go chase, want 1 - this fixture IS one the chase "+
			"walks (600 is inside its SeekRange 2500), so the zero above is the ORDER's "+
			"absence and not the fixture's:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "MONSTER CHASE") {
		t.Fatalf("the go chase walked without its own line:\n%s", logs.String())
	}
}

// TestScriptedApproachFollowsANewTarget pins the fourth end: a DIFFERENT target ordered
// while the body is walking rebuilds the walk from where the body stands (the old leg is
// not continued) and owes the ONE line again - once per order, never once per tick.
func TestScriptedApproachFollowsANewTarget(t *testing.T) {
	h, logs, victim, watcher, mon := orderZone(t,
		[3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
	approachDrive(h, 5*time.Second, func() bool { return len(movesOf(victim.conn)) >= 1 })

	other := watcher.sess.Team[0].Handle
	h.StartMonsterAttack(mon.Handle, other)
	approachDrive(h, 5*time.Second, func() bool { return len(movesOf(victim.conn)) >= 2 })

	moves := movesOf(victim.conn)
	if len(moves) != 2 {
		t.Fatalf("%d x 3550 for two targets, want 2 (one stand point each):\n%s",
			len(moves), logs.String())
	}
	_, goal, _ := decodeMonsterMove(t, moves[1])
	// The second walk is for the watcher at (0, 3000): the ring is 356 from its centre,
	// so the goal must be on that circle, towards the body (i.e. at y ~ 2645).
	if d := flatDist(goal, [3]int32{0, 3000, 29}); math.Abs(d-356) > 2 {
		t.Fatalf("the new walk's goal %v is %g from the new target, want the 356-unit "+
			"ring (MaxR 306 + both 50-unit bodies)", goal, d)
	}
	if n := strings.Count(logs.String(), "MONSTER APPROACH"); n != 2 {
		t.Fatalf("two orders produced %d approach line(s), want 2 - one per order, not "+
			"one per tick:\n%s", n, logs.String())
	}
}
