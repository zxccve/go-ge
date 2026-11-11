"""watch_key - breakpoint at the crash's lookup call and print the lookup key.

The barrack actor path does:
    FUN_004b6575(); mov ecx,eax; call FUN_00645a68   ; returns a JOB/item record
    mov edi,eax; mov cl,[edi+0x34]                   ; faults when it returned 0
So BEFORE the call, ECX holds the key that was not found. Break at 0x5ee287
(the `call 0x645a68`), print ECX, continue. The last line before the crash is
the missing key.

0xCC breakpoint + trap-flag dance (skill item 8). READ-ONLY apart from the
one-byte 0xCC at our chosen address, which is restored immediately.

Run: python watch_key.py   (then click LOGIN)
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
DBG_EXCEPTION_NOT_HANDLED = 0x80010001
EXCEPTION_DEBUG_EVENT = 1
EXCEPTION_BREAKPOINT = 0x80000003
EXCEPTION_SINGLE_STEP = 0x80000004
EXCEPTION_ACCESS_VIOLATION = 0xC0000005
CONTEXT_FLAGS = 0x00010007

BP_ADDR = 0x5EE287           # call FUN_00645a68 in the actor creator


class EXCEPTION_RECORD(ctypes.Structure):
    _fields_ = [("ExceptionCode", wt.DWORD), ("ExceptionFlags", wt.DWORD),
                ("ExceptionRecord", ctypes.c_void_p), ("ExceptionAddress", ctypes.c_void_p),
                ("NumberParameters", wt.DWORD),
                ("ExceptionInformation", ctypes.c_void_p * 15)]


class EXCEPTION_DEBUG_INFO(ctypes.Structure):
    _fields_ = [("ExceptionRecord", EXCEPTION_RECORD), ("dwFirstChance", wt.DWORD)]


class DEBUG_EVENT(ctypes.Structure):
    _fields_ = [("dwDebugEventCode", wt.DWORD), ("dwProcessId", wt.DWORD),
                ("dwThreadId", wt.DWORD), ("_pad", wt.DWORD),
                ("u", ctypes.c_byte * 160)]


class WOW64_CONTEXT(ctypes.Structure):
    _fields_ = [("ContextFlags", wt.DWORD),
                ("Dr0", wt.DWORD), ("Dr1", wt.DWORD), ("Dr2", wt.DWORD), ("Dr3", wt.DWORD),
                ("Dr6", wt.DWORD), ("Dr7", wt.DWORD), ("FloatSave", ctypes.c_byte * 112),
                ("SegGs", wt.DWORD), ("SegFs", wt.DWORD), ("SegEs", wt.DWORD), ("SegDs", wt.DWORD),
                ("Edi", wt.DWORD), ("Esi", wt.DWORD), ("Ebx", wt.DWORD), ("Edx", wt.DWORD),
                ("Ecx", wt.DWORD), ("Eax", wt.DWORD), ("Ebp", wt.DWORD), ("Eip", wt.DWORD),
                ("SegCs", wt.DWORD), ("EFlags", wt.DWORD), ("Esp", wt.DWORD), ("SegSs", wt.DWORD)]


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


def write_byte(h, addr, b):
    got = ctypes.c_size_t(0)
    return k32.WriteProcessMemory(h, ctypes.c_void_p(addr), ctypes.c_char(b), 1, ctypes.byref(got))


def get_ctx(th):
    c = WOW64_CONTEXT()
    c.ContextFlags = CONTEXT_FLAGS
    if wow.Wow64GetThreadContext(wt.HANDLE(th), ctypes.byref(c)):
        return c
    return None


def set_ctx(th, c):
    c.ContextFlags = CONTEXT_FLAGS
    return wow.Wow64SetThreadContext(wt.HANDLE(th), ctypes.byref(c))


def main():
    print("waiting for ge.exe ...", flush=True)
    pid = None
    for _ in range(300):
        pids = pids_of("ge.exe")
        if pids:
            pid = pids[0]
            break
        time.sleep(2)
    if not pid:
        print("ge.exe never appeared")
        return
    print("attaching to ge.exe pid", pid, flush=True)
    if not k32.DebugActiveProcess(pid):
        print("DebugActiveProcess failed:", ctypes.get_last_error())
        return
    h = k32.OpenProcess(PROCESS_ALL_ACCESS, False, pid)
    orig = read_mem(h, BP_ADDR, 1)
    if not orig:
        print("cannot read BP address")
        return
    if not write_byte(h, BP_ADDR, b"\xcc"):
        print("cannot write breakpoint")
        return
    print("breakpoint armed at %#x. click LOGIN (Ctrl+C to stop)\n" % BP_ADDR, flush=True)

    pending = False
    ev = DEBUG_EVENT()
    while True:
        if not k32.WaitForDebugEvent(ctypes.byref(ev), 3000):
            continue
        pid_ev, tid = ev.dwProcessId, ev.dwThreadId
        code = ev.dwDebugEventCode
        if code != EXCEPTION_DEBUG_EVENT:
            k32.ContinueDebugEvent(pid_ev, tid, DBG_CONTINUE)
            continue
        er = EXCEPTION_DEBUG_INFO.from_buffer_copy(bytes(ev.u))
        ecode = er.ExceptionRecord.ExceptionCode
        th = k32.OpenThread(0x0008 | 0x0010, False, tid)
        ctx = get_ctx(th) if th else None

        if ecode == EXCEPTION_BREAKPOINT:
            eip = ctx.Eip if ctx else 0
            print("BREAKPOINT eip=%#x (want %#x)" % (eip, BP_ADDR), flush=True)
            if ctx and eip in (BP_ADDR, BP_ADDR + 1):
                mem_ebx = read_mem(h, ctx.Ebx, 8) or b""
                key = struct.unpack_from("<I", mem_ebx, 0)[0] if len(mem_ebx) >= 4 else None
                key2 = struct.unpack_from("<I", mem_ebx, 4)[0] if len(mem_ebx) >= 8 else None
                print("KEY: ecx(table)=%#x  [ebx]=%s  [ebx+4]=%s  eax=%#x  edi=%#x  ebx=%#x"
                      % (ctx.Ecx, hex(key) if key is not None else "?",
                         hex(key2) if key2 is not None else "?",
                         ctx.Eax, ctx.Edi, ctx.Ebx), flush=True)
                write_byte(h, BP_ADDR, orig)
                ctx.Eip = BP_ADDR
                ctx.EFlags |= 0x100
                set_ctx(th, ctx)
                pending = True
                k32.ContinueDebugEvent(pid_ev, tid, DBG_CONTINUE)
                continue
            k32.ContinueDebugEvent(pid_ev, tid, DBG_CONTINUE)
            continue

        if ecode == EXCEPTION_SINGLE_STEP and pending:
            write_byte(h, BP_ADDR, b"\xcc")
            pending = False
            k32.ContinueDebugEvent(pid_ev, tid, DBG_CONTINUE)
            continue

        if ecode == EXCEPTION_ACCESS_VIOLATION:
            info = er.ExceptionRecord.ExceptionInformation
            kind = (info[0] or 0)
            bad = (info[1] or 0)
            print("ACCESS VIOLATION instr=%#x read/write=%s addr=%#x"
                  % (er.ExceptionRecord.ExceptionAddress,
                     {0: "READ", 1: "WRITE", 8: "EXEC"}.get(kind, str(kind)), bad), flush=True)
            if ctx:
                print("  EIP=%#x EAX=%#x ECX=%#x EDI=%#x ESI=%#x EBP=%#x"
                      % (ctx.Eip, ctx.Eax, ctx.Ecx, ctx.Edi, ctx.Esi, ctx.Ebp), flush=True)
            write_byte(h, BP_ADDR, orig)
            k32.DebugActiveProcessStop(pid)
            print("--- detached ---", flush=True)
            return

        k32.ContinueDebugEvent(pid_ev, tid, DBG_CONTINUE)


if __name__ == "__main__":
    main()
