package zone

import (
	"os"
	"path/filepath"
	"testing"
)

// npcScripts builds town NPCs with only the field the hide list reads, so a test says
// what it means without a data file.
func npcScripts(scripts ...string) []NPC {
	out := make([]NPC, len(scripts))
	for i, s := range scripts {
		// A handle per NPC, numbered the way the emitter does: base + index.
		out[i] = NPC{Handle: 1073741824 + uint32(i), Script: s,
			ClassType: "TOWN", NameToken: s}
	}
	return out
}

// scriptsOf is the list's scripts, in order, for a readable assertion.
func scriptsOf(npcs []NPC) []string {
	out := make([]string, len(npcs))
	for i, n := range npcs {
		out[i] = n.Script
	}
	return out
}

// TestHideGlobalRuleHidesEverywhere: a bare SCRIPTNAME is a rule for every map.
func TestHideGlobalRuleHidesEverywhere(t *testing.T) {
	hide := ParseNPCHide("GATCHA_COSTUME_VENDER\n")

	if hide.Rules() != 1 {
		t.Fatalf("Rules = %d, want 1", hide.Rules())
	}
	for _, mapID := range []int{10, 15, 208} {
		if !hide.Hides(mapID, "GATCHA_COSTUME_VENDER") {
			t.Fatalf("the global rule did not hide on map %d", mapID)
		}
	}
	// Case and padding are not part of the name.
	if !hide.Hides(15, "  gatcha_costume_vender  ") {
		t.Fatal("matching is not case-insensitive/trimmed")
	}
}

// TestHideMapRuleHidesOneMap: map15:SCRIPTNAME hides on 15 and NOWHERE else - the same
// script exists in every town and may be wanted in the others.
func TestHideMapRuleHidesOneMap(t *testing.T) {
	hide := ParseNPCHide("map15:NPC_LUMIN_CHANGER\n")

	if hide.Rules() != 1 {
		t.Fatalf("Rules = %d, want 1", hide.Rules())
	}
	if !hide.Hides(15, "NPC_LUMIN_CHANGER") {
		t.Fatal("the map rule did not hide on its own map")
	}
	if hide.Hides(10, "NPC_LUMIN_CHANGER") || hide.Hides(208, "NPC_LUMIN_CHANGER") {
		t.Fatal("a map15 rule hid the NPC on another map")
	}
}

// TestHideCommentsAndBlanksDoNothing: the file is mostly explanation, and none of it is
// a rule - an empty list must be visibly empty (Rules() == 0), not silently broken.
func TestHideCommentsAndBlanksDoNothing(t *testing.T) {
	hide := ParseNPCHide(`# zone_npc_hide.txt - town NPCs we do NOT want spawned.
#   SCRIPTNAME          hide that script on EVERY map

   # an indented comment
`)
	if hide.Rules() != 0 {
		t.Fatalf("a comment-only file produced %d rule(s)", hide.Rules())
	}
	if len(hide.Bad) != 0 {
		t.Fatalf("a comment-only file produced %d warning(s): %v", len(hide.Bad), hide.Bad)
	}
	if hide.Hides(15, "GATCHA_COSTUME_VENDER") {
		t.Fatal("an empty list hid something")
	}
}

// TestHideUnknownScriptHidesNothing: a rule nobody matches removes no NPC. The count
// is the proof, and it must come back unchanged.
func TestHideUnknownScriptHidesNothing(t *testing.T) {
	npcs := npcScripts("SHOP_A", "SHOP_B", "NPC_LUMIN_CHANGER")
	hide := ParseNPCHide("NO_SUCH_SCRIPT\nmap9:ALSO_MISSING\n")

	kept, hidden := hide.Apply(15, npcs)
	if hidden != 0 {
		t.Fatalf("an unknown script hid %d NPC(s)", hidden)
	}
	if len(kept) != len(npcs) {
		t.Fatalf("kept %d of %d NPC(s)", len(kept), len(npcs))
	}
}

// TestHideApplyCountsAndKeepsOrder: what is left is the input minus the hidden ones,
// in the same order, with the handles untouched - a hidden NPC leaves a GAP in the
// handle numbering and the client uses the handle, not the index.
func TestHideApplyCountsAndKeepsOrder(t *testing.T) {
	npcs := npcScripts("SHOP_A", "GATCHA_COSTUME_VENDER", "SHOP_B", "NPC_LUMIN_CHANGER")
	hide := ParseNPCHide("GATCHA_COSTUME_VENDER\nmap15:NPC_LUMIN_CHANGER\n")

	kept, hidden := hide.Apply(15, npcs)
	if hidden != 2 || len(kept) != 2 {
		t.Fatalf("hidden = %d, kept = %d; want 2 and 2", hidden, len(kept))
	}
	if got := scriptsOf(kept); got[0] != "SHOP_A" || got[1] != "SHOP_B" {
		t.Fatalf("kept = %v, want the two shops in order", got)
	}
	if kept[0].Handle != npcs[0].Handle || kept[1].Handle != npcs[2].Handle {
		t.Fatalf("handles were renumbered: %v", scriptsOf(kept))
	}

	// The map-scoped rule does not apply elsewhere, so only the global one hides there.
	if kept, hidden := hide.Apply(10, npcs); hidden != 1 || len(kept) != 3 {
		t.Fatalf("on map 10: hidden = %d, kept = %d; want 1 and 3", hidden, len(kept))
	}
}

