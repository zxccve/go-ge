package zone

import (
	"math"
	"time"
)

// bodyPoint is ONE measurement of an attacking character's body: the point, the
// SEGMENT that point was read from with how long that segment has been running, and the
// moment that leg's own clock started.
//
// THE POINT AND THE SEGMENT TRAVEL TOGETHER ON PURPOSE (R104). The swing's 3564
// hard-sets the client's own actor to the position at +8 (GeActor.cpp:309-311), so a
// point taken from one model and printed beside a segment taken from another is
// exactly the jump the owner reports: logs\go_zone1331.log, 19:13:21.181 printed
// `pos (2392, -770) from segment (2605, -1299) -> (2997, -1170)` - and 2392,-770 is
// the DESTINATION of the client's move report two lines later, ~600 units from where
// that segment had drawn the body. Moving the actor there is a teleport on screen.
type bodyPoint struct {
	pos     [3]int32
	from    [3]int32
	dest    [3]int32
	at      time.Time // the moment this leg's own clock started; zero = no leg
	elapsed time.Duration

	// rewind is set when this reading was BEHIND the one before it on the SAME leg, and
	// pos was then held at the previous point. next is the point it would have said,
	// prev/prevAt the point and time of the one before it, so the one log line
	// (attack.go, SWING POS REWIND) can name both. One leg, one clock (R111): the flag is
	// the alarm, never the cure.
	rewind bool
	next   [3]int32
	prev   [3]int32
	prevAt time.Time
}

// swingLeg is ONE leg a character's body is drawn on, with everything needed to put the
// body on it at any instant: the ends, the moment its clock started, and - for a walk
// this server sent - the planned polyline and the speed the client animates.
//
// IT EXISTS SO ONE LEG HAS ONE CLOCK (R111). attackBodyPoint (the position a swing sends)
// and swingSegment (the segment the SWING POS line names) used to decide this separately,
// and a reading that fell back to the model's stored point named the same segment as the
// swing before it while restarting the clock - 841 ms backwards in one engagement:
//
//	17:28:25.288310 SWING POS char 1 (1794) pos (2405, -718) ... elapsed 2346ms
//	17:28:27.196082 SWING POS char 1 (1794) pos (2339, -733) ... elapsed 1505ms
//
// Both decide through swingLegOf now, and both put the body on the leg with legPoint, so
// two readers of one leg cannot disagree by one unit, let alone by 68.
type swingLeg struct {
	from, dest [3]int32
	at         time.Time
	route      [][3]int32 // the planned polyline of a walk, or nil for the straight chord
	speed      float64
	placed     bool // no leg at all: the body stands on dest
}

// swingLegOf answers THE leg character idx's body is drawn on: the walk this server sent
// when one owns this character, and the leg the client's own last report opened otherwise.
//
// TWO SEGMENTS CAN BE OPEN ON ONE CHARACTER, and only one of them is where the body is on
// screen:
//
//   - the walk THIS SERVER sent (sess.Walk: a normal attack's approach, or a cast's).
//     The client animates exactly that segment at the speed carried in the 3548, so
//     the body at `now` is walkedRouteAt(w.route, w.from, w.goal, w.began, now, w.speed)
//     - along the walk's own ROUTE when it has one, and along the straight chord when it
//     does not. It is NOT the stand point: the body reaches that only when the segment is
//     walked out.
//   - the segment the CLIENT's own last move report opened (moveFrom -> Pos for the
//     slot the player drives, walks[idx] for a follower). clientLegOf answers that one.
//
// A THIRD RECORD covers the moment after the walk is dropped: a walk that has WALKED OUT
// hands its leg to keptLegAt (keepWalkedLeg), so the swing after it counts the same clock
// instead of falling back to the point an old move report declared 800 units behind. That
// record is read ONLY while the body still stands on its far end (keptLegOf): once anything
// newer moves the body, the leg is history, not a leg (R112).
//
// NOTHING IS WRITTEN HERE: this is a read, so no fire path can move the model as a
// side effect of firing.
func (s *Session) swingLegOf(idx int, speed float64) swingLeg {
	if w := s.approachOf(idx); w != nil {
		spd := w.speed
		if spd <= 0 {
			spd = speed
		}
		return swingLeg{from: w.from, dest: w.goal, at: w.began, route: w.route,
			speed: spd}
	}
	return s.clientLegOf(idx, speed)
}

