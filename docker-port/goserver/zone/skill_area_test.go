package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE GROUND / AREA CAST - the live packets in, the four answers out.
//
// The oracle is the owner's own `areacast20261031` recording, cut into fixtures and
// never retyped (`history\combat.md` §10.10):
//
//	zone\testdata\live_cz_skill_ground.bin        27 B  CZ_SKILL_GROUND 3647
//	zone\testdata\live_skill_casting_area.bin     60 B  the Fireball's 3571
//	zone\testdata\live_skill_area_fireball.bin   344 B  the Fireball's 3581, count 1
//	zone\testdata\live_add_hp_damage.bin          23 B  the 3557 that carries the damage
//
// The values every test below asserts are the capture's own: charIndex 2, skill
// 20211 ("Fireball"), the point (-2245, 1355, cell 50), the caster 0x186819 standing
// at (-1855, 1355, cell 50), and the Scarecrow 0x182326 at (-2303, 1441, cell 50).
const (
	capturedGroundCharIndex = 2
	capturedFireball        = 20211
	capturedBurningHands    = 20212
	capturedFlameGround     = 20215
	capturedWarlock         = 0x186819 // 1,598,489 - the third character's handle
	capturedWarlockX        = -1855
	capturedWarlockY        = 1355
	capturedWarlockCell     = 50
	capturedPointX          = -2245
	capturedPointY          = 1355
	capturedPointCell       = 50
	warlockStance           = 20210 // PossessionFire: 20211, 20212, 20215
)

// groundFixture reads one captured packet and checks it is the packet it claims to
// be before any offset is trusted.
func groundFixture(t *testing.T, name string, cmd uint16, size int) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(b) != size {
		t.Fatalf("%s is %d bytes, want %d", name, len(b), size)
	}
	if got := binary.LittleEndian.Uint16(b); got != cmd {
		t.Fatalf("%s is cmd %d, want %d", name, got, cmd)
	}
	return b
}

// groundZone is the same two tables the aimed cast's tests load, plus the one
// character this lane needs: the Warlock, in stance 20210 (PossessionFire), which is
// the stance the reference's own 3554 named for the third character.
func groundZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	h, logs := castZone(t)
	return h, logs
}

// warlock is the captured caster of the area recording: a THREE-member team, because
// the capture's own charIndex is 2 - the Warlock is the third character. Handle
// 1,598,489, job 5 (Warlock), stance 20210, standing where its own 3571 says it
// stood, with SP to spend so the 3654 has a number in it.
func warlock() *player {
	conn := &recordConn{}
	team := []Character{
		{Handle: capturedWarlock - 2, Name: "Lead", Job: 1, Level: 50,
			HP: 1000, MaxHP: 1000, Stance: 10010,
			Position: [3]int32{capturedWarlockX - 30, capturedWarlockY, capturedWarlockCell}},
		{Handle: capturedWarlock - 1, Name: "Scout", Job: 3, Level: 50,
			HP: 1000, MaxHP: 1000, Stance: 30110,
			Position: [3]int32{capturedWarlockX - 15, capturedWarlockY, capturedWarlockCell}},
		{Handle: capturedWarlock, Name: "Warlock", Job: 5, Level: 50,
			HP: 2000, MaxHP: 2000, SP: 500, MaxSP: 500, Stance: warlockStance,
			Position: [3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell}},
	}
	// A real weapon row in the right hand, so the sheet's ATK is a number the damage
	// line can be checked against instead of the formula's floor of 1 (the same reason
	// castPlayer holds one). Stance 20210 is a magic stance but its row blocks no hand
	// (BlkHand="None"), so this is a legal character, not a cheat.
	team[capturedGroundCharIndex].Equip[HandSpotRight] = 7201 // Sword
	sess := &Session{
		Conn: conn, Team: team, Pos: team[capturedGroundCharIndex].Position,
		skipped: map[uint16]int{},
	}
	return &player{sess: sess, conn: conn}
}

