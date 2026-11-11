package barrack

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
	"geserver/world"
)

// TestStartGameOKSizeAndFields pins the layout against the CLIENT's header and the
// live packet: FIXED 81 bytes, no length word, and startPos is three INT32 (the
// live packet holds 3317, -1515, 250 - not floats).
func TestStartGameOKSizeAndFields(t *testing.T) {
	b := BuildStartGameOK(StartGameOK{
		ZoneInstID: 7,
		IP:         "127.0.0.1",
		Port:       7002,
		MapName:    "reb_basic",
		Pos:        [3]int32{3317, -1515, 250},
		Channel:    2,
	})

	if len(b) != 81 {
		t.Fatalf("packet is %d bytes, want 81 (2+4+16+4+41+12+1+1)", len(b))
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 25 {
		t.Fatalf("command = %d, want 25", cmd)
	}
	if got := binary.LittleEndian.Uint32(b[2:]); got != 7 {
		t.Fatalf("zoneInstID = %d, want 7", got)
	}
	if got := trimNul(string(b[6:22])); got != "127.0.0.1" {
		t.Fatalf("zoneIP = %q, want 127.0.0.1", got)
	}
	if got := binary.LittleEndian.Uint32(b[22:]); got != 7002 {
		t.Fatalf("zonePort = %d, want 7002", got)
	}
	if got := trimNul(string(b[26:67])); got != "reb_basic" {
		t.Fatalf("map = %q, want reb_basic", got)
	}
	for i, want := range []int32{3317, -1515, 250} {
		if got := int32(binary.LittleEndian.Uint32(b[67+i*4:])); got != want {
			t.Fatalf("startPos[%d] = %d, want %d (int32, not float)", i, got, want)
		}
	}
	if b[79] != 2 {
		t.Fatalf("channel = %d, want 2", b[79])
	}
	if b[80] != 0 {
		t.Fatalf("continueMission = %d, want 0", b[80])
	}
}

// TestStartGameOKMatchesLiveLayout reads the live cmd 25 and checks our field
// offsets against it: the map name ends exactly where startPos begins, and the two
// trailing bytes are the last of the packet. The live packet's own data is the
// ORIGINAL server's address, so the VALUES are not copied - only the shape.
func TestStartGameOKMatchesLiveLayout(t *testing.T) {
	path := filepath.Join(replayLiveDir, "BC_STARTGAMEOK_25.bin")
	live, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("live packet not available: %v", err)
	}
	if len(live) != StartGameOKSize {
		t.Fatalf("live packet is %d bytes, StartGameOKSize says %d",
			len(live), StartGameOKSize)
	}
	if cmd := binary.LittleEndian.Uint16(live); cmd != 25 {
		t.Fatalf("live command = %d, want 25", cmd)
	}
	// The map name must be a readable class name, and it must END before startPos:
	// MAP_NAME_LEN is what makes the packet 81 bytes.
	if got := trimNul(string(live[26 : 26+mapNameLen])); got != "reb_basic" {
		t.Fatalf("live map = %q, want reb_basic", got)
	}
	// startPos really is int32: these three are a plausible position, not floats.
	pos := [3]int32{
		int32(binary.LittleEndian.Uint32(live[67:])),
		int32(binary.LittleEndian.Uint32(live[71:])),
		int32(binary.LittleEndian.Uint32(live[75:])),
	}
	for i, v := range pos {
		if v < -100000 || v > 100000 {
			t.Fatalf("startPos[%d] = %d - not a position; the field may be floats after all", i, v)
		}
	}
	t.Logf("live zoneIP=%q port=%d map=%q pos=%v channel=%d contMission=%d",
		trimNul(string(live[6:22])), binary.LittleEndian.Uint32(live[22:]),
		trimNul(string(live[26:67])), pos, live[79], live[80])
}

