package zone

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// The CHASE: walking the player to an NPC they clicked.
//
// The shape is NOT invented here. It was measured off the live server's own traffic
// and recorded in `history\zone-npc-click.md`, and everything below follows it:
//
//  1. **ONE move packet, destination = the NPC's own coordinates or a point clamped
//     out from it.** The live capture (`captures\granma`, 2026-10-22) shows a single
//     `MOVE pos=<the NPC's exact spot> spd=469 reason=4`. NO legs, no repeats - the
//     client walks the whole route itself in one smooth move. An earlier port split
//     the walk into legs and produced stop-go jitter; `GE_CHASE_HOP` now defaults to
//     0 for that reason. Do not re-introduce legs.
//  2. **The stop point is CLAMPED, not computed:** start at 140 units out from the NPC
//     (on the player's side) and walk back towards the NPC until the point is clear.
//     If the clamp is SHORTENED, something is pressed against the NPC and the client
//     refuses those poses - the live then sends the NPC's OWN position and lets the
//     client stop at collision distance (the 44 units measured). So an overlap is
//     NORMAL for those NPCs, not a bug.
//  3. **Speed is the stance run speed** (469 in the capture), and reason is
//     `MR_DIALOG` (4).
//  4. **A ground click cancels.** The live did not chase again afterwards; it only
//     echoed the player's move.
//  5. **The client reports NOTHING while it walks a server move**, so arrival is this
//     side's clock - and the clock must use the speed the character really walks at.
//
// What is left to this side, then, is one decision (which point) and one clock (when
// the dialog may open). `goserver\zone\nav.go` answers the point; the clock is below.
const (
	// MaxChase is the longest walk allowed WITHOUT a path engine, in world units.
	// Beyond it the destination is a guess this side cannot check, so nothing is sent.
	MaxChase = 900.0

	// MaxChaseWithRoute bounds a walk whose clamp came from the path engine. The client
	// paths it, so there is no straight-line risk; this is only a sanity bound.
	MaxChaseWithRoute = 4000.0

	// ChaseSpeed is the walk clock's fallback before the client's own speed has been
	// measured from its move reports (see Session.observeSpeed). It is the move-speed
	// formula's base, which is also the speed a bare character really walks at - see
	// zone\speed.go. The PACKET's speed is never this constant: it is the character's
	// own MSPD (Handler.packetSpeed).
	ChaseSpeed = MoveBaseSpeed

	// ChaseGiveUp is the ONE clock left around an approach: the budget a walk is given
	// to get there BEFORE it is written off. It is not the arrival - the arrival is the
	// body's own distance (chase.go, walk.arrived), re-tested every tick - it is the
	// honest fallback for a walk that CANNOT arrive: something the client collided with,
	// a body the path engine routed around but the client's own path refused.
	ChaseGiveUp = 6 * time.Second

	// ChaseGrace is the window after OUR move is sent during which a move report is
	// still the tail of the player's own walk, not a new instruction.
	ChaseGrace = 1500 * time.Millisecond

	// LegSafety is GONE from here: the chase's arrival is the body's own distance now
	// (walk.arrived, piece 2d). The stop that a PLAYER-DRIVEN leg owes the other players
	// still keeps its pads, because that leg has no body model at all: it is timed from
	// a report and a speed (move.go, charWalk.walkedOut). See the note there.

	// ArrivalEpsilon is how close the body's walked point must be to the walk's goal for
	// the walk to be OVER. It is one unit because there is nothing to absorb: the walked
	// point is the goal itself the moment the route is walked out (move.go,
	// walkedRouteAt), and the arithmetic truncates to whole units.
	ArrivalEpsilon = 1.0

	// ArriveRadius is how close a CLIENT REPORT to the goal counts as "arrived". A report
	// is a discrete point, not a body: the client stops at collision distance, so a
	// stand-off is reached within a few units and an overlap within about 44. 120 is
	// generous on purpose - the dialog's own near-test is 200.
	ArriveRadius = 120.0

	// WalkAwayDistance is how much farther from the goal the leader's report must be
	// than the previous one before this side accepts that the player went somewhere
	// else. Below it, jitter and the collision stop would cancel real walks.
	WalkAwayDistance = 60.0

	// WalkOrderNoise is how far from the drawn body a move report may name and still be
	// the tail or echo of an earlier walk rather than the player's own order (2026-11-09,
	// zone.go's move branch). A cancel click lands hundreds of units away.
	WalkOrderNoise = 50.0

	// pollInterval is how often Serve wakes up while a walk is in progress. It is the
	// resolution of EVERY delay around a click (the idle wait, the arrival check), so it
	// is kept at 100 ms: 200 was measurable as "feels sluggish" in game on 2026-10-25.
	pollInterval = 100 * time.Millisecond
)

// walk is one chase in progress, or nil.
type walk struct {
	from  [3]int32 // the player's position when the walk was sent
	goal  [3]int32 // the destination put in the packet
	eta   time.Duration
	began time.Time
	npc   NPC
	speed float64

	// dialogCleared is true when a CZ_SELECTCHANGE_PC arrived while this CLICK's walk was
	// still in flight (cancelWalkDialog, called from zone\control.go). What the switch
	// cancels is the DIALOG REQUEST, never the walk: the client goes on walking the body
	// to the goal, the stop and the echoes are unchanged, and only the arrival's box is
	// gone. Measured on the live server 2026-11-03 - `walk complete when change leader but
	// no dialog`, and NO ZC_DIALOG_SELECT anywhere after the 3652 in
	// captures\leaderdlg\20261103-200921, while the walk itself still ran (a 3548 echo at
	// 23.57 s).
	//
	// A CAST's or an ATTACK's approach is never cleared this way: their answer is a hit,
	// not a box, and this rule must not touch them.
	dialogCleared bool

	// takenOver is true when a move report the zone judged the PLAYER'S OWN ORDER named a
	// point away from this walk (more than WalkOrderNoise from the drawn body). The walk
	// itself keeps its clock - a report inside ChaseGrace may still be the tail of the
	// player's earlier walk, so nothing is cancelled on it - but the next CLICK on the
	// same NPC REPLACES this walk instead of being absorbed (2026-11-09: the owner's
	// click 1 walks and he cancels, click 2 used to do nothing, click 3 worked only after
	// the stale clock ran out).
	takenOver bool

	// target is the THING this walk approaches, and near is the purpose's own distance
	// test against it - the numbers the reference re-tests every tick (piece 2d):
	//
	//	dialog  IsNearObject(target, DISTANCE_TO_CHAR*2) = 200, edge to edge
	//	        PlayerChar_FSM.cpp:353, FieldObject.cpp:76-82
	//	cast    the skill's own MinR..MaxR band, re-checked by the fire itself
	//	        (zone\skill_approach.go, firePendingCast) - near stays 0 here
	//	attack  the stance's reach band, asked EVERY TICK by R100
	//	        (zone\attack_approach.go, fireHeldAttackOnArrival) - near stays 0 here
	//
	// near == 0 means "this purpose tests something else", never "no test": the two
	// purposes that answer it elsewhere are named above.
	target  [3]int32
	targetR float64
	near    float64

	// route is the PLANNED polyline this walk follows, as the path engine answered it
	// (nav.go, Plan): route[0] is `from` and the last point is `goal`. Every reader of
	// this walk's own position walks the body ALONG it (move.go, walkedRouteAt), so the
	// point the swing names is a point on the route the clock counted (R110).
	//
	// NIL means "no route to walk": no path engine, the walk graph, or a step-back,
	// whose goal is arithmetic and never planned. Then the body is drawn on the straight
	// chord, exactly as before.
	route [][3]int32

	// cast is the skill cast this walk exists for, or nil for a click on an NPC
	// (zone\skill_approach.go). A walk with a cast fires the cast where a click
	// opens a dialog; nothing else about the clock differs.
	cast *pendingCast

	// atk is the NORMAL ATTACK this walk exists for (R90, zone\attack_approach.go):
	// an out-of-reach swing walks the character in and the hit fires where a click
	// opens a dialog. Never set together with cast - one walk answers one request.
	atk *pendingAttack

	// pick is the PENDING PICKUP this walk exists for (2026-11-08, zone\pickup.go): an
	// out-of-reach CZ_PICKITEM used to be refused, and the reference instead CHASES the
	// item - `m_pickItemTarget = target; SetMoveState(MS_STOP);
	// SetActionState(AS_PICK_ITEM_CHASE);` (PlayerChar_Request.cpp:481-484) - and picks
	// it up when that walk is over. Never set together with cast or atk.
	pick *pendingPickup

	// caster is the team slot of the character walking: 0 for a click, because a
	// click walks the leader, and the CASTING slot for a skill approach. The
	// reference walks a follower when a follower casts (the capture's 3548s name
	// handles 1619550..1619552, one per team member), so this cannot be a constant.
	caster int

	// absorbed is how many repeat attack presses this walk swallowed (R97), and
	// absorbedAt is when the last of them was written to the log, so a client that
	// presses on every 100 ms AI pass writes ONE line per second and not ten. Both
	// die with the walk: a new walk starts its own count.
	absorbed   int
	absorbedAt time.Time

	// overlap is true when the destination is the NPC's OWN position because the
	// clamp was shortened: the client stops at collision distance, so the character
	// ends up touching the NPC. That is the live server's behaviour, not a defect.
	overlap bool
	// clamped is false when no path engine was available and the walk went straight to
	// the NPC's own spot.
	clamped bool

	// sentAt is when the packet left; move reports inside ChaseGrace of it are the tail
	// of the player's own walk.
	sentAt time.Time

	// The leader's own reports, used as the REAL arrival signal when one arrives. See
	// observeWalk: for a SERVER-driven walk this client sends none (measured 2026-10-29,
	// R64), so the field is only ever set by the tail of the player's own walk, and the
	// dialog clock below is what opens the box in practice.
	hasReport  bool
	lastDist   float64 // distance from that report to the goal
	lastReport time.Time

	// castDone is true once the pending cast has fired. It is what keeps one press from
	// casting twice now that the fire and the stop land on the same tick: the fire sets
	// it, and the walk's own close reads it to know nothing is owed.
	castDone bool

	// NOTE for whoever adds position saving: `goal` is the stand-off point (or the NPC's
	// own spot), not where the player really is. Saving it as the player's position is
	// the bug history\zone-npc-click.md records.
}

