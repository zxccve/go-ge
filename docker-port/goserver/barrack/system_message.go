package barrack

// THE PACKET THAT OPENS THE CREATE-FAMILY SCREEN, taken from the live server.
//
// The client's own way into BARRACKMODE_CREATE_COMMANDER is a system message: its barrack
// dispatcher switches to that mode on cmd 3 (BC_MESSAGE), and a live server answering an
// account with NO family sends exactly this - a debug line saying the commander failed to
// load. Measured 2026-11-11 by capturing the owner's client against the live server
// (captures\v42-nofamily\20261111-143143, s2c, packet at +8645):
//
//	03 00  76 00  <- cmd 3, len 0x76 = 118 = the whole packet
//	08 00 00 00 | 03 26 00 00 | 1f 00 00 00 | 07 00 00 00 | 0b 00 00 00
//	0a 00 00 00 | 7e 00 00 00 | 03 00 00 00 | 3a 01 00 00 | 00 00 00 00   <- 40-byte header
//	"m_commanderLoader result fail, maybe no create commander (accID:noxy001)\0"
//
// The 40-byte header is copied VERBATIM: the capture is the only source for those ten
// u32s and nothing in this project decodes them yet. The text is the live server's own
// wording, with the account name swapped - the client does not appear to read it (the
// dialog opens on the command id), but a different string is untested, so it stays.
const (
	systemMessageCmd = 3
	// systemMessageHeader is the 41 bytes between the length word and the text (the live
	// packet has five zero bytes before the 'm', not four - measured off the capture).
	systemMessageHeaderHex = "08000000032600001f000000070000" +
		"000b0000000a0000007e000000030000003a0100000000000000"
)

// BuildNoCommanderMessage is the BC_MESSAGE a family-less session is answered with: cmd 3,
// length at +2 (the packet's own total size), the captured header, then the NUL-terminated
// text.
func BuildNoCommanderMessage(accountName string) []byte {
	text := "m_commanderLoader result fail, maybe no create commander (accID:" + accountName + ")"
	header := make([]byte, 0, 40)
	for i := 0; i+2 <= len(systemMessageHeaderHex); i += 2 {
		header = append(header, hexByte(systemMessageHeaderHex[i])<<4|hexByte(systemMessageHeaderHex[i+1]))
	}
	total := 4 + len(header) + len(text) + 1
	out := make([]byte, 0, total)
	out = append(out, byte(systemMessageCmd), byte(systemMessageCmd>>8))
	out = append(out, byte(total), byte(total>>8))
	out = append(out, header...)
	out = append(out, text...)
	out = append(out, 0)
	return out
}

func hexByte(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
