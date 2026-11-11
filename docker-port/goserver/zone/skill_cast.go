package zone

import (
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"geserver/protocol"
)

// THE SKILL CAST: CZ_SKILL_TARGET 3646 / CZ_SKILL 3645 in,
// ZC_SKILL_CASTING 3571 now and ZC_SKILL_MELEE 3573 when the clock fires.
//
// WHY THIS EXISTS. The client sends 3646 when a skill is aimed at a monster
// (CZ_Skill_Target sets charIndex; the 3645 CZ_Skill sender does not and carries no
// target at all). Until this file the zone answered neither, so the request fell
// into the "not handled yet" branch and pressing a skill did nothing on screen.
//
// EVERY OFFSET IN THIS FILE IS MEASURED, NOT DERIVED. The source is
// `history\combat.md` §9 - one live cast of Rapid Shot at a Scarecrow on Thueringen
// Lakeside, whose 3571 and 3573 are cut into `zone\testdata\live_skill_casting.bin`
// (60 B) and `zone\testdata\live_skill_melee.bin` (328 B). §9.8 then read the same
// offsets out of the shipped `ge.exe`, at the widths it reads them, which is what
// turns "the arithmetic fits" into "the client reads these bytes":
//
//	CZ_SKILL_TARGET 3646 (19 B)  +10 BYTE charIndex, +11 DWORD skill, +15 int target
//	CZ_SKILL        3645 (15 B)  +10 BYTE charIndex, +11 DWORD skill  (no target)
//
//	ZC_SKILL_CASTING 3571 (60 B)
//	  +2 int caster, +6 WORLDPOS beginPos, +18 WORLDPOS endPos, +30 WORD skill,
//	  +44 int target handle, +48 WORLDPOS target position
//
//	ZC_SKILL_MELEE 3573 (328 B)
//	  +2 int caster, +6/+18 caster begin/end position, +30 WORD skill,
//	  +32 int32 stack skill count, +36 int target handle, +40 FLOAT64 damage,
//	  +48 int32 attack type, +52 FLOAT64 target HP, +60 BYTE[256] buff mask,
//	  +316 WORLDPOS target position
//
// THE CAST HAS TWO HALVES, SEPARATED BY A CLOCK, AND THE SPLIT IS MEASURED. On the
// live server the 3571 and the result are NOT one reply: they are 605-949 ms apart,
// and the player's move clicks fall in between, are echoed, and do not cancel anything
// (captures\cancel20261031\20261031-191902, read in
// history\cancel-and-animation.md §2/§3). So this file builds the 3571 and starts the
// clock (zone\skill_cast_clock.go), and re-runs ITSELF with `castLand` when the clock
// fires to build the 3573 from the body's LIVE position. One handler, one set of
// offsets, two moments.
//
// WHAT THIS FILE DELIBERATELY DOES NOT SEND, so a reader does not look for it:
//
//   - No 3574 ZC_SKILL_MELEE_KD. The live server flagged the knockdown INSIDE its
//     3573 (`+48 = 2`) and sent no 3574 at all (§9.2, 3574 x0).
//   - No 3556 ZC_CHANGE_BUFF for a monster handle (§9.5: a null-dereference warning
//     stands), no cooldown 3583, no buff list, no stance traffic - other lanes.
//   - The target's 3557 ZC_ADD_HP IS SENT, and this file used to say it was not. The
//     old note read "this lane has no LIVE monster HP ... the Scarecrow lane owns the
//     monster's HP". That was true when it was written and stopped being true when
//     zone\create.go gave every spawned body an HP - the SAME ledger the plain attack
//     spends (h.damageMonster). The landing reads `before`/`after` from it, so the
//     3573's +52 and the 3557's +6 cannot disagree, and BOTH go out in ONE fan-out
//     call (R93), because a watcher that got the animation with no delta, or a delta
//     with no animation, would draw half an action. history\combat.md §9.2/§9.4
//     measured the reference doing exactly this: the Rapid Shot's 3573 is followed by
//     the target's 3557 carrying the same damage double. A body this zone keeps NO HP
//     for still gets no delta - it is skipped, logged once per cast, with the reason
//     named, rather than invented.
//   - THE OTHER PLAYERS ARE TOLD, AND THIS LANE USED TO SAY THEY WERE NOT (R93).
//     The 3571, the 3573 and the target's 3557 are built ONCE and handed to every other
//     session in the zone unchanged - the same rule broadcastMove follows for a walk -
//     and each action logs its fan-out once (`-> N other player(s)`).
//     THE REASON IS THE OWNER'S OWN TWO CLIENTS, and it reverses what this file said
//     until R93: "no capture shows a second observer's screen during a cast, so a
//     wrong broadcast is a visible bug on somebody else's screen and a missing one is
//     not". The first half of that was true. The second was not. He stood a second
//     character in the field, cast at a Scarecrow with the first, and the watcher saw
//     a body STANDING STILL through the whole animation - his words: "animation not
//     showing on the observer ... only action such as normal attack or skill". So the
//     captures were not the last word, a MISSING broadcast is a visible bug on
//     somebody else's screen, and the conservative silence WAS the bug.
//     WHO IS TOLD IS sendOthers' DECISION, NOT THIS FILE'S: the fan-out shares the
//     movement one, so the R135e update cull (a viewer whose body is past the map's
//     ViewMiddleRange) suppresses a cast exactly as it suppresses a walk. No packet is
//     rebuilt per recipient - one map, one process, and the radius is the map row's own
//     number (ROADMAP R91/R93/R135e).

