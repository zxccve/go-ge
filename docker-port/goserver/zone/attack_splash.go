package zone

import (
	"encoding/binary"
	"math"
	"strconv"
)

// THE SPLASH SWING: ZC_ATTACK_SPLASH 3566 - the packet that names the main target
// AND every extra body the stance's own area reached.
//
// WHY THIS EXISTS. `datatable_stance.xml` holds 174 stances whose AtkType is "Splash"
// and 486 whose is "Single". Until this file every one of them answered a press with
// the Single shape (3564), so a character in a splash stance hit ONE body of a group
// and the rest of the group stood there untouched - the client was never told about
// them, because 3564 has no room for a second body. The reference branches on that
// same column (`PlayerChar.cpp:1987-2002`), and the owner's own client sends the
// splash packet itself, so the shape is measured and not invented.
//
// EVERY OFFSET HERE IS MEASURED, NOT DERIVED. The source is
// `docker-port\history\splash-attack.md`: 16 packets of command 3566 out of ONE live
// capture (`captures/splashmulti20261101/20261101-093405/s2c_7025_50357.bin`), all of
// them from the same attacker (9766799) and every one of them ending exactly on a
// command id the client's own table holds (16 of 16). Two of them are cut into the
// fixtures this file is held against:
//
//	zone\testdata\splash_live_123.bin  123 B  count 0   (6 of the 16 samples)
//	zone\testdata\splash_live_171.bin  171 B  count 1   (1 sample)
//
// The other nine are 160 and 245 bytes with main attack type 7; type 7 is NOT in the
// client's ATTACKTYPE enum (0..5) and what the shipped client does with it is
// unproven, so it is out of scope - see "NOT BUILT" below.
//
// THE ONE UNEXPLAINED BYTE PATTERN, said here so nobody hunts for a meaning: the wire
// always carries count+1 victim records and the LAST one is 48 bytes of zero, in
// 16 of 16 samples (§"The count byte at +74"). WHY the server sends one record too
// many is not established - it is measured, not explained - and it is written, not
// skipped, because the packet this zone sends has to be the packet the client saw.
//
// THE TWO THINGS A BUILDER MUST NOT GET WRONG:
//
//  1. THE AREA IS CENTRED ON THE MAIN TARGET, NOT ON THE CASTER. In the 171-byte
//     sample the extra victim is 109.5 units from the main target and 303.9 from the
//     caster, so a 220-unit circle drawn around the caster would not have contained
//     it (§"The 74-byte base shape" and the numbers this file's test recomputes).
//     Handler.splashVictims asks monstersInSplash about `main.X, main.Y` for exactly
//     that reason - see zone\attack.go's branch.
//  2. THE COUNT AT +74 DOES NOT INCLUDE THE MAIN HIT (measured: the 171 sample's one
//     record is a handle that is NOT the main target at +26). A +74 that included it
//     would make every reader put one body in the wrong slot.
//
// NOT BUILT, and why, so a reader does not look for it:
//
//   - NO TYPE 7. The 37-byte KNOCKDOWN_INFO block is built for a body this zone KILLED
//     and for no other: a killed victim's record (see SplashKillingVictimSize) and the
//     MAIN hit when it is the killing blow (BuildAttackSplashMainKnockdown) both state
//     type 6, the same fatal type the single-target killing blow sends (zone\attack.go).
//     A body that survives carries no block. Type 7 is
//     still not built: it is the wire's critical-looking flavour and this zone has no
//     critical model, so building it would be a roll nobody made. What type 7 MEASURES
//     is what makes the block below safe to write: the one live splash with a per-victim
//     KD block is an 85-byte record (`history\splash-attack.md`, the 245-byte sample,
//     +112 record then +160 KD), i.e. the client's victim loop really does consume a
//     KNOCKDOWN_INFO after a record - the same block this builder appends.
//   - NO CRITICAL ROLL. This zone has no critical model (zone\attack.go), so the MAIN
//     hit is plainAttackType (2 = KNOCKDOWN) and the 123-byte sample's main type 3 is
//     the ONE allowance the tests beside this file permit.
//   - BOTH TYPE FIELDS NOW STATE 0 (ATTACKTYPE_NORMAL), AND THAT IS A PROBE, NOT A
//     MEASUREMENT. The measured samples state 2 (main) and 3 (extra record); this build
//     writes 0 in both, because those are the two fields the client branches on and 0 is
//     the only flavour with nothing extra to consume. See splashMainAttackType and
//     splashVictimAttackType for what each one makes the client do.
//   - NO DROPS, NO DEATH, NO 3531: the same gap zone\attack.go states.

