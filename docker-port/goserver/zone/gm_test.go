package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"math"
	"strings"
	"testing"

	"geserver/protocol"
)

// gmTestItems is a two-row Item table for the command tests: the cheat list's own
// example item, with the MaxStack its row carries in ge\xml\datatable_item_etc.xml
// (ClassID 21419, MaxStack 32767), and no other name. The real table is 25k rows and
// is loaded by its own tests; this one exists so a test can say exactly what it
// resolved.
func gmTestItems() *ItemTable {
	return &ItemTable{
		byID: map[uint32]ItemRow{
			21419: {ID: 21419, Name: "SteelBulletEA", MaxStack: 32767, GroupName: "Etc"},
		},
		byName: map[string]uint32{"steelbulletea": 21419},
		byRule: map[string][]uint32{},
	}
}

// gmHandler is a Handler whose only wired piece is the log. A command that touches
// an unwired store must refuse, which is what several cases below expect.
func gmHandler() (*Handler, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Handler{Log: log.New(buf, "", 0)}, buf
}

// gmSession is the smallest session a command can act on: one character, driven at
// slot 0, with a resolved commander and an empty inventory mirror.
func gmSession(level int) *Session {
	return &Session{
		GmLevel:     level,
		CommanderID: 42,
		Team: []Character{{
			Handle: 0x40000011,
			Name:   "Tester",
			Level:  10,
			HP:     100,
			MaxHP:  150,
			SP:     50,
			MaxSP:  200,
			Exp:    1000,
		}},
		Inv: NewInvMirror(nil),
	}
}

// TestGMCommandFromLevelZeroIsRefused: the pass condition's first half. A `//hpup
// 100` from a level-0 session leaves HP untouched, sends NOTHING but the one
// refusal packet to that sender, and writes one log line naming the command and the
// level.
func TestGMCommandFromLevelZeroIsRefused(t *testing.T) {
	h, buf := gmHandler()
	sess := gmSession(0)

	out, err := h.Reply(context.Background(), sess, frameChat("//hpup 100"))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if sess.Team[0].HP != 100 {
		t.Fatalf("a level-0 //hpup changed HP to %g, want 100", sess.Team[0].HP)
	}
	if len(out) != 1 {
		t.Fatalf("%d packet(s), want exactly the one refusal: %d", len(out), len(out))
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCMessage {
		t.Fatalf("the refusal is cmd %d, want %d (ZC_MESSAGE, to the sender alone)",
			cmd, CmdZCMessage)
	}
	if n := packetsOfCmd(out, CmdZCAddHP); n != 0 {
		t.Fatalf("%d x %d went out - a refused command must not heal", n, CmdZCAddHP)
	}
	logged := buf.String()
	if n := strings.Count(logged, "GM command"); n != 1 {
		t.Fatalf("%d GM command log line(s), want 1:\n%s", n, logged)
	}
	if !strings.Contains(logged, "//hpup") {
		t.Fatalf("the refusal line does not name the command:\n%s", logged)
	}
	if !strings.Contains(logged, "level 0") {
		t.Fatalf("the refusal line does not name the level:\n%s", logged)
	}
}

// TestGMHpUpFromLevelOneRaisesByExactlyAndClamps: the pass condition's second half.
// Three cases: a positive delta lands exactly, a positive delta past the maximum
// clamps at MaxHP, and a negative delta is accepted (the client's own cheat list
// carries `//hpup -1000000`). The 3557 bytes are decoded, not trusted.
func TestGMHpUpFromLevelOneRaisesByExactlyAndClamps(t *testing.T) {
	cases := []struct {
		n     string
		want  float64
		delta float64
	}{
		{"20", 120, 20},    // exact
		{"100", 150, 50},   // clamped at MaxHP 150
		{"-30", 70, -30},   // a negative is a damage test, and it is accepted
		{"-1000", 0, -100}, // the lower bound is 0, never negative
	}
	for _, c := range cases {
		h, buf := gmHandler()
		sess := gmSession(1)

		out, err := h.Reply(context.Background(), sess, frameChat("//hpup "+c.n))
		if err != nil {
			t.Fatalf("%s: Reply: %v", c.n, err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCAddHP {
			t.Fatalf("%s: reply = %d packet(s), want one cmd %d",
				c.n, len(out), CmdZCAddHP)
		}
		pkt := out[0]
		if len(pkt) != AddHPSize {
			t.Fatalf("%s: %d bytes, want the registered %d", c.n, len(pkt), AddHPSize)
		}
		if got := binary.LittleEndian.Uint32(pkt[ahHandle:]); got != 0x40000011 {
			t.Fatalf("%s: handle = %#x, want 0x40000011", c.n, got)
		}
		if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahNowHP:])); got != c.want {
			t.Fatalf("%s: +6 new HP = %g, want %g", c.n, got, c.want)
		}
		if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:])); got != c.delta {
			t.Fatalf("%s: +14 delta = %g, want %g", c.n, got, c.delta)
		}
		wantFlag := byte(0)
		if c.delta < 0 {
			wantFlag = 1
		}
		if pkt[ahFlag] != wantFlag {
			t.Fatalf("%s: +22 flag = %d, want %d", c.n, pkt[ahFlag], wantFlag)
		}
		if sess.Team[0].HP != c.want {
			t.Fatalf("%s: the ledger holds %g, want %g", c.n, sess.Team[0].HP, c.want)
		}
		if !strings.Contains(buf.String(), "GM hpup") {
			t.Fatalf("%s: no hpup line:\n%s", c.n, buf.String())
		}
	}
}