// approachOf answers the approach walk character idx owns: a cast's or an attack's walk
// this server sent for THAT character, or nil.
//
// WHY THIS EXISTS (R131, 2026-11-04). The owner's rule: "you want each of the character to
// be independent unless u use the group mode ... changing leader should not affect
// anything". One `sess.Walk` for the whole team meant the opposite: a press on character 2
// REPLACED character 0's approach, and a leader switch dropped it outright (measured
// 17:12:15: a 419-unit approach sent, a switch 48 ms later, the next press re-planning the
// same leg with the distance unchanged to thirteen decimals). Now every character keeps its
// own walk in teamWalks, and Walk stays the CLICK chase - one click, by the character the
// player drives.
func (s *Session) approachOf(idx int) *walk {
	if w := s.Walk; w != nil && w.caster == idx && (w.atk != nil || w.cast != nil || w.pick != nil) {
		return w
	}
	for _, w := range s.teamWalks {
		if w != nil && w.caster == idx {
			return w
		}
	}
	return nil
}

// addApproach installs w as its character's ONE approach, and answers the walk it replaced
// (nil when there was none). A press REPLACES that character's own walk and nobody else's.
//
// The single-approach case still lands in Walk, so every reader that predates R131 - and
// every fixture that sets sess.Walk directly - keeps seeing exactly what it saw before.
func (s *Session) addApproach(w *walk) *walk {
	if cur := s.approachOf(w.caster); cur != nil {
		if s.Walk == cur {
			s.Walk = w
		} else {
			for i, other := range s.teamWalks {
				if other == cur {
					s.teamWalks[i] = w
					break
				}
			}
		}
		return cur
	}
	if s.Walk == nil {
		s.Walk = w
		return nil
	}
	// THE SAME CHARACTER'S OTHER WALK IS REPLACED, whatever it was made for: that is what
	// this record meant before R131 (a press took the click walk's record when the stand
	// point was the same, and the duplicate MOVE_PC was refused a line later -
	// TestAnAttackApproachDoesNotRepeatTheWalkInFlight pins it). Only a DIFFERENT
	// character's walk is left alone, which is the half R131 added.
	if s.Walk.caster == w.caster {
		old := s.Walk
		s.Walk = w
		return old
	}
	s.teamWalks = append(s.teamWalks, w)
	return nil
}

// clearApproach takes w out of whichever record holds it. Nothing else is touched: a walk
// belongs to the character that made it (R131).
func (s *Session) clearApproach(w *walk) {
	if w == nil {
		return
	}
	if s.Walk == w {
		s.Walk = nil
		return
	}
	for i, cur := range s.teamWalks {
		if cur == w {
			s.teamWalks[i] = nil
			return
		}
	}
}

// parkApproach moves an APPROACH out of Walk and into teamWalks, so a CLICK - which owns
// Walk, because a click is one character's order and the client drags the rest along - can
// take the record without ending anybody's approach (R131).
func (s *Session) parkApproach() {
	w := s.Walk
	if w == nil {
		return
	}
	s.Walk = nil
	if w.atk == nil && w.cast == nil && w.pick == nil {
		return // a click walk: the caller is replacing it, nothing to keep
	}
	s.teamWalks = append(s.teamWalks, w)
}

// walkOwnedBy answers the walk THIS character owns, whichever record holds it: the click
// chase it walked, or its own approach. It is the "is a server walk in flight for this
// slot" question, asked of the right record (R131).
func (s *Session) walkOwnedBy(idx int) *walk {
	if w := s.Walk; w != nil && w.caster == idx {
		return w
	}
	return s.approachOf(idx)
}

// teamApproaches is every approach in teamWalks, in slot order, nil slots skipped. Walk is
// NOT included: its holder steps it separately, so one tick cannot step a walk twice.
func (s *Session) teamApproaches() []*walk {
	var out []*walk
	for _, w := range s.teamWalks {
		if w != nil {
			out = append(out, w)
		}
	}
	return out
}

// chaseActive reports whether ANY walk is in flight, whoever owns it.
func (s *Session) chaseActive() bool {
	return s.Walk != nil || len(s.teamApproaches()) > 0
}

// describe names what a walk is walking towards, for a log line: the NPC for a
// click, the skill for an approach (R87), the target for a normal attack's approach
// (R90).
func (w *walk) describe() string {
	if w.cast != nil {
		return "the skill " + strconv.FormatUint(uint64(w.cast.skill), 10) + " cast"
	}
	if w.atk != nil {
		return "the attack on target " + strconv.FormatUint(uint64(w.atk.target), 10)
	}
	if w.pick != nil {
		return "the pickup of item 0x" + fmt.Sprintf("%08x", w.pick.item)
	}
	return describeNPC(w.npc)
}

// opensDialog is true when this walk's arrival still owes the player the NPC box: it is a
// CLICK (no cast, no attack) and no CZ_SELECTCHANGE_PC has cancelled its request.
func (w *walk) opensDialog() bool {
	return w.cast == nil && w.atk == nil && w.pick == nil && !w.dialogCleared
}

// cancelWalkDialog cancels the DIALOG REQUEST of a click that is still walking when the
// player switches character (CZ_SELECTCHANGE_PC, zone\control.go).
//
// ONLY THE BOX GOES. The walk keeps its clock, its goal, its stop and its echoes, so the
// character is drawn walking up to the NPC and stopping there - which is what the live
// server does (2026-11-03: `walk complete when change leader but no dialog`, and no
// ZC_DIALOG_SELECT anywhere after the 3652 in captures\leaderdlg\20261103-200921).
//
// An ATTACK's approach (w.atk) and a CAST's (w.cast) are not touched: their answer is a
// hit, not a box. That is why the test is opensDialog and not "a walk is open".
func (h *Handler) cancelWalkDialog(sess *Session) {
	w := sess.Walk
	if w == nil || !w.opensDialog() {
		return
	}
	w.dialogCleared = true
	h.Log.Printf("zone: CONTROL slot %d - the NPC click on %s (handle 0x%08x) is CANCELLED "+
		"by the switch: the character still finishes the walk, and no dialog opens on arrival",
		sess.Control, describeNPC(w.npc), w.npc.Handle)
}

