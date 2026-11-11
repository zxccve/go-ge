package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"

	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// TestServeAnswersOverTheWire drives the real socket loop: real cipher, real
// command table, real framing. It is the closest thing to a client test that can
// run without the client, and it catches the class of mistake the unit tests
// cannot - a packet that is right but never reaches the wire.
func TestServeAnswersOverTheWire(t *testing.T) {
	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}

	h := &Handler{
		Table:  table,
		Cipher: protocol.DefaultCipher,
		Log:    log.New(io.Discard, "", 0),
		TeamOf: func(_ context.Context, accountID uint32) ([]Character, error) {
			if accountID != 4242 {
				t.Errorf("TeamOf called with account %d, want 4242", accountID)
			}
			return []Character{
				{Handle: 34, Name: "Jin", Job: 3, Level: 100},
				{Handle: 40, Name: "Fighter", Job: 1, Level: 100},
				{Handle: 38, Name: "Warlock", Job: 5, Level: 100},
			}, nil
		},
	}

	server, client := net.Pipe()
	defer client.Close()
	go h.Serve(context.Background(), server)

	// -- CZ_START_GAME -----------------------------------------------------
	start := make([]byte, 40)
	binary.LittleEndian.PutUint16(start, CmdCZStartGame)
	binary.LittleEndian.PutUint32(start[10:], 0xABCD)
	binary.LittleEndian.PutUint32(start[14:], 4242)
	copy(start[18:], "archi")

	sendWire(t, client, start, table)

	// The reply is the entry packet followed by the entry burst, all plaintext and
	// framed by the table. Read the entry packet by its own length word, then the
	// burst by the exact sizes the builders produce - no guessing at offsets.
	head := readN(t, client, 4)
	if cmd := binary.LittleEndian.Uint16(head); cmd != CmdZCStartGameOK {
		t.Fatalf("answer is cmd %d, want %d", cmd, CmdZCStartGameOK)
	}
	entryLen := int(binary.LittleEndian.Uint16(head[2:]))
	if entryLen != headerSize+3*startInfoSize {
		t.Fatalf("entry packet is %d bytes, want %d (36 + 3 records with their "+
			"stance and skill blocks)", entryLen, headerSize+3*startInfoSize)
	}
	entry := append(head, readN(t, client, entryLen-4)...)
	if n := entry[27]; n != 3 {
		t.Fatalf("character count = %d, want 3", n)
	}
	if name := trimNul(entry[headerSize+siName : headerSize+siName+nameLen]); name != "Jin" {
		t.Fatalf("first character = %q, want Jin", name)
	}

	// NO ZC_UPDATE_ALL_STATUS (3684) HERE. That burst answers CZ_GAME_READY, not
	// this reply: the client's own 3684 handler drops every record while its
	// record-count gate is still 0, which is the state during START_GAME. Its
	// absence is asserted again in status_placement_test.go - and the socket
	// order after this point proves it too, because the burst is read below.
	burst, _ := EntryBurst(nil, nil)
	burstTotal := 0
	for _, p := range burst {
		burstTotal += len(p)
	}
	got := readN(t, client, burstTotal)
	// The burst must arrive whole and end where the builders say it does.
	if cmd := binary.LittleEndian.Uint16(got); cmd != CmdZCCommanderPoint {
		t.Fatalf("burst starts with cmd %d, want %d", cmd, CmdZCCommanderPoint)
	}
	if cmd := binary.LittleEndian.Uint16(got[len(got)-6:]); cmd != CmdZCCommWarpList {
		t.Fatalf("burst ends with cmd %d, want %d",
			binary.LittleEndian.Uint16(got[len(got)-6:]), CmdZCCommWarpList)
	}

	// -- CZ_GAME_READY -----------------------------------------------------
	// 13 bytes in the client's own table: a fixed-size packet, so a 10-byte
	// version would leave 3 bytes in the buffer and misalign everything after it.
	ready := make([]byte, 13)
	binary.LittleEndian.PutUint16(ready, CmdCZGameReady)
	sendWire(t, client, ready, table)
	if got := readN(t, client, 2); binary.LittleEndian.Uint16(got) != CmdZCEnterLegacyObjs {
		t.Fatalf("GAME_READY answer is cmd %d, want %d",
			binary.LittleEndian.Uint16(got), CmdZCEnterLegacyObjs)
	}

	// ONE ZC_UPDATE_ALL_STATUS (3684) per character follows the world-entry
	// handshake: the DERIVED half of the Character Info sheet, which the panel
	// reads and the client never computes. Fixed 125 bytes each, so they are read
	// by that size, never by guessing at offsets.
	//
	// EACH ONE IS FOLLOWED BY ITS ZC_PROMOTION_INFO (3093, 25 bytes): the sheet has
	// no promotion field, so that is where the party card's rank line comes from.
	// Reading only the 3684s would leave 25 bytes per character in the buffer and
	// misalign every packet after them.
	for i := 0; i < 3; i++ {
		rec := readN(t, client, UpdateAllStatusSize)
		if cmd := binary.LittleEndian.Uint16(rec); cmd != CmdZCUpdateAllStatus {
			t.Fatalf("status record %d after GAME_READY is cmd %d, want %d", i, cmd,
				CmdZCUpdateAllStatus)
		}
		prom := readN(t, client, PromotionInfoSize)
		if cmd := binary.LittleEndian.Uint16(prom); cmd != CmdZCPromotionInfo {
			t.Fatalf("promotion record %d after GAME_READY is cmd %d, want %d", i, cmd,
				CmdZCPromotionInfo)
		}
	}

	// -- CZ_MOVE_PC for a FOLLOWER ----------------------------------------
	move := moveFrame(1, [3]int32{8718, 10303, -1217}).Data
	sendWire(t, client, move, table)

	echo := readN(t, client, 30)
	if cmd := binary.LittleEndian.Uint16(echo); cmd != CmdZCMovePC {
		t.Fatalf("move echo is cmd %d, want %d", cmd, CmdZCMovePC)
	}
	if handle := binary.LittleEndian.Uint32(echo[2:]); handle != 40 {
		t.Fatalf("echo handle = %d, want 40 (the follower that moved)", handle)
	}
	if cell := int32(binary.LittleEndian.Uint32(echo[18:])); cell != -1217 {
		t.Fatalf("echo cell = %d, want -1217", cell)
	}
}

