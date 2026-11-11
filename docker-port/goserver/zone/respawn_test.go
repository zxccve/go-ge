package zone

// RESPAWN, SLICE 1 - the pass conditions the job names, one test each, plus the
// CORPSE-TIMING rule, which is now the REFERENCE's own (2026-11-09, source2 ZServerLib\
// MonsterNPC.cpp:2588-2602 and 2612-2655):
//
//	leaveDelay = 1 s + the shape's `DyingTime`, floored at 3 s   (a shape that states > 0)
//	           = 2 s                                             (a killing attack)
//	           = 5 s                                             (a death that is no attack)
//	leaveAt    = the kill + leaveDelay
//	deadAt     = leaveAt - DeathPacketLead (500 ms)
//
// So the ZC_DEAD leaves 500 ms BEFORE the body is taken away, never ahead of the blow
// that killed it: the fall is drawn after the attack animation it lands on.
//
// THE SPEC IS `history/monster-death.md` §5 and the two packets the zone already owns:
// `ZC_DEAD` (3586, 6 bytes, sent by the corpse's own clock - zone/respawn.go
// deathPacketTick), `ZC_LEAVE_MONSTER` (3531, 8 bytes, reason 0, zone/monster.go), and
// the spawn frame `ZC_ENTER_STANDINGMONSTER` (3527, 360 bytes, zone/monster.go
// BuildEnterStandingMonster).
//
// NOTHING SLEEPS AND NOTHING IS INJECTED THROUGH A GLOBAL CLOCK. The kill arms real
// instants, and the corpse's tick takes `now` as an argument (corpseTick), so a test
// stands one millisecond short of a leave or a return, for free. The corpse records both
// instants (`deadAt` for the packet, `leaveAt` for the leave), and the kill they are
// measured from is `leaveAt - delay`.
//
// THE SHAPE IS FIXTURE DATA HERE: `spawnedZoneWithShape` puts a row in the monster table
// whose `Shape` the shape table would have answered - `ShapeKnown` true and the shape's
// own `DyingTime` - and `spawnedZone` keeps a row with NO shape, which is the fallback
// case. The JOIN from `datatable_monster_shape.xml` is proved separately against a real
// file (TestLoadMonstersReadsDyingTimeFromTheShapeTable, respawn_tables_test.go).
//
// TWO FIXTURES FOR THE REST, on purpose:
//
//   - newFakeWorld + SpawnMonsterObject (create_test.go) is the REAL creation path: a
//     script-created body with the row behind it, its HP written by the spawn, in the
//     roster that is the world. Pass conditions 1, 2 and 4 ride it, because they are about
//     the handles, the HP and the "no one-shot flag" - and none of them needs an attacker.
//   - swingZone (monster_swing_test.go) is the row that NAMES AN ATTACK, the real stance
//     sheet and the real sight row (ViewRange 6,000 / ViewMiddleRange 4,000). Pass
//     conditions 3 and 5 ride it, and the respawned body's swing is the SAME 3564 the
//     monster-swing slice already sends - this lane only proves the new handle can do it.
//   - dropZone (drop_test.go) is pass condition 6: one 3536 per body, so a body that comes
//     back rolls its own.

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"
)

// scarecrowSpawn is the spec the create tests already use: the probe's own numbers, so the
// body has a row, a level, a tactics and an explicit max HP.
func scarecrowSpawn() MonsterSpec {
	return MonsterSpec{
		MonsterID: 11600, X: 2623, Y: -665, Z: 8, Count: 1,
		Direction: 270, Level: 130, MHP: 499900, Tactics: "MON_MISSION_84",
	}
}

// fixtureSpawnerDelay is the fixture's own spawner row: the interval a body the MAP
// SPAWNER placed comes back on. It stands in for a `mongentype` row's `RespawnTime`,
// which is the only clock a return has now (zone/respawn.go respawnDelayFor) - a body
// with no row at all is NOT brought back, and the tests that pin that live in
// respawn_no_fallback_test.go.
const fixtureSpawnerDelay = 5 * time.Second

// spawnerBodySpec is the spec the map spawner builds for the scarecrow: the probe's own
// numbers plus its type row's `RespawnTime` and the flag that says that row EXISTS
// (StartSpawner builds exactly this shape, zone/spawner.go). The chain tests below stand
// on it because a body with no spawner row now dies for good.
func spawnerBodySpec() MonsterSpec {
	spec := scarecrowSpawn()
	spec.Respawn = fixtureSpawnerDelay
	spec.HasSpawner = true
	spec.SpawnerClassID = 1
	return spec
}

