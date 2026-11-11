"""Find every 16x16 letter table in ge.exe and test columns/rows as keys."""
import re
import struct
import hashlib
import os

import bf_test
from ge_paths import CAPTURES, GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
LOG = os.path.join(CAPTURES, "barrack_stub.log")
ALLOWED = set(range(0x30, 0x3A)) | set(range(0x61, 0x7B)) | set(range(0x41, 0x5B))


def tables(d):
    out = []
    for i in range(0, len(d) - 256):
        w = d[i:i + 256]
        if all(b in ALLOWED for b in w) and len(set(w)) >= 12:
            # require the previous byte to be a boundary (not part of a longer run)
            out.append(i)
    return out


def main():
    d = open(CLIENT, "rb").read()
    ts = tables(d)
    print("candidate 16x16 tables:", [hex(t) for t in ts][:40], "total", len(ts))

    txt = open(LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkt = [bytes(int(x, 16) for x in m.split()) for m in ms]
    ct = [p for p in pkt if len(p) == 818][0][2:]
    probes = (b"Arcanine", b"Arcis", hashlib.md5(b"123456").digest(),
              hashlib.md5(b"654321").digest())

    tried = 0
    for off in ts:
        tbl = d[off:off + 256]
        cands = {}
        for i in range(16):
            cands["c%d" % i] = bytes(tbl[r * 16 + i] for r in range(16))
            cands["r%d" % i] = tbl[i * 16:(i + 1) * 16]
        for name, key in cands.items():
            tried += 1
            p, s = bf_test.key_schedule(key)
            out = bytearray()
            for i in range(0, len(ct), 8):
                xl, xr = struct.unpack(">2I", ct[i:i + 8])
                dl, dr = bf_test.dec_block(xl, xr, p, s)
                out += struct.pack(">2I", dl, dr)
            for pr in probes:
                j = bytes(out).find(pr)
                if j >= 0:
                    print("*** MATCH table=0x%X %s key=%r probe=%r at %d"
                          % (off, name, key, pr, j))
    print("tried keys:", tried)


if __name__ == "__main__":
    main()
