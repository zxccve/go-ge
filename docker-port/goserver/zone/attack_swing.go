package zone

import (
	"strconv"
	"time"

	"geserver/protocol"
)

// THE REPEAT: ONE CLICK, THEN A SWING AT THE CHARACTER'S OWN ATTACK SPEED (R94).
//
// THE OWNER'S REPORT, and why it is a SERVER bug and not a client one: "right now it
// only attack once despite having target locked. the normal attack seems to wait for
// my clicking input each time to register a single attack."
//
// WHO DRIVES THE RHYTHM - the question this file had to answer before it wrote a line
// of code, because a loop the client already drives would double every hit. THE CLIENT
// SENDS ONE 3666 PER LOCK AND THEN STOPS. Its own attack is a tactics state that fires
// the attack ONCE, when the state is ENTERED:
//
//	GeBaseObject.cpp:919-927     AIM_ATTACK -> OnHandleAiAttack(pTgt)
//	GENormalObject.cpp:640-655   OnHandleAiAttack -> packet::CZ_Attack(index, handle)
//	GeAiActor.cpp:930-934        CAiActor::Attack -> ONE AIM_ATTACK
//	GeAiActor_FSM.cpp:125-138    AttackTarget(): called from a state's ON_ENTER
//	ge\ai\attacker.scp           SCR_AI_LIB_ATTACKTARGET(...) BEFORE `while(1)`; nothing
//	                             inside that loop calls it again
//	ge\ai\lib.scp:118-131        SCR_AI_LIB_ATTACKTARGET -> Attack(selfAi, curTarget)
//
// The state's ON_UPDATE only asks "is the target still valid"; nothing in it swings.
// A client left to repeat would send a 3666 per frame, and the capture shows the
// opposite: 20 requests for a whole fight (history\combat-attack-heal.md §1). So THE
// RHYTHM IS THE SERVER'S, and the reference server says so in its own code:
//
//	FieldChar.cpp:953-990        DispatchAttack: ONE client request stores
//	                             `m_attackTarget = target` and enters AS_ATTACK_CHASE
//	FieldChar_FSM.cpp:121-173    AS_ATTACK ON_ENTER: ProcessAttack(target), then
//	                             `m_nextAttackTime = now + GetAttackSPD()`; ON_UPDATE
//	                             waits for that moment, then AS_ATTACK_PREPROCESS ->
//	                             AS_ATTACK -> another hit
//	FieldChar.cpp:1302-1304      GetAttackSPD() IS GetASPD() - the same number
//	                             zone\attack.go already writes into 3564's +20
//
// SO THIS FILE IS THAT LOOP, AND NOTHING ELSE ABOUT THE HIT CHANGES. Every swing goes
// through Handler.attackAt: the same 3564 and the same 3557, in the same order, from
// the same damage function, against the same HP ledger, with the same R93 fan-out. The
// repeat decides WHEN, never WHAT.
//
// ONE REPEAT PER CHARACTER, NEVER A QUEUE. Session.swings has one slot per team slot,
// exactly like Session.casts: arming again REPLACES that slot. So a client that sends
// 3666 for the target it already holds - its tactics state re-enters after a click, and
// the reference has the same re-send in PlayerChar_Request.cpp:1416-1421 - cannot start
// a second rhythm, and cannot double-hit either: the hit that request earns re-arms the
// ONE slot to `last hit + ASPD`, and the tick below only fires once that moment has
// come, which is the same gate Handler.attackAt already applies to a press.
//
// WHAT ENDS IT, each one a rule the reference states in code:
//
//	target gone     AS_ATTACK ON_UPDATE: `GetAttackTarget()` NULL ->
//	                SendAttackDisabled(INVALID_TARGET) + AS_NONE (:141-146)
//	target dead     ON_UPDATE: `BS_DEAD == GetBodyState()` -> AS_NONE (:136-140); this
//	                zone's HP ledger reaching 0 is the only death it can see (it sends
//	                no 3531 - that is another lane)
//	stance change   PlayerChar_Request.cpp:1364-1368: AS_ATTACK -> AS_NONE when the new
//	                stance's AI is not the attacker AI
//	skill cast      SetActionState(AS_SKILL_*) leaves AS_ATTACK, and ProcessAttack
//	                selects the plain-attack path only while the character is in it
//	                (PlayerChar.cpp:1987-2002)
//	disconnect      Handler.leaveZone drops it beside the pending casts
//
// A MOVE IS NOT ON THAT LIST (R105) - BUT A PLAYER MOVE ORDER ENDS THE ENGAGEMENT AND
// DROPS THE SWING THAT HAS NOT BEEN BUILT YET (R108, corrected 2026-11-03). The reference
// keeps the attack as an ACTION with a duration - the FSM re-decides once it is over, and
// nothing in FieldChar_FSM.cpp or PlayerChar.cpp aborts the attack action because the body
// was told to walk - so a swing ALREADY on the wire lands at its cadence and the body walks
// where the player pointed. What it may not do is build a NEW swing after the click: that
// swing's 3564 +8 is a point on the walk the player just ordered, and the client's 3564
// handler hard-sets the actor there and calls SetMoveState(CMS_STOP), so the click is
// answered with a stop (durable fact #219, the owner's "stuck now"). A swing already
// written to the wire survives, because this side cannot un-send it.
//
// AND WHERE THE BODY STANDS IS NOT A REASON TO END IT EITHER (R106). The repeat is an
// ENGAGEMENT, not a countdown that dies when the body drifts: every tick measures the
// EDGE distance and does one of three things - too near, step BACK to the min ring;
// too far, walk IN; inside the band, swing on the cadence - and none of those ends it.
// That is the reference's own AS_ATTACK: its update re-decides IsAttackArea every pass
// and turns -1 into GetAttackMinPos and +1 into GetAttackMaxPos (FieldChar_FSM.cpp:
// 28-80, 155-170), never into AS_NONE. The owner's log is why: the bodies overlap
// because the player walks right up to the monster, the old swing refused with
// "inside the stance's minimum reach", and the repeat ended after 1-8 hits - so every
// few seconds the attack "stopped" until a fresh press.
//
// THE ONLY ENDINGS LEFT are the four the PM card names: the target dying, the target
// leaving this zone, a different target being nominated (attack.go), and the body being
// carried CLEAR of the reach by the player's own movement - and that last one only when
// it is more than attackDragSlack units past MaxR for longer than attackDragHold, so a
// half-second reposition does not kill the engagement.
//
// WHAT IT DOES NOT DO, said out loud so a reader does not look for it:
//
//   - NO SECOND MOVEMENT SYSTEM. The repeat DOES move the body now (R106), but only
//     through the ONE movement path a normal attack already has: Handler.approachAttack
//     (zone\attack_approach.go), the same stand rings, the same 300 ms re-decision pace,
//     the same walk-once de-duplication and the same fire-on-arrival. A step-back never
//     asks the path engine at all - it is attackRetreatPoint's arithmetic (R104). So
//     there is still exactly one clock and one walk rule for a normal attack; what
//     changed is that the repeat is allowed to use it instead of dying.
//   - NO DOUBLE. datatable_stance.xml holds 486 Single, 174 Splash and 1 Double stance,
//     and the reference branches on that column (PlayerChar.cpp:1987-2002): a Double
//     stance lands two hits per swing, a Splash stance hits every body inside SplRange.
//     THE SPLASH SHAPE IS NOW BUILT: a swing in a Splash stance replays the same
//     press through zone\attack_splash.go, which answers 3566 instead of 3564 and
//     recomputes the circle from the bodies' positions AT SWING TIME - so the repeat
//     splashes exactly what a fresh press would, and a monster that walked out of the
//     circle is not hit by the rhythm. DOUBLE STILL IS NOT BUILT: it would be a SECOND
//     hit inside one swing, and no capture holds the packet that second hit would be -
//     inventing one is worse than refusing it, so those stances keep their single press
//     and get no repeat.
//   - NO HOLD-ATTACK BUFF RULE, YET. `IsHoldAttackAction()` reads the buff list
//     (FieldChar.cpp:1092-1100) and the zone now HAS one (zone\buff.go, buff slice 1),
//     but no buff row is applied yet on this path and the repeat does not consult it.
//     A buff whose row says `HoldAttack=YES` therefore does not stop a swing today -
//     a named GAP, not a rule. The stance column the zone DOES read is HoldMove.
//   - No death, no drops, no EXP, no 3531 - unchanged from zone\attack.go.

