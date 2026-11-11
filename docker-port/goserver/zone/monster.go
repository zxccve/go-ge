package zone

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ONE STANDING MONSTER - slice S1 of the combat plan (`history\combat.md` §6).
//
// The client draws a monster from `ZC_ENTER_STANDINGMONSTER` (3527), a FIXED
// 360-byte packet: cmd(2), handle(4), then a block of mostly-zero bytes in which
// only the position and a few appearance words are non-zero. Every byte this file
// writes is either MEASURED from a live packet or named as a choice - nothing here
// is filler invented to fill 360 bytes.
//
// THE ORACLE. `zone\testdata\live_enter_monster.bin` is one real 3527, cut
// unchanged out of `captures\queststage5\20261023-160543\s2c_7102_63470.bin` at
// file offset +296 (the first of the 56 live 3527s in that recording; the project's
// own splitter, `src\analyze_capture.py`, says 3527 is 360 bytes and the next packet
// starts exactly 360 bytes later, at +656). `TestEnterMonsterMatchesLiveCapture`
// holds this builder against those bytes one at a time.
//
// WHAT THE LIVE TRAFFIC SAYS VARIES, across all 56 packets of that recording:
//
//	+2..+5   the handle                        (per monster - allowed to differ)
//	+15..+26 x, y, and the navmesh CELL        (per monster - allowed to differ)
//	+28..+35 the CURRENT HP, a float64         (4 values: ~1,023,356 and three
//	                                           neighbours - one per monster type)
//	+292,+306 a WORD pair that moves together  (19763/394, 19765/396, 19764/399,
//	                                            19766/401)
//	+298..+305 the MAX HP, a float64           (the SAME number as +28..35: a live
//	                                           monster at full health)
//	+358     a WORD                            (261 in 52 of 56, 515 in 3, 771 in 1)
//	+343     STALE BYTE - see the note below
//
// Two things `history\combat.md` §2 lists as constant are NOT constant, measured
// over the whole recording rather than the twelve instances it compared:
//
//   - +32 is not one value. It is 10.951897 in 40 of 56 packets and three other
//     values in the rest, and +302 always carries the same value as +32. THAT IS THE
//     HP FIELD'S HIGH HALF: +28..+35 is a float64 (current HP) whose four values are
//     ~1,023,356 for four monster types, and +298..+305 is the second float64 (max
//     HP). The old reading - "a float scale at +32 and +302" - was two half-numbers
//     agreeing. The client binary proves it: see emHP/emMaxHP below.
//   - +343 holds leftovers, not data: the buffer was reused and not zeroed, which is
//     why some packets carry fragments there. The fixture is the packet whose
//     leftovers are ZERO, so this builder writing zero there is a copy of the capture
//     and not a tidy-up.
//
// WHAT IS STILL UNKNOWN, and is therefore NOT guessed at: which monster MODEL the
// client draws. No capture holds `ZC_MONSTER_PROP` (3806) or `ZC_MON_PROP` (3330) -
// 0 in all 25 recordings - and 3526 and 3527 carry the same 360-byte shape, so the
// block below is the whole of what the live server sent for a monster whose body
// the client did draw. The class id is at +292 (see emClass). S1 proves the BYTES;
// it does not prove a body appears, and the probe's log line says what to look at.

const (
	// CmdZCEnterStandingMonster is PKS_ZC_ENTER_STANDINGMONSTER: a monster the client
	// has not been told about yet appears in view.
	CmdZCEnterStandingMonster = 3527

	// EnterStandingMonsterSize is the whole packet, FIXED - `data\cmd_table_live_z.txt`
	// says 360, the live stream agrees, and there is no length word to disagree with.
	EnterStandingMonsterSize = 360

	// CmdZCLeaveMonster is PKS_ZC_LEAVE_MONSTER: the client drops the object. The
	// length is FIXED and small, which is why there is no length word in it either.
	//
	// THE SIZE IS THE CLIENT'S OWN TABLE, not an addition: `data\cmd_table_live.txt`
	// line 1198 and `data\cmd_table_live_z.txt` line 1198 both say
	// `3531  8  ZC_LEAVE_MONSTER`. Eight is exactly the struct below, so the table
	// and the header agree and nothing here is padded to fit:
	//
	//	PKS_DEFAULT  WORD     command   -> +0, 2 bytes (gePacketBase.h:38-41)
	//	PKS_ZC_LEAVE OBJHANDLE handle    -> +2, 4 bytes (geZCPacket.h:1063-1066)
	//	             WORD      code      -> +6, 2 bytes
	CmdZCLeaveMonster = 3531

	// LeaveMonsterSize is the whole packet: cmd(2) + handle(4) + code(2).
	LeaveMonsterSize = 8
)

