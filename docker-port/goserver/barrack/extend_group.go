package barrack

import (
	"context"
	"encoding/binary"
)

// The Team List's Extend button: CB_REQ_EXTEND_GROUP (52) asks, BC_REQ_EXTEND_GROUP
// (53) carries the price, BC_EXTEND_GROUP_RESULT (54) says it happened.
//
// THE PRICE IS THE GAME'S OWN SCRIPT, not a number invented here.
// `ge/shared/script/price_calculate.scp` (ISO-8859, CRLF) says:
//
//	func GET_TEAMGROUP_PRICE(commander)
//	{
//		var CurrnetSlot = commander.Lock_TeamGroup;
//		var NeedCount = 5;
//	  if(CurrnetSlot >= 10){
//	    NeedCount = 6 + (CurrnetSlot - 10);
//	  }
//		return "MoonStone", NeedCount;
//	}
//
//	func GET_TEAMGROUP_MAXSLOT()
//	{
//		return 20;
//	}
//
// and the item NAME it returns is resolved through the item table, exactly as the
// reference does (`geItemTable::Get(Itemname.c_str())` -> `pItemProp->type`,
// BarrackServer/Client.cpp:930-946). MoonStone's row in
// `ge/xml/datatable_item_etc.xml:2080` is:
//
//	<Class ClassID="75318" ClassName="MoonStone" ... EngName="Moon Stone"
//	       ClassType="Etc" MaxStack="32767" ... />
//
// The same id is what the shipped item index answers for the name
// (`data/item_catalog.json`: "MoonStone":[75318,1,32767]).
const (
	// MoonStoneClassID is the item class GET_TEAMGROUP_PRICE charges by name.
	MoonStoneClassID uint32 = 75318

	// TeamGroupMaxSlot is GET_TEAMGROUP_MAXSLOT(): the cap the reference refuses
	// at (`maxCount >= GetMaxTeamGroupCount()`, Client.cpp:949).
	TeamGroupMaxSlot uint16 = 20

	// extendGroupExecuteOffset is where PKS_CB_REQ_EXTEND_GROUP's one byte of
	// intent sits: PKS_SECURITY_DEFAULT is cmd(2) + seq(4) + checksum(4) = 10
	// bytes (gePacketBase.h:52-56) and the whole packet is 11
	// (data/cmd_table_barrack.txt: `52 11 CB_REQ_EXTEND_GROUP`).
	extendGroupExecuteOffset = 10
)

// TeamGroupPrice is GET_TEAMGROUP_PRICE's arithmetic, and nothing else: 5 stones
// below slot 10, then 6 + (slot - 10). The script is quoted above; the three
// numbers the card pins are slot 0 -> 5, slot 10 -> 6, slot 15 -> 11.
func TeamGroupPrice(slot uint16) int {
	if slot >= 10 {
		return 6 + int(slot) - 10
	}
	return 5
}

// BuildExtendGroupPrice is BC_REQ_EXTEND_GROUP (53): the price, as an item.
//
//	cmd(2) + ReqItemClassType DWORD + ReqItemCount DWORD = 10 bytes
//
// (geBCPacket.h:298-302; the client turns it into the "consume N of X?" dialog,
// GEBarrackNet.cpp:772-792).
func BuildExtendGroupPrice(classID uint32, count int) []byte {
	out := make([]byte, 0, 10)
	out = binary.LittleEndian.AppendUint16(out, 53)
	out = binary.LittleEndian.AppendUint32(out, classID)
	out = binary.LittleEndian.AppendUint32(out, uint32(count))
	return out
}

// BuildExtendGroupResult is BC_EXTEND_GROUP_RESULT (54): TWO bytes, the command
// alone (geBCPacket.h:304-307 - the struct is empty).
//
// THERE IS NO FAILURE FORM OF THIS PACKET. The client's OnExtendGroupResult
// (GEBarrackNet.cpp:794-799) always shows UI_MSG_SUCCESS_SLOT_EXTEND, so a
// refused extension must send NOTHING rather than a 54: a two-byte "failure" is
// not a thing the wire can carry, and sending one would tell the player the slot
// was bought when it was not.
func BuildExtendGroupResult() []byte {
	out := make([]byte, 0, 2)
	out = binary.LittleEndian.AppendUint16(out, 54)
	return out
}

