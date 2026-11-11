package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
	"geserver/text"
)

// waypoint 3743 tests. The layout they send is the client's own: a 10-byte security
// header then the map id as a WORD at offset 10 (see waypoint_travel.go's file comment
// for the source lines), which is 12 of the 13 bytes the client's table registers.

// waypointPacket builds a CZ_WAYPOINT_SELECT the way the client's own sender does.
// `extra` is the one byte the shipped client's registered size has and source2's struct
// does not name; it is set to a value no map id could be, so a parser that read the map
// id from the wrong offset would answer 0xBEEF territory instead of the town.
func waypointPacket(mapID int) []byte {
	pt := pack(CmdCZWaypointSelect) // 10-byte security header
	pt = append(pt, byte(mapID), byte(mapID>>8), 0xBE)
	return pt
}

// waypointHandler is a zone serving Coimbra (map 15) that can travel: the real spawn
// table, a live Reboldoeux (map 10) in the registry, and a private handoff folder.
//
// Every number these tests assert is the table's own, so a data change shows up as a
// failure here rather than as a player landing in the wrong place.
func waypointHandler(t *testing.T) (*Handler, string, *bytes.Buffer) {
	t.Helper()
	points := zoneTable(t) // skips when the emitted file is absent
	transfers := t.TempDir()
	logs := &bytes.Buffer{}
	h := &Handler{
		Log:        log.New(logs, "", 0),
		MapID:      15,
		MapName:    "coim_basic",
		ZonePoints: points,
		Zones:      liveZone(t, 10, 7002),
		Transfers:  transfers,
	}
	// The word tables, when the client data is on this machine: the waypoint line
	// carries the token <$>10000205</> and the resolver turns it into the sentence
	// the player reads. Not fatal when absent - the client folder is not in git.
	if book, err := text.Load(
		filepath.Join("..", "..", "data", "locale_id_map.json"),
		filepath.Join("..", "..", "..", "ge", "dictionary", "dictionary_local.xml"),
	); err == nil {
		h.Text = book
	} else {
		t.Logf("no dictionary (%v) - the waypoint line keeps its raw token", err)
	}
	return h, transfers, logs
}

// TestParseWaypointSelectFromTheClientLayout pins the offset. The body is the MAP ID and
// not the grid index (script.cpp:1663 sends `pWpInfo->mapID`), so 0xBEEF in the byte
// after it must change nothing.
func TestParseWaypointSelectFromTheClientLayout(t *testing.T) {
	for _, mapID := range []int{10, 15, 149} {
		pt := waypointPacket(mapID)
		if len(pt) != waypointSelectSize {
			t.Fatalf("test packet is %d bytes, want the client's table size %d",
				len(pt), waypointSelectSize)
		}
		got, ok := ParseWaypointSelect(pt)
		if !ok {
			t.Fatalf("map %d: the parser refused a full-size packet", mapID)
		}
		if got != mapID {
			t.Fatalf("map id = %d, want %d - it is the WORD at offset 10, right after "+
				"the 10-byte security header", got, mapID)
		}
	}
	// A packet too short to hold the word is a malformed client, not a crash and not a
	// zero (map 0 is not a map, and treating it as one would be a wrong refusal).
	for _, n := range []int{0, 2, 10, 11} {
		if _, ok := ParseWaypointSelect(make([]byte, n)); ok {
			t.Fatalf("%d bytes parsed as a map id; the WORD needs 12", n)
		}
	}
}