// startChase sends the ONE move that walks the player to npc.
//
// It refuses, out loud, when the destination would be a guess: no path engine and too
// far, or a clamp that says the player's own ground is invalid (the live sends nothing
// in that case too).
func (h *Handler) startChase(sess *Session, npc NPC) [][]byte {
	if sess.Walk != nil {
		h.Log.Printf("zone: chase to %s ignored - already walking to %s",
			describeNPC(npc), sess.Walk.describe())
		return nil
	}
	// A FRESH WALK OWNS THE LOCK: an NPC that was armed for a redirect is answered now.
	sess.ClickLockArmed = 0

	dist := npcDistance(sess.Pos, npc)
	npcPos := [3]int32{npc.X, npc.Y, npc.Cell}
	// The approach point is where the character ACTUALLY stands, not where the last click
	// pointed it: Pos is the DESTINATION of the latest CZ_MOVE_PC, so a click made
	// mid-walk would aim the stand-off from a place the character has not reached yet.
	// walkedPos is the same rule zone\gate.go has used since R55.
	//
	// BUT THE INTERPOLATION LAGS A WHOLE LEG (2026-11-09, the owner's Leonardo click). A
	// report names the step the client just finished, and movePC starts that leg's clock at
	// the report, so right after a report walkedPos still stands at the leg's BEGINNING.
	// The owner clicked Leonardo 239 units away by the gate's own number and the plan
	// started 2,078 units back - the box opened 4.5 s late
	// (logs\zone_go_10.log, 17:08:34-17:08:44). A report inside ChaseGrace is the newest
	// truth there is, so the plan starts at it instead of behind it.
	now := time.Now()
	approach := sess.walkedPos(now)
	if sess.ChaseFromSet {
		// A WALK WAS REPLACED MID-STRIDE (2026-11-09). plan from where the body is DRAWN,
		// not from the session's last report: during a server walk the client reports
		// nothing, so the report is older than the walk itself.
		approach = sess.ChaseFrom
		sess.ChaseFromSet = false
	} else if !sess.LastMoveAt.IsZero() && now.Sub(sess.LastMoveAt) <= ChaseGrace {
		approach = sess.Pos
	}
	goal := npcPos
	routeLen := 0.0
	var route [][3]int32 // the planned polyline, or nil when the walk goes straight
	overlap := true      // the fallback IS the NPC's own spot
	clamped := false

	if h.Plan != nil {
		var ok bool
		var err error
		goal, route, routeLen, overlap, ok, err = h.Plan(approach, npcPos,
			DialogStandoff(NPCDefaultRadius))
		switch {
		case err != nil:
			// A FAILED ENGINE STILL WALKS (RESTORED 2026-11-09, the owner: "check git
			// history before the pathengine-only rework - that is the working state").
			// The engine-only rework refused the click here, and a refusal reads in game
			// as "this NPC stopped responding".
			h.Log.Printf("zone: path engine failed for %s (%v) - sending the NPC's own "+
				"position instead", describeNPC(npc), err)
			goal, route, routeLen, overlap = npcPos, nil, 0, true
		case !ok:
			// The player is not standing on valid ground (Plan's only remaining refusal).
			// The live sends nothing here, and nothing is better than a move that animates
			// nothing.
			h.Log.Printf("zone: click on %s (0x%08x) REFUSED - the player's position "+
				"(%d,%d,cell %d) is not valid ground for a walk", describeNPC(npc), npc.Handle,
				sess.Pos[0], sess.Pos[1], sess.Pos[2])
			return nil
		default:
			clamped = true
			if dist > MaxChaseWithRoute {
				h.Log.Printf("zone: click on %s is %.0f units away - beyond the %.0f cap",
					describeNPC(npc), dist, MaxChaseWithRoute)
				return nil
			}
		}
	} else {
		// NO ENGINE: STRAIGHT LINE WITH THE OLD CAP (restored 2026-11-09 with the rest of
		// the pre-rework behaviour - the owner: "that is the working state"). This arm
		// refuses only a click farther than this side will walk in one straight line.
		if dist > MaxChase {
			h.Log.Printf("zone: click on %s is %.0f units away - farther than this side will "+
				"walk in one straight line (%.0f) without a path engine, so nothing is sent",
				describeNPC(npc), dist, MaxChase)
			return nil
		}
	}
	if goal == sess.Pos {
		return nil
	}

	// ONE speed for the packet AND the clock: the `spd` in the move is the speed the
	// client animates the walk at, so it is also the only honest basis for "how long
	// does this walk take" (see the arrival rule in move.go). The measured speed
	// (sess.walkSpeed) is still logged - it is evidence for the next in-game test.
	//
	// The character is the one the player DRIVES (zone\control.go), not Team[0]: the
	// packet below is built for that slot, so a different slot's MSPD here would time
	// the walk at one speed and animate it at another.
	speed := h.packetSpeed(controlledChar(sess))
	if speed <= 0 {
		// A stance row whose StnMSPD is 0 would make the eta infinite and the walk
		// never end. The base speed cannot be wrong here: it is the constant inside
		// the MSPD formula, and packetSpeed only reaches 0 through bad table data.
		speed = MoveBaseSpeed
	}
	walked := routeLen
	if walked <= 0 {
		walked = math.Hypot(float64(goal[0]-approach[0]), float64(goal[1]-approach[1]))
	}
	eta := time.Duration(walked / speed * float64(time.Second))
	sess.Walk = &walk{
		from: approach, goal: goal, eta: eta, began: now, sentAt: now,
		speed: speed, npc: npc, overlap: overlap, clamped: clamped, route: route,
		// THE THING THIS WALK APPROACHES, AND THE DIALOG'S OWN NEAR-TEST (piece 2d):
		// IsNearObject(target, DISTANCE_TO_CHAR*2), edge to edge, which is the check the
		// reference's AS_DIALOG_CHASE re-runs on every tick (PlayerChar_FSM.cpp:353).
		target: npcPos, targetR: NPCDefaultRadius, near: DialogNear,
		// WHICH CHARACTER THIS WALK WALKS (zone\control.go). A click walks the
		// character the player drives, and after a CZ_SELECTCHANGE_PC that is not
		// slot 0 any more. observeWalk compares a report's slot against this, so
		// leaving it at the field's zero would make the controlled character's own
		// report "not the walking one" and the follower's report the arrival signal.
		caster: sess.Control,
	}
	// A reported segment left open by the player's own walk must not close in the
	// middle of the chase and stop the far actor half way (see endWalks).
	//
	// NO rememberWalk HERE (R91 round 7): a click chase drives the character the player
	// drives (sess.Control), whose live point is s.moveFrom -> s.Pos and is never read
	// from Team[Control].Position.
	//
	// SENSE: sess.Control, not a literal 0. This drops the stale segment of THAT
	// character - the one whose walk the line above just replaced. With a literal 0 it
	// would drop a FOLLOWER's live segment instead the moment the player switches, and
	// the far client's follower would never be told to stop.
	sess.closeWalk(sess.Control)

	where := "the NPC's own position (clamp shortened - the client stops at collision distance)"
	if !overlap {
		where = "the clamped stand-off point"
	}
	if !clamped {
		where = "the NPC's own position (no path engine)"
	}
	if clamped {
		// Named in degrees AND in words, next to the side the player came from, so the
		// next in-game test can be read straight out of the log (R58). A stand-off on the
		// side the player is NOT on is the bug this line exists to catch.
		if !overlap {
			h.Log.Printf("zone: stand-off for %s (0x%08x) is sector %.0f deg (%s side of the "+
				"NPC, %d,%d); the approach is from %.0f deg (%s side, %d,%d)",
				describeNPC(npc), npc.Handle, SectorDir(npcPos, goal), sectorWord(SectorDir(npcPos, goal)),
				goal[0], goal[1], SectorDir(npcPos, approach), sectorWord(SectorDir(npcPos, approach)),
				approach[0], approach[1])
		} else {
			h.Log.Printf("zone: stand-off for %s (0x%08x) is the NPC itself (clamp "+
				"shortened - the client stops at collision distance); the approach is from "+
				"%.0f deg (%s side, %d,%d)", describeNPC(npc), npc.Handle,
				SectorDir(npcPos, approach), sectorWord(SectorDir(npcPos, approach)),
				approach[0], approach[1])
		}
	}
	h.Log.Printf("zone: chasing %s (0x%08x) - one move to (%d, %d, cell %d), %s, "+
		"%.0f units of route, eta %.1fs at %.0f u/s (%d measured)",
		describeNPC(npc), npc.Handle, goal[0], goal[1], goal[2], where,
		walked, eta.Seconds(), speed, sess.moveSamples)

	// ONE packet, built once, and the OTHER players get these exact bytes (R63,
	// symptom 1). Before this line the chase was the owner's alone: the other client
	// was never told the walk had started and only heard the ZC_STOP_PC when it ended,
	// so its copy of the actor stood still and then TELEPORTED to the NPC. Sending the
	// same slice is the point: the two clients animate the same walk, and the owner's
	// own copy is untouched (broadcastMove copies nothing and edits nothing).
	pkt, handle := h.legMove(sess, goal)
	if pkt == nil {
		// A WALK TO THIS POINT IS ALREADY ON THE WIRE (zone\walk_once.go): legMoveFor
		// refused the duplicate and said so. The chase itself stays set - it is the same
		// destination - and its clock still opens the dialog.
		return nil
	}
	// SENSE: the team slot of the character that just walked, which is the character
	// the player drives - NOT "slot 0". It is the same slot legMove put in the
	// packet's handle and the same one SetPos stores, so the far client's copy moves
	// on the right actor.
	h.broadcastMove(sess, sess.Control, handle, goal, pkt)
	return [][]byte{pkt}
}

// controlledChar is the character the player is DRIVING - Team[Control], or nil for
// a session with no team yet (packetSpeed answers the base speed for nil).
//
// It is NOT "the team's first character": see zone\control.go. Before the client
// sends CZ_SELECTCHANGE_PC, Control is 0 and this is exactly the old leaderOf.
func controlledChar(sess *Session) *Character {
	if sess.Control < 0 || sess.Control >= len(sess.Team) {
		return nil
	}
	return &sess.Team[sess.Control]
}

// sectorWords names the 16 compass sectors the emitter stores, in its own convention:
// 0 degrees is +x and 90 is +y.
var sectorWords = [16]string{"E", "ENE", "NE", "NNE", "N", "NNW", "NW", "WNW",
	"W", "WSW", "SW", "SSW", "S", "SSE", "SE", "ESE"}

// sectorWord turns a sector direction into the word a person reads faster than degrees.
func sectorWord(deg float64) string {
	i := int(math.Round(deg/22.5)) % 16
	if i < 0 {
		i += 16
	}
	return sectorWords[i]
}

// THE ARRIVAL IS A DISTANCE, RE-TESTED ON THE TICK (piece 2d).
//
// What was here: walkDone() = eta x StopSafety (1.4) + StopSlack (250 ms) decided when a
// walk was OVER, dialogDue() = eta + 250 ms decided when the box might open, and
// approachFireDue() = eta + 50 ms decided when a hit might fire. All three were
// arithmetic on a TIME ESTIMATE, so the answer always came 40% (plus a quarter second)
// after the body had already arrived - the owner measured it: `finished after 9.231s
// (eta 6.412s)`, and "NPC dialog kinda slow ... a slight delay of few seconds".
//
// What the reference does instead, in every approach state: it advances its own body by
// speed x dt each tick and, when the move state is over, re-tests the DISTANCE to the
// thing it is approaching - `if (false == IsNearObject(target, DISTANCE_TO_CHAR*2))` walk
// again, `else` act (PlayerChar_FSM.cpp:340-372). Its attack state does the same with
// `IsAttackArea` (FieldChar_FSM.cpp:9-95). There is no timer anywhere in either.
//
// So: the body's own walked point (bodyAt) has to reach the walk's goal. That is the one
// and only "the body has arrived" test left in this file. Nothing is padded, because
// nothing here is an estimate of when the body will be there - it is where the body IS.
//
// The pads themselves are NOT gone from the tree, and the one place they still belong is
// stated where it lives: a PLAYER-DRIVEN leg (move.go, charWalk.walkedOut) has no body
// model at all, only a report and a speed, so its stop - a packet that TELEPORTS the far
// actor to whatever position it names - keeps StopSafety and StopSlack. That is the
// client-leg rule of piece 2b and it is untouched here.

