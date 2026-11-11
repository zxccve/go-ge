r"""prepare - refresh what is DERIVED, or say what is stale without writing.

Run from src\:

    cd src
    python prepare.py            # refresh ONLY what is stale
    python prepare.py --check    # write nothing; FRESH / STALE per target;
                                 # exit 1 if anything is stale, 0 if all fresh

Why this file exists
--------------------
Three things in this tree are generated from the game's own data: the Lua
corpus under `out\lua_corpus`, `data\item_props.json` and
`data\item_stats.json`. A stale one is not an error - it is silently wrong
numbers, and until now the only guard was somebody REMEMBERING to re-run the
emitter. An `item_props.json` emitted before a table changed still answers
every question, just with the old values.

So the table below is the one place a derived file is declared, and this file
answers the only question that matters: is the output still newer than its
sources?

    python prepare.py --check     a lane and a session start run this
    doctor.py                     calls check() for its one-line summary
    python prepare.py             fixes exactly what the check found

The check compares mtimes only. It never re-emits to find out: a re-emit is
tens of seconds (82 MB of item xml, 4,239 scripts), which would make the check
itself the slowest step of a start sequence.

Nothing runs at import - doctor.py imports check(), so main() guards the work.
"""

import argparse
import glob
import os
import subprocess
import sys

import ge_paths

SRC = os.path.dirname(os.path.abspath(__file__))
ROOT = ge_paths.ROOT                      # docker-port: the OUTPUTS live here
GAMESRC = os.path.dirname(ROOT)           # the game tree (`ge\`) - inputs


# ---------------------------------------------------------------------------
# THE TABLE: (output, [input paths or globs], command_argv).
#
#   * the OUTPUT is relative to docker-port; an INPUT is relative to
#     the game tree root, because that is where the game's `ge\` tree is (the same
#     split `ge_paths.py` makes).
#   * a trailing separator on the output means a TREE - one output file per
#     input file, mirroring the relative path under the tree root (the folder
#     named before `**`). A tree is compared PER FILE: an output whose input is
#     newer, or that does not exist yet, is stale.
#   * any other output is ONE file, stale when its newest input is newer.
#   * command_argv is the whole-target rebuild, run from src\. It is what runs
#     for a tree whose output folder is gone entirely. For a partly stale tree
#     this file translates only the stale scripts, with the same translator
#     function `--dump` uses (scp_to_lua.translate_file) - otherwise one
#     changed script would cost a 4,239-file rewrite.
#
# Add a row here; nothing else in this file needs to learn about the new file.
# ---------------------------------------------------------------------------
ITEM_XML = ["ge\\xml\\datatable_item*.xml", "ge\\xml\\datatable_growing_item.xml"]

DERIVED = [
    ("out\\lua_corpus\\", ["ge\\script\\**\\*.scp"],
     ["scp_to_lua.py", "--dump", "../out/lua_corpus"]),
    ("data\\item_props.json", ITEM_XML, ["emit_item_props.py"]),
    ("data\\item_stats.json", ITEM_XML, ["emit_item_stats.py"]),
]


def _abs(rel):
    """An OUTPUT path, relative to docker-port."""
    return os.path.join(ROOT, rel)


def _input_abs(rel):
    """An INPUT path, relative to <path> (where `ge\\` lives)."""
    return os.path.join(GAMESRC, rel)


def _show(path):
    """A path the way the owner sees it: relative to docker-port, backslash."""
    return os.path.relpath(path, ROOT).replace("/", "\\")


def _commas(number):
    return "{:,}".format(number)


def _is_tree(out):
    return out.endswith("\\") or out.endswith("/")


def _glob(pattern):
    return glob.glob(_input_abs(pattern), recursive=True)


def _inputs(patterns):
    """Every existing file the patterns name."""
    found = []
    for pattern in patterns:
        found.extend(p for p in _glob(pattern) if os.path.isfile(p))
    return found


def _tree_root(pattern):
    """The folder the mirrored tree hangs from: the part before `**`."""
    norm = pattern.replace("/", "\\")
    head = norm.split("**")[0] if "**" in norm else os.path.dirname(norm)
    return _input_abs(head.rstrip("\\"))


