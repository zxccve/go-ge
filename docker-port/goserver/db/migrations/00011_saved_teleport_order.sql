-- +goose Up
-- THE WARP LIST IS DRAWN IN SAVE ORDER - `slot` is only a storage key.
--
-- WHAT WAS WRONG. `slot` was doing two jobs: the primary key of this table AND
-- the order the window draws its rows in, because the read was `ORDER BY slot`.
-- A save took the LOWEST FREE slot, so after a delete the next point refilled the
-- hole and the client drew it in the middle of the list. The owner's log,
-- logs\zone15_live.log, 2026-10-30 (his own five saves):
--
--   15:09:57  SAVED slot 4 ... memo "sdfsdfs"
--   15:10:00  CZ_COMMWARP_REMOVE (row 3) ... slot 3 deleted
--   15:10:07  SAVED slot 3 ... memo "56565"
--
-- and the window drew 111, 222, 333, 56565, sdfsdfs - the point saved LAST in
-- fourth place. His words: "deleting and inserting new entry replace that entry
-- number? normally it would just insert the new entry to the bottom of the list."
--
-- WHY A NEW COLUMN AND NOT A NEW ORDERING RULE. "Oldest first" can be read off
-- `saved_at`, but the window also has Up and Down (cmd 3639, which this client
-- really does send - an earlier note said it never did and that was wrong), and a
-- move cannot be expressed by a timestamp: two rows' times would have to be
-- rewritten into each other, which both loses when the points were saved and
-- cannot be done for a move that shifts several rows. So the display order gets
-- its own column, and it is written ONLY by the three things that change the list
-- (a save appends, a delete leaves a gap, a move splices).
--
-- `slot` keeps its job: the (commander_id, slot) primary key that makes "one row
-- per slot" true. It is NOT renumbered by a save or a delete any more, and the
-- player never sees it.
--
-- BACKFILL, and it is the ONLY place saved_at decides anything: rows already in
-- the table were saved in the order they arrived, so their earlier time IS their
-- earlier position. `saved_at, slot` gives a stable answer when two rows share a
-- clock tick.
ALTER TABLE saved_teleport ADD COLUMN display_order integer NOT NULL DEFAULT 0;

UPDATE saved_teleport s
   SET display_order = o.rn
  FROM (SELECT commander_id,
               slot,
               row_number() OVER (PARTITION BY commander_id
                                  ORDER BY saved_at, slot) - 1 AS rn
          FROM saved_teleport) o
 WHERE s.commander_id = o.commander_id
   AND s.slot = o.slot;

-- DELIBERATELY NOT UNIQUE. A move rewrites every row's display_order inside one
-- transaction, one UPDATE at a time, so an interim duplicate exists while it runs;
-- a unique index would make the second UPDATE fail. Five rows are read in order by
-- (commander_id, display_order, slot), which is what this index serves.
CREATE INDEX saved_teleport_display_idx
    ON saved_teleport (commander_id, display_order, slot);

-- +goose Down
DROP INDEX IF EXISTS saved_teleport_display_idx;
ALTER TABLE saved_teleport DROP COLUMN display_order;
