package zone

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"testing"
	"time"

	"geserver/protocol"
)

// moveFrame builds a CZ_MOVE_PC the way the client does: 10 bytes of security
// header, then charIndex and three int32s.
func moveFrame(charIndex byte, pos [3]int32) protocol.Frame {
	data := make([]byte, 23)
	binary.LittleEndian.PutUint16(data, CmdCZMovePC)
	data[10] = charIndex
	binary.LittleEndian.PutUint32(data[11:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(data[15:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(data[19:], uint32(pos[2]))
	return protocol.Frame{Cmd: CmdCZMovePC, Size: len(data), Data: data}
}

func quietHandler() *Handler {
	return &Handler{Log: log.New(io.Discard, "", 0)}
}

// TestMovePCLayout pins ZC_MOVE_PC: 30 bytes, no length word, and the position
// fields are the client's own WORLDPOS (x, y, cell).
func TestMovePCLayout(t *testing.T) {
	pos := [3]int32{7492, 10720, -1088}
	got := BuildMovePC(0xDEADBEEF, pos, 123456, float32(MoveBaseSpeed))

	if len(got) != 30 {
		t.Fatalf("packet is %d bytes, want 30", len(got))
	}
	if cmd := binary.LittleEndian.Uint16(got); cmd != CmdZCMovePC {
		t.Fatalf("command = %d, want %d", cmd, CmdZCMovePC)
	}
	if h := binary.LittleEndian.Uint32(got[2:]); h != 0xDEADBEEF {
		t.Fatalf("handle = %#x, want 0xDEADBEEF", h)
	}
	if tick := binary.LittleEndian.Uint32(got[6:]); tick != 123456 {
		t.Fatalf("tick = %d, want 123456", tick)
	}
	for i, want := range pos {
		off := 10 + i*4
		if v := int32(binary.LittleEndian.Uint32(got[off:])); v != want {
			t.Fatalf("pos[%d] = %d, want %d", i, v, want)
		}
	}
	if spd := math.Float32frombits(binary.LittleEndian.Uint32(got[22:])); spd != float32(MoveBaseSpeed) {
		t.Fatalf("speed = %v, want %v", spd, MoveBaseSpeed)
	}
	if reason := int32(binary.LittleEndian.Uint32(got[26:])); reason != ReasonMove {
		t.Fatalf("reason = %d, want %d (MR_MOVE)", reason, ReasonMove)
	}
}

// TestParseMovePC covers both directions: what the client sends, and a short
// packet that must not panic.
func TestParseMovePC(t *testing.T) {
	f := moveFrame(2, [3]int32{-1, -2, 250})
	idx, pos, ok := ParseMovePC(f.Data)
	if !ok {
		t.Fatal("a 23-byte CZ_MOVE_PC was rejected")
	}
	if idx != 2 {
		t.Fatalf("charIndex = %d, want 2", idx)
	}
	if pos != [3]int32{-1, -2, 250} {
		t.Fatalf("pos = %v, want [-1 -2 250]", pos)
	}
	if _, _, ok := ParseMovePC(f.Data[:22]); ok {
		t.Fatal("a 22-byte packet was accepted")
	}
}

// TestMovePCIsEchoedForTheMover is the rule that cost the Python server an
// evening: EVERY moving character gets its echo, not just the leader. With a
// leader-only echo, team members 2 and 3 never move at all.
func TestMovePCIsEchoedForTheMover(t *testing.T) {
	h := quietHandler()
	sess := &Session{Team: []Character{
		{Handle: 34, Name: "Jin"},
		{Handle: 40, Name: "Fighter"},
		{Handle: 38, Name: "Warlock"},
	}}

	// The FOLLOWER moves, which is the case a leader-only echo gets wrong.
	out, err := h.Reply(context.Background(), sess, moveFrame(1, [3]int32{8718, 10303, -1217}))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1 echo", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != CmdZCMovePC {
		t.Fatalf("echo is cmd %d, want %d", cmd, CmdZCMovePC)
	}
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != 40 {
		t.Fatalf("echo handle = %d, want 40 (the character that moved, not the leader)", got)
	}
	if sess.LastMover != 40 {
		t.Fatalf("LastMover = %d, want 40", sess.LastMover)
	}
	if sess.LastPos != [3]int32{8718, 10303, -1217} {
		t.Fatalf("LastPos = %v", sess.LastPos)
	}

	// A character outside the team must NOT be echoed: guessing a handle would
	// move somebody else's actor.
	out, err = h.Reply(context.Background(), sess, moveFrame(7, [3]int32{0, 0, 0}))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a move for char 7 (not in the team) produced %d packet(s)", len(out))
	}
}

// TestTheTickIsAClockSinceTheProcessStarted (piece 2i) pins the tick's SOURCE: it is a
// millisecond count from this zone process's own start, not time.Now().UnixMilli().
//
// THE REFERENCE IS THE EVIDENCE, not a preference: FieldChar_FSM.cpp:249-251 sets
// m_moveStartTick = Plat_FloatTime() when a walk starts and PlayerChar.cpp:232-242 copies
// it into ZC_MOVE_PC's tick. The capture's own 3548s read 0x00074f7c = 479100 on a server
// up about eight minutes (captures\moveback\20261102-231419). A wall clock is ~1.79e12 and
// the 4-byte field truncates it, so the number the client orders moves by was garbage.
func TestTheTickIsAClockSinceTheProcessStarted(t *testing.T) {
	first := nowMillis()
	if first < 0 || first >= 24*60*60*1000 {
		t.Fatalf("nowMillis() = %d, want a process-relative count under one day in ms", first)
	}
	if wall := time.Now().UnixMilli(); first == wall {
		t.Fatalf("nowMillis() = %d is the wall clock - the tick must count from process "+
			"start", first)
	}
	second := nowMillis()
	if second < first {
		t.Fatalf("nowMillis() went backwards: %d then %d", first, second)
	}
	t.Logf("a fresh process reads tick %d ms", first)
}

// TestMoveAndStopPacketsCarryThatOneClock reads the tick out of the packets themselves,
// so the test fails if either builder ever goes back to a wall clock (or to a constant).
func TestMoveAndStopPacketsCarryThatOneClock(t *testing.T) {
	pos := [3]int32{1000, -2000, 7}
	move := BuildMovePC(0x12345678, pos, uint32(nowMillis()), float32(MoveBaseSpeed))
	stop := BuildStopPC(0x12345678, pos)

	moveTick := binary.LittleEndian.Uint32(move[6:])
	stopTick := binary.LittleEndian.Uint32(stop[6:])
	for name, tick := range map[string]uint32{"ZC_MOVE_PC": moveTick, "ZC_STOP_PC": stopTick} {
		if tick >= 24*60*60*1000 {
			t.Fatalf("%s tick = %d, want a count since process start (< one day in ms)",
				name, tick)
		}
	}
	if stopTick < moveTick {
		t.Fatalf("the tick went backwards across two builders: move %d, stop %d",
			moveTick, stopTick)
	}
	// The low 32 bits of the wall clock are what the field USED to carry; neither packet
	// may match it. (They cannot both be right by accident: a process that has been up
	// for under a day is thousands of ms, never 1.79e12.)
	if wall := uint32(time.Now().UnixMilli()); moveTick == wall || stopTick == wall {
		t.Fatalf("a tick equals the wall clock's low 32 bits (%d): move %d, stop %d",
			wall, moveTick, stopTick)
	}
	t.Logf("move tick %d ms, stop tick %d ms", moveTick, stopTick)
}
