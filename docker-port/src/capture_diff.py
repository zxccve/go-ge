"""Field map from the controlled captures (task 10).

Reads .\captures\cap_*.log in order and, in 8-byte blocks (the wire unit),
reports which blocks are constant and which change between runs.

Run:  python capture_diff.py
"""
import os
import re
import glob

from ge_paths import CAPTURES

HERE = os.path.dirname(os.path.abspath(__file__))
CAPS = CAPTURES


def load():
    out = []
    for path in sorted(glob.glob(os.path.join(CAPS, "cap_*.log"))):
        txt = open(path, encoding="utf-8", errors="ignore").read()
        m = re.search(r"hex=\[(.*?)\]", txt)
        if not m:
            continue
        data = bytes(int(x, 16) for x in m.group(1).split())
        out.append((os.path.basename(path), data))
    return out


def blocks(data, off=2):
    body = data[off:]
    return [body[i:i + 8] for i in range(0, len(body) - 7, 8)]


def main():
    caps = load()
    print("captures:", [n for n, _ in caps])
    if len(caps) < 2:
        print("need at least 2 captures")
        return
    lens = {len(d) for _, d in caps}
    print("lengths:", sorted(lens))
    n = min(len(blocks(d)) for _, d in caps)

    # constant across ALL runs
    const = [i for i in range(n) if len({blocks(d)[i] for _, d in caps}) == 1]
    vary = [i for i in range(n) if i not in const]
    print("block size 8, offset 2, blocks =", n)
    print("CONSTANT blocks (%d):" % len(const), const)
    print("VARIABLE blocks (%d):" % len(vary), vary)

    # pairwise diff between consecutive runs
    for a in range(len(caps) - 1):
        b = a + 1
        diff = [i for i in range(n) if blocks(caps[a][1])[i] != blocks(caps[b][1])[i]]
        print("run %s vs %s -> %d blocks differ: %s"
              % (caps[a][0], caps[b][0], len(diff), diff))


if __name__ == "__main__":
    main()
