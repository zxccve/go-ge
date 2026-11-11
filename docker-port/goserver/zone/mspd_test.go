package zone

import (
	"bytes"
	"log"
	"math"
	"strings"
	"testing"
)

// THE WALK SPEED NOW INCLUDES THE WORN GEAR.
//
// SCR_Get_MSPD (ge\script\pc\get_status_pc.scp:1808, corpus copy
// out\lua_corpus\pc\get_status_pc.lua:1764) is
//
//	value = floor(469 * stance.StnMSPD * armor.MSPD * boots.MSPD * costume.MSPD
//	              * face.MSPD * back.MSPD * belt.MSPD * Achieve.MSPD * artifact.MSPD)
//	value = floor(value * (100 + self.MSPD_BM + achievefix) / 100)
//	then a cap: 650 default, 750 for a MELEE/HYBRID stance, per-stance overrides.
//
// The item terms are MULTIPLIERS (1 neutral, 0.95 slower, 1.05 faster), NOT
// percentages, and `data\item_stats.json` did not carry the column at all until
// `src\emit_item_stats.py` was widened. These tests pin the four cases that
// decide whether the number is right: a faster boot, two boots compounding, a
// slower boot, and the cap.
//
// WHY IT MATTERS: the swing's position is interpolated along the walk segment at
// this speed. All 235 measured swing positions sit exactly on that segment at the
// fraction this speed implies, so the MODEL is right and only the SPEED can be
// wrong - a body that really walks 5% faster than we assume is 5% ahead of every
// position we report, and every swing pulls it back.

// mspdStance is the stance every one of these tests walks in: Bareknuckle,
// StnMSPD 1, so the base 469 is the only stance term and every difference below
// is the item's.
const mspdStance = 10010

// mspdBoots19533 and mspdBoots19504 are REAL classes out of the emitted file.
// 19533 "SettlerShoes" is the owner's test boots (MSPD 1.05);
// 19504 "ChainBoots" is the slower pair (MSPD 0.95).
const (
	mspdBoots19533 = 19533
	mspdBoots19504 = 19504
)

// TestMSPDWornBootsRaiseTheWalk: the owner's boots carry MSPD 1.05, and
// 1.05 is a MULTIPLIER - floor(469 * 1 * 1.05) = 492, not 469. The same test
// pins that the value really comes out of the emitted item file and that the
// TERMS line is printed once per change and not once per packet.
func TestMSPDWornBootsRaiseTheWalk(t *testing.T) {
	table := speedTable(t)
	stats := loadTestItemStats(t)

	boots, ok := stats.For(mspdBoots19533)
	if !ok {
		t.Fatalf("item class %d has no row in item_stats.json - the walk speed "+
			"cannot see the boots the owner is wearing", mspdBoots19533)
	}
	if boots.MSPD != 1.05 {
		t.Fatalf("class %d MSPD = %g, want 1.05 - the emitted column or the "+
			"table changed, and this test would then prove nothing",
			mspdBoots19533, boots.MSPD)
	}

	// Barefoot first: the number the server has sent since R63.
	if got := table.MoveSpeedGear(mspdStance, 1, MSPDTerms{}); got != 469 {
		t.Fatalf("no items = %.0f, want the base 469", got)
	}

	// Now the boots. floor(469 * 1.05) = floor(492.45) = 492.
	if got := table.MoveSpeedGear(mspdStance, 1, MSPDTerms{Boots: boots.MSPD}); got != 492 {
		t.Fatalf("in MSPD 1.05 boots = %.0f, want 492 - floor(469 * 1 * 1.05). "+
			"A body that walks 5%% faster than we assume is 5%% ahead of every "+
			"position we report", got)
	}

	// THE SAME NUMBER ON THE REAL PATH: packetSpeed reads the worn slot through
	// wornGear, and its TERMS line is the one a reader greps.
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), Stances: table}
	c := Character{Handle: 0x40000031, Name: "owner", Job: 1, Stance: mspdStance}
	c.Equip[spotBoots] = mspdBoots19533

	if got := h.packetSpeed(&c); got != 492 {
		t.Fatalf("packetSpeed in class %d boots = %.0f, want 492", mspdBoots19533, got)
	}
	line := "zone: MSPD TERMS char 1073741873: 469 x stance 1 x item 1.05 -> 492, cap 750"
	if !strings.Contains(buf.String(), line) {
		t.Fatalf("the log does not carry the TERMS line\nwant a line containing:\n  %s\ngot:\n%s",
			line, buf.String())
	}
	// ONE LINE PER CHANGE, NOT PER PACKET: the same body walking again says
	// nothing new.
	h.packetSpeed(&c)
	if n := strings.Count(buf.String(), "MSPD TERMS"); n != 1 {
		t.Fatalf("two identical walks printed %d TERMS line(s), want 1 - the line "+
			"must follow the VALUE, not the packet:\n%s", n, buf.String())
	}
	// A real change does print again.
	c.Equip[spotBoots] = mspdBoots19504
	if got := h.packetSpeed(&c); got != 445 {
		t.Fatalf("packetSpeed after changing to class %d = %.0f, want 445",
			mspdBoots19504, got)
	}
	if n := strings.Count(buf.String(), "MSPD TERMS"); n != 2 {
		t.Fatalf("a change of gear printed %d TERMS line(s), want 2:\n%s", n, buf.String())
	}
}

