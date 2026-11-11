"""pcap_extract - pull TCP payload streams out of a .pcapng that `pktmon` made.

Why: `pktmon` captures at the network card, so it sees the traffic WITHOUT
touching the client or the server. That is strictly better than a relay for
three reasons:

  1. the client connects straight to the real server - no address rewriting,
     no chance of the rewrite corrupting a packet,
  2. BOTH directions are captured. A relay usually keeps only the
     server->client direction; here the client->server direction comes too,
     and it is Blowfish-encrypted, so it can be decrypted with `ge_crypt`
     (which knows the key),
  3. it needs no free local port, so our own server can keep running.

`pktmon` puts its own metadata header in front of every packet inside the
pcapng packet data, and the length of that header VARIES between packets.
So the Ethernet header cannot be found at a fixed offset - it has to be
located by validating the IPv4 header that follows it.

Every segment keeps the packet's OWN timestamp (from the pcapng Enhanced
Packet Block, scaled by the interface's if_tsresol), so a delay between a
client packet and the server's answer can be measured afterwards. A block
that carries no timestamp reports 0.0 - the wall clock is never used to fill
it in, because a guessed time would invent a delay that never happened.

Usage:
    python pcap_extract.py <capture.pcapng> [--port 7001] [--out-dir DIR]
    python pcap_extract.py --selftest

Writes one file per direction:
    <dir>/s2c.bin   server -> client (plaintext in this engine)
    <dir>/c2s.bin   client -> server (Blowfish-encrypted)
    <dir>/capture.txt   a human-readable log of every TCP segment
"""
import os
import struct
import sys


