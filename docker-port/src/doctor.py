r"""doctor - preflight check before running a server.

Run from src\:

    cd src
    python doctor.py

It only LOOKS; it changes nothing. Checks, in order:
  1. the LIVE database - container `granado_pg` is up, and one trivial query
     answers through the SAME DSN the Go server uses (`GE_PG_DSN` when set,
     else `DefaultDSN` in goserver\db\db.go). psql runs inside the container -
     the shape backup_db.py uses - so the password is never printed
  2. the SQL Server container `granado_espada` - WARN only. It holds the
     ORIGINAL world data and feeds the Python oracle; it is normally stopped and
     that does not stop the Go server
  3. ports 7001 (barrack) and 7002 (zone) are free
  4. the read-only inputs exist (captures\, config\, data\, ctables.pkl, and the
     game files the scripts read)
  5. the Go binaries are NEWER than their sources (an old `out\goserver.exe`
     silently tests yesterday's code)
  6. the derived files are fresh - Lua corpus, item_props/item_stats - asked of
     prepare.py's own check, so the two can never disagree
  7. the newest PostgreSQL dump is younger than 24 h (backup_db.py's own
     `--check`, by sidecar - WARN, because a stale dump does not block a
     read-only run)

Exit 0 = ready to run. Exit 1 = at least one blocking problem.
"""

import os
import re
import socket
import subprocess
import sys
from urllib.parse import unquote, urlsplit

import backup_db
import ge_paths
import prepare

SRC = os.path.dirname(os.path.abspath(__file__))
GAME = os.path.join(ge_paths.GAME_ROOT, "GEAndromida")
GO = os.path.join(ge_paths.ROOT, "goserver")
DB_GO = os.path.join(GO, "db", "db.go")

# postgres:16 listens here INSIDE the container; the DSN's port (5433) is the
# HOST's published mapping. Both hops are checked, against different sockets.
PG_INNER_PORT = 5432
PG_QUERY = "SELECT current_database(), current_user"

results = []


def add(level, msg):
    results.append((level, msg))


def container_state(name):
    """(state, status, published ports) for `name`, or (None, None, None).

    Exact match in Python: the CLI's `--filter name=` is a substring match, and
    `granado_pg` must never be answered by a differently-named container. The
    invocation shape is backup_db's - one way to call docker, not two.
    """
    p = backup_db._docker(["ps", "-a", "--format",
                           "{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Ports}}"])
    if p.returncode != 0:
        raise backup_db.BackupError(
            "cannot ask docker about %s (rc=%d): %s"
            % (name, p.returncode,
               ((p.stderr or p.stdout or "").strip().splitlines() or ["no output"])[0][:200]))
    for line in p.stdout.splitlines():
        fields = line.split("\t") + [""] * 3
        if fields[0].strip() == name:
            return fields[1].strip().lower(), fields[2].strip(), fields[3].strip()
    return None, None, None


def live_dsn():
    """(dsn, where) - the connection string the Go server actually uses.

    The string carries the password, so it is never printed: only what the DSN
    names (user@host:port/database) and where it came from reach the output.
    """
    try:
        with open(DB_GO, encoding="utf-8") as fh:
            go = fh.read()
    except OSError as exc:
        raise backup_db.BackupError("cannot read the server's DSN from %s (%s)"
                                    % (DB_GO, exc))
    m = re.search(r'EnvDSN\s*=\s*"([^"]+)"', go)
    env_name = m.group(1) if m else "GE_PG_DSN"
    value = os.environ.get(env_name)
    if value:
        return value, "env " + env_name
    m = re.search(r'DefaultDSN\s*=\s*"([^"]+)"', go)
    if not m:
        raise backup_db.BackupError('%s has no `DefaultDSN = "..."` line - doctor cannot '
                                    "tell which database the server uses" % DB_GO)
    return m.group(1), r"goserver\db\db.go DefaultDSN"


