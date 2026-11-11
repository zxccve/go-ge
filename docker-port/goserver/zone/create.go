package zone

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

// ACTORS A SCRIPT MAKES WHILE THE SERVER RUNS.
//
// This is the other half of `script\create.go`: `CreateTNPC` and `CreateTmonster`
// parse the game's own argument lists, this file turns them into something the
// client draws. It is the Scarecrow lane - the four standing dummies the
// "Scarecrow Creator" trigger makes, and the hittable Scarecrow a player makes by
// configuring one.
//
// WHAT THE ORIGINAL DOES, for each (both citations are the shipped server):
//
//	CreateTNPC     SystemFunc.cpp:2112-2127
//	  shape = geShapeTable::Get(npcProperty->size)
//	  Get3DPointToPosLocaly(shape, {x,y,z}, newPos, 1000, 300)     <- the MESH answers the cell
//	  world->CreateTriggerObject(npcName, npcTypeName, newPos, direction, ...)
//	CreateTmonster SystemFunc.cpp:1044-1051 -> World.cpp:797-856
//	  Get3DPointToPosLocaly(shape, {x,y,z}, newPos, 1000, 300)     <- the same call
//	  pIesObj->Set(Lv/MHPFix/Tactics) when the script passed them
//	  CreateMonsterNPC(pIesObj, newPos, direction)  -> 3527 to the zone
//
// SO THE SIXTH ARGUMENT IS A HEIGHT, NOT A CELL, and every call is resolved
// through the map's mesh before anything is sent. Measured on fld_auch with the
// game's own path engine, for the Scarecrow's own numbers:
//
//	valid 2240 -590 7            -> refused   (7 is not the triangle at that spot)
//	near  2240 -590 7 1000 300   -> cell 108  (7 IS a height hint there)
//
// A builder that trusted "cell 7" would draw NOTHING on screen and log nothing -
// the exact failure this project keeps paying for. `Handler.NavNear` is that
// `Get3DPointToPosLocaly` (zone\nav.go `Near`, `near x y z 1000 300`), and it is
// the same call the Last Point uses, with the same two ranges.
//
// WHAT THIS FILE DOES NOT DO: no AI, no attack, no damage, no drops, no exp. A
// created monster stands still and can be looked at. It is stored with its class,
// its HP and its tactics so the damage slice has something to hit, and nothing
// more.

// TNPCSpec is one parsed `CreateTNPC` call. It mirrors script.TNPCSpawn on
// purpose: the zone does not import the script package (main bridges the two), and
// the same split already exists for ClickObject -> script.Obj.
type TNPCSpec struct {
	Name       string  // npcName: the object's name; its click entry is SCR_CLICK_<Name>
	TypeName   string  // npcTypeName: an NPC table ClassName, e.g. 'npc_tbk_contbox_off'
	X, Y, Z    float64 // Z is a HEIGHT hint - the mesh turns it into a cell
	Direction  float64 // compass degrees (270 = west)
	Occupation string  // stored as a property in the original; carried for the log
	Possession int
}

// MonsterSpec is one parsed `CreateTmonster` call.
type MonsterSpec struct {
	MonsterID int32   // the Monster table's ClassID, e.g. 11600
	X, Y, Z   float64 // Z is a HEIGHT hint
	Count     int     // how many the call asked for; this lane makes ONE and says so
	Direction float64
	Level     float64 // the script's `lv` override (0 = the row's own)
	MHP       float64 // the script's `mhpFix` override (0 = the row's own)
	Tactics   string  // the script's tactics override; EMPTY = the row's own `Tactics`

	// Respawn is the interval this body's corpse comes back on, when the caller KNOWS
	// one: the map spawner's type row `RespawnTime` (zone/spawner.go). `HasSpawner`
	// says whether such a row exists at all - a probe or a script's `CreateTmonster`
	// passes false, and its body DOES NOT COME BACK (zone/respawn.go respawnDelayFor).
	// The pair exists so a row that states `RespawnTime="0"` (returns at once) cannot
	// be read as the absence of a row.
	Respawn    time.Duration
	HasSpawner bool

	// SpawnerClassID is the `mongentype` row that placed this body, for the log
	// only: a death line can then say which spawner's clock the return is on. 0
	// for a body no spawner placed.
	SpawnerClassID uint32
}

// The two mesh ranges of `Get3DPointToPosLocaly`. They are the original's own
// numbers, the ones the Last Point uses on the way in (Commander_Load.cpp:580),
// and they are NOT tuning knobs: a wider range "finds" ground across a wall.
const (
	spawnHRange = 1000
	spawnVRange = 300
)

// HANDLE BLOCKS. A handle is how the client names an actor, and two actors with
// one handle is one actor: the second is never drawn, or the first is moved. The
// blocks this server uses, and why the two new ones are where they are:
//
//	players      the character's own database row id, handed over in CZ_START_GAME
//	             (`store.go`: Handle: r.ID) - today four digits (7, 38, 40, 1792)
//	town NPCs    0x40000000 + index (`src\emit_zone_npcs.py`, NPC_HANDLE_BASE)
//	warp points  0x7F000000 + key (`gate.go`)
//	monsters     0x20000000 + a process-seeded, rising counter (`monster.go`,
//	             MonsterHandleBase, nextMonsterHandleLocked)
//	runtime NPCs 0x40000000 + the SAME index the town NPCs count with - they are
//	             the same kind of object to the client (a 3543 and a click), so
//	             they share the block and simply continue its numbering:
//	             base + len(h.NPCs) at the moment of creation. The town list only
//	             ever grows, so no number can be handed out twice.
//
// 0x40000000 is what the game itself uses for NPCs and 0x20000000 is below it, so
// neither block can reach the other and neither reaches the sign bit. A player
// handle (a database row id) would have to pass 536 million before it met the
// monster block - the table's own column cannot hold that.

