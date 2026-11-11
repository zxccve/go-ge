package zone

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

// TestParseSpawnAcceptsTheMapSpawn is the point the client needs: Reboldoeux's own
// spawn, the same one config\zone_spawn.txt carries for the Python server.
func TestParseSpawnAcceptsTheMapSpawn(t *testing.T) {
	got, err := ParseSpawn("5779 303 149")
	if err != nil {
		t.Fatalf("ParseSpawn: %v", err)
	}
	if got != [3]int32{5779, 303, 149} {
		t.Fatalf("spawn = %v, want [5779 303 149]", got)
	}
}

// TestParseSpawnRejectsAHeight is the regression test for the walking crash: the
// stored z of these characters is -1088, which as a cell is an out-of-range
// triangle index, and the client died in PathEngine with it.
func TestParseSpawnRejectsAHeight(t *testing.T) {
	for _, bad := range []string{"5779 303 -1088", "8718 10303 -1217"} {
		if _, err := ParseSpawn(bad); err == nil {
			t.Fatalf("ParseSpawn(%q) accepted a height as a cell", bad)
		}
	}
	if _, err := ParseSpawn("5779 303"); err == nil {
		t.Fatal("ParseSpawn accepted two fields")
	}
	if _, err := ParseSpawn("a b c"); err == nil {
		t.Fatal("ParseSpawn accepted non-numbers")
	}
}

// TestTeamSpawnsOnTheMapPoint proves the handler OVERRIDES the stored position:
// the database holds a height in that slot, so trusting it is what crashed.
func TestTeamSpawnsOnTheMapPoint(t *testing.T) {
	h := quietHandler()
	h.Spawn = [3]int32{DefaultSpawnX, DefaultSpawnY, DefaultSpawnCell}
	// TeamOf returns characters carrying a stored (height-like) third number.
	h.TeamOf = func(context.Context, uint32) ([]Character, error) {
		return []Character{
			{Handle: 34, Name: "Jin", Job: 3, Position: [3]int32{7492, 10720, -1088}},
			{Handle: 40, Name: "Fighter", Job: 1, Position: [3]int32{8718, 10303, -1217}},
		}, nil
	}

	out, err := h.Reply(context.Background(), &Session{},
		startGameFrame(4242))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// The FIRST packet is the entry packet; the rest is the entry burst, which
	// this test does not care about.
	if len(out) < 1 {
		t.Fatalf("sent %d packet(s), want at least the entry packet", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != CmdZCStartGameOK {
		t.Fatalf("first packet is cmd %d, want %d", cmd, CmdZCStartGameOK)
	}

	// Both records must carry the map spawn, not the stored height.
	for i := 0; i < 2; i++ {
		off := headerSize + i*startInfoSize + siInitPos
		cell := int32(binary.LittleEndian.Uint32(out[0][off+8:]))
		if cell != DefaultSpawnCell {
			t.Fatalf("character %d cell = %d, want the map spawn %d",
				i, cell, DefaultSpawnCell)
		}
		x := int32(binary.LittleEndian.Uint32(out[0][off:]))
		if x != DefaultSpawnX {
			t.Fatalf("character %d x = %d, want %d", i, x, DefaultSpawnX)
		}
	}
}

// map15Landing reads map 15's spawn row out of the REAL table, so a test about the
// formation stands the team on the offsets the server actually ships. A copy of them
// here would only prove the copy was edited when the table was corrected.
func map15Landing(t *testing.T) Landing {
	t.Helper()
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	landing, err := points.SpawnFor(15)
	if err != nil {
		t.Fatalf("SpawnFor(15): %v", err)
	}
	return landing
}

// TestTeamSpawnsInFormation is the ENTRY half of the stacking bug: the team enters on
// its own slots, not all on the leader's point, so the client never has two characters
// to draw on top of each other from the first frame.
func TestTeamSpawnsInFormation(t *testing.T) {
	landing := map15Landing(t)
	h := quietHandler()
	h.Spawn = landing.Pos
	h.SpawnFormation = landing.Formation
	h.TeamOf = func(context.Context, uint32) ([]Character, error) {
		return []Character{
			{Handle: 34, Name: "Jin", Job: 3, Position: [3]int32{7492, 10720, -1088}},
			{Handle: 40, Name: "Fighter", Job: 1, Position: [3]int32{8718, 10303, -1217}},
			{Handle: 41, Name: "Wizard", Job: 2, Position: [3]int32{1, 2, -3}},
		}, nil
	}

	out, err := h.Reply(context.Background(), &Session{}, startGameFrame(4242))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(landing.Formation) < 3 {
		t.Fatalf("map 15's spawn carries %d formation slot(s), want 3",
			len(landing.Formation))
	}
	// The expectation is the FILE's slots, read once here instead of through Slot, so a
	// bug in the slot rule cannot agree with itself.
	want := [][3]int32{landing.Pos}
	for i := 1; i < 3; i++ {
		slot := landing.Formation[i]
		if slot.Cell == nil {
			t.Fatalf("formation slot %d of map 15's spawn has a null cell", i)
		}
		want = append(want, [3]int32{slot.X, slot.Y, *slot.Cell})
	}
	assertFormationShape(t, landing.Pos, want)
	for i := range want {
		off := headerSize + i*startInfoSize + siInitPos
		got := [3]int32{
			int32(binary.LittleEndian.Uint32(out[0][off:])),
			int32(binary.LittleEndian.Uint32(out[0][off+4:])),
			int32(binary.LittleEndian.Uint32(out[0][off+8:])),
		}
		if got != want[i] {
			t.Fatalf("character %d enters at %v, want slot %d %v", i, got, i, want[i])
		}
	}
}

// startGameFrame builds the client's CZ_START_GAME plain packet.
func startGameFrame(accountID uint32) protocol.Frame {
	data := make([]byte, 40)
	binary.LittleEndian.PutUint16(data, CmdCZStartGame)
	binary.LittleEndian.PutUint32(data[10:], 0xABCD)
	binary.LittleEndian.PutUint32(data[14:], accountID)
	return protocol.Frame{Cmd: CmdCZStartGame, Size: len(data), Data: data}
}
