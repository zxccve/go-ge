package zone

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// THE LAST POINT IN POSTGRESQL: the map the barrack shows and the position the team is
// put back on.
//
// THE ORACLE'S OWN SHAPE (database GE_DATA, a v32 schema), copied rather than invented:
//
//  1. `commander.zone_id` is the ZONE TO RESUME - the LAST POINT's map. The barrack
//     reads it for BC_COMMANDER_INFO's contMapID, and the client turns contMapID into
//     the "이전 시작" (previous start) caption of the Continue button
//     (source2\geClient\Source\GEBarrackNet.cpp:234-238, tooltip 81171 "start the game
//     at the last EXIT point"). So writing this column IS the display half of THIS
//     feature - there is no packet to add.
//
//     THE SAVE POINT'S MAP DOES NOT LIVE HERE. It is a RESTART WARP ID in
//     `commander.point` now (zone\start_point_db.go, db\migrations\
//     00010_point_and_position.sql), because this column is written on every way out of
//     a zone and a save click writing it too is how the owner's Coimbra save was
//     replaced by a Reboldoeux log-out on 2026-10-29. Nothing in this file reads or
//     writes `commander.point`.
//
//  2. `character_position(xpos, ypos, zpos, map_id, updated)` is the POSITION, one row
//     per character, with the oracle's EXACT column names. `xpos`/`ypos` are the world
//     x/y; `zpos` is the WORLD HEIGHT of that spot, and it is NEVER the navmesh cell.
//     The original writes it as `m_world->GetHeight(pos)` (Commander_FSM.cpp:644-650,
//     `WORLDPOINT point; point.Set(pos.x, pos.y, height)`) and reads it back through
//     `Get3DPointToPosLocaly(point, leaderPos, 1000, 300)` (Commander_Load.cpp:575-580).
//     R73's second bug was this column holding the CELL: the cell reads as a height of a
//     few hundred units, the client then gets it as a triangle index, and the character
//     stands under the floor.
//
//     `map_id` is stored per row as well, because that is the oracle's shape. It is the
//     same map the commander's `zone_id` carries at the moment of the write; the read
//     below prefers it (COALESCE(cp.map_id, c.zone_id)) so a row that outlives a
//     commander edit still says which mesh its point is on.
//
// WHY A ROW PER CHARACTER AND NOT JUST THE LEADER. The position belongs to a character,
// and this zone tracks ONE live position per session: the leader's walk (Session.Pos,
// gate.go walkedPos). The followers are re-formed around the leader on entry by
// arrivalFormation, so writing a row per character means every member gets the oracle's
// shape at the spot the team actually stood - without inventing per-character positions
// this side never had. Only the LEADER's row is ever read back, through
// `pending_zone_entry.slot0` (barrack\team_handoff.go ParseStartGame, slot[0]).
//
// WHY NOT `commander_property`. The saved START POINT used to live there
// (`RestartWarpID`) and that was a WARP ROW - a named point on a map. The Last Point is
// a POSITION the player happened to stand on, and the two are different facts: storing a
// position as a property string would make a number read as an id. That property row is
// gone now; `commander.point` holds the warp id and this table holds the position.