// swingLoop is ONE character's running normal-attack repeat.
//
// It is written and read ONLY by the connection's own goroutine (Serve): the press
// arms it, the tick advances it, a stance change/cast/disconnect/death ends it. Nothing
// here needs a mutex, and nothing here may be touched from another goroutine - the same
// rule Session.casts keeps.
type swingLoop struct {
	// target is the monster handle the player locked. It is the handle the client put
	// in its own 3666 at +11, never one this side guessed.
	target uint32

	// stance is the stance the character stood in when this record was last armed. A
	// character whose stance no longer matches has left the attack state on the
	// client's side too, so the repeat ends.
	stance uint32

	// frame is the client's own CZ_ATTACK, replayed verbatim by every swing: same
	// handler, same parser, same checksum arithmetic, so a swing cannot be built by a
	// second code path that drifts from the click. The bytes are COPIED on the way in -
	// the splitter hands out slices that alias one decrypted chunk, and this record
	// outlives that chunk by seconds.
	frame protocol.Frame

	// next is the earliest moment the next swing is owed: `last hit + ASPD`. It is
	// never a fixed delay counted from the tick - it is always that sum, so a tick that
	// runs late cannot stretch the rhythm and a tick that runs early cannot shorten it.
	next time.Time

	// began is the moment the click that started this repeat landed its first hit.
	began time.Time

	// hits counts the hits this repeat has produced, the click's own hit included.
	// It is for the log only: "swing 4" is how a reader tells a working repeat from a
	// single hit with noise around it.
	hits int

	// farSince is when this tick first measured the body MORE THAN attackDragSlack
	// units past MaxR (R106). It is the hysteresis the PM card asks for: a body dragged
	// clear for a moment is a reposition and the repeat keeps walking it back in, and
	// only a body kept clear for longer than attackDragHold ends the engagement. It is
	// cleared the moment the body is back within the band, so a second drag starts its
	// own second.
	farSince time.Time

	// retryAt is the earliest moment a tick may try another swing after one built
	// NOTHING (R106). It exists for one reason: the repeat no longer ends when a swing
	// comes back empty, and a tick that comes back empty every poll would spin the read
	// loop. It is never the rhythm - `next` is the rhythm - and armSwing zeroes it on
	// every hit.
	retryAt time.Time

	// walkGoal is the stand point this ENGAGEMENT last ORDERED a walk to, walkGoalAt is
	// when that order went out, and walkTriedAt is when the repeat last ASKED for one (a
	// refusal counts as an ask). They are the reference's own m_moveTo and
	// m_nextAttackChaseTime (FieldChar_FSM.cpp:38-42, :45-52) kept on the ENGAGEMENT
	// instead of on the walk.
	//
	// WHY NOT ON THE WALK: sess.Walk is gone as soon as the walk's hit fires
	// (fireHeldAttackOnArrival marks it castDone and the tick's guard needs `!castDone`) or
	// a click cancels it, and the tick then has nothing left to compare against. The
	// reference keeps m_moveTo past the end of its walk for exactly this reason:
	// `IsEqualPos(moveTo, m_moveTo)` is compared against the LAST point it ordered, not
	// against the walk in flight.
	//
	// THE MEASUREMENT (owner's session, logs\go_zone1331.log, 2026-11-02): between
	// 22:18:13 and 22:18:51 the repeat ordered 30 walks back to the monster, and 12 of
	// them went to the IDENTICAL point (1779, -529) between 22:18:35 and 22:18:49 - one
	// every 1.1 s, because each walk's hit had already fired and closed it before the
	// next tick measured.
	walkOrdered bool
	walkGoal    [3]int32
	walkGoalAt  time.Time
	walkTriedAt time.Time

	// walkHeld counts the ticks that ordered NOTHING because the stand point had not
	// changed, and walkSaidAt is when that count was last written. Both are the R97
	// absorb rule's shape (absorbed/absorbedAt): the tick polls several times a second,
	// so one line per held tick would be the log flood this rule exists to remove.
	walkHeld   int
	walkSaidAt time.Time

	// cancelReason is set by a PLAYER MOVE ORDER (Handler.noteMoveOrder, from
	// zone.go's MOVE_PC) - a move report from THAT character that is not a walk
	// this zone sent, whatever its length, and for whichever slot of the team.
	//
	// IT MEANS "THE SWING THAT IS DUE GOES OUT FIRST, AND THEN NOTHING" (2026-11-03, the
	// owner's rule: "it will 9/10 let the damage go through and then the character
	// walk"). The order does NOT drop the hit. The swing that has already come due at
	// that instant is built from the body AS IT WAS BEFORE THE REPORT and put on the wire
	// ahead of the move echo in the same reply (Handler.drainMoveOrderSwing), so the
	// damage lands while the body still stands on the leg it was walking.
	//
	// A SWING THAT HAS NOT COME DUE IS NEVER BUILT AFTERWARDS. A 3564 built on the leg
	// the report just opened carries a +8 the client has not walked to, and its handler
	// hard-sets the actor there and calls SetMoveState(CMS_STOP) - the frozen walk in the
	// owner's "stuck now" (durable fact #219). So the mark is drained ONCE, at the order,
	// and whatever it did not build then is not built later: the tick only ends the
	// record with this text.
	cancelReason string
}

