package zone

// R84 SLICE 2 TESTS - the game's own damage script DECIDES the number.
//
// WHAT THESE PROVE, and nothing wider:
//
//	1 with a damage script armed, the packet's damage is `ATK x factor` - the
//	  script's number, a MULTIPLIER and not a percentage - on BOTH damage sites
//	  (the 3573 of the aimed cast and the 3581 record of the ground one), and it is
//	  NOT the placeholder;
//	2 the log line names the same number three ways: the factor, the damage it is
//	  worth at this ATK, and the number the packet carries;
//	3 a script that RAISES costs the cast nothing: the packet carries the
//	  placeholder and the cast still goes out;
//	4 with NO script armed (`h.Damage == nil`) the packet carries the placeholder
//	  and no SKILL_SCRIPT line is written at all - a checkout with no corpus is
//	  byte-identical to what it was before R84;
//	5 the missing-builtin recorder names what the cast asked for, unchanged.
//
// The "NOT the placeholder" half is the point: a test that only checked "a
// number arrived" would pass with the switch unwired.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
	"geserver/script"
)

// testDamageScript is ONE DamageScript per call, on the suite's own runtime, with
// the suite's Character Info source behind it so the formula's GetEquipItem reads
// the sword the fixture wears instead of a zero table.
//
// A fresh one per call on purpose: the missing-builtin report fires ONCE per
// source, so a shared source would print its list in whichever test ran first and
// leave the next one with nothing to assert.
func testDamageScript(t *testing.T) *DamageScript {
	t.Helper()
	requireStatus(t)
	if testScriptHost == nil {
		t.Skip("no script host in this checkout: build the corpus with " +
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus")
	}
	ds, err := NewDamageScript(testScriptHost, t.Logf)
	if err != nil {
		t.Fatalf("NewDamageScript: %v (SCR_SKILL_ATTACK must be in the corpus - "+
			"rebuild it with cd src && python scp_to_lua.py --dump ../out/lua_corpus)", err)
	}
	ds.Sheet = defaultStatusForTests
	return ds
}

// runGroundCast drives the CAPTURED Fireball cast through h.Reply and its own
// clock, and answers every packet the cast put on the wire in order (3654, 3571,
// then 3581 + one 3557 per victim).
//
// `ds` nil is the "no script" run: the zone as it was before R84. `mds` nil is a zone
// with the damage script but no target-DEF source, which is the fallback. Everything
// else about two runs is identical, which is what makes a comparison meaningful.
//
// The HANDLER comes back too, so a test can read the fixture's own sheet ATK and
// row - the two numbers the packet is checked against - instead of trusting the
// log line to describe itself.
func runGroundCast(t *testing.T, ds *DamageScript, mds *MonsterDefScript) (h *Handler, packets [][]byte, logs *bytes.Buffer) {
	t.Helper()
	h, logs = groundZone(t)
	if ds != nil {
		h.Damage = ds
	}
	if mds != nil {
		h.Defence = mds
	}
	p := warlock()
	spawnWithHP(h, areaScarecrow())

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v\n%s", err, logs.String())
	}
	// The wind-up waits for the row's own CastTime (Fireball: 900 ms), so the blast
	// is taken at its clock - the same two-step every other ground test uses.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	landed := h.chaseTick(p.sess)
	return h, append(press, landed...), logs
}

// runTargetedCast is the same driver for the AIMED cast (CZ_SKILL 3646 ->
// ZC_SKILL_MELEE 3573, zone\skill_cast.go): the other damage site.
func runTargetedCast(t *testing.T, ds *DamageScript, mds *MonsterDefScript) (h *Handler, packets [][]byte, logs *bytes.Buffer) {
	t.Helper()
	h, logs = castZone(t)
	if ds != nil {
		h.Damage = ds
	}
	if mds != nil {
		h.Defence = mds
	}
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}
	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v\n%s", err, logs.String())
	}
	castDueNow(t, p.sess, 0)
	fired := h.chaseTick(p.sess)
	return h, append(press, fired...), logs
}

