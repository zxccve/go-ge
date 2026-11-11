package zone

import (
	"math"
	"time"
)

// ONE WALK PER (CHARACTER, DESTINATION), AND ONE PER TICK.
//
// WHY THIS EXISTS. Every ZC_MOVE_PC the server sends RESTARTS the client's
// interpolation of that actor (the client starts a fresh segment from the packet's
// own point), so a second walk to the point the actor is already walking to does
// not "correct" anything - it re-anchors the body mid-stride, and the owner reads
// that as a twitch. The reference guards the same thing at its own source
// (`if (false == IsEqualPos(moveTo, m_moveTo))`, FieldChar_FSM.cpp:49 and :74), and
// each lane here had grown its own partial copy of that guard: the attack approach
// compares its stand point, the chase refuses while sess.Walk is set, the skill
// approach compares nothing. That is three half-rules that must agree, and the
// owner's own log shows they can be reached in orders they were not written for.
//
// SO THE RULE LIVES AT THE SENDER INSTEAD - legMoveFor (zone\chase.go), the ONE
// function every server-driven walk packet in this package is built by: a click's
// chase, a normal attack's approach and a skill approach all end there. A walk that
// leaves is recorded against its character slot, and a second request for the SAME
// destination is answered with no packet while the first one is still in flight (or
// while it is still inside the same tick, which is the case a recorded walk with a
// zero clock would otherwise let through).
//
// WHAT IT DOES NOT DO: it moves no clock, changes no speed, changes no destination
// and touches no position. A suppressed request still leaves the walk its caller
// set - the press still counts and the hit it owes still fires at the walk's end -
// only the duplicate packet is gone. A DIFFERENT destination always goes out: that
// is the player chaining clicks, and it is not a duplicate.
//
// The package-wide count of these is in TestWalkOncePerDestination and its
// neighbours (zone\walk_once_test.go).

// walkSent is one walk this zone put on the wire for a character: the destination
// the packet named, when it left, the instant its own clock says the body has had
// time to cover it (after which a walk to the same point is a NEW request), and the
// walk record itself, so a press that was dropped or fired stops claiming the point.
type walkSent struct {
	dest  [3]int32
	at    time.Time
	until time.Time
	w     *walk
}

// spent reports whether the walk this record belongs to has nothing left to do. A
// press that was DROPPED (dropHeldAttack) or has already FIRED is not walking to
// that point any more - castDone is the one flag both write - so a new request for
// the same destination is a new walk and must go out.
func (r walkSent) spent() bool { return r.w != nil && r.w.castDone }

// walkRepeat is the window in which a second walk to the same destination is the
// same walk even when the first one's own clock has already run out. It is the
// session's tick (pollInterval): two callers that ask inside one pass of the clock
// are asking once.
const walkRepeat = pollInterval

// walkOnceAt answers whether a walk to `to` for character `idx` may go out at `now`,
// and records it when it may. false means a walk to that exact point is already on
// the wire and has not ended, so this one must not be sent.
//
// It is a decision only: it sends nothing and changes no walk. legMoveFor is the one
// caller, and it returns no packet when this says false.
func (s *Session) walkOnceAt(idx int, to [3]int32, now time.Time) bool {
	if idx < 0 {
		return true // not a team slot: there is no character to remember, so no rule
	}
	for len(s.walksSent) <= idx {
		s.walksSent = append(s.walksSent, walkSent{})
	}
	if last := s.walksSent[idx]; last.dest == to && !last.at.IsZero() && !last.spent() {
		if now.Before(last.until) || now.Sub(last.at) < walkRepeat {
			return false
		}
	}
	// HOW LONG THIS WALK IS GIVEN. The caller has already put its walk on the session
	// (startChase, approachAttack and the skill approach all set sess.Walk before they
	// ask for the packet), so its own walk time is the honest answer: the body needs
	// `eta` to cover the route, and the walk ENDS there now (chase.go, walk.arrived).
	// A caller with no matching walk falls back to the tick window alone - that still
	// collapses two requests in one pass, which is all it can prove.
	until := now.Add(walkRepeat)
	var sent *walk
	if w := s.Walk; w != nil && w.caster == idx && w.goal == to {
		sent = w
		if w.eta > 0 {
			until = now.Add(w.eta)
		}
	}
	s.walksSent[idx] = walkSent{dest: to, at: now, until: until, w: sent}
	return true
}

// forgetWalk drops the walksSent record for slot idx when its walk was cancelled: the
// point is no longer in flight, so a new click on the same destination must be allowed to
// send (2026-11-09 - the owner's click 2 was swallowed by a stale record).
func (s *Session) forgetWalk(idx int) {
	if idx >= 0 && idx < len(s.walksSent) {
		s.walksSent[idx] = walkSent{}
	}
}

