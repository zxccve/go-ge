package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"geserver/protocol"
)

// THE STANCE KEY: CZ_CHANGE_STANCE (3625) -> ZC_CHANGE_STANCE (3554).
//
// The request is 15 bytes (cmd(2) seq(4) checksum(4) charIndex(1) stanceType(4),
// PKS_CZ_CHANGE_STANCE over PKS_SECURITY_DEFAULT, geZCPacket.h:1788 + gePacketBase.h:52)
// and the answer is 10 (cmd(2) handle(4) stanceType(4), geZCPacket.h:1794). Both
// sizes are the ones the command table registers, so a test that builds one of
// these frames by hand is building exactly what the client sends.
//
// THE PERMISSION RULE IS THE CHARACTER'S JOB AND ITS TWO HANDS, never the map:
//
//	job 1 "Fighter", Shield left + Sword right -> condition 102 -> 10110 "BackGuard",
//	10120 "HighGuard", 40823 "AcGuard"
//	job 5 "Warlock", bare hands -> condition 500 -> 20210 "PossessionFire"
//
// So 10120 is a stance job 1 may stand in with a shield and a sword, and 20210 is
// one it never learns: the pair separates "allowed" from "another job's stance"
// with no made-up ids anywhere. And there is NO map rule to test around: a stance
// change in a town is allowed (the owner's correction of 2026-10-31, pinned by
// TestChangeStanceWorksOnTowns below).

// changeStanceFrame builds one CZ_CHANGE_STANCE from the struct's own offsets:
// cmd(2) at +0, seq(4) at +2, checksum(4) at +6, charIndex(1) at +10, stance(4) at +11.
func changeStanceFrame(charIndex uint8, stance uint32) []byte {
	b := make([]byte, czChangeStanceSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZChangeStance)
	binary.LittleEndian.PutUint32(b[2:], 7)      // seq
	binary.LittleEndian.PutUint32(b[6:], 0x1234) // checksum
	b[10] = charIndex
	binary.LittleEndian.PutUint32(b[11:], stance)
	return b
}

// stanceZone is one owner whose leader is a JOB 1 Fighter holding a SHIELD in its
// left hand and a SWORD in its right (the class ids are real rows of the game's
// weapon table: 7215 "Shield", 7201 "Sword", neither two-handed), plus a watcher
// that joins AFTER the owner - so the watcher already knows the owner's handle by
// the time the stance change is sent. Both sockets are emptied after the joins, so
// a test reads only what the stance change itself produced.
//
// `initial` is the stance the character is spawned STANDING IN, and it is a
// parameter because the interesting assertion is that a request MOVES it: a test
// that spawns in the same stance it then asks for cannot tell an applied change from
// one that was never applied.
//
// The tables come from stanceDir() and are skipped when they cannot be read, the
// same way stance_test.go does it - the behaviour under test is the rule the tables
// describe, so a test without them would be testing nothing.
func stanceZone(t *testing.T, initial uint32) (*Handler, *bytes.Buffer, *player, *player) {
	t.Helper()
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), Stances: table}

	a := newPlayer(0x40000031, "Fighter", [3]int32{1000, 1000, 1})
	a.sess.Team[0].Job = 1 // Fighter: condition 102 is reachable only with Shield+Sword
	a.sess.Team[0].Stance = initial
	a.sess.Team[0].Equip[4] = 7215 // Shield, left hand (HandSpotLeft)
	a.sess.Team[0].Equip[5] = 7201 // Sword, right hand (HandSpotRight)

	b := newPlayer(0x40000041, "Watcher", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess) // AFTER the owner, so it holds the owner's handle already
	a.conn.writes, b.conn.writes = nil, nil
	return h, logs, a, b
}

