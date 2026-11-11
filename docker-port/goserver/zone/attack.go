package zone

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"time"

	"geserver/protocol"
)

// THE PLAIN ATTACK: CZ_ATTACK 3666 in, ZC_ATTACK_SINGLE 3564 then ZC_ADD_HP 3557
// out, and ZC_ATTACK_DISABLE 3568 when the attack is refused.
//
// WHY THIS EXISTS. The client sends 3666 the moment the player left-clicks a
// monster, and until this file the zone answered nothing at all - the request fell
// into Reply's default branch and logged `cmd 3666 not handled yet (skipped N
// time(s))` while the click did nothing on screen.
//
// EVERY OFFSET HERE IS MEASURED, NOT DERIVED. The source is
// `docker-port\history\combat-attack-heal.md` - the owner's team-combat demo of
// 2026-10-31 (20 attacks, 56 replies, 126 HP changes), cut into the fixtures in
// `zone\testdata\`:
//
//	live_cz_attack.bin      15 B  CZ_ATTACK 3666        (§1: 20 samples, all 15 B)
//	live_attack_single.bin  74 B  ZC_ATTACK_SINGLE 3564 (§2: 56 samples, all 74 B)
//	live_add_hp_party.bin   23 B  ZC_ADD_HP 3557        (§3.4: 126 samples, all 23 B)
//
//	CZ_ATTACK 3666 (15 B)   +2 DWORD seq, +6 int checksum, +10 BYTE charIndex,
//	                        +11 OBJHANDLE target
//	ZC_ATTACK_SINGLE 3564 (74 B)
//	  +2 WORD the whole length (74), +4 attacker handle, +8/+12/+16 the attacker's
//	  WORLDPOS, +20 WORD attackSpd, +22 DWORD startTime (0 in 56/56), +26 target
//	  handle, +30/+34/+38 the target's WORLDPOS, +42 FLOAT64 damage,
//	  +50 FLOAT64 the target's HP BEFORE this hit, +58 int32 attack type
//	  (2 = KNOCKDOWN, 3 = CRITICAL_KD), +62 WORLDPOS the knock-back position -
//	  which equals +30 in 56 of 56 samples, so it is a copy of the target's spot.
//	ZC_ADD_HP 3557 (23 B)   +2 the same handle, +6 FLOAT64 the HP AFTER,
//	                        +14 FLOAT64 the delta, +22 BYTE 1 only when the delta
//	                        is negative (§3.4: 20 of 20 and 106 of 106).
//
// THE THREE THINGS §7 MEASURED THAT A BUILDER MUST NOT GET WRONG:
//
//  1. THE DAMAGE IS A FLOAT64 AT +42, and the SAME DOUBLE must appear as the 3557's
//     delta in the same tick (§2.1, §5): the client draws the flying number from
//     one and moves the health plate from the other, so two different numbers are
//     two contradictory pictures on one screen.
//  2. +50 IS THE HP BEFORE THE HIT (§2.1: "as the server had it when it built the
//     reply, not the HP after the subtraction") and the 3557's +6 is the HP after.
//     A build that swaps them puts the plate one hit behind.
//  3. 3564 CARRIES NO "FOR WHOM" FLAG (§4). The same 74 bytes serve the attacker,
//     the other two characters in the party and any other player in the zone, so
//     the bytes are built ONCE and handed to everyone - a per-recipient rebuild is
//     where a hit turns into a different hit on somebody else's screen.
//
// WHAT THIS FILE DOES NOT DO, said out loud so a reader does not look for it:
//
//   - No death, no drops, no EXP, no 3531 ZC_LEAVE_MONSTER. A monster's HP can
//     reach 0 here (§7 measured the reference's own dummy bouncing around 0) and
//     the log says so; killing is another lane.
//   - No critical. The reference sent type 3 four times in 56 (§2), which is ITS
//     roll. This zone has no critical model, so every hit is type 2 - and type 2 is
//     also what makes the packet the measured 74 bytes, because a knockdown flavour
//     is the one that carries the trailing 12-byte position (§2.2).
//   - No second 3564 per REQUEST. §7.3 item 2 measured 56 replies to 20 attacks and
//     could NOT attribute a reply to a request (the two streams run concurrently
//     and the capture has no clock), so this file answers ONE request with ONE hit.
//     THE RHYTHM IS A SEPARATE THING AND IT IS NOW BUILT (R94): the client sends one
//     3666 per lock and then stops, so the SERVER keeps swinging at the character's
//     own ASPD - zone\attack_swing.go, which is the reference's AS_ATTACK loop
//     (FieldChar_FSM.cpp:121-160) and which replays THIS path for every swing. The
//     capture is not contradicted by that: 56 replies to 20 requests is what a fight
//     at ~1 s per swing looks like in a recording with no timestamps, and it never was
//     evidence for one hit per click.

