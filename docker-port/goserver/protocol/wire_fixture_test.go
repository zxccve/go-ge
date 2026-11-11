package protocol

import (
	"bufio"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wireFixtures returns every captured socket stream in testdata.
func wireFixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.wire.txt"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Skip("no wire fixtures in testdata - run: python make_fixtures.py")
	}
	return paths
}

// loadWireFixture reads a .wire.txt file: one transport chunk per line,
// `cipherLen <TAB> hex of the whole chunk`.
func loadWireFixture(t *testing.T, path string) (stream []byte, cipherLens []int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		if len(p) != 2 {
			t.Fatalf("%s: line %q should be `cipherLen hex`", path, line)
		}
		want, err := strconv.Atoi(p[0])
		if err != nil {
			t.Fatalf("%s: bad cipher length %q", path, p[0])
		}
		raw, err := hex.DecodeString(p[1])
		if err != nil {
			t.Fatalf("%s: bad hex: %v", path, err)
		}
		if len(raw)-2 != want {
			t.Fatalf("%s: line says %d cipher bytes but carries %d",
				path, want, len(raw)-2)
		}
		if want%8 != 0 {
			t.Fatalf("%s: cipher length %d is not a whole number of 8-byte blocks",
				path, want)
		}
		stream = append(stream, raw...)
		cipherLens = append(cipherLens, want)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(stream) == 0 {
		t.Fatalf("%s: no chunks", path)
	}
	return stream, cipherLens
}

// TestWireFixturesSplit holds the transport splitter against bytes a real
// client sent. This is the first thing the Go server does on a connection, so
// it is the first thing that has to be exactly right.
func TestWireFixturesSplit(t *testing.T) {
	for _, path := range wireFixtures(t) {
		stream, cipherLens := loadWireFixture(t, path)
		t.Run(filepath.Base(path), func(t *testing.T) {
			chunks, used := SplitWire(stream)
			if used != len(stream) {
				t.Fatalf("consumed %d of %d bytes - the tail was left over",
					used, len(stream))
			}
			if len(chunks) != len(cipherLens) {
				t.Fatalf("split into %d chunks, fixture says %d",
					len(chunks), len(cipherLens))
			}
			for i, c := range chunks {
				if c.CipherLen() != cipherLens[i] {
					t.Fatalf("chunk %d cipher = %d bytes, want %d",
						i, c.CipherLen(), cipherLens[i])
				}
			}
		})
	}
}

// TestWireFixtureTailIsHeldBack feeds the same capture one byte short. The
// splitter must consume only the whole chunks and leave the partial one in
// the buffer - a live socket delivers a packet in pieces all the time.
func TestWireFixtureTailIsHeldBack(t *testing.T) {
	for _, path := range wireFixtures(t) {
		stream, cipherLens := loadWireFixture(t, path)
		t.Run(filepath.Base(path), func(t *testing.T) {
			short := stream[:len(stream)-1]
			chunks, used := SplitWire(short)
			if used > len(short) {
				t.Fatalf("consumed %d of only %d bytes", used, len(short))
			}
			if len(chunks) != len(cipherLens)-1 {
				t.Fatalf("got %d whole chunks, want %d - the last one is short by one byte and must be held back",
					len(chunks), len(cipherLens)-1)
			}
			for i, c := range chunks {
				if c.CipherLen() != cipherLens[i] {
					t.Fatalf("chunk %d cipher = %d bytes, want %d",
						i, c.CipherLen(), cipherLens[i])
				}
			}
		})
	}
}
