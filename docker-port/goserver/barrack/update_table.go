package barrack

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"strings"
)

// BC_UPDATE_TABLE (cmd 32) - the barrack pushes a property table to the client,
// which OVERWRITES the table the client loaded from its own data files.
//
// This is the packet that was dropped for the whole Go port, and dropping it was
// not harmless. The barrack's team buttons are driven by `Lock` properties the
// client reads through `imcIES::GetPropertyNumber("Lock", 1, "Lock_..." )`
// (GEBarrackNet.cpp:578, GEBarrackUIHandler.cpp:298) - and `Lock_BarrackLv`,
// `Lock_MCCLv`, `Lock_CharSlot` are all in here. Measured 2026-10-25: with the
// PYTHON barrack (which sends this) "Last Point" is clickable; with ours (which
// dropped it) the button stays disabled, with every other packet equal.
//
// Shape, VARIABLE size, so `len` at +2 is the whole packet:
//
//	cmd(2) len(2) idspace[20] xml + NUL
//
// and the XML is exactly what the shipped server pushes:
//
//	<idspace id="Lock"> <Class ClassID="1" ClassName="Lock" Lock_X = "0" ... /></idspace> \n
//
// The 84 values below are the server-side defaults (datatable_commanderlock.xml
// in the original suite). They are NOT per-player, which is why they live in this
// package as data rather than in a database column.
const (
	// UpdateTableIDSpaceLen is the id-space name field: 20 bytes, NUL padded.
	UpdateTableIDSpaceLen = 20

	// updateTableHeader is cmd(2) + len(2) + idspace(20).
	updateTableHeader = 4 + UpdateTableIDSpaceLen
)

//go:embed lock_props.json
var lockPropsJSON []byte

// BuildUpdateTable builds cmd 32 from (name, value) pairs.
func BuildUpdateTable(idspace string, props [][2]string) []byte {
	var attrs strings.Builder
	for _, kv := range props {
		attrs.WriteString(kv[0])
		attrs.WriteString(` = "`)
		attrs.WriteString(kv[1])
		attrs.WriteString(`" `)
	}
	xml := `<idspace id="` + idspace + `"> <Class ClassID="1" ClassName="` + idspace +
		`" ` + attrs.String() + `/></idspace> ` + "\n"

	body := append([]byte(xml), 0) // the XML is NUL terminated
	total := updateTableHeader + len(body)

	out := make([]byte, 0, total)
	out = binary.LittleEndian.AppendUint16(out, 32)            // command
	out = binary.LittleEndian.AppendUint16(out, uint16(total)) // whole packet
	out = append(out, cstr(idspace, UpdateTableIDSpaceLen)...)
	out = append(out, body...)
	return out
}

// LockProps is the Lock table the barrack pushes, in the order the shipped server
// uses (it is sorted, and the byte-for-byte test against the captured packet
// depends on that order).
func LockProps() [][2]string {
	var pairs [][]string
	if err := json.Unmarshal(lockPropsJSON, &pairs); err != nil {
		// The file is embedded at build time, so a parse failure is a build defect,
		// not bad input. Panic loudly rather than send a truncated table.
		panic("barrack: lock_props.json is not a list of [name, value] pairs: " + err.Error())
	}
	out := make([][2]string, 0, len(pairs))
	for _, kv := range pairs {
		if len(kv) != 2 {
			panic("barrack: lock_props.json entry does not have two strings")
		}
		out = append(out, [2]string{kv[0], kv[1]})
	}
	return out
}

// LockTable builds the Lock table packet the barrack sends.
func LockTable() []byte {
	return BuildUpdateTable("Lock", LockProps())
}

// LockTableFor is the same table with Lock_TeamGroup replaced by ONE commander's
// own value.
//
// WHY IT MUST EXIST: the client's barrack reads the slot count from this table
// (`imcIES::GetPropertyNumber("Lock", 1, "Lock_TeamGroup")`, script.cpp:4776)
// and both the Extend guard and AddBarrackTeamGroup key off it. The embedded
// table's 5 is the server-side default, not the player's row: a commander whose
// Lock_TeamGroup is 7 would otherwise be told 5 on every login, and the 6th and
// 7th saved team would be refused by the CLIENT even though the database allows
// them. The reference's SendLockInfo extracts these properties from the
// commander's own IES object (Commander.cpp:322-350), which is why it carries the
// current value after an extension.
func LockTableFor(teamMax uint16) []byte {
	props := LockProps()
	for i := range props {
		if props[i][0] == "Lock_TeamGroup" {
			props[i][1] = strconv.Itoa(int(teamMax))
		}
	}
	return BuildUpdateTable("Lock", props)
}
