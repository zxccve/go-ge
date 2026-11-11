"""Oracle step 2: find who BUILDS the login packet (minimal 32-bit debugger).

Attaches to the running ge.exe, breakpoints the Windows socket send/connect
functions, and prints the caller address inside ge.exe plus the bytes sent.
The caller of the 816-byte send is the packet builder -> decompile in Ghidra.

READ-ONLY apart from one 0xCC breakpoint byte per target, restored on exit.

Run BOTH of these first:
    python capture_set.py     (the fake server; the client needs a listener)
  then, in another terminal:
    python oracle_send.py     (this debugger)
...then click LOGIN in the client.
"""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess
import time

k32 = ctypes.WinDLL("kernel32", use_last_error=True)
wow = ctypes.WinDLL("kernel32")

PROCESS_ALL_ACCESS = 0x1F0FFF
DBG_CONTINUE = 0x00010002
EXCEPTION_DEBUG_EVENT = 1
EXCEPTION_BREAKPOINT = 0x80000003
EXCEPTION_SINGLE_STEP = 0x80000004
CONTEXT_FLAGS = 0x00010007
TRAP_FLAG = 0x100

# name -> (dll, export) we want to break on
TARGETS = [
    ("send", ("ws2_32.dll", "send")),
    ("WSASend", ("ws2_32.dll", "WSASend")),
    ("sendto", ("ws2_32.dll", "sendto")),
    ("WSASendTo", ("ws2_32.dll", "WSASendTo")),
    ("connect", ("ws2_32.dll", "connect")),
]


class EXCEPTION_RECORD(ctypes.Structure):
    _fields_ = [("ExceptionCode", wt.DWORD), ("ExceptionFlags", wt.DWORD),
                ("ExceptionRecord", ctypes.c_void_p), ("ExceptionAddress", ctypes.c_void_p),
                ("NumberParameters", wt.DWORD), ("ExceptionInformation", ctypes.c_void_p * 15)]


class EXCEPTION_DEBUG_INFO(ctypes.Structure):
    _fields_ = [("ExceptionRecord", EXCEPTION_RECORD), ("dwFirstChance", wt.DWORD)]


class DEBUG_EVENT(ctypes.Structure):
    _fields_ = [("dwDebugEventCode", wt.DWORD), ("dwProcessId", wt.DWORD),
                ("dwThreadId", wt.DWORD), ("u", ctypes.c_byte * 160)]


class WOW64_CONTEXT(ctypes.Structure):
    _fields_ = [("ContextFlags", wt.DWORD),
                ("Dr0", wt.DWORD), ("Dr1", wt.DWORD), ("Dr2", wt.DWORD), ("Dr3", wt.DWORD),
                ("Dr6", wt.DWORD), ("Dr7", wt.DWORD),
                ("FloatSave", ctypes.c_byte * 112),
                ("SegGs", wt.DWORD), ("SegFs", wt.DWORD), ("SegEs", wt.DWORD), ("SegDs", wt.DWORD),
                ("Edi", wt.DWORD), ("Esi", wt.DWORD), ("Ebx", wt.DWORD), ("Edx", wt.DWORD),
                ("Ecx", wt.DWORD), ("Eax", wt.DWORD), ("Ebp", wt.DWORD), ("Eip", wt.DWORD),
                ("SegCs", wt.DWORD), ("EFlags", wt.DWORD), ("Esp", wt.DWORD), ("SegSs", wt.DWORD)]


def pids_of(name):
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


