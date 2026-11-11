# deploy - the whole stack in containers

The project target is the WHOLE stack in Docker: the game server (barrack + zone manager + zones), the
database, patching and the website (`ROADMAP.md`, M12). This folder holds the Linux half of that.

| file | what it is |
|---|---|
| **`GETTING-STARTED.md`** | **the plain-terms guide for somebody deploying this** - what to rent, how to build and ship the image, the one `.env` you write by hand, how to start it, which ports to open (and which to keep shut), how to back up, how to change a setting, and what is honestly not finished yet |
| `Dockerfile` | Debian 12 + Wine 32-bit + **both** Go binaries built from `goserver/` (`goserver`, and the manager `zoneman`) + `data/`, `config/`, `navmesh/`, `out/lua_corpus/` and the nine generated table images from `out/` |
| `server.json` | the container's paths, the ONE line that differs from Windows (`engine.command = "wine"`, `engine.args = ["/app/navmesh/pathtool.exe"]`), and the `manager` section: the fleet's rules (bin, base port, log dir, max/idle/linger/retain, memlimit, prepare) - the compose command carries no policy |
| `compose.yaml` | PostgreSQL 16 + the game server, `network_mode: host` on a real Linux host |
| `env.example` | copy to `.env` and set the database password (never commit `.env`) |

Build and run:

    docker build -f deploy/Dockerfile -t geserver:test .
    # the image's default process is the MANAGER (warm set from the zone_daemon plan):
    docker run --rm -e GE_PG_DSN=... -v /srv/ge:/ge:ro geserver:test
    # a single zone for a smoke test instead:
    docker run --rm -p 7102:7102 -e GE_PG_DSN=... -v /srv/ge:/ge:ro geserver:test \
      /app/out/goserver -role zone -map 10 -addr :7102

## What is PROVEN on this machine (2026-11-10, Docker Desktop, linux/amd64)

- **PathEngine ANSWERS under Wine.** `printf "faces\nvalid 24000 26000 146\nquit\n" | wine
  /app/navmesh/pathtool.exe /app/navmesh/mesh/btl_ktv03.tok` inside the image printed
  `pathengine interface 2.39 release 4`, `faces 3067`, `valid 24000 26000 146 0 0`, exit 0. The zone's own
  start line agrees: `zone: path engine: /app/navmesh/pathtool.exe on /app/navmesh/mesh/btl_ktv03.tok`.
- **A zone runs in the container and a player can enter it**: `loadgen -bots 2` against the container on map
  4063 (429 bodies) gave `entered 2, failed 0`, 165,954 bytes and 157 move echoes per bot, fan-out p50 1.05 ms.
- The engine process is visible inside the container: `/app/navmesh/pathtool.exe /app/navmesh/mesh/btl_ktv03.tok`
  plus `wineserver32`.

## The image is 1.30 GB, not 3.2 GB (2026-11-11)

The runtime layer was 2.15 GB of Wine and Debian furniture for a **32-bit console PE**. Nothing in the
baked inputs moved: `data/` 38 MB, `navmesh/` 37 MB, `out/lua_corpus/` 49 MB and the nine `out/*_bin`
table images 43 MB are the same files.

| what | before | after |
|---|---|---|
| `debian:bookworm-slim` base | 85.3 MB | 85.3 MB |
| the Wine layer (`apt-get install` + prefix) | 1.53 GB + 616 MB = **2.15 GB** | **688 MB** |
| app: both Go binaries + the baked inputs | 202 MB | 202 MB |
| **`docker images` DISK USAGE** | **3.24 GB** | **1.30 GB** |
| `docker images` CONTENT SIZE (compressed) | 808 MB | 324 MB |
| `du -xsh /` inside the container | 2.3 GB | 928 MB |

Where the 2.3 GB went, measured inside the trail image:

| path | before | after | what it was |
|---|---|---|---|
| `/usr` | 1.5 GB | 720 MB | |
| `/usr/lib/x86_64-linux-gnu/wine` | 643 MB | **0** (gone) | `x86_64-windows` 638 MB of 64-bit DLLs + `x86_64-unix` 5.2 MB |
| `/usr/lib/i386-linux-gnu/wine` | 555 MB | 555 MB | the 32-bit builtin DLL set - KEPT, it is what runs pathtool |
| `/wine` (the baked prefix) | 587 MB | **2.7 MB** | 587 MB of DLLs wineboot had COPIED in |
| `/usr/share` | 29 MB | 14 MB | doc, man, locale, iso-codes |
| `/app` | 193 MB | 193 MB | the baked inputs |

