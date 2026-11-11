package zone

// THE CLIENT'S OWN LEG: IT STARTS ON OUR BODY AND IT IS A ROUTE (movement piece 2b,
// 2026-11-02).
//
// CZ_MOVE_PC carries a DESTINATION and nothing else - there is no "I am at X" report
// (move.go, ParseMovePC) - so the two things this side must decide are "where did this
// leg start" and "what ground does it walk". The reference answers both on its own body:
// MS_MOVE's ON_ENTER sets m_moveStartPos = m_pos (FieldChar_FSM.cpp:246-270), ON_UPDATE
// advances its own path agent by m_moveSpeed x dt (FieldChar.cpp:240-260), and
// BroadcastStartMovement puts the DESTINATION, the start tick and the speed on the wire
// (PlayerChar.cpp:232-242) - the client follows, and the server never learns where the
// client's actor is.
//
// THESE TESTS PIN THAT, and they replace the two rules that are now deleted:
//
//   - `resyncStart`/`declaredLeg` (R106) chose the leg's start between the previous leg's
//     two ends, and `zone\walk_correction.go` (R107) paid the gap off at 200 u/s. The
//     measured cost, on the owner's own session (logs\go_zone1331.log, 2026/11/02
//     18:22:04.412745): `MOVE_PC char 2 handle 1793 -> (1964, -746) cell 108 | model was
//     at (2621, -816) 963 u off, leg 302 u` - the guess was 963 units from the body, and
//     that number was the snap.
//   - a leg was drawn on the straight chord between its two ends, whatever ground the
//     walk really crossed (R110 fixed that for the CHASE's walks only).
//
// The assertions that replace them are named one for one at each test.

