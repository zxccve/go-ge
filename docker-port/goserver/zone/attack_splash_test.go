package zone

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// THE SPLASH SWING - the live 3566 packets held against the builder, and the handler
// held against the ONE rule the capture measures about the geometry.
//
// The oracle is the owner's splash capture of 2026-11-01, cut into fixtures and never
// retyped (`docker-port\history\splash-attack.md`):
//
//	zone\testdata\splash_live_123.bin  123 B  3566, main type 3, count 0  (6 samples)
//	zone\testdata\splash_live_171.bin  171 B  3566, main type 3, count 1  (1 sample)
//
// The values every test below passes to the builder are the capture's own, written out
// as literals so that every byte compared is PROVED and not merely read back out of the
// packet the builder is being checked against: attacker 9766799 at (8665, 15630, 75),
// attack speed 250; the 123 sample's target 9764592 at (8233, 15412, 75), damage
// 24707589, HP before 6648; the 171 sample's target 9764281 at (8641, 15823, 75),
// damage 7923660, HP before 6163, its one victim 9759535 at (8623, 15931, 75), damage
// 20295658, HP before 6163.
//
// THE ALLOWANCE IS THE ATTACK TYPE, IN BOTH SAMPLES, AND IT IS ONE BYTE PER FIELD: this
// build writes splashMainAttackType (0) in the main block and splashVictimAttackType (0) in
// every extra record, while the reference's samples carry the flavours its own roll
// produced - 3 (CRITICAL_KD) in the 123 sample's main block, and 2 (KNOCKDOWN) main with 3
// (CRITICAL_KD) in the 171 sample's one record.
//
// THIS ALLOWANCE USED TO BE SMALLER, ON PURPOSE, AND THAT IS THE POINT OF THE CHANGE: the
// 171 sample was once compared with NO allowance at all, because this zone wrote the
// capture's own 2 and 3 there - a byte-perfect copy. The owner's retest still showed one
// flying number, so a faithful copy of those two fields cannot be the fix and they are now
// the PROBE: 0/0. The layout is still held byte for byte everywhere else, which is what
// makes the two differing bytes mean something.

// The two measured totals, written as the numbers the decode states rather than as an
// expression over the builder's constants, so a drift in either direction shows up.
const (
	splashNoVictimSize  = 123 // 6 of the 16 samples
	splashOneVictimSize = 171 // 1 sample
)

// TestBuildAttackSplashRebuildsTheLive123 is the 123-byte sample (count 0): the base
// block, the zero count at +74 and the whole 48-byte unused record, byte for byte.
func TestBuildAttackSplashRebuildsTheLive123(t *testing.T) {
	p := attackFixture(t, "testdata/splash_live_123.bin", CmdZCAttackSplash,
		splashNoVictimSize)

	const (
		attacker = 9766799
		target   = 9764592
	)
	ours := BuildAttackSplash(attacker, [3]int32{8665, 15630, 75}, 250, target,
		[3]int32{8233, 15412, 75}, 24707589.0, 6648.0, splashMainAttackType, nil)

	if len(ours) != splashNoVictimSize {
		t.Fatalf("our 3566 with no extra victim is %d bytes, want %d (measured: 6 "+
			"samples)", len(ours), splashNoVictimSize)
	}
	if got := binary.LittleEndian.Uint16(ours[2:]); int(got) != splashNoVictimSize {
		t.Fatalf("our 3566 length word at +2 = %d, want %d", got, splashNoVictimSize)
	}
	if got := ours[AttackSplashCount]; got != 0 {
		t.Fatalf("+%d count byte = %d with no extra victim, want 0 - the count does NOT "+
			"include the main hit", AttackSplashCount, got)
	}
	// THE TRAILING 48 BYTES ARE THE UNUSED RECORD, and they are ALL ZERO in the capture
	// (measured 16 of 16, cause unknown - the builder writes them, it does not skip them).
	tail := p[AttackSplashFirst:]
	if len(tail) != SplashVictimSize {
		t.Fatalf("the 123 sample carries %d byte(s) after +%d, want exactly one %d-byte "+
			"record", len(tail), AttackSplashFirst, SplashVictimSize)
	}
	for i, b := range tail {
		if b != 0 {
			t.Fatalf("the unused record's byte +%d is 0x%02x in the capture, want 0",
				AttackSplashFirst+i, b)
		}
		if ours[AttackSplashFirst+i] != 0 {
			t.Errorf("our unused record's byte +%d is 0x%02x, want 0 - it is written, not "+
				"left to the allocator", AttackSplashFirst+i, ours[AttackSplashFirst+i])
		}
	}

	// THE ALLOWANCE IS REAL, checked on the fixture itself before it is used.
	if got := int32(binary.LittleEndian.Uint32(p[asAttackType:])); got != 3 {
		t.Fatalf("the capture's main attack type at +%d is %d, want 3 (CRITICAL_KD) - the "+
			"reference's own crit roll, and the value this test allows one byte for",
			asAttackType, got)
	}
	if got := int32(binary.LittleEndian.Uint32(ours[asAttackType:])); got != splashMainAttackType {
		t.Fatalf("our +%d type = %d, want splashMainAttackType %d - the splash's own type, "+
			"which is NOT the plain 3564's plainAttackType", asAttackType, got,
			splashMainAttackType)
	}

	matched, allow, unexplained := 0, 0, 0
	for i := range p {
		if p[i] == ours[i] {
			matched++
			continue
		}
		if splashDiffAllowed(i) == "" {
			unexplained++
			t.Errorf("3566 byte +%d differs: capture 0x%02x, builder 0x%02x - and it is "+
				"NOT the attack-type allowance (+%d..+%d)", i, p[i], ours[i], asAttackType,
				asKnockPos-1)
			continue
		}
		allow++
	}
	t.Logf("%d of %d bytes identical, %d allowance byte(s) (attack type only), %d "+
		"unexplained", matched, len(p), allow, unexplained)
	// ONE byte, not four: the capture's 3 and this zone's 2 differ in the low byte of the
	// int32 and the other three are zero on both sides.
	if unexplained != 0 || allow != 1 {
		t.Fatalf("the builder must reproduce the live 123-byte 3566 exactly when it is "+
			"given the capture's own values, except for the attack type at +%d..+%d: "+
			"%d of %d identical, %d allowance byte(s), %d unexplained", asAttackType,
			asKnockPos-1, matched, len(p), allow, unexplained)
	}
}

