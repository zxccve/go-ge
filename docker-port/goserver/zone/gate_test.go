package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
	"geserver/world"
)

// zoneTable reads the REAL spawn table, because a gate's trigger radius, its exit
// point and its camera floats are the data this feature exists to serve. A copy
// written into this file would only ever prove the test was edited with it.
func zoneTable(t *testing.T) *ZonePoints {
	t.Helper()
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	return points
}

// coimGate is Coimbra's gate to the field map: the one gate in the docs, so every
// number asserted below is the table's own.
func coimGate(t *testing.T) MapGate {
	t.Helper()
	for _, g := range zoneTable(t).Gates(15) {
		if g.Name == "COIM_BASIC-FLD_COIM" {
			return g
		}
	}
	t.Fatalf("map 15 no longer carries the gate COIM_BASIC-FLD_COIM; the table changed")
	return MapGate{}
}

// liveZone writes ONE presence file in the format src\world.py and the Go announcer
// both write, so the gate path is tested against the real registry reader instead of a
// stub that can disagree with it.
func liveZone(t *testing.T, mapID, port int) *world.Registry {
	t.Helper()
	dir := t.TempDir()
	rec := world.Record{
		MapID: mapID, Channel: 0, IP: "127.0.0.1", Port: port, MapName: "fld_coim",
		LastSeen: float64(time.Now().UnixNano()) / 1e9,
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal presence: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("zone_%d.json", port))
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write presence: %v", err)
	}
	return &world.Registry{Dir: dir}
}

// gateHandler builds a zone serving map 15 that can actually start a map change: the
// real gate, a live destination zone, and a private handoff folder.
func gateHandler(t *testing.T, gate MapGate) (*Handler, string) {
	t.Helper()
	h, transfers, _ := gateHandlerLog(t, gate)
	return h, transfers
}

// gateHandlerLog is gateHandler with the log kept, so a test can read back the exact
// line the gate path printed.
func gateHandlerLog(t *testing.T, gate MapGate) (*Handler, string, *bytes.Buffer) {
	t.Helper()
	transfers := t.TempDir()
	logs := &bytes.Buffer{}
	return &Handler{
		Log: log.New(logs, "", 0), MapID: 15,
		Gates:     []MapGate{gate},
		Zones:     liveZone(t, gate.ToID, 7003),
		Transfers: transfers,
	}, transfers, logs
}

// insideGate is a leader position 100 units from the gate's own point, which is inside
// every measured range (the smallest is 90, so the caller checks).
func insideGate(gate MapGate) [3]int32 {
	return [3]int32{gate.X + 100, gate.Y, cellOrZero(gate.Cell)}
}

// standAt is "the character IS here": the assignment these tests mean when they move the
// leader by hand. sess.Pos alone is the click DESTINATION since walkedPos exists
// (gate.go), so a bare assignment after START_GAME - which leaves a walk segment behind -
// would read as a walk still in progress from the spawn.
func standAt(sess *Session, pos [3]int32) {
	sess.Pos = pos
	sess.moveFrom, sess.moveAt = pos, time.Now()
}

// TestArrivalInsideAGateDoesNotBounce is the crash a gate's own exit causes: the gate
// BACK from the destination map is often right where the gate FORWARD puts the team, so
// without arming that gate the arrival would fire it at once and send the player back,
// and back, and back. The first tick after entry must send nothing; walking out and back
// in must still fire.
func TestArrivalInsideAGateDoesNotBounce(t *testing.T) {
	gate := coimGate(t)
	h, _ := gateHandler(t, gate)
	h.Spawn = insideGate(gate)
	// The spawn is what places the team here; the formation is the town's own.
	h.SpawnFormation = nil

	a := newTeam([]uint32{0x40000031}, [][3]int32{{1, 2, 3}})
	start := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	if _, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(start), Data: start}); err != nil {
		t.Fatalf("START_GAME: %v", err)
	}
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("the tick right after arrival fired %d packet(s) - it would bounce the "+
			"player straight back", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("the gate the team landed in started a map change")
	}

	// Stepping out and back in is a real map change.
	standAt(a.sess, [3]int32{gate.X + gate.Range + 200, gate.Y, cellOrZero(gate.Cell)})
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("a tick outside the gate sent %d packet(s)", len(out))
	}
	standAt(a.sess, insideGate(gate))
	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("walking back in sent %d warp notice(s), want one ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(out) != 1 {
		t.Fatalf("walking back into the gate sent %d packet(s), want 1", len(out))
	}
	if a.sess.Gate == nil {
		t.Fatal("walking back into the gate did not start a map change")
	}
}

// splitWarp separates a tick's warp-point NOTICES (3540..3542) from the packets that move
// the map. Every assertion below counts the second group; the notices are counted where
// they are the subject (TestWarpPointEnterPrecedesMoveZone and friends).
func splitWarp(out [][]byte) (warp, rest [][]byte) {
	for _, p := range out {
		switch cmdOf(p) {
		case CmdZCSpawnWarpPoint, CmdZCEnterWarpPoint, CmdZCLeaveWarpPoint:
			warp = append(warp, p)
		default:
			rest = append(rest, p)
		}
	}
	return warp, rest
}

// handoffsIn lists the handoff files a zone has left for the destination zone. One map
// change leaves exactly one, so every count below is really "how many moves finished".
func handoffsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the handoff folder %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "transfer_") && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestWarpPointEnterPrecedesMoveZone pins the ORDER, which is the one thing about the
// gate handshake that is already known to be right: the client must be told it stands in
// a warp point (3541) BEFORE it is asked to change map (3593). gateTick answers the
// packets in the order they are to be written - zone.go hands the whole slice to ONE
// writeSession - so index 0 is what the client reads first.
//
// One of each, at the sizes the client's own table gives (data\cmd_table_live_z.txt: 3541
// is 32, 3593 is 3). A missing 3541, a 3541 that arrives second, an extra one of either,
// or either packet built at the wrong size all fail here.
func TestWarpPointEnterPrecedesMoveZone(t *testing.T) {
	table, err := protocol.LoadSpec(filepath.Join("..", "..", "data", "cmd_table_live_z.txt"))
	if err != nil {
		t.Fatalf("read the client's command table: %v", err)
	}
	gate := coimGate(t)
	if gate.Range <= 100 {
		t.Fatalf("the gate's range is %d, too small for this test to stand inside it",
			gate.Range)
	}
	h, _ := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031}, [][3]int32{insideGate(gate)})

	out := h.gateTick(a.sess)
	if len(out) != 2 {
		t.Fatalf("standing in gate %s sent %d packet(s), want exactly 2 (3541 then 3593)",
			gate.Name, len(out))
	}

	// The order itself: the enter notice is packet 0, the map change is packet 1.
	if got, want := cmdOf(out[0]), uint16(CmdZCEnterWarpPoint); got != want {
		t.Fatalf("the first packet is cmd %d, want %d - ZC_ENTER_WARPPOINT must reach "+
			"the client BEFORE ZC_MOVE_ZONE (got %d as the first packet)",
			got, want, got)
	}
	if got, want := cmdOf(out[1]), uint16(CmdZCMoveZone); got != want {
		t.Fatalf("the second packet is cmd %d, want %d", got, want)
	}

	// Each at the size the client's table states, and the table is the authority: a
	// builder that drifts from it is caught here, not in game.
	for i, p := range out {
		cmd := cmdOf(p)
		want, ok := table.Size(cmd)
		if !ok {
			t.Fatalf("packet %d is cmd %d, which the client's table does not list", i, cmd)
		}
		if len(p) != want {
			t.Fatalf("packet %d is cmd %d at %d bytes, the client's table says %d",
				i, cmd, len(p), want)
		}
	}
	// The two numbers the client's table gives for these two, spelled out: 32 and 3. The
	// named constants (WarpPointSize, MoveZoneSize) hold the same pair; the table loop
	// above is what catches a constant that drifts from it.
	if got := len(out[0]); got != 32 {
		t.Fatalf("ZC_ENTER_WARPPOINT is %d bytes, want the client's 32", got)
	}
	if got := len(out[1]); got != 3 {
		t.Fatalf("ZC_MOVE_ZONE is %d bytes, want the client's 3", got)
	}

	// Exactly once each: the very next tick is silent, so what the assertions above saw is
	// the entry and not a send that repeats every 100 ms.
	if again := h.gateTick(a.sess); len(again) != 0 {
		t.Fatalf("the next tick sent %d packet(s), want 0 - the enter notice and the map "+
			"change are one per entry", len(again))
	}
}

