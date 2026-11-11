package zone

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE PLAIN ATTACK - the live packets held against the builders.
//
// The oracle is the owner's team-combat demo of 2026-10-31, cut into fixtures and
// never retyped (`history\combat-attack-heal.md` §1, §2, §3.4, §6):
//
//	zone\testdata\live_cz_attack.bin       15 B  CZ_ATTACK 3666  (in)
//	zone\testdata\live_attack_single.bin   74 B  ZC_ATTACK_SINGLE 3564 (out)
//	zone\testdata\live_add_hp_party.bin    23 B  ZC_ADD_HP 3557  (out)
//
// The values every test below asserts are the capture's own: charIndex 1, target
// 1602174 (the Scarecrow dummy), attacker 1602611 at (-2271, 1268, cell 50),
// attack speed 471, damage 1328393.0 at +42, and the dummy's HP at +50.
const (
	attackCharIndex      = 1
	attackTarget         = 1602174
	attackAttacker       = 1602611
	attackChecksumInFile = 243
)

// attackFixture reads one captured packet and checks it is the packet it claims to
// be, the same way skillFixture does for the cast: the file is the whole packet, so
// its size and its command word must agree with the client's own table before any
// offset is trusted.
func attackFixture(t *testing.T, name string, cmd uint16, size int) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(b) != size {
		t.Fatalf("%s is %d bytes, want %d", name, len(b), size)
	}
	if got := binary.LittleEndian.Uint16(b); got != cmd {
		t.Fatalf("%s is cmd %d, want %d", name, got, cmd)
	}
	return b
}

// attackFrame builds one CZ_ATTACK (3666), 15 bytes, with a correct checksum - the
// same arithmetic czChecksum reads back, so a test can send a frame the client
// would have sent. The RULE itself is not proved here: test (a) proves it against
// the client's own bytes in the fixture.
func attackFrame(charIndex uint8, target uint32) []byte {
	b := make([]byte, czAttackSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZAttack)
	binary.LittleEndian.PutUint32(b[2:], 0x4a) // seq, as the capture had
	b[caCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[caTarget:], target)
	binary.LittleEndian.PutUint32(b[caChecksum:], czChecksum(b))
	return b
}

// attackDiffAllowed answers why a differing byte offset in 3564 is EXPECTED to
// differ, or "" when the builder must reproduce the capture there. It names the
// fields §2 measured as per-hit data; every other byte - the command, the length
// word and the four startTime bytes - is the builder's job to get identical.
func attackDiffAllowed(off int) string {
	switch {
	case off >= asAttacker && off < asAtkPos:
		return "attacker handle"
	case off >= asAtkPos && off < asAtkSpd:
		return "attacker position"
	case off >= asAtkSpd && off < asStartTime:
		return "attack speed"
	case off >= asTarget && off < asTargetPos:
		return "target handle"
	case off >= asTargetPos && off < asDamage:
		return "target position"
	case off >= asDamage && off < asTargetHP:
		return "damage"
	case off >= asTargetHP && off < asAttackType:
		return "target HP before"
	case off >= asAttackType && off < asKnockPos:
		return "attack type"
	case off >= asKnockPos && off < AttackSingleSize:
		return "knock-back position"
	}
	return ""
}

// TestParseAttackReadsTheLiveFixture is (a): the client's own 15 bytes decode to
// the character that swung and the dummy it clicked, AND the checksum at +6
// recomputes - which is the one field §7 asked a build lane to validate.
func TestParseAttackReadsTheLiveFixture(t *testing.T) {
	f := attackFixture(t, "testdata/live_cz_attack.bin", CmdCZAttack, czAttackSize)

	req, ok := ParseAttack(f)
	if !ok {
		t.Fatalf("the live 15-byte CZ_ATTACK was refused")
	}
	if req.CharIndex != attackCharIndex {
		t.Errorf("+10 charIndex = %d, want %d (the acting character, §1)", req.CharIndex,
			attackCharIndex)
	}
	if req.Target != attackTarget {
		t.Errorf("+11 target = %d, want %d", req.Target, attackTarget)
	}
	if req.Checksum != attackChecksumInFile {
		t.Errorf("+6 checksum = %d, want %d", req.Checksum, attackChecksumInFile)
	}
	if got := czChecksum(f); got != req.Checksum {
		t.Fatalf("czChecksum = %d on the live packet, but the client stored %d - the "+
			"rule in czChecksum is not the client's GetChecksum", got, req.Checksum)
	}
	// A frame of another length is refused rather than read past: 3666 is FIXED.
	if _, ok := ParseAttack(f[:14]); ok {
		t.Error("a 14-byte frame parsed as CZ_ATTACK")
	}
	t.Logf("live 3666: charIndex %d, target %d, checksum %d recomputes exactly",
		req.CharIndex, req.Target, req.Checksum)
}

