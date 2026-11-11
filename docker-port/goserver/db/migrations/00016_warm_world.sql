-- +goose Up
-- THE WARM WORLD: the maps a player lives in, from DECISIONS.md 2026-11-09 ("the everyday
-- interaction happens at the town and field and dungeon where people actually interact and
-- fight/grind"). Counted off datatable_map.xml's MovableZone closure: 17 outdoor towns, 19
-- fields they open onto (truce AND contested - Porto Bello is one), 7 open TRUCEWORLD
-- dungeons. 42 maps, ~6 GB at the measured live heap.
--
-- The two rows that were warm by accident are demoted here: 1361 btl_auch01 is a battleground
-- and 6004 office_coim is a room ("Pegadilla"). Both stay configured, both are on demand now -
-- a room is "mostly quest/mission related", the owner's words.
UPDATE zone_daemon SET warm = false, note = note || ' (on demand: a battleground)' WHERE map_id = 1361;
UPDATE zone_daemon SET warm = false, note = note || ' (on demand: a room)' WHERE map_id = 6004;

INSERT INTO zone_daemon (daemon_id, map_id, channel, name, port, warm, note) VALUES
    (200,    10, 0, 'reb_basic', 7010, true, 'Cite de Reboldoeux'),
    (201,    15, 0, 'coim_basic', 7011, true, 'Port of Coimbra'),
    (202,   208, 0, 'fld_coim', 7012, true, 'Coimbra Nimrod Bridge'),
    (203,  1301, 0, 'auch_basic', 7013, true, 'City of Auch'),
    (204,  1331, 0, 'fld_auch', 7014, true, 'Thueringen Lakeside'),
    (205,  9004, 0, 'gig_basic', 7020, true, 'Port of Gigante Island'),
    (206,  1401, 0, 'ust_basic', 7021, true, 'Ustiur Base Camp'),
    (207, 20401, 0, 'viron_basic', 7022, true, 'Viron'),
    (208, 20301, 0, 'cas_basic', 7023, true, 'Castilla Garrison 1'),
    (209,  8001, 0, 'bah_basic', 7024, true, 'Bahamar Base Camp'),
    (210, 20200, 0, 'erc_basic', 7025, true, 'Errac'),
    (211,   140, 0, 'alt_basic', 7026, true, 'Frourio'),
    (212,  6007, 0, 'los_basic', 7027, true, 'Los Toldos'),
    (213,    78, 0, 'bedel_basic', 7028, true, 'Cite de Bedel'),
    (214,   149, 0, 'ail_basic', 7029, true, 'Ailis Parlemo'),
    (215,    50, 0, 'arm_basic', 7030, true, 'Cite de Armonia'),
    (216, 20600, 0, 'bst_basic', 7031, true, 'Kielce'),
    (217,    90, 0, 'ile_basic', 7032, true, 'Illicia'),
    (218,    53, 0, 'fld_arm', 7033, true, 'Armonia, Anillo'),
    (219,   200, 0, 'fld_reb', 7034, true, 'Reboldoeux Queen''s Gate'),
    (220,  9001, 0, 'fld_gig', 7035, true, 'Gigante Outskirts'),
    (221,    80, 0, 'fld_ile01', 7036, true, 'Vernie mansion, Entrance'),
    (222,    91, 0, 'fld_ile02', 7037, true, 'Royal Road'),
    (223,   204, 0, 'fld_ptv', 7038, true, 'Porto Bello'),
    (224,   207, 0, 'fld_alq', 7039, true, 'Al Quelt Moreza'),
    (225,   142, 0, 'fld_alt01', 7040, true, 'Camellia Teia'),
    (226,   143, 0, 'fld_alt02', 7041, true, 'River of Fire'),
    (227,   144, 0, 'fld_alt03', 7042, true, 'Portus'),
    (228,   145, 0, 'fld_alt04', 7043, true, 'Luinna'),
    (229, 20602, 0, 'fld_bst01', 7044, true, 'Kielce Training Yard'),
    (230, 20605, 0, 'fld_bst02', 7045, true, 'Kielce Training Yard (Night)'),
    (231,  3331, 0, 'fld_jaq', 7046, true, 'Jaquin lava plateau'),
    (232, 20501, 0, 'fld_lucifer', 7047, true, 'Lucifer, Entrance'),
    (233,  3531, 0, 'fld_tor', 7048, true, 'Torsche Mansion, Savage Garden'),
    (234,   203, 0, 'fld_ttr', 7049, true, 'Tetra Great Ruins'),
    (235,    55, 0, 'dun_arm01', 7050, true, 'Armonia, Encerrar'),
    (236,   154, 0, 'dun_truman01', 7051, true, 'dun_truman01'),
    (237,   155, 0, 'dun_truman02', 7052, true, 'dun_truman02'),
    (238,   156, 0, 'dun_truman03', 7053, true, 'dun_truman03'),
    (239,   157, 0, 'dun_truman04', 7054, true, 'dun_truman04'),
    (240,   158, 0, 'dun_truman05', 7055, true, 'dun_truman05'),
    (241,   159, 0, 'dun_truman_Boss', 7056, true, 'dun_truman_Boss')
ON CONFLICT (map_id) DO UPDATE SET warm = true, port = EXCLUDED.port, note = EXCLUDED.note;

-- +goose Down
DELETE FROM zone_daemon WHERE daemon_id BETWEEN 200 AND 241;
UPDATE zone_daemon SET warm = true WHERE map_id IN (1361, 6004);
