package barrack

import (
	"context"
	"encoding/binary"
	"testing"

	"geserver/protocol"
)

// fakeBarrack replaces PostgreSQL for the substitution test.
type fakeBarrack struct {
	cmdr   CommanderRow
	chars  []CharInfo
	groups []TeamGroup
	found  bool
	failed error
	// saved, when set, collects every SaveZoneEntry call so a test can read back
	// what the handler recorded.
	saved *[]ZoneEntry
	// teamSaved, when set, collects every SaveTeamPositions call.
	teamSaved *[][3]uint16
}

func (f fakeBarrack) Commander(context.Context, uint32) (CommanderRow, bool, error) {
	return f.cmdr, f.found, f.failed
}

func (f fakeBarrack) Characters(context.Context, uint32) ([]CharInfo, error) {
	return f.chars, f.failed
}

func (f fakeBarrack) TeamGroups(context.Context, uint32) ([]TeamGroup, error) {
	return f.groups, f.failed
}

func (f fakeBarrack) SaveZoneEntry(_ context.Context, e ZoneEntry) error {
	if f.failed != nil {
		return f.failed
	}
	if f.saved != nil {
		*f.saved = append(*f.saved, e)
	}
	return nil
}

// SaveTeamPositions records the team the handler persisted onto the rows.
func (f fakeBarrack) SaveTeamPositions(_ context.Context, _ uint32, slots [3]uint16) error {
	if f.failed != nil {
		return f.failed
	}
	if f.teamSaved != nil {
		*f.teamSaved = append(*f.teamSaved, slots)
	}
	return nil
}

// ExtendTeamGroup is here so fakeBarrack keeps satisfying BarrackStore. It does
// NOT model the bag: the Extend-button tests use extendStore (extend_group_test.go),
// which does - a canned answer here would let a handler test pass while the real
// rule was wrong.
func (f fakeBarrack) ExtendTeamGroup(context.Context, uint32, uint32) (ExtendResult, error) {
	return ExtendResult{Outcome: ExtendAtCap, Slot: f.cmdr.TeamMax}, f.failed
}