// The `code` word of a leave packet, from the client's own enum
// (`geShared\include\geZCTypes.h:427-432`):
//
//	OBJ_LEAVE_LOGOUT = 0   the object is gone (a monster's death path uses this)
//	OBJ_DEAD         = 1   never written by the shipped server anywhere
//	OBJ_TELEPORT     = 2
//	OBJ_OUTOFSIGHT   = 3   the client FADES the object out instead of dropping it
//
// AND IT IS NOT COSMETIC: `CNormalNet::LeaveMonster` reads this word
// (`geClient\Source\GENormalNet_World.cpp:262-273`) and only a 3 fades.
//
// A kill is 0 because that is what the shipped server sends on the death path: an
// `OM_KILL` sets the monster ZOMBIE (`MonsterNPC.cpp:149-171`), and the viewer then
// sends its leave with `OBJ_LEAVE_LOGOUT` (`View.cpp:193-196`). Nothing in
// `geServer` writes OBJ_DEAD, so a 1 here would be an invented value.
const (
	LeaveCodeLogout     uint16 = 0 // OBJ_LEAVE_LOGOUT - the object is gone
	LeaveCodeTeleport   uint16 = 2 // OBJ_TELEPORT
	LeaveCodeOutOfSight uint16 = 3 // OBJ_OUTOFSIGHT - the client fades it out
)

// The two field offsets of a leave packet (see CmdZCLeaveMonster above).
const (
	lmHandle = 2 // DWORD the handle of the object leaving
	lmCode   = 6 // WORD  WHY it left
)

// ZC_DEAD: THE BODY IS DEAD (R98 slice 1). The client plays the death animation and
// zeroes its own HP plate for the handle; the object stays in the world until
// ZC_LEAVE_MONSTER (3531) takes it off the screen.
//
// THE STRUCT IS THE HEADER'S, and it is the smallest packet this zone sends:
// `PKS_ZC_DEAD` is `PKS_DEFAULT` + one OBJHANDLE
// (`source2\geShared\include\geZCPacket.h:1059-1061`), so it carries NO security
// header, no length word and no padding:
//
//	+0 WORD       command   3586
//	+2 OBJHANDLE  the monster
//
// THE ID IS THE CLIENT'S OWN TABLE, NEVER THE HEADER'S ARITHMETIC. `data\cmd_table.txt:1060`
// reads `3586   6     PKS_ZC_DEAD`; the header's own enum would give 3072, because the
// shipped numbering is not the enum's order (`history\monster-death.md` §1).
const (
	// CmdZCDead is PKS_ZC_DEAD: the monster is dead.
	CmdZCDead = 3586

	// DeadSize is the whole packet: cmd(2) + handle(4), exactly the table's 6.
	DeadSize = 6
)

// dmHandle is where the monster's handle sits in a ZC_DEAD.
const dmHandle = 2

// BuildDead builds the 6-byte ZC_DEAD for one monster.
//
// Six bytes and nothing else: the reference's builder writes the struct it declared
// (`MonsterNPC.cpp:2629-2632`) and the client's `CNormalNet::Dead`
// (`geClient\Source\GENormalNet_World.cpp:16-38`) reads the command and the handle,
// so a longer packet would be bytes neither side reads.
func BuildDead(handle uint32) []byte {
	out := make([]byte, DeadSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCDead)
	binary.LittleEndian.PutUint32(out[dmHandle:], handle)
	return out
}

