package zone

// -diag-distance's tests (zone\diag_distance.go) and -sightoff's (zone\sight.go,
// R135c). Both switches are DEVELOPMENT ONLY and default off, so every test here is
// about the BOUNDARY: what they must not change when off, and the exact shape of the
// one thing they add when on.
//
// WHAT A UNIT TEST CANNOT PROVE: at what distance the real client stops drawing a
// character. That number exists only on a screen, and the switch measures it - these
// tests only pin that the measurement is honest and that the wire is untouched.

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// distFixtures is the same tiny zone every test here builds: A is the viewer at the
// origin, B stands `bx` units east, one character each, leader handles 101 and 202.
func distZone(t *testing.T, sightOff, diagDistance bool, bx int32) (*Handler, *bytes.Buffer, *Session, *Session) {
	t.Helper()
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), MapID: 10, MapName: "reb_basic",
		SightOff: sightOff, DiagDistance: diagDistance}
	a := sightSession(101, [3]int32{0, 0, 0})
	b := sightSession(202, [3]int32{bx, 0, 0})
	h.Players.Add(a)
	h.Players.Add(b)
	return h, &buf, a, b
}

// TestDiagSwitchesOffLeaveTheRingWireIdentical is pass condition 1: with SightOff
// false and DiagDistance false the ring is what it was, byte for byte - and turning
// the DISTANCE switch on changes no byte of it either, because it is a log line and
// nothing else.
//
// "TODAY'S" HERE MEANS THE ZERO VALUE OF BOTH FIELDS, which is what every existing
// test and every real run builds. The two runs are driven through the same steps:
// first sight, walk out, walk back.
func TestDiagSwitchesOffLeaveTheRingWireIdentical(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})

	drive := func(h *Handler, a, b *Session) [][][]byte {
		var all [][][]byte
		all = append(all, h.sightTick(a))
		h.Players.SetPos(b, 0, [3]int32{9000, 0, 0})
		all = append(all, h.sightTick(a))
		h.Players.SetPos(b, 0, [3]int32{100, 0, 0})
		all = append(all, h.sightTick(a))
		return all
	}
	flat := func(rounds [][][]byte) [][]byte {
		var out [][]byte
		for _, r := range rounds {
			out = append(out, r...)
		}
		return out
	}

	// Today's handler: neither field set.
	plain, plainBuf, pa, pb := distZone(t, false, false, 100)
	todays := flat(drive(plain, pa, pb))

	// The same run with both switches explicitly OFF.
	off, _, oa, ob := distZone(t, false, false, 100)
	explicitOff := flat(drive(off, oa, ob))

	if len(todays) == 0 {
		t.Fatal("the ring sent nothing with both switches off - the scenario is wrong")
	}
	if len(explicitOff) != len(todays) {
		t.Fatalf("SightOff=false, DiagDistance=false sent %d packet(s), want today's %d",
			len(explicitOff), len(todays))
	}
	for i := range todays {
		if !bytes.Equal(todays[i], explicitOff[i]) {
			t.Fatalf("packet %d differs when both switches are explicitly false: "+
				"% x vs today's % x", i, explicitOff[i], todays[i])
		}
	}
	if strings.Contains(plainBuf.String(), "DIST") {
		t.Fatalf("a DIST line was written with DiagDistance off:\n%s", plainBuf.String())
	}

	// ...and the DISTANCE switch alone changes no byte: it is log-only.
	meas, measBuf, ma, mb := distZone(t, false, true, 100)
	withDist := flat(drive(meas, ma, mb))
	if len(withDist) != len(todays) {
		t.Fatalf("DiagDistance=true sent %d packet(s), want the same %d as today",
			len(withDist), len(todays))
	}
	for i := range todays {
		if !bytes.Equal(todays[i], withDist[i]) {
			t.Fatalf("packet %d differs with DiagDistance on: % x vs today's % x - "+
				"the switch is a log line and must not touch the wire", i, withDist[i], todays[i])
		}
	}
	// It did write the record, though: the pair line is built beside the same tick
	// (tickIfDue calls this after sightTick), and it names both leaders and the radius.
	meas.diagDistanceTick(ma, time.Now())
	if !strings.Contains(measBuf.String(), "zone: DIST 101 (0, 0) <-> 202 (100, 0) = 100 units") {
		t.Fatalf("DiagDistance on wrote no pair line:\n%s", measBuf.String())
	}
}

