"""check_all - run every feature check and print one line per feature.

Usage:
    python check_all.py            # the checks that need no server
    python check_all.py --servers  # also the ones that need a running server
WHY THIS EXISTS: the project grew a lot of features and each one had its own
test, but there was no single command that said "the thing still works". A
feature that silently stops working looks exactly like a feature that never
worked, and the only way to tell is to run everything.

The output is one line per check: PASS / FAIL / SKIP (needs a server) plus
the first line of the check's own output, so a failure says what broke.

The list of checks and what each covers is in docker-port/history/features.md.
"""
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# (label, script, needs a server)
CHECKS = [
    ("the world module", "test_world.py", False),
    ("the barrack's conversation", "test_client.py", True),
    ("two players in one barrack", "test_two_barracks.py", True),
    ("the NPC click rules", "test_click_range.py", True),
    ("two players in one zone", "test_two_players.py", True),
    ("the 'Last Point' chain", "test_continue_position.py", True),
    ("the saved position", "test_saved_position.py", True),
    ("the zone's bytes", "test_golden.py", True),
]


def run(label, script, needs_server):
    path = os.path.join(HERE, script)
    if not os.path.exists(path):
        print("SKIP  %-28s (%s not found)" % (label, script))
        return None
    args = [sys.executable, path]
    if script == "test_golden.py":
        # The golden file is the reference; the check compares against it.
        golden = os.path.join(os.path.dirname(HERE), "data", "zone_golden.txt")
        args += [os.environ.get("ZONE_PORT", "7002"), golden, "--check"]
    try:
        p = subprocess.run(args, cwd=HERE, capture_output=True, text=True,
                           timeout=600)
    except subprocess.TimeoutExpired:
        print("FAIL  %-28s (timed out)" % label)
        return False
    out = (p.stdout or "") + (p.stderr or "")
    lines = [l.strip() for l in out.splitlines() if l.strip()]
    detail = ""
    for l in lines:
        if l.startswith(("ok ", "all ", "done", "two players", "GOLDEN OK",
                         "RESULT: OK", "connected")):
            detail = l
            break
    if not detail and lines:
        detail = lines[-1]
    if p.returncode == 0:
        print("PASS  %-28s %s" % (label, detail[:70]))
        return True
    if needs_server and ("ConnectionRefused" in out or "ConnectError" in out
                         or "cannot connect" in out.lower()):
        print("SKIP  %-28s (no server on the port?)" % label)
        return None
    print("FAIL  %-28s %s" % (label, detail[:70]))
    return False


def main():
    with_servers = "--servers" in sys.argv
    print("=== checks that need no server")
    results = []
    for label, script, needs in CHECKS:
        if needs and not with_servers:
            print("SKIP  %-28s (needs a running server; use --servers)" % label)
            results.append(None)
            continue
        results.append(run(label, script, needs))
    bad = [l for l, r in zip([c[0] for c in CHECKS], results) if r is False]
    print()
    print("%d checks, %d failed" % (len(results), len(bad)))
    for l in bad:
        print("   FAILED: %s" % l)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main() or 0)
