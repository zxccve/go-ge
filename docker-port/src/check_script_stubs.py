r"""Rank the builtins that are STILL stubs, by what the corpus actually calls.

Why: 2,447 distinct builtins appear in the scripts, and the top 11 cover half of
all call sites. Every one that is still a stub is a feature that silently does
nothing - a shop that will not open, a check that always answers 0. Fixing them
by NPC name is endless; fixing them by FREQUENCY is one job with a wide blast
radius.

How it decides what is a stub: `ScriptHost` answers any name it has not bound
through `_missing`, which records the call and returns 0. So anything callable in
the Lua globals is implemented (including the Lua helpers in the prelude), and
every OTHER name that got called is a stub.

The ranking is per NAME, with two numbers, because they answer different
questions:
  * calls   - how much traffic it is (cheap to fix, widely felt)
  * scripts - how many DIFFERENT NPCs/quests need it (breadth of the unlock)

Usage
-----
    python check_script_stubs.py                # every click script
    python check_script_stubs.py --top 40
    python check_script_stubs.py --script C1_REB_LINDEN   # just one
"""

import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)


def click_scripts():
    """Every `SCR_CLICK_<name>` the host can load, as bare script names."""
    import script_host
    host = script_host.ScriptHost(zone_id=0)
    names = []
    for func in host.index():
        if func.startswith("SCR_CLICK_"):
            names.append(func[len("SCR_CLICK_"):])
    return sorted(names)


def run(top=30, only=None):
    import script_host

    host = script_host.ScriptHost(zone_id=0)
    host.commander_id = None            # no DB: the bag stays empty and local
    glob = host.lua.globals()
    implemented = {name for name in glob.keys() if callable(glob[name])}

    scripts = [only] if only else click_scripts()
    calls = {}          # name -> call sites, CAPPED per script
    users = {}          # name -> set of scripts
    errors = []
    dialogs = 0
    CAP = 20            # a `while(1)` can call one name millions of times; that
    #                     is the loop guard, not usage. Cap each script's
    #                     contribution so the ranking measures breadth.
    for i, script in enumerate(scripts, 1):
        before = len(host.calls)
        try:
            res = host.run_click(script)
            if res.get("dialog"):
                dialogs += 1
            if not res.get("ok"):
                errors.append((script, res.get("error")))
        except Exception as exc:        # one bad script must not stop the sweep
            errors.append((script, str(exc)))
        per_script = {}
        for call in host.calls[before:]:
            if call.name in implemented:
                continue
            per_script[call.name] = per_script.get(call.name, 0) + 1
        for name, count in per_script.items():
            calls[name] = calls.get(name, 0) + min(count, CAP)
            users.setdefault(name, set()).add(script)
        if i % 500 == 0:
            print("  ..   %d/%d scripts" % (i, len(scripts)))

    print("scripts: %d, dialogs: %d, errors: %d, distinct stubs: %d"
          % (len(scripts), dialogs, len(errors), len(calls)))
    total = sum(calls.values())
    print("stub call sites: %d" % total)
    print()
    ranked = sorted(calls.items(), key=lambda kv: (-len(users[kv[0]]), -kv[1]))
    print("%-34s %8s %8s %6s" % ("builtin (still a stub)", "scripts", "calls", "share"))
    for name, count in ranked[:top]:
        print("%-34s %8d %8d %5.1f%%"
              % (name, len(users[name]), count, 100.0 * count / total))
    if errors:
        print()
        print("first errors:")
        for script, err in errors[:5]:
            print("  %-30s %s" % (script, (err or "")[:90]))
        # Grouped counts: a NEW error after a change is worth more than the raw
        # total. Script-specific text (file paths, numbers) is stripped so one
        # class of failure counts once.
        kinds = {}
        for script, err in errors:
            key = re.sub(r"[0-9]+", "N", re.sub(r"[A-Za-z]:[\\/][^\s\"\']*?\.scp", "<file>", str(err)))
            key = re.sub(r"\[string \"[^\"]+\"\]", "[string]", key)[:70]
            kinds.setdefault(key, []).append(script)
        print()
        print("errors by kind:")
        for key, scripts in sorted(kinds.items(), key=lambda kv: -len(kv[1]))[:12]:
            print("  %4d  %-58s  e.g. %s" % (len(scripts), key, scripts[0]))
    return 0


def main(argv):
    top = 30
    only = None
    if "--top" in argv:
        at = argv.index("--top")
        if at + 1 < len(argv) and argv[at + 1].isdigit():
            top = int(argv[at + 1])
    if "--script" in argv:
        at = argv.index("--script")
        if at + 1 < len(argv):
            only = argv[at + 1]
    return run(top, only)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
