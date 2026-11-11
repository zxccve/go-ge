package zone

import (
	"math"
	"time"

	"geserver/protocol"
)

// THE SKILL APPROACH (R87): walking a character into range so a cast can land.
//
// WHY THIS EXISTS. R83 measured the skill's own range out of `datatable_skill.xml`
// and refused a cast whose target sat outside MinR..MaxR. That arithmetic is right
// and stays - but it exposed a behaviour the owner reported in his own words: "our
// server did not make the character approach the target hence why i said fighter not
// working. the range is too small and once i go near enough the skill actually
// landed." So an out-of-range press must not be a refusal AND must not be a cast
// from across the map: it is a WALK followed by the cast, which is what the
// reference does.
//
// THE REFERENCE'S OWN SHAPE, read out of
// `captures\rangeapproach20261031\20261031-183747` (both directions), because it
// decides which side walks:
//
//	c2s +536   3646 CZ_SKILL_TARGET  char 1 skill 10562 target 1602174  (out of range)
//	s2c +148432 3548 ZC_MOVE_PC   handle 1619551 -> (-2326, 1267, cell 50) reason 2
//	                              *** a stand point the CLIENT NEVER ASKED FOR: none of
//	                              its 35 CZ_MOVE_PC reports names that point, and its
//	                              own moves go out with reason 0. Reason 2 is MR_SKILL. ***
//	s2c +156200 3367 ZC_SHOW_DIGIT
//	s2c +156215 3573 ZC_SKILL_MELEE handle 1619551 skill 10562, begin (-2326, 1267, 50)
//	                              *** the cast fires from EXACTLY the point the server
//	                              walked it to - 175 units from the target, and NO second
//	                              3646 arrived first. ***
//
// The Warlock's ground press shows the same shape for 3647: handle 1619552 is moved by
// reason-2 moves, then 3571 (+164325, begin (-2384, 444)) and 3581 (+164703, point
// (-2250, 1233)) - a distance of exactly 800, Fireball's own MaxR.
//
// So the approach is the SERVER's, and that is what this file builds. It reuses the
// click-to-walk machinery instead of a second one: `Handler.Plan` (zone\nav.go, the
// path engine or the map's walk graph) chooses the point, `walk` and `chaseTick`
// (zone\chase.go) run the clock, `BuildMovePCReason` sends ONE move, and the same
// poll tick that opens a dialog fires the cast instead. Nothing here blocks: the walk
// is the same non-blocking chase, wantsTick already wakes the socket for it, and it
// is dropped by the same events that drop a chase (a click, a map change, a warp).
//
// WHAT THIS FILE DOES NOT DO:
//   - It does not touch the damage formula, the stance or skill lists, or the wide
//     (3581) / aimed (3573) builders. When the walk ends it calls the SAME handlers
//     the in-range path calls, so the answer is the in-range answer by construction -
//     and like the in-range answer it is now the 3571 only: the result waits for the
//     cast's own clock (zone\skill_cast_clock.go).
//   - It does not move a point that is TOO CLOSE (distance < MinR). Walking backwards
//     to cast is not a behaviour any capture shows, so that press keeps exactly the
//     answer it had (logged, applied unless -strictrange).
//   - It invents no ground: a stand point that the path engine cannot put on the mesh,
//     or that ends up outside the skill's range anyway, means ONE log line and
//     nothing sent. A walk to a guessed point is a falling character.

const (
	// castStandFraction is how far inside the skill's own MaxR the stand point is
	// put. The reference's one measured stand point was 175 units from its target
	// (well inside), so the capture does not say where in the range to aim; this
	// side aims as close to the boundary as is still safe, because that is the
	// shortest walk and a player presses a skill the moment it will reach. The 10%
	// margin is what absorbs the last few units: the client stops a little short of
	// a destination and a cell is a whole number.
	castStandFraction = 0.9
)

