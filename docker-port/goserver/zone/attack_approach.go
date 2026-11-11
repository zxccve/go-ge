package zone

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"geserver/protocol"
)

// THE PLAIN ATTACK'S REACH, AND THE WALK THAT CLOSES IT (R90).
//
// WHY THIS FILE EXISTS. Until it, `Handler.attack` answered every CZ_ATTACK 3666 at
// ANY distance - a Fighter's back-guard swing landed from across the map. The owner
// found it in game, twice: "again normal skill also need stance range check. Fighter
// melee attack is attacking from insane distance lol." and "normal attack is stance
// based. read the xml/ies." He was right, and the numbers were in the row all along:
// `datatable_stance.xml` carries MinR and MaxR on EVERY stance, next to the ASPD the
// zone already reads. A plain attack has no skill row to take a range from, so the
// stance is the only place a reach can come from.
//
// THE RULE IS THE SKILL'S RULE, because the game has one rule:
//
//	inside MinR..MaxR   the hit goes out at once (zone\attack.go)
//	farther than MaxR   the character WALKS to a stand point inside the range and the
//	                    hit goes out when it gets there (this file) - the same walk,
//	                    planner and clock the skill approach uses, so a normal attack
//	                    is not a second movement system
//	nearer than MinR    the character WALKS OUT to the MIN ring and the hit goes out
//	                    when it gets there (R101) - the far case's exact mirror: same
//	                    walk, same held press, same fire-on-arrival, one rule with two
//	                    directions (the reference's IsAttackArea -1 -> GetAttackMinPos,
//	                    FieldChar_FSM.cpp:63-79)
//	MaxR 0 or no row    NO reach check: the row states no bound, and a made-up one
//	                    would refuse a click the client itself thinks is fine.
//
// THE REACH IS EDGE TO EDGE, AND THE STAND POINT IS THE OUTER RING (R91 round 5). The
// rule above is still the game's, but two of its numbers were wrong here: the range was
// a CENTRE-to-centre distance (the reference subtracts both bodies, FieldChar.cpp:118)
// and the stand point was `maxR * 0.9` (the reference walks to `maxR + targetRadius`,
// PlayerChar.cpp:1393). Both are fixed in attackRangeAllowed and attackStandoff below,
// which stay the ONE pair of functions the press and the walk share.
//
// THE SERVER WALKS, AT ONCE (R91 round 6). Round 4 had this file WAITING for the
// client's own walk first; that rule is GONE, and the client's own AI script is why. A
// clicked enemy does NOT make the client walk its character to it:
// `SCR_ATTACKER_TS_BATTLE_NOMINATE` (attacker.scp:135-170) nominates the target and
// calls `SCR_AI_LIB_ATTACKTARGET`, and that function (lib.scp) only attacks or sleeps
// 1000 - it never moves. So nobody else was going to walk the body, and an out-of-reach
// press waited for a walk that never came.
//
// The reference walks, from the press, and re-decides the destination on a clock:
//
//	too far -> GetAttackMaxPos -> move                    FieldChar_FSM.cpp:28-80
//	at most every 300 ms (m_nextAttackChaseTime)           :38-42, checked at :57
//	and only when the destination CHANGED (IsEqualPos)      :49, :74
//
// Both halves are here: `attackChasePace` below holds the clock, and approachAttack
// keeps the walk in flight when this press would walk to the SAME point.
//
// WHAT IT DOES NOT DO, said out loud: it does not touch the damage formula (R84), the
// 3564/3557 layouts, the cadence, or the cancel/phase machinery. The swing has no
// wind-up of its own - the 3564 is sent the instant the character is in range - so a
// pending attack has ONE phase (approaching, cancellable) and nothing after it.

// pendingAttack is the client's own CZ_ATTACK, kept while its walk runs, so the hit
// that fires at the end of the walk is the SAME request that was refused a moment
// ago: same frame, same target, same handler.
type pendingAttack struct {
	frame  protocol.Frame // replayed verbatim: the handler parses it again
	target uint32         // for the log lines only; the frame carries it

	pos  [3]int32 // the monster's own position when the press was made
	dist float64  // how far the press was made from, EDGE TO EDGE (see attackRangeAllowed)
	minR float64  // the stance row's near bound, quoted in the log
	maxR float64  // and its far bound

	// myR and targetR are the two bodies the reach is measured between (R91 round 5):
	// the character's own collision radius and the monster's, kept on the press so the
	// walk and the replay at the end of it all measure the same two bodies even if a
	// stance or a row changes under them.
	myR     float64
	targetR float64

	idx   int       // the team slot that pressed: the walk is per character
	since time.Time // when the PLAYER pressed; survives a re-plan (see attackHoldTimeout)

	// stance is the stance the press was made in, so a change under the walk (a stance
	// switch, a different weapon) can DROP the press with a reason instead of leaving a
	// walk that may no longer reach what it was planned for (R100).
	stance uint32

	// stand is the stand-off this approach ASKS the path engine for, in units from the
	// target's centre - 0 means "the stance's own ring" (attackStandoff). A RE-WALK sets
	// it from the distance the walk it follows ended at (attackStandoffCloser, R99), so a
	// re-plan can never ask for the point that just proved too far.
	stand float64

	// stepBack says THIS press found the target INSIDE the stance's minimum reach, so the
	// walk it holds goes AWAY from the target, to the min ring (attackStepBackStand), and
	// `stand` carries that ring. It is the mirror of a far approach and it changes nothing
	// else: the press is held, the fire-on-arrival check fires it, the pace and the cap are
	// the far case's. The flag exists so the one log line says which direction the walk
	// goes, and so a re-plan does not aim the min ring back at the far one (R101).
	stepBack bool

	// answeredHere is true when the REPORT now being handled is what answered this press
	// by stepping it back (R102). A position report is read as a click by
	// cancelApproachAttack, which runs right after the fire-on-arrival check in movePC: a
	// click that walks the body ONTO the target must not cancel the very step-back that
	// report asked for, or the press dies one line later with nothing sent. It is cleared
	// by that one read, so the NEXT report cancels a step-back exactly as it always did.
	answeredHere bool

	// repeats is how many times THIS attack has walked again after a walk ended with
	// the target still out of reach (R98). It lives here, on the pending attack the
	// walk record carries (chase.go: walk.atk), so the count travels with the walk into
	// the next plan and dies with it - and a NEW PRESS builds a fresh pendingAttack,
	// which is exactly the reset the cap is meant to have.
	repeats int

	// repeat says this call comes from the ENGAGEMENT'S OWN TICK (swingTick's too-far
	// branch), not from a press. It is what gives that one caller the reference's
	// once-per-stand-point rule and its chase clock (R111): a press keeps exactly the
	// rules it already had (R97 absorption, R91's 300 ms clock), because a press that is
	// refused still owes the player an answer and its own log line.
	repeat bool
}

// attackChasePace is how often one character's attack walk may be re-decided: the
// reference's own m_nextAttackChaseTime (FieldChar_FSM.cpp:38-42, checked at :57).
const attackChasePace = 300 * time.Millisecond

// attackAbsorbPace is how often a SWALLOWED repeat press is written to the log (R97).
// The presses in between are only counted: the client's own AI presses on every 100 ms
// pass while a target is nominated (ge\ai\lib.scp:118-131 SCR_AI_LIB_ATTACKTARGET), so
// one line per press would be ten lines a second for the whole fight.
const attackAbsorbPace = time.Second

// attackHoldTimeout is how long ONE press is held while its character walks in (R100).
//
// THIS IS NOT THE CLOCK THAT DECIDES THE HIT. The hit fires the first time the body is
// inside the stance's reach, on the position update or the tick that says so
// (fireHeldAttackOnArrival) - a walk that ends a few units short of the stand point no
// longer stands there waiting for a clock. This limit only says when a press that never
// gets there stops being a press: a body stuck on a wall, a target that left the world, a
// walk nobody closes. Five seconds is longer than any walk to a stand point this zone
// plans (the owner's worst case, ~500 units at ~469 u/s, is about one second) and short
// enough that a stale press cannot still be waiting when the player presses again.
const attackHoldTimeout = 5 * time.Second

// attackWalkReplanCap is how many times ONE attack may walk again after its walk ended
// with the target still out of reach (R98). The reference states no such number: its
// chase state re-decides on its own clock for as long as the target is out of reach,
// because the body it chases is a live monster and its AI pass runs whether or not
// anybody clicks. Here the only driver is the walk's own tick, so a monster that keeps
// stepping away - which is exactly what the owner's log shows: a wandering body that
// had moved 50+ units by the time the character arrived - would keep a character
// walking for ever. Five re-plans is more than a wandering body needs, and it ends.
// A NEW PRESS starts a fresh count, so a player who keeps fighting is never capped.
const attackWalkReplanCap = 5

// attackStandoffMargin is how much CLOSER than the body a re-walk asks to stand (R99):
// 90% of where it stands now, so the new request is strictly closer than the point that
// just failed - the "same stand point every 300 ms, for ever" loop cannot start.
//
// IT NO LONGER APPLIES TO THE RING (2026-11-04, R132). It used to shrink the stance's own
// stand point by 10% as well, to swallow the path engine answering a few units past the
// reach (R99's 206.5 against a 200 max). The owner's question killed that: "the margin is
// useless no? ... i dont really see the point of the margin?" - and he was right, because
// the PULL-IN below already answers an overshooting plan by moving the point toward the
// target, and only when the engine is actually wrong. So the ring is the reference's own
// number now (`maxR + targetR` from the target's centre, PlayerChar.cpp:1393) and this
// margin survives only where it does real work: the re-walk.
const attackStandoffMargin = 0.9

// stanceRange answers the normal attack's reach for one stance, straight out of the
// row the zone already loaded (`datatable_stance.xml`, StanceTable.Combat). 0 for
// MaxR means "the row states no maximum" - the caller must then check nothing.
func (h *Handler) stanceRange(stance uint32) (minR, maxR float64) {
	if h.Stances == nil {
		return 0, 0
	}
	row := h.Stances.Combat(stance)
	return row.MinR, row.MaxR
}

// stanceName is the stance's ClassName ("BackGuard"), or "" when this zone holds no
// stance table or the id has no row - a fact a log line may state, never a panic.
func (h *Handler) stanceName(stance uint32) string {
	if h.Stances == nil {
		return ""
	}
	return h.Stances.StanceName(stance)
}

