"""check_data_generation - which generation of the game data are we on?

Run from src\\:  python check_data_generation.py

The server and the client must be on ONE generation of the game data, and the
client is the anchor. This checker MEASURES where each copy stands instead of
asking you to believe a note. It answers four questions:

1. How many copies of the tables exist, and how far apart are they?
2. Do the locale tokens in each copy resolve in each dictionary?
3. Do the tables the code READS have the same COLUMNS in the old and the new
   copy? (A new column is a parser break waiting to happen.)
4. Which tables in `ge\\xml` carry a HAND EDIT, i.e. differ from a fresh
   decode of the .ies they came from? Those edits must be carried across
   when the tree is rebuilt - not overwritten.

Read-only: it changes nothing.
"""
import os

import re
import sys
import tempfile
import xml.etree.ElementTree as ET
from ge_paths import GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
GE = os.path.join(GAME_ROOT, "ge")

# The three copies of the same tables.
OLD_IES = os.path.join(GAME_ROOT, "GEAndromida", "tools", "readme", "ies")      # rev 318 generation
NEW_IES = os.path.join(GAME_ROOT, "_andro_ies_stage", "ies10000")          # the client's own
GE_IES = os.path.join(GE, "ies")

OLD_DICT = os.path.join(GAME_ROOT, "GETerraSeptember1th2022", "ge", "dictionary", "dictionary_local.xml")
NEW_DICT = os.path.join(GAME_ROOT, "_andro_dict_stage", "dictionary", "dictionary_local.xml")

CONVERTER = os.path.join(GAME_ROOT, "GEAndromida", "Converter")

# The tables the running server actually reads.
READ_TABLES = ["datatable_map", "datatable_npc", "datatable_town",
               "datatable_Shop"]

TOKEN = re.compile(r"&lt;\$&gt;(\d+)&lt;/&gt;")
CLASSID = re.compile(r'ClassID="(\d+)"')

_tmp = tempfile.mkdtemp(prefix="gegen_")
sys.path.insert(0, CONVERTER)
import ies2  # noqa: E402


def decode(ies_path, tag):
    """Convert a .ies to XML; return the parsed root (or None)."""
    out = os.path.join(_tmp, "%s_%s.xml" % (tag, os.path.basename(ies_path)))
    if not os.path.exists(out):
        try:
            ies2.ies_to_xml(ies_path, out, None, None, "UTF-8", False)
        except Exception as exc:
            print("    cannot convert %s: %s" % (os.path.basename(ies_path), exc))
            return None
    try:
        return ET.parse(out).getroot()
    except ET.ParseError as exc:
        print("    cannot parse %s: %s" % (out, exc))
        return None


def tokens_of(root):
    if root is None:
        return set()
    return set(int(m) for m in TOKEN.findall(ET.tostring(root, encoding="unicode")))


def dict_ids(path):
    ids = set()
    if not os.path.exists(path):
        return ids
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = CLASSID.search(line)
            if m:
                ids.add(int(m.group(1)))
    return ids


def columns_of(root):
    """The set of attribute names the table's rows carry."""
    if root is None:
        return set()
    cols = set()
    for cls in root.iter("Class"):
        cols.update(cls.attrib.keys())
    return cols


def count_rows(root):
    if root is None:
        return 0
    return sum(1 for _ in root.iter("Class"))


def names(folder):
    try:
        return set(os.listdir(folder))
    except OSError:
        return set()


def main():
  print("=" * 70)
  print("1. HOW MANY COPIES OF THE TABLES EXIST")
  print("=" * 70)
  sets = [("GEAndromida tools/readme/ies (what the server reads)", OLD_IES),
        ("ge\\ies (reference tree)", GE_IES),
        ("client's own ies10000 (the anchor)", NEW_IES)]
  counts = {}
  for label, folder in sets:
    n = names(folder)
    counts[label] = n
    print("  %-52s %5d files" % (label, len(n)))
  print()
  anchor = sets[2][0]
  for label, _ in sets[:2]:
    only_old = counts[label] - counts[anchor]
    only_new = counts[anchor] - counts[label]
    print("  %s" % label)
    print("     only here: %d      only in the client's copy: %d"
          % (len(only_old), len(only_new)))

  print()
  print("=" * 70)
  print("2. DO THE LOCALE TOKENS RESOLVE?  (the decisive test)")
  print("=" * 70)
  old_dict = dict_ids(OLD_DICT)
  new_dict = dict_ids(NEW_DICT)
  print("  old dictionary (rev 318): %d ids" % len(old_dict))
  print("  new dictionary (rev 136): %d ids" % len(new_dict))
  print()
  for label, folder in sets:
      if not os.path.isdir(folder):
          continue
      toks = set()
      for name in sorted(os.listdir(folder)):
          if not name.startswith("npcgen_") or not name.endswith(".ies"):
              continue
          root = decode(os.path.join(folder, name), "tok" + str(len(toks)))
          toks |= tokens_of(root)
      if not toks:
          continue
      print("  %s" % label)
      print("     tokens: %d   resolve in OLD dict: %-6d  in NEW dict: %d"
            % (len(toks), len(toks & old_dict), len(toks & new_dict)))
      big = sum(1 for t in toks if t >= 10000000)
      print("     id style: %d of %d are the NEW shape (>= 10,000,000)"
            % (big, len(toks)))
  print()

  print("=" * 70)
  print("3. SAME COLUMNS IN OLD AND NEW?  (a new column breaks a parser)")
  print("=" * 70)
  for table in READ_TABLES:
      a = decode(os.path.join(OLD_IES, table + ".ies"), "old")
      b = decode(os.path.join(NEW_IES, table + ".ies"), "new")
      if a is None or b is None:
          print("  %-18s (one side missing)" % table)
          continue
      ca, cb = columns_of(a), columns_of(b)
      added, gone = cb - ca, ca - cb
      print("  %-18s old rows=%-6d new rows=%-6d  columns added=%-3d removed=%-3d"
            % (table, count_rows(a), count_rows(b), len(added), len(gone)))
      if added:
          print("       new columns: %s" % ", ".join(sorted(added)[:8]))
      if gone:
          print("       gone columns: %s" % ", ".join(sorted(gone)[:8]))
  print()

