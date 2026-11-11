"""Inspect a function in ge.exe (disassembly / strings)."""
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


def rdstr(va, n=60):
    o = off(va)
    if o is None:
        return None
    b = d[o:o + n]
    z = b.find(b"\0")
    if z <= 3:
        return None
    s = b[:z].decode("latin-1", "replace")
    if sum(32 <= c < 127 for c in b[:z]) < z * 0.8:
        return None
    return s


va = int(sys.argv[1], 16)
o = off(va)
print("fault VA %#x (rva %#x) file off %#x" % (va, va - IMG, o))
row = d[o - 16:o + 32]
for i in range(0, len(row), 16):
    print("%08X  %-47s  %s" % (va - 16 + i, " ".join("%02X" % b for b in row[i:i + 16]),
                               "".join(chr(b) if 32 <= b < 127 else "." for b in row[i:i + 16])))

# containing function prologue
seg = d[o - 8192:o]
j = 0; best = None
while True:
    j = seg.find(b"\x55\x8b\xec", j)
    if j < 0:
        break
    best = o - 8192 + j; j += 1
fn = IMG + 0x1000 + (best - 0x400) if best else None
print("containing fn prologue VA:", hex(fn) if fn else None)

if fn:
    fo = off(fn)
    body = d[fo:fo + 0x1200]
    found = {}
    for i in range(len(body) - 3):
        v = struct.unpack_from("<I", body, i)[0]
        s = rdstr(v)
        if s:
            found[v] = s
    print("strings referenced by fn %#x:" % fn)
    for v, s in list(found.items())[:20]:
        print("   %#x %r" % (v, s[:50]))
    # look for the constant 326 / 157 near the fault
    near = d[o - 400:o + 16]
    for c in (326, 157, 274, 105):
        pat = struct.pack("<I", c)
        idx = near.find(pat)
        print("   imm %d present near fault: %s" % (c, idx >= 0))