// spawnedZone is the real creation path with one player in the roster: a body made by
// SpawnMonsterObject, exactly as the map spawner's StartSpawner makes one (its type row's
// RespawnTime on the spec). Its row names NO shape, so its corpse leaves on the floor and
// the zone says so once.
func spawnedZone(t *testing.T) (*fakeWorld, *player, Monster) {
	t.Helper()
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	if _, err := w.SpawnMonsterObject(spawnerBodySpec()); err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}
	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) in the roster after one spawn, want 1", len(made))
	}
	return w, p, made[0]
}

// spawnedZoneWithShape is spawnedZone on a row whose shape the shape table ANSWERS: the
// same real creation path, with `dying` as the shape's own `DyingTime`. It is the fixture
// the three timing pass conditions stand on.
func spawnedZoneWithShape(t *testing.T, class uint32, dying time.Duration) (*fakeWorld, *player, Monster) {
	t.Helper()
	w := newFakeWorld(t)
	w.Monsters = newMonsters(map[uint32]MonsterRow{
		class: {
			ID: class, Name: "Monster_Shaped", Level: 10, Tactics: "MON_MISSION_84",
			ShapeKnown: true, DyingTime: dying,
		},
	})
	p := w.joinPlayer(1792)
	spec := spawnerBodySpec()
	spec.MonsterID = int32(class)
	if _, err := w.SpawnMonsterObject(spec); err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}
	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) in the roster after one spawn, want 1", len(made))
	}
	return w, p, made[0]
}

// logText is the zone's log as one string: fakeWorld keeps its lines in a slice, and every
// assertion here reads them the way an operator would.
func (w *fakeWorld) logText() string {
	return strings.Join(*w.lines, "\n") + "\n"
}

// corpsesOwed copies the corpse list, under the roster's own lock.
func corpsesOwed(t *testing.T, h *Handler) []corpse {
	t.Helper()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	return append([]corpse(nil), h.corpses...)
}

// corpseFor answers the corpse owed for one handle and fails when none is: every timing
// assertion reads the corpse's own leaveAt/backAt, never a wall clock.
func corpseFor(t *testing.T, h *Handler, handle uint32) corpse {
	t.Helper()
	for _, c := range corpsesOwed(t, h) {
		if c.handle == handle {
			return c
		}
	}
	t.Fatalf("no corpse is owed for 0x%08x", handle)
	return corpse{}
}

// killAndDead kills the fixture body with one hit, drives the corpse's ZC_DEAD at its own
// instant, and answers the instant of the KILL - the clock the leave and the return are
// both measured from.
//
// THE HIT ITSELF SENDS NOTHING (2026-11-09, zone/create.go damageMonsterBy): the packet
// belongs to the corpse and leaves DeathPacketLead before the leave (zone/respawn.go
// deathPacketTick), so the pass is driven here, at the corpse's own `deadAt`. A test that
// wants to see the killing press return in silence reads its socket BEFORE calling this.
func killAndDead(t *testing.T, h *Handler, m Monster, killer uint32) time.Time {
	t.Helper()
	before, after, ok := h.damageMonsterBy(m, m.MaxHP, killer, m.Level)
	if !ok || before != m.MaxHP || after != 0 {
		t.Fatalf("the fixture hit did not empty the body: %g -> %g (ok=%v), want %g -> 0",
			before, after, ok, m.MaxHP)
	}
	c := corpseFor(t, h, m.Handle)
	killed := c.leaveAt.Add(-c.delay)
	if got := c.deadAt.Sub(killed); got != c.delay-DeathPacketLead {
		t.Fatalf("the corpse's deadAt %s is %s after the kill, want its delay %s less the "+
			"%s packet lead - the reference's `m_deadTime = m_leaveTime - 500` "+
			"(source2 MonsterNPC.cpp:2653-2655)", c.deadAt, got, c.delay, DeathPacketLead)
	}
	h.deathPacketTick(c.deadAt)
	return killed
}

// leaveFrames returns every 3531 among a socket's packets.
func leaveFrames(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCLeaveMonster {
			out = append(out, p)
		}
	}
	return out
}

// enterFrames returns every 3527 among a socket's packets.
func enterFrames(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCEnterStandingMonster {
			out = append(out, p)
		}
	}
	return out
}

