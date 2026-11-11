# Architecture - three programs and a database

## The shape

```
   your game client
          |
          | login + character list            TCP 7001
          v
   +--------------+      "which map, where?"       +----------------+
   |   barrack    | -----------------------------> |  zone manager  |  TCP 7010
   | goserver.exe |                                | goserver_      |
   +--------------+                                | zoneman.exe    |
          |                                        +-------+--------+
          | entry: address of the zone for the map         | starts / stops
          v                                                v
   +---------------------------------------------------------------+
   |  one zone process per map   goserver_zone.exe   TCP 7010+     |
   |  the map, its monsters, NPCs, combat, items, dialogs, shops    |
   +-------------------------------+-------------------------------+
                                   |
                                   v
                        PostgreSQL  `ge_data`  (container granado_pg)
```

- **barrack** - login, the family, the three characters, the character-creation screens, the
  map list, and "Start Game" (which answers with the address of the zone for that map).
- **zone manager** - keeps a set of maps **warm** (started at boot), starts a map **on demand**
  when the barrack asks, stops idle ones. It reads its policy from `server.json`; it does not
  hard-code a map list.
- **zone** - one map per process: the sight ring, movement, combat, monsters, drops, NPCs,
  dialogs, shops, and the game's own script engine (Lua, produced from the game's `.scp` files).
- **PostgreSQL** - the live database: accounts, families, characters, bags, positions, the map
  plan. The original SQL Server (`granado_espada`) is kept only as the reference and the oracle.

## Why three programs and not one

One process per map is what makes the world scale and fail small: a map that crashes takes only
its own players, and the manager replaces it. It is also what makes the cost predictable: a zone
process is a few tens of megabytes idle, and the path engine's Wine stack is paid once per
container, not once per map.

## The Go packages (`docker-port\goserver\`)

431 files, ~157,000 lines. Sizes are lines, measured 2026-11-11.

| Package | Lines | What it owns |
|---|---|---|
| `zone\` | 120,623 | Everything inside one map: entry, movement, sight ring, combat, monsters, drops, NPCs, dialogs, shops, bags, buffs, skills, respawn |
| `barrack\` | 9,150 | The lobby: login, family, characters, lists, Extend, bag verbs, Start Game |
| `script\` | 11,187 | The game's own script engine: the Lua host, the builtins, the missing-name accounting |
| `protocol\` | 2,149 | Packet ids, layouts, record strides - the wire, in one place |
| `world\` | 1,008 | The zone registry: which zone serves which map, at what address, is it alive |
| `item\` | 948 | Item data: stats, equip slots, catalogue, prices |
| `config\` | 597 | `server.json` for all three programs, one resolution rule |
| `db\` | 398 | The PostgreSQL connection and migrations |
| `account\` | 165 | Account rules (create, password hash) shared by the tool and the console |
| `gatedb\` | 154 | The gate/warp table |
| `address\`, `text\` | - | Address helpers, locale/text ids |

`cmd\` holds one program per folder: `zoneman` (the manager), `admin` (operator console + the
`/api/register` route), `mkaccount`, `loadgen`, `scriptcensus` (how much of the script corpus
runs), `mintkit`, `mktext`, `seedplan`.

## Where a behaviour is decided

| Question | File |
|---|---|
| What packet id and byte layout? | `docker-port\goserver\protocol\` - and the client's own table, which you dump with `src\dump_cmds.py` |
| What does the game's script do? | `docker-port\goserver\script\` (the host and its builtins) |
| How does movement pace itself? | `docker-port\goserver\zone\` - `move.go`, `chase.go`, `walk` |
| How is damage computed? | `docker-port\goserver\zone\attack*.go`, `splash` |
| Where do the tables come from? | [DATA](DATA.md) |
| How is it deployed? | `docker-port\deploy\` |

## The one thing a new reader gets wrong

The server does **not** re-implement the game's rules in Go. It loads the game's own data tables
and runs the game's own scripts; the Go code is the plumbing around them. When a behaviour looks
missing, the first question is "which table or script is not loaded", not "which function is
missing".