// TestParseChangeStance pins the two field offsets against the struct: the char
// index is ONE byte at +10, and the stance is a DWORD at +11 - not at +12, which is
// where it would sit if the header's 10 bytes were counted wrong. A 14-byte frame
// must be refused rather than read past.
func TestParseChangeStance(t *testing.T) {
	frame := changeStanceFrame(2, 10010)
	if got := len(frame); got != 15 {
		t.Fatalf("the frame builder made %d bytes, want CZ_CHANGE_STANCE's 15", got)
	}
	req, ok := ParseChangeStance(frame)
	if !ok {
		t.Fatal("a 15-byte frame was refused")
	}
	if req.CharIndex != 2 {
		t.Fatalf("charIndex = %d, want 2 (byte +10)", req.CharIndex)
	}
	if req.Stance != 10010 {
		t.Fatalf("stance = %d, want 10010 (dword at +11)", req.Stance)
	}
	if _, ok := ParseChangeStance(frame[:14]); ok {
		t.Fatal("a 14-byte frame was accepted - it must be refused, not read past")
	}
}

// TestChangeStanceReplyIsTenBytes is the whole fan-out: one 10-byte 3554 for the
// owner, byte-identical to the one every other player gets, and the session's own
// copy moved to the new stance so a later spawn carries it.
//
// It goes through h.Reply, not h.changeStance, on purpose: the dispatch case is
// half of what makes the key work at all, and a test that calls the handler
// directly would pass with no case in the switch.
func TestChangeStanceReplyIsTenBytes(t *testing.T) {
	const stance = uint32(10120)      // HighGuard: condition 102, job 1 Shield+Sword
	h, logs, a, b := stanceZone(t, 0) // spawned with NO stance: the change must move it
	const handle = uint32(0x40000031)

	frame := changeStanceFrame(0, stance)
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) < 2 {
		t.Fatalf("Reply answered %d packet(s), want the 3554 and the 3684", len(out))
	}

	pkt := out[0]
	if len(pkt) != ChangeStanceSize {
		t.Fatalf("owner's first packet is %d bytes, want %d", len(pkt), ChangeStanceSize)
	}
	if cmd := cmdOf(pkt); cmd != CmdZCChangeStance {
		t.Fatalf("owner's first packet cmd = %d, want %d", cmd, CmdZCChangeStance)
	}
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != handle {
		t.Fatalf("handle = 0x%08x, want the character's own 0x%08x", got, handle)
	}
	if got := binary.LittleEndian.Uint32(pkt[6:]); got != stance {
		t.Fatalf("stance = %d, want %d", got, stance)
	}

	// The watcher gets the SAME BYTES, not a second packet built from the same
	// numbers: a per-recipient rebuild is how two clients end up disagreeing.
	seen := b.conn.sent()
	if len(seen) != 1 {
		t.Fatalf("the watcher received %d packet(s), want exactly the one 3554", len(seen))
	}
	if !bytes.Equal(seen[0], pkt) {
		t.Fatalf("the watcher's packet differs from the owner's:\n owner %x\n other %x",
			pkt, seen[0])
	}
	// The owner's copy is the RETURNED one: sendOthers skips the sender, so nothing
	// may have been written to the owner's own socket behind its back.
	if got := len(a.conn.sent()); got != 0 {
		t.Fatalf("the owner's socket was written %d packet(s) directly, want the reply "+
			"to be the returned slice only", got)
	}

	if got := a.sess.Team[0].Stance; got != stance {
		t.Fatalf("session stance = %d, want %d", got, stance)
	}
	// The sheet follows the stance, and the log says so in one line.
	if cmd := cmdOf(out[1]); cmd != CmdZCUpdateAllStatus {
		t.Fatalf("owner's second packet cmd = %d, want the 3684 status sheet", cmd)
	}
	if !strings.Contains(logs.String(), "3684 re-sent") {
		t.Fatalf("the summary line did not say the 3684 was re-sent:\n%s", logs.String())
	}
}

