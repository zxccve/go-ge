package zone

import (
	"fmt"
	"strconv"
)

// Spawn is where a team enters a map: WORLDPOS{x, y, CELL}.
//
// The CELL is a navmesh TRIANGLE INDEX. It is not a height, and it is not a
// coordinate: the client reads the ground height for that triangle from the mesh.
// Sending a height there is what crashed the client inside PathEngine.DLL on the
// first walk (Application Error 2026-10-25 12:52:58: faulting module
// PathEngine.DLL, 0xc0000005), because the triangle list was indexed with -1088.
//
// The default is the map the Go zone was first written on, Reboldoeux (map 10), and it
// is a FALLBACK for the -spawn override only: the normal path is the map's own point
// from the shared spawn table (docker-port\data\zone_points.json, see mapdata.go), so
// a zone serving another map gets that map's numbers. The Python zone keeps the same
// point in config\zone_spawn.txt and falls back to MAP_SPAWN in zone_server.py.
const (
	DefaultMapID = 10
	// Reboldoeux: 5779 303 149 (config\zone_spawn.txt).
	DefaultSpawnX    = 5779
	DefaultSpawnY    = 303
	DefaultSpawnCell = 149
)

// ParseSpawn reads "x y cell".
//
// A cell that looks like a height is REJECTED here rather than sent: a startup
// failure costs one command, while the alternative is a client crash that looks
// like a client bug and costs a login to rediscover.
func ParseSpawn(s string) ([3]int32, error) {
	var pos [3]int32
	fields := splitAny(s, " ,")
	if len(fields) != 3 {
		return pos, fmt.Errorf("want \"x y cell\", got %d field(s)", len(fields))
	}
	for i, f := range fields {
		v, err := strconv.Atoi(f)
		if err != nil {
			return pos, fmt.Errorf("field %d is not a number: %q", i+1, f)
		}
		pos[i] = int32(v)
	}
	if !PlausibleCell(pos[2]) {
		return pos, badCellError(pos[2])
	}
	return pos, nil
}

// badCellError is the ONE wording for "that number is a height, not a triangle
// index". ParseSpawn and the spawn-table loader both use it, so the two cannot drift
// into saying different things about the same crash.
func badCellError(cell int32) error {
	return fmt.Errorf("cell %d is not a navmesh triangle index - that is a "+
		"HEIGHT, and sending it crashes the client inside PathEngine", cell)
}

// PlausibleCell reports whether n can be a navmesh triangle index. A map has tens
// of thousands of triangles, never a negative index and never a world coordinate.
// The bound is loose on purpose: it only has to catch a height, which is either
// negative or in the tens of thousands with a different scale.
func PlausibleCell(n int32) bool {
	return n >= 0 && n <= 1000000
}

// HeightUnset is the number the oracle leaves in `character_position.ZPOS` when it has
// no position at all - 0x7FFF, read straight out of the live table (character 1790 of
// commander 24 holds 32767, 32767, 32767).
const HeightUnset int32 = 32767

// PlausibleHeight reports whether n can be a WORLD HEIGHT in this game.
//
// It is deliberately NOT PlausibleCell: heights are signed and small (the live rows hold
// 1299, 257, 250, 253, 255, -244, -1217, -2307) while a cell is a non-negative triangle
// index. A value cannot be told apart from a cell by its RANGE alone - a cell of 255 is
// a believable height - so this only rejects what a height can never be: the oracle's
// "no position" marker and anything beyond the world's own scale.
//
// Telling a stored cell from a stored height is the MESH's job, at entry
// (Handler.continuePoint): the ground height there is a different number from the cell.
func PlausibleHeight(n int32) bool {
	return n > -20000 && n < 20000 && n != HeightUnset
}