import (
	"bytes"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// legZone is driftSession with a log the test can READ: the move line is the product
// here, so a discarded logger would throw away half the evidence.
func legZone(t *testing.T, handles []uint32, stand [][3]int32) (*Handler, *Session, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	// DiagEvents: the tests in this file READ the per-report move line, which is a
	// diagnostic and off by default in a real run (`-diag-events`). Turning it on here is
	// the same switch a debugging session uses - the line's wording is unchanged.
	h := &Handler{Log: log.New(&buf, "", 0), Stances: speedTable(t), DiagEvents: true}
	a := newTeam(handles, stand)
	for i := range a.sess.Team {
		a.sess.Team[i].Job = 1
		a.sess.Team[i].Stance = 10010
	}
	h.joinZone(a.sess)
	if got := h.packetSpeed(&a.sess.Team[0]); got != MoveBaseSpeed {
		t.Fatalf("packet speed = %.0f, want the base %.0f", got, MoveBaseSpeed)
	}
	if !a.sess.moveAt.IsZero() {
		t.Fatalf("the character was PLACED with an open leg (%v): this test needs a"+
			"standing start", a.sess.moveFrom)
	}
	return h, a.sess, &buf
}

// distanceToPolyline is the distance from p to the nearest point of a polyline: 0 means p
// is ON it. It is the assertion "the body walks the ground the leg was planned on" made
// checkable - the drawn position is computed in float and stored in whole units, so the
// callers allow a unit or so of rounding.
func distanceToPolyline(p [3]int32, route [][3]int32) float64 {
	best := math.Inf(1)
	for i := 1; i < len(route); i++ {
		ax, ay := float64(route[i-1][0]), float64(route[i-1][1])
		bx, by := float64(route[i][0]), float64(route[i][1])
		dx, dy := bx-ax, by-ay
		seg := dx*dx + dy*dy
		f := 0.0
		if seg > 0 {
			f = ((float64(p[0])-ax)*dx + (float64(p[1])-ay)*dy) / seg
			if f < 0 {
				f = 0
			} else if f > 1 {
				f = 1
			}
		}
		cx, cy := ax+dx*f, ay+dy*f
		if d := math.Hypot(float64(p[0])-cx, float64(p[1])-cy); d < best {
			best = d
		}
	}
	return best
}

// TestSecondLegStartsAtTheBodysLivePoint is the rule itself, and it is the first of the
// two assertions the old rule is replaced by.
//
// REPLACED: TestSecondLegStartsAtTheFirstLegsDeclaredDestination, which asserted that leg
// 2 began where the CLIENT said leg 1 ended "even when our model is nowhere near it".
// That was the whole defect: the model was nowhere near the body.
//
// WHAT IS ASSERTED NOW: the body has walked 50 of leg 1's 1000 units when the second
// report arrives, and it names a destination 900 units from where the body stands. Leg 2
// must begin on the BODY - 50 units from where leg 1 began, and 950 units from the
// destination leg 1 named - and it must end exactly where the second report pointed.
func TestSecondLegStartsAtTheBodysLivePoint(t *testing.T) {
	stand := [3]int32{1000, 1000, 1}
	h, sess, logs := legZone(t, []uint32{0x40000031}, [][3]int32{stand})
	spd := h.packetSpeed(&sess.Team[0])

	firstDest := [3]int32{2000, 1000, 1} // 1000 units east, 2.13 s at 469
	driftMove(t, h, sess, 0, firstDest)
	if sess.moveFrom != stand {
		t.Fatalf("leg 1 starts at %v, want the standing place %v", sess.moveFrom, stand)
	}

	// 50 of its 1000 units walked: the model's own clock, wound back by the time the
	// body needs for them. The body is on (1050, 1000) - 5 % of the leg.
	sess.moveAt = sess.moveAt.Add(-time.Duration(50 / spd * float64(time.Second)))
	live := sess.drawnPos(0, time.Now(), spd)
	if flatDist(live, [3]int32{1050, 1000, 1}) > 2 {
		t.Fatalf("the body reads %v, want (1050, 1000) - 50 units along leg 1: this "+
			"test's own model is not where it thinks it is", live)
	}

	// The continuation: 900 units NORTH of the body.
	secondDest := [3]int32{live[0], live[1] + 900, live[2]}
	driftMove(t, h, sess, 0, secondDest)

	if d := flatDist(sess.moveFrom, live); d > 2 {
		t.Fatalf("leg 2 starts at %v, %.0f units from the body's live point %v: a leg "+
			"starts where OUR OWN BODY IS, never at a point inferred from the report",
			sess.moveFrom, d, live)
	}
	if d := flatDist(sess.moveFrom, stand); d < 48 || d > 52 {
		t.Fatalf("leg 2 starts %.0f units from where leg 1 started, want the 50 the body "+
			"has walked", d)
	}
	if d := flatDist(sess.moveFrom, firstDest); d < 900 {
		t.Fatalf("leg 2 starts %v, only %.0f units from the destination leg 1 declared "+
			"(%v): that is the old declared-point rule, and it is what put the model 963 "+
			"units from the body on the owner's log", sess.moveFrom, d, firstDest)
	}
	if sess.Pos != secondDest {
		t.Fatalf("leg 2 ends at %v, want the destination the report named %v",
			sess.Pos, secondDest)
	}
	if d := flatDist(sess.moveFrom, sess.Pos); math.Abs(d-900) > 1 {
		t.Fatalf("leg 2 is %.0f units long, want the 900 the report named", d)
	}
	// AND THE LOG NAMES THE BODY, not an offset from a guess.
	bodyRe := regexp.MustCompile(`MOVE_PC char 0 -> \(\d+, \d+\) cell \d+ \| body was at \((\d+), (\d+)\)`)
	all := bodyRe.FindAllStringSubmatch(logs.String(), -1)
	if len(all) != 2 {
		t.Fatalf("want two move lines, got %d. Log:\n%s", len(all), logs.String())
	}
	bx, _ := strconv.Atoi(all[1][1])
	by, _ := strconv.Atoi(all[1][2])
	if d := flatDist([3]int32{int32(bx), int32(by), 0}, live); d > 2 {
		t.Fatalf("the move line says the body was at (%d, %d), %.0f units from the point "+
			"the model had drawn (%v). Log:\n%s", bx, by, d, live, logs.String())
	}
	if strings.Contains(logs.String(), " u off") {
		t.Fatalf("the move line still prints an offset; a leg that starts on the body has "+
			"none to print. Log:\n%s", logs.String())
	}
}

// TestTheLegIsARouteFromTheBodyToTheDestination is the second half of the rule, and the
// first of the two assertions the old chord is replaced by.
//
// REPLACED: nothing named the chord for a CLIENT's leg - it was simply never planned.
// The chase's walks got a route at R110; these never did, so a walk the client bent
// around a wall was DRAWN across the wall.
//
// WHAT IS ASSERTED NOW, with a planner that has to route around a wall between the body
// and the destination: (a) the leg starts on the body, (b) a sampled position lies ON
// the planned polyline, and (c) the leg ends exactly at the destination the client named.
func TestTheLegIsARouteFromTheBodyToTheDestination(t *testing.T) {
	stand := [3]int32{1000, 1000, 1}
	h, sess, logs := legZone(t, []uint32{0x40000031}, [][3]int32{stand})

	// A wall between (1000,1000) and (1600,1600): the walk goes north first, then east.
	corner := [3]int32{1000, 1600, 1}
	dest := [3]int32{1600, 1600, 1}
	planned := [][3]int32{stand, corner, dest}
	calls := 0
	h.Route = func(from, to [3]int32) (route [][3]int32, ok bool) {
		calls++
		if from != stand || to != dest {
			t.Errorf("the planner was asked for %v -> %v, want the body %v -> the report's "+
				"destination %v", from, to, stand, dest)
		}
		return planned, true
	}

	spd := h.packetSpeed(&sess.Team[0])
	driftMove(t, h, sess, 0, dest)

	// PLANNED ONCE, for this one leg.
	if calls != 1 {
		t.Fatalf("the planner ran %d times for one leg, want once - a route is planned "+
			"where the leg opens and never per tick", calls)
	}
	// (a) THE LEG STARTS ON THE BODY.
	if sess.moveFrom != stand {
		t.Fatalf("leg start = %v, want the body's own point %v", sess.moveFrom, stand)
	}
	if !legRouteMatches(sess.moveRoute, stand, dest) {
		t.Fatalf("the leg's route is %v, which is not a route from %v to %v",
			sess.moveRoute, stand, dest)
	}
	wantLen := flatDist(stand, corner) + flatDist(corner, dest) // 1200
	if got := legLength(stand, dest, sess.moveRoute); math.Abs(got-wantLen) > 1 {
		t.Fatalf("the leg measures %.0f units, want the route's %.0f", got, wantLen)
	}

	// (b) A SAMPLED POSITION LIES ON THE PLANNED POLYLINE. Sampled a millisecond past the
	// corner's own distance - the model stores whole world units, so the exact instant is
	// half a unit short - the answer must BE the corner: on the polyline, distance 0.
	at := sess.moveAt
	cornerLen := flatDist(stand, corner)
	onCorner := sess.drawnPos(0, at.Add(
		time.Duration(cornerLen/spd*float64(time.Second))+time.Millisecond), spd)
	if onCorner != corner {
		t.Fatalf("at the corner's distance the body reads %v, want the corner %v",
			onCorner, corner)
	}
	if d := distanceToPolyline(onCorner, sess.moveRoute); d != 0 {
		t.Fatalf("the body at the corner reads %v, %.1f units off the planned route %v",
			onCorner, d, sess.moveRoute)
	}
	// Half way up the first side, the body is on that side (allow a unit: the model stores
	// whole world units).
	mid := sess.drawnPos(0, at.Add(time.Duration(cornerLen/2/spd*float64(time.Second))), spd)
	if d := distanceToPolyline(mid, sess.moveRoute); d > 1.5 {
		t.Fatalf("half way along the first side the body reads %v, %.1f units off the "+
			"planned route", mid, d)
	}
	// AND THE CHORD WOULD HAVE BEEN NOWHERE NEAR IT. This is the point of the test: the
	// old code drew the chord, and its midpoint is 300 units from the route this leg walks.
	chordMid := [3]int32{(stand[0] + dest[0]) / 2, (stand[1] + dest[1]) / 2, dest[2]}
	if d := distanceToPolyline(chordMid, sess.moveRoute); d < 250 {
		t.Fatalf("the chord's midpoint is only %.0f units off the route, so this test "+
			"could not tell a chord from the route", d)
	}

	// (c) THE LEG ENDS EXACTLY AT THE DESTINATION THE CLIENT NAMED.
	if late := sess.drawnPos(0, at.Add(30*time.Second), spd); late != dest {
		t.Fatalf("a walked-out leg reads %v, want the destination %v", late, dest)
	}
	// AND THE LOG GIVES BOTH LENGTHS, so a route that was not planned is visible from a
	// log: 1200 u walked along the route, 600 u straight.
	if !strings.Contains(logs.String(), "leg 1200 u (straight 849 u)") {
		t.Fatalf("the move line does not carry the route's length beside the straight "+
			"one. Log:\n%s", logs.String())
	}
}

// TestDrawnPosNeverLeavesTheLeg pins the clamp: whatever instant is asked for, the
// answer is on the open leg and no further. Past the leg's own duration it is the
// endpoint - never a point beyond it - and before the leg opened it is the start. A leg
// with no route is the straight one, which is every leg on a machine with no path engine.
func TestDrawnPosNeverLeavesTheLeg(t *testing.T) {
	stand := [3]int32{2500, 1700, 3}
	h, sess, _ := legZone(t, []uint32{0x40000031}, [][3]int32{stand})
	dest := [3]int32{3100, 2200, 3} // 781 units, 1.67 s at 469
	driftMove(t, h, sess, 0, dest)

	spd := h.packetSpeed(&sess.Team[0])
	at := sess.moveAt
	legLen := flatDist(stand, dest)

	if got := sess.drawnPos(0, at.Add(30*time.Second), spd); got != dest {
		t.Fatalf("30 s into a 1.67 s leg the body reads %v, want exactly the endpoint %v",
			got, dest)
	}
	if got := sess.drawnPos(0, at.Add(-5*time.Second), spd); got != stand {
		t.Fatalf("before its leg opened the body reads %v, want the leg's start %v",
			got, stand)
	}
	for ms := -1000; ms <= 3000; ms += 25 {
		p := sess.drawnPos(0, at.Add(time.Duration(ms)*time.Millisecond), spd)
		if outside := flatDist(p, stand) + flatDist(p, dest); outside > legLen+1 {
			t.Fatalf("%d ms into the leg the body reads %v, which is %.0f units off the "+
				"leg (its own ends sum to %.0f, the leg is %.0f): a drawn position may "+
				"never leave the leg", ms, p, outside-legLen, outside, legLen)
		}
	}
}

// TestMoveLogLineNamesTheBodyAndTheLeg is the instrument. One line per report, and the
// four numbers that are now checkable: the destination, where the body was, the length of
// the leg it walks (its route) and the same distance straight.
//
// REPLACED: TestMoveLogLineCarriesTheDrift and TestMoveLogLineCarriesTheCorrection, whose
// `u off`, `start from <reason>`, `correction <note> at <rate>: corrected/deferred` are
// gone with the machinery behind them.
func TestMoveLogLineNamesTheBodyAndTheLeg(t *testing.T) {
	stand := [3]int32{2500, 1700, 1}
	h, sess, logs := legZone(t, []uint32{0x40000031}, [][3]int32{stand})

	// Leg 1 is 500 units; the model is wound 300 ms along it, so the body is ~141 units
	// past the standing place when the next report arrives.
	driftMove(t, h, sess, 0, [3]int32{3000, 1700, 1})
	sess.moveAt = sess.moveAt.Add(-300 * time.Millisecond)
	logs.Reset()

	driftMove(t, h, sess, 0, [3]int32{3500, 1700, 1})
	line := lastMoveLine(t, logs)

	if !strings.Contains(line, "MOVE_PC char 0 -> (3500, 1700) cell 1") {
		t.Fatalf("the line does not carry the destination: %q", line)
	}
	re := regexp.MustCompile(`body was at \((\d+), (\d+)\), leg (\d+) u \(straight (\d+) u\), ` +
		`speed (\d+)`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the line does not carry the body's point, the leg's length and the "+
			"straight distance in the agreed shape: %q", line)
	}
	nums := make([]int, 5)
	for i := range nums {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			t.Fatalf("field %d of %q is not a number: %v", i+1, line, err)
		}
		nums[i] = v
	}
	bx, by, leg, straight, speed := nums[0], nums[1], nums[2], nums[3], nums[4]
	if bx < 2638 || bx > 2642 || by != 1700 {
		t.Fatalf("the body's own point reads (%d, %d), want (2640, 1700) - 300 ms of a "+
			"469 u/s leg past the standing place. Line: %q", bx, by, line)
	}
	if leg < 858 || leg > 862 {
		t.Fatalf("the leg reads %d units, want the ~860 left from the body (%d) to 3500. "+
			"Line: %q", leg, bx, line)
	}
	if straight != leg {
		t.Fatalf("with no planner wired the straight distance (%d) must BE the leg's "+
			"length (%d): they differ, so the line is describing two legs. Line: %q",
			straight, leg, line)
	}
	if speed != int(MoveBaseSpeed) {
		t.Fatalf("the speed reads %d, want %v. Line: %q", speed, MoveBaseSpeed, line)
	}
}

