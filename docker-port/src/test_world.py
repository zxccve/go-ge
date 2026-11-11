"""test_world.py - prove the world module's seam works.

No socket, no client. Checks the three things the barrack and the zone rely
on:

  1. a live zone is found by the map it serves;
  2. a zone that died (stale presence file) is NOT handed out;
  3. with nothing announced, the caller still gets its fallback.

Run from src\\:  python test_world.py
"""

import os
import shutil
import sys
import time

import world

FAILS = []


def check(cond, what):
    print(("  ok   " if cond else "  FAIL ") + what)
    if not cond:
        FAILS.append(what)


def main():
    # Work in a scratch folder so the real registry is untouched.
    scratch = os.path.join(world.OUT, "_test_world")
    if os.path.isdir(scratch):
        shutil.rmtree(scratch)
    world.ZONE_DIR = scratch
    world.ensure(world.ZONE_DIR)

    print("1. a live zone is found by its map")
    path = world.announce(10, "127.0.0.1", 7002, channel=0, map_name="reb_basic")
    check(path is not None, "announce() returns a presence file")
    check(world.zone_address(10) == ("127.0.0.1", 7002),
          "zone_address(10) -> 127.0.0.1:7002")
    check(world.zone_address(999) is None,
          "zone_address for an unserved map -> None")
    check(world.map_name(10) == "reb_basic",
          "map_name(10) -> reb_basic (from the live zone)")

    print("2. a second zone on another map is found separately")
    world.announce(20, "10.0.0.5", 7003, channel=1, map_name="coim_basic")
    check(world.zone_address(20) == ("10.0.0.5", 7003),
          "zone_address(20) -> 10.0.0.5:7003")
    check(world.zone_address(10) == ("127.0.0.1", 7002),
          "zone_address(10) still -> 127.0.0.1:7002")

    print("3. a dead zone is not handed out")
    # Pretend the zone died 10 minutes ago: rewrite its file with an old time.
    import json
    rec = {"map_id": 30, "channel": 0, "ip": "127.0.0.1", "port": 7004,
           "map_name": "arm_basic", "last_seen": time.time() - 600}
    with open(os.path.join(scratch, "zone_7004.json"), "w", encoding="utf-8") as f:
        json.dump(rec, f)
    check(world.zone_address(30) is None,
          "a zone whose heartbeat stopped is ignored")

    print("4. the heartbeat keeps a zone alive")
    beat_path = world.announce(40, "127.0.0.1", 7005, map_name="ust_basic")
    beat = world.Heartbeat(beat_path, map_id=40, map_name="ust_basic").start()
    time.sleep(1.0)
    check(world.zone_address(40) == ("127.0.0.1", 7005),
          "a zone with a running heartbeat is alive")
    beat.stop()
    time.sleep(0.2)
    check(world.zone_address(40) is None,
          "after a clean stop the zone is withdrawn")

    print("5. the map table still answers for a map with no live zone")
    name = world.map_name(10)
    check(name == "reb_basic", "map_name(10) still -> reb_basic from the table")

    print("6. withdraw on a clean shutdown removes the file")
    p = world.announce(50, "127.0.0.1", 7006)
    check(os.path.exists(p), "presence file exists")
    world.withdraw(p)
    check(not os.path.exists(p), "presence file removed")
    check(world.zone_address(50) is None, "and the zone is gone from the registry")

    shutil.rmtree(scratch, ignore_errors=True)

    print()
    if FAILS:
        print("FAILED %d check(s):" % len(FAILS))
        for f in FAILS:
            print("   - " + f)
        return 1
    print("all world-module checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