// pendingCast is the client's own request, kept while its walk runs, so the cast
// that fires when the walk ends is the SAME request that was refused a moment ago -
// same frame, same offsets, same handler.
type pendingCast struct {
	frame  protocol.Frame // replayed verbatim: the handler parses it again
	ground bool           // 3647 (a point) rather than 3646 (a monster)

	skill  uint32 // for log lines only; the handler re-reads both from the frame
	target uint32

	pos  [3]int32 // the target's position, or the client's point
	dist float64  // how far the press was made from

	// phase is WHERE this cast is in its life, and it is the ONLY thing a 3598
	// CZ_MOVE_PC is judged by (R89 - see castPhase below). It starts as
	// castPhaseApproaching and turns to castPhaseAnimating when the 3571 goes out.
	phase castPhase
}

// castPhase is where a pending cast is in its life: the two halves of the CANCEL
// WINDOW (R89), and nothing else.
//
// THE BOUNDARY IS THE 3571, and it is measured rather than chosen:
//
//   - BEFORE it the character is in the reference's AS_SKILL_CHASE, and a move
//     KILLS the cast. `CPlayerChar::DispatchMove()` ends with
//     `SetMoveState(MS_MOVE); if (GetActionState() != AS_PEACE) SetActionState(AS_NONE);`
//     (PlayerChar_Request.cpp:363-365), and AS_SKILL_CHASE is NOT in
//     `IsHoldAction()` (FieldChar.cpp:1023-1049), so a move is accepted and the
//     chase state is thrown away. The live server agrees byte for byte: the third
//     cast of `captures\cancel20261031\20261031-191902` is a 3647 press, a reason-2
//     3548 (33907.8), the player's 3598 at 34160.0, the echo of it - and then NO
//     3571 and NO 3581 at all (history\cancel-and-animation.md §4).
//   - FROM it the body is in AS_SKILL_PREPROCESS, AS_SKILL_CASTING or AS_SKILL_USE,
//     all three of which ARE `IsHoldAction()`, so `DispatchMove()` returns early
//     WITHOUT clearing the request: the move is kept, newest wins, and the cast
//     still lands. The same capture's first cast shows the caster's own clicks
//     between the 3571 and its 3581 never echoed, and landing anyway (§2, §6.4).
//
// So a 3571's WIND-UP belongs to ANIMATING, together with the result and the hold
// after it. "Mid flight" is the approach walk alone.
type castPhase int

const (
	// castPhaseNone is a cast that never started, or one that has been cancelled.
	castPhaseNone castPhase = iota

	// castPhaseApproaching is R87's walk into range: nothing has been sent for the
	// skill yet, and a move click cancels the whole cast.
	castPhaseApproaching

	// castPhaseAnimating is the 3571 (or, for a CastTime 0 row, the result) and the
	// hold that follows it: a move click is queued and the cast completes.
	castPhaseAnimating
)

func (p castPhase) String() string {
	switch p {
	case castPhaseApproaching:
		return "approaching"
	case castPhaseAnimating:
		return "animating"
	}
	return "none"
}

// castStandoff is the distance from the target this side wants a caster to stand at,
// read out of the skill's own row and never out of a number in this file.
//
// MaxR <= 0 means the row states no maximum, so there is no far side to walk in from
// and this answers 0 (the caller then logs and sends nothing - it is not this lane's
// case, see the header).
func castStandoff(row SkillRow) float64 {
	if row.MaxR <= 0 {
		return 0
	}
	stand := row.MaxR * castStandFraction
	if row.MinR >= stand {
		// A narrow band (or one that starts high): the only point that is not on a
		// bound is the middle. The caller re-checks the answer either way.
		stand = (row.MinR + row.MaxR) / 2
	}
	return stand
}

