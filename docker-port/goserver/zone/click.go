package zone

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"

	"geserver/item"
	"geserver/protocol"
)

// Clicking an NPC.
//
// The client sends CZ_CLICK_TRIGGER and then WAITS. It does not walk the player
// to the NPC: a live capture of the official server shows one click and then NO
// move packet at all for three minutes (history\zone-npc-click.md), and the
// client stays silent while it walks a move the SERVER sent. So this side has to
// decide "close enough" itself and, when it is not, drive the walk.
//
// WHAT THIS SLICE DOES: answers a click on an NPC the player is ALREADY beside.
// The chase is the next slice - with no NPC within 200 units of the map's spawn
// (the nearest is 503), a player has to walk up first, which the ordinary move
// echo already allows.
const (
	CmdCZClickTrigger = 3679 // client -> server: 16 bytes, handle at 12
	CmdCZDialogSelect = 3657 // client -> server: the player picked an option (index at 11)
	CmdCZAckDialog    = 3656 // client -> server: the player pressed OK / closed the box

	CmdZCDialogOK             = 3660 // server -> client: VAR (type, handle, text + NUL)
	CmdZCDialogSel            = 3663 // server -> client: VAR (type, count, handle, strings)
	CmdZCDialogClose          = 3665 // server -> client: command only, 2 bytes
	CmdZCNPCClickVox          = 3515 // server -> client: 6 bytes, the clicked handle
	CmdZCDialogNPCTrade       = 3680 // server -> client: 40 bytes, normal shop window (group name)
	CmdZCDialogNPCCustomTrade = 3681 // server -> client: 80-byte header + 33-byte records, custom shop window
)

// endDialog answers CZ_ACK_DIALOG (3656) and CZ_DIALOG_SELECT (3657).
//
// This is not optional housekeeping: the client is MODAL from the moment a dialog
// box opens. Until it is told the box is over it keeps the dialog camera, the
// player cannot move, and no key gets them out - measured twice, the second time
// by a player left staring at an NPC (2026-10-25). `ZC_DIALOG_CLOSE` is the bare
// 2-byte command that ends it.
//
// When a script yielded at ShowSelDlg and is waiting for the player's choice,
// CZ_DIALOG_SELECT resumes it down the picked branch and CZ_ACK_DIALOG resumes
// it with "no choice". Without this resume work, ShowSelDlg answers 0 and whole
// NPC families never branch.
func (h *Handler) endDialog(sess *Session, f protocol.Frame) ([][]byte, error) {
	if len(f.Data) < 12 {
		h.Log.Printf("zone: dialog answer (cmd %d) is %d bytes - too short to read, ignored",
			f.Cmd, len(f.Data))
		return nil, nil
	}
	who, value := f.Data[10], f.Data[11]

	// A suspended click waiting for a choice resumes here instead of just
	// closing. value 0 means the player pressed OK / closed without picking;
	// any other value is the 1-based option index.
	if cc := sess.Suspended; cc != nil && h.ResumeClick != nil {
		reply, again, rerr := h.ResumeClick(int(value))
		if rerr != nil {
			// The host no longer holds this session's suspended script: a later
			// click on some other NPC REPLACED it (StartClickCoroutine), or the
			// connection that owned it released it. The session's own flag is
			// then a lie, and keeping it would make the next answer try to resume
			// a script that is gone and report a false failure. Clear it and end
			// the box on the client, which is still modal.
			if staleSuspension(rerr) {
				h.Log.Printf("zone: %s is no longer suspended in the script engine (%v) - "+
					"the session's dialog state was stale, closing it", cc.entry, rerr)
				sess.DialogHandle = 0
				sess.Suspended = nil
				return [][]byte{BuildDialogClose()}, nil
			}
			h.Log.Printf("zone: resume %s failed: %v - closing anyway", cc.entry, rerr)
			sess.DialogHandle = 0
			sess.Suspended = nil
			return [][]byte{BuildDialogClose()}, nil
		}

		// Build the reply: when a window kind is returned (e.g. a shop) send
		// ONLY that window packet - no ZC_DIALOG_CLOSE ahead of it; the close
		// comes when the player dismisses the window. Without a window kind,
		// close the dialog first, then send the next/final dialog packets.
		out := [][]byte{}

		// drawn says whether a window really went out. A shop leaves a modal
		// window on the client, so only then does the session keep the handle.
		drawn := false
		if reply.WindowKind != "" {
			switch reply.WindowKind {
			case WindowShop:
				// Normal shop: the group name alone is enough to draw the window,
				// and the coin comes from the script's own 4th argument (Feso or
				// vis) - the client never tells us which shop it is looking at.
				out = append(out, h.tradeDialog(cc.handle, reply.WindowGroup,
					reply.WindowCoin, cc.entry))
				// Remember WHICH shop this is: a buy names its item by its INDEX
				// in this list, so without it the record cannot be resolved - and
				// the coin says which stack pays for it.
				sess.LastShop = &ShopState{Group: reply.WindowGroup, Kind: WindowShop,
					Coin: reply.WindowCoin, Items: h.ShopGroups.Items(reply.WindowGroup)}
				drawn = true
				h.Log.Printf("zone: %s resumed -> TRADE window on 0x%08x, group %q, coin %s (ZC_DIALOG_NPC_TRADE, 3680, 40 bytes)",
					cc.entry, cc.handle, reply.WindowGroup, reply.WindowCoin)
			case WindowShopCustom:
				// Custom shop: the window CARRIES its items and prices, so the
				// group name alone is not enough - it is looked up in the table
				// loaded at startup. Without rows there is nothing to draw, and
				// the close still goes out: the client is MODAL from the box it
				// just answered, so silence would leave the player unable to
				// move. An empty window beats a stuck one.
				entries := h.CustomShops.Entries(reply.WindowGroup)
				if len(entries) == 0 {
					h.Log.Printf("zone: %s resumed -> CUSTOM trade window %q but no "+
						"rows are loaded for that group (data\\custom_shops.json, built "+
						"by src\\emit_shop_data.py) - sending only the close",
						cc.entry, reply.WindowGroup)
					out = append(out, BuildDialogClose())
					break
				}
				window := BuildCustomTradeDialog(cc.handle, entries)
				out = append(out, window)
				// The index a barter buy names counts in THIS list, so the rows
				// are kept in the order they were sent.
				sess.LastShop = &ShopState{Group: reply.WindowGroup, Kind: WindowShopCustom,
					Entries: entries}
				drawn = true
				h.Log.Printf("zone: %s resumed -> CUSTOM trade window on 0x%08x, group %q, "+
					"%d row(s) (ZC_DIALOG_NPC_CUSTOM_TRADE, 3681, %d bytes)",
					cc.entry, cc.handle, reply.WindowGroup, len(entries), len(window))
			default:
				// Same reasoning: we cannot draw this window at all (warehouse,
				// lobby, anything unknown), so all we owe the client is the end.
				h.Log.Printf("zone: %s resumed -> unknown window kind %q (%q) - sending only the close",
					cc.entry, reply.WindowKind, reply.WindowGroup)
				out = append(out, BuildDialogClose())
			}

			// A drawn shop keeps the session's handle: the CZ_ACK_DIALOG the
			// player sends when they dismiss the window then logs a normal close
			// instead of a false "NO dialog open". The kinds we cannot draw are
			// over as of the close packet above, so their handle is cleared now.
			sess.Suspended = nil
			if drawn {
				sess.DialogHandle = cc.handle
			} else {
				sess.DialogHandle = 0
			}
			return out, nil
		} else if again {
			// The script asked for ANOTHER dialog on resume: close this one,
			// then send the next dialog and suspend again.
			sess.DialogHandle = cc.handle
			sess.Suspended = &suspendedClick{entry: cc.entry, handle: cc.handle}
			out = append(out, BuildDialogClose())
			out = append(out, buildDialogPackets(cc.handle, reply)...)
			return out, nil
		} else {
			// Dialog done: close it, then send any final dialog packets.
			out = append(out, BuildDialogClose())
			out = append(out, buildDialogPackets(cc.handle, reply)...)
		}

		sess.DialogHandle = 0
		sess.Suspended = nil
		return out, nil
	}

	if sess.DialogHandle == 0 {
		// No dialog of ours is open. The close is still worth sending: it is
		// harmless when the client is idle, and it is the only way out when our
		// bookkeeping and the client disagree (a reloaded server, a lost session).
		h.Log.Printf("zone: cmd %d (char %d, value %d) with NO dialog open - closing anyway",
			f.Cmd, who, value)
	} else {
		h.Log.Printf("zone: cmd %d (char %d, value %d) ends the dialog on handle 0x%08x",
			f.Cmd, who, value, sess.DialogHandle)
	}
	sess.DialogHandle = 0
	return [][]byte{BuildDialogClose()}, nil
}

