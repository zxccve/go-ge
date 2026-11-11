package zone

import (
	"fmt"
	"strconv"
	"time"

	"geserver/protocol"
)

// THE CAST'S CLOCK: the cast is ASYNCHRONOUS, and this is the half that waits.
//
// WHY IT EXISTS. A cast used to be two packets in one reply: ZC_SKILL_CASTING
// (3571) and its result (3573 aimed, 3581 ground) left together, in the same
// millisecond. The reference does not do that. Measured off the owner's own
// recording of the LIVE server (`captures\cancel20261031\20261031-191902`, 77 s,
// both directions, read in history\cancel-and-animation.md):
//
//	t 9749.3   s2c 3571 ZC_SKILL_CASTING   begin = end = the stand point
//	t 9879.6   c2s 3598 CZ_MOVE_PC         130 ms AFTER the 3571, and it is ECHOED
//	t 10690.5  s2c 3581 ZC_SKILL_AREA      the result, 941 ms after the 3571
//
// and, over the whole recording, 0 x 3549 ZC_STOP_PC, 0 x 3568, no cancel in either
// direction. So between the 3571 and its result there is a WINDOW, and the client
// spends it moving. The reference accepts those moves (echoes each one at ordinary
// latency, 295 and 238 ms in the capture, inside its own 220-603 ms click band) and
// STILL lands the cast, from wherever the body is when the clock runs out.
//
// WHAT THIS FILE IS, and what it is not:
//
//   - one pending cast PER CHARACTER (Session.casts, parallel to Team): the 3571 is
//     sent at once, the result waits here, and a second press REPLACES it (R87's
//     rule - one pending cast, never a queue);
//   - the result is built by RE-RUNNING the same handler with `castLand`, at the
//     moment it fires, so it is built from the world as it is THEN: the caster's
//     LIVE position, the monsters' current positions, the skill's current row. One
//     code path for both halves, so the two can never disagree about offsets;
//   - NOTHING here cancels a cast on a move. That is the whole finding of the
//     capture: a move is ECHOED (zone\zone.go, movePC - the ordinary echo every
//     move gets) and the cast lands anyway. What DOES drop a pending cast, without
//     sending a result: the client's own CZ_SKILL_CANCEL (3650), a second cast
//     press, a disconnect, and a map change. A refused cast sends nothing at all.

const (
	// CmdCZSkillCancel is CZ_SKILL_CANCEL, client -> server. The client's own table
	// registers it at 16 bytes (`data\cmd_table_live_z.txt:1317` "3650 16
	// CZ_SKILL_CANCEL"); the client never sent one in the 77 s recording above, so
	// nothing about its body is measured - only the id and the size are.
	CmdCZSkillCancel = 3650

	// SkillCancelSize is the whole packet in the client's table: the 10-byte
	// security header plus 6 bytes.
	SkillCancelSize = 16
)

// castStep says WHICH HALF of a cast one call is producing. Both casts
// (`zone\skill_cast.go` aimed, `zone\skill_area.go` ground) take it, and the two
// halves are the same validation and the same tables - only the packets differ.
type castStep int

const (
	// castPress is the client's own request: validate, send the 3571, start the
	// clock, and send NO result.
	castPress castStep = iota

	// castAfterWalk is the same request replayed at the end of R87's approach walk:
	// identical to castPress, except that an out-of-range target is a reason to say
	// so and stop rather than to start a second walk.
	castAfterWalk

	// castLand is the clock running out: send the RESULT only, built from the world
	// as it is now. It never sends a 3571 and never starts a clock.
	castLand
)

// resumed is true for every call that is not the client's own press, which is what
// the range gate reads: "the walk is over and the target is STILL out of range" is
// only a thing a replayed request can find.
func (s castStep) resumed() bool { return s != castPress }

// castClock is one cast: the client's own request, its wind-up, and the hold that
// follows the result.
type castClock struct {
	frame  protocol.Frame // replayed verbatim when the clock fires
	ground bool           // 3647 (a point) rather than 3646/3645 (a monster)

	caster int    // the team slot that pressed
	skill  uint32 // for log lines only; the handler re-reads them from the frame
	target uint32

	began  time.Time     // when the press was accepted
	due    time.Time     // when the result is owed
	length time.Duration // the wind-up that was used (CastTime)
	source string        // where that length came from, for the log line

	// THE HOLD, which is the SECOND column and a different clock (history\
	// cancel-and-animation.md §6). It starts when the RESULT does - `due`, which for
	// an instant skill is the press itself - and it is what refuses the body's next
	// action. `landed` is set the moment the result has gone, so an instant cast
	// (length 0) is never fired twice and is still held.
	holdUntil time.Time
	hold      time.Duration
	landed    bool

	// moveFrame is a CZ_MOVE_PC this character sent while its own hold was open. The
	// reference QUEUES it (`PlayerChar_Request.cpp:343-348`, `IsHoldAction()`) and
	// only the newest one survives, so this is the last report, not a list. It goes
	// out as its own 3548 when the hold ends.
	moveFrame *protocol.Frame
	movePos   [3]int32

	// phase is the ANIMATING half of the cancel window (R89, zone\skill_approach.go):
	// a record only ever exists once the 3571 (or an instant result) has gone out, so
	// it is born animating and a move click during it is queued, never a cancel.
	phase castPhase
}

