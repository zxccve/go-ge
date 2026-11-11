package zone

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
)

// THE ORDER THE LIVE SERVER USES: every ZC_SPAWN_STANDINGPC (3520) is followed by ONE
// ZC_SKILL_DISABLE (3572) naming the SAME handle.
//
// Measured on the official server on 2026-10-28: 3 of 3 spawns, and all 36 3521
// player-enter packets, carry the 3572 immediately after the spawn, with the same four
// handle bytes and five zero bytes.
//
// WHY THE ORDER IS NOT COSMETIC: the client's handler CNormalNet::SkillDisable
// (source2\geClient\Source\GENormalNet_Action.cpp:1242) looks the object up by
// packet->handle and calls a method on the result with NO NULL CHECK. A 3572 for a
// handle the client has not been introduced to is a null dereference, not a dropped
// packet - so this test asserts BOTH directions: every spawn is followed by its 3572,
// and no 3572 ever comes before the spawn that creates its actor.
//
// The command ids and offsets here are written as LITERALS on purpose: this file is
// the copy that is run against the OLD tree to prove the test can fail, so it may only
// use identifiers that existed before the 3572 was added.
func TestJoinSends3572AfterEverySpawn(t *testing.T) {
	const (
		cmdSpawn        = 3520
		cmdSkillDisable = 3572
		// The spawn's handle is at +4 (cmd 2 + length word 2); the 3572 has NO
		// length word, so its handle is at +2.
		spawnHandleAt   = 4
		disableHandleAt = 2
	)

	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newPlayer(0x40000041, "Observer", [3]int32{2000, 2000, 1})

	if out := h.joinZone(a.sess); len(out) != 0 {
		t.Fatalf("the first player got %d packet(s), want 0", len(out))
	}
	out := h.joinZone(b.sess) // everything b is told about a, in send order

	// EVERY spawn, and then the 3572 that must follow it. This walk is done first so
	// that the failure a missing 3572 deserves - the packet's own command id - is the
	// one that is reported, not just a count.
	for i := 0; i+1 < len(out); i += 3 {
		slot := i / 3
		if cmd := int(binary.LittleEndian.Uint16(out[i][0:])); cmd != cmdSpawn {
			t.Fatalf("packet %d cmd = %d, want the spawn %d", i, cmd, cmdSpawn)
		}
		handle := binary.LittleEndian.Uint32(out[i][spawnHandleAt:])
		if want := a.sess.Team[slot].Handle; handle != want {
			t.Fatalf("spawn %d names handle %#x, want %#x", i, handle, want)
		}

		next := out[i+1]
		if cmd := int(binary.LittleEndian.Uint16(next[0:])); cmd != cmdSkillDisable {
			t.Fatalf("packet %d (right after the spawn of handle %#x) has cmd %d, want "+
				"%d: the live server sends one ZC_SKILL_DISABLE after EVERY spawn",
				i+1, handle, cmd, cmdSkillDisable)
		}
		if len(next) != 11 {
			t.Fatalf("the 3572 of handle %#x is %d bytes, want 11", handle, len(next))
		}
		if got := binary.LittleEndian.Uint32(next[disableHandleAt:]); got != handle {
			t.Fatalf("the 3572 after the spawn of %#x names %#x - the client looks that "+
				"handle up with no null check", handle, got)
		}
	}

	if len(out) != 3*len(a.sess.Team) {
		t.Fatalf("the newcomer got %d packet(s) for %d character(s), want 3 each "+
			"(spawn, skill-disable, stance)", len(out), len(a.sess.Team))
	}

	// THE OTHER DIRECTION: no 3572 may precede the spawn that creates its actor.
	spawned := map[uint32]bool{}
	for i, p := range out {
		switch int(binary.LittleEndian.Uint16(p[0:])) {
		case cmdSpawn:
			spawned[binary.LittleEndian.Uint32(p[spawnHandleAt:])] = true
		case cmdSkillDisable:
			handle := binary.LittleEndian.Uint32(p[disableHandleAt:])
			if !spawned[handle] {
				t.Fatalf("a 3572 for handle %#x arrived at %d, BEFORE that actor was "+
					"spawned - the client's handler would dereference nothing", handle, i)
			}
		}
	}
	if len(spawned) != len(a.sess.Team) {
		t.Fatalf("%d actor(s) were introduced, want %d", len(spawned), len(a.sess.Team))
	}
}
