package zone

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// THE REFERENCE'S OWN 26 BYTES, and where they came from.
//
// `de 0d | 1a 06 2a 00 | 3f ab 02 00 | ea fa ff ff | 1c ca ff ff | 15 00 00 00 | 00 00 0a 43`
//
// is ONE ZC_MOVE_MONSTER taken off the LIVE reference server:
// `captures\kill01\20261105-214033\s2c_7026_56237.bin`, file offset **141035**,
// command 3550, 26 bytes, split with the client's own size table
// (`data\cmd_table_live_z.txt:1217` -> `3550  26  ZC_MOVE_MONSTER`).
//
// The fields a capture can settle, decoded with the struct the client's handler
// reads (`PKS_ZC_MOVE`, `source2\geShared\include\geZCPacket.h:1046-1052`):
//
//	+0  WORD  3550                     0x0dde
//	+2  DWORD handle                   2754074   (0x002a061a)
//	+6  DWORD tick                     174911    (0x0002ab3f) - SECONDS of server uptime
//	+10 INT32 x                        -1302
//	+14 INT32 y                        -13796
//	+18 INT32 cell                     21        (the DESTINATION's cell)
//	+22 FLOAT32 speed                  138.0     (0x430a0000)
//
// THE TICK'S UNIT IS MEASURED, not read off a header (2026-11-10): every 3548 and
// 3550 in that capture was paired with the arrival stamp of the chunk that carried
// it (`capture.txt`), and the word advanced **1 per second** over the whole
// 95.111 s recording (88 units between the first and the last move packet of the
// stream). So it is `Plat_FloatTime()`-in-seconds - `FieldChar_FSM.cpp:249-251`
// stamps `m_moveStartTick = Plat_FloatTime()` - and NOT milliseconds.
//
// THIS TEST EXISTS BECAUSE THE OLD ONE COULD NOT FAIL FOR THE RIGHT REASON.
// `TestBuildMoveMonsterIsThe26ByteHeader` (monster_chase_test.go:330) asserts our
// builder's own offsets against our builder's own numbers: that is self-consistency,
// and it passes just as happily on a frame the client would refuse. This one holds
// our bytes against the REFERENCE'S bytes, so an id, a width, an order or a units
// drift fails here.
func TestMoveMonsterIsByteIdenticalToTheLiveReferenceFrame(t *testing.T) {
	live := []byte{
		0xde, 0x0d, // 3550
		0x1a, 0x06, 0x2a, 0x00, // handle 2754074
		0x3f, 0xab, 0x02, 0x00, // tick 174911
		0xea, 0xfa, 0xff, 0xff, // x -1302
		0x1c, 0xca, 0xff, 0xff, // y -13796
		0x15, 0x00, 0x00, 0x00, // cell 21
		0x00, 0x00, 0x0a, 0x43, // speed 138.0f
	}
	if len(live) != 26 {
		t.Fatalf("the fixture is %d bytes, the client's table says 26", len(live))
	}

	got := BuildMoveMonster(2754074, [3]int32{-1302, -13796, 21}, 174911, 138)
	if !bytes.Equal(got, live) {
		t.Fatalf("our 3550 is not the reference's frame\n ours % x\n live % x", got, live)
	}

	// And field by field, so a failure names the field instead of dumping 26 bytes.
	if cmd := binary.LittleEndian.Uint16(got[0:]); cmd != 3550 {
		t.Fatalf("+0 command %d, want 3550 (capture offset 141035)", cmd)
	}
	if h := binary.LittleEndian.Uint32(got[2:]); h != 2754074 {
		t.Fatalf("+2 handle %d, want 2754074", h)
	}
	if tick := binary.LittleEndian.Uint32(got[6:]); tick != 174911 {
		t.Fatalf("+6 tick %d, want 174911", tick)
	}
	if x := int32(binary.LittleEndian.Uint32(got[10:])); x != -1302 {
		t.Fatalf("+10 x %d, want -1302", x)
	}
	if y := int32(binary.LittleEndian.Uint32(got[14:])); y != -13796 {
		t.Fatalf("+14 y %d, want -13796", y)
	}
	if cell := int32(binary.LittleEndian.Uint32(got[18:])); cell != 21 {
		t.Fatalf("+18 cell %d, want 21", cell)
	}
	if spd := math.Float32frombits(binary.LittleEndian.Uint32(got[22:])); spd != 138 {
		t.Fatalf("+22 speed %g, want 138 (a FLOAT32: 0x430a0000 in the capture)", spd)
	}
}