// approachCast starts the walk that a refused cast asked for, and answers the ONE
// move packet (or nothing, with one log line saying why).
//
// `word` is the log prefix of the lane that called it: "SKILL" or "SKILL_GROUND".
func (h *Handler) approachCast(sess *Session, caster int, pc pendingCast,
	row SkillRow, word string) [][]byte {

	tag := "zone: " + word

	// THE PHASE IS RECORDED ON THE REQUEST ITSELF, not read back out of the walk: a
	// 3598 CZ_MOVE_PC has to know which half of the cancel window this cast is in, and
	// the request is the one place that survives the whole of it.
	pc.phase = castPhaseApproaching

	// ONE PENDING CAST, NEVER A QUEUE. A second press means the player changed their
	// mind, exactly as a second click on an NPC replaces the walk to the first one
	// (chase.go, queueClick): the old walk is dropped and the new press plans from
	// where the character stands now. Without this a laggy player would stack casts
	// and get one hit per press from the same spot.
	//
	// A cast already on its CLOCK is dropped here too, and for the same reason: the
	// player pressed again, so the cast they pressed before is not the one they mean
	// (R87's rule, and zone\skill_cast_clock.go's startCast says the same when the two
	// presses are both in range).
	h.dropCast(sess, caster, "a second press replaced it")
	// THIS CHARACTER'S OWN WALK, NEVER THE TEAM'S (R131): a cast approach belongs to the
	// character that pressed it, so a second press here replaces THAT walk and leaves every
	// other character's approach running.
	if cur := sess.approachOf(caster); cur != nil {
		h.Log.Printf("%s: a new press replaces the walk to %s (%d, %d, cell %d) - the "+
			"pending cast it was walking for is dropped (this character's own walk, and "+
			"only its own - R131)", tag, cur.describe(),
			cur.goal[0], cur.goal[1], cur.goal[2])
	}

	if h.Plan == nil {
		h.Log.Printf("%s OUT OF RANGE: %g away, range MinR %g .. MaxR %g, but this zone "+
			"has no path engine (Handler.Plan is nil), so it cannot name a stand point "+
			"that is real ground - nothing sent", tag, pc.dist, row.MinR, row.MaxR)
		return nil
	}

	now := time.Now()
	// WHERE THE CHARACTER STANDS, by the walked-position rule: for the leader that is
	// walkedPos, for a follower its own open segment (zone\move.go, charPos). Aiming
	// from the last click's DESTINATION instead would plan a walk from a place the
	// body has not reached.
	from := sess.charPos(caster, now)
	stand := castStandoff(row)
	goal, route, routeLen, overlap, ok, err := h.Plan(from, pc.pos, stand)
	switch {
	case err != nil:
		h.Log.Printf("%s OUT OF RANGE: %g away, range MinR %g .. MaxR %g, and the path "+
			"engine failed (%v) - nothing sent", tag, pc.dist, row.MinR, row.MaxR, err)
		return nil
	case !ok:
		h.Log.Printf("%s OUT OF RANGE: %g away, range MinR %g .. MaxR %g, and the "+
			"character's own position (%d, %d, cell %d) is not valid ground to walk "+
			"from - nothing sent", tag, pc.dist, row.MinR, row.MaxR,
			from[0], from[1], from[2])
		return nil
	case overlap:
		// The clamp was shortened into the target itself. Standing inside a monster is
		// not ground, and the distance there is 0 - inside MinR for every skill that
		// states one, and a cast from a body overlapping its target for the rest.
		h.Log.Printf("%s OUT OF RANGE: %g away, range MinR %g .. MaxR %g, and no clear "+
			"stand point exists on the target's side (the path engine clamped into the "+
			"target itself) - nothing sent", tag, pc.dist, row.MinR, row.MaxR)
		return nil
	}

	// THE POINT MUST BE INSIDE THE SKILL'S RANGE. The plan answers ground, not range:
	// the clamp can shorten a request past the near bound, and a worker that ignored
	// the stand-off argument would answer the target itself. Both are caught here,
	// with the skill's own numbers, before anything is sent.
	standDist, inRange := skillRangeAllowed(goal, pc.pos, row)
	if !inRange {
		h.Log.Printf("%s OUT OF RANGE: %g away, range MinR %g .. MaxR %g, and the only "+
			"stand point the path engine offers (%d, %d, cell %d) is %g from the target "+
			"- still out of range, so nothing sent", tag, pc.dist, row.MinR, row.MaxR,
			goal[0], goal[1], goal[2], standDist)
		return nil
	}

	speed := h.packetSpeed(&sess.Team[caster])
	if speed <= 0 {
		speed = MoveBaseSpeed
	}
	walked := routeLen
	if walked <= 0 {
		walked = math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
	}
	eta := time.Duration(walked / speed * float64(time.Second))

	installed := &walk{
		from: from, goal: goal, eta: eta, began: now, sentAt: now,
		speed: speed, caster: caster, cast: &pc, clamped: true, route: route,
		// THE TARGET OF THE CAST, for the log line the arrival writes. near stays 0: the
		// cast's own band is the SKILL's MinR..MaxR, and firePendingCast re-checks it on
		// the body's live point when it fires (skill_approach.go) - one question, one ask.
		target: pc.pos,
	}
	sess.addApproach(installed)
	// A reported segment the character left open must not close in the middle of this
	// walk and stop the far actor half way (chase.go, startChase does the same).
	//
	// RECORDED FIRST (R91 round 7): placeWalker only writes the goal when the body
	// reaches it, so dropping a FOLLOWER's segment here would leave the model on the
	// point an earlier walk last wrote, and the cast's own position would be read from
	// it. rememberWalk returns at once for the leader, so slot 0 is unchanged.
	sess.rememberWalk(caster, now)
	sess.closeWalk(caster)

	pkt, handle := h.legMoveFor(sess, caster, goal, ReasonSkill)
	if pkt == nil {
		// A WALK TO THE SAME STAND POINT IS ALREADY IN FLIGHT (zone\walk_once.go), so
		// legMoveFor sent nothing. the walk installed above is this cast's own walk, so the cast
		// still fires when that walk's clock ends - only the duplicate move is gone.
		return nil
	}
	h.broadcastMove(sess, caster, handle, goal, pkt)

	h.Log.Printf("%s APPROACH: commander %d char %d (%d) skill %d (%q) target %d at "+
		"(%d, %d, cell %d) is %g away and the range is MinR %g .. MaxR %g - walking "+
		"%.0f units to (%d, %d, cell %d), %g from the target, eta %.1fs at %.0f u/s "+
		"(cmd %d, %d B, reason %d); the cast fires when the walk ends",
		word, sess.CommanderID, caster, sess.Team[caster].Handle, pc.skill,
		h.SkillName(pc.skill), pc.target,
		pc.pos[0], pc.pos[1], pc.pos[2], pc.dist, row.MinR, row.MaxR,
		walked, goal[0], goal[1], goal[2], standDist, eta.Seconds(), speed,
		CmdZCMovePC, len(pkt), ReasonSkill)

	return [][]byte{pkt}
}

