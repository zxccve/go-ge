package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
	"geserver/world"
)

// THE LAST POINT, tested with NO CLIENT and NO DATABASE: the 3595 request, the 3784
// answer, the one write per session, and the entry that puts the team back.
//
// The store is a fake whose behaviour is the CONTRACT the two Handler hooks promise
// (UseLastPoint), so a handler that forgets a case fails here. The one test that
// touches PostgreSQL is in last_point_db_test.go and is off by default, because it
// writes.

// lastPointFake is the store a handler test wires in: it COUNTS the writes, keeps what
// it was asked to write, and can be told to answer a load or to fail a save.
type lastPointFake struct {
	saves int
	saved []LastPoint
	// ids is every character id any call named, flattened; calls keeps them grouped
	// per call so a test can assert "this team's three members, once".
	ids   []uint32
	calls [][]uint32
	err   error

	// The load half, as the three answers PGStore.LastPointOf can give.
	point   LastPoint
	found   bool
	loadErr error
}

func (f *lastPointFake) save(ctx context.Context, commanderID int32, characterIDs []uint32,
	lp LastPoint) error {
	f.saves++
	f.saved = append(f.saved, lp)
	f.calls = append(f.calls, append([]uint32(nil), characterIDs...))
	f.ids = append(f.ids, characterIDs...)
	return f.err
}

func (f *lastPointFake) load(ctx context.Context, commanderID int32) (LastPoint, bool, error) {
	return f.point, f.found, f.loadErr
}

// lastPointSession is the session every test below starts from: a real commander, one
// leader standing at a real Coimbra-side point, and a skipped map because Reply's
// default branch writes to it.
func lastPointSession() *Session {
	return &Session{
		skipped:     map[uint16]int{},
		AccountID:   1,
		CommanderID: 25,
		Team:        []Character{{Handle: 1792, Name: "TesterF"}},
		Pos:         [3]int32{4301, 233, 146},
	}
}

// testMesh is the map's navmesh as a test stands it in: two small tables, so the two
// halves of the Last Point can be tested with no `pathtool.exe` and no `.tok` file.
//
// It is a FAKE of `Handler.NavHeight` / `Handler.NavNear`, which are the game's own
// `CWorld::GetHeight(pos)` and `Get3DPointToPosLocaly(point, out, 1000, 300)`. The two
// answers it gives are exactly the two facts the real mesh gives, and nothing else:
//
//	heights: WORLDPOS{x, y, CELL} -> the world HEIGHT of that spot
//	ground:  x, y                -> the WORLDPOS the mesh resolves a 3D point to
//
// `asked` records the z of every Near call, so a test can prove the entry handed the
// mesh the row's HEIGHT and not some number of its own.
type testMesh struct {
	heights map[[3]int32]int32
	ground  map[[2]int32][3]int32
	asked   []int32
}

func newTestMesh() *testMesh {
	return &testMesh{
		heights: map[[3]int32]int32{},
		ground:  map[[2]int32][3]int32{},
	}
}

// onGround registers one spot: the WORLDPOS the client stands on, and the height there.
func (m *testMesh) onGround(pos [3]int32, height int32) *testMesh {
	m.heights[pos] = height
	m.ground[[2]int32{pos[0], pos[1]}] = pos
	return m
}

func (m *testMesh) height(pos [3]int32) (int32, bool) {
	h, ok := m.heights[pos]
	return h, ok
}

func (m *testMesh) near(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
	m.asked = append(m.asked, pt[2])
	pos, ok := m.ground[[2]int32{pt[0], pt[1]}]
	if !ok {
		return [3]int32{}, 0, false
	}
	h, ok := m.heights[pos]
	if !ok {
		return [3]int32{}, 0, false
	}
	// The vertical range is the original's own tolerance, and it is what makes a stored
	// CELL miss: a cell is tens to thousands of units away from the ground height.
	if d := pt[2] - h; d > int32(vRange) || d < -int32(vRange) {
		return [3]int32{}, 0, false
	}
	return pos, h, true
}

