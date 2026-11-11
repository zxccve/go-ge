package zone

// MONSTER DROPS: THE ROLL (R98 slice 3).
//
// THE SPEC IS `history\monster-death.md` §4 and `source2\geServer\ZServerLib\
// MonsterDropItem.cpp`. THE REFERENCE RUNS THE ROLL ONCE, from the death state's
// own ON_UPDATE, guarded by `m_isItemDropped` (MonsterNPC.cpp:2613-2616), and the
// guard is reset on entering BS_DEAD (:2604). This file hangs the roll off the same
// instant the kill is armed (zone\create.go, damageMonster -> takeHP) because that
// IS this zone's death state: the HP hits zero, DEAD/exp/drop are decided once, and
// the 3586 leaves with the killing hit itself (2026-11-07 - it used to wait for the
// next tick's 500 ms deadline, which is why the owner saw it 101 ms after one kill
// and 503 ms after another). claimDropRoll is the m_isItemDropped guard, keyed by
// the monster handle.
//
// THREE OF THE FIVE RULES ARE HERE:
//
//	basic   the row's eight Drop*Freq 1-in-N rolls; the item is picked uniformly
//	        from the Rule's level window (`GetItemRandom`, :344-372)
//	unique  the row's six DropN slots (`DropItemByUniqueRule`, :288-305)
//	zone    the MAP row's DropItem1..7 (`DropItemByZoneRule`, :307-317)
//
// THE ITEM-GROUP AND SPECIAL-GROUP RULES ARE NOT HERE, ON PURPOSE: they need
// `datatable_itemgroup.ies` decoded (a RANDOM_TABLE format this server has not read
// yet) and `datatable_specialitem_drop.ies`; guessing an item out of either would be
// an invented drop. They are a later slice, and the first kill prints that sentence
// once.
//
// THE FREQUENCY IS 1-IN-N, NEVER A PERCENT. `GetItemRandom` and `DropAssignedItem`
// round N to an int, divide it by the world's RewardItemBM and the server's
// ItemRatio, floor it at 1, and only then ask `RandomInt(1, N) == 1`
// (MonsterDropItem.cpp:344-358, :378-392). Neither divisor is configured anywhere
// in the trees this build has, so both are 1 and the row's number is N. A rounded N
// of 1 draws NOTHING and always drops, which is why an absent column (0) must be
// skipped BEFORE the arithmetic: `round(0 * penalty)` would otherwise become 1 and
// every monster would drop everything.
//
// THE DROP PENALTY IS APPLIED WHEN ITS TABLE IS READABLE. It is a SERVER-side export
// (`ge\server_xml\datatable_droppenalty.xml`; the client has no copy). The lookup is
// the reference's: row = the KILLER's level, range chosen by `killedLv - killerLv`
// (`DropPenaltyTable.cpp:111-121`), and the multiplier scales every frequency before
// rounding (:228-262). No table -> 1.0 and the kill line says so. The reference
// falls back to the monster's own level as the killer level when no killer pointer
// exists (:764-766), and so does this.
//
// FATIGUE IS NOT MODELLED: `DropItemByBasicRule` multiplies the penalty by the
// attacker's fatigue multiplier (:222-225) and a negative one cancels drops. This
// zone keeps no fatigue state, so the multiplier is 1.0 - stated here, not guessed
// per kill.

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// THE FIELD ITEM'S SPAWN PACKET: 3536, 57 bytes.
//
// `PKS_ZC_SPAWN_STANDINGITEM` is `PKS_DEFAULT` + handle + WORLDPOS + APPEARANCE_ITEM
// + commanderName[COMMANDER_NAME_LEN] + squadID (`source2\geShared\include\
// geZCPacket.h:1161-1167`), and every enclosing struct in that header is inside
// `#pragma pack(1)` (:948 .. :5166). The client's own table says 57
// (`docker-port\data\cmd_table.txt:1013`), and the fields add up exactly:
//
//	command          2   WORD
//	handle           4   OBJHANDLE (int)
//	pos             12   WORLDPOS = 3 x long (x, y, cell)
//	appearance      18   APPEARANCE_ITEM (packed: 4+4+4+1+1+4)
//	commanderName   17   COMMANDER_NAME_LEN = 16 + 1 (geShared.h:188)
//	squadID          4   DWORD
//	               ---
//	                57
//
// APPEARANCE_ITEM's six fields (`geZCTypes.h:289-296`) and where each one comes
// from for a monster drop (`CFieldItem::GetAppearance`, FieldItem.cpp:67-85):
//
//	Type            the dropped item's ClassID
//	Count           the field item's stack count
//	parent          the MONSTER's handle - `CMonsterDropItem::Init` takes the
//	                monster's handle and passes it as `parent` to
//	                CreateFieldItem (MonsterDropItem.cpp:74, :502-504)
//	isOptionItem    0 when the item can carry no random option (see
//	                itemAppearanceCertain), 1 otherwise
//	socketCount     the sockets `GetDropRareVal` rolled - 0 when DropRareFreq is 0
//	reinforceCount  the reinforcement `GetDropRareVal` rolled - 0 likewise
//
// IF ANY OF THOSE IS NOT CERTAIN, NO PACKET IS BUILT and the kill line names the
// field and the table that would answer it. A 57-byte frame with an invented
// appearance byte is the bug class this project calls "reads as correct".
const (
	// CmdZCSpawnStandingItem is PKS_ZC_SPAWN_STANDINGITEM, from the CLIENT's own
	// table (data\cmd_table.txt:1013), never from header arithmetic.
	CmdZCSpawnStandingItem = 3536
	// SpawnStandingItemSize is the registered size of the whole frame.
	SpawnStandingItemSize = 57

	ssiHandle       = 2  // OBJHANDLE
	ssiPosX         = 6  // long x
	ssiPosY         = 10 // long y
	ssiPosCell      = 14 // long cell
	ssiAppType      = 18 // DWORD Type
	ssiAppCount     = 22 // DWORD Count
	ssiAppParent    = 26 // OBJHANDLE parent
	ssiAppIsOption  = 30 // BYTE isOptionItem
	ssiAppSockets   = 31 // BYTE socketCount
	ssiAppReinforce = 32 // int reinforceCount
	ssiCommander    = 36 // char commanderName[17]
	ssiCommanderLen = 17
	ssiSquadID      = 53 // DWORD squadID
)

