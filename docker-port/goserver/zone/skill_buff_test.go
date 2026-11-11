package zone

// THE SKILL'S BUFF LANDS - the chain research\item-buff-skill.md §3 named, driven end
// to end through the GAME'S OWN SCRIPTS:
//
//	the skill row's Script column -> SCR_SKILL_BUFF -> GetMagicalSkillArg + SendMsg
//	-> the target's SCR_MSG_<TYPE>_SKILL_MAGICAL_ASSIST -> SCR_RESULT_BATTLEBUFF_SKILL
//	-> AddBuff -> the zone's buff list -> 3556 (and 3125 for a player character)
//
// Every test here uses the SHIPPED data: `datatable_skill.xml` row 1002 (Coruscation,
// FORCE/100 - the roll is guaranteed) and row 1004 (Concentrate, AtkLayer 1) against
// real monster rows. Nothing is a hand-built mirror of the rule: the chance, the rank
// check and the skip are all the corpus's own, which is why these tests can fail.

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
)

// skillBuffZone is the fixture: the real Buff and Skill tables, the real corpus on
// the package's one host, and one player character already in the zone's roster (the
// message dispatch resolves a PC target through it).
func skillBuffZone(t *testing.T) (*Handler, *player, *bytes.Buffer) {
	t.Helper()
	requireStatus(t)
	buffs, err := LoadBuffs(XMLDir())
	if err != nil {
		t.Skipf("the Buff table is not readable (%v) - a test must not pass on zeros", err)
	}
	skills, err := LoadSkills(XMLDir())
	if err != nil {
		t.Skipf("the Skill table is not readable (%v)", err)
	}
	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)
	h := &Handler{Log: logger, Buffs: buffs, Skills: skills, Status: defaultStatusForTests}
	bs, err := NewBuffScript(testScriptHost, logger.Printf)
	if err != nil {
		t.Fatalf("arming the buff corpus failed: %v", err)
	}
	bs.Sheet = defaultStatusForTests
	h.BuffScript = bs
	ds, derr := NewMonsterDefScript(testScriptHost, logger.Printf)
	if derr != nil {
		t.Fatalf("arming the monster defence corpus failed: %v", derr)
	}
	h.Defence = ds

	p := newPlayer(1792, "Grace", [3]int32{0, 0, 0})
	h.Players.Add(p.sess)
	return h, p, logs
}

// skillBuffRow is one real row of the skill table, failed loudly when it is gone.
func skillBuffRow(t *testing.T, h *Handler, id uint32) SkillRow {
	t.Helper()
	row, ok := h.Skills.Row(id)
	if !ok {
		t.Fatalf("skill %d is not a row of datatable_skill.xml - the row this test "+
			"drives is gone, so its Script/Buff columns are gone with it", id)
	}
	if row.Script != SkillBuffEntry {
		t.Fatalf("skill %d's Script column reads %q, want %q - this test drives a BUFF row",
			id, row.Script, SkillBuffEntry)
	}
	return row
}

// monsterBodyForTest builds the body a spawn of that class produces, including the
// three computed properties a spawn caches (`MonsterDefScript.Ranks`).
func monsterBodyForTest(t *testing.T, h *Handler, class, handle uint32) Monster {
	t.Helper()
	if h.Monsters == nil {
		monsters, err := LoadMonsters(XMLDir())
		if err != nil {
			t.Skipf("monster table not readable (%v)", err)
		}
		h.Monsters = monsters
	}
	row, ok := h.Monsters.Row(class)
	if !ok {
		t.Fatalf("class %d is not a row of the Monster table", class)
	}
	m := Monster{
		Handle: handle, X: 100, Y: 100, Cell: 1,
		ClassID: class, Level: int(row.Level), HP: int32(row.MHP), MaxHP: float64(row.MHP),
		RaceType: row.RaceType, Arm: row.Arm, DEFBM: row.DEFBM, DEFPERBM: row.DEFPERBM,
		IMPBM: row.IMPBM, SizeType: row.SizeType,
		MobRank: row.MobRank, DRBM: row.DRBM, DefLayer: row.DefLayer,
		DefLayerBM: row.DefLayerBM, RSTAT: row.RSTAT, RSTATBM: row.RSTATBM,
	}
	ranks, ok2, err := h.Defence.Ranks(m)
	if !ok2 {
		t.Fatalf("SCR_Get_MON_DR / DefLayer / RSTAT could not be read for class %d: %v",
			class, err)
	}
	m.MONDR = ranks["MON_DR"]
	m.MONDefLayer = ranks["MON_DefLayer"]
	m.MONRSTAT = ranks["MON_RSTAT"]
	return m
}

