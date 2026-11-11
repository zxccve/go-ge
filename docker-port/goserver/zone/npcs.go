package zone

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"geserver/text"
)

// Town NPCs, as DATA.
//
// The position needs a WORLDPOS cell - a navmesh TRIANGLE INDEX, never a height -
// and resolving one requires the map's mesh through a 32-bit Windows helper
// (`navmesh\pathtool.exe`), which cannot run on the Linux VPS this server targets.
// So the lookup is a BUILD step: `src\emit_zone_npcs.py` writes one JSON file per
// map, and this file replays it. Adding or removing an NPC is a re-run of that
// tool, not a code change.
//
// The file is DERIVED data. Regenerate it; never hand-edit it.

// NPC is one town NPC from the emitted data file.
type NPC struct {
	// Handle is how the client refers to the NPC when clicked. The emitter numbers
	// them NPC_HANDLE_BASE + index, and the zone must keep the same numbering.
	Handle    uint32 `json:"handle"`
	X         int32  `json:"x"`
	Y         int32  `json:"y"`
	Cell      int32  `json:"cell"`
	Direction uint8  `json:"direction"`
	NPCType   uint32 `json:"npc_type"` // ClassID in the client's own "NPC" table
	GenID     uint32 `json:"gen_id"`   // NPCGen row id, used for the caption fallback
	// NameToken is the NPCGen row's own `Name` column - a locale TOKEN such as
	// `<$>10008848</>`, not text, and `None` (the ies word for "no value") on a
	// row that has no name. It is the ONE column the caption comes from; see
	// resolveNPCName for the reference citation and the measurement.
	NameToken string `json:"name_token"`
	ClassType string `json:"class_type"`

	// Name is the DISPLAY caption the spawn frame carries, resolved from
	// NameToken (resolveNPCName). It is COMPUTED, never read from the data file
	// (`json:"-"`), so regenerating the file cannot invent a caption and a test
	// can hand the builder any name it likes.
	Name string `json:"-"`

	// Script is the NPCGen `Script` column - the name whose click entry is
	// `SCR_CLICK_<Script>` in the script corpus. Empty means the NPC has no click
	// script, which is a real state (2 of map 10's 119) and not an error.
	Script string `json:"script"`

	// Range is the NPC table's `Range` column, in world units: the radius of
	// this NPC's ENTER/LEAVE trigger area (0 = the row declares none).
	//
	// It is NOT a click reach, and must never be used as one. The reference
	// server reads this column into `CTriggerObject::m_range` and uses it only
	// for the SCR_ENTER_/SCR_LEAVE_ visitor (geShared\Source\NPCTable.cpp:111 ->
	// ZServerLib\TriggerObject.cpp:314, 389); the client stores the value from
	// the appearance packet and never reads it (geclientlib\geScene\
	// TriggerObject.cpp:33). A click's reach is the CONSTANT DISTANCE_TO_CHAR
	// (100; x5 when the client flags longdistance) folded with both actors'
	// shape radii - ZServerLib\FieldObject.h:18, FieldObject.cpp:120-128 - which
	// is exactly what zone\click.go's ClickRange/ClickRangeLong already are.
	//
	// Carried because the appearance packet has a slot for it (apTrRange) and the
	// reference fills it with this value; the enter/leave triggers will read it
	// when the zone runs them.
	Range int32 `json:"range"`
}

// NPCFile is the whole data file for one map.
type NPCFile struct {
	MapID int `json:"map_id"`

	// Mesh is the map's walk-mesh name (`reb_basic`). The zone needs it to ask the
	// path engine for a route; a file without it still works, it just walks
	// straighter. Empty is allowed.
	Mesh string `json:"mesh"`

	ClientRevision string `json:"client_revision"`
	GeneratedBy    string `json:"generated_by"`
	NPCCount       int    `json:"npc_count"`
	NPCs           []NPC  `json:"npcs"`
}

// LoadNPCs reads one map's NPC data.
//
// A missing file is NOT an error by itself - a map with no town has no file - but
// the caller should say so, because "the town is empty" and "the file is missing"
// look identical in game and only one of them is expected.
func LoadNPCs(path string) (*NPCFile, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc NPCFile
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.NPCCount != len(doc.NPCs) {
		return nil, fmt.Errorf("%s claims %d NPC(s) but carries %d",
			path, doc.NPCCount, len(doc.NPCs))
	}
	for i, npc := range doc.NPCs {
		// The cell is a triangle index: a negative or absurd value means the file
		// was built from the wrong column, and the client would put the NPC through
		// the floor (or assert in CWorld::ConvertPosition).
		if !PlausibleCell(npc.Cell) {
			return nil, fmt.Errorf("%s npc %d (%s): cell %d is not a navmesh triangle "+
				"index - that is a height", path, i, npc.ClassType, npc.Cell)
		}
		if npc.Handle == 0 {
			return nil, fmt.Errorf("%s npc %d (%s): handle 0 - the client cannot "+
				"click an NPC without one", path, i, npc.ClassType)
		}
	}
	return &doc, nil
}

