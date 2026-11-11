package zone

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"geserver/barrack"
	"geserver/db"
)

// THE SAVED START POINT, tested with NO CLIENT and NO DATABASE: the entry side (which
// point the team is put on) and the Save button's write.
//
// THE STORAGE IS ONE COLUMN NOW: `commander.point`, holding a RESTART WARP ID
// (db\migrations\00010_point_and_position.sql). The fake below records only
// (commander, warp id), which is the structural proof that a save has nowhere to put a
// map, a position or `zone_id` - there is no such argument.
//
// The database itself is checked separately, against the real PostgreSQL, in
// TestStartPointDBRoundTrip below - it is off by default because it WRITES.

// startPointFake is the store the entry tests wire in.
type startPointFake struct {
	sp    StartPoint
	found bool
	load  error
	// warps records what a Save click wrote, so the test can assert the row after the
	// fact instead of trusting the handler's return value.
	warps []int
	save  error
}

func (f *startPointFake) loadOf(_ context.Context, _ int32) (StartPoint, bool, error) {
	if f.load != nil {
		return StartPoint{}, false, f.load
	}
	return f.sp, f.found, nil
}

func (f *startPointFake) saveOf(_ context.Context, _ int32, warpID int) (StartPoint, error) {
	if f.save != nil {
		return StartPoint{}, f.save
	}
	f.warps = append(f.warps, warpID)
	return StartPoint{WarpID: warpID}, nil
}

// startPointHandler is a zone serving Coimbra (map 15) with the REAL spawn table
// loaded, so every number asserted below is the table's own.
func startPointHandler(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	points := zoneTable(t) // skips when the emitted file is absent
	entry := points.Maps["15"]
	logs := &bytes.Buffer{}
	h := &Handler{
		Log:        log.New(logs, "", 0),
		MapID:      15,
		MapName:    "coim_basic",
		ZonePoints: points,
	}
	if entry.Spawn == nil || entry.Spawn.Cell == nil {
		t.Fatalf("map 15 has no spawn with a cell in the spawn table")
	}
	h.Spawn = [3]int32{entry.Spawn.X, entry.Spawn.Y, *entry.Spawn.Cell}
	h.SpawnFormation = entry.Spawn.Formation
	return h, logs
}

// TestSavedStartPointMovesTheTeam is the requirement itself: a commander whose saved
// warp is Coimbra's enters on that town's restart row, and not at the map's own spawn.
// Both points are real rows of data\zone_points.json - warp 213 on map 15 is
// (2268, 16931, cell 233) and the map's spawn is different, so a bug that ignored the
// save cannot pass this by accident.
//
// THE WARP IS THE ONLY STORED HALF, so this also pins the reverse lookup: warp 213 must
// resolve to map 15 through ZonePoints.RestartWarpMap, or nothing else here can work.
func TestSavedStartPointMovesTheTeam(t *testing.T) {
	h, logs := startPointHandler(t)
	f := &startPointFake{sp: StartPoint{WarpID: 213}, found: true}
	h.UseStartPoint(f.loadOf, f.saveOf)

	sess := &Session{AccountID: 36, CommanderID: 25}
	base, slots, placed := h.savedStartPoint(context.Background(), sess, h.Spawn,
		h.SpawnFormation)

	want := [3]int32{2268, 16931, 233}
	if base != want {
		t.Fatalf("entry point = %v, want the saved town's restart warp row %v; the "+
			"map's own spawn is %v", base, want, h.Spawn)
	}
	if base == h.Spawn {
		t.Fatalf("the team still enters at the map's own spawn %v - the save did nothing",
			h.Spawn)
	}
	if !strings.Contains(placed, "saved start point") {
		t.Fatalf("placed = %q, want it to name the saved start point", placed)
	}
	if !strings.Contains(logs.String(), "SAVED START POINT of commander 25") {
		t.Fatalf("the log does not name the saved start point; it says:\n%s", logs)
	}

	// The followers move with the leader: a formation left at the map's spawn would
	// stack the team across the map from its leader.
	if len(h.SpawnFormation) > 0 {
		if len(slots) != len(h.SpawnFormation) {
			t.Fatalf("%d formation slot(s), want %d", len(slots), len(h.SpawnFormation))
		}
		for i, s := range slots {
			if s.X != want[0]+(h.SpawnFormation[i].X-h.Spawn[0]) ||
				s.Y != want[1]+(h.SpawnFormation[i].Y-h.Spawn[1]) {
				t.Fatalf("slot %d = (%d, %d), want the leader's move applied", i, s.X, s.Y)
			}
			if s.Cell == nil || *s.Cell != want[2] {
				t.Fatalf("slot %d has cell %v, want %d", i, s.Cell, want[2])
			}
		}
	}
}