// TestGateFiresOncePerEntry is the core rule: the zone's tick notices the leader
// standing in a gate and sends ZC_MOVE_ZONE (3593) ONCE, then nothing until the player
// walks out and back in.
func TestGateFiresOncePerEntry(t *testing.T) {
	gate := coimGate(t)
	if gate.Range <= 100 {
		t.Fatalf("the gate's range is %d, too small for this test to stand inside it",
			gate.Range)
	}
	h, _ := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031}, [][3]int32{insideGate(gate)})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the first tick sent %d warp notice(s), want one ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(out) != 1 {
		t.Fatalf("the first tick sent %d packet(s), want 1 (ZC_MOVE_ZONE)", len(out))
	}
	if len(out[0]) != MoveZoneSize {
		t.Fatalf("ZC_MOVE_ZONE is %d bytes, want %d", len(out[0]), MoveZoneSize)
	}
	if got := cmdOf(out[0]); got != CmdZCMoveZone {
		t.Fatalf("gate packet is cmd %d, want %d", got, CmdZCMoveZone)
	}
	// The flag byte is FALSE, as the live server sends it (`09 0e 00`, capture line 593).
	if out[0][2] != 0 {
		t.Fatalf("ZC_MOVE_ZONE flag byte = %d, want 0", out[0][2])
	}
	if a.sess.Gate == nil {
		t.Fatal("the session does not remember the map change it started")
	}

	// A second tick with the player still standing there must not fire again: that is
	// the whole reason the session remembers which gate it is in.
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("a second tick in the same gate sent %d packet(s), want none", len(out))
	}

	// Leaving the gate re-arms it - and dropping the pending handshake is what a
	// client that never answered looks like.
	a.sess.Gate = nil
	a.sess.Pos = [3]int32{gate.X + gate.Range + 200, gate.Y, cellOrZero(gate.Cell)}
	left, out := splitWarp(h.gateTick(a.sess))
	if len(left) != 1 || cmdOf(left[0]) != CmdZCLeaveWarpPoint {
		t.Fatalf("walking out sent %d warp notice(s), want one ZC_LEAVE_WARPPOINT", len(left))
	}
	if len(out) != 0 {
		t.Fatalf("a tick outside the gate sent %d packet(s), want none", len(out))
	}
	a.sess.Pos = insideGate(gate)
	warp, out = splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("walking back in sent %d warp notice(s), want one ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(out) != 1 {
		t.Fatalf("walking back into the gate sent %d packet(s), want 1", len(out))
	}
}

// TestGateOutsideItsRangeDoesNothing: a gate is a circle of its OWN radius, not a
// point, and not one radius for every gate. Outside it the leader is simply walking
// past and nothing may be sent.
func TestGateOutsideItsRangeDoesNothing(t *testing.T) {
	gate := coimGate(t)
	h, _ := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031},
		[][3]int32{{gate.X, gate.Y + gate.Range + 1, cellOrZero(gate.Cell)}})

	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("a leader %d units away (range %d) sent %d packet(s), want none",
			gate.Range+1, gate.Range, len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("a gate fired from outside its range")
	}
}

// TestGateRefusesANullArrivalCell: 2 of the 332 gates carry no arrival cell. Sending
// 3593 for one of them would start a handshake that can only end with a player dropped
// on ground nobody resolved, so it is refused BEFORE 3593 - but the ENTER notice still
// goes out, because standing inside a warp point is true whether or not the door works
// (the reference sends it from the range visitor, not from the transfer).
func TestGateRefusesANullArrivalCell(t *testing.T) {
	gate := coimGate(t)
	gate.TargetCell = nil
	h, _ := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031}, [][3]int32{insideGate(gate)})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the refused gate answered %d warp notice(s), want one ZC_ENTER_WARPPOINT",
			len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("a gate with no arrival cell sent %d packet(s), want none", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("a gate with no arrival cell started a map change")
	}
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("the refusal repeated itself with %d packet(s)", len(out))
	}
}

// TestGateRefusesWithoutALiveZone: no zone serves the destination map, so there is no
// ip and port to name in 3594. Refusing before 3593 is the difference between "that
// gate does not work" and a client waiting for an answer that will never come.
func TestGateRefusesWithoutALiveZone(t *testing.T) {
	gate := coimGate(t)
	h, _ := gateHandler(t, gate)
	// A registry that only knows about a DIFFERENT map: "some zone is alive" is not
	// "this map is served".
	h.Zones = liveZone(t, 15, 7002)
	a := newTeam([]uint32{0x40000031}, [][3]int32{insideGate(gate)})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the refused gate answered %d warp notice(s), want one ZC_ENTER_WARPPOINT",
			len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("a gate with no live destination sent %d packet(s), want none", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("a gate with no live destination started a map change")
	}

	// And a zone with NO registry at all is the same answer, not a panic.
	h.Zones = nil
	a.sess.gateInside = 0
	warp, out = splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the forced re-entry answered %d warp notice(s), want one "+
			"ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("a zone with no registry sent %d packet(s), want none", len(out))
	}
}

// TestMoveZoneOKIsOneHundredAndOneBytes asserts the WHOLE layout of 3594 against
// Coimbra's gate to fld_coim, offset by offset, through the dispatch the socket uses.
//
// Every offset here is the client's, from geZCPacket.h: a packed struct with no
// padding, so a single wrong offset moves every field after it.
func TestMoveZoneOKIsOneHundredAndOneBytes(t *testing.T) {
	gate := coimGate(t)
	if gate.TargetCell == nil {
		t.Fatalf("gate %s has no arrival cell in the table", gate.Name)
	}
	h, transfers := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{insideGate(gate), insideGate(gate), insideGate(gate)})
	a.sess.AccountID = 7000001

	if out := h.gateTick(a.sess); len(out) != 2 {
		t.Fatalf("the gate did not start a map change: %d packet(s)", len(out))
	}

	// CZ_MOVE_ZONE_OK is the 10-byte security header and no body - the size comes from
	// the client's own table, so `pack` giving exactly 10 bytes is the real packet.
	pt := pack(CmdCZMoveZoneOK)
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("MOVE_ZONE_OK answered %d packet(s), want 1", len(out))
	}
	p := out[0]
	if len(p) != MoveZoneOKSize {
		t.Fatalf("ZC_MOVE_ZONE_OK is %d bytes, want %d", len(p), MoveZoneOKSize)
	}
	if got := cmdOf(p); got != CmdZCMoveZoneOK {
		t.Fatalf("cmd = %d, want %d", got, CmdZCMoveZoneOK)
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != uint32(gate.ToID) {
		t.Fatalf("+2 zoneInstID = %d, want the destination map id %d", got, gate.ToID)
	}
	// zoneIP is a dotted-decimal STRING in 16 bytes, NUL padded - the original fills it
	// with GetStrIP (ImcCore.h:42), so a packed 4-byte address here would be "127.0".
	ip := p[6:22]
	if got := string(ip[:9]); got != "127.0.0.1" {
		t.Fatalf("+6 zoneIP = %q, want the dotted 127.0.0.1", got)
	}
	for i := 9; i < 16; i++ {
		if ip[i] != 0 {
			t.Fatalf("+%d is %d, want the NUL that pads the ip", 6+i, ip[i])
		}
	}
	if got := binary.LittleEndian.Uint32(p[22:]); got != 7003 {
		t.Fatalf("+22 zonePort = %d, want 7003", got)
	}
	if got := string(p[26 : 26+len(gate.To)]); got != gate.To {
		t.Fatalf("+26 map = %q, want %q", got, gate.To)
	}
	for i := 26 + len(gate.To); i < 67; i++ {
		if p[i] != 0 {
			t.Fatalf("+%d is %d, want the NUL that pads the 41-byte map name", i, p[i])
		}
	}
	for _, tc := range []struct {
		off  int
		want float32
		name string
	}{
		{67, gate.Lat, "lat"},
		{71, gate.Long, "long"},
		{75, gate.ZoomMin, "zoom_min"},
		{79, gate.ZoomMax, "zoom_max"},
		{83, gate.InitZoom, "init_zoom"},
	} {
		got := math.Float32frombits(binary.LittleEndian.Uint32(p[tc.off:]))
		if got != tc.want {
			t.Fatalf("+%d %s = %v, want the table's %v", tc.off, tc.name, got, tc.want)
		}
	}
	if got := int32(binary.LittleEndian.Uint32(p[87:])); got != gate.TargetX {
		t.Fatalf("+87 startPos x = %d, want %d", got, gate.TargetX)
	}
	if got := int32(binary.LittleEndian.Uint32(p[91:])); got != gate.TargetY {
		t.Fatalf("+91 startPos y = %d, want %d", got, gate.TargetY)
	}
	if got := int32(binary.LittleEndian.Uint32(p[95:])); got != *gate.TargetCell {
		t.Fatalf("+95 startPos CELL = %d, want %d (a triangle index, never the height %d)",
			got, *gate.TargetCell, gate.TargetZ)
	}
	if got := p[99]; got != gate.TargetDirByte {
		t.Fatalf("+99 startDir = %d, want the BYTE %d (from %d degrees)",
			got, gate.TargetDirByte, gate.TargetDir)
	}
	if got := p[100]; got != 0 {
		t.Fatalf("+100 channel = %d, want 0 (the destination's channel)", got)
	}

	// The 3593 that started it is not repeated by the answer, and a second
	// CZ_MOVE_ZONE_OK must not answer twice.
	if out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt}); err != nil {
		t.Fatalf("Reply: %v", err)
	} else if len(out) != 0 {
		t.Fatalf("a repeated MOVE_ZONE_OK answered %d packet(s), want none", len(out))
	}

	// One line that PROVES what a real gate puts on the wire, so a future reader can
	// compare it with geZCPacket.h without decoding the test.
	t.Logf("3594 for map 15 -> %s: %d bytes | +2 zoneInstID %d | +6 ip %q | +22 port %d | "+
		"+26 map %q | +67 camera %.0f/%.0f/%.0f/%.0f/%.0f | +87 startPos (%d, %d, cell %d) | "+
		"+99 dir %d | +100 channel %d",
		gate.To, len(p), binary.LittleEndian.Uint32(p[2:]), string(p[6:22]),
		binary.LittleEndian.Uint32(p[22:]), string(p[26:26+len(gate.To)]),
		math.Float32frombits(binary.LittleEndian.Uint32(p[67:])),
		math.Float32frombits(binary.LittleEndian.Uint32(p[71:])),
		math.Float32frombits(binary.LittleEndian.Uint32(p[75:])),
		math.Float32frombits(binary.LittleEndian.Uint32(p[79:])),
		math.Float32frombits(binary.LittleEndian.Uint32(p[83:])),
		int32(binary.LittleEndian.Uint32(p[87:])), int32(binary.LittleEndian.Uint32(p[91:])),
		int32(binary.LittleEndian.Uint32(p[95:])), p[99], p[100])

	// The handoff the answer left behind names the same point the packet does.
	path := TransferPath(transfers, a.sess.AccountID)
	tf, err := ReadTransfer(path, a.sess.AccountID, gate.ToID, time.Now())
	if err != nil {
		t.Fatalf("the handoff the 3594 was built with is not usable: %v", err)
	}
	if tf.X != gate.TargetX || tf.Y != gate.TargetY || tf.Cell != *gate.TargetCell {
		t.Fatalf("handoff point = (%d, %d, %d), want (%d, %d, %d)", tf.X, tf.Y, tf.Cell,
			gate.TargetX, gate.TargetY, *gate.TargetCell)
	}
	if tf.Dir != gate.TargetDirByte {
		t.Fatalf("handoff dir = %d, want the packet's %d", tf.Dir, gate.TargetDirByte)
	}
}