// firePendingCast replays the client's own request once the character is in range.
//
// It calls the SAME handlers the in-range path uses, with `castAfterWalk` set, so
// the packets, their order and their log lines are the in-range ones by
// construction: the 3571 now, and the result when the cast's own clock fires
// (zone\skill_cast_clock.go). `castAfterWalk` exists for ONE reason: if the re-run
// finds the target out of range again - a table row that changed under us, a target
// that moved - it must say so and send nothing rather than start a second walk for
// ever.
func (h *Handler) firePendingCast(sess *Session, pc *pendingCast) [][]byte {
	var out [][]byte
	var err error
	if pc.ground {
		out, err = h.castSkillGroundAt(sess, pc.frame, castAfterWalk)
	} else {
		out, err = h.castSkillAt(sess, pc.frame, castAfterWalk)
	}
	if err != nil {
		h.Log.Printf("zone: SKILL APPROACH: the cast at the end of the walk failed: %v - "+
			"nothing sent", err)
		return nil
	}
	// THE PHASE TURNS HERE, on the one event the reference's own state machine turns on:
	// the 3571 has gone out (or, for a CastTime 0 row, the result has), so from this
	// instant a move click is kept instead of cancelling. A replay that sent nothing
	// has not started animating anything, so it does not turn the phase.
	if len(out) > 0 {
		h.setCastPhase(pc, castPhaseAnimating)
	}
	return out
}

