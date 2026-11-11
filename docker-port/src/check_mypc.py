"""check_mypc - read the LIVE client's MYPC_INFO records (the zone spawn).

From the shipped binary:
  * record base = session + 0x8b8 + idx*0x276   (FUN_005dfca8; stride 0x276)
  * record + 4  = name[17]                      (FUN_005dfcd2 returns record+4)
  * record + 39 = job (DWORD)                   (FUN_005dfcbd returns record+0x27)
  * record + 89 = Equipment[21] (12B entries)   (FUN_005dfc3e returns record+0x911)
  * record + 428 = BASIC_STAT.LV (byte)         (FUN_005e057a case 2)
  * record + 477 = stanceCount (WORD)           (the parser FUN_005e645a)

We scan the live process for a character name at record+4, walk back to the
record base, and print the fields. READ-ONLY.
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

REC_SIZE = 0x276
OFF_NAME = 4
OFF_APPEARANCE = 21
OFF_JOB = 39
OFF_GENDER = 38
OFF_EQUIP = 89
OFF_INITPOS = 416
OFF_LV = 428
OFF_HP = 429
OFF_STANCECOUNT = 477


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
    names = sys.argv[1:] or ["Grandies"]
    pid = pids_of("ge.exe")[0]
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        print("OpenProcess failed", ctypes.get_last_error())
        return 1
    print("ge.exe pid=%d, scanning for MYPC_INFO records named %s ..." % (pid, names))

    patterns = [n.encode("latin-1") + b"\0" for n in names]
    seen = set()
    for base, size in regions(h):
        chunk = 0x4000000 if size > 0x4000000 else size
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
                        for back in (OFF_NAME, OFF_APPEARANCE):
                            rec = hit - back
                            if rec in seen:
                                continue
                            recb = read_mem(h, rec, 0x1e6)   # 486 bytes
                            if not recb or len(recb) < 0x1e6:
                                continue
                            name = recb[OFF_NAME:OFF_NAME + 17].split(b"\0")[0].decode("latin-1")
                            if name != pat[:-1].decode("latin-1"):
                                continue
                            seen.add(rec)
                            handle, = struct.unpack_from("<I", recb, 0)
                            job, = struct.unpack_from("<I", recb, OFF_JOB)
                            lv = recb[OFF_LV]
                            hp, = struct.unpack_from("<d", recb, OFF_HP)
                            initpos = struct.unpack_from("<iii", recb, OFF_INITPOS)
                            sc, = struct.unpack_from("<H", recb, OFF_STANCECOUNT)
                            eq = [struct.unpack_from("<I", recb, OFF_EQUIP + 12 * s)[0]
                                  for s in (0, 1, 5)]
                            print("  rec=%#010x handle=%-8d name=%-12r job=%-4d gender=%-2d "
                                  "LV=%-3d HP=%.0f pos=%s stanceCount=%d equip[0,1,5]=%s"
                                  % (rec, handle, name, job, recb[OFF_GENDER], lv, hp,
                                     initpos, sc, eq))
            off += n
    print("done (%d record(s))" % len(seen))
    return 0


if __name__ == "__main__":
    sys.exit(main())