// changeBuffOf answers the 3556 in a packet batch, or nil.
func changeBuffOf(out [][]byte) []byte {
	for _, pkt := range out {
		if len(pkt) >= ChangeBuffSize && binary.LittleEndian.Uint16(pkt[0:]) == CmdZCChangeBuff {
			return pkt
		}
	}
	return nil
}

// maskHasBit is the 3556's mask test - the bit's own byte and shift, not "a packet".
func maskHasBit(pkt []byte, classID uint32) bool {
	if len(pkt) < ChangeBuffSize {
		return false
	}
	return pkt[6+(classID>>3)]&(1<<(classID&7)) != 0
}

// TestAMagicalSkillBuffLandsOnACharacter is pass condition 2 on the PC half: a REAL
// `SCR_SKILL_BUFF` row is run end to end, the message script runs, and the buff is on
// the character's list with its 3556 bit set and the duration its ENTER wrote.
//
// Coruscation (row 1002) is FORCE/100, so the rule's roll is guaranteed
// (`buffsucc * 100 >= IMCRandom(1,10000)` with buffsucc 100) and the test is not a
// coin toss. Its buff (class 63) is NORMAL with ApplyTime 0 in the table and
// `buff.ApplyTime = 180000` in its ENTER (ge\script\buff\buff.scp:13600), so the
// duration proves the ENTER ran.
func TestAMagicalSkillBuffLandsOnACharacter(t *testing.T) {
	h, p, logs := skillBuffZone(t)
	c := p.sess.Team[0]
	row := skillBuffRow(t, h, 1002)

	out := h.skillScriptBuff(p.sess, c, row, characterBuffTarget(c), false, 1)

	l := p.sess.liveBuffs(0)
	if l == nil || !l.IsBuffApplied("Coruscation") {
		t.Fatalf("the buff did not land; the list is %v\nlog:\n%s", l.Names(), logs.String())
	}
	b := l.buffs[0]
	if b.ClassID != 63 {
		t.Fatalf("the applied buff is class %d, want 63 (datatable_buff.xml, Coruscation)",
			b.ClassID)
	}
	if b.ApplyTime != 180000 {
		t.Fatalf("the buff's ApplyTime is %g ms, want 180000 - the number its own ENTER "+
			"wrote (buff.scp:13600); a different number means the ENTER did not run",
			b.ApplyTime)
	}

	pkt := changeBuffOf(out)
	if pkt == nil {
		t.Fatalf("no 3556 in the cast's %d packet(s); log:\n%s", len(out), logs.String())
	}
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != c.Handle {
		t.Fatalf("the 3556 carries handle %d, want the character's %d", got, c.Handle)
	}
	if !maskHasBit(pkt, 63) {
		t.Fatalf("the 3556 mask does not carry Coruscation's bit (class 63 = mask byte %d "+
			"bit %d)", 63>>3, 63&7)
	}
	// AND THE MESSAGE SCRIPT'S OWN WRITE came back through `ret`: the PC handler sets
	// `ret.BuffSucc = 1` when the rule answered 1 (common_pc.scp:5797) on the new-PVP
	// branch, and 0 on the normal one - either way the table exists.
	if !strings.Contains(logs.String(), "Buff \"Coruscation\"") {
		t.Fatalf("the log does not name the buff the bag carried:\n%s", logs.String())
	}
}