// TestChecksumRuleHoldsOnEveryCZFixture is why czChecksum may be trusted at all:
// the rule is checked against FOUR of the client's own packets, of four different
// lengths, and the reverse rule is shown to fail. A byte-wide accumulator matches
// the three short ones and fails on 3647 (0x2ac = 684), which is exactly the trap
// this test exists to catch.
func TestChecksumRuleHoldsOnEveryCZFixture(t *testing.T) {
	cases := []struct {
		file string
		cmd  uint16
		size int
	}{
		{"testdata/live_cz_attack.bin", CmdCZAttack, czAttackSize},
		{"testdata/live_cz_change_stance.bin", CmdCZChangeStance, czChangeStanceSize},
		{"testdata/live_cz_skill_target.bin", CmdCZSkillTarget, czSkillTargetSize},
		{"testdata/live_cz_skill_ground.bin", CmdCZSkillGround, CZSkillGroundSize},
	}
	for _, c := range cases {
		f := attackFixture(t, c.file, c.cmd, c.size)
		stated := binary.LittleEndian.Uint32(f[caChecksum:])
		ours := czChecksum(f)

		// The reverse reading (ADD the even bytes, XOR the odd ones) must NOT match,
		// or the rule is ambiguous and one of the four is a coincidence.
		var reverse uint32
		for i, b := range f {
			if i >= caChecksum && i < caChecksum+4 {
				continue
			}
			if i%2 == 0 {
				reverse += uint32(b)
				continue
			}
			reverse ^= uint32(b)
		}
		if ours != stated {
			t.Errorf("%s (cmd %d, %d B): stored checksum %d, recomputed %d",
				c.file, c.cmd, len(f), stated, ours)
		}
		if reverse == stated {
			t.Errorf("%s: the REVERSED rule also matches (%d) - the rule is ambiguous",
				c.file, stated)
		}
		t.Logf("cmd %d (%d B): stored %d, XOR-even/ADD-odd %d, reversed %d", c.cmd, len(f),
			stated, ours, reverse)
	}
}

// TestAttackSingleRebuildsTheLiveCapture is (b): the 3564 builder, given the
// capture's own values, must reproduce all 74 bytes. The allowances are named so a
// reader can see which bytes are per-hit data - and with the fixture's values in,
// NOT ONE of them is allowed to differ.
func TestAttackSingleRebuildsTheLiveCapture(t *testing.T) {
	p := attackFixture(t, "testdata/live_attack_single.bin", CmdZCAttackSingle,
		AttackSingleSize)

	attacker := binary.LittleEndian.Uint32(p[asAttacker:])
	atkPos := worldPos(p[asAtkPos:])
	spd := binary.LittleEndian.Uint16(p[asAtkSpd:])
	target := binary.LittleEndian.Uint32(p[asTarget:])
	targetPos := worldPos(p[asTargetPos:])
	damage := math.Float64frombits(binary.LittleEndian.Uint64(p[asDamage:]))
	hpBefore := math.Float64frombits(binary.LittleEndian.Uint64(p[asTargetHP:]))
	atkType := int32(binary.LittleEndian.Uint32(p[asAttackType:]))

	// The two facts §2 is built on, read back out of the bytes: the damage is a
	// float64 at +42, and the knock-back position IS the target's own position.
	if damage != 1328393.0 {
		t.Fatalf("+42 as a float64 = %v, want 1328393 (§2.1)", damage)
	}
	if knock := worldPos(p[asKnockPos:]); knock != targetPos {
		t.Fatalf("+62 %v is not the target position %v - 56 of 56 samples say it is",
			knock, targetPos)
	}
	if start := binary.LittleEndian.Uint32(p[asStartTime:]); start != 0 {
		t.Fatalf("+22 startTime = %d, want 0 (it is 0 in all 56 samples)", start)
	}

	ours := BuildAttackSingle(attacker, atkPos, spd, target, targetPos, damage,
		hpBefore, atkType)
	if len(ours) != AttackSingleSize {
		t.Fatalf("our 3564 is %d bytes, want %d", len(ours), AttackSingleSize)
	}
	if got := binary.LittleEndian.Uint16(ours[2:]); int(got) != AttackSingleSize {
		t.Fatalf("our 3564 length word at +2 = %d, want %d", got, AttackSingleSize)
	}

	matched, allow, unexplained := 0, 0, 0
	for i := range p {
		if p[i] == ours[i] {
			matched++
			continue
		}
		if attackDiffAllowed(i) == "" {
			unexplained++
			t.Errorf("3564 byte +%d differs: capture 0x%02x, builder 0x%02x - and it is "+
				"NOT an allowance", i, p[i], ours[i])
			continue
		}
		allow++
	}
	t.Logf("%d of %d bytes identical, %d allowance(s), %d unexplained", matched,
		len(p), allow, unexplained)
	if matched != AttackSingleSize || allow != 0 || unexplained != 0 {
		t.Fatalf("the builder must reproduce the live 3564 exactly when it is given the "+
			"capture's own values: %d of %d identical, %d allowance(s), %d unexplained",
			matched, len(p), allow, unexplained)
	}
}

