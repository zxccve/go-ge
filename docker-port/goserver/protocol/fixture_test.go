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

// cmdTablePath is the client's own packet table, shared with the Python probes.
const cmdTablePath = "../../data/cmd_table.txt"

// TestLoadRealCmdTable checks the loader against the real table. A size of 0
// means variable size, which is what the big zone packets are (3591 and 3747
// carry their own length - their 1229 and 2019 bytes in zone_golden.txt are
// the length word, NOT a table size). Fixed-size commands must come back with
// their size.
func TestLoadRealCmdTable(t *testing.T) {
	tab, err := LoadTable(cmdTablePath)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	if len(tab) < 1000 {
		t.Fatalf("loaded only %d commands, the table should hold thousands", len(tab))
	}
	for cmd, want := range map[uint16]int{3591: 0, 3747: 0, 256: 11, 259: 24} {
		got, ok := tab.Size(cmd)
		if !ok {
			t.Fatalf("command %d missing from the table", cmd)
		}
		if got != want {
			t.Fatalf("command %d size = %d, want %d", cmd, got, want)
		}
	}
}

// TestGoldenZoneSizes relates the fixture format to the golden zone capture:
// every command in data\zone_golden.txt must be known to the table, because
// that file was produced by splitting a real stream with the same table.
func TestGoldenZoneSizes(t *testing.T) {
	tab, err := LoadTable(cmdTablePath)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	f, err := os.Open("../../data/zone_golden.txt")
	if err != nil {
		t.Fatalf("open zone_golden.txt: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 2 {
			continue
		}
		cmd, err := strconv.ParseUint(p[0], 10, 16)
		if err != nil {
			continue
		}
		if _, ok := tab.Size(uint16(cmd)); !ok {
			t.Fatalf("line %q: command %s is not in the cmd table", sc.Text(), p[0])
		}
		lines++
	}
	if lines == 0 {
		t.Fatal("zone_golden.txt held no commands - the parser and the file disagree")
	}
}

// Fixture is one captured stream: the bytes, and the frames they must split
// into. Written by src\make_fixtures.py from a real capture.
type Fixture struct {
	Name   string
	Stream []byte
	Cmds   []uint16
	Sizes  []int
}

// loadFixture reads a .stream.txt file. One packet per line:
//
//	cmd <TAB> size <TAB> hex of the whole packet
func loadFixture(t *testing.T, path string) Fixture {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	fx := Fixture{Name: filepath.Base(path)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		if len(p) != 3 {
			t.Fatalf("%s: line %q should be `cmd size hex`", path, line)
		}
		cmd, err := strconv.ParseUint(p[0], 10, 16)
		if err != nil {
			t.Fatalf("%s: bad command %q", path, p[0])
		}
		size, err := strconv.Atoi(p[1])
		if err != nil {
			t.Fatalf("%s: bad size %q", path, p[1])
		}
		raw, err := hex.DecodeString(p[2])
		if err != nil {
			t.Fatalf("%s: bad hex for command %d: %v", path, cmd, err)
		}
		if len(raw) != size {
			t.Fatalf("%s: command %d says %d bytes but carries %d",
				path, cmd, size, len(raw))
		}
		fx.Stream = append(fx.Stream, raw...)
		fx.Cmds = append(fx.Cmds, uint16(cmd))
		fx.Sizes = append(fx.Sizes, size)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(fx.Stream) == 0 {
		t.Fatalf("%s: no packets", path)
	}
	return fx
}

func fixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.stream.txt"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Skip("no fixtures in testdata yet - run: python make_fixtures.py")
	}
	return paths
}

// TestFixturesSplit is the point of the whole exercise: a stream a real client
// sent must split into exactly the packets the probe said it would.
func TestFixturesSplit(t *testing.T) {
	tab, err := LoadTable(cmdTablePath)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	for _, path := range fixtures(t) {
		fx := loadFixture(t, path)
		t.Run(fx.Name, func(t *testing.T) {
			frames, used := tab.Split(fx.Stream)
			if used != len(fx.Stream) {
				t.Fatalf("consumed %d of %d bytes - the tail was left over",
					used, len(fx.Stream))
			}
			if len(frames) != len(fx.Cmds) {
				t.Fatalf("split into %d frames, fixture says %d",
					len(frames), len(fx.Cmds))
			}
			for i, fr := range frames {
				if fr.Cmd != fx.Cmds[i] || fr.Size != fx.Sizes[i] {
					t.Fatalf("frame %d = cmd %d size %d, want cmd %d size %d",
						i, fr.Cmd, fr.Size, fx.Cmds[i], fx.Sizes[i])
				}
			}
		})
	}
}

// TestFixtureTailIsHeldBack feeds the same stream one byte short and requires
// the splitter to hold the last packet back instead of inventing a frame from
// a half-arrived packet. This is what a live socket does every time.
func TestFixtureTailIsHeldBack(t *testing.T) {
	tab, err := LoadTable(cmdTablePath)
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	for _, path := range fixtures(t) {
		fx := loadFixture(t, path)
		if len(fx.Stream) < 2 {
			continue
		}
		t.Run(fx.Name, func(t *testing.T) {
			short := fx.Stream[:len(fx.Stream)-1]
			frames, used := tab.Split(short)
			if used > len(short) {
				t.Fatalf("consumed %d of only %d bytes", used, len(short))
			}
			if used != len(short) {
				t.Fatalf("consumed %d of %d: the last whole packet should still split",
					used, len(short))
			}
			for _, fr := range frames {
				if fr.Data[len(fr.Data)-1] == 0 && fr.Size == 1 {
					t.Fatalf("invented a frame from a partial packet: %+v", fr)
				}
			}
		})
	}
}
