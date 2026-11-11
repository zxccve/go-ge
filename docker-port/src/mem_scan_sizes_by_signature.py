"""mem_scan2 - locate the client's PACKET_TABLE.sizes array by value signature.

Known fixed sizes (from the source structs, which the login test confirms):
  cmd 5  BC_INPUTOTP          = 2
  cmd 6  CB_CANCEL_INPUTOTP   = 10
  cmd 7  CB_OTP              = 14
  cmd 8  BC_LOGINOK          = 250   <-- proven: our 250-byte LOGINOK was accepted
  cmd 9  CB_STARTBARRACK     = 11
Search process memory for the 4-byte-aligned run [2,10,14,250,11].
"""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010
MEM_COMMIT = 0x1000
PAGE_GUARD = 0x100
PAGE_NOACCESS = 0x01
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


sig = struct.pack("<5i", 2, 10, 14, 250, 11)


def main():
    pid = pid_of()
    print("pid", pid)
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    mbi = MBI()
    addr = 0
    found = []
    while addr < MAX_ADDR:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base = mbi.BaseAddress or 0
        size = mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            buf = ctypes.create_string_buffer(size)
            got = ctypes.c_size_t(0)
            if k32.ReadProcessMemory(h, ctypes.c_void_p(base), buf, size, ctypes.byref(got)):
                data = buf.raw[:got.value]
                i = data.find(sig)
                while i >= 0:
                    if (base + i) % 4 == 0:
                        found.append(base + i)
                    i = data.find(sig, i + 1)
        addr = base + size
    print("sizes_base candidates:", [hex(x) for x in found])
    for sb in found:
        print("--- sizes_base=%#x ---" % sb)
        ints = (ctypes.c_int * 64)()
        got = ctypes.c_size_t(0)
        k32.ReadProcessMemory(h, ctypes.c_void_p(sb), ints, 64 * 4, ctypes.byref(got))
        for cmd in range(2, 60):
            print("   cmd %2d size=%d" % (cmd, ints[cmd]))
        break


if __name__ == "__main__":
    main()