const (
	// CmdCZAttack is CZ_ATTACK, client -> server, 15 bytes FIXED:
	// cmd(2) seq(4) checksum(4) charIndex(1) target(4). The size is the client's own
	// table (`data\cmd_table_live_z.txt:1333` "3666 15 CZ_ATTACK") and every one of
	// the 20 captured samples is 15 bytes (§1).
	CmdCZAttack = 3666

	czAttackSize = 15

	// CZ_ATTACK's two fields, at the offsets §1 read out of 20 samples.
	caChecksum  = 6  // int32 - the client's own GetChecksum() over the packet
	caCharIndex = 10 // BYTE - the acting character's slot in THIS session's team
	caTarget    = 11 // OBJHANDLE - 1602174 (the dummy) in all 20 samples

	// CmdZCAttackSingle is ZC_ATTACK_SINGLE, server -> client, 74 bytes FIXED. The
	// registered size is 0 (variable), so the packet carries its own length at +2.
	CmdZCAttackSingle = 3564

	// AttackSingleSize is the whole packet: 2+2+4+12+2+4+4+12+8+8+4+12 = 74, no gap.
	AttackSingleSize = 74

	// 3564's fields.
	asAttacker   = 4  // OBJHANDLE the character that swung
	asAtkPos     = 8  // WORLDPOS - the attacker's own position and navmesh cell
	asAtkSpd     = 20 // WORD - the character's attack speed
	asStartTime  = 22 // DWORD - 0 in all 56 samples, and 0 in the older capture too
	asTarget     = 26 // OBJHANDLE the monster that was hit
	asTargetPos  = 30 // WORLDPOS - the monster's position and cell
	asDamage     = 42 // FLOAT64 the damage - THE number the client draws
	asTargetHP   = 50 // FLOAT64 the target's HP BEFORE this hit
	asAttackType = 58 // int32 - the client's ATTACKTYPE_* value
	asKnockPos   = 62 // WORLDPOS the knock-back position (= the target's own, 56/56)

	// CmdZCAttackDisable is ZC_ATTACK_DISABLE, server -> client, 15 bytes: the
	// client switches the attack OFF for this character instead of waiting for an
	// answer that never comes. The reference sent it 4 times in the same capture
	// (§7.4), so a refusal path really exists; this zone uses it for its own.
	//
	// THE LAYOUT IS THE SHIPPED SOURCE'S OWN STRUCT
	// (`source2\geShared\include\geZCPacket.h:1454`):
	//
	//	struct PKS_ZC_ATTACK_DISABLE : public PKS_DEFAULT {
	//	    OBJHANDLE handle;        // +2
	//	    OBJHANDLE targetHandle;  // +6
	//	    BYTE      type;          // +10
	//	    DWORD     value;         // +11
	//	};
	//
	// and 2+4+4+1+4 = 15, which is the size the client's own table registers
	// (`data\cmd_table_live_z.txt:1235` "3568 15 ZC_ATTACK_DISABLE").
	CmdZCAttackDisable = 3568

	AttackDisableSize = 15

	adHandle = 2  // OBJHANDLE the character whose attack is switched off
	adTarget = 6  // OBJHANDLE the target it was aimed at
	adType   = 10 // BYTE the reason
	adValue  = 11 // DWORD the reason's argument (an item id, for NO_ITEM)

	// The two reasons, from the client's own enum (`geShared\include\geZCTypes.h:551`):
	//
	//	ATTACK_DISABLE_NO_ITEM        = 0  shows a warning naming the item at `value`
	//	ATTACK_DISABLE_INVALID_TARGET = 1  drops the AI target if it matches
	//	                                   `targetHandle` (GENormalObject.cpp:1732)
	//
	// A refusal here is always 1: the character may attack, THIS target is not one
	// it can hit. Type 0 would make the client look `value` up in the item table and
	// assert on a row that is not there.
	attackDisableInvalidTarget = 1

	// plainAttackType is the ATTACKTYPE the plain attack reports: 2, which the LIVE
	// client collapses to "normal, no critical" (its 3564 reader branches on +0x3a
	// and takes 0..4 to 0 - `history\protocol-recovery.md`, "the 111-byte block is
	// read by nobody"). The shipped source's enum numbering (0 NORMAL, 2 KNOCKDOWN,
	// `geZCTypes.h:512`) is NOT the live client's: 2 is the value every measured
	// plain hit carries, and the reference sends that for the same ordinary swing.
	//
	// 3 (CRITICAL) is never sent: a critical is a roll this zone does not make, and
	// §5 says the ratio does not have to match the reference's.
	plainAttackType = 2

	// knockdownAttackType is the type a KILLING blow of a body WITHOUT an original
	// dead animation carries: 6, the plain knockdown. Two independent things agree.
	//
	//   - The REFERENCE SERVER makes exactly this call. `CFieldChar::OnSingleHit`
	//     (`source2\geServer\ZServerLib\FieldChar_Message.cpp:160-166`):
	//
	//	if ((0 >= GetHP() && false == IsOriginalDeadAni()) ||
	//	    (0 < ret->GetDamage() && true == TakeKDRank(ret->GetKDRank()))) {
	//	    ret->m_isKnockDown = true;
	//	    SetKnockDown(from->GetPos(), ..., ret->m_kDownPos);
	//	}
	//
	//     i.e. a monster whose model has no dead animation, brought to 0 HP, IS a
	//     knockdown - and `CFieldChar::GetHitInfo` (`FieldChar.cpp:386-400`) then
	//     writes ATTACKTYPE_KNOCKDOWN and appends the 37-byte KNOCKDOWN_INFO.
	//   - The only killing blows the live wire has shown are that form: 111 bytes at
	//     type 6 (`captures\kill01\20261105-214033`) and types 6/7
	//     (`captures\punisher20261101b\20261101-121054`). Monster_Bigfoot 11591 has
	//     OriginalDEADAni="NO" (`cache\datatable_monster_shape.xml:703`), so it is
	//     exactly the body that rule knocks down.
	//
	// WHY 6, AND WHAT THE CLIENT DOES WITH IT: the live client's 3564 handler is
	// `FUN_004e220b` (`docker-port\out\ge_decompiled.txt:162691`, the 2020 client we
	// run). It reads +4 attacker, +8 attacker pos, +0x14 attackSpd, +0x1a target,
	// +0x2a damage (f64), +0x32 HP-before (f64) and +0x3a type, then branches:
	//
	//	iVar2 = *(int *)(packet + 0x3a);
	//	if (type == 0)       packed = 0;
	//	else if (type < 4)   packed = type;                    // 1..3: NO KD pointer
	//	else if (type == 4)  packed = (packet + 0x4a) << 32;   // KD, attackType 0
	//	else if (type <= 7)  packed = (packet + 0x4a) << 32 | type;  // 5/6/7: KD
	//	FUN_0061379f(&attacker@+4, &pos@+8, spd@+0x14, &target@+0x1a, packed);
	//
	// The high half of `packed` is the KD block's ADDRESS - `FUN_004e11b1` is literally
	// `return param_1 + 0x4a` (`ge_decompiled.txt:161957`) - and `FUN_0061379f` hands
	// it to `FUN_004e5fd3`/`FUN_004e5f2a`, which read startPos +0, endPos +0xc,
	// delay +0x18, power +0x1a, vAngle +0x1c, hAngle +0x20 and type +0x24 out of it
	// and set the target's KD state (`actor + 0x3c4 = 1`, the flag `IsReadyKd` reads).
	// So the 37 bytes are READ, at +74, for types 4..7 - and a type 0..3 hit has no
	// KD pointer at all and copies the 12 bytes at +0x3e as the fall position
	// instead. 6 is the KD type the live reference's own killing blows carry; the
	// block below is what the fall is built from, not decoration.
	//
	// THE ONE DIVERGENCE, NAMED: this zone sends the knockdown form on EVERY killing
	// blow, where the reference does it only for a body with no original dead
	// animation (or a KDRank roll, which this zone does not model). For a model WITH
	// a dead animation the difference is invisible on screen: `CActor::FinalHit`
	// (`geClientShared\source\GeActor.cpp:539-558`) plays the model's own dead
	// animation and clears isKnockDown before the KD can start.
	knockdownAttackType = 6

	// KnockDownInfoSize is the block the live wire appends to a knockdown hit:
	// 12 startPos + 12 endPos + 2 delay + 2 power + 4 vAngle + 4 hAngle + 1 type = 37
	// (`geZCTypes.h:298`, measured byte for byte in `history\splash-attack.md`).
	KnockDownInfoSize = 37

	// AttackSingleKnockdownSize is the killing-blow form: the 74-byte hit plus the
	// 37-byte block. It is the 111-byte `ZC_ATTACK_SINGLE` the record already holds
	// (`history\protocol-recovery.md`, 0x6f length word twice in the punisher
	// capture) - 74 + 37, nothing else.
	AttackSingleKnockdownSize = AttackSingleSize + KnockDownInfoSize

	// The KD block's fields, as absolute offsets in the 111-byte form. The block
	// starts where the 74-byte form ends, +74, AFTER the 12-byte knock-back WORLDPOS
	// at +62. That 12-byte gap is why the live form is 74 and not 62: the live hit
	// block is 36 bytes (damage and HP are 8 bytes each) where the shipped `HIT_INFO`
	// is 28 with two ints (`geZCTypes.h:542-549`), and the block sits at +74 in every
	// measured kill frame - never at +62, where the shipped source's reader would look.
	asKDStartPos = AttackSingleSize  // WORLDPOS - the position the body falls from
	asKDEndPos   = asKDStartPos + 12 // WORLDPOS - the position it lands on
	asKDDelay    = asKDEndPos + 12   // WORD    - how long it stays down
	asKDPower    = asKDDelay + 2     // WORD    - 100 in 10 of 10 measured blocks
	asKDVAngle   = asKDPower + 2     // float   - 45.0 in 10 of 10
	asKDHAngle   = asKDVAngle + 4    // float   - the bearing of startPos -> endPos
	asKDType     = asKDHAngle + 4    // BYTE    - KD_TYPE, 1 = KDT_NORMAL in 10/10

	// The block's values, as the reference builds them and as the three surviving
	// kill frames measure them. `CFieldChar::SetKnockDown`
	// (`source2\geServer\ZServerLib\FieldChar.cpp:780-812`) starts at the body's own
	// position, takes power / vAngle / type from the hit result, and slides the body
	// by its bounce calculator's total distance along the bearing of
	// (target - attacker). That distance is NOT a constant chosen here: with gravity
	// 25, friction 0.5, delay 0.5 and TWO bounces (`imclib\Include\imcBounceCalculator.h:8`),
	// power 100 / vAngle 45 gives 100*cos45 * (2*100*sin45/25) = 400 for the first
	// bounce and 100 more for the second - 500.00 exactly. The measured frames land
	// 499.6 / 499.6 / 500.7 units out, with the packet's hAngle equal to the bearing
	// of that displacement to three decimals (170.640, 127.142, -66.401).
	//
	// `kdDelayMs` is the reference's own `GetDownTime()` - a MONSTER ROW value, 569 /
	// 757 / 689 ms in the three measured blocks. 600 sits inside the measured band;
	// this zone does not read that column yet, and says so rather than pretending.
	kdDelayMs      = 600
	kdPower        = 100
	kdVAngleDeg    = 45.0
	kdFallDistance = 500.0
	kdTypeNormal   = 1 // KD_TYPE KDT_NORMAL (`geShared.h:126`)

	// plainAttackPercent is the plain attack's "skill factor" in the ONE damage
	// function (castDamage, zone\skill_cast.go). A plain attack has no row in
	// datatable_skill.xml, so there is no SklATK to read: it is the character's ATK
	// at 100%. That keeps the formula in one place - see castDamage.
	plainAttackPercent = 100
)

// AttackRequest is one decoded CZ_ATTACK.
type AttackRequest struct {
	// CharIndex is the client's team slot: the position in THIS connection's own
	// team list, the same field CZ_CHANGE_STANCE and CZ_SKILL carry. §1 proved the
	// packet is SELF-IDENTIFYING (the byte was 1 and 2 for the two characters that
	// fought), so the attacker is READ, not inferred from the socket.
	CharIndex int
	// Target is the handle the player clicked. It is only trusted after
	// Handler.monsterByHandle finds it among the monsters THIS zone is running.
	Target uint32
	// Checksum is the DWORD the client stored at +6 - its own GetChecksum() over
	// the packet. See czChecksum for what this zone does with it.
	Checksum uint32
}

// ParseAttack reads CZ_ATTACK (3666):
//
//	+0 cmd(2)=3666  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 target(4)
//
// ok is false when the frame is not exactly the 15 bytes the client's table
// registers. EXACT, not "at least": 3666 is a FIXED-size packet, the splitter cuts
// it at the declared size, and a frame of another length means the table and the
// bytes disagree - the extra bytes would be read as fields.
func ParseAttack(pt []byte) (AttackRequest, bool) {
	var req AttackRequest
	if len(pt) != czAttackSize {
		return req, false
	}
	req.CharIndex = int(pt[caCharIndex])
	req.Target = binary.LittleEndian.Uint32(pt[caTarget:])
	req.Checksum = binary.LittleEndian.Uint32(pt[caChecksum:])
	return req, true
}

// czChecksum recomputes the client's GetChecksum() over one CZ packet: ONE 32-bit
// running value, XOR the bytes at EVEN indexes into it, ADD the bytes at ODD ones
// to it, with the checksum field itself (+6..+9) held at zero.
//
// MEASURED, not guessed: `source2\geShared\include\gePacketBase.h:25` declares
// GetChecksum, and §1 says it recomputes on 236 of the 236 client packets in the
// capture. It reproduces the 3666 fixture by hand too -
//
//	XOR 0x52, +0x0e, XOR 0x4a, XOR 0x01, +0x7e, XOR 0x72, XOR 0x18 = 0xf3 = 243
//
// which is the DWORD the client stored at +6 (TestParseAttackReadsTheLiveFixture).
//
// TWO DETAILS THAT LOOK LIKE DETAILS AND ARE NOT, both checked against the four
// c2s fixtures this tree holds (3666, 3625, 3646, 3647 - 15, 15, 19 and 27 bytes):
//
//   - The accumulator is NOT cut to a byte. 3647's stored value is 0x2ac = 684,
//     which no byte-wide sum can produce. Masking it to 0xFF would have matched
//     the three SHORT fixtures and quietly disagreed on every longer packet.
//   - It is ONE running value, not two totals XOR-ed together at the end: the
//     separate-totals reading gives 0x10f on the 3666 fixture, not 0xf3.
//
// WHAT THIS ZONE DOES WITH A BAD ONE: it LOGS it and ACCEPTS the attack. §7.3 item
// 4 says plainly that no sample exists of the reference server refusing a bad
// checksum, so refusing would be inventing a rule - and the checksum is not a
// security measure anyway (a forged packet can compute it; it is the client's own
// arithmetic). It is worth a line because a MISMATCH means this reader and the
// client disagree about the packet, which is a bug worth seeing.
func czChecksum(pt []byte) uint32 {
	var acc uint32
	for i, b := range pt {
		if i >= caChecksum && i < caChecksum+4 {
			continue // the field itself is zero while it is computed
		}
		if i%2 == 0 {
			acc ^= uint32(b)
			continue
		}
		acc += uint32(b)
	}
	return acc
}

