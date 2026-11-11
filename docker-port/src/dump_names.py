"""dump_names - scan the LIVE client's gePacketTable and print name/id/size.

find_cmd.py can only look up 3 hard-coded names; this scans a whole id range so we
can locate any command by name (e.g. the NPC spawn packets).

The base-finding lives in `dump_cmds.py` now, because the old guess here was
WRONG on the current client build: it took the LAST hit of
`BC_COMMANDER_INFO` in memory, which is not the real table. The names then
came out truncated FROM THE FRONT (`COMMANDEROK` for `BC_COMMANDEROK`) and
the sizes read past the end of a page, so the tool died with a short read.
`dump_cmds.py` picks the base by requiring EXACT (name, id) anchor pairs, and
locates the sizes array by anchors too.

Usage:
    python dump_names.py                     # ids 2..60
    python dump_names.py 3000 4200           # a range
    python dump_names.py 2 20000 VILLAGER    # only names containing VILLAGER
"""
import struct
import sys

import dump_cmds
import find_cmd


def table_bases(h):
    """The names/sizes bases, verified with anchor pairs (see dump_cmds)."""
    nb = dump_cmds.find_names_base(h)
    if nb is None:
        raise SystemExit("packet-name table not found (client running?)")
    sb = dump_cmds.find_sizes_base(h, nb)
    if sb is None:
        raise SystemExit("sizes array not found (client running?)")
    return nb, sb


def main():
    args = sys.argv[1:]
    filt = ""
    if args and not args[0].isdigit():
        filt = args.pop(0)
    lo = int(args[0]) if len(args) > 0 else 2
    hi = int(args[1]) if len(args) > 1 else 60
    if len(args) > 2:
        filt = args[2]

    h = find_cmd.k32.OpenProcess(find_cmd.PQI | find_cmd.PVR, False,
                                 find_cmd.pid_of())
    nb, sb = table_bases(h)
    print("names_base=%#x sizes_base=%#x" % (nb, sb))
    n = 0
    for c in range(lo, hi + 1):
        nm = find_cmd.rd(h, nb + c * find_cmd.NAMES_STRIDE, 50)
        nm = nm.split(b"\x00")[0].decode("latin-1", "replace")
        if not nm or not nm.isprintable():
            continue
        b4 = find_cmd.rd(h, sb + c * 4, 4)
        sz = struct.unpack("<i", b4)[0] if len(b4) == 4 else -1
        if filt and filt.upper() not in nm.upper():
            continue
        n += 1
        print("%-6d %-38s %d%s" % (c, nm, sz, "  (var)" if sz == 0 else ""))
    print("-- %d matches --" % n)


if __name__ == "__main__":
    main()
