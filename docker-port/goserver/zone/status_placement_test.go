package zone

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"testing"

	"geserver/protocol"
)

// TestStatusBurstRidesGameReadyNotStartGame pins WHERE the per-character 3684
// burst goes, because sending it in the wrong reply costs NOTHING visible: the
// client's ZC_UPDATE_ALL_STATUS handler exits silently while its record-count
// gate is still 0, which is the state during the CZ_START_GAME reply. That is
// what the owner saw on 2026-10-29 - the six BASE stats (they travel in
// START_INFO) drawn next to 0 for every DERIVED field, while the zone's own log
// printed real numbers.
//
// Both halves are asserted, so a later edit cannot quietly put it back: ABSENT
// from the START_GAME reply, PRESENT in the GAME_READY reply, one packet per team
// member, in team order, right after the world-entry handshake. Without the
// "absent" half the bug would come back as a double send with no test to stop it.
func TestStatusBurstRidesGameReadyNotStartGame(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	p := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1, 2, 1}, {3, 4, 1}, {5, 6, 1}})

	// -- CZ_START_GAME: the entry packet and the entry burst, and NO 3684 ------
	start := pack(CmdCZStartGame, appendU32(appendU32(nil, 42), 7000001)...)
	out, err := h.Reply(context.Background(), p.sess,
		protocol.Frame{Cmd: CmdCZStartGame, Size: len(start), Data: start})
	if err != nil {
		t.Fatalf("START_GAME: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("START_GAME sent nothing")
	}
	if n := countStatusPackets(out); n != 0 {
		t.Fatalf("the START_GAME reply carries %d status packet(s) (%d), want 0: the "+
			"client drops every 3684 it receives there", n, CmdZCUpdateAllStatus)
	}

	// -- CZ_GAME_READY: the handshake, then one 3684 per team member -----------
	ready := pack(CmdCZGameReady)
	out, err = h.Reply(context.Background(), p.sess,
		protocol.Frame{Cmd: CmdCZGameReady, Size: len(ready), Data: ready})
	if err != nil {
		t.Fatalf("GAME_READY: %v", err)
	}
	want := len(p.sess.Team)
	if n := countStatusPackets(out); n != want {
		t.Fatalf("the GAME_READY reply carries %d status packet(s), want %d - one per "+
			"team member", n, want)
	}
	if cmdOf(out[0]) != CmdZCEnterLegacyObjs {
		t.Fatalf("first GAME_READY packet cmd = %d, want the world-entry handshake %d",
			cmdOf(out[0]), CmdZCEnterLegacyObjs)
	}
	// Each team member gets its 3684 AND its 3093, in that order - the sheet and
	// then the promotion rank that turns the blank line under a party card's name
	// into "Master" / "High Master".
	for i := 0; i < want; i++ {
		pkt := out[1+2*i]
		if cmd := cmdOf(pkt); cmd != CmdZCUpdateAllStatus {
			t.Fatalf("GAME_READY packet %d cmd = %d, want %d (the burst must follow the "+
				"handshake, not the town)", 1+2*i, cmd, CmdZCUpdateAllStatus)
		}
		if len(pkt) != UpdateAllStatusSize {
			t.Fatalf("status packet %d is %d bytes, want %d", i, len(pkt),
				UpdateAllStatusSize)
		}
		if got, w := binary.LittleEndian.Uint32(pkt[2:]), p.sess.Team[i].Handle; got != w {
			t.Fatalf("status packet %d names handle %d, want %d (team order)", i, got, w)
		}
		prom := out[2+2*i]
		if cmd := cmdOf(prom); cmd != CmdZCPromotionInfo {
			t.Fatalf("GAME_READY packet %d cmd = %d, want %d (the promotion line)",
				2+2*i, cmd, CmdZCPromotionInfo)
		}
		if len(prom) != PromotionInfoSize {
			t.Fatalf("promotion packet %d is %d bytes, want %d", i, len(prom),
				PromotionInfoSize)
		}
		if got, w := binary.LittleEndian.Uint32(prom[2:]), p.sess.Team[i].Handle; got != w {
			t.Fatalf("promotion packet %d names handle %d, want %d", i, got, w)
		}
	}
}

// countStatusPackets counts the 3684 packets in one reply.
func countStatusPackets(out [][]byte) int {
	n := 0
	for _, pkt := range out {
		if len(pkt) >= 2 && cmdOf(pkt) == CmdZCUpdateAllStatus {
			n++
		}
	}
	return n
}
