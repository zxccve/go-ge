-- +goose Up
-- THE TRUMAN DUNGEONS ARE MISSION INSTANCES, NOT FIELDS (owner, 2026-11-09, looking at the console:
-- "we dont need the truman dungeon. that is mission instance").
--
-- The map table alone does not say so: dun_truman01..05 and dun_truman_Boss are Type="TRUCEWORLD"
-- with Mission="FALSE", exactly like the open dungeon we keep (dun_arm01). What separates them is
-- what the player reads and what the map saves:
--
--   dun_arm01        EngName="Armonia, Encerrar"   SaveWarp=TRUE     -> a place you can stand in
--   dun_truman01..   EngName is the internal name  SaveWarp=FALSE    -> nothing the client names
--
-- and the trumans' own door list points back at the Armonia dungeons. A map with no name, no save
-- warp and no door of its own is instanced content: it costs ~140 MB to keep warm and nobody walks
-- into it from the world.
--
-- They stay CONFIGURED and ON DEMAND: a mission that asks for one still gets it, and the manager
-- starts it for that request. Warm now means what the owner plays, nothing else.
UPDATE zone_daemon SET warm = false,
    note = note || ' (on demand 2026-11-09: a mission instance, not a field - owner''s call)'
WHERE map_id IN (154, 155, 156, 157, 158, 159);

-- +goose Down
UPDATE zone_daemon SET warm = true WHERE map_id IN (154, 155, 156, 157, 158, 159);
