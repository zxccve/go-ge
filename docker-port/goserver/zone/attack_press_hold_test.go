package zone

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// A PRESS INSIDE THE CHARACTER'S OWN ATTACK DELAY IS KEPT, NOT DROPPED (R100).
//
// THE REFERENCE, and what these tests hold this zone to:
//
//	FieldChar.cpp:955-982   DispatchAttack: while the delay is running it returns BEFORE
//	                        `m_lastRequest->type = REQ_NONE`, so the request slot KEEPS
//	                        the client's press
//	ProcessRequest / PlayerChar.cpp:265
//	                        the stored request is run again on every tick, so the swing
//	                        goes out on the first pass where the delay is over
//
// THE OWNER-VISIBLE HALF: the fast fighter (400 ms) rarely notices a dropped press; the
// warlock (2.244 s, stance 20210) is inside its own cooldown almost always, so dropping
// the press made it feel unresponsive. logs\zone_go_live.log holds 183 `ATTACK held`
// lines from one session - every one of them a press thrown away.
//
// THE CLOCK IS HANDED IN, not waited on: attackAt takes `now` and pressTick takes `now`,
// so a test can put the last hit an exact number of milliseconds in the past and ask the
// tick at an exact instant. `interval` is never a constant here either - it is read from
// the same sheet the handler reads (StatusOf -> ASPD), so the day the stance table or the
// item numbers change, only a real regression stays red.

// holdSetup is the shared opening of every test in this file: a zone, a log buffer, the
// captured player in reach of the captured Scarecrow, the press's own frame, and the
// number of milliseconds a press waits when it lands INSIDE the character's delay.
//
// `inside` is 932 ms - the press the owner's log measured at 19:41:43 - whenever the
// character's own interval is longer than that. A faster sheet falls back to half its
// interval, so the test still asks the same question ("a press inside the delay") without
// pretending a number the data does not give.
func holdSetup(t *testing.T) (h *Handler, logs *stringBuf, p *player, frame protocol.Frame,
	interval, inside time.Duration) {

	t.Helper()
	h, logs, p = attackZone(t)
	interval = attackInterval(h.sheetFor(p.sess.Team[0]))
	if interval <= 0 {
		t.Fatalf("the sheet gives this character no attack delay (ASPD %v), so there is no "+
			"cadence to hold a press inside - the data this test is written against is "+
			"missing", h.sheetFor(p.sess.Team[0]).ASPD)
	}
	inside = 932 * time.Millisecond
	if inside >= interval {
		inside = interval / 2
	}
	f := attackFrame(0, capturedTarget)
	frame = protocol.Frame{Cmd: CmdCZAttack, Size: len(f), Data: f}
	return h, logs, p, frame, interval, inside
}

