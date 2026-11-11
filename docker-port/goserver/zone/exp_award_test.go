package zone

import (
	"bufio"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"
)

// EXPERIENCE ON A KILL (R98 slice 2): FOUR THINGS MUST HOLD.
//
// THE SPEC IS `history\monster-death.md` §3. The per-level number is the game's own
// `Xp` idspace column `MonXp` (`MonsterLevelTable.cpp:40-66`), read through
// `geMonsterLevelTable::GetExp`, which CLAMPS the level to MAX_LV = 100
// (`MonsterLevelTable.cpp:79-83`, `geShared.h:174`). So a level-130 monster pays the
// LEVEL-100 row - the two rows differ in our table, and every test below compares
// against the emitted CSV rather than a number typed here.
//
// THE FIXTURE IS deadZone's: a Scarecrow with ONE hit left, and two sessions in the
// roster. The four tests cover the four claims in the task: the table's value reaches
// every living member; a dead member gets nothing; two kills pay twice and the tick
// pays once; and an absent table pays 0 and says so once, without crashing.

// monXpFromCSV reads ONE level out of the EMBEDDED `zone\data\monster_exp.csv`, without
// going through the code under test. That is what makes the assertions below "the
// emitted data", not a literal: if the table is re-emitted with different numbers, the
// tests still hold, and a code path that invents a value stops matching.
func monXpFromCSV(t *testing.T, level int) int64 {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(monsterExpCSV))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		lv, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || lv != level {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			t.Fatalf("monster_exp.csv has an unreadable value for Lv %d: %q", level, parts[1])
		}
		return v
	}
	t.Fatalf("monster_exp.csv has no row for Lv %d - the table the zone pays from is "+
		"not the one the task says it is", level)
	return 0
}

// expTableFor loads the real emitted table into the fixture.
func expTableFor(t *testing.T, h *Handler) {
	t.Helper()
	table, err := LoadMonsterExpTable()
	if err != nil {
		t.Fatalf("LoadMonsterExpTable: %v", err)
	}
	h.MonsterExp = table
}

// expZone is deadZone with the real emitted table loaded and the body at LEVEL 130 -
// the level the task names - so every test below is a statement about one and the same
// kill.
func expZone(t *testing.T) (*Handler, *stringBuf, *player, *player) {
	t.Helper()
	h, logs, p, other := deadZone(t)
	expTableFor(t, h)
	h.spawnedMonster = []Monster{monsterAtLevel(130)}
	return h, logs, p, other
}

// monsterAtLevel is the fixture's Scarecrow with a different level, so a test can ask
// for the level it wants without touching the body's handle or position.
func monsterAtLevel(level int) Monster {
	m := scarecrow()
	m.Level = level
	return m
}

// expPackets returns every ZC_EXP_UP among the packets a socket was written.
func expPackets(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCExpUp {
			out = append(out, p)
		}
	}
	return out
}

// expTotalMust reads the zone's own ledger for one character.
func expTotalMust(t *testing.T, h *Handler, handle uint32) int64 {
	t.Helper()
	total, ok := h.expTotal(handle)
	if !ok {
		t.Fatalf("the zone credited nothing to handle %d - the kill paid nobody", handle)
	}
	return total
}

// expAwardDrain drives the corpse past its ZC_DEAD instant, which is where the award
// happens now (zone/respawn.go deathPacketTick - the reference's BS_DEAD ON_UPDATE, not
// the blow that killed the body).
func expAwardDrain(t *testing.T, h *Handler) {
	t.Helper()
	h.deathPacketTick(time.Now().Add(time.Hour))
}

// pressCleared presses CZ_ATTACK after backdating the attack clock, so the ASPD
// throttle cannot hold the swing. It is the same trick swingDue uses, and it exists so
// a test can land a SECOND kill without sleeping.
func pressCleared(t *testing.T, h *Handler, sess *Session, target uint32) [][]byte {
	t.Helper()
	if interval := attackInterval(h.sheetFor(sess.Team[0])); interval > 0 {
		sess.attackAt[0] = sess.lastAttack(0).Add(-interval)
	}
	return pressAttack(t, h, sess, target)
}