// TestHandoffArrivesAtTheGateExit is the handoff end to end, with no sockets: the
// source zone leaves the exit point, and the DESTINATION zone's START_GAME puts the
// team on it - in the map's own formation shape - instead of on the town spawn.
func TestHandoffArrivesAtTheGateExit(t *testing.T) {
	gate := coimGate(t)
	if gate.TargetCell == nil {
		t.Fatalf("gate %s has no arrival cell in the table", gate.Name)
	}
	points := zoneTable(t)
	landing, err := points.SpawnFor(gate.ToID)
	if err != nil {
		t.Fatalf("SpawnFor(%d): %v", gate.ToID, err)
	}

	// The source zone: walk into the gate and accept the map change.
	src, transfers := gateHandler(t, gate)
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{insideGate(gate), insideGate(gate), insideGate(gate)})
	a.sess.AccountID = 7000001
	if out := src.gateTick(a.sess); len(out) != 2 {
		t.Fatalf("the source zone did not start the map change")
	}
	pt := pack(CmdCZMoveZoneOK)
	if out, err := src.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt}); err != nil {
		t.Fatalf("Reply: %v", err)
	} else if len(out) != 1 {
		t.Fatalf("the source zone sent %d packet(s), want the 3594", len(out))
	}

	// The destination zone: same handoff folder, its own map, its own spawn.
	dst := &Handler{
		Log: log.New(io.Discard, "", 0), MapID: gate.ToID,
		Spawn: landing.Pos, SpawnFormation: landing.Formation,
		Transfers: transfers,
	}
	b := newTeam([]uint32{0x40000051, 0x40000052, 0x40000053},
		[][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	start := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	if out, err := dst.Reply(context.Background(), b.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(start), Data: start}); err != nil {
		t.Fatalf("destination START_GAME: %v", err)
	} else if len(out) == 0 {
		t.Fatal("destination START_GAME sent nothing")
	}
	if b.sess.AccountID != 7000001 {
		t.Fatalf("the destination read account %d from START_GAME", b.sess.AccountID)
	}

	// Slot 0 IS the gate's exit. The followers keep the map's V shape, moved onto it,
	// and every slot carries the exit's cell - the emitter's own rule.
	exit := [3]int32{gate.TargetX, gate.TargetY, *gate.TargetCell}
	if got := b.sess.Team[0].Position; got != exit {
		t.Fatalf("the leader arrived at %v, want the gate's exit %v (the town spawn is %v)",
			got, exit, landing.Pos)
	}
	if got := b.sess.Pos; got != exit {
		t.Fatalf("the session stands at %v, want the gate's exit %v", got, exit)
	}
	if len(landing.Formation) < 3 {
		t.Fatalf("map %d carries %d formation slot(s), want 3", gate.ToID,
			len(landing.Formation))
	}
	for i := 1; i < 3; i++ {
		want := [3]int32{
			exit[0] + (landing.Formation[i].X - landing.Pos[0]),
			exit[1] + (landing.Formation[i].Y - landing.Pos[1]),
			exit[2],
		}
		if got := b.sess.Team[i].Position; got != want {
			t.Fatalf("character %d arrived at %v, want its slot of the V at %v", i, got, want)
		}
	}

	// Used once, deleted: the next entry must not be moved to a gate it did not walk
	// through.
	path := TransferPath(transfers, 7000001)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the used handoff file is still there (%v)", err)
	}
}

// TestReadTransferRefusals pins the four conditions as separate answers, because each
// one is fixed a different way and a silent "ignored" hides which.
func TestReadTransferRefusals(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	path := TransferPath(dir, 7000001)
	if !strings.HasSuffix(path, filepath.Join("", "transfer_7000001.json")) &&
		!strings.Contains(path, "transfer_7000001.json") {
		t.Fatalf("handoff path %q does not name the account", path)
	}

	// Nothing written yet: the normal entry from the barrack.
	if _, err := ReadTransfer(path, 7000001, 208, now); err == nil {
		t.Fatal("a missing handoff file was accepted")
	} else if !strings.Contains(err.Error(), "no handoff file") {
		t.Fatalf("a missing file says %v", err)
	}

	fresh := Transfer{AccountID: 7000001, MapID: 208, X: -17316, Y: 12711, Cell: 1,
		Dir: 128, At: float64(now.UnixNano()) / 1e9}
	if err := WriteTransfer(path, fresh); err != nil {
		t.Fatalf("WriteTransfer: %v", err)
	}

	// The fresh, matching file is used - and NOT deleted by the reader: the caller
	// uses the point first, then calls RemoveTransfer.
	got, err := ReadTransfer(path, 7000001, 208, now)
	if err != nil {
		t.Fatalf("a fresh handoff was refused: %v", err)
	}
	if got != fresh {
		t.Fatalf("handoff = %+v, want %+v", got, fresh)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the reader deleted the file before it was used: %v", err)
	}

	// Another account: a second player's file must never place this one.
	if _, err := ReadTransfer(path, 7000002, 208, now); err == nil {
		t.Fatal("a handoff for another account was accepted")
	} else if !strings.Contains(err.Error(), "account") {
		t.Fatalf("the wrong-account refusal says %v", err)
	}

	// Another map: the gate you walked through is not the map you asked for.
	if _, err := ReadTransfer(path, 7000001, 15, now); err == nil {
		t.Fatal("a handoff for another map was accepted")
	} else if !strings.Contains(err.Error(), "map") {
		t.Fatalf("the wrong-map refusal says %v", err)
	}

	// Stale: a file from a crashed session must not teleport anyone.
	old := fresh
	old.At = float64(now.Add(-2*TransferMaxAge).UnixNano()) / 1e9
	if err := WriteTransfer(path, old); err != nil {
		t.Fatalf("WriteTransfer: %v", err)
	}
	if _, err := ReadTransfer(path, 7000001, 208, now); err == nil {
		t.Fatal("a stale handoff was accepted")
	} else if !strings.Contains(err.Error(), "old") {
		t.Fatalf("the stale refusal says %v", err)
	}

	// A HEIGHT in the cell slot is refused by the same guard as the spawn table.
	height := fresh
	height.Cell = -1088
	if err := WriteTransfer(path, height); err != nil {
		t.Fatalf("WriteTransfer: %v", err)
	}
	if _, err := ReadTransfer(path, 7000001, 208, now); err == nil {
		t.Fatal("a height in the cell slot was accepted")
	} else if !strings.Contains(err.Error(), "HEIGHT") {
		t.Fatalf("the bad-cell refusal says %v", err)
	}

	if err := RemoveTransfer(path); err != nil {
		t.Fatalf("RemoveTransfer: %v", err)
	}
	if err := RemoveTransfer(path); err != nil {
		t.Fatalf("removing an already gone handoff is not an error: %v", err)
	}
}