The cuts, in the order they pay:

1. **No 64-bit Wine at all** (643 MB). pathtool.exe is the only PE that ever runs, and Wine 8.0 on
   Debian runs a 32-bit prefix from `i386-windows` + `wineserver32` alone. Two traps:
   `apt-get install libwine` resolves to **`libwine:amd64`** (667 MB) - the install line names
   `libwine:i386` - and `/usr/lib/x86_64-linux-gnu/wine` is removed anyway in case a dependency drags
   it back. PROVEN by moving both directories away, creating a **fresh** prefix and running pathtool.
2. **The prefix's duplicated DLL bytes** (584 MB). `wineboot` COPIES every builtin into
   `drive_c/windows/system32` and `winsxs`; they are not hardlinks (link count 1). They cannot simply
   be deleted - that gives `could not load kernel32.dll, status c0000135` - so each copy is replaced by
   a **symlink** to the builtin it came from (`i386-windows`, plus `nls` for the `.nls` tables): 799
   symlinks, 87 real files, and the prefix (registry + layout) still works. Baking it is kept on
   purpose, so a zone start still pays nothing and the runtime still needs no writable HOME.
3. **Documentation and locale furniture** (~23 MB): `/usr/share/{doc,man,locale,iso-codes}`.
4. **libicu, gstreamer and the video codecs** (~62 MB) that libwine's dependency list drags in. Nothing
   on this path collates or decodes media; PROVEN by moving them out and running **both** pathtool and
   a fresh `wineboot -u` (both exit 0). The packages stay listed in dpkg's database with their files
   stripped - `apt-get install --reinstall` puts any of them back.

**A delete in a later layer does not shrink an image** (the earlier layer keeps the bytes), so all four
cuts happen inside the one `RUN` that installs Wine - the `du -sh` at its end is the guard.

Proof on the slimmed image (2026-11-11, Docker Desktop, linux/amd64, `geserver:test` 1.30 GB):

- `printf "faces\nvalid 24000 26000 146\nquit\n" | wine /app/navmesh/pathtool.exe
  /app/navmesh/mesh/btl_ktv03.tok` -> `pathengine interface 2.39 release 4`, `mesh loaded, 3067 3D faces`,
  `faces 3067`, `valid 24000 26000 146 0 0`, exit 0.
- A zone on map 4063 (`-p 7102:7102`, client tree at `/ge`): `zone: path engine:
  /app/navmesh/pathtool.exe on /app/navmesh/mesh/btl_ktv03.tok`, `429 monsters placed (237 mesh
  quer(ies) 533 ms ...)`, `zone: UP on :7102`. `loadgen -bots 2 -ramp 1 -pattern walk -duration 12s
  -spread 0` -> **`entered 2, failed 0`** (156 move-echo samples, p50 8.69 ms).
- The manager `CMD` still starts children: a `request_4063.json` gave `zoneman: started map 4063 on
  :7017 (pid 30)` and the child logged the same `zone: path engine:` line.

## FIXED: the build context carried 16.4 GB of client crash dumps (2026-11-11)

`.dockerignore` dropped `cache/`, `backup/`, `captures/` and `**/*.log` - but nothing under `logs/`
except its `.log` files, and `logs/crashdumps/` + `logs/wer/` hold **12 client dumps of up to 2.0 GB,
15.9 GB in all**, plus `screenshots/` (186 MB). Every `docker build` shipped them to the daemon before
compiling a line.

| build context | before | after |
|---|---|---|
| rules | `**/*.log` only under `logs/` | `logs/` and `screenshots/` dropped whole |
| MEASURED, `FROM scratch` + `COPY . /x`, `--progress=plain` | **16.4 GB** (arithmetic: 68.5 MB content + 15,553 MB non-`.log` under `logs/` + 186 MB screenshots) | **68.47 MB, 5.0 s** |

