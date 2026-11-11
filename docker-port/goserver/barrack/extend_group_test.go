package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"geserver/protocol"
)

// The tests in this file pin the Team List's Extend button (ROADMAP R40):
// CB_REQ_EXTEND_GROUP (52) answers 53 with the game script's own price, and its
// "do it" byte takes the Moon Stones, raises Lock_TeamGroup by one and sends the
// Lock table (32) with the new slot count followed by the 2-byte 54.
//
// They are written against the PACKETS and against a store that MODELS the bag and
// the property - not against a canned answer - so a handler that skipped the write,
// priced the wrong slot or sent 54 on a refusal cannot pass.

// extendStore is a barrack whose Lock_TeamGroup and Moon Stone count the test can
// read back. The promoted fakeBarrack supplies the rest of BarrackStore.
type extendStore struct {
	fakeBarrack
	teamMax   uint16
	stones    int
	calls     int
	lastClass uint32
}

// Commander answers the CURRENT slot, so a price query after an extension sees the
// new value - the same thing the real store's property read does.
func (s *extendStore) Commander(ctx context.Context, id uint32) (CommanderRow, bool, error) {
	row := s.fakeBarrack.cmdr
	row.TeamMax = s.teamMax
	return row, true, nil
}

// ExtendTeamGroup applies the card's rules to the fake's bag: cap first, then the
// price from TeamGroupPrice, then the take and the +1 - and nothing at all on a
// refusal.
func (s *extendStore) ExtendTeamGroup(_ context.Context, _ uint32, classID uint32) (ExtendResult, error) {
	s.calls++
	s.lastClass = classID
	res := ExtendResult{Slot: s.teamMax, NewSlot: s.teamMax, Need: TeamGroupPrice(s.teamMax), Held: s.stones}
	switch {
	case s.teamMax >= TeamGroupMaxSlot:
		res.Outcome = ExtendAtCap
	case s.stones < res.Need:
		res.Outcome = ExtendNoStones
	default:
		s.stones -= res.Need
		s.teamMax++
		res.Outcome = ExtendDone
		res.Taken = res.Need
		res.NewSlot = s.teamMax
	}
	return res, nil
}

func newExtendStore(slot uint16, stones int) *extendStore {
	return &extendStore{
		fakeBarrack: fakeBarrack{
			found: true,
			cmdr:  CommanderRow{ID: 24, Name: "Arcanine", ZoneID: 10, ChannelID: 1, CharSlot: 10},
		},
		teamMax: slot,
		stones:  stones,
	}
}

func extendHandler(t *testing.T, st *extendStore) (*Handler, *bytes.Buffer) {
	t.Helper()
	h, buf := loggedHandler(t, fakeStore{ok: true})
	h.Barrack = st
	return h, buf
}

// extendRequest is the real shape: 11 bytes, cmd 52, the intent byte at +10 (after
// PKS_SECURITY_DEFAULT's cmd(2)+seq(4)+checksum(4)).
func extendRequest(execute byte) protocol.Frame {
	data := make([]byte, 11)
	binary.LittleEndian.PutUint16(data, 52)
	data[extendGroupExecuteOffset] = execute
	return protocol.Frame{Cmd: 52, Size: len(data), Data: data}
}

// TestThePriceQueryAnswersTheScriptsThreeNumbers is the card's price proof: the
// three slots it names must come out of GET_TEAMGROUP_PRICE's own rule, read off
// the packet that goes out.
func TestThePriceQueryAnswersTheScriptsThreeNumbers(t *testing.T) {
	cases := []struct {
		slot uint16
		want uint32
	}{
		{0, 5},
		{10, 6},
		{15, 11},
	}
	got := make([]uint32, 0, len(cases))
	for _, tc := range cases {
		h, _ := extendHandler(t, newExtendStore(tc.slot, 0))
		out, err := h.Reply(context.Background(), &Session{CommanderID: 24}, extendRequest(0))
		if err != nil {
			t.Fatalf("slot %d: EXTEND_GROUP price query: %v", tc.slot, err)
		}
		if len(out) != 1 {
			t.Fatalf("slot %d: the price query sent %d packet(s), want 1", tc.slot, len(out))
		}
		if cmd := binary.LittleEndian.Uint16(out[0]); cmd != 53 {
			t.Fatalf("slot %d: the price answer is cmd %d, want 53 (BC_REQ_EXTEND_GROUP)", tc.slot, cmd)
		}
		if len(out[0]) != 10 {
			t.Fatalf("slot %d: cmd 53 is %d bytes, want 10 (cmd + class + count)", tc.slot, len(out[0]))
		}
		class := binary.LittleEndian.Uint32(out[0][2:])
		count := binary.LittleEndian.Uint32(out[0][6:])
		if class != MoonStoneClassID {
			t.Fatalf("slot %d: cmd 53 names class %d, want MoonStone %d", tc.slot, class, MoonStoneClassID)
		}
		if count != tc.want {
			t.Fatalf("slot %d: cmd 53 asks %d stone(s), want %d (GET_TEAMGROUP_PRICE)", tc.slot, count, tc.want)
		}
		got = append(got, count)
	}
	// The one line the pass condition quotes.
	t.Logf("price0/10/15=%d/%d/%d", got[0], got[1], got[2])
}

