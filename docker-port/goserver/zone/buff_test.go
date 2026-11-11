package zone

// THE BUFF SLICE'S OWN PROOF (research\item-buff-skill.md §5).
//
// Every test here runs against the SHIPPED data: `ge\xml\datatable_buff.xml` (1,962
// rows) and the real corpus (`out\lua_corpus`, built by src\scp_to_lua.py). Nothing
// is a hand-built mirror of the table, so a test that passes is a statement about the
// game's own files - and each test SKIPS, rather than passing on zeros, when the
// corpus or the XML is not on this machine.
//
// The four pass conditions and where each lives:
//
//	table loads 1,962 rows, a ClassName resolves, UpdateType/ApplyTime asserted
//	    TestTheBuffTableLoadsEveryRowAndResolvesAKnownBuff
//	a NORMAL buff expires exactly at its ApplyTime, an INFINITY one never, driven by the tick
//	    TestANormalBuffExpiresWhenTheTickPassesItsApplyTime
//	    TestAnInfinityBuffNeverExpires
//	a scriptless NORMAL buff is refused and never granted with 0
//	    TestAScriptlessNormalBuffIsRefusedAndNeverGrantedWithZero
//	3556 goes out with the right bit set (the 262 bytes' bit, not "a packet")
//	    TestTheChangeBuffPacketCarriesTheBitAtTheBuffsClassID
//
// and the extras this slice's other rules need: the 3125 struct, the SCRIPT refusal,
// the rank rule, the session clock, and the item path that makes the owner's screen
// the acceptance test.

import (
	"bytes"
	"encoding/binary"
	"log"
	"strings"
	"testing"
	"time"
)

// buffZone is the fixture: the REAL Buff table, the REAL corpus on the package's one
// script host (zone\main_test.go, TestMain), and a session with one character.
func buffZone(t *testing.T) (*Handler, *player, *bytes.Buffer) {
	t.Helper()
	requireStatus(t)
	table, err := LoadBuffs(XMLDir())
	if err != nil {
		t.Skipf("the Buff table is not readable (%v) - a test must not pass on zeros", err)
	}
	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)
	h := &Handler{Log: logger, Buffs: table, Status: defaultStatusForTests}
	bs, err := NewBuffScript(testScriptHost, logger.Printf)
	if err != nil {
		t.Fatalf("arming the buff corpus failed: %v", err)
	}
	bs.Sheet = defaultStatusForTests
	h.BuffScript = bs
	return h, newPlayer(1792, "Grace", [3]int32{0, 0, 0}), logs
}

// addBuff is the tests' one call into the policy, with the failure printed.
func addBuff(t *testing.T, h *Handler, p *player, name string, a1, a2, a3 float64) [][]byte {
	t.Helper()
	out, added, why := h.AddBuff(p.sess, 0, name, a1, a2, a3, 0)
	if !added {
		t.Fatalf("%s was REFUSED (%s) - this test needs it granted", name, why)
	}
	return out
}

