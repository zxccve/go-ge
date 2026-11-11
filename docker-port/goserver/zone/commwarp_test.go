package zone

import (
	"context"
	"encoding/hex"
	"io"
	"log"
	"os"
	"strings"
	"testing"

	"geserver/protocol"
)

// realZonePointsPath is the shared spawn table the emitters write. Tests in this
// repo already read ../../data directly (see shop_data_test.go), so this is the
// same arrangement.
const realZonePointsPath = "../../data/zone_points.json"

// The 15 towns, in the CLIENT's own datatable_waypoint row order. This is the
// order the grid draws in, so a re-ordering here is a visible change, not a
// refactor.
//
// The map ids are the game's own `datatable_map.type` values, resolved by
// src\emit_zone_points.py from `Location` (which is a zone NAME, not a display
// string). Coimbra is 15 - that is the map this test was written on.
var wantTownMapIDs = []int{
	10, 15, 1301, 1401, 8001, 20200, 20401, 20600, 50, 20301, 9004, 78, 90, 140, 149,
}

// TestBuildWaypointListExactBytes pins the WHOLE packet for the real 15 towns: cmd,
// the length word, and every record.
//
// Why exact bytes and not "contains 15 rows": the client counts the records itself,
// as (len - 4) / 2, and steps 2 bytes per record (0x4b6504 SHR ESI,0x1 and
// 0x4b6522 ADD EDI,0x2 in FUN_004b64ea). A wrong length word or a 4-byte stride
// makes that produce a different count, and the window then draws the wrong towns
// with no error anywhere - the same silent failure this packet is being sent to fix.
func TestBuildWaypointListExactBytes(t *testing.T) {
	towns := make([]WaypointTown, 0, len(wantTownMapIDs))
	for i, id := range wantTownMapIDs {
		towns = append(towns, WaypointTown{ID: i + 1, MapID: id, Tab: 0})
	}

	got := BuildWaypointList(towns)

	// cmd(2) + len(2) + 15 * a 2-byte map id
	if want := 4 + 15*2; len(got) != want {
		t.Fatalf("packet = %d bytes, want %d (4 header + 15 x 2)", len(got), want)
	}
	if cmd := cmdOf(got); cmd != CmdZCWaypointList {
		t.Fatalf("cmd = %d, want %d (ZC_WAYPOINT_LIST)", cmd, CmdZCWaypointList)
	}
	if cmd := CmdZCWaypointList; cmd != 3742 {
		t.Fatalf("CmdZCWaypointList = %d, the client's table says 3742", cmd)
	}
	// The length word is the WHOLE packet, header included. The live sender sets
	// `out->len = bin.GetPosition()` after the header (Commander.cpp:2202-2222).
	if length := int(got[2]) | int(got[3])<<8; length != len(got) {
		t.Fatalf("len word = %d, packet is %d bytes (the live server writes the total)",
			length, len(got))
	}
	// The client's own arithmetic, done the client's way.
	if count := (int(got[2]) | int(got[3])<<8 - 4) / 2; count != 15 {
		t.Fatalf("the client would read %d record(s) from this packet, want 15", count)
	}

	// Record 0 = Reboldoeux (10 = 0x000a), record 1 = Coimbra (15 = 0x000f),
	// record 2 = Auch (1301 = 0x0515). Two bytes each, nothing between them.
	head := "9e0e22000a000f001505"
	if hexNow := hex.EncodeToString(got[:10]); hexNow != head {
		t.Fatalf("first 10 bytes:\n got %s\nwant %s\n"+
			"(cmd 3742 = 0x0e9e, len 34 = 0x0022, map 10, map 15, map 1301)",
			hexNow, head)
	}
	// The last record: Ailis (149 = 0x95) and nothing after it.
	last := got[len(got)-2:]
	if id := int(last[0]) | int(last[1])<<8; id != 149 {
		t.Fatalf("last record map id = %d, want 149 (ail_basic)", id)
	}
}

// TestBuildWaypointListEmptyIsHeaderOnly is the degenerate case: a header whose
// length word still says 4, which the client reads as zero records. It exists so
// that the length is never written as "records only".
func TestBuildWaypointListEmptyIsHeaderOnly(t *testing.T) {
	got := BuildWaypointList(nil)
	if len(got) != 4 {
		t.Fatalf("%d bytes, want only the 4-byte header", len(got))
	}
	if length := int(got[2]) | int(got[3])<<8; length != 4 {
		t.Fatalf("len word = %d, want 4", length)
	}
}

