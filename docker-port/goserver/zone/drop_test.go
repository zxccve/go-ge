package zone

// THE DROP ROLL (R98 slice 3): WHAT MUST HOLD.
//
// THE SPEC IS `history\monster-death.md` §4 and `MonsterDropItem.cpp`. The tests are
// the task's own pass conditions, one per claim:
//
//	a Drop1Per of 0 never drops; 1 always drops
//	a scripted sequence of rolls reproduces a known item AND quantity
//	a kill with every frequency 0 logs "nothing" and sends nothing
//	Quant splits stacks the way :415-421 says (unstackable = N pieces of 1)
//
// plus the 3536 side: the frame is 57 bytes with every field at its own offset, a
// watcher inside the ring receives it and one past it does not, and the item id the
// frame carries is a row of the CLIENT's own Item table.
//
// NOTHING SLEEPS AND NOTHING IS SEEDED GLOBALLY. The roll reads Handler.DropRand, so
// a scripted sequence is the whole of the randomness in a test; production leaves it
// nil and gets the standard library. The kill itself goes through the real ledger
// (damageMonsterBy -> takeHP), and the roll is taken on the corpse's ZC_DEAD pass
// (zone/respawn.go deathPacketTick, which killOnce drives), so the m_isItemDropped guard
// is exercised too.

import (
	"encoding/binary"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedDropRand answers the scripted sequence in order and fails the test if the
// roll asks for more values than the script holds - a silent default would turn a
// missing roll into a passing test.
type scriptedDropRand struct {
	t    *testing.T
	vals []int
	at   int
}

func (s *scriptedDropRand) Intn(n int) int {
	s.t.Helper()
	if s.at >= len(s.vals) {
		s.t.Fatalf("the drop roll asked for a 1-in-%d draw but the script is "+
			"exhausted (%d value(s) used)", n, len(s.vals))
	}
	v := s.vals[s.at]
	s.at++
	if v < 0 || v >= n {
		s.t.Fatalf("scripted roll %d is not in [0,%d)", v, n)
	}
	return v
}

// dropItem builds one fixture item row the way the loader would.
func dropItem(id uint32, name, rule string, lv int, unstack bool) ItemRow {
	return ItemRow{ID: id, Name: name, Lv: lv, DropRule: rule,
		Unstack: unstack, OptionGroup: "None"}
}

// newItemTable builds an ItemTable by hand: the loader has its own test, and a drop
// rule test must not depend on 75 MB of client XML being on this machine.
func newItemTable(rows ...ItemRow) *ItemTable {
	t := &ItemTable{
		byID:   make(map[uint32]ItemRow, len(rows)),
		byName: make(map[string]uint32, len(rows)),
		byRule: make(map[string][]uint32, 4),
	}
	for _, r := range rows {
		t.byID[r.ID] = r
		if r.Name != "" {
			t.byName[strings.ToLower(r.Name)] = r.ID
		}
		if r.DropRule != "" {
			t.byRule[r.DropRule] = append(t.byRule[r.DropRule], r.ID)
		}
	}
	return t
}

// flatPenalty is a one-row penalty table: every gap in [-5,5] is multiplied by p.
// The reference's own shipped row for a level-10 killer says 0.8 there
// (datatable_droppenalty.xml), so both values below are states that exist.
func flatPenalty(p float64) *DropPenaltyTable {
	return &DropPenaltyTable{rows: map[int][6]dropPenaltyRange{
		10: {{Min: -5, Max: 5, Penalty: p}},
	}}
}

// logBuf collects the zone's lines without a file. (A plain strings.Builder is not
// safe for concurrent writes, and a kill can be armed on a session's goroutine; the
// mutex keeps `go test -race` honest.)
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// dropZone is the fixture: one monster at handle 565826 (the captured Scarecrow's
// id), the kill's killer at level 10, and TWO watchers - one inside the sight ring
// and one 6,000 units out. h.MapID is the Lakeside map the fixture's map row names.
func dropZone(t *testing.T, row MonsterRow, items *ItemTable, rand DropRand) (*Handler, *logBuf, *player, *player) {
	t.Helper()
	logs := &logBuf{}
	h := &Handler{
		Log:         log.New(logs, "", 0),
		MapID:       1331,
		Monsters:    newMonsters(map[uint32]MonsterRow{row.ID: row}),
		Items:       items,
		MapDrops:    &MapDropTable{rows: map[int]MapDropRow{}},
		DropPenalty: flatPenalty(1),
		DropRand:    rand,
	}
	// One watcher inside the ring and one past it. The monster stands at (0,0).
	killer := newPlayer(1792, "Killer", [3]int32{0, 0, 1})
	h.Players.Add(killer.sess)
	watcher := newPlayer(0x00A11CE, "Watcher", [3]int32{100, 100, 1})
	h.Players.Add(watcher.sess)
	return h, logs, killer, watcher
}

// deathDrain drives the death-packet pass past every corpse the fixture has armed, so the
// work a death owes at that instant is done: the roll and the exp happen there now
// (zone/respawn.go deathPacketTick), not inside the hit that killed the body.
func deathDrain(t *testing.T, h *Handler) {
	t.Helper()
	h.deathPacketTick(time.Now().Add(time.Hour))
}

// killOnce takes the fixture monster's last HP, arms its corpse and drives the ZC_DEAD
// instant, then returns what every socket got.
//
// THE BODY GOES INTO THE ROSTER FIRST. These fixtures hand-build the monster instead of
// spawning it, and a body the zone did not create gets no corpse (armCorpseAt,
// zone/respawn.go) - which since 2026-11-09 also means no death packet and no roll.
func killOnce(t *testing.T, h *Handler, m Monster, killer uint32) {
	t.Helper()
	h.spawnMu.Lock()
	if h.monsterIndexLocked(m.Handle) < 0 {
		h.spawnedMonster = append(h.spawnedMonster, m)
	}
	if h.monsterHP == nil {
		h.monsterHP = map[uint32]float64{}
	}
	h.monsterHP[m.Handle] = 1
	h.spawnMu.Unlock()
	if _, after, ok := h.damageMonsterBy(m, 1, killer, 10); !ok || after != 0 {
		t.Fatalf("the fixture kill did not empty the body: ok=%v after=%v", ok, after)
	}
	deathDrain(t, h)
}

// itemFrames returns every 3536 among a socket's packets.
func itemFrames(pkts [][]byte) [][]byte {
	var out [][]byte
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCSpawnStandingItem {
			out = append(out, p)
		}
	}
	return out
}

