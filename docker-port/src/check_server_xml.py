r"""Are `ge\server_xml` and `ge\server_xml_export` really the same content?

WHY. The server-only tables (shops, boxes, rewards) exist in two folders, and the game
describes the intended difference as `raw xml -> export with strings replaced by dictionary
references`. If that is all the difference is, either folder will do for the rebuild. If the
DATA differs, picking the wrong one changes what a shop sells or a box grants.

The answer has to be measured per FILE and per FIELD, because the two kinds of difference
matter very differently:

- **text only** - one side holds the Korean words, the other `<$>44942</>`. Same data, and
  our server resolves those tokens through the client dictionary anyway.
- **data** - a number, a name or a boolean that is not the same on both sides. That is the
  kind that changes behaviour.

Two traps this tool exists to avoid: these files are EUC-KR (a strict UTF-8 parse fails, and
a failed parse on BOTH sides compares equal - that is how a first attempt wrongly reported
"205 identical"), and several files are not well-formed XML at all, so rows are read with a
regex rather than a parser.

Usage
-----
    python check_server_xml.py                 # every file, summarised
    python check_server_xml.py --data-only     # list only files whose DATA differs
    python check_server_xml.py --show datatable_custom_shop.xml

Exit code 1 when any file's DATA differs (not text): that is the case where the folder
choice matters.
"""

import argparse
import os
import re
import sys

from ge_paths import GE

RAW_DIR = os.path.join(GE, "server_xml")
EXPORT_DIR = os.path.join(GE, "server_xml_export")

# A `<Class ...>` tag. It MUST tolerate `>` inside a quoted value: a localized row holds
# `Name="<$>46252</>"`, and a `[^>]*` pattern stops at that `>`, silently dropping every
# attribute after it - which is what made an earlier version report 64 files as "data
# differs" when they differed only in words.
TAG_RE = re.compile(r'<Class\s(?:[^>"]|"[^"]*")*?/?>')
ATTR_RE = re.compile(r'([A-Za-z_][\w]*)="([^"]*)"')
DECL_RE = re.compile(rb'<\?xml[^>]*encoding="([^"]+)"')

# How a "text" value looks: Korean (or any non-ASCII), or a dictionary reference.
TEXTISH = re.compile(r"<\$>\d+</>|[^\x00-\x7f]")


def decode(path):
    """Read a table that may be EUC-KR and may not be well-formed XML.

    The declared encoding is tried first, then the two encodings these files actually
    use. `errors="replace"` keeps a bad byte from losing the whole file; the point is to
    compare two copies of the same text, not to validate it.
    """
    raw = open(path, "rb").read()
    found = DECL_RE.search(raw)
    declared = found.group(1).decode("ascii", "replace") if found else None
    for encoding in (declared, "euc-kr", "cp949", "utf-8"):
        if not encoding:
            continue
        try:
            return raw.decode(encoding)
        except (UnicodeDecodeError, LookupError):
            continue
    return raw.decode("utf-8", "replace")


CATEGORY_RE = re.compile(r"<Category>(.*?)</Category>", re.S)
NAME_RE = re.compile(r"<Name>(.*?)</Name>", re.S)


def rows(path):
    """{(category, ClassID): {attr: value}} - the table's real identity.

    These files are a list of `<Category>` groups, and ClassID restarts at 1 in EVERY
    group: a flat list compares the wrong rows against each other (and a dict keyed by id
    merges them). The pair is unique, and order stops mattering.
    """
    text = decode(path)
    out = {}
    groups = CATEGORY_RE.findall(text)
    if not groups:
        groups = [text]
    for group in groups:
        found = NAME_RE.search(group)
        category = found.group(1).strip() if found else ""
        for index, tag in enumerate(TAG_RE.findall(group)):
            attrs = dict(ATTR_RE.findall(tag))
            key = (category, attrs.get("ClassID") or attrs.get("ClassName")
                   or ("row%d" % index))
            out[key] = attrs
    return out


def textish(value):
    """A value that is a WORD (Korean, or a dictionary reference) rather than data."""
    return bool(TEXTISH.search(value or ""))


