package zone

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"geserver/db"
	"geserver/item"
)

// testHandler is a zone with no data files and a quiet log, just enough to run a
// click through openNPCDialog.
func testHandler(click func(string, ClickObject, ClickObject) (ClickReply, error)) (*Handler, *Session) {
	h := &Handler{
		Log:   log.New(io.Discard, "", 0),
		NPCs:  []NPC{{Handle: 0x40000031, NameToken: "<$>1</>", Script: "SHOP"}},
		Click: click,
	}
	sess := &Session{CommanderID: 24, Pos: [3]int32{1000, 1000, 0}}
	return h, sess
}

// TestCommittedLockPropertyIsPushedToTheClient is the rule that makes a
// transaction VISIBLE: the client caches its Lock_* properties, so a database
// write alone leaves the window showing the old number. `Lock_InventoryCount` is
// the bag's capacity, and 3748 is the only way it moves.
func TestCommittedLockPropertyIsPushedToTheClient(t *testing.T) {
	var gotCommander int32
	h, sess := testHandler(func(entry string, self, pc ClickObject) (ClickReply, error) {
		gotCommander = pc.Commander
		return ClickReply{
			DialogKind: ClickDialogOK,
			Text:       "bought",
			Properties: []Property{
				{Name: "Lock_InventoryCount", Value: 300},
				{Name: "Cwar_Mining", Value: 5},
			},
		}, nil
	})

	packets := h.openNPCDialog(sess, h.NPCs[0])
	if gotCommander != 24 {
		t.Fatalf("the click saw commander %d, want 24 - a transaction would write to nobody",
			gotCommander)
	}

	var props [][]byte
	for _, p := range packets {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCUpdatePropertyNum {
			props = append(props, p)
		}
	}
	// ONE: `Cwar_Mining` is a quest counter, not a cached window value, and the
	// client has no row for it in this packet's idSpace.
	if len(props) != 1 {
		t.Fatalf("%d property packet(s), want exactly the Lock_ one", len(props))
	}
	if len(props[0]) != 46 {
		t.Fatalf("property packet is %d bytes, want 46", len(props[0]))
	}
	if got := trimNul(props[0][22:42]); got != "Lock_InventoryCount" {
		t.Fatalf("property = %q, want Lock_InventoryCount", got)
	}
	if got := binary.LittleEndian.Uint32(props[0][42:]); got != 300 {
		t.Fatalf("value = %d, want the committed 300", got)
	}

	// The property packet goes out BEFORE the box: the Python oracle flushes it
	// first (`flush_property_events`), and a modal box must not swallow it.
	if binary.LittleEndian.Uint16(packets[0]) != CmdZCUpdatePropertyNum {
		t.Fatalf("first packet is %d, want the property push before the dialog",
			binary.LittleEndian.Uint16(packets[0]))
	}
}

// TestPlainPropertySendsNothingToTheClient: most commits move a quest counter,
// which no packet carries yet (that is the quest array, 3872). Sending a 3748
// for one would be inventing a cache entry the client cannot use.
func TestPlainPropertySendsNothingToTheClient(t *testing.T) {
	h, sess := testHandler(func(string, ClickObject, ClickObject) (ClickReply, error) {
		return ClickReply{
			DialogKind: ClickDialogOK,
			Text:       "tallied",
			Properties: []Property{{Name: "Cwar_Mining", Value: 5}},
		}, nil
	})

	for _, p := range h.openNPCDialog(sess, h.NPCs[0]) {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCUpdatePropertyNum {
			t.Fatal("a non-Lock property was pushed to the client")
		}
	}
}

