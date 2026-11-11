package protocol

import (
	"encoding/binary"
	"fmt"
	"strings"
)

//
// The body is 53 bytes after a 10-BYTE security header (command(2) + seq(4) + checksum(4));
// only VARIABLE packets carry a 12-byte one. Getting that wrong shifts every field by two:
// the old Python server read a slot id of 29537 and job 0, INSERTed the garbage and the
// client crashed on the next login (docker-port\history\barrack.md, item 11). The layout:
//
//	authCode[16]  WORD slotID  char name[17]  BYTE gender  DWORD job
//	BYTE headType  DWORD cardType  DWORD invIndex  DWORD costumeID
//
// 2 + 8 + 53 = 63, the size the client's own table gives (data\cmd_table_barrack.txt).
const (
	// CreateCharCmd is CB_CREATE_CHAR.
	CreateCharCmd uint16 = 18
	// CreateCharOKCmd is BC_CREATE_CHAROK.
	CreateCharOKCmd uint16 = 19

	// CreateCharSize is the whole request the client sends (table: 63).
	CreateCharSize = 63
	// CreateCharOKSize is the whole reply (table: 277).
	CreateCharOKSize = 277

	// createCharFixedHeader is PKS_SECURITY_DEFAULT: command(2) seq(4) checksum(4).
	createCharFixedHeader = 10
	// createCharAuthLen is the authCode every fixed barrack request carries.
	createCharAuthLen = 16
	// createCharSlotAt is the WORD slot id, right after the auth code.
	createCharSlotAt = createCharFixedHeader + createCharAuthLen // 26
	// createCharNameAt is the char name[17] that follows the slot.
	createCharNameAt = createCharSlotAt + 2 // 28
	// createCharNameLen is CHAR_NAME_LEN, the name field's width.
	createCharNameLen = 17
	// createCharGenderAt is the BYTE gender after the name.
	createCharGenderAt = createCharNameAt + createCharNameLen // 45
	// createCharJobAt is the DWORD class/job id after the gender byte.
	createCharJobAt = createCharGenderAt + 1 // 46
	// createCharHeadAt is the BYTE head type after the job dword.
	createCharHeadAt = createCharJobAt + 4 // 50

	// MaxCharSlots is MAX_SPC: the barrack's slot count in this generation.
	MaxCharSlots = 216
)

// CreateChar is a decoded CB_CREATE_CHAR.
type CreateChar struct {
	Slot     uint16
	Name     string
	Gender   byte
	Job      uint32
	HeadType byte
}

// ParseCreateChar decodes the fixed 63-byte request. The name is cut at its NUL, as the
// client itself does before using it.
func ParseCreateChar(plain []byte) (*CreateChar, error) {
	if len(plain) < CreateCharSize {
		return nil, fmt.Errorf("protocol: CB_CREATE_CHAR is %d bytes, got %d", CreateCharSize, len(plain))
	}
	if cmd := binary.LittleEndian.Uint16(plain[0:]); cmd != CreateCharCmd {
		return nil, fmt.Errorf("protocol: not a CB_CREATE_CHAR, command is %d", cmd)
	}
	return &CreateChar{
		Slot:     binary.LittleEndian.Uint16(plain[createCharSlotAt:]),
		Name:     strings.TrimSpace(cString(plain[createCharNameAt : createCharNameAt+createCharNameLen])),
		Gender:   plain[createCharGenderAt],
		Job:      binary.LittleEndian.Uint32(plain[createCharJobAt:]),
		HeadType: plain[createCharHeadAt],
	}, nil
}