const (
	// CmdZCAttackSplash is ZC_ATTACK_SPLASH, server -> client. The registered size is
	// 0 (variable), so the packet carries its own length at +2 - which is why the
	// length word, and the ceiling derived from it below, are the whole safety story.
	CmdZCAttackSplash = 3566

	// AttackSplashBase is the 74-byte block this packet SHARES with 3564, byte for
	// byte: the command word and length, then the attacker, its spot, the speed, the
	// start time, the main target, its spot, the damage, its HP before, the attack
	// type and the knock-back spot. The offsets are 3564's own (as* in zone\attack.go)
	// ON PURPOSE: it is the same measured block, and a second offset table would be
	// two names for one layout, i.e. the way an offset gets read from the wrong place.
	AttackSplashBase = 74

	// AttackSplashCount is +74, ONE BYTE: the number of EXTRA victims. It does not
	// include the main hit, and it is a BYTE, so 255 is the most it can ever say.
	AttackSplashCount = 74

	// AttackSplashFirst is +75, where the first 48-byte victim record starts.
	AttackSplashFirst = 75

	// AttackSplashMainKD is +75: where the MAIN hit's KNOCKDOWN_INFO sits when that hit
	// is the killing form. THE COUNT DOES NOT MOVE - it stays at +74, and the block
	// takes the place the first victim record would have occupied.
	//
	// THE CLIENT PUTS THE COUNT BEFORE THE BLOCK, and three witnesses say so:
	//
	//   - The 2020 client's 3566 handler `FUN_004e2302`
	//     (`docker-port\out\ge_decompiled.txt:162755`) takes its record cursor from
	//     `FUN_004e11bc` = **`return param_1 + 0x4b`** (`:161966-161972`, +75), keeps
	//     it as the KNOCKDOWN_INFO pointer for a main type 4..7, advances it by 0x25
	//     (+37) to +112 and then loops `while (i < *(byte *)(param_1 + 0x4a))` - the
	//     count read at **+0x4a = +74** (`:162854`, `:162973`). A block at +74 would be
	//     read from the count byte's own position.
	//   - The shipped source's `PKS_ZC_ATTACK_SPLASH` (geZCPacket.h:1449-1452) has
	//     `BYTE count` as the LAST MEMBER OF THE STRUCT, and `CNormalNet::AttackSplash`
	//     `PKT_ADVANCE(ZC_ATTACK_SPLASH)` past it BEFORE it reads a KNOCKDOWN_INFO
	//     (GENormalNet_Action.cpp:272-296) - count first, block after.
	//   - The measured wire: `zone	estdata\splash_live_171.bin` has the count 1 at
	//     +0x4a and the first victim handle at +0x4b; `splash_live_123.bin` has 0 at
	//     +0x4a. Both are the plain form, and both agree on where +74 is.
	//
	// So the killing form is [74-byte main][count @74][37-byte KD @75][records @112].
	// 74 + 1 + 37 = 112, which is why the records land on the same offset either way -
	// and why a builder can get the ORDER wrong and still produce the right LENGTH.
	AttackSplashMainKD = AttackSplashCount + 1

	// AttackSplashKDCount is +74 in the killing form too: the count is the same byte,
	// in the same place, in both forms. It is named here so a reader looking for "where
	// the count goes when the main hit kills" finds +74 and not a moved offset.
	AttackSplashKDCount = AttackSplashCount

	// AttackSplashKDFirst is +112, where the first 48-byte victim record starts in that
	// form - the count at +74, then the 37-byte block at +75.
	AttackSplashKDFirst = AttackSplashMainKD + KnockDownInfoSize

	// SplashVictimSize is one extra victim's record: handle(4) WORLDPOS(12)
	// damage(8) HP-before(8) attack type(4) knock-back WORLDPOS(12) = 48, no gap.
	SplashVictimSize = 48

	// The 48-byte record's fields.
	svHandle     = 0  // OBJHANDLE the extra body that was hit
	svPos        = 4  // WORLDPOS - the body's own position and navmesh cell
	svDamage     = 16 // FLOAT64 the damage, the number the client draws for it
	svHPBefore   = 24 // FLOAT64 that body's HP BEFORE this hit
	svAttackType = 32 // int32 - the client's ATTACKTYPE_* value
	svKnockPos   = 36 // WORLDPOS the knock-back position

	// SplashKillingVictimSize is one KILLING victim's record: the same 48-byte hit,
	// followed by the 37-byte KNOCKDOWN_INFO the client consumes for a knockdown
	// record. 85 = 48 + 37, and the wire measured exactly this stride: the 245-byte
	// sample in `history\splash-attack.md` carries a 48-byte record at +112 and its
	// 37-byte KD block at +160, then the unused record at +197 ("Total = 74 + 1 + 85
	// + 85 = 245. Nothing left over").
	//
	// THE ORDER IS RECORD THEN BLOCK, and that is the CLIENT's order, not a choice:
	// CNormalNet::AttackSplash reads a record with PKT_STR_ADVANCE(HIT_INFO) and then,
	// for a knockdown type, PKT_STR_ADVANCE(KNOCKDOWN_INFO) - so the 37 bytes belong
	// where the record's own 48 end (GENormalNet_Action.cpp:299-340). The history page
	// states the stride as "37-byte KD block then a 48-byte hit record", which its own
	// offsets contradict; the offsets and the client agree, and those are followed.
	SplashKillingVictimSize = SplashVictimSize + KnockDownInfoSize

	// The KNOCKDOWN_INFO block's fields, relative to the START OF THE RECORD (so at
	// +48 of an 85-byte record). The 37 bytes are the same block the single-target
	// killing blow appends at +74 of a 111-byte 3564 - the layout and the VALUES are
	// shared (kdDelayMs, kdPower, kdVAngleDeg, kdHAngleDeg, kdTypeNormal, all in
	// zone\attack.go) and only the base offset differs.
	svKDStartPos = SplashVictimSize  // WORLDPOS - the position the body falls from
	svKDEndPos   = svKDStartPos + 12 // WORLDPOS - the position it lands on
	svKDDelay    = svKDEndPos + 12   // WORD    - how long it stays down
	svKDPower    = svKDDelay + 2     // WORD    - 100 in 10 of 10 measured blocks
	svKDVAngle   = svKDPower + 2     // float   - 45.0 in 10 of 10
	svKDHAngle   = svKDVAngle + 4    // float   - the fall's heading
	svKDType     = svKDHAngle + 4    // BYTE    - KD_TYPE, 1 = KDT_NORMAL in 10/10

	// splashMainAttackType is what the MAIN hit's block states at asAttackType (+58):
	// 0 = ATTACKTYPE_NORMAL in the client's own enum.
	//
	// THIS IS A PROBE, NOT A MEASUREMENT. Every splash in the owner's capture states 2
	// (KNOCKDOWN), 3 (CRITICAL_KD) or 7 in this field; none states 0. It is 0 here because
	// of what the client does with the other two: CNormalNet::AttackSplash
	// (source2/geClient/Source/GENormalNet_Action.cpp:270-345) switches on this field
	// BEFORE its victim loop, and for ATTACKTYPE_KNOCKDOWN / ATTACKTYPE_CRITICAL_KD it
	// consumes a 37-byte KNOCKDOWN_INFO first. This packet carries no such block - the
	// measured tail at +62 is the 12-byte knock-back position, in all 16 samples, types 2,
	// 3 and 7 alike - so a KD-shaped type makes the client read its count and its records
	// from the wrong offset while the separate 3557s still move the plates. That is the
	// owner's report exactly: both bodies hurt, one number drawn. Type 0 takes the plain
	// branch, which consumes nothing, so the count at +74 is read where this builder
	// writes it. Whether the client then DRAWS every number is settled only by a retest in
	// game - a test cannot see a flying number.
	//
	// THE LAYOUT DOES NOT MOVE WITH IT: +62 stays the 12-byte knock-back position, +74
	// stays the count byte, the records stay 48 bytes. Only this value changes.
	//
	// ONE EXCEPTION, AND IT IS NOT THIS CONSTANT: a MAIN target this hit KILLS goes out
	// through BuildAttackSplashMainKnockdown as the fatal form - type 6 and the 37-byte
	// block, the count and the records moved to +111/+112. That is a different packet
	// for a different fact (the body died), not a second value for a surviving hit.
	splashMainAttackType int32 = 0

	// splashVictimAttackType is what EVERY extra victim's record states at svAttackType
	// (+32 of the record): 0 = ATTACKTYPE_NORMAL, for the same probe reason - and about
	// this field the case is sharper, because it was 3 and the copy was already perfect.
	//
	// WHAT IT WAS, AND WHY IT CHANGED: it was 3 (CRITICAL_KD), taken from the 171-byte
	// sample, the only measured splash carrying an extra victim. That copy was FAITHFUL -
	// the 171 test compares the whole packet with NO allowance, so this build reproduced
	// the live packet byte for byte - and the extra body's flying number still did not
	// draw. The measured type therefore cannot have been the cause on its own, and writing
	// it again would only reproduce the fault. 0 is the one flavour left to try: the
	// client's record switch hands type 0 to AddSplashTgt(isCritical=false, isKd=false),
	// the plain call that draws the number, and consumes no extra block.
	//
	// IT IS NOT A CRITICAL CALCULATION and it is not a measured value: this zone has no
	// critical model and rolls nothing. The retest decides; see
	// TestSplashPacketWritesTheNormalAttackType.
	splashVictimAttackType int32 = 0

	// maxSplashVictims is the ceiling the BUILDER enforces, and it is the COUNT BYTE's,
	// not the length word's: +74 is ONE byte, so 255 is the most extra victims a 3566 can
	// ever name, whatever the packet's own length allows.
	//
	// THE TWO CEILINGS, said together so neither is read as the other:
	//
	//	255   THIS one - the count byte at +74. A 256th victim would wrap the count to 0
	//	      while the packet still carried the records, i.e. a packet that disagrees
	//	      with its own count.
	//	224   the LIVE path's, and it is not this constant: Handler.splashVictims asks
	//	      monstersInSplash for at most maxAreaTargets (224) bodies (zone\skill_area.go),
	//	      so a swing in this zone cannot present more than 224 - and 224 is under 255,
	//	      which is why the wrap was never reachable from the game. It is a builder-API
	//	      hazard for any other caller, which is exactly why it is closed here.
	//
	// THE LENGTH WORD IS STILL SAFE AT 255: 75 + 48 x (255 + 1) = 12363 bytes, far under
	// 65535, so the +2 word cannot wrap either. (The length word's own limit would be
	// 1362 records - a number this builder must never use for the count.)
	maxSplashVictims = 255
)

