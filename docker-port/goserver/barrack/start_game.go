package barrack

import (
	"encoding/binary"
	"strings"
)

// BC_STARTGAMEOK (cmd 25) - "go to this zone server, load this map".
//
// This is the packet that lets the player LEAVE the barrack: the client picks a
// team, sends CB_STARTGAME (22), and waits for this. No answer means the client
// sits at the barrack forever, which is exactly what "New Game does nothing"
// looks like.
//
// Shape: FIXED 81 bytes, PKS_DEFAULT (no length word), from
// `PKS_BC_STARTGAMEOK` (geShared\include\geBCPacket.h) and confirmed by the live
// packet:
//
//	cmd(2) zoneInstID(4) zoneIP[16] zonePort(4) map[41] startPos[3] channel(1)
//	continueMission(1)
//
// **startPos is `long[3]`, i.e. three INT32 - not floats.** The header says
// `long` and the live packet proves it: it holds 3317, -1515, 250. The Python
// server packs "<fff", which only agrees because it always sends 0.0 and the
// bytes of 0.0f are four zeros. Sending a real position as floats would be read
// as garbage integers, so this builder takes int32 and says so.
const (
	// StartGameOKSize is the client's own fixed size for cmd 25.
	StartGameOKSize = 81

	zoneIPLen  = 16
	mapNameLen = 41 // MAP_NAME_LEN; fixes the packet at 2+4+16+4+41+12+1+1 = 81

	// StartGameZoneInstID is the zone instance id we hand the client. The live
	// server used a per-run instance id; the client only echoes it back in
	// CZ_START_GAME, and the original Python barrack sends 1.
	StartGameZoneInstID = 1
)

// StartGameOK is what the client needs to enter a zone.
type StartGameOK struct {
	ZoneInstID uint32
	IP         string
	Port       uint32
	MapName    string // the map's CLASS name ("reb_basic" for id 10), not a caption
	Pos        [3]int32
	Channel    byte
	// ContinueMission resumes a mission in progress. We never resume one yet.
	ContinueMission bool
}

// BuildStartGameOK builds cmd 25.
func BuildStartGameOK(s StartGameOK) []byte {
	out := make([]byte, 0, StartGameOKSize)

	out = binary.LittleEndian.AppendUint16(out, 25) // command; no length word
	out = binary.LittleEndian.AppendUint32(out, s.ZoneInstID)
	out = append(out, cstr(s.IP, zoneIPLen)...)
	out = binary.LittleEndian.AppendUint32(out, s.Port)
	out = append(out, cstr(s.MapName, mapNameLen)...)
	for _, v := range s.Pos {
		out = binary.LittleEndian.AppendUint32(out, uint32(v))
	}
	out = append(out, s.Channel)
	if s.ContinueMission {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}

	if len(out) > StartGameOKSize {
		return out[:StartGameOKSize]
	}
	return append(out, make([]byte, StartGameOKSize-len(out))...)
}

// zoneMapNames is the map CLASS name for the map ids we have data for.
//
// The client feeds this straight into SwitchMode(MT_NORMAL, name), so it loads
// the WORLD named here. A single hardcoded name for everybody was a real bug once
// (a character saved in another map could never be entered), so unknown ids are
// logged loudly rather than silently given "reb_basic".
//
// The full table is docker-port\data\map_table.xml (decoded from the client's
// datatable_map), and the Python world module reads it. Growth path: read that
// file at startup instead of extending this map.
var zoneMapNames = map[uint16]string{
	10: "reb_basic",
}

// ZoneMapName answers the map class name for a map id, and whether it is known.
func ZoneMapName(zoneID uint16) (string, bool) {
	name, ok := zoneMapNames[zoneID]
	return name, ok
}

// ParseZoneAddr splits "host:port" into the two fields cmd 25 wants. A malformed
// address is reported so the caller can fall back loudly instead of quietly
// pointing the client at nowhere.
func ParseZoneAddr(addr string) (string, uint32, bool) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", 0, false
	}
	port := 0
	for _, c := range addr[i+1:] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		port = port*10 + int(c-'0')
		if port > 65535 {
			return "", 0, false
		}
	}
	return addr[:i], uint32(port), true
}
