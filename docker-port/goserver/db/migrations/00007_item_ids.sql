-- +goose Up
-- The BASE half of an item id.
--
-- The shipped server mints ITEM_ID as `(base << 32) + counter`
-- (geServerShared\source\GUIDGen.cpp): one base per process, one counter inside
-- it. The base used to come from RESOURCE.dbo.INSTANCEHISTORY, a SQL Server table
-- this server does not have, so PostgreSQL hands it out instead - one row from
-- this sequence per PROCESS, the same guarantee by another engine.
--
-- WHY NOT JUST AN IDENTITY: an id with no structure is only a key, and a key is
-- all the client needs as far as anything here has been read. But the shape is
-- the game's own and costs nothing to keep, and "the client ignores the high
-- bits" is an assumption this server should not have to make.
--
-- WHY 100000: existing ids carry bases around 2,500 (id ~1.05e13 at base 2,500).
-- Starting well clear of that range means a later import of the SQL Server rows -
-- which bring their own ids - can never collide with one minted here.
--
-- WHO USES IT: goserver\zone, once per process, on the first item it mints.

CREATE SEQUENCE item_base_seq START WITH 100000 INCREMENT BY 1;

-- +goose Down
DROP SEQUENCE item_base_seq;
