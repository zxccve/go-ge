"""check_idspaces - does every data-table NAME a script asks for exist?

Usage:
    python check_idspaces.py          # report, exit 1 on a missing idspace
    python check_idspaces.py --alias  # also list names only found by alias

WHY THIS EXISTS: an idspace is not the file name (`datatable_missionenter.ies`
is idspace `Mission_Rank`), so `GetDataTableNumberValue('Mission_Rank', ...)`
answered 0 for 117 call sites without one error. A name typo, or a table that
was never shipped, looks exactly the same: 0 everywhere. This compares the
names the scripts USE against the names the `.ies` headers CARRY.

Work is in `main()` so importing this does not run it.
"""
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import npc_data

CALL = re.compile(
    r"GetDataTable\w*\s*\(\s*['\"]([^'\"]+)['\"]", re.IGNORECASE)


def scan_scripts(root):
    """`{idspace: (calls, files)}` over every `.scp` in the script tree."""
    found = {}
    for base, _dirs, files in os.walk(root):
        for fname in files:
            if not fname.lower().endswith(".scp"):
                continue
            path = os.path.join(base, fname)
            try:
                with open(path, "r", encoding="utf-8", errors="replace") as f:
                    text = f.read()
            except OSError:
                continue
            names = CALL.findall(text)
            if not names:
                continue
            for name in set(names):
                calls, scripts = found.setdefault(name, [0, 0])
                found[name] = [calls + names.count(name), scripts + 1]
    return found


def main():
    import ge_paths
    root = ge_paths.GE_SCRIPT
    used = scan_scripts(root)
    stems = npc_data.idspace_stems()
    missing = []
    alias = []
    for name in sorted(used, key=lambda n: -used[n][0]):
        tables = npc_data.tables_for_idspace(name)
        calls, scripts = used[name]
        if not tables:
            missing.append((name, calls, scripts))
        elif tables != ["datatable_" + name.lower()]:
            alias.append((name, calls, scripts, tables))

    print("script tree: %s" % root)
    print("idspaces named by scripts: %d (headers carry %d)"
          % (len(used), len(stems)))
    print("missing: %d, resolved only through the alias: %d"
          % (len(missing), len(alias)))

    if "--alias" in sys.argv:
        for name, calls, scripts, tables in alias:
            print("  ALIAS %-26s %4d calls / %3d scripts -> %s"
                  % (name, calls, scripts, ", ".join(tables)))
    for name, calls, scripts in missing:
        print("  MISSING %-26s %4d calls / %3d scripts"
              % (name, calls, scripts))

    if missing:
        print()
        print("A missing idspace answers 0 for every row. Either the name is a\n"
              "typo or the table is not shipped; both are silent at runtime.")
        return 1
    print("every idspace a script asks for exists")
    return 0


if __name__ == "__main__":
    sys.exit(main())
