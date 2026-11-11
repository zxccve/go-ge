package zone

import (
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"geserver/protocol"
)

// THE GROUND / AREA CAST (R85 + R83): CZ_SKILL_GROUND 3647 in, and out:
//
//	3654 ZC_UPDATE_SP + 3571 ZC_SKILL_CASTING   immediately, at the press
//	3581 ZC_SKILL_AREA + 3557 ZC_ADD_HP xN      later, when the cast's clock fires
//
// THE SPLIT IS MEASURED, not a design taste. On the live server the 3571 and the
// 3581 are 941 ms apart and the player's move clicks fall between them, are echoed,
// and cancel nothing (captures\cancel20261031\20261031-191902, read in
// history\cancel-and-animation.md §2/§3). The clock itself is in
// zone\skill_cast_clock.go; this file builds both halves, from the same request.
//
// WHY THIS EXISTS. The owner aimed the Warlock's three skills - Fireball, Burning
// Hands, Flame Ground - at a point rather than at a monster, and the client sent
// CZ_SKILL_GROUND (3647) for every one of them, never CZ_SKILL_TARGET (3646). Until
// this file the zone answered only 3646/3645 (`zone\skill_cast.go`), so a Fireball
// at the ground fell into the "not handled yet" branch and nothing happened on
// screen: no cast bar, no ball, no number.
//
// EVERY OFFSET IN THIS FILE IS MEASURED, NOT DERIVED. The source is
// `history\combat.md` §10 - the owner's own `captures\areacast20261031\20261031-173749`
// recording, five ground casts in one session - plus §9 for the packets that lane
// already measured. The fixtures every claim below is checked against:
//
//	zone\testdata\live_cz_skill_ground.bin        27 B  CZ_SKILL_GROUND 3647
//	zone\testdata\live_skill_casting_area.bin     60 B  the Fireball's 3571 (+44 = 0)
//	zone\testdata\live_skill_area_fireball.bin   344 B  the Fireball's 3581, count 1
//	zone\testdata\live_add_hp_damage.bin          23 B  the 3557 that carries the damage
//
// ONE FAMILY, AND ONLY ONE: UseType="AREA" + ValueType="Attack" (Fireball 20211).
// The other two Warlock skills are deliberately NOT built and are refused with one
// line each that names the family - Burning Hands 20212 is AREA+Buff and Flame
// Ground 20215 is PLACEMENT, and each needs packets (3556/3125, 3526/3572/3582/3566/
// 3586/3531) that this lane does not have. A half-answer there would draw a buff
// icon nothing granted or a ground effect that never ticks.
//
// WHAT THIS FILE DELIBERATELY DOES NOT SEND, so a reader does not look for it:
//
//   - No 3556 ZC_CHANGE_BUFF, no 3803 ZC_UPDATE_MHP, no 3367 ZC_SHOW_DIGIT. §10.11
//     marks all three optional and calls 3367's fields unsettled; the damage number
//     flies from the 3557 and the list of who was hit is the 3581, which is the
//     whole of what the owner can see.
//   - THE OTHER PLAYERS ARE TOLD, AND THIS LANE USED TO SAY THEY WERE NOT (R93).
//     The 3571 goes out at the press and the 3581 with its one 3557 per victim at the
//     clock, each built ONCE and handed to every other session in the zone unchanged
//     - the same rule broadcastMove follows for a walk. Only the 3654 STAYS HERE: it
//     reports the CASTER's own SP, a number only the caster's HUD draws.
//     THE REASON IS THE OWNER'S OWN TWO CLIENTS, and it reverses what this file said
//     until R93: "no capture shows a second observer's screen during a cast ... a
//     missing one is not [visible]". He watched a second character cast at a
//     Scarecrow and the observer saw a body standing still through the whole
//     animation ("animation not showing on the observer ... only action such as
//     normal attack or skill"), so the captures were not the last word: a missing
//     broadcast IS a visible bug on somebody else's screen.
//     WHO IS TOLD IS sendOthers' DECISION, NOT THIS FILE'S: the fan-out shares the
//     movement one, so the R135e update cull (a viewer whose body is past the map's
//     ViewMiddleRange) suppresses a blast exactly as it suppresses a walk. No packet
//     is rebuilt per recipient - one map, one process, and the radius is the map
//     row's own number (ROADMAP R91/R93/R135e).
//   - NO DROP IS ADDED BY THIS FILE, and no kill is DECIDED here: the victims' HP now
//     comes out of the ONE ledger (zone\create.go, damageMonster), the same call the
//     plain attack and the aimed cast make, so a body this blast EMPTIES is killed by
//     that function's own rule - the ZC_DEAD is armed there and EXP is paid there
//     (R98), alike for every damage site. §10.9 measured "no EXP" in the reference
//     because nothing died in that recording; monster drops remain another lane's.

