"""callers_of - find E8 rel32 calls targeting a VA, and show the containing fn."""
import struct
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
TEXT_VA, TEXT_RAW, TEXT_SIZE = 0x1000, 0x400, 6058496
IMG = 0x400000


def rva2off(rva):
    return rva - TEXT_VA + TEXT_RAW


def off2va(o):
    return IMG + TEXT_VA + (o - TEXT_RAW)


target = int(__import__("sys").argv[1], 16) if len(__import__("sys").argv) > 1 else 0x5AD488

text = d[TEXT_RAW:TEXT_RAW + TEXT_SIZE]
calls = []
for i in range(len(text) - 5):
    if text[i] != 0xE8:
        continue
    rel = struct.unpack_from("<i", text, i + 1)[0]
    va = off2va(TEXT_RAW + i)
    if va + 5 + rel == target:
        calls.append(va)

print("E8 callers of %#x:" % target, [hex(c) for c in calls])


def containing_fn(va):
    off = rva2off(va - IMG)
    seg = d[max(0, off - 8192):off]
    best = None
    j = 0
    while True:
        j = seg.find(b"\x55\x8b\xec", j)
        if j < 0:
            break
        best = max(0, off - 8192) + j
        j += 1
    return best


for c in calls:
    fn = containing_fn(c)
    print("\n--- call at VA %#x, containing fn prologue VA %#x ---" % (c, off2va(fn) if fn else 0))
    start = c - 24
    start_off = rva2off(start - IMG)
    row = d[start_off:start_off + 56]
    for k in range(0, len(row), 16):
        va = off2va(start_off + k)
        print("  %08X  %-47s" % (va, " ".join("%02X" % b for b in row[k:k + 16])))
