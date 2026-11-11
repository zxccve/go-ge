package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"geserver/protocol"
)

// THE SKILL CAST - the two live packets in, the two answers out.
//
// The oracle is the owner's own capture, cut into fixtures and never retyped
// (`history\combat.md` §9.3, §9.4, §9.7):
//
//	zone\testdata\live_cz_skill_target.bin   19 B  CZ_SKILL_TARGET 3646
//	zone\testdata\live_skill_casting.bin     60 B  ZC_SKILL_CASTING 3571
//	zone\testdata\live_skill_melee.bin      328 B  ZC_SKILL_MELEE 3573
//
// The values every test below asserts are the capture's own: charIndex 1, skill
// 10,561 ("Rapid Shot"), target 565,826 (the Scarecrow at -919, 2311, cell 136),
// caster 1,560,908 at (-910, 1392, cell 29), and the damage 15,233,403.1170533 as
// a float64 at 3573 +40.
const (
	capturedCharIndex = 1
	capturedSkill     = 10561
	capturedTarget    = 565826
	capturedCaster    = 1560908
	capturedDamage    = 15233403.1170533

	// instantSkill is Implode Shock: the real row with CastTime 0 and HoldTime 1630,
	// so it is the only shape that can answer the owner's own question - "some skills
	// have instant cast, so u also need to see the hold time".
	instantSkill = 10562
)

