"""find_cmd - locate a packet name in the live client's gePacketTable and
print its command id + size.

dump_table.py only prints ids 2..59 (the barrack range). The zone packets are
numbered differently in the shipped client (observed CZ_START_GAME = 0x0E06
= 3590, while source2 says 3076), so we must read the client's own table.

Usage:  python find_cmd.py CZ_START_GAME ZC_START_GAMEOK BC_BARRACK_INFO
"""
import ctypes
import ctypes.wintypes as wt
import os
import struct
import subprocess
import sys

PQI, PVR = 0x0400, 0x0010
MEM_COMMIT, PAGE_GUARD, PAGE_NOACCESS = 0x1000, 0x100, 0x01
NAMES_STRIDE = 50          # both confirmed by dump_table.py


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
    # GE_PID lets us force the SAME client the other tools watch: there can be
    # more than one ge.exe (another install), and the first is not necessarily
    # the one we are porting against.
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


def scan_all(h, needles=None):
    """Return {needle: [addrs]} for every printable packet-name-ish string."""
    if needles is None:
        needles = [b"BC_COMMANDER_INFO\x00", b"CZ_START_GAME\x00", b"ZC_START_GAMEOK\x00"]
    found = {}
    mbi = MBI(); addr = 0
    while addr < 0x7FFFFFFF:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            d = rd(h, base, size)
            for name in needles:
                i = d.find(name)
                while i >= 0:
                    found.setdefault(name, []).append(base + i)
                    i = d.find(name, i + 1)
        addr = base + size
    return found


def main():
    names = sys.argv[1:] or ["CZ_START_GAME", "ZC_START_GAMEOK"]
    needles = [b"BC_COMMANDER_INFO\x00", b"CZ_START_GAME\x00", b"ZC_START_GAMEOK\x00"]
    for nm in names:
        if not nm.lstrip("-").isdigit():
            needles.append((nm + "\x00").encode())
    h = k32.OpenProcess(PQI | PVR, False, pid_of())
    found = scan_all(h, list(dict.fromkeys(needles)))
    hits = found.get(b"BC_COMMANDER_INFO\x00")
    if not hits:
        print("could not locate the packet-name table (BC_COMMANDER_INFO not found)")
        return
    nb = hits[-1] - 500          # index 10 -> 10 * 50 = 500
    print("names_base=%#x  (name[10] should be BC_COMMANDER_INFO)" % nb)
    print("sanity: name[10]=%r" % rd(h, nb + 10 * NAMES_STRIDE, 40).split(b"\x00")[0])
    sizes_base = nb + 3276748    # same offset dump_table.py uses
    for nm in names:
        if nm.lstrip("-").isdigit():
            cmd = int(nm)
            name = rd(h, nb + cmd * NAMES_STRIDE, 50).split(b"\x00")[0].decode("latin-1", "replace")
            sz = struct.unpack("<i", rd(h, sizes_base + cmd * 4, 4))[0]
            print("cmd=%-6d %-34s size=%d%s" % (cmd, name, sz, "  (var)" if sz == 0 else ""))
            continue
        addrs = found.get((nm + "\x00").encode())
        if not addrs:
            print("%-22s NOT FOUND" % nm)
            continue
        for a in addrs:
            idx = (a - nb) / NAMES_STRIDE
            if 0 <= idx < 20000 and abs(idx - round(idx)) < 1e-9:
                cmd = int(round(idx))
                print("%-22s cmd=%d (%#x)" % (nm, cmd, cmd))


if __name__ == "__main__":
    main()