// TestAttackSingleAllowancesNameEveryDifference is the other half of (b): with OUR
// own numbers in the per-hit fields, every byte that moves must be covered by a
// NAMED allowance, so an offset the layout does not explain cannot hide behind a
// blanket "it differs".
func TestAttackSingleAllowancesNameEveryDifference(t *testing.T) {
	p := attackFixture(t, "testdata/live_attack_single.bin", CmdZCAttackSingle,
		AttackSingleSize)

	// A different character, a different monster, a different roll - as a real
	// session would have.
	ours := BuildAttackSingle(7, [3]int32{-419, 870, 29}, 1262, 0x20000003,
		[3]int32{2240, -590, 7}, 1147, 5000, plainAttackType)

	matched, allow, unexplained := 0, 0, 0
	names := map[string]bool{}
	for i := range p {
		if p[i] == ours[i] {
			matched++
			continue
		}
		if why := attackDiffAllowed(i); why == "" {
			unexplained++
			t.Errorf("3564 byte +%d differs and is not an allowance (capture 0x%02x, "+
				"ours 0x%02x)", i, p[i], ours[i])
			continue
		} else {
			allow++
			names[why] = true
		}
	}
	t.Logf("%d of %d bytes identical, %d allowance(s), %d unexplained, fields %v",
		matched, len(p), allow, unexplained, keysOf(names))
	if unexplained != 0 {
		t.Fatalf("%d unexplained byte(s) - the layout does not account for them",
			unexplained)
	}
	if allow == 0 {
		t.Fatal("nothing differed, so this test proved nothing about the allowances")
	}
	// The bytes that MUST be identical whatever the numbers are: the command word,
	// the length word and startTime.
	for _, off := range []int{0, 1, 2, 3, 22, 23, 24, 25} {
		if p[off] != ours[off] {
			t.Errorf("byte +%d is not per-hit data, but ours (0x%02x) differs from the "+
				"capture (0x%02x)", off, ours[off], p[off])
		}
	}
}

// keysOf lists a set's keys in a log line, sorted by name, so a failure message is
// stable from run to run.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestAddHPAfterTheAttackCarriesTheHPAfter is (c): the 3557 that follows the 3564
// names the same handle, carries the HP AFTER the hit, and its delta is the SAME
// double as the 3564's damage - byte for byte, because that is what §2.1 measured
// in 48 of 56 samples and what the client draws the flying number from.
func TestAddHPAfterTheAttackCarriesTheHPAfter(t *testing.T) {
	before, damage := 5000.0, 1147.0
	after := before - damage

	hit := BuildAttackSingle(7, [3]int32{-419, 870, 29}, 471, 0x20000003,
		[3]int32{2240, -590, 7}, damage, before, plainAttackType)
	add := BuildAddHP(0x20000003, after, damage)

	if len(add) != AddHPSize {
		t.Fatalf("3557 is %d bytes, want %d", len(add), AddHPSize)
	}
	if got := binary.LittleEndian.Uint32(add[ahHandle:]); got != 0x20000003 {
		t.Errorf("3557 +2 handle = %d, want the target 0x20000003", got)
	}
	// THE SAME DOUBLE, byte for byte - not "close".
	if !bytesEqual(add[ahAddHP:ahAddHP+8], hit[asDamage:asDamage+8]) {
		t.Fatalf("the 3557 delta %x is not the 3564 damage %x - the client would draw "+
			"one number and move the plate by another", add[ahAddHP:ahAddHP+8],
			hit[asDamage:asDamage+8])
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:])); got != after {
		t.Errorf("3557 +6 = %v, want the HP AFTER the hit %v", got, after)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:])); got == before {
		t.Errorf("3557 +6 carries the HP BEFORE the hit (%v) - the order is the other way "+
			"round (§2.1)", got)
	}
	if add[ahFlag] != 0 {
		t.Errorf("3557 +22 = %d, want 0 for a positive delta (§3.4)", add[ahFlag])
	}
	t.Logf("3564 damage %g then 3557 handle %d: HP %g -> %g, delta the same double, +22 = {0}",
		damage, 0x20000003, before, after)
}

// TestAddHPRebuildsTheLivePartyFixture holds BuildAddHP against the capture's own
// 23 bytes, so the layout the attack lane leans on is proved by a real packet and
// not only by our own arithmetic.
func TestAddHPRebuildsTheLivePartyFixture(t *testing.T) {
	p := attackFixture(t, "testdata/live_add_hp_party.bin", CmdZCAddHP, AddHPSize)
	handle := binary.LittleEndian.Uint32(p[ahHandle:])
	nowHP := math.Float64frombits(binary.LittleEndian.Uint64(p[ahNowHP:]))
	addHP := math.Float64frombits(binary.LittleEndian.Uint64(p[ahAddHP:]))

	ours := BuildAddHP(handle, nowHP, addHP)
	if !bytesEqual(p, ours) {
		t.Fatalf("3557 differs from the capture:\n got %x\nwant %x", ours, p)
	}
	t.Logf("3557: 23 of 23 bytes identical to the live capture (handle %d, %g, delta %g)",
		handle, nowHP, addHP)
}

// bytesEqual is a byte-for-byte compare, here so a failure message can print both
// slices as hex.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// attackZone is a zone that can answer an attack: the real stance table, the real
// job and item numbers, one logged-in character holding a sword (so ATK is the
// sheet's and not the formula's floor), and one monster with a known HP.
//
// The monster's HP is put where the world puts it - Handler.monsterHP, written by
// SpawnMonsterObject (zone\create.go) - because that map IS the ledger the two
// packets have to agree with.
//
// THE PRESS IS INSIDE THE STANCE'S OWN REACH (R90). The captured caster stood 919
// units from the Scarecrow, which is a press NO stance allows - Peltast (the
// capture's stance) states MinR 1 .. MaxR 125 - and since R90 a press that far walks
// the character in instead of hitting. These tests are about the packets, their
// offsets and the cadence, so the character is moved to a stand point 100 units from
// the monster: inside 125, every other field of the fixture untouched.
func attackZone(t *testing.T) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs := castZone(t)
	h.Players = Players{}
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}
	h.monsterHP = map[uint32]float64{capturedTarget: 5000}
	p.sess.Pos = attackReachStand
	p.sess.Team[0].Position = attackReachStand
	return h, &stringBuf{buf: logs}, p
}