const (
	// CmdCZSkillGround is CZ_SKILL_GROUND, client -> server. The client's own table
	// says 27 bytes (`data\cmd_table_live_z.txt:1314` "3647 27 CZ_SKILL_GROUND").
	CmdCZSkillGround = 3647

	// CZSkillGroundSize is the PAYLOAD. A frame can be longer than this: the client
	// pads its plaintext up to a whole 8-byte Blowfish block, so a 27-byte packet
	// arrives as 32 bytes and the five bytes after +26 are leftover buffer, not data
	// (§10.1: all five live casts carried DIFFERENT garbage there and nothing in the
	// 1,315 replies echoed it). Read the packet, ignore the rest - never assume the
	// frame is 27 and never refuse a longer one.
	CZSkillGroundSize = 27

	// CmdZCSkillArea is ZC_SKILL_AREA, server -> client, VARIABLE size: a 52-byte
	// header and then one 292-byte record per target. The length word at +2 is the
	// WHOLE packet (§10.4 measured 52, 344 and 928 in one recording).
	CmdZCSkillArea = 3581

	// SkillAreaHeaderSize is everything before the first record: 2 + 2 + 4 + 12 + 12
	// + 12 + 2 + 2 + 2 + 2 = 52.
	SkillAreaHeaderSize = 52

	// SkillAreaRecordSize is one target: handle 4 + damage 8 + attack type 4 + HP 8 +
	// the 256-byte buff mask + position 12 = 292. Corroborated twice: 52 + 292 = 344
	// is the one-target packet and 52 + 3x292 = 928 is the three-target one.
	SkillAreaRecordSize = 292

	// CmdZCAddHP is ZC_ADD_HP, 23 bytes (`data\cmd_table_live_z.txt:1224`).
	CmdZCAddHP = 3557

	// AddHPSize is 2 + 4 + 8 + 8 + 1.
	AddHPSize = 23

	// CmdZCUpdateSP is ZC_UPDATE_SP, 10 bytes (`data\cmd_table_live_z.txt:1321`).
	CmdZCUpdateSP = 3654

	// UpdateSPSize is 2 + 4 + 4: cmd, handle, the value.
	UpdateSPSize = 10

	// 3647's fields. +10 and +11 are the SAME two the 3645/3646 senders use
	// (czCharIndex, czSkillID in zone\skill_cast.go); the difference is that the
	// target dword becomes a WORLDPOS.
	cgPosX = 15 // int32 x
	cgPosY = 19 // int32 y
	cgCell = 23 // int32 navmesh CELL - never a height (§0, R73)

	// 3581's header fields.
	saCaster  = 4  // DWORD the CASTER's handle
	saBegin   = 8  // WORLDPOS beginPos - the caster's own position
	saEnd     = 20 // WORLDPOS endPos   - the same in all three live samples
	saPos     = 32 // WORLDPOS the point the client sent in its 3647
	saSkill   = 44 // WORD skill id
	saCount   = 50 // WORD how many 292-byte records follow
	saRecords = 52 // the first record

	// 3581's record fields, relative to the record.
	arHandle     = 0   // DWORD the target's handle
	arDamage     = 4   // FLOAT64 the damage - the same number the 3557 carries
	arAttackType = 12  // DWORD 2 in the live sample (the caster's knockdown decision)
	arHP         = 16  // FLOAT64 the target's HP
	arMask       = 24  // BYTE[256] buff mask, copied wholesale by the client
	arMaskLen    = 256 // its width
	arPos        = 280 // WORLDPOS the target's position

	// 3557's fields.
	ahHandle = 2  // DWORD
	ahNowHP  = 6  // FLOAT64 the HP after the delta
	ahAddHP  = 14 // FLOAT64 the delta itself: +damage for a hit
	ahFlag   = 22 // BYTE - 1 when the delta is negative, 0 when it is positive

	// 3654's fields. Named us* rather than sp*: zone\players.go already owns that
	// prefix for the 3520 spawn record's own offsets, and two meanings for one name
	// is how an offset gets read from the wrong packet.
	usHandle = 2 // DWORD
	usValue  = 6 // DWORD the character's SP after the skill's SpendSP

	// attackTypeKnockdown is the value §10.4 measured in the live record's +12.
	// It is the REFERENCE SERVER's decision (Fireball's row has KDownType="leftspin"),
	// so this zone does not copy it: the live path sends attackTypeNormal, exactly as
	// the aimed cast does. It exists so the re-serialise test can pass the capture's
	// own value in and prove every OTHER byte of the record.
	attackTypeKnockdown = 2

	// maxAreaTargets is the hard ceiling the length WORD imposes, not a game rule:
	// (65535 - 52) / 292 = 224, and a 225th record would make +2 wrap and cut the
	// packet in half on the reader's side.
	maxAreaTargets = (65535 - SkillAreaHeaderSize) / SkillAreaRecordSize
)

// GroundRequest is one decoded CZ_SKILL_GROUND.
type GroundRequest struct {
	// CharIndex is the client's team slot - 2 in all five live casts (the Warlock is
	// the third character), the same field 3646 and 3625 carry.
	CharIndex int
	// Skill is the skill CLASS ID the client pressed (20211 Fireball here). Never
	// trusted: the handler checks it against the stance the character stands in.
	Skill uint32
	// Pos is WORLDPOS: x, y and the navmesh CELL the client computed. The cell is
	// copied, never recomputed (§10.1: y 1355 -> cell 50, 783 and 610 -> cell 49,
	// 1363 -> 50).
	Pos [3]int32
}