// skillFixture reads one captured packet and checks it is the packet it claims to
// be: the file is the whole packet, so its size and its command word must agree
// with the client's own table before any offset is trusted.
func skillFixture(t *testing.T, name string, cmd uint16, size int) []byte {
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

// castingDiffAllowed answers why a differing byte offset in 3571 is EXPECTED to
// differ, or "" when the builder must reproduce the capture there.
//
// It used to be the whole twelve bytes +32..+43, because none of the three int32
// there had a meaning. TWO of them do now: +36 and +40 both carry the wind-up the
// client's cast state uses (see the disassembly in zone\skill_cast.go
// BuildSkillCasting), so the builder must put the row's own CastTime in both and
// this test holds it to the capture's 483 in both. What is left is FOUR bytes -
// +32..+35, which the client's 3571 handler never reads at all.
func castingDiffAllowed(off int) string {
	if off >= 32 && off < 36 {
		return "+32..+35: not a field - the client's 3571 handler never reads it"
	}
	return ""
}

// meleeDiffAllowed is the same for 3573: the four fields whose VALUE is this
// server's decision rather than the capture's, and nothing else.
//
//	+40 damage       ours, computed from our own sheet (see castDamage)
//	+48 attack type  ours: NORMAL, the reference's 2 is its knockdown decision
//	+52 target HP    ours: the HP the spawn gave the monster
//	+60..+315 mask   ours: zero, this lane models no buffs
func meleeDiffAllowed(off int) string {
	switch {
	case off >= 40 && off < 48:
		return "+40 float64 damage: ours, from our own sheet"
	case off >= 48 && off < 52:
		return "+48 attack type: ours NORMAL, the capture's 2 is a knockdown"
	case off >= 52 && off < 60:
		return "+52 float64 target HP: ours, the value the spawn gave the monster"
	case off >= 60 && off < 60+256:
		return "+60..+315 buff mask: ours zero, this lane models no buffs"
	}
	return ""
}

// TestParseSkillTargetLiveFrame is check (a): the captured 19-byte request decodes
// to the three values §9.1 measured, at the offsets it measured them.
func TestParseSkillTargetLiveFrame(t *testing.T) {
	p := skillFixture(t, "testdata/live_cz_skill_target.bin", CmdCZSkillTarget, czSkillTargetSize)

	req, ok := ParseSkillTarget(p)
	if !ok {
		t.Fatal("the captured 19-byte CZ_SKILL_TARGET was refused")
	}
	if req.CharIndex != capturedCharIndex {
		t.Fatalf("charIndex = %d, want %d (byte +10)", req.CharIndex, capturedCharIndex)
	}
	if req.Skill != capturedSkill {
		t.Fatalf("skill = %d, want %d (dword at +11)", req.Skill, capturedSkill)
	}
	if req.Target != capturedTarget {
		t.Fatalf("target = %d, want %d (int32 at +15)", req.Target, capturedTarget)
	}
	if !req.HasTarget {
		t.Fatal("3646 has a target field; HasTarget must be true")
	}

	// A frame of ANY other length is refused rather than read past - the fields are
	// fixed offsets and the splitter cuts the frame at the declared size.
	for _, n := range []int{0, 14, czSkillTargetSize - 1} {
		if _, ok := ParseSkillTarget(p[:n]); ok {
			t.Fatalf("a %d-byte frame was accepted; 3646 is a fixed %d bytes",
				n, czSkillTargetSize)
		}
	}
	// And one byte TOO LONG is refused as well: the extra bytes are not fields.
	if _, ok := ParseSkillTarget(append(append([]byte(nil), p...), 0, 0, 0, 0)); ok {
		t.Fatalf("a %d-byte frame was accepted; 3646 is a fixed %d bytes",
			czSkillTargetSize+4, czSkillTargetSize)
	}
}

// TestParseSkillHasNoTarget pins 3645: the same two fields, 15 bytes, and NO target.
func TestParseSkillHasNoTarget(t *testing.T) {
	b := make([]byte, czSkillSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZSkill)
	b[czCharIndex] = capturedCharIndex
	binary.LittleEndian.PutUint32(b[czSkillID:], capturedSkill)

	req, ok := ParseSkill(b)
	if !ok {
		t.Fatal("a 15-byte CZ_SKILL was refused")
	}
	if req.CharIndex != capturedCharIndex || req.Skill != capturedSkill {
		t.Fatalf("charIndex/skill = %d/%d, want %d/%d",
			req.CharIndex, req.Skill, capturedCharIndex, capturedSkill)
	}
	if req.HasTarget {
		t.Fatal("CZ_SKILL 3645 carries no target field; HasTarget must be false")
	}
	if _, ok := ParseSkill(b[:14]); ok {
		t.Fatal("a 14-byte CZ_SKILL was accepted; it is a fixed 15 bytes")
	}
}

// TestBuildSkillCastingMatchesLiveCapture is check (b): the builder re-serialises
// the captured 3571 from the capture's OWN fields, and the twelve bytes this server
// leaves zero are counted and named rather than compared.
func TestBuildSkillCastingMatchesLiveCapture(t *testing.T) {
	p := skillFixture(t, "testdata/live_skill_casting.bin", CmdZCSkillCasting, SkillCastingSize)

	caster := binary.LittleEndian.Uint32(p[scCaster:])
	begin := worldPos(p[scBegin:])
	end := worldPos(p[scEnd:])
	skill := binary.LittleEndian.Uint16(p[scSkill:])
	// THE CAPTURE'S OWN WIND-UP: 483 ms, which is Rapid Shot's CastTime 600 through
	// the reference's cast speed (zone\skill_cast.go BuildSkillCasting). It is in
	// BOTH time fields in the capture, and that is what this test holds the builder
	// to: one number, written twice, exactly as the reference's own packet has it.
	castTime := binary.LittleEndian.Uint32(p[scCastTime:])
	target := binary.LittleEndian.Uint32(p[scTarget:])
	targetPos := worldPos(p[scTargetPos:])

	// The capture's own numbers, so a wrong fixture cannot make this pass quietly.
	if caster != capturedCaster || skill != capturedSkill || target != capturedTarget {
		t.Fatalf("capture reads caster %d skill %d target %d, want %d/%d/%d",
			caster, skill, target, capturedCaster, capturedSkill, capturedTarget)
	}
	if begin != [3]int32{-910, 1392, 29} {
		t.Fatalf("beginPos = %v, want the caster's own -910/1392/29", begin)
	}
	if targetPos != [3]int32{-919, 2311, 136} {
		t.Fatalf("targetPos = %v, want the Scarecrow's -919/2311/136", targetPos)
	}

	if castTime != 483 {
		t.Fatalf("the capture's +36 cast time = %d, want 483 (600 x 0.805, the "+
			"reference's own casting time) - the fixture or the offset is wrong", castTime)
	}
	if other := binary.LittleEndian.Uint32(p[scCastTotal:]); other != castTime {
		t.Fatalf("the capture's +40 = %d and +36 = %d: on the one measured cast they "+
			"are the SAME number, which is the fact this builder reproduces", other,
			castTime)
	}

	got := BuildSkillCasting(caster, begin, end, skill, castTime, target, targetPos)
	if len(got) != SkillCastingSize {
		t.Fatalf("builder made %d bytes, want %d", len(got), SkillCastingSize)
	}

	allowed, differ, matched := 0, 0, 0
	for i := range p {
		if castingDiffAllowed(i) != "" {
			allowed++
		}
		if p[i] == got[i] {
			matched++
			continue
		}
		if castingDiffAllowed(i) == "" {
			t.Errorf("3571 byte +%d differs: capture 0x%02x, builder 0x%02x - not an allowance",
				i, p[i], got[i])
			continue
		}
		differ++
	}
	if allowed != 4 {
		t.Fatalf("3571 allows %d byte(s) to differ, want the 4 at +32..+35", allowed)
	}
	if differ > allowed {
		t.Fatalf("3571: %d byte(s) differ inside a %d-byte allowance - impossible", differ, allowed)
	}
	for _, off := range []int{scCastTime, scCastTotal} {
		if v := binary.LittleEndian.Uint32(got[off:]); v != 483 {
			t.Errorf("3571 +%d = %d, want the capture's own 483", off, v)
		}
	}
	t.Logf("3571: %d of %d bytes identical to the live capture (both time fields 483); "+
		"%d of the %d allowance bytes (+32: unread) differ", matched, len(p), differ, allowed)
}

// TestTheCastTimeReachesTheFieldTheClientReads is (a): the wind-up in milliseconds
// reaches +36 and +40, the two fields the client's cast state runs on. The chain
// from the wire to the purple bar is GENormalNet_Action.cpp:1055 -> GeSkill.cpp:118
// -> GeActor.cpp:764 (m_castTime) -> GeActor.cpp:2200-2206 (the state divides the
// skill's CastTime by it) -> GENormalObject.cpp:1260 (m_castTotalTime) ->
// GENormalObject.cpp:505-512 (the gauge), and the binary's own reads are quoted in
// zone\skill_cast.go BuildSkillCasting (0x5fd6d9/+36 -> actor+0x44 -> DIVSS at
// 0x5fc225; 0x5fc191/+40 -> the FSM cast-start message).
//
// THE OFFSETS ARE SPELLED AS LITERALS 36 AND 40 here and not as the constants, on
// purpose: a test that used scCastTime would move with the builder and could never
// notice the builder writing the right number into the wrong place.
func TestTheCastTimeReachesTheFieldTheClientReads(t *testing.T) {
	const off = 36      // literal on purpose: the field the client divides by
	const offTotal = 40 // and the second one it carries into the cast state
	caster := uint32(capturedCaster)
	begin := [3]int32{-910, 1392, 29}
	end := [3]int32{-910, 1392, 29}
	targetPos := [3]int32{-919, 2311, 136}

	// The report's own case, spelled out: a skill whose row says CastTime 900 must
	// put 900 in the packet - in both of the client's two time fields.
	p900 := BuildSkillCasting(caster, begin, end, uint16(capturedSkill), 900,
		capturedTarget, targetPos)
	if len(p900) != SkillCastingSize {
		t.Fatalf("3571 is %d bytes, want %d", len(p900), SkillCastingSize)
	}
	for _, at := range []int{off, offTotal} {
		if got := binary.LittleEndian.Uint32(p900[at:]); got != 900 {
			t.Fatalf("CastTime 900 came out as %d at +%d - the client would draw the "+
				"bar for %d ms, not 900", got, at, got)
		}
	}

	// Every value round-trips, and NOT ONE OTHER BYTE moves: the two time fields are
	// eight bytes wide together and nothing else in the packet is derived from them.
	for _, ct := range []uint32{0, 1, 300, 483, 600, 2000, 65535} {
		pkt := BuildSkillCasting(caster, begin, end, uint16(capturedSkill), ct,
			capturedTarget, targetPos)
		for _, at := range []int{off, offTotal} {
			if got := binary.LittleEndian.Uint32(pkt[at:]); got != ct {
				t.Errorf("CastTime %d came out as %d at +%d", ct, got, at)
			}
		}
		for i := range pkt {
			if i >= off && i < offTotal+4 {
				continue
			}
			if pkt[i] != p900[i] {
				t.Fatalf("CastTime %d moved byte +%d (0x%02x vs 0x%02x): the fields "+
					"are four bytes each and they are at +%d and +%d", ct, i, pkt[i],
					p900[i], off, offTotal)
			}
		}
	}

	// AND CastTime 0 IS A ZERO THERE, not a missing write and not a leftover: the
	// packet is the one this builder made before the fields were filled, byte for
	// byte. HOW THE ZERO CASE IS ACTUALLY HANDLED is one level up and already
	// pinned: a row with CastTime 0 sends NO 3571 at all, because the client divides
	// by this number (GeActor.cpp:2206) - see
	// TestAMoveDuringAnInstantHoldDoesNotCancel (zone\skill_approach_test.go:711).
	zero := BuildSkillCasting(caster, begin, end, uint16(capturedSkill), 0,
		capturedTarget, targetPos)
	for _, at := range []int{off, offTotal} {
		if got := binary.LittleEndian.Uint32(zero[at:]); got != 0 {
			t.Fatalf("CastTime 0 came out as %d at +%d, want 0", got, at)
		}
	}
	if !bytes.Equal(zero[:off], p900[:off]) || !bytes.Equal(zero[off+8:], p900[off+8:]) {
		t.Fatalf("the CastTime bytes are not the ONLY difference between a timed and "+
			"an instant wind-up")
	}
	t.Logf("3571 +%d and +%d carry the row's CastTime (900 -> 900, 0 -> 0); every "+
		"other byte is identical across all values", off, offTotal)
}

// TestBuildSkillMeleeMatchesLiveCapture is check (c), in two parts, because 3573
// has both a LAYOUT to prove and four VALUES this server decides for itself.
//
//  1. fed the capture's own damage, attack type, HP and mask, the builder must
//     reproduce all 328 bytes - that is what proves every offset and width, the
//     float64 damage at +40 included.
//  2. fed OUR values (a computed damage, NORMAL, the spawn's HP, no buffs), the 52
//     bytes that are not values must still match, and the allowance count is printed.
func TestBuildSkillMeleeMatchesLiveCapture(t *testing.T) {
	p := skillFixture(t, "testdata/live_skill_melee.bin", CmdZCSkillMelee, SkillMeleeSize)

	caster := binary.LittleEndian.Uint32(p[smCaster:])
	begin := worldPos(p[smBegin:])
	end := worldPos(p[smEnd:])
	skill := binary.LittleEndian.Uint16(p[smSkill:])
	stack := int32(binary.LittleEndian.Uint32(p[smStack:]))
	target := binary.LittleEndian.Uint32(p[smTarget:])
	damage := math.Float64frombits(binary.LittleEndian.Uint64(p[smDamage:]))
	attackType := int32(binary.LittleEndian.Uint32(p[smAttackType:]))
	hp := math.Float64frombits(binary.LittleEndian.Uint64(p[smTargetHP:]))
	mask := p[smBuffMask : smBuffMask+smBuffLen]
	targetPos := worldPos(p[smTargetPos:])

	// The capture's own numbers.
	if caster != capturedCaster || skill != capturedSkill || target != capturedTarget {
		t.Fatalf("capture reads caster %d skill %d target %d, want %d/%d/%d",
			caster, skill, target, capturedCaster, capturedSkill, capturedTarget)
	}
	if math.Abs(damage-capturedDamage) > 1e-6 {
		t.Fatalf("+40 double = %v, want %v - the damage is a float64, read as one",
			damage, capturedDamage)
	}
	if targetPos != [3]int32{-919, 2311, 136} {
		t.Fatalf("targetPos at +316 = %v, want the Scarecrow's -919/2311/136", targetPos)
	}
	if !bytes.Equal(mask[:134], make([]byte, 134)) || mask[134] != 0x02 {
		t.Fatalf("the captured 256-byte block is not the one bit at mask index 134 "+
			"that §9.4 measured; byte 134 = 0x%02x", mask[134])
	}

	// PART 1 - the layout, with the capture's own values. Not one byte may differ.
	same := BuildSkillMelee(caster, begin, end, skill, stack, target, damage, attackType,
		hp, mask, targetPos)
	if len(same) != SkillMeleeSize {
		t.Fatalf("builder made %d bytes, want %d", len(same), SkillMeleeSize)
	}
	if !bytes.Equal(same, p) {
		for i := range p {
			if p[i] != same[i] {
				t.Fatalf("3573 byte +%d differs from the capture: 0x%02x vs 0x%02x "+
					"(fed the capture's own values, nothing may differ)", i, p[i], same[i])
			}
		}
	}
	t.Logf("3573 layout: all %d bytes reproduced from the capture's own values "+
		"(0 allowances)", len(p))

	// PART 2 - our values: a computed damage, NORMAL attack type, the spawn's HP,
	// and no buff mask. Only the four value fields may differ.
	const ourDamage, ourHP = 42.5, 1000.0
	ours := BuildSkillMelee(caster, begin, end, skill, stack, target, ourDamage,
		attackTypeNormal, ourHP, nil, targetPos)

	allowed, differ, matched := 0, 0, 0
	for i := range p {
		if meleeDiffAllowed(i) != "" {
			allowed++
		}
		if p[i] == ours[i] {
			matched++
			continue
		}
		if meleeDiffAllowed(i) == "" {
			t.Errorf("3573 byte +%d differs: capture 0x%02x, builder 0x%02x - not an allowance",
				i, p[i], ours[i])
			continue
		}
		differ++
	}
	if allowed != 276 {
		t.Fatalf("3573 allows %d byte(s) to differ, want the 276 at +40 damage, +48 "+
			"attack type, +52 HP and +60..+315 mask", allowed)
	}
	if differ > allowed {
		t.Fatalf("3573: %d byte(s) differ inside a %d-byte allowance - impossible", differ, allowed)
	}
	// The two fields that carry the whole point of the packet, read back from OUR
	// bytes: the damage at +40 as a double, the target at +36 as a handle.
	if d := math.Float64frombits(binary.LittleEndian.Uint64(ours[smDamage:])); d != ourDamage {
		t.Fatalf("our 3573 +40 = %v, want %v", d, ourDamage)
	}
	if h := binary.LittleEndian.Uint32(ours[smTarget:]); h != capturedTarget {
		t.Fatalf("our 3573 +36 = %d, want the target handle %d", h, capturedTarget)
	}
	t.Logf("3573 with our own values: %d of %d bytes identical; %d of the %d allowance "+
		"bytes (damage, attack type, HP, 256-byte mask) differ", matched, len(p), differ, allowed)
}

// castZone is a zone that can answer a cast: the real stance table and the real
// skill table (skipped when the client's xml folder is not readable - the rule
// under test IS those tables), one logged-in character standing in stance 10560
// (Peltast, whose SkillID1 is 10561 Rapid Shot, §9.1), and no monster yet.
//
// The class base stats and the item columns are installed too, because they are
// what makes the damage a REAL number instead of the formula's floor: with them the
// caster's ATK is the sheet's ATK (zone\status.go) and the damage line is a number a
// reader can check against the character sheet. Both are restored when the test
// ends, so no other test sees this one's tables.
func castZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	stances, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	skills, err := LoadSkills(stanceDir())
	if err != nil {
		t.Skipf("skill table not readable (%v)", err)
	}
	if js, jerr := LoadJobStats("../../data/job_stats.json"); jerr == nil {
		old := jobStats
		SetJobStats(js)
		t.Cleanup(func() { jobStats = old })
	}
	if is, ierr := LoadItemStats("../../data/item_stats.json"); ierr == nil {
		old := itemStats
		SetItemStats(is)
		t.Cleanup(func() { itemStats = old })
	}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), Stances: stances, Skills: skills}
	return h, logs
}

