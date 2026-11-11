"""mem_scan - read the running client's gePacketTable::PACKET_TABLE from memory.

The shipped client decides fixed-packet sizes with sizeof() compiled into a
runtime table:
    struct PACKET_TABLE { char names[65536][50]; int sizes[65536]; }
We locate names[10] ("BC_COMMANDER_INFO") by scanning for that string, derive
the table base, then dump the sizes for the packet commands we care about.

Usage:  python mem_scan.py [pid]
"""
import ctypes
import ctypes.wintypes as wt
import struct
import sys

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010
MEM_COMMIT = 0x1000
PAGE_GUARD = 0x100
PAGE_NOACCESS = 0x01
MAX_ADDR = 0x7FFFFFFF
MAX_PACKET_NUM = 0xFFFF
NAME_LEN = 50


class MEMORY_BASIC_INFORMATION(ctypes.Structure):
    _fields_ = [("BaseAddress", ctypes.c_void_p),
                ("AllocationBase", ctypes.c_void_p),
                ("AllocationProtect", wt.DWORD),
                ("RegionSize", ctypes.c_size_t),
                ("State", wt.DWORD),
                ("Protect", wt.DWORD),
                ("Type", wt.DWORD)]


k32 = ctypes.WinDLL("kernel32", use_last_error=True)
k32.OpenProcess.restype = wt.HANDLE
k32.OpenProcess.argtypes = [wt.DWORD, wt.BOOL, wt.DWORD]
k32.VirtualQueryEx.restype = ctypes.c_size_t
k32.VirtualQueryEx.argtypes = [wt.HANDLE, ctypes.c_void_p,
                               ctypes.POINTER(MEMORY_BASIC_INFORMATION),
                               ctypes.c_size_t]
k32.ReadProcessMemory.restype = wt.BOOL
k32.ReadProcessMemory.argtypes = [wt.HANDLE, ctypes.c_void_p, ctypes.c_void_p,
                                  ctypes.c_size_t, ctypes.POINTER(ctypes.c_size_t)]


def find_pid(name="ge.exe"):
    import subprocess
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    for line in out.splitlines():
        parts = [p.strip('"') for p in line.split('","')]
        if len(parts) >= 2 and parts[1].isdigit():
            return int(parts[1])
    return None


def main():
    pid = int(sys.argv[1]) if len(sys.argv) > 1 else find_pid()
    if not pid:
        print("ge.exe not running")
        return
    print("pid", pid)
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        print("OpenProcess failed", ctypes.get_last_error())
        return

    needle = b"BC_COMMANDER_INFO\x00"
    mbi = MEMORY_BASIC_INFORMATION()
    addr = 0
    hits = []
    while addr < MAX_ADDR:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base = mbi.BaseAddress or 0
        size = mbi.RegionSize
        if (mbi.State == MEM_COMMIT and not (mbi.Protect & PAGE_GUARD)
                and not (mbi.Protect & PAGE_NOACCESS)):
            buf = ctypes.create_string_buffer(size)
            read = ctypes.c_size_t(0)
            if k32.ReadProcessMemory(h, ctypes.c_void_p(base), buf, size, ctypes.byref(read)):
                data = buf.raw[:read.value]
                i = data.find(needle)
                while i >= 0:
                    hits.append(base + i)
                    i = data.find(needle, i + 1)
        addr = base + size
    print("hits for BC_COMMANDER_INFO:", [hex(x) for x in hits])

    for hit in hits:
        names_base = hit - 10 * NAME_LEN     # command 10 == BC_COMMANDER_INFO
        sizes_base = names_base + MAX_PACKET_NUM * NAME_LEN
        # sanity: dump names 4,8,9,10,15,16
        print("--- candidate table, names_base=%#x sizes_base=%#x ---" % (names_base, sizes_base))
        ok = True
        for cmd, want in [(4, b"CB_LOGIN"), (8, b"BC_LOGINOK"), (9, b"CB_STARTBARRACK"),
                          (10, b"BC_COMMANDER_INFO"), (15, b"CB_REQ_BARRACK_INFO"),
                          (16, b"BC_BARRACK_INFO")]:
            nb = ctypes.create_string_buffer(NAME_LEN)
            got = ctypes.c_size_t(0)
            k32.ReadProcessMemory(h, ctypes.c_void_p(names_base + cmd * NAME_LEN),
                                  nb, NAME_LEN, ctypes.byref(got))
            name = nb.raw.split(b"\x00")[0].decode("latin-1", "replace")
            flag = "OK " if name == want.decode() else "?? "
            if name != want.decode():
                ok = False
            print("  %snames[%d]=%r" % (flag, cmd, name))
        if not ok:
            continue
        print("  ==> sizes:")
        for cmd in range(2, 60):
            sb = ctypes.c_int(0)
            got = ctypes.c_size_t(0)
            k32.ReadProcessMemory(h, ctypes.c_void_p(sizes_base + cmd * 4),
                                  ctypes.byref(sb), 4, ctypes.byref(got))
            nb = ctypes.create_string_buffer(NAME_LEN)
            k32.ReadProcessMemory(h, ctypes.c_void_p(names_base + cmd * NAME_LEN),
                                  nb, NAME_LEN, ctypes.byref(got))
            nm = nb.raw.split(b"\x00")[0].decode("latin-1", "replace")
            if nm:
                print("    cmd %2d %-28s size=%d%s" % (cmd, nm, sb.value,
                      "  (variable)" if sb.value == 0 else ""))
        return


if __name__ == "__main__":
    main()
