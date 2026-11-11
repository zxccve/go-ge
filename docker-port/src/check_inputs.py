"""check_inputs - are the artefacts this project is BUILT FROM still the ones we measured?

`docker-port\\data\\inputs.json` names every artefact the project stands on and
records the hash of the files that make it THAT artefact. This tool compares
the record against the disk: a path that is gone, a file whose bytes changed, a
tree that lost half its files. One line per failure, exit 1.

Why a hash and not a note: the whole reverse-engineering corpus is claims about
specific bytes - `ge.exe` 8bef2a66, the rev136 tables, the old rev318 rows used
as a cross-check. If the file under a claim is replaced, every finding in the
tree silently becomes a claim about something else. This is the alarm.

What is hashed, and what is deliberately not
--------------------------------------------
The three installs are ~57 GB and ~78,000 files between them. Hashing that
would take minutes and prove only that a disk is still a disk. So a DIRECTORY
entry records a per-file map over the handful of files that identify the build
(the revision marker, the executable, the decoder the table build imports) and
a `tree` census of file count and total bytes. Nothing is hashed that the
manifest does not name, and every entry's `note` says in words what was left
out and why. A FILE entry records its one digest.

The manifest is the record; this tool never guesses
---------------------------------------------------
`--record` rewrites the hashes from the disk, line by line, and is a deliberate
act after a deliberate change (a refreshed client, a rebuilt `ge\\xml`). It
REFUSES to bless a path that is missing - the entry keeps its old hash and the
run exits 1 - because "the input vanished" is the one thing this tool exists to
report.

Schema, `schema_version` 1
--------------------------
    {"schema_version": 1, "recorded": "YYYY-MM-DD", "note": "...",
     "input": [entry, ...]}

Each entry is a table with these keys:

    name          short id, unique, ASCII
    path          repo-relative (<path>); no absolute path, no ".."
    kind          install | source | data | derived
    build         the build the artefact is: rev136 | rev318 | v28 ...
    sha256        FILE entry: one 64-hex digest
                  DIRECTORY entry: {relative path: digest} - never one hash
    tree          DIRECTORY entry only: {"files": n, "bytes": n}
    derived_from  REQUIRED when kind == "derived": the paths it is made from.
                  Each must exist, so a derived file can never be mistaken for
                  an input and can never be the only copy of its own source.
    feeds         what reads it (the consumer)
    note          what it is; what was not hashed and why

Usage:
    cd docker-port/src
    python check_inputs.py               # check the record; exit 1 on any failure
    python check_inputs.py --record      # (re)write the hashes from disk
    python check_inputs.py --manifest other.json --root <path>

Exit status: 0 when every recorded path exists and every hash matches, 1 when
anything does not (including an unreadable or invalid manifest).

Nothing runs at import - doctor.py or a lane script may import check().
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import re
import sys
from pathlib import Path

# .../docker-port/src/check_inputs.py -> the repo root
REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_MANIFEST = Path("docker-port/data/inputs.json")

KINDS = ("install", "source", "data", "derived")
REQUIRED = ("name", "path", "kind", "build", "sha256", "feeds", "note")
SHA_RE = re.compile(r"^[0-9a-f]{64}$")

CHUNK = 1 << 20  # 1 MiB; a 30 MB exe is 30 reads, not 30 million


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        while True:
            block = fh.read(CHUNK)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def census(path: Path) -> dict:
    """File count and total bytes for a tree, from directory entries only.

    scandir + entry.stat() is metadata, never file contents, so this is ~0.1 s
    over the 30 GB client install - cheap enough that a truncated install
    cannot pass on its identity files alone.
    """
    files = 0
    total = 0
    stack = [str(path)]
    while stack:
        current = stack.pop()
        try:
            with os.scandir(current) as it:
                for entry in it:
                    try:
                        if entry.is_dir(follow_symlinks=False):
                            stack.append(entry.path)
                        elif entry.is_file(follow_symlinks=False):
                            files += 1
                            total += entry.stat(follow_symlinks=False).st_size
                    except OSError:
                        continue  # a file that vanished mid-walk; the count says so
        except OSError:
            continue
    return {"files": files, "bytes": total}


def short(digest: str) -> str:
    return digest[:12]


# ---------------------------------------------------------------------------
# Manifest validation: every structural problem is a failure line, never an
# exception, so one broken entry cannot hide the other five.
# ---------------------------------------------------------------------------

def load_data(path: Path, failures: list[str]) -> dict | None:
    """Parse and shape-check the manifest. None when it cannot be trusted."""
    if not path.is_file():
        failures.append(f"manifest does not exist: {path}")
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        failures.append(f"manifest is not valid JSON: {exc}")
        return None
    if not isinstance(data, dict):
        failures.append("manifest is not a JSON object")
        return None
    if data.get("schema_version") != 1:
        failures.append(f"schema_version must be 1, got {data.get('schema_version')!r}")
    entries = data.get("input")
    if not isinstance(entries, list) or not entries:
        failures.append('manifest has no "input" list')
        return None
    return data


def load_manifest(path: Path, failures: list[str]) -> list:
    data = load_data(path, failures)
    return data.get("input", []) if data else []


def validate(entry: object, seen: set[str], failures: list[str]) -> bool:
    """Structural check of one entry. True when it is safe to hash-check."""
    if not isinstance(entry, dict):
        failures.append("input entry is not a table")
        return False

    name = entry.get("name")
    if not isinstance(name, str) or not name:
        failures.append("input entry has no name")
        return False
    if name in seen:
        failures.append(f"{name}: duplicate name")
        return False
    seen.add(name)

    ok = True
    for key in REQUIRED:
        if key not in entry:
            failures.append(f"{name}: missing key {key!r}")
            ok = False

    if entry.get("kind") not in KINDS:
        failures.append(f"{name}: kind must be one of {KINDS}, got {entry.get('kind')!r}")
        ok = False
    for key in ("build", "feeds", "note"):
        if key in entry and (not isinstance(entry[key], str) or not entry[key].strip()):
            failures.append(f"{name}: {key} must be a non-empty string")
            ok = False

    rel = entry.get("path")
    if not isinstance(rel, str) or not rel:
        failures.append(f"{name}: path must be a non-empty string")
        return False
    if Path(rel).is_absolute() or ".." in Path(rel).parts:
        failures.append(f"{name}: path must be repo-relative, got {rel!r}")
        return False

    if entry.get("kind") == "derived":
        from_paths = entry.get("derived_from")
        if not isinstance(from_paths, list) or not from_paths:
            failures.append(
                f"{name}: a derived artefact must name derived_from (its sources)"
            )
            ok = False
        else:
            for src in from_paths:
                if not isinstance(src, str) or not src:
                    failures.append(f"{name}: derived_from entry is not a path: {src!r}")
                    ok = False
                    continue
                if Path(src).is_absolute() or ".." in Path(src).parts:
                    failures.append(f"{name}: derived_from must be repo-relative, got {src!r}")
                    ok = False

    return ok


# ---------------------------------------------------------------------------
# The check itself.
# ---------------------------------------------------------------------------

def check_entry(entry: dict, root: Path, failures: list[str]) -> tuple[int, int]:
    """Compare one entry to the disk. Returns (files hashed, trees censused)."""
    name = entry["name"]
    rel = entry["path"]
    path = root / rel
    hashed = 0
    trees = 0

    if not path.exists():
        # REPORTED, never skipped: this is the failure the manifest exists for.
        failures.append(f"{name}: path does not exist: {rel}")
        return 0, 0

    recorded = entry.get("sha256")

    if isinstance(recorded, dict):
        if not path.is_dir():
            failures.append(f"{name}: manifest records a directory, disk has a file: {rel}")
            return 0, 0
        if not recorded:
            failures.append(f"{name}: sha256 is an empty map - run --record")
            return 0, 0
        for sub in sorted(recorded):
            want = recorded[sub]
            target = path / sub
            if Path(sub).is_absolute() or ".." in Path(sub).parts:
                failures.append(f"{name}: sha256 key must be relative, got {sub!r}")
                continue
            if not target.is_file():
                failures.append(f"{name}: {sub} missing under {rel}")
                continue
            got = sha256_file(target)
            hashed += 1
            if not isinstance(want, str) or (want and SHA_RE.match(want) is None):
                failures.append(f"{name}: bad sha256 for {rel}/{sub}: {want!r}")
            elif want == "":
                failures.append(f"{name}: {rel}/{sub}: sha256 is empty - run --record")
            elif got != want:
                failures.append(
                    f"{name}: {rel}/{sub} sha256 mismatch (recorded {want}, on disk {got})"
                )

        tree = entry.get("tree")
        if isinstance(tree, dict):
            actual = census(path)
            trees += 1
            if actual != {"files": tree.get("files"), "bytes": tree.get("bytes")}:
                failures.append(
                    f"{name}: {rel} census changed (recorded {tree.get('files')} files / "
                    f"{tree.get('bytes')} bytes, on disk {actual['files']} / {actual['bytes']})"
                )
        return hashed, trees

    if isinstance(recorded, str):
        if not path.is_file():
            failures.append(f"{name}: manifest records a file, disk has a directory: {rel}")
            return 0, 0
        if not recorded:
            failures.append(f"{name}: sha256 is empty - run --record")
            return 0, 0
        if SHA_RE.match(recorded) is None:
            failures.append(f"{name}: bad sha256 value for {rel}: {recorded!r}")
            return 0, 0
        got = sha256_file(path)
        hashed += 1
        if got != recorded:
            failures.append(f"{name}: {rel} sha256 mismatch (recorded {recorded}, on disk {got})")
        return hashed, trees

    failures.append(f"{name}: sha256 must be a digest or a per-file map, got {type(recorded).__name__}")
    return 0, 0


def check(root: Path, manifest: Path) -> tuple[list, list[str]]:
    """The whole check. Importable, so a lane script can call it and read lines.

    Returns (entries, failures) - the entry count is the manifest's own, so a
    summary line can say how many artefacts were supposed to be there.
    """
    failures: list[str] = []
    entries = load_manifest(manifest, failures)
    seen: set[str] = set()
    usable = [e for e in entries if validate(e, seen, failures)]
    for entry in usable:
        check_entry(entry, root, failures)
    return entries, failures


# ---------------------------------------------------------------------------
# --record: write the hashes from the disk, after a deliberate change.
# ---------------------------------------------------------------------------

def record(root: Path, manifest: Path) -> int:
    failures: list[str] = []
    data = load_data(manifest, failures)
    entries = data.get("input", []) if data else []
    seen: set[str] = set()
    usable = [e for e in entries if validate(e, seen, failures)]
    for line in failures:
        print(f"FAIL {line}")
    if failures or data is None:
        return 1

    missing = 0
    for entry in usable:
        name = entry["name"]
        path = root / entry["path"]
        if not path.exists():
            print(f"record: SKIP {name}: path does not exist: {entry['path']} (its hash is unchanged)")
            missing += 1
            continue

        if path.is_dir():
            recorded = entry.get("sha256") if isinstance(entry.get("sha256"), dict) else {}
            new_map = {}
            for sub in sorted(recorded):
                target = path / sub
                if not target.is_file():
                    print(f"record: SKIP {name}: {sub} missing under {entry['path']}")
                    missing += 1
                    new_map[sub] = recorded[sub]
                    continue
                got = sha256_file(target)
                if got != recorded[sub]:
                    old = short(recorded[sub]) if recorded[sub] else "(none)"
                    print(f"record: {name}: {sub} {old} -> {short(got)}")
                new_map[sub] = got
            entry["sha256"] = new_map
            entry["tree"] = census(path)
        else:
            got = sha256_file(path)
            old = entry.get("sha256") if isinstance(entry.get("sha256"), str) else ""
            if got != old:
                print(f"record: {name}: {entry['path']} {short(old) if old else '(none)'} -> {short(got)}")
            entry["sha256"] = got

    data["recorded"] = datetime.date.today().isoformat()
    # LF explicitly: this file is compared byte for byte by nothing, but a
    # platform-dependent newline would make every --record a whole-file diff.
    manifest.write_text(
        json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8", newline="\n"
    )
    print(f"record: wrote {manifest}")
    return 1 if missing else 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Check docker-port/data/inputs.json against the files it names."
    )
    parser.add_argument(
        "--manifest",
        default=str(DEFAULT_MANIFEST),
        help=f"manifest, relative to the repo root (default: {DEFAULT_MANIFEST})",
    )
    parser.add_argument(
        "--root",
        default=str(REPO_ROOT),
        help=f"repo root the paths are relative to (default: {REPO_ROOT})",
    )
    parser.add_argument(
        "--record",
        action="store_true",
        help="rewrite the hashes from disk (deliberate: after a deliberate change)",
    )
    args = parser.parse_args(argv)

    root = Path(args.root).resolve()
    manifest = Path(args.manifest)
    if not manifest.is_absolute():
        manifest = root / manifest

    if args.record:
        return record(root, manifest)

    entries, failures = check(root, manifest)
    for line in failures:
        print(f"FAIL {line}")

    if failures:
        print(f"check_inputs: {len(entries)} artefacts, {len(failures)} failure(s)")
        return 1

    hashed = 0
    trees = 0
    for entry in entries:
        rec = entry.get("sha256")
        if isinstance(rec, dict):
            hashed += len(rec)
            trees += 1 if "tree" in entry else 0
        else:
            hashed += 1
    print(
        f"OK inputs: {len(entries)} artefacts, {hashed} files hashed, {trees} trees censused"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
