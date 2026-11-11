"""Test key-maker alone: print first/last 4 subkeys for each key."""
import struct
import pathlib

HERE = pathlib.Path(__file__).parent

# Import schedule from custom128 (uses ctables.pkl)
import pickle
T = pickle.load(open(HERE / "ctables.pkl", "rb"))
S1a, S1b, S1c, S1d = T["S1a"], T["S1b"], T["S1c"], T["S1d"]
S2a, S2b = T["S2a"], T["S2b"]

# Import schedule function from custom128 (FUN_0040d370)
import importlib.util
spec = importlib.util.spec_from_file_location("custom128", HERE / "custom128.py")
custom128 = importlib.util.module_from_spec(spec)

# Need to run custom128 setup first (load tables, define functions)
M = 0xFFFFFFFF
KEY = b"891882f7d4235da\x00"
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
    """72 subkey words from password (key16 is LE uint32 array)."""
    k = list(struct.unpack("<4I", key16))
    L = [0] * 80
    L[4], L[5], L[6], L[7] = 0xB7E15162, 0x8AED2A6A, 0xBF715880, 0x9CF4F3C7
    L[0], L[1], L[2], L[3] = 0x243F6A88, 0x85A308D3, 0x13198A2E, 0x03707344
    v9, v4, v2, v10 = k[0], k[1], k[2], k[3]
    wp = 8
    for _ in range(18):
        v8 = ((v10 + L[7]) & M) ^ ((v4 + L[5]) & M)
        v6 = ((v2 ^ L[6]) + (v9 ^ L[4])) & M
        v2 = S1(v8)
        v10 = S2(v2)
        v2 = S1(v6)
        v2 = S2(v2)
        v4 = ((v4 + L[5]) & M) ^ (v2 + v10) & M
        v10 = ((v9 ^ L[4]) + v2 + v10 * 2) & M
        b = v10 & 31
        v10 ^= L[0]
        L[wp - 1] = v10
        v2 = (ROR(v8, b) + L[3]) & M
        v9 = (v4 + L[1]) & M
        b = v4 & 31
        L[wp] = v9
        v4 = L[2] ^ ROL(v6, b)
        L[wp + 1] = v4
        L[wp + 2] = v2
        o0, o1, o2, o3 = L[0], L[1], L[2], L[3]
        L[4], L[5], L[6], L[7] = o0, o1, o2, o3
        L[0], L[1], L[2], L[3] = L[8], L[9], L[10], L[11]
        wp += 4
    KK = []
    i = 0
    while i < 0x318:
        KK.append(L[(i % 0x48) + 8])
        i += 0xB
    assert len(KK) == 72, f"expected 72 subkeys got {len(KK)}"
    return KK

KEYS = [b"891882f7d4235da", b"5fe02a7a34bcf778"]
for key in KEYS:
    # Pad to 16 bytes if needed (first key is 15, second is already 16)
    key = key + b"\x00" if len(key) < 16 else key
    KK = schedule(key)
    first4 = [hex(x) for x in KK[:4]]
    last4 = [hex(x) for x in KK[-4:]]
    print(f"KEY={key.hex()} subkeys=first4={first4} last4={last4}")