// clientLegOf answers the leg the CLIENT's own last move report opened.
//
// The leader's own segment is moveFrom -> Pos, opened by its latest move
// report (movePC). charPos reads it through walkedPos and IGNORES s.walks[0]
// for idx 0, so this does the same: two shapes for one rule is how a leader
// and a follower would start disagreeing.
// SENSE: "the character the player drives", not "slot 0". The session's one
// segment (moveFrom -> Pos) belongs to that character, so after a
// CZ_SELECTCHANGE_PC it is slot Control's, and slot 0 is an ordinary follower
// read from walks[0] below.
func (s *Session) clientLegOf(idx int, speed float64) swingLeg {
	if idx == s.Control {
		if s.moveAt.IsZero() {
			// No open segment: this character was PLACED (a spawn, a town warp, a
			// finished chase) - or it walked a leg this server sent and then the walk
			// record was dropped. The kept leg is the second case and it still owns a
			// clock; a placed body has none, so Pos is the truth.
			if l := s.keptLegOf(idx); l != nil {
				return l.leg()
			}
			return swingLeg{from: s.Pos, dest: s.Pos, placed: true}
		}
		// AND THE LEG'S ROUTE TRAVELS WITH IT (2026-11-02): movePC plans the polyline once
		// when the leg opens (moveRoute), and every reader draws the body along it.
		return swingLeg{from: s.moveFrom, dest: s.Pos, at: s.moveAt, route: s.moveRoute,
			speed: speed}
	}
	if idx < 0 || idx >= len(s.Team) {
		return swingLeg{placed: true}
	}
	// A follower's segment is the one its own move report opened.
	if idx < len(s.walks) {
		if w := s.walks[idx]; !w.at.IsZero() {
			return swingLeg{from: w.from, dest: w.dest, at: w.at, route: w.route,
				speed: speed}
		}
	}
	if l := s.keptLegOf(idx); l != nil {
		return l.leg()
	}
	// A placed body keeps its stored point: no leg, so no residual either (R107).
	return swingLeg{from: s.Team[idx].Position, dest: s.Team[idx].Position, placed: true}
}

// legPoint is THE ONE arithmetic that puts a body on a leg at `now`: along the walk's
// route when it has one that matches the leg, along the chord otherwise, never before the
// leg's start and never past its goal.
//
// drawnPos, attackBodyPoint and swingSegment all come through here, which is what makes
// them agree by construction instead of by two functions that were written to match.
//
// NO CLIENT RESIDUAL IS ADDED ANY MORE (2026-11-02). There is nothing left to correct:
// a leg now STARTS on the point this function had already drawn the body on (movePC,
// Handler.clientRoute), so the drawn position cannot step when a report arrives, and the
// residual that used to carry such a step at walking pace (R107, zone\walk_correction.go)
// has been deleted with it.
func (s *Session) legPoint(leg swingLeg, now time.Time) [3]int32 {
	if leg.placed {
		return leg.dest
	}
	return walkedRouteAt(leg.route, leg.from, leg.dest, leg.at, now, leg.speed)
}

// legOwnTime is how long the leg ITSELF takes to walk: its route's length (or its chord)
// at the speed the client animates. It is the END of that leg's clock, not a timeout.
//
// WHY THE ELAPSED MAY NOT RUN PAST IT (R112, measured 2026-11-02 18:22:49). A walk this
// zone sent ends on its own clock and hands its leg to keptLegAt with the clock it already
// had (R111) - and nothing stopped that clock counting. A 77-unit step-back (eta 164 ms)
// was still being named by every swing 54 seconds later: `SWING POS char 2 (1793) pos
// (2621, -816, cell 108) from segment (2622, -739) -> (2621, -816) elapsed 54453ms`. The
// POSITION was already held on the leg's far end by walkedRouteAt; only the clock ran on,
// so the line said a body had been walking 77 units for 54 seconds. One leg's clock stops
// where its body does, and the SWING POS line can then never name a leg older than that
// leg's own walk time.
func legOwnTime(leg swingLeg) time.Duration {
	length := legLength(leg.from, leg.dest, leg.route)
	if length <= 0 {
		return 0
	}
	spd := leg.speed
	if spd <= 0 {
		spd = MoveBaseSpeed
	}
	return time.Duration(length / spd * float64(time.Second))
}