// arrived says whether the body walking this walk is AT its goal, by distance.
//
// The body is read from the walk's own geometry - along its ROUTE at the speed the move
// on the wire told the client to animate (bodyAt, the same reader the swing uses) - and
// compared with the goal the move named. Once the route is walked out the point IS the
// goal, so the first tick at or past the route's own walk time answers yes; a tick
// before it answers no, whatever the eta FIELD says. That is the whole point of the
// piece: the eta is a budget, not an arrival.
func (w *walk) arrived(now time.Time) bool {
	return flatDist(w.bodyAt(now), w.goal) <= ArrivalEpsilon
}

// nearTarget measures the body against the THING this walk approaches, in the
// reference's own arithmetic: the ground distance between the two centres minus both
// bodies' radii (CFieldObject::IsNearObject, FieldObject.cpp:76-82).
//
// ok is true when the purpose states no edge-to-edge test here (near == 0): a cast's
// range and an attack's reach band are asked by their own code, on the same live point,
// and asking them twice would be the second clock this piece removes.
func (w *walk) nearTarget(body [3]int32) (gap float64, ok bool) {
	if w.near <= 0 {
		return 0, true
	}
	radius := w.targetR
	if radius <= 0 {
		radius = NPCDefaultRadius
	}
	gap = flatDist(body, w.target) - (PCBodyRadius + radius)
	return gap, gap <= w.near
}

// legMove is the packet that starts the walk. It answers the packet and the handle it
// names, so the caller can send the SAME bytes to the other players.
func (h *Handler) legMove(sess *Session, to [3]int32) ([]byte, uint32) {
	// SENSE: which slot walks - the one the player drives (zone\control.go), not
	// slot 0. The index must match sess.Walk.caster above and the broadcast index at
	// the end of the chase, or the packet's handle, the stored position and the one
	// record the gate follows would name three different characters.
	return h.legMoveFor(sess, sess.Control, to, ReasonDialog)
}

// legMoveFor is legMove for a NAMED character and movement reason: a click walks the
// leader with MR_DIALOG, a skill approach (zone\skill_approach.go) walks whichever
// character cast, with the MR_SKILL reason the reference uses for that walk.
//
// The speed in the packet is the character's own MSPD - the SAME number the echo of
// the player's own walk carries (zone.go, packetSpeed). The measurement is NOT used
// here: it only times the clock, and a sparse measurement sent as a speed is a
// visibly crawling character (2026-10-25).
func (h *Handler) legMoveFor(sess *Session, idx int, to [3]int32, reason uint32) ([]byte, uint32) {
	handle := uint32(0)
	var who *Character
	if idx >= 0 && idx < len(sess.Team) {
		handle = sess.Team[idx].Handle
		who = &sess.Team[idx]
	}
	// THE ONE CHOKE POINT, AND WHY IT IS HERE (zone\walk_once.go). Every walk this
	// side starts - a click's chase (legMove), a normal attack's approach and a skill
	// approach - is built by this function and by no other. The rule "one walk per
	// (character, destination)" therefore belongs here and not at the three callers:
	// each of them only knows its own reason for walking, and a duplicate can arrive
	// from a reason that is not the one already in flight.
	//
	// A REFUSED WALK SENDS NOTHING AND CHANGES NOTHING. The caller has already set its
	// sess.Walk, so the press (or the click, or the cast) still counts and its clock
	// still runs; only the second packet is gone. Returning a nil packet is what the
	// callers check.
	if !sess.walkOnceAt(idx, to, time.Now()) {
		h.Log.Printf("zone: SENT walk REFUSED char %d (%d) -> (%d, %d) cell %d: one walk "+
			"to that point is already in flight and has not ended, so nothing is sent - "+
			"a second ZC_MOVE_PC would restart the client's interpolation of this actor "+
			"mid-stride (IsEqualPos(moveTo, m_moveTo), FieldChar_FSM.cpp:49 and :74)",
			idx, handle, to[0], to[1], to[2])
		return nil, handle
	}
	speed := float32(h.packetSpeed(who))
	pkt := BuildMovePCReason(handle, to, uint32(nowMillis()), speed, reason)
	// THE GOAL IS ON THE LINE (R105). The bare "SENT cmd 3548, 30 bytes" said only
	// that a move went out and 30 bytes of it, so two walks could agree or contradict
	// and the log could not tell. The destination is what decides which, and it is the
	// field the next in-game reading needs.
	//
	// THE LINE IS A DIAGNOSTIC NOW (`-diag-events`): it is one line per walk leg, and a
	// leg is built per event on the movement path, so at 50 bots it is part of the ~100
	// lines/s that bury everything else. OFF, the METRICS line's `walks=` counts the
	// same legs, so the count is not lost - only the destination is, and the destination
	// is what `-diag-events` is turned on to read. ON, this line is byte for byte what
	// it always was.
	metrics.walk()
	// A WALK WITH NO ROUTE SAYS SO (2026-11-09). The walk installed by the caller is
	// the one this packet is for; when it carries no route the body walks the straight
	// chord, which until now left NOTHING in the log - the one outcome the owner could
	// not diagnose ("do we have like a fallback or what? thats why i hate fallback,
	// cant really pinpoint the exact issue"). The count goes up for EVERY chord; the
	// line is bounded to once per (map, cell) by noteChordWalk.
	if w := sess.walkOwnedBy(idx); w != nil && w.goal == to && len(w.route) == 0 {
		metrics.walkStraight()
		h.noteChordWalk(w)
	}
	if h.DiagEvents {
		h.Log.Printf("zone: SENT walk char %d -> (%d, %d) cell %d speed %.0f (cmd %d, %d B, "+
			"reason %d)", idx, to[0], to[1], to[2], speed, CmdZCMovePC, len(pkt), reason)
	}
	return pkt, handle
}

// noteChordWalk says, ONCE PER (map, cell), that the walk just sent is the STRAIGHT
// CHORD: a walk installed with NO route.
//
// WHY (2026-11-09, the owner on a click that moved nothing on fld_auch: "do we have
// like a fallback or what? thats why i hate fallback, cant really pinpoint the exact
// issue"). A walk has two outcomes and only one of them used to leave a trace. A
// REFUSAL is logged (no path engine, attack_approach.go; invalid ground, there too).
// The CHORD was silent: the body is simply sent at the goal, and on the wire that is
// indistinguishable from a client that never got the packet. The live log already has
// the loud half of the same shape - `2026/11/08 23:23:55 zone: LAST POINT not saved
// for commander 24 (Grace): the map 1331 mesh has no valid ground at (-15583, -2338,
// cell 100)` - and the click that followed it walked a chord with nothing to grep.
//
// THE WALK IS NOT CHANGED. This line is evidence and nothing else: the same packet
// goes out, at the same goal, by the same clock.
//
// THE BOUND IS (map, cell), never per walk and never per tick: a player clicking the
// same spot ten times would write ten identical lines, and a fight's step-backs
// (arithmetic, never planned - R104) one per press. The COUNT is not lost - the caller
// adds metrics.walkStraight for every chord - so `walks=` minus `walksStraight=` on the
// METRICS line says how many chords this line stayed silent about.
//
// The once-per-key printer is dropSayOnce (zone\drop.go): the project's one say-once
// helper, already used outside the drop path (pickup.go). It carries no drop meaning.
func (h *Handler) noteChordWalk(w *walk) {
	why := "the path engine answered no route for it"
	switch {
	case h.Plan == nil:
		why = "this zone has no path engine (Handler.Plan is nil)"
	case w.atk != nil && w.atk.stepBack:
		why = "a step-back is arithmetic and is never planned (R104)"
	}
	h.dropSayOnce(fmt.Sprintf("chord-%d-%d", h.MapID, w.from[2]), fmt.Sprintf(
		"zone: WALK IS THE STRAIGHT CHORD map %d (%s) from (%d, %d, cell %d) to "+
			"(%d, %d, cell %d): %s, so the body walks the chord and no route can be named "+
			"- said once per (map, cell); METRICS walksStraight= counts every one",
		h.MapID, h.MapName, w.from[0], w.from[1], w.from[2],
		w.goal[0], w.goal[1], w.goal[2], why))
}

// noteCellFix says ONCE PER (map, client cell > mesh cell) that a client's cell was not
// the triangle the mesh has at its (x, y), and the mesh's cell was adopted (2026-11-09).
// A foreign cell is what made the engine answer "no route" while the ground was fine.
func (h *Handler) noteCellFix(client, mesh int32) {
	h.dropSayOnce(fmt.Sprintf("cellfix-%d-%d-%d", h.MapID, client, mesh), fmt.Sprintf(
		"zone: CLIENT CELL %d is not the mesh's cell %d on map %d (%s) - the mesh's cell "+
			"is adopted; said once per (map, cell pair)", client, mesh, h.MapID, h.MapName))
}

