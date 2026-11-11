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

// THE SKILL APPROACH (R87): an out-of-range press walks, and the cast fires at the
// end of the walk.
//
// The oracle for the SHAPE is the owner's own recording
// `captures\rangeapproach20261031\20261031-183747`, read in both directions:
//
//	c2s +536    3646 char 1 skill 10562 target 1602174     <- pressed OUT OF RANGE
//	s2c +148432 3548 handle 1619551 -> (-2326, 1267, 50) reason 2   <- a stand point
//	              no client 3598 in the whole recording names, so the SERVER sent it
//	s2c +156215 3573 handle 1619551 skill 10562 begin (-2326, 1267, 50)  <- the hit,
//	              from exactly the point the walk named, with NO second 3646 first
//
// The numbers below are this project's own skills (Rapid Shot 10561 MinR 1 / MaxR
// 1200, Fireball 20211 MinR 150 / MaxR 800) because the capture's skill 10562/10563
// rows are not in this tree's table; what the capture decides is the BEHAVIOUR, and
// every packet field asserted here is the field the capture pinned.

// directPlan is the path engine at its simplest, and it is what these tests hand
// Handler.Plan: the stand point is `standoff` units from the target, on the line
// towards the caster, standing on the target's own cell. It answers a route length so
// the walk's clock is a real number.
func directPlan(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
	goal := pointTowards(target, from, standoff)
	goal[2] = target[2]
	route := math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
	return goal, nil, route, false, true, nil
}

// farScarecrow is the captured Scarecrow moved exactly one unit past Rapid Shot's
// MaxR (1200) from the captured caster: x = -910 + 1201, same y, so the distance is
// 1201 and nothing else about the fixture changes.
func farScarecrow() Monster {
	m := scarecrow()
	m.X = -910 + 1201
	m.Y = 1392
	m.Cell = 29
	return m
}

// walkFor drives a walk to its ARRIVAL, without waiting: the body has covered its route
// (eta), so the walk's distance test answers yes on the next tick - and NOT so far that
// it is given up.
//
// It used to advance to `dialogDue` (eta + 250 ms). There is no such clock any more
// (piece 2d): the arrival is the body's own distance, and the body walks its route in
// eta.
func walkFor(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Walk == nil {
		t.Fatal("there is no walk to advance")
	}
	sess.Walk.began = time.Now().Add(-(sess.Walk.eta + 50*time.Millisecond))
}

// walkPastArrival no longer exists as a SECOND clock: a walk ends on the tick its body
// arrives, so there is nothing left to advance after walkFor. It is kept as a named
// assertion for the calls that meant "the walk is over now", so a test that still
// expects a walk to survive its own arrival fails loudly here instead of silently
// measuring a state piece 2d removed.
func walkPastArrival(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Walk != nil {
		t.Fatalf("the walk is still running after its own arrival (%s of a %s route) - "+
			"something put a second clock back", time.Since(sess.Walk.began).Round(
			time.Millisecond), sess.Walk.eta)
	}
}

// recv is one packet of a reply, as a test wants to look at it: its command id and its
// bytes.
type recv struct {
	cmd uint16
	b   []byte
}

func recvs(out [][]byte) []recv {
	rs := make([]recv, 0, len(out))
	for _, p := range out {
		rs = append(rs, recv{cmd: binary.LittleEndian.Uint16(p), b: p})
	}
	return rs
}

// kinds names the commands of a reply, for a readable failure message.
func kinds(out [][]byte) []uint16 {
	ks := make([]uint16, 0, len(out))
	for _, p := range out {
		ks = append(ks, binary.LittleEndian.Uint16(p))
	}
	return ks
}

