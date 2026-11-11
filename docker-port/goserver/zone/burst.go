package zone

import (
	"encoding/binary"
	"fmt"
	"strings"

	"geserver/item"
)

// The ENTRY BURST: what the server sends right after ZC_START_GAMEOK.
//
// The Python server sends ~19 packets here. This file sends the ones that need no
// game data - empty lists with correct headers - and leaves the data-dependent
// ones out, logged by name so the gap is visible. An empty list is not the same as
// a missing packet: a missing one leaves the client waiting or drawing a stale
// panel, while an empty one tells it "there is nothing", which is true.
//
// Every layout below was measured from src\zone_server.py; the size asserts the
// Python code carries are reproduced as tests here.
const (
	CmdZCCommanderPoint    = 3744
	CmdZCQuestInfo         = 3872
	CmdZCInventoryItemList = 3601
	CmdZCChangeMaxWeight   = 3143
	CmdZCInventoryOption   = 3603
	CmdZCReinforceList     = 3855
	CmdZCInventorySocket   = 3604
	CmdZCInventoryProp     = 3606
	CmdZCCommanderInit     = 3184
	CmdZCPetList           = 3199
	CmdZCCommWarpList      = 3634
	CmdZCUpdatePropertyNum = 3748
	CmdZCSpawnTrigger      = 3543

	// invTypeInventory is ILT_INVENTORY: the list that is the player's bag.
	invTypeInventory = 0
	// maxWeightDefault is the game's own Lock_WeightLimit default (50000).
	maxWeightDefault = 50000
	// commWarpTypes is MAX_TELEPORT_TYPE: the client expects one list per type, and it
	// draws one panel per type - the basic "Warp List" and "Warp Extend(1)".."(3)".
	commWarpTypes = SavedTeleportLists
	// commWarpOwnList is teltype 0: the FIRST panel - the basic "Warp List". Since R76
	// slice 6 it is only the first of four lists this server fills; the other three are
	// served from `saved_teleport` too (migration 00012), so this is no longer "the only
	// list" and nothing refuses a save into 1-3.
	commWarpOwnList = 0

	// commWarpHeader is PKS_VARSIZE: cmd(2) + len(2), and `len` is the WHOLE packet.
	commWarpHeader = 4
	// commWarpPrefix is PKS_ZC_COMMWARP_LIST's own two fields, teltype and count,
	// one byte each - so the first record starts at +6.
	commWarpPrefix   = 2
	commWarpRecordsAt = commWarpHeader + commWarpPrefix
	// commWarpRecord is the shipped client's FLAT COPY stride, 46 = 0x2e:
	// `0x4b6d65 PUSH 0x2e` (the copy size) and `0x4b6d74 ADD ESI,0x2e` (the step).
	// source2's WARP_INFO says 45 with a 31-byte memo; the binary copies 46.
	commWarpRecord = 46
	// commWarpMapID is the map id inside a record, as a WORD.
	commWarpMapID = 12
	// commWarpMemo is the memo inside a record: 32 bytes, NUL padded. 46 = 14 + 32
	// leaves no spare byte, which is also why this window has no fare field.
	commWarpMemo     = 14
	commWarpMemoSize = 32
	// commWarpMaxRows is what the one-byte count can describe.
	commWarpMaxRows = 255
)

// EntryBurst returns the post-entry packets, in the order the reference server
// sends them, with the Warp List (`warplist`) left empty. Missing (data-dependent)
// ones are named in Skipped instead of being silently absent. `items` is the bag's
// 24-byte records, oldest first (see BuildInventoryRecords); nil is an empty bag.
//
// It is the shape every caller with no saved points uses; the zone's own entry goes
// through EntryBurstWithSavedTeleports.
func EntryBurst(props []Property, items []byte) (packets [][]byte, skipped []string) {
	return EntryBurstWithSavedTeleports(props, items, nil)
}