// runtimeNPCs returns the NPCs a script created, newest last. It is a COPY: the
// caller ranges it while another connection may be creating more.
func (h *Handler) runtimeNPCs() []NPC {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if len(h.spawnedNPC) == 0 {
		return nil
	}
	out := make([]NPC, len(h.spawnedNPC))
	copy(out, h.spawnedNPC)
	return out
}

// runtimeMonsters returns the monsters a script created, in the order they were
// made. A COPY, for the same reason as runtimeNPCs.
func (h *Handler) runtimeMonsters() []Monster {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if len(h.spawnedMonster) == 0 {
		return nil
	}
	out := make([]Monster, len(h.spawnedMonster))
	copy(out, h.spawnedMonster)
	return out
}

// meshCell turns a script's `(x, y, z)` into the WORLDPOS the client needs, or
// explains why it cannot. Z is a HEIGHT hint - see the file comment.
//
// ok=false is a REFUSAL, never a fallback: a guess at (0, 0, 0) is an actor
// standing in the middle of the map, and it looks exactly like a working spawn
// until somebody walks there.
func (h *Handler) meshCell(x, y, z float64) (pos [3]int32, height int32, err error) {
	// ONE ANSWER PER POINT. A map's spawner puts its quota of bodies on a SMALL set of points
	// (zone/spawner.go cycles one cursor per division), so 429 monsters on Katovic asked the
	// path engine the same dozen questions - the spawn took 29.6 s for that reason alone
	// (measured 2026-11-10). The mesh does not move, so the answer is cached, errors included:
	// a point off the mesh is off the mesh for every body that lands on it.
	key := cellKey{int32(math.Round(x)), int32(math.Round(y)), int32(math.Round(z))}
	if hit, ok := h.cachedCell(key); ok {
		return hit.pos, hit.height, hit.err
	}
	if h.NavNear == nil {
		return pos, 0, fmt.Errorf("this zone has no path engine wired, so the map's "+
			"mesh cannot turn (%g, %g, %g) into a navmesh cell - a cell this side "+
			"invents draws nothing", x, y, z)
	}
	pt := [3]int32{key.x, key.y, key.z}
	// COUNTED, so the spawner can say what the mesh cost it (the 429-monster map took 29.6 s and
	// three attributions were wrong before one was measured).
	startedAt := time.Now()
	near, ground, ok := h.NavNear(pt, spawnHRange, spawnVRange)
	meshQueries.Add(1)
	meshNanos.Add(int64(time.Since(startedAt)))
	if !ok {
		err := fmt.Errorf("the map's mesh has no ground within %d units of (%d, %d, %d): "+
			"that is a spot off the walkable mesh, and an actor put there is drawn nowhere at all",
			spawnHRange, pt[0], pt[1], pt[2])
		h.storeCell(key, cellHit{err: err})
		return pos, 0, err
	}
	if !PlausibleCell(near[2]) {
		err := fmt.Errorf("%w", badCellError(near[2]))
		h.storeCell(key, cellHit{err: err})
		return pos, 0, err
	}
	h.storeCell(key, cellHit{pos: near, height: ground})
	return near, ground, nil
}

// meshQueries and meshNanos count what the path engine actually cost: every cache MISS adds one
// query and its duration, and the spawner prints the totals.
var (
	meshQueries atomic.Int64
	meshNanos   atomic.Int64
	hpCalls     atomic.Int64
	hpNanos     atomic.Int64
	defCalls    atomic.Int64
	defNanos    atomic.Int64
)

// ScriptCost answers how many per-body script calls were made and how long they took, then resets.
func ScriptCost() (hpN int64, hpD time.Duration, defN int64, defD time.Duration) {
	return hpCalls.Swap(0), time.Duration(hpNanos.Swap(0)),
		defCalls.Swap(0), time.Duration(defNanos.Swap(0))
}

// MeshCost answers how many engine queries have been made and how long they took, and resets the
// counters - the spawner calls it once at the end of its run.
func MeshCost() (int64, time.Duration) {
	n := meshQueries.Swap(0)
	d := meshNanos.Swap(0)
	return n, time.Duration(d)
}

// cachedCell answers from the mesh cache, if this point has been asked about already.
func (h *Handler) cachedCell(key cellKey) (cellHit, bool) {
	h.cellMu.Lock()
	hit, ok := h.cellCache[key]
	h.cellMu.Unlock()
	return hit, ok
}

// storeCell remembers one answer. The cache is per zone and bounded: a map has a few thousand
// walkable points at most, and a map that somehow asks more starts clean rather than growing
// without limit.
func (h *Handler) storeCell(key cellKey, hit cellHit) {
	h.cellMu.Lock()
	if h.cellCache == nil || len(h.cellCache) >= 20000 {
		h.cellCache = make(map[cellKey]cellHit, 256)
	}
	h.cellCache[key] = hit
	h.cellMu.Unlock()
}

