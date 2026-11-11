"""pg_read - read-only PostgreSQL access for the parts of the Python server that
have to look at state the Go server now owns.

This is a BRIDGE, not the destination. The Python zone still reads the game data
from SQL Server (`ge_db`); the only thing it needs from PostgreSQL is the team the
Go barrack recorded when the player pressed New Game. When the zone itself is
ported to Go this module goes away with it - see DECISIONS.md.

Connections are opened per call and closed again: these reads happen once or twice
per login, not in a loop, and a pooled connection is one more thing to leak.

The DSN matches goserver\\db\\db.go so both halves talk to the same database:
GE_PG_DSN wins, otherwise the development default (the granado_pg container on
5433).

Usage:
    rows = pg_read.query("SELECT 1")
"""

import os

# Same default as goserver\db\db.go DefaultDSN (the password is `<local-dev-password>`,
# percent-encoded because `@` ends the userinfo part of a URL).
DEFAULT_DSN = "postgres://granado:Granado%400504@localhost:5433/ge_data?sslmode=disable"


def dsn():
    """The PostgreSQL connection string: GE_PG_DSN, else the dev default."""
    return os.environ.get("GE_PG_DSN") or DEFAULT_DSN


def available():
    """Whether the driver is installed. Callers fall back to the file when not."""
    try:
        import psycopg  # noqa: F401
    except Exception:
        return False
    return True


def query(sql, params=()):
    """Run a read-only SELECT and return a list of tuples.

    Raises nothing of its own: the psycopg exception is passed through so a caller
    can decide whether a failure is fatal. `available()` first if you want a
    silent fallback.
    """
    import psycopg

    with psycopg.connect(dsn(), connect_timeout=3) as conn:
        with conn.cursor() as cur:
            cur.execute(sql, params)
            return cur.fetchall()


def pending_zone_entry(commander_id):
    """The team the GO barrack recorded for this commander, or None.

    Returns {"channel": int, "slots": [s0, s1, s2], "continue": bool}, which is
    the same shape the JSON handoff file carries - so the zone's existing code can
    treat both the same way.

    None means "no row", and the caller should fall back to the file: the Python
    barrack still writes it, and it is the oracle path.
    """
    if not commander_id:
        return None
    rows = query(
        "SELECT channel, slot0, slot1, slot2, is_continue "
        "FROM pending_zone_entry WHERE commander_id = %s",
        (int(commander_id),))
    if not rows:
        return None
    channel, s0, s1, s2, cont = rows[0]
    return {"channel": int(channel), "slots": [int(s0), int(s1), int(s2)],
            "continue": bool(cont)}
