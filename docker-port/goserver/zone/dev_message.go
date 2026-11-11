package zone

// DEV ONLY, AND IT IS A MEASURING TAPE, NOT A FEATURE (2026-10-30, TYPE-HUNT).
//
// The shipped Granado Espada client renumbers `MESSAGE_TYPE_*` relative to the
// source tree in a way that is NOT a uniform shift, and the one number we need -
// MESSAGE_TYPE_CURRENT_WAY_POINT, the "You have chosen the waypoint of the current
// area." sentence - has now been guessed wrong twice on the owner's client:
//
//	sent 136 -> the box read "You can't participate in it as you did not satisfy
//	            its level limitation."   (the source's case 135)
//	sent 135 -> the box read "Password is incorrect."
//	            (the source's case 125)
//
// ANSWERED (2026-10-30 ~20:46): `/typemsg 147 3` boxed "You have chosen the
// waypoint of the current area." - the right sentence - so the shipped client's
// MESSAGE_TYPE_CURRENT_WAY_POINT is 147, eleven values past the header's 136, and
// `zone\message.go` carries it as a MEASURED constant. This file stays in the tree
// as the instrument, unchanged: the next message type gets settled the same way, in
// one sitting, instead of by reading geSharedMessage.h.
//
// `ZC_MESSAGE` (3596) is the key to settling it, because `type` is BOTH a case
// label and the text selector: for any type except 0 the client THROWS AWAY the
// text we send and prints its OWN sentence (GEPacket.cpp:210-211). So a command
// that sends one 3596 with a type the owner types, and a text that always says
// which numbers were sent, turns the client into the measuring instrument - the
// sentence that appears ON SCREEN names the type.
//
// There is no development switch in this server to put this behind. The flag list
// is -rawlog, -diag-hex, -no-scripts and -diag-stop, and none of them gates
// behaviour. Adding one would mean the owner cannot use the instrument without a
// server restart, which this task is not allowed to do - so it stays UNGATED and
// therefore HARMLESS BY CONSTRUCTION: it sends nothing but one message, touches no
// state, writes no database row, and its worst case is a box that says
// "type 999 level 0" to whoever typed the command.
//
// The numbers are bounded on purpose (type 0..999, level 0..5): the client's own
// level enum runs 0..5 (geSharedMessage.h:447-454: GENERAL, GENERAL_BOX, DEBUG,
// WARNING, ERROR, NOTIFY) and the type enum is a few hundred entries, so 999 is
// generous while still catching a typo like "1360". Out of range, the command
// answers with ONE plain chat line and sends NOTHING with the numbers typed.

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// devMessageCommand is what the player types. The name says what it does, and
	// it is not a word any /who or emote could collide with.
	devMessageCommand = "/typemsg"

	// devMessageUsage is the ONE line a malformed or out-of-range call answers
	// with. It names the shape and the bounds, because the person reading it is
	// about to type the command again.
	devMessageUsage = "usage: /typemsg <type> <level>   (type 0-999, level 0-5; " +
		"e.g. /typemsg 137 3)"

	// devMessageMaxType and devMessageMaxLevel are the bounds. Level 5 is the last
	// real MESSAGE_LEVEL_*; a type above 999 is not a case label any build has.
	devMessageMaxType  = 999
	devMessageMaxLevel = 5
)

// devMessageArgs reports whether a chat line is the dev command, and returns the
// words after it.
//
// The match is case-insensitive and accepts the name with no arguments, so
// "/typemsg" on its own answers the usage line instead of being logged as
// ordinary chat. The name must be a WHOLE word: "/typemsgx" is not the command.
func devMessageArgs(text string) ([]string, bool) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < len(devMessageCommand) ||
		!strings.EqualFold(trimmed[:len(devMessageCommand)], devMessageCommand) {
		return nil, false
	}
	rest := trimmed[len(devMessageCommand):]
	if rest == "" {
		return nil, true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return nil, false
	}
	// Fields collapses runs of spaces, so "136   3" reads the same as "136 3".
	return strings.Fields(rest), true
}

// parseDevMessage turns the arguments into the pair to send.
//
// why is a SENTENCE, and it is empty only when the pair is usable. It is worded
// for the log, which is where the project manager reads it when a number the
// owner typed comes back with a surprising sentence.
func parseDevMessage(args []string) (msgType int, level int, why string) {
	if len(args) != 2 {
		return 0, 0, fmt.Sprintf("%d argument(s), want 2 (<type> <level>)", len(args))
	}
	msgType, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, 0, fmt.Sprintf("type %q is not a number", args[0])
	}
	level, err = strconv.Atoi(args[1])
	if err != nil {
		return 0, 0, fmt.Sprintf("level %q is not a number", args[1])
	}
	if msgType < 0 || msgType > devMessageMaxType {
		return 0, 0, fmt.Sprintf("type %d is outside 0..%d", msgType, devMessageMaxType)
	}
	if level < 0 || level > devMessageMaxLevel {
		return 0, 0, fmt.Sprintf("level %d is outside 0..%d (0 = chat line, "+
			"1..5 = a box; 2 draws nothing)", level, devMessageMaxLevel)
	}
	return msgType, level, ""
}

// devMessage answers the dev chat command with exactly ONE ZC_MESSAGE.
//
// The text is always "type <n> level <m>" and it is ASCII on purpose: for type 0
// the client prints it (so the line proves the instrument itself works), and for
// every other type the client replaces it with its own sentence, which is the
// measurement. Either way the log line below names both numbers, so a sentence
// the owner reports can be matched to one emission.
func (h *Handler) devMessage(args []string) [][]byte {
	msgType, level, why := parseDevMessage(args)
	if why != "" {
		h.Log.Printf("zone: DEV %s %q: %s - nothing was sent with a type; the "+
			"player is told the usage line", devMessageCommand,
			strings.Join(args, " "), why)
		return [][]byte{BuildMessage(devMessageUsage, MessageTypeGeneral,
			MessageLevelGeneral)}
	}

	text := fmt.Sprintf("type %d level %d", msgType, level)
	pkt := BuildMessage(text, uint32(msgType), byte(level))
	h.Log.Printf("zone: DEV %s type %d level %d -> one ZC_MESSAGE (cmd %d, %d bytes, "+
		"text %q); THIS is the emission a reported sentence belongs to",
		devMessageCommand, msgType, level, CmdZCMessage, len(pkt), text)
	return [][]byte{pkt}
}
