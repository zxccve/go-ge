"""Brute a small space: key x mode x word-order, looking for the login text.

Keys = every column and row of the client's key_table at 0x6eba50, plus the
known key strings. Modes = ECB / CBC(IV=0) / CFB. Word order = big/little
endian. Probe = decrypt the capture and search for "Arcanine" / MD5 digests.

Run: python brute_modes.py
"""
import re
import struct
import hashlib
import os

import bf_test
from ge_paths import CAPTURES, GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
TABLE_OFF = 0x6eba50
LOG = os.path.join(CAPTURES, "barrack_stub.log")
IV = bytes(8)

PROBES = {
    "Arcanine": b"Arcanine",
    "Arcis": b"Arcis",
    "md5(123456)": hashlib.md5(b"123456").digest(),
    "md5(654321)": hashlib.md5(b"654321").digest(),
    "CB_LOGIN-ish": bytes.fromhex("0100"),
}


def payload():
    txt = open(LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    pkt = [p for p in pkts if len(p) == 818][0]
    return pkt[2:]


def blocks(data, endian):
    return [struct.unpack(endian + "2I", data[i:i + 8])
            for i in range(0, len(data) - 7, 8)]


def run(mode, ct, p, s, endian):
    bs = blocks(ct, endian)
    if mode == "ECB":
        out = [bf_test.dec_block(a, b, p, s) for a, b in bs]
    elif mode == "CBC":
        prev = struct.unpack(endian + "2I", IV)
        out = []
        for a, b in bs:
            x = bf_test.dec_block(a, b, p, s)
            out.append((x[0] ^ prev[0], x[1] ^ prev[1]))
            prev = (a, b)
    else:  # CFB
        prev = struct.unpack(endian + "2I", IV)
        out = []
        for a, b in bs:
            ks = bf_test.enc_block(prev[0], prev[1], p, s)
            out.append((a ^ ks[0], b ^ ks[1]))
            prev = (a, b)
    return b"".join(struct.pack(endian + "2I", *w) for w in out)


def main():
    d = open(CLIENT, "rb").read()
    tbl = d[TABLE_OFF:TABLE_OFF + 256]
    keys = {}
    for i in range(16):
        keys["col%d" % i] = bytes(tbl[r * 16 + i] for r in range(16))
        keys["row%d" % i] = tbl[i * 16:(i + 1) * 16]
    for s in ("nseviqugqscgnqvs", "891882f7d4235da", "5fe02a7a34bcf778",
              "1234567890123456"):
        keys[s] = s.encode()

    ct = payload()
    for kname, key in keys.items():
        p, s = bf_test.key_schedule(key)
        for mode in ("ECB", "CBC", "CFB"):
            for endian in (">", "<"):
                out = run(mode, ct, p, s, endian)
                for pn, pr in PROBES.items():
                    i = out.find(pr)
                    if i >= 0:
                        print("*** MATCH key=%s(%r) mode=%s endian=%s probe=%s at %d"
                              % (kname, key, mode, endian, pn, i))
    print("done")


if __name__ == "__main__":
    main()
