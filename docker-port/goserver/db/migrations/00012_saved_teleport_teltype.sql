-- +goose Up
-- FORTY POINTS IN FOUR LISTS OF TEN, AND EVERY ONE OF THEM IS FREE.
--
-- WHAT WAS WRONG. `saved_teleport` had no `teltype` column, so the Warp List window
-- (`warplist`) could only ever be filled for teletype 0 - the first panel - and the
-- three "Warp Extend" panels beside it were sent as empty lists on purpose. The
-- earlier call ("teltype 0 only") read the reference server's `Lock_Warp` cash
-- property as if the cash item PAID for a saved point.
--
-- THE OWNER OVERRULED THAT, in his words: "nothing was paid lol. just serve all 40.
-- it was supposed to be free to add to the entry. u only need those cash item to
-- ENABLED that slot extension. read the scp thank you". The client's own script says
-- the same: `func COMMWARP_SAVE` in ge\ui\uiscp\game.scp loops
--
--	for(telType = 0, 3) { if( CommWarpCount(telType) < 10 ) { CommWarpSave(telType, memo); return; } }
--
-- so the panel is CHOSEN BY THE CLIENT (the first one holding fewer than ten rows),
-- no item is checked and nothing is charged. The cash item only ever UNLOCKED the
-- extra panels; it never paid for a row.
--
-- WHY A COLUMN AND NOT A SECOND TABLE. A saved point is still one row of one shape -
-- (map, x, y, cell, memo) - and only its owner's PANEL and its position in it were
-- missing. One column keeps every existing query a single join-free read and makes
-- "all forty points of this commander" one statement, which is what the duplicate
-- rule needs (the same spot anywhere in the forty is refused).
--
-- THE PRIMARY KEY GROWS, it does not change meaning: a slot number is only unique
-- WITHIN a panel now, because the client draws four independent lists and names a
-- row by its position in the one it was sent. Every row that exists today was saved
-- into panel 0, so they all keep their slot (the DEFAULT 0) and nothing moves.
--
-- `display_order` IS PER PANEL TOO: a save appends to the bottom of the panel it was
-- saved into, and Up/Down (3639) splices inside one panel. Two panels may therefore
-- hold the same display_order numbers at the same time, which is why the index below
-- carries the teltype.
ALTER TABLE saved_teleport ADD COLUMN teltype smallint NOT NULL DEFAULT 0;

-- The four panels the client's own `MAX_TELEPORT_TYPE` describes. A fifth cannot be
-- drawn or clicked, so the table refuses it here as well as in the store.
ALTER TABLE saved_teleport
    ADD CONSTRAINT saved_teleport_teltype_check CHECK (teltype >= 0 AND teltype <= 3);

ALTER TABLE saved_teleport DROP CONSTRAINT IF EXISTS saved_teleport_pkey;
ALTER TABLE saved_teleport
    ADD CONSTRAINT saved_teleport_pkey PRIMARY KEY (commander_id, teltype, slot);

-- The read the window does: every row of one commander, in panel order, then the
-- panel's own display order. Replaces the old (commander_id, display_order, slot)
-- index, whose second column is now only half of the key.
DROP INDEX IF EXISTS saved_teleport_display_idx;
CREATE INDEX saved_teleport_display_idx
    ON saved_teleport (commander_id, teltype, display_order, slot);

-- +goose Down
-- LOSES DATA ON PURPOSE, and says so: the old schema has no panel to keep rows in,
-- so the points in panels 1-3 cannot survive a rollback. They are deleted BEFORE
-- the old (commander_id, slot) primary key goes back on, because two panels may hold
-- the same slot number and that key could not describe both.
DROP INDEX IF EXISTS saved_teleport_display_idx;
ALTER TABLE saved_teleport DROP CONSTRAINT IF EXISTS saved_teleport_pkey;
DELETE FROM saved_teleport WHERE teltype <> 0;
ALTER TABLE saved_teleport
    ADD CONSTRAINT saved_teleport_pkey PRIMARY KEY (commander_id, slot);
ALTER TABLE saved_teleport DROP CONSTRAINT IF EXISTS saved_teleport_teltype_check;
ALTER TABLE saved_teleport DROP COLUMN teltype;
CREATE INDEX saved_teleport_display_idx
    ON saved_teleport (commander_id, display_order, slot);
