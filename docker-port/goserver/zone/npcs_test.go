package zone

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestSpawnTriggerLayout pins the 109-byte fixed packet. These offsets come from
// src\zone_server.py, which measured them against the live client: cmd(2)
// handle(4) unknown(9) WORLDPOS(12) direction(1) appearance(...).
func TestSpawnTriggerLayout(t *testing.T) {
	npc := NPC{
		Handle:    0x40000000,
		X:         7566,
		Y:         1425,
		Cell:      165,
		Direction: 152,
		NPCType:   10008,
		GenID:     1,
	}
	pkt := BuildSpawnTrigger(npc)

	if len(pkt) != trSize {
		t.Fatalf("%d bytes, want %d", len(pkt), trSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCSpawnTrigger {
		t.Fatalf("command = %d, want %d", cmd, CmdZCSpawnTrigger)
	}
	if got := binary.LittleEndian.Uint32(pkt[trHandle:]); got != npc.Handle {
		t.Fatalf("handle = %#x, want %#x", got, npc.Handle)
	}
	for i, want := range []int32{npc.X, npc.Y, npc.Cell} {
		off := trPos + i*4
		if got := int32(binary.LittleEndian.Uint32(pkt[off:])); got != want {
			t.Fatalf("pos[%d] = %d, want %d", i, got, want)
		}
	}
	if pkt[trDirection] != npc.Direction {
		t.Fatalf("direction = %d, want %d", pkt[trDirection], npc.Direction)
	}
	ap := trAppearance
	if got := binary.LittleEndian.Uint32(pkt[ap+apTrType:]); got != npc.NPCType {
		t.Fatalf("npc type = %d, want %d", got, npc.NPCType)
	}
	if got := binary.LittleEndian.Uint32(pkt[ap+apTrGenID:]); got != npc.GenID {
		t.Fatalf("gen id = %d, want %d", got, npc.GenID)
	}
	// The caption must be EMPTY: the client resolves it from GenID, and sending the
	// raw locale token would draw "<$>10090776</>" under the NPC.
	for i := ap + apTrName; i < ap+apTrName+npcNameLen; i++ {
		if pkt[i] != 0 {
			t.Fatalf("caption byte %d is %#x, want 0 (the client resolves it)", i, pkt[i])
		}
	}
}

// TestLoadNPCDataReadsTheRealFile uses the file the emitter actually wrote, so a
// change in its shape shows up here rather than in a client session.
func TestLoadNPCDataReadsTheRealFile(t *testing.T) {
	dir := os.Getenv("GE_NPCS_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "data", "zone_npcs")
	}
	doc, err := LoadNPCs(filepath.Join(dir, "map10.json"))
	if err != nil {
		t.Skipf("no emitted NPC data (%v) - build it with: "+
			"cd src && python emit_zone_npcs.py 10", err)
	}
	if doc.MapID != 10 {
		t.Fatalf("map id = %d, want 10", doc.MapID)
	}
	if len(doc.NPCs) < 50 {
		t.Fatalf("%d NPC(s), expected the whole town (~119)", len(doc.NPCs))
	}
	// Every NPC must be usable: a real cell and a handle the client can click.
	for i, npc := range doc.NPCs {
		if !PlausibleCell(npc.Cell) {
			t.Fatalf("npc %d (%s): cell %d is not a triangle index", i, npc.ClassType, npc.Cell)
		}
		if npc.Handle == 0 || npc.NPCType == 0 {
			t.Fatalf("npc %d (%s): handle %d type %d - the client cannot draw it",
				i, npc.ClassType, npc.Handle, npc.NPCType)
		}
	}
	// Handles must be unique, or two NPCs collide on one click.
	seen := map[uint32]bool{}
	for _, npc := range doc.NPCs {
		if seen[npc.Handle] {
			t.Fatalf("handle %d is used twice", npc.Handle)
		}
		seen[npc.Handle] = true
	}
	t.Logf("map %d: %d NPC(s), first %q at (%d,%d,cell %d)",
		doc.MapID, len(doc.NPCs), doc.NPCs[0].ClassType,
		doc.NPCs[0].X, doc.NPCs[0].Y, doc.NPCs[0].Cell)
}

// TestLoadNPCsRejectsAHeightCell is the guard against a data file built from the
// wrong column: a height in the cell puts the NPC through the floor.
func TestLoadNPCsRejectsAHeightCell(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map99.json")
	blob := []byte(`{"map_id":99,"npc_count":1,"npcs":[` +
		`{"handle":1073741824,"x":1,"y":2,"cell":-1088,"npc_type":5,"gen_id":1}]}`)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNPCs(path); err == nil {
		t.Fatal("a negative cell was accepted as a navmesh triangle")
	}
}