// swingOf is character idx's running repeat, or nil.
func (s *Session) swingOf(idx int) *swingLoop {
	if idx < 0 || idx >= len(s.swings) {
		return nil
	}
	return s.swings[idx]
}

// hasSwing reports whether ANY character of this team still has a repeat running. It is
// what makes Session.wantsTick wake the read loop up (zone\move.go): this zone drives
// nothing itself, so without a tick the next swing would wait for the client's next
// packet - which, per the header, never comes until the player clicks again.
func (s *Session) hasSwing() bool {
	for _, w := range s.swings {
		if w != nil {
			return true
		}
	}
	return false
}

// armSwing (re-)arms character idx's repeat after a hit on `target`. It is called from
// the ONE place a hit exists - Handler.attackAt, right after `noteAttack` - so the
// press, the replay at the end of an approach walk and every swing of the repeat all
// come through here and cannot disagree about the cadence.
//
// IT ARMS NOTHING WHEN THE SHEET GIVES NO ATTACK DELAY (`interval` 0). An untimed
// rhythm would be one made-up number pretending to be the game's timing; attackAt has
// already logged "ATTACK NOT THROTTLED" once for that case, and this record simply does
// not exist.
func (h *Handler) armSwing(sess *Session, idx int, target, stance uint32, f protocol.Frame,
	interval time.Duration, now time.Time) {

	if idx < 0 || interval <= 0 {
		return
	}

	// ONLY THE SHAPES THIS BUILD CAN BUILD (R95 item 2). Every swing replays the client's
	// own press through Handler.attackAt, which builds exactly what a fresh press would:
	// a Single stance gets the 3564, and a Splash stance gets the 3566 plus its extra
	// victims (zone\attack_splash.go) - the circle is recomputed at swing time, so
	// the rhythm splashes what stands in it NOW and not what stood there at the click.
	// The reference branches on the same column (PlayerChar.cpp:1987-2002): a Double
	// stance lands two hits per swing. DOUBLE IS STILL NOT BUILT - a second hit with no
	// measured packet behind it - so arming a repeat there would put a stream of SINGLE
	// hits behind a name that promises two, and the player would see one body of a pair
	// taking damage for ever. The press itself is untouched: it keeps its one hit.
	//
	// THE THREE ANSWERS, and why the empty one still arms: a stance with NO AtkType (no
	// table, no row, or a row that states no type) is not evidence of a Double or a
	// Splash - the press already went out as a Single-shaped hit on exactly this
	// information - so the repeat keeps today's behaviour rather than inventing a
	// refusal. A row that NAMES another type is evidence, and that one is refused.
	//
	// EACH REFUSAL IS SAID ONCE PER RUN, through the same Swap(1) trick as
	// logNoCadenceOnce: it is a fact about the stance column this build loaded, not about
	// each click.
	switch atkType := h.Stances.Combat(stance).AtkType; atkType {
	case "Single", "Splash":
		// the two shapes this build has: arm, nothing to say
	case "":
		if h.swingNoAtkTypeLogged.Swap(1) == 0 {
			h.Log.Printf("zone: ATTACK REPEAT NOT BUILT: stance %d (%q) states no AtkType in "+
				"the stance table (or this zone has no table), so the repeat keeps today's "+
				"behaviour and arms the Single shape (said once per run)",
				stance, h.stanceName(stance))
		}
	default:
		if h.swingUnsupportedAtkTypeLogged.Swap(1) == 0 {
			h.Log.Printf("zone: ATTACK REPEAT NOT BUILT: stance %d (%q) is AtkType %q, and "+
				"the repeat only builds the Single shape - the press keeps its one hit and "+
				"no repeat is armed for that attack type (said once per run)",
				stance, h.stanceName(stance), atkType)
		}
		return
	}

	if len(sess.swings) <= idx {
		grown := make([]*swingLoop, len(sess.Team))
		copy(grown, sess.swings)
		sess.swings = grown
		if idx >= len(sess.swings) {
			return // a hit for a character this team does not have
		}
	}

	data := make([]byte, len(f.Data))
	copy(data, f.Data)
	copied := protocol.Frame{Cmd: f.Cmd, Size: f.Size, Data: data}
	next := now.Add(interval)

	if old := sess.swings[idx]; old != nil {
		if old.target != target {
			h.Log.Printf("zone: ATTACK REPEAT: commander %d char %d (%d) swings at target "+
				"%d now, not %d - ONE repeat per character, so the slot follows the new "+
				"target (the old one had %d hit(s) over %s)",
				sess.CommanderID, idx, sess.Team[idx].Handle, target, old.target,
				old.hits, time.Since(old.began).Round(time.Millisecond))
			sess.swings[idx] = &swingLoop{
				target: target, stance: stance, frame: copied,
				next: next, began: now, hits: 1,
			}
			return
		}
		// The same target: this hit IS the repeat's next swing (or a re-press that the
		// cadence gate let through). Either way the slot is re-armed, never duplicated.
		old.stance = stance
		old.frame = copied
		old.next = next
		old.retryAt = time.Time{}
		old.hits++
		return
	}

	sess.swings[idx] = &swingLoop{
		target: target, stance: stance, frame: copied,
		next: next, began: now, hits: 1,
	}
	h.Log.Printf("zone: ATTACK REPEAT: commander %d char %d (%d) holds target %d and now "+
		"swings every %s - the character's own ASPD (SCR_Get_ASPD get_status_pc.scp:1408), "+
		"not a number this zone chose. ONE client %d started it and the reference's own "+
		"%s loop is what keeps hitting (FieldChar_FSM.cpp:121-160). It is an ENGAGEMENT "+
		"(R106): being too near or too far MOVES the body and never ends it. It ends on the "+
		"target leaving or dying, a different target being pressed, the body being carried "+
		"clear of the reach by the player's own movement (more than %g units past MaxR for "+
		"more than %s), a PLAYER MOVE ORDER (ANY move report from THAT character "+
		"that is not a walk this zone sent: the swing is DROPPED at the order and only a "+
		"swing already sent survives), a stance change to a stance that is not the attacker AI, a "+
		"cast, or the connection ending - and on nothing else",
		sess.CommanderID, idx, sess.Team[idx].Handle, target, interval, CmdCZAttack,
		"AS_ATTACK", attackDragSlack, attackDragHold)
}