// TestMoveMonsterOfASpawnerWalkIsTheLoggedWalk pins ONE real walk this zone sent,
// field by field, from `logs\zone_btlauch01.log`:
//
//	2026/11/09 07:56:56.294561 zone: MONSTER MOVE 0x26ad8fe1 "Monster_AmokTortoise"
//	walks to the script's point (4518, 5605, cell 316) from (5079, 6170, cell 45)
//	at 276 u/s from SCR_Get_MON_MSPD - cmd 3550 (26 B) to 1 session(s)
//
// THE DESTINATION IS THE WHOLE PAYLOAD. There is no start point in a 3550
// (`PKS_ZC_MOVE` has handle, tick, pos, spd and nothing else), and the client
// passes `NULL` for posStart, so the actor walks from wherever its own body
// stands (`gemove::ActorMove`, `source2\geClientShared\source\GeMove.cpp:17-32`).
// The log's "from (...)" is the zone's own model of the body, never a wire field -
// which is why a body whose model has not advanced cannot mis-place the client's.
//
// THE CELL IS THE DESTINATION'S, and it is a cell of the CLIENT's own mesh: these
// three (x, y, cell) triples answer `valid ... 1` from `navmesh\pathtool.exe` on
// the client's `btl_auch01.tok` through `positionIsValid`, the very predicate the
// client's path agent asks (`imcPathEngine\Agent.cpp:39-47`). The same is true of
// the cells the owner's own client reported for its characters on that map
// (e.g. `(9457, 24667, 139)`, `(11339, 20898, 83)`), so the two sides share one
// cell numbering.
func TestMoveMonsterOfASpawnerWalkIsTheLoggedWalk(t *testing.T) {
	pkt := BuildMoveMonster(0x26ad8fe1, [3]int32{4518, 5605, 316}, 48300, 276)
	if len(pkt) != 26 {
		t.Fatalf("%d bytes, want 26 (cmd_table_live_z.txt:1217)", len(pkt))
	}
	if h := binary.LittleEndian.Uint32(pkt[2:]); h != 0x26ad8fe1 {
		t.Fatalf("+2 handle 0x%08x, want 0x26ad8fe1", h)
	}
	if x := int32(binary.LittleEndian.Uint32(pkt[10:])); x != 4518 {
		t.Fatalf("+10 x %d, want 4518 (the script's point)", x)
	}
	if y := int32(binary.LittleEndian.Uint32(pkt[14:])); y != 5605 {
		t.Fatalf("+14 y %d, want 5605", y)
	}
	if cell := int32(binary.LittleEndian.Uint32(pkt[18:])); cell != 316 {
		t.Fatalf("+18 cell %d, want 316 - the cell of the DESTINATION, which is what "+
			"the reference sends and what the client's path agent validates", cell)
	}
	if spd := math.Float32frombits(binary.LittleEndian.Uint32(pkt[22:])); spd != 276 {
		t.Fatalf("+22 speed %g, want 276 (SCR_Get_MON_MSPD for class 10594)", spd)
	}
}

