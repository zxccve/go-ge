"""Blowfish test rig: tables lifted from ge.exe, key from Ghidra.
Reads the captured 818-byte hello from barrack_stub.log and tries
Blowfish-ECB decrypt with key b'891882f7d4235da'.
Run: python bf_test.py
"""
import re
import struct
import pathlib

from ge_paths import CAPTURES, GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
LOG = pathlib.Path(CAPTURES) / "barrack_stub.log"
KEY = b"891882f7d4235da"
M = 0xFFFFFFFF

# Blowfish P-array word 2 (public constant, used only to FIND the table)
P1 = 0x85A308D3


def rva_to_offset(data, rva):
    pe = struct.unpack("<I", data[0x3C:0x40])[0]
    nsec = struct.unpack("<H", data[pe + 6:pe + 8])[0]
    optsz = struct.unpack("<H", data[pe + 20:pe + 22])[0]
    sh = pe + 24 + optsz
    for i in range(nsec):
        o = sh + i * 40
        vs, va, rs, rp = struct.unpack("<IIII", data[o + 8:o + 24])
        if va <= rva < va + max(vs, rs):
            return rp + (rva - va)
    return None


def load_tables():
    data = pathlib.Path(CLIENT).read_bytes()
    for rva in (0x651AF8 - 0x400000, 0x6EDE18 - 0x400000 if False else 0x2EDE18, 0x2EE368):
        pass
    # find P0 bytes (bytes([...]) form: shell-proof, no \x escapes),
    # then verify P1 follows
    needle = bytes([0x88, 0x6A, 0x3F, 0x24])
    hits = []
    start = 0
    while True:
        i = data.find(needle, start)
        if i < 0:
            break
        hits.append(i)
        start = i + 1
    for off in hits:
        words = struct.unpack("<1042I", data[off:off + 4168])
        if len(words) == 1042 and words[1] == P1:
            p = list(words[:18])
            s = [list(words[18 + i * 256:18 + (i + 1) * 256]) for i in range(4)]
            print("tables at file offset 0x%X" % off)
            return p, s
    raise SystemExit("tables not found")


P, S = load_tables()


def key_schedule(key):
    p = P[:]
    j = 0
    for i in range(18):
        d = 0
        for _ in range(4):
            d = ((d << 8) + key[j % len(key)]) & 0xFFFFFFFF
            j += 1
        p[i] ^= d
    xl = xr = 0
    for i in range(0, 18, 2):
        xl, xr = enc_block(xl, xr, p, S)
        p[i], p[i + 1] = xl, xr
    s = [row[:] for row in S]
    for box in range(4):
        for i in range(0, 256, 2):
            xl, xr = enc_block(xl, xr, p, s)
            s[box][i], s[box][i + 1] = xl, xr
    return p, s


def enc_block(xl, xr, p, s):
    xl ^= p[0]
    for i in range(1, 17):
        xr ^= (F(xl, s) ^ p[i]) & M
        xl, xr = xr, xl
    xl, xr = xr, xl
    xr ^= p[17]
    xl ^= p[16]
    return xl, xr


def dec_block(xl, xr, p, s):
    xl ^= p[17]
    for i in range(16, 0, -1):
        xr ^= (F(xl, s) ^ p[i]) & M
        xl, xr = xr, xl
    xl, xr = xr, xl
    xr ^= p[0]
    xl ^= p[1]
    return xl, xr


def F(xl, s):
    return (((s[0][(xl >> 24) & 0xFF] + s[1][(xl >> 16) & 0xFF]) & M)
            ^ s[2][(xl >> 8) & 0xFF] + s[3][xl & 0xFF]) & M


def ecb_crypt(blob, decrypt=True):
    p, s = key_schedule(KEY)
    out = bytearray()
    for i in range(0, len(blob), 8):
        xl, xr = struct.unpack(">2I", blob[i:i + 8])
        if decrypt:
            xl, xr = dec_block(xl, xr, p, s)
        else:
            xl, xr = enc_block(xl, xr, p, s)
        out += struct.pack(">2I", xl, xr)
    return bytes(out)


def score(d):
    return sum(1 for x in d if 32 <= x < 127 or x in (0, 9, 10, 13)) / max(len(d), 1)


def main():
    log = LOG.read_text(encoding="utf-8")
    m = re.findall(r"GOT len=(\d+) hex=\[([0-9A-F ]+)\]", log)
    b = bytes(int(x, 16) for x in m[1][1].split())
    print("packet len:", len(b), "prefix LE16:", int.from_bytes(b[:2], "little"))
    payload = b[2:]
    for name, blob in (("full-payload", payload), ("skip8", payload[8:]),
                       ("skip4", payload[4:])):
        if len(blob) % 8:
            blob = blob[:len(blob) // 8 * 8]
        d = ecb_crypt(blob, decrypt=True)
        print("%s score=%.3f head=%r" % (name, score(d), d[:72]))


if __name__ == "__main__":
    main()
