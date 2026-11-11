package protocol

import (
	"encoding/binary"
	"testing"
)

// chunk builds one transport frame: a length word, then that many bytes.
func chunk(payload ...byte) []byte {
	b := make([]byte, 2+len(payload))
	binary.LittleEndian.PutUint16(b, uint16(len(payload)))
	copy(b[2:], payload)
	return b
}

func TestSplitWireOneChunk(t *testing.T) {
	stream := chunk(1, 2, 3, 4, 5, 6, 7, 8)
	chunks, used := SplitWire(stream)
	if used != len(stream) || len(chunks) != 1 {
		t.Fatalf("chunks=%d used=%d, want 1 and %d", len(chunks), used, len(stream))
	}
	if got := chunks[0].CipherLen(); got != 8 {
		t.Fatalf("CipherLen = %d, want 8", got)
	}
}

func TestSplitWireSeveralChunks(t *testing.T) {
	stream := append(chunk(make([]byte, 16)...), chunk(make([]byte, 8)...)...)
	chunks, used := SplitWire(stream)
	if used != len(stream) {
		t.Fatalf("consumed %d of %d", used, len(stream))
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].CipherLen() != 16 || chunks[1].CipherLen() != 8 {
		t.Fatalf("cipher lengths = %d, %d, want 16, 8",
			chunks[0].CipherLen(), chunks[1].CipherLen())
	}
}

func TestSplitWireHoldsPartialChunk(t *testing.T) {
	stream := chunk(make([]byte, 16)...)
	chunks, used := SplitWire(stream[:len(stream)-1])
	if len(chunks) != 0 || used != 0 {
		t.Fatalf("chunks=%d used=%d, want nothing consumed from a partial chunk",
			len(chunks), used)
	}
}

func TestSplitWireEmpty(t *testing.T) {
	chunks, used := SplitWire(nil)
	if len(chunks) != 0 || used != 0 {
		t.Fatalf("chunks=%d used=%d, want nothing", len(chunks), used)
	}
}
