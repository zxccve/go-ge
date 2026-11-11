// Package-level: CB_CREATE_COMMANDER (11) and BC_CREATE_COMMANDEROK (12) - the family
// name, created by the CLIENT on the barrack screen a brand-new account lands on.
//
// THE LAYOUT IS THE ORIGINAL'S, not a guess, and the arithmetic is the check:
//
//	base PKS_SECURITY_DEFAULT   cmd(2) + seq(4) + checksum(4)          = 10
//	authCode[16]                                                      = 16
//	commanderName[COMMANDER_NAME_LEN = 17], NUL-terminated            = 17
//	                                                                    ----
//	                                                                    = 43
//
// 43 is exactly the size the client's own barrack table declares
// (`data\cmd_table_barrack.txt`, id 11), and the struct is
// `source2\geShared\include\geBCPacket.h:188` (`PKS_CB_CREATE_COMMANDER`).
//
// THE REPLY IS 12, and that is the second check: the table says 12, and
// `PKS_BC_CREATE_COMMANDEROK` (same header, :194) is citizenPoint(4) + rankID(1) +
// dutyID(1) + fame(2) + karma(2) = 10 plus the 2-byte command word. Nothing else is
// carried - the barrack itself is re-fetched by the client afterwards.
//
// offlinespada's v32 row file names both packets (0x000b / 0x000c, PROVEN for the
// name and length) and marks the body INFERRED; it has no field offsets, so it is a
// cross-check on the id and the size only. The body comes from the shipped C++.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	// CreateCommanderCmd is CB_CREATE_COMMANDER, the client's family creation.
	CreateCommanderCmd uint16 = 11
	// CreateCommanderOKCmd is BC_CREATE_COMMANDEROK.
	CreateCommanderOKCmd uint16 = 12

	// CreateCommanderSize is the whole packet the client sends (table: 43).
	CreateCommanderSize = 43
	// CreateCommanderOKSize is the whole reply (table: 12).
	CreateCommanderOKSize = 12

	// createCommanderNameAt is 10 (the security base) + 16 (authCode).
	createCommanderNameAt = 26
	// commanderNameLen is COMMANDER_NAME_LEN: 43 - 26. The original truncates the
	// last byte to NUL before using the name, so at most 16 characters survive.
	commanderNameLen = 17
)

// ErrEmptyCommanderName is what the original refuses with CheckEmptyName: a family
// with no name is not a family.
var ErrEmptyCommanderName = errors.New("protocol: the commander name is empty")

// CreateCommander is a decoded CB_CREATE_COMMANDER. Only the name is exposed: the
// authCode is the same security trailer every barrack packet carries and this server
// does not verify it (see Login.DigestVerified for why).
type CreateCommander struct {
	Name string
}

// ParseCreateCommander decodes the 43-byte request. The name is cut at its NUL and
// trimmed; a name that is empty after that is refused, as the original does.
func ParseCreateCommander(plain []byte) (*CreateCommander, error) {
	if len(plain) < CreateCommanderSize {
		return nil, fmt.Errorf("protocol: CB_CREATE_COMMANDER is %d bytes, got %d",
			CreateCommanderSize, len(plain))
	}
	if cmd := binary.LittleEndian.Uint16(plain[0:]); cmd != CreateCommanderCmd {
		return nil, fmt.Errorf("protocol: not a CB_CREATE_COMMANDER, command is %d", cmd)
	}
	raw := cString(plain[createCommanderNameAt : createCommanderNameAt+commanderNameLen])
	name := strings.TrimSpace(raw)
	if name == "" {
		return nil, ErrEmptyCommanderName
	}
	return &CreateCommander{Name: name}, nil
}

// BuildCreateCommanderOK builds the 12-byte reply. The five values are what the
// reference sends from the commander it just created (its citizen point, rank, duty,
// fame and karma); a brand-new family has 0 in all of them, which is what this server
// stores, so the caller passes the row's values when it has them.
func BuildCreateCommanderOK(citizenPoint uint32, rank, duty byte, fame, karma uint16) []byte {
	out := make([]byte, CreateCommanderOKSize)
	binary.LittleEndian.PutUint16(out[0:], CreateCommanderOKCmd)
	binary.LittleEndian.PutUint32(out[2:], citizenPoint)
	out[6] = rank
	out[7] = duty
	binary.LittleEndian.PutUint16(out[8:], fame)
	binary.LittleEndian.PutUint16(out[10:], karma)
	return out
}
