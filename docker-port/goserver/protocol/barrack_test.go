package protocol

import (
	"encoding/hex"
	"testing"
)

// barrackTablePath is the small id space the zone table does not cover.
const barrackTablePath = "../../data/cmd_table_barrack.txt"

// TestBarrackTableLoads checks the table the client itself provided: the login
// is variable size with its length word at +10 (PKS_SECURITY_VARSIZE), and the
// reply the client expects is 57 bytes.
func TestBarrackTableLoads(t *testing.T) {
	spec, err := LoadSpec(barrackTablePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}

	if size, ok := spec.Size(4); !ok || size != 0 {
		t.Fatalf("CB_LOGIN (4) size = %d (known=%v), want 0 (variable)", size, ok)
	}
	if off, ok := spec.LenAt(4); !ok || off != LenAtSecurity {
		t.Fatalf("CB_LOGIN (4) length offset = %d (known=%v), want %d", off, ok, LenAtSecurity)
	}
	if size, ok := spec.Size(8); !ok || size != 57 {
		t.Fatalf("BC_LOGINOK (8) size = %d (known=%v), want 57", size, ok)
	}
	// A fixed packet needs no length offset.
	if _, ok := spec.LenAt(9); ok {
		t.Fatal("CB_STARTBARRACK (9) is fixed size, it must not carry a length offset")
	}
}

// TestBarrackLoginSplits is the first BARRACK packet fixture: the captured
// login must cut into exactly one 812-byte packet, with the cipher's 4 bytes of
// padding left over.
//
// The padding is the trap: the wire carried 816 bytes because the cipher pads
// to a multiple of 8, but the length word says 812, and 812 is the packet.
func TestBarrackLoginSplits(t *testing.T) {
	spec, err := LoadSpec(barrackTablePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	lines := plainFixture(t, "testdata/barrack_stub.wire.txt")
	if len(lines) != 1 {
		t.Fatalf("expected 1 captured chunk, got %d", len(lines))
	}
	plain, err := hex.DecodeString(lines[0])
	if err != nil {
		t.Fatalf("fixture hex: %v", err)
	}

	frames, used := spec.Split(plain)
	if len(frames) != 1 {
		t.Fatalf("split into %d frames, want 1", len(frames))
	}
	if frames[0].Cmd != 4 {
		t.Fatalf("frame command = %d, want 4 (CB_LOGIN)", frames[0].Cmd)
	}
	if frames[0].Size != 812 {
		t.Fatalf("frame size = %d, want 812 (the length word)", frames[0].Size)
	}
	// The 4 padding bytes must be consumed, not turned into a second packet:
	// a splitter that keeps reading sees command 0 in them.
	if used != len(plain) {
		t.Fatalf("consumed %d of %d bytes - the padding was left behind", used, len(plain))
	}

	// The tail beyond the declared length must BE padding (all zeros) - that is
	// what makes consuming it safe rather than a guess.
	tail := plain[812:]
	if len(tail) != len(plain)-812 {
		t.Fatalf("tail = %d bytes, want %d", len(tail), len(plain)-812)
	}
	for i, b := range tail {
		if b != 0 {
			t.Fatalf("byte %d past the packet = %02x, want 0 (cipher padding)", 812+i, b)
		}
	}
}

// TestZoneTableDefaultsToSecurityShape proves the three-column zone file still
// loads, and that its size-0 rows take the security shape. That file is only
// ever used to cut client->server data, so every size-0 row is a CZ_ packet and
// keeps its length at +10. Defaulting to +2 read the sequence number and
// dropped CZ_BUYITEM (3672) entirely.
func TestZoneTableDefaultsToSecurityShape(t *testing.T) {
	spec, err := LoadSpec(cmdTablePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if size, ok := spec.Size(3672); !ok || size != 0 {
		t.Fatalf("CZ_BUYITEM (3672) size = %d (known=%v), want 0", size, ok)
	}
	if off, ok := spec.LenAt(3672); !ok || off != LenAtSecurity {
		t.Fatalf("CZ_BUYITEM length offset = %d (known=%v), want %d",
			off, ok, LenAtSecurity)
	}
}
