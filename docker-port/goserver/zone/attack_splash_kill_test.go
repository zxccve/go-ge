package zone

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// A KILL LANDED BY A SPLASH MUST CARRY THE SAME FATAL FORM THE SINGLE TARGET DOES.
//
// The single-target killing blow is the 111-byte 3564: the 74-byte hit, type 6, and the
// 37-byte KNOCKDOWN_INFO the client needs to play the fall (zone\attack.go, commit
// 9eceed8d). A splash kill still went out as a 48-byte record with type 0 and no block,
// so the client never called SetKnockDownInfo for that body and it blinked out - the
// hole `9eceed8d` named and did not close.
//
// WHERE THE BLOCK GOES, AND WHY IT IS NOT A GUESS. `CNormalNet::AttackSplash`
// (source2\geClient\Source\GENormalNet_Action.cpp:299-340) reads each extra victim as
// `HIT_INFO` (48 bytes) and then switches on that record's own type: for a knockdown
// flavour it advances a further 37-byte KNOCKDOWN_INFO and applies it with
// `pTgtActor->SetKnockDownInfo`, for everything else it advances nothing. So a killing
// record is 48 + 37 = 85 bytes with the block AFTER the record - and the wire has
// measured exactly that stride: the 245-byte splash sample (`history\splash-attack.md`)
// carries its record at +112 and its KD block at +160.
//
// THE VALUES ARE THE SINGLE TARGET'S, not a second table: startPos is the body's own
// position, endPos and hAngle are the fall the single form computes (500 units along
// attacker -> victim, `knockdownFall` - the reference's own `SetKnockDown`,
// `FieldChar.cpp:780-812`), delay 600, power 100, vAngle 45, type 1.
// `TestSplashKillingVictimCarriesTheFatalForm` compares the 37 bytes with
// `BuildAttackSingleKnockdown`'s own block, so the two forms cannot drift - if the fall
// geometry moves, both move, and this test follows it.

// splashRecord is one victim record as the CLIENT walks it: the 48-byte hit and, when
// its type is a knockdown flavour, the 37 bytes that follow.
type splashRecord struct {
	off      int // where the record starts in the packet
	size     int // 48, or 85 when the record carries a KNOCKDOWN_INFO
	handle   uint32
	pos      [3]int32
	damage   float64
	hpBefore float64
	typ      int32
	knockPos [3]int32
	kd       []byte // the 37-byte block, nil when the record carries none
}

// splashTypeReadsKnockdown is the client's OWN set of types that consume a
// KNOCKDOWN_INFO after the hit, in the one place this test needs it: the walk below.
//
//	2, 3  ATTACKTYPE_KNOCKDOWN / ATTACKTYPE_CRITICAL_KD in the shipped enum
//	      (`geZCTypes.h:512`, 0..5) - the two cases the source's switch has.
//	6     the wire's plain knockdown: every 111-byte 3564 carries 6 or 7, and 6 is the
//	      value this zone sends for its own killing blow (zone\attack.go).
//	7     the wire's critical-looking flavour: no 3564 or 3566 sample carries it in a
//	      74-byte hit, and the one 3566 with a per-victim block is the 245-byte type 7
//	      sample, i.e. the live client does consume 37 bytes there too.
//
// The builder writes 0 (a survivor) or 6 (a kill). The set is stated from the CLIENT and
// the WIRE, not from what this builder emits, so the walk is not its own oracle: if the
// builder wrote, say, 4 it would consume nothing and every later record would be read
// from the wrong byte, which is exactly what these tests must catch.
func splashTypeReadsKnockdown(typ int32) bool {
	switch typ {
	case 2, 3, 6, 7:
		return true
	}
	return false
}