// attackStandoff is where the character should stand to swing, as a distance from the
// TARGET'S CENTRE - the reference's ring, minus a margin:
//
//	int dist = (maxR + obj->GetDomainRadius());      PlayerChar.cpp:1393, GetAttackMaxPos
//	m_world->GetInnerPos(m_shape, m_pos, obj->GetPos(), dist, moveTo);
//
// The reference's ring is MaxR plus the target's own body, reached from the character's
// side of the line (GetInnerPos, where the too-near case uses GetOuterPos), and that is
// the number the reference stands on - because the reference's own tick puts it there.
// THIS side's answer is the PATH ENGINE's, and it is not the same thing: the owner's
// session shows the engine answering a point 6.5 units past a 200 reach, so a goal asked
// for exactly on the ring can arrive OUTSIDE the reach and be refused on arrival (R99).
//
// THE REQUEST IS THE RING ITSELF since R132 (his call, 2026-11-04): the path engine's own
// error is answered by the pull-in when it happens, not by standing every character 10%
// closer than the game would. MaxR 200, a 50 body and a 100 body still give a 300 stand
// point from the target's centre - the reference's number, not ours.
//
// The narrow-band case is the old rule kept in EDGE terms: a stance whose MinR is so
// close to its MaxR that the outer ring would sit under its near bound gets the middle
// of its band instead, measured the same edge-to-edge way the range check measures. The
// margin is NOT applied there - inside a band there is nothing to overshoot, and pulling
// the mid-band point closer would put it under MinR.
// 0 means "the row states no maximum", which the caller treats as nothing to walk for.
func attackStandoff(minR, maxR, myR, targetR float64) float64 {
	if maxR <= 0 {
		return 0
	}
	// THE REFERENCE'S OWN RING, NO MARGIN (R132): maxR + the target's body, from its
	// centre. A plan that answers PAST the reach is pulled in below, and that is the
	// fallback - only a wrong answer pays for it, not every stand point.
	stand := maxR + targetR
	if minR > 0 && maxR-myR < minR {
		stand = (minR+maxR)/2 + myR + targetR
	}
	return stand
}

// attackStepBackStand is where a character stands to swing when the target is INSIDE the
// stance's minimum reach (R101) - the near ring, and the exact mirror of attackStandoff's
// far one:
//
//	int dist = minR + (GetDomainRadius() + obj->GetDomainRadius());   PlayerChar.cpp:2108
//	if (false == m_world->GetOuterPosByTarget(m_shape, m_pos, obj->GetPos(), dist, moveTo))
//	                                                                 PlayerChar.cpp:2109
//
// GetOuterPosByTarget is GetInnerPos's mirror: the point `dist` from the target's centre
// on the character's own side, i.e. OUT of the target rather than into it. That is the
// point at which the EDGE distance is exactly MinR - dist minus the two bodies - so it is
// the nearest stand point the stance allows.
//
// THE 90% MARGIN IS NOT APPLIED HERE, and that is the one difference from attackStandoff.
// The margin exists to keep a FAR goal inside MaxR when the path engine overshoots
// outward; on the min ring the engine's error points the other way (inward), so the
// mirror of that margin would be a point OUTSIDE the ring, further than needed. The pull
// -out in approachAttack is what handles an engine answer that lands inside MinR.
//
// 0 means "the stance states no minimum", which the caller treats as nothing to step to.
func attackStepBackStand(minR, myR, targetR float64) float64 {
	if minR <= 0 {
		return 0
	}
	return minR + myR + targetR
}

// attackRetreatMargin is the ONE extra unit a retreat aims past the min ring, and it is
// not a second stand point: it is the price of an int32 DESTINATION.
//
// A retreat is computed as a vector from the target's centre (attackRetreatPoint), and
// the packet carries x and y as whole numbers, so each axis loses up to half a unit when
// it is written down. The worst case is 0.707 of a unit INWARD on the diagonal, which
// would put the goal at `MinR - 0.707` of clear ground - still under the minimum, so the
// fire-on-arrival check would keep refusing the very point the retreat chose. One unit
// past the ring swallows that rounding whole (423 written as 422.9 is still 50.9 clear),
// and 1 unit is nothing against a 50-unit band.
//
// The reference needs no such number: its GetOuterPosByTarget answers a WORLDPOS from the
// navmesh, not a rounded vector, so `MinR + both bodies` is exact for it.
const attackRetreatMargin = 1.0

// standNearRange is how far from a computed stand point the mesh is allowed to be asked for
// real ground, and how far vertically (R130). It is deliberately TIGHT: this is a correction
// for a point an obstacle ate, not a search - a point that has no ground within this radius
// is reported as such instead of being teleported somewhere else on the map.
const (
	standNearRange  = 200
	standNearRangeZ = 100
)

// standOnRealGround is the gate his question asked for (2026-11-04): "i dont want the
// character to suddenly get stuck due to collision. we want the attack to land at the
// stopping point but in a valid position."
//
// THE REFERENCE'S OWN ANSWER, read the same day: every attack stand point comes out of the
// path engine, not out of arithmetic - CPlayerChar::GetAttackMaxPos / GetAttackMinPos both
// return `m_world->GetOuterPosByTarget(...)` (PlayerChar.cpp:2109, :2117), which is
// `m_ground->GetOuterPosByTarget(...)` (World.cpp:2021). The mesh names a point it knows is
// walkable, or it refuses and the press does nothing. This zone's approach already asks the
// same engine through Handler.Plan - but the STEP BACK is pure arithmetic on purpose (R104:
// the engine treats the target as an obstacle and clamps every retreat request into it), so
// that one point could land inside a wall.
//
// WHAT IT DOES: answers the same point when the mesh says it is ground; otherwise the nearest
// ground within standNearRange, and says it moved; otherwise the same point and `known` = true
// with nothing moved (the caller logs that the mesh has no ground there). `known` = false means
// this zone has no mesh at all and the caller must not pretend it checked.
func (h *Handler) standOnRealGround(pos [3]int32) (out [3]int32, moved, known bool) {
	out = pos
	if h.NavHeight == nil {
		return out, false, false
	}
	if _, ok := h.NavHeight(pos); ok {
		return out, false, true
	}
	if h.NavNear != nil {
		if at, _, on := h.NavNear(pos, standNearRange, standNearRangeZ); on && at != pos {
			return at, true, true
		}
	}
	return out, false, true
}

// attackRetreatRing is the centre distance a RETREAT aims for: the reference's own sum,
// `MinR + (my body + its body)` (CMonsterNPC::GetAttackMinPos, MonsterNPC.cpp:1047:
// `int dist = int(GetMinR() + (GetDomainRadius() + obj->GetDomainRadius()))`), plus the
// rounding margin above. There is NO path-engine call, so there is no answer of "no clear
// stand point" and no clamp into the target: this number is arithmetic and always exists.
func attackRetreatRing(minR, myR, targetR float64) float64 {
	ring := attackStepBackStand(minR, myR, targetR)
	if ring <= 0 {
		return 0
	}
	return ring + attackRetreatMargin
}

// attackRetreatPoint is THE RETREAT, ARITHMETIC ONLY (R104): the point `ring` units from
// the TARGET'S CENTRE, on the line that runs from the target through the body - i.e.
// straight away from the monster, out to where the edge distance is `ring - both bodies`
// (= MinR + the margin).
//
// WHY IT IS NOT A PLAN. The path engine treats the target as an OBSTACLE, so a request
// for a point INSIDE the target's own body is clamped onto it and comes back as "no clear
// stand point" (Handler.Plan's `overlap`). When the bodies already overlap the body
// stands inside the monster's collision radius, so EVERY retreat request is inside the
// obstacle - the engine can never answer, and the press was dropped with nothing sent.
// The owner's log (logs\go_zone1331.log, 33 times since 19:46, edge -0.52 .. -45.18) is
// that state. Arithmetic has no such failure: the point exists whether or not it is
// walkable ground, and the client walks to it exactly as it walks to any other goal.
//
// THE DEGENERATE CASE IS A DIRECTION, NOT A SKIP. If the body is exactly on the target's
// centre there is no line to extend (a zero vector), so `fallback` - the body's last walk
// direction (Session.attackRetreatDir), else +X - picks the side. A press is never
// answered with no packet for want of an angle.
//
// THE CELL is the body's OWN cell (`from[2]`): the retreat moves away from the target,
// so the goal lies on the body's side of the line, and the body's cell is the only cell
// this rule knows to be ground. A plan answers a cell from the mesh; arithmetic must not
// pretend to.
func attackRetreatPoint(from, target [3]int32, ring float64, fallback [2]float64) [3]int32 {
	dx := float64(from[0] - target[0])
	dy := float64(from[1] - target[1])
	if d := math.Hypot(dx, dy); d > 1e-6 {
		dx, dy = dx/d, dy/d
	} else if d := math.Hypot(fallback[0], fallback[1]); d > 1e-6 {
		dx, dy = fallback[0]/d, fallback[1]/d
	} else {
		// +X, the stable last resort: a defined walk always exists.
		dx, dy = 1, 0
	}
	return [3]int32{
		target[0] + int32(math.Round(dx*ring)),
		target[1] + int32(math.Round(dy*ring)),
		from[2],
	}
}

// attackRetreatDir is the stable direction a degenerate retreat uses: the way this
// character was last WALKING. Two sources, newest first - the character's own reported
// move stream (walks[idx], the client's last CZ_MOVE_PC segment) and, failing that, the
// server-driven walk in flight when this character owns it. +X when there is neither,
// which is a defined answer and not a refusal.
//
// It answers the body's FORWARD direction, and that is the correct side: a body that
// walked INTO the target must step back out along the line it came in on, so the last
// walk's own direction points away from the monster.
func (s *Session) attackRetreatDir(idx int) [2]float64 {
	if idx >= 0 && idx < len(s.walks) {
		if w := s.walks[idx]; !w.at.IsZero() {
			if dx, dy := w.dest[0]-w.from[0], w.dest[1]-w.from[1]; dx != 0 || dy != 0 {
				return [2]float64{float64(dx), float64(dy)}
			}
		}
	}
	if w := s.approachOf(idx); w != nil {
		if dx, dy := w.goal[0]-w.from[0], w.goal[1]-w.from[1]; dx != 0 || dy != 0 {
			return [2]float64{float64(dx), float64(dy)}
		}
	}
	return [2]float64{1, 0}
}

