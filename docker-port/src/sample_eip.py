"""sample_eip - find WHERE the client is stuck, by sampling its instruction pointer.

crash_stack2.py only fires on a crash. When the client HANGS instead, nothing
crashes, so there is no event to catch. The cheap way to find a hang is to look
at the instruction pointer (EIP, "which line of code is running") over and over.

Why REGIONS and not single addresses: a busy loop is several instructions long
(a few bytes to a few dozen). Sampling lands anywhere inside it, so the samples
spread over those addresses and no single one "dominates". We therefore group
nearby addresses into a region and look for the region that eats most of a
thread's samples.

How it works, in plain terms:
  * Windows lets you read another process's registers if you first PAUSE its
    thread (SuspendThread), read the registers (Wow64GetThreadContext), then let
    it run again (ResumeThread). No debugger needed. This is READ-ONLY: it never
    writes to the game.
  * We do that for every thread, a few times a second.
  * At the end we print, per thread, the busiest region inside ge.exe. That is
    the loop. We also print a window of code bytes and the call stack.

The client is 32-bit, so we use the Wow64 variants (this Python is 64-bit).

Run:  python sample_eip.py
Then launch the client (its batch file!) and click LOGIN. Leave it running until
it hangs or you press Ctrl+C. Everything is also written to sample_eip.log.

Optional: `python sample_eip.py 120` stops by itself after 120s of sampling and
prints the report (handy for running it in the background).
"""
import ctypes
import ctypes.wintypes as wt
import os
import struct
import subprocess
import sys
import time
from collections import Counter, defaultdict

from ge_paths import LOGS, GAME_ROOT

k32 = ctypes.WinDLL("kernel32", use_last_error=True)
user32 = ctypes.WinDLL("user32", use_last_error=True)

TH32CS_SNAPTHREAD = 0x00000004
TH32CS_SNAPMODULE = 0x00000008
TH32CS_SNAPMODULE32 = 0x00000010

THREAD_SUSPEND_RESUME = 0x0002
THREAD_GET_CONTEXT = 0x0008
THREAD_QUERY_INFORMATION = 0x0040
THREAD_ACCESS = THREAD_SUSPEND_RESUME | THREAD_GET_CONTEXT | THREAD_QUERY_INFORMATION

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010

# WOW64_CONTEXT_CONTROL | _INTEGER | _SEGMENTS -> Eip, Esp, Ebp, Eax..Edi
WOW64_CONTEXT_ALL_WE_NEED = 0x00010007

SAMPLE_INTERVAL = 0.20       # seconds between samples
MODULE_SPAN = 0x2000000      # how far past the base counts as "inside ge.exe"
ADDR_GAP = 0x40              # addresses closer than this are one region
STACK_SCAN_BYTES = 0x800     # how much of the busy thread's stack we scan
# ge.exe's .text VA range (base + RVA). Stack values must be filtered to CODE:
# .rdata/.data/.rsrc sit ABOVE .text and would otherwise match "inside module"
# (globals, vtables and heap blocks flooded the histogram without this).
TEXT_LO_RVA = 0x1000
TEXT_HI_RVA = 0x5C8190
EXE = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
LOG = os.path.join(LOGS, "sample_eip.log")


# --------------------------------------------------------------------------
# Win32 structures
# --------------------------------------------------------------------------
class THREADENTRY32(ctypes.Structure):
    _fields_ = [("dwSize", wt.DWORD), ("cntUsage", wt.DWORD),
                ("th32ThreadID", wt.DWORD), ("th32OwnerProcessID", wt.DWORD),
                ("tpBasePri", wt.LONG), ("tpDeltaPri", wt.LONG),
                ("dwFlags", wt.DWORD)]


class MODULEENTRY32(ctypes.Structure):
    _fields_ = [("dwSize", wt.DWORD), ("th32ModuleID", wt.DWORD),
                ("th32ProcessID", wt.DWORD), ("GlblcntUsage", wt.DWORD),
                ("ProccntUsage", wt.DWORD), ("modBaseAddr", ctypes.c_void_p),
                ("modBaseSize", wt.DWORD), ("hModule", wt.HMODULE),
                ("szModule", ctypes.c_char * 256), ("szExePath", ctypes.c_char * 260)]


