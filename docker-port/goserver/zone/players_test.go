package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"geserver/protocol"
)

// recordConn is a net.Conn that keeps every write, so a fan-out test can read what
// a client would have received. Only Write is used by the code under test; the
// rest of net.Conn is here because the interface demands it.
type recordConn struct {
	mu     sync.Mutex
	writes [][]byte
	err    error
}

func (c *recordConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	c.writes = append(c.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (c *recordConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordConn) Close() error                     { return nil }
func (c *recordConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *recordConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *recordConn) SetDeadline(time.Time) error      { return nil }
func (c *recordConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordConn) SetWriteDeadline(time.Time) error { return nil }

// sent is what this connection was written, in order.
func (c *recordConn) sent() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

// player is one session in the roster, with its own socket.
type player struct {
	sess *Session
	conn *recordConn
}

// newPlayer builds a session that has entered the world: one leader, a socket and
// a position.
func newPlayer(handle uint32, name string, pos [3]int32) *player {
	conn := &recordConn{}
	c := Character{Handle: handle, Name: name, Job: 110, Level: 9, HP: 120, MaxHP: 200}
	sess := &Session{
		Conn: conn, Team: []Character{c}, Pos: pos,
		skipped: map[uint16]int{},
	}
	return &player{sess: sess, conn: conn}
}

// newTeam builds a session with a WHOLE team, each member with its own handle,
// class, stance and position - the case the first live test got wrong: only the
// leader was announced, so the other client saw one character of three.
func newTeam(handles []uint32, pos [][3]int32) *player {
	conn := &recordConn{}
	team := make([]Character, len(handles))
	for i, h := range handles {
		team[i] = Character{
			Handle: h, Name: "Char" + string(rune('A'+i)), Job: uint32(100 + i),
			Level: 9, HP: float64(100 + i), MaxHP: 200, Position: pos[i],
			// A real stance id, so a spawn built from this team cannot carry 0.
			Stance: uint32(10010 + 10*i),
		}
	}
	sess := &Session{
		Conn: conn, Team: team, Pos: pos[0],
		skipped: map[uint16]int{},
	}
	return &player{sess: sess, conn: conn}
}

// TestBuildLeavePC pins the 8-byte layout: cmd(2) handle(4) reason(2).
func TestBuildLeavePC(t *testing.T) {
	got := BuildLeavePC(0x40000031, LeavePCReason)
	if len(got) != 8 {
		t.Fatalf("%d bytes, want 8", len(got))
	}
	if cmd := cmdOf(got); cmd != CmdZCLeavePC {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCLeavePC)
	}
	if h := binary.LittleEndian.Uint32(got[2:]); h != 0x40000031 {
		t.Fatalf("handle = 0x%08x, want 0x40000031", h)
	}
	if r := binary.LittleEndian.Uint16(got[6:]); r != 0 {
		t.Fatalf("reason = %d, want 0 (OBJ_LEAVE_LOGOUT: the client only pops out "+
			"instantly for OBJ_OUTOFSIGHT=3, and every leave we send is a real one)", r)
	}
}

// TestBuildStopPC pins the 22-byte layout: cmd(2) handle(4) tick(4) x(4) y(4)
// cell(4). Without this packet a remote actor animates forever.
func TestBuildStopPC(t *testing.T) {
	got := BuildStopPC(0x40000031, [3]int32{1234, -5678, 9})
	if len(got) != 22 {
		t.Fatalf("%d bytes, want 22", len(got))
	}
	if cmd := cmdOf(got); cmd != CmdZCStopPC {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCStopPC)
	}
	if h := binary.LittleEndian.Uint32(got[2:]); h != 0x40000031 {
		t.Fatalf("handle = 0x%08x, want 0x40000031", h)
	}
	if tick := binary.LittleEndian.Uint32(got[6:]); tick == 0 {
		t.Fatal("tick is 0 - a real millisecond clock is what the client orders by")
	}
	if x, y, cell := int32(binary.LittleEndian.Uint32(got[10:])),
		int32(binary.LittleEndian.Uint32(got[14:])),
		int32(binary.LittleEndian.Uint32(got[18:])); x != 1234 || y != -5678 || cell != 9 {
		t.Fatalf("pos = (%d, %d, %d), want (1234, -5678, 9)", x, y, cell)
	}
}

