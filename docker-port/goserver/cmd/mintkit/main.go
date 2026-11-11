// Command mintkit puts a few REAL items into a commander's bag, through the
// same write path a shop purchase uses.
//
// WHY THIS IS A PROGRAM AND NOT SQL: an item id is not a row number. The shipped
// server mints it as (base<<32)+counter, the base comes from `item_base_seq` so
// two processes never collide, and the bag row has to be written next to the
// item row or the client is told about a slot that does not exist. That shape
// already exists once - `zone\items.go` NextItemID + mintItem - and a hand
// written INSERT is how the key space gets broken. This program calls THAT path
// (`PGStore.CommitTx` with a give op), so what it mints is indistinguishable
// from what a shop gives.
//
// It NEVER deletes and never touches an existing row: a give either joins the
// oldest stack of its class (when the item table says the class stacks) or gets a
// new slot, which is what equipment always does.
//
// HOW MANY AND HOW BIG: the per-slot cap is the CLASS's, read from the emitted
// catalog (`data\item_catalog.json`: WarpScroll = 32,767, equipment = 1). This
// program used to hardcode cap 1 for everything, so twenty warp scrolls arrived as
// twenty separate slots and read to the owner as "the game does not stack". A
// request for more than one slot's worth now fills full rows plus a remainder.
//
// Usage (from docker-port\goserver):
//
//	go run ./cmd/mintkit -commander 24 -items 7201,7202,7222,7203,15001,19124,19533,45011
//	go run ./cmd/mintkit -commander 24 -items 21006 -count 20
//
// Every minted id is printed, and the count is checked against the request: a
// run that minted 7 of 8 lines says so instead of looking successful.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"geserver/db"
	"geserver/item"
	"geserver/zone"
)

// catalogPath is where `src\emit_item_catalog.py` writes the item table. The
// default is relative to docker-port\goserver, which is where `go run ./cmd/mintkit`
// starts.
const catalogPath = "../data/item_catalog.json"

// mintRow is one BAG SLOT this give needs: how many go in it, and whether it
// joins a stack the commander already holds.
type mintRow struct {
	Count int64
	Merge bool
}

// mintPlan is one requested class resolved against the catalog.
type mintPlan struct {
	ClassID  int64
	Name     string // the catalog's spelling, "" when it does not know the class
	MaxStack int64  // the class's own per-slot cap, from the catalog
	Rows     []mintRow
}

// Rows counts the bag rows this plan writes.
func (p mintPlan) RowCount() int { return len(p.Rows) }

// planGive decides HOW MANY ROWS a request needs and WHAT COUNT each row gets.
//
// The cap belongs to the CLASS, never to this tool: item_catalog.json carries the
// client's own per-slot limit, and the store checks a give against the same number.
// A request bigger than one slot is split into full rows plus a remainder - it is
// not capped at one slot, and it is not refused for being large.
//
// Only the FIRST row may merge. Once it is full, a second give of the same class
// would find that full stack again and be refused, so the extra rows get their own
// slot (Merge false) - which is what a full stack means anyway.
//
// An UNKNOWN class is refused rather than guessed: with no cap, the write path
// would get a number nobody measured, and a wrong one either fails the whole
// transaction or writes a count the client cannot draw. The message says what the
// fallback would have been (cap 1, one slot per item), so the refusal is not silent.
func planGive(cat *item.Catalog, classID, count int64) (mintPlan, error) {
	if count < 1 {
		return mintPlan{}, fmt.Errorf("class %d: count %d is not a positive number", classID, count)
	}
	entry, known := cat.EntryOf(classID)
	if !known {
		return mintPlan{}, fmt.Errorf("class %d is not in the item catalog (%s): its per-slot cap is unknown, "+
			"so it would be minted one-per-slot (cap 1) - refusing to guess", classID, catalogPath)
	}

	perSlot := entry.MaxStack
	if perSlot < 1 {
		perSlot = 1 // a catalog row with no cap cannot mean "unlimited": the client draws one slot
	}

	plan := mintPlan{ClassID: classID, Name: cat.NameOf(classID), MaxStack: entry.MaxStack}
	for left := count; left > 0; {
		n := left
		if n > perSlot {
			n = perSlot
		}
		plan.Rows = append(plan.Rows, mintRow{
			Count: n,
			// The item table's word for the class, and only for the first row.
			Merge: entry.Mergeable && len(plan.Rows) == 0,
		})
		left -= n
	}
	return plan, nil
}

