# Credits

Nothing here was written in a vacuum. Two sources carried most of the weight.

## offlinespada - the sibling project

**<https://gitlab.com/2to2/offlinespada>** - MIT licensed. A public port of the *same game*, in
Rust. Credit is owed for three things:

- **An independent cross-check.** Their v32 track is our client generation, so their packet ids,
  sizes and format work were read against ours - and where the two disagreed, the shipped client
  settled it. (Their v28 work targets build 285146 and does **not** transfer; the pages here say so
  wherever it matters.)
- **The finding discipline.** The labels this project uses everywhere - **PROVEN**, **INFERRED**,
  **GAP**, each naming the build it was measured on - were adopted from their knowledge base on
  2026-11-08. It is the single reason these docs are readable a month later.
- **The shape of this repository.** One page per job, a facts record, credits, contributing, a
  licence, a public tree that carries code and method but no game files: the layout follows theirs.

No source code from their tree is copied into this one (theirs is Rust, ours is Go). Where a
figure, a formula or a conclusion came from them, the code comment and the docs say so.

## GE V32 DEVs Discord server

The community around the **v32 generation** of this client: the reference for how this build
behaves, what its data means, and where questions about this generation get answered by people who
have run it far longer than we have.

If you answered a question that ended up in this code and you want your name here, open an issue
and it is added - this page has no interest in being vague.

## The game and its owners

*Granado Espada*, its client and its assets belong to **IMC Games** and its licensors. This project
is unofficial, non-commercial, and ships **no game files**: no client, no archives, no tables, no
scripts, no captures. You bring your own copy.

The shipped server programs are theirs too. They cannot run any more (time-locked, and the starter
shield is lost) - which is why this exists. The decision here is **no cracking**.

## Tools this project did not write

Go, PostgreSQL, Docker, Ghidra, x64dbg, and the client's own path engine (PathEngine).