// SplashVictim is one EXTRA body a splash hit reached - never the main target, which
// has its own fields in the base block.
type SplashVictim struct {
	Handle     uint32
	Pos        [3]int32
	Damage     float64
	HPBefore   float64
	AttackType int32
	// Killing marks a hit that took this body's LAST HP: `before > 0 && after == 0`,
	// the same condition takeHP arms the ZC_DEAD on (zone\create.go), so the packet
	// and the death cannot disagree about which hit was the kill - exactly as the
	// single-target killing blow selects its form (zone\attack.go).
	//
	// IT IS THE LEDGER'S ANSWER, NOT A RE-DERIVATION. splashVictims knows `after`;
	// this struct does not carry it, and the builder must not infer a kill from
	// HPBefore <= Damage: a body already at 0 has hp <= damage on every later hit and
	// is not being killed again. A killing record states knockdownAttackType and
	// carries the 37-byte KNOCKDOWN_INFO; AttackType is what a SURVIVING record states
	// and is ignored on a killing one, because a record whose type said 0 while
	// carrying 37 bytes would leave the client's cursor inside the block.
	Killing bool
}

// BuildAttackSplash packs one ZC_ATTACK_SPLASH (3566): the main hit in the 74-byte
// block it shares with 3564, then ONE BYTE of victim count, then one record per extra
// victim - 48 bytes, or 85 for a victim this hit KILLED - then one 48-byte record of
// ALL ZEROS.
//
//	+0  WORD      3566
//	+2  WORD      the WHOLE length = 75 + the records (48 or 85 each) + 48
//	+4  OBJHANDLE the attacker
//	+8  WORLDPOS  the attacker's position (x, y, navmesh cell)
//	+20 WORD      attackSpd
//	+22 DWORD     startTime - always 0: 0 in all 16 samples (§"Still unknown" item 7)
//	+26 OBJHANDLE the main target
//	+30 WORLDPOS  the main target's position
//	+42 FLOAT64   the main damage
//	+50 FLOAT64   the main target's HP BEFORE this hit
//	+58 int32     the main attack type
//	+62 WORLDPOS  the main knock-back position - the target's own, copied
//	+74 BYTE      N = the number of EXTRA victims (the main hit is not counted)
//	+75 ...       N x victim record, then ONE 48-byte record of ALL ZEROS
//
// WHEN THE MAIN HIT IS THE KILL, the packet is BuildAttackSplashMainKnockdown instead:
// the same 74-byte block, then the count at +74 (it never moves), then the 37-byte
// KNOCKDOWN_INFO at +75 and the records from +112 - one block longer, nothing else moved.
// A main hit that survives is this function, byte for byte as before.
//
// A VICTIM RECORD is 48 bytes (handle, pos, damage, HP-before, type, knock-back pos);
// a record whose hit KILLED its body is the same 48 bytes followed by the 37-byte
// KNOCKDOWN_INFO (85 total), and its type field states knockdownAttackType instead of
// the caller's. The last, all-zero record is always 48 bytes - the measured unused
// record carries no KD after it, because the client's loop stops at `count`.
//
// The measured totals: N=0 -> 123 bytes (6 samples), N=1 -> 171 bytes (1 sample). The
// all-zero trailing record is written ALWAYS, even at N=0 (measured 16 of 16). A splash
// that KILLS one of two extras is 219 + 37 = 256 bytes: 75 + 48 + 85 + 48.
//
// A SPLASH WITH NO EXTRA VICTIM IS A VALID HIT, not a refusal: the 123-byte samples
// are exactly that, and the main target is hit either way, so this builder never
// refuses one.
//
// THE KNOCK-BACK POSITION IS THE BODY'S OWN POSITION, the same rule 3564 already uses
// (`asKnockPos`), and the honest half of it is that the capture does NOT make this a
// law: §"What is copied from where" says the 16 knock-backs equal the target position
// 6 times, the attacker's 3 times and neither 7 times. In BOTH fixtures this file is
// held against, it equals the body's own spot, and it is the only position this zone
// can name for a body - so it is written the way 3564 writes it and the gap is said
// out loud here rather than papered over.
//
// `victims` longer than maxSplashVictims (255, the count byte's own range) is TRUNCATED:
// the count at +74 is ONE byte, so a 256th record would wrap it and leave the packet
// disagreeing with its own count. The length word is not the binding limit at 255 - 75 +
// 48 x 256 = 12363 bytes - and the LIVE path is capped lower still, at maxAreaTargets
// (224) by monstersInSplash. The caller is the one that can say so in the log; see
// zone\attack.go's capped line.
func BuildAttackSplash(attacker uint32, attackPos [3]int32, attackSpd uint16,
	target uint32, targetPos [3]int32, damage, targetHPBefore float64,
	attackType int32, victims []SplashVictim) []byte {

	return buildAttackSplash(attacker, attackPos, attackSpd, target, targetPos,
		damage, targetHPBefore, attackType, false, victims)
}