// SpawnTNPCObject adds ONE static NPC to the zone at runtime and tells every
// player about it with the packet a town NPC already uses.
//
// The client gets `ZC_SPAWN_STANDINGTRIGGER` (3543) from `BuildSpawnTrigger` -
// REUSED, not copied: a runtime NPC and a town NPC are the same object to the
// client, and two builders would drift the moment one of them is fixed. Clicks
// work through the same path a town NPC uses, because the object is answered by
// `npcByHandle`/`openNPCDialog`; its click entry is `SCR_CLICK_<Name>`, exactly
// as the original derives it (`TriggerObject.cpp:328`), and its `NPCType` is the
// NPC table's ClassID for `TypeName` - the number that draws the body.
//
// A refusal returns an error that NAMES the argument or the table row it could not
// resolve, and changes NOTHING: no handle is spent, no packet is sent.
func (h *Handler) SpawnTNPCObject(spec TNPCSpec) (uint32, error) {
	if spec.Name == "" {
		return 0, fmt.Errorf("CreateTNPC: npcName is empty, so the object has no name " +
			"and no SCR_CLICK_ entry")
	}
	if h.NPCTypes == nil {
		return 0, fmt.Errorf("CreateTNPC(%q): no NPC table is loaded, so the type %q "+
			"cannot be resolved to the ClassID that draws its body - start the zone "+
			"with -xml pointing at the client's own xml folder", spec.Name, spec.TypeName)
	}
	classID, ok := h.NPCTypes.ClassID(spec.TypeName)
	if !ok {
		return 0, fmt.Errorf("CreateTNPC(%q): npcTypeName %q is not a row of the NPC "+
			"table (datatable_npc.xml) - an unknown type cannot be drawn",
			spec.Name, spec.TypeName)
	}
	pos, height, err := h.meshCell(spec.X, spec.Y, spec.Z)
	if err != nil {
		return 0, fmt.Errorf("CreateTNPC(%q): %w", spec.Name, err)
	}

	h.spawnMu.Lock()
	// THE HANDLE: the town NPCs' own block, numbered on from them. len(h.NPCs) is
	// read under this mutex and h.NPCs is never appended to after startup, so the
	// count cannot change under us.
	index := len(h.NPCs) + len(h.spawnedNPC)
	npc := NPC{
		Handle: NPC_HANDLE_BASE + uint32(index),
		X:      pos[0],
		Y:      pos[1],
		Cell:   pos[2],
		// The direction travels as the BYTE angle the 3543 carries, the same
		// narrowing the original does (`GetByteAngle`, TriggerObject.cpp:381).
		Direction: byteAngle(spec.Direction),
		NPCType:   classID,
		// GenID stays 0: there is no NPCGen row behind a runtime object, and the
		// caption is resolved from the row's own Name for a town NPC. A created NPC
		// therefore has no caption - see the log line, which says so.
		NameToken: spec.Occupation,
		ClassType: spec.TypeName,
		Script:    spec.Name,
	}
	h.spawnedNPC = append(h.spawnedNPC, npc)
	h.spawnMu.Unlock()

	pkt := BuildSpawnTrigger(npc)
	sent := h.broadcastWorldAt(pkt, npc.X, npc.Y, 0, 0)
	h.Log.Printf("zone: CREATE_TNPC %q (type %q, class %d) at (%d, %d, cell %d), "+
		"ground height %d from the mesh, dir %d -> handle 0x%08x, cmd %d (%d B) to "+
		"%d player(s); no caption: a runtime NPC has no NPCGen row for the client "+
		"to name it from",
		spec.Name, spec.TypeName, classID, npc.X, npc.Y, npc.Cell, height,
		npc.Direction, npc.Handle, binary.LittleEndian.Uint16(pkt), len(pkt), sent)
	return npc.Handle, nil
}