// groundFrame builds one CZ_SKILL_GROUND from the struct's own offsets.
func groundFrame(charIndex uint8, skill uint32, pos [3]int32) []byte {
	b := make([]byte, CZSkillGroundSize)
	binary.LittleEndian.PutUint16(b[0:], CmdCZSkillGround)
	binary.LittleEndian.PutUint32(b[2:], 20)  // seq, as the capture had
	binary.LittleEndian.PutUint32(b[6:], 684) // checksum, as the capture had
	b[czCharIndex] = charIndex
	binary.LittleEndian.PutUint32(b[czSkillID:], skill)
	binary.LittleEndian.PutUint32(b[cgPosX:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(b[cgPosY:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(b[cgCell:], uint32(pos[2]))
	return b
}

// areaScarecrow is the recorded victim: handle 0x182326, standing at the position
// its own 3581 record gave it.
func areaScarecrow() Monster {
	return Monster{Handle: 0x182326, X: -2303, Y: 1441, Cell: 50,
		ClassID: 11600, Level: 1, HP: 99935000}
}

// spawnWithHP puts monsters into the zone AS A REAL SPAWN WOULD: the world list AND
// the HP ledger, because zone\create.go writes a live entry for every body it creates
// with an HP above zero.
//
// A fixture that fills only `spawnedMonster` describes a body nothing ever gave an HP,
// and the blast now (correctly) sends no delta for one - the same refusal zone\attack.go
// makes for a hit on such a body. Seeding BOTH is what makes a hand-built fixture stand
// for a spawned monster instead of for a body the world cannot have.
func spawnWithHP(h *Handler, mons ...Monster) {
	h.spawnedMonster = append(h.spawnedMonster, mons...)
	if h.monsterHP == nil {
		h.monsterHP = make(map[uint32]float64, len(mons))
	}
	for _, m := range mons {
		if m.HP > 0 {
			h.monsterHP[m.Handle] = float64(m.HP)
		}
	}
}

// TestParseSkillGroundLiveFrame is check (a): the captured 27-byte request decodes to
// the five values §10.1 measured, at the offsets it measured them.
func TestParseSkillGroundLiveFrame(t *testing.T) {
	p := groundFixture(t, "testdata/live_cz_skill_ground.bin", CmdCZSkillGround,
		CZSkillGroundSize)

	req, ok := ParseSkillGround(p)
	if !ok {
		t.Fatalf("the captured %d-byte CZ_SKILL_GROUND was refused", len(p))
	}
	if req.CharIndex != capturedGroundCharIndex {
		t.Fatalf("charIndex = %d, want %d (byte +10)", req.CharIndex, capturedGroundCharIndex)
	}
	if req.Skill != capturedFireball {
		t.Fatalf("skill = %d, want %d (dword at +11)", req.Skill, capturedFireball)
	}
	want := [3]int32{capturedPointX, capturedPointY, capturedPointCell}
	if req.Pos != want {
		t.Fatalf("point = %v, want %v (+15 x, +19 y, +23 cell)", req.Pos, want)
	}
	t.Logf("3647: charIndex %d, skill %d, point (%d %d cell %d) from %d bytes",
		req.CharIndex, req.Skill, req.Pos[0], req.Pos[1], req.Pos[2], len(p))
}

// TestParseSkillGroundAcceptsAPaddedFrame is the trap the earlier lanes tripped on:
// the client's cipher pads a 27-byte packet to a whole 8-byte block, so the frame on
// the wire is 32 bytes and the last five are leftover buffer, NOT fields (§10.1: all
// five live casts carried different bytes there). The parser must read the packet out
// of the longer frame rather than refuse it, and must read the same fields.
func TestParseSkillGroundAcceptsAPaddedFrame(t *testing.T) {
	p := groundFixture(t, "testdata/live_cz_skill_ground.bin", CmdCZSkillGround,
		CZSkillGroundSize)
	// The real padded frame is 32 bytes; the pad is garbage the live client left in
	// its own buffer, so any non-zero pattern proves nothing is read from it.
	padded := append(append([]byte{}, p...), 0x37, 0x75, 0x4c, 0x6a, 0xa9)

	req, ok := ParseSkillGround(padded)
	if !ok {
		t.Fatalf("a %d-byte (padded) CZ_SKILL_GROUND was refused", len(padded))
	}
	plain, _ := ParseSkillGround(p)
	if req != plain {
		t.Fatalf("the padded frame decoded to %+v, the plain one to %+v - the pad was read",
			req, plain)
	}
	// And a frame SHORTER than the payload is still refused: its fields are not there.
	if _, ok := ParseSkillGround(p[:26]); ok {
		t.Fatal("a 26-byte frame was accepted; the packet's fields run to +26")
	}
	t.Logf("3647: %d-byte frame decodes the same as the %d-byte packet; the 5 pad bytes are ignored",
		len(padded), CZSkillGroundSize)
}

// areaDiffAllowed answers why a differing byte offset in 3581 is EXPECTED to differ,
// or "" when the builder must reproduce the capture there.
//
// The list is exactly the two doubles whose VALUE is this server's decision:
//
//	record +4  the damage - ours, from our own sheet (see castDamage)
//	record +16 the target's HP - ours, the HP the spawn gave the monster
//
// Everything else is the capture's, including the 256-byte mask: the builder takes a
// mask, so the test hands it the capture's own 256 bytes and holds it to them.
func areaDiffAllowed(off int) string {
	if off >= saRecords+arDamage && off < saRecords+arDamage+8 {
		return "record +4 float64 damage: ours, from our own sheet"
	}
	if off >= saRecords+arHP && off < saRecords+arHP+8 {
		return "record +16 float64 target HP: ours, from the spawn"
	}
	return ""
}

// TestBuildSkillAreaMatchesLiveCapture is check (b): the 3581 builder re-serialises
// the owner's own 344-byte packet, byte for byte, with only the two values that are
// this server's decision allowed to differ - and it PRINTS the count, so a reader can
// see how much of the packet is the capture's and how much is ours.
func TestBuildSkillAreaMatchesLiveCapture(t *testing.T) {
	capture := groundFixture(t, "testdata/live_skill_area_fireball.bin", CmdZCSkillArea, 344)

	// The capture's own mask, read out of the capture, not retyped: the builder is
	// given the field and must put it back unchanged.
	mask := capture[saRecords+arMask : saRecords+arMask+arMaskLen]

	// Our own numbers: the damage this server's formula produces for this skill, and
	// the HP the spawn gave the monster.
	h, _ := groundZone(t)
	row, ok := h.Skills.Row(capturedFireball)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedFireball)
	}
	sheet := h.sheetFor(warlock().sess.Team[capturedGroundCharIndex])
	damage := castDamage(sheet.ATK, row.SklATK, 0)
	mon := areaScarecrow()

	got := BuildSkillArea(capturedWarlock,
		[3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell},
		[3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell},
		[3]int32{capturedPointX, capturedPointY, capturedPointCell},
		capturedFireball,
		[]AreaTarget{{
			Handle:     mon.Handle,
			Damage:     damage,
			AttackType: attackTypeKnockdown,      // the capture's own value, see the const
			HP:         float64(mon.HP) - damage, // ours: the spawn's HP after this hit
			BuffMask:   mask,
			Pos:        [3]int32{mon.X, mon.Y, mon.Cell},
		}})

	if len(got) != len(capture) {
		t.Fatalf("3581 is %d bytes, the capture %d", len(got), len(capture))
	}
	identical, differ, explained := 0, 0, 0
	for i := range got {
		if got[i] == capture[i] {
			identical++
			continue
		}
		differ++
		if areaDiffAllowed(i) != "" {
			explained++
			continue
		}
		t.Errorf("3581 byte +%d differs: capture 0x%02x, builder 0x%02x - not an allowance",
			i, capture[i], got[i])
	}
	if remain := differ - explained; remain != 0 {
		t.Fatalf("3581: %d byte(s) differ with no allowance - %d of %d identical, %d allowance(s), %d unexplained",
			remain, identical, len(got), explained, remain)
	}
	// The test must not be vacuous: every byte of the damage double really is ours,
	// so all eight of them have to differ from the capture's.
	for off := saRecords + arDamage; off < saRecords+arDamage+8; off++ {
		if got[off] == capture[off] {
			t.Fatalf("damage byte +%d is identical to the capture - the builder ignored the "+
				"damage it was given", off)
		}
	}
	if explained == 0 {
		t.Fatalf("no byte differs - this test proves nothing")
	}
	t.Logf("3581: %d of %d bytes identical, %d allowance(s), 0 unexplained "+
		"(damage %g, target HP %g differ from the capture's %g / %g by design)",
		identical, len(got), explained, damage, float64(mon.HP),
		math.Float64frombits(binary.LittleEndian.Uint64(capture[saRecords+arDamage:])),
		math.Float64frombits(binary.LittleEndian.Uint64(capture[saRecords+arHP:])))
}

// TestBuildAddHPMatchesLiveCapture: the 3557 that makes the number fly, built from
// the capture's own two values, is the capture's 23 bytes - including the flag byte
// §10.10 measured as `addHP < 0`.
func TestBuildAddHPMatchesLiveCapture(t *testing.T) {
	capture := groundFixture(t, "testdata/live_add_hp_damage.bin", CmdZCAddHP, AddHPSize)
	handle := binary.LittleEndian.Uint32(capture[ahHandle:])
	nowHP := math.Float64frombits(binary.LittleEndian.Uint64(capture[ahNowHP:]))
	addHP := math.Float64frombits(binary.LittleEndian.Uint64(capture[ahAddHP:]))

	got := BuildAddHP(handle, nowHP, addHP)
	if !bytes.Equal(got, capture) {
		t.Fatalf("3557 differs from the capture:\n got %x\nwant %x", got, capture)
	}
	if got[ahFlag] != 0 {
		t.Fatalf("3557 +22 = %d, want 0 for a positive delta", got[ahFlag])
	}
	// The negative case, which the capture does not hold: a healing tick backwards.
	if neg := BuildAddHP(handle, nowHP, -addHP); neg[ahFlag] != 1 {
		t.Fatalf("3557 +22 = %d for a negative delta, want 1 (§10.10: 1 on all 20 negatives)",
			neg[ahFlag])
	}
	t.Logf("3557: %d of %d bytes identical to the live capture (handle %d, nowHP %g, addHP %g)",
		len(capture), len(got), handle, nowHP, addHP)
}

// TestGroundCastTwoDummiesGetTwoRecords is check (c): two bodies inside the skill's
// SplashRange produce ONE 3581 carrying TWO 292-byte records and TWO 3557s - one per
// damaged target. This is the case §10.4 could not prove ("not proven: two damaged
// targets in one packet"), and the one a builder must not turn into two packets.
func TestGroundCastTwoDummiesGetTwoRecords(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	first := areaScarecrow()
	// The second dummy: also within Fireball's SplashRange 300 of the point
	// (-2245, 1355) - 55 units away, on the other side.
	second := Monster{Handle: 0x182327, X: -2250, Y: 1300, Cell: 49,
		ClassID: 11600, Level: 1, HP: 500000}
	spawnWithHP(h, first, second)

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// THE WIND-UP FIRST: 3654, 3571 - and nothing else (zone\skill_cast_clock.go).
	if len(out) != 2 {
		t.Fatalf("the press produced %d packet(s), want 2 (3654, 3571):\n%s",
			len(out), logs.String())
	}
	press := out
	// THEN THE BLAST, ON ITS CLOCK: 3581, then one 3557 per damaged target.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out = h.chaseTick(p.sess)
	if len(out) != 3 {
		t.Fatalf("one cast at two dummies produced %d packet(s) at its clock, want 3 "+
			"(3581, 3557 x2):\n%s", len(out), logs.String())
	}
	wantCmds := []uint16{CmdZCSkillArea, CmdZCAddHP, CmdZCAddHP}
	for i, want := range wantCmds {
		if got := binary.LittleEndian.Uint16(out[i]); got != want {
			t.Fatalf("packet %d is cmd %d, want %d (§10.2's order: 3581, 3557)",
				i, got, want)
		}
	}

	area := out[0]
	if want := SkillAreaHeaderSize + 2*SkillAreaRecordSize; len(area) != want {
		t.Fatalf("3581 is %d bytes, want %d (52 + 2 x 292)", len(area), want)
	}
	if got := binary.LittleEndian.Uint16(area[saCount:]); got != 2 {
		t.Fatalf("3581 +50 count = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(area[2:]); int(got) != len(area) {
		t.Fatalf("3581 +2 length = %d, want the whole packet, %d", got, len(area))
	}
	// The records, in creation order, at the stride §10.4 measured.
	for i, want := range []Monster{first, second} {
		rec := area[saRecords+i*SkillAreaRecordSize:]
		if got := binary.LittleEndian.Uint32(rec[arHandle:]); got != want.Handle {
			t.Errorf("record %d +0 handle = %d, want %d", i, got, want.Handle)
		}
		if got := worldPos(rec[arPos:]); got != [3]int32{want.X, want.Y, want.Cell} {
			t.Errorf("record %d +280 position = %v, want (%d %d %d)", i, got, want.X, want.Y, want.Cell)
		}
		dmg := math.Float64frombits(binary.LittleEndian.Uint64(rec[arDamage:]))
		if dmg < 1 {
			t.Errorf("record %d damage = %v, want at least 1", i, dmg)
		}
	}
	// The records carry OUR attack type, not the reference's knockdown 2: this lane
	// models no knockdown, exactly as the aimed cast does.
	for i := 0; i < 2; i++ {
		rec := area[saRecords+i*SkillAreaRecordSize:]
		if got := binary.LittleEndian.Uint32(rec[arAttackType:]); got != attackTypeNormal {
			t.Errorf("record %d +12 attack type = %d, want %d (NORMAL)", i, got, attackTypeNormal)
		}
		if mask := rec[arMask : arMask+arMaskLen]; !bytes.Equal(mask, make([]byte, arMaskLen)) {
			t.Errorf("record %d wrote a buff mask - this lane models no buffs", i)
		}
	}

	// 3654: the caster's handle and its SP already cut by SpendSP (500 - 60 = 440).
	if got := binary.LittleEndian.Uint32(press[0][usHandle:]); got != capturedWarlock {
		t.Errorf("3654 +2 handle = %d, want the caster %d", got, capturedWarlock)
	}
	if got := binary.LittleEndian.Uint32(press[0][usValue:]); got != 440 {
		t.Errorf("3654 +6 SP = %d, want 440 (500 - Fireball's SpendSP 60)", got)
	}

	// And the point the client sent is at +32, not the caster's own position.
	if got := worldPos(area[saPos:]); got != [3]int32{capturedPointX, capturedPointY, capturedPointCell} {
		t.Errorf("3581 +32 pos = %v, want the client's point %d/%d/%d", got,
			capturedPointX, capturedPointY, capturedPointCell)
	}

	// TWO 3557s, one per damaged handle, each carrying the record's own damage.
	seen := map[uint32]bool{}
	for _, pkt := range out[1:] {
		handle := binary.LittleEndian.Uint32(pkt[ahHandle:])
		seen[handle] = true
		rec := findRecord(t, area, handle)
		dmg := math.Float64frombits(binary.LittleEndian.Uint64(rec[arDamage:]))
		delta := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:]))
		if delta != dmg {
			t.Errorf("3557 for %d carries delta %g, its 3581 record says %g", handle, delta, dmg)
		}
		if pkt[ahFlag] != 0 {
			t.Errorf("3557 for %d has flag %d, want 0 for a positive delta", handle, pkt[ahFlag])
		}
	}
	if !seen[first.Handle] || !seen[second.Handle] {
		t.Fatalf("the 3557s name %v, want both dummies %d and %d", seen, first.Handle, second.Handle)
	}
	t.Logf("zone log:\n%s", strings.TrimSpace(logs.String()))
}

