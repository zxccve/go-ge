import tempfile
"""Set-B core (FUN_00417e60, the unlocker) executed from its own decompiled
text (ghidra_dec.txt lines 49-380). Strict parser: unknown shapes raise.
Run: python coreB.py  (roundtrip test + hello decrypt attempt)
"""
import struct
import pickle
import pathlib
import re

M = 0xFFFFFFFF
HERE = pathlib.Path(__file__).parent
T = pickle.load(open(HERE / "ctables.pkl", "rb"))
# set-B tables are byte-identical to set-A (verified); map by role:
S1a, S1b, S1c, S1d = T["S1a"], T["S1b"], T["S1c"], T["S1d"]
S2a, S2b = T["S2a"], T["S2b"]


def ROL(x, r):
    r &= 31
    return ((x << r) | (x >> ((32 - r) & 31))) & M


def ROR(x, r):
    r &= 31
    return ((x >> r) | (x << ((32 - r) & 31))) & M


def S1(x):
    return (S1a[(x >> 16) & 0xFF] ^ S1b[(x >> 8) & 0xFF] ^
            S1c[(x >> 24) & 0xFF] ^ S1d[x & 0xFF]) & M


def S2(x):
    t = (S2a[(x >> 16) & 0xFF] ^ ((S2b[(x >> 24) & 0xFF] << 8) & M)) & M
    t = ((t << 8) & M) ^ S2b[(x >> 8) & 0xFF]
    t = ((t << 8) & M) ^ S2a[x & 0xFF]
    return t & M


def load_statements(path=None, lo=None, hi=None):
    if path is None:
        path = (os.path.join(tempfile.gettempdir(), "opencode", "ghidra_dec.txt"))
        lo, hi = (48, 380) if lo is None else (lo, hi)
    lines = open(path, encoding="utf-8", errors="replace").read().splitlines()
    # file lines 49..380 -> idx 48..379
    raw = lines[lo:hi]
    stmts, cur = [], ""
    for l in raw:
        s = l.strip()
        if not s or s in ("{", "}", "return;"):
            continue
        if re.match(r"^(byte|uint|int|void|undefined|char|ulong|ushort|"
                     r"long|short|bool|float|double|/|=====|DONE)", s):
            continue
        cur += " " + s
        if ";" in s:
            stmts.append(cur.strip())
            cur = ""
    assert not cur.strip(), "unbalanced: " + cur
    return stmts


RX_BSET = re.compile(r"^(bVar\d+) = \(byte\)(uVar\d+) & 0x1f;$")
RX_OUT = re.compile(r"^(?:\*param_2|param_2\[(\d+)\]) = (.*);$")
RX_ASN = re.compile(r"^(uVar\d+) = (.*);$")
RX_ROL = re.compile(
    r"\(([^()]+)\) >> \(0x20 - (bVar\d+) & 0x1f\) \| \(([^()]+)\) << \2")
RX_ROR = re.compile(
    r"\(([^()]+)\) << \(0x20 - (bVar\d+) & 0x1f\) \| \(([^()]+)\) >> \2")
RX_ROLv = re.compile(
    r"(uVar\d+) >> \(0x20 - (bVar\d+) & 0x1f\) \| \1 << \2")
RX_RORv = re.compile(
    r"(uVar\d+) << \(0x20 - (bVar\d+) & 0x1f\) \| \1 >> \2")


