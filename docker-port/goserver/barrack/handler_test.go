package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

const barrackTable = "../../data/cmd_table_barrack.txt"

// fakeStore replaces PostgreSQL in these tests, so the handler can be checked
// without a database - and so the tests still say something when one is down.
type fakeStore struct {
	acc Account
	ok  bool
	err error
}

func (f fakeStore) Account(context.Context, string) (Account, bool, error) {
	return f.acc, f.ok, f.err
}

func quietHandler(t *testing.T, s Store) *Handler {
	t.Helper()
	spec, err := protocol.LoadSpec(barrackTable)
	if err != nil {
		t.Fatalf("load barrack table: %v", err)
	}
	return &Handler{
		Store:  s,
		Table:  spec,
		Cipher: protocol.DefaultCipher,
		Log:    log.New(io.Discard, "", 0),
	}
}

// capturedFrame is the real CB_LOGIN from the capture, cut by the barrack table.
func capturedFrame(t *testing.T) protocol.Frame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "barrack_stub.plain.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var hexLine string
	for _, line := range splitLines(string(raw)) {
		if line != "" && line[0] != '#' {
			hexLine = line
			break
		}
	}
	if hexLine == "" {
		t.Fatal("fixture has no plaintext line")
	}
	plain, err := hex.DecodeString(hexLine)
	if err != nil {
		t.Fatalf("fixture hex: %v", err)
	}
	spec, err := protocol.LoadSpec(barrackTable)
	if err != nil {
		t.Fatalf("load barrack table: %v", err)
	}
	frames, _ := spec.Split(plain)
	if len(frames) == 0 {
		t.Fatal("the capture did not split into a packet")
	}
	return frames[0]
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// TestLoginIsAnswered is the end-to-end check of the Glue: a real captured
// login goes in, a 57-byte BC_LOGINOK comes out, carrying the account id from
// the store and the name the client itself sent.
func TestLoginIsAnswered(t *testing.T) {
	h := quietHandler(t, fakeStore{
		acc: Account{AccountID: 35, CommanderID: 24, MemberID: "arcanine"},
		ok:  true,
	})

	out, err := h.Reply(context.Background(), &Session{}, capturedFrame(t))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("reply packets = %d, want 1 (no replay configured)", len(out))
	}
	if len(out[0]) != 57 {
		t.Fatalf("reply = %d bytes, want 57", len(out[0]))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 8 {
		t.Fatalf("reply command = %d, want 8 (BC_LOGINOK)", cmd)
	}
	if acc := binary.LittleEndian.Uint32(out[0][2:]); acc != 35 {
		t.Fatalf("reply account = %d, want 35", acc)
	}
	// The NAME is echoed as the client sent it, not as the database stores it
	// (the legacy row is lower case; the client sends "Arcanine").
	if got := trimNul(string(out[0][10:])); got != "Arcanine" {
		t.Fatalf("reply name = %q, want %q", got, "Arcanine")
	}
}

func trimNul(s string) string {
	for i, c := range s {
		if c == 0 {
			return s[:i]
		}
	}
	return s
}

// TestUnknownAccountSendsNothing proves an unregistered name produces silence
// rather than a fake login: the client must not be let in on a guess.
func TestUnknownAccountSendsNothing(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: false})
	out, err := h.Reply(context.Background(), &Session{}, capturedFrame(t))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("reply = %d packet(s), want none for an unknown account", len(out))
	}
}

// TestUnhandledCommandSendsNothing keeps the server quiet for everything we do
// not implement yet, instead of replying with something invented.
func TestUnhandledCommandSendsNothing(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	// 99 is not in the table; 15 was the old example and it IS handled (it is the client's
	// barrack-info request), so it stopped being a good stand-in for "no handler".
	out, err := h.Reply(context.Background(), &Session{},
		protocol.Frame{Cmd: 99, Size: 10, Data: make([]byte, 10)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("reply = %d packet(s), want none for an unimplemented command", len(out))
	}
}

// TestStartBarrackWithoutAFamilySendsTheNoCommanderMessage: a family-less session is answered
// with the BC_MESSAGE the LIVE server sends (captures\v42-nofamily\20261111-143143) - cmd 3
// carrying the commander-loader failure line. That message is what puts the client on an
// empty barrack with the create-family dialog open; every other shape this server tried
// either sent it to the character wizard or hung it.
func TestStartBarrackWithoutAFamilySendsTheNoCommanderMessage(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	h.AfterStartBarrack = [][]byte{{0x38, 0x00, 0xAA, 0xAA}} // the capture must not be replayed

	out, err := h.Reply(context.Background(),
		&Session{AccountID: 38, MemberID: "laucher"},
		protocol.Frame{Cmd: 9, Size: 11, Data: make([]byte, 11)})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("answered %d packet(s), want the single no-commander message", len(out))
	}
	msg := out[0]
	if cmd := binary.LittleEndian.Uint16(msg); cmd != systemMessageCmd {
		t.Fatalf("packet is cmd %d, want %d (BC_MESSAGE)", cmd, systemMessageCmd)
	}
	if declared := binary.LittleEndian.Uint16(msg[2:]); int(declared) != len(msg) {
		t.Fatalf("declared length %d, packet is %d bytes - the length word is the whole packet",
			declared, len(msg))
	}
	if !bytes.Contains(msg, []byte("m_commanderLoader")) {
		t.Fatalf("the message text is not the captured one: %q", msg[44:])
	}
	if !bytes.Contains(msg, []byte("laucher")) {
		t.Fatal("the message does not name the account")
	}
	// The captured packet, byte for byte, is 118 bytes for a 15-character account name.
	if len(msg) != 118 {
		t.Fatalf("packet is %d bytes, the captured shape is 118 for this name length", len(msg))
	}
}

