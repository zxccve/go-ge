import tempfile
"""Known-plaintext hunt: login packet must contain b'Arcanine' / b'123456'.
Tries every core x schedule x mode combo, searches for the name.
Run: python hunt.py
"""
import struct
import re
import pathlib
import itertools

import custom128 as C
import coreB
from ge_paths import CAPTURES

HERE = pathlib.Path(__file__).parent
log = (pathlib.Path(CAPTURES) / "barrack_stub.log").read_text(encoding="utf-8")
m = re.findall(r"GOT len=(\d+) hex=\[([0-9A-F ]+)\]", log)
PACKET = bytes(int(x, 16) for x in m[1][1].split())
CT = PACKET[2:]
assert len(CT) % 16 == 0

NEEDLES = [b"Arcanine", b"ARCANINE", b"arcanine",
           "Arcanine".encode("utf-16le"), b"123456",
           "123456".encode("utf-16le")]

stmtsB = coreB.load_statements()
SCHEDS = {
    "key1": C.schedule(C.KEY),
    "key2": C.schedule(b"5fe02a7a34bcf778"),
    "key3": C.schedule(b"123456789012\x00\x00\x00\x00"),
}


def ecb_decrypt(core, K, stmts):
    out = bytearray()
    for i in range(0, len(CT), 16):
        w = struct.unpack(">4I", CT[i:i + 16])
        o = core(w, K, stmts) if stmts is not None else core(w, K)
        out += struct.pack(">4I", *o)
    return bytes(out)


def cfb_decrypt(core, K, stmts):
    chain = C.IV[:]
    out = bytearray()
    for i in range(0, len(CT), 16):
        blk = struct.unpack(">4I", CT[i:i + 16])
        if stmts is not None:
            ks = core(chain, K, stmts)
        else:
            ks = core(chain, K)
        out += struct.pack(">4I", *(a ^ b for a, b in zip(ks, blk)))
        chain = list(blk)
    return bytes(out)


def check(name, data):
    hits = [n for n in NEEDLES if n in data]
    tailhex = bool(re.fullmatch(rb"[0-9a-f]{32}", data[-32:]))
    score = sum(1 for x in data if 32 <= x < 127 or x in (0, 9, 10, 13)) / len(data)
    flag = " ***HIT***" if (hits or tailhex) else ""
    print("%-28s score=%.3f tailhex=%s needles=%s%s" % (
        name, score, tailhex, [h[:12] for h in hits], flag))
    return bool(hits or tailhex)


def main():
    stmtsA = coreB.load_statements(
        os.path.join(tempfile.gettempdir(), "opencode", "ghidra_bffull.txt"), 22, 322)
    for kn, K in SCHEDS.items():
        check("ecb/setA/%s" % kn, ecb_decrypt(C.core, K, None))
        check("ecb/setB/%s" % kn, ecb_decrypt(coreB.run_core, K, stmtsB))
        check("cfb/setA/%s" % kn, cfb_decrypt(C.core, K, None))
        check("cfb/setB/%s" % kn, cfb_decrypt(coreB.run_core, K, stmtsB))


if __name__ == "__main__":
    main()