// TestChangeStanceRefusedForAnotherJobsStance is the permission rule: 20210 is a
// WARLOCK stance (job 5, condition 500), so job 1 may never stand in it, whatever
// the packet says - not even holding the sword and shield that make 10120 legal.
//
// Nothing may be written to EITHER socket and the session's stance must not move -
// a refusal that still broadcasts would put a stance on every other screen that the
// client can draw but its owner cannot use.
func TestChangeStanceRefusedForAnotherJobsStance(t *testing.T) {
	const allowed = uint32(10120)
	h, logs, a, b := stanceZone(t, allowed)

	frame := changeStanceFrame(0, 20210) // PossessionFire: condition 500, job 5 Warlock
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	for _, p := range out {
		if len(p) >= 2 && cmdOf(p) == CmdZCChangeStance {
			t.Fatalf("a refused change still answered a %d-byte 3554 (stance %d)",
				len(p), binary.LittleEndian.Uint32(p[6:]))
		}
	}
	if len(out) != 0 {
		t.Fatalf("a refused change answered %d packet(s), want none", len(out))
	}
	if got := len(a.conn.sent()); got != 0 {
		t.Fatalf("the owner was written %d packet(s), want none", got)
	}
	for _, p := range b.conn.sent() {
		if len(p) >= 2 && cmdOf(p) == CmdZCChangeStance {
			t.Fatalf("the watcher received a 3554 for a stance that was refused")
		}
	}
	if got := len(b.conn.sent()); got != 0 {
		t.Fatalf("the watcher was written %d packet(s), want none", got)
	}
	if got := a.sess.Team[0].Stance; got != allowed {
		t.Fatalf("session stance moved to %d, want it left at %d", got, allowed)
	}
	logText := logs.String()
	if !strings.Contains(logText, "CHANGE_STANCE refused") {
		t.Fatalf("no refusal was logged:\n%s", logText)
	}
	if !strings.Contains(logText, "20210") {
		t.Fatalf("the refusal did not name the stance id 20210:\n%s", logText)
	}
	if !strings.Contains(logText, "job 1") {
		t.Fatalf("the refusal did not name the character's job 1:\n%s", logText)
	}
}

// TestChangeStanceWorksOnTowns pins the OWNER'S CORRECTION of 2026-10-31: a stance
// change in a TOWN is allowed. The shipped client does not refuse it - only the
// displayed skill changes there, because a town has no combat.
//
// This test exists because the earlier revision of this lane refused a change on a
// "CITY" map, copying `World()->IsVillageMap()` out of
// source2\geClient\Source\GENormalObject.cpp:2231 - a PRE-RELEASE reference, not
// what the shipped client does. So the SAME accepted request is run on two towns
// and a field map: every one of them must answer the 10-byte 3554, because there is
// no map rule left to stop it.
func TestChangeStanceWorksOnTowns(t *testing.T) {
	const stance = uint32(10120) // HighGuard, the one TestChangeStanceReplyIsTenBytes uses
	for _, tc := range []struct {
		mapID int
		name  string
	}{
		{10, "reb_basic"},  // Reboldoeux, a town
		{15, "coim_basic"}, // Coimbra, a town
		{1331, "fld_auch"}, // a field map: the same answer, no exception either way
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, a, b := stanceZone(t, 0)
			h.MapID = tc.mapID
			h.MapName = tc.name

			frame := changeStanceFrame(0, stance)
			out, err := h.Reply(context.Background(), a.sess,
				protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
			if err != nil {
				t.Fatalf("Reply: %v", err)
			}
			if len(out) == 0 {
				t.Fatalf("map %d (%s) answered no packet - a stance change is allowed on "+
					"every map, towns included", tc.mapID, tc.name)
			}
			pkt := out[0]
			if len(pkt) != ChangeStanceSize {
				t.Fatalf("map %d (%s): owner's first packet is %d bytes, want %d",
					tc.mapID, tc.name, len(pkt), ChangeStanceSize)
			}
			if cmd := cmdOf(pkt); cmd != CmdZCChangeStance {
				t.Fatalf("map %d (%s): cmd = %d, want %d",
					tc.mapID, tc.name, cmd, CmdZCChangeStance)
			}
			if got := binary.LittleEndian.Uint32(pkt[6:]); got != stance {
				t.Fatalf("map %d (%s): stance = %d, want %d",
					tc.mapID, tc.name, got, stance)
			}
			if got := a.sess.Team[0].Stance; got != stance {
				t.Fatalf("map %d (%s): session stance = %d, want %d",
					tc.mapID, tc.name, got, stance)
			}
			seen := b.conn.sent()
			if len(seen) != 1 || !bytes.Equal(seen[0], pkt) {
				t.Fatalf("map %d (%s): the watcher received %d packet(s), want the one "+
					"byte-identical 3554", tc.mapID, tc.name, len(seen))
			}
		})
	}
}

