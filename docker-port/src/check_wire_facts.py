"""Check docker-port/data/wire_facts.toml against the files it cites.

Every fact is a claim plus the artifacts that prove it. This checker exists so
a stored fact cannot quietly rot: when a cited source is edited, moved or
deleted, the fact fails here instead of being believed by the next session.

Two evidence shapes are accepted:

    { path = "...", lines = "N" or "N-M", quote = "..." }
        A text artifact. `lines` is 1-based and inclusive, must lie inside the
        file, and `quote` must be a verbatim substring of exactly those lines
        (joined with newlines). This is the same contract as the reference
        store (offlinespada-main/docs/facts/protocol.toml).

    { path = "...", sha256 = "..." }
        A binary artifact (capture, packet dump, executable). The file must
        exist and its SHA-256 must match, so a capture cannot be replaced
        silently.

The `doc` field must name a docker-port/history/*.md page and `doc_quote` must
be a verbatim substring of that page. A PROVEN fact must carry at least one
evidence entry (the artifact that records the observation), never prose alone.

Usage:
    python docker-port/src/check_wire_facts.py
    python docker-port/src/check_wire_facts.py --facts other.toml

Exit status: 0 when every fact checks, 1 when anything fails. Each failure is
one line, prefixed with the fact id, so a CI wrapper can grep it.
"""

from __future__ import annotations

import argparse
import hashlib
import re
import sys
import tomllib
from pathlib import Path

# .../docker-port/src/check_wire_facts.py -> the repo root
REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_FACTS = Path("docker-port/data/wire_facts.toml")
HISTORY_PREFIX = "docker-port/history/"

ID_RE = re.compile(r"^[a-z][a-z0-9._-]*$")
SHA_RE = re.compile(r"^[0-9a-f]{64}$")
LINES_RE = re.compile(r"^([0-9]+)(?:-([0-9]+))?$")
STATUSES = ("PROVEN", "INFERRED")


def read_text(path: Path) -> str:
    """Read a cited text file the way an editor sees it.

    errors="replace" is deliberate: a few shipped sources still carry CP949
    comments, and the ASCII lines a fact quotes are unaffected. newline=None
    (the default) folds CRLF to LF, so `lines` matches what an editor shows.
    """
    return path.read_text(encoding="utf-8", errors="replace")


def check_evidence(fact_id: str, ev: object, root: Path, failures: list[str]) -> None:
    if not isinstance(ev, dict):
        failures.append(f"{fact_id}: evidence entry is not a table")
        return

    rel = ev.get("path")
    if not isinstance(rel, str) or not rel:
        failures.append(f"{fact_id}: evidence entry has no path")
        return
    if Path(rel).is_absolute() or ".." in Path(rel).parts:
        failures.append(f"{fact_id}: evidence path must be repo-relative, got {rel!r}")
        return

    path = root / rel
    if not path.is_file():
        failures.append(f"{fact_id}: evidence path does not exist: {rel}")
        return

    linespec = ev.get("lines")
    digest = ev.get("sha256")

    if linespec is not None:
        m = LINES_RE.match(linespec) if isinstance(linespec, str) else None
        if m is None:
            failures.append(f"{fact_id}: bad lines value {linespec!r} for {rel}")
            return
        start = int(m.group(1))
        end = int(m.group(2) or start)
        if start < 1 or end < start:
            failures.append(f"{fact_id}: bad line range {linespec!r} for {rel}")
            return

        lines = read_text(path).split("\n")
        if end > len(lines):
            failures.append(
                f"{fact_id}: lines {linespec} outside {rel} ({len(lines)} lines)"
            )
            return

        quote = ev.get("quote")
        if not isinstance(quote, str) or not quote:
            failures.append(f"{fact_id}: {rel}:{linespec} has no quote")
            return

        window = "\n".join(lines[start - 1 : end])
        if quote not in window:
            failures.append(
                f"{fact_id}: quote not found at {rel}:{linespec}: {quote!r}"
            )
        return

    if digest is not None:
        if not isinstance(digest, str) or SHA_RE.match(digest) is None:
            failures.append(f"{fact_id}: bad sha256 value for {rel}")
            return
        actual = hashlib.sha256(path.read_bytes()).hexdigest()
        if actual != digest:
            failures.append(
                f"{fact_id}: sha256 mismatch for {rel} (want {digest}, got {actual})"
            )
        return

    failures.append(f"{fact_id}: evidence entry needs lines+quote or sha256: {rel}")