// NPC trigger packet offsets. Copied from src\zone_server.py, which measured them
// against the live client; the packet is FIXED at 109 bytes with no length word.
const (
	trSize       = 109
	trHandle     = 2
	trPos        = 15 // x, y, cell (three int32)
	trDirection  = 27
	trAppearance = 28
	apTrType     = 0
	apTrRange    = 4
	apTrName     = 8 // char[65]
	apTrGenID    = 73
	apTrPCShape  = 77
	apTrSize     = 81
	npcNameLen   = 65

	// npcNameMax is the most BYTES a caption may occupy: a char[65] holding a
	// NUL-terminated string. The reference's `strncpy_s(appearance.name,
	// name.c_str(), _TRUNCATE)` (TriggerObject.cpp:427) writes at most
	// NPC_NAME_LEN-1 characters and always the NUL, so 64 is the ceiling.
	npcNameMax = npcNameLen - 1
)

// BuildSpawnTrigger builds one ZC_SPAWN_STANDINGTRIGGER (3543) for a town NPC.
//
// The caption is NPC.Name - the DISPLAY name resolveNPCName read off this row's
// own NPCGen `Name` column through the game dictionary, which is what the
// reference puts in this slot (see resolveNPCName for the chain and the
// measurement). An EMPTY field is a real state, not a failure: the client then
// names the NPC from its GenID in its own NPCGen table, which is correct for a
// row that came from the client's own npcgen data (GeBaseObject.cpp:303-311).
//
// What must NEVER be written here is a raw `<$>N</>` token: the client draws the
// field verbatim, so a token would appear as "<$>10090776</>" over the NPC's head.
// resolveNPCName refuses exactly that, and the guard below repeats the refusal at
// the last gate before the wire.
func BuildSpawnTrigger(npc NPC) []byte {
	out := make([]byte, trSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCSpawnTrigger)
	binary.LittleEndian.PutUint32(out[trHandle:], npc.Handle)
	binary.LittleEndian.PutUint32(out[trPos:], uint32(npc.X))
	binary.LittleEndian.PutUint32(out[trPos+4:], uint32(npc.Y))
	binary.LittleEndian.PutUint32(out[trPos+8:], uint32(npc.Cell))
	out[trDirection] = npc.Direction

	ap := trAppearance
	binary.LittleEndian.PutUint32(out[ap+apTrType:], npc.NPCType)
	// apTrRange carries the NPC table's own `Range` - the ENTER/LEAVE trigger
	// radius. The reference fills this slot from CTriggerObject::m_range
	// (ZServerLib\TriggerObject.cpp:415, GetAppearance); it is NOT a click reach
	// (see the NPC.Range comment). A row that declares 0 sends 0.
	binary.LittleEndian.PutUint32(out[ap+apTrRange:], uint32(npc.Range))
	// apTrName: the caption, 65 bytes, NUL-padded. A token never rides here.
	name := npc.Name
	if strings.Contains(name, "<$>") {
		name = ""
	}
	putNPCName(out[ap+apTrName:ap+apTrName+npcNameLen], name)
	binary.LittleEndian.PutUint32(out[ap+apTrGenID:], npc.GenID)
	// apTrPCShape and apTrSize stay 0: the reference server sends 0 for both.
	return out
}

