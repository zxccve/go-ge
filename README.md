# ge_server - a replacement server for Granado Espada

A from-scratch **server** for the game client **Granado Espada**, written in **Go** on
**PostgreSQL**. The shipped servers cannot run any more (WinLicense time lock, starter shield
lost), so this project speaks the same protocol to the real client: a barrack, a zone manager,
and one zone process per map.

**Unofficial.** Not affiliated with IMC Games or HanbitSoft. *Granado Espada* is a trademark of
its owners. **No game files are included** - you bring your own copy.

## What it is

- **A Go server.** Barrack (the lobby), a zone manager, and one process per map: movement,
  combat, monsters, drops, NPCs, dialogs, shops, and the game's own script engine.
- **A set of tools.** The Python programs in `docker-port/src/` are how the client's tables are
  extracted, decoded and turned into what the server loads.
- **A method, not magic numbers.** Every behaviour is either measured (a capture, a log, a
  binary) or it is marked as a guess.

## What it is not

- **Not a game download.** No client, no archives, no `.ies` tables, no scripts, no database
  dumps, no captures. You bring files from a copy you have the right to use.
- **Not a public server.** It exists for a small private group.
- **Not version agnostic.** It is built against **one** client build (the Andromida install,
  file version 321819 - the v32 generation). A different client means adapting the packet ids and
  layouts yourself: see [docs/CLIENT-VERSIONS.md](docs/CLIENT-VERSIONS.md).
- **Not finished.** See [DEVELOPING](docs/DEVELOPING.md) for what runs today.

## What you need

| You bring | The project provides |
|---|---|
| A Granado Espada client you own (the Andromida build is the anchor) | the server code |
| Go 1.27 and Docker | the tools that read your client and produce its tables |
| Windows for a native run (the path engine is 32-bit Windows; it runs under Wine) | the Docker image for Linux |

## Start here

| Page | What it answers |
|---|---|
| [docs/DEVELOPING.md](docs/DEVELOPING.md) | prerequisites, build, run, first account, tests |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | the three programs, how they talk, the package map |
| [docs/DATA.md](docs/DATA.md) | how your client's tables become what the server loads |
| [docs/CLIENT-VERSIONS.md](docs/CLIENT-VERSIONS.md) | the client this is pinned to, and how to adapt it to yours |
| [docs/LEGAL.md](docs/LEGAL.md) | the boundary: no game files, ever |
| [CONTRIBUTING.md](CONTRIBUTING.md) | what a useful contribution looks like here |

## Credits

Two sources carried most of the weight: **offlinespada** (the MIT sibling port - an independent
cross-check on the protocol, and the finding discipline and repository shape this project copied)
and the **GE V32 DEVs Discord server** (the community around this client generation). The full
acknowledgement, including what came from where, is [CREDITS.md](CREDITS.md).
