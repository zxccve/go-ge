package zone

import (
	"encoding/binary"
	"time"

	"golang.org/x/text/encoding/korean"
)

// ZC_MESSAGE (3596) is the client's PLAIN system line - the one that scrolls in
// the chat box. It is how the ORIGINAL server says things the client will not
// print itself.
//
// NOT USED BY SELL ANY MORE (2026-10-26): the owner asked for the cyan line to go,
// so a sale now relies on the client's own YELLOW lines - "You obtained N Vis."
// from ADD_ITEM_EXCHANGE and "<item> N have been sold." from REMOVE_ITEM_SELL.
// (An earlier comment claimed a REMOVE_ITEM prints nothing; that is false for
// removeType=SELL.) This builder stays for a future notice that has no other home.
//
// type 0 / level 0 make the client call NormalUI()->SysMsg(text) (GEPacket.cpp,
// packet::ServerMsg) - the cyan chat line - and that pair is what every caller
// passes unless it deliberately wants something else.
//
// THE BOX IS THE SAME PACKET (2026-10-30): `level` alone decides chat vs modal,
// and `type` decides whether the client keeps our text or throws it away and
// prints its OWN sentence from clientmessages.ies. So a caller that passes a
// non-zero level for a non-zero type is choosing the client's wording, and the
// text it hands over is a log note the player never sees.
//
// MESSAGE_TYPE_CURRENT_WAY_POINT IS 147 ON THE SHIPPED CLIENT - MEASURED, NOT READ
// FROM THE SOURCE (2026-10-30 evening, TYPE-HUNT -> MEASURED). The source header
// says 136 (source2\geShared\include\geSharedMessage.h:159, counted 0-based from
// MESSAGE_TYPE_GENERAL = 0), and 136 is WRONG on this client. The owner measured the
// ladder himself with the instrument, at level 3 (MESSAGE_LEVEL_WARNING), and the
// box drew a different sentence each time:
//
//	sent 136 -> "You can't participate in it as you did not satisfy its level
//	            limitation."   (the source's case 135,
//	                             MESSAGE_TYPE_LOBBY_NOT_PROPER_CREATE_LEVEL)
//	sent 135 -> "Password is incorrect."
//	                             (the source's case 125,
//	                             MESSAGE_TYPE_LOBBY_INCORRECT_PASSWORD)
//	sent 147 -> "You have chosen the waypoint of the current area."   <- RIGHT
//
// So the shipped binary's numbering is NEITHER the source's NOR a uniform shift: two
// values landed on 135's text and then on 125's text, and the right one is eleven
// values past the header's 136. DO NOT re-derive this from the source, ever - the
// header is not the binary. Measure it: the dev-only chat command `/typemsg <type>
// <level>` (zone\dev_message.go) sends one ZC_MESSAGE with the numbers typed, and for
// any type but 0 the client OVERWRITES our text with its own sentence - so the
// sentence that appears on screen names the type. That is how this number was
// settled in one sitting, and how the next message type gets settled. The procedure
// and this ladder are in history\protocol-recovery.md -> the modal-box section.
const (
	CmdZCMessage          = 3596 // server -> client: VAR
	MessageTypeGeneral    = 0
	MessageLevelGeneral   = 0

	// MessageTypeCurrentWayPoint is MESSAGE_TYPE_CURRENT_WAY_POINT on the SHIPPED
	// client: MEASURED 147 with `/typemsg 147 3`, not read from geSharedMessage.h
	// (which says 136 and is wrong - see the ladder above). The client answers it
	// with UI_ALERT_MSG_203 -> 10000205, "You have chosen the waypoint of the current
	// area." (clientmessages.xml:214, dictionary_local.xml:207), so the text our
	// caller passes is never printed: it is a log note that goes on the wire.
	MessageTypeCurrentWayPoint = 147

	messageTMStructSize   = 36 // 9 ints, packed (geZCPacket.h is #pragma pack(1))
	messageHeaderFixedLen = 4 + 4 + 1 + messageTMStructSize

	// MessageLevelWarning is MESSAGE_LEVEL_WARNING (geSharedMessage.h:451) - the level
	// the reference server uses for the waypoint answer, and the level we send: level
	// >= 1 pops a MODAL box, and 3 is the one whose default title is "Message"
	// (GEPacket.cpp:1780-1783) - the title the owner saw on screen with `/typemsg 147 3`.
	MessageLevelWarning = 3
)