// attackStepBackPlan is THE TOO-NEAR ANSWER, in ONE place (R102), for both callers that
// decide a reach: the press (zone\attack.go's default case, i.e. the click) and the
// fire-on-arrival check below. Nothing here is a second rule:
//
//	the ring is attackStepBackStand, the reference's GetAttackMinPos
//	    (PlayerChar.cpp:2102-2115, GetOuterPosByTarget);
//	the walk is approachAttack, the same one the walk-in uses, told to go OUT; and
//	stepBack is what makes the one log line name the direction and what makes a re-plan
//	    aim the min ring again instead of the far one.
//
// WHY IT EXISTS AS A FUNCTION. The owner's session (logs\go_zone1331.log, 18:01:33.948
// and 18:01:36.216) showed the same press stepped back correctly and then died at the
// FIRE, because only the entry path knew this case. One rule with two implementations is
// how the two ends of one walk came to disagree; this is the one implementation.
func (h *Handler) attackStepBackPlan(sess *Session, pa pendingAttack, why string) [][]byte {
	pa.stand = attackStepBackStand(pa.minR, pa.myR, pa.targetR)
	pa.stepBack = true
	return h.approachAttack(sess, pa.idx, pa, why)
}

// attackPacketIn answers whether an answer really carries a HIT: the 3564 (or a splash's
// 3566) that the client turns into a swing. A move packet, a stop packet and an empty
// answer are all "no".
//
// IT EXISTS SO A LOG LINE CANNOT CLAIM MORE THAN THE WIRE (R102). "ATTACK FIRED ON
// ARRIVAL" is what the owner and the PM read as proof that a hit went out, and it was
// being written before the hit was built - so a press that the replay refused (too near,
// too far, a stance that changed) printed the proof and sent nothing.
func attackPacketIn(out [][]byte) bool {
	for _, pkt := range out {
		if len(pkt) < 2 {
			continue
		}
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			return true
		}
	}
	return false
}

// attackStandoffCloser is the stand point a RE-WALK asks for (R99): 90% of where the body
// ACTUALLY stands from the target's centre, not the stance's fixed ring.
//
// WHY. A re-plan that asks for the same distance asks for the same point, and the owner's
// session shows what that costs: `still 206.5 away`, then "a new press replaces the walk
// to the attack on target 536870916 (2504, -884, cell 108)" - the identical stand point,
// every ~300 ms, for ever. The request cannot change if it is a constant, so it is read
// from the body: `gapNow` is the edge-to-edge distance the walk ended at, so the centre
// distance is that plus the two bodies, and 90% of it is strictly closer than the point
// that just failed. A monster that has stopped moving is then inside the reach one step
// later; a monster that keeps stepping away is what attackWalkReplanCap is for.
//
// It answers 0 - "use the ring" - when there is nothing closer to ask for: the body is
// already inside the far bound, or the 90% point would fall under the stance's near bound.
func attackStandoffCloser(minR, maxR, myR, targetR, gapNow float64) float64 {
	if maxR <= 0 || gapNow <= maxR {
		return 0
	}
	closer := (gapNow + myR + targetR) * attackStandoffMargin
	if minR > 0 && closer-(myR+targetR) < minR {
		return 0
	}
	return closer
}

// attackRangeAllowed is THE NORMAL ATTACK'S RANGE CHECK, and it is ONE function so
// the press, the stand point and the replay at the end of the walk cannot disagree.
//
// THE DISTANCE IS EDGE TO EDGE (R91 round 5), measured exactly as the reference measures
// it (`CFieldChar::IsAttackArea`, FieldChar.cpp:98-133):
//
//	float len = GetDistance(m_pos, pos) - (GetDomainRadius() + domainSize);   :118
//	if (0 >= minR && 0 >= len) return 0;                    :120-121  touching is in range
//	if (minR <= ceil(len) && maxR >= floor(len)) return 0;   :123-124
//	if (minR >  ceil(len)) return -1;                       too NEAR
//	if (maxR <  floor(len)) return 1;                       too FAR
//
// So two bodies 220 apart with a 50 and a 60 between them lie 110 of CLEAR GROUND
// apart, and 110 is what MaxR is compared against. A centre-to-centre test refused
// hits the game lands and walked a character in for a hit it did not need - the
// owner's "still jumpy" on the live build.
//
// The answer is that same len, already reduced by both bodies, so every caller that
// logs it quotes the number the rule used. An unstated MaxR (0) bounds nothing, and
// minR <= 0 with len <= 0 is the reference's own "we are touching, so we are in range".
func attackRangeAllowed(from, to [3]int32, minR, maxR, myR, targetR float64) (float64, bool) {
	gap := math.Sqrt(distSq(from[0], from[1], to[0], to[1])) - (myR + targetR)
	if minR <= 0 && gap <= 0 {
		return gap, true
	}
	if gap < minR {
		return gap, false
	}
	if maxR > 0 && gap > maxR {
		return gap, false
	}
	return gap, true
}

