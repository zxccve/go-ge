package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"geserver/protocol"
)

// AN ACTION MUST BE VISIBLE TO THE OTHER PLAYERS (R93).
//
// The owner stood a second character in the field and fought with the first. The
// walker moved on the watcher's screen; the attacker and the caster did not - he saw
// a body STANDING STILL through every attack and every cast. His words: "animation
// not showing on the observer. walking no snap or stutter, only action such as normal
// attack or skill." Movement already fanned out (one 3548 per leg, one stop at the
// end); the action packets did not, on purpose, because no capture we hold has a
// second observer's screen in it. These tests are that missing screen.
//
// THE RULE THEY PIN IS ONE RULE, THREE TIMES: the packets of one action are built
// ONCE, handed to the caster, and handed to every other session UNCHANGED - the same
// rule TestMoveIsBroadcastUnchanged pins for a walk. So each test compares the
// watcher's bytes with the caster's bytes, and a difference in one byte fails.
//
// WHAT THEY DO NOT CLAIM: that a packet is well formed (its own lane's tests do
// that), or that the observer draws it (only a second client can say that).

// watcherInZone joins a second session - one character, its own socket - into a zone
// that already has its actor, and clears both sockets so the assertion is about the
// action and not about the two spawns the join writes.
func watcherInZone(t *testing.T, h *Handler, actor *player) *player {
	t.Helper()
	w := newPlayer(0x40000041, "Watcher", [3]int32{1, 1, 1})
	h.joinZone(actor.sess)
	h.joinZone(w.sess)
	actor.conn.writes, w.conn.writes = nil, nil
	return w
}