// attackDragSlack and attackDragHold are the hysteresis on the one ending that is about
// position (R106): the body must be MORE than 100 units past MaxR, and it must STAY
// there for longer than one second, before the engagement is ended by the player's own
// movement. Anything shorter is a reposition - a player clicking past the monster, a
// step that overshot - and the next tick simply walks back in.
const (
	attackDragSlack = 100.0
	attackDragHold  = time.Second
)

// noteMoveOrder marks character idx's engagement as CANCELLED by a player move order of
// `leg` units (ANY report from `idx` that is not a walk this zone sent, measured by
// zone.go's MOVE_PC path; the leg's length is carried for the log only).
//
// EVERY SLOT SPEAKS FOR ITSELF (2026-11-02): the client sends one move report per
// character, so idx need not be sess.Control. A follower's own report is that character's
// order, and it ends that character's engagement and nobody else's.
//
// IT BUILDS NOTHING ITSELF AND DELETES NOTHING, and both halves are the point:
//
//   - Nothing is deleted, because the swing that was ALREADY DUE must still build and
//     land. The wire proves it: durable fact #236 - of 8 attack+move windows on the live
//     server, 7 produced damage and in 5 of them the damage came BEFORE the move echo
//     (captures\longwalk\20261103-173112: move report -> damage at +165 ms and +233 ms,
//     move echo at +251 ms and +371 ms). A record removed here would take that hit with
//     it, which is the old bug in the other direction.
//   - Nothing is sent here, because the hit has to be built on the body the character was
//     walking BEFORE this report, and that is Handler.drainMoveOrderSwing's job - it is
//     called from the MOVE_PC path in the same reply, BEFORE the move's own leg is
//     written down (zone.go). Building it here would read a body state this function
//     cannot vouch for.
//
// THE ORDER DROPS NOTHING AND THE TICK BUILDS NOTHING. Whatever drainMoveOrderSwing did
// not build at the order - because the cadence had not come, or the body was already out
// of the stance's band - is never built afterwards: the tick ends the record with this
// text (swingTick's cancelled branch). A 3564 built on the NEW leg is a point the client
// has not walked, and its +8 stops the player's walk (fact #219).
func (h *Handler) noteMoveOrder(sess *Session, idx int, leg float64) bool {
	// A MOVE ORDER CLEARS A KEPT PRESS FIRST, AND THAT IS THE WHOLE OF ITS ANSWER TO ONE
	// (zone\attack.go, holdPress). The reference's slot is single: a move order writes
	// the character's own request, so the stored attack can no longer fire
	// (FieldChar.cpp:955-982 - a new request replaces the slot, and `ProcessRequest`
	// runs THAT one). It is done before the repeat's own record is looked at, because a
	// press can be kept with no repeat running at all - a first press inside a delay the
	// previous hit opened.
	h.clearPress(sess, idx, "the player gave a move order of "+
		strconv.FormatFloat(leg, 'f', 0, 64)+" u, and a move replaces whatever the "+
		"character was asking to do")
	s := sess.swingOf(idx)
	if s == nil || s.cancelReason != "" {
		return false
	}
	why := "the player moved " + strconv.FormatFloat(leg, 'f', 0, 64) + " u away " +
		"(move order) - a move no matter how small is a cancel, so this engagement is " +
		"over: no new swing is scheduled and the body is not walked back toward target " +
		strconv.Itoa(int(s.target)) + " (keep mode is over until a new attack press or a " +
		"new target)"
	s.cancelReason = why
	// ONE LINE PER CANCELLED ENGAGEMENT, AND IT NAMES THE SLOT AND THE LEG (2026-11-02).
	// A follower's ending says the report was THAT CHARACTER'S OWN - otherwise a reader of
	// a team fight cannot tell which of the three stopped swinging and why.
	own := ""
	if idx != sess.Control {
		own = " (that was character " + strconv.Itoa(idx) + "'s OWN move report, not char " +
			strconv.Itoa(sess.Control) + "'s)"
	}
	h.Log.Printf("zone: ATTACK REPEAT: commander %d char %d (%d) got a PLAYER MOVE ORDER "+
		"of %g u for target %d%s - a move no matter how small is a cancel: the swing that "+
		"has already come due at this instant is built from the body as it was BEFORE the "+
		"move and sent AHEAD of the move echo, and nothing further is built (no new swing, "+
		"and no walk back toward the target); a swing that has not come due is never built",
		sess.CommanderID, idx, sess.Team[idx].Handle, leg, s.target, own)
	return true
}

