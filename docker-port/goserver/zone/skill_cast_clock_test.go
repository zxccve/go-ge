package zone

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE CAST'S TWO CLOCKS (R88): what the reference really does between a 3571 and the
// moment the character may act again.
//
// THE ORACLE IS TWO SOURCES THAT AGREE, and they are written up with their
// file:line citations in `docker-port\history\cancel-and-animation.md` §6:
//
//  1. THE RETAIL SERVER'S OWN STATE MACHINE, `source2\geServer\ZServerLib\`:
//     `FieldChar_FSM.cpp:363-367` - `CastTime` 0 skips the cast state entirely;
//     `:381-426` - the cast state starts `now + CastTime` and sends the 3571 with
//     `time = castingTime`;
//     `:429-435` - the state that sends the RESULT is entered when that runs out;
//     `:523` - and then `m_useSkillEndTime = now + HoldTime` holds the body.
//     `PlayerChar_Request.cpp:343-348` - while any of those states is up,
//     `DispatchMove()` returns early WITHOUT clearing the request, so a move is
//     QUEUED, newest wins, never echoed and never a cancel.
//
//  2. THE OWNER'S OWN RECORDING OF THE LIVE SERVER,
//     `captures\cancel20261031\20261031-191902`, 77 s both directions. The caster is
//     team slot 2, handle 1633040. Between its 3571 (s2c +140445) and its 3581
//     (+140860) the server echoed slots 0 and 1 - the two TEAMMATES - and never the
//     caster. The caster's next 3548 (+141407) carried (-2287, 143), which is the
//     LAST of the three clicks it had sent during the cast. That is `m_lastRequest`
//     read out of the bytes. Nothing is cancelled: 0 x 3549, 0 x 3568.
//
// SO THE TWO COLUMNS ARE IN SEQUENCE, NOT ONE: `CastTime` decides WHEN THE RESULT IS
// SENT, `HoldTime` decides how long the body is then held. From the press to "may act
// again" the body is locked for CastTime + HoldTime.
//
//	THIS SERVER, after this lane (CastTime > 0)
//	  press             3571 (+ 3654 for a ground cast)      nothing else
//	  a 3598 meanwhile  NOTHING - it is queued                and the cast is untouched
//	  clock fires       3573 (aimed) or 3581 + 3557 (ground)  from the body's LIVE point
//	  ...then HoldTime   then ONE 3548 for the queued move, if there was one
//
//	THIS SERVER, CastTime == 0
//	  press             the result NOW, and NO 3571           (the reference's own fork)
//	  then HoldTime     the same hold, the same single 3548

// castDueNow drives character idx's pending cast past its due time, without waiting.
// It moves the CLOCK, never the rule: the code under test still decides that a due
// cast fires, and when.
func castDueNow(t *testing.T, sess *Session, idx int) {
	t.Helper()
	c := sess.castOf(idx)
	if c == nil {
		t.Fatalf("char %d has no pending cast to fire", idx)
	}
	c.due = time.Now().Add(-time.Millisecond)
}

// castHoldOver drives character idx's HOLD past its end, without waiting, so the
// release of a queued move can be observed in the same millisecond.
func castHoldOver(t *testing.T, sess *Session, idx int) {
	t.Helper()
	c := sess.castOf(idx)
	if c == nil {
		t.Fatalf("char %d has no cast record to hold", idx)
	}
	if !c.landed {
		t.Fatalf("char %d's cast has not landed yet, so its hold has not started", idx)
	}
	c.holdUntil = time.Now().Add(-time.Millisecond)
}

// walkIsUnderway moves the body idx is walking with to `d` into its segment, without
// waiting: both the leader's own clock (moveAt) and the open walk segment (walks[idx].at)
// are shifted, because the tick's endWalks and its walkedPos must agree about when the
// walk began or one of them closes it under the other.
func walkIsUnderway(t *testing.T, sess *Session, idx int, d time.Duration) {
	t.Helper()
	began := time.Now().Add(-d)
	if idx == 0 {
		sess.moveAt = began
	}
	if idx < len(sess.walks) && !sess.walks[idx].at.IsZero() {
		sess.walks[idx].at = began
	}
}

// pendingCastLen is how long character idx's pending cast is waiting for, and where
// that number came from.
func pendingCastLen(sess *Session, idx int) (time.Duration, string) {
	if c := sess.castOf(idx); c != nil {
		return c.length, c.source
	}
	return 0, ""
}