// TestAttackReachesTheOtherPlayer is check (a): a plain attack's ZC_ATTACK_SINGLE
// (3564) and its ZC_ADD_HP (3557) reach a second session in the zone BYTE-IDENTICAL
// to the caster's own copies.
func TestAttackReachesTheOtherPlayer(t *testing.T) {
	h, logs, p := attackZone(t)
	w := watcherInZone(t, h, p)

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// THE ACTOR'S OWN TRAFFIC IS UNCHANGED (check d): still the hit and then the HP.
	if len(out) != 2 {
		t.Fatalf("one attack produced %d packet(s) for the caster, want 2 (3564, 3557):\n%s",
			len(out), logs.String())
	}

	sent := w.conn.sent()
	if len(sent) != 2 {
		t.Fatalf("the other player got %d packet(s), want 2 (3564 then 3557):\n%s",
			len(sent), logs.String())
	}
	for i, pkt := range sent {
		if !bytes.Equal(pkt, out[i]) {
			t.Fatalf("the other player's packet %d (cmd %d) differs from the caster's "+
				"(cmd %d):\n got %x\nwant %x", i, cmdOf(pkt), cmdOf(out[i]), pkt, out[i])
		}
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCAttackSingle {
		t.Fatalf("the first packet the other player got is cmd %d, want %d",
			cmd, CmdZCAttackSingle)
	}
	if cmd := cmdOf(sent[1]); cmd != CmdZCAddHP {
		t.Fatalf("the second packet the other player got is cmd %d, want %d",
			cmd, CmdZCAddHP)
	}
	// The caster is told once by its own return value, not twice: sendOthers skips it.
	if got := len(p.conn.sent()); got != 0 {
		t.Fatalf("the caster's own socket got %d packet(s) from the fan-out, want 0 - "+
			"the actor's copy is the returned slice", got)
	}
	// AND THE LINE THE NEXT TWO-CLIENT TEST READS.
	if line := lineWith(logs.String(), "zone: ATTACK"); !strings.Contains(line, "-> 1 other player(s)") {
		t.Fatalf("the attack's fan-out line does not say `-> 1 other player(s)`:\n%s", line)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(logs.String()))
}

// TestAimedSkillReachesTheOtherPlayer is check (b): an aimed skill's ZC_SKILL_CASTING
// (3571, at the press), ZC_SKILL_MELEE (3573, when the cast's clock fires) and the
// target's ZC_ADD_HP (3557, the delta that flies) do the same - the watcher gets the
// caster's own bytes, in the same order.
func TestAimedSkillReachesTheOtherPlayer(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}
	// THE BODY HAS AN HP, because a spawned body always does (zone\create.go seeds the
	// ledger at spawn). 250,000 and NOT scarecrow()'s 1,000,000, on purpose: the 3557's
	// nowHP then proves the landing read the LEDGER and not the Monster row's MHP.
	h.monsterHP = map[uint32]float64{capturedTarget: 250000}
	w := watcherInZone(t, h, p)

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCSkillCasting {
		t.Fatalf("the press produced %d packet(s) for the caster, want the 3571 alone:\n%s",
			len(out), logs.String())
	}

	// THE WIND-UP IS THE ANIMATION the observer was missing.
	sent := w.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("the other player got %d packet(s) at the press, want the 3571:\n%s",
			len(sent), logs.String())
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCSkillCasting {
		t.Fatalf("the other player's packet is cmd %d, want %d", cmd, CmdZCSkillCasting)
	}
	if !bytes.Equal(sent[0], out[0]) {
		t.Fatalf("the other player's 3571 differs from the caster's:\n got %x\nwant %x",
			sent[0], out[0])
	}

	// THE HIT, ON THE CLOCK: the 3573 AND the target's 3557 - the reference's own order
	// (history\combat.md §9.2: the 3573, then the 3557 carrying the same damage double).
	castDueNow(t, p.sess, 0)
	fired := h.chaseTick(p.sess)
	want := []uint16{CmdZCSkillMelee, CmdZCAddHP}
	if len(fired) != len(want) {
		t.Fatalf("the cast's clock produced %d packet(s) for the caster, want %d "+
			"(3573, 3557):\n%s", len(fired), len(want), logs.String())
	}
	for i, cmd := range want {
		if got := cmdOf(fired[i]); got != cmd {
			t.Fatalf("the caster's packet %d is cmd %d, want %d", i, got, cmd)
		}
	}
	// THE DELTA IS THE LEDGER'S NUMBER, not the spawn's minus the damage: the plate the
	// client moves has to be the HP this zone believes the body is on.
	damage := math.Float64frombits(binary.LittleEndian.Uint64(fired[0][smDamage:]))
	if got, want := math.Float64frombits(binary.LittleEndian.Uint64(fired[1][ahNowHP:])), 250000-damage; got != want {
		t.Fatalf("the 3557's nowHP = %v, want %v (the ledger's 250000 minus the %v "+
			"the 3573 carries)", got, want, damage)
	}
	sent = w.conn.sent()
	if len(sent) != 1+len(want) {
		t.Fatalf("the other player got %d packet(s) in total, want %d (3571, 3573, "+
			"3557):\n%s", len(sent), 1+len(want), logs.String())
	}
	for i, cmd := range want {
		got := sent[1+i]
		if cmdOf(got) != cmd {
			t.Fatalf("the other player's packet %d is cmd %d, want %d", 1+i, cmdOf(got), cmd)
		}
		if !bytes.Equal(got, fired[i]) {
			t.Fatalf("the other player's cmd %d differs from the caster's:\n got %x\nwant %x",
				cmd, got, fired[i])
		}
	}
	if got := len(p.conn.sent()); got != 0 {
		t.Fatalf("the caster's own socket got %d packet(s) from the fan-out, want 0 - "+
			"the actor's copy is the returned slice", got)
	}
	for _, word := range []string{"-> 1 other player(s)"} {
		if !strings.Contains(logs.String(), word) {
			t.Fatalf("the cast's log does not carry %q:\n%s", word, logs.String())
		}
	}
	if line := lineWith(logs.String(), "LANDED"); !strings.Contains(line, "-> 1 other player(s)") {
		t.Fatalf("the 3573's fan-out line does not say `-> 1 other player(s)`:\n%s", line)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(logs.String()))
}