// TestTheDoItBranchTakesTheStonesAndRaisesTheSlot is the whole success path: the
// bag is charged, Lock_TeamGroup goes 5 -> 6, and the client is told with the Lock
// table (32, carrying the NEW value) and then the 2-byte 54.
func TestTheDoItBranchTakesTheStonesAndRaisesTheSlot(t *testing.T) {
	st := newExtendStore(5, 5)
	h, logBuf := extendHandler(t, st)

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24}, extendRequest(1))
	if err != nil {
		t.Fatalf("EXTEND_GROUP do-it: %v", err)
	}
	got := cmdsOf(out)
	want := []uint16{32, 54}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the do-it answer is %v, want %v (lock table, then the result)", got, want)
	}
	if len(out[1]) != 2 {
		t.Fatalf("cmd 54 is %d bytes, want 2 (the command alone - it has no flag)", len(out[1]))
	}
	if total := binary.LittleEndian.Uint16(out[0][2:]); int(total) != len(out[0]) {
		t.Fatalf("cmd 32's length word is %d, want %d", total, len(out[0]))
	}
	if xml := string(out[0]); !strings.Contains(xml, `Lock_TeamGroup = "6"`) {
		t.Fatalf("cmd 32 does not carry the NEW slot count (Lock_TeamGroup = \"6\"):\n%s", xml)
	}

	if st.teamMax != 6 {
		t.Fatalf("Lock_TeamGroup is %d after the extension, want 6", st.teamMax)
	}
	if st.stones != 0 {
		t.Fatalf("the bag holds %d stone(s) after paying 5, want 0", st.stones)
	}
	if st.calls != 1 || st.lastClass != MoonStoneClassID {
		t.Fatalf("the store was called %d time(s) for class %d, want once for %d",
			st.calls, st.lastClass, MoonStoneClassID)
	}
	if line := logBuf.String(); !strings.Contains(line, "slot 5 -> 6") || !strings.Contains(line, "took 5 MoonStone") {
		t.Fatalf("the success line does not say what happened:\n%s", line)
	}
}

// TestWithoutTheStonesNothingIsWrittenAndNo54: a short bag must not buy a slot AND
// must not send 54 - the client shows a SUCCESS message for any 54 it receives.
func TestWithoutTheStonesNothingIsWrittenAndNo54(t *testing.T) {
	st := newExtendStore(5, 4)
	h, logBuf := extendHandler(t, st)

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24}, extendRequest(1))
	if err != nil {
		t.Fatalf("EXTEND_GROUP do-it: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a refused extension sent %v, want NOTHING (a 54 would read as success)", cmdsOf(out))
	}
	if st.teamMax != 5 {
		t.Fatalf("Lock_TeamGroup moved to %d on a refusal, want it left at 5", st.teamMax)
	}
	if st.stones != 4 {
		t.Fatalf("the bag holds %d stone(s) after a refusal, want the 4 untouched", st.stones)
	}
	if line := logBuf.String(); !strings.Contains(line, "REFUSED") || !strings.Contains(line, "holds 4 of the 5") {
		t.Fatalf("the refusal line does not name the shortfall:\n%s", line)
	}
}

// TestAtTheCapNothingIsWritten: at GET_TEAMGROUP_MAXSLOT the button buys nothing,
// even with a full bag, and sends nothing.
func TestAtTheCapNothingIsWritten(t *testing.T) {
	st := newExtendStore(TeamGroupMaxSlot, 1000)
	h, logBuf := extendHandler(t, st)

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24}, extendRequest(1))
	if err != nil {
		t.Fatalf("EXTEND_GROUP do-it: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an extension at the cap sent %v, want NOTHING", cmdsOf(out))
	}
	if st.teamMax != TeamGroupMaxSlot {
		t.Fatalf("Lock_TeamGroup is %d after the cap refusal, want %d", st.teamMax, TeamGroupMaxSlot)
	}
	if st.stones != 1000 {
		t.Fatalf("the bag holds %d stone(s) after a cap refusal, want the 1000 untouched", st.stones)
	}
	if line := logBuf.String(); !strings.Contains(line, "REFUSED") || !strings.Contains(line, "cap") {
		t.Fatalf("the cap refusal line does not name the cap:\n%s", line)
	}
}

