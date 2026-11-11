package zone

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// THE START OF AN ATTACK WALK WHEN IT IS EXPRESSED AS A CELL (R1xx, 2026-11-10).
//
// THE OWNER'S REPORT, 2026-11-08: "i cant attack one of the monster." A click on a body
// 2301 units away is supposed to walk the character into the stance's ring and fire on
// arrival; instead every press near the map-1331 team spawn printed
//
//	ATTACK APPROACH OUT OF REACH: 2301.08 away, stance 20210 reach MinR 50 .. MaxR 900,
//	and the character's own position (-225, 1107, cell 29) is not valid ground to walk
//	from - nothing sent
//
// and sent nothing (32 times in 82 seconds in logs\zone_go_script.log, and 4 times the
// evening before - it predates the script work).
//
// THE THIRD NUMBER IS A CELL, NOT A HEIGHT. Every position the zone carries is a
// WORLDPOS {x, y, CELL}; a mid-walk point carries the cell of the CORNER the walk is
// aiming at (move.go walkedRouteAt:566 route[i][2], walkedToAt:513 dest[2]), not the
// triangle under the interpolated x,y. On map 1331 the spawn's cell is 29 while the
// ground there is height 25; on map 1301 the spawn's cell is 140 while the ground is
// 1385 - the same pair main.go's dummyHintVRange comment measured when a z-0 spawn was
// refused there.
//
// fixture values, measured with the game's own helper (not derived):
//
//	pathtool fld_auch.tok   near -225 1107 0 1000 20000 -> cell 29, height 25
//	pathtool auch_basic.tok near -8856 4111 0 1000 20000 -> cell 140, height 1385
//	                        near -8856 4111 140 1000 300 -> "no ground" (1245 apart)
var (
	ownerSpawn  = [3]int32{-225, 1107, 29}  // the log's own refused start, cell 29
	ownerTarget = [3]int32{2623, -665, 108} // the dummy, class 11600 at (2623,-665)
)

// mapEngine runs the game's own path helper on one map's shipped mesh, and closes it.
func mapEngine(t *testing.T, mesh string) *PathEngine {
	t.Helper()
	engine := &PathEngine{
		Tool:   filepath.Join("..", "..", "navmesh", "pathtool.exe"),
		Mesh:   filepath.Join("..", "..", "navmesh", "mesh", mesh),
		Radius: 50,
	}
	if !engine.Available() {
		t.Skipf("no path engine at %s / %s", engine.Tool, engine.Mesh)
	}
	// The engine keeps ONE pathtool.exe alive until it is closed (nav.go, navWorker).
	t.Cleanup(func() { engine.Close() })
	return engine
}

// TestACellExpressedStartResolvesWhereAThreeHundredBandCannot is pass condition (a), on
// the one map whose ground/cell gap this project MEASURED: map 1301's ground at
// (-8856, 4111) is height 1385 and its navmesh cell there is 140 - 1245 units apart.
//
// The SAME x,y is asked twice, in the two shapes a start arrives in:
//
//	(140)  the navmesh CELL, as every WORLDPOS carries it;
//	(1385) the ground HEIGHT, as a row or a hand-written flag carries it.
//
// Both must plan. The height one is the case a 300-unit vertical search cannot answer:
// the ground it must find is 1245 above the number it was handed.
func TestACellExpressedStartResolvesWhereAThreeHundredBandCannot(t *testing.T) {
	engine := mapEngine(t, "auch_basic.tok")
	npc := [3]int32{-7540, 4986, 139} // map 1301's own spawn point, its own cell

	for _, tc := range []struct {
		name string
		from [3]int32
		cell int32
	}{
		{"as the mesh's cell", [3]int32{-8856, 4111, 140}, 140},
		{"as the ground height", [3]int32{-8856, 4111, 1385}, 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, route, _, _, ok, err := engine.Plan(tc.from, npc, 100)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if !ok {
				t.Fatalf("a start %v (the ground under it is height 1385, the mesh's "+
					"cell there is 140 - 1245 apart) was refused as not ground; a "+
					"300-unit vertical band cannot answer that number, and the owner's "+
					"click dies with it", tc.from)
			}
			if len(route) < 2 {
				t.Fatalf("the plan answered no route for %v (route %v)", tc.from, route)
			}
			// PASS CONDITION (c): the route begins on the mesh, at the start's own x,y,
			// carrying the MESH's cell and not the number the caller handed in.
			if route[0][0] != tc.from[0] || route[0][1] != tc.from[1] {
				t.Errorf("the route starts at %v, want x/y of %v kept exactly",
					route[0], tc.from)
			}
			if route[0][2] != tc.cell {
				t.Errorf("the route starts on cell %d, want the mesh's cell %d at "+
					"(%d, %d)", route[0][2], tc.cell, tc.from[0], tc.from[1])
			}
			if _, valid, verr := engine.Valid(route[0]); verr != nil || !valid {
				t.Errorf("the route's first point %v is not valid ground (%v): a walk "+
					"from it would be a walk out of the mesh", route[0], verr)
			}
		})
	}
}

