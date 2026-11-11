package zone

import (
	"encoding/binary"
	"math"
)

// The layout constants below are copied from src\zone_server.py, which measured
// them against the real client. They are NOT guesses: SI_SIZE is asserted by the
// Python builder, and the client's own GeSession::SetMyPcInfo walks the records.
const (
	headerSize    = 36  // cmd(2)+total(2)+tick(4)+2 bytes+party(17)+count(1)+time(4)+rsv(4)
	startInfoSize = 485 // one character record; the client reads it field by field

	nameLen    = 17 // CHAR_NAME_LEN and PARTY_NAME_LEN are both 17
	seqNameLen = 17 // the login id in CZ_START_GAME

	siHandle       = 0
	siName         = 4
	siAppearance   = 21
	apName         = 0
	apGender       = 17
	apJob          = 18
	apHeadType     = 22
	apEquipment    = 68
	// apLevel and apPromotionLevel are the appearance's own {level, promotionLevel}
	// bytes - the pair the client uses to draw a character's rank ("High Master")
	// instead of a bare level, and the ONLY place a REMOTE character's level travels
	// in a spawn (3520 carries no BASIC_STAT at all).
	//
	// MEASURED, not inherited from a struct guess: the live capture kept as
	// testdata\live_fighter_spawn.bin has the u16 0x0596 at packet +689 - low byte
	// 150 (level), high byte 5 (promotion) - for a character whose OWN 3684 in the
	// same capture set reads level 150 and maxSP 995 (= 195 + 5x150 + 5x10, i.e.
	// promotion 5). Two independent numbers landing on those two bytes is the pin.
	// This build's headers do not give the offset: the equipment array is 21 x 12
	// bytes (68..319), so the source's field order runs out before the tail here.
	apLevel          = 383
	apPromotionLevel = 384
	equipSpotLast    = 21
	equipEntrySize   = 12
	siInitPos      = 416
	siBasicStat    = 428
	bsLevel        = 0
	bsHP           = 1  // float64
	bsMaxHP        = 9  // float64
	bsSP           = 17 // uint16
	bsMaxSP        = 19 // uint16
	bsExp          = 21 // int64
	bsNextExp      = 29 // int64
	bsStats        = 37 // six uint16: STR, CON, DEX, INT, AGI, CHA
	siStanceCount  = 477
	siSkillCount   = 479
	siOptionCount  = 481

	// AppearanceLen is the size of the client's APPEARANCE_PC: the block that
	// starts at siAppearance and ends exactly where the initial position begins
	// (21 + 395 = 416 = siInitPos). It is the SAME 395 bytes ZC_UPDATED_PCAPPEARANCE
	// (3546) carries, which is why 3546 is 2 + 4 + 395 = 401 bytes
	// (source2\geShared\include\geZCPacket.h:1365 PKS_ZC_UPDATED_PCAPPEARANCE,
	// 401 in data\cmd_table_live.txt:1213).
	AppearanceLen = 395

	// apNameplate and apNameplateLen are the SECOND name string inside the
	// appearance block, 17 bytes at block offset 35 (packet offset 341 of a spawn,
	// which is why players.go calls it spAppearanceName). The client draws it over
	// the remote player's head, so an empty one means no nameplate.
	apNameplate    = 35
	apNameplateLen = 17
)

// appearanceBlock is the ONE builder of a character's APPEARANCE_PC - the 395
// bytes that describe what a body looks like: name, gender, job, head type, the
// 21-slot equipment array, the nameplate.
//
// TWO packets carry it and they must never disagree:
//
//	ZC_SPAWN_STANDINGPC  3520, at +306 (players.go BuildSpawnStandingPC)
//	ZC_UPDATED_PCAPPEARANCE 3546, at +6  (appearance.go BuildUpdatedPCAppearance)
//
// 3546 is what REDRAWS a body after an equip (the reference's
// BroadcastUpdateAppearance, PlayerChar.cpp:458-467), so a second, hand-built
// copy of this block would be a model that drifts from the spawn. It is a slice
// of BuildStartInfo instead - the very record the client has accepted since the
// first login - so every field comes from one place:
//
//	+0   Name[17]        c.Name
//	+17  Gender          c.Gender
//	+18  Job (DWORD)     c.Job & 0xFF, read as a DWORD by the client
//	+22  HeadType        c.HeadType
//	+68  Equipment[21]   c.Equip[spot], with EquipNone[spot] for an empty slot -
//	                     NEVER 0, the class the client dereferences (see EquipNone)
//	+35  nameplate[17]   c.Name, written here so both packets get it
//
// Every other byte of APPEARANCE_PC (commander name, party name, phrase, team
// index, AR/DR, level, buff colour) is left as BuildStartInfo leaves it - zero -
// which is the state the spawn already ships in game. They are cosmetic on a
// remote body and are not invented here.
func appearanceBlock(c Character) []byte {
	start := BuildStartInfo(c)
	out := make([]byte, AppearanceLen)
	copy(out, start[siAppearance:siAppearance+AppearanceLen])
	putCStr(out[apNameplate:apNameplate+apNameplateLen], c.Name)
	return out
}

