package barrack

import (
	"encoding/binary"
	"os"
	"testing"
)

const liveCapture = "../../data/captures/live-20261021/s2c_7001_49167.bin"

// TestServerToClientIsPlaintext is the regression test for the bug that cost a
// whole session: the rebuild encrypted its replies, the real client ignored
// every one, and it looked exactly like "the client is waiting for a packet".
//
// The live server's own capture settles it. Its FIRST word is a command
// (8 = BC_LOGINOK), not a length, and the second packet starts at offset 57 -
// so the packets are plaintext concatenation with no wrapper.
func TestServerToClientIsPlaintext(t *testing.T) {
	b, err := os.ReadFile(liveCapture)
	if err != nil {
		t.Skipf("live capture not available: %v", err)
	}
	if len(b) < 60 {
		t.Fatal("capture is too small to check")
	}

	if cmd := binary.LittleEndian.Uint16(b); cmd != 8 {
		t.Fatalf("first word = %d, want 8 (BC_LOGINOK). A different value here "+
			"would mean the stream is NOT plaintext", cmd)
	}
	// BC_LOGINOK is 57 bytes in the client's table; the next command must
	// follow immediately, with no length prefix in between.
	if cmd := binary.LittleEndian.Uint16(b[57:]); cmd != 56 {
		t.Fatalf("command at offset 57 = %d, want 56 (BC_HOTKEY_BIND_INFO) - "+
			"a length prefix would make this off by two", cmd)
	}
}
