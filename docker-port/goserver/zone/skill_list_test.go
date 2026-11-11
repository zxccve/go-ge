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

// THE LIVECAPTURE TEST FOR ZC_SKILLLIST (3621).
//
// The fixture is the RAW 1916 bytes of one ZC_SKILLLIST from the live server, cut out
// of captures\pcdialogs\20261022-213904\s2c_7054_55316.bin by
// `src\extract_replay_packets.py --cmd 3621` (that stream is 1502 packets with 0 bytes
// left over, so the packet boundaries are the capture's own). It is cmd 3621,
// len 1916, handle 188005, count 119, bmCount 119, sha256
// 73c3628b1feae4b57e18c2c1c3d2ba557e9dcfeffecd34bf75a1ef1c2c7aed70.
//
// It is a fixture and not a live read on purpose, for the same reason the spawn
// fixture is: the capture lives outside this repository, and a test that cannot run
// without it is a test that stops being run.
const liveSkillList = "testdata/ZC_SKILLLIST_3621.bin"

// The measured frame sizes: (SKILL_INFO count -> whole packet). Every one of these
// came out of a real ZC_SKILLLIST, 27 packets across 13 capture folders, and each one
// closes exactly as 12 + 10*count + 6*bmCount.
var liveSkillListSizes = []struct {
	count int
	size  int
}{
	{6, 108}, {10, 172}, {16, 268}, {20, 332}, {21, 348},
	{23, 380}, {34, 556}, {54, 876}, {56, 908}, {119, 1916},
}

// skillListLiveBytes reads the fixture and pins that it IS the packet it claims to be
// before anything compares against it: a wrong file would make every assertion below
// pass or fail for the wrong reason.
func skillListLiveBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(liveSkillList)
	if err != nil {
		t.Fatalf("read %s: %v", liveSkillList, err)
	}
	if len(b) != 1916 {
		t.Fatalf("%s is %d bytes, want the measured 1916", liveSkillList, len(b))
	}
	if cmd := cmdOf(b); cmd != CmdZCSkillList {
		t.Fatalf("%s is cmd %d, want %d", liveSkillList, cmd, CmdZCSkillList)
	}
	if n := binary.LittleEndian.Uint16(b[2:]); int(n) != len(b) {
		t.Fatalf("%s's length word says %d, the file is %d bytes - the fixture is not "+
			"the packet it claims to be", liveSkillList, n, len(b))
	}
	return b
}

// skillListRegion names the part of the packet a byte offset falls in, so a failure
// says WHERE it is instead of only "+1234 differs".
func skillListRegion(off, count int) string {
	infoEnd := skillListHeader + count*skillListInfoSize
	buffEnd := infoEnd + count*skillListBuffSize
	switch {
	case off == 0 || off == 1:
		return "the cmd word (+0)"
	case off == 2 || off == 3:
		return "the length word (+2)"
	case off >= 4 && off < 8:
		return "the handle (+4)"
	case off == 8 || off == 9:
		return "the count word (+8)"
	case off == 10 || off == 11:
		return "the bmCount word (+10)"
	case off >= skillListHeader && off < infoEnd:
		rec := (off - skillListHeader) / skillListInfoSize
		rel := (off - skillListHeader) % skillListInfoSize
		return fmt.Sprintf("SKILL_INFO record %d, byte +%d of 10 (packet +%d)", rec, rel, off)
	case off >= infoEnd && off < buffEnd:
		rec := (off - infoEnd) / skillListBuffSize
		rel := (off - infoEnd) % skillListBuffSize
		return fmt.Sprintf("BUFF_MODIFIER_SKILL_INFO record %d, byte +%d of 6 (packet +%d)",
			rec, rel, off)
	}
	return fmt.Sprintf("past the end of the packet (+%d)", off)
}

