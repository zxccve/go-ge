package script

import (
	"fmt"
	"strings"
	"testing"
)

// THE ARGUMENT LISTS, AS THE GAME'S OWN SCRIPT CALLS THEM.
//
// The five calls below are copied character for character out of the corpus the
// zone loads: `out\lua_corpus\npc\evenpc\auch\npc_hitdummy_creater.lua`. They are
// the reason this file exists - the owner walked Thueringen Lakeside, found no
// Scarecrow, and the cause was that both names answered 0 from the missing-builtin
// stub.
//
// Every test here uses a FAKE spawner: no zone, no mesh, no client. The question
// is only "did the right arguments arrive, and was a bad call refused".

// callLog records what the builtins asked the world for.
type callLog struct {
	tnpc    []TNPCSpawn
	tmon    []MonsterSpawn
	handles uint32
	err     error
}

func (c *callLog) CreateTNPC(s TNPCSpawn) (uint32, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.tnpc = append(c.tnpc, s)
	c.handles++
	return 0x40000000 + c.handles, nil
}

func (c *callLog) CreateTmonster(s MonsterSpawn) (uint32, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.tmon = append(c.tmon, s)
	c.handles++
	return 0x20000002 + c.handles, nil
}

// newCreateHost is a runtime with a fake world and a log that keeps its lines, so
// a refusal can be checked for the argument it names. (newTestHost in click_test.go
// is the corpus-less runtime the dialog tests use and has a different signature.)
func newCreateHost(t *testing.T, world Spawner) (*Host, *[]string) {
	t.Helper()
	h := New()
	h.TickBudget = DefaultTickBudget
	var lines []string
	h.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	h.Spawner = world
	t.Cleanup(h.Close)
	return h, &lines
}

// TestCreateTNPCTheRealCalls runs the four calls the Scarecrow Creator makes and
// checks every argument landed where the shipped server's binding puts it
// (SystemFunc.cpp:2068: zoneInstID, npcName, npcTypeName, x, y, z, direction,
// Occupation, Possession).
func TestCreateTNPCTheRealCalls(t *testing.T) {
	world := &callLog{}
	h, lines := newCreateHost(t, world)

	// The script calls GetCurrentZoneInstID(self) first; any number stands in for
	// it here, and 7 is what a single-instance world answers.
	const zone = 7
	script := `
		CreateTNPC(` + "7" + `, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 2240, -590, 7, 270, "<$>10089936</>", 1)
		CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 3896, -749, 17, 270, "<$>10089936</>", 1)
		CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 5936, -823, 19, 270, "<$>10089936</>", 1)
		CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 7204, -1248, 14, 230, "<$>10089936</>", 1)
	`
	if err := h.L.DoString(script); err != nil {
		t.Fatalf("the four CreateTNPC calls raised: %v", err)
	}
	if len(world.tnpc) != 4 {
		t.Fatalf("%d NPC(s) asked for, want 4 - the script makes four dummies", len(world.tnpc))
	}

	// The spots, in the script's own order. The sixth number is a HEIGHT HINT
	// (7, 17, 19, 14) and travels unchanged: the MESH turns it into a cell.
	want := []struct{ x, y, z, dir float64 }{
		{2240, -590, 7, 270},
		{3896, -749, 17, 270},
		{5936, -823, 19, 270},
		{7204, -1248, 14, 230},
	}
	for i, w := range want {
		got := world.tnpc[i]
		if got.X != w.x || got.Y != w.y || got.Z != w.z {
			t.Errorf("call %d: (%g, %g, z %g), want (%g, %g, z %g)",
				i+1, got.X, got.Y, got.Z, w.x, w.y, w.z)
		}
		if got.Direction != w.dir {
			t.Errorf("call %d: direction %g, want %g", i+1, got.Direction, w.dir)
		}
		if got.Name != "NPC_HITDUMMY01" {
			t.Errorf("call %d: npcName %q, want NPC_HITDUMMY01", i+1, got.Name)
		}
		if got.TypeName != "npc_tbk_contbox_off" {
			t.Errorf("call %d: npcTypeName %q, want npc_tbk_contbox_off", i+1, got.TypeName)
		}
		if got.Occupation != "<$>10089936</>" {
			t.Errorf("call %d: Occupation %q, want the token the script passes",
				i+1, got.Occupation)
		}
		if got.Possession != 1 {
			t.Errorf("call %d: Possession %d, want 1", i+1, got.Possession)
		}
		if got.ZoneID != zone {
			t.Errorf("call %d: zoneInstID %d, want %d", i+1, got.ZoneID, zone)
		}
	}

	// Every creation gets ONE line, and none of them is a refusal.
	for _, ln := range *lines {
		if strings.Contains(ln, "refused") {
			t.Fatalf("a real call was refused: %s", ln)
		}
	}
	if len(*lines) != 4 {
		t.Fatalf("%d log line(s) for 4 creations, want one each: %q", len(*lines), *lines)
	}
}