// TestKillAwardsTheGamesPerLevelValue is claim 1: a level-130 kill pays the table's own
// number to EACH living member, and that number is the LEVEL-100 row.
//
// It would fail if the code invented a value, if it used the raw level (the Lv-130 row
// is a different number, asserted below), if it paid only the killer, and if the packet
// carried the gain instead of the character's total.
func TestKillAwardsTheGamesPerLevelValue(t *testing.T) {
	h, logs, p, other := expZone(t)

	want := monXpFromCSV(t, MonsterExpMaxLevel)
	raw130 := monXpFromCSV(t, 130)
	if want == raw130 {
		t.Fatalf("the emitted table gives Lv %d and Lv 130 the same value (%d), so this "+
			"test cannot show WHICH row the clamp picks", MonsterExpMaxLevel, want)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit, so nothing was killed:\n%s", logs.String())
	}
	if hp := h.monsterHPFor(capturedTarget); hp != 0 {
		t.Fatalf("the press left the monster at %v HP, want 0 - this test needs a KILL:\n%s",
			hp, logs.String())
	}
	expAwardDrain(t, h)

	// EVERY LIVING MEMBER, not only the one who swung.
	for _, c := range []Character{p.sess.Team[0], other.sess.Team[0]} {
		if got := expTotalMust(t, h, c.Handle); got != want {
			t.Fatalf("handle %d was paid %d, want %d - the table's Lv %d row "+
				"(a kill at level 130 uses the clamped row, not the Lv-130 one: %d):\n%s",
				c.Handle, got, want, MonsterExpMaxLevel, raw130, logs.String())
		}
	}

	// THE PACKET: 23 bytes, cmd 3629, this character's slot, its new TOTAL, its stance.
	pkts := expPackets(p.conn.sent())
	if len(pkts) != 1 {
		t.Fatalf("the caster's own client got %d ZC_EXP_UP(s), want 1 - the exp bar "+
			"only moves for the session that is told:\n%s", len(pkts), logs.String())
	}
	pkt := pkts[0]
	if len(pkt) != ExpUpSize {
		t.Fatalf("ZC_EXP_UP is %d bytes, want %d (cmd 2 + charIndex 1 + exp 8 + "+
			"stanceType 4 + stanceExp 4 + autoRule 4)", len(pkt), ExpUpSize)
	}
	if got := binary.LittleEndian.Uint16(pkt); got != CmdZCExpUp {
		t.Fatalf("the exp packet's command is %d, want %d", got, CmdZCExpUp)
	}
	if got := pkt[euCharIndex]; got != 0 {
		t.Fatalf("charIndex is %d, want 0 - the leader's own team slot", got)
	}
	if got := int64(binary.LittleEndian.Uint64(pkt[euExp:])); got != want {
		t.Fatalf("+%d exp = %d, want the character's TOTAL %d (the client puts the "+
			"total on the bar, TxExpUp.cpp:231)", euExp, got, want)
	}
	if got := binary.LittleEndian.Uint32(pkt[euStanceType:]); got != p.sess.Team[0].Stance {
		t.Fatalf("+%d stanceType = %d, want the character's stance %d",
			euStanceType, got, p.sess.Team[0].Stance)
	}
	if got := binary.LittleEndian.Uint32(pkt[euStanceExp:]); got != 0 {
		t.Errorf("+%d stanceExp = %d, want 0 - this zone sends every stance at "+
			"level 1 / 0 exp already (zone\\stance.go)", euStanceExp, got)
	}
	if got := binary.LittleEndian.Uint32(pkt[euAutoRule:]); got != 0 {
		t.Errorf("+%d autoRule = %d, want 0 - this zone has no auto-inspect rule",
			euAutoRule, got)
	}

	// THE ONE LINE PER KILL: monster, level, per-level value, factor, share, packet.
	text := logs.String()
	for _, piece := range []string{
		"lv 130 (table lv 100)",
		"per-level " + strconv.FormatInt(want, 10) + " x factor 1.0",
		"WHOLE to each",
		"cmd 3629 ZC_EXP_UP (23 B)",
	} {
		if !strings.Contains(text, piece) {
			t.Errorf("the kill line does not carry %q:\n%s", piece, text)
		}
	}
}

