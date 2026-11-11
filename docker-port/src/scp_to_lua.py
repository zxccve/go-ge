"""Translate a `.scp` script into plain Lua 5.1 text.

Why this file exists
--------------------
The game's script dialect is **stock Lua with C-style spelling**. Proof is the
maker's own parser in `source2\\imcIES\\lua\\`: `llex.cpp` lists the keyword set
(`func`, `var`, `null`, `&&`, `||`, `!=`) and `lparser.cpp` closes a block with
`check_match(ls, '}', TK_IF, line)`. So a script can be rewritten to real Lua
and run by an ordinary Lua, instead of writing a new interpreter.

What it changes
---------------
    func F(a)      -> function F(a)
    var x = 1;     -> local x = 1
    if (c) {       -> if (c) then
    else if (c) {  -> elseif (c) then
    while (c) {    -> while (c) do
    for (i=1, n) { -> for i = 1, n do
    for (k,v in t){-> for k, v in t do
    }              -> end            (only when `}` closes a BLOCK)
    {              -> {              (kept when it opens a TABLE)
    a && b         -> a and b
    a || b         -> a or b
    !a / a != b    -> not a / a ~= b
    null           -> nil
    // c  /* c */  -> -- c  --[[ c ]]
    ;              -> (dropped)

The one real ambiguity is `{`: it opens a block after a control header, and a
table everywhere else. This file decides that by remembering which header the
last `)` closed, and it forgets the header the moment it is used - otherwise a
call such as `if (a) Foo(b)` would arm the next table literal as a block.

Balance checks - and the one that was WRONG
-------------------------------------------
    balance_source(scp_text)   counts `{` and `}` in the .scp SOURCE, after
                               blanking comments and strings. This is the only
                               balance check that means anything for a .scp:
                               `{ }` IS the dialect's block delimiter.
    translate_report(text)     the translation plus the number of closers it had
                               to synthesise, which is the exact count of blocks
                               the source left open.

This file used to carry a keyword counter (`then` - `elseif` + `function` + `do`
+ `repeat` vs `end` + `until`) and `--check` applied it to .scp SOURCE. It could
not fire: the dialect spells blocks `{ }` and a function `func`, so a .scp has
none of those words in that role. Measured 2026-11-03 - a file cut off
mid-expression, `head -c 2000 ge\\script\\pc\\get_status_pc.scp` (braces 3 open,
2 close), scored 0 openers / 0 ends and was reported BALANCED. The same function
on the translated .lua is no better: the translator has already auto-closed the
blocks, so that text balances by construction. It is deleted rather than left
lying around. Its old `unbalanced: 8` list was keyword coincidence, not proof
those files were broken; the honest figure, counted with braces, is 0 of 4,239.

Usage
-----
    python scp_to_lua.py ..\\..\\ge\\script\\util.scp          # print Lua to stdout
    python scp_to_lua.py ..\\..\\ge\\script\\util.scp -o x.lua  # write a file
    python scp_to_lua.py --check                               # whole corpus, structure only
"""

import argparse
import os
import re
import sys

from ge_paths import GE_SCRIPT

CONTROL = {"if", "elseif", "while", "for"}
# Words that are not values, so they never take a comma in a table.
KEYWORDS = {"break", "do", "else", "end", "false", "for", "func", "function",
            "if", "in", "var", "local", "null", "nil", "repeat", "return",
            "then", "true", "until", "while", "and", "or", "not"}
NUM_RE = re.compile(r"\d+\.?\d*|\.\d+")
IDENT_RE = re.compile(r"[A-Za-z_]\w*")
WORD_MAP = {"func": "function", "var": "local", "null": "nil"}


