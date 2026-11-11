package barrack

import "encoding/binary"

// BC_ALL_EQUIP_ITEM_PRESET_INFO (cmd 57) - every character's saved equipment
// presets, in its EMPTY form.
//
// SHAPE: FIXED 866 bytes, no length word. Two independent facts say so:
//
//   - the client's own packet table (data\cmd_table_live.txt) registers id 57
//     with size 866, and a size of 0 is what marks a VARIABLE packet there - so
//     a real size means "read exactly this many";
//   - the live packet at +2 holds 00, not a length. On a variable packet that
//     word is the whole-packet length and can never be 0.
//
// The body is one slot per (character, preset):
//
//	866 = cmd(2) + 864, and 864 = 12 characters x 2 presets x 36
//
// Neither factor is a guess, both come from the CLIENT's own data:
//
//   - 2 presets per character is MaxPresetCount="2" in the client's data table
//     datatable_equip_item_preset.ies (ClassID 1, ClassName "preset"), and the
//     two windows the feature ships - ui\uixml\presetequip.xml and
//     presetselect.xml - define exactly two buttons, "preset1" and "preset2";
//   - 12 is the barrack page's character slots, the same 12 cmd 16 sends.
//
// ZERO is the honest value, and it is what makes this cheap: an empty preset has
// no items, so every slot is 0 in whatever order the records are laid out. Only
// the total SIZE has to be right to send a correct EMPTY packet, and that size is
// the client's own registration. Which means this builder does not depend on the
// record stride - 24 x 36 and 12 x 72 both give 864 zeros.
//
// Why the live packet is NOT replayed: 18 of its bytes are non-zero and they sit
// at no regular stride (offsets 4, 16, 20, 96, 104, 272, ... scatter under every
// candidate stride from 36 to 96), which is the signature of the shipped
// server's uninitialised memory - the same thing cmd 41's TEAM_GROUP records
// carry. Copying them copies someone else's bytes.
const (
	// EquipPresetInfoSize is the size the CLIENT registers for cmd 57.
	EquipPresetInfoSize = 866

	// EquipPresetMax is MaxPresetCount from datatable_equip_item_preset.ies.
	// Documented here because it is the reason 864 divides the way it does; this
	// builder sends every preset empty and does not need the value at run time.
	EquipPresetMax = 2
)

// BuildEquipPresetInfo builds cmd 57 with every preset empty: the command word,
// then 864 zero bytes.
func BuildEquipPresetInfo() []byte {
	b := make([]byte, EquipPresetInfoSize)
	binary.LittleEndian.PutUint16(b[0:], 57) // command; no length word
	return b
}

// BuildEquipPresetInfoJP is the JP 375639 form of the same packet. Its barrack
// has 32 channels (288 slots), and the client's table says cmd 58 is 1,154
// bytes: 2 + 288*4, against our 2 + 216*4 = 866. The body is all zeros (every
// preset empty); only the command and the length differ.
func BuildEquipPresetInfoJP() []byte {
	const size = 1154
	b := make([]byte, size)
	binary.LittleEndian.PutUint16(b[0:], 58) // BC_ALL_EQUIP_ITEM_PRESET_INFO, JP id
	return b
}