// TestAMagicalSkillBuffLandsOnAMonster is pass condition 2 on the half the owner will
// test on his screen: a debuff cast at a MONSTER lands on the monster's own list, and
// the 3556 names the MONSTER's handle - which is the packet that draws its bar.
//
// The monster is a real row (11600, the Scarecrow): DefLayer 1, so the rule's
// `arg.AtkLayer < self.MON_DefLayer` gate (2 < 1) lets Coruscation through.
func TestAMagicalSkillBuffLandsOnAMonster(t *testing.T) {
	h, p, logs := skillBuffZone(t)
	c := p.sess.Team[0]
	row := skillBuffRow(t, h, 1002)
	mon := monsterBodyForTest(t, h, 11600, 0x20001234)
	h.spawnedMonster = []Monster{mon}

	out := h.skillScriptBuff(p.sess, c, row, monsterBuffTarget(h, &mon), false, 1)

	l := h.liveMonsterBuffs(mon.Handle)
	if l == nil || !l.IsBuffApplied("Coruscation") {
		t.Fatalf("the buff did not land on monster %d; log:\n%s", mon.Handle, logs.String())
	}
	// THE PACKET REACHES THE PLAYER'S OWN SOCKET (a monster has no session, so its bar
	// is broadcast), exactly once.
	sent := p.conn.sent()
	var pkt []byte
	changes := 0
	for _, frame := range sent {
		if len(frame) >= ChangeBuffSize && binary.LittleEndian.Uint16(frame[0:]) == CmdZCChangeBuff {
			changes++
			pkt = frame
		}
	}
	if changes != 1 {
		t.Fatalf("the caster's socket got %d 3556 packet(s) for a monster buff, want 1 "+
			"(broadcast once: %d frame(s) in all)\nlog:\n%s", changes, len(sent), logs.String())
	}
	if changeBuffOf(out) != nil {
		t.Fatal("the reply batch carries the monster's 3556 as well as broadcasting it - " +
			"the same client would receive the bar twice")
	}
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != mon.Handle {
		t.Fatalf("the 3556 carries handle %d, want the monster's %d - a player's handle "+
			"would draw the icon on the wrong bar", got, mon.Handle)
	}
	if !maskHasBit(pkt, 63) {
		t.Fatal("the 3556 mask does not carry Coruscation's bit (class 63)")
	}
	// THE MONSTER'S OWN RULE read real numbers: MON_DefLayer came from the game's own
	// SCR_Get_MON_DefLayer, so it is 1 and the gate above was a real comparison.
	if mon.MONDefLayer != 1 {
		t.Fatalf("the body's MON_DefLayer is %g, want 1 (SCR_Get_MON_DefLayer for class "+
			"11600, whose row states DefLayer 1)", mon.MONDefLayer)
	}
}

// TestARefusedMagicalSkillBuffAddsNothingAndSaysWhy is pass condition 3: the same
// chain, with a row the SCRIPT'S OWN rule rejects - Concentrate (1004) is AtkLayer 1
// against a DefLayer-2 monster, and `SCR_MSG_MON_SKILL_MAGICAL_ASSIST` returns before
// the buff rule on `arg.AtkLayer < self.MON_DefLayer` (common_monster.scp:8481).
//
// The test asserts the two halves that make a refusal usable: NO buff on the list,
// and exactly ONE line naming the skill, the buff and the reason.
func TestARefusedMagicalSkillBuffAddsNothingAndSaysWhy(t *testing.T) {
	h, p, logs := skillBuffZone(t)
	c := p.sess.Team[0]
	row := skillBuffRow(t, h, 1004) // Concentrate: Buff Concentrate, FORCE, AtkLayer 1
	mon := monsterBodyForTest(t, h, 10071, 0x20005678) // Monster_Hornet: DefLayer 2
	h.spawnedMonster = []Monster{mon}

	out := h.skillScriptBuff(p.sess, c, row, monsterBuffTarget(h, &mon), false, 1)

	if l := h.liveMonsterBuffs(mon.Handle); l != nil && l.IsBuffApplied("Concentrate") {
		t.Fatalf("the rule refused the buff (AtkLayer 1 < MON_DefLayer 2) and it landed "+
			"anyway: %v", l.Names())
	}
	if pkt := changeBuffOf(out); pkt != nil {
		t.Fatalf("a refused buff still put a 3556 in the reply (%d bytes)", len(pkt))
	}
	for _, frame := range p.conn.sent() {
		if len(frame) >= 2 && binary.LittleEndian.Uint16(frame[0:]) == CmdZCChangeBuff {
			t.Fatal("a refused buff still BROADCAST a 3556 to the caster's socket")
		}
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "SKILL BUFF refused") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no 'SKILL BUFF refused' line - a refusal must be visible, not a "+
			"silent no-op:\n%s", logs.String())
	}
	for _, want := range []string{"Concentrate", "\"Concentrate\"", "monster"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the refusal line does not name %q:\n%s", want, line)
		}
	}
	// ONE line, not one per branch or per call: the card says "one log line".
	if n := strings.Count(logs.String(), "SKILL BUFF refused"); n != 1 {
		t.Fatalf("%d refusal line(s), want 1:\n%s", n, logs.String())
	}
}