// castTimingFor is how long one skill's wind-up lasts and how long its body is held
// afterwards, and where those numbers came from. BOTH come from the skill's OWN row
// and never from a table in this file - the reference reads the same two columns
// through `GetCastTime()` / `GetHoldTime()` (`SkillTable.cpp:341-342`).
//
// WHICH ONE IS WHICH, and why (history\cancel-and-animation.md §6):
//
//   - `CastTime` decides WHEN THE RESULT IS SENT. The reference starts
//     `m_skillCastingEndTime = now + castingTime` and sends the 3571 on entering the
//     cast state (FieldChar_FSM.cpp:381-426), and moves to the state that sends the
//     result when it runs out (:429-435).
//   - `HoldTime` decides HOW LONG THE BODY IS HELD afterwards, counted from the
//     result: `m_useSkillEndTime = timeGetTime() + m_skill->GetHoldTime()`
//     (FieldChar_FSM.cpp:523).
//   - `CastTime == 0` is not "casts instantly, so lock nothing": the reference skips
//     the cast state entirely (:363-367) and the hold is then the ONLY window. That
//     is the whole reason the owner said "some skills have instant cast, so u also
//     need to see the hold time".
func castTimingFor(row SkillRow) (windup, hold time.Duration, source string) {
	windup = time.Duration(row.CastTime) * time.Millisecond
	hold = time.Duration(row.HoldTime) * time.Millisecond
	if windup < 0 {
		windup = 0
	}
	if hold < 0 {
		hold = 0
	}
	source = fmt.Sprintf("CastTime %d ms into HoldTime %d ms, both from its own row",
		row.CastTime, row.HoldTime)
	return windup, hold, source
}

// ---- the pending cast, per character ---------------------------------------

// castOf is character idx's pending cast, or nil.
func (s *Session) castOf(idx int) *castClock {
	if idx < 0 || idx >= len(s.casts) {
		return nil
	}
	return s.casts[idx]
}

// hasCast reports whether ANY character of this team has a cast waiting on its
// clock. It is what makes Session.wantsTick wake the read loop up (move.go): with
// no tick, a cast's result would wait for the client's next packet.
func (s *Session) hasCast() bool {
	for _, c := range s.casts {
		if c != nil {
			return true
		}
	}
	return false
}