// staleSuspension reports whether a resume failed because the HOST has nothing
// suspended, as opposed to a script that raised. The two answers need different
// handling: the first means this session's flag is out of date and the box must be
// closed; the second still has a session state worth keeping until the close.
//
// The test is on the message because the zone deliberately does not import the
// script package (the dependency stays one-way: main wires them). Both strings are
// the ones script.ResumeClick returns and are pinned by a test there.
func staleSuspension(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no suspended click") ||
		strings.Contains(msg, "click state is gone")
}

// buildDialogPackets turns one ClickReply into the packets the client draws:
// ZC_DIALOG_OK / ZC_DIALOG_SELECT plus the click voice, addressed to handle.
//
// THE TEXT GOES OUT AS THE SCRIPT WROTE IT - no dictionary lookup here. The
// client resolves its own `<$>N</>` tokens (37 of 116 live server->client
// captures carry them raw, e.g. exitroom2 @69071), so resolving them on this
// side substitutes OUR dictionary for the one the player's client ships.
// The captures and the corpus count: docker-port\lanes\build.md (2026-11-08).
func buildDialogPackets(handle uint32, reply ClickReply) [][]byte {
	var out [][]byte
	if reply.DialogKind == ClickDialogOK {
		out = append(out, BuildDialogOK(handle, reply.Text, reply.Fade))
	} else if reply.DialogKind == ClickDialogSelect {
		out = append(out, BuildDialogSelect(handle, reply.Prompt, reply.Options, reply.Fade))
	}
	out = append(out, BuildNPCClickVoice(handle))
	return out
}

// Click packet offsets. Fixed 16 bytes, no length word - the client's own table
// says 16 (data\cmd_table.txt) and src\zone_server.py reads the same three
// fields at the same offsets.
const (
	clIndex  = 10 // which team member clicked (0 = leader)
	clLong   = 11 // non-zero = the x5 "reach" click
	clHandle = 12 // the clicked trigger, 4 bytes
	clSize   = 16
)

// Click reach, in world units.
//
// A click counts as "already there" when the CENTRES are within 100
// (DISTANCE_TO_CHAR) plus both shape radii (S = 50 each), so 200 - and 600 for a
// long-distance click. Values from src\zone_server.py, which took them from the
// client's own constants.
const (
	ClickRange     = 200
	ClickRangeLong = 600
)

// ParseClickTrigger reads CZ_CLICK_TRIGGER:
//
//	security header(10) charIndex(1) longdistance(1) handle(4)
//
// charIndex is the position in the TEAM, not a handle. ok is false for a short
// packet, which is a malformed client and not a reason to close the socket.
func ParseClickTrigger(pt []byte) (charIndex int, longDistance bool, handle uint32, ok bool) {
	if len(pt) < clSize {
		return 0, false, 0, false
	}
	charIndex = int(pt[clIndex])
	longDistance = pt[clLong] != 0
	handle = binary.LittleEndian.Uint32(pt[clHandle:])
	return charIndex, longDistance, handle, true
}

