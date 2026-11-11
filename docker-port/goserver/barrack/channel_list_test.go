package barrack

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestChannelListLayoutMatchesLive parses the live cmd 35 with our own constants.
// This is what proves the record size: 15 bytes = 7 + 2*4, so mapID is a WORD, not
// a DWORD (that would make the packet 19).
func TestChannelListLayoutMatchesLive(t *testing.T) {
	path := filepath.Join(replayLiveDir, "BC_CHANNEL_LIST_35.bin")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("live packet not available: %v", err)
	}

	if cmd := binary.LittleEndian.Uint16(live); cmd != 35 {
		t.Fatalf("live command = %d, want 35", cmd)
	}
	// VARIABLE packet: the length word is the whole packet.
	if got := binary.LittleEndian.Uint16(live[2:]); int(got) != len(live) {
		t.Fatalf("live length word = %d, want %d (the whole packet)", got, len(live))
	}

	count := int(live[4])
	if count != 2 {
		t.Fatalf("live count = %d, want 2", count)
	}
	if got, want := ChannelListHeader+count*ChannelInfoSize, len(live); got != want {
		t.Fatalf("7 + %d*4 = %d bytes, live packet is %d - the record is not 4 bytes",
			count, got, want)
	}

	// The two entries are the two channels of ONE map: channel 0 and channel 1.
	var ids []uint16
	for i := 0; i < count; i++ {
		off := ChannelListHeader + i*ChannelInfoSize
		ids = append(ids, binary.LittleEndian.Uint16(live[off:]))
		if got, want := live[off+2], byte(i); got != want {
			t.Fatalf("record %d channel = %d, want %d", i, got, want)
		}
	}
	if ids[0] != ids[1] {
		t.Fatalf("live records hold different map ids %d and %d; the list is per map",
			ids[0], ids[1])
	}
}

// TestChannelListOneChannel pins what we send: one entry, and the three count
// bytes all 1. A zero count here disables New Game in the client.
func TestChannelListOneChannel(t *testing.T) {
	b := BuildChannelList(ChannelsForZone(10, 0))

	if len(b) != ChannelListHeader+ChannelInfoSize {
		t.Fatalf("packet is %d bytes, want %d", len(b), ChannelListHeader+ChannelInfoSize)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 35 {
		t.Fatalf("command = %d, want 35", cmd)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != len(b) {
		t.Fatalf("length word = %d, want %d", got, len(b))
	}
	for i, name := range []string{"count", "newZoneChannelCount", "continueZoneChannelCount"} {
		if b[4+i] != 1 {
			t.Fatalf("%s = %d, want 1 (a zero count disables New Game)", name, b[4+i])
		}
	}
	if got := binary.LittleEndian.Uint16(b[ChannelListHeader:]); got != 10 {
		t.Fatalf("record map id = %d, want the zone id 10", got)
	}
	if b[ChannelListHeader+2] != 0 || b[ChannelListHeader+3] != 0 {
		t.Fatalf("record channel/traffic = %d/%d, want 0/0",
			b[ChannelListHeader+2], b[ChannelListHeader+3])
	}
}

// TestChannelListEmpty keeps the empty form honest: the header only, count 0.
func TestChannelListEmpty(t *testing.T) {
	b := BuildChannelList(nil)
	if len(b) != ChannelListHeader {
		t.Fatalf("empty packet is %d bytes, want %d", len(b), ChannelListHeader)
	}
	if b[4] != 0 || b[5] != 0 || b[6] != 0 {
		t.Fatalf("empty counts = %d/%d/%d, want 0/0/0", b[4], b[5], b[6])
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != ChannelListHeader {
		t.Fatalf("empty length word = %d, want %d", got, ChannelListHeader)
	}
}

// TestChannelListCarriesTheCommanderChannel pins the channel number: the client
// matches an entry on mapID AND channel, so a player on channel 3 must be offered
// channel 3, not channel 0.
func TestChannelListCarriesTheCommanderChannel(t *testing.T) {
	b := BuildChannelList(ChannelsForZone(10, 3))
	if got := b[ChannelListHeader+2]; got != 3 {
		t.Fatalf("record channel = %d, want 3 (the commander's channel_id)", got)
	}
}

// TestChannelListMapIDMatchesCommanderInfo is the coupling that matters: the
// client filters the channel drop-down on the map id cmd 10 sends, so both
// packets must carry the same number.
func TestChannelListMapIDMatchesCommanderInfo(t *testing.T) {
	const zone uint16 = 10 // the zone our commander is parked in

	info := BuildCommanderInfo(CommanderInfo{Name: "Arcis", SaveMapID: zone})
	// newMapID and contMapID follow the three strings + two WORDs, and the
	// commander point/ladder point before them. Read them back rather than
	// recomputing the offsets: BuildCommanderInfo is the authority.
	newMapID := binary.LittleEndian.Uint16(info[2+commanderNameLen+partyNameLen+profileLen+
		partyTitleLen+2+2+4+2:])
	if newMapID != zone {
		t.Fatalf("cmd 10 newMapID = %d, want %d", newMapID, zone)
	}

	list := BuildChannelList(ChannelsForZone(zone, 0))
	if got := binary.LittleEndian.Uint16(list[ChannelListHeader:]); got != newMapID {
		t.Fatalf("cmd 35 map id = %d, cmd 10 newMapID = %d - the client would "+
			"filter the channel out", got, newMapID)
	}
}
