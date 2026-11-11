package zone

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
	"time"
)

// The IDLE of a remote player, and the packet that gives it to him.
//
// Measured live 2026-10-28 with two real clients in map 10: the OTHER player stood in
// a T-pose (bind pose, no animation) while his own screen was fine. What is PROVEN
// about the packets (ge.exe, 2026-10-28):
//   - the spawn (3520) DOES carry a stance at +30, but it applies it with the flag 0,
//     which STORES the stance and starts NOTHING;
//   - ZC_CHANGE_STANCE (3554) is what starts the stance's idle (flag 1), and the live
//     server pushes one per character carrying the SAME stance id as that character's
//     spawn - see BuildChangeStance;
//   - so the packet pair now matches the official server's, twice over: on the join
//     (spawn then stance) and, for a character that never had one, after the stop that
//     ends its walk (pushOwedStance).
//
// THE VALUE was the suspect this file used to leave open, and it is now decided the
// same way the client decides it: the stance that matches what the character HOLDS
// (stance.go PickStance, whose rule ladder and ASTD check are pinned in
// stance_pick_test.go). The "the motion name does not resolve" possibility is dead
// for the shipped data - every job row and every stance row names a playable motion
// (TestNoShippedMotionNameIsUnusable).
// This file pins the packets, not the cure.
//
// Source lines for the chain (reference client source, read 2026-10-28):
//   - sender:  source2\geServer\ZServerLib\PlayerChar.cpp:1033-1040 SendCurStance()
//   - handler: source2\geClient\Source\GENormalNet.cpp:456 -> GENormalNet_Action.cpp:969
//     CNormalNet::ChangeStance -> CNormalObject::SetStance(packet->stanceType, true)
//   - apply:   source2\geClient\Source\GENormalObject.cpp:2453 -> GeBaseObject.cpp:736
//     CBaseObject::SetStance -> geclientlib\geScene\PC.cpp:81 CPC::SetStance ->
//     PC.cpp:1219 PlayASTDAnim -> the stance row's own "ASTD" motion.
//   - the field:  data\cmd_table.txt "3554   10    PKS_ZC_CHANGE_STANCE"

// TestChangeStanceIsTenBytesHandleThenStance pins the layout: cmd(2) handle(4)
// stanceType(4), NO length word.
func TestChangeStanceIsTenBytesHandleThenStance(t *testing.T) {
	const handle, stance = uint32(0x40000031), uint32(10010)
	p := BuildChangeStance(handle, stance)

	if ChangeStanceSize != 10 {
		t.Fatalf("ChangeStanceSize = %d, want the registered 10", ChangeStanceSize)
	}
	if len(p) != ChangeStanceSize {
		t.Fatalf("%d bytes, want 10 (cmd 2 + handle 4 + stance 4)", len(p))
	}
	if got := binary.LittleEndian.Uint16(p[0:]); got != CmdZCChangeStance {
		t.Fatalf("cmd = %d, want %d", got, CmdZCChangeStance)
	}
	// Bytes 2..5 are the WHOLE handle: a "length word" here would be the low half of
	// the handle, and reading it as a length is the trap this packet is known for.
	if got := binary.LittleEndian.Uint32(p[2:]); got != handle {
		t.Fatalf("handle at +2 = %#x, want %#x", got, handle)
	}
	if got := binary.LittleEndian.Uint32(p[6:]); got != stance {
		t.Fatalf("stanceType at +6 = %d, want %d", got, stance)
	}
	if got := int(binary.LittleEndian.Uint16(p[2:])); got == 10 {
		t.Log("bytes 2..3 happen to equal the size on this handle - the packet has " +
			"no length word; the test above is what proves it")
	}
}