// noteClientChord is the CLIENT-LEG half of the chord record (2026-11-09). A player's
// own reported leg cannot be refused - the client has already moved - so a leg the
// engine could not plan is DRAWN on the chord, COUNTED in METRICS walksStraight=, and
// named once per (map, from cell, to cell). Never silent, never per tick.
func (h *Handler) noteClientChord(from, to [3]int32) {
	metrics.walkStraight()
	h.dropSayOnce(fmt.Sprintf("client-chord-%d-%d-%d", h.MapID, from[2], to[2]), fmt.Sprintf(
		"zone: CLIENT LEG IS THE STRAIGHT CHORD map %d (%s) from (%d, %d, cell %d) to "+
			"(%d, %d, cell %d): the path engine could not plan it - drawn on the chord and "+
			"counted; said once per (map, cell pair)",
		h.MapID, h.MapName, from[0], from[1], from[2], to[0], to[1], to[2]))
}

// tickIfDue runs the session's own clock at most once per pollInterval, and answers
// the packets it produced and whether it ran at all.
//
// IT EXISTS BECAUSE THE TICK USED TO BE REACHED ONLY ON A READ ERROR (R95 item 4). A
// client that keeps the socket busy - a frame every few milliseconds, faster than the
// 100 ms poll - resets the read deadline on every pass, so the read never times out, the
// old tick never ran, and the swing repeat, a cast's wind-up and a walk's stop all waited
// for a packet that never comes. Serve now asks this at the TOP of every pass, so the
// clock advances on the loop's own terms whatever the read returned.
//
// lastTick is advanced ONLY when the tick really runs, so the cadence is exactly
// pollInterval and a loop that spins through many inbound frames cannot shorten it. A
// session with no pending work answers false at the first check and blocks on the socket
// as before - wantsTick's five reasons are unchanged.
func (h *Handler) tickIfDue(sess *Session) ([][]byte, bool) {
	// A ZC_DEAD WAITING IS A REASON OF ITS OWN (R98). The five reasons wantsTick names
	// are the SESSION's work; a monster's death belongs to the zone, and the session
	// whose hit caused it can be left with nothing to do - its swing repeat ends on the
	// body being empty. Without this the clock would stop and the packet would wait for
	// traffic that never comes.
	//
	// A MONSTER THAT CAN SWING IS THE SAME KIND OF REASON (the monster swing slice): its
	// cadence is the zone's, not a session's, and a session standing still is exactly
	// the case in which it must still fire. monsterSwingArmed answers false the moment
	// no body in the zone names an attack, so an ordinary zone does not tick for it.
	// SP REGENERATION IS THE SAME KIND AGAIN (zone\sp_regen.go): a regenerate stance's
	// 3 s clock is the zone's, and the player it belongs to sends nothing while he
	// stands there waiting for the bar to fill.
	if !sess.wantsTick() && !h.deadPending() && !h.monsterSwingArmed() &&
		!h.monsterChaseArmed() && !h.sightArmed() && !h.DiagDistance && !h.MonsterAIScript &&
		!sess.hasBuffs() && !h.spRegenArmed(sess) && !h.tacticThreadsArmed() {
		return nil, false
	}
	now := time.Now()
	if !sess.lastTick.IsZero() && now.Sub(sess.lastTick) < pollInterval {
		return nil, false
	}
	sess.lastTick = now
	// THE ZC_DEAD IS NOT HERE ANY MORE (2026-11-07): it leaves with the killing hit
	// (zone\create.go damageMonsterBy), so a body's death cannot wait for a tick. What
	// goes first here is the corpse's own two phases (zone/respawn.go): the leave that
	// takes the body off the field max(shape DyingTime, 600ms) after its ZC_DEAD, then
	// the new body at the same point. They follow the death packet on the same clock so
	// a client can never be told a body returned before it was told the old one left.
	h.corpseTick(now)
	// THE SCRIPT'S ORDERED WALK ADVANCES FIRST (zone\monster_order.go): a
	// `Move`/`RandomMove` 3550 is a LEG, and the body's own point is what the think turn's
	// `self.X`/`self.Y`, the swing's reach, the sight ring and the next 3564 read. Stepping
	// it BEFORE the think turn is what makes the point exact when the script asks
	// `IsEnableMove`: the pass that finishes a leg writes the goal, so the next order's
	// "from" is the point the last one named and not one tick of travel short of it. In go
	// mode it returns on its first line.
	h.monsterMoveTick(now)
	// THEN THE CHASE, THEN THE SWING, in that order and on the same clock. The chase
	// moves the bodies this pass (zone\monster_chase.go); running it FIRST means the
	// swing below measures the reach on the point a body has just walked to, so a
	// monster that arrives inside its reach this pass swings on this pass instead of
	// one tick later. Both write their own packets (a monster has no session to return
	// them to) and both are locked, so every session reaching this line in the same
	// instant still produces one swing per cadence and one chase step per poll.
	h.monsterAITick(now)
	// THEN THE NPCs' TRIGGER THREADS (zone	actics.go, R141): an arm manager's
	// `while(1) sleep(2000)` is a thread whose period is the script's own sleep,
	// exactly as a monster's is, so it rides this same clock. It runs AFTER
	// monsterAITick because that pass holds its own mutex throughout and this one
	// takes the script host's - in this order nothing nests, so there is no lock
	// order to get wrong.
	h.tacticTick(now)
	// THE SCRIPT'S APPROACH WALKS ON ITS OWN CLOCK (zone\monster_order.go): `-monai
	// script` switches the Go chase below off, and the approach must not depend on the
	// SWING's per-body filter either (that filter skips every row that states no attack).
	// In go mode the call returns on its first line, so this is a boolean read there.
	h.monsterApproachTick(now)
	// THE GO CHASE IS THE FALLBACK, NOT A SECOND OPINION: with -monai script the
	// script walks the body through `Move`, and two things steering one body would
	// fight over every step. OFF is the default and this line is the old one.
	if !h.MonsterAIScript {
		h.monsterChaseTick(now)
	}
	h.monsterSwingTick(now)
	out := h.chaseTick(sess)
	out = append(out, h.gateTick(sess)...)
	// THE SIGHT RING RUNS LAST, on the same clock and for the same reason the cast and
	// the swing ride it: an actor does not move into or out of view only when the
	// client sends a frame. It is last so the packets this session is owed for its OWN
	// actions are queued before the world's announcements.
	out = append(out, h.sightTick(sess)...)
	// THE PAIR DISTANCES GO LAST, and they return nothing: -diag-distance is a log
	// line, so it is not part of the batch this tick owes the client and it cannot
	// change a byte of it (zone\diag_distance.go).
	h.diagDistanceTick(sess, now)
	return out, true
}

// sessionClockAwake answers whether this session's READ must wake at `pollInterval` so
// the zone's own clock keeps running - the wake half of `tickIfDue`.
//
// IT IS ONE FUNCTION BECAUSE THE TWO HALVES DRIFTED APART, and the measurement is the
// proof: `tickIfDue`'s guard already names every zone-owned clock (`monsterSwingArmed`,
// `monsterChaseArmed`, `MonsterAIScript`), but the read deadline did not, so a session
// with nothing of its own to do blocked on the socket and the tick simply did not run.
// `logs\zone_go_script.log` (map 1331, `-monai script`): a `StartAttack` at
// 08:19:26.906 whose first 3550 left at 08:19:29.021 - 2.1 s, not the 0.1 s poll - and
// no line at all between 08:19:41.514 and 08:19:46.434 while the owner stood still.
//
// THE SCRIPT MODE IS THE ONE ADDED HERE, and it is added for the whole mode and not for
// one body: with the game's own script running, a body's thread, its swing and its
// approach are all work this zone owes on its own clock, whatever the client is doing.
// `-monai go` answers EXACTLY what it answered before (`wantsTick`/`deadPending`/
// `sightArmed`/`DiagDistance`), byte for byte, which is what keeps the Go chase's
// behaviour untouched. The go chase's own missing wake is named in the card's notes and
// left alone: fixing it here would change go-mode behaviour, which this card forbids.
// A LIVE BUFF IS THE SIXTH REASON (buff slice 1). A buff's end is the one event this
// zone owes that no packet can wake it for: the player stands still, sends nothing,
// and the 30 s the ENTER script set must still run out. `hasBuffs` answers false the
// moment no character holds one, so an ordinary session is unaffected - the same
// shape `monsterSwingArmed` has for a body's cadence.
// SP REGENERATION IS THE SEVENTH (zone\sp_regen.go). A standing player's SP comes back
// on the stance's own 3 s clock, which is exactly the case no packet can trigger; the
// reason answers false for a stance with no regeneration lane, so it costs nothing when
// nobody regenerates. It is also what lets an unknown RSPType print its one line.
//
// BOTH HALVES ARE CHANGED ON PURPOSE. `tickIfDue`'s own guard is the other half of
// this switch (see the comment on it: the two drifted apart once and a walk waited
// 2.1 s for a tick that never came), so a buff listed only here would wake the read
// and then be refused by the tick's first line - the exact drift this pair of
// functions exists to prevent.
func (h *Handler) sessionClockAwake(sess *Session) bool {
	if sess.wantsTick() || h.deadPending() || h.sightArmed() || h.DiagDistance ||
		sess.hasBuffs() || h.spRegenArmed(sess) {
		return true
	}
	return h.MonsterAIScript
}