// TestReplyStartGameAnswersWithStartGameOK covers the case that unblocks the zone:
// CB_STARTGAME (22) must be answered with cmd 25, pointing at OUR zone, never at
// the capture's public address.
func TestReplyStartGameAnswersWithStartGameOK(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	var saved []ZoneEntry
	var team [][3]uint16
	h.Barrack = fakeBarrack{
		found:     true,
		cmdr:      CommanderRow{ID: 24, Name: "Arcis", ZoneID: 10, ChannelID: 0},
		saved:     &saved,
		teamSaved: &team,
	}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 25 {
		t.Fatalf("answer is cmd %d, want 25", cmd)
	}
	if got := trimNul(string(out[0][6:22])); got != "127.0.0.1" {
		t.Fatalf("zoneIP = %q, want our own zone 127.0.0.1", got)
	}
	if got := trimNul(string(out[0][26:67])); got != "reb_basic" {
		t.Fatalf("map = %q, want reb_basic for zone 10", got)
	}
	if strings.Contains(string(out[0]), "85.11.190.15") {
		t.Fatal("the answer carries the CAPTURE's public IP - cmd 25 must be built, never replayed")
	}

	// And the picked team must be recorded for the zone, in the database.
	if len(saved) != 1 {
		t.Fatalf("SaveZoneEntry called %d time(s), want 1", len(saved))
	}
	got := saved[0]
	if got.CommanderID != 24 || got.Channel != 1 || got.Continue {
		t.Fatalf("recorded %+v, want commander 24 channel 1 start", got)
	}
	if got.Slots != [3]uint16{0, 3, EmptyCharSlot} {
		t.Fatalf("recorded slots %v, want [0 3 255] - the raw triple, empties kept", got.Slots)
	}

	// The team must be PERSISTED too, or the next barrack has no previous team and
	// Last Point never becomes a Continue Game.
	if len(team) != 1 {
		t.Fatalf("SaveTeamPositions called %d time(s), want 1", len(team))
	}
	if team[0] != [3]uint16{0, 3, EmptyCharSlot} {
		t.Fatalf("saved team %v, want the picked slots in order", team[0])
	}
}

// TestReplyContinueGameToo pins that CB_CONTINUEGAME (23) gets the same answer.
func TestReplyContinueGameToo(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	h.Barrack = fakeBarrack{found: true, cmdr: CommanderRow{ID: 24, ZoneID: 10}}

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 23, Size: 22})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || binary.LittleEndian.Uint16(out[0]) != 25 {
		t.Fatalf("cmd 23 was not answered with cmd 25")
	}
}

// TestZoneAddrAndMapName covers the two lookups the answer depends on.
func TestZoneAddrAndMapName(t *testing.T) {
	ip, port, ok := ParseZoneAddr("127.0.0.1:7002")
	if !ok || ip != "127.0.0.1" || port != 7002 {
		t.Fatalf("ParseZoneAddr = %q %d %v", ip, port, ok)
	}
	for _, bad := range []string{"", "127.0.0.1", ":7002", "host:port", "host:70000"} {
		if _, _, ok := ParseZoneAddr(bad); ok {
			t.Fatalf("ParseZoneAddr(%q) said ok", bad)
		}
	}
	if name, ok := ZoneMapName(10); !ok || name != "reb_basic" {
		t.Fatalf("ZoneMapName(10) = %q %v, want reb_basic", name, ok)
	}
	if _, ok := ZoneMapName(999); ok {
		t.Fatal("ZoneMapName(999) claimed a name for an unknown zone")
	}
}

// announceZone writes a live presence file, the way src\world.py does.
func announceZone(t *testing.T, dir string, mapID, port int, ip, mapName string) {
	t.Helper()
	rec := fmt.Sprintf(`{"map_id":%d,"channel":0,"ip":%q,"port":%d,`+
		`"map_name":%q,"last_seen":%f}`,
		mapID, ip, port, mapName, float64(time.Now().Unix()))
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("zone_%d.json", port)),
		[]byte(rec), 0o644); err != nil {
		t.Fatalf("write presence file: %v", err)
	}
}

