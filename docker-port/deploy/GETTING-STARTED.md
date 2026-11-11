# Run your own server, in plain terms

This is the short version for somebody who wants their own Granado Espada server on a rented
machine. It assumes you can copy files to the machine and run one command in its terminal. If you
want the engineering record instead (why each piece is shaped the way it is), read `README.md` next to
this file.

**What you are running: two containers.** One is the database. The other is the game server, which
starts every map as a child process inside itself. Nothing else is installed on the machine - no Go,
no Python, no Wine on the host.

---

## 1. What you need

| | |
|---|---|
| machine | any Linux VPS, VDS or dedicated server; 4 GB RAM runs a trimmed world, **8 GB is comfortable** |
| disk | ~20 GB (the image is 1.3 GB, the game data ~0.5 GB, the rest is your database and logs) |
| software | Docker with the compose plugin - nothing else |
| from your client | the `ge` folder: `dictionary\` and `xml\` are what the server reads (~500 MB) |
| time | about 15 minutes, most of it the first image build |

## 2. Build the image once, on your own PC

Nothing is compiled on the server; it only pulls a finished image.

```
docker build -f deploy/Dockerfile -t geserver:1 .
docker tag geserver:1 your-registry-name/geserver:1
docker push your-registry-name/geserver:1
```

No registry account? Send it straight over SSH instead - it is about 320 MB compressed:

```
docker save geserver:1 | gzip | ssh you@your-server 'gunzip | docker load'
```

The build needs the generated data already in the repo (`data\`, `navmesh\`, `out\lua_corpus\` and the
nine `out\*_image.bin` tables). Those are produced on a development machine by `src\prepare.py`; they
are not in git.

## 3. The only file you write by hand: `.env`

Copy `deploy/env.example` to `.env` and edit it:

```
POSTGRES_PASSWORD=pick-something-long-and-random
GE_CLIENT_DIR=/srv/ge
```

- **`POSTGRES_PASSWORD`** is the database password. It lives here and nowhere else. `.env` is
  git-ignored - never commit it, and never put it in `server.json`, which is public.
- **`GE_CLIENT_DIR`** is where you copied the client's `ge` folder on the server.
- If you skip this file, compose refuses to start and says which variable is missing. That is on
  purpose.

## 4. Start it

```
docker compose -f deploy/compose.yaml up -d
docker compose -f deploy/compose.yaml logs -f geserver
```

You should see, in this order:

```
zoneman: config: /app/out/server.json
zoneman: policy: max 60, idle 5m0s, linger 15m0s, ... base-port 7010, prepare false
zoneman: plan from zone_daemon: 44 row(s), 30 warm, any map on request
zoneman: watching zones for requests ...
```

**On a brand-new database the manager creates the schema itself**, then reads the plan - which is
seeded with 44 map rows, 30 of them warm. That is why the first start takes a few extra seconds. If
30 warm maps is too much for your machine, trim the `zone_daemon` rows marked warm (see §8).

## 5. Open the ports - and only these

| port | what | open to the internet? |
|---|---|---|
| 7001 | login / character list | **yes** |
| 7010 and up | one per map, as maps start | **yes** |
| 5433 | the database | **NEVER** |

The database is already bound to `127.0.0.1` inside the compose file, so it is not reachable from
outside even if you do nothing. The provider's firewall is a second lock, not the lock: deny
everything except 7001 and the map range.

The address your client is told to dial is `listen.advertise` in `server.json`. Leave it empty and the
server reads it from the database's machine row; on a VPS set it to your public IP.

## 6. Make your first account

One command inside the image creates an account (the login row and its password row).
It deliberately does **not** create a family or a character - the client asks for both
in the game (`CB_CREATE_COMMANDER`, `CB_CREATE_CHAR`), so an account made here logs in to
an empty barrack, which is the screen where the player types their family name.

```
docker compose -f deploy/compose.yaml exec geserver /app/out/mkaccount <member_id>
```

`<member_id>` is what the client sends as its account name - the id the launcher's
`accounts.ini` maps a name to. It refuses an id that already exists (case-insensitive),
and an id containing `:`, `=` or spaces, because those split the launcher's line.

Add `-password <pw>` and the row is stored the way the original engine stores it -
unsalted MD5 hex in `account_pass.pass_md5`:

```
docker compose -f deploy/compose.yaml exec geserver /app/out/mkaccount -password secret <member_id>
```

It prints `ACCOUNT_ID` and `COMMANDER_ID=0`. Add the same id to the player's
`accounts.ini` next to their launcher (`name=<id>:<password>`) and they can log in.

**For a LAUNCHER, the same thing over HTTP** - one JSON route on the console that already
runs (no website, no domain):

```
POST http://127.0.0.1:7099/api/register
{"member_id":"newplayer","password":"secret"}
-> 200 {"account_id":4,"member_id":"newplayer","note":"..."}
-> 400 {"error":"account \"newplayer\" already exists (account_id 4)"}
```

The console binds `127.0.0.1` by default, so this is local only today. If a remote launcher
must reach it, expose that one route and add the alpha guards first (rate limit, a check for
suspicious rows) - a public POST that creates accounts is a spam target. `/register` in a
browser is the same endpoint as a form, for your own use.

**Two limits, stated:** the password in `accounts.ini` is a LOCAL launcher check - the
barrack does not verify the client's digest yet; and once the account logs in, the family
name is created by the client (`CB_CREATE_COMMANDER`, answered since 2026-11-11) and the
first character is still a card, so a new player can make a family but not yet a character.

## 7. Back it up - the database is the only thing you cannot rebuild

```
docker compose -f deploy/compose.yaml exec -T db pg_dump -U granado ge_data > ge_$(date +%F).sql
```

Run that nightly, keep it off the machine. Restoring:

```
cat ge_2026-11-11.sql | docker compose -f deploy/compose.yaml exec -T db psql -U granado ge_data
```

Everything else - the image, the maps, the script corpus - is replaceable from the repo.

## 8. Change a setting

Everything lives in `deploy\server.json`. It is public, has no secrets, and every value can also be
passed as a flag - but you should never need a flag.

| section | what it decides |
|---|---|
| `listen` | which address each role binds, and what the CLIENT is told to dial |
| `paths` | where the data, logs and registries are |
| `engine` | how the route helper is started (`wine` in the container, the `.exe` on Windows) |
| `mode` | which role this process is (barrack or zone) and its map |
| `manager` | the fleet's rules: which binary starts a map, the port base, `max` maps at once, `idle` for an unused map, `linger` for one a player left, `memlimit`, `prepare` |

Two ways to apply an edit:

1. **Rebuild** the image (normal, keeps one artifact).
2. **Without a rebuild:** copy `server.json` to the server, uncomment the mount line in
   `deploy\compose.yaml` (`/srv/geserver/server.json:/app/out/server.json:ro`), then
   `docker compose restart geserver`. The file is read at startup by the manager and by every map.

To trim the warm set, edit the `zone_daemon` rows in the database (a `warm` flag per map) - the
manager reads that table at boot.

## 9. Update to a new build

```
docker build -f deploy/Dockerfile -t geserver:2 . && docker push ...
# on the server
docker compose -f deploy/compose.yaml pull && docker compose -f deploy/compose.yaml up -d
```

Your database volume and mounted game data survive. Take the backup first anyway.

## 10. When it does not work

| symptom | look here |
|---|---|
| container restarts in a loop | `docker compose logs geserver \| tail -40` |
| "the zone plan could not be read" | the database is not reachable - check `POSTGRES_PASSWORD` and that `db` is healthy (`docker compose ps`) |
| maps never appear | the warm set: `docker compose logs geserver \| grep 'plan from'` |
| client cannot connect | port 7001 closed at the provider, or `advertise` points somewhere the client cannot reach |
| a map refuses | the `max` cap in the `manager` section, or that map has no `zone_daemon` row (it starts when a gate asks) |
| the game starts but text is blank | the client folder is not mounted at `/ge` (dictionary + xml) |

## What this guide does not cover, on purpose

- **Building the generated data** (`src\prepare.py`) - a development step, done before the image build.
- **The client** - use your own client install; this is the server only.
- **A website or a patcher** - not built yet.
- **Windows** - the repo is developed on Windows and `goserver\server.json` carries that platform's
  `manager` block (paths written relative to the `goserver\` folder, where the file lives). Run it from
  there with no flags at all:

  ```
  cd docker-port\goserver
  ..\out\goserver_zoneman.exe
  ```

  The manager itself runs from `goserver\`, and the maps it starts inherit that folder, which is what
  the zone defaults are written for.
