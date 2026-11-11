"""check_spc - read the LIVE client's SPC_INFO records to settle the blank grid.

From the decompiler:
  * SPC_INFO stride in memory is 0x158 (344) bytes, base = session + 0x1068
    (`FUN_005e010f(slot) = slot*0x158 + 0x1068 + this`).
  * `CSession::SetSPcInfo` (`FUN_005e6930`) copies 0x157 (343) bytes from the
    packet record, then writes 1 to record + 0x157 (0x11bf - 0x1068 = 0x157).
  * `FUN_0077d289` (the character-list refresh) fills a cell only when
    `*(char*)(record + 0x157) != 0`, otherwise it writes "" (the blank path used
    the empty string at 0x00a76b73).

So we hunt the live process for a character name at record+3, walk back to the
record base, and print the fields. READ-ONLY: it only reads memory.
"""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess
import sys

k32 = ctypes.WinDLL("kernel32", use_last_error=True)
k32.OpenProcess.restype = ctypes.c_void_p
k32.VirtualQueryEx.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
                               ctypes.c_size_t]
k32.ReadProcessMemory.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
                                  ctypes.c_size_t, ctypes.c_void_p]

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010
MEM_COMMIT = 0x1000
PAGE_GUARD = 0x100
PAGE_NOACCESS = 0x01

STRIDE = 0x158
OFF_FLAG = 0x157
OFF_NAME = 3
OFF_JOB = 0x15
OFF_GENDER = 0x14


class MEMORY_BASIC_INFORMATION(ctypes.Structure):
    _fields_ = [("BaseAddress", ctypes.c_void_p), ("AllocationBase", ctypes.c_void_p),
                ("AllocationProtect", wt.DWORD), ("RegionSize", ctypes.c_size_t),
                ("State", wt.DWORD), ("Protect", wt.DWORD), ("Type", wt.DWORD)]


def pids_of(name):
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    pids = []
    for line in out.splitlines():
        p = [x.strip('"') for x in line.split('","')]
        if len(p) >= 2 and p[0].lower() == name.lower() and p[1].isdigit():
            pids.append(int(p[1]))
    return pids


def read_mem(h, addr, n):
    buf = ctypes.create_string_buffer(n)
    got = ctypes.c_size_t(0)
    if not k32.ReadProcessMemory(h, ctypes.c_void_p(addr), buf, n, ctypes.byref(got)):
        return None
    return buf.raw[:got.value]


def regions(h):
    """Yield (base, size) of readable committed regions."""
    mbi = MEMORY_BASIC_INFORMATION()
    addr = 0
    while addr < 0x7FFF0000:
        got = k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi),
                                 ctypes.sizeof(mbi))
        if not got:
            break
        base = mbi.BaseAddress or 0
        size = mbi.RegionSize or 0
        if not size:
            break
        if (mbi.State == MEM_COMMIT and not (mbi.Protect & PAGE_GUARD)
                and not (mbi.Protect & PAGE_NOACCESS)):
            yield base, size
        addr = base + size


def main():
    names = sys.argv[1:] or ["Warlock", "Jin", "Amy"]
    pid = pids_of("ge.exe")[0]
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        print("OpenProcess failed", ctypes.get_last_error())
        return 1
    print("ge.exe pid=%d, scanning for %s ..." % (pid, names))

    patterns = [n.encode("latin-1") + b"\0" for n in names]
    for base, size in regions(h):
        if size > 0x4000000:            # skip huge regions per read
            chunk = 0x4000000
        else:
            chunk = size
        off = 0
        while off < size:
            n = min(chunk, size - off)
            buf = read_mem(h, base + off, n)
            if buf:
                for pat in patterns:
                    start = 0
                    while True:
                        i = buf.find(pat, start)
                        if i < 0:
                            break
                        start = i + 1
                        hit = base + off + i
                        rec = hit - OFF_NAME
                        recb = read_mem(h, rec, 0x160)
                        if not recb or len(recb) < 0x160:
                            continue
                        slot, = struct.unpack_from("<H", recb, 0)
                        teampos = recb[2]
                        job, = struct.unpack_from("<I", recb, OFF_JOB)
                        flag = recb[OFF_FLAG]
                        name = recb[OFF_NAME:OFF_NAME + 17].split(b"\0")[0].decode("latin-1")
                        # only believe it if the record looks coherent
                        if name == pat[:-1].decode("latin-1") and slot < 0x200:
                            print("  hit %#010x  rec=%#010x slot=%-4d team=%-2d job=%-4d "
                                  "gender=%-2d FLAG(+0x157)=%d  name=%r"
                                  % (hit, rec, slot, teampos, job,
                                     recb[OFF_GENDER], flag, name))
            off += n
    print("done")
    return 0


if __name__ == "__main__":
    sys.exit(main())
