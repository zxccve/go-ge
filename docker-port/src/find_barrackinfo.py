"""find_barrackinfo - locate the client's BarrackInfo loop and its PC_INFO stride."""
import struct
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


def va_of(off_):
    for v, sz, raw in SECT:
        if raw <= off_ < raw + sz:
            return IMG + v + (off_ - raw)
    return None


def find_str(s):
    b = s if isinstance(s, bytes) else s.encode()
    i = d.find(b)
    while i >= 0:
        yield i
        i = d.find(b, i + 1)


for needle in (b"barrackteamcheck", b"selectteam"):
    for o in find_str(needle):
        print(needle.decode(), "at file", hex(o), "VA", hex(va_of(o)))

# xref "barrackteamcheck" (VA) inside .text
needle = b"barrackteamcheck\x00"
o = d.find(needle)
sva = va_of(o)
print("\nbarrackteamcheck VA", hex(sva))
text = d[0x400:0x400 + 6058496]
refs = []
i = text.find(struct.pack("<I", sva))
while i >= 0:
    refs.append(0x400 + i)
    i = text.find(struct.pack("<I", sva), i + 1)
print("refs:", [hex(va_of(r)) for r in refs])

for r in refs[:4]:
    ro = r
    seg = d[max(0, ro - 8192):ro]
    j = 0; best = None
    while True:
        j = seg.find(b"\x55\x8b\xec", j)
        if j < 0:
            break
        best = max(0, ro - 8192) + j; j += 1
    if not best:
        continue
    fn = va_of(best)
    print("\n== fn %#x, immediates 100..1200 in next 0x1000 bytes ==" % fn)
    body = d[best:best + 0x1000]
    hits = []
    for k in range(len(body) - 4):
        v = struct.unpack_from("<I", body, k)[0]
        if 100 <= v <= 1200:
            hits.append((best + k, v))
    for pos, v in hits[:30]:
        print("   off %#x imm %d" % (va_of(pos), v))
