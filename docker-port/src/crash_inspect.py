"""crash_inspect - dump bytes around the client crash RVA 0x1ad488."""
import struct
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
TEXT_VA, TEXT_RAW = 0x1000, 0x400


def rva_to_off(rva):
    return rva - TEXT_VA + TEXT_RAW


crash_rva = 0x001AD488
off = rva_to_off(crash_rva)
print("crash RVA %#x -> file offset %#x, VA %#x" % (crash_rva, off, 0x400000 + crash_rva))


def dump(start, n):
    for i in range(0, n, 16):
        row = d[start + i:start + i + 16]
        print("%08X  %-47s  %s" % (0x400000 + TEXT_VA + (start + i - TEXT_RAW),
                                   " ".join("%02X" % b for b in row),
                                   "".join(chr(b) if 32 <= b < 127 else "." for b in row)))


print("--- 48 bytes before the fault ---")
dump(off - 48, 48)
print("--- fault instruction onward ---")
dump(off, 48)

# look back for a function prologue: 55 8B EC (push ebp; mov ebp,esp)
print("--- prologue candidates in the 4KB before ---")
seg = d[off - 4096:off + 16]
base = off - 4096
cands = []
i = 0
while i >= 0:
    i = seg.find(b"\x55\x8b\xec", i)
    if i < 0:
        break
    cands.append(base + i)
    i += 1
for c in cands[-8:]:
    print("   prologue at RVA %#x (VA %#x)" % (c - TEXT_RAW + TEXT_VA, 0x400000 + c - TEXT_RAW + TEXT_VA))
