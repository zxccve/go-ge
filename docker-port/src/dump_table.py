"""dump_table - print gePacketTable names[] and sizes[] side by side."""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess

PQI, PVR = 0x0400, 0x0010
MEM_COMMIT, PAGE_GUARD, PAGE_NOACCESS = 0x1000, 0x100, 0x01


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


def main():
    h = k32.OpenProcess(PQI | PVR, False, pid_of())
    needle = b"BC_COMMANDER_INFO\x00"
    mbi = MBI(); addr = 0; hits = []
    while addr < 0x7FFFFFFF:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            d = rd(h, base, size)
            i = d.find(needle)
            while i >= 0:
                hits.append(base + i); i = d.find(needle, i + 1)
        addr = base + size
    nb = hits[1] - 500
    sb = nb + 3276748
    print("names_base=%#x sizes_base=%#x" % (nb, sb))
    print("%-4s %-34s %s" % ("cmd", "name", "size"))
    for c in range(2, 60):
        nm = rd(h, nb + c * 50, 50).split(b"\x00")[0].decode("latin-1", "replace")
        sz = struct.unpack("<i", rd(h, sb + c * 4, 4))[0]
        print("%-4d %-34s %d%s" % (c, nm, sz, "  (var)" if sz == 0 else ""))


if __name__ == "__main__":
    main()
