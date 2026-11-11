"""check_findings - does every finding under docker-port/history\\ carry a label?

The rule (AGENTS.md -> "How to write docs", copied from offlinespada): every
finding carries **PROVEN(scope)**, **INFERRED(scope)** or **GAP(scope)** - "a
claim with no label is not finished". This is the tool that makes the rule
observable instead of aspirational.

It walks `docker-port\\history\\*.md` and asks one question per `## ` / `### `
heading: does the finding that begins here carry a label? "Begins here" is the
heading line plus the first non-blank line under it. A label in the HEADING
counts (`### R57 - ... [PROVEN IN GAME]` is labelled): the rule is that the
finding carries a label, not where on the page it sits. A heading with no body
line at all is reported too - nothing follows it to carry a label.

IT WARNS; IT DOES NOT FAIL. The tree holds hundreds of headings written before
the rule existed, and this tool must not pretend they are wrong: the rule binds
NEW findings, and history pages are a record that is never retro-edited to
satisfy a checker. So the default output is the count plus the list, and the
exit status is 0. `--strict` is for a caller that wants the answer in CI terms.

The scope VOCABULARY is not policed. The six scopes in AGENTS.md are the
target, but the tree also carries honest variants - `(live PostgreSQL, ...)`,
`(shipped ge.exe 8bef2a66, live DB)`, `(our build 3732f622 plus a live barrack)`
- and rejecting those would measure spelling, not provenance. The number of
labelled findings that name no scope at all is printed as information only.

Not walked: `history\\plans-2026-10\\` (archived plans) and any `#### ` or
deeper heading (a sub-point inside a finding, not a finding). Neither is a
finding by this project's definition.

Usage:
    cd docker-port/src
    python check_findings.py              # warn: counts + the list, exit 0
    python check_findings.py --strict     # exit 1 when anything is unlabelled
    python check_findings.py --quiet      # the counts, not the list

Exit status: 0 by default, even with unlabelled headings - that is the point.
1 under --strict when any heading is unlabelled. 1 in either mode when a
history page cannot be read, because then the count itself is unknown.

Nothing runs at import: the work is in main(), so `import check_findings` (or
reusing scan_page) is free.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

# .../docker-port/src/check_findings.py -> the repo root
REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_HISTORY = Path("docker-port/history")

# A label is the bare word, not a substring of one (`PROVENANCE` must not pass,
# and neither must a word glued to an identifier). Case is exact: the rule and
# every page write them in capitals.
LABEL_RE = re.compile(r"(?<![A-Za-z0-9_])(PROVEN|INFERRED|GAP)(?![A-Za-z0-9_])")
# ... and a scope is a parenthesised clause straight after it.
SCOPE_RE = re.compile(r"(?<![A-Za-z0-9_])(PROVEN|INFERRED|GAP)[ \t]*\(")

FENCE = ("```", "~~~")

HEADING_PREFIXES = ("## ", "### ")

# One indented line per unlabelled heading, aligned. Long enough for
# `history/shipped-binaries.md:1073`, which is the widest real path.
WHERE_WIDTH = 42


class Heading:
    """One `##`/`###` heading and what the page puts under it."""

    __slots__ = ("where", "text", "body", "labelled", "scoped")

    def __init__(self, where: str, text: str, body: str) -> None:
        self.where = where
        self.text = text
        self.body = body
        self.labelled = False
        self.scoped = False


def scan_page(path: Path, label: str) -> list[Heading]:
    """Every heading in one page, with its verdict. Raises OSError if unreadable.

    newline=None (the default of read_text) folds CRLF to LF, so line numbers
    match what an editor shows. errors="replace" because a few shipped notes
    still carry CP949; a heading line is ASCII either way, and refusing to
    count a page over one odd byte would hide every finding on it.
    """
    lines = path.read_text(encoding="utf-8", errors="replace").split("\n")
    headings: list[Heading] = []
    in_fence = False

    for i, line in enumerate(lines):
        stripped = line.rstrip()
        if stripped.startswith(FENCE):
            in_fence = not in_fence
            continue
        # A `## ` inside a code block is sample text, not a section.
        if in_fence or not line.startswith(HEADING_PREFIXES):
            continue

        # The first body line: skip blank lines, stop at the next heading.
        j = i + 1
        while j < len(lines) and not lines[j].strip():
            j += 1
        body = lines[j].strip() if j < len(lines) else ""
        if body.startswith("#"):
            body = ""  # nothing but the next heading follows: no finding at all

        h = Heading(f"{label}:{i + 1}", line.strip(), body)
        haystack = line + "\n" + body
        h.labelled = LABEL_RE.search(haystack) is not None
        h.scoped = SCOPE_RE.search(haystack) is not None
        headings.append(h)

    return headings


def scan(history: Path) -> tuple[list[Heading], list[str]]:
    """Scan every history/*.md page. Returns (headings, unreadable pages)."""
    headings: list[Heading] = []
    unreadable: list[str] = []
    for path in sorted(history.glob("*.md")):
        # `history/<page>.md` for the real tree; whatever the folder is called
        # when --history points elsewhere, so a probe never claims to be history.
        label = f"{history.name}/{path.name}"
        try:
            headings.extend(scan_page(path, label))
        except OSError as exc:
            unreadable.append(f"{label}: {exc}")
    return headings, unreadable


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Count labelled vs unlabelled findings in docker-port/history/*.md."
    )
    parser.add_argument(
        "--history",
        default=str(DEFAULT_HISTORY),
        help=f"history folder, relative to the repo root (default: {DEFAULT_HISTORY})",
    )
    parser.add_argument(
        "--root",
        default=str(REPO_ROOT),
        help=f"repo root (default: {REPO_ROOT})",
    )
    parser.add_argument(
        "--strict",
        action="store_true",
        help="exit 1 when any heading is unlabelled (default: warn and exit 0)",
    )
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="print the counts, not the list of headings",
    )
    args = parser.parse_args(argv)

    history = Path(args.history)
    if not history.is_absolute():
        history = Path(args.root) / history
    if not history.is_dir():
        print(f"FAIL history folder does not exist: {history}")
        return 1

    headings, unreadable = scan(history)
    for line in unreadable:
        print(f"FAIL cannot read {line}")

    labelled = [h for h in headings if h.labelled]
    unlabelled = [h for h in headings if not h.labelled]
    scoped = sum(1 for h in labelled if h.scoped)

    print(
        f"findings: {len(headings)} headings, {len(labelled)} labelled, "
        f"{len(unlabelled)} unlabelled "
        f"(new ones must carry a label - AGENTS.md)"
    )
    if labelled:
        print(
            f"          {scoped} of {len(labelled)} labelled name a scope in "
            f"parentheses (information only - the scope words are not policed)"
        )

    if unlabelled and not args.quiet:
        print("")
        for h in unlabelled:
            note = "" if h.body else "  (nothing under the heading)"
            print(f"  {h.where:<{WHERE_WIDTH}} {h.text}{note}")

    if unreadable:
        print(f"check_findings: {len(headings)} headings, {len(unreadable)} unreadable page(s)")
        return 1

    if unlabelled:
        if args.strict:
            print(f"FAIL {len(unlabelled)} unlabelled headings (--strict)")
            return 1
        print(
            f"WARN {len(unlabelled)} unlabelled headings - the rule binds NEW findings, "
            f"so this is a count, not a failure (--strict makes it one)"
        )
        return 0

    print("OK every heading carries a label")
    return 0


if __name__ == "__main__":
    sys.exit(main())