// TestWaypointSelectSendsOneMoveZone is the whole point of the slice: a click on
// Reboldoeux from Coimbra sends EXACTLY ONE ZC_MOVE_ZONE, byte for byte the packet a gate
// sends - 3 bytes, command 3593, flag byte FALSE.
//
// It goes through Handler.Reply, so it also proves the dispatch is wired: the command is
// no longer swallowed by the "logged only" case.
func TestWaypointSelectSendsOneMoveZone(t *testing.T) {
	h, _, logs := waypointHandler(t)
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{2209, 16830, 233}, {2209, 16830, 233}, {2209, 16830, 233}})

	pt := waypointPacket(10) // Reboldoeux, the town with a live zone
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZWaypointSelect, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want exactly 1 (the 3593): %s", len(out), logs)
	}
	p := out[0]
	if len(p) != MoveZoneSize {
		t.Fatalf("MOVE_ZONE is %d bytes, want %d", len(p), MoveZoneSize)
	}
	if got := cmdOf(p); got != CmdZCMoveZone {
		t.Fatalf("cmd = %d, want %d (ZC_MOVE_ZONE)", got, CmdZCMoveZone)
	}
	if p[2] != moveZoneFlag {
		t.Fatalf("flag byte = %d, want %d - 0x01 tells the client this is a WORLD PvP "+
			"jump and it then answers nothing at all", p[2], moveZoneFlag)
	}
	// The pending move is the SAME state a gate leaves behind, so the client's own
	// CZ_MOVE_ZONE_OK can finish it (that is what the next test proves).
	if a.sess.Gate == nil {
		t.Fatalf("the click did not leave a pending map change; the 3686 would answer " +
			"\"no map change pending\" and the player would never travel")
	}
	if a.sess.Gate.gate.ToID != 10 || a.sess.Gate.gate.To != "reb_basic" {
		t.Fatalf("pending destination = map %d (%s), want 10 (reb_basic)",
			a.sess.Gate.gate.ToID, a.sess.Gate.gate.To)
	}
	// The ONE line that has to be readable without the client.
	line := logs.String()
	for _, want := range []string{"WAYPOINT_SELECT map 10", "town 1", "live zone ",
		"landing (3946, 193, cell 146)", "from the warp row"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the 3743 log line does not name %q:\n%s", want, line)
		}
	}
	// Printed so the shape of the log can be read here without a client.
	t.Logf("a click on Reboldoeux from Coimbra logs:\n%s", line)
}

// TestWaypointSelectAnswersTheHandshakeWithTheTownArrival is the landing rule end to end:
// the client's 3686 produces the 101-byte 3594 AND the handoff file, both carrying
// Reboldoeux's RESTART WARP point (warp 203 -> 3946, 193, cell 146) and not the map's
// generic spawn (4110, -130), which is where a gate-less entry would land.
func TestWaypointSelectAnswersTheHandshakeWithTheTownArrival(t *testing.T) {
	h, transfers, _ := waypointHandler(t)
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{2209, 16830, 233}, {2209, 16830, 233}, {2209, 16830, 233}})
	a.sess.AccountID = 7000001

	pt := waypointPacket(10)
	if out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZWaypointSelect, Size: len(pt), Data: pt}); err != nil {
		t.Fatalf("Reply: %v", err)
	} else if len(out) != 1 {
		t.Fatalf("%d packet(s) for the click, want 1", len(out))
	}

	// The town row's own warp id, from the emitted data: asserting it here is what ties
	// the test to the table rather than to a constant in this file.
	town, ok := h.ZonePoints.waypointRow(10)
	if !ok || town.RestartWarpID != 203 {
		t.Fatalf("map 10's town row = %+v, ok %v; want the Reboldoeux row (warp 203)",
			town, ok)
	}

	ack := pack(CmdCZMoveZoneOK) // the 10-byte security header and no body
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(ack), Data: ack})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("MOVE_ZONE_OK answered %d packet(s), want 1", len(out))
	}
	p := out[0]
	if len(p) != MoveZoneOKSize {
		t.Fatalf("ZC_MOVE_ZONE_OK is %d bytes, want %d", len(p), MoveZoneOKSize)
	}
	if got := cmdOf(p); got != CmdZCMoveZoneOK {
		t.Fatalf("cmd = %d, want %d", got, CmdZCMoveZoneOK)
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != 10 {
		t.Fatalf("+2 zoneInstID = %d, want 10", got)
	}
	if got := string(p[26 : 26+len("reb_basic")]); got != "reb_basic" {
		t.Fatalf("+26 map = %q, want reb_basic", got)
	}
	if x, y := int32(binary.LittleEndian.Uint32(p[87:])),
		int32(binary.LittleEndian.Uint32(p[91:])); x != 3946 || y != 193 {
		t.Fatalf("+87 startPos = (%d, %d), want the warp 203 row's (3946, 193)", x, y)
	}
	if cell := int32(binary.LittleEndian.Uint32(p[95:])); cell != 146 {
		t.Fatalf("+95 startPos CELL = %d, want 146 (warp 203's own cell)", cell)
	}

	tf, err := ReadTransfer(TransferPath(transfers, a.sess.AccountID), a.sess.AccountID,
		10, time.Now())
	if err != nil {
		t.Fatalf("the handoff the 3594 was built with is not usable: %v", err)
	}
	if tf.X != 3946 || tf.Y != 193 || tf.Cell != 146 {
		t.Fatalf("handoff point = (%d, %d, cell %d), want the town's (3946, 193, 146)",
			tf.X, tf.Y, tf.Cell)
	}
	// The map's generic spawn is a DIFFERENT place, and landing there is the bug this
	// test exists to catch: 4110, -130 is what SpawnFor(10) answers.
	if tf.X == 4110 && tf.Y == -130 {
		t.Fatalf("the team landed on map 10's generic spawn, not on the town's arrival")
	}
}