// TestACastSendsTheWindUpAndNoResult is check (a): one accepted press sends the 3571
// and NOTHING else - no 3573, no 3581 - and leaves exactly one pending cast behind.
//
// This is the shape the reference's own first cast had: a 3571 at t 9749.3 and no
// result for another 941 ms. A result in the same reply is what this lane removes.
func TestACastSendsTheWindUpAndNoResult(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("one press produced %v, want ONLY the 3571:\n%s",
			kinds(out), logs.String())
	}
	if r := recvs(out)[0]; r.cmd != CmdZCSkillCasting || len(r.b) != SkillCastingSize {
		t.Fatalf("the packet is cmd %d, %d bytes, want %d at %d bytes", r.cmd, len(r.b),
			CmdZCSkillCasting, SkillCastingSize)
	}
	// THE CLOCK IS RUNNING, and its length is the skill's OWN row.
	if got := p.sess.castOf(0); got == nil {
		t.Fatalf("no pending cast was left behind:\n%s", logs.String())
	}
	row, ok := h.Skills.Row(capturedSkill)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedSkill)
	}
	length, _ := pendingCastLen(p.sess, 0)
	if want := time.Duration(row.CastTime) * time.Millisecond; length != want {
		t.Errorf("the clock is %s, want the row's CastTime %s", length, want)
	}
	// AND NOTHING FIRES UNTIL IT RUNS OUT: a tick now sends nothing.
	if early := h.chaseTick(p.sess); len(early) != 0 {
		t.Fatalf("a tick before the clock sent %v, want nothing", kinds(early))
	}
	// The session is kept awake for it - without this the result would wait for the
	// client's next packet.
	if !p.sess.wantsTick() {
		t.Error("a session with a pending cast does not wake up for its clock")
	}
}

// TestTheResultFiresOnTheClockFromTheLivePosition is check (b): the result must name
// where the body IS when the clock runs out, not where it stood when the key was
// pressed.
//
// The fixture is section 4 of the capture: the player is ALREADY WALKING and presses
// the skill mid-walk. The reference never stops them for it - it keeps walking and the
// result still fires (history\cancel-and-animation.md §4). So the walk is opened FIRST,
// the press comes second, the body advances during the wind-up, and the 3573 must begin
// at a point strictly between the two.
func TestTheResultFiresOnTheClockFromTheLivePosition(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	pressAt := [3]int32{-910, 1392, 29}
	click := [3]int32{pressAt[0] + 300, pressAt[1], pressAt[2]}
	// WALKING FIRST: the ordinary move and its ordinary echo, before any cast exists.
	if _, err := h.Reply(context.Background(), p.sess, moveTo(0, click)); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the press sent %v, %v", kinds(out), err)
	}
	// A moment of that walk has been walked by the time the clock fires.
	walkIsUnderway(t, p.sess, 0, 300*time.Millisecond)

	castDueNow(t, p.sess, 0)
	out := h.chaseTick(p.sess)
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the clock sent %v, want the one %d:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	melee := out[0]
	begin := worldPos(melee[smBegin:])
	end := worldPos(melee[smEnd:])
	if begin == pressAt {
		t.Fatalf("the 3573 begins at %v - where the character stood when the key was "+
			"pressed. The client does SetGroundPos(srcPos) with it (GeActor.cpp:~301), "+
			"so this is the owner's teleport", begin)
	}
	if begin == click {
		t.Errorf("the 3573 begins at the click destination %v, which the body has not "+
			"reached yet", click)
	}
	if begin[0] <= pressAt[0] || begin[0] >= click[0] {
		t.Fatalf("the 3573 begins at %v, want a point strictly between %v and %v",
			begin, pressAt, click)
	}
	if begin[2] != pressAt[2] {
		t.Errorf("the 3573 begins on cell %d, want %d", begin[2], pressAt[2])
	}
	if end != begin {
		t.Errorf("the 3573 ends at %v but begins at %v", end, begin)
	}
}