// packetOfCmd answers the FIRST packet of a command, and fails when the cast did
// not send one at all - a lane that went silent is the bug these tests exist for.
func packetOfCmd(t *testing.T, packets [][]byte, cmd uint16) []byte {
	t.Helper()
	for _, p := range packets {
		if len(p) >= 2 && cmdOf(p) == cmd {
			return p
		}
	}
	t.Fatalf("the cast sent no cmd %d; it sent %d packet(s)", cmd, len(packets))
	return nil
}

// samePackets fails the test on the first byte that differs, with the packet's
// index and command so the failure names which half moved. It is used ONLY where
// the packet must NOT move: the raising script and the no-script run.
func samePackets(t *testing.T, want, got [][]byte) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("the cast sent %d packet(s) and %d the other time - this run was "+
			"supposed to change nothing on the wire", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(want[i], got[i]) {
			t.Fatalf("packet %d (cmd %d) differs: %d B vs %d B - this run was "+
				"supposed to change nothing on the wire",
				i, cmdOf(want[i]), len(want[i]), len(got[i]))
		}
	}
}

// groundFixtureFor is the two numbers a test needs about the captured Fireball
// cast, read from the FIXTURE (the row and the sheet) and never from the line.
type castNumbers struct {
	atk         float64 // the caster's own sheet ATK
	factor      float64 // the row's own formula: what the script must answer
	deal        float64 // ATK x factor: what the packet must carry
	placeholder float64 // castDamage's number: what it carried before R84 slice 2
}

// scriptFactor is the number `attack.lua:359-365` computes for a row, written
// out so the tests can hold the packet against it without asking the script.
//
// THE SPLASH BRANCH IS THE WHOLE POINT of the `splash` argument: only a SPLASH
// cast multiplies by the row's `SplDam` (`attack.lua:359-361`); a single-target
// cast is the bare `SklATK x (level factor)` (attack.lua:362-364). Rapid Shot
// 10561 has SplDam 0, so a formula that always multiplied would predict ZERO for
// the aimed cast and floor it at 1 - which is exactly the mistake this helper
// exists to prevent.
func scriptFactor(row SkillRow, splash bool) float64 {
	f := row.SklATK * ((row.SklLv+row.SklLvBM)*10 + 100) / 100
	if splash {
		f *= row.SplDam
	}
	return f
}

func groundNumbers(t *testing.T, h *Handler) (SkillRow, castNumbers) {
	t.Helper()
	row, known := h.Skills.Row(capturedFireball)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedFireball)
	}
	c := warlock().sess.Team[capturedGroundCharIndex]
	n := castNumbers{atk: h.sheetFor(c).ATK}
	n.factor = scriptFactor(row, true)
	n.deal = n.atk * n.factor // the factor MULTIPLIES: no /100
	n.placeholder = castDamage(n.atk, row.SklATK, 0)
	return row, n
}

