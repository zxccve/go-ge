-- +goose Up
-- THE SAVED START POINT ("Save as Start Point", zone\start_point.go) - the town a
-- team re-enters the world at after the barrack.
--
-- WHY THIS TABLE EXISTS AT ALL, AND WHAT IT REPLACES. The save point used to be
-- stored in `commander.zone_id` (the map) plus `commander_property.RestartWarpID`
-- (the warp). `commander.zone_id` ALSO holds the LAST POINT - where the leader stood
-- when the session left, written on every log-out, map change and disconnect
-- (zone\last_point_db.go). ONE COLUMN, TWO MEANINGS, so the two wrote over each
-- other: on 2026-10-29 the owner saved The Port of Coimbra as his start point and
-- then logged out in Cite de Reboldoeux, and the log-out writer put Reboldoeux's map
-- into the column the barrack reads. The barrack then offered Reboldoeux.
--
-- So the save point gets its OWN row here and never touches `commander.zone_id`
-- again. The Last Point keeps `commander.zone_id` + `character.pos_x/pos_y/pos_z`
-- exactly as before. The two facts can no longer disagree, because they are no
-- longer the same column.
--
-- ONE ROW PER COMMANDER. A commander has one chosen start point; a second save
-- replaces the first, which is an UPSERT on the primary key.
--
-- `warp_id` IS CHECKED AGAINST `map_id` WHEN IT IS READ, NOT WHEN IT IS WRITTEN.
-- The pair must be (a map, a restart-warp row that belongs to THAT map). The write
-- side only ever saves the town the player is standing in, so the pair it writes is
-- correct by construction; a row that is wrong anyway (a hand edit, a table that
-- changed under an old save) is REFUSED at entry with a log line and the entry falls
-- through to the Last Point. A guessed map is the one thing this project never does.
--
-- NO ROWS ARE INSERTED HERE, ON PURPOSE. Commander 25 currently holds the
-- mismatched pair the bug produced: `commander.zone_id = 10` (Reboldoeux, written by
-- a log-out) and `RestartWarpID = 213` (Coimbra's warp, written by the save). There
-- is no way to recover WHICH map 213 was meant to sit on from the rows alone, and
-- writing a guess would break the rule above. So the table starts EMPTY and the
-- fallbacks do the work: commander 25 enters at its Last Point (map 10, a valid pair
-- for the map it is on), and the next Save click in Coimbra writes a correct row.
-- `commander.zone_id` is left alone because it is a VALID Last Point - rewriting it
-- would be throwing away a good fact to make room for a guess.
--
-- WHO WRITES IT: goserver\zone on CZ_SAVE_RESTART_WARPID (3468).
-- WHO READS IT: the same zone on entry (Handler.savedStartPoint), and the barrack
-- (barrack\barrack_store.go Commander) for the map name on the Last Point button.

CREATE TABLE saved_start_point (
    commander_id integer     PRIMARY KEY REFERENCES commander (commander_id) ON DELETE CASCADE,
    map_id       integer     NOT NULL,          -- the map the saved warp belongs to
    warp_id      integer     NOT NULL,          -- a restart-warp row ON that map
    saved_at     timestamptz NOT NULL DEFAULT now()
);

-- No second index: every read is by commander_id, which is the primary key.

-- +goose Down
DROP TABLE saved_start_point;