// legElapsed is THE age of a leg at `now`: never before the leg's own start (a caller with
// an earlier clock must not walk backwards) and never past the leg's own walk time (a body
// held on the leg's far end has finished that leg).
//
// attackBodyPoint and swingSegment both read it, so the SWING POS line and the point beside
// it cannot disagree about the age either - the same reason both decide their leg through
// swingLegOf.
func legElapsed(leg swingLeg, now time.Time) time.Duration {
	if leg.placed || leg.at.IsZero() {
		return 0
	}
	age := now.Sub(leg.at)
	if age < 0 {
		return 0
	}
	if own := legOwnTime(leg); age > own {
		return own
	}
	return age
}

// attackBodyPoint answers where character idx's body is DRAWN at `now`, and which
// segment that answer came from. `now` is the moment the caller is BUILDING its packet
// (attackAt reads it once per pass and hands it down); a caller that does not have that
// instant must read the current time, never re-use an older one.
func (s *Session) attackBodyPoint(idx int, now time.Time, speed float64) bodyPoint {
	leg := s.swingLegOf(idx, speed)
	bp := bodyPoint{
		pos:  s.legPoint(leg, now),
		from: leg.from,
		dest: leg.dest,
		at:   leg.at,
	}
	if !leg.placed {
		bp.elapsed = legElapsed(leg, now)
	}
	return s.legDoesNotRewind(idx, leg, bp, now)
}

// drawnPos is where the CLIENT has already DRAWN character idx, by the same
// segment arithmetic charPos uses, but run at the speed the client was TOLD
// rather than at the speed the server measured.
//
// WHY THIS EXISTS (R91). The attack packet 3564 hard-sets the attacker's own
// actor to the position at +8 on every swing
// (source2/geClient/Source/GeActor.cpp:309). A move packet is a walk, and a stop
// is built for OTHER players, so for a solo player the ATTACK is the only packet
// that can move their own body. The number in it is therefore ours, and it used
// to be charPos: the position extrapolated at walkSpeed() - the MEASURED band
// 250..800 (zone.go). The client, meanwhile, is animating the walk at the speed
// the server itself sent it, Handler.packetSpeed: the character's own MSPD, ~469
// with no gear. A measured 800 therefore put the body up to 331 units ahead of
// the body on screen after one second, on every swing, and the client obeys the
// packet - the character snaps forward. Same segment, same start, the speed the
// screen is using: no snap.
//
// speed is passed in, not read from the session, so the caller states which
// clock it wants and this file owns no policy. The clamp is walkedToAt's own:
// never before the segment's from, never past its dest, and a zero-length
// segment reads as arrived.
//
// IT IS THE CLIENT'S OWN LEG AND NOTHING ELSE (movePC's move guard asks this while a
// walk of ours may also be open), so it reads clientLegOf - the same leg, the same
// arithmetic, from the same two functions attackBodyPoint uses.
func (s *Session) drawnPos(idx int, now time.Time, speed float64) [3]int32 {
	return s.legPoint(s.clientLegOf(idx, speed), now)
}

// walkedLeg is a leg that has been WALKED OUT, remembered after its walk record is dropped:
// the ends, the moment its own clock started, the speed the client animated it at, and the
// planned polyline when the walk had one.
//
// THE ROUTE TRAVELS WITH IT (R110 meets R111). A bent walk draws the body along its route,
// so a leg remembered without one would be redrawn on the chord the moment the walk record
// went - a jump of exactly the bend, which is the defect R110 removed. And the guard
// measures "how far along the leg" along the ROUTE, so the reading before the handover and
// the reading after it must be measured on the same geometry or the alarm would fire on a
// body that never moved.
type walkedLeg struct {
	from, dest [3]int32
	at         time.Time
	speed      float64
	route      [][3]int32
}

