"""dump_cmds - print the LIVE client's packet table (id, name, size) from memory.

Why: `data\\cmd_table.txt` is a PARTIAL dump - it covers only part of the id
range (it starts at 256 and its tail is 244..255), so "this id is not in the
table" does NOT prove the client does not know it. The authoritative table
lives in the running client's memory (`gePacketTable`): an array of 50-byte
name slots plus an array of 4-byte sizes.

Both arrays are found by SEARCH, not by a fixed offset, because the offset
between them moved between client builds:

  names array  - found by locating the string `BC_COMMANDER_INFO` (it is
    name[10], so the array base is 10 * 50 = 500 bytes below it).
  sizes array  - found by looking for the offset where sizes[10] == 744
    (BC_COMMANDER_INFO's size) and sizes[57] == 12 (BC_LOGINOK's size).
    Both are recorded in `history\\barrack.md` / `RESUME.md`.

Usage:
    python dump_cmds.py                 # dump every readable id
    python dump_cmds.py 0 120           # dump an id range
    python dump_cmds.py --name CZ_START_GAME   # look one name up
    python dump_cmds.py --out data\\cmd_table_live.txt

The client must be RUNNING (it reads the process, not the file).
"""
import ctypes
import ctypes.wintypes as wt
import struct
import sys

PQI, PVR = 0x0400, 0x0010
MEM_COMMIT, PAGE_GUARD, PAGE_NOACCESS = 0x1000, 0x100, 0x01
NAMES_STRIDE = 50

# Known anchors, read out of the shipped client (history/barrack.md,
# history/client.md, RESUME.md "Shipped client's real ids/sizes").
#
# A PROFILE is ONE client build's anchor set. The ids below are all < 56, so
# they are the same in the 2020 build and in the JP 375639 build: the JP only
# inserted CB_COMMANDER_TRANSFER at 56 and shifted what comes after it (see
# history\jp-client.md, 2026-11-06). What differs between builds is the SIZES -
# the JP BC_LOGINOK is 61 bytes (not 57), BC_COMMANDER_INFO 880 (not 744).
PROFILES = {
    "v32": {
        # name -> command id (all < 56, unshifted in JP too)
        "names": {
            "BC_LOGINOK": 8,
            "BC_COMMANDER_INFO": 10,
            "BC_BARRACK_INFO": 16,
            "BC_BARRACK_EQUIP_INFO_EXTRA": 17,
            "BC_STARTGAMEOK": 25,
            "BC_LOGGING": 30,
            "BC_CHANNEL_LIST": 35,
            "BC_COMMANDER_LV_INFO": 37,
        },
        # id -> expected size, used to locate the sizes array
        "sizes": {8: 57, 10: 744},
        # names->sizes distance on the 2020 build, tried before searching
        "sizes_offset": 3276748,
    },
    "jp": {
        "names": {
            "BC_MESSAGE": 3,
            "CB_LOGIN": 4,
            "BC_LOGINOK": 8,
            "BC_COMMANDER_INFO": 10,
            "BC_BARRACK_INFO": 16,
            "BC_STARTGAMEOK": 25,
            "BC_LOGGING": 30,
            "BC_CHANNEL_LIST": 35,
            "BC_COMMANDER_LV_INFO": 37,
        },
        "sizes": {8: 61, 10: 880},
        "sizes_offset": None,
    },
}


class MBI(ctypes.Structure):
    _fields_ = [("BaseAddress", ctypes.c_void_p), ("AllocationBase", ctypes.c_void_p),
                ("AllocationProtect", wt.DWORD), ("RegionSize", ctypes.c_size_t),
                ("State", wt.DWORD), ("Protect", wt.DWORD), ("Type", wt.DWORD)]


k32 = ctypes.WinDLL("kernel32", use_last_error=True)
k32.OpenProcess.restype = wt.HANDLE
k32.VirtualQueryEx.argtypes = [wt.HANDLE, ctypes.c_void_p, ctypes.POINTER(MBI), ctypes.c_size_t]
k32.ReadProcessMemory.argtypes = [wt.HANDLE, ctypes.c_void_p, ctypes.c_void_p,
                                  ctypes.c_size_t, ctypes.POINTER(ctypes.c_size_t)]