// TestPressInsideTheDelayIsKeptAndFiresWhenTheDelayEnds is pass condition 1: a press
// 932 ms into the delay sends NO attack packet, and exactly ONE hit leaves when the clock
// is advanced past the interval.
func TestPressInsideTheDelayIsKeptAndFiresWhenTheDelayEnds(t *testing.T) {
	h, logs, p, frame, interval, inside := holdSetup(t)

	now := time.Now()
	p.sess.noteAttack(0, now.Add(-inside)) // the last hit landed `inside` ago
	hpBefore := h.monsterHPFor(capturedTarget)

	out, why := h.attackAt(p.sess, frame, attackPress, nil, now)
	if len(out) != 0 {
		t.Fatalf("a press %v into the %v delay produced %d packet(s), want 0 - the "+
			"character cannot swing twice that fast:\n%s", inside, interval, len(out),
			logs.String())
	}
	if why == "" {
		t.Errorf("the held press gave no reason at all:\n%s", logs.String())
	}
	if !strings.Contains(why, "kept") {
		t.Errorf("the reason does not say the press was KEPT, so a reader would take it "+
			"for a drop: %q", why)
	}
	if hp := h.monsterHPFor(capturedTarget); hp != hpBefore {
		t.Fatalf("the held press moved the monster's HP to %v (was %v):\n%s", hp, hpBefore,
			logs.String())
	}
	if p.sess.pressOf(0) == nil {
		t.Fatalf("the press was DROPPED, not kept - that is the bug this lane removes:\n%s",
			logs.String())
	}
	heldLine := lineWith(logs.String(), "ATTACK held")
	if !strings.Contains(heldLine, "(kept, not dropped - it fires when the delay ends)") {
		t.Errorf("the held line does not say the press was kept:\n%s", heldLine)
	}

	// THE CLOCK IS ADVANCED PAST THE INTERVAL - to the exact instant the delay ends, which
	// is the press's own age when the reference would retry it. Exactly one hit leaves.
	fired := h.pressTick(p.sess, now.Add(interval-inside))
	if len(fired) != 2 {
		t.Fatalf("when the delay ran out the kept press produced %d packet(s), want 2 "+
			"(3564 then 3557, the normal press path):\n%s", len(fired), logs.String())
	}
	if cmd := binary.LittleEndian.Uint16(fired[0]); cmd != CmdZCAttackSingle {
		t.Fatalf("the kept press fired cmd %d, want %d ZC_ATTACK_SINGLE", cmd,
			CmdZCAttackSingle)
	}
	if hp := h.monsterHPFor(capturedTarget); !(hp < hpBefore) {
		t.Errorf("the kept press fired but the monster's HP is still %v (was %v)", hp,
			hpBefore)
	}
	if p.sess.pressOf(0) != nil {
		t.Errorf("the press is still held after it fired - it would fire a second time")
	}
	retry := lineWith(logs.String(), "ATTACK RETRIED")
	if !strings.Contains(retry, "ATTACK RETRIED char 0:") ||
		!strings.Contains(retry, "and the delay has now run out") {
		t.Errorf("the fire wrote no 'ATTACK RETRIED char 0: ... the delay has now run "+
			"out' line:\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), "ATTACK RETRIED char"); n != 1 {
		t.Errorf("%d 'ATTACK RETRIED char' line(s), want exactly 1:\n%s", n, logs.String())
	}
	// THE RETRY IS NOT A NEW CLIENT REQUEST: one 3666 came in, so the request line stays
	// at one and the retry says what it is instead.
	if n := strings.Count(logs.String(), "REQ ATTACK"); n != 1 {
		t.Errorf("%d 'REQ ATTACK' line(s) for ONE client press, want exactly 1 - the "+
			"retry is not a second request:\n%s", n, logs.String())
	}
	t.Logf("retry line: %s", retry)

	// AND NOT TWICE: a later tick has nothing left to fire.
	if again := h.pressTick(p.sess, now.Add(3*interval)); len(again) != 0 {
		t.Errorf("a later tick fired %d packet(s) for a press that already fired:\n%s",
			len(again), logs.String())
	}
}

// TestMoveOrderDropsTheKeptPress is pass condition 2: a press held, then a move order for
// that character -> no hit ever fires for the held press.
//
// It goes through Handler.noteMoveOrder, which is the function zone.go's MOVE_PC path
// calls for a report that is the PLAYER'S OWN order (not a walk this zone sent) - so the
// test exercises the same code the wire does, not a copy of it.
func TestMoveOrderDropsTheKeptPress(t *testing.T) {
	h, logs, p, frame, interval, inside := holdSetup(t)

	now := time.Now()
	p.sess.noteAttack(0, now.Add(-inside))
	if out, _ := h.attackAt(p.sess, frame, attackPress, nil, now); len(out) != 0 {
		t.Fatalf("the press was not held, so this test proves nothing:\n%s", logs.String())
	}
	if p.sess.pressOf(0) == nil {
		t.Fatalf("the press was not kept, so this test proves nothing:\n%s", logs.String())
	}

	h.noteMoveOrder(p.sess, 0, 120)
	if p.sess.pressOf(0) != nil {
		t.Fatalf("a move order left the kept press in place - it would fire mid-walk")
	}
	if out := h.pressTick(p.sess, now.Add(2*interval)); len(out) != 0 {
		t.Fatalf("the move order sent %d packet(s) for the press it cleared, want 0:\n%s",
			len(out), logs.String())
	}
	if strings.Contains(logs.String(), "ATTACK RETRIED char") {
		t.Errorf("a hit was retried for a press a move order had cleared:\n%s",
			logs.String())
	}
}

// TestTwoPressesInsideTheDelayFireOneHit is pass condition 3: two presses inside the delay
// -> exactly ONE hit at the boundary. One slot per character, newest wins.
func TestTwoPressesInsideTheDelayFireOneHit(t *testing.T) {
	h, logs, p, frame, interval, inside := holdSetup(t)

	now := time.Now()
	p.sess.noteAttack(0, now.Add(-inside))
	if out, _ := h.attackAt(p.sess, frame, attackPress, nil, now); len(out) != 0 {
		t.Fatalf("the first press was not held, so this test proves nothing:\n%s",
			logs.String())
	}
	second := now.Add(100 * time.Millisecond)
	if out, _ := h.attackAt(p.sess, frame, attackPress, nil, second); len(out) != 0 {
		t.Fatalf("the second press produced %d packet(s) inside the delay, want 0:\n%s",
			len(out), logs.String())
	}
	held := p.sess.pressOf(0)
	if held == nil {
		t.Fatalf("neither press is held - both were dropped:\n%s", logs.String())
	}
	if !held.pressedAt.Equal(second) {
		t.Errorf("the slot holds the press of %s, want the NEWER one of %s (one slot per "+
			"character, newest wins)", held.pressedAt.Format("15:04:05.000000"),
			second.Format("15:04:05.000000"))
	}

	fired := h.pressTick(p.sess, now.Add(interval-inside))
	if len(fired) != 2 {
		t.Fatalf("the two kept presses produced %d packet(s) at the boundary, want 2 "+
			"(ONE hit = 3564 + 3557):\n%s", len(fired), logs.String())
	}
	if n := strings.Count(logs.String(), "ATTACK RETRIED char"); n != 1 {
		t.Errorf("%d 'ATTACK RETRIED char' line(s), want exactly 1:\n%s", n, logs.String())
	}
	if out := h.pressTick(p.sess, now.Add(2*interval)); len(out) != 0 {
		t.Errorf("a later tick fired %d more packet(s), want 0:\n%s", len(out), logs.String())
	}
}

// TestKeptPressDiesWithTheEngagement is pass condition 4: the held press dies with the
// engagement - the target killed, and the target gone - and sends nothing.
func TestKeptPressDiesWithTheEngagement(t *testing.T) {
	t.Run("target killed", func(t *testing.T) {
		h, logs, p, frame, interval, inside := holdSetup(t)
		now := time.Now()
		p.sess.noteAttack(0, now.Add(-inside))
		if out, _ := h.attackAt(p.sess, frame, attackPress, nil, now); len(out) != 0 {
			t.Fatalf("the press was not held, so this test proves nothing:\n%s",
				logs.String())
		}
		hpBefore := h.monsterHPFor(capturedTarget)
		h.monsterHP[capturedTarget] = 0 // another body's hit killed it while the press waited

		if out := h.pressTick(p.sess, now.Add(interval-inside)); len(out) != 0 {
			t.Fatalf("a press kept against a DEAD target produced %d packet(s), want 0:\n%s",
				len(out), logs.String())
		}
		if p.sess.pressOf(0) != nil {
			t.Errorf("the kept press survived its target's death")
		}
		if strings.Contains(logs.String(), "ATTACK RETRIED char") {
			t.Errorf("a hit was retried against a dead target:\n%s", logs.String())
		}
		if hp := h.monsterHPFor(capturedTarget); hp != 0 || hpBefore <= 0 {
			t.Errorf("the ledger reads %v (was %v) - this test proves nothing", hp, hpBefore)
		}
	})

	t.Run("target gone", func(t *testing.T) {
		h, logs, p, frame, interval, inside := holdSetup(t)
		now := time.Now()
		p.sess.noteAttack(0, now.Add(-inside))
		if out, _ := h.attackAt(p.sess, frame, attackPress, nil, now); len(out) != 0 {
			t.Fatalf("the press was not held, so this test proves nothing:\n%s",
				logs.String())
		}
		h.spawnedMonster = nil // it left this zone while the press waited

		if out := h.pressTick(p.sess, now.Add(interval-inside)); len(out) != 0 {
			t.Fatalf("a press kept against a target that LEFT produced %d packet(s), "+
				"want 0:\n%s", len(out), logs.String())
		}
		if p.sess.pressOf(0) != nil {
			t.Errorf("the kept press survived its target leaving the zone")
		}
	})
}

// TestKeptPressRequiresATickToFire pins the clock this lane rides on: Session.wantsTick
// must answer true while a press is held, or the read loop blocks on the socket and the
// kept press never fires - the client sends no further packet until the player clicks
// again (attack_swing.go's header).
func TestKeptPressRequiresATickToFire(t *testing.T) {
	h, logs, p, frame, _, inside := holdSetup(t)
	if p.sess.wantsTick() {
		t.Logf("this session already wants a tick - the check below is weaker but the " +
			"press is still held")
	}
	now := time.Now()
	p.sess.noteAttack(0, now.Add(-inside))
	if out, _ := h.attackAt(p.sess, frame, attackPress, nil, now); len(out) != 0 {
		t.Fatalf("the press was not held, so this test proves nothing:\n%s", logs.String())
	}
	if !p.sess.wantsTick() {
		t.Fatalf("a session holding a press does not want a tick, so nothing would ever "+
			"fire it:\n%s", logs.String())
	}
}