const (
	emHandle = 2  // DWORD the monster's handle
	emFlag   = 6  // BYTE 1 in every one of the 56
	emPos    = 15 // x, y, cell - three INT32, the same shape a WORLDPOS has

	// emHP is the monster's CURRENT HP. The field is an HPTYPE, and on this client
	// an HPTYPE is a FLOAT64 (8 bytes), NOT an int32. THE CLIENT'S OWN PARSER IS THE
	// PROOF: `FUN_004f3ebe` (the 2020 client's handler for case 0x49 = 3527) copies
	// 8 bytes from packet+0x1c, and `geactor::MonsterEnter` reads them as a double
	// (`*(double *)(param+0x20)`) before storing it as the object's HP. The same
	// double appears in the capture at +0x1c..0x23: 1,023,356.0, one live monster's
	// full HP. The old reading of +32 as a "float scale 0x412F3AF8" was the HIGH HALF
	// of this number - there is no separate scale field here.
	emHP = 28

	// emClass is the monster's CLASS ID - a DWORD, and the ONE field the client
	// needs to draw a body at all. It is named emWord292 above in the parked lane's
	// first reading as "a WORD"; it is not a WORD and it is not a constant: it is
	// the `Monster` table's ClassID (11600 for the Scarecrow), and it is REQUIRED.
	//
	// THE PROOF, from the shipped client's own handler rather than from the header
	// (`ge.exe` CNormalNet::EnterStandingMonster at 0x4f3ebe, reached by the xref to
	// the string 0x9d4c20 "Mon type : %d"):
	//
	//	0x4f3eea LEA  ESI,[EBX+0x124]    ; +292
	//	0x4f3ef0 PUSH [ESI]              ; the 32-bit value there
	//	0x4f3ef2 PUSH 0x9d4c20           ; "Mon type : %d"
	//
	// and in the client's object code, `GeBaseObject.cpp:227-232` ->
	// `Monster.cpp:31` `GetPropertyString("Monster", Type, "Shape")` -> `BindModel()`.
	// The 56 live packets carry four different values here (19763 x40, 19765 x12,
	// 19766 x3, 19764 x1) because the capture held four monster types - it is
	// per-monster, exactly as a class id would be.
	//
	// WHAT HAPPENS IF IT IS 0: `imcIES::GetClass` answers NULL for an id it does not
	// know (`ClassTable.cpp:62-66`) and the client dereferences it before any model
	// work. So a 0 here is a client-side crash, not an empty spot - which is why the
	// builder takes the class from the caller and the world REFUSES a monster whose
	// class it cannot resolve (zone\create.go).
	emClass = 292

	// emMaxHP is the monster's MAXIMUM HP: `APPEARANCE_MONSTER.mhp`, at appearance+6
	// (the appearance block starts at emClass). ALSO 8 BYTES AND ALSO A DOUBLE, and
	// it is what the client's target frame divides by.
	//
	// THE PROOF IS THE CLIENT BINARY, twice over:
	//   - `FUN_004f3ebe` passes packet+0x124 as the appearance pointer, so the block
	//     starts at +292;
	//   - `CBaseObject::MonsterEnterWorld` (decompiled) stores the 8 bytes at
	//     `appearance + 6` straight into the object's `m_MHP`, and the level byte at
	//     `appearance + 14` into m_Lv. The captured bytes at +298..305 are the SAME
	//     double as +28..35 (1,023,356.0): a live monster at full HP sends its max,
	//     and the "float at +302" is that double's high half.
	//
	// The target frame's gauge is `CTargetInfoFrame::SetHP(m_HP, m_MHP)`, so a zero
	// here is a monster with no HP bar at all - the symptom the Bigfoot showed.
	emMaxHP = emClass + 6 // 298

	emWord306 = 306 // WORD - moves 1:1 with the class; meaning NOT established
	emByte308 = 308 // BYTE
	emWord347 = 347 // WORD: the top half of a FLOAT 1.0 at +346
	emByte350 = 350 // BYTE
	emWord358 = 358 // WORD
)

// The captured constants, named so the builder never invents one.
//
// A field whose meaning is not established is named by its OFFSET and carries the
// value the capture has. Naming it "radius" or "colour" would be a guess wearing a
// label, and the next lane would trust it.
const (
	// emFlagValue: +6 is 1 in all 56 live packets. Not 0 anywhere, so it is written.
	emFlagValue = 1

	// emCaptureClass is the class id the FIXTURE's monster had (0x4D33 = 19763,
	// ClassName "mon_flealouse1", Lv 140, Size S). It is not a constant of the packet:
	// it is one monster's class, and it is here only so the one fixed S1 monster can
	// be held against the capture byte for byte.
	emCaptureClass = 19763

	// capturedMaxHP is the HP the FIXTURE's own 3527 carries: the 8 bytes at +0x1c
	// (and again at appearance+6) decode to this double. It is one live monster's
	// full HP, kept here for the S1 probe, which has no row and no script to ask -
	// the packet it sends is the captured packet, so it keeps the captured number.
	capturedMaxHP = 1_023_356.0

	// +306 WORD 394 (0x018A) moves 1:1 with the class across the 56 packets
	// (19763/394, 19765/396, 19764/399, 19766/401). What produces it is NOT
	// established: it is not the monster shape's ClassID (394 is nothing like 15114
	// for mon_flealouse1's shape) and not its row number in any table that was
	// checked. So the captured value is kept and the log line for a script-made
	// monster says these bytes were copied rather than derived.
	emWord306Value = 394

	// +308 BYTE 1, in all 56.
	emByte308Value = 1

	// +346 is FLOAT 1.0 (0x3F800000) and only its top two bytes are non-zero, so this
	// is a WORD write at +347. A scale of 1.0 is the least surprising reading; the
	// name stays "word 347" for the same reason as above.
	emWord347Value = 0x3F80

	// +350 BYTE 1, in all 56.
	emByte350Value = 1

	// +358 WORD 261 (0x0105) in 52 of 56, 515 (0x0203) in three, 771 (0x0305) in one.
	// Two enum bytes, most likely; copied from the fixture.
	emWord358Value = 261
)