// swingFrames returns every 3564 among a socket's packets.
func swingFrames(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCAttackSingle {
			out = append(out, p)
		}
	}
	return out
}

// assertStillWaiting is pass condition 4's two halves, applied at any instant before the
// leave: NO 3531 has gone out, and the body is STILL IN THE ROSTER (the client cannot play
// an animation for an object this server has already dropped).
func assertStillWaiting(t *testing.T, w *fakeWorld, p *player, m Monster, when time.Time, why string) {
	t.Helper()
	w.corpseTick(when)
	if n := len(leaveFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d 3531(s) went out %s, want 0:\n%s", n, why, w.logText())
	}
	for _, x := range w.runtimeMonsters() {
		if x.Handle == m.Handle {
			return
		}
	}
	t.Fatalf("0x%08x left the roster %s - the corpse must survive until its leave:\n%s",
		m.Handle, why, w.logText())
}

// respawnNow drives one corpse to its new body: the leave at the corpse's OWN instant,
// then the return at its own, which is what makes the helper shape-agnostic.
func respawnNow(t *testing.T, h *Handler, handle uint32) Monster {
	t.Helper()
	c := corpseFor(t, h, handle)
	h.corpseTick(c.leaveAt)
	h.corpseTick(c.backAt)
	for _, m := range h.runtimeMonsters() {
		if m.Handle != handle {
			return m
		}
	}
	t.Fatalf("no body came back: the roster is still %v", h.runtimeMonsters())
	return Monster{}
}

// TestCorpseOfAZeroDyingTimeShapeTakesTheAttackDelay is pass condition 1 under the
// reference's rule: a shape that STATES `DyingTime` 0 (the Bigfoot's `Monster_Bigfoot`,
// the field's own shape) states no death animation for the `1 s + DyingTime` branch to
// wait for, so the corpse takes the branch its death is - a killing attack, 2 s. The
// leave is that interval after the KILL, the ZC_DEAD is DeathPacketLead before the leave,
// and a shape that states 0 is NOT reported as a missing shape.
//
// IT REPLACED the version that pinned a 600 ms floor (2026-11-07..09): every assertion
// about the packet (8 bytes, reason 0, the dead handle, once, after the 3586 on the wire)
// is kept, and the two the rule touches - the interval and the log's number - read the
// reference's attack branch instead of our own floor.
func TestCorpseOfAZeroDyingTimeShapeTakesTheAttackDelay(t *testing.T) {
	w, p, mon := spawnedZoneWithShape(t, 11591, 0) // Monster_Bigfoot: DyingTime="0"
	at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle) // the kill

	if n := len(leaveFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d 3531(s) went out with the ZC_DEAD, want 0 - the corpse owes its delay", n)
	}
	owed := corpsesOwed(t, w.Handler)
	if len(owed) != 1 || owed[0].handle != mon.Handle {
		t.Fatalf("the corpse list is %v, want one corpse for 0x%08x", owed, mon.Handle)
	}
	if got := owed[0].leaveAt.Sub(at); got != CorpseLeaveAttack {
		t.Fatalf("a DyingTime of 0 leaves %s after the kill, want the %s attack delay "+
			"(the shape states no death animation, so the reference's 1 s + DyingTime "+
			"branch does not apply)", got, CorpseLeaveAttack)
	}

	assertStillWaiting(t, w, p, mon, at.Add(CorpseLeaveAttack-time.Millisecond),
		"one millisecond short of the 2 s attack delay")

	// AT THE MARK: exactly one, 8 bytes, reason 0, the dead handle.
	w.corpseTick(at.Add(CorpseLeaveAttack))
	sent := leaveFrames(p.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the socket has %d 3531(s) at the deadline, want exactly 1:\n%s",
			len(sent), w.logText())
	}
	pkt := sent[0]
	if len(pkt) != LeaveMonsterSize {
		t.Fatalf("3531 is %d bytes, want %d (cmd 2 + handle 4 + code 2)",
			len(pkt), LeaveMonsterSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCLeaveMonster {
		t.Fatalf("the leave's command is %d, want %d", cmd, CmdZCLeaveMonster)
	}
	if got := binary.LittleEndian.Uint32(pkt[lmHandle:]); got != mon.Handle {
		t.Fatalf("the leave names handle 0x%08x, want the dead 0x%08x", got, mon.Handle)
	}
	if code := binary.LittleEndian.Uint16(pkt[lmCode:]); code != LeaveCodeLogout {
		t.Fatalf("the leave's reason is %d, want %d - the client only FADES for 3, and a "+
			"killed monster must be dropped at once", code, LeaveCodeLogout)
	}

	// ONCE. The corpse is off the list and a second tick sends nothing.
	w.corpseTick(at.Add(CorpseLeaveAttack))
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("a second tick sent the leave again (%d 3531s), want 1", n)
	}

	// THE DEAD HANDLE IS OFF THE ROSTER (this is what "the corpse left the world" means).
	for _, m := range w.runtimeMonsters() {
		if m.Handle == mon.Handle {
			t.Fatalf("0x%08x is still in the roster after its corpse left", mon.Handle)
		}
	}
	if _, ok := w.monsterLiveHP(mon.Handle); ok {
		t.Errorf("the dead handle still has a live-HP entry, want none")
	}

	// THE ORDER ON THE WIRE, and the line says WHICH number it is using.
	all := p.conn.sent()
	dead, leave := -1, -1
	for i, q := range all {
		switch binary.LittleEndian.Uint16(q) {
		case CmdZCDead:
			dead = i
		case CmdZCLeaveMonster:
			leave = i
		}
	}
	if dead < 0 || leave < 0 || dead > leave {
		t.Fatalf("the socket order is ZC_DEAD at %d and leave at %d - the leave must follow "+
			"the death:\n%s", dead, leave, w.logText())
	}
	text := w.logText()
	if !strings.Contains(text, "CORPSE LEAVE") || !strings.Contains(text, "2000ms after the ZC_DEAD") {
		t.Errorf("the leave line does not name the attack branch's 2000 ms and what it is "+
			"measured from:\n%s", text)
	}
	if !strings.Contains(text, "SPAWNER row") {
		t.Errorf("the line does not name the spawner row as the return's source - the row "+
			"is the only clock a return has now:\n%s", text)
	}
	if strings.Contains(text, "FALLBACK") {
		t.Errorf("a body with a spawner row was described as using a fallback:\n%s", text)
	}
	if strings.Contains(text, "has NO shape row") {
		t.Errorf("a shape that STATES DyingTime 0 was reported as a missing shape:\n%s", text)
	}
}