// ParseSkillGround reads CZ_SKILL_GROUND (3647):
//
//	+0 cmd(2)=3647  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 skill(4)
//	+15 int32 x     +19 int32 y  +23 int32 navmesh cell
//
// ok is false only when the frame is SHORTER than those 27 bytes: the fields are
// fixed offsets, so a short frame cannot hold them. A LONGER frame is accepted - the
// client's cipher pads to a multiple of 8, and the bytes past +26 are leftover
// buffer, not data (§10.1). The caller logs the extra length rather than refusing
// it, which is the difference between reading the packet and guessing its size.
func ParseSkillGround(pt []byte) (GroundRequest, bool) {
	var req GroundRequest
	if len(pt) < CZSkillGroundSize {
		return req, false
	}
	req.CharIndex = int(pt[czCharIndex])
	req.Skill = binary.LittleEndian.Uint32(pt[czSkillID:])
	req.Pos = [3]int32{
		int32(binary.LittleEndian.Uint32(pt[cgPosX:])),
		int32(binary.LittleEndian.Uint32(pt[cgPosY:])),
		int32(binary.LittleEndian.Uint32(pt[cgCell:])),
	}
	return req, true
}

// AreaTarget is one record of ZC_SKILL_AREA: one body the skill reached.
type AreaTarget struct {
	Handle     uint32
	Damage     float64 // the number that flies - the 3557 carries the same value
	AttackType int32   // attackTypeNormal on our side; see attackTypeKnockdown
	HP         float64 // the target's HP after the hit
	BuffMask   []byte  // 256 bytes, copied wholesale; nil means "all zero"
	Pos        [3]int32
}

// BuildSkillArea packs one ZC_SKILL_AREA (3581): the 52-byte header and one
// 292-byte record per target.
//
//	+0  WORD      3581
//	+2  WORD      the WHOLE packet's length
//	+4  DWORD     the caster's handle
//	+8  WORLDPOS  beginPos      the caster's own position
//	+20 WORLDPOS  endPos        the same (the caster stands still while casting)
//	+32 WORLDPOS  pos           *** the point the client sent in its 3647 ***
//	+44 WORD      skill id
//	+46 WORD      0             unnamed, 0 in all three live samples
//	+48 WORD      0             unnamed, 0 in all three live samples
//	+50 WORD      count
//	+52 count x 292-byte records, in the order they are given
//
// The count is a LIST, so ONE packet carries N targets (§10.4 proved N=3 on the buff
// cast; two DAMAGED targets in one packet is what the two-dummy test in this lane
// checks).
func BuildSkillArea(caster uint32, begin, end, pos [3]int32, skill uint16, targets []AreaTarget) []byte {
	if len(targets) > maxAreaTargets {
		// The length WORD cannot express it and a wrapped length cuts the packet in
		// half on the client's side. Truncation is the only honest answer here; the
		// caller caps by the skill's own SplLimit long before this.
		targets = targets[:maxAreaTargets]
	}
	out := make([]byte, SkillAreaHeaderSize+SkillAreaRecordSize*len(targets))
	binary.LittleEndian.PutUint16(out[0:], CmdZCSkillArea)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	binary.LittleEndian.PutUint32(out[saCaster:], caster)
	putWorldPos(out[saBegin:], begin)
	putWorldPos(out[saEnd:], end)
	putWorldPos(out[saPos:], pos)
	binary.LittleEndian.PutUint16(out[saSkill:], skill)
	// +46 and +48 are left zero: the capture has 0 there and nothing names them.
	binary.LittleEndian.PutUint16(out[saCount:], uint16(len(targets)))
	for i, t := range targets {
		r := out[saRecords+i*SkillAreaRecordSize:]
		binary.LittleEndian.PutUint32(r[arHandle:], t.Handle)
		binary.LittleEndian.PutUint64(r[arDamage:], math.Float64bits(t.Damage))
		binary.LittleEndian.PutUint32(r[arAttackType:], uint32(t.AttackType))
		binary.LittleEndian.PutUint64(r[arHP:], math.Float64bits(t.HP))
		copy(r[arMask:arMask+arMaskLen], t.BuffMask) // short or nil: the rest stays 0
		putWorldPos(r[arPos:], t.Pos)
	}
	return out
}

// BuildAddHP packs one ZC_ADD_HP (3557): the packet that makes a number FLY.
//
//	+0  WORD      3557
//	+2  DWORD     handle
//	+6  FLOAT64   nowHP   the HP after the delta
//	+14 FLOAT64   addHP   the delta: +damage for a hit, negative for damage taken
//	+22 BYTE      1 when addHP < 0, 0 when it is positive
//
// THE FLAG BYTE IS MEASURED, not chosen: over 81 live 3557s, +22 is 0 on all 61
// positive deltas and 1 on all 20 negative ones (§10.10), so it is computed as
// `addHP < 0` rather than copied. The damage reaches the client TWICE - the 3581
// record's double at +4 and this delta - and THIS is the half the client turns into
// the flying number (§10.11): a 3581 alone draws no number.
func BuildAddHP(handle uint32, nowHP, addHP float64) []byte {
	out := make([]byte, AddHPSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCAddHP)
	binary.LittleEndian.PutUint32(out[ahHandle:], handle)
	binary.LittleEndian.PutUint64(out[ahNowHP:], math.Float64bits(nowHP))
	binary.LittleEndian.PutUint64(out[ahAddHP:], math.Float64bits(addHP))
	if addHP < 0 {
		out[ahFlag] = 1
	}
	return out
}