const (
	// CmdCZSkillTarget is CZ_SKILL_TARGET, client -> server, 19 bytes FIXED:
	// cmd(2) seq(4) checksum(4) charIndex(1) skill(4) target(4). The size is the
	// one the client's own table registers (`data\cmd_table_live_z.txt:1313`
	// "3646 19 CZ_SKILL_TARGET").
	CmdCZSkillTarget = 3646

	// CmdCZSkill is CZ_SKILL, client -> server, 15 bytes FIXED: the same header and
	// charIndex, a skill, and NO target field (`cmd_table_live_z.txt:1312`).
	CmdCZSkill = 3645

	// CmdZCSkillCasting is ZC_SKILL_CASTING, server -> client, 60 bytes FIXED.
	CmdZCSkillCasting = 3571

	// SkillCastingSize is 3571's whole packet: 2+4+12+12+2+4+4+4+4+12 = 60. The
	// client's handler's highest read is +56..+59, which is the registered size.
	SkillCastingSize = 60

	// CmdZCSkillMelee is ZC_SKILL_MELEE, server -> client, 328 bytes FIXED.
	CmdZCSkillMelee = 3573

	// SkillMeleeSize is 3573's whole packet: 2+4+12+12+2+4+4+8+4+8+256+12 = 328.
	// Corroborated twice: every byte is accounted for, and 3574 (the knockdown
	// variant) is registered at 365 = 328 + 37.
	SkillMeleeSize = 328

	// The client's two small fields, by offset.
	czCharIndex = 10 // BYTE
	czSkillID   = 11 // DWORD
	czTarget    = 15 // int32 handle, CZ_SKILL_TARGET only

	czSkillSize       = 15 // 3645: 10 header + 1 + 4
	czSkillTargetSize = 19 // 3646: 10 header + 1 + 4 + 4

	// 3571's fields.
	scCaster    = 2  // int handle
	scBegin     = 6  // WORLDPOS: 3 x int32
	scEnd       = 18 // WORLDPOS
	scSkill     = 30 // WORD skill id
	scCastTime  = 36 // DWORD the wind-up in ms - the client's `packet->time`
	scCastTotal = 40 // DWORD the same wind-up, carried into the FSM cast message
	scTarget    = 44 // int handle
	scTargetPos = 48 // WORLDPOS

	// 3573's fields.
	smCaster     = 2   // int handle
	smBegin      = 6   // WORLDPOS
	smEnd        = 18  // WORLDPOS
	smSkill      = 30  // WORD skill id
	smStack      = 32  // int32 stack skill count
	smTarget     = 36  // int handle
	smDamage     = 40  // FLOAT64 the damage number
	smAttackType = 48  // int32, the client's ATTACKTYPE_* value
	smTargetHP   = 52  // FLOAT64 the target's HP
	smBuffMask   = 60  // BYTE[256], copied wholesale by the client
	smBuffLen    = 256 // its width
	smTargetPos  = 316 // WORLDPOS, 60 + 256

	// worldPosLen is the width of every WORLDPOS in these two packets: three
	// int32 (x, y, navmesh cell) - the same triple zone\monster.go sends at +15 of
	// a 3527. The height is never on the wire; the cell is what makes it ground.
	worldPosLen = 12

	// attackTypeNormal is 0 in the client's own enum (`geZCTypes.h:512`:
	// 0 NORMAL, 1 CRITICAL, 2 KNOCKDOWN, 3 CRITICAL_KD, 4 BUFF_SUCCESS,
	// 5 NO_EFFECT). This lane sends NORMAL because it models no knockdown: the
	// reference's 2 is the reference server's decision, not a field we can copy.
	attackTypeNormal = 0
)

// SkillRequest is one decoded cast request.
type SkillRequest struct {
	// CharIndex is the client's team slot: the position in THIS connection's own
	// team list, the same field CZ_CHANGE_STANCE and CZ_EQUIP carry.
	CharIndex int
	// Skill is the skill CLASS ID the client pressed (10561 RapidShot in the
	// capture). It is never trusted: the handler checks it against the stance the
	// character is actually standing in before anything is applied.
	Skill uint32
	// Target is the monster's handle. It is only meaningful when HasTarget is true:
	// CZ_SKILL (3645) has no such field.
	Target uint32
	// HasTarget says whether the frame carried a target at all.
	HasTarget bool
}

// ParseSkillTarget reads CZ_SKILL_TARGET (3646):
//
//	+0 cmd(2)=3646  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 skill(4)  +15 target(4)
//
// ok is false when the frame is not exactly the 19 bytes the client's table
// registers. EXACT, not "at least": the fields are fixed offsets, the splitter cuts
// a frame at the declared size, and a frame of another length means the table and
// the bytes disagree - reading fields out of it would be reading whatever followed.
func ParseSkillTarget(pt []byte) (SkillRequest, bool) {
	var req SkillRequest
	if len(pt) != czSkillTargetSize {
		return req, false
	}
	req.CharIndex = int(pt[czCharIndex])
	req.Skill = binary.LittleEndian.Uint32(pt[czSkillID:])
	req.Target = binary.LittleEndian.Uint32(pt[czTarget:])
	req.HasTarget = true
	return req, true
}

