package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TWICE ON THE SAME SCARECROW CREATOR, THROUGH THE GAME'S OWN SCRIPT.
//
// This is the owner's report turned into a test: he clicked the Creator, got a
// Scarecrow, and then every later click offered CREATE again instead of "Do you want
// to remove the created Scarecrow?". The cause was three builtins that did not exist
// (`GetNearObjectCount`, `GetNearObject`, `SendMsg`), so the script's own
// `if (obj.ClassID == HitDummyID[t])` never ran and it fell through to the create
// question every time.
//
// Nothing here re-implements the script: `SCR_CLICK_NPC_HITDUMMY01` is the corpus
// file, loaded by name, the same way a player's click loads it. What the test does is
// press the buttons a player presses, over a real socket:
//
//	click 1 with nothing near   -> the CREATE question   "<$>10096663</>"
//	answer 2                    -> 3527 (the Scarecrow), one monster in the registry
//	click 2 with it near        -> the REMOVE question   "<$>10096661</>"
//	answer 2                    -> NO packet: the body becomes a ZOMBIE and stays in
//	                               the roster; the corpse's own 3531 (reason 0)
//	                               leaves it 600 ms later (om_kill_test.go)
//	click 3 with nothing near   -> the CREATE question again
//
// The three locale tokens are the script's own, quoted from
// `out\lua_corpus\npc\evenpc\auch\npc_hitdummy_creater.lua` (:36 and :55): the zone
// sends them verbatim and the client resolves them from its dictionary.

const (
	createPrompt = "<$>10096663</>" // :55 - "create a Scarecrow?"
	removePrompt = "<$>10096661</>" // :36 - "do you want to remove the created Scarecrow?"
	quitOption   = "<$>10096434</>" // both dialogs' first option
)

