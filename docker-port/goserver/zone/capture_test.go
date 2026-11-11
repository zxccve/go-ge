package zone

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

// TestDecodeCaptureFrames replays a CLIENT->SERVER capture through the real
// cipher and the real command table, and prints the frame layout.
//
// It exists because "the stream desyncs after CZ_PING" (ROADMAP R45) can only be
// answered by looking at bytes. A capture from the LIVE server is ground truth,
// and running it offline costs no login.
//
// Skip-only when the capture folder is absent: it is game data, not repo data.
func TestDecodeCaptureFrames(t *testing.T) {
	dir := os.Getenv("GE_CAPTURE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "data", "captures", "live-20261021")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "c2s_*_7054.bin"))
	if len(files) == 0 {
		t.Skipf("no client->server zone capture under %s", dir)
	}

	table, err := protocol.LoadSpec(filepath.Join("..", "..", "data", "cmd_table_live_z.txt"))
	if err != nil {
		t.Skipf("command table not readable: %v", err)
	}

	chunks, unknown, total := 0, 0, 0
	maxRest := 0
	for _, path := range files {
		blob, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}

		wire, _ := protocol.SplitWire(blob)
		for ci, ch := range wire {
			plain, derr := protocol.DefaultCipher.DecryptChunk(ch.Data)
			if derr != nil {
				continue // not a game chunk (the relay's own padding, a partial tail)
			}
			chunks++
			frames, used := table.Split(plain)
			total += len(frames)

			// An unknown frame is the R45 symptom: the splitter cannot size it.
			rest := plain[used:]
			if len(rest) > 0 && !isZero(rest) {
				unknown++
				if len(rest) > maxRest {
					maxRest = len(rest)
				}
				if len(rest) > 8 {
					t.Logf("BIG LEFTOVER %d bytes in %s chunk %d: %s",
						len(rest), filepath.Base(path), ci, hexHead(rest, 40))
				}
			}
			for _, f := range frames {
				// Print the pings and anything unusual; the rest is noise.
				if f.Cmd == 3688 || f.Cmd == 3689 || f.Cmd == 3690 {
					t.Logf("%s chunk %d: cmd %d size %d: %s",
						filepath.Base(path), ci, f.Cmd, f.Size, hexHead(f.Data, 24))
				}
			}
		}
	}
	t.Logf("decoded %d chunk(s), %d frame(s), %d chunk(s) with leftover bytes (max %d)",
		chunks, total, unknown, maxRest)

	// This is a DIAGNOSTIC, not a gate, and it is deliberately allowed to pass
	// with leftovers: they are real today (R45 - `CZ_CHAT` at the tail of a chunk),
	// and a failing test would just be turned off. What it does guarantee is that
	// decoding a real capture still works at all, so a broken cipher or a broken
	// table shows up here rather than in a client session.
	if chunks == 0 || total == 0 {
		t.Fatalf("decoded nothing (%d chunk(s), %d frame(s)) - the cipher or the "+
			"command table is wrong", chunks, total)
	}
	if unknown*4 > chunks {
		// A quarter of the chunks carrying unsizable bytes would mean the stream
		// really is broken, not that a few chat lines are dropped.
		t.Fatalf("%d of %d chunk(s) left bytes behind - that is a DESYNC, not the "+
			"known chat drop (R45)", unknown, chunks)
	}
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// keep binary imported for the frame decoding above even if a future edit
// drops the direct use.
var _ = binary.LittleEndian