// findRecord returns the 3581 record for one handle, or fails the test.
func findRecord(t *testing.T, area []byte, handle uint32) []byte {
	t.Helper()
	count := int(binary.LittleEndian.Uint16(area[saCount:]))
	for i := 0; i < count; i++ {
		rec := area[saRecords+i*SkillAreaRecordSize:]
		if binary.LittleEndian.Uint32(rec[arHandle:]) == handle {
			return rec
		}
	}
	t.Fatalf("handle %d is not in the 3581's %d record(s)", handle, count)
	return nil
}

// TestGroundCastSecondBlastReadsTheLedger IS THIS CARD: two consecutive casts at the
// SAME body, and the second 3557's nowHP follows the LEDGER (first after - second
// damage), not `spawn HP - damage` twice.
//
// THE DEFECT IT PINS. The area lane computed `mon.HP - damage` from the value the
// spawn carried, so a second blast at a body the first had already hurt reported the
// same spawn-based "after" HP and the target's bar jumped back up. The aimed cast
// (zone\skill_cast.go) was fixed to read `before`/`after` from h.damageMonster; this is
// the area lane's half of the same fix.
//
// THE OLD ARITHMETIC ANSWERS `spawnHP - d2` HERE, which with two equal hits is one full
// damage ABOVE the ledger - so the check is not "the number changed" but "the second
// cast started exactly where the first one ended".
func TestGroundCastSecondBlastReadsTheLedger(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	victim := areaScarecrow()
	spawnHP := float64(victim.HP)
	spawnWithHP(h, victim) // the world list AND the ledger, as zone\create.go writes them

	// ONE CAST, from its press to the END of its hold, so the next press is not
	// dropped as "still animating" (R89): the HP is what this test is about, not
	// waiting out Fireball's own 900 ms + 1330 ms.
	blast := func(which string) (area, add []byte) {
		t.Helper()
		f := groundFrame(capturedGroundCharIndex, capturedFireball,
			[3]int32{capturedPointX, capturedPointY, capturedPointCell})
		press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZSkillGround, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("%s: Reply: %v", which, err)
		}
		if len(press) != 2 || cmdOf(press[0]) != CmdZCUpdateSP ||
			cmdOf(press[1]) != CmdZCSkillCasting {
			t.Fatalf("%s: the press sent %v, want 3654 then 3571:\n%s",
				which, kinds(press), logs.String())
		}
		castDueNow(t, p.sess, capturedGroundCharIndex)
		out := h.chaseTick(p.sess)
		if len(out) != 2 || cmdOf(out[0]) != CmdZCSkillArea || cmdOf(out[1]) != CmdZCAddHP {
			t.Fatalf("%s: the clock sent %v, want 3581 then one 3557:\n%s",
				which, kinds(out), logs.String())
		}
		castHoldOver(t, p.sess, capturedGroundCharIndex)
		if late := h.chaseTick(p.sess); len(late) != 0 {
			t.Fatalf("%s: the end of the hold sent %v, want nothing", which, kinds(late))
		}
		return out[0], out[1]
	}

	area1, add1 := blast("first blast")
	area2, add2 := blast("second blast")

	nowHP := func(pkt []byte) float64 {
		return math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahNowHP:]))
	}
	delta := func(pkt []byte) float64 {
		return math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:]))
	}
	d1, d2 := delta(add1), delta(add2)

	// THE FIRST BLAST starts at the spawn's HP, which IS the ledger's opening value.
	if want := spawnHP - d1; nowHP(add1) != want {
		t.Fatalf("the first blast's 3557 says nowHP %g, want the ledger's %g "+
			"(spawn %g - %g)", nowHP(add1), want, spawnHP, d1)
	}
	// THE SECOND BLAST STARTS WHERE THE FIRST ONE ENDED. `mon.HP - damage` would answer
	// spawnHP - d2 here: the spawn value read a second time, which is the bar jump.
	if want := nowHP(add1) - d2; nowHP(add2) != want {
		t.Fatalf("the second blast's 3557 says nowHP %g, want the ledger's %g "+
			"(first after %g - %g); `spawn HP - damage` would say %g, which is the "+
			"spawn-based number this card removes",
			nowHP(add2), want, nowHP(add1), d2, spawnHP-d2)
	}

	// AND THE TWO PACKETS OF ONE BLAST CANNOT DISAGREE: the live Fireball's record +16
	// and its 3557 +6 carry the SAME number (99,935,000 in both fixtures), so ours do.
	for i, pair := range []struct{ area, add []byte }{{area1, add1}, {area2, add2}} {
		rec := findRecord(t, pair.area, victim.Handle)
		recHP := math.Float64frombits(binary.LittleEndian.Uint64(rec[arHP:]))
		if recHP != nowHP(pair.add) {
			t.Errorf("blast %d: the 3581 record says HP %g and its 3557 says %g",
				i+1, recHP, nowHP(pair.add))
		}
		recDmg := math.Float64frombits(binary.LittleEndian.Uint64(rec[arDamage:]))
		if recDmg != delta(pair.add) {
			t.Errorf("blast %d: the 3581 record says damage %g and its 3557 delta %g",
				i+1, recDmg, delta(pair.add))
		}
	}

	// THE LEDGER moved by exactly the two hits, which is what makes the NEXT cast - and
	// the plain attack's own read of the same body - agree with these two packets.
	if got, want := h.monsterHPFor(victim.Handle), spawnHP-d1-d2; got != want {
		t.Errorf("the ledger holds %g after two blasts, want %g (spawn %g - %g - %g)",
			got, want, spawnHP, d1, d2)
	}
	t.Logf("two blasts at handle %d: ledger %g -> %g -> %g (deltas %g then %g: the "+
		"second nowHP is the ledger's, not the spawn's %g)",
		victim.Handle, spawnHP, nowHP(add1), nowHP(add2), d1, d2, spawnHP-d2)
}