// Monster is one standing monster as the client needs it: a handle, a position and
// a navmesh CELL.
//
// The CELL is a triangle index, never a height - the same rule the team's spawn and
// the town NPCs live under. A cell that is not the ground at (X, Y) draws NOTHING,
// which is why `Handler.spawnDevMonster` and `Handler.SpawnMonsterObject` both ask
// the mesh before they send.
//
// ClassID, Level, HP, Direction and Tactics are NOT part of the packet this lane
// sends (see the offsets above and BuildEnterStandingMonster): they are what the
// monster IS, kept so the damage slice has a real target to describe, and so the
// creation log line can name the row the script asked for. A field that the wire
// turns out to carry is added to the builder with its own measured offset - never
// inferred from its name.
type Monster struct {
	Handle uint32
	X      int32
	Y      int32
	Cell   int32
	// GroundZ is the MESH HEIGHT under (X, Y) at the moment the body was placed - the z
	// of the reference's `m_createdPos = pos` (MonsterNPC.cpp:1409-1414), which is a 3D
	// point and not a cell.
	//
	// IT IS CARRIED BECAUSE THE MESH NEEDS A z HINT, and 0 is the wrong one on any map
	// whose ground is far below zero. The measured failure (btl_auch01, map 1361,
	// 2026-11-09): `monsterHomeAt` asked the mesh at (x, y, 0) and got "no ground within
	// 1000 units" for ground at -6,027, so the body's HOME HEIGHT became 0 and every
	// script walk - `RandomMove`, `RandomMoveCreatedLocaly`, a `Move` that names the home
	// z - was refused with the same message. 93 such refusals in one 34-second test.
	// `Cell` is a navmesh CELL (triangle id) and cannot stand in for the height.
	GroundZ int32
	// SpawnX/Y/Cell are the body's BIRTH point, kept for the respawn: `bringBack` puts the
	// new body back here and NOT where the old one died (2026-11-09, the owner: "they spawn
	// near the death spot? i thought they spawn at their own spawn spot?"). The roster entry
	// is live - a walk writes X/Y/Cell back into it (zone\monster_chase.go), so by the time
	// a body dies its X/Y is the corpse's spot, and the template alone cannot answer where
	// the species lives. Set once, at creation, and never written again.
	SpawnX    int32
	SpawnY    int32
	SpawnCell int32
	ClassID   uint32 // the Monster table's ClassID (11600 for the Scarecrow)
	Level     int
	HP        int32
	Direction uint8
	Tactics   string

	// MaxHP is the body's MAXIMUM HP, and at a spawn it is also the current HP the
	// 3527 carries (the reference writes `packet.hp = GetHP()` and
	// `appearance.mhp = m_MHP`, and a fresh body's HP is its max). It is float64
	// because the reference's own Scarecrow sat at ~1.0e12, which an int32 cannot
	// hold - see zone\create.go's ledger comment.
	//
	// `HP` above is a legacy int32 display copy; it is CLAMPED and must never price
	// damage. The live HP is Handler.monsterHP, keyed by handle.
	MaxHP float64

	// THE DEFENCE OF THIS BODY, carried with it (R84 slice 2). These are the columns
	// the game's own `SCR_Get_MON_DEF` reads (monster\get_status_monster.lua:481-505)
	// and the ones the damage rule reads (common_monster.lua:2884-2896). They are
	// copied from the table row at spawn time and are 0 / "" for a monster with no
	// row behind it, which is the reference's own answer for an absent column.
	//
	// NONE OF THEM IS ON THE WIRE: they exist so a cast has a real target to price.
	RaceType string
	Arm      float64
	DEFBM    float64
	DEFPERBM float64
	IMPBM    float64
	SizeType string

	// THE RANK / LAYER / RESIST INPUTS of the game's computed properties
	// (MobRank/DR_BM/DefLayer/DefLayer_BM/RSTAT/RSTAT_BM). They are the row's own
	// columns, carried on the body so a script message can run the SAME entry points
	// (`SCR_Get_MON_DR`, `SCR_Get_MON_DefLayer`, `SCR_Get_MON_RSTAT`) the reference
	// caches as `MON_DR` / `MON_DefLayer` / `MON_RSTAT` - see zone\skillmsg.go.
	MobRank    float64
	DRBM       float64
	DefLayer   float64
	DefLayerBM float64
	RSTAT      float64
	RSTATBM    float64

	// THE THREE COMPUTED PROPERTIES a debuff rule reads off the DEFENDER
	// (`SCR_RESULT_BATTLEBUFF_SKILL`: `self.MON_DR`, `self.MON_RSTAT`,
	// `self.MON_DefLayer`, common_pc.scp:6752-6760). The reference caches them on the
	// body (`CP_MON_DR="SCR_Get_MON_DR"` in the row; MonsterNPC.h:143), which is
	// exactly what a spawn does here: zone\monster_def.go's Ranks runs the three entry
	// points ONCE with the body's own level and the answers are carried. 0 means "no
	// script source was armed", never a guessed rank.
	MONDR       float64
	MONDefLayer float64
	MONRSTAT    float64

	// THE MAX-HP COLUMNS of this body (see create_tables.go): exactly the row
	// attributes the game's own `SCR_Get_MON_MHP` reads. They are copied from the
	// row so the script can run for a Monster already in the world.
	CON      float64
	MHPFix   float64
	MHPBM    float64
	ADDMHPBM float64

	// THE ATTACK OF THIS BODY (the monster swing): what the row's own columns say, as
	// the game's scripts read them. ATK is `SCR_Get_MON_DAM` for a monster with no
	// owner (get_status_monster.lua:398-471, via zone\monster_swing.go's monsterATK);
	// MinR/MaxR are the row's own reach (MonsterNPC.cpp:1035-1046); ASPD is the
	// SCR_Get_MON_ASPD delay in MILLISECONDS (get_status_monster.lua:496-521), which
	// is BOTH the cadence the FSM re-arms with (`FieldChar_FSM.cpp:135`) and the
	// number the 3564 carries at +20.
	//
	// ZERO IS "THIS BODY CANNOT ATTACK": a row with no attack power, no reach or no
	// shape Hits is not given a made-up one, and the swing tick skips it.
	ATK  float64
	MinR float64
	MaxR float64
	ASPD float64

	// THE ENGAGEMENT (the monster chase, zone\monster_chase.go): where this body
	// starts caring, how far it will follow, and how fast it walks. Zero
	// AssaultRange or zero ChaseSpeed means "this body does not chase" - it stands
	// where it was put and swings at what comes into reach, which is what every
	// monster did before this slice, so an unfinished row degrades to the old
	// behaviour instead of guessing.
	//
	// ASSAULT RANGE is the body's OWN tactics row's `SeekRange`
	// (`datatable_monsterai.xml`; MON_AGGRESSIVE 2500), which is what the game's AI
	// script hands `GetObjectLocaly` to find a target. KEEP RETURN RANGE is the
	// ATTACKER profile's `KeepReturnRange` (2000). CHASE SPEED is
	// `SCR_Get_MON_MSPD` of the row (143 for class 11591).
	AssaultRange    float64
	KeepReturnRange float64
	ChaseSpeed      float64

	// AINoAttack is true when the tactics row this body runs says
	// `Attack="FALSE"` (datatable_monsterai.xml) - a mission prop, a marker, a
	// body that is not supposed to swing. It is resolved ONCE at spawn, from the
	// same row the chase reads, and the swing tick refuses the body from then on.
	//
	// ZERO IS "NO SUCH ROW SAYS OTHERWISE": a body with no tactics row (or a
	// hand-built Monster in a test) keeps the swing's own ATK/MaxR gate rather
	// than having a missing file silently disarm every monster in the zone.
	AINoAttack bool

	// THE SHAPE'S DEATH ANIMATION (zone/respawn.go). `DyingTime` is how long the
	// client's own death animation runs for this body's shape, read from
	// `datatable_monster_shape.xml` through the row's `Shape` (create_tables.go) and
	// carried with the body so a corpse needs no table lookup at the kill. 0 is a
	// real answer; `ShapeKnown` false means there was no shape row to ask, and then
	// the corpse uses the zone's floor and says so once.
	//
	// NOTHING IS SENT FOR IT: the client plays `DIE1`/`DN` itself. The only thing
	// this number decides is how long the body must stay in the world for that
	// animation to finish before ZC_LEAVE_MONSTER takes it away.
	DyingTime  time.Duration
	ShapeKnown bool

	// THE SPAWNER BEHIND THIS BODY (zone/spawner.go). `SpawnerDelay` is the
	// `mongentype` row's own `RespawnTime`, carried from the spawn through the
	// corpse to the reborn body, so one field's monsters each return on their own
	// spawner's clock. `SpawnerClassID` is that row's ClassID, for the log.
	//
	// `HasSpawner` is what says a row EXISTS, and it is a separate field on purpose:
	// a row that states `RespawnTime="0"` is a row, and its body returns at once,
	// while `SpawnerDelay == 0` and `HasSpawner == false` means a body no spawner
	// placed - a probe (`-spawnmonster`, `-spawndummy`), a script's `CreateTmonster`,
	// or any body on a map without spawner data. That body DOES NOT COME BACK
	// (zone/respawn.go respawnDelayFor): there is no row to read an interval from and
	// none is invented. Reading `SpawnerDelay > 0` as the presence test would confuse
	// those two cases, which is exactly the fallback-style guess the owner ruled out.
	HasSpawner     bool
	SpawnerDelay   time.Duration
	SpawnerClassID uint32
}