// THE SWING REPEAT ACROSS A STANCE CHANGE (R94, corrected).
//
// THE REFERENCE'S OWN QUALIFIER, and the whole reason this file grew four tests:
//
//	IMC_DECLARE_STRINGID(ATTACKER);
//	int actionState = GetActionState();
//	if (AS_ATTACK == actionState && ATTACKER != m_stance->GetAI())
//		SetActionState(AS_NONE);              // PlayerChar_Request.cpp:1364-1368
//
// The attack state ends only when the NEW stance's AI is not the attacker's. This
// build dropped that clause and ended the repeat on EVERY change, so the fight died
// at the one moment the client asks again - its single follow-up CZ_ATTACK lands
// inside the cadence and is held, and nothing resumes. The owner's words: "changing
// stance sometimes drop the attack".
//
// THE FOUR TESTS below are one per rule, each written against REAL rows of
// datatable_stance.xml so no id is made up:
//
//	1  attacker -> attacker (10510 BlandirCruz -> 10550 TronadaCruz, job 1 + Polearm):
//	   the engagement SURVIVES and the next swing goes out at the NEW cadence
//	2  attacker -> non-attacker (job 3 Scout + Dagger, 10660 Arnis -> 30160
//	   EnhancedTactics, AI HEALER): it ends, ONCE, and the line says which branch
//	3  the accessor answers what the loader read: 10510/10550 are AI 1, 30110 is AI 2
//	4  a stance the table does not know is NOT the attacker AI, and the line says so

// changeStanceHits counts the hit packets of one swing in EITHER shape this build
// sends - the Single 3564 and the Splash 3566 - and fails when the count is not
// matched by the same number of 3557s. Test 1 needs it because both of its stances
// are AtkType="Splash" in the shipped table (10510 Splash/Circle/200, 10550 the
// same), so a 3564-only counter would read the new stance's first swing as zero.
func changeStanceHits(t *testing.T, out [][]byte) int {
	t.Helper()
	hits, hps := 0, 0
	for _, p := range out {
		if len(p) < 2 {
			continue
		}
		switch binary.LittleEndian.Uint16(p) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			hits++
		case CmdZCAddHP:
			hps++
		}
	}
	if hits != hps {
		t.Fatalf("%d hit packet(s) and %d HP update(s) in one reply - they go out in "+
			"pairs (%v)", hits, hps, kinds(out))
	}
	return hits
}

// polearmFighterZone is attackZone(t) moved into a JOB 1 FIGHTER holding a POLEARM,
// standing in `stance`. Polearm is the hands pair that reaches 10510 and 10550
// (datatable_stancecondition.xml condition 108, RHand="Polearm", LHand="None"), so
// both stances of test 1 are ones this character could really have chosen - the
// change would be REFUSED otherwise and the test would measure nothing.
//
// 501 is "Javelin", ClassType "Polearm", not two-handed, so the left hand reads as
// empty and condition 108 is the one that applies.
func polearmFighterZone(t *testing.T, stance uint32) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZone(t)
	p.sess.Team[0].Job = 1
	p.sess.Team[0].Equip[4] = 0   // no shield: condition 108 wants LHand "None"
	p.sess.Team[0].Equip[5] = 501 // Javelin, "Polearm" (datatable_item_weapon.xml)
	p.sess.Team[0].Stance = stance
	return h, logs, p
}

