"""mem_scan4 - loose search for the sizes array (adjacent 250, 11)."""
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


pat = struct.pack("<2i", 250, 11)


def main():
    pid = pid_of()
    h = k32.OpenProcess(PQI | PVR, False, pid)
    mbi = MBI()
    addr = 0
    hits = []
    while addr < MAX_ADDR:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            buf = ctypes.create_string_buffer(size)
            got = ctypes.c_size_t(0)
            if k32.ReadProcessMemory(h, ctypes.c_void_p(base), buf, size, ctypes.byref(got)):
                data = buf.raw[:got.value]
                i = data.find(pat)
                while i >= 0:
                    if (base + i) % 4 == 0:
                        hits.append((base + i, data[max(0, i - 24):i + 40]))
                    i = data.find(pat, i + 1)
        addr = base + size
    print("loose hits:", len(hits))
    for p, win in hits[:20]:
        ints = struct.unpack("<%di" % (len(win) // 4), win[:len(win) // 4 * 4])
        print("@%#x : %s" % (p, ints))


if __name__ == "__main__":
    main()