// TestCreateTmonsterTheRealCall is the Scarecrow itself:
//
//	CreateTmonster(curZoneID, HitDummyID1[stat1.Armor][stat1.Species], 2623, -665,
//	               8, 1, 270, 130, mhp, 'MON_MISSION_84')
//
// with `mhp = stat1.HP * 100 - 100` - the game's own damage meter. HitDummyID1[1][1]
// is 11600, which is the id a real click resolves to.
func TestCreateTmonsterTheRealCall(t *testing.T) {
	world := &callLog{}
	h, lines := newCreateHost(t, world)

	script := `
		local HitDummyID1 = { {11600, 11601, 11602, 11603, 11604},
		                      {11605, 11606, 11607, 11608, 11609},
		                      {11610, 11611, 11612, 11613, 11614} }
		local stat1 = { HP = 5000, Armor = 1, Species = 1 }
		local mhp = stat1.HP * 100 - 100
		local Dummy1 = CreateTmonster(7, HitDummyID1[stat1.Armor][stat1.Species],
			2623, -665, 8, 1, 270, 130, mhp, 'MON_MISSION_84')
		assert(Dummy1 ~= nil, "a call that worked must answer an object")
		Dummy1.DR_BM = -(65 - 44)
		Dummy1.DEF_BM = 175
		UpdateStates(Dummy1)
		ANSWERED_ID = Dummy1.ClassID
	`
	if err := h.L.DoString(script); err != nil {
		t.Fatalf("the Scarecrow call raised: %v", err)
	}
	if len(world.tmon) != 1 {
		t.Fatalf("%d monster(s) asked for, want 1", len(world.tmon))
	}
	got := world.tmon[0]
	if got.MonsterID != 11600 {
		t.Errorf("id %d, want 11600 (HitDummyID1[1][1])", got.MonsterID)
	}
	if got.X != 2623 || got.Y != -665 || got.Z != 8 {
		t.Errorf("at (%g, %g, z %g), want (2623, -665, z 8)", got.X, got.Y, got.Z)
	}
	if got.Count != 1 {
		t.Errorf("count %d, want 1", got.Count)
	}
	if got.Direction != 270 {
		t.Errorf("direction %g, want 270", got.Direction)
	}
	if got.Level != 130 {
		t.Errorf("lv %g, want 130", got.Level)
	}
	if got.MHP != 5000*100-100 {
		t.Errorf("mhp %g, want 499900 (the player's HP x 100 - 100)", got.MHP)
	}
	if got.Tactics != "MON_MISSION_84" {
		t.Errorf("tactics %q, want MON_MISSION_84", got.Tactics)
	}
	// The result is an OBJECT, not nil and not a number: the script sets fields on
	// it straight away (`Dummy1.DR_BM = ...`), and indexing a number is a Lua error
	// that would abort the click after the monster was already made.
	if id, ok := h.L.GetGlobal("ANSWERED_ID").(interface{ String() string }); !ok ||
		id.String() != "11600" {
		t.Errorf("the answered object does not carry ClassID 11600, it is %v", id)
	}
	for _, ln := range *lines {
		if strings.Contains(ln, "refused") {
			t.Fatalf("a real call was refused: %s", ln)
		}
	}
}

