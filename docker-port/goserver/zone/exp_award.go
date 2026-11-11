package zone

import (
	"encoding/binary"
)

// EXPERIENCE ON A KILL (R98 slice 2).
//
// THE SPEC IS `history\monster-death.md` §3. The reference adds the experience in
// `CMonsterNPC::DistributeEXP` (`source2\geServer\ZServerLib\MonsterNPC.cpp:795-874`),
// reached from the death state's own ON_UPDATE (`:2651`):
//
//	exp = (float) geMonsterLevelTable::GetExp(lv) * GetEXP();     :801
//
// and pays it to the players who damaged the body and are still alive within
// MAX_EXP_RANGE (30 m), then the receiving commander splits that among its own
// living team (`Commander.cpp:3654-3692`, `:1418-1470`).
//
// WHAT THIS ZONE CAN AND CANNOT SETTLE, all of it logged rather than invented:
//
//  1. THE PER-LEVEL NUMBER IS THE GAME'S. `MonsterExpTable` (zone\monster_exp.go)
//     holds the `Xp` idspace's `MonXp` column, emitted by `src\export_exp_table.py`
//     from the client's own `ge\ies\datatable_xp.ies`.
//  2. THE MONSTER'S OWN FACTOR IS FIXED AT 1.0. `GetEXP()` has NO reader anywhere
//     in either tree: `datatable_monster.ies` carries the column (GameQueen
//     ClassID 10 = 4.403) but `CMonsterTable::AddProperty` never reads it
//     (`MonsterTable.cpp:92-140`) and no `GetEXP()` is declared for a monster or a
//     field character (`PlayerChar.h:428` is the PLAYER's accumulated exp). The
//     server's exp-rate config (`GetWorld()->GetRewardExpBM()`, `:797`) is not in
//     the trees either. A 4.403-style number here would be invented, so the factor
//     is 1.0 and the assumption is printed ONCE, at the first kill.
//  3. THE SHARE IS WHOLE, PER LIVING CHARACTER, NOT SPLIT. The reference's split is
//     level-weighted (`pow(level, GetExpDistributeRatio()) + 100`,
//     `Commander.cpp:3686`) and its damage split needs `m_attackers`
//     (`:815-860`) - a list this zone does not keep, because the kill ledger
//     (`zone\create.go`, takeHP) records the body and not the hitter. Both numbers
//     are config/state the source does not settle here, so every living character
//     inside the reference's own range gets the WHOLE amount and the kill line says
//     so. The 30 m range gate IS kept: it is a measured constant
//     (`MAX_EXP_RANGE = 3000`, `FieldChar.h:7`), not a choice.
//  4. THE LEVEL-GAP SCALE IS NOT APPLIED. `gePCLevelTable::GetXpAdjustValue`
//     (`Commander.cpp:1449-1450`) scales the award by the gap between the
//     character's level and the monster's. It needs a settled monster-level-to-
//     character-level mapping that this slice does not have, so it is left out and
//     the kill line is the record of that.
//
// THE CLIENT IS TOLD. `ZC_EXP_UP` is 3629, 23 bytes (`data\cmd_table.txt:1103`,
// `PKS_ZC_EXP_UP`, `geZCPacket.h:1839-1846`), sent by the exp transaction
// (`TxExpUp.cpp:227-235`):
//
//	cmd(2) charIndex(1) exp(8) stanceType(4) stanceExp(4) autoRule(4)
//
// `exp` is the character's TOTAL accumulated experience, not the gain
// (`expReward.exp = m_pc->GetEXP()`), and the client's handler hands it to
// `gestatus::SetExp(charIndex, exp, stanceType, stanceExp)`
// (`GENormalNet_Session.cpp:238-244`) - that is the exp bar. `charIndex` is the
// character's own slot in the team (`m_pc->m_index`). `stanceExp` is 0 because
// this zone's stance records already say level 1 / 0 exp for every stance
// (`zone\stance.go`, BuildStanceRecords - the legacy stance rows are not read
// yet), so 0 is what the entry burst already told the client. `autoRule` is the
// auto-inspect folder key, which this zone does not have: 0 encodes "no rule".

