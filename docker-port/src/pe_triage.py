"""PE triage for the shipped server binaries - is it packed/locked, or readable?

Written 2026-10-18 with `history\\shipped-binaries.md`. No REA/Ghidra needed: it
reads the PE headers and the raw bytes directly, so it is safe and instant.

    python pe_triage.py release\\ZoneServer_Release.exe       # scan one/many
    python pe_triage.py --names <exe> <out.json>             # dump the names

Scan prints: size, sha256 prefix, entropy, PE machine / optional-header / .NET
flag, each section with its own entropy, and any WinLicense / DNGuard / trial
marker. Section entropy near 8.0 means encrypted payload, not code.
"""
import collections
import hashlib
import json
import math
import os
import re
import struct
import sys

MARKERS = (b"WinLicense", b"winlice", b"DNGuard", b"trial", b"HVMRuntm")


def entropy(b):
    if not b:
        return 0.0
    c = [0] * 256
    for x in b:
        c[x] += 1
    n = len(b)
    return -sum((v / n) * math.log2(v / n) for v in c if v)


def parse(path):
    """Return (mach, is_net, sections) or None when the file is not a PE."""
    d = open(path, "rb").read()
    if d[:2] != b"MZ":
        return None
    e = struct.unpack_from("<I", d, 0x3C)[0]
    if d[e:e + 4] != b"PE\0\0":
        return None
    mach, nsec = struct.unpack_from("<HH", d, e + 4)
    optsz = struct.unpack_from("<H", d, e + 20)[0]
    opt = e + 24
    magic = struct.unpack_from("<H", d, opt)[0]
    nrdir = struct.unpack_from("<I", d, opt + (0x5C if magic == 0x10B else 0x6C))[0]
    is_net = False
    if nrdir >= 15:
        comdir = opt + (0x60 if magic == 0x10B else 0x70)
        _rva, size = struct.unpack_from("<II", d, comdir + 14 * 8)
        is_net = size > 0
    sh = e + 24 + optsz
    secs = []
    for i in range(nsec):
        o = sh + i * 40
        name = d[o:o + 8].rstrip(b"\0").decode("latin1")
        vsz = struct.unpack_from("<I", d, o + 8)[0]
        ro = struct.unpack_from("<I", d, o + 20)[0]
        secs.append((name, vsz, entropy(d[ro:ro + vsz])))
    return mach, magic, is_net, secs


def scan(path):
    d = open(path, "rb").read()
    print(os.path.basename(path), len(d), hashlib.sha256(d).hexdigest()[:12],
          "ent=%.2f" % entropy(d[:min(len(d), 4 << 20)]))
    r = parse(path)
    if r:
        mach, magic, is_net, secs = r
        print("   mach=0x%04x opt=%s .NET=%s" % (
            mach, "pe32+" if magic == 0x20B else "pe32", is_net))
        print("   secs=" + " ".join("%s(%.2f)" % (n, e) for n, _v, e in secs))
    else:
        print("   not PE")
    hit = [m.decode("latin1") for m in MARKERS if m in d]
    if hit:
        print("   marks=" + ",".join(hit))


def names(path, out):
    d = open(path, "rb").read()
    text = " ".join(m.decode("latin1") for m in re.findall(rb"[ -~]{5,}", d))
    data = {
        "source": os.path.abspath(path),
        "size": len(d),
        "pks": sorted(set(re.findall(r"\bPKS_[A-Z0-9_]+", text))),
        "rtti": sorted(set(re.findall(r"\.\?A[VU][^ ]*", text))),
        "class_func": sorted({"%s::%s" % (c, f) for c, f in
                              re.findall(r"\b(C[A-Za-z0-9_]+)::([A-Za-z_~][A-Za-z0-9_]*)", text)}),
        "bindings": sorted(set(re.findall(r"BindFunction::([A-Za-z0-9_]+)", text))),
    }
    cls = collections.Counter(x.split("::")[0] for x in data["class_func"])
    data["classes"] = cls.most_common()
    with open(out, "w", encoding="utf8") as f:
        json.dump(data, f, indent=1)
    print("strings>=5: %d  PKS_: %d  RTTI: %d  class::func: %d  classes: %d" % (
        len(set(re.findall(rb"[ -~]{5,}", d))), len(data["pks"]),
        len(data["rtti"]), len(data["class_func"]), len(cls)))
    print("wrote", out)


if __name__ == "__main__":
    args = sys.argv[1:]
    if args and args[0] == "--names":
        names(args[1], args[2])
    else:
        for p in args:
            if os.path.isfile(p):
                scan(p)
            else:
                print("skip (not a file):", p)
