package zone

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAStandPointOnRealGroundIsLeftAlone is the hot path of R130: a point the mesh already
// knows is ground is returned untouched, with nothing moved and the check reported as done -
// the retreat must not be nudged by a mesh that agreed with it.
func TestAStandPointOnRealGroundIsLeftAlone(t *testing.T) {
	at := [3]int32{2240, -590, 7}
	h := &Handler{
		NavHeight: func(p [3]int32) (int32, bool) { return 12, p == at },
		NavNear: func(pt [3]int32, hr, vr int) ([3]int32, int32, bool) {
			t.Fatalf("the mesh was asked to move a point it had already called ground: %v", pt)
			return [3]int32{}, 0, false
		},
	}
	got, moved, known := h.standOnRealGround(at)
	if got != at || moved || !known {
		t.Fatalf("standOnRealGround(%v) = %v, moved %v, known %v - want the same point, "+
			"nothing moved, checked", at, got, moved, known)
	}
}

// TestAStandPointInAWallIsMovedToTheNearestGround is the defect his question named: a retreat
// computed by arithmetic alone can land inside geometry, and a body ordered there never
// arrives - a press that dies. The point must move to real ground, and say that it moved.
func TestAStandPointInAWallIsMovedToTheNearestGround(t *testing.T) {
	wall := [3]int32{1000, 2000, 3}
	ground := [3]int32{1040, 2030, 3}
	h := &Handler{
		NavHeight: func(p [3]int32) (int32, bool) { return 0, p != wall },
		NavNear: func(pt [3]int32, hr, vr int) ([3]int32, int32, bool) {
			if pt != wall {
				t.Fatalf("the mesh was asked to resolve %v, want only the point it refused (%v)",
					pt, wall)
			}
			return ground, 12, true
		},
	}
	got, moved, known := h.standOnRealGround(wall)
	if !moved || !known {
		t.Fatalf("standOnRealGround(%v) = %v, moved %v, known %v - want it MOVED onto the "+
			"ground the mesh named", wall, got, moved, known)
	}
	if got != ground {
		t.Fatalf("the stand point moved to %v, want the mesh's own answer %v", got, ground)
	}
}

// TestAStandPointWithNoGroundNearbyIsWalkedAnyway: a refused retreat is a press that does
// nothing (the R104 measurement - 33 dead presses), so a point with no ground inside the
// search radius is still walked, and the caller is told the mesh had no opinion that helped.
func TestAStandPointWithNoGroundNearbyIsWalkedAnyway(t *testing.T) {
	wall := [3]int32{5000, 5000, 9}
	h := &Handler{
		NavHeight: func(p [3]int32) (int32, bool) { return 0, false },
		NavNear:   func(pt [3]int32, hr, vr int) ([3]int32, int32, bool) { return [3]int32{}, 0, false },
	}
	got, moved, known := h.standOnRealGround(wall)
	if got != wall || moved {
		t.Fatalf("standOnRealGround(%v) = %v (moved %v), want the point unchanged - this "+
			"function must never invent a destination", wall, got, moved)
	}
	if !known {
		t.Fatalf("known = false, but this zone HAS a mesh that answered - the caller cannot "+
			"log an honest line without that")
	}
}

// TestAZoneWithNoMeshDoesNotPretendItChecked keeps the Linux target (no pathtool.exe) honest:
// no mesh means no opinion, and the caller must be able to tell "checked, fine" from "never
// checked".
func TestAZoneWithNoMeshDoesNotPretendItChecked(t *testing.T) {
	p := [3]int32{1, 2, 3}
	h := &Handler{}
	got, moved, known := h.standOnRealGround(p)
	if got != p || moved || known {
		t.Fatalf("a zone with no mesh answered %v, moved %v, known %v - want the point "+
			"unchanged and known = false", got, moved, known)
	}
}