def parse_dsn(dsn, where):
    """(host, port, user, password, database) from a postgres:// DSN."""
    parts = urlsplit(dsn)
    if parts.scheme not in ("postgres", "postgresql"):
        raise backup_db.BackupError("the DSN from %s is not a postgres:// URL (scheme %r)"
                                    % (where, parts.scheme or "none"))
    host = parts.hostname or "127.0.0.1"
    try:
        port = parts.port or PG_INNER_PORT
    except ValueError:
        raise backup_db.BackupError("the DSN from %s has an invalid port" % where)
    user = unquote(parts.username or "") or backup_db.PG_USER
    password = unquote(parts.password or "")
    db = unquote((parts.path or "").lstrip("/")) or backup_db.DATABASE
    return host, port, user, password, db


def dsn_query(host, port, user, password, db, local, timeout=15):
    """One trivial query through the DSN: (output, error). Raises nothing.

    A LOCAL DSN is answered over TCP by the container's own server on
    PG_INNER_PORT, so the DSN's user, database and a live server are all really
    exercised; the published host port is probed first, because that is the hop
    the Go server takes and a container can be up with its port unmapped. The
    password goes in through `docker exec -e PGPASSWORD`, copied from this
    process's environment - never an argument. Note what that does NOT prove:
    the container's pg_hba trusts loopback, so a wrong password still connects
    from inside; this check proves reachability and target, not the credential.
    """
    if local:
        try:
            socket.create_connection((host, port), 2).close()
        except OSError as exc:
            return None, ("nothing accepts TCP on %s:%d (%s)"
                          % (host, port, exc.strerror or exc))
        psql_where = ["-h", "127.0.0.1", "-p", str(PG_INNER_PORT)]
    else:
        psql_where = ["-h", host, "-p", str(port)]
    env = dict(os.environ)
    args = ["exec", "-i"]
    if password:
        args += ["-e", "PGPASSWORD"]
        env["PGPASSWORD"] = password
    args += ([backup_db.CONTAINER, "psql"] + psql_where
             + ["-U", user, "-d", db, "-tA", "-c", PG_QUERY])
    try:
        p = backup_db._docker(args, env=env, timeout=timeout)
    except backup_db.BackupError as exc:
        return None, str(exc)
    except subprocess.TimeoutExpired:
        return None, "psql did not answer within %d s" % timeout
    if p.returncode != 0:
        line = ((p.stderr or p.stdout or "").strip().splitlines() or ["no output"])[0]
        return None, "psql rc=%d: %s" % (p.returncode, line[:200])
    return p.stdout.strip(), None


def check_live_db():
    """The live database: container up AND the server's own DSN answers.

    A running container is not a connection. Two lines on purpose: a stopped
    container has one fix (`docker start granado_pg`), while a DSN that cannot
    be reached or authenticated has another - and that failure names the target
    (user@host:port/database) and where the DSN came from. Constants, psql
    invocation and error wording follow backup_db.py, the tool that already
    talks to this database. Read-only: one SELECT.
    """
    try:
        dsn, where = live_dsn()
        host, port, user, password, db = parse_dsn(dsn, where)
    except backup_db.BackupError as exc:
        add("FAIL", str(exc))
        return
    target = "%s@%s:%d/%s" % (user, host, port, db)
    local = host in ("localhost", "127.0.0.1", "::1")

    if local:
        try:
            state, status, ports = container_state(backup_db.CONTAINER)
        except backup_db.BackupError as exc:
            add("FAIL", str(exc))
            return
        if state is None:
            add("FAIL", "container %s does not exist (docker ps -a) - database %s is "
                        "expected on port %d;  docker start %s"
                % (backup_db.CONTAINER, db, port, backup_db.CONTAINER))
        elif state != "running":
            add("FAIL", "container %s is NOT running (%s). Start it with  docker start %s"
                % (backup_db.CONTAINER, status or state, backup_db.CONTAINER))
        elif "unhealthy" in (status or "").lower():
            add("warn", "container %s is up but unhealthy (%s) - port %d, database %s"
                % (backup_db.CONTAINER, status, port, db))
        else:
            add("ok", "container %s: %s - port %d, database %s (%s)"
                % (backup_db.CONTAINER, status, port, db,
                   ports or "port not published"))
    else:
        add("ok", "DSN target is remote (%s) - the local %s container is not used"
            % (host, backup_db.CONTAINER))

    got, err = dsn_query(host, port, user, password, db, local)
    if err:
        add("FAIL", "live database %s (%s) is unreachable: %s" % (target, where, err))
        return
    fields = [f.strip() for f in got.split("|")] + ["", ""]
    if fields[0] != db or fields[1] != user:
        add("FAIL", "%s (%s) answered as %r on %r, but the DSN names %r as %r"
            % (target, where, fields[1], fields[0], user, db))
        return
    add("ok", "connectivity: %s answered %s via psql in %s (DSN: %s)"
        % (target, PG_QUERY, backup_db.CONTAINER, where))


