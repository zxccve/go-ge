package barrack

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

// TestBuildEquipPresetInfoEmpty pins the empty form of cmd 57: the command, then
// 864 zero bytes. It is the whole packet - a FIXED one, so there is no length
// word at +2 and the size cannot vary with the player's characters.
func TestBuildEquipPresetInfoEmpty(t *testing.T) {
	b := BuildEquipPresetInfo()

	if len(b) != 866 {
		t.Fatalf("packet is %d bytes, want 866 (the size the client registers)", len(b))
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 57 {
		t.Fatalf("command = %d, want 57", cmd)
	}
	if body := len(b) - 2; body != 864 {
		t.Fatalf("body is %d bytes, want 864 (12 characters x 2 presets x 36)", body)
	}
	for i := 2; i < len(b); i++ {
		if b[i] != 0 {
			t.Fatalf("body byte %d = 0x%02x: an empty preset has no items, so the "+
				"whole body must be zero", i-2, b[i])
		}
	}
}

// TestEquipPresetInfoAgainstLivePacket pins our size and header against the
// packet the REAL server sent (data\replay_live). It deliberately does NOT
// compare bodies: 18 of the live packet's bytes are non-zero and scattered with
// no regular stride, which is the shipped server's uninitialised memory (the same
// thing cmd 41's records carry), so matching them would mean copying another
// account's bytes.
func TestEquipPresetInfoAgainstLivePacket(t *testing.T) {
	path := filepath.Join(replayLiveDir, "BC_ALL_EQUIP_ITEM_PRESET_INFO_57.bin")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("live packet not available: %v", err)
	}

	if got, want := len(live), len(BuildEquipPresetInfo()); got != want {
		t.Fatalf("live packet is %d bytes, our builder makes %d", got, want)
	}
	if cmd := binary.LittleEndian.Uint16(live); cmd != 57 {
		t.Fatalf("live packet holds command %d, want 57", cmd)
	}
	// The proof that there is no length word: at 866 bytes the length would never
	// be zero, yet +2 is 0.
	if live[2] != 0 || live[3] != 0 {
		t.Fatalf("live +2 = %02x %02x: expected 0, which is how we know the field "+
			"there is payload, not a length word", live[2], live[3])
	}

	nonZero := 0
	for _, c := range live[2:] {
		if c != 0 {
			nonZero++
		}
	}
	// If this ever trips, the fixture changed to a packet that carries real
	// presets and the body layout has to be recovered before it can be trusted.
	if nonZero > 32 {
		t.Fatalf("live body holds %d non-zero bytes - too many to be residue; the "+
			"record layout must be recovered from ge.exe before trusting an empty build",
			nonZero)
	}
	t.Logf("live body: %d non-zero bytes of 864 (residue, not presets)", nonZero)
}

// TestReplyEquipPresetInitAnswersWithPresetList covers cmd 64. The client asks
// per character selection and retries until answered, so a missing answer leaves
// the panel of the character just picked empty and floods the log.
func TestReplyEquipPresetInitAnswersWithPresetList(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 64, Size: 14})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1 (the preset list)", len(out))
	}
	if len(out[0]) != EquipPresetInfoSize {
		t.Fatalf("answer is %d bytes, want %d", len(out[0]), EquipPresetInfoSize)
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 57 {
		t.Fatalf("answer is cmd %d, want 57 (BC_ALL_EQUIP_ITEM_PRESET_INFO)", cmd)
	}
}
