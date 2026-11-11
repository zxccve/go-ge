package zone

// R84 SLICE 2 (CONTINUED) TESTS - the target's DEF and the rule that applies it.
//
// WHAT THESE PROVE, and nothing wider:
//
//	1 `SCR_Get_MON_DEF` RUNS for a real monster row and its answer is the quoted
//	  formula (monster\get_status_monster.lua:481-505) applied to that row - for the
//	  four classes the slice's table names: DEF 0, 40, 100 and 790;
//	2 the defence rule (common_monster.lua:2884-2896, quoted in zone\monster_def.go)
//	  turns those four DEFs into 0%, 3.5%, 33.5% and a 95% clamp at a fixed ATK, and
//	  `applyDefence` multiplies the damage by `(100 - defence)/100` - asserted per
//	  DELTA, so a rule that happened to be right at DEF 0 cannot pass;
//	3 the packet's number is `ATK x factor` with that defence applied, for a DEF-0 and
//	  a DEF-40 target;
//	4 every input this build cannot supply - no defence source, a defence script that
//	  raises - leaves the PLACEHOLDER on the wire and says so.
//
// The packet-level half lives in zone\skill_script_test.go (both damage sites, with
// both sources armed).

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"geserver/script"
)

// testMonsterDefScript is ONE MonsterDefScript per call, on the suite's own runtime.
func testMonsterDefScript(t *testing.T) *MonsterDefScript {
	t.Helper()
	requireStatus(t)
	if testScriptHost == nil {
		t.Skip("no script host in this checkout: build the corpus with " +
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus")
	}
	ds, err := NewMonsterDefScript(testScriptHost, t.Logf)
	if err != nil {
		t.Fatalf("NewMonsterDefScript: %v (SCR_Get_MON_DEF must be in the corpus - "+
			"rebuild it with cd src && python scp_to_lua.py --dump ../out/lua_corpus)", err)
	}
	return ds
}

// monsterOfRow is the Monster a spawn of that class produces: the SAME copy
// SpawnMonsterObject makes (zone\create.go), so a test prices the body a real spawn
// would have made instead of one typed in here.
func monsterOfRow(t *testing.T, h *Handler, class, handle uint32) Monster {
	t.Helper()
	if h.Monsters == nil {
		monsters, err := LoadMonsters(stanceDir())
		if err != nil {
			t.Skipf("monster table not readable (%v)", err)
		}
		h.Monsters = monsters
	}
	row, ok := h.Monsters.Row(class)
	if !ok {
		t.Fatalf("class %d is not a row of the Monster table - the row this test "+
			"prices is gone, so the defence columns it reads are gone with it", class)
	}
	return Monster{
		Handle: handle, X: 3160, Y: -1800, Cell: 55,
		ClassID: class, Level: int(row.Level), HP: int32(row.MHP),
		RaceType: row.RaceType, Arm: row.Arm, DEFBM: row.DEFBM,
		DEFPERBM: row.DEFPERBM, IMPBM: row.IMPBM, SizeType: row.SizeType,
	}
}

// monDefByHand is `SCR_Get_MON_DEF` written out in Go, line for line
// (monster\get_status_monster.lua:485-505):
//
//	value = math.floor((self.Lv * 0.05 + 1) * self.Arm)
//	value = value + self.DEF_BM
//	value = value * (100 + self.DEFPER_BM) / 100
//	if value < 0 then value = 0 end
//	if IsWorldPVPServer() == 1 then value = value * 0.75 end
//	return math.floor(value)
//
// It is the TEST's own copy on purpose: checking the script against the file that
// produced the same numbers would prove only that one function called itself.
func monDefByHand(m Monster) float64 {
	if m.RaceType == "Home" {
		return 9999 // the rule's own first branch, before any arithmetic
	}
	v := math.Floor((float64(m.Level)*0.05 + 1) * m.Arm)
	v += m.DEFBM
	v = v * (100 + m.DEFPERBM) / 100
	if v < 0 {
		v = 0
	}
	return math.Floor(v) // IsWorldPVPServer() is 0 in this build
}

// TestMonsterDefScriptMatchesTheRowsOwnFormula is the script half: the game's own entry
// point, run through the zone's host, answers the quoted formula for a real row - and
// the four classes the slice prices really are 0, 40, 100 and 790. The number is
// deliberately a PIN: if the shipped table moves one of these rows, the probe's log
// line and this test must be updated together.
func TestMonsterDefScriptMatchesTheRowsOwnFormula(t *testing.T) {
	ds := testMonsterDefScript(t)
	h, _ := groundZone(t)

	want := []struct {
		class uint32
		def   float64
	}{
		{11600, 0},    // Scarecrow: Lv 130, Arm 0 - the map's own dummies
		{10232, 40},   // small: Lv 30, Arm 16
		{100212, 100}, // mid: Lv 60, Arm 25
		{153538, 790}, // heavy: Lv 140, Arm 30, DEF_BM 550
	}
	for i, c := range want {
		m := monsterOfRow(t, h, c.class, MonsterHandle(0x100+i))
		got, ok, err := ds.Def(m)
		if err != nil || !ok {
			t.Fatalf("class %d: SCR_Get_MON_DEF did not answer: ok=%v err=%v",
				c.class, ok, err)
		}
		if byHand := monDefByHand(m); got != byHand {
			t.Fatalf("class %d: the script says DEF %g, the quoted formula says %g "+
				"(Lv %d, Arm %g, DEF_BM %g, DEFPER_BM %g, RaceType %q)",
				c.class, got, byHand, m.Level, m.Arm, m.DEFBM, m.DEFPERBM, m.RaceType)
		}
		if got != c.def {
			t.Fatalf("class %d: the script says DEF %g, the R84 slice-2 table says %g "+
				"- either the row changed or the table is stale; do not renumber this "+
				"test silently", c.class, got, c.def)
		}
		t.Logf("class %d: DEF %g (Lv %d, Arm %g, DEF_BM %g, DEFPER_BM %g, RaceType %q)",
			c.class, got, m.Level, m.Arm, m.DEFBM, m.DEFPERBM, m.RaceType)
	}

	// THE Home BRANCH IS 9999 BEFORE ANY ARITHMETIC, so it must answer that for a row
	// with a real Arm - otherwise "9999" could be arithmetic that happens to land there.
	home := Monster{ClassID: 1, Level: 100, Arm: 30, RaceType: "Home"}
	if got, ok, err := ds.Def(home); err != nil || !ok || got != 9999 {
		t.Fatalf("RaceType Home answered DEF %g (ok=%v err=%v), want 9999 - the rule's "+
			"first branch (get_status_monster.lua:486-487)", got, ok, err)
	}
}

// TestDefenceRuleIsTheQuotedExpression is the arithmetic half, with the ATK and the
// factor FIXED so every difference between two rows is the rule's:
//
//	defense = (MON_DEF - pierce - DefPierce - immune)/2 - AR/2
//	defense = defense * (100 - IgnoreDEFPer)/100
//	  < 0 -> 0 ;  > 95 (no ProtectionField) -> 95
//	damage  = ATK * ratio, then * (100 - defense)/100
//
// A rule that answered 0 for everything would pass a DEF-0 check alone, so the check
// that matters is the DELTA: what the damage DROPS when DEF goes 0 -> 40 -> 100 is the
// base times the percentage difference, and a flat subtraction (the placeholder's old
// `- DEF`) gives a different number for every one of them.
func TestDefenceRuleIsTheQuotedExpression(t *testing.T) {
	const atk, ratio, ar = 1365.5, 5.5, 33.0
	base := atk * ratio // the factor MULTIPLIES: no /100

	cases := []struct {
		def     float64
		percent float64
		branch  string
	}{
		{0, 0, "defense<0 (or IgnoreDef) -> 0"},
		{40, 3.5, "the formula"},
		{100, 33.5, "the formula"},
		{790, 95, "clamped to 95"},
	}
	var prevPercent, prevDamage float64
	for i, c := range cases {
		in := defenceInputs{MonsterDEF: c.def, AttackerAR: ar}
		percent, applied := in.percent()
		if math.Abs(percent-c.percent) > 1e-9 {
			t.Fatalf("DEF %g with AR %g gave defence %g%%, the quoted expression says "+
				"%g%% ((%g - %g)/2 = %g)", c.def, ar, percent, c.percent, c.def, ar,
				(c.def-ar)/2)
		}
		if applied != c.branch {
			t.Fatalf("DEF %g was decided by %q, want %q - the branch ORDER is the "+
				"reference's, and a reorder changes which rule is applied", c.def,
				applied, c.branch)
		}
		damage := applyDefence(base, percent)
		wantDamage := base * (100 - c.percent) / 100
		if math.Abs(damage-wantDamage) > 1e-9 {
			t.Fatalf("DEF %g at ATK %g x ratio %g gave %g damage, the quoted "+
				"expression says %g (%g x (100 - %g)/100)", c.def, atk, ratio, damage,
				wantDamage, base, c.percent)
		}
		if i > 0 {
			wantDrop := base * (c.percent - prevPercent) / 100
			if drop := prevDamage - damage; math.Abs(drop-wantDrop) > 1e-9 {
				t.Fatalf("DEF %g dropped the damage by %g from the previous row, the "+
					"rule says %g - a flat `- DEF` would not scale with the base at all",
					c.def, drop, wantDrop)
			}
		}
		prevPercent, prevDamage = c.percent, damage
		t.Logf("DEF %g -> defence %g%% (%s) -> %g damage (base %g), a drop of %g",
			c.def, percent, applied, damage, base, base-damage)
	}

	// THE PLAYER-ONLY PERCENT TERM. `arg.Attacker == 'player'` multiplies the defence
	// by (100 - IgnoreDEFPer)/100; the stance row is the source of that number, and the
	// branch must MOVE the percentage when it is set. DEF 100 is used rather than 790 so
	// the result stays below the rule's own 95 clamp - the clamp is an `elseif`, i.e. it
	// runs AFTER this term, and 790 would be 95 either way.
	in := defenceInputs{MonsterDEF: 100, AttackerAR: ar, IgnoreDEFPer: 50}
	if percent, _ := in.percent(); math.Abs(percent-(100.0-ar)/2*0.5) > 1e-9 {
		t.Fatalf("IgnoreDEFPer 50 gave defence %g%%, want (%g)/2 x 0.5 = %g - the "+
			"player percent term is not applied", percent, 100.0-ar, (100.0-ar)/2*0.5)
	}

	// AND THE ONE DETERMINATE PIERCE. 'Pierce' takes floor(MON_DEF x 0.1) off the DEF
	// before the halving (common_monster.lua:2470-2473); every other special type is
	// NOT applied by this lane, and that is said in the log line instead.
	pierce, pierced := pierceFor("Pierce", 790)
	if !pierced || pierce != 79 {
		t.Fatalf("Pierce on DEF 790 gave %g (pierced %v), want floor(790 x 0.1) = 79",
			pierce, pierced)
	}
	if _, other := pierceFor("SmallBane", 790); other {
		t.Fatalf("SmallBane was treated as a pierce term - only 'Pierce' is " +
			"determinate in this build")
	}

	// THE immune TERM: max(0, IgnoreDEF + DefIP - MON_IMP) is SUBTRACTED from the DEF
	// before the halving. The MONSTER's immunity makes the attacker's ignore-DEF worth
	// less, so 100 points of IgnoreDEF against a monster with IMP 40 is 60 points - and
	// the same DEF-100 target then defends at 3.5% instead of 33.5%.
	plain := defenceInputs{MonsterDEF: 100, AttackerAR: ar}
	withIgnore := defenceInputs{MonsterDEF: 100, AttackerAR: ar, SkillIgnoreDEF: 100,
		MonsterIMP: 40}
	before, _ := plain.percent()
	after, _ := withIgnore.percent()
	if math.Abs(before-33.5) > 1e-9 || math.Abs(after-3.5) > 1e-9 {
		t.Fatalf("IgnoreDEF 100 against IMP 40 gave defence %g%% (plain %g%%), want "+
			"3.5%% from (%g - max(0, 100 - 40))/2 - %g/2 - the immune term is not "+
			"subtracted before the halving", after, before, 100.0, ar)
	}
	// AND IT CANNOT GO NEGATIVE: a monster immunity ABOVE the attacker's ignore-DEF
	// points answers an immune term of 0, NOT a negative one that would ADD defence
	// back - a DEF-40 target with IMP 100 still defends at (40)/2 - AR/2 = 3.5%, not
	// ((40 + 100)/2 - AR/2) = 53.5%.
	over := defenceInputs{MonsterDEF: 40, AttackerAR: ar, MonsterIMP: 100}
	if percent, applied := over.percent(); math.Abs(percent-3.5) > 1e-9 ||
		applied != "the formula" {
		t.Fatalf("IMP 100 with no IgnoreDEF gave defence %g%% (%s), want 3.5%% - "+
			"max(0, ...) keeps the immune term at 0 instead of adding immunity back",
			percent, applied)
	}
}

// TestSkillScriptDamageWithoutADefenceSourceKeepsThePlaceholder is the "missing input"
// rule at its decision point: the damage script answers, but no defence source is
// armed, and 0 is the Scarecrow's REAL DEF - so an invented 0 would look like a
// measurement. The placeholder comes back and the line says why.
func TestSkillScriptDamageWithoutADefenceSourceKeepsThePlaceholder(t *testing.T) {
	h, logs := groundZone(t)
	h.Damage = testDamageScript(t)

	row, known := h.Skills.Row(capturedFireball)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedFireball)
	}
	c := warlock().sess.Team[capturedGroundCharIndex]
	mon := areaScarecrow()
	sheet := h.sheetFor(c)
	placeholder := castDamage(sheet.ATK, row.SklATK, 0)

	if got := h.skillScriptDamage(c, row, &mon, true, 1, placeholder, sheet); got != placeholder {
		t.Fatalf("with no defence source the packet's number is %g, want the "+
			"placeholder %g - a cast must never be priced from a DEF nobody read",
			got, placeholder)
	}
	log := logs.String()
	for _, want := range []string{
		"the target's DEF could not be read",
		"no monster defence source is armed",
		"the packet carries the placeholder",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("the fallback line does not say %q:\n%s", want, log)
		}
	}
}

