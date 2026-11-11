package zone

import "encoding/binary"

// ZC_PROMOTION_INFO (3093, 25 bytes) - THE RANK WORD ON THE PARTY CARD.
//
// WHAT IT LOOKS LIKE WITHOUT IT: each party card has a small line under the name
// where the character's promotion rank belongs ("Master", "High Master", ...),
// and with no 3093 ever sent that line is blank for every character - the exact
// report from the owner's screen on 2026-11-04 ("supposed to say High Master or
// Master").
//
// WHY IT IS NOT IN THE 3684: the Character Info packet has no promotion field at
// all (its 49-byte BASIC_STAT is level, HP pair, SP pair, EXP pair and the six
// base stats), and neither does START_INFO (485 bytes). The rank travels in its
// own packet, and the reference sends it PER CHARACTER on the way into the world:
//
//	CCommander_FSM.cpp:251   pc->UpdatePromotionInfo();      (zone enter)
//	CCommander.cpp:8780      pc->UpdatePromotionInfo();      (PC added / swapped)
//
// Both are `CPlayerChar::UpdatePromotionInfo` (PlayerChar.cpp:1472), which fills:
//
//	out.level     = GetPromotionLv()        the BANDED promotion level
//	out.maxStep   = GetPromotionStep()      the RAW `character_property` step
//	out.cashLevel = GetCashPromotionStep()
//	out.statPoint = GetRemainStatPoint()    points still unspent
//	out.stat      = the PROMOTION-added STR/AGI/DEX/INT/CON/CHA
//
// The client turns `level` into the word: its message table has
// UI_MSG_PROMOTIONLV_VE / _EX / _MA / _HI (clientmessages.xml 1684-1681 =
// "Veteran", "Expert", "Master", "High Master"), and `datatable_promotion_step`
// is the table those steps come from (1 Veteran 100-110 ... 4 HighMaster 130-140).
//
// LAYOUT, from the shipped struct (geZCPacket.h:3463, `PKS_DEFAULT` = 2 bytes,
// `#pragma pack(1)`) - the size check is 2+4+1+12+1+1+4 = 25, and the client's own
// command table (data\cmd_table.txt:574) gives 3093 that same 25:
//
//	+0   2  command            3093
//	+2   4  handle             the character it describes
//	+6   1  level              the BANDED promotion level (Character.Promotion)
//	+7  12  stat               BASE_STAT: 6 WORDs, STR AGI DEX INT CON CHA -
//	                           NOTE the order, it is NOT the BASIC_STAT order
//	+19  1  maxStep            the RAW step (Character.PromotionStep)
//	+20  1  cashLevel          the raw CASH step (Character.CashPromotion)
//	+21  4  statPoint          unspent promotion points (int)
//
// The stat block is the PROMOTION-added stats, not the character's base stats,
// and this server grants none yet, so it is written as six explicit zeros - a
// deliberate zero, with the meaning named here rather than implied by a memset.
const (
	// CmdZCPromotionInfo is ZC_PROMOTION_INFO.
	CmdZCPromotionInfo = 3093
	// PromotionInfoSize is fixed: 25 bytes, measured against the client's own
	// command table, not inferred from the struct alone.
	PromotionInfoSize = 25

	piHandle    = 2
	piLevel     = 6
	piStat      = 7  // +6 WORDs
	piMaxStep   = 19
	piCashLevel = 20
	piStatPoint = 21 // int32
)

// BuildPromotionInfo makes one 3093 for one character.
//
// ONE BUILDER, because the packet is sent from more than one moment (entry today,
// a promotion change later) and a second hand-built copy is how the two come to
// disagree about a level.
func BuildPromotionInfo(c Character) []byte {
	b := make([]byte, PromotionInfoSize)
	binary.LittleEndian.PutUint16(b[0:], CmdZCPromotionInfo)
	binary.LittleEndian.PutUint32(b[piHandle:], c.Handle)
	b[piLevel] = c.Promotion
	b[piMaxStep] = c.PromotionStep
	b[piCashLevel] = c.CashPromotion
	// +7..+18 and +21..+24 stay zero: no promotion stat is granted yet and no
	// point is unspent. Both are the truth, not a placeholder - see the note above.
	return b
}