The after number is the measured line `transferring context: 68.47 MB 5.0s`; the before number is
that measured content plus the two directories, not a second transfer - no 16 GB was ever sent to
prove it. Nothing the image reads lives under either path: runtime logs are the compose mount
`../out/logs:/app/out/logs`, and the screenshots are client captures.

## FIXED: the script corpus index (2026-11-10, commit `d5da6acb`)

The first container run exposed a silent killer: `.index_image.bin` is built on Windows and stores the walk's
paths - **4,239 of 4,239 interned entries were `..\out\lua_corpus\...`** - so on Linux every script load failed
and monster AI, skill damage, monster HP and DEF were dead while the server looked healthy.

`script\index_image.go` now re-roots those entries on READ (`resolveIndexPath`): a legacy entry is cut at the
last component equal to the corpus folder's own name and its tail is joined to the runtime root; a portable
(corpus-relative) entry is joined as-is; anything else is returned unchanged, so nothing that worked can
regress. The WRITER is untouched - the Windows zone reads the same string it always did. RED proof: with the
re-root removed, "4239 of 4239 interned paths do not resolve"; green: all 4,239 resolve, and the container log
now reads `skill damage script armed`, `monster defence script armed`, `monster HP script armed`,
`buff ENTER scripts armed`, with zero `no such file` lines.

## FIXED: the four generated inputs (2026-11-10, `t_629cf3c8`)