// TestSightOffRingIsInertAndJoinZoneAnnounces is pass condition 2: with the ring
// switched off a session 9,000 units away produces NOTHING from sightTick - no enter,
// no leave, ever - while joinZone still announces that far player, which is exactly
// the behaviour every map had before R135.
//
// THE MAP HAS A ROW ON PURPOSE (ViewRange 4000). Without one, "no enter and no leave"
// would prove nothing: the ring is already unarmed on a map with no data. This is the
// case where the ring WOULD cull, and the switch must stop it.
func TestSightOffRingIsInertAndJoinZoneAnnounces(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h, _, a, b := distZone(t, true, false, 9000)

	// The radius still exists in the table - and the ring must still not arm.
	if h.sightArmed() {
		t.Fatal("SightOff=true but sightArmed answered true - the ring would still cull")
	}
	if r, armed := h.sightRadius(); armed || r != 0 {
		t.Fatalf("SightOff=true but sightRadius answered (%d, armed=%v), want (0, false)", r, armed)
	}
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("SightOff=true, a player 9,000 units away produced %d packet(s) %v - "+
			"the ring must answer nil", len(got), sightCmds(got))
	}
	if got := h.sightTick(b); len(got) != 0 {
		t.Fatalf("SightOff=true, the far session's own tick produced %d packet(s) %v",
			len(got), sightCmds(got))
	}

	// Moving the far player to the far side of the map must not produce a LEAVE on
	// anybody's tick: with the ring off nothing is ever left by range.
	h.Players.SetPos(b, 0, [3]int32{60000, 0, 0})
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("SightOff=true, a player walking 60,000 units away produced %d packet(s) %v - "+
			"no leave may come from range", len(got), sightCmds(got))
	}

	// THE PRE-R135 HALF: a newcomer is still told about everyone, and everyone is
	// still told about the newcomer.
	var joinBuf bytes.Buffer
	fresh := &Handler{Log: log.New(&joinBuf, "", 0), MapID: 10, MapName: "reb_basic", SightOff: true}
	fa := sightSession(101, [3]int32{0, 0, 0})
	fb := sightSession(303, [3]int32{90000, 0, 0})
	fresh.joinZone(fa)
	burst := fresh.joinZone(fb)
	if n := countCmd(burst, CmdZCSpawnStandingPC); n != 1 {
		t.Fatalf("SightOff=true: a player 9,000 units away was announced %d time(s) in the "+
			"join burst %v - the ring is off, so the newcomer must hear about EVERYBODY",
			n, sightCmds(burst))
	}
	// ...and the session already there must hear about the newcomer. These fixture
	// sessions have no socket, so the write itself fails - what the pre-R135 path
	// proves here is that the fan-out WENT to handle 101 at 90,000 units: the ring is
	// off, so the distance plays no part in who is told.
	if !strings.Contains(joinBuf.String(), "announced 1 character(s) of handle 303 to handle 101") {
		t.Fatalf("SightOff=true: the player already in the zone was NOT told about the "+
			"newcomer 90,000 units away:\n%s", joinBuf.String())
	}
}

// TestFormatDistLineIsTheExactShape is pass condition 3a: the line has the shape the
// lane asked for, character for character, and it names the radius - or says the ring
// is off.
//
// It is a test of the FORMATTER, not of time: the shape is the deliverable, so a
// clock is the wrong thing to measure.
func TestFormatDistLineIsTheExactShape(t *testing.T) {
	got := formatDistLine(101, [3]int32{0, 0, 3}, 202, [3]int32{3000, 4000, 7}, 5000, 6000, true)
	want := "zone: DIST 101 (0, 0) <-> 202 (3000, 4000) = 5000 units [ring 6000]"
	if got != want {
		t.Fatalf("line =\n  %q\nwant\n  %q", got, want)
	}
	// The ring not armed says so, and names no number that could be mistaken for one.
	off := formatDistLine(101, [3]int32{0, 0, 0}, 202, [3]int32{9000, 0, 0}, 9000, 0, false)
	wantOff := "zone: DIST 101 (0, 0) <-> 202 (9000, 0) = 9000 units [ring off]"
	if off != wantOff {
		t.Fatalf("line =\n  %q\nwant\n  %q", off, wantOff)
	}
	// A negative coordinate keeps its sign - the maps have negative y.
	neg := formatDistLine(1, [3]int32{-100, -200, 0}, 2, [3]int32{0, 0, 0}, 224, 6000, true)
	if !strings.Contains(neg, "(-100, -200) <-> 2 (0, 0)") {
		t.Fatalf("negative coordinates were not printed as they are: %q", neg)
	}
}