// TestAMoveDuringTheHoldIsQueuedAndReleasedOnce is check (c), the lane's whole finding
// read out of the bytes: a 3598 during the cast does NOT reach the wire. The reference
// queues it (`PlayerChar_Request.cpp:343-348`) and the capture shows the caster's own
// echo only after the result, carrying the LAST of the clicks it sent. Nothing is
// cancelled and no stop goes out, and the cast still lands.
func TestAMoveDuringTheHoldIsQueuedAndReleasedOnce(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the press sent %v, %v", kinds(out), err)
	}

	// THE CLICK, mid-wind-up. NOTHING comes back: not an echo, not a stop, not a
	// cancel. This is the one thing §3 of history\cancel-and-animation.md got wrong.
	first := [3]int32{-610, 1392, 29}
	out, err := h.Reply(context.Background(), p.sess, moveTo(0, first))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a move during the cast sent %v, want NOTHING - the reference queues "+
			"it and echoes it only after the hold:\n%s", kinds(out), logs.String())
	}
	if !strings.Contains(logs.String(), "QUEUED") {
		t.Errorf("the queueing was not logged:\n%s", logs.String())
	}
	// AND THE BODY DID NOT MOVE: no walk segment was opened for it.
	if pos := p.sess.charPos(0, time.Now()); pos != ([3]int32{-910, 1392, 29}) {
		t.Errorf("the queued move moved the body to %v, want it still standing at "+
			"(-910, 1392, 29)", pos)
	}

	// A SECOND click replaces the queued one - one pending move, the newest, exactly
	// as the reference's single m_lastRequest behaves.
	second := [3]int32{-710, 1392, 29}
	if out, err := h.Reply(context.Background(), p.sess, moveTo(0, second)); err != nil ||
		len(out) != 0 {
		t.Fatalf("the second click sent %v, %v - want nothing", kinds(out), err)
	}

	// THE CAST IS UNTOUCHED: still pending, same skill.
	if c := p.sess.castOf(0); c == nil || c.skill != capturedSkill {
		t.Fatalf("the move cancelled the pending cast: %+v", c)
	}

	// AND IT LANDS - one result, from the stand point, no walk invented.
	castDueNow(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the cast did not land after the move: %v\n%s", kinds(out), logs.String())
	}
	if begin := worldPos(out[0][smBegin:]); begin != ([3]int32{-910, 1392, 29}) {
		t.Errorf("the 3573 begins at %v, want the stand point (-910, 1392, 29) - the "+
			"queued move is not the body's position", begin)
	}

	// THE HOLD IS STILL ON: another tick, still inside HoldTime, sends nothing.
	if mid := h.chaseTick(p.sess); len(mid) != 0 {
		t.Fatalf("a tick inside the hold sent %v, want nothing", kinds(mid))
	}

	// THE HOLD ENDS: exactly ONE 3548, and it carries the SECOND click - the newest.
	castHoldOver(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCMovePC {
		t.Fatalf("the end of the hold sent %v, want exactly one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	if got := worldPos(r[0].b[10:]); got != second {
		t.Errorf("the released echo names %v, want the newest click %v", got, second)
	}
	for _, pkt := range out {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd == CmdZCStopPC ||
			cmd == CmdZCAttackDisable || cmd == CmdZCSkillDisable {
			t.Fatalf("a %d went out as part of a cast: the reference sends none", cmd)
		}
	}
	// AND NOTHING IS LEFT: one release, and the record is gone.
	if p.sess.castOf(0) != nil {
		t.Error("the cast record survived the end of its hold")
	}
	if later := h.chaseTick(p.sess); len(later) != 0 {
		t.Fatalf("the tick after the release sent %v, want nothing", kinds(later))
	}
}

// TestASecondPressDuringTheAnimationIsDropped is check (d), and it REVERSES the rule it
// was written with. It used to assert that a second press mid-cast REPLACES the pending
// cast with a fresh 3571. The reference does not do that, and the evidence is in its own
// dispatchers:
//
//   - the press stores itself in the single `m_lastRequest` slot,
//     `CFieldChar::IsEnableSkillAction` refuses it while `IsHoldAction()` is true and
//     leaves the slot set (FieldChar.cpp:1100-1104), and `ProcessRequest` retries it
//     every tick (FieldChar.cpp:833-872): so far, "held";
//   - but the retry that lands after the hold meets the barrier at the top of the same
//     dispatcher - `if (m_skillRefreshTime > timeGetTime()) { m_lastRequest->type =
//     REQ_NONE; SkillDisabled(); return; }` (PlayerChar_Request.cpp:726-731 aimed,
//     :831-836 ground) - and `m_skillRefreshTime` is set to `now + RefreshTime` the
//     moment the hold ends (FieldChar_FSM.cpp:527-531). Rapid Shot's own RefreshTime is
//     140 ms, so the held press is CLEARED, not cast, and its CoolDown 5000 ms is the
//     second barrier behind it.
//
// So a second press during the animation does not become a cast: the first one keeps
// animating and lands. That is what this pins. The gate is NOT permanent - the last part
// of the test proves a press works again the moment the hold is over.
func TestASecondPressDuringTheAnimationIsDropped(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	// Two monsters inside Rapid Shot's range, so both presses are immediate casts and
	// the two targets cannot be confused.
	other := scarecrow()
	other.Handle = 0x900002
	other.Y = 2400
	h.spawnedMonster = []Monster{scarecrow(), other}

	first := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(first), Data: first,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the first press sent %v, %v", kinds(out), err)
	}
	if c := p.sess.castOf(0); c == nil || c.phase != castPhaseAnimating {
		t.Fatalf("the first press left no animating cast: %+v", c)
	}

	second := skillTargetFrame(0, capturedSkill, other.Handle)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(second), Data: second,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// NOTHING GOES OUT: no fresh 3571, no second result, no packet at all.
	if len(out) != 0 {
		t.Fatalf("the second press sent %v, want nothing - it is dropped while the "+
			"first cast animates:\n%s", kinds(out), logs.String())
	}
	if !strings.Contains(logs.String(), "the press is DROPPED") {
		t.Errorf("the dropped press was not logged:\n%s", logs.String())
	}
	// THE ANIMATING CAST IS UNTOUCHED, and it is still the FIRST press's skill.
	if c := p.sess.castOf(0); c == nil || c.skill != capturedSkill {
		t.Fatalf("the dropped press disturbed the animating cast: %+v", c)
	}

	// ONE RESULT, AND IT IS THE FIRST PRESS'S TARGET.
	castDueNow(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the clock sent %v, want exactly one %d:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	if got := binary.LittleEndian.Uint32(out[0][smTarget:]); got != capturedTarget {
		t.Errorf("the result names target %d, want the first press's %d", got,
			capturedTarget)
	}
	// AND NOTHING IS LEFT TO FIRE.
	if later := h.chaseTick(p.sess); len(later) != 0 {
		t.Fatalf("a second tick sent %v, want nothing", kinds(later))
	}

	// THE GATE IS THE WINDOW, NOT THE SKILL: once the hold is over, a press is accepted
	// and sends its 3571 like any other. Without this, "dropped" would read as "the
	// skill button stopped working".
	castHoldOver(t, p.sess, 0)
	_ = h.chaseTick(p.sess) // ends the hold and clears the record
	if c := p.sess.castOf(0); c != nil {
		t.Fatalf("the cast record survived the end of its hold: %+v", c)
	}
	again := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err = h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(again), Data: again,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillCasting {
		t.Fatalf("a press after the hold sent %v, want one %d - the drop must only "+
			"apply inside the animation:\n%s", kinds(out), CmdZCSkillCasting,
			logs.String())
	}
}

// TestAnInstantSkillStillWaitsItsHold is check (a), and it is the owner's own point:
// "some skills have instant cast, so u also need to see the hold time".
//
// For a row whose CastTime is 0 the reference does NOT wait - `FieldChar_FSM.cpp:363-367`
// skips the cast state, so there is no 3571 and the result goes at the press. What it
// does NOT skip is the hold: `:523` still sets `m_useSkillEndTime = now + HoldTime`.
// So the thing an instant skill "waits" is its HOLD, and that is the assertion here.
//
// Implode Shock 10562 is the real row: CastTime 0, HoldTime 1630.
func TestAnInstantSkillStillWaitsItsHold(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	// A monster INSIDE Implode Shock's own MaxR 125, so the cast fires where it stands
	// instead of starting R87's approach walk.
	near := scarecrow()
	near.Handle = 0x900003
	near.X, near.Y, near.Cell = -1000, 1392, 29
	h.spawnedMonster = []Monster{near}

	row, ok := h.Skills.Row(instantSkill)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", instantSkill)
	}
	if row.CastTime != 0 {
		t.Fatalf("skill %d states CastTime %d - this test needs a row that states 0",
			instantSkill, row.CastTime)
	}
	if row.HoldTime <= 0 {
		t.Fatalf("skill %d states HoldTime %d - the whole point of this test is a hold",
			instantSkill, row.HoldTime)
	}

	windup, hold, source := castTimingFor(row)
	if windup != 0 {
		t.Errorf("a CastTime 0 row waits %s for its wind-up, want 0", windup)
	}
	if want := time.Duration(row.HoldTime) * time.Millisecond; hold != want {
		t.Errorf("a CastTime 0 row holds for %s, want its own HoldTime %s", hold, want)
	}
	if !strings.Contains(source, "HoldTime") {
		t.Errorf("the source says %q, want it to name HoldTime", source)
	}

	// THE PRESS: NO 3571, and the RESULT NOW. That is the reference's fork.
	f := skillTargetFrame(0, instantSkill, near.Handle)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCSkillMelee {
		t.Fatalf("an instant cast sent %v, want ONLY the %d result (no wind-up):\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	for _, pkt := range out {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd == CmdZCSkillCasting {
			t.Fatalf("a 3571 went out for a CastTime 0 skill - the client's own cast "+
				"state would then divide by its `time` (GeActor.cpp:2206)")
		}
	}

	// AND THE HOLD IS REAL, AND IT IS THE ROW'S OWN NUMBER. This is the assertion that
	// fails if someone makes an instant skill lock nothing: with hold == 0 the record
	// would already be gone here.
	c := p.sess.castOf(0)
	if c == nil {
		t.Fatalf("an instant cast left no hold behind - the character could act again "+
			"at once:\n%s", logs.String())
	}
	if !c.landed {
		t.Fatal("the instant cast's result has not been marked as sent")
	}
	if want := time.Duration(row.HoldTime) * time.Millisecond; c.hold != want {
		t.Fatalf("the hold is %s, want the row's HoldTime %s", c.hold, want)
	}

	// THE BODY IS REFUSED FOR THE WHOLE HOLD: a click is queued, not echoed, and the
	// clock sends no second result.
	click := [3]int32{-960, 1392, 29}
	if out, err := h.Reply(context.Background(), p.sess, moveTo(0, click)); err != nil ||
		len(out) != 0 {
		t.Fatalf("a move during the hold sent %v, %v - want nothing", kinds(out), err)
	}
	if mid := h.chaseTick(p.sess); len(mid) != 0 {
		t.Fatalf("a tick inside the hold sent %v, want nothing", kinds(mid))
	}
	// AND IT IS RELEASED ONCE, at the row's own HoldTime and not before.
	castHoldOver(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	r = recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCMovePC {
		t.Fatalf("the end of the hold sent %v, want exactly one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	if got := worldPos(r[0].b[10:]); got != click {
		t.Errorf("the released echo names %v, want the client's own point %v", got, click)
	}
}

// TestAnInstantSkillLocksNothingIfItsRowSaysSo is the other half of (a), and it exists
// so the test above cannot be satisfied by a constant: a row that states HoldTime 0
// really does leave the character free at once, and our code must follow the ROW, not
// a number of its own.
func TestAnInstantSkillLocksNothingIfItsRowSaysSo(t *testing.T) {
	windup, hold, _ := castTimingFor(SkillRow{ID: 1, CastTime: 0, HoldTime: 0})
	if windup != 0 || hold != 0 {
		t.Fatalf("a row with CastTime 0 and HoldTime 0 gives %s/%s, want 0/0", windup, hold)
	}
	windup, hold, _ = castTimingFor(SkillRow{ID: 10111, CastTime: 0, HoldTime: 2670})
	if windup != 0 {
		t.Errorf("Deep Straight's CastTime 0 became a wind-up of %s", windup)
	}
	if hold != 2670*time.Millisecond {
		t.Errorf("Deep Straight's HoldTime 2670 became %s", hold)
	}
}

// TestACancelDropsThePendingCastWithoutAResult: the ONE cancel the protocol has. The
// client's 3650 drops the cast and nothing at all is sent - the same silence the
// reference shows for a cast that does not land.
func TestACancelDropsThePendingCastWithoutAResult(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the press sent %v, %v", kinds(out), err)
	}
	cancel := make([]byte, SkillCancelSize)
	binary.LittleEndian.PutUint16(cancel, CmdCZSkillCancel)
	cancel[czCharIndex] = 0
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillCancel, Size: len(cancel), Data: cancel,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a cancel sent %v, want nothing at all", kinds(out))
	}
	if p.sess.castOf(0) != nil {
		t.Fatal("the pending cast survived the client's cancel")
	}
	// AND NOTHING FIRES LATER: the clock has no cast left to fire.
	if late := h.chaseTick(p.sess); len(late) != 0 {
		t.Fatalf("the tick after the cancel sent %v, want nothing", kinds(late))
	}
	if !strings.Contains(logs.String(), "3650") {
		t.Errorf("the cancel was not logged:\n%s", logs.String())
	}
}

