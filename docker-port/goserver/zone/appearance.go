package zone

import "encoding/binary"

// WHAT MAKES A WORN ITEM VISIBLE ON THE BODY.
//
// An equip changes two different things, and the client is told about them by two
// different packets:
//
//	ZC_UPDATE_EQUIPMENT   3616  the NUMBERS (the armour is worn, its DEF applies,
//	                            the compare window says "equipped")
//	ZC_UPDATED_PCAPPEARANCE 3546 the BODY  (the model is rebuilt from the new
//	                            appearance, so the armour can be seen)
//
// The client's handler for 3616 (geClient\Source\GENormalNet_Action.cpp:1402,
// CNormalNet::UpdateEquipment) does ONLY Session().UpdateMyPcEquipment,
// UISession()->SetEquipOption and a skill-panel refresh. It never touches the
// model. The reference server therefore also calls BroadcastUpdateAppearance()
// (source2\geServer\ZServerLib\PlayerChar.cpp:458-467) at the end of
// CPlayerChar::OnChangedEquipment, and the client's handler for THAT
// (GENormalNet_Action.cpp:125-136, CNormalNet::UpdatedPCAppearance) is
// `World()->GetObject(handle)->ChangeAppearance(&appearance)`.
//
// Without 3546 a character wears the armour in every window and on nobody's
// screen - the bug reported in game on 2026-10-28 19:48.
const (
	// CmdZCUpdatedPCAppearance is ZC_UPDATED_PCAPPEARANCE, server -> client:
	// cmd(2) handle(4) appearance(395) = 401 bytes, FIXED.
	//
	// THERE IS NO LENGTH WORD IN THIS PACKET, and that is not an oversight: the
	// struct is PKS_ZC_UPDATED_PCAPPEARANCE : public PKS_DEFAULT
	// (source2\geShared\include\geZCPacket.h:1365), and PKS_DEFAULT is
	// `WORD command;` and nothing else (gePacketBase.h:40-42). Only PKS_VARSIZE
	// adds a length word. So bytes 2..5 are the HANDLE, and writing a length there
	// would corrupt it. The client reads it with
	// `PKT_ASN(ZC_UPDATED_PCAPPEARANCE)` at a size the command table fixes at 401
	// (data\cmd_table_live.txt:1213), the same way it reads ZC_LEAVE_PC (8 bytes)
	// and ZC_STOP_PC (22 bytes), neither of which carries a length word either.
	CmdZCUpdatedPCAppearance = 3546

	// UpdatedPCAppearanceSize is the whole packet: 2 + 4 + 395.
	UpdatedPCAppearanceSize = 2 + 4 + AppearanceLen

	upcHandle     = 2 // immediately after the command: cmd(2) then handle(4)
	upcAppearance = 6

	// CmdZCEquipReinforceInfoList is ZC_EQUIP_REINFORCE_INFO_LIST: the "+N"
	// reinforcement of every worn item (PlayerChar.cpp:3767-3780).
	CmdZCEquipReinforceInfoList = 3871
	// CmdZCEquipSocketInfoList is ZC_EQUIP_SOCKET_INFO_LIST: the sockets of every
	// worn item (PlayerChar.cpp:3782-3796).
	CmdZCEquipSocketInfoList = 3859
	// CmdZCEquipAdditionPropList is ZC_EQUIP_ADDITIONAL_PROP_LIST: the additional
	// properties of every worn item (PlayerChar.cpp:3798-3813).
	CmdZCEquipAdditionPropList = 3861

	// equipInfoListHeader is cmd(2) len(2) charIndex(1) count(4), and an EMPTY list
	// is exactly that header with count 0 - no record after it:
	//
	//	source2\geShared\include\geZCPacket.h:1318 PKS_ZC_EQUIP_REINFORCE_INFO_LIST
	//	source2\geShared\include\geZCPacket.h:1284 PKS_ZC_EQUIP_SOCKET_INFO_LIST
	//	source2\geShared\include\geZCPacket.h:1289 PKS_ZC_EQUIP_ADDITIONAL_PROP_LIST
	//
	// All three are `PKS_VARSIZE { BYTE charIndex; DWORD count; }` with
	// `#pragma pack(1)` (gePacketBase.h:37), so: +0 cmd, +2 len, +4 charIndex,
	// +5 count. The reference sends them unconditionally and its count is whatever
	// the worn set holds - 0 is a real answer, not a missing one.
	equipInfoListSize     = 9
	equipInfoListCharIdx  = 4
	equipInfoListCountOff = 5
)

// BuildUpdatedPCAppearance makes ZC_UPDATED_PCAPPEARANCE (3546), 401 bytes:
//
//	+0   cmd(2) = 3546
//	+2   handle(4) = the character's handle (the database's character id, the same
//	     value START_INFO and ZC_UPDATE_EQUIPMENT carry for this character)
//	+6   APPEARANCE_PC(395) = appearanceBlock(c), byte for byte the block the
//	     spawn packet carries at +306
//
// `c` must already hold the worn set AFTER the change: the caller puts
// res.Equip into its team copy first, then builds both this packet and the 3616
// from that copy, so the body and the numbers describe the same character.
//
// The client needs the actor: its handler asserts World()->GetObject(handle), so
// this may only be sent for a character that has been spawned - which the own
// player and every announced team member are.
func BuildUpdatedPCAppearance(handle uint32, c Character) []byte {
	out := make([]byte, UpdatedPCAppearanceSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCUpdatedPCAppearance)
	binary.LittleEndian.PutUint32(out[upcHandle:], handle)
	copy(out[upcAppearance:], appearanceBlock(c))
	return out
}

// BuildEquipInfoList makes one of the three equip info lists with count 0 and no
// record after it - the honest answer for a worn set this server knows nothing
// more about than its class ids.
//
// WHY EMPTY IS CORRECT HERE AND NOT A STUB: this server does not store
// reinforcement, sockets or additional item properties yet, so the reference's
// own GetItemReinforces / GetItemSockets / GetItemProperties would return 0 for
// every character we can build. Inventing records would tell the client an item
// is +10 with three sockets, and the client would draw it that way.
//
// `charIndex` is the character's place in this connection's team - the same
// number ZC_UPDATE_EQUIPMENT's handle names, which is how the client's handler
// (geClient\Source\GEModeNet.cpp:245-268, CModeNet::EquipReinforceInfoList and
// friends) files the data: UISession()->ExtractEquipReinforceInfo(buffer,
// charIndex, count).
func BuildEquipInfoList(cmd uint16, charIndex uint8) []byte {
	out := make([]byte, equipInfoListSize)
	binary.LittleEndian.PutUint16(out[0:], cmd)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	out[equipInfoListCharIdx] = charIndex
	// count at +5 stays 0: no record follows, so the length word above is the
	// whole packet and the client reads nothing past it.
	return out
}

// BuildEquipInfoLists makes the three lists in the reference's own order -
// reinforce (3871), socket (3859), additional property (3861), which is the order
// CPlayerChar::OnChangedEquipment calls them in (PlayerChar.cpp:2353-2355).
func BuildEquipInfoLists(charIndex uint8) [][]byte {
	return [][]byte{
		BuildEquipInfoList(CmdZCEquipReinforceInfoList, charIndex),
		BuildEquipInfoList(CmdZCEquipSocketInfoList, charIndex),
		BuildEquipInfoList(CmdZCEquipAdditionPropList, charIndex),
	}
}
