package zone

import (
	"context"
	"testing"
	"time"

	"geserver/db"
)

// THE STORE TEST: the packet builders were already able to carry worn equipment;
// the bug was that nothing ever put it in Character.Equip (zone\store.go TeamOf).
// This test drives the real loader against the real PostgreSQL and demands that a
// character with a wear row comes back wearing it.
//
// It SKIPS when PostgreSQL is unreachable, so `go test ./...` still runs on a
// machine without the container up - but on this project the database is a
// required input (AGENTS.md: start the database FIRST), so a skip here means
// "nothing was proven", never "it works".
func TestLoadCharactersFillsWornEquipment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("PostgreSQL not reachable, so worn equipment was NOT checked: %v", err)
	}
	defer pool.Close()
	store := NewPGStore(pool)

	// A commander whose characters include at least one worn item. The wear rows
	// are the source of truth here, so the expectation below is read from the SAME
	// tables independently of the code under test.
	var commanderID int32
	err = pool.QueryRow(ctx,
		`SELECT c.commander_id FROM character c
		   JOIN wear_item w ON w.character_id = c.character_id
		  WHERE c.iscard = 0
		  ORDER BY c.commander_id LIMIT 1`).Scan(&commanderID)
	if err != nil {
		t.Skipf("no character has a worn item, so nothing could be checked: %v", err)
	}

	type want struct {
		spot  int
		class uint32
	}
	worn := map[uint32][]want{}
	rows, err := pool.Query(ctx,
		`SELECT w.character_id, w.spot, i.class_id
		   FROM wear_item w JOIN item i ON i.item_id = w.item_id
		  WHERE w.character_id IN (SELECT character_id FROM character
		                            WHERE commander_id = $1)`, commanderID)
	if err != nil {
		t.Fatalf("read the expected worn rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			charID uint32
			spot   int
			class  int64
		)
		if err := rows.Scan(&charID, &spot, &class); err != nil {
			t.Fatalf("scan expected worn row: %v", err)
		}
		worn[charID] = append(worn[charID], want{spot: spot, class: uint32(class)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the expected worn rows: %v", err)
	}

	chars, err := store.loadCharacters(ctx, commanderID)
	if err != nil {
		t.Fatalf("loadCharacters(commander %d): %v", commanderID, err)
	}
	if len(chars) == 0 {
		t.Fatalf("commander %d has characters but none was loaded", commanderID)
	}

	checked := 0
	for _, c := range chars {
		for _, w := range worn[c.ID] {
			checked++
			if got := c.Equip[w.spot]; got != w.class {
				t.Fatalf("character %d slot %d = %d, want the worn item's class %d "+
					"(the remote player is drawn from exactly this array)",
					c.ID, w.spot, got, w.class)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("loaded %d characters but none of them had a worn row to check - "+
			"the team selection and the wear rows have drifted apart", len(chars))
	}
	// And the untouched slots stay 0, which the packet builder turns into the
	// client's None id: a filled slot must not smear into its neighbours.
	for _, c := range chars {
		filled := map[int]bool{}
		for _, w := range worn[c.ID] {
			filled[w.spot] = true
		}
		for spot := 0; spot < equipSpotLast; spot++ {
			if !filled[spot] && c.Equip[spot] != 0 {
				t.Fatalf("character %d slot %d = %d but no wear row names it",
					c.ID, spot, c.Equip[spot])
			}
		}
	}
}