// startCast stores this character's cast and answers it, so the caller can see both
// the wind-up it must announce and the hold that follows it. A cast already pending
// for that character is REPLACED, which is R87's rule read the other way round: one
// pending cast, never a queue, because a second press means the player changed their
// mind.
//
// It answers nil for a press this team cannot have, and the caller then sends
// nothing - the same silence every refusal in this lane gives.
func (h *Handler) startCast(sess *Session, idx int, f protocol.Frame, ground bool,
	skill, target uint32, row SkillRow) *castClock {

	if idx < 0 {
		return nil
	}
	if len(sess.casts) <= idx {
		grown := make([]*castClock, len(sess.Team))
		copy(grown, sess.casts)
		sess.casts = grown
		if idx >= len(sess.casts) {
			return nil // a press for a character this team does not have
		}
	}
	if old := sess.casts[idx]; old != nil {
		if old.landed {
			h.Log.Printf("zone: SKILL CAST: char %d pressed skill %d while the %s hold of "+
				"its previous skill %d was still %s from ending - the hold is REPLACED by "+
				"the new cast, and any move it was carrying is dropped",
				idx, skill, old.hold.Round(time.Millisecond), old.skill,
				time.Until(old.holdUntil).Round(time.Millisecond))
		} else {
			h.Log.Printf("zone: SKILL CAST: char %d pressed skill %d while its cast of %d "+
				"was still %s from landing - the pending cast is REPLACED (one cast at a "+
				"time, R87's rule), so only the new one will land",
				idx, skill, old.skill, time.Until(old.due).Round(time.Millisecond))
		}
	}
	windup, hold, source := castTimingFor(row)
	now := time.Now()
	// A CAST REPLACES THE PLAIN ATTACK (R94): in the reference `SetActionState(AS_SKILL_*)`
	// leaves AS_ATTACK, and `ProcessAttack` selects the plain-attack path only while the
	// character IS in it (PlayerChar.cpp:1987-2002). Without this line a character could
	// cast a skill and keep swinging underneath it - two damage streams from one body.
	//
	// AND IT REPLACES A KEPT PRESS (zone\attack.go, holdPress) for the same reason: the
	// press is a plain attack waiting for its delay, and a character that is casting is
	// not in the plain-attack state - the press it kept would fire a second damage stream
	// under the cast.
	h.clearPress(sess, idx, "skill "+strconv.Itoa(int(skill))+" was cast, and a press "+
		"waiting for the plain attack's delay does not survive a cast "+
		"(PlayerChar.cpp:1987-2002)")
	if h.endSwing(sess, idx, "skill "+strconv.Itoa(int(skill))+" was cast, and a cast is not the "+
		"plain attack (PlayerChar.cpp:1987-2002)") {
		h.Log.Printf("zone: SKILL CAST: the plain attack's repeat for char %d is ended by "+
			"this cast", idx)
	}
	c := &castClock{
		frame: f, ground: ground, caster: idx, skill: skill, target: target,
		began: now, due: now.Add(windup), length: windup, source: source,
		hold: hold, holdUntil: now.Add(windup + hold),
		phase: castPhaseAnimating,
	}
	sess.casts[idx] = c
	return c
}

// landCast sends one cast's RESULT and marks the record so it can never fire twice.
// It is the only place a result is built, so the press of an instant skill and the
// tick of a clocked one cannot disagree.
func (h *Handler) landCast(sess *Session, c *castClock) [][]byte {
	if c == nil || c.landed {
		return nil
	}
	c.landed = true
	return h.fireCast(sess, c)
}

// castHolding reports whether this character is inside its own cast or hold right
// now, which is exactly `IsHoldAction()` in the reference (FieldChar.cpp:1023-1049:
// true in AS_SKILL_PREPROCESS, AS_SKILL_CASTING and AS_SKILL_USE).
func (h *Handler) castHolding(sess *Session, idx int, now time.Time) *castClock {
	c := sess.castOf(idx)
	if c == nil {
		return nil
	}
	if now.Before(c.holdUntil) {
		return c
	}
	return nil
}

// queueCastMove keeps the client's own report for a character whose own cast is
// holding, and answers nothing. The reference does the same and for the same reason:
// `DispatchMove()` returns early on `IsHoldAction()` WITHOUT clearing the request
// (PlayerChar_Request.cpp:343-348), so every press overwrites the last and only the
// newest one dispatches when the hold ends. It is NOT a refusal and NOT a cancel -
// the capture shows exactly this: the caster's two clicks during its cast were never
// echoed, and the one that came out afterwards was the LAST of them
// (history\cancel-and-animation.md §6.4).
func (h *Handler) queueCastMove(c *castClock, f protocol.Frame, pos [3]int32) {
	if c.moveFrame != nil {
		h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d, cell %d) arrived while its own "+
			"skill %d was holding - it REPLACES the move already queued for (%d, %d, "+
			"cell %d), which will never be echoed: one pending move, the newest, exactly "+
			"as the reference's single m_lastRequest - move queued until the hold ends",
			c.caster, pos[0], pos[1], pos[2], c.skill,
			c.movePos[0], c.movePos[1], c.movePos[2])
	}
	cp := f
	c.moveFrame = &cp
	c.movePos = pos
}

// releaseHeldMove is the other half of queueCastMove: the hold has ended, so the one
// queued report goes out as an ordinary 3548 through the ordinary path, and the body
// walks to it.
func (h *Handler) releaseHeldMove(sess *Session, c *castClock) [][]byte {
	if c == nil || c.moveFrame == nil {
		return nil
	}
	f := *c.moveFrame
	c.moveFrame = nil
	h.Log.Printf("zone: MOVE_PC char %d's queued move to (%d, %d, cell %d) is RELEASED - "+
		"the %s hold of its skill %d has ended, so it goes out as one 3548 now",
		c.caster, c.movePos[0], c.movePos[1], c.movePos[2],
		c.hold.Round(time.Millisecond), c.skill)
	out, err := h.movePC(sess, f)
	if err != nil {
		h.Log.Printf("zone: MOVE_PC the released move of char %d failed: %v - nothing sent",
			c.caster, err)
		return nil
	}
	return out
}

