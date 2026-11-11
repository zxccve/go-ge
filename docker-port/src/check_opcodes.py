#!/usr/bin/env python3
"""Cross-check our opcode tables against an extraction from the client executable.

WHY THIS EXISTS. Three tables describe the same thing and nobody had compared them:
`data\\cmd_table.txt` (the zone-side ids the Go server switches on), `data\\cmd_table_live.txt`
(what the live client actually sent, 1,555 rows) and `data\\opcodes.txt` (1,587 names). A wrong
id or a wrong SIZE in any of them is a packet the server builds to the wrong length, which the
client either rejects or misreads - the kind of bug that costs a session to find.

THE FOURTH SOURCE, and the reason this file is new (2026-11-09): offlinespada's
`ge keys opcodes --from <ge.exe> --to <file.tsv>` reads the client's own packet-registration
calls out of the executable WITHOUT running it (offlinespada-main\\rust\\crates\\ge-keys). That
is an extraction from the binary we ship against, not a dump of a live session:

    cd <path>
    cargo run -q -p ge -- keys opcodes --from "<your client>/release/ge.exe" \
        --to data/opcodes_extracted.tsv

Run this checker after refreshing that file:

    cd docker-port\\src && python check_opcodes.py

WHAT IT REPORTS. Ids in the extraction that our tables lack; ids our tables have that the
extraction lacks; and every SIZE or NAME that disagrees. A disagreement is not automatically
our bug - but it is always one of the two, and the row names which.
"""
from __future__ import annotations

import csv
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "..", "data")

EXTRACTED = os.path.join(DATA, "opcodes_extracted.tsv")
ZONE = os.path.join(DATA, "cmd_table.txt")
LIVE = os.path.join(DATA, "cmd_table_live.txt")


def load_extracted(path: str) -> dict[int, tuple[int, str, str]]:
    """id -> (size, channel, name) from offlinespada's TSV (size 0 = variable length)."""
    out: dict[int, tuple[int, str, str]] = {}
    with open(path, encoding="utf-8", newline="") as fh:
        for row in csv.DictReader(fh, delimiter="\t"):
            out[int(row["id_dec"])] = (int(row["size_hex"], 16), row["channel"], row["name"])
    return out


def load_dump(path: str) -> dict[int, tuple[int, str]]:
    """id -> (size, name) from one of our own dumps (tab separated, # comments allowed)."""
    out: dict[int, tuple[int, str]] = {}
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip().lstrip("\ufeff")
            if not line or line.startswith("#"):
                continue
            parts = line.replace("\r", "").split("\t")
            if len(parts) < 3:
                parts = line.split()
            if len(parts) < 3:
                continue
            try:
                out[int(parts[0])] = (int(parts[1]), parts[2])
            except ValueError:
                continue
    return out


def compare(name: str, extracted: dict[int, tuple[int, str, str]], mine: dict[int, tuple[int, str]]) -> int:
    """Print one table's disagreements; answer how many rows disagreed."""
    shared = sorted(set(extracted).intersection(mine))
    only_theirs = sorted(set(extracted) - set(mine))
    only_ours = sorted(set(mine) - set(extracted))

    size_diff = [(i, mine[i][0], extracted[i][0], extracted[i][2]) for i in shared if mine[i][0] != extracted[i][0]]
    name_diff = []
    for i in shared:
        ours, theirs = mine[i][1], extracted[i][2]
        if ours == theirs or (theirs.startswith("PKS_") and theirs[4:] == ours):
            continue
        name_diff.append((i, ours, theirs))

    print("== %s: %d row(s), %d shared with the extraction" % (name, len(mine), len(shared)))
    print("   only in the extraction: %d %s" % (len(only_theirs), only_theirs[:12]))
    print("   only in ours:           %d %s" % (len(only_ours), only_ours[:12]))
    print("   SIZE disagreements:     %d" % len(size_diff))
    for i, a, b, n in size_diff[:12]:
        print("      id %-6d ours %-5d extraction %-5d  %s" % (i, a, b, n))
    print("   NAME disagreements:     %d" % len(name_diff))
    for i, a, b in name_diff[:12]:
        print("      id %-6d ours %-30s extraction %s" % (i, a, b))
    return len(size_diff) + len(name_diff) + len(only_ours)


def main() -> int:
    for path in (EXTRACTED, ZONE, LIVE):
        if not os.path.exists(path):
            print("missing input: %s" % path)
            if path == EXTRACTED:
                print("  build it with the offlinespada command named in this file's header")
            return 2

    extracted = load_extracted(EXTRACTED)
    print("extraction: %d id(s) from %s" % (len(extracted), EXTRACTED))

    print()
    bad = compare("cmd_table_live.txt", extracted, load_dump(LIVE))
    print()
    bad += compare("cmd_table.txt (zone side)", extracted, load_dump(ZONE))

    print()
    if bad == 0:
        print("opcodes OK - every shared id agrees on size and name")
        return 0
    print("%d disagreement(s): each one is either our table or the extraction, and the row says which" % bad)
    return 1


if __name__ == "__main__":
    sys.exit(main())
