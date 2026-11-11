-- +goose Up
-- The player's OWN saved points - the "save where I stand" list behind the
-- teleportation window (CZ_TELEPORT_SAVE 3640 / CZ_TELEPORTLIST 3641 /
-- CZ_TELEPORT_REMOVE 3643 / CZ_TELEPORT 3644 and the list they are answered with,
-- ZC_TELEPORTLIST 3642).
--
-- WHY A ROW IS A POSITION AND NOT A WARP ID. The original server kept its list in
-- memory and gave each point the next free `warpID` (CTeleportInfo::GetNewWarpID,
-- source2\geServer\ZServerLib\TeleportInfo.cpp:31), then wrote the point to its SQL
-- Server through a `geDB::Insert` job. Nothing about that id is on the wire: the
-- client names a point by its INDEX in the list it was last sent (script.cpp:1326
-- `TeleportSelect(myPcIndex, teleportIndex)`), so the index is the only key this
-- side has to keep stable, and a slot number is enough.
--
-- THE SHAPE OF A SAVED POINT, from the original server's own write:
--   TeleportInfo.cpp:128-133  WARP_INFO* info = bin.Allocate(sizeof(WARP_INFO));
--                             info->pos.x = point.x; info->pos.y = point.y;
--                             info->pos.cell = point.z; info->mapId = ...;
--   geShared.h:2440           struct WARP_INFO { WORLDPOS pos; MAP_ID mapId; char warpMemo[]; }
--   geShared.h:712            struct WORLDPOS { long x; long y; long cell; }
-- So a saved point is (x, y, CELL, map id, memo) - the third coordinate is a
-- NAVMESH CELL index, exactly like every other position this server stores
-- (`character.pos_x/pos_y/pos_z`, see 00002_barrack.sql). It is NOT a height:
-- a cell that does not exist on the destination's mesh drops the character
-- through the world (the PathEngine crash `zone\gate.go` refuses for the same
-- reason), so the column is named `cell` and not `z`.
--
-- ONE ROW PER SLOT PER COMMANDER. The client's list is positional and its remove
-- button sends an index, so two rows in one slot would make "remove slot 2" delete
-- one of them and leave the other to be drawn again on the next list.
--
-- `name` IS THE POINT'S MEMO, and the Warp List window draws it: 3634
-- ZC_COMMWARP_LIST carries a 32-byte memo at record +14 and the client prints
-- "(n) memo", falling back to "(n) <map name>" when it is empty
-- (UIWarpListFrame.cpp:53-60). CORRECTED 2026-10-30: this comment used to say the
-- record carries no memo, quoting geZCPacket.h:2342 - that struct belongs to the
-- OTHER window (ZC_TELEPORTLIST 3642, the `teleportation` frame), whose 14-byte
-- record really is position + map id alone and whose rows get their text from the
-- map table. One table, two windows; this column is drawn by one of them.
-- WHO WRITES IT: goserver\zone on 3640 (save), 3643 (remove), 3635 (Warp List
-- save) and 3638 (Warp List rename). WHO READS IT: the same zone on 3641 and on
-- the entry burst's 3634.

CREATE TABLE saved_teleport (
    commander_id integer     NOT NULL REFERENCES commander (commander_id) ON DELETE CASCADE,
    slot         integer     NOT NULL,          -- the INDEX the client names, 0-based
    map_id       integer     NOT NULL,
    x            integer     NOT NULL,
    y            integer     NOT NULL,
    cell         integer     NOT NULL,          -- a navmesh triangle index, never a height
    name         varchar(64) NOT NULL DEFAULT '',
    saved_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (commander_id, slot)
);

-- The list query is "every saved point of this commander, in slot order", which is
-- the primary key order, so no second index is needed. A point that is saved again
-- into its own slot is an UPSERT on that key.

-- +goose Down
DROP TABLE saved_teleport;