// EntryBurstWithSavedTeleports is EntryBurst plus the player's saved teleport points,
// which ride the 3634 that fills the Warp List window: `saved` is the commander's WHOLE
// set (all four lists, each row carrying its TelType) and CommWarpPackets splits it into
// one packet per list. A list with no rows stays the byte-identical 6-byte empty packet.
func EntryBurstWithSavedTeleports(props []Property, items []byte, saved []SavedPoint) (packets [][]byte, skipped []string) {
	// The Warp List's cap is the SERVER's number and it is TOLD to the client on
	// every entry, whether or not a `commander_property` row exists (a stale row
	// cannot win). `noPropertyRows` is captured first because the note below is
	// about the rows the database did NOT have, and the cap adds one.
	noPropertyRows := len(props) == 0
	props, _ = PropsWithWarpCap(props)

	packets = [][]byte{
		BuildCommanderPoint(0),        // 3744: the commander's field point, 0 = none
		BuildQuestInfo(nil),           // 3872: no quest state yet (one byte per quest)
		BuildInventoryItemList(items), // 3601
		BuildMaxWeight(maxWeightDefault),
	}
	// One 46-byte property number per Lock_* row, where the reference server puts
	// them: after the weight, before the inventory sub-lists. `PropsWithWarpCap` has
	// already forced `Lock_Warp` in, so the Warp List's own cap is always one of them.
	for _, p := range props {
		packets = append(packets, BuildUpdatePropertyNumber(p.Name, p.Value))
	}
	packets = append(packets,
		BuildInventoryOptionList(nil),   // 3603
		BuildReinforceList(nil),         // 3855
		BuildInventorySocketList(),      // 3604
		BuildInventoryPropertyList(nil), // 3606
		BuildCommanderInitInfo(),        // 3184
		BuildPetList(),                  // 3199
	)
	packets = append(packets, CommWarpPackets(saved)...)
	skipped = []string{
		"3747 ZC_UPDATE_TABLE (needs the Lock_* property XML; without it the client " +
			"draws its table defaults, which is what it did before)",
	}
	if noPropertyRows {
		skipped = append(skipped,
			"3748 ZC_UPDATE_PROPERTY_NUMBER for the OTHER Lock_* rows (no such row in "+
				"commander_property, so the client keeps its table default - 248 bag "+
				"slots from Lock_InventoryCount)")
	}
	skipped = append(skipped,
		"3097/3099 inventory + equip CASH lists",
		"3344/3347 LUMIN cash lists",
		"3874 ZC_QUEST_STAGE_INFO",
	)
	return packets, skipped
}

// lockWarpProperty is the commander property the ORIGINAL server read for its Warp
// List cap (`if (teltype == 0) maxCount = GetLock_Warp();`, Commander.cpp:4895).
//
// THE SHIPPED CLIENT HAS NO READER FOR IT, which was measured the hard way in R76
// slice 3: `ge.exe` contains the strings `Lock_MCCLv` and `Lock_InventoryCount` but
// NOT `Lock_Warp`, and the only two places `Lock_Warp` appears in source2 are the
// original SERVER (Commander.cpp/h) and the UI TEST program's property XML default
// (`Tests\geUITest\WinMain.cpp:312`, `Lock_Warp = "5"`). So the 3748 below is a
// statement of the server's cap on the same wire the original used, not something
// that redraws a counter: what makes the owner's window show ten is the ten 3634
// rows the server now accepts and sends (the window draws what it is sent and
// clamps nothing, `UIWarpListFrame.cpp:47-64`).
const lockWarpProperty = "Lock_Warp"

