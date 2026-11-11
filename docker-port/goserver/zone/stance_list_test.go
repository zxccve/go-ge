package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"geserver/protocol"
)

// THE LIVECAPTURE TEST FOR ZC_STANCELIST (3626).
//
// The fixture is the RAW 136 bytes of one ZC_STANCELIST from the LIVE server, cut out
// of captures\skillcast20261031\20261031-153900\s2c_7061_55960.bin at stream offset
// 69172 - the smallest of the three in that capture, and the one whose 3621 (the
// 876-byte live_skill_list.bin) follows it 136 bytes later. It is cmd 3626, len 136,
// handle 1560907, count 9, and its character is the Scout (job 3).
//
// A fixture and not a live read, for the same reason as live_skill_list.bin: the
// capture lives outside this repository, and a test that cannot run without it is a
// test that stops being run.
const liveStanceList = "testdata/live_stance_list.bin"

// The three frames the capture measured, (STANCE_INFO count -> whole packet). Each one
// closes exactly on its own length word: 10 + n*14. A builder with the wrong header or
// the wrong stride closes at none of them.
var liveStanceListSizes = []struct {
	count int
	size  int
}{
	{9, 136},  // Scout,   job 3, offset 69172
	{26, 374}, // Fighter, job 1, offset 70306
	{13, 192}, // Warlock, job 5, offset 72780
}

// stanceListHandle is OUR handle for the character, deliberately NOT the fixture's: the
// handle is the one field the two sides legitimately disagree on, and the test below
// names it as the only allowance.
const stanceListHandle = uint32(0x40000031)

// stanceListLiveBytes reads the fixture and pins that it IS the packet it claims to be
// before anything compares against it: a wrong file would make every assertion below
// pass or fail for the wrong reason.
func stanceListLiveBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(liveStanceList)
	if err != nil {
		t.Fatalf("read %s: %v", liveStanceList, err)
	}
	if len(b) != 136 {
		t.Fatalf("%s is %d bytes, want the measured 136", liveStanceList, len(b))
	}
	if cmd := cmdOf(b); cmd != CmdZCStanceList {
		t.Fatalf("%s is cmd %d, want %d", liveStanceList, cmd, CmdZCStanceList)
	}
	if n := binary.LittleEndian.Uint16(b[2:]); int(n) != len(b) {
		t.Fatalf("%s's length word says %d, the file is %d bytes - the fixture is not "+
			"the packet it claims to be", liveStanceList, n, len(b))
	}
	if count := int(binary.LittleEndian.Uint16(b[8:])); stanceListHeader+count*stanceListInfoSize != len(b) {
		t.Fatalf("%s's count %d with a %d-byte stride does not reach %d bytes",
			liveStanceList, count, stanceListInfoSize, len(b))
	}
	return b
}

// stanceListRegion names the part of the packet an offset falls in, so a failure says
// WHERE it is instead of only "+1234 differs".
func stanceListRegion(off int) string {
	switch {
	case off == 0 || off == 1:
		return "the cmd word (+0)"
	case off == 2 || off == 3:
		return "the length word (+2)"
	case off >= 4 && off < 8:
		return "the handle (+4)"
	case off == 8 || off == 9:
		return "the count word (+8)"
	case off >= stanceListHeader:
		rec := (off - stanceListHeader) / stanceListInfoSize
		rel := (off - stanceListHeader) % stanceListInfoSize
		return fmt.Sprintf("STANCE_INFO record %d, byte +%d of 14 (packet +%d)", rec, rel, off)
	}
	return fmt.Sprintf("past the end of the packet (+%d)", off)
}

