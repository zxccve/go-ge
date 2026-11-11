package zone

// THE INNER RADIUS: AN ACTOR PAST ViewMiddleRange IS FROZEN, NOT GONE.
//
// The spec is docker-port\research\Q6-view-middle-range.md, and it is the reference's own
// code: CView::Update visits inside m_viewRange (`View.cpp:139`), so an actor past 6,000 on
// map 1331 is LEFT with reason 3; inside CView::Broadcast() a second check
// (`int cullDistance = m_viewMiddleRange;`, `View.cpp:269`) suppresses the broadcasts of an
// actor past 4,000. Between the two radii the actor stays in the client's world and stops
// being updated - and NOTHING is sent to say so.
//
// WHAT THESE TESTS PIN, in the order the brief asked for them:
//
//	1. an actor at 4,500 from the viewer: no move packet, no 3525;
//	2. an actor at 3,500: the 3548 arrives BYTE-IDENTICAL to the bytes the fan-out built;
//	3. a body crossing 6,000: the leave still fires, once, with reason 3;
//	4. the measurement is the BODY, at both ends - R135d's trap, one radius in;
//	5. the METRICS line carries cull=<n> and the number is the real suppressions.
//
// THE THIRD THING THEY PIN is what is NOT touched: the entry burst, the re-enter frames,
// the leave's reason, and every map without a middle range all behave exactly as before -
// a gate that swallowed those would be a server that looks broken, not a freeze.
//
// THE BODY IS MODELLED BY THE SAME HELPERS R135d'S TESTS USE (reportWalk, sight_body_test.go)
// and the distance is read the way the server reads it (sightCentre / cullActorAt, one
// clock per fan-out). A test that set `Pos` and expected a distance would be testing the
// value fact #231 says is a DESTINATION, not a body.

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// cullZoneRow is the two-player zone every case here starts from: a viewer and an actor,
// both ANNOUNCED to each other (the join burst), both sockets cleared, so an assertion is
// about the cull and not about the entry packets.
//
// `row` is the fixture's map_table row - test data, not a second source of the real
// numbers: 1331's own row in data\map_table.xml is ViewRange 6000 / ViewMiddleRange 4000,
// which is why cullZone uses those two.
func cullZoneRow(t *testing.T, mapID int, row SightRow, viewerPos, actorPos [3]int32) (*Handler, *player, *player) {
	t.Helper()
	sightUseTable(t, map[int]SightRow{mapID: row})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: mapID, MapName: "fld_auch"}
	a := newPlayer(0x40000101, "Viewer", viewerPos)
	b := newPlayer(0x40000202, "Actor", actorPos)
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil
	return h, a, b
}

// cullZone is cullZoneRow on map 1331 - the map the brief's 4,500 / 3,500 / 6,000 are on.
func cullZone(t *testing.T, viewerPos, actorPos [3]int32) (*Handler, *player, *player) {
	t.Helper()
	return cullZoneRow(t, 1331, SightRow{ViewRange: 6000, ViewMiddleRange: 4000}, viewerPos, actorPos)
}

// cullPlace puts the actor's BODY at pos with NO leg (a placed body: the stored point is
// where it is drawn). It is the only way to pin a comparison at an exact distance, because
// a walking body moves between the fan-out and the reading.
func cullPlace(h *Handler, b *player, pos [3]int32) {
	b.sess.Pos = pos
	b.sess.moveFrom, b.sess.moveAt, b.sess.moveRoute = [3]int32{}, time.Time{}, nil
	b.sess.Team[0].Position = pos
	h.Players.SetPos(b.sess, 0, pos)
}

// cullFanOut runs ONE real 3548 about the actor through the real fan-out
// (broadcastMove -> sendOthers) and answers the bytes that were built once for every
// recipient, so a test can compare what arrived with what was sent.
func cullFanOut(h *Handler, b *player, pos [3]int32) []byte {
	handle := b.sess.Team[0].Handle
	pkt := BuildMovePC(handle, pos, 1, float32(MoveBaseSpeed))
	h.broadcastMove(b.sess, 0, handle, pos, pkt)
	return pkt
}