// TestCorpseLeavesAtTheShapesOwnDyingTime is pass condition 2: a shape that states 3500 ms
// takes the reference's `1 s + DyingTime` branch - 4500 ms after the kill, and the ZC_DEAD
// 500 ms before that - not our old floor, and not the raw 3500 either. Pass condition 4
// rides with it: nothing is sent and the roster entry survives every instant before the
// leave.
func TestCorpseLeavesAtTheShapesOwnDyingTime(t *testing.T) {
	const dying = 3500 * time.Millisecond
	const want = time.Second + dying // the reference's 1000 + GetDyingTime()
	w, p, mon := spawnedZoneWithShape(t, 11595, dying)
	at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle) // the kill

	c := corpseFor(t, w.Handler, mon.Handle)
	if got := c.leaveAt.Sub(at); got != want {
		t.Fatalf("a shape with DyingTime %s leaves %s after the kill, want the "+
			"reference's 1s + DyingTime = %s", dying, got, want)
	}
	if got := c.deadAt.Sub(at); got != want-DeathPacketLead {
		t.Fatalf("the ZC_DEAD is owed %s after the kill, want %s - the packet leaves "+
			"DeathPacketLead before the leave", got, want-DeathPacketLead)
	}

	// Condition 4, twice: at the floor, and one millisecond short of the shape's own mark.
	assertStillWaiting(t, w, p, mon, at.Add(CorpseLeaveDeadAniFloor),
		"at the 3 s floor, which this shape's 4500 ms has not reached")
	assertStillWaiting(t, w, p, mon, at.Add(want-time.Millisecond),
		"one millisecond short of the shape's own 4500 ms")

	w.corpseTick(at.Add(want))
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("%d 3531(s) at the shape's own deadline, want exactly 1:\n%s",
			n, w.logText())
	}
	if text := w.logText(); !strings.Contains(text, "4500ms after the ZC_DEAD") ||
		!strings.Contains(text, "shape DyingTime 3500ms") {
		t.Errorf("the leave line does not name the shape's 3500 ms and its 4500 ms "+
			"deadline:\n%s", text)
	}
}

