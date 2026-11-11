package protocol

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// zoneTablePath is the three-column table the zone loads: the client's own id
// space, which is all client->server packets.
const zoneTablePath = "../../data/cmd_table_live_z.txt"

// pkt builds a packet of exactly size bytes: the command word first, then
// whatever words are given, zero padded. A test that forgets the padding
// tests nothing, because the splitter would be cutting a packet that is
// shorter than the table says it is.
func pkt(size int, cmd uint16, words ...uint16) []byte {
	b := make([]byte, size)
	binary.LittleEndian.PutUint16(b, cmd)
	for i, v := range words {
		if 2+2*i+2 <= size {
			binary.LittleEndian.PutUint16(b[2+2*i:], v)
		}
	}
	return b
}

func TestSplitFixedSize(t *testing.T) {
	tab := Table{100: 6, 200: 8}
	stream := append(pkt(6, 100), pkt(8, 200)...)

	frames, used := tab.Split(stream)
	if used != len(stream) {
		t.Fatalf("consumed %d, want %d", used, len(stream))
	}
	if len(frames) != 2 || frames[0].Cmd != 100 || frames[1].Cmd != 200 {
		t.Fatalf("frames = %+v, want cmd 100 then 200", frames)
	}
	if len(frames[0].Data) != 6 || len(frames[1].Data) != 8 {
		t.Fatalf("frame sizes = %d, %d, want 6, 8",
			len(frames[0].Data), len(frames[1].Data))
	}
}

func TestSplitVariableSize(t *testing.T) {
	// Size 0 = variable. The length word counts the whole packet, itself
	// included, which is the packet's total size.
	tab := Table{300: 0}
	pkt := pkt(10, 300, 10) // cmd 300, length word = 10

	frames, used := tab.Split(pkt)
	if used != 10 {
		t.Fatalf("consumed %d, want 10", used)
	}
	if len(frames) != 1 || frames[0].Cmd != 300 || frames[0].Size != 10 {
		t.Fatalf("frames = %+v, want one 10-byte 300", frames)
	}
}