// A MOVE REPORT FROM THE DRIVEN CHARACTER ENDS ITS ENGAGEMENT UNLESS IT IS A WALK WE
// SENT.
//
// THE OWNER'S RULE (2026-11-01 22:44): a move is a cancel, no matter how small. The
// scripts say the client cannot produce one on its own where the fight happens.
// KEEPPOSITION_DIST(selfAi, 2000) (ge\ai\lib.scp:106) only steps the character when it
// is farther than KeepReturnRange from its hold spot, and every AI profile in
// ge\xml\datatable_ai.xml sets KeepReturnRange 2000 - so inside 2000 units the client's
// AI never moves the character by itself, and a report from it is
// the PLAYER, whatever the length of the leg. SCR_AI_LIB_ATTACKTARGET (lib.scp:118)
// never walks either: out of range it warns and sleeps. The approach is OUR walk, so an
// approach is never mistaken for a click. SINCE 2026-11-02 THIS IS ASKED OF EVERY SLOT,
// not only the driven one: the client reports one move per character and each report is
// that character's own order (zone\zone.go's MOVE_PC block).
//
// WHAT IS LEFT TO TELL APART IS PROVENANCE, NOT LENGTH. A walk this zone asked for
// comes back to us as the client's report of its own leg, naming the destination we put
// on the wire; walkOnceAt records that destination per character slot (walksSent), so
// this answers whether the point the client just named is the one we sent.
//
// PROVENANCE IS NOT ENOUGH ON ITS OWN: THE WALK MUST STILL BE GOING SOMEWHERE (2026-11-03).
// A walk of ours excuses a report only while it still has ground to cover, and only when
// the report moves TOWARDS its goal. The owner's own session is the proof
// (logs\zone_go_live.log, 18:29:33):
//
//	18:29:33.056616 MOVE_PC char 1 -> (2198, -819) | body was at (2411, -738), leg 228 u
//	               MOVE_PC char 1 -> (2198, -819): a path step of the walk this server sent
//	               to (2411, -738) (228 u), not a move order
//	18:29:33.257320 SENT walk char 1 -> (2411, -739) ... ATTACK APPROACH ... 223.5 u away
//
// The body was AT that walk's goal, and the point "reported" was that walk's own START -
// 228 u back down the leg. Nothing was left to advance, so this was the player clicking
// away from the fight. It was swallowed, the engagement was not cancelled, and 200 ms
// later the server walked the character back: the owner's "still stubborn".
//
// SO THE QUESTION HAS THREE PARTS, and all three must hold: is this point on a leg of ours
// (walkLegCovers), has that leg still got ground to cover (its body more than
// walkRoundSlack short of the goal), and is the point CLOSER to the goal than the body
// already is. Fail any part and the report is the player's order. The walksSent record is
// held to the same test, so a walk that has ARRIVED stays arrived - walkRoundFallbackAge
// may not resurrect it.
//
// THE TWO NUMBERS BELOW ARE THE ROUNDING AND THE CLOCK, not a threshold by another name:
// walkRoundSlack exists so our own walk coming back - a point re-pathed into a
// neighbour, a diagonal rounded into a different cell - is never read as the player's
// click, and walkRoundAge is how long a walk of ours can still plausibly be reporting
// back. A click is anything else, however short.
// walkWasOurs reports whether this move report is a step of a walk this zone opened, and
// so does not end the character's engagement. It is the yes/no face of walkOurs.
func (s *Session) walkWasOurs(idx int, dest [3]int32) bool {
	return s.walkOurs(idx, dest, time.Now()).ours
}

// walkVerdict is walkOurs' answer AND the numbers it was made on, so the caller prints the
// decision instead of measuring it again (zone.go's MOVE_PC block).
//
// `ours` is the answer. `arrived` and `covered` are the two ways of saying NO that a reader
// has to be able to tell apart: a walk record of ours existed and its body had already
// ARRIVED (so nothing was left to advance), or the report covered a leg of ours but did not
// move it forward. Everything else - no record, or a point off the leg - is the plain
// "not ours" that needs no explanation, because no walk of ours ever described that point.
type walkVerdict struct {
	goal    [3]int32 // the goal of the walk the report was measured against
	dist    float64  // how far the REPORTED point sat from that goal
	left    float64  // how far the BODY still was from it at `now`
	ours    bool     // the report advances an unfinished walk of ours
	covered bool     // the geometry put the report on a leg of ours (walkLegCovers)
	arrived bool     // that leg's body was already within walkRoundSlack of its goal
}

