package zone

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
)

// TestPromotionInfoLayout pins ZC_PROMOTION_INFO (3093) byte by byte.
//
// WHY THIS PACKET EXISTS AT ALL: the Character Info sheet (3684) has no promotion
// field, and neither does START_INFO, so the rank word under a party card's name
// ("Master", "High Master") can only come from here. The reference sends it per
// character on zone entry (CPlayerChar::UpdatePromotionInfo, PlayerChar.cpp:1472,
// called at Commander_FSM.cpp:251). Sending nothing is what left that line BLANK
// on the owner's screen on 2026-11-04.
//
// THE SIZE IS THE LOAD-BEARING FACT: 2 + 4 + 1 + 12 + 1 + 1 + 4 = 25, which is
// also what the client's own command table says (data\cmd_table.txt:574). A field
// order that "looks right" but is two bytes out would move the rank onto the stat
// block and print garbage, so every offset is asserted, not just the length.
func TestPromotionInfoLayout(t *testing.T) {
	c := Character{Handle: 0x00000304, Name: "Amy", Promotion: 4, PromotionStep: 4, CashPromotion: 3}
	b := BuildPromotionInfo(c)

	if len(b) != PromotionInfoSize {
		t.Fatalf("ZC_PROMOTION_INFO is %d bytes, want %d (the client's own table says 25)",
			len(b), PromotionInfoSize)
	}
	if got := binary.LittleEndian.Uint16(b[0:]); got != CmdZCPromotionInfo {
		t.Errorf("command %d, want %d", got, CmdZCPromotionInfo)
	}
	if got := binary.LittleEndian.Uint32(b[2:]); got != c.Handle {
		t.Errorf("handle at +2 is %d, want %d", got, c.Handle)
	}
	if got := b[6]; got != 4 {
		t.Errorf("level at +6 is %d, want 4 - this byte is what the client turns into "+
			"\"High Master\"", got)
	}
	// The 12-byte BASE_STAT block (STR AGI DEX INT CON CHA) is the PROMOTION-added
	// stats: nothing this server grants yet, and it is written as zeros on purpose.
	for i := piStat; i < piMaxStep; i++ {
		if b[i] != 0 {
			t.Errorf("stat byte +%d is %d, want 0 (no promotion stat is granted)", i, b[i])
		}
	}
	if got := b[piMaxStep]; got != c.PromotionStep {
		t.Errorf("maxStep at +%d is %d, want %d (the RAW step)", piMaxStep, got, c.PromotionStep)
	}
	if got := b[piCashLevel]; got != c.CashPromotion {
		t.Errorf("cashLevel at +%d is %d, want %d", piCashLevel, got, c.CashPromotion)
	}
	if got := binary.LittleEndian.Uint32(b[piStatPoint:]); got != 0 {
		t.Errorf("statPoint at +%d is %d, want 0 (no unspent points)", piStatPoint, got)
	}
}

// TestStatusBurstCarriesThePromotionPerCharacter is the wiring half: the entry
// burst must send one 3093 PER TEAM MEMBER, each with its own handle and level.
// One packet for "the team" would leave two of the three party cards blank, and a
// single shared handle would print the leader's rank on everyone.
func TestStatusBurstCarriesThePromotionPerCharacter(t *testing.T) {
	h := &Handler{Stances: testStances(t), Log: log.New(io.Discard, "", 0)}
	sess := &Session{Team: []Character{
		{Handle: 455, Name: "Grace", Job: 1, Level: 131, Promotion: 4, PromotionStep: 4, CashPromotion: 3},
		{Handle: 304, Name: "Amy", Job: 1, Level: 131, Promotion: 4, PromotionStep: 4, CashPromotion: 3},
		{Handle: 460, Name: "Adelina", Job: 1, Level: 120, Promotion: 3, PromotionStep: 3, CashPromotion: 3},
	}}

	out := h.statusBurst(sess)
	if len(out) != 2*len(sess.Team) {
		t.Fatalf("the entry burst is %d packet(s), want %d - one 3684 and one 3093 "+
			"per character", len(out), 2*len(sess.Team))
	}
	for i := range sess.Team {
		pkt := out[2*i+1]
		if len(pkt) != PromotionInfoSize {
			t.Fatalf("packet %d is %d bytes, want a %d-byte 3093", 2*i+1, len(pkt),
				PromotionInfoSize)
		}
		if got := binary.LittleEndian.Uint16(pkt); got != CmdZCPromotionInfo {
			t.Fatalf("packet %d is cmd %d, want %d", 2*i+1, got, CmdZCPromotionInfo)
		}
		if got := binary.LittleEndian.Uint32(pkt[piHandle:]); got != sess.Team[i].Handle {
			t.Errorf("3093 %d describes handle %d, want %d (%q)", i, got,
				sess.Team[i].Handle, sess.Team[i].Name)
		}
		if got := pkt[piLevel]; got != sess.Team[i].Promotion {
			t.Errorf("3093 for %q carries promotion level %d, want %d",
				sess.Team[i].Name, got, sess.Team[i].Promotion)
		}
	}
	// Adelina is level 120 with step 3, so her card must read "Master" while the
	// two 131s read "High Master" - the band is what makes that difference, and it
	// is the difference the owner can SEE.
	if got := out[2*2+1][piLevel]; got != 3 {
		t.Errorf("Adelina (level 120) carries promotion level %d, want 3 (Master)", got)
	}
}