// ItemAppearance is APPEARANCE_ITEM, field for field, so a builder cannot silently
// forget one.
type ItemAppearance struct {
	Type           uint32
	Count          uint32
	Parent         uint32
	IsOptionItem   bool
	SocketCount    uint8
	ReinforceCount int32
}

// BuildSpawnStandingItem packs PKS_ZC_SPAWN_STANDINGITEM.
//
// commanderName is the reference's item-priority commander (`GetItemPriorityOwner`,
// MonsterDropItem.cpp:516-556): the highest-damage commander's name, or EMPTY when
// nobody has priority. This zone keeps no per-attacker damage list (the same reason
// zone\exp_award.go pays every living member whole), so it always passes empty and
// squadID 0 - the reference's own "no priority owner" state, not an invented name.
func BuildSpawnStandingItem(handle uint32, pos [3]int32, app ItemAppearance,
	commander string, squadID uint32) []byte {

	if len(commander) > ssiCommanderLen-1 {
		commander = commander[:ssiCommanderLen-1] // strncpy_s(_TRUNCATE)
	}
	pkt := make([]byte, SpawnStandingItemSize)
	binary.LittleEndian.PutUint16(pkt[0:], uint16(CmdZCSpawnStandingItem))
	binary.LittleEndian.PutUint32(pkt[ssiHandle:], handle)
	binary.LittleEndian.PutUint32(pkt[ssiPosX:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(pkt[ssiPosY:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(pkt[ssiPosCell:], uint32(pos[2]))
	binary.LittleEndian.PutUint32(pkt[ssiAppType:], app.Type)
	binary.LittleEndian.PutUint32(pkt[ssiAppCount:], app.Count)
	binary.LittleEndian.PutUint32(pkt[ssiAppParent:], app.Parent)
	if app.IsOptionItem {
		pkt[ssiAppIsOption] = 1
	}
	pkt[ssiAppSockets] = app.SocketCount
	binary.LittleEndian.PutUint32(pkt[ssiAppReinforce:], uint32(app.ReinforceCount))
	copy(pkt[ssiCommander:ssiCommander+ssiCommanderLen], commander)
	binary.LittleEndian.PutUint32(pkt[ssiSquadID:], squadID)
	return pkt
}

// DropRand is the ONE source of every drop roll in this zone.
//
// Intn(n) answers a number in [0, n) - the Go form of `RandomInt(1, n) - 1`. It is
// ONLY called with n > 1: a 1-in-1 rule draws nothing, exactly as the reference's
// `if (1 < dropItemFreq)` does. A test installs a scripted sequence through
// Handler.DropRand; nothing in this file calls rand.* directly except the default
// implementation below, which exists so production needs no wiring.
type DropRand interface {
	Intn(n int) int
}

// mathDropRand is the production source: the standard library's own, which is safe
// for concurrent use because a kill can be armed on any session's goroutine.
type mathDropRand struct{}

func (mathDropRand) Intn(n int) int { return rand.Intn(n) }

// dropIntn is the only reader of the drop RNG.
func (h *Handler) dropIntn(n int) int {
	if n <= 1 {
		return 0
	}
	if h.DropRand != nil {
		return h.DropRand.Intn(n)
	}
	return rand.Intn(n)
}

// claimDropRoll is the reference's `m_isItemDropped` guard: the FIRST call for a
// handle owns the roll, every later one is refused. It is keyed by handle because a
// body is spawned once; a refilled HP ledger cannot make the corpse drop twice.
func (h *Handler) claimDropRoll(handle uint32) bool {
	h.dropMu.Lock()
	defer h.dropMu.Unlock()
	if h.dropDone == nil {
		h.dropDone = make(map[uint32]bool, 8)
	}
	if h.dropDone[handle] {
		return false
	}
	h.dropDone[handle] = true
	return true
}

// dropSayOnce prints one line at most once per key.
func (h *Handler) dropSayOnce(key, line string) {
	h.dropMu.Lock()
	defer h.dropMu.Unlock()
	if h.dropSaid == nil {
		h.dropSaid = make(map[string]bool, 8)
	}
	if h.dropSaid[key] {
		return
	}
	h.dropSaid[key] = true
	if h.Log != nil {
		h.Log.Printf("%s", line)
	}
}

// The three readers behind dropTables are VARIABLES for ONE reason (2026-11-07):
// the drop tables used to load inside the FIRST KILL, and a test wraps each with a
// counter to prove a kill no longer reaches the disk (zone\drop_test.go,
// TestTheKillLoadsNoDropTable). Production never reassigns them.
var (
	loadItemTablesFn  = loadItemTableWithImage
	loadMapDropsFn    = loadMapDropsWithImage
	loadDropPenaltyFn = loadDropPenaltyWithImage
)

// dropTables arms whatever this handler was not already given, ONCE.
//
// IT IS NO LONGER LAZY (2026-11-07). It used to load on the first kill, inside the
// death path: the owner's own log shows the first kill's `DROP rules` line 1.85 s
// after the `DEAD` line (`logs\zone_go_live.log`, 17:08:26.580 -> 17:08:28.431), and
// everything the kill owed - the death packet included - waited behind ~75 MB of
// Item XML. A start-up cost belongs at start-up; ArmDropTables calls this before the
// listener opens, and a kill then finds the tables already in hand.
//
// THE ONCE STAYS, and the per-field checks stay with it: a Handler a test builds (or
// one field a future path pre-fills) must still work, and a table that cannot be
// read is reported by its own line, once, not guessed at. A build with no item table
// still rolls the unique trophies (their ids need no item row).
func (h *Handler) dropTables() (*ItemTable, *MapDropTable) {
	h.dropOnce.Do(func() {
		dir := XMLDir()
		// PER-TABLE TIME (card t_96ee949e): the whole block was 2.26 s on map 204 and the
		// log had no line inside it, so nobody could say WHICH table to image first.
		if h.Items == nil {
			at := time.Now()
			t, err := loadItemTablesFn(dir)
			if err != nil {
				h.dropSayOnce("no-items",
					fmt.Sprintf("zone: DROP item table not readable from %s (%v) - "+
						"basic and zone-rule drops cannot be picked; the trophy rule "+
						"still rolls. This line is printed once.", dir, err))
			} else {
				h.Items = t
				h.dropSayOnce("items-ms", fmt.Sprintf("zone: DROP item table parsed in %d ms",
					time.Since(at).Milliseconds()))
			}
		}
		if h.MapDrops == nil {
			at := time.Now()
			t, err := loadMapDropsFn(dir)
			if err != nil {
				h.dropSayOnce("no-maps",
					fmt.Sprintf("zone: DROP map table not readable from %s (%v) - "+
						"the zone rule fires nothing. This line is printed once.", dir, err))
			} else {
				h.MapDrops = t
				h.dropSayOnce("maps-ms", fmt.Sprintf("zone: DROP map table parsed in %d ms",
					time.Since(at).Milliseconds()))
			}
		}
		if h.DropPenalty == nil {
			// THE IMAGE HOLDS THE PATH IT WAS BUILT FROM (zone\penalty_image.go): the
			// candidate list is resolved in ONE place, so a moved export or a different
			// GE_DROP_PENALTY is a rebuild, not a silently stale table.
			at := time.Now()
			t, err := loadDropPenaltyFn()
			if err != nil {
				h.dropSayOnce("no-penalty",
					"zone: DROP penalty table not readable (ge\\server_xml\\"+
						"datatable_droppenalty.xml; the client has no copy) - every "+
						"frequency is used unmultiplied (penalty 1.00). Set "+
						"GE_DROP_PENALTY to point at an export. This line is printed once.")
			} else {
				h.DropPenalty = t
				h.dropSayOnce("penalty-ms", fmt.Sprintf("zone: DROP penalty table read in %d ms",
					time.Since(at).Milliseconds()))
			}
		}
		h.dropSayOnce("rules",
			"zone: DROP rules: basic + unique + zone only - the item-group and "+
				"special-group rules need datatable_itemgroup.ies decoded and are a "+
				"later slice. Fatigue is not modelled (multiplier 1.0).")
	})
	return h.Items, h.MapDrops
}

// ArmDropTables reads the three drop tables at ZONE START and writes ONE line
// naming what it armed. main.go calls it beside the item/status tables, before the
// listener opens, so no kill can ever be the call that reads the disk.
//
// THE LINE IS ALWAYS WRITTEN, including a zone whose tables could not be read: the
// per-table lines above say what is missing and why, and this one says what the
// rolls will actually have - 0 item rows is a fact an operator can act on, while a
// silent start is indistinguishable from a loaded one.
func (h *Handler) ArmDropTables() (items, maps, penalty int) {
	h.dropTables()
	items, maps, penalty = h.Items.Len(), h.MapDrops.Len(), h.DropPenalty.Len()
	h.dropSayOnce("armed", fmt.Sprintf(
		"zone: drop tables armed - %d item row(s), %d map row(s), %d penalty level(s)",
		items, maps, penalty))
	return items, maps, penalty
}

// fieldDrop is ONE field item the roll decided to create, with the numbers that
// justify it. Pieces > 1 is the UNSTACK split: the reference creates one field item
// per piece, each of `Count` units (MonsterDropItem.cpp:415-421).
type fieldDrop struct {
	ItemID uint32
	Count  int
	Pieces int
	Rule   string
	OneIn  int
	Roll   int
	// Why is set when the 3536's appearance cannot be filled from the item row.
	// The drop still happens (the roll decided it); only the packet is withheld.
	Why string
}

// rollMonsterDrop is the ONE roll of ONE kill. It always writes exactly one line and
// sends the 3536 only for drops whose appearance is certain.
func (h *Handler) rollMonsterDrop(m Monster, killer uint32, killerLv int) {
	if !h.claimDropRoll(m.Handle) {
		return
	}
	items, maps := h.dropTables()

	if killerLv <= 0 {
		// The reference's own fallback when no killer pointer exists
		// (MonsterNPC.cpp:764-766): the monster's level, which makes the gap 0.
		killerLv = m.Level
	}

	penalty, havePenalty := h.DropPenalty.Penalty(killerLv, m.Level)
	if !havePenalty {
		penalty = 1
	}

	var row MonsterRow
	haveRow := false
	if h.Monsters != nil {
		row, haveRow = h.Monsters.Row(m.ClassID)
	}

	var drops []fieldDrop
	basicRules := 8
	blocked := false
	if maps != nil {
		if mr, ok := maps.Row(h.MapID); ok {
			blocked = mr.Blocked
		}
	}
	if haveRow && !blocked {
		// `isMonsterDropBlocked` suppresses the basic and unique rules, never the
		// zone rule (MonsterDropItem.cpp:218-220, :290-292).
		drops = append(drops, h.dropByBasicRule(row, items, penalty)...)
		drops = append(drops, h.dropByTrophyRule(row, items, penalty)...)
	}
	if maps != nil {
		drops = append(drops, h.dropByZoneRule(maps, items, penalty)...)
	}

	// THE ONE LINE. It exists for every kill, including one that drops nothing and
	// including a zone whose tables could not be read.
	body := "nothing"
	switch {
	case len(drops) > 0:
		parts := make([]string, 0, len(drops))
		for _, d := range drops {
			p := fmt.Sprintf("%d x%d (%s, 1 in %d, roll %d)",
				d.ItemID, d.Count*d.Pieces, d.Rule, d.OneIn, d.Roll)
			if d.Why != "" {
				p += " [no 3536: " + d.Why + "]"
			}
			if d.Pieces > 1 {
				p += fmt.Sprintf(" as %d x%d", d.Pieces, d.Count)
			}
			parts = append(parts, p)
		}
		body = fmt.Sprintf("%s - penalty %.2f%s", strings.Join(parts, "; "),
			penalty, penaltyNote(havePenalty))
	case !haveRow:
		body = fmt.Sprintf("nothing (no monster row for class %d; penalty %.2f%s)",
			m.ClassID, penalty, penaltyNote(havePenalty))
	default:
		blockNote := ""
		if blocked {
			blockNote = "; map blocks monster drops (MonsterDropBlock=TRUE)"
		}
		body = fmt.Sprintf("nothing (%d rules, penalty %.2f%s%s)", basicRules,
			penalty, penaltyNote(havePenalty), blockNote)
	}

	sent, packets, skipped := h.sendDropPackets(m, killer, drops)
	if h.Log != nil {
		line := fmt.Sprintf("zone: DROP handle %d (class %d) killer %d: %s",
			m.Handle, m.ClassID, killer, body)
		if packets > 0 {
			line += fmt.Sprintf("; cmd %d (%d B) x%d sent %d to %d player(s), %d withheld",
				CmdZCSpawnStandingItem, SpawnStandingItemSize, packets, sent,
				len(h.Players.Snapshot()), skipped)
		}
		h.Log.Printf("%s", line)
	}
}

// penaltyNote makes a missing penalty table visible in the kill line rather than
// letting a 1.00 look like a table reading.
func penaltyNote(have bool) string {
	if have {
		return ""
	}
	return " - no penalty table, used 1.00"
}

// dropByBasicRule is `DropItemByBasicRule` (MonsterDropItem.cpp:216-286): the seven
// drop-rule windows, then the crystal.
//
// THE WINDOWS ARE THE REFERENCE'S OWN, verbatim:
//
//	OldEqp    max=floor(lv/4)+1  min=clamp(max-5, 1, 20)
//	NormEqp   max=floor(lv/4)+1  min=clamp(max,   1, 20)
//	EliteEqp  max=floor(lv/4)+4  min=clamp(max-5, 1, 20)
//	Acc       max=floor(lv/4)+1  min=1
//	Etc       max=floor(lv/4)+1  min=clamp(max,   1, 20)
//	Inst      max=floor(lv/4)+1  min=clamp(floor(lv/4)-3, 1, 25)
//	Recipe    max=floor(lv/4)+1  min=clamp(max,   1, 20)
//
// `imc::Clamp(x, lo, hi)` is min(max(x, lo), hi); the max side is NEVER clamped,
// which is how a level-130 monster asks for level 33 items.
func (h *Handler) dropByBasicRule(row MonsterRow, items *ItemTable, penalty float64) []fieldDrop {
	lv := row.Level
	q := lv / 4 // floor for a positive level
	rules := [7]struct {
		rule  string
		freq  float64
		minLv int
		maxLv int
	}{
		{"OldEqp", row.DropOldEqpFreq, clampInt(q+1-5, 1, 20), q + 1},
		{"NormEqp", row.DropNormEqpFreq, clampInt(q+1, 1, 20), q + 1},
		{"EliteEqp", row.DropEliteEqpFreq, clampInt(q+4-5, 1, 20), q + 4},
		{"Acc", row.DropAccFreq, 1, q + 1},
		{"Etc", row.DropEtcFreq, clampInt(q+1, 1, 20), q + 1},
		{"Inst", row.DropInstFreq, clampInt(q-3, 1, 25), q + 1},
		{"Recipe", row.DropRecipeFreq, clampInt(q+1, 1, 20), q + 1},
	}

	var out []fieldDrop
	for _, r := range rules {
		if r.freq == 0 {
			continue // absent/zero: no roll at all (rounding 0 would become 1)
		}
		n, roll, passed := h.oneIn(r.freq, penalty)
		if !passed {
			continue
		}
		if items == nil {
			continue
		}
		cands := items.ByRule(r.rule, r.minLv, r.maxLv)
		if len(cands) == 0 {
			continue // the reference answers NULL on an empty window
		}
		id := cands[h.dropIntn(len(cands))]
		out = append(out, fieldDrop{ItemID: id, Count: 1, Pieces: 1,
			Rule: r.rule, OneIn: n, Roll: roll})
	}

	// THE CRYSTAL, the eighth frequency. Its item is named, not id'd:
	// `geItemTable::Get("ShinyCrystal")` (MonsterDropItem.cpp:272-276), and its
	// count is `RandomInt(1, DropCrystalCount)` (:278-281).
	if row.DropCrystalFreq > 0 {
		if n, roll, passed := h.oneIn(row.DropCrystalFreq, penalty); passed {
			if items == nil {
				return out
			}
			if id, ok := items.IDByName("ShinyCrystal"); ok {
				count := 1
				if row.DropCrystalCount > 1 {
					count = h.dropIntn(row.DropCrystalCount) + 1
				}
				out = append(out, fieldDrop{ItemID: id, Count: count, Pieces: 1,
					Rule: "Crystal", OneIn: n, Roll: roll})
			} else {
				h.dropSayOnce("no-crystal",
					"zone: DROP the crystal rule fired but the Item table has no "+
						"Class named ShinyCrystal - no crystal is created (the "+
						"reference asserts here). This line is printed once.")
			}
		}
	}
	return out
}

// dropByTrophyRule is `DropItemByUniqueRule` (MonsterDropItem.cpp:288-305) plus
// `DropAssignedItem` (:378-430).
func (h *Handler) dropByTrophyRule(row MonsterRow, items *ItemTable, penalty float64) []fieldDrop {
	var out []fieldDrop
	for i, t := range row.Trophies {
		if t.ID == 0 || t.Per == 0 || t.Quant == 0 {
			continue
		}
		n, roll, passed := h.oneIn(t.Per, penalty)
		if !passed {
			continue
		}
		count := 1
		if t.Quant > 1 {
			count = h.dropIntn(t.Quant) + 1
		}
		rule := fmt.Sprintf("Drop%d", i+1)
		d := fieldDrop{ItemID: t.ID, Count: count, Pieces: 1, Rule: rule, OneIn: n, Roll: roll}
		if !h.resolveItemShape(items, &d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// dropByZoneRule is `DropItemByZoneRule` (MonsterDropItem.cpp:307-317): every
// non-zero DropItemN at 1-in-DropItemNFreq, one unit per hit. The count is 7 because
// `MAXDROPITEM` is 7 - the doc's "1..5" is short by two columns, and the loop in the
// reference is over MAXDROPITEM.
func (h *Handler) dropByZoneRule(maps *MapDropTable, items *ItemTable, penalty float64) []fieldDrop {
	row, ok := maps.Row(h.MapID)
	if !ok {
		return nil
	}
	var out []fieldDrop
	for i := 0; i < 7; i++ {
		id, freq := row.Item[i], row.Freq[i]
		if id == 0 || freq == 0 {
			continue
		}
		n, roll, passed := h.oneIn(freq, penalty)
		if !passed {
			continue
		}
		d := fieldDrop{ItemID: id, Count: 1, Pieces: 1,
			Rule: fmt.Sprintf("Zone%d", i+1), OneIn: n, Roll: roll}
		if !h.resolveItemShape(items, &d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// resolveItemShape applies the reference's UNSTACK split and records why the 3536
// cannot be built, if it cannot. It answers false when the item row is MISSING: the
// reference asserts and creates nothing there (`dropItemID = %d ... itemProperty not
// exists`, MonsterDropItem.cpp:404-410), and inventing one would be worse.
func (h *Handler) resolveItemShape(items *ItemTable, d *fieldDrop) bool {
	if items == nil {
		d.Why = "no item table"
		return true // the drop happened; only its packet cannot be described
	}
	item, ok := items.Row(d.ItemID)
	if !ok {
		h.dropSayOnce(fmt.Sprintf("no-item-%d", d.ItemID),
			fmt.Sprintf("zone: DROP item id %d is in a drop table but NOT in the "+
				"client's Item idspace - the drop is discarded, no field item can "+
				"exist for it (the reference asserts here). This line is printed once.",
				d.ItemID))
		return false
	}
	if item.Unstack && d.Count > 1 {
		// Each unit is its OWN field item: Count=1, Pieces=count
		// (MonsterDropItem.cpp:415-421).
		d.Pieces, d.Count = d.Count, 1
	}
	d.Why = itemAppearanceUncertain(item)
	return true
}

// itemAppearanceUncertain answers "" when every APPEARANCE_ITEM field is known from
// the item row, and otherwise the FIRST field this server cannot fill plus what
// would answer it.
//
// THE THREE BYTES THAT ARE NOT ALWAYS ZERO ARE isOptionItem AND THE TWO RARE-DROP
// COUNTS, and each has a real reader in the reference:
//
//	isOptionItem  1 when the option generator produced a non-empty blob. An option
//	              attaches only when an ItemOpt row's ReqGroupName equals the item's
//	              OptionGroup AND its Rarity is non-zero (OptionGen.cpp:14-40); no
//	              shipped ItemOpt row with Rarity > 0 names "None", so a None-group
//	              item can NEVER carry one. A unique item (Option1ID != 0) ALWAYS
//	              gets one, from its own min/max columns (:83-114) - nothing here
//	              generates that blob, so those drops get no packet.
//	socketCount /  `GetDropRareVal` rolls both when the item's DropRareFreq is
//	reinforceCount non-zero (ItemTable.cpp:211-260). The roll itself is readable,
//	              but the DropReinforceType / DropSocketType tables are not read
//	              by this server yet, so a non-zero DropRareFreq is withheld
//	              rather than sent as zero.
func itemAppearanceUncertain(item ItemRow) string {
	if item.Option1ID != 0 {
		return "isOptionItem: a unique item always gets a generated option blob " +
			"(OptionGen.cpp:83-114), which this zone does not generate"
	}
	if g := item.OptionGroup; g != "" && !strings.EqualFold(g, "None") {
		return "isOptionItem: OptionGroup " + g + " can take a random option " +
			"(an ItemOpt row exists for it); that generator is not implemented"
	}
	if item.DropRareFreq != 0 {
		return "socketCount/reinforceCount: DropRareFreq is non-zero, so " +
			"GetDropRareVal (ItemTable.cpp:211-260) would roll sockets and a " +
			"reinforcement - that roll needs the DropSocketType/DropReinforceType " +
			"tables, which this server does not read"
	}
	return ""
}

// oneIn is the reference's 1-in-N gate (`GetItemRandom` / `DropAssignedItem`):
// round(freq * penalty), divide by RewardItemBM and ItemRatio (both 1 here), floor at
// 1, then pass only when `RandomInt(1, N) == 1`. It answers N, the drawn value
// (1-based, for the log) and whether the rule fired.
func (h *Handler) oneIn(freq, penalty float64) (n, roll int, passed bool) {
	if freq == 0 {
		return 0, 0, false
	}
	f := freq * penalty
	if f < 0 {
		f = 0
	}
	n = int(math.Floor(f + 0.5)) // imc::Round
	if n != 1 {
		if n < 1 {
			n = 1 // the floor, after dividing by BM/ratio (both 1)
		}
	}
	if n <= 1 {
		return 1, 1, true // 1-in-1: always, and no draw is made
	}
	roll = h.dropIntn(n) + 1
	return n, roll, roll == 1
}

// sendDropPackets builds and fans out one 3536 per field item whose appearance is
// certain. It answers how many players accepted the frames, how many frames were
// built, and how many field items were withheld (their kills are still in the log).
//
// EVERY FRAME IT SENDS IS ALSO RECORDED (R98 slice 4). The registry entry goes in
// BEFORE the frame leaves, so a client that has seen a 3536 can never name a handle
// the pickup cannot resolve: the item the player clicks is the item in
// Handler.groundItems. `killer` is the character whose damage took the body's last HP
// and it is stored for a LATER priority slice - nothing refuses on it today
// (zone\pickup.go's header says why the rule is "anyone in reach").
//
// THE FAN-OUT IS THE SIGHT RING'S, BOTH RADII OF IT (broadcastDropAt below): a player
// past the map's ViewRange is not told, and one inside ViewRange but past
// ViewMiddleRange is not either - that inner band is the R135e cull, the same one the
// action fan-out (sendOthers) applies to a swing. Until this was fixed the drop used
// broadcastWorldAt alone, which knows only the outer ring, and a player 4,919 out on
// map 1331 received a 3536 for an item the reference freezes at 4,000.
//
// NOTE FOR A LATER SLICE: 3536 is the SPAWN frame. A player who enters the ring
// AFTER the drop needs 3537 (ZC_ENTER_STANDINGITEM), and this file has no answer for
// that yet - the registry now exists, so the frame has a source, but nothing sends it.
func (h *Handler) sendDropPackets(m Monster, killer uint32, drops []fieldDrop) (sent, packets, skipped int) {
	for _, d := range drops {
		if d.Why != "" {
			skipped += d.Pieces
			continue
		}
		for i := 0; i < d.Pieces; i++ {
			app := ItemAppearance{
				Type:   d.ItemID,
				Count:  uint32(d.Count),
				Parent: m.Handle,
			}
			handle := h.nextItemHandle()
			h.recordGroundItem(groundItem{
				Handle: handle,
				ItemID: d.ItemID,
				Count:  d.Count,
				Pos:    [3]int32{m.X, m.Y, m.Cell},
				Parent: m.Handle,
				Killer: killer,
			})
			pkt := BuildSpawnStandingItem(handle,
				[3]int32{m.X, m.Y, m.Cell}, app, "", 0)
			sent += h.broadcastDropAt(pkt, m.X, m.Y)
			packets++
		}
	}
	return sent, packets, skipped
}

// broadcastDropAt fans ONE field-item frame out over the sight ring's TWO radii.
//
// THE OUTER RADIUS IS THE RING (ViewRange): a player past it never had the object in
// its world, so it is not told - broadcastWorldAt's own rule, unchanged.
// THE INNER RADIUS IS R135e (ViewMiddleRange): a viewer inside the ring but past the
// middle is FROZEN, not gone (`int cullDistance = m_viewMiddleRange;`, View.cpp:269 ->
// zone\sight.go's sightCulls), so the frame that would add this item to its world is
// not sent either. This is the same cull and the same body reader sendOthers uses
// (sightCentre, never the roster's destination - the R135d trap).
//
// WHY NOT sendOthers: it applies the cull but no outer ring, and a drop must do both.
// WHY NOT broadcastWorldAt: the monster spawn and the npc spawn share it, and this
// lane does not change who sees those.
//
// A RING THAT IS NOT ARMED (no map row, -sightoff) SENDS TO EVERYONE, exactly as
// broadcastWorldAt does: a drop nobody can explain is worse than one shown widely.
func (h *Handler) broadcastDropAt(pkt []byte, x, y int32) int {
	view, middle, armed := h.sightRadii()
	var now time.Time
	if armed {
		now = time.Now() // ONE clock for every viewer's centre, as sendOthers reads it
	}
	sent := 0
	for _, m := range h.Players.Snapshot() {
		if armed && len(m.Pos) > 0 {
			if !inRing(m.Pos[0], x, y, view) {
				continue // past ViewRange: this object was never in its world
			}
			if m.Sess != nil && len(m.Sess.Team) > 0 &&
				sightCulls(h.sightCentre(m.Sess, now), x, y, middle, view) {
				continue // R135e: frozen, not gone - the item stays unseen
			}
		}
		if m.Sess == nil {
			continue
		}
		if err := m.Sess.send(pkt); err != nil {
			if h.Log != nil {
				h.Log.Printf("zone: could not tell a player about a field item: %v", err)
			}
			continue
		}
		sent++
	}
	return sent
}

// nextItemHandle hands out one item handle from its OWN block.
//
// The block map is written down in zone\create.go and zone\monster.go: players are
// their database row ids, monsters 0x20000000+index, town/runtime NPCs 0x40000000,
// warp points 0x7F000000. Items take 0x30000000 - between monsters and NPCs, far
// above any row id, and below the sign bit. The reference does not encode a type in
// the handle (its own field items came from a GUID generator), so the only rule is
// that two live actors never share a number.
const ItemHandleBase = 0x30000000

// ItemHandle is the handle of the n-th field item this server made.
func ItemHandle(index int) uint32 { return ItemHandleBase + uint32(index) }

// nextItemHandle is guarded by spawnMu, the same lock the monster counter uses.
func (h *Handler) nextItemHandle() uint32 {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.itemSeq++
	return ItemHandle(h.itemSeq)
}

// clampInt is imc::Clamp for the level windows.
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
