"""Reassemble the standard Blowfish P+S from the source's scrambled dummy table.

CryptTable.cpp: bf_dummy_table + partNum={16,2,256,768}, seek={4,1056,24,284}.
Concatenating src[seek:seek+num] for each part yields the standard P(18)+S(4x256).
"""
import re
import struct
from ge_paths import GAME_ROOT

SRC = os.path.join(GAME_ROOT, "source2", "geShared", "Source", "CryptTable.cpp")
M = 0xFFFFFFFF


def reassemble(path):
    txt = open(path, encoding="utf-8", errors="ignore").read()
    body = txt.split("bf_dummy_table", 1)[1].split("};", 1)[0]
    nums = [int(x, 16) for x in re.findall(r"0x([0-9A-Fa-f]{8})L?", body)]
    part_num = [int(x) for x in re.search(r"tablePartNum\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    seek = [int(x) for x in re.search(r"tableSeekPointer\[NUM_PART\]\s*=\s*\{([^}]*)\}", txt).group(1).split(",")]
    tbl = []
    for n, s in zip(part_num, seek):
        tbl += nums[s:s + n]
    assert len(tbl) == 1042, len(tbl)
    P = tbl[:18]
    S = [tbl[18 + k * 256:18 + (k + 1) * 256] for k in range(4)]
    return P, S


def main():
    P, S = reassemble(SRC)
    print("P =", " ".join("%08X" % x for x in P))
    print("S0[0:4] =", " ".join("%08X" % x for x in S[0][:4]))
    std_p = [0x243F6A88, 0x85A308D3, 0x13198A2E, 0x03707344, 0xA4093822,
             0x299F31D0, 0x082EFA98, 0xEC4E6C89, 0x452821E6, 0x38D01377,
             0xBE5466CF, 0x34E90C6C, 0xC0AC29B7, 0xC97C50DD, 0x3F84D5B5,
             0xB5470917, 0x9216D5D9, 0x8979FB1B]
    print("P == standard pi tables:", P == std_p)
    print("S0 start standard:", S[0][:4] == [0xD1310BA6, 0x98DFB5AC, 0x2FFD72DB, 0xD01ADFB7])

    import bf2
    print("self-test with these tables:")
    print("vectors pass:", bf2.self_test(P, S))


if __name__ == "__main__":
    main()