// setCastPhase moves a pending cast to its next phase and logs the ONE line that
// change is worth. It says nothing when the phase does not change, so a replay that
// refused to cast keeps its own single refusal line instead of a false transition.
func (h *Handler) setCastPhase(pc *pendingCast, to castPhase) {
	if pc == nil || pc.phase == to {
		return
	}
	from := pc.phase
	pc.phase = to
	h.Log.Printf("zone: cast phase %s -> %s", from, to)
}

// cancelApproachCast is the CANCEL WINDOW's first half (R89): a `3598 CZ_MOVE_PC`
// from the character that is still WALKING to its cast cancels the whole cast.
//
// It answers true when it cancelled something, and the caller then goes on to handle
// the move in the ordinary way - the echo, the broadcast, a fresh walk segment, and
// the stop at that segment's end - which is EXACTLY the click-to-walk path, so the
// character walks to the clicked point with no second mover added. Nothing is sent
// for the skill: no 3571, no 3573, no 3581, and no cancel packet either. The reference
// sends no cancel packet for this, in either direction
// (history\cancel-and-animation.md §4/§5).
//
// The approach's own reason-2 move is dropped with it, and the segment the client had
// left open goes too - the same two things `cancelChase` drops for an NPC click. No
// ZC_STOP_PC is sent: the click that cancelled it opens its OWN segment, and that
// segment's end sends the only stop this body is owed (zone\move.go, endWalks). A stop
// here would name the stand point the body has just been told to abandon.
func (h *Handler) cancelApproachCast(sess *Session, idx int) bool {
	w := sess.approachOf(idx)
	if w == nil || w.cast == nil ||
		w.cast.phase != castPhaseApproaching || w.castDone {
		return false
	}
	pc := w.cast
	elapsed := time.Since(w.began)
	pc.phase = castPhaseNone
	h.Log.Printf("zone: cast CANCELLED during the approach by a move click (nothing sent) "+
		"- char %d (%d) was %s into the walk to (%d, %d, cell %d) for skill %d (%q) "+
		"target %d, and the click for the new point cancels the whole cast: no %d, no "+
		"result, no cancel packet (history\\cancel-and-animation.md §4)",
		w.caster, sess.Team[idx].Handle, elapsed.Round(time.Millisecond),
		w.goal[0], w.goal[1], w.goal[2], pc.skill, h.SkillName(pc.skill), pc.target,
		CmdZCSkillCasting)
	sess.clearApproach(w)
	// Same recording as the attack's cancel (R91 round 7): the click's own move opens
	// its segment from sess.drawnPos, so the follower's live point is written before
	// this one is dropped.
	sess.rememberWalk(idx, time.Now())
	sess.closeWalk(idx)
	return true
}

// placeWalker records that the walking character is now AT goal. The leader also has
// the session's own segment fields (Pos, moveFrom, moveAt) - the gate and the click
// range read those - and a follower only has its own stored position.
func (h *Handler) placeWalker(sess *Session, caster int, goal [3]int32) {
	if caster == 0 {
		sess.Pos = goal
		sess.moveFrom, sess.moveAt = goal, time.Now()
	}
	if caster >= 0 && caster < len(sess.Team) {
		sess.Team[caster].Position = goal
	}
}

// stopWalker is stopOthers for whichever character walked: the leader keeps the
// existing path unchanged, and a follower gets the same stop every follower's
// reported walk already gets (move.go, endWalks). The stop RULE - the ZC_STOP_PC a
// far client needs, and its pad - is untouched; only the handle it names is chosen.
func (h *Handler) stopWalker(sess *Session, caster int, goal [3]int32) {
	if caster <= 0 || caster >= len(sess.Team) {
		h.stopOthers(sess, goal)
		return
	}
	handle := sess.Team[caster].Handle
	h.broadcastStop(sess, caster, handle, goal, BuildStopPC(handle, goal))
	// NO rememberWalk (R91 round 7): this stop NAMES goal and placeWalker has just
	// written goal into Team[caster].Position, so the body's live point is already
	// recorded. Adding a walked point here could only put a different point back.
	sess.closeWalk(caster)
}