// approachAttack starts the walk an out-of-reach normal attack asked for, and answers
// the ONE move packet (or nothing, with one log line saying why).
//
// IT WALKS AT ONCE since R91 round 6, and `why` is the reason the caller states for the
// walk: the client's own AI never moves the character to a clicked enemy
// (attacker.scp:135-170 -> lib.scp's SCR_AI_LIB_ATTACKTARGET only attacks or sleeps), so
// the reference walks from the press itself (FieldChar_FSM.cpp:28-80) and so does this
// zone. The reason is appended to the one line that describes the walk, not written as a
// second line for the same state change.
//
// It is `approachCast`'s twin on purpose, down to the order of the checks: the same
// `Handler.Plan`, the same `walk` record (with `atk` where a cast puts `cast`), the
// same MR_SKILL reason - the only reason a capture shows for a SERVER-driven walk -
// and the same clock firing the answer at the end. Nothing new is invented for the
// attack except which request is replayed.
func (h *Handler) approachAttack(sess *Session, idx int, pa pendingAttack, why string) [][]byte {
	const tag = "zone: ATTACK APPROACH"

	now := time.Now()

	// THE REPEAT ASKS AT MOST ONCE PER CHASE CLOCK, AND A REFUSAL IS AN ASK (R111).
	//
	// The reference checks m_nextAttackChaseTime before it plans anything
	// (FieldChar_FSM.cpp:38-42 stamp, :57 check), so a second re-measure inside the
	// window changes nothing at all - and a stand point it could not compute is not
	// retried on the next pass either, it waits for the clock. This is the same clock,
	// kept on the engagement (swingLoop.walkTriedAt) rather than on the walk, so a walk
	// that has already ended still holds the window.
	//
	// IT IS THE REPEAT'S ALONE (pa.repeat). A press has the two rules below, both keyed
	// on the walk in flight, and it keeps them: a click is a person asking.
	if pa.repeat {
		if eng := sess.swingOf(pa.idx); eng != nil && eng.target == pa.target {
			if !eng.walkTriedAt.IsZero() && now.Sub(eng.walkTriedAt) < attackChasePace {
				return nil // inside the reference's 300 ms re-measure window
			}
			eng.walkTriedAt = now
		}
	}

	// A RE-DECISION INSIDE 300 ms IS NOT A RE-DECISION (R91 round 6). The reference
	// checks its chase clock before it plans anything - `if (m_nextAttackChaseTime <
	// timeGetTime())` (FieldChar_FSM.cpp:57), the stamp set a few lines above at :38-42 -
	// so a press that arrives inside that window changes nothing at all: not the
	// destination, not the walk, not the hit it is walking for. Without this, two clicks
	// in one frame would send two ZC_MOVE_PC for one body and the far client would see
	// it snap between them.
	//
	// IT HOLDS A PRESS FOR THE SAME TARGET ONLY, and that is a deliberate difference from
	// the reference's per-character clock. The reference re-decides by ITSELF on the next
	// AI pass once the 300 ms are up; this zone re-decides only when a press arrives. A
	// held press for a DIFFERENT enemy would therefore never be re-decided at all and the
	// player's click would vanish, so a new target wins at once (see the drop below).
	if w := sess.approachOf(idx); w != nil && w.atk != nil && !w.castDone &&
		w.atk.target == pa.target && now.Sub(w.sentAt) < attackChasePace {
		h.Log.Printf("%s: char %d (%d) pressed target %d again %s into the walk to (%d, "+
			"%d, cell %d) - the destination is re-decided at most every %s "+
			"(m_nextAttackChaseTime, FieldChar_FSM.cpp:38-42, checked at :57), so this "+
			"press is treated as the same stand point as the one in flight: NOTHING is "+
			"sent, keeping the walk (its hit is not dropped)",
			tag, idx, sess.Team[idx].Handle, pa.target,
			now.Sub(w.sentAt).Round(time.Millisecond), w.goal[0], w.goal[1], w.goal[2],
			attackChasePace)
		return nil
	}

	// A REPEAT PRESS FOR THE TARGET ALREADY BEING WALKED TO IS NOT A NEW DECISION (R97).
	//
	// THE MEASUREMENT. The owner's session (logs\go_zone1331.log, 15:59-16:01, with
	// millisecond stamps) holds 110 attack presses in two minutes: 97 of them started a
	// walk-in, 10 landed a hit, 1 was cancelled, 2 got no answer. The 10 hits each
	// carried a server latency of 0-1 ms, so answering is already instant - the failure
	// is that 88% of the presses never became a hit at all. The presses are ~1 s apart
	// and EVERY one of them re-planned the walk, so the previous walk was replaced
	// before it could finish: the character never arrived and the player read it as
	// "attack does not work".
	//
	// WHY THE CLIENT SENDS THEM. It is not a stray click. ge\ai\lib.scp:118-131
	// SCR_AI_LIB_ATTACKTARGET calls Attack() on every AI loop (100 ms) while a target is
	// nominated and its own IsAbleToAttack says yes - and that notion of "able" is the AI
	// profile's keep range (ge\xml\datatable_ai.xml: ATTACKER KeepRange 1000), not the
	// stance's 200-unit reach. So a 1 s cadence of presses for one nominated enemy is
	// the client doing exactly what its script says.
	//
	// WHAT THE REFERENCE DOES. It does not re-plan per press: the approach is a STATE
	// (source2/geServer/ZServerLib/FieldChar_FSM.cpp:28-80) that re-decides on its own
	// 300 ms clock and only when the destination CHANGED (IsEqualPos(moveTo, m_moveTo)).
	// This zone has no per-character AI pass to re-decide from, so the press is what
	// re-decides - and that is the only difference this rule closes: a press for the
	// target already in flight is ABSORBED. Nothing is sent, nothing is cancelled,
	// nothing is re-planned, and the walk keeps the hit it owes.
	//
	// THE ORDER IS THE RULE. The 300 ms clock above answers a press inside its own
	// window; this answers the presses OUTSIDE it, which are the ones the owner saw
	// re-plan (88% of them). The stand-point equality check below stays exactly as it
	// is - it is the reference's own text - and this rule only means the same-target
	// press never reaches it.
	//
	// THE FOUR WAYS A PRESS STILL RE-PLANS, and each one is untouched here: the target
	// differs (this check does not match it at all), there is no walk in flight, the walk
	// has ENDED, or the player's own click cancelled it (move.go drops the walk, so
	// there is nothing left to absorb into).
	if w := sess.approachOf(idx); w != nil && w.atk != nil && !w.castDone &&
		w.atk.target == pa.target {
		w.absorbed++
		// ONE LINE PER SECOND, WITH A COUNT (attackAbsorbPace): the client presses on
		// every 100 ms AI pass, and a line per press is the log flood this rule is
		// meant to remove from the WIRE, not to add to the log.
		if w.absorbedAt.IsZero() || now.Sub(w.absorbedAt) >= attackAbsorbPace {
			left := w.eta - now.Sub(w.began)
			if left < 0 {
				left = 0
			}
			h.Log.Printf("%s: char %d (%d) pressed target %d again and that target is "+
				"ALREADY being walked to (%d, %d, cell %d, %s of walking left of an eta "+
				"%s) - %d press(es) absorbed since the last line, NOTHING sent: no move, "+
				"no cancel and no re-plan, so the walk keeps the hit it owes (the client's "+
				"own AI presses every 100 ms: lib.scp:118-131)",
				tag, idx, sess.Team[idx].Handle, pa.target, w.goal[0], w.goal[1],
				w.goal[2], left.Round(100*time.Millisecond), w.eta.Round(time.Millisecond),
				w.absorbed)
			w.absorbed, w.absorbedAt = 0, now
		}
		return nil
	}

	from := sess.charPos(idx, now)
	// THE RING IS THE ONE POINT THAT IS ALWAYS INSIDE THE REACH, and `stand` is what this
	// approach asks for: the ring for a press, 90% of where the body stands for a re-walk
	// (R99). `ring` is kept as well because the pull-back below falls back to it.
	ring := attackStandoff(pa.minR, pa.maxR, pa.myR, pa.targetR)
	stand := ring
	if pa.stand > 0 {
		stand = pa.stand
	}

	var (
		goal     [3]int32
		route    [][3]int32 // the planned polyline, or nil when the walk goes straight
		routeLen float64
		// clampHit says the plan answered a CLAMP HIT: the destination is the target's
		// own position and the route is the engine's, so the walk goes out and the hit
		// fires on arrival. See the `case overlap` below.
		clampHit bool
	)
	if pa.stepBack {
		// A RETREAT IS NEVER PLANNED (R104). The path engine treats the target as an
		// OBSTACLE, so every retreat request - which by definition lands inside the
		// monster's own body - is clamped into it and answered "no clear stand point",
		// and the press then went out as NO packet and NO walk. The owner's log has 33 of
		// those since 19:46 (edge -0.52 .. -45.18), each one a press that died, and each
		// one followed by another press that died the same way: that is the character
		// standing on the monster and never attacking again.
		//
		// The answer is arithmetic, exactly the reference's GetAttackMinPos sum
		// (`MinR + both bodies`, MonsterNPC.cpp:1047) taken along the line from the
		// target through the body, so it exists for every input and cannot be refused.
		// NOTHING BELOW THIS POINT ASKS Handler.Plan for it - including the h.Plan == nil
		// refusal above, which this branch is reached before.
		goal = attackRetreatPoint(from, pa.pos,
			attackRetreatRing(pa.minR, pa.myR, pa.targetR), sess.attackRetreatDir(idx))
		routeLen = math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
		// THE STOPPING POINT MUST BE GROUND (R130): the retreat is the one stand point this
		// zone computes WITHOUT the path engine (R104), so it is the one that can land inside a
		// wall - and a body ordered into a wall never arrives, which is a press that dies.
		if fixed, moved, known := h.standOnRealGround(goal); moved {
			h.Log.Printf("%s STEP BACK: the retreat point (%d, %d, cell %d) is NOT walkable "+
				"ground - moved to (%d, %d, cell %d), the nearest ground this map's mesh has "+
				"(R130)", tag, goal[0], goal[1], goal[2], fixed[0], fixed[1], fixed[2])
			goal = fixed
		} else if known {
			if _, ok := h.NavHeight(goal); !ok {
				h.Log.Printf("%s STEP BACK: the retreat point (%d, %d, cell %d) is NOT walkable "+
					"ground and this mesh has none within %d units - walked anyway, because the "+
					"retreat is arithmetic and a refused retreat is a press that does nothing "+
					"(R130)", tag, goal[0], goal[1], goal[2], standNearRange)
			}
		}
	} else {
		if h.Plan == nil {
			h.Log.Printf("%s OUT OF REACH: %g away, stance %d reach MinR %g .. MaxR %g, "+
				"but this zone has no path engine (Handler.Plan is nil), so it cannot "+
				"name a stand point that is real ground - nothing sent", tag, pa.dist,
				sess.Team[idx].Stance, pa.minR, pa.maxR)
			return nil
		}
		var (
			overlap bool
			ok      bool
			err     error
		)
		goal, route, routeLen, overlap, ok, err = h.Plan(from, pa.pos, stand)
		switch {
		case err != nil:
			h.Log.Printf("%s OUT OF REACH: %g away, stance %d reach MinR %g .. MaxR %g, "+
				"and the path engine failed (%v) - nothing sent", tag, pa.dist,
				sess.Team[idx].Stance, pa.minR, pa.maxR, err)
			return nil
		case !ok:
			h.Log.Printf("%s OUT OF REACH: %g away, stance %d reach MinR %g .. MaxR %g, "+
				"and the character's own position (%d, %d, cell %d) is not valid ground "+
				"to walk from - nothing sent", tag, pa.dist, sess.Team[idx].Stance,
				pa.minR, pa.maxR, from[0], from[1], from[2])
			return nil
		case overlap:
			// THE CLAMP'S ANSWER IS A WALK, NOT A REFUSAL (2026-11-10).
			//
			// WHAT IT USED TO DO. A clamp hit answered `overlap`, and this switch
			// refused the press with "no clear stand point exists on the target's side
			// (the path engine clamped into the target itself) - nothing sent". That
			// line is the owner's silent click one branch further along: the newest
			// post-fix log (logs\zone_go_9001.log, 2026-11-09 19:54) holds 10 of them
			// against 250 walks, and the press they answer is a body standing on the
			// map with the target in sight.
			//
			// WHY THE PLAN IS NOT SAYING "NO". Since the agreed clamp rule (owner,
			// 2026-11-09 16:25: "we agree on the clamp") a clamp hit answers the
			// TARGET'S OWN POSITION and the REAL ROUTE to it - the client walks at the
			// body and stops at collision distance (measured 15:56: arrived, gap -100,
			// box opened). The click path has walked that answer since; only the attack
			// still refused it. The reach is not measured on the destination anyway:
			// the hit fires on the first position update or tick that finds the body
			// inside the stance's band (fireHeldAttackOnArrival), so a walk toward the
			// target passes through the ring and fires there.
			//
			// THE REFERENCE'S RULE, and the reason a refusal here is wrong: a player
			// standing on the map can always attack something out of reach - the gate
			// is the ATTACK, not the walk (FieldChar.cpp:956-991 -> AS_ATTACK_CHASE;
			// FieldChar_FSM.cpp:61-79 walks the body at the ring from wherever it
			// stands). A refusal is owed only when the ground genuinely cannot answer,
			// and that is the `!ok` case above.
			//
			// NOTHING IS RE-DECIDED HERE: goal, route and routeLen are the plan's own
			// answer, and the mesh stays the authority for the route. The reach check
			// below is skipped for this destination because it is the TARGET ITSELF
			// (the edge gap is negative), not a stand point that missed the band.
			clampHit = true
		}
	}

	// THE POINT MUST BE INSIDE THE STANCE'S OWN RANGE, checked with the same function
	// the press used: the plan answers ground, not reach.
	standDist, inRange := attackRangeAllowed(goal, pa.pos, pa.minR, pa.maxR, pa.myR,
		pa.targetR)
	if !inRange && pa.maxR > 0 && standDist > pa.maxR {
		// A PLAN ANSWER PAST THE REACH IS PULLED IN, NOT REFUSED (R99).
		//
		// THE MEASUREMENT: the owner's walk ended on the plan's own answer and the next
		// line refused it - 206.54526582545685 against a 200 reach - so nothing was ever
		// sent to a body that was standing on the point the walk had chosen. The answer is
		// the only ground there is, so arithmetic cannot be the fix; the POINT moves
		// instead. One projection along the line from the target to the plan's own answer,
		// out to the margin ring - which is inside the reach by construction - and one
		// line saying how far it moved. The refusal below still stands for a point that is
		// too NEAR, where moving toward the target is the wrong direction.
		pulled := pointTowards(pa.pos, goal, ring)
		pulled[2] = goal[2]
		if pulledDist, pulledOK := attackRangeAllowed(pulled, pa.pos, pa.minR, pa.maxR,
			pa.myR, pa.targetR); pulledOK {
			moved := math.Hypot(float64(goal[0]-pulled[0]), float64(goal[1]-pulled[1]))
			h.Log.Printf("%s: the path engine's stand point (%d, %d, cell %d) is %g from "+
				"target %d, PAST the %g reach, so it was pulled %g units toward the "+
				"target to (%d, %d, cell %d) - %g from it, inside the reach - and the walk "+
				"is sent", tag, goal[0], goal[1], goal[2], standDist, pa.target, pa.maxR,
				moved, pulled[0], pulled[1], pulled[2], pulledDist)
			goal, standDist, inRange = pulled, pulledDist, true
		}
	}
	if !inRange && pa.stepBack && pa.minR > 0 && standDist < pa.minR {
		// A PLAN ANSWER INSIDE THE MINIMUM REACH IS PUSHED OUT, NOT REFUSED (R101) - the
		// exact mirror of the pull-in above, and the same reason: the plan answers ground,
		// not reach, and the path engine's own answer can be a few units off. Here the
		// error points inward, so the point moves AWAY from the target along the line the
		// engine chose, out to the min ring (which is inside the reach by construction).
		// The refusal below still stands for a point that is too FAR, where moving away
		// from the target is the wrong direction.
		outRing := attackStepBackStand(pa.minR, pa.myR, pa.targetR)
		pushed := pointTowards(pa.pos, goal, outRing)
		pushed[2] = goal[2]
		if pushedDist, pushedOK := attackRangeAllowed(pushed, pa.pos, pa.minR, pa.maxR,
			pa.myR, pa.targetR); pushedOK {
			moved := math.Hypot(float64(goal[0]-pushed[0]), float64(goal[1]-pushed[1]))
			h.Log.Printf("%s: the path engine's stand point (%d, %d, cell %d) is %g from "+
				"target %d, INSIDE MinR %g, so it was pushed %g units AWAY from the "+
				"target to (%d, %d, cell %d) - %g from it, inside the reach - and the "+
				"walk is sent", tag, goal[0], goal[1], goal[2], standDist, pa.target,
				pa.minR, moved, pushed[0], pushed[1], pushed[2], pushedDist)
			goal, standDist, inRange = pushed, pushedDist, true
		}
	}
	if !inRange && !pa.stepBack && !clampHit {
		// A CLAMP HIT IS NOT THIS REFUSAL (2026-11-10). Its destination is the TARGET
		// ITSELF, so the edge gap is NEGATIVE (the two bodies overlap) and this check
		// would read "too near" as "still out of reach" - the opposite of what the
		// number says. The walk goes out and the hit fires on arrival; the too-near
		// branch of fireHeldAttackOnArrival steps the body back out if it ends up on
		// top of the target (R101/R102), exactly as it does for a walk that overshoots.
		h.Log.Printf("%s OUT OF REACH: %g away, stance %d reach MinR %g .. MaxR %g, and "+
			"the only stand point the path engine offers (%d, %d, cell %d) is %g from "+
			"the target - still out of reach, so nothing sent", tag, pa.dist,
			sess.Team[idx].Stance, pa.minR, pa.maxR, goal[0], goal[1], goal[2], standDist)
		return nil
	}
	// A RETREAT NEVER ENDS HERE (R104). Its goal is arithmetic on the stub's own numbers,
	// so it is inside MinR..MaxR by construction; if the check above ever disagreed that
	// would be a BUG in the sum, not a reason to answer a press with nothing. The walk is
	// sent either way: the hit it owes fires the first moment the body really is inside
	// the reach, and a body with a walk is never a body standing still.

	// THE SAME STAND POINT IS NOT A NEW REQUEST (R91 round 6). The reference compares the
	// point it would walk to with the one already in flight and does NOTHING when they
	// are equal - `if (false == IsEqualPos(moveTo, m_moveTo))` (FieldChar_FSM.cpp:49 and
	// :74). Two presses for one target from one spot plan to the same ring point, so the
	// second one sends nothing and the walk, with the hit it owes, carries on. Dropping
	// it here is what left a rapid attack/move/attack with a walk and no swing.
	//
	// This runs AFTER the plan so the comparison is between two real destinations, and
	// it is the ONLY reason an old walk survives a press: every failure above has already
	// returned with the walk untouched.
	if w := sess.approachOf(idx); w != nil && w.atk != nil && !w.castDone &&
		w.atk.target == pa.target && w.goal == goal {
		h.Log.Printf("%s: char %d (%d) pressed target %d again and the stand point (%d, "+
			"%d, cell %d) is the one already in flight - same stand point, so NOTHING is "+
			"sent, keeping the walk and the hit it owes (IsEqualPos(moveTo, m_moveTo), "+
			"FieldChar_FSM.cpp:49 and :74)", tag, idx, sess.Team[idx].Handle, pa.target,
			goal[0], goal[1], goal[2])
		return nil
	}

	// THE STAND POINT THIS ENGAGEMENT LAST ORDERED IS REMEMBERED PAST THE END OF ITS
	// WALK, AND AN UNCHANGED ONE IS NOT ORDERED AGAIN (R111).
	//
	// This is the reference's rule with nothing added: it compares the point it WOULD
	// walk to with m_moveTo, and m_moveTo survives the end of a walk -
	// `if (false == IsEqualPos(moveTo, m_moveTo)) { m_moveTo = moveTo;
	// SetMoveState(MS_MOVE); }` (FieldChar_FSM.cpp:45-52, :70-78) - so a body whose stand
	// point did not change is walked to ONCE, however often the state machine re-measures.
	//
	// THE BUG IT CLOSES, measured on the owner's own session (logs\go_zone1331.log,
	// 2026-11-02): 30 orders back to the monster in 38 seconds while he dragged the
	// warlock away, 12 of them to the identical point (1779, -529). Every one of those
	// walks was already OVER - its hit had fired, so swingTick's guard (`!w.castDone`) no
	// longer held - and the next tick ordered the same destination again a second later,
	// which is the "it teleports her back instead of walking her" he reported.
	//
	// IT HAS NO CLOCK OF ITS OWN: an unchanged point is not re-ordered on the next tick,
	// nor the next second. Only a point that CHANGED is a new order, which is the
	// reference's IsEqualPos and nothing else.
	if pa.repeat {
		if eng := sess.swingOf(pa.idx); eng != nil && eng.target == pa.target &&
			eng.walkOrdered && eng.walkGoal == goal {
			eng.walkHeld++
			// ONE LINE PER attackAbsorbPace, WITH THE COUNT (the R97 absorb rule's
			// shape): the tick polls several times a second, and a line per held tick
			// would be the log flood this rule exists to remove from the wire.
			if eng.walkSaidAt.IsZero() || now.Sub(eng.walkSaidAt) >= attackAbsorbPace {
				h.Log.Printf("%s: char %d (%d) is still out of reach of target %d and the "+
					"stand point it would walk to is (%d, %d, cell %d) - the ONE already "+
					"ordered %s ago - so NOTHING is sent: the stand point has not changed "+
					"(IsEqualPos(moveTo, m_moveTo), FieldChar_FSM.cpp:45-52); %d tick(s) "+
					"held since the last line", tag, idx, sess.Team[idx].Handle, pa.target,
					goal[0], goal[1], goal[2],
					now.Sub(eng.walkGoalAt).Round(100*time.Millisecond), eng.walkHeld)
				eng.walkHeld, eng.walkSaidAt = 0, now
			}
			return nil
		}
	}

	speed := h.packetSpeed(&sess.Team[idx])
	if speed <= 0 {
		speed = MoveBaseSpeed
	}
	// THE ROUTE ENDS WHERE THE WALK DOES (R110). The plan's polyline ends on the point it
	// answered; the pull-in and the push-out above can MOVE that point, so the last corner
	// follows it and the length is measured again. Without this the clock would count a
	// route to a spot the body is not going to, and the geometry would stop short of the
	// goal - the same disagreement this walk is being fixed for, one unit smaller.
	if len(route) > 0 {
		route[len(route)-1] = goal
		routeLen = polylineLength(route)
	}
	walked := routeLen
	if walked <= 0 {
		walked = math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
	}
	eta := time.Duration(walked / speed * float64(time.Second))

	// ONE PENDING ANSWER, NEVER A QUEUE, exactly as for a cast: this press needs a
	// destination of its own, so whatever was in flight is dropped HERE - after every
	// check that can refuse - and never on a path that sends nothing.
	// ONE APPROACH PER CHARACTER, NEVER A QUEUE (R131). A press replaces THIS character's
	// own walk and nobody else's: character 2 hunting must not take character 0's approach
	// away, which is exactly what the team's single record used to do.
	if cur := sess.approachOf(idx); cur != nil {
		h.Log.Printf("%s: a new press replaces the walk to %s (%d, %d, cell %d) - this "+
			"character's own walk, and only its own (R131: every other character's keeps "+
			"running)", tag, cur.describe(), cur.goal[0], cur.goal[1], cur.goal[2])
	}

	installed := &walk{
		from: from, goal: goal, eta: eta, began: now, sentAt: now,
		speed: speed, caster: idx, atk: &pa, clamped: true, route: route,
		// THE TARGET AND ITS OWN BODY, for the log line the arrival writes. The reach
		// itself is NOT re-tested here: the stance's band is asked on every tick, on the
		// body's live point (attack_approach.go, fireHeldAttackOnArrival), so near stays 0.
		target: pa.pos, targetR: pa.targetR,
	}
	sess.addApproach(installed)

	// THE ENGAGEMENT REMEMBERS THE POINT IT JUST ORDERED (R111). It is written here, on
	// the walk that is really being installed, so the record can only name a destination
	// that reached the wire (or was already in flight, when legMoveFor answers nil) - and
	// it outlives this walk: see swingLoop.walkGoal, and the equality gate above.
	if eng := sess.swingOf(idx); eng != nil && eng.target == pa.target {
		eng.walkOrdered, eng.walkGoal, eng.walkGoalAt = true, goal, now
		eng.walkHeld = 0
	}
	// A reported segment the character left open must not close in the middle of this
	// walk and stop the far actor half way (chase.go, startChase does the same).
	//
	// RECORDED FIRST (R91 round 7): this walk's goal is written into Team[idx].Position
	// only when the body gets there (placeWalker), so a FOLLOWER's live point would
	// otherwise be lost with the segment and the swing that ends the approach would name
	// the point an earlier walk last wrote - the snap. idx 0 is untouched by
	// rememberWalk, so the leader's path is exactly what it was.
	sess.rememberWalk(idx, now)
	sess.closeWalk(idx)

	pkt, handle := h.legMoveFor(sess, idx, goal, ReasonSkill)
	if pkt == nil {
		// A WALK TO THE SAME STAND POINT IS ALREADY IN FLIGHT (zone\walk_once.go), so
		// legMoveFor sent nothing. The press is NOT spent: the walk installed above is this press's
		// own walk, so the hit still fires when it ends - only the duplicate move is gone.
		return nil
	}
	h.broadcastMove(sess, idx, handle, goal, pkt)

	// THE ONE LINE, WITH THE DIRECTION ON IT (R101). A step-back and a walk-in send the
	// same packet through the same code, so the line is the only thing that tells a reader
	// which way the body went - and for a step-back it must carry the three numbers the
	// card asks for: the EDGE distance (negative when the bodies overlap), the min ring,
	// and the destination.
	walking := "walking"
	ringText := fmt.Sprintf("ring MaxR %g + its body %g", pa.maxR, pa.targetR)
	atText := "from the target's centre"
	if pa.stepBack {
		walking = "STEPPING BACK"
		ringText = fmt.Sprintf("min ring MinR %g + its body %g + its own %g", pa.minR,
			pa.targetR, pa.myR)
		// standDist is the EDGE distance the reach is measured in, so the step-back line
		// says so rather than calling it a centre distance it is not.
		atText = "of clear ground from the target"
	} else if clampHit {
		// THE CLAMP HIT'S DESTINATION IS THE TARGET ITSELF, and the line must say so:
		// the packet's destination is the monster's own position, which reads as a bug
		// unless the reason is on the same line.
		walking = "walking AT THE TARGET ITSELF"
		ringText = "the clamp HIT, so the destination is the target's own position - the " +
			"agreed clamp rule, 2026-11-09: the client walks at the body and stops at " +
			"collision distance, and the hit fires on the first tick inside the band"
		atText = "of clear ground from the target"
	}
	h.Log.Printf("%s: commander %d char %d (%d) target %d at (%d, %d, cell %d) is %g "+
		"of clear ground away and stance %d (%q) reaches MinR %g .. MaxR %g - %s "+
		"%.0f units to (%d, %d, cell %d), %g %s (%s), eta %.1fs at "+
		"%.0f u/s (cmd %d, %d B, reason %d); the hit fires when the walk ends (%s)",
		tag, sess.CommanderID, idx, sess.Team[idx].Handle, pa.target, pa.pos[0],
		pa.pos[1], pa.pos[2], pa.dist, sess.Team[idx].Stance,
		h.stanceName(sess.Team[idx].Stance), pa.minR, pa.maxR, walking, walked, goal[0],
		goal[1], goal[2], standDist, atText, ringText, eta.Seconds(), speed,
		CmdZCMovePC, len(pkt), ReasonSkill, why)

	return [][]byte{pkt}
}

