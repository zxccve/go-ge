"""check_knowledge - is the documentation still TRUE, not just small?

Run from src\\:
    python check_knowledge.py            # report, default 15 examples per section
    python check_knowledge.py --all      # stop truncating the lists
    python check_knowledge.py --limit 40

check_docs.py answers "is each doc within its ceiling?". This answers the other
half of the problem, which a line count cannot see:

  1. INVISIBLE   a source file that no doc names AND that has no docstring of
                 its own. Nothing can describe it, so the next session cannot
                 find it and writes it again. A file that merely lacks a row in
                 INDEX.md is only a note: the generated tool index lists it.
  2. DEAD PATH     a doc points at a file/folder that does not exist. Usually a
                   rename or a move that the doc never caught up with. A line
                   that says the file is gone (or "not in this tree") is
                   history and is left alone.
  3. STALE DOC     a doc whose last commit is OLDER than the files it names, so
                   the file changed after the text describing it was written.

What this does NOT do: judge whether a sentence is right. Two things are
provable without reading - does the name exist, and which changed last - so
only those are reported. A "stale" row means "look here", not "this is wrong".

Exit: 0 clean, 1 if anything needs looking at. Safe to re-run; it writes nothing.
"""
import os
import re
import subprocess
import sys

import ge_paths

ROOT = ge_paths.ROOT                       # ...\docker-port
PROJECT_ROOT = os.path.dirname(ROOT)       # ...\ge_server

# A doc is "live" if a reader is meant to trust it. progress\ is excluded on
# purpose: it is an append-only log, so a path it mentions may legitimately be
# gone. Judging history by today's file tree would produce noise, not signal.
LIVE_DOCS = [
    os.path.join(PROJECT_ROOT, 'README.md'),
    os.path.join(PROJECT_ROOT, 'AGENTS.md'),
    os.path.join(ROOT, 'QUEUE.md'),
    os.path.join(ROOT, 'RESUME.md'),
    os.path.join(ROOT, 'ROADMAP.md'),
    os.path.join(ROOT, 'OPERATIONS.md'),
    os.path.join(ROOT, 'DECISIONS.md'),
    os.path.join(ROOT, 'GLOSSARY.md'),
    os.path.join(ROOT, 'INDEX.md'),
    os.path.join(ROOT, 'HISTORY.md'),
]
HISTORY_DIR = os.path.join(ROOT, 'history')

# Files whose mtime/commit is compared against the docs that name them.
SRC_DIR = os.path.join(ROOT, 'src')
SRC_EXT = '.py'

# A source file counts as documented if its stem appears as a whole word.
# Matching ".py" would miss "analyze_capture (see below)" style mentions.
MIN_STALE_DAYS = 7
DOC_MAX_BYTES = 400_000       # a doc bigger than this is not worth scanning

PATH_TOKEN = re.compile(
    r'(?:[A-Za-z]:\\[^\s|`"\'\<>]+)'  # an absolute Windows path
    r'|(?:docker-port[\\/][^\s|`"\'<>]+)'               # docker-port\src\...
    r'|(?:(?:src|data|config|logs|out|captures|backup|history|progress|ghidra|tests?|ge)[\\/][^\s|`"\'<>]+)'
    r'|(?:[\w.\-]+\.(?:py|md|json|pkl|xml|ies|txt|go|cs|sql))'
)
# Placeholders and prose that look like paths but are not one.
NOT_A_PATH = re.compile(r'[<>*{}]|YYYY|XXX|\.\.\.|\bFoo\b|\bfoo\b|\bname\b')

# A doc that says the thing is GONE is history, not a stale claim. The row a
# checker should raise is "the doc promises a file that should be here", not
# "the doc remembers a file we deleted on purpose".
GONE_HINT = re.compile(
    r'\b(since deleted|was deleted|already deleted|deleted|removed|no longer exists|now gone|scrapped'
    r'|not in (this|the current) tree|not present in this tree)\b',
    re.I,
)
# Folders whose contents are produced at run time. A doc naming
# out\x.txt or backup\y.bak is describing something that will be CREATED, so
# "it does not exist" is not a broken reference - it is expected.
RUNTIME_DIRS = {'out', 'logs', 'cache', 'captures', 'backup'}
# A bare filename (no folder) is only worth checking when it is code or a doc:
# bare data names are usually artifacts that come and go.
CHECK_BARE_EXT = ('.py', '.go', '.md', '.cs', '.sql')


def _read(path):
    try:
        if os.path.getsize(path) > DOC_MAX_BYTES:
            return ''
        with open(path, encoding='utf-8', errors='replace') as f:
            return f.read()
    except OSError:
        return ''


def _live_doc_paths():
    docs = [p for p in LIVE_DOCS if os.path.isfile(p)]
    if os.path.isdir(HISTORY_DIR):
        docs.extend(
            os.path.join(HISTORY_DIR, n)
            for n in sorted(os.listdir(HISTORY_DIR))
            if n.endswith('.md')
        )
    return docs


