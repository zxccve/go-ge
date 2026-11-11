package zone

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// THE KILLING BLOW MUST SATISFY THE CLIENT'S OWN DEATH RULE.
//
// The client does not take a death instruction off the wire: `geactor::Dead`
// (`source2\geClient\Source\GENormalActor.cpp:74-80`) only plays `Die()` for an actor
// that is NOT already deadly, and the deadly flag is raised by the hit packet itself.
// `geattack::Attack` (`geClientShared\source\GeAttack.cpp:42-48`) does
//
//	if (hitInfo.hp <= hitInfo.damage) pTgtActor->SetDeadlyHit(pAtkActor);
//
// and the actual fall is played later, on the attack animation's final hit, when
// `CNormalObject::OnHandleFSMHit` (`GENormalObject.cpp:1159-1161`) sees
//
//	IsDeadlyHit() && pHitParm->isFinal && (pAtker == GetKillMeActor())
//
// `isFinal` is the animation's own flag and cannot be sent; the other two are this
// packet: `hp <= damage` (with hp BEFORE the hit) and the attacker handle at +4 that
// became `m_pKillMeActor`. For a body whose model has no dead animation the fall needs
// `isKnockDown`, which needs a KNOCKDOWN TYPE - the live client passes types 5/6/7
// into the attack call and reads no byte after the hit (`FUN_004e220b`, decompiled
// 2026-11-05) - and the REFERENCE picks that type itself for exactly this body: a
// monster at 0 HP with no original dead animation is a knockdown
// (`CFieldChar::OnSingleHit`, `source2\geServer\ZServerLib\FieldChar_Message.cpp:160-166`).
//
// So this test asserts the packet a killing hit really builds, field by field, against
// those lines - not against what this zone meant to write.
func TestKillingBlowCarriesTheClientsDeathConditions(t *testing.T) {
	h, logs, p, _ := deadZone(t) // the target's HP is 1: this press IS the kill

	var hit []byte
	for _, pkt := range pressAttack(t, h, p.sess, capturedTarget) {
		if len(pkt) >= 2 && binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			if hit != nil {
				t.Fatalf("the killing press sent TWO %d packets:\n%s",
					CmdZCAttackSingle, logs.String())
			}
			hit = pkt
		}
	}
	if hit == nil {
		t.Fatalf("the killing press sent no %d at all:\n%s",
			CmdZCAttackSingle, logs.String())
	}

	// SIZE: the killing-blow form is the 74-byte hit plus the 37-byte block = 111,
	// and the length word at +2 must agree (3564 is registered size 0, "variable").
	if len(hit) != AttackSingleKnockdownSize {
		t.Fatalf("the killing %d is %d bytes, want %d (74 + the %d-byte KNOCKDOWN_INFO):\n%s",
			CmdZCAttackSingle, len(hit), AttackSingleKnockdownSize,
			KnockDownInfoSize, logs.String())
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Fatalf("the killing %d length word is %d, the frame is %d bytes",
			CmdZCAttackSingle, got, len(hit))
	}

	// THE CLIENT'S CONDITION ITSELF: hp BEFORE the hit, compared with the damage.
	// `hp` must be BEFORE, and the LIVE client proves it twice: its 3564 handler sets
	// the real HP from `hp - damage` (`FUN_004e220b` at 0x4e229c:
	// `dVar3 = *(double *)(param_1 + 0x32) - dVar4; if (dVar3 < 0.0) dVar3 = 0.0;`),
	// and the reference server writes `ret->SetHP(hp)` from `ProcessDefense`
	// (`source2\geServer\ZServerLib\FieldChar.cpp:645-651`) where `TakeDamage` is
	// `CMonsterNPC::TakeDamage` (`MonsterNPC.cpp:652-703`): it reads `int hp = GetHP()`
	// BEFORE applying the damage and returns that same value. Sending the AFTER value
	// would make the client compute `0 - damage` as the plate.
	hp := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	if !(hp <= damage) {
		t.Fatalf("the killing hit says HP-before %g > damage %g, so the client will NOT "+
			"call SetDeadlyHit and no death animation can run (GeAttack.cpp:42):\n%s",
			hp, damage, logs.String())
	}
	if hp != 1 {
		t.Errorf("the killing hit reports HP-before %g, but this fixture's body had 1 HP "+
			"- the field is not the pre-hit value:\n%s", hp, logs.String())
	}

	// TYPE: 6 is the live client's knockdown (`history\protocol-recovery.md`: the
	// 111-byte form goes with type 6/7; the Rebo kill is 111/type 6). Types 2 and 3
	// are the 74-byte forms and carry no block (`history\splash-attack.md`).
	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != knockdownAttackType {
		t.Fatalf("the killing hit's type is %d, want %d - the type that carries the KD "+
			"block:\n%s", got, knockdownAttackType, logs.String())
	}

	// THE KILLER: +4 is the handle `geattack::Attack` passes to SetDeadlyHit, i.e. the
	// `GetKillMeActor()` the client compares against the actor that swung.
	if got := binary.LittleEndian.Uint32(hit[asAttacker:]); got != p.sess.Team[0].Handle {
		t.Fatalf("the killing hit's attacker is %d, want the character that swung %d - "+
			"the client compares GetKillMeActor() with the attacker actor, so a wrong "+
			"handle means the fall never matches:\n%s",
			got, p.sess.Team[0].Handle, logs.String())
	}
	if got := binary.LittleEndian.Uint32(hit[asTarget:]); got != capturedTarget {
		t.Fatalf("the killing hit's target is %d, want %d", got, capturedTarget)
	}

	// THE KD BLOCK, at +74 and nowhere else. Its startPos is the body's own position,
	// and its landing point / hAngle are the reference's own geometry: 500 units along
	// attacker -> target (the reference's bounce calculator with power 100, vAngle 45,
	// gravity 25 and two bounces gives 400 + 100 = 500.00 - `FieldChar.cpp:780-812`),
	// with hAngle the bearing of that displacement. The three surviving kill frames
	// measure 499.6 / 499.6 / 500.7 units and an hAngle equal to the bearing.
	start := worldPos(hit[asKDStartPos:])
	end := worldPos(hit[asKDEndPos:])
	stand := worldPos(hit[asTargetPos:])
	atk := worldPos(hit[asAtkPos:])
	if start != stand {
		t.Errorf("KD startPos is %v, want the body's own position %v", start, stand)
	}
	dx := float64(stand[0] - atk[0])
	dy := float64(stand[1] - atk[1])
	dist := math.Hypot(dx, dy)
	if dist == 0 {
		t.Fatalf("the fixture's attacker stands ON its target, so there is no fall " +
			"direction to check")
	}
	wantEnd := [3]int32{
		stand[0] + int32(math.Round(dx/dist*kdFallDistance)),
		stand[1] + int32(math.Round(dy/dist*kdFallDistance)),
		stand[2],
	}
	if end != wantEnd {
		t.Errorf("KD endPos is %v, want %v - the body's position plus %g units along "+
			"attacker %v -> target %v", end, wantEnd, kdFallDistance, atk, stand)
	}
	if got := math.Hypot(float64(end[0]-start[0]), float64(end[1]-start[1])); math.Abs(got-kdFallDistance) > 1 {
		t.Errorf("the KD lands %g units away, want %g (the reference's own bounce "+
			"distance for power %d / vAngle %g)", got, kdFallDistance, kdPower, kdVAngleDeg)
	}
	wantAngle := math.Atan2(dy, dx) * 180 / math.Pi
	if got := math.Float32frombits(binary.LittleEndian.Uint32(hit[asKDHAngle:])); math.Abs(float64(got)-wantAngle) > 0.01 {
		t.Errorf("KD hAngle is %v, want the bearing of the fall %v (start %v -> end %v)",
			got, wantAngle, start, end)
	}
	if end[2] != stand[2] {
		t.Errorf("KD endPos cell is %d, want the body's own %d", end[2], stand[2])
	}
	if got := binary.LittleEndian.Uint16(hit[asKDDelay:]); got != kdDelayMs {
		t.Errorf("KD delay is %d, want %d", got, kdDelayMs)
	}
	if got := binary.LittleEndian.Uint16(hit[asKDPower:]); got != kdPower {
		t.Errorf("KD power is %d, want %d", got, kdPower)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(hit[asKDVAngle:])); got != float32(kdVAngleDeg) {
		t.Errorf("KD vAngle is %v, want %v", got, float32(kdVAngleDeg))
	}
	if got := hit[asKDType]; got != kdTypeNormal {
		t.Errorf("KD type byte is %d, want %d (KDT_NORMAL)", got, kdTypeNormal)
	}

	// AND THE LOG NAMES WHAT WENT OUT: 111 B and type 6, not the 74/2 of an ordinary hit.
	if !strings.Contains(logs.String(), "cmd 3564 (111 B, type 6") {
		t.Errorf("the ATTACK line does not state the killing packet's size and type:\n%s",
			logs.String())
	}
}