// TestSightCullStopsUpdatesPastTheMiddleRange is pass conditions 1 and 2 as one table: at
// 4,500 the viewer hears NOTHING (and no 3525 - the actor is not left), at 3,500 the 3548
// arrives byte-identical, and the boundary is the reference's own strict comparison - an
// actor EXACTLY ViewMiddleRange away is still updated, 4,001 is not.
func TestSightCullStopsUpdatesPastTheMiddleRange(t *testing.T) {
	cases := []struct {
		name   string
		actorX int32
		want   bool // does the 3548 reach the viewer?
	}{
		{name: "3500 is inside the middle range", actorX: 3500, want: true},
		{name: "exactly 4000 is still updated", actorX: 4000, want: true},
		{name: "4001 is past the middle", actorX: 4001, want: false},
		{name: "4500 is frozen", actorX: 4500, want: false},
		{name: "5999 is frozen at the rim", actorX: 5999, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos := [3]int32{c.actorX, 0, 0}
			h, a, b := cullZone(t, [3]int32{0, 0, 0}, pos)
			cullPlace(h, b, pos)

			pkt := cullFanOut(h, b, pos)
			sent := a.conn.sent()

			if !c.want {
				if len(sent) != 0 {
					t.Fatalf("the viewer's body is %d units from the actor, past the 4,000 "+
						"middle range, yet it received %d packet(s) %v - the freeze is missing",
						c.actorX, len(sent), sightCmds(sent))
				}
				return
			}
			if len(sent) != 1 {
				t.Fatalf("the viewer's body is %d units from the actor, inside the 4,000 "+
					"middle range, and it received %d packet(s) %v - want the one 3548",
					c.actorX, len(sent), sightCmds(sent))
			}
			if !bytes.Equal(sent[0], pkt) {
				t.Fatalf("the forwarded 3548 differs from the bytes the fan-out built:\n"+
					" got % x\nwant % x", sent[0], pkt)
			}
			if got := cmdOf(sent[0]); got != CmdZCMovePC {
				t.Fatalf("the viewer received cmd %d, want %d ZC_MOVE_PC", got, CmdZCMovePC)
			}
		})
	}
}

// TestSightCullKeepsTheActorFrozenNotGone is the other half of pass condition 1, and the
// half the owner's in-game card will look for: at 4,500 the actor is STILL IN THE VIEWER'S
// WORLD. Passes send nothing (no leave, no re-enter, no new spawn) while the body is frozen,
// and the updates resume by themselves - with no announcement of any kind - the moment the
// bodies are back inside the middle range. That is what "present and frozen" means on the
// wire, as opposed to "gone".
func TestSightCullKeepsTheActorFrozenNotGone(t *testing.T) {
	h, a, b := cullZone(t, [3]int32{0, 0, 0}, [3]int32{4500, 0, 0})
	cullPlace(h, b, [3]int32{4500, 0, 0})

	// THE RING KEEPS RUNNING over the frozen actor: it must say nothing at all. A 3525
	// would mean the ring had left it at ViewMiddleRange, which is the bug this lane is
	// here to not write.
	for i := 0; i < 3; i++ {
		if got := h.sightTick(a.sess); len(got) != 0 {
			t.Fatalf("ring pass %d over an actor 4,500 away sent %v - the actor stays "+
				"announced and nothing is left at the middle range", i, sightCmds(got))
		}
	}

	// A move while frozen: the viewer hears none of it.
	cullFanOut(h, b, [3]int32{4500, 0, 0})
	if sent := a.conn.sent(); len(sent) != 0 {
		t.Fatalf("a frozen actor's 3548 reached the viewer: %v", sightCmds(sent))
	}

	// THE BODIES COME BACK INSIDE: the update arrives, and NOT ONE announcement does -
	// there is nothing to re-enter, because nothing was ever left.
	cullPlace(h, b, [3]int32{3000, 0, 0})
	pkt := cullFanOut(h, b, [3]int32{3000, 0, 0})
	sent := a.conn.sent()
	if len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
		t.Fatalf("coming back inside the middle range sent %v, want exactly the one "+
			"unchanged 3548 %v", sightCmds(sent), sightCmds([][]byte{pkt}))
	}
	for _, q := range sent {
		switch cmdOf(q) {
		case CmdZCLeavePC, CmdZCReEnterPC, CmdZCReEnterMovingPC, CmdZCSpawnStandingPC:
			t.Fatalf("the returning update carried cmd %d - the actor was never left, so "+
				"there is nothing to leave or re-enter", cmdOf(q))
		}
	}
	if got := h.sightTick(a.sess); len(got) != 0 {
		t.Fatalf("the pass after the bodies came back inside sent %v, want nothing",
			sightCmds(got))
	}
}