// ClickRangeFor answers the reach for one click: the x5 reach when the client
// flagged it, the ordinary one otherwise.
func ClickRangeFor(longDistance bool) float64 {
	if longDistance {
		return ClickRangeLong
	}
	return ClickRange
}

// InClickRange reports whether the player is close enough to the NPC to talk to
// it. Distances are measured between CENTRES in the map plane; the height is not
// part of it, exactly as the client's own DISTANCE_TO_CHAR is a 2-D distance.
func InClickRange(pc, npc [2]int32, longDistance bool) bool {
	dx := float64(npc[0]) - float64(pc[0])
	dy := float64(npc[1]) - float64(pc[1])
	return math.Hypot(dx, dy) <= ClickRangeFor(longDistance)
}

// BuildDialogOK builds ZC_DIALOG_OK, the NPC's single line:
//
//	command(2) length(2) type(1) handle(4) text + NUL
//
// The text starts at byte 9 and length is the WHOLE packet including the
// terminator, because the client's handler fills the box from that offset and
// size (TriggerDialogOk -> FillDialogText). Read off
// src\zone_server.py `build_dialog_ok`, which measured it against the live client.
//
// THE TEXT IS COPIED BYTE FOR BYTE. The script's line is what the client draws:
// `<$>N</>` is its own token, `<?>`, `<|>` and `</?>` are its own markup, and a
// literal line is CP949 (the three `NO_LOCAL_STRING` lines in
// npc\utility\util_define.scp). Any conversion here - the latin1() the resolved
// English used - would rewrite the token, the markup, or the Korean.
func BuildDialogOK(handle uint32, text string, fade uint8) []byte {
	body := dialogBytes(text)
	out := make([]byte, 9+len(body)+1)
	binary.LittleEndian.PutUint16(out[0:], CmdZCDialogOK)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	out[4] = fade
	binary.LittleEndian.PutUint32(out[5:], handle)
	copy(out[9:], body)
	// out[9+len(body)] stays 0: the NUL terminator.
	return out
}

// BuildDialogSelect builds ZC_DIALOG_SELECT, the NPC's question:
//
//	command(2) length(2) type(1) count(1) handle(4) then NUL-terminated strings
//
// The FIRST string is the prompt and the rest are the choices, so count is
// 1 + len(options). Taken from the client's own TriggerDialogSelect, which walks
// the buffer that way (GENormalNet_Action.cpp).
//
// Every string is copied byte for byte, for the same reason as BuildDialogOK:
// the prompt and the labels are the script's (and the client's) own text.
func BuildDialogSelect(handle uint32, prompt string, options []string, fade uint8) []byte {
	body := make([]byte, 0, 6+len(prompt)+1)
	body = append(body, fade, byte(1+len(options)))
	body = binary.LittleEndian.AppendUint32(body, handle)
	for _, text := range append([]string{prompt}, options...) {
		body = append(body, dialogBytes(text)...)
		body = append(body, 0)
	}

	out := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint16(out[0:], CmdZCDialogSel)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	copy(out[4:], body)
	return out
}

// BuildDialogClose builds ZC_DIALOG_CLOSE - the bare command, 2 bytes.
//
// Without it the client stays in modal dialog mode: the camera is locked and the
// player cannot move. It is sent when the player dismisses the box, so this slice
// does not send it; it lives here so the acknowledgement path has it ready.
func BuildDialogClose() []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out[0:], CmdZCDialogClose)
	return out
}

// BuildNPCClickVoice builds ZC_PLAY_NPC_CLICK_VOICE: the NPC's "click" voice,
// sent right after the dialog, as the live server does (capture `liveclick4`).
// Fixed 6 bytes: command(2) handle(4).
func BuildNPCClickVoice(handle uint32) []byte {
	out := make([]byte, 6)
	binary.LittleEndian.PutUint16(out[0:], CmdZCNPCClickVox)
	binary.LittleEndian.PutUint32(out[2:], handle)
	return out
}

// Coin is the coin a NORMAL shop window charges.
//
// The values ARE the client's own coin dword: it sits at +7 of the 3680 window
// and nowhere else. The buy confirm (CZ_BUYITEM) does not carry it at all -
// shopType is 0 for a vis shop and for a Feso shop alike - so the window send is
// the only moment this side can learn it, and the session has to remember it for
// the buy.
type Coin uint8

const (
	CoinVis  Coin = 0 // SHOP_VIS:  the player pays vis, item class 30000
	CoinFeso Coin = 1 // SHOP_FESO: the player pays Feso, item class 30001
)

// String is how a log line spells the coin, so a refusal says which one it meant.
func (c Coin) String() string {
	if c == CoinFeso {
		return "Feso"
	}
	return "vis"
}

// TradeGroupNameLimit is how many characters of a group name the client's 3680
// reader keeps. It copies 25 bytes from +11, so 24 characters plus the NUL; a
// longer name has no terminator in the copy and runs on into the next field.
const TradeGroupNameLimit = 24