// chaseTick advances the clock of a walk and returns the packets due now.
func (h *Handler) chaseTick(sess *Session) [][]byte {
	// The other players are owed a STOP for every character whose OWN reported walk
	// has finished (see endWalks). It runs on the same tick, before the chase, so a
	// click that turns into a chase cannot leave a second stop behind.
	h.endWalks(sess, time.Now())

	// A CAST WAITING ON ITS WIND-UP IS OWED ITS RESULT (zone\skill_cast_clock.go), and
	// it is answered FIRST because it has nothing to do with any walk: the reference
	// sends the 3571 and the result 605-949 ms apart whether or not the player walked
	// in between. Whatever this returns is appended to the walk's own packets below -
	// a cast landing on the same tick as a walk's dialog is two separate answers.
	out := h.castTick(sess)
	// THE BUFFS AGE ON THE SAME CLOCK, AND THEY ARE THE SIXTH REASON IT WAKES. A buff
	// is the one piece of character state that changes with NO packet to trigger it: a
	// 30 s buff on a player who stands still has to expire, its bar has to be resent
	// and its bit has to clear. `buffTick` measures the wall time itself, so it does
	// not depend on this tick's cadence - only on the tick happening at all, which is
	// what `sessionClockAwake` and `tickIfDue`'s guard now count (zone\chase.go).
	out = append(out, h.buffTick(sess)...)
	// SP COMES BACK ON THE STANCE'S OWN CLOCK (zone\sp_regen.go): the game's SCR_Get_RSP
	// amount once every SPRegenInterval, clamped at the character's max SP. It is here
	// for the same reason the buffs are - a player who stands still and sends nothing
	// must still be given SP back, so `spRegenArmed` is also a reason the clock wakes.
	out = append(out, h.spRegenTick(sess, time.Now())...)

	// A KEPT ATTACK PRESS IS OWED ITS HIT THE MOMENT THE DELAY RUNS OUT (zone\attack.go,
	// holdPress/pressTick). It rides the same tick as a cast's clock and for the same
	// reason, and it goes BEFORE the repeat's own swing: this is the player's press, and
	// the repeat is the rhythm it started - both draw on the same lastAttack, so whichever
	// runs first is the one that gets this interval's hit.
	out = append(out, h.pressTick(sess, time.Now())...)

	// A NORMAL ATTACK'S REPEAT IS OWED ITS NEXT SWING (zone\attack_swing.go, R94), and
	// it rides the same tick for the same reason as a cast's clock: the zone drives
	// nothing itself, so without this the rhythm would wait for a client packet that
	// never comes until the player clicks again. A swing is a whole 3564 + 3557, so it
	// is appended as its own group and cannot interleave with a cast's result.
	out = append(out, h.swingTick(sess)...)

	// EVERY WALK STEPS ON THE SAME TICK, AND EACH STEPS ITSELF (R131). Walk is the click
	// chase; teamWalks holds one approach per character, so two characters hunting at once
	// do not take each other's walk away. The order is stable (the click first, then slot
	// order) so a log quoted from one tick reads the same twice.
	now := time.Now()
	if w := sess.Walk; w != nil {
		out = append(out, h.stepWalk(sess, w, now)...)
	}
	for _, w := range sess.teamApproaches() {
		out = append(out, h.stepWalk(sess, w, now)...)
	}
	return out
}

// stepWalk advances ONE walk by one tick and answers what that walk owes the wire: the
// held press's own reach check, the arrival's fire, or the give-up. It was the body of
// chaseTick until R131; it is a function now because each character's approach has to be
// stepped on its own clock instead of the team's single record.
func (h *Handler) stepWalk(sess *Session, w *walk, now time.Time) [][]byte {
	var out [][]byte
	elapsed := now.Sub(w.began)

	// A HELD ATTACK PRESS IS ASKED ON EVERY TICK (R100), BEFORE the arrival, and it is
	// not a clock: it asks the stance's own reach band of the body's live point and fires
	// the moment the answer is yes (`IsAttackArea` -> SetMoveState(MS_STOP);
	// SetActionState(AS_ATTACK), FieldChar_FSM.cpp:28-80, case 0). The body advances on
	// this tick, so this is the check a real client exercises - the leader sends no
	// reports during a SERVER-driven walk.
	//
	// It stays ahead of the arrival on purpose: a target that stepped CLOSER puts the body
	// inside the band before the walk's goal, and the reference fires there too. Piece 2d
	// changed only what happens when the body reaches the GOAL, not this earlier answer.
	if w.atk != nil {
		out = append(out, h.fireHeldAttackOnArrival(sess, w.caster, now)...)
	}

	// A PENDING PICKUP IS DROPPED THE MOMENT ITS ITEM LEAVES THE GROUND, and nothing is
	// sent for it (zone\pickup.go, pickChaseTick). While the item is still there the
	// chase is silent on a tick: the reference's AS_PICK_ITEM_CHASE returns at once on
	// `MS_MOVE == GetMoveState()` (PlayerChar_FSM.cpp:166-167), so the ONE walk the
	// press ordered is the whole chase and the pickup runs when that walk ends.
	if w.pick != nil {
		out = append(out, h.pickChaseTick(w, now)...)
	}

	// THE ARRIVAL: THE BODY'S OWN DISTANCE, ON THIS TICK. There is no pad and no "wait
	// until the walk should have finished" - see the note above walk.arrived.
	if w.arrived(now) {
		return append(out, h.finishWalk(sess, w, now)...)
	}

	// THE ONE FALLBACK (piece 2d, item 4). A walk that CANNOT arrive - the client collided
	// with something this side cannot see, the ground refused the path it was given - has
	// no distance answer at all, so it is written off on its budget: the walk's own route
	// time plus ChaseGiveUp. Nothing fires, and the line says so.
	if elapsed > w.eta+ChaseGiveUp {
		what := "- no dialog, because a box from the wrong place is worse than none"
		if w.cast != nil {
			what = "- and no cast, because a hit from a place the character never " +
				"reached is worse than none"
		}
		if w.atk != nil {
			what = "- and no hit, because a hit from a place the character never " +
				"reached is worse than none"
		}
		if w.pick != nil {
			// A PICKUP THAT NEVER GOT THERE IS DROPPED, not retried: the item cannot
			// have moved, so the same stand point would be the same walk again, and the
			// reference's own chase only re-orders while a move is NOT in flight
			// (PlayerChar_FSM.cpp:166-167). One line names it; castDone is the drop.
			what = "- and no pickup, because an item taken from a place the character " +
				"never reached is worse than none - the pending pickup is DROPPED"
			w.castDone = true
		}
		h.Log.Printf("zone: chase to %s gave up after %s (eta %s) %s",
			w.describe(), elapsed.Round(time.Millisecond), w.eta.Round(time.Millisecond), what)
		// THE LEG KEEPS ITS BODY AND ITS CLOCK EVEN WHEN THE WALK IS GIVEN UP (R111/R112).
		// The body is drawn where the walk got to (walkedRouteAt stops at the far end),
		// and that is the point the swing beside this line was reading; handing it over is
		// what stops the NEXT reading from falling back to a point an old move report
		// declared. No place-and-no-leg would be the 808-unit jump of R111 in the other
		// direction.
		sess.keepWalkedLeg(w)
		sess.clearApproach(w)
		h.stopWalker(sess, w.caster, w.goal)
		return out
	}
	return out
}

// finishWalk closes a walk whose body has ARRIVED, and it is the ONE place a walk ends
// on its distance. Three things happen on the same tick, in this order, and none of them
// waits for anything:
//
//  1. the answer the walk was made for fires - the dialog opens, or the cast, or the hit;
//  2. the leg is handed over exactly as piece 2c hands one over (keepWalkedLeg: the
//     body's stored point follows the goal and the leg is remembered with its own
//     clock), so the next swing reads the point the body really walked to;
//  3. the other players get the ONE ZC_STOP_PC they are owed, naming the goal - safe
//     here precisely because it is the point the body has provably reached.
//
// That is also why the box's line and the stop's line are tens of milliseconds apart on
// the log now: they are the same event. Before piece 2d the box opened at eta + 250 ms
// and the stop left at eta x 1.4 + 250 ms.
// landClickArrival writes where a CLICK walk's body stopped, on the character that
// actually walked it.
//
// A click walks the character the player DROVE when the click was made (w.caster), and a
// CZ_SELECTCHANGE_PC can have changed which character that is while the walk was in
// flight (zone\control.go). `Pos` and the session's own segment (moveFrom / moveAt)
// belong to the character the player drives NOW (R109), so a walk somebody else walked
// must not write them: the newly driven character would stand on an NPC it never
// approached, and its next swing would be drawn there - the teleport R109 removed, one
// packet later. That character's own stored point is what has to follow the goal.
//
// BOTH RECORDS, EVERY ARRIVAL (measured in game 2026-11-03 20:52). The walker's own point
// in Team[] used to be written ONLY when the walker was no longer the driven character -
// so a click by the driven character moved `Pos` alone and left Team[caster] on the
// PRE-WALK spot. Nothing noticed until a CZ_SELECTCHANGE_PC arrived: a switch re-seats
// `Pos` from that record (move.go, closeLegForControlSwitch), so switching away and back
// put the session's position 2,636 units behind the body, and the next click on the NPC
// the character was standing beside walked the whole 5.3 s leg again. The owner's own
// words: "character 1 is near the NPC, but if i change leader to character 2 and then
// change back to 1 and click the NPC, there will be a visible delay". Writing Team[caster]
// unconditionally is what makes the two records agree, exactly as keepWalkedLeg does for
// a cast's or an attack's walk (attack_position.go:351).
func (s *Session) landClickArrival(w *walk, at [3]int32, now time.Time) {
	if w.caster >= 0 && w.caster < len(s.Team) {
		s.Team[w.caster].Position = at
	}
	if w.caster == s.Control {
		s.Pos = at
		s.moveFrom, s.moveAt = at, now // the walk is over: no segment
	}
}

