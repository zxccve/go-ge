package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
	"geserver/world"
)

// R73, BUG 1: A NEW GAME ENTERS AT THE SAVE POINT'S MAP, AND A CONTINUE AT THE LAST
// POINT'S MAP.
//
// The two are different facts in different columns, and the barrack is the only place
// that decides which one the client is sent to:
//
//	CB_STARTGAME     22  -> commander.point   (a restart WARP id) -> that map's zone
//	CB_CONTINUEGAME  23  -> commander.zone_id (the map they logged out on)
//
// THE BUG THIS FILE PINS (owner, 2026-10-29 22:31: "my save point is in Coimbra but it
// send me to Rebo"): cmd 22 was routed by `commander.zone_id`, which the zone REWRITES on
// every log-out. A Coimbra save (commander.point = 213, a restart warp on map 15) was
// therefore sent to the map-10 zone, which cannot honour a map-15 point and dropped the
// team on Reboldoeux's own spawn.
//
// No client and no database here: the store is a fake and the registry is a directory of
// presence files, exactly as src\world.py writes them.

// savePointBarrackHandler is a barrack whose log is kept, so a test can read back the
// routing line and the refusal line.
func savePointBarrackHandler(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	h := quietHandler(t, fakeStore{ok: true})
	logs := &bytes.Buffer{}
	h.Log = log.New(logs, "", 0)
	// warp 213 is Coimbra's restart warp and owns map 15; warp 203 is Reboldoeux's and
	// owns map 10 (zone\ZonePoints.RestartWarpMap, data\zone_points.json).
	h.WarpMaps = func(warpID uint16) (uint16, bool) {
		switch warpID {
		case 213:
			return 15, true
		case 203:
			return 10, true
		}
		return 0, false
	}
	// Map names without a live zone, so the address and the name can be judged apart.
	h.MapNames = func(mapID uint16) (string, bool) {
		switch mapID {
		case 10:
			return "reb_basic", true
		case 15:
			return "coim_basic", true
		}
		return "", false
	}
	return h, logs
}

// TestNewGameRoutesToTheSavePointsMap is the fix itself: with a Coimbra save point and a
// Reboldoeux last point, cmd 22 must reach the zone that serves map 15.
//
// THE MISTAKE IT CATCHES: routing by `commander.zone_id`, which is the last point's map.
func TestNewGameRoutesToTheSavePointsMap(t *testing.T) {
	dir := t.TempDir()
	announceZone(t, dir, 10, 7002, "127.0.0.1", "reb_basic") // the last point's map, live
	announceZone(t, dir, 15, 7005, "127.0.0.1", "coim_basic") // the save point's map, live

	h, logs := savePointBarrackHandler(t)
	h.Zones = &world.Registry{Dir: dir}
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{
		ID: 24, Name: "Arcis", ZoneID: 10, SaveWarpID: 213, ChannelID: 0}}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1; the log says:\n%s", len(out), logs)
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 25 {
		t.Fatalf("answer is cmd %d, want 25 (BC_STARTGAMEOK)", cmd)
	}
	if got := binary.LittleEndian.Uint32(out[0][22:]); got != 7005 {
		t.Fatalf("zonePort = %d, want 7005 - the zone that serves the SAVE POINT's map 15",
			got)
	}
	if got := trimNul(string(out[0][26:67])); got != "coim_basic" {
		t.Fatalf("map = %q, want coim_basic (the save point's world), not reb_basic", got)
	}
	// The definition-of-done line: it names the map AND where the choice came from.
	if !strings.Contains(logs.String(),
		"routed by the SAVED START POINT (warp 213, map 15)") {
		t.Fatalf("the routing line does not name the save point's map; it says:\n%s", logs)
	}
	if !strings.Contains(logs.String(), `zone 15 map "coim_basic"`) {
		t.Fatalf("the routing line does not name zone 15; it says:\n%s", logs)
	}
	t.Logf("barrack log:\n%s", logs)
}