// TestADeadMemberReceivesNothing is claim 2: a teammate with no HP left has no share
// and is not told. The living one still is, so the test cannot pass by paying nobody.
func TestADeadMemberReceivesNothing(t *testing.T) {
	h, logs, p, other := expZone(t)

	// The observer's only character is DEAD. The roster copies a team when it is added,
	// so the session is added again - otherwise the ledger walk reads the old HP.
	other.sess.Team[0].HP = 0
	h.Players.Add(other.sess)

	want := monXpFromCSV(t, MonsterExpMaxLevel)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit, so nothing was killed:\n%s", logs.String())
	}
	expAwardDrain(t, h)

	if got := expTotalMust(t, h, p.sess.Team[0].Handle); got != want {
		t.Fatalf("the living character was paid %d, want %d - the dead one's refusal "+
			"must not starve the rest:\n%s", got, want, logs.String())
	}
	if total, ok := h.expTotal(other.sess.Team[0].Handle); ok {
		t.Fatalf("the DEAD character was credited %d exp, want nothing:\n%s",
			total, logs.String())
	}
	if pkts := expPackets(other.conn.sent()); len(pkts) != 0 {
		t.Fatalf("a dead character was sent %d ZC_EXP_UP(s), want 0:\n%s",
			len(pkts), logs.String())
	}
	if !strings.Contains(logs.String(), "1 living character(s)") {
		t.Errorf("the kill line does not count the ONE living character:\n%s", logs.String())
	}
}

// TestTwoKillsAwardTwiceAndOneKillAwardsOnce is claims 3 and 4 of the task's list: a
// SECOND kill pays again, and one kill is paid EXACTLY once - the award happens on the
// corpse's ZC_DEAD pass (2026-11-09, zone/respawn.go deathPacketTick; it used to happen
// inside the killing hit), so that pass must pay once and only once.
func TestTwoKillsAwardTwiceAndOneKillAwardsOnce(t *testing.T) {
	h, logs, p, other := expZone(t)

	want := monXpFromCSV(t, MonsterExpMaxLevel)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the first press produced no hit:\n%s", logs.String())
	}
	// THE KILL ITSELF PAYS NOTHING: the body is dead and its packet is still owed.
	if total, ok := h.expTotal(p.sess.Team[0].Handle); ok {
		t.Fatalf("the killing hit paid %d exp on its own, want nothing - the award "+
			"belongs to the ZC_DEAD pass:\n%s", total, logs.String())
	}
	expAwardDrain(t, h)
	if got := expTotalMust(t, h, p.sess.Team[0].Handle); got != want {
		t.Fatalf("after ONE kill handle %d has %d, want %d:\n%s",
			p.sess.Team[0].Handle, got, want, logs.String())
	}

	// THE DEATH PACKET'S PASS PAYS ONCE AND PAYS NOTHING EXTRA: a second pass over the
	// same corpse must not pay again.
	if got := len(deadPackets(p.conn.sent())); got != 1 {
		t.Fatalf("the death pass sent %d ZC_DEAD(s), want 1:\n%s", got, logs.String())
	}
	expAwardDrain(t, h)
	if got := expTotalMust(t, h, p.sess.Team[0].Handle); got != want {
		t.Fatalf("a second death pass left the exp at %d, want it at %d - one kill "+
			"is paid once:\n%s", got, want, logs.String())
	}
	if got := len(expPackets(p.conn.sent())); got != 1 {
		t.Fatalf("one kill sent %d ZC_EXP_UP(s) to the caster, want 1:\n%s",
			got, logs.String())
	}

	// A SECOND KILL: the body is refilled and hit again, which arms a NEW death (the
	// first one's packet has been taken by the pass above).
	h.spawnMu.Lock()
	h.monsterHP[capturedTarget] = 1
	h.spawnMu.Unlock()
	if out := pressCleared(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the second press produced no hit:\n%s", logs.String())
	}
	expAwardDrain(t, h)
	if got := expTotalMust(t, h, p.sess.Team[0].Handle); got != 2*want {
		t.Fatalf("after TWO kills handle %d has %d, want %d (twice %d):\n%s",
			p.sess.Team[0].Handle, got, 2*want, want, logs.String())
	}
	if got := len(expPackets(p.conn.sent())); got != 2 {
		t.Fatalf("two kills sent %d ZC_EXP_UP(s) to the caster, want 2:\n%s",
			got, logs.String())
	}
	// And the second packet carries the DOUBLED total, which is what the bar shows.
	pkts := expPackets(other.conn.sent())
	if len(pkts) != 2 {
		t.Fatalf("the observer got %d ZC_EXP_UP(s) for two kills, want 2:\n%s",
			len(pkts), logs.String())
	}
	if got := int64(binary.LittleEndian.Uint64(pkts[1][euExp:])); got != 2*want {
		t.Fatalf("the second packet's exp is %d, want the running total %d",
			got, 2*want)
	}
}