// attackReachStand is attackZone's stand point: 100 units from the Scarecrow along
// the y axis, on its own cell. `scarecrow()` is at (-919, 2311, 136), so the distance
// is exactly 100 - inside Peltast's MaxR 125 and outside its MinR 1.
var attackReachStand = [3]int32{-919, 2211, 136}

// stringBuf is a tiny adapter so these tests log through the same buffer castZone
// built (bytes.Buffer) while reading it as text.
type stringBuf struct{ buf interface{ String() string } }

func (s *stringBuf) String() string { return s.buf.String() }

// TestUnknownTargetSendsAttackDisableAndNothingElse is (d): a handle this zone is
// not running produces ZC_ATTACK_DISABLE (3568) and NO hit - not a 3564, not a
// 3557 - with one log line naming the handle.
//
// It goes through h.Reply, not h.attack, on purpose: half of what makes the click
// do anything is the dispatch case, and a test that called the handler directly
// would pass with no case in the switch.
func TestUnknownTargetSendsAttackDisableAndNothingElse(t *testing.T) {
	h, logs, p := attackZone(t)
	const forged = 0x20123456

	f := attackFrame(0, forged)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a refused attack sent %d packet(s), want 1 (3568 only):\n%s",
			len(out), logs.String())
	}
	disable := out[0]
	if cmd := binary.LittleEndian.Uint16(disable); cmd != CmdZCAttackDisable {
		t.Fatalf("the refusal sent cmd %d, want %d ZC_ATTACK_DISABLE", cmd,
			CmdZCAttackDisable)
	}
	if len(disable) != AttackDisableSize {
		t.Fatalf("3568 is %d bytes, want %d", len(disable), AttackDisableSize)
	}
	if got := binary.LittleEndian.Uint32(disable[adHandle:]); got != capturedCaster {
		t.Errorf("3568 +2 handle = %d, want the character that swung (%d)", got,
			capturedCaster)
	}
	if got := binary.LittleEndian.Uint32(disable[adTarget:]); got != forged {
		t.Errorf("3568 +6 targetHandle = %d, want the handle that was clicked (%d)", got,
			forged)
	}
	if got := disable[adType]; got != attackDisableInvalidTarget {
		t.Errorf("3568 +10 type = %d, want %d ATTACK_DISABLE_INVALID_TARGET (type 0 "+
			"makes the client look `value` up as an item id)", got,
			attackDisableInvalidTarget)
	}
	if got := binary.LittleEndian.Uint32(disable[adValue:]); got != 0 {
		t.Errorf("3568 +11 value = %d, want 0", got)
	}

	text := logs.String()
	if n := strings.Count(text, "ATTACK refused"); n != 1 {
		t.Fatalf("%d refusal line(s), want exactly 1:\n%s", n, text)
	}
	if !strings.Contains(text, "not a monster this zone is running") {
		t.Errorf("the refusal does not say why:\n%s", text)
	}
	if strings.Contains(text, "3564") || strings.Contains(text, "3557") {
		t.Errorf("a refused attack named a hit packet in its log:\n%s", text)
	}
	t.Logf("refusal log line: %s", lastLine(text))
}

