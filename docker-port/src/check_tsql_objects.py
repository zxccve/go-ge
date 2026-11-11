"""check_tsql_objects.py - what in the legacy database will NOT translate.

The Go server targets PostgreSQL. Schema and code are built against PostgreSQL
from the start; only the row data is moved later, by a Python script.

That makes one question urgent: **which stored procedures, triggers, functions
and views in the Microsoft database are T-SQL-only?** They do not translate
automatically. Each one is either rewritten as Go logic or replaced by a
PostgreSQL function - and that is a decision, not a copy-paste.

The Python server already calls some of them directly (the item layer's
TxGiveItem / TxTakeItem run inside a transaction), so they are load-bearing.

Read-only. Usage (from src\\):
    python check_tsql_objects.py            # summary + the objects found
    python check_tsql_objects.py --all      # every object, not just T-SQL ones
    python check_tsql_objects.py --sql FILE # also write the inventory as text
"""
import sys

import ge_db

# The two databases the PostgreSQL target mirrors (ge_data / ge_resource).
# NOTE: the SQL Server name is RESOURCE, not GE_RESOURCE - querying
# sys.databases shows: EVENT, GE_DATA, GE_LOG, GEEU_CLIENT, HBS, RESOURCE,
# RESOURCE_LOG. A wrong name fails the LOGIN, not the query, so it reads like
# "permission denied".
DBS = ("GE_DATA", "RESOURCE")

# Object types worth looking at, from sys.objects.
TYPES = {
    "P": "procedure", "TR": "trigger", "FN": "function", "IF": "inline function",
    "TF": "table function", "V": "view",
}

# Constructs with no direct PostgreSQL equivalent, or whose meaning differs
# enough that a blind rewrite is a bug. (pattern, why)
TSQL_ONLY = [
    ("MERGE ", "MERGE syntax and semantics differ from INSERT ... ON CONFLICT"),
    ("TOP ", "LIMIT does not accept TOP n, and TOP (n) PERCENT has no direct form"),
    ("ISNULL(", "use COALESCE; ISNULL also truncates the result type"),
    ("GETDATE(", "use now()"),
    ("SYSDATETIME(", "use now()"),
    ("NEWID(", "use gen_random_uuid() or uuid_generate_v4()"),
    ("IDENTITY(", "use GENERATED ... AS IDENTITY"),
    ("@@IDENTITY", "use RETURNING / lastval()"),
    ("SCOPE_IDENTITY(", "use RETURNING"),
    ("NOLOCK", "no equivalent; depends on MVCC instead"),
    ("RAISERROR", "use RAISE"),
    ("TRY", "TRY/CATCH is T-SQL only; PostgreSQL uses BEGIN ... EXCEPTION"),
    ("CATCH", "TRY/CATCH is T-SQL only; PostgreSQL uses BEGIN ... EXCEPTION"),
    ("OUTPUT INSERTED", "use RETURNING"),
    ("OUTPUT DELETED", "use RETURNING"),
    ("#", "temp tables: PostgreSQL uses CREATE TEMP TABLE with a real name"),
    ("EXEC ", "dynamic SQL via EXEC/sp_executesql has no direct equivalent"),
    ("sp_executesql", "dynamic SQL via EXEC/sp_executesql has no direct equivalent"),
    ("COLUMNS_UPDATED(", "triggers differ fundamentally (statement triggers)"),
    ("INSERTED", "the INSERTED/DELETED pseudo-tables do not exist; use NEW/OLD"),
    ("DELETED", "the INSERTED/DELETED pseudo-tables do not exist; use NEW/OLD"),
    ("PIVOT", "no PIVOT operator; use crosstab or explicit CASE"),
    ("GO\r\n", "GO is a client batch separator, not SQL"),
]


def objects(db):
    """Every procedure/trigger/function/view in one database."""
    types = ",".join("'%s'" % t for t in TYPES)
    rows = ge_db._run(
        "SELECT o.name, o.type FROM sys.objects o "
        "WHERE o.type IN (%s) AND o.is_ms_shipped = 0 ORDER BY o.type, o.name" % types,
        db=db)
    return [(r[0], r[1]) for r in rows]


def definition(db, name):
    rows = ge_db._run(
        "SELECT m.definition FROM sys.sql_modules m "
        "JOIN sys.objects o ON o.object_id = m.object_id WHERE o.name = '%s'" % name,
        db=db)
    return rows[0][0] if rows else ""