// TestCorpseFloorsAShortDyingTime is pass condition 3: 300 ms is shorter than a death
// animation can finish in, so the corpse waits the reference's 3 s floor (1000 + 300 =
// 1300, floored) and not the row's 300.
func TestCorpseFloorsAShortDyingTime(t *testing.T) {
	w, p, mon := spawnedZoneWithShape(t, 11596, 300*time.Millisecond)
	at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle) // the kill

	c := corpseFor(t, w.Handler, mon.Handle)
	if got := c.leaveAt.Sub(at); got != CorpseLeaveDeadAniFloor {
		t.Fatalf("a shape with DyingTime 300ms leaves %s after the kill, want the "+
			"%s dead-animation floor", got, CorpseLeaveDeadAniFloor)
	}
	assertStillWaiting(t, w, p, mon, at.Add(CorpseLeaveDeadAniFloor-time.Millisecond),
		"one millisecond short of the 3 s dead-animation floor")
	w.corpseTick(at.Add(CorpseLeaveDeadAniFloor))
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("%d 3531(s) at the floor, want exactly 1:\n%s", n, w.logText())
	}
}

// TestTheBodyComesBackAsANewHandleAtFullHP is pass condition 2 of the respawn half: after
// the delay a NEW handle stands at the same point with the row's full HP, announced by a
// 3527, and the dead handle is gone from the roster.
func TestTheBodyComesBackAsANewHandleAtFullHP(t *testing.T) {
	w, p, mon := spawnedZone(t)
	fullHP, ok := w.monsterLiveHP(mon.Handle)
	if !ok || fullHP != mon.MaxHP || fullHP != 499900 {
		t.Fatalf("the spawn's own HP is %v (MaxHP %v), want the spec's 499900 - the fixture "+
			"must start at the number the return has to restore", fullHP, mon.MaxHP)
	}
	at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle)
	w.corpseTick(corpseFor(t, w.Handler, mon.Handle).leaveAt) // the leave
	p.conn.writes = nil                                       // measure the return on its own

	// ONE MILLISECOND SHORT of the return: still nothing.
	back := at.Add(fixtureSpawnerDelay)
	w.corpseTick(back.Add(-time.Millisecond))
	if n := len(enterFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d 3527(s) went out before the return was due, want 0:\n%s", n, w.logText())
	}

	w.corpseTick(back)
	sent := enterFrames(p.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the socket has %d 3527(s) at the return, want exactly 1:\n%s",
			len(sent), w.logText())
	}
	pkt := sent[0]
	if len(pkt) != EnterStandingMonsterSize {
		t.Fatalf("3527 is %d bytes, want %d", len(pkt), EnterStandingMonsterSize)
	}
	backHandle := binary.LittleEndian.Uint32(pkt[emHandle:])
	if backHandle == mon.Handle {
		t.Fatalf("the returning body carries the DEAD handle 0x%08x - it must be a new "+
			"entity id", backHandle)
	}
	if backHandle != mon.Handle+1 {
		t.Errorf("the new handle is 0x%08x, want the NEXT number in the block (0x%08x) - "+
			"the allocator only rises, so the return takes the one after the dead body",
			backHandle, mon.Handle+1)
	}

	// THE 3527 CARRIES THE FULL HP IN BOTH FIELDS (+0x1c current, +max), which is what a
	// fresh body is.
	for _, off := range []int{emHP, emMaxHP} {
		got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[off:]))
		if got != fullHP {
			t.Errorf("the 3527's +%d HP is %g, want the full %g", off, got, fullHP)
		}
	}

	// SAME CLASS, SAME POINT: the species at its own spawn point.
	if got := binary.LittleEndian.Uint32(pkt[emClass:]); got != mon.ClassID {
		t.Errorf("the new body's class is %d, want %d", got, mon.ClassID)
	}
	if x, y := int32(binary.LittleEndian.Uint32(pkt[emPos:])),
		int32(binary.LittleEndian.Uint32(pkt[emPos+4:])); x != mon.X || y != mon.Y {
		t.Errorf("the new body stands at (%d, %d), want (%d, %d)", x, y, mon.X, mon.Y)
	}

	// THE ROSTER: one body, the new handle, and the dead one nowhere.
	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) in the roster after the return, want 1 (the new body)", len(made))
	}
	if made[0].Handle != backHandle {
		t.Fatalf("the roster holds 0x%08x, the 3527 announced 0x%08x",
			made[0].Handle, backHandle)
	}
	if hp, ok := w.monsterLiveHP(backHandle); !ok || hp != fullHP {
		t.Fatalf("the new body's live HP is %v (present %v), want the full %g", hp, ok, fullHP)
	}
	if _, ok := w.monsterLiveHP(mon.Handle); ok {
		t.Error("the dead handle still has a live-HP entry")
	}
	if !strings.Contains(w.logText(), "RESPAWN") {
		t.Errorf("the return was not logged:\n%s", w.logText())
	}
}

