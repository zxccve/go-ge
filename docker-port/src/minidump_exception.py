"""minidump_exception - pull the faulting address out of a Windows minidump.

Why: the Windows event log only says "faulting module: BugTrap.dll", which is
the crash REPORTER, not the code that went wrong. The minidump has the real
ExceptionRecord: the faulting instruction address and the data address.

Minidump format (what we need):
  header: 'MDMP' + 4 version fields + NumberOfStreams + StreamDirectoryRva
  stream directory entries: StreamType(4) DataSize(4) Rva(4)
  stream type 6 = ExceptionStream:
      ThreadId(4) __alignment(4)
      ExceptionRecord: ExceptionCode(4) ExceptionFlags(4) ExceptionRecord(8)
                       ExceptionAddress(8) NumberParameters(4)
                       ExceptionInformation[15] (8 bytes each)
      ThreadContext: Location(4) ContextSize(4) then the CONTEXT

Usage:  python minidump_exception.py <file.dmp>
"""
import struct
import sys

EXCEPTION_STREAM = 6


def u32(d, o):
    return struct.unpack_from("<I", d, o)[0]


def u64(d, o):
    return struct.unpack_from("<Q", d, o)[0]


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return
    path = sys.argv[1]
    d = open(path, "rb").read(8 * 1024 * 1024)   # the header+streams are at the front
    if d[:4] != b"MDMP":
        print("not a minidump (no MDMP signature)")
        return

    n_streams = u32(d, 8)
    dir_rva = u32(d, 12)
    print("minidump: %s" % path)
    print("streams: %d (directory at %#x)" % (n_streams, dir_rva))

    exc = None
    for i in range(n_streams):
        o = dir_rva + i * 12
        if o + 12 > len(d):
            break
        stype = u32(d, o)
        size = u32(d, o + 4)
        rva = u32(d, o + 8)
        if stype == EXCEPTION_STREAM:
            exc = rva
            print("  exception stream at %#x (%d bytes)" % (rva, size))
        else:
            print("  stream type %-3d at %#x (%d bytes)" % (stype, rva, size))

    if exc is None:
        print("no exception stream")
        return

    o = exc
    thread_id = u32(d, o)
    code = u32(d, o + 8)
    flags = u32(d, o + 12)
    address = u64(d, o + 24)
    n_params = u32(d, o + 32)
    params = []
    for i in range(min(n_params, 15)):
        params.append(u64(d, o + 36 + i * 8))

    print()
    print("thread id          : %d" % thread_id)
    print("exception code     : %#010x" % code)
    print("exception flags    : %#x" % flags)
    print("exception address  : %#018x" % address)
    print("parameters         : %d" % n_params)
    for i, p in enumerate(params):
        kind = ""
        if i == 0 and code in (0xC0000005, 0xC0000006):
            kind = "  (0=read, 1=write, 8=exec)"
        if i == 1 and code in (0xC0000005, 0xC0000006):
            kind = "  (the data address)"
        print("   [%d] %#018x%s" % (i, p, kind))

    # Module list (stream type 4) to name the module the address belongs to.
    print()
    mod_rva = None
    for i in range(n_streams):
        o = dir_rva + i * 12
        if o + 12 > len(d):
            break
        if u32(d, o) == 4:
            mod_rva = u32(d, o + 8)
    if mod_rva is not None:
        n_mod = u32(d, mod_rva)
        print("modules: %d" % n_mod)
        hit = None
        for i in range(n_mod):
            mo = mod_rva + 4 + i * 108
            if mo + 108 > len(d):
                break
            base = u64(d, mo)
            size = u32(d, mo + 8)
            name_rva = u32(d, mo + 92)
            nl = u32(d, name_rva)
            name = d[name_rva + 4:name_rva + 4 + nl].decode("utf-16-le", "replace")
            name = name.replace("\x00", "")
            # The console here is cp1252 and a module path can carry
            # characters it cannot encode - keep it printable, not fatal.
            name = name.encode("ascii", "replace").decode("ascii")
            if base <= address < base + size:
                hit = (name, base, size)
        if hit:
            name, base, size = hit
            print("  ** fault is in %s" % name)
            print("     module base %#x, size %#x" % (base, size))
            print("     -> RVA %#x  (VA = imagebase + RVA = %#x)"
                  % (address - base, address))
        else:
            print("  fault address is not inside any listed module")


if __name__ == "__main__":
    main()
