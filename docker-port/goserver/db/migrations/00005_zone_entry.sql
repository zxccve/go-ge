-- +goose Up
-- The team the player picked in the barrack, waiting for the ZONE process to read
-- it. This replaces the out\zone_team_<id>.json handoff file for anything that
-- speaks PostgreSQL.
--
-- It is a MESSAGE, not history: one row per commander, overwritten on every New
-- Game / Continue Game, and only the newest pick is meaningful. Keying it by
-- commander_id is what makes it safe for several players at once - the old shared
-- fallback file was last-writer-wins.
--
-- Slots are the raw three WORDs the client sent (CB_STARTGAME, geBCPacket.h), with
-- 255 meaning EMPTY_CHARSLOT - the same value the original server writes
-- (Client.cpp:1583). Storing the empties keeps the row a faithful copy of the
-- request instead of a paraphrase of it.
--
-- WHO READS IT: src\zone_server.py, while the zone is still Python. That read is a
-- BRIDGE and is deleted when the zone is ported to Go; see DECISIONS.md.
-- WHO WRITES IT: goserver\barrack on CB_STARTGAME (22) and CB_CONTINUEGAME (23).

CREATE TABLE pending_zone_entry (
    commander_id integer     NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    channel      smallint    NOT NULL DEFAULT 0,
    slot0        smallint    NOT NULL,
    slot1        smallint    NOT NULL,
    slot2        smallint    NOT NULL,
    is_continue  boolean     NOT NULL DEFAULT false,
    picked_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (commander_id)
);

-- +goose Down
DROP TABLE pending_zone_entry;
