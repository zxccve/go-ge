package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"strings"
	"testing"

	"geserver/protocol"
)

// The tests in this file pin ONE promise (ROADMAP R41): the answer to
// CB_REQ_BARRACK_INFO (op 15) carries the team groups (41), the equipment presets
// (57) and the preset-extend counts (65) - AFTER 35, once each, with real bodies -
// and the captured burst still does NOT carry them, so one login sees each list
// exactly once.
//
// They are written against the PACKETS, not against the handler's code: the
// sequence the client would receive is decoded back into command ids, and the one
// log line is read out of the logger. A test that cannot fail pins nothing, so
// every assertion here names the value it read.

// loggedHandler is quietHandler with a log the test can READ: the tail line is the
// card's diagnostic for a grey "Last Point", so the test must see the line itself.
func loggedHandler(t *testing.T, s Store) (*Handler, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := quietHandler(t, s)
	h.Log = log.New(&buf, "", 0)
	return h, &buf
}

// tailGroups is the data cmd 41 must carry: two saved teams, so its body cannot be
// the honest-but-empty 5-byte form.
func tailGroups() []TeamGroup {
	return []TeamGroup{
		{Name: "A team", Slots: [TeamGroupSlots]byte{0, 1, EmptyCharSlot}},
		{Name: "B team", Slots: [TeamGroupSlots]byte{2, EmptyCharSlot, EmptyCharSlot}},
	}
}

// tailHandler is a barrack that CAN rebuild from its store, with characters and two
// saved teams, so every branch of the op-15 answer is live.
func tailHandler(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	h, buf := loggedHandler(t, fakeStore{ok: true})
	h.Barrack = fakeBarrack{
		found:  true,
		cmdr:   CommanderRow{ID: 24, Name: "Arcanine", ZoneID: 10, ChannelID: 1, CharSlot: 4, TeamMax: 3},
		chars:  []CharInfo{{Slot: 0, Name: "Vex", Job: 7, Level: 89}},
		groups: tailGroups(),
	}
	return h, buf
}

// cmdsOf decodes a packet sequence back into its command ids.
func cmdsOf(pkts [][]byte) []uint16 {
	out := make([]uint16, 0, len(pkts))
	for _, p := range pkts {
		if len(p) < 2 {
			continue
		}
		out = append(out, binary.LittleEndian.Uint16(p))
	}
	return out
}

func countCmd(cmds []uint16, cmd uint16) int {
	n := 0
	for _, c := range cmds {
		if c == cmd {
			n++
		}
	}
	return n
}

func indexCmd(cmds []uint16, cmd uint16) int {
	for i, c := range cmds {
		if c == cmd {
			return i
		}
	}
	return -1
}

// TestTheBarrackInfoAnswerEndsWithTheThreeLists is the pass condition read off the
// wire: the answer to op 15 must END with 16, 17, 35, 41, 57, 65, and each of the
// three lists must appear exactly once, with a body the client can read.
func TestTheBarrackInfoAnswerEndsWithTheThreeLists(t *testing.T) {
	h, logBuf := tailHandler(t)
	sess := &Session{CommanderID: 24}

	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 2})
	if err != nil {
		t.Fatalf("REQ_BARRACK_INFO: %v", err)
	}
	got := cmdsOf(out)
	want := []uint16{16, 17, 35, 41, 57, 65}
	if len(got) < len(want) {
		t.Fatalf("the answer carried %d packet(s) %v - want it to END with %v", len(got), got, want)
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("the answer ends %v, want %v (whole sequence %v)", tail, want, got)
		}
	}
	// "Never twice" inside one answer.
	for _, cmd := range want[3:] {
		if n := countCmd(got, cmd); n != 1 {
			t.Fatalf("cmd %d appears %d time(s) in %v, want exactly 1", cmd, n, got)
		}
	}

	// The lists are REAL bodies, not two-byte stubs. 41: header + one 20-byte
	// record per saved team, with the first team's name on the wire. 57: the
	// client's own 866-byte registration. 65: header + one 8-byte row per Extend
	// item.
	g41, g57, g65 := out[len(out)-3], out[len(out)-2], out[len(out)-1]
	if n := int(g41[4]); n != len(tailGroups()) {
		t.Fatalf("cmd 41 says %d team group(s), want %d", n, len(tailGroups()))
	}
	if want41 := TeamGroupHeader + len(tailGroups())*TeamGroupSize; len(g41) != want41 {
		t.Fatalf("cmd 41 is %d bytes, want %d", len(g41), want41)
	}
	if name := string(g41[TeamGroupHeader : TeamGroupHeader+len("A team")]); name != "A team" {
		t.Fatalf("cmd 41's first record carries %q, want %q", name, "A team")
	}
	if len(g57) != EquipPresetInfoSize {
		t.Fatalf("cmd 57 is %d bytes, want %d", len(g57), EquipPresetInfoSize)
	}
	if want65 := presetRemainHeader + len(PresetExtendItems())*presetRemainRecord; len(g65) != want65 {
		t.Fatalf("cmd 65 is %d bytes, want %d", len(g65), want65)
	}
	if n := int(binary.LittleEndian.Uint32(g65[4:])); n != len(PresetExtendItems()) {
		t.Fatalf("cmd 65 says %d item(s), want %d", n, len(PresetExtendItems()))
	}

	// ONE tail line, quoting the ids and the byte total that really went out.
	total := 0
	for _, p := range out {
		total += len(p)
	}
	wantLine := fmt.Sprintf("barrack: info tail 16 17 35 41 57 65 (%d bytes)", total)
	if n := strings.Count(logBuf.String(), "barrack: info tail "); n != 1 {
		t.Fatalf("%d tail line(s) in the log, want exactly 1:\n%s", n, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), wantLine) {
		t.Fatalf("the log does not carry %q:\n%s", wantLine, logBuf.String())
	}
}

