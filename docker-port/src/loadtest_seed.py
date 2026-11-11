"""loadtest_seed.py - a SCRATCH database plus N test accounts, for a load test.

WHY THIS FILE EXISTS
A load test needs many bots the zone will accept, but the live database holds
the owner's real characters and must not be touched. So this copies the SHAPE,
not the data: `pg_dump --schema-only ge_data` copies every table into a scratch
database `ge_loadtest`, and then N fake accounts are inserted there. The live
database is read for its schema only - never its rows - and never written.

WHAT ONE BOT IS
The zone answers CZ_START_GAME by asking PostgreSQL for the account's team
(zone\\store.go, TeamOf): account_info -> commander_id -> character rows. So a
bot is one account_info row, one account_pass row, one commander and one
character. Worn items are left EMPTY on purpose: the zone handles a character
with nothing worn, and gear is not what a connection load test measures.

THE GOOSE LEDGER
The zone applies its own migrations at start (main.go, db.Migrate). If the
scratch database had every table from the dump but an empty goose_db_version,
goose would try to CREATE TABLE account_info again and the zone would refuse to
start. So the ledger rows travel with the schema. That is one bookkeeping table
copied through pg_dump - the same read-only way the schema arrives - and no
game row is ever copied.

HARD RAILS
- the target database name must be exactly `ge_loadtest`; anything else refuses
- ge_data is READ ONLY, and only through pg_dump (never psql against it)
- a .lock file in out\\loadtest\\ stops two runs applying at once
- re-running deletes and re-creates only this prefix's rows (idempotent)

Usage (from src\\):
    python loadtest_seed.py                 # 10 bots, prefix lt, applied
    python loadtest_seed.py --count 50
    python loadtest_seed.py --dry           # write the .sql, apply nothing
"""
import argparse
import csv
import os
import re
import subprocess
import sys

import ge_paths

CONTAINER = "granado_pg"
PG_USER = "granado"
SOURCE_DB = "ge_data"      # read ONLY, and only through pg_dump
TARGET_DB = "ge_loadtest"  # the guard below is what makes this safe

# Bot ids live in a block nothing else uses. The legacy accounts are small
# numbers, so 900000 is clear of them; a clash is refused, never overwritten.
BOT_ID_BASE = 900000
MAX_BOTS = 1000

OUT_DIR = os.path.join(ge_paths.ROOT, "out", "loadtest")
LOCK_PATH = os.path.join(OUT_DIR, ".lock")
SQL_PATH = os.path.join(OUT_DIR, "seed.sql")
CSV_PATH = os.path.join(OUT_DIR, "accounts.csv")


def q(value):
    """Quote a value for SQL, or NULL. Same helper as seed_legacy_account.py."""
    if value is None:
        return "NULL"
    return "'%s'" % str(value).replace("'", "''")


def psql_cmd(db):
    """The one and only way this script talks to PostgreSQL.

    No driver and no password: `docker exec -i granado_pg psql` is the pattern
    seed_legacy_account.py uses, and it keeps what runs readable as plain SQL.
    """
    return ["docker", "exec", "-i", CONTAINER, "psql", "-U", PG_USER,
            "-d", db, "-v", "ON_ERROR_STOP=1", "-q"]


def guard_target(db):
    """Refuse anything that is not the scratch database."""
    if db != TARGET_DB or db != "ge_loadtest":
        raise SystemExit("refusing to run: target database is %r, not 'ge_loadtest'" % db)
    if db == SOURCE_DB:
        raise SystemExit("refusing to run: target database is the live one")


def psql_text(db, sql):
    """Run SQL and return psql's -tA output (one value per line)."""
    cmd = psql_cmd(db) + ["-tA", "-c", sql]
    proc = subprocess.run(cmd, text=True, encoding="utf-8", errors="replace", capture_output=True)
    if proc.returncode != 0:
        raise SystemExit("psql failed on %s:\n%s\n%s"
                         % (db, proc.stdout.strip(), proc.stderr.strip()))
    return proc.stdout.strip()


def psql_apply(db, sql):
    """Apply SQL text to db. Used for the schema dump and for the seed file."""
    guard_target(db)
    proc = subprocess.run(psql_cmd(db), input=sql, text=True,
                          encoding="utf-8", errors="replace", capture_output=True)
    if proc.returncode != 0:
        raise SystemExit("psql failed applying to %s:\n%s\n%s"
                         % (db, proc.stdout.strip(), proc.stderr.strip()))
    return proc.stdout