// TestSightCullMeasuresTheBodyNotTheDestination is pass condition 4, the R135d trap at the
// INNER radius. The roster's Pos is the DESTINATION a CZ_MOVE_PC named (fact #231), so a
// cull that read it would keep sending updates to a viewer whose actor had already walked
// past the middle - or freeze a viewer the moment the actor clicked, while its body was
// still beside it. Both directions are here, and each one fails if the cull reads Pos.
func TestSightCullMeasuresTheBodyNotTheDestination(t *testing.T) {
	cases := []struct {
		name string
		from [3]int32 // where the BODY is when the update is sent
		dest [3]int32 // the DESTINATION the move named - what Pos holds
		want bool     // does the update reach the viewer?
	}{
		{
			// Pos says 3,000 (inside), the body is at 4,500 (past the middle): the
			// actor walked IN toward the viewer and has not arrived - its updates must
			// already be frozen, because the body the viewer draws is far away.
			name: "destination inside, body outside",
			from: [3]int32{4500, 0, 0}, dest: [3]int32{3000, 0, 0},
			want: false,
		},
		{
			// Pos says 9,100 (far), the body is at 3,000 (inside): the actor just
			// ordered a long walk - the viewer must still see it move.
			name: "destination far, body inside",
			from: [3]int32{3000, 0, 0}, dest: [3]int32{9100, 0, 0},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, a, b := cullZone(t, [3]int32{0, 0, 0}, c.from)
			reportWalk(h, b.sess, 0, c.from, c.dest, time.Now(), MoveBaseSpeed)
			// The roster really does hold the destination - the value a Pos-reading
			// cull would measure. Assert it, so this test cannot pass by accident.
			if b.sess.Pos != c.dest {
				t.Fatalf("setup: the roster holds %v, want the destination %v",
					b.sess.Pos, c.dest)
			}

			pkt := cullFanOut(h, b, c.dest)
			sent := a.conn.sent()
			if c.want {
				if len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
					t.Fatalf("the body is %d units away (inside 4,000) and the destination "+
						"is %d, yet the viewer got %v - the cull read the DESTINATION",
						c.from[0], c.dest[0], sightCmds(sent))
				}
				return
			}
			if len(sent) != 0 {
				t.Fatalf("the body is %d units away (past 4,000) and the destination is %d, "+
					"yet the viewer got %v - the cull read the DESTINATION, so a body that has "+
					"walked away is updated as if it stood beside it",
					c.from[0], c.dest[0], sightCmds(sent))
			}
		})
	}
}

// TestSightCullLeavesTheLeaveAlone is pass condition 3, the regression this slice must not
// break: an actor whose BODY crosses 6,000 still leaves, once, with reason 3 - the inner
// radius never owns a case the outer one does.
func TestSightCullLeavesTheLeaveAlone(t *testing.T) {
	h, a, b := cullZone(t, [3]int32{0, 0, 0}, [3]int32{3000, 0, 0})

	// B's body walks from 3,000 to 9,100; nine seconds in it has walked ~4,221 units and
	// is past the 6,000 ring while the leg is still running.
	reportWalk(h, b.sess, 0, [3]int32{3000, 0, 0}, [3]int32{9100, 0, 0}, time.Now(), MoveBaseSpeed)
	b.sess.moveAt = time.Now().Add(-9 * time.Second)

	out := h.sightTick(a.sess)
	leaves := 0
	for _, p := range out {
		if len(p) == 8 && binary.LittleEndian.Uint16(p) == CmdZCLeavePC {
			leaves++
			if reason := binary.LittleEndian.Uint16(p[6:]); reason != LeaveCodeOutOfSight {
				t.Fatalf("the range leave carried reason %d, want %d (OBJ_OUTOFSIGHT)",
					reason, LeaveCodeOutOfSight)
			}
			if got := binary.LittleEndian.Uint32(p[2:]); got != b.sess.Team[0].Handle {
				t.Fatalf("the range leave names handle %d, want %d", got, b.sess.Team[0].Handle)
			}
		}
	}
	if leaves != 1 {
		t.Fatalf("a body crossing 6,000 sent %d leave(s) %v, want exactly one 3525",
			leaves, sightCmds(out))
	}
}