// BuildAttackSingle packs one ZC_ATTACK_SINGLE (3564), 74 bytes: the hit, and the
// packet the client draws the damage number from.
//
//	+0  WORD      3564
//	+2  WORD      74 (the whole packet - the registered size is 0, "variable")
//	+4  OBJHANDLE the attacker
//	+8  WORLDPOS  the attacker's position (x, y, navmesh cell)
//	+20 WORD      attackSpd
//	+22 DWORD     startTime - always 0: 0 in all 56 samples (§2)
//	+26 OBJHANDLE the target
//	+30 WORLDPOS  the target's position (x, y, navmesh cell)
//	+42 FLOAT64   the damage
//	+50 FLOAT64   the target's HP BEFORE this hit
//	+58 int32     attack type
//	+62 WORLDPOS  the knock-back position - the target's own, copied
//
// `attackSpd` is a WORD and the sheet's CSPD is a float, so the caller converts
// once and this builder never rounds: a 0 here is a visible "no weapon speed" on
// the client, not a crash, and the caller logs the number it passed.
func BuildAttackSingle(attacker uint32, attackPos [3]int32, attackSpd uint16,
	target uint32, targetPos [3]int32, damage, targetHPBefore float64,
	attackType int32) []byte {

	out := make([]byte, AttackSingleSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCAttackSingle)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	binary.LittleEndian.PutUint32(out[asAttacker:], attacker)
	putWorldPos(out[asAtkPos:], attackPos)
	binary.LittleEndian.PutUint16(out[asAtkSpd:], attackSpd)
	// +22 startTime is left ZERO: 0 in all 56 samples (§2), so it is the capture's
	// value and not a field this zone has an opinion about.
	binary.LittleEndian.PutUint32(out[asTarget:], target)
	putWorldPos(out[asTargetPos:], targetPos)
	binary.LittleEndian.PutUint64(out[asDamage:], math.Float64bits(damage))
	binary.LittleEndian.PutUint64(out[asTargetHP:], math.Float64bits(targetHPBefore))
	binary.LittleEndian.PutUint32(out[asAttackType:], uint32(attackType))
	// THE KNOCK-BACK POSITION IS THE TARGET'S OWN POSITION, and that is measured,
	// not tidy: §2 found +62 equal to +30 in 56 of 56 samples.
	putWorldPos(out[asKnockPos:], targetPos)
	return out
}

// BuildAttackSingleKnockdown packs the KILLING-BLOW form of ZC_ATTACK_SINGLE: the
// same 74-byte hit, plus the 37-byte KNOCKDOWN_INFO the REFERENCE sends with it.
//
//	+74 WORLDPOS startPos - where the body falls from (its own position)
//	+86 WORLDPOS endPos   - where it lands, 500 units along attacker -> target
//	+98 WORD     delay    - how long it stays down, 600 ms
//	+100 WORD    power    - 100
//	+102 float   vAngle   - 45.0
//	+106 float   hAngle   - the bearing of startPos -> endPos, in degrees
//	+110 BYTE    type     - KD_TYPE 1 (KDT_NORMAL)
//
// WHAT MAKES THE BODY FALL IS THE TYPE, AND THIS BLOCK IS WHAT THE TYPE READS. The
// live client's 3564 handler (`FUN_004e220b`, `docker-port\out\ge_decompiled.txt:162691`)
// branches on the type at +0x3a: types 4..7 hand `packet + 0x4a` to the attack call as
// the KNOCKDOWN_INFO pointer (`FUN_004e11b1` is `return param_1 + 0x4a`), and
// `FUN_004e5fd3`/`FUN_004e5f2a` read this block's startPos +0, endPos +0xc, delay +0x18,
// power +0x1a, vAngle +0x1c, hAngle +0x20 and type +0x24 from it and set the target's
// KD state. Types 0..3 pass no pointer and copy the 12 bytes at +62..+74 as the fall
// position instead. So the block is here because the client consumes it, and because
// the REFERENCE writes it on every knockdown
// (`CFieldChar::GetHitInfo`, `FieldChar.cpp:386-400`) and the live wire's killing
// blows are the 111-byte form (`captures\kill01\20261105-214033`: 111 bytes, type 6;
// `captures\punisher20261101b\20261101-121054`: 111 bytes, types 6 and 7), so a
// 111-byte form keeps our bytes the same shape as its own.
//
// THOSE TWO CAPTURES ARE NO LONGER ON DISK (checked 2026-11-07 - only
// `data\captures\liveattack01\` and the `captures\*` logs remain), so the 111 bytes are
// not re-measurable today. They do not need to be: the client's own handler is the
// proof that the form is 74 + 37 at +0x4a, and the frame this builder writes is the one
// that handler consumes.
//
// WHY THE BLOCK STARTS AT +74. The live hit block is 36 bytes (8-byte damage and HP)
// where the shipped `HIT_INFO` is 28 with two ints (`geZCTypes.h:542-549`), so the
// struct the live client advances is 74 bytes - and +62 keeps the 12-byte knock-back
// position in every sample. The shipped source's `KNOCKDOWN_INFO` reader would look at
// +62, which is exactly why that offset cannot be the one the live client runs.
//
// WHERE IT LANDS IS THE REFERENCE'S OWN GEOMETRY (see knockdownFall): 500 units along
// attacker -> target, hAngle the bearing of it. A block whose endPos is its startPos
// would describe a fall that cannot land where it says - the reference's only in-place
// knockdown is the suicide path, and it asks for vAngle 89 / power 207 instead
// (`MonsterNPC.cpp:1686-1696`).
func BuildAttackSingleKnockdown(attacker uint32, attackPos [3]int32, attackSpd uint16,
	target uint32, targetPos [3]int32, damage, targetHPBefore float64,
	attackType int32) []byte {

	out := BuildAttackSingle(attacker, attackPos, attackSpd, target, targetPos,
		damage, targetHPBefore, attackType)
	out = append(out, make([]byte, KnockDownInfoSize)...)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	endPos, hAngle := knockdownFall(attackPos, targetPos)
	putWorldPos(out[asKDStartPos:], targetPos)
	putWorldPos(out[asKDEndPos:], endPos)
	binary.LittleEndian.PutUint16(out[asKDDelay:], kdDelayMs)
	binary.LittleEndian.PutUint16(out[asKDPower:], kdPower)
	binary.LittleEndian.PutUint32(out[asKDVAngle:], math.Float32bits(kdVAngleDeg))
	binary.LittleEndian.PutUint32(out[asKDHAngle:], math.Float32bits(hAngle))
	out[asKDType] = kdTypeNormal
	return out
}

// knockdownFall answers the KD block's landing point and hAngle, the way the
// reference computes them.
//
// `CFieldChar::SetKnockDown` (`source2\geServer\ZServerLib\FieldChar.cpp:780-812`)
// takes the bearing of the body from its attacker, runs its bounce calculator with
// the hit's power / vAngle, and slides the body by the calculator's total distance
// along that bearing. With power 100, vAngle 45 and the calculator's own constants
// (gravity 25, friction 0.5, delay 0.5, TWO bounces - `imcBounceCalculator.h:8`) the
// distance is 400 + 100 = 500.00 units, and the three surviving kill frames measure
// 499.6 / 499.6 / 500.7 with an hAngle equal to that bearing (170.640, 127.142,
// -66.401 - `captures\kill01\20261105-214033` and `captures\punisher20261101b`).
//
// An attacker standing exactly on the target has no bearing; the reference's
// Vec2Normalize would leave a zero vector, so this answers the body's own position
// and hAngle 0 - the only honest fall for a direction that does not exist.
func knockdownFall(attackerPos, targetPos [3]int32) ([3]int32, float32) {
	dx := float64(targetPos[0] - attackerPos[0])
	dy := float64(targetPos[1] - attackerPos[1])
	dist := math.Hypot(dx, dy)
	if dist == 0 {
		return targetPos, 0
	}
	end := [3]int32{
		targetPos[0] + int32(math.Round(dx/dist*kdFallDistance)),
		targetPos[1] + int32(math.Round(dy/dist*kdFallDistance)),
		targetPos[2],
	}
	return end, float32(math.Atan2(dy, dx) * 180 / math.Pi)
}

// BuildAttackDisable packs one ZC_ATTACK_DISABLE (3568), 15 bytes: the zone
// telling the client to switch this character's attack off.
//
//	+0  WORD      3568
//	+2  OBJHANDLE the character whose attack is switched off
//	+6  OBJHANDLE the target it was aimed at
//	+10 BYTE      the reason (attackDisableInvalidTarget)
//	+11 DWORD     the reason's argument (0 for INVALID_TARGET)
//
// A handle of 0 is legal and means "no character could be named": the client's own
// handler looks the handle up in the world and finds nothing, so the packet is
// harmless. It is passed only when the request named a team slot that does not
// exist, which is a broken client - see Handler.attack.
func BuildAttackDisable(handle, target uint32, reason byte, value uint32) []byte {
	out := make([]byte, AttackDisableSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCAttackDisable)
	binary.LittleEndian.PutUint32(out[adHandle:], handle)
	binary.LittleEndian.PutUint32(out[adTarget:], target)
	out[adType] = reason
	binary.LittleEndian.PutUint32(out[adValue:], value)
	return out
}

// attackSpdOf is the attack speed this zone reports in 3564's +20: the character's
// own ASPD - the DELAY between one normal attack and the next, in MILLISECONDS -
// rounded to the WORD the packet carries.
//
// IT IS NOT CSPD. The sheet's CSPD (82, 40, 29 in this tree's own numbers) is the
// number the client's status PANEL prints, and it is that delay divided by the
// stance's hit count and by 10 (SCR_Get_CSPD, `get_status_pc.scp:1759-1760`). The
// client does arithmetic on +20 and the arithmetic is a division BY the stance's
// base ASPD (`GePC.cpp:69` `CPC::GetAtkRatio` = `atkSpd / Stance.ASPD`), so a
// panel number there reads as an attack animation at ~1/40th speed. The two
// captured characters carried 471 and 1262 - the same family as the stance rows'
// ASPD (1667 x 1.1 and down), not as any CSPD this tree computes.
//
// The reference fills the field the same way: `attack->attackSpd =
// GetAttackSPD()` (`FieldChar.cpp:425`), and `GetAttackSPD` IS `GetASPD()`
// (`FieldChar.cpp:1302-1304`).
func attackSpdOf(sheet StatusSheet) uint16 {
	if sheet.ASPD <= 0 {
		return 0
	}
	if sheet.ASPD > 65535 {
		return 65535
	}
	return uint16(math.Round(sheet.ASPD))
}

// attackInterval is how long ONE character must wait between two normal attacks:
// the sheet's ASPD, which is that delay in milliseconds and comes from the
// character's own stance row (`zone\status.go` sheetASPD, SCR_Get_ASPD
// `get_status_pc.scp:1408`). Zero means the sheet could not produce one, and the
// caller must then NOT throttle - a made-up number here would be this server
// inventing the game's timing.
func attackInterval(sheet StatusSheet) time.Duration {
	if sheet.ASPD <= 0 {
		return 0
	}
	return time.Duration(sheet.ASPD) * time.Millisecond
}

// lastAttack is when character idx of this session last landed a normal attack,
// or the zero time when it has never attacked. An index outside the slice is the
// same answer: no hit yet.
func (s *Session) lastAttack(idx int) time.Time {
	if idx < 0 || idx >= len(s.attackAt) {
		return time.Time{}
	}
	return s.attackAt[idx]
}