// TestSkillScriptCarriesTheFactorAndTheTargetsDefence is the switch itself: with BOTH
// sources armed, the number the packet carries is `ATK x factor` with the quoted
// defence applied, for a DEF-0 and a DEF-40 target.
func TestSkillScriptCarriesTheFactorAndTheTargetsDefence(t *testing.T) {
	h, logs := groundZone(t)
	h.Damage = testDamageScript(t)
	h.Defence = testMonsterDefScript(t)

	row, known := h.Skills.Row(capturedFireball)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedFireball)
	}
	c := warlock().sess.Team[capturedGroundCharIndex]
	sheet := h.sheetFor(c)
	placeholder := castDamage(sheet.ATK, row.SklATK, 0)

	// The factor the SCRIPT answers, from the row's own numbers
	// (`attack.lua:359-365`), and the stance's own ignore-DEF percent - both read here
	// so the expected damage is derived, not copied from the line.
	factor := scriptFactor(row, true)
	ignorePer := 0.0
	if raw, ok := h.Stances.RawStance(c.Stance); ok {
		ignorePer = atof(raw["StnIgnoreDEFPer"])
	}

	var sawNonZero bool
	for _, class := range []uint32{11600, 10232, 153538} {
		logs.Reset()
		m := monsterOfRow(t, h, class, MonsterHandle(0x200+int(class)))
		got := h.skillScriptDamage(c, row, &m, false, 1, placeholder, sheet)

		def := monDefByHand(m)
		// THE immune TERM IS CLAMPED AT 0 (common_monster.lua:2433 ->
		// SCR_GET_BATTLE_POSITIVENUM): this build supplies no attacker DefIP and no
		// IGN_DEF, so it is max(0, skill.IgnoreDEF - MON_IMP) - a monster with more
		// immunity than the skill ignores answers 0, not a negative amount that would
		// ADD defence back.
		immune := math.Max(0, row.IgnoreDEF-monsterIMP(m))
		percent := (def-immune)/2 - sheet.AR/2
		percent = percent * (100 - ignorePer) / 100
		if percent < 0 {
			percent = 0
		} else if percent > 95 {
			percent = 95
		}
		want := applyDefence(sheet.ATK*factor, percent)
		if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Fatalf("class %d (DEF %g): the packet's number is %g, the quoted rules "+
				"give %g (ATK %g x factor %g, then x (100 - %g)/100)",
				class, def, got, want, sheet.ATK, factor, percent)
		}
		if def > 0 && got == placeholder {
			t.Fatalf("class %d (DEF %g): the packet still carries the placeholder "+
				"%g - the switch is not wired", class, def, placeholder)
		}
		sawNonZero = sawNonZero || def > 0

		line := logs.String()
		for _, literal := range []string{
			"zone: SKILL_SCRIPT slice 2: skill ",
			fmt.Sprintf("DEF %g", def),
			"the packet carries ",
		} {
			if !strings.Contains(line, literal) {
				t.Fatalf("class %d: the line does not name %q:\n%s", class, literal, line)
			}
		}
		t.Logf("class %d (DEF %g): defence %g%% -> %g, placeholder %g: %s", class, def,
			percent, got, placeholder, strings.SplitN(line, "\n", 2)[0])
	}
	// AT LEAST ONE TARGET HAD A REAL DEFENCE, or this test never exercised the rule.
	if !sawNonZero {
		t.Fatalf("both targets answered DEF 0 - the test never applied the defence rule")
	}
}

