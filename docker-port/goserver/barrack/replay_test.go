package barrack

import (
	"context"
	"encoding/binary"
	"testing"

	"geserver/protocol"
)

const replayDir = "../../data/replay"

// TestLoadReplay checks the captured packets the live barrack sends after
// LOGINOK: the script-binding table first, then the rotation lists, in the
// manifest's order, each agreeing with its own command header.
func TestLoadReplay(t *testing.T) {
	spec, err := protocol.LoadSpec(barrackTable)
	if err != nil {
		t.Fatalf("load barrack table: %v", err)
	}
	packets, err := LoadReplay(replayDir)
	if err != nil {
		t.Fatalf("LoadReplay: %v", err)
	}
	if len(packets) < 2 {
		t.Fatalf("loaded %d packets, expected the hotkey table and rotation lists", len(packets))
	}

	first := packets[0]
	if cmd := binary.LittleEndian.Uint16(first); cmd != 56 {
		t.Fatalf("first packet command = %d, want 56 (BC_HOTKEY_BIND_INFO)", cmd)
	}
	// The declared length is the whole packet, so it must match the file size.
	if got := int(binary.LittleEndian.Uint16(first[2:])); got != len(first) {
		t.Fatalf("first packet says %d bytes but carries %d", got, len(first))
	}
	if len(first) != 7571 {
		t.Fatalf("hotkey table = %d bytes, want 7571", len(first))
	}

	for i, p := range packets[1:] {
		cmd := binary.LittleEndian.Uint16(p)
		if cmd != 66 {
			t.Fatalf("packet %d command = %d, want 66 (BC_ROTATION_CHAR_LIST)", i+1, cmd)
		}
		// BC_ROTATION_CHAR_LIST is FIXED size in the client's table, so it has
		// NO length word: bytes +2 are payload. Reading a length there returns
		// 8 and looks like a broken capture - it is not.
		if size, ok := spec.Size(66); ok && size != 0 {
			if len(p) != size {
				t.Fatalf("packet %d is %d bytes, the client's table says %d",
					i+1, len(p), size)
			}
		}
	}
}

// TestLoginSendsReplayAfterLoginOK proves the ORDER: the LOGINOK comes first,
// then the static packets, exactly as the live server does it.
func TestLoginSendsReplayAfterLoginOK(t *testing.T) {
	h := quietHandler(t, fakeStore{
		acc: Account{AccountID: 35, CommanderID: 24, MemberID: "arcanine"},
		ok:  true,
	})
	packets, err := LoadReplay(replayDir)
	if err != nil {
		t.Fatalf("LoadReplay: %v", err)
	}
	h.AfterLogin = packets

	out, err := h.Reply(context.Background(), &Session{}, capturedFrame(t))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1+len(packets) {
		t.Fatalf("sent %d packets, want %d", len(out), 1+len(packets))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 8 {
		t.Fatalf("first packet = %d, want 8 (the LOGINOK must come first)", cmd)
	}
	// Byte-for-byte: a replayed static packet must not be altered on the way out.
	for i, want := range packets {
		if string(out[i+1]) != string(want) {
			t.Fatalf("replayed packet %d differs from the capture", i)
		}
	}
}
const replayLiveDir = "../../data/replay_live"
