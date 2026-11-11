"""emit_barrack_oracle.py - dump the WORKING server's barrack packets.

The Go server builds BC_COMMANDER_INFO and BC_BARRACK_INFO from PostgreSQL; the
working Python server builds the same packets from MSSQL. They must be identical
for the same commander, because the PostgreSQL rows were copied from those MSSQL
rows.

This writes the Python bytes as fixtures so the Go tests can compare against
them byte for byte - no client, no guessing, and a difference points at a field.

It calls the SERVER's own functions (`load_chars`, `build_commander_info`,
`build_barrack_info`), so the oracle cannot drift from what the server really
sends.

Usage (from src\\):
    python emit_barrack_oracle.py [commander_id]
"""
import os
import sys

import ge_barrack_server as gs
import ge_db
import ge_paths

OUT_DIR = os.path.join(ge_paths.ROOT, "goserver", "barrack", "testdata")


def main(argv):
    commander_id = int(argv[1]) if len(argv) > 1 else 24

    chars = gs.load_chars(commander_id)
    cm = ge_db.get_commander(commander_id)
    if cm is None:
        print("commander %d not found" % commander_id)
        return 1
    cm["barrack_names"] = ge_db.get_barrack_names(commander_id)

    p10 = gs.build_commander_info(cm)
    p16 = gs.build_barrack_info(chars)

    os.makedirs(OUT_DIR, exist_ok=True)
    for name, blob in (("oracle_10.bin", p10), ("oracle_16.bin", p16)):
        path = os.path.join(OUT_DIR, name)
        with open(path, "wb") as f:
            f.write(blob)
        print("wrote %s (%d bytes)" % (path, len(blob)))

    print("commander %d %r: %d characters" % (commander_id, cm.get("name"), len(chars)))
    for c in chars[:3]:
        print("  slot %-3s %-10s job %-4s teampos %-2s exp %s"
              % (c.get("slot"), c.get("name"), c.get("job"), c.get("teampos"), c.get("exp")))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