// TestBuildUIOpenIsThirtyFourBytes pins ZC_UI_OPEN: cmd, then the frame name in a
// fixed 32-byte field with no length word (PKS_DEFAULT, geZCPacket.h:2557). The
// client's own table says 3753 is a 34-byte packet, which is what this checks.
func TestBuildUIOpenIsThirtyFourBytes(t *testing.T) {
	got := BuildUIOpen(WaypointListFrame)
	if len(got) != 34 {
		t.Fatalf("%d bytes, want 34 (cmd 2 + frame 32)", len(got))
	}
	if cmd := cmdOf(got); cmd != 3753 {
		t.Fatalf("cmd = %d, want 3753 (ZC_UI_OPEN)", cmd)
	}
	if frame := string(got[2:]); frame != "waypointlist"+strings.Repeat("\x00", 20) {
		t.Fatalf("frame field = %q, want %q zero padded to 32 bytes",
			frame, "waypointlist")
	}
}

// TestTownGridFromRealData loads the emitted file itself: 15 towns, in the client's
// table order, each with a real map id.
//
// SKIPPED when the file is absent: it is derived data, and a checkout without a
// game tree is expected to lack it.
func TestTownGridFromRealData(t *testing.T) {
	if _, err := os.Stat(realZonePointsPath); err != nil {
		t.Skipf("no emitted map data (%v); run: cd src && python emit_zone_points.py", err)
	}
	points, err := LoadZonePoints(realZonePointsPath)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	if points.Version < 4 {
		t.Fatalf("zone_points.json is version %d; the town grid needs version 4 - "+
			"rebuild it with: cd src && python emit_zone_points.py", points.Version)
	}

	towns, skipped := points.TownGrid()
	if len(towns) != len(wantTownMapIDs) {
		t.Fatalf("%d town(s) in the grid, want %d - the client's window has exactly 15 "+
			"slots (waypointlist.xml bg_town0..14)", len(towns), len(wantTownMapIDs))
	}
	for i, want := range wantTownMapIDs {
		if towns[i].MapID != want {
			t.Fatalf("grid slot %d = map %d (%s), want %d - the slot order is the "+
				"client's datatable_waypoint order, and the player sees it",
				i, towns[i].MapID, towns[i].Location, want)
		}
		if towns[i].RestartWarpID <= 0 {
			t.Fatalf("grid slot %d (%s) carries no restart warp id - a town row without "+
				"one is not a teleport target", i, towns[i].Location)
		}
	}
	// Coimbra is where the Waypoint structure that started this stands.
	if towns[1].MapID != 15 || towns[1].Location != "coim_basic" {
		t.Fatalf("slot 1 = %+v, want coim_basic / map 15", towns[1])
	}
	// The 15 field rows (Tab 1) are NOT towns: they belong to the window's second
	// grid, which this slice does not fill.
	if len(skipped) != 15 {
		t.Fatalf("%d row(s) skipped, want the 15 Tab-1 field rows", len(skipped))
	}
	for _, s := range skipped {
		if s.Tab != 1 {
			t.Fatalf("row %s was skipped but is Tab %d - only Tab 1 may be skipped",
				s.Location, s.Tab)
		}
	}
}

