"""Dump the BC_BARRACK_INFO packet fields."""
import struct
from ge_paths import GAME_ROOT
d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
TEXT_VA, TEXT_RAW = 0x1000, 0x400


def off(va):
    return va - 0x400000 - TEXT_VA + TEXT_RAW


def dump(va, n):
    o = off(va)
    for i in range(0, n, 16):
        row = d[o + i:o + i + 16]
        print("%08X  %-47s  %s" % (va + i, " ".join("%02X" % b for b in row),
                                   "".join(chr(b) if 32 <= b < 127 else "." for b in row)))


print("--- BarrackInfo loop / call site (fn 0x521784) ---")
dump(0x521A00, 0x150)
print()
print("--- function prologue ---")
dump(0x521784, 0x60)