// TestBuildAttackSplashRebuildsTheLive171 is the 171-byte sample (count 1): one 48-byte
// victim record after the count, then the unused all-zero record.
//
// THIS SAMPLE IS THE HARDEST EVIDENCE IN THE TREE AND IT IS WHY THE TYPES ARE THE PROBE:
// this zone used to write the capture's own 2 (main) and 3 (victim) here, and this test
// compared all 171 bytes with NO allowance at all - so the packet it sent the client was
// the live packet, byte for byte. The extra body's flying number still did not draw. So the
// two type fields now carry splashMainAttackType / splashVictimAttackType (0/0) and this
// test allows exactly those two bytes and nothing else.
func TestBuildAttackSplashRebuildsTheLive171(t *testing.T) {
	p := attackFixture(t, "testdata/splash_live_171.bin", CmdZCAttackSplash,
		splashOneVictimSize)

	const (
		attacker  = 9766799
		target    = 9764281
		victim    = 9759535
		victimHP  = 6163.0
		victimDmg = 20295658.0
	)
	victims := []SplashVictim{{
		Handle: victim, Pos: [3]int32{8623, 15931, 75}, Damage: victimDmg,
		HPBefore: victimHP, AttackType: splashVictimAttackType,
	}}
	ours := BuildAttackSplash(attacker, [3]int32{8665, 15630, 75}, 250, target,
		[3]int32{8641, 15823, 75}, 7923660.0, 6163.0, splashMainAttackType, victims)

	if len(ours) != splashOneVictimSize {
		t.Fatalf("our 3566 with one extra victim is %d bytes, want %d (measured: 1 "+
			"sample)", len(ours), splashOneVictimSize)
	}
	if got := binary.LittleEndian.Uint16(ours[2:]); int(got) != splashOneVictimSize {
		t.Fatalf("our 3566 length word at +2 = %d, want %d", got, splashOneVictimSize)
	}
	if got := ours[AttackSplashCount]; got != 1 {
		t.Fatalf("+%d count byte = %d with one extra victim, want 1", AttackSplashCount,
			got)
	}
	// The count's own record is a handle that is NOT the main target - that is how the
	// decode proved the count excludes the main hit.
	if got := binary.LittleEndian.Uint32(ours[AttackSplashFirst+svHandle:]); got != victim {
		t.Errorf("the victim record's handle = %d, want the capture's %d", got, victim)
	}
	if got := binary.LittleEndian.Uint32(ours[AttackSplashFirst+svHandle:]); got == target {
		t.Errorf("the first victim record carries the MAIN target's handle (%d) - the +%d "+
			"count excludes the main hit", got, AttackSplashCount)
	}
	// The unused record after the real one: 48 bytes of zero, both sides.
	for i := 0; i < SplashVictimSize; i++ {
		off := AttackSplashFirst + SplashVictimSize + i
		if p[off] != 0 {
			t.Fatalf("the unused record's byte +%d is 0x%02x in the capture, want 0", off,
				p[off])
		}
		if ours[off] != 0 {
			t.Errorf("our unused record's byte +%d is 0x%02x, want 0", off, ours[off])
		}
	}

	// THE MAIN TYPE IN THE CAPTURE IS 2 (KNOCKDOWN), and that is the reference's own
	// flavour, not this build's: ours is splashMainAttackType (0) and the one byte that
	// differs is allowed below, by name.
	const captureMainType, captureVictimType = int32(2), int32(3)
	if got := int32(binary.LittleEndian.Uint32(p[asAttackType:])); got != captureMainType {
		t.Fatalf("the capture's main attack type at +%d is %d, want %d - THIS sample's main "+
			"hit is the reference's KNOCKDOWN (the decode reads (171, 2, 1))", asAttackType,
			got, captureMainType)
	}
	if got := int32(binary.LittleEndian.Uint32(ours[asAttackType:])); got != splashMainAttackType {
		t.Fatalf("our main block's type at +%d is %d, want splashMainAttackType %d - the "+
			"extra record's type must not spread to the main hit", asAttackType, got,
			splashMainAttackType)
	}
	// THE EXTRA RECORD'S TYPE IS 3 IN THE CAPTURE - the reference's CRITICAL_KD, the ONLY
	// measured value for that field - and splashVictimAttackType (0) in ours. This is the
	// field the client's extra-victim path reads to decide whether it draws the flying
	// number, so it is the one byte the retest is really about.
	if got := int32(binary.LittleEndian.Uint32(
		p[AttackSplashFirst+svAttackType:])); got != captureVictimType {
		t.Fatalf("the capture's victim attack type at +%d is %d, want %d (CRITICAL_KD)",
			AttackSplashFirst+svAttackType, got, captureVictimType)
	}
	if got := int32(binary.LittleEndian.Uint32(
		ours[AttackSplashFirst+svAttackType:])); got != splashVictimAttackType {
		t.Fatalf("our victim record's type at +%d is %d, want splashVictimAttackType %d - "+
			"the capture states %d", AttackSplashFirst+svAttackType, got,
			splashVictimAttackType, captureVictimType)
	}

	// EVERY OTHER BYTE IS THE CAPTURE'S. The allowance is the two type fields and nothing
	// else - one byte each, because 2/3 and 0/0 share their three high zero bytes - so this
	// still proves the 48-byte record's layout, the count and the length word.
	matched, allow, unexplained := 0, 0, 0
	for i := range p {
		if p[i] == ours[i] {
			matched++
			continue
		}
		why := splashDiffAllowed(i)
		if why == "" {
			unexplained++
			t.Errorf("3566 byte +%d differs: capture 0x%02x, builder 0x%02x - and it is "+
				"NOT one of the two attack-type fields", i, p[i], ours[i])
			continue
		}
		allow++
		t.Logf("allowed difference at +%d: %s (capture 0x%02x, builder 0x%02x)", i, why,
			p[i], ours[i])
	}
	if unexplained != 0 || allow != 2 {
		t.Fatalf("the builder must reproduce the live 171-byte 3566 when it is given the "+
			"capture's own values, except for the two attack-type fields (main and the one "+
			"record's): %d of %d identical, %d allowance byte(s), %d unexplained",
			matched, len(p), allow, unexplained)
	}
	t.Logf("%d of %d bytes identical, %d allowance byte(s) (the two attack types)", matched,
		len(p), allow)
}