// lastMoveLine is the LAST move line in the log buffer - the one this report wrote. The
// log accumulates, and every test here cares about the report it just made.
func lastMoveLine(t *testing.T, logs *bytes.Buffer) string {
	t.Helper()
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "MOVE_PC char 0 ->") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no move line was written. Log:\n%s", logs.String())
	}
	return line
}

// TestTenLegsAtThePacketSpeedStayOnTheBody is the property that replaces
// TestTenLegsWithTenPercentSpeedErrorStayWithinOneLeg.
//
// THE OLD TEST ASSUMED THE OLD RULE, and it has to be said plainly: it made the body walk
// 10 % faster than the speed this side had, and relied on each leg starting at a point
// the CLIENT declared to keep the error from adding up. Without that anchor a wrong speed
// simply accumulates - so this is NOT a property the new rule can have, and pretending
// otherwise would be a test of the deleted code.
//
// WHAT IS TRUE NOW, and what this asserts: the client animates at the speed the server
// PUT IN THE MOVE (that is what packetSpeed is - the character's own MSPD, R92), so ten
// legs of 600 units, each walked for exactly its own time, land the model on the body at
// every handover. The leg start is the body's own point, so nothing is carried forward but
// the body.
//
// ONE UNIT OF ROUNDING IS THE ONLY THING THAT ACCUMULATES, and it is the model's own
// arithmetic, not a rule: the drawn point is stored in whole world units, so a leg that
// ends at 599.99 is remembered as 599 and the next leg starts from 599. The clock is not
// affected (the leg's own start and destination are exact), so this is at most one unit
// per handover - which the bound below states.
func TestTenLegsAtThePacketSpeedStayOnTheBody(t *testing.T) {
	stand := [3]int32{0, 0, 1}
	h, sess, _ := legZone(t, []uint32{0x40000031}, [][3]int32{stand})

	const legLen = 600
	spd := h.packetSpeed(&sess.Team[0]) // the speed the client was TOLD, 469
	arrival := time.Duration(float64(legLen) / spd * float64(time.Second))

	body := stand // the point the model has drawn the body on, leg by leg
	for leg := 1; leg <= 10; leg++ {
		dest := [3]int32{stand[0] + int32(legLen*leg), 0, 1}
		driftMove(t, h, sess, 0, dest)
		// THE LEG STARTS ON THE BODY, which is the point the previous leg ended on.
		if d := flatDist(sess.moveFrom, body); d > 1 {
			t.Fatalf("leg %d starts at %v, %.0f units from the body %v: the legs no "+
				"longer chain through the body", leg, sess.moveFrom, d, body)
		}
		// The body has had exactly the time this leg needs.
		sess.moveAt = sess.moveAt.Add(-arrival)
		body = sess.drawnPos(0, time.Now(), spd)
		if off := flatDist(body, dest); off > float64(leg) {
			t.Fatalf("leg %d: the body reads %v, %.0f units from the destination %v it "+
				"had exactly the time to reach - more than the one unit of rounding per "+
				"handover this model can lose", leg, body, off, dest)
		}
	}
}

