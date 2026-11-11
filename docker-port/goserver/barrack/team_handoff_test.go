package barrack

import (
	"encoding/binary"
	"testing"
)

// startGameFrame builds a CB_STARTGAME/CB_CONTINUEGAME body: the 10-byte security
// header, the channel word, then the three slot words - padded to the 22 bytes the
// live client really sends.
func startGameFrame(cmd, channel uint16, slots [3]uint16) []byte {
	b := make([]byte, 22)
	binary.LittleEndian.PutUint16(b[0:], cmd)
	binary.LittleEndian.PutUint16(b[StartGameHeaderLen:], channel)
	for i, s := range slots {
		binary.LittleEndian.PutUint16(b[StartGameHeaderLen+2+i*2:], s)
	}
	return b
}

// TestParseStartGame pins the offsets: channel at +10, slots at +12..18, which is
// PKS_SECURITY_DEFAULT (10 bytes) + CHANNEL_ID + WORD charSlotID[3].
func TestParseStartGame(t *testing.T) {
	f := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})

	channel, slots, ok := ParseStartGame(f)
	if !ok {
		t.Fatal("ParseStartGame said the frame is too short")
	}
	if channel != 1 {
		t.Fatalf("channel = %d, want 1", channel)
	}
	if slots != [3]uint16{0, 3, EmptyCharSlot} {
		t.Fatalf("slots = %v, want [0 3 255]", slots)
	}

	// EMPTY_CHARSLOT must not be reported as a picked character, but the row keeps
	// it so the record stays a faithful copy of the request.
	if got := pickedSlots(slots); len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("picked = %v, want [0 3]", got)
	}

	// A short frame must be reported, not silently zero-filled.
	if _, _, ok := ParseStartGame(f[:17]); ok {
		t.Fatal("a 17-byte frame was accepted")
	}
}