class Translator:
    """One-pass scanner. Strings and comments are copied, never rewritten."""

    def __init__(self, text, guard=False):
        self.text = text
        self.guard = guard       # emit `_vtick()` in loop bodies (runaway guard)
        self.out = []
        self.braces = []          # 'B' = block, 'T' = table
        self.header = None        # control word waiting for its `(`
        self.pending = None       # header kind whose `{` opens the next block
        self.deferred = 0         # `end` debts owed by closed blocks
        self.last_val = False     # last token can end a value (table needs a comma)
        self.for_open = False     # the `for` header's own `(` is open
        self.header_open = False  # the header's own `(` is already marked
        self.paren = 0
        self.paren_marks = []     # 'for' for a `for` header's own paren, '' otherwise
        self.closers_added = 0    # blocks still open at EOF: closers WE invented

    def emit(self, s):
        self.out.append(s)

    def arm(self, kind):
        if kind:
            self.pending = kind

    def before_value(self):
        """The dialect lets table elements be separated by whitespace alone.

        `{ 70175 70141 56325 }` is a three-element table there; in Lua a missing
        comma is a syntax error. Insert one when a new value starts while the
        previous token already ended a value.
        """
        if self.braces and self.braces[-1] == "T" and self.last_val:
            self.emit(",")
        self.last_val = False

    def open_braceless(self):
        """`if (c) return;` - no `{`. Lua still needs `then` and `end`."""
        kind, self.pending = self.pending, None
        self.braces.append("S")
        if kind in ("if", "elseif"):
            self.emit(" then ")
        elif kind in ("while", "for"):
            self.emit(" do " + ("_vtick() " if self.guard else ""))

    def flush(self, keep_one=False):
        """Emit the `end` debts. A `}` before `else` owes none: Lua's one `end`
        closes the whole if/else, so `} else {` must not become `end else end`."""
        if keep_one and self.deferred:
            self.deferred -= 1
        while self.deferred:
            self.deferred -= 1
            self.emit(" end ")

    def run(self):
        t, n, i = self.text, len(self.text), 0
        while i < n:
            c = t[i]

            # comments: never flush, and never let a comment split a statement
            if c == "/" and t.startswith("//", i):
                j = t.find("\n", i)
                j = n if j < 0 else j
                body = t[i + 2:j]
                # A long comment, not `--`: a deferred ` end ` emitted later must
                # not land inside the comment and disappear.
                # Pad before `]]`: a body ending in `]` would close the long
                # comment early and dump the rest of the line into the code.
                if "]]" in body or "\n" in body:
                    self.emit("--" + body)
                else:
                    self.emit("--[[" + body + " ]]")
                i = j
                continue
            if c == "/" and t.startswith("/*", i):
                # `/* */` nests in this dialect: commented-out code often holds
                # its own comment, so the first `*/` is not the end.
                depth, j = 1, i + 2
                while j < n and depth:
                    if t.startswith("/*", j):
                        depth += 1
                        j += 2
                        continue
                    if t.startswith("*/", j):
                        depth -= 1
                        j += 2
                        continue
                    j += 1
                body = t[i + 2:max(i + 2, j - 2)]
                if "]]" in body:
                    self.emit("--" + body.replace("\n", " "))
                else:
                    self.emit("--[[" + body + " ]]")
                i = j
                continue

            if c in " \t\r\n":
                self.emit(c)
                i += 1
                continue

            # Strings. The game's own lexer (llex.cpp read_string) treats only
            # \" \' and \\ as escapes and keeps every other backslash literally,
            # so "pc\path" must be doubled for Lua or it is an invalid escape.
            if c in "\"'":
                self.flush()
                self.before_value()
                j = i + 1
                self.emit(c)
                while j < n:
                    ch = t[j]
                    if ch == "\\":
                        nxt = t[j + 1] if j + 1 < n else ""
                        if nxt in "\"'\\" and nxt:
                            self.emit("\\" + nxt)
                            j += 2
                        else:
                            self.emit("\\\\")
                            j += 1
                        continue
                    self.emit(ch)
                    j += 1
                    if ch == c:
                        break
                self.last_val = True
                i = j
                continue

            # operators
            if t.startswith("&&", i):
                self.flush(); self.emit(" and "); self.last_val = False; i += 2; continue
            if t.startswith("||", i):
                self.flush(); self.emit(" or "); self.last_val = False; i += 2; continue
            if t.startswith("!=", i):
                self.flush(); self.emit("~="); self.last_val = False; i += 2; continue
            if c == "!":
                self.flush(); self.emit("not "); self.last_val = False; i += 1; continue
            if t.startswith("->", i):
                self.flush(); self.emit("."); self.last_val = False; i += 2; continue
            if t.startswith("..", i):
                self.flush(); self.emit(".."); self.last_val = False; i += 2; continue

            m = NUM_RE.match(t, i)
            if m:
                self.flush()
                self.before_value()
                self.last_val = True
                self.emit(m.group(0))
                i = m.end()
                continue

            # identifiers and keywords
            m = IDENT_RE.match(t, i)
            if m:
                word = m.group(0)
                i = m.end()
                if word == "else":
                    self.flush(keep_one=True)
                    nxt = IDENT_RE.search(t, i)
                    if nxt and nxt.group(0) == "if" and not t[i:nxt.start()].strip():
                        self.emit("elseif")
                        self.header = "elseif"
                        i = nxt.end()
                        continue
                    self.emit("else")
                    self.pending = "else"
                    continue
                self.flush()
                if self.pending:            # brace-less header body
                    self.open_braceless()
                if word in CONTROL or word in ("func", "function"):
                    self.header = "func" if word in ("func", "function") else word
                    self.header_open = False
                    self.emit(WORD_MAP.get(word, word))
                    continue
                if word not in KEYWORDS:
                    self.before_value()
                    self.last_val = True
                self.emit(WORD_MAP.get(word, word))
                continue

            # braces: block or table, decided by the header we just passed
            if c == "{":
                if self.pending:
                    kind, self.pending = self.pending, None
                    self.braces.append("B")
                    if kind in ("if", "elseif"):
                        self.emit(" then ")
                    elif kind in ("while", "for"):
                        self.emit(" do " + ("_vtick() " if self.guard else ""))
                    i += 1
                    continue
                self.flush()
                self.before_value()
                self.braces.append("T")
                self.emit("{")
                self.last_val = False
                i += 1
                continue
            if c == "}":
                self.flush()
                kind = self.braces.pop() if self.braces else "T"
                if kind == "B":
                    self.deferred += 1
                elif kind == "S":
                    self.emit(" end ")
                    self.last_val = True
                else:
                    self.emit("}")
                    self.last_val = True
                i += 1
                continue

            # Parens. The `)` that closes a header arms the next `{`. A `for`
            # header drops its own parens, because Lua writes it without
            # (`for i = 1, n do`), but a paren nested inside it must still
            # nest normally - one counter, with a marker per open paren.
            if c == "(":
                self.flush()
                self.paren += 1
                self.last_val = False
                if self.header == "for" and not self.for_open:
                    self.for_open = True
                    self.paren_marks.append("for")
                    self.emit(" ")
                    i += 1
                    continue
                if self.header and self.header != "for" and not self.header_open:
                    self.header_open = True
                    self.paren_marks.append("H")
                else:
                    self.paren_marks.append("")
                self.emit("(")
                i += 1
                continue
            if c == ")":
                self.flush()
                if self.paren > 0:
                    self.paren -= 1
                mark = self.paren_marks.pop() if self.paren_marks else ""
                if mark == "for":
                    self.for_open = False
                    kind, self.header = self.header, None
                    self.arm(kind)
                    i += 1
                    continue
                self.emit(")")
                self.last_val = True
                i += 1
                if mark == "H":
                    self.header_open = False
                    kind, self.header = self.header, None
                    self.arm(kind)
                continue

            if c == ";":
                self.flush()
                if self.braces and self.braces[-1] == "S":
                    # A brace-less body before `else` must not close the chain:
                    # `if a then x end elseif b` is invalid Lua. Defer instead.
                    self.braces.pop()
                    self.deferred += 1
                self.pending = None
                i += 1
                continue

            self.flush()
            if self.pending:
                self.open_braceless()
            self.last_val = c == "]"
            self.emit(c)
            i += 1

        self.flush()
        while self.braces:
            if self.braces.pop() in ("B", "S"):
                # A block the source never closed. This is the evidence that a
                # file is cut off: the closer below exists only because we made
                # it up, and the count is what prepare.py reports.
                self.closers_added += 1
                self.emit(" end ")
        return "".join(self.out)