// TestAimedCastBeyondMaxRWalks walks, it does not cast (check a).
//
// The press is 1201 units from a monster whose skill reaches 1200. It must answer ONE
// ZC_MOVE_PC - to a stand point the path engine chose, on the caster's own handle,
// with the reference's MR_SKILL reason - and NOTHING else: no 3571, no 3573, and no
// refusal. A cast here would be the bug R83 found; a refusal is the bug the owner
// reported.
func TestAimedCastBeyondMaxRWalks(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{farScarecrow()}

	row, ok := h.Skills.Row(capturedSkill)
	if !ok {
		t.Fatalf("skill %d is not in datatable_skill.xml", capturedSkill)
	}
	var gotFrom, gotTarget [3]int32
	var gotStandoff float64
	calls := 0
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		calls++
		gotFrom, gotTarget, gotStandoff = from, target, standoff
		return directPlan(from, target, standoff)
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("an out-of-range press produced %v, want exactly one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	move := recvs(out)[0]
	if move.cmd != CmdZCMovePC {
		t.Fatalf("the only packet is cmd %d, want %d ZC_MOVE_PC", move.cmd, CmdZCMovePC)
	}
	if len(move.b) != 30 {
		t.Fatalf("ZC_MOVE_PC is %d bytes, want 30", len(move.b))
	}

	// THE HANDLE IS THE CASTER'S: charIndex 0 here, and a follower would be its own
	// slot (the capture's approach moved handles 1619550..1619552, one per member).
	if got := binary.LittleEndian.Uint32(move.b[2:]); got != capturedCaster {
		t.Errorf("move +2 handle = %d, want the caster %d", got, capturedCaster)
	}
	// THE REASON IS MR_SKILL 2, the word the reference uses for exactly this walk, and
	// not MR_DIALOG 4, which is the NPC-click walk.
	if got := binary.LittleEndian.Uint32(move.b[26:]); got != ReasonSkill {
		t.Errorf("move +26 reason = %d, want %d (MR_SKILL: the reference's own value "+
			"for a skill approach)", got, ReasonSkill)
	}
	// THE DESTINATION IS THE PLANNED STAND POINT, inside the skill's own MaxR.
	dest := [3]int32{
		int32(binary.LittleEndian.Uint32(move.b[10:])),
		int32(binary.LittleEndian.Uint32(move.b[14:])),
		int32(binary.LittleEndian.Uint32(move.b[18:])),
	}
	want := pointTowards([3]int32{291, 1392, 29}, [3]int32{-910, 1392, 29},
		row.MaxR*castStandFraction)
	want[2] = 29
	if dest != want {
		t.Errorf("move destination = %v, want the stand point %v", dest, want)
	}
	if d, in := skillRangeAllowed(dest, [3]int32{291, 1392, 29}, row); !in {
		t.Errorf("the stand point %v is %g from the target, outside MinR %g .. MaxR %g",
			dest, d, row.MinR, row.MaxR)
	}

	// THE ENGINE WAS ASKED THE RIGHT QUESTION: from where the character STANDS, for the
	// target's own position, at a stand-off the skill's row decided (0.9 x MaxR).
	if calls != 1 {
		t.Errorf("the path engine was called %d time(s), want 1", calls)
	}
	if gotFrom != [3]int32{-910, 1392, 29} {
		t.Errorf("planned from %v, want the caster's own position (-910, 1392, 29)", gotFrom)
	}
	if gotTarget != [3]int32{291, 1392, 29} {
		t.Errorf("planned towards %v, want the target (291, 1392, 29)", gotTarget)
	}
	if math.Abs(gotStandoff-row.MaxR*castStandFraction) > 0.001 {
		t.Errorf("asked for a stand-off of %g, want %g (0.9 x MaxR %g)",
			gotStandoff, row.MaxR*castStandFraction, row.MaxR)
	}

	// AND THE PENDING CAST IS THE CLIENT'S OWN FRAME, uncast.
	w := p.sess.Walk
	if w == nil || w.cast == nil {
		t.Fatal("no walk with a pending cast was left on the session")
	}
	if w.caster != 0 {
		t.Errorf("walk caster = %d, want 0 (the character that pressed)", w.caster)
	}
	if w.cast.skill != capturedSkill || w.cast.target != capturedTarget {
		t.Errorf("pending cast is skill %d target %d, want %d/%d",
			w.cast.skill, w.cast.target, capturedSkill, capturedTarget)
	}
	if w.cast.ground {
		t.Error("the pending cast is marked as a GROUND cast; it came from a 3646")
	}
	if w.goal != dest {
		t.Errorf("walk goal = %v, want the packet's destination %v", w.goal, dest)
	}

	// MID-WALK: NOTHING. A second packet would re-target the actor or fire the cast
	// from a place the character has not reached.
	if mid := h.chaseTick(p.sess); len(mid) != 0 {
		t.Fatalf("mid-walk the tick sent %v, want nothing", kinds(mid))
	}

	line := logs.String()
	for _, want := range []string{"SKILL APPROACH", "1201", "MinR 1", "MaxR 1200",
		"the cast fires when the walk ends"} {
		if !strings.Contains(line, want) {
			t.Errorf("the approach line does not name %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "SKILL refused") {
		t.Errorf("an out-of-range press was REFUSED and -strictrange is OFF:\n%s", line)
	}
	t.Logf("approach: %s", strings.TrimSpace(firstLine(line, "SKILL APPROACH")))
}

// TestAimedCastFiresWhenTheWalkEnds is check (b): the walk's own clock ends it and the
// cast goes out with the in-range packet order and the in-range content - 3571 then
// 3573, the caster's handle, the skill, the target, the damage.
func TestAimedCastFiresWhenTheWalkEnds(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{farScarecrow()}
	h.Plan = directPlan

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the approach did not start: %v, %v", kinds(out), err)
	}
	goal := p.sess.Walk.goal
	walkFor(t, p.sess)

	out := h.chaseTick(p.sess)
	if len(out) != 1 {
		t.Fatalf("the end of the walk sent %v, want the one 3571 (its result waits for "+
			"the cast's own clock):\n%s", kinds(out), logs.String())
	}
	r := recvs(out)
	if r[0].cmd != CmdZCSkillCasting {
		t.Fatalf("first packet is cmd %d, want %d ZC_SKILL_CASTING", r[0].cmd,
			CmdZCSkillCasting)
	}
	// IT CASTS FROM THE STAND POINT IT WALKED TO: 3571's beginPos is +6, and that is
	// the field the reference's own 3573 pinned to its move's destination.
	if begin := worldPos(r[0].b[scBegin:]); begin != goal {
		t.Errorf("3571 beginPos = %v, want the stand point %v", begin, goal)
	}
	if got := binary.LittleEndian.Uint32(r[0].b[scCaster:]); got != capturedCaster {
		t.Errorf("3571 +2 caster = %d, want %d", got, capturedCaster)
	}
	if got := binary.LittleEndian.Uint16(r[0].b[scSkill:]); uint32(got) != capturedSkill {
		t.Errorf("3571 +30 skill = %d, want %d", got, capturedSkill)
	}

	// THE RESULT, WHEN THE CLOCK FIRES: 3573, from the body's live point.
	castDueNow(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the cast's clock sent %v, want the one %d:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	r = recvs(out)
	if begin := worldPos(r[0].b[smBegin:]); begin != goal {
		t.Errorf("3573 beginPos = %v, want the stand point %v", begin, goal)
	}
	if got := binary.LittleEndian.Uint32(r[0].b[smTarget:]); got != capturedTarget {
		t.Errorf("3573 +36 target = %d, want %d", got, capturedTarget)
	}
	dmg := math.Float64frombits(binary.LittleEndian.Uint64(r[0].b[smDamage:]))
	if dmg < 1 {
		t.Errorf("3573 +40 damage = %v, want the caster's own number", dmg)
	}
	// The caster's stored position followed the walk - a second cast must not measure
	// from where the character used to stand - and the walk is OVER on the tick the cast
	// fired (piece 2d: the arrival closes the walk, so the ZC_STOP_PC the other players are
	// owed leaves there and nothing is left waiting for a second clock).
	if p.sess.Team[0].Position != goal {
		t.Errorf("the caster's stored position is %v, want the stand point %v",
			p.sess.Team[0].Position, goal)
	}
	if p.sess.Walk != nil {
		t.Fatalf("after the cast the walk should have ended on its own arrival: %+v",
			p.sess.Walk)
	}
	// ONE PRESS, ONE CAST: a later tick sends nothing at all.
	if again := h.chaseTick(p.sess); len(again) != 0 {
		t.Fatalf("a later tick sent %v, want nothing (the cast fired once)", kinds(again))
	}
	line := logs.String()
	for _, want := range []string{"the body has walked to the stand point", "SKILL_CAST",
		"damage "} {
		if !strings.Contains(line, want) {
			t.Errorf("the log is missing %q:\n%s", want, line)
		}
	}
	// THE PHASE TURNED ON THE ONE EVENT THAT TURNS IT (R89): the 3571 going out. This
	// line is how the owner's next in-game test is read, so it is pinned here rather
	// than left to a 2 a.m. grep.
	if !strings.Contains(line, "cast phase approaching -> animating") {
		t.Errorf("the phase change was not logged:\n%s", line)
	}
	if strings.Contains(line, "cast phase approaching -> none") {
		t.Errorf("a cast that fired was logged as cancelled:\n%s", line)
	}
	t.Logf("the cast after the walk:\n%s", strings.TrimSpace(firstLine(line, "SKILL_CAST")))
}

// TestGroundCastFiresWhenTheWalkEnds is check (c): the same for a POINT cast, whose
// in-range answer is 3654 + 3571 + 3581 + one 3557 per victim inside the blast.
func TestGroundCastFiresWhenTheWalkEnds(t *testing.T) {
	h, logs := groundZone(t)
	p := warlock()
	h.Plan = directPlan

	// The point is 900 units west of the Warlock: 100 past Fireball's MaxR 800.
	point := [3]int32{capturedWarlockX - 900, capturedWarlockY, capturedWarlockCell}
	victim := Monster{Handle: 0x182327, X: point[0] + 60, Y: point[1], Cell: point[2],
		ClassID: 11600, Level: 1, HP: 5000000}
	spawnWithHP(h, victim)

	f := groundFrame(capturedGroundCharIndex, capturedFireball, point)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillGround, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCMovePC {
		t.Fatalf("an out-of-range point cast produced %v, want one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	// THE CASTER WALKS, not the leader: charIndex 2 is the Warlock, and the move names
	// the Warlock's own handle.
	if got := binary.LittleEndian.Uint32(out[0][2:]); got != capturedWarlock {
		t.Errorf("move +2 handle = %d, want the Warlock %d", got, capturedWarlock)
	}
	if w := p.sess.Walk; w == nil || w.caster != capturedGroundCharIndex || !w.cast.ground {
		t.Fatalf("the walk is not the Warlock's ground-cast walk: %+v", w)
	}

	goal := p.sess.Walk.goal
	walkFor(t, p.sess)
	out = h.chaseTick(p.sess)

	// 3654, 3571 - the wind-up, in the order §10.2 measured. The 3581 and its 3557
	// wait for the cast's own clock (zone\skill_cast_clock.go).
	r := recvs(out)
	if len(r) != 2 {
		t.Fatalf("the end of the walk sent %v, want 3654, 3571:\n%s",
			kinds(out), logs.String())
	}
	if r[0].cmd != CmdZCUpdateSP || r[1].cmd != CmdZCSkillCasting {
		t.Fatalf("the order is %v, want 3654, 3571", kinds(out))
	}
	if begin := worldPos(r[1].b[scBegin:]); begin != goal {
		t.Errorf("3571 beginPos = %v, want the stand point %v", begin, goal)
	}
	// +44 = 0 and +48 = the point: a ground cast, unchanged.
	if got := binary.LittleEndian.Uint32(r[1].b[scTarget:]); got != 0 {
		t.Errorf("3571 +44 target = %d, want 0 for a ground cast", got)
	}
	if got := worldPos(r[1].b[scTargetPos:]); got != point {
		t.Errorf("3571 +48 = %v, want the client's point %v", got, point)
	}

	// THE BLAST, WHEN THE CLOCK FIRES: 3581 then one 3557.
	castDueNow(t, p.sess, capturedGroundCharIndex)
	out = h.chaseTick(p.sess)
	r = recvs(out)
	if len(r) != 2 || r[0].cmd != CmdZCSkillArea || r[1].cmd != CmdZCAddHP {
		t.Fatalf("the clock sent %v, want 3581 then 3557:\n%s", kinds(out), logs.String())
	}
	// ONE RECORD, and it is the victim the blast contains.
	count := binary.LittleEndian.Uint16(r[0].b[saCount:])
	if count != 1 {
		t.Fatalf("3581 count = %d, want 1 (the victim 60 units from the point)", count)
	}
	if got := binary.LittleEndian.Uint32(r[0].b[saRecords:]); got != victim.Handle {
		t.Errorf("3581 record handle = %d, want %d", got, victim.Handle)
	}
	// AND ONE 3557, for that same handle, carrying the same damage: the number that
	// flies.
	if got := binary.LittleEndian.Uint32(r[1].b[ahHandle:]); got != victim.Handle {
		t.Errorf("3557 +2 handle = %d, want %d", got, victim.Handle)
	}
	areaDamage := math.Float64frombits(binary.LittleEndian.Uint64(r[0].b[saRecords+arDamage:]))
	addHP := math.Float64frombits(binary.LittleEndian.Uint64(r[1].b[ahAddHP:]))
	if areaDamage != addHP {
		t.Errorf("the 3581 record carries %v and the 3557 %v - they must be one number",
			areaDamage, addHP)
	}
	t.Logf("the ground cast after the walk:\n%s",
		strings.TrimSpace(firstLine(logs.String(), "SKILL_GROUND commander")))
}

// TestCastInsideMaxRDoesNotWalk is check (d): a target already in range casts at once,
// and the path engine is never asked. This is the guard that keeps the approach from
// becoming a tax on every press.
func TestCastInsideMaxRDoesNotWalk(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()} // the capture's own 919 units
	calls := 0
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		calls++
		return directPlan(from, target, standoff)
	}

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillCasting {
		t.Fatalf("an in-range cast produced %v, want the one 3571 (the hit waits for the "+
			"cast's clock):\n%s", kinds(out), logs.String())
	}
	if calls != 0 {
		t.Errorf("the path engine was called %d time(s) for an in-range cast, want 0", calls)
	}
	if p.sess.Walk != nil {
		t.Error("an in-range cast left a walk behind")
	}
	// AND THE HIT IS OWED, not forgotten: the clock fires it.
	castDueNow(t, p.sess, 0)
	if out := h.chaseTick(p.sess); len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the in-range cast's clock sent %v, want the one 3573", kinds(out))
	}
	if s := logs.String(); strings.Contains(s, "APPROACH") {
		t.Errorf("an in-range cast logged an approach:\n%s", s)
	}
}

// TestApproachThatCannotBePlannedSendsNothing is check (e): no path engine, invalid
// ground, a clamp into the target itself, and a stand point that is still outside the
// range - four ways to have no real point to stand on. Each one says so in ONE line
// with the distance and the range, and sends NOTHING.
func TestApproachThatCannotBePlannedSendsNothing(t *testing.T) {
	cases := []struct {
		name string
		plan func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error)
		want string
	}{
		{
			name: "no path engine",
			plan: nil,
			want: "no path engine",
		},
		{
			name: "the character is not on valid ground",
			plan: func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
				return target, nil, 0, true, false, nil
			},
			want: "not valid ground",
		},
		{
			name: "the clamp ran into the target itself",
			plan: func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
				return target, nil, 500, true, true, nil
			},
			want: "clamped into the target",
		},
		{
			name: "a stand point that is still out of range",
			plan: func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
				// A planner that ignores the stand-off and answers a point 2000 away.
				return [3]int32{target[0] + 2000, target[1], target[2]}, nil, 0, false, true, nil
			},
			want: "still out of range",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, logs := castZone(t)
			p := castPlayer()
			h.spawnedMonster = []Monster{farScarecrow()}
			h.Plan = c.plan

			f := skillTargetFrame(0, capturedSkill, capturedTarget)
			out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
				Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
			})
			if err != nil {
				t.Fatalf("Reply: %v", err)
			}
			if len(out) != 0 {
				t.Fatalf("an unplannable approach sent %v, want nothing:\n%s",
					kinds(out), logs.String())
			}
			if p.sess.Walk != nil {
				t.Fatal("an unplannable approach left a walk on the session")
			}
			// The LAST line with the word: the range gate's own line comes first and
			// then the approach's refusal, which is the one that says why.
			line := lastOutOfRange(logs.String())
			for _, want := range []string{c.want, "1201", "MinR 1", "MaxR 1200"} {
				if !strings.Contains(line, want) {
					t.Errorf("the one line does not name %q:\n%s", want, logs.String())
				}
			}
			t.Logf("%s: %s", c.name, strings.TrimSpace(line))
		})
	}
}

