// THE TWO BUFF PACKETS - 3556 and 3125.
//
// MEASURED, not guessed. Every number here comes from a live capture or the
// reference's own struct, and the file names which:
//
//	3556 ZC_CHANGE_BUFF  262 B: cmd(2) + handle(4) + a 256-byte MASK.
//	     `history\combat.md` §10.7 read it out of the capture: handle at +2, then a
//	     256-byte bitmap at +6 ("REP MOVSD, 64 dwords"), and the ONLY byte that
//	     changed on three teammates' bars when Burning Hands landed was +10 = 0x40 -
//	     bit 6 of mask byte 4 = bit 38 = that buff's own id. Built by
//	     `CFieldChar::SendChangeBuff` (FieldChar.cpp:714-731) and broadcast.
//	     SIZE from `data\cmd_table_live.txt:1223` (3556 262) and `cmd_table.txt:608`.
//
//	3125 ZC_BUFF_REMAINTIME  15 B: cmd(2) + pcIndex(4) + buffType(4) + level(1) +
//	     duration(4). `PKS_ZC_BUFF_REMAINTIME` (source2\geShared\include\
//	     geZCPacket.h:3596-3601) is exactly those four fields after the header, and
//	     the live capture agrees field for field: handle +2, buff id +6, a BYTE at
//	     +10, a DWORD of remaining MILLISECONDS at +11 (combat.md §10.7: "MOV
//	     AL,[EBX+0x0A] then MOV EAX,[EBX+0x0B]; DIV 1000"). `level` is the buff's
//	     applied level - `CBuff::GetSkiilLevel()` answers `m_buff.arg1`
//	     (Buff.cpp:583-586) and `SendBuffRemainTime` sends `m_buff` with the
//	     remaining time, so the byte is arg1 and not a second-bar flag.
//
// THE MASK'S WIDTH IS 256 BYTES AND IT IS NOT COPIED FROM source2. That header's
// `BUFF_MASK` is BYTE[128] (`geShared\include\geZCTypes.h:384`) and belongs to the
// older generation: this table's max ClassID is 2047, which is the 2,048th bit and
// therefore needs byte 255. The live 3556 is 262 bytes = 2 + 4 + 256, and the
// client's own 3573 mask field is the same 256 (`zone\skill_cast.go:335-337`).
//
// THE BIT ORDER is `imc::BitSet(mask, id)`: byte `id >> 3`, bit `id & 7`, least
// significant bit first - the rule the measurement above pins for id 38.
package zone

import "encoding/binary"

const (
	// CmdZCChangeBuff is ZC_CHANGE_BUFF: "this actor's whole buff bar is now this
	// mask". The client keys the icon on the bit, so a bit with no state behind it
	// draws a buff that changes nothing - which is why this packet is only ever
	// built from a list that really holds the buff.
	CmdZCChangeBuff = 3556
	// CmdZCBuffRemainTime is ZC_BUFF_REMAINTIME: one buff's remaining milliseconds
	// for the bar's own timer. Sent to the OWNER of the buff, never broadcast
	// (`CBuff::OnEnter` sends it only when `OT_PLAYERCHAR == owner`'s type,
	// Buff.cpp:104-105).
	CmdZCBuffRemainTime = 3125

	// BuffMaskSize is the mask's width in bytes: 256, i.e. 2,048 bits, and the
	// table's largest ClassID is 2,047.
	BuffMaskSize = 256

	// ChangeBuffSize is 3556's wire size: 2 + 4 + 256 = 262.
	ChangeBuffSize = 2 + 4 + BuffMaskSize
	// BuffRemainTimeSize is 3125's wire size: 2 + 4 + 4 + 1 + 4 = 15.
	BuffRemainTimeSize = 2 + 4 + 4 + 1 + 4
)

// BuildChangeBuff builds ZC_CHANGE_BUFF (3556) for one actor: the whole bar as a
// 256-byte bitmap, one bit per buff, indexed by the buff's ClassID.
func BuildChangeBuff(handle uint32, mask [BuffMaskSize]byte) []byte {
	out := make([]byte, ChangeBuffSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCChangeBuff)
	binary.LittleEndian.PutUint32(out[2:], handle)
	copy(out[6:], mask[:])
	return out
}

// BuildBuffRemainTime builds ZC_BUFF_REMAINTIME (3125) for one buff on one
// character: its class id, its applied level and what is left of it, in
// milliseconds.
//
// remainMS is the LIST's own number (ApplyTime - elapsed), clamped at 0 by the
// caller - the reference clamps in `SendBuffRemainTime` ("if(remainTime < 0)
// remainTime = 0", Buff.cpp:565-567).
func BuildBuffRemainTime(handle, classID uint32, level byte, remainMS uint32) []byte {
	out := make([]byte, BuffRemainTimeSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCBuffRemainTime)
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint32(out[6:], classID)
	out[10] = level
	binary.LittleEndian.PutUint32(out[11:], remainMS)
	return out
}

// buffMaskBit sets one buff's bit in a mask: byte id>>3, bit id&7, LSB first - the
// rule the live 3556 measured for buff 38 (byte 4 = 0x40).
//
// It is a SEPARATE function only so the one place the shift appears is named: the
// mask and the table's ClassID range have to agree (2,047 max), and a caller that
// computed the index differently would move every icon on the bar.
func buffMaskBit(mask *[BuffMaskSize]byte, id uint32) {
	if id >= BuffMaskSize*8 {
		return
	}
	mask[id>>3] |= 1 << (id & 7)
}