// TestSavedStartPointKeepsTheSpawnWhenItCannotBeUsed pins every refusal: in each one
// the team must still enter at the map's own spawn, and the log must say WHY. A silent
// fallback here is how "the save button does nothing" survives a fix.
//
// THE TWO "WRONG" CASES ARE DIFFERENT PROBLEMS and are worded differently on purpose:
// a warp that belongs to ANOTHER map is a save made somewhere else (not an error), and
// a warp that belongs to NO map is a data error. Collapsing them would hide which one
// happened.
func TestSavedStartPointKeepsTheSpawnWhenItCannotBeUsed(t *testing.T) {
	cases := []struct {
		name string
		wire func(h *Handler, f *startPointFake)
		want string
	}{
		{
			name: "nothing is saved yet",
			wire: func(h *Handler, f *startPointFake) { h.UseStartPoint(f.loadOf, f.saveOf) },
			want: "has no saved start point yet",
		},
		{
			name: "the saved warp belongs to another map",
			wire: func(h *Handler, f *startPointFake) {
				// Warp 203 is Reboldoeux's restart row and lives on map 10.
				f.sp, f.found = StartPoint{WarpID: 203}, true
				h.UseStartPoint(f.loadOf, f.saveOf)
			},
			want: "is map 10 (warp 203) and this zone serves map 15",
		},
		{
			name: "no map carries the saved warp id (a DATA ERROR)",
			wire: func(h *Handler, f *startPointFake) {
				f.sp, f.found = StartPoint{WarpID: 999999}, true
				h.UseStartPoint(f.loadOf, f.saveOf)
			},
			want: "NO map's restart row in the spawn table carries that warp id",
		},
		{
			name: "the store cannot be read",
			wire: func(h *Handler, f *startPointFake) {
				f.load = errors.New("connection refused")
				h.UseStartPoint(f.loadOf, f.saveOf)
			},
			want: "could not be read (connection refused)",
		},
		{
			name: "no store is wired in at all",
			wire: func(h *Handler, f *startPointFake) {},
			want: "no start-point store is wired in",
		},
		{
			name: "no spawn table is loaded, so the warp names no map",
			wire: func(h *Handler, f *startPointFake) {
				f.sp, f.found = StartPoint{WarpID: 213}, true
				h.ZonePoints = nil
				h.UseStartPoint(f.loadOf, f.saveOf)
			},
			want: "no spawn table is loaded",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, logs := startPointHandler(t)
			f := &startPointFake{}
			c.wire(h, f)

			sess := &Session{AccountID: 36, CommanderID: 25}
			base, _, placed := h.savedStartPoint(context.Background(), sess, h.Spawn,
				h.SpawnFormation)
			if base != h.Spawn {
				t.Fatalf("entry point = %v, want the map's own spawn %v - this "+
					"refusal must not move the team", base, h.Spawn)
			}
			if placed != "" {
				t.Fatalf("placed = %q, want empty for a refusal", placed)
			}
			if !strings.Contains(logs.String(), c.want) {
				t.Fatalf("the log does not say why; want %q in:\n%s", c.want, logs)
			}
		})
	}
}

// TestSaveRestartWarpWritesTheWarpOnly is the WRITE half, and it is where the owner's
// bug lived: a save must write the restart warp id and NOTHING else.
//
// The fake records the single value a save is even allowed to carry - (commander, warp
// id) - so there is no argument in which a map, a position or `zone_id` could travel.
// The real columns are asserted against PostgreSQL in TestStartPointDBRoundTrip.
func TestSaveRestartWarpWritesTheWarpOnly(t *testing.T) {
	h, logs := startPointHandler(t) // map 15 = coim_basic
	f := &startPointFake{}
	h.UseStartPoint(f.loadOf, f.saveOf)

	sess := &Session{AccountID: 36, CommanderID: 25}
	out, err := h.Reply(context.Background(), sess, saveRestartWarpFrame())
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s) sent, want exactly one (ZC_UPDATE_RESTART_WARP)", len(out))
	}
	if len(f.warps) != 1 {
		t.Fatalf("%d save(s) written, want 1: %v", len(f.warps), f.warps)
	}
	if f.warps[0] != 213 {
		t.Fatalf("the save wrote warp %d, want 213 (Coimbra's restart warp)",
			f.warps[0])
	}
	// ONE line naming the ONE column, and naming what it deliberately did not touch.
	for _, want := range []string{"START POINT SAVED", "WROTE commander.point = 213",
		"commander.zone_id and character_position", "NOT touched"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the save line does not name %q; it says:\n%s", want, logs)
		}
	}
}