// THE PACKET AND THE DEATH CANNOT DISAGREE. The same hit that builds the 111-byte form
// must be the hit the LEDGER armed the death on: the 3557 that follows carries the HP
// after the hit (0), the ledger holds 0, and the zone owes exactly one ZC_DEAD - for
// this handle. `takeHP` arms on `before > 0 && after == 0` (zone\create.go), the same
// test `attackAt` selects the kill form with, so a divergence here is a kill the
// client would be told about twice - or a fall with no death behind it.
func TestKillingBlowAgreesWithTheDeathTheZoneArmed(t *testing.T) {
	h, logs, p, _ := deadZone(t)

	pkts := pressAttack(t, h, p.sess, capturedTarget)
	var add []byte
	hit := 0
	for _, pkt := range pkts {
		if len(pkt) < 2 {
			continue
		}
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle:
			hit++
		case CmdZCAddHP:
			add = pkt
		}
	}
	if hit != 1 {
		t.Fatalf("the killing press sent %d %d packet(s), want exactly 1:\n%s",
			hit, CmdZCAttackSingle, logs.String())
	}
	if add == nil {
		t.Fatalf("the killing press sent no %d - the plate would not move:\n%s",
			CmdZCAddHP, logs.String())
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:])); got != 0 {
		t.Errorf("the 3557 after the kill carries %v, want the HP after the hit, 0", got)
	}
	if hp, ok := h.monsterLiveHP(capturedTarget); !ok || hp != 0 {
		t.Errorf("the ledger holds %v (known %v), want 0", hp, ok)
	}
	// The death belongs to the corpse's own clock, not to the press (2026-11-09).
	deadTick(t, h, capturedTarget)
	deaths := deadLines(logs.String())
	if len(deaths) != 1 || !deaths[capturedTarget] {
		t.Fatalf("the kill wrote %d death line(s) %v, want exactly one for %d",
			len(deaths), deaths, capturedTarget)
	}
}