// TestSkillScriptDecidesTheGroundNumber is the switch, proven on the ground lane:
// the 3581 record's damage is the script's number (`ATK x factor`) and not
// the placeholder - and the line names that same number three ways.
func TestSkillScriptDecidesTheGroundNumber(t *testing.T) {
	h, packets, logs := runGroundCast(t, testDamageScript(t), testMonsterDefScript(t))
	row, n := groundNumbers(t, h)

	area := packetOfCmd(t, packets, CmdZCSkillArea)
	if len(area) < saRecords+arDamage+8 {
		t.Fatalf("the 3581 is %d B, too short to hold one record", len(area))
	}
	got := math.Float64frombits(binary.LittleEndian.Uint64(area[saRecords+arDamage:]))
	assertNear(t, "the 3581 record's damage", got, n.deal)
	if got == n.placeholder {
		t.Fatalf("the 3581 record carries %g, which is the PLACEHOLDER - the script's "+
			"number never reached the packet (row SklATK %g, script factor %g)",
			got, row.SklATK, n.factor)
	}

	log := logs.String()
	head := fmt.Sprintf(`zone: SKILL_SCRIPT slice 2: skill %d ("Fireball") char 1599513 "Warlock" - the script says `,
		capturedFireball)
	if !strings.Contains(log, head) {
		t.Fatalf("the cast wrote no slice-2 line for the script's answer; want %q in:\n%s",
			head, log)
	}
	if strings.Contains(log, "RAISED") {
		t.Fatalf("the script raised on a fixture cast, so this test proves nothing:\n%s", log)
	}
	line := strings.SplitN(log[strings.Index(log, head):], "\n", 2)[0]
	said := parseSkillScriptSay(t, line)
	assertNear(t, "the script's factor", said.factor, n.factor)
	assertNear(t, "the ATK the line names", said.atk, n.atk)
	assertNear(t, "the factor's damage at that ATK", said.deal, n.deal)
	assertNear(t, "the placeholder", said.placeholder, n.placeholder)
	// THE LINE AND THE PACKET MUST AGREE: the third number is what went out.
	assertNear(t, "the number the line says the packet carries", said.carried, got)
	if said.carried != got {
		t.Fatalf("the line says the packet carries %g and the 3581 record holds %g - "+
			"the line and the wire disagree about the same cast", said.carried, got)
	}
	// AND IT IS NOT THE PLACEHOLDER: that is the whole of slice 2.
	if said.carried == said.placeholder {
		t.Fatalf("the line's carried number equals its placeholder (%g) - the script "+
			"never moved the packet: %s", said.carried, line)
	}
	if said.deal == said.factor {
		t.Fatalf("the line's derived damage equals its factor (%g) - the ATK never "+
			"multiplied anything, so the factor is still being printed as a damage",
			said.factor)
	}
	// AND THE DEFENCE CLAUSE IS REAL, not decoration: the Scarecrow fixture has DEF 0,
	// so the rule's own branch answers 0% and the carried number IS the deal - a
	// non-zero percentage here would mean the rule ran on a defence nobody read.
	assertNear(t, "the target's DEF on the line", said.def, 0)
	assertNear(t, "the defence percentage on the line", said.percent, 0)
	assertNear(t, "the carried number against ATK x factor x (100-0)/100",
		said.carried, said.deal*(100-said.percent)/100)
	t.Logf("the ground cast's own line: %s", line)
}

// TestSkillScriptDecidesTheTargetedNumber is the same proof for the OTHER damage
// site: the aimed cast's 3573 +40. A test that only drove the ground one would
// leave half the wiring unproven.
func TestSkillScriptDecidesTheTargetedNumber(t *testing.T) {
	h, packets, logs := runTargetedCast(t, testDamageScript(t), testMonsterDefScript(t))

	row, known := h.Skills.Row(capturedSkill)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedSkill)
	}
	c := castPlayer().sess.Team[0]
	atk := h.sheetFor(c).ATK
	factor := scriptFactor(row, false) // an aimed cast: no SplDam term
	wantDeal := atk * factor           // the factor MULTIPLIES: no /100
	wantPlaceholder := castDamage(atk, row.SklATK, 0)

	melee := packetOfCmd(t, packets, CmdZCSkillMelee)
	got := math.Float64frombits(binary.LittleEndian.Uint64(melee[smDamage:]))
	assertNear(t, "the 3573 +40 damage", got, wantDeal)
	if got == wantPlaceholder {
		t.Fatalf("the 3573 +40 carries %g, which is the PLACEHOLDER - the script's "+
			"number never reached the packet (row SklATK %g, script factor %g)",
			got, row.SklATK, factor)
	}

	log := logs.String()
	head := `zone: SKILL_SCRIPT slice 2: skill 10561 ("Rapid Shot") char `
	if !strings.Contains(log, head) {
		t.Fatalf("the aimed cast wrote no slice-2 line; want %q in:\n%s", head, log)
	}
	if strings.Contains(log, "RAISED") {
		t.Fatalf("the script raised on the aimed fixture cast:\n%s", log)
	}
	line := strings.SplitN(log[strings.Index(log, head):], "\n", 2)[0]
	said := parseSkillScriptSay(t, line)
	assertNear(t, "the aimed line's factor", said.factor, factor)
	assertNear(t, "the aimed line's derived damage", said.deal, wantDeal)
	assertNear(t, "the aimed line's carried number", said.carried, got)
	// THE AIMED TARGET IS A DEF-0 FIXTURE TOO (`scarecrow()` carries no row, so its
	// Arm and DEF_BM are 0): the defence branch is the rule's own 0, and the carried
	// number is the deal untouched.
	assertNear(t, "the aimed target's DEF on the line", said.def, 0)
	assertNear(t, "the aimed defence percentage on the line", said.percent, 0)
	t.Logf("the aimed cast's own line: %s", line)
}

