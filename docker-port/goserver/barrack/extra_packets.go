package barrack

import "encoding/binary"

// Two packets the barrack burst carries that describe the player's stuff:
// BC_BARRACK_EQUIP_INFO_EXTRA (17) and BC_INVENTORY_ITEM_LIST (26).
//
// Both are built here in their EMPTY form first. That is deliberate: an empty
// list is honest (this character has nothing recorded yet) and, unlike a
// replayed packet, it cannot carry another account's item ids. Options and bag
// contents become real data once the item layer is ported.
const (
	// EquipExtraHeaderLen is what the shipped client reads: it takes THREE
	// counts after the command, not one. Sending a 12-byte packet made it read
	// the third count from beyond our packet, then grind for ~58 s over records
	// that did not exist. With a correct header the same screen loads in ~2.6 s.
	EquipExtraHeaderLen = 16

	// EmptyItemListLen is the documented empty form of every inventory-family
	// list: cmd(2) + len(2) + count(4) + type(4).
	EmptyItemListLen = 12
)

// BuildEquipExtra builds an option/socket/property header with no records.
//
// Layout the client reads: cmd(2) len(2) nOptions(4) nSockets(4) nProperties(4),
// then nOptions * 16-byte records, nSockets * 12-byte, nProperties * 9-byte.
func BuildEquipExtra() []byte {
	b := make([]byte, EquipExtraHeaderLen)
	binary.LittleEndian.PutUint16(b[0:], 17)                 // command
	binary.LittleEndian.PutUint16(b[2:], EquipExtraHeaderLen) // whole packet
	binary.LittleEndian.PutUint32(b[4:], 0)                  // nOptions
	binary.LittleEndian.PutUint32(b[8:], 0)                  // nSockets
	binary.LittleEndian.PutUint32(b[12:], 0)                 // nProperties
	return b
}

// BuildEmptyItemList builds an empty BC_INVENTORY_ITEM_LIST: the shape every
// inventory-family list has when it holds nothing.
func BuildEmptyItemList() []byte {
	b := make([]byte, EmptyItemListLen)
	binary.LittleEndian.PutUint16(b[0:], 26)             // command
	binary.LittleEndian.PutUint16(b[2:], EmptyItemListLen) // whole packet
	return b
}