// TestTheBuffTableLoadsEveryRowAndResolvesAKnownBuff is pass condition 1.
//
// The counts are the SHIPPED file's own: 1,962 rows, UpdateType NORMAL 1,093 /
// SCRIPT 406 / INFINITY 463, and ApplyTime non-zero on exactly 8 of them (Aroma 817,
// P_BloodMark 825, E_BloodMark 826, Fear2 827, Haste3 828, Coma 887, MagicPlague 955,
// OldShock 1107). A loader that dropped the tail of the file, or that read
// `UpdateType` case-sensitively, fails on one of those numbers.
func TestTheBuffTableLoadsEveryRowAndResolvesAKnownBuff(t *testing.T) {
	table, err := LoadBuffs(XMLDir())
	if err != nil {
		t.Skipf("the Buff table is not readable (%v)", err)
	}
	if got := table.Len(); got != 1962 {
		t.Fatalf("the table loads %d row(s), want 1962 (ge\\xml\\datatable_buff.xml)", got)
	}

	counts := map[string]int{}
	nonZeroApply := 0
	for id := uint32(1); id <= 2047; id++ {
		r, ok := table.RowByID(id)
		if !ok {
			continue
		}
		counts[r.UpdateType]++
		if r.ApplyTime != 0 {
			nonZeroApply++
		}
		if r.ID != id || r.Name == "" {
			t.Fatalf("row %d decodes as id %d name %q - the ClassID/ClassName pair is the "+
				"key every script uses", id, r.ID, r.Name)
		}
		if r.Attrs.Get("ClassName") != r.Name || r.Attrs.Get("UpdateType") == "" {
			t.Fatalf("row %d (%s) kept %d raw attribute(s) and its ClassName reads %q - the "+
				"ENTER script's `buff` table is seeded from those attributes",
				id, r.Name, r.Attrs.Len(), r.Attrs.Get("ClassName"))
		}
	}
	if counts[BuffUpdateNormal] != 1093 || counts[BuffUpdateScript] != 406 ||
		counts[BuffUpdateInfinity] != 463 {
		t.Fatalf("UpdateType populations are NORMAL %d / SCRIPT %d / INFINITY %d, want "+
			"1093 / 406 / 463 (the shipped file)",
			counts[BuffUpdateNormal], counts[BuffUpdateScript], counts[BuffUpdateInfinity])
	}
	if nonZeroApply != 8 {
		t.Fatalf("%d row(s) carry a non-zero ApplyTime, want 8 - the duration is the ENTER "+
			"script's, and only eight rows carry one of their own", nonZeroApply)
	}

	// ONE KNOWN BUFF, FIELD BY FIELD. Revenge is `buff.scp:22-26`'s own example: the
	// table says NORMAL / ApplyTime 0 / group 702 / rank 0, and its ENTER writes 30000.
	revenge, ok := table.Row("Revenge")
	if !ok {
		t.Fatalf("the table has no row named Revenge (ClassID 145)")
	}
	if revenge.ID != 145 {
		t.Fatalf("Revenge resolves to ClassID %d, want 145 - that id is the mask bit", revenge.ID)
	}
	if revenge.UpdateType != BuffUpdateNormal {
		t.Fatalf("Revenge's UpdateType is %q, want NORMAL", revenge.UpdateType)
	}
	if revenge.ApplyTime != 0 {
		t.Fatalf("Revenge's table ApplyTime is %g, want 0 - its 30 s lives in its ENTER script", revenge.ApplyTime)
	}
	if revenge.GroupIndex != "702" || revenge.BuffRank != 0 {
		t.Fatalf("Revenge is group %q rank %d, want group 702 rank 0", revenge.GroupIndex, revenge.BuffRank)
	}
	if revenge.EnterEntry() != "SCR_BUFF_ENTER_Revenge" {
		t.Fatalf("the ENTER entry is %q", revenge.EnterEntry())
	}

	// The two other UpdateTypes, by a name each.
	if r, ok := table.Row("Clique"); !ok || r.UpdateType != BuffUpdateInfinity || r.ID != 43 {
		t.Fatalf("Clique decodes as %+v, want ClassID 43 INFINITY", r)
	}
	if r, ok := table.Row("Sandmann"); !ok || r.UpdateType != BuffUpdateScript || r.ID != 7 {
		t.Fatalf("Sandmann decodes as %+v, want ClassID 7 SCRIPT", r)
	}

	// AN UNKNOWN NAME IS NOT A ZERO ROW. If Row answered a row here, every name the
	// scripts misspell would be granted as a 0 ms buff with group "".
	if _, ok := table.Row("NotABuffAnywhere"); ok {
		t.Fatal("an unknown ClassName resolved to a row - a name the table does not carry " +
			"must never be granted from invented values")
	}

	// THE TWO COUNTS THE SLICE TURNS ON, measured against the SHIPPED corpus rather
	// than quoted from a note: how many rows have an ENTER (their duration exists) and
	// how many are the refused shape (NORMAL, ApplyTime 0, no ENTER). The research's
	// figures were 1,193 / 387 against `buff.scp` alone; the host's index covers the
	// WHOLE corpus (`itembuff.scp`, `partybuff.scp`, ...), which is what the code asks.
	requireStatus(t) // the corpus host is what answers HasScript
	enter, refused := 0, 0
	for id := uint32(1); id <= 2047; id++ {
		r, ok := table.RowByID(id)
		if !ok {
			continue
		}
		hasEnter := testScriptHost.HasScript(r.EnterEntry())
		if hasEnter {
			enter++
		}
		if !hasEnter && r.UpdateType == BuffUpdateNormal && r.ApplyTime == 0 {
			refused++
		}
	}
	t.Logf("buff table: %d rows, %d with an ENTER, %d scriptless NORMAL rows that can "+
		"never be granted with 0", table.Len(), enter, refused)
	if enter == 0 || enter == table.Len() {
		t.Fatalf("%d of %d rows have an ENTER - neither zero nor all of them is believable",
			enter, table.Len())
	}
	if refused == 0 {
		t.Fatal("no scriptless NORMAL row with ApplyTime 0 was found - the refusal rule " +
			"would have nothing to refuse")
	}
}