// TestAttackFromASlotThatIsNotInTheTeam covers the other refusal: a charIndex the
// session does not have is a broken client, and the 3568 goes out with handle 0
// because there is no character to name.
func TestAttackFromASlotThatIsNotInTheTeam(t *testing.T) {
	h, logs, p := attackZone(t)

	f := attackFrame(7, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || binary.LittleEndian.Uint16(out[0]) != CmdZCAttackDisable {
		t.Fatalf("charIndex 7 produced %d packet(s), want one 3568:\n%s", len(out),
			logs.String())
	}
	if got := binary.LittleEndian.Uint32(out[0][adHandle:]); got != 0 {
		t.Errorf("3568 +2 handle = %d, want 0 (no character can be named)", got)
	}
	if hp, ok := h.monsterLiveHP(capturedTarget); !ok || hp != 5000 {
		t.Errorf("a refused attack moved the monster's HP to %v (known %v), want 5000", hp, ok)
	}
	t.Logf("refusal log line: %s", lastLine(logs.String()))
}

// TestAttackAnswersTheHitThenTheHP is the whole lane in one call: one 3666 gives
// 3564 then 3557, at the two measured sizes, with the damage in +42, the HP BEFORE
// in +50, the HP AFTER in the 3557's +6 and the same double as its delta - and the
// damage is the SAME function the skill cast uses (castDamage), called with the
// plain attack's 100%.
func TestAttackAnswersTheHitThenTheHP(t *testing.T) {
	h, logs, p := attackZone(t)

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("one attack produced %d packet(s), want 2 (3564 then 3557):\n%s",
			len(out), logs.String())
	}
	hit, add := out[0], out[1]
	if cmd := binary.LittleEndian.Uint16(hit); cmd != CmdZCAttackSingle {
		t.Fatalf("first packet is cmd %d, want %d", cmd, CmdZCAttackSingle)
	}
	if len(hit) != AttackSingleSize {
		t.Fatalf("3564 is %d bytes, want %d", len(hit), AttackSingleSize)
	}
	if cmd := binary.LittleEndian.Uint16(add); cmd != CmdZCAddHP {
		t.Fatalf("second packet is cmd %d, want %d", cmd, CmdZCAddHP)
	}

	// THE DAMAGE IS castDamage's, with the plain attack's own factor.
	c := p.sess.Team[0]
	sheet := h.sheetFor(c)
	want := castDamage(sheet.ATK, plainAttackPercent, 0)
	if got := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:])); got != want {
		t.Fatalf("3564 +42 = %v, want castDamage(ATK %v, %d, 0) = %v", got, sheet.ATK,
			plainAttackPercent, want)
	}

	// The two HP numbers, and the order §2.1 measured.
	before := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	after := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:]))
	if before != 5000 {
		t.Errorf("3564 +50 = %v, want the HP BEFORE the hit (5000)", before)
	}
	if after != 5000-want {
		t.Errorf("3557 +6 = %v, want the HP AFTER the hit (%v)", after, 5000-want)
	}
	if !bytesEqual(add[ahAddHP:ahAddHP+8], hit[asDamage:asDamage+8]) {
		t.Errorf("the 3557 delta is not the 3564's damage double")
	}
	if got := h.monsterHPFor(capturedTarget); got != after {
		t.Errorf("the ledger holds %v after the hit, but the zone sent %v", got, after)
	}
	// The attacker, the target and the spots, at their measured offsets.
	if got := binary.LittleEndian.Uint32(hit[asAttacker:]); got != capturedCaster {
		t.Errorf("3564 +4 attacker = %d, want %d", got, capturedCaster)
	}
	if got := binary.LittleEndian.Uint32(hit[asTarget:]); got != capturedTarget {
		t.Errorf("3564 +26 target = %d, want %d", got, capturedTarget)
	}
	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != plainAttackType {
		t.Errorf("3564 +58 type = %d, want %d", got, plainAttackType)
	}
	if got := worldPos(hit[asTargetPos:]); got != ([3]int32{-919, 2311, 136}) {
		t.Errorf("3564 +30 target pos = %v, want the monster's own spot", got)
	}
	t.Logf("%s", lastLine(logs.String()))
}

// TestTwoMonstersKeepSeparateHP is (e): two monsters hit in turn must each carry
// their own HP. One shared number would show up here as the second monster starting
// at the first one's remaining HP.
func TestTwoMonstersHitInTurnKeepSeparateHP(t *testing.T) {
	h, logs, p := attackZone(t)
	const second = 0x20000002

	h.spawnMu.Lock()
	h.spawnedMonster = append(h.spawnedMonster,
		// WITHIN THE STANCE'S REACH OF THE CHARACTER (R90): 80 units along x from
		// attackReachStand, on the same cell. It used to stand at (-400, 900), which
		// no stance reaches from the stand point - and since R90 a press that far
		// walks instead of hitting, which is a different test's question.
		Monster{Handle: second, X: -839, Y: 2211, Cell: 136, ClassID: 11600, Level: 130})
	h.monsterHP[second] = 9000
	h.spawnMu.Unlock()

	before := func(pkt []byte) float64 {
		return math.Float64frombits(binary.LittleEndian.Uint64(pkt[asTargetHP:]))
	}
	after := func(pkt []byte) float64 {
		return math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahNowHP:]))
	}
	fire := func(target uint32) [][]byte {
		// THE CADENCE IS NOT WHAT THIS TEST IS ABOUT. zone\attack.go now holds a
		// second 3666 that arrives inside the character's own ASPD, so each click
		// here is first placed one full interval in the past - the question this
		// test asks is the two monsters' HP ledgers, and nothing about it changes.
		p.sess.noteAttack(0, time.Now().Add(-time.Hour))
		f := attackFrame(0, target)
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZAttack, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("an attack on %d produced %d packet(s), want 2:\n%s", target,
				len(out), logs.String())
		}
		return out
	}

	first := fire(capturedTarget)      // #1 on the Scarecrow: 5000 -> 5000-d
	secondHit := fire(second)          // #2 on the other one: 9000 -> 9000-d
	firstAgain := fire(capturedTarget) // #3 back on the Scarecrow

	d := 5000 - after(first[1])
	if before(first[0]) != 5000 {
		t.Errorf("monster 1's first hit reported before = %v, want 5000", before(first[0]))
	}
	if before(secondHit[0]) != 9000 {
		t.Fatalf("monster 2's first hit reported before = %v, want its OWN 9000 - the two "+
			"monsters share one HP", before(secondHit[0]))
	}
	if got, want := before(firstAgain[0]), 5000-d; got != want {
		t.Errorf("monster 1's second hit reported before = %v, want %v (its own remaining "+
			"HP, not the other monster's)", got, want)
	}
	if a, b := after(first[1]), after(secondHit[1]); a == b {
		t.Errorf("both monsters ended at %v - one ledger, not two", a)
	}
	if got, want := h.monsterHPFor(second), 9000-d; got != want {
		t.Errorf("monster 2's ledger holds %v, want %v", got, want)
	}
	t.Logf("monster 1: 5000 -> %v -> %v | monster 2: 9000 -> %v", after(first[1]),
		after(firstAgain[1]), after(secondHit[1]))
}