// drainMoveOrderSwing answers the ONE thing a move report that cancels an engagement owes
// the wire BY ITSELF: the swing that has ALREADY COME DUE at that instant, built from the
// body AS IT WAS BEFORE THE REPORT and returned so the caller can put it in front of the
// move echo.
//
// THE ORDER IS THE OWNER'S, AND IT IS THE REFERENCE'S: "can the damage go through and then
// initiate the walk? i think the reference does that? it will 9/10 let the damage go
// through and then the character walk. essentially nothing is cut/abrupt" (2026-11-03).
// Measured on the live server as durable fact #236: of 8 attack+move windows, 7 produced
// damage and 5 of those put the damage BEFORE the move echo
// (captures\longwalk\20261103-173112: move report -> damage +165 ms / +233 ms, move echo
// +251 ms / +371 ms).
//
// WHAT WAS WRONG BEFORE THIS (measured, logs\zone_go_live.log 18:52:06):
//
//	18:52:06.738392 MOVE_PC char 1 -> (2515, -760) | body was at (2412, -588)
//	18:52:06.838803 SWING POS char 1 pos (2437, -630) from segment (2412, -588) -> (2515, -760) elapsed 100ms
//
// The swing was built by the TICK 100 ms after the move, and its +8 was read along the
// NEW leg - 49 u of travel the client had never made. The 3564 handler hard-sets the actor
// to +8 and sets CMS_STOP (fact #219), so the client was snapped forward and stopped, and
// no move report came back for 43 s while the owner kept clicking. The hit belongs at
// (2412, -588): the body before the move, on the leg it was walking.
//
// SO THE CALLER MUST ASK THIS BEFORE IT APPLIES THE MOVE. That is why the function takes
// no position: it measures the body through attackBodyPoint, which reads the leg the
// character is on RIGHT NOW, and the MOVE_PC path in zone.go calls it before the leg this
// report opens is written down (sess.moveFrom / sess.Pos / sess.openWalkTo). The measured
// point is handed to attackAt as its `from` (R103: one measurement, the packet's +8 and
// the log line name the same point), so the 3564 cannot describe a body this check did
// not read.
//
// THE CONDITIONS are the tick's own: the target is still here and alive, the sheet still
// gives a cadence, no walk of this engagement owns the next hit, the drawn body is inside
// the stance's band, the cadence has come (`next`) and the last empty swing's back-off has
// passed (`retryAt`). When any of those says no, nothing is built - and nothing is built
// later either: swingTick's cancelled branch only ends the record.
//
// It ends the engagement itself once the swing has built, so one record can never be
// drained twice.
func (h *Handler) drainMoveOrderSwing(sess *Session, idx int, now time.Time) [][]byte {
	if idx < 0 || idx >= len(sess.Team) {
		return nil
	}
	s := sess.swingOf(idx)
	if s == nil || s.cancelReason == "" {
		return nil
	}
	c := &sess.Team[idx]
	if c.Stance != s.stance && !h.Stances.IsAttackerAI(int(c.Stance)) {
		return nil // a stance change ends it without a swing; swingTick says so in its own words
	}
	mon, ok := h.monsterByHandle(s.target)
	if !ok {
		return nil
	}
	if hp, known := h.monsterLiveHP(s.target); known && hp <= 0 {
		return nil
	}
	interval := attackInterval(h.sheetFor(*c))
	if interval <= 0 {
		return nil
	}
	if w := sess.approachOf(idx); w != nil && !w.castDone && w.atk != nil &&
		w.atk.target == s.target {
		return nil // the walk in flight carries this hit (fire-on-arrival)
	}
	// THE ONE MEASUREMENT: the body on the leg it is walking NOW, at the instant of the
	// report. Everything below - the band check, the packet's +8 and the log line - is
	// this one reading, so they cannot disagree.
	body := sess.attackBodyPoint(idx, now, h.packetSpeed(c))
	pos := body.pos
	minR, maxR := h.stanceRange(c.Stance)
	targetPos := [3]int32{mon.X, mon.Y, mon.Cell}
	_, inBand := attackRangeAllowed(pos, targetPos, minR, maxR, PCBodyRadius,
		h.monsterBodyRadius(mon))
	if !inBand || now.Before(s.next) {
		return nil // out of the band, or not owed yet: nothing is built, then or later
	}
	if !s.retryAt.IsZero() && now.Before(s.retryAt) {
		return nil // the last swing built nothing; give the reason its cadence
	}
	before := sess.lastAttack(idx)
	out := h.fireSwingFrom(sess, idx, s, now, &body)
	if len(out) == 0 || !sess.lastAttack(idx).After(before) {
		return nil // nothing built: the mark stays, and swingTick only ends the record
	}
	// THE LINE SAYS WHICH BODY STATE THE SWING CAME FROM (2026-11-03). Without it a
	// reader cannot tell a hit built on the OLD leg from the "stuck now" one built on the
	// new leg: same packet id, same target, 49 u apart.
	h.Log.Printf("zone: ATTACK REPEAT: commander %d char %d (%d) swing built from the "+
		"PRE-MOVE body at (%d, %d) on segment (%d, %d) -> (%d, %d) elapsed %dms, before the "+
		"move echo - the player's move order cancels the engagement after this hit, and no "+
		"further swing is built",
		sess.CommanderID, idx, c.Handle, pos[0], pos[1], body.from[0], body.from[1],
		body.dest[0], body.dest[1], body.elapsed.Milliseconds())
	h.endSwing(sess, idx, s.cancelReason+" - and the swing that was due at that instant "+
		"went out FIRST, built from the body as it was BEFORE the move")
	return out
}