// TestANormalBuffExpiresWhenTheTickPassesItsApplyTime is pass condition 2's NORMAL
// half, DRIVEN BY THE TICK (Handler.tickIfDue -> chaseTick -> buffTick), which is the
// only path that can expire a buff on a player who stands still.
func TestANormalBuffExpiresWhenTheTickPassesItsApplyTime(t *testing.T) {
	h, p, logs := buffZone(t)
	addBuff(t, h, p, "Revenge", 1, 0, 0)

	l := p.sess.liveBuffs(0)
	if l.Len() != 1 || !l.IsBuffApplied("Revenge") {
		t.Fatalf("Revenge is not on the list after AddBuff: %v", l.Names())
	}
	// THE DURATION IS THE SCRIPT'S, not the table's: `buff.ApplyTime = 30000`
	// (ge\script\buff\buff.scp:24), and the row's own column says 0.
	if got := l.buffs[0].ApplyTime; got != 30000 {
		t.Fatalf("Revenge's duration is %g ms, want 30000 - the ENTER script's own value", got)
	}
	if got := l.buffs[0].BM["DR_BM"]; got != 5 {
		t.Fatalf("Revenge's DR_BM is %g, want 5 (the same ENTER sets it)", got)
	}

	// 100 ms SHORT: the buff is still up and the tick sends NOTHING.
	p.sess.buffAt = time.Now().Add(-29900 * time.Millisecond)
	p.sess.lastTick = time.Time{}
	if out, ran := h.tickIfDue(p.sess); !ran || len(out) != 0 {
		t.Fatalf("the tick 100 ms short of 30 s returned %d packet(s) (ran=%v), want 0", len(out), ran)
	}
	if !p.sess.liveBuffs(0).IsBuffApplied("Revenge") {
		t.Fatal("the buff expired BEFORE its ApplyTime")
	}

	// AT the boundary the reference KEEPS it: `if (m_elapsedTime <= GetApplyTime())
	// return 1` (Buff.cpp:155-161), i.e. elapsed == ApplyTime is still applied. A wall
	// clock cannot land on exactly 30000, so the boundary is pinned on the list's own
	// rule and the tick proves the two sides of it.
	probe := &buffList{}
	probe.Add(&buff{Name: "probe", UpdateType: BuffUpdateNormal, ApplyTime: 30000})
	if expired := probe.Update(30000); len(expired) != 0 {
		t.Fatalf("Update(30000) expired the buff: the reference expires on elapsed > ApplyTime, " +
			"not >=")
	}
	if expired := probe.Update(0.5); len(expired) != 1 {
		t.Fatal("Update(0.5) past the boundary did not expire the buff")
	}

	// PAST IT: the tick expires the buff and sends ONE 3556 whose mask no longer has
	// the bit. (buffTick measures `now - buffAt`, so 30,100 ms is 100 ms past.)
	p.sess.buffAt = time.Now().Add(-30100 * time.Millisecond)
	p.sess.lastTick = time.Time{}
	out, ran := h.tickIfDue(p.sess)
	if !ran {
		t.Fatal("the tick did not run")
	}
	if len(out) != 1 {
		t.Fatalf("the expiry tick returned %d packet(s), want exactly one 3556", len(out))
	}
	if got := binary.LittleEndian.Uint16(out[0][0:]); got != CmdZCChangeBuff {
		t.Fatalf("the expiry's packet is cmd %d, want %d", got, CmdZCChangeBuff)
	}
	mask := out[0][6:]
	for i, b := range mask {
		if b != 0 {
			t.Fatalf("mask byte %d is 0x%02x after the only buff expired, want the whole bar clear",
				i, b)
		}
	}
	if p.sess.liveBuffs(0).Len() != 0 {
		t.Fatalf("the list still holds %v after the expiry tick", p.sess.liveBuffs(0).Names())
	}
	if n := strings.Count(logs.String(), "BUFF EXPIRED"); n != 1 {
		t.Fatalf("%d EXPIRED line(s) in the log, want exactly 1:\n%s", n, logs.String())
	}
}

