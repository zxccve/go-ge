package zone

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ZC_DEAD (3586): THE DEATH PACKET, ON THE CORPSE'S OWN CLOCK (REWRITTEN 2026-11-09).
//
// THE SPEC IS `history\monster-death.md` §1 and §2: `PKS_ZC_DEAD` is `PKS_DEFAULT` +
// one OBJHANDLE, six bytes, sent from the monster's own death state at
// `m_deadTime = m_leaveTime - 500` (`MonsterNPC.cpp:2629-2632`, `:2655`), to every
// player whose view holds the body.
//
// WHAT CHANGED, AND WHY. The packet used to leave inside the killing hit (2026-11-07),
// which put the 3586 AHEAD of the 3566 of the same blow - the owner saw the body fall
// before his hit registered. The reference does it from BS_DEAD's ON_UPDATE: the leave
// is `leaveDelay` after the kill and the packet is 500 ms before the leave, so the fall
// is drawn after the attack animation it lands on. The zone now copies that
// (zone/respawn.go deathPacketTick, called by corpseTick), and these tests drive the
// corpse's own instant (`corpse.deadAt`) instead of the press.
//
// THE FIXTURE IS attackZone's - the real stance table, a real sword, the captured
// Scarecrow - with ONE change: its HP is 1, so the single press these tests make IS the
// kill. A press that failed to kill is caught by name, not turned into a quiet pass.
// Its drop tables are given, not loaded, so nothing here reads the XML folder: this
// test is about the death packet, and a fixture that loaded tables inside the kill
// would be paying the old bug's cost by accident.

// deadZone is attackZone with a body ONE hit kills, and TWO sessions in the roster: the
// caster and one observer, so "the fan-out is every player in the zone" is measured on
// a socket neither of them shares.
func deadZone(t *testing.T) (*Handler, *stringBuf, *player, *player) {
	t.Helper()
	h, logs, p := attackZone(t)
	h.monsterHP = map[uint32]float64{capturedTarget: 1}
	h.Players = Players{}
	h.Players.Add(p.sess)
	other := newPlayer(0x00A11CE, "Observer", attackReachStand)
	h.Players.Add(other.sess)
	// The drop tables, given rather than loaded: a kill must not reach the disk, and
	// that is proved by its own test (drop_test.go, TestTheKillLoadsNoDropTable).
	h.Items = newItemTable(dropItem(30000, "Vis", "", 0, false))
	h.MapDrops = &MapDropTable{rows: map[int]MapDropRow{}}
	h.DropPenalty = flatPenalty(1)
	return h, logs, p, other
}

// deadPackets returns every ZC_DEAD among the packets a socket was written.
func deadPackets(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCDead {
			out = append(out, p)
		}
	}
	return out
}

// deadLines answers the handles the zone wrote a kill line for - one line per body whose
// ZC_DEAD left, on the corpse's own clock (zone/respawn.go deathPacketTick). It exists for
// the fixtures whose player roster is empty: the packet then goes "to 0 player(s)", so a
// socket cannot hold it, but the line names the body it was built for, which is what those
// tests must check.
func deadLines(text string) map[uint32]bool {
	const marker = "zone: DEAD "
	out := map[uint32]bool{}
	for _, line := range strings.Split(text, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		fields := strings.Fields(line[i+len(marker):])
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(fields[0], 10, 32); err == nil {
			out[uint32(n)] = true
		}
	}
	return out
}

// deadTick drives the corpse's ZC_DEAD instant for one body: the pass the packet, the
// drop roll and the exp award all belong to now (zone/respawn.go deathPacketTick).
func deadTick(t *testing.T, h *Handler, handle uint32) time.Time {
	t.Helper()
	c := corpseFor(t, h, handle)
	h.deathPacketTick(c.deadAt)
	return c.deadAt
}

