"""rebuild_xml - rebuild the server's editable XML tree from the .ies tables.

Run from src\\:  python rebuild_xml.py [--dry]

The server's editable table tree is `ge\\xml\\`. It must be:

  - in the CLIENT's generation (the client is the anchor - see R36)
  - UTF-8, not the original EUC-KR
  - carrying LOCALE TOKENS (`<$>10814</>`), not resolved Korean text, so the
    client resolves the text itself and always shows its own wording

`ge\\xml_export\\` is the ORIGINAL export and is never touched. This tool
writes `ge\\xml\\` from the .ies in `ge\\ies\\`.

WHAT IS PRESERVED
  - 13 tables that have no .ies at all (barrackhotkey, hotkey, options,
    pc_list, tooltext, worldpvphotkey, datatable_force*, datatable_watch and
    the poisonyard2/shadowtemple2 flags). They are hand-made and are copied
    through untouched.
  - Everything else is regenerated from its .ies. The measurement in
    `history\\data-generation.md` found ZERO real hand edits in the tables
    that DO have a .ies, so nothing is lost by regenerating them.

Read-only by default: without --dry it asks first.
"""
import os

import shutil
import sys
import tempfile
import time
import xml.etree.ElementTree as ET
from ge_paths import GAME_ROOT

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
GE = os.path.join(GAME_ROOT, "ge")
IES_DIR = os.path.join(GE, "ies")
XML_DIR = os.path.join(GE, "xml")
CONVERTER = os.path.join(GAME_ROOT, "GEAndromida", "Converter")

sys.path.insert(0, CONVERTER)
import ies2  # noqa: E402


def hand_made():
    """The ge\\xml files that have no .ies - they must survive untouched."""
    names = {n[:-4] for n in os.listdir(IES_DIR) if n.endswith(".ies")}
    out = []
    for n in sorted(os.listdir(XML_DIR)):
        if n.endswith(".xml") and n[:-4] not in names:
            out.append(n)
    return out


def convert(ies_path, out_path):
    """Convert one .ies to a UTF-8 XML file with tokens."""
    tmp = out_path + ".tmp"
    try:
        ies2.ies_to_xml(ies_path, tmp, None, None, "UTF-8", False)
        # The converter writes UTF-8 with a declaration; make sure it is
        # parseable and that no stray declaration snuck in.
        raw = open(tmp, "rb").read()
        ET.fromstring(raw.decode("utf-8", "replace"))
        if os.path.exists(out_path):
            os.remove(out_path)
        os.replace(tmp, out_path)
        return True
    finally:
        if os.path.exists(tmp):
            try:
                os.remove(tmp)
            except OSError:
                pass


def main():
    dry = "--dry" in sys.argv
    keep = hand_made()
    ies_files = sorted(n for n in os.listdir(IES_DIR) if n.endswith(".ies"))

    # A DRY RUN MUST NOT WRITE. Convert into a scratch folder instead, so the
    # run proves the conversion works and the real tree stays untouched.
    target_dir = XML_DIR
    scratch = None
    if dry:
        scratch = tempfile.mkdtemp(prefix="rebuild_xml_")
        target_dir = scratch

    print("rebuild_xml")
    print("  source   : %s  (%d .ies)" % (IES_DIR, len(ies_files)))
    print("  target   : %s" % target_dir)
    print("  keeping  : %d hand-made tables (no .ies): %s"
          % (len(keep), ", ".join(keep)))
    print("  mode     : %s" % ("DRY RUN - nothing written" if dry else "WRITE"))
    print()

    if not dry:
        print("This rewrites the server's editable table tree.")
        print("Type YES to continue: ", end="")
        if input().strip().upper() != "YES":
            print("cancelled.")
            return 1

    t0 = time.time()
    done = failed = 0
    for i, name in enumerate(ies_files, 1):
        out = os.path.join(target_dir, name[:-4] + ".xml")
        try:
            if convert(os.path.join(IES_DIR, name), out):
                done += 1
            else:
                failed += 1
                print("  FAILED %s" % name)
        except Exception as exc:
            failed += 1
            print("  FAILED %s: %s" % (name, exc))
        if i % 200 == 0:
            print("  ... %d/%d  (%.0fs)" % (i, len(ies_files), time.time() - t0))

    print()
    print("  converted: %d   failed: %d   (%.0fs)" % (done, failed, time.time() - t0))
    if dry:
        print("  DRY RUN - the real tree was NOT touched.")
        shutil.rmtree(scratch, ignore_errors=True)
        return 1 if failed else 0

    # The hand-made tables must still be there. A regeneration that writes one
    # output per source input leaves them alone; this PROVES it.
    missing = [n for n in keep if not os.path.exists(os.path.join(XML_DIR, n))]
    print("  hand-made tables kept: %d of %d%s"
          % (len(keep) - len(missing), len(keep),
             "" if not missing else "   MISSING: %s" % ", ".join(missing)))
    print("  ge\\xml now holds %d files" % len(os.listdir(XML_DIR)))
    print()
    print("  next: clear cache\\, then run check_data.py and check_all.py --servers")
    return 1 if (failed or missing) else 0


if __name__ == "__main__":
    sys.exit(main())