// BuildUpdateSP packs one ZC_UPDATE_SP (3654): cmd(2), handle(4), the value(4).
//
// The reference sends it IMMEDIATELY BEFORE the 3571, with the caster's SP already
// cut by the skill's own SpendSP (§10.9: 669->609, 677->587, 655->475, 645->465
// against the table's 60/90/180 - four of four).
func BuildUpdateSP(handle uint32, sp uint32) []byte {
	out := make([]byte, UpdateSPSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUpdateSP)
	binary.LittleEndian.PutUint32(out[usHandle:], handle)
	binary.LittleEndian.PutUint32(out[usValue:], sp)
	return out
}

// skillRangeAllowed is THE RANGE CHECK (R83), and it is ONE function so the aimed
// cast (3646, zone\skill_cast.go) and the ground cast (3647, this file) cannot
// disagree about what "in range" means.
//
//	dist = sqrt((x1-x2)^2 + (y1-y2)^2)     X and Y only, no height - the same rule
//	                                       the shipped server's own circle checker
//	                                       uses (zone\near.go distSq, Common.cpp:47-53)
//	ok   = dist <= MaxR  AND  dist >= MinR
//
// Both bounds come from the skill's OWN row in `datatable_skill.xml`, never from a
// number in this file: Fireball 20211 is MinR 150 / MaxR 800, Rapid Shot 10561 is
// MinR 1 / MaxR 1200. A MaxR of 0 means the row states no maximum, so only MinR
// applies - a row with neither allows any distance, which is the honest reading of
// "the table says nothing".
//
// The distance is in the map's own units, which are centimetres (zone\near.go
// RadiusUnitsPerMetre): 800 is 8 metres, roughly two character widths on screen.
//
// R84: this function only MEASURES. Whether an out-of-range answer is allowed to
// refuse a cast is Handler.rangeGate's call, and off by default.
func skillRangeAllowed(from, to [3]int32, row SkillRow) (dist float64, ok bool) {
	dist = math.Sqrt(distSq(from[0], from[1], to[0], to[1]))
	if row.MaxR > 0 && dist > row.MaxR {
		return dist, false
	}
	if dist < row.MinR {
		return dist, false
	}
	return dist, true
}

// rangeGate is WHAT AN OUT-OF-RANGE ANSWER MEANS, in one place, so the aimed cast
// and the point cast cannot disagree about whether the check blocks.
//
// It does NOT decide the arithmetic - skillRangeAllowed does, and it is right. It
// decides what the arithmetic is allowed to do, and since R87 that is measured:
//
//	too FAR          the character WALKS into range and the cast fires when it arrives
//	                 (zone\skill_approach.go) - the reference's own behaviour, read out
//	                 of captures\rangeapproach20261031
//	too CLOSE        the cast is applied from where the character stands. No capture
//	                 shows a character backing away to cast, so nothing is invented;
//	                 this is R84's answer, unchanged.
//	-strictrange ON  refuses both, also unchanged (a development switch).
//
// The return is:
//
//	label  the word the log line carries: "refused" or "OUT OF RANGE"
//	tail   the phrase that ends the line: what happens next, said out loud
//	refuse true ONLY when Handler.StrictRange (-strictrange) is ON
//
// label is "" when the cast is in range, which is the caller's cue to log nothing.
func (h *Handler) rangeGate(outOfRange, tooFar bool) (label, tail string, refuse bool) {
	if !outOfRange {
		return "", "", false
	}
	if h.StrictRange {
		return "refused", "- nothing sent", true
	}
	if tooFar {
		return "OUT OF RANGE", "- the character walks into range and the cast fires " +
			"when the walk ends (R87, zone\\skill_approach.go)", false
	}
	return "OUT OF RANGE", "- the cast is applied from where the character stands: " +
		"being too CLOSE is not walked, because no capture shows a character backing " +
		"off to cast", false
}

// castSkillGround answers CZ_SKILL_GROUND (3647): a skill aimed at a POINT.
//
// THE REFUSALS COME FIRST, EACH WITH ONE LINE AND NOTHING SENT, in this order: a
// frame shorter than the 27-byte payload, a character index that is not this team, a
// zone with no stance table, a skill the character's CURRENT stance does not offer,
// a zone with no skill table, a skill the table does not hold, and a skill whose family
// is not AREA+Attack. A refused cast sends NOTHING: the client keeps its own cooldown
// and its own animation, and a 3581 for a cast this server never validated would be a
// hit neither side holds.
//
// R87: a point FARTHER than the skill's MaxR is a WALK, not a refusal and not a cast
// from where the character stands - the walk ends in the in-range answer below
// (zone\skill_approach.go). A point too CLOSE keeps R84's answer, and -strictrange
// still refuses both.
//
// THE ANSWER, in the order §10.2 measured for a damaged area cast:
//
//	3654 ZC_UPDATE_SP   the caster's SP after SpendSP   (10 B)
//	3571 ZC_SKILL_CASTING  60 B, +44 = 0 (a point, not an object), +48 = the point
//	3581 ZC_SKILL_AREA     52 B + one 292-byte record per victim
//	3557 ZC_ADD_HP         23 B per damaged victim - the number that flies
func (h *Handler) castSkillGround(sess *Session, f protocol.Frame) ([][]byte, error) {
	return h.castSkillGroundAt(sess, f, castPress)
}

