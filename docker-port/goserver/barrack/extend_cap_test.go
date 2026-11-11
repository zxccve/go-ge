package barrack

// WHY THIS FILE EXISTS. On 2026-11-08 the owner pressed the Team List's Extend
// button twice and the client refused both times; the button had never sent a
// packet. The cause was OUR value, not the client: cmd 10's `rank` carried the
// commander's own Lock_TeamGroup, and the client's gate is
// `IES("Lock").Lock_TeamGroup < cmd10.rank` (PROVEN, client 0x005426c1 - the
// whole gate is quoted in history\barrack.md "2026-11-08 - the Extend gate").
// Two equal numbers make that test false at every value, at 20 and at 5 alike.
//
// So cmd 10's rank MUST be the cap (GET_TEAMGROUP_MAXSLOT) and cmd 32's Lock
// table MUST keep the commander's CURRENT count. These tests read both numbers
// off the wire the handler produces, and then run the client's own comparison on
// them - a pair that only LOOKS right (rank 20 with a Lock of 20) still fails.

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"geserver/protocol"
)

// rankAt is CommanderInfo.Rank's byte offset in cmd 10, added up from the
// builder's own constants: command (2) + name(17) + party name(17) + profile(251)
// + party title(9) = 296. commander_info_test.go pins the same number.
const rankAt = 2 + commanderNameLen + partyNameLen + profileLen + partyTitleLen

// lockTeamGroupOnWire reads Lock_TeamGroup out of a cmd 32 packet's XML.
func lockTeamGroupOnWire(t *testing.T, p []byte) int {
	t.Helper()
	if len(p) <= updateTableHeader {
		t.Fatalf("cmd 32 is %d bytes, too short to carry an XML body", len(p))
	}
	if got := binary.LittleEndian.Uint16(p); got != 32 {
		t.Fatalf("packet is cmd %d, want 32", got)
	}
	xml := string(p[updateTableHeader:])
	const key = `Lock_TeamGroup = "`
	i := strings.Index(xml, key)
	if i < 0 {
		t.Fatalf("cmd 32's XML carries no %q: %s", key, xml)
	}
	rest := xml[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("cmd 32's Lock_TeamGroup has no closing quote: %s", xml)
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		t.Fatalf("cmd 32's Lock_TeamGroup = %q, which is not a number: %v", rest[:j], err)
	}
	return n
}

// cmd10FromStartBarrack runs one CB_STARTBARRACK through the real handler and
// returns the cmd 10 it built. The burst is two 2-byte stubs: startBarrack
// substitutes a packet it can build for the captured copy, so a stub is enough to
// put cmd 10 (and, when wanted, cmd 32) in the answer without a capture file.
func cmd10FromStartBarrack(t *testing.T, h *Handler, burst [][]byte) ([][]byte, []byte) {
	t.Helper()
	h.AfterStartBarrack = burst
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("STARTBARRACK: %v", err)
	}
	for _, p := range out {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == 10 {
			return out, p
		}
	}
	t.Fatalf("the answer carries no cmd 10 (%d packet(s))", len(out))
	return nil, nil
}

// capHandler is a commander at 7 of the cap 20: the state the owner was in, and
// the state in which the old code's guard was `7 < 7`.
func capHandler(t *testing.T, jp bool) *Handler {
	t.Helper()
	h := quietHandler(t, fakeStore{ok: true})
	h.JP = jp
	h.Barrack = fakeBarrack{
		found: true,
		cmdr: CommanderRow{
			ID: 24, Name: "Arcanine", ZoneID: 10, ChannelID: 1, CharSlot: 4, TeamMax: 7,
		},
		chars: []CharInfo{{Slot: 0, Name: "Vex", Job: 7, Level: 89}},
	}
	return h
}

// TestCmd10sRankIsTheTeamGroupCapOnBothBuilders is the card's first pass
// condition, on BOTH build paths: the v32 744-byte BC_COMMANDER_INFO and the JP
// 880-byte one. The value is read at offset 296 and compared to the game's own
// GET_TEAMGROUP_MAXSLOT(), never to a literal.
func TestCmd10sRankIsTheTeamGroupCapOnBothBuilders(t *testing.T) {
	for _, tc := range []struct {
		name string
		jp   bool
		size int
	}{
		{"v32", false, CommanderInfoSize},
		{"JP", true, CommanderInfoSize + 8*barrackNameLen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := capHandler(t, tc.jp)
			_, p := cmd10FromStartBarrack(t, h, [][]byte{{10, 0}})

			if len(p) != tc.size {
				t.Fatalf("cmd 10 is %d bytes, want %d", len(p), tc.size)
			}
			got := binary.LittleEndian.Uint16(p[rankAt:])
			if got != TeamGroupMaxSlot {
				t.Fatalf("cmd 10's rank at +%d = %d, want GET_TEAMGROUP_MAXSLOT() %d - the "+
					"client's Extend guard is Lock_TeamGroup < rank, so a rank equal to the "+
					"count can never open the button", rankAt, got, TeamGroupMaxSlot)
			}
			// The old bug in one line: the commander's own count must NOT be here.
			if got == h.Barrack.(fakeBarrack).cmdr.TeamMax {
				t.Fatalf("cmd 10's rank = %d, the commander's Lock_TeamGroup - that is the "+
					"n < n the Extend button died on", got)
			}
		})
	}
}

// TestCmd32sLockKeepsTheCommandersCurrentCount is the card's second pass
// condition: cmd 32's Lock_TeamGroup is NOT forced to rank. It carries the
// commander's CURRENT count, from `commander_property.Lock_TeamGroup` (read by the
// store as CommanderRow.TeamMax) - not cmd 10's cap and not the number of saved
// `team_group` rows. A row count would be wrong on this very database, which holds
// Lock_TeamGroup 5 with 0 saved teams: advertising 0 would close the list the
// player is trying to extend. The live captures show the same shape, a Lock of 13
// and 7 against a rank of 20.
func TestCmd32sLockKeepsTheCommandersCurrentCount(t *testing.T) {
	h := capHandler(t, false)
	out, p10 := cmd10FromStartBarrack(t, h, [][]byte{{10, 0}, {32, 0}})

	var p32 []byte
	for _, p := range out {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == 32 {
			p32 = p
		}
	}
	if p32 == nil {
		t.Fatalf("the answer carries no cmd 32 (%d packet(s))", len(out))
	}

	const wantLock = 7 // the commander's Lock_TeamGroup in capHandler
	if got := lockTeamGroupOnWire(t, p32); got != wantLock {
		t.Fatalf("cmd 32's Lock_TeamGroup = %d, want the commander's current count %d "+
			"(commander_property.Lock_TeamGroup)", got, wantLock)
	}
	rank := int(binary.LittleEndian.Uint16(p10[rankAt:]))
	if got := lockTeamGroupOnWire(t, p32); got == rank {
		t.Fatalf("cmd 32's Lock_TeamGroup = %d = cmd 10's rank - the two sides must not "+
			"carry the same number, or the client's `lock < rank` guard is false", got)
	}
	// THE CLIENT'S OWN GUARD, run on the bytes this handler produced.
	if !(wantLock < rank) {
		t.Fatalf("the client would REFUSE the Extend button with Lock_TeamGroup %d and "+
			"rank %d - the guard is `Lock_TeamGroup < rank`", wantLock, rank)
	}
}