// TestXYDistanceRoundsToNearestUnit pins the number itself: straight X/Y, and ROUNDED,
// not truncated - a distance of 2.83 units is 3, and the ring's own number (a
// float square comparison) must not disagree with the printed one by a unit.
func TestXYDistanceRoundsToNearestUnit(t *testing.T) {
	cases := []struct {
		a, b [3]int32
		want int64
	}{
		{[3]int32{0, 0, 0}, [3]int32{3000, 4000, 0}, 5000}, // 3-4-5, exact
		{[3]int32{0, 0, 0}, [3]int32{1, 1, 0}, 1},         // 1.41 -> 1
		{[3]int32{0, 0, 0}, [3]int32{2, 2, 0}, 3},         // 2.83 -> 3 (rounded UP)
		{[3]int32{0, 0, 0}, [3]int32{0, 0, 9999}, 0},      // z is not part of it
		{[3]int32{-100, -100, 0}, [3]int32{100, 100, 0}, 283},
	}
	for _, c := range cases {
		if got := xyDistance(c.a, c.b); got != c.want {
			t.Errorf("distance %v -> %v = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestDiagDistanceOneLinePerPairPerSecond is pass condition 3b end to end: one line
// per pair, from ONE side of it, once a second - and the line names the radius.
//
// THE SECOND PART IS THE ONE THAT MATTERS. Both sessions tick; if each wrote the pair
// the record would hold two lines a second for the same distance, and a reader
// counting them would double every count.
func TestDiagDistanceOneLinePerPairPerSecond(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h, buf, a, b := distZone(t, false, true, 100)
	t0 := time.Now()

	h.diagDistanceTick(a, t0)
	if n := strings.Count(buf.String(), "zone: DIST 101 (0, 0) <-> 202 (100, 0) = 100 units [ring 4000]"); n != 1 {
		t.Fatalf("one tick wrote %d pair line(s), want exactly 1:\n%s", n, buf.String())
	}

	// 100 ms later - a real tick - the same pair is NOT written again.
	buf.Reset()
	h.diagDistanceTick(a, t0.Add(100*time.Millisecond))
	if buf.Len() != 0 {
		t.Fatalf("a second tick inside the same second wrote again:\n%s", buf.String())
	}

	// A second later it is due again, and the OTHER side of the pair stays silent.
	h.diagDistanceTick(a, t0.Add(time.Second))
	if n := strings.Count(buf.String(), "DIST 101"); n != 1 {
		t.Fatalf("one second later wrote %d line(s) starting with 101, want 1:\n%s", n, buf.String())
	}
	buf.Reset()
	h.diagDistanceTick(b, t0.Add(time.Second))
	if buf.Len() != 0 {
		t.Fatalf("the higher-handle session wrote the same pair a second time - the log "+
			"must not double:\n%s", buf.String())
	}

	// The ring off with the map's row missing says so on every line. The radius is
	// cached per process, so the table is re-installed first (sightUseTable clears it).
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	var buf2 bytes.Buffer
	h2 := &Handler{Log: log.New(&buf2, "", 0), MapID: 99, DiagDistance: true}
	a2 := sightSession(101, [3]int32{0, 0, 0})
	b2 := sightSession(202, [3]int32{9000, 0, 0})
	h2.Players.Add(a2)
	h2.Players.Add(b2)
	h2.diagDistanceTick(a2, t0)
	if !strings.Contains(buf2.String(), "[ring off]") {
		t.Fatalf("a zone whose ring is not armed did not print `[ring off]`:\n%s", buf2.String())
	}
}

// TestStopSuffixCarriesTheDistance is the second half of pass condition 3: the stop
// line gains `| DIST to <handle> = <N> units` when the switch is on, and the line is
// byte for byte today's when it is off.
func TestStopSuffixCarriesTheDistance(t *testing.T) {
	h, _, a, _ := distZone(t, false, true, 250)
	pos := [3]int32{0, 0, 0}
	if got, want := h.stopDistSuffix(a, pos), " | DIST to 202 = 250 units"; got != want {
		t.Fatalf("suffix = %q, want %q", got, want)
	}
	// The NEAREST other player, when there is more than one.
	far := sightSession(909, [3]int32{0, 8000, 0})
	h.Players.Add(far)
	if got, want := h.stopDistSuffix(a, pos), " | DIST to 202 = 250 units"; got != want {
		t.Fatalf("suffix with two neighbours = %q, want the nearest %q", got, want)
	}
	// OFF is OFF: no suffix, and the stop line is today's text again.
	h.DiagDistance = false
	if got := h.stopDistSuffix(a, pos); got != "" {
		t.Fatalf("DiagDistance=false still produced the suffix %q", got)
	}
	// Alone in the zone there is nobody to measure against.
	empty := &Handler{Log: log.New(io.Discard, "", 0), DiagDistance: true}
	solo := sightSession(101, [3]int32{0, 0, 0})
	empty.Players.Add(solo)
	if got := empty.stopDistSuffix(solo, pos); got != "" {
		t.Fatalf("a session alone in the zone produced the suffix %q", got)
	}
}