// TestMonsterDefRaiseKeepsThePlaceholder is the other missing input: the DEF source is
// armed but the game's own entry point RAISES. The cast keeps the placeholder and the
// line says which error it was.
func TestMonsterDefRaiseKeepsThePlaceholder(t *testing.T) {
	h, logs := groundZone(t)
	h.Damage = testDamageScript(t)

	dir := t.TempDir()
	const raising = `function SCR_Get_MON_DEF(self)
  error("the stub monster status script always raises")
end
`
	if err := os.WriteFile(filepath.Join(dir, "monster.lua"), []byte(raising), 0o644); err != nil {
		t.Fatalf("write the stub corpus: %v", err)
	}
	host := script.New()
	defer host.Close()
	if _, err := host.Index(dir); err != nil {
		t.Fatalf("index the stub corpus: %v", err)
	}
	mds, err := NewMonsterDefScript(host, t.Logf)
	if err != nil {
		t.Fatalf("NewMonsterDefScript on the stub corpus: %v", err)
	}
	h.Defence = mds

	row, known := h.Skills.Row(capturedFireball)
	if !known {
		t.Fatalf("skill %d is not in the skill table", capturedFireball)
	}
	c := warlock().sess.Team[capturedGroundCharIndex]
	mon := areaScarecrow()
	sheet := h.sheetFor(c)
	placeholder := castDamage(sheet.ATK, row.SklATK, 0)

	if got := h.skillScriptDamage(c, row, &mon, true, 1, placeholder, sheet); got != placeholder {
		t.Fatalf("a raising SCR_Get_MON_DEF left %g on the wire, want the placeholder "+
			"%g - a broken script must cost nothing but a log line", got, placeholder)
	}
	log := logs.String()
	for _, want := range []string{
		"the target's DEF could not be read",
		"the stub monster status script always raises",
		"the packet carries the placeholder",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("the raise line does not say %q:\n%s", want, log)
		}
	}
}

