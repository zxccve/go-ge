"""analyze_click_log - one line per NPC click in the zone server log.

Usage:
    python analyze_click_log.py [--log PATH] [--handle 0x40000031] [--last N]

Answers the question that keeps coming back: "which NPC clicks actually work?"
It groups the zone log into CLICK CYCLES - from a `CZ_CLICK_TRIGGER` to the next
one - and prints, per cycle:

    time  handle  chases  first<N>  min<N>  outcome

  * `chases`  - how many `chasing <handle> -> (x, y, cell)` packets we sent.
  * `first`   - the FIRST distance to the NPC the log reported ("closing in on
                ... (N units)"), which is roughly how far away the click was.
  * `min`     - the smallest distance seen in the cycle.
  * `outcome` - DIALOG (we sent ZC_DIALOG_OK), GAVEUP (the stall clock dropped
                it), or OPEN (the cycle never finished in the log).

A cycle that ends OPEN with a chase into a DIFFERENT cell than the player's is
the signature of "the client cannot path to the destination we made up": the
ZC_MOVE_PC animates nothing, no client walk report follows, and the actor never
moves. Compare the failing handles' cells with the ones that work.

Keep it cheap: the log is megabytes, so this reads line by line and prints at
most `--last` cycles (default 40).
"""
import argparse
import re
from ge_paths import GAME_ROOT

LOG = os.path.join(GAME_ROOT, "docker-port", "logs", "zone_server.log")

CLICK = re.compile(r"^\[(\d\d:\d\d:\d\d)\]\s+CZ_CLICK_TRIGGER .*handle=0x([0-9a-f]+)")
CHASE = re.compile(r"chasing 0x([0-9a-f]+) -> \((-?\d+), (-?\d+), cell=(-?\d+)\)")
DIALOG = re.compile(r"SENT ZC_DIALOG_OK handle=0x([0-9a-f]+)")
GAVE = re.compile(r"gave up click|never reported")
CLOSE = re.compile(r"closing in on 0x([0-9a-f]+) \((\d+) units\)")
MOVE = re.compile(r"CZ_MOVE_PC char=\d+ pos=\((-?\d+), (-?\d+), cell=(-?\d+)\)")


def cycles(path, want_handle=None):
    out = []
    cur = None
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            m = CLICK.match(line)
            if m:
                cur = {"t": m.group(1), "h": m.group(2), "chases": [],
                       "first": None, "min": None, "dialog": False,
                       "gave": False, "moves": []}
                out.append(cur)
                continue
            if cur is None:
                continue
            m = CHASE.search(line)
            if m and m.group(1) == cur["h"]:
                cur["chases"].append((int(m.group(2)), int(m.group(3)),
                                      int(m.group(4))))
            m = CLOSE.search(line)
            if m and m.group(1) == cur["h"]:
                d = int(m.group(2))
                if cur["first"] is None:
                    cur["first"] = d
                cur["min"] = d if cur["min"] is None else min(cur["min"], d)
            m = DIALOG.search(line)
            if m and m.group(1) == cur["h"]:
                cur["dialog"] = True
            if GAVE.search(line):
                cur["gave"] = True
            m = MOVE.search(line)
            if m:
                cur["moves"].append((int(m.group(1)), int(m.group(2)),
                                     int(m.group(3))))
    if want_handle:
        out = [c for c in out if c["h"] == want_handle]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--log", default=LOG)
    ap.add_argument("--handle", default=None,
                    help="only this handle, e.g. 0x40000031")
    ap.add_argument("--last", type=int, default=40)
    args = ap.parse_args()
    cs = cycles(args.log, args.handle)
    for c in cs[-args.last:]:
        outcome = ("DIALOG" if c["dialog"] else
                   "GAVEUP" if c["gave"] else "OPEN")
        cells = ",".join(str(x[2]) for x in c["chases"]) or "-"
        print("%s 0x%s chases=%d cells=%s first=%s min=%s %s"
              % (c["t"], c["h"], len(c["chases"]), cells, c["first"],
                 c["min"], outcome))
    print("%d click cycle(s) in total" % len(cs))


if __name__ == "__main__":
    main()
