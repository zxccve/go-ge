package zone

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
)

// THE LIVE-CAPTURE TEST (R32i) - what an armed remote character looks like on the
// wire when the OFFICIAL server introduces it.
//
// The fixture is the RAW 705 bytes of one ZC_SPAWN_STANDINGPC (3520) from a capture
// of the live server, taken while it introduced another real player's team:
//
//	captures\pcref2\20261028-232622\s2c_7054_62474.bin, spawn 2 of 6
//	"Fighter", handle 101781, job 1, stance 10110, body class 15001 in appearance
//	slot 1, shield 7215 in slot 4, sword 7201 in slot 5
//
// The bytes were copied out of the capture, not retyped: the file is the packet.
// It is a fixture and not a live read on purpose - the capture lives outside this
// repository, and a test that cannot run without it is a test that stops being run.
const liveFighterSpawn = "testdata/live_fighter_spawn.bin"

// The appearance-array geometry this project writes, restated here so the capture
// comparison never quietly borrows the server's own constants: the 21 equipment
// slots live at appearance + 68, which is packet + 374 of a spawn, and each slot is
// 12 bytes with the item's class id in its first DWORD.
const (
	liveEquipBase   = spAppearance + apEquipment // 374
	liveEquipStride = 12
)

func liveFighterSpawnBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(liveFighterSpawn)
	if err != nil {
		t.Fatalf("read %s: %v", liveFighterSpawn, err)
	}
	if len(b) != SpawnStandingPCSize {
		t.Fatalf("%s is %d bytes, want the fixed spawn size %d",
			liveFighterSpawn, len(b), SpawnStandingPCSize)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != CmdZCSpawnStandingPC {
		t.Fatalf("%s is cmd %d, want %d", liveFighterSpawn, cmd, CmdZCSpawnStandingPC)
	}
	if n := binary.LittleEndian.Uint16(b[2:]); int(n) != len(b) {
		t.Fatalf("%s's length word says %d, the file is %d bytes - the fixture is not "+
			"the packet it claims to be", liveFighterSpawn, n, len(b))
	}
	return b
}

// characterFromLive builds the Character the capture itself describes, so the test
// below compares LAYOUT - which byte holds what - and not our database's numbers.
//
// A slot whose live value is the client's own "nothing worn" id is left at 0, so the
// builder substitutes EquipNone itself; a slot carrying anything else is a worn item.
// That means the fallback and the worn path are BOTH exercised by one comparison.
func characterFromLive(t *testing.T, p []byte) Character {
	t.Helper()
	var c Character
	c.Handle = binary.LittleEndian.Uint32(p[spHandle:])
	c.Name = trimNul(p[spAppearance+apName : spAppearance+apName+nameLen])
	c.Gender = p[spAppearance+apGender]
	c.Job = uint32(p[spAppearance+apJob])
	c.HeadType = p[spAppearance+apHeadType]
	c.Stance = binary.LittleEndian.Uint32(p[spStance:])
	c.HP = math.Float64frombits(binary.LittleEndian.Uint64(p[spHP:]))
	c.MaxHP = math.Float64frombits(binary.LittleEndian.Uint64(p[spMaxHP:]))
	c.Position = [3]int32{
		int32(binary.LittleEndian.Uint32(p[spPos:])),
		int32(binary.LittleEndian.Uint32(p[spPos+4:])),
		int32(binary.LittleEndian.Uint32(p[spPos+8:])),
	}
	// The appearance's own level and promotion bytes (measured: the fixture has
	// 150 / 5 there). They are read OUT of the capture so the comparison below can
	// prove the builder writes that pair back byte for byte - the spawn carries no
	// other level for a remote character.
	c.Level = p[spAppearance+apLevel]
	c.Promotion = p[spAppearance+apPromotionLevel]
	for spot := 0; spot < equipSpotLast; spot++ {
		cls := binary.LittleEndian.Uint32(p[liveEquipBase+spot*liveEquipStride:])
		if cls != EquipNone[spot] {
			c.Equip[spot] = cls
		}
	}
	return c
}

