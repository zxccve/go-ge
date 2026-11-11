"""pe_find - find an ASCII string in a PE file and print its virtual address.

Ghidra looks at VIRTUAL addresses, but a hex editor / python search gives FILE
offsets. This maps one to the other through the section table, so we can take a
string we see on disk straight to an xref query in Ghidra.

Usage:  python pe_find.py <pe> <needle> [max]
"""
import struct
import sys


def sections(data):
    e_lfanew = struct.unpack_from("<I", data, 0x3C)[0]
    nsec = struct.unpack_from("<H", data, e_lfanew + 6)[0]
    opt_size = struct.unpack_from("<H", data, e_lfanew + 20)[0]
    opt = e_lfanew + 24
    image_base = struct.unpack_from("<I", data, opt + 28)[0]
    base = opt + opt_size
    out = []
    for i in range(nsec):
        s = base + i * 40
        name = data[s:s + 8].rstrip(b"\0").decode("latin-1")
        vsize, vaddr, rawsize, rawptr = struct.unpack_from("<IIII", data, s + 8)
        out.append((name, vaddr, vsize, rawptr, rawsize))
    return image_base, out


def main():
    path, needle = sys.argv[1], sys.argv[2].encode("latin-1")
    limit = int(sys.argv[3]) if len(sys.argv) > 3 else 20
    with open(path, "rb") as f:
        data = f.read()
    image_base, secs = sections(data)
    print("image_base=%#x  %d sections" % (image_base, len(secs)))
    i, n = 0, 0
    while n < limit:
        i = data.find(needle, i)
        if i < 0:
            break
        va = None
        for name, vaddr, vsize, rawptr, rawsize in secs:
            if rawptr <= i < rawptr + rawsize:
                va = image_base + vaddr + (i - rawptr)
                sec = name
                break
        print("file %#08x  va %#010x  (%s)" % (i, va or 0, sec if va else "?"))
        i += 1
        n += 1
    if n == 0:
        print("not found")


if __name__ == "__main__":
    main()
