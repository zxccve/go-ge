"""Decrypt the captured login with Blowfish + the source-derived key.

geCrypt::MixKey() with isVertical=false, useArrayIndex=3 -> key is column 3
of key_table: "nseviqugqscgnqvs". imcCrypt::SetKey feeds it into a
Blowfish-style key schedule (BF_KEYTABLE_SIZE = 18 + 4*256 = 1042).

Run: python decrypt_net.py
"""
import re
import struct
import hashlib
import os

import bf_test
from ge_paths import CAPTURES

KEY = b"nseviqugqscgnqvs"
LOG = os.path.join(CAPTURES, "barrack_stub.log")
CAP1 = os.path.join(CAPTURES, "cap_01.log")


def payloads():
    txt = open(LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    old = [p for p in pkts if len(p) == 818][0]
    t2 = open(CAP1, encoding="utf-8", errors="ignore").read()
    m2 = re.search(r"hex=\[(.*?)\]", t2)
    new = bytes(int(x, 16) for x in m2.group(1).split())
    return {"old": old, "cap_01": new}


def main():
    p, s = bf_test.key_schedule(KEY)
    print("key schedule built from key", KEY.decode())

    for name, pkt in payloads().items():
        for off in (2, 0):
            blob = pkt[off:]
            blob = blob[:len(blob) // 8 * 8]
            out = bytearray()
            for i in range(0, len(blob), 8):
                xl, xr = struct.unpack(">2I", blob[i:i + 8])
                dl, dr = bf_test.dec_block(xl, xr, p, s)
                out += struct.pack(">2I", dl, dr)
            txt = "".join(chr(b) if 32 <= b < 127 else "." for b in out)
            print("\n[%s off=%d] printable:" % (name, off))
            print(" ", txt[:200])
            for probe in (b"Arcanine", b"Arcis",
                          hashlib.md5(b"123456").digest(),
                          hashlib.md5(b"654321").digest()):
                i = bytes(out).find(probe)
                if i >= 0:
                    print("  FOUND %r at offset %d" % (probe, i))


if __name__ == "__main__":
    main()