// TestStanceListMatchesLiveCapture is the strongest evidence this lane can produce
// without a client: our build of 3626 against the live server's own bytes.
//
// The IDs AND the level/exp/skillPoint of every record are read OUT OF THE FIXTURE,
// because those three are the character's own progress (record 8 is 30110 FirstAid at
// level 25, exp 315303, sp 79) and this zone does not read a character's stance rows
// from the legacy database yet. Feeding them in is what makes the byte comparison a
// test of the LAYOUT: the stride, the field order inside the 14 bytes, the count, and a
// length word written from the finished buffer.
//
// THE ONLY ALLOWANCE IS THE HANDLE. The fixture belongs to the live server's handle
// 1560907; the packet is built for ours. The stance-id SET is named below as a second
// named difference, but it does not reach the byte loop: the fixture's own order is
// used to serialise, so the set cannot make a byte differ here - it is asserted
// separately, against the tables.
func TestStanceListMatchesLiveCapture(t *testing.T) {
	live := stanceListLiveBytes(t)
	count := int(binary.LittleEndian.Uint16(live[8:]))

	entries := make([]StanceEntry, 0, count)
	for i := 0; i < count; i++ {
		at := stanceListHeader + i*stanceListInfoSize
		entries = append(entries, StanceEntry{
			ID:         binary.LittleEndian.Uint32(live[at:]),
			Level:      binary.LittleEndian.Uint16(live[at+4:]),
			Exp:        binary.LittleEndian.Uint32(live[at+6:]),
			SkillPoint: binary.LittleEndian.Uint32(live[at+10:]),
		})
	}

	got := BuildStanceList(stanceListHandle, entries)
	if len(got) != len(live) {
		t.Fatalf("built %d bytes, the capture is %d - the frame geometry is wrong",
			len(got), len(live))
	}

	// The id of every record checked on its own BEFORE the byte loop, so its failure
	// names the record and not an offset.
	for i := 0; i < count; i++ {
		at := stanceListHeader + i*stanceListInfoSize
		want := binary.LittleEndian.Uint32(live[at:])
		if have := binary.LittleEndian.Uint32(got[at:]); have != want {
			t.Fatalf("STANCE_INFO record %d id = %d, the live server sends %d", i, have, want)
		}
	}

	identical, allowed := 0, 0
	var firstUnexplained string
	for i := range live {
		if got[i] == live[i] {
			identical++
			continue
		}
		if i >= 4 && i < 8 {
			allowed++ // our handle instead of the live character's
			continue
		}
		if firstUnexplained == "" {
			firstUnexplained = fmt.Sprintf("offset +%d (%s): ours 0x%02x, live 0x%02x",
				i, stanceListRegion(i), got[i], live[i])
		}
	}
	if firstUnexplained != "" {
		t.Fatalf("the built packet differs from the live capture at an offset that is "+
			"NOT the handle - %s", firstUnexplained)
	}
	t.Logf("%d of %d bytes identical, %d allowance(s), 0 unexplained",
		identical, len(live), allowed)

	// THE STANCE-ID SET, the second named difference: the live packet names the job's
	// DefaultStance list, this zone names every stance the job's conditions offer
	// (history\stance-skill-lists.md §4). The live set is therefore a SUBSET, and the
	// extras are printed so the difference is a number in the log and not a surprise.
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Fatalf("stance tables not readable (%v) - the id set cannot be checked", err)
	}
	ours := table.JobStances(3)
	liveSet := map[uint32]bool{}
	for _, e := range entries {
		liveSet[e.ID] = true
		if !intIn(ours, int(e.ID)) {
			t.Fatalf("the live packet names stance %d, which job 3's conditions do not "+
				"offer (%v) - the table no longer covers the measured set", e.ID, ours)
		}
	}
	var extras []int
	for _, id := range ours {
		if !liveSet[uint32(id)] {
			extras = append(extras, id)
		}
	}
	t.Logf("stance-id set: live names %d, datatable_job+datatable_stancecondition give "+
		"%d for job 3; this zone additionally names %v (a named difference, not a defect: "+
		"the live server sends only the job's DefaultStance names)",
		len(liveSet), len(ours), extras)
}

// TestStanceListFrameClosesAtEveryLiveCount pins the frame geometry against every size
// the live server was measured sending: (count -> total bytes), from the three 3626s in
// captures\skillcast20261031\20261031-153900. A builder with the wrong header size or
// the wrong record size closes at none of them.
func TestStanceListFrameClosesAtEveryLiveCount(t *testing.T) {
	for _, tc := range liveStanceListSizes {
		entries := make([]StanceEntry, 0, tc.count)
		for i := 0; i < tc.count; i++ {
			entries = append(entries, StanceEntry{
				ID:    uint32(10110 + i),
				Level: stanceEntryLevel,
				Exp:   stanceEntryExp,
			})
		}
		got := BuildStanceList(stanceListHandle, entries)
		if len(got) != tc.size {
			t.Fatalf("count %d built %d bytes, the live server sends %d",
				tc.count, len(got), tc.size)
		}
		if n := int(binary.LittleEndian.Uint16(got[2:])); n != tc.size {
			t.Fatalf("count %d: length word = %d, the packet is %d bytes", tc.count, n, len(got))
		}
		if n := int(binary.LittleEndian.Uint16(got[8:])); n != tc.count {
			t.Fatalf("count %d: the count word says %d", tc.count, n)
		}
	}
}