// swingTick is the repeat's ONE TICK, and since R106 it is the engagement's whole state
// machine: measure, then move, swing, or end - in that order, once per pass.
//
// THE STOP CHECKS COME FIRST AND THEY DO NOT DEPEND ON THE CLOCK: a repeat whose target
// left this zone, whose target's HP has reached 0, whose stance changed or whose sheet
// lost its attack delay has no next swing at all, and waiting for `next` to say so would
// send one more hit at a body that is not there. The clock is asked only after them.
//
// THE POSITION IS MEASURED EVERY PASS, NOT ONLY WHEN A SWING IS DUE (R106). That is the
// whole repair: a body that is too near or too far is a body to MOVE, and finding it out
// at the moment a swing was owed is what used to end the repeat after one or two hits.
//
//	too near  (gap < MinR)  step BACK out to the min ring (attackRetreatPoint, arithmetic,
//	                        never a path-engine query) and keep the engagement
//	too far   (gap > MaxR)  walk IN through the same approachAttack a press uses, unless
//	                        the body has been more than attackDragSlack past MaxR for
//	                        longer than attackDragHold - that one is the player carrying
//	                        the character away, and it ends the repeat
//	in band                 swing, if the cadence has come
//
// ONE MORE BRANCH OUTRANKS ALL THREE AND IT SENDS NOTHING (R108, corrected 2026-11-03):
// a record a PLAYER MOVE ORDER cancelled. The swing that had come due at the order was
// already built and sent THERE, from the body as it was before the move
// (Handler.drainMoveOrderSwing, called from zone.go's MOVE_PC path). Every record that
// still carries the mark here is one the order did NOT swing - the cadence had not come,
// or the body was already outside the band - and that swing is never built afterwards: it
// would carry a 3564 +8 on the leg the report just opened, a point the client has not
// walked, and the handler hard-sets the actor there and calls SetMoveState(CMS_STOP)
// (durable fact #219, the owner's "stuck now"). So this branch only ends the record: no
// hit, no damage, no HP packet, no step back, no walk in, nothing scheduled.
//
// WHY THE ORDER SENT NOTHING IS SAID IN THE ENDING: the reason text is extended below with
// the fact that no swing was built, so a reader of the log is never left asking "where did
// the hit go". The two move branches further down are for a body this side moved or a body
// dragged without a report; an order means the player has taken the character, so moving it
// back would be fighting the player's own hands.
//
// NOTHING HERE ENDS THE REPEAT FOR BUILDING NOTHING. A swing that comes back empty - a
// race with the monster's own step, an unknown HP ledger - leaves the engagement alive
// and the next tick re-measures. The old "the swing it was due produced nothing" ending
// is gone: it was the second half of the owner's "attack stops", because the same body
// was refused twice and the second refusal killed the rhythm.
func (h *Handler) swingTick(sess *Session) [][]byte {
	if len(sess.swings) == 0 {
		return nil
	}
	now := time.Now()
	var out [][]byte
	for idx := 0; idx < len(sess.swings); idx++ {
		s := sess.swings[idx]
		if s == nil {
			continue
		}
		if idx >= len(sess.Team) {
			sess.swings[idx] = nil
			continue
		}
		c := &sess.Team[idx]

		// THE STANCE MAY MOVE UNDER A LIVE ENGAGEMENT, AND ONLY A NON-ATTACKER STANCE
		// ENDS IT (R94, corrected by the reference's own qualifier). `s.stance` is the
		// stance this record was armed in; a character that has changed to ANOTHER
		// ATTACKER STANCE is still in the same fight - the new cadence, reach and attack
		// type are read from the CURRENT stance lower down on every pass
		// (attackInterval/StatusOf, stanceRange, AtkType), so the only thing this record
		// has to do is learn the new id and carry on. Ending it here as well is what made
		// every stance change kill the rhythm twice: once in changeStance, once here.
		//
		// The one case that still ends: a stance whose AI is not the attacker's, which is
		// `ATTACKER != GetAI()` in PlayerChar_Request.cpp:1367 - and a stance this table
		// does not know, because an unnameable AI is not the attacker's either.
		if c.Stance != s.stance {
			if h.Stances.IsAttackerAI(int(c.Stance)) {
				s.stance = c.Stance
			} else {
				h.endSwing(sess, idx, "the character stands in stance "+strconv.Itoa(int(c.Stance))+
					" now, not "+strconv.Itoa(int(s.stance))+", and that stance's AI is not "+
					"the attacker AI (AI "+strconv.Itoa(h.Stances.AI(int(c.Stance)))+
					", the attacker profile is "+strconv.Itoa(AttackerAI)+"), so the attack "+
					"state ends (PlayerChar_Request.cpp:1364-1368)")
				continue
			}
		}
		mon, ok := h.monsterByHandle(s.target)
		if !ok {
			h.endSwing(sess, idx, "target "+strconv.Itoa(int(s.target))+" is not a monster this "+
				"zone is running any more (AS_ATTACK ends on a null target, "+
				"FieldChar_FSM.cpp:141-146)")
			continue
		}
		if hp, known := h.monsterLiveHP(s.target); known && hp <= 0 {
			h.endSwing(sess, idx, "target "+strconv.Itoa(int(s.target))+" has no HP left in this "+
				"zone's own ledger (a dead body is not attacked, FieldChar_FSM.cpp:136-140)")
			continue
		}
		interval := attackInterval(h.sheetFor(*c))
		if interval <= 0 {
			h.endSwing(sess, idx, "its sheet gives no attack delay any more (ASPD 0), so "+
				"there is no cadence to swing at")
			continue
		}

		// A CANCELLED ENGAGEMENT ENDS HERE, AND BUILDS NOTHING (R108, corrected 2026-11-03).
		// A player move order marked this record (noteMoveOrder); the swing that had come
		// due at that instant was built and sent THERE, before the move echo
		// (drainMoveOrderSwing). Reaching this branch means that call built nothing - the
		// cadence had not come, or the body was already out of the band - and the rule is
		// that such a swing is never built afterwards: a 3564 on the report's NEW leg is a
		// point the client has not walked, and its +8 stops the player's walk (fact #219).
		//
		// It is asked BEFORE the walk guard below on purpose: the order opens the player's
		// own walk on this session, and a record that waited for "no walk in flight" could
		// wait for ever.
		//
		// NO WALK IS EVER SENT FROM HERE, and that is the whole point of the order: the
		// branches below walk the body back toward the target, which is the "it kept
		// walking itself back into the band" half of the owner's report.
		//
		// IT ENDS ON THE SAME PASS, not after a second: the hysteresis below exists for a
		// body DRAGGED with no move report, and an order is not a drag, it is a decision.
		if s.cancelReason != "" {
			h.endSwing(sess, idx, s.cancelReason+" - and no swing was built for it at "+
				"that instant (the cadence had not come, or the body was no longer inside "+
				"the stance's band), so none is built now either")
			continue
		}

		// A WALK THIS ENGAGEMENT ALREADY ASKED FOR OWNS THE NEXT HIT (R106). While the
		// body is travelling to its stand point the tick measures nothing and sends
		// nothing: that walk carries the client's own 3666 and fires the hit when it
		// arrives (fire-on-arrival), which re-arms this same slot through armSwing. To
		// swing in the middle of it would put a second hit inside one cadence and turn
		// the walk's own owed hit into one the gate has to hold.
		if w := sess.approachOf(idx); w != nil && !w.castDone && w.atk != nil &&
			w.atk.target == s.target {
			continue
		}

		// THE MEASUREMENT, ONCE PER PASS, IN THE SAME TERMS THE PRESS USES (R106): the
		// body the CLIENT is drawing (attackBodyPoint -> drawnPos, the speed the client
		// was told), the monster's own row, and the edge distance between the two bodies
		// (attackRangeAllowed). Nothing here is a second arithmetic.
		minR, maxR := h.stanceRange(c.Stance)
		myR := PCBodyRadius
		targetR := h.monsterBodyRadius(mon)
		pos := sess.attackBodyPoint(idx, now, h.packetSpeed(c)).pos
		targetPos := [3]int32{mon.X, mon.Y, mon.Cell}
		gap, inBand := attackRangeAllowed(pos, targetPos, minR, maxR, myR, targetR)


		if !inBand && maxR > 0 && gap > maxR {
			// TOO FAR: WALK IN, AND KEEP THE ENGAGEMENT. The walk is the press's own
			// approachAttack - the same ring, the same pace, the same absorb rule - asked
			// by the repeat instead of by a click, and the hit it owes is the client's own
			// frame replayed when the body arrives.
			if gap > maxR+attackDragSlack {
				// THE ONE POSITION ENDING (R106), WITH ITS HYSTERESIS: more than 100 units
				// past MaxR, and kept there for longer than a second. A shorter excursion is
				// a reposition, and the tick below walks the body back.
				if s.farSince.IsZero() {
					s.farSince = now
					h.Log.Printf("zone: ATTACK REPEAT: char %d (%d) is %g of clear ground from "+
						"target %d (%q), more than %g past the stance's MaxR %g - the engagement "+
						"stays and walks IN; it ends only if the body is kept that clear for "+
						"longer than %s", idx, c.Handle, gap, s.target, h.MonsterName(mon),
						attackDragSlack, maxR, attackDragHold)
				} else if kept := now.Sub(s.farSince); kept > attackDragHold {
					h.endSwing(sess, idx, "the body was carried clear of the stance's reach by "+
						"the player's own movement - "+strconv.FormatFloat(gap, 'f', 0, 64)+
						" of clear ground, more than "+strconv.FormatFloat(attackDragSlack, 'f',
						0, 64)+" past MaxR "+strconv.FormatFloat(maxR, 'f', 0, 64)+", kept for "+
						kept.Round(time.Millisecond).String()+" (more than "+
						attackDragHold.String()+"), so there is nothing left to swing at "+
						"from there")
					continue
				}
			} else {
				s.farSince = time.Time{}
			}
			out = append(out, h.approachAttack(sess, idx, pendingAttack{
				frame: s.frame, target: s.target, pos: targetPos, dist: gap,
				minR: minR, maxR: maxR, myR: myR, targetR: targetR,
				idx: idx, since: now, stance: c.Stance,
				// repeat marks this as the ENGAGEMENT's own re-measure, so approachAttack
				// gives it the reference's once-per-stand-point rule and its chase clock
				// (R111) instead of the press's absorb rules.
				repeat: true,
			}, "the repeat's own swing found the target out of the stance's reach, so the "+
				"repeat walks IN and keeps swinging (R106)")...)
			continue
		}

		if !inBand {
			// TOO NEAR: STEP BACK OUT TO THE MIN RING, AND KEEP THE ENGAGEMENT. The ring and
			// the point are the reference's GetAttackMinPos sum taken as ARITHMETIC
			// (attackRetreatPoint/attackRetreatRing, R104) - no path-engine query, because
			// the path engine treats the monster as an obstacle and refuses every point
			// inside it, which is exactly where a retreat aims.
			s.farSince = time.Time{}
			out = append(out, h.approachAttack(sess, idx, pendingAttack{
				frame: s.frame, target: s.target, pos: targetPos, dist: gap,
				minR: minR, maxR: maxR, myR: myR, targetR: targetR,
				idx: idx, since: now, stance: c.Stance,
				stand: attackStepBackStand(minR, myR, targetR), stepBack: true,
			}, "the repeat's own swing found the target INSIDE the stance's minimum reach, so "+
				"the repeat steps BACK to the min ring and keeps swinging (R106)")...)
			continue
		}

		// IN THE BAND: the engagement is where it should be, so the only question left is
		// the clock.
		s.farSince = time.Time{}

		if now.Before(s.next) {
			continue // not owed yet
		}
		if !s.retryAt.IsZero() && now.Before(s.retryAt) {
			continue // the last swing built nothing; give the reason a cadence to clear
		}

		before := sess.lastAttack(idx)
		out = append(out, h.fireSwing(sess, idx, s, now)...)
		if !sess.lastAttack(idx).After(before) {
			// NOTHING BUILT, AND THE ENGAGEMENT STAYS (R106). The ATTACK line above says
			// which gate it was; this only makes sure the next tick does not ask again at
			// poll speed. The old ending - "the swing it was due produced nothing" - is
			// deleted: a refused swing is a reason to try again, not a reason to stop.
			s.retryAt = now.Add(interval)
			continue
		}
		s.retryAt = time.Time{}
		h.Log.Printf("zone: ATTACK REPEAT: commander %d char %d (%d) swing %d on target "+
			"%d (%q), %s after the previous hit (cadence: the character's own ASPD %s)",
			sess.CommanderID, idx, c.Handle, s.hits, s.target, h.MonsterName(mon),
			now.Sub(before).Round(time.Millisecond), interval)
	}
	return out
}