// TestBuildSpawnStandingPC checks every field the client reads, and the two things
// that make a WRONG spawn (a length word that disagrees with the bytes, and a
// non-zero buff mask) impossible to ship unnoticed.
func TestBuildSpawnStandingPC(t *testing.T) {
	c := Character{Handle: 0x40000031, Name: "Jin", Job: 110, Level: 9,
		HP: 120.5, MaxHP: 200.25, Stance: 30110}
	pos := [3]int32{111, 222, 3}
	got := BuildSpawnStandingPC(c.Handle, c, pos, DefaultPosture)

	if len(got) != SpawnStandingPCSize {
		t.Fatalf("%d bytes, want %d", len(got), SpawnStandingPCSize)
	}
	// The number itself, not just the constant: 705 is what the client's reader and
	// the reference's builder agree on.
	if SpawnStandingPCSize != 705 {
		t.Fatalf("SpawnStandingPCSize = %d, want 705", SpawnStandingPCSize)
	}
	if cmd := cmdOf(got); cmd != CmdZCSpawnStandingPC {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCSpawnStandingPC)
	}
	// THE discipline: the length word IS the packet length. A mismatch makes the
	// client read the rest of the stream as option records, forever.
	if n := binary.LittleEndian.Uint16(got[2:]); int(n) != len(got) {
		t.Fatalf("length word = %d, packet = %d bytes - they MUST agree", n, len(got))
	}
	if h := binary.LittleEndian.Uint32(got[4:]); h != c.Handle {
		t.Fatalf("handle = 0x%08x, want 0x%08x", h, c.Handle)
	}
	if got[8] != 0 {
		t.Fatalf("direction = %d, want 0", got[8])
	}
	if cls := binary.LittleEndian.Uint16(got[9:]); cls != uint16(c.Job) {
		t.Fatalf("classID = %d, want the character's job %d", cls, c.Job)
	}
	if w := binary.LittleEndian.Uint16(got[11:]); w != 0 {
		t.Fatalf("weaponClassID = %d, want 0 (no equipment is sent)", w)
	}
	if p := binary.LittleEndian.Uint32(got[13:]); p != DefaultPosture {
		t.Fatalf("posture = %d, want %d", p, DefaultPosture)
	}
	if x, y, cell := int32(binary.LittleEndian.Uint32(got[17:])),
		int32(binary.LittleEndian.Uint32(got[21:])),
		int32(binary.LittleEndian.Uint32(got[25:])); x != pos[0] || y != pos[1] || cell != pos[2] {
		t.Fatalf("pos = (%d, %d, %d), want %v", x, y, cell, pos)
	}
	if got[29] != 0 {
		t.Fatalf("+29 = %d, want 0", got[29])
	}
	// THE STANCE, which is what stops the remote actor running: the character's own
	// id, never 0 when one is known.
	if st := binary.LittleEndian.Uint32(got[30:]); st != c.Stance {
		t.Fatalf("stance = %d, want the character's %d", st, c.Stance)
	}
	if st := binary.LittleEndian.Uint32(got[30:]); st == 0 {
		t.Fatal("stance = 0: a remote actor with no stance falls back to the run animation")
	}
	if hp, maxhp := math.Float64frombits(binary.LittleEndian.Uint64(got[34:])),
		math.Float64frombits(binary.LittleEndian.Uint64(got[42:])); hp != c.HP || maxhp != c.MaxHP {
		t.Fatalf("hp/maxhp = %v/%v, want %v/%v", hp, maxhp, c.HP, c.MaxHP)
	}
	// The buff mask: ANY set bit draws a glowing buff circle on the remote player.
	for i := 50; i < 306; i++ {
		if got[i] != 0 {
			t.Fatalf("buff mask byte %d = %d, want 0", i-50, got[i])
		}
	}

	// The appearance is COPY, not a hand-built block: everything equals
	// BuildStartInfo's own [21:416], except the nameplate string the spawn adds.
	start := BuildStartInfo(c)
	if len(start) < siAppearance+spAppearanceLen {
		t.Fatalf("START_INFO is %d bytes, too short for the %d-byte appearance",
			len(start), spAppearanceLen)
	}
	for off := 0; off < spAppearanceLen; off++ {
		if off >= spAppearanceName && off < spAppearanceName+spAppearanceNameLen {
			continue // the nameplate, checked below
		}
		if got[spAppearance+off] != start[siAppearance+off] {
			t.Fatalf("appearance byte %d = %d, want START_INFO's %d",
				off, got[spAppearance+off], start[siAppearance+off])
		}
	}
	// The NAMEPLATE, inside the appearance. Empty here means no name over the head.
	if name := trimNul(got[spAppearance+spAppearanceName : spAppearance+spAppearanceName+spAppearanceNameLen]); name != c.Name {
		t.Fatalf("nameplate = %q, want %q", name, c.Name)
	}
	// No equip options: the count is 0 and NOTHING follows the last field.
	if n := binary.LittleEndian.Uint32(got[spOptions:]); n != 0 {
		t.Fatalf("option count = %d, want 0", n)
	}
}