// BuildAttackSplashMainKnockdown packs the same ZC_ATTACK_SPLASH with the MAIN hit in
// the KILLING-BLOW form: the 74-byte main block, the count at +74 (exactly where the
// ordinary form puts it), then the 37-byte KNOCKDOWN_INFO that
// `BuildAttackSingleKnockdown` appends to a killing 3564, then every victim record -
// the records 37 bytes further along than BuildAttackSplash puts them, the count not
// moving at all.
//
// WHY THE BLOCK SITS AT +75 AND NOT AT +74. A 3566 carries its victim count at +74,
// and the client reads that byte unconditionally before it walks the records; the main
// hit's KNOCKDOWN_INFO therefore starts AFTER it, at +75 (AttackSplashMainKD), and the
// records follow at +112. That is the same order the shipped source uses: `BYTE count`
// is the last member of `PKS_ZC_ATTACK_SPLASH` and `CNormalNet::AttackSplash` advances
// past it before reading a block (GENormalNet_Action.cpp:272-296). The 2020 client's
// `FUN_004e2302` is explicit: its KNOCKDOWN_INFO pointer is `FUN_004e11bc()` = packet +
// 0x4b, and its loop bound is `*(byte *)(packet + 0x4a)`. A block written at +74 would
// be read as the count, and the count written at +111 would be read as the block's type
// byte - the packet would still be 160 bytes and every record would still land on +112,
// which is why the order is easy to get wrong and only the client's own two offsets
// settle it.
//
// THE 111 MAIN BYTES ARE COPIED FROM THE SINGLE-TARGET BUILDER, not re-derived: the
// first 74 are the block a 3564 and a 3566 share byte for byte, and the 37 after them
// are the block that builder appends (startPos the target's own position, endPos and
// hAngle `knockdownFall` computes, delay 600, power 100, vAngle 45, type 1). Only +0
// and +2 are corrected - the command is 3566 and the length is this packet's - so the
// two forms cannot drift: one writer, and a change to the fall moves both.
//
// A NON-LETHAL MAIN HIT IS NOT THIS FUNCTION'S JOB: that is BuildAttackSplash, whose
// bytes do not move. The caller selects on `before > 0 && after == 0`, the condition
// takeHP arms the ZC_DEAD on (zone\create.go), so the packet and the death cannot
// disagree about which hit was the kill - the same selector the victims use
// (SplashVictim.Killing) and the single-target killing blow uses (zone\attack.go).
func BuildAttackSplashMainKnockdown(attacker uint32, attackPos [3]int32, attackSpd uint16,
	target uint32, targetPos [3]int32, damage, targetHPBefore float64,
	victims []SplashVictim) []byte {

	return buildAttackSplash(attacker, attackPos, attackSpd, target, targetPos,
		damage, targetHPBefore, knockdownAttackType, true, victims)
}

