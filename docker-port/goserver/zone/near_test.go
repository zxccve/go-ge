package zone

import (
	"encoding/binary"
	"strings"
	"testing"
)

// THE SCAN AND THE KILL, AGAINST THE ZONE'S OWN REGISTRY.
//
// The two rules that matter here are the ones a fake cannot answer for:
//
//  1. WHICH monsters a scan finds - the radius is measured on X and Y only, and it
//     is the shipped server's STRICT `distanceSq < range*range`, so a monster
//     standing exactly 800 units away is NOT found (`geCircleChecker.cpp:19-23`).
//  2. WHAT a kill does - NOTHING leaves the registry and NOTHING is sent: the body
//     becomes a zombie where it stands, and its corpse's own 3531 takes it off later
//     (the respawn chain in this package, and the cases in om_kill_test.go).

// seedMonster puts one monster in the runtime registry the way CreateTmonster does.
func seedMonster(w *fakeWorld, handle uint32, class uint32, x, y int32) Monster {
	m := Monster{Handle: handle, X: x, Y: y, Cell: 100, ClassID: class, Level: 130,
		HP: 499900}
	w.spawnMu.Lock()
	w.spawnedMonster = append(w.spawnedMonster, m)
	w.spawnMu.Unlock()
	return m
}

// TestNearObjectsCountsOnlyWhatIsInsideTheRadius is the measurement the script's
// "is one already here?" depends on. The four monsters are placed to sit either
// side of the boundary, and the one at EXACTLY 800 is the interesting case.
func TestNearObjectsCountsOnlyWhatIsInsideTheRadius(t *testing.T) {
	w := newFakeWorld(t)
	// The Scarecrow Creator's own dummy stands at (2240, -590).
	const npcX, npcY = 2240, -590
	seedMonster(w, 0x20000002, 11600, 2623, -665)  // ~390 away  - inside
	seedMonster(w, 0x20000003, 11605, 2240, -1390) // exactly 800 - OUTSIDE
	seedMonster(w, 0x20000004, 11610, 2240, -1389) // 799        - inside
	seedMonster(w, 0x20000005, 11601, 9000, 9000)  // far        - outside

	found, err := w.NearObjects(npcX, npcY, 800, NearKindMonster)
	if err != nil {
		t.Fatalf("NearObjects: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("%d monster(s) within 800, want 2: %+v\n(the one at EXACTLY 800 must "+
			"not be counted - the shipped checker is `distanceSq < range*range`)",
			len(found), found)
	}
	for i, want := range []uint32{0x20000002, 0x20000004} {
		if found[i].Handle != want {
			t.Errorf("monster %d is handle 0x%08x, want 0x%08x (the registry's order)",
				i, found[i].Handle, want)
		}
	}
	// The class ids travel with them: that is what the script compares against the
	// fifteen Scarecrow rows.
	if found[0].ClassID != 11600 || found[1].ClassID != 11610 {
		t.Errorf("classes %d and %d, want 11600 and 11610", found[0].ClassID, found[1].ClassID)
	}
	// And the position is the monster's OWN, not the circle's centre.
	if found[0].X != 2623 || found[0].Y != -665 {
		t.Errorf("the object stands at (%d, %d), want (2623, -665)", found[0].X, found[0].Y)
	}

	// An empty field is an ordinary answer: nothing, and no error.
	empty, err := w.NearObjects(npcX, npcY, 100, NearKindMonster)
	if err != nil || len(empty) != 0 {
		t.Fatalf("a 100-unit circle found %d monster(s) (%v), want none and no error",
			len(empty), err)
	}
}

// TestNearObjectsServesOnlyTheMonsterKind: a type word this zone cannot answer is an
// error naming it, so the script host can say WHY it answered 0 instead of pretending
// the field was empty.
func TestNearObjectsServesOnlyTheMonsterKind(t *testing.T) {
	w := newFakeWorld(t)
	seedMonster(w, 0x20000002, 11600, 2240, -590)

	for _, kind := range []string{"PC", "Item", "MonsterAndPC", ""} {
		found, err := w.NearObjects(2240, -590, 800, kind)
		if err == nil {
			t.Errorf("kind %q was served (%d object(s)); only %q is modelled here",
				kind, len(found), NearKindMonster)
			continue
		}
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the refusal for kind %q does not name it: %v", kind, err)
		}
	}
}

