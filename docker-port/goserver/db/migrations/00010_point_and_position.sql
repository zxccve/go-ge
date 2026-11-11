-- +goose Up
-- THE SAVED START POINT AND THE LAST POINT, SEPARATED FOR GOOD.
--
-- WHAT THE ORACLE SAYS. The genuine v32 SQL Server schema (database GE_DATA) keeps
-- the two facts in two places, and this migration copies that shape rather than
-- inventing one:
--
--   COMMANDER.POINT          integer  - the SAVED START POINT, and it is a RESTART
--                                       WARP ID (203 Reboldoeux, 213 Coimbra, 227
--                                       Auch ...), NOT a map id and NOT a town id.
--                                       The original server resolves the zone and
--                                       the landing spot from that warp row every
--                                       time: Commander.cpp:378 GetNewGameZoneInfo
--                                       reads geWarpTable::Get(m_restartWarpID).
--   COMMANDER.ZONE_ID        smallint - the LAST POINT's map: the zone to resume.
--   CHARACTER_POSITION       xpos, ypos, zpos, map_id, updated - the LAST POINT's
--                                       position, ONE ROW PER CHARACTER.
--   MISSION_RESTART_INFO              the MISSION feature's table. It is EMPTY in
--                                       the oracle, so nothing here touches it and
--                                       the mission button has nothing to resume.
--
-- `commander.point` ALREADY EXISTS in this PostgreSQL schema, so this file does not
-- create it. It only starts being the save point instead of a copy of somebody
-- else's number.
--
-- WHY TWO PLACES AND NOT ONE. Until now the save point and the last point competed
-- for `commander.zone_id`, and the last point is written on EVERY way out of a zone
-- (log-out, map change, disconnect). The owner's case, measured on 2026-10-29:
--
--   20:15:17  LAST POINT saved for commander 25 ...: map 10 - wrote commander.zone_id
--   20:16:21  the saved start point of commander 25 names warp 213 on map 10 ...
--             and that warp belongs to map 15 (Coimbra), not map 10
--
-- so the log-out in Cite de Reboldoeux replaced the Coimbra save. Two facts, two
-- places: a save writes `commander.point` and never `zone_id`; a log-out writes
-- `commander.zone_id` and `character_position` and never `point`. Neither can destroy
-- the other because neither reads or writes the other's column.
--
-- ONE ROW PER CHARACTER is the oracle's shape, and it is not an invention of ours:
-- the position belongs to a character. This zone tracks ONE live position per
-- session (the leader's walk), so a save writes every character of the team at the
-- leader's walked spot; the followers are re-formed around the leader on entry by
-- arrivalFormation anyway. Only the leader's row (pending_zone_entry.slot0) is read
-- back.
--
-- THE EXACT REPAIR ON COMMANDER 25. The bug left it holding a mismatched pair:
--
--   commander.zone_id = 10        a VALID Last Point (the log-out's map) - LEFT ALONE,
--                                 rewriting it would throw away a good fact.
--   commander.point   = 3090      a copy of commander 24's value that came in with
--                                 the account seed - NEVER a save point of 25's.
--   commander_property RestartWarpID = 213
--                                 the real save: Coimbra's restart warp.
--
-- The UPDATE below MOVES 213 into `commander.point` and the DELETE drops the
-- property row, so 25's pair becomes coherent: point = 213 (Coimbra), zone_id = 10
-- (Reboldoeux, last point).
--
-- THE `point = 0` GUARD IS DELIBERATELY ABSENT. A guard would refuse the repair
-- because 25's `point` holds 3090, and the value it would be protecting was never a
-- save point. The property row has no such ambiguity: a `RestartWarpID` row only
-- ever meant "the saved start point".
--
-- COMMANDER 24'S `point = 3090` IS NEVER TOUCHED, because 24 has no `RestartWarpID`
-- property row; the UPDATE is driven from that row, not from `commander`.
--
-- `saved_start_point` IS DROPPED, and only because it is EMPTY (0 rows) and
-- superseded by `commander.point`. Nothing is lost by dropping it: the row it would
-- have held is exactly what the UPDATE above writes.
--
-- `kind` SAYS WHICH BARRACK BUTTON THE PLAYER PRESSED, because the entry order
-- depends on it:
--
--   0 = new / saved start  - CB_STARTGAME       (cmd 22, BAR_NEW)
--   1 = continue / last    - CB_CONTINUEGAME    (cmd 23, BAR_CON)
--   2 = mission            - CB_CONTINUEMISSION (cmd 24, BAR_CON_MISSION)
--
-- `is_continue` is KEPT and written in step with `kind = 1`, so any reader that still
-- asks the old column gets the same answer.

ALTER TABLE pending_zone_entry ADD COLUMN kind smallint NOT NULL DEFAULT 0;
UPDATE pending_zone_entry SET kind = CASE WHEN is_continue THEN 1 ELSE 0 END;

CREATE TABLE character_position (
    character_id integer     PRIMARY KEY REFERENCES character (character_id) ON DELETE CASCADE,
    xpos         integer     NOT NULL,
    ypos         integer     NOT NULL,
    zpos         integer     NOT NULL,   -- a NAVMESH CELL index, never a height
    map_id       integer     NOT NULL,
    updated      timestamptz NOT NULL DEFAULT now()
);

-- Seed it from what this server already had, so the FIRST "resume" after this
-- migration is the player's real spot and not a fallback to the map spawn. A
-- character that never moved (0, 0) is skipped on purpose: those columns carry the
-- table defaults, and (0, 0) is off every map.
INSERT INTO character_position (character_id, xpos, ypos, zpos, map_id)
SELECT ch.character_id, ch.pos_x, ch.pos_y, ch.pos_z, c.zone_id
  FROM character ch JOIN commander c ON c.commander_id = ch.commander_id
 WHERE c.zone_id > 0 AND (ch.pos_x <> 0 OR ch.pos_y <> 0)
ON CONFLICT (character_id) DO NOTHING;

-- The old save point lived in commander_property.RestartWarpID; move it to
-- commander.point. See "THE EXACT REPAIR ON COMMANDER 25" above for why there is no
-- `c.point = 0` guard here.
UPDATE commander c SET point = p.value::integer
  FROM commander_property p
 WHERE p.commander_id = c.commander_id
   AND p.property = 'RestartWarpID'
   AND p.value ~ '^[0-9]{1,9}$'
   AND p.value::integer > 0;

DELETE FROM commander_property WHERE property = 'RestartWarpID';

DROP TABLE saved_start_point;

-- +goose Down
-- The `saved_start_point` table comes back EMPTY: the row it used to hold is now in
-- `commander.point`, and there is no `map` half left to rebuild it from (the warp id
-- alone is enough for the ENTRY path, which resolves the map from the warp row, but
-- not for this table's own pair).
--
-- THE DELETED `RestartWarpID` PROPERTY ROWS ARE NOT RESTORED, and cannot be: the
-- DELETE above dropped the commander -> value association, and nothing in the
-- remaining schema records which commander held which warp id. A reader of this Down
-- should treat a commander whose save point is missing after a rollback as a value
-- this migration consumed, not as a bug.
CREATE TABLE saved_start_point (
    commander_id integer     PRIMARY KEY REFERENCES commander (commander_id) ON DELETE CASCADE,
    map_id       integer     NOT NULL,
    warp_id      integer     NOT NULL,
    saved_at     timestamptz NOT NULL DEFAULT now()
);

DROP TABLE character_position;

ALTER TABLE pending_zone_entry DROP COLUMN kind;