// noteAttack records that character idx landed a hit at `at`. The slice grows to
// this session's team, exactly as `walks` does (move.go openWalk): a team is
// spawned before any click, but a test or a resumed session may reach the attack
// path with the slice still nil, and a nil slice must mean "nothing recorded
// yet", never a panic.
func (s *Session) noteAttack(idx int, at time.Time) {
	if idx < 0 {
		return
	}
	if len(s.attackAt) <= idx {
		grown := make([]time.Time, len(s.Team))
		copy(grown, s.attackAt)
		s.attackAt = grown
		if idx >= len(s.attackAt) {
			return // a character this team does not have
		}
	}
	s.attackAt[idx] = at
}

// heldPress is ONE character's KEPT attack press: the client's own CZ_ATTACK that
// arrived inside the character's attack delay, waiting for that delay to run out.
//
// IT IS THE REFERENCE'S `m_lastRequest` SLOT, and nothing more (FieldChar.cpp:955-982):
// the press is stored, the type is NOT cleared while the gate refuses it, and the next
// tick runs the request again. What the slot holds is therefore exactly what the client
// sent: the target handle it named, the stance the character stood in, the moment the
// press arrived (so the log can say how old it is when it finally fires) and the frame
// bytes themselves - COPIED, like swingLoop.frame, because the splitter hands out slices
// that alias one decrypted chunk and this record outlives it.
type heldPress struct {
	target    uint32
	stance    uint32
	pressedAt time.Time
	frame     protocol.Frame
}

// pressOf is character idx's kept press, or nil.
func (s *Session) pressOf(idx int) *heldPress {
	if idx < 0 || idx >= len(s.presses) {
		return nil
	}
	return s.presses[idx]
}

// hasPress reports whether ANY character of this team is holding a press. It is one of
// the reasons Session.wantsTick wakes the read loop up (zone\move.go): this zone drives
// nothing itself, so without a tick the kept press would wait for the client's next
// packet - which, per attack_swing.go's header, never comes until the player clicks
// again.
func (s *Session) hasPress() bool {
	for _, p := range s.presses {
		if p != nil {
			return true
		}
	}
	return false
}

// holdPress stores a press the cadence gate refused, as the ONE press of record for
// character idx. A press already there is REPLACED - one slot per character, newest wins,
// exactly as the reference's single m_lastRequest slot behaves.
//
// IT DOES NOT RE-CHECK THE CLOCK. The gate that calls it has already measured
// `now - lastAttack < interval`; this function is the record of that verdict.
func (h *Handler) holdPress(sess *Session, idx int, target, stance uint32, f protocol.Frame,
	now time.Time) {

	if idx < 0 {
		return
	}
	if len(sess.presses) <= idx {
		grown := make([]*heldPress, len(sess.Team))
		copy(grown, sess.presses)
		sess.presses = grown
		if idx >= len(sess.presses) {
			return // a press for a character this team does not have
		}
	}
	if old := sess.presses[idx]; old != nil {
		h.Log.Printf("zone: ATTACK RETRY: char %d drops the press it was holding for target "+
			"%d (pressed %s ago) - this newer press replaces it, one slot per character",
			idx, old.target, now.Sub(old.pressedAt).Round(time.Millisecond))
	}
	data := make([]byte, len(f.Data))
	copy(data, f.Data)
	sess.presses[idx] = &heldPress{
		target: target, stance: stance, pressedAt: now,
		frame: protocol.Frame{Cmd: f.Cmd, Size: f.Size, Data: data},
	}
}

// clearPress drops character idx's kept press and says why, in one line. It answers
// whether there WAS one, so a caller that only wants to log a real clear does not have to
// ask twice. The line starts with `ATTACK RETRY` and never with `ATTACK held`, so the two
// can never be confused in a grep.
func (h *Handler) clearPress(sess *Session, idx int, why string) bool {
	if idx < 0 || idx >= len(sess.presses) || sess.presses[idx] == nil {
		return false
	}
	p := sess.presses[idx]
	sess.presses[idx] = nil
	handle := uint32(0)
	if idx < len(sess.Team) {
		handle = sess.Team[idx].Handle
	}
	h.Log.Printf("zone: ATTACK RETRY: char %d (%d) drops the press it was holding for "+
		"target %d (pressed %s ago) and nothing is sent for it - %s",
		idx, handle, p.target, time.Since(p.pressedAt).Round(time.Millisecond), why)
	return true
}

// dropPresses clears every kept press this session has, and answers how many it cleared.
// It is the disconnect path (Handler.leaveZone), beside dropCasts and dropSwings and for
// the same reason: a record left behind belongs to a session that is gone.
func (h *Handler) dropPresses(sess *Session, why string) int {
	n := 0
	for idx := range sess.presses {
		if h.clearPress(sess, idx, why) {
			n++
		}
	}
	return n
}

// pressTick fires every kept press whose character's attack delay has now run out, and
// returns the packets that made. It is the reference's ProcessRequest loop
// (PlayerChar.cpp:265) on this zone's own tick: the request is re-run every pass and the
// hit goes out on the first pass where the delay is over.
//
// THE STOP CHECKS COME FIRST AND THEY ARE NOT ABOUT THE CLOCK, exactly as swingTick's
// are: a press whose target left this zone, whose target's HP has reached zero or whose
// sheet lost its attack delay has nothing left to fire at, and firing it at a body that
// is not there would be a hit for nobody. Those clear the slot and send nothing.
//
// THE FIRE IS THE NORMAL PRESS PATH - Handler.attackAt with attackRetry - so the hit is
// the same parse, the same reach check, the same cadence check, the same 3564/3566, the
// same 3557, the same HP ledger and the same fan-out as a press at that instant. The body
// position is read INSIDE that call, at `now`, so a press kept while the body walked
// fires from where the body stands when it fires - never from where it stood.
//
// NOTHING HERE RE-STARTS THE WAIT: the slot is cleared BEFORE the call, and the call
// cannot store it again (its own step is attackRetry, and the gate is `>= interval` by
// then anyway).
func (h *Handler) pressTick(sess *Session, now time.Time) [][]byte {
	if len(sess.presses) == 0 {
		return nil
	}
	var out [][]byte
	for idx := 0; idx < len(sess.presses); idx++ {
		p := sess.presses[idx]
		if p == nil {
			continue
		}
		if idx >= len(sess.Team) {
			sess.presses[idx] = nil
			continue
		}
		if _, ok := h.monsterByHandle(p.target); !ok {
			h.clearPress(sess, idx, "target "+strconv.FormatUint(uint64(p.target), 10)+
				" is not a monster this zone is running any more, so the kept press dies "+
				"with the engagement (AS_ATTACK ends on a null target, "+
				"FieldChar_FSM.cpp:141-146)")
			continue
		}
		if hp, known := h.monsterLiveHP(p.target); known && hp <= 0 {
			h.clearPress(sess, idx, "target "+strconv.FormatUint(uint64(p.target), 10)+
				" has no HP left in this zone's own ledger, so the kept press dies with "+
				"the engagement (a dead body is not attacked, FieldChar_FSM.cpp:136-140)")
			continue
		}
		interval := attackInterval(h.sheetFor(sess.Team[idx]))
		if interval <= 0 {
			h.clearPress(sess, idx, "its sheet gives no attack delay any more (ASPD 0), "+
				"so there is no delay for the press to wait out")
			continue
		}
		if last := sess.lastAttack(idx); !last.IsZero() && now.Sub(last) < interval {
			continue // the delay is still running: the press keeps its place
		}
		aged := now.Sub(p.pressedAt)
		frame := p.frame
		sess.presses[idx] = nil
		h.Log.Printf("zone: ATTACK RETRIED char %d: the press of %d ms ago is inside the "+
			"%d ms delay, and the delay has now run out",
			idx, aged.Milliseconds(), interval.Milliseconds())
		fired, _ := h.attackAt(sess, frame, attackRetry, nil, now)
		out = append(out, fired...)
	}
	return out
}

// attack answers CZ_ATTACK (3666).
//
// THE REFUSALS COME FIRST, EACH WITH ONE LINE AND 3568 AND NO HIT, in this order:
// a frame that is not 15 bytes (nothing is sent - a broken client cannot name a
// target to switch off), a character index that is not this team, and a target
// handle that is not a monster this zone is running.
//
// THE REACH IS THE STANCE'S OWN (R90), and it is checked HERE, before anything is
// sent: `datatable_stance.xml` carries MinR and MaxR on every stance - BackGuard 0..150,
// PossessionFire 50..900, FirstAid 1..100 - and a plain attack has no skill row, so the
// stance is the only row that can say how far a swing reaches. Three answers, all read
// off that row and none invented:
//
//	inside the range   the hit goes out now, below
//	past MaxR          the character WALKS in and the hit fires when it arrives
//	                   (zone\attack_approach.go)
//	nearer than MinR   the character WALKS OUT to the min ring and the hit fires when
//	                   it arrives - the far case's mirror, same walk, same held press
//	                   (R101, zone\attack_approach.go's attackStepBackStand)
//
// AND WHEN THE ROW STATES NO MAXIMUM (MaxR 0, or no stance table loaded) NOTHING IS
// CHECKED: a made-up reach would refuse a click the client itself thinks is fine, which
// is the mistake R84 made with skills. One line says so, once per run.
//
// THE ANSWER, in the order §7.4 measured and §4's timeline shows (3564, then the
// 3557 that carries the same number):
//
//	3564 ZC_ATTACK_SINGLE  the hit, 74 B, damage at +42, HP BEFORE at +50
//	3557 ZC_ADD_HP         the same handle, HP AFTER at +6, the same double as the
//	                       delta at +14, +22 = 0 because the delta is positive
//
// UNLESS THE STANCE ROW IS AtkType="Splash": then the hit is 3566 ZC_ATTACK_SPLASH
// instead of the 3564 (the same 74-byte block, one byte of count at +74 and one
// 48-byte record per extra body after it), and the main target's 3557 is followed by
// one 3557 per body the circle reached - zone\attack_splash.go, built below.
//
// BOTH GO TO EVERY PLAYER IN THE ZONE: §4's table puts 3564 and 3557 in the "about
// the world" column, and the bytes themselves carry no recipient, so the actor gets
// the returned slice and every other session gets those SAME bytes through fanOut
// (R93). That is the same rule TestMoveIsBroadcastUnchanged pins for a walk: one
// build, many watchers.
func (h *Handler) attack(sess *Session, f protocol.Frame) ([][]byte, error) {
	out, _ := h.attackAt(sess, f, attackPress, nil, time.Now())
	return out, nil
}