// resolveNPCName answers the caption a spawn frame should carry, and - when it
// carries none - WHY, as a sentence the log can print. An empty name with a
// non-empty why is a REFUSAL, not a fallback: no class name, no script name and
// no token is ever invented to fill the slot.
//
// THE SOURCE IS THE NPCGEN ROW'S OWN `Name` COLUMN. The chain, in the reference's
// own words: `CWorld::InitTriggerObject` reads `cls->Get(StrID::Name, occupation)`
// from the npcgen_<world> row (source2\geServer\ZServerLib\World.cpp:1222) and
// stores it on the trigger as its "Occupation" property when the value is not the
// ies word None (:1239-1240); `CTriggerObject::GetAppearance` writes that property
// into the 65-byte field and ZEROES the field when it is `StrID::None`
// (TriggerObject.cpp:422-427). NOT the datatable_npc row and not the script: that
// table has an `Occupation` column of its own and it is "None" on all 1,942 rows,
// which is why the same word in two tables must not be confused.
//
// The npcgen `Name` is a locale TOKEN, and the wire carries the RESOLVED TEXT:
// the live v32 capture `captures\animref\20261028-221827` holds 42 map-10 (Reboldoeux)
// 3543 records, and all 42 equal this row's Name token through
// dictionary_local.xml - "Sir Lyndon", "[Mass Seller] Weapons", "Waypoint",
// "Reboldoeux Deposit Officer" - including the six whose Name is the ies word
// "None", which arrive EMPTY (54, 55, 1217, 1218, 1219, 1220). That is 42/42,
// so the column and the resolution are measured, not inferred.
//
// THE LOOKUP DOES NOT GO THROUGH Book.Resolve, deliberately: Resolve applies the
// id map first, and the id map is a SCRIPT rewrite of the old id space. Map 10's
// genId 1215 carries `<$>184538</>`, which the modern dictionary answers "Crack" -
// what the capture's wire says - while the id map points 184538 at a dialog line
// ("{#D89EFC}(Kurt goes into the crevise of the ship.){/}"). A caption is the
// client's OWN id, so only Dict and then ServerPack are asked, in that order (the
// pack may fill a gap and never overwrite - text\book.go's own precedence).
func resolveNPCName(npc NPC, book *text.Book) (name, why string) {
	tok := strings.TrimSpace(npc.NameToken)
	if tok == "" {
		return "", "its NPCGen row's Name column is empty"
	}
	if strings.EqualFold(tok, "None") {
		// The ies convention for "no value", and the reference's own test
		// (`occupation != StrID::None`, World.cpp:1239) keeps such a row nameless.
		return "", `its NPCGen row's Name is the ies word "None" (no name)`
	}

	id := ""
	if m := text.TokenRe.FindStringSubmatch(tok); m != nil && m[0] == tok {
		id = m[1]
	}
	if id == "" {
		// Not a dictionary token: a plain string needs no resolution and is sent
		// as it stands (the reference writes the property verbatim). Anything
		// still bearing token punctuation is refused rather than drawn raw.
		if strings.Contains(tok, "<$>") {
			return "", fmt.Sprintf("its NPCGen row's Name %q is not a plain "+
				"dictionary id, and a token must never reach the wire", tok)
		}
		return tok, ""
	}
	if book == nil {
		return "", fmt.Sprintf("this zone loaded no dictionary, so %q cannot "+
			"be resolved", tok)
	}
	if v, ok := book.Dict.Get(id); ok && v != "" && !strings.Contains(v, "<$>") {
		return v, ""
	}
	if v, ok := book.ServerPack.Get(id); ok && v != "" && !strings.Contains(v, "<$>") {
		return v, ""
	}
	return "", fmt.Sprintf("the dictionary has no text for %q", tok)
}

// putNPCName writes one caption into the packet's fixed 65-byte field: at most 64
// bytes of text, cut on a UTF-8 boundary so a multi-byte character is never split,
// the rest NUL. The field is part of a fixed-size packet, so the packet's length
// never changes with the name's.
func putNPCName(field []byte, name string) {
	for i := range field {
		field[i] = 0
	}
	n := len(name)
	if n > npcNameMax {
		n = npcNameMax
		// 0x80..0xBF is a UTF-8 CONTINUATION byte: stepping back over one keeps
		// the cut on a character boundary instead of splitting a rune.
		for n > 0 && name[n]&0xC0 == 0x80 {
			n--
		}
	}
	copy(field[:n], name[:n])
}

// npcSpawnPackets builds the 3543 for every NPC in the list, naming each from its
// own row through the dictionary the Handler already holds (`Handler.Text`). It is
// the ONE production way a town NPC's spawn frame is built: zone.go's GAME_READY
// branch calls it for the map's NPCs and for the ones a script created.
//
// A row that cannot be named sends an EMPTY field and writes ONE line naming the
// row and the reason (dropSayOnce, so a player re-entering does not repeat it).
// The client's own NPCGen fallback then names it, which is today's behaviour and
// a correct one for a row from the client's own npcgen data.
func (h *Handler) npcSpawnPackets(npcs []NPC) [][]byte {
	out := make([][]byte, 0, len(npcs))
	named, unnamed := 0, 0
	for _, npc := range npcs {
		name, why := resolveNPCName(npc, h.Text)
		if why != "" {
			unnamed++
			h.dropSayOnce(fmt.Sprintf("npc-name-%08x", npc.Handle), fmt.Sprintf(
				"zone: SPAWN NPC 0x%08x (%s) carries NO caption: %s - the field "+
					"stays empty and the client names it from NPCGen row %d if it "+
					"has one", npc.Handle, npc.ClassType, why, npc.GenID))
		} else {
			named++
		}
		npc.Name = name
		out = append(out, BuildSpawnTrigger(npc))
	}
	if len(npcs) > 0 {
		h.dropSayOnce(fmt.Sprintf("npc-names-%d", len(npcs)), fmt.Sprintf(
			"zone: NPC captions: %d of %d row(s) carry a name, %d have none",
			named, len(npcs), unnamed))
	}
	return out
}

// BuildNPCs turns a whole map's data into the spawn packets, in file order. The
// client skips a handle it already has, so re-sending is harmless.
//
// It does NOT resolve captions: a row whose NPC.Name is empty sends an empty
// field. Production builds these packets through Handler.npcSpawnPackets, which
// names each row first; this stays the plain builder for callers that hold a row
// already named (and for tests of the layout).
func BuildNPCs(npcs []NPC) [][]byte {
	out := make([][]byte, 0, len(npcs))
	for _, npc := range npcs {
		out = append(out, BuildSpawnTrigger(npc))
	}
	return out
}