// BuildTradeDialog builds ZC_DIALOG_NPC_TRADE (3680), the normal shop window.
// Fixed 40 bytes, all little-endian, MEASURED from the shipped client's reader:
//
//	Offset  Size  Field
//	0       2     command = 3680
//	2       2     length   = 40
//	4       1     isCashItem (0: a coin shop, not a cash-item shop)
//	5       1     unused
//	6       1     shop mode: 0x01 for a VIS window, 0x00 for FESO
//	7       4     coin type: SHOP_VIS = 0, SHOP_FESO = 1
//	11      25    group name (latin-1, NUL-terminated): the client copies 25
//	              bytes, so only the first 24 characters are read
//	36      4     sellCount = 0  <- the client draws the item list itself
//
// So the old 7-byte prefix `00 00 01 00 00 00 00` IS this header: isCashItem=0,
// unused=0, mode=1, then the vis coin dword.
//
// +6 IS NOT A CONSTANT. Two captures disagree by case and both are right: an NPC
// shop window (Leonardo) carries 0x01, the Feso window the client's own toolbar
// opens carries 0x00. The client never compares that byte to 1 - it passes it as
// the 5th argument of its shop-mode call - which is why both work. So the byte
// follows the coin: 0x01 for CoinVis (what has always worked in game) and 0x00
// for CoinFeso (what the retail server sent).
//
// +36 (sellCount) does stay 0, because the client resolves the stock from its own
// table using the group name. Carrying an item list here is what the CUSTOM window
// (3681) is for.
//
// The group is a raw group name like "ETC_LEONARDO" - it is NOT resolved through
// the text dictionary. `handle` is the NPC the window belongs to; the wire format
// carries no handle, so it is only logged by the caller.
//
// A name longer than TradeGroupNameLimit is CUT here silently; the caller logs the
// cut with the full name (Handler.tradeDialog), because a builder has no logger.
func BuildTradeDialog(handle uint32, group string, coin Coin) []byte {
	name := latin1(group)
	if len(name) > TradeGroupNameLimit {
		name = name[:TradeGroupNameLimit]
	}

	body := make([]byte, 0, 36)
	// +4 isCashItem, +5 unused, and +6 - the shop mode, which is NOT the same for
	// the two coins (see the byte map above).
	mode := byte(0x01)
	if coin == CoinFeso {
		mode = 0x00
	}
	body = append(body, 0x00, 0x00, mode)
	// +7 coin type, written as a dword rather than four literal bytes: that is
	// what makes a Feso window one number away from a vis window.
	var coinWord [4]byte
	binary.LittleEndian.PutUint32(coinWord[:], uint32(coin))
	body = append(body, coinWord[:]...)
	body = append(body, name...) // group name
	body = append(body, 0)       // NUL terminator
	for len(body) < 36 {         // pad body to 36 bytes (+36..+39 stay zero)
		body = append(body, 0)
	}

	out := make([]byte, 40)
	binary.LittleEndian.PutUint16(out[0:], CmdZCDialogNPCTrade)
	binary.LittleEndian.PutUint16(out[2:], 40)
	copy(out[4:], body[:36])
	return out
}

// tradeDialog builds the 3680 window a click script asked for, and says so when
// the group name is longer than the client's reader keeps.
//
// The builder cuts such a name silently (it has no logger) and the cut changes
// WHICH SHOP opens, so the line belongs here, once per window sent, with the full
// name and what the client will actually read. `who` is the script entry that
// asked, so the line points at the NPC.
func (h *Handler) tradeDialog(handle uint32, group string, coin Coin, who string) []byte {
	if n := len(latin1(group)); n > TradeGroupNameLimit {
		h.Log.Printf("zone: %s -> group %q is %d characters and the client's 3680 "+
			"reader keeps only %d, so the window opens as %q (coin %s)",
			who, group, n, TradeGroupNameLimit, string(latin1(group)[:TradeGroupNameLimit]),
			coin)
	}
	return BuildTradeDialog(handle, group, coin)
}

// CustomShopEntry is one line a custom shop offers: the client can buy BuyItem
// by paying PriceItem0 x Count0 or PriceItem1 x Count1. Wire order per record
// is (BuyItem, PriceItem[0], PriceItem[1], Count[0], Count[1]) plus 13 pad
// bytes = 33 bytes.
type CustomShopEntry struct {
	BuyItem    uint32
	PriceItem0 uint32
	PriceItem1 uint32
	Count0     uint32
	Count1     uint32
}

// BuildCustomTradeDialog builds ZC_DIALOG_NPC_CUSTOM_TRADE (3681), the custom
// shop window: an 80-byte header plus sellCount 33-byte records.
//
//	Header (80 bytes):
//	Offset  Size  Field
//	0       2     command = 3681
//	2       2     length   = 80 + 33*sellCount
//	4       4     normalTradeType = SHOP_VIS (0)
//	8       4     sellCount
//	12      4     CustomShopType = CUSTOM_SHOP_NORMAL (0)
//	16     64    PropName (64 bytes, NUL-padded; here zero bytes)
//
// The length word is the TOTAL packet length, header included - not the body,
// and not header+4. The Python oracle writes `4 + 76 + 33*n`, which is the same
// number reached a different way: 80 = 4 + 76. A live reference packet for
// CLAUD was 773 bytes = 80 + 21*33.
//
//	Records: sellCount x 33 bytes each
//	Offset  Size  Field
//	0       4     BuyItem
//	4       4     PriceItem[0]
//	8       4     PriceItem[1]
//	12      4     Count[0]
//	16      4     Count[1]
//	20     13    pad (13 zero bytes)
//
// `handle` is the NPC the window belongs to; the wire format carries no
// handle, so it is only logged by the caller.
//
// `entries` are sent IN THE ORDER GIVEN. That order is what a later buy's index
// counts in, so the list is never sorted on the way here or on the way back.
func BuildCustomTradeDialog(handle uint32, entries []CustomShopEntry) []byte {
	sellCount := len(entries)
	header := make([]byte, customWindowHeader)
	binary.LittleEndian.PutUint16(header[0:], CmdZCDialogNPCCustomTrade)
	binary.LittleEndian.PutUint16(header[2:], uint16(customWindowHeader+customEntrySize*sellCount))
	binary.LittleEndian.PutUint32(header[4:], 0) // normalTradeType = SHOP_VIS
	binary.LittleEndian.PutUint32(header[8:], uint32(sellCount))
	binary.LittleEndian.PutUint32(header[12:], 0) // CustomShopType = CUSTOM_SHOP_NORMAL
	// header[16:80] = PropName, 64 zero bytes (no group name is passed in)

	out := make([]byte, 0, customWindowHeader+customEntrySize*sellCount)
	out = append(out, header...)
	for _, e := range entries {
		rec := make([]byte, customEntrySize)
		binary.LittleEndian.PutUint32(rec[0:], e.BuyItem)
		binary.LittleEndian.PutUint32(rec[4:], e.PriceItem0)
		binary.LittleEndian.PutUint32(rec[8:], e.PriceItem1)
		binary.LittleEndian.PutUint32(rec[12:], e.Count0)
		binary.LittleEndian.PutUint32(rec[16:], e.Count1)
		// rec[20:33] stays 0: 13 pad bytes
		out = append(out, rec...)
	}
	return out
}