func (h *Handler) finishWalk(sess *Session, w *walk, now time.Time) [][]byte {
	var out [][]byte
	body := w.bodyAt(now)

	if w.cast != nil || w.atk != nil || w.pick != nil {
		if !w.castDone {
			w.castDone = true
			tag, what := "zone: SKILL APPROACH", "the cast it walked for"
			if w.atk != nil {
				tag, what = "zone: ATTACK APPROACH", "the attack it walked for"
			}
			if w.pick != nil {
				tag, what = "zone: PICKUP CHASE", "the pickup it walked for"
			}
			h.Log.Printf("%s: the body has walked to the stand point (%d, %d, cell %d) "+
				"after %s (eta %s) - firing %s",
				tag, w.goal[0], w.goal[1], w.goal[2], now.Sub(w.began).Round(time.Millisecond),
				w.eta.Round(time.Millisecond), what)
			// THE CAST PLACES THE CASTER, THE ATTACK DOES NOT (R104). A cast's builders
			// and its range check read the caster's stored position, so its placement is
			// what that lane is written against. An attack is built from the BODY: nothing
			// is placed, because writing the goal would move the actor on screen for a
			// body that had not walked it.
			switch {
			case w.cast != nil:
				h.placeWalker(sess, w.caster, w.goal)
				out = append(out, h.firePendingCast(sess, w.cast)...)
			case w.pick != nil:
				// THE PICKUP RUNS FROM THE BODY, like a hit: nothing is placed, and the
				// transaction is the client's own frame replayed through pickItem
				// (zone\pickup.go, firePickup).
				out = append(out, h.firePickup(sess, w, now)...)
			default:
				// NO `from`: this close did not measure the reach, the held press's own
				// check did, so attackAt reads the body's drawn point and its R98 re-plan
				// stands. The clock is read HERE, at the build (R111).
				built, _ := h.firePendingAttack(sess, w.atk, nil, time.Now())
				out = append(out, built...)
			}
		}
		// A FIRE THAT WALKED AGAIN OWNS THE SESSION NOW (R98). replanAfterWalk installs a
		// NEW walk, planned from the body's own walked point, when the answer was "still out
		// of reach". That walk is the live one and its own arrival will close it - so this
		// close must not clear it, and must not write this leg over the point the new plan
		// started from. Before piece 2d the fire and the close were on two clocks, and the
		// earlier one always won; now they are the same tick, so the order is stated here.
		if cur := sess.approachOf(w.caster); cur != nil && cur != w {
			return out
		}
		sess.keepWalkedLeg(w)
		sess.clearApproach(w)
		h.stopWalker(sess, w.caster, w.goal)
		return out
	}

	// A CLICK'S BOX. The client's last report is checked FIRST: a report far from the goal
	// means the player walked somewhere else, and a modal box must not appear for a body
	// standing 500 units from the NPC.
	if w.hasReport && w.lastDist > ArriveRadius {
		h.Log.Printf("zone: the body walked to its stand point, but the client last "+
			"reported %.0f units from the goal (it walked elsewhere) - NO dialog",
			w.lastDist)
		sess.Walk = nil
		// NO stop here. The position to name would be the DESTINATION the client last
		// reported, which is not where the character stands, and a stop at a point the
		// character never reached is exactly the jump this lane removes (R63). The player's
		// own reported walk has its own segment, and that segment sends its own stop when
		// it is walked out (move.go, endWalks).
		return out
	}

	// THE PURPOSE'S OWN NEAR-TEST, on the body the client has drawn. For a click it is
	// the dialog's IsNearObject(target, DISTANCE_TO_CHAR*2): the goal was taken from the
	// target's shape, so arriving at it is well inside 200 - and if a plan ever hands back
	// a point that is NOT, nothing opens and the line says why. That is the guard the
	// reference's `else SetActionState(AS_DIALOG)` really is.
	if gap, near := w.nearTarget(body); !near {
		h.Log.Printf("zone: the body walked to (%d, %d, cell %d) but is %g units from %s's "+
			"shape, outside the dialog's own near test of %g - NO dialog, and the stand "+
			"point was wrong (path engine clamp?)",
			w.goal[0], w.goal[1], w.goal[2], gap, describeNPC(w.npc), w.near)
		sess.Walk = nil
		h.stopWalker(sess, w.caster, w.goal)
		return out
	}

	// THE ARRIVAL, IN ONE LINE, ON THE TICK IT HAPPENS. It carries the distance to the
	// target's shape, because that is the number the arrival is judged by.
	gap, _ := w.nearTarget(body)
	answer := "opening the dialog"
	if !w.opensDialog() {
		answer = "the switch cancelled this click's box, so NOTHING opens"
	}
	h.Log.Printf("zone: ARRIVED at %s (0x%08x) after %s (eta %s)%s - the body is %g units "+
		"from its shape (near test %g) at (%d, %d, cell %d) - %s",
		describeNPC(w.npc), w.npc.Handle, now.Sub(w.began).Round(time.Millisecond),
		w.eta.Round(time.Millisecond),
		map[bool]string{true: ", overlap", false: ""}[w.overlap],
		gap, w.near, w.goal[0], w.goal[1], w.goal[2], answer)
	sess.landClickArrival(w, w.goal, now)
	// THE WALK IS OVER BEFORE THE SCRIPT RUNS, which is the order the old stop clock used
	// and it matters: a click script may itself ask for a walk, and clearing the record
	// after it would throw that walk away.
	sess.Walk = nil
	// A CANCELLED CLICK OWES NO BOX (zone\control.go): the body arrived, the stop below
	// still goes out, and nothing opens. The script is not run either - on the live server
	// the whole request is gone, not just its window.
	if w.opensDialog() {
		out = append(out, h.openNPCDialog(sess, w.npc)...)
	}
	// The remote actors stop at the goal, with this player's own client - on the same
	// tick, because this is the tick the body got there.
	h.stopWalker(sess, w.caster, w.goal)
	return out
}

// queueClick starts the walk a click on an out-of-reach NPC asked for, AT ONCE.
//
// NO IDLE WAIT (piece 2d, item 3). It used to park the click in sess.Pending and wait
// PlayerIdleWindow (250 ms) of silence from the client, because a ground click leaves the
// client streaming move reports for a second or more and a chase sent on top of them was
// cancelled (measured 2026-10-25). The reference does not wait for anything: its
// DispatchDialog sets AS_DIALOG_CHASE, and that state walks the character on its very
// next update (PlayerChar_Request.cpp:436-444, PlayerChar_FSM.cpp:340-372). The tail of
// the player's own walk is handled where it belongs and already existed - ChaseGrace
// (1500 ms) makes a report inside that window the tail of the player's walk rather than a
// new instruction (observeWalk).
//
// WHAT A CLICK DURING A WALK DOES: pointing at a DIFFERENT NPC REPLACES that walk,
// because that is what the player asked for. A REPEAT CLICK ON THE NPC THIS WALK IS
// ALREADY SERVING is absorbed and sends nothing - it is the same order said twice, and
// re-planning it restarts the arrival clock.
//
// WHY THE ABSORB IS NOT A REFINEMENT, MEASURED 2026-11-03 20:36. The client re-sends a
// click about six times a second while the character walks (40 packets in 3.76 s in
// `logs\zone_go_live.log`, then 24 in 1.80 s). Each one used to re-plan the stand-off
// from the body's CURRENT sector, so the goal moved around the NPC (`180 deg W side
// (2091,-597)` -> `202 deg WSW side (2110,-663)`), the walk was re-sent or refused, and
// `walk.began` restarted - a 0.5 s leg opened its box 2.9 s later, which the owner saw
// as "warlock dialog delay when clicking NPC". Absorbing keeps ONE leg, ONE clock and
// ONE 3548, which is also what the reference's single move-request slot does
// (FieldChar.cpp:932-953, R115).
func (h *Handler) queueClick(sess *Session, npc NPC) [][]byte {
	if w := sess.Walk; w != nil {
		// The same NPC, and a CLICK's walk (not a cast or an attack approach - those
		// answer with a hit, and a click on top of them IS a change of mind).
		if w.cast == nil && w.atk == nil && w.npc.Handle == npc.Handle {
			// The player pointed at the NPC this walk already serves: whatever OTHER NPC
			// was armed is not what they want any more.
			sess.ClickLockArmed = 0
			if w.dialogCleared {
				// A leader switch cleared this click's box (cancelWalkDialog). The player
				// has now asked again, so re-arm it: the walk keeps its own clock and the
				// box opens on arrival, which is the whole of what he asked for.
				w.dialogCleared = false
				h.Log.Printf("zone: click on %s re-arms the dialog this walk already "+
					"asked for (the leader switch had cleared it) - the walk keeps its "+
					"clock, nothing is sent", describeNPC(npc))
				return nil
			}
			if w.takenOver {
				// THE PLAYER ALREADY OVERTURNED THIS WALK (2026-11-09). His own move order
				// marked it (zone.go's move branch); absorbing this click was the reason
				// click 2 did nothing while click 3 worked, so the click REPLACES the walk
				// and a fresh chase goes out.
				h.Log.Printf("zone: click on %s replaces the walk the player's own move "+
					"overtook - a fresh chase, not an absorbed repeat", describeNPC(npc))
				// NO ChaseFrom HERE: the player's own move just REPORTED a position, and a
				// report is fresher than our model (startChase's ChaseGrace arm uses it).
				// The drawn anchor is for the case where the client is silent: a redirect.
				sess.Walk = nil
				sess.forgetWalk(w.caster)
			} else {
				h.Log.Printf("zone: click on %s repeats the click this walk is already "+
					"serving - absorbed: one leg, one arrival clock, no packet (a re-planned "+
					"stand-off would move the goal and restart the clock)", describeNPC(npc))
				return nil
			}
		} else {
			// THE REFERENCE'S CLICK LOCK (history\zone-npc-click.md, 2026-10-16 - "A click
			// LOCKS on to its NPC"). A click on a DIFFERENT NPC while this walk runs does NOT
			// redirect the actor. The FIRST one only ARMS that NPC and drops the box this
			// walk was going to open - the owner's own words: "clicking another NPC while
			// the character is still moving to the initial NPC will just cancel the dialog
			// and not the move itself" - so the actor keeps walking to the NPC it was locked
			// on to. Only a SECOND click on that same other NPC cancels the walk where the
			// body stands and redirects it: "click NPC A and then click NPC B twice, it
			// should immediately target NPC B".
			if sess.ClickLockArmed == npc.Handle {
				h.Log.Printf("zone: the second click on %s redirects the walk from %s - "+
					"the actor stops where it stands and chases the new NPC",
					describeNPC(npc), sess.Walk.describe())
				sess.ClickLockArmed = 0
				sess.ChaseFrom, sess.ChaseFromSet = sess.Walk.bodyAt(time.Now()), true
				sess.parkApproach()
			} else {
				sess.ClickLockArmed = npc.Handle
				w.dialogCleared = true
				h.Log.Printf("zone: click on %s ARMS it - the walk to %s keeps going and "+
					"this box is dropped; click %s again to redirect",
					describeNPC(npc), sess.Walk.describe(), describeNPC(npc))
				return nil
			}
		}
	}
	return h.startChase(sess, npc)
}