// trophyRow is a monster row whose ONLY rule is one trophy slot, so a scripted roll
// lands on a number a test can name.
func trophyRow(per float64, quant int) MonsterRow {
	return MonsterRow{
		ID: 11600, Name: "Monster_Hersu01", Level: 10,
		Trophies: [6]DropTrophy{{ID: 30000, Per: per, Quant: quant}},
	}
}

// TestDropOneInZeroNeverDrops is the first pass condition: a slot whose Per is 0 is
// never even rolled, so the kill logs "nothing" and no 3536 reaches anyone.
func TestDropOneInZeroNeverDrops(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, trophyRow(0, 5), items, nil)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "nothing (8 rules, penalty") {
		t.Fatalf("a Per of 0 must log \"nothing\", got:\n%s", text)
	}
	if got := len(itemFrames(killer.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) reached the killer for a Per-0 slot, want 0", got)
	}
	if got := len(itemFrames(watcher.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) reached the watcher for a Per-0 slot, want 0", got)
	}
}

// TestDropOneInOneAlwaysDrops is the second: Per 1 takes NO draw (the reference's
// `if (1 < dropItemFreq)`), creates the item, and a 57-byte 3536 carries it.
func TestDropOneInOneAlwaysDrops(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	// No scripted values: Per 1 and Quant 1 must not draw at all, and the scripted
	// source fails the test if they do.
	h, logs, killer, watcher := dropZone(t, trophyRow(1, 1), items, &scriptedDropRand{t: t})

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "30000 x1 (Drop1, 1 in 1, roll 1)") {
		t.Fatalf("the kill line does not name the 1-in-1 drop:\n%s", text)
	}
	frames := itemFrames(watcher.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("%d 3536(s) reached the watcher, want exactly 1:\n%s", len(frames), text)
	}
	pkt := frames[0]
	if len(pkt) != SpawnStandingItemSize {
		t.Fatalf("the frame is %d bytes, want %d", len(pkt), SpawnStandingItemSize)
	}
	if got := binary.LittleEndian.Uint32(pkt[ssiAppType:]); got != 30000 {
		t.Fatalf("the frame's appearance.Type is %d, want 30000", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[ssiAppCount:]); got != 1 {
		t.Fatalf("the frame's appearance.Count is %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[ssiAppParent:]); got != 565826 {
		t.Fatalf("the frame's appearance.parent is %d, want the corpse handle 565826", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[ssiHandle:]); got != ItemHandle(1) {
		t.Fatalf("the frame's handle is 0x%X, want the first item handle 0x%X",
			got, ItemHandle(1))
	}
}

// TestDropScriptedSequenceReproducesItemAndQuantity is the third: Per 4 takes ONE
// draw and Quant 3 takes ONE, and the scripted values name the outcome exactly. The
// frequency draw must be 1-based roll 1 (the reference's `RandomInt(1, N) == 1` is
// the ONLY passing value), and the quantity draw 2 is 3 of 3.
func TestDropScriptedSequenceReproducesItemAndQuantity(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	rng := &scriptedDropRand{t: t, vals: []int{0, 2}} // 1-in-4 passes on roll 1; qty 3
	h, logs, killer, watcher := dropZone(t, trophyRow(4, 3), items, rng)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "30000 x3 (Drop1, 1 in 4, roll 1)") {
		t.Fatalf("the scripted roll did not reproduce item 30000 x3 at 1 in 4 / roll 2:\n%s", text)
	}
	if rng.at != 2 {
		t.Fatalf("the roll used %d scripted values, want exactly 2 (frequency + count)", rng.at)
	}
	frames := itemFrames(watcher.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("%d 3536(s) reached the watcher, want exactly 1", len(frames))
	}
	if got := binary.LittleEndian.Uint32(frames[0][ssiAppCount:]); got != 3 {
		t.Fatalf("the frame carries Count %d, want 3", got)
	}
}

// TestDropFailingRollSendsNothing is the other half of the same rule: a draw that is
// not 1 is a miss, and the line says nothing dropped.
func TestDropFailingRollSendsNothing(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	rng := &scriptedDropRand{t: t, vals: []int{2}} // roll 3 of 3: miss
	h, logs, killer, watcher := dropZone(t, trophyRow(3, 1), items, rng)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	if text := logs.String(); !strings.Contains(text, "nothing (8 rules, penalty") {
		t.Fatalf("a missed roll must log \"nothing\":\n%s", text)
	}
	if got := len(itemFrames(watcher.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) reached the watcher after a miss, want 0", got)
	}
}

// TestDropAllFrequenciesZeroLogsNothingAndSendsNothing is the fourth: a row with no
// rule armed at all still writes its ONE line and sends not one byte.
func TestDropAllFrequenciesZeroLogsNothingAndSendsNothing(t *testing.T) {
	row := MonsterRow{ID: 11600, Name: "Monster_Hersu01", Level: 10}
	h, logs, killer, watcher := dropZone(t, row, newItemTable(), nil)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "nothing (8 rules, penalty") {
		t.Fatalf("the all-zero row must log \"nothing (8 rules, penalty ...)\":\n%s", text)
	}
	if got := len(itemFrames(killer.conn.sent())) + len(itemFrames(watcher.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) went out for an all-zero row, want 0", got)
	}
	// And the roll cannot be run twice for the same body.
	before := strings.Count(text, "zone: DROP handle")
	if _, after, ok := h.damageMonsterBy(scarecrow(), 1, killer.sess.Team[0].Handle, 10); !ok || after != 0 {
		t.Fatalf("the second hit did not empty the body again: ok=%v after=%v", ok, after)
	}
	// The second corpse's own pass runs here: without it the guard below would be proved
	// by a roll that never happened.
	deathDrain(t, h)
	if after := strings.Count(logs.String(), "zone: DROP handle"); after != before {
		t.Fatalf("a second hit on the same handle produced another DROP line (%d -> %d): "+
			"m_isItemDropped's guard is not working", before, after)
	}
}