// PropsWithWarpCap returns the property rows the burst will send, with `Lock_Warp`
// forced to MaxSavedTeleports - the SAME constant the store enforces.
//
// WHY FORCED AND NOT READ FROM THE DATABASE. The cap the server applies and the cap
// it states have to be one number, or a stale `commander_property` row (editable, and
// a leftover of the cash-item system this rebuild does not have) would promise a size
// the server does not honour. So the CAP wins, and the stale row is returned so the
// caller can log the disagreement once, naming both numbers.
//
// The row keeps its alphabetical place, which is the order LockProperties returns
// (ORDER BY property) and the order the other rows were always sent in. A second
// `Lock_Warp` row is dropped: exactly ONE such packet goes out.
//
// stale is nil when there was no row, or the row already agreed.
func PropsWithWarpCap(props []Property) (out []Property, stale *Property) {
	want := Property{Name: lockWarpProperty, Value: MaxSavedTeleports}
	out = make([]Property, 0, len(props)+1)
	placed := false
	for _, p := range props {
		if p.Name != lockWarpProperty {
			out = append(out, p)
			continue
		}
		if placed {
			continue
		}
		placed = true
		if p.Value != want.Value {
			old := p
			stale = &old
		}
		out = append(out, want)
	}
	if placed {
		return out, stale
	}
	at := len(out)
	for i, p := range out {
		if p.Name > lockWarpProperty {
			at = i
			break
		}
	}
	out = append(out, Property{})
	copy(out[at+1:], out[at:])
	out[at] = want
	return out, nil
}

// WarpCapWarning is the ONE line the zone logs when a `commander_property` row for
// `Lock_Warp` disagrees with this server's cap. It names BOTH numbers so the row can
// be cleaned up, and it says which one won, because the reader's next question is
// "which list size did the window draw?".
func WarpCapWarning(stale Property) string {
	return fmt.Sprintf("zone: commander_property says %s=%d but this server's Warp List "+
		"holds %d, so the CAP wins and %d is what the client was told (the row itself "+
		"was not changed)", stale.Name, stale.Value, MaxSavedTeleports, MaxSavedTeleports)
}

// BuildUpdatePropertyNumber is the 46-byte FIXED packet that sets one value in
// the client's own property cache:
//
//	cmd(2) idSpace[20] property[20] value(4)
//
// It has NO length word. The idSpace is "Lock" for Lock_* properties, and the
// client redraws the window that property drives (`Lock_InventoryCount` ->
// "inventory"), which is why a database write alone is invisible on screen.
func BuildUpdatePropertyNumber(property string, value uint32) []byte {
	out := make([]byte, 46)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUpdatePropertyNum)
	putCStr(out[2:22], "Lock")
	putCStr(out[22:42], property)
	binary.LittleEndian.PutUint32(out[42:], value)
	return out
}

// BuildCommanderPoint is 6 bytes: cmd + the point id. 0 means "no point set".
func BuildCommanderPoint(point uint32) []byte {
	out := make([]byte, 6)
	binary.LittleEndian.PutUint16(out[0:], CmdZCCommanderPoint)
	binary.LittleEndian.PutUint32(out[2:], point)
	return out
}

// BuildMaxWeight is 6 bytes: cmd + the bag's weight limit.
func BuildMaxWeight(weight uint32) []byte {
	out := make([]byte, 6)
	binary.LittleEndian.PutUint16(out[0:], CmdZCChangeMaxWeight)
	binary.LittleEndian.PutUint32(out[2:], weight)
	return out
}

// BuildQuestInfo carries ONE BYTE per quest the client knows about, and the length
// word counts the whole packet. With no quest state that is 4 bytes.
func BuildQuestInfo(states []byte) []byte {
	out := make([]byte, 4, 4+len(states))
	binary.LittleEndian.PutUint16(out[0:], CmdZCQuestInfo)
	binary.LittleEndian.PutUint16(out[2:], uint16(4+len(states)))
	return append(out, states...)
}