// ---- warp NPCs: the green arrows -----------------------------------------

// map15NPCs reads the REAL NPC file for Coimbra. The two green arrows and their Script
// column are the data this feature exists to serve, so a copy written into this file
// would only prove the test was edited with it.
func map15NPCs(t *testing.T) []NPC {
	t.Helper()
	path := filepath.Join("..", "..", "data", "zone_npcs", "map15.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no NPC file at %s (run: cd src && python emit_zone_npcs.py)", path)
	}
	doc, err := LoadNPCs(path)
	if err != nil {
		t.Fatalf("LoadNPCs: %v", err)
	}
	return doc.NPCs
}

// arrow answers one of Coimbra's NPCs by its Script column.
func arrow(t *testing.T, script string) NPC {
	t.Helper()
	for _, n := range map15NPCs(t) {
		if n.Script == script {
			return n
		}
	}
	t.Fatalf("map 15 no longer carries an NPC with script %q; the data changed", script)
	return NPC{}
}

// warpHandler builds a zone serving map 15 with the town's NPCs and NO datatable gate, so
// anything that fires can only have come from an NPC's script.
func warpHandler(t *testing.T, zones *world.Registry) (*Handler, string) {
	t.Helper()
	transfers := t.TempDir()
	return &Handler{
		Log: log.New(io.Discard, "", 0), MapID: 15, MapName: "coim_basic",
		NPCs:       map15NPCs(t),
		ZonePoints: zoneTable(t),
		Zones:      zones,
		Transfers:  transfers,
		// The real Warp table: a door's `type` is its row's ClassID, and without the
		// table there is no arrow to draw (see warpPointFor).
		Warps: warpTypes(t),
	}, transfers
}

// TestWarpScriptNamesAMap is the resolver: the two arrows' scripts, the candidate that
// wins for each, and the scripts that are NOT doors.
func TestWarpScriptNamesAMap(t *testing.T) {
	h := &Handler{ZonePoints: zoneTable(t)}

	for _, tc := range []struct {
		script  string
		class   string
		mapID   int
		triedAt int // 1-based position in the candidate list that won
	}{
		{"WARP_OFFICE_COIM", "office_coim", 6004, 1},
		{"TR_COIM_EMILIA_LABORATORY", "mission_emilia_laboratory", 19225, 4},
	} {
		class, mapID, ok := h.warpMapName(NPC{Script: tc.script})
		if !ok {
			t.Fatalf("%s resolved to no map; by_name has %s", tc.script, tc.class)
		}
		if class != tc.class || mapID != tc.mapID {
			t.Fatalf("%s -> %q (map %d), want %q (map %d)", tc.script, class, mapID,
				tc.class, tc.mapID)
		}
		cands := warpCandidates(strings.ToLower(strings.TrimPrefix(tc.script,
			warpScriptPrefixOf(tc.script))))
		if len(cands) < tc.triedAt || cands[tc.triedAt-1] != class {
			t.Fatalf("%s won on candidate %d, which is %v (want %q at %d)", tc.script,
				tc.triedAt, cands, class, tc.triedAt)
		}
		t.Logf("%s -> %q = map %d, candidate %d of %v", tc.script, class, mapID,
			tc.triedAt, cands)
	}

	// The candidate list itself, in order, for the one script whose winning name is NOT
	// simply its own tail.
	want := "coim_emilia_laboratory emilia_laboratory mission_coim_emilia_laboratory mission_emilia_laboratory"
	if got := strings.Join(warpCandidates("coim_emilia_laboratory"), " "); got != want {
		t.Fatalf("candidates for coim_emilia_laboratory = %q, want %q", got, want)
	}

	// Not doors: no warp prefix at all, a prefixed script that names nothing, and empty.
	for _, script := range []string{"C1_COIM_EMILIA", "SHOP_MARKET", "TR_BOOK_15",
		"TR_BACHA_SUB9", ""} {
		if class, mapID, ok := h.warpMapName(NPC{Script: script}); ok {
			t.Fatalf("%q is not a warp but resolved to %q = map %d", script, class, mapID)
		}
	}

	// The prefix is stripped ONCE, and the match is case-insensitive: the Script column
	// is the client's spelling, not the table's.
	if name, ok := warpScriptName("warp_office_coim"); !ok || name != "office_coim" {
		t.Fatalf("warp_office_coim -> %q/%v, want office_coim/true", name, ok)
	}
	if _, ok := warpScriptName("C1_COIM_EMILIA"); ok {
		t.Fatal("a script with no warp prefix was read as a warp")
	}
}

// warpScriptPrefixOf answers the prefix warpScriptName would strip, for the test above.
func warpScriptPrefixOf(script string) string {
	for _, p := range warpScriptPrefixes {
		if strings.HasPrefix(script, p) {
			return p
		}
	}
	return ""
}

// TestWarpNPCOfficeArrowIsBuiltFromTheData: WARP_OFFICE_COIM becomes a MapGate whose
// arrival is office_coim's OWN spawn, slot 0 of its formation, and whose camera is the
// one the client's table holds for that map.
func TestWarpNPCOfficeArrowIsBuiltFromTheData(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	if npc.X != -8771 || npc.Y != 5529 {
		t.Fatalf("%s stands at (%d, %d), want the data's (-8771, 5529)",
			npc.ClassType, npc.X, npc.Y)
	}
	points := zoneTable(t)
	h := &Handler{Log: log.New(io.Discard, "", 0), ZonePoints: points}

	gate, reason, ok := h.warpGate(npc)
	if !ok {
		t.Fatalf("WARP_OFFICE_COIM was refused: %s", reason)
	}
	if gate.To != "office_coim" || gate.ToID != 6004 {
		t.Fatalf("the arrow leads to %q (map %d), want office_coim (6004)", gate.To, gate.ToID)
	}
	landing, err := points.SpawnFor(6004)
	if err != nil {
		t.Fatalf("SpawnFor(6004): %v", err)
	}
	if landing.Pos != [3]int32{0, -500, 18} {
		t.Fatalf("office_coim's spawn moved to %v; the test below asserts this point",
			landing.Pos)
	}
	if gate.TargetX != landing.Pos[0] || gate.TargetY != landing.Pos[1] {
		t.Fatalf("startPos = (%d, %d), want the map's own spawn (%d, %d)",
			gate.TargetX, gate.TargetY, landing.Pos[0], landing.Pos[1])
	}
	if gate.TargetCell == nil || *gate.TargetCell != landing.Pos[2] {
		t.Fatalf("startPos cell = %v, want the spawn's %d", gate.TargetCell, landing.Pos[2])
	}
	if gate.TargetDirByte != 0 || gate.TargetDir != 0 {
		t.Fatalf("startDir = byte %d / %d degrees, want 0", gate.TargetDirByte,
			gate.TargetDir)
	}
	if gate.Range != WarpNPCRange {
		t.Fatalf("trigger radius = %d, want %d", gate.Range, WarpNPCRange)
	}
	// The camera is office_coim's, donated by the row that leaves it: the same five
	// numbers the client's own Warp(pc, 5005) row carries for that map.
	if gate.Lat != 25 || gate.Long != -297 || gate.ZoomMin != 200 ||
		gate.ZoomMax != 2200 || gate.InitZoom != 1000 {
		t.Fatalf("camera = %v/%v/%v/%v/%v, want 25/-297/200/2200/1000",
			gate.Lat, gate.Long, gate.ZoomMin, gate.ZoomMax, gate.InitZoom)
	}
}

