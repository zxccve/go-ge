package protocol

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestKeyScheduleMatchesPython is the check that makes the rest trustworthy:
// the Go key schedule must expand the network key into exactly the P array and
// S boxes that src\ge_crypt.py builds. If this passes, the round function and
// the schedule agree with the implementation the probes already trust.
func TestKeyScheduleMatchesPython(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "bf_keyschedule.txt"))
	if err != nil {
		t.Skipf("no key schedule fixture: run python emit_go_bf_tables.py (%v)", err)
	}
	defer f.Close()

	wantP := make(map[int]uint32)
	wantS := [4]map[int]uint32{}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		if len(p) != 3 {
			t.Fatalf("bad line %q, want `name index hex`", line)
		}
		idx, err := strconv.Atoi(p[1])
		if err != nil {
			t.Fatalf("bad index in %q", line)
		}
		v, err := strconv.ParseUint(p[2], 16, 32)
		if err != nil {
			t.Fatalf("bad word in %q", line)
		}
		if p[0] == "P" {
			wantP[idx] = uint32(v)
			continue
		}
		if len(p[0]) != 2 || p[0][0] != 'S' {
			t.Fatalf("bad table name %q", p[0])
		}
		box := int(p[0][1] - '0')
		if box < 0 || box > 3 {
			t.Fatalf("bad S box %q", p[0])
		}
		if wantS[box] == nil {
			wantS[box] = make(map[int]uint32)
		}
		wantS[box][idx] = uint32(v)
	}
	if len(wantP) != 18 {
		t.Fatalf("fixture has %d P words, want 18", len(wantP))
	}

	got := newBlowfish(geKey)
	for i := 0; i < 18; i++ {
		if got.p[i] != wantP[i] {
			t.Fatalf("P[%d] = %08x, python says %08x", i, got.p[i], wantP[i])
		}
	}
	for box := 0; box < 4; box++ {
		if len(wantS[box]) != 256 {
			t.Fatalf("fixture S%d has %d words, want 256", box, len(wantS[box]))
		}
		for i := 0; i < 256; i++ {
			if got.s[box][i] != wantS[box][i] {
				t.Fatalf("S%d[%d] = %08x, python says %08x",
					box, i, got.s[box][i], wantS[box][i])
			}
		}
	}
}

// plainFixtures maps a fixture name to the plaintext lines beside it.
func plainFixture(t *testing.T, wirePath string) []string {
	t.Helper()
	path := strings.TrimSuffix(wirePath, ".wire.txt") + ".plain.txt"
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return out
}

// TestDecryptMatchesPython holds the Go cipher against a real capture: each
// chunk of the wire fixture must decrypt to the exact bytes Python produced.
// This is the whole point - if it passes, the Go side reads the client exactly
// as the probes do.
func TestDecryptMatchesPython(t *testing.T) {
	c := DefaultCipher
	for _, path := range wireFixtures(t) {
		stream, cipherLens := loadWireFixture(t, path)
		want := plainFixture(t, path)
		if len(want) != len(cipherLens) {
			t.Fatalf("%s: %d wire chunks but %d plaintext lines",
				path, len(cipherLens), len(want))
		}

		chunks, used := SplitWire(stream)
		if used != len(stream) {
			t.Fatalf("%s: consumed %d of %d", path, used, len(stream))
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			for i, ch := range chunks {
				got, err := c.DecryptChunk(ch.Data)
				if err != nil {
					t.Fatalf("chunk %d: %v", i, err)
				}
				wantBytes, err := hex.DecodeString(want[i])
				if err != nil {
					t.Fatalf("chunk %d: fixture hex: %v", i, err)
				}
				if !bytes.Equal(got, wantBytes) {
					n := 0
					for n < len(got) && n < len(wantBytes) && got[n] == wantBytes[n] {
						n++
					}
					t.Fatalf("chunk %d: plaintext differs at byte %d of %d\n got %x\nwant %x",
						i, n, len(wantBytes), got[:min(24, len(got))], wantBytes[:min(24, len(wantBytes))])
				}
			}
		})
	}
}

// TestEncryptRoundTrip proves the two directions are inverses, so a reply the
// Go server builds will decrypt on the far side the way the client expects.
func TestEncryptRoundTrip(t *testing.T) {
	c := DefaultCipher
	for _, path := range wireFixtures(t) {
		want := plainFixture(t, path)
		t.Run(filepath.Base(path), func(t *testing.T) {
			for i, line := range want {
				plain, err := hex.DecodeString(line)
				if err != nil {
					t.Fatalf("line %d: %v", i, err)
				}
				chunk := c.EncryptChunk(plain)
				back, err := c.DecryptChunk(chunk)
				if err != nil {
					t.Fatalf("line %d: %v", i, err)
				}
				if !bytes.Equal(back[:len(plain)], plain) {
					t.Fatalf("line %d: round trip changed the plaintext", i)
				}
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