// TestJoinAnnouncesBothPlayers is the whole point: two sessions, and each one gets
// a spawn of the other. The newcomer's packets are RETURNED (Reply answers only its
// own frame) and the existing player's are written to their socket.
func TestJoinAnnouncesBothPlayers(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "Jin", [3]int32{100, 200, 1})
	b := newPlayer(0x40000041, "Mika", [3]int32{300, 400, 2})

	if out := h.joinZone(a.sess); len(out) != 0 {
		t.Fatalf("the first player got %d spawn(s), want 0", len(out))
	}
	if n := h.Players.Len(); n != 1 {
		t.Fatalf("roster holds %d player(s), want 1", n)
	}

	out := h.joinZone(b.sess)
	if len(out) != 2 {
		t.Fatalf("the newcomer got %d packet(s), want exactly the existing player's "+
			"spawn and the skill-disable that follows it", len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCSpawnStandingPC {
		t.Fatalf("newcomer packet cmd = %d, want %d", cmd, CmdZCSpawnStandingPC)
	}
	if got, want := binary.LittleEndian.Uint32(out[0][4:]), a.sess.Team[0].Handle; got != want {
		t.Fatalf("the newcomer was told about handle %d, want %d", got, want)
	}
	if cmd := cmdOf(out[1]); cmd != CmdZCSkillDisable {
		t.Fatalf("newcomer packet 1 cmd = %d, want the skill-disable %d that the live "+
			"server sends right after every spawn", cmd, CmdZCSkillDisable)
	}
	if got, want := binary.LittleEndian.Uint32(out[1][2:]), a.sess.Team[0].Handle; got != want {
		t.Fatalf("the skill-disable names handle %d, want the spawned %d", got, want)
	}

	// The existing player heard about the newcomer, once, from the newcomer's own
	// state (position included).
	if len(a.conn.sent()) != 2 {
		t.Fatalf("the existing player got %d packet(s), want the newcomer's spawn and "+
			"its skill-disable", len(a.conn.sent()))
	}
	spawn := a.conn.sent()[0]
	if cmd := cmdOf(spawn); cmd != CmdZCSpawnStandingPC {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCSpawnStandingPC)
	}
	if got, want := binary.LittleEndian.Uint32(spawn[4:]), b.sess.Team[0].Handle; got != want {
		t.Fatalf("the existing player was told about handle %d, want %d", got, want)
	}
	if got, want := int32(binary.LittleEndian.Uint32(spawn[17:])), b.sess.Pos[0]; got != want {
		t.Fatalf("spawn x = %d, want the newcomer's %d", got, want)
	}
	if cmd := cmdOf(a.conn.sent()[1]); cmd != CmdZCSkillDisable {
		t.Fatalf("the existing player's second packet is cmd %d, want %d", cmd,
			CmdZCSkillDisable)
	}
	// Two spawns in total, one each way, and one skill-disable after each.
	if total := len(a.conn.sent()) + len(out); total != 4 {
		t.Fatalf("%d packet(s) in total, want 4 (2 spawns + 2 skill-disables)", total)
	}
}