// keptLegAt is character idx's remembered leg, grown to this team's size on first use. It
// answers nil for a slot this team does not have, exactly as lastAttack does.
func (s *Session) keptLegAt(idx int) *walkedLeg {
	if idx < 0 {
		return nil
	}
	if len(s.keptLegs) <= idx {
		grown := make([]walkedLeg, len(s.Team))
		copy(grown, s.keptLegs)
		s.keptLegs = grown
		if idx >= len(s.keptLegs) {
			return nil // a slot this team does not have
		}
	}
	return &s.keptLegs[idx]
}

// keptLegOf is keptLegAt as a read: nil when this character has no remembered leg, and nil
// when the body is no longer standing on that leg's far end.
//
// A REMEMBERED LEG IS THE BODY'S LEG ONLY WHILE THE BODY STILL STANDS ON ITS END
// (R112, measured 2026-11-02 18:22). keepWalkedLeg writes BOTH the body's stored point and
// the leg, so the two agree at the handover. Everything that moves the body afterwards -
// the client's own move report, its walked-out leg's stop (zone\move.go, endWalks), a
// placement - writes the stored point and not the leg, and from that instant the remembered
// leg describes ground the body has left. Reading it anyway IS the owner's "still jump": a
// 77-unit step-back (eta 164 ms, walkDone 480 ms) was still being named by every swing 54
// seconds later, and each of those swings told the client to put the warlock back on
// (2621, -816) - 900+ units from where the client was walking it:
//
//	18:22:31.152629 MOVE_PC char 2 handle 1793 -> (1783, -698) cell 34 | model was at
//	(2621, -816) 751 u off, leg 100 u ...
//	18:22:49.688481 SWING POS char 2 (1793) pos (2621, -816, cell 108) from segment
//	(2622, -739) -> (2621, -816) elapsed 54453ms
//
// The stored point is the ONE answer for "where does this body stand when no leg runs"
// (clientLegOf's placed branch), so the leg is checked against it and nothing else. No
// second flag, and no second place for the same fact.
func (s *Session) keptLegOf(idx int) *walkedLeg {
	if idx < 0 || idx >= len(s.keptLegs) {
		return nil
	}
	l := &s.keptLegs[idx]
	if l.at.IsZero() {
		return nil
	}
	if l.dest != s.stoodPoint(idx) {
		return nil // the body has been moved since the handover: the leg is history
	}
	return l
}

// stoodPoint is where this character's body STANDS when no leg is running: the session's
// own point for the character the player drives, and its stored position for a follower -
// the same two answers clientLegOf's placed branch gives, so the check above and the
// fallback below cannot disagree about where the body is.
func (s *Session) stoodPoint(idx int) [3]int32 {
	if idx == s.Control {
		return s.Pos
	}
	if idx < 0 || idx >= len(s.Team) {
		return [3]int32{}
	}
	return s.Team[idx].Position
}

// keepWalkedLeg hands a walk that has just WALKED OUT to the two records every other
// reader in this zone uses: the body's own stored point follows the goal, and the leg
// itself is remembered with ITS OWN clock.
//
// WHY (R111, measured). When an attack approach's clock ran out, chaseTick dropped the
// walk and wrote nothing about the body. The stored point was therefore still the one an
// old move report had declared, 808 units behind where the walk had just drawn the body -
// and the next reading fell back to it, so the repeat measured 932 of clear ground,
// walked the whole leg again from that stale point with a FRESH clock, and the swing
// reported 741 units along a leg the previous swing had already reported walked out. The
// client obeys the swing's position (GeActor.cpp:300-309), so that is the jump on screen:
//
//	17:28:25.288310 ... pos (2405, -718) elapsed 2346ms
//	17:28:27.196082 ... pos (2339, -733) elapsed 1505ms
//
// The walk's own clock is the leg's clock, so keeping (from, goal, began) is what makes
// the next reading COUNT it (2748 ms, then 4254 ms) instead of starting a new one. The
// route is not carried: a leg is only handed over once it is walked out, and a walked-out
// route and its chord both end on the same goal.
func (s *Session) keepWalkedLeg(w *walk) {
	if w == nil {
		return
	}
	idx := w.caster
	if idx >= 0 && idx < len(s.Team) {
		s.Team[idx].Position = w.goal
	}
	if idx >= 0 && idx == s.Control {
		s.Pos = w.goal
	}
	if l := s.keptLegAt(idx); l != nil {
		*l = walkedLeg{from: w.from, dest: w.goal, at: w.began, speed: w.speed, route: w.route}
	}
}