// fireSwing replays the client's own 3666 with `attackSwing` set, which is how a repeat
// swing becomes the same hit a press makes: one handler, one parser, one damage
// function, one HP ledger, one fan-out - and no way for the rhythm to invent a packet
// the click could not have sent.
//
// A SWING THAT BUILDS NOTHING IS NOT AN ENDING (R106). The line below still names the
// gate, because a reader must never see "nothing sent" without the reason, but the
// engagement lives on: the tick re-measures on its next pass and, if the body is out of
// the band, moves it. Only endSwing's own reasons end a repeat: the target leaving or
// dying, a stance change to a stance whose AI is not the attacker's, a sheet that lost
// its cadence, the body kept clear of the
// reach for longer than attackDragHold, and a PLAYER MOVE ORDER (any move report from
// the driven character that is not a walk this zone sent).
// `from` IS THE CALLER'S OWN MEASUREMENT, or nil to read the character's own. The move
// order's swing passes the body point it measured BEFORE the move was applied
// (drainMoveOrderSwing): that is the only way the packet can carry the point the character
// was standing on when the report arrived, because by the time the reply is assembled the
// session already describes the leg the report opened. One measurement, one packet, one
// log line (R103).
func (h *Handler) fireSwing(sess *Session, idx int, s *swingLoop, now time.Time) [][]byte {
	return h.fireSwingFrom(sess, idx, s, now, nil)
}