// TestDropQuantSplitsStacks is the fifth: the reference's :415-421 split. An
// UNSTACKABLE item becomes `count` field items of 1; a stackable one becomes ONE
// field item of `count`.
func TestDropQuantSplitsStacks(t *testing.T) {
	t.Run("unstackable", func(t *testing.T) {
		items := newItemTable(dropItem(30000, "Vis", "", 0, true))
		rng := &scriptedDropRand{t: t, vals: []int{2}} // quantity 3 (0-based 2)
		h, logs, killer, watcher := dropZone(t, trophyRow(1, 3), items, rng)

		killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

		frames := itemFrames(watcher.conn.sent())
		if len(frames) != 3 {
			t.Fatalf("%d frames for an unstackable x3, want 3:\n%s", len(frames), logs.String())
		}
		for i, p := range frames {
			if got := binary.LittleEndian.Uint32(p[ssiAppCount:]); got != 1 {
				t.Fatalf("frame %d carries Count %d, want 1 (each piece is its own item)", i, got)
			}
		}
		if text := logs.String(); !strings.Contains(text, "30000 x3 (Drop1, 1 in 1, roll 1) as 3 x1") {
			t.Fatalf("the kill line does not say the 3 units were split into 3 pieces:\n%s", text)
		}
	})

	t.Run("stackable", func(t *testing.T) {
		items := newItemTable(dropItem(30000, "Vis", "", 0, false))
		rng := &scriptedDropRand{t: t, vals: []int{2}} // quantity 3
		h, logs, killer, watcher := dropZone(t, trophyRow(1, 3), items, rng)

		killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

		frames := itemFrames(watcher.conn.sent())
		if len(frames) != 1 {
			t.Fatalf("%d frames for a stackable x3, want 1:\n%s", len(frames), logs.String())
		}
		if got := binary.LittleEndian.Uint32(frames[0][ssiAppCount:]); got != 3 {
			t.Fatalf("the stacked frame carries Count %d, want 3", got)
		}
	})
}