// TestWarpNPCFiresOnceAndNamesTheSpawn: the arrow starts the SAME handshake a gate does,
// once per entry, and its 3594 carries office_coim's spawn as startPos.
func TestWarpNPCFiresOnceAndNamesTheSpawn(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h, transfers := warpHandler(t, liveZone(t, 6004, 7004))
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{npc.X, npc.Y, npc.Cell}, {npc.X, npc.Y, npc.Cell}, {npc.X, npc.Y, npc.Cell}})
	a.sess.AccountID = 7000001

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("standing on the arrow sent %d warp notice(s), want one ZC_ENTER_WARPPOINT",
			len(warp))
	}
	if len(out) != 1 {
		t.Fatalf("standing on the arrow sent %d packet(s), want 1 (ZC_MOVE_ZONE)", len(out))
	}
	if got := cmdOf(out[0]); got != CmdZCMoveZone {
		t.Fatalf("cmd = %d, want %d", got, CmdZCMoveZone)
	}
	if a.sess.Gate == nil {
		t.Fatal("the session does not remember the map change it started")
	}
	// Standing still must not fire again.
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("a second tick on the same arrow sent %d packet(s), want none", len(out))
	}

	pt := pack(CmdCZMoveZoneOK)
	answer, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(answer) != 1 {
		t.Fatalf("MOVE_ZONE_OK answered %d packet(s), want 1", len(answer))
	}
	p := answer[0]
	if len(p) != MoveZoneOKSize || cmdOf(p) != CmdZCMoveZoneOK {
		t.Fatalf("the answer is %d bytes of cmd %d, want %d bytes of %d", len(p), cmdOf(p),
			MoveZoneOKSize, CmdZCMoveZoneOK)
	}
	if got := binary.LittleEndian.Uint32(p[mzInstID:]); got != 6004 {
		t.Fatalf("+2 zoneInstID = %d, want 6004 (office_coim)", got)
	}
	if got := binary.LittleEndian.Uint32(p[mzPort:]); got != 7004 {
		t.Fatalf("+22 zonePort = %d, want the announcing zone's 7004", got)
	}
	if got := string(p[mzMap : mzMap+len("office_coim")]); got != "office_coim" {
		t.Fatalf("+26 map = %q, want office_coim", got)
	}
	if x, y, cell := int32(binary.LittleEndian.Uint32(p[mzPos:])),
		int32(binary.LittleEndian.Uint32(p[mzPos+4:])),
		int32(binary.LittleEndian.Uint32(p[mzPos+8:])); x != 0 || y != -500 || cell != 18 {
		t.Fatalf("+87 startPos = (%d, %d, cell %d), want office_coim's spawn "+
			"(0, -500, cell 18)", x, y, cell)
	}
	if p[mzDir] != 0 {
		t.Fatalf("+99 startDir = %d, want 0", p[mzDir])
	}
	t.Logf("3594 from the arrow: +2 %d | +22 %d | +26 %q | +87 (%d, %d, cell %d) | +99 %d",
		binary.LittleEndian.Uint32(p[mzInstID:]), binary.LittleEndian.Uint32(p[mzPort:]),
		string(p[mzMap:mzMap+11]), int32(binary.LittleEndian.Uint32(p[mzPos:])),
		int32(binary.LittleEndian.Uint32(p[mzPos+4:])),
		int32(binary.LittleEndian.Uint32(p[mzPos+8:])), p[mzDir])

	// The handoff is the gate's own, unchanged: same file, same four checks, deleted
	// once used, and the destination zone puts the team on the spawn it names.
	path := TransferPath(transfers, a.sess.AccountID)
	if _, err := ReadTransfer(path, a.sess.AccountID, 6004, time.Now()); err != nil {
		t.Fatalf("the handoff the 3594 was built with is not usable: %v", err)
	}
	landing, err := zoneTable(t).SpawnFor(6004)
	if err != nil {
		t.Fatalf("SpawnFor(6004): %v", err)
	}
	dst := &Handler{
		Log: log.New(io.Discard, "", 0), MapID: 6004,
		Spawn: landing.Pos, SpawnFormation: landing.Formation, Transfers: transfers,
	}
	b := newTeam([]uint32{0x40000051, 0x40000052, 0x40000053},
		[][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	start := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	if out, err := dst.Reply(context.Background(), b.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(start), Data: start}); err != nil {
		t.Fatalf("destination START_GAME: %v", err)
	} else if len(out) == 0 {
		t.Fatal("destination START_GAME sent nothing")
	}
	if got := b.sess.Team[0].Position; got != landing.Pos {
		t.Fatalf("the team arrived at %v, want office_coim's spawn %v", got, landing.Pos)
	}
	if _, err := ReadTransfer(path, a.sess.AccountID, 6004, time.Now()); err == nil {
		t.Fatal("the handoff survived a START_GAME; it must be used once")
	}
}

// TestWarpNPCRefusesWithoutALiveZone: the arrow names a map nobody serves, so there is no
// ip and port for 3594. Refusing BEFORE 3593 is what keeps the client out of a handshake
// that would never be answered - the very freeze this feature exists to end.
func TestWarpNPCRefusesWithoutALiveZone(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h, _ := warpHandler(t, liveZone(t, 15, 7002)) // a live zone, but not for 6004
	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the refused arrow answered %d warp notice(s), want one ZC_ENTER_WARPPOINT",
			len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("an arrow with no live destination sent %d packet(s), want none", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("an arrow with no live destination started a map change")
	}
	// A refusal is once per entry, not once per tick.
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("the refusal repeated itself with %d packet(s)", len(out))
	}

	// No registry at all is the same answer, and not a panic.
	h.Zones = nil
	a.sess.gateInside = 0
	warp, out = splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the forced re-entry answered %d warp notice(s), want one "+
			"ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("a zone with no registry sent %d packet(s), want none", len(out))
	}
}

// TestWarpNPCRefusesAnInstanceMap: the Emilia laboratory arrow resolves to
// mission_emilia_laboratory, a MISSION map created per party as an instance. There is no
// process to send anyone to, so the arrow stays shut: 3593 is never sent, and neither is
// the enter notice - no Warp row leads to that instance, so this door is not a warp POINT
// at all (nothing to draw, nothing to enter), only a trigger the script refuses.
func TestWarpNPCRefusesAnInstanceMap(t *testing.T) {
	npc := arrow(t, "TR_COIM_EMILIA_LABORATORY")
	if npc.X != -7801 || npc.Y != 7295 {
		t.Fatalf("%s stands at (%d, %d), want (-7801, 7295)", npc.ClassType, npc.X, npc.Y)
	}
	// Even with a zone announcing that map, the refusal stands for THIS reason: the
	// instance could not be entered, so the packet would name a world that does not exist.
	h, _ := warpHandler(t, liveZone(t, 19225, 7005))
	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 0 {
		t.Fatalf("the laboratory arrow answered %d warp notice(s), want none - no Warp "+
			"row leads to a per-party instance", len(warp))
	}
	if len(out) != 0 {
		t.Fatalf("the laboratory arrow sent %d packet(s), want none", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("an instanced mission map started a map change")
	}
	_, reason, ok := h.warpGate(npc)
	if ok {
		t.Fatal("warpGate accepted an instanced mission map")
	}
	if !strings.Contains(reason, "INSTANCED") || !strings.Contains(reason, "19225") {
		t.Fatalf("the refusal does not say it is an instance of map 19225: %s", reason)
	}
}

// TestWarpNPCReArmsAfterWalkingOut: the same rule the gates use. One arrow, one 3593 per
// entry, and walking out and back in fires again.
func TestWarpNPCReArmsAfterWalkingOut(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h, _ := warpHandler(t, liveZone(t, 6004, 7004))
	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})

	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the first entry sent %d warp notice(s), want one ZC_ENTER_WARPPOINT",
			len(warp))
	}
	if len(out) != 1 {
		t.Fatalf("the first entry sent %d packet(s), want 1", len(out))
	}
	a.sess.Gate = nil // the client never answered; the session is free to try again
	a.sess.Pos = [3]int32{npc.X + WarpNPCRange + 200, npc.Y, npc.Cell}
	left, out := splitWarp(h.gateTick(a.sess))
	if len(left) != 1 || cmdOf(left[0]) != CmdZCLeaveWarpPoint {
		t.Fatalf("walking off the arrow sent %d warp notice(s), want one "+
			"ZC_LEAVE_WARPPOINT", len(left))
	}
	if len(out) != 0 {
		t.Fatalf("a tick outside the arrow sent %d packet(s), want none", len(out))
	}
	if a.sess.gateInside != 0 {
		t.Fatalf("walking out left the door armed (gateInside %d)", a.sess.gateInside)
	}
	if a.sess.warpInside != 0 {
		t.Fatalf("walking out left the warp point armed (warpInside %d)", a.sess.warpInside)
	}
	a.sess.Pos = [3]int32{npc.X, npc.Y, npc.Cell}
	if out := h.gateTick(a.sess); len(out) != 2 {
		t.Fatalf("walking back in sent %d packet(s), want 2", len(out))
	}
}

