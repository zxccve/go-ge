package zone

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// THE SAVED-POINT TABLE: one row per slot per commander PER LIST, and there are four
// lists (teltype 0..3) - forty points, all free (R76 slice 6,
// db\migrations\00012_saved_teleport_teltype.sql).
//
// The wire side (what the client sends and what it is answered with) is
// saved_teleport.go. This file is only the database half, and it owns the ONE
// decision the client cannot make for us: WHICH SLOT a saved point goes into, in the
// list the client named.
//
// THE DISPLAY ORDER IS NOT THE SLOT (changed 2026-10-30, R76 slice 2). The window
// draws its rows in SAVE ORDER, oldest first: a save appends to the bottom, a delete
// closes the gap on screen only, and the Up/Down buttons (3639) move a row. `slot` is
// an internal storage key that is never renumbered and never shown, and `display_order`
// (db\migrations\00011_saved_teleport_order.sql) is the only thing the read orders by.
// Since slice 6 that order is PER LIST: each of the four panels has its own sequence,
// its own slots and its own cap, so the read orders by (teltype, display_order, slot).
// Before this, the read was `ORDER BY slot` and a save took the lowest free slot, so a
// deleted row was refilled by the next save and the new point appeared in the middle of
// the window - the owner saw exactly that in game (logs\zone15_live.log 15:09:57..15:10:07).
//
// WHY THE SERVER CHOOSES THE SLOT. `CZ_Teleport_Save` carries no slot at all - the
// client's own sender fills one byte and it is the CHARACTER index:
//
//	source2\geClient\Source\GENormalSendPacket.cpp:621
//	  void packet::CZ_Teleport_Save(int index)
//	  { CHECK_SEND_CONDITION; PKT_DECL(CZ_TELEPORT_SAVE); packet.charIndex = index; }
//	source2\geClient\Source\script.cpp:1337
//	  void TeleportSave(int myPcIndex) { packet::CZ_Teleport_Save(myPcIndex); }
//	ge\ui\uiscp\game.scp:133
//	  func UI_TELEPORT_SAVE(frame, control, arg1, arg2) { TeleportSave(GetUiIndex(uiFrame)); }
//
// so "save" means "put where I stand into my list" and the position of the point in
// that list is the SERVER's choice. The original server did the same thing, one
// layer up: it appended to an in-memory list and gave the point the next free
// teleport id (source2\geServer\ZServerLib\Commander.cpp:4917-4951,
// CTeleportInfo::GetNewWarpID).
//
// WHAT THE ORIGINAL DID DIFFERENTLY, and why this refuses instead. When the list was
// FULL the original deleted its OLDEST point and saved the new one in its place
// (Commander.cpp:4939-4946), without asking. Losing a point the player saved is not
// something a rebuild should do silently, so a full list here REFUSES with a log
// line, and the player keeps every point they had.
//
// THE TWO POSITIONS THAT COME BACK. `SavedPoint.Pos` is {x, y, CELL}, the same triple
// every other position in this server is stored as (see the migration's comment).
// There is no height in it, and there cannot be: the client's travel path takes a
// navmesh CELL and drops the character through the world when it names one that is
// not on the destination mesh - which is why `zone\gate.go` refuses such a cell
// rather than sending the move.

// MaxSavedTeleports is how many slots ONE teleport list holds: TEN.
//
// THE CAP IS PER LIST, and there are FOUR lists (teltype 0..3), so a commander has
// forty points. That is not a cash figure in either direction: the client's own
// `COMMWARP_SAVE` picks the first panel holding fewer than ten rows
// (ge\ui\uiscp\game.scp, `for(telType = 0, 3) { if( CommWarpCount(telType) < 10 ) ... }`)
// and checks no item at all. The owner's ruling: "nothing was paid lol. just serve all
// 40. it was supposed to be free to add to the entry. u only need those cash item to
// ENABLED that slot extension." The cash item on the reference only unlocked the extra
// panels; this rebuild serves all four of them open.
//
// WHERE TEN COMES FROM. The owner photographed the LIVE reference server's Warp List
// window: ten points in the basic "Warp List" panel, and the "Warp Extend(1)" panel
// beside it holds more. Ten is also the literal in the client's own loop above, so the
// number the client caps each panel at and the number this server enforces are the
// same one, and it is written once (here).
//
// THE CAP IS STATED TO THE CLIENT, and that statement is not what draws the rows.
// The 3748 `Lock_Warp` packet is sent because it is the contract the original server
// had (it read the property back for its own cap: `if (teltype == 0) maxCount =
// GetLock_Warp();`, Commander.cpp:4895). The SHIPPED CLIENT HAS NO READER for
// `Lock_Warp` - measured in R76 slice 3: `ge.exe` carries `Lock_MCCLv` and
// `Lock_InventoryCount` as strings but not `Lock_Warp`, and `warplist.xml` has no
// count text at all. The rows the client draws are the 3634 rows this server sends, so
// ten rows per list here IS ten rows on screen; the "used / total" text belongs to the
// separate `teleportation` window and its total is the Teleportation SKILL level.
// `Lock_Warp` describes ONE list's size and is still sent once (zone\burst.go ->
// PropsWithWarpCap); there is no per-type property, because the client's own script
// caps types 1-3 at the same ten.
const MaxSavedTeleports = 10

