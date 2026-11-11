package zone

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"geserver/barrack"
)

// R70: THE SAVED START POINT AND THE LAST POINT ARE TWO FACTS, AND THE PLAYER'S BUTTON
// DECIDES WHICH ONE WINS.
//
// No client and no database here: every store is a fake whose shape IS the contract
// (zone\start_point.go UseStartPoint, zone\last_point.go UseLastPoint,
// zone\entry_kind.go UseEntryKind). The one test that touches PostgreSQL is in
// last_point_db_test.go and is off by default, because it writes.

// TestEntryOrderFollowsTheBarrackButton is the requirement of R70's entry half: the
// SAME commander, with BOTH a saved start point and a last point on file, must enter at
// a different place depending on which of the barrack's two handled buttons was
// pressed.
//
//	New Game   (22, kind 0) -> the SAVED START POINT wins
//	Continue   (23, kind 1) -> the LAST POINT wins
//
// (The client's third button, the mission one at cmd 24, is parked by the owner and is
// not answered, so there is no case for it here.)
//
// THE MISTAKE IT CATCHES: one fixed order for both buttons, which is what made
// "Continue Game" and "New Game" do the same thing, and (before that) let a log-out
// outrank a save the player had chosen.
func TestEntryOrderFollowsTheBarrackButton(t *testing.T) {
	// Coimbra's restart warp row on map 15, from data\zone_points.json.
	const saveWarp = 213
	// The last point is STORED as a height (R73: `character_position.ZPOS` is a world
	// height, never a navmesh cell), and the mesh resolves that height back to the cell
	// the client is given.
	lastPointCell := [3]int32{5000, 6000, 233}
	lastPointStored := [3]int32{5000, 6000, 250}
	savePointPos := [3]int32{2268, 16931, 233}

	cases := []struct {
		name       string
		kind       EntryKind
		wantPlaced string
		wantPos    [3]int32
		wantLog    []string
		notInLog   []string
	}{
		{
			name:       "New Game uses the saved start point even though a last point exists",
			kind:       EntryNew,
			wantPlaced: "the saved start point (map 15, warp 213)",
			wantPos:    savePointPos,
			wantLog:    []string{"SAVED START POINT of commander 25", "ENTRY KIND"},
			notInLog:   []string{"LAST POINT of commander 25"},
		},
		{
			name:       "Continue Game uses the last point even though a save point exists",
			kind:       EntryContinue,
			wantPlaced: "the Last Point of commander 25 (map 15, stored z 250, cell 233)",
			wantPos:    lastPointCell,
			wantLog:    []string{"LAST POINT of commander 25"},
			notInLog: []string{"SAVED START POINT of commander 25",
				"no saved start point yet"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, logs := startPointHandler(t) // map 15 = coim_basic
			newTestMesh().onGround(lastPointCell, lastPointStored[2]).wire(h)
			sf := &startPointFake{sp: StartPoint{WarpID: saveWarp}, found: true}
			lf := &lastPointFake{
				point: LastPoint{MapID: 15, Pos: lastPointStored, Name: "TesterF"},
				found: true,
			}
			h.UseStartPoint(sf.loadOf, sf.saveOf)
			h.UseLastPoint(lf.load, lf.save)
			h.UseEntryKind(func(context.Context, int32) (EntryKind, error) {
				return c.kind, nil
			})

			// AccountID 0 skips the handoff file on purpose: this test is about the two
			// stored points, and a handoff always wins over both.
			sess := &Session{CommanderID: 25}
			base, _, placed := h.placeTeam(context.Background(), sess)

			if placed != c.wantPlaced {
				t.Fatalf("placed = %q, want %q; the log says:\n%s", placed,
					c.wantPlaced, logs)
			}
			if base != c.wantPos {
				t.Fatalf("entry point = %v, want %v; the log says:\n%s", base,
					c.wantPos, logs)
			}
			for _, want := range c.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("the log does not say %q; it says:\n%s", want, logs)
				}
			}
			for _, unwanted := range c.notInLog {
				if strings.Contains(logs.String(), unwanted) {
					t.Fatalf("the log says %q, which means the WRONG point was tried "+
						"for this button; it says:\n%s", unwanted, logs)
				}
			}
			// Every case still ends with ONE line naming the decision.
			if !strings.Contains(logs.String(), "ENTRY POINT for commander 25 on map 15") {
				t.Fatalf("no ENTRY POINT line; the log says:\n%s", logs)
			}
			t.Logf("entry log:\n%s", logs)
		})
	}
}