def run_core(w, K, stmts=None):
    if stmts is None:
        stmts = load_statements()
    V = {}
    B = {}
    outs = {}
    V["uVar4"], V["uVar5"], V["uVar6"], V["uVar7"] = None, None, None, None
    # inputs per head of set-B core (lines 49/58/68/78-79):
    # uVar4 = K[0x44] + in0 ; uVar6 = (K[0x45] ^ in1) + S.. ; etc.
    # => the parser executes everything literally; seed inputs below.
    W = list(w)
    for si, st in enumerate(stmts):
        if "DAT_" in st:
            # S-layer application: find target + single arg var
            m = re.match(r"^(uVar\d+) = ", st)
            assert m, "S-stmt no target: " + st[:80]
            tgt = m.group(1)
            # S1 = pure xor ladder (no shifts); S2 = shift ladder
            picks16 = re.findall(r"(uVar\d+) >> 0x10 & 0xff", st)
            picks8 = re.findall(r"(uVar\d+) >> (?:0x)?8 & 0xff", st)
            picks24 = re.findall(r"(uVar\d+) >> 0x18(?: & 0xff)?", st)
            picks0 = re.findall(r"\((uVar\d+) & 0xff\)", st)
            allp = picks16 + picks8 + picks24 + picks0
            assert len(allp) == 4 and len(set(allp)) == 1, \
                "S-stmt#%d picks %s: %s" % (si, allp, st[:150])
            arg = allp[0]
            is_s2 = ("<<" in st)
            V[tgt] = (S1(V[arg]) if not is_s2 else S2(V[arg]))
            continue
        m = RX_BSET.match(st)
        if m:
            B[m.group(1)] = V[m.group(2)] & 31
            continue
        m = RX_OUT.match(st)
        if m:
            idx = 0 if m.group(1) is None else int(m.group(1))
            outs[idx] = eval_expr(m.group(2), V, B, K, W)
            continue
        m = RX_ASN.match(st)
        if m:
            V[m.group(1)] = eval_expr(m.group(2), V, B, K, W)
            continue
        raise AssertionError("UNKNOWN shape: " + st[:120])
    assert set(outs) == {0, 1, 2, 3}, outs.keys()
    return (outs[0], outs[1], outs[2], outs[3])


def sub_rot(rx, fn, e2):
    def rep(m):
        if m.group(1) != m.group(3):
            raise AssertionError("rotation sides differ: %s vs %s" % (m.group(1), m.group(3)))
        return "%s(%s,B['%s'])" % (fn, m.group(1), m.group(2))
    return rx.sub(rep, e2)


def eval_expr(e, V, B, K, W):
    e2 = e.replace("(uint)", "")  # no-op under mod 2^32
    e2 = sub_rot(RX_ROL, "ROL", e2)
    e2 = sub_rot(RX_ROR, "ROR", e2)
    e2 = RX_ROLv.sub(lambda m: "ROL(%s,B['%s'])" % (m.group(1), m.group(2)), e2)
    e2 = RX_RORv.sub(lambda m: "ROR(%s,B['%s'])" % (m.group(1), m.group(2)), e2)
    assert "<<" not in e2 and ">>" not in e2, "leftover shift: " + e2[:120]
    e2 = re.sub(r"param_3\[(0x[0-9a-f]+|\d+)\]",
                lambda m: "K[%d]" % int(m.group(1), 0), e2)
    e2 = re.sub(r"\*param_1\b", "W[0]", e2)
    e2 = re.sub(r"\*param_3\b", "K[0]", e2)
    e2 = re.sub(r"param_1\[(\d+)\]", r"W[\1]", e2)
    e2 = re.sub(r"\b(uVar\d+)\b", r"V['\1']", e2)
    e2 = re.sub(r"(?<![\w'])(bVar\d+)(?![\w'])", r"B['\1']", e2)
    assert re.fullmatch(r"[KVWBROL\d\[\]'+\-^ *(),a-z]+", e2), "bad chars: " + e2[:100]
    try:
        return eval(e2, {"__builtins__": {}, "ROL": ROL, "ROR": ROR},
                    {"K": K, "V": V, "B": B, "W": W}) & M
    except Exception:
        raise AssertionError("eval fail: " + e2[:200])


if __name__ == "__main__":
    import custom128 as C
    K = C.schedule(C.KEY)
    print("subkeys ok:", len(K) == 72)
    stmtsB = load_statements()
    stmtsA = load_statements(
        os.path.join(tempfile.gettempdir(), "opencode", "ghidra_bffull.txt"), 22, 322)
    print("stmts B/A:", len(stmtsB), len(stmtsA))
    import random
    random.seed(1)
    ok = True
    for _ in range(3):
        x = tuple(random.getrandbits(32) for _ in range(4))
        hand = C.core(x, K)
        par = run_core(x, K, stmtsA)
        print("hand==parsedA:", hand == par)
        dec = run_core(par, K, stmtsB)
        print("parsedB(parsedA(x))==x:", dec == x)
        ok &= (dec == x)
    print("ALL:", ok)