// TestCreateRefusesAWrongCount is the first half of the "loud failure" rule: a
// call with the wrong number of arguments is refused, refused BY NAME, and creates
// NOTHING.
func TestCreateRefusesAWrongCount(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string // the words the log line must carry
	}{
		{
			name:   "CreateTNPC with seven arguments",
			script: `CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 2240, -590, 7, 270)`,
			want:   "8 or 9 arguments",
		},
		{
			name: "CreateTNPC with ten",
			script: `CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 2240, -590,
				7, 270, "<$>10089936</>", 1, 2)`,
			want: "8 or 9 arguments",
		},
		{
			name:   "CreateTmonster with nine",
			script: `CreateTmonster(7, 11600, 2623, -665, 8, 1, 270, 130, 499900)`,
			want:   "10 or 11 arguments",
		},
		{
			name: "CreateTmonster with twelve",
			script: `CreateTmonster(7, 11600, 2623, -665, 8, 1, 270, 130, 499900,
				'MON_MISSION_84', 0, 0)`,
			want: "10 or 11 arguments",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			world := &callLog{}
			h, lines := newCreateHost(t, world)
			if err := h.L.DoString(c.script); err != nil {
				t.Fatalf("a refused call must not raise inside Lua: %v", err)
			}
			if len(world.tnpc)+len(world.tmon) != 0 {
				t.Fatalf("a refused call still asked the world for %d object(s)",
					len(world.tnpc)+len(world.tmon))
			}
			if len(*lines) != 1 {
				t.Fatalf("%d log line(s), want exactly one: %q", len(*lines), *lines)
			}
			if !strings.Contains((*lines)[0], "refused") {
				t.Fatalf("the line does not say it was refused: %q", (*lines)[0])
			}
			if !strings.Contains((*lines)[0], c.want) {
				t.Fatalf("the line does not say %q: %q", c.want, (*lines)[0])
			}
		})
	}
}

// TestCreateRefusesAWrongType is the second half: a number where a name belongs
// (and the reverse) is refused by ARGUMENT NAME, and nothing is created. A silent
// conversion here is how a wrong call produces an actor at a plausible-looking
// spot.
func TestCreateRefusesAWrongType(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "CreateTNPC npcName as a number",
			script: `CreateTNPC(7, 11600, 'npc_tbk_contbox_off', 2240, -590, 7, 270,
				"<$>10089936</>", 1)`,
			want: "argument 2 (npcName) is a number, want a name",
		},
		{
			name: "CreateTNPC npcTypeName as a table",
			script: `CreateTNPC(7, 'NPC_HITDUMMY01', {}, 2240, -590, 7, 270,
				"<$>10089936</>", 1)`,
			want: "argument 3 (npcTypeName) is a table, want a name",
		},
		{
			name: "CreateTNPC x as a string",
			script: `CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', "2240",
				-590, 7, 270, "<$>10089936</>", 1)`,
			want: "argument 4 (x) is a string, want a number",
		},
		{
			name: "CreateTNPC Possession as a table",
			script: `CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off', 2240, -590,
				7, 270, "<$>10089936</>", {})`,
			want: "argument 9 (Possession) is a table, want a number",
		},
		{
			name: "CreateTmonster id as a string",
			script: `CreateTmonster(7, "11600", 2623, -665, 8, 1, 270, 130, 499900,
				'MON_MISSION_84')`,
			want: "argument 2 (id) is a string, want a number",
		},
		{
			name: "CreateTmonster z as nil",
			script: `CreateTmonster(7, 11600, 2623, -665, nil, 1, 270, 130, 499900,
				'MON_MISSION_84')`,
			want: "argument 5 (z) is missing, want a number",
		},
		{
			name:   "CreateTmonster tactics as a number",
			script: `CreateTmonster(7, 11600, 2623, -665, 8, 1, 270, 130, 499900, 84)`,
			want:   "argument 10 (tactics) is a number, want a name",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			world := &callLog{}
			h, lines := newCreateHost(t, world)
			if err := h.L.DoString(c.script); err != nil {
				t.Fatalf("a refused call must not raise inside Lua: %v", err)
			}
			if len(world.tnpc)+len(world.tmon) != 0 {
				t.Fatalf("a refused call still asked the world for %d object(s)",
					len(world.tnpc)+len(world.tmon))
			}
			if len(*lines) != 1 || !strings.Contains((*lines)[0], c.want) {
				t.Fatalf("want one line carrying %q, got %q", c.want, *lines)
			}
		})
	}
}