// TestAnInfinityBuffNeverExpires is pass condition 2's other half: Clique is
// `UpdateType=INFINITY`, and an hour of tick time must leave it up with no packet.
func TestAnInfinityBuffNeverExpires(t *testing.T) {
	h, p, _ := buffZone(t)
	addBuff(t, h, p, "Clique", 1, 0, 0)

	p.sess.buffAt = time.Now().Add(-time.Hour)
	p.sess.lastTick = time.Time{}
	if out, ran := h.tickIfDue(p.sess); !ran || len(out) != 0 {
		t.Fatalf("the tick after an hour returned %d packet(s) (ran=%v), want 0 - an INFINITY "+
			"buff does not expire", len(out), ran)
	}
	l := p.sess.liveBuffs(0)
	if !l.IsBuffApplied("Clique") {
		t.Fatal("Clique expired: UpdateType INFINITY returns 1 before any time is compared " +
			"(Buff.cpp:157-160)")
	}
	if b := l.buffs[0]; b.ApplyTime != 0 {
		t.Fatalf("Clique's ApplyTime is %g, want 0 - an INFINITY row is never given a duration",
			b.ApplyTime)
	}
}

// TestAScriptlessNormalBuffIsRefusedAndNeverGrantedWithZero is pass condition 3.
//
// `Execution` (ClassID 1005) is NORMAL, its own ApplyTime is 0, and the corpus
// defines no `SCR_BUFF_ENTER_Execution` - 325 of the 1,962 rows are that shape. The
// test asserts the DATA first (so it cannot quietly stop being about a scriptless
// buff) and then that granting it is refused with nothing on the wire.
func TestAScriptlessNormalBuffIsRefusedAndNeverGrantedWithZero(t *testing.T) {
	h, p, logs := buffZone(t)
	row, ok := h.Buffs.Row("Execution")
	if !ok {
		t.Fatalf("the table has no row named Execution (ClassID 1005)")
	}
	if row.UpdateType != BuffUpdateNormal || row.ApplyTime != 0 {
		t.Fatalf("Execution is %s apply %g, want NORMAL apply 0 - this test's premise",
			row.UpdateType, row.ApplyTime)
	}
	if h.BuffScript.Has(row.EnterEntry()) {
		t.Fatalf("the corpus now defines %s - this test's premise is that it defines NONE, so "+
			"the case must be re-picked rather than left asserting an old fact", row.EnterEntry())
	}

	out, added, why := h.AddBuff(p.sess, 0, "Execution", 1, 0, 0, 0)
	if added {
		t.Fatalf("a scriptless NORMAL buff with ApplyTime 0 was GRANTED (%s) - it would expire "+
			"on the first tick, which is the 0-duration grant the rail forbids", why)
	}
	if len(out) != 0 {
		t.Fatalf("the refusal sent %d packet(s), want none", len(out))
	}
	if l := p.sess.liveBuffs(0); l.Len() != 0 {
		t.Fatalf("the refusal left %v on the list", l.Names())
	}
	if !strings.Contains(logs.String(), "BUFF REFUSED") ||
		!strings.Contains(logs.String(), "no duration") {
		t.Fatalf("the refusal is not in the log with its reason:\n%s", logs.String())
	}

	// AND THE SAME ROW WITH NO CORPUS AT ALL is refused the same way - the scriptless
	// answer must not depend on the host being wired.
	h.BuffScript = nil
	if _, added, _ := h.AddBuff(p.sess, 0, "Execution", 1, 0, 0, 0); added {
		t.Fatal("with no corpus the scriptless NORMAL buff was GRANTED")
	}

	// BUT A 0 IS LEGAL WHERE IT IS THE ROW'S OWN ANSWER: an INFINITY buff with no
	// ENTER never expires, so there is no duration to be missing (num09, ClassID 1054).
	num, ok := h.Buffs.Row("num09")
	if !ok || num.UpdateType != BuffUpdateInfinity {
		t.Fatalf("num09 is %+v, want an INFINITY row", num)
	}
	if h.BuffScript != nil && h.BuffScript.Has(num.EnterEntry()) {
		t.Skipf("num09 now has an ENTER - pick another scriptless INFINITY row")
	}
	addBuff(t, h, p, "num09", 1, 0, 0)
	if !p.sess.liveBuffs(0).IsBuffApplied("num09") {
		t.Fatal("a scriptless INFINITY buff was refused: 0 is its own (correct) duration")
	}
}

