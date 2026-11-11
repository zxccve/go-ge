package zone

// The DEV-ONLY measuring tape for an unmeasured MESSAGE_TYPE_* number
// (2026-10-30, TYPE-HUNT). Two properties are worth a test and nothing here is a
// feature: exactly one 3596 with the numbers typed, and a malformed line that
// answers with a plain chat line instead of something the client would box.

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"
)

// devHandler is a zone with nothing loaded but a log: the command must work with
// no zone data at all, because it is a measuring tape and not a game feature.
func devHandler() (*Handler, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	return &Handler{Log: log.New(logs, "", 0)}, logs
}

// TestDevMessageEmitsOneZCMessageWithTheTypedNumbers is the whole contract, BY
// BYTES: one packet, cmd 3596, the length word equal to the packet, the requested
// type as a DWORD at +4, the requested level as a byte at +8, and the ASCII text
// "type <n> level <m>" at +45 ending in exactly one NUL.
//
// The numbers asserted are literals (137 / 3) and NOT read back from the request,
// because a bug that echoed the wrong field would agree with itself.
func TestDevMessageEmitsOneZCMessageWithTheTypedNumbers(t *testing.T) {
	for _, tc := range []struct {
		line  string
		typ   uint32
		level byte
		text  string
	}{
		{"/typemsg 137 3", 137, 3, "type 137 level 3"},
		{"/TYPEMSG 130 3", 130, 3, "type 130 level 3"},
		{"  /typemsg   136 \t 1  ", 136, 1, "type 136 level 1"},
		// Type 0 is the one case where the client keeps OUR text, so the same
		// command doubles as a check that the instrument itself reaches the screen.
		{"/typemsg 0 0", 0, 0, "type 0 level 0"},
	} {
		h, logs := devHandler()
		out, err := h.Reply(context.Background(), &Session{}, frameChat(tc.line))
		if err != nil {
			t.Fatalf("%q: Reply: %v", tc.line, err)
		}
		if len(out) != 1 {
			t.Fatalf("%q: %d packet(s), want EXACTLY one 3596", tc.line, len(out))
		}
		got := out[0]
		if cmd := cmdOf(got); cmd != 3596 {
			t.Fatalf("%q: cmd = %d, want 3596 (ZC_MESSAGE)", tc.line, cmd)
		}
		if lw := binary.LittleEndian.Uint16(got[2:]); int(lw) != len(got) {
			t.Fatalf("%q: length word = %d, want the packet size %d", tc.line, lw, len(got))
		}
		// 45-byte header (4 cmd/len + 4 type + 1 level + 36 tm) + text + NUL.
		if want := 45 + len(tc.text) + 1; len(got) != want {
			t.Fatalf("%q: packet = %d bytes, want %d", tc.line, len(got), want)
		}
		if v := binary.LittleEndian.Uint32(got[4:]); v != tc.typ {
			t.Fatalf("%q: type = %d at +4, want %d", tc.line, v, tc.typ)
		}
		if lv := got[8]; lv != tc.level {
			t.Fatalf("%q: level = %d at +8, want %d", tc.line, lv, tc.level)
		}
		if body := string(got[45 : len(got)-1]); body != tc.text {
			t.Fatalf("%q: text = %q, want %q", tc.line, body, tc.text)
		}
		if got[len(got)-1] != 0 {
			t.Fatalf("%q: last byte = 0x%02x, want the NUL terminator", tc.line, got[len(got)-1])
		}
		// The log line is what the project manager correlates a reported sentence
		// with: both numbers, in the same line as the fact that it was sent.
		if !strings.Contains(logs.String(), "type 137 level 3") && tc.typ == 137 {
			t.Fatalf("%q: the emission did not log both numbers:\n%s", tc.line, logs)
		}
		if !strings.Contains(logs.String(), "DEV /typemsg") {
			t.Fatalf("%q: the log line does not name the command:\n%s", tc.line, logs)
		}
	}
}

// TestDevMessageRejectsBadArguments: a malformed or out-of-range call answers with
// ONE plain chat line (type 0, level 0) and sends NOTHING carrying the numbers it
// was given - otherwise a typo would look like a measurement.
func TestDevMessageRejectsBadArguments(t *testing.T) {
	for _, line := range []string{
		"/typemsg",              // no arguments at all
		"/typemsg 137",          // one argument
		"/typemsg 137 3 9",      // three
		"/typemsg abc 3",        // not a number
		"/typemsg 137 xyz",      // not a number
		"/typemsg 1000 3",       // type over the bound
		"/typemsg -1 3",         // negative type
		"/typemsg 137 6",        // level over the bound
		"/typemsg 137 -2",       // negative level
		"/typemsg 9999999999 3", // too big to parse at all
	} {
		h, logs := devHandler()
		out, err := h.Reply(context.Background(), &Session{}, frameChat(line))
		if err != nil {
			t.Fatalf("%q: Reply: %v", line, err)
		}
		if len(out) != 1 {
			t.Fatalf("%q: %d packet(s), want exactly the one usage line", line, len(out))
		}
		got := out[0]
		if cmd := cmdOf(got); cmd != 3596 {
			t.Fatalf("%q: cmd = %d, want 3596 (the usage line is a plain ZC_MESSAGE)", line, cmd)
		}
		if v := binary.LittleEndian.Uint32(got[4:]); v != 0 {
			t.Fatalf("%q: type = %d, want 0 - a refused call must not send the type "+
				"it was handed", line, v)
		}
		if lv := got[8]; lv != 0 {
			t.Fatalf("%q: level = %d, want 0 - a refused call must not open a box", line, lv)
		}
		if body := string(got[45 : len(got)-1]); !strings.Contains(body, devMessageUsage) {
			t.Fatalf("%q: the answer does not carry the usage line: %q", line, body)
		}
		if !strings.Contains(logs.String(), "DEV /typemsg") {
			t.Fatalf("%q: the refusal is not logged:\n%s", line, logs)
		}
	}
}

// TestDevMessageIsNotOrdinaryChat: a line that only LOOKS like the command must
// still be ordinary chat (no packet), or the instrument would swallow real talk -
// and it must not shadow /fesoshop.
func TestDevMessageIsNotOrdinaryChat(t *testing.T) {
	h, logs := devHandler()
	h.ShopGroups = ShopGroups{"FESO_ITEMSHOP": {60081}}

	for _, line := range []string{"/typemsgx 137 3", "typemsg 137 3", "/typemsg2"} {
		out, err := h.Reply(context.Background(), &Session{}, frameChat(line))
		if err != nil {
			t.Fatalf("%q: Reply: %v", line, err)
		}
		if len(out) != 0 {
			t.Fatalf("%q: %d packet(s), want none - that is ordinary chat", line, len(out))
		}
	}
	if strings.Contains(logs.String(), "DEV") {
		t.Fatalf("a look-alike line reached the dev command:\n%s", logs)
	}

	// The Feso Shop line still opens its window: the dev command sits in front of
	// it in the chat handler and must not eat it.
	out, err := h.Reply(context.Background(), &Session{}, frameChat("/fesoshop"))
	if err != nil || len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
		t.Fatalf("/fesoshop gave %d packet(s), err %v; want the 3680 window", len(out), err)
	}
}