// castPlayer is the captured caster: handle 1,560,908, job 1, standing in 10560 and
// at its own captured position, holding the sword the stance's condition needs
// (7201 "Sword", a real weapon row - so ATK is the sheet's, not 0).
func castPlayer() *player {
	p := newPlayer(capturedCaster, "Rapid", [3]int32{-910, 1392, 29})
	p.sess.Team[0].Job = 1
	p.sess.Team[0].Stance = 10560
	p.sess.Team[0].Position = [3]int32{-910, 1392, 29}
	p.sess.Team[0].Equip[5] = 7201 // Sword, right hand (HandSpotRight)
	return p
}

// scarecrow is the captured target: handle 565,826, class 11600, at the position
// its own 3527 gave it.
func scarecrow() Monster {
	return Monster{Handle: capturedTarget, X: -919, Y: 2311, Cell: 136,
		ClassID: 11600, Level: 1, HP: 1000000}
}

// skillTargetFrame builds one CZ_SKILL_TARGET from the struct's own offsets.
func skillTargetFrame(charIndex uint8, skill, target uint32) []byte {
	b := make([]byte, czSkillTargetSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZSkillTarget)
	binary.LittleEndian.PutUint32(b[2:], 0x1c)   // seq, as the capture had
	binary.LittleEndian.PutUint32(b[6:], 0x67)   // checksum, as the capture had
	b[czCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[czSkillID:], skill)
	binary.LittleEndian.PutUint32(b[czTarget:], target)
	return b
}

