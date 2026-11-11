-- +goose Up
-- THE ZONE REGISTRY IS IN THE DATABASE, NOT ON A COMMAND LINE.
--
-- THE ORIGINAL KEPT IT THERE, and this table is that idea. Measured 2026-11-09 in the
-- imported RESOURCE database: `DAEMON` (54 rows: DAEMON_ID, MACHINE_ID, DAEMON_KIND 'B'
-- barrack / 'Z' zone, DAEMON_NAME, DAEMON_PORT, MANAGEMENT_ONOFF bit, COMMAND - the whole
-- command line) plus `ZONE_DAEMON` (402 rows: ZONE_ID, CHANNEL_ID, DAEMON_ID). Together
-- they answer "which daemon runs which map, on which port, switched on or off", and
-- `NS_GET_ZONE_DAEMON_EXISTS` reads exactly those two tables. The database held the PLAN;
-- the ManagerHub fleet watched the processes. Ours splits the same way: this table is the
-- plan, `cmd/zoneman` is the watcher that starts and stops the processes.
--
-- WHY ONE TABLE AND NOT TWO. The original needed two because one daemon hosted many maps.
-- Our zone process serves ONE map (the settled call: one process per town, for blame), so
-- a daemon row and a zone row are the same thing.
--
-- `warm` IS THE OWNER'S RULE: "i prefer we load everything and only mission/instance load
-- on demand". A warm row is started at boot and never stopped; every other row is started
-- when a gate asks for it and stopped after -idle minutes with nobody in it.
--
-- `port` IS FIXED PER ROW so a map's address does not move between restarts: clients cache
-- the last address they were told, and a map that wanders between ports reads as "the
-- server is down". The zone still announces the port it actually BOUND - that file stays
-- the authority for a live address, this table only decides which port to try.
CREATE TABLE zone_daemon (
    daemon_id     smallint PRIMARY KEY,
    map_id        integer  NOT NULL UNIQUE,
    channel       smallint NOT NULL DEFAULT 0,
    name          text     NOT NULL,
    port          integer  NOT NULL,
    kind          char(1)  NOT NULL DEFAULT 'Z',
    management_on boolean  NOT NULL DEFAULT true,
    warm          boolean  NOT NULL DEFAULT false,
    note          text     NOT NULL DEFAULT ''
);

-- The seven maps this server has been running by hand, now rows. They are the towns and
-- fields the owner actually plays; the rest of the 2,381 map rows stay unconfigured until
-- somebody wants them, which is what "on demand" means for a map with no row at all.
INSERT INTO zone_daemon (daemon_id, map_id, channel, name, port, warm, note) VALUES
    (101,   10, 0, 'reb_basic',   7010, true, 'Reboldoeux'),
    (102,   15, 0, 'coim_basic',  7011, true, 'Coimbra'),
    (103,  208, 0, 'fld_coim',    7012, true, 'Coimbra field'),
    (104, 1301, 0, 'auch_basic',  7013, true, 'Auch'),
    (105, 1331, 0, 'fld_auch',    7014, true, 'Auch field'),
    (106, 1361, 0, 'btl_auch01',  7015, true, 'Auch battle field'),
    (107, 6004, 0, 'office_coim', 7016, true, 'Coimbra office');

-- +goose Down
DROP TABLE zone_daemon;
