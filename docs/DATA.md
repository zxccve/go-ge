# Data - what the server reads, and what you must bring

The server does not contain the game's numbers. It **reads the game's own tables**, decoded from
a client you own, and runs the game's own scripts. This page explains that pipeline, because it
is the one part of a fresh install that cannot be downloaded.

## The chain

```
your client's archives (.ipf)
        |  extract / decode
        v
 .ies tables + dictionary  (the game's own data, per client build)
        |  decode to XML, then the emitters
        v
docker-port\data\*.json   (what the server loads at start-up)
        |
        v
the zone and the barrack read them; src\prepare.py knows which are stale
```

Two consequences worth understanding before you touch it:

- **A table from a different client build is a different table.** This project's anchor is the
  Andromida client, revision 136. The old Terra client (revision 318) is a different generation,
  and its tables produced names the client could not resolve. Never mix them.
- **A stale derived file is silently wrong.** It answers every question with old numbers and
  nothing complains. That is why `src\prepare.py` exists: it declares which outputs are derived
  and compares their timestamps with their sources.

## The one command

```bash
cd docker-port/src
python prepare.py --check     # FRESH / STALE per target, writes nothing, exit 1 if stale
python prepare.py             # refresh only what is stale
```

`doctor.py` calls the same check for its one-line summary.

## The emitters

`docker-port\src\emit_*.py` - one per output family, 11 of them:

| Tool | Produces |
|---|---|
| `emit_item_props.py`, `emit_item_stats.py`, `emit_item_equip.py`, `emit_item_catalog.py` | the item tables the zone loads |
| `emit_shop_data.py` | `custom_shops.json`, `shop_groups.json`, prices |
| `emit_zone_points.py` | `zone_points.json` - spawns, town points, and the team formation |
| `emit_zone_nav.py` | `zone_nav\` - the per-map navmesh data |
| `emit_zone_npcs.py` | `zone_npcs\` - town NPC placement per map |
| `emit_barrack_oracle.py`, `emit_cipher_vectors.py`, `emit_go_bf_tables.py` | test fixtures: the barrack oracle, the cipher vectors, the Blowfish tables |

Each emitter's docstring says what it reads and what it writes. Run it from `src\`.

## What is in `data\` (after you produce it)

Four groups, and the difference matters when something looks wrong:

1. **Loaded by the running server** - `map_table.xml`, `zone_points.json`, the `item_*.json`
   family, `job_stats.json`, `warp.xml`, `locale_id_map.json`, `zone_npcs\`, `zone_nav\`, and the
   `cmd_table` family.
2. **Read only by the Python tools** - the text-id bridge, the opcode dumps, `inputs.json`.
3. **Test fixtures** - recorded packet streams the tests replay.
4. **Superseded one-off outputs** - safe to delete; nothing reads them.

## What a clone can and cannot regenerate

| Kind | Regenerable from a client you own? |
|---|---|
| the item / zone / shop / NPC JSON | yes - the emitters |
| the Lua corpus the script engine runs | yes - `src\scp_to_lua.py --dump ..\out\lua_corpus` |
| the navmesh meshes | yes on Windows (the mesh converter), the one heavy step |
| `src\ctables.pkl`, `src\bf_tables.pkl` | **not published**: client-derived pickles. The public copy drops every `src\*.pkl`; a reader regenerates the cipher vectors with `emit_cipher_vectors.py` and dumps the packet table with `dump_cmds.py` against a running client |
| the client itself | no - you bring it |

That last pair is the honest boundary: the *code* is complete, the *game's data* is yours to
produce, and one hand-lifted artefact cannot be shared at all.
