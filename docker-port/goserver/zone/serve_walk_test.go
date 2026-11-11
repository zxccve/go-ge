package zone

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"geserver/protocol"
)

// The zone's own clock, driven through the REAL Serve loop over a REAL socket.
//
// These tests exist because the stop rule was already written, already unit-tested,
// and still never ran in game: Serve only sets a read deadline while a chase or a
// pending click is running, so a player who clicked the GROUND (no chase) left the
// loop in a blocking read and the zone tick - the only thing that sends the walk's
// ZC_STOP_PC - was never called. Measured in logs\zone10_live.log after the 20:53:52
// restart: 12 MOVE_PC reports for 6 characters and 0 stops.
//
// A TCP socket, not net.Pipe: net.Pipe is unbuffered, so Serve's write would block
// until the test read it, and the test would have to know every reply's exact size.
// A socket buffer lets the replies pile up while the test watches the CLOCK.

// tickWalkServer starts Serve on a loopback socket for one team of `team` characters
// standing at spawn, with one observer already in the zone whose socket is a
// recordConn. It answers the connected client connection and the table.
//
// `setup` runs ONCE, BEFORE the listener exists, so a test can hand the zone its own
// log buffer, its stance/monster tables and its fixture monsters without racing the
// goroutine Serve runs on (zone\wire_scenario_test.go is the caller that needs all
// four). It is variadic so the walk tests that need none of that are untouched.
func tickWalkServer(t *testing.T, spawn [3]int32, team []Character,
	setup ...func(*Handler)) (*Handler, *player, net.Conn, *protocol.Spec) {
	t.Helper()

	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}
	chars := team
	h := &Handler{
		Table:  table,
		Cipher: protocol.DefaultCipher,
		Log:    log.New(io.Discard, "", 0),
		Spawn:  spawn,
		TeamOf: func(_ context.Context, _ uint32) ([]Character, error) { return chars, nil },
	}
	for _, configure := range setup {
		configure(h)
	}

	// The observer joins FIRST: when the mover joins afterwards, its characters are
	// announced to the observer through a recordConn, which never blocks.
	obs := newPlayer(0x40000041, "Obs", [3]int32{9000, 9000, 1})
	h.joinZone(obs.sess)
	obs.conn.writes = nil

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	served := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			close(served)
			return
		}
		h.Serve(context.Background(), conn)
		close(served)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return h, obs, client, table
}

// logInAs drives the two packets a client sends to enter a zone: CZ_START_GAME and
// CZ_GAME_READY. The replies are left in the socket buffer - nothing here reads them.
func logInAs(t *testing.T, conn net.Conn, table *protocol.Spec) {
	t.Helper()
	start := make([]byte, 40)
	binary.LittleEndian.PutUint16(start, CmdCZStartGame)
	binary.LittleEndian.PutUint32(start[14:], 4242)
	copy(start[18:], "tina")
	sendWire(t, conn, start, table)

	ready := make([]byte, 13)
	binary.LittleEndian.PutUint16(ready, CmdCZGameReady)
	sendWire(t, conn, ready, table)

	// The join runs on Serve's goroutine; give it a moment to reach the roster before
	// the move is sent, so the observer is already known when the walk starts.
	time.Sleep(100 * time.Millisecond)
}

// drainStops reads whatever has arrived on the mover's own socket without blocking and
// counts the stops in it. A client's own socket must never carry a stop for its own
// walk, and this is the only way to say that about the real wire.
func drainStops(t *testing.T, conn net.Conn) int {
	t.Helper()
	n := 0
	buf := make([]byte, 4096)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatalf("deadline: %v", err)
		}
		k, err := conn.Read(buf)
		for i := 0; i+1 < k; i++ {
			if binary.LittleEndian.Uint16(buf[i:]) == CmdZCStopPC {
				n++
			}
		}
		if err != nil {
			return n
		}
	}
}