// TestMonsterDefScriptSerialisesOnOneHost is the same hazard DamageScript guards: the
// script host installs `self` for the length of one call, so two runs must never
// overlap. Ten goroutines on ONE source must all get the right class's DEF.
func TestMonsterDefScriptSerialisesOnOneHost(t *testing.T) {
	ds := testMonsterDefScript(t)
	h, _ := groundZone(t)

	cases := []uint32{11600, 10232, 100212, 153538}

	// The bodies are built BEFORE the goroutines start: `monsterOfRow` reports a missing
	// row with t.Fatalf, and a *testing.T must not be used from another goroutine.
	bodies := make([]Monster, 0, 40)
	for i := 0; i < 40; i++ {
		bodies = append(bodies, monsterOfRow(t, h, cases[i%len(cases)],
			MonsterHandle(0x300+i)))
	}

	var buf bytes.Buffer
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := i; j < len(bodies); j += 10 {
				m := bodies[j]
				got, ok, err := ds.Def(m)
				want := monDefByHand(m)
				if err != nil || !ok || got != want {
					buf.WriteString(fmt.Sprintf("class %d answered %g (ok=%v err=%v), "+
						"want %g\n", m.ClassID, got, ok, err, want))
				}
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	if buf.Len() > 0 {
		t.Fatalf("concurrent DEF reads crossed each other (the host's `self` is per "+
			"call, and one run must not see another's):\n%s", buf.String())
	}
}
