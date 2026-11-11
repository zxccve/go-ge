"""capture_live - capture a live game server WITHOUT touching the client.

Why this exists: a relay (point the client at 127.0.0.1) works, but it has
three drawbacks. It rewrites the server's address, and a rewrite that changes
the byte length silently desyncs the whole stream; it usually keeps only the
server->client direction; and it needs a free local port, so our own server
must stop.

Capturing at the network card with `pktmon` has none of those problems. The
client connects straight to the real server, nothing is rewritten, and BOTH
directions are captured. The client->server direction is encrypted with the
Blowfish cipher this project already implements, so it can be decrypted with
`ge_crypt.decrypt_packet` - which means the capture shows what the client
actually ASKED for, not just what it was told.

Usage:
    python capture_live.py start [--port 7001] [--port 7054] [--tag NAME]
    python capture_live.py stop  [--tag NAME]
    python capture_live.py list

Then: start the client, log in, play, and run `stop`.
`stop` writes the raw streams next to the .etl:
    <tag>_s2c.bin   server -> client (plaintext in this engine)
    <tag>_c2s.bin   client -> server (Blowfish-encrypted)
    <tag>_c2s.txt   the client->server stream, decrypted, one packet per line
    <tag>_s2c.txt   the server->client stream, hex, one packet per line

`capture.txt` puts the packet's own timestamp first on every line, so a
delay between a client packet and the server's answer can be measured. `stop`
also prints the first and last timestamp, and the wall time, of each leg.
"""
import datetime
import glob
import os
import tempfile
import struct
import subprocess
import sys

import pcap_extract

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(os.path.dirname(HERE), "..", "captures")
TEMP = os.environ.get("TEMP", tempfile.gettempdir())


def sh(cmd):
    r = subprocess.run(cmd, capture_output=True, text=True, shell=True)
    return (r.stdout or "") + (r.stderr or "")


def tag_path(tag, ext):
    return os.path.join(TEMP, "ge_capture_%s.%s" % (tag, ext))


def cmd_start(args):
    ports = []
    ips = []
    tag = "live"
    i = 0
    while i < len(args):
        if args[i] == "--port":
            ports.append(int(args[i + 1])); i += 2
        elif args[i] == "--ip":
            ips.append(args[i + 1]); i += 2
        elif args[i] == "--tag":
            tag = args[i + 1]; i += 2
        else:
            i += 1
    # An ADDRESS filter is the right one for the LIVE server: it runs one zone
    # process per map and each map's zone has its own port (7061 was measured
    # once, 7054 another time), so a fixed port list silently drops the zone
    # leg. `-i` catches every port of that host, both directions. Ports are
    # still accepted for a local relay.
    if not ports and not ips:
        ports = [7001, 7054]

    os.makedirs(OUT, exist_ok=True)
    etl = tag_path(tag, "etl")

    sh("pktmon stop")
    sh("pktmon filter remove")
    for p in ports:
        sh("pktmon filter add -p %d" % p)
    for ip in ips:
        sh("pktmon filter add -i %s" % ip)
    # --pkt-size 0 is NOT optional: the default logs only the first 128 bytes
    # of each packet, which silently truncates every payload past that.
    r = sh('pktmon start --capture --pkt-size 0 --file-name "%s" --comp nics' % etl)
    if "Collected Data" not in r:
        print("pktmon did not start:\n" + r)
        return 1
    what = []
    if ports:
        what.append("ports %s" % ports)
    if ips:
        what.append("address %s (all ports)" % ", ".join(ips))
    print("capturing %s -> %s" % (" and ".join(what), etl))
    print("Now start the client and log in. Run:  python capture_live.py stop --tag %s" % tag)
    return 0


def decrypt_stream(path, out_path):
    """Decrypt a client->server stream with the cipher we already have.

    The client frames each packet as [WORD blockByte][ciphertext], so the
    stream can be walked packet by packet. This is the payoff of capturing
    BOTH directions: we see what the client ASKED for, not just what it was
    told.
    """
    import ge_crypt
    data = open(path, "rb").read()
    out = []
    off = 0
    while off + 2 <= len(data):
        block = struct.unpack_from("<H", data, off)[0]
        if block == 0 or off + 2 + block > len(data):
            break
        try:
            plain = ge_crypt.decrypt_packet(data[off:off + 2 + block])
        except Exception:
            break
        out.append(plain)
        off += 2 + block
    with open(out_path, "wb") as f:
        f.write(b"".join(out))
    return len(out), off, len(data)