// skillListDiffAllowed answers why a differing byte offset is EXPECTED to differ, or
// "" when it is not allowed and the comparison must fail.
//
// The ONLY allowed differences are inside a SKILL_INFO record's level (2 bytes at
// record +4) and exp (4 bytes at record +6). Everything else - the command, the
// length word, the handle, the count and bmCount words, the ID of all 119 records and
// the WHOLE buff block - must match the live server byte for byte.
//
// Level and exp are allowed because they are the one thing this zone does not have:
// the live packet carries the character's LEARNED skills with their real levels and
// exp, read from the legacy database, while this zone sends the stance's skills at
// level 1 / exp 1 (118 of the 119 live records are exactly that, which is why 1/1 is
// the value chosen - but the 119th is level 10 / exp 0 and proves the field is real).
func skillListDiffAllowed(count int) func(off int) string {
	return func(off int) string {
		infoEnd := skillListHeader + count*skillListInfoSize
		if off < skillListHeader || off >= infoEnd {
			return ""
		}
		switch rel := (off - skillListHeader) % skillListInfoSize; {
		case rel == 4 || rel == 5:
			return "a SKILL_INFO record's level: we send 1, the live value differs"
		case rel >= 6 && rel <= 9:
			return "a SKILL_INFO record's exp: we send 1, the live value differs"
		}
		return ""
	}
}

// TestSkillListMatchesLiveCapture is the strongest evidence this lane can produce
// without a client: our build of the packet against the live server's own bytes.
//
// The IDs are read OUT OF THE FIXTURE on purpose. They are the captured character's
// learned set - 119 skills from 10115 up to 10415 - and this zone cannot compute that
// set: it has no skill table and does not read a character's learned skills from the
// legacy database. So the test compares the LAYOUT and the ordering, feeding our
// builder the live IDs and our own level/exp, which is the only part our builder
// decides.
func TestSkillListMatchesLiveCapture(t *testing.T) {
	live := skillListLiveBytes(t)
	count := int(binary.LittleEndian.Uint16(live[8:]))
	bmCount := int(binary.LittleEndian.Uint16(live[10:]))
	if count == 0 || bmCount != count {
		t.Fatalf("the fixture has count %d, bmCount %d - every measured sample has "+
			"bmCount == count and a non-zero count", count, bmCount)
	}

	entries := make([]SkillListEntry, 0, count)
	for i := 0; i < count; i++ {
		at := skillListHeader + i*skillListInfoSize
		entries = append(entries, SkillListEntry{
			ID:    binary.LittleEndian.Uint32(live[at:]),
			Level: skillListLevel,
			Exp:   skillListExp,
		})
	}

	got := BuildSkillList(binary.LittleEndian.Uint32(live[4:]), entries)
	if len(got) != len(live) {
		t.Fatalf("built %d bytes, the capture is %d - the frame geometry is wrong",
			len(got), len(live))
	}

	// The ID of every record and the whole buff block, checked on their own BEFORE
	// the byte loop, so their failure names the record and not an offset.
	for i := 0; i < count; i++ {
		at := skillListHeader + i*skillListInfoSize
		want := binary.LittleEndian.Uint32(live[at:])
		if have := binary.LittleEndian.Uint32(got[at:]); have != want {
			t.Fatalf("SKILL_INFO record %d id = %d, the live server sends %d", i, have, want)
		}
	}
	buffAt := skillListHeader + count*skillListInfoSize
	for i := 0; i < bmCount; i++ {
		at := buffAt + i*skillListBuffSize
		want := binary.LittleEndian.Uint32(live[at:])
		if have := binary.LittleEndian.Uint32(got[at:]); have != want {
			t.Fatalf("BUFF_MODIFIER record %d id = %d, the live server sends %d",
				i, have, want)
		}
		if lvl := binary.LittleEndian.Uint16(live[at+4:]); lvl != 0 {
			t.Fatalf("buffer record %d's live level is %d, not 0 - the measured shape "+
				"(level 0 in the buff block) is wrong", i, lvl)
		}
	}

	allow := skillListDiffAllowed(count)
	identical, allowed := 0, 0
	var firstUnexplained string
	for i := range live {
		if got[i] == live[i] {
			identical++
			continue
		}
		why := allow(i)
		if why == "" {
			if firstUnexplained == "" {
				firstUnexplained = fmt.Sprintf("offset +%d (%s): ours 0x%02x, live 0x%02x",
					i, skillListRegion(i, count), got[i], live[i])
			}
			continue
		}
		allowed++
	}
	if firstUnexplained != "" {
		t.Fatalf("the built packet differs from the live capture at an offset that is "+
			"NOT allowed to differ - %s", firstUnexplained)
	}
	t.Logf("%d of %d bytes identical, %d allowance(s), 0 unexplained",
		identical, len(live), allowed)
}

