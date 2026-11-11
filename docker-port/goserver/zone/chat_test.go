package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"log"
	"strings"
	"testing"

	"geserver/item"
	"geserver/protocol"
)

// frameChat is CZ_CHAT as the client sends it:
//
//	cmd(2) seq(4) checksum(4) len(2) BalloonColor(4) ChatType(1) text + NUL
//
// `pack` writes the command and the 8 security bytes (10 in total), so the body
// here starts at +10. The length word is the WHOLE packet, which is what the
// retail capture shows (27 bytes for "/fesoshop").
func frameChat(text string) protocol.Frame {
	body := []byte{0, 0}         // +10: the length word, filled in below
	body = appendU32(body, 0)    // +12: BalloonColor
	body = append(body, 0)       // +16: ChatType
	body = append(body, text...) // +17: the text
	body = append(body, 0)       // NUL terminator
	pt := pack(CmdCZChat, body...)
	binary.LittleEndian.PutUint16(pt[10:], uint16(len(pt)))
	return protocol.Frame{Cmd: CmdCZChat, Size: len(pt), Data: pt}
}

// TestChatFesoShop is the whole reason CZ_CHAT is handled: the toolbar's "Feso
// Shop" button sends no shop packet, it calls Chat("/fesoshop"), and the window
// only appears because the server answers that line with ONE 40-byte 3680.
func TestChatFesoShop(t *testing.T) {
	newHandler := func() (*Handler, *bytes.Buffer) {
		buf := &bytes.Buffer{}
		return &Handler{
			Log:        log.New(buf, "", 0),
			ShopGroups: ShopGroups{"FESO_ITEMSHOP": {60081, 60082}},
		}, buf
	}

	for _, text := range []string{"/fesoshop", "  /FesoShop  "} {
		h, _ := newHandler()
		sess := &Session{}
		out, err := h.Reply(context.Background(), sess, frameChat(text))
		if err != nil {
			t.Fatalf("%q: Reply: %v", text, err)
		}
		if len(out) != 1 {
			t.Fatalf("%q: %d packet(s), want exactly the one window: %d",
				text, len(out), len(out))
		}
		got := out[0]
		if len(got) != 40 {
			t.Fatalf("%q: window is %d bytes, want 40", text, len(got))
		}
		if cmd := cmdOf(got); cmd != CmdZCDialogNPCTrade {
			t.Fatalf("%q: cmd = %d, want %d", text, cmd, CmdZCDialogNPCTrade)
		}
		if n := binary.LittleEndian.Uint16(got[2:]); n != 40 {
			t.Fatalf("%q: length word = %d, want 40", text, n)
		}
		// +6 is the Feso shop mode, +7 the FESO coin dword - the two bytes that
		// make this the window the retail server sent and not an NPC shop's.
		if got[6] != 0x00 {
			t.Fatalf("%q: +6 = %d, want 0 for a Feso window", text, got[6])
		}
		if coin := binary.LittleEndian.Uint32(got[7:]); coin != 1 {
			t.Fatalf("%q: coin dword = %d, want 1 (SHOP_FESO)", text, coin)
		}
		// The name, NUL-terminated, then sellCount 0: the client draws the stock.
		wantName := append([]byte(fesoShopGroup), 0)
		if !bytes.HasPrefix(got[11:], wantName) {
			t.Fatalf("%q: name = %q, want %q", text, got[11:36], fesoShopGroup)
		}
		if s := binary.LittleEndian.Uint32(got[36:]); s != 0 {
			t.Fatalf("%q: sellCount = %d, want 0", text, s)
		}

		// The session must remember the shop, or the buy that follows cannot turn
		// its index into an item.
		if sess.LastShop == nil {
			t.Fatalf("%q: the window was sent but LastShop is nil", text)
		}
		if sess.LastShop.Group != fesoShopGroup || sess.LastShop.Coin != CoinFeso ||
			sess.LastShop.Kind != WindowShop {
			t.Fatalf("%q: LastShop = %+v, want the Feso %q window",
				text, sess.LastShop, fesoShopGroup)
		}
		if len(sess.LastShop.Items) != 2 || sess.LastShop.Items[0] != 60081 {
			t.Fatalf("%q: LastShop.Items = %v, want [60081 60082]",
				text, sess.LastShop.Items)
		}
	}
}

// TestChatFesoShopWithoutData sends nothing rather than an empty window: a window
// with no list opens and then refuses every buy, which looks like a broken shop.
func TestChatFesoShopWithoutData(t *testing.T) {
	h := &Handler{Log: log.New(&bytes.Buffer{}, "", 0)}
	out, err := h.Reply(context.Background(), &Session{}, frameChat("/fesoshop"))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) for a group with no rows, want 0", len(out))
	}
}