// TestLastPointWritesTheTeamsCharacterPositions is the write half of the last point:
// ONE `character_position` row per member of the team, all at the LEADER's walked spot,
// because that is the only live position this zone has.
//
// THE MISTAKE IT CATCHES: writing only the leader (the oracle's table is keyed by
// character, and every follower then has no row), or writing character 0 for a slot
// that never got a handle.
//
// `commander.point` (the save point) cannot be touched from here at all: the save hook
// takes (commander, character ids, map/pos) and has no argument for a warp id.
func TestLastPointWritesTheTeamsCharacterPositions(t *testing.T) {
	f := &lastPointFake{}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), MapID: 10, MapName: "reb_basic",
		Transfers: t.TempDir()}
	// The leader's spot on the map's mesh: cell 146, ground height 257.
	newTestMesh().onGround([3]int32{4301, 233, 146}, 257).wire(h)
	h.UseLastPoint(f.load, f.save)

	sess := &Session{
		skipped:     map[uint16]int{},
		AccountID:   1,
		CommanderID: 25,
		Team: []Character{
			{Handle: 1792, Name: "TesterF"},
			{Handle: 1793, Name: "TesterG"},
			{Handle: 0, Name: "NeverSpawned"}, // no handle: names no row
		},
		Pos: [3]int32{4301, 233, 146},
	}
	h.noteLastPoint(sess, lastPointSysMenu)

	if f.saves != 1 {
		t.Fatalf("%d save(s), want exactly 1", f.saves)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d call(s), want exactly 1", len(f.calls))
	}
	got := f.calls[0]
	want := []uint32{1792, 1793}
	if len(got) != len(want) {
		t.Fatalf("character ids = %v, want the two members that have a handle %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("character ids = %v, want %v", got, want)
		}
	}
	wantPos := [3]int32{sess.Pos[0], sess.Pos[1], 257}
	if lp := f.saved[0]; lp.MapID != 10 || lp.Pos != wantPos {
		t.Fatalf("saved %+v, want map 10 at %v (x/y the leader's, z the HEIGHT 257)",
			lp, wantPos)
	}
	if f.saved[0].Pos[2] == sess.Pos[2] {
		t.Fatalf("zpos = %d, the navmesh CELL - the oracle's ZPOS holds a height",
			f.saved[0].Pos[2])
	}
	// The log names the two rows written and the column it did NOT write.
	for _, want := range []string{"wrote commander.zone_id",
		"character_position rows for 2 character(s)",
		"commander.point (the SAVED START", "was NOT touched"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the save line does not name %q; it says:\n%s", want, logs)
		}
	}
}

// TestRestartWarpMapAnswersTheOwningMap pins the reverse lookup the saved start point
// depends on, INCLUDING the determinism rule: the map table is a Go map, so an
// iteration order that leaked into the answer would make two identical logins land in
// two different towns.
func TestRestartWarpMapAnswersTheOwningMap(t *testing.T) {
	cell := int32(146)
	twoMaps := &ZonePoints{
		Version: ZonePointsVersion,
		Maps: map[string]MapEntry{
			"10": {Name: "reb_basic", Restart: &RestartPoint{WarpID: 203, Cell: &cell}},
			"15": {Name: "coim_basic", Restart: &RestartPoint{WarpID: 213, Cell: &cell}},
			// A hand-made table where two maps claim one warp id: the LOWEST map id
			// must win, every time.
			"20": {Name: "fld_alq", Restart: &RestartPoint{WarpID: 203, Cell: &cell}},
			// A map with no restart row at all must not match anything.
			"30": {Name: "office_coim"},
		},
	}

	if mapID, ok := twoMaps.RestartWarpMap(213); !ok || mapID != 15 {
		t.Fatalf("warp 213 -> map %d (ok %v), want 15", mapID, ok)
	}
	for i := 0; i < 20; i++ {
		if mapID, ok := twoMaps.RestartWarpMap(203); !ok || mapID != 10 {
			t.Fatalf("warp 203 -> map %d (ok %v) on attempt %d, want the lowest map 10",
				mapID, ok, i)
		}
	}
	if mapID, ok := twoMaps.RestartWarpMap(999999); ok {
		t.Fatalf("warp 999999 -> map %d (ok %v), want no answer", mapID, ok)
	}
	if mapID, ok := twoMaps.RestartWarpMap(0); ok {
		t.Fatalf("warp 0 -> map %d (ok %v), want no answer", mapID, ok)
	}
	if mapID, ok := twoMaps.RestartWarpMap(-5); ok {
		t.Fatalf("warp -5 -> map %d (ok %v), want no answer", mapID, ok)
	}
	var nilTable *ZonePoints
	if mapID, ok := nilTable.RestartWarpMap(213); ok {
		t.Fatalf("a nil table -> map %d (ok %v), want no answer", mapID, ok)
	}
}

// TestEntryKindNamesAndStoredValues pins the two values against the migration's own
// comment, so a renumbering cannot silently reorder the entry.
func TestEntryKindNamesAndStoredValues(t *testing.T) {
	if int(EntryNew) != entryKindValueNew || int(EntryContinue) != entryKindValueContinue {
		t.Fatalf("EntryKind values = %d/%d, want the stored %d/%d",
			int(EntryNew), int(EntryContinue),
			entryKindValueNew, entryKindValueContinue)
	}
	for _, k := range []EntryKind{EntryNew, EntryContinue} {
		if name := k.String(); name == "" || strings.Contains(name, "not one of") {
			t.Fatalf("EntryKind %d has no name: %q", int(k), name)
		}
	}
	if got := EntryKind(7).String(); !strings.Contains(got, "kind 7") {
		t.Fatalf("an unknown kind prints %q, want the number in it", got)
	}
	// The barrack's own writer must agree with those numbers.
	for _, c := range []struct {
		e    barrack.ZoneEntry
		kind int16
	}{
		{barrack.ZoneEntry{}, 0},
		{barrack.ZoneEntry{Continue: true}, 1},
	} {
		if got := c.e.Kind(); got != c.kind {
			t.Fatalf("barrack.ZoneEntry%+v.Kind() = %d, want %d", c.e, got, c.kind)
		}
	}
}