// TestGroundWalkStopsItselfWithoutAnotherClientPacket is the owner's own test, driven
// through the loop: a player clicks the ground, walks 300 units, and then the client
// sends NOTHING. The zone must notice the walk is over by its own clock and send
// exactly one 3549 to the other player - and none to the mover.
func TestGroundWalkStopsItselfWithoutAnotherClientPacket(t *testing.T) {
	spawn := [3]int32{1000, 1000, 1}
	h, obs, client, table := tickWalkServer(t, spawn, []Character{
		{Handle: 0x40000031, Name: "Jin", Job: 3, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10010, Position: spawn},
	})
	logInAs(t, client, table)

	dest := [3]int32{1000, 1300, 1} // 300 units: ~640 ms at 469 u/s
	move := moveTo(0, dest).Data
	sendWire(t, client, move, table)

	// STILL WALKING: a quarter of the way in, the other player must not have been
	// told to stop. If the rule fired early it would cut the animation short.
	time.Sleep(250 * time.Millisecond)
	if got := len(stopsOf(obs.conn.sent())); got != 0 {
		t.Fatalf("the walk is still running and %d stop(s) were sent, want 0", got)
	}

	// ARRIVED. Nothing else is sent by the client - that is the point. Give the loop
	// several ticks to notice the walk is walked out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(stopsOf(obs.conn.sent())) == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	stops := stopsOf(obs.conn.sent())
	if len(stops) != 1 {
		t.Fatalf("the finished walk sent %d stop(s) to the other player, want exactly 1 "+
			"(0 means the zone never ticked: Serve only wakes on a timer while a chase "+
			"or a pending click runs - see wantsTick)", len(stops))
	}
	p := stops[0]
	if len(p) != 22 {
		t.Fatalf("stop is %d bytes, want 22", len(p))
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != 0x40000031 {
		t.Fatalf("stop names handle %#x, want 0x40000031", got)
	}
	for i, want := range dest {
		if got := int32(binary.LittleEndian.Uint32(p[10+i*4:])); got != want {
			t.Fatalf("stop pos[%d] = %d, want %d", i, got, want)
		}
	}

	// One arrival, one stop: more ticks must not repeat it.
	time.Sleep(300 * time.Millisecond)
	if got := len(stopsOf(obs.conn.sent())); got != 1 {
		t.Fatalf("later ticks sent %d stop(s), want the same 1", got)
	}

	// The mover is never told to stop its own walk.
	if got := drainStops(t, client); got != 0 {
		t.Fatalf("the mover's own socket carried %d stop(s), want 0", got)
	}
	_ = h
}

// TestTeamWalkThroughTheLoopStopsEveryCharacter: three characters walk, the client goes
// silent, and the other player is owed one stop per character.
func TestTeamWalkThroughTheLoopStopsEveryCharacter(t *testing.T) {
	spawn := [3]int32{1000, 1000, 1}
	team := []Character{
		{Handle: 0x40000031, Name: "A", Job: 3, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10010, Position: spawn},
		{Handle: 0x40000032, Name: "B", Job: 4, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10020, Position: [3]int32{1200, 1000, 1}},
		{Handle: 0x40000033, Name: "C", Job: 5, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10030, Position: [3]int32{1400, 1000, 1}},
	}
	_, obs, client, table := tickWalkServer(t, spawn, team)
	logInAs(t, client, table)

	dests := [][3]int32{{1000, 1300, 1}, {1200, 1300, 1}, {1400, 1300, 1}}
	for i, d := range dests {
		sendWire(t, client, moveTo(byte(i), d).Data, table)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(stopsOf(obs.conn.sent())) < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	stops := stopsOf(obs.conn.sent())
	if len(stops) != 3 {
		t.Fatalf("the team's walk sent %d stop(s), want one per character (3)", len(stops))
	}
	seen := map[uint32]bool{}
	for _, p := range stops {
		handle := binary.LittleEndian.Uint32(p[2:])
		if seen[handle] {
			t.Fatalf("handle %#x was stopped twice", handle)
		}
		seen[handle] = true
	}
	for _, c := range team {
		if !seen[c.Handle] {
			t.Fatalf("character %#x was never stopped", c.Handle)
		}
	}
}