// TestDropPenaltyScalesTheFrequency: the penalty multiplies the frequency BEFORE it
// is rounded (MonsterDropItem.cpp:228-262). A Per of 3 under the shipped 0.8 becomes
// round(2.4) = a 1-in-2, which the log states and the roll draws against.
func TestDropPenaltyScalesTheFrequency(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	rng := &scriptedDropRand{t: t, vals: []int{0}} // roll 1 of 2: pass
	h, logs, killer, watcher := dropZone(t, trophyRow(3, 1), items, rng)
	h.DropPenalty = flatPenalty(0.8)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "30000 x1 (Drop1, 1 in 2, roll 1)") {
		t.Fatalf("Per 3 x penalty 0.8 should become a 1-in-2, got:\n%s", text)
	}
	if !strings.Contains(text, "penalty 0.80") {
		t.Fatalf("the kill line does not state the penalty it used:\n%s", text)
	}
	if got := len(itemFrames(watcher.conn.sent())); got != 1 {
		t.Fatalf("%d 3536(s) reached the watcher, want 1", got)
	}
}

// TestSpawnStandingItemFrame pins the 57-byte frame field by field. The size is the
// client's own registration (data\cmd_table.txt:1013); the offsets are
// geZCPacket.h:1161-1167 inside `#pragma pack(1)`.
//
// THE OFFSETS BELOW ARE LITERALS FROM THE HEADER, NOT THE PACKAGE'S OWN CONSTANTS.
// Reading them back through ssiHandle & co. would only prove that drop.go agrees with
// itself; the numbers are re-derived here from the struct and checked against the
// constants as a SEPARATE assertion, so a constant that drifts fails this test instead
// of moving with it:
//
//	PKS_DEFAULT.command     0   2   WORD
//	handle                  2   4   OBJHANDLE
//	pos (WORLDPOS)          6  12   3 x long (x, y, cell) - geShared.h:712
//	appearance             18  18   APPEARANCE_ITEM - geZCTypes.h:289-296
//	  Type                 18   4   DWORD
//	  Count                22   4   DWORD
//	  parent               26   4   OBJHANDLE
//	  isOptionItem         30   1   BYTE
//	  socketCount          31   1   BYTE
//	  reinforceCount       32   4   int
//	commanderName          36  17   COMMANDER_NAME_LEN = 16 + 1
//	squadID                53   4   DWORD
//	                           57
func TestSpawnStandingItemFrame(t *testing.T) {
	// The named constants must BE the header's offsets; a rename or a re-layout that
	// moves one without the other is the failure this catches.
	named := []struct {
		name string
		got  int
		want int
	}{
		{"ssiHandle", ssiHandle, 2},
		{"ssiPosX", ssiPosX, 6},
		{"ssiPosY", ssiPosY, 10},
		{"ssiPosCell", ssiPosCell, 14},
		{"ssiAppType", ssiAppType, 18},
		{"ssiAppCount", ssiAppCount, 22},
		{"ssiAppParent", ssiAppParent, 26},
		{"ssiAppIsOption", ssiAppIsOption, 30},
		{"ssiAppSockets", ssiAppSockets, 31},
		{"ssiAppReinforce", ssiAppReinforce, 32},
		{"ssiCommander", ssiCommander, 36},
		{"ssiCommanderLen", ssiCommanderLen, 17},
		{"ssiSquadID", ssiSquadID, 53},
	}
	for _, c := range named {
		if c.got != c.want {
			t.Errorf("%s is %d, the header puts it at %d", c.name, c.got, c.want)
		}
	}

	pkt := BuildSpawnStandingItem(0x3000002A, [3]int32{-919, 2311, 136},
		ItemAppearance{Type: 30000, Count: 2, Parent: 565826}, "Commander", 7)
	if len(pkt) != SpawnStandingItemSize {
		t.Fatalf("the frame is %d bytes, want %d", len(pkt), SpawnStandingItemSize)
	}
	if len(pkt) != 57 {
		t.Fatalf("the CLIENT table says 57 bytes; the builder made %d", len(pkt))
	}
	// The last field must END at the registration: squadID 53 + 4 = 57.
	if ssiSquadID+4 != len(pkt) {
		t.Fatalf("squadID ends at %d but the frame is %d bytes - a field is unplaced "+
			"or the frame is padded", ssiSquadID+4, len(pkt))
	}
	checks := []struct {
		name string
		off  int
		want uint32
	}{
		{"command", 0, CmdZCSpawnStandingItem},
		{"handle", 2, 0x3000002A},
		{"pos.y", 10, 2311},
		{"pos.cell", 14, 136},
		{"appearance.Type", 18, 30000},
		{"appearance.Count", 22, 2},
		{"appearance.parent", 26, 565826},
		{"squadID", 53, 7},
	}
	for _, c := range checks {
		var got uint32
		if c.off == 0 {
			got = uint32(binary.LittleEndian.Uint16(pkt[0:]))
		} else {
			got = binary.LittleEndian.Uint32(pkt[c.off:])
		}
		if got != c.want {
			t.Errorf("%s at +%d is %d (0x%X), want %d", c.name, c.off, got, got, c.want)
		}
	}
	if pkt[30] != 0 || pkt[31] != 0 {
		t.Errorf("isOptionItem/socketCount are %d/%d, want 0/0", pkt[30], pkt[31])
	}
	// x is SIGNED: -919 must survive as two's complement, not as a rejected value.
	if got := int32(binary.LittleEndian.Uint32(pkt[6:])); got != -919 {
		t.Errorf("pos.x at +6 is %d, want -919", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[32:]); got != 0 {
		t.Errorf("reinforceCount is %d, want 0", got)
	}
	if got := string(pkt[36 : 36+17]); got != "Commander\x00\x00\x00\x00\x00\x00\x00\x00" {
		t.Errorf("commanderName is %q", got)
	}
}

// TestSpawnStandingItemFansOutByTheSightRing: a watcher inside ViewRange receives
// the 3536; one past it does not. The radius is the map row's own (zone\sight.go),
// installed here through the same helper the ring's tests use.
func TestSpawnStandingItemFansOutByTheSightRing(t *testing.T) {
	sightUseTable(t, map[int]SightRow{1331: {ViewRange: 5000}})

	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, trophyRow(1, 1), items, nil)

	far := newPlayer(0x00FA4, "Far", [3]int32{6000, 0, 1})
	h.Players.Add(far.sess)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	if got := len(itemFrames(watcher.conn.sent())); got != 1 {
		t.Fatalf("%d 3536(s) reached the in-view watcher, want 1", got)
	}
	if got := len(itemFrames(far.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) reached a watcher 6,000 units out with a 5,000 ring, want 0", got)
	}
	if got := len(itemFrames(killer.conn.sent())); got != 1 {
		t.Fatalf("%d 3536(s) reached the killer, who stands on the corpse, want 1", got)
	}
	if !strings.Contains(logs.String(), "cmd 3536 (57 B) x1 sent 2 to 3 player(s)") {
		t.Fatalf("the kill line does not record the fan-out (2 of 3):\n%s", logs.String())
	}
}

