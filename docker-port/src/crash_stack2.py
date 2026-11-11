"""crash_stack2 - like crash_stack.py but reports the AV data address + access type.

The old script printed only the instruction address (ExceptionAddress). For an
access violation we also want ExceptionInformation[0] (0=read, 1=write, 8=exec)
and [1] (the data address). That tells us whether the memcpy faulted reading
the source (packet) or writing the destination (session array).

READ-ONLY: reads memory, continues events, never writes the target.

Run:  python crash_stack2.py     (then click LOGIN in the client)
"""
import ctypes
import ctypes.wintypes as wt
import struct
import subprocess
import time
from ge_paths import GAME_ROOT

k32 = ctypes.WinDLL("kernel32", use_last_error=True)
wow = ctypes.WinDLL("kernel32")

PROCESS_ALL_ACCESS = 0x1F0FFF
DBG_CONTINUE = 0x00010002
EXCEPTION_DEBUG_EVENT = 1
EXCEPTION_ACCESS_VIOLATION = 0xC0000005
CONTEXT_FLAGS = 0x00010007
IMG_BASE = 0x400000
CODE_LO, CODE_HI = 0x401000, 0xE40000

ACCESS = {0: "READ", 1: "WRITE", 8: "EXEC"}


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


def prologue_before(data, va):
    off = va - IMG_BASE - 0x1000 + 0x400
    seg = data[max(0, off - 4096):off]
    j = 0
    best = None
    while True:
        j = seg.find(b"\x55\x8b\xec", j)
        if j < 0:
            break
        best = max(0, off - 4096) + j
        j += 1
    return (IMG_BASE + 0x1000 + (best - 0x400)) if best else None


def dump(sess, h, label, addr):
    b = read_mem(h, addr, 16)
    if b is None:
        print("     %s @ %#x -> UNMAPPED" % (label, addr))
        return
    print("     %s @ %#x -> %s" % (label, addr, " ".join("%02X" % x for x in b)))


def main():
    print("waiting for ge.exe (launch the client if it is not running)...", flush=True)
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
    data = open(os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe"), "rb").read()
    print("attached. click LOGIN in the client now (Ctrl+C to stop)\n", flush=True)

    ev = DEBUG_EVENT()
    while True:
        if not k32.WaitForDebugEvent(ctypes.byref(ev), 3000):
            continue
        if ev.dwDebugEventCode != EXCEPTION_DEBUG_EVENT:
            k32.ContinueDebugEvent(ev.dwProcessId, ev.dwThreadId, DBG_CONTINUE)
            continue
        er = EXCEPTION_DEBUG_INFO.from_buffer_copy(bytes(ev.u))
        code = er.ExceptionRecord.ExceptionCode
        addr = er.ExceptionRecord.ExceptionAddress or 0
        npar = er.ExceptionRecord.NumberParameters
        info = [er.ExceptionRecord.ExceptionInformation[i] or 0 for i in range(npar)]
        th = k32.OpenThread(0x0008 | 0x0010, False, ev.dwThreadId)
        ctx = WOW64_CONTEXT()
        ctx.ContextFlags = CONTEXT_FLAGS
        got = th and wow.Wow64GetThreadContext(wt.HANDLE(th), ctypes.byref(ctx))
        if code != EXCEPTION_ACCESS_VIOLATION:
            print("EXCEPTION code=%#x addr=%#x first=%d (ignored)"
                  % (code, addr, er.dwFirstChance), flush=True)
            k32.ContinueDebugEvent(ev.dwProcessId, ev.dwThreadId, DBG_CONTINUE)
            continue

        print("ACCESS VIOLATION code=%#x instr=%#x first=%d" % (code, addr, er.dwFirstChance),
              flush=True)
        if npar >= 2:
            kind = ACCESS.get(info[0], "?%d" % info[0])
            bad = info[1]
            print("  FAULT: %s at %#x" % (kind, bad), flush=True)
            dump(0, h, "fault mem", bad)
        if got:
            print("  EIP=%#x ESP=%#x EBP=%#x" % (ctx.Eip, ctx.Esp, ctx.Ebp), flush=True)
            print("  EAX=%#x ECX=%#x EDX=%#x EBX=%#x ESI=%#x EDI=%#x"
                  % (ctx.Eax, ctx.Ecx, ctx.Edx, ctx.Ebx, ctx.Esi, ctx.Edi), flush=True)
            dump(0, h, "  [esi]", ctx.Esi)
            dump(0, h, "  [edi]", ctx.Edi)
            for off in (0x204, 0x208, 0x224, 0x228):
                v = read_mem(h, ctx.Edi + off, 4)
                if v:
                    import struct as _s
                    print("     [edi+%#x] = %#x" % (off, _s.unpack("<I", v)[0]), flush=True)
            blk = read_mem(h, ctx.Edi + 0x1E0, 0x90)
            if blk:
                import struct as _s
                for i in range(0, len(blk), 16):
                    vals = _s.unpack_from("<4I", blk, i)
                    print("     edi+%#04x: %s" % (0x1E0 + i,
                          " ".join("%08x" % v for v in vals)), flush=True)
            if ctx.Ebp:
                frame = read_mem(h, ctx.Ebp, 0x14)
                if frame:
                    saved_ebp, ret, a0, a1 = struct.unpack_from("<IIII", frame, 0)
                    print("  frame: saved_ebp=%#x ret=%#x [ebp+8]=%#x [ebp+0xC]=%#x"
                          % (saved_ebp, ret, a0, a1), flush=True)
                    dump(0, h, "  [ebp+0xC] src", a1)
        if got:
            print("  --- EBP chain ---", flush=True)
            ebp = ctx.Ebp
            for depth in range(24):
                if not ebp or ebp < 0x1000 or ebp > 0x7FFFFFFF:
                    break
                frame = read_mem(h, ebp, 8)
                if not frame:
                    break
                saved_ebp, ret = struct.unpack("<II", frame)
                fn = prologue_before(data, ret) if CODE_LO <= ret < CODE_HI else None
                print("    ret=%#x   fn=%s" % (ret, hex(fn) if fn else "-"), flush=True)
                if saved_ebp <= ebp:
                    break
                ebp = saved_ebp
        k32.DebugActiveProcessStop(pid)
        print("\n--- detached ---", flush=True)
        return


if __name__ == "__main__":
    main()