// TestWaypointLandingIsTheWarpRow is the two numbers, straight out of the emitted data:
// warp 203 -> map 10 at (3946, 193, cell 146) and warp 213 -> map 15 at (2268, 16931,
// cell 233). They can be eyeballed with no client and no server.
func TestWaypointLandingIsTheWarpRow(t *testing.T) {
	points := zoneTable(t)
	for _, tc := range []struct {
		town  int
		mapID int
		warp  int
		pos   [3]int32
	}{
		{1, 10, 203, [3]int32{3946, 193, 146}},   // Reboldoeux
		{2, 15, 213, [3]int32{2268, 16931, 233}}, // Coimbra
	} {
		town, ok := points.waypointRow(tc.mapID)
		if !ok {
			t.Fatalf("map %d is not a row in the waypoint list any more", tc.mapID)
		}
		if town.ID != tc.town || town.RestartWarpID != tc.warp {
			t.Fatalf("map %d's town row = id %d warp %d, want id %d warp %d",
				tc.mapID, town.ID, town.RestartWarpID, tc.town, tc.warp)
		}
		landing, err := points.WaypointLanding(town)
		if err != nil {
			t.Fatalf("map %d: %v", tc.mapID, err)
		}
		if !landing.FromWarp {
			t.Fatalf("map %d landed from %q, want the restart warp row %d",
				tc.mapID, landing.Source, tc.warp)
		}
		if landing.Pos != tc.pos {
			t.Fatalf("warp %d lands at %v, want %v", tc.warp, landing.Pos, tc.pos)
		}
		if !strings.Contains(landing.Source, fmt.Sprint(tc.warp)) {
			t.Fatalf("map %d: source %q does not name warp %d", tc.mapID,
				landing.Source, tc.warp)
		}
	}
}

// TestWaypointLandingFallsBackToTheSpawn is the "the table cannot answer" case: the
// restart row names a DIFFERENT warp than the town row, so the team must land on the
// map's own spawn and the source must SAY why - never a number invented here.
func TestWaypointLandingFallsBackToTheSpawn(t *testing.T) {
	spawnCell := int32(146)
	otherCell := int32(9)
	points := &ZonePoints{
		Version: ZonePointsVersion,
		Maps: map[string]MapEntry{
			"10": {
				Name: "reb_basic",
				Spawn: &MapPoint{X: 4110, Y: -130, Cell: &spawnCell,
					Source: "townpoint:1"},
				Restart: &RestartPoint{WarpID: 999, X: 1, Y: 2, Cell: &otherCell},
			},
		},
	}
	town := WaypointTown{ID: 1, Location: "reb_basic", MapID: 10, RestartWarpID: 203, Tab: 0}

	landing, err := points.WaypointLanding(town)
	if err != nil {
		t.Fatalf("WaypointLanding: %v", err)
	}
	if landing.FromWarp {
		t.Fatalf("the restart row is warp 999 and the town row says 203; the warp row "+
			"must NOT answer - landing %v from %q", landing.Pos, landing.Source)
	}
	if landing.Pos != [3]int32{4110, -130, 146} {
		t.Fatalf("fallback landing = %v, want the map's spawn (4110, -130, 146)",
			landing.Pos)
	}
	if !strings.Contains(landing.Source, "spawn fallback") ||
		!strings.Contains(landing.Source, "999") {
		t.Fatalf("the fallback source must name the spawn AND the row that could not "+
			"answer: %q", landing.Source)
	}

	// A town whose destination cannot be placed at all is an ERROR, not a zero point.
	points.Maps["10"] = MapEntry{Name: "reb_basic"} // no spawn, no restart
	if _, err := points.WaypointLanding(town); err == nil {
		t.Fatalf("a map with no spawn and no restart row resolved a landing")
	}
}