// TestHideMalformedLineIsAWarning: a line nobody can read hides nothing and is
// reported once, with its number - it must not stop the loader or the zone.
func TestHideMalformedLineIsAWarning(t *testing.T) {
	hide := ParseNPCHide("SHOP_A\nnotamap:SHOP_B\nmap:SHOP_C\nmap15:\n")

	if hide.Rules() != 1 {
		t.Fatalf("Rules = %d, want only the good line", hide.Rules())
	}
	if len(hide.Bad) != 3 {
		t.Fatalf("%d warning(s), want one per bad line: %v", len(hide.Bad), hide.Bad)
	}
	if hide.Hides(15, "SHOP_B") || hide.Hides(15, "SHOP_C") {
		t.Fatal("a malformed line hid an NPC")
	}
}

// TestHideMissingFileIsNotAnError: the file is optional by design. A nil list is also
// usable, so a caller never has to nil-check.
func TestHideMissingFileIsNotAnError(t *testing.T) {
	hide, err := LoadNPCHide(filepath.Join(t.TempDir(), "nope.txt"))
	if err != nil {
		t.Fatalf("a missing hide list is an error: %v", err)
	}
	if hide.Rules() != 0 || hide.Hides(15, "GATCHA_COSTUME_VENDER") {
		t.Fatal("a missing hide list hid something")
	}

	var none *NPCHide
	if none.Rules() != 0 || none.Hides(15, "X") {
		t.Fatal("a nil list answered a rule")
	}
	if kept, hidden := none.Apply(15, npcScripts("A", "B")); hidden != 0 || len(kept) != 2 {
		t.Fatal("a nil list dropped an NPC")
	}
}

// TestHideTheRealListOnCoimbra is the owner's own file against the owner's own map.
//
// It asserts the INVARIANT rather than a fixed number, because the owner edits this
// file while playing: every script the list names for map 15 must be gone, every other
// NPC must still be there, and the count must be exactly total-minus-hidden. On
// 2026-10-27 the file names two scripts for Coimbra (the costume box and the enchant
// barter; the lumin changer is marked KEPT ON PURPOSE below them), so the count is
// 100 -> 98.
func TestHideTheRealListOnCoimbra(t *testing.T) {
	hidePath := filepath.Join("..", "..", "config", "zone_npc_hide.txt")
	npcPath := filepath.Join("..", "..", "data", "zone_npcs", "map15.json")
	if _, err := os.Stat(hidePath); err != nil {
		t.Skipf("no hide list at %s", hidePath)
	}
	if _, err := os.Stat(npcPath); err != nil {
		t.Skipf("no NPC data at %s (run: cd src && python emit_zone_npcs.py 15)", npcPath)
	}
	hide, err := LoadNPCHide(hidePath)
	if err != nil {
		t.Fatalf("LoadNPCHide: %v", err)
	}
	if len(hide.Bad) != 0 {
		t.Fatalf("the owner's own list has %d unreadable line(s): %v", len(hide.Bad), hide.Bad)
	}
	if hide.Rules() == 0 {
		t.Fatal("the owner's list produced no rule at all")
	}
	doc, err := LoadNPCs(npcPath)
	if err != nil {
		t.Fatalf("LoadNPCs: %v", err)
	}

	kept, hidden := hide.Apply(15, doc.NPCs)
	if hidden == 0 {
		t.Fatal("the owner's list hid nothing on Coimbra")
	}
	if len(kept) != len(doc.NPCs)-hidden {
		t.Fatalf("kept %d + hidden %d != %d NPC(s)", len(kept), hidden, len(doc.NPCs))
	}
	for _, npc := range kept {
		if hide.Hides(15, npc.Script) {
			t.Fatalf("%s is still spawned although the list hides it", npc.Script)
		}
	}
	// The count must not drift for any other reason: every NPC that disappeared is one
	// the list names.
	gone := 0
	for _, npc := range doc.NPCs {
		if hide.Hides(15, npc.Script) {
			gone++
		}
	}
	if gone != hidden {
		t.Fatalf("%d NPC(s) disappeared but %d are named by the list", hidden, gone)
	}
	t.Logf("map 15: %d NPC(s) -> %d, %d hidden by %d rule(s)",
		len(doc.NPCs), len(kept), hidden, hide.Rules())
}