// MonsterHandleBase is where this server's monster handles start.
//
// THE CLIENT TREATS A HANDLE AS AN OPAQUE KEY. The live server proved that by
// allocating monsters and players from ONE rising counter: its monsters were
// 189,764..190,259 while its own players were in the 190,3xx range, i.e. the
// numbers overlap in kind and are simply the next free value - nothing in the
// client reads a bit out of them. So any value that no OTHER actor in the session
// can hold is correct, and the choice below is about collision, not meaning:
//
//	players       the character's own row id, handed to us by the client in
//	              CZ_START_GAME (`zone\store.go:167`, `Handle: r.ID`) - four digits
//	              today (7, 38, 40, 1792)
//	town NPCs     0x40000000 + index (`src\emit_zone_npcs.py`, `npcs.go`)
//	warp points   0x7F000000 + key (`zone\gate.go:321`)
//	MONSTERS      0x20000000 + a counter, seeded per process  <- this, a block of its own
//
// 0x20000000 (536,870,912) is disjoint from all three: it is above any plausible
// database row id, below the NPC block, and it avoids the 0x40000000 bit the game
// uses for NPCs as well as the sign bit. The block is 0x10000000 wide (268,435,456
// numbers); a process may only use the lower half of it (monsterBlockSpan).
const MonsterHandleBase = 0x20000000

