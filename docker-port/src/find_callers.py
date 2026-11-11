"""Walk up from the send wrappers: find each wrapper's function start, then
list the callers (E8 rel32) that target it. Static, no debugger.

Run:  python find_callers.py
"""
import struct
from ge_paths import GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
SEND_CALLS = (0x0042DE6A, 0x0042DF07, 0x004DC2BF)


def load():
    data = open(CLIENT, "rb").read()
    e = struct.unpack_from("<I", data, 0x3C)[0]
    nsec = struct.unpack_from("<H", data, e + 6)[0]
    optsz = struct.unpack_from("<H", data, e + 20)[0]
    opt = e + 24
    base = struct.unpack_from("<I", data, opt + 28)[0]
    sec = opt + optsz
    S = []
    for i in range(nsec):
        o = sec + i * 40
        nm = data[o:o + 8].rstrip(b"\0").decode(errors="ignore")
        vs, va, rs, rp = struct.unpack_from("<IIII", data, o + 8)
        ch = struct.unpack_from("<I", data, o + 36)[0]
        S.append((nm, va, vs, rp, rs, ch))
    return data, base, S


def v2o(base, S, v):
    for _nm, va, vs, rp, rs, _ch in S:
        if va <= v < va + max(vs, rs):
            return rp + (v - va)
    return None


def find_start(data, base, S, va):
    o = v2o(base, S, va)
    start = v2o(base, S, base)  # not used
    # scan backward for 55 8b ec preceded by padding/ret
    for back in range(0, 0x600):
        p = o - back
        if p < 4:
            break
        if data[p:p + 3] == b"\x55\x8b\xec":
            prev = data[p - 1]
            if prev in (0xCC, 0xC3, 0x90, 0x00) or data[p - 2:p] == b"\xc2\x04" or data[p - 3:p] == b"\xc2\x04\x00":
                return base + (p - v2o(base, S, base)) if False else va - back
    return None


def callers_of(data, base, S, target_va):
    out = []
    for _nm, va, vs, rp, rs, ch in S:
        if not (ch & 0x20000000):
            continue
        blob = data[rp:rp + rs]
        rel = target_va - (va + 5)
        pat = b"\xE8" + struct.pack("<i", rel)
        i = blob.find(pat)
        while i >= 0:
            out.append(va + i)
            i = blob.find(pat, i + 1)
    return out


def main():
    data, base, S = load()
    for call_va in SEND_CALLS:
        start = find_start(data, base, S, call_va)
        print("send call 0x%08X -> wrapper start ~0x%08X" % (call_va, start or 0))
        if not start:
            continue
        cs = callers_of(data, base, S, start)
        print("   %d caller(s):" % len(cs))
        for c in sorted(cs):
            print("      call at 0x%08X" % c)


if __name__ == "__main__":
    main()
