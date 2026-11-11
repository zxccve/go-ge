package zone

import (
	"bytes"
	"context"
	"encoding/hex"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
)

// CZ_SAVE_RESTART_WARPID (3468) and its answer ZC_UPDATE_RESTART_WARP (3467), tested
// with NO CLIENT and NO DATABASE.
//
// The request's four tail bytes are the REAL ONES from the capture in
// logs\zone15_live.log:14879 - stale stack, not a field. A handler that read them
// would be writing a random warp id here, so the test keeps them.

// saveRestartWarpRequest is the 3468 the shipped client actually sent.
func saveRestartWarpRequest() []byte {
	return []byte{
		0x8c, 0x0d, //              cmd 3468
		0x17, 0x00, 0x00, 0x00, //  seq 23
		0xd1, 0x01, 0x00, 0x00, //  checksum 465
		0xe4, 0x7d, 0x3c, 0xf6, //  stale stack: NOT a warp id, a map id or an index
	}
}

// saveRestartWarpFrame wraps that request as a frame, so the writer tests ask the
// handler the same question the client asks it.
func saveRestartWarpFrame() protocol.Frame {
	data := saveRestartWarpRequest()
	return protocol.Frame{Cmd: CmdCZSaveRestartWarpID, Size: len(data), Data: data}
}

// TestBuildUpdateRestartWarpLayout pins EVERY byte of 3467: the command word, then the
// warp id as a DWORD at +2, and NO length word. The client's handler reads [EAX+2]
// (ge.exe 0x4cbc80), so a length word added here would be read as part of the id.
func TestBuildUpdateRestartWarpLayout(t *testing.T) {
	for _, warp := range []int{203, 213, 15001} {
		out := BuildUpdateRestartWarp(warp)
		if len(out) != 6 {
			t.Fatalf("packet is %d bytes, want 6 (the client's own table size)", len(out))
		}
		want := "8b0d" + hex.EncodeToString([]byte{
			byte(warp), byte(warp >> 8), byte(warp >> 16), byte(warp >> 24)})
		if got := hex.EncodeToString(out); got != want {
			t.Fatalf("ZC_UPDATE_RESTART_WARP(%d):\n got %s\nwant %s", warp, got, want)
		}
		if back, ok := ParseUpdateRestartWarp(out); !ok || back != warp {
			t.Fatalf("round trip = %d (ok %v), want %d", back, ok, warp)
		}
	}
	// Coimbra's own row, byte for byte: warp 213 = 0xd5.
	if got := hex.EncodeToString(BuildUpdateRestartWarp(213)); got != "8b0dd5000000" {
		t.Fatalf("warp 213 = %s, want 8b0dd5000000", got)
	}
	// A short packet never parses: the server does not receive this id, but a test
	// that reads one must not read a byte that is not there.
	if _, ok := ParseUpdateRestartWarp(make([]byte, 5)); ok {
		t.Fatalf("5 bytes parsed as a warp id; the DWORD needs 6")
	}
}

// TestSaveRestartWarpRefusesAndWritesNothing pins every refusal. In each one NOTHING
// is written and NOTHING is sent: a 3467 for a save that did not happen would make the
// window claim a start point the barrack does not have.
func TestSaveRestartWarpRefusesAndWritesNothing(t *testing.T) {
	cases := []struct {
		name  string
		mapID int
		store bool
		data  []byte
		want  string
	}{
		{
			name:  "the map is not a town row at all",
			mapID: 100, // no row in data\zone_points.json's waypoints list
			store: true,
			data:  saveRestartWarpRequest(),
			want:  "is not a row in the waypoint grid",
		},
		{
			name:  "the row is the FIELD grid and carries no restart warp",
			mapID: 207, // fld_alq: Tab 1, restart warp 0
			store: true,
			data:  saveRestartWarpRequest(),
			want:  "carries no restart warp",
		},
		{
			name:  "no store is wired in",
			mapID: 15,
			store: false,
			data:  saveRestartWarpRequest(),
			want:  "no start-point store is wired in",
		},
		{
			name:  "the packet is not the size the client registers",
			mapID: 15,
			store: true,
			data:  saveRestartWarpRequest()[:13],
			want:  "want the client's registered 14",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, logs := startPointHandler(t)
			h.MapID = c.mapID
			f := &startPointFake{}
			if c.store {
				h.UseStartPoint(f.loadOf, f.saveOf)
			}
			sess := &Session{AccountID: 36, CommanderID: 25}
			out, err := h.Reply(context.Background(), sess, protocol.Frame{
				Cmd: CmdCZSaveRestartWarpID, Size: len(c.data), Data: c.data})
			if err != nil {
				t.Fatalf("Reply: %v", err)
			}
			if len(out) != 0 {
				t.Fatalf("%d packet(s) sent on a refusal, want none: %x", len(out), out)
			}
			if len(f.warps) != 0 {
				t.Fatalf("a refused save wrote warp(s) %v, want nothing", f.warps)
			}
			if !strings.Contains(logs.String(), c.want) {
				t.Fatalf("the log does not say why; want %q in:\n%s", c.want, logs)
			}
		})
	}
}

// TestSaveRestartWarpRefusesWhenTheWarpRowDoesNotAnswer is the "the table and the town
// row disagree" case: the save must NOT fall back to the spawn point. Saving a point
// the map's own restart row cannot answer would move the entry to an unrelated spot on
// the next login, with nothing to show why.
func TestSaveRestartWarpRefusesWhenTheWarpRowDoesNotAnswer(t *testing.T) {
	cell := int32(146)
	points := &ZonePoints{
		Version: ZonePointsVersion,
		Maps: map[string]MapEntry{
			"10": {
				Name:    "reb_basic",
				Restart: &RestartPoint{WarpID: 999, X: 1, Y: 2, Cell: &cell},
			},
		},
		Waypoints: []WaypointTown{{ID: 1, Location: "reb_basic", MapID: 10,
			RestartWarpID: 203, Tab: 0}},
	}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), MapID: 10, MapName: "reb_basic",
		ZonePoints: points}
	f := &startPointFake{}
	h.UseStartPoint(f.loadOf, f.saveOf)

	sess := &Session{AccountID: 36, CommanderID: 25}
	out, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZSaveRestartWarpID, Size: 14, Data: saveRestartWarpRequest()})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 || len(f.warps) != 0 {
		t.Fatalf("out = %d packet(s), saves = %v - a warp row that does not answer "+
			"must refuse, not fall back", len(out), f.warps)
	}
	if !strings.Contains(logs.String(), "restart row cannot place a team") {
		t.Fatalf("the log does not say why; it says:\n%s", logs)
	}
}

// TestSaveRestartWarpRefusesWithoutACommander: a session whose commander was never
// resolved has no row to write, and the player must be told by the log rather than
// answered as if it worked.
func TestSaveRestartWarpRefusesWithoutACommander(t *testing.T) {
	h, logs := startPointHandler(t)
	f := &startPointFake{}
	h.UseStartPoint(f.loadOf, f.saveOf)

	out, err := h.Reply(context.Background(), &Session{AccountID: 36},
		protocol.Frame{Cmd: CmdCZSaveRestartWarpID, Size: 14,
			Data: saveRestartWarpRequest()})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 || len(f.warps) != 0 {
		t.Fatalf("a session with no commander wrote %v and sent %d packet(s)",
			f.warps, len(out))
	}
	if !strings.Contains(logs.String(), "commander was never resolved") {
		t.Fatalf("the log does not say why; it says:\n%s", logs)
	}
}