// ParseSkill reads CZ_SKILL (3645): the same fields at the same offsets, 15 bytes,
// and NO target - this sender does not even fill charIndex in the reference
// (§9.1). HasTarget is false.
func ParseSkill(pt []byte) (SkillRequest, bool) {
	var req SkillRequest
	if len(pt) != czSkillSize {
		return req, false
	}
	req.CharIndex = int(pt[czCharIndex])
	req.Skill = binary.LittleEndian.Uint32(pt[czSkillID:])
	return req, true
}

// putWorldPos writes one WORLDPOS: x, y, cell, three int32, little endian.
func putWorldPos(dst []byte, p [3]int32) {
	binary.LittleEndian.PutUint32(dst[0:], uint32(p[0]))
	binary.LittleEndian.PutUint32(dst[4:], uint32(p[1]))
	binary.LittleEndian.PutUint32(dst[8:], uint32(p[2]))
}

// worldPos reads one WORLDPOS back, for the tests and for a re-serialise check.
func worldPos(src []byte) [3]int32 {
	return [3]int32{
		int32(binary.LittleEndian.Uint32(src[0:])),
		int32(binary.LittleEndian.Uint32(src[4:])),
		int32(binary.LittleEndian.Uint32(src[8:])),
	}
}

// BuildSkillCasting packs one ZC_SKILL_CASTING (3571), 60 bytes: the wind-up, sent
// BEFORE the hit (§9.2 measured 3571 then 3573, never the reverse).
//
//	+0  WORD      3571
//	+2  int       caster handle
//	+6  WORLDPOS  beginPos   the caster's own position
//	+18 WORLDPOS  endPos     the same in the capture (the caster stood still)
//	+30 WORD      skill id
//	+32 .. +35    LEFT ZERO - see below
//	+36 DWORD     castTime   the wind-up in ms - the animation-speed divisor
//	+40 DWORD     castTime   the same number - the total the cast state carries
//	+44 int       target handle
//	+48 WORLDPOS  target position
//
// +36 AND +40 ARE THE WIND-UP, AND THE CLIENT SAYS SO ITSELF. Every measurement
// that follows is from the shipped `ge.exe` (handler 0x4e4331, read 2026-10-31;
// addresses are the binary's own):
//
//	0x4e435c PUSH [ESI+0x24]   +36 is argument 7 of the call at 0x4e43a1
//	0x4e4353 PUSH [ESI+0x28]   +40 is argument 8 of the same call
//	0x612b12                   copies them to the cast-paremeter block at
//	                           +0x148 and +0x14c (beginPos +0x118, tgtPos +0x138,
//	                           target handle +0x144), then calls 0x6024ad
//	0x5fd6d9 MOV EAX,[EBP+0x1c] / 0x5fd6dc MOV [ESI+0x44],EAX   <- +36 -> m_castTime
//	0x5fd6df MOV EAX,[EBP+0x20] / 0x5fd6e6 MOV [ESI+0x48],EAX   <- +40 -> the next
//	0x5fc20a MOVD XMM0,[ESI+0x44] / 0x5fc212 CVTDQ2PS / 0x5fc205 Skill CastTime /
//	0x5fc225 DIVSS             <- the CLIENT DIVIDES THE SKILL'S OWN CastTime BY +36
//	                           and hands the result to sceneActor->SetAnimSpeed
//	                           (vtable +0xc0); this is GeActor.cpp:2206
//	0x5fc191 MOV ECX,[ESI+0x48] <- +40 is pushed into the FSM cast-start message
//	                           (`PUSH 0xa; CALL 0x5f03cd`), i.e. it is the total the
//	                           cast state runs on, which is what
//	                           CNormalObject::OnHandleFSMCastStart stores as
//	                           `m_castTotalTime` and the gauge is drawn from
//
// Both fields held the SAME value (483) in the one captured cast, which is exactly
// what a server writing its own `castingTime` into both would look like - and the
// alternative reading (writing only one and leaving the other to chance) cannot
// produce two identical numbers. So this builder writes the ROW'S OWN CastTime into
// BOTH, and the capture is reproduced byte for byte at both offsets.
//
// THE SOURCE CHAIN FROM THE WIRE TO THE PURPLE BAR is:
//
//	GENormalNet_Action.cpp:1047 CNormalNet::SkillCasting
//	  :1055  geskill::SkillCasting(packet->handle, ..., packet->skill, packet->time)
//	GeSkill.cpp:106,118              geskill::SkillCasting(..., int castTime)
//	                                 `castParm.castTime = castTime`
//	GeActor.cpp:764,790              CActor::EnterCast: `m_castTime = castTime`, then
//	                                 SetActionState(CAS_SKILLCASTING)
//	GeActor.cpp:2204                 the state's ON_ENTER hands `(int)m_castTime` to
//	                                 the FSM as AM_CAST_START, and :2206 scales the
//	                                 SKILL animation by `CastTime / m_castTime`
//	GENormalObject.cpp:1258-1262     CNormalObject::OnHandleFSMCastStart stores it as
//	                                 `m_castTotalTime` and resets `m_castAccumTime`
//	GENormalObject.cpp:505-512       every frame: `m_castAccumTime += elapsed`, and
//	                                 while the state is CAS_SKILLCASTING the gauge is
//	                                 drawn as `SetInfo(x, y, m_castAccumTime,
//	                                 m_castTotalTime)` and shown
//	GENormalObject.cpp:3079          the gauge itself: `MakeGauge("castgauge_pc", "_CAST")`
//
// So the bar is CLIENT-side and its length is this wind-up, NOTHING ELSE. THE TWO
// NUMERIC TIE-BREAKERS: the reference server fills `out.time` from `castingTime`
// (`FieldChar_FSM.cpp:419-427`, where `castingTime` is `GetSkillCastingTime()`,
// `PlayerChar.cpp:2942-2957`), and the one captured cast carries 483 = the skill
// row's CastTime 600 x 0.805 - the server's cast-speed-reduced wind-up, not a
// coincidence. THIS SERVER MODELS NO CAST SPEED, so it writes the row's CastTime
// as-is: the bar is correct in construction and may run slightly longer than the
// reference's 483 for that character. That difference is a missing stat, not a
// made-up delay.
//
// WITH 0 THE BAR IS NOT DRAWN, and worse: `GeActor.cpp:2206` computes
// `SetAnimSpeed(originCastTime / m_castTime)`, i.e. a division by zero. The
// animation is over in no time, the state leaves CAS_SKILLCASTING, and
// GENormalObject hides the gauge in the same frame. That is why this zone sends NO
// 3571 at all when the row's CastTime is 0 (zone\skill_cast.go cast, the
// `clock.length <= 0` branch): the packet would be a divide by zero, not a bar of
// length zero.
//
// THE FOUR BYTES AT +32..+35 ARE LEFT ZERO ON PURPOSE, and they are the only
// allowance this builder has against the captured packet: the 3571 handler never
// reads +32 (no instruction touches [ESI+0x20]; the live packet's 0x3168C6C8 there
// is uninitialised stack), so writing anything would invent a field. Everything
// else in the 60 bytes is either measured from the capture or set from this zone's
// own world.
func BuildSkillCasting(caster uint32, begin, end [3]int32, skill uint16, castTime uint32,
	target uint32, targetPos [3]int32) []byte {

	out := make([]byte, SkillCastingSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCSkillCasting)
	binary.LittleEndian.PutUint32(out[scCaster:], caster)
	putWorldPos(out[scBegin:], begin)
	putWorldPos(out[scEnd:], end)
	binary.LittleEndian.PutUint16(out[scSkill:], skill)
	binary.LittleEndian.PutUint32(out[scCastTime:], castTime)
	binary.LittleEndian.PutUint32(out[scCastTotal:], castTime)
	binary.LittleEndian.PutUint32(out[scTarget:], target)
	putWorldPos(out[scTargetPos:], targetPos)
	return out
}