// dialogBytes is the script's dialog text, unchanged.
//
// The corpus is CP949 and a Go string holds its bytes as they were read, so
// []byte(s) is the line the script wrote. This is NOT latin1(): that maps every
// UTF-8 rune above 0xFF to '?', and a CP949 byte is not UTF-8 at all, so a
// Korean line would arrive as one '?' per byte. The dialog is the one place the
// client does its own resolving and its own decoding; every other text this
// zone sends still goes through latin1()/cp949() as before.
func dialogBytes(s string) []byte { return []byte(s) }

// latin1 turns a Go string into the bytes the client reads.
//
// The client's dialog text is single-byte; the Python server encodes with
// latin-1 and a replacement character for anything outside it, and the text we
// send comes from the client's own English dictionary, so the two agree. A
// multi-byte character becomes '?' rather than shifting every later byte.
//
// NOT for dialog text any more (dialogBytes above): a dialog carries the
// script's tokens and CP949 bytes untouched. This is for the single-byte
// fields this side builds itself, e.g. a shop group name.
func latin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r <= 0xFF {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return out
}

// What a click script asked for, in the zone's own words. The script package has
// the same three kinds; keeping them here is what lets the zone be tested with a
// stub instead of a Lua runtime. They must agree, and main.go maps them.
const (
	ClickDialogNone   = ""
	ClickDialogOK     = "ok"
	ClickDialogSelect = "select"

	// Window kinds a click script can ask the client to open. They mirror
	// script.WindowShop / script.WindowShopCustom but the zone keeps its own
	// copies so it can be tested without the Lua engine. main.go maps the
	// script's strings into ClickReply.WindowKind.
	WindowNone       = ""
	WindowShop       = "normal" // ShowNPCTradeDlg   -> ZC_DIALOG_NPC_TRADE   (3680)
	WindowShopCustom = "custom" // ShowCustomNPCTradeDlg -> ZC_DIALOG_NPC_CUSTOM_TRADE (3681)
	// WindowWaypoint is the town-teleport grid: ShowWayPointList, answered with
	// ZC_WAYPOINT_LIST (3742) and ZC_UI_OPEN (3753). See commwarp.go.
	WindowWaypoint = "waypoint"
)

// customWindowPacket is ZC_DIALOG_NPC_CUSTOM_TRADE's header size, and
// customEntrySize is one sell record (five words plus 13 pad bytes). Both are
// proven by a live reference capture: CLAUD's window was 773 bytes = 80 + 21*33.
// Source2's 20-byte stride is what crashed the client.
const (
	customWindowHeader = 80
	customEntrySize    = 33
)

// ClickObject is one actor as a click script sees it.
type ClickObject struct {
	Handle uint32
	Name   string
	Script string
	Level  int
	// Commander is whose properties `pc.<Name>` reads and whose transaction a
	// script's `TxEnd` writes. Only the PLAYER's object carries one: an NPC has
	// no commander, and 0 means "not known".
	Commander int32
	X         int32
	Y         int32
	Z         int32
	// HP is the character's MAXIMUM HP, and it is here because the Scarecrow's own
	// script calibrates the dummy against it: `mhp = stat1.HP * 100 - 100`
	// (`out\lua_corpus\npc\evenpc\auch\npc_hitdummy_creater.lua`, the game's own
	// damage meter). Only the LEADER's object carries one; 0 means "not known".
	HP float64
}

// ClickReply is what the script asked the client to show.
type ClickReply struct {
	DialogKind string
	Text       string   // DialogOK
	Prompt     string   // DialogSelect
	Options    []string // DialogSelect
	Fade       uint8

	// WindowKind and WindowGroup are a panel the script opened (a shop, a
	// lobby). WindowShop is answered with ZC_DIALOG_NPC_TRADE (3680) from the
	// group name alone; WindowShopCustom is answered with
	// ZC_DIALOG_NPC_CUSTOM_TRADE (3681), whose rows come from
	// data\custom_shops.json. A custom group with no rows loaded is answered
	// with ZC_DIALOG_CLOSE only - the client must never be left modal.
	WindowKind  string
	WindowGroup string

	// WindowCoin is the coin the window charges. CoinVis is every shop whose
	// script does not say otherwise; CoinFeso comes from the script's 4th argument
	// naming 'FesoItem'. The buy confirm does NOT carry the coin, so the window
	// send is the only moment it reaches this side, and the session remembers it.
	WindowCoin Coin

	// Superseded counts dialogs the script asked for after the first one.
	Superseded int

	// BadText counts dialog requests whose text was missing or numeric - a helper
	// that answered 0. They are NOT shown: a box reading "0" is not an NPC's line.
	BadText int

	// Properties are the commander properties a COMMITTED script transaction
	// moved. The Lock_* ones are pushed to the client, which caches them.
	Properties []Property

	// Items are the bag changes a COMMITTED transaction made. The zone re-sends
	// the inventory list so the client shows them.
	Items []item.Change

	// Suspended is true when the script yielded at ShowSelDlg and is waiting
	// for the player to pick an option. The zone sends ZC_DIALOG_SELECT now
	// and resumes the script when CZ_DIALOG_SELECT / CZ_ACK_DIALOG arrives.
	Suspended bool
}

