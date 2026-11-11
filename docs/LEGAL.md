# Legal and what "no game files" means

**This project is unofficial.** It is not affiliated with, endorsed by, or connected to IMC Games
or HanbitSoft. *Granado Espada* and its assets are the property of their owners.

## What this repository does not contain, ever

- No game client, no client archives (`.ipf`), no patches.
- No shipped server binaries (`release\`, `v28\`, the C# rewrite) - and they cannot be run
  anyway: they are WinLicense-locked and their starter shield is lost. **The decision is no
  cracking.**
- No game data derived from those files: no `.ies` tables, no `ge\` data tree, no navmesh
  geometry, no database backups, no key material.
- No captured traffic from any live server, and no memory dumps of the client.

## What it does contain

- **Our own code**: the Go server, the Python tools, the deployment files.
- **Our own writing**: measurements, packet layouts, protocol findings, and the reasoning
  behind them. A packet layout observed on the wire is a fact about a protocol; the project
  publishes the fact, not the game.

## What you must bring

A copy of the game client **you have the right to use**. The tools in `docker-port\src\` read
that copy and produce the tables the server loads (see [DATA](DATA.md)). Nothing is uploaded
anywhere; the tools only read.

## What the project refuses to do

- Run a public or commercial server, or support anyone who does.
- Break the shipped binaries' protection.
- Redistribute the game or any part of it.

## The licence of this project's code

**Not decided yet.** The reference project this one learned its structure from
(`offlinespada`) is MIT, and MIT is the natural choice for the same reason: the code is a study
project and an invitation to read. Nothing is published until the owner picks a licence and the
`LICENSE` file exists - a repository with no licence is "all rights reserved" by default, which
is the opposite of the intent.