// BuildInventoryItemList: cmd + len + count + type + records of 24 bytes.
//
// The record stride is NOT free: the client's walker reads a DWORD at +16 and one
// at +20, so 24 bytes is the shape. Empty is 12 bytes and that is exactly what the
// Python server sends for a player with nothing in the bag.
func BuildInventoryItemList(items []byte) []byte {
	body := make([]byte, 8) // count(4) + type(4)
	binary.LittleEndian.PutUint32(body[0:], uint32(len(items)/24))
	binary.LittleEndian.PutUint32(body[4:], invTypeInventory)

	out := make([]byte, 4, 4+len(body)+len(items))
	binary.LittleEndian.PutUint16(out[0:], CmdZCInventoryItemList)
	out = append(out, body...)
	out = append(out, items...)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// BuildInventoryRecords turns bag rows into the 24-byte records 3601 carries.
//
// Layout PROVEN from the client's own walker `FUN_007aa28c` (stride `ADD EDI,0x18`)
// and the crash that followed 16-byte records: DWORD type@0 (the item-table key),
// DWORD count@4, DWORD invIndex@8, BYTE@12, DWORD@16, DWORD@20. The first four are
// source2's {type, count, invIndex, protect}; the trailing 8 were appended in the
// shipped build and stay zero. invIndex runs 1..N in BAG ORDER, which is why the
// rows must arrive oldest first.
func BuildInventoryRecords(stacks []item.Stack) []byte {
	out := make([]byte, 0, len(stacks)*24)
	for i, st := range stacks {
		rec := make([]byte, 24)
		binary.LittleEndian.PutUint32(rec[0:], uint32(st.ClassID))
		binary.LittleEndian.PutUint32(rec[4:], uint32(st.Count))
		binary.LittleEndian.PutUint32(rec[8:], uint32(i+1))
		out = append(out, rec...)
	}
	return out
}

// BuildInventoryOptionList: cmd + len + count + type + 12-byte records. Empty = 12.
func BuildInventoryOptionList(records []byte) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:], uint32(len(records)/12))
	binary.LittleEndian.PutUint32(body[4:], invTypeInventory)

	out := make([]byte, 4, 4+len(body)+len(records))
	binary.LittleEndian.PutUint16(out[0:], CmdZCInventoryOption)
	out = append(out, body...)
	out = append(out, records...)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// BuildReinforceList: cmd + len + TYPE + COUNT + 5-byte records.
//
// The order is type-then-count here, the opposite of every other list in this
// group. Getting it the "consistent" way round makes the client read the type as a
// count, which is a silent misparse, not a crash.
func BuildReinforceList(records []byte) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:], invTypeInventory) // type
	binary.LittleEndian.PutUint32(body[4:], uint32(len(records)/5))

	out := make([]byte, 4, 4+len(body)+len(records))
	binary.LittleEndian.PutUint16(out[0:], CmdZCReinforceList)
	out = append(out, body...)
	out = append(out, records...)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// BuildInventorySocketList is the 12-byte empty shape: cmd, len 12, count 0, type 0.
func BuildInventorySocketList() []byte {
	out := make([]byte, 12)
	binary.LittleEndian.PutUint16(out[0:], CmdZCInventorySocket)
	binary.LittleEndian.PutUint16(out[2:], 12)
	return out
}