// TestNewGameRefusesWhenNoZoneServesTheSavePoint: a save point whose map has no live zone
// must NOT be quietly turned into the last point's map - that is the bug, and a wrong map
// is worse than a refusal, because the zone then drops the team on its own spawn.
//
// THE MISTAKE IT CATCHES: the old "fall back to commander.zone_id" path, which is what
// sent a Coimbra save to Reboldoeux.
func TestNewGameRefusesWhenNoZoneServesTheSavePoint(t *testing.T) {
	dir := t.TempDir()
	announceZone(t, dir, 10, 7002, "127.0.0.1", "reb_basic") // only the LAST point's map

	h, logs := savePointBarrackHandler(t)
	h.Zones = &world.Registry{Dir: dir}
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{
		ID: 24, Name: "Arcis", ZoneID: 10, SaveWarpID: 213, ChannelID: 0}}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("sent %d packet(s) - the client was moved to another map instead of "+
			"being refused: %x; the log says:\n%s", len(out), out, logs)
	}
	if !strings.Contains(logs.String(), "REFUSED") {
		t.Fatalf("no refusal in the log; it says:\n%s", logs)
	}
	for _, want := range []string{"SAVED START POINT is map 15", "warp 213",
		"NO LIVE ZONE serves that map", "NOT sent to map 10"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refusal does not say %q; it says:\n%s", want, logs)
		}
	}
	t.Logf("barrack log:\n%s", logs)
}

// TestContinueGameStillRoutesByTheLastPointsMap: the other button is untouched. cmd 23
// asks for where the player logged out, which is `commander.zone_id` - the SAME column
// the bug used for both.
//
// THE MISTAKE IT CATCHES: "fixing" cmd 22 by changing the column for both buttons, which
// would send a Continue to the saved town instead of the last point.
func TestContinueGameStillRoutesByTheLastPointsMap(t *testing.T) {
	dir := t.TempDir()
	announceZone(t, dir, 10, 7002, "127.0.0.1", "reb_basic")
	announceZone(t, dir, 15, 7005, "127.0.0.1", "coim_basic")

	h, logs := savePointBarrackHandler(t)
	h.Zones = &world.Registry{Dir: dir}
	// The save point is Coimbra, the last point is Reboldoeux: a Continue must follow
	// the last point.
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{
		ID: 24, Name: "Arcis", ZoneID: 10, SaveWarpID: 213, ChannelID: 0}}

	frame := startGameFrame(23, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 23, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1; the log says:\n%s", len(out), logs)
	}
	if got := binary.LittleEndian.Uint32(out[0][22:]); got != 7002 {
		t.Fatalf("zonePort = %d, want 7002 - the LAST POINT's map 10", got)
	}
	if got := trimNul(string(out[0][26:67])); got != "reb_basic" {
		t.Fatalf("map = %q, want reb_basic (the last point's world)", got)
	}
	if !strings.Contains(logs.String(), "routed by the LAST POINT's map") {
		t.Fatalf("the routing line does not say it followed the last point; it says:\n%s",
			logs)
	}
}

// TestNewGameWithNoSavePointKeepsTheOldAnswer: a commander who has never pressed Save has
// `commander.point = 0`, and there is no map to route to. That is the state every
// commander was in before the save point existed, so it must keep the old answer (the
// last point's map) - and say so.
//
// THE MISTAKE IT CATCHES: refusing every login whose commander has no save point, which
// would lock new players out of the game.
func TestNewGameWithNoSavePointKeepsTheOldAnswer(t *testing.T) {
	dir := t.TempDir()
	announceZone(t, dir, 10, 7002, "127.0.0.1", "reb_basic")

	h, logs := savePointBarrackHandler(t)
	h.Zones = &world.Registry{Dir: dir}
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{
		ID: 24, Name: "Arcis", ZoneID: 10, ChannelID: 0}} // SaveWarpID 0

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1; the log says:\n%s", len(out), logs)
	}
	if got := binary.LittleEndian.Uint32(out[0][22:]); got != 7002 {
		t.Fatalf("zonePort = %d, want 7002", got)
	}
	if !strings.Contains(logs.String(), "no start point is saved") {
		t.Fatalf("the routing line does not say why it used the last point's map; "+
			"it says:\n%s", logs)
	}
}
