package zone

import (
	"context"
	"os"
	"testing"
	"time"

	"geserver/db"
)

// THE LAST POINT IN THE DATABASE, one real write and one real read - plus the ONE
// property R70 exists for: the save point and the last point must not be able to
// overwrite each other.
//
// It is OFF BY DEFAULT because it writes, and it writes to commander 25 - the throwaway
// test account, NEVER 24. Every row it touches is READ FIRST and put back by a defer,
// whatever this test does:
//
//   - `commander.zone_id` and `commander.point` (one UPDATE);
//
//   - every `character_position` row of commander 25's characters: the rows are
//     DELETED and the saved copies re-inserted, because a row that did not exist
//     before this test must not be left behind either.
//
//     GE_LAST_POINT_DB_TEST=1 go test ./zone -run TestLastPointRowInPostgreSQL -v
func TestLastPointRowInPostgreSQL(t *testing.T) {
	if os.Getenv("GE_LAST_POINT_DB_TEST") == "" {
		t.Skip("set GE_LAST_POINT_DB_TEST=1 and start the database container to run the write test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("PostgreSQL is not reachable, so the last-point write was NOT "+
			"checked: %v", err)
	}
	defer pool.Close()
	// The schema the binary carries, applied the same way a start applies it. This is
	// what creates `character_position` and `pending_zone_entry.kind`.
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)

	const commander = 25 // the throwaway test commander, never 24

	// The commander's row, taken BEFORE anything is written.
	var (
		oldZone  int16
		oldPoint int
	)
	if err := pool.QueryRow(ctx,
		`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
		Scan(&oldZone, &oldPoint); err != nil {
		t.Fatalf("commander %d has no row to test against: %v", commander, err)
	}

	// The team, and every position row they have now.
	row, err := pool.Query(ctx,
		`SELECT character_id FROM character WHERE commander_id = $1 ORDER BY slot`,
		commander)
	if err != nil {
		t.Fatalf("read the characters of commander %d: %v", commander, err)
	}
	var ids []uint32
	for row.Next() {
		var id int32
		if err := row.Scan(&id); err != nil {
			row.Close()
			t.Fatalf("scan character id: %v", err)
		}
		ids = append(ids, uint32(id))
	}
	row.Close()
	if len(ids) == 0 {
		t.Fatalf("commander %d has no character row, so the position cannot be "+
			"checked - the test account is incomplete", commander)
	}

	// `z` is ZPOS, which is a world HEIGHT - never the navmesh cell (R73).
	// `updated` is carried because a row that is deleted and re-inserted with a
	// fresh timestamp is not the row that was there: the gate's checksum sees it.
	type positionRow struct {
		x, y, z, mapID int32
		updated        time.Time
	}
	savedRows := map[uint32]positionRow{}
	posRows, err := pool.Query(ctx,
		`SELECT cp.character_id, cp.xpos, cp.ypos, cp.zpos, cp.map_id, cp.updated
		   FROM character_position cp
		   JOIN character ch ON ch.character_id = cp.character_id
		  WHERE ch.commander_id = $1`, commander)
	if err != nil {
		t.Fatalf("read the position rows of commander %d: %v", commander, err)
	}
	for posRows.Next() {
		var id int32
		var p positionRow
		if err := posRows.Scan(&id, &p.x, &p.y, &p.z, &p.mapID, &p.updated); err != nil {
			posRows.Close()
			t.Fatalf("scan position row: %v", err)
		}
		savedRows[uint32(id)] = p
	}
	posRows.Close()
	t.Logf("BEFORE: commander %d zone_id = %d, point = %d, %d character(s), %d "+
		"character_position row(s)", commander, oldZone, oldPoint, len(ids),
		len(savedRows))

	// Put every row back. A failure here is worth reporting even while another failure
	// is already unwinding (t.Errorf, not Fatalf). A FRESH context: the test's own may
	// be cancelled by the time a fatal failure unwinds into this defer.
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(bg,
			`UPDATE commander SET zone_id = $2, point = $3 WHERE commander_id = $1`,
			commander, oldZone, oldPoint); err != nil {
			t.Errorf("restore commander %d zone_id=%d point=%d: %v", commander,
				oldZone, oldPoint, err)
		}
		if _, err := pool.Exec(bg,
			`DELETE FROM character_position cp USING character ch
			  WHERE cp.character_id = ch.character_id AND ch.commander_id = $1`,
			commander); err != nil {
			t.Errorf("restore: delete the position rows of commander %d: %v",
				commander, err)
			return
		}
		for id, p := range savedRows {
			if _, err := pool.Exec(bg,
				`INSERT INTO character_position
				        (character_id, xpos, ypos, zpos, map_id, updated)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				int32(id), p.x, p.y, p.z, p.mapID, p.updated); err != nil {
				t.Errorf("restore: position row of character %d: %v", id, err)
			}
		}
	}()

	// readCommander is the one reader of the two columns, so every assertion below asks
	// the same SQL and a later edit to one of them cannot pass by accident.
	readCommander := func(t *testing.T) (int16, int) {
		t.Helper()
		var zone int16
		var point int
		if err := pool.QueryRow(ctx,
			`SELECT zone_id, point FROM commander WHERE commander_id = $1`, commander).
			Scan(&zone, &point); err != nil {
			t.Fatalf("read commander %d back: %v", commander, err)
		}
		return zone, point
	}
	// reading the team's position rows, map of character id -> row.
	readPositions := func(t *testing.T) map[uint32]positionRow {
		t.Helper()
		out := map[uint32]positionRow{}
		rows, err := pool.Query(ctx,
			`SELECT cp.character_id, cp.xpos, cp.ypos, cp.zpos, cp.map_id
			   FROM character_position cp
			   JOIN character ch ON ch.character_id = cp.character_id
			  WHERE ch.commander_id = $1`, commander)
		if err != nil {
			t.Fatalf("read the position rows of commander %d back: %v", commander, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int32
			var p positionRow
			if err := rows.Scan(&id, &p.x, &p.y, &p.z, &p.mapID); err != nil {
				t.Fatalf("scan position row: %v", err)
			}
			out[uint32(id)] = p
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("position rows: %v", err)
		}
		return out
	}

	// A DELIBERATE PRE-STATE, so each check below proves the write MOVED its own column
	// and is not reading back a value that was already there.
	if _, err := pool.Exec(ctx,
		`UPDATE commander SET zone_id = 10, point = 7 WHERE commander_id = $1`,
		commander); err != nil {
		t.Fatalf("set the pre-state of commander %d: %v", commander, err)
	}

	// STEP 1 - A SAVE-POINT WRITE MUST LEAVE `zone_id` ALONE, and must not write a
	// position either. This is the owner's bug, inverted: the save used to land in the
	// column the log-out owns.
	if _, err := store.SaveStartPoint(ctx, commander, 213); err != nil {
		t.Fatalf("SaveStartPoint: %v", err)
	}
	zone, point := readCommander(t)
	t.Logf("after the SAVE POINT: commander.zone_id = %d, commander.point = %d", zone, point)
	if point != 213 {
		t.Fatalf("commander.point = %d after a save, want 213", point)
	}
	if zone != 10 {
		t.Fatalf("commander.zone_id = %d after a save, want the untouched 10 - a save "+
			"must never move the last point's map", zone)
	}

	// STEP 2 - A LAST-POINT WRITE MUST LEAVE `point` ALONE and must write one
	// `character_position` row per character of the team.
	want := LastPoint{MapID: 15, Pos: [3]int32{2268, 16931, 233}, Name: "coim_basic"}
	if err := store.SaveLastPoint(ctx, commander, ids, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	zone, point = readCommander(t)
	t.Logf("after the LAST POINT: commander.zone_id = %d, commander.point = %d", zone, point)
	if zone != 15 {
		t.Fatalf("commander.zone_id = %d after a last point on map 15, want 15", zone)
	}
	if point != 213 {
		t.Fatalf("commander.point = %d after a log-out, want it STILL 213 - the last "+
			"point overwrote the save point", point)
	}

	got := readPositions(t)
	if len(got) != len(ids) {
		t.Fatalf("%d character_position row(s) for %d character(s), want one each",
			len(got), len(ids))
	}
	for _, id := range ids {
		p, ok := got[id]
		if !ok {
			t.Fatalf("character %d has no character_position row", id)
		}
		t.Logf("character_position row: character %d -> xpos=%d ypos=%d zpos=%d map_id=%d",
			id, p.x, p.y, p.z, p.mapID)
		if p.x != want.Pos[0] || p.y != want.Pos[1] || p.z != want.Pos[2] ||
			p.mapID != int32(want.MapID) {
			t.Fatalf("character %d row = (%d, %d, %d, map %d), want (%d, %d, %d, map %d)",
				id, p.x, p.y, p.z, p.mapID, want.Pos[0], want.Pos[1], want.Pos[2],
				want.MapID)
		}
	}

	// A z that cannot be a world height writes NEITHER row: the map must not move to 15
	// while the position stays on the old map. 32767 is the number the oracle itself
	// leaves in ZPOS when it has no position at all (character 1790 holds it three
	// times), so it is the one value a real height can never be.
	if err := store.SaveLastPoint(ctx, commander, ids,
		LastPoint{MapID: 10, Pos: [3]int32{1, 2, HeightUnset}}); err == nil {
		t.Fatalf("z = %d (the oracle's no-position marker) was accepted, want a refusal",
			HeightUnset)
	}
	if zone, point := readCommander(t); zone != 15 || point != 213 {
		t.Fatalf("after a refused save: zone_id %d, point %d - want 15 and 213 (the "+
			"refused write must not have touched either row)", zone, point)
	}

	// A team whose ids are none of this commander's characters writes NOTHING and says
	// so: that is the guard against landing one player's position on another's row.
	if err := store.SaveLastPoint(ctx, commander, []uint32{999999},
		LastPoint{MapID: 10, Pos: [3]int32{1, 2, 3}}); err == nil {
		t.Fatalf("a save for a character of no commander was accepted")
	}
	if zone, point := readCommander(t); zone != 15 || point != 213 {
		t.Fatalf("after a refused save: zone_id %d, point %d - want 15 and 213", zone, point)
	}
}