// TestKillMonsterZombiesItAndLeavesTheRosterAlone is the script kill's NEW shape: an
// `OM_KILL` does NOT take the monster off the field - the registry the next session is
// drawn from still holds it, in its place, and NOTHING goes out. The 3531-at-once
// version of this test is exactly what made a kited body vanish off a screen that had
// been told to drop it; the leave is the corpse's own (respawn.go, and the
// corpse/leave cases in om_kill_test.go).
func TestKillMonsterZombiesItAndLeavesTheRosterAlone(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	first := seedMonster(w, 0x20000002, 11600, 2623, -665)
	second := seedMonster(w, 0x20000003, 11601, 2700, -700)

	if err := w.KillMonsterObject(first.Handle); err != nil {
		t.Fatalf("KillMonsterObject: %v", err)
	}

	// 1. STILL REGISTERED, in its place, and the other one untouched.
	left := w.runtimeMonsters()
	if len(left) != 2 {
		t.Fatalf("%d monster(s) left (want both - the killed one is a zombie, not a "+
			"removal): %+v", len(left), left)
	}
	if left[0].Handle != first.Handle || left[1].Handle != second.Handle {
		t.Errorf("the roster is %+v, want the order the bodies were created in", left)
	}

	// 2. NOTHING SENT. The corpse's 3531 is the only leave, and it is not due yet.
	if n := len(p.conn.sent()); n != 0 {
		t.Fatalf("%d packet(s) sent with the OM_KILL, want none:\n%s", n,
			strings.Join(*w.lines, "\n"))
	}

	// 3. A CORPSE IS OWED for it, and not yet left - the handshake that will take the
	// body off the field.
	if c := corpseFor(t, w.Handler, first.Handle); c.left {
		t.Errorf("the corpse for 0x%08x is already marked left", first.Handle)
	}

	// 4. THE ONE LINE: the handle, its class, where it stands, and what will happen.
	text := strings.Join(*w.lines, "\n")
	for _, want := range []string{
		"OM_KILL 0x20000002",
		"class 11600",
		"(2623, -665",
		"is now a zombie",
		"no leave now",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log line does not carry %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "OM_KILL"); got != 1 {
		t.Errorf("%d OM_KILL line(s), want exactly one:\n%s", got, text)
	}
}

// TestKillMonsterRefusesAHandleItIsNotRunning: a stale or forged handle must not be
// able to kill somebody else's monster, and it must send nothing.
func TestKillMonsterRefusesAHandleItIsNotRunning(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)
	seedMonster(w, 0x20000002, 11600, 2623, -665)

	err := w.KillMonsterObject(0x20000009)
	if err == nil {
		t.Fatal("an unknown handle was removed")
	}
	if !strings.Contains(err.Error(), "0x20000009") {
		t.Errorf("the refusal does not name the handle: %v", err)
	}
	if got := len(w.runtimeMonsters()); got != 1 {
		t.Errorf("%d monster(s) left, want the untouched 1", got)
	}
	if got := len(p.conn.sent()); got != 0 {
		t.Errorf("%d packet(s) sent for a refused kill, want none", got)
	}

	// The FIXED S1 monster is not in the registry either, so it is not killable.
	w.SpawnMonster = true
	if err := w.KillMonsterObject(DevStandingMonster().Handle); err == nil {
		t.Error("the fixed debug monster was removed; it is not a registry entry")
	}
}

// TestBuildLeaveMonsterIsTheShippedStruct holds the builder against the shipped
// struct and the client's own table size, byte by byte. There is no capture of a
// 3531 on this machine, so the two sources ARE the proof:
//
//	PKS_DEFAULT  WORD     command -> +0
//	PKS_ZC_LEAVE OBJHANDLE handle -> +2 (4 bytes: 2+4+2 = the 8 the client's table
//	             WORD      code   -> +6  says, `data\cmd_table_live.txt:1198`)
func TestBuildLeaveMonsterIsTheShippedStruct(t *testing.T) {
	pkt := BuildLeaveMonster(0x20000002, LeaveCodeLogout)
	if len(pkt) != 8 {
		t.Fatalf("%d bytes, want 8", len(pkt))
	}
	want := []byte{
		0xCB, 0x0D, // 3531 = 0x0DCB, little endian
		0x02, 0x00, 0x00, 0x20, // the handle
		0x00, 0x00, // OBJ_LEAVE_LOGOUT
	}
	for i := range want {
		if pkt[i] != want[i] {
			t.Fatalf("byte +%d is 0x%02x, want 0x%02x\n got % x\nwant % x",
				i, pkt[i], want[i], pkt, want)
		}
	}

	// A teleport leaves with a different code, and the builder must carry it.
	other := BuildLeaveMonster(1, LeaveCodeTeleport)
	if got := binary.LittleEndian.Uint16(other[lmCode:]); got != 2 {
		t.Errorf("code = %d, want 2 (OBJ_TELEPORT)", got)
	}
}

// TestRadiusUnitsAreCentimetres pins the answer to "how far is 800?": the stored
// map unit is a centimetre, so the script's 800 is 8 m. This is not a new
// measurement - it is the unit of the server's own move speed (`zone\speed.go`,
// MoveBaseSpeed 469 cm/s, read off the live stream).
func TestRadiusUnitsAreCentimetres(t *testing.T) {
	if RadiusUnitsPerMetre != 100 {
		t.Fatalf("RadiusUnitsPerMetre = %d, want 100", RadiusUnitsPerMetre)
	}
	// 469 units per second is 4.69 m/s: a walking character, and 800 units is under
	// two seconds of it. If the unit were a metre this number would be absurd.
	if perSec := MoveBaseSpeed / RadiusUnitsPerMetre; perSec < 4 || perSec > 6 {
		t.Fatalf("%g m/s from a 469 cm/s move speed - the unit cannot be a metre",
			perSec)
	}
}
