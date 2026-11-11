package zone

import "testing"

// TestANewCharacterStartsAtFullHealth is the rule a brand-new character needs: the create
// screen sends no hit points, so the row is written with HP 0 and the panel drew 0/680 -
// alive with an empty gauge (owner, 2026-11-11: "but hp/sp? im dead?"). A stored ZERO is
// the only value that is filled; a character who really was beaten down keeps their number,
// which is what the second case pins.
func TestANewCharacterStartsAtFullHealth(t *testing.T) {
	for _, tc := range []struct {
		name           string
		hp, maxHP      float64
		sp, maxSP      uint16
		wantHP, wantSP float64
	}{
		{"a character that never played", 0, 660, 0, 200, 660, 200},
		{"one that was beaten down", 12, 660, 5, 200, 12, 5},
		{"a maximum of zero is left alone", 0, 0, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Character{HP: tc.hp, MaxHP: tc.maxHP, SP: tc.sp, MaxSP: tc.maxSP}
			fillMissingGauges(c)
			if c.HP != tc.wantHP || float64(c.SP) != tc.wantSP {
				t.Fatalf("HP/SP %v/%v, want %v/%v", c.HP, c.SP, tc.wantHP, tc.wantSP)
			}
		})
	}
}