def _if_tsresol(data, body, blen, endian):
    """Return the interface's if_tsresol (option 9) as a raw byte, else 6.

    6 means the timestamp counts microseconds, which is the pcapng default.
    """
    p = body + 8                       # after linktype(2) reserved(2) snaplen(4)
    end = body + blen - 12             # before the trailing block length
    while p + 4 <= end:
        code, olen = struct.unpack_from(endian + "HH", data, p)
        if code == 0:                  # opt_endofopt
            break
        if code == 9 and olen >= 1:
            return data[p + 4]
        p += 4 + ((olen + 3) // 4) * 4
    return 6


def _ts_unit(resol):
    """Seconds per timestamp tick from a raw if_tsresol byte."""
    if resol & 0x80:                   # high bit = power of two
        return 2.0 ** -(resol & 0x7F)
    return 10.0 ** -resol


def parse_pcapng(data):
    """Yield (packet_bytes, timestamp_seconds) for every captured packet.

    timestamp_seconds is seconds since the epoch, decoded from the block's own
    ts_high/ts_low and the interface's if_tsresol. A block with no timestamp
    yields 0.0 - never the wall clock, because the whole point is to measure a
    delay and a guessed time would invent one.
    """
    off = 0
    endian = "<"
    tsresol = []                       # if_tsresol per interface, in IDB order
    while off + 12 <= len(data):
        btype, blen = struct.unpack_from(endian + "II", data, off)
        if btype == 0x0A0D0D0A:        # Section Header Block: new section
            magic = struct.unpack_from("<I", data, off + 8)[0]
            endian = "<" if magic == 0x1A2B3C4D else ">"
            btype, blen = struct.unpack_from(endian + "II", data, off)
            tsresol = []
        if blen < 12 or off + blen > len(data):
            break
        body = off + 8
        if btype == 1:                 # Interface Description Block
            tsresol.append(_if_tsresol(data, body, blen, endian))
        elif btype == 6:               # Enhanced Packet Block
            if blen >= 32:
                iface, th, tl, caplen = struct.unpack_from(endian + "IIII",
                                                           data, body)
                raw_ts = (th << 32) | tl
                if raw_ts:
                    unit = tsresol[iface] if iface < len(tsresol) else 6
                    ts = raw_ts * _ts_unit(unit)
                else:
                    ts = 0.0           # the block carries no timestamp
                yield data[body + 20:body + 20 + caplen], ts
        off += blen


def read_pcapng(path):
    """Yield (packet_data, timestamp_seconds) for every packet in the file."""
    with open(path, "rb") as f:
        data = f.read()
    for item in parse_pcapng(data):
        yield item


def find_ipv4(buf):
    """Return (ip_start, ip_len) for the IPv4 header in a pcapng packet blob.

    The pktmon metadata header length varies, so scan for a valid IPv4 header
    instead of assuming an offset.
    """
    for i in range(0, min(len(buf), 64)):
        if buf[i] >> 4 != 4:
            continue
        ihl = (buf[i] & 0x0F) * 4
        if ihl < 20 or i + ihl > len(buf):
            continue
        total = struct.unpack_from(">H", buf, i + 2)[0]
        if total < ihl or i + total > len(buf):
            continue
        if buf[i + 9] not in (6, 17):        # TCP or UDP
            continue
        return i, total
    return None, None


def segments(path):
    """Yield (src_ip, src_port, dst_ip, dst_port, payload, seq, ts) per segment.

    `ts` is the packet's own capture time in seconds since the epoch, or 0.0
    when the block carried no timestamp.
    """
    for raw, ts in read_pcapng(path):
        ip, total = find_ipv4(raw)
        if ip is None:
            continue
        if raw[ip + 9] != 6:                  # TCP only
            continue
        ihl = (raw[ip] & 0x0F) * 4
        tcp = ip + ihl
        src = ".".join(str(b) for b in raw[ip + 12:ip + 16])
        dst = ".".join(str(b) for b in raw[ip + 16:ip + 20])
        sport, dport = struct.unpack_from(">HH", raw, tcp)
        doff = (raw[tcp + 12] >> 4) * 4
        if doff < 20:
            continue
        payload = raw[tcp + doff:ip + total]
        seq = struct.unpack_from(">I", raw, tcp + 4)[0]
        yield src, sport, dst, dport, payload, seq, ts


def reassemble(segs):
    """Join TCP segments into a byte stream, in sequence order.

    Sorts by sequence number and drops duplicates (a retransmit), so the
    result is the true stream the application saw.
    """
    if not segs:
        return b""
    # segs entries are (src, sport, dst, dport, payload, seq, ts).
    # The 7th field (the packet timestamp) is not used here.
    # The base must be the lowest seq that CARRIES PAYLOAD: the SYN's
    # sequence number is one lower than the first data byte, and using it
    # shifts the whole stream by one byte.
    withdata = [s for s in segs if s[4]]
    if not withdata:
        return b""
    base = min(s[5] for s in withdata)
    out = bytearray()
    for seg in sorted(segs, key=lambda x: x[5]):
        payload = seg[4]
        if not payload:
            continue
        off = (seg[5] - base) % (1 << 32)
        if off > (1 << 31):                    # wrapped / before base
            off -= (1 << 32)
        if off < 0:
            continue
        if off + len(payload) <= len(out):
            continue                           # already have it
        if off > len(out):
            out.extend(b"\x00" * (off - len(out)))   # gap: pad, flag later
        out[off:off + len(payload)] = payload
    return bytes(out)


def extract(pcapng_path, out_dir, server_ports=(7001, 7054), timings=None):
    """Split a pktmon capture into per-direction streams. Returns a dict.

    If `timings` is a dict, each leg is added to it as
    (name, sport, dport) -> (first_ts, last_ts, segments), so the caller can
    say how long the capture covered. Timestamps of 0.0 (no timestamp in the
    block) are ignored when picking the first and last.
    """
    os.makedirs(out_dir, exist_ok=True)
    groups = {}
    log = []
    for src, sport, dst, dport, payload, seq, ts in segments(pcapng_path):
        key = (src, sport, dst, dport)
        groups.setdefault(key, []).append((src, sport, dst, dport, payload, seq, ts))
        if payload:
            log.append("%.3f  %s:%d -> %s:%d  seq=%-10d len=%-6d %s"
                       % (ts, src, sport, dst, dport, seq, len(payload),
                          payload[:24].hex(" ")))

    written = {}
    for key, segs in sorted(groups.items()):
        data = reassemble(segs)
        src, sport, dst, dport = key
        # The SERVER is the side listening on one of the server ports.
        # src -> server = client -> server (c2s, usually encrypted)
        # server -> src = server -> client (s2c, plaintext in this engine)
        if dport in server_ports:
            name = "c2s"
        elif sport in server_ports:
            name = "s2c"
        else:
            name = "c2s" if sport > dport else "s2c"
        # One file PER CONNECTION: a session opens several connections (barrack
        # plus zone, and the client may reconnect), and writing them all to
        # the same name would keep only the last one.
        fn = os.path.join(out_dir, "%s_%d_%d.bin" % (name, sport, dport))
        with open(fn, "wb") as f:
            f.write(data)
        written[(name, sport, dport)] = fn
        if timings is not None:
            stamps = [s[6] for s in segs if s[6] > 0.0]
            timings[(name, sport, dport)] = (min(stamps) if stamps else 0.0,
                                             max(stamps) if stamps else 0.0,
                                             len(segs))
        print("  %s:%d -> %s:%d   %d segments, %d bytes -> %s"
              % (src, sport, dst, dport, len(segs), len(data), os.path.basename(fn)))

    with open(os.path.join(out_dir, "capture.txt"), "w", encoding="utf-8") as f:
        f.write("\n".join(log))
    return written


def _blk(btype, body):
    """Wrap a block body the way pcapng does: a length field at both ends."""
    body += b"\x00" * (-len(body) % 4)
    total = 12 + len(body)
    return struct.pack("<II", btype, total) + body + struct.pack("<I", total)


def _idb(tsresol):
    """An Interface Description Block carrying only if_tsresol (option 9)."""
    opts = struct.pack("<HHB3x", 9, 1, tsresol) + struct.pack("<HH", 0, 0)
    return _blk(1, struct.pack("<HHI", 1, 0, 0) + opts)


def _epb(iface, raw_ts, packet):
    """An Enhanced Packet Block. raw_ts is the 64-bit tick count."""
    head = struct.pack("<IIIII", iface, raw_ts >> 32, raw_ts & 0xFFFFFFFF,
                       len(packet), len(packet))
    return _blk(6, head + packet)


def selftest():
    """Decode synthetic pcapng bytes and check the timestamp maths.

    Run:  python pcap_extract.py --selftest
    No capture file and no game are needed.
    """
    us = 1793700000123456                  # microseconds -> ...000.123456 s
    ns = 1793700000123456789               # nanoseconds  -> ...000.123456789 s
    data = (_blk(0x0A0D0D0A, struct.pack("<IHHq", 0x1A2B3C4D, 1, 0, -1))
            + _idb(6)                      # interface 0: microseconds
            + _idb(9)                      # interface 1: nanoseconds
            + _epb(0, us, b"\x00" * 8)
            + _epb(1, ns, b"\x00" * 8)
            + _epb(0, 1 << 32, b"\x00" * 8)   # high word alone: 4294.967296 s
            + _idb(6)                      # interface 2: microseconds
            + _epb(2, 0, b"\x00" * 8)      # carries no timestamp
            + _blk(6, b"\x00" * 4))        # truncated block: skipped
    got = [ts for _, ts in parse_pcapng(data)]
    checks = [
        ("packets decoded", len(got), 4),
        ("tsresol 6 (microseconds)", got[0], 1793700000.123456),
        ("tsresol 9 (nanoseconds)", got[1], 1793700000.123456789),
        ("ts_high honoured", got[2], 4294.967296),
        ("no timestamp -> 0.0", got[3], 0.0),
    ]
    ok = True
    for name, value, want in checks:
        if isinstance(want, float):
            good = abs(value - want) < 1e-6
        else:
            good = value == want
        ok = ok and good
        print("%-4s %-26s got %r  want %r"
              % ("PASS" if good else "FAIL", name, value, want))
    print("selftest: %s" % ("PASS" if ok else "FAIL"))
    return ok


def main():
    args = sys.argv[1:]
    if args and args[0] == "--selftest":
        return 0 if selftest() else 1
    if not args:
        print(__doc__)
        return
    path = args[0]
    port = None
    out_dir = None
    i = 1
    while i < len(args):
        if args[i] == "--port":
            port = int(args[i + 1]); i += 2
        elif args[i] == "--out-dir":
            out_dir = args[i + 1]; i += 2
        else:
            i += 1
    out_dir = out_dir or os.path.dirname(os.path.abspath(path))
    os.makedirs(out_dir, exist_ok=True)

    # Group by (direction) using the port that is not the local one.
    groups = {}
    log = []
    for src, sport, dst, dport, payload, seq, ts in segments(path):
        if port is not None and sport != port and dport != port:
            continue
        key = (src, sport, dst, dport)
        groups.setdefault(key, []).append((src, sport, dst, dport, payload, seq, ts))
        if payload:
            log.append("%.3f  %s:%d -> %s:%d  seq=%-10d len=%-6d %s"
                       % (ts, src, sport, dst, dport, seq, len(payload),
                          payload[:24].hex(" ")))

    print("capture: %s" % path)
    print("streams found: %d" % len(groups))
    for key, segs in sorted(groups.items()):
        data = reassemble(segs)
        src, sport, dst, dport = key
        # The SERVER is the side listening on the filtered port.
        # src -> server  = client -> server (c2s, usually encrypted)
        # server -> src  = server -> client (s2c, plaintext in this engine)
        if port is not None:
            name = "c2s" if dport == port else "s2c"
        else:
            name = "c2s" if sport > dport else "s2c"
        fn = os.path.join(out_dir, "%s.bin" % name)
        with open(fn, "wb") as f:
            f.write(data)
        print("  %s:%d -> %s:%d   %d segments, %d bytes -> %s"
              % (src, sport, dst, dport, len(segs), len(data), fn))

    with open(os.path.join(out_dir, "capture.txt"), "w", encoding="utf-8") as f:
        f.write("\n".join(log))
    print("log: %s" % os.path.join(out_dir, "capture.txt"))


if __name__ == "__main__":
    sys.exit(main() or 0)