// skillSayLine is the numbers a success line prints, read out of the line with the
// shape it actually has - so a changed wording fails in ONE place, loudly.
type skillSayLine struct {
	factor      float64 // the script's own ATKRatio: a MULTIPLIER
	deal        float64 // what that factor is worth: ATK x factor (BEFORE the defence)
	atk         float64 // the caster's sheet ATK the deal was derived from
	placeholder float64 // `castDamage`'s number, the pre-R84 answer
	def         float64 // the target's own DEF, as SCR_Get_MON_DEF answered it
	percent     float64 // the defence PERCENTAGE the rule computed
	carried     float64 // the number the line says the packet carries
}

// parseSkillScriptSay reads the success line and requires it to be a SLICE 2 line, so
// a line that still promises "instrument, not switch" cannot parse as a success.
//
// The two tails it accepts are the rule's own: a target's DEF and the branch that
// decided the percentage, or (a ground cast at empty ground) the absence of a target.
func parseSkillScriptSay(t *testing.T, line string) skillSayLine {
	t.Helper()
	const marker = "zone: SKILL_SCRIPT slice 2: "
	if !strings.Contains(line, marker) {
		t.Fatalf("the line is not a slice-2 line (want %q in it): %s", marker, line)
	}
	const says = ` - the script says `
	i := strings.Index(line, says)
	if i < 0 {
		t.Fatalf("the slice-2 line never gets to the script's answer: %s", line)
	}
	var got skillSayLine
	// THE HEAD, which every slice-2 shape starts with.
	n, err := fmt.Sscanf(line[i+len(says):],
		"factor %g (= %g damage at ATK %g), the placeholder says %g,", &got.factor,
		&got.deal, &got.atk, &got.placeholder)
	if err != nil || n != 4 {
		t.Fatalf("the slice-2 line does not start with the factor, its damage, the ATK "+
			"and the placeholder (read %d of 4, err=%v): %s", n, err, line)
	}
	// THE TAIL, which is the target's defence and the rule's branch. A cast that
	// recorded no target says so instead, and prints the deal as the carried number.
	if strings.Contains(line, "recorded no target") {
		got.carried = numberAfter(t, line, "the packet carries ")
		return got
	}
	var def, percent float64
	var discardHandle int
	var discardName, discardBranch string
	n, err = fmt.Sscanf(line[strings.Index(line, "target "):],
		"target %d (%q) DEF %g -> defence %g%% by %q", &discardHandle, &discardName,
		&def, &percent, &discardBranch)
	if err != nil || n != 5 {
		t.Fatalf("the slice-2 line does not name the target's DEF and the defence "+
			"percentage (read %d of 5, err=%v): %s", n, err, line)
	}
	got.def, got.percent = def, percent
	got.carried = numberAfter(t, line, "the packet carries ")
	return got
}

// numberAfter reads the first number printed after `key`. The line is prose with
// numbers in it, so a positional Sscanf breaks the moment a clause is reworded;
// anchoring on the phrase the number belongs to does not.
func numberAfter(t *testing.T, line, key string) float64 {
	t.Helper()
	i := strings.Index(line, key)
	if i < 0 {
		t.Fatalf("the line has no %q, so the number it belongs to cannot be read: %s",
			key, line)
	}
	var v float64
	if _, err := fmt.Sscanf(line[i+len(key):], "%g", &v); err != nil {
		t.Fatalf("the text after %q is not a number (err=%v): %s", key, err, line)
	}
	return v
}

