"""strs_in_fn - list strings referenced by 4-byte immediates inside functions.

Given a function range, scan for dword operands that point into the image and
print the ASCII string they reference. Cheap way to name an unknown function.
"""
import struct
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
IMG = 0x400000
SECTIONS = [(0x1000, 6058496, 0x400), (0x5c9000, 1182208, 0x5c7600),
            (0x6ea000, 252928, 0x6e8000), (0xd16000, 1173504, 0x725c00)]


def off_of_va(va):
    rva = va - IMG
    for vsec, vsize, raw in SECTIONS:
        if vsec <= rva < vsec + vsize:
            return raw + (rva - vsec)
    return None


def read_str(va, n=48):
    o = off_of_va(va)
    if o is None:
        return None
    b = d[o:o + n]
    z = b.find(b"\0")
    if z <= 3:
        return None
    s = b[:z].decode("latin-1", "replace")
    if sum(32 <= c < 127 for c in b[:z]) < len(s) * 0.8:
        return None
    return s


def scan(va_start, length=0x600):
    o = off_of_va(va_start)
    if o is None:
        print("  (va not mapped) %#x" % va_start)
        return
    body = d[o:o + length]
    found = {}
    for i in range(len(body) - 3):
        v = struct.unpack_from("<I", body, i)[0]
        s = read_str(v)
        if s:
            found[v] = s
    print("== fn %#x: %d string refs ==" % (va_start, len(found)))
    for v, s in list(found.items())[:25]:
        print("   %#x  %r" % (v, s[:60]))


for fn in [0x5251e5, 0x60de1d, 0x4b569f, 0x52048d, 0x521d4b, 0x90aa22]:
    scan(fn)