// SpawnMonsterObject adds ONE monster to the zone at runtime and draws it with
// `ZC_ENTER_STANDINGMONSTER` (3527, 360 bytes).
//
// Adoption note: the builder is `monster.go`'s, which came from the cancelled S1
// lane and is held byte for byte against a live 3527
// (`zone\testdata\live_enter_monster.bin`). This function only supplies the
// numbers; it does not touch a byte of that layout.
//
// The monster is STORED as well as sent, so a player who enters afterwards is
// drawn the same world (see Handler.runtimeMonsters and its use in the GAME_READY
// branch). It is not a database row and it does not survive a restart - the script
// that made it runs again on the next start, which is where a world's actors
// belong.
func (h *Handler) SpawnMonsterObject(spec MonsterSpec) (uint32, error) {
	if h.Monsters == nil {
		return 0, fmt.Errorf("CreateTmonster(id %d): no monster table is loaded, so "+
			"the id cannot be checked against the game's own rows - start the zone "+
			"with -xml pointing at the client's own xml folder", spec.MonsterID)
	}
	row, ok := h.Monsters.Row(uint32(spec.MonsterID))
	if !ok {
		return 0, fmt.Errorf("CreateTmonster: id %d is not a row of the Monster table "+
			"(datatable_monster*.xml) - an unknown id has no body, no level and no "+
			"tactics, so nothing can be drawn", spec.MonsterID)
	}
	pos, height, err := h.meshCell(spec.X, spec.Y, spec.Z)
	if err != nil {
		return 0, fmt.Errorf("CreateTmonster(id %d, %s): %w", spec.MonsterID, row.Name, err)
	}

	if spec.Count > 1 {
		// The original loops and puts every one of them on the SAME point
		// (SystemFunc.cpp:1046-1051 reuses `point`), so a count above one is a
		// pile nobody can tell apart. ONE is made, and the difference is said out
		// loud rather than hidden.
		h.Log.Printf("zone: CREATE_TMONSTER id %d asked for %d monsters - this lane "+
			"makes ONE and says so (the original stacks them all on the same point)",
			spec.MonsterID, spec.Count)
	}

	level := int(row.Level)
	if spec.Level > 0 {
		level = int(spec.Level)
	}
	hp := int64(row.MHP)
	if spec.MHP > 0 {
		hp = int64(spec.MHP)
	}
	// The row a monster's ATK is computed from: the spawned level, not the table's.
	atkRow := row
	atkRow.Level = level

	// THE MAX HP. AN EXPLICIT spec.MHP WINS: it is the script's own `mhpFix` (the
	// Scarecrow's player HP x 100, or a probe's number), and the body must keep the
	// number the script asked for.
	//
	// WITH NONE, THE GAME'S OWN `SCR_Get_MON_MHP` ANSWERS IT (zone\monster_hp.go).
	// The Monster table has NO `MHP` column at all - it carries `HP`, `RHP`, `MHPFix`,
	// `MHP_BM`, `CP_MON_MHP` and 248 more, but never `MHP` - so `row.MHP` is 0 for
	// all 7,725 rows and the row alone can never answer. This is the same shape the
	// DEF path already has: the SCRIPT owns the formula, the row supplies its inputs.
	//
	// THE SCRIPT RUNS OUTSIDE spawnMu. It is a Lua call on the shared host, and the
	// mutex guards the world lists, not one body's arithmetic.
	candidate := Monster{
		ClassID: uint32(spec.MonsterID), Level: level,
		RaceType: row.RaceType, Arm: row.Arm, DEFBM: row.DEFBM,
		DEFPERBM: row.DEFPERBM, IMPBM: row.IMPBM, SizeType: row.SizeType,
		MobRank: row.MobRank, DRBM: row.DRBM, DefLayer: row.DefLayer,
		DefLayerBM: row.DefLayerBM, RSTAT: row.RSTAT, RSTATBM: row.RSTATBM,
		CON: row.CON, MHPFix: row.MHPFix, MHPBM: row.MHPBM, ADDMHPBM: row.ADDMHPBM,
	}
	if hp <= 0 {
		hpAt := time.Now()
		scriptHP, ok, herr := h.monsterMaxHP(candidate)
		hpNanos.Add(int64(time.Since(hpAt)))
		hpCalls.Add(1)
		if ok && scriptHP > 0 {
			hp = int64(scriptHP)
		} else {
			if herr == nil {
				herr = fmt.Errorf("%s answered %g", MonsterHPEntry, scriptHP)
			}
			h.logMaxHPUnavailable(uint32(spec.MonsterID), row.Name, herr)
		}
	}

	// THE DEFENDER'S THREE COMPUTED PROPERTIES (zone\monster_def.go, MonsterDefScript
	// .Ranks): the game's own `SCR_Get_MON_DR` / `SCR_Get_MON_DefLayer` /
	// `SCR_Get_MON_RSTAT`, run ONCE here with the body's spawned level and carried -
	// the same cache the reference keeps on the body (MonsterNPC.h:143). A skill's
	// buff rule reads them off `self` (common_pc.scp:6752-6760), so they must be the
	// script's answers, never an arithmetic copy of the columns.
	//
	// A RUN THAT FAILS IS NAMED, NOT DEFAULTED: the three stay 0 and the line says the
	// rule will read 0 - a number nobody measured is a GAP, not a rank.
	ranks := map[string]float64{}
	if h.Defence != nil {
		var ranksErr error
		var ranksOK bool
		defAt := time.Now()
		ranks, ranksOK, ranksErr = h.Defence.Ranks(candidate)
		defNanos.Add(int64(time.Since(defAt)))
		defCalls.Add(1)
		if !ranksOK {
			ranks = map[string]float64{}
			h.Log.Printf("zone: SPAWN %d (%q): the defender's MON_DR / MON_DefLayer / "+
				"MON_RSTAT could not be read (%v) - a debuff rule will read 0 for them",
				spec.MonsterID, row.Name, ranksErr)
		}
	}

	// THE EFFECTIVE TACTICS and the AI row they name (the monster chase,
	// zone\monster_chase.go). A script that overrides `Tactics` decides the body's
	// behaviour, so the OVERRIDE wins over the row's own name - exactly as the
	// reference's spawn does (`pIesObj->Set(Tactics, tactics)` in CreateTmonster,
	// SystemFunc.cpp:1078-1079). An EMPTY override uses the row's own name, which
	// is what every probe that is not testing tactics wants: the probe must not
	// turn an aggressive monster passive by naming a mission prop.
	effectiveTactics := strings.TrimSpace(spec.Tactics)
	if effectiveTactics == "" {
		effectiveTactics = strings.TrimSpace(row.Tactics)
	}
	ai := row.AI()
	aiKnown := row.AIKnown
	if effectiveTactics != strings.TrimSpace(row.Tactics) {
		ai, aiKnown = h.Monsters.AI(effectiveTactics)
	}

	h.spawnMu.Lock()
	// THE HANDLE: from the process's own seeded, rising block (monster.go,
	// nextMonsterHandleLocked). It is one call for every way a body is created -
	// this spawn, a map body, -spawndummy and a respawn - so a number is never
	// handed out twice in the life of the process, whatever happened to the body
	// that held it.
	handle := h.nextMonsterHandleLocked()
	m := Monster{
		Handle:    handle,
		X:         pos[0],
		Y:         pos[1],
		Cell:      pos[2],
		SpawnX:    pos[0],
		SpawnY:    pos[1],
		SpawnCell: pos[2],
		GroundZ:   height, // the mesh's own height under the spawn: the z every later walk hint needs
		ClassID:   uint32(spec.MonsterID),
		Level:     level,
		HP:        int32HP(float64(hp)),
		MaxHP:     float64(hp),
		Direction: byteAngle(spec.Direction),
		Tactics:   effectiveTactics,
		// THE DEFENCE, from the same row: what SCR_Get_MON_DEF reads (zone\
		// monster_def.go). Copied, not derived - the row IS the game's answer.
		RaceType: row.RaceType,
		Arm:      row.Arm,
		DEFBM:    row.DEFBM,
		DEFPERBM: row.DEFPERBM,
		IMPBM:    row.IMPBM,
		SizeType: row.SizeType,

		// THE RANK / LAYER / RESIST INPUTS, from the same row: what
		// SCR_Get_MON_DR / SCR_Get_MON_DefLayer / SCR_Get_MON_RSTAT read when a
		// script message (a debuff) has to know the defender (zone\skillmsg.go).
		MobRank:    row.MobRank,
		DRBM:       row.DRBM,
		DefLayer:   row.DefLayer,
		DefLayerBM: row.DefLayerBM,
		RSTAT:      row.RSTAT,
		RSTATBM:    row.RSTATBM,
		// The script's own answers for the three computed properties, cached on the
		// body exactly as the reference caches them (see `ranks` above).
		MONDR:       ranks["MON_DR"],
		MONDefLayer: ranks["MON_DefLayer"],
		MONRSTAT:    ranks["MON_RSTAT"],

		// THE MAX-HP INPUTS, from the same row: what SCR_Get_MON_MHP reads
		// (zone\monster_hp.go). Kept on the body so a later reader can re-run the
		// same formula without going back to the table.
		CON:      row.CON,
		MHPFix:   row.MHPFix,
		MHPBM:    row.MHPBM,
		ADDMHPBM: row.ADDMHPBM,

		// THE ATTACK, from the same row: the game's own SCR_Get_MON_DAM (monsterATK),
		// the row's MinR/MaxR reach, and its SCR_Get_MON_ASPD cadence - see
		// zone\monster_swing.go. A row that states no attack leaves these zero and the
		// body never swings.
		//
		// ATK IS COMPUTED FROM THE SPAWNED LEVEL, not the row's: the script's
		// `CreateTmonster` can override it (`spec.Level`), and the reference's
		// SCR_Get_MON_DAM reads `self.Lv` - the level the body actually has.
		ATK:  monsterATK(atkRow),
		MinR: row.MinR,
		MaxR: row.MaxR,
		ASPD: monsterASPD(row),

		// THE ENGAGEMENT (the monster chase, zone\monster_chase.go): where this
		// body starts caring, how far it will follow, and how fast it walks. The
		// seek radius is the tactics ROW's own `SeekRange`; the leash is the
		// ATTACKER profile's KeepReturnRange. A body whose tactics name another
		// family, or whose row says Attack="FALSE" / SeekRange 0, keeps zeros
		// here and never chases. The speed is the row's own SCR_Get_MON_MSPD - the
		// WALK a body with no engagement carries. A body with one (a chase or an
		// attack order in flight) runs at the row's SPDBM rule instead
		// (monsterRunSpeed, zone\monster_chase.go).
		AssaultRange:    monsterAssaultRange(ai),
		KeepReturnRange: monsterKeepReturnRange(ai.AIType),
		ChaseSpeed:      monsterChaseSpeed(ai.AIType, row),

		// THE SWING'S TACTICS GATE: a row that says `Attack="FALSE"` must never
		// swing, even with a player inside MaxR. Resolved here, once, from the same
		// tactics row the chase reads; an unresolved tactics row leaves it false so
		// the body's own ATK/MaxR gate still decides (see Monster.AINoAttack).
		AINoAttack: aiKnown && !ai.Attack,

		// THE SHAPE'S DEATH ANIMATION, from the same row's shape (see MonsterRow):
		// how long the client's own death animation runs, so the corpse knows how
		// long the body must stay for it (zone/respawn.go).
		DyingTime:  row.DyingTime,
		ShapeKnown: row.ShapeKnown,

		// THE SPAWNER'S OWN CLOCK (zone/spawner.go): the type row's `RespawnTime`
		// when a map spawner placed this body, and `HasSpawner` says that row exists.
		// A probe or a script's spawn passes neither, and its body does not come back.
		// Carried on the body because the kill happens with no table at hand.
		HasSpawner:     spec.HasSpawner,
		SpawnerDelay:   spec.Respawn,
		SpawnerClassID: spec.SpawnerClassID,
	}
	h.spawnedMonster = append(h.spawnedMonster, m)
	// THE MONSTER'S LIVE HP STARTS HERE, at the number the spawn or the game's own
	// SCR_Get_MON_MHP gave it, and lives in monsterHP - NOT in Monster.HP, which is
	// an int32 and cannot hold the reference's own Scarecrow HP (999,350,000,000 or
	// 1.012e12, measured in `history\combat-attack-heal.md` §2.3). A float64 can hold
	// either exactly, and so can the 3527's HPTYPE field (see zone\monster.go emHP).
	//
	// ONLY WHEN IT IS ABOVE ZERO. The shipped Monster table has NO MHP column at all
	// (`datatable_monster.xml` carries `HP`, `RHP` and `MHP_BM`, never `MHP`, so
	// MonsterRow.MHP is 0 for all 7,725 rows). A spawn that passes no MHP now asks
	// the game's script; a script that cannot answer leaves 0 and logMaxHPUnavailable
	// says so ONCE. Writing a 0 there would be the worst of both: the attack lane
	// would report HP 0 before a hit, and the client turns `+50 - damage <= 0` into a
	// DEATH animation (`GENormalNet_Action.cpp:192-196` -> `GeAttack.cpp:45`
	// SetDeadlyHit). So the map stays empty for an unanswered body and the attack
	// lane refuses to invent one - see damageMonster.
	if hp > 0 {
		if h.monsterHP == nil {
			h.monsterHP = make(map[uint32]float64, 4)
		}
		h.monsterHP[handle] = float64(hp)
	}
	h.spawnMu.Unlock()

	pkt := BuildEnterStandingMonster(m)
	sent := h.broadcastWorldAt(pkt, m.X, m.Y, m.Handle, sightKindMonster)
	metrics.sightEnterN(sent)
	// THE DECISION, ONCE PER BODY: which tactics row this body runs, and what that
	// row says the body may do. A reader of the log can now tell a passive mission
	// prop (tactics MON_MISSION_84, AIType None, attack no, seek 0) from an
	// aggressive monster (MON_AGGRESSIVE, AGGRESSIVE, attack yes, seek 2500) without
	// opening the client's XML. `attack` is the swing gate: `no` only when the
	// tactics row states Attack="FALSE"; a body with no tactics row keeps its own
	// ATK/MaxR gate and is printed `yes`.
	aiLabel := ai.AIType
	if strings.TrimSpace(aiLabel) == "" {
		aiLabel = "-"
	}
	attackLabel := "yes"
	if aiKnown && !ai.Attack {
		attackLabel = "no"
	}
	if h.quietSpawnFor(aiKnown, aiLabel) {
		// `-quietspawn`: the DECISION is counted, not written - one aggregate line
		// per distinct (class, tactics, AIType, attack, seek) when the burst ends
		// (zone\spawn_quiet.go). The event itself is unchanged: the packet was built
		// and broadcast above, and the body is in the roster.
		h.spawnQuiet.add(spawnQuietKey{
			Class:   spec.MonsterID,
			Name:    row.Name,
			Tactics: effectiveTactics,
			AIType:  aiLabel,
			Attack:  attackLabel == "yes",
			Seek:    ai.SeekRange,
		}, m.Cell)
	} else {
		h.Log.Printf("zone: MONSTER AI class %d tactics %q AIType %s attack %s seek %g",
			spec.MonsterID, effectiveTactics, aiLabel, attackLabel, ai.SeekRange)

		// `hp` and not `m.HP`: the same number, from the int64 it was computed in. A
		// Scarecrow whose script asked for 1e12 has no honest int32, and this line is
		// the one a reader checks the HP ledger and the 3527's own HP field against.
		h.Log.Printf("zone: CREATE_TMONSTER %q (class %d, Lv %d, EXP %g, tactics %q) at "+
			"(%d, %d, cell %d), ground height %d from the mesh, dir %d, hp %g -> handle "+
			"0x%08x, cmd %d (%d B) to %d player(s)",
			row.Name, spec.MonsterID, level, row.Exp, effectiveTactics, m.X, m.Y, m.Cell,
			height, m.Direction, float64(hp), m.Handle,
			binary.LittleEndian.Uint16(pkt), len(pkt), sent)
	}
	return handle, nil
}

