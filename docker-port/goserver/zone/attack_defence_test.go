// THE PLAYER'S PLAIN ATTACK CARRIES THE TARGET'S DEFENCE (card t_528e0d15).
package zone

import "testing"

// TestPlainSwingDefenceIsTheScriptsTerm pins the arithmetic the game's own script uses for
// a PLAYER attacker: `(DEF / 2) - (AR / 2)`, floored at 0 and capped at 95.
func TestPlainSwingDefenceIsTheScriptsTerm(t *testing.T) {
	cases := []struct {
		name   string
		def, ar float64
		want   float64
	}{
		{"the Scarecrow: no DEF, no AR - full damage", 0, 0, 0},
		{"DEF 100 against AR 20 - 40% off", 100, 20, 40},
		{"DEF 100 against AR 100 - nothing off", 100, 100, 0},
		{"AR beats DEF - never a negative term", 100, 300, 0},
		{"DEF 790 (a real row) against AR 0 - capped at 95", 790, 0, 95},
		{"DEF 60 against AR 10 - 25% off", 60, 10, 25},
	}
	for _, c := range cases {
		if got := plainSwingDefence(c.def, c.ar); got != c.want {
			t.Errorf("%s: plainSwingDefence(%g, %g) = %g, want %g",
				c.name, c.def, c.ar, got, c.want)
		}
	}
}

// TestAPlainAttackIsReducedByTheTargetsDefence is the damage itself: the same base the old
// code sent (`castDamage(ATK, 100, 0)`) goes through `applyDefence` with that term, which is
// the pipeline the skill path already uses. RED with the term removed: the second number
// below is the full ATK, and this fails.
func TestAPlainAttackIsReducedByTheTargetsDefence(t *testing.T) {
	const atk = 1000.0
	base := castDamage(atk, plainAttackPercent, 0)
	if got := applyDefence(base, plainSwingDefence(0, 0)); got != base {
		t.Fatalf("a DEF-0 target took %g, want the full %g (the Scarecrow must not change)", got, base)
	}
	got := applyDefence(base, plainSwingDefence(100, 20))
	if want := base * 0.6; got != want {
		t.Fatalf("a DEF-100/AR-20 target took %g, want %g (40%% off)", got, want)
	}
	if got >= base {
		t.Fatal("the defence term did not reduce the damage at all")
	}
	// THE FLOOR IS THE LAST STEP: 95% off a small hit still lands 1, never 0.
	if got := applyDefence(10, plainSwingDefence(790, 0)); got != 1 {
		t.Fatalf("a 95%% defence on a 10-damage hit dealt %g, want the floor of 1", got)
	}
	// ...and on a big hit it is the 5% that survives, not the floor.
	if got := applyDefence(base, plainSwingDefence(790, 0)); got != 50 {
		t.Fatalf("a 95%% defence on a %g hit dealt %g, want %g", base, got, base*0.05)
	}
}