// TestDropBasicRulePicksFromTheLevelWindow: the basic rule picks UNIFORMLY from the
// rule's own level window (`GetItemsByDropRule`, ItemTable.cpp:1350-1381). Level 40
// makes OldEqp ask for [6,11]; the three fixture items sit at 6, 7 and 8, and the
// scripted pick 1 is the SECOND one (Lv 7, id 40002).
func TestDropBasicRulePicksFromTheLevelWindow(t *testing.T) {
	items := newItemTable(
		dropItem(40001, "OldA", "OldEqp", 6, false),
		dropItem(40002, "OldB", "OldEqp", 7, false),
		dropItem(40003, "OldC", "OldEqp", 8, false),
	)
	row := MonsterRow{ID: 11600, Name: "Monster_Hersu01", Level: 40, DropOldEqpFreq: 2}
	rng := &scriptedDropRand{t: t, vals: []int{0, 1}} // 1-in-2 passes; pick index 1
	h, logs, killer, _ := dropZone(t, row, items, rng)

	killOnce(t, h, monsterAtLevel(40), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "40002 x1 (OldEqp, 1 in 2, roll 1)") {
		t.Fatalf("the window pick did not land on 40002:\n%s", text)
	}
}

// TestDropZoneRule: the MAP row's DropItem1..7 fire on their own, independent of the
// monster's basic/trophy columns (:307-317).
func TestDropZoneRule(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, MonsterRow{ID: 11600, Level: 10}, items, nil)
	h.MapDrops = &MapDropTable{rows: map[int]MapDropRow{
		1331: {Item: [7]uint32{30000}, Freq: [7]float64{1}},
	}}

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "30000 x1 (Zone1, 1 in 1, roll 1)") {
		t.Fatalf("the zone rule did not drop:\n%s", text)
	}
	if got := len(itemFrames(watcher.conn.sent())); got != 1 {
		t.Fatalf("%d 3536(s) reached the watcher, want 1", got)
	}
}