// TestSightCullCoversTheAttackFamily is the brief's 3564/3566 half: the attack fan-out
// (zone\attack.go's h.fanOut) is sendOthers too, so a frozen actor's swing does not reach
// the far viewer - and a near one gets the caster's own bytes, unchanged.
func TestSightCullCoversTheAttackFamily(t *testing.T) {
	cases := []struct {
		name   string
		actorX int32
		want   bool
	}{
		{name: "3500 delivers the swing", actorX: 3500, want: true},
		{name: "4500 drops the swing", actorX: 4500, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos := [3]int32{c.actorX, 0, 0}
			h, a, b := cullZone(t, [3]int32{0, 0, 0}, pos)
			handle := b.sess.Team[0].Handle
			pkt := BuildAttackSingle(handle, pos, 100,
				uint32(0x9001), [3]int32{c.actorX + 50, 0, 0}, 10, 500, 0)
			if got := cmdOf(pkt); got != CmdZCAttackSingle {
				t.Fatalf("setup: built cmd %d, want the 3564", got)
			}

			h.fanOut(b.sess, handle, pkt)
			sent := a.conn.sent()
			if !c.want {
				if len(sent) != 0 {
					t.Fatalf("the actor is %d units away, past the 4,000 middle range, yet its "+
						"swing reached the viewer: %v", c.actorX, sightCmds(sent))
				}
				return
			}
			if len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
				t.Fatalf("the actor is %d units away, inside the middle range, and the viewer "+
					"got %v - want the caster's own 3564 bytes", c.actorX, sightCmds(sent))
			}
		})
	}
}

// TestSightCullIsOffWithoutAMiddleRange is the do-no-harm half, three ways. A row with no
// ViewMiddleRange column, a map with no row at all, and -sightoff must each leave the
// fan-out exactly as it was - a freeze nobody asked for is a server that looks broken.
func TestSightCullIsOffWithoutAMiddleRange(t *testing.T) {
	t.Run("row has no middle range", func(t *testing.T) {
		pos := [3]int32{4500, 0, 0}
		h, a, b := cullZoneRow(t, 1331, SightRow{ViewRange: 6000}, [3]int32{0, 0, 0}, pos)
		cullPlace(h, b, pos)
		pkt := cullFanOut(h, b, pos)
		if sent := a.conn.sent(); len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
			t.Fatalf("a map row without ViewMiddleRange sent %v, want today's one 3548",
				sightCmds(sent))
		}
	})

	t.Run("map has no row", func(t *testing.T) {
		pos := [3]int32{9000, 0, 0}
		h, a, b := cullZoneRow(t, 99, SightRow{ViewRange: 6000, ViewMiddleRange: 4000},
			[3]int32{0, 0, 0}, pos)
		cullPlace(h, b, pos)
		pkt := cullFanOut(h, b, pos)
		if sent := a.conn.sent(); len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
			t.Fatalf("a map with no table row sent %v, want today's one 3548 (nobody is "+
				"hidden because a table is missing)", sightCmds(sent))
		}
	})

	t.Run("sightoff", func(t *testing.T) {
		pos := [3]int32{4500, 0, 0}
		h, a, b := cullZone(t, [3]int32{0, 0, 0}, pos)
		h.SightOff = true
		cullPlace(h, b, pos)
		pkt := cullFanOut(h, b, pos)
		if sent := a.conn.sent(); len(sent) != 1 || !bytes.Equal(sent[0], pkt) {
			t.Fatalf("-sightoff sent %v, want today's one 3548 - the flag makes the whole "+
				"ring inert, the cull included", sightCmds(sent))
		}
	})
}

// TestMetricsLineCarriesTheCullCount is pass condition 5: the freeze has to be measurable
// in a run. The line carries cull=<n>, and the number is the REAL suppressions of the
// window - one per (viewer, packet) pair the inner radius dropped - not a field nothing
// writes.
func TestMetricsLineCarriesTheCullCount(t *testing.T) {
	if line := (Snapshot{PktsIn: 1, Cull: 7}).Line(10 * time.Second); !strings.Contains(line, "cull=7") {
		t.Fatalf("the METRICS line does not carry cull=7:\n got: %s", line)
	}

	pos := [3]int32{4500, 0, 0}
	h, a, b := cullZone(t, [3]int32{0, 0, 0}, pos)
	cullPlace(h, b, pos)

	metrics.take() // zero the window: other tests in this package write packets too
	// A window only renders at all once something arrived (Line answers "" for an idle
	// map), so a client frame is counted the way the zone counts one.
	metrics.packetIn(1)
	cullFanOut(h, b, pos)
	if got := len(a.conn.sent()); got != 0 {
		t.Fatalf("setup: the culled fan-out reached the viewer (%d packet(s))", got)
	}
	s := metrics.take()
	if s.Cull != 1 {
		t.Fatalf("one suppressed update recorded cull=%d, want 1", s.Cull)
	}
	if line := s.Line(10 * time.Second); !strings.Contains(line, "cull=1") {
		t.Fatalf("the window's METRICS line does not carry cull=1:\n got: %s", line)
	}
}
