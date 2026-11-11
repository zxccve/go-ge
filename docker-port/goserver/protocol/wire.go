package protocol

import "encoding/binary"

// The game has TWO layers of framing, and mixing them up is how a stream
// misaligns:
//
//	layer 1 (transport): [u16 length][length bytes of ciphertext]
//	layer 2 (packet):    [u16 command][...] inside the decrypted bytes
//
// Layer 1 is what a socket reader sees and it needs no game knowledge, so it
// is the first thing the Go server does. Layer 2 is Table.Split, and it needs
// the client's command table.
//
// Layer 1 is proven by the working Python server: src\ge_barrack_server.py
// (recv_packets) cuts the wire exactly this way and each piece decrypts.

// Chunk is one transport frame: the length word plus its ciphertext.
type Chunk struct {
	Data []byte // the whole chunk, length word included
}

// CipherLen is how many encrypted bytes the chunk carries. It is always a
// multiple of 8, because the cipher works in 8-byte blocks - a chunk whose
// ciphertext is not a multiple of 8 was cut in the wrong place.
func (c Chunk) CipherLen() int { return len(c.Data) - 2 }

// SplitWire cuts whole transport chunks off the front of buf and reports how
// many bytes it used. Like Table.Split it never fails on a partial chunk: the
// tail simply stays in the buffer until more bytes arrive.
func SplitWire(buf []byte) (chunks []Chunk, consumed int) {
	i := 0
	for i+2 <= len(buf) {
		n := int(binary.LittleEndian.Uint16(buf[i:]))
		if i+2+n > len(buf) {
			break // the ciphertext has not fully arrived
		}
		chunks = append(chunks, Chunk{Data: buf[i : i+2+n]})
		i += 2 + n
	}
	return chunks, i
}
