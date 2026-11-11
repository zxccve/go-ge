"""Decrypt all captures with the working key/tables and show the plaintext."""
import os
import re
import struct

import bf2
import final_decrypt as fd
from ge_paths import CAPTURES

KEY = b"hsunffalqyrqewes"
P, S = fd.reassemble(fd.CLIENT_SRC)
KP, KS = bf2.key_schedule(KEY, P, S)


def decrypt(pkt):
    ct = pkt[2:]
    return b"".join(
        struct.pack(">2I", *bf2.dec(*struct.unpack(">2I", ct[i:i + 8]), KP, KS))
        for i in range(0, len(ct) // 8 * 8, 8))


def load_caps():
    out = {}
    txt = open(fd.LOG, encoding="utf-8", errors="ignore").read()
    ms = re.findall(r"hex=\[(.*?)\]", txt)
    pkts = [bytes(int(x, 16) for x in m.split()) for m in ms]
    out["old"] = [p for p in pkts if len(p) == 818][0]
    import glob
    for pth in sorted(glob.glob(os.path.join(CAPTURES, "cap_*.log"))):
        t = open(pth, encoding="utf-8", errors="ignore").read()
        m = re.search(r"hex=\[(.*?)\]", t)
        out[pth.split("\\")[-1]] = bytes(int(x, 16) for x in m.group(1).split())
    return out


def main():
    for name, pkt in load_caps().items():
        pt = decrypt(pkt)
        print("==", name)
        print("  hex :", " ".join("%02X" % b for b in pt[:64]))
        print("  text:", "".join(chr(b) if 32 <= b < 127 else "." for b in pt[:80]))
    # field probe on the first capture
    pt = decrypt(load_caps()["cap_01.log"])
    for probe in (b"Arcanine", b"123456"):
        print(probe, "at", pt.find(probe))


if __name__ == "__main__":
    main()
