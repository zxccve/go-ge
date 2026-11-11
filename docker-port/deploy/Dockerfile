# syntax=docker/dockerfile:1
#
# The game server, containerised for Linux.
#
# WHY: the project target is the WHOLE stack in Docker - game server, database,
# patching, website - and the VPS is Linux. The one Windows binary in the tree is
# PatEngine (navmesh\pathtool.exe, 32-bit PE); it is proven byte-identical under
# Wine (2026-11-08, Debian 12 + Wine 8.0), so the image carries Wine instead of
# the host carrying a 32-bit userspace. Nothing in the Go tree changes: the zone
# runs the shipped exe through the launcher seam in zone\nav.go
# (-pathtool wine -pathtool-args /app/navmesh/pathtool.exe).
#
# BUILD (from docker-port, so data/ and navmesh/ are in the context):
#   docker build -f deploy/Dockerfile -t geserver:test .
#
# RUN a zone (host networking, scratch DB), client tree mounted at /ge. The image's
# CMD is the MANAGER - `/app/out/zoneman -bin /app/out/goserver -prepare=false` - and this
# is the single-zone smoke test instead:
#   docker run --rm --network host -e GE_PG_DSN=... -v /srv/ge:/ge:ro geserver:test \
#     /app/out/goserver -role zone -map 10 -addr :7102 \
#       -pathtool wine -pathtool-args /app/navmesh/pathtool.exe \
#       -meshdir /app/navmesh/mesh

# ---- build the Linux binaries from the same source the Windows one comes from ---
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY goserver/go.mod goserver/go.sum ./
RUN go mod download
COPY goserver/ ./
# Build BOTH binaries from one source: the zone server (`goserver`) and the world manager
# (`cmd/zoneman`). The manager is what a deployment runs - it hands out ports, warms maps,
# writes/reads the presence registry and stops idle zones - and it starts each zone as a
# CHILD process, passing the same binary path in -bin.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/goserver . \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/zoneman ./cmd/zoneman \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/mkaccount ./cmd/mkaccount

# ---- runtime: Debian 12 (the distro PatEngine was proven on) + Wine 32-bit -----
FROM debian:bookworm-slim

# THE IMAGE IS 32-BIT WINE AND NOTHING ELSE. pathtool.exe is a 32-bit console PE and
# the ONLY thing that ever runs under Wine, so the 64-bit half of the install is dead
# weight: libwine:amd64 is ~667 MB, almost all of it /usr/lib/x86_64-linux-gnu/wine/
# x86_64-windows (638 MB of 64-bit DLLs). `libwine:i386` is requested by name because
# a bare `libwine` makes apt pick the amd64 one; the x86_64 wine directory is removed
# anyway, in case a dependency drags it back.
#
# WINEARCH=win32 is fixed for the image: a pure 32-bit prefix, so wineboot never wants
# the 64-bit loader and the prefix holds one DLL set instead of two. WINEDEBUG=-all
# keeps the log clean; mscoree/mshtml are disabled because the helper is a plain
# console PE and the Wine stubs would only add noise.
ENV WINEPREFIX=/wine \
    WINEARCH=win32 \
    WINEDEBUG=-all \
    WINEDLLOVERRIDES=mscoree,mshtml= \
    DEBIAN_FRONTEND=noninteractive

# ONE RUN FOR THE WHOLE RUNTIME FILESYSTEM, ON PURPOSE: a delete in a LATER layer does
# not remove the bytes from the image (the earlier layer still carries them), so every
# cut below has to happen inside the same RUN that installs Wine.
#
# What is cut, and why a 32-bit console PE never needs it (each measured on the image
# that measured 3.2 GB - the before/after table is in deploy\README.md):
#   * /usr/lib/x86_64-linux-gnu/wine* - the 64-bit DLLs and host libs (~643 MB). Wine
#     8.0 on Debian runs a 32-bit prefix entirely from
#     /usr/lib/i386-linux-gnu/wine/i386-windows plus wineserver32; PROVEN by moving
#     both directories away, creating a FRESH prefix and running pathtool (faces 3067).
#   * /usr/share/doc, /usr/share/man, /usr/share/locale, /usr/share/iso-codes -
#     documentation, translated man pages, locale tables. A helper that prints
#     `faces 3067` on stdout reads none of them.
#   * libicu72, gstreamer and the video codecs (libaom/libx265/libSvtAv1/librav1e) that
#     libwine's dependency list drags in (~62 MB): nothing on this path decodes media or
#     does ICU collation. PROVEN by moving them out of the tree and running BOTH pathtool
#     and a fresh `wineboot -u` (both exit 0).
#   * THE PREFIX'S DUPLICATED DLL BYTES: wineboot COPIES every builtin into
#     drive_c/windows/system32 and winsxs (587 MB on the trail image; the copies are not
#     hardlinks - link count 1). The prefix must keep those names - deleting them gives
#     `could not load kernel32.dll, status c0000135` - so each copy is replaced by a
#     SYMLINK to the builtin it was copied from. That keeps the layout and the registry
#     (so the first zone start still pays nothing and the runtime still needs no writable
#     HOME) while the layer stores one copy of the bytes instead of two. PROVEN:
#     pathtool still prints `pathengine interface 2.39 release 4`, `faces 3067`,
#     `valid ... 0 0`, exit 0.
RUN set -eux; \
    dpkg --add-architecture i386; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        wine wine32:i386 libwine:i386 tini ca-certificates; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*.deb; \
    rm -rf /usr/lib/x86_64-linux-gnu/wine /usr/lib/wine/wine64; \
    rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/locale/* /usr/share/iso-codes; \
    for d in /usr/lib/x86_64-linux-gnu /usr/lib/i386-linux-gnu; do \
        find "$d" -maxdepth 1 \( -name 'libicu*' -o -name 'libgst*' -o -name 'gstreamer*' \
            -o -name 'libaom*' -o -name 'libx265*' -o -name 'libSvtAv1*' -o -name 'librav1e*' \) \
            -exec rm -rf {} +; \
    done; \
    mkdir -p /wine; \
    wineboot -u >/dev/null 2>&1 || true; \
    find /wine/drive_c -type f -exec sh -c ' \
        f="$1"; bn="${f##*/}"; \
        for cand in /usr/lib/i386-linux-gnu/wine/i386-windows /usr/share/wine/nls; do \
            if [ -e "$cand/$bn" ]; then rm -f "$f"; ln -s "$cand/$bn" "$f"; exit 0; fi; \
        done' _ {} \; ; \
    rm -rf /tmp/* /var/tmp/* /root/.cache /root/.wine*; \
    du -sh /wine /usr/lib/i386-linux-gnu/wine /usr