// assertNear compares two float64s that travelled through `%g`, which keeps six
// significant digits - so the tolerance is RELATIVE, and wide enough for that
// rounding while still being far too tight to hide a placeholder/deal mix-up
// (the two differ by a whole multiplier, not by a rounding step).
func assertNear(t *testing.T, what string, got, want float64) {
	t.Helper()
	tol := 1e-6 * math.Max(1, math.Abs(want))
	if math.Abs(got-want) > tol {
		t.Fatalf("%s is %g, want %g - the line, the packet and the cast disagree "+
			"about the same cast", what, got, want)
	}
}

// TestSkillScriptComputesTheRowsOwnFactor is the arithmetic half: the number the
// script answers is the formula in `attack.lua:359-365` applied to the row
// (`SklATK * ((SklLv + SklLv_BM) * 10 + 100) / 100`, times `SplDam` for a splash
// cast only), not a constant and not the row's bare SklATK - which is what makes
// it worth more than the placeholder.
func TestSkillScriptComputesTheRowsOwnFactor(t *testing.T) {
	ds := testDamageScript(t)
	h, _ := groundZone(t)
	row, known := h.Skills.Row(capturedFireball)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedFireball)
	}
	c := warlock().sess.Team[capturedGroundCharIndex]
	mon := areaScarecrow()

	said, ok, err := ds.Damage(c, row, &mon, true, 1)
	if err != nil || !ok {
		t.Fatalf("the damage script did not answer: ok=%v err=%v", ok, err)
	}
	want := scriptFactor(row, true) // the ground cast IS a splash: SplDam applies
	if math.Abs(said-want) > 1e-9 {
		t.Fatalf("the script answered %g, the row's own formula says %g "+
			"(SklATK %g, SklLv %g, SklLv_BM %g, SplDam %g)",
			said, want, row.SklATK, row.SklLv, row.SklLvBM, row.SplDam)
	}
	if said == row.SklATK {
		t.Fatalf("the script answered the row's bare SklATK %g - the skill-level "+
			"factor was never applied, so the run proves nothing", row.SklATK)
	}
	t.Logf("script %g vs the row's bare SklATK %g (the factor the placeholder uses)",
		said, row.SklATK)
}

// TestSkillScriptRaiseLeavesTheCastIntact: a script that RAISES must not cost the
// cast anything. The packet is the placeholder's, byte for byte, and the call
// itself does not sit and wait for the failure.
//
// The raising script is a throwaway corpus of one file, so the real one in the
// suite's runtime is never replaced: the index finds an entry point by its own
// `function SCR_...(` line, which is why the stub is written the way the
// translator emits it.
func TestSkillScriptRaiseLeavesTheCastIntact(t *testing.T) {
	_, plain, _ := runGroundCast(t, nil, nil)

	dir := t.TempDir()
	const raising = `function SCR_SKILL_ATTACK(self, target, skill, splash, ret, targetcount)
  error("the stub damage script always raises")
end
`
	if err := os.WriteFile(filepath.Join(dir, "attack.lua"), []byte(raising), 0o644); err != nil {
		t.Fatalf("write the stub corpus: %v", err)
	}
	host := script.New()
	defer host.Close()
	if _, err := host.Index(dir); err != nil {
		t.Fatalf("index the stub corpus: %v", err)
	}
	ds, err := NewDamageScript(host, t.Logf)
	if err != nil {
		t.Fatalf("NewDamageScript on the stub corpus: %v", err)
	}

	_, withScript, logs := runGroundCast(t, ds, nil)
	// THE WHOLE POINT: a raise leaves the wire exactly as it was with no script.
	samePackets(t, plain, withScript)

	log := logs.String()
	if !strings.Contains(log, "RAISED: ") {
		t.Fatalf("the raise was not reported - a broken script must be a line in "+
			"the log, not a silence:\n%s", log)
	}
	if !strings.Contains(log, "the packet carries the placeholder") {
		t.Fatalf("the raise line does not say the packet kept the placeholder:\n%s", log)
	}

	// "NOT DELAYED", measured on the CALL and not on the fixture: the corpus load
	// and the stance/skill tables a groundZone parses cost ~600 ms by themselves,
	// so timing the whole cast would measure the wrong thing. A raise returns at
	// once; the bar catches a script that hangs to its 3 s deadline.
	h, _ := groundZone(t)
	row, _ := h.Skills.Row(capturedFireball)
	c := warlock().sess.Team[capturedGroundCharIndex]
	mon := areaScarecrow()
	start := time.Now()
	_, ok, callErr := ds.Damage(c, row, &mon, true, 1)
	elapsed := time.Since(start)
	if ok || callErr == nil {
		t.Fatalf("the raising script answered: ok=%v err=%v", ok, callErr)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("the script call took %s to raise - a broken script is delaying the "+
			"cast", elapsed)
	}
	t.Logf("a raising script returns in %s, and the packet never moved", elapsed.Round(time.Microsecond))
}

