"""Inspect the login packet flow."""
import os
import re, struct, sys
import ge_crypt as gc
from ge_paths import CAPTURES

path = os.path.join(CAPTURES, "cap_01.log")
raw = open(path, "r", encoding="latin-1").read()
m = re.search(r"hex=\[([0-9A-Fa-f ]+)\]", raw)
wire = bytes(int(x, 16) for x in m.group(1).split())
print("wire len", len(wire))
pt = gc.decrypt_packet(wire)
print("pt len", len(pt))
cmd, seq, chk, ln = struct.unpack_from("<HIIH", pt, 0)
print("cmd", cmd, "seq", seq, "chk", chk, "declared len", ln)

# hexdump
def dump(b, base=0):
    for i in range(0, len(b), 16):
        row = b[i:i+16]
        hexs = " ".join("%02X" % c for c in row)
        asc = "".join(chr(c) if 32 <= c < 127 else "." for c in row)
        print("%04X  %-47s  %s" % (base+i, hexs, asc))

dump(pt[:320])
print("...")
dump(pt[320:], 320)