// TestARespawnedBodyStartsAFreshDeathTimer is pass condition 5: the returned body arrives
// after ITS own delay (unchanged), and when it dies its corpse is armed from that death -
// a fresh instant and a fresh interval, never the first kill's.
func TestARespawnedBodyStartsAFreshDeathTimer(t *testing.T) {
	const dying = 3500 * time.Millisecond
	const leaveAfterKill = time.Second + dying
	w, p, first := spawnedZoneWithShape(t, 11597, dying)
	killer := p.sess.Team[0].Handle

	at := killAndDead(t, w.Handler, first, killer) // the first kill
	second := respawnNow(t, w.Handler, first.Handle)
	if second.Handle == first.Handle {
		t.Fatalf("the body came back on the dead handle")
	}

	at2 := killAndDead(t, w.Handler, second, killer) // the second kill
	if !at2.After(at) {
		t.Fatalf("the new body's kill is at %s, not after the first death's %s - "+
			"that is not a fresh timer", at2, at)
	}
	c2 := corpseFor(t, w.Handler, second.Handle)
	if got := c2.leaveAt.Sub(at2); got != leaveAfterKill {
		t.Fatalf("the new body's corpse leaves %s after ITS kill, want the shape's "+
			"1s + %s - the timer must start at this death, not the first one", got, dying)
	}
	if got := c2.backAt.Sub(at2); got != fixtureSpawnerDelay {
		t.Fatalf("the new body's return is %s after its kill, want the unchanged %s",
			got, fixtureSpawnerDelay)
	}
	if owed := corpsesOwed(t, w.Handler); len(owed) != 1 || owed[0].handle != second.Handle {
		t.Fatalf("the corpse list is %v, want exactly the new body's corpse", owed)
	}
}

// TestAMonsterWithNoShapeRowSaysSoOnceAndTakesTheAttackDelay is pass condition 6: the
// default fixture's row names no shape, so its corpse takes the branch its death is - a
// killing attack, 2 s - and the zone writes ONE line saying why there is no DyingTime to
// wait for, even across a second kill of the same class.
func TestAMonsterWithNoShapeRowSaysSoOnceAndTakesTheAttackDelay(t *testing.T) {
	w, p, first := spawnedZone(t)
	killer := p.sess.Team[0].Handle

	at := killAndDead(t, w.Handler, first, killer) // the kill
	c := corpseFor(t, w.Handler, first.Handle)
	if got := c.leaveAt.Sub(at); got != CorpseLeaveAttack {
		t.Fatalf("a body with no shape row leaves %s after the kill, want the %s "+
			"attack delay", got, CorpseLeaveAttack)
	}
	if n := strings.Count(w.logText(), "has NO shape row"); n != 1 {
		t.Fatalf("%d missing-shape line(s) after the first kill, want exactly 1:\n%s",
			n, w.logText())
	}
	assertStillWaiting(t, w, p, first, at.Add(CorpseLeaveAttack-time.Millisecond),
		"one millisecond short of the attack delay with no shape row")

	// A SECOND kill of the same class: the attack delay again, and still ONE line.
	second := respawnNow(t, w.Handler, first.Handle)
	at2 := killAndDead(t, w.Handler, second, killer)
	c2 := corpseFor(t, w.Handler, second.Handle)
	if got := c2.leaveAt.Sub(at2); got != CorpseLeaveAttack {
		t.Fatalf("the second body's corpse leaves %s after its kill, want the attack "+
			"delay", got)
	}
	if n := strings.Count(w.logText(), "has NO shape row"); n != 1 {
		t.Fatalf("%d missing-shape line(s) after two kills of one class, want exactly 1:"+
			"\n%s", n, w.logText())
	}
}