func main() {
	var (
		commander = flag.Int("commander", 0, "commander_id whose bag is filled (required)")
		items     = flag.String("items", "", "comma separated item CLASS ids to mint")
		count     = flag.Int64("count", 1, "how many of each class; split across slots at the class's own cap")
		dryRun    = flag.Bool("dry", false, "print what would be minted and write nothing")
	)
	flag.Parse()

	if *commander == 0 || *items == "" {
		flag.Usage()
		os.Exit(2)
	}

	var classes []int64
	for _, part := range strings.Split(*items, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			log.Fatalf("mintkit: %q is not an item class id: %v", part, err)
		}
		classes = append(classes, id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The cap and the stack rule come from the emitted item table. A missing file
	// is fatal HERE (unlike the zone, which can run without item counts): with no
	// catalog this program cannot know a cap, and the guess is what minted one
	// scroll per slot.
	catalog, err := item.Load(catalogPath)
	if err != nil {
		log.Fatalf("mintkit: %v - build the item table with: cd src && python emit_item_catalog.py", err)
	}

	// Resolve every class BEFORE touching the database, so an unknown class refuses
	// the whole run while the bag is still untouched.
	plans := make([]mintPlan, 0, len(classes))
	for _, class := range classes {
		plan, err := planGive(catalog, class, *count)
		if err != nil {
			log.Fatalf("mintkit: NOTHING was written: %v", err)
		}
		plans = append(plans, plan)
	}

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		log.Fatalf("mintkit: %v", err)
	}
	defer pool.Close()

	store := zone.NewPGStore(pool)

	// Show the bag BEFORE, so "nothing was removed" is a measured fact and not a
	// promise: the owner's rows are his.
	before, err := store.Stacks(ctx, int32(*commander))
	if err != nil {
		log.Fatalf("mintkit: read bag of commander %d: %v", *commander, err)
	}
	fmt.Printf("commander %d: bag holds %d row(s) BEFORE\n", *commander, len(before))
	for _, st := range before {
		fmt.Printf("   existing item_id %d class %d count %d\n",
			st.ItemID, st.ClassID, st.Count)
	}

	totalRows := 0
	for _, plan := range plans {
		totalRows += plan.RowCount()
		name := plan.Name
		if name == "" {
			name = "unnamed"
		}
		fmt.Printf("   class %d (%s) cap %d: %d row(s)", plan.ClassID, name, plan.MaxStack, plan.RowCount())
		for _, row := range plan.Rows {
			fmt.Printf(" %d", row.Count)
		}
		if plan.Rows[0].Merge {
			fmt.Print(" (first row joins an existing stack if one is held)")
		}
		fmt.Println()
	}

	if *dryRun {
		return
	}

	// ONE transaction for the whole kit: a half-minted kit would be a state nobody
	// asked for, and CommitTx already guarantees all-or-nothing.
	ops := make([]item.Op, 0, totalRows)
	for _, plan := range plans {
		for _, row := range plan.Rows {
			ops = append(ops, item.Op{
				Give:    true,
				ClassID: plan.ClassID,
				Count:   row.Count,
				// Merge/MaxStack are the ITEM TABLE's word for this class, not this
				// program's: that pair is exactly what a shop buy carries.
				Merge:    row.Merge,
				MaxStack: plan.MaxStack,
				Name:     fmt.Sprintf("mintkit class %d", plan.ClassID),
			})
		}
	}

	changes, err := store.CommitTx(ctx, int32(*commander), nil, ops)
	if err != nil {
		log.Fatalf("mintkit: commander %d: NOTHING was written: %v", *commander, err)
	}

	// Say WHICH rows were joined and which were new. A give that joins a held stack
	// adds no bag row, and calling that "minted 1 bag row" is how a correct merge got
	// read as a destroyed stack on 2026-10-30 - the owner saw "minted 1 row" and a
	// new item id, and had no way to tell a merge from a replacement.
	wasHeld := make(map[int64]bool, len(before))
	for _, st := range before {
		wasHeld[st.ItemID] = true
	}
	var joined, opened int
	for _, c := range changes {
		how := "NEW slot"
		if wasHeld[c.ItemID] {
			how = "JOINED the held stack"
			joined++
		} else {
			opened++
		}
		fmt.Printf("   item_id %d  class %d  count %d  %s\n", c.ItemID, c.ClassID, c.Total, how)
	}
	fmt.Printf("commander %d: %d item change(s) in one transaction (%d joined a held stack, %d new slot(s))\n",
		*commander, len(changes), joined, opened)
	if len(changes) != len(ops) {
		fmt.Fprintf(os.Stderr,
			"mintkit: asked for %d bag row(s) and got %d back - the kit is SHORT\n",
			len(ops), len(changes))
		os.Exit(1)
	}

	after, err := store.Stacks(ctx, int32(*commander))
	if err != nil {
		log.Fatalf("mintkit: read bag back: %v", err)
	}
	fmt.Printf("commander %d: bag holds %d row(s) AFTER (+%d)\n",
		*commander, len(after), len(after)-len(before))
}