// TestAScriptBuffIsRefusedRatherThanDrawnWithoutAnEnd pins the one UpdateType this
// slice does NOT run. A SCRIPT buff's expiry is its `SCR_BUFF_UPDATE_*` entry's
// answer (Buff.cpp:163-186); granting one without that entry draws an icon nothing
// can ever clear, which is worse than the refusal.
func TestAScriptBuffIsRefusedRatherThanDrawnWithoutAnEnd(t *testing.T) {
	h, p, logs := buffZone(t)
	if _, added, why := h.AddBuff(p.sess, 0, "Sandmann", 1, 0, 0, 0); added {
		t.Fatalf("a SCRIPT buff was granted (%s)", why)
	}
	if !strings.Contains(logs.String(), "UpdateType SCRIPT") {
		t.Fatalf("the refusal does not name the reason:\n%s", logs.String())
	}
	if p.sess.liveBuffs(0).Len() != 0 {
		t.Fatal("the refused SCRIPT buff stayed on the list")
	}
}

// TestAHigherRankOfTheSameGroupRefusesALowerOne is `CBuffList::ExistHigherRank`
// (BuffList.cpp:62-99) on a real pair: group 110 holds BlessOfAncient (rank 1) and
// Clique (rank 2), so applying Clique first makes BlessOfAncient a refusal.
func TestAHigherRankOfTheSameGroupRefusesALowerOne(t *testing.T) {
	h, p, logs := buffZone(t)
	addBuff(t, h, p, "Clique", 1, 0, 0) // rank 2, INFINITY
	if _, added, _ := h.AddBuff(p.sess, 0, "BlessOfAncient", 1, 0, 0, 0); added {
		t.Fatal("rank 1 of group 110 was granted while rank 2 was applied")
	}
	if !strings.Contains(logs.String(), "ExistHigherRank") {
		t.Fatalf("the refusal does not cite the rule:\n%s", logs.String())
	}
	if p.sess.liveBuffs(0).Len() != 1 {
		t.Fatalf("the list is %v, want only Clique", p.sess.liveBuffs(0).Names())
	}
}