// parseExtendGroupRequest reads CB_REQ_EXTEND_GROUP's intent byte: 0 = "what
// does it cost?", 1 = "do it" (Client.cpp:938-958).
func parseExtendGroupRequest(packet []byte) (byte, bool) {
	if len(packet) <= extendGroupExecuteOffset {
		return 0, false
	}
	return packet[extendGroupExecuteOffset], true
}

// extendTeamGroup answers CB_REQ_EXTEND_GROUP (52).
//
//	execute 0 -> 53, the class and the count for the CURRENT Lock_TeamGroup.
//	execute 1 -> charge the stones, Lock_TeamGroup + 1, then cmd 32 (the Lock
//	             table with the NEW value - the reference's SendLockInfo,
//	             ClientDB.cpp:145) and 54.
//
// A refusal sends NOTHING: no 54 (see BuildExtendGroupResult), no write, and one
// log line naming the reason. That is the reference's own shape - it returns
// after an error message for a short bag (Client.cpp:960-966) and returns at the
// cap before touching anything (:949-950).
func (h *Handler) extendTeamGroup(ctx context.Context, sess *Session, packet []byte) ([][]byte, error) {
	if h.Barrack == nil || sess.CommanderID == 0 {
		h.Log.Printf("EXTEND_GROUP: no barrack store (or no commander on this session) - nothing sent")
		return nil, nil
	}

	execute, ok := parseExtendGroupRequest(packet)
	if !ok {
		h.Log.Printf("EXTEND_GROUP: request is %d bytes, too short to carry the intent byte at +%d - nothing sent",
			len(packet), extendGroupExecuteOffset)
		return nil, nil
	}

	switch execute {
	case 0:
		cmdr, found, err := h.Barrack.Commander(ctx, sess.CommanderID)
		if err != nil {
			return nil, err
		}
		if !found {
			h.Log.Printf("EXTEND_GROUP: commander %d is not in the database - no price sent",
				sess.CommanderID)
			return nil, nil
		}
		count := TeamGroupPrice(cmdr.TeamMax)
		out := BuildExtendGroupPrice(MoonStoneClassID, count)
		h.Log.Printf("EXTEND_GROUP: commander %d asks the price at slot %d -> %d x MoonStone "+
			"(class %d), cmd 53 (%d bytes)", sess.CommanderID, cmdr.TeamMax, count,
			MoonStoneClassID, len(out))
		return [][]byte{out}, nil

	case 1:
		res, err := h.Barrack.ExtendTeamGroup(ctx, sess.CommanderID, MoonStoneClassID)
		if err != nil {
			return nil, err
		}
		switch res.Outcome {
		case ExtendAtCap:
			h.Log.Printf("EXTEND_GROUP REFUSED: commander %d is at slot %d, the cap "+
				"GET_TEAMGROUP_MAXSLOT() names (%d) - nothing written, nothing sent",
				sess.CommanderID, res.Slot, TeamGroupMaxSlot)
			return nil, nil
		case ExtendNoStones:
			h.Log.Printf("EXTEND_GROUP REFUSED: commander %d holds %d of the %d MoonStone "+
				"(class %d) that slot %d costs - nothing written, nothing sent",
				sess.CommanderID, res.Held, res.Need, MoonStoneClassID, res.Slot)
			return nil, nil
		}
		// The slot count lives in the Lock table (cmd 32): the client reads it as
		// imcIES::GetPropertyNumber("Lock", 1, "Lock_TeamGroup") and both the
		// Extend guard and AddBarrackTeamGroup key off it (script.cpp:4776, :4798).
		// This is the reference's SendLockInfo step, and it is the packet that makes
		// the new slot appear - 16/17/35 rebuild characters, not the lock value, and
		// the reference sends none of them here.
		lock := LockTableFor(res.NewSlot)
		result := BuildExtendGroupResult()
		h.Log.Printf("EXTEND_GROUP: commander %d slot %d -> %d, took %d MoonStone (class %d), "+
			"sent cmd 32 (%d bytes, Lock_TeamGroup=%d) then cmd 54 (%d bytes)",
			sess.CommanderID, res.Slot, res.NewSlot, res.Taken, MoonStoneClassID,
			len(lock), res.NewSlot, len(result))
		return [][]byte{lock, result}, nil

	default:
		// The reference acts on 0 and 1 only (`if (packet->ExecutePks == 0) ...
		// else if (== 1)`); any other byte is a client this build does not know.
		h.Log.Printf("EXTEND_GROUP: commander %d sent intent %d, which the reference does "+
			"not act on - nothing sent", sess.CommanderID, execute)
		return nil, nil
	}
}