// TestDropMapBlockSuppressesBasicAndUnique: `MonsterDropBlock="TRUE"` (the shipped
// Lakeside row, map 1331, is one) stops the basic and trophy rules - and only those.
// The line says why, because otherwise a blocked map looks like a broken table.
func TestDropMapBlockSuppressesBasicAndUnique(t *testing.T) {
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, trophyRow(1, 1), items, nil)
	h.MapDrops = &MapDropTable{rows: map[int]MapDropRow{1331: {Blocked: true}}}

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "nothing (8 rules, penalty") ||
		!strings.Contains(text, "map blocks monster drops") {
		t.Fatalf("a blocked map must log nothing AND say the map blocked it:\n%s", text)
	}
	if got := len(itemFrames(watcher.conn.sent())); got != 0 {
		t.Fatalf("%d 3536(s) went out on a blocked map, want 0", got)
	}
}

// TestDroppedItemIsOnTheClientsOwnTable is the last pass condition: the id a 3536
// carries must be a row of the CLIENT's Item idspace, not a fixture's invention.
// It reads the real files (the merged Item table is 25k rows across 26 XML files),
// and skips only on a checkout that has no client data at all.
func TestDroppedItemIsOnTheClientsOwnTable(t *testing.T) {
	dir := XMLDir()
	items, err := LoadItemTables(dir)
	if err != nil {
		t.Skipf("no client Item table at %s (%v) - this machine has no ge\\xml", dir, err)
	}
	if items.Len() < 1000 {
		t.Fatalf("the merged Item table holds only %d rows - the loader is reading one "+
			"file, not the whole idspace", items.Len())
	}
	// 30000 is the Vis row the tests above drop; it is the CLIENT's own class id.
	row, ok := items.Row(30000)
	if !ok {
		t.Fatalf("item 30000 (Vis) is not in the client's Item table")
	}
	// And a frame built for it resolves back through the same table.
	pkt := BuildSpawnStandingItem(ItemHandle(1), [3]int32{0, 0, 1},
		ItemAppearance{Type: row.ID, Count: 1}, "", 0)
	if _, ok := items.Row(binary.LittleEndian.Uint32(pkt[ssiAppType:])); !ok {
		t.Fatalf("the id the frame carries does not resolve on the client's table")
	}
}

// TestDropRollIsClaimedOncePerBody is pass condition (e): `m_isItemDropped` is a
// guard on the BODY, not on the call site. A second kill of the same handle - the
// body's HP refilled, its ZC_DEAD already owed and drained - must roll NOTHING.
func TestDropRollIsClaimedOncePerBody(t *testing.T) {
	// The guard on its own: first claim wins, every later one is refused, and a
	// different handle is unaffected.
	g := &Handler{}
	if !g.claimDropRoll(565826) {
		t.Fatal("the first claim for a handle was refused - nothing could ever drop")
	}
	if g.claimDropRoll(565826) {
		t.Fatal("a second claim for the SAME handle was granted: the corpse can drop twice")
	}
	if !g.claimDropRoll(565827) {
		t.Fatal("a claim for a different handle was refused: the guard is not per body")
	}

	// And through the real kill path, with the ledger deliberately made able to arm
	// a second death: drain the waiting ZC_DEAD, then refill the HP and hit it again.
	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, watcher := dropZone(t, trophyRow(1, 1), items, nil)
	m := monsterAtLevel(10)
	killerHandle := killer.sess.Team[0].Handle

	killOnce(t, h, m, killerHandle)
	if n := strings.Count(logs.String(), "zone: DROP handle"); n != 1 {
		t.Fatalf("%d DROP line(s) after one kill, want 1:\n%s", n, logs.String())
	}

	h.spawnMu.Lock()
	h.monsterHP[m.Handle] = 1 // refill the body: a new kill can be armed
	h.spawnMu.Unlock()
	if _, after, ok := h.damageMonsterBy(m, 1, killerHandle, 10); !ok || after != 0 {
		t.Fatalf("the second kill did not empty the refilled body: ok=%v after=%v", ok, after)
	}
	// The refilled body's death owes its own pass - the claim is refused THERE.
	deathDrain(t, h)

	if n := strings.Count(logs.String(), "zone: DROP handle"); n != 1 {
		t.Fatalf("%d DROP line(s) after killing the same handle twice, want 1 - "+
			"claimDropRoll let the corpse roll twice:\n%s", n, logs.String())
	}
	if n := len(itemFrames(watcher.conn.sent())); n != 1 {
		t.Fatalf("%d 3536(s) reached the watcher after two kills of one body, want 1", n)
	}
}