// TestTheNewBodyCanBeHitAndSwings is pass condition 3 of the respawn half: the returned
// body is a body - it takes damage through the ordinary ledger, and it swings at a player
// in its row's reach.
func TestTheNewBodyCanBeHitAndSwings(t *testing.T) {
	h, logs, victim, _, mon := swingZone(t,
		[3]int32{0, 150, 29},  // in reach: MinR 1 .. MaxR 100 edge to edge
		[3]int32{0, 3000, 29}) // a watcher inside the ring
	h.spawnedMonster[0].MaxHP = swingMonsterHP
	mon = h.spawnedMonster[0]

	killAndDead(t, h, mon, victim.sess.Team[0].Handle)
	back := respawnNow(t, h, mon.Handle)
	if back.Handle == mon.Handle {
		t.Fatalf("the body came back on the dead handle")
	}
	victim.conn.writes = nil // the swing is the only thing this half measures

	// HITTABLE: the ordinary ledger move, priced against the returned body's full HP.
	before, after, ok := h.damageMonster(back, 1)
	if !ok {
		t.Fatalf("the returned body has no live HP, so nothing can hit it")
	}
	if before != swingMonsterHP || after != swingMonsterHP-1 {
		t.Fatalf("one point of damage moved the new body %g -> %g, want %g -> %g",
			before, after, swingMonsterHP, swingMonsterHP-1)
	}

	// AND IT SWINGS, from its NEW handle, with its row's own numbers.
	h.monsterSwingTick(time.Unix(1_700_000_000, 0))
	frames := swingFrames(victim.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("the new body sent %d 3564(s) with a player in reach, want 1:\n%s",
			len(frames), logs.String())
	}
	f := decodeSwing(t, frames[0])
	if f.attacker != back.Handle {
		t.Errorf("the swing's attacker is 0x%08x, want the new handle 0x%08x",
			f.attacker, back.Handle)
	}
	if f.victim != victim.sess.Team[0].Handle {
		t.Errorf("the swing names victim 0x%08x, want 0x%08x",
			f.victim, victim.sess.Team[0].Handle)
	}
	if float64(f.atkSpd) != back.ASPD || back.ASPD != mon.ASPD {
		t.Errorf("the new body's cadence is %g (packet %d), want the row's %g",
			back.ASPD, f.atkSpd, mon.ASPD)
	}
}

// TestASecondKillBringsItBackAgain is pass condition 4 of the respawn half: no one-shot
// flag. The body that came back can be killed, leaves, and comes back once more - three
// distinct handles, one name.
func TestASecondKillBringsItBackAgain(t *testing.T) {
	w, p, first := spawnedZone(t)
	killer := p.sess.Team[0].Handle

	killAndDead(t, w.Handler, first, killer)
	second := respawnNow(t, w.Handler, first.Handle)

	killAndDead(t, w.Handler, second, killer)
	third := respawnNow(t, w.Handler, second.Handle)

	if first.Handle == second.Handle || second.Handle == third.Handle || first.Handle == third.Handle {
		t.Fatalf("the three bodies are not three handles: 0x%08x, 0x%08x, 0x%08x",
			first.Handle, second.Handle, third.Handle)
	}
	made := w.runtimeMonsters()
	if len(made) != 1 || made[0].Handle != third.Handle {
		t.Fatalf("the roster is %v, want one body on 0x%08x", made, third.Handle)
	}
	if hp, ok := w.monsterLiveHP(third.Handle); !ok || hp != first.MaxHP {
		t.Fatalf("the third body's HP is %v (present %v), want the full %g", hp, ok, first.MaxHP)
	}
	if n := strings.Count(w.logText(), "RESPAWN"); n != 2 {
		t.Fatalf("%d RESPAWN line(s), want 2 - the second kill must bring it back again:\n%s",
			n, w.logText())
	}
	if n := strings.Count(w.logText(), "CORPSE LEAVE"); n != 2 {
		t.Fatalf("%d CORPSE LEAVE line(s), want 2:\n%s", n, w.logText())
	}
}