// THE NUMBER A BODY GETS IS NEVER ONE THE CLIENT HAS ALREADY SEEN, and that is a
// stronger rule than "unique inside the zone". `monsterSeq` alone is unique for the
// life of a process, but the CLIENT OUTLIVES A ZONE RESTART: the 2026-11-08 trace
// shows the same client still holding `0x2000000a` from the run that created
// `Monster_Hersu01` at 07:47, and then being sent a Bigfoot on that SAME number by
// the next run at 08:23 - 36 minutes apart, `zone: UP` at 08:17:53 in between, and
// no leave packet ever told the client the old actor was gone (a stopped process
// cannot send one). The client skips a handle it already holds, so the actor it
// kept and the body the zone now answers disagree: the owner stood beside a
// monster every press refused as 2,600 units away (`ATTACK APPROACH OUT OF REACH`
// x6, 08:26:23-44, all targeting 536870922).
//
// So each PROCESS starts its counter in a random window of the block. A restart
// cannot hand out a number an older run gave the same client (the odds are
// window/span per restart, and a zone makes thousands of bodies, not millions),
// and within the process the counter only rises - a body that dies and leaves
// never frees its number. `-spawndummy` bodies, script `CreateTmonster` bodies,
// map-spawned ones and respawns all come through nextMonsterHandleLocked.
const (
	// monsterBlockSpan is how much of the block one process may use. The upper
	// half is left alone so a very long run still cannot reach the NPC block.
	monsterBlockSpan = 0x08000000
	// monsterHandleWindow is the clearance a run keeps from the block's base and
	// from its end. The fixed S1 monster keeps index 1; a run starts far above it.
	monsterHandleWindow = 0x10000
)

var (
	// monsterHandleOnce seeds the process's counter on the first body.
	monsterHandleOnce sync.Once
	// monsterHandleNext is the next INDEX in the monster block, process-wide and
	// only ever added to (`atomic.AddUint32`). It is package state, not Handler
	// state, because "twice in the life of the process" is the promise - even if
	// two Handlers were ever built in one process (tests do).
	monsterHandleNext uint32
)

// pickMonsterSeed picks a first index in the monster block. It is called ONCE per
// process (monsterHandleOnce); the value it answers is not itself memoised.
//
// crypto/rand rather than the clock: a restart loop (a crash, a supervisor) can
// happen twice inside one clock tick, and this number is the one thing separating
// this run's handles from the ones a still-connected client is holding.
func pickMonsterSeed() uint32 {
	var raw [4]byte
	var v uint32
	if _, err := rand.Read(raw[:]); err == nil {
		v = binary.LittleEndian.Uint32(raw[:])
	} else {
		// No entropy at all (never seen on this host): the clock is still better
		// than the base, which is a number every previous run has already used.
		v = uint32(time.Now().UnixNano() / int64(time.Microsecond))
	}
	return monsterHandleWindow + v%(monsterBlockSpan-2*monsterHandleWindow)
}

// MonsterHandle is the handle of the n-th index in the monster block.
func MonsterHandle(index int) uint32 { return MonsterHandleBase + uint32(index) }

// nextMonsterHandleLocked answers the next handle this PROCESS hands out. Callers
// hold spawnMu (every caller does: SpawnMonsterObject and bringBack).
//
// MONOTONIC, NEVER REUSED: the number is a rising process-wide counter, so a body
// that died and left never frees its own, and the corpse's old handle can never
// name the body that comes back. The guard below is a loud assertion of that
// invariant, not a rule: it fires only if somebody later adds a second allocation
// path that does not come through here.
func (h *Handler) nextMonsterHandleLocked() uint32 {
	monsterHandleOnce.Do(func() {
		monsterHandleNext = pickMonsterSeed() + uint32(runtimeMonsterFirst)
		if h.Log != nil {
			h.Log.Printf("zone: MONSTER HANDLES this run start at 0x%08x - a seeded, rising "+
				"block: no two bodies share a number, and a restart does not hand out one a "+
				"client that outlived the last run is still holding (see nextMonsterHandleLocked)",
				MonsterHandle(int(monsterHandleNext)))
		}
	})
	index := atomic.AddUint32(&monsterHandleNext, 1) - 1
	handle := MonsterHandle(int(index))
	h.monsterSeq++
	if h.monsterHandleLiveLocked(handle) {
		if h.Log != nil {
			h.Log.Printf("zone: MONSTER HANDLE COLLISION 0x%08x - a live body or an owed "+
				"corpse already holds this number. The allocator is supposed to make that "+
				"impossible; a second allocation path exists", handle)
		}
	}
	return handle
}