// LastPointOf reads a commander's last point, and it deliberately answers TWO
// questions at once: where the leader stood, and whether the player actually asked to
// continue from it.
//
// found is false in three ordinary cases, and none of them is an error:
//
//   - there is no `pending_zone_entry` row (pgx.ErrNoRows): the player never picked a
//     team in the barrack at all, so nothing names a leader - and no "Last Point" was
//     pressed;
//   - `kind` is not 1 (continue): the player pressed NEW GAME (CB_STARTGAME 22, kind 0),
//     and only BAR_CON / CB_CONTINUEGAME 23 (kind 1) means "put me back where I left";
//   - there is no `character_position` row for the leader (the LEFT JOIN's
//     `cp.character_id IS NOT NULL` is false): the position was never written, which is
//     what a commander from before this table existed looks like;
//   - the slot names no living character (`iscard = 0`): the leader was carded away
//     between the barrack and the zone. (That one shows up as ErrNoRows, i.e. the first
//     case, because the JOIN drops the row.)
//
// `kind` is asked instead of `is_continue`, and the migration keeps the two in step, so
// an old row that only has `is_continue` is still readable: the SQL falls back to it.
func (s *PGStore) LastPointOf(ctx context.Context, commanderID int32) (LastPoint, bool, error) {
	if commanderID <= 0 {
		return LastPoint{}, false, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	var (
		mp     int
		kind   int
		hasPos bool
		lp     LastPoint
	)
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(cp.map_id, c.zone_id),
		        COALESCE(kind, CASE WHEN pe.is_continue THEN 1 ELSE 0 END),
		        ch.name,
		        COALESCE(cp.xpos, 0), COALESCE(cp.ypos, 0), COALESCE(cp.zpos, 0),
		        cp.character_id IS NOT NULL
		   FROM commander c
		   JOIN pending_zone_entry pe ON pe.commander_id = c.commander_id
		   JOIN character ch ON ch.commander_id = c.commander_id
		                    AND ch.slot = pe.slot0
		                    AND ch.iscard = 0
		   LEFT JOIN character_position cp ON cp.character_id = ch.character_id
		  WHERE c.commander_id = $1`, commanderID).
		Scan(&mp, &kind, &lp.Name, &lp.Pos[0], &lp.Pos[1], &lp.Pos[2], &hasPos)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No pending entry means the player did not press Last Point
			// (BAR_CON / CB_CONTINUEGAME 23). That is NOT an error - it is the normal
			// Save Point login, and the caller must then use the saved start point.
			return LastPoint{}, false, nil
		}
		return LastPoint{}, false, fmt.Errorf("read the last point of commander %d: %w",
			commanderID, err)
	}
	if kind != entryKindValueContinue {
		// NEW GAME (kind 0) or MISSION (kind 2): the row above exists because the
		// player picked a team, not because they asked to be put back. Entering at the
		// stored position here would make every new game start wherever the last
		// session logged out, which is exactly the behaviour Last Point is meant to
		// gate.
		return LastPoint{}, false, nil
	}
	if !hasPos {
		// A continue with no position on file: there is nothing to put the team back
		// on, and (0, 0, 0) is off every map rather than a spot.
		return LastPoint{}, false, nil
	}
	lp.MapID = mp
	return lp, true, nil
}

// SaveLastPoint writes where the leader left, and it is the ONE write of this feature.
//
// ONE transaction, because the two rows must never disagree: a map on the commander
// with a position from another map is worse than nothing - the barrack would show a map
// the stored cell does not belong to, and the entry would then be refused as "the saved
// cell means nothing here" with no way for a player to fix it.
//
// IT NEVER WRITES `commander.point` AND NEVER WRITES `character.pos_x/pos_y/pos_z`, and
// both are on purpose. `commander.point` is the SAVED START POINT (zone\start_point_db.go)
// - the town the player chose with a Save click - and a log-out writing it is the bug
// this pair of files exists to fix. `character.pos_*` is the BARRACK's copy of a
// character's spot (db\migrations\00002_barrack.sql), which the barrack scene draws; the
// last point is a different fact ("where this session ended"), so it has its own rows
// and leaves the barrack's column alone.
//
// The `commander_id` in the second statement's WHERE is not decoration: it makes it
// impossible to write a position onto another commander's character, even if a caller
// ever passed a handle from the wrong team. `ch.slot` is not asked for, because a
// session's team is what it spawned, and every member of it stood where the leader did.
//
// A commander that does not exist, and a set of ids that matches none of that
// commander's characters, are both ERRORS and not silent zeroes: this runs on the way out
// of a session, and "the save worked" for a row that was never written is exactly the lie
// this project refuses (see SaveStartPoint's own note).
func (s *PGStore) SaveLastPoint(ctx context.Context, commanderID int32, characterIDs []uint32,
	lp LastPoint) error {
	if commanderID <= 0 {
		return fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if len(characterIDs) == 0 {
		return fmt.Errorf("commander %d has no character ids, so there is nobody whose "+
			"position could be written", commanderID)
	}
	if lp.MapID <= 0 {
		return fmt.Errorf("map id %d is not a map", lp.MapID)
	}
	// A height, not a cell: the caller resolved it from the map's mesh (Handler.NavHeight).
	// This guard only rejects what a height can never be - see PlausibleHeight for why a
	// cell cannot be told apart by its range and must be caught by the mesh at entry.
	if !PlausibleHeight(lp.Pos[2]) {
		return fmt.Errorf("z %d is not a world height (the oracle's ZPOS holds a height, "+
			"never a navmesh cell) - the last point was NOT written", lp.Pos[2])
	}

	// pgx encodes an int32 slice for `int[]` directly; the wire handles are uint32.
	ids := make([]int32, 0, len(characterIDs))
	for _, id := range characterIDs {
		if id == 0 {
			continue // a slot that never got a handle names no row
		}
		ids = append(ids, int32(id))
	}
	if len(ids) == 0 {
		return fmt.Errorf("commander %d has no usable character id, so there is nobody "+
			"whose position could be written", commanderID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the last point of commander %d: %w", commanderID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE commander SET zone_id = $2 WHERE commander_id = $1`,
		commanderID, int16(lp.MapID))
	if err != nil {
		return fmt.Errorf("write zone_id of commander %d: %w", commanderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("commander %d does not exist, so its last point was NOT "+
			"written", commanderID)
	}

	tag, err = tx.Exec(ctx,
		`INSERT INTO character_position (character_id, xpos, ypos, zpos, map_id)
		 SELECT ch.character_id, $3, $4, $5, $2
		   FROM character ch
		  WHERE ch.commander_id = $1 AND ch.character_id = ANY($6::int[])
		 ON CONFLICT (character_id) DO UPDATE
		        SET xpos = EXCLUDED.xpos, ypos = EXCLUDED.ypos, zpos = EXCLUDED.zpos,
		            map_id = EXCLUDED.map_id, updated = now()`,
		commanderID, lp.MapID, lp.Pos[0], lp.Pos[1], lp.Pos[2], ids)
	if err != nil {
		return fmt.Errorf("write the position of commander %d: %w", commanderID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no character of commander %d was written, so the last point "+
			"was NOT saved", commanderID)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the last point of commander %d: %w", commanderID, err)
	}
	return nil
}

// SaveWarpIDOf reads a commander's SAVED START POINT: `commander.point`, which is a WARP ID
// (203 Reboldoeux, 213 Coimbra - the numbers the barrack shows as the character's save point
// and the client's 3467 answer carries).
//
// found is false when the commander has no row or `point` is 0 - the ordinary "never pressed
// Save" state, not an error. The "To the Save Point" button asks exactly this question
// (zone\sysmenu.go), because the reference's TransferRandomLobbyZone reads RestartWarpID
// (Commander.cpp:4022-4050) and nothing else.
func (s *PGStore) SaveWarpIDOf(ctx context.Context, commanderID int32) (int, bool, error) {
	if commanderID <= 0 {
		return 0, false, nil
	}
	var warp *int32
	err := s.pool.QueryRow(ctx, `SELECT NULLIF(point, 0) FROM commander WHERE commander_id = $1`,
		commanderID).Scan(&warp)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read commander %d save point: %w", commanderID, err)
	}
	if warp == nil || *warp <= 0 {
		return 0, false, nil
	}
	return int(*warp), true, nil
}