// EquipNone is the item class each equipment slot holds when NOTHING is worn.
//
// Read from the client's own initializer (FUN_0064b4e6 writes 17000 into slot 0,
// 14000 into the next, stride 12). A ZERO here is not "nothing": the client looks
// the id up, gets no item property object, and then dereferences it -
// `CMP dword ptr [EDI+0x1dc]` with EDI=0 in age_dll_crash.log, which is exactly
// what crashed the first two live tests.
var EquipNone = [equipSpotLast]uint32{
	17000, 14000, 18000, 15000, 10000, 10000, 16000, 16000, 19000,
	19500, 41000, 42000, 46000, 45000, 90000, 91000, 24000, 28000,
	60001, 60001, 48000,
}

// BuildStartInfo makes one START_INFO record: a fixed 485-byte body followed by
// the stance and skill blocks.
//
// The blocks are part of the RECORD, not of the packet: the counts at 477 and 479
// say how many follow, and the client reads exactly that many. Sending 485 bytes
// with a count of 0 is what crashed the client - see the note in stance.go.
func BuildStartInfo(c Character) []byte {
	b := make([]byte, startInfoSize)

	binary.LittleEndian.PutUint32(b[siHandle:], c.Handle)
	putCStr(b[siName:siName+nameLen], c.Name)

	ap := siAppearance
	putCStr(b[ap+apName:ap+apName+nameLen], c.Name)
	b[ap+apGender] = c.Gender
	// Job is read as a DWORD by the client (FUN_005dfcbd); the three bytes after
	// it must stay zero.
	binary.LittleEndian.PutUint32(b[ap+apJob:], c.Job&0xFF)
	b[ap+apHeadType] = c.HeadType
	// Equipment: a worn item wins; an empty slot takes the client's own "None"
	// class id, NEVER zero. Zero makes the client dereference a null item
	// property object - the crash this project paid for twice in one evening.
	for spot := 0; spot < equipSpotLast; spot++ {
		cls := EquipNone[spot]
		if c.Equip[spot] != 0 {
			cls = c.Equip[spot]
		}
		binary.LittleEndian.PutUint32(
			b[ap+apEquipment+spot*equipEntrySize:], cls)
	}

	binary.LittleEndian.PutUint32(b[siInitPos:], uint32(c.Position[0]))
	binary.LittleEndian.PutUint32(b[siInitPos+4:], uint32(c.Position[1]))
	binary.LittleEndian.PutUint32(b[siInitPos+8:], uint32(c.Position[2]))

	// The LEVEL and the PROMOTION LEVEL, in the appearance block where the client
	// reads them (apLevel / apPromotionLevel, measured - see their comment). A zero
	// here is not harmless: it is what made the party card print a bare level instead
	// of the character's rank ("High Master"), and it is the only level a REMOTE
	// character's spawn carries. Written from the SAME Character fields the 3093 uses,
	// so the rank and the promotion window cannot disagree.
	b[ap+apLevel] = c.Level
	b[ap+apPromotionLevel] = c.Promotion

	// The six base stats, the HP pair, the EXP and the level all come from ONE
	// builder, so this record and ZC_UPDATE_ALL_STATUS (3684) can never disagree
	// about them - 3684 OVERWRITES this same block in the client's record.
	copy(b[siBasicStat:], basicStatBlock(c))

	binary.LittleEndian.PutUint16(b[siStanceCount:], uint16(len(c.Stances)/14))
	binary.LittleEndian.PutUint16(b[siSkillCount:], uint16(len(c.Skills)/10))
	binary.LittleEndian.PutUint32(b[siOptionCount:], 0)

	// Copy, do not append: appending to c.Stances would write into the caller's
	// slice when it has spare capacity.
	out := make([]byte, 0, len(b)+len(c.Stances)+len(c.Skills))
	out = append(out, b...)
	out = append(out, c.Stances...)
	out = append(out, c.Skills...)
	return out
}