// TestSplashTypeFieldsAreWrittenFromSeparateValues keeps the LAW the retired
// "the extra victim's type is not the main hit's" test stood on, without the numbers that
// test used to state. It asserted splashVictimAttackType == 3 and != plainAttackType; both
// fields are 0 now (the probe), so those two assertions could only be deleted - but the
// trap they caught must not come back: the builder takes ONE main type argument and ONE
// type per record, and writing either from the other is what made both bodies hurt while
// one number drew.
//
// SO IT IS PROVED BY CONSTRUCTION, NOT BY THE CURRENT VALUES: give the builder a main type
// and a victim type that CANNOT be confused (5 = NO_EFFECT, 4 = BUFF_SUCCESS - values this
// zone never sends) and read both back out of the packet. A builder that took a record's
// type from its main argument, or the main block's from the first victim, fails here
// whatever the live values happen to be.
func TestSplashTypeFieldsAreWrittenFromSeparateValues(t *testing.T) {
	const mainType, victimType = int32(5), int32(4)
	pkt := BuildAttackSplash(9766799, [3]int32{8665, 15630, 75}, 250, 9764281,
		[3]int32{8641, 15823, 75}, 7923660.0, 6163.0, mainType,
		[]SplashVictim{{
			Handle: 9759535, Pos: [3]int32{8623, 15931, 75}, Damage: 20295658.0,
			HPBefore: 6163.0, AttackType: victimType,
		}})

	if got := int32(binary.LittleEndian.Uint32(pkt[asAttackType:])); got != mainType {
		t.Errorf("the MAIN block states type %d at +%d, want the argument's %d - the main "+
			"block is written from the argument, never from a victim record", got,
			asAttackType, mainType)
	}
	if got := int32(binary.LittleEndian.Uint32(
		pkt[AttackSplashFirst+svAttackType:])); got != victimType {
		t.Errorf("the victim record states type %d at +%d, want its own %d - a record's "+
			"type is the record's, never the main hit's", got,
			AttackSplashFirst+svAttackType, victimType)
	}
}

// TestSplashPacketWritesTheNormalAttackType is the ONE VARIABLE of the owner's retest,
// checked on a packet THIS SERVER actually sends rather than on a builder call handed the
// capture's own values: a live splash with TWO extras must state type 0 (ATTACKTYPE_NORMAL)
// in the main block at +58 and in EVERY extra record at +32.
//
// WHY THE TYPE, AND WHY 0: on the live server the owner sees a flying number over EVERY
// monster a splash hits. This zone's packet instead stated 2 (KNOCKDOWN) on the main hit
// and 3 (CRITICAL_KD) in each extra record, and the client's CNormalNet::AttackSplash
// branches on exactly these two fields: the main type decides whether a 37-byte
// KNOCKDOWN_INFO is consumed before the victim loop, and each record's type decides which
// AddSplashTgt call draws that body's number. Type 0 takes the plain path on both, with no
// KD block to consume - so the count at +74 stays where this builder writes it and the
// records are parsed. The layout is deliberately untouched: still the 12-byte knock-back
// position at +62, still 48-byte records, still the count at +74.
//
// THIS TEST IS THE PROBE, NOT A PROOF: what the client DRAWS is settled only by the
// owner's retest in game. If one number still draws with 0/0, the next variable is
// attackSpd (250 in the live samples), not another layout change.
func TestSplashPacketWritesTheNormalAttackType(t *testing.T) {
	const wantMain, wantVictim = int32(0), int32(0)

	h, logs, p := splashZone(t)
	first := splashExtra(t, h, 0x20003007, scarecrow().X+100, scarecrow().Y, 1e9)
	second := splashExtra(t, h, 0x20003008, scarecrow().X, scarecrow().Y+100, 1e9)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit, count, hps := splashReplyShape(t, out)
	if count != 2 {
		t.Fatalf("the 3566 states %d extra victim(s), want 2 - the question is what EVERY " +
"record carries, so one record is not enough:\n%s", count, logs.String())
	}
	if hps != 3 {
		t.Fatalf("%d 3557(s), want 3 (the main target and the two extras)", hps)
	}

	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != wantMain {
		t.Errorf("the MAIN block's type at +%d is %d, want %d (ATTACKTYPE_NORMAL) - a "+
			"KNOCKDOWN/CRITICAL_KD main type makes the client consume a 37-byte KD block "+
			"before the victim loop, which is not in this packet", asAttackType, got,
			wantMain)
	}
	for i := 0; i < count; i++ {
		rec := hit[AttackSplashFirst+i*SplashVictimSize:]
		handle := binary.LittleEndian.Uint32(rec[svHandle:])
		if handle != first && handle != second {
			t.Fatalf("victim record %d names handle %d, want one of the two extras (%d, "+
				"%d)", i, handle, first, second)
		}
		if got := int32(binary.LittleEndian.Uint32(rec[svAttackType:])); got != wantVictim {
			t.Errorf("victim record %d (handle %d) states type %d at +%d, want %d "+
				"(ATTACKTYPE_NORMAL) - this is the field AddSplashTgt reads to draw that "+
				"body's flying number", i, handle, got,
				AttackSplashFirst+i*SplashVictimSize+svAttackType, wantVictim)
		}
	}
	t.Logf("%d B, count %d, main type %d, every extra record type %d", len(hit), count,
		binary.LittleEndian.Uint32(hit[asAttackType:]),
		binary.LittleEndian.Uint32(hit[AttackSplashFirst+svAttackType:]))
}

