"""probe_framing - work out how a capture is framed, instead of assuming.

Why: when a capture will not split, the instinct is to blame the protocol
("the server changed"). That is often wrong. A capture can fail to split for
reasons that have nothing to do with drift: the table is the wrong build, the
size column means something else, or the leg is encrypted.

So: enumerate the CANDIDATE FRAMINGS, walk each one with the client's own
table, and let the arithmetic decide. The right framing is the one that walks
the buffer to EXACTLY the last byte, with every id known to the table.

Candidates:
  A  cmd(2) + payload(size from table; size 0 -> len(2) at +2)
  B  cmd(2) + len(2) + payload,  len = payload length
  C  cmd(2) + len(2) + payload,  len = TOTAL packet length
  D  cmd(2) + payload, size from table means TOTAL packet length
  E  cmd(2) + len(2) + payload, but cmd ids are only known in a sub-range
     (the table's ids start above 0 - the server may use a different base)

Usage:  python probe_framing.py <capture.bin> [table.txt]
"""
import struct
import sys

HERE = __file__.rsplit("\\", 1)[0].rsplit("/", 1)[0]


def load_table(path):
    sizes = {}
    names = {}
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            p = line.split()
            if len(p) >= 3 and p[0].isdigit():
                cid = int(p[0])
                sizes[cid] = int(p[1])
                names[cid] = p[2]
    return sizes, names


def walk_A(d, sizes):
    i, out = 0, []
    n = len(d)
    while i + 2 <= n:
        cmd = struct.unpack_from("<H", d, i)[0]
        sz = sizes.get(cmd)
        if sz is None:
            return out, i, "unknown id %d" % cmd
        if sz == 0:
            if i + 4 > n:
                return out, i, "truncated"
            ln = struct.unpack_from("<H", d, i + 2)[0]
            out.append((i, cmd, ln))
            i += 4 + ln
        else:
            out.append((i, cmd, sz))
            i += 2 + sz
    return out, i, "end of buffer"


def walk_B(d, sizes, total=False):
    i, out = 0, []
    n = len(d)
    while i + 4 <= n:
        cmd, ln = struct.unpack_from("<HH", d, i)
        step = ln if total else ln + 4
        if step < 4 or i + step > n:
            return out, i, "length overflow (len=%d at %d)" % (ln, i)
        out.append((i, cmd, ln))
        i += step
    return out, i, "end of buffer"


def walk_D(d, sizes):
    """size from the table means TOTAL packet length (cmd included)."""
    i, out = 0, []
    n = len(d)
    while i + 2 <= n:
        cmd = struct.unpack_from("<H", d, i)[0]
        sz = sizes.get(cmd)
        if sz is None:
            return out, i, "unknown id %d" % cmd
        if sz == 0:
            if i + 4 > n:
                return out, i, "truncated"
            ln = struct.unpack_from("<H", d, i + 2)[0]
            out.append((i, cmd, ln))
            i += 4 + ln
        else:
            if sz < 2:
                return out, i, "bad size"
            out.append((i, cmd, sz - 2))
            i += sz
    return out, i, "end of buffer"


def report(tag, d, sizes, names, out, end, why):
    n = len(d)
    exact = (end == n)
    known = sum(1 for _, c, _ in out if c in sizes)
    print("%-46s packets=%-6d ended=%-7d/%-7d %-9s known_ids=%d/%d  %s"
          % (tag, len(out), end, n, "EXACT" if exact else "partial",
             known, len(out), why))
    return exact


def main():
    cap = sys.argv[1]
    tbl = sys.argv[2] if len(sys.argv) > 2 else None
    if tbl is None:
        tbl = HERE + "/../data/cmd_table_live.txt"
    d = open(cap, "rb").read()
    sizes, names = load_table(tbl)
    print("capture %s: %d bytes" % (cap, len(d)))
    print("table %s: %d ids, id range %d..%d"
          % (tbl, len(sizes), min(sizes), max(sizes)))
    print()

    results = []
    for tag, fn in (("A cmd+table-size (var: len@+2)", walk_A),
                    ("B cmd+len+payload (len=payload)", None),
                    ("C cmd+len+payload (len=total)", None),
                    ("D cmd+payload (size=total len)", walk_D)):
        if tag.startswith("B"):
            out, end, why = walk_B(d, sizes, total=False)
        elif tag.startswith("C"):
            out, end, why = walk_B(d, sizes, total=True)
        else:
            out, end, why = fn(d, sizes)
        results.append((tag, out, end, why))

    winner = None
    for tag, out, end, why in results:
        if report(tag, d, sizes, names, out, end, why):
            winner = (tag, out)

    print()
    if winner is None:
        print("NO framing walks the buffer exactly.")
        print("That points at one of: the wrong table (wrong client build),")
        print("an encrypted leg, or a size column that means something else.")
        print("\n-- raw head --")
        for off in range(0, min(96, len(d)), 16):
            ch = d[off:off + 16]
            print("%06X  %-47s  %s" % (off, " ".join("%02X" % b for b in ch),
                                       "".join(chr(b) if 32 <= b < 127 else "." for b in ch)))
        # Look for readable strings - plaintext legs carry them.
        s = []
        cur = b""
        for b in d[:4000]:
            if 32 <= b < 127:
                cur += bytes([b])
            else:
                if len(cur) >= 6:
                    s.append(cur.decode("latin-1"))
                cur = b""
        print("\n-- printable runs in the first 4000 bytes --")
        for x in s[:25]:
            print("   ", x)
        return

    tag, out = winner
    print("WINNER: %s" % tag)
    print("%-8s %-7s %-8s %s" % ("offset", "cmd", "size", "name"))
    for off, cmd, ln in out[:40]:
        print("%-8d %-7d %-8d %s" % (off, cmd, ln, names.get(cmd, "?")))


if __name__ == "__main__":
    main()