// TestACastRunsTheRowsOwnBuffScript drives the WHOLE live path the owner uses: one
// CZ_SKILL_TARGET through h.Reply, the skill's own CastTime, and then the result -
// where the row's `Script` column decides that `SCR_SKILL_BUFF` runs instead of the
// damage formula (zone\skill_cast.go, the castLand branch).
//
// The skill is 20154 "Invulnerable": UseType MELEE (so the client targets one body),
// Script SCR_SKILL_BUFF, Buff Invulnerable, BuffType FORCE/100 (the rule's roll is
// guaranteed), and buff class 32 is NORMAL with an ENTER that writes the duration.
func TestACastRunsTheRowsOwnBuffScript(t *testing.T) {
	h, logs := castZone(t)
	requireStatus(t)
	buffs, err := LoadBuffs(XMLDir())
	if err != nil {
		t.Skipf("the Buff table is not readable (%v)", err)
	}
	h.Buffs = buffs
	bs, berr := NewBuffScript(testScriptHost, log.Printf)
	if berr != nil {
		t.Fatalf("arming the buff corpus failed: %v", berr)
	}
	bs.Sheet = defaultStatusForTests
	h.BuffScript = bs
	h.Status = defaultStatusForTests
	if ds, derr := NewMonsterDefScript(testScriptHost, log.Printf); derr == nil {
		h.Defence = ds
	}

	p := castPlayer()
	h.Players.Add(p.sess)
	mon := monsterBodyForTest(t, h, 11600, capturedTarget)
	// WITHIN THE SKILL'S OWN REACH: 20154's row says MaxR 250, and the cast lane refuses
	// (or walks) a further target - the body stands 48 units from the caster.
	mon.X, mon.Y, mon.Cell = -910, 1440, 29
	h.spawnedMonster = []Monster{mon}
	// THE STANCE OFFERS THIS ONE SKILL: the cast lane's permission rule is the
	// stance's own SkillID1..5 list (zone\skill_cast.go), so the test says the caster
	// stands in a stance that grants 20154 instead of inventing a permission bypass.
	if h.Stances == nil {
		t.Skip("no stance table")
	}
	h.Stances.stanceSkills[10560] = []int{20154}

	f := skillTargetFrame(0, 20154, capturedTarget)
	out, rerr := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if rerr != nil {
		t.Fatalf("Reply: %v", rerr)
	}
	if len(out) != 1 || binary.LittleEndian.Uint16(out[0]) != CmdZCSkillCasting {
		t.Fatalf("the accepted cast answered %d packet(s), want the one 3571:\n%s",
			len(out), logs.String())
	}

	// THE RESULT, WHEN THE ROW'S OWN CastTime IS UP.
	castDueNow(t, p.sess, 0)
	fired := h.chaseTick(p.sess)
	if len(fired) == 0 || binary.LittleEndian.Uint16(fired[0]) != CmdZCSkillMelee {
		t.Fatalf("the cast's clock produced %d packet(s), none of them the 3573:\n%s",
			len(fired), logs.String())
	}
	if l := h.liveMonsterBuffs(mon.Handle); l == nil || !l.IsBuffApplied("Invulnerable") {
		t.Fatalf("skill 20154's own buff did not land on the target; log:\n%s", logs.String())
	}
	// AND THE BAR REACHED THE PLAYER (a monster's 3556 is broadcast, not returned).
	found := false
	for _, frame := range p.conn.sent() {
		if len(frame) >= ChangeBuffSize && binary.LittleEndian.Uint16(frame[0:]) == CmdZCChangeBuff &&
			binary.LittleEndian.Uint32(frame[2:]) == mon.Handle && maskHasBit(frame, 32) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no 3556 for the monster with Invulnerable's bit (class 32) reached the "+
			"caster's socket; log:\n%s", logs.String())
	}
}

// TestASkillThatLandsTwiceDoesNotAddTwice is the rule's own "already applied" gate
// (`IsBuffApplied(self, arg.Buff) == 'NO' || arg.BuffType == 'FORCE'`,
// common_pc.scp:6895) seen from the zone: the second cast still runs the script and
// still says what happened, but the list holds ONE entry.
func TestASkillThatLandsTwiceDoesNotAddTwice(t *testing.T) {
	h, p, logs := skillBuffZone(t)
	c := p.sess.Team[0]
	row := skillBuffRow(t, h, 1002)
	mon := monsterBodyForTest(t, h, 11600, 0x20009999)
	h.spawnedMonster = []Monster{mon}

	h.skillScriptBuff(p.sess, c, row, monsterBuffTarget(h, &mon), false, 1)
	h.skillScriptBuff(p.sess, c, row, monsterBuffTarget(h, &mon), false, 1)

	l := h.liveMonsterBuffs(mon.Handle)
	if l == nil || len(l.buffs) != 1 {
		t.Fatalf("the monster holds %d buff(s) after two casts of one FORCE buff, want 1:"+
			"\n%s", len(l.buffs), logs.String())
	}
}