// SavedTeleportLists is how many teleport lists a commander has: the client's
// MAX_TELEPORT_TYPE, teltype 0..3. Packet 3634 is sent once per list.
const SavedTeleportLists = 4

// SavedPoint is one row of `saved_teleport`, in the shape the wire wants.
type SavedPoint struct {
	// TelType is WHICH list the point belongs to, 0..3: the Warp List window draws four
	// panels (the basic one, then "Warp Extend(1)".."(3)") and each is its own list of
	// ten. It is the packet's teltype byte and nothing else - a point has no other
	// notion of "which panel".
	TelType int
	Slot    int
	MapID   int
	// Pos is {x, y, navmesh cell}. Index 2 is a CELL, never a height.
	Pos [3]int32
	// Order is the row's position in ITS OWN list, oldest save first. It is the
	// `display_order` column and it is NOT on the wire: the client names a row by its
	// index in the list it was last sent, so the slice it is sent in IS the order. Two
	// lists may hold the same `Order` numbers at the same time, which is why the column
	// is read together with `TelType`.
	Order int
	// Name is the point's memo, and it DOES go on the wire - in the Warp List's
	// record only. 3634 ZC_COMMWARP_LIST carries a 32-byte memo at record +14 and
	// the client draws "(n) memo", or "(n) <map name>" when it is empty
	// (UIWarpListFrame.cpp:53-60). The `teleportation` window's 3642 record is the
	// one that carries no memo: it is 14 bytes of position and map id, and that
	// window builds its row text from the map table instead. Same table, two
	// windows, one of them shows the name.
	Name string
}

// rowsOfType returns the rows of ONE teleport list, in the order they were given.
//
// Every caller needs it, because the store reads ALL of a commander's points in one
// statement (the duplicate rule needs the whole set) while each window draws ONE list:
// the Warp List sends four 3634 packets from one read, and the `teleportation` window
// (3642) is teltype 0 only. Nothing here sorts - the store's read is already ordered
// by (teltype, display_order, slot), so filtering keeps the display order.
func rowsOfType(rows []SavedPoint, telType int) []SavedPoint {
	var out []SavedPoint
	for _, p := range rows {
		if p.TelType == telType {
			out = append(out, p)
		}
	}
	return out
}

// validTelType says whether a list number is one the client can draw. The four panels
// are teltype 0..3; anything else is a broken client, not a fifth list.
func validTelType(telType int) bool {
	return telType >= 0 && telType < SavedTeleportLists
}

// errNoFreeSlot is the "this LIST is full" refusal, separately named so a caller can
// tell it from a database failure. The cap counts ROWS IN ONE LIST: ten rows are ten,
// whatever slot numbers they happen to hold, and the other three lists are unaffected.
var errNoFreeSlot = errors.New("every slot of that list is taken")

// errNoSuchRow is the "that display index is not a row" refusal of a move. The caller
// checks the index against the list it just read, so this only fires on a race - and a
// race must change nothing.
var errNoSuchRow = errors.New("no saved point at that display index")

// errDuplicatePoint is the "you already have a point at that spot" refusal of a save
// (R76 slice 5). It is the ONE rule the client cannot be trusted with, even though it
// warns on its own: the client compares against the list it was LAST SENT, so two
// clients, a stale list or a refused refresh all get past it. The server owns the list,
// so the server enforces it.
//
// THE KEY IS THE COORDINATES, NEVER THE NAME AND NEVER THE LIST. The owner's words:
// "im talking about coordinates and not names. the names does not matter if
// duplicate." Two points with the same map_id + x + y + cell are the same spot,
// whatever they are called AND whichever of the four panels holds them, so the second
// save is refused. The reference server's line - "This warp point has already been
// saved." - names no panel either. The same x/y/cell on a DIFFERENT map is a different
// place and is allowed - which is exactly why this is NOT a unique index: coordinates
// are not a key, and a player may legitimately hold (2209, 16830) on map 15 and on
// map 10.
var errDuplicatePoint = errors.New("a saved point already stands at that spot on that map")