// TestTheSameGroupReplacesAndTheBarIsResentInOnePacket is the other half of the group
// rule: a NEW buff of the same group removes the old one, and the ONE 3556 that goes
// out carries both changes (the old bit cleared, the new bit set) - because 3556 is
// the whole bar, not a delta.
func TestTheSameGroupReplacesAndTheBarIsResentInOnePacket(t *testing.T) {
	h, p, _ := buffZone(t)
	addBuff(t, h, p, "BlessOfAncient", 1, 0, 0) // 171, rank 1, group 110
	out := addBuff(t, h, p, "Clique", 1, 0, 0)  // 43, rank 2, same group

	if got := p.sess.liveBuffs(0).Names(); len(got) != 1 || got[0] != "Clique" {
		t.Fatalf("the list is %v, want only Clique - the same-group buff is replaced", got)
	}
	if len(out) != 2 {
		t.Fatalf("the replacement sent %d packet(s), want 3556 + 3125", len(out))
	}
	mask := out[0][6:]
	if b := mask[171>>3]; b&(1<<(171&7)) != 0 {
		t.Fatalf("mask byte %d still carries BlessOfAncient's bit 0x%02x after it was replaced",
			171>>3, 1<<(171&7))
	}
	if b := mask[43>>3]; b&(1<<(43&7)) == 0 {
		t.Fatalf("mask byte %d does not carry Clique's bit 0x%02x", 43>>3, 1<<(43&7))
	}
}