// TestStartBarrackSubstitutesBuiltPackets proves the captured burst keeps its
// ORDER while the four per-player packets are replaced by built ones. The order
// is load-bearing (the client crashes otherwise), so position must not move.
func TestStartBarrackSubstitutesBuiltPackets(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	burst, err := LoadReplay(replayLiveDir)
	if err != nil {
		t.Fatalf("LoadReplay: %v", err)
	}
	h.AfterStartBarrack = burst
	h.Barrack = fakeBarrack{
		found: true,
		cmdr:  CommanderRow{ID: 24, Name: "Arcanine", ZoneID: 100, CharSlot: 4, TeamMax: 3},
		chars: []CharInfo{{Name: "Vex", Job: 7, Level: 89}},
	}

	sess := &Session{CommanderID: 24}
	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// Packets referencing the other account are dropped; everything else keeps
	// the captured order. Build the expectation the same way the handler does.
	var wantCmds []uint16
	seen66 := 0
	for _, p := range burst {
		cmd := binary.LittleEndian.Uint16(p)
		// burstSkip is the current measurement; while it is on, those packets are
		// not expected in the output either.
		if unpbuildable[cmd] || burstSkip[cmd] {
			continue
		}
		// The probe also trims the job list to the nine copies the working server sends.
		if cmd == 66 {
			seen66++
			if seen66 > 9 {
				continue
			}
		}
		wantCmds = append(wantCmds, cmd)
	}
	if len(out) != len(wantCmds) {
		t.Fatalf("sent %d packets, want %d", len(out), len(wantCmds))
	}
	for i, p := range out {
		if got := binary.LittleEndian.Uint16(p); got != wantCmds[i] {
			t.Fatalf("packet %d is cmd %d, captured order has cmd %d", i, got, wantCmds[i])
		}
	}

	// And the four per-player ones must now be OUR sizes, not the capture's.
	for i, p := range out {
		switch binary.LittleEndian.Uint16(p) {
		case 10:
			if len(p) != CommanderInfoSize {
				t.Fatalf("packet %d (cmd 10) = %d bytes, want %d", i, len(p), CommanderInfoSize)
			}
			if got := trimNul(string(p[2:])); got != "Arcanine" {
				t.Fatalf("built commander name = %q, want Arcanine", got)
			}
		case 16:
			want := BarrackInfoHeaderLen + 1*PCInfoSize // one character in the fake store
			if len(p) != want {
				t.Fatalf("packet %d (cmd 16) = %d bytes, want %d", i, len(p), want)
			}
			if p[4] != 1 {
				t.Fatalf("built barrack says %d characters, want 1", p[4])
			}
		case 17:
			if len(p) != EquipExtraHeaderLen {
				t.Fatalf("packet %d (cmd 17) = %d bytes, want %d", i, len(p), EquipExtraHeaderLen)
			}
		case 26:
			if len(p) != EmptyItemListLen {
				t.Fatalf("packet %d (cmd 26) = %d bytes, want %d", i, len(p), EmptyItemListLen)
			}
		case 37:
			// The family level, built from the characters' levels. The capture's
			// own answer is 89 (the other account); one level-89 character here
			// happens to give the same, so check the SIZE and that the packet is
			// ours - the level itself is pinned by commander_level_test.go.
			if len(p) != CommanderLvInfoSize {
				t.Fatalf("packet %d (cmd 37) = %d bytes, want %d", i, len(p), CommanderLvInfoSize)
			}
		case 41:
			// The team groups. The fake store has none, so this must be the
			// EMPTY form - and in particular not the capture's 145-byte list of
			// another account's team names.
			if len(p) != TeamGroupHeader {
				t.Fatalf("packet %d (cmd 41) = %d bytes, want the empty %d",
					i, len(p), TeamGroupHeader)
			}
			if p[4] != 0 {
				t.Fatalf("packet %d (cmd 41) says %d groups, want 0", i, p[4])
			}
		case 57:
			// The equipment presets. FIXED size, so it must be sent at exactly the
			// registered 866 bytes whether or not this player has any presets.
			if len(p) != EquipPresetInfoSize {
				t.Fatalf("packet %d (cmd 57) = %d bytes, want %d",
					i, len(p), EquipPresetInfoSize)
			}
			for j := 2; j < len(p); j++ {
				if p[j] != 0 {
					t.Fatalf("packet %d (cmd 57) body byte %d = 0x%02x, want 0 - "+
						"the capture's residue must not be replayed", i, j-2, p[j])
				}
			}
		case 65:
			// The preset-extend items: zeros, because no item layer exists yet.
			// preset_remain_test.go pins the layout against the capture.
			if len(p) != PresetRemainCountSize {
				t.Fatalf("packet %d (cmd 65) = %d bytes, want %d",
					i, len(p), PresetRemainCountSize)
			}
		case 35:
			// The channel list. It MUST NOT be empty: a zero count disables New
			// Game and Continue Game in the client.
			if len(p) != ChannelListHeader+ChannelInfoSize {
				t.Fatalf("packet %d (cmd 35) = %d bytes, want %d",
					i, len(p), ChannelListHeader+ChannelInfoSize)
			}
			if p[4] == 0 {
				t.Fatalf("packet %d (cmd 35) has no channels - that greys out "+
					"New Game in the barrack", i)
			}
		}
	}
}

// TestStartBarrackWithoutAStoreStillAnswers: with no barrack store (a probe run) a
// family-less session is still answered - with the family trigger alone, never with the
// capture's burst (that burst belongs to an account that HAS a family).
func TestStartBarrackWithoutAStoreStillAnswers(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	burst, err := LoadReplay(replayLiveDir)
	if err != nil {
		t.Fatalf("LoadReplay: %v", err)
	}
	h.AfterStartBarrack = burst

	out, err := h.Reply(context.Background(), &Session{}, protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s) - a family-less session gets the no-commander message alone", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != systemMessageCmd {
		t.Fatalf("the answer is cmd %d, want %d (BC_MESSAGE)", cmd, systemMessageCmd)
	}
}