// TestSkillScriptOffCarriesThePlaceholder is the state a checkout with no corpus
// is in: `h.Damage == nil` means the placeholder and NOT ONE line about a script.
// This is what keeps every pre-R84 test's numbers true.
func TestSkillScriptOffCarriesThePlaceholder(t *testing.T) {
	h, packets, logs := runGroundCast(t, nil, nil)
	_, n := groundNumbers(t, h)

	area := packetOfCmd(t, packets, CmdZCSkillArea)
	got := math.Float64frombits(binary.LittleEndian.Uint64(area[saRecords+arDamage:]))
	assertNear(t, "the 3581 record's damage with no script armed", got, n.placeholder)

	if strings.Contains(logs.String(), "SKILL_SCRIPT") {
		t.Fatalf("a zone with no damage script wrote a SKILL_SCRIPT line:\n%s", logs.String())
	}
}

// TestSkillScriptRecordsTheBuiltinsItAsksFor is the deliverable that slice 1 left
// behind, and slice 2 keeps it exactly as it was: the recorder names what THIS
// cast asked for and nothing answered.
//
// It asserts the RECORDER, not a list written down here: the names come from
// `Host.Missing()` after the run. The one name this test knows in advance is
// `GetSkillTargetPos`, which `attack.lua` asks for TWICE on every cast (:6 and
// :181) - measured by running it, not by reading the file.
func TestSkillScriptRecordsTheBuiltinsItAsksFor(t *testing.T) {
	var buf bytes.Buffer
	ds := testDamageScript(t)
	ds.Log = func(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) }

	h, _ := groundZone(t)
	row, _ := h.Skills.Row(capturedFireball)
	c := warlock().sess.Team[capturedGroundCharIndex]
	mon := areaScarecrow()

	before := ds.Host.Missing()
	if _, ok, err := ds.Damage(c, row, &mon, true, 1); err != nil || !ok {
		t.Fatalf("the damage script did not answer: ok=%v err=%v", ok, err)
	}

	// The recorder itself: the name is there, and the count is the one the SCRIPT
	// asked for - two call sites, so two calls, whatever the process had before.
	was := map[string]int{}
	for _, m := range before {
		was[m.Name] = m.Calls
	}
	var found bool
	for _, m := range ds.Host.Missing() {
		if m.Name != "GetSkillTargetPos" {
			continue
		}
		found = true
		if got := m.Calls - was[m.Name]; got != 2 {
			t.Fatalf("GetSkillTargetPos was recorded %d time(s) by this cast, want 2 "+
				"(attack.lua:6 and :181)", got)
		}
	}
	if !found {
		t.Fatalf("the run did not record GetSkillTargetPos; Host.Missing() = %v",
			ds.Host.Missing())
	}

	// And the ONE line, in the shape the slice asked for.
	out := buf.String()
	head := "zone: SKILL SCRIPT host: "
	if !strings.Contains(out, head) {
		t.Fatalf("no missing-builtin line was written; want %q in:\n%s", head, out)
	}
	if !strings.Contains(out, "missing builtin(s) that THIS cast asked for: ") {
		t.Fatalf("the missing-builtin line is not the slice's shape:\n%s", out)
	}
	if !strings.Contains(out, "GetSkillTargetPos x2") {
		t.Fatalf("the line does not carry the recorder's counts:\n%s", out)
	}
	line := out[strings.Index(out, head):]
	t.Logf("the worklist: %s", strings.SplitN(line, "\n", 2)[0])
}