class WOW64_CONTEXT(ctypes.Structure):
    _fields_ = [("ContextFlags", wt.DWORD),
                ("Dr0", wt.DWORD), ("Dr1", wt.DWORD), ("Dr2", wt.DWORD), ("Dr3", wt.DWORD),
                ("Dr6", wt.DWORD), ("Dr7", wt.DWORD), ("FloatSave", ctypes.c_byte * 112),
                ("SegGs", wt.DWORD), ("SegFs", wt.DWORD), ("SegEs", wt.DWORD), ("SegDs", wt.DWORD),
                ("Edi", wt.DWORD), ("Esi", wt.DWORD), ("Ebx", wt.DWORD), ("Edx", wt.DWORD),
                ("Ecx", wt.DWORD), ("Eax", wt.DWORD), ("Ebp", wt.DWORD), ("Eip", wt.DWORD),
                ("SegCs", wt.DWORD), ("EFlags", wt.DWORD), ("Esp", wt.DWORD), ("SegSs", wt.DWORD)]


# Explicit prototypes. HANDLE is pointer-sized, and SuspendThread returns -1 on
# failure, which must not be read as a huge positive number.
k32.OpenThread.restype = ctypes.c_void_p
k32.OpenProcess.restype = ctypes.c_void_p
k32.SuspendThread.argtypes = [ctypes.c_void_p]
k32.SuspendThread.restype = ctypes.c_long
k32.ResumeThread.argtypes = [ctypes.c_void_p]
k32.ResumeThread.restype = ctypes.c_long
k32.CloseHandle.argtypes = [ctypes.c_void_p]
k32.Wow64GetThreadContext.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
k32.ReadProcessMemory.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p,
                                  ctypes.c_size_t, ctypes.c_void_p]


# --------------------------------------------------------------------------
# local copy of ge.exe, so we can turn a VA back into file bytes
# --------------------------------------------------------------------------
class PeFile:
    """Minimal PE reader: map a virtual address to a byte offset in the file."""

    def __init__(self, path):
        self.data = open(path, "rb").read()
        d = self.data
        pe = struct.unpack_from("<I", d, 0x3C)[0]
        self.base = struct.unpack_from("<I", d, pe + 52)[0]
        nsec = struct.unpack_from("<H", d, pe + 6)[0]
        optsz = struct.unpack_from("<H", d, pe + 20)[0]
        self.sections = []
        off = pe + 24 + optsz
        for i in range(nsec):
            o = off + i * 40
            name = d[o:o + 8].rstrip(b"\0").decode("latin-1")
            vsz, va, rsz, raw = struct.unpack_from("<IIII", d, o + 8)
            self.sections.append((name, self.base + va, vsz, raw, rsz))

    def offset(self, va):
        for name, sva, vsz, raw, rsz in self.sections:
            if sva <= va < sva + vsz:
                off = raw + (va - sva)
                if off < len(self.data):
                    return off
        return None

    def read(self, va, n):
        off = self.offset(va)
        return None if off is None else self.data[off:off + n]

    def dump(self, lo, hi, label, pad=16):
        """Hex dump the code from lo-pad to hi+pad, so the whole loop shows."""
        off_lo = self.offset(lo)
        off_hi = self.offset(hi)
        if off_lo is None or off_hi is None:
            return None
        start = max(0, off_lo - pad)
        end = min(len(self.data), off_hi + pad)
        base_va = lo - (off_lo - start)
        lines = ["  code %s (%#x..%#x):" % (label, lo, hi)]
        for i in range(start, end, 16):
            chunk = self.data[i:i + 16]
            lines.append("    %08x  %s" % (base_va + (i - start),
                                           " ".join("%02X" % b for b in chunk)))
        return "\n".join(lines)