// monsterHandleLiveLocked answers whether a handle is held by a live body or by a
// corpse that has not left yet. Callers hold spawnMu.
func (h *Handler) monsterHandleLiveLocked(handle uint32) bool {
	if h.monsterIndexLocked(handle) >= 0 {
		return true
	}
	for i := range h.corpses {
		if h.corpses[i].handle == handle {
			return true
		}
	}
	return false
}

// The S1 monster: one fixed spot on Thueringen Lakeside.
//
// THE CELL IS PROVED ON THE MAP'S OWN MESH, not chosen. `fld_auch.tok` (881 faces,
// the client's own `ge\bg\fld_auch.tok`, `cmp`-identical) was asked through the
// game's own path engine the way the Last Point asks:
//
//	valid -169 870 29   -> ok, ground height 26
//	valid -169 870 28   -> refused      (the cell is what makes this ground)
//	valid -169 870 30   -> refused
//	path  -419 870 29 -169 870 29 50 -> ok, 2 points, length 250, no detour
//	clamp -419 870 29 -169 870 50    -> free (nothing in the way)
//
// So it is 250 units east of the field spawn (-419, 870, cell 29) - close enough to
// be on screen when the team lands, and on the same walkable ground. The height is
// NOT sent: 3527 carries x, y and a CELL and no z, which is the same reason a
// WORLDPOS works.
const (
	devMonsterX    = -169
	devMonsterY    = 870
	devMonsterCell = 29
)

// DevStandingMonster is the one monster S1 puts in the world.
func DevStandingMonster() Monster {
	return Monster{
		Handle: MonsterHandle(1),
		X:      devMonsterX,
		Y:      devMonsterY,
		Cell:   devMonsterCell,
		// The class the FIXTURE's monster had. It is what makes this one packet
		// comparable to the capture byte for byte; a script-made monster carries its
		// own class instead (zone\create.go).
		ClassID: emCaptureClass,
		// THE FIXTURE'S OWN HP, decoded from the packet's +0x1c double. This body is
		// not a row and has no script, so the fixed packet keeps the captured number
		// rather than inventing one; it has no HP ledger either, so it cannot be hit.
		MaxHP: capturedMaxHP,
	}
}

// BuildEnterStandingMonster makes one ZC_ENTER_STANDINGMONSTER (3527), 360 bytes.
//
// The layout is the captured one, field for field:
//
//	+0   WORD  3527
//	+2   DWORD the handle                        (per monster)
//	+6   BYTE  1
//	+15  INT32 x                                 (per monster)
//	+19  INT32 y                                 (per monster)
//	+23  INT32 the navmesh CELL                  (per monster)
//	+28  HPTYPE the monster's CURRENT HP (float64, 8 bytes)
//	+292 DWORD the monster's CLASS id                (per monster, and REQUIRED)
//	+298 HPTYPE the monster's MAX HP (float64, 8 bytes; appearance+6)
//	+306 WORD  394
//	+308 BYTE  1
//	+347 WORD  0x3F80 - the top half of a FLOAT 1.0
//	+350 BYTE  1
//	+358 WORD  261
//
// Every other byte is ZERO, which is what the live server sent in the packet this
// builder is held against.
func BuildEnterStandingMonster(m Monster) []byte {
	out := make([]byte, EnterStandingMonsterSize)

	binary.LittleEndian.PutUint16(out[0:], CmdZCEnterStandingMonster)
	binary.LittleEndian.PutUint32(out[emHandle:], m.Handle)
	out[emFlag] = emFlagValue
	binary.LittleEndian.PutUint32(out[emPos:], uint32(m.X))
	binary.LittleEndian.PutUint32(out[emPos+4:], uint32(m.Y))
	binary.LittleEndian.PutUint32(out[emPos+8:], uint32(m.Cell))

	// ONE NUMBER, WRITTEN TWICE: the body's HP and its max. At a spawn they are the
	// same number (the reference sends `packet.hp = GetHP()` and
	// `appearance.mhp = m_MHP`, and a fresh body's HP is its max), so a reader cannot
	// find the ledger and the bar disagreeing. Both fields are float64 - see emHP
	// and emMaxHP - so the Scarecrow's ~1.0e12 goes out exactly.
	hp := hpFieldValue(m.MaxHP)
	binary.LittleEndian.PutUint64(out[emHP:], math.Float64bits(hp))

	// THE CLASS, a DWORD, and the only field here that decides WHICH BODY: see emClass.
	// A monster with no class is a client crash, not an empty spot, so the caller is
	// required to have resolved one.
	binary.LittleEndian.PutUint32(out[emClass:], m.ClassID)
	binary.LittleEndian.PutUint64(out[emMaxHP:], math.Float64bits(hp))
	binary.LittleEndian.PutUint16(out[emWord306:], emWord306Value)
	out[emByte308] = emByte308Value
	binary.LittleEndian.PutUint16(out[emWord347:], emWord347Value)
	out[emByte350] = emByte350Value
	binary.LittleEndian.PutUint16(out[emWord358:], emWord358Value)

	return out
}