// TestJoinOrder is the order the reference announced in: the newcomer is told
// about the players who were there FIRST, in join order.
func TestJoinOrder(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "A", [3]int32{1, 1, 1})
	c := newPlayer(0x40000051, "C", [3]int32{2, 2, 2})
	b := newPlayer(0x40000041, "B", [3]int32{3, 3, 3})
	h.joinZone(a.sess)
	h.joinZone(c.sess)

	out := h.joinZone(b.sess)
	// Each character arrives as a spawn and the skill-disable that follows it.
	if len(out) != 4 {
		t.Fatalf("%d packet(s) for the newcomer, want 4 (a spawn and a skill-disable "+
			"per player)", len(out))
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != a.sess.Team[0].Handle {
		t.Fatalf("first spawn is handle %d, want the first joiner %d",
			got, a.sess.Team[0].Handle)
	}
	if got := binary.LittleEndian.Uint32(out[2][4:]); got != c.sess.Team[0].Handle {
		t.Fatalf("second spawn is handle %d, want the second joiner %d",
			got, c.sess.Team[0].Handle)
	}
}

// TestLeaveIsSentOnlyToOthers: a disconnect announces the leaver to every OTHER
// player once, and never to the leaver itself.
func TestLeaveIsSentOnlyToOthers(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "A", [3]int32{1, 1, 1})
	b := newPlayer(0x40000041, "B", [3]int32{2, 2, 2})
	h.joinZone(a.sess)
	h.joinZone(b.sess)

	// Clear what the join already wrote, so the assertion is about the leave alone.
	a.conn.writes, b.conn.writes = nil, nil

	h.leaveZone(b.sess)

	if got := len(b.conn.sent()); got != 0 {
		t.Fatalf("the leaver got %d packet(s), want 0", got)
	}
	sent := a.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("the other player got %d packet(s), want exactly the leave", len(sent))
	}
	if cmd := cmdOf(sent[0]); cmd != CmdZCLeavePC {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCLeavePC)
	}
	if got, want := binary.LittleEndian.Uint32(sent[0][2:]), b.sess.Team[0].Handle; got != want {
		t.Fatalf("leave names handle %d, want %d", got, want)
	}
	if n := h.Players.Len(); n != 1 {
		t.Fatalf("roster holds %d player(s) after the leave, want 1", n)
	}
	// A second leave for the same session announces nothing.
	h.leaveZone(b.sess)
	if got := len(a.conn.sent()); got != 1 {
		t.Fatalf("a repeated leave sent %d packet(s), want none", got-1)
	}
}

// TestMoveIsBroadcastUnchanged: the mover's echo and the other players' packet are
// the SAME bytes, and the roster learns where the leader now stands.
func TestMoveIsBroadcastUnchanged(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "A", [3]int32{1, 1, 1})
	b := newPlayer(0x40000041, "B", [3]int32{2, 2, 2})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil

	pos := [3]int32{777, 888, 4}
	// CZ_MOVE_PC: 10-byte header, charIndex, then x, y, cell.
	body := []byte{0}
	body = appendU32(body, uint32(pos[0]))
	body = appendU32(body, uint32(pos[1]))
	body = appendU32(body, uint32(pos[2]))
	pt := pack(CmdCZMovePC, body...)
	f := protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt}

	out, err := h.movePC(a.sess, f)
	if err != nil {
		t.Fatalf("movePC: %v", err)
	}
	if len(out) == 0 || cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("the mover's echo is missing: %d packet(s)", len(out))
	}
	sent := b.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("the other player got %d packet(s), want the move", len(sent))
	}
	if !bytes.Equal(sent[0], out[0]) {
		t.Fatalf("broadcast bytes differ from the echo:\n got %x\nwant %x", sent[0], out[0])
	}
	// The roster now knows where A stands, so a newcomer spawns there.
	for _, m := range h.Players.Snapshot() {
		if m.Sess == a.sess && m.Pos[0] != pos {
			t.Fatalf("roster leader position = %v, want %v", m.Pos[0], pos)
		}
	}
}