// parseSplashLikeClient walks one 3566 exactly as `CNormalNet::AttackSplash` does: read
// the count at +74, then `count` records of 48 bytes, each followed by 37 more when its
// type reads a knockdown.
//
// NO REMAINDER is part of the walk, not an extra assertion: after the last record the
// only bytes allowed to be left are the ONE 48-byte unused record the wire always
// carries (all zero in 16 of 16 samples). The client stops at `count`, so that record is
// never read - but a builder that misplaced a block would leave a remainder of some
// other length, and this walk fails on it.
func parseSplashLikeClient(t *testing.T, pkt []byte) []splashRecord {
	t.Helper()
	count := int(pkt[AttackSplashCount])
	recs := make([]splashRecord, 0, count)
	off := AttackSplashFirst
	for i := 0; i < count; i++ {
		if off+SplashVictimSize > len(pkt) {
			t.Fatalf("record %d of %d starts at +%d, past the %d-byte packet - the "+
				"count and the bytes disagree", i, count, off, len(pkt))
		}
		rec := splashRecord{
			off:      off,
			size:     SplashVictimSize,
			handle:   binary.LittleEndian.Uint32(pkt[off+svHandle:]),
			pos:      worldPos(pkt[off+svPos:]),
			damage:   math.Float64frombits(binary.LittleEndian.Uint64(pkt[off+svDamage:])),
			hpBefore: math.Float64frombits(binary.LittleEndian.Uint64(pkt[off+svHPBefore:])),
			typ:      int32(binary.LittleEndian.Uint32(pkt[off+svAttackType:])),
			knockPos: worldPos(pkt[off+svKnockPos:]),
		}
		off += SplashVictimSize
		if splashTypeReadsKnockdown(rec.typ) {
			if off+KnockDownInfoSize > len(pkt) {
				t.Fatalf("record %d (handle %d) states type %d, so the client reads a "+
					"%d-byte KNOCKDOWN_INFO at +%d, past the %d-byte packet",
					i, rec.handle, rec.typ, KnockDownInfoSize, off, len(pkt))
			}
			rec.kd = append([]byte(nil), pkt[off:off+KnockDownInfoSize]...)
			rec.size = SplashKillingVictimSize
			off += KnockDownInfoSize
		}
		recs = append(recs, rec)
	}
	if rest := len(pkt) - off; rest != SplashVictimSize {
		t.Fatalf("the client's walk of the %d-byte 3566 (count %d) stopped at +%d and "+
			"left %d byte(s); the only bytes after the last record may be the one "+
			"%d-byte unused record", len(pkt), count, off, rest, SplashVictimSize)
	}
	for i, b := range pkt[off:] {
		if b != 0 {
			t.Fatalf("the unused record's byte +%d is 0x%02x, want 0 (the one record "+
				"after the last real one is all zero in 16 of 16 samples)", off+i, b)
		}
	}
	return recs
}

// splashHitOf finds the ONE 3566 in a reply and refuses a second hit packet.
func splashHitOf(t *testing.T, out [][]byte) []byte {
	t.Helper()
	var hit []byte
	for _, pkt := range out {
		if cmdOf(pkt) == CmdZCAttackSplash {
			if hit != nil {
				t.Fatalf("the reply carries TWO %d packets - one press is one hit",
					CmdZCAttackSplash)
			}
			hit = pkt
			continue
		}
		if cmdOf(pkt) == CmdZCAttackSingle {
			t.Fatalf("a Splash stance answered %d ZC_ATTACK_SINGLE", cmdOf(pkt))
		}
	}
	if hit == nil {
		t.Fatalf("the press sent no %d at all", CmdZCAttackSplash)
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Fatalf("the 3566 length word at +2 is %d, the frame is %d bytes", got, len(hit))
	}
	return hit
}

// splashKillZone is the two-victim fixture every test below starts from: the splash
// zone's Scarecrow as the main target (1e9 HP, it survives), one extra at 1 HP that this
// hit KILLS, and one at 1e9 HP that it hurts and leaves standing. Both extras are
// strictly inside the measured 220-unit circle drawn on the MAIN TARGET.
func splashKillZone(t *testing.T) (h *Handler, logs *stringBuf, p *player, killed, survivor uint32) {
	t.Helper()
	h, logs, p = splashZone(t)
	killed = splashExtra(t, h, 0x20004001, scarecrow().X+100, scarecrow().Y, 1)
	survivor = splashExtra(t, h, 0x20004002, scarecrow().X, scarecrow().Y+120, 1e9)
	return h, logs, p, killed, survivor
}

