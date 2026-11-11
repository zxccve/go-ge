package barrack

import "encoding/binary"

// BC_COMMANDER_INFO (cmd 10, 744 bytes) - the packet that describes the
// commander standing in the barrack.
//
// Ported field by field from the working Python builder
// (src\ge_barrack_server.py, build_commander_info), so the layout is not a
// guess. The comments that matter are kept: two fields are DERIVED, and one
// must be a real map id or the client dereferences a null.
const (
	// CommanderInfoSize is the client's own table size for cmd 10.
	CommanderInfoSize = 744

	commanderNameLen   = 17
	partyNameLen       = 17
	profileLen         = 251
	partyTitleLen      = 9
	maxBarrackChannel  = 24
	barrackNameLen     = 17
	barrackNameDefault = "None"
)

// CommanderInfo is what the barrack screen needs about one commander.
type CommanderInfo struct {
	Name string

	// Rank is the MAX TEAM GROUP COUNT, not a rank number, and CharSlot is the
	// commander's Lock_CharSlot property. The original derives both; hardcoding
	// them sends values the original never sends.
	//
	// Rank is the CAP, never the commander's current Lock_TeamGroup: the client's
	// Extend guard is `Lock_TeamGroup < rank` (proven at client 0x005426c1,
	// history\barrack.md "2026-11-08 - the Extend gate"), so equal numbers close
	// the button forever. The handler passes GET_TEAMGROUP_MAXSLOT() here.
	Rank     uint16
	CharSlot uint16

	// SaveMapID is the map the NEW GAME button enters at: the player's SAVED START
	// POINT (`commander.point`, a restart warp id, resolved to its map). It goes out
	// as cmd 10's newMapID and it must be a REAL map class id: the client does
	// `geMapTable::Get(packet->newMapID)->name` with NO null check
	// (source2\geClient\Source\GEBarrackNet.cpp:230 and :234-235).
	SaveMapID uint16

	// LastMapID is the map the CONTINUE button resumes: the LAST POINT, i.e.
	// `commander.zone_id`. It goes out as contMapID, and the client null-checks
	// this one (`if (NULL != pContinueMapProperty)`, GEBarrackNet.cpp:236).
	//
	// WHY TWO FIELDS AT ALL: they are two different facts. A log-out in Reboldoeux
	// must not erase a Coimbra save. When no start point is saved, or its warp id
	// resolves to no map, the caller sends the last point's map here too and the
	// barrack names one map, exactly as before the save point existed.
	LastMapID uint16

	// BarrackNames replace the captions of the 24 barrack buttons. An empty
	// slot sends "None", which makes the client keep its own localised caption
	// ("Barrack 1" ... "Barrack 24"). Sending "" blanks the whole grid.
	BarrackNames [maxBarrackChannel]string
}

// BuildCommanderInfo builds the packet. The trailing fields the client's struct
// has but we do not set are left zero, which is what the original sends.
func BuildCommanderInfo(ci CommanderInfo) []byte {
	return buildCommanderInfo(ci, maxBarrackChannel)
}

// BuildCommanderInfoChannels builds the same packet for a client whose barrack
// has a different channel count. The JP 375639 build has 32 channels (measured
// in history\barrack.md: 32 x 9 = 288 slots), and its cmd 10 is 744 + 8*17 =
// 880 bytes - the extra eight 17-byte barrack names sit where the 24-name
// table ends. Sending the 744-byte 2020 shape desynchronises its table and the
// client sits on the loading screen (measured 2026-11-06).
func BuildCommanderInfoChannels(ci CommanderInfo, channels int) []byte {
	if channels < maxBarrackChannel {
		channels = maxBarrackChannel
	}
	return buildCommanderInfo(ci, channels)
}

func buildCommanderInfo(ci CommanderInfo, channels int) []byte {
	size := CommanderInfoSize + (channels-maxBarrackChannel)*barrackNameLen
	out := make([]byte, 0, size)

	put16 := func(v uint16) { out = binary.LittleEndian.AppendUint16(out, v) }
	put32 := func(v uint32) { out = binary.LittleEndian.AppendUint32(out, v) }
	put8 := func(v byte) { out = append(out, v) }
	putStr := func(s string, n int) { out = append(out, cstr(s, n)...) }

	put16(10) // command
	putStr(ci.Name, commanderNameLen)
	putStr("", partyNameLen)
	putStr("", profileLen)
	putStr("", partyTitleLen)
	put16(ci.Rank)
	put16(ci.CharSlot)
	put32(0) // commanderPoint
	put16(0) // ladderPoint
	// TWO MAPS, TWO FACTS. newMapID is where a new game starts (the SAVED start
	// point); contMapID is the Continue button's map (the LAST point). Sending one
	// number twice is what made a Coimbra save read as Reboldoeux after a log-out.
	//
	// newMapID is NEVER sent as 0: the client dereferences its map with no null
	// check, and 0 is not a map. With no save point the last point's map is used.
	newMapID := ci.SaveMapID
	if newMapID == 0 {
		newMapID = ci.LastMapID
	}
	contMapID := ci.LastMapID
	if contMapID == 0 {
		contMapID = newMapID
	}
	put16(newMapID)  // newMapID
	put16(contMapID) // contMapID
	put16(0)         // continueMissionMapID
	put16(0)         // commLvMax
	put8(0)          // commLvPercent
	put16(0)         // commLvTotal
	put16(0)         // commLvCurrent
	put32(0)         // mccTime
	put32(0)         // mccCashTime

	for j := 0; j < channels; j++ {
		name := ""
		if j < len(ci.BarrackNames) {
			name = ci.BarrackNames[j]
		}
		if name == "" {
			name = barrackNameDefault
		}
		putStr(name, barrackNameLen)
	}
	put8(0) // WorldPVPClass
	put8(0) // MeClique

	if len(out) > size {
		return out[:size]
	}
	return append(out, make([]byte, size-len(out))...)
}

// cstr builds a fixed-width, NUL-terminated byte field: truncate to n-1 and pad
// with zeros. This is the rule every text field in this protocol uses.
func cstr(s string, n int) []byte {
	b := make([]byte, n)
	if len(s) > n-1 {
		s = s[:n-1]
	}
	copy(b, s)
	return b
}
