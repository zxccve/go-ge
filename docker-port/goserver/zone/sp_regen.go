// SP REGENERATION: the stance's own clock.
//
// THE RULE IS THE GAME'S, and it is ONE function:
//
//	ge\script\pc\get_status_pc.scp:492  // RSP ; SP regeneration value per 3 seconds
//	ge\script\pc\get_status_pc.scp:493  func SCR_Get_RSP(self)
//
// ...which returns the amount a character's SP moves by in ONE 3-second tick, signed:
// positive to give back, negative to drain. Read it (lines 525-561) and the whole
// formula is there:
//
//	ApplyRSPType = stance.RSPType                    :525
//	if mapType == 'CITY' then ApplyRSPType = 'regenerate'   :526-528
//	regenerate: 15 + floor((Lv-1)/5) + 6*promotionLv
//	            + floor(stance.IncRSP * stance.StnLv)
//	            + ring1.RSP + ring2.RSP + rweapon.RSP + lweapon.RSP + artifact.RSP
//	            + self.RSP_BM                        :539-545
//	reduction:  -15 + SellenesMemory                  :546-551
//	darkness:   -35 + SellenesMemory                  :552-557
//	anything else: 0                                  :558-560
//
// SellenesMemory is the ARTIFACT's own RSP, and only when its ClassName is one of the
// twelve in the script's own list (:509-522, :532-537). It is NOT the gear sum again;
// the gear sum belongs to the regenerate branch alone.
//
// WHAT THE C++ ADDS (source2\geServer\ZServerLib):
//
//	PlayerChar.cpp:1170-1200  UpdateDelayChargeSP: every 3 s, only when
//	                          GetRSPType() == regenerate (or RSP_CASH_BM > 0), and
//	                          not while casting/using/hit/knockdown/down unless the
//	                          cash buff is on. GetRSPType() is the STANCE's column
//	                          (:1288-1294) - a CITY map does not change it.
//	PlayerChar.cpp:1202-1220  UpdateDelayDischargeSP: every 3 s, only when the stance
//	                          is reduction/darkness; it calls the same GetRSP() and
//	                          ADDS it. In a CITY the script has flipped the value
//	                          positive, so a draining stance in town GIVES SP BACK -
//	                          that is the CITY term's only effect the reference can see.
//	PlayerChar_FSM.cpp:609-613  both run from the ALIVE body state's ON_UPDATE.
//	PlayerChar.cpp:1046-1074  AddSP clamps at GetMSP() and 0, returns when nothing
//	                          changed, and sends PKS_ZC_UPDATE_SP to the owner only.
//
// SO THE CITY TERM DOES NOT MAKE AN UNRELATED STANCE REGENERATE. The offlinespada note
// (docs\kb\gameplay-rules-v32.md:76-79) says a CITY map restores SP; the tick's own
// gate says otherwise: a stance with no lane still has no tick. What a CITY changes is
// the SIGN of a reduction/darkness stance's value. The SCP wins (DECISIONS.md).
//
// WHAT IS NOT IN THIS PORT, AND WHY:
//
//	RSP_BM and RSP_CASH_BM  no buff-modifier / cash-shop value exists in this server,
//	                        so both read 0 exactly as every *_BM in the status self
//	                        does. The ManaRegen gate only guards RSP_BM, so with 0 it
//	                        cannot change the drain.
//	stance level            the table's own StnLv is 1 on all 661 rows, and this server
//	                        stores no trained stance level, so stance.StnLv = the row's.
//	                        ZC_PC_STANCE_LEVELUP is not implemented (GAP).
//	action-state gate       the reference pauses the charge tick while casting; this
//	                        zone has no action-state field, so the tick does not pause.
//
// THE CLOCK is 3000 ms, and it is the game's: the script's own header says "per 3
// seconds" (get_status_pc.scp:492), and the reference re-arms PC_RHP_TIME = 1000 * 3
// (PlayerChar.h:45) after each charge tick (PlayerChar.cpp:1198); the discharge path
// spells the same 3000 out (PlayerChar.cpp:1217). It is NOT a number chosen here.
package zone

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// SPRegenInterval is the game's 3-second tick. See the file comment for the two
	// places the corpus states it.
	SPRegenInterval = 3000 * time.Millisecond

	// The script's own constants, get_status_pc.scp:540-556. Named so the arithmetic
	// below cannot be edited into a different rule by accident.
	rspBase           = 15
	rspLevelStep      = 5
	rspPromotionTerm  = 6
	rspReductionDrain = 15
	rspDarknessDrain  = 35
)

// rspLane is the tick a stance row selects with its RSPType column.
type rspLane int

const (
	// rspNoLane is a row with no RSPType, an unknown RSPType, or no stance at all:
	// no tick is owed, and the caller says so once.
	rspNoLane rspLane = iota
	rspRegenerate
	rspReduction
	rspDarkness
)

