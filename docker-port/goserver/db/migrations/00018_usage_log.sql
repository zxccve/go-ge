-- +goose Up
-- THE RECORDS THE ORIGINAL KEPT IN ITS DATABASE, kept in ours.
--
-- MEASURED 2026-11-09 in the imported server databases (the owner: "thats the whole point of having
-- the DB as oracle"). The original splits its state in three:
--
--   RESOURCE       the PLAN        DAEMON (54 rows: id, machine, kind B/Z, name, port, on-off, command)
--                                  ZONE_DAEMON (402 rows: zone+channel -> daemon)   [we: zone_daemon]
--   RESOURCE_LOG   the SAMPLES     SESSION_COUNT_ZONE (LOG_DATE, GROUP_ID, ZONE_ID, CHANNEL_ID, CONN_COUNT)
--                                  LOG_MANAGER_CCU (3,823 rows), USER_COUNTRY (9,209 rows)
--   GE_LOG         the EVENTS      ACCOUNT_LOGIN (4,204 rows), ACCOUNT_LOGOUT (4,102 rows)
--
-- What it did NOT put in a table: the per-map static data (XML/SCP on disk) and the walk meshes. Our
-- presence / request / handoff files stay files for the same reason - they are process-to-process
-- signals, and a DB round-trip on the login path would be a new way to fail.
--
-- These three tables are the records that OUTLIVE a process: "how many players were on map 15 at
-- 21:00", "who logged in from where", "which instance has already been used". Column names and types
-- are the original's own, so a query written against the imported RESOURCE_LOG / GE_LOG runs here.

CREATE TABLE session_count_zone (
    log_date   timestamptz NOT NULL,
    group_id   smallint    NOT NULL DEFAULT 1,
    zone_id    smallint    NOT NULL,
    channel_id smallint    NOT NULL DEFAULT 0,
    conn_count integer     NOT NULL
);
-- The one question this table is asked is "what happened in this window", so the index is the clock.
CREATE INDEX session_count_zone_log_date ON session_count_zone (log_date);

CREATE TABLE account_login (
    log_date timestamptz NOT NULL,
    group_id smallint    NOT NULL DEFAULT 1,
    member_id text       NOT NULL,
    conn_ip   text       NOT NULL DEFAULT ''
);
CREATE INDEX account_login_log_date ON account_login (log_date);

CREATE TABLE account_logout (
    log_date timestamptz NOT NULL,
    group_id smallint    NOT NULL DEFAULT 1,
    member_id text       NOT NULL,
    conn_ip   text       NOT NULL DEFAULT ''
);
CREATE INDEX account_logout_log_date ON account_logout (log_date);

-- RESOURCE.INSTANCEHISTORY is two columns in the original (INSTANCE_ID, DATE) and it is the
-- instance LOCK: an id that is on this list may not be started again. Nothing writes it yet -
-- this server has no instance maps - so it stays empty until that lane exists.
CREATE TABLE instance_history (
    instance_id integer     NOT NULL PRIMARY KEY,
    date        timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE instance_history;
DROP TABLE account_logout;
DROP TABLE account_login;
DROP TABLE session_count_zone;
