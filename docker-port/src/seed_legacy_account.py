"""seed_legacy_account.py - copy ONE account from the legacy DB into PostgreSQL.

The rule for this project: schema and code move to PostgreSQL now, the row DATA
moves later. But a login cannot be tested against an empty table, so this is the
"subset as seed fixture" escape hatch: one account, one commander, copied
deliberately and visibly.

It is NOT the data migration. It copies a single account by name, it prints
exactly what it wrote, and it is safe to re-run (upsert).

No PostgreSQL driver is needed: the script writes a .sql file and hands it to
`docker exec -i granado_pg psql`, so what runs is plain SQL you can read.

Usage (from src\\):
    python seed_legacy_account.py Arcanine          # copy + apply
    python seed_legacy_account.py Arcanine --dry    # write the .sql, do not apply
"""
import os
import subprocess
import sys

import ge_db
import ge_paths

CONTAINER = "granado_pg"
PG_USER = "granado"
PG_DB = "ge_data"
OUT_DIR = os.path.join(ge_paths.ROOT, "out")


def q(value):
    """Quote a value for SQL, or NULL."""
    if value is None:
        return "NULL"
    return "'%s'" % str(value).replace("'", "''")


def fetch(member_id):
    """The legacy rows for one account. Read-only."""
    data = ge_db._run(
        "SELECT ACCOUNT_ID, MEMBER_ID, COMMANDER_ID, CLASS, SPECIAL_IP "
        "FROM ACCOUNT_INFO WHERE MEMBER_ID = %s" % q(member_id))
    if not data:
        return None, None
    # ge_db._run returns STRINGS, always. Convert once, here, so nothing
    # downstream has to remember.
    acc = {
        "account_id": int(data[0][0]),
        "member_id": data[0][1],
        "commander_id": int(data[0][2]),
        "class": int(data[0][3] or 0),
        "special_ip": int(data[0][4] or 0),
    }

    res = ge_db._run(
        "SELECT PASS, CUMULATE_COMM FROM ACCOUNT_INFO WHERE MEMBER_ID = %s"
        % q(member_id), db="RESOURCE")
    # A missing password row is normal for an account created by a test run.
    pw = ({"pass": res[0][0], "cumulate_comm": int(res[0][1] or 0)} if res
          else {"pass": "-", "cumulate_comm": 0})
    return acc, pw


def build_sql(acc, pw):
    return (
        "-- Seed fixture copied from the legacy database by src\\seed_legacy_account.py\n"
        "-- One account only. This is NOT the data migration.\n"
        "BEGIN;\n"
        "INSERT INTO account_info (account_id, member_id, commander_id, class, special_ip)\n"
        "VALUES (%d, %s, %d, %d, %d)\n"
        "ON CONFLICT (account_id) DO UPDATE SET\n"
        "  member_id = EXCLUDED.member_id,\n"
        "  commander_id = EXCLUDED.commander_id,\n"
        "  class = EXCLUDED.class,\n"
        "  special_ip = EXCLUDED.special_ip;\n"
        "INSERT INTO account_pass (member_id, pass_md5, cumulate_comm)\n"
        "VALUES (%s, %s, %d)\n"
        "ON CONFLICT (member_id) DO UPDATE SET\n"
        "  pass_md5 = EXCLUDED.pass_md5,\n"
        "  cumulate_comm = EXCLUDED.cumulate_comm;\n"
        "COMMIT;\n"
        % (int(acc["account_id"]), q(acc["member_id"]), int(acc["commander_id"]),
           int(acc["class"]), int(acc["special_ip"]),
           q(acc["member_id"]), q(pw["pass"]), int(pw["cumulate_comm"]))
    )


def apply(sql_path):
    with open(sql_path, encoding="utf-8") as f:
        sql = f.read()
    cmd = ["docker", "exec", "-i", CONTAINER, "psql", "-U", PG_USER,
           "-d", PG_DB, "-v", "ON_ERROR_STOP=1", "-q"]
    proc = subprocess.run(cmd, input=sql, text=True, capture_output=True)
    if proc.returncode != 0:
        print("psql failed:\n%s\n%s" % (proc.stdout, proc.stderr))
        return False
    if proc.stdout.strip():
        print(proc.stdout.strip())
    return True


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    member_id = argv[1]
    dry = "--dry" in argv

    acc, pw = fetch(member_id)
    if acc is None:
        print("no account %r in GE_DATA.ACCOUNT_INFO" % member_id)
        return 1

    os.makedirs(OUT_DIR, exist_ok=True)
    path = os.path.join(OUT_DIR, "seed_%s.sql" % member_id)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(build_sql(acc, pw))
    print("account %s -> account_id %d, commander %d, pass %r"
          % (member_id, acc["account_id"], acc["commander_id"], pw["pass"]))
    print("wrote %s" % path)

    if dry:
        print("--dry: not applied")
        return 0
    if not apply(path):
        return 1
    print("applied to %s:%s" % (CONTAINER, PG_DB))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
