package zone

// THE SP REGENERATION LANE'S PROOF (card: "SP comes back on the stance's own clock").
//
// Every test runs against the SHIPPED data: the real `datatable_stance.xml` (through
// the suite's stance table), the real `datatable_map.xml` (through LoadMapDrops), and
// the real `data\item_props.json` (through the suite's status source). The one hand-
// built row is the UNKNOWN RSPType, which the shipped file does not contain - the file
// has only regenerate / reduction / darkness - and it is labelled as such where it is
// used.
//
// THE NUMBERS ARE PINNED BY HAND, from ge\script\pc\get_status_pc.scp:540:
//
//	15 + floor((100 - 1) / 5) + 6*0 + floor(1 * 1) = 15 + 19 + 0 + 1 = 35
//
// on stance 10080 (MartialArtsT: RSPType="regenerate", IncRSP="1", StnLv="1"), with no
// gear so the five-item RSP sum is 0.

import (
	"bytes"
	"encoding/binary"
	"log"
	"strings"
	"testing"
	"time"
)

// The shipped rows these tests stand on.
const (
	spRegenStance     = 10080 // MartialArtsT:        RSPType regenerate, IncRSP 1,   StnLv 1
	spReductionStance = 20140 // Levitation:         RSPType reduction,  IncRSP 0,   StnLv 1
	spDarknessStance  = 20170 // ThePowerofDarkness: RSPType darkness,   IncRSP 0,   StnLv 1
	spTestHandle      = 1792
)

// spZone is the fixture: the real stance table, the real map table, the real item
// properties, and one level-100 character with no gear in a chosen stance.
//
// mapID decides the CITY term and is ALWAYS explicit: map 10 (Reboldoeux) is a CITY,
// and a drain test written on it would prove the opposite of what it says.
func spZone(t *testing.T, mapID int) (*Handler, *player, *bytes.Buffer) {
	t.Helper()
	requireStatus(t)
	if defaultStatusForTests.Stances == nil {
		t.Skip("the stance table is not readable - a test must not pass on zeros")
	}
	maps, err := LoadMapDrops(XMLDir())
	if err != nil {
		t.Skipf("datatable_map.xml is not readable (%v) - the CITY term cannot be decided", err)
	}
	logs := &bytes.Buffer{}
	h := &Handler{
		Log:      log.New(logs, "", 0),
		Stances:  defaultStatusForTests.Stances,
		MapID:    mapID,
		MapDrops: maps,
		Status:   defaultStatusForTests,
	}
	p := newPlayer(spTestHandle, "Grace", [3]int32{0, 0, 0})
	c := &p.sess.Team[0]
	c.Level = 100
	c.Promotion = 0
	c.Stance = spRegenStance
	c.SP = 0
	c.MaxSP = 200
	return h, p, logs
}

// spDue backdates the session's SP clock so the next call is a tick.
func spDue(p *player) { p.sess.spAt = time.Now().Add(-SPRegenInterval) }

// TestARegeneratingStanceAddsTheSCPsAmountOnOneTick is the card's first pass condition:
// the amount is EXACTLY SCR_Get_RSP's 35, the tick is the game's 3 s, and the 3654
// carries the new value in the client's own layout.
func TestARegeneratingStanceAddsTheSCPsAmountOnOneTick(t *testing.T) {
	h, p, logs := spZone(t, 1) // map 1 is a MISSION, so the CITY term cannot muddy this
	c := &p.sess.Team[0]

	// THE FIRST TICK IS IMMEDIATE, as the reference's m_delayChargeSPTime = 0 is
	// (PlayerChar.cpp:186). 0 -> 35, not 0 -> 0.
	out := h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 {
		t.Fatalf("one tick produced %d packet(s), want 1:\n%s", len(out), logs.String())
	}
	if c.SP != 35 {
		t.Fatalf("SP after one tick is %d, want 35 (15 + floor((100-1)/5) + 6*0 + "+
			"floor(1*1), get_status_pc.scp:540)", c.SP)
	}
	// THE BYTES, not only the number: 3654 (0x0E46), the handle, the new SP.
	want := []byte{0x46, 0x0e, 0x00, 0x07, 0x00, 0x00, 0x23, 0x00, 0x00, 0x00}
	if !bytes.Equal(out[0], want) {
		t.Fatalf("ZC_UPDATE_SP\n got % x\nwant % x", out[0], want)
	}
	if got := binary.LittleEndian.Uint16(out[0][0:]); got != CmdZCUpdateSP {
		t.Fatalf("the packet's command is %d, want ZC_UPDATE_SP %d", got, CmdZCUpdateSP)
	}
	if CmdZCUpdateSP != 3654 {
		t.Fatalf("CmdZCUpdateSP is %d, the client's table says 3654 "+
			"(data\\cmd_table_live_z.txt:1321)", CmdZCUpdateSP)
	}

	// NOT AGAIN BEFORE 3 s: the clock is the game's, not the poll's.
	if out := h.spRegenTick(p.sess, time.Now()); len(out) != 0 {
		t.Fatalf("a second tick immediately after produced %d packet(s) - the "+
			"interval is not the game's 3 s", len(out))
	}
	if c.SP != 35 {
		t.Fatalf("SP moved inside the 3 s interval: %d", c.SP)
	}

	// AND AT 3 s IT IS OWED AGAIN.
	out = h.spRegenTick(p.sess, time.Now().Add(SPRegenInterval))
	if len(out) != 1 || c.SP != 70 {
		t.Fatalf("the 3 s tick gave %d packet(s) and SP %d, want 1 and 70", len(out), c.SP)
	}
}