// TestChangingBetweenAttackerStancesKeepsTheFight is rule 1, and it is the owner's
// report in a test: BOTH stances are attacker AI, so the reference never leaves the
// attack state and neither may this build.
//
// TODAY IT FAILS HERE, at the first assertion after the change: changeStance ends
// the repeat, swingOf answers nil, and the log says
//
//	ATTACK REPEAT ENDED: commander ... stops swinging at target ... - the stance
//	changed from 10510 ("BlandirCruz") to 10550 ("TronadaCruz") - a stance change
//	leaves the attack state (PlayerChar_Request.cpp:1364-1368)
//
// which is the clause the reference does NOT have.
func TestChangingBetweenAttackerStancesKeepsTheFight(t *testing.T) {
	const from, to = uint32(10510), uint32(10550) // BlandirCruz -> TronadaCruz
	h, logs, p := polearmFighterZone(t, from)

	// THE FIXTURE'S OWN FACTS, checked before anything is asserted about them: two
	// attacker rows, and two different cadences (1700 vs 2233 ASPD), because a test
	// whose stances swung at the same speed could not tell the new cadence from the
	// old one.
	if ai := h.Stances.AI(int(from)); ai != AttackerAI {
		t.Fatalf("stance %d is AI %d in datatable_stance.xml, want the attacker %d - "+
			"this test is not about two attacker stances", from, ai, AttackerAI)
	}
	if ai := h.Stances.AI(int(to)); ai != AttackerAI {
		t.Fatalf("stance %d is AI %d in datatable_stance.xml, want the attacker %d - "+
			"this test is not about two attacker stances", to, ai, AttackerAI)
	}
	oldCadence := attackInterval(h.sheetFor(p.sess.Team[0]))

	if got := changeStanceHits(t, pressAttack(t, h, p.sess, capturedTarget)); got != 1 {
		t.Fatalf("the press in stance %d did not land: %d hit(s)\n%s", from, got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the press armed no repeat, so there is nothing for the stance change to "+
			"keep alive:\n%s", logs.String())
	}

	// THE CHANGE, through the dispatch: the client's own 15-byte 3625.
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: changeStanceFrame(0, to)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the change to stance %d was answered with nothing, so it was REFUSED - "+
			"the fixture is wrong, not the rule:\n%s", to, logs.String())
	}

	if p.sess.swingOf(0) == nil {
		t.Fatalf("the change from attacker stance %d to attacker stance %d ENDED the "+
			"engagement. The reference leaves AS_ATTACK alone when the new stance's AI IS "+
			"the attacker's (PlayerChar_Request.cpp:1367), and this is the owner's "+
			"\"changing stance sometimes drop the attack\": %q",
			from, to, attackRepeatLog(logs.String()))
	}
	if line := lineWith(logs.String(), "CHANGE_STANCE commander"); !strings.Contains(line,
		"the swing repeat continues (attacker AI)") {
		t.Fatalf("the change's own line does not say which branch it took - a reader must "+
			"not have to guess whether the fight continued:\n%s", line)
	}

	// THE NEXT SWING, at the NEW cadence. swingDue moves the clock by one attack delay
	// of the stance the character stands in NOW (10550), which is the whole point: the
	// tick re-reads the stance's own row every pass, so the new ASPD applies with no
	// restart.
	swingDue(t, h, p.sess, 0)
	if got := changeStanceHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing after the change produced %d hit(s), want 1 - the engagement "+
			"survived but did not swing:\n%s", got, logs.String())
	}
	newCadence := attackInterval(h.sheetFor(p.sess.Team[0]))
	if newCadence == oldCadence {
		t.Fatalf("both stances swing at %s, so this test cannot tell the new cadence from "+
			"the old one", newCadence)
	}
	// The swing's own line names the cadence it went out at ("cadence: the character's
	// own ASPD <duration>"), and it must be the NEW stance's - not the press's 1.666s.
	if text := logs.String(); !strings.Contains(text, "ASPD "+newCadence.String()) {
		t.Fatalf("the swing after the change did not go out at stance %d's own cadence %s:\n%s",
			to, newCadence, lineWith(text, "ATTACK REPEAT: commander"))
	}
	if text := logs.String(); strings.Contains(text, "ATTACK REPEAT ENDED") {
		t.Fatalf("an attacker->attacker change still ended the repeat somewhere:\n%s",
			attackRepeatLog(text))
	}
}

// scoutDaggerZone is attackZone(t) moved to a JOB 3 SCOUT holding a DAGGER, standing
// `gap` clear units from the Scarecrow. Condition 301 is the hands pair that reaches
// 10630, 10660 AND the HEALER stance 30160, which is what makes test 2 a REAL change
// this character could have asked for.
//
// 601 is "KidneyDagger", ClassType "Dagger". The gap is 50 because both stances used
// here have MaxR 100, and 50 sits inside the band with no float edge to argue about.
func scoutDaggerZone(t *testing.T, stance uint32, gap float64) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZone(t)
	p.sess.Team[0].Job = 3
	p.sess.Team[0].Equip[4] = 0   // condition 301 wants LHand "None"
	p.sess.Team[0].Equip[5] = 601 // KidneyDagger, "Dagger"
	p.sess.Team[0].Stance = stance
	standAtGap(t, h, p, gap)
	return h, logs, p
}

