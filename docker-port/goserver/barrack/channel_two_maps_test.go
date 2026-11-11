package barrack

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"geserver/protocol"
)

// THE TWO-MAP GUARD, no client needed.
//
// The barrack names two maps: cmd 10's newMapID (where a NEW GAME starts: the SAVED
// start point) and contMapID (where CONTINUE resumes: the LAST point). cmd 35 must
// carry an entry for EACH of them, with `count` = the number of entries and the two
// zone counts saying how many channel numbers each map offers.
//
// On 2026-10-29 the barrack named two maps while cmd 35 carried ONE entry, built for
// the last point's map only. CSelectTeamFrame::UpdateChannel asks
// Session().GetChannelInfo(i, GetNewMapType()) for every i below newZoneChannelCount
// (source2\geUI\source\UISelectTeamFrame.cpp:326-327) and GetChannelInfo scans the
// whole list for `mapID == mapID && channel == channel`
// (source2\geClientShared\source\GeSession.cpp:2384-2406). The new-game map's entry
// was simply not there. ge.exe then died at 0x004ba478 (0xc0000005, 21:31:20).
//
// THE ENTRY ORDER CARRIES NO MEANING, and that is proven, not assumed: the client
// never indexes the list by position. It passes the loop index i as the CHANNEL
// NUMBER and searches the whole list for that (map, channel) pair. So the two entries
// are pinned byte for byte here for reproducibility, but the rule they satisfy is "one
// entry per map, each with the channel number the player is on".
const (
	lastMapID  uint16 = 10 // reb_basic - commander.zone_id, the LAST point
	saveMapID  uint16 = 15 // coim_basic - the SAVED start point
	saveWarpID uint16 = 213
	lastWarpID uint16 = 203

	// commanderMapsAt is where cmd 10 holds newMapID: cmd(2) name(17) party(17)
	// profile(251) title(9) rank(2) charSlot(2) commanderPoint(4) ladderPoint(2).
	commanderMapsAt = 2 + commanderNameLen + partyNameLen + profileLen +
		partyTitleLen + 2 + 2 + 4 + 2
)

// channelEntry is one CHANNEL_INFO record: WORD mapID, BYTE channel, BYTE traffic.
type channelEntry struct {
	MapID   uint16
	Channel byte
	Traffic byte
}

// readEntry decodes entry number i of a cmd 35.
func readEntry(list []byte, i int) channelEntry {
	off := ChannelListHeader + i*ChannelInfoSize
	return channelEntry{
		MapID:   binary.LittleEndian.Uint16(list[off:]),
		Channel: list[off+2],
		Traffic: list[off+3],
	}
}

// stopWarpMaps is the reverse warp table the barrack gets in production, faked: the
// real one is zone\ZonePoints.RestartWarpMap over data\zone_points.json, and
// zone\r70_test.go pins these very two pairs (213 -> 15, 203 -> 10).
func stopWarpMaps(warpID uint16) (uint16, bool) {
	switch warpID {
	case saveWarpID:
		return saveMapID, true
	case lastWarpID:
		return lastMapID, true
	}
	return 0, false
}

// twoMapHandler is the real handler with PostgreSQL and the client replaced.
func twoMapHandler(t *testing.T, cmdr CommanderRow) *Handler {
	t.Helper()
	h := quietHandler(t, fakeStore{ok: true})
	h.Barrack = fakeBarrack{
		found: true,
		cmdr:  cmdr,
		chars: []CharInfo{{Slot: 0, Name: "Arcis"}},
	}
	h.WarpMaps = stopWarpMaps
	// A burst that carries cmd 10, which is what makes startBarrack emit the built
	// commander info. cmd 35 in a burst is DROPPED on purpose (burstSkip[35]): a
	// barrack's channel list belongs to the answer to CB_REQ_BARRACK_INFO, LAST.
	h.AfterStartBarrack = [][]byte{{10, 0}, {35, 0}}
	return h
}

// findCmd returns the first packet in out whose command is cmd.
func findCmd(t *testing.T, out [][]byte, cmd uint16) []byte {
	t.Helper()
	cmds := make([]uint16, 0, len(out))
	for _, p := range out {
		if len(p) < 2 {
			continue
		}
		if got := binary.LittleEndian.Uint16(p); got == cmd {
			return p
		} else {
			cmds = append(cmds, got)
		}
	}
	t.Fatalf("no cmd %d among the %d packet(s) sent (cmds %v)", cmd, len(out), cmds)
	return nil
}

// channelListOf drives the CB_REQ_BARRACK_INFO path - the path the client really gets
// its channel list from - and returns that cmd 35.
func channelListOf(t *testing.T, h *Handler, sess *Session) []byte {
	t.Helper()
	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 2})
	if err != nil {
		t.Fatalf("REQ_BARRACK_INFO: %v", err)
	}
	return findCmd(t, out, 35)
}