// TestSplashKillingVictimCarriesTheFatalForm is items 1 and 4 of the card: in a
// two-victim splash where ONE victim dies, the dead victim's record is the fatal form -
// type 6 and the 37-byte KNOCKDOWN_INFO, byte for byte the block the single-target
// killing blow sends - while the survivor's record stays exactly the 48 bytes it is
// today (compared against the current builder, not against a remembered layout), and the
// main block, the count, the damage and the 3557s do not move.
func TestSplashKillingVictimCarriesTheFatalForm(t *testing.T) {
	h, logs, p, killed, survivor := splashKillZone(t)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)

	if got, want := len(hit), AttackSplashFirst+SplashVictimSize+
		SplashKillingVictimSize+SplashVictimSize; got != want {
		t.Fatalf("the two-victim splash with one kill is %d bytes, want %d (75 + 48 + 85 "+
			"+ 48 - one extra 37 bytes for the kill)", got, want)
	}
	if got := int(hit[AttackSplashCount]); got != 2 {
		t.Fatalf("the count byte at +%d is %d, want 2 extra victims", AttackSplashCount, got)
	}

	// WHAT THE HANDLER ACTUALLY SENT, read back out of the packet so the reference
	// packets below are built from the same numbers the client was given.
	attacker := binary.LittleEndian.Uint32(hit[asAttacker:])
	atkPos := worldPos(hit[asAtkPos:])
	mainPos := worldPos(hit[asTargetPos:])
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	mainHP := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	spd := binary.LittleEndian.Uint16(hit[asAtkSpd:])

	// THE MAIN BLOCK DOES NOT MOVE: everything from +4 to the count (the attacker, both
	// WORLDPOS, the speed, the main target's fields, the type and the knock-back) is
	// today's packet for the same hit, byte for byte. +0..+3 are the command word and
	// the LENGTH, and the length has to differ - that is the +37 item 2 pins.
	today := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos, damage,
		mainHP, splashMainAttackType, nil)
	if !bytes.Equal(hit[4:AttackSplashCount], today[4:AttackSplashCount]) {
		t.Errorf("the main 74-byte block changed when a victim died:\n got %x\nwant %x",
			hit[4:AttackSplashCount], today[4:AttackSplashCount])
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Errorf("the length word is %d in a %d-byte frame - it must carry the +%d",
			got, len(hit), KnockDownInfoSize)
	}

	recs := parseSplashLikeClient(t, hit)
	if len(recs) != 2 {
		t.Fatalf("the client reads %d record(s), want 2", len(recs))
	}
	var dead, alive *splashRecord
	for i := range recs {
		switch recs[i].handle {
		case killed:
			dead = &recs[i]
		case survivor:
			alive = &recs[i]
		}
	}
	if dead == nil || alive == nil {
		t.Fatalf("the records name %v, want both the killed %d and the survivor %d",
			recHandles(recs), killed, survivor)
	}

	// THE KILLED VICTIM: the fatal type, then the 37 bytes.
	if dead.typ != knockdownAttackType {
		t.Fatalf("the killed victim %d states type %d at its +%d, want %d - the type the "+
			"client reads a KNOCKDOWN_INFO on (the single-target killing blow's own):\n%s",
			killed, dead.typ, svAttackType, knockdownAttackType, logs.String())
	}
	if dead.size != SplashKillingVictimSize {
		t.Fatalf("the killed victim's record is %d bytes, want %d (48 + %d)",
			dead.size, SplashKillingVictimSize, KnockDownInfoSize)
	}
	if dead.kd == nil {
		t.Fatalf("the killed victim %d carries no KNOCKDOWN_INFO - the client would "+
			"not call SetKnockDownInfo and the body would blink out:\n%s",
			killed, logs.String())
	}
	// The client's own deadly condition, on the record it is read from: hp BEFORE the
	// hit, and it must not be above the damage (`GeAttack.cpp:42`).
	if !(dead.hpBefore > 0 && dead.hpBefore <= dead.damage) {
		t.Errorf("the killed victim's record says HP-before %g, damage %g - the client "+
			"only calls SetDeadlyHit for hp <= damage", dead.hpBefore, dead.damage)
	}

	// THE 37 BYTES THEMSELVES: the same block the single-target form appends at +74.
	single := BuildAttackSingleKnockdown(attacker, atkPos, spd, killed, dead.pos, damage,
		dead.hpBefore, knockdownAttackType)
	if !bytes.Equal(dead.kd, single[AttackSingleSize:]) {
		t.Fatalf("the victim's KNOCKDOWN_INFO is not the single-target form's block:\n"+
			" got %x\nwant %x", dead.kd, single[AttackSingleSize:])
	}
	// Offsets inside the block are the record's own (svKD* - SplashVictimSize).
	kd := func(off int) int { return off - SplashVictimSize }
	if got := worldPos(dead.kd[kd(svKDStartPos):]); got != dead.pos {
		t.Errorf("KD startPos is %v, want the body's own position %v", got, dead.pos)
	}
	// THE LANDING POINT is the reference's geometry - 500 units along attacker ->
	// victim with hAngle that bearing (`knockdownFall`, `FieldChar.cpp:780-812`), on the
	// victim's record as on the single-target form.
	dx := float64(dead.pos[0] - atkPos[0])
	dy := float64(dead.pos[1] - atkPos[1])
	dist := math.Hypot(dx, dy)
	wantEnd := [3]int32{
		dead.pos[0] + int32(math.Round(dx/dist*kdFallDistance)),
		dead.pos[1] + int32(math.Round(dy/dist*kdFallDistance)),
		dead.pos[2],
	}
	if got := worldPos(dead.kd[kd(svKDEndPos):]); got != wantEnd {
		t.Errorf("KD endPos is %v, want %v - %g units along attacker %v -> victim %v",
			got, wantEnd, kdFallDistance, atkPos, dead.pos)
	}
	if got := binary.LittleEndian.Uint16(dead.kd[kd(svKDDelay):]); got != kdDelayMs {
		t.Errorf("KD delay is %d, want %d", got, kdDelayMs)
	}
	if got := binary.LittleEndian.Uint16(dead.kd[kd(svKDPower):]); got != kdPower {
		t.Errorf("KD power is %d, want %d", got, kdPower)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(
		dead.kd[kd(svKDVAngle):])); got != float32(kdVAngleDeg) {
		t.Errorf("KD vAngle is %v, want %v", got, float32(kdVAngleDeg))
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(
		dead.kd[kd(svKDHAngle):])); math.Abs(float64(got)-math.Atan2(dy, dx)*180/math.Pi) > 0.01 {
		t.Errorf("KD hAngle is %v, want the bearing of the fall %v", got,
			math.Atan2(dy, dx)*180/math.Pi)
	}
	if got := dead.kd[kd(svKDType)]; got != kdTypeNormal {
		t.Errorf("KD type byte is %d, want %d (KDT_NORMAL)", got, kdTypeNormal)
	}

	// THE SURVIVOR IS UNCHANGED, and proved against TODAY'S BUILDER rather than against
	// a transcription of the layout: the same one-victim call with no kill flag must
	// produce those 48 bytes identically.
	if alive.typ != splashVictimAttackType {
		t.Errorf("the surviving victim %d states type %d, want %d - only a KILL takes "+
			"the fatal form", survivor, alive.typ, splashVictimAttackType)
	}
	if alive.size != SplashVictimSize {
		t.Fatalf("the surviving victim's record is %d bytes, want %d - a survivor "+
			"carries no block", alive.size, SplashVictimSize)
	}
	if alive.hpBefore != 1e9 {
		t.Errorf("the survivor's HP-before is %g, want its own 1e9", alive.hpBefore)
	}
	todaySurvivor := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos,
		damage, mainHP, splashMainAttackType, []SplashVictim{{
			Handle: survivor, Pos: alive.pos, Damage: alive.damage,
			HPBefore: alive.hpBefore, AttackType: splashVictimAttackType,
		}})
	if !bytes.Equal(hit[alive.off:alive.off+SplashVictimSize],
		todaySurvivor[AttackSplashFirst:AttackSplashFirst+SplashVictimSize]) {
		t.Errorf("the surviving victim's 48-byte record differs from the current "+
			"builder's:\n got %x\nwant %x", hit[alive.off:alive.off+SplashVictimSize],
			todaySurvivor[AttackSplashFirst:AttackSplashFirst+SplashVictimSize])
	}

	// AND NOTHING ELSE MOVED: one 3557 per body, the same damage on every record and
	// every delta, and the plates of the two extra bodies at 0 and 1e9-damage.
	var hps [][]byte
	for _, pkt := range out {
		if cmdOf(pkt) == CmdZCAddHP {
			hps = append(hps, pkt)
		}
	}
	if len(hps) != 3 {
		t.Fatalf("%d 3557(s), want 3 (the main target and both extras)", len(hps))
	}
	seen := map[uint32]float64{}
	for _, pkt := range hps {
		if got := math.Float64frombits(binary.LittleEndian.Uint64(
			pkt[ahAddHP:])); got != damage {
			t.Errorf("a 3557 states a delta of %g, want the hit's own %g", got, damage)
		}
		seen[binary.LittleEndian.Uint32(pkt[ahHandle:])] = math.Float64frombits(
			binary.LittleEndian.Uint64(pkt[ahNowHP:]))
	}
	if got, ok := seen[killed]; !ok || got != 0 {
		t.Errorf("the killed victim's 3557 carries %v (present %v), want 0", got, ok)
	}
	if got, ok := seen[survivor]; !ok || got <= 0 {
		t.Errorf("the survivor's 3557 carries %v (present %v), want the HP it has left",
			got, ok)
	}
	for _, rec := range recs {
		if rec.damage != damage {
			t.Errorf("record %d (handle %d) states damage %g, want the hit's %g",
				rec.off, rec.handle, rec.damage, damage)
		}
	}
	t.Logf("%d B, count 2, killed %d: type %d + %d B KD; survivor %d: %d B, type %d",
		len(hit), killed, dead.typ, KnockDownInfoSize, survivor, alive.size, alive.typ)
}

