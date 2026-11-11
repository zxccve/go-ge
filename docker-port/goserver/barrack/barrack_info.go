package barrack

import (
	"encoding/binary"
	"math"
)

// BC_BARRACK_INFO (cmd 16) - the list of characters standing in the barrack.
//
// Ported from src\ge_barrack_server.py (build_barrack_info / build_pc_info),
// using the CLIENT's own offsets rather than the source2 header. That
// distinction is not academic: an earlier Python version used the header's
// 34-byte name and put job at +38, the client read job = 0 at +21 and crashed in
// geJobTable::Get(0).
//
// The header is ELEVEN bytes, not the six the source2 header implies: the
// shipped handler reads 0x0B for the header and then 0x157 (343) per record.
const (
	// BarrackInfoHeaderLen is what the client reads before the records.
	BarrackInfoHeaderLen = 11
	// PCInfoSize is one character record.
	PCInfoSize = 343

	pcInfoNameLen    = 17
	pcInfoOffGender  = 20
	pcInfoOffJob     = 21
	pcInfoOffEquip   = 25
	equipEntryStride = 12
	equipSpotLast    = 21
	pcInfoOffReinforce = pcInfoOffEquip + equipSpotLast*equipEntryStride
	pcInfoOffHeadType  = 298
	pcInfoOffLv        = 299
	// From +300 the record is NOT the four uint16s an earlier port assumed.
	// Measured on the live server's own BC_BARRACK_INFO (captures\barrack201-20261027,
	// cmd 16 @88,099, 10 records): HP is a FLOAT64 at 300 (312,323.0 / 345,813.0 read
	// clean; the zero bytes at 300-303 are its low mantissa), 308-315 are zero,
	// SP is a u32 at 316, EXP a u64 at 320 and the next level's cumulative total a
	// u64 at 328. Writing HP as uint16 there made the client read a denormal (~0)
	// and left the barrack's experience progress at 0.0000 %.
	pcInfoOffHP        = 300 // float64
	pcInfoOffSP        = 316 // uint32
	pcInfoOffExp       = 320 // uint64, cumulative
	pcInfoOffNextExp   = 328 // uint64, cumulative total for the next level
	pcInfoOffPromotion = 336
)

// CharInfo is one character in the barrack.
type CharInfo struct {
	Slot    uint16
	TeamPos byte
	Name    string
	Gender  bool
	Job     byte

	// Equip is the class id worn at each of the 21 spots. A zero means "no
	// item"; the caller supplies the client's own per-spot default if the game
	// needs one, because we must not invent an item id.
	Equip [equipSpotLast]uint32
	// Reinforce is the per-spot "+N" level. It was left at zero for a long time
	// in Python, and the barrack then showed no reinforcement on gear that has
	// it - a silent, cosmetic-looking lie.
	Reinforce [equipSpotLast]byte

	HeadType byte
	Level    byte
	// HP is the current HP. The wire carries it as a FLOAT64 at +300 (measured on
	// the live server's own records, history\barrack.md "PC_INFO tail"), so it is an
	// int64 here and converted on the way out - never truncated to 16 bits.
	HP int64
	// SP is the current SP, a u32 on the wire at +316.
	SP int64
	// Exp is the character's total experience; NextExp is the CUMULATIVE total
	// needed for the next level (the client subtracts Exp from it), i.e. the
	// table's TotalXp for level+1 - not a delta.
	Exp     int64
	NextExp int64

	Promotion     uint16
	CashPromotion uint16
}

// BuildBarrackInfo builds the packet: an 11-byte header then one 343-byte record
// per character, in the order given.
func BuildBarrackInfo(chars []CharInfo) []byte {
	out := make([]byte, BarrackInfoHeaderLen, BarrackInfoHeaderLen+len(chars)*PCInfoSize)
	total := BarrackInfoHeaderLen + len(chars)*PCInfoSize

	binary.LittleEndian.PutUint16(out[0:], 16)                 // command
	binary.LittleEndian.PutUint16(out[2:], uint16(total))      // whole packet
	out[4] = byte(len(chars))                                  // characterCount
	out[5] = 0                                                 // pad
	binary.LittleEndian.PutUint32(out[6:], 0)                  // existOnGoingMission
	out[10] = 0                                                // pad

	for _, c := range chars {
		out = append(out, buildPCInfo(c)...)
	}
	return out
}