def _ge_processes():
    """[(exe path, pid)] for every running ge.exe - matched by PATH.

    There can be THREE ge.exe on this host at once (two repo clients and the
    owner's Andro install), and `tasklist` returns whichever comes first.
    Dumping the wrong one writes a plausible-looking table for the wrong
    build, so the caller pins the path it wants with GE_EXE.
    """
    import subprocess
    ps = ("Get-CimInstance Win32_Process -Filter \"Name='ge.exe'\" | "
          "ForEach-Object { $_.ExecutablePath + \"`t\" + $_.ProcessId }")
    out = subprocess.run(["powershell.exe", "-NoProfile", "-Command", ps],
                         capture_output=True, text=True).stdout
    rows = []
    for line in out.splitlines():
        if "\t" in line:
            p, pid = line.rsplit("\t", 1)
            if pid.strip().isdigit():
                rows.append((p.strip(), int(pid)))
    return rows


def process_path(pid):
    for path, p in _ge_processes():
        if p == pid:
            return path
    return None


def pid_of(name="ge.exe"):
    import os
    import subprocess
    forced = os.environ.get("GE_PID")
    if forced and forced.isdigit():
        return int(forced)
    want = os.environ.get("GE_EXE")
    if want:
        for path, pid in _ge_processes():
            if path.lower().endswith(want.lower()):
                return pid
        return None
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq " + name, "/FO", "CSV", "/NH"],
                         capture_output=True, text=True).stdout
    for line in out.splitlines():
        p = [x.strip('"') for x in line.split('","')]
        if len(p) >= 2 and p[1].isdigit():
            return int(p[1])
    return None


def rd(h, addr, n):
    b = ctypes.create_string_buffer(n)
    got = ctypes.c_size_t(0)
    k32.ReadProcessMemory(h, ctypes.c_void_p(addr), b, n, ctypes.byref(got))
    return b.raw[:got.value]


def committed_regions(h):
    mbi = MBI()
    addr = 0
    while addr < 0x7FFFFFFF:
        if not k32.VirtualQueryEx(h, ctypes.c_void_p(addr), ctypes.byref(mbi), ctypes.sizeof(mbi)):
            break
        base, size = mbi.BaseAddress or 0, mbi.RegionSize
        if mbi.State == MEM_COMMIT and not (mbi.Protect & (PAGE_GUARD | PAGE_NOACCESS)):
            yield base, size
        addr = base + size


def find_all(h, needle):
    """Every address holding `needle` (NUL-terminated), across committed memory."""
    hits = []
    for base, size in committed_regions(h):
        d = rd(h, base, size)
        i = d.find(needle)
        while i >= 0:
            hits.append(base + i)
            i = d.find(needle, i + 1)
    return hits


def _looks_like_name(s):
    """A packet name is ASCII upper/underscore/digits, at least 4 chars."""
    if len(s) < 4:
        return False
    return all(65 <= c <= 90 or 97 <= c <= 122 or c in (95, 48) for c in s)


def find_names_base(h, name_ids):
    """The names array base, chosen by requiring ALL known (name, id) pairs.

    Searching memory for one known name finds SEVERAL copies - the real
    table, a .rdata copy, a heap copy - and they are NOT interchangeable. The
    wrong one still yields plausible-looking output, but the names are
    truncated FROM THE FRONT (`COMMANDEROK` instead of `BC_COMMANDEROK`),
    because the read lands inside a string instead of on a slot boundary.

    So: try every candidate base (one per occurrence of each known name) and
    keep the one where EVERY known name sits at exactly `id * stride`.
    Scoring by "how many look plausible" is weaker - a shifted read still
    looks plausible.
    """
    cands = set()
    for nm, cid in name_ids.items():
        for a in find_all(h, (nm + "\x00").encode()):
            cands.add(a - cid * NAMES_STRIDE)
    best = None
    for nb in cands:
        if nb < 0:
            continue
        score = 0
        for nm, cid in name_ids.items():
            raw = rd(h, nb + cid * NAMES_STRIDE, NAMES_STRIDE)
            if len(raw) < NAMES_STRIDE:
                break
            if raw.split(b"\x00")[0].decode("latin-1", "replace") == nm:
                score += 1
        if score and (best is None or score > best[0]):
            best = (score, nb)
    return best[1] if best else None


def read_i32(h, addr):
    """Read a 4-byte int, or None if the read failed (page not mapped)."""
    b = rd(h, addr, 4)
    if len(b) != 4:
        return None
    return struct.unpack("<i", b)[0]


