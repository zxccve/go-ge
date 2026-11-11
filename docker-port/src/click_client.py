"""click_client - click inside the ge.exe client window.

Usage:
    python click_client.py <x> <y>            # window-relative pixels
    python click_client.py <x> <y> --abs      # screen pixels
    python click_client.py --type "123456"    # type text into the focused field
    python click_client.py --find "Connect"   # click a button by its text?

The GE client renders with D3D, so you cannot send it a normal window message
for a click. You have to move the real mouse and click for real - the same
reason `shot.py` copies the desktop pixels instead of using PrintWindow().

The window is brought to the front first, so the click lands on the client and
not on whatever was in front of it.

Window-relative coordinates are measured from the CLIENT AREA, i.e. the same
image you get from `python shot.py out.png` - the screenshot is the window
rect, so an x/y you read off the screenshot is what you pass here.
"""
import os
import sys
import time

import ctypes

import win32api
import win32con
import win32gui
import win32process

WM_LBUTTONDOWN = 0x0201
WM_LBUTTONUP = 0x0202
u32 = ctypes.windll.user32


def client_window(exe="ge.exe"):
    """(hwnd, left, top, width, height) of the first visible ge.exe window."""
    found = []
    target = exe.lower()

    def cb(hwnd, _):
        if not win32gui.IsWindowVisible(hwnd):
            return True
        _, pid = win32process.GetWindowThreadProcessId(hwnd)
        try:
            import psutil
            name = psutil.Process(pid).name().lower()
        except Exception:
            name = ""
        if name != target:
            return True
        l, t, r, b = win32gui.GetWindowRect(hwnd)
        found.append((hwnd, l, t, r - l, b - t))
        return True

    win32gui.EnumWindows(cb, None)
    if not found:
        return None
    hwnd, left, top, w, h = found[0]
    # Bring it to the front properly. Windows blocks a plain SetForegroundWindow
    # from a background process, so borrow the foreground thread's input queue.
    u32.ShowWindow(hwnd, win32con.SW_RESTORE)
    fg = u32.GetForegroundWindow()
    tid_fg = u32.GetWindowThreadProcessId(fg, None)
    tid_me = ctypes.windll.kernel32.GetCurrentThreadId()
    u32.AttachThreadInput(tid_me, tid_fg, True)
    u32.BringWindowToTop(hwnd)
    u32.SetForegroundWindow(hwnd)
    u32.AttachThreadInput(tid_me, tid_fg, False)
    time.sleep(0.5)
    return hwnd, left, top, w, h


def move(x, y):
    win32api.SetCursorPos((int(x), int(y)))
    time.sleep(0.1)


def click(x, y):
    move(x, y)
    win32api.mouse_event(win32con.MOUSEEVENTF_LEFTDOWN, 0, 0, 0, 0)
    time.sleep(0.06)
    win32api.mouse_event(win32con.MOUSEEVENTF_LEFTUP, 0, 0, 0, 0)
    time.sleep(0.2)


def type_text(text):
    """Type text with real key events (the game ignores synthetic messages)."""
    import ctypes
    VK = {"8": 0x38, "1": 0x31, "2": 0x32, "3": 0x33, "4": 0x34,
          "5": 0x35, "6": 0x36, "7": 0x37, "9": 0x39, "0": 0x30}
    for ch in text:
        if ch.isdigit():
            vk = VK[ch]
            win32api.keybd_event(vk, 0, 0, 0)
            win32api.keybd_event(vk, 0, win32con.KEYEVENTF_KEYUP, 0)
        elif ch.isalpha():
            vk = win32api.VkKeyScan(ch) & 0xFF
            win32api.keybd_event(vk, 0, 0, 0)
            win32api.keybd_event(vk, 0, win32con.KEYEVENTF_KEYUP, 0)
        time.sleep(0.04)


def main(argv):
    win = client_window()
    if not win:
        print("no visible ge.exe window")
        return 1
    hwnd, left, top, w, h = win
    print("window hwnd=%d %dx%d at (%d,%d)" % (hwnd, w, h, left, top))

    if argv and argv[0] == "--type":
        type_text(" ".join(argv[1:]))
        print("typed %r" % " ".join(argv[1:]))
        return 0

    if len(argv) < 2:
        print(__doc__)
        return 1
    x, y = int(argv[0]), int(argv[1])
    if "--abs" not in argv:
        x, y = left + x, top + y
    print("click at screen (%d,%d)" % (x, y))
    click(x, y)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]) or 0)