// TestAttackWithoutAMonsterHPInventsNone is the honesty branch: a monster this zone
// runs but keeps no HP for must NOT be answered with an invented HP. Nothing that
// carries one is sent, and the log says so. Sending 0 would be worse than sending
// nothing - the client computes `+50 - damage` and plays the death animation when
// that lands at or under zero.
func TestAttackWithoutAMonsterHPInventsNone(t *testing.T) {
	h, logs, p := attackZone(t)

	h.spawnMu.Lock()
	delete(h.monsterHP, capturedTarget) // no HP was ever given to this one
	h.spawnMu.Unlock()

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an attack with no HP to report sent %d packet(s), want 0:\n%s",
			len(out), logs.String())
	}
	text := logs.String()
	if !strings.Contains(text, "applied WITHOUT HP tracking") {
		t.Fatalf("the log does not say the hit went unrecorded:\n%s", text)
	}
	if !strings.Contains(text, "no 3564 and no 3557 sent") {
		t.Errorf("the log does not name what it withheld:\n%s", text)
	}
}

// TestAttackProbeOverASocket is the lane's live probe: a REAL zone, wired from the
// real files (the same helper the Scarecrow creation test uses), a real monster
// created by the same call the Scarecrow's script makes, and a real encrypted 3666
// on a loopback socket. It touches no database and no running role.
//
// It skips - never fails - when the corpus, the mesh or the client's xml are not on
// this machine, because those are the inputs it is made of.
func TestAttackProbeOverASocket(t *testing.T) {
	h, lines := scarecrowZone(t)

	handle, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: 11600, X: 2240, Y: -590, Z: 7, MHP: 5000, Level: 130,
	})
	if err != nil {
		t.Skipf("this machine cannot create the Scarecrow here: %v", err)
	}
	if hp, ok := h.monsterLiveHP(handle); !ok || hp != 5000 {
		t.Fatalf("the created monster's ledger says %v (known %v), want the 5000 the "+
			"script gave it - without this the two HP fields mean nothing", hp, ok)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0") // port 0: the OS picks a spare port
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		h.Serve(context.Background(), conn)
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	t.Logf("probe zone listening on %s (no database, no live role touched)", ln.Addr())

	logInAs(t, client, h.Table)

	// THE CLICK: one real CZ_ATTACK, encrypted the way the client encrypts it.
	sendWire(t, client, attackFrame(0, handle), h.Table)

	stream := readFor(t, client, 2500*time.Millisecond)
	hit := findBytes(stream, CmdZCAttackSingle, AttackSingleSize)
	if hit == nil {
		t.Fatalf("the probe's attack produced no 3564 on the wire:\n%s", lines.String())
	}
	add := findBytes(stream, CmdZCAddHP, AddHPSize)
	if add == nil {
		t.Fatalf("the probe's attack produced no 3557 on the wire:\n%s", lines.String())
	}
	if binary.LittleEndian.Uint32(hit[asTarget:]) != handle {
		t.Fatalf("the 3564 names target %d, want the Scarecrow %d",
			binary.LittleEndian.Uint32(hit[asTarget:]), handle)
	}
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	if damage < 1 {
		t.Errorf("the 3564 carries damage %v - a landed hit must put a number on screen",
			damage)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:])); got != 5000 {
		t.Errorf("3564 +50 = %v, want the HP before the hit (5000)", got)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:])); got != 5000-damage {
		t.Errorf("3557 +6 = %v, want %v", got, 5000-damage)
	}
	if !bytesEqual(add[ahAddHP:ahAddHP+8], hit[asDamage:asDamage+8]) {
		t.Errorf("the 3557 delta is not the 3564's damage double")
	}

	text := waitForLine(t, lines, "zone: ATTACK commander", 2*time.Second)
	line := lineWith(text, "zone: ATTACK commander")
	if line == "" {
		t.Fatalf("the zone logged no ATTACK line:\n%s", text)
	}
	if strings.Contains(text, "not handled yet") {
		t.Errorf("the attack still fell into the unhandled branch:\n%s", text)
	}
	t.Logf("probe log line: %s", line)
}

// monsterHPFor reads the ledger without taking the lock twice in one expression.
func (h *Handler) monsterHPFor(handle uint32) float64 {
	hp, _ := h.monsterLiveHP(handle)
	return hp
}

// lastLine is the last non-empty line of a log, for a t.Logf a reader can quote.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