// BuildSkillMelee packs one ZC_SKILL_MELEE (3573), 328 bytes: the hit, and the one
// packet the client draws a damage number from.
//
//	+0   WORD      3573
//	+2   int       caster handle
//	+6   WORLDPOS  beginPos, +18 WORLDPOS endPos   the caster's position
//	+30  WORD      skill id
//	+32  int32     stack skill count   (0: this lane has no stacking skills)
//	+36  int       target handle
//	+40  FLOAT64   DAMAGE - and it stays a float64: the client loads it with MOVSD
//	               and prints it truncated, so an integer conversion would truncate
//	               a bigger number silently (§9.7).
//	+48  int32     attack type         (attackTypeNormal)
//	+52  FLOAT64   the target's HP, as this zone knows it
//	+60  BYTE[256] buff mask - copied whole by the client (0x100 + a REP MOVSD); it
//	               never looks inside. Zero here: this lane models no buffs, and a
//	               bit invented here would draw a buff icon nothing granted.
//	+316 WORLDPOS  the target's position
//
// `buffMask` may be nil, which is the normal case and means "all zero". A slice
// longer than 256 is refused by truncation rather than by panic: the field is
// fixed width and the packet is the thing that must be 328 bytes.
func BuildSkillMelee(caster uint32, begin, end [3]int32, skill uint16, stackSkillCount int32,
	target uint32, damage float64, attackType int32, targetHP float64,
	buffMask []byte, targetPos [3]int32) []byte {

	out := make([]byte, SkillMeleeSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCSkillMelee)
	binary.LittleEndian.PutUint32(out[smCaster:], caster)
	putWorldPos(out[smBegin:], begin)
	putWorldPos(out[smEnd:], end)
	binary.LittleEndian.PutUint16(out[smSkill:], skill)
	binary.LittleEndian.PutUint32(out[smStack:], uint32(stackSkillCount))
	binary.LittleEndian.PutUint32(out[smTarget:], target)
	binary.LittleEndian.PutUint64(out[smDamage:], math.Float64bits(damage))
	binary.LittleEndian.PutUint32(out[smAttackType:], uint32(attackType))
	binary.LittleEndian.PutUint64(out[smTargetHP:], math.Float64bits(targetHP))
	copy(out[smBuffMask:smBuffMask+smBuffLen], buffMask) // short or nil: the rest stays 0
	putWorldPos(out[smTargetPos:], targetPos)
	return out
}