// TestAreaCastReachesTheOtherPlayer is check (c): a ground cast's 3581 and EVERY
// 3557 reach the observer, byte-identical and in the capture's order - and the 3654
// (the caster's own SP) does NOT, because it names a number only the caster's HUD
// draws.
func TestAreaCastReachesTheOtherPlayer(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	first := areaScarecrow()
	second := Monster{Handle: 0x182327, X: -2250, Y: 1300, Cell: 49,
		ClassID: 11600, Level: 1, HP: 500000}
	spawnWithHP(h, first, second)
	w := watcherInZone(t, h, p)

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(press) != 2 || cmdOf(press[0]) != CmdZCUpdateSP ||
		cmdOf(press[1]) != CmdZCSkillCasting {
		t.Fatalf("the press produced %d packet(s) for the caster, want 3654 then 3571:\n%s",
			len(press), logs.String())
	}

	// AT THE PRESS: the 3571 and ONLY the 3571. The 3654 is the caster's own SP.
	sent := w.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("the other player got %d packet(s) at the press, want 1 (the 3571):\n%s",
			len(sent), logs.String())
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCSkillCasting {
		t.Fatalf("the other player's press packet is cmd %d, want %d - the %d names "+
			"the caster's own SP and stays with it", cmd, CmdZCSkillCasting, CmdZCUpdateSP)
	}
	if !bytes.Equal(sent[0], press[1]) {
		t.Fatalf("the other player's 3571 differs from the caster's:\n got %x\nwant %x",
			sent[0], press[1])
	}

	// AT THE CLOCK: the 3581 and one 3557 per victim, in that order.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	fired := h.chaseTick(p.sess)
	want := []uint16{CmdZCSkillArea, CmdZCAddHP, CmdZCAddHP}
	if len(fired) != len(want) {
		t.Fatalf("the clock produced %d packet(s) for the caster, want %d (3581, 3557 x2):\n%s",
			len(fired), len(want), logs.String())
	}
	sent = w.conn.sent()
	if len(sent) != 1+len(want) {
		t.Fatalf("the other player got %d packet(s) in total, want %d (3571, 3581, "+
			"3557 x2):\n%s", len(sent), 1+len(want), logs.String())
	}
	for i, cmd := range want {
		got := sent[1+i]
		if cmdOf(got) != cmd {
			t.Fatalf("the other player's packet %d is cmd %d, want %d", 1+i, cmdOf(got), cmd)
		}
		if !bytes.Equal(got, fired[i]) {
			t.Fatalf("the other player's cmd %d differs from the caster's:\n got %x\nwant %x",
				cmd, got, fired[i])
		}
	}
	// Every victim's 3557 is there, not only the blast: the number that flies.
	seen := map[uint32]bool{}
	for _, pkt := range sent[2:] {
		seen[binary.LittleEndian.Uint32(pkt[ahHandle:])] = true
	}
	if !seen[first.Handle] || !seen[second.Handle] {
		t.Fatalf("the other player's 3557s name %v, want both victims %d and %d",
			seen, first.Handle, second.Handle)
	}
	if line := lineWith(logs.String(), "LANDED"); !strings.Contains(line, "-> 1 other player(s)") {
		t.Fatalf("the blast's fan-out line does not say `-> 1 other player(s)`:\n%s", line)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(logs.String()))
}

// TestAnActionAloneInTheZoneWritesNothingToTheOtherSockets is check (d), the safe
// half: with ONE session in the zone every fan-out reaches nobody, so the caster's
// own traffic is exactly what it was before R93 - and nothing panics on an empty
// roster.
func TestAnActionAloneInTheZoneWritesNothingToTheOtherSockets(t *testing.T) {
	h, logs, p := attackZone(t)
	h.joinZone(p.sess)
	p.conn.writes = nil

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("an attack in an empty zone produced %d packet(s) for the caster, want "+
			"2 (3564, 3557):\n%s", len(out), logs.String())
	}
	if got := len(p.conn.sent()); got != 0 {
		t.Fatalf("the fan-out wrote %d packet(s) back to the actor's own socket, want 0", got)
	}
	// And the count says 0: a solo fight never claims to have reached anybody, which
	// is exactly what makes the next two-client test checkable from the log - a 0
	// WHILE a second client is connected means the roster, not the broadcast, is at
	// fault.
	if line := lineWith(logs.String(), "zone: ATTACK"); !strings.Contains(line, "-> 0 other player(s)") {
		t.Fatalf("a solo attack's line does not say `-> 0 other player(s)`:\n%s", line)
	}
}