// TestASecondPressReplacesThePendingCast is check (f): one press while a walk is
// running REPLACES it, the way a second click on an NPC replaces the walk to the first
// (chase.go, queueClick). Nothing is queued: the session ends up with ONE walk and ONE
// pending cast - the newer one - and no cast has fired.
func TestASecondPressReplacesThePendingCast(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	// TWO monsters past the range, on opposite sides, so the two presses cannot share
	// a destination by accident.
	near := farScarecrow()
	far := Monster{Handle: 0x900001, X: -910 - 1500, Y: 1392, Cell: 29,
		ClassID: 11600, Level: 1, HP: 1000}
	h.spawnedMonster = []Monster{near, far}
	h.Plan = directPlan

	first := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(first), Data: first,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the first press did not start a walk: %v %v", kinds(out), err)
	}
	firstGoal := p.sess.Walk.goal

	second := skillTargetFrame(0, capturedSkill, far.Handle)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(second), Data: second,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// The second press answers with ONE move of its own - and nothing else. A cast here
	// would be the bug: the character is still out of range.
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCMovePC {
		t.Fatalf("the second press produced %v, want one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.cast == nil {
		t.Fatal("the second press left no walk")
	}
	if w.cast.target != far.Handle {
		t.Errorf("the pending cast is target %d, want the SECOND press's %d",
			w.cast.target, far.Handle)
	}
	if w.goal == firstGoal {
		t.Errorf("the walk still aims at the first press's stand point %v", firstGoal)
	}
	if !strings.Contains(logs.String(), "a new press replaces the walk") {
		t.Errorf("the replacement was not logged:\n%s", logs.String())
	}

	// ONE walk, ONE cast: letting the clock run fires the second press's cast only.
	walkFor(t, p.sess)
	out = h.chaseTick(p.sess)
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCSkillCasting {
		t.Fatalf("the end of the walk sent %v, want the one 3571:\n%s",
			kinds(out), logs.String())
	}
	// AND ITS RESULT, when the cast's clock fires - the SECOND press's target.
	castDueNow(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	r = recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the cast's clock sent %v, want the one %d:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	if got := binary.LittleEndian.Uint32(r[0].b[smTarget:]); got != far.Handle {
		t.Errorf("the cast fired at target %d, want the second press's %d", got, far.Handle)
	}
	// ONE cast for one press: the walk ended on its arrival, and a later tick sends no
	// second hit.
	if again := h.chaseTick(p.sess); len(again) != 0 {
		t.Fatalf("a later tick sent %v, want nothing (one press, one cast)", kinds(again))
	}
	walkPastArrival(t, p.sess)
}