const (
	// CmdZCExpUp is ZC_EXP_UP, from the CLIENT's own table (`data\cmd_table.txt:1103`),
	// never from the header's enum arithmetic.
	CmdZCExpUp = 3629
	// ExpUpSize is the registered size: cmd(2) charIndex(1) exp(8) stanceType(4)
	// stanceExp(4) autoRule(4). There is no length word - the packet is sized by
	// its command, the same rule ZC_CHANGE_STANCE follows.
	ExpUpSize = 23

	// ExpFactor is the monster's own `GetEXP()` percentage. SEE 2 ABOVE: fixed at
	// 1.0 because no reader for the column exists in either tree.
	ExpFactor = 1.0

	// ExpRange is `MAX_EXP_RANGE` (`source2\geServer\ZServerLib\FieldChar.h:7`): how
	// far from the corpse a character may be and still be paid. 3000 world units.
	ExpRange = 3000

	// ZC_EXP_UP field offsets, measured from the struct in geZCPacket.h:1839.
	euCharIndex  = 2
	euExp        = 3
	euStanceType = 11
	euStanceExp  = 15
	euAutoRule   = 19
)

// BuildExpUp builds ZC_EXP_UP in its 23 bytes. `exp` is the character's TOTAL, not
// the gain (`TxExpUp.cpp:231`), because that is what the client puts on the bar.
func BuildExpUp(charIndex uint8, exp int64, stanceType, stanceExp uint32, autoRule int32) []byte {
	out := make([]byte, ExpUpSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCExpUp)
	out[euCharIndex] = charIndex
	binary.LittleEndian.PutUint64(out[euExp:], uint64(exp))
	binary.LittleEndian.PutUint32(out[euStanceType:], stanceType)
	binary.LittleEndian.PutUint32(out[euStanceExp:], stanceExp)
	binary.LittleEndian.PutUint32(out[euAutoRule:], uint32(autoRule))
	return out
}

// monsterExpPerLevel answers the per-level number a monster of this level pays,
// and which table level was actually used. ok is false when this zone has no
// table at all, which the award turns into "award 0, say so once".
//
// THE CLAMP LIVES HERE and nowhere else: `GetExp` refuses to look past MAX_LV, so
// a level-130 monster pays the level-100 row (`monster_exp.go`).
func (h *Handler) monsterExpPerLevel(level int) (perLevel int64, tableLevel int, ok bool) {
	if h.MonsterExp == nil {
		return 0, level, false
	}
	if level > MonsterExpMaxLevel {
		level = MonsterExpMaxLevel
	}
	v, found := h.MonsterExp.For(level)
	return v, level, found
}

// expSayOnce prints a line the FIRST time it is asked for that key, and never
// again. The two assumptions (no table, factor 1.0) are one-time lines: a kill
// loop must not bury the log.
func (h *Handler) expSayOnce(key, line string) {
	h.expMu.Lock()
	if h.expSaid == nil {
		h.expSaid = map[string]bool{}
	}
	seen := h.expSaid[key]
	h.expSaid[key] = true
	h.expMu.Unlock()
	if !seen && h.Log != nil {
		h.Log.Printf("%s", line)
	}
}

// creditExp adds one award to a character's running total and answers the new
// total. The total starts from the experience the character loaded with, so the
// client's bar continues from where it was; it lives in memory only - nothing is
// written to the database, and a relog reads the stored value again.
func (h *Handler) creditExp(handle uint32, base, gain int64) int64 {
	h.expMu.Lock()
	defer h.expMu.Unlock()
	if h.expTotals == nil {
		h.expTotals = map[uint32]int64{}
	}
	total, ok := h.expTotals[handle]
	if !ok {
		total = base
	}
	total += gain
	h.expTotals[handle] = total
	return total
}

// expTotal answers the total this zone has credited to a character, and whether
// it has credited anything - the test's view of the ledger.
func (h *Handler) expTotal(handle uint32) (int64, bool) {
	h.expMu.Lock()
	defer h.expMu.Unlock()
	v, ok := h.expTotals[handle]
	return v, ok
}