// hpFieldValue is the ONE number a 3527 carries, in the form its field holds it.
//
// THE FIELD IS 8 BYTES WIDE (an HPTYPE float64 - see emHP), so the reference's own
// Scarecrow HP (~1.0e12) is carried EXACTLY and there is no narrower width to clamp
// against. The clamp that remains is the bottom one: a negative (or NaN) HP is 0,
// which is the client's own clamp in `CBaseObject::SetRealHP`.
func hpFieldValue(hp float64) float64 {
	if hp < 0 || math.IsNaN(hp) {
		return 0
	}
	return hp
}

// BuildLeaveMonster makes one ZC_LEAVE_MONSTER (3531), 8 bytes: the client takes
// the object with this handle off the screen.
//
// The layout is the shipped struct, field for field, and the length is the client's
// own table's (both quoted at CmdZCLeaveMonster):
//
//	+0 WORD  3531
//	+2 DWORD the handle of the object leaving
//	+6 WORD  code - WHY
//
// `code` matters to the client: only OBJ_OUTOFSIGHT (3) makes it FADE the object out
// (`GENormalNet_World.cpp:262-273`); anything else drops it at once, which is what a
// killed monster must do. Pass LeaveCodeLogout for a kill - the value the shipped
// server sends on its own death path.
func BuildLeaveMonster(handle uint32, code uint16) []byte {
	out := make([]byte, LeaveMonsterSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCLeaveMonster)
	binary.LittleEndian.PutUint32(out[lmHandle:], handle)
	binary.LittleEndian.PutUint16(out[lmCode:], code)
	return out
}

// spawnDevMonster answers the S1 packets for a session that is ready to see the
// world: ONE 3527, once, at the fixed spot - or nothing at all.
//
// It is called from the CZ_GAME_READY branch of Handler.Reply, in the same place
// the town NPCs and the door arrows are sent, because the client draws no actor
// before it has the world. It is NOT called on a move, on a tick, or on any other
// request: the monster stands still in S1, and a second copy of the packet would
// only re-send bytes the client already has (it skips a handle it knows).
//
// THE FLAG COMES FIRST. Handler.SpawnMonster is `-spawnmonster`, default OFF, and
// with it off this function returns before it touches anything - so every map and
// every other packet is exactly what it was before S1 existed.
//
// THE MESH IS THE SECOND GATE. A cell that is not the ground at (x, y) draws
// nothing at all, and that failure is invisible on the wire: the packet looks fine
// and the screen stays empty. So the fixed cell is asked of the map's own mesh
// (Handler.NavHeight, the game's `CWorld::positionIsValid`, the same call the Last
// Point makes) before anything is sent, and a zone whose mesh cannot confirm it
// sends NOTHING and says why. On a map with no path engine wired in this refuses
// too: an unproved cell is the one thing this slice must not ship.
func (h *Handler) spawnDevMonster() [][]byte {
	if !h.SpawnMonster {
		return nil
	}
	m := DevStandingMonster()

	if h.NavHeight == nil {
		h.Log.Printf("zone: DEV MONSTER not sent: no path engine is wired in, so the fixed "+
			"cell %d of (%d, %d) cannot be proved on this map's mesh - an unproved cell "+
			"draws nothing and looks like a server bug", m.Cell, m.X, m.Y)
		return nil
	}
	// The height is not part of the packet; it is the mesh's own ANSWER, which is the
	// proof that (x, y, cell) is real ground and not three numbers that look real.
	height, ok := h.NavHeight([3]int32{m.X, m.Y, m.Cell})
	if !ok {
		h.Log.Printf("zone: DEV MONSTER not sent: this map's mesh has no valid ground at "+
			"(%d, %d, cell %d) - the fixed cell was proved on fld_auch (map 1331), so a "+
			"zone on another map refuses instead of drawing nothing",
			m.X, m.Y, m.Cell)
		return nil
	}

	pkt := BuildEnterStandingMonster(m)
	// ONE LINE, and it carries what a human needs to check the card against the
	// screen: the handle the client now holds, the spot, and the mesh's own answer.
	h.Log.Printf("zone: DEV MONSTER announced: handle %d at (%d, %d) cell %d, "+
		"ground height %d from this map's mesh, %d bytes cmd %d",
		m.Handle, m.X, m.Y, m.Cell, height,
		len(pkt), binary.LittleEndian.Uint16(pkt))
	return [][]byte{pkt}
}
