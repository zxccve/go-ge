"""mem_probe.py - how much memory does our server's data actually cost?

Prints the process's working set after each step, so we can see what each
piece of the game data costs. Run from src\\:  python mem_probe.py
"""

import os
import sys


def mb():
    try:
        import psutil  # not installed; fall back to the OS
    except ImportError:
        psutil = None
    if psutil is not None:
        return psutil.Process().memory_info().rss / (1024.0 * 1024.0)
    # Windows: read the working set from the OS
    import ctypes
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

    c = PMC()
    c.cb = ctypes.sizeof(PMC)
    ctypes.windll.psapi.GetProcessMemoryInfo(
        ctypes.windll.kernel32.GetCurrentProcess(), ctypes.byref(c), c.cb)
    return c.WorkingSetSize / (1024.0 * 1024.0)


def step(label):
    print("  %-46s %7.1f MB" % (label, mb()))


def main():
    step("python interpreter")
    import ge_crypt
    step("+ ge_crypt (the cipher)")
    import ge_db
    step("+ ge_db")
    import stance_data
    step("+ stance_data")
    import skill_data
    step("+ skill_data")
    import world
    step("+ world (the world layer)")

    print()
    print("the heavy tables:")
    import npc_data
    step("+ npc_data imported")
    names = npc_data.load_map_table()
    step("+ datatable_map (%d rows)" % len(names))
    rows = npc_data.load_npc_gen("reb_basic")
    step("+ npcgen_reb_basic (%d rows)" % len(rows))
    try:
        import item_prices
        n = len(item_prices.build_cache())
        step("+ item price cache (%d items)" % n)
    except Exception as exc:
        print("  item_prices: %s" % exc)
    try:
        import ge_locale
        n = len(ge_locale.load())
        step("+ locale dictionary (%d entries)" % n)
    except Exception as exc:
        print("  ge_locale: %s" % exc)
    try:
        import ge_tables
        step("+ ge_tables")
    except Exception as exc:
        print("  ge_tables: %s" % exc)

    print()
    print("total working set: %.1f MB" % mb())
    return 0


if __name__ == "__main__":
    sys.exit(main())