// TestChangingToANonAttackerStanceEndsTheFightOnce is rule 2, the other half of the
// reference's clause: a stance whose AI is NOT the attacker's DOES end the attack
// state, and this build must keep doing that - once, with the reason on the line.
//
// The pair is real: 10660 "Arnis" is AI ATTACKER (MinR 1 .. MaxR 100), 30160
// "EnhancedTactics" is AI HEALER, and both are offered by condition 301 (Scout,
// Dagger in the right hand, nothing in the left).
func TestChangingToANonAttackerStanceEndsTheFightOnce(t *testing.T) {
	const from, to = uint32(10660), uint32(30160)
	h, logs, p := scoutDaggerZone(t, from, 50)

	if ai := h.Stances.AI(int(from)); ai != AttackerAI {
		t.Fatalf("stance %d is AI %d, want the attacker %d - the fight has to start in an "+
			"attacker stance", from, ai, AttackerAI)
	}
	if ai := h.Stances.AI(int(to)); ai == AttackerAI {
		t.Fatalf("stance %d is AI %d in datatable_stance.xml, want anything but the "+
			"attacker %d - this test is about the ending branch", to, ai, AttackerAI)
	}

	if got := changeStanceHits(t, pressAttack(t, h, p.sess, capturedTarget)); got != 1 {
		t.Fatalf("the press in stance %d did not land: %d hit(s)\n%s", from, got,
			logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the press armed no repeat, so there is nothing for the change to end:\n%s",
			logs.String())
	}

	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: changeStanceFrame(0, to)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the change to stance %d was refused - the fixture is wrong, not the rule:\n%s",
			to, logs.String())
	}

	if s := p.sess.swingOf(0); s != nil {
		t.Fatalf("the change to the non-attacker stance %d (%q, AI %d) KEPT the "+
			"engagement - the reference drops AS_ATTACK exactly here "+
			"(PlayerChar_Request.cpp:1367)", to, h.Stances.StanceName(to),
			h.Stances.AI(int(to)))
	}
	ended := lineWith(logs.String(), "ATTACK REPEAT ENDED")
	if !strings.Contains(ended, "not the attacker AI") {
		t.Fatalf("the ending does not say WHY - the line must name the new stance's AI:\n%s",
			ended)
	}
	if !strings.Contains(ended, "AI "+itoa(h.Stances.AI(int(to)))) {
		t.Fatalf("the ending does not quote stance %d's own AI id %d:\n%s", to,
			h.Stances.AI(int(to)), ended)
	}
	if line := lineWith(logs.String(), "CHANGE_STANCE commander"); !strings.Contains(line,
		"the swing repeat is ended (the new stance is not attacker AI)") {
		t.Fatalf("the change's own line does not say which branch it took:\n%s", line)
	}
	if n := strings.Count(logs.String(), "ATTACK REPEAT ENDED"); n != 1 {
		t.Fatalf("%d ATTACK REPEAT ENDED line(s), want exactly 1 - one change ends one "+
			"engagement once:\n%s", n, logs.String())
	}
}