// TestScarecrowRemoveThroughTheRealScriptPath is slices 1 and 2 end to end.
func TestScarecrowRemoveThroughTheRealScriptPath(t *testing.T) {
	h, lines := scarecrowZone(t)

	// ZONE START: the trigger's tactics entry creates the four standing dummies.
	h.StartupTactics()
	made := h.runtimeNPCs()
	if len(made) != 4 {
		t.Fatalf("%d dummy/dummies at start, want 4\n%s", len(made), lines.String())
	}

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
	readFor(t, client, 800*time.Millisecond) // the world, the 3543s, the team

	creator := made[0].Handle

	// -- CLICK 1: nothing is near, so the script offers CREATE ----------------
	clickDummy(t, client, h, creator)
	if got := readUntil(t, client, []byte(createPrompt), 5*time.Second); !bytes.Contains(got, []byte(createPrompt)) {
		t.Fatalf("the first click did not offer to create one (no %q)\n%s",
			createPrompt, lines.String())
	}
	answerDialog(t, client, h, 2)

	afterCreate := readFor(t, client, 1200*time.Millisecond)
	if findBytes(afterCreate, CmdZCEnterStandingMonster, EnterStandingMonsterSize) == nil {
		t.Fatalf("choosing to create did not send the Scarecrow (no %d)\n%s",
			CmdZCEnterStandingMonster, lines.String())
	}
	monsters := h.runtimeMonsters()
	if len(monsters) != 1 {
		t.Fatalf("%d monster(s) after creating one, want 1\n%s", len(monsters), lines.String())
	}
	dummy := monsters[0]
	if dummy.ClassID != 11600 {
		t.Fatalf("the created Scarecrow is class %d, want 11600 (HitDummyID1[1][1])",
			dummy.ClassID)
	}
	// THE SCAN SEES THE OBJECT WE ARE REALLY RUNNING: the monster the script just
	// made, at its OWN position and with its own class - not a synthetic entry and
	// not the NPC's numbers.
	near, err := h.NearObjects(made[0].X, made[0].Y, 800, NearKindMonster)
	if err != nil {
		t.Fatalf("the scan around the Creator: %v", err)
	}
	if len(near) != 1 {
		t.Fatalf("%d monster(s) within 800 of the Creator, want the one just created: %+v",
			len(near), near)
	}
	if near[0].Handle != dummy.Handle || near[0].ClassID != 11600 {
		t.Errorf("the scan found handle 0x%08x class %d, want the Scarecrow 0x%08x class 11600",
			near[0].Handle, near[0].ClassID, dummy.Handle)
	}
	if near[0].X != 2623 || near[0].Y != -665 {
		t.Errorf("the scan reports it at (%d, %d), want its own (2623, -665) - the "+
			"script's CreateTmonster spot on this map", near[0].X, near[0].Y)
	}

	// -- CLICK 2: one Scarecrow stands near, so the script offers REMOVE ------
	clickDummy(t, client, h, creator)
	removeBox := readUntil(t, client, []byte(removePrompt), 5*time.Second)
	if !bytes.Contains(removeBox, []byte(removePrompt)) {
		t.Fatalf("the second click did not offer to remove the Scarecrow (no %q) - "+
			"this is the owner's bug: the scan found nothing\n%s",
			removePrompt, lines.String())
	}
	if bytes.Contains(removeBox, []byte(createPrompt)) {
		t.Fatalf("the second click offered CREATE with a Scarecrow standing there\n%s",
			lines.String())
	}
	// The remove dialog's own first option is Quit, which must NOT remove anything.
	if !bytes.Contains(removeBox, []byte(quitOption)) {
		t.Errorf("the remove dialog has no Quit option (%q)\n%s", quitOption, lines.String())
	}

	// Option 2 is "Remove the Scarecrow" (`if (select1 == 1 ...) then return`, so the
	// second option is the one that kills). It is a SCRIPT OM_KILL now, so the first
	// thing to prove is what does NOT happen: no packet in the first quarter second,
	// and the body still in the roster with its HP ledger at 0.
	answerDialog(t, client, h, 2)
	quiet := readFor(t, client, 250*time.Millisecond)
	if leave := findBytes(quiet, CmdZCLeaveMonster, LeaveMonsterSize); leave != nil {
		t.Fatalf("the OM_KILL sent a 3531 at once - the reference's OnKill sends nothing "+
			"and the body must become a zombie:\n%s", lines.String())
	}
	if left := h.runtimeMonsters(); len(left) != 1 || left[0].Handle != dummy.Handle {
		t.Fatalf("the roster after the kill is %+v, want the one Scarecrow 0x%08x still "+
			"there (a zombie, not a removal)", left, dummy.Handle)
	}
	if hp, ok := h.monsterLiveHP(dummy.Handle); !ok || hp != 0 {
		t.Fatalf("the ledger is %v (ok=%v) after the kill, want 0 - the body is dead "+
			"but still on the field", hp, ok)
	}

	// THE LEAVE THAT FOLLOWS IS THE CORPSE'S: after the shape's DyingTime (floor
	// 600 ms), reason 0, the Scarecrow's own handle. It is the same packet the death
	// path sends, from the same handshake in this package (respawn.go), not a second
	// leave bolted onto the kill.
	afterRemove := readUntil(t, client, BuildLeaveMonster(dummy.Handle, LeaveCodeLogout),
		5*time.Second)

	// 1. THE PACKET: an 8-byte 3531 carrying the Scarecrow's own handle.
	leave := findBytes(afterRemove, CmdZCLeaveMonster, LeaveMonsterSize)
	if leave == nil {
		t.Fatalf("the corpse never sent its %d-byte %d\n%s", LeaveMonsterSize,
			CmdZCLeaveMonster, lines.String())
	}
	if got := binary.LittleEndian.Uint32(leave[lmHandle:]); got != dummy.Handle {
		t.Errorf("the leave packet names handle 0x%08x, want the Scarecrow's 0x%08x",
			got, dummy.Handle)
	}
	if got := binary.LittleEndian.Uint16(leave[lmCode:]); got != LeaveCodeLogout {
		t.Errorf("the leave code is %d, want %d (a kill drops the object; 3 would fade it)",
			got, LeaveCodeLogout)
	}

	// 2. THE REGISTRY: it is gone, and the leave is the only thing that took it off.
	if left := h.runtimeMonsters(); len(left) != 0 {
		t.Fatalf("%d monster(s) still registered after the corpse left: %+v\n%s",
			len(left), left, lines.String())
	}

	// 3. THE LINE: one, and it says the body is a zombie whose corpse will leave.
	text := waitForLine(t, lines, "OM_KILL", 3*time.Second)
	for _, want := range []string{
		// The Scarecrow's OWN handle, not a literal: the allocator seeds each run's
		// numbers (monster.go), so 0x20000002 is nobody's number any more.
		fmt.Sprintf("OM_KILL 0x%08x", dummy.Handle),
		"class 11600",
		"is now a zombie",
		"no leave now",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log does not carry %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "OM_KILL"); got != 1 {
		t.Errorf("%d OM_KILL line(s), want exactly one:\n%s", got, text)
	}

	// -- CLICK 3: nothing is near again, so CREATE is offered once more -------
	clickDummy(t, client, h, creator)
	if got := readUntil(t, client, []byte(createPrompt), 5*time.Second); !bytes.Contains(got, []byte(createPrompt)) {
		t.Fatalf("after removing it the next click did not offer to create one "+
			"(no %q)\n%s", createPrompt, lines.String())
	}

	// THE PROBE'S OWN EVIDENCE, printed with `go test -v`: the two dialogs, the
	// create and the remove. A reader can see the whole round trip without a client.
	for _, line := range strings.Split(lines.String(), "\n") {
		for _, want := range []string{"DIALOG_SELECT", "CREATE_TMONSTER", "OM_KILL"} {
			if strings.Contains(line, want) {
				t.Log(line)
				break
			}
		}
	}
}

// readUntil collects what the server sends until `want` appears in it, or `max`
// passes. It is the same reader as readFor with an early exit, so a test that is
// waiting for one packet does not pay a whole timeout for it.
func readUntil(t *testing.T, conn net.Conn, want []byte, max time.Duration) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 8192)
	deadline := time.Now().Add(max)
	for !bytes.Contains(out, want) {
		left := time.Until(deadline)
		if left <= 0 {
			return out
		}
		// Read in short slices so a packet that has already arrived is not waited
		// out: 100 ms is far longer than a loopback round trip here.
		slice := 100 * time.Millisecond
		if left < slice {
			slice = left
		}
		_ = conn.SetReadDeadline(time.Now().Add(slice))
		n, err := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			continue
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return out
		}
	}
	return out
}

// clickDummy sends the real CZ_CLICK_TRIGGER (3679) a player's click makes: 16
// bytes with the handle at +12.
func clickDummy(t *testing.T, client net.Conn, h *Handler, handle uint32) {
	t.Helper()
	click := make([]byte, 16)
	binary.LittleEndian.PutUint16(click, CmdCZClickTrigger)
	binary.LittleEndian.PutUint32(click[12:], handle)
	sendWire(t, client, click, h.Table)
}

// answerDialog sends the real CZ_DIALOG_SELECT (3657) with a 1-based option index,
// which the client puts at +11.
func answerDialog(t *testing.T, client net.Conn, h *Handler, index byte) {
	t.Helper()
	answer := make([]byte, 16)
	binary.LittleEndian.PutUint16(answer, CmdCZDialogSelect)
	answer[11] = index
	sendWire(t, client, answer, h.Table)
}
