package zone

import (
	"encoding/binary"
	"math"
	"strconv"
)

// ZC_UPDATE_ALL_STATUS (3684): the packet that puts the Character Info sheet's
// DERIVED numbers on the panel.
//
// WHY IT EXISTS: the shipped client does NOT compute ATK, DEF, A.R., D.R., HR,
// CSPD or the five resistances. Its handler `FUN_004f0b60` (registered at
// 0x6a0e34, id 0xE64, size 125) hands the buffer to `FUN_005676f5` ->
// `FUN_005e6ae6` (`SetStat`), which memcpys into the SAME per-character record
// `SetMyPcInfo` fills, and the panel `FUN_007820ce` prints those stored words
// with no arithmetic at all. Proven, and written down in
// `history\zone-entry.md` / `history\gameplay.md`; do not re-derive it.
//
// THE LENGTH IS FIXED AT 125 BYTES:
//
//	2   cmd
//	4   handle
//	49  BASIC_STAT          (LV, HP, maxHP, SP, maxSP, EXP, nextEXP, 6 stats)
//	24  BUFF_MODIFIER_STAT  (6 DWORD: STR, CON, DEX, INT, AGI, CHA modifiers)
//	36  BATTLE_STAT         (18 WORD - the SHIPPED client, NOT source2's 34)
//	10  RESISTANCE_STAT     (5 short)
//
// A 34-byte BATTLE_STAT shifts the resistances by two bytes and their values
// land in the wrong fields; that trap is pinned by a test here.
//
// MEASURED AGAINST THE LIVE SERVER 2026-10-29, and the map below is why 125 is
// not in doubt. 83 ZC_UPDATE_ALL_STATUS packets across 13 live captures are ALL
// exactly 125 bytes, and this is the first of them
// (captures\charpanel2\20261029-165952\s2c_7062_60772.bin, +54046, a level-150
// 3rdScout with handle 0x000B6E06), read at the offsets below:
//
//	+0    2  cmd            3684
//	+2    4  handle         0x000B6E06
//	+6    1  LV             150            GetLv()
//	+7    8  HP             135400.0       GetHP()   (double, NOT a word)
//	+15   8  maxHP          124030.0       GetMHP()
//	+23   2  SP             995            GetSP()
//	+25   2  maxSP          995            GetMSP()
//	+27   8  EXP            59487183514    GetEXP()
//	+35   8  nextEXP        59487183514    gePCLevelTable::GetExp(LV+1)
//	+43   2  STR 30  +45 CON 60  +47 DEX 70  +49 INT 80  +51 AGI 70  +53 CHA 50
//	+55   4  BM STR 16  +59 CON 16  +63 DEX 16  +67 INT 16  +71 AGI 36  +75 CHA 36
//	         (GetBuffModifierStat = GetBMValue(".._BM") + Get.._WP(), so this block
//	          is GEAR-dependent: it changed 16/16/16/16/36/36 -> 19/19/19/21/39/39
//	          when the owner changed equipment during the capture)
//	+79   2  AR             33             GetAR()      SCR_Get_AR
//	+81   4  CATK           0              GetCATK()    SCR_Get_CATK (32-bit!)
//	+85   2  DR             61             GetDR()      SCR_Get_DR
//	+87   2  DEF            205            GetDEF()     SCR_Get_DEF
//	+89   2  HR             100            GetCHR()     SCR_Get_CHR (capped 100)
//	+91   2  PR             16             GetPR()
//	+93   2  CSPD           35             GetCSPD()    SCR_Get_CSPD
//	+95   2  BLK            0              GetBLK()
//	+97   2  CRT            15             GetCRT()
//	+99   2  IMP            0              GetIMP()
//	+101  2  IgnoreDEFPer   10             GetIgnoreDEFPer()
//	+103  2  DefIP          2              GetDefIP()
//	+105  2  FireIP         2              GetFireIP()
//	+107  2  IceIP          2              GetIceIP()
//	+109  2  LgtIP          2              GetLgtIP()
//	+111  2  PsyIP          2              GetPsyIP()
//	+113  2  MSPD           562            GetMSPD()    SCR_Get_MSPD (see speed.go)
//	+115  2  rFire          67             SCR_Get_RFIRE
//	+117  2  rIce           67             SCR_Get_RICE
//	+119  2  rLight         67             SCR_Get_RLGHT
//	+121  2  rState         51             SCR_Get_RSTAT
//	+123  2  rPsy           78             SCR_Get_RPSY
//
// The rules that decided these offsets, so a later reader does not re-guess them:
//
//  1. THE 123-BYTE TRAP. The project's capture tool prints a packet as
//     (offset, cmd, length), and for a FIXED-size packet that length EXCLUDES the
//     2 cmd bytes (analyze_capture.split_packets: `total - 2`). A 125-byte 3684
//     therefore prints as 123, and the byte AFTER the 10 resist shorts is rPsy
//     (+123), not the end of the packet. Reading 123 would drop rPsy - pinned by
//     TestLiveUpdateAllStatusIs125Bytes.
//  2. HR, not DR, is at +89: SCR_Get_CHR caps its result at 100, and every one of
//     the 83 live packets carries exactly 100 there. A 34-byte BATTLE_STAT would
//     read that byte as HR 205 (and DR 0), which the script cannot produce.
//  3. MSPD at +113 is the walk speed: 469 appears in the live set, and 469 is the
//     constant the live server's own moves animate at (speed.go, R63).
//  4. CATK is 4 bytes at +81: the 83 packets carry 0 (bare hands) and 2844..4565
//     (armed) there. As a WORD the block would be 34 and every later field would
//     shift - source2\geShared\include\geZCTypes.h still declares `WORD CATK`, so
//     that header is the one that disagrees with BOTH the live wire and the
//     shipped client's panel (`[EDI+0x247]`, a 4-byte read).
const (
	// CmdZCUpdateAllStatus is ZC_UPDATE_ALL_STATUS (3684), data\cmd_table.txt:1159.
	CmdZCUpdateAllStatus = 3684

	updHeader = 6 // cmd(2) + handle(4)

	basicStatLen      = 49
	buffModifierLen   = 24
	battleStatLen     = 36 // 18 WORDs in the shipped client
	resistanceStatLen = 10

	updBasicAt  = updHeader
	updBuffAt   = updBasicAt + basicStatLen   // 55
	updBattleAt = updBuffAt + buffModifierLen // 79
	updResistAt = updBattleAt + battleStatLen // 115

	// UpdateAllStatusSize is the WHOLE packet: 125.
	UpdateAllStatusSize = updHeader + basicStatLen + buffModifierLen +
		battleStatLen + resistanceStatLen
)