// TestClickOnWaypointOpensTheTownGrid is the behaviour this slice is for: the NPC's
// script asks for the waypoint window, and the zone answers with the game's 15
// towns followed by the frame that OPENS it.
//
// The order matters: the live server sends the list first, then the open
// (Commander.cpp:654-658). Reversed, the window opens on an empty list and the
// player sees 15 blank thumbnails.
func TestClickOnWaypointOpensTheTownGrid(t *testing.T) {
	points, err := LoadZonePoints(realZonePointsPath)
	if err != nil {
		t.Skipf("no emitted map data (%v); run: cd src && python emit_zone_points.py", err)
	}
	npc := NPC{Handle: 0x40000026, Script: "WAYPOINT_AUCH_BASIC", X: 1000, Y: 1000}
	h := &Handler{
		Log:        log.New(io.Discard, "", 0),
		NPCs:       []NPC{npc},
		ZonePoints: points,
		Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
			// The waypoint NPC's own click script is what asks for this window:
			// out\lua_corpus\waypoint\waypoint_auch_basic.lua:13-17.
			if entry != "SCR_CLICK_WAYPOINT_AUCH_BASIC" {
				t.Errorf("entry = %q, want the waypoint click script", entry)
			}
			return ClickReply{WindowKind: WindowWaypoint}, nil
		},
	}
	sess := &Session{Pos: [3]int32{1000, 1000, 1}}

	out, err := h.clickTrigger(sess, frameClick(0, npc.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2: the town list then the frame that opens it", len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCWaypointList {
		t.Fatalf("first packet is %d, want %d (ZC_WAYPOINT_LIST) - the data must arrive "+
			"BEFORE the window opens", cmd, CmdZCWaypointList)
	}
	if len(out[0]) != 4+15*2 {
		t.Fatalf("town list = %d bytes, want %d", len(out[0]), 4+15*2)
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCUIOpen {
		t.Fatalf("second packet is %d, want %d (ZC_UI_OPEN)", cmd, CmdZCUIOpen)
	}
	if frame := string(out[1][2:]); frame != "waypointlist"+strings.Repeat("\x00", 20) {
		t.Fatalf("frame = %q, want waypointlist", frame)
	}
}

// TestClickOnWaypointWithNoRowsSaysSo is the honest failure: with no data loaded the
// zone sends NOTHING, rather than a window with an empty grid and a Save button that
// cannot work. Silence is safe here because the script asked for a panel, not for a
// dialog box, so the client is not modal.
func TestClickOnWaypointWithNoRowsSaysSo(t *testing.T) {
	npc := NPC{Handle: 0x40000026, Script: "WAYPOINT_AUCH_BASIC", X: 1000, Y: 1000}
	h := &Handler{
		Log:  log.New(io.Discard, "", 0),
		NPCs: []NPC{npc},
		Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
			return ClickReply{WindowKind: WindowWaypoint}, nil
		},
	}
	sess := &Session{Pos: [3]int32{1000, 1000, 1}}

	out, err := h.clickTrigger(sess, frameClick(0, npc.Handle))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s), want none: a nil ZonePoints must not draw an empty grid", len(out))
	}
}

// TestTownGridRequestsAreLoggedAndNotAnswered is the SCOPE line made checkable for
// the request this slice still does NOT answer: the saved-teleport window's SELECT
// (travel). A silent reply is deliberate, so it is pinned - a later slice that
// starts answering it should have to delete this test on purpose.
//
// FOUR IDS USED TO BE IN THIS LIST AND ARE NOT ANY MORE, for the same reason each
// time: a test that pins "nothing is sent" would pass for the wrong reason once the
// handler is real. 3743 (a click on a town) travels since R57
// (Handler.waypointSelect, zone\waypoint_travel_test.go). 3468 (the grid's Save
// button) saves since R68 (Handler.saveRestartWarp, zone\start_point_test.go) - and
// the handler below has no spawn table and no store, so it would refuse and send
// nothing either way. 3635 (the Warp List's Save) writes since R76
// (Handler.commWarpSave, zone\warplist_test.go), and 3636 / 3638 (remove and rename)
// with it.
func TestTownGridRequestsAreLoggedAndNotAnswered(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	sess := &Session{}
	for _, cmd := range []uint16{CmdCZCommWarpSelect} {
		// The bytes are a plausible body: a security header's worth of zeros plus
		// the row / teltype the request carries. The handler must not care.
		data := make([]byte, 13)
		data[0], data[1] = byte(cmd), byte(cmd>>8)
		out, err := h.Reply(context.Background(), sess, protocol.Frame{
			Cmd: cmd, Size: len(data), Data: data})
		if err != nil {
			t.Fatalf("cmd %d: %v", cmd, err)
		}
		if len(out) != 0 {
			t.Fatalf("cmd %d was answered with %d packet(s); this slice logs and "+
				"answers nothing", cmd, len(out))
		}
	}
}

// TestCommWarpListStaysEmpty pins the OTHER window. 3634 is not the town grid: the
// client feeds it to `warplist`, the SAVED-teleport window, and it is empty because
// this server has no saved teleports yet. It stays six bytes so that a later slice
// that fills it cannot accidentally be the thing that makes the town grid appear.
func TestCommWarpListStaysEmpty(t *testing.T) {
	for telType := 0; telType < commWarpTypes; telType++ {
		got := BuildCommWarpList(telType)
		if len(got) != 6 {
			t.Fatalf("type %d: %d bytes, want the 6-byte empty list", telType, len(got))
		}
		if cmd := cmdOf(got); cmd != CmdZCCommWarpList {
			t.Fatalf("type %d: cmd %d, want %d", telType, cmd, CmdZCCommWarpList)
		}
		if got[4] != byte(telType) || got[5] != 0 {
			t.Fatalf("type %d: teltype/count = %d/%d, want %d/0", telType, got[4], got[5], telType)
		}
	}
}