def flags(body):
    """The T-SQL-only constructs this body uses, deduplicated, in list order."""
    hits = []
    for needle, why in TSQL_ONLY:
        if needle in body:
            token = needle.strip()
            if token not in [h[0] for h in hits]:
                hits.append((token, why))
    return hits


def columns(table, db="GE_DATA"):
    """Column list of one legacy table: name, type, length, nullable, default."""
    rows = ge_db._run(
        "SELECT c.COLUMN_NAME, c.DATA_TYPE, c.CHARACTER_MAXIMUM_LENGTH, "
        "c.NUMERIC_PRECISION, c.NUMERIC_SCALE, c.IS_NULLABLE, c.COLUMN_DEFAULT "
        "FROM INFORMATION_SCHEMA.COLUMNS c "
        "WHERE c.TABLE_NAME = '%s' ORDER BY c.ORDINAL_POSITION" % table,
        db=db)
    out = []
    for name, typ, charlen, prec, scale, nullable, default in rows:
        detail = typ
        if charlen not in (None, ""):
            detail = "%s(%s)" % (typ, charlen)
        elif prec not in (None, "") and typ in ("decimal", "numeric"):
            detail = "%s(%s,%s)" % (typ, prec, scale)
        out.append((name, detail, nullable, default))
    return out


def show_columns(table, db="GE_DATA"):
    rows = columns(table, db)
    if not rows:
        print("%s.%s: no such table (or no columns)" % (db, table))
        return 1
    print("%s.%s - %d column(s)" % (db, table, len(rows)))
    for name, detail, nullable, default in rows:
        print("  %-28s %-18s %s%s"
              % (name, detail,
                 "NULL" if nullable == "YES" else "NOT NULL",
                 "" if not default else "  DEFAULT %s" % default))
    return 0


def show_tables(pattern):
    """Tables whose name matches a LIKE pattern, with their column count."""
    rows = ge_db._run(
        "SELECT t.TABLE_NAME, COUNT(c.COLUMN_NAME) "
        "FROM INFORMATION_SCHEMA.TABLES t "
        "LEFT JOIN INFORMATION_SCHEMA.COLUMNS c ON c.TABLE_NAME = t.TABLE_NAME "
        "WHERE t.TABLE_NAME LIKE '%%%s%%' "
        "GROUP BY t.TABLE_NAME ORDER BY t.TABLE_NAME" % pattern, db="GE_DATA")
    for name, count in rows:
        print("  %-34s %s column(s)" % (name, count))
    print("%d table(s) matching %r" % (len(rows), pattern))
    return 0


def main(argv):
    if "--tables" in argv:
        i = argv.index("--tables")
        pattern = argv[i + 1] if i + 1 < len(argv) else ""
        return show_tables(pattern)
    if "--columns" in argv:
        i = argv.index("--columns")
        if i + 1 >= len(argv):
            print("usage: check_tsql_objects.py --columns TABLE [DB]")
            return 2
        db = argv[i + 2] if i + 2 < len(argv) else "GE_DATA"
        return show_columns(argv[i + 1], db)
    show_all = "--all" in argv
    out_path = None
    if "--sql" in argv:
        i = argv.index("--sql")
        if i + 1 < len(argv):
            out_path = argv[i + 1]

    lines = []
    total = 0
    portable, needs_work = [], []

    for db in DBS:
        try:
            objs = objects(db)
        except Exception as exc:                       # a missing DB is a fact
            lines.append("%s: cannot read (%s)" % (db, exc))
            continue
        lines.append("%s: %d object(s)" % (db, len(objs)))
        total += len(objs)
        for name, typ in objs:
            body = definition(db, name)
            hits = flags(body)
            kind = TYPES.get(typ.strip(), typ)
            label = "%s %s.%s (%d chars)" % (kind, db, name, len(body))
            if hits:
                needs_work.append((label, hits))
                lines.append("  NEEDS WORK  " + label)
                for token, why in hits:
                    lines.append("              %-16s %s" % (token, why))
            else:
                portable.append(label)
                if show_all:
                    lines.append("  portable    " + label)

    print("\n".join(lines))
    print("")
    print("%d object(s): %d portable, %d need manual work"
          % (total, len(portable), len(needs_work)))

    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        print("wrote %s" % out_path)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