// TestDeadPacketLeavesOnTheCorpsesClockNotWithTheHit is the whole slice: the press that
// empties the body sends NOTHING (the 3566 of the killing blow must reach the client
// first), and the ONE 6-byte 3586 goes out on the corpse's own clock - 500 ms before the
// leave - to BOTH sockets, and is never re-sent.
//
// THE ORDER IS THE POINT (2026-11-09): the reference raises BS_DEAD and sends the packet
// from its ON_UPDATE at `m_deadTime = m_leaveTime - 500` (MonsterNPC.cpp:2629-2632,
// :2653-2655), not from the blow, which is what put the fall ahead of the owner's hit.
func TestDeadPacketLeavesOnTheCorpsesClockNotWithTheHit(t *testing.T) {
	h, logs, p, other := deadZone(t)

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit, so nothing was killed:\n%s", logs.String())
	}
	if hp := h.monsterHPFor(capturedTarget); hp != 0 {
		t.Fatalf("the press left the monster at %v HP, want 0 - this test needs a KILL "+
			"(its damage is the stance's own ATK, read from the table):\n%s",
			hp, logs.String())
	}

	// NOTHING AT THE PRESS. The body is dead and its packet is owed, not sent.
	if got := len(deadPackets(other.conn.sent())); got != 0 {
		t.Fatalf("the observer has %d ZC_DEAD(s) when the killing press returned, want 0 "+
			"- the packet belongs to the corpse's clock:\n%s", got, logs.String())
	}
	if got := len(deadPackets(p.conn.sent())); got != 0 {
		t.Fatalf("the caster has %d ZC_DEAD(s) when the killing press returned, want 0:\n%s",
			got, logs.String())
	}

	// ON THE CORPSE'S OWN INSTANT: exactly one, 6 bytes, the dead handle - on both sockets.
	if c := corpseFor(t, h, capturedTarget); c.leaveAt.Sub(c.deadAt) != DeathPacketLead {
		t.Fatalf("the ZC_DEAD is owed %s before the leave, want the %s packet lead "+
			"(m_deadTime = m_leaveTime - 500)", c.leaveAt.Sub(c.deadAt), DeathPacketLead)
	}
	deadTick(t, h, capturedTarget)
	sent := deadPackets(other.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the observer has %d ZC_DEAD(s) at the corpse's instant, want exactly 1:\n%s",
			len(sent), logs.String())
	}
	pkt := sent[0]
	if len(pkt) != DeadSize {
		t.Fatalf("ZC_DEAD is %d bytes, want %d (cmd 2 + handle 4, no security header)",
			len(pkt), DeadSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCDead {
		t.Fatalf("the death packet's command is %d, want %d", cmd, CmdZCDead)
	}
	if got := binary.LittleEndian.Uint32(pkt[dmHandle:]); got != capturedTarget {
		t.Fatalf("ZC_DEAD +%d handle = %d, want the killed monster's %d",
			dmHandle, got, capturedTarget)
	}
	if got := len(deadPackets(p.conn.sent())); got != 1 {
		t.Fatalf("the caster's own client got %d ZC_DEAD(s), want 1 - the fan-out is "+
			"every player in the zone", got)
	}

	// THE KILL LINE says when the packet left and gives the byte count.
	text := logs.String()
	if !strings.Contains(text, "ZC_DEAD on the corpse's own clock") {
		t.Errorf("the kill line does not say the packet left on the corpse's clock:\n%s", text)
	}
	if !strings.Contains(text, "cmd 3586 (6 B)") {
		t.Errorf("the kill line does not give the byte count:\n%s", text)
	}
	if !strings.Contains(text, "to 2 player(s)") {
		t.Errorf("the kill line does not count the zone's 2 players:\n%s", text)
	}
	if strings.Contains(text, "ZC_DEAD in 500ms") {
		t.Errorf("the kill line still promises a 500 ms deadline:\n%s", text)
	}

	// NO SECOND PACKET. The body's HP are 0, so a further swing is refused before the
	// ledger, and nothing re-sends the death.
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess)
	deadTick(t, h, capturedTarget) // a second pass over the same corpse
	if got := len(deadPackets(other.conn.sent())); got != 1 {
		t.Fatalf("%d ZC_DEAD(s) after a later tick, want still 1", got)
	}
	if got := len(deadPackets(p.conn.sent())); got != 1 {
		t.Fatalf("the caster got %d ZC_DEAD(s) for one kill, want 1", got)
	}
}

// TestASecondHitOnTheDeadMonsterSendsNothing pins the other half: the swing that is due
// on a body with no HP left produces NO attack packet (the behaviour the repeat already
// has, `Zone\attack_swing.go`), and no second 3586 either.
func TestASecondHitOnTheDeadMonsterSendsNothing(t *testing.T) {
	h, logs, p, other := deadZone(t)

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit, so nothing was killed:\n%s", logs.String())
	}
	deadTick(t, h, capturedTarget)
	if got := len(deadPackets(other.conn.sent())); got != 1 {
		t.Fatalf("the death packet did not go out (%d), so the rest proves nothing:\n%s",
			got, logs.String())
	}

	// THE NEXT SWING IS DUE, and the body it was aimed at has no HP.
	swingDue(t, h, p.sess, 0)
	tick := h.swingTick(p.sess)
	if got := swingHits(t, tick); got != 0 {
		t.Fatalf("a swing at a dead body produced %d hit(s), want 0:\n%s",
			got, logs.String())
	}
	if len(tick) != 0 {
		t.Fatalf("a swing at a dead body produced %v, want nothing:\n%s",
			kinds(tick), logs.String())
	}
	if s := p.sess.swingOf(0); s != nil {
		t.Fatalf("the repeat is still armed on a dead body (%v), so it will keep "+
			"asking:\n%s", s, logs.String())
	}
	if !strings.Contains(logs.String(), "no HP left") {
		t.Errorf("the repeat did not say why it ended:\n%s", logs.String())
	}

	// A SECOND PRESS ON THE CORPSE ADDS NO SECOND 3586, on either socket.
	pressCleared(t, h, p.sess, capturedTarget)
	if got := len(deadPackets(other.conn.sent())); got != 1 {
		t.Fatalf("the observer got %d ZC_DEAD(s) for one kill, want 1", got)
	}
	if got := len(deadPackets(p.conn.sent())); got != 1 {
		t.Fatalf("the caster got %d ZC_DEAD(s) for one kill, want 1", got)
	}
}

