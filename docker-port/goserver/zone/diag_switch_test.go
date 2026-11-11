package zone

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE DIAGNOSTIC SWITCHES ARE PINS, NOT FEATURES.
//
// Handler.DiagStopOffset and Handler.DiagHex exist to answer ONE question each - is the
// stop's handle found, and what EXACTLY do we put on the wire for a standing character.
// They are worthless as probes unless they are SURE to change only what they claim, so
// every test here is about the boundary: what they move, what they leave alone, and
// that OFF is byte-identical to the plain builders.
//
// The default tests are the reason this file exists at all: a probe that quietly
// altered a normal packet would corrupt the very comparison it was built for, and
// nobody would see it - both switches are off in every run that is not a probe.

// diagLog is the logger these tests hand a Handler: a diagnostic still logs, and the
// tests do not want it in the test output.
func diagLog() *log.Logger { return log.New(io.Discard, "", 0) }

// TestDiagStopOffsetShiftsOnlyTheXField pins the shift twice: directly on one packet
// (every other byte identical, the input not mutated) and end to end through a real
// walk, where the stop the observer receives must carry the SHIFTED x at the TRUE y
// and cell.
//
// WHY THE CELL IS THE PART THAT MATTERS. It is a navmesh triangle index, not a
// height: a shifted cell puts the client on an invalid triangle and crashes it in
// PathEngine.DLL, and the run would then say "the packet is broken" instead of
// answering whether the handle was found.
func TestDiagStopOffsetShiftsOnlyTheXField(t *testing.T) {
	const handle = uint32(0xAABBCCDD)
	base := BuildStopPCAt(handle, [3]int32{1000, 2000, 3}, 424242)
	before := append([]byte(nil), base...)

	h := &Handler{Log: diagLog(), DiagStopOffset: 700}
	got := h.stopPCForOthers(handle, base)

	if len(got) != 22 {
		t.Fatalf("the shifted stop is %d bytes, want 22", len(got))
	}
	if c := binary.LittleEndian.Uint16(got[0:]); c != CmdZCStopPC {
		t.Fatalf("cmd = %d, want %d", c, CmdZCStopPC)
	}
	if g := binary.LittleEndian.Uint32(got[2:]); g != handle {
		t.Fatalf("handle = %#x, want %#x", g, handle)
	}
	if g := binary.LittleEndian.Uint32(got[6:]); g != 424242 {
		t.Fatalf("tick = %d, want the caller's 424242 - the shift must not restamp it", g)
	}
	if g := int32(binary.LittleEndian.Uint32(got[10:])); g != 1700 {
		t.Fatalf("x = %d, want 1700 (1000 shifted 700 east)", g)
	}
	if g := int32(binary.LittleEndian.Uint32(got[14:])); g != 2000 {
		t.Fatalf("y = %d, want the true 2000 - only x moves", g)
	}
	if g := int32(binary.LittleEndian.Uint32(got[18:])); g != 3 {
		t.Fatalf("cell = %d, want the true 3 - a shifted cell is an invalid triangle "+
			"and crashes the client in PathEngine.DLL", g)
	}
	// THE COPY PROOF: the warp path hands the same slice to its own reply, so an
	// in-place edit would move the mover's own team too.
	if !bytes.Equal(base, before) {
		t.Fatalf("the input packet was EDITED in place: % x -> % x", before, base)
	}
	for i := range got {
		if i >= 10 && i <= 13 {
			continue
		}
		if got[i] != before[i] {
			t.Fatalf("byte %d changed (%#x -> %#x) - the switch may only move x at +10",
				i, before[i], got[i])
		}
	}

	// END TO END: the same shift on a real walk, and only on the observer's copy.
	hp := &Handler{Log: diagLog(), DiagStopOffset: 700}
	a := newPlayer(0x40000031, "A", [3]int32{1000, 1000, 1})
	b := newPlayer(0x40000041, "B", [3]int32{2000, 2000, 1})
	hp.joinZone(a.sess)
	hp.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil

	dest := [3]int32{1000, 1300, 1} // 300 units: ~640 ms at 469 u/s
	if _, err := hp.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	b.conn.writes = nil
	time.Sleep(walkOverWait)
	hp.chaseTick(a.sess)

	sent := stopsOf(b.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the finished walk sent %d stop(s) to the observer, want exactly 1", len(sent))
	}
	p := sent[0]
	if g := binary.LittleEndian.Uint32(p[2:]); g != a.sess.Team[0].Handle {
		t.Fatalf("the stop names handle %#x, want %#x", g, a.sess.Team[0].Handle)
	}
	if g := int32(binary.LittleEndian.Uint32(p[10:])); g != dest[0]+700 {
		t.Fatalf("the stop's x = %d, want %d (the stopped x shifted 700 east)", g, dest[0]+700)
	}
	if g := int32(binary.LittleEndian.Uint32(p[14:])); g != dest[1] {
		t.Fatalf("the stop's y = %d, want the true %d", g, dest[1])
	}
	if g := int32(binary.LittleEndian.Uint32(p[18:])); g != dest[2] {
		t.Fatalf("the stop's cell = %d, want the true %d", g, dest[2])
	}
	// The roster keeps the TRUE position: the shift is wire-only.
	var stored [3]int32
	found := false
	for _, m := range hp.Players.Snapshot() {
		if m.Sess == a.sess && len(m.Pos) > 0 {
			stored, found = m.Pos[0], true
		}
	}
	if !found {
		t.Fatal("the walker is not in the roster")
	}
	if stored != dest {
		t.Fatalf("the stored position is %v, want the true %v - the shift must never "+
			"reach the roster", stored, dest)
	}
}

