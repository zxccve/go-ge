"""Correct Blowfish (stock), with the standard test vectors.

bf_test.py had a precedence bug in F: it computed ((S0+S1) ^ (S2+S3))
instead of (((S0+S1) ^ S2) + S3). That made every Blowfish test so far wrong.

Run: python bf2.py    (prints test vectors + decrypts the capture)
"""
import os
import re
import struct

from ge_paths import CAPTURES, GAME_ROOT

M = 0xFFFFFFFF
BF_ROUNDS = 16

# Standard Blowfish init tables (hex digits of pi).
P_INIT = [
    0x243F6A88, 0x85A308D3, 0x13198A2E, 0x03707344, 0xA4093822, 0x299F31D0,
    0x082EFA98, 0xEC4E6C89, 0x452821E6, 0x38D01377, 0xBE5466CF, 0x34E90C6C,
    0xC0AC29B7, 0xC97C50DD, 0x3F84D5B5, 0xB5470917, 0x9216D5D9, 0x8979FB1B,
]

S_INIT = [
    [0xD1310BA6, 0x98DFB5AC, 0x2FFD72DB, 0xD01ADFB7, 0xB8E1AFED, 0x6A267E96,
     0xBA7C9045, 0xF12C7F99, 0x24A19947, 0xB3916CF7, 0x0801F2E2, 0x858EFC16,
     0x636920D8, 0x71574E69, 0xA458FEA3, 0xF4933D7E],
]


def load_tables(path):
    """Read the full P(18)+S(4x256) from ge.exe."""
    d = open(path, "rb").read()
    needle = bytes([0x88, 0x6A, 0x3F, 0x24])
    start = 0
    while True:
        i = d.find(needle, start)
        if i < 0:
            raise SystemExit("bf tables not found")
        words = struct.unpack("<1042I", d[i:i + 4168])
        if words[1] == 0x85A308D3:
            p = list(words[:18])
            s = [list(words[18 + k * 256:18 + (k + 1) * 256]) for k in range(4)]
            return p, s
        start = i + 1


def F(x, s):
    a = (x >> 24) & 0xFF
    b = (x >> 16) & 0xFF
    c = (x >> 8) & 0xFF
    d = x & 0xFF
    return ((((s[0][a] + s[1][b]) & M) ^ s[2][c]) + s[3][d]) & M


def enc(l, r, p, s):
    for i in range(16):
        l ^= p[i]
        r ^= F(l, s)
        l, r = r, l
    l, r = r, l
    r ^= p[16]
    l ^= p[17]
    return l, r


def dec(l, r, p, s):
    for i in range(17, 1, -1):
        l ^= p[i]
        r ^= F(l, s)
        l, r = r, l
    l, r = r, l
    r ^= p[1]
    l ^= p[0]
    return l, r


def key_schedule(key, P, S):
    p = list(P)
    j = 0
    for i in range(18):
        d = 0
        for _ in range(4):
            d = ((d << 8) | key[j % len(key)]) & M
            j += 1
        p[i] ^= d
    l = r = 0
    for i in range(0, 18, 2):
        l, r = enc(l, r, p, S)
        p[i], p[i + 1] = l, r
    s = [row[:] for row in S]
    for box in range(4):
        for i in range(0, 256, 2):
            l, r = enc(l, r, p, s)
            s[box][i], s[box][i + 1] = l, r
    return p, s


def self_test(P, S):
    cases = [
        (b"\x00" * 8, 0, 0, 0x4EF99745, 0x6198DD78),
        (b"\x01\x23\x45\x67\x89\xAB\xCD\xEF", 0x11111111, 0x11111111, 0x61F9C380, 0x2281B096),
        (b"\xFF" * 8, 0xFFFFFFFF, 0xFFFFFFFF, 0x51866FD5, 0xB85ECB8A),
    ]
    ok = True
    for key, l, r, el, er in cases:
        p, s = key_schedule(key, P, S)
        gl, gr = enc(l, r, p, s)
        good = (gl == el and gr == er)
        ok = ok and good
        print("  vec key=%s -> %08X%08X %s" % (key.hex(), gl, gr, "OK" if good else "FAIL"))
    return ok


def capture():
    txt = open(os.path.join(CAPTURES, "barrack_stub.log"),
               encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    return [p for p in pkts if len(p) == 818][0]


def main():
    P, S = load_tables(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"))
    print("self-test:")
    print("all vectors pass:", self_test(P, S))

    d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
    tbl = d[0x6eba50:0x6eba50 + 256]
    keys = {}
    for i in range(16):
        keys["col%d" % i] = bytes(tbl[r * 16 + i] for r in range(16))
        keys["row%d" % i] = tbl[i * 16:(i + 1) * 16]
    keys["src_col3"] = b"nseviqugqscgnqvs"

    ct = capture()[2:]
    probes = (b"Arcanine", b"Arcis", b"arcanine")
    for kn, key in keys.items():
        p, s = key_schedule(key, P, S)
        out = bytearray()
        for i in range(0, len(ct) // 8 * 8, 8):
            xl, xr = struct.unpack(">2I", ct[i:i + 8])
            dl, dr = dec(xl, xr, p, s)
            out += struct.pack(">2I", dl, dr)
        hit = any(out.find(pr) >= 0 for pr in probes)
        txt = "".join(chr(b) if 32 <= b < 127 else "." for b in out[:100])
        print("%-10s %r %s" % (kn, key, ("*** HIT" if hit else "")))
        print("     ", txt)


if __name__ == "__main__":
    main()