def pg_dump(args):
    """Run pg_dump inside the container and return its output as text."""
    cmd = ["docker", "exec", "-i", CONTAINER, "pg_dump", "-U", PG_USER] + args
    proc = subprocess.run(cmd, text=True, encoding="utf-8", errors="replace", capture_output=True)
    if proc.returncode != 0:
        raise SystemExit("pg_dump failed:\n%s" % proc.stderr.strip())
    return proc.stdout


def ensure_database():
    """Create ge_loadtest if it is not there. Returns True when it was created."""
    have = psql_text("postgres", "SELECT 1 FROM pg_database WHERE datname = %s"
                     % q(TARGET_DB))
    if have == "1":
        return False
    # The name is a constant, checked by the guard above, and no other database
    # is ever named in a CREATE. Quoted anyway: it is the one statement here
    # that cannot run inside a transaction.
    psql_text("postgres", 'CREATE DATABASE "%s"' % TARGET_DB)
    return True


def ensure_schema(created):
    """Load the live schema into the scratch database. Returns True if it did.

    The schema is `--schema-only`: table definitions, no rows. The goose ledger
    is the ONE exception and it is a bookkeeping table, not game data (see the
    module docstring for why the zone needs it).
    """
    if not created:
        # A half-built scratch database (created, dump failed) is repaired here
        # instead of being deleted: delete is destructive, and this is cheaper.
        have = psql_text(TARGET_DB, "SELECT to_regclass('public.account_info') IS NOT NULL")
        if have == "t":
            return False

    psql_apply(TARGET_DB, pg_dump(["--schema-only", SOURCE_DB]))
    psql_apply(TARGET_DB, pg_dump(["--data-only", "-t", "goose_db_version", SOURCE_DB]))
    return True


def bot_login(prefix, i):
    """The login id of bot i (0-based). Fits member_id varchar(20)."""
    return "%s%d" % (prefix, i + 1)


def build_sql(prefix, count):
    """The whole seed, as readable SQL. Nothing here is a driver call."""
    like = prefix + "%"
    out = [
        "-- Load-test seed written by src\\loadtest_seed.py\n"
        "-- Scratch database ONLY (ge_loadtest). Not the data migration.\n",
        "BEGIN;\n",
        "-- Delete this prefix's rows first, so a re-run replaces them instead of\n"
        "-- doubling them. Everything hanging off `commander` is ON DELETE CASCADE\n"
        "-- (character, wear_item, storage, team_group, commander_property,\n"
        "-- character_position, saved_teleport, saved_start_point,\n"
        "-- pending_zone_entry, character_property).\n",
        "DELETE FROM account_info WHERE member_id LIKE %s;\n" % q(like),
        "DELETE FROM account_pass WHERE member_id LIKE %s;\n" % q(like),
        "DELETE FROM commander    WHERE member_id LIKE %s;\n" % q(like),
    ]
    for i in range(count):
        bot = BOT_ID_BASE + i
        login = bot_login(prefix, i)
        out.append(
            "\n-- bot %d: %s\n"
            "INSERT INTO account_info (account_id, member_id, commander_id, class, special_ip)\n"
            "VALUES (%d, %s, %d, 0, 0)\n"
            "ON CONFLICT (account_id) DO UPDATE SET\n"
            "  member_id = EXCLUDED.member_id,\n"
            "  commander_id = EXCLUDED.commander_id,\n"
            "  class = EXCLUDED.class,\n"
            "  special_ip = EXCLUDED.special_ip;\n"
            "INSERT INTO account_pass (member_id, pass_md5, cumulate_comm)\n"
            "VALUES (%s, '-', 0)\n"
            "ON CONFLICT (member_id) DO UPDATE SET\n"
            "  pass_md5 = EXCLUDED.pass_md5,\n"
            "  cumulate_comm = EXCLUDED.cumulate_comm;\n"
            "INSERT INTO commander (commander_id, account_id, member_id, name, zone_id)\n"
            "VALUES (%d, %d, %s, %s, %d)\n"
            "ON CONFLICT (commander_id) DO UPDATE SET\n"
            "  account_id = EXCLUDED.account_id,\n"
            "  member_id = EXCLUDED.member_id,\n"
            "  name = EXCLUDED.name,\n"
            "  zone_id = EXCLUDED.zone_id;\n"
            # Job 1 is the Fighter, the one job this project has driven end to
            # end (its bare-hand stance resolves). exp 0 is level 1; hp/sp are
            # real current values so the client draws a non-empty bar.
            "INSERT INTO \"character\" (character_id, commander_id, slot, name, teampos,\n"
            "  gender, job, headtype, exp, life, hp, sp, iscard, pos_x, pos_y, pos_z)\n"
            "VALUES (%d, %d, 0, %s, 0, false, 1, 0, 0, 500, 500, 100, 0, 0, 0, 0)\n"
            "ON CONFLICT (character_id) DO UPDATE SET\n"
            "  commander_id = EXCLUDED.commander_id,\n"
            "  slot = EXCLUDED.slot,\n"
            "  name = EXCLUDED.name,\n"
            "  hp = EXCLUDED.hp,\n"
            "  sp = EXCLUDED.sp;\n"
            % (i + 1, login,
               bot, q(login), bot,
               q(login),
               bot, bot, q(login), q(login), 10,
               bot, bot, q(login))
        )
    out.append("\nCOMMIT;\n")
    return "".join(out)


