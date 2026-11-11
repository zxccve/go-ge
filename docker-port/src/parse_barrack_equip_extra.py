"""parse_barrack_equip_extra - work out the layout of cmd 17's sections.

Usage:  python parse_barrack_equip_extra.py <the captured packet .bin>

It parses the live server's BC_BARRACK_EQUIP_INFO_EXTRA and prints the
section boundaries it finds, so the same parser can be used on OUR packet.

The layout (from the client's own handler FUN_0052195d):
    cmd(2) len(2) nOptions(4) nSockets(4) nProperties(4)
    nOptions   x 16 bytes : (DWORD optionType, DWORD optionValue,
                             DWORD charSlot, DWORD equipSpot)
    nSockets   x 12 bytes : (DWORD a, DWORD b, DWORD c)
    nProperties x (DWORD invIndex, BYTE exist, [CObject if exist])
        CObject = DWORD classCount, DWORD propertyCount,
                  per class: NUL-terminated idspace + DWORD ClassID,
                  per property: BYTE keyLen, key[keyLen], BYTE type, value
                  (type 0 = NUMBER = 8-byte double, 1/2 = string with a
                  BYTE length that includes the NUL)
"""
import struct
import sys


def parse_cobject(buf, p):
    cls, props = struct.unpack_from("<ii", buf, p)
    p += 8
    for _ in range(cls):
        p = buf.index(b"\x00", p) + 1
        p += 4
    for _ in range(props):
        kl = buf[p]
        p += 1 + kl
        t = buf[p]
        p += 1
        if t == 0:
            p += 8
        else:
            vl = buf[p]
            p += 1 + vl
    return p


def parse(buf, start, hdr=9, n_max=1000):
    """Walk the property records from `start`.

    The record header is **9 bytes**: the client's reader (FUN_0067dfdd) does
    `Read(this, 4)`, then `ReadInto(this, 4)`, then reads ONE byte - the
    "exist" flag - and only then the CObject, if exist is 1.

    Returns (records, end_offset, error). A parse error stops the walk but
    keeps what was read - that is how the section boundary is found.
    """
    p = start
    out = []
    err = None
    while p < len(buf) and len(out) < n_max:
        try:
            if hdr == 9:
                a = struct.unpack_from("<I", buf, p)[0]
                b = struct.unpack_from("<I", buf, p + 4)[0]
                ex = buf[p + 8]
                p += 9
            else:
                a = struct.unpack_from("<I", buf, p)[0]
                b = None
                ex = buf[p + 4]
                p += 5
            obj = None
            if ex:
                obj = p
                p = parse_cobject(buf, p)
            out.append((a, b, ex, obj))
        except (IndexError, ValueError, struct.error) as exc:
            err = "%s at %d" % (exc, p)
            break
    return out, p, err


def main():
    path = sys.argv[1]
    with open(path, "rb") as f:
        d = f.read()
    n_opt, n_sock, n_prop = struct.unpack_from("<III", d, 4)
    print("total %d  nOptions=%d nSockets=%d nProperties=%d"
          % (len(d), n_opt, n_sock, n_prop))
    opt_end = 16 + n_opt * 16
    sock_end = opt_end + n_sock * 12
    print("options  %d..%d" % (16, opt_end))
    print("sockets  %d..%d" % (opt_end, sock_end))
    # The property section starts somewhere after the sockets. Try every
    # offset and keep the one that parses exactly nProperties records and
    # ends on the last byte of the packet.
    for hdr in (9, 5):
        for start in range(sock_end, sock_end + 80):
            recs, end, err = parse(d, start, hdr)
            if len(recs) == n_prop and end == len(d):
                print("PROPERTY SECTION: header=%d bytes, starts at %d, "
                      "%d records, ends at %d" % (hdr, start, len(recs), end))
                for r in recs[:4]:
                    print("   a=%s b=%s exist=%s obj@%s" % r)
                return 0
    # Nothing lines up: report the best attempt.
    best = (0, None, None, None)
    for hdr in (9, 5):
        for start in range(sock_end, sock_end + 80):
            recs, end, err = parse(d, start, hdr)
            if len(recs) > best[0]:
                best = (len(recs), (start, end), err, hdr)
    print("no exact fit. best: %d records, hdr %s, start %s end %s, err %s "
          "(packet %d, wanted %d records ending at %d)"
          % (best[0], best[3], best[1][0] if best[1] else None,
             best[1][1] if best[1] else None, best[2], len(d), n_prop, len(d)))
    return 1


if __name__ == "__main__":
    sys.exit(main() or 0)