// buildAttackSplash is the ONE writer behind the two exported forms. mainKilling selects
// the main 111-byte killing block and moves the count and the records with it; when it is
// false every byte is the packet this builder has always sent.
func buildAttackSplash(attacker uint32, attackPos [3]int32, attackSpd uint16,
	target uint32, targetPos [3]int32, damage, targetHPBefore float64,
	attackType int32, mainKilling bool, victims []SplashVictim) []byte {

	if len(victims) > maxSplashVictims {
		victims = victims[:maxSplashVictims]
	}

	// WHERE THE COUNT AND THE RECORDS START. The count is +74 in BOTH forms - the
	// client reads it there unconditionally (`FUN_004e2302` at +0x4a) - and only the
	// records move: +75 in the ordinary form, +112 in the killing-main form, because
	// the 37-byte block sits between the count and the first record. Reading both
	// offsets from the same flag is what keeps the builder and the client's cursor in
	// step; it is NOT what makes the block's position right, which is
	// AttackSplashMainKD's job.
	countAt, first := AttackSplashCount, AttackSplashFirst
	if mainKilling {
		first = AttackSplashKDFirst
	}

	// THE LENGTH IS BUILT FIRST, from the records themselves: one 48-byte unused
	// record at the end plus one record per victim, and a KILLING victim's record is
	// 37 bytes longer - and a KILLING main hit is 37 bytes longer still. Deriving it
	// beats the old expression: with a lethal victim in the list, 75 + 48*(N+1) is
	// 37 bytes short and the length word would cut the last record in half
	// (`splashRecordLen`).
	total := first + SplashVictimSize
	for _, v := range victims {
		total += splashRecordLen(v)
	}

	out := make([]byte, total)
	if mainKilling {
		// THE MAIN BLOCK IS THE SINGLE-TARGET KILLING BLOW'S, split around the count
		// byte: its first 74 bytes are the block the two packets share, and its 37-byte
		// KNOCKDOWN_INFO lands at +75 (AttackSplashMainKD) instead of at +74, because a
		// 3566 has the count at +74 and a 3564 does not. The block's BYTES are copied,
		// not re-written: one writer for the fall, so the two forms cannot drift.
		main := BuildAttackSingleKnockdown(attacker, attackPos, attackSpd, target,
			targetPos, damage, targetHPBefore, knockdownAttackType)
		copy(out[:AttackSplashCount], main[:AttackSplashCount])
		copy(out[AttackSplashMainKD:AttackSplashKDFirst], main[AttackSplashCount:])
	} else {
		binary.LittleEndian.PutUint32(out[asAttacker:], attacker)
		putWorldPos(out[asAtkPos:], attackPos)
		binary.LittleEndian.PutUint16(out[asAtkSpd:], attackSpd)
		// +22 startTime is left ZERO: 0 in all 16 samples, so it is the capture's value
		// and not a field this zone has an opinion about.
		binary.LittleEndian.PutUint32(out[asTarget:], target)
		putWorldPos(out[asTargetPos:], targetPos)
		binary.LittleEndian.PutUint64(out[asDamage:], math.Float64bits(damage))
		binary.LittleEndian.PutUint64(out[asTargetHP:], math.Float64bits(targetHPBefore))
		binary.LittleEndian.PutUint32(out[asAttackType:], uint32(attackType))
		putWorldPos(out[asKnockPos:], targetPos)
	}
	binary.LittleEndian.PutUint16(out[0:], CmdZCAttackSplash)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	out[countAt] = byte(len(victims))

	at := first
	for _, v := range victims {
		r := out[at:]
		binary.LittleEndian.PutUint32(r[svHandle:], v.Handle)
		putWorldPos(r[svPos:], v.Pos)
		binary.LittleEndian.PutUint64(r[svDamage:], math.Float64bits(v.Damage))
		binary.LittleEndian.PutUint64(r[svHPBefore:], math.Float64bits(v.HPBefore))
		if v.Killing {
			// THE FATAL FORM, inside the victim loop: the type the live client takes as
			// a knockdown, then the same 37-byte KNOCKDOWN_INFO the single-target kill
			// appends at +74 (the VALUES are attack.go's, not copies) - startPos the
			// victim's own, endPos 500 units along attacker -> victim with hAngle that
			// bearing, the reference's own `SetKnockDown` geometry (`knockdownFall`).
			binary.LittleEndian.PutUint32(r[svAttackType:], uint32(knockdownAttackType))
			binary.LittleEndian.PutUint16(r[svKDDelay:], kdDelayMs)
			binary.LittleEndian.PutUint16(r[svKDPower:], kdPower)
			binary.LittleEndian.PutUint32(r[svKDVAngle:], math.Float32bits(kdVAngleDeg))
			endPos, hAngle := knockdownFall(attackPos, v.Pos)
			binary.LittleEndian.PutUint32(r[svKDHAngle:], math.Float32bits(hAngle))
			r[svKDType] = kdTypeNormal
			putWorldPos(r[svKDStartPos:], v.Pos)
			putWorldPos(r[svKDEndPos:], endPos)
		} else {
			binary.LittleEndian.PutUint32(r[svAttackType:], uint32(v.AttackType))
		}
		putWorldPos(r[svKnockPos:], v.Pos)
		at += splashRecordLen(v)
	}
	// THE LAST 48 BYTES STAY ZERO - that is the unused record, not padding (see the
	// file header: measured in 16 of 16, cause unknown).
	return out
}