def check_id_block(prefix, count):
    """Refuse to reuse the bot id block for rows that belong to someone else."""
    like = prefix + "%"
    rows = psql_text(
        TARGET_DB,
        "SELECT account_id || ' ' || member_id FROM account_info "
        "WHERE account_id BETWEEN %d AND %d AND member_id NOT LIKE %s"
        % (BOT_ID_BASE, BOT_ID_BASE + count - 1, q(like)))
    if rows:
        raise SystemExit(
            "refusing to run: bot id %d..%d already holds %s - "
            "these are a different prefix's rows, not this run's"
            % (BOT_ID_BASE, BOT_ID_BASE + count - 1, rows.replace("\n", ", ")))


def count_bots(prefix):
    like = prefix + "%"
    return psql_text(TARGET_DB, "SELECT count(*) FROM account_info WHERE member_id LIKE %s"
                     % q(like))


def write_csv(prefix, count):
    """One row per bot, in id order: what the Go load generator reads."""
    with open(CSV_PATH, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(["account_id", "commander_id", "login"])
        for i in range(count):
            w.writerow([BOT_ID_BASE + i, BOT_ID_BASE + i, bot_login(prefix, i)])


def acquire_lock():
    """One run at a time. O_EXCL is the claim: two runs cannot both win it."""
    try:
        fd = os.open(LOCK_PATH, os.O_CREAT | os.O_EXCL | os.O_WRONLY)
    except FileExistsError:
        try:
            with open(LOCK_PATH, encoding="utf-8") as f:
                held = f.read().strip()
        except OSError:
            held = "?"
        raise SystemExit(
            "refusing to run: %s exists (%s).\n"
            "Another run is applying, or a run died. If nothing is running, "
            "delete that file and re-run." % (LOCK_PATH, held))
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as f:
        f.write("pid %d\n" % os.getpid())
    return LOCK_PATH


def main(argv):
    ap = argparse.ArgumentParser(
        description="Seed the scratch database ge_loadtest with N load-test bots.")
    ap.add_argument("--count", type=int, default=10,
                    help="number of bots (default 10, max %d)" % MAX_BOTS)
    ap.add_argument("--prefix", default="lt",
                    help="member_id/login prefix, letters and digits (default lt)")
    ap.add_argument("--dry", action="store_true",
                    help="write the .sql file, apply nothing")
    a = ap.parse_args(argv[1:])

    if not 1 <= a.count <= MAX_BOTS:
        raise SystemExit("--count must be 1..%d" % MAX_BOTS)
    if not re.fullmatch(r"[a-z0-9]{1,8}", a.prefix):
        raise SystemExit("--prefix must be 1-8 lowercase letters or digits")

    guard_target(TARGET_DB)
    os.makedirs(OUT_DIR, exist_ok=True)

    if a.dry:
        sql = build_sql(a.prefix, a.count)
        with open(SQL_PATH, "w", encoding="utf-8", newline="\n") as f:
            f.write(sql)
        print("--dry: wrote %s (%d bot(s)); nothing applied"
              % (SQL_PATH, a.count))
        return 0

    lock = acquire_lock()
    try:
        created = ensure_database()
        loaded = ensure_schema(created)
        check_id_block(a.prefix, a.count)

        sql = build_sql(a.prefix, a.count)
        with open(SQL_PATH, "w", encoding="utf-8", newline="\n") as f:
            f.write(sql)
        psql_apply(TARGET_DB, sql)

        got = count_bots(a.prefix)
        if got != str(a.count):
            raise SystemExit("expected %d bot(s) in %s, found %s"
                             % (a.count, TARGET_DB, got))
        write_csv(a.prefix, a.count)
    finally:
        try:
            os.remove(lock)
        except OSError:
            pass

    print("ge_loadtest: %d bot(s) %s1..%s%d (ids %d..%d)%s%s -> %s"
          % (a.count, a.prefix, a.prefix, a.count,
             BOT_ID_BASE, BOT_ID_BASE + a.count - 1,
             ", database created" if created else "",
             ", schema loaded" if loaded else "",
             CSV_PATH))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