// TestGroundCastWithoutALedgerSendsNoDelta is the other half of the rule: a body this
// zone keeps NO HP for is still ONE OF THE VICTIMS - it stays in the 3581, which is the
// list of who was hit - but no 3557 goes out for it, because a delta whose nowHP this
// zone cannot name would be an invented number. The reason is stated ONCE per cast,
// naming the handle; zone\attack.go refuses a hit on such a body for the same reason.
func TestGroundCastWithoutALedgerSendsNoDelta(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	// NO spawnWithHP on purpose: the body is in the world and the ledger has no entry
	// for it. That is a monster created without an HP - the Monster row has no MHP
	// column at all, so a body whose script asked for none gets none.
	orphan := areaScarecrow()
	h.spawnedMonster = []Monster{orphan}

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	if _, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out := h.chaseTick(p.sess)

	if len(out) != 1 || cmdOf(out[0]) != CmdZCSkillArea {
		t.Fatalf("a victim with no ledger entry produced %v, want the 3581 alone "+
			"(no delta this zone cannot name):\n%s", kinds(out), logs.String())
	}
	if got := binary.LittleEndian.Uint16(out[0][saCount:]); got != 1 {
		t.Fatalf("3581 count = %d, want 1 - the body IS a victim, it is who was hit", got)
	}
	rec := out[0][saRecords:]
	if got := binary.LittleEndian.Uint32(rec[arHandle:]); got != orphan.Handle {
		t.Errorf("the record names handle %d, want %d", got, orphan.Handle)
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(rec[arHP:])); got != float64(orphan.HP) {
		t.Errorf("the record carries HP %g, want the spawn's own %g - nothing better exists",
			got, float64(orphan.HP))
	}
	want := "no ZC_ADD_HP (3557) for handle(s) [" + strconv.FormatUint(uint64(orphan.Handle), 10) + "]"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the skipped delta is not explained in the log, want %q:\n%s",
			want, logs.String())
	}
	if n := strings.Count(logs.String(), "no ZC_ADD_HP (3557) for handle(s)"); n != 1 {
		t.Errorf("the skip was logged %d time(s), want ONCE per cast:\n%s", n, logs.String())
	}
	if _, ok := h.monsterHP[orphan.Handle]; ok {
		t.Error("the blast wrote a ledger entry for a body that had none")
	}
}