// splashRecordLen is how many bytes one victim's record occupies in a 3566: the
// 48-byte hit, plus the 37-byte KNOCKDOWN_INFO a KILLING hit carries. The builder and
// the client's own loop must agree on this number, so it is stated once and used for
// both the length word and the write cursor.
func splashRecordLen(v SplashVictim) int {
	if v.Killing {
		return SplashKillingVictimSize
	}
	return SplashVictimSize
}

// splashVictims returns the extras for one splash hit: every monster this zone runs
// that is STRICTLY inside radius of the MAIN TARGET, minus the main target itself,
// with its HP taken off the ONE ledger. addHP carries one 3557 per victim, in the
// same order, because a body the client is told about must also get the number that
// moves its plate. capped reports that the area cap was reached.
//
// THE CENTRE IS THE MAIN TARGET (`main.X, main.Y`), never the caster: that is the one
// thing the 171-byte sample measures about the geometry (109.5 from the main target,
// 303.9 from the caster) and the one mistake this helper exists to make impossible.
//
// THE DISTANCE RULE IS NOT WRITTEN HERE. monstersInSplash (zone\skill_area.go) is the
// ONE strict "< radius" scan, shared with the ground skill's 3581, so a splash and a
// skill blast can never disagree about who is inside a circle.
//
// AN EXTRA WITH NO HP IN THE LEDGER IS SKIPPED, NOT REFUSED. This zone cannot name an
// HP it does not keep, and every 48-byte record carries one - but a missing HP on a
// BODY THAT IS MERELY NEARBY must not cancel the hit on the body the player actually
// clicked. The main target's own ledger miss is still a refusal, in zone\attack.go,
// and deliberately not this.
//
// THE RECORD'S ATTACK TYPE IS splashVictimAttackType (0), NEVER THE MAIN HIT'S. That is
// what this function's signature says: `mainAttackType` is what the MAIN block reports and
// it is passed in only so a reader can see the two side by side - the extra records
// deliberately do not use it, so the two can never drift back into one value by accident.
// Both are 0 today (the probe), but they are TWO constants because they are two fields of
// the packet and the client reads them in two different places.
//
// A VICTIM THIS HIT KILLS IS THE ONE EXCEPTION, and it is the builder's: the record's
// type becomes knockdownAttackType and the 37-byte KNOCKDOWN_INFO follows it, so the
// client plays the fall for a body killed by a splash exactly as it does for one killed
// by a single target (zone\attack_splash.go's SplashKillingVictimSize). The flag set here
// is `before > 0 && after == 0` - the ledger's answer, the one takeHP arms the death on.
func (h *Handler) splashVictims(main Monster, radius float64, damage float64,
	mainAttackType int32, killer uint32, killerLv int) (victims []SplashVictim, addHP [][]byte, capped bool) {

	// NOT USED FOR THE RECORDS, on purpose (see above): stated as an explicit discard so
	// no reader can think an extra victim's type is derived from the main hit's.
	_ = mainAttackType

	found := h.monstersInSplash(main.X, main.Y, radius, maxAreaTargets)
	capped = len(found) >= maxAreaTargets

	for _, m := range found {
		if m.Handle == main.Handle {
			continue // the main hit has its own fields in the base block
		}
		before, after, ok := h.damageMonsterBy(m, damage, killer, killerLv)
		if !ok {
			continue // no HP in this zone's ledger, so no HP this side can state
		}
		victims = append(victims, SplashVictim{
			Handle:     m.Handle,
			Pos:        [3]int32{m.X, m.Y, m.Cell},
			Damage:     damage,
			HPBefore:   before,
			AttackType: splashVictimAttackType,
			// THE KILL, DECIDED WHERE `after` IS KNOWN. `before > 0 && after == 0` is
			// the condition takeHP arms the ZC_DEAD on (zone\create.go), so this
			// record's fatal form and the death packet can never name different hits.
			Killing: before > 0 && after == 0,
		})
		addHP = append(addHP, BuildAddHP(m.Handle, after, damage))
	}
	return victims, addHP, capped
}