// walkOurs answers the same question, and hands back WHICH walk the report belongs to and
// how far it sat from that walk's goal - the two numbers the decision was made on, so the
// caller can print them instead of guessing them again. The verdict also says WHY a report
// was not excused when a walk of ours covered it (walkVerdict).
//
// THE QUESTION IS "IS THIS REPORT WALKING THE LEG WE OPENED?", NOT "IS IT THE POINT WE
// NAMED" (2026-11-01 23:48, the owner's ~1.1 s pause every third swing). The client walks
// its own route and reports ONE PATH STEP AT A TIME (move.go: "the client repeats its
// destination per path step"), so its destinations sit a few units to a few tens of units
// from the goal we put on the wire. Measured at 23:31:49 that evening: the zone sent char 1
// to (2603,-843) from (2560,-1207) and the client reported (2595,-908) - ON that leg, 65 u
// short of the goal. The old rule allowed 30 u around the goal and nothing else, so it
// answered "not ours" every time; the count of the exemption line in logs\go_zone1331.log
// is ZERO, which is how that went unseen.
//
// AND SINCE 2026-11-03 IT MUST ALSO BE A STEP FORWARD (walkAdvances): a leg of ours that has
// ARRIVED covers every point on its own line, including the start it came from, so geometry
// alone would excuse the player clicking back down a walk that was already finished. That is
// the bug in the header, measured on the owner's own session.
func (s *Session) walkOurs(idx int, dest [3]int32, now time.Time) walkVerdict {
	if idx < 0 {
		return walkVerdict{}
	}

	// A "no" that can be explained, kept for the fallback below: if the open leg covers the
	// report but cannot advance it, the record of the last walk sent may still be a LATER
	// leg that can, and that one has to be asked too.
	var why walkVerdict

	// (1) THE LEG WE OPENED, while it is still in flight. This is the walk whose path steps
	// the client is reporting, and its life is the walk's OWN: the time the body needs to
	// cover its route (eta), which is when the tick ends it (chase.go, walk.arrived). No
	// fixed number of seconds can be more honest than the leg's own flight, and a longer
	// one would let a click that happens to land on an old leg pass as ours.
	if w := s.Walk; w != nil && w.caster == idx && w.inFlightAt(now) {
		v := walkAdvances(w, w.goal, dest, now)
		if v.ours {
			return v
		}
		if v.covered {
			why = v
		}
	}

	// (2) THE RECORD of the last walk this zone put on the wire for this slot. This is the
	// FALLBACK, and it needs a window of its own because a leg's LAST path step can be
	// reported after the walk itself has ended and Session.Walk is already nil - that is
	// what walkRoundFallbackAge is for, and it is the only fixed window here.
	//
	// THE WINDOW IS A CLOCK, NOT AN EXCUSE: it says how long a report may still be running
	// late, and it does NOT say the walk was still walking. A record whose walk has arrived
	// is answered by walkAdvances as "arrived", however young the record is - which is the
	// 18:29:33 case, where a finished walk excused a 228 u click and the clock alone would
	// have done it again 100 ms later.
	if idx < len(s.walksSent) {
		last := s.walksSent[idx]
		if !last.at.IsZero() {
			if age := now.Sub(last.at); age < 0 || age <= walkRoundFallbackAge {
				if last.w != nil {
					v := walkAdvances(last.w, last.dest, dest, now)
					if v.ours || v.covered {
						return v
					}
					return why
				}
				if d := flatDist(last.dest, dest); d <= walkRoundSlack {
					// NO LEG START WAS RECORDED (a caller with no matching walk), so the
					// only thing that can be proved is the point itself. There is no walk
					// here to have arrived, so nothing is left to ask.
					return walkVerdict{goal: last.dest, dist: d, ours: true}
				}
			}
		}
	}
	return why
}