// dropPressWhileAnimating is the ONE rule for a SECOND SKILL PRESS during the
// ANIMATING phase (R89): it is DROPPED, one line is logged, and the cast that is
// already animating is left alone.
//
// WHY DROPPED AND NOT QUEUED FOR AFTER THE HOLD - read out of the reference, not
// chosen here. A press during AS_SKILL_PREPROCESS / AS_SKILL_CASTING / AS_SKILL_USE
// stores itself in the single `m_lastRequest` slot, and
// `CFieldChar::IsEnableSkillAction` refuses it while `IsHoldAction()` is true
// (FieldChar.cpp:1100-1104) - so it IS held, and `CFieldChar::ProcessRequest`
// retries it every tick (FieldChar.cpp:833-872). But the retry that lands after the
// hold meets the barrier at the top of the same dispatcher:
//
//	if (m_skillRefreshTime > timeGetTime()) {
//		m_lastRequest->type = REQ_NONE; SkillDisabled(); return; }
//
// (PlayerChar_Request.cpp:726-731 aimed, :831-836 ground), and `m_skillRefreshTime`
// is set to `now + RefreshTime` the moment the hold ends (FieldChar_FSM.cpp:527-531).
// The rows this lane casts state RefreshTime 140 (Rapid Shot 10561), 163 (Implode
// Shock 10562) and 223 (Fireball 20211), so the held press is CLEARED rather than
// cast; the row's own CoolDown (5000 ms on Rapid Shot) is the second barrier behind
// it. The scripts set no clock of their own in either case (attack.lua copies the
// table's numbers into the engine's argument struct and returns - §6.3).
//
// So the reference's answer to "pressed again while animating" is: the press does not
// become a cast, and the body finishes the animation it started. That is what this
// function enforces, and it says so in ONE line. It does NOT send the reference's
// `3572 ZC_SKILL_DISABLE`, because this lane may not add a packet (the build lane's
// rule: no 3568/3572 as part of a cast; history\cancel-and-animation.md §"the exact
// packet list").
//
// It answers true when the caller must send NOTHING.
func (h *Handler) dropPressWhileAnimating(sess *Session, idx int, skill uint32,
	word string) bool {

	c := h.castHolding(sess, idx, time.Now())
	if c == nil {
		return false
	}
	when := "wind-up"
	if c.landed {
		when = "hold"
	}
	h.Log.Printf("zone: %s: char %d pressed skill %d (%q) while its previous skill %d "+
		"was still in its %s (%s left) - the press is DROPPED and the animating cast is "+
		"left alone: the reference holds the request through the cast and then clears it "+
		"on the row's own RefreshTime barrier (PlayerChar_Request.cpp:726-731, "+
		"FieldChar_FSM.cpp:527-531), so it never becomes a cast - nothing sent",
		word, idx, skill, h.SkillName(skill), c.skill, when,
		time.Until(c.holdUntil).Round(time.Millisecond))
	return true
}

// dropCast drops character idx's pending cast WITHOUT sending its result, and says
// why. It answers whether there was one, so a caller that logs can stay quiet when
// there is nothing to drop.
func (h *Handler) dropCast(sess *Session, idx int, why string) bool {
	if idx < 0 || idx >= len(sess.casts) || sess.casts[idx] == nil {
		return false
	}
	c := sess.casts[idx]
	sess.casts[idx] = nil
	h.Log.Printf("zone: SKILL CAST: char %d's pending cast of skill %d (%q) is dropped "+
		"without a result %s of the way through its %s clock - %s. Nothing is sent: a "+
		"cast that does not land sends nothing at all "+
		"(history\\cancel-and-animation.md)",
		idx, c.skill, h.SkillName(c.skill),
		time.Since(c.began).Round(time.Millisecond), c.length, why)
	return true
}

// dropCasts drops EVERY pending cast of this session, for the events that end the
// whole connection or move the team to another map.
func (h *Handler) dropCasts(sess *Session, why string) int {
	n := 0
	for idx := range sess.casts {
		if h.dropCast(sess, idx, why) {
			n++
		}
	}
	return n
}

