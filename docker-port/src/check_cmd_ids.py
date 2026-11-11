"""Cross-check every hardcoded command id against the CLIENT's own table.

Why this exists
---------------
A command id is a NUMBER, and numbers move between builds. The name does not.
So a constant like `CMD_ZC_REMOVEITEM = 3608` is only trustworthy if you can
prove that 3608 still means ZC_REMOVEITEM in the client you are shipping
against. This script does exactly that: it reads the client's own table,
looks up every id the port hardcodes, and prints the name the CLIENT has for
that id.

A mismatch is the bug this catches. In one measured build pair, id 3680 meant
ZC_DIALOG_NPC_TRADE in the new client but CZ_START_GAME in the old server -
same number, different command, and a silent no-op at runtime.

Usage
-----
    python check_cmd_ids.py                 # check the port's own files
    python check_cmd_ids.py <table> <file>  # check other files against a table

Exit code is 1 if any id disagrees with the client, so it can gate a commit.
"""

import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# The client's table. data\cmd_table.txt format:  <id>  <size>  <NAME>
DEFAULT_TABLE = os.path.normpath(os.path.join(HERE, "..", "data", "cmd_table.txt"))
# The port's server files that hardcode command ids.
DEFAULT_FILES = [
    os.path.join(HERE, "zone_server.py"),
    os.path.join(HERE, "ge_barrack_server.py"),
    os.path.join(HERE, "world.py"),
]

CMD_RE = re.compile(r"^\s*(CMD_[A-Z0-9_]+)\s*=\s*(\d+)", re.M)


def load_table(path):
    """id -> (size, name), from the client's command table."""
    table = {}
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            if not line.strip() or line.startswith("#"):
                continue
            cols = line.replace("\t", " ").split()
            if len(cols) < 3:
                continue
            try:
                cmd_id = int(cols[0])
            except ValueError:
                continue
            if cmd_id > 0 and cmd_id not in table:
                table[cmd_id] = (cols[1], cols[2])
    return table


def norm(name):
    """Strip the prefixes and punctuation so a constant can match a table name.

    The prefix has to go FIRST: removing the punctuation first turns
    "PKS_CZ_START_GAME" into "PKSCZSTARTGAME", and then a "PKS_" prefix
    check can never match.
    """
    out = name.upper()
    for prefix in ("PKS_", "CMD_"):
        if out.startswith(prefix):
            out = out[len(prefix):]
    return re.sub(r"[^A-Z0-9]", "", out)


def check(table, files):
    checked = agree = 0
    mismatched = []
    uncovered = []
    for path in files:
        if not os.path.exists(path):
            continue
        with open(path, encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        for name, value in CMD_RE.findall(text):
            checked += 1
            cmd_id = int(value)
            entry = table.get(cmd_id)
            if entry is None:
                uncovered.append((os.path.basename(path), name, cmd_id))
                continue
            size, client_name = entry
            if norm(name) == norm(client_name):
                agree += 1
            else:
                mismatched.append(
                    (os.path.basename(path), name, cmd_id, client_name, size)
                )
    return checked, agree, mismatched, uncovered


def main():
    if len(sys.argv) >= 3:
        table_path = sys.argv[1]
        files = sys.argv[2:]
    else:
        table_path = DEFAULT_TABLE
        files = DEFAULT_FILES

    if not os.path.exists(table_path):
        print("client table not found: %s" % table_path)
        print("Pass the path as the first argument.")
        return 0

    table = load_table(table_path)
    checked, agree, mismatched, uncovered = check(table, files)

    print("client table : %s (%d ids)" % (table_path, len(table)))
    print("checked      : %d hardcoded ids" % checked)
    print("agree        : %d" % agree)
    print("MISMATCH     : %d" % len(mismatched))
    print("not covered  : %d" % len(uncovered))

    if mismatched:
        print("\nMISMATCH - the id means something else in the client:")
        for path, name, cmd_id, client_name, size in mismatched:
            print("  %s: %s = %d  ->  client has '%s' (size %s)"
                  % (path, name, cmd_id, client_name, size))

    if uncovered:
        print("\nNOT COVERED by this table (expected for the barrack side, "
              "which it does not list):")
        for path, name, cmd_id in uncovered:
            print("  %s: %s = %d" % (path, name, cmd_id))

    return 1 if mismatched else 0


if __name__ == "__main__":
    sys.exit(main())
