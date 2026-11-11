package zone

// THE SCRIPT KILL: `SendMsg(self, self, 'OM_KILL', 0, 0)` MAKES A ZOMBIE.
//
// The reference's `CMonsterNPC::OnKill` (`MonsterNPC.cpp:149-171`) sends NOTHING and
// removes nothing: the body is marked a zombie, its script threads are never resumed
// again (`THREAD_PRE_RESUME` answers false on `IsZombie()`, `FieldObject.cpp:29-34`),
// it stays where it stands, and the leave reaches a viewer only when THAT viewer's own
// view update finds the object a zombie (`View.cpp:193-196`, reason
// `OBJ_LEAVE_LOGOUT`). Ours removed the body and sent 3531 at that moment, which is the
// owner's "it vanish" on a monster he had kited out of the ring.
//
// These are the pass conditions, one test each:
//
//	(a) TestScriptKillZombiesTheBodyAndSendsNothing - the body STAYS in the roster,
//	    dead, and not one packet goes out;
//	(b) TestTheZombiesCorpseLeavesWithReasonZero - the corpse arms through the SAME
//	    handshake the death path uses, leaves at its own instant with reason 0, the body
//	    is gone from the roster, and the same corpse brings it back;
//	(c) TestScriptKillOnABodyItsDeathAlreadyArmedArmsNothing and
//	    TestTwoScriptKillsOfOneBodyLeaveOnce - a body that already has a corpse owed is
//	    not armed a second time, and exactly ONE leave is ever produced.

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

// zombieFixture is spawnedZone with the spawn's own traffic thrown away, so every
// packet a test reads was caused by the kill.
func zombieFixture(t *testing.T) (*fakeWorld, *player, Monster) {
	t.Helper()
	w, p, mon := spawnedZone(t)
	p.conn.writes = nil // the 3527 the spawn sent is not this card's evidence
	return w, p, mon
}

// (a) THE BODY IS NOT TAKEN OFF THE FIELD. It stays in the roster, at its point, with
// its HP ledger at 0 (which is what "dead" means here: the swing, think and chase ticks
// all refuse `hp <= 0`), and the call sends NOTHING - no 3531, no 3586.
func TestScriptKillZombiesTheBodyAndSendsNothing(t *testing.T) {
	w, p, mon := zombieFixture(t)

	// The fixture must start where a kill is observable: a body this zone keeps HP for.
	if hp, ok := w.monsterLiveHP(mon.Handle); !ok || hp != mon.MaxHP {
		t.Fatalf("the fixture's live HP is %v (ok=%v), want the row's %v - a body with "+
			"no ledger could not show the mark", hp, ok, mon.MaxHP)
	}

	if err := w.KillMonsterObject(mon.Handle); err != nil {
		t.Fatalf("KillMonsterObject: %v", err)
	}

	// 1. STILL ON THE FIELD: the same entry, at the same point, in its place.
	left := w.runtimeMonsters()
	if len(left) != 1 || left[0].Handle != mon.Handle {
		t.Fatalf("the roster after the kill is %+v, want the one body 0x%08x - a script "+
			"OM_KILL must not remove it", left, mon.Handle)
	}
	if left[0].X != mon.X || left[0].Y != mon.Y || left[0].Cell != mon.Cell {
		t.Errorf("the zombie stands at (%d, %d, cell %d), want its own (%d, %d, cell %d)",
			left[0].X, left[0].Y, left[0].Cell, mon.X, mon.Y, mon.Cell)
	}

	// 2. DEAD IN THIS ZONE: the ledger is 0. That is the one number the swing
	// (`monsterSwingTick`), think (`monsterAITick`) and chase (`monsterChaseTick`) ticks
	// read before they let a body act.
	if hp, ok := w.monsterLiveHP(mon.Handle); !ok || hp != 0 {
		t.Fatalf("the ledger is %v (ok=%v) after the kill, want 0 - the body must be "+
			"dead without being removed", hp, ok)
	}

	// 3. NOTHING SENT - not a 3531 and not anything else.
	if n := len(p.conn.sent()); n != 0 {
		t.Fatalf("%d packet(s) went out with the OM_KILL, want 0 (the leave belongs "+
			"to the corpse):\n%s", n, w.logText())
	}

	// 4. THE CORPSE IS OWED, and not yet left: the body must not come back before its
	// own leave has taken it off the field.
	c := corpseFor(t, w.Handler, mon.Handle)
	if c.left {
		t.Fatalf("the corpse 0x%08x is already marked left", mon.Handle)
	}

	// 5. ONE line, and it says what will happen rather than what was done.
	text := w.logText()
	if got := strings.Count(text, "OM_KILL"); got != 1 {
		t.Fatalf("%d OM_KILL line(s), want exactly one:\n%s", got, text)
	}
	for _, want := range []string{
		fmt.Sprintf("OM_KILL 0x%08x", mon.Handle),
		"class 11600",
		"is now a zombie",
		"no leave now",
		"the corpse leaves in 5s",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, text)
		}
	}
}