// diagDumpedPackets keeps only the packets a DIAGHEX dump covers - the 3520 spawn
// and the 3554 stance - in the order they were built.
//
// The announce is allowed to grow other per-character packets (it already carries a
// ZC_SKILL_DISABLE 3572, ported from the live capture), and that must not make a pin
// about the spawn and the stance fail: the dump, the golden and the "off is
// byte-identical" proof are all about THESE two packets.
func diagDumpedPackets(pkts [][]byte) [][]byte {
	out := make([][]byte, 0, len(pkts))
	for _, p := range pkts {
		if len(p) < 2 {
			continue
		}
		switch cmdOf(p) {
		case CmdZCSpawnStandingPC, CmdZCChangeStance:
			out = append(out, p)
		}
	}
	return out
}

// TestDiagDefaultsMatchThePlainBuilders is the "off is not merely equivalent" pin:
// with both switches at their zero value the announce must produce the SAME BYTES as
// the plain builders called by hand, and stopPCForOthers must return the very same
// slice - not a copy, not a re-serialisation.
//
// A diagnostic that changes a normal packet would poison every comparison it was
// built to make, and the change would be invisible: both switches are off in every
// run that is not a probe.
func TestDiagDefaultsMatchThePlainBuilders(t *testing.T) {
	h := &Handler{Log: diagLog()} // DiagStopOffset 0 and DiagHex false: off
	pos := [][3]int32{{1000, 1000, 1}, {1200, 1000, 1}}
	a := newTeam([]uint32{0x40000031, 0x40000032}, pos)

	// ONLY the two packets a DIAGHEX dump is about. The announce also carries a
	// ZC_SKILL_DISABLE (3572) per character - ported from the same live capture by
	// another lane - and that packet is not this switch's business; pinning its
	// bytes here would make this file fail every time that lane moves.
	got := diagDumpedPackets(h.announceChars(a.sess, a.sess.Team, pos))
	var want [][]byte
	for i, c := range a.sess.Team {
		want = append(want, BuildSpawnStandingPC(c.Handle, c, pos[i], DefaultPosture))
		if c.Stance != 0 {
			want = append(want, BuildChangeStance(c.Handle, c.Stance))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the announce built %d packet(s), want the plain builders' %d",
			len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d differs from the plain builder at byte %d: %#x vs %#x "+
				"(%d vs %d bytes)", i, firstDiff(got[i], want[i]),
				got[i], want[i], len(got[i]), len(want[i]))
		}
	}

	pkt := BuildStopPCAt(0x40000031, pos[0], 424242)
	out := h.stopPCForOthers(0x40000031, pkt)
	if len(out) != len(pkt) || &out[0] != &pkt[0] {
		t.Fatalf("with the switch off stopPCForOthers returned a DIFFERENT slice "+
			"(%d bytes vs %d) - it must hand the caller's own packet back untouched",
			len(out), len(pkt))
	}
	if !bytes.Equal(out, pkt) {
		t.Fatalf("with the switch off the packet changed: % x -> % x", pkt, out)
	}
}

// firstDiff is the index of the first byte that differs, or -1 with equal lengths.
// Only used to make a builder mismatch readable.
func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return len(a)
		}
		return len(b)
	}
	return -1
}

// diagGolden is the file both flags off must reproduce, byte for byte.
const diagGolden = "testdata/diag_default.golden"