// castSkillGroundAt is castSkillGround with the bit that says WHICH HALF of the cast
// this call produces: the client's own frame (castPress), the end of the approach
// walk (castAfterWalk), or the pending clock running out (castLand). See
// zone\skill_cast_clock.go.
func (h *Handler) castSkillGroundAt(sess *Session, f protocol.Frame, step castStep) ([][]byte, error) {
	req, ok := ParseSkillGround(f.Data)
	if !ok {
		h.Log.Printf("zone: SKILL_GROUND refused: %d-byte frame, CZ_SKILL_GROUND 3647 "+
			"carries fields up to +23 and needs %d bytes - nothing sent",
			len(f.Data), CZSkillGroundSize)
		return nil, nil
	}
	// A LONGER frame is normal and is not a refusal: the cipher pads the client's
	// plaintext to a whole 8-byte block. §10.1 measured five different 5-byte tails
	// after +26 and nothing in 1,315 replies echoing them, so the tail is read and
	// dropped, and said out loud once per padded frame.
	if len(f.Data) > CZSkillGroundSize {
		h.Log.Printf("zone: SKILL_GROUND: %d-byte frame, the packet is %d bytes and "+
			"+%d..+%d is cipher padding (§10.1: leftover buffer, different in all five "+
			"live casts) - read and ignored",
			len(f.Data), CZSkillGroundSize, CZSkillGroundSize, len(f.Data)-1)
	}

	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d character index %d is "+
			"not in this team (%d member(s) spawned) - nothing sent",
			sess.CommanderID, req.CharIndex, len(sess.Team))
		return nil, nil
	}
	c := &sess.Team[req.CharIndex]

	if h.Stances == nil {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d char %d (%d) skill %d - "+
			"this zone has no stance table (Handler.Stances is nil), so it cannot say "+
			"which skills the character's stance offers - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}
	// THE SAME PERMISSION RULE AS THE AIMED CAST: the skill must be one the
	// character's CURRENT stance offers (datatable_stance.xml SkillID1..5), which is
	// exactly the list the client's own bar was built from.
	offered := h.Stances.StanceSkills(c.Stance)
	if !intIn(offered, int(req.Skill)) {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d char %d (%d) skill %d "+
			"(%q) is not one its current stance %d (%q) offers %v - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill,
			h.SkillName(req.Skill), c.Stance, h.Stances.StanceName(c.Stance), offered)
		return nil, nil
	}

	if h.Skills == nil {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d char %d (%d) skill %d - "+
			"this zone has no skill table (Handler.Skills is nil), so it cannot name "+
			"the skill, price its damage or read its range - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}
	row, known := h.Skills.Row(req.Skill)
	if !known {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d char %d (%d) skill %d is "+
			"not a row in datatable_skill.xml, so its family, its range and its damage "+
			"factor are all unknown - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill)
		return nil, nil
	}

	// THE FAMILY GATE. This lane builds AREA + Attack and nothing else, and it says
	// so in ONE line rather than answering half a family (see the file header).
	if !row.isAreaAttack() {
		h.Log.Printf("zone: SKILL_GROUND refused: commander %d char %d (%d) skill %d "+
			"(%q) is UseType %q ValueType %q - the %s family is not built yet (this "+
			"lane answers UseType AREA + ValueType Attack only) - nothing sent",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
			row.UseType, row.ValueType, row.familyWord())
		return nil, nil
	}

	// THE RANGE CHECK (R83/R84/R87), shared with the aimed cast. The point is the
	// target. R87: a point FARTHER than MaxR starts the approach walk and the cast
	// fires from the stand point (zone\skill_approach.go); a point too CLOSE keeps
	// R84's answer; -strictrange turns both back into a refusal.
	pos := casterPos(sess, req.CharIndex, time.Now())

	// HALF TWO: THE CLOCK RAN OUT (`castLand`, zone\skill_cast_clock.go). The 3654 and
	// the 3571 are already on the client's screen, so the walk, the range gate and the
	// SP cut are all behind us. What is built HERE is the blast, from the world as it
	// is now: the victims' current positions, the damage the sheet gives now, and the
	// caster's LIVE position (`pos` above - the client moves its actor to it).
	if step == castLand {
		return h.landGround(sess, req, c, row, pos), nil
	}

	// A SECOND PRESS WHILE THIS CHARACTER IS STILL ANIMATING IS DROPPED (R89) - asked
	// before the range gate and before the SP is cut, so a dropped press sends nothing
	// and spends nothing (zone\skill_cast_clock.go, dropPressWhileAnimating).
	if step == castPress &&
		h.dropPressWhileAnimating(sess, req.CharIndex, req.Skill, "SKILL_GROUND") {
		return nil, nil
	}

	dist, inRange := skillRangeAllowed(pos, req.Pos, row)
	if label, tail, refuse := h.rangeGate(!inRange, row.MaxR > 0 && dist > row.MaxR); label != "" {
		h.Log.Printf("zone: SKILL_GROUND %s: commander %d char %d (%d) skill %d "+
			"(%q) point (%d %d cell %d) is %g away from the caster at (%d %d cell %d), "+
			"and the skill's range is MinR %g .. MaxR %g %s",
			label,
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
			req.Pos[0], req.Pos[1], req.Pos[2], dist, pos[0], pos[1], pos[2],
			row.MinR, row.MaxR, tail)
		if refuse {
			return nil, nil
		}
		if step.resumed() {
			h.Log.Printf("zone: SKILL_GROUND APPROACH: after the walk the point is still "+
				"%g away and the range is MinR %g .. MaxR %g - nothing sent",
				dist, row.MinR, row.MaxR)
			return nil, nil
		}
		if row.MaxR > 0 && dist > row.MaxR {
			return h.approachCast(sess, req.CharIndex, pendingCast{
				frame: f, ground: true, skill: req.Skill,
				pos: req.Pos, dist: dist,
			}, row, "SKILL_GROUND"), nil
		}
	}

	// THE SP, cut by the skill's own SpendSP and never below zero: the reference's
	// 3654 lands immediately before the 3571 with the reduced value (§10.9). It is cut
	// ONCE, here, at the press - not again when the clock fires.
	sp := int(c.SP)
	if row.SpendSP > 0 {
		if row.SpendSP >= sp {
			sp = 0
		} else {
			sp -= row.SpendSP
		}
	}
	// THE LOCK (`-splock`, a DEVELOPMENT FLAG, off in every normal run): the cut above
	// is made exactly as the reference makes it, then the bar is put back, so the 3654
	// tells the client its gauge is full and the next press is not refused on the
	// CLIENT's side for want of SP. Nothing else changes - same skill, same clock, same
	// damage, and the cost is still on the record in this line. See Handler.SPLock.
	if h.SPLock && row.SpendSP > 0 && c.MaxSP > 0 {
		sp = int(c.MaxSP)
		h.Log.Printf("zone: SPLOCK char %d (%q) cast %d (%q) spent %d - the bar is back at "+
			"%d (development switch -splock: no game rule reads this)",
			sess.CommanderID, c.Name, req.Skill, h.SkillName(req.Skill), row.SpendSP, c.MaxSP)
	}
	c.SP = uint16(sp)

	updateSP := BuildUpdateSP(c.Handle, uint32(sp))
	// +44 target handle is 0 for a ground cast and +48 is the client's own point:
	// that is the one field that separates "aimed at an object" from "aimed at a
	// point" (§10.3, 5 of 5 zero here, 565826 in §9's aimed cast).
	casting := BuildSkillCasting(c.Handle, pos, pos, uint16(req.Skill), uint32(row.CastTime), 0, req.Pos)

	// AND THE CLOCKS: the blast itself waits for the skill's own CastTime, and the
	// body is then held for its own HoldTime (zone\skill_cast_clock.go). The
	// reference sends the 3581 941 ms after the 3571 and holds the caster's own move
	// for the whole window instead of echoing it
	// (history\cancel-and-animation.md §6).
	cc := h.startCast(sess, req.CharIndex, f, true, req.Skill, 0, row)
	if cc == nil {
		// The SP was cut above on the strength of a cast this team cannot have; put
		// nothing on the wire and say so. Nothing else can reach here (the index was
		// already checked), so this is a guard, not a path.
		h.Log.Printf("zone: SKILL_GROUND refused: char %d is not a character this team "+
			"has - nothing sent", req.CharIndex)
		return nil, nil
	}

	// AN INSTANT GROUND SKILL: no 3571 (FieldChar_FSM.cpp:363-367), the blast NOW,
	// and the body held for the row's own HoldTime. The 3654 still goes first, in the
	// same reply, because the SP was cut at the press.
	if cc.length <= 0 {
		h.Log.Printf("zone: SKILL_GROUND commander %d char %d (%d) skill %d (%q, "+
			"UseType %q ValueType %q, SpendSP %d) point (%d %d cell %d) %g from the "+
			"caster (MinR %g MaxR %g), splash %g capped %d | cmd %d (%d B, sp %d) NOW, "+
			"CastTime 0 so NO cmd %d: the blast goes NOW and the body is then held %s",
			sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
			row.UseType, row.ValueType, row.SpendSP, req.Pos[0], req.Pos[1], req.Pos[2],
			dist, row.MinR, row.MaxR, row.SplashRange, row.SplLimit,
			CmdZCUpdateSP, len(updateSP), sp, CmdZCSkillCasting,
			cc.hold.Round(time.Millisecond))
		out := [][]byte{updateSP}
		return append(out, h.landCast(sess, cc)...), nil
	}

	// THE WIND-UP IS A WATCHED ANIMATION, NOT A PRIVATE ONE (R93). The 3654 above is
	// the caster's own SP gauge and stays here; the 3571 is the packet the CLIENT
	// starts the casting animation from, so the observer must get exactly these bytes
	// or it sees a body standing still while a blast it cannot see charges up.
	others := h.fanOut(sess, c.Handle, casting)

	// THE ONE LINE THAT PROVES THE CARD FOR THIS HALF: who cast, which skill by id
	// and name, the point, how far it was against the row's own range, what it cost,
	// and the two clocks the blast now waits for. `%g` and never `%d` for the
	// distance: it is a float64 and an integer format would hide a fraction that is
	// really there.
	h.Log.Printf("zone: SKILL_GROUND commander %d char %d (%d) skill %d (%q, UseType "+
		"%q ValueType %q, SpendSP %d) point (%d %d cell %d) %g from the caster "+
		"(MinR %g MaxR %g), splash %g capped %d | cmd %d (%d B, sp %d) + cmd %d "+
		"(%d B) NOW, the blast in %s, then its own hold %s - a move in either window "+
		"is QUEUED and does NOT cancel it; the animation reaches %d other player(s), "+
		"the SP stays the caster's own",
		sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
		row.UseType, row.ValueType, row.SpendSP, req.Pos[0], req.Pos[1], req.Pos[2],
		dist, row.MinR, row.MaxR, row.SplashRange, row.SplLimit,
		CmdZCUpdateSP, len(updateSP), sp, CmdZCSkillCasting, len(casting),
		cc.length.Round(time.Millisecond), cc.hold.Round(time.Millisecond), others)

	return [][]byte{updateSP, casting}, nil
}