// TestNoRouteWalksTheStraightChord pins the fallback: a planner that answers "no route"
// (or no planner at all: a machine with no path engine, the Linux target) leaves the leg
// exactly as it was - the straight line from the body to the destination - and says so in
// the log with the two lengths equal. A missing engine must cost nothing but the bend.
func TestNoRouteWalksTheStraightChord(t *testing.T) {
	stand := [3]int32{1000, 1000, 1}
	h, sess, logs := legZone(t, []uint32{0x40000031}, [][3]int32{stand})
	asked := 0
	h.Route = func(from, to [3]int32) (route [][3]int32, ok bool) {
		asked++
		return nil, false // the engine has no route between these two points
	}

	dest := [3]int32{1600, 1000, 1}
	driftMove(t, h, sess, 0, dest)

	if asked != 1 {
		t.Fatalf("the planner was asked %d times, want once", asked)
	}
	if sess.moveRoute != nil {
		t.Fatalf("a refused route was kept (%v): the leg must walk the straight line",
			sess.moveRoute)
	}
	spd := h.packetSpeed(&sess.Team[0])
	if got := sess.drawnPos(0, sess.moveAt.Add(
		time.Duration(300/spd*float64(time.Second))), spd); math.Abs(flatDist(got, stand)-300) > 1 {
		t.Fatalf("300 units into a 600-unit straight leg the body reads %v, %.0f units "+
			"from the start: it is not walking the chord", got, flatDist(got, stand))
	}
	if !strings.Contains(logs.String(), "leg 600 u (straight 600 u)") {
		t.Fatalf("the move line does not show a straight leg. Log:\n%s", logs.String())
	}
}
