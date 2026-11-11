package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"geserver/protocol"
	"geserver/script"
)

// END TO END THROUGH THE GAME'S OWN SCRIPTS.
//
// Everything else in create_test.go stubs the world. This file does the opposite:
// a REAL socket, the REAL command table, the REAL translated corpus
// (out\lua_corpus), the REAL fld_auch mesh, and the REAL zone command table - so
// the sentence "the Scarecrow's dummies exist and a click makes a Scarecrow" is
// measured on the same path a player takes, and not on a mock.
//
// The path under test, in the order a player meets it:
//
//	zone start        StartupTactics -> SCR_NPC_HITDUMMYMANAGER_TS_NONE
//	                  -> CreateTNPC x4 -> ZC_SPAWN_STANDINGTRIGGER (3543) x4
//	login             CZ_START_GAME, CZ_GAME_READY -> the 3543s above
//	click a dummy     CZ_CLICK_TRIGGER (3679) on a dummy's handle
//	                  -> SCR_CLICK_NPC_HITDUMMY01 -> ShowSelDlg (ZC_DIALOG_SELECT)
//	answer "create"   CZ_DIALOG_SELECT (3657) index 2
//	                  -> ShowScarecrowCreater -> CreateTmonster
//	                  -> ZC_ENTER_STANDINGMONSTER (3527, 360 B)
//
// It skips (never fails) when a piece of the real data is not on this machine: the
// corpus is built by `python scp_to_lua.py --dump`, and the mesh ships with the
// client.

// scarecrowZone wires a Handler the way main.go does, from the real files.
func scarecrowZone(t *testing.T) (*Handler, *syncBuffer) {
	t.Helper()

	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}
	mesh := filepath.Join("..", "..", "navmesh", "mesh", "fld_auch.tok")
	tool := filepath.Join("..", "..", "navmesh", "pathtool.exe")
	engine := &PathEngine{Tool: tool, Mesh: mesh, Radius: 50}
	if !engine.Available() {
		t.Skipf("no path engine (tool %s, mesh %s)", tool, mesh)
	}
	t.Cleanup(func() { engine.Close() })
	// The corpus path is per-package: script.DefaultCorpus is written for a server
	// started in goserver\, and a test in goserver\zone sits one level deeper.
	corpus := ""
	for _, cand := range []string{script.DefaultCorpus,
		filepath.Join("..", "..", "out", "lua_corpus")} {
		if _, err := os.Stat(cand); err == nil {
			corpus = cand
			break
		}
	}
	if corpus == "" {
		t.Skipf("no script corpus: build it with cd src && python scp_to_lua.py --dump ../out/lua_corpus")
	}
	npcTypes, err := LoadNPCTypes(XMLDir())
	if err != nil {
		t.Skipf("no NPC table: %v", err)
	}
	monsters, err := LoadMonsters(XMLDir())
	if err != nil {
		t.Skipf("no Monster table: %v", err)
	}

	var lines syncBuffer
	logger := log.New(&lines, "", 0)

	// The four dummies stand at (2240, -590), (3896, -749), (5936, -823) and
	// (7204, -1248). The team is put on the FIRST one so the click is in reach and
	// no chase is needed - the chase is R63's business, not this lane's.
	const clickX, clickY, clickZ = 2240, -590, 7
	near := func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		at, height, ok, nerr := engine.Near(pt, hRange, vRange)
		if nerr != nil {
			logger.Printf("mesh: %v", nerr)
			return [3]int32{}, 0, false
		}
		return at, height, ok
	}
	at, _, ok := near([3]int32{clickX, clickY, clickZ}, 1000, 300)
	if !ok {
		t.Skipf("fld_auch has no ground near the first dummy")
	}

	h := &Handler{
		Table:    table,
		Cipher:   protocol.DefaultCipher,
		Log:      logger,
		MapID:    1331,
		Spawn:    at,
		NPCTypes: npcTypes,
		Monsters: monsters,
		NavNear:  near,
		NavHeight: func(pos [3]int32) (int32, bool) {
			height, hok, _ := engine.Height(pos)
			return height, hok
		},
		TeamOf: func(context.Context, uint32) ([]Character, error) {
			return []Character{{Handle: 1792, Name: "Tester", Job: 110, Level: 130,
				HP: 5000, MaxHP: 5000, Stance: 10010}}, nil
		},
	}

	// The map's own town NPCs: 11 of them on 1331, including the Scarecrow Creator's
	// trigger (script NPC_HITDUMMYMANAGER).
	doc, npcErr := LoadNPCs(filepath.Join("..", "..", "data", "zone_npcs", "map1331.json"))
	if npcErr != nil {
		t.Skipf("no emitted NPC data for map 1331: %v", npcErr)
	}
	h.NPCs = doc.NPCs

	// The script side, wired the way clickEngine wires it: one runtime, the same
	// mutex, and the world bridge pointing back at this handler.
	host := script.New()
	host.Log = logger.Printf
	host.Spawner = zoneSpawner{h}
	// The SAME bridge behind the radius scan and `SendMsg` - main.go wires both
	// fields to one object, and a test that left this nil would answer 0 to every
	// "is a Scarecrow already here?" and never reach the remove branch.
	host.Near = zoneSpawner{h}
	t.Cleanup(host.Close)
	if _, err := host.Index(corpus); err != nil {
		t.Skipf("corpus index: %v", err)
	}
	if err := host.LoadLibraries(); err != nil {
		t.Skipf("corpus libraries: %v", err)
	}

	var mu sync.Mutex
	h.Click = func(entry string, self, pc ClickObject) (ClickReply, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, err := host.EnsureScript(entry); err != nil {
			return ClickReply{}, err
		}
		out, suspended, err := host.StartClickCoroutine(entry, scriptObj(self),
			scriptObj(pc), 10*time.Second)
		return ClickReply{
			DialogKind: out.Dialog.Kind, Text: out.Dialog.Text,
			Prompt: out.Dialog.Prompt, Options: out.Dialog.Options,
			Fade: out.Dialog.Fade, Suspended: suspended,
		}, err
	}
	h.ResumeClick = func(chosen int) (ClickReply, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		out, suspended, err := host.ResumeClick(chosen, 10*time.Second)
		return ClickReply{DialogKind: out.Dialog.Kind, Text: out.Dialog.Text,
			Options: out.Dialog.Options, Suspended: suspended}, suspended, err
	}
	h.Tactics = func(entry string, self ClickObject) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, err := host.EnsureScript(entry); err != nil {
			return false, err
		}
		return host.RunTacticsState(entry, scriptObj(self), 10*time.Second)
	}

	return h, &lines
}