// Where each field sits INSIDE BATTLE_STAT (the 36-byte block at packet offset
// 79), read from the shipped client's own panel `FUN_007820ce` - the code that
// fills the Character Info labels (each line is the panel's own read):
//
//	word 0  +0x245 AR      "battleStat.AR"        0x782866
//	DWORD   +0x247 CATK    "battleStat.CATK"      0x7828b2  - FOUR bytes!
//	word 3  +0x24b DR                             0x7828fc
//	word 4  +0x24d DEF                            0x782948
//	word 5  +0x24f HR      "battleStat.HR"        0x78288c
//	word 6  +0x251 PR                             0x7828d6
//	word 7  +0x253 CSPD                           0x782922
//	word 8  +0x255 BLK, 9 +0x257 CRT, 10 +0x259 IMP
//	word 11 +0x25b IgnoreDEFPer, 12 +0x25d DefIP
//	words 13..16 +0x25f..+0x265 FireIP, IceIP, LgtIP, PsyIP
//	word 17 +0x267 MSPD                           0x782ab4
//
// THAT DWORD IS THE WHOLE "34 vs 36" TRAP: source2
// (geShared\include\geZCTypes.h:235-253) declares CATK as a WORD, so its struct
// is 34 bytes and every field from DR on sits two bytes earlier. The shipped
// client keeps CATK in 4 bytes, which is what makes the block 36 - and why a
// 34-byte build puts DR, DEF, HR and CSPD two bytes off and lands the
// resistances in the wrong place. The block is 17 FIELDS; there is no extra word.
const (
	battleAR           = 0
	battleCATK         = 2 // DWORD, not a WORD - see above
	battleDR           = 6
	battleDEF          = 8
	battleHR           = 10
	battlePR           = 12
	battleCSPD         = 14
	battleBLK          = 16
	battleCRT          = 18
	battleIMP          = 20
	battleIgnoreDEFPer = 22
	battleDefIP        = 24
	battleFireIP       = 26
	battleIceIP        = 28
	battleLgtIP        = 30
	battlePsyIP        = 32
	battleMSPD         = 34
)