// A HIT THAT DOES NOT KILL KEEPS THE MEASURED 74-BYTE FORM. The KD block is a
// killing-blow shape here: an ordinary hit must not knock the monster down (nothing
// else in this zone does), and 56 of 56 live samples of a plain hit are 74 bytes at
// type 2 with no block after them.
func TestNonKillingHitKeepsTheMeasuredForm(t *testing.T) {
	h, logs, p := attackZone(t) // the Scarecrow has 5000 HP here

	var hit []byte
	for _, pkt := range pressAttack(t, h, p.sess, capturedTarget) {
		if len(pkt) >= 2 && binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			hit = pkt
		}
	}
	if hit == nil {
		t.Fatalf("the press sent no %d:\n%s", CmdZCAttackSingle, logs.String())
	}
	if len(hit) != AttackSingleSize {
		t.Fatalf("a non-killing %d is %d bytes, want the measured %d:\n%s",
			CmdZCAttackSingle, len(hit), AttackSingleSize, logs.String())
	}
	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != plainAttackType {
		t.Errorf("a non-killing hit's type is %d, want %d", got, plainAttackType)
	}
	hp := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	if hp <= damage {
		t.Errorf("this fixture's hit is lethal (HP %g <= damage %g), so it cannot test "+
			"the non-killing form", hp, damage)
	}
}

