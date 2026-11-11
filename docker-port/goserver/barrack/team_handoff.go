package barrack

import "encoding/binary"

// Reading CB_STARTGAME / CB_CONTINUEGAME, the "send me to the zone" request.
//
// The picked team is written to PostgreSQL (table pending_zone_entry) instead of
// the out\zone_team_<id>.json file the Python barrack still writes. A row keyed by
// commander is the same information without a file, and it cannot be clobbered by
// another player entering at the same moment.
//
// The zone reads that row while it is still Python - a BRIDGE, deleted when the
// zone is ported (DECISIONS.md).
const (
	// StartGameHeaderLen is PKS_SECURITY_DEFAULT: cmd(2) + seq(4) + checksum(4).
	StartGameHeaderLen = 10
)

// ParseStartGame reads the channel and the three character slots out of a
// CB_STARTGAME/CB_CONTINUEGAME body.
//
// Layout (PKS_SECURITY_DEFAULT + CHANNEL_ID + WORD charSlotID[3], geBCPacket.h):
//
//	cmd(2) seq(4) checksum(4) channel(2) slot[0](2) slot[1](2) slot[2](2)
//
// so the slots start at +12, and a slot equal to EmptyCharSlot (255, defined in
// team_group.go) is unused.
func ParseStartGame(data []byte) (channel uint16, slots [3]uint16, ok bool) {
	if len(data) < StartGameHeaderLen+2+6 {
		return 0, slots, false
	}
	channel = binary.LittleEndian.Uint16(data[StartGameHeaderLen:])
	for i := 0; i < 3; i++ {
		slots[i] = binary.LittleEndian.Uint16(data[StartGameHeaderLen+2+i*2:])
	}
	return channel, slots, true
}

// pickedSlots lists the slots that actually hold a character.
func pickedSlots(slots [3]uint16) []uint16 {
	out := make([]uint16, 0, len(slots))
	for _, s := range slots {
		if s != EmptyCharSlot {
			out = append(out, s)
		}
	}
	return out
}
