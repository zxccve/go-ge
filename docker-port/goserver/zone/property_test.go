package zone

import (
	"encoding/binary"
	"testing"
)

// TestUpdatePropertyNumberLayout pins the 46-byte FIXED packet that sets a value
// in the client's own property cache. It has NO length word: +2 starts the
// idSpace. Reading a length there would take "Lo" as a size and hang the stream.
func TestUpdatePropertyNumberLayout(t *testing.T) {
	pkt := BuildUpdatePropertyNumber("Lock_InventoryCount", 283)

	if len(pkt) != 46 {
		t.Fatalf("%d bytes, want 46 (2 + idSpace[20] + property[20] + value[4])", len(pkt))
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCUpdatePropertyNum {
		t.Fatalf("command = %d, want %d", cmd, CmdZCUpdatePropertyNum)
	}
	if got := trimNul(pkt[2:22]); got != "Lock" {
		t.Fatalf("idSpace = %q, want Lock", got)
	}
	if got := trimNul(pkt[22:42]); got != "Lock_InventoryCount" {
		t.Fatalf("property = %q, want Lock_InventoryCount", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[42:]); got != 283 {
		t.Fatalf("value = %d, want 283", got)
	}
}

// TestPropertyPacketReachesTheBurst proves the wiring: a Lock_* property must add
// one 3748 packet to the entry burst, right after the weight and before the
// inventory sub-lists, which is where the reference server puts it.
func TestPropertyPacketReachesTheBurst(t *testing.T) {
	props := []Property{{Name: "Lock_InventoryCount", Value: 283}}
	packets, skipped := EntryBurst(props, nil)
	weightAt, propAt, sockAt := -1, -1, -1
	for i, p := range packets {
		switch binary.LittleEndian.Uint16(p) {
		case CmdZCChangeMaxWeight:
			weightAt = i
		case CmdZCUpdatePropertyNum:
			propAt = i
		case CmdZCInventorySocket:
			sockAt = i
		}
	}
	if propAt < 0 {
		t.Fatal("the property packet never reached the burst")
	}
	if !(weightAt < propAt && propAt < sockAt) {
		t.Fatalf("property packet at %d, want it between the weight (%d) and the "+
			"socket list (%d)", propAt, weightAt, sockAt)
	}
	// With a property present the "no Lock_* row" note must NOT be there.
	for _, s := range skipped {
		if len(s) > 8 && s[:4] == "3748" {
			t.Fatalf("burst still claims 3748 is skipped: %q", s)
		}
	}
}
