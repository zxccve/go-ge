package barrack

import (
	"context"
	"encoding/binary"
	"testing"

	"geserver/protocol"
)

// familyWithChars is fakeBarrack plus the character write the create screen needs.
type familyWithChars struct {
	fakeBarrack
	got     protocol.CreateChar
	created bool
}

func (f *familyWithChars) CreateCharacter(_ context.Context, _ uint32, in protocol.CreateChar) (CharInfo, bool, error) {
	f.got = in
	f.created = true
	return CharInfo{Slot: in.Slot, Name: in.Name, Gender: in.Gender != 0, Job: byte(in.Job)}, true, nil
}

// createCharFrame builds the 63-byte CB_CREATE_CHAR the client sends: 10-byte security
// header, authCode[16], WORD slot, char name[17], BYTE gender, DWORD job, BYTE headType.
func createCharFrame(slot uint16, name string, gender byte, job uint32, head byte) []byte {
	b := make([]byte, protocol.CreateCharSize)
	binary.LittleEndian.PutUint16(b, protocol.CreateCharCmd)
	binary.LittleEndian.PutUint16(b[26:], slot)
	copy(b[28:36], name)
	b[45] = gender
	binary.LittleEndian.PutUint32(b[46:], job)
	b[50] = head
	return b
}

// TestCreateCharStoresTheCharacterAndAnswersWith277Bytes: cmd 18 in, the row created, and a
// 277-byte BC_CREATE_CHAROK back with the level byte at 275 (writing it at 276 made the
// barrack show a new character at level 0).
func TestCreateCharStoresTheCharacterAndAnswersWith277Bytes(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	store := &familyWithChars{fakeBarrack: fakeBarrack{found: true, cmdr: CommanderRow{ID: 990402, Name: "diu"}}}
	h.Barrack = store

	out, err := h.Reply(context.Background(), &Session{AccountID: 38, CommanderID: 990402},
		protocol.Frame{Cmd: 18, Size: protocol.CreateCharSize, Data: createCharFrame(0, "Scouty", 1, 540, 1)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !store.created {
		t.Fatal("the character was not written")
	}
	if store.got.Name != "Scouty" || store.got.Slot != 0 || store.got.Job != 540 || store.got.Gender != 1 {
		t.Fatalf("parsed %+v, want name Scouty slot 0 job 540 gender 1", store.got)
	}
	if len(out) != 1 {
		t.Fatalf("answered %d packet(s), want the OK alone - a barrack list behind it raises the "+
			"barrack window over the creation screen", len(out))
	}
	ok := out[0]
	if len(ok) != protocol.CreateCharOKSize {
		t.Fatalf("OK is %d bytes, the client's table says %d", len(ok), protocol.CreateCharOKSize)
	}
	if cmd := binary.LittleEndian.Uint16(ok); cmd != protocol.CreateCharOKCmd {
		t.Fatalf("OK command is %d, want %d", cmd, protocol.CreateCharOKCmd)
	}
	if ok[275] != 1 {
		t.Fatalf("level byte at 275 is %d, want 1 (a level written at 276 shows a level 0 character)", ok[275])
	}
}

// TestCreateCharRefusesGarbage: a bad class id or an empty name must not reach the store -
// the old Python server wrote such a row and the client crashed on the next login.
func TestCreateCharRefusesGarbage(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	store := &familyWithChars{fakeBarrack: fakeBarrack{found: true}}
	h.Barrack = store
	sess := &Session{AccountID: 38, CommanderID: 990402}

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"no job", createCharFrame(0, "Scouty", 1, 0, 1)},
		{"no name", createCharFrame(0, "", 1, 540, 1)},
		{"slot too high", createCharFrame(protocol.MaxCharSlots, "Scouty", 1, 540, 1)},
	} {
		out, err := h.Reply(context.Background(), sess,
			protocol.Frame{Cmd: 18, Size: len(tc.frame), Data: tc.frame})
		if err != nil {
			t.Fatalf("%s: Reply: %v", tc.name, err)
		}
		if len(out) != 0 {
			t.Fatalf("%s: answered %d packet(s), want silence", tc.name, len(out))
		}
	}
	if store.created {
		t.Fatal("a refused request still reached the store")
	}
}
