-- +goose Up
-- THE HANDOFF IS A ONE-SHOT TOKEN (2026-11-10, card t_0cda12ac).
--
-- pending_zone_entry is what the barrack's START button writes and what the zone reads
-- when CZ_START_GAME arrives. It was written as an UPSERT and only ever SELECTed:
-- nothing consumed it and nothing expired it, so a replayed CZ_START_GAME could enter
-- with an old team for ever - the client never has to prove it just came from the
-- barrack.
--
-- The reference consumes the row once and refuses a stale one (offlinespada
-- docs\kb\zone.md, "AUTH_TIME version": -9993 stale / -9994 missing / -9995 unknown,
-- PROVEN on the stock binaries). Two columns are that mechanism here:
--
--   version      counts the barrack's writes: every new pick is a NEW token
--   consumed_at  the moment the zone took it; NULL means "unused"
--
-- The zone's consume is ONE compare-and-swap (zone\store.go ConsumeZoneEntry): update
-- where consumed_at IS NULL and picked_at is younger than 60 s, returning the slots.
-- A second attempt finds nothing. An old row is deleted and refused with its age.
ALTER TABLE pending_zone_entry
    ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS consumed_at timestamptz;

-- +goose Down
ALTER TABLE pending_zone_entry
    DROP COLUMN IF EXISTS consumed_at,
    DROP COLUMN IF EXISTS version;