// TestFollowerMoveMovesOnlyThatFollower: a CZ_MOVE_PC names a character INDEX, and
// the roster must store that character's position - not overwrite the leader's. The
// broadcast is still sent for the follower's handle.
func TestFollowerMoveMovesOnlyThatFollower(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	leader := [3]int32{10, 10, 1}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{leader, {20, 20, 1}, {30, 30, 1}})
	b := newPlayer(0x40000041, "B", [3]int32{1, 1, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	b.conn.writes = nil

	// charIndex 2 = the THIRD character of the team.
	pos := [3]int32{999, 888, 7}
	body := []byte{2}
	body = appendU32(body, uint32(pos[0]))
	body = appendU32(body, uint32(pos[1]))
	body = appendU32(body, uint32(pos[2]))
	pt := pack(CmdCZMovePC, body...)

	out, err := h.movePC(a.sess, protocol.Frame{Cmd: CmdCZMovePC, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("movePC: %v", err)
	}
	if len(out) == 0 || cmdOf(out[0]) != CmdZCMovePC {
		t.Fatalf("the mover's echo is missing")
	}
	if moved := binary.LittleEndian.Uint32(out[0][2:]); moved != 0x40000033 {
		t.Fatalf("the echo names handle 0x%08x, want the third character 0x40000033", moved)
	}
	if sent := b.conn.sent(); len(sent) != 1 || !bytes.Equal(sent[0], out[0]) {
		t.Fatalf("the other player did not get the follower's move unchanged")
	}

	for _, m := range h.Players.Snapshot() {
		if m.Sess != a.sess {
			continue
		}
		if m.Pos[2] != pos {
			t.Fatalf("the moved follower's stored position = %v, want %v", m.Pos[2], pos)
		}
		if m.Pos[0] != leader {
			t.Fatalf("the LEADER's stored position moved to %v, want %v untouched",
				m.Pos[0], leader)
		}
		if m.Pos[1] != ([3]int32{20, 20, 1}) {
			t.Fatalf("the other follower moved to %v", m.Pos[1])
		}
	}
}

// TestTeamJoinAnnouncesEveryCharacter is BUG A: a three-character team used to be
// announced as its leader only. Every character needs its own spawn, with its own
// handle, appearance and position, or the far client can never draw (or move) the
// followers.
func TestTeamJoinAnnouncesEveryCharacter(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	posA := [][3]int32{{11, 11, 1}, {22, 22, 1}, {33, 33, 1}}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033}, posA)
	b := newPlayer(0x40000041, "B", [3]int32{44, 44, 1})

	if out := h.joinZone(a.sess); len(out) != 0 {
		t.Fatalf("the first player got %d spawn(s), want 0", len(out))
	}
	out := h.joinZone(b.sess)
	if len(out) != 9 {
		t.Fatalf("the newcomer got %d packet(s), want 9: a spawn, the skill-disable that "+
			"follows it and a stance for each of the 3 characters (a spawn alone leaves "+
			"the actor in a T-pose)", len(out))
	}
	seen := map[uint32]bool{}
	for i := 0; i < len(out); i += 3 {
		pkt := out[i]
		if cmd := cmdOf(out[i+1]); cmd != CmdZCSkillDisable {
			t.Fatalf("character %d was announced without the skill-disable the live "+
				"server sends after its spawn (cmd %d)", i/3, cmd)
		}
		if got := binary.LittleEndian.Uint32(out[i+1][2:]); got != a.sess.Team[i/3].Handle {
			t.Fatalf("character %d's skill-disable names handle %d, want %d",
				i/3, got, a.sess.Team[i/3].Handle)
		}
		if cmd := cmdOf(out[i+2]); cmd != CmdZCChangeStance {
			t.Fatalf("character %d was announced without its stance (cmd %d)", i/3, cmd)
		}
		if len(pkt) != SpawnStandingPCSize {
			t.Fatalf("spawn %d is %d bytes, want %d", i, len(pkt), SpawnStandingPCSize)
		}
		if n := binary.LittleEndian.Uint16(pkt[2:]); int(n) != len(pkt) {
			t.Fatalf("spawn %d length word = %d, packet = %d bytes", i, n, len(pkt))
		}
		handle := binary.LittleEndian.Uint32(pkt[4:])
		if seen[handle] {
			t.Fatalf("handle %d was spawned twice", handle)
		}
		seen[handle] = true
		if want := a.sess.Team[i/3].Handle; handle != want {
			t.Fatalf("spawn %d names handle %d, want %d", i, handle, want)
		}
		x := int32(binary.LittleEndian.Uint32(pkt[17:]))
		y := int32(binary.LittleEndian.Uint32(pkt[21:]))
		cell := int32(binary.LittleEndian.Uint32(pkt[25:]))
		if x != posA[i/3][0] || y != posA[i/3][1] || cell != posA[i/3][2] {
			t.Fatalf("spawn %d at (%d,%d,%d), want %v", i, x, y, cell, posA[i/3])
		}
		if got, want := binary.LittleEndian.Uint32(pkt[30:]), a.sess.Team[i/3].Stance; got != want {
			t.Fatalf("spawn %d stance = %d, want the character's %d", i, got, want)
		}
	}

	// The existing player was told about B's one character, and only that: its spawn
	// and the skill-disable that follows it (B's character has no stance).
	if sent := a.conn.sent(); len(sent) != 2 || cmdOf(sent[0]) != CmdZCSpawnStandingPC ||
		cmdOf(sent[1]) != CmdZCSkillDisable {
		t.Fatalf("the existing player got %d packet(s), want one spawn and one "+
			"skill-disable", len(sent))
	}
}

