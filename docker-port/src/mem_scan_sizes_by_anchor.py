"""mem_scan3 - find the client's sizes[] array via the BC_LOGINOK=250 anchor."""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess

PQI, PVR = 0x0400, 0x0010
MEM_COMMIT, PAGE_GUARD, PAGE_NOACCESS = 0x1000, 0x100, 0x01
MAX_ADDR = 0x7FFFFFFF


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


def main():
    pid = pid_of()
    h = k32.OpenProcess(PQI | PVR, False, pid)
    mbi = MBI()
    addr = 0
    cands = []
    while addr < MAX_ADDR:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            buf = ctypes.create_string_buffer(size)
            got = ctypes.c_size_t(0)
            if k32.ReadProcessMemory(h, ctypes.c_void_p(base), buf, size, ctypes.byref(got)):
                data = buf.raw[:got.value]
                # aligned scan for int==250 then int==11
                for off in range(0, len(data) - 64, 4):
                    v = struct.unpack_from("<i", data, off)[0]
                    if v != 250:
                        continue
                    w = struct.unpack_from("<16i", data, off - 12)
                    # w: [-12,-8,-4, 0(250), +4(11?), ...]
                    if w[4] == 11 and 0 < w[5] < 6000 and 0 < w[3] < 64 and 0 < w[2] < 64:
                        cands.append((base + off, w))
        addr = base + size
    print("candidates:", len(cands))
    for p, w in cands:
        sb = p - 8 * 4
        print("BC_LOGINOK@%#x  sizes_base=%#x  window(cmd2..17)=%s" % (p, sb, list(w)))
    if cands:
        p, w = cands[0]
        sb = p - 8 * 4
        ints = (ctypes.c_int * 64)()
        got = ctypes.c_size_t(0)
        k32.ReadProcessMemory(h, ctypes.c_void_p(sb), ints, 64 * 4, ctypes.byref(got))
        print("FULL sizes 2..59:")
        for c in range(2, 60):
            print("   cmd %2d size=%d" % (c, ints[c]))


if __name__ == "__main__":
    main()