// syncBuffer is the zone's log, with a lock.
//
// The Handler serves each connection in its own goroutine, so a test that reads the
// log while the zone is still writing a line is a DATA RACE on a plain
// bytes.Buffer - one that showed up as "the log does not carry OM_KILL" on a run
// where the line was there all along. One mutex, and the reader sees whole lines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLine returns the log once it carries `want`, or its whole text after
// `max`. A line is written by the SERVE goroutine, after the packet it describes,
// so a test that has already seen the packet may still be a few microseconds ahead
// of the line - waiting for the line is not a workaround for a bug, it is reading a
// log that is written asynchronously by design.
func waitForLine(t *testing.T, lines *syncBuffer, want string, max time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(max)
	for {
		text := lines.String()
		if strings.Contains(text, want) || time.Now().After(deadline) {
			return text
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// scriptObj is the test's copy of main.go's toScriptObj (that function is in
// package main and cannot be imported here).
func scriptObj(o ClickObject) script.Obj {
	return script.Obj{Handle: o.Handle, Name: o.Name, Script: o.Script,
		Level: o.Level, Commander: o.Commander, X: o.X, Y: o.Y, Z: o.Z, HP: o.HP}
}

// zoneSpawner is the test's copy of main.go's worldSpawner.
type zoneSpawner struct{ h *Handler }

func (s zoneSpawner) CreateTNPC(spec script.TNPCSpawn) (uint32, error) {
	return s.h.SpawnTNPCObject(TNPCSpec{Name: spec.Name, TypeName: spec.TypeName,
		X: spec.X, Y: spec.Y, Z: spec.Z, Direction: spec.Direction,
		Occupation: spec.Occupation, Possession: spec.Possession})
}

func (s zoneSpawner) CreateTmonster(spec script.MonsterSpawn) (uint32, error) {
	return s.h.SpawnMonsterObject(MonsterSpec{MonsterID: spec.MonsterID,
		X: spec.X, Y: spec.Y, Z: spec.Z, Count: spec.Count,
		Direction: spec.Direction, Level: spec.Level, MHP: spec.MHP,
		Tactics: spec.Tactics})
}

// NearObjects is script.NearWorld's first half: the radius scan.
func (s zoneSpawner) NearObjects(x, y int32, radius float64, kind string) ([]script.NearObject, error) {
	found, err := s.h.NearObjects(x, y, radius, kind)
	if err != nil {
		return nil, err
	}
	out := make([]script.NearObject, len(found))
	for i, o := range found {
		out[i] = script.NearObject{Handle: o.Handle, ClassID: o.ClassID, X: o.X, Y: o.Y}
	}
	return out, nil
}

// KillMonster is script.NearWorld's second half: `OM_KILL` on a monster.
func (s zoneSpawner) KillMonster(handle uint32) error {
	return s.h.KillMonsterObject(handle)
}

// TestScarecrowThroughTheRealScriptPath is this lane's end-to-end proof.
func TestScarecrowThroughTheRealScriptPath(t *testing.T) {
	h, lines := scarecrowZone(t)

	// ZONE START: the trigger's tactics entry runs once, and the four dummies exist
	// BEFORE anybody connects.
	h.StartupTactics()
	made := h.runtimeNPCs()
	if len(made) != 4 {
		t.Fatalf("%d dummy/dummies created at start, want 4 - the manager's script "+
			"makes four\n%s", len(made), lines.String())
	}
	for i, npc := range made {
		if npc.Script != "NPC_HITDUMMY01" {
			t.Errorf("dummy %d script %q, want NPC_HITDUMMY01", i, npc.Script)
		}
		if npc.Cell == int32(7) || npc.Cell == int32(17) {
			t.Errorf("dummy %d cell %d is the script's z, not the mesh's answer",
				i, npc.Cell)
		}
	}
	// The four spots the script names, in order, each resolved by the mesh.
	wantSpots := [][2]int32{{2240, -590}, {3896, -749}, {5936, -823}, {7204, -1248}}
	for i, spot := range wantSpots {
		if made[i].X != spot[0] || made[i].Y != spot[1] {
			t.Errorf("dummy %d stands at (%d, %d), want (%d, %d)",
				i, made[i].X, made[i].Y, spot[0], spot[1])
		}
	}

	// A CLIENT ENTERS, over a real socket, and is sent the dummies.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		h.Serve(context.Background(), conn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	logInAs(t, client, h.Table)

	// The four 3543s, in the stream the client really received.
	stream := readFor(t, client, 1500*time.Millisecond)
	for _, npc := range made {
		want := BuildSpawnTrigger(npc)
		if !bytes.Contains(stream, want) {
			t.Fatalf("the client never received the 3543 for handle 0x%08x\n%s",
				npc.Handle, lines.String())
		}
	}

	// THE CLICK, on the first dummy: a real 3679 with the handle at +12.
	click := make([]byte, 16)
	binary.LittleEndian.PutUint16(click, CmdCZClickTrigger)
	binary.LittleEndian.PutUint32(click[12:], made[0].Handle)
	sendWire(t, client, click, h.Table)

	// The dummy's script asks a question first (`ShowSelDlg`, three options given,
	// so two choices), and the zone answers with ZC_DIALOG_SELECT.
	dialog := readFor(t, client, 3*time.Second)
	if !bytes.Contains(dialog, protocolBE(CmdZCDialogSel)) {
		t.Fatalf("the click did not ask the dummy's question (no %d)\n%s",
			CmdZCDialogSel, lines.String())
	}

	// The player's answer: index 2 ("create one"). CZ_DIALOG_SELECT carries the
	// index at +11 (zone\click.go's own offset).
	answer := make([]byte, 16)
	binary.LittleEndian.PutUint16(answer, CmdCZDialogSelect)
	answer[11] = 2
	sendWire(t, client, answer, h.Table)

	// THE SCARECROW ITSELF: one 3527, 360 bytes.
	final := readFor(t, client, 4*time.Second)
	height, width := 360, 0
	pkt := findBytes(final, CmdZCEnterStandingMonster, height)
	if pkt == nil {
		t.Fatalf("no %d-byte %d arrived after choosing to create a Scarecrow\n%s",
			height, CmdZCEnterStandingMonster, lines.String())
	}
	width = len(pkt)
	if int(binary.LittleEndian.Uint32(pkt[emPos+8:])) == 8 {
		t.Fatalf("the Scarecrow's cell is 8 - the script's z. It must be the " +
			"mesh's answer, or it is drawn nowhere")
	}

	monsters := h.runtimeMonsters()
	if len(monsters) != 1 {
		t.Fatalf("%d monster(s) created, want 1\n%s", len(monsters), lines.String())
	}
	m := monsters[0]
	if m.ClassID != 11600 {
		t.Errorf("class %d, want 11600 (Monster_Hersu01, the first Scarecrow)", m.ClassID)
	}
	if m.HP != 5000*100-100 {
		t.Errorf("hp %d, want %d - the player's own HP x 100 - 100", m.HP, 5000*100-100)
	}

	// THE LOG LINES, as a reader would find them: one per created NPC, one for the
	// monster and the packet it went out as.
	text := lines.String()
	if got := strings.Count(text, "zone: CREATE_TNPC"); got != 4 {
		t.Errorf("%d CREATE_TNPC line(s), want 4", got)
	}
	if !strings.Contains(text, "zone: CREATE_TMONSTER") {
		t.Errorf("no CREATE_TMONSTER line\n%s", text)
	}
	for _, want := range []string{
		"CREATE_TMONSTER \"Monster_Hersu01\"",
		"class 11600, Lv 130, EXP 0",
		"cmd 3527 (360 B)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log does not carry %q", want)
		}
	}
	_ = width
}

// readFor collects everything the server sends for a while.
func readFor(t *testing.T, conn net.Conn, d time.Duration) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 8192)
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return out
		}
		_ = conn.SetReadDeadline(time.Now().Add(left))
		n, err := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return out
			}
			if err == io.EOF {
				return out
			}
			return out
		}
	}
}

// findBytes locates a fixed-size packet in a server->client stream by its command
// id, using the length the client's own table allocates.
func findBytes(stream []byte, cmd uint16, size int) []byte {
	pattern := protocolBE(cmd)
	for at := 0; at+size <= len(stream); at++ {
		if bytes.Equal(stream[at:at+2], pattern) {
			return stream[at : at+size]
		}
	}
	return nil
}

// protocolBE is a command id as the two bytes it is on the wire.
func protocolBE(cmd uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, cmd)
	return b
}