// TestWaypointSelectRefusesWithoutALiveZone: the one case that would strand the player.
// 3593 goes out, the client answers, and there is no address for the 3594 - so nothing
// is sent at all, and the log says which map and which folder were looked in.
func TestWaypointSelectRefusesWithoutALiveZone(t *testing.T) {
	points := zoneTable(t)
	logs := &bytes.Buffer{}
	h := &Handler{
		Log: log.New(logs, "", 0), MapID: 15, MapName: "coim_basic",
		ZonePoints: points,
		Zones:      liveZone(t, 1301, 7006), // Auch is up; Reboldoeux is not
		Transfers:  t.TempDir(),
	}
	sess := &Session{skipped: map[uint16]int{}}

	out, err := h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: waypointSelectSize, Data: waypointPacket(10)})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent with no live zone for map 10, want none", len(out))
	}
	if sess.Gate != nil {
		t.Fatalf("a refused click left a pending map change behind")
	}
	line := logs.String()
	if strings.Count(strings.TrimSpace(line), "\n") != 0 {
		t.Fatalf("want ONE log line for the refusal, got:\n%s", line)
	}
	if !strings.Contains(line, "REFUSED - no live zone serves map 10") {
		t.Fatalf("the line does not name the missing zone:\n%s", line)
	}
}

// TestWaypointSelectRefusesAnUnknownMap: a map id that is not a town row sends nothing.
// It is the case a wrong 3743 layout would produce, so it must be loud.
func TestWaypointSelectRefusesAnUnknownMap(t *testing.T) {
	h, _, logs := waypointHandler(t)
	sess := &Session{skipped: map[uint16]int{}}

	out, err := h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: waypointSelectSize, Data: waypointPacket(4242)})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent for an unknown map id, want none", len(out))
	}
	if !strings.Contains(logs.String(), "REFUSED - map 4242 is not a row") {
		t.Fatalf("the refusal does not name the map id:\n%s", logs.String())
	}

	// The layout safety net: this build's packet is one byte longer than source2's
	// struct, so the map id COULD sit at 11 instead of 10. A packet whose offset 10
	// reads 2560 (not a row) and whose offset 11 reads 10 (Reboldeux) must say so -
	// that is the difference between "the click does nothing" and one number to fix.
	logs.Reset()
	shifted := pack(CmdCZWaypointSelect)
	shifted = append(shifted, 0x00, 0x0A, 0x00)
	out, err = h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: len(shifted), Data: shifted})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent for a shifted map id, want none: a wrong offset "+
			"must never travel", len(out))
	}
	if !strings.Contains(logs.String(), "one byte later reads map 10") {
		t.Fatalf("a map id that only resolves one byte later must be CALLED OUT in "+
			"the log, not swallowed:\n%s", logs.String())
	}
}

// TestWaypointSelectRefusesATabOneRow: the window's second grid is fields, not towns, and
// those rows carry no restart warp.
func TestWaypointSelectRefusesATabOneRow(t *testing.T) {
	h, _, logs := waypointHandler(t)
	sess := &Session{skipped: map[uint16]int{}}

	// 1301 is Auch, a town; rebuild the handler's table with its row marked field-tab so
	// the refusal is tested without needing a real field map to be live.
	points := *h.ZonePoints
	points.Waypoints = append([]WaypointTown(nil), h.ZonePoints.Waypoints...)
	for i := range points.Waypoints {
		if points.Waypoints[i].MapID == 1301 {
			points.Waypoints[i].Tab = 1
			points.Waypoints[i].RestartWarpID = 0
		}
	}
	h.ZonePoints = &points

	out, err := h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: waypointSelectSize, Data: waypointPacket(1301)})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent for a Tab 1 row, want none", len(out))
	}
	if !strings.Contains(logs.String(), "is Tab 1, the FIELD grid") {
		t.Fatalf("the refusal does not say the row is the field grid:\n%s", logs.String())
	}
}