// TestAClampedPlanWalksTheCharacterAtTheTarget is the fix and the owner's own press: a
// click on a monster 3,354 units away, from the spawn point his log names, must send ONE
// walk - not "nothing sent".
//
// WHY THE PLAN CLAMPS HERE, measured: pathtool fld_auch.tok says the ring point (950
// units from the dummy along the line to the player) is off the walkable mesh
// (`path ... endOk=0`) and the clamp stops 460 units from the dummy
// (`clamp 2623 -665 108 1816 -163 50 -> clamp ok 2287 -456 171 hit`). The agreed clamp
// rule (owner, 2026-11-09 16:25: "we agree on the clamp") is that a clamp hit's
// destination is the TARGET'S OWN POSITION, and the click path has walked it since. The
// attack refused that answer as "no clear stand point exists ... nothing sent", which is
// the same silent press one branch further along - the newest post-fix log
// (zone_go_9001.log, 2026-11-09 19:54) has 10 of them against 250 walks.
//
// The reference's rule is the reason the walk must win: a player standing on the map can
// always attack a body out of reach - the gate is the ATTACK, not the walk
// (FieldChar.cpp:956-991 -> AS_ATTACK_CHASE, FieldChar_FSM.cpp:61-79).
func TestAClampedPlanWalksTheCharacterAtTheTarget(t *testing.T) {
	h, logs, p := attackZoneAt(t, 20210, ownerSpawn) // PossessionFire, MaxR 900
	h.Plan = mapEngine(t, "fld_auch.tok").Plan
	h.spawnedMonster[0].X, h.spawnedMonster[0].Y, h.spawnedMonster[0].Cell =
		ownerTarget[0], ownerTarget[1], ownerTarget[2]

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("the owner's press sent %d move(s), want exactly 1 - he is standing on "+
			"the map and the body is 3,354 units away, so the server walks him in and "+
			"the hit fires on arrival:\n%s", moves(out), logs.String())
	}
	move := recvs(out)[0]
	if move.cmd != CmdZCMovePC {
		t.Fatalf("the press sent cmd %d, want %d ZC_MOVE_PC - the walk is the answer",
			move.cmd, CmdZCMovePC)
	}
	if dest := destOf(move.b); dest != ownerTarget {
		t.Errorf("the walk's destination is %v, want the target's own position %v (the "+
			"clamp's agreed answer - the client walks at the body and stops at "+
			"collision distance, measured 15:56: arrived, gap -100, box opened)",
			dest, ownerTarget)
	}

	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the walk carries no pending attack, so no hit would ever fire:\n%s",
			logs.String())
	}
	// PASS CONDITION (c): the walk uses the ENGINE's route, and its first point is on
	// the mesh - the mesh is the authority for the route, not for the refusal.
	if len(w.route) < 2 {
		t.Fatalf("the walk carries no route (%v): the character would walk the chord",
			w.route)
	}
	if w.route[0][0] != ownerSpawn[0] || w.route[0][1] != ownerSpawn[1] {
		t.Errorf("the route starts at %v, want the start's own x/y %v", w.route[0],
			ownerSpawn)
	}
	// A line must NAME what the walk is, so the destination is not read as a bug.
	if line := lineWith(logs.String(), "ATTACK APPROACH"); !strings.Contains(line,
		"clamp") {
		t.Errorf("the approach line does not name the clamp that chose the "+
			"destination: %s", line)
	}
	if strings.Contains(logs.String(), "no clear stand point exists") {
		t.Errorf("the press still refused the clamp's answer:\n%s", logs.String())
	}
	t.Logf("the press sends ONE walk at the target itself (%v) over %d route point(s)",
		ownerTarget, len(w.route))
}