// logSplashCappedOnce says, ONCE per server run, that a splash circle held more bodies
// than this build will put in one 3566 and the list was cut. The binding ceiling is
// maxAreaTargets (224) - the area scan's own cap, below the count byte's 255 - so the
// line quotes that number. ONE line, because it is a fact about the area cap and not
// about each swing - the same Swap(1) trick as logNoCadenceOnce.
func (h *Handler) logSplashCappedOnce(stance uint32, radius float64) {
	if h.splashCappedLogged.Swap(1) != 0 {
		return
	}
	h.Log.Printf("zone: ATTACK SPLASH CAPPED: stance %d (%q) splashes a %g-unit circle "+
		"and it held %d bodies, which is this build's area cap (maxAreaTargets) - the list "+
		"stops there, and the count byte at +74 could not name more than 255 anyway (said "+
		"once per run)", stance, h.stanceName(stance), radius, maxAreaTargets)
}

// logSplashNoRangeOnce says, ONCE per server run, why a Splash stance splashed nobody:
// its row states SplRange 0 (or this zone has no stance table). It is NOT an error -
// 0 is the table's own "no area" - and the hit still goes out as a 123-byte 3566,
// because a splash with no extra victim is a valid hit.
func (h *Handler) logSplashNoRangeOnce(stance uint32) {
	if h.splashNoRangeLogged.Swap(1) != 0 {
		return
	}
	h.Log.Printf("zone: ATTACK SPLASH WITHOUT AN AREA: stance %d (%q) is AtkType %q but "+
		"its row states SplRange 0 (or this zone has no stance table), so the circle holds "+
		"nobody and the 3566 carries count 0 - the main target is still hit, and no radius "+
		"is invented for it (said once per run)", stance, h.stanceName(stance),
		"Splash")
}

// splashHandles is the log's rendering of a victim list: the handles in the order the
// records carry them, so a reader can check the packet against the line.
func splashHandles(victims []SplashVictim) []string {
	out := make([]string, 0, len(victims))
	for _, v := range victims {
		out = append(out, strconv.FormatUint(uint64(v.Handle), 10))
	}
	return out
}