# THE WORKING DIRECTORY IS THE BINARY'S OWN FOLDER, exactly as on Windows: the
# Windows server runs FROM out\, and four of its inputs are found only that way,
# because their paths are relative to the PROCESS's cwd and are not in server.json:
#   ../out/text_image.bin                                   the shared dialog image
#   ../data/item_props.json                                 the status sheet's source
#   ../../ge/dictionary/dictionary_local_serverpack.xml     the script pack's dictionary
# With WORKDIR /app every one of them resolved one level too high - /data/...,
# /out/... - so files that ARE in the image (and in the client mount) read as
# `no such file`.
#
# THE THREE LEVELS ARE THE REPO'S: /app is docker-port (data, config, navmesh, out)
# and / is the repo root, where the client tree sits exactly as D:\ge_server\ge
# sits beside D:\ge_server\docker-port. `../..` from /app/out is therefore `/`, and
# that is why the client mount below is /ge and not /app/ge.
WORKDIR /app/out
COPY --from=build /out/goserver /app/out/goserver
COPY --from=build /out/zoneman /app/out/zoneman
COPY --from=build /out/mkaccount /app/out/mkaccount
COPY data/ /app/data/
COPY config/ /app/config/
COPY navmesh/ /app/navmesh/
# The compiled script corpus: the zone indexes it at start (41,573 names) and loads
# each script on first use. Without it the server runs but every scripted feature is
# dead (monster AI, skill damage, monster HP/DEF) - see the path bug in deploy\README.md.
COPY out/lua_corpus/ /app/out/lua_corpus/
# THE GENERATED TABLE IMAGES (41 MB) ARE INPUTS, NOT BUILD OUTPUT. Each is a parsed copy
# of a table the zone would otherwise re-parse on every start; without the file the code
# falls back to the XML and REWRITES the image, so a missing one is a slow start, not a
# wrong one - except text_image.bin, whose absence is the `no such file` in the first
# container run. They are excluded from the build context by out/*.bin in .dockerignore
# and re-included one by one. NINE today: text, buff, item, monster, skill, mapdrops,
# penalty, stance and sight (the sight ring's map_table, card t_38b6216f).
COPY out/text_image.bin out/buff_image.bin out/item_image.bin \
     out/monster_image.bin out/skill_image.bin out/mapdrops_image.bin \
     out/penalty_image.bin out/stance_image.bin out/sight_image.bin /app/out/
# The client tree (dictionary_local.xml 20 MB + dictionary_local_serverpack.xml
# 17 MB + xml\datatable_*.xml) is NOT baked in: it is a client input, not our data,
# and the repo tracks docker-port\data but not the client tree. The mount is
# REQUIRED - the zone reads all three from it - and it goes on /ge, because the
# defaults are written for the repo layout (`../../ge` from out):
#   ../../ge/dictionary/dictionary_local_serverpack.xml   -> the script pack
#   ../../ge/xml/datatable_*.xml                          -> zone.XMLDir()
#   server.json's "dictionary"                            -> -dict
#   -v /srv/ge:/ge:ro
COPY deploy/server.json /app/out/server.json

# The zone writes logs and presence files here; a compose volume overrides it.
RUN mkdir -p /app/out/zones /app/out/logs

# THE MANAGER IS THE CONTAINER'S DEFAULT PROCESS: it is the world (ports, warm maps, presence,
# idle stops) and it starts each zone as a child. `-logdir /app/out/logs` is passed because the
# manager's own default (`../logs`) is docker-port\logs on Windows and does not exist here.
#
# `-prepare=false` IS THE CONTAINER DEFAULT, AND IT IS A DEPLOYMENT DECISION (owner, 2026-11-10):
# this image is DEPLOYMENT ONLY - everything is built and developed on the Windows dev box, and
# the VPS pulls and runs. So PYTHON IS NOT IN THIS IMAGE and `src\prepare.py` is never run here;
# the corpus, item_props/item_stats and the table images were baked at BUILD time on the dev
# machine. The cost is that a stale corpus is INVISIBLE at runtime - run `src\prepare.py` and
# rebuild the image before a deploy that depends on new data (deploy\README.md).
# `-db` is left at its default (GE_PG_DSN): with a reachable database the WARM SET comes from
# the zone_daemon plan, which is the deployment's data, not a flag.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/app/out/zoneman", "-bin", "/app/out/goserver", "-base-port", "7010", "-logdir", "/app/out/logs", "-prepare=false"]