// TestSkillCastAnswersCastingThenMelee is the whole point of the lane: one accepted
// cast produces ZC_SKILL_CASTING first and ZC_SKILL_MELEE second, at the two fixed
// sizes, with the damage in +40 as a float64 and the target where §9.4 put it.
//
// It goes through h.Reply, not h.castSkill, on purpose: half of what makes a pressed
// skill do anything is the dispatch case, and a test that called the handler
// directly would pass with no case in the switch.
func TestSkillCastAnswersCastingThenMelee(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("one cast produced %d packet(s), want 1 - the 3571, whose result waits "+
			"for the cast's clock:\n%s", len(out), logs.String())
	}

	casting := out[0]
	if cmd := binary.LittleEndian.Uint16(casting); cmd != CmdZCSkillCasting {
		t.Fatalf("first packet is cmd %d, want %d (the wind-up comes FIRST, §9.2)", cmd,
			CmdZCSkillCasting)
	}
	if len(casting) != SkillCastingSize {
		t.Fatalf("3571 is %d bytes, want %d", len(casting), SkillCastingSize)
	}

	// THE HIT, WHEN THE CLOCK FIRES (zone\skill_cast_clock.go).
	castDueNow(t, p.sess, 0)
	fired := h.chaseTick(p.sess)
	if len(fired) != 1 {
		t.Fatalf("the cast's clock produced %d packet(s), want 1 (the 3573):\n%s",
			len(fired), logs.String())
	}
	melee := fired[0]
	if cmd := binary.LittleEndian.Uint16(melee); cmd != CmdZCSkillMelee {
		t.Fatalf("the clock's packet is cmd %d, want %d", cmd, CmdZCSkillMelee)
	}
	if len(melee) != SkillMeleeSize {
		t.Fatalf("3573 is %d bytes, want %d", len(melee), SkillMeleeSize)
	}

	// The caster, the skill and the target, at their measured offsets, in BOTH.
	for _, pkt := range append([][]byte{casting}, fired...) {
		if got := binary.LittleEndian.Uint32(pkt[scCaster:]); got != capturedCaster {
			t.Errorf("cmd %d +2 caster = %d, want %d", binary.LittleEndian.Uint16(pkt),
				got, capturedCaster)
		}
		if got := binary.LittleEndian.Uint16(pkt[scSkill:]); uint32(got) != capturedSkill {
			t.Errorf("+30 skill = %d, want %d", got, capturedSkill)
		}
	}
	if got := binary.LittleEndian.Uint32(casting[scTarget:]); got != capturedTarget {
		t.Errorf("3571 +44 target = %d, want %d", got, capturedTarget)
	}
	// THE WIND-UP THE BAR RUNS FOR is the row's own CastTime, not a constant and not
	// zero: this is the end-to-end half of TestTheCastTimeReachesTheFieldTheClientReads
	// (the builder writes it) - here it is the HANDLER that has to put the row's value
	// in, and Rapid Shot's row says 600.
	if row, ok := h.Skills.Row(capturedSkill); ok {
		if got := binary.LittleEndian.Uint32(casting[scCastTime:]); got != uint32(row.CastTime) {
			t.Errorf("3571 +36 cast time = %d, want the skill row's CastTime %d - the "+
				"client draws the casting bar from this field", got, row.CastTime)
		}
	}
	if got := worldPos(casting[scTargetPos:]); got != [3]int32{-919, 2311, 136} {
		t.Errorf("3571 +48 target position = %v, want -919/2311/136", got)
	}
	if got := binary.LittleEndian.Uint32(melee[smTarget:]); got != capturedTarget {
		t.Errorf("3573 +36 target = %d, want %d", got, capturedTarget)
	}
	if got := worldPos(melee[smTargetPos:]); got != [3]int32{-919, 2311, 136} {
		t.Errorf("3573 +316 target position = %v, want -919/2311/136", got)
	}

	// THE DAMAGE: our own formula, and a float64 on the wire. A zero here is the
	// failure this test exists for - it is what the screen would show.
	row, ok := h.Skills.Row(capturedSkill)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedSkill)
	}
	want := castDamage(h.sheetFor(p.sess.Team[0]).ATK, row.SklATK, 0)
	got := math.Float64frombits(binary.LittleEndian.Uint64(melee[smDamage:]))
	if got != want {
		t.Fatalf("3573 +40 damage = %v, want %v (castDamage of our own sheet)", got, want)
	}
	if got < 1 {
		t.Fatalf("3573 +40 damage = %v, want at least 1: a landed cast must show a number", got)
	}
	// The target's HP is the value the spawn carried, not a decremented one.
	if hp := math.Float64frombits(binary.LittleEndian.Uint64(melee[smTargetHP:])); hp != 1000000 {
		t.Errorf("3573 +52 HP = %v, want the spawn's 1000000", hp)
	}
	// No knockdown: this lane sends NORMAL and no 3574 at all.
	if at := binary.LittleEndian.Uint32(melee[smAttackType:]); at != attackTypeNormal {
		t.Errorf("3573 +48 attack type = %d, want %d (NORMAL)", at, attackTypeNormal)
	}

	// THE ONE PROOF LINE: commander, character, skill id AND name, target, damage.
	line := logs.String()
	for _, want := range []string{"SKILL_CAST", "skill 10561", "Rapid Shot",
		"target 565826", "damage "} {
		if !strings.Contains(line, want) {
			t.Errorf("the cast log line does not contain %q:\n%s", want, line)
		}
	}
	// And the packet this lane does NOT send is said out loud, not left silent.
	if !strings.Contains(line, "no ZC_ADD_HP (3557)") {
		t.Errorf("the 3557 skip is not logged:\n%s", line)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(line))
}