// firePendingAttack replays the client's own 3666 once the character is in reach.
//
// It calls the SAME handler the in-range press calls, with attackAfterWalk set, so the
// packets, their order (3564 then 3557) and the log line are the in-range ones by
// construction. The flag exists for ONE reason: if the replay finds the target out of
// reach again - a stance changed, a monster that stepped away - it must re-decide the
// approach the way the reference does (replanAfterWalk) instead of swinging at a body
// it cannot reach.
//
// IT ANSWERS THE REASON IT BUILT NOTHING (R103), in the second return value: the empty
// string when a hit was built, a short sentence naming the gate otherwise (see attackAt).
// The fire-on-arrival path prints that sentence, so no line in the log says "no packet
// was built" without saying which gate stopped it.
//
// `from` is the body point the CALLER measured, or nil to read the character's own.
// The continuous check passes its own (one press, one measurement); the walk's fire
// clock passes nil and keeps its R98 re-plan untouched.
func (h *Handler) firePendingAttack(sess *Session, pa *pendingAttack,
	from *bodyPoint, now time.Time) ([][]byte, string) {
	if pa == nil {
		return nil, "the press this was called for is already gone"
	}
	return h.attackAt(sess, pa.frame, attackAfterWalk, from, now)
}

// movePacketIn reports whether one answer contains a WALK (a ZC_MOVE_PC). The fire-on-
// arrival path needs it because a walk is an OUTCOME of its own (R103): the press was
// handed to a walk, which is not the same thing as a press that bought nothing.
func movePacketIn(out [][]byte) bool {
	for _, pkt := range out {
		if len(pkt) < 2 {
			continue
		}
		if binary.LittleEndian.Uint16(pkt) == CmdZCMovePC {
			return true
		}
	}
	return false
}