// TestTheBurstAndTheAnswerSendEachListOnce is the other half of the card: "never
// before 35, never twice" is a SESSION-level fact. The capture carries 41/57/65 in
// the burst, so the burst must drop them - and then the answer adds each exactly
// once, after 35.
func TestTheBurstAndTheAnswerSendEachListOnce(t *testing.T) {
	h, _ := tailHandler(t)
	// A burst shaped like the capture's: cmd 10 makes startBarrack build, and the
	// three lists are present in the capture and must not be replayed.
	h.AfterStartBarrack = [][]byte{{10, 0}, {41, 0}, {57, 0}, {65, 0}, {16, 0}}

	sess := &Session{CommanderID: 24}
	burst, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("STARTBARRACK: %v", err)
	}
	answer, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 2})
	if err != nil {
		t.Fatalf("REQ_BARRACK_INFO: %v", err)
	}

	burstIds := cmdsOf(burst)
	if n := countCmd(burstIds, 16); n != 0 {
		t.Fatalf("the burst carries %d barrack-info packet(s) %v - 16/41/57/65 belong to the answer", n, burstIds)
	}
	for _, cmd := range []uint16{41, 57, 65} {
		if n := countCmd(burstIds, cmd); n != 0 {
			t.Fatalf("the burst carries cmd %d (%v) - the list belongs to the answer, once", cmd, burstIds)
		}
	}

	all := append(append([][]byte{}, burst...), answer...)
	got := cmdsOf(all)
	for _, cmd := range []uint16{41, 57, 65} {
		if n := countCmd(got, cmd); n != 1 {
			t.Fatalf("cmd %d went out %d time(s) across the burst and the answer (%v), want exactly 1", cmd, n, got)
		}
	}
	i35, i41 := indexCmd(got, 35), indexCmd(got, 41)
	if i35 < 0 || i41 < 0 || i41 < i35 {
		t.Fatalf("cmd 35 is at %d and cmd 41 at %d in %v - the list must come AFTER the channel list", i35, i41, got)
	}
}

// TestTheDegradedAnswerSaysWhatItSent: with no database there is no 35 and no lists.
// The ONE tail line must say exactly that, because a grey-"Last Point" report is read
// against this line - an empty answer and a missing 35 must not look the same.
func TestTheDegradedAnswerSaysWhatItSent(t *testing.T) {
	h, logBuf := loggedHandler(t, fakeStore{ok: true})
	sess := &Session{CommanderID: 24, BarrackInfo: BuildBarrackInfo([]CharInfo{{Name: "Vex", Job: 7}})}

	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 2})
	if err != nil {
		t.Fatalf("REQ_BARRACK_INFO: %v", err)
	}
	got := cmdsOf(out)
	if len(got) != 1 || got[0] != 16 {
		t.Fatalf("the degraded answer is %v, want [16] only (no database, so no 35 and no lists)", got)
	}
	wantLine := fmt.Sprintf("barrack: info tail 16 (%d bytes)", len(out[0]))
	if n := strings.Count(logBuf.String(), "barrack: info tail "); n != 1 {
		t.Fatalf("%d tail line(s) in the log, want exactly 1:\n%s", n, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), wantLine) {
		t.Fatalf("the log does not carry %q:\n%s", wantLine, logBuf.String())
	}
}

// TestTheJPListIdsAreKeptOutOfTheAnswer: the JP build shifted 57/65 to 58/66, gets
// its own copies inside the burst, and never sends op 15. A JP session that falls
// through to the op-15 answer must still get exactly 16, 17, 35 - appending the 2020
// ids would hand it two packets it cannot place.
func TestTheJPListIdsAreKeptOutOfTheAnswer(t *testing.T) {
	h, _ := tailHandler(t)
	h.JP = true
	sess := &Session{CommanderID: 24}

	out, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 15, Size: 2})
	if err != nil {
		t.Fatalf("REQ_BARRACK_INFO: %v", err)
	}
	got := cmdsOf(out)
	want := []uint16{16, 17, 35}
	if len(got) != len(want) {
		t.Fatalf("the JP answer is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the JP answer is %v, want %v", got, want)
		}
	}
	for _, cmd := range []uint16{41, 57, 65, 58, 66} {
		if n := countCmd(got, cmd); n != 0 {
			t.Fatalf("the JP op-15 answer carries cmd %d - its lists ride in the burst", cmd)
		}
	}
}

// TestTheJPBurstStillCarriesItsOwnLists is the "keep the JP special case exactly as
// it is" guard: the JP client never asks for op 15, so its set (41, 58, 66) must
// still be built INSIDE the burst and deferred to CB_REQ_BARRACK_INFO.
func TestTheJPBurstStillCarriesItsOwnLists(t *testing.T) {
	h, _ := tailHandler(t)
	h.JP = true
	h.AfterStartBarrack = [][]byte{{10, 0}, {41, 0}, {57, 0}, {65, 0}}
	sess := &Session{CommanderID: 24}

	burst, err := h.Reply(context.Background(), sess, protocol.Frame{Cmd: 9, Size: 11})
	if err != nil {
		t.Fatalf("STARTBARRACK: %v", err)
	}
	if got := cmdsOf(burst); len(got) != 1 || got[0] != 10 {
		t.Fatalf("the JP startBarrack answer is %v, want [10] only (the rest is deferred)", got)
	}
	deferred := cmdsOf(sess.JPBarrackSet)
	for _, cmd := range []uint16{41, 58, 66} {
		if n := countCmd(deferred, cmd); n != 1 {
			t.Fatalf("cmd %d appears %d time(s) in the deferred JP set %v, want exactly 1", cmd, n, deferred)
		}
	}
}