// TestTheGroundCastSplitsTheSameWay: the area cast has the same two halves, and the
// SP is cut exactly ONCE - at the press, where the 3654 goes out.
func TestTheGroundCastSplitsTheSameWay(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	// A victim AT the point, so the blast contains exactly one body, and a point well
	// inside MaxR 800 / outside MinR 150 so there is no walk.
	victim := Monster{Handle: 0x182327, X: capturedWarlockX + 300, Y: capturedWarlockY,
		Cell: capturedWarlockCell, ClassID: 11600, Level: 1, HP: 5000000}
	spawnWithHP(h, victim)
	point := [3]int32{capturedWarlockX + 300, capturedWarlockY, capturedWarlockCell}

	spBefore := p.sess.Team[capturedGroundCharIndex].SP
	f := groundFrame(capturedGroundCharIndex, capturedFireball, point)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	r := recvs(out)
	if len(r) != 2 || r[0].cmd != CmdZCUpdateSP || r[1].cmd != CmdZCSkillCasting {
		t.Fatalf("the press sent %v, want 3654 then 3571 only:\n%s",
			kinds(out), logs.String())
	}
	spAfter := p.sess.Team[capturedGroundCharIndex].SP
	if spAfter >= spBefore {
		t.Errorf("the press did not cut the SP: %d -> %d", spBefore, spAfter)
	}
	if p.sess.castOf(capturedGroundCharIndex) == nil {
		t.Fatalf("no pending cast was left for the Warlock:\n%s", logs.String())
	}

	// THE BLAST, on the clock, and the SP IS NOT CUT A SECOND TIME.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out = h.chaseTick(p.sess)
	r = recvs(out)
	if len(r) != 2 || r[0].cmd != CmdZCSkillArea || r[1].cmd != CmdZCAddHP {
		t.Fatalf("the clock sent %v, want 3581 then 3557:\n%s", kinds(out), logs.String())
	}
	if got := p.sess.Team[capturedGroundCharIndex].SP; got != spAfter {
		t.Errorf("the SP moved again when the blast landed: %d -> %d", spAfter, got)
	}
	if got := binary.LittleEndian.Uint16(r[0].b[saCount:]); got != 1 {
		t.Errorf("3581 count = %d, want 1", got)
	}
	// THE RESULT IS BUILT FROM THE WORLD AS IT IS NOW: the caster's live position.
	begin := worldPos(r[0].b[saBegin:])
	if begin != [3]int32{capturedWarlockX, capturedWarlockY, capturedWarlockCell} {
		t.Errorf("3581 beginPos = %v, want the Warlock's own position", begin)
	}
	if dmg := math.Float64frombits(binary.LittleEndian.Uint64(r[0].b[saRecords+arDamage:])); dmg < 1 {
		t.Errorf("the blast carries damage %v, want at least 1", dmg)
	}
}