// TestPropertyRoundTripInPostgres proves the SQL against the real schema, which
// a mock would not: the upsert, the section it lands in, and the read-back.
//
// It is GUARDED, because it WRITES: only a run that asked for it (`GE_TX_DB_TEST=1`)
// touches the database, and it restores what it wrote. The rule is a backup
// before a run that writes, and an accidental `go test ./...` must not be one.
func TestPropertyRoundTripInPostgres(t *testing.T) {
	if os.Getenv("GE_TX_DB_TEST") == "" {
		t.Skip("set GE_TX_DB_TEST=1 to run the database round trip (it writes a row)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL (%v)", err)
	}
	defer pool.Close()

	commanderID := testItemCommander(t, ctx, pool)

	store := NewPGStore(pool)
	const name = "Test_PropRoundTrip"

	// The row this test writes, AS IT WAS: absent on a clean database, but a
	// killed earlier run could have left one, and "restore" means put that value
	// back, not delete the row.
	var beforeValue string
	var beforeRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(value), '')
		   FROM commander_property WHERE commander_id = $1 AND property = $2`,
		commanderID, name).Scan(&beforeRows, &beforeValue); err != nil {
		t.Fatalf("read the pre-test %s row: %v", name, err)
	}
	// Never leave a row behind, whatever happens below.
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if beforeRows == 0 {
			_, _ = pool.Exec(bg,
				`DELETE FROM commander_property WHERE commander_id=$1 AND property=$2`,
				commanderID, name)
			return
		}
		_, _ = pool.Exec(bg,
			`UPDATE commander_property SET value = $3
			  WHERE commander_id = $1 AND property = $2`, commanderID, name, beforeValue)
	}()

	if _, err := store.CommitTx(ctx, commanderID, map[string]int64{name: 1234}, nil); err != nil {
		t.Fatalf("CommitTx: %v", err)
	}
	props, err := store.CommanderProps(ctx, commanderID)
	if err != nil {
		t.Fatalf("CommanderProps: %v", err)
	}
	if got := props[name]; got != 1234 {
		t.Fatalf("read back %d, want 1234", got)
	}

	// Second write must OVERWRITE, not add a second row: the primary key is
	// (commander_id, property, section), and two rows per name would make a
	// read-back depend on row order.
	if _, err := store.CommitTx(ctx, commanderID, map[string]int64{name: 4321}, nil); err != nil {
		t.Fatalf("second CommitTx: %v", err)
	}
	props, err = store.CommanderProps(ctx, commanderID)
	if err != nil {
		t.Fatalf("CommanderProps: %v", err)
	}
	if got := props[name]; got != 4321 {
		t.Fatalf("after overwrite read %d, want 4321", got)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM commander_property WHERE commander_id=$1 AND property=$2`,
		commanderID, name).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows for one property, want 1", rows)
	}
}

// TestCommittedItemIsAnnouncedIncrementally is the end of the item loop: a
// committed give must reach the client as ZC_ADDITEM addressed to a position the
// client already knows, BEFORE the dialog, and with the new total.
func TestCommittedItemIsAnnouncedIncrementally(t *testing.T) {
	h, sess := testHandler(func(string, ClickObject, ClickObject) (ClickReply, error) {
		return ClickReply{
			DialogKind: ClickDialogOK,
			Text:       "here you go",
			Items: []item.Change{
				{Give: true, ClassID: 21613, ItemID: 555, Total: 5},
			},
		}, nil
	})
	// One item already in the bag, so the give APPENDS at position 2.
	sess.Inv = NewInvMirror([]item.Stack{{ItemID: 100, ClassID: 30000, Count: 500}})

	packets := h.openNPCDialog(sess, h.NPCs[0])
	if len(packets) < 2 {
		t.Fatalf("%d packet(s), want the item packet and the dialog", len(packets))
	}
	if cmd := binary.LittleEndian.Uint16(packets[0]); cmd != CmdZCAddItem {
		t.Fatalf("first packet is %d, want ZC_ADDITEM before the dialog", cmd)
	}
	if idx := binary.LittleEndian.Uint32(packets[0][8:]); idx != 2 {
		t.Fatalf("announced at position %d, want 2", idx)
	}
	if count := binary.LittleEndian.Uint32(packets[0][12:]); count != 5 {
		t.Fatalf("count = %d, want the new total 5", count)
	}
	if packets[0][25] != 0 {
		t.Fatalf("addType = %d, want 0 (ADD_ITEM) - a grant must print 'obtained', "+
			"never 'purchased'", packets[0][25])
	}
}

// TestItemChangeWithoutAMirrorIsLoggedNotGuessed: a session that never read a bag
// has no positions, so nothing is sent - a guessed index would edit the wrong row.
func TestItemChangeWithoutAMirrorIsLoggedNotGuessed(t *testing.T) {
	h, sess := testHandler(func(string, ClickObject, ClickObject) (ClickReply, error) {
		return ClickReply{
			DialogKind: ClickDialogOK,
			Text:       "ok",
			Items:      []item.Change{{Give: true, ClassID: 21613, ItemID: 555, Total: 5}},
		}, nil
	})

	for _, p := range h.openNPCDialog(sess, h.NPCs[0]) {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == CmdZCAddItem {
			t.Fatal("an item packet was sent with no bag mirror to address it")
		}
	}
}
