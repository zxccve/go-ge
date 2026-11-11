package barrack

import (
	"bufio"
	_ "embed"
	"encoding/binary"
	"strconv"
	"strings"
)

// BC_COMMANDER_LV_INFO (cmd 37) - the family (commander) level.
//
// The level is NOT stored in any table. The real server derives it from the
// CHARACTER LEVELS:
//
//	CCommander::ComputeCommanderLv   source2\geServer\BarrackServer\Commander.cpp:856
//	geCommLevelTable::GetLevel       source2\geShared\Source\CommLevelTable.cpp:142
//
// and then sends four WORDs (geBCPacket.h:400):
//
//	cmd(2) commLvMax(2) commLvTotal(2) commLvCurrent(2) commLvPercent(2)  = 10 bytes
//
// This is a FIXED packet: there is no length field (see HANDOFF.md traps).
const CommanderLvInfoSize = 10

// The curve is the Xp idspace rows with 1000 < ClassID < 2000, which is a
// DIFFERENT curve from the PC levels in exp_levels.csv. Exported from the Python
// oracle by src\export_commander_exp_table.py - do not re-derive it here.
//
//go:embed data/commander_exp_levels.csv
var commanderExpCSV string

// commanderExpByLevel is the cumulative exp needed to BE at a commander level.
var commanderExpByLevel = map[int]int64{}

// maxCommLevel is geShared.h:183 MAX_COMM_LV - the table stops at 99.
const maxCommLevel = 99

func init() {
	sc := bufio.NewScanner(strings.NewReader(commanderExpCSV))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		lv, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		total, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		commanderExpByLevel[lv] = total
	}
}

// CommanderExpOf is geCommLevelTable::GetExp.
func CommanderExpOf(level int) int64 {
	if level > maxCommLevel {
		level = maxCommLevel
	}
	return commanderExpByLevel[level]
}

// commanderBand finds the band for an exp total: the commander level `lv` whose
// [exp(lv), exp(lv+1)) contains `exp`, and how far into that band it is.
//
// Both GetLevel and GetLevelPercentage walk the SAME loop (CommLevelTable.cpp:142
// and :164), i == 2..MAX_COMM_LV, so they are computed together here - a
// percentage from one band and a level from another is not a thing the server
// can produce.
func commanderBand(exp int64) (level int, into, span int64) {
	for i := 2; i <= maxCommLevel; i++ {
		e1, e2 := CommanderExpOf(i-1), CommanderExpOf(i)
		if exp >= e1 && exp < e2 {
			return i - 1, exp - e1, e2 - e1
		}
		if i == maxCommLevel && exp >= e2 {
			// The C++ re-reads GetExp(i) here; e2 IS GetExp(i), so this is the
			// same test - keep it spelled the same way so it stays obviously
			// faithful to CommLevelTable.cpp:154.
			return i, 0, 0
		}
	}
	return 0, 0, 0
}

// CommanderLevel is geCommLevelTable::GetLevel.
func CommanderLevel(exp int64) int {
	lv, _, _ := commanderBand(exp)
	return lv
}

// CommanderLevelPercent is geCommLevelTable::GetLevelPercentage: the integer
// division the C++ does, and 100 once the last band is passed.
func CommanderLevelPercent(exp int64) int {
	lv, into, span := commanderBand(exp)
	if span == 0 {
		if lv == maxCommLevel {
			return 100
		}
		return 0
	}
	return int(into * 100 / span)
}

// CommanderExp is CCommander::ComputeCommanderLv's exp sum over the account's
// characters - the ONLY input the family level has. Per character:
//
//	level*level, plus 500 * ((Lv-101)/10 + 1) for every level from 101 to Lv,
//	then x1.2 for the HIGHEST-level character of each (job, gender) pair.
//
// The x1.2 lands on one character per pair, not on the pair's whole exp.
func CommanderExp(chars []CharInfo) int64 {
	// Which index holds the highest level for each job*2+gender.
	best := map[int]int{}
	for i, c := range chars {
		if c.Level == 0 {
			continue
		}
		key := int(c.Job)*2 + boolToInt(c.Gender)
		if prev, ok := best[key]; !ok || int(c.Level) > int(chars[prev].Level) {
			best[key] = i
		}
	}

	var total int64
	for i, c := range chars {
		if c.Level == 0 {
			continue
		}
		lv := int64(c.Level)
		exp := lv * lv
		for countLevel := 101; countLevel <= int(c.Level); countLevel++ {
			exp += int64(((countLevel-101)/10)+1) * 500
		}
		key := int(c.Job)*2 + boolToInt(c.Gender)
		if best[key] == i {
			// charCommExp *= 1.2, where charCommExp is an int: the multiply is
			// done in double and truncated back. Do NOT "simplify" this to
			// exp*12/10 - 1.2 is not exact in binary (it is just BELOW 1.2), so
			// the C++ truncates 97500*1.2 to 116,999, and integer arithmetic
			// would answer 117,000. One per character, up to a few hundred.
			exp = int64(float64(exp) * 1.2)
		}
		total += exp
	}
	return total
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BuildCommanderLvInfo builds cmd 37.
//
// storedLevel is the commander level already in the database (GetCommLeve). The
// server keeps the level from going DOWN, so max = the larger of the two. There
// is no party bonus in our server yet, so total == max (the real server adds
// COMMLV_BY_PARTYLV to total only).
func BuildCommanderLvInfo(chars []CharInfo, storedLevel int) []byte {
	exp := CommanderExp(chars)
	current := CommanderLevel(exp)

	level := current
	if storedLevel > level {
		level = storedLevel
	}

	b := make([]byte, CommanderLvInfoSize)
	binary.LittleEndian.PutUint16(b[0:], 37)              // command
	binary.LittleEndian.PutUint16(b[2:], uint16(level))   // commLvMax
	binary.LittleEndian.PutUint16(b[4:], uint16(level))   // commLvTotal
	binary.LittleEndian.PutUint16(b[6:], uint16(current)) // commLvCurrent
	binary.LittleEndian.PutUint16(b[8:], uint16(CommanderLevelPercent(exp)))
	return b
}
