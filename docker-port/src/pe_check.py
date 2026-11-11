"""PE triage for the shipped binaries."""
import struct, math
from collections import Counter
from ge_paths import GAME_ROOT

d = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
print("size", len(d))
e = struct.unpack_from("<I", d, 0x3c)[0]
print("PE sig", d[e:e+4])
mach = struct.unpack_from("<H", d, e + 4)[0]
print("machine", hex(mach), "(0x14c=x86, 0x8664=x64)")
nsect = struct.unpack_from("<H", d, e + 6)[0]
print("sections", nsect)
print("timestamp", struct.unpack_from("<I", d, e + 8)[0])
oh = e + 24
magic = struct.unpack_from("<H", d, oh)[0]
print("opt magic", hex(magic))
sect = oh + struct.unpack_from("<H", d, e + 20)[0]
for i in range(nsect):
    o = sect + i * 40
    name = d[o:o + 8].rstrip(b"\0").decode("latin1")
    vs, va, rs, ro = struct.unpack_from("<IIII", d, o + 8)
    chunk = d[ro:ro + rs]
    if chunk:
        c = Counter(chunk); n = len(chunk)
        ent = -sum((v / n) * math.log2(v / n) for v in c.values())
    else:
        ent = 0
    print(f"{name:8s} va={va:#010x} vsize={vs:8d} raw={rs:8d} off={ro:#x} ent={ent:.2f}")

# search for some UI keys we care about
for needle in [b"commander_lv_current", b"commandername", b"commander_name",
               b"commander_lv_max", b"party_name", b"selectteam", b"barrackteamcheck"]:
    idx = d.find(needle)
    print(needle.decode(), "->", hex(idx) if idx >= 0 else "not found")