def _commit_days():
    """path -> unix time of its last commit, for the whole repo, in ONE git call.

    Commit time is used instead of mtime because a checkout or a copy rewrites
    mtime and would make every doc look fresh.
    """
    try:
        out = subprocess.run(
            ['git', '-C', PROJECT_ROOT, 'log', '--format=@%ct', '--name-only', '-n', '600'],
            capture_output=True, text=True, timeout=60,
        )
    except (OSError, subprocess.SubprocessError):
        return {}
    stamp, table = None, {}
    for line in out.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        if line.startswith('@') and line[1:].isdigit():
            stamp = int(line[1:])
        elif stamp is not None and line not in table:
            table[line.replace('\\', '/')] = stamp
    return table


def _repo_key(path):
    return os.path.relpath(path, PROJECT_ROOT).replace('\\', '/')


# The docs write client paths as `ge\ui\...` - shorthand for the shipped client
# tree. Resolving that literally would report every client reference as missing,
# which is a fault of the checker, not of the doc.
PATH_ALIASES = {"ge": "GETerraSeptember1th2022/ge"}

# Small top-level folders outside docker-port that docs legitimately name.
EXTRA_BASENAME_ROOTS = [
    os.path.join(PROJECT_ROOT, "SQL"),
    os.path.join(PROJECT_ROOT, "Converter"),
    os.path.join(PROJECT_ROOT, "geserver_release_folder_source_code"),
]


def _resolves(token, basenames):
    """Does this path-like token exist anywhere sensible?"""
    if not token or NOT_A_PATH.search(token):
        return True
    bare = token.replace('\\', '/').split('/')[-1]
    cands = [token] if os.path.isabs(token) else []
    if not os.path.isabs(token):
        cands += [
            os.path.join(ROOT, token),
            os.path.join(PROJECT_ROOT, token),
            os.path.join(SRC_DIR, os.path.basename(token)),
            os.path.join(PROJECT_ROOT, *bare.split('/')),
        ]
        head, _, rest = token.replace('\\', '/').partition('/')
        alias = PATH_ALIASES.get(head.lower())
        if alias and rest:
            cands.append(os.path.join(PROJECT_ROOT, *alias.split('/'), *rest.split('/')))
    if any(os.path.exists(c) for c in cands):
        return True
    # A doc may name a file by bare name that lives somewhere else (a skill's
    # reference file, progress\2026-10.md, ...). "Exists in the tree" is what
    # matters, not "sits at the path I guessed".
    return bare in basenames


# Walked once, pruned heavily: this answers "does a file of this name exist
# anywhere", nothing more.
PRUNE = {'cache', '.git', 'node_modules', 'ghidra', 'out', 'logs', 'captures',
         'backup', '__pycache__'}
SKILLS_ROOT = os.path.join(os.path.expanduser('~'), '.config', 'opencode', 'skills')


def _basenames():
    names = set()
    for base, prune in (
        (ROOT, PRUNE),
        (SKILLS_ROOT, {'.git'}),
        *[(p, PRUNE) for p in EXTRA_BASENAME_ROOTS],
    ):
        if not os.path.isdir(base):
            continue
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames[:] = [d for d in dirnames if d.lower() not in prune]
            names.update(filenames)
            if len(names) > 80_000:
                return names
    return names


def _has_docstring(path):
    """A file whose own docstring describes it can be found by build-index.mjs."""
    head = _read(path)[:600]
    return '"""' in head or "'''" in head


def check_undocumented(src_files, docs_text):
    """Source files whose stem no live doc names."""
    missing = []
    for name in src_files:
        stem = os.path.splitext(name)[0]
        if not re.search(r'\b' + re.escape(stem) + r'\b', docs_text):
            missing.append(name)
    return missing