One cause, not four. Every one of those defaults is **relative to the process's working directory**, and the
image ran the binary with `WORKDIR /app` while the binary is `/app/out/goserver`. So `../data/item_props.json`
resolved to `/data/item_props.json` - a file that WAS in the image and still read as `no such file`. Windows
never sees this because the Windows server runs FROM `out\`, where `../` means what it says.

The image now uses `WORKDIR /app/out`, and its three levels are the repo's: `/app` is `docker-port` and `/` is
the repo root, exactly as `D:\ge_server\docker-port` sits under `D:\ge_server`. That is why the client tree is
mounted at **`/ge`** and not `/app/ge` - `../../ge` from `out` is the repo root's `ge`.

Nothing in Go changed, and the Windows run is untouched: same binary, same `out\`, same relative defaults.

| input | how the container gets it |
|---|---|
| `out\text_image.bin` (31 MB) | **copied** - `.dockerignore` re-includes it and `COPY` puts it at `/app/out/text_image.bin` |
| `out\buff_image.bin` (772 KB) | **copied** - `/app/out/buff_image.bin` (the same file the manager's prepare step builds) |
| `out\item_image.bin`, `monster_image.bin`, `skill_image.bin`, `mapdrops_image.bin` (9 MB together) | **copied** - these never logged `no such file`: they were silently regenerated from the XML on every start, which is a slow start, not a wrong one. Same rule, one `COPY` |
| `ge\dictionary\dictionary_local_serverpack.xml` (17 MB) | **mounted** with the whole client tree, at `/ge/...` - see below |
| `ge\dictionary\dictionary_local.xml` (20 MB) + `ge\xml\datatable_*.xml` | **mounted** (the `-dict` in `server.json`, and `zone.XMLDir()`) |
| `data\item_props.json` (4.6 MB) | **copied** already, at `/app/data/item_props.json` - only its path was wrong |
| the nine `out\*.bin` images themselves | **generated** on the host, and **regenerated inside the container** on demand: the zone rewrites any image that is missing or older than its source (proof below). The copies are a start-up saving, not a hidden dependency |

**The client mount is REQUIRED**, and it goes on `/ge`:

    -v /srv/ge:/ge:ro          # compose: GE_CLIENT_DIR, default /srv/ge

PROVEN in this image (2026-11-10, docker, linux/amd64, zone on map 4063):

- **zero `no such file` lines** in the whole zone log, and no `NO STATUS SHEET`. The positive lines are there:
  `dialog text from the SHARED image /app/out/text_image.bin - 210218 entries mapped read-only`,
  `status sheet source armed - 24975 item class(es), 45 growing curve(s)`,
  `buff table from the IMAGE /app/out/buff_image.bin - 1962 row(s), no XML parse`.
- the four paths, read from the container's own working directory (`docker exec -w /app/out`):
  `../out/text_image.bin` 32,041,500 B, `../data/item_props.json` 4,744,287 B,
  `../../ge/dictionary/dictionary_local_serverpack.xml` 17,004,099 B, plus `../../ge/xml/datatable_buff.xml`.
- **the server pack is READ, not merely present.** The one step that opens it is a text-image rebuild, so with
  `text_image.bin` removed the zone rebuilt it inside the container - `zone: rebuilt the SHARED text image
  /app/out/text_image.bin (30.6 MB) - it does not exist yet` - and mapped the same **210,218** entries as the
  prebuilt image. The pack is a required read in `text\Load` / `SaveImage`, so a wrong path fails that build
  instead of passing it.
- `loadgen -bots 2 -ramp 1 -pattern walk -duration 12s` -> `entered 2, failed 0`, fan-out p50 10.46 ms
  (157 move-echo samples). The p95 of 115 ms is a 2-bot synthetic run, not a server number.

The manager (`cmd\zoneman`) IS built into this image and is its `CMD` - this replaces the old note
here that adding it would need `python` + `src\prepare.py` in the image.

## The manager runs in the image (M12a, 2026-11-11, container `geserver:test`)

`cmd\zoneman` is built beside `goserver` from the same source and is the image's default process:

    /app/out/zoneman -bin /app/out/goserver -base-port 7010 -logdir /app/out/logs -prepare=false

PROVEN in the image (docker, linux/amd64, run as
`-warm 10,15 -idle 1m -linger 0 -retain 1h -db ""`):

| seam | the line from the run |
|---|---|
| **2 maps up** | manager: `zoneman: started map 10 on :7010 (pid 13)` and `started map 15 on :7011 (pid 14)`; each child: `zone: announced map 10 (reb_basic) at 127.0.0.1:7010 in /app/out/zones/zone_7010.json` / `announced map 15 (coim_basic) at 127.0.0.1:7011` |
| **each child's wine engine** | each child's OWN log carries `config: read server.json (role zone, map 10, engine "wine /app/navmesh/pathtool.exe")` and `zone: path engine: /app/navmesh/pathtool.exe on /app/navmesh/mesh/reb_basic.tok` (map 15: `coim_basic.tok`) |
| **presence files** | `/app/out/zones/zone_7010.json` and `zone_7011.json`, each `{"map_id":…,"port":…,"pid":…,"managed":true}` |
| **a bot enters a manager-started zone** | on-demand map 4063, started from a request file: `loadgen -bots 2 -ramp 1 -pattern walk -duration 15s` against `127.0.0.1:17012` -> `entered 2, failed 0`, 194 move-echo samples |
| **one idle stop** | `zoneman: map 4063 stopping on :7012 (idle: 0 players for 1m0s (grace 1m0s))`; its presence file was gone afterwards and the two warm files stayed |

### Why the child sees the wine engine (no `-config` was added)

`main.go` starts each zone with explicit flags and **no `-config`**, so the child's search is
`config.Preload()`: `server.json` in the working directory first, then beside the executable. The
manager is the image's `CMD`, so its working directory is `WORKDIR /app/out` - where the image's own
`server.json` carries `engine.command = "wine"`. The child's `config:` line above is the proof; the
launch did not need a `-config` flag.

### Python: NOT in the image, `-prepare=false` is the container default

**Decision (owner, 2026-11-10 23:30): the VPS/container stack is DEPLOYMENT ONLY.** Everything is
built and developed on the Windows box; the VPS pulls and runs. So the runtime image carries no
Python (`python` and `python3` are both absent) and `src\prepare.py` is never copied in; the corpus,
`item_props.json`/`item_stats.json` and the table images are baked at BUILD time on the dev machine.

The skip is **not silent**: the manager logs

    zoneman: ignition OFF (-prepare=false) - the corpus and the table images are taken as BUILT;
    a stale one is invisible here: run src\prepare.py on the dev machine and rebuild the image

A stale corpus is then **INVISIBLE** at runtime: `src\prepare.py` must be run on the dev machine,
and the image rebuilt, before a deploy that depends on new data.

### The image is the deployable artifact

Built on the dev machine for `linux/amd64`, pushed to a registry, pulled by the VPS - nothing is
built on the VPS:

    docker build -f deploy/Dockerfile -t <registry>/geserver:<tag> .
    docker push <registry>/geserver:<tag>
    # on the VPS: pull, then `docker compose -f deploy/compose.yaml up -d` (the CMD is the manager)

### Ports on Docker Desktop (a local test only)

The zones bind 7010, 7011, ... INSIDE the container. On a real Linux host `network_mode: host` needs
no publishing. Docker Desktop on Windows does not give the container the host's loopback, so a local
test publishes a RANGE - and **not 7010-7015**, because the live fleet owns those on this host:

    -p 17010-17015:7010-7015     # dial a container zone on :701x as 127.0.0.1:1701x

### Warm maps in the container

With a reachable database the WARM SET comes from the `zone_daemon` plan, not from `-warm` (the
manager replaces the flag set with the plan's `warm` rows). The test above passed `-db ""` on purpose:
`-warm 10,15` is then authoritative, which is what "warm TWO maps only" needs. How the fleet's
`-max`/warm set is chosen is another card.

**The compose CMD is only the binary; the POLICY lives in `deploy/server.json`** (2026-11-11). The
manager reads the same file the zone does, section `"manager"`: the zone binary, base port, log dir,
`max`, `idle`, `linger`, `retain`, `memlimit`, `prepare`, `preload`, the warm set. Current values and
why: `max 60` (the default 8 counts WARM maps, so 18 warm cities would refuse the rest of the world -
`t_d127e535`), `idle 5m` for a map nobody has used, `linger 15m` for one that had a player (the 2 h
default was priced against a 2.6 s map start; a town is up in ~0.63 s now and a 429-body field in
~2.5 s, while an idle map costs ~95 MB measured PSS), and `memlimit` left at the manager's 200MiB,
which holds a busy zone at ~233 MB instead of ~329 MB. A flag still overrides the file; the container
passes none, so there is ONE place to edit and it is diffable.

**Tuning a running deployment without a rebuild.** The file is baked into the image, so an edit on
the VPS is normally a rebuild + push + pull. The compose file carries a COMMENTED mount
(`/srv/geserver/server.json:/app/out/server.json:ro`): copy that one small file to the host, uncomment
the mount, `docker compose restart geserver`, and the manager plus every zone child read the new values
at startup. It is commented out because Docker creates a DIRECTORY where a bind-mounted file is
missing, and the server would then try to read a directory as its config.

### One-start guard (fixed in this card)

The first container run of the manager exposed a boot race: `startWarm` and `run()`'s first
`reconcileWarm` both started map 10 - on :7010 and :7012 - and the registry held two presence files
for one map. `start()` now claims the map in `m.starting` under the same lock as `m.live`, and it
returns false when the child exits before announcing. `cmd\zoneman\start_test.go` pins both halves.

## Network note (important, and platform-specific)

`network_mode: host` is correct on a real Linux VPS: every zone binds its own port and announces the host's own
address to the client, with no NAT and no publishing of 40 ports. **Docker Desktop on Windows does NOT give the
container the Windows loopback**, so a local test must publish the port instead - a zone (`-p 7102:7102`) or,
for the manager, a range (`-p 17010-17015:7010-7015`, chosen off 7010+ because a live fleet owns those on this
host) - and the database must be reached as `host.docker.internal:5433`. That difference is a Docker Desktop
limit, not a server one - do not "fix" the compose file for it.

## Not in the image, on purpose

- **Python** (`python`/`python3`) and `src\prepare.py`: this is a DEPLOYMENT image (owner, 2026-11-10) - the
  derived data is built on the Windows dev box and baked at build time. See "Python: NOT in the image" above;
  a stale corpus is invisible at runtime, which is the price of the decision.
- The client tree (`ge\`: both dictionaries, `xml\datatable_*.xml`): a client input, not our data, and the repo
  tracks `docker-port\data` but not the client tree. Mount it read-only at `/ge` - no flag needed, the image's
  `server.json` points `-dict` at `/ge/dictionary/dictionary_local.xml` and both `../../ge` defaults (the server
  pack and `zone.XMLDir()`) land inside the same mount.
- `out\` (binaries, logs, load-test results): only `out\lua_corpus\` and the nine `out\*_image.bin` tables are
  inputs and are copied in.