// TestGMItemGivesByNameAndRefusesAnUnknownName: `//item SteelBulletEA 3` writes ONE
// bag op of the row's own class and count, and answers with the ordinary 3607; an
// unknown name writes nothing and names the name in the log.
func TestGMItemGivesByNameAndRefusesAnUnknownName(t *testing.T) {
	f := newFakeShop()
	buf := &bytes.Buffer{}
	h := &Handler{Log: log.New(buf, "", 0), Items: gmTestItems(), Commit: f.commit}
	sess := gmSession(1)

	out, err := h.Reply(context.Background(), sess, frameChat("//item SteelBulletEA 3"))
	if err != nil {
		t.Fatalf("Reply(//item): %v", err)
	}
	if len(f.stacks) != 1 || f.stacks[0].ClassID != 21419 || f.stacks[0].Count != 3 {
		t.Fatalf("the bag holds %+v, want one row of class 21419 x3", f.stacks)
	}
	if len(f.ops) != 1 {
		t.Fatalf("%d commit op(s), want 1", len(f.ops))
	}
	op := f.ops[0]
	if !op.Give || op.ClassID != 21419 || op.Count != 3 {
		t.Fatalf("op = %+v, want give 21419 x3", op)
	}
	if op.Merge != true || op.MaxStack != 32767 {
		t.Fatalf("op merge/max = %v/%d, want the item row's true/32767",
			op.Merge, op.MaxStack)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCAddItem {
		t.Fatalf("reply = %d packet(s), want one cmd %d", len(out), CmdZCAddItem)
	}
	if got := binary.LittleEndian.Uint32(out[0][4:]); got != 21419 {
		t.Fatalf("the 3607 names class %d, want 21419", got)
	}
	if got := binary.LittleEndian.Uint32(out[0][12:]); got != 3 {
		t.Fatalf("the 3607 count = %d, want 3 (the NEW total)", got)
	}
	if sess.Inv.Len() != 1 {
		t.Fatalf("the mirror has %d position(s), want 1", sess.Inv.Len())
	}

	// THE BOGUS NAME. One line naming it, no write, and no 3607.
	beforeOps := len(f.ops)
	buf.Reset()
	out, err = h.Reply(context.Background(), sess, frameChat("//item NoSuchItem 3"))
	if err != nil {
		t.Fatalf("Reply(//item bogus): %v", err)
	}
	if len(f.ops) != beforeOps {
		t.Fatalf("an unknown name reached the bag writer: %d op(s), want %d",
			len(f.ops), beforeOps)
	}
	if len(f.stacks) != 1 {
		t.Fatalf("an unknown name added a bag row: %+v", f.stacks)
	}
	if n := packetsOfCmd(out, CmdZCAddItem); n != 0 {
		t.Fatalf("an unknown name sent %d x %d", n, CmdZCAddItem)
	}
	logged := buf.String()
	if !strings.Contains(logged, "NoSuchItem") {
		t.Fatalf("the refusal line does not name the item:\n%s", logged)
	}
}

// TestGMUnknownCommandIsLoggedOnceAndChangesNothing: a typo is never swallowed. It
// writes one line (the same text twice is still one line) and touches nothing.
func TestGMUnknownCommandIsLoggedOnceAndChangesNothing(t *testing.T) {
	h, buf := gmHandler()
	sess := gmSession(1)

	for i := 0; i < 2; i++ {
		out, err := h.Reply(context.Background(), sess, frameChat("//bogus 1"))
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		if len(out) != 1 || cmdOf(out[0]) != CmdZCMessage {
			t.Fatalf("turn %d: reply = %d packet(s), want the one refusal",
				i, len(out))
		}
	}
	if sess.Team[0].HP != 100 || sess.Team[0].SP != 50 || sess.Team[0].Exp != 1000 {
		t.Fatalf("an unknown command changed the character: %+v", sess.Team[0])
	}
	logged := buf.String()
	if n := strings.Count(logged, "unknown command"); n != 1 {
		t.Fatalf("%d unknown-command line(s), want 1:\n%s", n, logged)
	}
	if !strings.Contains(logged, "//bogus 1") {
		t.Fatalf("the line does not carry the text typed:\n%s", logged)
	}
}

// TestGMSpUpAndExpUpUseTheExistingPackets: the two commands beyond the pass
// condition, pinned to the packets their existing paths build - ZC_UPDATE_SP's new
// value (the skill cast's setter) and ZC_EXP_UP's total (the kill award's packet).
func TestGMSpUpAndExpUpUseTheExistingPackets(t *testing.T) {
	h, buf := gmHandler()
	sess := gmSession(1)

	out, err := h.Reply(context.Background(), sess, frameChat("//spup 25"))
	if err != nil {
		t.Fatalf("Reply(//spup): %v", err)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCUpdateSP || len(out[0]) != UpdateSPSize {
		t.Fatalf("//spup sent %d packet(s), want one %d-byte %d",
			len(out), UpdateSPSize, CmdZCUpdateSP)
	}
	if got := binary.LittleEndian.Uint32(out[0][usValue:]); got != 75 {
		t.Fatalf("//spup 25 from 50 sent %d, want 75", got)
	}
	if sess.Team[0].SP != 75 {
		t.Fatalf("the SP ledger is %d, want 75", sess.Team[0].SP)
	}

	out, err = h.Reply(context.Background(), sess, frameChat("//expup 500"))
	if err != nil {
		t.Fatalf("Reply(//expup): %v", err)
	}
	if len(out) != 1 || cmdOf(out[0]) != CmdZCExpUp || len(out[0]) != ExpUpSize {
		t.Fatalf("//expup sent %d packet(s), want one %d-byte %d",
			len(out), ExpUpSize, CmdZCExpUp)
	}
	if got := out[0][euCharIndex]; got != 0 {
		t.Fatalf("//expup charIndex = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint64(out[0][euExp:]); got != 1500 {
		t.Fatalf("//expup total = %d, want 1500 (1000 + 500)", got)
	}
	if !strings.Contains(buf.String(), "GM expup") {
		t.Fatalf("no expup line:\n%s", buf.String())
	}
}

// TestStartGameReadsTheGmLevelOnce: the level comes from account_level through
// Handler.GmLevelOf, once, keyed by the login id the client sent - and a missing
// row is 0, not an error.
func TestStartGameReadsTheGmLevelOnce(t *testing.T) {
	cases := []struct {
		name  string
		level int
		want  int
	}{
		{"the operator", 1, 1},
		{"no row", 0, 0},
	}
	for _, c := range cases {
		h := quietHandler()
		calls := 0
		h.GmLevelOf = func(_ context.Context, member string) (int, error) {
			calls++
			if member != "arcanine" {
				t.Fatalf("%s: looked up %q, want the client's login id",
					c.name, member)
			}
			return c.level, nil
		}
		sess := &Session{}
		if _, err := h.startGame(context.Background(), sess,
			startGameLoginFrame(0, "arcanine")); err != nil {
			t.Fatalf("%s: startGame: %v", c.name, err)
		}
		if sess.GmLevel != c.want {
			t.Fatalf("%s: GmLevel = %d, want %d", c.name, sess.GmLevel, c.want)
		}
		if calls != 1 {
			t.Fatalf("%s: GmLevelOf called %d time(s), want exactly 1 per entry",
				c.name, calls)
		}
	}
}

// TestGMItemNamesResolveInTheRealItemTable: the cheat list's own `//item` example,
// SteelBulletEA, is in the merged Item idspace the zone loads at start, with the
// class and the per-slot cap the command hands to the bag writer. This is the
// difference between "the handler reads a table" and "the NAME the game uses is in
// that table". SKIPS when the client XML is not on this machine.
func TestGMItemNamesResolveInTheRealItemTable(t *testing.T) {
	items, err := LoadItemTables(XMLDir())
	if err != nil {
		t.Skipf("no item XML (%v)", err)
	}
	id, ok := items.IDByName("SteelBulletEA")
	if !ok {
		t.Fatalf("the real Item table (%d rows) has no ClassName SteelBulletEA",
			items.Len())
	}
	if id != 21419 {
		t.Fatalf("SteelBulletEA resolved to class %d, want 21419", id)
	}
	row, ok := items.Row(id)
	if !ok {
		t.Fatalf("class %d resolved but has no row", id)
	}
	if row.MaxStack != 32767 {
		t.Fatalf("SteelBulletEA MaxStack = %d, want 32767 (the row's own cap)",
			row.MaxStack)
	}
	if row.Unstack {
		t.Fatal("SteelBulletEA says Unstack TRUE - the command would not merge it")
	}
}

// startGameLoginFrame is CZ_START_GAME with the login id at +18, the field
// ParseStartGame reads. Account 0 keeps startGame off the team/commander stores, so
// the only wired thing under test is GmLevelOf.
func startGameLoginFrame(accountID uint32, login string) protocol.Frame {
	data := make([]byte, 40)
	binary.LittleEndian.PutUint16(data, CmdCZStartGame)
	binary.LittleEndian.PutUint32(data[10:], 0xABCD)
	binary.LittleEndian.PutUint32(data[14:], accountID)
	copy(data[18:], login)
	return protocol.Frame{Cmd: CmdCZStartGame, Size: len(data), Data: data}
}