def module_base(pid, dllname):
    TH32CS_SNAPMODULE = 0x08
    TH32CS_SNAPMODULE32 = 0x10
    MAX_PATH = 260

    class MODULEENTRY32(ctypes.Structure):
        _fields_ = [("dwSize", wt.DWORD), ("th32ModuleID", wt.DWORD), ("th32ProcessID", wt.DWORD),
                    ("GlblcntUsage", wt.DWORD), ("ProccntUsage", wt.DWORD),
                    ("modBaseAddr", ctypes.c_void_p), ("modBaseSize", wt.DWORD),
                    ("hModule", wt.HMODULE), ("szModule", ctypes.c_char * (MAX_PATH + 1)),
                    ("szExePath", ctypes.c_char * (MAX_PATH + 1))]

    snap = k32.CreateToolhelp32Snapshot(TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32, pid)
    if snap == -1:
        return None
    me = MODULEENTRY32()
    me.dwSize = ctypes.sizeof(me)
    ok = k32.Module32First(snap, ctypes.byref(me))
    while ok:
        if me.szModule.decode(errors="ignore").lower() == dllname.lower():
            r = (me.modBaseAddr or 0, me.modBaseSize)
            k32.CloseHandle(snap)
            return r
        ok = k32.Module32Next(snap, ctypes.byref(me))
    k32.CloseHandle(snap)
    return None


_pe_cache = {}


def exe_base_offset(data):
    e = struct.unpack_from("<I", data, 0x3C)[0]
    nsec = struct.unpack_from("<H", data, e + 6)[0]
    optsz = struct.unpack_from("<H", data, e + 20)[0]
    return e, nsec, e + 24, optsz


def export_rva(data, name):
    e, nsec, opt, optsz = exe_base_offset(data)
    magic = struct.unpack_from("<H", data, opt)[0]
    dd = opt + (96 if magic == 0x10B else 112)
    exp_rva = struct.unpack_from("<II", data, dd)[0]
    sec = opt + optsz

    def rva2off(rva):
        for i in range(nsec):
            o = sec + i * 40
            vs, va, rs, rp = struct.unpack_from("<IIII", data, o + 8)
            if va <= rva < va + max(vs, rs):
                return rp + (rva - va)
        return None

    eo = rva2off(exp_rva)
    nnam = struct.unpack_from("<I", data, eo + 24)[0]
    afun, anam, aord = struct.unpack_from("<III", data, eo + 28)
    for i in range(nnam):
        name_rva = struct.unpack_from("<I", data, rva2off(anam) + i * 4)[0]
        noff = rva2off(name_rva)
        nm = data[noff:data.find(b"\0", noff)].decode()
        if nm == name:
            ordv = struct.unpack_from("<H", data, rva2off(aord) + i * 2)[0]
            return struct.unpack_from("<I", data, rva2off(afun) + ordv * 4)[0]
    return None


def export_of(dllpath, name):
    key = (dllpath, name)
    if key not in _pe_cache:
        _pe_cache[key] = open(dllpath, "rb").read()
    return export_rva(_pe_cache[key], name)


def read_mem(h, addr, n):
    buf = ctypes.create_string_buffer(n)
    got = ctypes.c_size_t(0)
    if not k32.ReadProcessMemory(h, ctypes.c_void_p(addr), buf, n, ctypes.byref(got)):
        return None
    return buf.raw[:got.value]