// TestGroundCastBeyondMaxRSendsNothingWithoutAPathEngine is R87 on R84's own fixture:
// a point 801 units away - one past Fireball's MinR 150 / MaxR 800 - is neither
// REFUSED nor cast from where the caster stands. It is walked into, and with no path
// engine installed (groundZone has Plan nil) there is no stand point that is real
// ground, so the answer is ONE line naming the distance and both bounds and NOTHING on
// the wire.
//
// The ARITHMETIC and the LOG are exactly what R83 measured; only what the answer MEANS
// changed, and the walk itself is in zone\skill_approach_test.go.
func TestGroundCastBeyondMaxRSendsNothingWithoutAPathEngine(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	h.spawnedMonster = []Monster{areaScarecrow()}

	row, ok := h.Skills.Row(capturedFireball)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedFireball)
	}
	if row.MaxR != 800 {
		t.Fatalf("Fireball's MaxR is %g, this test is written against 800", row.MaxR)
	}

	// 801 units to the right of the caster, on the same y: one unit too far.
	far := [3]int32{capturedWarlockX + 801, capturedWarlockY, capturedWarlockCell}
	f := groundFrame(capturedGroundCharIndex, capturedFireball, far)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a point 801 away produced %v, want nothing (no path engine, so no "+
			"stand point to walk to):\n%s", kinds(out), logs.String())
	}
	line := lastOutOfRange(logs.String())
	// THE NUMBER IS STILL MEASURABLE: the distance and both bounds are on the line.
	for _, want := range []string{"801", "MinR 150", "MaxR 800", "no path engine"} {
		if !strings.Contains(line, want) {
			t.Errorf("the OUT OF RANGE line does not name %q:\n%s", want, logs.String())
		}
	}
	// R84's claim is gone: nothing is applied from here any more.
	if strings.Contains(logs.String(), "applied anyway") {
		t.Errorf("the line still claims the cast applies anyway:\n%s", logs.String())
	}

	// The boundary itself is IN range (MaxR is inclusive: the reference's own checker
	// is `distance <= range` for a skill, unlike the strict "<" of a scan), and an
	// in-range cast logs no range line at all.
	logs.Reset()
	edge := [3]int32{capturedWarlockX + 800, capturedWarlockY, capturedWarlockCell}
	f = groundFrame(capturedGroundCharIndex, capturedFireball, edge)
	if out, err = h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 2 { // 3654, 3571 - the boundary is in range, so the cast starts
		t.Fatalf("a point exactly 800 away produced %v, want 3654, 3571 - MinR/"+
			"MaxR are bounds, not exclusions:\n%s", kinds(out), logs.String())
	}
	// AND ITS BLAST IS OWED: the clock fires 3581 (nothing in that area, so no 3557).
	castDueNow(t, p.sess, capturedGroundCharIndex)
	if blast := h.chaseTick(p.sess); len(blast) != 1 ||
		recvs(blast)[0].cmd != CmdZCSkillArea {
		t.Fatalf("the boundary cast's clock sent %v, want one 3581", kinds(blast))
	}
	if s := logs.String(); strings.Contains(s, "OUT OF RANGE") {
		t.Fatalf("a point exactly 800 away was called OUT OF RANGE:\n%s", s)
	}
	t.Logf("801 away with no path engine: nothing sent, logged as %s", strings.TrimSpace(line))
}

// TestGroundCastRefusesAPointBeyondMaxR is the OTHER half of R84: `-strictrange`
// (Handler.StrictRange, the -strictrange flag) still has to restore R83's refusal, so
// the day the reference's rule IS measured the switch is already there and works.
func TestGroundCastRefusesAPointBeyondMaxR(t *testing.T) {
	h, logs := groundZone(t)
	h.StrictRange = true
	p := warlock()
	h.spawnedMonster = []Monster{areaScarecrow()}

	far := [3]int32{capturedWarlockX + 801, capturedWarlockY, capturedWarlockCell}
	f := groundFrame(capturedGroundCharIndex, capturedFireball, far)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("with -strictrange ON a point 801 away produced %d packet(s), want 0",
			len(out))
	}
	line := logs.String()
	if !strings.Contains(line, "SKILL_GROUND refused") {
		t.Fatalf("nothing was logged as a refusal with -strictrange ON:\n%s", line)
	}
	for _, want := range []string{"801", "MinR 150", "MaxR 800"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, line)
		}
	}
	t.Logf("-strictrange ON: 801 away refused with:\n%s",
		strings.TrimSpace(firstLine(logs.String(), "refused")))
}

// firstLine is the first log line containing `word`, for a readable t.Logf.
func firstLine(log, word string) string {
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, word) {
			return l
		}
	}
	return ""
}