// sellenesArtifacts is the script's own ArtifactList (get_status_pc.scp:509-522).
// The comparison is EXACT and case-sensitive, as `artifact.ClassName == ArtifactList[i]`
// is: an artifact outside this list contributes nothing to a drain.
var sellenesArtifacts = []string{
	"OldSellenesMemory",
	"SellenesMemory",
	"E_SellenesMemory",
	"SellenesDarkMemory",
	"SellenessRose",
	"Sellenessaegis",
	"MoonlightMastersNut",
	"MoonlightMastersBolt",
	"LunarEclipsePiercings",
	"MoonBrightNightEagleEye",
	"DarkMoonSnipersRing",
	"CrescentLocket",
}

// stanceRSPLane answers the stance row's RSPType. The second answer is the row's raw
// value, for the one log line a bad row gets.
//
// A stance id of 0 means "no stance chosen yet" (START_GAME resolves it - see
// StanceTable.PickStance), not "a row without the column", so it is no lane and no
// line.
func (h *Handler) stanceRSPLane(c Character) (rspLane, string) {
	if c.Stance == 0 {
		return rspNoLane, ""
	}
	row, ok := h.Stances.RawStance(c.Stance)
	if !ok {
		return rspNoLane, ""
	}
	raw := row["RSPType"]
	switch raw {
	case "regenerate":
		return rspRegenerate, raw
	case "reduction":
		return rspReduction, raw
	case "darkness":
		return rspDarkness, raw
	}
	return rspNoLane, raw
}

// spRSPReady says the two tables the formula cannot run without are present: the item
// properties (the gear RSP term) and the stance table. A handler without them gives no
// SP back and says why, rather than adding a formula with a term silently missing.
func (h *Handler) spRSPReady() bool {
	return h != nil && h.Status != nil && h.Status.Props != nil
}

// spRegenArmed answers whether this session owes an SP tick, and is what
// sessionClockAwake and tickIfDue count so an idle player's read keeps waking.
//
// A character with an UNKNOWN RSPType also arms it ONCE: the tick has to run to print
// the one line that says the stance regenerates nothing, and spSayOnce's key is what
// stops it waking after that.
func (h *Handler) spRegenArmed(sess *Session) bool {
	if sess == nil || !h.spRSPReady() {
		return false
	}
	for i := range sess.Team {
		c := &sess.Team[i]
		if c.HP <= 0 || c.MaxSP == 0 || c.Stance == 0 {
			continue
		}
		if lane, raw := h.stanceRSPLane(*c); lane != rspNoLane {
			return true
		} else if !h.spSaidBefore(spModeKey(c.Handle, raw)) {
			return true
		}
	}
	return false
}

// spRegenTick advances the session's SP clock and answers the 3654s it owes.
//
// IT IS ONE CLOCK FOR THE SESSION, like buffAt: every character in the team ticks
// together, and the clock is advanced only when a tick really fires - a session that
// was asleep (or in a stance with no lane) does not bank ticks. The first tick after
// the body is alive is immediate, because the reference's m_delayChargeSPTime starts
// at 0 (PlayerChar.cpp:186) and `0 > timeGetTime()` is false.
func (h *Handler) spRegenTick(sess *Session, now time.Time) [][]byte {
	if !h.spRegenArmed(sess) {
		return nil
	}
	if sess.spAt.IsZero() {
		sess.spAt = now // m_delayChargeSPTime starts at 0: the first update fires.
	} else if now.Sub(sess.spAt) < SPRegenInterval {
		return nil
	} else {
		sess.spAt = now
	}

	mapType := h.spMapType()
	var out [][]byte
	for i := range sess.Team {
		c := &sess.Team[i]
		if c.HP <= 0 || c.MaxSP == 0 {
			continue
		}
		lane, raw := h.stanceRSPLane(*c)
		if lane == rspNoLane {
			if c.Stance != 0 {
				h.spSayOnce(spModeKey(c.Handle, raw), fmt.Sprintf(
					"zone: SP REGEN char %d (%q) stance %d (%s): RSPType is %q - no "+
						"regeneration lane, so this character's SP does not move "+
						"(get_status_pc.scp:525,538-560)", c.Handle, c.Name, c.Stance,
					h.stanceName(c.Stance), raw))
			}
			continue
		}

		amount := h.rspAmount(*c, mapType)
		if amount == 0 {
			continue
		}
		old := int(c.SP)
		next := old + amount
		if next > int(c.MaxSP) {
			next = int(c.MaxSP)
		}
		if next < 0 {
			next = 0
		}
		if next == old {
			// AddSP returns before a packet when the value did not move
			// (PlayerChar.cpp:1058-1060): a full bar is silent, not a 3654 at max.
			continue
		}
		c.SP = uint16(next)
		out = append(out, BuildUpdateSP(c.Handle, uint32(next)))
	}
	return out
}