// landGround builds the BLAST of a ground cast whose clock has run out: the 3581
// ZC_SKILL_AREA and one 3557 ZC_ADD_HP per damaged victim, in that order
// (§10.2 measured 3581 then 3557).
//
// It is the second half of castSkillGroundAt, split out for one reason: the packets
// are built from the world AS IT IS when the clock fires - the victims' current
// positions, the caster's LIVE position - and not from the moment the player pressed
// the key. The reference does the same: its own result fired from where the body had
// walked to during the wind-up.
//
// The SP is NOT touched here. `c.SP` was already cut at the press and the 3654
// already went out with it; a second cut would charge twice and a second 3654 would
// redraw a number that did not move.
func (h *Handler) landGround(sess *Session, req GroundRequest, c *Character,
	row SkillRow, pos [3]int32) [][]byte {

	// WHO IS IN THE AREA. The victims are the monsters THIS ZONE is running inside
	// the skill's own SplashRange of the point, capped by the skill's own SplLimit
	// (Fireball 5). SplashRange 0 is not special-cased into a hit: a point with no
	// area contains nothing, which is the strict "<" the shipped circle checker uses
	// (zone\near.go), and the 3581 then goes out with count 0 exactly as §10.8
	// measured for a cast at empty ground.
	victims := h.monstersInSplash(req.Pos[0], req.Pos[1], row.SplashRange, row.SplLimit)

	sheet := h.sheetFor(*c)
	damage := castDamage(sheet.ATK, row.SklATK, 0) // the ONE damage function

	// R84 SLICE 2: the game's own damage script DECIDES the number for this cast -
	// ONCE, not once per victim: the splash's one number is shared by every body it
	// hit, so one run answers for all of them (`splash=true`, `targetCount` = how
	// many this cast recorded). The body it is shown is the first the blast
	// recorded: the script reads ONE `target` and the count is what says how many
	// there were. `damage` above is the placeholder, which is what a zone with no
	// script (or a raising one) still carries.
	var firstHit *Monster
	if len(victims) > 0 {
		firstHit = &victims[0]
	}
	damage = h.skillScriptDamage(*c, row, firstHit, true, len(victims), damage, sheet)

	targets := make([]AreaTarget, 0, len(victims))
	addHP := make([][]byte, 0, len(victims))
	handles := make([]string, 0, len(victims))
	noHP := make([]string, 0, len(victims))
	for _, mon := range victims {
		// THE HP THIS HIT MOVES COMES OUT OF THE ONE LEDGER (zone\create.go,
		// damageMonster) - the same call the plain attack (zone\attack.go), the attack
		// splash (zone\attack_splash.go) and the aimed cast (zone\skill_cast.go) make.
		//
		// IT IS NOT `mon.HP - damage` ANY MORE, and the difference is visible on the
		// SECOND blast at the same body: the spawn's number never moves, so a repeated
		// cast reported the same "after" HP twice and the target's bar jumped back up.
		// Reading the ledger makes the second cast start where the first one ended.
		//
		// `after` is what the 3557's +6 carries, and the 3581 record's +16 carries the
		// SAME number - which is what the live Fireball's own pair does: both read
		// 99,935,000 in testdata\live_skill_area_fireball.bin and
		// testdata\live_add_hp_damage.bin. So one ledger read answers both fields.
		_, after, hpKnown := h.damageMonsterBy(mon, damage, c.Handle, int(c.Level))
		hp := after
		if !hpKnown {
			// NOTHING EVER GAVE THIS BODY AN HP - a monster assembled without one (the
			// row carries no MHP column, so a body created without a script-supplied HP
			// has none). Then NO DELTA is sent for it: a 3557 whose nowHP this zone
			// cannot name is an invented number, and zone\attack.go refuses the same
			// way for the attack family. The 3581 record still goes out - it is the
			// list of who was hit - and it carries the number the spawn gave the body,
			// exactly as the aimed cast's 3573 does.
			hp = float64(mon.HP)
			noHP = append(noHP, strconv.FormatUint(uint64(mon.Handle), 10))
		}
		targets = append(targets, AreaTarget{
			Handle:     mon.Handle,
			Damage:     damage,
			AttackType: attackTypeNormal,
			HP:         hp,
			BuffMask:   nil, // this lane models no buffs; a bit here would draw an icon
			Pos:        [3]int32{mon.X, mon.Y, mon.Cell},
		})
		handles = append(handles, strconv.FormatUint(uint64(mon.Handle), 10))
		if hpKnown {
			addHP = append(addHP, BuildAddHP(mon.Handle, after, damage))
		}
	}

	area := BuildSkillArea(c.Handle, pos, pos, req.Pos, uint16(req.Skill), targets)

	out := make([][]byte, 0, 1+len(addHP))
	out = append(out, area)
	out = append(out, addHP...)

	// AND THE WATCHERS GET THE WHOLE SET, in the same order and unchanged (R93): the
	// 3581 - which is the list of who was hit, and the packet the client fans the
	// blast out from - and one 3557 per victim, because that is the half the client
	// turns into the number that flies. Half of the set would be a blast with no
	// numbers, or a number on a body that was never hit.
	others := h.fanOut(sess, c.Handle, out...)

	// THE ONE LINE THAT PROVES THE CARD: the point, how many records, the damage, and
	// the position the client is told the caster stands on - the field its 3581
	// handler fans the blast out from.
	h.Log.Printf("zone: SKILL_GROUND commander %d char %d (%d) skill %d (%q) LANDED "+
		"at point (%d %d cell %d) from the caster's live position (%d %d cell %d), "+
		"splash %g capped %d -> %d victim(s) %v, damage %g | cmd %d (%d B) + cmd %d "+
		"x%d (%d B) -> %d other player(s)",
		sess.CommanderID, req.CharIndex, c.Handle, req.Skill, h.SkillName(req.Skill),
		req.Pos[0], req.Pos[1], req.Pos[2], pos[0], pos[1], pos[2],
		row.SplashRange, row.SplLimit, len(targets), handles, damage,
		CmdZCSkillArea, len(area), CmdZCAddHP, len(addHP), AddHPSize, others)

	if row.SplashRange <= 0 {
		h.Log.Printf("zone: SKILL_GROUND: skill %d (%q) has SplashRange 0, so the "+
			"point has no area: the %d goes out with count 0 and no %d follows, "+
			"which is the empty-ground answer §10.8 measured",
			req.Skill, h.SkillName(req.Skill), CmdZCSkillArea, CmdZCAddHP)
	}

	// AND WHEN A DELTA IS ABSENT, the reason is said out loud ONCE per cast, with the
	// handles named - never a silence a reader would take for an oversight, and never
	// a number this zone cannot name. The aim of the same rule is at the attack
	// family's ledger read (zone\attack.go): a hit whose HP is invented is a health
	// plate telling a story neither side can maintain.
	if len(noHP) > 0 {
		h.Log.Printf("zone: SKILL_GROUND: no ZC_ADD_HP (3557) for handle(s) %v - this zone "+
			"keeps no HP for those bodies (nothing gave them one when they were created), "+
			"so no delta is sent for them: the %d record(s) carry the number the spawn "+
			"gave each body and invent nothing, which is the rule the attack family "+
			"states at its own ledger read (zone\\attack.go)", noHP, CmdZCSkillArea)
	}
	return out
}

