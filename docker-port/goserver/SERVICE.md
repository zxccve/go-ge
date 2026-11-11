# SERVICE.md - the two service recipes, as DOC

Card: `goserver: one server.json, two platforms` (2026-11-09). Code lives in
`main.go` + package `config`; this file is the operational half: how a barrack
and a zone are started by the OS instead of by hand.

The startup ORDER is the same on both platforms, and it is a dependency chain,
not a suggestion:

    DATABASE  ->  BARRACK (:7001)  ->  ZONE (:7002)

The barrack reads the address book and every character out of the database; the
zone reads its map's NPC data and announces itself in the registry the barrack
reads. A zone that starts first still serves, but the barrack cannot hand a map
to it until it appears, and a database that is not up turns the barrack's login
into an error. Each recipe below makes that chain explicit.

## 0. Build and lay out the files

    cd docker-port/goserver
    sh build.sh          # ../out/goserver_win_amd64.exe and ../out/goserver_linux_amd64

Deploy these side by side (the folder is the service's working directory):

    goserver_linux_amd64      (or goserver_win_amd64.exe on Windows)
    server.json               # barrack.json and zone.json below, or copies

`server.json` in the working directory is read automatically; `-config <file>`
names another one. A file NAMED by `-config` that is missing is a fatal error
naming the path; with no `-config` and no `server.json`, the built-in defaults
are used (the pre-config command line, unchanged).

`join the repo tree`: the data, `navmesh/` and `out/zones` live ABOVE
`goserver/`, so a deployed folder either reproduces that shape or the paths in
server.json are made absolute. The recipes use `/opt/geserver` (Linux) and
`D:/ge_server/docker-port/out` (Windows).

## 1. The ONE line that differs: the path engine

Windows runs the shipped helper directly:

    "engine": { "command": "../navmesh/pathtool.exe" }

Linux has no native pathtool, so it runs the same exe under wine. The command is
the launcher and the FIRST arg is the helper file (the thing whose existence the
zone checks); the mesh is appended last:

    "engine": { "command": "wine", "args": ["./pathtool.exe"] }

`WINEPREFIX` for that wine is the trap: use a PERSISTENT path, never `/tmp`.
`/tmp` is recreated at boot, so wine re-initialises its prefix (and loses the
32-bit prefix state the helper needs) on every restart.

## 2. Linux - systemd

Three units. `geserver-db` is the DB gate; if PostgreSQL is native, use
`After=postgresql.service` / `Requires=postgresql.service` in the other two and
drop the first unit.

/etc/systemd/system/geserver-db.service

    [Unit]
    Description=Granado Espada database (PostgreSQL container)
    After=network-online.target docker.service
    Wants=network-online.target docker.service

    [Service]
    Type=oneshot
    RemainAfterExit=yes
    ExecStart=/usr/bin/docker start granado_pg
    ExecStop=/usr/bin/docker stop granado_pg

    [Install]
    WantedBy=multi-user.target

/etc/systemd/system/geserver-barrack.service

    [Unit]
    Description=Granado Espada barrack (:7001)
    Requires=geserver-db.service
    After=network-online.target geserver-db.service
    Wants=network-online.target

    [Service]
    Type=simple
    User=geserver
    Group=geserver
    WorkingDirectory=/opt/geserver
    Environment=GE_PG_DSN=postgres://granado:PASSWORD@localhost:5433/ge_data?sslmode=disable
    Environment=WINEPREFIX=/var/lib/geserver/wine
    Environment=WINEDEBUG=-all
    ExecStart=/opt/geserver/goserver_linux_amd64 -config /etc/geserver/barrack.json
    Restart=on-failure
    RestartSec=2

    [Install]
    WantedBy=multi-user.target

/etc/systemd/system/geserver-zone.service

    [Unit]
    Description=Granado Espada zone (:7002)
    Requires=geserver-db.service
    After=network-online.target geserver-db.service geserver-barrack.service
    Wants=network-online.target

    [Service]
    Type=simple
    User=geserver
    Group=geserver
    WorkingDirectory=/opt/geserver
    Environment=GE_PG_DSN=postgres://granado:PASSWORD@localhost:5433/ge_data?sslmode=disable
    Environment=WINEPREFIX=/var/lib/geserver/wine
    Environment=WINEDEBUG=-all
    ExecStart=/opt/geserver/goserver_linux_amd64 -config /etc/geserver/zone.json
    Restart=on-failure
    RestartSec=2

    [Install]
    WantedBy=multi-user.target

The `After=` line is the order: db -> barrack -> zone. `Requires=` makes a
failed earlier stage stop the later one instead of leaving a half stack.

    sudo useradd --system --home /var/lib/geserver geserver
    sudo install -d -o geserver -g geserver /var/lib/geserver/wine
    sudo systemctl daemon-reload
    sudo systemctl enable --now geserver-db geserver-barrack geserver-zone

`/var/lib/geserver/wine` is the persistent WINEPREFIX. Wine creates it on the
first zone start; back it up with the rest of the deployment and never point it
at `/tmp`.

## 3. Windows - the same three stages

`sc.exe` is the built-in equivalent, but it starts the process in
`C:\Windows\System32`, so every relative path in server.json breaks. Either use
ABSOLUTE paths in the Windows JSON, or use NSSM (the practical equivalent,
because it sets the working directory):

    nssm install geserver-barrack "D:\ge_server\docker-port\out\goserver_win_amd64.exe"
    nssm set geserver-barrack AppParameters "-config D:/ge_server/docker-port/out/barrack.json"
    nssm set geserver-barrack AppDirectory "D:/ge_server/docker-port/out"
    nssm set geserver-barrack DependOnService postgresql-x64-16
    nssm set geserver-barrack Start SERVICE_AUTO_START

    nssm install geserver-zone "D:\ge_server\docker-port\out\goserver_win_amd64.exe"
    nssm set geserver-zone AppParameters "-config D:/ge_server/docker-port/out/zone.json"
    nssm set geserver-zone AppDirectory "D:/ge_server/docker-port/out"
    nssm set geserver-zone DependOnService geserver-barrack
    nssm set geserver-zone Start SERVICE_AUTO_START

    nssm start geserver-barrack
    nssm start geserver-zone

The `DependOnService` lines ARE the startup order (database service -> barrack
-> zone). With `sc.exe` alone, the same chain:

    sc.exe create geserver-barrack binPath= "\"D:\ge_server\docker-port\out\goserver_win_amd64.exe\" -config D:\ge_server\docker-port\out\barrack.json" start= auto
    sc.exe config geserver-barrack depend= postgresql-x64-16
    sc.exe create geserver-zone binPath= "\"D:\ge_server\docker-port\out\goserver_win_amd64.exe\" -config D:\ge_server\docker-port\out\zone.json" start= auto
    sc.exe config geserver-zone depend= geserver-barrack
    sc.exe start geserver-barrack && sc.exe start geserver-zone

## 4. The two config files (copy server.json and change only these)

barrack.json

    "mode": { "role": "barrack" }
    "listen": { "barrack_addr": ":7001" }

zone.json (map 1331, the owner's test town)

    "mode": { "role": "zone", "map": 1331 }
    "listen": { "barrack_addr": ":7002" }
    "engine": { "command": "wine", "args": ["./pathtool.exe"] }   # Linux only

`listen.barrack_addr` is the port a role binds; it is called barrack_addr only
because the flag was `-addr` for both roles.

## 5. Check it is the right file

Every start prints ONE line before it accepts a connection:

    config: read /etc/geserver/zone.json (role zone, map 1331, engine "wine ./pathtool.exe")

A zone showing `role barrack` or the wrong map is the wrong `-config`, and the
line says so before the first player connects. Under systemd:
`journalctl -u geserver-zone -n 20`. Under Windows: the log named by
`paths.log` (set it; an unset log means console only, which a service does not
show).
