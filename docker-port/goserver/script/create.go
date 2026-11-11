package script

// RUNTIME CREATION: `CreateTNPC` and `CreateTmonster`.
//
// The game's own scripts make actors while the server runs. The Scarecrow on
// Thueringen Lakeside is the case that matters: `npc_hitdummy_creater.scp` calls
// `CreateTNPC` four times when the "Scarecrow Creator" trigger enters the world
// (four standing dummies) and `CreateTmonster` once when a player configures one
// (the hittable Scarecrow itself). Both names were `nil` in this host, so the
// calls quietly answered 0 and nothing ever appeared - which is exactly what the
// owner walked the field and reported: "Scarecrow not found in Lakeside".
//
// THE ARGUMENT LISTS ARE THE SHIPPED SERVER'S, not a guess and not the brief's
// summary. `source2\geServer\ZServerLib\BindFunction\SystemFunc.cpp`:
//
//	BEGIN_FUNCTION(CreateTNPC, 0)              // SystemFunc.cpp:2068
//	  zoneInstID, npcName, npcTypeName, x, y, z, direction, Occupation[, Possession]
//	BEGIN_FUNCTION(CreateTmonster, 1)          // SystemFunc.cpp:1136
//	  zoneInstID, id, x, y, z, count, direction, lv, mhpFix, tactics[, startID]
//
// TWO THINGS THAT LOOK LIKE A CELL AND ARE NOT. The sixth argument of both is
// named `z` in the original and is handed to the map's mesh, not to the packet:
// `long point[] = {x, y, z};
//  world->Get3DPointToPosLocaly(shape, point, newPos, 1000, 300);`
// (SystemFunc.cpp:2112-2115 and :1044-1051). That call is the SAME one the Last
// Point uses to turn a stored HEIGHT into a navmesh CELL - `zone\NavNear`, the
// game's own `Get3DPointToPosLocaly`. Measured on fld_auch with the game's own
// path engine, the difference is not academic:
//
//	valid 2240 -590 7            -> refused      (7 is NOT the cell at that spot)
//	near  2240 -590 7 1000 300   -> cell 108     (7 IS a height hint there)
//
// So the script's `7` is a height, the mesh answers the cell, and a caller that
// believes the brief's "the 6th argument is a CELL" would refuse all five of the
// Scarecrow's calls. The world side does the conversion (zone\create.go); this
// file only carries the numbers through unchanged.
//
// The parser is STRICT on purpose, and refuses with a line naming the argument
// rather than spawning something at (0, 0, 0): a silent zero is what a script's
// own typo looks like, and it is invisible in game.

