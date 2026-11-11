"""Can the CLIENT draw the lines in a script?

One question, no server and no client: for every locale token in a `.scp`, does
the client's own dictionary hold that id? If it does not, the line will show as
a bare number - and that is a DATA fault. No packet work can fix it, because
the client has no string to draw.

This is the static half of "is it the packet or the script?". Run it BEFORE
you debug a dialog that looks wrong.

    python check_script_ids.py npc/genpc/shop_promotion.scp
    python check_script_ids.py npc/genpc/                 # a folder
    python check_script_ids.py --all --worst 20           # whole tree + ranking
    python check_script_ids.py --all --quiet              # just the totals

Exit code is 1 if any line cannot be drawn, 0 if every line can. That makes it
usable as a gate.
"""

import argparse
import os
import re
import sys

from ge_paths import GE_SCRIPT

TOKEN = re.compile(rb"<\$>(\d+)</>")


def _client_ids():
    import ge_locale
    return set(ge_locale.client_table())


def _scan(path, client_ids):
    """Return [(line_number, [ids], drawable_count, missing_ids)] for one file."""
    with open(path, "rb") as fh:
        raw = fh.read()
    rows = []
    for n, line in enumerate(raw.splitlines(), 1):
        ids = [m.decode("ascii") for m in TOKEN.findall(line)]
        if not ids:
            continue
        missing = [i for i in ids if i not in client_ids]
        rows.append((n, ids, len(ids) - len(missing), missing))
    return rows


def _scp_files(target):
    if os.path.isdir(target):
        found = []
        for base, _dirs, names in os.walk(target):
            for name in names:
                if name.lower().endswith(".scp"):
                    found.append(os.path.join(base, name))
        return sorted(found)
    return [target]


def _show_file(path, client_ids, limit=40):
    rows = _scan(path, client_ids)
    total = sum(len(r[1]) for r in rows)
    missing = sum(len(r[3]) for r in rows)
    print("\n%s  (%d token(s) on %d line(s); %d cannot be drawn)"
          % (os.path.relpath(path), total, len(rows), missing))
    if not rows:
        print("   no locale tokens here - nothing for the client to draw")
        return total, missing
    shown = 0
    for n, ids, ok, bad in rows:
        if not bad:
            continue
        print("   line %-5d NUMBER   %s" % (n, ", ".join(bad)))
        shown += 1
        if shown >= limit:
            print("   ... (%d more lines)" % (len([r for r in rows if r[3]]) - shown))
            break
    if shown == 0:
        print("   every line can be drawn by the client")
    return total, missing


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("target", nargs="?", help="a .scp file or a folder")
    parser.add_argument("--all", action="store_true", help="every script under ge\\script")
    parser.add_argument("--worst", type=int, default=12,
                        help="with --all, how many files to rank (default 12)")
    parser.add_argument("--quiet", action="store_true", help="totals only")
    args = parser.parse_args(argv)

    if not args.all and not args.target:
        parser.error("give a file, a folder, or --all")

    client_ids = _client_ids()

    if args.all:
        files = _scp_files(GE_SCRIPT)
    else:
        files = _scp_files(args.target)
        if not files or not os.path.exists(files[0]):
            print("nothing found at %s" % args.target)
            return 2

    if not args.all and len(files) == 1:
        total, missing = _show_file(files[0], client_ids)
        print("\n  %d of %d token(s) cannot be drawn" % (missing, total))
        return 1 if missing else 0

    grand_total = 0
    grand_missing = 0
    ranked = []
    for path in files:
        rows = _scan(path, client_ids)
        tot = sum(len(r[1]) for r in rows)
        bad = sum(len(r[3]) for r in rows)
        grand_total += tot
        grand_missing += bad
        if bad:
            ranked.append((bad, tot, os.path.relpath(path, GE_SCRIPT)))

    print("scripts scanned       : %d" % len(files))
    print("tokens                : %d" % grand_total)
    print("the client can draw   : %d  (%.1f%%)"
          % (grand_total - grand_missing,
             100.0 * (grand_total - grand_missing) / grand_total if grand_total else 0))
    print("cannot be drawn       : %d  (%.1f%%)"
          % (grand_missing, 100.0 * grand_missing / grand_total if grand_total else 0))

    if ranked and not args.quiet:
        ranked.sort(reverse=True)
        print("\nthe files with the most lines the client cannot draw:")
        for bad, tot, rel in ranked[:args.worst]:
            print("   %5d / %-5d  %s" % (bad, tot, rel))
    return 1 if grand_missing else 0


if __name__ == "__main__":
    sys.exit(main())