// walkAdvances is the second half of the exemption: it asks walkLegCovers whether the report
// is ON the leg, and then whether the leg still has ground to cover and the report MOVES it.
//
// THE THREE ANSWERS, and why each one exists (2026-11-03):
//
//   - not covered: no walk of ours describes that point at all, so there is nothing to
//     explain and the caller prints nothing extra;
//   - covered but ARRIVED: the body is already within walkRoundSlack of the goal, so the
//     leg is finished and every point on its line - above all its own start - would
//     otherwise still "belong" to it. A finished walk excuses nothing;
//   - covered, still walking, but NOT CLOSER: the body is past the point the client named,
//     so the report is walking back along the leg, not along it. That is a click too.
//
// THE BODY IS THE WALK'S OWN (walk.bodyAt): along the route this walk was timed on, at the
// speed the client was told to animate, which is the same body the swing's reach and the
// drawn position use. A leg whose clock is missing has a body that never advances, so it
// can never claim to have arrived - the honest answer for a walk we cannot time.
func walkAdvances(w *walk, goal, dest [3]int32, now time.Time) walkVerdict {
	v := walkVerdict{goal: goal}
	if w == nil {
		return v
	}
	_, d, ok := walkLegCovers(w.from, goal, dest)
	if !ok {
		return v // nothing of this walk describes that point
	}
	v.covered = true
	v.dist = d
	v.left = flatDist(w.bodyAt(now), goal)
	if v.left <= walkRoundSlack {
		v.arrived = true
		return v
	}
	v.ours = v.dist < v.left
	return v
}

// inFlightAt says whether this walk can still be the leg the client is reporting steps of:
// the time the body needs to walk its own route has not run out. A walk with no clock at
// all is treated as in flight - the only thing that could bound it is the clock that is
// missing, and no walk this package builds leaves that field empty.
func (w *walk) inFlightAt(now time.Time) bool {
	if w == nil {
		return false
	}
	if w.began.IsZero() {
		return true
	}
	age := now.Sub(w.began)
	return age < 0 || age <= w.eta
}

// walkLegCovers says whether `dest` is a point ON the leg from `from` to `goal`, and if it
// is, the leg's goal and the distance from the report to it.
//
// THREE WAYS A POINT BELONGS TO A LEG, and each is a different fact:
//
//   - it IS the goal, or it is the leg's own start (a re-anchor: a client that re-paths can
//     name the point it set off from, and that is our walk too) - within walkRoundSlack;
//   - it LIES ON the leg: its perpendicular distance to the line start->goal is at most
//     walkCorridorWidth AND its projection falls between the two ends. This is the case
//     that was missing, and it is what a path step is;
//   - nothing else. A point behind the start, past the goal by more than the slack, or off
//     the corridor is a click, and the caller must end the engagement on it.
//
// THIS IS A GEOMETRY QUESTION ONLY, AND IT IS NOT THE WHOLE RULE. A leg that has ARRIVED
// still covers its own line and its own start for ever, so "on the leg" alone would excuse
// the player clicking back down a walk that is already finished (2026-11-03). The caller
// must ask walkAdvances, which is this plus "the body is not there yet" and "the report is
// closer to the goal than the body". walkLegCovers is unchanged by that: the geometry is
// still exactly the corridor, the two ends and the slack.
func walkLegCovers(from, goal, dest [3]int32) (g [3]int32, dist float64, ok bool) {
	dist = flatDist(goal, dest)
	if dist <= walkRoundSlack || flatDist(from, dest) <= walkRoundSlack {
		return goal, dist, true
	}
	gx := float64(goal[0] - from[0])
	gy := float64(goal[1] - from[1])
	l2 := gx*gx + gy*gy
	if l2 == 0 {
		return goal, dist, false // a leg of no length has no corridor
	}
	vx := float64(dest[0] - from[0])
	vy := float64(dest[1] - from[1])
	// t is the projection onto the leg: 0 at its start, 1 at its goal, negative behind the
	// start and greater than 1 past the goal.
	if t := (vx*gx + vy*gy) / l2; t < 0 || t > 1 {
		return goal, dist, false
	}
	if perp := math.Abs(vx*gy-vy*gx) / math.Sqrt(l2); perp > walkCorridorWidth {
		return goal, dist, false
	}
	return goal, dist, true
}

const (
	// walkRoundSlack is how far a report may sit from an END of a leg we sent - its goal, or
	// its own start - and still be that same walk. It is 30 units because the client
	// re-paths and rounds a point on the way back; it is NOT a size a leg has to reach.
	walkRoundSlack = 30.0

	// walkCorridorWidth is how far OFF the line a report may sit and still be a step of the
	// leg we opened: the corridor is the line start->goal, this wide. 100 units is about one
	// character body plus the rounding of a diagonal into a cell, and it is narrower than a
	// click that means anything: the owner's own orders were 213, 271 and 357 units.
	walkCorridorWidth = 100.0

	// walkRoundFallbackAge is the window used when NO walk is open - a leg's last path step
	// can arrive after the walk's own clock has ended it. It is the only fixed window here,
	// and it is a fallback, not the rule: an open walk is bounded by its own flight
	// (walk.inFlightAt).
	walkRoundFallbackAge = 2 * time.Second
)