// TestPaddingIsNotLogged is ROADMAP R45 at the zone's own door. The cipher
// pads the plaintext to a whole 8-byte block, so a chunk can end in up to 7
// bytes of rubble that are not zeros. The zone used to hand those bytes to
// Reply as a command and log "cmd 47637 not handled yet" once a second, which
// is how a real CZ_CHAT went missing for an hour.
//
// This replays one chunk's plaintext through the table the zone really loads and
// then through Reply, which is where that line comes from - no socket needed.
func TestPaddingIsNotLogged(t *testing.T) {
	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}

	logs := &bytes.Buffer{}
	h := &Handler{Table: table, Cipher: protocol.DefaultCipher, Log: log.New(logs, "", 0)}

	// A 13-byte CZ_GAME_READY - a packet the zone handles, so its own line
	// proves the real packet still arrived - and the six bytes the live log
	// showed after the end of a packet.
	plain := make([]byte, 19)
	binary.LittleEndian.PutUint16(plain, CmdCZGameReady)
	copy(plain[13:], []byte{0x15, 0xba, 0x96, 0x09, 0x3a, 0xd2})

	frames, _ := table.Split(plain)
	if len(frames) != 1 {
		t.Fatalf("split into %d frames, want only CZ_GAME_READY: %+v", len(frames), frames)
	}
	for _, f := range frames {
		if _, rerr := h.Reply(context.Background(), &Session{}, f); rerr != nil {
			t.Fatalf("reply to cmd %d: %v", f.Cmd, rerr)
		}
	}

	out := logs.String()
	if strings.Contains(out, "not handled yet") {
		t.Fatalf("the padding was logged as a command:\n%s", out)
	}
	if !strings.Contains(out, "GAME_READY") {
		t.Fatalf("the real packet never reached the handler:\n%s", out)
	}
}

