-- +goose Up
-- SIX OF THE 42 WARM MAPS HAVE NO SPAWN POINT, SO THEY CANNOT HOST A CLIENT YET.
--
-- Started live 2026-11-09 19:2x: 36 of the 42 came up and announced; these six logged
--   "map N (fld_alt01) has no spawn row in the table, so the client cannot be placed on it"
-- and exited. `data\zone_points.json` (rebuilt the same evening) says 757 of 2,381 maps have no
-- spawn at all, and these six are in that set: the path engine found no ground for the point the
-- map's own row names, so their cell is null.
--
-- They stay CONFIGURED (a row, a port, a mesh, NPCs) and become warm again the moment their
-- spawn can be resolved - either by fixing the point derivation for their mesh, or by giving the
-- zone a measured fallback cell. Warm is a promise that the map is playable; until then the
-- manager should not spend 25 s per boot discovering it is not.
UPDATE zone_daemon SET warm = false,
    note = note || ' (warm OFF 2026-11-09: no spawn point in zone_points.json)'
WHERE map_id IN (91, 142, 143, 145, 20602, 20605);

-- +goose Down
UPDATE zone_daemon SET warm = true WHERE map_id IN (91, 142, 143, 145, 20602, 20605);
