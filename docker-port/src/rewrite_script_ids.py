"""Rewrite the locale ids in ge\\script\\*.scp onto the CLIENT's generation.

The scripts came from the server pack and quote that pack's ids. The client
(rev 136) numbers the same lines differently, so every dialog used to need a
lookup at send time (`ge_locale.resolve_for_client`). This pass applies the
decided mapping to the files themselves, so a script names the client's id
directly and no runtime translation is needed.

Byte level on purpose: the files are read and written as BYTES, so encoding,
BOM and line endings survive untouched. Only the digits inside a `<\\d+</>`
token are changed; nothing else in the file is touched.

    python rewrite_script_ids.py                  # dry run, prints the numbers
    python rewrite_script_ids.py --apply          # writes + records a manifest
    python rewrite_script_ids.py --apply --force  # apply again anyway

A second --apply is REFUSED while the manifest is present: one id in the table
(`199387 -> 10021685`) is both a key and a target, so running twice would move
it twice. Delete `data\\script_rewrite_manifest.json` when you really mean it.
"""

import argparse
import hashlib
import json
import os
import re
import sys

from ge_paths import DATA, GE_SCRIPT

TABLE = os.path.join(DATA, "locale_id_map.json")
MANIFEST = os.path.join(DATA, "script_rewrite_manifest.json")

# <$>12345</> - the only thing this tool is allowed to change.
TOKEN = re.compile(rb"<\$>(\d+)</>")


def _client_ids():
    """The ids the client's own dictionary knows (used to classify leftovers)."""
    try:
        import ge_locale
        return set(ge_locale.client_table())
    except Exception as exc:                      # pragma: no cover - best effort
        print("  (could not read the client's dictionary: %s)" % exc)
        return set()


def _scp_files():
    found = []
    for base, _dirs, names in os.walk(GE_SCRIPT):
        for name in names:
            if name.lower().endswith(".scp"):
                found.append(os.path.join(base, name))
    found.sort()
    return found


def _sha1_file(path):
    with open(path, "rb") as fh:
        return hashlib.sha1(fh.read()).hexdigest()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--apply", action="store_true",
                        help="write the files (default: dry run)")
    parser.add_argument("--force", action="store_true",
                        help="apply even though a manifest says it already ran")
    args = parser.parse_args(argv)

    if not os.path.isdir(GE_SCRIPT):
        print("script folder not found: %s" % GE_SCRIPT)
        return 2
    if not os.path.exists(TABLE):
        print("id table not found: %s  (run make_locale_map.py first)" % TABLE)
        return 2

    if args.apply and os.path.exists(MANIFEST) and not args.force:
        try:
            with open(MANIFEST, "r", encoding="utf-8") as fh:
                old = json.load(fh)
        except Exception:
            old = {}
        print("REFUSED: this tree was already rewritten.")
        if old:
            print("  applied   : %s" % old.get("applied", "?"))
            print("  files     : %s changed, %s tokens replaced" %
                  (old.get("files_changed", "?"), old.get("tokens_replaced", "?")))
        print("  The mapping is not idempotent (199387 is both a key and a")
        print("  target), so a second run would move that id again.")
        print("  Delete data\\script_rewrite_manifest.json, or pass --force.")
        return 1

    with open(TABLE, "r", encoding="utf-8") as fh:
        table = json.load(fh)
    client_ids = _client_ids()

    files = _scp_files()
    changed = 0
    replaced = 0
    already = 0        # token already names an id the client knows
    missing = 0        # no client text for this line - cannot be renumbered
    worst = []
    tree = hashlib.sha1()

    for path in files:
        with open(path, "rb") as fh:
            raw = fh.read()

        hits = [0]

        def _sub(match):
            old = match.group(1).decode("ascii")
            new = table.get(old)
            if new and new != old:
                hits[0] += 1
                return b"<$>" + new.encode("ascii") + b"</>"
            return match.group(0)

        out = TOKEN.sub(_sub, raw)

        left_missing = 0
        for match in TOKEN.finditer(out):
            token_id = match.group(1).decode("ascii")
            if token_id in client_ids:
                already += 1
            else:
                missing += 1
                left_missing += 1

        replaced += hits[0]
        if hits[0]:
            changed += 1
        if left_missing:
            worst.append((left_missing, os.path.relpath(path, GE_SCRIPT)))

        if args.apply and out != raw:
            with open(path, "wb") as fh:
                fh.write(out)

        tree.update(os.path.relpath(path, GE_SCRIPT).replace("\\", "/").encode("utf-8"))
        tree.update(b"\0")
        tree.update(hashlib.sha1(out).digest())

    print("%s - %s" % ("applied" if args.apply else "dry run",
                       "files written" if args.apply else "nothing written (use --apply)"))
    print("  scripts scanned             : %d" % len(files))
    print("  files %-22s: %d" % ("that change", changed))
    print("  tokens renumbered           : %d" % replaced)
    print("  tokens already the client's : %d" % already)
    print("  tokens with NO client text  : %d" % missing)

    if worst:
        worst.sort(reverse=True)
        print("\n  the files with the most lines that cannot be renumbered:")
        for count, rel in worst[:8]:
            print("    %4d  %s" % (count, rel))

    if args.apply:
        record = {
            "applied": __import__("datetime").datetime.now().isoformat(timespec="seconds"),
            "table": os.path.basename(TABLE),
            "table_sha1": _sha1_file(TABLE),
            "files_changed": changed,
            "tokens_replaced": replaced,
            "tokens_missing": missing,
            "tree_sha1": tree.hexdigest(),
        }
        with open(MANIFEST, "w", encoding="utf-8") as fh:
            json.dump(record, fh, indent=2, sort_keys=True)
            fh.write("\n")
        print("\n  manifest written: %s" % os.path.relpath(MANIFEST, DATA))
    return 0


if __name__ == "__main__":
    sys.exit(main())