// TestBuildAttackSplashCapsTheCountByteAt255 is the COUNT BYTE's own ceiling, and the one
// way a caller could make a 3566 disagree with itself.
//
// +74 is ONE BYTE (AttackSplashCount), so it can say 0..255 and nothing else, while the
// length word at +2 is a WORD (65535). A builder that truncated to the LENGTH word's
// ceiling and then wrote `byte(len(victims))` would send 1362 records behind a count that
// wrapped: 300 victims would state 44 and the client would read 44 bodies out of a packet
// carrying 1362. THE LIVE PATH CANNOT REACH IT TODAY - Handler.splashVictims asks for at
// most maxAreaTargets (224) bodies, so the count never exceeds 224 - but the builder is a
// function any caller may use, so its own ceiling has to be the smaller of the two
// numbers: the count byte's 255.
//
// 300 victims is chosen because it is one over the byte's range and wraps to a count a
// reader would believe (44). The builder must answer 255 records + the one all-zero
// record, with a length word that still equals the length of the slice it returned.
func TestBuildAttackSplashCapsTheCountByteAt255(t *testing.T) {
	const (
		attacker = 9766799
		target   = 9764592
		asked    = 300 // victims passed in: 300 % 256 = 44, which is the wrap this test forbids
		keep     = 255 // the count byte's own ceiling
	)
	// An Errorf, not a Fatalf: the count-byte assertion below is the BEHAVIOUR, and a run
	// that fails here should still show it - the two lines together are the whole defect.
	if maxSplashVictims != keep {
		t.Errorf("maxSplashVictims is %d, want %d - the builder's ceiling is the COUNT "+
			"BYTE's (one byte at +%d), not the length word's 1362: a count byte cannot "+
			"name more than %d bodies", maxSplashVictims, keep, AttackSplashCount, keep)
	}

	victims := make([]SplashVictim, asked)
	for i := range victims {
		victims[i] = SplashVictim{
			Handle:     0x30000000 + uint32(i),
			Pos:        [3]int32{8623 + int32(i), 15931, 75},
			Damage:     float64(1000 + i),
			HPBefore:   float64(6000 + i),
			AttackType: plainAttackType,
		}
	}

	pkt := BuildAttackSplash(attacker, [3]int32{8665, 15630, 75}, 250, target,
		[3]int32{8641, 15823, 75}, 7923660.0, 6163.0, plainAttackType, victims)

	// THE LENGTH WORD AND THE SLICE STILL AGREE: whatever the count byte says, the packet
	// must be as long as it claims, or the client cuts it where +2 tells it to.
	if got := int(binary.LittleEndian.Uint16(pkt[2:])); got != len(pkt) {
		t.Fatalf("the length word at +2 is %d but the packet is %d bytes - a truncated "+
			"list must still state its own length", got, len(pkt))
	}
	// THE COUNT BYTE IS THE CEILING, not 300 % 256. Asked FIRST, because this is the
	// defect: a wrapped count in a packet that still carries the records.
	if got := int(pkt[AttackSplashCount]); got != keep {
		t.Fatalf("the count byte at +%d is %d for %d victims, want %d - the count byte "+
			"WRAPPED while the packet still carries the records (%d %% 256 = %d), so the "+
			"packet disagrees with its own count", AttackSplashCount, got, asked, keep,
			asked, asked%256)
	}

	if want := AttackSplashFirst + SplashVictimSize*(keep+1); len(pkt) != want {
		t.Fatalf("a %d-victim call produced %d bytes, want %d (75 + 48 x (%d + 1): %d "+
			"records plus the one all-zero record)", asked, len(pkt), want, keep, keep)
	}

	// AND THE RECORDS BEHIND IT ARE EXACTLY THAT MANY, in order, so nothing was written
	// past the count the client will read.
	for i := 0; i < keep; i++ {
		off := AttackSplashFirst + i*SplashVictimSize
		if got, want := binary.LittleEndian.Uint32(pkt[off+svHandle:]),
			uint32(0x30000000+i); got != want {
			t.Fatalf("victim record %d carries handle %d, want %d (the caller's order)", i,
				got, want)
		}
	}
	// THE LAST 48 BYTES ARE THE UNUSED, ALL-ZERO RECORD - still there, still zero.
	tail := pkt[AttackSplashFirst+keep*SplashVictimSize:]
	if len(tail) != SplashVictimSize {
		t.Fatalf("%d byte(s) after the %d victim records, want one %d-byte record", len(tail),
			keep, SplashVictimSize)
	}
	for i, b := range tail {
		if b != 0 {
			t.Fatalf("the unused record's byte +%d is 0x%02x, want 0",
				AttackSplashFirst+keep*SplashVictimSize+i, b)
		}
	}
	t.Logf("%d victims asked for, %d declared and carried, %d bytes, length word %d - no wrap",
		asked, keep, len(pkt), binary.LittleEndian.Uint16(pkt[2:]))
}

// splashDiffAllowed answers why a differing byte offset in a 3566 is EXPECTED to differ,
// or "" when the builder must reproduce the capture there. Two ranges, both the client's
// ATTACKTYPE_* field and nothing else:
//
//	+58..+61              the MAIN block's type - the reference rolls criticals, this zone
//	                      does not and states splashMainAttackType (0)
//	each record's +32..35 the EXTRA victim's type - the reference's own measured flavour
//	                      was 3, this build states splashVictimAttackType (0)
//
// The record range is expressed as an offset INTO a 48-byte record so it cannot drift when
// the count changes; it covers the all-zero unused record too, where both sides are zero,
// so it never counts as an allowance in a packet that has no extra victim.
func splashDiffAllowed(off int) string {
	if off >= asAttackType && off < asKnockPos {
		return "main attack type"
	}
	if off >= AttackSplashFirst {
		if r := (off - AttackSplashFirst) % SplashVictimSize; r >= svAttackType &&
			r < svKnockPos {
			return "victim attack type"
		}
	}
	return ""
}

// splashStance is the stance the one live splash capture belongs to: 120039
// "Disablering", AtkType "Splash", SplType "Circle", SplRange 220, MinR 0 .. MaxR 1050.
// Every test below that presses in a splash stance uses this id, and splashZone fails
// loudly when the row it is written against is not in the table.
const splashStance = uint32(120039)

// splashZone is attackZone with the character standing in the measured splash stance, at
// the fixture's in-reach stand point (100 units from the Scarecrow, well inside MaxR
// 1050 and over MinR 0).
//
// THE MONSTER'S LEDGER IS RAISED: the stance's StnATK is 4.8, so its plain hit is worth
// far more than the fixture's 5000 HP, and a swing after the first would then end the
// repeat for the right reason (an empty body) but the wrong question. 1e9 keeps the
// Scarecrow standing through every swing these tests ask for, and it is a number the
// test states out loud rather than one hidden in the fixture.
func splashZone(t *testing.T) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZoneAt(t, splashStance, attackReachStand)
	row := h.Stances.Combat(splashStance)
	if row.AtkType != "Splash" || row.SplRange <= 0 {
		t.Fatalf("stance %d reads AtkType %q, SplRange %v in this tree's "+
			"datatable_stance.xml - the splash row these tests are written against is "+
			"missing", splashStance, row.AtkType, row.SplRange)
	}
	h.monsterHP[capturedTarget] = 1e9
	return h, logs, p
}

