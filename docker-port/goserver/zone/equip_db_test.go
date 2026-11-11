package zone

import (
	"context"
	"os"
	"testing"
	"time"

	"geserver/db"
	"geserver/item"
)

// THE WRITE TEST: an equip and an unequip against the real PostgreSQL, asserting
// the one thing that actually matters - EXACTLY ONE bag row and EXACTLY ONE wear
// row move, and every other row stays where it was. An equip that also ate the
// neighbour, or that re-minted the item id on the way back, would pass a
// packet-level test and lose a player's gear.
//
// It is off by default and says so: it WRITES to the database, and this project's
// rule is that a write run needs a backup first. Commander 25 is the throwaway test
// account (24 belongs to the owner), and every row it creates is deleted again -
// only the rows it minted.
//
//	GE_EQUIP_DB_TEST=1 go test ./zone -run TestEquipDBMovesExactlyOneRow -v
func TestEquipDBMovesExactlyOneRow(t *testing.T) {
	if os.Getenv("GE_EQUIP_DB_TEST") == "" {
		t.Skip("set GE_EQUIP_DB_TEST=1 and start granado_pg to run the write test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Fatalf("PostgreSQL is not reachable, so the equip write was NOT checked: %v", err)
	}
	defer pool.Close()
	store := NewPGStore(pool)

	// The SAME table the zone loads, so a wrong slot in the emitted file fails here
	// and not in game. (Tests run with the package folder as the working
	// directory, so `data` is two levels up - the same path the other zone tests
	// use.)
	slots, err := item.LoadEquipSlots("../../data/item_equip.json")
	if err != nil {
		t.Fatalf("data\\item_equip.json: %v (write it with: cd src && python emit_item_equip.py)", err)
	}
	store.Wearable = slots

	const commander = 25 // the throwaway test commander, never 24
	const account = 36   // account 36 -> commander 25

	var charID int32
	if err := pool.QueryRow(ctx,
		`SELECT character_id FROM character
		  WHERE commander_id = $1 AND iscard = 0
		  ORDER BY slot LIMIT 1`, commander).Scan(&charID); err != nil {
		t.Fatalf("commander %d has no character to equip: %v", commander, err)
	}

	// A free slot is a precondition, not an accident: with something already worn
	// at spot 5 the first equip is a SWAP and the "nothing else moved" counts below
	// would be measuring a different transaction.
	var wornAt5 int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM wear_item WHERE character_id = $1 AND spot = 5`,
		charID).Scan(&wornAt5); err != nil {
		t.Fatalf("read spot 5 of character %d: %v", charID, err)
	}
	if wornAt5 != 0 {
		t.Skipf("character %d already wears something at spot 5; this test needs a free slot", charID)
	}

	// Cleanup deletes the rows THIS test created and nothing else: the item ids
	// are minted in this process, so a row that names one of them is ours. A row
	// that does not is somebody else's and is left alone.
	//
	// It is registered BEFORE the first mint, and it works from a growing list,
	// because a failure on the second mint must not leave the first one in the
	// bag: a leak of one test item is exactly what the gate's checksum reports.
	var minted []int64
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range minted {
			if _, err := pool.Exec(bg,
				`DELETE FROM wear_item WHERE item_id = $1`, id); err != nil {
				t.Errorf("cleanup: wear row of item %d: %v", id, err)
			}
			if _, err := pool.Exec(bg,
				`DELETE FROM storage WHERE item_id = $1 AND commander_id = $2`,
				id, commander); err != nil {
				t.Errorf("cleanup: storage row of item %d: %v", id, err)
			}
			if _, err := pool.Exec(bg,
				`DELETE FROM item WHERE item_id = $1`, id); err != nil {
				t.Errorf("cleanup: item %d: %v", id, err)
			}
		}
	}()

	// mint puts one item of a class into the commander's bag through the same path
	// the script engine uses, and answers the item id it was given.
	mint := func(class int64) int64 {
		t.Helper()
		changes, err := store.CommitTx(ctx, commander, nil, []item.Op{{
			Give: true, ClassID: class, Count: 1, MaxStack: 1, Name: "equip test",
		}})
		if err != nil {
			t.Fatalf("mint class %d for commander %d: %v", class, commander, err)
		}
		if len(changes) != 1 || changes[0].ItemID == 0 {
			t.Fatalf("minting class %d answered %+v, want one new item", class, changes)
		}
		minted = append(minted, changes[0].ItemID)
		return changes[0].ItemID
	}
	swordA := mint(7201)  // either hand: spot 4 or 5
	swordB := mint(7202)  // the same, for the SWAP
	armour := mint(15001) // BODY: spot 1, so spot 5 must be refused

	// snapshot is the "nothing else moved" measurement: how many rows the two
	// tables hold for this character and this commander.
	//
	//	SELECT count(*) FROM wear_item WHERE character_id = $1
	//	SELECT count(*) FROM storage WHERE commander_id = $1
	snapshot := func() (wear, storage int) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wear_item WHERE character_id = $1`,
			charID).Scan(&wear); err != nil {
			t.Fatalf("count wear_item of character %d: %v", charID, err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM storage WHERE commander_id = $1`,
			commander).Scan(&storage); err != nil {
			t.Fatalf("count storage of commander %d: %v", commander, err)
		}
		return wear, storage
	}
	// wornItem is the item id at a spot, or 0 when nothing is worn there.
	//
	//	SELECT item_id FROM wear_item WHERE character_id = $1 AND spot = $2
	wornItem := func(spot int) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx,
			`SELECT item_id FROM wear_item WHERE character_id = $1 AND spot = $2`,
			charID, spot).Scan(&id)
		if err != nil {
			return 0
		}
		return id
	}
	// inBag reports whether an item row is in the commander's bag.
	//
	//	SELECT count(*) FROM storage WHERE item_id = $1 AND commander_id = $2
	inBag := func(itemID int64) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM storage WHERE item_id = $1 AND commander_id = $2`,
			itemID, commander).Scan(&n); err != nil {
			t.Fatalf("count storage for item %d: %v", itemID, err)
		}
		return n
	}
	// bagCount is the item's count column, read through its storage row.
	//
	//	SELECT i.count FROM storage s JOIN item i ON i.item_id = s.item_id
	//	 WHERE s.item_id = $1 AND s.commander_id = $2
	bagCount := func(itemID int64) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT i.count FROM storage s JOIN item i ON i.item_id = s.item_id
			  WHERE s.item_id = $1 AND s.commander_id = $2`,
			itemID, commander).Scan(&n); err != nil {
			t.Fatalf("read the bag row of item %d: %v", itemID, err)
		}
		return n
	}

	// (a) AN EMPTY SLOT: one wear row appears, the bag row goes, nothing else moves.
	wearBefore, storageBefore := snapshot()
	res, err := store.EquipItem(ctx, commander, charID, swordA, 5)
	if err != nil {
		t.Fatalf("equip 7201 at spot 5: %v", err)
	}
	if res.Spot != 5 || res.ClassID != 7201 || res.Removed != swordA {
		t.Fatalf("equip answered spot %d class %d removed %d, want 5 / 7201 / %d",
			res.Spot, res.ClassID, res.Removed, swordA)
	}
	if res.Returned != nil {
		t.Fatalf("an empty slot returned item %d, want nothing", res.Returned.ItemID)
	}
	if got := wornItem(5); got != swordA {
		t.Fatalf("spot 5 wears item %d, want the one just equipped (%d)", got, swordA)
	}
	if n := inBag(swordA); n != 0 {
		t.Fatalf("item %d is still in the bag (%d row(s)) after being equipped", swordA, n)
	}
	wearAfter, storageAfter := snapshot()
	if wearAfter != wearBefore+1 {
		t.Fatalf("wear_item rows %d -> %d, want exactly one more", wearBefore, wearAfter)
	}
	if storageAfter != storageBefore-1 {
		t.Fatalf("storage rows %d -> %d, want exactly one fewer", storageBefore, storageAfter)
	}
	if res.Equip[5] != 7201 {
		t.Fatalf("the worn set says slot 5 = %d, want 7201", res.Equip[5])
	}

	// (b) THE SWAP: the second sword takes the slot, and the first one is BACK in
	// the bag with its own id.
	wearBefore, storageBefore = snapshot()
	res, err = store.EquipItem(ctx, commander, charID, swordB, 5)
	if err != nil {
		t.Fatalf("equip 7202 at spot 5 (the swap): %v", err)
	}
	if res.Returned == nil || res.Returned.ItemID != swordA {
		t.Fatalf("the swap returned %+v, want item %d back in the bag", res.Returned, swordA)
	}
	if got := inBag(swordA); got != 1 {
		t.Fatalf("the displaced item %d is in %d bag row(s), want exactly 1", swordA, got)
	}
	if got := inBag(swordB); got != 0 {
		t.Fatalf("the equipped item %d is still in the bag", swordB)
	}
	if got := wornItem(5); got != swordB {
		t.Fatalf("spot 5 wears item %d, want %d", got, swordB)
	}
	wearAfter, storageAfter = snapshot()
	if wearAfter != wearBefore {
		t.Fatalf("wear_item rows %d -> %d on a swap, want no change", wearBefore, wearAfter)
	}
	if storageAfter != storageBefore {
		t.Fatalf("storage rows %d -> %d on a swap, want no change", storageBefore, storageAfter)
	}

	// (c) UNEQUIP: the row comes back with the SAME item id and count - not a new
	// one, which would make the client lose track of its own item.
	res, err = store.UnequipItem(ctx, commander, charID, 5)
	if err != nil {
		t.Fatalf("unequip spot 5: %v", err)
	}
	if res.Returned == nil || res.Returned.ItemID != swordB {
		t.Fatalf("unequip returned %+v, want item %d", res.Returned, swordB)
	}
	if res.Returned.Count != 1 {
		t.Fatalf("the returned item's count = %d, want the 1 it was worn with",
			res.Returned.Count)
	}
	if got := inBag(swordB); got != 1 {
		t.Fatalf("the unequipped item %d is in %d bag row(s), want exactly 1", swordB, got)
	}
	if got := bagCount(swordB); got != 1 {
		t.Fatalf("the unequipped item's count = %d, want 1", got)
	}
	if got := wornItem(5); got != 0 {
		t.Fatalf("spot 5 still wears item %d after the unequip", got)
	}

	// (d) SOMEBODY ELSE'S ITEM: a row that is really in commander 24's bag must be
	// refused, and neither table may change.
	var foreign int64
	if err := pool.QueryRow(ctx,
		`SELECT item_id FROM storage
		  WHERE commander_id = 24 AND type = 0 AND trash = 0
		  ORDER BY item_id LIMIT 1`).Scan(&foreign); err != nil {
		t.Fatalf("commander 24 holds no item to try: %v", err)
	}
	wearBefore, storageBefore = snapshot()
	if _, err := store.EquipItem(ctx, commander, charID, foreign, 5); err == nil {
		t.Fatalf("equipping item %d, which belongs to commander 24, was ACCEPTED", foreign)
	}
	wearAfter, storageAfter = snapshot()
	if wearAfter != wearBefore || storageAfter != storageBefore {
		t.Fatalf("a refused foreign item moved rows: wear %d -> %d, storage %d -> %d",
			wearBefore, wearAfter, storageBefore, storageAfter)
	}
	if got := inBag(foreign); got != 0 {
		t.Fatalf("commander 24's item %d is now in %d row(s) of commander 25's bag",
			foreign, got)
	}
	//	SELECT count(*) FROM storage WHERE item_id = $1 AND commander_id = 24
	var stillOwner int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM storage WHERE item_id = $1 AND commander_id = 24`,
		foreign).Scan(&stillOwner); err != nil {
		t.Fatalf("count commander 24's row for item %d: %v", foreign, err)
	}
	if stillOwner != 1 {
		t.Fatalf("commander 24's item %d is in %d of ITS own rows, want 1 untouched",
			foreign, stillOwner)
	}

	// (e) AN ITEM ID THAT DOES NOT EXIST: refused, nothing moves. MaxInt64 is above
	// anything the item-base sequence can have issued in this database.
	wearBefore, storageBefore = snapshot()
	if _, err := store.EquipItem(ctx, commander, charID, 9223372036854775807, 5); err == nil {
		t.Fatal("equipping an item id that does not exist was ACCEPTED")
	}
	wearAfter, storageAfter = snapshot()
	if wearAfter != wearBefore || storageAfter != storageBefore {
		t.Fatalf("a refused unknown item moved rows: wear %d -> %d, storage %d -> %d",
			wearBefore, wearAfter, storageBefore, storageAfter)
	}

	// (f) THE WRONG SLOT: 15001 is BODY armour (spot 1), so spot 5 must be refused
	// BEFORE anything is written - the shipped server would silently move it.
	wearBefore, storageBefore = snapshot()
	if _, err := store.EquipItem(ctx, commander, charID, armour, 5); err == nil {
		t.Fatal("a BODY armour was ACCEPTED at spot 5 (either hand)")
	}
	wearAfter, storageAfter = snapshot()
	if wearAfter != wearBefore || storageAfter != storageBefore {
		t.Fatalf("a refused wrong-slot equip moved rows: wear %d -> %d, storage %d -> %d",
			wearBefore, wearAfter, storageBefore, storageAfter)
	}
	if got := inBag(armour); got != 1 {
		t.Fatalf("the refused armour is in %d bag row(s), want 1 (untouched)", got)
	}

	// (g) A FRESH LOGIN FINDS IT: TeamOf is what entry uses, so the worn class has
	// to come back through it. Equip again first, because (c) took it off.
	if _, err := store.EquipItem(ctx, commander, charID, swordA, 5); err != nil {
		t.Fatalf("re-equip 7201 at spot 5 for the entry check: %v", err)
	}
	team, err := store.TeamOf(ctx, account)
	if err != nil {
		t.Fatalf("TeamOf(account %d): %v", account, err)
	}
	found := false
	for _, c := range team {
		if int32(c.Handle) != charID {
			continue
		}
		found = true
		if c.Equip[5] != 7201 {
			t.Fatalf("entry reports slot 5 = %d, want the worn class 7201", c.Equip[5])
		}
	}
	if !found {
		t.Fatalf("account %d's team does not include character %d, so the fresh-login "+
			"check proved nothing", account, charID)
	}
}
