"""Known-plaintext test with the CORRECT plaintext guesses.

We now believe the client keeps the login as text. Account field = packet
bytes 10..25 (16 bytes), password field = packet bytes 226..249 (24 bytes).
The account field being exactly 16 bytes fits UTF-16 "Arcanine" exactly.

Tries, for each cipher+key+encoding: does encrypt(guess) == observed bytes?

Run:  python test_known.py
"""
import struct
import re

import bf_test
import custom128 as c128

M = 0xFFFFFFFF

ACCT_CT = bytes.fromhex("de9541f50dbac0556aba6f8384b8b7c4")   # 16 bytes
PW_CT = bytes.fromhex("e83dff1242c2966bc615d4ae1bd33df077301bfdc09f765b")  # 24 bytes


def encodings(word, size):
    out = {}
    a = word.encode("ascii")
    w = word.encode("utf-16-le")
    if len(a) <= size:
        out["ascii+nul"] = a + b"\0" * (size - len(a))
        out["ascii+space"] = a + b" " * (size - len(a))
    if len(w) <= size:
        out["utf16+nul"] = w + b"\0" * (size - len(w))
    out["ascii"] = a
    out["utf16"] = w
    return {k: v for k, v in out.items() if len(v) == size}


def keys():
    ks = {}
    for s in ("891882f7d4235da", "5fe02a7a34bcf778", "123456789012",
              "1234567890123456", "Arcanine", "arcanine"):
        b = s.encode()
        if len(b) < 16:
            ks[s] = b + b"\0" * (16 - len(b))
        ks[s] = b[:16]
    return {k: v for k, v in ks.items() if len(v) == 16}


def try_custom128(label, pt16, ct16):
    for kname, k16 in keys().items():
        K = c128.schedule(k16)
        words = struct.unpack(">4I", pt16)
        enc = struct.pack(">4I", *c128.core(list(words), K))
        if enc == ct16:
            print("HIT custom128-ECB %s key=%s" % (label, kname))
        dec = struct.pack(">4I", *c128.core(list(struct.unpack(">4I", ct16)), K))
        if dec == pt16:
            print("HIT custom128-ECD(dec) %s key=%s" % (label, kname))


def try_blowfish(label, pt8, ct8):
    for kname, k16 in keys().items():
        for klen in (len(kname.encode()), 16):
            key = kname.encode()[:klen] if klen <= 16 else kname.encode()[:16]
            if not key:
                continue
            try:
                p, s = bf_test.key_schedule(key)
            except Exception:
                continue
            xl, xr = struct.unpack(">2I", pt8)
            el, er = bf_test.enc_block(xl, xr, p, s)
            if struct.pack(">2I", el, er) == ct8:
                print("HIT blowfish-ECB %s key=%r" % (label, key))
            xl, xr = struct.unpack(">2I", ct8)
            dl, dr = bf_test.dec_block(xl, xr, p, s)
            if struct.pack(">2I", dl, dr) == pt8:
                print("HIT blowfish-ECB(dec) %s key=%r" % (label, key))


def main():
    print("=== account field (16 bytes) ===")
    for name, pt in encodings("Arcanine", 16).items():
        print(" guess %-12s %s" % (name, pt.hex()))
        try_custom128("acct/" + name, pt, ACCT_CT)
        if len(pt) >= 8:
            try_blowfish("acct-b1/" + name, pt[:8], ACCT_CT[:8])
            try_blowfish("acct-b2/" + name, pt[8:16], ACCT_CT[8:16])

    print("=== password field (24 bytes) ===")
    for name, pt in encodings("123456", 24).items():
        print(" guess %-12s %s" % (name, pt.hex()))
        for i in (0, 8, 16):
            if len(pt) >= i + 16:
                try_custom128("pw%d/%s" % (i, name), pt[i:i + 16], PW_CT[i:i + 16])
            if len(pt) >= i + 8:
                try_blowfish("pw%d/%s" % (i, name), pt[i:i + 8], PW_CT[i:i + 8])
    print("done (no HIT line means no simple text match)")


if __name__ == "__main__":
    main()