// TestAMoveDuringTheApproachCancelsTheCast is check (a) of R89, and it REPLACES a test
// that asserted the opposite. The old test read one capture and concluded "a move never
// cancels a skill". The bytes do not say that; they say a move never cancels a cast
// that has ALREADY SENT ITS 3571:
//
//	captures\cancel20261031\20261031-191902
//	  first cast   click at +460 (130 ms AFTER the 3571) -> echo -> the 3581 lands anyway
//	               (history\cancel-and-animation.md §2)
//	  third cast   3647 at 33574.7 -> reason-2 3548 at 33807.8 -> TWO 3598 at 34160.0
//	               with their echoes -> and then NO 3571 and NO 3581 AT ALL (§4)
//
// So the reference has a CANCEL WINDOW and the 3571 is its boundary: mid flight the
// click kills the cast, mid animation it does not. The owner said the same thing in his
// own words ("like the NPC click where u can cancel mid flight and not mid animation").
//
// What this pins: the approach is dropped, NOTHING goes out for the skill (no 3571, no
// 3573, no 3581, no stop of its own), and the click's own echo and walk take over.
func TestAMoveDuringTheApproachCancelsTheCast(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{farScarecrow()}
	h.Plan = directPlan

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the approach did not start: %v %v", kinds(out), err)
	}
	if p.sess.Walk == nil || p.sess.Walk.cast == nil {
		t.Fatalf("the out-of-range press left no pending cast:\n%s", logs.String())
	}
	// THE PHASE IS THE APPROACH: nothing has been sent for the skill yet.
	if got := p.sess.Walk.cast.phase; got != castPhaseApproaching {
		t.Fatalf("the pending cast's phase is %q, want approaching - a press that has "+
			"not sent its %d is mid flight", got, CmdZCSkillCasting)
	}

	away := [3]int32{-910 + 40, 1392 - 40, 29}
	out, err := h.Reply(context.Background(), p.sess, moveTo(0, away))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// EXACTLY ONE PACKET AND IT IS THE ORDINARY ECHO: the client's own coordinates, no
	// stop, no cancel packet, nothing else.
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCMovePC {
		t.Fatalf("a move during the approach sent %v, want only the %d echo:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	if got := worldPos(r[0].b[10:]); got != away {
		t.Errorf("the echo names %v, want the client's own point %v", got, away)
	}

	// THE CAST IS CANCELLED - the walk and its request are both gone.
	if p.sess.Walk != nil {
		t.Fatalf("the approach survived the move click: %+v", p.sess.Walk)
	}
	if c := p.sess.castOf(0); c != nil {
		t.Fatalf("a clocked cast survived the move click: %+v", c)
	}

	// AND THE WALK IS THE CLICK'S OWN, with the click-to-walk rules: one segment whose
	// destination is the point the player clicked, not the stand point.
	if len(p.sess.walks) == 0 || p.sess.walks[0].dest != away {
		t.Fatalf("the click opened no walk to %v - the segment is %+v", away,
			p.sess.walks)
	}

	// NOTHING FOR THE SKILL, NOW OR LATER: no 3571, no 3573, no 3581 - and no 3549 of
	// the cancelled approach's own making.
	for _, want := range []uint16{CmdZCSkillCasting, CmdZCSkillMelee, CmdZCSkillArea,
		CmdZCStopPC} {
		for _, pkt := range out {
			if cmd := binary.LittleEndian.Uint16(pkt); cmd == want {
				t.Fatalf("a %d went out for a cast that was cancelled", want)
			}
		}
	}
	// The tick right after: a cancelled cast owes no result, and the click's segment is
	// still being walked.
	if after := h.chaseTick(p.sess); len(after) != 0 {
		t.Fatalf("the tick after the cancel sent %v, want nothing - a cast that does not "+
			"land sends nothing at all:\n%s", kinds(after), logs.String())
	}
	// And when the click's walk is walked out, the ONLY thing owed is its stop.
	if len(p.sess.walks) > 0 {
		p.sess.walks[0].at = time.Now().Add(-time.Hour)
	}
	for _, pkt := range h.chaseTick(p.sess) {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCStopPC {
			t.Fatalf("the end of the click's walk sent %d, want only the %d stop",
				cmd, CmdZCStopPC)
		}
	}

	// AND THE ONE LINE THE OWNER'S NEXT TEST IS CHECKED WITH.
	if !strings.Contains(logs.String(),
		"cast CANCELLED during the approach by a move click (nothing sent)") {
		t.Errorf("the cancel was not logged:\n%s", logs.String())
	}
	t.Logf("%s", strings.TrimSpace(firstLine(logs.String(), "cast CANCELLED")))
}