// wire installs the two hooks on a handler, the way main.go does with the real engine.
func (m *testMesh) wire(h *Handler) {
	h.NavHeight = m.height
	h.NavNear = m.near
}

// sysMenuFrame builds CZ_SYSMENU (3595) as the client sends it: command, seq, checksum,
// then the selection BYTE at +10 and the bool at +11.
func sysMenuFrame(selection uint8) protocol.Frame {
	data := []byte{0x0b, 0x0e, 0x12, 0x00, 0x00, 0x00, 0x0a, 0x00, 0x00, 0x00, 0, 0}
	data[sysMenuSelectionAt] = selection
	return protocol.Frame{Cmd: CmdCZSysMenu, Size: len(data), Data: data}
}

// lastPointHandler is a map-10 zone with a private handoff folder and a kept log, so a
// test can read back the exact line the handler printed.
//
// The mesh knows the leader's own spot, which is what a log-out needs: the height of
// (4301, 233, cell 146) is 257, so `zpos` must be written as 257 and never as 146.
func lastPointHandler(t *testing.T, f *lastPointFake) (*Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	h := &Handler{
		Log: log.New(logs, "", 0), MapID: 10, MapName: "coim_basic",
		Transfers: t.TempDir(),
	}
	newTestMesh().onGround([3]int32{4301, 233, 146}, 257).wire(h)
	h.UseLastPoint(f.load, f.save)
	return h, logs
}