def check_reference_sqlserver():
    """The SQL Server is reference/oracle data - its being stopped is NOT a failure.

    `granado_espada` holds the ORIGINAL world data and feeds the Python oracle;
    the Go server never dials it (history\\postgres-migration.md). One line, WARN
    when it is down, so a preflight cannot fail on it again.
    """
    try:
        state, status, _ports = container_state("granado_espada")
    except backup_db.BackupError as exc:
        add("warn", "SQL Server (reference/oracle): %s" % exc)
        return
    if state == "running":
        add("ok", "SQL Server (reference/oracle) is running (%s) - only the Python "
                  "oracle needs it" % (status or "granado_espada"))
    else:
        add("warn", "SQL Server (reference/oracle) is not running - only the Python "
                    "oracle needs it:  docker start granado_espada")


def check_port(port, who):
    s = socket.socket()
    s.settimeout(0.5)
    try:
        s.connect(("127.0.0.1", port))
        busy = True
    except OSError:
        busy = False
    finally:
        s.close()
    if busy:
        add("warn", "port %d (%s) is ALREADY IN USE - only one server at a time"
                    % (port, who))
    else:
        add("ok", "port %d (%s) is free" % (port, who))


def check_inputs():
    items = [
        ("captures", ge_paths.CAPTURES, "dir"),
        ("config", ge_paths.CONFIG, "dir"),
        ("data", ge_paths.DATA, "dir"),
        ("ctables.pkl", os.path.join(SRC, "ctables.pkl"), "file"),
        ("bf_tables.pkl", os.path.join(SRC, "bf_tables.pkl"), "file"),
    ]
    for label, path, kind in items:
        exists = os.path.isdir(path) if kind == "dir" else os.path.isfile(path)
        add("ok" if exists else "FAIL",
            "%s: %s%s" % (label, path, "" if exists else "   (MISSING)"))
    ok = os.path.isdir(GAME)
    add("ok" if ok else "warn",
        "game files: %s%s" % (GAME, "" if ok else "   (missing - some scripts read it)"))


def check_build():
    """The binaries are BUILD ARTEFACTS: an old one tests yesterday's code.

    `out\\goserver.exe` is what `run_barrack.cmd` launches, and nothing rebuilt
    it for you. Compare it against the newest Go source; a test file counts,
    because a source tree newer than the binary is a rebuild nobody did.
    """
    sources = [os.path.join(d, f) for d, _s, fs in os.walk(GO)
               for f in fs if f.endswith(".go")]
    gomod = os.path.join(GO, "go.mod")
    if os.path.isfile(gomod):
        sources.append(gomod)
    if not sources:
        add("warn", "goserver\\ not found - cannot tell whether out\\goserver*.exe "
                    "is current")
        return
    newest = max(sources, key=os.path.getmtime)
    newest_name = "goserver\\" + os.path.relpath(newest, GO).replace("/", "\\")
    for exe in ("goserver.exe", "goserver_zone.exe"):
        path = os.path.join(ge_paths.OUT, exe)
        if not os.path.isfile(path):
            add("FAIL", "out\\%s is MISSING - build it:  cd goserver   "
                        "go build -o ..\\out\\%s ." % (exe, exe))
        elif os.path.getmtime(path) < os.path.getmtime(newest):
            add("warn", "out\\%s is OLDER than %s - rebuild:  cd goserver   "
                        "go build -o ..\\out\\%s ." % (exe, newest_name, exe))
        else:
            add("ok", "out\\%s is newer than the newest source (%s)"
                % (exe, newest_name))


