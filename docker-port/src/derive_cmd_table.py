"""derive_cmd_table - DERIVE the packet-table geometry from the live client.

Why: `dump_cmds.py` needs `names_base` and `sizes_base`. Guessing them from
old notes gave a table whose names were truncated FROM THE FRONT
(`COMMANDEROK` instead of `BC_COMMANDEROK`), which is easy to mistake for a
renamed protocol. So measure instead of guess.

Method: several packet names are known, and so are their command ids. The
distance between two names in memory is
    (id2 - id1) * stride
so each pair gives a candidate stride. The true stride is the one every pair
agrees on, and the base is then
    base = offset(name1) - id1 * stride

Then verify: with the derived base, reading consecutive slots must give WHOLE
names (not fragments), and `name[10]` must be exactly `BC_COMMANDER_INFO`.

Usage:  python derive_cmd_table.py
"""
import ctypes
import ctypes.wintypes as wt
import struct
import sys

PQI, PVR = 0x0400, 0x0010
MEM_COMMIT, PAGE_GUARD, PAGE_NOACCESS = 0x1000, 0x100, 0x01

# name -> command id, all read out of the shipped client (history/barrack.md,
# history/client.md, RESUME.md "Shipped client's real ids/sizes").
KNOWN = {
    "BC_COMMANDER_INFO": 10,
    "BC_BARRACK_EQUIP_INFO_EXTRA": 13,
    "BC_STARTGAMEOK": 16,
    "BC_LOGGING": 30,
    "BC_CHANNEL_LIST": 35,
    "BC_LOGINOK": 57,
    "BC_EQUIP_ITEM_PRESET_INIT": 64,
    "BC_ROTATION_CHAR_LIST": 66,
}


class MBI(ctypes.Structure):
    _fields_ = [("BaseAddress", ctypes.c_void_p), ("AllocationBase", ctypes.c_void_p),
                ("AllocationProtect", wt.DWORD), ("RegionSize", ctypes.c_size_t),
                ("State", wt.DWORD), ("Protect", wt.DWORD), ("Type", wt.DWORD)]


k32 = ctypes.WinDLL("kernel32", use_last_error=True)
k32.OpenProcess.restype = wt.HANDLE
k32.VirtualQueryEx.argtypes = [wt.HANDLE, ctypes.c_void_p, ctypes.POINTER(MBI), ctypes.c_size_t]
k32.ReadProcessMemory.argtypes = [wt.HANDLE, ctypes.c_void_p, ctypes.c_void_p,
                                  ctypes.c_size_t, ctypes.POINTER(ctypes.c_size_t)]


def pid_of(name="ge.exe"):
    import os
    import subprocess
    forced = os.environ.get("GE_PID")
    if forced and forced.isdigit():
        return int(forced)
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    for line in out.splitlines():
        p = [x.strip('"') for x in line.split('","')]
        if len(p) >= 2 and p[1].isdigit():
            return int(p[1])
    return None


def rd(h, addr, n):
    b = ctypes.create_string_buffer(n)
    got = ctypes.c_size_t(0)
    k32.ReadProcessMemory(h, ctypes.c_void_p(addr), b, n, ctypes.byref(got))
    return b.raw[:got.value]


def committed_regions(h):
    mbi = MBI()
    addr = 0
    while addr < 0x7FFFFFFF:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            yield base, size
        addr = base + size


def find_all(h, needle):
    hits = []
    for base, size in committed_regions(h):
        d = rd(h, base, size)
        i = d.find(needle)
        while i >= 0:
            hits.append(base + i)
            i = d.find(needle, i + 1)
    return hits


def main():
    pid = pid_of()
    if not pid:
        print("ge.exe is not running - start the client first")
        return
    h = k32.OpenProcess(PQI | PVR, False, pid)

    # Every occurrence of every known name.
    occ = {}
    for nm, cid in KNOWN.items():
        occ[nm] = find_all(h, (nm + "\x00").encode())
        print("%-32s id=%-4d occurrences=%d" % (nm, cid, len(occ[nm])))

    # A candidate table = one occurrence per name, all in the same region.
    # For each pair, distance / (id difference) = candidate stride.
    print("\n-- candidate strides --")
    strides = {}
    names = list(KNOWN)
    for i, n1 in enumerate(names):
        for n2 in names[i + 1:]:
            d_id = KNOWN[n2] - KNOWN[n1]
            for a in occ[n1]:
                for b in occ[n2]:
                    if a == b:
                        continue
                    delta = b - a
                    if delta % d_id:
                        continue
                    s = delta // d_id
                    if 8 <= s <= 256:
                        strides.setdefault(s, []).append((n1, n2, a, b))

    for s in sorted(strides, key=lambda k: -len(strides[k])):
        pairs = strides[s]
        # keep only pairs whose names are in the same region
        print("stride %-4d  %d agreeing pairs" % (s, len(pairs)))

    if not strides:
        print("no consistent stride found")
        return

    best = max(strides, key=lambda k: len(strides[k]))
    print("\nbest stride = %d" % best)

    # Now derive the base from the pair that agrees, and verify.
    pairs = strides[best]
    n1, n2, a, b = pairs[0]
    base = a - KNOWN[n1] * best
    print("candidate names_base = %#x  (from %s @ %#x)" % (base, n1, a))

    print("\n-- verify: read consecutive slots with that base --")
    ok = 0
    for idx in range(0, 30):
        raw = rd(h, base + idx * best, best)
        nm = raw.split(b"\x00")[0].decode("latin-1", "replace")
        print("%-4d %r" % (idx, nm))
        if nm and all(65 <= ord(c) <= 90 or 97 <= ord(c) <= 122 or c in "_0123456789" for c in nm):
            ok += 1
    print("whole-looking names: %d / 30" % ok)

    print("\n-- check every known name at its id --")
    for nm, cid in sorted(KNOWN.items(), key=lambda kv: kv[1]):
        raw = rd(h, base + cid * best, best)
        got = raw.split(b"\x00")[0].decode("latin-1", "replace")
        print("%-4d expect %-32s got %r  %s" % (cid, nm, got, "OK" if got == nm else "MISMATCH"))


if __name__ == "__main__":
    main()