// TestStanceListUsesTheJobStances follows the PRODUCTION path: the ids come from
// t.JobStances(job) - the same call BuildStanceRecords packs into START_INFO, so the
// rows the client creates and the rows this packet unlocks cannot drift apart.
//
// Job 3 (Scout) is the captured character, so the count is a number the live server
// agrees with up to its DefaultStance filter: 12 in the tables, and all 9 the live
// packet named are among them.
func TestStanceListUsesTheJobStances(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	entries, unresolved := table.stanceEntries(3)
	if len(unresolved) != 0 {
		t.Fatalf("job 3's conditions name stances with no row in datatable_stance.xml: %v",
			unresolved)
	}
	ids := table.JobStances(3)
	if len(entries) != len(ids) {
		t.Fatalf("job 3 has %d stance(s) but the list carries %d", len(ids), len(entries))
	}
	if len(entries) == 0 {
		t.Fatalf("job 3 has no stances at all")
	}

	pkt := BuildStanceList(stanceListHandle, entries)
	if want := stanceListHeader + len(ids)*stanceListInfoSize; len(pkt) != want {
		t.Fatalf("%d bytes, want 10 + %d*14 = %d", len(pkt), len(ids), want)
	}
	if n := int(binary.LittleEndian.Uint16(pkt[2:])); n != len(pkt) {
		t.Fatalf("length word = %d, packet = %d bytes - they MUST agree", n, len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[4:]); got != stanceListHandle {
		t.Fatalf("handle = 0x%08x, want 0x%08x", got, stanceListHandle)
	}
	if n := int(binary.LittleEndian.Uint16(pkt[8:])); n != len(ids) {
		t.Fatalf("count = %d, want %d", n, len(ids))
	}
	for i, sid := range ids {
		at := stanceListHeader + i*stanceListInfoSize
		if got := binary.LittleEndian.Uint32(pkt[at:]); got != uint32(sid) {
			t.Fatalf("record %d id = %d, want the table's %d (same order as START_INFO)",
				i, got, sid)
		}
		if got := binary.LittleEndian.Uint16(pkt[at+4:]); got != stanceEntryLevel {
			t.Fatalf("record %d level = %d, want %d", i, got, stanceEntryLevel)
		}
		if got := binary.LittleEndian.Uint32(pkt[at+6:]); got != stanceEntryExp {
			t.Fatalf("record %d exp = %d, want %d", i, got, stanceEntryExp)
		}
		if got := binary.LittleEndian.Uint32(pkt[at+10:]); got != stanceEntrySkillPoint {
			t.Fatalf("record %d skillPoint = %d, want %d", i, got, stanceEntrySkillPoint)
		}
	}

	// Every stance the live packet named must be in ours, or the window keeps them
	// grey exactly as before.
	live := stanceListLiveBytes(t)
	count := int(binary.LittleEndian.Uint16(live[8:]))
	for i := 0; i < count; i++ {
		id := binary.LittleEndian.Uint32(live[stanceListHeader+i*stanceListInfoSize:])
		if !intIn(ids, int(id)) {
			t.Fatalf("the live packet unlocks stance %d and this list does not carry it", id)
		}
	}
	t.Logf("job 3: %d stance(s), %d bytes; the live Scout packet named %d of them",
		len(ids), len(pkt), count)
}

// TestStanceListWithholdsStancesItCannotDescribe is the "do not invent" rule at the one
// place it can bite: a stance-condition row that names a stance id datatable_stance.xml
// has no row for. Such an id has no name and no icon, so a record for it would be a
// zero the client draws as a blank row - it is WITHHELD, and the log names it.
//
// The table is built by hand because the shipped data has no such row (asserted for job
// 3 in TestStanceListUsesTheJobStances); this is the guard for the day one appears, e.g.
// a table left over from an older client.
func TestStanceListWithholdsStancesItCannotDescribe(t *testing.T) {
	const missing, knownA, knownB = 999999, 10110, 10120
	tbl := &StanceTable{
		rows: map[int]StanceRow{
			knownA: {Name: "BackGuard"},
			knownB: {Name: "HighGuard"},
		},
		jobs:  map[int][]int{99: {900}, 98: {901}},
		conds: map[int][]int{900: {knownA, missing, knownB}, 901: {missing}},
		hands: map[int][2]string{900: {"None", "None"}, 901: {"None", "None"}},
	}

	t.Run("the rest is still sent, and the log names the missing id", func(t *testing.T) {
		logs := &bytes.Buffer{}
		h := &Handler{Log: log.New(logs, "", 0), Stances: tbl}
		a := newPlayer(0x40000031, "Partial", [3]int32{1000, 1000, 1})
		a.sess.CommanderID = 4242
		a.sess.Team[0].Job = 99

		out := h.stanceListBurst(a.sess)
		if len(out) != 1 {
			t.Fatalf("the burst built %d packet(s), want the one with the two known stances",
				len(out))
		}
		pkt := out[0]
		if n := int(binary.LittleEndian.Uint16(pkt[8:])); n != 2 {
			t.Fatalf("count = %d, want the 2 stances this zone can describe", n)
		}
		if want := stanceListHeader + 2*stanceListInfoSize; len(pkt) != want {
			t.Fatalf("%d bytes, want %d", len(pkt), want)
		}
		if n := int(binary.LittleEndian.Uint16(pkt[2:])); n != len(pkt) {
			t.Fatalf("length word = %d, packet = %d bytes", n, len(pkt))
		}
		for i, want := range []uint32{knownA, knownB} {
			at := stanceListHeader + i*stanceListInfoSize
			if got := binary.LittleEndian.Uint32(pkt[at:]); got != want {
				t.Fatalf("record %d id = %d, want %d", i, got, want)
			}
		}
		// NO ZERO ENTRY: an id this zone cannot describe must not become a record.
		for i := 0; i < 2; i++ {
			at := stanceListHeader + i*stanceListInfoSize
			if id := binary.LittleEndian.Uint32(pkt[at:]); id == 0 || id == missing {
				t.Fatalf("record %d id = %d - an unresolvable stance was written anyway", i, id)
			}
		}
		logText := logs.String()
		if !strings.Contains(logText, "999999") {
			t.Fatalf("the log does not name the withheld stance 999999:\n%s", logText)
		}
		if !strings.Contains(logText, "4242") {
			t.Fatalf("the log does not name the commander:\n%s", logText)
		}
	})

	t.Run("nothing describable sends nothing, and says so", func(t *testing.T) {
		logs := &bytes.Buffer{}
		h := &Handler{Log: log.New(logs, "", 0), Stances: tbl}
		a := newPlayer(0x40000031, "Nothing", [3]int32{1000, 1000, 1})
		a.sess.CommanderID = 4243
		a.sess.Team[0].Job = 98

		if out := h.stanceListBurst(a.sess); len(out) != 0 {
			t.Fatalf("the burst built %d packet(s) for a job with nothing describable",
				len(out))
		}
		logText := logs.String()
		if !strings.Contains(logText, "4243") {
			t.Fatalf("the refusal did not name the commander:\n%s", logText)
		}
		if !strings.Contains(logText, "999999") {
			t.Fatalf("the refusal did not name the withheld stance:\n%s", logText)
		}
		if !strings.Contains(logText, "STANCELIST not sent") {
			t.Fatalf("the refusal does not say the packet was not sent:\n%s", logText)
		}
	})
}

// TestEntryBurstSendsStanceListBeforeSkillList pins the measured PAIR and its order: in
// the live entry burst every ZC_STANCELIST (3626) is immediately followed by that same
// handle's ZC_SKILLLIST (3621) - 69172 -> 69308, 70306 -> 70680, 72780 -> 72972 - and
// both come before the first 3520 spawn of the answer.
//
// The order is not cosmetic: the client's skill-icon test needs the stance row unlocked
// as well as the skill row (UISkillListBox.cpp:170), and the 3626 is what unlocks it.
func TestEntryBurstSendsStanceListBeforeSkillList(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), Stances: table}

	a := newTeam([]uint32{0x40000031, 0x40000032},
		[][3]int32{{1000, 1000, 1}, {1100, 1000, 1}})
	for i := range a.sess.Team {
		a.sess.Team[i].Job = 1 // Fighter: 30 stances in the tables, 119 skills
		a.sess.Team[i].Stance = 10110
	}
	b := newPlayer(0x40000041, "Watcher", [3]int32{2000, 2000, 1})
	h.joinZone(b.sess)
	b.conn.writes = nil

	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZGameReady})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	stanceAt := map[uint32]int{}
	skillAt := map[uint32]int{}
	firstSpawn := -1
	for i, p := range out {
		switch cmdOf(p) {
		case CmdZCStanceList:
			stanceAt[binary.LittleEndian.Uint32(p[4:])] = i
		case CmdZCSkillList:
			skillAt[binary.LittleEndian.Uint32(p[4:])] = i
		case CmdZCSpawnStandingPC:
			if firstSpawn < 0 {
				firstSpawn = i
			}
		}
	}
	if len(stanceAt) != len(a.sess.Team) {
		t.Fatalf("the answer carries %d ZC_STANCELIST packet(s), want one per team "+
			"member (%d)", len(stanceAt), len(a.sess.Team))
	}
	if firstSpawn < 0 {
		t.Fatalf("the answer carries no 3520 spawn, so the ordering cannot be checked")
	}

	wantStances := len(table.JobStances(1))
	wantSkills := len(JobSkillList(table, 1))
	for _, c := range a.sess.Team {
		at, ok := stanceAt[c.Handle]
		if !ok {
			t.Fatalf("no ZC_STANCELIST names handle 0x%08x - that character's stance "+
				"column would stay grey", c.Handle)
		}
		skill, ok := skillAt[c.Handle]
		if !ok {
			t.Fatalf("no ZC_SKILLLIST names handle 0x%08x", c.Handle)
		}
		if at > skill {
			t.Fatalf("handle 0x%08x's 3626 is packet %d and its 3621 is packet %d - the "+
				"live server sends the stance list FIRST", c.Handle, at, skill)
		}
		if at > firstSpawn {
			t.Fatalf("handle 0x%08x's 3626 is packet %d, the first 3520 is packet %d - "+
				"both lists belong in the entry burst", c.Handle, at, firstSpawn)
		}
		if n := int(binary.LittleEndian.Uint16(out[at][8:])); n != wantStances {
			t.Fatalf("handle 0x%08x lists %d stance(s), want job 1's %d",
				c.Handle, n, wantStances)
		}
		if n := int(binary.LittleEndian.Uint16(out[skill][8:])); n != wantSkills {
			t.Fatalf("handle 0x%08x lists %d skill(s), want job 1's whole %d",
				c.Handle, n, wantSkills)
		}
	}
	t.Logf("entry burst for job 1: %d stance(s) and %d skill(s) per character",
		wantStances, wantSkills)
}

// TestStanceChangeDoesNotResendStanceList pins the decision this lane took on evidence:
// a stance change sends NO 3626, because the set it names is the JOB's and a stance
// change cannot change a job. The live server agrees - the last ZC_CHANGE_STANCE in the
// capture is at 150431 and no 3626 follows it (history\stance-skill-lists.md).
//
// The 3621 that DOES follow a change is pinned by TestStanceChangeResendsSkillList.
func TestStanceChangeDoesNotResendStanceList(t *testing.T) {
	h, _, a, _ := stanceZone(t, 10110)
	frame := changeStanceFrame(0, 10120)
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	var have3554, have3621 bool
	for _, p := range out {
		switch cmdOf(p) {
		case CmdZCChangeStance:
			have3554 = true
		case CmdZCSkillList:
			have3621 = true
		case CmdZCStanceList:
			t.Fatalf("the stance change answered a 3626 - the unlocked stance set is the "+
				"job's and a stance change cannot move it (%d bytes)", len(p))
		}
	}
	if !have3554 || !have3621 {
		t.Fatalf("the stance change answered 3554=%v 3621=%v, want both", have3554, have3621)
	}
}