// TestSkillListFrameClosesAtEveryLiveCount pins the frame geometry against every
// count the live server was measured sending: (count -> total bytes), from 27
// ZC_SKILLLIST packets in 13 capture folders. A builder that got the header size or
// the record sizes wrong would close at none of these.
func TestSkillListFrameClosesAtEveryLiveCount(t *testing.T) {
	for _, tc := range liveSkillListSizes {
		entries := make([]SkillListEntry, 0, tc.count)
		for i := 0; i < tc.count; i++ {
			entries = append(entries, SkillListEntry{
				ID: uint32(10000 + i), Level: skillListLevel, Exp: skillListExp,
			})
		}
		got := BuildSkillList(0x40000031, entries)
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
		if n := int(binary.LittleEndian.Uint16(got[10:])); n != tc.count {
			t.Fatalf("count %d: the bmCount word says %d", tc.count, n)
		}
	}
}

// TestSkillListUsesTheStanceSkills follows the production path: the stance table says
// which skills a stance grants, and the packet carries exactly those, in the table's
// order, with the buff block repeating them at level 0.
//
// Stance 10110 BackGuard is job 1's Shield+Sword stance, and its row has
// SkillID1..5 = 10111, 0, 10113, 10114, 10115 - so a builder that dropped the 0 slot
// and kept the order produces four entries.
func TestSkillListUsesTheStanceSkills(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	const handle = uint32(0x40000031)
	want := []uint32{10111, 10113, 10114, 10115}

	entries := StanceSkillList(table, 10110)
	if len(entries) != len(want) {
		t.Fatalf("stance 10110 grants %d skill(s), want %d (%v)", len(entries), len(want), want)
	}
	for i, id := range want {
		if entries[i].ID != id {
			t.Fatalf("entry %d id = %d, want %d (the table's order)", i, entries[i].ID, id)
		}
	}

	pkt := BuildSkillList(handle, entries)
	if len(pkt) != 76 { // 12 + 10*4 + 6*4
		t.Fatalf("%d bytes, want 12 + 10*4 + 6*4 = 76", len(pkt))
	}
	if n := int(binary.LittleEndian.Uint16(pkt[2:])); n != len(pkt) {
		t.Fatalf("length word = %d, packet = %d bytes - they MUST agree", n, len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[4:]); got != handle {
		t.Fatalf("handle = 0x%08x, want 0x%08x", got, handle)
	}
	count := int(binary.LittleEndian.Uint16(pkt[8:]))
	bmCount := int(binary.LittleEndian.Uint16(pkt[10:]))
	if count != 4 || bmCount != 4 {
		t.Fatalf("count %d, bmCount %d, want 4 and 4", count, bmCount)
	}
	for i, id := range want {
		at := skillListHeader + i*skillListInfoSize
		if got := binary.LittleEndian.Uint32(pkt[at:]); got != id {
			t.Fatalf("record %d id = %d, want %d", i, got, id)
		}
		if lvl := binary.LittleEndian.Uint16(pkt[at+4:]); lvl != skillListLevel {
			t.Fatalf("record %d level = %d, want %d", i, lvl, skillListLevel)
		}
		if e := binary.LittleEndian.Uint32(pkt[at+6:]); e != skillListExp {
			t.Fatalf("record %d exp = %d, want %d", i, e, skillListExp)
		}
	}
	buffAt := skillListHeader + count*skillListInfoSize
	for i, id := range want {
		at := buffAt + i*skillListBuffSize
		if got := binary.LittleEndian.Uint32(pkt[at:]); got != id {
			t.Fatalf("buff record %d id = %d, want %d", i, got, id)
		}
		if lvl := binary.LittleEndian.Uint16(pkt[at+4:]); lvl != 0 {
			t.Fatalf("buff record %d level = %d, want the measured 0", i, lvl)
		}
	}
}

// TestEntryBurstSendsSkillListBeforeTheSpawn pins the ORDER inside CZ_GAME_READY's
// answer, and it is the live ordering: every ZC_SKILLLIST comes before the first
// ZC_SPAWN_STANDINGPC (3520). In the live entry capture the 3621 is packet 63, the
// same handle's 3520 is packet 83 and its 3572 packet 84.
//
// THE COUNT IS THE WHOLE JOB SET, not the current stance's four skills: job 1 is the
// captured Fighter, whose live 3621 carries 119 records, and that is what the widening
// to JobSkillList makes this burst write (see TestJobSkillListIsTheLiveSetForTheThreeCapturedJobs
// for the set, and history\stance-skill-lists.md §2 for the measurement).
//
// The 3520s in the answer are the OTHER player's characters (a session's own spawns
// left with its START_INFO), which is why the test needs a second player in the zone:
// without one, joinZone returns nothing and there would be no spawn to order against.
func TestEntryBurstSendsSkillListBeforeTheSpawn(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	logs := &bytes.Buffer{}
	h := &Handler{Log: log.New(logs, "", 0), Stances: table}

	// The session under test: a TWO-character team, both job 1 in 10110.
	a := newTeam([]uint32{0x40000031, 0x40000032},
		[][3]int32{{1000, 1000, 1}, {1100, 1000, 1}})
	for i := range a.sess.Team {
		a.sess.Team[i].Job = 1
		a.sess.Team[i].Stance = 10110
	}
	// Somebody already in the zone, so the answer carries real spawns.
	b := newPlayer(0x40000041, "Watcher", [3]int32{2000, 2000, 1})
	h.joinZone(b.sess)
	b.conn.writes = nil

	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZGameReady})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	var lists []int // indexes of the 3621s, in order
	firstSpawn := -1
	for i, p := range out {
		switch cmdOf(p) {
		case CmdZCSkillList:
			lists = append(lists, i)
		case CmdZCSpawnStandingPC:
			if firstSpawn < 0 {
				firstSpawn = i
			}
		}
	}
	if len(lists) != len(a.sess.Team) {
		t.Fatalf("the answer carries %d ZC_SKILLLIST packet(s), want one per team "+
			"member (%d)", len(lists), len(a.sess.Team))
	}
	if firstSpawn < 0 {
		t.Fatalf("the answer carries no 3520 spawn at all, so the ordering cannot be " +
			"checked")
	}
	for n, at := range lists {
		pkt := out[at]
		want := a.sess.Team[n].Handle
		if got := binary.LittleEndian.Uint32(pkt[4:]); got != want {
			t.Fatalf("ZC_SKILLLIST %d names handle 0x%08x, want team member %d's 0x%08x "+
				"(team order is the live order)", n, got, n, want)
		}
		if at > firstSpawn {
			t.Fatalf("ZC_SKILLLIST %d is packet %d, the first 3520 is packet %d - the "+
				"live server sends the skill list BEFORE the spawn", n, at, firstSpawn)
		}
		wantCount := len(JobSkillList(table, 1))
		if wantCount != 119 {
			t.Fatalf("job 1's whole skill set is %d record(s), the live Fighter packet "+
				"carries 119 - the tables changed", wantCount)
		}
		if count := int(binary.LittleEndian.Uint16(pkt[8:])); count != wantCount {
			t.Fatalf("ZC_SKILLLIST %d lists %d skill(s), want job 1's whole %d",
				n, count, wantCount)
		}
	}
}