# --------------------------------------------------------------------------
# process / thread helpers
# --------------------------------------------------------------------------
def pids_of(name):
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    pids = []
    for line in out.splitlines():
        p = [x.strip('"') for x in line.split('","')]
        if len(p) >= 2 and p[0].lower() == name.lower() and p[1].isdigit():
            pids.append(int(p[1]))
    return pids


def window_pids():
    """Pids that own a visible top-level window. The real client has one; a
    transient helper process does not."""
    found = set()
    WNDENUMPROC = ctypes.WINFUNCTYPE(ctypes.c_bool, ctypes.c_void_p, ctypes.c_void_p)

    def cb(hwnd, lparam):
        if user32.IsWindowVisible(hwnd):
            pid = wt.DWORD(0)
            user32.GetWindowThreadProcessId(hwnd, ctypes.byref(pid))
            if pid.value:
                found.add(pid.value)
        return True

    user32.EnumWindows(WNDENUMPROC(cb), 0)
    return found


def pick_pid(exclude=None):
    """Preferred ge.exe: the one running EXE (our Terra client), else the one
    with a window, else the first.

    There can be MORE than one ge.exe on this machine (for example another
    Granado Espada install on a network drive), and both have a window, so the
    module path is the only reliable way to tell them apart."""
    px = [p for p in pids_of("ge.exe") if p != exclude]
    if not px:
        return None
    want = os.path.normcase(EXE)
    same = []
    for p in px:
        _b, path = first_module(p)
        if path and os.path.normcase(os.path.abspath(path)) == want:
            same.append(p)
    if same:
        return same[0]
    wins = window_pids()
    with_window = [p for p in px if p in wins]
    return (with_window or px)[0]


def thread_ids(pid):
    snap = k32.CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0)
    if snap == -1:
        return []
    out = []
    te = THREADENTRY32()
    te.dwSize = ctypes.sizeof(te)
    if k32.Thread32First(snap, ctypes.byref(te)):
        while True:
            if te.th32OwnerProcessID == pid:
                out.append(te.th32ThreadID)
            if not k32.Thread32Next(snap, ctypes.byref(te)):
                break
    k32.CloseHandle(snap)
    return out


def first_module(pid):
    """(base, path) of the main module, from the module snapshot."""
    snap = k32.CreateToolhelp32Snapshot(TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32, pid)
    if snap == -1:
        return None, None
    me = MODULEENTRY32()
    me.dwSize = ctypes.sizeof(me)
    base, path = None, None
    if k32.Module32First(snap, ctypes.byref(me)):
        base = me.modBaseAddr or 0
        path = me.szExePath.decode("latin-1")
    k32.CloseHandle(snap)
    return base, path


def read_mem(h, addr, n):
    buf = ctypes.create_string_buffer(n)
    got = ctypes.c_size_t(0)
    if not k32.ReadProcessMemory(h, ctypes.c_void_p(addr), buf, n, ctypes.byref(got)):
        return None
    return buf.raw[:got.value]


def sample_thread(tid):
    """Pause the thread, read EIP/ESP/EBP, resume. Returns a dict or None."""
    h = k32.OpenThread(THREAD_ACCESS, False, tid)
    if not h:
        return None
    try:
        if k32.SuspendThread(h) == -1:
            return None
        try:
            ctx = WOW64_CONTEXT()
            ctx.ContextFlags = WOW64_CONTEXT_ALL_WE_NEED
            if not k32.Wow64GetThreadContext(h, ctypes.byref(ctx)):
                return None
            return {"tid": tid, "eip": ctx.Eip, "esp": ctx.Esp, "ebp": ctx.Ebp,
                    "eax": ctx.Eax, "ecx": ctx.Ecx, "edx": ctx.Edx,
                    "ebx": ctx.Ebx, "esi": ctx.Esi, "edi": ctx.Edi}
        finally:
            k32.ResumeThread(h)
    finally:
        k32.CloseHandle(h)


# --------------------------------------------------------------------------
# analysis
# --------------------------------------------------------------------------
def in_module(eip, base):
    return bool(base) and base <= eip < base + MODULE_SPAN


