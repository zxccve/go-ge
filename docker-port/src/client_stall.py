"""client_stall - measure how long the client takes to reach the barrack screen.

Usage:
    python client_stall.py [--ref <known barrack png>] [--out <file>]

It starts the client, clicks Connect, and then watches the window until the
barrack screen appears - and it says how long that took.

THE DETECTOR MATTERS (this cost an hour once):
a "the screen changed" test built on one region's brightness is NOT evidence.
The loading screen is dark in the same corner the barrack screen is, so the
naive test reports the LOADING screen as the barrack screen. The test here is
a WHOLE-IMAGE difference against a known-good screenshot of the barrack
screen, downscaled, mean absolute difference.

Calibrated on this install:
    barrack screen   ->   0
    login screen     ->  57
    loading screen   ->  56

If you do not have a reference screenshot, one is taken for you: the first
run saves the frame it found and tells you to look at it. LOOK at it before
you trust the number.

The window is grabbed from the desktop (the client renders with D3D, so
PrintWindow() gives a black frame).
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
GO_AT = (1280, 837)
SMALL = (320, 183)          # the comparison size
THRESHOLD = 30              # MAD below this = the reference screen


def mad(frame, ref):
    a = np.asarray(frame.convert("L").resize(SMALL), dtype=float)
    r = np.asarray(ref.convert("L").resize(SMALL), dtype=float)
    return float(np.abs(a - r).mean())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ref", default=os.path.join(SCREENSHOTS, "barrack_ref.png"),
                    help="known-good screenshot of the barrack screen")
    ap.add_argument("--out", default=os.path.join(LOGS, "stall_result.txt"))
    ap.add_argument("--max", type=int, default=180)
    args = ap.parse_args()

    ref = None
    if os.path.exists(args.ref):
        ref = Image.open(args.ref)
        print("reference: %s" % args.ref)
    else:
        print("NO reference yet (%s). I will save the first matching frame "
              "and you must LOOK at it." % args.ref)

    subprocess.Popen([EXE, "-SERVICE", "-USEMULTIPLEIPF", "-USELOADINGIMG"],
                     cwd=CLIENT_DIR)
    time.sleep(30)
    win = cc.client_window()
    if not win:
        print("FAIL: no ge.exe window")
        return 1
    hwnd, left, top, w, h = win
    print("window %dx%d at (%d,%d)" % (w, h, left, top))
    cc.click(left + PW_AT[0], top + PW_AT[1])
    cc.type_text("123456")
    t0 = time.time()
    # The click can miss if the client re-created its window while we were
    # typing, so click Connect until the server sees a login.
    log = os.path.join(LOGS, "barrack_server.log")
    t0 = time.time()
    for attempt in range(4):
        cc.click(left + GO_AT[0], top + GO_AT[1])
        time.sleep(4)
        seen = 0
        if os.path.exists(log):
            with open(log, encoding="utf-8", errors="replace") as f:
                seen = f.read().count("CONNECT from")
        if seen > 0:
            print("clicked Connect at t=0 (attempt %d, login seen)" % (attempt + 1))
            break
        print("  no login yet, clicking Connect again")
    else:
        print("FAIL: the client never logged in")
        return 1

    box = (left, top, left + w, top + h)
    lines = []
    hit = None
    for i in range(args.max):
        el = time.time() - t0
        frame = ImageGrab.grab(bbox=box)
        if ref is None:
            # Without a reference we cannot tell the barrack screen from the
            # loading screen. So we do not guess: we save the frame at 60 s
            # and the operator picks the reference by looking at it.
            if el >= 60:
                frame.save(args.ref)
                print("saved %s at %.0f s - LOOK at it, then re-run" % (args.ref, el))
                return 2
        else:
            m = mad(frame, ref)
            lines.append("%6.1fs  mad=%5.1f" % (el, m))
            if m < THRESHOLD:
                hit = el
                frame.save(os.path.join(SCREENSHOTS, "stall_reached.png"))
                break
        time.sleep(1)

    with open(args.out, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    if hit is None:
        print("NEVER reached the barrack screen in %d s" % args.max)
        return 1
    print("BARRACK SCREEN at %.1f s  (saved stall_reached.png - look at it)" % hit)
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
