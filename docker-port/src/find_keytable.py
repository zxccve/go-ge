"""Find the geCrypt key_table in ge.exe and derive the Blowfish key.

key_table is 16x16 bytes. MixKey (isVertical=false, useArrayIndex=3) makes
key[i] = key_table[i][3]. Find the table by its first row and try both
orientations, then Blowfish-ECB decrypt the capture and look for the login
text / MD5 digest.

Run: python find_keytable.py
"""
import re
import struct
import hashlib
import os

import bf_test
from ge_paths import CAPTURES, GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
LOG = os.path.join(CAPTURES, "barrack_stub.log")

ROW0 = b"gqbnnmwmymsi0doz"


def capture():
    txt = open(LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    return [p for p in pkts if len(p) == 818][0]


def bf_ecb(data, p, s):
    out = bytearray()
    for i in range(0, len(data) - 7, 8):
        xl, xr = struct.unpack(">2I", data[i:i + 8])
        dl, dr = bf_test.dec_block(xl, xr, p, s)
        out += struct.pack(">2I", dl, dr)
    return bytes(out)


def main():
    data = open(CLIENT, "rb").read()
    hits = []
    start = 0
    while True:
        i = data.find(ROW0, start)
        if i < 0:
            break
        hits.append(i)
        start = i + 1
    print("row0 occurrences in ge.exe:", [hex(h) for h in hits])

    pkt = capture()
    payload = pkt[2:]
    probes = {
        "Arcanine": b"Arcanine",
        "Arcis": b"Arcis",
        "md5(123456)": hashlib.md5(b"123456").digest(),
        "md5(654321)": hashlib.md5(b"654321").digest(),
    }

    keys = {}
    for off in hits:
        tbl = data[off:off + 256]
        if len(tbl) < 256:
            continue
        keys["col3@%X" % off] = bytes(tbl[r * 16 + 3] for r in range(16))
        keys["row3@%X" % off] = bytes(tbl[3 * 16 + c] for c in range(16))
    # the source's own key, for reference
    src = [b"gqbnnmwmymsi0doz", b"gsqsda...", b""]
    print("derived keys:")
    for k, v in keys.items():
        print("  %-14s %r" % (k, v))

    for kname, key in keys.items():
        p, s = bf_test.key_schedule(key)
        out = bf_ecb(payload, p, s)
        for pname, probe in probes.items():
            i = out.find(probe)
            if i >= 0:
                print("*** MATCH key=%s probe=%s at offset %d" % (kname, pname, i))
        txt = "".join(chr(b) if 32 <= b < 127 else "." for b in out)
        print("  %s -> %s" % (kname, txt[:120]))


if __name__ == "__main__":
    main()
