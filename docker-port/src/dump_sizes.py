"""dump_sizes - raw dump around the PACKET_TABLE names array to find sizes[]."""
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
    pid = pid_of()
    h = k32.OpenProcess(PQI | PVR, False, pid)
    # locate names[10]
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
    print("names_base", hex(nb))
    # verify names[3]
    for c in [2, 3, 4]:
        print("  names[%d]=%r" % (c, rd(h, nb + c * 50, 40).split(b"\x00")[0]))
    # scan forward for the start of sizes: look for 3 consecutive -1 (0xff ff ff ff)
    for delta in range(3276000, 3277600, 4):
        v = struct.unpack("<i", rd(h, nb + delta, 4))[0]
        if v == -1:
            i0 = struct.unpack_from("<10i", rd(h, nb + delta, 40))
            if list(i0[:3]) == [-1, -1, -1]:
                print("possible sizes start at names_base+%d (0x%x)" % (delta, nb + delta))
                ints = struct.unpack_from("<20i", rd(h, nb + delta, 80))
                for i, x in enumerate(ints):
                    print("    sizes[%d]=%d" % (i, x))
                return
    print("no -1,-1,-1 run found in window")


if __name__ == "__main__":
    main()