// TestSysMenuLogoutSavesTheLastPointAndAnswersLogoutOK is the whole log-out path: the
// request is saved, and ZC_LOGOUT_OK goes out so the client closes its own socket
// instead of waiting out its 10 s reserve timer.
//
// THE MISTAKE IT CATCHES: answering 3595 with nothing (or with the wrong command), which
// leaves a player staring at a frozen screen for ten seconds after pressing Log Out -
// the exact symptom measured in docker-port\logs\zone10_err.txt (19:38:20 request,
// 19:38:30 EOF), and the reason the packet exists at all.
func TestSysMenuLogoutSavesTheLastPointAndAnswersLogoutOK(t *testing.T) {
	f := &lastPointFake{}
	h, logs := lastPointHandler(t, f)
	sess := lastPointSession()

	out, err := h.Reply(context.Background(), sess, sysMenuFrame(sysMenuLogout))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 || len(out[0]) != 2 {
		t.Fatalf("answered %d packet(s) (%v), want one 2-byte ZC_LOGOUT_OK", len(out), out)
	}
	// 3784 is 0x0ec8, so on the wire the two bytes are c8 0e. Asserted through the
	// constant, and then as bytes, so neither a renamed constant nor a swapped word
	// can pass.
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != CmdZCLogoutOK {
		t.Fatalf("command = %d, want %d", cmd, CmdZCLogoutOK)
	}
	if out[0][0] != 0xc8 || out[0][1] != 0x0e {
		t.Fatalf("bytes = %x, want c80e (3784 little endian)", out[0])
	}

	if f.saves != 1 {
		t.Fatalf("%d save(s), want exactly 1", f.saves)
	}
	// x and y are the leader's own; z is the HEIGHT the mesh has there (257), NOT the
	// cell the leader stands on (146). That difference is R73's second bug.
	wantSaved := [3]int32{sess.Pos[0], sess.Pos[1], 257}
	if got := f.saved[0]; got.MapID != h.MapID || got.Pos != wantSaved {
		t.Fatalf("saved %+v, want map %d at %v (x/y from the session, z = the HEIGHT)",
			got, h.MapID, wantSaved)
	}
	if f.saved[0].Pos[2] == sess.Pos[2] {
		t.Fatalf("zpos = %d, the navmesh CELL - the oracle's ZPOS holds a height",
			f.saved[0].Pos[2])
	}
	if f.ids[0] != 1792 {
		t.Fatalf("character id = %d, want the leader's handle 1792", f.ids[0])
	}
	if !strings.Contains(logs.String(), "LAST POINT saved") {
		t.Fatalf("the save is not in the log:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "trigger "+lastPointSysMenu) {
		t.Fatalf("the log does not name the sysmenu trigger:\n%s", logs)
	}
}

// TestSysMenuFromTheBarrackSavesAndSendsMoveBarrack: selection 0 is "go to barrack", which
// also leaves the map, so the point is saved - and ZC_MOVE_BARRACK (3597, two bytes) is
// answered, which is what the reference's TransferBarrack sends (Commander.cpp:1236).
// Until 2026-11-09 this asserted NO answer ("the barrack hand-off is a different task"); the
// owner then pressed the button and nothing happened, and the reference showed why.
//
// THE MISTAKE IT CATCHES: only saving on LOGOUT, which would lose the last point of every
// player who leaves through the barrack button - the most common way out of a town - and
// answering a selection with the wrong packet.
func TestSysMenuFromTheBarrackSavesAndSendsMoveBarrack(t *testing.T) {
	f := &lastPointFake{}
	h, _ := lastPointHandler(t, f)
	sess := lastPointSession()

	out, err := h.Reply(context.Background(), sess, sysMenuFrame(0))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// TWO packets, in the client's order: ZC_LOGOUT_OK (it is what lets the client act on the
	// switch) and then ZC_MOVE_BARRACK (the switch target). See zone\sysmenu.go.
	if len(out) != 2 ||
		out[0][0] != byte(CmdZCLogoutOK&0xFF) || out[0][1] != byte(CmdZCLogoutOK>>8) ||
		out[1][0] != byte(CmdZCMoveBarrack&0xFF) || out[1][1] != byte(CmdZCMoveBarrack>>8) {
		t.Fatalf("answered %v for the barrack selection, want ZC_LOGOUT_OK (%d) then ZC_MOVE_BARRACK (%d)",
			out, CmdZCLogoutOK, CmdZCMoveBarrack)
	}
	if f.saves != 1 {
		t.Fatalf("%d save(s), want exactly 1 (the barrack leaves the map too)", f.saves)
	}
}

// TestDisconnectWithNoPositionWritesNothing: a client that dies before reporting a single
// position has nothing to save, and a zero would overwrite the good row already on file.
//
// THE MISTAKE IT CATCHES: writing (0, 0, cell 0) on every disconnect during the loading
// screen. It would erase the player's last point silently - the row would still look
// written, and the next entry would drop the team into the void.
func TestDisconnectWithNoPositionWritesNothing(t *testing.T) {
	f := &lastPointFake{}
	// A previous, GOOD row: it must survive untouched.
	previous := LastPoint{MapID: 10, Pos: [3]int32{3946, 193, 146}, Name: "TesterF"}
	f.saved = []LastPoint{previous}

	h, logs := lastPointHandler(t, f)
	sess := lastPointSession()
	sess.Pos = [3]int32{}

	h.noteLastPoint(sess, lastPointDisconnect)

	if f.saves != 0 {
		t.Fatalf("%d save(s), want none: no position was ever reported", f.saves)
	}
	if len(f.saved) != 1 || f.saved[0] != previous {
		t.Fatalf("the stored row changed: %+v, want the previous %+v", f.saved, previous)
	}
	if !strings.Contains(logs.String(), "nothing written") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}

// TestMapChangeSavesTheMapLeftNotTheArrival: walking through a gate leaves map 10 for
// map 15, and the point saved must be the one on the map being LEFT.
//
// THE MISTAKE IT CATCHES: saving `m.pos`, which is the ARRIVAL point on the OTHER map.
// The row would then claim a position on map 10 that belongs to map 15's mesh, and the
// client would be dropped through the world on the next entry. It also pins the
// one-write rule: the session ends right after this, and the disconnect that follows
// must not write a second row.
func TestMapChangeSavesTheMapLeftNotTheArrival(t *testing.T) {
	f := &lastPointFake{}
	h, logs := lastPointHandler(t, f)
	h.MapID = 10

	sess := lastPointSession()
	sess.Gate = &gateMove{
		gate: MapGate{Name: "REB_BASIC-COIM_BASIC", To: "coim_basic", ToID: 15},
		dest: world.Record{IP: "127.0.0.1", Port: 7002},
		pos:  [3]int32{2268, 16931, 233},
	}

	h.acceptMove(sess, sess.Gate)

	if f.saves != 1 {
		t.Fatalf("%d save(s), want exactly 1", f.saves)
	}
	if got := f.saved[0].MapID; got != 10 {
		t.Fatalf("saved map = %d, want 10 (the map being LEFT)", got)
	}
	if got := f.saved[0].Pos; got == sess.Gate.pos {
		t.Fatalf("saved the gate's ARRIVAL point %v - that is a cell on map 15", got)
	}
	// The x and y are the leader's walked spot; the z is the HEIGHT there, not the cell.
	if got := f.saved[0].Pos; got[0] != sess.Pos[0] || got[1] != sess.Pos[1] || got[2] != 257 {
		t.Fatalf("saved %v, want the leader's walked position %v with z = the HEIGHT 257",
			got, sess.Pos)
	}
	if !strings.Contains(logs.String(), "trigger "+lastPointMapChange) {
		t.Fatalf("the log does not name the map-change trigger:\n%s", logs)
	}

	// The socket closes right after a map change: exactly one row, one line.
	h.noteLastPoint(sess, lastPointDisconnect)
	if f.saves != 1 {
		t.Fatalf("%d save(s) after the follow-up disconnect, want still 1", f.saves)
	}
}

// continueHandler is a map-10 zone that can place a team: three characters, a spawn with
// a formation, the Last Point store wired, and the map's mesh (faked) so a stored HEIGHT
// can be turned into a cell. The commander comes from CommanderOf, because startGame
// resolves it before it asks where the team enters.
func continueHandler(t *testing.T, f *lastPointFake) (*Handler, *testMesh, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	cell := int32(146)
	spawn := [3]int32{3946, 193, cell}
	h := &Handler{
		Log: log.New(logs, "", 0), MapID: 10, MapName: "coim_basic",
		Transfers: t.TempDir(),
		Spawn:     spawn,
		SpawnFormation: []FormationSlot{
			{X: spawn[0], Y: spawn[1], Cell: &cell},
			{X: spawn[0] + 60, Y: spawn[1] + 60, Cell: &cell},
			{X: spawn[0] - 60, Y: spawn[1] - 60, Cell: &cell},
		},
		TeamOf: func(ctx context.Context, accountID uint32) ([]Character, error) {
			return []Character{
				{Handle: 1792, Name: "TesterF", Job: 1, Level: 1},
				{Handle: 1793, Name: "TesterG", Job: 1, Level: 1},
				{Handle: 1794, Name: "TesterH", Job: 1, Level: 1},
			}, nil
		},
		CommanderOf: func(ctx context.Context, accountID uint32) (int32, error) {
			return 25, nil
		},
	}
	// The map's ground: the spawn, and the spot the leader logged out on. Its HEIGHT is
	// 257, which is what a row must carry - the CELL 146 is what the client is given.
	mesh := newTestMesh().
		onGround(spawn, 250).
		onGround([3]int32{4301, 233, 146}, 257)
	mesh.wire(h)
	h.UseLastPoint(f.load, f.save)
	return h, mesh, logs
}

// TestContinueEntryPutsTheTeamBackAtTheLastPoint is the entry half: the barrack's Last
// Point was pressed, and the team must land where the leader stood - not at the map's
// restart point.
//
// THE MISTAKE IT CATCHES: reading the row and not using it. Before this, every entry went
// handoff -> saved start point -> map spawn, and a player who pressed "Last Point" was
// put on the town's restart point instead of the spot they left.
//
// IT ALSO PINS R73's UNIT CHANGE: the row holds a HEIGHT (257) and the team must enter on
// the CELL the mesh resolves that height to (146). A server that handed the stored number
// straight to the client would place the character at cell 257 - under the floor.
func TestContinueEntryPutsTheTeamBackAtTheLastPoint(t *testing.T) {
	f := &lastPointFake{
		point: LastPoint{MapID: 10, Pos: [3]int32{4301, 233, 257}, Name: "TesterF"},
		found: true,
	}
	h, mesh, logs := continueHandler(t, f)
	sess := &Session{skipped: map[uint16]int{}}

	if _, err := h.Reply(context.Background(), sess, startGameFrame(4242)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(sess.Team) != 3 {
		t.Fatalf("%d character(s), want 3", len(sess.Team))
	}
	want := [3]int32{4301, 233, 146} // the same x/y, the CELL the mesh resolves
	if got := sess.Team[0].Position; got != want {
		t.Fatalf("the leader enters at %v, want %v (x/y from the row, cell from the mesh)",
			got, want)
	}
	if sess.Pos != want {
		t.Fatalf("the session's leader position is %v, want %v", sess.Pos, want)
	}
	// The mesh was asked about the row's HEIGHT, not about a cell.
	if len(mesh.asked) == 0 || mesh.asked[0] != 257 {
		t.Fatalf("the mesh was asked about z %v, want the stored HEIGHT 257", mesh.asked)
	}
	if !strings.Contains(logs.String(), "LAST POINT of commander") {
		t.Fatalf("the entry did not name the last point:\n%s", logs)
	}
}

// TestContinueEntryRefusesAPositionOnAnotherMap: the row names a map this zone does not
// serve, so its cell means nothing here and the team must enter at the map's own point.
//
// THE MISTAKE IT CATCHES: sending the stored cell whatever map it came from. A zone
// serves ONE map; a cell from map 15's navmesh is not a triangle on map 10, and the
// client drops the character through the world (PathEngine, same crash gate.go refuses).
func TestContinueEntryRefusesAPositionOnAnotherMap(t *testing.T) {
	f := &lastPointFake{
		point: LastPoint{MapID: 15, Pos: [3]int32{2268, 16931, 233}, Name: "TesterF"},
		found: true,
	}
	h, _, logs := continueHandler(t, f)
	sess := &Session{skipped: map[uint16]int{}}

	if _, err := h.Reply(context.Background(), sess, startGameFrame(4242)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := sess.Team[0].Position; got != h.Spawn {
		t.Fatalf("the leader enters at %v, want the map's own spawn %v", got, h.Spawn)
	}
	if !strings.Contains(logs.String(), "the saved cell means nothing here") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}

// TestContinueEntryRefusedWhenTheBarrackDidNotAsk: the row exists but the player pressed
// NEW GAME, so the store answers found=false and the map's own spawn is used.
//
// THE MISTAKE IT CATCHES: treating "there is a position on file" as "the player asked to
// continue". That is the difference between BAR_NEW / CB_STARTGAME (22) and BAR_CON /
// CB_CONTINUEGAME (23): without it every new game would start wherever the last session
// ended, and the barrack's two buttons would do the same thing.
func TestContinueEntryRefusedWhenTheBarrackDidNotAsk(t *testing.T) {
	f := &lastPointFake{
		point: LastPoint{MapID: 10, Pos: [3]int32{4301, 233, 146}, Name: "TesterF"},
		found: false,
	}
	h, _, logs := continueHandler(t, f)
	sess := &Session{skipped: map[uint16]int{}}

	if _, err := h.Reply(context.Background(), sess, startGameFrame(4242)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := sess.Team[0].Position; got != h.Spawn {
		t.Fatalf("the leader enters at %v, want the map's own spawn %v", got, h.Spawn)
	}
	for _, c := range sess.Team {
		if c.Position == f.point.Pos {
			t.Fatalf("a character was placed at the stored point %v although the "+
				"player did not ask to continue", f.point.Pos)
		}
	}
	if !strings.Contains(logs.String(), "did not come through the barrack's Last Point") {
		t.Fatalf("the refusal is not in the log:\n%s", logs)
	}
}