// splashExtra puts ONE extra monster in the zone with an HP in the ledger and answers
// its handle. It is written as a helper because four tests need exactly this body and
// none of them may let the bookkeeping drift from the others.
func splashExtra(t *testing.T, h *Handler, handle uint32, x, y int32, hp float64) uint32 {
	t.Helper()
	h.spawnMu.Lock()
	h.spawnedMonster = append(h.spawnedMonster,
		Monster{Handle: handle, X: x, Y: y, Cell: scarecrow().Cell, ClassID: 11600,
			Level: 1})
	h.monsterHP[handle] = hp
	h.spawnMu.Unlock()
	return handle
}

// splashMoveExtra re-points one extra body, which is how a test asks "what if it walked
// out of the circle before the swing".
func splashMoveExtra(h *Handler, handle uint32, x, y int32) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == handle {
			h.spawnedMonster[i].X, h.spawnedMonster[i].Y = x, y
			return
		}
	}
}

// splashReplyShape checks one reply against the measured shape and hands back the 3566,
// its count byte and the number of 3557s. It fails when the reply holds a SECOND hit
// packet: one press is one hit, and two 3566s in one reply would be two hits.
func splashReplyShape(t *testing.T, out [][]byte) (hit []byte, count, hps int) {
	t.Helper()
	if len(out) == 0 {
		t.Fatal("a splash press produced no packet at all")
	}
	hit = out[0]
	if cmd := cmdOf(hit); cmd != CmdZCAttackSplash {
		t.Fatalf("the first packet is cmd %d, want %d ZC_ATTACK_SPLASH", cmd,
			CmdZCAttackSplash)
	}
	count = int(hit[AttackSplashCount])
	if want := AttackSplashFirst + SplashVictimSize*(count+1); len(hit) != want {
		t.Fatalf("the 3566 is %d bytes with count %d, want %d (75 + 48*(N+1))", len(hit),
			count, want)
	}
	for _, pkt := range out[1:] {
		switch cmdOf(pkt) {
		case CmdZCAddHP:
			hps++
		case CmdZCAttackSplash, CmdZCAttackSingle:
			t.Fatalf("a second hit packet (cmd %d) is in the same reply - one press is "+
				"one hit", cmdOf(pkt))
		}
	}
	return hit, count, hps
}

// TestSplashVictimsAreCentredOnTheMainTarget is the geometry, with the capture's own
// numbers: caster (8665, 15630, 75), main target (8641, 15823, 75), one extra victim at
// (8623, 15931, 75).
//
// The extra is ~109.5 units from the MAIN TARGET and ~303.9 from the CASTER, and the
// stance's SplRange is 220 - so a circle drawn around the caster would NOT contain it.
// That is the whole reason splashVictims is asked about the main target, and this test
// goes red the moment anyone measures the radius from the character instead.
func TestSplashVictimsAreCentredOnTheMainTarget(t *testing.T) {
	h, logs, _ := attackZone(t)

	const (
		casterX, casterY = 8665, 15630
		mainX, mainY     = 8641, 15823
		extraX, extraY   = 8623, 15931
	)
	main := Monster{Handle: 9764281, X: mainX, Y: mainY, Cell: 75, ClassID: 11600, Level: 1}
	extra := Monster{Handle: 9759535, X: extraX, Y: extraY, Cell: 75, ClassID: 11600, Level: 1}
	h.spawnMu.Lock()
	h.spawnedMonster = []Monster{main, extra}
	h.monsterHP = map[uint32]float64{main.Handle: 6163, extra.Handle: 6163}
	h.spawnMu.Unlock()

	radius := h.Stances.Combat(splashStance).SplRange
	if radius != 220 {
		t.Fatalf("stance %d states SplRange %v, want the capture's 220 - this test is "+
			"written against the measured row", splashStance, radius)
	}

	toMain := math.Hypot(float64(extraX-mainX), float64(extraY-mainY))
	toCaster := math.Hypot(float64(extraX-casterX), float64(extraY-casterY))
	if math.Abs(toMain-109.5) > 0.1 {
		t.Fatalf("the extra body is %g from the main target, want ~109.5 (the capture's "+
			"own geometry)", toMain)
	}
	if math.Abs(toCaster-303.9) > 0.1 {
		t.Fatalf("the extra body is %g from the caster, want ~303.9", toCaster)
	}

	victims, addHP, capped := h.splashVictims(main, radius, 1000, plainAttackType, 0, 0)
	if capped {
		t.Errorf("two bodies capped a %d-body area", maxAreaTargets)
	}
	if len(victims) != 1 || victims[0].Handle != extra.Handle {
		t.Fatalf("%d victim(s) %v inside %g of the main target, want exactly the extra "+
			"%d:\n%s", len(victims), splashHandles(victims), radius, extra.Handle,
			logs.String())
	}
	if len(addHP) != 1 {
		t.Fatalf("%d 3557(s) for one victim - each body's plate must move with its "+
			"number", len(addHP))
	}

	// THE FAILURE THIS TEST EXISTS FOR: the same circle, centred on the caster.
	if toCaster <= radius {
		t.Fatalf("the extra body is %g from the caster, which is INSIDE the %g circle - "+
			"this sample can no longer tell a target-centred splash from a caster-centred "+
			"one", toCaster, radius)
	}
	t.Logf("extra %d: %g from the main target (inside %g), %g from the caster (outside "+
		"it) - the circle is the main target's", extra.Handle, toMain, radius, toCaster)
}

// TestSplashVictimsExcludeTheMainTarget: the body the player clicked has its own fields
// in the base block, so it must never appear AGAIN as an extra victim - a reader would
// put the wrong body in the first record and the count would name one body too many.
func TestSplashVictimsExcludeTheMainTarget(t *testing.T) {
	h, _, _ := attackZone(t)
	main := Monster{Handle: 0x20001001, X: -919, Y: 2311, Cell: 136, ClassID: 11600, Level: 1}
	extra := Monster{Handle: 0x20001002, X: -819, Y: 2311, Cell: 136, ClassID: 11600, Level: 1}
	h.spawnMu.Lock()
	h.spawnedMonster = []Monster{main, extra}
	h.monsterHP = map[uint32]float64{main.Handle: 5000, extra.Handle: 5000}
	h.spawnMu.Unlock()

	radius := h.Stances.Combat(splashStance).SplRange
	victims, _, _ := h.splashVictims(main, radius, 100, plainAttackType, 0, 0)
	if len(victims) != 1 || victims[0].Handle != extra.Handle {
		t.Fatalf("%d victim(s) %v, want exactly the extra %d and NOT the main target",
			len(victims), splashHandles(victims), extra.Handle)
	}
	for _, v := range victims {
		if v.Handle == main.Handle {
			t.Fatalf("the main target %d is in its own victim list", main.Handle)
		}
	}
}