// clickTrigger answers CZ_CLICK_TRIGGER - the player clicked an NPC.
//
// Two outcomes: within reach it talks, and beyond reach it WALKS there and talks
// on arrival (chase.go). The walk is the client's job to animate but this side's
// job to ask for, because the client stays silent for a clicked trigger.
func (h *Handler) clickTrigger(sess *Session, f protocol.Frame) ([][]byte, error) {
	charIndex, longDistance, handle, ok := ParseClickTrigger(f.Data)
	if !ok {
		h.Log.Printf("zone: CLICK_TRIGGER is %d bytes, too short to carry a handle - ignored",
			len(f.Data))
		return nil, nil
	}

	// A NEW CLICK ENDS THE OLD BOX (2026-11-09, the owner: "the dialog straight up open").
	// The client closes any open dialog when the player clicks somewhere else and answers it
	// with CZ_ACK_DIALOG(0); the server then RESUMES the suspended script, and a script that
	// re-shows its dialog pops the OLD NPC's box on top of the new click. Measured:
	// logs\zone_go_10.log 17:48:20.441 click on GRANMA, .994 SCR_CLICK_LEO_SERVICE sent its
	// box - Leonardo's window, 0.5 s after a click on the event NPC, while the walk to the
	// event NPC was still running. Dropping the suspended click here means that late answer
	// resumes nothing and only the NPC the player actually clicked can open a box.
	if sess.Suspended != nil {
		if h.ReleaseClick != nil {
			h.ReleaseClick()
		}
		sess.Suspended = nil
	}

	// A CLICK REPLACES A KEPT PRESS (zone\attack.go, holdPress). The player has aimed at
	// something else; the reference's single request slot is overwritten by the new
	// request, so the press kept for the old aim is cleared and nothing is sent for it.
	//
	// THE CHARACTER IS THE ONE THE PACKET NAMES, not the one being driven: after a
	// CZ_SELECTCHANGE_PC the two can differ, and clearing the wrong slot would keep a
	// follower's press alive (it would swing late) while eating the driven character's
	// (it never would). The slot falls back to Control only when the packet names one
	// this session does not have.
	pressSlot := charIndex
	if pressSlot < 0 || pressSlot >= len(sess.Team) {
		pressSlot = sess.Control
	}
	h.clearPress(sess, pressSlot, "the player clicked an NPC/trigger (handle 0x"+
		strconv.FormatUint(uint64(handle), 16)+"), which replaces what the character was "+
		"asking to do")

	npc, found := h.npcByHandle(handle)
	if !found {
		// Not an NPC of ours: a trigger, a quest object, a waypoint. Saying so is
		// the whole point - an unanswered click on a KNOWN NPC looks like a bug.
		h.Log.Printf("zone: click on handle 0x%08x - not one of the %d town NPC(s)",
			handle, len(h.NPCs))
		return nil, nil
	}

	if !InClickRange([2]int32{sess.Pos[0], sess.Pos[1]}, [2]int32{npc.X, npc.Y}, longDistance) {
		h.Log.Printf("zone: click on %s (0x%08x) is %.0f units away, reach is %.0f - walking",
			describeNPC(npc), handle, npcDistance(sess.Pos, npc), ClickRangeFor(longDistance))
		return h.queueClick(sess, npc), nil
	}

	// AN IN-RANGE CLICK STILL OBEYS THE LOCK (2026-11-09, the owner: "the box still opens mid
	// walk"). This arm used to open the dialog IMMEDIATELY, without consulting the walk in
	// flight - and the client re-sends a click on the NPC that was under the cursor, so a
	// click on Leonardo that arrived 0.7 s after a click on the event NPC popped Leonardo's
	// box in the middle of the walk away from him (logs\zone_go_10.log 17:51:17.704 click
	// GRANMA 888 away -> 17:51:18.387 SCR_CLICK_LEO_SERVICE). While a click walk is in
	// flight, an in-range click is the same event as an out-of-range one: it ARMS (first
	// click, box dropped, nothing moves) and only the second click redirects.
	if w := sess.Walk; w != nil && w.cast == nil && w.atk == nil && w.pick == nil &&
		w.npc.Handle != npc.Handle {
		return h.queueClick(sess, npc), nil
	}
	return h.openNPCDialog(sess, npc), nil
}

