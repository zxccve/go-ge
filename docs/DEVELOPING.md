# Developing - from clone to a running world

Reading the code needs nothing but Go. **Running** it needs a Granado Espada client you have the
right to use, because the server loads the game's own tables (see [DATA](DATA.md)).

## What you need

| Thing | What this project uses | Why |
|---|---|---|
| Go | 1.27 | the server |
| Docker | any recent release | PostgreSQL 16 in a container |
| Python | 3.13 | the tools in `docker-port\src\` - they produce everything the server loads |
| A game client | the Andromida build | the client whose protocol this speaks |
| Windows | 10/11 for a native run | the path engine is a 32-bit Windows DLL; on Linux it runs under Wine |

## Build

```bash
cd docker-port/goserver
go build -o ../out/goserver.exe .                       # barrack
go build -o ../out/goserver_zone.exe .                  # one zone
go build -o ../out/goserver_zoneman.exe ./cmd/zoneman   # the manager
```

On Linux use `build.sh` in the same folder - the deploy image does exactly that.

## Database

```bash
docker run -d --name granado_pg \
  -e POSTGRES_USER=granado -e POSTGRES_PASSWORD=<your-password> -e POSTGRES_DB=ge_data \
  -v granado_pg_data:/var/lib/postgresql/data \
  -p 127.0.0.1:5433:5432 postgres:16
```

The `127.0.0.1:` prefix is the security control: it publishes the port on loopback only. The
server and the tools read `GE_PG_DSN` (and `GE_SQL_PASSWORD`); the password is never in a file.

The schema builds itself: every program that needs the database runs the migrations first, so a
brand-new database is created on the first start.

## Run

```bash
cd docker-port/goserver
../out/goserver.exe -role barrack                        # 1. the lobby, port 7001
../out/goserver_zoneman.exe -config server.json          # 2. the manager, port 7010
```

Every program reads `server.json` from the working directory; a flag overrides it. The manager
keeps a **warm** set of maps running and starts any other map on demand when the barrack asks
for it, so there is no map list to maintain by hand.

Point your client's server list at `127.0.0.1:7001` and start it through its own launcher (a bare
`ge.exe` exits by itself).

## The first account

```bash
cd docker-port/goserver
go run ./cmd/mkaccount -id YourName -password secret
```

That creates one account and **no family**: the client then shows its own "Create A Family"
screen, which is the path a new player takes. The password is stored as an unsalted MD5, which is
what the original does.

## Test

```bash
cd docker-port/goserver
go build ./... && go vet ./... && go test ./... -count=1
```

The full run takes about five and a half minutes; the `zone` package is the slow one.

**A test must fail with its fix removed.** In this project a fix is not done until a test proves
it was needed: break the code, watch it go red, restore it, watch it go green. A test that passes
on the broken code is not evidence.

## Where to change something

| You want to change | Go here |
|---|---|
| a packet layout | `docker-port\goserver\protocol\` |
| a behaviour inside a map | `docker-port\goserver\zone\` |
| the lobby | `docker-port\goserver\barrack\` |
| a script builtin | `docker-port\goserver\script\` |
| the map fleet, ports, idle policy | `docker-port\goserver\cmd\zoneman\` + `server.json` |
| the database shape | `docker-port\goserver\db\` (numbered, idempotent migrations) |
| deployment | `docker-port\deploy\` |

## Two things a newcomer should know

1. **The server does not re-implement the game's rules in Go.** It loads the game's data tables
   and runs the game's own scripts; the Go code is the plumbing around them. When a behaviour
   looks missing, the first question is "which table or script is not loaded", not "which
   function is missing".
2. **Paths come from the tree, not from a fixed disk.** `src\ge_paths.py` computes the root from
   the file's own location and exposes `GAME_ROOT` - the repository root, where the game tree
   lives. Set `GE_GAME_ROOT` when your game files are somewhere else and every tool follows it.
   No script carries a hard-coded drive path; that is a rule the project checks (it was 52 files
   on 2026-11-11, now zero).

## Style the project enforces

- `gofmt`; one program per folder under `cmd\`; small packages.
- A comment explains **why** and cites its evidence (`PlayerChar.cpp:1393`, a capture, a log
  line). A comment that restates the code is noise.
- Never commit a secret. The database password lives in the environment.
