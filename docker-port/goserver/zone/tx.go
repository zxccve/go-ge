package zone

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The database side of a script transaction (`TxBegin` -> `TxSetCommProp` ->
// `TxEnd`). The SCRIPT engine owns the transaction's lifetime and decides when
// it commits; this file only knows how to read and write the rows, which is
// what keeps the Lua side free of SQL.
//
// The zone's PGStore satisfies `script.TxStore` structurally, so neither package
// imports the other - main wires them together.

// propertySectionNamed is the SECTION named properties live in.
//
// The table is keyed by (commander_id, property, section). SECTION 1 rows are
// the ones the legacy server keyed by PROPERTY_IDX (their `property` text is the
// literal 'NULL' - the import lost the name), and SECTION 2 is where a row that
// carries its own name belongs: `Lock_InventoryCount` is a section-2 row.
// Writing every named property as section 2 keeps one row per name, which is
// what the primary key promises.
const propertySectionNamed = 2

// CommanderProps reads every named property of a commander.
//
// A property with no row is simply absent, and the script reads 0 for it - the
// same answer the shipped IES object gives an unset integer, and the same
// answer the Python oracle's CommanderBag gives.
func (s *PGStore) CommanderProps(ctx context.Context, commanderID int32) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT property, value FROM commander_property
		  WHERE commander_id = $1 AND section = $2 AND property IS NOT NULL
		  ORDER BY property`, commanderID, propertySectionNamed)
	if err != nil {
		return nil, fmt.Errorf("properties of commander %d: %w", commanderID, err)
	}
	defer rows.Close()

	out := make(map[string]int64, 64)
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("scan property: %w", err)
		}
		// The legacy import wrote the literal text 'NULL' into rows that have no
		// name (see propertySectionNamed). It is not a property, and treating it
		// as one would collide with a real name.
		if name == "" || strings.EqualFold(name, "NULL") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			// The column is TEXT. A row that is not a number is skipped, not
			// read as 0: 0 is a real value a quest guard compares against.
			continue
		}
		out[name] = value
	}
	return out, rows.Err()
}

// The WRITE half of a transaction lives in items.go (`CommitTx`), because a script
// transaction may move properties AND items and both must land together - one
// method, one database transaction. Property-only writes go through it too.