// TestBarrackTwoMapsCarryBothChannelEntries is the guard the crash needed.
//
// It checks the three things the fix promises: (a) cmd 10 names two DIFFERENT maps,
// (b) cmd 35 has count 2, new 1, cont 1 and one entry per map, and (c) the single-map
// case is still count 1, new 1, cont 1.
func TestBarrackTwoMapsCarryBothChannelEntries(t *testing.T) {
	t.Run("saved town differs from last town", func(t *testing.T) {
		h := twoMapHandler(t, CommanderRow{
			ID: 25, Name: "test01", ZoneID: lastMapID, SaveWarpID: saveWarpID,
			ChannelID: 0,
		})
		sess := &Session{CommanderID: 25}

		out, err := h.startBarrack(context.Background(), sess)
		if err != nil {
			t.Fatalf("startBarrack: %v", err)
		}
		info := findCmd(t, out, 10)
		newMapID := binary.LittleEndian.Uint16(info[commanderMapsAt:])
		contMapID := binary.LittleEndian.Uint16(info[commanderMapsAt+2:])

		// (a) Two different maps, the right way round: the SAVE point feeds newMapID
		// (the client dereferences that map's name with no null check) and the LAST
		// point feeds contMapID.
		if newMapID != saveMapID {
			t.Fatalf("cmd 10 newMapID = %d, want %d (the saved start point, warp %d)",
				newMapID, saveMapID, saveWarpID)
		}
		if contMapID != lastMapID {
			t.Fatalf("cmd 10 contMapID = %d, want %d (the last point)", contMapID, lastMapID)
		}
		if newMapID == contMapID {
			t.Fatalf("cmd 10 names ONE map (%d) twice - the two-map case is not wired",
				newMapID)
		}

		// (b) cmd 35 must carry an entry for BOTH maps, or the drop-down for the
		// missing one comes out empty and its button does nothing.
		list := channelListOf(t, h, sess)
		if got, want := len(list), ChannelListHeader+2*ChannelInfoSize; got != want {
			t.Fatalf("cmd 35 is %d bytes, want %d (header + 2 entries)", got, want)
		}
		if got := binary.LittleEndian.Uint16(list[2:]); int(got) != len(list) {
			t.Fatalf("cmd 35 length word = %d, want %d (the whole packet)", got, len(list))
		}
		if list[4] != 2 {
			t.Fatalf("cmd 35 count = %d, want 2 (one entry per map)", list[4])
		}
		if list[5] != 1 {
			t.Fatalf("cmd 35 newZoneChannelCount = %d, want 1", list[5])
		}
		if list[6] != 1 {
			t.Fatalf("cmd 35 continueZoneChannelCount = %d, want 1", list[6])
		}

		want := []channelEntry{
			{MapID: saveMapID, Channel: 0, Traffic: 0},
			{MapID: lastMapID, Channel: 0, Traffic: 0},
		}
		for i, w := range want {
			if got := readEntry(list, i); got != w {
				t.Fatalf("cmd 35 entry %d = %+v, want %+v (the new-game map's entry "+
					"first, then the continue map's, both on the player's channel)",
					i, got, w)
			}
		}
		t.Logf("two-map cmd 35 = %s", hexBytes(list))
	})

	t.Run("no saved start point stays single-map", func(t *testing.T) {
		h := twoMapHandler(t, CommanderRow{
			ID: 24, Name: "Arcis", ZoneID: lastMapID, SaveWarpID: 0, ChannelID: 0,
		})
		sess := &Session{CommanderID: 24}

		out, err := h.startBarrack(context.Background(), sess)
		if err != nil {
			t.Fatalf("startBarrack: %v", err)
		}
		info := findCmd(t, out, 10)
		if got := binary.LittleEndian.Uint16(info[commanderMapsAt:]); got != lastMapID {
			t.Fatalf("cmd 10 newMapID = %d, want the last point's map %d (never 0: the "+
				"client does not null-check it)", got, lastMapID)
		}
		if got := binary.LittleEndian.Uint16(info[commanderMapsAt+2:]); got != lastMapID {
			t.Fatalf("cmd 10 contMapID = %d, want %d", got, lastMapID)
		}

		// (c) One entry, and all three count bytes 1 - the shape this build shipped.
		list := channelListOf(t, h, sess)
		if got, want := len(list), ChannelListHeader+ChannelInfoSize; got != want {
			t.Fatalf("cmd 35 is %d bytes, want %d (header + 1 entry)", got, want)
		}
		if list[4] != 1 || list[5] != 1 || list[6] != 1 {
			t.Fatalf("cmd 35 counts = %d/%d/%d, want 1/1/1", list[4], list[5], list[6])
		}
		if e := readEntry(list, 0); e.MapID != lastMapID || e.Channel != 0 {
			t.Fatalf("cmd 35 entry = %+v, want map %d channel 0", e, lastMapID)
		}
	})

	t.Run("a warp id no map carries falls back to the last point", func(t *testing.T) {
		// Commander 24's real row: point 3090, zone_id 15.
		h := twoMapHandler(t, CommanderRow{
			ID: 24, Name: "Arcis", ZoneID: saveMapID, SaveWarpID: 3090, ChannelID: 0,
		})
		sess := &Session{CommanderID: 24}

		out, err := h.startBarrack(context.Background(), sess)
		if err != nil {
			t.Fatalf("startBarrack: %v", err)
		}
		info := findCmd(t, out, 10)
		if got := binary.LittleEndian.Uint16(info[commanderMapsAt:]); got != saveMapID {
			t.Fatalf("cmd 10 newMapID = %d, want the last point's map %d (warp 3090 "+
				"resolves to nothing, and 0 is not a map)", got, saveMapID)
		}
		list := channelListOf(t, h, sess)
		if list[4] != 1 || list[5] != 1 || list[6] != 1 {
			t.Fatalf("cmd 35 counts = %d/%d/%d, want 1/1/1", list[4], list[5], list[6])
		}
	})
}

// hexBytes formats a packet so a failure log shows the real bytes.
func hexBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return fmt.Sprint(parts)
}