// openNPCDialog runs the NPC's click script and answers with what it asked for.
//
// Separate from clickTrigger because a CHASE arrives here too, seconds later and
// with no packet of its own: the click and the arrival are the same event from the
// player's side.
func (h *Handler) openNPCDialog(sess *Session, npc NPC) [][]byte {
	handle := npc.Handle
	self := ClickObject{
		Handle: npc.Handle, Name: npc.NameToken, Script: npc.Script,
		X: npc.X, Y: npc.Y, Z: npc.Cell,
	}
	pc := ClickObject{Level: 0, X: sess.Pos[0], Y: sess.Pos[1], Z: sess.Pos[2],
		Commander: sess.CommanderID}
	if len(sess.Team) > 0 {
		pc.Handle = sess.Team[0].Handle
		pc.Name = sess.Team[0].Name
		pc.Level = int(sess.Team[0].Level)
		pc.HP = sess.Team[0].MaxHP
	}

	if npc.Script == "" {
		h.Log.Printf("zone: %s (0x%08x) has NO script - every click on it does nothing",
			describeNPC(npc), handle)
		return nil
	}
	// A warp NPC is a DOOR, not a dialog. Its Script names a map, so its behaviour is a
	// map change and belongs to the zone tick (gate.go), not to a click script: the
	// client's own entry for these is `SCR_ENTER_<Script>`, and no `SCR_CLICK_` exists
	// for either arrow in the corpus. Running the click entry here would answer the
	// player with nothing - or, for a script that warps, with a half-done map change.
	//
	// Gated HERE and not in clickTrigger, because a CHASE arrives at this function too
	// (chase.go): a click from across the map walks up to the door and must still not
	// run its script.
	if _, _, isWarp := h.warpMapName(npc); isWarp {
		h.Log.Printf("zone: click on %s (0x%08x) - its script %q is a zone warp, so no "+
			"click script is run: walking into it is what changes the map",
			describeNPC(npc), handle, npc.Script)
		return nil
	}
	if h.Click == nil {
		h.Log.Printf("zone: click on %s (0x%08x) but NO script engine is wired - "+
			"started without one?", describeNPC(npc), handle)
		return nil
	}

	entry := "SCR_CLICK_" + npc.Script
	reply, err := h.Click(entry, self, pc)
	if err != nil {
		// A script that raised may still have asked for a line before it did, so
		// the reply is shown. The error goes to the log with its name: the count of
		// broken click scripts is the worklist, and it must not be silent.
		h.Log.Printf("zone: %s FAILED: %v", entry, err)
	}
	if reply.Superseded > 0 {
		h.Log.Printf("zone: %s asked for %d more dialog(s) after the first - the choice "+
			"cannot be resumed yet, so only the first is shown",
			entry, reply.Superseded)
	}

	// A COMMITTED transaction may have moved a property the client CACHES:
	// `Lock_InventoryCount` is the bag's capacity, and the client only learns it
	// from 3748 - a database write alone leaves the window showing its table
	// default. Pushed BEFORE the dialog, the same order the Python oracle uses
	// (`flush_property_events` runs before the box).
	//
	// Only Lock_* are pushed because only they are cached that way; moving a
	// quest counter is told to the client by the quest array, not by this packet.
	var pushed [][]byte
	for _, p := range reply.Properties {
		if !strings.HasPrefix(p.Name, "Lock_") {
			continue
		}
		pushed = append(pushed, BuildUpdatePropertyNumber(p.Name, p.Value))
		h.Log.Printf("zone: %s committed %s=%d - pushed 3748 so the client's "+
			"cache moves", entry, p.Name, p.Value)
	}
	// A committed give/take must be SEEN, and it must be seen the way the client
	// expects: as an INCREMENTAL change to a position it already knows, not as a
	// fresh list (which re-lays the bag and leaves an open shop's counters stale).
	//
	// The position comes from `sess.Inv`, the mirror of the client's key space -
	// never from the database order, because a take deletes a row and the client
	// keeps that position as a hole.
	//
	// addType 12 (CUSTOM_SHOP_BUY) is what makes the client print its OWN "you
	// received X" line. We do not write that text; the client builds it from its
	// item table.
	if len(reply.Items) > 0 {
		if sess.Inv == nil {
			h.Log.Printf("zone: %s moved %d stack(s) but this session has no bag "+
				"mirror (no startup inventory), so the client is not told", entry,
				len(reply.Items))
		} else {
			for _, pkt := range sess.Inv.PacketsFor(reply.Items) {
				pushed = append(pushed, pkt)
			}
			for _, c := range reply.Items {
				h.Log.Printf("zone: %s %s class=%d item=%d newTotal=%d - sent %s",
					entry, itemVerb(c.Give), c.ClassID, c.ItemID, c.Total,
					itemPacketName(c.Give))
			}
		}
	}

	// A script that yielded at ShowSelDlg is waiting for the player's choice.
	// Send the dialog now and remember the suspension so CZ_DIALOG_SELECT /
	// CZ_ACK_DIALOG can resume it down the chosen branch.
	if reply.Suspended {
		sess.Suspended = &suspendedClick{entry: entry, handle: handle}
		sess.DialogHandle = handle
		out := buildDialogPackets(handle, reply)
		h.Log.Printf("zone: %s -> DIALOG_SELECT on 0x%08x, %d option(s): %q (suspended)",
			entry, handle, len(reply.Options), h.Text.Resolve(reply.Prompt))
		return append(pushed, out...)
	}

	switch reply.DialogKind {
	case ClickDialogSelect:
		out := BuildDialogSelect(handle, reply.Prompt, reply.Options, reply.Fade)
		sess.DialogHandle = handle
		h.Log.Printf("zone: %s -> DIALOG_SELECT on 0x%08x, %d option(s): %q",
			entry, handle, len(reply.Options), h.Text.Resolve(reply.Prompt))
		return append(pushed, out, BuildNPCClickVoice(handle))
	case ClickDialogOK:
		out := BuildDialogOK(handle, reply.Text, reply.Fade)
		sess.DialogHandle = handle
		h.Log.Printf("zone: %s -> DIALOG_OK on 0x%08x: %q",
			entry, handle, h.Text.Resolve(reply.Text))
		return append(pushed, out, BuildNPCClickVoice(handle))
	}

	if reply.WindowKind != "" {
		switch reply.WindowKind {
		case WindowShop:
			// Normal shop: the group name alone is enough to draw the window, and
			// the coin comes from the script's own 4th argument (Feso or vis).
			out := h.tradeDialog(handle, reply.WindowGroup, reply.WindowCoin, entry)
			// Remember WHICH shop this is: a buy names its item by its INDEX in
			// this list, so without it the record cannot be resolved - and the
			// coin says which stack pays for it.
			sess.LastShop = &ShopState{Group: reply.WindowGroup, Kind: WindowShop,
				Coin: reply.WindowCoin, Items: h.ShopGroups.Items(reply.WindowGroup)}
			h.Log.Printf("zone: %s -> TRADE window on 0x%08x, group %q, coin %s (ZC_DIALOG_NPC_TRADE, 3680, 40 bytes)",
				entry, handle, reply.WindowGroup, reply.WindowCoin)
			return append(pushed, out)
		case WindowShopCustom:
			// Custom shop: the window carries its own rows, so the group is
			// looked up in the table loaded at startup. Without rows there is
			// nothing to draw and nothing to send - this click opened no box, so
			// the client is not modal and silence is safe here (unlike the
			// resumed branch, where a box was just answered).
			entries := h.CustomShops.Entries(reply.WindowGroup)
			if len(entries) == 0 {
				h.Log.Printf("zone: %s asked for a CUSTOM trade window %q but no rows "+
					"are loaded for that group (data\\custom_shops.json, built by "+
					"src\\emit_shop_data.py) - sending nothing", entry, reply.WindowGroup)
				return pushed
			}
			out := BuildCustomTradeDialog(handle, entries)
			// The index a barter buy names counts in THIS list, so the rows are
			// kept in the order they were sent.
			sess.LastShop = &ShopState{Group: reply.WindowGroup, Kind: WindowShopCustom,
				Entries: entries}
			h.Log.Printf("zone: %s -> CUSTOM trade window on 0x%08x, group %q, %d row(s) "+
				"(ZC_DIALOG_NPC_CUSTOM_TRADE, 3681, %d bytes)",
				entry, handle, reply.WindowGroup, len(entries), len(out))
			return append(pushed, out)
		case WindowWaypoint:
			// The town grid. The live server's order is the list FIRST, then the
			// command that opens the frame (`CCommander::ShowWayPointList`,
			// Commander.cpp:654-658: SendWayPointList(); OpenUi("waypointlist")).
			// Reversing it opens an empty window and the player sees 0 towns.
			//
			// The rows are the game's own town table, not the script's: the script
			// only says "the player asked for this window".
			towns, skipped := h.ZonePoints.TownGrid()
			if len(towns) == 0 {
				h.Log.Printf("zone: %s asked for the town-teleport window but no town rows "+
					"are loaded (data\\zone_points.json version 4, built by "+
					"src\\emit_zone_points.py) - sending NOTHING rather than an empty frame: "+
					"an empty grid with a dead Save button is not the live server's window",
					entry)
				return pushed
			}
			list := BuildWaypointList(towns)
			h.Log.Printf("zone: %s -> TOWN grid on 0x%08x: %d town(s), %d bytes "+
				"(ZC_WAYPOINT_LIST, 3742, %dx%d + %d header), then ZC_UI_OPEN (3753) for %q",
				entry, handle, len(towns), len(list), len(towns), waypointRecord,
				waypointHeader, WaypointListFrame)
			if len(skipped) > 0 {
				h.Log.Printf("zone: %s: %d waypoint row(s) are not towns (Tab 1) or name no "+
					"map - they are NOT in the grid: %s", entry, len(skipped), describeTowns(skipped))
			}
			// The tax badge on each thumbnail is the COLONY HOLDER's tax rate, and
			// this packet has no field for it - so nothing here is invented. There is
			// no colony system yet, and the client draws the badge from its own state.
			return append(pushed, list, BuildUIOpen(WaypointListFrame))
		default:
			h.Log.Printf("zone: %s asked for an unknown window kind %q (%q) - sending nothing",
				entry, reply.WindowKind, reply.WindowGroup)
			return pushed
		}
	}
	if reply.BadText > 0 {
		h.Log.Printf("zone: %s asked for %d dialog(s) with no usable text - a builtin "+
			"it calls answered 0, so there is nothing worth showing", entry, reply.BadText)
		return pushed
	}
	if err != nil {
		// The script did not finish, so silence here is a BUG, not a design. Saying
		// "legitimately does nothing" after a failure is the kind of line that makes
		// a broken NPC look intended (seen in the first live session, 2026-10-25).
		h.Log.Printf("zone: %s broke before it asked for anything - the click is silent "+
			"because of the error above, NOT by design", entry)
		return pushed
	}
	h.Log.Printf("zone: %s asked for nothing - the click legitimately does nothing "+
		"(no dialog, no window)", entry)
	return pushed
}
func itemVerb(give bool) string {
	if give {
		return "gave"
	}
	return "took"
}