// TestDeadPacketIsTheSixExactBytes asserts the BYTES, not the length: the command
// 3586 as the two little-endian bytes `02 0e`, then the handle little-endian, and
// nothing after it.
//
// 3586 IS 0x0E02, so its low byte goes first: `02 0e`. (`86 0e` would be 0x0E86 = 3718,
// a different command - the number in the task text was that slip, not this packet.)
func TestDeadPacketIsTheSixExactBytes(t *testing.T) {
	// A handle with four DIFFERENT bytes, so a big-endian write cannot pass.
	const forged = uint32(0x11223344)
	want := []byte{0x02, 0x0e, 0x44, 0x33, 0x22, 0x11}

	got := BuildDead(forged)
	if !bytes.Equal(got, want) {
		t.Fatalf("BuildDead(0x%08x)\n got % x\nwant % x", forged, got, want)
	}
	if len(got) != DeadSize {
		t.Fatalf("ZC_DEAD is %d bytes, want %d", len(got), DeadSize)
	}
	// THE ID IS THE CLIENT'S TABLE'S (`data\cmd_table.txt:1060`), never the header's
	// enum arithmetic: the shipped numbering would make this 3072.
	if CmdZCDead != 3586 {
		t.Fatalf("CmdZCDead is %d, the client's table says 3586", CmdZCDead)
	}
	if got := binary.LittleEndian.Uint16(want); got != CmdZCDead {
		t.Fatalf("the expected bytes read back as command %d, want %d", got, CmdZCDead)
	}
	if got := binary.LittleEndian.Uint32(want[dmHandle:]); got != forged {
		t.Fatalf("the expected bytes read back as handle 0x%08x, want 0x%08x", got, forged)
	}
}

// TestAMonsterThatTakesNoDamageNeverDies is the negative: the timer is armed by the
// KILL, not by the tick. A zone with a healthy monster owes nothing, no matter how many
// times its clock runs, and a hit that leaves HP above zero arms nothing either.
func TestAMonsterThatTakesNoDamageNeverDies(t *testing.T) {
	h, logs, p, other := deadZone(t)
	// The fixture's monster is healthy again: no hit can empty it here.
	h.monsterHP = map[uint32]float64{capturedTarget: 5000}

	if h.deadPending() {
		t.Fatalf("a zone that killed nothing owes a death packet already:\n%s",
			logs.String())
	}
	// THE SESSION'S OWN CLOCK, which is the path the packet rides: it must not even
	// want to run, because it has nothing to do.
	if out, ran := h.tickIfDue(p.sess); ran || len(out) != 0 {
		t.Fatalf("a quiet zone's tick ran (%v, %d packet(s)) with nothing to send:\n%s",
			ran, len(out), logs.String())
	}
	if n := len(deadPackets(other.conn.sent())) + len(deadPackets(p.conn.sent())); n != 0 {
		t.Fatalf("%d ZC_DEAD(s) went out with no kill at all:\n%s", n, logs.String())
	}

	// AND A HIT THAT DOES NOT EMPTY THE BODY ARMS NOTHING.
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit:\n%s", logs.String())
	}
	if hp := h.monsterHPFor(capturedTarget); hp <= 0 {
		t.Fatalf("the fixture's 5000 HP monster is at %v after one hit - a test of the "+
			"NO-kill case needs it alive:\n%s", hp, logs.String())
	}
	if h.deadPending() {
		t.Fatalf("a non-fatal hit left a corpse owed:\n%s", logs.String())
	}
	if n := len(deadPackets(other.conn.sent())); n != 0 {
		t.Fatalf("%d ZC_DEAD(s) went out for a monster that is still alive:\n%s",
			n, logs.String())
	}
}
