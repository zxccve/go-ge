"""Extract the v28 server's command registration table.

The server registers handlers with the pattern

    68 <name string RVA>   push "PKS_..."
    6A <size>              push size        (0 = variable length)
    68 <cmd id>            push id
    E8 <rel32>             call <register>

Finding that byte pattern gives the whole id -> (name, size) map with no Ghidra.

    python v28_cmdtable.py            # summary + writes data\\v28_handler_table.txt
    python v28_cmdtable.py --diff     # also diff against the client's data\\cmd_table.txt
"""
import struct
import os

import sys
from ge_paths import GAME_ROOT

TARGET = os.path.join(GAME_ROOT, "v28", "rGE_server", "server_release", "ZoneServer_Release.exe")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "data",
                   "v28_handler_table.txt")


def sections(d):
    e = struct.unpack_from("<I", d, 0x3C)[0]
    nsec = struct.unpack_from("<H", d, e + 6)[0]
    optsz = struct.unpack_from("<H", d, e + 20)[0]
    sh = e + 24 + optsz
    out = []
    for i in range(nsec):
        o = sh + i * 40
        name = d[o:o + 8].rstrip(b"\0").decode("latin1")
        vsz = struct.unpack_from("<I", d, o + 8)[0]
        va = struct.unpack_from("<I", d, o + 12)[0]
        rsz = struct.unpack_from("<I", d, o + 16)[0]
        ro = struct.unpack_from("<I", d, o + 20)[0]
        out.append((name, va, vsz, ro, rsz))
    return out


def cstring(d, off):
    if off is None:
        return "?"
    end = d.find(b"\0", off)
    if end < 0 or end - off > 80:
        return "?"
    raw = d[off:end]
    if not raw or not all(32 <= c < 127 for c in raw):
        return "?"
    return raw.decode("latin1")


def cstr_at_va(d, secs, va):
    rva = va - 0x400000
    for _n, sva, vsz, ro, rsz in secs:
        if sva <= rva < sva + max(vsz, rsz):
            return cstring(d, ro + (rva - sva))
    return "?"


def scan(d, secs):
    """Every `push name; push size; push id; call register` site, both encodings.

    Form A (the common one): `68 <nameVA> 6A <size8> 68 <id32> E8` - 13 bytes.
    Form B: `68 <nameVA> 68 <size32> 68 <id32> E8` - 17 bytes, used where the
    size does not fit an imm8 (and, it turned out, by a whole second batch of
    registrations: 109 ids, including 0x0E09, were invisible to form A alone
    and that is what made the s2c walk stop mid-stream on 2026-11-05).
    """
    _n, tva, _tvsz, tro, trsz = next(s for s in secs if s[0] == ".text")
    rows = []
    o = tro
    end = tro + trsz
    while o < end - 16:
        if d[o] == 0x68 and d[o + 5] == 0x6A and d[o + 7] == 0x68 and d[o + 12] == 0xE8:
            name_va = struct.unpack_from("<I", d, o + 1)[0]
            size = d[o + 6]
            cmd = struct.unpack_from("<I", d, o + 8)[0]
            if 0x400000 <= name_va < 0x2000000 and 0 < cmd < 0x10000:
                name = cstr_at_va(d, secs, name_va)
                if name.startswith("PKS_"):
                    rows.append((cmd, size, name, 0x400000 + tva + (o - tro)))
                    o += 13
                    continue
        o += 1
    o = tro
    while o < end - 20:
        if d[o] == 0x68 and d[o + 5] == 0x68 and d[o + 10] == 0x68 and d[o + 15] == 0xE8:
            name_va = struct.unpack_from("<I", d, o + 1)[0]
            size = struct.unpack_from("<I", d, o + 6)[0]
            cmd = struct.unpack_from("<I", d, o + 11)[0]
            if 0x400000 <= name_va < 0x2000000 and 0 < cmd < 0x10000 and 0 <= size < 0x10000:
                name = cstr_at_va(d, secs, name_va)
                if name.startswith("PKS_"):
                    rows.append((cmd, size, name, 0x400000 + tva + (o - tro)))
                    o += 17
                    continue
        o += 1
    return rows


def main():
    d = open(TARGET, "rb").read()
    secs = sections(d)
    rows = scan(d, secs)

    seen = {}
    for cmd, size, name, at in rows:
        seen.setdefault(cmd, (size, name, at))
    rows = sorted((c, s, n, a) for c, (s, n, a) in seen.items())

    print("registered commands found: %d" % len(rows))
    if rows:
        print("id range: 0x%X .. 0x%X" % (rows[0][0], rows[-1][0]))
    print("variable-size (size 0): %d, fixed: %d" % (
        sum(1 for r in rows if r[1] == 0), sum(1 for r in rows if r[1] != 0)))

    with open(OUT, "w", encoding="utf8") as f:
        f.write("# v28 server command registration table\n")
        f.write("# id\tsize\tname\tregistration_site\n")
        for cmd, size, name, at in rows:
            f.write("0x%04X\t%d\t%s\t0x%06X\n" % (cmd, size, name, at))
    print("wrote", os.path.abspath(OUT))

    if "--diff" in sys.argv:
        diff(rows)


def diff(v28_rows):
    client = {}
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "..", "data", "cmd_table.txt")
    for line in open(path, encoding="utf8"):
        parts = line.split()
        if len(parts) >= 3 and parts[2].startswith("PKS_"):
            client[int(parts[0])] = (int(parts[1]), parts[2])

    v28 = {name: (cmd, size) for cmd, size, name, _a in v28_rows}
    cli = {name: (cmd, size) for cmd, (size, name) in client.items()}

    common = set(v28) & set(cli)
    same = [n for n in common if v28[n][0] == cli[n][0]]
    moved = sorted(n for n in common if v28[n][0] != cli[n][0])
    print()
    print("names in both: %d   same id: %d (%.1f%%)   different id: %d" % (
        len(common), len(same), 100.0 * len(same) / len(common) if common else 0,
        len(moved)))
    print("only in client (new): %d   only in v28 (removed): %d" % (
        len(set(cli) - set(v28)), len(set(v28) - set(cli))))
    print()
    print("sample of moved ids (name: client -> v28):")
    for n in moved[:15]:
        print("   %-42s %5d -> %5d" % (n, cli[n][0], v28[n][0]))
    print()
    print("only in the client (these are the genuinely new commands):")
    for n in sorted(set(cli) - set(v28))[:20]:
        print("   %-45s id %d" % (n, cli[n][0]))


if __name__ == "__main__":
    main()