// leg is a remembered leg as the swing reads it: the same shape a live walk has, route and
// all, so the reading either side of the handover is the same leg drawn the same way.
func (l *walkedLeg) leg() swingLeg {
	return swingLeg{from: l.from, dest: l.dest, at: l.at, route: l.route,
		speed: l.speed}
}

// swingMark is the last body point one character's swing reported, with the leg it was
// read from - the guard's own memory.
type swingMark struct {
	from, dest [3]int32
	at         time.Time
	pos        [3]int32
	now        time.Time
	warned     bool
}

// swingMarkAt is character idx's guard record, grown like lastAttack.
func (s *Session) swingMarkAt(idx int) *swingMark {
	if idx < 0 {
		return nil
	}
	if len(s.swingMarks) <= idx {
		grown := make([]swingMark, len(s.Team))
		copy(grown, s.swingMarks)
		s.swingMarks = grown
		if idx >= len(s.swingMarks) {
			return nil
		}
	}
	return &s.swingMarks[idx]
}

// legDoesNotRewind is THE INVARIANT, as code: within ONE leg, the position a swing reports
// must never move BACKWARDS along that leg (R111).
//
// A leg is identified by its ends AND the moment its clock started, so a genuine new leg -
// a new walk, a new move report, a resync that starts somewhere else - is never held back:
// it gets a fresh record. What the guard catches is the case that produced the 841 ms
// regression: a reader that describes the same leg while handed a time that does not
// belong to it. The point is held at the previous one (so the screen does not jump) and
// the first occurrence on that leg is logged once by attackAt, with both points and both
// times - the line is the regression alarm; the fix is that the leg has one clock, not
// this function.
func (s *Session) legDoesNotRewind(idx int, leg swingLeg, bp bodyPoint,
	now time.Time) bodyPoint {
	m := s.swingMarkAt(idx)
	if m == nil || bp.dest == bp.from {
		// No leg, or a leg of no length: there is nothing to run backwards along.
		return bp
	}
	same := m.at.Equal(bp.at) && m.from == bp.from && m.dest == bp.dest
	if !same {
		*m = swingMark{from: bp.from, dest: bp.dest, at: bp.at, pos: bp.pos, now: now}
		return bp
	}
	// THE MEASURE IS THE LEG'S OWN: along the route for a walk that has one, along the
	// chord otherwise. Comparing two points of a bent walk by their distance from the
	// start would read a corner as a step backwards, and one int32 unit of truncation
	// (walkedToAt's rounding) is not a step either.
	if legAlong(leg, bp.pos) >= legAlong(leg, m.pos)-1 {
		m.pos, m.now = bp.pos, now
		return bp
	}
	if !m.warned {
		m.warned = true
		bp.rewind, bp.prev, bp.prevAt, bp.next = true, m.pos, m.now, bp.pos
	}
	bp.pos = m.pos
	return bp
}

// legAlong is HOW FAR ALONG a leg one point is, measured on the leg's own geometry: the
// polyline distance when the leg has a route, the chord distance when it does not. It is
// the guard's ruler, so a body that walks a bent approach is measured along the path it
// actually walks - distances from the start would call a corner a step backwards.
func legAlong(leg swingLeg, p [3]int32) float64 {
	if len(leg.route) < 2 {
		return flatDist(leg.from, p)
	}
	best, bestAlong, travelled := math.Inf(1), 0.0, 0.0
	for i := 1; i < len(leg.route); i++ {
		ax, ay := float64(leg.route[i-1][0]), float64(leg.route[i-1][1])
		dx := float64(leg.route[i][0]) - ax
		dy := float64(leg.route[i][1]) - ay
		seg := math.Hypot(dx, dy)
		if seg == 0 {
			continue // a zero-length corner is walked in no distance at all
		}
		px, py := float64(p[0])-ax, float64(p[1])-ay
		t := (px*dx + py*dy) / (seg * seg)
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		if off := math.Hypot(px-dx*t, py-dy*t); off < best-1e-9 {
			best, bestAlong = off, travelled+seg*t
		}
		travelled += seg
	}
	if math.IsInf(best, 1) {
		return flatDist(leg.from, p)
	}
	return bestAlong
}

