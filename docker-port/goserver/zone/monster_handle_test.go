package zone

// THE MONSTER HANDLE ALLOCATOR - the live bug of 2026-11-08.
//
// The owner stood beside a monster and could not attack it: every press named
// 536870922 (0x2000000a) and the zone answered `ATTACK APPROACH OUT OF REACH ...
// 2,600 away` (logs\zone_go_script.log, 08:26:23-44, six presses). `0x2000000a` was
// handed out TWICE in that log: to `Monster_Hersu01` at 07:47:41 by the run that
// started 07:38, and to a respawned `Monster_Bigfoot` at 08:23:08 by the run that
// started 08:17 (`zone: UP` 08:17:53, client CONNECT 08:18:37 in between). The
// client outlived the zone process, kept the actor it was given for that number, and
// skipped the second spawn - so the body it drew and the body the zone resolved were
// never the same one.
//
// What makes that impossible is one rule, and these tests are the rule:
//
//	(a) a dead body, its respawn and a script body get THREE distinct numbers;
//	(b) a press naming the dead body's number is refused - never answered by the
//	    body that came back;
//	(c) the respawn's 3527 carries a number no 3527 of this process carried before;
//	(d) a run's numbers start in their own seeded window, never at the block's base
//	    (0x20000002 was every run's first number, which is exactly what a client
//	    that outlived a restart was still holding).

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"geserver/protocol"
)

// respawnBody drives one corpse to its return and answers the body that came back.
// It does NOT assume the new body has a new handle - that is what these tests are
// about - so the assertion stays in the test, not in this helper.
func respawnBody(t *testing.T, w *fakeWorld, dead uint32) Monster {
	t.Helper()
	c := corpseFor(t, w.Handler, dead)
	w.corpseTick(c.leaveAt)
	w.corpseTick(c.backAt)
	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d body/bodies in the roster after the return, want 1:\n%s",
			len(made), w.logText())
	}
	return made[0]
}

// TestEveryBodyGetsItsOwnNumber is (a). Create A, kill it, let it leave, then let it
// come back as B (the respawn) and create C (the script's CreateTmonster path).
// Three bodies, three numbers, rising, and A's number is nowhere in the roster.
func TestEveryBodyGetsItsOwnNumber(t *testing.T) {
	w, p, a := spawnedZone(t)

	killAndDead(t, w.Handler, a, p.sess.Team[0].Handle)
	b := respawnBody(t, w, a.Handle) // the leave, then the return

	ch, err := w.SpawnMonsterObject(scarecrowSpawn()) // C: what CreateTmonster calls
	if err != nil {
		t.Fatalf("the script body's spawn: %v", err)
	}
	c, ok := w.monsterByHandle(ch)
	if !ok {
		t.Fatalf("the script body 0x%08x is not in the roster", ch)
	}

	if a.Handle == b.Handle || a.Handle == c.Handle || b.Handle == c.Handle {
		t.Fatalf("three bodies share a handle: A 0x%08x, B 0x%08x, C 0x%08x - one "+
			"number, one actor on the client", a.Handle, b.Handle, c.Handle)
	}
	if !(a.Handle < b.Handle && b.Handle < c.Handle) {
		t.Fatalf("the numbers did not rise: A 0x%08x, B 0x%08x, C 0x%08x - the "+
			"allocator must only ever add to its counter", a.Handle, b.Handle, c.Handle)
	}
	for _, m := range w.runtimeMonsters() {
		if m.Handle == a.Handle {
			t.Fatalf("the dead body's 0x%08x is back in the roster (%q at (%d, %d))",
				a.Handle, w.MonsterName(m), m.X, m.Y)
		}
	}
}