// =================================================================================
// R100: A HELD PRESS FIRES THE MOMENT THE BODY IS INSIDE THE STANCE'S REACH
// =================================================================================
//
// THE REPORTED SYMPTOM, in the owner's words: "moves, stops in front of the target, does
// not attack immediately". Two clocks were to blame and only one of them is this side's:
//
//	the client's own AI, after a failed attack, does WarningMsg(...); sleep(1000)
//	    (D:\ge_server\ge\ai\lib.scp:118-131 SCR_AI_LIB_ATTACKTARGET), so its NEXT attempt
//	    can be a full second away - the character arrives, stands, and swings a second
//	    later, or never;
//	this zone fired the walk's hit on a clock (approachFireDue = eta + 50 ms) and, when
//	    the body was a few units short of the stand point it measured against, refused
//	    the hit and re-planned instead (the owner's "still 206.5 away, reach 200").
//
// THE REFERENCE FIRES ON THE CONDITION, not on a clock: its attack state asks
// IsAttackArea and, when the body is in area, does SetMoveState(MS_STOP);
// SetActionState(AS_ATTACK) (source2/geServer/ZServerLib/FieldChar_FSM.cpp:28-80, case 0).
// There is no timer between "in reach" and "swing".
//
// So the press is HELD while the character walks in, and evaluated continuously: on every
// position update for that character (zone\zone.go, movePC - each CZ_MOVE_PC) and on every
// tick (zone\chase.go, chaseTick). The first answer that passes fires the press AT ONCE
// and forgets it; nothing about the walk's own clock can fire it afterwards, because the
// one flag that clock reads (walk.castDone) is set here.
//
// The walk's clock is NOT removed. It still closes the walk, and its short-fall re-plan
// (replanAfterWalk) stays as the fallback for the case this check cannot see: a body that
// stops short with no position report coming - which is the ordinary case on this client,
// because the leader sends no reports at all during a SERVER-driven walk (measured
// 2026-10-29, R64, see observeWalk).

// holdOf answers the attack press this character is walking for, or nil.
//
// The press lives on the WALK (walk.atk), which is what makes it travel into a re-plan
// unchanged and die with the walk; castDone is the flag that says the press has already
// been answered or dropped, so nothing may fire it again.
func holdOf(sess *Session, idx int) (*walk, *pendingAttack) {
	w := sess.approachOf(idx)
	if w == nil || w.atk == nil || w.castDone {
		return nil, nil
	}
	return w, w.atk
}

// bodyAt is where the body walking to its attack stands at `now`, by the walk's own
// geometry: along the walk's ROUTE at the speed the move we sent told the client to
// animate, and on the straight chord only when the walk has no route.
//
// It is walkedRouteAt, the same interpolation every other clock in this zone uses, so the
// distance the reach is measured against is the body the player sees - not the stand
// point, and not the point the press was made from. A bent walk is the case that needs
// it: the chord cuts the corner, so it put the body up to hundreds of units away from the
// route the clock counted, and the swing hard-sets the client's actor to whatever this
// returns (GeActor.cpp:300-309).
func (w *walk) bodyAt(now time.Time) [3]int32 {
	return walkedRouteAt(w.route, w.from, w.goal, w.began, now, w.speed)
}