def translate(text, guard=False):
    return Translator(text, guard=guard).run()


def translate_report(text, guard=False):
    """(lua, closers_added). A closer was synthesised at EOF => a block was left open."""
    tr = Translator(text, guard=guard)
    return tr.run(), tr.closers_added


def translate_file(path, guard=False):
    with open(path, encoding="latin-1") as fh:
        return translate(fh.read(), guard=guard)


def translate_file_report(path, guard=False):
    """translate_file plus the synthesised-closer count."""
    with open(path, encoding="latin-1") as fh:
        return translate_report(fh.read(), guard=guard)


def _blank_source(text):
    """Blank comments and string bodies, keeping length and line breaks.

    WHY a local copy and not `count_script_builtins.strip_noise`: `/* */`
    NESTS in this dialect (the translator below relies on it - commented-out code
    often holds its own comment), and strip_noise stops at the first `*/`. That
    difference is real: npc\\evenpc\\ust\\npc_ust_eteruban.scp counts 27 braces
    with strip_noise and 19 with this one, all inside a nested comment.

    Strings go too: a `{` in a dialog line is text, not a block. The dialect's
    lexer (llex.cpp read_string) honours a backslash only before a quote or
    another backslash, so a backslash simply swallows the next character here -
    never a brace.
    """
    out, i, n = [], 0, len(text)
    while i < n:
        c = text[i]
        if c == "/" and text.startswith("//", i):
            j = text.find("\n", i)
            j = n if j < 0 else j
            out.append(" " * (j - i))
            i = j
            continue
        if c == "/" and text.startswith("/*", i):
            depth, j = 1, i + 2
            while j < n and depth:
                if text.startswith("/*", j):
                    depth += 1
                    j += 2
                    continue
                if text.startswith("*/", j):
                    depth -= 1
                    j += 2
                    continue
                j += 1
            out.append("".join("\n" if ch == "\n" else " " for ch in text[i:j]))
            i = j
            continue
        if c in "\"'":
            q, j = c, i + 1
            while j < n:
                ch = text[j]
                if ch == "\\":
                    j += 2
                    continue
                j += 1
                if ch == q:
                    break
            out.append("".join("\n" if ch == "\n" else " " for ch in text[i:j]))
            i = j
            continue
        out.append(c)
        i += 1
    return "".join(out)