// broadcastWorld sends ONE world-object packet to every player in the zone,
// including the one whose click made it: the roster is the only list of sessions
// there is, and the client skips a handle it already holds.
//
// The log line for the object is written by the caller, so a failure here only
// says how many players ACCEPTED the packet - a client that is gone must not stop
// the packet going to the others.
func (h *Handler) broadcastWorld(pkt []byte) int {
	sent := 0
	for _, m := range h.Players.Snapshot() {
		if err := m.Sess.send(pkt); err != nil {
			h.Log.Printf("zone: could not tell handle %d about a new world object: %v",
				m.Chars[0].Handle, err)
			continue
		}
		sent++
	}
	return sent
}

// damageMonster takes ONE hit off a monster's live HP and answers the HP BEFORE
// and AFTER it, which are the two numbers ZC_ATTACK_SINGLE +50 and ZC_ADD_HP +6
// carry (`history\combat-attack-heal.md` §2.1: +50 is the value before the
// subtraction, the 3557's +6 is the value after).
//
// ok is false when this zone has NO HP for the monster, and then nothing is
// written: the caller must not invent one (see the attack lane's refusal - a 0
// makes the client's own `hp - damage` land under zero, and the reference client
// answers that with its death animation).
//
// THE CLAMP AT ZERO IS THE PLATE, NOT A KILL - but THE KILL IS COUNTED HERE, ONCE (R98
// slice 1). A hit that takes a POSITIVE HP to exactly zero is the only thing that arms
// a ZC_DEAD, and the packet LEAVES IN THIS CALL (2026-11-07): the reference's 500 ms
// m_deadTime was being spent waiting for the zone's next tick, which is why the owner
// measured the same code sending it 101 ms after one kill and 503 ms after another.
// The 3586 is built and broadcast right after the kill line below, so it leaves with
// the blow; the tick is left with the corpse's leave and the body's return only.
// This lane still does not send 3531 ZC_LEAVE_MONSTER or the exp itself here - EXP and
// the drop roll are the calls below, and 3531 is zone\respawn.go. The delta the caller
// sends is still the full hit (§2.1 pins the 3564's damage and the 3557's delta to the
// same double), so the last hit that empties a dummy reports 0 and the log says so.
func (h *Handler) damageMonster(m Monster, damage float64) (before, after float64, ok bool) {
	return h.damageMonsterBy(m, damage, 0, 0)
}

