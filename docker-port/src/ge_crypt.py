"""ge_crypt - the Granado Espada network cipher, in one place.

Cipher: stock Blowfish, ECB, 8-byte blocks (imcCrypt::NetEncrypt).
Wire framing (imcCrypt::NetEncrypt / NetDecrypt):
    wire[0:2] = WORD(blockByte)          little-endian, blockByte = ceil(len/8)*8
    wire[2:2+blockByte] = ciphertext

Key and tables come from the client source (verified against the shipped
binary and against two real captures):
  - key      : geCrypt key_table row 11  (isVertical=true, useArrayIndex=11)
  - tables   : bf_dummy_table reassembled with part_num={16,2,256,768},
               seek={4,1056,24,284}.  The client's table is NOT the stock
               pi table, so the stock test vectors do not apply here.

Usage:
    import ge_crypt
    packet = ge_crypt.decrypt_packet(wire_bytes)   # client -> you (plaintext)
    wire   = ge_crypt.encrypt_packet(packet_bytes)  # you -> client
"""
import os
import pickle
import re
import struct

from ge_paths import CAPTURES, GAME_ROOT

M = 0xFFFFFFFF

KEY = b"hsunffalqyrqewes"

# Source of the scrambled table (build-time only; result is cached to a .pkl).
TABLE_SOURCE = os.path.join(GAME_ROOT, "source2", "geShared", "Source", "CryptTable.cpp")
_HERE = os.path.dirname(os.path.abspath(__file__))
_CACHE = os.path.join(_HERE, "bf_tables.pkl")


# ---------------------------------------------------------------- tables
def _reassemble(path):
    txt = open(path, encoding="utf-8", errors="ignore").read()
    body = txt.split("bf_dummy_table", 1)[1].split("};", 1)[0]
    nums = [int(x, 16) for x in re.findall(r"0x([0-9A-Fa-f]{8})L?", body)]
    part_num = [int(x) for x in re.search(
        r"tablePartNum\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    seek = [int(x) for x in re.search(
        r"tableSeekPointer\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    tbl = []
    for n, s in zip(part_num, seek):
        tbl += nums[s:s + n]
    if len(tbl) != 1042:
        raise ValueError("unexpected table size %d" % len(tbl))
    P = tbl[:18]
    S = [tbl[18 + k * 256:18 + (k + 1) * 256] for k in range(4)]
    return P, S


def load_tables():
    if os.path.exists(_CACHE):
        with open(_CACHE, "rb") as f:
            return pickle.load(f)
    P, S = _reassemble(TABLE_SOURCE)
    with open(_CACHE, "wb") as f:
        pickle.dump((P, S), f)
    return P, S


P_INIT, S_INIT = load_tables()


# ---------------------------------------------------------------- blowfish
def _F(x, s):
    a = (x >> 24) & 0xFF
    b = (x >> 16) & 0xFF
    c = (x >> 8) & 0xFF
    d = x & 0xFF
    return ((((s[0][a] + s[1][b]) & M) ^ s[2][c]) + s[3][d]) & M


def _enc(l, r, p, s):
    for i in range(16):
        l ^= p[i]
        r ^= _F(l, s)
        l, r = r, l
    l, r = r, l
    r ^= p[16]
    l ^= p[17]
    return l, r


def _dec(l, r, p, s):
    for i in range(17, 1, -1):
        l ^= p[i]
        r ^= _F(l, s)
        l, r = r, l
    l, r = r, l
    r ^= p[1]
    l ^= p[0]
    return l, r


def key_schedule(key):
    p = list(P_INIT)
    j = 0
    for i in range(18):
        d = 0
        for _ in range(4):
            d = ((d << 8) | key[j % len(key)]) & M
            j += 1
        p[i] ^= d
    s = [row[:] for row in S_INIT]
    l = r = 0
    for i in range(0, 18, 2):
        l, r = _enc(l, r, p, s)
        p[i], p[i + 1] = l, r
    for box in range(4):
        for i in range(0, 256, 2):
            l, r = _enc(l, r, p, s)
            s[box][i], s[box][i + 1] = l, r
    return p, s


_KP, _KS = key_schedule(KEY)


# ---------------------------------------------------------------- packets
def decrypt_packet(wire):
    """wire = the bytes the client sent (length header + ciphertext)."""
    block = struct.unpack_from("<H", wire, 0)[0]
    ct = wire[2:2 + block]
    out = bytearray()
    for i in range(0, len(ct) - (len(ct) % 8), 8):
        l, r = struct.unpack(">2I", ct[i:i + 8])
        l, r = _dec(l, r, _KP, _KS)
        out += struct.pack(">2I", l, r)
    return bytes(out)


def encrypt_packet(plain):
    """plain = the plaintext packet. Returns bytes ready to send to the client."""
    n = (len(plain) + 7) // 8
    block = n * 8
    buf = bytearray(block)
    buf[:len(plain)] = plain
    for i in range(0, block, 8):
        l, r = struct.unpack(">2I", buf[i:i + 8])
        l, r = _enc(l, r, _KP, _KS)
        buf[i:i + 8] = struct.pack(">2I", l, r)
    return struct.pack("<H", block) + bytes(buf)


# ---------------------------------------------------------------- self test
def _self_test():
    import glob
    caps = []
    log = os.path.join(CAPTURES, "barrack_stub.log")
    if os.path.exists(log):
        txt = open(log, encoding="utf-8", errors="ignore").read()
        ms = re.findall(r"hex=\[(.*?)\]", txt)
        pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
        caps += [("old", p) for p in pkts if len(p) == 818]
    for pth in sorted(glob.glob(os.path.join(CAPTURES, "cap_*.log"))):
        t = open(pth, encoding="utf-8", errors="ignore").read()
        m = re.search(r"hex=\[(.*?)\]", t)
        caps.append((os.path.basename(pth), bytes(int(x, 16) for x in m.group(1).split())))
    ok = 0
    for name, wire in caps:
        pt = decrypt_packet(wire)
        has_id = (b"Arcanine" in pt) or (b"Arcis" in pt)
        rt = decrypt_packet(encrypt_packet(pt)) == pt
        print("%-12s id=%-5s round-trip=%s  head=%r"
              % (name, has_id, rt, pt[:26]))
        ok += (has_id and rt)
    print("decrypt+round-trip OK on %d/%d captures" % (ok, len(caps)))
    return ok == len(caps)


if __name__ == "__main__":
    print("key:", KEY)
    print("tables cached:", os.path.exists(_CACHE))
    _self_test()