// TestAKillWithNoExpTablePaysZeroOnce is the honesty case: a zone without the table
// awards NOTHING (no invented number), sends no ZC_EXP_UP, does not crash - and says
// why ONCE, not once per kill.
func TestAKillWithNoExpTablePaysZeroOnce(t *testing.T) {
	h, logs, p, other := deadZone(t)
	// h.MonsterExp is left NIL on purpose: that is "the table is absent".
	if h.MonsterExp != nil {
		t.Fatalf("the fixture already has an exp table, so the absent case cannot be tested")
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press produced no hit, so nothing was killed:\n%s", logs.String())
	}
	expAwardDrain(t, h)
	if got := len(deadPackets(p.conn.sent())); got != 1 {
		t.Fatalf("the death pass sent %d ZC_DEAD(s), want 1:\n%s", got, logs.String())
	}

	// THE SECOND KILL, so "once" is measured over two of them.
	h.spawnMu.Lock()
	h.monsterHP[capturedTarget] = 1
	h.spawnMu.Unlock()
	if out := pressCleared(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the second press produced no hit:\n%s", logs.String())
	}
	expAwardDrain(t, h)

	// NOTHING WAS PAID, to anybody, and no packet went out.
	if total, ok := h.expTotal(p.sess.Team[0].Handle); ok {
		t.Fatalf("a zone with no exp table credited %d exp, want nothing", total)
	}
	if total, ok := h.expTotal(other.sess.Team[0].Handle); ok {
		t.Fatalf("a zone with no exp table credited the observer %d exp, want nothing", total)
	}
	if pkts := expPackets(p.conn.sent()); len(pkts) != 0 {
		t.Fatalf("%d ZC_EXP_UP(s) went out with no exp table, want 0:\n%s",
			len(pkts), logs.String())
	}
	if pkts := expPackets(other.conn.sent()); len(pkts) != 0 {
		t.Fatalf("%d ZC_EXP_UP(s) reached the observer with no exp table, want 0:\n%s",
			len(pkts), logs.String())
	}

	// AND THE MISSING TABLE IS NAMED ONCE, with the two kills both logged as 0.
	text := logs.String()
	if n := strings.Count(text, "no monster exp table"); n != 1 {
		t.Fatalf("the missing table was named %d time(s), want exactly 1:\n%s", n, text)
	}
	if n := strings.Count(text, "nothing is awarded and no cmd 3629 goes out"); n != 2 {
		t.Fatalf("%d kill line(s) say they paid nothing, want 2 (one per kill):\n%s", n, text)
	}
}