// TestWarpNPCArrivalOnAnArrowDoesNotBounce: office_coim's gate out lands on
// (-8771, 5529), which is the pixel WARP_OFFICE_COIM stands on. Without arming the arrow
// at START_GAME the player would be sent back to the office the instant they arrived,
// for ever.
func TestWarpNPCArrivalOnAnArrowDoesNotBounce(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h, _ := warpHandler(t, liveZone(t, 6004, 7004))
	h.Spawn = [3]int32{npc.X, npc.Y, npc.Cell}
	h.SpawnFormation = nil

	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})
	start := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	if _, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(start), Data: start}); err != nil {
		t.Fatalf("START_GAME: %v", err)
	}
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("the tick right after arrival sent %d packet(s) - it would bounce the "+
			"player straight back", len(out))
	}
	if a.sess.Gate != nil {
		t.Fatal("the arrow the team landed on started a map change")
	}

	// Stepping out and back in is a real map change. Walking OUT sends nothing here on
	// purpose: the arrival armed this door without telling the client it had walked into
	// it, so there is no enter notice for a leave notice to pair with.
	standAt(a.sess, [3]int32{npc.X + WarpNPCRange + 50, npc.Y, npc.Cell})
	if out := h.gateTick(a.sess); len(out) != 0 {
		t.Fatalf("a tick outside the arrow sent %d packet(s), want none", len(out))
	}
	standAt(a.sess, [3]int32{npc.X, npc.Y, npc.Cell})
	if out := h.gateTick(a.sess); len(out) != 2 {
		t.Fatalf("walking back onto the arrow sent %d packet(s), want 2", len(out))
	}
}

// TestWarpNPCIsNotADialog: a door runs no click script. The client's entry for these
// arrows is SCR_ENTER_<Script>, and neither has a SCR_CLICK_ in the corpus, so running
// the click path would answer the player with nothing - or start a second, half-done
// warp from inside a script.
func TestWarpNPCIsNotADialog(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h := &Handler{
		Log: log.New(io.Discard, "", 0), MapID: 15,
		NPCs:       map15NPCs(t),
		ZonePoints: zoneTable(t),
		Click: func(entry string, self, pc ClickObject) (ClickReply, error) {
			t.Fatalf("the click script %s was run on a zone warp NPC", entry)
			return ClickReply{}, nil
		},
	}
	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})
	if out := h.openNPCDialog(a.sess, npc); len(out) != 0 {
		t.Fatalf("clicking a warp NPC sent %d packet(s), want none", len(out))
	}

	// A normal NPC on the same map is untouched by the gate: its script still runs.
	var normal NPC
	for _, n := range map15NPCs(t) {
		if n.Script == "C1_COIM_EMILIA" {
			normal = n
			break
		}
	}
	if normal.Handle == 0 {
		t.Skip("map 15 no longer carries C1_COIM_EMILIA; nothing to compare with")
	}
	ran := ""
	h.Click = func(entry string, self, pc ClickObject) (ClickReply, error) {
		ran = entry
		return ClickReply{}, nil
	}
	a.sess.Pos = [3]int32{normal.X, normal.Y, normal.Cell}
	if out := h.openNPCDialog(a.sess, normal); len(out) != 0 {
		t.Fatalf("a dialog NPC with an empty reply sent %d packet(s)", len(out))
	}
	if ran != "SCR_CLICK_C1_COIM_EMILIA" {
		t.Fatalf("a normal NPC ran %q, want SCR_CLICK_C1_COIM_EMILIA", ran)
	}
}

// ---- the warp POINT: the arrow, its enter notice and its leave --------------

// TestWarpPointPacketLayout pins the WHOLE 32 bytes of 3540 and 3541 and the 8 of 3542.
// Every offset is asserted, because a single wrong one moves every field after it: the
// arrow then appears somewhere else (or nowhere) and the enter notice names a handle the
// client never saw.
//
// The layout is MEASURED, not source2's: see the wpPos comment in gate.go for why the
// position is at +15 and not +6 (the client's own table says 32, its 3543 handler reads
// pos at +15, direction at +27, and 28+4 = 32).
func TestWarpPointPacketLayout(t *testing.T) {
	pos := [3]int32{-8771, 5529, 40}
	for _, tc := range []struct {
		name string
		cmd  uint16
		pkt  []byte
	}{
		{"spawn", CmdZCSpawnWarpPoint, BuildSpawnWarpPoint(0x7F000002, pos, 0, 5005)},
		{"enter", CmdZCEnterWarpPoint, BuildEnterWarpPoint(0x7F000002, pos, 0, 5005)},
	} {
		p := tc.pkt
		if len(p) != WarpPointSize {
			t.Fatalf("%s is %d bytes, want %d", tc.name, len(p), WarpPointSize)
		}
		if got := cmdOf(p); got != tc.cmd {
			t.Fatalf("%s cmd = %d, want %d", tc.name, got, tc.cmd)
		}
		if got := binary.LittleEndian.Uint32(p[wpHandle:]); got != 0x7F000002 {
			t.Fatalf("%s handle = 0x%08x, want 0x7f000002", tc.name, got)
		}
		for i, want := range pos {
			if got := int32(binary.LittleEndian.Uint32(p[wpPos+4*i:])); got != want {
				t.Fatalf("%s pos[%d] = %d, want %d", tc.name, i, got, want)
			}
		}
		if p[wpDirection] != 0 {
			t.Fatalf("%s direction = %d, want 0", tc.name, p[wpDirection])
		}
		if got := binary.LittleEndian.Uint32(p[wpType:]); got != 5005 {
			t.Fatalf("%s type = %d, want the Warp row 5005", tc.name, got)
		}
		// The 9 bytes source2 has and the shipped client ignores must be zero, not
		// leftovers: the client's handler skips them, and a future one may not.
		for i := wpHandle + 4; i < wpPos; i++ {
			if p[i] != 0 {
				t.Fatalf("%s byte +%d = %d, want 0 (the ignored gap)", tc.name, i, p[i])
			}
		}
		t.Logf("%s: % x", tc.name, p)
	}
}

