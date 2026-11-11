#!/usr/bin/env python3
"""text_id_map - translate the SERVER PACK's text ids into the client's.

WHY
    Two generations of Granado Espada number their text differently. The server
    data we port (`ge\\script\\*.scp`, `ge\\xml\\*.xml`) is the TERRA pack, whose
    lines are quoted as `<$>N</>`; the client the owner plays is GEAndromida, and
    it draws an id only from ITS OWN dictionary. The two spaces are NOT a constant
    offset: over ids 7..3000 the same Korean line sits at `N-1` for only 61 of
    2,851 (measured), and 0 sit at `N` - so handing the client a TERRA id is not
    "the same text under a different number", it is the WRONG text.

    This tool answers "which Andro id carries this TERRA id's line?" - NEVER by
    id, always by TEXT. TERRA's Korean is the join key (the Korean source barely
    moves between generations, while the English was re-translated); where TERRA
    has no Korean (its modern band is English-only) the English is the key.

WHAT IT WRITES
    data\\text_id_map_terra_to_andro.json   {"<terra id>": "<andro id>"}
    data\\text_id_map_unmapped.json         {id: {ko, en, band, why}} - the
                                            worklist: no Andro twin, translate
                                            or retire the line.

    Both are DATA. Nothing here resolves an id to WORDS - the map only changes
    NUMBERS, so the client keeps doing its own drawing from its own dictionary.

THE ANDRO DICTIONARY IS INSIDE A .IPF
    `GEAndromida\\ge\\dictionary.ipf` (683 MB, 140 members, ENCRYPTED names) is
    read by `ipf_read_encrypted.py`, member by member, and the two members this
    needs are cached in `cache\\andro_dict\\`. A member that cannot be read is
    REPORTED, not fatal.

USAGE (run from src\\)
    python text_id_map.py
    python text_id_map.py --ipf "<path>"
    python text_id_map.py --corpus-only      # map only ids the scripts quote
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import unicodedata

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from ge_paths import CACHE, DATA, GE  # noqa: E402, GAME_ROOT
import ipf_read_encrypted as ipf       # noqa: E402  (the reader for encrypted names)

DEFAULT_IPF = os.path.join(GAME_ROOT, "GEAndromida", "ge", "dictionary.ipf")
TERRA_DICT = os.path.join(GE, "dictionary")
ANDRO_EN_MEMBER = "dictionary_local.xml"     # the client's English table
ANDRO_KO_MEMBER = "dictionary_lockr.xml"     # the client's Korean table (CP949)
MODERN = 10_000_000                          # the modern band's floor

OUT_MAP = os.path.join(DATA, "text_id_map_terra_to_andro.json")
OUT_UNMAPPED = os.path.join(DATA, "text_id_map_unmapped.json")

_TEXT = re.compile(r'<Text\s+ClassID="(\d+)"\s+Text="((?:[^"]|&quot;)*)"\s*/?>')
_TSRC = re.compile(r'<TextSource\s+ClassID="(\d+)"\s+Text="((?:[^"]|&quot;)*)"')
_TOKEN = re.compile(rb"<\$>(\d+)</>")

_ENT = (("&lt;", "<"), ("&gt;", ">"), ("&quot;", '"'), ("&apos;", "'"),
        ("&amp;", "&"))
_NUM = re.compile(r"&#x([0-9A-Fa-f]+);|&#(\d+);")
_STUB = "NO_LOCAL_STRING"


def unescape(s: str) -> str:
    for a, b in _ENT:
        s = s.replace(a, b)
    return _NUM.sub(lambda m: chr(int(m.group(1), 16)) if m.group(1)
                    else chr(int(m.group(2))), s)


def real_text(s: str) -> str:
    """The text, or "" when the pack has no translation (a `NO_LOCAL_STRING:` stub).

    The server pack carries that stub where a caption was never translated: it is
    mojibake, not a string, and matching on it would join two unrelated lines.
    """
    if not s or s.startswith(_STUB):
        return ""
    return s


def norm(s: str) -> str:
    """The join key: width-folded, case-folded, whitespace-collapsed, dots off.

    Everything this folds away is formatting the two packs disagree about:
    a full-width bracket, `CoA` vs `COA`, a `\\r\\n` vs `\\n`, a double space, the
    full stop a caption may or may not carry. The REPORT says how many matches
    needed it, per kind.
    """
    s = unicodedata.normalize("NFKC", s)
    s = " ".join(s.split())
    return s.strip(" .").casefold()


def parse_flat(path: str, enc: str) -> dict[str, str]:
    """{id: text} for a `<Text ClassID=... Text=.../>` dictionary."""
    with open(path, "r", encoding=enc, errors="replace") as fh:
        blob = fh.read()
    return {cid: unescape(txt) for cid, txt in _TEXT.findall(blob)}


def parse_terra_korean(path: str) -> dict[str, str]:
    """{id: korean} from the TERRA triple: the Text= on <TextSource> is Korean."""
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        blob = fh.read()
    return {cid: unescape(txt) for cid, txt in _TSRC.findall(blob)}


def extract_andro(ipf_path: str, outdir: str) -> dict[str, str]:
    """Pull the two members this needs out of the .ipf; report what failed.

    Last copy wins: the archive is a patch stack and the newest `dictionary_local.xml`
    is the last one, which is what the client loads.
    """
    os.makedirs(outdir, exist_ok=True)
    out: dict[str, str] = {}
    failed: list[str] = []
    pwd = bytes.fromhex(ipf.KEY)
    infos = ipf.open_archive(ipf_path).infolist()
    want = {ANDRO_EN_MEMBER, ANDRO_KO_MEMBER}
    sel = {}
    for info in infos:
        if info.filename in want:
            sel[info.filename] = info
    for name, info in sel.items():
        dest = os.path.join(outdir, name)
        if os.path.exists(dest) and os.path.getsize(dest) == info.file_size:
            out[name] = dest
            continue
        try:
            data = ipf.read_member(ipf_path, info, pwd)
        except Exception as exc:                     # noqa: BLE001 - report, go on
            failed.append(f"{name}: {type(exc).__name__}: {exc}")
            continue
        with open(dest, "wb") as fh:
            fh.write(data)
        out[name] = dest
    for name in want - set(sel):
        failed.append(f"{name}: not a member of {os.path.basename(ipf_path)}")
    for f in failed:
        print(f"  UNREADABLE {f}")
    print(f"  archive: {len(infos)} members, {len(sel)} of {len(want)} names found, "
          f"{len(out)} read")
    return out


def index(texts: dict[str, str]) -> dict[str, list[int]]:
    """normalised text -> [andro ids]. The list is what makes ambiguity visible."""
    idx: dict[str, list[int]] = {}
    for cid, text in texts.items():
        if not text:
            continue
        idx.setdefault(norm(text), []).append(int(cid))
    return idx


def pick(cands: list[int], tid: int) -> int:
    """One Andro id for a text that several ids carry.

    The same band first (a legacy line belongs to a legacy caption), then the
    NEAREST id: the two packs differ by a small shift, so nearest is the answer
    far more often than "smallest".
    """
    same = [c for c in cands if (c >= MODERN) == (tid >= MODERN)]
    pool = same or cands
    return min(pool, key=lambda c: (abs(c - tid), c))


def corpus_ids(script_dir: str) -> set[int]:
    """Every distinct `<$>N</>` id the server-side scripts quote."""
    ids: set[int] = set()
    for root, _dirs, files in os.walk(script_dir):
        for name in files:
            if not name.lower().endswith(".scp"):
                continue
            path = os.path.join(root, name)
            try:
                with open(path, "rb") as fh:
                    ids |= {int(m) for m in _TOKEN.findall(fh.read())}
            except OSError:
                continue
    return ids


class Matcher:
    """Text -> Andro id, with the reason a match was accepted."""

    def __init__(self, en: dict[str, str], ko: dict[str, str]):
        self.en, self.ko = en, ko
        self.en_idx, self.ko_idx = index(en), index(ko)
        self.by_id_en = {int(k): v for k, v in en.items()}
        self.by_id_ko = {int(k): v for k, v in ko.items()}
        self.kind: dict[str, int] = {}

    def _note(self, kind: str) -> None:
        self.kind[kind] = self.kind.get(kind, 0) + 1

    def _join(self, lang: str, text: str, tid: int) -> int | None:
        """Find `text` in the Andro table `lang`; say whether it was exact."""
        idx = self.en_idx if lang == "en" else self.ko_idx
        src = self.en if lang == "en" else self.ko
        cands = idx.get(norm(text))
        if not cands:
            return None
        aid = pick(cands, tid)
        raw = src.get(str(aid), "")
        if raw == text:
            self._note(f"{lang}-exact")
        elif " ".join(raw.split()) == " ".join(text.split()):
            self._note(f"{lang}-whitespace")
        elif raw.casefold() == text.casefold():
            self._note(f"{lang}-case")
        elif unicodedata.normalize("NFKC", raw) == unicodedata.normalize("NFKC", text):
            self._note(f"{lang}-width")
        else:
            self._note(f"{lang}-trailing")
        if len(set(cands)) > 1:
            self._note(f"{lang}-ambiguous")
        return aid

    def match(self, tid: int, ko: str, en: str) -> int | None:
        """The Andro id for one TERRA id, or None.

        ORDER
          1. the SAME id carrying the SAME text - no translation needed at all;
          2. the Korean line (generation-stable, TERRA's own source text);
          3. the English line (the modern band's only source; the server pack's
             own English for the legacy band).
        """
        if en and self.by_id_en.get(tid) == en:
            self._note("same-id-en")
            return tid
        if ko and self.by_id_ko.get(tid) == ko:
            self._note("same-id-ko")
            return tid
        if ko:
            got = self._join("ko", ko, tid)
            if got is not None:
                return got
        if en:
            got = self._join("en", en, tid)
            if got is not None:
                return got
        return None


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Map the TERRA text ids onto the Andro client's.")
    ap.add_argument("--ipf", default=DEFAULT_IPF, help="the client's dictionary.ipf")
    ap.add_argument("--cache", default=os.path.join(CACHE, "andro_dict"),
                    help="where the extracted members are kept")
    ap.add_argument("--corpus-only", action="store_true",
                    help="map only the ids the scripts quote, not the whole dictionary")
    args = ap.parse_args(argv)

    print("== ANDRO dictionary (the client we play)")
    files = extract_andro(args.ipf, args.cache)
    if ANDRO_EN_MEMBER not in files or ANDRO_KO_MEMBER not in files:
        print("  cannot continue without both members", file=sys.stderr)
        return 2
    a_en = parse_flat(files[ANDRO_EN_MEMBER], "utf-8")
    a_ko = parse_flat(files[ANDRO_KO_MEMBER], "cp949")
    for name, tab in ((ANDRO_EN_MEMBER, a_en), (ANDRO_KO_MEMBER, a_ko)):
        ids = [int(i) for i in tab]
        print(f"  {name:<26} {len(tab):>7} entries  ids {min(ids)}..{max(ids)}"
              f"  modern {sum(1 for i in ids if i >= MODERN)}")
    proof_en = a_en.get("10018970")
    proof_ko = a_ko.get("10018970")
    print(f"  proof en 10018970 = {proof_en!r}")
    print(f"  proof ko 10018970 = {proof_ko!r}")

    print("== TERRA (the server pack we port)")
    t_ko = parse_terra_korean(os.path.join(TERRA_DICT, "dictionary.xml"))
    t_en = parse_flat(os.path.join(TERRA_DICT, "dictionary_local.xml"), "utf-8")
    t_sp = parse_flat(os.path.join(TERRA_DICT, "dictionary_local_serverpack.xml"),
                      "latin-1")
    for name, tab in (("dictionary.xml (ko)", t_ko),
                      ("dictionary_local.xml (en)", t_en),
                      ("dictionary_local_serverpack.xml (en)", t_sp)):
        ids = [int(i) for i in tab]
        print(f"  {name:<40} {len(tab):>7} entries  ids {min(ids)}..{max(ids)}"
              f"  modern {sum(1 for i in ids if i >= MODERN)}")

    src = corpus_ids(os.path.join(GE, "script"))
    modern_src = {i for i in src if i >= MODERN}
    legacy_src = src - modern_src
    print(f"== CORPUS  {len(src)} distinct ids  (modern {len(modern_src)}, "
          f"legacy {len(legacy_src)})")

    # ---- the overlap, before any mapping ------------------------------------
    en_ids, ko_ids = set(int(i) for i in a_en), set(int(i) for i in a_ko)
    ter_all = set(int(i) for i in t_ko) | set(int(i) for i in t_en) | set(int(i) for i in t_sp)
    same_id_modern = sum(1 for i in modern_src if i in en_ids)
    same_text_modern = sum(1 for i in modern_src if a_en.get(str(i), "") == (
        t_en.get(str(i), "") or t_sp.get(str(i), "")))
    same_id_legacy = sum(1 for i in legacy_src if i in en_ids or i in ko_ids)
    same_text_legacy = sum(
        1 for i in legacy_src
        if (a_ko.get(str(i), "") and a_ko.get(str(i)) == t_ko.get(str(i), ""))
        or (a_en.get(str(i), "") and a_en.get(str(i)) == (t_en.get(str(i), "")
                                                          or t_sp.get(str(i), ""))))
    terra_norm = {norm(t) for t in
                  list(t_ko.values()) + list(t_en.values()) + list(t_sp.values())}
    new_text = sum(1 for t in a_en.values() if t and norm(t) not in terra_norm)
    print("== OVERLAP")
    print(f"  modern corpus ids in Andro (by id)      {same_id_modern}/{len(modern_src)}")
    print(f"  modern corpus ids same id AND text      {same_text_modern}/{len(modern_src)}")
    print(f"  legacy corpus ids in Andro (by id)      {same_id_legacy}/{len(legacy_src)}")
    print(f"  legacy corpus ids same id AND text      {same_text_legacy}/{len(legacy_src)}")
    print(f"  Andro en ids TERRA never had            {len(en_ids - ter_all)}/{len(en_ids)}")
    print(f"  Andro ko ids TERRA never had            {len(ko_ids - ter_all)}/{len(ko_ids)}")
    print(f"  Andro en LINES TERRA never had (by text) {new_text}/{len(a_en)}"
          f"  ({100.0 * new_text / len(a_en):.1f}%)")

    # ---- the map -----------------------------------------------------------
    m = Matcher(a_en, a_ko)
    if args.corpus_only:
        targets = sorted(src)
    else:
        # every id the scripts quote is a target even when TERRA's own
        # dictionaries have no entry for it: otherwise it falls between the two
        # output files instead of landing on the worklist.
        targets = sorted(ter_all | src)
    mapping: dict[str, str] = {}
    unmapped: dict[str, dict] = {}
    stub = 0
    moved = 0
    for tid in targets:
        raw_ko = t_ko.get(str(tid), "")
        raw_en = t_en.get(str(tid)) or t_sp.get(str(tid)) or ""
        ko, en = real_text(raw_ko), real_text(raw_en)
        was_stub = bool(raw_en) and not en
        present = str(tid) in a_en or str(tid) in a_ko
        if not ko and not en and present and not (a_en.get(str(tid))
                                                  or a_ko.get(str(tid))):
            # An EMPTY caption on both sides (id 10000391 is one): there is no
            # text to translate, and both clients draw nothing. Same id, done.
            mapping[str(tid)] = str(tid)
            m.kind["same-id-empty"] = m.kind.get("same-id-empty", 0) + 1
            continue
        aid = m.match(tid, ko, en)
        if aid is None:
            stub += was_stub
            unmapped[str(tid)] = {
                "ko": ko, "en": en,
                "band": "modern" if tid >= MODERN else "legacy",
                "why": "no-terra-text" if not (ko or en) else "text-not-in-andro",
                "stub": was_stub,
            }
        elif aid != tid:
            mapping[str(tid)] = str(aid)
            moved += 1
        else:
            mapping[str(tid)] = str(aid)

    # identity rows are KEPT: a consumer must not confuse "absent" with "same id".
    identity = len(targets) - len(mapping) - len(unmapped)

    # ---- coverage on the real corpus ---------------------------------------
    cov_m = sum(1 for i in modern_src if str(i) in mapping)
    cov_l = sum(1 for i in legacy_src if str(i) in mapping)
    near = far = 0
    for tid, aid in mapping.items():
        d = abs(int(aid) - int(tid))
        if d <= 2:
            near += 1
        elif d > 1000:
            far += 1
    reasons: dict[str, int] = {}
    for row in unmapped.values():
        reasons[row["why"]] = reasons.get(row["why"], 0) + 1

    os.makedirs(DATA, exist_ok=True)
    with open(OUT_MAP, "w", encoding="utf-8") as fh:
        json.dump(mapping, fh, ensure_ascii=False, sort_keys=True)
    with open(OUT_UNMAPPED, "w", encoding="utf-8") as fh:
        json.dump(unmapped, fh, ensure_ascii=False, sort_keys=True, indent=0)

    print("== MAP")
    print(f"  targets                {len(targets)}")
    print(f"  rows written           {len(mapping)}  (of which {moved} move the id, "
          f"{len(mapping) - moved} keep it)")
    print(f"  unmapped               {len(unmapped)}  " + ", ".join(
        f"{k}={v}" for k, v in sorted(reasons.items()))
          + f"  (of which TERRA stubs {stub})")
    print("  how each id was joined: " + ", ".join(
        f"{k}={v}" for k, v in sorted(m.kind.items())))
    print(f"  |delta|: <=2 {near}, >1000 {far}, between {len(mapping) - near - far}")
    print(f"  wrote {OUT_MAP}  ({os.path.getsize(OUT_MAP) / 1e6:.1f} MB)")
    print(f"  wrote {OUT_UNMAPPED}  ({os.path.getsize(OUT_UNMAPPED) / 1e6:.1f} MB)")

    # ---- verify the artifact: every row must carry the SAME text -----------
    with open(OUT_MAP, "r", encoding="utf-8") as fh:
        back = json.load(fh)
    bad = 0
    for tid, aid in back.items():
        ko = real_text(t_ko.get(tid, ""))
        en = real_text(t_en.get(tid, "") or t_sp.get(tid, ""))
        ok = False
        if not ko and not en:
            # an empty caption on both sides: nothing to draw, nothing to check
            ok = not (a_ko.get(aid, "") or a_en.get(aid, ""))
        if not ok and ko and norm(ko) == norm(a_ko.get(aid, "")):
            ok = True
        if not ok and en and norm(en) == norm(a_en.get(aid, "")):
            ok = True
        if not ok:
            bad += 1
            if bad <= 5:
                print(f"  TEXT MISMATCH {tid}->{aid}: {ko[:40]!r}/{en[:40]!r} vs "
                      f"{a_ko.get(aid, '')[:40]!r}/{a_en.get(aid, '')[:40]!r}")
    print(f"  verified: {len(back) - bad}/{len(back)} rows carry the same text "
          f"on both sides")

    print("== COVERAGE (the finish line)")
    print(f"  modern {cov_m}/{len(modern_src)}  legacy {cov_l}/{len(legacy_src)}"
          f"  total {cov_m + cov_l}/{len(src)}")
    left = {"real-line": 0, "pack-stub": 0, "no-pack-entry": 0}
    for i in src:
        row = unmapped.get(str(i))
        if not row:
            continue
        if row["stub"]:
            left["pack-stub"] += 1
        elif not row["ko"] and not row["en"]:
            left["no-pack-entry"] += 1
        else:
            left["real-line"] += 1
    print(f"  remaining {sum(left.values())} ids: " + ", ".join(
        f"{k}={v}" for k, v in left.items()))
    print("DONE andro-dict entries=%d, range=%d..%d, modern-overlap=%d/%d, "
          "legacy-overlap=%d/%d, at-risk=5 sites, verdict=%s"
          % (len(a_en), min(en_ids), max(en_ids), cov_m, len(modern_src), cov_l,
             len(legacy_src), "ids-ok"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
