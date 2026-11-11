package zone

import (
	"io"
	"log"
	"math"
	"testing"

	"geserver/protocol"
)

// The move speed in a ZC_MOVE_PC comes from ONE place: SCR_Get_MSPD in the original's
// script (ge\script\pc\get_status_pc.scp:1764-2013), reached through the calculated
// property `CP_MSPD="SCR_Get_MSPD"` (datatable_pc.xml). It is `469 * stance.StnMSPD`
// with no gear and no buffs - NOT the stance's RSPD, which only scales the client's
// run animation. These tests pin that, and pin the two packets that carry it.

func speedTable(t *testing.T) *StanceTable {
	t.Helper()
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("LoadStances(%q): %v", stanceDir(), err)
	}
	return table
}

// moveSpeedIn reads the float at offset 22 of a ZC_MOVE_PC - the speed field.
func moveSpeedIn(b []byte) float64 {
	return float64(math.Float32frombits(le32(b[22:])))
}

// TestMoveSpeedIsTheStanceData: the table is really read, and the answer is the base
// times StnMSPD. 469 is the number the live server sends for a bare character
// (captures\warp_ref\20261027-200136\s2c_7054_64343.bin: 469.0 x4 of 62 records).
func TestMoveSpeedIsTheStanceData(t *testing.T) {
	table := speedTable(t)

	// Every bare-handed job resolves to Bareknuckle (10010): MELEE, RSPD 469,
	// StnMSPD 1 - the stance the capture's 469 group is standing in.
	row, ok := table.rows[10010]
	if !ok {
		t.Fatal("stance 10010 is not in the table - this test would prove nothing")
	}
	if row.StnMSPD != 1 {
		t.Fatalf("Bareknuckle StnMSPD = %g, want 1", row.StnMSPD)
	}

	got := table.MoveSpeed(10010, 1)
	if got != MoveBaseSpeed {
		t.Fatalf("MoveSpeed(10010) = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
	if got == 415 {
		t.Fatal("the packet speed is still CFieldChar's 415 placeholder")
	}

	// An unknown stance still walks: the formula's constant does not depend on the
	// stance, only its multiplier does.
	if got := table.MoveSpeed(0, 1); got != MoveBaseSpeed {
		t.Fatalf("MoveSpeed(no stance) = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
}

// TestMoveSpeedUsesStnMSPDNotRSPD is the trap the plan fell into: three stances whose
// RSPD and wire speed disagree. RSPD is the client's ANIMATION divisor
// (geclientlib\geScene\PC.cpp:1175-1180), so using it would send 351 and 240 where the
// real server sends 469 and 304.
func TestMoveSpeedUsesStnMSPDNotRSPD(t *testing.T) {
	table := speedTable(t)

	cases := []struct {
		stance uint32
		name   string
		want   float64
		why    string
	}{
		// PossessionFire: RSPD 351, StnMSPD 1 -> 469, not 351.
		{20210, "PossessionFire", 469, "RSPD 351 must not reach the wire"},
		// Levitation: StnMSPD 1.25 -> floor(469*1.25) = 586; the stance name raises
		// the cap to 1450 (get_status_pc.scp:1785), so nothing clamps it.
		{20140, "Levitation", 586, "StnMSPD 1.25 multiplies the 469 base"},
		// SnipeShot: StnMSPD 0.65 -> 304, then the stance's own x0.7 -> 212
		// (get_status_pc.scp:1928).
		{11530, "SnipeShot", 212, "the stance's own 0.7 penalty applies after the cap"},
	}
	for _, tc := range cases {
		row, ok := table.rows[int(tc.stance)]
		if !ok {
			t.Fatalf("stance %d (%s) is not in the table", tc.stance, tc.name)
		}
		if row.Name != tc.name {
			t.Fatalf("stance %d is %q, want %q - the test's own data moved", tc.stance, row.Name, tc.name)
		}
		if got := table.MoveSpeed(tc.stance, 1); got != tc.want {
			t.Fatalf("MoveSpeed(%s) = %.0f, want %.0f (%s)", tc.name, got, tc.want, tc.why)
		}
	}
}

// TestMoveSpeedCaps: the original clamps MSPD at 650 by default, at 750 for a MELEE or
// HYBRID stance, and not at all for jobs 200/201/213 (get_status_pc.scp:1783-1845).
func TestMoveSpeedCaps(t *testing.T) {
	table := speedTable(t)

	// GameQueen3: StnMSPD 2 -> 938. Job 1 is capped at 650...
	if got := table.MoveSpeed(19991, 1); got != DefaultSpeedCap {
		t.Fatalf("MoveSpeed(GameQueen3, job 1) = %.0f, want the 650 cap", got)
	}
	// ...and job 213 is one of the three the original leaves uncapped.
	if got := table.MoveSpeed(19991, 213); got != 938 {
		t.Fatalf("MoveSpeed(GameQueen3, job 213) = %.0f, want 938 (uncapped)", got)
	}

	// The MELEE/HYBRID cap path has no reachable stance in the table (no melee stance
	// has a StnMSPD above 1.6), so it is pinned on a table built here.
	melee := &StanceTable{rows: map[int]StanceRow{
		1: {Name: "TestMelee", ClassType: "MELEE", StnMSPD: 2},
	}}
	if got := melee.MoveSpeed(1, 1); got != MeleeSpeedCap {
		t.Fatalf("a MELEE stance at 938 = %.0f, want the 750 cap", got)
	}
	magic := &StanceTable{rows: map[int]StanceRow{
		1: {Name: "TestMagic", ClassType: "MAGIC", StnMSPD: 2},
	}}
	if got := magic.MoveSpeed(1, 1); got != DefaultSpeedCap {
		t.Fatalf("a MAGIC stance at 938 = %.0f, want the 650 cap", got)
	}
}

// TestMoveEchoCarriesTheStanceSpeed: the echo of the player's own walk carries the
// character's own speed, 469 for Bareknuckle - not 415.
func TestMoveEchoCarriesTheStanceSpeed(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0), Stances: speedTable(t)}
	a := newTeam([]uint32{0x40000031}, [][3]int32{{10, 10, 1}})
	a.sess.Team[0].Job = 1
	a.sess.Team[0].Stance = 10010
	h.joinZone(a.sess)

	pos := [3]int32{777, 888, 4}
	body := []byte{0}
	body = appendU32(body, uint32(pos[0]))
	body = appendU32(body, uint32(pos[1]))
	body = appendU32(body, uint32(pos[2]))
	pt := pack(CmdCZMovePC, body...)

	out, err := h.movePC(a.sess, protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("movePC: %v", err)
	}
	if len(out) == 0 || cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("no ZC_MOVE_PC echoed (%d packet(s))", len(out))
	}
	if got := moveSpeedIn(out[0]); got != MoveBaseSpeed {
		t.Fatalf("echoed speed = %.1f, want %.0f for a Bareknuckle character", got, MoveBaseSpeed)
	}
}

// TestChaseUsesTheSameSpeed: a chase is the same character walking, so its packet must
// carry the SAME number the echo does - one function, one answer.
func TestChaseUsesTheSameSpeed(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0), Stances: speedTable(t)}
	a := newTeam([]uint32{0x40000031}, [][3]int32{{10, 10, 1}})
	a.sess.Team[0].Job = 1
	a.sess.Team[0].Stance = 10010
	h.joinZone(a.sess)

	leg, _ := h.legMove(a.sess, [3]int32{1370, 1010, 70})
	if cmd := cmdOf(leg); cmd != CmdZCMovePC {
		t.Fatalf("cmd = %d, want ZC_MOVE_PC", cmd)
	}
	if reason := uint32(leg[26]); reason != ReasonDialog {
		t.Fatalf("reason = %d, want MR_DIALOG %d", reason, ReasonDialog)
	}
	if got, want := moveSpeedIn(leg), h.packetSpeed(&a.sess.Team[0]); got != want {
		t.Fatalf("chase speed = %.1f, echo speed = %.1f - they must be one number", got, want)
	}
	if got := moveSpeedIn(leg); got != MoveBaseSpeed {
		t.Fatalf("chase speed = %.1f, want %.0f", got, MoveBaseSpeed)
	}

	// A stance with a different StnMSPD proves the chase reads the character, not a
	// constant: Levitation multiplies by 1.25.
	//
	// A DIFFERENT DESTINATION, not the same one twice: a walk to the point already in
	// flight is refused now (zone\walk_once.go), because a second ZC_MOVE_PC restarts
	// the client's interpolation of that actor. This line is about the SPEED the packet
	// carries, so the point it walks to is free to differ.
	a.sess.Team[0].Stance = 20140
	legLev, _ := h.legMove(a.sess, [3]int32{1370, 1040, 70})
	if got := moveSpeedIn(legLev); got != 586 {
		t.Fatalf("chase speed in Levitation = %.1f, want 586", got)
	}
}