// liveDiffAllowed answers why a differing byte offset is EXPECTED to differ, or ""
// when it is not allowed and the comparison must fail.
//
// The list is deliberately small, and every entry names the reason. Anything not on
// it - the command, the length word, the handle, the posture, the position, the
// stance, the HP pair, the name, the gender, the job, and the CLASS ID of all 21
// equipment slots - must match the live server byte for byte.
func liveDiffAllowed(off int) string {
	switch {
	case off == spClass || off == spClass+1:
		// +9: OUR value is the character's job (1 here); the live server sends 0 in
		// every one of its spawns, armed or not. Nothing yet shows which the client
		// needs, so it is left alone on purpose - it is the NEXT variable to test, and
		// changing two fields at once is how the last three passes lost a week.
		return "+9 classID: we send the job, the live server sends 0 (unproven)"
	case off >= spBuffMask && off < spAppearance:
		// 256 bytes at +50. Ours is zero BY DECISION: any value here draws the other
		// player standing in a glowing buff circle. The live server sends the
		// character's live buff state, which this server does not model.
		return "+50 buff mask: ours zero by decision, the live one is the character's buffs"
	case off >= spAppearance+apHeadType+1 && off <= spAppearance+apEquipment-1:
		// Appearance bytes 23..67: classID, AR/DR, commander name, party name, team
		// index, the display scale, the second (family) name string and one flag
		// byte at +67. This server does not model them, and they are not what this
		// lane is about - the head type at +22 IS compared.
		return "appearance +23..+67: fields we do not model (family name, scale, ...)"
	case off >= liveEquipBase && off < liveEquipBase+equipSpotLast*liveEquipStride:
		// Inside the equipment array. The CLASS ID (the first DWORD of each slot) must
		// match - that is the whole point. The two DWORDs after it are values the live
		// server fills per worn item (5591/8786 for the body, 5595 for the shield) and
		// this server writes 0; what they mean is not known.
		if inner := (off - liveEquipBase) % liveEquipStride; inner >= 4 {
			return "equipment slot's 2nd/3rd DWORD: we send 0, the live server fills them"
		}
		return ""
	case off >= liveEquipBase+equipSpotLast*liveEquipStride && off < spAppearance+spAppearanceLen:
		// The LEVEL and PROMOTION LEVEL are NOT excused: they are measured offsets
		// (apLevel / apPromotionLevel) and the fixture carries 150 / 5 there, so a
		// builder that stopped writing them fails this test instead of quietly going
		// back to a blank rank line.
		if off == spAppearance+apLevel || off == spAppearance+apPromotionLevel {
			return ""
		}
		// The rest of the appearance block: phrase, buff colour, one int32 -1. The
		// live server fills them (1430 at packet +689 is the level/promotion pair
		// above, 0xffffffff at +696 is a sentinel this build does not model); we leave
		// them zero and the client has accepted that since the first login.
		return "appearance tail +320..+394: we leave it zero"
	}
	return ""
}