// TestTheRegeneratedSPNeverPassesTheMaximum: AddSP clamps at GetMSP()
// (PlayerChar.cpp:1052-1056), and a bar already at max sends nothing (:1058-1060).
func TestTheRegeneratedSPNeverPassesTheMaximum(t *testing.T) {
	h, p, _ := spZone(t, 1)
	c := &p.sess.Team[0]

	c.SP = 190
	out := h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 {
		t.Fatalf("one tick at 190/200 produced %d packet(s), want 1", len(out))
	}
	if c.SP != 200 {
		t.Fatalf("SP is %d, want the 200 maximum (190 + 35 clamped)", c.SP)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 200 {
		t.Fatalf("the packet carries SP %d, want the clamped 200", got)
	}

	// A FULL BAR IS SILENT: the reference returns before the packet.
	spDue(p)
	if out := h.spRegenTick(p.sess, time.Now()); len(out) != 0 || c.SP != 200 {
		t.Fatalf("a tick at max SP produced %d packet(s) and SP %d, want 0 and 200",
			len(out), c.SP)
	}
}

// TestADrainingStanceTakesItsSCPAmountAndStopsAtZero is the card's second pass
// condition. reduction is -15 and darkness is -35, from get_status_pc.scp:546-557, on
// a map that is NOT a city (the CITY branch would flip both positive - proved below).
func TestADrainingStanceTakesItsSCPAmountAndStopsAtZero(t *testing.T) {
	h, p, _ := spZone(t, 1)
	c := &p.sess.Team[0]

	c.Stance = spReductionStance
	c.SP = 100
	out := h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 || c.SP != 85 {
		t.Fatalf("reduction: %d packet(s), SP %d, want 1 and 85 (-15)", len(out), c.SP)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 85 {
		t.Fatalf("reduction packet carries %d, want 85", got)
	}

	c.Stance = spDarknessStance
	c.SP = 100
	spDue(p)
	out = h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 || c.SP != 65 {
		t.Fatalf("darkness: %d packet(s), SP %d, want 1 and 65 (-35)", len(out), c.SP)
	}

	// THE FLOOR IS 0, never negative, and the packet carries the clamped 0.
	c.Stance = spReductionStance
	c.SP = 10
	spDue(p)
	out = h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 || c.SP != 0 {
		t.Fatalf("reduction at 10: %d packet(s), SP %d, want 1 and 0", len(out), c.SP)
	}
	if got := binary.LittleEndian.Uint32(out[0][6:]); got != 0 {
		t.Fatalf("the clamped packet carries %d, want 0", got)
	}
}

// TestACityFlipsADrainingStancePositive: the SCP's CITY branch
// (get_status_pc.scp:508,526-528) is real and the zone's own Map table answers it.
// Reboldoeux (map 10) is a CITY; the same Levitation row that drained 15 above GIVES
// SP there, because the discharge tick calls the same SCR_Get_RSP and adds its answer
// (PlayerChar.cpp:1202-1220). The amount is the regenerate formula with Levitation's
// own IncRSP 0: 15 + floor(99/5) = 34.
func TestACityFlipsADrainingStancePositive(t *testing.T) {
	h, p, _ := spZone(t, 10)
	c := &p.sess.Team[0]
	c.Stance = spReductionStance
	c.SP = 100

	out := h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 || c.SP != 134 {
		t.Fatalf("in a CITY, reduction: %d packet(s), SP %d, want 1 and 134 "+
			"(100 + 15 + floor(99/5))", len(out), c.SP)
	}
}

// TestAnUnknownRSPTypeRegeneratesNothingAndSaysSoOnce is the card's third pass
// condition. Two characters are used so the clock keeps ticking after the first line:
// the second one is a real regenerate stance, and the first must not move on either
// tick. The unknown row is HAND-BUILT: the shipped file has only the three values.
func TestAnUnknownRSPTypeRegeneratesNothingAndSaysSoOnce(t *testing.T) {
	h, p, logs := spZone(t, 1)
	c := &p.sess.Team[0]
	c.Stance = 999
	c.SP = 0

	// The unknown row and the companion's real row, in ONE table so the tick has
	// something legitimate to do on both passes.
	h.Stances = &StanceTable{rawStance: map[int]map[string]string{
		999:   {"RSPType": "bogus", "IncRSP": "1", "StnLv": "1"},
		10080: {"RSPType": "regenerate", "IncRSP": "1", "StnLv": "1"},
	}}
	p.sess.Team = append(p.sess.Team, Character{
		Handle: 1793, Name: "Other", Level: 100, HP: 100, SP: 0, MaxSP: 200,
		Stance: spRegenStance,
	})

	out := h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 {
		t.Fatalf("the tick produced %d packet(s), want the companion's 1: %s",
			len(out), logs.String())
	}
	if c.SP != 0 {
		t.Fatalf("an unknown RSPType moved SP to %d, want 0", c.SP)
	}

	spDue(p)
	out = h.spRegenTick(p.sess, time.Now())
	if len(out) != 1 || c.SP != 0 {
		t.Fatalf("the second tick: %d packet(s), unknown char's SP %d, want 1 and 0",
			len(out), c.SP)
	}
	if n := strings.Count(logs.String(), `RSPType is "bogus"`); n != 1 {
		t.Fatalf("the unknown RSPType was logged %d time(s), want exactly 1: %s",
			n, logs.String())
	}

	// AND A ROW WITH NO COLUMN AT ALL is the same answer, with its own one line.
	p.sess.Team[0].Stance = 998
	h.Stances.rawStance[998] = map[string]string{"IncRSP": "1", "StnLv": "1"}
	spDue(p)
	if out := h.spRegenTick(p.sess, time.Now()); len(out) != 1 {
		t.Fatalf("the no-column tick produced %d packet(s), want the companion's 1", len(out))
	}
	if p.sess.Team[0].SP != 0 {
		t.Fatalf("a row with no RSPType moved SP to %d, want 0", p.sess.Team[0].SP)
	}
	if n := strings.Count(logs.String(), `stance 998`); n != 1 {
		t.Fatalf("the row with no RSPType was logged %d time(s), want exactly 1: %s",
			n, logs.String())
	}
}

// TestTheSessionClockWakesForARegeneratingStance: without this reason an idle player's
// read blocks on the socket and the 3 s tick never runs - the buff slice's own bug,
// one lane over.
func TestTheSessionClockWakesForARegeneratingStance(t *testing.T) {
	h, p, _ := spZone(t, 1)
	// THE RING IS TURNED OFF so this test measures the SP reason and nothing else: on
	// a map whose ViewRange is set, sightArmed is true for every session and would
	// answer "awake" whatever the stance is.
	h.SightOff = true
	if !h.sessionClockAwake(p.sess) {
		t.Fatal("a session whose character stands in a regenerate stance says it has " +
			"nothing to do - its SP would never come back")
	}
	if _, ran := h.tickIfDue(p.sess); !ran {
		t.Fatal("tickIfDue refused to run for a session whose only work is SP")
	}

	// A stance with no lane is NOT a reason: an idle character in a `charge`-style
	// stance must not wake the server ten times a second. A FRESH session is used
	// because the tick above has its own state; this question is about the stance.
	bare := newPlayer(0x00BA5E, "Bare", [3]int32{0, 0, 0})
	bare.sess.Team[0].MaxSP = 200
	bare.sess.Team[0].Stance = 0
	if h.sessionClockAwake(bare.sess) {
		t.Fatal("a character with no stance keeps the clock awake")
	}
}

// TestAnUnknownMapIsNotACity: no Map row means the CITY term is UNKNOWN, not false;
// the tick must still use the stance's own value and say once that the term was not
// applied. A reduction stance on an unknown map therefore drains 15, not 34.
func TestAnUnknownMapIsNotACity(t *testing.T) {
	h, p, logs := spZone(t, 999999)
	c := &p.sess.Team[0]
	c.Stance = spReductionStance
	c.SP = 100

	if out := h.spRegenTick(p.sess, time.Now()); len(out) != 1 || c.SP != 85 {
		t.Fatalf("unknown map: %d packet(s), SP %d, want 1 and 85", len(out), c.SP)
	}
	if n := strings.Count(logs.String(), "no row for map 999999"); n != 1 {
		t.Fatalf("the unknown map was logged %d time(s), want exactly 1: %s",
			n, logs.String())
	}
}