// TestGroundCastRefusesBuffAndPlacementFamilies is check (e): the two other Warlock
// skills are NOT this lane's family, and each is refused with ONE line naming the
// family and nothing sent. Both are legitimately offered by stance 20210, so the
// stance check passes and the family gate is what refuses them.
func TestGroundCastRefusesBuffAndPlacementFamilies(t *testing.T) {
	cases := []struct {
		skill  uint32
		family string
	}{
		{capturedBurningHands, "BUFF"},     // UseType AREA, ValueType Buff
		{capturedFlameGround, "PLACEMENT"}, // UseType PLACEMENT, ValueType Attack
	}
	for _, c := range cases {
		h, logs := groundZone(t)
		p := warlock()
		h.spawnedMonster = []Monster{areaScarecrow()}

		if !intIn(h.Stances.StanceSkills(warlockStance), int(c.skill)) {
			t.Fatalf("stance %d does not offer %d - this test needs it to", warlockStance, c.skill)
		}
		f := groundFrame(capturedGroundCharIndex, c.skill,
			[3]int32{capturedPointX, capturedPointY, capturedPointCell})
		out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
			Cmd: CmdCZSkillGround, Size: len(f), Data: f,
		})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("skill %d (%s family) produced %d packet(s), want 0",
				c.skill, c.family, len(out))
		}
		line := logs.String()
		if got := len(strings.Split(strings.TrimSpace(line), "\n")); got != 1 {
			t.Fatalf("skill %d was refused with %d log line(s), want exactly 1:\n%s",
				c.skill, got, line)
		}
		for _, want := range []string{"SKILL_GROUND refused", c.family, "not built yet"} {
			if !strings.Contains(line, want) {
				t.Errorf("the %s refusal does not say %q:\n%s", c.family, want, line)
			}
		}
		t.Logf("refused, skill %d: %s", c.skill, strings.TrimSpace(line))
	}
}

// TestAimedCastBeyondMaxRSendsNothingWithoutAPathEngine is the aimed half: a target
// 1201 units away - one past Rapid Shot's MaxR 1200 - is walked into, and with no path
// engine installed that means ONE line naming the distance and both bounds and NOTHING
// on the wire. The walk itself is zone\skill_approach_test.go (check a).
func TestAimedCastBeyondMaxRSendsNothingWithoutAPathEngine(t *testing.T) {
	h, logs := groundZone(t)
	p := castPlayer() // the captured caster, stance 10560 Peltast, Rapid Shot 10561
	far := scarecrow()
	far.X = -910 + 1201 // the captured caster stands at x -910; same y, so exactly 1201
	far.Y = 1392
	far.Cell = 29
	h.spawnedMonster = []Monster{far}

	row, ok := h.Skills.Row(capturedSkill)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedSkill)
	}
	if row.MaxR != 1200 {
		t.Fatalf("Rapid Shot's MaxR is %g, this test is written against 1200", row.MaxR)
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a target 1201 away produced %v, want nothing (no path engine, so no "+
			"stand point to walk to):\n%s", kinds(out), logs.String())
	}
	line := lastOutOfRange(logs.String())
	for _, want := range []string{"1201", "MinR 1", "MaxR 1200", "no path engine"} {
		if !strings.Contains(line, want) {
			t.Errorf("the OUT OF RANGE line does not name %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "applied anyway") {
		t.Errorf("the line still claims the cast applies anyway:\n%s", logs.String())
	}

	// The same cast at the capture's own distance (919.04 units) still answers at once,
	// with no walk and with nothing said about range: an in-range cast is not special.
	logs.Reset()
	h.spawnedMonster = []Monster{scarecrow()}
	if out, err = h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("the captured cast (919 units away) produced %v, want the one 3571:\n%s",
			kinds(out), logs.String())
	}
	if s := logs.String(); strings.Contains(s, "OUT OF RANGE") || strings.Contains(s, "APPROACH") {
		t.Fatalf("the captured 919-unit cast was treated as out of range:\n%s", s)
	}
	if p.sess.Walk != nil {
		t.Fatal("an in-range cast left a walk behind")
	}
	t.Logf("1201 away with no path engine: nothing sent, logged as %s", strings.TrimSpace(line))
}

// TestAimedCastRefusesATargetBeyondMaxR is the OTHER half of R84: with `-strictrange`
// ON (Handler.StrictRange) the R83 refusal comes back unchanged.
func TestAimedCastRefusesATargetBeyondMaxR(t *testing.T) {
	h, logs := groundZone(t)
	h.StrictRange = true
	p := castPlayer() // the captured caster, stance 10560 Peltast, Rapid Shot 10561
	far := scarecrow()
	far.X = -910 + 1201 // the captured caster stands at x -910; same y, so exactly 1201
	far.Y = 1392
	far.Cell = 29
	h.spawnedMonster = []Monster{far}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("with -strictrange ON a target 1201 away produced %d packet(s), want 0",
			len(out))
	}
	line := logs.String()
	if !strings.Contains(line, "SKILL refused") {
		t.Fatalf("nothing was logged as a refusal with -strictrange ON:\n%s", line)
	}
	for _, want := range []string{"1201", "MinR 1", "MaxR 1200"} {
		if !strings.Contains(line, want) {
			t.Errorf("the aimed refusal does not name %q:\n%s", want, line)
		}
	}
	t.Logf("-strictrange ON: aimed cast refused with:\n%s",
		strings.TrimSpace(firstLine(line, "refused")))
}

// TestGroundCastEmptyGroundSendsCountZero is §10.8's measured answer built: a cast at
// a point with nothing in the area is 3654 + 3571 + a 52-byte 3581 with count 0, and
// NO 3557 - no damage packet of any kind.
func TestGroundCastEmptyGroundSendsCountZero(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock() // no monsters registered at all

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(press) != 2 {
		t.Fatalf("the press produced %d packet(s), want 2 (3654, 3571):\n%s",
			len(press), logs.String())
	}
	// THE BLAST, ON ITS CLOCK: one 52-byte 3581 with count 0, and no 3557 at all.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out := h.chaseTick(p.sess)
	if len(out) != 1 {
		t.Fatalf("an empty-ground cast produced %d packet(s) at its clock, want 1 (3581):\n%s",
			len(out), logs.String())
	}
	area := out[0]
	if len(area) != SkillAreaHeaderSize {
		t.Fatalf("3581 is %d bytes, want the 52-byte header alone", len(area))
	}
	if got := binary.LittleEndian.Uint16(area[saCount:]); got != 0 {
		t.Fatalf("3581 +50 count = %d, want 0", got)
	}
	for _, pkt := range append(press, out...) {
		if binary.LittleEndian.Uint16(pkt) == CmdZCAddHP {
			t.Fatal("an empty-ground cast sent a 3557: §10.8 measured no damage packet at all")
		}
	}
	// 3571's +44 is 0: a GROUND cast names no target object.
	if got := binary.LittleEndian.Uint32(press[1][scTarget:]); got != 0 {
		t.Fatalf("3571 +44 target = %d, want 0 for a ground cast (§10.3, 5 of 5)", got)
	}
	if got := worldPos(press[1][scTargetPos:]); got != [3]int32{capturedPointX, capturedPointY, capturedPointCell} {
		t.Fatalf("3571 +48 = %v, want the client's point", got)
	}
	t.Logf("empty ground: 3581 %d B, count 0; 3571 +44 = 0, +48 = the point; log:\n%s",
		len(area), strings.TrimSpace(firstLine(logs.String(), "SKILL_GROUND commander")))
}

