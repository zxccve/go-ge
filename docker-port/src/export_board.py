"""export_board - write docker-port/QUEUE.md from the live kanban board.

The board is the queue AND the handoff. This page is the board's shadow, tracked
in git so the queue survives a profile rebuild, a lost session or a new clone.
It is GENERATED: never edit QUEUE.md by hand, and never treat it as the source -
the live board is.

Read through the CLI on purpose (`hermes kanban ...`), so the script needs no
database path and works on any machine where the CLI is installed.

    cd docker-port\\src
    python export_board.py                      # writes ..\\QUEUE.md
    python export_board.py --board ge_server    # the same, explicit
    python export_board.py --out other.md

Exit 0 when the page is written, 1 when the board cannot be read (a silent
empty page would look like an empty queue).
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time

import ge_paths

ROOT = ge_paths.ROOT                       # ...\\docker-port
DEFAULT_OUT = os.path.join(ROOT, 'QUEUE.md')
DEFAULT_BOARD = 'ge_server'

# The open states, in the order a reader cares about them. `archived` is NOT
# here and is filtered out by name as well: a retired card is not queue.
OPEN_ORDER = ['running', 'review', 'ready', 'scheduled', 'todo', 'blocked']
OPEN = set(OPEN_ORDER)

DONE_WINDOW_H = 24
COMMENT_CHARS = 200
CLI_TIMEOUT = 120


def _cli(args):
    """Run `hermes kanban ...` and return (stdout, stderr, returncode)."""
    exe = shutil.which('hermes') or 'hermes'
    cmd = [exe, 'kanban'] + list(args)
    proc = subprocess.run(cmd, capture_output=True, text=True,
                          encoding='utf-8', errors='replace',
                          timeout=CLI_TIMEOUT)
    return proc.stdout, proc.stderr, proc.returncode


def load_cards(board):
    """Every card the board lists, as dicts. Archived cards are dropped here."""
    out, err, code = _cli(['--board', board, 'ls', '--json'])
    if code != 0:
        raise RuntimeError('`hermes kanban ls` exited %d: %s'
                           % (code, (err or out).strip()[:300]))
    try:
        cards = json.loads(out)
    except json.JSONDecodeError as exc:
        raise RuntimeError('`hermes kanban ls --json` did not return JSON (%s): %s'
                           % (exc, out.strip()[:200]))
    if not isinstance(cards, list):
        raise RuntimeError('`hermes kanban ls --json` returned %s, not a list'
                           % type(cards).__name__)
    return [c for c in cards if c.get('status') != 'archived']


def last_comment(board, card_id):
    """The card's last comment, else its latest run summary, else ''."""
    out, err, code = _cli(['--board', board, 'show', card_id, '--json'])
    if code != 0:
        return ''
    try:
        data = json.loads(out)
    except json.JSONDecodeError:
        return ''
    comments = data.get('comments') or []
    if comments:
        body = (comments[-1] or {}).get('body') or ''
        if body.strip():
            return body
    return (data.get('latest_summary') or '').strip()


def one_line(text, limit=COMMENT_CHARS):
    """Collapse whitespace, drop table-breaking characters, cut to `limit`."""
    text = ' '.join((text or '').split())
    text = text.replace('|', '/').replace('\u2014', '-')
    if len(text) > limit:
        cut = text[:limit]
        space = cut.rfind(' ')
        if space > limit * 0.6:
            cut = cut[:space]
        text = cut.rstrip(' ,.;:') + ' ...'
    return text or '(no comment)'


def _cell(text):
    return one_line(text, limit=10_000)


def render(cards, board, now=None):
    """The whole page as text. Pure: no I/O, so it is testable by eye."""
    now = now or time.time()
    open_cards = [c for c in cards if c.get('status') in OPEN]
    open_cards.sort(key=lambda c: (OPEN_ORDER.index(c['status']),
                                   -(c.get('priority') or 0),
                                   c.get('created_at') or 0))
    done = [c for c in cards
            if c.get('status') == 'done'
            and isinstance(c.get('completed_at'), (int, float))
            and now - c['completed_at'] <= DONE_WINDOW_H * 3600]
    done.sort(key=lambda c: c['completed_at'], reverse=True)

    stamp = time.strftime('%Y-%m-%d %H:%M', time.localtime(now))
    lines = [
        '# QUEUE - the board, exported',
        '',
        '> **GENERATED - do not edit this page.** Written by `src\\export_board.py` at',
        '> **%s** from `hermes kanban --board %s ls --json`. **The live board is the' % (stamp, board),
        '> source**: this file is its shadow, tracked in git so the queue survives a',
        '> profile rebuild. Refresh it with:',
        '>',
        '>     cd docker-port\\src',
        '>     python export_board.py',
        '',
        'The board is both the queue and the handoff. A card carries its own state and its',
        'own last line ("STARTED 18:08", "LANDED <commit>"), so no separate handoff page is',
        'kept. The last column below is the card\'s last comment, else its latest run summary,',
        'cut to %d characters - open the card on the board for the whole thread.' % COMMENT_CHARS,
        '',
        '## Open cards (%d)' % len(open_cards),
        '',
    ]
    if open_cards:
        lines += [
            '| id | status | prio | assignee | title | last comment |',
            '|----|--------|------|----------|-------|--------------|',
        ]
        for c in open_cards:
            lines.append('| `%s` | %s | %s | %s | %s | %s |' % (
                c.get('id', '?'),
                c.get('status', '?'),
                c.get('priority', ''),
                c.get('assignee') or '-',
                _cell(c.get('title', '')),
                one_line(c.get('last_comment', '')),
            ))
    else:
        lines.append('_No open cards._')

    lines += ['', '## Done in the last %d hours (%d)' % (DONE_WINDOW_H, len(done)), '']
    if done:
        for c in done:
            when = time.strftime('%H:%M', time.localtime(c['completed_at']))
            lines.append('- %s (%s, `%s`)' % (_cell(c.get('title', '')), when, c.get('id', '?')))
    else:
        lines.append('_Nothing completed in the window._')
    lines.append('')
    return '\n'.join(lines)


def main(argv=None):
    ap = argparse.ArgumentParser(
        description='Write QUEUE.md - the live kanban board, exported.')
    ap.add_argument('--board', default=DEFAULT_BOARD)
    ap.add_argument('--out', default=DEFAULT_OUT)
    args = ap.parse_args(argv)

    try:
        cards = load_cards(args.board)
    except (RuntimeError, OSError, subprocess.SubprocessError) as exc:
        print('export_board: %s' % exc, file=sys.stderr)
        return 1

    n_open = sum(1 for c in cards if c.get('status') in OPEN)
    for c in cards:
        c['last_comment'] = last_comment(args.board, c['id']) if c.get('status') in OPEN else ''

    text = render(cards, args.board)
    out = os.path.abspath(args.out)
    with open(out, 'w', encoding='utf-8') as f:
        f.write(text)

    now = time.time()
    n_done = sum(1 for c in cards
                 if c.get('status') == 'done'
                 and isinstance(c.get('completed_at'), (int, float))
                 and now - c['completed_at'] <= DONE_WINDOW_H * 3600)
    print('QUEUE.md: %d open card(s), %d done in %dh -> %s'
          % (n_open, n_done, DONE_WINDOW_H, out))
    return 0


if __name__ == '__main__':
    sys.exit(main())
