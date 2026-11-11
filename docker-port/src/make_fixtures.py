"""make_fixtures.py - turn a capture into a test vector for the Go server.

The rule for this project: Python probes, Go serves. So a behaviour found by a
probe does not get copied into Go as hand-written logic - it gets written down
as a FIXTURE, and the Go tests are held against that fixture.

Two fixture kinds, because the protocol has two framing layers:

    wire    the bytes on the socket: [u16 length][that many ciphertext bytes]
            -> testdata\\<name>.wire.txt   (tests protocol.SplitWire)

    stream  the bytes AFTER decryption: command(2) + body
            -> testdata\\<name>.stream.txt (tests protocol.Table.Split)

Both are written as one item per line with hex, so they diff and review well.
The decryption and the packet splitting are NOT re-implemented here: they come
from `ge_crypt` and `test_golden`, the code the probes already trust.

Usage (from src\\):
    python make_fixtures.py                        # wire + stream from the
                                                   # default barrack capture
    python make_fixtures.py wire  <log> [name]
    python make_fixtures.py stream <log> [name]
    python make_fixtures.py --list [log]           # what is in the capture

A capture that cannot be split cleanly is REFUSED rather than written: a
fixture must never encode a guess, or the Go test starts passing on nonsense.
"""
import os
import re
import sys

import ge_crypt as gc
import ge_paths
from test_golden import load_table, split_packets

DEFAULT_LOG = os.path.join(ge_paths.ROOT, "captures", "barrack_stub.log")
OUT_DIR = os.path.join(ge_paths.ROOT, "goserver", "protocol", "testdata")

# `[2026-10-15 00:42:30] GOT len=818 hex=[30 03 ...] txt=[...]`
LINE = re.compile(r"GOT len=(\d+) hex=\[([0-9A-Fa-f ]+)\]")


def reads(path):
    """Yield (stamp, wire bytes) for every logged read in a stub capture."""
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            m = LINE.search(line)
            if not m:
                continue
            raw = bytes(int(b, 16) for b in m.group(2).split())
            if len(raw) != int(m.group(1)):
                continue
            stamp = line[1:line.index("]")] if line.startswith("[") else ""
            yield stamp, raw


def chunks(buf):
    """Cut the wire into transport chunks: [u16 length][length bytes]."""
    out = []
    i = 0
    while i + 2 <= len(buf):
        n = int.from_bytes(buf[i:i + 2], "little")
        if i + 2 + n > len(buf):
            break
        out.append(buf[i:i + 2 + n])
        i += 2 + n
    return out, i


def describe(path):
    """Print one line per captured read: does it frame, and into what?"""
    sizes = load_table()
    for n, (stamp, raw) in enumerate(reads(path)):
        cs, used = chunks(raw)
        note = ""
        if used != len(raw):
            note = "  LEFT OVER %d bytes" % (len(raw) - used)
        else:
            odd = [len(c) - 2 for c in cs if (len(c) - 2) % 8]
            if odd:
                note = "  cipher lengths not a multiple of 8: %s" % odd
        cmds = []
        for c in cs:
            try:
                plain = gc.decrypt_packet(c)
                cmd = int.from_bytes(plain[:2], "little") if len(plain) >= 2 else -1
                cmds.append(cmd)
            except Exception:
                cmds.append("?")
        print("%2d %s len=%4d chunks=%d cmds=%s%s"
              % (n, stamp, len(raw), len(cs), " ".join(str(c) for c in cmds), note))


def _write(name, suffix, header, lines):
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, name + suffix)
    with open(out, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(header + [""] + lines) + "\n")
    print("wrote %s (%d line(s))" % (out, len(lines)))
    return out


def pick_read(path):
    """Return (stamp, chunks, raw) for the first read that cuts cleanly.

    A capture holds more than protocol traffic (an old probe logged the plain
    text "HELLO-TEST-LOGIN", for example), so the generator takes a read that
    frames perfectly instead of concatenating everything and hoping.
    """
    for stamp, raw in reads(path):
        cs, used = chunks(raw)
        if not cs or used != len(raw):
            continue
        if any((len(c) - 2) % 8 for c in cs):
            continue                      # cipher must be whole 8-byte blocks
        return stamp, cs, raw
    return None, None, None