// basicStatBlock is the client's 49-byte BASIC_STAT: the block at wire +428 of
// START_INFO and at +6 of ZC_UPDATE_ALL_STATUS 3684.
//
//	+0  BYTE  LV        +17 WORD  SP        +21 int64 EXP
//	+1  double HP       +19 WORD  maxSP     +29 int64 nextEXP
//	+9  double maxHP                         +37 6 x WORD STR,CON,DEX,INT,AGI,CHA
//
// It is ONE builder because BOTH packets write the same record in the client: a
// zeroed copy in 3684 would wipe the level, HP, EXP and the six base stats the
// panel had just drawn. See stats_oracle_test.go for the byte-for-byte proof of
// the start-info half.
func basicStatBlock(c Character) []byte {
	b := make([]byte, basicStatLen)
	// The six base stats are the JOB's own row; the maximum HP is the game's formula
	// and it is ALREADY ON the character - Handler.refreshSheet put SCR_Get_MHP and
	// SCR_Get_MSP there before any packet was built (zone\status_script.go), and the
	// store's value is only a stand-in for a character that never reached that path.
	// A zero maximum would draw an EMPTY bar for a character who is alive, so the
	// stored maximum stands in that case (a missing job row logs itself).
	stats := BaseStats(c)
	maxHP := c.MaxHP
	hp := c.HP
	if maxHP <= 0 {
		maxHP = c.HP
	}
	if hp > maxHP {
		// The stored HP can sit ABOVE the formula's maximum - Grace is stored at
		// 17300 with a maximum of 9140 - so an unclamped write would draw a bar past
		// its own end. The Python builder clamps the same way
		// (zone_server.build_start_info: hp = min(hp, max_hp)).
		hp = maxHP
	}
	b[bsLevel] = c.Level
	putFloat64(b[bsHP:], hp)
	putFloat64(b[bsMaxHP:], maxHP)
	binary.LittleEndian.PutUint16(b[bsSP:], c.SP)
	binary.LittleEndian.PutUint16(b[bsMaxSP:], c.MaxSP)
	putInt64(b[bsExp:], c.Exp)
	// BS_NEXTEXP stays 0: the Python server sends 0 too, and an exp curve invented
	// here would be a number nobody chose.
	putInt64(b[bsNextExp:], 0)
	// The six base stats, in the CLIENT's order (STR, CON, DEX, INT, AGI, CHA) as
	// WORDs, starting at +37. Without them the Character Info panel shows 0 for
	// those six lines. The panel's OTHER numbers (ATK, DEF, A.R., D.R., the
	// resistances) are NOT computed by the client: they arrive in
	// ZC_UPDATE_ALL_STATUS 3684 - see zone\status.go, history\zone-entry.md and
	// R61b. See zone\stats.go for where the numbers and the order come from.
	for i, word := range stats {
		binary.LittleEndian.PutUint16(b[bsStats+i*2:], word)
	}
	return b
}

// BuildStartGameOK wraps the records in ZC_START_GAMEOK (3591).
//
// Layout, in order: tick(float32), permission(1), connectionPos(1),
// partyName(17), count(1), serverTime(4), reserved(4), then the records. The
// length word counts the WHOLE packet, so it is 4 + everything after it.
func BuildStartGameOK(records [][]byte) []byte {
	body := make([]byte, headerSize-4) // 32 bytes: tick(4) perm(1) connPos(1) party(17) count(1) time(4) rsv(4)
	// tick stays 0: the live server's value is a frame timer the client only uses
	// for the loading bar.
	// permission (body[4]) and connectionPos (body[5]) stay 0.
	// party name: empty, 17 bytes at body[6:23].
	body[23] = byte(len(records)) // count
	// serverTime at body[24] and reserved at body[28] stay zero: the client uses
	// them only for the server clock display, and a wrong clock is worse than none.

	out := make([]byte, 4, 4+len(body)+len(records)*startInfoSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCStartGameOK)
	out = append(out, body...)
	for _, rec := range records {
		out = append(out, rec...)
	}
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// BuildEnterLegacyObjects answers CZ_GAME_READY. It is TWO BYTES: the packet has
// no payload at all, and the client leaves the "moving to <map>" screen when it
// arrives. Without it the session looks connected but never playable.
func BuildEnterLegacyObjects() []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, CmdZCEnterLegacyObjs)
	return out
}

// ---- small helpers -------------------------------------------------------

func putCStr(dst []byte, s string) {
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

func trimNul(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func putFloat64(dst []byte, v float64) {
	binary.LittleEndian.PutUint64(dst, math.Float64bits(v))
}

func putInt64(dst []byte, v int64) {
	binary.LittleEndian.PutUint64(dst, uint64(v))
}