// TestDropUncertainAppearanceWithholdsThe3536 is pass condition (g): a drop whose
// APPEARANCE_ITEM cannot be filled from the item row still HAPPENS and is still
// logged, but NO 3536 is built - and the line names the field and what would answer
// it. The three cases are the three fields that are not always zero.
func TestDropUncertainAppearanceWithholdsThe3536(t *testing.T) {
	cases := []struct {
		name string
		item ItemRow
		why  string // the exact reason the kill line must carry
	}{
		{
			name: "unique item always gets an option blob",
			item: ItemRow{ID: 30000, Name: "Vis", OptionGroup: "None", Option1ID: 99001},
			why:  "isOptionItem: a unique item always gets a generated option blob",
		},
		{
			name: "option group can take an option",
			item: ItemRow{ID: 30000, Name: "Vis", OptionGroup: "ItemOpt_Rare_Weapon"},
			why:  "isOptionItem: OptionGroup ItemOpt_Rare_Weapon can take a random option",
		},
		{
			name: "rare-drop roll not implemented",
			item: ItemRow{ID: 30000, Name: "Vis", OptionGroup: "None", DropRareFreq: 50},
			why:  "socketCount/reinforceCount: DropRareFreq is non-zero",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, logs, killer, watcher := dropZone(t, trophyRow(1, 1),
				newItemTable(c.item), nil)

			killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

			text := logs.String()
			// The DROP happened: the roll names the item and its rule.
			if !strings.Contains(text, "30000 x1 (Drop1, 1 in 1, roll 1)") {
				t.Fatalf("the roll did not create the item at all:\n%s", text)
			}
			// The packet did NOT.
			if !strings.Contains(text, "[no 3536: "+c.why) {
				t.Fatalf("the kill line does not name the missing field %q:\n%s", c.why, text)
			}
			if n := len(itemFrames(killer.conn.sent())); n != 0 {
				t.Fatalf("%d 3536(s) reached the killer for an uncertain appearance, want 0", n)
			}
			if n := len(itemFrames(watcher.conn.sent())); n != 0 {
				t.Fatalf("%d 3536(s) reached the watcher for an uncertain appearance, want 0", n)
			}
		})
	}
}

// TestSpawnStandingItemCulledInsideViewRange is pass condition (h), the part the
// outer ring does not cover: a viewer INSIDE ViewRange but PAST ViewMiddleRange is
// frozen, not gone (zone\sight.go's sightCulls, View.cpp:269), so the 3536 that adds
// the field item to its world is not sent. Map 1331's own row is 6000 / 4000.
func TestSpawnStandingItemCulledInsideViewRange(t *testing.T) {
	sightUseTable(t, map[int]SightRow{1331: {ViewRange: 6000, ViewMiddleRange: 4000}})

	items := newItemTable(dropItem(30000, "Vis", "", 0, false))
	h, logs, killer, _ := dropZone(t, trophyRow(1, 1), items, nil)
	// THE CORPSE IS AT (-919, 2311), NOT (0,0) - the fixture monster's own spawn point,
	// so a viewer's distance must be measured from THERE. Placing these at x=5000
	// from the origin would put them 6,354 out and this test would pass on the OUTER
	// ring, proving nothing about the cull.
	const mx, my = -919, 2311
	mid := newPlayer(0x00BEEF, "Mid", [3]int32{4000, my, 1}) // 4,919: in view, past middle
	h.Players.Add(mid.sess)
	far := newPlayer(0x00FA4, "Far", [3]int32{6000, my, 1}) // 6,919: past the outer ring
	h.Players.Add(far.sess)

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	if n := len(itemFrames(killer.conn.sent())); n != 1 {
		t.Fatalf("%d 3536(s) reached the killer, who stands on the corpse, want 1", n)
	}
	if n := len(itemFrames(mid.conn.sent())); n != 0 {
		t.Fatalf("%d 3536(s) reached a viewer 5,000 out - inside ViewRange 6000 but "+
			"past ViewMiddleRange 4000, which the R135e cull freezes:\n%s",
			n, logs.String())
	}
	if n := len(itemFrames(far.conn.sent())); n != 0 {
		t.Fatalf("%d 3536(s) reached a viewer 7,000 out with ViewRange 6000, want 0", n)
	}
	if !strings.Contains(logs.String(), "sent 2 to 4 player(s)") {
		t.Fatalf("the kill line does not record the fan-out (2 of 4):\n%s", logs.String())
	}
}