// TestAStepBackOutOfAWallWalksToRealGround is R130 end to end, on the step-back path that
// actually computes its destination by arithmetic (attackRetreatPoint, R104). The mesh is
// told to say the retreat point is NOT ground and to name one that is; the press must then
// walk the body to the mesh's point, and the log must name both.
//
// WHY THIS ONE: "i dont want the character to suddenly get stuck due to collision. we want
// the attack to land at the stopping point but in a valid position." The step-back is the
// only stand point this zone computes without asking the path engine, so it is the only one
// that can be ordered into geometry - and a body sent into a wall never arrives.
func TestAStepBackOutOfAWallWalksToRealGround(t *testing.T) {
	h, logs, p := fireArrivalZone(t)

	// THE ONLY GROUND THIS MESH KNOWS, chosen away from the arithmetic point so the
	// correction is visible in the packet and in the log.
	// 201 units from the target's centre: outside the min ring (MinR 50 + its body 60 + its
	// own 50 = 160) and well inside MaxR 900, so the pull-in below has nothing to do and
	// the walk must end EXACTLY here.
	corrected := [3]int32{-919, 2110, 136}
	h.NavHeight = func(pt [3]int32) (int32, bool) { return 7, pt == corrected }
	h.NavNear = func(pt [3]int32, hr, vr int) ([3]int32, int32, bool) {
		return corrected, 7, true
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	rep := arrivalAt(t, h, p.sess, fireArrivalEdge(45))

	if n := countLines(logs.String(), "STEP BACK: the retreat point"); n != 1 {
		t.Fatalf("the step-back line was written %d time(s), want exactly 1:\n%s",
			n, logs.String())
	}
	want := fmt.Sprintf("moved to (%d, %d, cell %d)", corrected[0], corrected[1], corrected[2])
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the step-back line does not name the ground the mesh gave (%q):\n%s",
			want, logs.String())
	}
	if p.sess.Walk == nil {
		t.Fatalf("the corrected step-back left no walk:\n%s", logs.String())
	}
	if got := p.sess.Walk.goal; got != corrected {
		t.Fatalf("the walk's goal is %v, want the mesh's ground %v - the body would be "+
			"ordered into the wall the retreat point fell in:\n%s", got, corrected,
			logs.String())
	}
	if n := hitAttacks(rep); n != 0 {
		t.Fatalf("the corrected step-back sent %d attack packet(s) - it is a WALK out, the "+
			"press is held:\n%s", n, logs.String())
	}
}

// TestTheFirstHitStopsTheWalkThere is R132, his observation: "the character will reposition
// themselves when hit started landing?"
//
// MEASURED (logs\zone_go_live.log 17:50:17): a press from out of reach started an approach
// to the ring, the wide band was satisfied while she was still walking, the hit fired at
// (1442,-821) - and she kept walking, so the second hit came from (1575,-804), 138 units
// further on. The reference stops the body when the attack starts (IsAttackArea yes ->
// SetMoveState(MS_STOP) -> SetActionState(AS_ATTACK), FieldChar_FSM.cpp:28-80), so the walk
// in flight must be CLOSED at the point the hit was built from.
func TestTheFirstHitStopsTheWalkThere(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan // the ring needs a planner; the fixture has none by default

	// OUT OF REACH, so the press becomes an approach walk.
	far := [3]int32{-919, 1900, 136}
	p.sess.Pos = far
	p.sess.Team[0].Position = far
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press from %v produced %d move(s), want 1:\n%s", far, moves(out),
			logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("no approach walk carrying the press:\n%s", logs.String())
	}
	goal := w.goal

	// THE BODY IS INSIDE THE BAND BUT SHORT OF THE GOAL: 190 units into the leg at the
	// packet speed - past the band's edge by ~14 units and still ~36 short of the ring - which is
	// the state the client reaches before the walk ends.
	spd := h.packetSpeed(&p.sess.Team[0])
	if spd <= 0 {
		t.Fatalf("no packet speed")
	}
	w.began = time.Now().Add(-time.Duration(250.0 / spd * float64(time.Second)))
	fired := h.chaseTick(p.sess)

	if n := hitAttacks(fired); n != 1 {
		t.Fatalf("the tick fired %d hit(s) with the body inside the band, want 1:\n%s",
			n, logs.String())
	}
	// (1) THE WALK IS GONE - she is not walking to the ring any more.
	if got := p.sess.approachOf(0); got != nil {
		t.Fatalf("the walk is still installed after the first hit (goal %v): she keeps "+
			"walking while swinging:\n%s", got.goal, logs.String())
	}
	if p.sess.Walk != nil {
		t.Fatalf("Walk still holds a walk after the first hit:\n%s", logs.String())
	}
	// (2) AND THE BODY IS PINNED WHERE THE HIT CAME FROM: a reading two seconds later, long
	// past the leg's duration, must answer the same point and not the ring.
	body := p.sess.attackBodyPoint(0, time.Now().Add(2*time.Second), spd)
	if body.pos == goal {
		t.Fatalf("two seconds after the hit the body reads the RING %v - it was not pinned "+
			"at the point the hit was built from:\n%s", goal, logs.String())
	}
	if d := flatDist(body.pos, goal); d < 1 {
		t.Fatalf("the pin put the body on the ring anyway (%v)", body.pos)
	}
	// (3) THE LINE SAYS SO, naming the dropped walk and where she stands.
	if n := countLines(logs.String(), "is DROPPED - the body stands at"); n != 1 {
		t.Fatalf("the drop was logged %d time(s), want 1:\n%s", n, logs.String())
	}
	t.Logf("pinned at %v (walk goal %v dropped)", body.pos, goal)
}