// TestWarpLeaveSize: 3542 is command(2) handle(4) code(2) and nothing else - the client's
// handler reads exactly those and calls geactor::Leave.
func TestWarpLeaveSize(t *testing.T) {
	p := BuildLeaveWarpPoint(0x7F000002, 0)
	if len(p) != WarpLeaveSize {
		t.Fatalf("ZC_LEAVE_WARPPOINT is %d bytes, want %d", len(p), WarpLeaveSize)
	}
	if got := cmdOf(p); got != CmdZCLeaveWarpPoint {
		t.Fatalf("cmd = %d, want %d", got, CmdZCLeaveWarpPoint)
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != 0x7F000002 {
		t.Fatalf("handle = 0x%08x, want 0x7f000002", got)
	}
	if got := binary.LittleEndian.Uint16(p[6:]); got != 0 {
		t.Fatalf("code = %d, want 0", got)
	}
}

// warpTypes loads the REAL Warp table. A copy written into this file would only prove the
// test was edited with it: the ClassIDs are the client's own row ids.
func warpTypes(t *testing.T) *WarpTypeTable {
	t.Helper()
	dir := XMLDir()
	if _, err := os.Stat(filepath.Join(dir, "datatable_warp.xml")); err != nil {
		t.Skipf("no warp table at %s", filepath.Join(dir, "datatable_warp.xml"))
	}
	warps, err := LoadWarpTypes(dir)
	if err != nil {
		t.Fatalf("LoadWarpTypes(%s): %v", dir, err)
	}
	return warps
}

// TestWarpTypeIsTheRowClassID: the `type` a warp point carries is the ClassID of its Warp
// row - the client looks the row up by that number to draw the arrow
// (CBaseObject::WarpPointEnterWorld). A gate is a row named by its ClassName; an arrow NPC
// is the row that leads from this map to the arrow's destination.
func TestWarpTypeIsTheRowClassID(t *testing.T) {
	warps := warpTypes(t)

	// Coimbra's own gate row, and the row for the office arrow in coim_basic.
	if got, ok := warps.TypeOf("COIM_BASIC-FLD_COIM"); !ok || got != 10017 {
		t.Fatalf("COIM_BASIC-FLD_COIM -> %d (%v), want 10017", got, ok)
	}
	if got, ok := warps.TypeForArrow("coim_basic", "office_coim"); !ok || got != 5005 {
		t.Fatalf("coim_basic -> office_coim -> %d (%v), want 5005", got, ok)
	}
	// A row that names no create zone answers a doorway only when ONE row targets it.
	if got, ok := warps.TypeForArrow("", "office_coim"); !ok || got != 5005 {
		t.Fatalf("the script-driven door to office_coim -> %d (%v), want 5005", got, ok)
	}
	if _, ok := warps.TypeOf("no-such-warp-row"); ok {
		t.Fatal("a name that is not in the table answered a ClassID")
	}
}

// TestEntrySpawnsOneWarpPointPerDoor: the entry burst puts one 3540 on every door of the
// map - the two datatable gates and the two arrow NPCs - each with its own handle, its own
// position, and the row's type. Without these the client draws no arrow at all.
func TestEntrySpawnsOneWarpPointPerDoor(t *testing.T) {
	points := zoneTable(t)
	gates := points.Gates(15)
	if len(gates) == 0 {
		t.Fatalf("map 15 no longer carries a gate; the table changed")
	}
	h := &Handler{
		Log: log.New(io.Discard, "", 0), MapID: 15, MapName: "coim_basic",
		Gates: gates, NPCs: map15NPCs(t), ZonePoints: points, Warps: warpTypes(t),
	}

	out := h.spawnWarpPoints()
	arrows := 0
	for _, n := range h.NPCs {
		if _, ok := h.warpPointFor(len(gates)+1, MapGate{}, n, true); ok && isWarpScript(n.Script) {
			arrows++
		}
	}
	if want := len(gates) + arrows; len(out) != want {
		t.Fatalf("entry spawned %d warp point(s), want %d (%d gate(s) + %d arrow(s))",
			len(out), want, len(gates), arrows)
	}

	seen := map[uint32]bool{}
	for _, p := range out {
		if len(p) != WarpPointSize {
			t.Fatalf("a spawned warp point is %d bytes, want %d", len(p), WarpPointSize)
		}
		if got := cmdOf(p); got != CmdZCSpawnWarpPoint {
			t.Fatalf("a spawned warp point is cmd %d, want %d", got, CmdZCSpawnWarpPoint)
		}
		handle := binary.LittleEndian.Uint32(p[wpHandle:])
		if handle < WarpPointHandleBase {
			t.Fatalf("handle 0x%08x is below the warp point range - it could collide with "+
				"an NPC (0x40000000+) or a character", handle)
		}
		if seen[handle] {
			t.Fatalf("two warp points share handle 0x%08x", handle)
		}
		seen[handle] = true
	}

	// Every gate that HAS a row in the Warp table carries that row's ClassID. A gate the
	// table does not name, and an arrow to a map no row serves (the Emilia laboratory is a
	// per-party instance), carry 0 - which the spawn line logs as a guess.
	named := 0
	for i, g := range gates {
		typ, ok := h.Warps.TypeOf(g.Name)
		got := binary.LittleEndian.Uint32(out[i][wpType:])
		if ok {
			named++
			if int(got) != typ {
				t.Fatalf("gate %s carries type %d, its Warp row is %d", g.Name, got, typ)
			}
		} else if got != 0 {
			t.Fatalf("gate %s carries type %d but the table names no row for it", g.Name, got)
		}
	}
	if named == 0 {
		t.Fatalf("no gate of map 15 is named in the Warp table; the lookup is broken")
	}

	// The gate's own arrow is at the gate's own point, with its row's ClassID.
	first := out[0]
	if got := int32(binary.LittleEndian.Uint32(first[wpPos:])); got != gates[0].X {
		t.Fatalf("the first gate's arrow is at x %d, want %d", got, gates[0].X)
	}
	if got := int32(binary.LittleEndian.Uint32(first[wpPos+8:])); got != *gates[0].Cell {
		t.Fatalf("the first gate's arrow carries cell %d, want %d", got, *gates[0].Cell)
	}
}

// TestWarpPointEnterNamesTheSpawnedHandle: 3541 must name the SAME handle 3540 did, or the
// client has two warp points and no idea which one it walked into. Then it re-arms on
// leaving, which is what a second visit needs.
func TestWarpPointEnterNamesTheSpawnedHandle(t *testing.T) {
	npc := arrow(t, "WARP_OFFICE_COIM")
	h, _ := warpHandler(t, liveZone(t, 6004, 7004))
	h.MapName = "coim_basic"
	h.Warps = warpTypes(t)
	a := newTeam([]uint32{0x40000031}, [][3]int32{{npc.X, npc.Y, npc.Cell}})

	spawned := h.spawnWarpPoints()
	if len(spawned) != 1 {
		t.Fatalf("map 15's doors spawned %d warp point(s), want 1 - the office arrow "+
			"(the laboratory arrow leads to a per-party instance and draws nothing)", len(spawned))
	}
	want := binary.LittleEndian.Uint32(spawned[0][wpHandle:])
	wantType := binary.LittleEndian.Uint32(spawned[0][wpType:])

	warp, rest := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("the entry sent %d warp notice(s), want one ZC_ENTER_WARPPOINT", len(warp))
	}
	if got := binary.LittleEndian.Uint32(warp[0][wpHandle:]); got != want {
		t.Fatalf("the enter notice names handle 0x%08x, the spawn said 0x%08x", got, want)
	}
	if got := binary.LittleEndian.Uint32(warp[0][wpType:]); got != wantType {
		t.Fatalf("the enter notice carries type %d, the spawn said %d", got, wantType)
	}
	if len(rest) != 1 || cmdOf(rest[0]) != CmdZCMoveZone {
		t.Fatalf("the entry sent %d move-zone packet(s), want one 3593", len(rest))
	}
	if a.sess.warpInside == 0 {
		t.Fatal("the session does not remember the warp point it entered")
	}

	// Out and back in: a leave notice for the SAME handle, then a fresh enter notice.
	a.sess.Gate = nil
	a.sess.Pos = [3]int32{npc.X + WarpNPCRange + 100, npc.Y, npc.Cell}
	warp, rest = splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCLeaveWarpPoint {
		t.Fatalf("walking off the arrow sent %d warp notice(s), want one "+
			"ZC_LEAVE_WARPPOINT", len(warp))
	}
	if got := binary.LittleEndian.Uint32(warp[0][2:]); got != want {
		t.Fatalf("the leave notice names handle 0x%08x, the spawn said 0x%08x", got, want)
	}
	if len(rest) != 0 {
		t.Fatalf("walking off the arrow sent %d other packet(s)", len(rest))
	}

	a.sess.Pos = [3]int32{npc.X, npc.Y, npc.Cell}
	warp, rest = splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("walking back sent %d warp notice(s), want one ZC_ENTER_WARPPOINT", len(warp))
	}
	if len(rest) != 1 {
		t.Fatalf("walking back sent %d move-zone packet(s), want 1", len(rest))
	}
}