// sameSpot is the WHOLE duplicate rule in one place: two saved points are the same
// point when they name the same MAP and the same X, Y and CELL. Nothing else is
// compared - not the NAME, not the TelType, not the Slot. The same spot saved twice
// under two memos is still one spot, and so is the same spot saved once into the basic
// list and once into "Warp Extend(2)": the forty points are one collection of places,
// the panels are only where they are filed.
//
// It is a plain comparison on purpose: the store calls it inside its locked read, and a
// test can call it with no database at all, so the rule cannot drift from the test that
// pins it.
func sameSpot(a, b SavedPoint) bool {
	return a.MapID == b.MapID && a.Pos == b.Pos
}

// SavedTeleports reads one commander's saved points - ALL FOUR LISTS - in panel order,
// and inside each panel in DISPLAY ORDER (oldest save first), which is the order the
// window draws them in.
//
// It is NOT slot order, and the flat result is NOT one list: the rows carry `TelType`
// and `Order`, and a caller that draws one window filters with rowsOfType. One read for
// all four lists is what the duplicate rule needs (the same spot anywhere in the forty
// is refused), and the Warp List answers four 3634 packets from this one answer.
// `slot` breaks a tie only, and cannot be needed while (teltype, display_order) is
// unique per commander - it is there so the answer is never random.
func (s *PGStore) SavedTeleports(ctx context.Context, commanderID int32) ([]SavedPoint, error) {
	if commanderID <= 0 {
		return nil, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT teltype, slot, map_id, x, y, cell, name, display_order
		   FROM saved_teleport
		  WHERE commander_id = $1
		  ORDER BY teltype, display_order, slot`, commanderID)
	if err != nil {
		return nil, fmt.Errorf("read saved points of commander %d: %w", commanderID, err)
	}
	defer rows.Close()

	var out []SavedPoint
	for rows.Next() {
		var p SavedPoint
		if err := rows.Scan(&p.TelType, &p.Slot, &p.MapID, &p.Pos[0], &p.Pos[1],
			&p.Pos[2], &p.Name, &p.Order); err != nil {
			return nil, fmt.Errorf("read saved points of commander %d: %w",
				commanderID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read saved points of commander %d: %w", commanderID, err)
	}
	return out, nil
}

// SaveSavedTeleport writes p into a slot OF ONE LIST and reports the row it wrote,
// including the slot it chose and its new position in that list.
//
// `telType` is the list the client chose (its 3635 carries it at +10): 0..3, one per
// panel of the Warp List window. A save only ever fills the named list - the other
// three are untouched, which is what makes forty points four independent lists of ten.
//
// `slot` is -1 for "any free slot", which is what a client save asks for (the packet
// cannot name one - see the file comment). A non-negative slot is honoured only when
// it is a real slot AND it is free; that form exists for the tests and for a later
// "save into the selected row" feature.
//
// THE NEW ROW GOES TO THE BOTTOM OF ITS LIST, whatever slot it lands in. Its
// display_order is one past the highest in use IN THAT LIST, so the client draws it
// last - which is what "insert the new entry to the bottom" means to the player.
// Picking the lowest free slot is only about the storage key, and no other row is
// renumbered: a second source of truth is exactly what made a delete's hole get
// refilled in the middle of the window (db\migrations\00011_saved_teleport_order.sql).
//
// THE CAP IS TEN ROWS PER LIST. Ten rows fill THAT list and nothing else: a full basic
// list still accepts a save into "Warp Extend(1)", which is the whole point of forty
// points.
//
// A SPOT ALREADY IN THE COMMANDER'S FORTY IS REFUSED (R76 slice 5, widened to all four
// lists in slice 6). A point whose map_id, x, y AND cell all match a point this
// commander holds IN ANY LIST is a duplicate, and nothing is written - see
// errDuplicatePoint for why the name and the list are not part of the test. The check
// runs INSIDE this transaction, against the same locked read that picks the slot, so
// two saves racing on one spot cannot both land: the second one's read either sees the
// first one's row or waits for it. A re-read before the insert is enough here; a unique
// index would be wrong, because coordinates are not a key.
//
// One transaction, and the commander's existing rows are locked while it runs: two
// saves from two characters of the same commander must not both land in the same slot,
// nor both take the same display position.
func (s *PGStore) SaveSavedTeleport(ctx context.Context, commanderID int32, telType, slot int, p SavedPoint) (SavedPoint, error) {
	if commanderID <= 0 {
		return SavedPoint{}, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if !validTelType(telType) {
		return SavedPoint{}, fmt.Errorf("teleport list %d is not one of the %d the client "+
			"draws (0..%d)", telType, SavedTeleportLists, SavedTeleportLists-1)
	}
	if p.MapID <= 0 {
		return SavedPoint{}, fmt.Errorf("map id %d is not a map", p.MapID)
	}
	if !PlausibleCell(p.Pos[2]) {
		return SavedPoint{}, fmt.Errorf("saved point (%d, %d): %v", p.Pos[0], p.Pos[1],
			badCellError(p.Pos[2]))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SavedPoint{}, fmt.Errorf("begin save of a teleport point for commander %d: %w",
			commanderID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// EVERY list's taken slots, every stored spot, and the highest display position in
	// THIS list, then the first gap. Reading them under a lock on the commander's own
	// rows is what makes "first free", "one past the end" and "already saved there" the
	// same answer twice in a row.
	rows, err := tx.Query(ctx,
		`SELECT teltype, slot, display_order, map_id, x, y, cell FROM saved_teleport
		  WHERE commander_id = $1 FOR UPDATE`,
		commanderID)
	if err != nil {
		return SavedPoint{}, fmt.Errorf("read the taken slots of commander %d: %w",
			commanderID, err)
	}
	taken := map[int]bool{}
	lastOrder := -1
	duplicate := false
	for rows.Next() {
		var list, got, ord, mapID int
		var x, y, cell int32
		if err := rows.Scan(&list, &got, &ord, &mapID, &x, &y, &cell); err != nil {
			rows.Close()
			return SavedPoint{}, fmt.Errorf("read the taken slots of commander %d: %w",
				commanderID, err)
		}
		// Slots are per list: another panel's slot 3 does not make this panel's slot 3
		// taken, and another panel's display_order is a different sequence.
		if list == telType {
			taken[got] = true
			if ord > lastOrder {
				lastOrder = ord
			}
		}
		// THE DUPLICATE TEST: all four of map, x, y and cell, through sameSpot - which
		// does not look at the name OR at the list, on purpose. A spot is already saved
		// if it is anywhere in the commander's forty points.
		if sameSpot(SavedPoint{MapID: mapID, Pos: [3]int32{x, y, cell}}, p) {
			duplicate = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SavedPoint{}, fmt.Errorf("read the taken slots of commander %d: %w",
			commanderID, err)
	}

	// THE DUPLICATE IS ANSWERED FIRST, before the cap: on a full list "remove a point
	// to make room" would be useless advice for a spot that is already saved - removing
	// any point would not make this save legal.
	if duplicate {
		return SavedPoint{}, errDuplicatePoint
	}

	// THE CAP IS A COUNT OF ROWS IN THIS LIST, checked before a slot is chosen: ten
	// rows fill it even if a move or an old save left unusual slot numbers behind.
	if len(taken) >= MaxSavedTeleports {
		return SavedPoint{}, errNoFreeSlot
	}

	chosen := -1
	switch {
	case slot >= 0:
		// An explicit slot: honoured only when it is a real slot and it is free.
		if slot >= MaxSavedTeleports {
			return SavedPoint{}, fmt.Errorf("slot %d is outside 0..%d",
				slot, MaxSavedTeleports-1)
		}
		if taken[slot] {
			return SavedPoint{}, fmt.Errorf("slot %d is already taken", slot)
		}
		chosen = slot
	default:
		for i := 0; i < MaxSavedTeleports; i++ {
			if !taken[i] {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			return SavedPoint{}, errNoFreeSlot
		}
	}

	p.TelType = telType
	p.Slot = chosen
	p.Order = lastOrder + 1

	// A SECOND LOOK, immediately before the INSERT - the "re-read before insert" this
	// rule needs. The locked read above only locks rows that EXIST, so two saves on one
	// empty spot can both see "no duplicate" and both insert. This statement takes a NEW
	// snapshot (READ COMMITTED), so a row committed in between IS seen here and the
	// second save is refused.
	//
	// No teltype in the WHERE on purpose: the duplicate is refused across all four
	// lists. And it is deliberately not a unique index: coordinates are not a key, and
	// a player may legitimately hold the same x/y/cell on two maps.
	var clash bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM saved_teleport
		      WHERE commander_id = $1 AND map_id = $2 AND x = $3 AND y = $4 AND cell = $5)`,
		commanderID, p.MapID, p.Pos[0], p.Pos[1], p.Pos[2]).Scan(&clash); err != nil {
		return SavedPoint{}, fmt.Errorf("re-read the spots of commander %d: %w",
			commanderID, err)
	}
	if clash {
		return SavedPoint{}, errDuplicatePoint
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO saved_teleport
		        (commander_id, teltype, slot, map_id, x, y, cell, name, display_order)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		commanderID, p.TelType, p.Slot, p.MapID, p.Pos[0], p.Pos[1], p.Pos[2], p.Name,
		p.Order); err != nil {
		return SavedPoint{}, fmt.Errorf("write list %d slot %d of commander %d: %w",
			p.TelType, p.Slot, commanderID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SavedPoint{}, fmt.Errorf("commit list %d slot %d of commander %d: %w",
			p.TelType, p.Slot, commanderID, err)
	}
	return p, nil
}

// RenameSavedTeleport changes one slot's `name` (the memo) in one list, and nothing
// else.
//
// It exists for the WARP LIST's 3638, not for the 3640 family: that window draws
// each row's memo out of the 3634 record, so renaming a point is a single UPDATE and
// the position, the map and the slot stay exactly where they were. A rename cannot
// move a point, which is why nothing but `name` is written here. `telType` is the list
// the client named at +11 and `slot` is the storage key of the row, already translated
// from the display index by the caller.
//
// found is false when the commander has no row in that slot OF THAT LIST - not an
// error, and not a reason to invent one. The row is returned so the log can name what
// it was called.
func (s *PGStore) RenameSavedTeleport(ctx context.Context, commanderID int32, telType, slot int, name string) (SavedPoint, bool, error) {
	if commanderID <= 0 {
		return SavedPoint{}, false, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if !validTelType(telType) {
		return SavedPoint{}, false, fmt.Errorf("teleport list %d is not one of the %d the "+
			"client draws", telType, SavedTeleportLists)
	}
	if slot < 0 || slot >= MaxSavedTeleports {
		return SavedPoint{}, false, fmt.Errorf("slot %d is outside 0..%d", slot,
			MaxSavedTeleports-1)
	}
	var p SavedPoint
	err := s.pool.QueryRow(ctx,
		`UPDATE saved_teleport
		    SET name = $4
		  WHERE commander_id = $1 AND teltype = $2 AND slot = $3
		 RETURNING teltype, slot, map_id, x, y, cell, name`,
		commanderID, telType, slot, name).
		Scan(&p.TelType, &p.Slot, &p.MapID, &p.Pos[0], &p.Pos[1], &p.Pos[2], &p.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return SavedPoint{}, false, nil
	case err != nil:
		return SavedPoint{}, false, fmt.Errorf("rename list %d slot %d of commander %d: %w",
			telType, slot, commanderID, err)
	}
	return p, true, nil
}

// MoveSavedTeleport is the Warp List's 3639 SWAP, done as a MOVE inside ONE list: the
// row at display index `from` is taken out of THAT list and spliced back in at `to`.
// Every other row of the list keeps its relative order, so moving the last row up one
// step swaps two neighbours and nothing else moves. The other three lists are not
// touched at all.
//
// THE INDEX IS THE DISPLAY INDEX OF THE NAMED LIST, which is the ONLY order the client
// knows: it sends the row numbers it drew, so they are positions in that list - never
// slots, and never a position in some flat forty-row collection (see commwarp.go's 3639
// note for the captured bytes).
//
// Why slots are not touched: the slot is half of the primary key, and reassigning keys
// to move rows is how a second source of truth gets born. The move rewrites
// `display_order` only, in one transaction, one UPDATE per row - at most ten.
//
// The returned slice is the commander's WHOLE row set in canonical order, not just the
// moved list: the next 3634 draw needs all four lists, and returning them from the
// transaction that changed them is one source of truth instead of a re-read that could
// disagree.
func (s *PGStore) MoveSavedTeleport(ctx context.Context, commanderID int32, telType, from, to int) ([]SavedPoint, error) {
	if commanderID <= 0 {
		return nil, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if !validTelType(telType) {
		return nil, fmt.Errorf("teleport list %d is not one of the %d the client draws",
			telType, SavedTeleportLists)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the move of a saved point for commander %d: %w",
			commanderID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT teltype, slot, map_id, x, y, cell, name, display_order
		   FROM saved_teleport
		  WHERE commander_id = $1
		  ORDER BY teltype, display_order, slot
		    FOR UPDATE`, commanderID)
	if err != nil {
		return nil, fmt.Errorf("read saved points of commander %d: %w", commanderID, err)
	}
	var all []SavedPoint
	for rows.Next() {
		var p SavedPoint
		if err := rows.Scan(&p.TelType, &p.Slot, &p.MapID, &p.Pos[0], &p.Pos[1],
			&p.Pos[2], &p.Name, &p.Order); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read saved points of commander %d: %w",
				commanderID, err)
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read saved points of commander %d: %w", commanderID, err)
	}

	// The move happens inside the ONE list the client named. Its rows come out of the
	// flat read already in that list's display order.
	list := rowsOfType(all, telType)
	if from < 0 || from >= len(list) || to < 0 || to >= len(list) {
		return nil, fmt.Errorf("rows %d -> %d of a %d-row list %d: %w", from, to,
			len(list), telType, errNoSuchRow)
	}
	moved := list[from]
	spliced := make([]SavedPoint, 0, len(list))
	spliced = append(spliced, list[:from]...)
	spliced = append(spliced, list[from+1:]...)
	spliced = append(spliced, SavedPoint{})
	copy(spliced[to+1:], spliced[to:])
	spliced[to] = moved

	for i, p := range spliced {
		if p.Order == i {
			continue // already in place: a move of neighbours rewrites one number
		}
		if _, err := tx.Exec(ctx,
			`UPDATE saved_teleport SET display_order = $4
			  WHERE commander_id = $1 AND teltype = $2 AND slot = $3`,
			commanderID, telType, p.Slot, i); err != nil {
			return nil, fmt.Errorf("move list %d slot %d to display position %d of "+
				"commander %d: %w", telType, p.Slot, i, commanderID, err)
		}
		spliced[i].Order = i
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the move of list %d slot %d of commander %d: %w",
			telType, moved.Slot, commanderID, err)
	}

	// THE WHOLE ROW SET GOES BACK, with the moved list in its new order and the other
	// three lists exactly as they were read. The caller's next 3634 draw needs all four,
	// and handing it only the moved list would draw the other three panels EMPTY on the
	// client - they are re-sent, not merged.
	out := make([]SavedPoint, 0, len(all))
	for _, p := range all {
		if p.TelType != telType {
			out = append(out, p)
		}
	}
	out = append(out, spliced...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TelType != out[j].TelType {
			return out[i].TelType < out[j].TelType
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Slot < out[j].Slot
	})
	return out, nil
}

// RemoveSavedTeleport deletes one slot of one list and reports the row it deleted.
//
// found is false when the commander had no row there - which is NOT an error: it is
// what a second click on the same delete button looks like. The row is returned
// because its map and position are what the log line names.
//
// A remove does NOT renumber anything, in this list or any other: the hole it leaves in
// `display_order` is closed by the ORDER BY on the next read, and no other row is
// written. `slot` was never the order, so nothing that a player can see changes except
// the missing row.
func (s *PGStore) RemoveSavedTeleport(ctx context.Context, commanderID int32, telType, slot int) (SavedPoint, bool, error) {
	if commanderID <= 0 {
		return SavedPoint{}, false, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	if !validTelType(telType) {
		return SavedPoint{}, false, fmt.Errorf("teleport list %d is not one of the %d the "+
			"client draws", telType, SavedTeleportLists)
	}
	if slot < 0 || slot >= MaxSavedTeleports {
		return SavedPoint{}, false, fmt.Errorf("slot %d is outside 0..%d", slot,
			MaxSavedTeleports-1)
	}
	var p SavedPoint
	err := s.pool.QueryRow(ctx,
		`DELETE FROM saved_teleport
		  WHERE commander_id = $1 AND teltype = $2 AND slot = $3
		 RETURNING teltype, slot, map_id, x, y, cell, name`,
		commanderID, telType, slot).
		Scan(&p.TelType, &p.Slot, &p.MapID, &p.Pos[0], &p.Pos[1], &p.Pos[2], &p.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return SavedPoint{}, false, nil
	case err != nil:
		return SavedPoint{}, false, fmt.Errorf("delete list %d slot %d of commander %d: %w",
			telType, slot, commanderID, err)
	}
	return p, true, nil
}