// THE PACKET, READ THE WAY THE CLIENT READS IT.
//
// Every other test in this file parses with the BUILDER's constants. This one parses
// with the offsets taken out of the client's own handler (`FUN_004e220b`,
// `docker-port\out\ge_decompiled.txt:162691`), so a builder constant that drifts away
// from the reader fails here even when every other test still agrees with itself.
//
//	+0x04 OBJHANDLE attacker -> the actor `SetDeadlyHit` is handed (the killer)
//	+0x1a OBJHANDLE target
//	+0x2a FLOAT64   damage
//	+0x32 FLOAT64   HP BEFORE the hit
//	+0x3a INT32     type     -> 4..7 take the KD path, `packet + 0x4a` is the block
//	+0x3e 12 bytes  fall pos -> copied only when there is NO KD block
//	+0x4a 37 bytes  KNOCKDOWN_INFO -> startPos +0, endPos +0xc, delay +0x18,
//	                                 power +0x1a, vAngle +0x1c, hAngle +0x20, type +0x24
//
// The death gate in `FUN_0061379f` is `HP-before == damage || HP-before < damage`
// (the decompile's `*(double *)(param_4 + 6) == / < *(double *)(param_4 + 4)`), i.e.
// `hp <= damage`, and that is asserted below on the client's own +0x32/+0x2a.
func TestKillingBlowParsesAtTheClientsOwnOffsets(t *testing.T) {
	h, logs, p, _ := deadZone(t)

	var hit []byte
	for _, pkt := range pressAttack(t, h, p.sess, capturedTarget) {
		if len(pkt) >= 2 && binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			hit = pkt
		}
	}
	if hit == nil {
		t.Fatalf("the killing press sent no %d:\n%s", CmdZCAttackSingle, logs.String())
	}

	// THE CLIENT'S OFFSETS, spelled out here so the builder cannot agree with itself.
	const (
		cAttacker = 0x04
		cTarget   = 0x1a
		cDamage   = 0x2a
		cHP       = 0x32
		cType     = 0x3a
		cFallPos  = 0x3e
		cKD       = 0x4a
	)

	// SIZE AND FRAME: 0x4a + 37 = 111, and the length word at +2 says so.
	if len(hit) != 111 {
		t.Fatalf("killing %d is %d bytes, the client's KD pointer + 37 needs 111:\n%s",
			CmdZCAttackSingle, len(hit), logs.String())
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Fatalf("length word %d, frame %d - the splitter would cut it elsewhere", got, len(hit))
	}

	// WHO KILLED IT: +0x04 becomes the actor `SetDeadlyHit` stores, and the fall's
	// third condition is `pAtker == GetKillMeActor()` (GENormalObject.cpp:1161).
	if got := binary.LittleEndian.Uint32(hit[cAttacker:]); got != p.sess.Team[0].Handle {
		t.Errorf("attacker +0x04 is %d, want the swinging character %d", got, p.sess.Team[0].Handle)
	}
	if got := binary.LittleEndian.Uint32(hit[cTarget:]); got != capturedTarget {
		t.Errorf("target +0x1a is %d, want %d", got, capturedTarget)
	}

	// THE DEATH GATE, on the client's own fields.
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[cDamage:]))
	hpBefore := math.Float64frombits(binary.LittleEndian.Uint64(hit[cHP:]))
	if damage <= 0 {
		t.Errorf("damage +0x2a is %g - a non-positive hit can never arm the death", damage)
	}
	if hpBefore <= 0 {
		t.Errorf("HP +0x32 is %g, want this body's 1 HP before the hit", hpBefore)
	}
	if !(hpBefore <= damage) {
		t.Errorf("the client's own gate fails: HP-before %g > damage %g, so FUN_0061379f "+
			"never calls SetDeadlyHit and Dead() will Die() instantly", hpBefore, damage)
	}

	// THE TYPE: 4..7 is the KD path; 6 is what the live reference's killing blows carry.
	typ := int32(binary.LittleEndian.Uint32(hit[cType:]))
	if typ != knockdownAttackType {
		t.Errorf("type +0x3a is %d, want %d", typ, knockdownAttackType)
	}
	if typ < 4 || typ > 7 {
		t.Errorf("type %d is outside the client's KD set 4..7 - FUN_004e220b passes no KD "+
			"pointer for it, so no fall can start", typ)
	}

	// THE 12 BYTES AT +0x3e: the fall position the client copies when the type gave it
	// no KD pointer. In the killing form it is still written (the reference always
	// writes it), and it must be the body's own spot.
	stand := worldPos(hit[asTargetPos:])
	if got := worldPos(hit[cFallPos:]); got != stand {
		t.Errorf("fall position +0x3e is %v, want the body's own %v", got, stand)
	}

	// THE KD BLOCK AT +0x4a: the pointer the handler computes is exactly `packet + 0x4a`.
	if cKD+KnockDownInfoSize != len(hit) {
		t.Fatalf("the KD block would end at %d, the frame is %d", cKD+KnockDownInfoSize, len(hit))
	}
	if got, want := worldPos(hit[cKD:]), stand; got != want {
		t.Errorf("KD startPos is %v, want the body's own %v", got, want)
	}
	end := worldPos(hit[cKD+12:])
	if end == stand {
		t.Errorf("KD endPos is the startPos %v - the fall would not move", end)
	}
	if got := binary.LittleEndian.Uint16(hit[cKD+0x18:]); got != kdDelayMs {
		t.Errorf("KD delay is %d, want %d ms", got, kdDelayMs)
	}
	if got := binary.LittleEndian.Uint16(hit[cKD+0x1a:]); got != kdPower {
		t.Errorf("KD power is %d, want %d", got, kdPower)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(hit[cKD+0x1c:])); got != float32(kdVAngleDeg) {
		t.Errorf("KD vAngle is %v, want %v", got, float32(kdVAngleDeg))
	}
	dx := float64(stand[0] - worldPos(hit[asAtkPos:])[0])
	dy := float64(stand[1] - worldPos(hit[asAtkPos:])[1])
	wantAngle := math.Atan2(dy, dx) * 180 / math.Pi
	if got := math.Float32frombits(binary.LittleEndian.Uint32(hit[cKD+0x20:])); math.Abs(float64(got)-wantAngle) > 0.01 {
		t.Errorf("KD hAngle is %v, want the bearing of attacker -> target %v", got, wantAngle)
	}
	if got := hit[cKD+0x24]; got != kdTypeNormal {
		t.Errorf("KD type byte is %d, want %d (KDT_NORMAL)", got, kdTypeNormal)
	}
}
