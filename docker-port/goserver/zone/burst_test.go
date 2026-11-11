package zone

import (
	"encoding/binary"
	"testing"
)

// TestBurstSizesMatchTheReference pins every packet size. These numbers come from
// the Python builders, which measured them against the live client; a "tidy up"
// that changes one is a misparse in the client, not a style choice.
func TestReinforceListIsTypeThenCount(t *testing.T) {
	// One 5-byte record: item index 7, reinforce count 2.
	pkt := BuildReinforceList([]byte{7, 0, 0, 0, 2})
	if len(pkt) != 17 {
		t.Fatalf("%d bytes, want 17 (4 + 8 + 5)", len(pkt))
	}
	if typ := binary.LittleEndian.Uint32(pkt[4:]); typ != invTypeInventory {
		t.Fatalf("type = %d, want %d", typ, invTypeInventory)
	}
	if count := binary.LittleEndian.Uint32(pkt[8:]); count != 1 {
		t.Fatalf("count = %d, want 1 record", count)
	}
}

// TestPropertyListHasTheFlagByte is the 13-vs-12 distinction: without the flag the
// client shifts every following record by one byte and crashes.
func TestPropertyListHasTheFlagByte(t *testing.T) {
	pkt := BuildInventoryPropertyList(nil)
	if len(pkt) != 13 {
		t.Fatalf("%d bytes, want 13 - the header is 13 because of the flag byte", len(pkt))
	}
	if pkt[12] != 0 {
		t.Fatalf("flag byte = %d, want 0 (keep the properties)", pkt[12])
	}
}

// TestEntryBurstOrderAndContent checks the burst as the client receives it: the
// packets go out in one order, and the data-dependent ones are NAMED as skipped
// rather than silently absent.
func TestEntryBurstOrderAndContent(t *testing.T) {
	packets, skipped := EntryBurst(nil, nil)
	if len(packets) < 10 {
		t.Fatalf("%d packet(s), expected at least 10", len(packets))
	}
	// The first packet must be the commander point, and the four commwarp lists
	// must come last, one per type.
	if cmd := binary.LittleEndian.Uint16(packets[0]); cmd != CmdZCCommanderPoint {
		t.Fatalf("first packet is cmd %d, want %d", cmd, CmdZCCommanderPoint)
	}
	last := packets[len(packets)-1]
	if cmd := binary.LittleEndian.Uint16(last); cmd != CmdZCCommWarpList {
		t.Fatalf("last packet is cmd %d, want %d", cmd, CmdZCCommWarpList)
	}
	if last[4] != commWarpTypes-1 {
		t.Fatalf("last commwarp type = %d, want %d", last[4], commWarpTypes-1)
	}
	// Four commwarp packets, types 0..3 in order.
	warp := 0
	for _, p := range packets {
		if binary.LittleEndian.Uint16(p) == CmdZCCommWarpList {
			if p[4] != byte(warp) {
				t.Fatalf("commwarp type %d arrived out of order", p[4])
			}
			warp++
		}
	}
	if warp != commWarpTypes {
		t.Fatalf("%d commwarp packet(s), want %d", warp, commWarpTypes)
	}
	if len(skipped) == 0 {
		t.Fatal("no skipped packet was named: the data-dependent ones must be listed")
	}
}
