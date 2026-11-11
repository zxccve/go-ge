"""Find who calls the imported `send` in ge.exe - static, no debugger.

Parses the PE import table to get the IAT slot for ws2_32.send (and friends),
then scans the code for `call dword ptr [slot]` (FF 15 xx xx xx xx) and
`jmp dword ptr [slot]` (FF 25 ...). Prints the call-site virtual addresses;
the nearest function start is the packet builder.

Run:  python find_send_callers.py
"""
import struct
import sys
from ge_paths import GAME_ROOT

CLIENT = os.path.join(GAME_ROOT, "GEAndromida", "release", "ge.exe")
# ge.exe imports ws2_32 BY ORDINAL, so map the ones we care about.
WINSOCK_ORD = {19: "send", 20: "sendto", 4: "connect", 112: "WSASend",
               113: "WSASendTo", 16: "recv", 110: "WSARecv", 23: "socket",
               3: "closesocket", 115: "WSAStartup", 18: "select",
               12: "ioctlsocket", 9: "htons", 10: "inet_addr",
               11: "inet_ntoa", 111: "WSAGetLastError"}
WANT = set(WINSOCK_ORD.values())


def parse(path):
    data = open(path, "rb").read()
    e = struct.unpack_from("<I", data, 0x3C)[0]
    assert data[e:e + 4] == b"PE\0\0", "not a PE"
    nsec = struct.unpack_from("<H", data, e + 6)[0]
    optsz = struct.unpack_from("<H", data, e + 20)[0]
    opt = e + 24
    magic = struct.unpack_from("<H", data, opt)[0]
    base = struct.unpack_from("<I", data, opt + 28)[0]
    dd = opt + (96 if magic == 0x10B else 112)
    imp_rva = struct.unpack_from("<I", data, dd + 8)[0]
    sec = opt + optsz
    sections = []
    for i in range(nsec):
        o = sec + i * 40
        nm = data[o:o + 8].rstrip(b"\0").decode(errors="ignore")
        vs, va, rs, rp = struct.unpack_from("<IIII", data, o + 8)
        ch = struct.unpack_from("<I", data, o + 36)[0]
        sections.append((nm, va, vs, rp, rs, ch))

    def rva2off(rva):
        for _nm, va, vs, rp, rs, _ch in sections:
            if va <= rva < va + max(vs, rs):
                return rp + (rva - va)
        return None

    # imports
    slots = {}
    o = rva2off(imp_rva)
    while True:
        oft, ts, fc, name_rva, first_thunk = struct.unpack_from("<IIIII", data, o)
        if name_rva == 0 and first_thunk == 0:
            break
        dll_off = rva2off(name_rva)
        dll = data[dll_off:data.find(b"\0", dll_off)].decode(errors="ignore")
        if dll.lower() == "ws2_32.dll":
            thunk_rva = oft or first_thunk
            idx = 0
            to = rva2off(thunk_rva)
            while True:
                val = struct.unpack_from("<I", data, to + idx * 4)[0]
                if val == 0:
                    break
                if not (val & 0x80000000):
                    noff = rva2off(val)
                    nm = data[noff + 2:data.find(b"\0", noff + 2)].decode(errors="ignore")
                else:
                    nm = WINSOCK_ORD.get(val & 0xFFFF, "")
                if nm in WANT:
                    slot_va = base + first_thunk + idx * 4
                    slots[nm] = slot_va
                idx += 1
        o += 20
    return data, base, sections, slots


def main():
    data, base, sections, slots = parse(CLIENT)
    print("image base 0x%08X" % base)
    for nm, va in slots.items():
        print("IAT slot %-8s -> 0x%08X" % (nm, va))

    code = [s for s in sections if s[5] & 0x20000000]  # IMAGE_SCN_MEM_EXECUTE
    for nm, va in slots.items():
        target = struct.pack("<I", va)
        pat_call = b"\xFF\x15" + target
        pat_jmp = b"\xFF\x25" + target
        hits = []
        for _snm, sva, _vs, srp, srs, _ch in code:
            blob = data[srp:srp + srs]
            for pat, kind in ((pat_call, "call"), (pat_jmp, "jmp")):
                i = blob.find(pat)
                while i >= 0:
                    hits.append((sva + i, kind))
                    i = blob.find(pat, i + 1)
        print("\n%s -> %d direct reference(s)" % (nm, len(hits)))
        for addr, kind in sorted(hits):
            print("   %s at 0x%08X" % (kind, addr))


if __name__ == "__main__":
    main()
