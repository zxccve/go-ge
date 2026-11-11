package barrack

import "encoding/binary"

// BC_TEAMGROUP_LIST (cmd 41) - the player's saved team groups.
//
// The list is VARIABLE size: cmd(2) len(2) count(1), then `count` records of
// TEAM_GROUP. The len field is the TOTAL packet length, cmd and len included
// (Client.cpp:1592 does `out->len = bin.GetPosition()`).
//
// The record is the C struct, not a wire format of its own
// (geShared.h:388 + :367 + :146):
//
//	char name[MAX_TEAM_GROUP_NAME_LEN]   // 16 chars + NUL = 17
//	BYTE slot[MAX_PLAYER_CNT]            // 3
//
// which is 20 bytes and is confirmed by the live packet: 145 bytes in total is
// 5 + 7*20 exactly, and each record starts with a readable name.
//
// An unused member slot is EMPTY_CHARSLOT (255, geShared.h:233), never 0 -
// slot 0 is a real character.
const (
	TeamGroupNameLen = 17 // MAX_TEAM_GROUP_NAME_LEN = 16 + 1
	TeamGroupSlots   = 3  // MAX_PLAYER_CNT
	TeamGroupSize    = TeamGroupNameLen + TeamGroupSlots
	TeamGroupHeader  = 5 // cmd(2) + len(2) + count(1)

	// EmptyCharSlot is what the real server writes for "no character here"
	// (Client.cpp:1583). Zero would mean the character in slot 0.
	EmptyCharSlot = 255
)

// TeamGroup is one saved team.
type TeamGroup struct {
	Name string
	// Slots are character slots, or EmptyCharSlot. The array is fixed at three,
	// matching the client's struct - a team is at most three characters.
	Slots [TeamGroupSlots]byte
}

// BuildTeamGroupList builds cmd 41. With no groups it is 5 bytes, which is the
// honest empty form and the one the client clears its list on.
func BuildTeamGroupList(groups []TeamGroup) []byte {
	count := len(groups)
	if count > 255 {
		count = 255 // the field is a BYTE
	}

	out := make([]byte, TeamGroupHeader, TeamGroupHeader+count*TeamGroupSize)
	total := TeamGroupHeader + count*TeamGroupSize
	binary.LittleEndian.PutUint16(out[0:], 41)            // command
	binary.LittleEndian.PutUint16(out[2:], uint16(total)) // whole packet
	out[4] = byte(count)

	for _, g := range groups[:count] {
		rec := make([]byte, TeamGroupSize)
		copy(rec[0:TeamGroupNameLen], cstr(g.Name, TeamGroupNameLen))
		copy(rec[TeamGroupNameLen:], g.Slots[:])
		out = append(out, rec...)
	}
	return out
}
