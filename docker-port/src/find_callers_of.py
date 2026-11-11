"""find_callers_of - E8 callers of an address, with each caller fn's strings."""
import struct, sys
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
IMG = 0x400000
SECT = [(0x1000, 6058496, 0x400), (0x5c9000, 1182208, 0x5c7600),
        (0x6ea000, 252928, 0x6e8000), (0xd16000, 1173504, 0x725c00)]


def off(va):
    rva = va - IMG
    for v, sz, raw in SECT:
        if v <= rva < v + sz:
            return raw + (rva - v)
    return None


def va_of(o):
    for v, sz, raw in SECT:
        if raw <= o < raw + sz:
            return IMG + v + (o - raw)
    return None


def rdstr(va, n=64):
    o = off(va)
    if o is None:
        return None
    b = d[o:o + n]
    z = b.find(b"\0")
    if z <= 4:
        return None
    s = b[:z].decode("latin-1", "replace")
    if sum(32 <= c < 127 for c in b[:z]) < z * 0.85:
        return None
    return s


def containing(va):
    o = off(va)
    seg = d[o - 8192:o]
    j = 0; best = None
    while True:
        j = seg.find(b"\x55\x8b\xec", j)
        if j < 0:
            break
        best = o - 8192 + j; j += 1
    return va_of(best) if best else None


target = int(sys.argv[1], 16)
text = d[0x400:0x400 + 6058496]
callers = []
for i in range(len(text) - 5):
    if text[i] != 0xE8:
        continue
    rel = struct.unpack_from("<i", text, i + 1)[0]
    v = va_of(0x400 + i)
    if v + 5 + rel == target:
        callers.append(v)
print("callers of %#x:" % target, [hex(c) for c in callers][:40])

seen = set()
for c in callers[:40]:
    fn = containing(c)
    if not fn or fn in seen:
        continue
    seen.add(fn)
    fo = off(fn)
    body = d[fo:fo + 0x1400]
    strs = []
    for k in range(len(body) - 3):
        s = rdstr(struct.unpack_from("<I", body, k)[0])
        if s:
            strs.append(s)
    tag = [s for s in strs if s in ("barrackteamcheck", "selectteam", "commandername", "TeamSelect")]
    print("fn %#x  interesting=%s  (%d strs)" % (fn, tag, len(strs)))