def check_dead_paths(docs):
    """(doc, token) for every doc reference that points nowhere."""
    dead, seen = [], set()
    basenames = _basenames()
    for path in docs:
        text = _read(path)
        rel = os.path.relpath(path, ROOT)
        for m in PATH_TOKEN.finditer(text):
            raw = m.group(0)
            # A token that starts in the MIDDLE of a longer path is not a path
            # of its own: in `_andro_ies_stage\ies10000`, `ge\ies10000` is a
            # substring, not a missing folder.
            start = m.start()
            if start > 0 and (text[start - 1].isalnum() or text[start - 1] in "_.-/"):
                continue
            # Placeholder check runs on the RAW match: `GE_DATA_<stamp>.bak`
            # must be rejected as a template, not shrunk to `GE_DATA_` and then
            # reported as a missing file.
            if NOT_A_PATH.search(raw):
                continue
            token = raw.rstrip('.,;:)]}\'"')
            # ...and a line that says the file is gone is deliberately telling
            # the reader it is gone. The sentence may wrap, so the next line
            # counts as part of the same claim.
            line_start = text.rfind("\n", 0, start) + 1
            line_end = text.find("\n", start)
            nxt_end = text.find("\n", (line_end + 1) if line_end >= 0 else len(text))
            line = text[line_start: nxt_end if nxt_end >= 0 else len(text)]
            # Sentences wrap, so judge the claim with its line breaks collapsed.
            if GONE_HINT.search(re.sub(r'\s+', ' ', line)):
                continue
            if not token:
                continue
            has_sep = '\\' in token or '/' in token
            if has_sep:
                top = token.replace('/', '\\').split('\\')[0].lower()
                if top in RUNTIME_DIRS:
                    continue
            elif not token.lower().endswith(CHECK_BARE_EXT):
                continue
            # A real path's last segment ends in a short extension. This rejects
            # code expressions that merely look path-like, e.g.
            # `src\npc_data.find_paths(`.
            ext = os.path.splitext(token)[1]
            if ext and not re.fullmatch(r'\.[A-Za-z0-9]{1,6}', ext):
                continue
            # An absolute path cut at a SPACE (a folder name with a space in it) has no
            # extension and is not checkable. Relative ones are kept, because
            # `ge\ies10000` is a real finding.
            if re.match(r'^[A-Za-z]:\\', token) and not os.path.splitext(token)[1]:
                continue
            # `data/cache/logs/output` is prose listing folders, not a path.
            if not os.path.splitext(token)[1] and (token.count('\\') + token.count('/')) > 1:
                continue
            if (rel, token) in seen:
                continue
            if not _resolves(token, basenames):
                seen.add((rel, token))
                dead.append((rel, token))
    return dead


def check_stale(docs, src_files, commits):
    """Docs whose last commit predates the files they name."""
    hits = []
    for path in docs:
        text = _read(path)
        key = _repo_key(path)
        doc_t = commits.get(key)
        if doc_t is None:
            continue
        for name in src_files:
            stem = os.path.splitext(name)[0]
            if not re.search(r'\b' + re.escape(stem) + r'\b', text):
                continue
            src_t = commits.get(_repo_key(os.path.join(SRC_DIR, name)))
            if src_t is None:
                continue
            if src_t - doc_t > MIN_STALE_DAYS * 86_400:
                hits.append((os.path.relpath(path, ROOT), name,
                             int((src_t - doc_t) // 86_400)))
    return hits


def _show(title, rows, limit, fmt):
    if not rows:
        return
    print(f'\n{title} ({len(rows)}):')
    for row in rows[:limit]:
        print(fmt(row))
    if len(rows) > limit:
        print(f'  ... and {len(rows) - limit} more (--all to see them)')


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    show_all = '--all' in argv
    limit = 10 ** 9 if show_all else (
        int(argv[argv.index('--limit') + 1]) if '--limit' in argv else 15
    )

    src_files = sorted(
        n for n in os.listdir(SRC_DIR)
        if n.endswith(SRC_EXT) and os.path.isfile(os.path.join(SRC_DIR, n))
    ) if os.path.isdir(SRC_DIR) else []
    docs = _live_doc_paths()
    docs_text = '\n'.join(_read(p) for p in docs)

    undocumented = check_undocumented(src_files, docs_text)
    # A file whose own docstring describes it is already listed by
    # build-index.mjs, which the agent is told to read. That is FINDABLE, so it
    # is a note, not a failure. The failure is a file nothing can describe:
    # no doc names it and it does not say what it is.
    invisible = [n for n in undocumented if not _has_docstring(os.path.join(SRC_DIR, n))]
    named_but_quiet = [n for n in undocumented if n not in invisible]
    dead = check_dead_paths(docs)
    stale = check_stale(docs, src_files, _commit_days())

    print(f'Scanned {len(docs)} live docs and {len(src_files)} src\\*{SRC_EXT} files.')
    _show('INVISIBLE - no doc names it AND it has no docstring (nothing can find this)',
          invisible, limit, lambda n: f'  {n}')
    _show('UNDOCUMENTED - only the generated tool index names it (optional to add to INDEX.md)',
          named_but_quiet, limit, lambda n: f'  {n}')
    _show('DEAD PATH - doc points at something that does not exist', dead, limit,
          lambda r: f'  {r[0]}: {r[1]}')
    _show(f'STALE DOC - last commit older than a file it names (>{MIN_STALE_DAYS}d)',
          sorted(stale, key=lambda r: -r[2]), limit,
          lambda r: f'  {r[0]}: {r[1]} changed {r[2]}d after the doc')

    problems = len(invisible) + len(dead) + len(stale)
    if not problems:
        if named_but_quiet:
            print(f'\nKnowledge OK. {len(named_but_quiet)} tool(s) are only in the generated index,'
                  ' which is enough to find them.')
        else:
            print('\nKnowledge OK.')
        return 0
    print(f'\n{problems} row(s) to look at. Each is a place to LOOK, not a verdict.')
    return 1


if __name__ == '__main__':
    sys.exit(main())
