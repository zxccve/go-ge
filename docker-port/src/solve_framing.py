"""solve_framing - find the packet framing of a capture by SEARCH, not by guess.

Why: the client's table gives a size per command, but that size is the size
the client ALLOCATES. How the bytes are actually laid out on the wire is a
separate question, and getting it wrong stops a capture from splitting at
all. The framing can be SOLVED: at each packet boundary, try every plausible
step and keep the path that lands on the most known command ids.

Candidates for the step from a boundary at `off` (cmd is the 2 bytes at off):
  * table[cmd]                      - the table's size IS the total length
  * 4 + len@off+2                   - cmd(2) + len(2) + payload
  * 2 + len@off+2                   - cmd(2) + len(2) + payload, len counts cmd
  * 2 + table[cmd]                  - the table's size is the payload length

Usage:
    python solve_framing.py <s2c.bin> [--table PATH] [--max-packets 400]
"""
import collections
import os
import struct
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)


def load_table(path):
    sizes, names = {}, {}
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            p = line.split()
            if len(p) >= 3 and p[0].isdigit():
                cid = int(p[0])
                sizes[cid] = int(p[1])
                names[cid] = p[2]
    return sizes, names


def steps(buf, off, cmd, sizes):
    """Plausible next-boundary offsets from `off`."""
    out = []
    sz = sizes.get(cmd)
    if sz:
        out.append(("table-total", off + sz))
        out.append(("table-payload", off + 2 + sz))
    if off + 4 <= len(buf):
        ln = struct.unpack_from("<H", buf, off + 2)[0]
        out.append(("len+payload", off + 4 + ln))
        out.append(("len+total", off + 2 + ln))
    return out


def solve(buf, sizes, max_packets=400):
    """Greedy walk with backtracking-lite: keep the path with the best score."""
    n = len(buf)
    best = ([], 0, None)          # packets, score, stop reason

    def known(off):
        if off + 2 > n:
            return None
        c = struct.unpack_from("<H", buf, off)[0]
        return c if c in sizes else None

    def walk(start, limit):
        pkts = []
        off = start
        for _ in range(limit):
            c = known(off)
            if c is None:
                return pkts, off, "unknown id %d" % struct.unpack_from("<H", buf, off)[0]
            cands = [s for s in steps(buf, off, c, sizes) if s[1] <= n]
            if not cands:
                return pkts, off, "no step"
            # Prefer a step that lands on a known id; among those, the one
            # that keeps the chain going longest.
            good = [s for s in cands if known(s[1]) is not None]
            pick = good[0] if good else cands[0]
            pkts.append((off, c, pick[0], pick[1] - off - 2))
            off = pick[1]
        return pkts, off, "limit"

    for start in range(0, 64):
        pkts, off, why = walk(start, max_packets)
        # score: bytes covered AND fraction of ids known
        if pkts:
            covered = off - start
            score = covered + 1000 * (len(pkts) / max(1, max_packets))
            if score > best[1]:
                best = (pkts, score, why)
    return best


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return
    path = args[0]
    table = os.path.join(ROOT, "data", "cmd_table_live_z.txt")
    if "--table" in args:
        table = args[args.index("--table") + 1]
    limit = 400
    if "--max-packets" in args:
        limit = int(args[args.index("--max-packets") + 1])

    sizes, names = load_table(table)
    buf = open(path, "rb").read()
    pkts, score, why = solve(buf, sizes, limit)

    print("capture: %s (%d bytes)" % (os.path.basename(path), len(buf)))
    print("packets: %d   stopped: %s" % (len(pkts), why))
    if not pkts:
        return
    end = pkts[-1][3]
    print("covered to offset %d of %d (%.1f%%)" % (end, len(buf), 100.0 * end / len(buf)))
    print()

    # which framing rule won?
    rules = collections.Counter(p[2] for p in pkts)
    print("framing rules used: %s" % dict(rules))
    print()

    counts = collections.Counter(p[1] for p in pkts)
    print("%-6s %-7s %-6s %-40s %s" % ("cmd", "hex", "count", "name", "sizes"))
    for cmd, cnt in sorted(counts.items()):
        szs = sorted(set(p[3] for p in pkts if p[1] == cmd))
        print("%-6d %-7s %-6d %-40s %s"
              % (cmd, hex(cmd), cnt, names.get(cmd, "?")[:40],
                 ",".join(str(x) for x in szs[:4])))

    print()
    print("-- first 25 packets --")
    for off, cmd, rule, sz in pkts[:25]:
        print("%8d  cmd=%-5d %-38s size=%-6d via %s"
              % (off, cmd, names.get(cmd, "?")[:38], sz, rule))


if __name__ == "__main__":
    main()