def main():
    pids = pids_of("ge.exe")
    if not pids:
        print("ge.exe not running. Start the client and get to the login screen.")
        return
    pid = pids[0]
    print("attaching to ge.exe pid", pid)

    ws = module_base(pid, "ws2_32.dll")
    if not ws:
        print("ws2_32.dll not loaded in target yet - is the client at the login screen?")
        return
    sysdir = os.path.join(os.environ["SystemRoot"], "SysWOW64", "ws2_32.dll")
    targets = {}
    for label, (_dll, fn) in TARGETS:
        rva = export_of(sysdir, fn)
        if rva is not None:
            targets[ws[0] + rva] = label
    print("ws2_32 base=0x%08X" % ws[0])
    for a, n in sorted(targets.items()):
        print("   %-10s 0x%08X" % (n, a))

    if not k32.DebugActiveProcess(pid):
        print("DebugActiveProcess failed:", ctypes.get_last_error())
        return
    h = k32.OpenProcess(PROCESS_ALL_ACCESS, False, pid)
    if not h:
        print("OpenProcess failed:", ctypes.get_last_error())
        k32.DebugActiveProcessStop(pid)
        return

    originals = {}
    written = ctypes.c_size_t(0)
    for a in targets:
        b = read_mem(h, a, 1)
        if b:
            originals[a] = b
            k32.WriteProcessMemory(h, ctypes.c_void_p(a), b"\xCC", 1, ctypes.byref(written))
    print("\nbreakpoints set. Click LOGIN in the client now. (Ctrl+C to stop)\n")

    ev = DEBUG_EVENT()
    hits = 0
    rearm = None
    last_beat = time.time()
    try:
        while True:
            if not k32.WaitForDebugEvent(ctypes.byref(ev), 2000):
                if time.time() - last_beat > 15:
                    print("   ...still waiting (no socket call yet)", flush=True)
                    last_beat = time.time()
                continue
            last_beat = time.time()
            if ev.dwDebugEventCode != EXCEPTION_DEBUG_EVENT:
                k32.ContinueDebugEvent(ev.dwProcessId, ev.dwThreadId, DBG_CONTINUE)
                continue
            er = EXCEPTION_DEBUG_INFO.from_buffer_copy(bytes(ev.u))
            code = er.ExceptionRecord.ExceptionCode
            addr = er.ExceptionRecord.ExceptionAddress or 0
            th = k32.OpenThread(0x0008 | 0x0010, False, ev.dwThreadId)
            ctx = WOW64_CONTEXT()
            ctx.ContextFlags = CONTEXT_FLAGS
            got = th and wow.Wow64GetThreadContext(wt.HANDLE(th), ctypes.byref(ctx))
            if code == EXCEPTION_BREAKPOINT and got and (addr - 1) in targets:
                name = targets[addr - 1]
                stack = read_mem(h, ctx.Esp, 24)
                if stack:
                    words = struct.unpack("<IIIIII", stack)
                    ret = words[0]
                    if name == "connect":
                        sock = words[1]
                        sa = words[2]
                        port = None
                        if sa:
                            sa_raw = read_mem(h, sa, 8)
                            if sa_raw:
                                port = struct.unpack_from(">H", sa_raw, 2)[0]
                        print("CONNECT caller=0x%08X sock=%d port=%s" % (ret, sock, port),
                              flush=True)
                    else:
                        # send/WSASend: args are (sock, buf, len, ...)
                        sock, buf, ln = words[1], words[2], words[3]
                        head = read_mem(h, buf, min(ln, 24)) or b""
                        print("SEND[%s] len=%-6d caller=0x%08X sock=%d head=%s"
                              % (name, ln, ret, sock, head.hex()), flush=True)
                        hits += 1
                # step over, then re-arm the breakpoint
                k32.WriteProcessMemory(h, ctypes.c_void_p(addr - 1), originals[addr - 1], 1,
                                       ctypes.byref(written))
                ctx.Eip = addr - 1
                ctx.EFlags |= TRAP_FLAG
                wow.Wow64SetThreadContext(wt.HANDLE(th), ctypes.byref(ctx))
                rearm = addr - 1
            elif code == EXCEPTION_SINGLE_STEP and rearm is not None:
                k32.WriteProcessMemory(h, ctypes.c_void_p(rearm), b"\xCC", 1,
                                       ctypes.byref(written))
                rearm = None
            if th:
                k32.CloseHandle(th)
            k32.ContinueDebugEvent(ev.dwProcessId, ev.dwThreadId, DBG_CONTINUE)
            if hits >= 8:
                print("\nstopping after", hits, "sends")
                break
    except KeyboardInterrupt:
        print("\nstopped by user")
    finally:
        for a, b in originals.items():
            k32.WriteProcessMemory(h, ctypes.c_void_p(a), b, 1, ctypes.byref(written))
        k32.CloseHandle(h)
        k32.DebugActiveProcessStop(pid)
        print("detached")


if __name__ == "__main__":
    main()