// itemPacketName is the packet a change is announced with.
func itemPacketName(give bool) string {
	if give {
		return "ZC_ADDITEM (3607)"
	}
	return "ZC_REMOVEITEM (3608)"
}

// npcByHandle finds the town NPC a click refers to.
//
// It searches the NPCs a SCRIPT created first, then the map's own list: both are
// NPCs to the client, and a click on a Scarecrow dummy arrives with a handle that
// only the runtime list holds. The runtime list is copied out from under its mutex
// (zone\create.go), because another connection may be creating one right now.
func (h *Handler) npcByHandle(handle uint32) (NPC, bool) {
	for _, npc := range h.runtimeNPCs() {
		if npc.Handle == handle {
			return npc, true
		}
	}
	for _, npc := range h.NPCs {
		if npc.Handle == handle {
			return npc, true
		}
	}
	return NPC{}, false
}

// npcDistance is the plane distance between the player and an NPC.
func npcDistance(pc [3]int32, npc NPC) float64 {
	dx := float64(npc.X) - float64(pc[0])
	dy := float64(npc.Y) - float64(pc[1])
	return math.Hypot(dx, dy)
}

// describeNPC names an NPC in a log line: class type, and the script when it has
// one, so "which NPC was that" needs no second lookup.
func describeNPC(npc NPC) string {
	if npc.Script == "" {
		return npc.ClassType
	}
	return npc.ClassType + "/" + npc.Script
}