// (b) THE CORPSE IS THE DEATH PATH'S OWN, and only the corpse sends the leave: at its
// own instant, reason 0 (a 3 would FADE the body), once. The same handshake then brings
// the body back as a new handle at the point it stood.
func TestTheZombiesCorpseLeavesWithReasonZero(t *testing.T) {
	w, p, mon := zombieFixture(t)
	// THE RETURN IS LONGER THAN THE LEAVE ON PURPOSE. The script path's leave is the
	// reference's 5 s (CorpseLeaveIdle) and the fixture's spawner row is 5 s too; at that
	// pair the leave and the return fall on ONE tick (corpseTick runs the leave first),
	// and this test could not show the leave arriving before the return. Only the
	// fixture's row value moves - the row's number is still the only clock.
	const returnAfter = 8 * time.Second
	w.spawnMu.Lock()
	for i := range w.spawnedMonster {
		w.spawnedMonster[i].SpawnerDelay = returnAfter
	}
	w.spawnMu.Unlock()

	if err := w.KillMonsterObject(mon.Handle); err != nil {
		t.Fatalf("KillMonsterObject: %v", err)
	}
	c := corpseFor(t, w.Handler, mon.Handle)

	// THE SAME NUMBERS, UNADJUSTED: the reference's 5 s branch - a script's OM_KILL is not
	// an attack (source2 MonsterNPC.cpp:2588-2602) - with the ZC_DEAD DeathPacketLead
	// (500 ms) before the leave, and the fixture's own spawner row for the return (the
	// `-respawn` flag would replace the second, and this test runs without it). The
	// fixture carries a spawner row on purpose: a body with none is never brought back
	// (zone/respawn.go).
	killed := c.leaveAt.Add(-c.delay)
	if got := c.leaveAt.Sub(killed); got != CorpseLeaveIdle {
		t.Fatalf("the zombie's corpse leaves %s after the kill, want the %s idle delay "+
			"- the death path's own handshake", got, CorpseLeaveIdle)
	}
	if got := c.leaveAt.Sub(c.deadAt); got != DeathPacketLead {
		t.Fatalf("the ZC_DEAD is owed %s before the leave, want the %s packet lead "+
			"(m_deadTime = m_leaveTime - 500)", got, DeathPacketLead)
	}
	if got := c.backAt.Sub(killed); got != returnAfter {
		t.Fatalf("the zombie comes back %s after the kill, want its spawner row's %s",
			got, returnAfter)
	}

	// ONE MILLISECOND SHORT: nothing, and the body is still in the roster.
	w.corpseTick(c.leaveAt.Add(-time.Millisecond))
	if n := len(leaveFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d 3531(s) went out one millisecond early:\n%s", n, w.logText())
	}
	if len(w.runtimeMonsters()) != 1 {
		t.Fatalf("the zombie left the roster before its leave: %+v", w.runtimeMonsters())
	}

	// AT THE MARK: exactly one 3531, 8 bytes, reason 0, the zombie's handle.
	w.corpseTick(c.leaveAt)
	sent := leaveFrames(p.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the socket has %d 3531(s) at the corpse's deadline, want exactly 1:\n%s",
			len(sent), w.logText())
	}
	pkt := sent[0]
	if len(pkt) != LeaveMonsterSize {
		t.Fatalf("the leave is %d bytes, want %d", len(pkt), LeaveMonsterSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCLeaveMonster {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCLeaveMonster)
	}
	if got := binary.LittleEndian.Uint32(pkt[lmHandle:]); got != mon.Handle {
		t.Fatalf("the leave names 0x%08x, want the zombie 0x%08x", got, mon.Handle)
	}
	if code := binary.LittleEndian.Uint16(pkt[lmCode:]); code != LeaveCodeLogout {
		t.Fatalf("the reason is %d, want %d (OBJ_LEAVE_LOGOUT - 3 would make the "+
			"client FADE the body, and the corpse's leave must be the drop-at-once one)",
			code, LeaveCodeLogout)
	}

	// GONE: the roster entry and the HP ledger.
	for _, m := range w.runtimeMonsters() {
		if m.Handle == mon.Handle {
			t.Fatalf("0x%08x is still in the roster after its corpse left", mon.Handle)
		}
	}
	if _, ok := w.monsterLiveHP(mon.Handle); ok {
		t.Errorf("the zombie's handle still has a live-HP entry, want none")
	}

	// THE SAME CORPSE BRINGS IT BACK: a NEW handle, at the point it stood, at full HP.
	p.conn.writes = nil
	w.corpseTick(c.backAt)
	enters := enterFrames(p.conn.sent())
	if len(enters) != 1 {
		t.Fatalf("the socket has %d 3527(s) at the return, want exactly 1:\n%s",
			len(enters), w.logText())
	}
	back := binary.LittleEndian.Uint32(enters[0][emHandle:])
	if back == mon.Handle {
		t.Fatalf("the returning body carries the zombie's handle 0x%08x - it must be a "+
			"new entity id", back)
	}
	var got Monster
	for _, m := range w.runtimeMonsters() {
		if m.Handle == back {
			got = m
		}
	}
	if got.Handle == 0 {
		t.Fatalf("the returned handle 0x%08x is not in the roster", back)
	}
	if got.X != mon.X || got.Y != mon.Y {
		t.Errorf("the body came back at (%d, %d), want the point it was killed at "+
			"(%d, %d)", got.X, got.Y, mon.X, mon.Y)
	}
	if hp, ok := w.monsterLiveHP(back); !ok || hp != got.MaxHP {
		t.Errorf("the returned body's HP is %v (ok=%v), want its full %v", hp, ok, got.MaxHP)
	}
}