def in_code(va, base):
    """True only for addresses inside ge.exe's .text (code), not data.

    Stack frames are full of pointers to globals/vtables/`this` objects, all of
    which live in .rdata/.data/.rsrc ABOVE .text. Filtering on the code section
    is what turns a stack histogram from noise into a caller chain."""
    return bool(base) and (base + TEXT_LO_RVA) <= va < (base + TEXT_HI_RVA)


def clusters(counter, base, gap=ADDR_GAP):
    """Group EIPs inside ge.exe into regions <= `gap` bytes apart.

    Returns a list of (lo, hi, count, items) sorted by nothing in particular;
    `items` is the list of (eip, n) inside the region."""
    addrs = sorted((e, n) for e, n in counter.items() if in_module(e, base))
    out = []
    for e, n in addrs:
        if out and e - out[-1][1] <= gap:
            lo, _hi, cnt, items = out[-1]
            out[-1] = (lo, e, cnt + n, items + [(e, n)])
        else:
            out.append((e, e, n, [(e, n)]))
    return out


def stuck_region(per_thread, base, min_share=0.5, min_samples=12, min_cluster=3):
    """The most convincing busy-loop region: inside ge.exe, eaten most of a
    thread's samples, and made of more than one address.

    A healthy client waiting in the message loop, or blocked in recv, has almost
    no samples inside ge.exe at all, so this stays quiet."""
    best = None
    for tid, counter in per_thread.items():
        total = sum(counter.values())
        if total < min_samples:
            continue
        cl = clusters(counter, base)
        if not cl:
            continue
        lo, hi, cnt, items = max(cl, key=lambda c: c[2])
        if cnt < min_cluster or cnt < min_share * total:
            continue
        if best is None or cnt > best["count"]:
            best = {"tid": tid, "lo": lo, "hi": hi, "count": cnt,
                    "total": total, "share": float(cnt) / total, "items": items}
    return best


def hottest(per_thread, base):
    """(tid, eip, hits) for the single most-seen EIP, preferring ge.exe code."""
    best = None
    for tid, counter in per_thread.items():
        for eip, n in counter.items():
            key = (1 if in_module(eip, base) else 0, n)
            if best is None or key > best[0]:
                best = (key, tid, eip, n)
    return None if not best else (best[1], best[2], best[3])