// TestAnOffMeshStartStillRefusesWithTheSameLine is pass condition (b): the fix must not
// turn a genuine refusal into a walk. A start 900,000 units off the map is not ground, and
// the press answers with the SAME line the owner's log carries.
func TestAnOffMeshStartStillRefusesWithTheSameLine(t *testing.T) {
	const offMesh = 900000
	h, logs, p := attackZoneAt(t, 20210, [3]int32{offMesh, offMesh, 1})
	h.Plan = mapEngine(t, "fld_auch.tok").Plan
	h.spawnedMonster[0].X, h.spawnedMonster[0].Y, h.spawnedMonster[0].Cell =
		ownerTarget[0], ownerTarget[1], ownerTarget[2]

	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 0 {
		t.Fatalf("a start %d units off the map sent %d packet(s), want none:\n%s",
			offMesh, len(out), logs.String())
	}
	line := lineWith(logs.String(), "ATTACK APPROACH OUT OF REACH")
	if !strings.Contains(line, "is not valid ground to walk from") {
		t.Fatalf("the refusal line changed: %q - a start that is genuinely off the mesh "+
			"must still refuse and say why", line)
	}
	if !strings.Contains(line, "cell 1") {
		t.Errorf("the refusal does not print the start it refused: %s", line)
	}
}