// TestAShortRequestSendsNothing: the intent byte sits at +10; a packet that stops
// short of it must not be guessed at.
func TestAShortRequestSendsNothing(t *testing.T) {
	st := newExtendStore(5, 5)
	h, logBuf := extendHandler(t, st)

	short := make([]byte, extendGroupExecuteOffset) // 10 bytes: no intent byte
	binary.LittleEndian.PutUint16(short, 52)
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 52, Size: len(short), Data: short})
	if err != nil {
		t.Fatalf("EXTEND_GROUP short request: %v", err)
	}
	if len(out) != 0 || st.calls != 0 {
		t.Fatalf("a 10-byte request answered %v and called the store %d time(s), want neither",
			cmdsOf(out), st.calls)
	}
	if !strings.Contains(logBuf.String(), "too short") {
		t.Fatalf("the short request was not reported:\n%s", logBuf.String())
	}
}

// TestTheBarrackBurstCarriesTheCommandersTeamGroup is the RELOGIN half: the burst's
// Lock table must carry the commander's own value, or a player who extended to 6 is
// told 5 on the next login and the client refuses the 6th saved team itself.
func TestTheBarrackBurstCarriesTheCommandersTeamGroup(t *testing.T) {
	st := newExtendStore(7, 0)
	h, _ := extendHandler(t, st)
	// The captured burst carries a cmd 32; the builder must replace it.
	h.AfterStartBarrack = [][]byte{{10, 0}, {32, 0}}

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("STARTBARRACK: %v", err)
	}
	for _, p := range out {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == 32 {
			if xml := string(p); !strings.Contains(xml, `Lock_TeamGroup = "7"`) {
				t.Fatalf("the burst's cmd 32 does not carry the commander's slot count (7):\n%s", xml)
			}
			return
		}
	}
	t.Fatalf("the burst carried no cmd 32: %v", cmdsOf(out))
}

// TestTheOtherIntentByteSendsNothing: the reference acts on 0 and 1 only.
func TestTheOtherIntentByteSendsNothing(t *testing.T) {
	st := newExtendStore(5, 5)
	h, logBuf := extendHandler(t, st)

	out, err := h.Reply(context.Background(), &Session{CommanderID: 24}, extendRequest(2))
	if err != nil {
		t.Fatalf("EXTEND_GROUP intent 2: %v", err)
	}
	if len(out) != 0 || st.calls != 0 {
		t.Fatalf("intent 2 answered %v and called the store %d time(s), want neither",
			cmdsOf(out), st.calls)
	}
	if !strings.Contains(logBuf.String(), "intent 2") {
		t.Fatalf("intent 2 was not reported:\n%s", logBuf.String())
	}
}

// TestThePriceRuleIsTheScriptsOwn keeps the arithmetic honest at the boundaries the
// card does not name: 9 is still 5, 11 is 7, and the cap's own slot would be 16.
func TestThePriceRuleIsTheScriptsOwn(t *testing.T) {
	for slot, want := range map[uint16]int{0: 5, 9: 5, 10: 6, 11: 7, 15: 11, 19: 15, 20: 16} {
		if got := TeamGroupPrice(slot); got != want {
			t.Errorf("TeamGroupPrice(%d) = %d, want %d", slot, got, want)
		}
	}
}

// TestThePriceAnswerNamesTheMoonStoneRow pins the class id against the item table's
// own row, so a later edit cannot quietly price the ForSale copy (88345) instead.
func TestThePriceAnswerNamesTheMoonStoneRow(t *testing.T) {
	if MoonStoneClassID != 75318 {
		t.Fatalf("MoonStoneClassID = %d, want 75318 (datatable_item_etc.xml ClassName=\"MoonStone\")",
			MoonStoneClassID)
	}
	if TeamGroupMaxSlot != 20 {
		t.Fatalf("TeamGroupMaxSlot = %d, want 20 (GET_TEAMGROUP_MAXSLOT)", TeamGroupMaxSlot)
	}
}