// TestSkillCastRefusedWhenStanceDoesNotOffer is check (d): a skill the character's
// CURRENT stance does not offer sends nothing and says why. Job 1 standing in 10010
// (Bareknuckle) has no Rapid Shot in its row, which is the real shape of the
// mistake - a skill id from a stance the character is not in.
func TestSkillCastRefusedWhenStanceDoesNotOffer(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	p.sess.Team[0].Stance = 10010 // Bareknuckle: a job-1 stance that does not list 10561
	h.spawnedMonster = []Monster{scarecrow()}

	if intIn(h.Stances.StanceSkills(10010), capturedSkill) {
		t.Fatalf("stance 10010 offers skill %d - this test needs a stance that does NOT", capturedSkill)
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a skill the stance does not offer produced %d packet(s), want 0", len(out))
	}
	line := logs.String()
	if !strings.Contains(line, "SKILL refused") {
		t.Fatalf("nothing was logged as a refusal:\n%s", line)
	}
	for _, want := range []string{"skill 10561", "stance 10010"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, line)
		}
	}
}

// TestSkillCastRefusedForUnknownTarget is check (e): a target handle that is not a
// monster this zone is running sends nothing. The Scarecrow's real handle is used
// with NO monster registered, so the frame is well formed and only the target is
// wrong - which is exactly a stale or forged handle.
func TestSkillCastRefusedForUnknownTarget(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an unknown target produced %d packet(s), want 0", len(out))
	}
	line := logs.String()
	if !strings.Contains(line, "SKILL refused") || !strings.Contains(line, "target handle 565826") {
		t.Fatalf("the unknown target was not refused by name:\n%s", line)
	}
}