// TestSplashVictimOutsideTheRadiusIsExcluded: the circle is STRICT - a body 300 units out
// is not splashed by a 220-unit radius, one 100 units out is - and that is the same
// strict "<" monstersInSplash uses for a skill blast (zone\near.go's distSq).
func TestSplashVictimOutsideTheRadiusIsExcluded(t *testing.T) {
	h, _, _ := attackZone(t)
	main := Monster{Handle: 0x20002001, X: -919, Y: 2311, Cell: 136, ClassID: 11600, Level: 1}
	near := Monster{Handle: 0x20002002, X: -819, Y: 2311, Cell: 136, ClassID: 11600, Level: 1}
	far := Monster{Handle: 0x20002003, X: -919, Y: 2611, Cell: 136, ClassID: 11600, Level: 1}
	h.spawnMu.Lock()
	h.spawnedMonster = []Monster{main, near, far}
	h.monsterHP = map[uint32]float64{main.Handle: 5000, near.Handle: 5000, far.Handle: 5000}
	h.spawnMu.Unlock()

	radius := h.Stances.Combat(splashStance).SplRange
	if d := math.Hypot(float64(far.X-main.X), float64(far.Y-main.Y)); d <= radius {
		t.Fatalf("the far body stands %g from the main target, which is inside %g - the "+
			"fixture no longer tests the edge", d, radius)
	}
	victims, addHP, _ := h.splashVictims(main, radius, 100, plainAttackType, 0, 0)
	if len(victims) != 1 || victims[0].Handle != near.Handle {
		t.Fatalf("%d victim(s) %v, want only the body %g inside the radius (the one at "+
			"%g must be out)", len(victims), splashHandles(victims), 100.0, 300.0)
	}
	if len(addHP) != len(victims) {
		t.Fatalf("%d 3557(s) for %d victim(s)", len(addHP), len(victims))
	}
	// AND THE LEDGERS DISAGREE ABOUT WHO WAS TOUCHED, which is the only proof that the
	// exclusion is about the circle and not about the list being built at all.
	if hp, _ := h.monsterLiveHP(far.Handle); hp != 5000 {
		t.Errorf("the body outside the circle had its HP moved to %v", hp)
	}
	if hp, _ := h.monsterLiveHP(near.Handle); hp != 4900 {
		t.Errorf("the body inside the circle holds %v, want 4900 (5000 - 100)", hp)
	}
}

// TestStance120039IsASplashRow is the DATA half, and the one test that would go red if
// the two new columns were read from the wrong attribute: the row the live capture
// belongs to is a Splash/Circle/220 row in the shipped datatable_stance.xml, and no id,
// handle or name in the code can make it one.
func TestStance120039IsASplashRow(t *testing.T) {
	h, _ := castZone(t) // skips, loudly, when the client's xml is not on this machine

	row := h.Stances.Combat(splashStance)
	if row.AtkType != "Splash" || row.SplType != "Circle" || row.SplRange != 220 {
		t.Fatalf("stance %d (%q) reads AtkType %q, SplType %q, SplRange %v - want "+
			"\"Splash\", \"Circle\", 220 (docker-port\\history\\splash-attack.md)",
			splashStance, h.Stances.StanceName(splashStance), row.AtkType, row.SplType,
			row.SplRange)
	}
	// The same row still carries the reach, so the splash is not the only column read
	// off it - and a hit at the fixture's stand point is inside that reach.
	if row.MinR != 0 || row.MaxR != 1050 {
		t.Errorf("stance %d reads MinR %v MaxR %v, want 0 .. 1050 - the row this test is "+
			"written against has moved", splashStance, row.MinR, row.MaxR)
	}
	t.Logf("stance %d %q: AtkType %q, SplType %q, SplRange %v, MinR %v .. MaxR %v",
		splashStance, h.Stances.StanceName(splashStance), row.AtkType, row.SplType,
		row.SplRange, row.MinR, row.MaxR)
}

// TestSplashStancePressSends3566Not3564 is the lane in one press: the SAME click that
// used to answer 3564 answers 3566 with one extra record, one 3557 for the main target
// and one for every body the circle reached - and the extra body's HP actually moves.
func TestSplashStancePressSends3566Not3564(t *testing.T) {
	h, logs, p := splashZone(t)
	const extraHP = 1e9
	extra := splashExtra(t, h, 0x20003001, scarecrow().X+100, scarecrow().Y, extraHP)

	out := pressAttack(t, h, p.sess, capturedTarget)

	hit, count, hps := splashReplyShape(t, out)
	if len(hit) != splashOneVictimSize {
		t.Fatalf("one extra victim gave a %d-byte 3566, want %d (75 + 48*2)", len(hit),
			splashOneVictimSize)
	}
	if count != 1 {
		t.Fatalf("the count byte is %d with one extra body, want 1", count)
	}
	for i, pkt := range out {
		if cmd := cmdOf(pkt); cmd == CmdZCAttackSingle {
			t.Fatalf("packet %d of the reply is a %d ZC_ATTACK_SINGLE - a Splash stance "+
				"must not answer the Single shape:\n%s", i, CmdZCAttackSingle,
				logs.String())
		}
	}
	if hps != 2 {
		t.Fatalf("%d 3557(s), want 2 (the main target and the one extra)", hps)
	}
	// ORDER: the hit, the MAIN target's HP, then the extras in the order the records
	// carry them (zone\attack.go builds it exactly that way).
	if got := binary.LittleEndian.Uint32(out[1][ahHandle:]); got != capturedTarget {
		t.Errorf("the first 3557 names %d, want the main target %d", got, capturedTarget)
	}
	if got := binary.LittleEndian.Uint32(out[2][ahHandle:]); got != extra {
		t.Errorf("the second 3557 names %d, want the extra victim %d", got, extra)
	}
	if got := binary.LittleEndian.Uint32(hit[AttackSplashFirst+svHandle:]); got != extra {
		t.Errorf("the victim record names %d, want %d", got, extra)
	}
	// THE LEDGER MOVED for the extra body, by the damage the 3557 carried.
	damage := math.Float64frombits(binary.LittleEndian.Uint64(out[1][ahAddHP:]))
	got, known := h.monsterLiveHP(extra)
	if !known {
		t.Fatalf("the extra body has no HP in the ledger after the hit")
	}
	if want := extraHP - damage; got != want {
		t.Fatalf("the extra body's ledger holds %v, want %v (it started at %v and the "+
			"hit was %v)", got, want, extraHP, damage)
	}
	if got >= extraHP {
		t.Fatalf("the extra body's HP did not go down (%v)", got)
	}
	// AND THE RECORD CARRIES ITS OWN HP BEFORE, not a copy of the main hit's.
	if got := math.Float64frombits(binary.LittleEndian.Uint64(
		hit[AttackSplashFirst+svHPBefore:])); got != extraHP {
		t.Errorf("the victim record's HP before = %v, want the body's own %v", got, extraHP)
	}
	t.Logf("%s", lineWith(logs.String(), "ATTACK SPLASH"))
}