// BarrackInfoHeaderLenJP is the JP 375639 header: TWELVE bytes, read by the
// client's own handler as FUN_005b0ff2(0xc) in FUN_0052198f (Ghidra, JP_PROJ,
// 2026-11-06): u16 opcode, u16 length, u16 record count at +4, u8 at +6,
// u32 at +7 (0 = clear all 288 slots), u8 at +0xB.
const BarrackInfoHeaderLenJP = 12

// PCInfoSizeJP is the JP record: 344 bytes (0x158), the size the handler passes
// to FUN_005b0ff2 for EACH record. It is our 2020 343-byte payload plus the
// per-record flag byte at +0x157 (the 2020 client set that byte to 1 inside
// CSession::SetSPcInfo after memmoving 343; the JP build reads the full 344 from
// the wire). Sending 343 left every record one byte short and made the client's
// CBinary parser run past the packet -> the CRT Runtime Error.
const PCInfoSizeJP = 344

// JPOnlyFirst and JPBareEquip are DIAGNOSTIC switches for the JP barrack:
// JPOnlyFirst sends only the first character, JPBareEquip ignores worn items
// and fills every slot with the client's "None" id. Together they isolate
// "record layout" from "one bad character/item" without another code edit.
var (
	JPOnlyFirst bool
	JPBareEquip bool
)

// jpEquipNone is the JP client's own "None" class id per equipment spot.
//
// It is our 2020 equipNone EXCEPT spot 16: 24000 is not in ANY JP item table
// (base or JPN), while every other value is present in the JP base tables. The
// JP artifact "None" is 48000 (NoArtifact), so the old artifact slot maps to it.
// A missing None id is an item lookup miss inside the create call - the crash.
var jpEquipNone = [equipSpotLast]uint32{
	17000, 14000, 18000, 15000, 10000, 10000, 16000, 16000, 19000,
	19500, 41000, 42000, 46000, 45000, 90000, 91000, 48000, 28000,
	60001, 60001, 48000,
}

// BuildBarrackInfoJP builds BC_BARRACK_INFO for the JP client: a 12-byte header
// then 344-byte character records.
func BuildBarrackInfoJP(chars []CharInfo) []byte {
	if JPOnlyFirst && len(chars) > 1 {
		chars = chars[:1]
	}
	total := BarrackInfoHeaderLenJP + len(chars)*PCInfoSizeJP
	out := make([]byte, BarrackInfoHeaderLenJP, total)
	binary.LittleEndian.PutUint16(out[0:], 16)            // command
	binary.LittleEndian.PutUint16(out[2:], uint16(total)) // whole packet
	binary.LittleEndian.PutUint16(out[4:], uint16(len(chars)))
	out[6] = 0                                // pad
	binary.LittleEndian.PutUint32(out[7:], 0) // 0 = clear every barrack slot
	out[11] = 0                               // pad
	for _, c := range chars {
		out = append(out, buildPCInfoJP(c)...)
	}
	return out
}

