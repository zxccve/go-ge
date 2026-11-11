package protocol

// The family-name packet: the two numbers that matter are the SIZES, because they are
// the only thing that ties this parser to the client - 43 in, 12 out, both declared by
// the client's own barrack table and both reproduced by the original's structs.

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// buildCreateCommander is a helper that lays out the request the way the original's
// struct does: 10 bytes of security base, 16 of authCode, then the name padded to 17.
func buildCreateCommander(cmd uint16, name string) []byte {
	out := make([]byte, CreateCommanderSize)
	binary.LittleEndian.PutUint16(out[0:], cmd)
	for i := 10; i < 26; i++ {
		out[i] = 0xAA // authCode: any value, it is never read
	}
	copy(out[createCommanderNameAt:createCommanderNameAt+commanderNameLen-1], name)
	out[createCommanderNameAt+commanderNameLen-1] = 0 // the original's own NUL force
	return out
}

func TestParseCreateCommanderReadsTheNameAtOffset26(t *testing.T) {
	req, err := ParseCreateCommander(buildCreateCommander(CreateCommanderCmd, "VonHabsburg"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "VonHabsburg" {
		t.Fatalf("name = %q, want VonHabsburg", req.Name)
	}
}

func TestParseCreateCommanderCutsAtTheNULAndTrims(t *testing.T) {
	raw := buildCreateCommander(CreateCommanderCmd, "  Anne  ")
	// A name shorter than the field leaves zeros after it; add junk beyond the NUL to
	// prove the NUL wins, the way the client's stack leftovers do.
	copy(raw[createCommanderNameAt:], []byte("Anne\x00ZZZZZZ"))
	req, err := ParseCreateCommander(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "Anne" {
		t.Fatalf("name = %q, want Anne (cut at NUL, trimmed)", req.Name)
	}
}

func TestParseCreateCommanderRefusesEmptyAndShort(t *testing.T) {
	blank := buildCreateCommander(CreateCommanderCmd, "")
	blank[createCommanderNameAt] = 0
	if _, err := ParseCreateCommander(blank); !errors.Is(err, ErrEmptyCommanderName) {
		t.Fatalf("empty name gave %v, want ErrEmptyCommanderName", err)
	}

	spaces := buildCreateCommander(CreateCommanderCmd, "   ")
	if _, err := ParseCreateCommander(spaces); !errors.Is(err, ErrEmptyCommanderName) {
		t.Fatalf("spaces-only name gave %v, want ErrEmptyCommanderName", err)
	}

	if _, err := ParseCreateCommander(make([]byte, CreateCommanderSize-1)); err == nil {
		t.Fatal("a 42-byte packet was accepted")
	}
	if _, err := ParseCreateCommander(buildCreateCommander(9, "Anne")); err == nil ||
		!strings.Contains(err.Error(), "not a CB_CREATE_COMMANDER") {
		t.Fatalf("wrong command gave %v", err)
	}
}

// TestBuildCreateCommanderOKIsTwelveBytesAtTheStructsOffsets pins the reply the way the
// original declares it: command, then citizenPoint(4), rank(1), duty(1), fame(2),
// karma(2) - 10 bytes of body under a 2-byte command word.
func TestBuildCreateCommanderOKIsTwelveBytesAtTheStructsOffsets(t *testing.T) {
	got := BuildCreateCommanderOK(1234, 7, 3, 0x0102, 0x0304)
	if len(got) != CreateCommanderOKSize {
		t.Fatalf("reply is %d bytes, want %d", len(got), CreateCommanderOKSize)
	}
	if cmd := binary.LittleEndian.Uint16(got[0:]); cmd != CreateCommanderOKCmd {
		t.Fatalf("command = %d, want %d", cmd, CreateCommanderOKCmd)
	}
	if v := binary.LittleEndian.Uint32(got[2:]); v != 1234 {
		t.Fatalf("citizenPoint = %d, want 1234", v)
	}
	if got[6] != 7 || got[7] != 3 {
		t.Fatalf("rank/duty = %d/%d, want 7/3", got[6], got[7])
	}
	if v := binary.LittleEndian.Uint16(got[8:]); v != 0x0102 {
		t.Fatalf("fame = %#x, want 0x0102", v)
	}
	if v := binary.LittleEndian.Uint16(got[10:]); v != 0x0304 {
		t.Fatalf("karma = %#x, want 0x0304", v)
	}
}