// TestCreateRefusesAnEmptyName: the name is what the click entry and the object
// are built from, so an empty one is refused rather than creating something
// nothing can ever refer to.
func TestCreateRefusesAnEmptyName(t *testing.T) {
	world := &callLog{}
	h, lines := newCreateHost(t, world)
	if err := h.L.DoString(`CreateTNPC(7, '', 'npc_tbk_contbox_off', 2240, -590, 7,
		270, "<$>10089936</>", 1)`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if len(world.tnpc) != 0 {
		t.Fatal("an NPC with an empty name was created")
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "npcName") {
		t.Fatalf("the refusal does not name the argument: %q", *lines)
	}
}

// TestCreateRefusesAnImpossibleId: the script itself resolves the monster id from
// its own table, so a 0 there means the lookup failed (an unknown Armor/Species).
// A 0 id is refused by name rather than spawning a bodiless actor.
func TestCreateRefusesAnImpossibleId(t *testing.T) {
	world := &callLog{}
	h, lines := newCreateHost(t, world)
	if err := h.L.DoString(`local d = CreateTmonster(7, 0, 2623, -665, 8, 1, 270,
		130, 499900, 'MON_MISSION_84'); NIL_ANSWER = (d == nil)`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if len(world.tmon) != 0 {
		t.Fatal("monster id 0 was created")
	}
	if got := h.L.GetGlobal("NIL_ANSWER"); got.String() != "true" {
		t.Fatalf("a refused CreateTmonster answered %v, want nil (the script "+
			"branches on it)", got)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "argument 2 (id)") {
		t.Fatalf("the refusal does not name the argument: %q", *lines)
	}
}

// TestCreateRefusesWithNoWorldWired: a zone started with scripts but no world hook
// must not pretend. Both names refuse and say why.
func TestCreateRefusesWithNoWorldWired(t *testing.T) {
	h, lines := newCreateHost(t, nil)
	if err := h.L.DoString(`CreateTNPC(7, 'NPC_HITDUMMY01', 'npc_tbk_contbox_off',
		2240, -590, 7, 270, "<$>10089936</>", 1)`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if err := h.L.DoString(`CreateTmonster(7, 11600, 2623, -665, 8, 1, 270, 130,
		499900, 'MON_MISSION_84')`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if len(*lines) != 2 {
		t.Fatalf("%d line(s), want one per refusal: %q", len(*lines), *lines)
	}
	for _, ln := range *lines {
		if !strings.Contains(ln, "no world is wired") {
			t.Fatalf("the refusal does not say there is no world: %q", ln)
		}
	}
}

// TestCreateReportsAWorldRefusal: when the WORLD refuses (an unknown monster id,
// a cell off the mesh), the script host reports that reason and creates nothing -
// it does not answer an object as if it had worked.
func TestCreateReportsAWorldRefusal(t *testing.T) {
	world := &callLog{err: fmt.Errorf("id 99999 is not a row of the Monster table")}
	h, lines := newCreateHost(t, world)
	if err := h.L.DoString(`local d = CreateTmonster(7, 99999, 2623, -665, 8, 1, 270,
		130, 499900, 'MON_MISSION_84'); NIL_ANSWER = (d == nil)`); err != nil {
		t.Fatalf("unexpected raise: %v", err)
	}
	if got := h.L.GetGlobal("NIL_ANSWER"); got.String() != "true" {
		t.Fatalf("the world refused but the script got %v, want nil", got)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "not a row of the Monster table") {
		t.Fatalf("the world's reason is not in the log: %q", *lines)
	}
}

// TestShowScarecrowCreaterIsAStandIn pins the ONE thing this lane must not let
// drift: the Scarecrow's configuration window is NOT built, the stand-in says so,
// and the object it answers still lets the script reach CreateTmonster.
func TestShowScarecrowCreaterIsAStandIn(t *testing.T) {
	world := &callLog{}
	h, lines := newCreateHost(t, world)
	if err := h.L.DoString(`
		local pc = { HP = 4321 }
		local stat = ShowScarecrowCreater(pc)
		assert(stat ~= nil, "the stand-in must answer an object")
		local mhp = stat.HP * 100 - 100
		CreateTmonster(7, 11600, 2623, -665, 8, 1, 270, 130, mhp, 'MON_MISSION_84')
	`); err != nil {
		t.Fatalf("the stand-in broke the Scarecrow script: %v", err)
	}
	if len(world.tmon) != 1 {
		t.Fatalf("%d monster(s), want 1", len(world.tmon))
	}
	if got := world.tmon[0].MHP; got != 4321*100-100 {
		t.Errorf("mhp %g, want %d - the dummy is sized off the PLAYER", got, 4321*100-100)
	}
	if len(*lines) != 2 {
		t.Fatalf("want the stand-in's own line and the creation's, got %q", *lines)
	}
	if !strings.Contains((*lines)[0], "STAND-IN") {
		t.Fatalf("the first line must say the window is a stand-in: %q", (*lines)[0])
	}
}