// (c1) A BODY THE KILLING HIT ALREADY MADE DEAD. `takeHP` armed the corpse a moment
// before the script's OM_KILL; the call must not arm a second one, must send nothing,
// and the ONE leave is the corpse's own instant.
func TestScriptKillOnABodyItsDeathAlreadyArmedArmsNothing(t *testing.T) {
	w, p, mon := zombieFixture(t)

	// The killing hit: the ZC_DEAD and the corpse, exactly as the death path does it.
	killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle)
	c := corpseFor(t, w.Handler, mon.Handle)
	p.conn.writes = nil

	if err := w.KillMonsterObject(mon.Handle); err != nil {
		t.Fatalf("KillMonsterObject on a dead body: %v", err)
	}

	// ONE corpse, not two, and the same one - no instant was recomputed.
	owed := corpsesOwed(t, w.Handler)
	if len(owed) != 1 {
		t.Fatalf("%d corpse(s) are owed after the second kill, want the ONE the hit "+
			"armed (a second would be a second 3531 and a second return): %+v",
			len(owed), owed)
	}
	if !owed[0].leaveAt.Equal(c.leaveAt) {
		t.Errorf("the corpse's leave moved from %s to %s - the OM_KILL re-armed it",
			c.leaveAt, owed[0].leaveAt)
	}

	// NOTHING SENT.
	if n := len(p.conn.sent()); n != 0 {
		t.Fatalf("%d packet(s) went out, want 0:\n%s", n, w.logText())
	}

	// ONE line, and it says it armed nothing.
	text := w.logText()
	if got := strings.Count(text, "OM_KILL"); got != 1 {
		t.Fatalf("%d OM_KILL line(s), want exactly one:\n%s", got, text)
	}
	for _, want := range []string{
		fmt.Sprintf("OM_KILL 0x%08x", mon.Handle),
		"was already dead",
		"no second corpse",
		"no leave now",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, text)
		}
	}

	// EXACTLY ONE leave, at the corpse's own instant, reason 0 - and a second tick
	// sends nothing more.
	w.corpseTick(c.leaveAt)
	sent := leaveFrames(p.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("%d 3531(s) at the corpse's instant, want exactly 1:\n%s",
			len(sent), w.logText())
	}
	if code := binary.LittleEndian.Uint16(sent[0][lmCode:]); code != LeaveCodeLogout {
		t.Errorf("the reason is %d, want %d", code, LeaveCodeLogout)
	}
	w.corpseTick(c.leaveAt)
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("a second tick sent the leave again (%d 3531s), want 1", n)
	}
}

// (c2) TWO OM_KILLS OF THE SAME LIVING BODY: the first arms the corpse, the second finds
// the body already a zombie. One corpse, one leave, two lines (one per call).
func TestTwoScriptKillsOfOneBodyLeaveOnce(t *testing.T) {
	w, p, mon := zombieFixture(t)

	if err := w.KillMonsterObject(mon.Handle); err != nil {
		t.Fatalf("the first KillMonsterObject: %v", err)
	}
	first := corpseFor(t, w.Handler, mon.Handle)

	// A second kill is not an error: the body is there, already dead.
	if err := w.KillMonsterObject(mon.Handle); err != nil {
		t.Fatalf("the second KillMonsterObject: %v", err)
	}

	owed := corpsesOwed(t, w.Handler)
	if len(owed) != 1 {
		t.Fatalf("%d corpse(s) are owed after two kills, want 1: %+v", len(owed), owed)
	}
	if !owed[0].leaveAt.Equal(first.leaveAt) {
		t.Errorf("the second kill moved the corpse's leave from %s to %s",
			first.leaveAt, owed[0].leaveAt)
	}
	if n := len(p.conn.sent()); n != 0 {
		t.Fatalf("%d packet(s) went out over the two kills, want 0:\n%s", n, w.logText())
	}

	// ONE LINE PER CALL, and the second says the body was already dead.
	text := w.logText()
	if got := strings.Count(text, "OM_KILL"); got != 2 {
		t.Fatalf("%d OM_KILL line(s) for two calls, want one each:\n%s", got, text)
	}
	if !strings.Contains(text, "is now a zombie") || !strings.Contains(text, "was already dead") {
		t.Errorf("the two lines do not tell the first kill from the second:\n%s", text)
	}
	if got := strings.Count(text, "the corpse leaves in"); got < 1 {
		t.Errorf("neither line says when the corpse leaves:\n%s", text)
	}

	w.corpseTick(first.leaveAt)
	if n := len(leaveFrames(p.conn.sent())); n != 1 {
		t.Fatalf("%d 3531(s) after two kills, want exactly 1:\n%s", n, w.logText())
	}
}