// TestSplashWithNoExtrasIsStillAHit: a splash whose circle holds nobody is a VALID hit -
// the 123-byte samples are exactly that - so it must not be refused, and the main
// target's 3557 still goes out.
func TestSplashWithNoExtrasIsStillAHit(t *testing.T) {
	h, logs, p := splashZone(t)
	// FAR OUTSIDE a 220-unit circle, and still a monster this zone runs.
	splashExtra(t, h, 0x20003002, scarecrow().X+5000, scarecrow().Y+5000, 5000)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit, count, hps := splashReplyShape(t, out)
	if len(out) != 2 {
		t.Fatalf("a splash with no extra body answered %d packet(s) %v, want 2 (3566 then "+
			"3557):\n%s", len(out), kinds(out), logs.String())
	}
	if len(hit) != splashNoVictimSize {
		t.Fatalf("the 3566 is %d bytes, want %d with count 0", len(hit),
			splashNoVictimSize)
	}
	if count != 0 {
		t.Fatalf("the count byte is %d, want 0", count)
	}
	if hps != 1 {
		t.Fatalf("%d 3557(s), want 1 (the main target)", hps)
	}
	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != splashMainAttackType {
		t.Errorf("the main type is %d, want %d (splashMainAttackType) - a splash's main "+
			"block does not take the plain 3564's plainAttackType", got,
			splashMainAttackType)
	}
}

