package zone

import (
	"bufio"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

// THE MONSTER EXPERIENCE TABLE (R98 slice 2).
//
// A kill pays `geMonsterLevelTable::GetExp(level)` BEFORE the monster's own
// percentage is applied (`MonsterNPC.cpp:801`):
//
//	exp = (float) geMonsterLevelTable::GetExp(lv) * GetEXP();
//
// and `GetExp` reads the client's own `Xp` idspace, column **MonXp**
// (`source2\geShared\Source\MonsterLevelTable.cpp:40-66`). That is
// `ge\ies\datatable_xp.ies` - the SAME file the PC level curve comes from, a
// different column, which is why nothing under `data\` carried it: the barrack
// export kept `TotalXp`/`NextXp` only.
//
// The numbers are exported ONCE by `src\export_exp_table.py`
// (-> `zone\data\monster_exp.csv`) and embedded, so a zone restart does not
// parse a 140 KB .ies and the Go side never re-derives a table. This is the
// project's oracle rule, the same one `barrack\data\exp_levels.csv` follows.
//
//go:embed data/monster_exp.csv
var monsterExpCSV string

// MonsterExpMaxLevel is the reference's `MAX_LV`, and `GetExp` CLAMPS to it:
//
//	DWORD CMonsterLevelTable::GetExp(WORD level)
//	{
//	    if (level > MAX_LV)
//	        level = MAX_LV;
//
// (`MonsterLevelTable.cpp:79-83`, `const int MAX_LV = 100;`,
// `source2\geShared\include\geShared.h:174`.) So a level-130 monster pays the
// level-100 row, NOT the level-130 one. That is the reference's own behaviour
// and this port keeps it: the two rows differ (MonXp 100 = 1018, 130 = 1492),
// so the clamp is visible, and `monsterExpPerLevel` is the ONE place it applies.
const MonsterExpMaxLevel = 100

// MonsterExpTable is level -> MonXp, exactly the rows the emitter wrote.
type MonsterExpTable struct {
	byLevel map[int]int64
	low     int
	high    int
}

// LoadMonsterExpTable parses the embedded copy. A row that cannot be read is
// SKIPPED, not defaulted: a level with no usable row must answer "no row", so
// the award can say so instead of paying a number nobody printed.
func LoadMonsterExpTable() (*MonsterExpTable, error) {
	t := &MonsterExpTable{byLevel: map[int]int64{}}
	sc := bufio.NewScanner(strings.NewReader(monsterExpCSV))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		level, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		exp, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		t.byLevel[level] = exp
		if t.low == 0 || level < t.low {
			t.low = level
		}
		if level > t.high {
			t.high = level
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(t.byLevel) == 0 {
		return nil, fmt.Errorf("no level,monster_exp rows in the embedded table")
	}
	return t, nil
}

// For is the RAW lookup: the row for `level`, with no clamp and no scaling.
// GetExp is the reference's entry point and is what the award uses; this one is
// here so a test can show that the two differ above MAX_LV.
func (t *MonsterExpTable) For(level int) (int64, bool) {
	if t == nil || len(t.byLevel) == 0 {
		return 0, false
	}
	v, ok := t.byLevel[level]
	return v, ok
}

// GetExp is `geMonsterLevelTable::GetExp` exactly: clamp the level to MAX_LV,
// then find that row. ok is false when the table has no such row (the reference
// would Assert and pay 0).
func (t *MonsterExpTable) GetExp(level int) (int64, bool) {
	if level > MonsterExpMaxLevel {
		level = MonsterExpMaxLevel
	}
	return t.For(level)
}

// Describe is for the boot/kill log: how many levels the table gave us.
func (t *MonsterExpTable) Describe() string {
	if t == nil || len(t.byLevel) == 0 {
		return "no monster exp table"
	}
	return fmt.Sprintf("%d level(s) Lv %d..%d", len(t.byLevel), t.low, t.high)
}