// TestSplashKillAddsExactlyOneKnockdownBlock is item 2: the packet's length is today's
// plus exactly ONE 37-byte block, the length word agrees, and the client's own walk of
// the packet ends on the last record with no remainder.
func TestSplashKillAddsExactlyOneKnockdownBlock(t *testing.T) {
	h, logs, p, killed, survivor := splashKillZone(t)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)

	attacker := binary.LittleEndian.Uint32(hit[asAttacker:])
	atkPos := worldPos(hit[asAtkPos:])
	mainPos := worldPos(hit[asTargetPos:])
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	mainHP := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	spd := binary.LittleEndian.Uint16(hit[asAtkSpd:])

	// TODAY'S SHAPE, same two victims, neither of them a kill: 75 + 48*3 = 219.
	noKill := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos, damage,
		mainHP, splashMainAttackType, []SplashVictim{
			{Handle: killed, Pos: [3]int32{scarecrow().X + 100, scarecrow().Y,
				scarecrow().Cell}, Damage: damage, HPBefore: 1,
				AttackType: splashVictimAttackType},
			{Handle: survivor, Pos: [3]int32{scarecrow().X, scarecrow().Y + 120,
				scarecrow().Cell}, Damage: damage, HPBefore: 1e9,
				AttackType: splashVictimAttackType},
		})
	if want := AttackSplashFirst + 3*SplashVictimSize; len(noKill) != want {
		t.Fatalf("the CONTROL packet without a kill is %d bytes, want today's %d "+
			"(75 + 48 x 3) - the control itself is wrong, so the +37 below would mean "+
			"nothing", len(noKill), want)
	}
	if got, want := len(hit), len(noKill)+KnockDownInfoSize; got != want {
		t.Fatalf("the killing splash is %d bytes, want %d = today's %d + one %d-byte "+
			"KNOCKDOWN_INFO for the one victim that died:\n%s", got, want, len(noKill),
			KnockDownInfoSize, logs.String())
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Fatalf("the length word at +2 is %d, the frame is %d bytes - the +2 word "+
			"must carry the block too", got, len(hit))
	}

	// THE CLIENT'S WALK, and the arithmetic stated from its side: 75 + 48 (survivor) +
	// 85 (the killed record) + 48 (the unused record) = 256, nothing left over.
	recs := parseSplashLikeClient(t, hit)
	if len(recs) != 2 {
		t.Fatalf("the client reads %d records, want 2", len(recs))
	}
	total := AttackSplashFirst + SplashVictimSize // the unused record
	for _, rec := range recs {
		total += rec.size
	}
	if total != len(hit) {
		t.Fatalf("the client's records account for %d of the packet's %d bytes", total,
			len(hit))
	}
	if want := 256; len(hit) != want {
		t.Fatalf("the measured shape of this case is %d bytes (75 + 48 + 85 + 48), got %d",
			want, len(hit))
	}
	kdRecs := 0
	for _, rec := range recs {
		if rec.kd != nil {
			kdRecs++
		}
	}
	if kdRecs != 1 {
		t.Fatalf("%d record(s) carry a KNOCKDOWN_INFO, want exactly the killed one", kdRecs)
	}
	t.Logf("%d B = 75 + %d + %d + %d, one KD block, client walk ends on the unused record",
		len(hit), SplashVictimSize, SplashKillingVictimSize, SplashVictimSize)
}