// TestMoveZoneIsThreeBytesWithAFalseFlag pins the WHOLE of ZC_MOVE_ZONE (3593): command
// word 3593, then ONE bool byte, and that byte is 0x00.
//
// The authority is the live server, not source2: D:\ge_server\captures\warp_ref\20261027-200136\
// capture.txt line 593 is the live server sending the three bytes `09 0e 00` - 0x0e09 =
// 3593, then 0x00 - and lines 594/595 are the client acking (3686) and the server
// answering (3594) in the same millisecond.
//
// This byte was the gate blocker: sent as 0x01, the shipped client answered nothing and
// stopped writing to its own socket (docker-port\logs\zone15_err.txt, 23:13:27). The
// byte is ToWorldPVPServer, and the live server leaves it FALSE for a normal map change.
func TestMoveZoneIsThreeBytesWithAFalseFlag(t *testing.T) {
	p := BuildMoveZone()
	if len(p) != MoveZoneSize {
		t.Fatalf("ZC_MOVE_ZONE is %d bytes, want %d", len(p), MoveZoneSize)
	}
	if len(p) != 3 {
		t.Fatalf("ZC_MOVE_ZONE is %d bytes, the client's table says 3", len(p))
	}
	if got := cmdOf(p); got != uint16(CmdZCMoveZone) {
		t.Fatalf("cmd = %d, want %d (ZC_MOVE_ZONE)", got, CmdZCMoveZone)
	}
	// `09 0e 00` on the wire: byte 0 is 0x09, byte 1 is 0x0e, byte 2 is 0x00.
	if p[0] != 0x09 || p[1] != 0x0e {
		t.Fatalf("command word is %#02x %#02x, the live capture says 0x09 0x0e", p[0], p[1])
	}
	if p[2] != 0x00 {
		t.Fatalf("the flag byte is %#02x, want 0x00 - the live server sends `09 0e 00` "+
			"(D:\\ge_server\\captures\\warp_ref\\20261027-200136\\capture.txt line 593). With a 1 the "+
			"shipped client answers nothing and stops sending, so the gate never moves", p[2])
	}
}

// TestMoveZoneAckIsTheOnlyFinish is the one 3593 -> one 3594 -> one handoff rule, and the
// proof that NOTHING ELSE can finish a map change.
//
// Three things are checked, and the third is the important one:
//  1. one CZ_MOVE_ZONE_OK (3686) produces exactly one 3594 and exactly one handoff file;
//  2. a repeated 3686 produces nothing, and a further tick produces nothing;
//  3. NO TIMEOUT PATH EXISTS. The handshake is aged by a whole hour and the tick is run
//     many times: it must send nothing and leave no handoff. The old fallback waited one
//     second and then completed the move the client had refused - which is exactly what
//     hid the 3593 flag bug. There is also no timer constant left to reference: if
//     moveZoneOKWait still existed, this file would name it here.
func TestMoveZoneAckIsTheOnlyFinish(t *testing.T) {
	gate := coimGate(t)
	if gate.Range <= 100 {
		t.Fatalf("the gate's range is %d, too small for this test to stand inside it",
			gate.Range)
	}
	h, transfers, _ := gateHandlerLog(t, gate)
	a := newTeam([]uint32{0x40000031}, [][3]int32{insideGate(gate)})
	a.sess.AccountID = 7000001

	out := h.gateTick(a.sess)
	if len(out) != 2 {
		t.Fatalf("standing in gate %s sent %d packet(s), want exactly 2 (3541 then 3593)",
			gate.Name, len(out))
	}
	if cmdOf(out[0]) != CmdZCEnterWarpPoint || cmdOf(out[1]) != CmdZCMoveZone {
		t.Fatalf("the two packets are cmd %d then %d, want %d then %d",
			cmdOf(out[0]), cmdOf(out[1]), CmdZCEnterWarpPoint, CmdZCMoveZone)
	}
	if a.sess.Gate == nil {
		t.Fatal("the first tick did not remember the map change it started")
	}

	// (3) NO TIMER: make the pending move look a whole hour old, then let the tick run
	// many times. Silence, and no handoff file.
	a.sess.Gate.since = time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		if early := h.gateTick(a.sess); len(early) != 0 {
			t.Fatalf("tick %d after an hour without an ack sent %d packet(s), want 0 - "+
				"only the client's own CZ_MOVE_ZONE_OK may finish a move", i+1, len(early))
		}
	}
	if n := len(handoffsIn(t, transfers)); n != 0 {
		t.Fatalf("a handoff file was left without any ack (%d files) - the destination "+
			"zone could read a move the client never accepted", n)
	}
	if a.sess.Gate == nil {
		t.Fatal("a tick without an ack dropped the pending map change")
	}

	// (1) THE ACK: exactly one 3594, exactly one handoff, and the move is no longer
	// pending.
	pt := pack(CmdCZMoveZoneOK)
	ack, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("answering MOVE_ZONE_OK: %v", err)
	}
	if len(ack) != 1 {
		t.Fatalf("the ack sent %d packet(s), want exactly one 3594", len(ack))
	}
	if got := cmdOf(ack[0]); got != uint16(CmdZCMoveZoneOK) {
		t.Fatalf("the ack answered with cmd %d, want %d (ZC_MOVE_ZONE_OK)", got, CmdZCMoveZoneOK)
	}
	if len(ack[0]) != MoveZoneOKSize {
		t.Fatalf("the 3594 is %d bytes, want %d", len(ack[0]), MoveZoneOKSize)
	}
	if a.sess.Gate != nil {
		t.Fatal("the ack finished the move but left it pending on the session - the next " +
			"tick could send a second 3594")
	}
	if n := len(handoffsIn(t, transfers)); n != 1 {
		t.Fatalf("the ack left %d handoff file(s), want exactly 1", n)
	}

	// (2) A REPEATED ack and further ticks change nothing.
	again, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZMoveZoneOK, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("answering MOVE_ZONE_OK a second time: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a second 3686 sent %d packet(s), want 0 - one 3593 gets one 3594",
			len(again))
	}
	for i := 0; i < 5; i++ {
		if extra := h.gateTick(a.sess); len(extra) != 0 {
			t.Fatalf("tick %d after the ack sent %d packet(s), want 0", i+1, len(extra))
		}
	}
	if n := len(handoffsIn(t, transfers)); n != 1 {
		t.Fatalf("the handoff count is %d after a repeated ack and more ticks, want 1 - it "+
			"must not be written twice for one move", n)
	}
}

// TestWarpGateFiresOnArrivalNotOnClick is the bug the player reported as "the warp arrow
// did not wait for my character - the moment I click the area it warps me to the next
// zone". CZ_MOVE_PC carries the click's DESTINATION (geZCPacket.h:1133), so a report that
// names the arrow says where the player POINTED, not where the character stands: it is
// 2000 units away. The tick must stay silent until the walk has covered that distance,
// and then fire exactly once.
func TestWarpGateFiresOnArrivalNotOnClick(t *testing.T) {
	gate := coimGate(t)
	if gate.Range <= 100 {
		t.Fatalf("the gate's range is %d, too small for this test to stand inside it",
			gate.Range)
	}
	h, _ := gateHandler(t, gate)

	// The character STANDS 2000 units from the arrow and the player clicks the arrow, so
	// the report's pos is the arrow's own point - the bug's exact input.
	cell := cellOrZero(gate.Cell)
	far := [3]int32{gate.X + 2000, gate.Y, cell}
	dest := insideGate(gate)
	a := newTeam([]uint32{0x40000031}, [][3]int32{far})

	body := []byte{0} // charIndex 0 = the leader
	body = appendU32(body, uint32(dest[0]))
	body = appendU32(body, uint32(dest[1]))
	body = appendU32(body, uint32(dest[2]))
	click := pack(CmdCZMovePC, body...)
	if _, err := h.movePC(a.sess,
		protocol.Frame{Cmd: CmdCZMovePC, Size: len(click), Data: click}); err != nil {
		t.Fatalf("MOVE_PC: %v", err)
	}

	// HALF ONE: the click has only just arrived. Nothing may fire, because the character
	// has not walked anywhere yet.
	away := math.Hypot(float64(far[0]-gate.X), float64(far[1]-gate.Y))
	for i := 0; i < 3; i++ {
		for _, p := range h.gateTick(a.sess) {
			if c := cmdOf(p); c == CmdZCEnterWarpPoint || c == CmdZCMoveZone {
				t.Fatalf("tick %d, with the character %.0f units from the arrow, sent cmd "+
					"%d - the warp fired on the CLICK, not on the arrival", i+1, away, c)
			}
		}
	}
	if a.sess.Gate != nil {
		t.Fatal("a map change started while the character was still 2000 units away")
	}

	// HALF TWO: the walk has had time. The segment is 1900 units long, which at the
	// base speed (469) takes about 4.1 s, so a start 6 s old is past its end - the walk
	// has arrived and walkedPos clamps to the destination.
	a.sess.moveAt = time.Now().Add(-6 * time.Second)
	warp, out := splitWarp(h.gateTick(a.sess))
	if len(warp) != 1 || cmdOf(warp[0]) != CmdZCEnterWarpPoint {
		t.Fatalf("on arrival the tick sent %d warp notice(s), want one 3541", len(warp))
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCMoveZone {
		t.Fatalf("on arrival the tick sent %d map-change packet(s), want one 3593", len(out))
	}
	if again := h.gateTick(a.sess); len(again) != 0 {
		t.Fatalf("the tick after the arrival sent %d packet(s), want 0 - one per entry",
			len(again))
	}
}