def fmt_duration(seconds):
    """A duration in words: 421.333 s (7m 01s)."""
    s = int(round(seconds))
    if s >= 3600:
        return "%.3f s (%dh %02dm %02ds)" % (seconds, s // 3600,
                                             s % 3600 // 60, s % 60)
    if s >= 60:
        return "%.3f s (%dm %02ds)" % (seconds, s // 60, s % 60)
    return "%.3f s" % seconds


def print_legs(timings):
    """Say when each leg started, when it ended, and how long it covered."""
    if not timings:
        print("\ncapture time: no leg carried a timestamp")
        return
    print("\ncapture time per leg (epoch seconds, taken from the packets):")
    firsts = []
    lasts = []
    for (name, sport, dport), (first, last, n) in sorted(timings.items()):
        if first <= 0.0 and last <= 0.0:
            print("  %-4s %5d -> %-5d  no timestamp on any of its %d segments"
                  % (name, sport, dport, n))
            continue
        firsts.append(first)
        lasts.append(last)
        print("  %-4s %5d -> %-5d  first %.3f  last %.3f  wall %s  (%d segments)"
              % (name, sport, dport, first, last, fmt_duration(last - first), n))
    if firsts:
        print("  whole capture: first %.3f  last %.3f  wall %s"
              % (min(firsts), max(lasts), fmt_duration(max(lasts) - min(firsts))))


def cmd_stop(args):
    tag = "live"
    ports = []
    i = 0
    while i < len(args):
        if args[i] == "--tag":
            tag = args[i + 1]; i += 2
        elif args[i] == "--port":
            # The live zone process listens on a DIFFERENT port every day (the
            # server list assigns it), so `stop` has to take the same ports the
            # START used - a hardcoded pair silently drops the zone leg.
            ports.append(int(args[i + 1])); i += 2
        else:
            i += 1
    if not ports:
        ports = [7001, 7054]
    etl = tag_path(tag, "etl")
    if not os.path.exists(etl):
        print("no capture file: %s" % etl)
        return 1
    print(sh("pktmon stop"))
    pcap = tag_path(tag, "pcapng")
    sh('pktmon etl2pcap "%s" --out "%s"' % (etl, pcap))

    outdir = os.path.join(OUT, tag)
    os.makedirs(outdir, exist_ok=True)
    # Split by direction. The client->server side is the one whose SOURCE
    # port is an ephemeral local port; the server side listens on the ports the
    # capture was started with.
    timings = {}
    for port in ports:
        if not os.path.exists(pcap):
            continue
        try:
            pcap_extract.extract(pcap, outdir, server_ports=tuple(ports),
                                timings=timings)
        except Exception as exc:                       # keep going, report later
            print("extract failed for port %d: %r" % (port, exc))

    print_legs(timings)

    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    final = os.path.join(outdir, stamp)
    os.makedirs(final, exist_ok=True)
    for f in glob.glob(os.path.join(outdir, "*.bin")) + glob.glob(os.path.join(outdir, "*.txt")):
        os.replace(f, os.path.join(final, os.path.basename(f)))
    print("\nwrote: %s" % final)
    for f in sorted(os.listdir(final)):
        print("  %-20s %d bytes" % (f, os.path.getsize(os.path.join(final, f))))
    return 0


def cmd_list(args):
    for f in sorted(glob.glob(os.path.join(OUT, "*"))):
        if os.path.isdir(f):
            print(f)
            for g in sorted(glob.glob(os.path.join(f, "*"))):
                print("   %s  %d bytes" % (os.path.basename(g), os.path.getsize(g)))
    return 0


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return
    cmd = sys.argv[1]
    args = sys.argv[2:]
    if cmd == "start":
        return cmd_start(args)
    if cmd == "stop":
        return cmd_stop(args)
    if cmd == "list":
        return cmd_list(args)
    print(__doc__)
    return 1


if __name__ == "__main__":
    sys.exit(main() or 0)
