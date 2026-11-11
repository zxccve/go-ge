"""check_layout - flag any file that does not belong where it is.

The top level of docker-port holds only the handoff docs (`.md`). Everything else
lives in a folder, and every writer reaches those folders through ge_paths.py.
Run this after a session to catch a stray output before it becomes scenery:

    cd src
    python check_layout.py

Exit 0 = clean. Exit 1 = a loose file is sitting at the top level.

Set `GE_DOCKER_PORT_ROOT` to check a different checkout (the git hook uses this
to test itself).
"""

import os
import sys

import ge_paths

# Non-Markdown files allowed to sit at the top level. The handoff docs always
# end in `.md` and are allowed by rule; a stray OUTPUT file never ends in `.md`,
# so anything else at the top level is reported. Add names here only for a
# legitimate non-Markdown file that must live at the top.
TOP_FILES_OK = {
    # The clerk's brake (AGENTS.md, "A clerk may start cards while you read this"):
    # `touch docker-port\STOP-QUEUE` stops the self-dispatching queue, deleting it resumes.
    # It MUST live at the top level because that is where pick_next.py looks for it.
    "STOP-QUEUE",
    # The container build's ignore file (deploy\, 2026-11-10). Docker reads it from
    # the BUILD CONTEXT root and nowhere else, and the context is docker-port\ - so it
    # must sit here even though nothing human reads it.
    ".dockerignore",
}

# Folders allowed to sit at the top level.
TOP_DIRS_OK = {"src", "data", "cache", "config", "logs", "out", "captures",
               "backup", "screenshots", "ghidra", "navmesh",
               "history", "progress", "__pycache__", "goserver", "archive",
               # 2026-11-06, the parallel-lane workflow (AGENTS.md "How work
               # flows", rule 6): the knowledge lane's question queue and the
               # per-lane claim files.
               "research", "lanes",
               # 2026-11-11: the Docker deployment, the JP-client launcher and its shim
               # are real project folders, not strays.
               "deploy", "launcher", "shim"}

# Which extensions each owned folder may hold (folders not listed: anything).
FOLDER_EXTS = {
    "src": {".py", ".pkl"},
    "captures": {".log"},
    "config": {".txt"},
    # cache\ holds the converter's XML output (a decoded .ies table) as well
    # as the built JSON caches. It is all regenerable scratch.
    "cache": {".json", ".pkl", ".xml"},
    "logs": {".log", ".err", ".out", ".txt"},   # early runs wrote .txt; they ARE logs
    "screenshots": {".png"},
    "history": {".md"},
    "progress": {".md"},
    "ghidra": {".json"},                        # decompile text output -> ghidra\decompiles\
    "backup": {".bak", ".txt"},                 # backup_db.py writes last_backup.txt
    # The Go server. testdata\ holds the fixtures and is skipped (subfolders
    # are not checked). run_barrack.cmd is the double-click launcher; build.sh
    # is the Linux/WSL build script; server.json is the config the zone and the
    # manager read from the working directory.
    "goserver": {".go", ".mod", ".sum", ".md", ".cmd", ".sh", ".json"},
}


def main(root=None):
    root = root or os.environ.get("GE_DOCKER_PORT_ROOT") or ge_paths.ROOT
    problems = []
    warnings = []

    for name in sorted(os.listdir(root)):
        full = os.path.join(root, name)
        if os.path.isdir(full):
            if name not in TOP_DIRS_OK:
                warnings.append("new top-level folder: %s\\" % name)
        elif name.lower().endswith(".md"):
            pass                      # handoff docs belong at the top
        elif name not in TOP_FILES_OK:
            problems.append("loose file at the top level: %s" % name)

    for folder, exts in FOLDER_EXTS.items():
        d = os.path.join(root, folder)
        if not os.path.isdir(d):
            continue
        for name in sorted(os.listdir(d)):
            if os.path.isdir(os.path.join(d, name)):
                continue
            ext = os.path.splitext(name)[1].lower()
            if ext not in exts:
                warnings.append("%s\\%s (expected: %s)"
                                % (folder, name, " ".join(sorted(exts))))

    for w in warnings:
        print("warn : %s" % w)
    for p in problems:
        print("FAIL : %s" % p)

    if problems:
        print("\n%d loose file(s) at the top level. Move each into its folder - "
              "see INDEX.md -> Conventions." % len(problems))
        return 1
    print("layout OK - no loose files at the top level.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
