r"""backup_db - dump the LIVE PostgreSQL database and prove the dump is whole.

Why this exists: the database is the only part of this project that is not in
git. `OPERATIONS.md` requires a fresh backup before any run that WRITES, and
"it said success" is not a backup.

The live data is PostgreSQL - container `granado_pg`, database `ge_data` (the
SQL Server container `granado_espada` has been stopped since the move, so the
sqlcmd/BACKUP path this file used to drive only ever answered "container is not
running"). This tool dumps `ge_data` in pg_dump's CUSTOM format, then asks
`pg_restore -l` what the file actually contains: its count of `TABLE DATA`
entries must equal the number of tables schema `public` has in the LIVE
database. A dump missing tables is the failure mode worth catching - it looks
exactly like a good one until the day it is restored - so a mismatch is a loud
failure (exit 1) that prints both numbers.

    cd src
    python backup_db.py                        # ..\backup\pg\ge_data_<date>_manual.dump
    python backup_db.py --reason pre_login     # the reason goes in the file name
    python backup_db.py --check                # is the newest dump younger than 24 h?
    python backup_db.py --list                 # what backup\pg holds

Exit code is non-zero unless a dump on the HOST verified, so a script can depend
on it:  python backup_db.py && python zone_server.py

`--check` reads a sidecar JSON written beside each dump (`<name>.dump.json`
holds its table count, size and time), so doctor.py can ask without touching
Docker. A dump with no sidecar - an older, hand-made one - is measured with
`pg_restore -l` and reported as "measured now".
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
from typing import NoReturn

from ge_paths import BACKUP

# The live database. Constants on purpose: this tool backs up ONE database and
# every error message names it. `--db` exists for the wrong-name case only.
CONTAINER = "granado_pg"
DATABASE = "ge_data"
PG_USER = "granado"
SCHEMA = "public"

DUMP_DIR = os.path.join(BACKUP, "pg")
KEEP = 5                 # a dump is the only undo for a DB write; keep five
MAX_AGE_HOURS = 24.0     # what --check calls fresh
MIN_BYTES = 4096         # a custom-format dump of this database is ~100 KB
MAGIC = b"PGDMP"         # every pg_dump custom-format archive starts with this


class BackupError(RuntimeError):
    """A failure whose message already names the thing that is wrong."""


# --------------------------------------------------------------------------
# talking to docker - no shell, no pipes: file handles and argument lists only
# --------------------------------------------------------------------------

def _docker(args, **kw):
    try:
        return subprocess.run(["docker"] + args, capture_output=True, text=True,
                              encoding="utf-8", errors="replace", **kw)
    except FileNotFoundError:
        raise BackupError("docker is not on PATH - is Docker Desktop running?")


def require_container():
    """Fail naming the container, never a container id."""
    p = _docker(["inspect", "--format", "{{.State.Running}}", CONTAINER])
    if p.returncode != 0:
        raise BackupError("%s is not running (docker ps): no such container. "
                          "Start it with  docker start %s" % (CONTAINER, CONTAINER))
    if p.stdout.strip().lower() != "true":
        raise BackupError("%s is not running (docker ps). "
                          "Start it with  docker start %s" % (CONTAINER, CONTAINER))


def require_database(db):
    """Fail naming the database asked for AND the ones the container has."""
    p = _docker(["exec", "-i", CONTAINER, "psql", "-U", PG_USER, "-d", "postgres",
                 "-tAc", "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1"])
    if p.returncode != 0:
        raise BackupError("cannot list the databases of %s (psql rc=%d): %s"
                          % (CONTAINER, p.returncode,
                             ((p.stderr or p.stdout).strip().splitlines() or ["no output"])[0][:200]))
    names = [n.strip() for n in p.stdout.splitlines() if n.strip()]
    if db not in names:
        raise BackupError("database %r does not exist in %s (it has: %s)"
                          % (db, CONTAINER, ", ".join(names) or "nothing"))
    return names


def live_table_count(db):
    """How many tables the live database has in schema `public`."""
    sql = ("SELECT count(*) FROM information_schema.tables "
           "WHERE table_schema = '%s'" % SCHEMA)
    p = _docker(["exec", "-i", CONTAINER, "psql", "-U", PG_USER, "-d", db, "-tA", "-c", sql])
    if p.returncode != 0:
        raise BackupError("cannot count the tables of %r in %s: %s"
                          % (db, CONTAINER,
                             ((p.stderr or p.stdout).strip().splitlines() or ["no output"])[0][:200]))
    try:
        return int(p.stdout.strip())
    except ValueError:
        raise BackupError("psql answered %r when asked how many tables %r has"
                          % (p.stdout.strip()[:80], db))


# --------------------------------------------------------------------------
# reading a dump back
# --------------------------------------------------------------------------

def archive_summary(path):
    """(database the archive records, count of TABLE DATA entries in `public`).

    `pg_restore -l` needs no database and no password: it reads the archive and
    prints its table of contents. The file goes in on stdin through a real file
    handle - never a shell pipe.
    """
    with open(path, "rb") as fh:
        head = fh.read(len(MAGIC))
    if head != MAGIC:
        raise BackupError("%s is not a pg_dump custom-format archive (first bytes %r)"
                          % (path, head))
    with open(path, "rb") as fh:
        p = subprocess.run(["docker", "exec", "-i", CONTAINER, "pg_restore", "-l"],
                           stdin=fh, capture_output=True, text=True,
                           encoding="utf-8", errors="replace")
    if p.returncode != 0:
        raise BackupError("pg_restore -l cannot read %s (rc=%d): %s"
                          % (path, p.returncode,
                             ((p.stderr or p.stdout).strip().splitlines() or ["no output"])[0][:200]))
    dbname, tables = None, 0
    for line in p.stdout.splitlines():
        m = re.match(r"^;\s+dbname:\s*(\S+)", line)
        if m:
            dbname = m.group(1)
        if re.search(r"\bTABLE DATA %s\b" % SCHEMA, line):
            tables += 1
    return dbname, tables


# --------------------------------------------------------------------------
# the dump itself
# --------------------------------------------------------------------------

def slug(text):
    """A file-name-safe, short tag: the database and the reason."""
    s = re.sub(r"[^A-Za-z0-9_.-]+", "_", (text or "").strip()).strip("._-")
    return s[:40] or "manual"


def dump_name(db, reason, folder=None):
    """`<db>_<YYYY-MM-DD>_<reason>.dump`, with -2/-3 if that name is taken."""
    stem = "%s_%s_%s" % (slug(db), time.strftime("%Y-%m-%d"), slug(reason))
    folder = folder or DUMP_DIR
    name, n = stem + ".dump", 1
    while os.path.exists(os.path.join(folder, name)):
        n += 1
        name = "%s-%d.dump" % (stem, n)
    return name


def dumps(folder=None):
    """The dumps in `folder`, newest first: (path, bytes, mtime).

    Only `*.dump` is ever listed, so a `.bak` under `backup\\` can never be
    listed as a dump, pruned, or touched by this tool at all.
    """
    folder = folder or DUMP_DIR
    if not os.path.isdir(folder):
        return []
    rows = []
    for name in os.listdir(folder):
        path = os.path.join(folder, name)
        if name.endswith(".dump") and os.path.isfile(path):
            rows.append((path, os.path.getsize(path), os.path.getmtime(path)))
    return sorted(rows, key=lambda r: r[2], reverse=True)


def _record(path, db, tables, seconds):
    """Write the sidecar `--check` reads: what this dump was and what it holds."""
    meta = {
        "dump": os.path.basename(path),
        "database": db,
        "container": CONTAINER,
        "schema": SCHEMA,
        "tables": tables,
        "bytes": os.path.getsize(path),
        "taken": time.strftime("%Y-%m-%d %H:%M:%S %z"),
        "seconds": round(seconds, 2),
        "format": "pg_dump custom (-Fc)",
    }
    tmp = path + ".json.tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(meta, fh, indent=2)
        fh.write("\n")
    os.replace(tmp, path + ".json")
    return meta


def _discard(path, why) -> NoReturn:
    """Remove a dump THIS run just wrote, because it is not a backup."""
    try:
        os.remove(path)
        gone = " (the file was removed)"
    except OSError:
        gone = " (the file could not be removed - delete it by hand)"
    raise BackupError("%s%s" % (why, gone))


def take(reason="manual", db=DATABASE, keep=KEEP):
    """Dump `db` to backup\\pg, verify it, record it, prune older dumps.

    Returns (path, meta). Raises BackupError - with a message that names what
    is wrong - unless the dump on the HOST verifies.
    """
    require_container()
    require_database(db)
    live = live_table_count(db)
    if live <= 0:
        raise BackupError("database %r in %s has no tables in schema %r - "
                          "that is not the game database" % (db, CONTAINER, SCHEMA))

    os.makedirs(DUMP_DIR, exist_ok=True)
    name = dump_name(db, reason)
    path = os.path.join(DUMP_DIR, name)
    started = time.time()
    with open(path, "wb") as fh:
        p = subprocess.run(["docker", "exec", "-i", CONTAINER, "pg_dump",
                            "-U", PG_USER, "-d", db, "-Fc"],
                           stdout=fh, stderr=subprocess.PIPE, text=True,
                           encoding="utf-8", errors="replace")
    if p.returncode != 0:
        detail = (p.stderr or "").strip().splitlines()
        _discard(path, "pg_dump failed on %s/%s (rc=%d): %s"
                       % (CONTAINER, db, p.returncode,
                          detail[0][:200] if detail else "no stderr"))
    elapsed = time.time() - started

    size = os.path.getsize(path)
    if size < MIN_BYTES:
        _discard(path, "the dump on the host is only %d bytes - that is not a "
                       "dump of %r in %s" % (size, db, CONTAINER))

    try:
        dumped_db, dumped_tables = archive_summary(path)
    except BackupError as exc:
        _discard(path, str(exc))

    if dumped_db and dumped_db != db:
        _discard(path, "the dump records database %r, not %r (%s, from %s)"
                       % (dumped_db, db, name, CONTAINER))

    if dumped_tables != live:
        _discard(path,
                 "VERIFY FAILED: %s carries %d TABLE DATA entr%s, but %r in %s "
                 "reports %d table(s) in schema %r.\n"
                 "         A dump missing tables is not a backup - do not restore "
                 "from it."
                 % (name, dumped_tables, "y" if dumped_tables == 1 else "ies",
                    db, CONTAINER, live, SCHEMA))

    meta = _record(path, db, dumped_tables, elapsed)
    if keep:
        prune(keep)
    return path, meta


def prune(keep=KEEP, folder=None):
    """Keep the newest `keep` dumps in `folder`; return the paths removed.

    Only `*.dump` files are candidates. A `.bak` under `backup\\` is history
    from the SQL Server days and is never listed, let alone deleted.
    """
    folder = folder or DUMP_DIR
    removed = []
    if not keep or keep <= 0:
        return removed
    for path, _size, _mtime in dumps(folder)[keep:]:
        try:
            os.remove(path)
        except OSError as exc:
            print("could not prune %s (%s)" % (path, exc))
            continue
        sidecar = path + ".json"
        if os.path.isfile(sidecar):
            try:
                os.remove(sidecar)
            except OSError:
                pass
        removed.append(path)
        print("pruned:", path)
    return removed


# --------------------------------------------------------------------------
# the cheap question: is there a fresh dump?
# --------------------------------------------------------------------------

def newest():
    """The newest dump as a dict, or None.

    `tables`/`database` come from the sidecar when one exists (no Docker call
    at all), else from `pg_restore -l` ("measured now") so a dump taken before
    this tool wrote sidecars still answers.
    """
    rows = dumps()
    if not rows:
        return None
    path, size, mtime = rows[0]
    info = {"path": path, "bytes": size, "mtime": mtime,
            "age_hours": max(0.0, (time.time() - mtime) / 3600.0),
            "tables": None, "database": None, "source": None}
    sidecar = path + ".json"
    if os.path.isfile(sidecar):
        try:
            with open(sidecar, encoding="utf-8") as fh:
                meta = json.load(fh)
            info["tables"] = int(meta["tables"])
            info["database"] = meta.get("database")
            info["source"] = "recorded in %s" % os.path.basename(sidecar)
        except (OSError, ValueError, KeyError, TypeError):
            pass                      # a broken sidecar is not a refusal
    if info["tables"] is None:
        db, tables = archive_summary(path)
        info["tables"] = tables
        info["database"] = db
        info["source"] = "measured now (no sidecar)"
    return info


def freshness(max_age_hours=MAX_AGE_HOURS):
    """(fresh, one-line summary) for doctor.py. Cheap: file reads only."""
    info = newest()
    if info is None:
        return False, "no dump in %s - take one:  python backup_db.py" % DUMP_DIR
    fresh = info["age_hours"] <= max_age_hours
    return fresh, "%s - %.1f h old, %d table(s) (%s)" % (
        info["path"], info["age_hours"], info["tables"], info["source"])


# --------------------------------------------------------------------------
# command line
# --------------------------------------------------------------------------

def do_check(max_age_hours):
    info = newest()
    if info is None:
        print("no dump in %s" % DUMP_DIR)
        print("take one:  python backup_db.py")
        return 1
    print("newest dump : %s" % info["path"])
    print("size        : %d bytes (%.1f KB)" % (info["bytes"], info["bytes"] / 1024.0))
    print("age         : %.1f h (%s)"
          % (info["age_hours"], time.strftime("%Y-%m-%d %H:%M",
                                              time.localtime(info["mtime"]))))
    print("tables      : %d in schema %s (%s)"
          % (info["tables"], SCHEMA, info["source"]))
    if info["database"]:
        print("database    : %s in %s" % (info["database"], CONTAINER))
    if info["age_hours"] <= max_age_hours:
        print("status      : FRESH (limit %g h)" % max_age_hours)
        return 0
    print("status      : STALE - %.1f h old, limit %g h" % (info["age_hours"], max_age_hours))
    print("take one:  python backup_db.py")
    return 1


def do_list():
    rows = dumps()
    if not rows:
        print("no dump in %s" % DUMP_DIR)
        return 0
    for path, size, mtime in rows:
        tables = "?"
        sidecar = path + ".json"
        if os.path.isfile(sidecar):
            try:
                with open(sidecar, encoding="utf-8") as fh:
                    tables = str(json.load(fh).get("tables", "?"))
            except (OSError, ValueError):
                pass
        print("%-46s %7.1f KB  %s  %3s table(s)"
              % (os.path.basename(path), size / 1024.0,
                 time.strftime("%Y-%m-%d %H:%M", time.localtime(mtime)), tables))
    print("%d dump(s) in %s" % (len(rows), DUMP_DIR))
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(
        description="Back up the live PostgreSQL game database (granado_pg/ge_data), verified.")
    ap.add_argument("--db", default=DATABASE,
                    help="database to dump (default: %(default)s - the live one)")
    ap.add_argument("--reason", default="manual",
                    help="short tag for the file name, e.g. pre_login (default: manual)")
    ap.add_argument("--keep", type=int, default=KEEP,
                    help="dumps to keep in backup\\pg (default: %(default)s)")
    ap.add_argument("--no-prune", action="store_true", help="delete no dump, keep them all")
    ap.add_argument("--check", action="store_true",
                    help="print the newest dump's age and table count; exit 1 if older "
                         "than --max-age-hours")
    ap.add_argument("--max-age-hours", type=float, default=MAX_AGE_HOURS,
                    help="what --check calls fresh (default: %(default)s)")
    ap.add_argument("--list", action="store_true", help="list the dumps in backup\\pg")
    args = ap.parse_args(argv)

    if args.check:
        return do_check(args.max_age_hours)
    if args.list:
        return do_list()

    try:
        path, meta = take(args.reason, args.db, keep=0 if args.no_prune else args.keep)
    except BackupError as exc:
        print("FAILED:", exc)
        return 1
    except KeyboardInterrupt:
        print("FAILED: interrupted")
        return 1

    print("backup verified: %s" % path)
    print("  %s, %.1f KB, %d table(s) in schema %s, verified with pg_restore -l"
          % (meta["taken"], meta["bytes"] / 1024.0, meta["tables"], SCHEMA))
    return 0


if __name__ == "__main__":
    sys.exit(main())