// TestChatOtherTextIsIgnored: chat is not relayed, echoed or guessed at. Common
// text produces NO packet, and it is logged ONCE per distinct line so a player
// talking normally cannot flood the log.
func TestChatOtherTextIsIgnored(t *testing.T) {
	buf := &bytes.Buffer{}
	h := &Handler{Log: log.New(buf, "", 0),
		ShopGroups: ShopGroups{"FESO_ITEMSHOP": {60081}}}
	sess := &Session{}

	for _, text := range []string{"hello", "hello", "HELLO", " hello "} {
		out, err := h.Reply(context.Background(), sess, frameChat(text))
		if err != nil {
			t.Fatalf("%q: Reply: %v", text, err)
		}
		if len(out) != 0 {
			t.Fatalf("%q: %d packet(s) for ordinary chat, want 0", text, len(out))
		}
	}
	if sess.LastShop != nil {
		t.Fatalf("ordinary chat set LastShop = %+v", sess.LastShop)
	}
	// "hello", "HELLO" and " hello " are the same line as far as the log is
	// concerned, so exactly ONE line was written.
	if n := strings.Count(buf.String(), "CHAT"); n != 1 {
		t.Fatalf("%d chat log line(s) for one repeated text, want 1:\n%s", n, buf.String())
	}
}

// TestChatJunkDoesNotPanic: a chat packet can be short, unterminated, or binary.
// None of it may index past the buffer or reach the shop logic by accident, and a
// text with no NUL is still read (up to the end of the frame).
func TestChatJunkDoesNotPanic(t *testing.T) {
	h := &Handler{Log: log.New(&bytes.Buffer{}, "", 0),
		ShopGroups: ShopGroups{"FESO_ITEMSHOP": {60081}}}

	cases := [][]byte{
		nil,
		{},
		{0x30, 0x0f},                         // the command only
		pack(CmdCZChat),                      // a bare 10-byte header
		pack(CmdCZChat, 0, 0, 0, 0, 0, 0, 0), // 17 bytes: no text at all
		pack(CmdCZChat, 0, 0, 0, 0, 0, 0, 0xff, 0xfe, 0x00), // junk, then a NUL
	}
	for i, data := range cases {
		f := protocol.Frame{Cmd: CmdCZChat, Size: len(data), Data: data}
		if out, err := h.Reply(context.Background(), &Session{}, f); err != nil || len(out) != 0 {
			t.Fatalf("case %d (%d bytes): %d packet(s), err %v; want none", i, len(data), len(out), err)
		}
	}

	// A text with no NUL is read to the end of the frame, so the button still works
	// on a client that omits the terminator.
	noNul := frameChat("/fesoshop")
	noNul.Data = noNul.Data[:len(noNul.Data)-1]
	noNul.Size = len(noNul.Data)
	out, err := h.Reply(context.Background(), &Session{}, noNul)
	if err != nil || len(out) != 1 || cmdOf(out[0]) != CmdZCDialogNPCTrade {
		t.Fatalf("an unterminated %q gave %d packet(s), err %v; want the window",
			"/fesoshop", len(out), err)
	}
}

// TestChatThenBuy is the pair that matters: after /fesoshop the session holds the
// FESO_ITEMSHOP list, so the buy's index resolves against it AND the cart is
// charged in Feso (class 30001) instead of vis.
func TestChatThenBuy(t *testing.T) {
	f := newFakeShop(
		item.Stack{ItemID: 101, ClassID: VisClass, Count: 1_000_000},
		item.Stack{ItemID: 301, ClassID: FesoClass, Count: 10_000},
	)
	h := shopHandler(f, testPrices(t))
	h.FesoPrices = testFesoPrices(t)
	h.ShopGroups = ShopGroups{"FESO_ITEMSHOP": {21613}}
	sess := &Session{CommanderID: 42, Inv: NewInvMirror(f.stacks)}

	if out, err := h.Reply(context.Background(), sess, frameChat("/fesoshop")); err != nil ||
		len(out) != 1 {
		t.Fatalf("the chat did not open the shop: %d packet(s), err %v", len(out), err)
	}
	out, err := h.Reply(context.Background(), sess, frameBuy(ShopVis, 0,
		BuyRecord{Count: 2, Index: 0})) // 2 x Fire stone at 700 Feso
	if err != nil {
		t.Fatalf("buyItem: %v", err)
	}

	const cost = 2 * 700
	if len(f.ops) != 2 || f.ops[0].ClassID != FesoClass || f.ops[0].Count != cost {
		t.Fatalf("ops = %+v, want %d out of the Feso row", f.ops, cost)
	}
	if f.held(101) != 1_000_000 {
		t.Fatalf("the vis row holds %d - a Feso cart paid vis", f.held(101))
	}
	if len(out) != 2 || out[0][25] != AddItemBuy || out[1][10] != RemoveItemBuy {
		t.Fatalf("reply = %d packet(s), want the goods (addType 1) then the payment (removeType 2)",
			len(out))
	}
}