// TestDiagDefaultGolden is the WHOLE-STREAM pin: one fixed scenario (a 2-character
// team announcing, then one stop) written as one hex line per packet, compared
// against a committed file.
//
// WHY A FILE AND NOT ASSERTIONS. Both switches off must be byte-identical to the
// build BEFORE they existed - that is the promise the owner relies on when a run
// without flags is compared to an earlier one. Field-by-field assertions can all
// pass while some other byte drifts; 1452 bytes compared as a whole cannot.
//
// Regenerate ONLY after an intended packet change, and read the diff:
//
//	DIAG_GOLDEN_UPDATE=1 go test ./zone/ -run TestDiagDefaultGolden
func TestDiagDefaultGolden(t *testing.T) {
	h := &Handler{Log: diagLog()} // both switches at their zero value: OFF
	const handle = uint32(0x40000031)
	pos := [][3]int32{{1000, 1000, 1}, {1200, 1000, 1}}
	a := newTeam([]uint32{handle, 0x40000032}, pos)

	// The golden pins the 3520, the 3554 and the 3549 - the three packets this lane
	// touches. Other per-character packets in the announce (ZC_SKILL_DISABLE 3572)
	// are somebody else's pin, so filtering keeps this one about its own subject.
	pkts := diagDumpedPackets(h.announceChars(a.sess, a.sess.Team, pos))
	pkts = append(pkts, h.stopPCForOthers(handle, BuildStopPCAt(handle, pos[0], 424242)))

	sizes := []int{705, 10, 705, 10, 22} // spawn, stance, spawn, stance, stop
	if len(pkts) != len(sizes) {
		t.Fatalf("the scenario built %d packet(s), want %d", len(pkts), len(sizes))
	}
	var sb strings.Builder
	for i, p := range pkts {
		if len(p) != sizes[i] {
			t.Fatalf("packet %d is %d bytes, want %d - the golden's line sizes are "+
				"fixed and the file below would be regenerated around a bug",
				i, len(p), sizes[i])
		}
		sb.WriteString(hex.EncodeToString(p))
		sb.WriteString("\n")
	}

	if os.Getenv("DIAG_GOLDEN_UPDATE") == "1" {
		if err := os.WriteFile(diagGolden, []byte(sb.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", diagGolden, err)
		}
		t.Logf("regenerated %s: %d line(s), %d bytes", diagGolden,
			len(sizes), sb.Len())
		return
	}

	want, err := os.ReadFile(diagGolden)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with DIAG_GOLDEN_UPDATE=1)", diagGolden, err)
	}
	if !bytes.Equal([]byte(sb.String()), want) {
		got := strings.Split(sb.String(), "\n")
		old := strings.Split(string(want), "\n")
		for i := 0; i < len(got) && i < len(old); i++ {
			if got[i] != old[i] {
				t.Fatalf("packet %d differs from %s: %d bytes on the wire, %d in the "+
					"file (%s)", i, diagGolden, len(got[i])/2, len(old[i])/2,
					describeHexDiff(old[i], got[i]))
			}
		}
		t.Fatalf("%s has %d line(s), the scenario built %d - with both switches off the "+
			"stream must be byte-identical to the committed one", diagGolden, len(old), len(got))
	}
}

// describeHexDiff names the first byte of two hex lines that differs, so a golden
// mismatch points at a field instead of at a wall of hex.
func describeHexDiff(want, got string) string {
	w, g := []byte(want), []byte(got)
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return "first difference at hex character " + strconv.Itoa(i) +
				", byte " + strconv.Itoa(i/2) + " (the file has " + string(w[i]) +
				", the wire has " + string(g[i]) + ")"
		}
	}
	return "one line is a prefix of the other"
}