// TestStanceChangeResendsSkillList pins that a stance change moves the SKILL BAR with
// the stance: the owner gets the 3554, then the 3684, then a 3621 for the NEW stance.
//
// It also pins WHO gets it: the owner alone. The watcher's screen does not show this
// character's skill bar, so it must receive the one 3554 and nothing else.
func TestStanceChangeResendsSkillList(t *testing.T) {
	h, _, a, b := stanceZone(t, 10110) // BackGuard: [10111, 10113, 10114, 10115]
	const handle = uint32(0x40000031)
	want := []uint32{10121, 10122, 10123, 10125} // HighGuard's SkillID1..5

	frame := changeStanceFrame(0, 10120)
	out, err := h.Reply(context.Background(), a.sess,
		protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	var stancePkt, listPkt []byte
	for _, p := range out {
		switch cmdOf(p) {
		case CmdZCChangeStance:
			stancePkt = p
		case CmdZCSkillList:
			listPkt = p
		}
	}
	if stancePkt == nil {
		t.Fatalf("the answer carries no 3554")
	}
	if got := binary.LittleEndian.Uint32(stancePkt[6:]); got != 10120 {
		t.Fatalf("the 3554 names stance %d, want 10120", got)
	}
	if listPkt == nil {
		t.Fatalf("the answer carries no 3621 - a stance change must re-send the skill bar")
	}
	if got := binary.LittleEndian.Uint32(listPkt[4:]); got != handle {
		t.Fatalf("the 3621 names handle 0x%08x, want the character's own 0x%08x", got, handle)
	}
	if count := int(binary.LittleEndian.Uint16(listPkt[8:])); count != len(want) {
		t.Fatalf("the 3621 lists %d skill(s), want 10120's %d", count, len(want))
	}
	for i, id := range want {
		at := skillListHeader + i*skillListInfoSize
		if got := binary.LittleEndian.Uint32(listPkt[at:]); got != id {
			t.Fatalf("skill %d of the re-sent list is %d, want 10120's %d (not the old "+
				"stance's)", i, got, id)
		}
	}

	seen := b.conn.sent()
	if len(seen) != 1 {
		t.Fatalf("the watcher received %d packet(s), want exactly the one 3554", len(seen))
	}
	if cmd := cmdOf(seen[0]); cmd != CmdZCChangeStance {
		t.Fatalf("the watcher's packet is cmd %d, want only the %d", cmd, CmdZCChangeStance)
	}
}

// TestSkillListRefusedWhenTheStanceHasNoSkills covers the one stance the tables say
// grants nothing: 10010 Bareknuckle, whose SkillID1..5 are all 0.
//
// Two halves, because there are two ways in: the entry burst (skillListBurst) and a
// legal stance change TO 10010. Neither may invent a list, and both must SAY so -
// silence would be indistinguishable from a wiring bug on the next run.
//
// The two halves now ask different questions, because the two paths differ on purpose:
//
//   - the STANCE CHANGE still sends that stance's own skills, so a stance that grants
//     none still sends nothing (unchanged);
//   - the ENTRY BURST sends the whole JOB set (JobSkillList), so it no longer cares
//     which stance the character stands in. Its "nothing to send" case is therefore a
//     job the tables give no skills for, which is what the first half now exercises -
//     the same refusal, checked against the only input that can still produce it.
func TestSkillListRefusedWhenTheStanceHasNoSkills(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	if ids := table.StanceSkills(10010); len(ids) != 0 {
		t.Fatalf("stance 10010 grants %v - this test is about a stance that grants "+
			"nothing, so the table changed and the test must be re-read", ids)
	}
	const unknownJob = 9999 // no row in datatable_job.xml, so no stances and no skills
	if ids := table.JobSkills(unknownJob); len(ids) != 0 {
		t.Fatalf("job %d has skills %v - the test needs a job with none", unknownJob, ids)
	}

	t.Run("entry burst", func(t *testing.T) {
		logs := &bytes.Buffer{}
		h := &Handler{Log: log.New(logs, "", 0), Stances: table}
		a := newPlayer(0x40000031, "Bare", [3]int32{1000, 1000, 1})
		a.sess.CommanderID = 4242
		a.sess.Team[0].Job = unknownJob
		a.sess.Team[0].Stance = 10010

		if got := h.skillListBurst(a.sess); len(got) != 0 {
			t.Fatalf("the burst built %d packet(s) for a character the tables give no "+
				"skills", len(got))
		}
		logText := logs.String()
		if !strings.Contains(logText, "4242") {
			t.Fatalf("the refusal did not name the commander id:\n%s", logText)
		}
		if !strings.Contains(logText, "9999") {
			t.Fatalf("the refusal did not name the job:\n%s", logText)
		}
	})

	t.Run("stance change", func(t *testing.T) {
		logs := &bytes.Buffer{}
		h := &Handler{Log: log.New(logs, "", 0), Stances: table}
		// A job 1 Fighter holding NOTHING: stance condition 100 (RHand=None LHand=None)
		// offers exactly 10010, so this change is legal and must be applied.
		a := newPlayer(0x40000031, "Bare", [3]int32{1000, 1000, 1})
		a.sess.Team[0].Job = 1
		a.sess.Team[0].Stance = 10110

		frame := changeStanceFrame(0, 10010)
		out, err := h.Reply(context.Background(), a.sess,
			protocol.Frame{Cmd: CmdCZChangeStance, Size: czChangeStanceSize, Data: frame})
		if err != nil {
			t.Fatalf("Reply: %v", err)
		}
		var have3554, have3684, have3621 bool
		for _, p := range out {
			switch cmdOf(p) {
			case CmdZCChangeStance:
				have3554 = true
			case CmdZCUpdateAllStatus:
				have3684 = true
			case CmdZCSkillList:
				have3621 = true
			}
		}
		if !have3554 {
			t.Fatalf("a legal change to 10010 answered no 3554")
		}
		if !have3684 {
			t.Fatalf("a legal change to 10010 answered no 3684")
		}
		if have3621 {
			t.Fatalf("a legal change to 10010 answered a 3621 - a stance that grants no "+
				"skills must send no list")
		}
		if got := a.sess.Team[0].Stance; got != 10010 {
			t.Fatalf("session stance = %d, want the accepted 10010", got)
		}
	})
}

// liveScoutSkillList is the 876-byte ZC_SKILLLIST that follows the 136-byte
// live_stance_list.bin in the capture - the same Scout (job 3), 54 records. Same pair,
// same character, which is what lets the two stance/skill tests agree about the set.
const liveScoutSkillList = "testdata/live_skill_list.bin"

// skillListIDsFromFixture pulls the SKILL_INFO ids out of a live ZC_SKILLLIST, after
// pinning that the file is the packet it claims to be.
func skillListIDsFromFixture(t *testing.T, path string, wantSize int) []uint32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) != wantSize {
		t.Fatalf("%s is %d bytes, want the measured %d", path, len(b), wantSize)
	}
	if cmd := cmdOf(b); cmd != CmdZCSkillList {
		t.Fatalf("%s is cmd %d, want %d", path, cmd, CmdZCSkillList)
	}
	if n := int(binary.LittleEndian.Uint16(b[2:])); n != len(b) {
		t.Fatalf("%s's length word says %d, the file is %d bytes", path, n, len(b))
	}
	count := int(binary.LittleEndian.Uint16(b[8:]))
	ids := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		ids = append(ids, binary.LittleEndian.Uint32(b[skillListHeader+i*skillListInfoSize:]))
	}
	return ids
}