// castDamage is the damage number this server writes into 3573 +40, and it is OURS.
//
// The 15,233,403 on the reference's screen came from THAT server's character and
// ITS multipliers (`DMG Multi 9.65x | Hard Multi 1000x`, §9), which this tree does
// not have and must not pretend to.
//
//	damage = ATK x SklATK / 100  -  target DEF
//
//	ATK    this character's own sheet, Handler.sheetFor(c).ATK (zone\status_script.go):
//	       base stats, the worn weapon and the stance's own StnATK. The same number
//	       the 3684 status panel shows.
//	SklATK the skill's own row in datatable_skill.xml, a PERCENTAGE of ATK (7 for
//	       Rapid Shot 10561) - so a hard skill hits harder than a weak one.
//	DEF    the target's defence: THIS ZONE HAS NONE. A Monster row carries level,
//	       EXP, MHP and tactics and no DEF, and monster stats belong to the
//	       Scarecrow lane. The term is therefore 0 today and keeps its place.
//
// Floored at 1: a weaponless character has ATK 0 and a buff skill has SklATK 0, and
// a cast that landed must still put a number on screen rather than 0.
//
// THIS IS THE FALLBACK, NOT THE ANSWER, AND IT SITS AT ABOUT 1/100 OF IT. When a
// damage-script corpus is armed the number on the wire is `sheet.ATK * factor`, a
// MULTIPLIER and no `/100` (zone\skill_script.go, and the game's own arithmetic at
// common_monster.scp:7864 / common_pc.scp:4933). This function keeps its `/100`
// because `SklATK` really is a percentage here, so for a fire-and-forget cast it
// lands roughly two orders of magnitude low - which is exactly why it is only what
// a zone with no corpus, or a raising script, falls back to.
func castDamage(atk, sklATK, targetDEF float64) float64 {
	d := atk*sklATK/100 - targetDEF
	if d < 1 {
		return 1
	}
	return d
}

// monsterByHandle finds a monster THIS ZONE is running, so a cast cannot name a
// body that is not here (a stale handle, a teammate's handle, a forged one).
//
// The two sources are the whole world this zone has: the fixed S1 monster
// (`DevStandingMonster`, only when `-spawnmonster` is on) and the monsters a script
// created (`Handler.spawnedMonster`, zone\create.go). The lock is the same one the
// spawn path takes: a cast can run while a script is mid-spawn on another
// connection.
func (h *Handler) monsterByHandle(handle uint32) (Monster, bool) {
	if h.SpawnMonster {
		if m := DevStandingMonster(); m.Handle == handle {
			return m, true
		}
	}
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for _, m := range h.spawnedMonster {
		if m.Handle == handle {
			return m, true
		}
	}
	return Monster{}, false
}

// casterPos is where the casting character stands RIGHT NOW, by the walked-position
// rule (Session.charPos, zone\move.go) - never the last position this side stored
// for it.
//
// WHY THIS IS A BUG FIX AND NOT A REFACTOR. Every cast packet carries the caster's
// position, and the CLIENT TREATS THAT FIELD AS THE TRUTH rather than as a label:
//
//	ZC_SKILL_CASTING 3571 -> CNormalNet::SkillCasting (GENormalNet_Action.cpp:1047)
//	  -> geskill::SkillCasting (GeSkill.cpp:106) -> CActor::EnterCast
//	     (geClientShared\source\GeActor.cpp:756-776), which does
//	     `SetGroundPos(beginPos); SetMoveState(CMS_STOP);`
//	ZC_SKILL_MELEE 3573 -> CActor::Attack does the same with its srcPos
//	     (`SetGroundPos(pAtkParm->srcPos)`, GeActor.cpp:~301-311)
//
// SetGroundPos MOVES THE ACTOR. So a stale position here is not a cosmetic number in a
// log: it is the owner's "it will jump/teleport ... to the initial skill casting
// location". Character.Position is NOT that place - it is written only when a walk is
// CLOSED (move.go endWalks sets it to the segment's destination at the moment it sends
// the stop, which is eta x 1.4 + 250 ms after the client's report) or when the team is
// PLACED (zone.go, the spawn's formation slots). A player who walks and then presses a
// skill before that stop fires would be cast from the point they stood on BEFORE the
// walk.
//
// The ORIGINAL server reads the same rule: FieldChar_FSM.cpp:396
// `WORLDPOS beginPos = m_pos;` - its own current position of the body, at the frame the
// cast starts, not a remembered cast origin.
func casterPos(sess *Session, idx int, now time.Time) [3]int32 {
	return sess.charPos(idx, now)
}

// castSkill answers CZ_SKILL_TARGET (3646) and CZ_SKILL (3645).
//
// THE REFUSALS COME FIRST, EACH WITH ITS OWN LINE AND NOTHING SENT, in this order:
// a frame of the wrong length, a character index that is not this team, a skill the
// character's CURRENT STANCE does not offer, a zone with no skill table to name or
// price the skill, a 3645 with no target, and a target handle that is not a monster
// this zone is running. A refused cast sends NOTHING: the client keeps its own
// cooldown and its own animation, and a 3573 for a cast this server never validated
// would be a hit neither side holds.
//
// R87: AN OUT-OF-RANGE TARGET IS NOT A REFUSAL ANY MORE - it is a WALK, and the cast
// fires when the character gets there (zone\skill_approach.go). `resumed` marks the
// replay of that same frame at the end of the walk, which must never start a second
// walk.
func (h *Handler) castSkill(sess *Session, f protocol.Frame) ([][]byte, error) {
	return h.castSkillAt(sess, f, castPress)
}

