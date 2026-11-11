package barrack

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// TestTeamGroupRecordMatchesTheLivePacket is the oracle for the record layout.
//
// The live server's own cmd 41 is 145 bytes: 5 header + 7 records of 20. If our
// TeamGroupSize or the name offset were wrong, this fails - and it is the only
// place the 20-byte stride is checked against real bytes rather than against the
// C header alone.
func TestTeamGroupRecordMatchesTheLivePacket(t *testing.T) {
	blob, err := os.ReadFile("../../data/replay_live/BC_TEAMGROUP_LIST_41.bin")
	if err != nil {
		t.Skipf("no live packet: %v", err)
	}
	if cmd := binary.LittleEndian.Uint16(blob); cmd != 41 {
		t.Fatalf("fixture is cmd %d, want 41", cmd)
	}
	if got := binary.LittleEndian.Uint16(blob[2:]); int(got) != len(blob) {
		t.Fatalf("len field = %d, packet is %d bytes - the field must be the WHOLE packet", got, len(blob))
	}

	count := int(blob[4])
	if len(blob) != TeamGroupHeader+count*TeamGroupSize {
		t.Fatalf("packet is %d bytes, but count %d at %d bytes per record gives %d",
			len(blob), count, TeamGroupSize, TeamGroupHeader+count*TeamGroupSize)
	}

	// The names the live account had. They are the capture's, and they are the
	// reason this packet must be BUILT rather than replayed.
	want := []string{"Farm", "Buff", "Fast", "Incrusio", "Buff2", "Hector", "Gissy"}
	if count != len(want) {
		t.Fatalf("live packet carries %d groups, expected %d", count, len(want))
	}
	for i, exp := range want {
		o := TeamGroupHeader + i*TeamGroupSize
		name := string(blob[o : o+TeamGroupNameLen])
		if n := strings.IndexByte(name, 0); n >= 0 {
			name = name[:n]
		}
		if name != exp {
			t.Errorf("record %d name = %q, want %q", i, name, exp)
		}
	}
}

// TestBuildTeamGroupListEmpty pins the empty form: 5 bytes, no records. This is
// what a player with no saved teams must get - and it is what clears the stale
// list the client keeps showing from an earlier session.
func TestBuildTeamGroupListEmpty(t *testing.T) {
	b := BuildTeamGroupList(nil)
	if len(b) != TeamGroupHeader {
		t.Fatalf("empty packet is %d bytes, want %d", len(b), TeamGroupHeader)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 41 {
		t.Errorf("command = %d, want 41", cmd)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != TeamGroupHeader {
		t.Errorf("len field = %d, want %d", got, TeamGroupHeader)
	}
	if b[4] != 0 {
		t.Errorf("count = %d, want 0", b[4])
	}
}

// TestBuildTeamGroupListRecords pins one record: name at 0 (NUL terminated),
// slots at +17, and EmptyCharSlot for a member that is not set - never 0, which
// is the character in slot 0.
func TestBuildTeamGroupListRecords(t *testing.T) {
	b := BuildTeamGroupList([]TeamGroup{
		{Name: "Farm", Slots: [3]byte{1, 5, EmptyCharSlot}},
		{Name: "A name that is far too long to fit", Slots: [3]byte{0, 0, 0}},
	})
	if len(b) != TeamGroupHeader+2*TeamGroupSize {
		t.Fatalf("packet is %d bytes, want %d", len(b), TeamGroupHeader+2*TeamGroupSize)
	}
	if b[4] != 2 {
		t.Fatalf("count = %d, want 2", b[4])
	}

	o := TeamGroupHeader
	if got := string(b[o : o+4]); got != "Farm" {
		t.Errorf("record 0 name = %q, want Farm", got)
	}
	if b[o+4] != 0 {
		t.Errorf("record 0 name is not NUL terminated: byte 4 = %d", b[o+4])
	}
	if got := b[o+TeamGroupNameLen : o+TeamGroupSize]; string(got) != string([]byte{1, 5, EmptyCharSlot}) {
		t.Errorf("record 0 slots = %v, want [1 5 255]", got)
	}

	// A long name is truncated and still NUL terminated: the C++ uses
	// strcpy_s into a 17-byte buffer, so an over-long name cannot be sent as is.
	o = TeamGroupHeader + TeamGroupSize
	name := b[o : o+TeamGroupNameLen]
	if name[TeamGroupNameLen-1] != 0 {
		t.Errorf("a truncated name must still end in NUL, got %v", name)
	}
	if got := string(name[:TeamGroupNameLen-1]); got != "A name that is f" {
		t.Errorf("truncated name = %q, want %q", got, "A name that is f")
	}
}