// castTick fires every cast whose wind-up has run out, and ends every hold that has.
// It is called from chaseTick (zone\chase.go), which Serve wakes up for on the poll
// interval while Session.wantsTick is true.
//
// TWO CLOCKS, ONE PASS. A record that has not landed yet is owed its RESULT at
// `due` (the row's CastTime); once it has landed it owes the END of its HOLD at
// `holdUntil` (the row's HoldTime), and the end of the hold is where the character's
// queued move is released. An instant skill lands at the press, so by its first tick
// it is already in the second phase and the tick only ever ends the hold.
//
// ONE TICK, AT MOST ONE RESULT PER CHARACTER: `landCast` marks the record before it
// builds anything, so a second tick in the same millisecond cannot fire it twice.
func (h *Handler) castTick(sess *Session) [][]byte {
	now := time.Now()
	var out [][]byte
	for idx := range sess.casts {
		c := sess.casts[idx]
		if c == nil {
			continue
		}

		if !c.landed && !now.Before(c.due) {
			// WHERE THE BODY IS NOW, not where it was when the press arrived. This is
			// the point of the clock: the result fires from the body's live position.
			live := sess.charPos(idx, now)
			h.Log.Printf("zone: SKILL CAST: char %d's %s is up - sending the result "+
				"from the body's live position (%d, %d, cell %d), then holding it for %s",
				idx, c.source, live[0], live[1], live[2],
				c.hold.Round(time.Millisecond))
			out = append(out, h.landCast(sess, c)...)
		}

		if c.landed && !now.Before(c.holdUntil) {
			out = append(out, h.releaseHeldMove(sess, c)...)
			sess.casts[idx] = nil
		}
	}
	return out
}

// fireCast builds the result of one pending cast by REPLAYING the client's own
// request with `castLand`: the same handler, the same tables, the same offsets, and
// no second wind-up. A replay that refuses (a stance changed under the cast, the
// target left this zone) says so in one line and sends nothing, which is what a cast
// that does not land looks like on the wire.
func (h *Handler) fireCast(sess *Session, c *castClock) [][]byte {
	var out [][]byte
	var err error
	if c.ground {
		out, err = h.castSkillGroundAt(sess, c.frame, castLand)
	} else {
		out, err = h.castSkillAt(sess, c.frame, castLand)
	}
	if err != nil {
		h.Log.Printf("zone: SKILL CAST: the result of char %d's skill %d failed: %v - "+
			"nothing sent", c.caster, c.skill, err)
		return nil
	}
	return out
}

// ---- the client's own cancel ------------------------------------------------

// ParseSkillCancel reads CZ_SKILL_CANCEL (3650): the 10-byte security header, then
// the character index at +10 - the byte every CZ_SKILL_* sender of this client puts
// there (3645, 3646 and 3647 all do; zone\skill_cast.go).
//
// NOT MEASURED BEYOND THE ID AND THE SIZE. The client sent no 3650 at all in the 77 s
// recording that this lane is built on, so the BODY is read defensively: a frame too
// short to carry the byte is still accepted (it drops every pending cast, since the
// player asked for a cancel and nothing else can be wrong), and an index outside the
// team is treated the same way, with a line saying so. Wrong guesses here cost at
// most one cast the player asked to cancel; the frame is logged either way.
func ParseSkillCancel(pt []byte) (charIndex int, ok bool) {
	if len(pt) < czCharIndex+1 {
		return -1, false
	}
	return int(pt[czCharIndex]), true
}

// skillCancel answers CZ_SKILL_CANCEL: the player cancelled the cast on screen, so
// the pending one is dropped and NOTHING is sent.
//
// It is the one cancel the reference's protocol actually has. A MOVE is not a
// cancel - that is the finding this lane exists for - and no 3549 or 3568 goes out
// for either.
func (h *Handler) skillCancel(sess *Session, f protocol.Frame) ([][]byte, error) {
	if len(f.Data) != SkillCancelSize {
		h.Log.Printf("zone: SKILL_CANCEL: %d-byte frame, the client's table registers 3650 "+
			"at %d bytes - reading the character index at +%d anyway, and dropping the "+
			"pending cast without a result", len(f.Data), SkillCancelSize, czCharIndex)
	}
	idx, ok := ParseSkillCancel(f.Data)
	if !ok || idx < 0 || idx >= len(sess.Team) {
		h.Log.Printf("zone: SKILL_CANCEL: %d-byte frame names character %d, which is not "+
			"this team (%d member(s)) - dropping every pending cast",
			len(f.Data), idx, len(sess.Team))
		if n := h.dropCasts(sess, "the client asked to cancel (3650)"); n == 0 {
			h.Log.Printf("zone: SKILL_CANCEL: there was no pending cast - nothing to do")
		}
		return nil, nil
	}
	if !h.dropCast(sess, idx, "the client asked to cancel it (3650 CZ_SKILL_CANCEL)") {
		h.Log.Printf("zone: SKILL_CANCEL: char %d has no pending cast (it may already have "+
			"landed) - nothing sent", idx)
	}
	return nil, nil
}