// The EQUIP_SPOT indices the status rules read, in the client's own numbering
// (geSharedTypes.h:7-30, the same numbering emit_item_equip.py writes into
// data\item_equip.json and Character.Equip uses). 4 and 5 are HandSpotLeft /
// HandSpotRight in stance.go.
const (
	spotHead           = 0
	spotBody           = 1
	spotFace           = 2
	spotNeck           = 3
	spotRing1          = 6
	spotRing2          = 7
	spotGlove          = 8
	spotBoots          = 9
	spotBack           = 10
	spotCostume        = 11
	spotEar            = 12
	spotBelt           = 13
	spotAchieve        = 16
	spotLCostumeWeapon = 18
	spotRCostumeWeapon = 19
	spotArtifact       = 20
)

// StatusSheet is the DERIVED block one character's 3684 carries, in the game's
// own numbers. Every value comes from a rule in `ge\script\pc\get_status_pc.scp`
// applied to the client's own tables - never from a guess. A rule this server
// has no input for leaves its field ZERO and says why in the code's comment.
type StatusSheet struct {
	AR    float64 // SCR_Get_AR   :565
	ATK   float64 // SCR_Get_CATK :944
	DEF   float64 // SCR_Get_DEF  :1126
	DR    float64 // SCR_Get_DR   :632
	HR    float64 // SCR_Get_CHR  :1277
	// ASPD is the DELAY between one normal attack and the next, in MILLISECONDS -
	// not a "speed" that grows: the script's own header says so
	// (`get_status_pc.scp:1407` "ASPD (attack speed - delay time(1/1000sec)
	// between normal attacks)") and the reference server spends it as one
	// (`FieldChar_FSM.cpp:135` `m_nextAttackTime = timeGetTime() + GetAttackSPD()`,
	// and `FieldChar.cpp:1302` makes GetAttackSPD `GetASPD()`).
	ASPD  float64 // SCR_Get_ASPD :1408
	CSPD  float64 // SCR_Get_CSPD :1613
	MSPD  float64 // SCR_Get_MSPD :1764
	// MHP and MSP are max HP and max SP - the two numbers BASIC_STAT carries
	// rather than BATTLE_STAT, and the two the panel's bars are drawn against.
	// They live on this sheet so the spawn (3520), the status packet (3684) and
	// the panel can never disagree about a maximum (SCR_Get_MHP :308,
	// SCR_Get_MSP :442).
	MHP   float64
	MSP   float64
	RFIRE float64 // SCR_Get_RFIRE :2105
	RICE  float64 // SCR_Get_RICE  :2153
	RLGHT float64 // SCR_Get_RLGHT :2200
	RSTAT float64 // SCR_Get_RSTAT :2247
	RPSY  float64 // SCR_Get_RPSY  :2292
}

// gear is the worn items of one character, by the role the scripts give them.
type gear struct {
	rw, lw                      ItemStat // RH, LH
	body, head, face, neck, ear ItemStat
	glove, boots, belt          ItemStat
	back, costume, achieve      ItemStat
	artifact, lcos, rcos        ItemStat
	ring1, ring2                ItemStat
}

// wornGear reads one character's equipment slots through the item table.
func wornGear(c Character) gear {
	return gear{
		rw: wornItem(c, HandSpotRight), lw: wornItem(c, HandSpotLeft),
		body: wornItem(c, spotBody), head: wornItem(c, spotHead),
		face: wornItem(c, spotFace), neck: wornItem(c, spotNeck),
		ear: wornItem(c, spotEar), glove: wornItem(c, spotGlove),
		boots: wornItem(c, spotBoots), belt: wornItem(c, spotBelt),
		back: wornItem(c, spotBack), costume: wornItem(c, spotCostume),
		achieve: wornItem(c, spotAchieve), artifact: wornItem(c, spotArtifact),
		lcos: wornItem(c, spotLCostumeWeapon), rcos: wornItem(c, spotRCostumeWeapon),
		ring1: wornItem(c, spotRing1), ring2: wornItem(c, spotRing2),
	}
}


// num formats a term for the log: at most two decimals, and no trailing ".0" -
// so a stance's 1700 stays "1700" and a product of 1847.5643 reads "1847.56".
func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}