// TestFireballWaitsItsOwnCastTime is check (b) on the real row the owner named:
// Fireball 20211, CastTime 900, HoldTime 1330. The blast must wait the row's CastTime
// and the body must then be held the row's HoldTime - 900 and 1330, not a number this
// package chose.
func TestFireballWaitsItsOwnCastTime(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	// Fireball is an AREA skill aimed at a point, so the probe needs no monster: only
	// the point has to be inside the row's own MinR..MaxR against the caster's stand.
	row, ok := h.Skills.Row(capturedFireball)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedFireball)
	}
	if row.CastTime != 900 || row.HoldTime != 1330 {
		t.Fatalf("Fireball's row is CastTime %d / HoldTime %d, want the owner's own "+
			"900 / 1330 - the fixture has drifted", row.CastTime, row.HoldTime)
	}

	windup, hold, _ := castTimingFor(row)
	if windup != 900*time.Millisecond {
		t.Errorf("Fireball's wind-up is %s, want its row's CastTime 900 ms", windup)
	}
	if hold != 1330*time.Millisecond {
		t.Errorf("Fireball's hold is %s, want its row's HoldTime 1330 ms", hold)
	}

	// AND IN A REAL PRESS: one 3654 and one 3571, and NO blast.
	f := groundFrame(capturedGroundCharIndex, capturedFireball,
		[3]int32{capturedPointX, capturedPointY, capturedPointCell})
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	r := recvs(out)
	if len(r) != 2 || r[0].cmd != CmdZCUpdateSP || r[1].cmd != CmdZCSkillCasting {
		t.Fatalf("the Fireball press sent %v, want the 3654 then the 3571 only:\n%s",
			kinds(out), logs.String())
	}
	got := p.sess.castOf(capturedGroundCharIndex)
	if got == nil {
		t.Fatalf("no pending cast was left:\n%s", logs.String())
	}
	if got.length != 900*time.Millisecond || got.hold != 1330*time.Millisecond {
		t.Fatalf("the pending cast waits %s then holds %s, want 900 ms then 1330 ms",
			got.length, got.hold)
	}

	// NOTHING FIRES UNTIL THE ROW'S OWN CASTTIME HAS RUN OUT.
	for i := 0; i < 3; i++ {
		if early := h.chaseTick(p.sess); len(early) != 0 {
			t.Fatalf("tick %d before the clock sent %v, want nothing", i, kinds(early))
		}
	}
	castDueNow(t, p.sess, capturedGroundCharIndex)
	fired := h.chaseTick(p.sess)
	if len(fired) == 0 || recvs(fired)[0].cmd != CmdZCSkillArea {
		t.Fatalf("the clock sent %v, want the %d blast:\n%s",
			kinds(fired), CmdZCSkillArea, logs.String())
	}
	// THE HOLD IS STILL ON: the record survives the blast.
	if c := p.sess.castOf(capturedGroundCharIndex); c == nil || !c.landed {
		t.Fatalf("the hold did not survive the blast: %+v", c)
	}
	if mid := h.chaseTick(p.sess); len(mid) != 0 {
		t.Fatalf("a tick inside Fireball's 1330 ms hold sent %v, want nothing",
			kinds(mid))
	}
	castHoldOver(t, p.sess, capturedGroundCharIndex)
	h.chaseTick(p.sess)
	if p.sess.castOf(capturedGroundCharIndex) != nil {
		t.Error("Fireball's hold never ended")
	}
}