// TestDropTablesAreLoadedOnceAndSayWhichRulesAreIn pins the ONE log line the card
// asks for: the first kill states which of the five reference rules this build runs.
func TestDropTablesAreLoadedOnceAndSayWhichRulesAreIn(t *testing.T) {
	h, logs, killer, _ := dropZone(t, trophyRow(1, 1),
		newItemTable(dropItem(30000, "Vis", "", 0, false)), nil)
	h.MapDrops = &MapDropTable{rows: map[int]MapDropRow{1331: {}}}

	killOnce(t, h, monsterAtLevel(10), killer.sess.Team[0].Handle)

	text := logs.String()
	if !strings.Contains(text, "zone: DROP rules: basic + unique + zone only") {
		t.Fatalf("the first kill does not say which rules are in:\n%s", text)
	}
	if !strings.Contains(text, "item-group and special-group rules") {
		t.Fatalf("the first kill does not say which rules are OUT:\n%s", text)
	}
}

// TestTheKillLoadsNoDropTable is pass condition (a) of the death-timing fix (2026-11-07):
// the three disk readers behind dropTables are wrapped with counters, the tables are armed
// at start (what main.go does before the listener opens), and a REAL kill is then made.
// The count must be 1 of each BEFORE the kill and still 1 AFTER it - the first kill used to
// be the call that read ~75 MB of Item XML, which is the delay the owner felt.
//
// IT ALSO PINS THE START-UP LINE: "drop tables armed - <n> item row(s), <n> map row(s),
// <n> penalty level(s)". A silent start would be indistinguishable from a loaded one.
func TestTheKillLoadsNoDropTable(t *testing.T) {
	var itemLoads, mapLoads, penLoads int
	oldItems, oldMaps, oldPen := loadItemTablesFn, loadMapDropsFn, loadDropPenaltyFn
	loadItemTablesFn = func(dir string) (*ItemTable, error) {
		itemLoads++
		return newItemTable(dropItem(30000, "Vis", "", 0, false)), nil
	}
	loadMapDropsFn = func(dir string) (*MapDropTable, error) {
		mapLoads++
		return &MapDropTable{rows: map[int]MapDropRow{}}, nil
	}
	loadDropPenaltyFn = func() (*DropPenaltyTable, error) {
		penLoads++
		return flatPenalty(1), nil
	}
	defer func() {
		loadItemTablesFn, loadMapDropsFn, loadDropPenaltyFn = oldItems, oldMaps, oldPen
	}()

	// A handler with NO tables, which is the state a zone starts in.
	h, logs, killer, _ := dropZone(t, trophyRow(1, 1), nil, nil)
	h.MapDrops, h.DropPenalty = nil, nil
	if itemLoads+mapLoads+penLoads != 0 {
		t.Fatalf("a drop table was read before the zone started: %d/%d/%d",
			itemLoads, mapLoads, penLoads)
	}

	// THE START: one read of each, and the one line naming them.
	items, maps, penalty := h.ArmDropTables()
	if itemLoads != 1 || mapLoads != 1 || penLoads != 1 {
		t.Fatalf("ArmDropTables read %d item / %d map / %d penalty table(s), want 1 each",
			itemLoads, mapLoads, penLoads)
	}
	if items != 1 || maps != 0 || penalty != 1 {
		t.Fatalf("the armed tables hold %d item / %d map / %d penalty row(s), want 1/0/1",
			items, maps, penalty)
	}
	if want := "zone: drop tables armed - 1 item row(s), 0 map row(s), 1 penalty level(s)"; !strings.Contains(logs.String(), want) {
		t.Fatalf("the start-up line is missing or wrong; want %q in:\n%s", want, logs.String())
	}

	// THE KILLS. Two of them, because the FIRST is the one that used to pay.
	m := monsterAtLevel(10)
	killOnce(t, h, m, killer.sess.Team[0].Handle)
	killOnce(t, h, m, killer.sess.Team[0].Handle)
	if itemLoads != 1 || mapLoads != 1 || penLoads != 1 {
		t.Fatalf("a kill read the drop tables from disk (%d item / %d map / %d penalty "+
			"load(s) after two kills, want still 1 each): the first kill is paying for "+
			"the start-up load again\n%s", itemLoads, mapLoads, penLoads, logs.String())
	}
}
