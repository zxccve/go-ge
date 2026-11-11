-- +goose Up
-- The barrack slice: who a commander is, and which characters and gear belong
-- to them. Mirrors the legacy tables we actually join, in PostgreSQL types.
--
-- Legacy source (SQL Server):
--   GE_DATA.COMMANDER      17 columns, incl. NAME, ZONE_ID, ACCOUNT_ID
--   GE_DATA.CHARACTERS     SLOT, NAME, TEAMPOS, GENDER(bit), JOB, HEADTYPE,
--                          EXP(bigint), LIFE, HP(bigint), SP
--   GE_DATA.WEARITEM       CHARACTER_ID, ITEM_ID, SPOT   (which item is worn where)
--   GE_DATA.ITEM           ITEM_ID, CLASS_ID, COUNT, TYPE
--   GE_DATA.ITEM_REINFORCE ITEM_ID, ATK, DEF, HIT_COUNT, WLV  (HIT_COUNT = the "+N")
--
-- NOTE on `life`: it is NOT the character level. Level comes from `exp` through
-- the exp table, so life is carried only because the legacy row has it.

CREATE TABLE commander (
    commander_id integer     PRIMARY KEY,
    account_id   integer     NOT NULL,
    member_id    varchar(20) NOT NULL,
    name         varchar(20) NOT NULL,
    nation_id    smallint    NOT NULL DEFAULT 0,
    nation_point smallint    NOT NULL DEFAULT 0,
    rank         smallint    NOT NULL DEFAULT 0,
    duty         smallint    NOT NULL DEFAULT 0,
    fame         smallint    NOT NULL DEFAULT 0,
    karma        smallint    NOT NULL DEFAULT 0,
    duel_win     integer     NOT NULL DEFAULT 0,
    duel_lost    integer     NOT NULL DEFAULT 0,
    duel_draw    integer     NOT NULL DEFAULT 0,
    ladder       integer     NOT NULL DEFAULT 1000,
    zone_id      smallint    NOT NULL DEFAULT 0,
    channel_id   smallint    NOT NULL DEFAULT 0,
    point        integer     NOT NULL DEFAULT 0
);

-- Per-commander properties (Lock_CharSlot, Lock_TeamGroup, BrkName1..24 ...).
CREATE TABLE commander_property (
    commander_id integer     NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    property     varchar(64) NOT NULL,
    section      smallint    NOT NULL DEFAULT 0,
    value        varchar(64) NOT NULL,
    PRIMARY KEY (commander_id, property, section)
);

CREATE TABLE character (
    character_id integer     PRIMARY KEY,
    commander_id integer     NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    slot         integer     NOT NULL,
    name         varchar(20) NOT NULL,
    teampos      smallint    NOT NULL DEFAULT 0,
    gender       boolean     NOT NULL DEFAULT false,
    job          integer     NOT NULL,
    headtype     smallint    NOT NULL DEFAULT 0,
    exp          bigint      NOT NULL DEFAULT 0,
    life         smallint    NOT NULL DEFAULT 0,   -- NOT the level; see above
    hp           bigint      NOT NULL DEFAULT 0,
    sp           integer     NOT NULL DEFAULT 0,
    iscard       smallint    NOT NULL DEFAULT 0,
    pos_x        integer     NOT NULL DEFAULT 0,
    pos_y        integer     NOT NULL DEFAULT 0,
    pos_z        integer     NOT NULL DEFAULT 0
);
CREATE INDEX character_commander_idx ON character (commander_id);

CREATE TABLE item (
    item_id  bigint  PRIMARY KEY,
    class_id integer NOT NULL,
    count    integer NOT NULL DEFAULT 1,
    type     smallint NOT NULL DEFAULT 0
);

CREATE TABLE item_reinforce (
    item_id   bigint  PRIMARY KEY REFERENCES item (item_id) ON DELETE CASCADE,
    atk       integer NOT NULL DEFAULT 0,
    def       integer NOT NULL DEFAULT 0,
    hit_count integer NOT NULL DEFAULT 0,   -- the "+N" shown on the barrack gear
    wlv       integer NOT NULL DEFAULT 1
);

CREATE TABLE wear_item (
    character_id integer NOT NULL REFERENCES character (character_id) ON DELETE CASCADE,
    spot         smallint NOT NULL,         -- 0..20, the client's equipment slot
    item_id      bigint NOT NULL REFERENCES item (item_id),
    PRIMARY KEY (character_id, spot)
);

-- +goose Down
DROP TABLE wear_item;
DROP TABLE item_reinforce;
DROP TABLE item;
DROP TABLE character;
DROP TABLE commander_property;
DROP TABLE commander;