def check_derived():
    """Generated files (Lua corpus, item tables) - stale means wrong numbers.

    prepare.py owns that question; this only prints its one-line summary, so a
    stale corpus and a stale item table cannot be judged two different ways.
    """
    try:
        stale, summary = prepare.check(verbose=False)
    except Exception as exc:              # a broken DERIVED row must not hide
        add("warn", "derived: check failed (%s) - run:  python prepare.py" % exc)
        return
    add("ok" if not stale else "warn", "derived: " + summary)


def check_backup():
    """Is there a fresh PostgreSQL dump? `backup_db --check`, asked cheaply.

    WARN, not FAIL: this preflight decides whether a server can RUN, and a
    stale dump does not block a read-only session - but it must be visible
    before one that WRITES. `freshness()` reads a sidecar and touches no
    Docker when the dump was taken by the current tool.
    """
    try:
        fresh, line = backup_db.freshness()
    except Exception as exc:              # a broken dump must not hide the rest
        add("warn", "backup: cannot read the newest dump (%s)" % exc)
        return
    add("ok" if fresh else "warn", "backup: " + line)


def check_input_manifest():
    """Do the files this project is BUILT FROM still match `data\inputs.json`?

    `check_inputs.py` owns the question (a manifest of hashes with what each
    artefact feeds); this only asks it and prints one line, so a changed input
    cannot be judged two different ways. WARN, not FAIL: a drifted input does not
    stop a server from running - it means a number measured earlier may have moved.
    """
    try:
        import check_inputs

        entries, failures = check_inputs.check(
            check_inputs.REPO_ROOT, check_inputs.REPO_ROOT / check_inputs.DEFAULT_MANIFEST
        )
    except Exception as exc:              # a broken manifest must not hide the rest
        add("warn", "inputs: cannot check the manifest (%s) - run:  python check_inputs.py" % exc)
        return
    if failures:
        add("warn", "inputs: %d problem(s) in data\\inputs.json - %s (run:  python check_inputs.py)"
            % (len(failures), failures[0]))
        return
    add("ok", "inputs: %d artefact(s) in data\\inputs.json match their hashes" % len(entries))


def main():
    print("doctor - preflight for the docker-port server\n")
    check_live_db()
    check_reference_sqlserver()
    check_port(7001, "barrack")
    check_port(7002, "zone")
    check_inputs()
    check_build()
    check_derived()
    check_input_manifest()
    check_backup()
    print()
    for level, msg in results:
        print("%-5s %s" % (level, msg))
    bad = [m for l, m in results if l == "FAIL"]
    print()
    if bad:
        print("%d blocking problem(s). Fix them before running a server." % len(bad))
        return 1
    # The servers are the Go binaries. This line used to send the user to
    # `python ge_barrack_server.py` / `python zone_server.py`; those are probes
    # and serve nothing the client sees (DECISIONS.md, "PYTHON PROBES, GO
    # SERVES"), so it was the one piece of stale advice in a preflight.
    print("ready. Build:  cd goserver   go build -o ..\\out\\goserver.exe .")
    print("       Run:    out\\goserver.exe -addr :7001 -zone 127.0.0.1:7003 "
          "(the zone line is in OPERATIONS.md)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