// TestSplashKillIsDecidedPerVictimByTheLedger is item 3: `before > 0 && after == 0` is
// the selector, for each victim INDEPENDENTLY - not "hp <= damage", which is also true of
// a body that was already at 0 and is not being killed again.
//
// Three extras, one hit: one with 1 HP dies, one with 1e9 survives, one already at 0 HP
// stays at 0. Only the first may carry the fatal form, and the zone must owe exactly one
// death - for that handle.
func TestSplashKillIsDecidedPerVictimByTheLedger(t *testing.T) {
	h, logs, p := splashZone(t)
	killed := splashExtra(t, h, 0x20004101, scarecrow().X+80, scarecrow().Y, 1)
	survivor := splashExtra(t, h, 0x20004102, scarecrow().X-80, scarecrow().Y, 1e9)
	empty := splashExtra(t, h, 0x20004103, scarecrow().X, scarecrow().Y+90, 0)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)
	recs := parseSplashLikeClient(t, hit)
	if len(recs) != 3 {
		t.Fatalf("the client reads %d records, want 3 (one per extra):\n%s",
			len(recs), logs.String())
	}
	byHandle := map[uint32]splashRecord{}
	for _, rec := range recs {
		byHandle[rec.handle] = rec
	}
	for _, want := range []uint32{killed, survivor, empty} {
		if _, ok := byHandle[want]; !ok {
			t.Fatalf("no record names %d; the records are %v", want, recHandles(recs))
		}
	}

	// THE KILLED ONE: before 1 > 0, after 0.
	if rec := byHandle[killed]; rec.kd == nil || rec.typ != knockdownAttackType {
		t.Errorf("the victim at 1 HP (before 1 > 0, after 0) states type %d and carries "+
			"no block: %v - it IS the kill", rec.typ, rec.kd != nil)
	}
	// THE SURVIVOR: before 1e9, after 1e9-damage > 0.
	if rec := byHandle[survivor]; rec.kd != nil || rec.typ != splashVictimAttackType {
		t.Errorf("the survivor (after > 0) states type %d and carries a block: %v",
			rec.typ, rec.kd != nil)
	}
	// THE EMPTY BODY: before 0, after 0 - `after == 0` alone is NOT the kill.
	rec := byHandle[empty]
	if rec.kd != nil || rec.typ != splashVictimAttackType {
		t.Errorf("the body that was ALREADY at 0 HP states type %d and carries a "+
			"block: %v - `after == 0` without `before > 0` is not a kill, and this "+
			"packet would tell the client to knock down a corpse", rec.typ,
			rec.kd != nil)
	}
	// Its record still satisfies the client's deadly condition (hp <= damage), which is
	// exactly why the builder must not derive the fatal form from that condition.
	if !(rec.hpBefore == 0 && rec.hpBefore <= rec.damage) {
		t.Errorf("the empty body's record says HP-before %g, damage %g - this test is "+
			"only sharp while hp <= damage holds there", rec.hpBefore, rec.damage)
	}
	// AND THE LEDGER AGREES WITH THE PACKET: exactly one death, for the killed handle.
	// Its send belongs to the corpse's own clock (2026-11-09, zone/respawn.go
	// deathPacketTick); this fixture's roster holds no player, so the packet goes "to 0
	// player(s)" and the kill line is what names the body it was built for.
	deadTick(t, h, killed)
	deaths := deadLines(logs.String())
	if len(deaths) != 1 || !deaths[killed] {
		t.Fatalf("the press sent deaths %v, want exactly one for %d (the survivor and "+
			"the already-empty body are not kills):\n%s", deaths, killed, logs.String())
	}
	if hp, ok := h.monsterLiveHP(empty); !ok || hp != 0 {
		t.Errorf("the already-empty body's ledger holds %v (known %v), want 0 untouched",
			hp, ok)
	}
	t.Logf("killed %d (block, type %d), survivor %d and empty %d (48 B, type %d); "+
		"one death owed", killed, byHandle[killed].typ, survivor, empty,
		byHandle[survivor].typ)
}

// recHandles is a record list's handles, in wire order, so a failure names what was
// actually there instead of only what was expected.
func recHandles(recs []splashRecord) []uint32 {
	out := make([]uint32, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.handle)
	}
	return out
}