// damageMonsterBy is damageMonster with the KILLER named, which the drop roll needs
// twice: the kill line prints the handle, and the drop penalty is looked up by the
// killer's level (`MonsterNPC.cpp:764-766` passes `m_killer->GetLv()`). A nil killer
// (0) is legal - the reference then falls back to the monster's own level, and so does
// zone\drop.go.
func (h *Handler) damageMonsterBy(m Monster, damage float64, killer uint32, killerLv int) (before, after float64, ok bool) {
	before, after, ok, armed := h.takeHP(m, damage, killer, killerLv)
	// THE KILL'S OWN LINE, WRITTEN OUTSIDE THE LEDGER'S LOCK: it names the monster (a
	// table read) and counts the players (a roster read), and neither may happen while
	// a second hit on another body waits for spawnMu.
	// NOTHING IS SENT HERE ANY MORE (2026-11-09). The reference sends a monster's
	// ZC_DEAD from BS_DEAD's ON_UPDATE, not from the hit that killed it:
	// `m_deadTime = m_leaveTime - 500` (source2 ZServerLib\MonsterNPC.cpp:2653-2655),
	// so the fall is drawn 500 ms BEFORE the body leaves and 1.5 s AFTER the killing
	// blow - after the attack animation it lands on. Sending it here put 3586 ahead of
	// the 3566 of the same blow, which is what the owner saw. `armed` still says the
	// kill happened; the corpse armed in takeHP now also owns the death packet
	// (zoneespawn.go corpseTick).
	_ = armed
	return before, after, ok
}

