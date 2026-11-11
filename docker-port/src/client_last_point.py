"""client_last_point - log in, press "Last Point", report what the server did.

Usage:
    python client_last_point.py [--wait 20]

It drives the REAL client: starts it, logs in, waits for the barrack screen,
clicks the "Last Point" button, and then prints what the barrack and the
zone said about it.

This is the end-to-end check of the "continue" feature - the one the user
looks at. The chain underneath it (save -> read back -> handoff -> spawn) is
checked by `test_continue_position.py`; this one proves the CLIENT asks for
it and the server answers.

The button position is measured off a screenshot of the barrack screen, in
window-relative pixels, on the 2566x1469 window of this install.
"""
import argparse
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
from PIL import Image, ImageGrab

import click_client as cc
from ge_paths import LOGS, SCREENSHOTS, GAME_ROOT

CLIENT_DIR = os.path.join(GAME_ROOT, "GEAndromida", "release")
EXE = os.path.join(CLIENT_DIR, "ge.exe")
PW_AT = (1283, 715)
GO_AT = (1287, 835)
LAST_POINT_AT = (970, 1396)      # the "Last Point" button (measured off the
                                  # barrack screenshot; y=1372 is too high)
REF = os.path.join(SCREENSHOTS, "barrack_ref.png")
SMALL = (320, 183)


def log_tail(path, n=6):
    if not os.path.exists(path):
        return []
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read().splitlines()[-n:]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wait", type=int, default=20)
    args = ap.parse_args()

    barrack_log = os.path.join(LOGS, "barrack_server.log")
    zone_log = os.path.join(LOGS, "zone_server.log")

    def logins():
        """How many logins the barrack log has seen so far."""
        if not os.path.exists(barrack_log):
            return 0
        with open(barrack_log, encoding="utf-8", errors="replace") as f:
            return f.read().count("CONNECT from")

    before_b = logins()

    subprocess.Popen([EXE, "-SERVICE", "-USEMULTIPLEIPF", "-USELOADINGIMG"],
                     cwd=CLIENT_DIR)
    time.sleep(30)
    win = cc.client_window()
    if not win:
        print("FAIL: no ge.exe window")
        return 1
    hwnd, left, top, w, h = win
    ref = np.asarray(Image.open(REF).convert("L").resize(SMALL), dtype=float)
    cc.click(left + PW_AT[0], top + PW_AT[1])
    cc.type_text("123456")
    t0 = time.time()
    for attempt in range(4):
        cc.click(left + GO_AT[0], top + GO_AT[1])
        time.sleep(4)
        if logins() > before_b:
            print("logged in (attempt %d)" % (attempt + 1))
            break
    else:
        print("FAIL: never logged in")
        return 1

    # Wait for the barrack screen.
    for _ in range(40):
        frame = ImageGrab.grab(bbox=(left, top, left + w, top + h))
        a = np.asarray(frame.convert("L").resize(SMALL), dtype=float)
        if np.abs(a - ref).mean() < 30:
            print("barrack screen after %.1f s" % (time.time() - t0))
            break
        time.sleep(1)
    else:
        print("FAIL: the barrack screen never appeared")
        return 1

    print("clicking 'Last Point'")
    cc.click(left + LAST_POINT_AT[0], top + LAST_POINT_AT[1])
    time.sleep(args.wait)

    print("--- barrack log ---")
    for line in log_tail(barrack_log, 14):
        print("   " + line)
    print("--- zone log (continue) ---")
    with open(zone_log, encoding="utf-8", errors="replace") as f:
        lines = f.read().splitlines()
    for line in lines[-400:]:
        if "continue" in line.lower() or "spawn" in line.lower():
            print("   " + line)
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