import (
	"context"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// TNPCSpawn is one `CreateTNPC` call, parsed. The names are the original's.
type TNPCSpawn struct {
	// ZoneID is the zone INSTANCE id the script passed - `GetCurrentZoneInstID(self)`.
	// A world serves several instances in the original; our zone is one, so a
	// caller that knows the id checks it and a caller that does not is not refused.
	ZoneID int32

	// Name is `npcName`, e.g. 'NPC_HITDUMMY01'. It is the NPC's own name, and the
	// click entry the original runs for it is `SCR_CLICK_<Name>`.
	Name string

	// TypeName is `npcTypeName`, e.g. 'npc_tbk_contbox_off'. It is an NPC TABLE
	// ClassName: the original resolves it through `geNPCTable::Get` for the body
	// and the shape. Its ClassID is what the spawn packet carries.
	TypeName string

	// X, Y, Z are the original's `float x, y, z`. Z is a HEIGHT hint, NOT a cell -
	// see the file comment.
	X, Y, Z float64

	// Direction is a compass angle in degrees (270 = west).
	Direction float64

	// Occupation is the original's `imc::CStringID Occupation` - a token the trigger
	// object stores as a property. The Scarecrow passes "<$>10089936</>".
	Occupation string

	// Possession is the optional 9th argument (`BYTE Possession`, default 0).
	Possession int
}

// MonsterSpawn is one `CreateTmonster` call, parsed.
type MonsterSpawn struct {
	ZoneID int32
	// MonsterID is the `Monster` table's ClassID, e.g. 11600 "Monster_Hersu01".
	MonsterID int32
	// X, Y, Z are the original's floats; Z is a HEIGHT hint (see the file comment).
	X, Y, Z float64
	// Count is how many the call asks for. The original makes ALL of them on the
	// same spot (SystemFunc.cpp:1046-1051 loops and reuses `point`); the zone
	// makes ONE and says so when the count is larger.
	Count int
	// Direction is a compass angle in degrees.
	Direction float64
	// Level and MHP are the original's `lv` and `mhpFix` - the row's own values are
	// replaced when they are non-zero (World.cpp:832-842).
	Level float64
	MHP   float64
	// Tactics is the AI/tactics name, e.g. 'MON_MISSION_84'. No AI is built here;
	// it is carried so the monster is described by what the script asked for.
	Tactics string
}

// Spawner is what these builtins need from the world. It is an interface, not an
// import, for the same reason Handler.Click is a function field: the script
// package must not depend on the zone (main wires the two together), and the
// argument parsing must be testable with no world at all.
//
// A refusal is an error that NAMES the argument or the table it could not
// resolve. The caller logs it and creates nothing.
type Spawner interface {
	CreateTNPC(TNPCSpawn) (uint32, error)
	CreateTmonster(MonsterSpawn) (uint32, error)
}

// installCreate registers the two names. They are set, not left to the
// `__on_missing` stub: the stub answers 0, which for `CreateTmonster` is not
// `nil`, so the script's `if(Dummy1 ~= nil)` passes and its next line indexes a
// number - a Lua error in the middle of the Scarecrow dialog.
func (h *Host) installCreate() {
	h.L.SetGlobal("CreateTNPC", h.L.NewFunction(h.createTNPC))
	h.L.SetGlobal("CreateTmonster", h.L.NewFunction(h.createTmonster))
	h.L.SetGlobal("ShowScarecrowCreater", h.L.NewFunction(h.showScarecrowCreater))
}

// showScarecrowCreater stands in for the Scarecrow's configuration WINDOW.
//
// What the original does (`PlayerCharFunc.cpp:5374`): it sends
// `ZC_SHOW_SCARECROW_CREATER` - a bare command, no payload - which opens the
// client's own "scarecrow_creater" frame, and then SUSPENDS the script until the
// player presses its button; the resumed call returns an object carrying the HP,
// AR, DEF, immunity, elemental resistance, Armor and Species the player typed.
//
// THIS LANE DOES NOT BUILD THAT WINDOW, and it must not fake it: the packet's
// id is not in the zone's own table and its answer packet was never recorded, so
// an invented one would be a guess on the wire. What it does instead is answer
// with the object the script needs and SAY SO in one line, so the Scarecrow is
// created and the missing window is visible rather than silent:
//
//   - `HP` is the player's own maximum HP, which is the number the game's damage
//     meter is calibrated against (`mhp = stat1.HP * 100 - 100`).
//   - `Armor` and `Species` are 1: the script indexes its own table with them
//     (`HitDummyID1[stat1.Armor][stat1.Species]`) and Lua counts from 1, so 1/1
//     is 11600 - the first Scarecrow of the fifteen the script lists.
//   - AR, DEF, Immune, ElemReg are 0: "no adjustment", which is what an untouched
//     window would leave them at.
//
// A player therefore clicks the Scarecrow Creator and gets a Scarecrow, but
// cannot tune it yet. That is the whole of the promise this lane makes.
func (h *Host) showScarecrowCreater(L *lua.LState) int {
	hp := 0.0
	if t, ok := L.Get(1).(*lua.LTable); ok {
		if n, ok := t.RawGetString("HP").(lua.LNumber); ok {
			hp = float64(n)
		}
	}
	h.logf("script: ShowScarecrowCreater is a STAND-IN - the client's window is "+
		"not built (packet not recorded), so the dummy is created with the player's "+
		"own HP %g, Armor 1, Species 1 (monster %d) and no stat adjustments",
		hp, 11600)

	t := L.NewTable()
	t.RawSetString("HP", lua.LNumber(hp))
	t.RawSetString("Armor", lua.LNumber(1))
	t.RawSetString("Species", lua.LNumber(1))
	t.RawSetString("AR", lua.LNumber(0))
	t.RawSetString("DEF", lua.LNumber(0))
	t.RawSetString("Immune", lua.LNumber(0))
	t.RawSetString("ElemReg", lua.LNumber(0))
	propertyFallback(L, t)
	L.Push(t)
	return 1
}

// createTNPC is `CreateTNPC(zoneInstID, npcName, npcTypeName, x, y, z, direction,
// Occupation[, Possession])`. The original returns nothing, and so does this.
func (h *Host) createTNPC(L *lua.LState) int {
	spec, err := parseTNPC(L)
	if err == nil && h.Spawner == nil {
		err = errNoWorld
	}
	if err != nil {
		h.logf("script: CreateTNPC refused: %v - nothing was created", err)
		return 0
	}
	handle, werr := h.Spawner.CreateTNPC(spec)
	if werr != nil {
		h.logf("script: CreateTNPC refused: %v - nothing was created", werr)
		return 0
	}
	h.logf("script: CreateTNPC(%q, %q) at (%g, %g, z %g) dir %g -> handle %d",
		spec.Name, spec.TypeName, spec.X, spec.Y, spec.Z, spec.Direction, handle)
	return 0
}

// createTmonster is `CreateTmonster(zoneInstID, id, x, y, z, count, direction, lv,
// mhpFix, tactics[, startID])`.
//
// It answers with an OBJECT on success and NIL on a refusal, because the
// original answers `PushObject(ret->GetIESObject())` / `PushNull()` and the
// corpus depends on both halves: `if(Dummy1 ~= nil)` guards the property writes
// that follow. The object is a plain table, so `Dummy1.DR_BM = ...` lands in it
// and the click finishes instead of raising.
func (h *Host) createTmonster(L *lua.LState) int {
	spec, err := parseTmonster(L)
	if err == nil && h.Spawner == nil {
		err = errNoWorld
	}
	if err != nil {
		h.logf("script: CreateTmonster refused: %v - nothing was created", err)
		L.Push(lua.LNil)
		return 1
	}
	handle, werr := h.Spawner.CreateTmonster(spec)
	if werr != nil {
		h.logf("script: CreateTmonster refused: %v - nothing was created", werr)
		L.Push(lua.LNil)
		return 1
	}
	h.logf("script: CreateTmonster(monster %d) at (%g, %g, z %g) count %d dir %g "+
		"lv %g mhp %g tactics %q -> handle %d",
		spec.MonsterID, spec.X, spec.Y, spec.Z, spec.Count, spec.Direction,
		spec.Level, spec.MHP, spec.Tactics, handle)

	obj := L.NewTable()
	obj.RawSetString("Handle", lua.LNumber(handle))
	obj.RawSetString("ClassID", lua.LNumber(spec.MonsterID))
	obj.RawSetString("X", lua.LNumber(spec.X))
	obj.RawSetString("Y", lua.LNumber(spec.Y))
	L.Push(obj)
	return 1
}

// errNoWorld is the refusal a zone started without a world hook gets. It is a
// real state (a unit test, or a zone with scripts but no zone handler), and it
// must read as one rather than as a bad argument.
var errNoWorld = fmt.Errorf("no world is wired to this script host")

// The argument lists' names, in the original's order, so a refusal can quote the
// one that was wrong instead of a position.
var (
	tnpcArgs = [9]string{"zoneInstID", "npcName", "npcTypeName", "x", "y", "z",
		"direction", "Occupation", "Possession"}
	tmonsterArgs = [11]string{"zoneInstID", "id", "x", "y", "z", "count",
		"direction", "lv", "mhpFix", "tactics", "startID"}
)

// parseTNPC reads the 8 required arguments and the optional Possession.
//
// The original accepts 8 or more (`GetNumFunctionArgs() >= 9` gates the 9th) and
// reads the first eight unconditionally, so a call with fewer is a script bug
// that would have read whatever the stack held. Here it is REFUSED by name.
func parseTNPC(L *lua.LState) (TNPCSpawn, error) {
	var s TNPCSpawn
	if n := L.GetTop(); n < 8 || n > 9 {
		return s, fmt.Errorf("it takes 8 or 9 arguments (zoneInstID, npcName, "+
			"npcTypeName, x, y, z, direction, Occupation[, Possession]) but got %d", n)
	}
	zone, err := numArg(L, 1, tnpcArgs[0])
	if err != nil {
		return s, err
	}
	if s.Name, err = strArg(L, 2, tnpcArgs[1]); err != nil {
		return s, err
	}
	if s.TypeName, err = strArg(L, 3, tnpcArgs[2]); err != nil {
		return s, err
	}
	if s.X, err = numArg(L, 4, tnpcArgs[3]); err != nil {
		return s, err
	}
	if s.Y, err = numArg(L, 5, tnpcArgs[4]); err != nil {
		return s, err
	}
	if s.Z, err = numArg(L, 6, tnpcArgs[5]); err != nil {
		return s, err
	}
	if s.Direction, err = numArg(L, 7, tnpcArgs[6]); err != nil {
		return s, err
	}
	if s.Occupation, err = strArg(L, 8, tnpcArgs[7]); err != nil {
		return s, err
	}
	if L.GetTop() >= 9 {
		poss, perr := numArg(L, 9, tnpcArgs[8])
		if perr != nil {
			return s, perr
		}
		s.Possession = int(poss)
	}
	s.ZoneID = int32(zone)

	// An empty name is not a number to fix - it is the name the click entry and
	// the world object are built from, so there is nothing to create without it.
	if s.Name == "" {
		return s, fmt.Errorf("argument 2 (npcName) is empty: it names the object and " +
			"its SCR_CLICK_ entry, so there is no NPC to make")
	}
	if s.TypeName == "" {
		return s, fmt.Errorf("argument 3 (npcTypeName) is empty: it names the NPC " +
			"table row that draws the body, so there is no NPC to make")
	}
	return s, nil
}

// parseTmonster reads the 10 required arguments and the optional startID.
func parseTmonster(L *lua.LState) (MonsterSpawn, error) {
	var s MonsterSpawn
	if n := L.GetTop(); n < 10 || n > 11 {
		return s, fmt.Errorf("it takes 10 or 11 arguments (zoneInstID, id, x, y, z, "+
			"count, direction, lv, mhpFix, tactics[, startID]) but got %d", n)
	}
	zone, err := numArg(L, 1, tmonsterArgs[0])
	if err != nil {
		return s, err
	}
	s.ZoneID = int32(zone)

	id, err := numArg(L, 2, tmonsterArgs[1])
	if err != nil {
		return s, err
	}
	s.MonsterID = int32(id)

	if s.X, err = numArg(L, 3, tmonsterArgs[2]); err != nil {
		return s, err
	}
	if s.Y, err = numArg(L, 4, tmonsterArgs[3]); err != nil {
		return s, err
	}
	if s.Z, err = numArg(L, 5, tmonsterArgs[4]); err != nil {
		return s, err
	}
	count, err := numArg(L, 6, tmonsterArgs[5])
	if err != nil {
		return s, err
	}
	s.Count = int(count)
	if s.Direction, err = numArg(L, 7, tmonsterArgs[6]); err != nil {
		return s, err
	}
	if s.Level, err = numArg(L, 8, tmonsterArgs[7]); err != nil {
		return s, err
	}
	if s.MHP, err = numArg(L, 9, tmonsterArgs[8]); err != nil {
		return s, err
	}
	if s.Tactics, err = strArg(L, 10, tmonsterArgs[9]); err != nil {
		return s, err
	}
	if L.GetTop() >= 11 {
		// startID is a `float` in the original and is NOT carried by this lane: no
		// start point is sent (World.cpp only sets it when non-zero). It is still
		// TYPE-CHECKED, so a script that passes a table there is caught here rather
		// than three layers down.
		if _, aerr := numArg(L, 11, tmonsterArgs[10]); aerr != nil {
			return s, aerr
		}
	}

	// The id names a row in the Monster table: 0 draws nothing and is what an
	// unresolved table index looks like.
	if s.MonsterID <= 0 {
		return s, fmt.Errorf("argument 2 (id) is %d: a monster id is the Monster "+
			"table's ClassID (11600 for the Scarecrow), never 0", s.MonsterID)
	}
	if s.Count < 1 {
		return s, fmt.Errorf("argument 6 (count) is %d: a call that asks for no "+
			"monster creates nothing, so the script's own table lookup failed", s.Count)
	}
	return s, nil
}

// numArg reads one argument as a NUMBER, or refuses it by name.
//
// A number that arrived as a string is REFUSED rather than converted: the
// original reads these with `GetFloat`, which on a string is the script's bug,
// and a silent conversion is how "the position looked right" hides a wrong call.
func numArg(L *lua.LState, i int, name string) (float64, error) {
	switch v := L.Get(i).(type) {
	case lua.LNumber:
		return float64(v), nil
	case *lua.LNilType:
		return 0, fmt.Errorf("argument %d (%s) is missing, want a number", i, name)
	default:
		return 0, fmt.Errorf("argument %d (%s) is %s, want a number",
			i, name, luaTypeName(L.Get(i)))
	}
}

// strArg reads one argument as TEXT, or refuses it by name.
func strArg(L *lua.LState, i int, name string) (string, error) {
	switch v := L.Get(i).(type) {
	case lua.LString:
		return string(v), nil
	case *lua.LNilType:
		return "", fmt.Errorf("argument %d (%s) is missing, want a name", i, name)
	default:
		return "", fmt.Errorf("argument %d (%s) is %s, want a name",
			i, name, luaTypeName(v))
	}
}

// RunTacticsState runs one actor's tactics entry: `SCR_<script>_<STATE>`.
//
// The original keeps a script THREAD per trigger object, and entering its first
// tactics state starts that thread:
//
//	CTriggerObject's state machine, source2\geServer\ZServerLib\TriggerObject.cpp:49-57
//	IMPL_STATE(TS_NONE) ON_ENTER RunTacticsStateThread("TS_NONE")
//	RunTacticsStateThread  sprintf_s(funcName,"SCR_%s_%s", m_scriptName, name)
//	                       _strupr_s(funcName)          // the corpus names are upper case
//
// `m_scriptName` is the NPC's `Script` column (NPCGen), so the entry for the
// Scarecrow's trigger - `Script = NPC_HITDUMMYMANAGER` - is
// SCR_NPC_HITDUMMYMANAGER_TS_NONE, which is the ONLY thing that calls
// `CreateTNPC` four times. Without this runner that entry never executes, and the
// builtins alone would change nothing on screen: this is the half that makes the
// dummies appear.
//
// It takes ONE argument, the object itself, exactly as the original pushes
// `GetIESObject()` onto the thread. A missing entry is an ordinary answer (most
// NPCs have no tactics script at all), so `found` says which; anything else is an
// error and is returned with the entry's name on it.
func (h *Host) RunTacticsState(entry string, self Obj, timeout time.Duration) (found bool, err error) {
	// RAW access to the globals table. `L.GetGlobal` goes through the prelude's
	// __index, which INVENTS a recording stub for any name - so a plain lookup
	// would answer "found" for every entry in the game and quietly record it as a
	// missing builtin. RawGetString asks the table itself.
	globals, ok := h.L.Get(lua.GlobalsIndex).(*lua.LTable)
	if !ok {
		return false, fmt.Errorf("script: the globals table is not a table")
	}
	fn, ok := globals.RawGetString(entry).(*lua.LFunction)
	if !ok {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	h.ticks = 0
	h.callCtx = ctx
	defer func() { h.callCtx = nil }()
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	if err := h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		selfTable(h.L, self)); err != nil {
		return true, fmt.Errorf("%s: %w", entry, err)
	}
	return true, nil
}

// luaTypeName is what a refused argument was, in words a log reader can act on.
func luaTypeName(v lua.LValue) string {
	switch v.(type) {
	case *lua.LNilType:
		return "nil"
	case lua.LBool:
		return "a boolean"
	case lua.LString:
		return "a string"
	case lua.LNumber:
		return "a number"
	case *lua.LTable:
		return "a table"
	case *lua.LFunction:
		return "a function"
	default:
		return "a " + v.Type().String()
	}
}