// withinExpRange is `MAX_EXP_RANGE` around the corpse, measured on the world's
// own x/y (the same two numbers every position in this zone carries; the CELL is
// a navmesh triangle, never a height).
func withinExpRange(m Monster, pos [3]int32) bool {
	dx := int64(pos[0]) - int64(m.X)
	dy := int64(pos[1]) - int64(m.Y)
	return dx*dx+dy*dy <= int64(ExpRange)*int64(ExpRange)
}

// awardMonsterExp pays the team for ONE kill, at the instant the kill is armed
// (zone\create.go, damageMonster) - NOT when the ZC_DEAD goes out 500 ms later,
// so a kill that is never ticked still pays and the tick never pays twice.
//
// It writes ONE line per kill: the monster, its level (and the table level the
// clamp used), the per-level value, the factor, the share each member got, and
// the packet that went out.
func (h *Handler) awardMonsterExp(m Monster) {
	perLevel, tableLevel, haveTable := h.monsterExpPerLevel(m.Level)
	if !haveTable {
		h.expSayOnce("no-table",
			"zone: EXP pays 0 - this zone has no monster exp table "+
				"(goserver\\zone\\data\\monster_exp.csv, written by "+
				"src\\export_exp_table.py). Kills award nothing and no "+
				"ZC_EXP_UP goes out. This line is printed once.")
		perLevel, tableLevel = 0, m.Level
	} else {
		h.expSayOnce("factor",
			"zone: monster EXP factor is FIXED at 1.0 - the reference has no "+
				"reader for a monster's EXP column (MonsterTable.cpp:92-140) and "+
				"no GetEXP() for a monster, so the 4.403-style numbers in "+
				"datatable_monster.ies are NOT used. See history\\monster-death.md §3.")
	}

	gain := int64(float64(perLevel) * ExpFactor)
	if gain < 0 {
		gain = 0
	}
	// The reference pays nothing when the product is 0 (MonsterNPC.cpp:802-803).
	if gain == 0 {
		if h.Log != nil {
			h.Log.Printf("zone: EXP %q lv %d (table lv %d) - per-level %d x "+
				"factor %.1f = 0, so nothing is awarded and no cmd %d goes out",
				h.MonsterName(m), m.Level, tableLevel, perLevel, ExpFactor, CmdZCExpUp)
		}
		return
	}

	living, teams, sent := 0, 0, 0
	for _, mem := range h.Players.Snapshot() {
		var pkts [][]byte
		for i, c := range mem.Chars {
			// A DEAD TEAMMATE IS PAID NOTHING. This zone keeps no body state other
			// than HP, so HP <= 0 is the whole of "dead" (the reference also skips
			// BS_DEAD and zombie, `MonsterNPC.cpp:817-818`).
			if !(c.HP > 0) {
				continue
			}
			if i >= len(mem.Pos) || !withinExpRange(m, mem.Pos[i]) {
				continue
			}
			living++
			total := h.creditExp(c.Handle, c.Exp, gain)
			// stanceExp 0: every stance this zone sends is already level 1 / 0 exp
			// (zone\stance.go, BuildStanceRecords). autoRule 0: no auto-inspect rule.
			pkts = append(pkts, BuildExpUp(uint8(i), total, c.Stance, 0, 0))
		}
		if len(pkts) == 0 {
			continue
		}
		teams++
		if err := h.writeSession(mem.Sess, pkts); err != nil {
			h.Log.Printf("zone: EXP - could not tell handle %d about the gain: %v",
				mem.Chars[0].Handle, err)
			continue
		}
		sent += len(pkts)
	}

	if h.Log != nil {
		h.Log.Printf("zone: EXP %q lv %d (table lv %d) - per-level %d x factor "+
			"%.1f = %d, WHOLE to each: %d to %d living character(s) in %d team(s); "+
			"cmd %d ZC_EXP_UP (%d B) sent %d",
			h.MonsterName(m), m.Level, tableLevel, perLevel, ExpFactor, gain,
			gain, living, teams, CmdZCExpUp, ExpUpSize, sent)
	}
}
