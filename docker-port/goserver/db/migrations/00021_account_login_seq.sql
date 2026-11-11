-- +goose Up
-- ONE LIVE LOGIN PER ACCOUNT, the barrack half (2026-11-11, card t_c38359e7).
--
-- `zone\live.go` closes the older ZONE session when a second one enters (card
-- t_487ea31c), but nothing stopped two BARRACK connections on one account from both
-- pressing Start Game: each wrote its own `pending_zone_entry` row and each was handed
-- a zone address. Only the zone's own claim then refused the second, and the older
-- connection's handoff row was left in the table for the next reader.
--
-- THE REFERENCE KEEPS ONE SESSION PER ACCOUNT and closes the older one
-- (offlinespada docs\kb\accounts-security.md:32, PROVEN on the stock binaries; see
-- docker-port\research\offlinespada-adopt-2026-11-09.md A1). This column is the
-- barrack's witness of that order: it is INCREMENTED once per CB_LOGIN, kept on the
-- connection, and READ BACK when the player asks to go to the zone - a Start Game
-- stamped below the account's latest login came from a superseded connection.
--
-- A COLUMN AND NOT A MAP IN MEMORY, on purpose. The account's latest login has to
-- survive a barrack restart: after a restart the process remembers nothing, so a
-- client still holding the pre-restart socket would look exactly like the live one.
-- The row is the only witness both a restart and a second connection can read.
--
-- bigint, not a timestamp: it counts logins and is never compared with a clock. Every
-- existing account starts at 0, so the first login after this migration is 1 and
-- equal-to-latest is allowed - a single login is never refused (see barrack\login_seq.go).
ALTER TABLE account_info
    ADD COLUMN IF NOT EXISTS login_seq bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE account_info
    DROP COLUMN IF EXISTS login_seq;