// farWatcher joins a second session 4,500 units from the actor's BODY - inside map 1331's
// ViewRange (6,000) and past its ViewMiddleRange (4,000), which is exactly the band R135e
// froze - and clears both sockets so an assertion is about the action and not about the
// two joins.
//
// THE DISTANCE IS FROM THE BODY THE FAN-OUT MEASURES (zone\sight.go: cullActorAt), not
// from `sess.Pos` and not from Team[0]: an action names ONE handle (the caster's
// character, which is index 2 in the area lane's three-member team), and that handle's
// own stood point is what has to be 4,500 away.
func farWatcher(t *testing.T, h *Handler, actor *player, idx int) *player {
	t.Helper()
	sightUseTable(t, map[int]SightRow{1331: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = 1331
	at := actor.sess.Team[idx].Position
	far := [3]int32{at[0] + 4500, at[1], at[2]}
	w := newPlayer(0x40000043, "Far", far)
	h.joinZone(actor.sess)
	h.joinZone(w.sess)
	cullPlace(h, w, far)
	actor.conn.writes, w.conn.writes = nil, nil
	return w
}

// TestAnActionIsCulledPastTheMiddleRange is check (d): the R135e update cull reaches the
// ACTIONS, and it reaches them without one line of action code asking for it - every
// action family fans out through sendOthers, the same door a walk uses.
//
// A viewer whose body stands 4,500 away (inside 6,000, past 4,000) keeps the actor in its
// world and receives NONE of the packets: no 3564, no 3573, no 3581, no 3557, and no 3525
// either (nothing left; the actor is frozen, not gone).
//
// THIS IS history\monster-attack.md:47-56 TURNED AROUND. The reference's own culled
// whitelist does NOT carry ZC_ATTACK_*, so a hand-rolled "action broadcast" beside the
// movement one would hand a frozen viewer a swing. Going through sendOthers is what makes
// that impossible here, and this test is what keeps the next lane from opening a second
// door.
func TestAnActionIsCulledPastTheMiddleRange(t *testing.T) {
	t.Run("normal attack", func(t *testing.T) {
		h, logs, p := attackZone(t)
		w := farWatcher(t, h, p, 0)

		f := attackFrame(0, capturedTarget)
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZAttack, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		// THE ACTION HAPPENED: without this, a refusal would look like a working cull.
		if len(out) != 2 {
			t.Fatalf("the attack produced %d packet(s) for the caster, want 2 (3564, "+
				"3557):\n%s", len(out), logs.String())
		}
		if sent := w.conn.sent(); len(sent) != 0 {
			t.Fatalf("the watcher stands 4,500 away, past the 4,000 middle range, yet it "+
				"received %v", sightCmds(sent))
		}
	})

	t.Run("aimed skill", func(t *testing.T) {
		h, logs := castZone(t)
		p := castPlayer()
		h.spawnedMonster = []Monster{scarecrow()}
		h.monsterHP = map[uint32]float64{capturedTarget: 250000}
		w := farWatcher(t, h, p, 0)

		f := skillTargetFrame(0, capturedSkill, capturedTarget)
		press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if len(press) != 1 {
			t.Fatalf("the press produced %d packet(s) for the caster, want the 3571:\n%s",
				len(press), logs.String())
		}
		castDueNow(t, p.sess, 0)
		fired := h.chaseTick(p.sess)
		if len(fired) != 2 {
			t.Fatalf("the cast's clock produced %d packet(s) for the caster, want the "+
				"3573 and the 3557:\n%s", len(fired), logs.String())
		}
		if sent := w.conn.sent(); len(sent) != 0 {
			t.Fatalf("the watcher stands 4,500 away, past the 4,000 middle range, yet it "+
				"received %v", sightCmds(sent))
		}
	})

	t.Run("area cast", func(t *testing.T) {
		h, logs := groundZone(t)
		p := warlock()
		spawnWithHP(h, areaScarecrow(), Monster{Handle: 0x182327, X: -2250, Y: 1300,
			Cell: 49, ClassID: 11600, Level: 1, HP: 500000})
		w := farWatcher(t, h, p, capturedGroundCharIndex)

		f := groundFrame(capturedGroundCharIndex, capturedFireball,
			[3]int32{capturedPointX, capturedPointY, capturedPointCell})
		press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZSkillGround, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if len(press) != 2 {
			t.Fatalf("the press produced %d packet(s) for the caster, want 3654 then "+
				"3571:\n%s", len(press), logs.String())
		}
		castDueNow(t, p.sess, capturedGroundCharIndex)
		fired := h.chaseTick(p.sess)
		if len(fired) != 3 {
			t.Fatalf("the blast produced %d packet(s) for the caster, want 3581 and two "+
				"3557s:\n%s", len(fired), logs.String())
		}
		if sent := w.conn.sent(); len(sent) != 0 {
			t.Fatalf("the watcher stands 4,500 away, past the 4,000 middle range, yet it "+
				"received %v", sightCmds(sent))
		}
	})
}