// BuildMessage builds ZC_MESSAGE: cmd(2) len(2) type(4) level(1) tm(36) text+NUL.
//
// The layout is the Python oracle's (`build_system_message` in
// src\zone_server.py, around line 1680) and the length word is the WHOLE packet:
//
//	Offset  Size  Field
//	0       2     command = 3596
//	2       2     length  = 4 + body, i.e. the packet size
//	4       4     type    = msgType (0 = general: the client prints OUR text)
//	8       1     level   = level (0 = chat line; >= 1 = modal box)
//	9       36    tm      = struct tm as 9 little-endian int32
//	45      ..    text (cp949) + one NUL terminator
//
// `tm` is the C runtime struct - year counted from 1900 and month from 0 - and
// is packed, so it is 36 bytes with no padding. The client reads it into a
// SYSTEMTIME-ish display; a wrong one only misdates the line, so the local clock
// is passed straight through. tm_isdst has no Go equivalent and stays 0.
//
// type and level are arguments because they ARE the presentation (2026-10-30):
// MessageTypeGeneral/MessageLevelGeneral keep the plain cyan line, where the client
// prints OUR text. Any other pair is a caller deliberately asking for the client's
// OWN sentence (a non-zero type) and/or a modal box (a level above 0) - and the
// waypoint "already here" answer is exactly that pair now: 147 / 3, MEASURED. See
// the ladder above.
func BuildMessage(text string, msgType uint32, level byte) []byte {
	body := make([]byte, 0, 128)
	typeBuf := make([]byte, 5)
	binary.LittleEndian.PutUint32(typeBuf[0:], msgType)
	typeBuf[4] = level
	body = append(body, typeBuf...)
	body = append(body, buildTM(time.Now())...)
	body = append(body, cp949(text)...)
	body = append(body, 0) // NUL terminator

	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint16(out[0:], CmdZCMessage)
	binary.LittleEndian.PutUint16(out[2:], uint16(4+len(body)))
	return append(out, body...)
}

// buildTM is the 9-int time block: sec, min, hour, mday, mon, year, wday, yday,
// isdst - a C `struct tm`, with two fields packed the way PYTHON fills them,
// because that is the only thing the client has ever been sent:
//
//	tm_wday  MONDAY is 0 (Python's convention, not C's Sunday=0)
//	tm_yday  is 1-BASED (Python's convention, not C's 0-based)
//
// The other seven agree in both languages. tm_isdst has no Go equivalent and is
// 0, which is also what Python reports here (no DST), so the bytes match the
// oracle exactly rather than "morally" matching it.
func buildTM(t time.Time) []byte {
	out := make([]byte, messageTMStructSize)
	fields := [9]int32{
		int32(t.Second()),
		int32(t.Minute()),
		int32(t.Hour()),
		int32(t.Day()),
		int32(t.Month()) - 1,              // Python's tm_mon is 1-12; the oracle subtracts 1
		int32(t.Year()) - 1900,            // both count from 1900
		int32((int(t.Weekday()) + 6) % 7), // Monday=0, as Python's tm_wday
		int32(t.YearDay()),                // already 1-based, as Python's tm_yday
		0,                                 // tm_isdst
	}
	for i, v := range fields {
		binary.LittleEndian.PutUint32(out[i*4:], uint32(v))
	}
	return out
}

// cp949 encodes a system line the way the Python oracle does: cp949 with '?'
// where the code page cannot carry a character.
//
// Why not latin1, which the dialog builder uses: the oracle really does encode
// ZC_MESSAGE as cp949, and the two only differ for non-ASCII text. Every item
// name in data\item_prices.json is ASCII (checked over all 25,448 rows), so a
// sale line is byte-identical either way today - this just keeps it right if a
// Korean client's own string ever reaches here.
func cp949(s string) []byte {
	out := make([]byte, 0, len(s))
	enc := korean.EUCKR.NewEncoder()
	for _, r := range s {
		if r < 0x80 {
			out = append(out, byte(r))
			continue
		}
		b, err := enc.Bytes([]byte(string(r)))
		if err != nil || len(b) == 0 {
			// One '?' for one character, the same as Python's "replace": never
			// drop it, or the rest of the line would shift.
			out = append(out, '?')
			continue
		}
		out = append(out, b...)
	}
	return out
}
