"""Custom 128-bit cipher copy, transliterated line-by-line from ge.exe
(Ghidra: core FUN_0040d5f0, schedule FUN_0040d370, mode-3 CFB FUN_0040fc00).
Tables: ctables.pkl (lifted from ge.exe). Key: b'891882f7d4235da'.
Refs like [C24] = ghidra_bffull.txt line 24, [S37] = ghidra_sched.txt.
Run: python custom128.py  (decrypts the captured hello)
"""
import struct
import pickle
import pathlib
import re

from ge_paths import CAPTURES, DATA

M = 0xFFFFFFFF
HERE = pathlib.Path(__file__).parent
T = pickle.load(open(HERE / "ctables.pkl", "rb"))
S1a, S1b, S1c, S1d = T["S1a"], T["S1b"], T["S1c"], T["S1d"]
S2a, S2b = T["S2a"], T["S2b"]
KEY = b"891882f7d4235da\x00"  # 15-char string + zero pad to 16
IV = [0x12345678, 0x56785678, 0x9ABC5678, 0xDEFF5678]


def ROL(x, r):
    r &= 31
    return ((x << r) | (x >> ((32 - r) & 31))) & M


def ROR(x, r):
    r &= 31
    return ((x >> r) | (x << ((32 - r) & 31))) & M


def S1(x):
    return (S1a[(x >> 16) & 0xFF] ^ S1b[(x >> 8) & 0xFF] ^
            S1c[(x >> 24) & 0xFF] ^ S1d[x & 0xFF]) & M


def S2(x):
    t = (S2a[(x >> 16) & 0xFF] ^ ((S2b[(x >> 24) & 0xFF] << 8) & M)) & M
    t = ((t << 8) & M) ^ S2b[(x >> 8) & 0xFF]
    t = ((t << 8) & M) ^ S2a[x & 0xFF]
    return t & M


def schedule(key16):
    """FUN_0040d370 -> 72 subkey words. Key words are LE of key bytes."""
    k = list(struct.unpack("<4I", key16))
    L = [0] * 80
    L[4], L[5], L[6], L[7] = 0xB7E15162, 0x8AED2A6A, 0xBF715880, 0x9CF4F3C7
    L[0], L[1], L[2], L[3] = 0x243F6A88, 0x85A308D3, 0x13198A2E, 0x03707344
    v9, v4, v2, v10 = k[0], k[1], k[2], k[3]  # [S22-25]
    wp = 8
    for _ in range(18):  # [S35-75]
        v8 = ((v10 + L[7]) & M) ^ ((v4 + L[5]) & M)  # [S37]
        v6 = ((v2 ^ L[6]) + (v9 ^ L[4])) & M  # [S38]
        v2 = S1(v8)  # [S39-42]
        v10 = S2(v2)  # [S43-46]
        v2 = S1(v6)  # [S47-50]
        v2 = S2(v2)  # [S51-54]
        v4 = ((v4 + L[5]) & M) ^ ((v2 + v10) & M)  # [S55]
        v10 = ((v9 ^ L[4]) + v2 + v10 * 2) & M  # [S56]
        b = v10 & 31  # [S57]
        v10 ^= L[0]  # [S58]
        L[wp - 1] = v10  # [S59]
        v2 = (ROR(v8, b) + L[3]) & M  # [S60]
        v9 = (v4 + L[1]) & M  # [S61]
        b = v4 & 31  # [S62]
        L[wp] = v9  # [S63]
        v4 = L[2] ^ ROL(v6, b)  # [S64]
        L[wp + 1] = v4  # [S65]
        L[wp + 2] = v2  # [S66]
        o0, o1, o2, o3 = L[0], L[1], L[2], L[3]  # [S67-72]
        L[4], L[5], L[6], L[7] = o0, o1, o2, o3
        L[0], L[1], L[2], L[3] = L[8], L[9], L[10], L[11]
        wp += 4
    KK = []  # [S76-83]
    i = 0
    while i < 0x318:
        KK.append(L[(i % 0x48) + 8])
        i += 0xB
    assert len(KK) == 72, len(KK)
    return KK