// TestJoinSendsStanceAfterEverySpawn: when a client learns about another player, every
// character arrives as a SPAWN, then the SKILL-DISABLE the live server sends after it,
// then its STANCE - in both directions, once per character, and never a stance with no
// actor in front of it.
func TestJoinSendsStanceAfterEverySpawn(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newTeam([]uint32{0x40000041}, [][3]int32{{2000, 2000, 1}})

	h.joinZone(a.sess)
	b.conn.writes = nil // only what b is told about a matters here

	out := h.joinZone(b.sess) // b's own connection: what it is told about a
	if len(out) != 9 {
		t.Fatalf("the newcomer got %d packet(s) for 3 characters, want 9 "+
			"(a spawn, a skill-disable and a stance each)", len(out))
	}
	for i := 0; i < len(out); i += 3 {
		c := a.sess.Team[i/3]
		spawn := out[i]
		if cmd := cmdOf(spawn); cmd != CmdZCSpawnStandingPC {
			t.Fatalf("packet %d cmd = %d, want the spawn %d", i, cmd, CmdZCSpawnStandingPC)
		}
		if got := binary.LittleEndian.Uint32(spawn[4:]); got != c.Handle {
			t.Fatalf("spawn %d names handle %#x, want %#x", i, got, c.Handle)
		}
		// The 3572 the live server sends right after every spawn, naming the same
		// handle: bytes 2..5, because this packet has no length word.
		if cmd := cmdOf(out[i+1]); cmd != CmdZCSkillDisable {
			t.Fatalf("packet %d cmd = %d, want the skill-disable %d the live server "+
				"sends after every spawn", i+1, cmd, CmdZCSkillDisable)
		}
		if got := binary.LittleEndian.Uint32(out[i+1][2:]); got != c.Handle {
			t.Fatalf("skill-disable %d names handle %#x, want %#x", i+1, got, c.Handle)
		}
		if cmd := cmdOf(out[i+2]); cmd != CmdZCChangeStance {
			t.Fatalf("packet %d cmd = %d, want the stance %d - a spawn alone leaves "+
				"this client's actor with no stance and it draws a T-pose", i+2, cmd,
				CmdZCChangeStance)
		}
		if got := binary.LittleEndian.Uint32(out[i+2][2:]); got != c.Handle {
			t.Fatalf("stance %d names handle %#x, want %#x", i+2, got, c.Handle)
		}
		if got := binary.LittleEndian.Uint32(out[i+2][6:]); got != c.Stance {
			t.Fatalf("stance %d carries %d, want the character's own %d",
				i+2, got, c.Stance)
		}
	}

	// The other direction: a is told about b's character, the same three packets.
	sent := a.conn.sent()
	if len(sent) != 3 {
		t.Fatalf("the existing player got %d packet(s), want a spawn, a skill-disable "+
			"and a stance", len(sent))
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCSpawnStandingPC {
		t.Fatalf("first packet cmd = %d, want %d", cmd, CmdZCSpawnStandingPC)
	}
	if cmd := cmdOf(sent[1]); cmd != CmdZCSkillDisable {
		t.Fatalf("second packet cmd = %d, want %d", cmd, CmdZCSkillDisable)
	}
	if cmd := cmdOf(sent[2]); cmd != CmdZCChangeStance {
		t.Fatalf("third packet cmd = %d, want %d", cmd, CmdZCChangeStance)
	}
	if got, want := binary.LittleEndian.Uint32(sent[2][6:]), b.sess.Team[0].Stance; got != want {
		t.Fatalf("stance = %d, want the newcomer's own %d", got, want)
	}
}

// TestNoStancePacketWhenStanceIsUnknown: a character with no stance is announced by its
// spawn alone. Sending 3554 with 0 would tell the client to stand in stance 0, which is
// not a stance - worse than leaving the spawn's value in place.
func TestNoStancePacketWhenStanceIsUnknown(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "Jin", [3]int32{1, 1, 1}) // no Stance: unknown
	b := newPlayer(0x40000041, "Mika", [3]int32{2, 2, 2})
	h.joinZone(a.sess)
	out := h.joinZone(b.sess)

	for _, p := range out {
		if cmdOf(p) == CmdZCChangeStance {
			t.Fatalf("a character with no stance produced a %d packet", CmdZCChangeStance)
		}
	}
}

// TestWantsTickWhileAWalkIsOpen is the loop condition, and it is the bug of
// 2026-10-28: Serve only sets a read deadline while a chase or a pending click runs,
// and the zone tick (which sends the walk's stop) only runs when that deadline fires.
// A click on the GROUND starts no chase, so the tick never ran and the other client
// ran in place for ever - with the stop rule present and never executed.
func TestWantsTickWhileAWalkIsOpen(t *testing.T) {
	s := &Session{
		Pos:  [3]int32{1000, 1000, 1},
		Team: []Character{{Handle: 0x40000031, Name: "A", Position: [3]int32{1000, 1000, 1}}},
	}

	if s.wantsTick() {
		t.Fatal("a session that is doing nothing asked for a timer")
	}
	s.openWalk(0, [3]int32{1000, 1000, 1}, [3]int32{1000, 1300, 1}, time.Now(), MoveBaseSpeed)
	if !s.wantsTick() {
		t.Fatal("an open walk did not ask for a timer: the stop would never be sent")
	}
	s.closeWalk(0)
	if s.wantsTick() {
		t.Fatal("a closed walk still asked for a timer")
	}
}