// TestDiagHandleMatchesTheMoveEchoForEverySlot answers the other half of "the far
// client ignores what we send": the handle a character is ANNOUNCED with, the handle
// its stance names, and the handle its own move echo carries must be one value for
// every slot of the team - follower slots included.
//
// It is an ASSERTION, not a fix: the three already agree. Its value is that it fails
// the moment our own code takes a handle from two different places (the roster copy,
// a "+4 vs +2" mix-up, a truncated 16-bit read), which is exactly the class of bug
// that makes the far client look like it is ignoring a packet.
func TestDiagHandleMatchesTheMoveEchoForEverySlot(t *testing.T) {
	h := &Handler{Log: diagLog()} // the switch's HOME state: off, so the echo is the plain one
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newPlayer(0x40000041, "Observer", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	out := h.joinZone(b.sess)
	b.conn.writes = append(b.conn.writes, out...)

	var spawnHandles, stanceHandles []uint32
	for _, p := range b.conn.sent() {
		switch cmdOf(p) {
		case CmdZCSpawnStandingPC:
			spawnHandles = append(spawnHandles, binary.LittleEndian.Uint32(p[4:]))
		case CmdZCChangeStance:
			stanceHandles = append(stanceHandles, binary.LittleEndian.Uint32(p[2:]))
		}
	}
	if len(spawnHandles) != len(a.sess.Team) {
		t.Fatalf("%d spawn(s) for a team of %d", len(spawnHandles), len(a.sess.Team))
	}
	if len(stanceHandles) != len(a.sess.Team) {
		t.Fatalf("%d stance packet(s) for a team of %d", len(stanceHandles), len(a.sess.Team))
	}

	dest := [3]int32{1400, 1500, 1}
	for i := range spawnHandles {
		got, err := h.movePC(a.sess, moveTo(byte(i), dest))
		if err != nil {
			t.Fatalf("movePC slot %d: %v", i, err)
		}
		var echo []byte
		for _, p := range got {
			if len(p) >= 2 && cmdOf(p) == CmdZCMovePC {
				echo = p
			}
		}
		if echo == nil {
			t.Fatalf("slot %d produced no ZC_MOVE_PC echo", i)
		}
		moved := binary.LittleEndian.Uint32(echo[2:])
		if moved != spawnHandles[i] || moved != stanceHandles[i] {
			t.Fatalf("slot %d: the move echo names handle %#x, its spawn named %#x and "+
				"its stance named %#x - a mismatch here means our own code takes a "+
				"handle from two different places", i, moved, spawnHandles[i], stanceHandles[i])
		}
	}
}

// diagHexLine is one parsed DIAGHEX line: the fields the two DIAGHEX tests read. The
// hex field is kept as text so it can be compared with encoding/hex directly, which is
// the point - the dump must be the OBSERVER'S bytes, not a re-build of them.
type diagHexLine struct {
	kind   string // "spawn" or "stance"
	handle uint32
	slot   int
	team   int
	stance uint32
	none   bool
	hex    string
}

// parseDiagHex reads the DIAGHEX lines out of a log, in order, and refuses a line it
// cannot make sense of: a dump whose fields drifted is not a dump.
func parseDiagHex(t *testing.T, out string) []diagHexLine {
	t.Helper()
	var lines []diagHexLine
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "DIAGHEX ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			t.Fatalf("a DIAGHEX line has only %d field(s): %q", len(f), line)
		}
		l := diagHexLine{kind: f[1]}
		for _, kv := range f[2:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue // part of the truncation note, not a field of its own
			}
			switch k {
			case "handle":
				n, err := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
				if err != nil {
					t.Fatalf("DIAGHEX handle %q: %v", v, err)
				}
				l.handle = uint32(n)
			case "slot":
				l.slot, _ = strconv.Atoi(v)
			case "team":
				l.team, _ = strconv.Atoi(v)
			case "stance":
				n, _ := strconv.ParseUint(v, 10, 32)
				l.stance = uint32(n)
			case "none":
				l.none = v == "1"
			case "hex":
				l.hex = v
			}
		}
		lines = append(lines, l)
	}
	return lines
}

// diagHexScenario runs ONE fixed standing scenario for both DIAGHEX tests: B enters
// first, so A's arrival is the announcement B actually receives through its socket.
//
// Nothing walks. That is the whole point of this probe: the symptom the owner reports
// happens with both followers STANDING STILL, never having taken a step, so the bytes
// of the spawn are the only evidence there is.
func diagHexScenario(t *testing.T, buf *bytes.Buffer, diagHex bool) [][]byte {
	t.Helper()
	h := &Handler{Log: log.New(buf, "", 0), DiagHex: diagHex}
	pos := [][3]int32{{1000, 1000, 1}, {1200, 1000, 1}}
	a := newTeam([]uint32{0x40000031, 0x40000032}, pos)
	b := newPlayer(0x40000041, "Observer", [3]int32{2000, 2000, 1})
	h.joinZone(b.sess)  // B's own entry: its characters are announced to nobody yet
	b.conn.writes = nil // and its socket starts empty for the measurement
	buf.Reset()         // only A's arrival is under test
	h.joinZone(a.sess)  // this is what writes A's spawns and stances to B
	return b.conn.sent()
}

