"""shot - grab the live ge.exe window to a PNG (evidence for the client UI).

Usage:  python shot.py [out.png] [--crop x y w h]

The GE client renders with D3D, so PrintWindow() gives a black frame. We instead
copy the pixels straight off the desktop for the window's rect (same trick used
for screenshots\barrack_shot.png). The window must be visible (not minimised); use
--minimize-others is not available - bring the client to the front first.
"""
import os
import sys
import ctypes
from ctypes import wintypes

import win32gui
import win32process
from PIL import ImageGrab

from ge_paths import SCREENSHOTS

u32 = ctypes.windll.user32


def windows_of(exe="ge.exe"):
    """[(hwnd, title, w, h)] for every visible top-level window of that exe."""
    out = []
    target = exe.lower()

    def cb(hwnd, _):
        if not win32gui.IsWindowVisible(hwnd):
            return True
        tid, pid = win32process.GetWindowThreadProcessId(hwnd)
        try:
            import psutil
            name = psutil.Process(pid).name().lower()
        except Exception:
            name = ""
        if name != target:
            return True
        l, t, r, b = win32gui.GetWindowRect(hwnd)
        out.append((hwnd, win32gui.GetWindowText(hwnd), r - l, b - t, (l, t, r, b)))
        return True

    win32gui.EnumWindows(cb, None)
    return out


def force_foreground(hwnd):
    """Bring a window to the front reliably (Windows blocks plain
    SetForegroundWindow for background processes)."""
    u32.ShowWindow(hwnd, 9)                      # SW_RESTORE
    if u32.IsIconic(hwnd):
        u32.ShowWindow(hwnd, 3)                  # SW_MAXIMIZE
    fg = u32.GetForegroundWindow()
    tid_fg = u32.GetWindowThreadProcessId(fg, None)
    tid_me = ctypes.windll.kernel32.GetCurrentThreadId()
    u32.AttachThreadInput(tid_me, tid_fg, True)
    u32.BringWindowToTop(hwnd)
    u32.SetForegroundWindow(hwnd)
    u32.AttachThreadInput(tid_me, tid_fg, False)


def main():
    args = [a for a in sys.argv[1:]]
    out = os.path.join(SCREENSHOTS, "shot.png")
    crop = None
    if args and not args[0].startswith("--"):
        out = args.pop(0)
    if "--crop" in args:
        i = args.index("--crop")
        crop = tuple(int(v) for v in args[i + 1:i + 5])

    wins = windows_of()
    wins = [w for w in wins if w[2] > 200 and w[3] > 200]
    if not wins:
        print("no visible ge.exe window found")
        return 1
    wins.sort(key=lambda w: w[2] * w[3], reverse=True)
    hwnd, title, w, h, (l, t, r, b) = wins[0]
    print("window hwnd=%s %dx%d at (%d,%d) iconic=%s title=%r"
          % (hwnd, w, h, l, t, u32.IsIconic(hwnd), title))

    force_foreground(hwnd)
    import time
    time.sleep(0.8)
    l, t, r, b = win32gui.GetWindowRect(hwnd)
    print("after restore: (%d,%d)-(%d,%d) foreground=%s"
          % (l, t, r, b, u32.GetForegroundWindow() == hwnd))
    img = ImageGrab.grab(bbox=(l, t, r, b), all_screens=True)
    if crop:
        x, y, cw, ch = crop
        img = img.crop((x, y, x + cw, y + ch))
    img.save(out)
    print("saved", out, img.size)
    return 0


if __name__ == "__main__":
    sys.exit(main())