// observeWalk uses the leader's OWN position report as the arrival signal.
//
// WHAT THIS CLIENT REALLY SENDS (measured 2026-10-29, R64, and it corrects this
// comment's earlier claim): during a SERVER-driven walk the leader sends nothing at
// all. `logs\zone10_live.log` holds 13 chases and 393 `MOVE_PC char 0` reports, and
// not one leader report falls inside a chase - every report in those windows belongs
// to char 1 or char 2 (the followers, which the client moves itself to follow). The
// earlier "it keeps reporting the leader every second or so" note came from a walk the
// PLAYER drove, where the client reports every path step because the player is the one
// moving it. So this function is the RIGHT answer when a report does arrive (the tail
// of the player's own walk, or a client build that does report), and it is not what
// opens a chase's box in practice: chaseTick's arrival test does that (walk.arrived).
//
// Two things come out of one report:
//   - within ArriveRadius of the goal -> ARRIVED: open the dialog now;
//   - farther from the goal than the previous report -> the player is going elsewhere:
//     cancel, after ChaseGrace so the tail of their own earlier walk cannot do it.
func (h *Handler) observeWalk(sess *Session, idx int, pos [3]int32) [][]byte {
	w := sess.Walk
	if w == nil {
		return nil
	}
	// ONLY THE WALKING CHARACTER'S OWN REPORT SAYS ANYTHING ABOUT THE WALK. A click
	// chase walks the character the player drives (w.caster, set from
	// sess.Control in zone\control.go); an approach walks whichever character
	// pressed the skill. The client drags the other two along and reports their steps
	// the whole time, so a report from another slot is noise here - and it used to be
	// worse than noise: the leader's report during a FOLLOWER's approach was measured
	// against that follower's goal, and could "arrive" it and open a dialog from an
	// empty NPC.
	if idx != w.caster {
		return nil
	}
	if w.cast != nil || w.atk != nil || w.pick != nil {
		// A SKILL APPROACH (R87): NOTHING IS CANCELLED HERE. The click's own echo has
		// already gone out (zone.go, movePC, the ordinary ZC_MOVE_PC every report
		// gets), and the approach keeps its own clock: the 3571 goes out when the walk
		// ends and the result when the cast's clock fires, from wherever the body is by
		// then.
		//
		// A NORMAL ATTACK'S APPROACH (R90) is the same: the walk is the SERVER's, so a
		// report is the client's own step and not a change of mind. The player's change
		// of mind has its own event - a move CLICK (cancelApproachAttack) or a new
		// press - and this is neither.
		//
		// THE MEASUREMENT THAT PUT THIS HERE. An earlier lane read the original
		// server's `CPlayerChar::DispatchMove` (PlayerChar_Request.cpp:345-348, gated
		// on `IsHoldAction`, which lists AS_SKILL_CASTING) and concluded "a move during
		// a cast cancels it". The live bytes contradict that inference: the reference
		// echoes the click and lands the cast anyway, and over 77 s it sends no 3549,
		// no 3568 and no cancel in either direction
		// (captures\cancel20261031\20261031-191902, history\cancel-and-animation.md
		// §2/§5). A test that asserts a cancel is asserting the source, not the server.
		return nil
	}
	d := math.Hypot(float64(pos[0]-w.goal[0]), float64(pos[1]-w.goal[1]))
	// THE STEP LINE (2026-11-09): the client's own position next to the model's, on every
	// report during a click walk. This is the measurement the timing argument needs - if the
	// two numbers disagree, the box is early or late by exactly that difference.
	if model := w.bodyAt(time.Now()); true {
		h.Log.Printf("zone: STEP of %s: the client reports (%d, %d) = %.0f from the goal; "+
			"the model's body is (%d, %d) = %.0f from it", w.describe(), pos[0], pos[1], d,
			model[0], model[1],
			math.Hypot(float64(model[0]-w.goal[0]), float64(model[1]-w.goal[1])))
	}

	if d <= ArriveRadius {
		sess.Walk = nil
		answer := "opening the dialog"
		if !w.opensDialog() {
			answer = "nothing opens, because the switch cancelled this click's box"
		}
		h.Log.Printf("zone: the client reports the character %.0f units from the goal - "+
			"arrived at %s, %s", d, w.describe(), answer)
		// The other players are owed the end of the walk HERE, not only on the clock:
		// past this point nothing else will ever send it, and without it their copy of
		// the actor animates at the NPC for ever. The stop names the position the
		// client itself reported, which is a place the body provably is.
		h.stopOthers(sess, pos)
		sess.landClickArrival(w, pos, time.Now()) // the client says it stands here
		if !w.opensDialog() {
			return nil // a cancelled click: the body arrived, the box does not
		}
		return h.openNPCDialog(sess, w.npc)
	}

	if w.hasReport && d > w.lastDist+WalkAwayDistance && time.Since(w.sentAt) > ChaseGrace {
		h.Log.Printf("zone: the client reports %.0f units from the goal (was %.0f) - the "+
			"player walked elsewhere, so the walk to %s is cancelled and NO dialog opens",
			d, w.lastDist, w.describe())
		sess.Walk = nil
		return nil
	}

	// THE CLOCK FOLLOWS THE BODY (2026-11-09, owner: "the arrival time is still being
	// calculated on my initial position rather than current click position"). The client
	// walks the player to a clicked NPC by itself and reports every step; each report is a
	// real position, so the walk is RE-ANCHORED on it - from = the reported point, the route
	// becomes the straight line to the goal, and the clock restarts on the distance that is
	// actually left. Without this the clock ran on from wherever the move was sent, and every
	// slow step of the client's own walk came out as a late box.
	left := math.Hypot(float64(w.goal[0]-pos[0]), float64(w.goal[1]-pos[1]))
	w.from, w.began, w.route = pos, time.Now(), nil
	if w.speed > 0 {
		w.eta = time.Duration(left / w.speed * float64(time.Second))
	}
	w.hasReport = true
	w.lastDist = d
	w.lastReport = time.Now()
	return nil
}

// cancelChase drops a walk in progress: the player took control.
func (h *Handler) cancelChase(sess *Session) {
	if sess.Walk == nil {
		return
	}
	// The click is dead with the walk: an NPC armed for a redirect has nothing to redirect.
	sess.ClickLockArmed = 0
	h.Log.Printf("zone: player moved during the walk to %s - walk cancelled",
		sess.Walk.describe())
	caster := sess.Walk.caster
	sess.Walk = nil
	// THE DESTINATION IS NO LONGER IN FLIGHT (2026-11-09). Without this the walksSent
	// record kept claiming the point until the cancelled walk's own clock ran out, and a
	// fresh click on the same NPC was refused as a duplicate - the second half of the
	// owner's "click 2 does nothing".
	sess.forgetWalk(caster)
	// A segment the client opened DURING the chase is dropped with it: it describes a
	// path step of the walk we just cancelled, and letting it close later would stop
	// the other players at a place the character has already left (R63).
	//
	// RECORDED FIRST (R91 round 7) for the same reason as the two approach cancels: a
	// cancelled walk plans no arrival, so nothing else would write this character's live
	// point. It is a no-op for the leader, which is every click chase.
	sess.rememberWalk(caster, time.Now())
	sess.closeWalk(caster)
}