// monstersInSplash returns the monsters THIS ZONE is running within `radius` of
// (x, y), in the order they were created, at most `limit` of them.
//
// The distance rule is the same one zone\near.go uses (distSq, X and Y only,
// STRICTLY less than the radius) so a splash and a script's "near" scan can never
// disagree. It differs from NearObjects in exactly one way: the fixed S1 debug
// monster (`-spawnmonster`) counts here, because it is drawn on the player's screen
// and a Fireball that leaves it undamaged would look broken. A limit of 0 or less
// means "the row states none", which is what Flame Ground's SplLimit="0" says.
func (h *Handler) monstersInSplash(x, y int32, radius float64, limit int) []Monster {
	if radius <= 0 {
		return nil // a point has no area; see the SplashRange 0 note in the handler
	}
	if limit <= 0 || limit > maxAreaTargets {
		limit = maxAreaTargets
	}
	sq := radius * radius

	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	out := make([]Monster, 0, 4)
	if h.SpawnMonster {
		if m := DevStandingMonster(); distSq(m.X, m.Y, x, y) < sq {
			out = append(out, m)
		}
	}
	for _, m := range h.spawnedMonster {
		if len(out) == limit {
			break
		}
		if distSq(m.X, m.Y, x, y) >= sq {
			continue
		}
		out = append(out, m)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