// BuildInventoryPropertyList: cmd + len + count + type + a FLAG BYTE + records.
//
// The header is 13 bytes, not 12, because of that flag. Dropping it shifts every
// record by one byte and the client crashes in CObject::Extract (Python note,
// 2026-10-19). Empty = 13 bytes.
func BuildInventoryPropertyList(records []byte) []byte {
	body := make([]byte, 9) // count(4) + type(4) + flag(1)
	binary.LittleEndian.PutUint32(body[4:], invTypeInventory)
	body[8] = 0 // 0 = keep the properties, 1 = clear them

	out := make([]byte, 4, 4+len(body)+len(records))
	binary.LittleEndian.PutUint16(out[0:], CmdZCInventoryProp)
	out = append(out, body...)
	out = append(out, records...)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// BuildCommanderInitInfo is 15 bytes of zeros: cmd, len 15, seven bytes of
// defaults, WP_GLOBAL_PROP, two clique-war bytes and VarConfCount.
func BuildCommanderInitInfo() []byte {
	out := make([]byte, 15)
	binary.LittleEndian.PutUint16(out[0:], CmdZCCommanderInit)
	binary.LittleEndian.PutUint16(out[2:], 15)
	return out
}

// BuildPetList is 6 bytes with no pets.
func BuildPetList() []byte {
	out := make([]byte, 6)
	binary.LittleEndian.PutUint16(out[0:], CmdZCPetList)
	binary.LittleEndian.PutUint16(out[2:], 6)
	return out
}

// BuildCommWarpList is the EMPTY list for one teleport TYPE (0..3): cmd, len 6,
// type, count 0. It is exactly what it has always been; a type with no rows must
// stay byte-identical or the entry burst changes shape.
func BuildCommWarpList(telType int) []byte {
	return BuildCommWarpListRows(telType, nil)
}

// BuildCommWarpListRows is ZC_COMMWARP_LIST (3634) for one teleport type:
//
//	cmd(2) len(2) teltype(1) count(1) then `count` records of 46 bytes:
//	  x i32 @+0, y i32 @+4, cell i32 @+8, mapId u16 @+12, memo char[32] @+14
//
// MEASURED FROM THE SHIPPED BINARY, which disagrees with source2 twice: the records
// start at +6 (not +4) and the stride is 46 with a 32-byte memo (source2 says 45 and
// 31). The record's third int is the navmesh CELL, never a height - the same
// {x, y, cell} triple every other position in this server uses.
//
// `len` is the TOTAL packet, header included: 6 + 46*count. The client takes the
// count from the byte at +5, so a wrong length word is invisible on screen until
// the count byte and the length disagree.
//
// The memo is the row's `name`, NUL padded and nothing else: the client draws
// "(n) memo", or "(n) <map name>" when the memo is empty, so an empty memo still
// draws a usable row. It is capped at commWarpMemoSize-1 characters so the NUL
// always fits.
//
// More than 255 rows cannot be described by the one-byte count, so the extra ones
// are dropped here rather than wrapping the byte. A commander has ten slots, so
// this is a guard, not a behaviour.
func BuildCommWarpListRows(telType int, rows []SavedPoint) []byte {
	if len(rows) > commWarpMaxRows {
		rows = rows[:commWarpMaxRows]
	}
	out := make([]byte, commWarpRecordsAt+commWarpRecord*len(rows))
	binary.LittleEndian.PutUint16(out[0:], CmdZCCommWarpList)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	out[4] = byte(telType)
	out[5] = byte(len(rows))
	for i, p := range rows {
		at := commWarpRecordsAt + i*commWarpRecord
		binary.LittleEndian.PutUint32(out[at:], uint32(p.Pos[0]))
		binary.LittleEndian.PutUint32(out[at+4:], uint32(p.Pos[1]))
		binary.LittleEndian.PutUint32(out[at+8:], uint32(p.Pos[2]))
		binary.LittleEndian.PutUint16(out[at+commWarpMapID:], uint16(p.MapID))
		putCStr(out[at+commWarpMemo:at+commWarpMemo+commWarpMemoSize], p.Name)
	}
	return out
}

// CommWarpPackets is the whole 3634 send: one packet per teleport list, in type order,
// each carrying THAT list's rows in their own display order. FOUR lists of ten, all
// free - this is what makes forty points one feature (R76 slice 6). A list the commander
// has never saved into goes out as the byte-identical 6-byte empty packet, so an empty
// panel is unchanged from the build before slice 6.
//
// Four packets every time, and only one packet per list: the client REPLACES
// `COMM_WARP_LIST[teltype]` and redraws the window on each one, so a type left out of a
// send keeps whatever it held before.
//
// This is also the ONLY confirmation this window understands: the client has no
// receive handler for 3635 / 3636 / 3638, so a save, a remove and a rename answer with
// these four (a save adds one ZC_MESSAGE line on top - see commwarp.go).
func CommWarpPackets(saved []SavedPoint) [][]byte {
	out := make([][]byte, 0, commWarpTypes)
	for telType := 0; telType < commWarpTypes; telType++ {
		out = append(out, BuildCommWarpListRows(telType, rowsOfType(saved, telType)))
	}
	return out
}

// CommWarpSummary names how many rows each of the four lists carried, for one log
// line per send: "type0 2 row(s), type1 0 row(s), ...". It counts what is ON the wire,
// so it is read through the same rowsOfType the packets are built from.
func CommWarpSummary(saved []SavedPoint) string {
	var b strings.Builder
	for telType := 0; telType < commWarpTypes; telType++ {
		if telType > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "type%d %d row(s)", telType, len(rowsOfType(saved, telType)))
	}
	return b.String()
}