// TestStoreErrorIsNotSilent: a broken database must surface as an error, not as
// "no such account". Confusing the two hides outages as typos.
func TestStoreErrorIsNotSilent(t *testing.T) {
	h := quietHandler(t, fakeStore{err: io.ErrUnexpectedEOF})
	if _, err := h.Reply(context.Background(), &Session{}, capturedFrame(t)); err == nil {
		t.Fatal("a store failure must be returned, not swallowed")
	}
}

// familyCreator is fakeBarrack plus the one write the create-family flow needs.
type familyCreator struct {
	fakeBarrack
	row CommanderRow
}

func (f familyCreator) CreateCommander(context.Context, uint32, string, string) (CommanderRow, bool, error) {
	return f.row, true, nil
}

// TestCreateCommanderPushesTheWholeBarrackState: the OK alone is invisible to this client -
// its barrack dispatcher has no case for cmd 12 (ge.exe FUN_005251e5 falls through on 0x0c) -
// and the OK plus a bare cmd 16 CRASHED the client twice at ge.exe+0x1AD488, a getter whose
// parameter was NULL (Windows Application log, 2026-11-11 14:03:10 and 14:03:43): the barrack
// screen went live with no channel/map entry behind it. So the answer must be cmd 12, then
// cmd 10 carrying the family's name, then the WHOLE barrack state - 16, 17, 35 and the lists
// behind it, the same sequence CB_REQ_BARRACK_INFO uses.
func TestCreateCommanderPushesTheWholeBarrackState(t *testing.T) {
	row := CommanderRow{ID: 990402, Name: "diu", CharSlot: 10, TeamMax: 5}
	h := quietHandler(t, fakeStore{ok: true})
	h.Barrack = familyCreator{fakeBarrack: fakeBarrack{found: true, cmdr: row}, row: row}

	frame := make([]byte, protocol.CreateCommanderSize)
	binary.LittleEndian.PutUint16(frame, protocol.CreateCommanderCmd)
	copy(frame[26:], "diu") // createCommanderNameAt: 10 security + 16 authCode

	out, err := h.Reply(context.Background(),
		&Session{AccountID: 38, MemberID: "laucher"},
		protocol.Frame{Cmd: 11, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	ids := make([]uint16, 0, len(out))
	for _, p := range out {
		ids = append(ids, binary.LittleEndian.Uint16(p))
	}
	// The answer is the OK, the card and the lock table - the reference's own sequence; the
	// barrack that follows is the client's own request.
	want := []uint16{12, 10, 32}
	if len(ids) != len(want) {
		t.Fatalf("answer ids %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("answer ids %v, want %v", ids, want)
		}
	}
	if name := out[1]; !bytes.Contains(name, []byte("diu")) {
		t.Fatal("cmd 10 does not carry the family name the client just created")
	}
}
// CB_REQ_BARRACK_INFO (15) right after the start burst, and OUR answer (16) re-switched it
// to character creation - measured at 13:50:03.974156 in logs\barrack_family_test6.log,
// 4 ms after the cmd 14 that had just been sent. So this answer must end with cmd 14 too.
func TestReqBarrackInfoWithoutAFamilyStillTriggersTheFamilyScreen(t *testing.T) {
	h := quietHandler(t, fakeStore{ok: true})
	sess := &Session{BarrackInfo: BuildBarrackInfo(nil)} // what a family-less account is given

	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 10})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want the single no-commander message", len(out))
	}
	if cmd := binary.LittleEndian.Uint16(out[0]); cmd != systemMessageCmd {
		t.Fatalf("first packet is cmd %d, want %d (BC_MESSAGE)", cmd, systemMessageCmd)
	}

	// A session that HAS a family must not get the trigger - it would throw the player
	// back to the family screen every time the barrack is refreshed.
	sess2 := &Session{BarrackInfo: make([]byte, 11), CommanderID: 282}
	out2, err := h.Reply(context.Background(), sess2, protocol.Frame{Cmd: 15, Size: 10})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	for _, p := range out2 {
		if len(p) == 2 && binary.LittleEndian.Uint16(p) == 14 {
			t.Fatal("cmd 14 was sent to a session that already has a family")
		}
	}
}