def check_fact(fact: object, root: Path, seen: set[str], failures: list[str]) -> None:
    if not isinstance(fact, dict):
        failures.append("<unknown>: [[fact]] is not a table")
        return

    fact_id = fact.get("id")
    if not isinstance(fact_id, str) or ID_RE.match(fact_id) is None:
        failures.append(f"<unknown>: bad or missing id {fact_id!r}")
        return
    if fact_id in seen:
        failures.append(f"{fact_id}: duplicate id")
        return
    seen.add(fact_id)

    claim = fact.get("claim")
    if not isinstance(claim, str) or not claim.strip():
        failures.append(f"{fact_id}: missing claim")

    status = fact.get("status")
    if status not in STATUSES:
        failures.append(f"{fact_id}: status must be one of {STATUSES}, got {status!r}")

    builds = fact.get("builds")
    if (
        not isinstance(builds, list)
        or not builds
        or not all(isinstance(b, str) and b for b in builds)
    ):
        failures.append(f"{fact_id}: builds must be a non-empty list of strings")

    evidence = fact.get("evidence")
    if not isinstance(evidence, list) or not evidence:
        failures.append(f"{fact_id}: evidence must be a non-empty list")
        evidence = []
    for ev in evidence:
        check_evidence(fact_id, ev, root, failures)

    doc = fact.get("doc")
    doc_quote = fact.get("doc_quote")
    if not isinstance(doc, str) or not doc:
        failures.append(f"{fact_id}: missing doc")
        return
    if not doc.startswith(HISTORY_PREFIX) or not doc.endswith(".md"):
        failures.append(f"{fact_id}: doc must be a {HISTORY_PREFIX}*.md page, got {doc!r}")
        return

    doc_path = root / doc
    if not doc_path.is_file():
        failures.append(f"{fact_id}: doc file does not exist: {doc}")
        return
    if not isinstance(doc_quote, str) or not doc_quote:
        failures.append(f"{fact_id}: missing doc_quote for {doc}")
        return
    if doc_quote not in read_text(doc_path):
        failures.append(f"{fact_id}: doc_quote not found in {doc}: {doc_quote!r}")


def load_facts(path: Path, failures: list[str]) -> list:
    if not path.is_file():
        failures.append(f"facts file does not exist: {path}")
        return []
    try:
        with path.open("rb") as fh:
            data = tomllib.load(fh)
    except tomllib.TOMLDecodeError as exc:
        failures.append(f"facts file is not valid TOML: {exc}")
        return []

    if data.get("schema_version") != 1:
        failures.append(
            f"schema_version must be 1, got {data.get('schema_version')!r}"
        )
    facts = data.get("fact")
    if not isinstance(facts, list) or not facts:
        failures.append("facts file has no [[fact]] tables")
        return []
    return facts


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Check the wire facts store.")
    parser.add_argument(
        "--facts",
        default=str(DEFAULT_FACTS),
        help=f"facts file, relative to the repo root (default: {DEFAULT_FACTS})",
    )
    parser.add_argument(
        "--root",
        default=str(REPO_ROOT),
        help=f"repo root the cited paths are relative to (default: {REPO_ROOT})",
    )
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    facts_path = Path(args.facts)
    if not facts_path.is_absolute():
        facts_path = root / facts_path

    failures: list[str] = []
    facts = load_facts(facts_path, failures)
    seen: set[str] = set()
    for fact in facts:
        check_fact(fact, root, seen, failures)

    for line in failures:
        print(f"FAIL {line}")
    if failures:
        print(f"check_wire_facts: {len(facts)} facts, {len(failures)} failure(s)")
        return 1

    print(f"OK wire-facts: {len(facts)} facts, all evidence and doc quotes verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