// TestTheChangeBuffPacketCarriesTheBitAtTheBuffsClassID is pass condition 4: the 262
// bytes' own bit, at the buff's ClassID, with the index rule the live capture
// measured (buff 38 -> mask byte 4 = 0x40, history\combat.md §10.7).
func TestTheChangeBuffPacketCarriesTheBitAtTheBuffsClassID(t *testing.T) {
	h, p, _ := buffZone(t)

	// THE MEASURED REFERENCE BIT FIRST: 38 >> 3 = byte 4, 38 & 7 = bit 6 -> 0x40.
	var one [BuffMaskSize]byte
	buffMaskBit(&one, 38)
	if one[4] != 0x40 {
		t.Fatalf("buff 38 sets mask byte 4 to 0x%02x, want 0x40 - the live capture's own "+
			"value, and the index every icon on the bar depends on", one[4])
	}

	out := addBuff(t, h, p, "Revenge", 1, 0, 0)
	if len(out) != 2 {
		t.Fatalf("a granted buff sent %d packet(s), want 3556 then 3125", len(out))
	}
	pkt := out[0]
	if len(pkt) != ChangeBuffSize || ChangeBuffSize != 262 {
		t.Fatalf("3556 is %d bytes, want 262 (2 cmd + 4 handle + 256 mask)", len(pkt))
	}
	if got := binary.LittleEndian.Uint16(pkt[0:]); got != 3556 {
		t.Fatalf("the packet's command is %d, want 3556", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[2:]); got != 1792 {
		t.Fatalf("the packet's handle is %d, want 1792", got)
	}
	mask := pkt[6:]
	if len(mask) != BuffMaskSize {
		t.Fatalf("the mask is %d bytes, want %d", len(mask), BuffMaskSize)
	}
	if got := mask[145>>3]; got != 1<<(145&7) {
		t.Fatalf("mask byte %d is 0x%02x, want 0x%02x - Revenge's ClassID 145 is 145>>3 = 18, "+
			"bit 1", 145>>3, got, 1<<(145&7))
	}
	if n := countMaskBits(mask); n != 1 {
		t.Fatalf("the bar carries %d bit(s) after one buff, want exactly 1", n)
	}

	// A SECOND buff adds its own bit and KEEPS the first: 43 >> 3 = byte 5, bit 3.
	out = addBuff(t, h, p, "Clique", 1, 0, 0)
	mask = out[0][6:]
	if got := mask[43>>3]; got&(1<<(43&7)) == 0 {
		t.Fatalf("mask byte %d does not carry Clique's bit (0x%02x), got 0x%02x",
			43>>3, 1<<(43&7), got)
	}
	if got := mask[145>>3]; got&(1<<(145&7)) == 0 {
		t.Fatalf("mask byte %d lost Revenge's bit when Clique was added: 0x%02x", 145>>3, got)
	}
	if n := countMaskBits(mask); n != 2 {
		t.Fatalf("the bar carries %d bit(s) after two buffs, want exactly 2", n)
	}
}

// TestTheRemainTimePacketIsTheReferenceStruct pins 3125 field for field against
// `PKS_ZC_BUFF_REMAINTIME` (geZCPacket.h:3596-3601): pcIndex, buffType, level,
// duration - and `level` is the buff's applied level, which is arg1.
func TestTheRemainTimePacketIsTheReferenceStruct(t *testing.T) {
	h, p, _ := buffZone(t)
	out := addBuff(t, h, p, "Revenge", 7, 0, 0) // arg1 = 7 = the buff's level

	rt := out[1]
	if len(rt) != BuffRemainTimeSize || BuffRemainTimeSize != 15 {
		t.Fatalf("3125 is %d bytes, want 15", len(rt))
	}
	if got := binary.LittleEndian.Uint16(rt[0:]); got != 3125 {
		t.Fatalf("the command is %d, want 3125", got)
	}
	if got := binary.LittleEndian.Uint32(rt[2:]); got != 1792 {
		t.Fatalf("+2 is %d, want the character handle 1792", got)
	}
	if got := binary.LittleEndian.Uint32(rt[6:]); got != 145 {
		t.Fatalf("+6 is the buff id %d, want 145 (Revenge's ClassID)", got)
	}
	if got := rt[10]; got != 7 {
		t.Fatalf("+10 is the level byte %d, want 7 (the arg1 the buff was applied with)", got)
	}
	if got := binary.LittleEndian.Uint32(rt[11:]); got != 30000 {
		t.Fatalf("+11 is %d ms remaining, want the 30000 the ENTER script set", got)
	}
}

// TestTheSessionClockWakesForALiveBuff is the rail's item 6: without this reason the
// zone's read never wakes for an idle player and a 30 s buff would never expire.
func TestTheSessionClockWakesForALiveBuff(t *testing.T) {
	h, p, _ := buffZone(t)
	if h.sessionClockAwake(p.sess) {
		t.Fatal("an idle session with no buff claims work - the fixture is not the case " +
			"this test is about")
	}
	addBuff(t, h, p, "Revenge", 1, 0, 0)
	if !h.sessionClockAwake(p.sess) {
		t.Fatal("a session holding a 30 s buff says it has nothing to do - its read would " +
			"block on the socket and the buff would never expire")
	}
	// AND THE TICK'S OWN GUARD AGREES: with nothing but a buff, the tick must RUN.
	p.sess.lastTick = time.Time{}
	if _, ran := h.tickIfDue(p.sess); !ran {
		t.Fatal("tickIfDue refused to run for a session whose only work is a buff - " +
			"sessionClockAwake would wake the read and the tick would immediately stop")
	}
}

// TestAUsedItemRunsItsOwnBuffScript is the mechanism behind the owner's screen: the
// item's OWN script (`SCP_USE_ITEM_AddBuff`, ge\script\item\item.scp:1567) calls
// `AddBuffImm(self, BuffName, arg1, arg2)`, which is bound - so a real potion's
// BuffName lands on the list and puts 3556 + 3125 on the wire.
//
// The script-level question the deleted stub answered 'NO' to is asserted here too:
// `IsBuffApplied` through the same world the builtins use.
func TestAUsedItemRunsItsOwnBuffScript(t *testing.T) {
	h, p, logs := buffZone(t)
	c := p.sess.Team[0]

	w := &buffWorld{h: h, sess: p.sess, inScript: true}
	ran, err := h.BuffScript.RunItem("SCP_USE_ITEM_AddBuff", c, "Revenge", 1, 0, w)
	if err != nil {
		t.Fatalf("the item's own script raised: %v", err)
	}
	if !ran {
		t.Fatal("SCP_USE_ITEM_AddBuff is not defined by the corpus - the item path would be " +
			"dead code")
	}
	if len(w.out) != 2 {
		t.Fatalf("the item's script produced %d packet(s), want 3556 + 3125", len(w.out))
	}
	if got := binary.LittleEndian.Uint16(w.out[0][0:]); got != CmdZCChangeBuff {
		t.Fatalf("the first packet is cmd %d, want %d", got, CmdZCChangeBuff)
	}
	// THE SCRIPT-LEVEL ANSWER, where script\status.go:189 used to say NO.
	if !w.IsBuffApplied(c.Handle, "Revenge") {
		t.Fatal("IsBuffApplied(self,'Revenge') answers NO right after the buff landed")
	}
	if lv := w.AppliedBuffLevel(c.Handle, "Revenge"); lv != 1 {
		t.Fatalf("GetAppliedBuffLevel is %g, want 1 (the arg1 the buff was applied with)", lv)
	}
	if lv := w.AppliedBuffLevel(c.Handle, "Clique"); lv != -1 {
		t.Fatalf("an unapplied buff's level is %g, want -1 (CBuffList::GetAppliedBuffLevel)", lv)
	}
	if !strings.Contains(logs.String(), "BUFF ADD") {
		t.Fatalf("no BUFF ADD line:\n%s", logs.String())
	}
}

// TestTeamAddBuffLandsOnEveryCharacterOfTheSession runs the OTHER item script:
// `SCP_USE_ITEM_TeamAddBuff` (339 item classes) -> `AddBuffToTeam`, which the
// reference routes through the commander's characters (`PlayerCharFunc.cpp:5949` ->
// `AddBuffAllPC`); here that is the session's own team.
//
// It uses Meditation, the buff class 20002 (Nutrition) grants, so the numbers are the
// game's own: its ENTER writes `buff.ApplyTime = 300000 + skilllv*30000 + AddAppTime
// + arg2` (ge\script\buff\buff.scp), i.e. 450,000 ms at skilllv 5 - and it ASKS
// `IsBuffApplied(self,'Deathfire')` on the way, which is why the world is installed
// for an ENTER run.
func TestTeamAddBuffLandsOnEveryCharacterOfTheSession(t *testing.T) {
	h, _, _ := buffZone(t)
	p := newTeam([]uint32{1792, 1793}, [][3]int32{{0, 0, 0}, {10, 0, 0}})

	w := &buffWorld{h: h, sess: p.sess, inScript: true}
	ran, err := h.BuffScript.RunItem("SCP_USE_ITEM_TeamAddBuff", p.sess.Team[0], "Meditation", 5, 0, w)
	if err != nil || !ran {
		t.Fatalf("the team script did not run (ran=%v, err=%v)", ran, err)
	}
	if len(w.out) != 4 {
		t.Fatalf("a two-character team add produced %d packet(s), want 4 (3556 + 3125 each)",
			len(w.out))
	}
	for i := range p.sess.Team {
		l := p.sess.liveBuffs(i)
		if !l.IsBuffApplied("Meditation") {
			t.Fatalf("character %d (%d) did not get the team buff: %v",
				i, p.sess.Team[i].Handle, l.Names())
		}
		if got := l.buffs[0].ApplyTime; got != 450000 {
			t.Fatalf("character %d's Meditation is %g ms, want 450000 (300000 + 5*30000)", i, got)
		}
	}
	// Each character's own 3556 carries ITS handle and ITS bit; the bit is the same
	// buff, which is the point of a team buff.
	for i := 0; i < 2; i++ {
		pkt := w.out[i*2]
		if got := binary.LittleEndian.Uint32(pkt[2:]); got != p.sess.Team[i].Handle {
			t.Fatalf("packet %d carries handle %d, want %d", i*2, got, p.sess.Team[i].Handle)
		}
		if b := pkt[6+(56>>3)]; b&(1<<(56&7)) == 0 {
			t.Fatalf("packet %d does not carry Meditation's bit (ClassID 56, mask byte 7 bit 0)",
				i*2)
		}
	}
}

// countMaskBits counts the set bits - the whole bar, so a packet that sets one bit
// and clears another cannot pass for "one buff".
func countMaskBits(mask []byte) int {
	n := 0
	for _, b := range mask {
		for b != 0 {
			n += int(b & 1)
			b >>= 1
		}
	}
	return n
}