// fireHeldAttackOnArrival is THE CONTINUOUS CHECK (R100): it answers the held press the
// first moment the body is inside the stance's reach, and nothing at all before that.
//
// THE BODY IS READ HERE, FROM ONE MODEL (R104). It used to be handed in by the caller:
// a position update passed the point the CLIENT named, and a tick passed the point the
// walk had walked to. Those are two different things, and the first one is not a
// position at all - CZ_MOVE_PC carries a DESTINATION (move.go, openWalk), so the fire
// for a report was measured at, and then sent to, a point the body had not walked to:
// logs\go_zone1331.log 19:13:21.181 printed `pos (2392, -770) from segment (2605, -1299)
// -> (2997, -1170)`, and 2392,-770 is the destination of the report two lines later.
// attackBodyPoint is now the only reader, so the reach is measured at the point the
// client has DRAWN the body at, and that same point is the one handed to the builder -
// one measurement, one point.
//
// A BODY THAT IS NOT THERE YET DOES NOT FIRE. It keeps walking (the move was already
// sent, so the press stays held) and the first update that finds it inside the reach
// fires it; nothing about this check moves the body itself.
func (h *Handler) fireHeldAttackOnArrival(sess *Session, idx int,
	now time.Time) [][]byte {
	if idx < 0 || idx >= len(sess.Team) {
		return nil
	}
	w, pa := holdOf(sess, idx)
	if pa == nil {
		return nil
	}

	// THE PRESS DIES WITH ITS REASON, AND THE LINE IS THE ONLY TRACE. Every case below
	// leaves the walk running (the other players were already sent that move, and its
	// stop is still owed), so without the line a reader would watch a character walk to a
	// stand point and never swing with nothing on the log to explain it.
	if stance := sess.Team[idx].Stance; pa.stance != 0 && stance != pa.stance {
		return h.dropHeldAttack(sess, w, fmt.Sprintf("the character stands in stance %d "+
			"(%q) now, not the %d (%q) the press was made in", stance,
			h.stanceName(stance), pa.stance, h.stanceName(pa.stance)))
	}
	mon, ok := h.monsterByHandle(pa.target)
	if !ok {
		return h.dropHeldAttack(sess, w, fmt.Sprintf("target %d is not a monster this "+
			"zone is running any more (AS_ATTACK ends on a null target, "+
			"FieldChar_FSM.cpp:141-146)", pa.target))
	}
	if hp, known := h.monsterLiveHP(pa.target); known && hp <= 0 {
		return h.dropHeldAttack(sess, w, fmt.Sprintf("target %d has no HP left in this "+
			"zone's own ledger (a dead body is not attacked, FieldChar_FSM.cpp:136-140)",
			pa.target))
	}

	// THE HIT'S OWN QUESTION, asked of the body the client has DRAWN and the target as
	// it stands NOW: a monster that stepped towards the character is in reach sooner,
	// and one that stepped away is in reach later, without either of them moving the
	// press.
	//
	// THE ONE MEASUREMENT (R104). This point is where the body is on screen, and it is
	// the point the builder is handed below, so the reach this line measures and the
	// position in the 3564's +8 are the same number. Nothing here writes the model: the
	// fire used to PLACE the body on the stand point first (placeWalker), which moved
	// the actor on screen whether or not the body had walked there - the reference
	// builds its attack from `m_pos`, the live position, and never moves the character
	// to its goal (source2/geServer/ZServerLib/FieldChar.cpp:424).
	body := sess.attackBodyPoint(idx, now, h.packetSpeed(&sess.Team[idx]))
	target := [3]int32{mon.X, mon.Y, mon.Cell}
	gap, inRange := attackRangeAllowed(body.pos, target, pa.minR, pa.maxR, pa.myR, pa.targetR)
	if !inRange {
		// THE HOLD'S OWN LIMIT, IN BOTH DIRECTIONS. A body that never reaches the ring and
		// a body that never leaves the inside of it are the same unanswered press.
		if held := now.Sub(pa.since); held >= attackHoldTimeout {
			return h.dropHeldAttack(sess, w, fmt.Sprintf("the press has been held %s and "+
				"the body is still %g from target %d against a reach of MinR %g .. MaxR "+
				"%g (limit %s)", held.Round(time.Millisecond), gap, pa.target, pa.minR,
				pa.maxR, attackHoldTimeout))
		}
		// TOO NEAR IS THE PRESS PATH'S OWN CASE, AND IT IS ANSWERED WITH THE PRESS PATH'S
		// OWN CODE (R102). THE MEASUREMENT: the owner's session, logs\go_zone1331.log,
		// 18:01:33.948 and 18:01:36.216 - the body arrived at 45 of clear ground against
		// BlandirCruz's MinR 50, this check answered "not yet", the walk's clock fired
		// instead, firePendingAttack re-measured 45, and replanAfterWalk - which had no
		// step-back to re-issue, because the press was not marked as one - answered "the
		// last plan was only 104ms ago ... NOTHING is sent". The press was consumed, the
		// line above it claimed a hit, and no 3564 ever left.
		//
		// The reference has ONE answer to IsAttackArea -1 wherever it is asked, entry or
		// arrival: GetAttackMinPos -> move (FieldChar_FSM.cpp:63-79). So this is that
		// same answer - attackStepBackPlan, the function the press calls - and the press
		// stays HELD, so the first report or tick that finds the body inside MinR..MaxR
		// fires it. A step-back already in flight is not a new decision: the body is on
		// its way out.
		if pa.minR > 0 && gap < pa.minR {
			// THE REPORT THAT ASKED FOR THIS WALK IS NOT THE CLICK THAT CANCELS IT.
			pa.answeredHere = true
			if pa.stepBack {
				return nil
			}
			step := *pa
			step.pos = target
			step.dist = gap
			// THE WALK THAT WAS WALKING IN HAS DONE ITS PART: marking it answered is what
			// lets the step-back be a NEW decision instead of an absorbed re-press (the
			// absorb rule matches by target, and this press has the same target).
			w.castDone = true
			return h.attackStepBackPlan(sess, step, fmt.Sprintf("the body arrived %g of "+
				"clear ground from target %d, INSIDE the stance's minimum reach MinR %g "+
				"(the reference's own IsAttackArea -1 -> GetAttackMinPos, "+
				"FieldChar_FSM.cpp:63-79): the character steps BACK to the min ring, and "+
				"the hit fires when the body is inside MinR %g .. MaxR %g", gap,
				pa.target, pa.minR, pa.minR, pa.maxR))
		}
		return nil
	}

	// THE FIRE IS BUILT FIRST, AND THE LINE IS WRITTEN ONLY IF IT WAS (R102): a press
	// that the replay refuses must not leave "ATTACK FIRED ON ARRIVAL" on the log, because
	// that line is read as proof that a hit went out.
	//
	// ANSWERED AND FORGOTTEN IN THE SAME BREATH. castDone is what stops the walk's own
	// clock firing the same press again, and it is what the absorb rule and the click's
	// cancel read to know this press no longer wants anything.
	w.castDone = true
	// THE MEASUREMENT IS HANDED DOWN, AND NOTHING ELSE IS (R104): `body` is the point
	// this function just measured, so the packet's +8 is that point and not a second
	// reading of a second model. The model is NOT touched - see the placement note above
	// the measurement: firing must not move the character.
	//
	// THE SAME INSTANT GOES WITH IT (R111): `now` is the clock this measurement was read
	// at, and it is the clock the packet is built at, so the reach this line measured and
	// the position the 3564 carries are one instant.
	out, why := h.firePendingAttack(sess, pa, &body, now)

	// ONE PRESS, EXACTLY ONE OUTCOME (R103). Before this the only question was "is there
	// an attack packet?", and a press handed to a walk answered it with "no" - so the
	// same press got a plan line AND a consumed line, and the walk it sent was live while
	// the line said the press was spent and not retried.
	switch {
	case attackPacketIn(out):
		// THE FIRE, AND THE LINE THAT PROVES IT: the edge distance it landed at, against
		// the reach it had to be inside, and how long the press was held.
		h.Log.Printf("zone: ATTACK FIRED ON ARRIVAL char %d (%d) at %g of reach %g - the "+
			"press was held %s", idx, sess.Team[idx].Handle, gap, pa.maxR,
			now.Sub(pa.since).Round(time.Millisecond))
		// THE FIGHT STARTS HERE, SO THE BODY STOPS HERE (R132, his observation 2026-11-04:
		// "the character will reposition themselves when hit started landing?").
		//
		// MEASURED. The band is wide (ShotgunBlaster MaxR 1125), so this fire lands LONG
		// before the stand point: logs\zone_go_live.log 17:50:17 - press at 17.406, first
		// hit at 18.711 with the body at (1442,-821), second hit at 19.919 at (1575,-804),
		// 138 units further on. She was swinging and walking to the ring at the same time.
		//
		// THE REFERENCE STOPS THE BODY WHEN THE ATTACK STARTS: `IsAttackArea` says yes ->
		// SetMoveState(MS_STOP) -> SetActionState(AS_ATTACK) (FieldChar_FSM.cpp:28-80,
		// quoted in R100). So the walk in flight is CLOSED here, at the point the hit was
		// measured from, and every later swing of the engagement is built from that point -
		// exactly the pin R129 put on a press, one door further along.
		if !w.arrived(now) {
			// THE BODY IS ON A WALK THIS SERVER SENT, so it is closed by SEATING the body and
			// stopping it - the pin above is for a client leg, and here there is none open.
			goal := w.goal
			sess.settleBodyAt(idx, body.pos)
			sess.clearApproach(w)
			h.stopWalker(sess, idx, body.pos)
			h.Log.Printf("zone: ATTACK FIRED ON ARRIVAL char %d (%d): the walk to (%d, %d, "+
				"cell %d) is DROPPED - the body stands at (%d, %d, cell %d) and every later "+
				"swing is built from there, because the reference STOPS the body when the "+
				"attack starts (SetMoveState(MS_STOP), FieldChar_FSM.cpp:28-80; R132)",
				idx, sess.Team[idx].Handle, goal[0], goal[1], goal[2],
				body.pos[0], body.pos[1], body.pos[2])
			return out
		}
		sess.clearApproach(w)
		h.stopWalker(sess, idx, body.pos)
		return out
	case movePacketIn(out):
		// HANDED TO THE WALK, which writes its own plan line and its one 3548. The press
		// is not spent - the new walk carries it - so no second line belongs here.
		return out
	default:
		// CONSUMED, AND THE LINE NAMES THE GATE THAT CONSUMED IT. The body was inside the
		// stance's reach, so the press is spent here and will not be tried again - and
		// `why` is the builder's own sentence: the attack-speed gate with the two numbers
		// it compared, a cast in progress, a target this zone does not run or keeps no HP
		// for, an empty build. Without the reason the line was unreadable: 72 presses in
		// one session ended on it and nothing on the log said why.
		h.Log.Printf("zone: ATTACK HELD press CONSUMED, no hit: char %d (%d) target %d was "+
			"%g of clear ground - inside MinR %g .. MaxR %g - when the press fired, but "+
			"the hit was not built: %s; no hit follows this press and it is not "+
			"retried", idx, sess.Team[idx].Handle, pa.target, gap, pa.minR, pa.maxR, why)
		return out
	}
}

// dropHeldAttack forgets a held press with ONE line saying why, and answers no packets.
//
// THE WALK IS NOT DROPPED WITH IT, and that is deliberate: the move already went to every
// player in the zone, so the character walks to its stand point and the stop clock closes
// it as usual - what changes is that nothing fires at the end. castDone is the only flag
// the fire paths read, so one write is the whole drop.
func (h *Handler) dropHeldAttack(sess *Session, w *walk, reason string) [][]byte {
	pa := w.atk
	w.castDone = true
	handle := uint32(0)
	if pa.idx >= 0 && pa.idx < len(sess.Team) {
		handle = sess.Team[pa.idx].Handle
	}
	h.Log.Printf("zone: ATTACK HELD press DROPPED: char %d (%d) target %d - %s; nothing "+
		"is sent for it, no hit follows, and the walk keeps only its stop clock",
		pa.idx, handle, pa.target, reason)
	return nil
}

// dropHeldAttackForAnotherTarget drops a press the player has walked away from by
// pressing a DIFFERENT enemy (R100).
//
// The replacement itself is approachAttack's (a new target wins at once, because this zone
// re-decides on a press and not on an AI pass) and it logs the new walk. What this adds is
// the death of the OLD press WITH ITS OWN LINE, for the presses that never reach
// approachAttack at all: a new target already inside the reach is answered by a hit
// (supersedePendingAttack writes its own line), and a new target the stance refuses answers
// nothing - and in both cases the old press must stop wanting a hit.
func (h *Handler) dropHeldAttackForAnotherTarget(sess *Session, idx int, target uint32) {
	w, pa := holdOf(sess, idx)
	if pa == nil || pa.target == target {
		return
	}
	h.dropHeldAttack(sess, w, fmt.Sprintf("the player pressed target %d instead of "+
		"this one", target))
}