// takeHP is damageMonster's locked half: the ledger move, and the ONE place a death is
// armed. It answers whether THIS call was the kill.
//
// BEFORE > 0 IS WHAT MAKES IT THE KILL. Every later hit on the same body starts at 0
// and ends at 0, so it cannot arm a second packet. A body whose HP were refilled is a
// new life, not a second copy of the same death, and arming it again is right.
func (h *Handler) takeHP(m Monster, damage float64, killer uint32, killerLv int) (before, after float64, ok, armed bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	hp, known := h.monsterHP[m.Handle]
	if !known {
		return 0, 0, false, false
	}
	before = hp
	after = hp - damage
	if after < 0 {
		after = 0
	}
	if h.monsterHP == nil {
		h.monsterHP = make(map[uint32]float64, 4)
	}
	h.monsterHP[m.Handle] = after
	if before > 0 && after == 0 {
		// ONE CLOCK for the whole death: the instant of the kill, which is also the
		// instant the ZC_DEAD leaves (damageMonsterBy sends it as soon as this lock
		// is released), and - below - the corpse's leave and the body's return.
		killed := time.Now()
		// THE CORPSE IS ARMED IN THE SAME LOCKED STEP (zone/respawn.go): the body
		// leaves with 3531 reason 0 once the shape's death animation has had its time
		// - max(DyingTime, CorpseLeaveFloor) after the ZC_DEAD - and on its spawner
		// row's `RespawnTime` the species comes back at this point as a NEW handle with
		// its full HP. A body with no roster entry (not something this zone created) gets
		// no corpse; a body with no spawner row still gets the leave, but no return.
		h.armCorpseAt(m, killed, killer, killerLv, true)
		armed = true
	}
	return before, after, true, armed
}

// monsterLiveHP answers the HP this zone currently holds for one monster, without
// changing it. The hit path uses damageMonster, which is the ONLY writer; this is
// the reader the tests and the live probe use to say "the ledger and the packet
// agree".
func (h *Handler) monsterLiveHP(handle uint32) (float64, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	hp, ok := h.monsterHP[handle]
	return hp, ok
}