def find_sizes_base(h, names_base, size_anchors, sizes_offset=None):
    """The sizes array: an offset where the known (id, size) anchors all match.

    Fast path first: the historical offset (names_base + 3276748) still holds on
    some builds - try it and verify it with the anchors. Only if that fails,
    search for it.

    The search had a real bug worth remembering: a memory REGION can START
    before the names array and extend past it, so skipping every region whose
    BASE is below names_base throws away the region that holds the sizes
    array. Test for OVERLAP, not for base >= start.
    """
    def ok(cand):
        for cid, csz in size_anchors.items():
            v = read_i32(h, cand + cid * 4)
            if v is None or v != csz:
                return False
        return True

    if sizes_offset:
        hist = names_base + sizes_offset
        if ok(hist):
            return hist

    best = None
    for anchor_id, anchor_size in size_anchors.items():
        for base, size in committed_regions(h):
            # The sizes array sits after the names array (3.2 MB away on one
            # build), so only look in a window after it - but test OVERLAP.
            if base + size < names_base or base > names_base + 0x2000000:
                continue
            blob = rd(h, base, min(size, 0x1000000))
            if len(blob) < 0x1000:
                continue
            for off in range(0, len(blob) - 4, 4):
                if struct.unpack_from("<i", blob, off)[0] != anchor_size:
                    continue
                cand = base + off - anchor_id * 4
                if ok(cand) and (best is None or cand < best):
                    best = cand
    return best


def main():
    args = [a for a in sys.argv[1:]]
    out_path = None
    if "--out" in args:
        i = args.index("--out")
        out_path = args[i + 1]
        del args[i:i + 2]
    only_name = None
    if "--name" in args:
        i = args.index("--name")
        only_name = args[i + 1]
        del args[i:i + 2]

    profile_name = "v32"
    if "--profile" in args:
        i = args.index("--profile")
        profile_name = args[i + 1]
        del args[i:i + 2]
    if profile_name not in PROFILES:
        print("unknown profile %r - known: %s" % (profile_name, ", ".join(sorted(PROFILES))))
        return
    prof = PROFILES[profile_name]

    pid = pid_of()
    if not pid:
        print("ge.exe is not running - start the client first")
        return
    exe_path = process_path(pid) or "?"
    h = k32.OpenProcess(PQI | PVR, False, pid)
    if not h:
        print("cannot open ge.exe (pid %d)" % pid)
        return
    print("client: %s (pid %d)  profile: %s" % (exe_path, pid, profile_name))

    nb = find_names_base(h, prof["names"])
    if nb is None:
        print("cannot locate the packet-name table")
        return
    sb = find_sizes_base(h, nb, prof["sizes"], prof["sizes_offset"])
    print("names_base=%#x  sizes_base=%s" % (nb, ("%#x" % sb) if sb else "NOT FOUND"))
    if sb is None:
        print("! sizes array not found - sizes will be blank, names are still valid")
    print()

    lo = int(args[0]) if len(args) > 0 else 0
    hi = int(args[1]) if len(args) > 1 else 4000

    rows = []
    for cmd in range(lo, hi):
        raw = rd(h, nb + cmd * NAMES_STRIDE, NAMES_STRIDE)
        if len(raw) < NAMES_STRIDE:
            break
        name = raw.split(b"\x00")[0].decode("latin-1", "replace")
        if not name:
            continue
        if only_name and only_name not in name:
            continue
        sz = -1
        if sb is not None:
            b4 = rd(h, sb + cmd * 4, 4)
            if len(b4) == 4:
                sz = struct.unpack("<i", b4)[0]
        rows.append((cmd, name, sz))

    print("%-6s %-36s %s" % ("cmd", "name", "size"))
    for cmd, name, sz in rows:
        print("%-6d %-36s %s" % (cmd, name, ("%d" % sz) if sz >= 0 else "?"))

    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("# id\tsize\tname  (dumped from the live client, pid %d)\n" % pid)
            f.write("# client: %s\n# profile: %s\n" % (exe_path, profile_name))
            for cmd, name, sz in rows:
                f.write("%d\t%d\t%s\n" % (cmd, sz, name))
        print("\nwrote %s (%d rows)" % (out_path, len(rows)))


if __name__ == "__main__":
    main()