// TestTheReturnFollowsTheSightRing is pass condition 5 of the respawn half: a watcher in
// view receives the new 3527; a watcher past ViewMiddleRange (but inside ViewRange) does
// not - the R135e cull, and it is the cull that did it, because that same watcher WAS told
// the corpse left.
func TestTheReturnFollowsTheSightRing(t *testing.T) {
	h, logs, victim, watcher, mon := swingZone(t,
		[3]int32{0, 150, 29},  // in view
		[3]int32{0, 4500, 29}) // inside ViewRange 6,000, past ViewMiddleRange 4,000
	if _, _, armed := h.sightRadii(); !armed {
		t.Fatal("the sight ring is not armed, so the cull is not being tested")
	}
	h.spawnedMonster[0].MaxHP = swingMonsterHP
	mon = h.spawnedMonster[0]

	killAndDead(t, h, mon, victim.sess.Team[0].Handle)
	h.corpseTick(corpseFor(t, h, mon.Handle).leaveAt)
	if n := len(leaveFrames(watcher.conn.sent())); n != 1 {
		t.Fatalf("the culled watcher got %d 3531(s) for the corpse, want 1 - it still holds "+
			"the object, so it must be told it left:\n%s", n, logs.String())
	}
	back := respawnNow(t, h, mon.Handle)

	if n := len(enterFrames(victim.conn.sent())); n != 1 {
		t.Fatalf("the player in view got %d 3527(s) for the new body, want 1:\n%s",
			n, logs.String())
	}
	if n := len(enterFrames(watcher.conn.sent())); n != 0 {
		t.Fatalf("a watcher 4,500 units out (middle range 4,000) got %d 3527(s) for the new "+
			"body - the R135e cull is missing:\n%s", n, logs.String())
	}
	if got := binary.LittleEndian.Uint32(enterFrames(victim.conn.sent())[0][emHandle:]); got != back.Handle {
		t.Errorf("the announcement named 0x%08x, the roster holds 0x%08x", got, back.Handle)
	}
}

// TestTheReturnedBodyRollsItsOwnDrop is pass condition 6 of the respawn half: the drop
// guard is one roll per BODY, and a body that comes back is a new body - so the second kill
// rolls again, on the new handle.
func TestTheReturnedBodyRollsItsOwnDrop(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, trophyRow(1, 1), items, &scriptedDropRand{t: t})
	body := monsterAtLevel(10)
	body.MaxHP = 500 // the full HP the return restores
	// A MAP SPAWNER'S BODY: this fixture is hand-built, so it states the spawner row it
	// stands for. Without one there is no return to test (zone/respawn.go).
	body.HasSpawner = true
	body.SpawnerDelay = fixtureSpawnerDelay
	h.spawnedMonster = []Monster{body}
	// The ledger a spawn writes (create.go SpawnMonsterObject): the body starts at its max.
	h.spawnMu.Lock()
	h.monsterHP = map[uint32]float64{body.Handle: body.MaxHP}
	h.spawnMu.Unlock()

	killAndDead(t, h, body, killer.sess.Team[0].Handle)
	back := respawnNow(t, h, body.Handle)
	if back.Handle == body.Handle {
		t.Fatalf("the body came back on the dead handle, so this test cannot tell a new " +
			"body from the same one")
	}

	// THE SECOND KILL, on the returned body: one point left, then the hit.
	h.spawnMu.Lock()
	h.monsterHP[back.Handle] = 1
	h.spawnMu.Unlock()
	if _, after, ok := h.damageMonsterBy(back, 1, killer.sess.Team[0].Handle, 10); !ok || after != 0 {
		t.Fatalf("the second kill did not empty the returned body (after %g, ok %v):\n%s",
			after, ok, logs.String())
	}
	// The roll belongs to the corpse's ZC_DEAD instant, not to the hit (zone/respawn.go
	// deathPacketTick): the second body's packet has to leave before its drop exists.
	h.deathPacketTick(corpseFor(t, h, back.Handle).deadAt)

	frames := itemFrames(watcher.conn.sent())
	if len(frames) != 2 {
		t.Fatalf("%d 3536(s) reached the watcher for two kills, want 2 - a respawned body "+
			"rolls its own drop:\n%s", len(frames), logs.String())
	}
	if got := binary.LittleEndian.Uint32(frames[0][ssiAppParent:]); got != body.Handle {
		t.Errorf("the first drop's parent is 0x%08x, want the first body 0x%08x",
			got, body.Handle)
	}
	if got := binary.LittleEndian.Uint32(frames[1][ssiAppParent:]); got != back.Handle {
		t.Errorf("the second drop's parent is 0x%08x, want the returned body 0x%08x",
			got, back.Handle)
	}
	if n := strings.Count(logs.String(), "30000 x1"); n != 2 {
		t.Errorf("%d drop line(s) naming the item, want 2 (one roll per body):\n%s",
			n, logs.String())
	}
}
