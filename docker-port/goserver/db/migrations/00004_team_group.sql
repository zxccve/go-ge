-- +goose Up
-- BC_TEAMGROUP_LIST (cmd 41): the player's saved team groups.
--
-- Legacy source: TEAM_GROUP_SERVER in the barrack server
-- (source2\geServer\BarrackServer\CommanderLoader.h:11), which is
-- (index, name, characters[3] as character IDs).
--
-- The client's wire struct is name[17] + BYTE slot[3] - SLOTS, not character
-- ids (geShared.h:388). So a character id is stored here and resolved to its
-- slot at send time, which is also what the original does
-- (Client.cpp:1581 GetCharacter(characterID) -> GetSlot()).

CREATE TABLE team_group (
    commander_id integer  NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    idx          smallint NOT NULL,
    name         varchar(16) NOT NULL,
    character1   integer,
    character2   integer,
    character3   integer,
    PRIMARY KEY (commander_id, idx),
    FOREIGN KEY (character1) REFERENCES character (character_id) ON DELETE SET NULL,
    FOREIGN KEY (character2) REFERENCES character (character_id) ON DELETE SET NULL,
    FOREIGN KEY (character3) REFERENCES character (character_id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE team_group;
