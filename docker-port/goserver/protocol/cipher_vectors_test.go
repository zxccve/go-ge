package protocol

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCipherVectorsMatchPython is the interop test, and it is the one that
// matters for the OUTBOUND direction.
//
// A round trip through our own cipher proves only that Encrypt and Decrypt are
// inverses of each other; it passes even when the client cannot read a byte we
// send. This test compares against vectors produced by src\emit_cipher_vectors.py
// using ge_crypt, the implementation the client demonstrably accepts.
//
// Blowfish-ECB is deterministic, so the bytes must be identical.
func TestCipherVectorsMatchPython(t *testing.T) {
	path := filepath.Join("testdata", "cipher_vectors.txt")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no cipher vectors: run python emit_cipher_vectors.py (%v)", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)

	cases := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) < 2 {
			t.Fatalf("bad vector line: %q", line)
		}
		plain, err := hex.DecodeString(p[0])
		if err != nil {
			t.Fatalf("plain hex: %v", err)
		}
		want, err := hex.DecodeString(p[1])
		if err != nil {
			t.Fatalf("wire hex: %v", err)
		}

		what := "vector"
		if len(p) >= 3 {
			what = p[2]
		}
		got := DefaultCipher.EncryptChunk(plain)

		if !bytes.Equal(got, want) {
			n := 0
			for n < len(got) && n < len(want) && got[n] == want[n] {
				n++
			}
			t.Fatalf("%s: encrypted output differs from Python at byte %d of %d\n got %x\nwant %x",
				what, n, len(want), got[:min(24, len(got))], want[:min(24, len(want))])
		}
		cases++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	if cases == 0 {
		t.Fatal("no vectors were read")
	}
	t.Logf("%d cipher vectors matched Python exactly", cases)
}