def busiest_in_module(per_thread, base):
    """(tid, hits) for the thread with the most samples in ge.exe's own code.

    Gate stack walking on THIS, not on hottest(): the globally hottest address
    is often a DLL wait (ntdll/user32) that no thread is really running, and
    gating on it skips the stack walk for every sample."""
    best = (None, 0)
    for tid, counter in per_thread.items():
        n = sum(c for a, c in counter.items() if in_module(a, base))
        if n > best[1]:
            best = (tid, n)
    return best


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------
def main():
    logf = open(LOG, "w", encoding="utf-8")

    def emit(s=""):
        print(s, flush=True)
        logf.write(s + "\n")
        logf.flush()

    emit("waiting for ge.exe (start it with its batch file if it is not running)...")
    pid = None
    for _ in range(600):
        pid = pick_pid()
        if pid:
            break
        time.sleep(2)
    if not pid:
        emit("ge.exe never appeared")
        return 1

    base, path = first_module(pid)
    emit("ge.exe pid=%d base=%#x %s" % (pid, base or 0, path or ""))
    try:
        pe = PeFile(EXE)
    except Exception as e:
        emit("cannot read local %s: %r" % (EXE, e))
        pe = None

    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        emit("OpenProcess failed: %d" % ctypes.get_last_error())
        return 1

    emit("sampling every %.2fs - click LOGIN now. Ctrl+C to stop and print the report.\n"
         % SAMPLE_INTERVAL)
    per_thread = defaultdict(Counter)         # tid -> Counter(eip)
    stack_hits = Counter()                    # return address -> times seen on the stack
    stack_scan_hits = Counter()               # in-module values anywhere on the stack
    last_regs = {}                            # tid -> most recent sample
    samples = 0
    t0 = time.time()
    limit = float(sys.argv[1]) if len(sys.argv) > 1 else 0.0
    stuck_streak = 0
    stuck_lo = None

    def print_report():
        emit("\n================ REPORT ================")
        emit("samples taken: %d over %.1fs" % (samples, time.time() - t0))
        if not per_thread:
            emit("no samples - is the client running?")
            return
        for tid, counter in sorted(per_thread.items(), key=lambda kv: -sum(kv[1].values())):
            total = sum(counter.values())
            emit("\nthread %d  (%d samples)" % (tid, total))
            cl = sorted(clusters(counter, base), key=lambda c: -c[2])[:15]
            if not cl:
                for eip, n in counter.most_common(3):
                    emit("   eip=%#08x hits=%-5d (outside ge.exe)"
                         % (eip, n))
                continue
            for lo, hi, cnt, items in cl:
                emit("   region %#08x..%#08x hits=%-5d %5.1f%%   (top addr %#x x%d)"
                     % (lo, hi, cnt, 100.0 * cnt / total, items[0][0], items[0][1]))
                if cnt >= 0.5 * total:
                    emit("      => STUCK here")
            outside = sorted(((e, n) for e, n in counter.items() if not in_module(e, base)),
                             key=lambda kv: -kv[1])[:3]
            for eip, n in outside:
                emit("   eip=%#08x hits=%-5d (outside ge.exe - a DLL)" % (eip, n))

        if stack_hits:
            emit("\n--- most frequent return addresses = the loop's ancestors ---")
            emit("(these stay on the stack every sample, so they name the caller chain)")
            for ret, n in stack_hits.most_common(15):
                emit("   ret=0x%08x rva=0x%-9x seen=%d" % (ret, ret - base, n))

        if stack_scan_hits:
            emit("\n--- stack scan: most frequent in-module values (caller chain) ---")
            emit("(frame-pointer-free code: these are return addresses sitting on the stack)")
            for ret, n in stack_scan_hits.most_common(20):
                emit("   ret=0x%08x rva=0x%-9x seen=%d" % (ret, ret - base, n))

        reg = stuck_region(per_thread, base)
        if reg:
            emit("\n--- HOTTEST: thread %d stuck in region %#x..%#x, %d/%d samples (%.0f%%) ---"
                 % (reg["tid"], reg["lo"], reg["hi"], reg["count"], reg["total"],
                    100.0 * reg["share"]))
            emit("=> That is a BUSY LOOP: %d bytes of code, hit %.0f%% of the time."
                 % (reg["hi"] - reg["lo"] + 1, 100.0 * reg["share"]))
            if pe:
                txt = pe.dump(reg["lo"], reg["hi"], "loop")
                if txt:
                    emit(txt)
            tid = reg["tid"]
        else:
            tid, eip, n = hottest(per_thread, base)
            emit("\n--- No dominant in-ge.exe region ---")
            emit("hottest address is %#x (%d hits)." % (eip, n))
            emit("If that is in ws2_32/ntdll the client is NOT looping - it is")
            emit("blocked (e.g. waiting on the socket), which is a different bug.")
            if pe:
                txt = pe.dump(eip, eip, "hottest")
                if txt:
                    emit(txt)

        s = last_regs.get(tid)
        if s:
            emit("\nregisters: eax=%#x ecx=%#x edx=%#x ebx=%#x esi=%#x edi=%#x ebp=%#x esp=%#x"
                 % (s["eax"], s["ecx"], s["edx"], s["ebx"], s["esi"], s["edi"],
                    s["ebp"], s["esp"]))
            emit("--- call path (EBP chain, innermost first) ---")
            ebp = s["ebp"]
            seen = set()
            for depth in range(32):
                if not ebp or ebp < 0x1000 or ebp > 0x7FFFFFFF or ebp in seen:
                    break
                seen.add(ebp)
                frame = read_mem(h, ebp, 8)
                if not frame:
                    break
                saved, ret = struct.unpack("<II", frame)
                where = ("rva %#x" % (ret - base)) if in_module(ret, base) else "outside ge.exe"
                emit("   frame %-2d ebp=%#010x ret=%#010x  %s" % (depth, ebp, ret, where))
                if saved <= ebp:
                    break
                ebp = saved

            emit("--- raw stack scan (may contain stale pointers) ---")
            esp = s["esp"]
            for depth in range(48):
                b = read_mem(h, esp, 4)
                if not b:
                    break
                val, = struct.unpack("<I", b)
                if in_module(val, base):
                    emit("   [esp+%#04x] = %#010x  <-- code, rva=%#x"
                         % (depth * 4, val, val - base))
                esp += 4
        emit("\nfull log: %s" % LOG)

    try:
        while True:
            tids = thread_ids(pid)
            if not tids:
                # the client may relaunch itself, so the pid can go away
                newpid = pick_pid(exclude=pid)
                if newpid:
                    pid = newpid
                    if h:
                        k32.CloseHandle(h)
                    base, path = first_module(pid)
                    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ,
                                        False, pid)
                    emit("\nre-attached to ge.exe pid=%d base=%#x" % (pid, base or 0))
                    tids = thread_ids(pid)
            for tid in tids:
                s = sample_thread(tid)
                if not s:
                    continue
                per_thread[tid][s["eip"]] += 1
                last_regs[tid] = s
            samples += 1

            hot = hottest(per_thread, base)
            if hot and samples % 10 == 0:
                emit("[%5.1fs] hottest thread %d: eip=%#x (%s) hits=%d"
                     % (time.time() - t0, hot[0], hot[1],
                        ("rva %#x" % (hot[1] - base)) if base else "?", hot[2]))
                if stack_scan_hits:
                    top = ", ".join("%#x x%d" % (a, n)
                                    for a, n in stack_scan_hits.most_common(4))
                    emit("          stack scan top: %s" % top)

            # Histogram the CALL PATH of the busy thread. The loop we are hunting
            # is deep inside; its ancestors stay on the stack every sample, so
            # the most frequent return addresses name the code that owns the loop.
            busy_tid, busy_hits = busiest_in_module(per_thread, base)
            if busy_tid is not None and busy_hits > 0:
                s2 = last_regs.get(busy_tid)
                if s2:
                    ebp = s2["ebp"]
                    seen2 = set()
                    for _ in range(20):
                        if not ebp or ebp < 0x1000 or ebp > 0x7FFFFFFF or ebp in seen2:
                            break
                        seen2.add(ebp)
                        fr = read_mem(h, ebp, 8)
                        if not fr:
                            break
                        saved, ret = struct.unpack("<II", fr)
                        if in_code(ret, base):
                            stack_hits[ret] += 1
                        if saved <= ebp:
                            break
                        ebp = saved

                    # The busy code often has NO frame pointer, so the EBP walk
                    # above can come up empty. Also scan a WINDOW of the stack
                    # for in-module addresses and accumulate across samples: the
                    # ones that recur name the caller chain.
                    raw = read_mem(h, s2["esp"], STACK_SCAN_BYTES)
                    if raw:
                        for off in range(0, len(raw) - 3, 4):
                            val, = struct.unpack_from("<I", raw, off)
                            if in_code(val, base):
                                stack_scan_hits[val] += 1

            reg = stuck_region(per_thread, base)
            if reg and reg["lo"] == stuck_lo:
                stuck_streak += 1
            else:
                stuck_lo = reg["lo"] if reg else None
                stuck_streak = 1 if reg else 0
            if stuck_streak >= 15:
                emit("\n*** HANG DETECTED: same code region %#x..%#x for %d samples (%.1fs),"
                     " %.0f%% of thread %d ***"
                     % (stuck_lo, reg["hi"], stuck_streak,
                        stuck_streak * SAMPLE_INTERVAL, 100.0 * reg["share"], reg["tid"]))
                break

            if limit and (time.time() - t0) >= limit:
                emit("\ntime limit %.0fs reached" % limit)
                break
            time.sleep(SAMPLE_INTERVAL)
    except KeyboardInterrupt:
        pass

    print_report()
    logf.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
