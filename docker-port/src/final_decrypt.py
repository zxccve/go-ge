"""Verify the Blowfish algorithm on the stock table, then decrypt with the
client's real (non-standard) table from source2.
"""
import re
import os
import struct

import bf2
from ge_paths import CAPTURES, GAME_ROOT

STANDARD_SRC = os.path.join(GAME_ROOT, "source", "geShared", "Source", "CryptTable.cpp")
CLIENT_SRC = os.path.join(GAME_ROOT, "source2", "geShared", "Source", "CryptTable.cpp")
LOG = os.path.join(CAPTURES, "barrack_stub.log")


def reassemble(path):
    txt = open(path, encoding="utf-8", errors="ignore").read()
    body = txt.split("bf_dummy_table", 1)[1].split("};", 1)[0]
    nums = [int(x, 16) for x in re.findall(r"0x([0-9A-Fa-f]{8})L?", body)]
    part_num = [int(x) for x in re.search(r"tablePartNum\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    seek = [int(x) for x in re.search(r"tableSeekPointer\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    tbl = []
    for n, s in zip(part_num, seek):
        tbl += nums[s:s + n]
    P = tbl[:18]
    S = [tbl[18 + k * 256:18 + (k + 1) * 256] for k in range(4)]
    return P, S


def capture():
    txt = open(LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    return [p for p in pkts if len(p) == 818][0][2:]


def main():
    stdP, stdS = reassemble(STANDARD_SRC)
    print("stock table P[0] =", "%08X" % stdP[0], "(expect 243F6A88)")
    print("algorithm self-test:", bf2.self_test(stdP, stdS))

    P, S = reassemble(CLIENT_SRC)
    print("\nclient table P[0] =", "%08X" % P[0])

    d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
    tbl = d[0x6eba50:0x6eba50 + 256]
    keys = {}
    for i in range(16):
        keys["col%d" % i] = bytes(tbl[r * 16 + i] for r in range(16))
        keys["row%d" % i] = tbl[i * 16:(i + 1) * 16]
    keys["src_col3"] = b"nseviqugqscgnqvs"

    ct = capture()
    probes = (b"Arcanine", b"Arcis", b"arcanine", b"123456")
    for kn, key in keys.items():
        p, s = bf2.key_schedule(key, P, S)
        out = b"".join(struct.pack(">2I", *bf2.dec(*struct.unpack(">2I", ct[i:i + 8]), p, s))
                       for i in range(0, len(ct) // 8 * 8, 8))
        hit = [pr for pr in probes if out.find(pr) >= 0]
        if hit:
            print("*** HIT key=%s(%r) probes=%r" % (kn, key, hit))
            print("     ", "".join(chr(b) if 32 <= b < 127 else "." for b in out[:120]))
    print("done")


if __name__ == "__main__":
    main()
