package barrack

// The SQL half of the family name: the handler tests prove the packet, this proves the
// WRITE - a commander row exists, the account points at it, and a second attempt writes
// nothing.
//
// IT IS BEHIND AN ENV SWITCH, unlike the Extend test, because it CREATES accounts: a stray
// run against the live database would leave test families behind. Run it at a scratch
// database:
//
//	GE_CMDR_DB_TEST=1 GE_PG_DSN=postgres://granado:...@127.0.0.1:PORT/ge_data \
//	    go test ./barrack -run TestCreateCommanderWritesTheFamily -v
//
// Everything it creates it deletes again in t.Cleanup, and the name carries a unique suffix
// so a crashed run is identifiable rather than confusing.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"geserver/account"
	"geserver/db"
)

func TestCreateCommanderWritesTheFamily(t *testing.T) {
	if os.Getenv("GE_CMDR_DB_TEST") != "1" {
		t.Skip("set GE_CMDR_DB_TEST=1 and GE_PG_DSN at a SCRATCH database to run this")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("no database: %v", err)
	}
	// Cleanups run LAST-REGISTERED-FIRST, so the pool is registered here and closed after
	// the deletions below. A plain `defer pool.Close()` closes it BEFORE t.Cleanup runs,
	// which left this test's rows behind the first time it was written.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGBarrackStore(pool)

	member := fmt.Sprintf("cmdr_test_%d", time.Now().UnixNano()%1_000_000)
	accID, err := account.Create(ctx, pool, member, "secret")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Children first: commander_property and character cascade from commander, but
		// deleting explicitly keeps this readable and independent of the schema's CASCADEs.
		if _, err := pool.Exec(ctx, `DELETE FROM commander_property WHERE commander_id IN
			(SELECT commander_id FROM commander WHERE account_id = $1)`, accID); err != nil {
			t.Logf("cleanup properties: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM character WHERE commander_id IN
			(SELECT commander_id FROM commander WHERE account_id = $1)`, accID); err != nil {
			t.Logf("cleanup characters: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM commander WHERE account_id = $1", accID); err != nil {
			t.Logf("cleanup commander: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM account_pass WHERE member_id = $1", member); err != nil {
			t.Logf("cleanup pass: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM account_info WHERE account_id = $1", accID); err != nil {
			t.Logf("cleanup account: %v", err)
		}
	})

	row, created, err := store.CreateCommander(ctx, accID, member, "TestFamily")
	if err != nil {
		t.Fatalf("CreateCommander: %v", err)
	}
	if !created {
		t.Fatal("created = false on an account with no commander")
	}
	if row.Name != "TestFamily" || row.ID == 0 {
		t.Fatalf("row = id %d name %q, want a real id and TestFamily", row.ID, row.Name)
	}
	// The reader's own defaults must come back, because the reply and the next login use them.
	if row.CharSlot != defaultCharSlot || row.TeamMax != defaultTeamMax {
		t.Fatalf("charSlot/teamMax = %d/%d, want %d/%d", row.CharSlot, row.TeamMax,
			defaultCharSlot, defaultTeamMax)
	}

	var pointed uint32
	if err := pool.QueryRow(ctx,
		"SELECT commander_id FROM account_info WHERE account_id = $1", accID).Scan(&pointed); err != nil {
		t.Fatalf("read back account: %v", err)
	}
	if pointed != row.ID {
		t.Fatalf("account_info.commander_id = %d, want %d", pointed, row.ID)
	}

	// A SECOND CALL WRITES NOTHING: the same family comes back, with its original name.
	again, createdAgain, err := store.CreateCommander(ctx, accID, member, "SomeOtherName")
	if err != nil {
		t.Fatalf("second CreateCommander: %v", err)
	}
	if createdAgain {
		t.Fatal("created = true on the second call; it must not make a second family")
	}
	if again.ID != row.ID || again.Name != "TestFamily" {
		t.Fatalf("second call returned id %d name %q, want the first family %d %q",
			again.ID, again.Name, row.ID, row.Name)
	}
}
