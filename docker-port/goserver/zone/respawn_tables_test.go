package zone

// THE SHAPE JOIN, proved against a file: `DyingTime` is a column of
// `datatable_monster_shape.xml`, reached through the monster row's `Shape`, the same
// join `Hits` already used (zone/create_tables.go). These two tests are deliberately
// separate from the timing tests in respawn_test.go: one proves WHERE the number comes
// from, the others prove what the corpse does with it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadMonstersReadsDyingTimeFromTheShapeTable is the hermetic half: a two-row
// monster table and a two-row shape table in a temp folder, so the assertion does not
// depend on the client being installed.
func TestLoadMonstersReadsDyingTimeFromTheShapeTable(t *testing.T) {
	dir := t.TempDir()
	monster := `<?xml version="1.0" encoding="UTF-8"?>
<IES>
	<Class ClassID="5001" ClassName="Monster_Shaped" Lv="10" Shape="Monster_Shaped" />
	<Class ClassID="5002" ClassName="Monster_NoShape" Lv="10" />
	<Class ClassID="5003" ClassName="Monster_MisspeltShape" Lv="10" Shape="Monster_NotInTheShapeTable" />
</IES>`
	shape := `<?xml version="1.0" encoding="UTF-8"?>
<IES>
	<Class ClassID="5001" ClassName="Monster_Shaped" Hits="3" DyingTime="3500" />
	<Class ClassID="5003" ClassName="Monster_AnsweredByItsOwnID" Hits="2" DyingTime="300" />
</IES>`
	if err := os.WriteFile(filepath.Join(dir, "datatable_monster.xml"), []byte(monster), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, monsterShapeFile), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}

	table, err := LoadMonsters(dir)
	if err != nil {
		t.Fatalf("LoadMonsters: %v", err)
	}

	// THE SHAPE NAMED BY THE ROW: both columns come off that row.
	shaped, ok := table.Row(5001)
	if !ok {
		t.Fatal("monster 5001 is not in the table")
	}
	if !shaped.ShapeKnown {
		t.Fatalf("5001 names Shape Monster_Shaped, a row of the shape table, but the join " +
			"did not resolve")
	}
	if got := shaped.DyingTime; got != 3500*time.Millisecond {
		t.Errorf("5001's DyingTime is %s, want 3500ms", got)
	}
	if shaped.Hits != 3 {
		t.Errorf("5001's Hits is %g, want 3 - the same join must still answer the swing's "+
			"divisor", shaped.Hits)
	}

	// NO SHAPE AT ALL: unknown, and the leave falls back to the floor.
	none, ok := table.Row(5002)
	if !ok {
		t.Fatal("monster 5002 is not in the table")
	}
	if none.ShapeKnown || none.DyingTime != 0 {
		t.Errorf("5002 names no shape but reports known=%v DyingTime=%s, want unknown/0",
			none.ShapeKnown, none.DyingTime)
	}

	// A MISSPELT SHAPE NAME, answered by the row's own ClassID: the fallback `Hits` has
	// always had, now carrying DyingTime with it.
	byID, ok := table.Row(5003)
	if !ok {
		t.Fatal("monster 5003 is not in the table")
	}
	if !byID.ShapeKnown || byID.DyingTime != 300*time.Millisecond {
		t.Errorf("5003's id fallback reports known=%v DyingTime=%s, want known/300ms",
			byID.ShapeKnown, byID.DyingTime)
	}
	if byID.Hits != 2 {
		t.Errorf("5003's id fallback Hits is %g, want 2", byID.Hits)
	}
}

// TestTheBigfootsShapeDyingTimeIsZero is the same join against the SHIPPED files: the
// field monster the owner will watch dies with `DyingTime="0"` in its shape row, so the
// `1 s + DyingTime` branch has no death animation to wait for and its corpse takes the
// branch its death is - the reference's 2 s for a killing attack.
func TestTheBigfootsShapeDyingTimeIsZero(t *testing.T) {
	table, err := LoadMonsters(stanceDir())
	if err != nil {
		t.Skipf("no client xml folder at %s: %v", stanceDir(), err)
	}
	row, ok := table.Row(11591) // Monster_Bigfoot
	if !ok {
		t.Fatal("monster 11591 (Monster_Bigfoot) is not in the table")
	}
	if !row.ShapeKnown {
		t.Fatal("Monster_Bigfoot's Shape did not resolve against datatable_monster_shape.xml")
	}
	if row.DyingTime != 0 {
		t.Fatalf("Monster_Bigfoot's shape DyingTime is %s, want 0 (the shipped row says "+
			"DyingTime=\"0\")", row.DyingTime)
	}
	if got := corpseLeaveDelay(Monster{ShapeKnown: row.ShapeKnown, DyingTime: row.DyingTime}, true); got != CorpseLeaveAttack {
		t.Fatalf("the Bigfoot's corpse leaves %s after the kill, want the %s attack "+
			"delay (a stated 0 is not a dead-animation branch)", got, CorpseLeaveAttack)
	}
}