// TestStartPointDBRoundTrip is the END-TO-END proof for R70, and it needs no game
// client. It does the two things the owner did, in the order that broke:
//
//  1. SAVE as start point, standing in Coimbra (map 15), through the real 3468;
//  2. LOG OUT in Cite de Reboldoeux (map 10), which is the Last Point writer.
//
// Before this fix both steps wrote `commander.zone_id`, so step 2 destroyed step 1. Now
// step 1 writes `commander.point` and step 2 writes `zone_id` + `character_position`,
// and this test reads BOTH back and quotes them.
//
// It is off by default because it WRITES. Commander 25 is the throwaway test commander
// (24 belongs to the owner and is never named here). Every row it touches is read first
// and put back afterward, so a run leaves no trace - including a `character_position`
// row that did not exist before, which is DELETED rather than blanked.
//
//	GE_LAST_POINT_DB_TEST=1 go test ./zone -run TestStartPointDBRoundTrip -v
func TestStartPointDBRoundTrip(t *testing.T) {
	if os.Getenv("GE_LAST_POINT_DB_TEST") == "" {
		t.Skip("set GE_LAST_POINT_DB_TEST=1 and start the database container to run " +
			"the write test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("PostgreSQL is not reachable, so the save was NOT checked: %v", err)
	}
	defer pool.Close()
	// The schema the binary carries, applied the same way a start applies it.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)

	const commander = 25 // the throwaway test commander, never 24

	// THE STATE BEFORE, every row this test touches.
	var (
		beforeZone  int16
		beforePoint int
		leader      int32
	)
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
		Scan(&beforeZone, &beforePoint); err != nil {
		t.Fatalf("read commander %d: %v", commander, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT character_id FROM character
		  WHERE commander_id = $1 AND iscard = 0 ORDER BY slot LIMIT 1`, commander).
		Scan(&leader); err != nil {
		t.Fatalf("commander %d has no living character row: %v", commander, err)
	}
	var (
		beforeX, beforeY, beforeCell, beforeMap int32
		beforeUpdated                          time.Time
		hadPosition                            bool
	)
	err = pool.QueryRow(ctx,
		`SELECT xpos, ypos, zpos, map_id, updated FROM character_position
		  WHERE character_id = $1`, leader).
		Scan(&beforeX, &beforeY, &beforeCell, &beforeMap, &beforeUpdated)
	switch {
	case err == nil:
		hadPosition = true
	case errors.Is(err, pgx.ErrNoRows):
		// The normal state before any last point was written through the new table.
	default:
		t.Fatalf("read the position of character %d: %v", leader, err)
	}
	t.Logf("BEFORE: commander %d zone_id = %d, point = %d; character %d position row "+
		"present = %v", commander, beforeZone, beforePoint, leader, hadPosition)

	// A FRESH context in the revert: the test's own may be cancelled by the time a
	// fatal failure unwinds into this defer. `updated` is restored too - a row with
	// a fresh timestamp is not the row that was there.
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(bg,
			`UPDATE commander SET zone_id = $2, point = $3 WHERE commander_id = $1`,
			commander, beforeZone, beforePoint); err != nil {
			t.Errorf("revert: commander %d zone_id/point: %v", commander, err)
		}
		if hadPosition {
			if _, err := pool.Exec(bg,
				`UPDATE character_position
				    SET xpos = $2, ypos = $3, zpos = $4, map_id = $5, updated = $6
				  WHERE character_id = $1`,
				leader, beforeX, beforeY, beforeCell, beforeMap,
				beforeUpdated); err != nil {
				t.Errorf("revert: position of character %d: %v", leader, err)
			}
		} else if _, err := pool.Exec(bg,
			`DELETE FROM character_position WHERE character_id = $1`, leader); err != nil {
			t.Errorf("revert: delete the position row of character %d: %v", leader, err)
		}
	}()

	// STEP 1 - THE SAVE BUTTON, through the real 3468 and the real store.
	h := &Handler{
		Log:        log.New(io.Discard, "", 0),
		MapID:      15,
		MapName:    "coim_basic",
		ZonePoints: zoneTable(t), // skips when the emitted file is absent
	}
	h.UseStartPoint(store.StartPointOf, store.SaveStartPoint)
	sess := &Session{AccountID: 36, CommanderID: commander}
	sent, err := h.Reply(ctx, sess, saveRestartWarpFrame())
	if err != nil {
		t.Fatalf("the Save button: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("the Save button sent %d packet(s), want one 3467", len(sent))
	}
	if warp, ok := ParseUpdateRestartWarp(sent[0]); !ok || warp != 213 {
		t.Fatalf("the answer carries warp %d (ok %v), want 213", warp, ok)
	}

	var (
		afterSaveZone  int16
		afterSavePoint int
	)
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
		Scan(&afterSaveZone, &afterSavePoint); err != nil {
		t.Fatalf("read commander %d after the save: %v", commander, err)
	}
	t.Logf("AFTER SAVE on map 15: commander.point = %d, commander.zone_id = %d",
		afterSavePoint, afterSaveZone)

	if afterSavePoint != 213 {
		t.Fatalf("commander.point = %d after the save, want 213 (Coimbra's restart "+
			"warp) - the save did not write the column", afterSavePoint)
	}
	if afterSaveZone != beforeZone {
		t.Fatalf("commander.zone_id = %d after the save, want it UNCHANGED at %d: that "+
			"column belongs to the Last Point, and the save writing it is the bug this "+
			"task fixes", afterSaveZone, beforeZone)
	}

	// STEP 2 - THE LAST POINT, on map 10 (Cite de Reboldoeux): the write a log-out
	// makes. It MUST NOT touch the saved start point.
	leaderPos := [3]int32{3946, 193, 146}
	if err := store.SaveLastPoint(ctx, commander, []uint32{uint32(leader)}, LastPoint{
		MapID: 10, Pos: leaderPos, Name: "Tester"}); err != nil {
		t.Fatalf("SaveLastPoint: %v", err)
	}
	var (
		lastZone  int16
		lastPoint int
		x, y      int32
		cell      int32
		mapped    int32
	)
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
		Scan(&lastZone, &lastPoint); err != nil {
		t.Fatalf("read commander %d after the last point: %v", commander, err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT xpos, ypos, zpos, map_id FROM character_position
		  WHERE character_id = $1`, leader).Scan(&x, &y, &cell, &mapped); err != nil {
		t.Fatalf("read the position of character %d after the last point: %v", leader, err)
	}
	t.Logf("AFTER LAST POINT on map 10: commander.zone_id = %d, commander.point = %d, "+
		"character %d position (xpos %d, ypos %d, zpos %d, map_id %d)", lastZone,
		lastPoint, leader, x, y, cell, mapped)

	if lastZone != 10 {
		t.Fatalf("commander.zone_id = %d after the Last Point on map 10, want 10", lastZone)
	}
	if lastPoint != 213 {
		t.Fatalf("commander.point = %d after a log-out on map 10, want it STILL 213 - "+
			"the Last Point overwrote the save again", lastPoint)
	}
	if x != leaderPos[0] || y != leaderPos[1] || cell != leaderPos[2] || mapped != 10 {
		t.Fatalf("character_position = (%d, %d, %d, map %d), want (%d, %d, %d, map 10)",
			x, y, cell, mapped, leaderPos[0], leaderPos[1], leaderPos[2])
	}

	// THE ENTRY SIDE reads the save back.
	sp, found, err := store.StartPointOf(ctx, commander)
	if err != nil || !found {
		t.Fatalf("StartPointOf after both writes: %+v, found %v, err %v", sp, found, err)
	}
	if sp.WarpID != 213 {
		t.Fatalf("StartPointOf = %+v, want warp 213", sp)
	}

	// THE BARRACK: newMapID is the SAVE POINT's map (15, resolved from warp 213) and
	// contMapID is the LAST POINT's map (10). The first is the caption the owner reads
	// on New Game, so it now says The Port of Coimbra instead of Cite de Reboldoeux.
	bstore := barrack.NewPGBarrackStore(pool)
	row, ok, err := bstore.Commander(ctx, commander)
	if err != nil {
		t.Fatalf("the barrack's own query failed: %v", err)
	}
	if !ok {
		t.Fatalf("the barrack's own query found no commander %d", commander)
	}
	saveMap, resolved := zoneTable(t).RestartWarpMap(int(row.SaveWarpID))
	t.Logf("BARRACK sees: name %q, contMapID (last) %d, save warp %d -> newMapID %d "+
		"(resolved %v), channel %d", row.Name, row.ZoneID, row.SaveWarpID, saveMap,
		resolved, row.ChannelID)
	if row.ZoneID != 10 {
		t.Fatalf("the barrack answers contMapID %d, want the Last Point's map 10",
			row.ZoneID)
	}
	if !resolved || saveMap != 15 {
		t.Fatalf("warp %d resolves to map %d (ok %v), want the SAVE POINT's map 15",
			row.SaveWarpID, saveMap, resolved)
	}

	// A save that would name no warp is refused and writes NOTHING.
	if _, err := store.SaveStartPoint(ctx, commander, 0); err == nil {
		t.Fatalf("SaveStartPoint(warp 0) was accepted")
	}
	var refusedZone int16
	var refusedPoint int
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
		Scan(&refusedZone, &refusedPoint); err != nil {
		t.Fatalf("read commander %d after a refused save: %v", commander, err)
	}
	if refusedPoint != 213 {
		t.Fatalf("commander.point = %d after a refused save, want 213", refusedPoint)
	}

	// A commander that does not exist is an ERROR, not a silent no-op: the handler must
	// be able to tell the player nothing was written.
	if _, err := store.SaveStartPoint(ctx, 999999, 213); err == nil {
		t.Fatalf("a save for a commander that does not exist was accepted")
	}
}
