package zone

import (
	"os"
	"path/filepath"
	"testing"
)

// R74: the meshless map. The Last Point stores a world HEIGHT, and the only thing that
// can answer one is a `pathtool` mesh (`Handler.NavHeight` / `NavNear`). Map 15 had no
// mesh deployed, so `pathEngine` returned nil and every save in Coimbra was refused -
// correctly, because a guessed height is worse than no write. The fix is not code, it is
// the mesh: the client ships `ge\bg\coim_basic.tok` and it is now in `navmesh\mesh\`.
//
// This test asks the engine the EXACT question the refused save asked, from the log:
//
//	zone15 23:15:47 LAST POINT not saved for commander 24 (Fighter): no path engine is
//	wired in, so the height of (2268, 16931, cell 233) is unknown
//
// THE NUMBERS THAT MATTER, and why the assertion is not a range:
//
//   - the CELL is 233. `valid` must NOT come back with 233: a height that equals its own
//     cell is R73's bug, and it is what put the owner's team under the floor.
//   - the HEIGHT is -1088, and the client's own map table says so independently:
//     `data\map_table.xml`, map 15 `coim_basic`, `WayPointX="2459" WayPointY="16773"
//     WayPointZ="-1088"` (`DefGenPosZ="-1088"` too). So the ground of Port of Coimbra is
//     around -1100, NOT the 0..17000 a town-height guess would assume. Measured across
//     the whole mesh on 2026-10-30 (a 800-unit grid over x -10000..32000, y 0..32000,
//     asked with `near`): every ground height on `coim_basic` lies in -1218..871, and
//     every one of the walk graph's own 100 spots answers `valid`.
//
// The tolerance is 50 units, not an exact match: the stored position is ~250 units from
// the waypoint, a different mesh revision may shift the floor slightly, and the two
// things this test exists to catch (0, and 233) are hundreds of units away from -1088.
const (
	map15Tool = "pathtool.exe"   // the shipped 32-bit helper
	map15Mesh = "coim_basic.tok" // map 15's walk-mesh, from the client's ge\bg
	// map15Ground is the client's own ground height for the waypoint, map_table.xml.
	map15Ground = -1088
	// map15GroundSlack is how far the mesh may disagree with the map table at that spot.
	map15GroundSlack = 50
)

// TestMap15MeshAnswersTheHeightALastPointNeeds is the R74 proof: map 15's deployed mesh
// answers the height of the position the save was refused for, twice the same, and turns
// that height back into the cell the client needs on entry (the restore side, `NavNear`).
//
// It SKIPS only when `pathtool.exe` itself is absent (a Linux zone has none), and it
// FAILS when the tool is there and the mesh is not - a missing mesh is the bug this test
// exists for, so a silent skip would be no proof at all.
func TestMap15MeshAnswersTheHeightALastPointNeeds(t *testing.T) {
	tool := filepath.Join("..", "..", "navmesh", map15Tool)
	mesh := filepath.Join("..", "..", "navmesh", "mesh", map15Mesh)

	if _, err := os.Stat(tool); err != nil {
		t.Skipf("no %s at %s: the helper is a 32-bit Windows binary, so a zone on the "+
			"Linux VPS has no path engine and this question cannot be asked here", map15Tool, tool)
	}
	if _, err := os.Stat(mesh); err != nil {
		t.Fatalf("pathtool.exe IS here, but map 15's walk-mesh is missing at %s. Deploy it "+
			"from the client's own ge\\bg\\%s - with no mesh the zone refuses every Last "+
			"Point on map 15 and its chase falls back to the walk graph", mesh, map15Mesh)
	}

	engine := &PathEngine{Tool: tool, Mesh: mesh, Radius: 50}
	t.Cleanup(func() { engine.Close() }) // the engine keeps one pathtool.exe running

	// The position from the 23:15:47 log: x, y, and the navmesh CELL the client walked on.
	pos := [3]int32{2268, 16931, 233}

	height, ok, err := engine.Height(pos)
	if err != nil {
		t.Fatalf("the mesh could not answer the height of %v: %v", pos, err)
	}
	if !ok {
		t.Fatalf("the mesh says %v is not valid ground - the save would be refused for "+
			"this reason, not for a missing engine", pos)
	}

	// The bug in numbers: R73 stored the CELL in a height column and the team stood
	// under the floor.
	if height == int32(pos[2]) {
		t.Fatalf("the height at %v came back as %d - that IS the cell (R73's bug), not a "+
			"world height", pos, height)
	}
	if height == 0 {
		t.Fatalf("the height at %v is 0 - a made-up number, not this map's ground", pos)
	}
	// The map's own ground band, from map_table.xml's WayPointZ for map 15.
	if d := height - map15Ground; d > map15GroundSlack || d < -map15GroundSlack {
		t.Fatalf("the height at %v is %d, which is %d off the map table's own ground "+
			"height %d for map 15 (data\\map_table.xml WayPointZ) - this is not Coimbra's "+
			"floor", pos, height, d, map15Ground)
	}

	// The same question twice: the engine is a separate process per call, so an
	// answer that moves between calls would make every save unstable.
	again, ok, err := engine.Height(pos)
	if err != nil || !ok {
		t.Fatalf("the second Height(%v) failed: ok=%v err=%v", pos, ok, err)
	}
	if again != height {
		t.Fatalf("Height(%v) answered %d then %d - the mesh is not stable", pos, height, again)
	}

	// The RESTORE side (`zone\last_point.go:417`): the stored height must turn back into
	// a position AND a cell, or the entry has nothing to place the team on.
	at, ground, ok, err := engine.Near([3]int32{pos[0], pos[1], height}, NearRangeXY, NearRangeZ)
	if err != nil {
		t.Fatalf("Near(%d, %d, %d): %v", pos[0], pos[1], height, err)
	}
	if !ok {
		t.Fatalf("the mesh found no ground within %d/%d of (%d, %d, %d) - the entry would "+
			"fall back to the map's spawn", NearRangeXY, NearRangeZ, pos[0], pos[1], height)
	}
	if at[0] != pos[0] || at[1] != pos[1] {
		t.Fatalf("Near answered %v, want the same x/y as %v", at, pos)
	}
	if at[2] != pos[2] {
		t.Fatalf("Near answered cell %d, want %d - the cell the client walked on", at[2], pos[2])
	}
	if ground != height {
		t.Fatalf("Near says the ground there is %d, Height says %d - they must agree", ground, height)
	}
	t.Logf("map 15: (2268, 16931, cell 233) is ground at height %d, twice; Near turns %d "+
		"back into cell %d", height, height, at[2])
}