// rspAmount computes SCR_Get_RSP for one character, in full.
//
// mapType comes from datatable_map.xml through MapDropTable (the same file the drop
// rules read). An empty type is "this build has no Map row", and then the CITY branch
// is not applied - the honest answer, logged once by spMapType.
func (h *Handler) rspAmount(c Character, mapType string) int {
	row, ok := h.Stances.RawStance(c.Stance)
	if !ok {
		return 0
	}
	apply := row["RSPType"]
	if mapType == "CITY" {
		apply = "regenerate" // get_status_pc.scp:526-528
	}

	switch apply {
	case "regenerate":
		// get_status_pc.scp:540-545
		value := float64(rspBase)
		value += math.Floor(float64(int(c.Level)-1) / rspLevelStep)
		value += float64(rspPromotionTerm * int(c.Promotion))
		value += math.Floor(atof(row["IncRSP"]) * atofDefault(row["StnLv"], 1))
		value += h.gearRSP(c)
		// + self.RSP_BM: no buff-modifier value exists in this server; 0.
		if value < 0 {
			value = 0
		}
		return int(math.Floor(value))
	case "reduction":
		// get_status_pc.scp:546-551. RSP_BM is 0, so the ManaRegen gate cannot
		// change the answer; SellenesMemory is the artifact's own RSP, when it is
		// one of the script's twelve.
		return -rspReductionDrain + int(h.sellenesMemory(c))
	case "darkness":
		// get_status_pc.scp:552-557, the same shape as reduction.
		return -rspDarknessDrain + int(h.sellenesMemory(c))
	}
	return 0
}

// gearRSP is the script's own five-item sum (get_status_pc.scp:541):
//
//	ring1.RSP + ring2.RSP + rweapon.RSP + lweapon.RSP + artifact.RSP
//
// ONLY those five slots. Armour, boots, back and the rest are NOT in SCR_Get_RSP; a
// sum over every worn item would be a different, wrong rule.
func (h *Handler) gearRSP(c Character) float64 {
	if !h.spRSPReady() {
		return 0
	}
	var v float64
	for _, spot := range []int{spotRing1, spotRing2, HandSpotRight, HandSpotLeft, spotArtifact} {
		v += propNumber(h.Status.Props.Table(c.Equip[spot], int(c.Level)), "RSP")
	}
	return v
}

// sellenesMemory is the SCP's SellenesMemory: the ARTIFACT's RSP when the artifact's
// ClassName is one of the twelve (get_status_pc.scp:532-537), otherwise 0.
func (h *Handler) sellenesMemory(c Character) float64 {
	if !h.spRSPReady() {
		return 0
	}
	row := h.Status.Props.Table(c.Equip[spotArtifact], int(c.Level))
	name, _ := row["ClassName"].(string)
	for _, want := range sellenesArtifacts {
		if name == want {
			return propNumber(row, "RSP")
		}
	}
	return 0
}

// spMapType answers this zone's Map row `Type` column ("CITY", "MISSION", ...), which
// is what get_status_pc.scp:508 reads. It is the SAME datatable_map.xml the drop rules
// already load (zone\drop_tables.go), so the two cannot describe a map differently.
//
// No table or no row is NOT "not a city": the CITY term is then unknown, and one line
// says so. It answers "" and the script's branch is not applied.
func (h *Handler) spMapType() string {
	if h == nil || h.MapDrops == nil {
		h.spSayOnce("no-map-table", "zone: SP REGEN: no Map table is loaded, so the "+
			"Map Type column (get_status_pc.scp:508) is unknown - the CITY term is "+
			"not applied; the stance's own RSPType still decides the tick")
		return ""
	}
	t, ok := h.MapDrops.TypeOf(h.MapID)
	if !ok {
		h.spSayOnce(fmt.Sprintf("no-map-%d", h.MapID), fmt.Sprintf(
			"zone: SP REGEN: datatable_map.xml has no row for map %d, so its Type is "+
				"unknown - the CITY term is not applied", h.MapID))
		return ""
	}
	return strings.TrimSpace(t)
}

// spModeKey is the once-per line's key: one line per character and per row value.
func spModeKey(handle uint32, raw string) string {
	return fmt.Sprintf("mode-%d-%q", handle, raw)
}

// spSayOnce prints one line the FIRST time it is asked for that key, and never again.
// Same contract as expSayOnce: a tick loop must not bury the log.
func (h *Handler) spSayOnce(key, line string) {
	h.spMu.Lock()
	if h.spSaid == nil {
		h.spSaid = map[string]bool{}
	}
	seen := h.spSaid[key]
	h.spSaid[key] = true
	h.spMu.Unlock()
	if !seen && h.Log != nil {
		h.Log.Printf("%s", line)
	}
}

// spSaidBefore answers whether spSayOnce has already printed that key.
func (h *Handler) spSaidBefore(key string) bool {
	h.spMu.Lock()
	defer h.spMu.Unlock()
	return h.spSaid[key]
}