// TestTeamLeaveNamesEveryCharacter: the far client drew every member of the team,
// so a disconnect sends one ZC_LEAVE_PC per character handle - and none to the
// player who is leaving.
func TestTeamLeaveNamesEveryCharacter(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1, 1, 1}, {2, 2, 1}, {3, 3, 1}})
	b := newPlayer(0x40000041, "B", [3]int32{9, 9, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil

	h.leaveZone(a.sess)

	sent := b.conn.sent()
	if len(sent) != len(a.sess.Team) {
		t.Fatalf("%d leave packet(s), want one per character (%d)", len(sent), len(a.sess.Team))
	}
	for i, pkt := range sent {
		if len(pkt) != 8 {
			t.Fatalf("leave %d is %d bytes, want 8", i, len(pkt))
		}
		if cmd := cmdOf(pkt); cmd != CmdZCLeavePC {
			t.Fatalf("leave %d cmd = %d, want %d", i, cmd, CmdZCLeavePC)
		}
		if got, want := binary.LittleEndian.Uint32(pkt[2:]), a.sess.Team[i].Handle; got != want {
			t.Fatalf("leave %d names handle %d, want %d", i, got, want)
		}
	}
	if got := len(a.conn.sent()); got != 0 {
		t.Fatalf("the leaver got %d packet(s), want 0", got)
	}
}

// TestGameReadyAnnouncesThroughReply: the announcement really happens on
// CZ_GAME_READY, after the world-entry handshake, which is where the client is
// ready to draw another player.
func TestGameReadyAnnouncesThroughReply(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "A", [3]int32{1, 1, 1})
	b := newPlayer(0x40000041, "B", [3]int32{2, 2, 2})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.Reply(context.Background(), b.sess,
		protocol.Frame{Cmd: CmdCZGameReady, Size: 2, Data: pack(CmdCZGameReady)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("%d packet(s), want the handshake, the entering character's own "+
			"status sheet, its promotion line, the spawn and the skill-disable that "+
			"follows it: %d", len(out), len(out))
	}
	if cmdOf(out[0]) != CmdZCEnterLegacyObjs {
		t.Fatalf("first packet cmd = %d, want the world-entry handshake", cmdOf(out[0]))
	}
	if cmdOf(out[1]) != CmdZCUpdateAllStatus {
		t.Fatalf("second packet cmd = %d, want the entering character's own %d",
			cmdOf(out[1]), CmdZCUpdateAllStatus)
	}
	// The rank line rides beside the sheet, per character: the 3684 has no
	// promotion field, so ZC_PROMOTION_INFO (3093) is the ONLY carrier of
	// "Master" / "High Master" for the party card.
	if cmdOf(out[2]) != CmdZCPromotionInfo {
		t.Fatalf("third packet cmd = %d, want the promotion line %d",
			cmdOf(out[2]), CmdZCPromotionInfo)
	}
	if cmdOf(out[3]) != CmdZCSpawnStandingPC {
		t.Fatalf("fourth packet cmd = %d, want the spawn of the other player", cmdOf(out[3]))
	}
	if cmdOf(out[4]) != CmdZCSkillDisable {
		t.Fatalf("fifth packet cmd = %d, want the skill-disable of the spawned player",
			cmdOf(out[4]))
	}
	if sent := a.conn.sent(); len(sent) != 2 || cmdOf(sent[0]) != CmdZCSpawnStandingPC ||
		cmdOf(sent[1]) != CmdZCSkillDisable {
		t.Fatalf("the other player was not told about the newcomer (%d packet(s))", len(sent))
	}
}
