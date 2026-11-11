"""Central path map for the docker-port work area.

Every script that reads or writes a file outside its own folder imports this,
so a run never drops stray files in the top level again.

    from ge_paths import CAPTURES, LOGS, OUT

The folders are created on import, so no caller has to make them first.
"""

import os

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# The repository root - `docker-port`'s parent. Everything the game ships lives
# under here (`ge\`, the client installs, the old release folders), so a tool
# that needs one of those joins it to GAME_ROOT instead of writing a drive path.
# GE_GAME_ROOT overrides it, which is what makes the tools portable: a clone
# anywhere works as long as the caller points this at their own game tree.
GAME_ROOT = os.environ.get("GE_GAME_ROOT") or os.path.dirname(ROOT)

# The game's own data tree (git-ignored, many GB). Read-only for the server,
# except for the script rewrite - which is why its path belongs here too.
GE = os.path.join(GAME_ROOT, "ge")
GE_SCRIPT = os.path.join(GE, "script")

DATA = os.path.join(ROOT, "data")
CACHE = os.path.join(ROOT, "cache")
CONFIG = os.path.join(ROOT, "config")
LOGS = os.path.join(ROOT, "logs")
OUT = os.path.join(ROOT, "out")
CAPTURES = os.path.join(ROOT, "captures")
BACKUP = os.path.join(ROOT, "backup")
SCREENSHOTS = os.path.join(ROOT, "screenshots")

_FOLDERS = (DATA, CACHE, CONFIG, LOGS, OUT, CAPTURES, BACKUP, SCREENSHOTS)


def ensure(*folders):
    """Create the given folders (default: all of them). Safe to call often."""
    for d in folders or _FOLDERS:
        os.makedirs(d, exist_ok=True)


ensure()
