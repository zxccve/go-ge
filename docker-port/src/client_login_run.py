"""client_login_run - start the game client, log in, and report what happened.

Usage:
    python client_login_run.py [--wait 25] [--pass 123456] [--no-start]

It does the whole login dance with the REAL mouse (the client renders with
D3D and ignores synthetic window messages), then reports:

  * whether ge.exe is still alive after the wait (it crashes on a bad packet),
  * the last lines of the barrack log, with timestamps - those are the packet
    timeline, which is the real evidence.

The click coordinates are window-relative, measured off a screenshot of the
client window (the screenshot IS the window rect). They are for the 2566x1469
window of this install; --pw-at / --go-at override them.
"""
import argparse
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import click_client as cc  # noqa: E402
from ge_paths import LOGS, GAME_ROOT  # noqa: E402, 

CLIENT_DIR = os.path.join(GAME_ROOT, "GEAndromida", "release")
EXE = os.path.join(CLIENT_DIR, "ge.exe")
BARRACK_LOG = os.path.join(LOGS, "barrack_server.log")

# Window-relative pixels on the 2566x1469 client window.
PW_AT = (1283, 715)
GO_AT = (1280, 837)


def log_lines(n=14):
    if not os.path.exists(BARRACK_LOG):
        return []
    with open(BARRACK_LOG, encoding="utf-8", errors="replace") as f:
        return f.read().splitlines()[-n:]


def alive():
    out = subprocess.run(["tasklist", "/FI", "IMAGENAME eq ge.exe"],
                         capture_output=True, text=True).stdout
    return "ge.exe" in out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--wait", type=int, default=25,
                    help="seconds to wait after clicking Connect")
    ap.add_argument("--pwd", default="123456")
    ap.add_argument("--no-start", action="store_true",
                    help="the client is already running")
    ap.add_argument("--pw-at", default="%d,%d" % PW_AT)
    ap.add_argument("--go-at", default="%d,%d" % GO_AT)
    args = ap.parse_args()

    before = len(log_lines(10000))
    if not args.no_start:
        subprocess.Popen([EXE, "-SERVICE", "-USEMULTIPLEIPF", "-USELOADINGIMG"],
                         cwd=CLIENT_DIR)
        time.sleep(12)

    win = cc.client_window()
    if not win:
        print("FAIL: no ge.exe window")
        return 1
    hwnd, left, top, w, h = win
    print("window %dx%d at (%d,%d)" % (w, h, left, top))
    if (w, h) != (2566, 1469):
        print("WARN: this install's window is %dx%d; the click points were "
              "measured on 2566x1469" % (w, h))

    pw = tuple(int(x) for x in args.pw_at.split(","))
    go = tuple(int(x) for x in args.go_at.split(","))
    cc.click(left + pw[0], top + pw[1])
    cc.type_text(args.pwd)
    print("clicked Connect at %s" % (go,))
    cc.click(left + go[0], top + go[1])

    for i in range(args.wait):
        time.sleep(1)
        if not alive():
            print("client DIED after ~%d s" % (i + 1))
            break
    else:
        print("client still alive after %d s" % args.wait)

    print("--- barrack log ---")
    for line in log_lines(16):
        print("   " + line)
    return 0


if __name__ == "__main__":
    sys.exit(main() or 0)