def core(w, K):
    """FUN_0040d5f0: 4 BE words -> 4 words. Literal transcription."""
    v5 = (w[0] + K[0]) & M  # [C23]
    v4 = S2(S1(v5))  # [C24-31]
    v7 = ((w[1] ^ K[1]) + v4) & M  # [C32]
    v6 = S1(v7)  # [C33-36]
    v8 = S2(v6)  # [C37-40]
    b1 = v5 & 31  # [C41]
    v4 = (((w[2] + K[2]) & M) ^ v4) + v8 & M  # [C42]
    v4 &= M
    b2 = v7 & 31  # [C43]
    v6 = S1(v4)  # [C44-47]
    v9 = S2(v6)  # [C48-51]
    v8 = ((ROR(w[3] ^ K[3], b1) ^ v8) + v9) & M  # [C52-53]
    v6 = S1(v8)  # [C54-57]
    b1 = v4 & 31  # [C58]
    v6 = S2(v6)  # [C59-62]
    v9 = ((ROR(v5, b2) ^ v9) + v6) & M  # [C63]
    v5 = S1(v9)  # [C64-67]
    v10 = S2(v5)  # [C68-71]
    v6 = ((ROR(v7, b1) ^ v6) + v10) & M  # [C72]
    b1 = v8 & 31  # [C73]
    v5 = S1(v6)  # [C74-77]
    v5 = S2(v5)  # [C78-81]
    b2 = v9 & 31  # [C82]
    v7 = ((ROR(v4, b1) ^ v10) + v5) & M  # [C83]
    v4 = S1(v7)  # [C84-87]
    b3 = v7 & 31  # [C88]
    v10 = S2(v4)  # [C89-92]
    b1 = v6 & 31  # [C93]
    v5 = ((ROR(v8, b2) ^ v5) + v10) & M  # [C94]
    v4 = S1(v5)  # [C95-98]
    v8 = S2(v4)  # [C99-102]
    v4 = ((ROL(v6, b3) ^ v8) + K[5]) & M  # [C103]
    v6 = (((ROL(v9, b1) ^ v10) + v8) & M) ^ K[4]  # [C104]
    b1 = v5 & 31  # [C105]
    v5 ^= v4  # [C106]
    v7 = (ROL(v7, b1) + v6) & M  # [C107]
    v4 = (v4 ^ (v5 + v7)) & M  # [C108]
    v6 = (v6 + v7 + v5 * 2) & M  # [C109]
    # G1..G11 [C110-197]: roles cycle every 4 groups.
    v4, v7, v6, v5 = g(v4, v7, v6, v5, K, 0x06)  # G1
    v7, v5, v4, v6 = g(v7, v5, v4, v6, K, 0x0A)  # G2
    v5, v6, v7, v4 = g(v5, v6, v7, v4, K, 0x0E)  # G3
    v6, v4, v5, v7 = g(v6, v4, v5, v7, K, 0x12)  # G4
    v4, v7, v6, v5 = g(v4, v7, v6, v5, K, 0x16)  # G5
    v7, v5, v4, v6 = g(v7, v5, v4, v6, K, 0x1A)  # G6
    v5, v6, v7, v4 = g(v5, v6, v7, v4, K, 0x1E)  # G7
    v6, v4, v5, v7 = g(v6, v4, v5, v7, K, 0x22)  # G8
    v4, v7, v6, v5 = g(v4, v7, v6, v5, K, 0x26)  # G9
    v7, v5, v4, v6 = g(v7, v5, v4, v6, K, 0x2A)  # G10
    v5, v6, v7, v4 = g(v5, v6, v7, v4, K, 0x2E)  # G11
    # G12 [C198-205]
    b1 = v6 & 31  # [C198]
    v6 ^= K[0x34]  # [C199]
    v8 = ((ROL(v4, b1) ^ K[0x32]) + K[0x35]) & M  # [C200]
    b1 = v5 & 31  # [C201]
    v5 ^= v8  # [C202]
    v4 = (ROL(v7, b1) + K[0x33] + v6) & M  # [C203]
    v8 = (v8 ^ (v5 + v4)) & M  # [C204]
    v6 = (v6 + v4 + v5 * 2) & M  # [C205]
    # G13 [C206-213]
    b1 = v8 & 31  # [C206]
    v8 ^= K[0x38]  # [C207]
    v9 = ((ROL(v4, b1) ^ K[0x36]) + K[0x39]) & M  # [C208]
    b1 = v6 & 31  # [C209]
    v6 ^= v9  # [C210]
    v4 = ((ROL(v5, b1) ^ K[0x37]) + v8) & M  # [C211]
    v9 = (v9 ^ (v4 + v6)) & M  # [C212]
    v8 = (v8 + v4 + v6 * 2) & M  # [C213]
    # G14a [C214-221]
    b1 = v9 & 31  # [C214]
    v9 ^= K[0x3C]  # [C215]
    v4 = ((ROL(v4, b1) ^ K[0x3A]) + K[0x3D]) & M  # [C216]
    b1 = v8 & 31  # [C217]
    v8 ^= v4  # [C218]
    v5 = (ROL(v6, b1) + K[0x3B] + v9) & M  # [C219]
    v4 = (v4 ^ (v5 + v8)) & M  # [C220]
    v9 = (v9 + v5 + v8 * 2) & M  # [C221]
    # G14b [C222-229]
    b1 = v4 & 31  # [C222]
    v4 ^= K[0x40]  # [C223]
    v7 = ((ROL(v5, b1) ^ K[0x3E]) + K[0x41]) & M  # [C224]
    b1 = v9 & 31  # [C225]
    v9 ^= v7  # [C226]
    v5 = (ROL(v8, b1) + K[0x3F] + v4) & M  # [C227]
    v7 = (v7 ^ (v9 + v5)) & M  # [C228]
    v4 = (v4 + v5 + v9 * 2) & M  # [C229]
    # tail S-mix [C230-316]
    b1 = v4 & 31  # [C230]
    v6 = (ROL(v9, b1) + K[0x43]) & M  # [C231]
    v8 = S1(v4)  # [C232-235]
    v9 = S2(v8)  # [C236-239]
    v8 = ROR(v6, b1)  # [C240]
    b1 = v7 & 31  # [C241]
    v6 = ((ROL(v5, b1) ^ K[0x42]) ^ v9) & M  # [C242]
    b1 = v8 & 31  # [C243] FIX 2026-10-15: Ghidra sets bVar1 = uVar8 & 31 here
    v5 = S1(v8)  # [C244-247]
    v10 = S2(v5)  # [C248-251]
    v6 = ROR(v6, b1)  # [C252] (b1 = v8&31 from [C243])
    v7 = ((v7 - v9) & M) ^ v10  # [C253]
    v5 = S1(v6)  # [C254-257]
    v5 = S2(v5)  # [C258-261]
    b1 = v6 & 31  # [C262]
    v4 = ((v4 - v10) & M) ^ v5  # [C263]
    v7 = ROR(v7, b1)  # [C264]
    v9 = S1(v7)  # [C265-268]
    b1 = v7 & 31  # [C269]
    v9 = S2(v9)  # [C270-273]
    v5 = ((v8 - v5) & M) ^ v9  # [C274]
    v8 = ROR(v4, b1)  # [C275]
    b1 = v8 & 31  # [C276]
    v4 = S1(v8)  # [C277-280]
    v10 = S2(v4)  # [C281-284]
    v9 = ((v6 - v9) & M) ^ v10  # [C285]
    v6 = ROR(v5, b1)  # [C286]
    v4 = S1(v6)  # [C287-290]
    v5 = S2(v4)  # [C291-294]
    b1 = v6 & 31  # [C295]
    v4 = ((v7 - v10) & M) ^ v5  # [C296]
    v7 = ROR(v9, b1)  # [C297]
    v9 = S1(v7)  # [C298-301]
    v9 = S2(v9)  # [C302-305]
    b1 = v7 & 31  # [C306]
    v8 = ((v8 - v5) & M) ^ v9  # [C307]
    v4 = ROR(v4, b1)  # [C308]
    v5 = S1(v4)  # [C309-312]
    v5 = S2(v5)  # [C313-316]
    o0 = (v4 - K[0x44]) & M  # [C317]
    o1 = ((v7 - v5) & M) ^ K[0x45]  # [C318]
    o2 = ((v5 ^ ((v6 - v9) & M)) - K[0x46]) & M  # [C319]
    b1 = v4 & 31  # [C320]
    o3 = ROR(v8, b1) ^ K[0x47]  # [C321]
    return (o0, o1, o2, o3)


