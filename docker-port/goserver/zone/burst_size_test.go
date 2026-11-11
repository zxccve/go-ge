package zone

import (
	"encoding/binary"
	"testing"
)

// TestBurstSizesMatchTheReference pins every packet size. These numbers come from
// the Python builders, which measured them against the live client; a "tidy up"
// that changes one is a misparse in the client, not a style choice.
//
// `fixed` marks the packets with NO length word: their bytes at +2 are PAYLOAD,
// not a size. The commander point's +2 is the point id and the weight packet's +2
// is the limit. Reading those as lengths is the exact trap that misaligns a stream.
func TestBurstSizesMatchTheReference(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pkt   []byte
		want  int
		cmd   uint16
		fixed bool
	}{
		{"commander point", BuildCommanderPoint(0), 6, CmdZCCommanderPoint, true},
		{"max weight", BuildMaxWeight(maxWeightDefault), 6, CmdZCChangeMaxWeight, true},
		{"quest info (no quests)", BuildQuestInfo(nil), 4, CmdZCQuestInfo, false},
		{"item list (empty)", BuildInventoryItemList(nil), 12, CmdZCInventoryItemList, false},
		{"option list (empty)", BuildInventoryOptionList(nil), 12, CmdZCInventoryOption, false},
		{"reinforce list (empty)", BuildReinforceList(nil), 12, CmdZCReinforceList, false},
		{"socket list", BuildInventorySocketList(), 12, CmdZCInventorySocket, false},
		{"property list (empty)", BuildInventoryPropertyList(nil), 13, CmdZCInventoryProp, false},
		{"commander init", BuildCommanderInitInfo(), 15, CmdZCCommanderInit, false},
		{"pet list", BuildPetList(), 6, CmdZCPetList, false},
		{"commwarp 0", BuildCommWarpList(0), 6, CmdZCCommWarpList, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.pkt) != tc.want {
				t.Fatalf("%d bytes, want %d", len(tc.pkt), tc.want)
			}
			if cmd := binary.LittleEndian.Uint16(tc.pkt); cmd != tc.cmd {
				t.Fatalf("command = %d, want %d", cmd, tc.cmd)
			}
			if tc.fixed {
				return // no length word to check
			}
			// Every packet in this group that DOES carry a length word must agree
			// with its own size: the client frames it from that word.
			if size := binary.LittleEndian.Uint16(tc.pkt[2:]); int(size) != tc.want {
				t.Fatalf("length word = %d, want %d (it counts the whole packet)",
					size, tc.want)
			}
		})
	}
}

// TestMaxWeightIsAPayloadNotALength proves the FIXED shape: +2 holds 50000, which
// is far larger than the packet. Reading it as a length would hang the stream.
func TestMaxWeightIsAPayloadNotALength(t *testing.T) {
	pkt := BuildMaxWeight(maxWeightDefault)
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != maxWeightDefault {
		t.Fatalf("weight = %d, want %d", got, maxWeightDefault)
	}
	if int(binary.LittleEndian.Uint32(pkt[2:])) == len(pkt) {
		t.Fatal("weight happens to equal the packet size - the test cannot prove anything")
	}
}
