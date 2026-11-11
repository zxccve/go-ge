package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// CB_LOGIN (4) and its reply BC_LOGINOK (8).
//
// Everything here comes from two real captures plus the shipped struct
// (source2\geShared\include\geBCPacket.h: PKS_CB_LOGIN : PKS_SECURITY_VARSIZE),
// not from guessing. The packet is 812 bytes on the wire even though the cipher
// hands us 816, because the cipher pads to a whole 8-byte block.
//
// TWO THINGS ARE NOT STABLE BETWEEN LOGINS, and both are measured:
//
//  1. Everything after the NUL of a fixed-size text field. The client sends
//     stack leftovers there: only 258 of 816 bytes matched between two logins of
//     the same account. So this parser reads FIELDS, never compares a buffer.
//  2. The password digest. The same account sent a different 16-byte value on
//     each login while the database holds one static MD5 hex. It cannot be
//     verified by comparison - see Login.DigestVerified.

const (
	// Field widths, empirical and confirmed against captures. ACC_NAME_LEN in
	// particular is NOT what the name looks like: the name is 8 bytes and the
	// rest of the field is garbage, but the WIDTH is what locates the next field.
	accNameLen      = 218
	pwdLen          = 17 // 16 digest bytes + NUL
	nationLen       = 8
	loginHeaderLen  = 12 // cmd(2) seq(4) checksum(4) len(2)
	loginPacketSize = 812
	loginOKSize     = 57 // the client's own table: BC_LOGINOK is fixed 57
)

// ErrShortLogin means the buffer is too small to be a CB_LOGIN.
var ErrShortLogin = errors.New("protocol: buffer is too short to be a CB_LOGIN")

// Login is a decoded CB_LOGIN. Only the fields a server actually uses are
// exposed; the volatile parts are deliberately not carried around.
type Login struct {
	Cmd      uint16
	Seq      uint32
	Checksum uint32
	Len      uint16 // the packet's own length word: the WHOLE packet, padding excluded

	// AccountID is the login name the client sent, already cut at its NUL.
	AccountID string

	// Digest is the 16 bytes the client sent in the password field. It is NOT
	// comparable to a stored hash: see DigestVerified.
	Digest []byte

	ServerID uint16
	Version  uint32
	Nation   string

	// DigestVerified is always false today. It exists so that the caller has to
	// decide what to do instead of quietly pretending the login was checked.
	DigestVerified bool
}

// ParseLogin decodes a CB_LOGIN from decrypted bytes. The buffer may be longer
// than the packet (cipher padding); only Len bytes are considered.
func ParseLogin(plain []byte) (*Login, error) {
	if len(plain) < loginHeaderLen {
		return nil, ErrShortLogin
	}
	l := &Login{
		Cmd:      binary.LittleEndian.Uint16(plain[0:]),
		Seq:      binary.LittleEndian.Uint32(plain[2:]),
		Checksum: binary.LittleEndian.Uint32(plain[6:]),
		Len:      binary.LittleEndian.Uint16(plain[10:]),
	}
	if l.Cmd != 4 {
		return nil, fmt.Errorf("protocol: not a CB_LOGIN, command is %d", l.Cmd)
	}
	// Trust the packet's own length when it is sane, so padding is ignored.
	body := plain
	if int(l.Len) >= loginHeaderLen && int(l.Len) <= len(plain) {
		body = plain[:l.Len]
	}
	if len(body) < loginHeaderLen+accNameLen+pwdLen+2+4+nationLen {
		return nil, ErrShortLogin
	}

	l.AccountID = cString(body[12 : 12+accNameLen])
	digestAt := 12 + accNameLen
	l.Digest = append([]byte(nil), body[digestAt:digestAt+pwdLen-1]...)
	l.ServerID = binary.LittleEndian.Uint16(body[digestAt+pwdLen:])
	l.Version = binary.LittleEndian.Uint32(body[digestAt+pwdLen+2:])
	l.Nation = cString(body[digestAt+pwdLen+6 : digestAt+pwdLen+6+nationLen])
	return l, nil
}

// cString cuts a fixed-size text field at its first NUL. Everything after it is
// uninitialised buffer, not data.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// BuildLoginOK builds the reply: command(2) account(4) session(4) ident padded
// to exactly 57 bytes.
//
// The client's stream is kept in sync by the LENGTH, not by the text: a shorter
// or longer packet leaves stray bytes and every later packet is misread.
func BuildLoginOK(accountID, sessionID uint32, ident string) []byte {
	b := make([]byte, loginOKSize)
	binary.LittleEndian.PutUint16(b, 8) // BC_LOGINOK
	binary.LittleEndian.PutUint32(b[2:], accountID)
	binary.LittleEndian.PutUint32(b[6:], sessionID)
	copy(b[10:], ident) // copy truncates if the name is longer than the space
	return b
}

// BuildLoginOKJP is the JP 375639 form: the same fields, but the client's own
// table says BC_LOGINOK is 61 bytes (four more than our 2020 client's 57). The
// extra room is at the end of the ident field; a 61-byte packet keeps the JP
// client's stream in sync, and a 57-byte one leaves four bytes of the next
// packet to be misread.
func BuildLoginOKJP(accountID, sessionID uint32, ident string) []byte {
	const loginOKSizeJP = 61
	b := make([]byte, loginOKSizeJP)
	binary.LittleEndian.PutUint16(b, 8)
	binary.LittleEndian.PutUint32(b[2:], accountID)
	binary.LittleEndian.PutUint32(b[6:], sessionID)
	copy(b[10:], ident)
	return b
}