// deadPending answers whether the zone owes ANY death work, which is what tells a
// session's loop that it must keep ticking (zone\chase.go, tickIfDue) and keep waking
// up (zone\zone.go, Serve).
//
// IT IS THE CORPSE ONLY NOW (2026-11-07): the ZC_DEAD itself leaves with the killing
// hit, so by the time a tick runs there is no packet owed - what remains is the leave
// and the body's return (zone\respawn.go), one chain the loop must wake for. The
// session whose hit caused the kill can have nothing left to do, its swing repeat
// ending on the body being empty.
//
// IT IS NOT ONE OF wantsTick's FIVE REASONS ON PURPOSE. Those are the SESSION's own
// work; a monster's death belongs to the zone.
func (h *Handler) deadPending() bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	return len(h.corpses) > 0
}

// StartupTactics STARTS every scripted town NPC's tactics entry as a thread, at
// zone start, which is what the original does as each trigger object enters the
// world (`TriggerObject.cpp:49-57`, the FSM's TS_NONE state ->
// `RunTacticsStateThread`, the entry name `SCR_<script>_<STATE>` upper-cased).
//
// A THREAD, NOT A CALL (R141). Most entries return at once - and the thread is then
// spent, exactly as the reference's DestroyThread. Some never return: the battle
// maps' arm managers are `while(1) ... sleep(2000)` loops, and run as a one-shot call
// with a 3 s deadline they were aborted at the deadline without ever doing their job.
// Eight of them made 24.1 s of map 4063's 33.4 s start (2026-11-10). The thread parks
// on its own `sleep` and is woken by the zone's clock (zone\tactics.go).
//
// THIS is what actually puts the Scarecrow's four dummies on Thueringen Lakeside:
// `AUCH_HITDUMMY01`'s script is NPC_HITDUMMYMANAGER, so its entry is
// `SCR_NPC_HITDUMMYMANAGER_TS_NONE`, and that function is the only caller of
// `CreateTNPC` in the corpus. Before this existed the entry never ran - the go
// script host only ever ran `SCR_CLICK_` on a click - so the builtins alone would
// have changed nothing on screen.
//
// ONE call per NPC, not one per player: the objects enter the world once, and a
// second player entering must not create a second set of dummies. It runs before
// the listener opens for the same reason.
//
// An entry that does not exist is the normal case (most NPCs have no tactics
// script), so only a real ERROR is logged per NPC, and one summary line always
// says how many ran.
func (h *Handler) StartupTactics() {
	if h.Tactics == nil {
		h.Log.Printf("zone: no tactics scripts - the map's NPC scripts will not run " +
			"at start (a zone started with -no-scripts)")
		return
	}
	ran, failed := 0, 0
	for _, npc := range h.NPCs {
		if npc.Script == "" {
			continue
		}
		entry := TacticsEntry(npc.Script, "TS_NONE")
		self := ClickObject{
			Handle: npc.Handle, Name: npc.NameToken, Script: npc.Script,
			X: npc.X, Y: npc.Y, Z: npc.Cell,
		}
		found, err := h.Tactics(entry, self)
		if err != nil {
			failed++
			h.Log.Printf("zone: %s FAILED: %v", entry, err)
			continue
		}
		if found {
			ran++
			h.Log.Printf("zone: started %s as a thread for %s (handle 0x%08x)",
				entry, describeNPC(npc), npc.Handle)
		}
	}
	h.Log.Printf("zone: %d tactic thread(s) started at start over %d scripted NPC(s), "+
		"%d failed", ran, len(h.NPCs), failed)
}

// TacticsEntry is the original's own name for one tactics state:
// `sprintf_s(funcName,"SCR_%s_%s", m_scriptName, name); _strupr_s(funcName)`
// (TriggerObject.cpp:536-537). Exported so a test can pin the spelling.
func TacticsEntry(script, state string) string {
	return strings.ToUpper("SCR_" + script + "_" + state)
}

// byteAngle is the game's own `GetByteAngle`, which is NOT a cast:
//
//	inline BYTE GetByteAngle(int angleInDegree)
//	{ return (BYTE)(((float(angleInDegree)/360.0f)*255.0f) + 0.5f); }
//	                                          gePacketBase.h:11-14
//
// A plain `uint8(deg)` is the bug this project has already paid for once: it turns
// 278 into 22, a silent 256-degree error, and the NPC faces the wrong way
// (`mapdata.go`, TargetDirByte). The degrees are wrapped into one compass turn
// first, because a negative angle has no meaning to a byte and nothing in the
// game's data uses one.
func byteAngle(deg float64) uint8 {
	d := math.Mod(deg, 360)
	if d < 0 {
		d += 360
	}
	return uint8(d/360.0*255.0 + 0.5)
}

// NPC_HANDLE_BASE is the block the game uses for NPC handles, and it is the SAME
// number `src\emit_zone_npcs.py` writes into every town NPC's row. Runtime NPCs
// continue its numbering (see the handle-block note at the top of this file).
const NPC_HANDLE_BASE = 0x40000000

// runtimeMonsterFirst is the first index `CreateTmonster` hands out in the monster
// block. Index 1 is already owned by the one fixed monster of the parked S1 slice
// (`DevStandingMonster`, and `monster_capture_test.go` pins that number), so the
// runtime counter starts above it.
const runtimeMonsterFirst = 2