// TestMoveAndStopShareOneClock is the answer to "does the client need a start
// time we send as 0 / a repeat interval?". It does not need either:
//
//   - we never send 0: the tick is `nowMillis()`, milliseconds since this process
//     started (`zone\move.go:70-74`), and a walking zone is never at 0 by the time
//     a body moves;
//   - the reference's value is SECONDS of uptime (27, below), so our UNIT differs
//     and the VALUE cannot be compared - but the client's 3550 path never reads it:
//     `gemove::ActorMove` receives `tick` and does not use it (`GeMove.cpp:17-41`),
//     and `CActor::MoveTo` has no tick parameter at all (`GeActor.cpp:249-258`);
//   - the same field on the PC's 3548 is read by the SAME function
//     (`CNormalNet::MovePC` -> `gemove::ActorMove(..., packet->tick, ...)`), and our
//     3548s demonstrably walk characters in the shipped client (the NPC dialog walk
//     and the three 3549 warp stops, owner-confirmed 2026-10-22/27). A unit that
//     broke the client would have broken those too.
//
// So the clock is pinned here as ONE clock for the move family - a 3548 and a 3550
// built for the same instant carry the same word - and never as a cause of a body
// not being drawn.
func TestMoveAndStopShareOneClock(t *testing.T) {
	const tick = 174911
	move := BuildMoveMonster(7, [3]int32{1, 2, 3}, tick, 100)
	pc := BuildMovePC(7, [3]int32{1, 2, 3}, tick, 100)
	stop := BuildStopPCAt(7, [3]int32{1, 2, 3}, tick)
	if got := binary.LittleEndian.Uint32(move[6:]); got != tick {
		t.Fatalf("the 3550's +6 is %d; the caller's clock is %d", got, tick)
	}
	if got := binary.LittleEndian.Uint32(pc[6:]); got != tick {
		t.Fatalf("the 3548's +6 is %d; the caller's clock is %d", got, tick)
	}
	// A stop carries the same clock in the same place, so all three packets are one
	// time line - which is what the live capture shows (3548 174908..174969 and
	// 3550 174911..174996 over the same 95 s).
	if got := binary.LittleEndian.Uint32(stop[6:]); got != tick {
		t.Fatalf("the 3549's +6 is %d; the caller's clock is %d", got, tick)
	}
	if nowMillis() == 0 && tick == 0 {
		t.Fatalf("a live zone must not stamp a move with 0")
	}
}

// TestAZeroSpeedWalkIsRefusedNotSent pins the ONE value in a 3550 that the client
// cannot survive, and it is not a layout question: the SPEED.
//
// `CActor::MoveTo` opens with `Assert(fSpeed != 0.0f)` (`GeActor.cpp:250`) and the
// path state it enters advances the body by `m_updateTime * m_moveSpeed`
// (`GeActor.cpp:2254`). At speed 0 the actor is put into a move state that never
// advances and never arrives: the body stands in a frozen walk pose until something
// else stops it. That is the exact symptom "ordered but not drawn".
//
// A live 3550 never carries 0 - 0 of 494 across `captures\kill01\20261105-214033`,
// `captures\kite4000c\20261108-104235` and `captures\chasec20261108\20260808-084508`;
// the range is 67..1419 - and until this test the zone could still build one: a body
// whose class is not in the Monster table and whose ChaseSpeed is 0 took the
// `!known` branch in `orderMonsterMove`, which WARNED and then sent the walk with a
// speed of 0 anyway.
//
// RED: with the `return` in that branch removed, this test reports 1 x 3550 and the
// speed it carries is 0.
func TestAZeroSpeedWalkIsRefusedNotSent(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)
	host.Monster = h

	// A body the table cannot answer: no Monster row at all, and no fallback speed.
	h.Monsters = nil
	bodies := h.spawnedMonster
	for i := range bodies {
		bodies[i].ChaseSpeed = 0
	}
	h.spawnedMonster = bodies

	src := `
		local mon = { Handle = ` + luaNum(mon.Handle) + ` }
		Move(mon, 500, 800, 26)
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != 0 {
		_, _, speed := decodeMonsterMove(t, moves[0])
		t.Fatalf("%d x 3550 for a body with no walk speed (speed %g), want 0 - a "+
			"0-speed move is one the client starts and can never advance "+
			"(GeActor.cpp:250, :2254)\nzone log:\n%s", len(moves), speed, logs.String())
	}
	if n := strings.Count(logs.String(), "NO walk is ordered"); n != 1 {
		t.Fatalf("%d refusal line(s) for one order, want exactly 1:\n%s", n, logs.String())
	}
}