// lineWith is the FIRST line of a log that contains want, or "". It exists so a
// probe can quote the line it is about rather than whatever came last: the zone
// writes its own SENT dump after the ATTACK line, and "the last line" is then the
// dump.
func lineWith(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// THE ATTACK CADENCE AND THE FIELD THE CLIENT TIMES ITS SWING FROM.
//
// What the reference does, and therefore what these tests hold this zone to:
//
//	FieldChar_FSM.cpp:133-136  AS_ATTACK ON_ENTER: ProcessAttack(target); then
//	                           `m_nextAttackTime = timeGetTime() + GetAttackSPD()`
//	FieldChar_FSM.cpp:141-142  ON_UPDATE: `if (timeGetTime() <= m_nextAttackTime)
//	                           return 0;` - the next swing waits
//	FieldChar.cpp:1302-1304    GetAttackSPD() returns GetASPD()
//	get_status_pc.scp:1407-08  ASPD is the DELAY (1/1000 s) between normal attacks
//	GePC.cpp:69                the client's animation ratio is `atkSpd / Stance.ASPD`,
//	                           so 3564's +20 must be that same delay, not the panel CSPD
//
// The interval itself is NEVER a constant in this test either: it is read from
// the same sheet the handler reads (StatusOf -> ASPD), so the day the stance table
// or the item numbers change, the test moves with them and only a real regression
// stays red.

// attackASPD is the interval the handler must use for this test's character,
// computed the same way the handler computes it. A zero here is a FAILURE of the
// premise: this test is not written to tolerate "no interval".
func attackASPD(t *testing.T, h *Handler, p *player) float64 {
	t.Helper()
	sheet := h.sheetFor(p.sess.Team[0])
	if sheet.ASPD <= 0 {
		t.Fatalf("the sheet gives ASPD %v, so this test cannot ask anything about a "+
			"cadence - the data it is written against is missing", sheet.ASPD)
	}
	return sheet.ASPD
}

// TestSecondAttackInsideTheIntervalIsHeld is (c): two 3666 from the SAME character
// in the same instant produce ONE hit. The second one sends nothing at all - no
// 3564, no 3557, no 3568 (it is not a refusal with a reason, it is a swing the
// character cannot make yet) - moves no HP, and writes exactly one log line that
// names the interval AND where it came from.
func TestSecondAttackInsideTheIntervalIsHeld(t *testing.T) {
	h, logs, p := attackZone(t)
	const aswd = 0

	interval := attackASPD(t, h, p)

	f := attackFrame(aswd, capturedTarget)
	fire := func() [][]byte {
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZAttack, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		return out
	}

	first := fire()
	if len(first) != 2 {
		t.Fatalf("the first attack produced %d packet(s), want 2 (3564 then 3557):\n%s",
			len(first), logs.String())
	}
	hpAfterFirst := h.monsterHPFor(capturedTarget)

	second := fire()
	if len(second) != 0 {
		t.Fatalf("a second attack inside the %v interval produced %d packet(s), want 0 "+
			"- the character cannot swing twice that fast:\n%s", interval, len(second),
			logs.String())
	}
	if hp := h.monsterHPFor(capturedTarget); hp != hpAfterFirst {
		t.Errorf("the held attack still moved the monster's HP to %v (was %v)", hp,
			hpAfterFirst)
	}

	text := logs.String()
	line := lineWith(text, "ATTACK held")
	if line == "" {
		t.Fatalf("the held swing wrote no log line naming the interval:\n%s", text)
	}
	if n := strings.Count(text, "ATTACK held"); n != 1 {
		t.Errorf("%d 'ATTACK held' line(s), want exactly 1:\n%s", n, text)
	}
	if !strings.Contains(line, "SCR_Get_ASPD") {
		t.Errorf("the held line does not say WHERE the interval came from:\n%s", line)
	}
	if want := time.Duration(interval) * time.Millisecond; !strings.Contains(line,
		want.String()) {
		t.Errorf("the held line does not name the interval %v:\n%s", want, line)
	}
	if strings.Contains(line, "3568") || strings.Contains(line, "ZC_ATTACK_DISABLE") {
		t.Errorf("a held swing was reported as a REFUSAL (3568); it is not one:\n%s", line)
	}
	t.Logf("held line: %s", line)
}

// TestAttackRequestIsLoggedBeforeTheAnswer pins the measurement line: ONE line per
// client 3666, written at the parse, carrying the character index, that
// character's handle and the target - and written BEFORE the decision line, so the
// two stamps bracket the decision and the press-to-answer gap can be read off.
// The timestamps themselves are a logger flag and are not tested (main.go).
func TestAttackRequestIsLoggedBeforeTheAnswer(t *testing.T) {
	h, logs, p := attackZone(t)
	handle := p.sess.Team[0].Handle

	f := attackFrame(0, capturedTarget)
	if _, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	text := logs.String()
	want := fmt.Sprintf("zone: REQ ATTACK char 0 (%d) target %d", handle, capturedTarget)
	if n := strings.Count(text, want); n != 1 {
		t.Fatalf("%d %q line(s), want exactly 1:\n%s", n, want, text)
	}
	at := strings.Index(text, want)
	answer := strings.Index(text, "zone: ATTACK commander")
	if answer < 0 {
		t.Fatalf("the press wrote no decision line:\n%s", text)
	}
	if at > answer {
		t.Errorf("the request line came AFTER the decision line, so it cannot bracket it:\n%s",
			text)
	}
	t.Logf("request line: %s", want)
}

// TestAttackLandsAgainAfterTheInterval is the other half of (c): the gate is a
// WAIT, not a lockout. Once the sheet's own interval has passed, the next 3666 is
// answered with its own 3564 + 3557.
//
// The wait is simulated by moving the recorded hit back in time, because the test
// must not sleep for the character's real ~1 s delay - and because that is exactly
// what the recorded time means: "this character last swung at T".
func TestAttackLandsAgainAfterTheInterval(t *testing.T) {
	h, logs, p := attackZone(t)

	interval := time.Duration(attackASPD(t, h, p)) * time.Millisecond
	f := attackFrame(0, capturedTarget)
	fire := func() [][]byte {
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZAttack, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		return out
	}

	if out := fire(); len(out) != 2 {
		t.Fatalf("the first attack produced %d packet(s), want 2:\n%s", len(out),
			logs.String())
	}
	if out := fire(); len(out) != 0 {
		t.Fatalf("the second attack inside %v was answered - the gate is not there", interval)
	}

	// One millisecond MORE than the interval: the wait is over.
	p.sess.noteAttack(0, time.Now().Add(-interval-time.Millisecond))
	out := fire()
	if len(out) != 2 {
		t.Fatalf("an attack %v after the last one produced %d packet(s), want 2 - the "+
			"interval has passed and the swing is owed:\n%s", interval+time.Millisecond,
			len(out), logs.String())
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != CmdZCAttackSingle {
		t.Fatalf("the late attack answered with cmd %d, want %d", cmd, CmdZCAttackSingle)
	}
	// The chain of four readings above must be a chain, not one lucky number: the
	// second hit's HP BEFORE is the first hit's HP AFTER.
	before := math.Float64frombits(binary.LittleEndian.Uint64(out[0][asTargetHP:]))
	after := math.Float64frombits(binary.LittleEndian.Uint64(out[1][ahNowHP:]))
	if !(after < before) {
		t.Errorf("the second hit reported HP %v -> %v: the ledger did not move", before, after)
	}
	t.Logf("interval %v from the character's own ASPD; the swing after it landed %v -> %v",
		interval, before, after)
}

// TestAttackSpdCarriesTheStanceASPD is the field itself: 3564's +20 is the
// character's ASPD - the attack DELAY in ms - and NOT the CSPD the status panel
// prints. The client divides it by the stance's own ASPD to get its animation
// ratio (GePC.cpp:69), so filling it with a panel number plays the swing at a
// fraction of its speed, which is what the owner saw as "the attack speed is
// bonkers".
//
// The captured characters carried 471 and 1262 in this field, and no CSPD this
// tree computes is in that family; a stance's ASPD is.
func TestAttackSpdCarriesTheStanceASPD(t *testing.T) {
	h, logs, p := attackZone(t)

	sheet := h.sheetFor(p.sess.Team[0])
	if sheet.ASPD <= 0 || sheet.CSPD <= 0 {
		t.Fatalf("the sheet gives ASPD %v and CSPD %v - neither can be told apart",
			sheet.ASPD, sheet.CSPD)
	}
	if sheet.ASPD == sheet.CSPD {
		t.Fatalf("ASPD and CSPD are both %v on this character, so this test could not "+
			"tell the two fields apart", sheet.ASPD)
	}

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("the attack produced %d packet(s), want 2:\n%s", len(out), logs.String())
	}
	got := binary.LittleEndian.Uint16(out[0][asAtkSpd:])
	if want := uint16(math.Round(sheet.ASPD)); got != want {
		t.Errorf("3564 +20 = %d, want the character's ASPD %d (the DELAY in ms, "+
			"SCR_Get_ASPD) - NOT the panel CSPD %v", got, want, sheet.CSPD)
	}
	if got == uint16(math.Round(sheet.CSPD)) {
		t.Errorf("3564 +20 = %d is the panel CSPD %v: the client would divide its "+
			"animation by the stance's ASPD and play the swing far too slow",
			got, sheet.CSPD)
	}
	t.Logf("ASPD %v -> 3564 +20 = %d; the panel's CSPD %v is a different number",
		sheet.ASPD, got, sheet.CSPD)
}

// TestTheHeldAttackIsNotARefusal pins the TARGET LOCK's side. What the source
// shows, in one line each:
//
//	geZCPacket.h:1386-1390   CZ_ATTACK carries seq, checksum, charIndex and ONE
//	                         targetID - it has no per-swing state to drop
//	FieldChar.cpp:989         the reference server STORES the target
//	                         (`m_attackTarget = target`) and walks into
//	                         AS_ATTACK_CHASE; its AS_ATTACK loop re-attacks that
//	                         stored target (`FieldChar_FSM.cpp:121-160`)
//	FieldChar_FSM.cpp:123-131 a null target is what ends it: `GetAttackTarget()`
//	                         NULL -> SendAttackDisabled(INVALID_TARGET)
//
// So the lock lives on the SERVER, in the reference's attack state, and this zone
// answers one click with one hit - nothing this zone sends CLEARS a client target.
// The ONE packet in this lane that can drop a client's attack is 3568 with
// ATTACK_DISABLE_INVALID_TARGET (`GENormalObject.cpp:1746-1750`), which is why a swing
// that is merely too early must NOT be answered with one: it would drop the target
// the player is still holding. This test pins exactly that.
func TestTheHeldAttackIsNotARefusal(t *testing.T) {
	h, logs, p := attackZone(t)

	f := attackFrame(0, capturedTarget)
	fire := func() [][]byte {
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZAttack, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		return out
	}
	if out := fire(); len(out) != 2 {
		t.Fatalf("the first attack produced %d packet(s), want 2:\n%s", len(out),
			logs.String())
	}
	for _, pkt := range fire() {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd == CmdZCAttackDisable {
			t.Fatalf("a swing inside the character's own interval was answered with "+
				"%d: the client would DROP the target the player is holding "+
				"(GENormalObject.cpp:1746-1750)", cmd)
		}
	}
	// And the hit that DID land named the clicked target, which is the handle the
	// client keeps attacking: nothing in our 3564/3557 rewrites it.
	out := fire()
	if len(out) != 0 {
		t.Fatalf("a third attack inside the interval was answered")
	}
	t.Logf("held swing: nothing sent, so nothing cleared the client's target")
}