// TestGroundCastSplashRangeZeroSendsCountZero answers the question the brief asks by
// name: a skill whose own row says SplashRange="0" has no area, so nothing can be
// inside it, the 3581 goes out with count 0 and no 3557 follows - and the log says so
// rather than leaving a reader to wonder why a Fireball at a crowd hit nobody.
func TestGroundCastSplashRangeZeroSendsCountZero(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	h.spawnedMonster = []Monster{areaScarecrow()} // one dummy standing ON the point
	h.spawnedMonster[0].X, h.spawnedMonster[0].Y, h.spawnedMonster[0].Cell =
		capturedPointX, capturedPointY, capturedPointCell

	// Patch the ROW the handler reads, not the handler: the table is a plain map, so
	// a test can state the row it wants without a fixture file.
	row, ok := h.Skills.Row(capturedFireball)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedFireball)
	}
	row.SplashRange = 0
	h.Skills.byID[capturedFireball] = row
	t.Cleanup(func() {
		r := row
		r.SplashRange = 300
		h.Skills.byID[capturedFireball] = r
	})

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	press, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(press) != 2 {
		t.Fatalf("the press produced %d packet(s), want 2 (3654, 3571)", len(press))
	}
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out := h.chaseTick(p.sess)
	if len(out) != 1 {
		t.Fatalf("SplashRange 0 produced %d packet(s) at its clock, want 1 (3581 count 0)",
			len(out))
	}
	if got := binary.LittleEndian.Uint16(out[0][saCount:]); got != 0 {
		t.Fatalf("3581 count = %d with SplashRange 0, want 0", got)
	}
	if !strings.Contains(logs.String(), "SplashRange 0") {
		t.Fatalf("the SplashRange 0 cast is not explained in the log:\n%s", logs.String())
	}
	t.Logf("SplashRange 0: %d bytes, count 0, no 3557; log:\n%s", len(out[0]),
		strings.TrimSpace(firstLine(logs.String(), "SplashRange 0")))
}

// TestGroundCastCapsBySplLimit: the skill's own SplLimit is the cap (§10.4's records
// are one per victim, and Fireball's row says 5). Six dummies in the area must give
// five records, not six - the packet must never carry more than the row allows.
func TestGroundCastCapsBySplLimit(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	for i := 0; i < 6; i++ {
		spawnWithHP(h, Monster{
			Handle: uint32(0x183000 + i), X: capturedPointX + int32(i), Y: capturedPointY,
			Cell: capturedPointCell, ClassID: 11600, Level: 1, HP: 1000})
	}
	row, _ := h.Skills.Row(capturedFireball)
	if row.SplLimit != 5 {
		t.Fatalf("Fireball's SplLimit is %d, this test is written against 5", row.SplLimit)
	}

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	if _, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out := h.chaseTick(p.sess)
	if got := binary.LittleEndian.Uint16(out[0][saCount:]); got != 5 {
		t.Fatalf("3581 count = %d with six dummies in range and SplLimit 5, want 5", got)
	}
	if len(out) != 1+5 {
		t.Fatalf("the blast produced %d packet(s), want 6 (3581, 3557 x5)", len(out))
	}
	t.Logf("cap: six dummies in range, %d record(s) and %d 3557(s); log:\n%s",
		binary.LittleEndian.Uint16(out[0][saCount:]), len(out)-1,
		strings.TrimSpace(firstLine(logs.String(), "SKILL_GROUND commander")))
}

// TestGroundCastRefusesAnIndexOutsideTheTeam: the char index is a BYTE from the
// client, checked against this session's team before it reads one - the same rule the
// aimed cast and CZ_CHANGE_STANCE live under.
func TestGroundCastRefusesAnIndexOutsideTheTeam(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	h.spawnedMonster = []Monster{areaScarecrow()}

	f := groundFrame(3, capturedFireball, // the team is three members
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("character index 3 of a 3-member team produced %d packet(s)", len(out))
	}
	if line := logs.String(); !strings.Contains(line, "character index 3") {
		t.Fatalf("the index refusal does not name the index:\n%s", line)
	}
}

// TestGroundCastRefusesASkillTheStanceDoesNotOffer: the permission rule the aimed
// cast already had, now on the ground path - a Warlock skill pressed by a character
// standing in a stance that does not list it is refused and nothing is sent.
func TestGroundCastRefusesASkillTheStanceDoesNotOffer(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	p.sess.Team[0].Stance = 10010 // Bareknuckle: a job-1 stance, no Fireball in it
	h.spawnedMonster = []Monster{areaScarecrow()}

	if intIn(h.Stances.StanceSkills(10010), capturedFireball) {
		t.Fatalf("stance 10010 offers skill %d - this test needs one that does NOT",
			capturedFireball)
	}
	f := groundFrame(0, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a skill the stance does not offer produced %d packet(s)", len(out))
	}
	for _, want := range []string{"SKILL_GROUND refused", "skill 20211", "stance 10010"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, logs.String())
		}
	}
}

// TestGroundCastRefusesAShortFrame: a frame shorter than the 27-byte payload cannot
// hold the fields, so it is refused by size and nothing is sent.
func TestGroundCastRefusesAShortFrame(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	f := groundFrame(0, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})[:20]
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a 20-byte 3647 produced %d packet(s)", len(out))
	}
	if line := logs.String(); !strings.Contains(line, "20-byte frame") {
		t.Fatalf("the short-frame refusal does not name the length:\n%s", line)
	}
}

