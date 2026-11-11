package barrack

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestPresetRemainCountLayoutMatchesLive parses the live cmd 65 and compares it
// with our builder field by field. This is the pin for the whole layout: ids,
// count and the length word all come from the capture, not from a header.
//
// Only the three VALUES may differ - we send 0 because this server has no item
// layer yet, and the capture's account owned 8656 Growth Stones.
func TestPresetRemainCountLayoutMatchesLive(t *testing.T) {
	path := filepath.Join(replayLiveDir,
		"BC_EQUIP_ITEM_PRESET_ADD_ITEM_REMAIN_COUNT_65.bin")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("live packet not available: %v", err)
	}
	if len(live) != PresetRemainCountSize {
		t.Fatalf("live packet is %d bytes, PresetRemainCountSize says %d",
			len(live), PresetRemainCountSize)
	}
	if cmd := binary.LittleEndian.Uint16(live); cmd != 65 {
		t.Fatalf("live command = %d, want 65", cmd)
	}
	// VARIABLE packet: the length word is the whole packet.
	if got := binary.LittleEndian.Uint16(live[2:]); int(got) != len(live) {
		t.Fatalf("live length word = %d, want %d (the whole packet)", got, len(live))
	}

	count := int(binary.LittleEndian.Uint32(live[4:]))
	if count != 3 {
		t.Fatalf("live count = %d, want 3", count)
	}
	if got, want := presetRemainHeader+count*presetRemainRecord, len(live); got != want {
		t.Fatalf("layout arithmetic gives %d bytes, live packet is %d", got, want)
	}

	// The ids must be the client's own ExtendItemID list, in this order.
	wantIds := []uint32{ItemPresetDrillEvent, ItemPresetDrillTrade, ItemGrowthStone}
	items := PresetExtendItems()
	ours := BuildPresetRemainCount(items)

	if len(items) != count {
		t.Fatalf("we send %d item(s), the capture has %d", len(items), count)
	}
	for i, want := range wantIds {
		off := presetRemainHeader + i*presetRemainRecord
		if got := binary.LittleEndian.Uint32(live[off:]); got != want {
			t.Fatalf("record %d: live item id = %d, want %d", i, got, want)
		}
		if got := binary.LittleEndian.Uint32(ours[off:]); got != want {
			t.Fatalf("record %d: our item id = %d, want %d", i, got, want)
		}
		if got := binary.LittleEndian.Uint32(ours[off+4:]); got != items[i].Value {
			t.Fatalf("record %d: our value = %d, want %d", i, got, items[i].Value)
		}
	}

	// Everything except the three value fields must be byte-identical.
	for i := range live {
		if live[i] == ours[i] {
			continue
		}
		if inValueField(i) {
			continue
		}
		t.Fatalf("byte %d differs (live 0x%02x, ours 0x%02x) and it is not one of "+
			"the three value fields", i, live[i], ours[i])
	}

	// The one difference we expect: the capture account owned Growth Stones.
	if got := binary.LittleEndian.Uint32(live[presetRemainHeader+2*presetRemainRecord+4:]); got == 0 {
		t.Fatalf("capture value for GrowthStone is 0 - the fixture changed, re-read it")
	}
}

// inValueField reports whether an offset holds one of the three 4-byte values
// (the second word of each record), i.e. the only bytes a probe may change.
func inValueField(off int) bool {
	if off < presetRemainHeader {
		return false
	}
	return (off-presetRemainHeader)%presetRemainRecord >= 4
}

// TestPresetRemainCountEmpty keeps the builder honest for no items: just the
// header, with a length word of 8.
func TestPresetRemainCountEmpty(t *testing.T) {
	b := BuildPresetRemainCount(nil)
	if !bytes.Equal(b, []byte{65, 0, 8, 0, 0, 0, 0, 0}) {
		t.Fatalf("empty packet = % x, want 41 00 08 00 00 00 00 00", b)
	}
}