func TestSplitUnknownCommandTakesRest(t *testing.T) {
	tab := Table{100: 6}
	stream := append(pkt(6, 100), pkt(8, 999, 1, 2, 3)...)

	frames, used := tab.Split(stream)
	if used != len(stream) {
		t.Fatalf("consumed %d, want all %d bytes", used, len(stream))
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	last := frames[len(frames)-1]
	if last.Cmd != 999 || last.Size != 8 {
		t.Fatalf("unknown frame = %+v, want cmd 999 with the 8 bytes left", last)
	}
}

func TestSplitKeepsPartialPacket(t *testing.T) {
	tab := Table{100: 6, 200: 8}
	stream := append(pkt(6, 100), pkt(8, 200)...)

	// Drop the last byte: the 200 packet is not complete, so it must stay.
	frames, used := tab.Split(stream[:len(stream)-1])
	if len(frames) != 1 || frames[0].Cmd != 100 {
		t.Fatalf("frames = %+v, want only cmd 100", frames)
	}
	if used != 6 {
		t.Fatalf("consumed %d, want 6 - the partial packet must stay in the buffer", used)
	}
}

func TestSplitVariableSizeRejectsShortLength(t *testing.T) {
	// A length of 3 cannot even cover cmd+len, so it is not a packet.
	tab := Table{300: 0}
	frames, used := tab.Split(pkt(8, 300, 3))
	if len(frames) != 0 || used != 0 {
		t.Fatalf("frames=%+v used=%d, want nothing consumed", frames, used)
	}
}

func TestSplitEmpty(t *testing.T) {
	frames, used := Table{}.Split(nil)
	if len(frames) != 0 || used != 0 {
		t.Fatalf("frames=%+v used=%d, want nothing", frames, used)
	}
}

// czBuyItem is a real CZ_BUYITEM (3672) off the wire, decrypted: 33 bytes of
// packet and 7 bytes of the cipher's padding, 40 in all.
//
//	+0  cmd 0x0E58   +2  seq 50   +6  checksum 0x1D8   +10  length 0x21 = 33
//
// The zone table is three-column, so this row has no shape of its own. Reading
// the length at +2 reads the SEQUENCE NUMBER (50), which is longer than the
// chunk, so the splitter holds the bytes back and the packet never reaches the
// handler. The length is at +10, inside the security header.
const czBuyItem = "580e32000000d80100002100000000000100000001d03241" +
	"760a0000000000000088d7cedf48e0e6"

func TestSplitZoneBuyItem(t *testing.T) {
	spec, err := LoadSpec(zoneTablePath)
	if err != nil {
		t.Skipf("zone command table not readable: %v", err)
	}

	// The shape a three-column table gave this row must be the security one.
	if size, ok := spec.Size(3672); !ok || size != 0 {
		t.Fatalf("CZ_BUYITEM (3672) size = %d (known=%v), want 0 (variable)", size, ok)
	}
	if off, ok := spec.LenAt(3672); !ok || off != LenAtSecurity {
		t.Fatalf("CZ_BUYITEM (3672) length offset = %d (known=%v), want %d",
			off, ok, LenAtSecurity)
	}

	wire, err := hex.DecodeString(czBuyItem)
	if err != nil {
		t.Fatalf("sample hex: %v", err)
	}
	if len(wire) != 40 {
		t.Fatalf("sample is %d bytes, want 40", len(wire))
	}

	frames, used := spec.Split(wire)
	if len(frames) == 0 {
		t.Fatalf("split produced no frame - CZ_BUYITEM is being dropped (used %d)", used)
	}
	first := frames[0]
	if first.Cmd != 3672 {
		t.Fatalf("first frame command = %d, want 3672 (CZ_BUYITEM)", first.Cmd)
	}
	if first.Size != 33 {
		t.Fatalf("first frame size = %d, want 33 (the length word)", first.Size)
	}
	if first.Data[10] != 0x21 {
		t.Fatalf("byte 10 of the frame = %#x, want 0x21 (the length word)", first.Data[10])
	}
	if used < 33 {
		t.Fatalf("consumed %d bytes, want at least 33", used)
	}

	// The 7 pad bytes after it are not zero, so they are still not a command:
	// they are shorter than the shortest client frame, which makes them cipher
	// padding (see MinSecurityFrame). Nothing may be invented from them.
	if len(frames) > 1 {
		t.Fatalf("frames = %+v, want only CZ_BUYITEM - the pad bytes became a packet", frames)
	}
	if used != len(wire) {
		t.Fatalf("consumed %d of %d bytes, want the padding consumed too", used, len(wire))
	}
}

// TestSplitPaddingIsNotACommand is ROADMAP R45. The cipher pads the plaintext
// to a whole 8-byte block, so a chunk can end in up to 7 bytes that are NOT
// zeros. Read as a command word they look like an id the table never heard of,
// and the zone logged one "cmd N not handled yet" line for them per frame,
// once a second, until a real CZ_CHAT was lost among them.
func TestSplitPaddingIsNotACommand(t *testing.T) {
	spec, err := LoadSpec(zoneTablePath)
	if err != nil {
		t.Skipf("zone command table not readable: %v", err)
	}

	// The same six bytes the live log showed: cmd 47637 and four more.
	rubble := []byte{0x15, 0xba, 0x96, 0x09, 0x3a, 0xd2}

	// One clean packet plus the rubble is EXACTLY one command.
	// 3437 is CZ_REQ_UPDATE_CURRENT_TAXSUM, a real 10-byte CZ_ packet.
	stream := append(pkt(10, 3437), rubble...)
	frames, used := spec.Split(stream)
	if len(frames) != 1 || frames[0].Cmd != 3437 {
		t.Fatalf("frames = %+v, want only cmd 3437", frames)
	}
	if used != len(stream) {
		t.Fatalf("consumed %d of %d bytes - the padding was left for the next read",
			used, len(stream))
	}

	// Padding on its own is not a command at all.
	frames, used = spec.Split(append([]byte(nil), rubble...))
	if len(frames) != 0 || used != len(rubble) {
		t.Fatalf("frames = %+v used = %d, want none and all %d bytes consumed",
			frames, used, len(rubble))
	}

	// The floor is a LENGTH test, so anything at least MinSecurityFrame long is
	// still handed back: an unknown id that could be a real packet must stay
	// visible, because swallowing it would hide a command the table is missing.
	big := make([]byte, MinSecurityFrame)
	binary.LittleEndian.PutUint16(big, 0x9999)
	frames, _ = spec.Split(big)
	if len(frames) != 1 || frames[0].Cmd != 0x9999 {
		t.Fatalf("frames = %+v, want the unknown %d-byte command reported", frames, len(big))
	}

	// And a real command that arrives short is still held back, not reported as
	// padding: its id IS in the table, and a fixed-size packet that has not
	// finished arriving is not rubble.
	frames, used = spec.Split(pkt(10, 3437)[:8])
	if len(frames) != 0 || used != 0 {
		t.Fatalf("frames = %+v used = %d, want the short packet held back", frames, used)
	}
}