// TestSkillCastRefusesAMisSizedFrame is the first refusal in the list: a frame that
// is not the client's registered size sends nothing and says its length.
func TestSkillCastRefusesAMisSizedFrame(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)[:18]
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an 18-byte 3646 produced %d packet(s), want 0", len(out))
	}
	if line := logs.String(); !strings.Contains(line, "fixed 19 bytes") {
		t.Fatalf("the wrong-length refusal does not name the size:\n%s", line)
	}
}

// TestSkillCastRefusesAnIndexOutsideTheTeam: the char index is a BYTE from the
// client, so it is checked against this session's team before it is used to read
// one - the same rule CZ_CHANGE_STANCE and CZ_EQUIP live under.
func TestSkillCastRefusesAnIndexOutsideTheTeam(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(2, capturedSkill, capturedTarget) // the team is one member
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("character index 2 of a 1-member team produced %d packet(s), want 0", len(out))
	}
	if line := logs.String(); !strings.Contains(line, "character index 2") {
		t.Fatalf("the index refusal does not name the index:\n%s", line)
	}
}

// syncLog is a log destination a test goroutine and the test itself can both use.
// The probe below serves a REAL socket on its own goroutine, so a plain bytes.Buffer
// there would be a data race the moment the two touch it at once.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestProbeSkillCastOverASocket is the LIVE PROBE: a real TCP listener on an
// OS-chosen spare port, the real zone Serve loop, the real Blowfish framing, and a
// real client-side socket sending CZ_START_GAME and then CZ_SKILL_TARGET.
//
// Nothing here touches a database, a live role or the owner's running zones: it
// binds 127.0.0.1 on an ephemeral port, and the team arrives through Handler.TeamOf
// (the hook main.go wires to PostgreSQL) instead of through a login. That is the one
// difference from a client-driven cast, and the log line it prints is quoted in the
// hand-back as PROBE evidence - not as "proven in game".
//
// The skill it casts is the first one the stance the ZONE ITSELF picked offers, so
// the probe cannot cheat by naming a stance the permission rule would refuse.
func TestProbeSkillCastOverASocket(t *testing.T) {
	stances, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	skills, err := LoadSkills(stanceDir())
	if err != nil {
		t.Skipf("skill table not readable (%v)", err)
	}
	monsters, _ := LoadMonsters(stanceDir())
	// TWO tables, on purpose. The Handler needs a *Spec: that is what cuts the
	// CLIENT's frames, length word and all. The probe's READER needs plain SIZES,
	// because the server->client stream is fixed-size plaintext packets and a Spec's
	// length-offset rule is for the other direction - the Spec says ZC_START_GAMEOK
	// keeps its length at +10, while the packet itself says +2, and reading it the
	// Spec's way cuts nothing at all.
	spec, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}
	sizes, err := protocol.LoadTable("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}

	// The captured caster, as the barrack would hand it over: job 1, a sword in the
	// right hand. The server picks its stance from what it holds.
	c := Character{Handle: capturedCaster, Name: "Rapid", Job: 1, Level: 50,
		HP: 1000, MaxHP: 1000, Position: [3]int32{-910, 1392, 29}}
	c.Equip[HandSpotRight] = 7201 // Sword

	// The class base stats and the item columns, so the damage is the sheet's own
	// ATK x SklATK and not the formula's floor.
	if js, jerr := LoadJobStats("../../data/job_stats.json"); jerr == nil {
		old := jobStats
		SetJobStats(js)
		t.Cleanup(func() { jobStats = old })
	}
	if is, ierr := LoadItemStats("../../data/item_stats.json"); ierr == nil {
		old := itemStats
		SetItemStats(is)
		t.Cleanup(func() { itemStats = old })
	}

	logs := &syncLog{}
	h := &Handler{
		Table: spec, Cipher: protocol.DefaultCipher,
		Log:     log.New(logs, "", 0),
		Stances: stances, Skills: skills, Monsters: monsters,
		Spawn: [3]int32{-910, 1392, 29},
		TeamOf: func(context.Context, uint32) ([]Character, error) {
			return []Character{c}, nil
		},
	}
	// THE TARGET HAS TO BE INSIDE THE SKILL'S OWN RANGE. This probe casts the FIRST
	// skill the stance the zone picks offers, and that stance is not the capture's
	// Peltast: job 1 holding only a sword picks 10110 "BackGuard", whose first skill is
	// 10111 "Deep-straight" with MaxR="100". The Scarecrow's captured position is 919
	// units away, which the range check (R83, skillRangeAllowed) now refuses - rightly,
	// and it means this probe had been casting a 100-unit skill across 9 metres and
	// getting an answer. Stand the same monster 40 units from the caster instead.
	probeTarget := scarecrow()
	probeTarget.X, probeTarget.Y, probeTarget.Cell = -910+40, 1392, 29
	h.spawnedMonster = []Monster{probeTarget}

	// Which stance the zone will pick, and the first skill it offers - computed the
	// same way Handler.startGame does it, so the probe presses a legal skill.
	pick, ok := stances.PickStance(1, stances.Hands(c))
	if !ok {
		t.Fatalf("job 1 holding %v has no stance - the probe cannot cast", stances.Hands(c))
	}
	offered := stances.StanceSkills(pick.Stance)
	if len(offered) == 0 {
		t.Fatalf("stance %d (%s) offers no skills - the probe cannot cast",
			pick.Stance, stances.StanceName(pick.Stance))
	}
	skill := uint32(offered[0])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0") // port 0: the OS picks a spare port
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go h.Serve(ctx, conn)
		}
	}()
	t.Logf("probe zone listening on %s (no database, no live role touched)", ln.Addr())

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// CZ_START_GAME (3590): cmd(2) seq(4) checksum(4) session(4) account(4) login(17).
	sg := make([]byte, 18+17)
	binary.LittleEndian.PutUint16(sg[0:], CmdCZStartGame)
	binary.LittleEndian.PutUint32(sg[2:], 1)      // seq
	binary.LittleEndian.PutUint32(sg[6:], 0x1234) // checksum
	binary.LittleEndian.PutUint32(sg[10:], 1)     // session
	binary.LittleEndian.PutUint32(sg[14:], 7)     // account
	copy(sg[18:], "probe")
	if _, err := conn.Write(h.Cipher.EncryptChunk(sg)); err != nil {
		t.Fatalf("write START_GAME: %v", err)
	}

	// CZ_SKILL_TARGET (3646): charIndex 0, the stance's first skill, the Scarecrow.
	if _, err := conn.Write(h.Cipher.EncryptChunk(
		skillTargetFrame(0, skill, capturedTarget))); err != nil {
		t.Fatalf("write SKILL_TARGET: %v", err)
	}

	// Read the answers: the zone sends plaintext, packets back to back, so the same
	// table the splitter uses cuts them out of the stream.
	read := make([]byte, 64*1024)
	var buf []byte
	seen := map[uint16]int{}
	recv := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		n, rerr := conn.Read(read)
		if n > 0 {
			recv += n
			buf = append(buf, read[:n]...)
			frames, used := sizes.Split(buf)
			buf = buf[used:]
			for _, f := range frames {
				seen[f.Cmd]++
			}
			if seen[CmdZCSkillCasting] > 0 && seen[CmdZCSkillMelee] > 0 {
				break
			}
		}
		if rerr != nil && n == 0 {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				continue
			}
			break
		}
	}

	// THE WIND-UP IS NOT ALWAYS THERE. This probe presses the stance's first skill,
	// and for job 1 that is 10111 Deep-straight - whose row states CastTime 0, so the
	// reference's own fork (FieldChar_FSM.cpp:363-367) sends NO 3571 and the result at
	// the press. What must always arrive is the RESULT; the 3571 must arrive exactly
	// when the row states a CastTime.
	row, known := h.Skills.Row(skill)
	if !known {
		t.Fatalf("skill %d is not in datatable_skill.xml", skill)
	}
	wantCasting := 0
	if row.CastTime > 0 {
		wantCasting = 1
	}
	if seen[CmdZCSkillMelee] != 1 {
		t.Fatalf("the probe's cast produced no result over the socket: 3571 x%d, "+
			"3573 x%d, %d byte(s) received, cmds seen %v",
			seen[CmdZCSkillCasting], seen[CmdZCSkillMelee], recv, seen)
	}
	if seen[CmdZCSkillCasting] != wantCasting {
		t.Fatalf("skill %d (CastTime %d) produced %d wind-up packet(s), want %d: a "+
			"CastTime 0 row must send no 3571 at all (the client's cast state would "+
			"divide by its `time`), and a CastTime > 0 row must send exactly one",
			skill, row.CastTime, seen[CmdZCSkillCasting], wantCasting)
	}

	line := logs.String()
	for _, want := range []string{"SKILL_CAST", "damage "} {
		if !strings.Contains(line, want) {
			t.Errorf("the live probe's log has no %q: %s", want, line)
		}
	}
	for _, l := range strings.Split(line, "\n") {
		if strings.Contains(l, "SKILL_CAST commander") {
			t.Logf("LIVE PROBE (%s) on port %s: %s",
				tableName(t), portOf(ln.Addr().String()), strings.TrimSpace(l))
		}
	}
	if strings.Contains(line, "SKILL refused") {
		t.Fatalf("the probe's cast was refused: %s", line)
	}
}

// portOf and tableName exist so the probe's log line names the port it listened on
// and the command table it used, which is what a reader needs to repeat it.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

func tableName(t *testing.T) string {
	t.Helper()
	return "cmd_table_live_z.txt"
}