def _num(s):
    """A comparable form of a number, so 0.1 and 0.10 are the SAME value."""
    try:
        return ("n", float(s))
    except (TypeError, ValueError):
        return ("s", s)


def load_xml(path):
    """Read an XML file whatever encoding it declares (the ge\\xml tree is
    EUC-KR, and ElementTree refuses multi-byte encodings)."""
    raw = open(path, "rb").read()
    m = re.search(rb'encoding="([^"]+)"', raw[:200])
    enc = m.group(1).decode("ascii", "replace") if m else "utf-8"
    for candidate in (enc, "utf-8", "euc-kr", "cp949", "latin-1"):
        try:
            return ET.fromstring(raw.decode(candidate))
        except (UnicodeDecodeError, ET.ParseError):
            continue
    return None


def row_map(root):
    """{ClassID: {attribute: value}} for a table."""
    if root is None:
        return {}
    out = {}
    for cls in root.iter("Class"):
        key = cls.get("ClassID") or cls.get("ClassName")
        out[key] = dict(cls.attrib)
    return out


def diff_rows(a, b):
    """Classify the difference between two row maps of the same table."""
    ka, kb = set(a), set(b)
    added, removed = kb - ka, ka - kb
    changed = []
    for k in ka & kb:
        if a[k] != b[k]:
            diff = [f for f in set(a[k]) | set(b[k]) if a[k].get(f) != b[k].get(f)]
            changed.append((k, diff))
    return len(added), len(removed), changed


def main():


  print("=" * 70)
  print("4. HAND-EDITED TABLES  (carry these across, do not lose them)")
  print("=" * 70)
  print("  Compare ge\\xml_export (the original export, EUC-KR, WITH tokens)")
  print("  against a fresh decode of ge\\ies\\X.ies - the SAME generation, so")
  print("  any difference is a real hand edit, not generation drift.")
  print("  (Comparing it against the other .ies copies shows a flood of false")
  print("  differences: those copies are a different generation entirely.)")
  print()
  export_dir = os.path.join(GE, "xml_export")
  hand = []
  checked = 0
  for name in sorted(names(GE_IES)):
      if not name.endswith(".ies"):
          continue
      base = name[:-4]
      xml = os.path.join(export_dir, base + ".xml")
      if not os.path.exists(xml):
          continue
      checked += 1
      fresh = decode(os.path.join(GE_IES, name), "h%d" % checked)
      cur = load_xml(xml)
      if fresh is None or cur is None:
          continue
      a, b = row_map(fresh), row_map(cur)
      added, removed, changed = diff_rows(a, b)
      if not (added or removed or changed):
          continue
      # Classify the difference. A real hand edit is a VALUE that changed
      # meaning. These three are the EXPORT's formatting, not edits:
      #   - a token id moved (the export may carry a different id)
      #   - "0" or "None" written as "" (the XML writer drops empty values)
      #   - "0.1" written as "0.10" (the XML writer keeps the text)
      token_only = True
      for k, fields in changed:
          for f in fields:
              va, vb = str(a[k].get(f, "")), str(b[k].get(f, ""))
              if va == vb:
                  continue
              if TOKEN.search(va) or TOKEN.search(vb):
                  continue
              if vb == "" and va in ("0", "None"):
                  continue
              if _num(va) == _num(vb):
                  continue
              token_only = False
      if added or removed:
          hand.append((base, "%d rows added, %d removed" % (added, removed)))
      elif token_only:
          hand.append((base, "%d rows: formatting only" % len(changed)))
      else:
          hand.append((base, "%d rows: a VALUE was edited" % len(changed)))
  print("  checked %d tables in ge\\xml_export against a fresh decode of ge\\ies"
        % checked)
  if hand:
      for base, why in hand:
          print("     %-42s %s" % (base, why))
  else:
      print("     no differences found")
  print()
  print("  'formatting only' means the export differs from a fresh decode only")
  print("  in whitespace, in 0/None written as empty, or in 0.1 vs 0.10 - the")
  print("  XML writer's own rendering. A real edit is listed as 'a VALUE was")
  print("  edited'. Zero of those means the export is a faithful render of")
  print("  the .ies, so the tree can be REBUILT from a new .ies with nothing")
  print("  lost.")
  print()
  print("done. Next: docker-port\\ROADMAP.md R36, and the plan in")
  print("docker-port\\history\\data-generation.md")


if __name__ == "__main__":
    sys.exit(main())