// TestStartGameUsesTheZoneRegistry is the hardcode that blocked cross-map
// travel: a character saved on map 20 must be sent to the zone that ANNOUNCED
// map 20, with that zone's own map name - not to 127.0.0.1:7002 / "reb_basic",
// which is a different world than the one the player is standing in.
func TestStartGameUsesTheZoneRegistry(t *testing.T) {
	dir := t.TempDir()
	announceZone(t, dir, 20, 7003, "10.0.0.5", "mission_sales1")

	h := quietHandler(t, fakeStore{ok: true})
	h.Zones = &world.Registry{Dir: dir}
	h.Barrack = fakeBarrack{found: true,
		cmdr: CommanderRow{ID: 24, ZoneID: 20, ChannelID: 0}}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1", len(out))
	}
	if got := trimNul(string(out[0][6:22])); got != "10.0.0.5" {
		t.Fatalf("zoneIP = %q, want the announced 10.0.0.5", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][22:]); got != 7003 {
		t.Fatalf("zonePort = %d, want the announced 7003", got)
	}
	if got := trimNul(string(out[0][26:67])); got != "mission_sales1" {
		t.Fatalf("map = %q, want mission_sales1 - the zone names its own world", got)
	}
}

// TestStartGameFallsBackWhenNoZoneIsAnnounced is the other half of the deal: an
// empty registry must leave the OLD answer exactly as it was, because that
// answer is what works in the live client today. A registry that answers "I do
// not know" is not allowed to change anything.
func TestStartGameFallsBackWhenNoZoneIsAnnounced(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	h.Zones = &world.Registry{Dir: t.TempDir()} // readable, but no zone inside
	h.Barrack = fakeBarrack{found: true,
		cmdr: CommanderRow{ID: 24, ZoneID: 10, ChannelID: 0}}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1", len(out))
	}
	ip := trimNul(string(out[0][6:22]))
	port := binary.LittleEndian.Uint32(out[0][22:])
	name := trimNul(string(out[0][26:67]))
	if ip != "127.0.0.1" || port != 7002 || name != "reb_basic" {
		t.Fatalf("fallback answer changed: ip=%q port=%d map=%q - want "+
			"127.0.0.1/7002/reb_basic", ip, port, name)
	}
}

// TestStartGameNamesTheMapFromTheTable is the step between the registry and the
// constants: with no live zone, map 15's name comes from the shared table
// (data\zone_points.json), NOT from the built-in table - which does not know map 15
// and would have answered "reb_basic", a different world.
func TestStartGameNamesTheMapFromTheTable(t *testing.T) {
	names := func(mapID uint16) (string, bool) {
		if mapID == 15 {
			return "coim_basic", true
		}
		return "", false
	}

	h := quietHandler(t, fakeStore{ok: true})
	h.Zones = &world.Registry{Dir: t.TempDir()} // readable, but no zone inside
	h.MapNames = names
	h.Barrack = fakeBarrack{found: true,
		cmdr: CommanderRow{ID: 24, ZoneID: 15, ChannelID: 0}}

	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1", len(out))
	}
	// The address still comes from the flag: only the NAME moved.
	if ip := trimNul(string(out[0][6:22])); ip != "127.0.0.1" {
		t.Fatalf("zoneIP = %q, want the unchanged flag default", ip)
	}
	if name := trimNul(string(out[0][26:67])); name != "coim_basic" {
		t.Fatalf("map = %q, want coim_basic from the table", name)
	}

	// An id the table does not know keeps the old loud fallback.
	h.Barrack = fakeBarrack{found: true,
		cmdr: CommanderRow{ID: 24, ZoneID: 77, ChannelID: 0}}
	out, err = h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if name := trimNul(string(out[0][26:67])); name != "reb_basic" {
		t.Fatalf("map = %q for an unknown id, want the reb_basic fallback", name)
	}
}
