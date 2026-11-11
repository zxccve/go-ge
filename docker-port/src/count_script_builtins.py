"""Measure the .scp corpus: which builtin commands do the scripts really call?

Why: `history\\script-engine.md` guessed "start with 20-30 builtins". The parser
is the cheap half (the dialect is Lua - see `history\\script-dialect.md`); the
builtins are the real work. This counts them so the size of the job is a number,
not a guess.

Writes `data\\script_builtins.txt` and prints a summary.

    python count_script_builtins.py            # whole corpus
    python count_script_builtins.py --top 60   # longer console table
"""

import argparse
import collections
import os
import re
import sys

from ge_paths import DATA, GE_SCRIPT

OUT_FILE = os.path.join(DATA, "script_builtins.txt")

# The dialect's own keywords, from source2\\imcIES\\lua\\llex.cpp token2string.
KEYWORDS = {
    "break", "do", "else", "end", "false", "for", "func", "if", "in",
    "var", "null", "nil", "repeat", "return", "then", "true", "until", "while",
}

# `func NAME(` ... names may be dotted (`func table.contains(`).
DEF_RE = re.compile(r"\bfunc\s+([A-Za-z_][\w.]*)\s*\(")
# A call: NAME( where the name does not follow a dot (that would be a method).
CALL_RE = re.compile(r"(?<![\w.])([A-Za-z_]\w*)\s*\(")
CLICK_DEF_RE = re.compile(r"\bfunc\s+(SCR_CLICK_\w+)\s*\(")


def strip_noise(text):
    """Blank out comments and string bodies, keeping offsets and newlines.

    Length is preserved so a brace counted later still lines up.
    """
    out = []
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c in "\"'":
            q = c
            out.append(q)
            i += 1
            while i < n:
                ch = text[i]
                if ch == "\\" and i + 1 < n:
                    out.append("  ")
                    i += 2
                    continue
                out.append(ch if ch == "\n" else " ")
                i += 1
                if ch == q:
                    break
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "/":
            while i < n and text[i] != "\n":
                out.append(" ")
                i += 1
            continue
        if c == "/" and i + 1 < n and text[i + 1] == "*":
            while i < n and not (text[i] == "*" and i + 1 < n and text[i + 1] == "/"):
                out.append("\n" if text[i] == "\n" else " ")
                i += 1
            out.append("  ")
            i += 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


def func_spans(clean):
    """{name: body text} for every `func` in a cleaned source."""
    spans = {}
    for m in DEF_RE.finditer(clean):
        start = clean.find("{", m.end())
        if start < 0:
            continue
        depth, i = 0, start
        while i < len(clean):
            if clean[i] == "{":
                depth += 1
            elif clean[i] == "}":
                depth -= 1
                if depth == 0:
                    break
            i += 1
        spans[m.group(1)] = clean[start:i + 1]
    return spans


def calls_in(text):
    return [c for c in CALL_RE.findall(text) if c not in KEYWORDS]


def scan(roots=None):
    """Walk the script trees. Returns (call_counts, file_counts, click_counts)."""
    call_counts = collections.Counter()
    file_counts = collections.Counter()
    click_counts = collections.Counter()
    defined = set()
    files = []

    for root in roots or [GE_SCRIPT]:
        for dirpath, _dirs, names in os.walk(root):
            for name in names:
                if name.endswith(".scp"):
                    files.append(os.path.join(dirpath, name))

    for path in files:
        try:
            with open(path, encoding="latin-1") as fh:
                clean = strip_noise(fh.read())
        except OSError:
            continue
        spans = func_spans(clean)
        defined.update(spans)
        seen = set()
        for name in calls_in(clean):
            call_counts[name] += 1
            seen.add(name)
        for name in seen:
            file_counts[name] += 1
        for fname, body in spans.items():
            if fname.startswith("SCR_CLICK_"):
                for name in calls_in(body):
                    if name not in spans:
                        click_counts[name] += 1

    builtins = {k: v for k, v in call_counts.items() if k not in defined}
    return builtins, file_counts, click_counts, len(files), defined


def coverage(counts):
    """How many builtins cover 50/80/90/95/99% of all call sites."""
    total = sum(counts.values()) or 1
    rows = []
    for mark in (50, 80, 90, 95, 99, 100):
        run, need = 0, 0
        for i, (_name, n) in enumerate(sorted(counts.items(), key=lambda kv: -kv[1]), 1):
            run += n
            if run * 100 >= total * mark:
                need = i
                break
        rows.append((mark, need, run * 100 // total))
    return rows


def main():
    ap = argparse.ArgumentParser(description="Count builtin calls in the .scp corpus.")
    ap.add_argument("--top", type=int, default=40, help="rows in the console table")
    args = ap.parse_args()

    builtins, file_counts, click_counts, nfiles, defined = scan()
    if not builtins:
        print("no scripts found under", GE_SCRIPT)
        return 1

    total = sum(builtins.values())
    ranked = sorted(builtins.items(), key=lambda kv: -kv[1])

    lines = [
        "# Builtin calls in ge\\script, measured (count_script_builtins.py)",
        f"files scanned      : {nfiles}",
        f"functions defined  : {len(defined)}",
        f"call sites         : {total}",
        f"distinct builtins  : {len(builtins)}",
        "",
        "# coverage: N builtins cover X% of all call sites",
    ]
    for mark, need, pct in coverage(builtins):
        lines.append(f"  top {need:>4} builtins = {pct:>3}% of call sites (target {mark}%)")

    lines += ["", "# top builtins: calls  files  in-click-scripts"]
    for name, n in ranked:
        lines.append(f"{n:>8} {file_counts[name]:>6} {click_counts.get(name, 0):>6}  {name}")

    os.makedirs(DATA, exist_ok=True)
    with open(OUT_FILE, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")

    print(f"files {nfiles}  functions {len(defined)}  call sites {total}  "
          f"distinct builtins {len(builtins)}")
    for mark, need, pct in coverage(builtins):
        print(f"  top {need:>4} builtins = {pct:>3}% of call sites")
    print()
    print(f"{'calls':>8} {'files':>6} {'click':>6}  builtin")
    for name, n in ranked[:args.top]:
        print(f"{n:>8} {file_counts[name]:>6} {click_counts.get(name, 0):>6}  {name}")
    print()
    print("written:", OUT_FILE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
