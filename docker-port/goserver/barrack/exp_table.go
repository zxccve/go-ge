package barrack

import (
	"bufio"
	_ "embed"
	"strconv"
	"strings"
)

// The exp -> level table, exported from the Python oracle
// (src\export_exp_table.py -> ge_tables). Do NOT re-derive it in Go: the game's
// table is the authority, and the database's LIFE column is NOT the level (it
// disagrees for 10 of 12 characters here).
//
//go:embed data/exp_levels.csv
var expTableCSV string

type expRow struct {
	level int
	total int64 // cumulative exp needed to BE this level
	band  int   // promotion cap for this level (ge_tables.promotion_band)
}

var expTable []expRow

func init() {
	sc := bufio.NewScanner(strings.NewReader(expTableCSV))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) < 2 {
			continue
		}
		lv, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		total, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		band := 0
		if len(parts) == 3 {
			if b, err3 := strconv.Atoi(strings.TrimSpace(parts[2])); err3 == nil {
				band = b
			}
		}
		expTable = append(expTable, expRow{level: lv, total: total, band: band})
	}
}

// LevelOf returns the level for a cumulative exp total: the highest level whose
// required total the character has reached.
func LevelOf(exp int64) int {
	if len(expTable) == 0 {
		return 1
	}
	lo, hi := 0, len(expTable)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if expTable[mid].total <= exp {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return expTable[lo].level
}

// PromotionBand is the promotion cap for a level (ge_tables.promotion_band).
// The packet's promotionLv is min(band, the character's PromotionStep).
func PromotionBand(level int) int {
	for _, r := range expTable {
		if r.level == level {
			return r.band
		}
	}
	return 0
}

// ExpOf returns the cumulative total needed for a level, or 0 when the table
// does not reach that level.
func ExpOf(level int) int64 {
	for _, r := range expTable {
		if r.level == level {
			return r.total
		}
	}
	return 0
}