// TestAMoveDuringAnInstantHoldDoesNotCancel is check (b): the other half of the window,
// on the row that has NO approach at all. Implode Shock (10562) states CastTime 0, so a
// press inside its range sends the result immediately and only the HOLD follows. A move
// click there must NOT cancel: the cast is animating, so the click is queued and goes out
// when the hold ends (history\cancel-and-animation.md §6.4).
func TestAMoveDuringAnInstantHoldDoesNotCancel(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	near := scarecrow()
	near.Handle = 0x900003
	near.X, near.Y, near.Cell = -1000, 1392, 29
	h.spawnedMonster = []Monster{near}

	f := skillTargetFrame(0, instantSkill, near.Handle)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the instant press sent %v, want only the %d result:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	// THE PHASE IS ANIMATING: the result has already gone, so this is the hold.
	c := p.sess.castOf(0)
	if c == nil || c.phase != castPhaseAnimating {
		t.Fatalf("the instant cast's phase is %v, want animating (its result has "+
			"already gone): %+v", c, c)
	}

	click := [3]int32{-960, 1392, 29}
	out, err = h.Reply(context.Background(), p.sess, moveTo(0, click))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a move during the hold sent %v, want nothing (it is queued):\n%s",
			kinds(out), logs.String())
	}
	// NOT CANCELLED: the record is still there, still animating, with the move kept.
	if p.sess.castOf(0) == nil {
		t.Fatal("a move during the hold cancelled the cast - the owner's rule is that " +
			"mid animation is never interrupted")
	}
	if !strings.Contains(logs.String(), "move queued until the hold ends") {
		t.Errorf("the queued move was not logged with the owner's line:\n%s",
			logs.String())
	}
	// AND IT IS RELEASED ONCE, AT THE CLICKED POINT, WHEN THE HOLD ENDS.
	castHoldOver(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCMovePC {
		t.Fatalf("the end of the hold sent %v, want one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	if got := worldPos(r[0].b[10:]); got != click {
		t.Errorf("the released move names %v, want the click %v", got, click)
	}
}

// TestACastTimeSkillMidWindUpBelongsToAnimating is check (c), and it fixes where the
// boundary is against the owner's own words: his "mid animation" INCLUDES the wind-up.
//
// Rapid Shot (10561) states CastTime 600, so a press inside its range sends the 3571 and
// then waits 600 ms for the result - the reference's AS_SKILL_CASTING, which IS
// `IsHoldAction()` (FieldChar.cpp:1023-1049). A move click inside that wind-up is
// therefore ANIMATING, not flight: it must not cancel, and the result must still land
// (history\cancel-and-animation.md §2: the click 130 ms after the 3571 and the 3581
// arrived 941 ms later).
func TestACastTimeSkillMidWindUpBelongsToAnimating(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	h.spawnedMonster = []Monster{scarecrow()} // in range: no approach, straight to the 3571

	f := skillTargetFrame(0, capturedSkill, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(f), Data: f,
	})
	if err != nil || len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillCasting {
		t.Fatalf("the press sent %v, %v - want the one %d:\n%s", kinds(out), err,
			CmdZCSkillCasting, logs.String())
	}
	// THE WIND-UP IS ANIMATING, and the phase says so.
	if c := p.sess.castOf(0); c == nil || c.phase != castPhaseAnimating {
		t.Fatalf("a cast mid-wind-up is %v, want animating: %+v", c, c)
	}
	if !p.sess.castOf(0).due.After(time.Now()) {
		t.Fatal("the wind-up is already over - this test needs to click INSIDE it")
	}

	click := [3]int32{-610, 1392, 29}
	out, err = h.Reply(context.Background(), p.sess, moveTo(0, click))
	if err != nil || len(out) != 0 {
		t.Fatalf("a move mid-wind-up sent %v, %v - want nothing (queued):\n%s",
			kinds(out), err, logs.String())
	}
	if p.sess.castOf(0) == nil {
		t.Fatal("a move mid-wind-up cancelled the cast - the wind-up is the animation")
	}

	// THE RESULT STILL LANDS, and the body has not moved to the click: the cast is
	// built from where the body stands.
	castDueNow(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCSkillMelee {
		t.Fatalf("the wind-up's clock sent %v, want the one %d:\n%s",
			kinds(out), CmdZCSkillMelee, logs.String())
	}
	if begin := worldPos(out[0][smBegin:]); begin != ([3]int32{-910, 1392, 29}) {
		t.Errorf("the %d begins at %v, want the stand point (-910, 1392, 29) - the "+
			"queued move is not the body's position", CmdZCSkillMelee, begin)
	}
	// AND THE MOVE WAITS FOR THE END OF THE HOLD, then goes out once.
	castHoldOver(t, p.sess, 0)
	out = h.chaseTick(p.sess)
	if len(out) != 1 || recvs(out)[0].cmd != CmdZCMovePC {
		t.Fatalf("the end of the hold sent %v, want one %d:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
}

// TestTwoRapidPressesThenAMoveCastNothing is check (d). Two presses while the character
// is still walking to the first one leave ONE pending action (R87's rule: a new press
// replaces the walk, never a queue), and the move click then cancels that one. The
// result: no walk, no pending cast, and nothing at all on the wire for either press.
func TestTwoRapidPressesThenAMoveCastNothing(t *testing.T) {
	h, logs := castZone(t)
	p := castPlayer()
	near := farScarecrow()
	far := Monster{Handle: 0x900001, X: -910 - 1500, Y: 1392, Cell: 29,
		ClassID: 11600, Level: 1, HP: 1000}
	h.spawnedMonster = []Monster{near, far}
	h.Plan = directPlan

	first := skillTargetFrame(0, capturedSkill, capturedTarget)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(first), Data: first,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the first press did not start a walk: %v %v", kinds(out), err)
	}
	second := skillTargetFrame(0, capturedSkill, far.Handle)
	if out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZSkillTarget, Size: len(second), Data: second,
	}); err != nil || len(out) != 1 {
		t.Fatalf("the second press did not replace the walk: %v %v", kinds(out), err)
	}
	// ONE pending action, and it is the SECOND press's.
	w := p.sess.Walk
	if w == nil || w.cast == nil || w.cast.target != far.Handle {
		t.Fatalf("the session does not hold exactly the second press's cast: %+v", w)
	}
	if w.cast.phase != castPhaseApproaching {
		t.Fatalf("the phase is %q, want approaching", w.cast.phase)
	}

	away := [3]int32{-910 + 60, 1392 - 60, 29}
	out, err := h.Reply(context.Background(), p.sess, moveTo(0, away))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	r := recvs(out)
	if len(r) != 1 || r[0].cmd != CmdZCMovePC {
		t.Fatalf("the cancelling click sent %v, want only the %d echo:\n%s",
			kinds(out), CmdZCMovePC, logs.String())
	}
	if p.sess.Walk != nil || p.sess.castOf(0) != nil {
		t.Fatalf("a pending action survived the click: walk %+v, cast %+v",
			p.sess.Walk, p.sess.castOf(0))
	}
	if len(p.sess.walks) == 0 || p.sess.walks[0].dest != away {
		t.Fatalf("the click opened no walk to %v: %+v", away, p.sess.walks)
	}
	// NOTHING IS CAST, now or on any later tick: the walk the click started is the only
	// thing left, and its end owes one stop and no skill packet.
	for i := 0; i < 3; i++ {
		for _, pkt := range h.chaseTick(p.sess) {
			switch cmd := binary.LittleEndian.Uint16(pkt); cmd {
			case CmdZCSkillCasting, CmdZCSkillMelee, CmdZCSkillArea:
				t.Fatalf("a %d went out for a cancelled cast", cmd)
			}
		}
	}
}