// TestAnAttackFromACellTheMeshRejectsWalksTheResolvedGround is pass condition (a) driven
// through the ATTACK CALLER, not through the planner: the tests above prove that Plan
// resolves a start; this one proves that a CLICK on it sends a walk. Nothing else in this
// package pins that, and the card's title is the click, not the planner.
//
// THE START IS CELL-EXPRESSED AND THE MESH REJECTS THAT CELL - the shape a mid-walk press
// has. `walkedRouteAt` (move.go:566) hands the press `route[i][2]`, the cell of the corner
// the body is walking TOWARD, which is not the triangle under the body's own interpolated
// x,y. Map 1301's measured point carries cell 139 (a real cell of that mesh) and the
// ground there is height 1385 - 1246 units from the number handed in:
//
//	pathtool auch_basic.tok valid -8856 4111 139           -> 0            (rejected)
//	pathtool auch_basic.tok near -8856 4111 139 1000 300   -> no ground    (a 300 band cannot answer)
//	pathtool auch_basic.tok near -8856 4111 139 1000 20000 -> cell 140, height 1385
//
// The target is map 1301's own spawn point with its own cell, so both ends are real ground.
// The clamp answers the 900-unit ring point free (`clamp -7540 4986 139 -8289 4487 50 ->
// clamp ok -8289 4488 140 free`) and `path` finds the route to it, so the walk goes to the
// RING and the hit fires on arrival - a walk, which is what the reference does
// (FieldChar.cpp:956-991 -> AS_ATTACK_CHASE), never the "not valid ground to walk from"
// line the owner's log carries.
func TestAnAttackFromACellTheMeshRejectsWalksTheResolvedGround(t *testing.T) {
	start := [3]int32{-8856, 4111, 139}  // map 1301's measured point, cell 139; ground 1385
	target := [3]int32{-7540, 4986, 139} // map 1301's own spawn point, its own cell
	engine := mapEngine(t, "auch_basic.tok")

	// THE FIXTURE'S OWN CLAIM, measured here so the test cannot go vacuous: the mesh
	// rejects the start's cell, a 300-unit band given that cell finds no ground, and the
	// wide search answers the real one.
	if _, valid, err := engine.Valid(start); err != nil || valid {
		t.Fatalf("the mesh accepts %v, so this test cannot ask about a cell the mesh "+
			"must resolve (err %v)", start, err)
	}
	if _, _, ok, err := engine.Near(start, 1000, cellRangeZ); err != nil || ok {
		t.Fatalf("a %d-unit band given cell %d answered ground (%v, err %v), so this test "+
			"cannot prove the WIDE search is what plans the press", cellRangeZ, start[2], ok, err)
	}
	at, height, ok, err := engine.Near([3]int32{start[0], start[1], 0}, 1000, 20000)
	if err != nil || !ok || at[2] != 140 || height != 1385 {
		t.Fatalf("the wide search answered %v height %d ok=%v (err %v), want cell 140 "+
			"height 1385 - the measured fixture has moved", at, height, ok, err)
	}

	h, logs, p := attackZoneAt(t, 20210, start) // PossessionFire; 1,580 away
	_, maxR := stanceReach(t, h, 20210)
	h.Plan = engine.Plan
	h.spawnedMonster[0].X, h.spawnedMonster[0].Y, h.spawnedMonster[0].Cell =
		target[0], target[1], target[2]

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("a click from %v (cell 139, the ground there is 1385 - 1246 away) sent "+
			"%d move(s), want exactly 1: the mesh resolves the start or the character "+
			"never walks:\n%s", start, moves(out), logs.String())
	}
	if strings.Contains(logs.String(), "is not valid ground to walk from") {
		t.Errorf("the press still refused a start the mesh can resolve:\n%s", logs.String())
	}
	move := recvs(out)[0]
	if move.cmd != CmdZCMovePC {
		t.Fatalf("the press sent cmd %d, want %d ZC_MOVE_PC - the walk is the answer",
			move.cmd, CmdZCMovePC)
	}
	// THE DESTINATION IS THE PLAN'S OWN ANSWER - the ring point the clamp answered, or
	// the target itself when that ring has no route. Either way it must be GROUND and
	// inside the stance's own reach: the point of the card is that the walk happens.
	dest := destOf(move.b)
	if _, valid, err := engine.Valid(dest); err != nil || !valid {
		t.Errorf("the walk's destination %v is not valid ground (%v): a walk to it "+
			"would be a walk out of the mesh", dest, err)
	}
	if d := math.Hypot(float64(dest[0]-target[0]), float64(dest[1]-target[1])); d > maxR+1 {
		t.Errorf("the walk's destination %v is %g from target %v, past the %g reach - "+
			"the walk is supposed to end on the ring", dest, d, target, maxR)
	}

	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the walk carries no pending attack, so no hit would ever fire:\n%s",
			logs.String())
	}
	// PASS CONDITION (c): the route is the mesh's, and its first point is on it - with
	// the MESH's cell (140), not the cell the caller handed in (139).
	if len(w.route) < 2 {
		t.Fatalf("the walk carries no route (%v): the character would walk the chord",
			w.route)
	}
	if w.route[0][0] != start[0] || w.route[0][1] != start[1] {
		t.Errorf("the route starts at %v, want the start's own x/y %v", w.route[0], start)
	}
	if w.route[0][2] != 140 {
		t.Errorf("the route starts on cell %d, want the mesh's cell 140 at (%d, %d) - "+
			"the number the caller handed in (139) is not this triangle",
			w.route[0][2], start[0], start[1])
	}
	if _, valid, err := engine.Valid(w.route[0]); err != nil || !valid {
		t.Errorf("the route's first point %v is not valid ground (%v): a walk from it "+
			"would be a walk out of the mesh", w.route[0], err)
	}
	if last := w.route[len(w.route)-1]; last != dest {
		t.Errorf("the route ends at %v, want the walk's own goal %v", last, dest)
	}
	t.Logf("the click walks: ONE %d, the route starts on the mesh's cell 140 (the caller "+
		"said 139) and ends on the plan's answer %v, %d point(s)",
		move.cmd, dest, len(w.route))
}