// TestMSPDTwoItemsCompound: the eight categories MULTIPLY, they do not add.
// 1.05 x 1.10 = 1.155, and floor(469 * 1.155) = floor(541.695) = 541.
// Adding them (1.15) would answer 539 - two numbers a reader could not tell
// apart without this test.
func TestMSPDTwoItemsCompound(t *testing.T) {
	table := speedTable(t)

	items := MSPDTerms{Boots: 1.05, Back: 1.10}
	// A tolerance, because these are binary floats: 1.05 x 1.10 is
	// 1.1550000000000002, not 1.155.
	if got := items.Product(); math.Abs(got-1.155) > 1e-9 {
		t.Fatalf("1.05 x 1.10 = %v, want 1.155", got)
	}
	if got := table.MoveSpeedGear(mspdStance, 1, items); got != 541 {
		t.Fatalf("boots 1.05 with a back 1.10 = %.0f, want 541 (floor(469 * 1.155)); "+
			"539 would be the ADDeD version, which the script does not do", got)
	}
}

// TestMSPDBelowOneSlowsTheWalk: a multiplier under 1 is SLOWER, and the answer
// must follow it down. Class 19504 "ChainBoots" is 0.95: floor(469 * 0.95) =
// floor(445.55) = 445. A rule that clamped the item term at 1, or that treated
// the column as a percentage, gives 469 here.
func TestMSPDBelowOneSlowsTheWalk(t *testing.T) {
	table := speedTable(t)
	stats := loadTestItemStats(t)

	boots, ok := stats.For(mspdBoots19504)
	if !ok {
		t.Fatalf("item class %d has no row in item_stats.json", mspdBoots19504)
	}
	if boots.MSPD != 0.95 {
		t.Fatalf("class %d MSPD = %g, want 0.95", mspdBoots19504, boots.MSPD)
	}
	if got := table.MoveSpeedGear(mspdStance, 1, MSPDTerms{Boots: boots.MSPD}); got != 445 {
		t.Fatalf("in MSPD 0.95 boots = %.0f, want 445 - floor(469 * 0.95)", got)
	}
}

// TestMSPDCapAppliesToTheStack: the gear can push the walk past the cap, and the
// cap is what answers. 469 x 2.0 = 938, which is 650 for an ordinary stance and
// 750 for a MELEE one (get_status_pc.scp:1783-1832).
func TestMSPDCapAppliesToTheStack(t *testing.T) {
	// A stack worth 2.0 - two real terms, not a made-up stance: boots 1.25 x
	// back 1.6 = 2.0.
	items := MSPDTerms{Boots: 1.25, Back: 1.6}
	if got, want := items.Product(), 2.0; got != want {
		t.Fatalf("1.25 x 1.6 = %v, want %v", got, want)
	}

	magic := &StanceTable{rows: map[int]StanceRow{
		1: {Name: "TestMagic", ClassType: "MAGIC", StnMSPD: 1},
	}}
	if got := magic.MoveSpeedGear(1, 1, items); got != DefaultSpeedCap {
		t.Fatalf("a MAGIC stance at 938 = %.0f, want the 650 cap", got)
	}

	melee := &StanceTable{rows: map[int]StanceRow{
		1: {Name: "TestMelee", ClassType: "MELEE", StnMSPD: 1},
	}}
	if got := melee.MoveSpeedGear(1, 1, items); got != MeleeSpeedCap {
		t.Fatalf("a MELEE stance at 938 = %.0f, want the 750 cap", got)
	}
}