// castSkillAt is castSkill with the bit that says WHICH HALF of the cast this call
// produces: the client's frame (castPress), the end of the approach walk
// (castAfterWalk), or the pending clock running out (castLand). See zone\skill_cast_clock.go.
func (h *Handler) castSkillAt(sess *Session, f protocol.Frame, step castStep) ([][]byte, error) {
	var req SkillRequest
	var ok bool
	switch f.Cmd {
	case CmdCZSkillTarget:
		req, ok = ParseSkillTarget(f.Data)
		if !ok {
			h.Log.Printf("zone: SKILL refused: %d-byte frame, CZ_SKILL_TARGET is a fixed "+
				"%d bytes - nothing sent", len(f.Data), czSkillTargetSize)
			return nil, nil
		}
	case CmdCZSkill:
		req, ok = ParseSkill(f.Data)
		if !ok {
			h.Log.Printf("zone: SKILL refused: %d-byte frame, CZ_SKILL is a fixed %d "+
				"bytes - nothing sent", len(f.Data), czSkillSize)
			return nil, nil
		}
	default:
		return nil, nil
	}

	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: SKILL refused: commander %d character index %d is not in "+
			"this team (%d member(s) spawned) - nothing sent",
			sess.CommanderID, req.CharIndex, len(sess.Team))
		return nil, nil
	}
	c := &sess.Team[req.CharIndex]

	// A zone with no stance table cannot say what the character may cast, and it
	// must not guess: the stance's own SkillID1..5 IS the list the client was sent,
	// so without it there is no way to tell a real cast from a forged one.
	if h.Stances == nil {
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d - this "+
			"zone has no stance table (Handler.Stances is nil), so it cannot say which "+
			"skills the character's stance offers - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}

	// THIS IS THE ONLY PERMISSION RULE. The skill must be one the character's
	// CURRENT stance offers (datatable_stance.xml SkillID1..5), which is exactly the
	// list the client's own bar was built from (zone\skill_list.go) - so a skill the
	// player cannot legitimately have pressed is refused, and nothing else is
	// second-guessed: no cooldown, no SP, no range (other lanes own those).
	offered := h.Stances.StanceSkills(c.Stance)
	if !intIn(offered, int(req.Skill)) {
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d (%q) is "+
			"not one its current stance %d (%q) offers %v - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill,
			h.SkillName(req.Skill), c.Stance, h.Stances.StanceName(c.Stance), offered)
		return nil, nil
	}

	if h.Skills == nil {
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d - this "+
			"zone has no skill table (Handler.Skills is nil), so it cannot name the "+
			"skill or price its damage - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}

	if !req.HasTarget {
		// CZ_SKILL 3645 is a cast with no target: a self or team skill. This lane
		// answers the TARGETED cast only, because the damage number it can prove is
		// the one 3573 carries for one monster; a buff or a heal needs the buff list
		// (3556) and the SP traffic, which other lanes own.
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d (%q) "+
			"CZ_SKILL 3645 carries no target handle, and this lane answers the targeted "+
			"cast (3646) only - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill))
		return nil, nil
	}

	mon, ok := h.monsterByHandle(req.Target)
	if !ok {
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d (%q) "+
			"target handle %d is not a monster this zone is running - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill,
			h.SkillName(req.Skill), req.Target)
		return nil, nil
	}

	// THE SKILL'S OWN ROW: its range, its damage factor, and the CastTime the pending
	// clock is built from. Unknown means nothing is sent - a cast with no row has no
	// range and no length.
	row, known := h.Skills.Row(req.Skill)
	if !known {
		h.Log.Printf("zone: SKILL refused: commander %d char %d (%d) skill %d is not a "+
			"row in datatable_skill.xml, so its range and its damage factor are both "+
			"unknown - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}

	pos := casterPos(sess, req.CharIndex, time.Now())
	targetPos := [3]int32{mon.X, mon.Y, mon.Cell}

	// ACCEPTED. The cast is described in the character's own numbers, from its own
	// sheet and the skill's own row - never from the reference's multipliers.
	sheet := h.sheetFor(*c)
	damage := castDamage(sheet.ATK, row.SklATK, 0) // the ONE damage function

	// THE 3573's HP WHEN THE LEDGER DOES NOT KNOW THE BODY: the value the spawn carried
	// (a Monster row's MHP). A body this zone SPAWNED has a live entry in
	// zone\create.go's ledger, and the landing reads THAT instead (castLand below);
	// this is the fallback for a body assembled without one, and it is the only number
	// this file can honestly state for such a body.
	targetHP := float64(mon.HP)

	// HALF TWO: THE CLOCK RAN OUT (`castLand`, zone\skill_cast_clock.go). The wind-up
	// has been on the client's screen for the skill's own CastTime, so everything that
	// decides whether this cast may happen is BEHIND us - the range gate, the damage,
	// the walk. Two things are read fresh here and only here: the caster's LIVE
	// position (`pos` above, which the client moves its actor to - GeActor.cpp:~301)
	// and the monster's current position. Nothing is re-validated: the reference's own
	// result is a hit, not a second range check, and a cast that reached this point and
	// then refused would be a wind-up on screen followed by silence.
	if step == castLand {
		// R84 SLICE 2: the game's own damage script DECIDES the number here, once per
		// landed cast, through the one function both damage sites call. `damage` above
		// is the placeholder - what a zone with no script, or a raising one, carries.
		//
		// A BUFF ROW RUNS ITS OWN SCRIPT INSTEAD (this slice): the row's `Script`
		// column is the dispatch (`CFieldChar::ProcessSkill` runs `skill->GetScript()`,
		// FieldChar_Skill.cpp:65-67), and 714 rows name `SCR_SKILL_BUFF` - a script
		// that computes no damage and hands its argument bag to the target's message
		// script (ge\script\skill\buff.scp:143). Running the ATTACK formula for such a
		// row would price a skill that has no attack; its SklATK is 0, so the number
		// below stays 0 and the packet still marks the cast as landed.
		var buffOut [][]byte
		if row.Script == SkillBuffEntry {
			buffOut = h.skillScriptBuff(sess, *c, row, monsterBuffTarget(h, &mon), false, 1)
		} else {
			damage = h.skillScriptDamage(*c, row, &mon, false, 1, damage, sheet)
		}
		// THE HP THIS HIT MOVES, out of the ONE ledger (zone\create.go) and the ONE
		// writer of it - the same call the plain attack and the splash make. `before` is
		// what the 3573's +52 carries and `after` is what the 3557's +6 carries, so the
		// plate the client overwrites and the number that flies cannot disagree, which
		// is the rule §7.4 of history\combat-attack-heal.md states for the attack family.
		//
		// `hpKnown` is false only for a body nothing ever gave an HP - a zone that
		// assembled the monster by hand. Then the 3573 keeps the number the spawn
		// carried and NO 3557 goes out, because a hit whose "before" is invented is a
		// health plate telling a story neither side can maintain (zone\attack.go says
		// the same thing at its own ledger read, and refuses the hit outright).
		before, after, hpKnown := h.damageMonsterBy(mon, damage, c.Handle, int(c.Level))
		meleeHP := targetHP
		if hpKnown {
			meleeHP = before
		}
		melee := BuildSkillMelee(c.Handle, pos, pos, uint16(req.Skill), 0, req.Target,
			damage, attackTypeNormal, meleeHP, nil, targetPos)
		// THE ACTION IS ONE SET OF SLICES, AND IT IS THE SAME SET FOR EVERYONE (R93):
		// the hit and the delta are built ONCE, here, and handed to the caster and to
		// every other session in the zone unchanged - the rule broadcastMove follows for
		// a walk. Without this the observer sees a body standing still while the fight
		// happens, which is exactly what the owner's two clients showed on 2026-10-31.
		//
		// THE DELTA IS IN THE SAME CALL as the hit, not a second one: a watcher must not
		// be able to receive the animation with no number, or a number with no
		// animation, and fanOut hands over the whole set or reports on the smallest
		// count any one of them got.
		out := [][]byte{melee}
		// THE BUFF PACKETS RIDE WITH THE HIT (this slice): 3556 (and the 3125 to the
		// owner of a player buff) are built by the zone's buff rules while the message
		// script ran, and they leave in the same reply batch as the cast's own result.
		out = append(out, buffOut...)
		if hpKnown {
			out = append(out, BuildAddHP(req.Target, after, damage))
		}
		others := h.fanOut(sess, c.Handle, out...)
		// THE ONE LINE THAT PROVES THE CARD: commander, character, skill id AND name,
		// target handle, and the damage written. `%g` and never `%d`: the damage is a
		// float64 on the wire, and an integer format would hide a fraction that is
		// really there (the reference's 15233403.1170533 shows as 15,233,403).
		h.Log.Printf("zone: SKILL_CAST commander %d char %d (%d) skill %d (%q) target %d "+
			"(%q Lv %d, at %d %d cell %d) LANDED from the caster's live position "+
			"(%d %d cell %d) damage %g -> cmd %d (%d B), attack type %d, target HP %g "+
			"-> %d other player(s)",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
			req.Target, h.MonsterName(mon), mon.Level, mon.X, mon.Y, mon.Cell,
			pos[0], pos[1], pos[2], damage, CmdZCSkillMelee, len(melee),
			attackTypeNormal, meleeHP, others)

		// AND WHEN THE DELTA IS ABSENT, the reason is said out loud rather than left as
		// a silence a reader would take for an oversight.
		if !hpKnown {
			h.Log.Printf("zone: SKILL_CAST: no ZC_ADD_HP (3557) for handle %d - this zone "+
				"keeps no HP for that body (nothing gave it one when it was created), so it "+
				"will not send a delta; the 3573 carries HP %g, the value the spawn gave "+
				"this monster", req.Target, meleeHP)
		}
		return out, nil
	}

	// A SECOND PRESS WHILE THIS CHARACTER IS STILL ANIMATING IS DROPPED (R89), and it
	// is asked HERE - before the range gate, before the SP is cut and before a single
	// byte is built - so a dropped press sends NOTHING and costs nothing. The cast it
	// arrived under is left alone: the reference clears such a press on its own
	// RefreshTime barrier (zone\skill_cast_clock.go, dropPressWhileAnimating).
	if step == castPress && h.dropPressWhileAnimating(sess, req.CharIndex, req.Skill, "SKILL") {
		return nil, nil
	}

	// THE RANGE CHECK (R83/R84/R87) - THE SAME FUNCTION THE GROUND CAST USES
	// (skillRangeAllowed, zone\skill_area.go), so the two ways of casting cannot
	// disagree about what "in range" means.
	//
	// R87 REPLACES WHAT R84 DID WITH THE ANSWER. The reference walks the character in
	// and casts from the stand point it chose (captures\rangeapproach20261031: a
	// reason-2 ZC_MOVE_PC, then a 3573 whose beginPos is exactly that point), so a
	// TOO-FAR target is neither refused nor cast across the map: it starts the
	// approach walk and the cast fires when the walk ends (zone\skill_approach.go).
	// `-strictrange` still refuses, unchanged; a point TOO CLOSE keeps R84's answer,
	// because walking backwards to cast is not a behaviour any capture shows.
	dist, inRange := skillRangeAllowed(pos, targetPos, row)
	if label, tail, refuse := h.rangeGate(!inRange, row.MaxR > 0 && dist > row.MaxR); label != "" {
		h.Log.Printf("zone: SKILL %s: commander %d char %d (%d) skill %d (%q) "+
			"target %d at (%d %d cell %d) is %g away from the caster at (%d %d cell "+
			"%d), and the skill's range is MinR %g .. MaxR %g %s",
			label,
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill,
			h.SkillName(req.Skill), req.Target, mon.X, mon.Y, mon.Cell, dist,
			pos[0], pos[1], pos[2], row.MinR, row.MaxR, tail)
		if refuse {
			return nil, nil
		}
		if step.resumed() {
			// The walk is over and the target is STILL out of it: a row that changed
			// under us, or a target that moved away. Saying so once is the honest end;
			// starting a second walk here would loop for ever.
			h.Log.Printf("zone: SKILL APPROACH: after the walk the target is still %g "+
				"away and the range is MinR %g .. MaxR %g - nothing sent",
				dist, row.MinR, row.MaxR)
			return nil, nil
		}
		if row.MaxR > 0 && dist > row.MaxR {
			return h.approachCast(sess, req.CharIndex, pendingCast{
				frame: f, skill: req.Skill, target: req.Target,
				pos: targetPos, dist: dist,
			}, row, "SKILL"), nil
		}
	}

	// HALF ONE: THE WIND-UP, AND THE CLOCK IT STARTS. The 3571 goes out NOW - it is the
	// packet that starts the client's animation - and NOTHING else does: the result
	// waits for the skill's own CastTime (zone\skill_cast_clock.go), and the body is
	// then held for the row's own HoldTime. Both positions are the stand point as the
	// body is at it now.
	clock := h.startCast(sess, req.CharIndex, f, false, req.Skill, req.Target, row)
	if clock == nil {
		h.Log.Printf("zone: SKILL refused: char %d is not a character this team has - "+
			"nothing sent", req.CharIndex)
		return nil, nil
	}

	// AN INSTANT SKILL TAKES THE OTHER ROAD (`FieldChar_FSM.cpp:363-367`): the
	// reference skips the cast state when `GetSkillCastingTime()` is 0, so no 3571 is
	// sent and the result goes NOW. The client agrees: its cast state divides by the
	// packet's `time` (`GeActor.cpp:2206`), so a 3571 with 0 would be a divide by
	// zero. The HOLD still applies - that is the whole point of HoldTime.
	if clock.length <= 0 {
		h.Log.Printf("zone: SKILL_CAST commander %d char %d (%d) skill %d (%q) target %d "+
			"(%q Lv %d, at %d %d cell %d) %g away (MinR %g MaxR %g) damage %g - CastTime "+
			"0, so NO cmd %d: the result goes NOW and the body is then held %s",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
			req.Target, h.MonsterName(mon), mon.Level, mon.X, mon.Y, mon.Cell,
			dist, row.MinR, row.MaxR, damage, CmdZCSkillCasting,
			clock.hold.Round(time.Millisecond))
		return h.landCast(sess, clock), nil
	}

	casting := BuildSkillCasting(c.Handle, pos, pos, uint16(req.Skill), uint32(row.CastTime),
		req.Target, targetPos)

	// THE WIND-UP IS A WATCHED ANIMATION, NOT A PRIVATE ONE (R93). The 3571 is the
	// packet the CLIENT starts the cast animation and the purple bar from, so without
	// it on the observer's wire that character simply stands there for the whole
	// wind-up and something invisible happens at the end of it.
	others := h.fanOut(sess, c.Handle, casting)

	card := "zone: SKILL_CAST"
	if step == castAfterWalk {
		card = "zone: SKILL_CAST (after the approach walk)"
	}
	h.Log.Printf("%s commander %d char %d (%d) skill %d (%q) target %d (%q Lv %d, "+
		"at %d %d cell %d) %g away (MinR %g MaxR %g) damage %g -> cmd %d (%d B) NOW, "+
		"its result in %s, then its own hold %s; a move in either window is QUEUED and "+
		"does NOT cancel it -> %d other player(s)",
		card, sess.CommanderID, req.CharIndex, c.Handle, req.Skill,
		h.SkillName(req.Skill), req.Target, h.MonsterName(mon), mon.Level,
		mon.X, mon.Y, mon.Cell, dist, row.MinR, row.MaxR, damage,
		CmdZCSkillCasting, len(casting), clock.length.Round(time.Millisecond),
		clock.hold.Round(time.Millisecond), others)

	return [][]byte{casting}, nil
}

// SkillName is the display name of a skill, or a phrase that says the table does
// not hold it - a log line must never end in an empty set of quotes.
func (h *Handler) SkillName(id uint32) string {
	if h.Skills == nil {
		return "(no skill table)"
	}
	return h.Skills.Name(id)
}

// MonsterName is the Monster row's name for a monster, so a log line can name the
// thing that was hit. It falls back to the class id: an unknown row is still a fact
// worth printing.
func (h *Handler) MonsterName(m Monster) string {
	if h.Monsters != nil {
		if row, ok := h.Monsters.Row(m.ClassID); ok && row.Name != "" {
			return row.Name
		}
	}
	return "class " + strconv.Itoa(int(m.ClassID))
}