// TestSpawnMatchesLiveCapture is the strongest evidence this lane can produce without
// a client: our build of an ARMED character against the live server's own bytes for
// the same character. It fails on any field that differs outside the named list, and
// it says WHICH offset.
//
// It also pins the two facts the lane was opened on:
//   - the spawn's WORD at +11 is 0 on an armed character, which is what the live
//     server sends and what this server now sends;
//   - the 21-slot equipment array is where this project already writes it, and each
//     slot's class id equals the live server's value.
func TestSpawnMatchesLiveCapture(t *testing.T) {
	live := liveFighterSpawnBytes(t)
	c := characterFromLive(t, live)

	got := BuildSpawnStandingPC(c.Handle, c, c.Position, DefaultPosture)

	if len(got) != len(live) {
		t.Fatalf("built %d bytes, the capture is %d", len(got), len(live))
	}
	if n := binary.LittleEndian.Uint16(got[2:]); int(n) != len(got) {
		t.Fatalf("length word = %d, packet = %d bytes - they MUST agree", n, len(got))
	}

	// The one variable this lane changed, named before the byte loop so a failure
	// says it plainly.
	if w := binary.LittleEndian.Uint16(got[spWeapon:]); w != 0 {
		t.Fatalf("weapon word at +11 = %d, want 0 - the live server sends 0 for this "+
			"armed Fighter and it is the value that broke the animation", w)
	}
	if w := binary.LittleEndian.Uint16(live[spWeapon:]); w != 0 {
		t.Fatalf("the capture's own +11 is %d, not 0 - this fixture is not the "+
			"packet this test was written against", w)
	}

	// The 21 class ids, one at a time, with the slot named on a failure.
	for spot := 0; spot < equipSpotLast; spot++ {
		want := binary.LittleEndian.Uint32(live[liveEquipBase+spot*liveEquipStride:])
		have := binary.LittleEndian.Uint32(got[liveEquipBase+spot*liveEquipStride:])
		if have != want {
			t.Fatalf("equipment slot %d (packet +%d) = %d, the live server sends %d",
				spot, liveEquipBase+spot*liveEquipStride, have, want)
		}
		if have == 0 {
			t.Fatalf("equipment slot %d is 0 - the client dereferences that id", spot)
		}
	}

	// Everything else, byte by byte, against the named list.
	differed, allowed := 0, 0
	var firstUnexplained string
	type run struct{ from, to int } // inclusive
	var runs []run
	var runWhy []string
	for i := range live {
		if got[i] == live[i] {
			continue
		}
		differed++
		why := liveDiffAllowed(i)
		if why == "" {
			if firstUnexplained == "" {
				firstUnexplained = fmt.Sprintf("offset +%d: ours 0x%02x, live 0x%02x",
					i, got[i], live[i])
			}
			continue
		}
		allowed++
		if n := len(runs); n > 0 && runWhy[n-1] == why && runs[n-1].to == i-1 {
			runs[n-1].to = i
			continue
		}
		runs = append(runs, run{i, i})
		runWhy = append(runWhy, why)
	}
	if firstUnexplained != "" {
		t.Fatalf("the built packet differs from the live capture at an offset that is "+
			"NOT allowed to differ - %s", firstUnexplained)
	}
	for i, r := range runs {
		t.Logf("differs at +%d..+%d (%d bytes): %s", r.from, r.to, r.to-r.from+1, runWhy[i])
	}
	t.Logf("%d of %d bytes differ from the live capture, all of them in the named "+
		"list; %d allowed, 0 unexplained", differed, len(live), allowed)
}

// TestEquipmentArrayBaseIsAppearancePlus68 pins the measurement the lane turned on,
// from the capture alone.
//
// A 12-byte slot is a CLASS ID followed by two more DWORDs, so the array can be found
// by counting: from appearance + 68, the 21st class id is the client's own "nothing
// worn" id for that slot (48000); from appearance + 80 - the same list read one slot
// later, which is what the lane's brief reported as the array's start - the 21st
// slot reads 0, a class id that makes the client dereference nothing. The live
// server has never crashed a client, so +80 cannot be the base.
func TestEquipmentArrayBaseIsAppearancePlus68(t *testing.T) {
	live := liveFighterSpawnBytes(t)

	at68 := binary.LittleEndian.Uint32(live[liveEquipBase+20*liveEquipStride:])
	if at68 != EquipNone[20] {
		t.Fatalf("slot 20 read from appearance +68 (packet +%d) = %d, want the "+
			"client's None id %d", liveEquipBase+20*liveEquipStride, at68, EquipNone[20])
	}
	at80 := liveEquipBase + liveEquipStride // the brief's base: one slot later
	slot20 := binary.LittleEndian.Uint32(live[at80+20*liveEquipStride:])
	if slot20 != 0 {
		t.Fatalf("slot 20 read from appearance +80 (packet +%d) = %d, expected the 0 "+
			"that shows this base is one slot late - if this is ever non-zero the "+
			"measurement must be redone", at80+20*liveEquipStride, slot20)
	}
	for spot := 0; spot < equipSpotLast; spot++ {
		if v := binary.LittleEndian.Uint32(live[liveEquipBase+spot*liveEquipStride:]); v == 0 {
			t.Fatalf("slot %d of the capture is 0 - the base or the stride is wrong", spot)
		}
	}
}
