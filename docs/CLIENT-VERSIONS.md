# Client versions - this tree is pinned to one client

**This project is not version agnostic.** It was built against one client build, and every
packet id, declared size and record layout in the code was measured against that build. If you
develop on a different client, adapting it is your job - this page says exactly what to change.

## What it is pinned to

- **Client:** the Andromida / AndroReborn install - the one the project calls the anchor.
- **File version:** `321819` - the generation this project calls **v32**.
- **Client revision:** 136.

The other install this project has on disk (Terra) reports the same file version but is a
different client with a different data generation. **Its tables are not interchangeable**, and
mixing them is how a client ends up showing a bare number where a name should be.

## What is version-locked (you will have to change these)

| Locked to the client | Where |
|---|---|
| the command id of every packet, and the size the client allocates for it | the whole `protocol\` package, and the ids named in `zone\` and `barrack\` |
| record strides and field offsets inside a packet | `protocol\`, `zone\attack*.go`, the spawn/roster writers |
| a few behaviours that are opcode-specific (a packet that changed meaning between builds) | wherever the id is handled |
| text / locale ids and the dictionary | `locale_id_map.json` and the data pipeline |
| the `.ies` table layout | the tools in `src\` |

## What is not version-locked

The structure: three programs, one zone per map, the manager, the PostgreSQL schema, the data
pipeline, and the rule engine itself - because the game rules come from the **client's own**
data tables and scripts, not from us.

## Adapting to your client - five steps

1. **Dump your client's packet table.** With your client running:
   `python src\dump_cmds.py 0 4000 --out ..\data\cmd_table_live.txt`
2. **Cross-check every id the code hardcodes.** This is the step that matters:
   `python src\check_cmd_ids.py <your table>`. It prints the name *your* client has for each id
   the code uses. A mismatch here does not crash - it silently does nothing at runtime. One
   measured pair had id `3680` mean `ZC_DIALOG_NPC_TRADE` in one build and `CZ_START_GAME` in the
   other: same number, different command.
3. **Fix what moved.** Ids and sizes in `protocol\`; record layouts in the handlers that write
   them; then re-record the test fixtures the tests replay (`goserver\*\testdata`).
4. **Regenerate the data** with `python src\prepare.py` against your client's files.
5. **Run the gate** (`go build ./... && go vet ./... && go test ./... -count=1`) and one real
   login before you trust anything.

## Two rules that save a fork

- **Never mix two builds' tables.** One table, one client, named in your notes.
- **Re-run step 2 after every client update.** Ids move quietly between builds; a green test suite
  proves the code is consistent with itself, not with your client.
