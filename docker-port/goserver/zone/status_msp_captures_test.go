package zone

import (
	"testing"
)

// TestStatusMSPReproducesTheLiveCaptures settles the ONE open question the sheet
// carried (history\script-engine.md, "Still open - named here, deliberately not
// fixed", item 1): the script computed max SP 695 while the packet sent a stored
// 995, and with a 3684 carrying no promotion level there was no way to tell "the
// live server computes it" from "the live server stores it".
//
// THE TWO NUMBERS ARE NOT OURS. They are what the live server's own sheet read:
// level 100 -> 695 and level 150 -> 995. `SCR_Get_MSP` (get_status_pc.scp:442) is
//
//	value = 195 + self.Lv*5 + promotionLv*10      (then gear multipliers)
//
// so 695 = 195 + 5x100 at promotion 0, and 995 = 195 + 5x150 + 5x10 at promotion
// 5 - which is a level the promotion band allows (ge_tables.promotion_band is 6 at
// level 150). BOTH captures are the formula, to the point, with no gear.
//
// The promotion level is real input now, not a zero: PGStore.loadPromotion reads
// `character_property` (character 304 "Amy" carries PromotionStep 4) and bands it
// with barrack.PromotionBand. That is why this test can prove the formula at TWO
// promotion levels on data this server owns - and it is why the third case below
// must differ: a promotion term that does not move the answer is not wired.
func TestStatusMSPReproducesTheLiveCaptures(t *testing.T) {
	requireStatus(t)

	for _, tc := range []struct {
		why   string
		lv    uint8
		promo uint8
		want  float64
	}{
		{"the level-100 live capture (no promotion)", 100, 0, 695},
		{"the level-150 live capture (promotion 5)", 150, 5, 995},
		// The promotion term has to MOVE the answer, or the binding is a stub
		// again: 150 at promotion 0 is the same character, unpromoted.
		{"level 150 with no promotion - the term must show", 150, 0, 945},
	} {
		c := Character{Handle: 1, Name: "MSPFixture", Job: 1,
			Level: tc.lv, Promotion: tc.promo}
		sheet, ok := defaultStatusForTests.Sheet(c)
		if !ok {
			t.Fatalf("%s: the status script did not answer (a field raised - see the log)", tc.why)
		}
		if sheet.MSP != tc.want {
			t.Errorf("%s: MSP = %.0f, want %.0f (level %d, promotionLv %d)",
				tc.why, sheet.MSP, tc.want, tc.lv, tc.promo)
		}
	}
}