// TestProbeGroundCastOverASocket is the LIVE PROBE for this lane: a real TCP
// listener on an OS-chosen spare port, the real zone Serve loop, the real Blowfish
// framing, and a real client-side socket sending CZ_START_GAME and then
// CZ_SKILL_GROUND.
//
// Nothing here touches a database, a live role or the owner's running zones: it binds
// 127.0.0.1 on an ephemeral port and the team arrives through Handler.TeamOf instead
// of through a login. The log line it prints is PROBE evidence, not "proven in game".
func TestProbeGroundCastOverASocket(t *testing.T) {
	stances, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	skills, err := LoadSkills(stanceDir())
	if err != nil {
		t.Skipf("skill table not readable (%v)", err)
	}
	monsters, _ := LoadMonsters(stanceDir())
	spec, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}
	sizes, err := protocol.LoadTable("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}

	// The Warlock as the barrack would hand it over: job 5, bare hands - which is the
	// condition that gives it stance 20210 PossessionFire, the stance whose SkillID1
	// is 20211 Fireball.
	c := Character{Handle: capturedWarlock, Name: "Warlock", Job: 5, Level: 50,
		HP: 2000, MaxHP: 2000, SP: 500, MaxSP: 500,
		Position: [3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell}}
	c.Equip[HandSpotRight] = 7201 // Sword, so ATK is the sheet's and not the floor

	if js, jerr := LoadJobStats("../../data/job_stats.json"); jerr == nil {
		old := jobStats
		SetJobStats(js)
		t.Cleanup(func() { jobStats = old })
	}
	if is, ierr := LoadItemStats("../../data/item_stats.json"); ierr == nil {
		old := itemStats
		SetItemStats(is)
		t.Cleanup(func() { itemStats = old })
	}

	logs := &syncLog{} // the probe serves on its own goroutine, so the buffer needs a lock
	h := &Handler{
		Table: spec, Cipher: protocol.DefaultCipher,
		Log:     log.New(logs, "", 0),
		Stances: stances, Skills: skills, Monsters: monsters,
		Spawn: [3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell},
		MapID: 1331,
		TeamOf: func(context.Context, uint32) ([]Character, error) {
			return []Character{c}, nil
		},
	}
	// Two dummies in the splash, so the probe's answer carries two records and two
	// 3557s over a real socket.
	first := areaScarecrow()
	second := Monster{Handle: 0x182328, X: -2250, Y: 1300, Cell: 49,
		ClassID: 11600, Level: 1, HP: 500000}
	spawnWithHP(h, first, second)

	pick, ok := stances.PickStance(5, stances.Hands(c))
	if !ok {
		t.Fatalf("job 5 with %v has no stance - the probe cannot cast", stances.Hands(c))
	}
	offered := stances.StanceSkills(pick.Stance)
	if !intIn(offered, capturedFireball) {
		t.Fatalf("the stance the zone picks for job 5 (%d %s) offers %v, not Fireball %d - "+
			"the probe would be refused", pick.Stance, stances.StanceName(pick.Stance),
			offered, capturedFireball)
	}
	t.Logf("zone picks stance %d (%s) for job 5, which offers Fireball %d",
		pick.Stance, stances.StanceName(pick.Stance), capturedFireball)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0") // port 0: the OS picks a spare port
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go h.Serve(ctx, conn)
		}
	}()
	t.Logf("probe zone listening on %s (no database, no live role touched)", ln.Addr())

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// CZ_START_GAME (3590): cmd(2) seq(4) checksum(4) session(4) account(4) login(17).
	sg := make([]byte, 18+17)
	binary.LittleEndian.PutUint16(sg[0:], CmdCZStartGame)
	binary.LittleEndian.PutUint32(sg[2:], 1)
	binary.LittleEndian.PutUint32(sg[6:], 0x1234)
	binary.LittleEndian.PutUint32(sg[10:], 1)
	binary.LittleEndian.PutUint32(sg[14:], 7)
	copy(sg[18:], "probe")
	if _, err := conn.Write(h.Cipher.EncryptChunk(sg)); err != nil {
		t.Fatalf("write START_GAME: %v", err)
	}
	// CZ_SKILL_GROUND (3647): charIndex 0, Fireball, the captured point.
	if _, err := conn.Write(h.Cipher.EncryptChunk(groundFrame(0, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell}))); err != nil {
		t.Fatalf("write SKILL_GROUND: %v", err)
	}

	// Read the answers: the zone sends plaintext, packets back to back, so the same
	// table the splitter uses cuts them out of the stream.
	read := make([]byte, 64*1024)
	var buf []byte
	seen := map[uint16]int{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		n, rerr := conn.Read(read)
		if n > 0 {
			buf = append(buf, read[:n]...)
			frames, used := sizes.Split(buf)
			buf = buf[used:]
			for _, f := range frames {
				seen[f.Cmd]++
			}
			if seen[CmdZCSkillArea] > 0 && seen[CmdZCAddHP] >= 2 {
				break
			}
		}
		if rerr != nil && n == 0 {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				continue
			}
			break
		}
	}

	if seen[CmdZCSkillArea] != 1 || seen[CmdZCAddHP] != 2 {
		t.Fatalf("the probe's ground cast produced 3581 x%d, 3557 x%d, cmds seen %v - "+
			"want 1 and 2:\n%s", seen[CmdZCSkillArea], seen[CmdZCAddHP], seen, logs.String())
	}
	if seen[CmdZCSkillCasting] != 1 || seen[CmdZCUpdateSP] != 1 {
		t.Errorf("the probe saw 3571 x%d, 3654 x%d - want 1 each (§10.2's order)",
			seen[CmdZCSkillCasting], seen[CmdZCUpdateSP])
	}
	line := logs.String()
	if strings.Contains(line, "SKILL_GROUND refused") {
		t.Fatalf("the probe's cast was refused: %s", line)
	}
	for _, l := range strings.Split(line, "\n") {
		if strings.Contains(l, "SKILL_GROUND commander") {
			t.Logf("LIVE PROBE (%s) on port %s: %s",
				tableName(t), portOf(ln.Addr().String()), strings.TrimSpace(l))
		}
	}
}

// lastOutOfRange is the LAST line naming OUT OF RANGE: the range answer's own line
// comes first and the refusal that says WHY nothing was sent comes after it.
func lastOutOfRange(log string) string {
	last := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "OUT OF RANGE") {
			last = l
		}
	}
	return last
}

// TestSPLockKeepsTheCastersBarFull is the `-splock` development switch. The cost is
// still CUT (the reference's rule, kept as written) and then put back, so the 3654
// carries the character's own maximum instead of 500 - 60: the OFF case is the
// two-dummy test above, which still reads 440, so this test only has to show that ON
// differs in exactly that one number and says so in the log.
func TestSPLockKeepsTheCastersBarFull(t *testing.T) {
	h, logs := groundZone(t)
	h.SPLock = true
	p := warlock()
	h.spawnedMonster = []Monster{areaScarecrow()}

	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("the press produced %d packet(s), want 2 (3654, 3571):\n%s",
			len(out), logs.String())
	}
	if got := binary.LittleEndian.Uint32(out[0][usValue:]); got != 500 {
		t.Errorf("3654 +6 SP = %d, want the full bar 500 (Fireball's SpendSP 60 put back)", got)
	}
	if got := p.sess.Team[capturedGroundCharIndex].SP; got != 500 {
		t.Errorf("the character's SP = %d after the cast, want 500 with -splock on", got)
	}
	want := "SPLOCK char"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("-splock left no %q line in the log:\n%s", want, logs.String())
	}

	// THE OFF CASE IS NOT RE-TESTED HERE ON PURPOSE: it is the two-dummy test above,
	// which reads 440 on the same Fireball (500 - 60), so the flag's whole effect is the
	// difference between that number and this one. A second press in THIS test would
	// have to wait out Fireball's own 900 ms CastTime and 1330 ms hold first (R89 drops
	// a press that arrives while the body is animating), and a wait is not what this
	// switch is about.
}
