-- +goose Up
-- Two bits of per-player data the barrack packet carries that the legacy schema
-- keeps in separate tables. Without them our packet differs from the reference
-- server's in exactly two places (proved by barrack\oracle_test.go):
--   * barrackName[0] was "None" instead of the player's own caption
--   * one character's promotion / cashPromotion were 0 instead of 4 / 3
--
-- Legacy source: COMMANDER_BARRACK_NAME (CHANNEL, NAME) and
-- CHARACTERS_PROPERTY (PROPERTY, SECTION, VALUE).

CREATE TABLE commander_barrack_name (
    commander_id integer     NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    channel      smallint    NOT NULL,
    name         varchar(20) NOT NULL,
    PRIMARY KEY (commander_id, channel)
);

CREATE TABLE character_property (
    character_id integer     NOT NULL REFERENCES character (character_id) ON DELETE CASCADE,
    property     varchar(64) NOT NULL,
    section      smallint    NOT NULL DEFAULT 0,
    value        varchar(64) NOT NULL,
    PRIMARY KEY (character_id, property, section)
);

-- +goose Down
DROP TABLE character_property;
DROP TABLE commander_barrack_name;