// TestDiagHexDumpsExactlyWhatTheObserverGot pins the whole promise of -diag-hex: one
// line per packet per announced character, each line starting with the DIAGHEX marker,
// and each line's hex field decoding to EXACTLY the bytes that character's observer
// received.
//
// A dump that agreed with the builder but not with the socket would be useless - it
// would look right while the client got something else, which is the exact failure
// mode the owner is chasing.
//
// IT DOES NOT PIN HOW MANY PACKETS AN ANNOUNCE HAS. The announce grows: a
// ZC_SKILL_DISABLE (3572) now follows the spawn, from the same day's live capture, and
// that is another lane's decision. What is pinned here is the RULE - every packet the
// observer received appears in the log exactly once, and every dumped packet names the
// slot it belongs to.
func TestDiagHexDumpsExactlyWhatTheObserverGot(t *testing.T) {
	var buf bytes.Buffer
	got := diagHexScenario(t, &buf, true)
	lines := parseDiagHex(t, buf.String())

	if n := strings.Count(buf.String(), "DIAGHEX"); n != len(lines) {
		t.Fatalf("%d occurrence(s) of DIAGHEX but only %d line(s) START with the marker - "+
			"a dump line must begin with it so it greps", n, len(lines))
	}

	// Every packet the observer received is dumped exactly once, under the kind its
	// own command id names.
	for i, p := range got {
		k := diagKindOf(p)
		if k == "" {
			t.Fatalf("the observer received cmd %d, which no DIAGHEX line names - a dump "+
				"that skips a packet cannot be diffed", cmdOf(p))
		}
		want := hex.EncodeToString(p)
		n := 0
		for _, l := range lines {
			if l.kind == k && l.hex == want {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("packet %d (%d bytes, cmd %d) is dumped %d time(s) as a %s line, want "+
				"exactly once. DIAGHEX must be the bytes the client got, byte for byte",
				i, len(p), cmdOf(p), n, k)
		}
	}

	// The other direction: a line that carries bytes must carry bytes whose own command
	// id matches the kind it claims, or the dump is lying about what it shows.
	for _, l := range lines {
		if l.hex == "" {
			continue
		}
		raw, err := hex.DecodeString(l.hex)
		if err != nil {
			t.Fatalf("a DIAGHEX %s line's hex does not decode: %v", l.kind, err)
		}
		if k := diagKindOf(raw); k != l.kind {
			t.Fatalf("a DIAGHEX %s line carries bytes whose command id is %d (%s) - the "+
				"dump names the wrong packet", l.kind, cmdOf(raw), k)
		}
	}

	// The slot is what makes a leader and a follower comparable, so a multi-character
	// team must say it on every one of its lines.
	for _, kv := range []struct {
		kind   string
		handle uint32
		team   int
		slot   int
	}{
		{"spawn", 0x40000031, 2, 0}, // A's leader
		{"spawn", 0x40000032, 2, 1}, // A's follower
	} {
		found := false
		for _, l := range lines {
			if l.kind != kv.kind || l.handle != kv.handle {
				continue
			}
			found = true
			if l.slot != kv.slot || l.team != kv.team {
				t.Fatalf("%s of handle %#x says slot=%d team=%d, want slot=%d team=%d",
					l.kind, l.handle, l.slot, l.team, kv.slot, kv.team)
			}
		}
		if !found {
			t.Fatalf("no %s line for handle %#x: the character was not dumped at all",
				kv.kind, kv.handle)
		}
	}
}

// diagKindOf is the DIAGHEX kind for a packet, or "" for a packet the dump does not
// cover. It is the one place that maps a command id to a dump line, so the dump and
// the test cannot disagree about what a line means.
func diagKindOf(p []byte) string {
	if len(p) < 2 {
		return ""
	}
	switch cmdOf(p) {
	case CmdZCSpawnStandingPC:
		return "spawn"
	case CmdZCChangeStance:
		return "stance"
	case CmdZCSkillDisable:
		return "skilldisable"
	}
	return ""
}

// TestDiagHexOffIsSilent is the other half: with the switch off the log says nothing
// about packets, and the WIRE is byte-identical to the run with the switch on.
//
// The second half is the promise a diagnostic has to keep - it changes what is
// written DOWN, never what is sent - and it is checked by running the same scenario
// twice rather than by trusting the code.
func TestDiagHexOffIsSilent(t *testing.T) {
	var on, off bytes.Buffer
	want := diagHexScenario(t, &on, true)
	got := diagHexScenario(t, &off, false)

	if strings.Contains(off.String(), "DIAGHEX") {
		t.Fatalf("with -diag-hex off the log still carries a DIAGHEX line:\n%s", off.String())
	}
	for _, line := range strings.Split(off.String(), "\n") {
		if strings.Contains(line, "DIAG ") {
			t.Fatalf("with -diag-hex off a line still says DIAG: %q", line)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the observer got %d packet(s) with the switch off and %d with it on",
			len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d differs between -diag-hex off and on: % x vs % x - the "+
				"switch may change the LOG, never the wire", i, got[i], want[i])
		}
	}
}