// attackStep says WHICH HALF of a plain attack this call produces: the client's own
// 3666 (attackPress), or the replay at the end of the walk the press earned
// (attackAfterWalk). It exists for one reason: a replay that finds the target out of
// reach again must say so and send nothing, never start a second walk for ever. The
// same split as the cast's castStep (zone\skill_cast.go).
type attackStep int

const (
	attackPress     attackStep = iota // the client pressed the mouse
	attackAfterWalk                   // the approach walk ended; replay and hit
	// attackSwing is one swing of the repeat R94 keeps running once a press has landed
	// (zone\attack_swing.go). It is the client's own frame replayed on the character's
	// own cadence, so it must NOT start an approach walk of its own - a repeat that
	// walked would be a second movement system, and the reference's chase
	// (FieldChar_FSM.cpp:155-170) is deliberately not modelled here.
	attackSwing
	// attackRetry is a press the cadence gate KEPT and the tick is firing now, once the
	// character's own delay has run out (zone\attack.go holdPress/pressTick). It is the
	// same client 3666, replayed through this same function, so the hit it builds cannot
	// differ from the one a press at that instant would have built - the only thing the
	// step changes is what the log says about WHY the hit is being built now.
	attackRetry
)

// attackAt builds the answer to one CZ_ATTACK and SAYS WHY IT BUILT NOTHING when it
// did not.
//
// TWO RETURN VALUES, and the second one is not decoration (R103). A press has exactly
// one outcome, and the log has to name it: the first answer is the packets, the second
// is the REASON no hit is among them - the empty string when a hit WAS built, and a
// short sentence naming the gate that stopped it otherwise (a frame this zone cannot
// parse, a character or a target it does not have, a cast in progress, the reach, the
// attack-speed gate, a missing HP ledger, an empty build). Before this the caller could
// only ask "is there an attack packet?", which cannot tell "the target is out of reach
// so the walk carries it" from "the cadence gate held it" - and the fire-on-arrival line
// ended up saying "no attack packet was built" for 72 presses in one session.
//
// `from` is the body point the caller ALREADY MEASURED, or nil to read the character's
// own (attackBodyPoint). The fire-on-arrival path passes its own measurement (R103):
// one press, one measurement. Reading the position there a second time re-asked the
// question from a different model - for a character that is not the slot the session
// drives, a stale one - and the two answers disagreed: the walk was re-planned AND the
// press was reported consumed, for one press.
//
// `now` IS THE ONE CLOCK OF THE PACKET (R111): the wall time of the moment this call
// BUILDS it. Every caller hands that instant in - the press reads it on entry, the
// repeat's tick reads it once for the whole pass (so the reach it measured and the point
// it sends are the same instant), the arrival check hands in the time it measured with.
// Nothing here reads a second time, because two reads of this clock is exactly how one
// leg came to be reported at two different elapsed values.
//
// IT IS A bodyPoint AND NOT A BARE POINT (R104), because the SWING POS line has to name
// the segment the point came from: a point and a segment taken from two models is a
// line that cannot be read.
func (h *Handler) attackAt(sess *Session, f protocol.Frame, step attackStep,
	from *bodyPoint, now time.Time) ([][]byte, string) {
	req, ok := ParseAttack(f.Data)
	if !ok {
		h.Log.Printf("zone: ATTACK refused: %d-byte frame, CZ_ATTACK 3666 is a fixed "+
			"%d bytes - nothing sent", len(f.Data), czAttackSize)
		return nil, "the frame was not a CZ_ATTACK this zone could parse"
	}

	// THE CLIENT'S OWN REQUEST, before ANY decision below. One line per press, the
	// moment the frame parses, so the stamp on it and the stamp on whichever answer
	// follows (`ATTACK ...`, `ATTACK held ...`, `ATTACK APPROACH ...`) put the
	// press-to-answer gap on the clock instead of leaving it to guesswork.
	//
	// ONLY attackPress: attackAfterWalk and attackSwing are this side REPLAYING the
	// frame the client sent once, not a new request, and a line for them would
	// count one press as two or three.
	//
	// THE HANDLE IS THE TEAM'S, read here and guarded: the index may be out of range,
	// and that refusal has its own line just below. It logs 0 in that case rather
	// than inventing an attacker.
	//
	// NO POSITION: CZ_ATTACK 3666 is a FIXED 15 bytes (+0 cmd, +2 seq, +6 checksum,
	// +10 charIndex, +11 target) and carries none. There is nothing to print here.
	//
	// WHY THE WORDS ARE `REQ ATTACK` AND NOT `ATTACK REQ`: a decision line starts
	// with `zone: ATTACK`, and two existing tests (action_broadcast_test.go) read the
	// FIRST line that starts that way to check the fan-out count. A `zone: ATTACK REQ`
	// line would be picked up by them first and fail a test that has nothing to do
	// with this one. Putting REQ in front of ATTACK keeps those readers finding the
	// decision line they were written for, and keeps this line greppable by both words.
	if step == attackPress {
		handle := uint32(0)
		if req.CharIndex >= 0 && req.CharIndex < len(sess.Team) {
			handle = sess.Team[req.CharIndex].Handle
		}
		h.Log.Printf("zone: REQ ATTACK char %d (%d) target %d",
			req.CharIndex, handle, req.Target)
	}

	// The checksum verdict rides on EVERY line below, so "was it validated?" can be
	// answered from one line of the log without a second one per attack.
	stated := req.Checksum
	recomputed := czChecksum(f.Data)
	checksum := "checksum " + strconv.FormatUint(uint64(stated), 10) + " ok"
	if stated != recomputed {
		checksum = "checksum MISMATCH (stated " + strconv.FormatUint(uint64(stated), 10) +
			", recomputed " + strconv.FormatUint(uint64(recomputed), 10) +
			") ACCEPTED - no sample shows the reference refusing one"
	}

	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: ATTACK refused: commander %d character index %d is not in "+
			"this team (%d member(s) spawned), %s - 3568 sent with handle 0, no hit",
			sess.CommanderID, req.CharIndex, len(sess.Team), checksum)
		return [][]byte{BuildAttackDisable(0, req.Target, attackDisableInvalidTarget, 0)},
			"character index " + strconv.Itoa(req.CharIndex) + " is not in this team, so a " +
				"3568 was sent and no hit"
	}
	c := &sess.Team[req.CharIndex]

	// A PRESS WHILE A CAST IS IN PROGRESS ANSWERS NOTHING (R95 item 3): no 3564, no 3557,
	// no 3568 and no repeat. The character is not in the plain-attack state while it
	// casts - the reference takes the plain-attack path only from AS_ATTACK
	// (PlayerChar.cpp:1987-2002) - so a 3666 that arrives in the middle of a wind-up has
	// no hit behind it.
	//
	// THIS BEHAVIOUR IS UNMEASURED, and it is said here rather than discovered later: no
	// capture shows the client sending 3666 in the middle of a cast. The client's own AI
	// sends ONE 3666 per target lock, on ENTERING the attack state (attack_swing.go's
	// header lists the chain), so the press this refuses is one the client should never
	// send in the first place. REFUSING IS THE CONSERVATIVE CHOICE all the same: a second
	// damage stream running under a cast is a real bug on the player's screen, while
	// ignoring a press nobody's client can send costs nothing at all.
	//
	// THE CHECK IS UNCONDITIONAL, so it covers attackPress, attackAfterWalk and
	// attackSwing alike: an approach walk or a repeat swing that arrives during a cast is
	// the same second damage stream and gets the same silence.
	//
	// ONE LINE PER PRESS, never two: endSwing already writes the ending line when there
	// was a repeat to stop, and the Printf below writes only when there was none.
	if cc := sess.castOf(req.CharIndex); cc != nil {
		if !h.endSwing(sess, req.CharIndex, "the player pressed the plain attack while skill "+
			strconv.FormatUint(uint64(cc.skill), 10)+" was casting - a cast is in progress, "+
			"so nothing is sent and no repeat is armed") {
			h.Log.Printf("zone: ATTACK refused: commander %d char %d (%d) pressed the plain attack "+
				"while skill %d is casting - a cast is in progress, so nothing is sent and no "+
				"repeat is armed", sess.CommanderID, req.CharIndex, c.Handle, cc.skill)
		}
		return nil, "a cast of skill " + strconv.FormatUint(uint64(cc.skill), 10) +
			" is in progress, so the plain attack is answered with silence"
	}

	// A PRESS FOR A DIFFERENT TARGET ENDS THE REPEAT ON THE OLD ONE (R95 item 1), BEFORE
	// the monster lookup, the reach check and the cadence - the order matters, because a
	// repeat left running through any of those would keep swinging at the body the player
	// has already clicked away from while the character walks to the new one.
	//
	// THE OLD BEAT IS THE ONE BUG THIS LANE MUST NOT HAVE: "a hit against the wrong
	// target". So endSwing is still the ONE place a repeat ends - the line therefore says
	// the OLD target with its own `after N hit(s)` - and the reason below adds the NEW
	// handle the player pressed for. Nothing is ended when the target is unchanged: that
	// re-press is the client's own tactics state re-entering, and the hit it earns re-arms
	// this same slot through armSwing further down.
	if step == attackPress {
		// A NEWER PRESS REPLACES A KEPT ONE (newest wins, one slot per character). It is
		// done here, before any decision, so every outcome below - a hit, a walk, a
		// refusal - leaves exactly ONE press of record: this one.
		h.clearPress(sess, req.CharIndex, "a newer press for target "+
			strconv.FormatUint(uint64(req.Target), 10)+" replaced it")
		if cur := sess.swingOf(req.CharIndex); cur != nil && cur.target != req.Target {
			h.endSwing(sess, req.CharIndex, "the player pressed for target "+
				strconv.FormatUint(uint64(req.Target), 10)+" now, not "+
				strconv.FormatUint(uint64(cur.target), 10)+" - a repeat still swinging at "+
				"the old target would keep hitting it while the character walks to the new one")
		}
		// AND A PRESS FOR ANOTHER TARGET ENDS A HELD PRESS TOO (R100). A press for the
		// target already being walked to is absorbed below and must NOT be dropped here;
		// one for a different target is a new decision, and the old press must stop
		// wanting a hit - including on the presses that never reach approachAttack (a new
		// target already in reach, or one the stance refuses).
		h.dropHeldAttackForAnotherTarget(sess, req.CharIndex, req.Target)
	}

	mon, ok := h.monsterByHandle(req.Target)
	if !ok {
		h.Log.Printf("zone: ATTACK refused: commander %d char %d (%d) target handle %d "+
			"is not a monster this zone is running, %s - 3568 sent, no hit",
			sess.CommanderID, req.CharIndex, c.Handle, req.Target, checksum)
		return [][]byte{BuildAttackDisable(c.Handle, req.Target,
			attackDisableInvalidTarget, 0)}, "target " +
			strconv.FormatUint(uint64(req.Target), 10) + " is not a monster this zone is " +
			"running, so a 3568 was sent and no hit"
	}

	// ACCEPTED SO FAR: the frame, the character and the target are real. The reach is
	// checked BEFORE the cadence and before anything is sent (R90) - see the header.
	sheet := h.sheetFor(*c)

	// A SWING OF THE REPEAT MUST BE THE SWING THE REPEAT ARMED (R94): if the record was
	// ended or re-pointed between the tick's own check and this call, there is nothing
	// to answer. It cannot happen from one goroutine, and it is still checked, because a
	// hit against the wrong target is the one mistake this lane must not make.
	if step == attackSwing {
		if cur := sess.swingOf(req.CharIndex); cur == nil || cur.target != req.Target {
			return nil, "the repeat's own record was gone by the time its swing was due, " +
				"so there is no rhythm to answer"
		}
	}

	// THE POSITION IS READ ONCE, for the reach check and for the 3564's +8, so the
	// packet can never describe a body the check did not measure. It is read at the
	// speed the CLIENT was told to walk at (attackBodyPoint -> drawnPos, R91): the 3564
	// sets the attacker's own actor to +8 on every swing (GeActor.cpp:309), so a
	// position taken at the MEASURED speed would put the body ahead of the one already
	// drawn and the character would snap on every swing.
	//
	// A CALLER THAT ALREADY MEASURED HANDS ITS POINT IN (R103): the fire-on-arrival
	// path measured the body before calling here, so `from` carries it and the model is
	// NOT asked again. Asking twice is what put a walk and a "press CONSUMED" on the log
	// for one press: the two models answered differently (754 of clear ground against
	// 1090), the second answer re-planned the walk, and the caller then reported the
	// press as spent.
	body := sess.attackBodyPoint(req.CharIndex, now, h.packetSpeed(c))
	if from != nil {
		body = *from
	}
	pos := body.pos
	targetPos := [3]int32{mon.X, mon.Y, mon.Cell}

	minR, maxR := h.stanceRange(c.Stance)

	// THE TWO BODIES THE REACH IS MEASURED BETWEEN (R91 round 5). The character's own is
	// the reference's 50 (geShapeTable ST_SMALL, ShapeTable.cpp:47-51, which is also the
	// shape both of its attack stand points fall back to); the target's is its row's own
	// Size. Read ONCE here, so the press, the walk it asks for and the replay at the end
	// of it all measure the same two bodies.
	myR := PCBodyRadius
	targetR := h.monsterBodyRadius(mon)
	if maxR <= 0 {
		// NO FAR BOUND IN THE ROW, SO NO REACH CHECK: the stance states no maximum (or
		// this zone has no stance table). A number made up here is what refused the
		// owner's Fighter at 443 units on R84's skills; it is not repeated.
		h.logNoReachOnce(req.CharIndex, c.Handle, c.Stance)
	} else if dist, inRange := attackRangeAllowed(pos, targetPos, minR, maxR, myR,
		targetR); !inRange {
		switch {
		case dist > maxR:
			if step == attackAfterWalk {
				// THE WALK ENDED AND THE TARGET IS STILL OUT OF REACH: a monster that
				// stepped away, or a stance that changed under the walk. THE REFERENCE
				// WALKS AGAIN (R98) - its attack state re-decides on a 300 ms clock and
				// issues ANOTHER move to GetAttackMaxPos while the target is too far
				// (FieldChar_FSM.cpp:28-80, case 1), and it never stands still out of
				// range. So this zone walks again too, through the SAME approachAttack
				// the press uses (one ring, one edge reach, one reuse rule) and with a
				// cap on the repeats, so a monster that keeps stepping away is a stop
				// and not a loop. replanAfterWalk owns the pace, the count and the cap.
				return h.replanAfterWalk(sess, pendingAttack{
						frame: f, target: req.Target, pos: targetPos, dist: dist,
						minR: minR, maxR: maxR, myR: myR, targetR: targetR,
						idx: req.CharIndex, since: now, stance: c.Stance,
					}, mon), "the target is out of the stance's reach, so the walk re-decides " +
						"instead of building a hit"
			}
			// A SWING OF THE REPEAT NEVER GETS HERE OUT OF POSITION (R106). The repeat's
			// tick measures this same edge distance BEFORE it fires and, when it is out of
			// the band, MOVES the body instead of swinging (zone\attack_swing.go: too far ->
			// walk in, too near -> step back). So this branch is only reachable when the
			// body drifted between the tick's measurement and this call, inside one pass of
			// one goroutine - and the answer to a drift is the next tick's measurement, not
			// an ending. NOTHING IS LOGGED AND NOTHING IS ENDED: silence here means the
			// engagement is still alive, and the old "cannot reach target ... the repeat
			// ends" sentence is deleted because it WAS the owner's "attack stops".
			if step == attackSwing {
				return nil, ""
			}
			// OUT OF REACH: THE SERVER WALKS THE CHARACTER IN, NOW (R91 round 6). R91
			// round 4 left this spot silent and waited for the CLIENT to walk the leader
			// to the clicked enemy. That walk never comes: the client's own AI nominates
			// the target and calls SCR_AI_LIB_ATTACKTARGET, which only attacks or sleeps
			// 1000 - it never moves the body (D:\ge_server\ge\ai\attacker.scp:135-170
			// SCR_ATTACKER_TS_BATTLE_NOMINATE, lib.scp SCR_AI_LIB_ATTACKTARGET). So the
			// reference walks from the press itself - too far -> GetAttackMaxPos -> move
			// (FieldChar_FSM.cpp:28-80) - and so does this zone, in the same walk, with
			// the same reason and the same clock a skill approach uses.
			//
			// approachAttack decides the rest: a re-decision inside 300 ms sends nothing
			// (m_nextAttackChaseTime, :38-42 and :57), and a press whose stand point is
			// the one already in flight sends nothing either (IsEqualPos(moveTo,
			// m_moveTo), :49 and :74).
			return h.approachAttack(sess, req.CharIndex, pendingAttack{
					frame: f, target: req.Target, pos: targetPos, dist: dist,
					minR: minR, maxR: maxR, myR: myR, targetR: targetR,
					idx: req.CharIndex, since: now, stance: c.Stance,
				}, "the press is out of reach, and the client's own AI never walks to a "+
					"clicked enemy (attacker.scp:135-170)"), "the target is out of the " +
					"stance's reach, so the press was handed to the walk"
		default:
			// TOO NEAR FOR MinR: STEP BACK OUT TO THE MIN RING, KEEP THE PRESS (R101).
			//
			// THE MEASUREMENT (owner's session, logs\go_zone1331.log, 17:37:46 and
			// 17:37:49): the same press was refused twice for being UNDER the minimum -
			// "is 29.321305282617686 away" and "is -6.565530985615382 away" against
			// BlandirCruz's MinR 50 - and a NEGATIVE edge distance means the two bodies
			// overlap. 6 of 27 presses in that session ended this way: the character stood
			// on top of the monster and did nothing, which is the owner's "standing in
			// front of the monster and it will not attack".
			//
			// THE REFERENCE DOES NOT REFUSE IT. IsAttackArea answers -1 (too near) and
			// that case takes GetAttackMinPos and MOVES the character out to the min ring
			// (FieldChar_FSM.cpp:63-79: `case -1` -> GetAttackMinPos -> SetMoveState),
			// the exact mirror of the too-far case above it, on the same 300 ms clock and
			// with the same "only when the destination CHANGED" rule (:68-79). GetAttackMinPos
			// is minR + both bodies from the target's centre (PlayerChar.cpp:2102-2115,
			// GetOuterPosByTarget), which is attackStepBackStand below.
			//
			// SO THIS IS THE FAR CASE'S MIRROR, NOT A SECOND PATH: the same approachAttack,
			// the same held press, the same fire-on-arrival check - only the ring is the
			// NEAR one, and the walk goes away from the target instead of towards it.
			//
			// THE RING AND THE WALK COME FROM attackStepBackPlan, because the FIRE now
			// answers this same case and one rule must not have two implementations (R102).
			if step == attackAfterWalk {
				// THE WALK ENDED AND THE TARGET IS STILL UNDER THE MINIMUM - it walked
				// into the character while the body was stepping out. THE REFERENCE
				// RE-DECIDES on its own chase clock instead of standing there, so this
				// zone does the same, through the far case's own replanAfterWalk: the same
				// 300 ms pace, the same count and the same cap, one rule for both
				// directions.
				return h.replanAfterWalk(sess, pendingAttack{
						frame: f, target: req.Target, pos: targetPos, dist: dist,
						minR: minR, maxR: maxR, myR: myR, targetR: targetR,
						idx: req.CharIndex, since: now, stance: c.Stance,
						stand: attackStepBackStand(minR, myR, targetR), stepBack: true,
					}, mon), "the target is inside the stance's minimum reach, so the walk " +
						"re-decides instead of building a hit"
			}
			if step == attackSwing {
				// THE MIRROR OF THE FAR CASE (R106): the tick steps the body back out to the
				// min ring when it measures this, and this call only sees a drift inside one
				// pass. It ends nothing and says nothing - the next tick measures again, and
				// the old "inside the stance's minimum reach ... nothing sent" refusal (10
				// of them in the owner's log) is gone with it.
				return nil, ""
			}
			// THE STEP BACK ITSELF: one move, out to the min ring, press held. The line
			// that names the edge distance, the min ring and the destination is written
			// by approachAttack when the walk is sent, so there is ONE line per step-back.
			return h.attackStepBackPlan(sess, pendingAttack{
					frame: f, target: req.Target, pos: targetPos, dist: dist,
					minR: minR, maxR: maxR, myR: myR, targetR: targetR,
					idx: req.CharIndex, since: now, stance: c.Stance,
				}, "the target is INSIDE the stance's minimum reach, so the character steps "+
					"BACK to the min ring (the reference's own answer to IsAttackArea -1: "+
					"GetAttackMinPos, FieldChar_FSM.cpp:63-79) and the hit fires when the "+
					"body is inside MinR..MaxR"), "the target is inside the stance's minimum " +
					"reach, so the press was handed to the step-back walk"
		}
	}

	// THE CADENCE, AND WHERE ITS NUMBER COMES FROM (Part B.1 of the 20:48 report).
	// On the reference server a character cannot swing twice inside its own attack
	// delay: the FSM sets `m_nextAttackTime = timeGetTime() + GetAttackSPD()`
	// (`FieldChar_FSM.cpp:135`, again at :187 and :200) and its UPDATE re-attacks
	// only after that moment. The number is the character's own ASPD
	// (`FieldChar.cpp:1302-1304`), i.e. the stance row's ASPD through SCR_Get_ASPD
	// (`get_status_pc.scp:1408-1555`, zone\status.go sheetASPD) - the same value
	// this file writes into the 3564's +20. NOTHING IS INVENTED HERE: the interval
	// is read off the sheet, and if the sheet cannot give one the attack is NOT
	// throttled (see attackInterval) - an untimed hit is the honest answer, a made
	// up delay is not.
	if interval := attackInterval(sheet); interval > 0 {
		if last := sess.lastAttack(req.CharIndex); !last.IsZero() {
			if since := now.Sub(last); since < interval {
				h.Log.Printf("zone: ATTACK held: commander %d char %d (%d) asked to swing "+
					"%s after the last hit, and this character's own attack delay is %s "+
					"(ASPD %g ms, SCR_Get_ASPD get_status_pc.scp:1408 - stance %d ASPD %g "+
					"x 1.1, then stance level, AGI and the worn items' ASPD) - no %d and "+
					"no %d sent, %s (kept, not dropped - it fires when the delay ends)",
					sess.CommanderID, req.CharIndex, c.Handle, since.Round(time.Millisecond),
					interval, sheet.ASPD, c.Stance, h.stanceASPD(c.Stance), CmdZCAttackSingle,
					CmdZCAddHP, checksum)
				// THE PRESS IS KEPT, NOT DROPPED (R100). The reference holds it in its
				// request slot and retries on every tick (FieldChar.cpp:955-982 returns
				// BEFORE clearing `m_lastRequest->type`; ProcessRequest, PlayerChar.cpp:265,
				// runs it again each pass), so a slow stance still answers the player's
				// click instead of swallowing it. A retry never re-stores itself: its own
				// clock is the interval's, and storing again would restart the wait.
				if step != attackRetry {
					h.holdPress(sess, req.CharIndex, req.Target, c.Stance, f, now)
				}
				return nil, "the attack-speed gate held it (and kept it - it fires when " +
					"the delay ends): " +
					strconv.FormatFloat(float64(since)/float64(time.Millisecond), 'f', 1, 64) +
					"ms after the last hit, and this character's own delay is " +
					strconv.FormatFloat(float64(interval)/float64(time.Millisecond), 'f', 0, 64) +
					"ms (ASPD " + strconv.FormatFloat(sheet.ASPD, 'f', 0, 64) + ")"
			}
		}
	} else {
		h.logNoCadenceOnce(req.CharIndex, c.Handle)
	}

	// THE TARGET'S DEFENCE (card t_528e0d15). A player's plain attack used to land at
	// `ATK x percent / 100` with NO target term at all, while a monster's swing already
	// applied the same rule from the other side (`monsterSwingDamage`). The game's own
	// script (ge_snapshot_script_20261022\pc\common_pc.scp, the `arg.Attacker == 'player'`
	// branch beside :546) reduces by `(DEF/2) - (AR/2)`, with the TARGET's DEF from the
	// DEF script already armed for skills (`SCR_Get_MON_DEF`) and the ATTACKER's AR from
	// its own sheet (`SCR_Get_AR`).
	//
	// THE MISS ROLL IS NOT HERE: the same block refuses a hit when
	// `(100 - (DR - AR) * 2) < math.random(1,100)`, and a MONSTER's DR is not in this
	// build - the table carries `DR_BM` only, and no script read for a monster's DR is
	// armed. A roll with no DR would read as "never misses", which is exactly the bug this
	// card is against, so it waits for that prerequisite (carded) instead of pretending a
	// zero.
	damage := castDamage(sheet.ATK, plainAttackPercent, 0)
	defenceLine := "no target DEF (the DEF script is not armed)"
	if def, ok, derr := h.monsterDefence(mon); ok {
		term := plainSwingDefence(def, sheet.AR)
		damage = applyDefence(damage, term)
		defenceLine = fmt.Sprintf("target DEF %g - AR %g -> defence %g%%", def, sheet.AR, term)
	} else if derr != nil {
		defenceLine = fmt.Sprintf("no target DEF (%v)", derr)
	}
	spd := attackSpdOf(sheet)

	// THE LEDGER, AND THE ONE PLACE IT IS WRITTEN. before/after come from the HP
	// this zone keeps per monster (zone\create.go); ok is false when no HP is
	// known for it at all, and then NOTHING is sent that carries one - a hit whose
	// "before" is invented is a health plate that tells the player a story neither
	// side can maintain.
	before, after, hpKnown := h.damageMonsterBy(mon, damage, c.Handle, int(c.Level))
	if !hpKnown {
		h.Log.Printf("zone: ATTACK applied WITHOUT HP tracking: commander %d char %d "+
			"(%d) target %d (%q) is a monster this zone runs but keeps no HP for "+
			"(nothing gave it one when it was created), %s - no %d and no %d sent, "+
			"because both carry an HP this zone cannot name, and the client OVERWRITES "+
			"the plate with +50 and plays its death animation when +50 <= the damage",
			sess.CommanderID, req.CharIndex, c.Handle, req.Target, h.MonsterName(mon),
			checksum, CmdZCAttackSingle, CmdZCAddHP)
		return nil, "this zone keeps no HP for target " +
			strconv.FormatUint(uint64(req.Target), 10) + ", so neither the " +
			strconv.Itoa(CmdZCAttackSingle) + " nor the " + strconv.Itoa(CmdZCAddHP) +
			" could carry an honest number"
	}

	// A PRESS THAT IS ANSWERED DISARMS THE WALK IT MEANS (R91 round 6), and this is the
	// ONE place it may: every check that can refuse the press - the frame, the character,
	// the monster, the reach, the cadence and the HP ledger above - has returned by now.
	// A press that is refused or held answers nothing, so it must never disarm the walk in
	// flight: that walk would arrive and swing for a press nobody got, which is the
	// walk-with-no-hit this call removes. WHICH walk a press survives is not decided here:
	// approachAttack keeps the one whose stand point is the same, and drops any other.
	h.supersedePendingAttack(sess, req.CharIndex)

	// A PRESS THAT IS ANSWERED FROM MID-WALK STOPS THE WALK THERE (R129). The press answers
	// from the point the body stands on NOW (body.pos, below) - and the walk it was making is
	// closed here so the rest of the engagement cannot be built from that walk's destination.
	// Measured: logs\zone_go_live.log, 16:11:07 and 16:15:03/16:15:06 - swing 1 mid-walk,
	// swing 2 at the leg's end, and the body the client had stopped at the press snapped there.
	// attackSwing is EXCLUDED ON PURPOSE: a swing of the repeat that fires mid-walk is the
	// random walk the reference draws its swings along (R104/R110), and pinning there would
	// freeze a body that is genuinely still walking.
	if step == attackPress || step == attackRetry {
		if dropped, pinned := sess.pinStandingPointOnPress(req.CharIndex, body); pinned {
			h.Log.Printf("zone: ATTACK: char %d (%d) pressed MID-WALK - the body is PINNED at "+
				"(%d, %d, cell %d), where it stands, and the walk to (%d, %d, cell %d) is DROPPED: "+
				"every later swing of this engagement is built from the pinned point, never from a "+
				"leg the press ended (R129)",
				req.CharIndex, c.Handle, body.pos[0], body.pos[1], body.pos[2],
				dropped[0], dropped[1], dropped[2])
		}
	}

	// THE FOUR INPUTS OF THE POSITION IN THE PACKET, ONE LINE PER SWING (R91 round
	// 3): the character, the point sent, the segment that point was read from
	// (from -> dest) and how long that segment had been running. The client
	// hard-sets its own actor to this point on every swing (GeActor.cpp:309-311),
	// so a number that is not where the body stands IS the lurch on screen - and
	// these four are everything that can make it one.
	//
	// THE SEGMENT IS THE ONE THE POINT CAME FROM (R104), not a second read of the
	// session: the point above is `body.pos`, so `body.from -> body.dest` is the
	// segment it was walked along by construction. The point therefore always lies on
	// the segment this line names, to within one int32 unit of rounding.
	//
	// AND IF THAT READING RAN BACKWARDS, THE LINE SAYS SO (R111): one leg has one clock,
	// so a swing that reports a point BEHIND the previous swing on the same leg is a
	// caller handing in a time that does not belong to the leg. The point sent is the
	// previous one (the guard held it) and this line is the alarm, with both points and
	// both times - the regression then shows up here instead of on the owner's screen.
	if body.rewind {
		h.Log.Printf("zone: SWING POS REWIND char %d (%d) leg (%d, %d) -> (%d, %d) "+
			"started %s: the previous swing reported (%d, %d) at %s and this reading at "+
			"%s put the body at (%d, %d), BEHIND it - one leg has ONE clock "+
			"(zone\\attack_position.go, R111), so the point sent is the earlier one and "+
			"the caller that handed in this time is the regression",
			req.CharIndex, c.Handle, body.from[0], body.from[1], body.dest[0],
			body.dest[1], body.at.Format("15:04:05.000000"), body.prev[0], body.prev[1],
			body.prevAt.Format("15:04:05.000000"), now.Format("15:04:05.000000"),
			body.next[0], body.next[1])
	}
	h.Log.Printf("zone: SWING POS char %d (%d) pos (%d, %d, cell %d) from segment "+
		"(%d, %d) -> (%d, %d) elapsed %dms", req.CharIndex, c.Handle,
		pos[0], pos[1], pos[2], body.from[0], body.from[1], body.dest[0],
		body.dest[1], body.elapsed.Milliseconds())

	// A KILLING BLOW IS A KNOCKDOWN, AND IT CARRIES ITS OWN 37-BYTE BLOCK (see
	// knockdownAttackType and BuildAttackSingleKnockdown above). `before > 0 &&
	// after == 0` is exactly the condition takeHP arms the ZC_DEAD on (zone/create.go),
	// so the packet and the death cannot disagree about which hit was the kill; every
	// other hit stays the measured 74-byte type 2.
	killing := before > 0 && after == 0
	var hit []byte
	hitType := int32(plainAttackType)
	if killing {
		hit = BuildAttackSingleKnockdown(c.Handle, pos, spd, req.Target, targetPos,
			damage, before, knockdownAttackType)
		hitType = knockdownAttackType
	} else {
		hit = BuildAttackSingle(c.Handle, pos, spd, req.Target, targetPos, damage,
			before, plainAttackType)
	}
	// THE HIT HAPPENED, so the cadence clock starts HERE and not on the click: a
	// refused or unrecorded swing (the branch above) leaves the interval untouched,
	// which is what the reference does too - `m_nextAttackTime` is set in AS_ATTACK's
	// ON_ENTER, after ProcessAttack (FieldChar_FSM.cpp:133-136).
	sess.noteAttack(req.CharIndex, now)
	// THE SAME DOUBLE, twice: the 3564's +42 and this delta (§2.1). The delta is
	// positive, so BuildAddHP writes +22 = 0 by itself.
	addHP := BuildAddHP(req.Target, after, damage)
	out := [][]byte{hit, addHP}
	hitCmd := CmdZCAttackSingle

	// THE SPLASH SHAPE (the splash lane, docker-port\history\splash-attack.md): a stance
	// whose OWN ROW says AtkType="Splash" answers with
	// 3566 instead of 3564 - the same main hit, plus one 48-byte record per extra body
	// inside the row's own SplRange, plus one 3557 per body so every plate moves with
	// its number (docker-port\history\splash-attack.md, zone\attack_splash.go).
	//
	// THE DECISION IS THE ROW'S ALONE. Not a character id, not a handle, not a name: the
	// table says 174 stances are Splash, and a character standing in one of them swings a
	// splash because its stance row says so. A row without the attribute reads 0 / "",
	// which is not "Splash", so nothing about the Single path below changes.
	//
	// WHAT THE CAPTURE MEASURED AND WHAT IT DID NOT: the circle is centred on the MAIN
	// TARGET (the extra victim in the 171-byte sample is 109.5 from it and 303.9 from the
	// caster); SplType "Circle" is the only shape on any row measured, so only the
	// circle is built; and NO capture holds a splash with two or more extras, so the
	// 48-byte stride is proven for counts 0 and 1 only. The count is capped where the
	// packet's own bytes force it - the COUNT byte's 255 (maxSplashVictims, in the
	// builder) and, lower still, this zone's area scan at maxAreaTargets 224 - and by
	// nothing this zone made up.
	//
	// THE ONE THING NO CAPTURE MEASURES HERE: whether the reference applies SplLimit (4
	// on Disablering) or SplDam (1) to a splash. Nine of the sixteen samples carry one
	// extra, which is under that limit, so neither column is applied - the circle is
	// capped only where the packet's own length forces it, and the damage is the plain
	// attack's, the same number the main hit got.
	if stance := h.Stances.Combat(c.Stance); stance.AtkType == "Splash" {
		if stance.SplRange <= 0 {
			h.logSplashNoRangeOnce(c.Stance)
		}
		victims, extraHP, capped := h.splashVictims(mon, stance.SplRange, damage,
			splashMainAttackType, c.Handle, int(c.Level))
		if capped {
			h.logSplashCappedOnce(c.Stance, stance.SplRange)
		}
		// THE SPLASH'S TYPE IS splashMainAttackType (0), NOT plainAttackType (2): the
		// client's AttackSplash would consume a 37-byte KD block before its victim loop
		// for type 2, and this packet has none. The plain 3564 above keeps 2. See
		// zone\attack_splash.go's splashMainAttackType.
		//
		// THE ONE EXCEPTION IS THE KILLING BLOW, and it is the same condition the plain
		// form above uses. When THIS hit took the main target's last HP (`before > 0 &&
		// after == 0`, the condition takeHP arms the ZC_DEAD on), the main block takes the
		// 111-byte fatal form - type 6 and the 37-byte KNOCKDOWN_INFO, the single-target
		// killing blow's own bytes - and the count and the records move 37 bytes along
		// with it. Without this a splash's CLICKED target blinked out while the extras
		// beside it fell, which is the hole f56bb353 closed per victim and not here.
		if killing {
			hit = BuildAttackSplashMainKnockdown(c.Handle, pos, spd, req.Target,
				targetPos, damage, before, victims)
			hitType = knockdownAttackType
		} else {
			hit = BuildAttackSplash(c.Handle, pos, spd, req.Target, targetPos, damage,
				before, splashMainAttackType, victims)
			hitType = splashMainAttackType
		}
		out = append([][]byte{hit, addHP}, extraHP...)
		hitCmd = CmdZCAttackSplash
		h.Log.Printf("zone: ATTACK SPLASH: commander %d char %d (%d) stance %d (%q) is "+
			"AtkType %q SplType %q, so the hit on target %d (%q) reaches every body "+
			"strictly inside %g units of IT (not of the caster) -> %d extra victim(s) %v, "+
			"%g damage each, HP taken off the same ledger | cmd %d (%d B)",
			sess.CommanderID, req.CharIndex, c.Handle, c.Stance, h.stanceName(c.Stance),
			stance.AtkType, stance.SplType, req.Target, h.MonsterName(mon), stance.SplRange,
			len(victims), splashHandles(victims), damage, CmdZCAttackSplash, len(hit))
	}

	// THE ONE LINE THAT PROVES THE CARD: who swung (with the ATK it was rolled
	// from), what was hit, where both stood, the damage, and the HP before and
	// after. `%g` and never `%d` for the damage and the HP: they are float64 on the
	// wire and an integer format would hide a fraction that is really there.
	//
	// THE FAN-OUT IS ONE CALL WITH BOTH PACKETS (R93), so the observer cannot get a
	// hit without its number: the hit and the 3557 are the same two slices the
	// caster is handed, and sendOthers skips the caster's own session. The count on
	// the line is the number of OTHER sessions that took all of them.
	others := h.fanOut(sess, c.Handle, out...)

	h.Log.Printf("zone: ATTACK commander %d char %d (%d) %q ATK %g -> damage %g "+
		"(plain attack: ATK x %d / 100, no skill factor, %s) | target %d "+
		"(%q Lv %d) at (%d %d cell %d) from (%d %d cell %d) | HP %g -> %g | cmd %d "+
		"(%d B, type %d, spd %d) + cmd %d (%d B) -> %d other player(s), %s",
		sess.CommanderID, req.CharIndex, c.Handle, c.Name, sheet.ATK, damage,
		plainAttackPercent, defenceLine, req.Target, h.MonsterName(mon), mon.Level, mon.X, mon.Y,
		mon.Cell, pos[0], pos[1], pos[2], before, after,
		hitCmd, len(hit), hitType, spd,
		CmdZCAddHP, len(addHP), others, checksum)

	// AND THIS IS WHERE THE REPEAT IS ARMED (R94), in the ONE place a hit exists, so the
	// press, the replay at the end of an approach walk and a swing of the repeat itself
	// all come through the same line and the same cadence. `next` is THIS hit's clock
	// plus the character's own ASPD - never a delay counted from a tick.
	//
	// It is written AFTER the ATTACK line on purpose: the hit comes first in the log, the
	// rhythm it starts second, and a reader who greps "zone: ATTACK" still lands on the
	// hit's own line.
	h.armSwing(sess, req.CharIndex, req.Target, c.Stance, f, attackInterval(sheet), now)

	return out, ""
}

