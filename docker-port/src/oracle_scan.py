"""Client memory scanner (oracle step 1) - READ ONLY, no debugger needed.

Finds the running ge.exe, reads its memory, and searches for the login
text you typed, in both plain ASCII and UTF-16, plus the ciphertext
blocks we captured. Prints each hit with 64 bytes around it.

Run (in docker-port):
    python oracle_scan.py Arcanine 123456
Stop: Ctrl+C (or it exits after one pass).

What to look for:
- If "Arcanine" is found as UTF-16 (A\\0r\\0c\\0...) the name field is
  wide text. If only ASCII is found, it is narrow text.
- If a captured ciphertext block is found nearby, we have both sides of
  the scramble in one process -> known plaintext at a known address.
"""
import ctypes
import ctypes.wintypes as wt
import os
import sys
import time

from ge_paths import OUT

k32 = ctypes.WinDLL("kernel32", use_last_error=True)

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010
MEM_COMMIT = 0x1000
PAGE_GUARD = 0x100
PAGE_NOACCESS = 0x01
READABLE = (0x02, 0x04, 0x20, 0x40)  # RO, RW, EXEC_R, EXEC_RW


class MBI(ctypes.Structure):
    _fields_ = [
        ("BaseAddress", ctypes.c_void_p),
        ("AllocationBase", ctypes.c_void_p),
        ("AllocationProtect", wt.DWORD),
        ("RegionSize", ctypes.c_size_t),
        ("State", wt.DWORD),
        ("Protect", wt.DWORD),
        ("Type", wt.DWORD),
    ]


def pids_of(name):
    import subprocess
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    pids = []
    for line in out.splitlines():
        parts = [p.strip('"') for p in line.split('","')]
        if len(parts) >= 2 and parts[0].lower() == name.lower():
            try:
                pids.append(int(parts[1]))
            except ValueError:
                pass
    return pids


def scan(h, needle):
    hits = []
    addr = 0
    mbi = MBI()
    maxaddr = 0x7FFFFFFF  # 32-bit target
    while addr < maxaddr:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi),
                                  ctypes.sizeof(mbi)):
            break
        base = mbi.BaseAddress or 0
        size = mbi.RegionSize or 0
        ok = (mbi.State == MEM_COMMIT and mbi.Protect in READABLE
              and not (mbi.Protect & PAGE_GUARD))
        if ok and size:
            buf = ctypes.create_string_buffer(size)
            got = ctypes.c_size_t(0)
            if k32.ReadProcessMemory(h, ctypes.c_void_p(base), buf, size,
                                     ctypes.byref(got)):
                data = buf.raw[:got.value]
                i = data.find(needle)
                while i >= 0:
                    hits.append((base + i, data[max(0, i - 32):i + 32]))
                    i = data.find(needle, i + 1)
        addr = base + size
    return hits


def hexdump(b):
    return " ".join("%02X" % x for x in b)


def main():
    words = sys.argv[1:] or ["Arcanine", "123456"]
    pids = pids_of("ge.exe")
    if not pids:
        print("ge.exe not running. Start the client and log in first.")
        return
    print("ge.exe pids:", pids)
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pids[0])
    if not h:
        print("OpenProcess failed:", ctypes.get_last_error())
        return
    needles = {}
    for w in words:
        needles["ascii:" + w] = w.encode("latin-1", "ignore")
        needles["utf16:" + w] = w.encode("utf-16-le")
    needles["ct_hdr"] = bytes.fromhex("de9541f50dbac055")
    needles["ct_pw"] = bytes.fromhex("e83dff1242c2966b")
    with open(os.path.join(OUT, "oracle_scan_out.txt"), "w", encoding="utf-8") as f:
        for tag, nd in needles.items():
            hits = scan(h, nd)
            for a, ctx in hits:
                line = "%s @0x%08X ctx=%s" % (tag, a, hexdump(ctx))
                print(line, flush=True)
                f.write(line + "\n")
    k32.CloseHandle(h)


if __name__ == "__main__":
    main()