// StatusPacketFor is the ONE way a zone produces a 3684: it asks the game's own
// status script for the character's fifteen numbers (zone\status_script.go) and
// builds the packet from them. Entry (zone\zone.go) and an equipment change
// (zone\equip.go) both call this, so the two paths cannot disagree about a
// single field - the reference server has the same shape
// (CPlayerChar::SendAllStatus, PlayerChar.cpp:2292, called from both the entry
// path and SendCurStance after an equip change).
//
// A nil or failing script answers a ZERO sheet and logs which character and
// field failed: a zero is visible, a plausible wrong number is not.
func StatusPacketFor(c Character, st *StatusScript) ([]byte, StatusSheet) {
	if st == nil {
		// The suite's one source (nil in production, see zone\zone.go): a Handler
		// built inline by a test has no explicit Status, and a zero sheet would make
		// every packet assertion pass while proving nothing.
		st = defaultStatusForTests
	}
	sheet := StatusSheet{}
	if st != nil {
		sheet, _ = st.Sheet(c)
	}
	return BuildUpdateAllStatus(c, sheet), sheet
}

// BuildUpdateAllStatus makes the 125-byte ZC_UPDATE_ALL_STATUS for ONE
// character. The BASIC_STAT block is the very same 49 bytes the start-info
// record carries (basicStatBlock), because the client's handler OVERWRITES that
// part of its record: a zeroed copy here would wipe the level, the HP pair, the
// EXP and the six base stats the panel just showed.
func BuildUpdateAllStatus(c Character, sheet StatusSheet) []byte {
	b := make([]byte, UpdateAllStatusSize)
	binary.LittleEndian.PutUint16(b[0:], CmdZCUpdateAllStatus)
	binary.LittleEndian.PutUint32(b[2:], c.Handle)

	copy(b[updBasicAt:], basicStatBlock(c))
	// BUFF_MODIFIER_STAT (55..78) stays zero: this server has no buffs, no
	// mastery and no BM rows, and a buff value would have to be invented.

	putBattleWord(b, battleAR, sheet.AR)
	// ATK is the one 32-bit field of the block (`[EDI+0x247]`, 0x7828b2): the
	// live server's own panels read four and five digits, so a WORD here would
	// cut a high-level character's attack in half.
	binary.LittleEndian.PutUint32(b[updBattleAt+battleCATK:], clampDword(sheet.ATK))
	putBattleWord(b, battleDR, sheet.DR)
	putBattleWord(b, battleDEF, sheet.DEF)
	putBattleWord(b, battleHR, sheet.HR)
	putBattleWord(b, battleCSPD, sheet.CSPD)
	putBattleWord(b, battleMSPD, sheet.MSPD)
	// Every other BATTLE_STAT word (PR, BLK, CRT, IMP, IgnoreDEFPer, DefIP and the
	// four element IPs) is a number no rule this server has an input for produces,
	// so it is left 0 rather than guessed. The live packet is NOT zero there (PR 16,
	// CRT 15, IgnoreDEFPer 10, DefIP 2, FireIP..PsyIP 2 on the captured character),
	// and zone\status_live_test.go names each of those offsets as an allowed
	// difference instead of pretending they match.

	putResist(b, 0, sheet.RFIRE)
	putResist(b, 2, sheet.RICE)
	putResist(b, 4, sheet.RLGHT)
	putResist(b, 6, sheet.RSTAT)
	putResist(b, 8, sheet.RPSY)
	return b
}

// putBattleWord writes one BATTLE_STAT word. The wire type is a WORD, so a
// value is clamped to 0..65535 and floored rather than allowed to wrap into the
// neighbouring field.
func putBattleWord(b []byte, at int, v float64) {
	binary.LittleEndian.PutUint16(b[updBattleAt+at:], clampWord(v))
}

// putResist writes one RESISTANCE_STAT short. It is SIGNED on the wire (the
// script can produce -100), so it is clamped to -32768..32767.
func putResist(b []byte, at int, v float64) {
	binary.LittleEndian.PutUint16(b[updResistAt+at:], uint16(clampInt16(v)))
}

func clampWord(v float64) uint16 {
	v = math.Floor(v)
	switch {
	case v <= 0:
		return 0
	case v >= 65535:
		return 65535
	}
	return uint16(v)
}

// clampDword is clampWord for the 32-bit ATK field.
func clampDword(v float64) uint32 {
	v = math.Floor(v)
	switch {
	case v <= 0:
		return 0
	case v >= 4294967295:
		return 4294967295
	}
	return uint32(v)
}

func clampInt16(v float64) int16 {
	v = math.Floor(v)
	switch {
	case v <= -32768:
		return -32768
	case v >= 32767:
		return 32767
	}
	return int16(v)
}