// TestSingleStancePressIsUnchanged is the negative control: the fixture's own Single
// stance (Peltast) must still answer 3564 of 74 bytes and must never mention 3566. A
// splash branch that fired for every stance would pass the tests above and break every
// character in the game.
func TestSingleStancePressIsUnchanged(t *testing.T) {
	h, logs, p := attackZone(t)

	if atkType := h.Stances.Combat(p.sess.Team[0].Stance).AtkType; atkType != "Single" {
		t.Fatalf("the fixture stands in stance %d whose AtkType is %q, want Single - this "+
			"control is written against a Single row", p.sess.Team[0].Stance, atkType)
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 {
		t.Fatalf("a Single stance press answered %d packet(s) %v, want 2:\n%s", len(out),
			kinds(out), logs.String())
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCAttackSingle {
		t.Fatalf("the first packet is cmd %d, want %d", cmd, CmdZCAttackSingle)
	}
	if len(out[0]) != AttackSingleSize {
		t.Fatalf("the 3564 is %d bytes, want %d", len(out[0]), AttackSingleSize)
	}
	for _, pkt := range out {
		if cmd := cmdOf(pkt); cmd == CmdZCAttackSplash {
			t.Fatalf("a Single stance press sent a %d", cmd)
		}
	}
	if strings.Contains(logs.String(), "ATTACK SPLASH") {
		t.Errorf("a Single stance press wrote a splash line:\n%s", logs.String())
	}
}

// TestSplashRepeatArmsAndSwings3566: the repeat follows the stance. A splash press
// arms the rhythm - the shape it replays now exists - and the swing is a 3566, not a
// 3564, because it goes through the SAME Handler.attackAt the press used.
func TestSplashRepeatArmsAndSwings3566(t *testing.T) {
	h, logs, p := splashZone(t)
	splashExtra(t, h, 0x20003003, scarecrow().X+100, scarecrow().Y, 1e9)

	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 3 {
		t.Fatalf("the press answered %d packet(s) %v, want 3:\n%s", len(out), kinds(out),
			logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("a Splash stance press armed no repeat - the shape the repeat replays "+
			"now exists (zone\\attack_splash.go):\n%s", logs.String())
	}
	if s.target != capturedTarget {
		t.Errorf("the repeat follows target %d, want %d", s.target, capturedTarget)
	}

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	hit, count, hps := splashReplyShape(t, out)
	if count != 1 || len(hit) != splashOneVictimSize {
		t.Fatalf("the swing's 3566 is %d bytes with count %d, want %d with count 1 - the "+
			"swing must splash what the press splashed:\n%s", len(hit), count,
			splashOneVictimSize, logs.String())
	}
	if hps != 2 {
		t.Fatalf("the swing carried %d 3557(s), want 2", hps)
	}
	if strings.Contains(logs.String(), "ATTACK REPEAT NOT BUILT") {
		t.Errorf("a Splash stance was refused a repeat:\n%s", logs.String())
	}
	t.Logf("swing: %s", lineWith(logs.String(), "ATTACK SPLASH"))
}

// TestSplashSwingRecomputesTheCircle: the circle is computed AT SWING TIME from where
// the bodies stand then, not replayed from the click. A monster that walked out is not
// splashed (count 0, the 123-byte shape) and one that is inside when the swing comes is.
func TestSplashSwingRecomputesTheCircle(t *testing.T) {
	h, logs, p := splashZone(t)
	extra := splashExtra(t, h, 0x20003004, scarecrow().X+100, scarecrow().Y, 1e9)

	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 3 {
		t.Fatalf("the press answered %d packet(s) %v, want 3:\n%s", len(out), kinds(out),
			logs.String())
	}

	// OUT OF THE CIRCLE (500 units away) before the swing is owed.
	splashMoveExtra(h, extra, scarecrow().X+500, scarecrow().Y)
	swingDue(t, h, p.sess, 0)
	hit, count, hps := splashReplyShape(t, h.swingTick(p.sess))
	if count != 0 || len(hit) != splashNoVictimSize {
		t.Fatalf("the swing's 3566 is %d bytes with count %d, want %d with count 0 - the "+
			"body has left the circle:\n%s", len(hit), count, splashNoVictimSize,
			logs.String())
	}
	if hps != 1 {
		t.Fatalf("%d 3557(s), want 1 (the main target only)", hps)
	}

	// AND BACK INSIDE before the next one.
	splashMoveExtra(h, extra, scarecrow().X+100, scarecrow().Y)
	swingDue(t, h, p.sess, 0)
	hit, count, hps = splashReplyShape(t, h.swingTick(p.sess))
	if count != 1 || len(hit) != splashOneVictimSize {
		t.Fatalf("the next swing's 3566 is %d bytes with count %d, want %d with count 1 - "+
			"a body inside the circle at swing time must be hit:\n%s", len(hit), count,
			splashOneVictimSize, logs.String())
	}
	if hps != 2 {
		t.Fatalf("%d 3557(s), want 2", hps)
	}
}

// TestSplashSwingAfterAMoveOrderIsStillAFullSplash: the splash repeat is the same repeat,
// so the player's own click ends the engagement - A 60-UNIT ONE INCLUDED, because the owner's
// rule is that a move no matter how small is a cancel (2026-11-01) - and the swing that was
// already due still goes out as a FULL splash, one 3566 with its circle and one 3557 per body
// in it. It goes through h.Reply, the route the swing tests use, so the dispatch case is
// exercised too.
//
// THIS TEST USED TO PIN THE OPPOSITE and it was the old 150-unit line that made it look
// right: a 60 u leg was "a background correction" and had to leave the engagement alone. The
// measurement that line was built on was real (the client does send 33-146 u legs), but its
// reading was not: inside KeepReturnRange 2000 the client's AI never moves the driven
// character by itself (ge\ai\lib.scp:106), so that leg IS the player. The guard for the one
// report that may be ours is in attack_swing_test.go (TestOnlyAWalkWeSentIsNotAMoveOrder).
func TestSplashSwingAfterAMoveOrderIsStillAFullSplash(t *testing.T) {
	h, logs, p := splashZone(t)
	splashExtra(t, h, 0x20003005, scarecrow().X+100, scarecrow().Y, 1e9)

	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 3 {
		t.Fatalf("the press answered %d packet(s) %v, want 3:\n%s", len(out), kinds(out),
			logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("no repeat was armed:\n%s", logs.String())
	}
	// THE SWING IS ALREADY DUE when the click lands, so the order may not take it.
	swingDue(t, h, p.sess, 0)

	// 60 units, and NOT a walk of ours: this is the player's click, and it ends the
	// engagement however small it is. The swing that is already due goes out in THIS
	// reply, BEFORE the echo - the reference's measured order (piece 2i).
	walkTo := [3]int32{p.sess.Team[0].Position[0] + smallTestLeg, p.sess.Team[0].Position[1], 136}
	moved := moveReply(t, h, p.sess, walkTo)
	if got := cmdCount(moved, CmdZCMovePC); got != 1 {
		t.Fatalf("the move answered %d ZC_MOVE_PC(s), want 1:\n%s", got, logs.String())
	}
	hit, count, hps := splashReplyShape(t, moved)
	// One 3557 per body the circle took: the main target and the one extra victim.
	if count != 1 || len(hit) != splashOneVictimSize || hps != 2 {
		t.Fatalf("the swing in the move's reply is %d bytes with count %d and %d 3557(s), "+
			"want %d / 1 / 2 - a full splash:\n%s", len(hit), count, hps,
			splashOneVictimSize, logs.String())
	}
	if echoAt := cmdCount(moved, CmdZCMovePC); echoAt != 1 ||
		cmdOf(moved[len(moved)-1]) != CmdZCMovePC {
		t.Fatalf("the reply is %v: the splash must come BEFORE the move echo, as the "+
			"reference's own capture shows:\n%s", kinds(moved), logs.String())
	}
	// AND THEN THE ENGAGEMENT IS OVER, already in this reply, which is what the click
	// bought: the swing landed and there is nothing further.
	if p.sess.swingOf(0) != nil {
		t.Fatalf("the splash repeat survived a %d u player move order - that is the owner's "+
			"bug:\n%s", smallTestLeg, logs.String())
	}
	if got := h.swingTick(p.sess); len(got) != 0 {
		t.Fatalf("the tick after the move order sent %d packet(s) %v, want none:\n%s",
			len(got), kinds(got), logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT ENDED"); !strings.Contains(line,
		"move order") {
		t.Fatalf("the ending does not name the move order: %q\n%s", line, logs.String())
	}
}

// TestSplashSwingReachesTheOtherPlayerByteIdentical is R93 held against the splash: the
// 3566 and every 3557 are built ONCE and the watcher gets those same slices, in the same
// order. A per-recipient rebuild is where one client's splash becomes a different splash
// on somebody else's screen (`history\combat-attack-heal.md` §4).
func TestSplashSwingReachesTheOtherPlayerByteIdentical(t *testing.T) {
	h, logs, p := splashZone(t)
	splashExtra(t, h, 0x20003006, scarecrow().X+100, scarecrow().Y, 1e9)
	w := watcherInZone(t, h, p)

	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 3 {
		t.Fatalf("the press answered %d packet(s) %v, want 3:\n%s", len(out), kinds(out),
			logs.String())
	}
	w.conn.writes, p.conn.writes = nil, nil

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	hit, count, hps := splashReplyShape(t, out)
	if count != 1 {
		t.Fatalf("the swing's count is %d, want 1:\n%s", count, logs.String())
	}
	sent := w.conn.sent()
	if len(sent) != 1+hps {
		t.Fatalf("the other player got %d packet(s) from the swing, want %d (one 3566 and "+
			"one 3557 per body):\n%s", len(sent), 1+hps, logs.String())
	}
	for i := range sent {
		if !bytesEqual(sent[i], out[i]) {
			t.Fatalf("the other player's packet %d (cmd %d) differs from the actor's "+
				"(cmd %d):\n got %x\nwant %x", i, cmdOf(sent[i]), cmdOf(out[i]), sent[i],
				out[i])
		}
	}
	if got := cmdOf(sent[0]); got != CmdZCAttackSplash {
		t.Fatalf("the other player's first packet is %d, want %d", got, CmdZCAttackSplash)
	}
	if len(hit) != splashOneVictimSize {
		t.Errorf("the shared 3566 is %d bytes, want %d", len(hit), splashOneVictimSize)
	}
	// The actor's own socket got nothing from the fan-out: its copy is the returned slice.
	if got := len(p.conn.sent()); got != 0 {
		t.Errorf("the actor's own socket got %d fan-out packet(s), want 0", got)
	}
}
