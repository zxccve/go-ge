package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// capturedLogin is the plaintext of a real CB_LOGIN (the October capture).
func capturedLogin(t *testing.T) []byte {
	t.Helper()
	lines := plainFixture(t, "testdata/barrack_stub.wire.txt")
	if len(lines) != 1 {
		t.Fatalf("expected 1 captured chunk, got %d", len(lines))
	}
	plain, err := hex.DecodeString(lines[0])
	if err != nil {
		t.Fatalf("fixture hex: %v", err)
	}
	return plain
}

// TestParseLoginFromCapture holds the parser against real client bytes: the
// fields it exposes must be the ones that are actually stable.
func TestParseLoginFromCapture(t *testing.T) {
	l, err := ParseLogin(capturedLogin(t))
	if err != nil {
		t.Fatalf("ParseLogin: %v", err)
	}
	if l.Cmd != 4 {
		t.Fatalf("command = %d, want 4", l.Cmd)
	}
	if l.Len != 812 {
		t.Fatalf("length word = %d, want 812 (the cipher pads to 816)", l.Len)
	}
	if l.AccountID != "Arcanine" {
		t.Fatalf("account = %q, want %q", l.AccountID, "Arcanine")
	}
	if l.Nation != "DEU" {
		t.Fatalf("nation = %q, want %q", l.Nation, "DEU")
	}
	if l.Version == 0 {
		t.Fatal("version is 0 - the offset is wrong")
	}
	if len(l.Digest) != 16 {
		t.Fatalf("digest = %d bytes, want 16", len(l.Digest))
	}
	// The whole point: the digest is NOT verified, and the parser says so
	// rather than letting a caller assume a check happened.
	if l.DigestVerified {
		t.Fatal("DigestVerified must be false: the digest cannot be checked")
	}
}

// TestParseLoginIgnoresPadding proves the parser trusts the packet's own length
// word, so the cipher's trailing zeros never reach a field.
func TestParseLoginIgnoresPadding(t *testing.T) {
	plain := capturedLogin(t)
	withJunk := append(append([]byte{}, plain...), 0xFF, 0xFF, 0xFF, 0xFF)
	l, err := ParseLogin(withJunk)
	if err != nil {
		t.Fatalf("ParseLogin: %v", err)
	}
	if l.AccountID != "Arcanine" || l.Nation != "DEU" {
		t.Fatalf("padding changed the parsed fields: %+v", l)
	}
}

func TestParseLoginRejectsNothing(t *testing.T) {
	if _, err := ParseLogin(nil); err == nil {
		t.Fatal("a nil buffer must be refused")
	}
	short := make([]byte, 8)
	if _, err := ParseLogin(short); err == nil {
		t.Fatal("a short buffer must be refused")
	}
	// Right size, wrong command.
	wrong := capturedLogin(t)
	wrong[0] = 9
	if _, err := ParseLogin(wrong); err == nil {
		t.Fatal("a packet that is not CB_LOGIN must be refused")
	}
}

// TestBuildLoginOK checks the reply against the client's own table: command 8,
// exactly 57 bytes, account and session echoed, name padded with zeros.
func TestBuildLoginOK(t *testing.T) {
	b := BuildLoginOK(35, 0x3333, "Arcanine")
	if len(b) != 57 {
		t.Fatalf("login reply = %d bytes, want 57 (the client's table size)", len(b))
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 8 {
		t.Fatalf("command = %d, want 8 (BC_LOGINOK)", cmd)
	}
	if acc := binary.LittleEndian.Uint32(b[2:]); acc != 35 {
		t.Fatalf("account = %d, want 35", acc)
	}
	if name := cString(b[10:]); name != "Arcanine" {
		t.Fatalf("name = %q, want Arcanine", name)
	}
	for i, c := range b[10+len("Arcanine"):] {
		if c != 0 {
			t.Fatalf("padding byte %d = %02x, want 0", i, c)
		}
	}
}

// TestBuildLoginOKTruncates proves a very long name cannot overflow the packet:
// the size is what keeps the client's stream in sync.
func TestBuildLoginOKTruncates(t *testing.T) {
	long := strings.Repeat("x", 200)
	b := BuildLoginOK(1, 2, long)
	if len(b) != 57 {
		t.Fatalf("long name produced %d bytes, want 57", len(b))
	}
}

// TestLoginReplySurvivesTheCipher proves the reply encrypts and decrypts back
// to the same 57 bytes, which is what the client will read.
func TestLoginReplySurvivesTheCipher(t *testing.T) {
	plain := BuildLoginOK(35, 0x3333, "Arcanine")
	chunk := DefaultCipher.EncryptChunk(plain)
	if got := len(chunk) - 2; got != 64 {
		t.Fatalf("ciphertext = %d bytes, want 64 (57 padded to a block)", got)
	}
	back, err := DefaultCipher.DecryptChunk(chunk)
	if err != nil {
		t.Fatalf("DecryptChunk: %v", err)
	}
	if !bytes.Equal(back[:57], plain) {
		t.Fatal("the reply did not survive the round trip")
	}
}