// TestStanceAIRowIsWhatTheLoaderRead is rule 3: the accessor answers the ID that the
// stance's OWN row names, mapped through datatable_ai.xml, and both files are read
// here so the assertion cannot drift from the data.
//
//	10510 BlandirCruz  AI="ATTACKER" -> datatable_ai ClassID 1
//	10550 TronadaCruz  AI="ATTACKER" -> 1
//	30110 FirstAid     AI="HEALER"   -> 2
//	999999             no row        -> 0, which is NOT the attacker AI
func TestStanceAIRowIsWhatTheLoaderRead(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}

	// WHAT THE FILES SAY, read straight out of them.
	stanceAI := map[int]string{}
	if _, err := eachClass(filepath.Join(stanceDir(), "datatable_stance.xml"),
		func(attrs map[string]string) {
			if id, ok := atoi(attrs["ClassID"]); ok {
				stanceAI[id] = attrs["AI"]
			}
		}); err != nil {
		t.Fatalf("read datatable_stance.xml: %v", err)
	}
	aiByName := map[string]int{}
	if _, err := eachClass(filepath.Join(stanceDir(), "datatable_ai.xml"),
		func(attrs map[string]string) {
			if id, ok := atoi(attrs["ClassID"]); ok {
				aiByName[attrs["ClassName"]] = id
			}
		}); err != nil {
		t.Fatalf("read datatable_ai.xml: %v", err)
	}
	if aiByName["ATTACKER"] != AttackerAI {
		t.Fatalf("datatable_ai.xml gives ATTACKER ClassID %d, and the code's constant is "+
			"%d - one of them moved", aiByName["ATTACKER"], AttackerAI)
	}

	for _, tc := range []struct {
		stance int
		name   string
	}{
		{10510, "BlandirCruz"},
		{10550, "TronadaCruz"},
		{30110, "FirstAid"},
	} {
		rowAI, ok := stanceAI[tc.stance]
		if !ok {
			t.Fatalf("stance %d is not in datatable_stance.xml - the test's own data moved",
				tc.stance)
		}
		want := aiByName[rowAI]
		if got := table.AI(tc.stance); got != want {
			t.Fatalf("stance %d (%q) reads AI %q in datatable_stance.xml, which is ClassID "+
				"%d in datatable_ai.xml, and the table answers %d", tc.stance, tc.name,
				rowAI, want, got)
		}
		if got := table.IsAttackerAI(tc.stance); got != (want == AttackerAI) {
			t.Fatalf("stance %d (%q) AI %d: IsAttackerAI = %v, want %v", tc.stance, tc.name,
				want, got, want == AttackerAI)
		}
	}

	// The two the tests above stand on, said as numbers rather than names.
	if got := table.AI(10510); got != 1 {
		t.Fatalf("BlandirCruz 10510 is AI %d, want 1", got)
	}
	if got := table.AI(10550); got != 1 {
		t.Fatalf("TronadaCruz 10550 is AI %d, want 1", got)
	}
	// A stance the table does not know: 0, and NOT the attacker AI.
	const unknown = 999999
	if got := table.AI(unknown); got != 0 {
		t.Fatalf("a stance with no row answers AI %d, want 0", got)
	}
	if table.IsAttackerAI(unknown) {
		t.Fatalf("stance %d has no row in datatable_stance.xml and was called the attacker "+
			"AI", unknown)
	}
}

// TestAnUnknownStanceIsNotTheAttackerAI is rule 4: a stance the table does not know is
// NOT the attacker AI, so a repeat that finds the character standing in one ends - and
// the line names the AI it could not find.
//
// The path is the tick's, not changeStance's: a stance id this table has no row for
// can never pass the job+hands check in changeStance, so the honest way a character
// ends up in one is a session carrying an id from a table this zone cannot read. That
// is exactly the state the tick has to survive, and it is the state a one-line revert
// of the guard would get wrong in the other direction (0 == 0 is not "attacker").
func TestAnUnknownStanceIsNotTheAttackerAI(t *testing.T) {
	h, logs, p := attackZone(t)
	if h.Stances.IsAttackerAI(1234567) {
		t.Fatal("a stance id with no row was called the attacker AI")
	}

	if got := changeStanceHits(t, pressAttack(t, h, p.sess, capturedTarget)); got != 1 {
		t.Fatalf("the press did not land: %d hit(s)\n%s", got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the press armed no repeat:\n%s", logs.String())
	}

	p.sess.Team[0].Stance = 1234567 // an id no row of datatable_stance.xml carries
	if out := h.swingTick(p.sess); changeStanceHits(t, out) != 0 {
		t.Fatalf("a character standing in an unknown stance still swung (%v)", kinds(out))
	}
	if p.sess.swingOf(0) != nil {
		t.Fatal("the repeat lived on while the character stood in a stance with no row")
	}
	ended := lineWith(logs.String(), "ATTACK REPEAT ENDED")
	if !strings.Contains(ended, "not the attacker AI") {
		t.Fatalf("the ending does not say the unknown stance is not the attacker AI:\n%s",
			ended)
	}
	if !strings.Contains(ended, "AI 0") {
		t.Fatalf("the ending does not say the AI id it read (0 for no row):\n%s", ended)
	}
}