def make_wire(path, name):
    """Fixture of the raw socket bytes: one transport chunk per line."""
    stamp, cs, raw = pick_read(path)
    if not cs:
        print("REFUSED: no read in %s cuts cleanly into whole chunks" % path)
        return None

    lines = ["%d\t%s" % (len(c) - 2, c.hex()) for c in cs]
    header = [
        "# Fixture: %s (transport layer)" % name,
        "# Source: %s  read at %s  (%d bytes on the socket)" % (path, stamp, len(raw)),
        "# %d chunk(s), cipher is whole 8-byte blocks."
        % len(cs),
        "# Each line: cipherLen<TAB>hex of the whole chunk (length word included).",
        "# Generated by src\\make_fixtures.py - regenerate, do not hand-edit.",
    ]
    return _write(name, ".wire.txt", header, lines)


def make_stream(path, name):
    """Fixture of the decrypted packets: one packet per line."""
    sizes = load_table()
    stamp, cs, raw = pick_read(path)
    if not cs:
        print("REFUSED: no read in %s cuts cleanly into whole chunks" % path)
        return None

    plain = b""
    for c in cs:
        plain += gc.decrypt_packet(c)

    frames = split_packets(plain, sizes)
    got = sum(len(d) for _, d in frames)
    if got != len(plain):
        print("REFUSED: %d of %d decrypted bytes are not a packet the client's "
              "table can size - fix the table before making a fixture"
              % (len(plain) - got, len(plain)))
        return None
    unknown = [c for c, _ in frames if c not in sizes]
    if unknown:
        print("REFUSED: commands %s are not in data\\cmd_table.txt (that table "
              "is the ZONE id space; small barrack ids are not in it)" % unknown)
        return None

    lines = ["%d\t%d\t%s" % (cmd, len(data), data.hex()) for cmd, data in frames]
    header = [
        "# Fixture: %s (packet layer)" % name,
        "# Source: %s  read at %s" % (path, stamp),
        "# %d chunk(s) decrypted to %d bytes = %d packet(s)."
        % (len(cs), len(plain), len(frames)),
        "# Each line: cmd<TAB>size<TAB>hex of the whole packet, in stream order.",
        "# Generated by src\\make_fixtures.py - regenerate, do not hand-edit.",
    ]
    return _write(name, ".stream.txt", header, lines)


def make_plain(path, name):
    """Fixture of the DECRYPTED bytes of each chunk, one line per chunk.

    This is what the Go cipher is held against: no packet parsing, no table,
    just "the same ciphertext must decrypt to the same bytes Python gets".
    """
    stamp, cs, raw = pick_read(path)
    if not cs:
        print("REFUSED: no read in %s cuts cleanly into whole chunks" % path)
        return None

    plains = [gc.decrypt_packet(c) for c in cs]
    lines = [p.hex() for p in plains]
    header = [
        "# Fixture: %s (decrypted bytes)" % name,
        "# Source: %s  read at %s  (%d bytes on the socket)" % (path, stamp, len(raw)),
        "# One line per transport chunk, in order: hex of the plaintext.",
        "# Plaintext is PADDED to a multiple of 8 by the client, so trailing",
        "# zeros are normal and are part of the expected bytes.",
        "# Generated by src\\make_fixtures.py - regenerate, do not hand-edit.",
    ]
    return _write(name, ".plain.txt", header, lines)


def main(argv):
    if len(argv) > 1 and argv[1] == "--list":
        describe(argv[2] if len(argv) > 2 else DEFAULT_LOG)
        return 0

    kind = argv[1] if len(argv) > 1 else "all"
    path = argv[2] if len(argv) > 2 else DEFAULT_LOG
    name = argv[3] if len(argv) > 3 else os.path.splitext(os.path.basename(path))[0]

    if kind == "wire":
        return 0 if make_wire(path, name) else 1
    if kind == "plain":
        return 0 if make_plain(path, name) else 1
    if kind == "stream":
        return 0 if make_stream(path, name) else 1
    if kind != "all":
        print(__doc__)
        return 2
    ok = make_wire(path, name)
    make_plain(path, name)
    # The barrack id space is NOT in data\cmd_table.txt (that table is the
    # zone's), so a stream fixture from a barrack capture is expected to be
    # refused. It is not a failure: run `--list` to see why.
    make_stream(path, name)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