def _tree_stale(out_dir, patterns):
    """[(source, target)] for every input whose .lua is missing or older."""
    pairs = []
    for pattern in patterns:
        root = _tree_root(pattern)
        for src in _glob(pattern):
            rel = os.path.relpath(src, root)
            dst = os.path.join(out_dir, os.path.splitext(rel)[0] + ".lua")
            if not os.path.isfile(dst) or os.path.getmtime(src) > os.path.getmtime(dst):
                pairs.append((src, dst))
    return pairs


def _file_stale(out, patterns):
    """(is_stale, why) for one single-file target. why is '' when fresh."""
    path = _abs(out)
    inputs = _inputs(patterns)
    if not os.path.isfile(path):
        return True, "missing"
    if not inputs:
        return True, "no source files matched the glob"
    newest = max(inputs, key=os.path.getmtime)
    age = os.path.getmtime(newest) - os.path.getmtime(path)
    if age > 0:
        return True, "source newer by " + _age(age)
    return False, ""


def _age(seconds):
    """How far behind: 2d 4h, 3h 12m, 4m, 9s."""
    seconds = int(max(seconds, 0))
    if seconds >= 86400:
        return "%dd %dh" % (seconds // 86400, (seconds % 86400) // 3600)
    if seconds >= 3600:
        return "%dh %dm" % (seconds // 3600, (seconds % 3600) // 60)
    if seconds >= 60:
        return "%dm" % (seconds // 60)
    return "%ds" % seconds


def check(verbose=True):
    """Report every derived target. Writes nothing.

    Returns (stale_count, summary) - the summary is what doctor.py prints as
    one line: `all fresh` or `2 stale - run: python prepare.py`.
    """
    stale = 0
    for out, patterns, _argv in DERIVED:
        label = out.rstrip("\\/")
        if _is_tree(out):
            pairs = _tree_stale(_abs(out), patterns)
            total = len(_inputs(patterns))
            if not total:
                stale += 1
                if verbose:
                    print("STALE  %s (no source files matched the glob)" % label)
            elif pairs:
                stale += 1
                if verbose:
                    print("STALE  %s (%s of %s .lua behind: %s)"
                          % (label, _commas(len(pairs)), _commas(total),
                             _tree_names(out, pairs)))
            elif verbose:
                print("FRESH  %s (%s file(s))" % (label, _commas(total)))
        else:
            is_stale, why = _file_stale(out, patterns)
            if is_stale:
                stale += 1
                if verbose:
                    print("STALE  %s (%s)" % (label, why))
            elif verbose:
                print("FRESH  %s" % label)
    summary = "all fresh" if not stale else \
        "%d stale - run: python prepare.py" % stale
    return stale, summary


def _tree_names(out, pairs, limit=5):
    base = _abs(out.rstrip("\\/"))
    names = [os.path.relpath(dst, base).replace("/", "\\")
             for _src, dst in pairs[:limit]]
    more = len(pairs) - len(names)
    text = ", ".join(names)
    if more:
        text += " (+%d more)" % more
    return text


def _translate(pairs):
    """Translate exactly these (source, target) pairs.

    Returns (done, skipped, written) - `written` is [(source, target,
    closers_added)] for the pairs that really landed on disk, which is what the
    balance check below is allowed to speak about. `closers_added` is what the
    translator had to invent to close the file, so it is the exact number of
    blocks the SOURCE left open.
    """
    import scp_to_lua            # here, not at the top: --check never needs it
    done = skipped = 0
    written = []
    for src, dst in pairs:
        try:
            text, closers = scp_to_lua.translate_file_report(src)
        except Exception as exc:              # a file the translator rejects
            skipped += 1
            print("SKIP %s: %s" % (_show(src), exc))
            continue
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        with open(dst, "w", encoding="utf-8") as fh:
            fh.write(text)
        done += 1
        written.append((src, dst, closers))
    return done, skipped, written


def _balance(written):
    """Report the scripts THIS run translated whose blocks do not balance.

    WHY: the translator AUTO-CLOSES an unfinished block, so a cut-off .scp comes
    out as VALID Lua and the Go compile check (script\\host.go) cannot see it.
    The counts are therefore taken from the .scp SOURCE, with
    scp_to_lua.balance_source() - `{` versus `}` is the dialect's block
    delimiter. Counting Lua keywords on .scp text (the function that used to do
    this) could never fire: measured 2026-11-03, a .scp cut off mid-expression
    after 3 openers scored 0 openers and 0 ends, i.e. "balanced".

    The better signal is reported too: the translator TELLS us how many closers
    it synthesised for each file, and one or more of those IS a source that left
    a block open. A file is named when either signal fires.

    MEASURED EXCEPTION (2026-11-03): exactly one shipped file makes the
    translator synthesise a closer while its braces balance - item\\item.scp,
    braces 4,961/4,961 - because the translator armed a BLOCK for one `{` it
    should have called a table (a braceless-body quirk; the block stays on its
    stack to the end of the file). So a whole-corpus rebuild names that one file
    and it is not cut off. Every other shipped script is clean on both signals.

    WHICH FILES: only `written`. The shipped corpus is brace-balanced (0 of
    4,239 measured 2026-11-03), so an unconditional walk would be noise; the
    seven-ish files the OLD keyword count flagged were coincidence.

    UNBALANCED IS A WARNING, never an error: the exit code stays 0 and the zone
    starts. The Go side turns these lines into boot warnings.
    """
    if not written:
        return
    import scp_to_lua
    bad = []
    for src, _dst, closers in written:
        try:
            with open(src, encoding="latin-1") as fh:
                source = fh.read()
        except OSError:
            continue                          # a file we cannot read is not ours to judge
        openers, ends = scp_to_lua.balance_source(source)
        if closers is None:                   # whole-corpus rebuild (a child ran it)
            closers = scp_to_lua.translate_report(source)[1]
        if openers == ends and not closers:
            continue
        bad.append((src, openers, ends, closers))
    for src, openers, ends, closers in bad:
        print("UNBALANCED %s (openers %d, ends %d, %d closer(s) added "
              "automatically)" % (_script_rel(src), openers, ends, closers))
    print("BALANCE %d of %d translated script(s) unbalanced"
          % (len(bad), len(written)))


def _script_rel(path):
    """A .scp path the way scp_to_lua --check names it: relative to ge\\script."""
    import scp_to_lua
    try:
        return os.path.relpath(path, scp_to_lua.GE_SCRIPT).replace("/", "\\")
    except ValueError:                        # a different drive: keep it whole
        return path


def _run(argv):
    """Run a declared emitter from src\\; on failure show its own words."""
    p = subprocess.run([sys.executable] + argv, cwd=SRC,
                       capture_output=True, text=True)
    if p.returncode != 0:
        sys.stderr.write((p.stdout or "") + (p.stderr or ""))
        sys.exit("prepare: %s failed (exit %d)" % (argv[0], p.returncode))


def refresh():
    """Do only the work that is needed, in table order. Returns actions taken."""
    actions = 0
    for out, patterns, argv in DERIVED:
        if _is_tree(out):
            out_dir = _abs(out)
            pairs = _tree_stale(out_dir, patterns)
            total = len(_inputs(patterns))
            if not total:
                print("SKIP %s: no input files matched %s"
                      % (out.rstrip("\\/"), ", ".join(patterns)))
                actions += 1
                continue
            if not pairs:
                continue
            if os.path.isdir(out_dir):
                count, skipped, written = _translate(pairs)
                print("TRANSLATED %s script(s)" % _commas(count))
                if skipped:
                    print("%d script(s) skipped - the translator rejected them "
                          "above" % skipped)
            else:
                # The whole tree is gone: the declared rebuild is the right call.
                _run(argv)
                print("TRANSLATED %s script(s) (whole corpus - the output "
                      "folder was missing)" % _commas(total))
                # A child did the translating, so the per-file closer counts were
                # never ours to see. None says "ask the translator again"; only
                # this rare path pays for it (see _balance).
                written = [(src, dst, None) for src, dst in pairs]
            _balance(written)
            actions += 1
        else:
            is_stale, why = _file_stale(out, patterns)
            if not is_stale:
                continue
            _run(argv)
            print("EMITTED %s (%s)" % (out.rstrip("\\/"), why))
            actions += 1
    if not actions:
        print("nothing to do")
    return actions


def main(argv=None):
    ap = argparse.ArgumentParser(
        description="Refresh the derived files, or say what is stale.")
    ap.add_argument("--check", action="store_true",
                    help="write nothing; FRESH/STALE per target; "
                         "exit 1 if any is stale")
    args = ap.parse_args(argv)

    if args.check:
        stale, summary = check()
        print(summary)
        return 1 if stale else 0
    refresh()
    return 0


if __name__ == "__main__":
    sys.exit(main())