// TestAStaleNumberIsRefusedNotAnsweredByTheNewBody is (b), and it goes through
// h.Reply - the real dispatch - because half of what a press does is the packet
// handler. A is dead and gone; B stands at A's own point with the SAME class and
// name. A press naming A's number must be one 3568 and one log line, and B's HP
// must not move: the number must not resolve to B.
func TestAStaleNumberIsRefusedNotAnsweredByTheNewBody(t *testing.T) {
	w, p, a := spawnedZone(t)
	killAndDead(t, w.Handler, a, p.sess.Team[0].Handle)
	b := respawnBody(t, w, a.Handle)
	stale := a.Handle

	if b.ClassID != a.ClassID || b.X != a.X || b.Y != a.Y {
		t.Fatalf("the fixture is wrong: B is not A's own body at A's own point "+
			"(%d vs %d, (%d,%d) vs (%d,%d))", b.ClassID, a.ClassID, b.X, b.Y, a.X, a.Y)
	}
	hpBefore, known := w.monsterLiveHP(b.Handle)
	if !known {
		t.Fatalf("the returning body has no HP in the ledger")
	}
	p.conn.writes = nil

	f := attackFrame(0, stale)
	out, err := w.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a press naming the dead number answered with %d packet(s), want 1 "+
			"(the 3568):\n%s", len(out), w.logText())
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != CmdZCAttackDisable {
		t.Fatalf("the answer is cmd %d, want %d ZC_ATTACK_DISABLE", cmd,
			CmdZCAttackDisable)
	}
	if got := binary.LittleEndian.Uint32(out[0][adTarget:]); got != stale {
		t.Fatalf("the 3568 names target 0x%08x, want the pressed (dead) 0x%08x",
			got, stale)
	}
	if n := len(swingFrames(p.conn.sent())); n != 0 {
		t.Fatalf("%d hit(s) went out for a dead number - a freed handle was resolved "+
			"to another body:\n%s", n, w.logText())
	}
	if hp, _ := w.monsterLiveHP(b.Handle); hp != hpBefore {
		t.Fatalf("the new body's HP moved %g -> %g on a press for the DEAD number - "+
			"the stale handle was answered by B", hpBefore, hp)
	}

	text := w.logText()
	if n := strings.Count(text, "ATTACK refused"); n != 1 {
		t.Fatalf("%d refusal line(s) for the dead number, want exactly 1:\n%s", n, text)
	}
	if !strings.Contains(text, strconv.FormatUint(uint64(stale), 10)) {
		t.Errorf("the refusal does not name the pressed handle %d:\n%s", stale, text)
	}
}

// TestARespawnSpawnCarriesANumberTheClientNeverHad is (c): every 3527 this socket
// was ever sent is remembered by its handle, and the respawn's 3527 must carry a
// number none of them did. That is the client's side of the rule - a number it has
// already been given is a number it may still be holding.
func TestARespawnSpawnCarriesANumberTheClientNeverHad(t *testing.T) {
	w, p, a := spawnedZone(t)

	seen := map[uint32]bool{}
	for _, pkt := range enterFrames(p.conn.sent()) {
		seen[binary.LittleEndian.Uint32(pkt[emHandle:])] = true
	}
	if !seen[a.Handle] {
		t.Fatalf("the fixture is wrong: the first body's own 3527 did not reach the "+
			"socket (saw %v, A is 0x%08x)", seen, a.Handle)
	}

	killAndDead(t, w.Handler, a, p.sess.Team[0].Handle)
	p.conn.writes = nil
	respawnBody(t, w, a.Handle)

	sent := enterFrames(p.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the return sent %d 3527(s), want exactly 1:\n%s", len(sent),
			w.logText())
	}
	back := binary.LittleEndian.Uint32(sent[0][emHandle:])
	if seen[back] {
		t.Fatalf("the respawn announced 0x%08x, a number this client was ALREADY "+
			"given - it skips a handle it holds, so the body it keeps and the body "+
			"the zone answers are two different actors", back)
	}
	if back == a.Handle {
		t.Fatalf("the respawn came back on the dead body's own 0x%08x", back)
	}
}

// TestARunStartsInItsOwnWindowNotAtTheBlockBase is (d). The number a run starts on
// is picked from a seeded window (crypto/rand), never the block's base: 0x20000002
// was every previous run's first handle, so a client that outlived a restart was
// holding exactly the numbers the next run was about to hand out.
func TestARunStartsInItsOwnWindowNotAtTheBlockBase(t *testing.T) {
	for i := 0; i < 32; i++ {
		seed := pickMonsterSeed()
		if seed < monsterHandleWindow || seed >= monsterBlockSpan-monsterHandleWindow {
			t.Fatalf("seed %d (0x%08x) is outside the block's window [0x%08x, 0x%08x) - "+
				"a run must not start at the base or reach the NPC block",
				seed, seed, uint32(monsterHandleWindow),
				uint32(monsterBlockSpan-monsterHandleWindow))
		}
	}

	// And the body itself: whatever the process has issued so far, a new one is
	// still inside the block and above the clearing window.
	_, _, a := spawnedZone(t)
	if a.Handle < MonsterHandleBase+monsterHandleWindow ||
		a.Handle >= MonsterHandleBase+monsterBlockSpan {
		t.Fatalf("a body got 0x%08x, outside the monster block's seeded window",
			a.Handle)
	}
	if a.Handle == MonsterHandle(runtimeMonsterFirst) {
		t.Fatalf("a body got 0x%08x - the number every run used to start on",
			a.Handle)
	}
}
