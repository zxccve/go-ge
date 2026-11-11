package barrack

import "testing"

// TestExpTableIsUsable proves the embedded table loaded and that the boundaries
// are the ones the export printed (level 1 at 0 exp, level 2 at 419).
func TestExpTableIsUsable(t *testing.T) {
	if len(expTable) < 100 {
		t.Fatalf("embedded table has %d rows - the export did not take", len(expTable))
	}
	if got := LevelOf(0); got != 1 {
		t.Fatalf("LevelOf(0) = %d, want 1", got)
	}
	if got := ExpOf(2); got != 419 {
		t.Fatalf("ExpOf(2) = %d, want 419", got)
	}
	if got := LevelOf(418); got != 1 {
		t.Fatalf("LevelOf(418) = %d, want 1 (not yet level 2)", got)
	}
	if got := LevelOf(419); got != 2 {
		t.Fatalf("LevelOf(419) = %d, want 2", got)
	}
	// nextExp for a level must be a bigger total than the level's own, or the
	// client's bar arithmetic goes negative.
	for _, lv := range []int{1, 2, 50, 99} {
		if ExpOf(lv+1) <= ExpOf(lv) {
			t.Fatalf("ExpOf(%d+1) <= ExpOf(%d): the table is not increasing", lv, lv)
		}
	}
}
