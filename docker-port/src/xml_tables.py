"""xml_tables - read one of the server's XML tables, whatever its encoding.

The server's editable table tree is `ge\\xml\\`. Its encoding is MIXED:

  - 1,489 tables rebuilt from the .ies are UTF-8 with locale TOKENS
  - a handful of hand-made tables (no .ies) are still the original EUC-KR

So a reader cannot simply pick one encoding. This module reads the
declaration in the file, tries that, and falls back through the other
candidates - so every reader in the server gets the same answer and no
table is ever silently mangled.

The OLD readers did `raw.decode("cp949", "replace")`. That was right when
the whole tree was EUC-KR, but on a UTF-8 file it either mangles the text
or (on a file with a byte sequence cp949 rejects) turns whole fields into
replacement characters. Two of the rebuilt tables hit that.

Use it like this:

    from xml_tables import read_table
    root = read_table(r"<path>")

`read_table` returns an ElementTree root, or None if the file cannot be
read at all (a missing table is not a crash).
"""
import os
import re
import xml.etree.ElementTree as ET

# The order matters: try the declared encoding first, then the other one, then
# a byte-preserving fallback so a stray byte never loses the whole table.
CANDIDATES = ("utf-8", "cp949", "euc-kr", "latin-1")

_DECL = re.compile(rb'encoding="([^"]+)"')


def _decode(raw):
    """Decode the bytes, honouring the file's own declaration."""
    m = _DECL.search(raw[:200])
    order = list(CANDIDATES)
    if m:
        declared = m.group(1).decode("ascii", "replace").strip().lower()
        if declared in ("utf-8", "utf8"):
            declared = "utf-8"
        elif declared in ("euc-kr", "cp949"):
            declared = "cp949"
        order = [declared] + [c for c in order if c != declared]
    for enc in order:
        try:
            return raw.decode(enc)
        except (UnicodeDecodeError, LookupError):
            continue
    return raw.decode("latin-1", "replace")


def read_table(path):
    """Read an XML table into an ElementTree root, or None."""
    if not os.path.exists(path):
        return None
    try:
        raw = open(path, "rb").read()
    except OSError:
        return None
    text = _decode(raw)
    # Drop the declaration: ElementTree refuses a multi-byte encoding, and we
    # have already decoded the bytes ourselves.
    text = re.sub(r"^\s*<\?xml[^>]*\?>", "", text, count=1)
    try:
        return ET.fromstring(text)
    except ET.ParseError:
        return None


def read_table_text(path):
    """The raw text of a table, decoded - for tools that send it verbatim."""
    if not os.path.exists(path):
        return None
    return _decode(open(path, "rb").read())


if __name__ == "__main__":
    import sys
    for p in sys.argv[1:]:
        root = read_table(p)
        rows = sum(1 for _ in root.iter("Class")) if root is not None else 0
        print("%-46s %s  rows=%d" % (os.path.basename(p),
                                     "ok" if root is not None else "FAILED", rows))