// stanceASPD is the stance row's own base ASPD (datatable_stance.xml, loaded by
// StanceTable.Combat), quoted in the cadence log line so the reader can see the
// number the sheet started from and not only the one it ended at. 0 means "no
// stance table here", which is a fact the line may state and not an error.
func (h *Handler) stanceASPD(stance uint32) float64 {
	if h.Stances == nil {
		return 0
	}
	return h.Stances.Combat(stance).ASPD
}

// logNoCadenceOnce says, ONCE per server run, that the sheet gave no attack delay
// and that normal attacks are therefore going out untimed. It is the honest half
// of the rule: an answer this server cannot time is left alone rather than given a
// delay nobody measured. ONE line, because the condition is a property of this
// build's data and not of each click.
func (h *Handler) logNoCadenceOnce(charIndex int, handle uint32) {
	if h.noCadenceLogged.Swap(1) != 0 {
		return
	}
	h.Log.Printf("zone: ATTACK NOT THROTTLED: char index %d (handle %d) has no attack "+
		"delay on its sheet (ASPD 0 from SCR_Get_ASPD get_status_pc.scp:1408), so every "+
		"%d is answered - this zone will not invent an interval the character's own data "+
		"does not give", charIndex, handle, CmdCZAttack)
}

// logNoReachOnce says, ONCE per server run, that the character's stance row states
// no far bound (MaxR 0, or no stance table loaded) and that normal attacks therefore
// reach as far as the client lets them aim - the state the zone was in before R90,
// kept as an honest fallback rather than replaced by a made-up reach. ONE line,
// because the condition is a property of this build's data, not of each click.
func (h *Handler) logNoReachOnce(charIndex int, handle uint32, stance uint32) {
	if h.noReachLogged.Swap(1) != 0 {
		return
	}
	h.Log.Printf("zone: ATTACK REACH NOT CHECKED: char index %d (handle %d) stands in "+
		"stance %d (%q), whose row states no MaxR (or this zone has no stance table), so "+
		"a normal attack reaches as far as the client aims - this zone will not invent a "+
		"reach the character's own data does not give", charIndex, handle, stance,
		h.stanceName(stance))
}

// plainSwingDefence is the TARGET's defence TERM for a player's plain attack, as the
// game's own script computes it (ge_snapshot_script_20261022\pc\common_pc.scp, the
// `arg.Attacker == 'player'` damage branch):
//
//	defence = (target.DEF / 2) - (attacker.AR / 2)
//
// It is the same shape the monster path uses from the other side
// (`monsterSwingDamage`: `applyDefence(deal, victimDEF/2)`, with the attacker's AR read
// as 0 because a monster has none modelled).
//
// TWO CLAMPS, both the reference's: a NEGATIVE term means the attacker's AR beats the
// target's DEF, and it must NOT heal the target - the script's own `SCR_GET_BATTLE_
// POSITIVENUM` floors the mirrored term at 0 - while the percent passed to
// `applyDefence` is capped at 95 by the defence rule itself.
func plainSwingDefence(targetDEF, attackerAR float64) float64 {
	term := (targetDEF - attackerAR) / 2
	if term < 0 {
		return 0
	}
	if term > 95 {
		return 95
	}
	return term
}