// TestJobSkillListIsTheLiveSetForTheThreeCapturedJobs is the test the WIDENING stands
// on: for all three characters in the owner's capture, the set this zone now sends is
// the set the live server sent - the SAME ids, not merely a similar count.
//
//	Scout   job 3: 54 ids, compared against testdata/live_skill_list.bin
//	Fighter job 1: 119 ids, compared against testdata/ZC_SKILLLIST_3621.bin
//	Warlock job 5: 56 ids, from the measurement (908-byte packet, no fixture kept)
//
// The two ids no stance row names - 10415 HighlanderSoul and 1005 ManaActivation - are
// checked separately, because they are the half a "union of the stances" list would
// silently miss: they come from the job row's own `JobSkill` column.
//
// The ORDER is not compared on purpose: the live order is a hash-map walk and the client
// matches by `type` (history\stance-skill-lists.md §2).
func TestJobSkillListIsTheLiveSetForTheThreeCapturedJobs(t *testing.T) {
	table, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}

	cases := []struct {
		name  string
		job   int
		count int
		live  []uint32
	}{
		{"Scout", 3, 54, skillListIDsFromFixture(t, liveScoutSkillList, 876)},
		{"Fighter", 1, 119, skillListIDsFromFixture(t, liveSkillList, 1916)},
		{"Warlock", 5, 56, nil},
	}
	for _, tc := range cases {
		got := JobSkillList(table, tc.job)
		if len(got) != tc.count {
			t.Fatalf("%s (job %d): %d skill(s), the live server sends %d",
				tc.name, tc.job, len(got), tc.count)
		}
		ours := map[uint32]bool{}
		for _, e := range got {
			if ours[e.ID] {
				t.Fatalf("%s (job %d): skill %d listed twice - the live packet has no repeats",
					tc.name, tc.job, e.ID)
			}
			ours[e.ID] = true
			if e.Level != skillListLevel || e.Exp != skillListExp {
				t.Fatalf("%s (job %d): skill %d is level %d / exp %d, want the placeholder "+
					"%d / %d", tc.name, tc.job, e.ID, e.Level, e.Exp,
					skillListLevel, skillListExp)
			}
		}
		if tc.live == nil {
			continue
		}
		if len(tc.live) != tc.count {
			t.Fatalf("%s: the fixture has %d records, the measurement says %d",
				tc.name, len(tc.live), tc.count)
		}
		for _, id := range tc.live {
			if !ours[id] {
				t.Fatalf("%s (job %d): the live packet lists skill %d and ours does not",
					tc.name, tc.job, id)
			}
		}
		t.Logf("%s (job %d): the widened list writes %d skill(s); the live packet carries "+
			"%d - the same %d id(s)", tc.name, tc.job, len(got), len(tc.live), tc.count)
	}
	for _, tc := range cases {
		if tc.live == nil {
			t.Logf("%s (job %d): the widened list writes %d skill(s); the live server sent "+
				"%d (measured, 908-byte packet, no fixture kept)", tc.name, tc.job,
				len(JobSkillList(table, tc.job)), tc.count)
		}
	}

	// The job's OWN skill, from datatable_job.xml `JobSkill`, is the one id no stance
	// row names. Checked by name so a broken column read fails loudly.
	for _, tc := range []struct {
		job    int
		skill  int
		column bool // also assert no stance row names it
	}{
		{1, 10415, true},  // HighlanderSoul
		{5, 1005, true},   // ManaActivation
		{3, 30113, false}, // the Scout's own job skill is already a stance skill
	} {
		if got := table.JobSkill(tc.job); got != tc.skill {
			t.Fatalf("job %d's JobSkill column reads %d, want %d (datatable_job.xml)",
				tc.job, got, tc.skill)
		}
		if !intIn(idsOf(JobSkillList(table, tc.job)), tc.skill) {
			t.Fatalf("job %d's own skill %d is not in its list", tc.job, tc.skill)
		}
		if tc.column && intIn(table.JobSkills(tc.job), tc.skill) {
			t.Fatalf("stance rows name %d for job %d - this test assumes they do not",
				tc.skill, tc.job)
		}
	}
}

// idsOf is the ids of a list, for the membership checks above.
func idsOf(list []SkillListEntry) []int {
	out := make([]int, 0, len(list))
	for _, e := range list {
		out = append(out, int(e.ID))
	}
	return out
}