// TestNoStopNoDisableIsEverPartOfACast is check (d). Over a whole cast - press, a
// click during the wind-up, the result, the end of the hold - the three packets that
// would mean "the server stopped or disabled this character" must never appear. The
// capture has 0 x 3549 and 0 x 3568 across 77 s, and its 7 x 3572 all fire during
// login, four seconds before the first cast (history\cancel-and-animation.md §2).
func TestNoStopNoDisableIsEverPartOfACast(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()}

	forbidden := map[uint16]string{
		CmdZCStopPC:        "3549 ZC_STOP_PC",
		CmdZCAttackDisable: "3568 ZC_ATTACK_DISABLE",
		CmdZCSkillDisable:  "3572 ZC_SKILL_DISABLE",
	}
	check := func(stage string, out [][]byte) {
		t.Helper()
		for _, pkt := range out {
			cmd := binary.LittleEndian.Uint16(pkt)
			if name, bad := forbidden[cmd]; bad {
				t.Fatalf("%s produced %s (%d B) - no cast ever carries one:\n%s",
					stage, name, len(pkt), logs.String())
			}
		}
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	check("the press", out)

	out, err = h.Reply(context.Background(), p.sess, moveTo(0, [3]int32{-710, 1392, 29}))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	check("a click during the wind-up", out)

	castDueNow(t, p.sess, 0)
	check("the result", h.chaseTick(p.sess))

	castHoldOver(t, p.sess, 0)
	check("the end of the hold", h.chaseTick(p.sess))

	// AND NOTHING AT ALL ON THE WAY OUT: the release is the only packet, and the
	// record is gone afterwards.
	if p.sess.castOf(0) != nil {
		t.Fatal("the cast record survived its own hold")
	}
	if later := h.chaseTick(p.sess); len(later) != 0 {
		check("after the hold", later)
		t.Fatalf("the tick after the hold sent %v, want nothing", kinds(later))
	}
}
