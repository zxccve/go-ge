package zone

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// THE SAVED START POINT: the town a team re-enters the world at after the barrack.
//
// IT IS ONE COLUMN, `commander.point`, AND IT HOLDS A RESTART WARP ID. That is the
// oracle's own shape (database GE_DATA, COMMANDER.POINT, a v32 schema) and the
// original server's: it stored `m_restartWarpID` and resolved the zone and the
// landing spot from that warp row every time -
//
//	source2\geServer\BarrackServer\Commander.cpp:378-397
//	  void CCommander::GetNewGameZoneInfo(ZONE_ID& zoneID, CHANNEL_ID& channelID, float* startPos)
//	  {
//	      geWarpTable::WARP_PROPERTY* warpProperty = geWarpTable::Get(m_restartWarpID);
//	      ...
//	      startPos[0] = warpProperty->targetX;
//	      ...
//	  }
//
// WHY THIS FILE CHANGED SHAPE (R70, 2026-10-29). The save point used to live in
// `commander.zone_id` (map) plus the `RestartWarpID` commander property (warp), and
// `commander.zone_id` is ALSO the LAST POINT - written on every log-out, map change
// and disconnect (zone\last_point_db.go). One column, two meanings, so the log-out
// writer destroyed the owner's Coimbra save:
//
//	20:15:17  LAST POINT saved for commander 25 ...: map 10 - wrote commander.zone_id
//	20:16:21  the saved start point of commander 25 names warp 213 on map 10 and it
//	          cannot be resolved (map 10's restart row is warp 203, not warp 213)
//
// The repair is in db\migrations\00010_point_and_position.sql. A save writes `point`
// and NEVER `zone_id`; the last-point writer writes `zone_id` and `character_position`
// and NEVER `point`. Two facts, two columns, so they cannot destroy each other.
//
// THE MAP IS NOT STORED, ON PURPOSE. There is no global warp table in this project -
// each map's `restart` row in data\zone_points.json names its own warp id - so the
// map is resolved FROM the warp id by ZonePoints.RestartWarpMap when it is needed. A
// stored second half would be a second source of truth for the same fact, and a wrong
// pair is a team in the wrong town.
//
// AND THERE IS STILL NO POSITION COLUMN HERE. The landing spot IS the warp row: this
// server's copy of it is the destination map's `restart` entry, whose X/Y/CELL were
// resolved against the map's navmesh when the file was emitted (warp 213 on map 15 ->
// (2268, 16931, cell 233)). A stored POSITION would be a second truth for the same
// fact, and the one thing this project must never write is a cell that is not on the
// destination mesh (see zone\spawn.go, badCellError - that crash is inside
// PathEngine.DLL). The Last Point is the opposite case and rightly does store a
// position: there the player's own spot IS the fact.

// StartPoint is where a commander re-enters the world: ONE restart warp id.
//
// The map is resolved from the warp id, not stored beside it - see the file comment.
// A warp id that no map's restart row carries is REFUSED at entry with a log line,
// never resolved on a guess.
type StartPoint struct {
	WarpID int
}

// errStartPointNoWarp is the shape rule of a save, named so a caller can tell "you
// passed nonsense" from "the database is down".
var errStartPointNoWarp = errors.New("warp id is not a warp")

// StartPointOf reads a commander's saved entry point.
//
// found is false when `commander.point` is empty (<= 0) - NOT an error: that is the
// normal state before the player ever pressed the button, and the caller must then
// fall through to the Last Point and then the map's own spawn. A point of 0 would be
// a restart warp id of 0, which no map carries.
//
// A MISSING COMMANDER ROW IS ALSO NOT AN ERROR, for the same reason (pgx.ErrNoRows):
// there is nothing saved, so there is nothing to enter at.
//
// THE WARP IS NOT VALIDATED HERE. Whether warp N belongs to a map, and to WHICH map,
// is a question about the spawn table (data\zone_points.json), not about this
// database, and the entry path answers it with ZonePoints.RestartWarpMap and then
// RestartLanding - which is also where the landing position comes from. Splitting the
// check across two places is how the two would eventually disagree.
func (s *PGStore) StartPointOf(ctx context.Context, commanderID int32) (StartPoint, bool, error) {
	if commanderID <= 0 {
		return StartPoint{}, false, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	var point int
	err := s.pool.QueryRow(ctx,
		`SELECT point FROM commander WHERE commander_id = $1`, commanderID).Scan(&point)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StartPoint{}, false, nil
		}
		return StartPoint{}, false, fmt.Errorf("read the start point of commander %d: %w",
			commanderID, err)
	}
	if point <= 0 {
		return StartPoint{}, false, nil
	}
	return StartPoint{WarpID: point}, true, nil
}

// SaveStartPoint writes a commander's saved entry point and hands back what it wrote.
//
// ONE statement: `UPDATE commander SET point = $2`. The same click in the same
// session writes the same answer every time, and there is no second row that could
// disagree with the first.
//
// IT NEVER WRITES `commander.zone_id` AND NEVER TOUCHES `character_position`, and
// that is the whole fix. `zone_id` is the LAST POINT's map (zone\last_point_db.go
// writes it on every way out of a map) and `character_position` is the last point's
// position. This function writing either is exactly how the owner's Coimbra save was
// replaced by Reboldoeux on log-out. A save cannot change where the session ended, and
// a log-out cannot change what the player chose - so the two facts cannot destroy each
// other.
//
// A commander that does not exist is an ERROR and not a silent zero: the caller is a
// player's click, and "the save worked" for a row that was never written is exactly
// the kind of lie this project refuses (see Handler.Commit). RowsAffected answers it
// without a second round trip, and without a foreign key's constraint name in a
// player's face.
func (s *PGStore) SaveStartPoint(ctx context.Context, commanderID int32, warpID int) (StartPoint, error) {
	if commanderID <= 0 {
		return StartPoint{}, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if warpID <= 0 {
		return StartPoint{}, fmt.Errorf("warp id %d %w", warpID, errStartPointNoWarp)
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE commander SET point = $2 WHERE commander_id = $1`, commanderID, warpID)
	if err != nil {
		return StartPoint{}, fmt.Errorf("write the start point of commander %d: %w",
			commanderID, err)
	}
	if tag.RowsAffected() == 0 {
		return StartPoint{}, fmt.Errorf("commander %d does not exist, so its start point "+
			"was NOT written", commanderID)
	}
	return StartPoint{WarpID: warpID}, nil
}
