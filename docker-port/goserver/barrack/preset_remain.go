package barrack

import "encoding/binary"

// BC_EQUIP_ITEM_PRESET_ADD_ITEM_REMAIN_COUNT (cmd 65) - the items that can pay
// for an extra preset slot, and one number per item that the client displays as
// "the number of possession" in the Extend Notice box.
//
// LAYOUT comes from the LIVE packet
// (data\replay_live\BC_EQUIP_ITEM_PRESET_ADD_ITEM_REMAIN_COUNT_65.bin, 32 bytes):
//
//	cmd(2) len(2)=32 count(4)=3, then count x { itemId(4) value(4) }
//
// It is a VARIABLE packet - the client's own table registers size 0 for it - so
// the length word at +2 is the whole packet, this one included.
//
// The three ids in that capture are exactly the ExtendItemID list from the
// client's own datatable_equip_item_preset.ies, and the item index names them:
//
//	189370  PresetDrill_Event
//	 21183  PresetDrill_Trade
//	 75330  GrowthStone
//
// i.e. three ways to pay for the same extra preset slot, which is why the Notice
// box says "you need Growth Stone x 1".
//
// WHAT THE VALUE MEANS IS NOW PROVEN (2026-10-25). It is a count, and the client
// picks the FIRST item whose count is above zero:
//
//   - the capture holds 0, 0, 8656 and the Notice said "Growth Stone" - only the
//     third was above zero, so the third was chosen;
//   - a probe sending 1111, 2222, 3333 made the Notice say "Equipment preset
//     drill (Event) x 1 ... possession : 1111" - the FIRST of the three, even
//     though 3333 was the largest.
//
// So filling these with the player's real counts picks the right item by itself.
// There is no priority list to reproduce.
const (
	// PresetRemainCountSize is what the live packet measures. The size is not
	// registered (cmd 65 is variable), so this number comes from the capture.
	PresetRemainCountSize = 32

	presetRemainHeader = 8 // cmd(2) + len(2) + count(4)
	presetRemainRecord = 8 // itemId(4) + value(4)
)

// The three payment items, from the client's own ExtendItemID list.
const (
	ItemPresetDrillEvent = 189370 // datatable: PresetDrill_Event
	ItemPresetDrillTrade = 21183  // datatable: PresetDrill_Trade
	ItemGrowthStone      = 75330  // datatable: GrowthStone
)

// PresetExtendItem is one way to pay for an extra preset slot.
type PresetExtendItem struct {
	ItemID uint32
	Value  uint32 // how many the player owns; 0 means "cannot pay with this one"
}

// BuildPresetRemainCount builds cmd 65 from the given items.
func BuildPresetRemainCount(items []PresetExtendItem) []byte {
	total := presetRemainHeader + len(items)*presetRemainRecord
	out := make([]byte, total)
	binary.LittleEndian.PutUint16(out[0:], 65)            // command
	binary.LittleEndian.PutUint16(out[2:], uint16(total)) // whole packet
	binary.LittleEndian.PutUint32(out[4:], uint32(len(items)))
	for i, it := range items {
		off := presetRemainHeader + i*presetRemainRecord
		binary.LittleEndian.PutUint32(out[off:], it.ItemID)
		binary.LittleEndian.PutUint32(out[off+4:], it.Value)
	}
	return out
}

// BuildPresetRemainCountJP is the same variable-size body under the JP id: the
// JP 375639 table has BC_EQUIP_ITEM_PRESET_ADD_ITEM_REMAIN_COUNT at 66 (the
// 2020 build has it at 65), and its type is variable, which this packet
// already is (len at +2).
func BuildPresetRemainCountJP(items []PresetExtendItem) []byte {
	out := BuildPresetRemainCount(items)
	binary.LittleEndian.PutUint16(out[0:], 66)
	return out
}

// PresetExtendItems is the packet's data for a player who owns nothing.
//
// The Go server has no item layer yet - BC_INVENTORY_ITEM_LIST (26) is sent EMPTY
// on purpose - so the honest count for every one of the three is 0, and Extend
// will correctly refuse. Do NOT put made-up numbers here to make the button look
// alive: the client prints this value as "the number of possession". When the
// item layer is ported, this becomes a lookup per item id.
func PresetExtendItems() []PresetExtendItem {
	return []PresetExtendItem{
		{ItemID: ItemPresetDrillEvent, Value: 0},
		{ItemID: ItemPresetDrillTrade, Value: 0},
		{ItemID: ItemGrowthStone, Value: 0},
	}
}