// TestRawLogDumpsChunkAndFrames is the -rawlog switch at the zone's own door.
//
// The switch exists for 2026-10-27 23:04:14: the client was told to change map
// (ZC_MOVE_ZONE 3593) and then sent NOTHING - no CZ_MOVE_ZONE_OK (3686), no
// error, and its own ping stream stopped. The splitter drops a frame it cannot
// size and says so to no one, so the only way to tell "the packet never came"
// from "the splitter ate it" is to log the bytes as they arrive and the frames
// cut from them. This drives that through Serve itself, so it fails if the lines
// ever stop being produced, or if they leak into a normal run.
func TestRawLogDumpsChunkAndFrames(t *testing.T) {
	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}

	// A 13-byte CZ_GAME_READY: a packet the zone handles, so the log of the run
	// with the switch OFF still proves the chunk arrived.
	plain := make([]byte, 13)
	binary.LittleEndian.PutUint16(plain, CmdCZGameReady)

	// What the zone will decrypt: the cipher pads the plaintext up to a whole
	// 8-byte block (protocol.Cipher.EncryptChunk), so the dump ends in zeros.
	padded := make([]byte, (len(plain)+7)/8*8)
	copy(padded, plain)

	t.Run("on", func(t *testing.T) {
		out := serveOneChunk(t, table, true, plain)

		want := fmt.Sprintf("zone: RAW chunk %d bytes: %s",
			len(padded), hex.EncodeToString(padded))
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in the log:\n%s", want, out)
		}
		wantFrames := fmt.Sprintf("zone: RAW frames: [%d/%dB]", CmdCZGameReady, len(plain))
		if !strings.Contains(out, wantFrames) {
			t.Fatalf("no %q in the log - the splitter's cut is not shown:\n%s", wantFrames, out)
		}
	})

	t.Run("off", func(t *testing.T) {
		out := serveOneChunk(t, table, false, plain)

		if strings.Contains(out, "RAW") {
			t.Fatalf("a normal run dumped raw bytes:\n%s", out)
		}
		if !strings.Contains(out, "GAME_READY - world entry handshake") {
			t.Fatalf("the chunk never reached the handler:\n%s", out)
		}
	})
}

// serveOneChunk feeds ONE transport chunk to a real Serve loop and returns its
// log once the connection is over. net.Pipe, not a socket: it is in memory and
// synchronous, which is what makes the result readable without a lock - a Write
// only returns once Serve's Read has taken the bytes, and Serve has finished with
// the chunk by the time it reports the connection gone.
func serveOneChunk(t *testing.T, table *protocol.Spec, rawLog bool, plain []byte) string {
	t.Helper()

	logs := &bytes.Buffer{}
	h := &Handler{
		Table:  table,
		Cipher: protocol.DefaultCipher,
		Log:    log.New(logs, "", 0),
		RawLog: rawLog,
	}

	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		h.Serve(context.Background(), server)
		close(done)
	}()

	sendWire(t, client, plain, table)
	// The reply this packet draws is never read; closing unblocks that write and
	// ends the connection, and the lines under test are logged before it.
	client.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the client closed")
	}
	return logs.String()
}

// sendWire encrypts one plain packet and writes it as a transport chunk, the way
// the client does. EncryptChunk already prefixes the ciphertext length, so the
// result IS the whole chunk - prefixing it again is what made the first version
// of this test fail with "not a whole number of 8-byte blocks".
func sendWire(t *testing.T, conn net.Conn, plain []byte, table *protocol.Spec) {
	t.Helper()
	chunk := protocol.DefaultCipher.EncryptChunk(plain)

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(chunk); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readN reads exactly n bytes, or fails.
func readN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return buf
}