// buildPCInfoJP builds one 344-byte JP character record.
//
// The JP record is a HYBRID, read from FUN_005e96b0/FUN_0052af7d in JP_PROJ
// (2026-11-06): the v42-style head - name at +4, gender at +0x15 (21), class as
// a u32 at +0x16 (22), equipment at +0x1A (26) - followed by the 2020 tail:
// 21 x 12-byte equipment entries + 21 reinforce bytes ending at 299, level at
// +299, HP float64 at +300, SP u32 at +316, exp/next at 320/328, promotion at
// 336. Writing job as a BYTE at +21 left the u32 at +22 zero, and the client
// crashed in geJobTable::Get(0).
func buildPCInfoJP(c CharInfo) []byte {
	const (
		nameOff      = 4
		genderOff    = 21
		classOff     = 22
		equipOff     = 26
		reinforceOff = equipOff + equipSpotLast*equipEntryStride // 278
		levelOff     = 299
		flagOff      = 343
	)

	b := make([]byte, PCInfoSizeJP)
	binary.LittleEndian.PutUint16(b[0:], c.Slot)
	binary.LittleEndian.PutUint16(b[2:], uint16(c.TeamPos))
	copy(b[nameOff:nameOff+pcInfoNameLen], cstr(c.Name, pcInfoNameLen))
	if c.Gender {
		b[genderOff] = 1
	}
	binary.LittleEndian.PutUint32(b[classOff:], uint32(c.Job))

	for spot := 0; spot < equipSpotLast; spot++ {
		class := c.Equip[spot]
		if JPBareEquip || class == 0 {
			class = jpEquipNone[spot]
		}
		binary.LittleEndian.PutUint32(b[equipOff+spot*equipEntryStride:], class)
		b[reinforceOff+spot] = c.Reinforce[spot]
	}

	// The crash dump (2026-11-06, ge.exe_261106_173903.dmp) settles the tail: the
	// fatal was FUN_0056b524's bounds check with INDEX 100 - our character LEVEL,
	// which we had written at +299. FUN_0052af7d reads +299 as the create
	// struct's BoneType index (a 0/1 selector into the job's two-bone list), not
	// the level. The JP level belongs at +298 (the 2020 HeadType slot).
	b[298] = c.Level
	b[299] = 0 // BoneType index, not the level
	b[300] = c.HeadType
	binary.LittleEndian.PutUint64(b[304:], math.Float64bits(float64(c.HP)))
	binary.LittleEndian.PutUint32(b[312:], uint32(c.SP))
	binary.LittleEndian.PutUint64(b[316:], uint64(c.Exp))
	binary.LittleEndian.PutUint64(b[324:], uint64(c.NextExp))
	// +0x151 (337) is a u16 the client CLAMPS TO 5 (FUN_0052198f) and then looks
	// up with FUN_00526f8e; a zero is not a neutral value there. Send at least 1.
	promo := c.Promotion
	if promo == 0 {
		promo = 1
	}
	binary.LittleEndian.PutUint16(b[337:], promo)
	b[flagOff] = 1 // the 344th byte; FUN_0052af7d checks record[0x158] for it
	return b
}

// equipNone is the class id the CLIENT writes for an EMPTY equipment spot.
//
// A zero here is a crash, not a blank space: history\barrack.md crash #4 - the
// client's initialiser (FUN_0064b4e6) fills each spot with these ids, and a 0
// reaches FUN_00636a9a(0) (null item) and aborts. Values ported from
// src\ge_barrack_server.py `EQUIP_NONE`; note glove=19000 and boots=19500.
var equipNone = [equipSpotLast]uint32{
	17000, 14000, 18000, 15000, 10000, 10000, 16000, 16000, 19000,
	19500, 41000, 42000, 46000, 45000, 90000, 91000, 24000, 28000,
	60001, 60001, 48000,
}

// buildPCInfo builds one character record.
func buildPCInfo(c CharInfo) []byte {
	b := make([]byte, PCInfoSize)

	binary.LittleEndian.PutUint16(b[0:], c.Slot)
	b[2] = c.TeamPos
	copy(b[3:3+pcInfoNameLen], cstr(c.Name, pcInfoNameLen))
	if c.Gender {
		b[pcInfoOffGender] = 1
	}
	b[pcInfoOffJob] = c.Job

	for spot := 0; spot < equipSpotLast; spot++ {
		class := c.Equip[spot]
		if class == 0 {
			// Never send 0: an empty spot carries the client's own "None" id.
			class = equipNone[spot]
		}
		binary.LittleEndian.PutUint32(b[pcInfoOffEquip+spot*equipEntryStride:], class)
		b[pcInfoOffReinforce+spot] = c.Reinforce[spot]
	}

	b[pcInfoOffHeadType] = c.HeadType
	b[pcInfoOffLv] = c.Level
	binary.LittleEndian.PutUint64(b[pcInfoOffHP:], math.Float64bits(float64(c.HP)))
	binary.LittleEndian.PutUint32(b[pcInfoOffSP:], uint32(c.SP))
	binary.LittleEndian.PutUint64(b[pcInfoOffExp:], uint64(c.Exp))
	binary.LittleEndian.PutUint64(b[pcInfoOffNextExp:], uint64(c.NextExp))
	binary.LittleEndian.PutUint16(b[pcInfoOffPromotion:], c.Promotion)
	binary.LittleEndian.PutUint16(b[pcInfoOffPromotion+2:], c.CashPromotion)
	return b
}