// fireSwingFrom is fireSwing with a body point the caller already measured.
func (h *Handler) fireSwingFrom(sess *Session, idx int, s *swingLoop, now time.Time,
	from *bodyPoint) [][]byte {
	if s == nil {
		return nil
	}
	out, why := h.attackAt(sess, s.frame, attackSwing, from, now)
	if len(out) == 0 && why != "" {
		h.Log.Printf("zone: ATTACK REPEAT: the swing for char %d on target %d built "+
			"nothing this pass: %s - nothing sent, and the engagement stays alive: the next "+
			"tick re-measures the body and swings or moves it (R106)", idx, s.target, why)
		return nil
	}
	return out
}

// endSwing stops character idx's repeat and logs why, in one line naming the target,
// how many hits it produced and how long it ran. It answers whether there WAS one, so a
// caller that only wants to log a real ending does not have to ask twice.
func (h *Handler) endSwing(sess *Session, idx int, why string) bool {
	if idx < 0 || idx >= len(sess.swings) || sess.swings[idx] == nil {
		return false
	}
	s := sess.swings[idx]
	sess.swings[idx] = nil
	handle := uint32(0)
	if idx < len(sess.Team) {
		handle = sess.Team[idx].Handle
	}
	h.Log.Printf("zone: ATTACK REPEAT ENDED: commander %d char %d (%d) stops swinging at "+
		"target %d after %d hit(s) over %s - %s",
		sess.CommanderID, idx, handle, s.target, s.hits,
		time.Since(s.began).Round(time.Millisecond), why)
	return true
}

// dropSwings ends every repeat this session has, and answers how many it ended. It is
// the disconnect path (Handler.leaveZone), beside dropCasts and for the same reason: a
// record left behind belongs to a session that is gone, and the next tick would build
// packets for nobody.
func (h *Handler) dropSwings(sess *Session, why string) int {
	n := 0
	for idx := range sess.swings {
		if h.endSwing(sess, idx, why) {
			n++
		}
	}
	return n
}