// replanAfterWalk answers a walk that ended with the target still out of reach: it
// walks the character AGAIN, on the reference's own pace, or says in one line why it
// will not (R98).
//
// THE REFERENCE WALKS AGAIN, and this is the bug it fixes. Its attack state
// (AS_ATTACK_CHASE) re-decides on its own 300 ms clock and, when IsAttackArea answers
// -1 (still too far), takes GetAttackMaxPos and issues ANOTHER move to it
// (source2/geServer/ZServerLib/FieldChar_FSM.cpp:28-80, case 1). It never stands still
// out of range. This zone had the opposite rule - one line, nothing sent, "no second
// walk" - and the owner saw what that costs in game: logs\go_zone1331.log holds 92
// such refusals, the body ~50 units short of its stance's reach each time, and the
// report was "the character refuses to attack while standing in front of the target".
//
// EVERYTHING ABOUT THE WALK IS approachAttack's: the same Handler.Plan, the same ring,
// the same edge-to-edge reach, the same reuse rule, the same move packet. This function
// only decides WHETHER there is another walk - the pace, the count on the walk record,
// and the cap - so the ring and the reach stay in one place.
//
// THE PACE IS THE REFERENCE'S OWN 300 ms (m_nextAttackChaseTime, FieldChar_FSM.cpp:38-42
// set, :57 checked). A walk that takes longer than that - every walk that ends still out
// of reach does - passes it at once; the check is here because the reference has it, and
// because two re-decisions inside one window would put two ZC_MOVE_PC for one body on
// the wire.
func (h *Handler) replanAfterWalk(sess *Session, pa pendingAttack, mon Monster) [][]byte {
	const tag = "zone: ATTACK APPROACH"

	stance := sess.Team[pa.idx].Stance

	// THE COUNT AND THE LAST PLAN TIME come off the walk record that just ended - the
	// same record approachAttack wrote, so there is one place a repeat can be counted.
	// THE PRESS'S OWN CLOCK COMES OFF IT TOO (R100): `since` is when the PLAYER pressed,
	// and the hold's limit is measured from that, so a re-plan - this side walking again
	// for the SAME press - must not restart it.
	repeats, plannedAt, pressedAt := 0, time.Time{}, time.Time{}
	if w := sess.approachOf(pa.idx); w != nil && w.atk != nil &&
		w.atk.target == pa.target {
		repeats, plannedAt, pressedAt = w.atk.repeats, w.sentAt, w.atk.since
	}

	// THE CAP, CHECKED BEFORE ANYTHING ELSE: a body that keeps stepping away is a stop,
	// not a loop. ONE line names the distance, the reach and the cap, and nothing is
	// sent again for this press - the walk ends on its own stop clock and the next press
	// starts a fresh count.
	if repeats >= attackWalkReplanCap {
		h.Log.Printf("%s: the walk ended and target %d (%q) is still %g away, stance %d "+
			"(%q) reach MinR %g .. MaxR %g - this attack has walked again %d time(s) and "+
			"the target is STILL out of reach, so it stops here (cap %d re-plans per "+
			"press: a monster that keeps stepping away is not a loop to run for ever; a "+
			"new press starts a fresh count)", tag, pa.target, h.MonsterName(mon), pa.dist,
			stance, h.stanceName(stance), pa.minR, pa.maxR, repeats, attackWalkReplanCap)
		return nil
	}

	// THE PACE, the reference's own chase clock. Nothing is sent inside it, and the line
	// says which clock held it rather than leaving a reader to guess at a silence.
	now := time.Now()
	if !plannedAt.IsZero() && now.Sub(plannedAt) < attackChasePace {
		h.Log.Printf("%s: the walk ended and target %d (%q) is still %g away, stance %d "+
			"(%q) reach MinR %g .. MaxR %g - the last plan was only %s ago, INSIDE the "+
			"reference's %s chase clock (m_nextAttackChaseTime, FieldChar_FSM.cpp:38-42, "+
			"checked at :57), so NOTHING is sent and the next press re-decides",
			tag, pa.target, h.MonsterName(mon), pa.dist, stance, h.stanceName(stance),
			pa.minR, pa.maxR, now.Sub(plannedAt).Round(time.Millisecond), attackChasePace)
		return nil
	}

	// THE WALK AGAIN: one move, through the press's own path. The line names the
	// distance, the reach and the repeat number, so the next reading of a log is one
	// grep.
	pa.repeats = repeats + 1
	if !pressedAt.IsZero() {
		pa.since = pressedAt // the press's own clock: a re-plan is not a new press (R100)
	}
	// THE DIRECTION, so one line is true for both rings (R101): the far case says which
	// reference call it is re-issuing, and the near case says its mirror.
	where := "case 1: still too far -> another move to GetAttackMaxPos"
	if pa.stepBack {
		where = "case -1: still too NEAR -> another move to GetAttackMinPos"
	}
	h.Log.Printf("%s: the walk ended and target %d (%q) is still %g away, stance %d (%q) "+
		"reach MinR %g .. MaxR %g - WALKING AGAIN, re-plan %d of %d on the reference's "+
		"%s chase clock (FieldChar_FSM.cpp:28-80, %s)", tag, pa.target, h.MonsterName(mon),
		pa.dist, stance, h.stanceName(stance), pa.minR, pa.maxR, pa.repeats,
		attackWalkReplanCap, attackChasePace, where)

	// THE TARGET MOVED, SO THE RING MOVED: pa.pos is the monster's position read for
	// THIS replay, so the new stand point is planned against where the body is now.
	//
	// AND IT AIMS CLOSER THAN THE POINT THAT JUST FAILED (R99): the stand-off comes from
	// the distance this walk ENDED at (pa.dist), not from the stance's constant ring, so
	// the answer is a different point every time and the treadmill above cannot happen.
	//
	// A STEP-BACK REPLAN AIMS AT THE MIN RING AGAIN (R101), not at the far one: the walk
	// that just ended was trying to get OUT of the minimum, so the point it failed at is
	// nearer than the min ring, and the answer is that ring itself - the mirror of
	// attackStandoffCloser, and the only re-plan whose destination is fixed.
	if pa.stepBack {
		pa.stand = attackStepBackStand(pa.minR, pa.myR, pa.targetR)
	} else {
		pa.stand = attackStandoffCloser(pa.minR, pa.maxR, pa.myR, pa.targetR, pa.dist)
	}
	why := "the walk ended with the target still out " +
		"of reach, so the approach re-decides on the reference's own chase clock " +
		"(FieldChar_FSM.cpp:28-80, case 1)"
	if pa.stepBack {
		why = "the walk ended with the target still INSIDE the minimum reach, so the " +
			"approach re-decides on the reference's own chase clock and steps back out " +
			"(FieldChar_FSM.cpp:28-80, case -1)"
	}
	return h.approachAttack(sess, pa.idx, pa, why)
}

// supersedePendingAttack marks an attack approach this character still had in flight
// as ANSWERED, because the press being handled now IS the hit that walk was walking for
// (R91 round 6). Only `castDone` is touched - the walk itself lives on to its STOP
// clock, because the other players are owed a ZC_STOP_PC for a move they were already
// sent, and a hit is not a reason to leave their copy of the actor walking for ever.
//
// IT IS CALLED ONLY WHERE A PRESS IS CERTAIN TO BE ANSWERED, and that placement is the
// rule: a press that is refused or held answers nothing, so it must not disarm the walk
// in flight - the walk would then arrive and swing for nobody. A press that hits means
// the walk's hit for the same request is no longer owed.
//
// WHICH WALK SURVIVES A PRESS is not decided here any more: the re-use rule (the same
// stand point keeps the walk, `IsEqualPos(moveTo, m_moveTo)`, FieldChar_FSM.cpp:49 and
// :74, plus the 300 ms clock at :38-42) belongs to approachAttack, which is the only
// place that knows which point the new press would walk to.
func (h *Handler) supersedePendingAttack(sess *Session, idx int) {
	w := sess.approachOf(idx)
	if w == nil || w.atk == nil || w.castDone {
		return
	}
	w.castDone = true
	h.Log.Printf("zone: ATTACK: char %d (%d) pressed again while walking to (%d, %d, "+
		"cell %d) for target %d - that walk's hit is dropped (this press is answered "+
		"now) and the walk keeps only its stop clock",
		idx, sess.Team[idx].Handle, w.goal[0], w.goal[1], w.goal[2], w.atk.target)
}

// monsterBodyRadius is the TARGET's half of the edge-to-edge reach: the collision body
// its own row names. `datatable_monster.xml` carries `Size="S"` on the Scarecrow (11600),
// which is the same four bodies the reference's shape table holds
// (source/geShared/Source/ShapeTable.cpp:47-63) - so the number is the game's, not this
// file's. A row that names no size, or a zone with no monster table, answers 0 and says
// so ONCE: the reach is then one body short, which is the honest half of a measurement
// rather than a monster size invented to fill it.
func (h *Handler) monsterBodyRadius(mon Monster) float64 {
	if h.Monsters != nil {
		if row, ok := h.Monsters.Row(mon.ClassID); ok {
			if r := row.BodyRadius(); r > 0 {
				return r
			}
			h.logNoMonsterSizeOnce(mon.ClassID, row.Size)
			return 0
		}
	}
	h.logNoMonsterSizeOnce(mon.ClassID, "")
	return 0
}

// logNoMonsterSizeOnce names the missing half of the reach ONCE per server run: the
// condition is a property of this build's data, not of each click.
func (h *Handler) logNoMonsterSizeOnce(classID uint32, size string) {
	if h.noMonsterSizeLogged.Swap(1) != 0 {
		return
	}
	h.Log.Printf("zone: REACH UNKNOWN: monster class %d names no body size (%q in "+
		"datatable_monster*.xml) or this zone has no monster table, so its half of the "+
		"edge-to-edge reach is UNKNOWN and only the character's own %g is subtracted - "+
		"the reach is one body short, and this zone will not invent a monster size",
		classID, size, PCBodyRadius)
}

// cancelApproachAttack is the normal attack's half of the CANCEL WINDOW (R90 item 4):
// a `3598 CZ_MOVE_PC` from the character that is still WALKING to its attack cancels
// the whole press, exactly as it does for a cast's approach (zone\skill_approach.go,
// cancelApproachCast).
//
// It answers true when it cancelled something, and the caller then carries on with the
// ordinary move - the same click-to-walk path, so the character walks where the player
// pointed and nothing is sent for the attack: no 3564, no 3557, no cancel packet. A
// swing that has already gone out is NOT cancellable: it is one packet sent the instant
// the body is in reach, and by then there is no state left to cancel.
func (h *Handler) cancelApproachAttack(sess *Session, idx int) bool {
	w := sess.approachOf(idx)
	if w == nil || w.atk == nil || w.castDone {
		return false
	}
	// THE REPORT THAT ASKED FOR THIS WALK IS NOT THE CLICK THAT CANCELS IT (R102). A click
	// that walks the body ONTO the target is answered by the fire-on-arrival check first,
	// with a step-back out to the min ring; this cancel runs right after it in movePC, would
	// read its own report as "the player walked somewhere else", and would drop the press
	// one line later with the step-back move already on the wire. answeredHere is set by
	// that check for exactly this one read, so the next report cancels as it always did.
	if w.atk.answeredHere {
		w.atk.answeredHere = false
		return false
	}
	elapsed := time.Since(w.began)
	h.Log.Printf("zone: ATTACK approach CANCELLED by a move click (nothing sent) - char %d "+
		"(%d) was %s into the walk to (%d, %d, cell %d) for the attack on target %d, and "+
		"the click for the new point cancels it: no %d, no %d",
		w.caster, sess.Team[idx].Handle, elapsed.Round(time.Millisecond), w.goal[0],
		w.goal[1], w.goal[2], w.atk.target, CmdZCAttackSingle, CmdZCAddHP)
	sess.clearApproach(w)
	// The click that cancelled this carries on as an ordinary move, and it opens its
	// own segment from sess.drawnPos (zone.go, movePC). RECORDED FIRST (R91 round 7):
	// without it that next start reads the point an earlier walk last wrote instead of
	// where the body actually stopped walking toward this target.
	sess.rememberWalk(idx, time.Now())
	sess.closeWalk(idx)
	return true
}
