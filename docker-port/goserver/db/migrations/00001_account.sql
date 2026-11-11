-- +goose Up
-- The login/barrack slice, in PostgreSQL types.
--
-- Mirrors the legacy tables, because the row data will be moved from them
-- later and the columns must line up. Legacy source (SQL Server):
--   GE_DATA.ACCOUNT_INFO    ACCOUNT_ID, MEMBER_ID, COMMANDER_ID, CLASS, SPECIAL_IP
--   RESOURCE.ACCOUNT_INFO   ..., PASS (MD5 hex), CUMULATE_COMM, CREATE_DATE
--
-- DELIBERATE DEVIATION: the password lives in this database for now, although
-- the legacy engine keeps it in the RESOURCE database. The two-database split
-- is still an OPEN decision (history\postgres-migration.md); when it is
-- settled, this table moves to ge_resource and the migration splits with it.
-- Keeping it in one place until then means the login can be tested end to end.
--
-- No MSSQL-isms: no IDENTITY, no GETDATE(), no nvarchar, no stored procedure.

CREATE TABLE account_info (
    account_id   integer      PRIMARY KEY,
    member_id    varchar(20)  NOT NULL UNIQUE,   -- the login id the client sends
    commander_id integer      NOT NULL,          -- the barrack's commander
    class        smallint     NOT NULL DEFAULT 0,
    special_ip   smallint     NOT NULL DEFAULT 0
);

CREATE TABLE account_pass (
    member_id     varchar(20)  PRIMARY KEY,
    pass_md5      varchar(50)  NOT NULL DEFAULT '-',   -- MD5 hex, as legacy stores it
    cumulate_comm bigint       NOT NULL DEFAULT 0,
    created_at    timestamptz  NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE account_pass;
DROP TABLE account_info;
