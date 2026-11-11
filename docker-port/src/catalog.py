"""catalog - list every script in src\\ with its one-line summary.

Run from src\\:
    python catalog.py                # all scripts, grouped by category
    python catalog.py --search quest # filter by name or summary
    python catalog.py --flat        # one line per script, no grouping

Categories come from a small heuristic on the filename (test_*, check_*, dump_*,
ge_*_server.py, ...). Scripts that do not match any rule land in "Other" so the
gaps are visible. Each script's summary is its module docstring's first line.
"""
import argparse
import ast
import os
import sys
import warnings

warnings.filterwarnings('ignore', category=SyntaxWarning)


def _category(name):
    n = name.lower()
    if n.startswith('test_'):
        return 'Tests'
    if n.startswith('check_'):
        return 'Checkers'
    if n.startswith('dump_'):
        return 'Dumpers'
    if 'probe' in n:
        return 'Probes'
    if n.startswith('capture') or n.startswith('pcap'):
        return 'Capture'
    if n.endswith('_server.py'):
        return 'Servers'
    if n.startswith('ge_'):
        return 'Core (data/crypto)'
    if n.startswith('analyze'):
        return 'Analysis'
    if n.startswith('find_') or n.startswith('hunt') or n.startswith('scan_') or n.startswith('brute_'):
        return 'Search'
    if n.startswith('mem_'):
        return 'Memory'
    if n.startswith('ies_') or n.startswith('ies'):
        return 'IES'
    if n.startswith('_'):
        return 'One-off helpers'
    if n.startswith('bf') or n.startswith('core') or n.startswith('custom'):
        return 'Cipher (legacy)'
    if n.startswith('decrypt') or n.startswith('final_decrypt'):
        return 'Cipher'
    if n.startswith('callers_'):
        return 'Search'
    if n.startswith('cmd_gap'):
        return 'Analysis'
    if n.startswith('decode_ies'):
        return 'IES'
    if n.startswith('review_'):
        return 'Analysis'
    if n.startswith('xref_'):
        return 'Crash/PE analysis'
    if n.startswith('doctor'):
        return 'Ops'
    if n.startswith('world'):
        return 'World'
    if (n.startswith('stance_') or n.startswith('skill_') or n.startswith('npc_')
            or n.startswith('item_') or n.startswith('xml_') or n.startswith('ge_locale')
            or n.startswith('ge_item') or n.startswith('ge_paths') or n.startswith('ge_tables')
            or n.startswith('ge_crypt') or n.startswith('ge_db')):
        return 'Core (data/crypto)'
    if n.startswith('client_') or n.startswith('click_'):
        return 'Client tooling'
    if (n.startswith('crash_') or n.startswith('minidump') or n.startswith('sample_eip')
            or n.startswith('oracle_') or n.startswith('pe_') or n.startswith('strs_')):
        return 'Crash/PE analysis'
    if n.startswith('live_') or n.startswith('compare_'):
        return 'Analysis'
    if n.startswith('make_') or n.startswith('rebuild_') or n.startswith('rewrite_'):
        return 'Data build'
    if n.startswith('parse_') or n.startswith('extract_') or n.startswith('derive_') or n.startswith('solve_') or n.startswith('v28_'):
        return 'Analysis'
    if n.startswith('verify_') or n.startswith('switch_') or n.startswith('watch_'):
        return 'Verification'
    if n.endswith('_stub.py'):
        return 'Stubs (legacy)'
    if n.startswith('inspect_'):
        return 'Inspection'
    if n == 'shot.py':
        return 'Screenshots'
    return 'Other'


def _summary(path):
    try:
        tree = ast.parse(open(path, encoding='utf-8', errors='replace').read())
        if (tree.body and isinstance(tree.body[0], ast.Expr)
                and isinstance(tree.body[0].value, ast.Constant)
                and isinstance(tree.body[0].value.value, str)):
            return tree.body[0].value.value.strip().splitlines()[0]
    except Exception:
        pass
    return '(no docstring)'


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser()
    ap.add_argument('--search', '-s', default='')
    ap.add_argument('--flat', action='store_true')
    ap.add_argument('--category', '-c', default='')
    args = ap.parse_args()

    files = sorted(f for f in os.listdir(here)
                   if f.endswith('.py') and f != 'catalog.py')

    rows = []
    for f in files:
        cat = _category(f)
        summ = _summary(os.path.join(here, f))
        if args.search:
            q = args.search.lower()
            if q not in f.lower() and q not in summ.lower() and q not in cat.lower():
                continue
        if args.category and args.category.lower() not in cat.lower():
            continue
        rows.append((cat, f, summ))

    if args.flat:
        for cat, f, summ in rows:
            print(f'{f:34s} {summ}')
        return 0

    groups = {}
    for cat, f, summ in rows:
        groups.setdefault(cat, []).append((f, summ))

    for cat in sorted(groups):
        print(f'\n== {cat} ({len(groups[cat])}) ==')
        for f, summ in groups[cat]:
            print(f'  {f:34s} {summ}')

    # Surface uncategorized count as a hint.
    other = [r for r in rows if r[0] == 'Other']
    if other and not args.search and not args.category:
        print(f'\n({len(other)} scripts in "Other" - consider adding a category rule or a better filename prefix.)')
    return 0


if __name__ == '__main__':
    sys.exit(main())