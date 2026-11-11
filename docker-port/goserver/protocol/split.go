package protocol

import (
	"encoding/binary"
)

// Frame is one packet taken off the wire.
type Frame struct {
	Cmd  uint16
	Size int    // bytes in Data, which is the whole packet
	Data []byte // the packet itself, starting at its command word
}

// MinSecurityFrame is the shortest frame a client can send: the security
// header alone, cmd(2) + seq(4) + checksum(4). It is read off the tables, not
// guessed - the smallest fixed-size CZ_ row in data\cmd_table_live_z.txt is 10
// (CZ_REQ_UPDATE_CURRENT_TAXSUM) and the smallest fixed-size CB_ row in
// data\cmd_table_barrack.txt is also 10 (CB_CANCEL_INPUTOTP). A variable-size
// one is longer still: it needs 12 before its length word at +10 is even there.
//
// This is the floor for CLIENT data only. A server->client packet may be 2
// bytes (ZC_DIALOG_CLOSE 3665), so the rule must never be applied to that side.
const MinSecurityFrame = 10

// Split cuts frames off the front of buf and reports how many bytes it used.
//
// This is the ZONE flavour: it assumes a variable-size packet keeps its length
// at +2, which is what every packet in data\cmd_table.txt does. For the
// barrack id space use Spec.Split, because CB_LOGIN keeps its length at +10.
//
// It passes no minimum frame size: a bare Table is just ids and sizes, and its
// callers are tests and probes that cut whatever id space they built. The
// padding rule belongs to Spec.Split, which is what the two servers use.
func (t Table) Split(buf []byte) (frames []Frame, consumed int) {
	return splitWith(buf, t.Size, func(uint16) (int, bool) { return LenAtPlain, true }, 0)
}

// Split cuts frames off the front of buf using each command's own length
// offset. Use this for a table that carries packet shapes.
//
// Every table loaded with LoadSpec is a CLIENT->SERVER id space in practice -
// the zone and the barrack both cut incoming data with it - so a remainder too
// short to be a client frame is treated as cipher padding (see splitWith).
func (s *Spec) Split(buf []byte) (frames []Frame, consumed int) {
	return splitWith(buf, s.Size, s.LenAt, MinSecurityFrame)
}

// allZero reports whether every byte is zero. The cipher pads the plaintext to
// a whole 8-byte block, so a trailing run of zeros is padding, never a packet -
// and there is no command 0 to confuse it with.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// splitWith is the one implementation both splitters use.
// Anything it cannot complete stays unconsumed: a half-arrived packet is not
// an error, it is a packet that has not finished arriving. The caller keeps
// those bytes and appends the next read to them.
//
// The rules are mirrored from src\test_golden.py (split_packets) and must be
// changed in both places at once. The one exception is the padding floor,
// which is Go-only for now: the Python splitter is a test harness and its
// caller decides what to do with a stray frame, while the Go zone LOGGED it
// (ROADMAP R45). The rules:
//
//   - a command the table does not know ends the split; everything left is
//     returned as one frame, because there is no way to find its end - unless
//     what is left is shorter than minFrame, which makes it padding
//   - size 0 means variable size: the length at the command's own offset counts
//     the whole packet, its own 2 bytes included
//   - otherwise the packet is exactly that many bytes
//
// minFrame is the shortest frame the stream can legally carry, 0 meaning "no
// floor known". It is what tells padding from a packet.
func splitWith(buf []byte, sizeOf func(uint16) (int, bool), lenAt func(uint16) (int, bool), minFrame int) (frames []Frame, consumed int) {
	i := 0
	for i+2 <= len(buf) {
		cmd := binary.LittleEndian.Uint16(buf[i:])

		size, known := sizeOf(cmd)
		if !known {
			// Zero bytes are the cipher's padding to the next 8-byte block,
			// not a packet: the client pads the plaintext and there is no
			// command 0. Consume them and stop.
			if allZero(buf[i:]) {
				return frames, len(buf)
			}
			// The same padding, but not zeros. The cipher pads to a whole
			// 8-byte block, so a chunk can trail up to 7 bytes after its last
			// packet, and those 7 bytes are NOT zeros - they are whatever the
			// client left in its buffer. Read as a command word they look like
			// an id the table has never heard of, which is exactly what made
			// the zone log "cmd 47637 not handled yet, 6 bytes" once a second
			// (ROADMAP R45) and bury a real CZ_CHAT among them.
			//
			// The test used here is the LENGTH alone, and it is safe in one
			// direction only: no client frame is shorter than MinSecurityFrame,
			// so a remainder under that cannot be a packet, real or truncated.
			// A known command id never reaches this branch, so a genuinely
			// truncated packet is still held back (fixed size) or reported
			// (uncuttable), exactly as before.
			if minFrame > 0 && len(buf)-i < minFrame {
				return frames, len(buf)
			}
			// A real unknown command: nothing tells us where it ends, so take
			// the rest and stop. The caller decides what to do with it.
			frames = append(frames, Frame{Cmd: cmd, Size: len(buf) - i, Data: buf[i:]})
			return frames, len(buf)
		}

		if size == 0 {
			off, shaped := lenAt(cmd)
			if !shaped {
				// Variable size but no known length position: guessing would
				// misalign the stream, so treat it like an unknown command.
				frames = append(frames, Frame{Cmd: cmd, Size: len(buf) - i, Data: buf[i:]})
				return frames, len(buf)
			}
			if i+off+2 > len(buf) {
				break // the length word has not arrived yet
			}
			total := int(binary.LittleEndian.Uint16(buf[i+off:]))
			// A length under the header size cannot be a packet, and one that
			// runs past the buffer means the packet is still arriving.
			if total < off+2 || i+total > len(buf) {
				break
			}
			frames = append(frames, Frame{Cmd: cmd, Size: total, Data: buf[i : i+total]})
			i += total
			continue
		}

		if i+size > len(buf) {
			break
		}
		frames = append(frames, Frame{Cmd: cmd, Size: size, Data: buf[i : i+size]})
		i += size
	}
	return frames, i
}