def g(A, B, C, D, K, base):
    """Standard 8-line group, roles (A,B,C,D). Returns new tuple."""
    b = A & 31
    A ^= K[base + 2]
    B = ((ROL(B, b) ^ K[base]) + K[base + 3]) & M
    b = C & 31
    C = (C ^ B) & M
    D = (ROL(D, b) + K[base + 1] + A) & M
    B = (B ^ (C + D)) & M
    A = (A + D + C * 2) & M
    return A, B, C, D


def cfb_decrypt(ct, K):
    """Mode-3 CFB decipher: chain=IV then prev-ciphertext; pt = core(chain)^ct."""
    assert len(ct) % 16 == 0
    chain = IV[:]
    pt = bytearray()
    for i in range(0, len(ct), 16):
        blk = struct.unpack(">4I", ct[i:i + 16])
        ks = core(chain, K)
        pt += struct.pack(">4I", *(a ^ b for a, b in zip(ks, blk)))
        chain = list(blk)
    return bytes(pt)


def score(d):
    return sum(1 for x in d if 32 <= x < 127 or x in (0, 9, 10, 13)) / max(len(d), 1)


def main():
    K = schedule(KEY)
    print("subkeys:", len(K), "K0=%08X K71=%08X" % (K[0], K[71]))
    log = (pathlib.Path(CAPTURES) / "barrack_stub.log").read_text(encoding="utf-8")
    m = re.findall(r"GOT len=(\d+) hex=\[([0-9A-F ]+)\]", log)
    b = bytes(int(x, 16) for x in m[1][1].split())
    print("packet:", len(b), "prefix:", int.from_bytes(b[:2], "little"))
    d = cfb_decrypt(b[2:], K)
    print("score=%.3f" % score(d))
    print("head:", d[:96])
    (pathlib.Path(DATA) / "hello_plain.bin").write_bytes(d)
    print("wrote hello_plain.bin")


if __name__ == "__main__":
    main()