// TestWaypointSelectOnTheSameMapTellsThePlayer: clicking the town you already stand in
// must not travel (the client and the server are on the same map instance) - it tells the
// player the RIGHT sentence, in the client's own MODAL box.
//
// THE TYPE IS MEASURED, NOT GONE (2026-10-30 evening, TYPE-HUNT -> MEASURED). The
// reference server answers this with MESSAGE_TYPE_CURRENT_WAY_POINT at
// MESSAGE_LEVEL_WARNING (Commander_Packet.cpp:2028), i.e. a box. Two numbers were
// wrong on the owner's client: type 136 boxed the level-limit sentence, type 135 boxed
// "Password is incorrect.". The owner then ran `/typemsg 147 3` and the box read "You
// have chosen the waypoint of the current area." - so 147 is the number, the box is
// back, and the text we send is only the log note (the client replaces it for any type
// but 0). See zone\message.go for the ladder.
func TestWaypointSelectOnTheSameMapTellsThePlayer(t *testing.T) {
	h, _, logs := waypointHandler(t)
	sess := &Session{skipped: map[uint16]int{}}

	out, err := h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: waypointSelectSize, Data: waypointPacket(15)})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want just the 3596 notice: %s", len(out), logs)
	}
	if got := cmdOf(out[0]); got != CmdZCMessage {
		t.Fatalf("cmd = %d, want %d (ZC_MESSAGE) - a map change here would send the "+
			"player to the map they are on", got, CmdZCMessage)
	}
	if sess.Gate != nil {
		t.Fatalf("clicking the town you stand in started a map change")
	}
	// Literals, not the constants, so a wrong constant cannot pass its own test:
	// 147 is the MEASURED waypoint type and 3 is the level whose title is "Message".
	if v := binary.LittleEndian.Uint32(out[0][4:]); v != 147 {
		t.Fatalf("type = %d, want 147 (the MEASURED MESSAGE_TYPE_CURRENT_WAY_POINT) - "+
			"136 boxed the level-limit sentence and 135 boxed \"Password is "+
			"incorrect.\"", v)
	}
	if lv := out[0][8]; lv != 3 {
		t.Fatalf("level = %d, want 3 (modal box, title \"Message\") - level 0 is the "+
			"plain cyan chat line", lv)
	}
	// The text on the wire is the LOG NOTE, never the sentence: for any type but 0
	// the client replaces it with its own line (GEPacket.cpp:210-211).
	if body := string(out[0][45 : len(out[0])-1]); body != currentWaypointText {
		t.Fatalf("text = %q, want the log note %q", body, currentWaypointText)
	}
	if !strings.Contains(logs.String(), "already on map 15") {
		t.Fatalf("the refusal does not say the player is already there:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "type 147 level 3") {
		t.Fatalf("the log line does not name the pair that was sent:\n%s", logs.String())
	}
}

// TestWaypointSelectRefusesWhileAMoveIsRunning: one 3593 per map change. A second click
// while the client has not answered yet must send nothing, or its 3686 would finish the
// wrong destination.
func TestWaypointSelectRefusesWhileAMoveIsRunning(t *testing.T) {
	h, _, logs := waypointHandler(t)
	sess := &Session{skipped: map[uint16]int{}, Gate: &gateMove{
		gate: MapGate{Name: "COIM_BASIC-FLD_COIM", To: "fld_coim", ToID: 208}}}

	out, err := h.waypointSelect(sess, protocol.Frame{
		Cmd: CmdCZWaypointSelect, Size: waypointSelectSize, Data: waypointPacket(10)})
	if err != nil {
		t.Fatalf("waypointSelect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) sent while a map change was pending, want none", len(out))
	}
	if !strings.Contains(logs.String(), "already running") {
		t.Fatalf("the refusal does not say a change is running:\n%s", logs.String())
	}
}
