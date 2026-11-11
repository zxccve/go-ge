"""mem_maps.py - what does ONE more map cost our zone server?

Loads map after map the way the zone does (npcgen rows + town points) and
prints the working set after each, so the per-map cost is a measured number
instead of a guess.

Run from src\\:  python mem_maps.py [how many maps]
"""

import ctypes
import sys
from ctypes import wintypes


class PMC(ctypes.Structure):
    _fields_ = [("cb", wintypes.DWORD),
                ("PageFaultCount", wintypes.DWORD),
                ("PeakWorkingSetSize", ctypes.c_size_t),
                ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
                ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t),
                ("PeakPagefileUsage", ctypes.c_size_t)]


def mb():
    """Working set in MB. psutil if we have it, else the Windows API."""
    try:
        import psutil
        return psutil.Process().memory_info().rss / (1024.0 * 1024.0)
    except ImportError:
        pass
    k32 = ctypes.windll.kernel32
    fn = getattr(k32, "K32GetProcessMemoryInfo", None)
    if fn is None:
        fn = ctypes.windll.psapi.GetProcessMemoryInfo
    fn.restype = wintypes.BOOL
    fn.argtypes = [wintypes.HANDLE, ctypes.POINTER(PMC), wintypes.DWORD]
    c = PMC()
    c.cb = ctypes.sizeof(PMC)
    if not fn(k32.GetCurrentProcess(), ctypes.byref(c), c.cb):
        return float("nan")
    return c.WorkingSetSize / (1024.0 * 1024.0)


def main():
    how_many = int(sys.argv[1]) if len(sys.argv) > 1 else 8
    import npc_data
    maps = npc_data.load_map_table()
    # The biggest worlds first, so the number is a worst case.
    names = sorted(maps.keys())
    print("working set before any map: %.1f MB" % mb())

    base = None
    for i, zid in enumerate(names[:how_many]):
        world_name = maps[zid][0]
        try:
            rows = npc_data.load_npc_gen(world_name)
        except Exception as exc:
            print("  map %-5s %-24s SKIP (%s)" % (zid, world_name, exc))
            continue
        try:
            npc_data.load_town_points(world_name)
        except Exception:
            pass
        now = mb()
        if base is None:
            base = now
        print("  map %-5s %-24s npc=%-5d  now %6.1f MB  (+%.2f MB this map)"
              % (zid, world_name, len(rows), now, now - base))
        base = now
    print("total working set: %.1f MB" % mb())
    return 0


if __name__ == "__main__":
    sys.exit(main())