// settleBodyAt puts a character's body where it stands at the moment a walk is CLOSED, so
// every later reading (a swing's +8, the reach check, the next plan) starts from the point
// the client was last told to stop on instead of the leg the walk never finished (R132).
//
// It writes BOTH records for the driven character - Pos and the client leg - and Team[] for
// any character, exactly as landClickArrival and keepWalkedLeg do, because a switch re-seats
// Pos from Team[] and the two must not disagree.
func (s *Session) settleBodyAt(idx int, at [3]int32) {
	if idx >= 0 && idx < len(s.Team) {
		s.Team[idx].Position = at
	}
	if idx == s.Control {
		s.Pos = at
		s.moveFrom, s.moveAt, s.moveRoute = at, time.Time{}, nil
	}
	if l := s.keptLegAt(idx); l != nil {
		*l = walkedLeg{}
	}
}

// pinStandingPointOnPress CLOSES the walk the body is on, at the point a press is answered
// from, so the rest of that walk can never carry a later swing (R129).
//
// THE DEFECT IT REMOVES, measured on the owner's own demos (2026-11-04, both in
// logs\\zone_go_live.log): a keep-mode press that lands MID-WALK is answered from where the
// body stands - `16:11:07.180 SWING POS char 0 (455) pos (2488, -1092) from segment (2964,
// -1054) -> (2240, -1112) elapsed 926ms` - but the leg stayed OPEN and kept being simulated,
// so the engagement's SECOND swing carried the leg's DESTINATION, `16:11:08.383 ... pos
// (2240, -1112) ... elapsed 1410ms`. The client had stopped the body where the press
// landed (no walk report in between), so that point was one the body never walked to, and
// the 3564 hard-sets the client's own actor to +8 (GeActor.cpp:309-311): the jump the owner
// calls "it jumps to the destination instead of standing where it started attacking".
// His second run, 16:15:03 and 16:15:06, shows the same signature twice.
//
// THE BODY IS PLACED, NOT MOVED. The record is rewritten to a leg of no length at p (from ==
// dest == p, no clock, no route) - the same shape a spawn or a finished chase has - and the
// remembered leg of an older walk is dropped, because clientLegOf reads that record when no
// segment is open and would otherwise hand a later swing a leg from before the press.
//
// p MUST BE THE POINT THE CALLER ALREADY MEASURED for its own swing: one press, one
// measurement (R111). It returns the destination it dropped, so the caller can say so in
// one line, and false when there was nothing to close.
func (s *Session) pinStandingPointOnPress(idx int, body bodyPoint) (dropped [3]int32, pinned bool) {
	if idx < 0 || idx >= len(s.Team) {
		return dropped, false
	}
	// NOTHING TO DROP WHEN THE LEG IS WALKED OUT: the body already stands on its end, so the
	// walk has nowhere left to carry a later swing - and the rewind guard's own fixture needs
	// such a leg to stay OPEN so a reading can be run backwards on it (R111).
	if body.pos == body.dest {
		return dropped, false
	}
	p := body.pos
	// A WALK THIS SERVER SENT IS NOT THIS FUNCTION'S TO CLOSE: an attack or cast approach
	// owns its own clock, its own arrival and its own ring (zone\\attack_approach.go,
	// zone\\skill_approach.go), and swingLegOf prefers it for the body for the same reason.
	if w := s.approachOf(idx); w != nil {
		return dropped, false
	}
	if idx == s.Control {
		if s.moveAt.IsZero() {
			return dropped, false // no client leg open: the body is already placed
		}
		dropped = s.Pos
		s.moveFrom, s.Pos = p, p
		s.moveAt, s.moveRoute = time.Time{}, nil
	} else {
		if idx >= len(s.walks) {
			return dropped, false // a follower this session keeps no client leg for
		}
		w := &s.walks[idx]
		if w.at.IsZero() {
			return dropped, false
		}
		dropped = w.dest
		w.from, w.dest = p, p
		w.at, w.route = time.Time{}, nil
	}
	s.Team[idx].Position = p
	if l := s.keptLegAt(idx); l != nil {
		*l = walkedLeg{}
	}
	return dropped, true
}
