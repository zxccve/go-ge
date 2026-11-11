package barrack

import "encoding/binary"

// BC_CHANNEL_LIST (cmd 35) - the barrack's channel list.
//
// THIS ONE IS NOT COSMETIC. CBarrackMode::UpdateTeamButton does
//
//	if (Session().CHANNEL_LIST.Count() == 0) {
//		enableNewGame = false;
//		enableContinueGame = false;
//	}
//
// (geClient\Source\GEBarrackMode.cpp:465), so an EMPTY list greys out New Game and
// Continue Game in the barrack. Dropping this packet is how a player ends up stuck
// at the barrack with buttons that cannot be pressed.
//
// Shape, from the shipped headers and confirmed by the live packet:
// VARIABLE size, so the length word at +2 counts the whole packet.
//
//	cmd(2) len(2) count(1) newZoneChannelCount(1) continueZoneChannelCount(1)
//	count x { WORD mapID; BYTE channel; BYTE traffic }
//
//	PKS_BC_CHANNEL_LIST -> count, newZoneChannelCount, continueZoneChannelCount
//	                       (geShared\include\geBCPacket.h)
//	CHANNEL_INFO        -> mapID, channel, traffic
//	                       (geClientShared\include\geClientShared.h)
//
// The live packet is 15 bytes = 7 + 2*4, which is what fixes the record at 4 bytes
// (it also rules out a DWORD mapID - that would be 19).
//
// mapID IS NOT FREE TO CHOOSE. CSelectTeamFrame::UpdateChannel fills the two
// channel drop-downs with Session().GetChannelInfo(i, GetNewMapType()), and
// GetChannelInfo matches on `mapID == m_commanderInfo.newMapID`. Those fields are
// the ones cmd 10 sends as newMapID / contMapID, so entries here MUST carry the
// same value or the drop-down comes out empty even though the buttons are enabled.
const (
	// ChannelListHeader is cmd(2) + len(2) + the three count bytes.
	ChannelListHeader = 7

	// ChannelInfoSize is mapID(2) + channel(1) + traffic(1).
	ChannelInfoSize = 4
)

// Channel is one entry of the list.
type Channel struct {
	// MapID must match the newMapID / contMapID that cmd 10 carries for this
	// commander, or the client's channel drop-down filters this entry away.
	MapID uint16

	// Channel is 0-based; the client displays `channel+1`.
	Channel byte

	// Traffic is the load, 0..100. The client refuses a channel move above 90
	// (script.cpp ChannelSelect).
	Traffic byte
}

// BuildChannelList builds cmd 35 for the ONE-map case. An empty list is legal and
// is what tells the client "there is nowhere to go", but it also disables New Game
// and Continue Game - so it is only right when the server really has no channel.
//
// Both zone counts become the whole list, which is right when every entry names the
// same map. A barrack that names TWO different maps (the saved start point and the
// last point) must use ChannelsForEntry + BuildChannelSet instead: here both counts
// would claim every channel belongs to both maps.
func BuildChannelList(channels []Channel) []byte {
	return BuildChannelSet(ChannelSet{
		Channels:  channels,
		NewCount:  len(channels),
		ContCount: len(channels),
	})
}

// ChannelSet is one barrack's whole cmd 35 body: the entries plus the two counts the
// client reads for its two channel drop-downs.
//
// THE TWO COUNTS ARE NOT COSMETIC, and they are not offsets into the entry list
// either. CSelectTeamFrame::UpdateChannel loops
//
//	for (i = 0; i < imc::Min(CHANNEL_LIST.Count(), m_newZoneChannelCount); ++i)
//		Session().GetChannelInfo(i, Session().GetNewMapType());
//
// (source2\geUI\source\UISelectTeamFrame.cpp:322-336) and CSession::GetChannelInfo
// treats its FIRST argument as the CHANNEL NUMBER, then scans the WHOLE list for
// `mapID == mapID && channel == channel` (source2\geClientShared\source\GeSession.cpp:
// 2384-2406). So the POSITION of an entry carries no meaning at all: what matters is
// that an entry exists for each channel number of each map, and that the count for a
// map is not larger than the channel numbers that map really has.
//
// An entry the loop cannot find is skipped with `continue` (UISelectTeamFrame.cpp:
// 328-330), so the drop-down comes out EMPTY - and then StartGame returns at
// `if (key == NULL) return;` (:551): the Continue button does nothing at all, with no
// error on screen.
type ChannelSet struct {
	Channels  []Channel
	NewCount  int // channel numbers the client may offer for the new-game map
	ContCount int // ... and for the continue map
}

// ChannelsForEntry is the list for a barrack that names maps: the map a NEW GAME
// starts on (newMapID - the saved start point) and the map CONTINUE resumes
// (contMapID - the last point). They are the same map for a commander who never
// saved, and the list is then one entry, exactly as before.
//
// One entry per map, both carrying the player's own channel number: GetChannelInfo
// matches on mapID AND channel, so a player parked on channel 1 must be offered
// channel 1 on both maps.
func ChannelsForEntry(newMapID, contMapID uint16, channel byte) ChannelSet {
	one := Channel{MapID: newMapID, Channel: channel, Traffic: 0}
	if contMapID == 0 || contMapID == newMapID {
		return ChannelSet{Channels: []Channel{one}, NewCount: 1, ContCount: 1}
	}
	return ChannelSet{
		Channels: []Channel{one, {MapID: contMapID, Channel: channel, Traffic: 0}},
		NewCount: 1,
		ContCount: 1,
	}
}

// BuildChannelSet builds cmd 35 from a set. The counts are clamped to the list, which
// is what the client does with them anyway (imc::Min against CHANNEL_LIST.Count()).
func BuildChannelSet(cs ChannelSet) []byte {
	channels := cs.Channels
	count := len(channels)
	if count > 255 {
		count = 255 // the field is a BYTE
	}
	newCount := clampByte(cs.NewCount, count)
	contCount := clampByte(cs.ContCount, count)

	total := ChannelListHeader + count*ChannelInfoSize
	out := make([]byte, ChannelListHeader, total)
	binary.LittleEndian.PutUint16(out[0:], 35)            // command
	binary.LittleEndian.PutUint16(out[2:], uint16(total)) // whole packet
	out[4] = byte(count)
	out[5] = byte(newCount)
	out[6] = byte(contCount)

	for _, c := range channels[:count] {
		rec := make([]byte, ChannelInfoSize)
		binary.LittleEndian.PutUint16(rec[0:], c.MapID)
		rec[2] = c.Channel
		rec[3] = c.Traffic
		out = append(out, rec...)
	}
	return out
}

func clampByte(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// ChannelsForZone is the list for a commander who is on `channel` of `zoneID`.
//
// This server runs exactly ONE channel per zone, so the list is one entry. The
// channel number is the commander's own `channel_id`, not a constant: the client
// matches an entry on mapID AND channel number, so a player parked on channel 1
// must be offered channel 1.
func ChannelsForZone(zoneID uint16, channel byte) []Channel {
	return []Channel{{MapID: zoneID, Channel: channel, Traffic: 0}}
}