def balance_source(text):
    """(openers, ends) for .scp SOURCE text: `{` versus `}`.

    FOR .scp ONLY. The two counts are equal in a whole file and differ in a file
    that was cut off (a probe taken as `head -c 2000 ...get_status_pc.scp` reads
    3 openers / 2 ends). Apply it to the translated .lua and it always balances -
    the translator closes what the source left open - which is why the count
    comes from the source, and why scp_to_lua's own translation is not the check.

    One thing it cannot see: a file that ends in a bare header (`if (x)` with no
    body at all) opens no brace. The Go compile check still catches that, because
    the translation of it is not valid Lua.
    """
    code = _blank_source(text)
    return code.count("{"), code.count("}")


def check_all(root=None):
    """Report the SHIPPED scripts whose own braces do not balance.

    A .scp in the game's tree is reported here; a file this run did not translate
    is not prepare.py's business. The count is the real baseline, not a promise
    that the file is broken: the translator closes an unclosed block, so a script
    named here still runs.
    """
    root = root or GE_SCRIPT
    files = [os.path.join(d, f) for d, _s, fs in os.walk(root)
             for f in fs if f.endswith(".scp")]
    bad = []
    for path in files:
        try:
            with open(path, encoding="latin-1") as fh:
                text = fh.read()
            translate(text)                   # still prove the translator accepts it
        except OSError:
            continue
        openers, ends = balance_source(text)
        if openers != ends:
            bad.append((os.path.relpath(path, root), openers, ends))
    print(f"files           : {len(files)}")
    print(f"structurally ok : {len(files) - len(bad)}")
    print(f"unbalanced      : {len(bad)}   (source braces {{ }}; the old keyword "
          f"count was wrong)")
    for name, o, e in bad[:20]:
        print(f"  {name}: openers {o} ends {e}")
    return bad


def dump_all(dest, root=None):
    """Write every translated script under `dest`, keeping the relative tree.

    A BUILD step, not a server feature. The Go side needs the corpus as plain
    .lua files to judge a Lua implementation against the REAL 4,000+ scripts
    instead of a sample, and doing it here means the translator stays in one
    place (Python) instead of being rewritten in Go.
    """
    root = root or GE_SCRIPT
    files = sorted(os.path.join(d, f) for d, _s, fs in os.walk(root)
                   for f in fs if f.endswith(".scp"))
    skipped = 0
    for path in files:
        rel = os.path.relpath(path, root)
        out = os.path.join(dest, os.path.splitext(rel)[0] + ".lua")
        try:
            text = translate_file(path)
        except Exception as exc:                 # a file the translator rejects
            skipped += 1
            print("SKIP %s: %s" % (rel, exc))
            continue
        os.makedirs(os.path.dirname(out), exist_ok=True)
        with open(out, "w", encoding="utf-8") as fh:
            fh.write(text)
    print("dumped: %d file(s) -> %s (skipped %d)"
          % (len(files) - skipped, dest, skipped))
    return 0


def main():
    ap = argparse.ArgumentParser(description="Translate .scp scripts to plain Lua 5.1.")
    ap.add_argument("script", nargs="?", help="one .scp file")
    ap.add_argument("-o", "--out", help="write the Lua here instead of stdout")
    ap.add_argument("--check", action="store_true",
                    help="translate the whole corpus and report unbalanced files")
    ap.add_argument("--dump", metavar="DIR",
                    help="write every translated .scp under DIR, keeping the tree")
    args = ap.parse_args()

    if args.check:
        check_all()
        return 0
    if args.dump:
        return dump_all(args.dump)
    if not args.script:
        ap.print_help()
        return 1

    lua = translate_file(args.script)
    if args.out:
        os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(lua)
        print("written:", args.out)
    else:
        sys.stdout.write(lua)
    return 0


if __name__ == "__main__":
    sys.exit(main())