def safe(value):
    """Console-safe: a Korean value must not crash the report on a cp1252 terminal."""
    return str(value).encode("ascii", "backslashreplace").decode("ascii")


def compare_file(name, show=False):
    a_path = os.path.join(RAW_DIR, name)
    b_path = os.path.join(EXPORT_DIR, name)
    if not (os.path.exists(a_path) and os.path.exists(b_path)):
        return ("missing", name, ["only in one folder"])

    a, b = rows(a_path), rows(b_path)
    if a == b:
        return ("identical", name, [])

    text_diffs = 0
    data_diffs = []
    rows_only_a = set(a) - set(b)
    rows_only_b = set(b) - set(a)
    for key in set(a) & set(b):
        row_a, row_b = a[key], b[key]
        label = row_a.get("ClassName") or row_a.get("ItemName") or key[1]
        for attr, value in row_a.items():
            other = row_b.get(attr)
            if (value or "") == (other or ""):
                continue
            # A WORD on either side is localization, whatever the other side holds.
            if textish(value) or textish(other):
                text_diffs += 1
                continue
            data_diffs.append("%s (%s) %s: server_xml=%r export=%r"
                              % (label, key[0], attr, value, other))

    if rows_only_a or rows_only_b or data_diffs:
        notes = [safe(line) for line in data_diffs[:6]]
        if rows_only_a:
            notes.append("%d row(s) only in server_xml, e.g. %s"
                         % (len(rows_only_a), safe(sorted(rows_only_a)[0])))
        if rows_only_b:
            notes.append("%d row(s) only in server_xml_export, e.g. %s"
                         % (len(rows_only_b), safe(sorted(rows_only_b)[0])))
        return ("data", name, notes)
    return ("text", name, ["%d localized field(s) differ" % text_diffs] if text_diffs else [])


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--ge", help="use ANOTHER data tree (the folder that holds xml\\, ies\\, ...)")
    ap.add_argument("--raw", help="compare this folder instead of ge\\server_xml")
    ap.add_argument("--export", help="compare against this folder instead of ge\\server_xml_export")
    ap.add_argument("--data-only", action="store_true",
                    help="list only the files whose DATA differs")
    ap.add_argument("--show", help="detail one file")
    args = ap.parse_args()

    global RAW_DIR, EXPORT_DIR
    base = args.ge or GE
    RAW_DIR = args.raw or os.path.join(base, "server_xml")
    EXPORT_DIR = args.export or os.path.join(base, "server_xml_export")
    print("A = %s" % RAW_DIR)
    print("B = %s" % EXPORT_DIR)

    if args.show:
        kind, name, notes = compare_file(args.show)
        print("%s: %s" % (name, kind))
        for note in notes:
            print("  " + note)
        return

    names = sorted(f for f in os.listdir(RAW_DIR) if f.endswith(".xml"))
    buckets = {"identical": [], "text": [], "data": [], "missing": []}
    for name in names:
        kind, _, notes = compare_file(name)
        buckets[kind].append((name, notes))

    print("files                 : %d" % len(names))
    print("  identical           : %d" % len(buckets["identical"]))
    print("  text-only difference: %d" % len(buckets["text"]))
    print("  DATA difference     : %d" % len(buckets["data"]))
    if buckets["missing"]:
        print("  only in one folder  : %d" % len(buckets["missing"]))

    if buckets["text"] and not args.data_only:
        print("\ntext-only (safe either way):")
        for name, _ in buckets["text"]:
            print("  " + name)

    if buckets["data"]:
        print("\nDATA differences (the folder choice matters here):")
        for name, notes in buckets["data"]:
            print("  %s" % name)
            for note in notes:
                print("      " + note)

    print("\n%s" % ("EITHER FOLDER WILL DO - no data differs"
                    if not buckets["data"] else
                    "DATA DIFFERS - decide which folder the server reads before using it"))
    sys.exit(1 if buckets["data"] else 0)


if __name__ == "__main__":
    main()
