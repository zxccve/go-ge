package item

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// A class id -> the SLOT(S) that class can be worn in.
//
// WHY A DATA FILE: which slot a weapon or a piece of armour belongs to is the
// game's own item table's word, and the zone has no client tables. The client
// sends CZ_EQUIP with the slot it THINKS the item goes in, and the shipped server
// takes it as given - it only moves the item to its own default slot when the two
// disagree (EquipSlotControl.cpp:276-280). This zone refuses the disagreement
// instead, so it has to know the real slot: `src\emit_item_equip.py` writes it
// once (`data\item_equip.json`, 13,814 classes, 158 KB) and this file loads it.
// Same arrangement as the name -> class id catalog, and for the same reason.
//
// THE EQUIP_SPOT NUMBERING, in one place. It is the index into the appearance
// block's equipment array (START_INFO +68, stride 12) and the `spot` column of
// `wear_item` - the same number, never remapped (source2\geShared\include\
// geSharedTypes.h:7-30):
//
//	 0 HEAD      1 BODY      2 FACE      3 NECK      4 LHAND     5 RHAND
//	 6 RING1     7 RING2     8 GLOVE     9 BOOTS    10 BACK     11 COSTUME
//	12 EAR      13 BELT     14 BREAST   15 SHOULDER 16 ACHIEVE  17 ASSIST
//	18 LCOS     19 RCOS     20 ARTIFACT            (21 = equipSpotLast)
//
// The emitted file stores two VALUES that are not a spot of their own, because
// the game lets the item go in one of two places and pick its free hand/finger
// itself:
//
//	100  EqpEitherHand  -> spots 4 or 5 (LHAND / RHAND)
//	101  EqpEitherRing  -> spots 6 or 7 (RING1 / RING2)
//
// Anything else in the file IS the spot.
const (
	// EqpEitherHand is the emitted slot for a one-handed weapon or a shield: the
	// game decides which hand, and the client names the hand it chose in the
	// packet, so both 4 (LHAND) and 5 (RHAND) are allowed.
	EqpEitherHand = 100
	// EqpEitherRing is the emitted slot for a ring: spots 6 (RING1) and 7
	// (RING2) are the same slot as far as the item table is concerned.
	EqpEitherRing = 101

	// equipSpotLast is how many appearance slots the client has (0..20).
	equipSpotLast = 21

	// The four spots the two "either" values stand for.
	spotLeftHand  = 4
	spotRightHand = 5
	spotRing1     = 6
	spotRing2     = 7
)

// EquipSlots is the class id -> wearable slot table, loaded once at zone start.
//
// The ZERO VALUE knows nothing, and every lookup on it answers false: a missing
// table must never read as "every class is wearable", which would let any bag
// class be put in any slot - the whole check this type exists for.
type EquipSlots struct {
	slots map[int64]int
}

// equipDoc is the emitted JSON:
//
//	{"count":N,"slots":{"<classid>":<slot>},"note":"..."}
//
// `note` is the emitter's own explanation of 100/101 and is not read here.
type equipDoc struct {
	Count int            `json:"count"`
	Slots map[string]int `json:"slots"`
}

// LoadEquipSlots reads the emitted class -> slot table.
//
// A missing or empty file is an ERROR, not an empty table: an empty one refuses
// every equip, which looks exactly like "the client cannot equip anything" and
// hides the real cause (a file that was never written). The caller turns the
// error into one startup log line naming the emitter.
func LoadEquipSlots(path string) (*EquipSlots, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the equip slot table: %w", err)
	}
	var doc equipDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse the equip slot table %s: %w", path, err)
	}
	slots, err := FromEquipRows(doc.Slots)
	if err != nil {
		return nil, err
	}
	return slots, nil
}

// FromEquipRows builds the table from the decoded rows, so a test can hand it
// three classes instead of a file on disk.
func FromEquipRows(rows map[string]int) (*EquipSlots, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("the equip slot table holds no rows; write it with: " +
			"cd src && python emit_item_equip.py")
	}
	e := &EquipSlots{slots: make(map[int64]int, len(rows))}
	for key, slot := range rows {
		classID, err := strconv.ParseInt(key, 10, 64)
		if err != nil || classID == 0 {
			// The file is keyed by class id, so a key that is not one means this
			// is not the file the loader was written for. Skip the row, not the
			// table: one bad key must not cost the other 13,813.
			continue
		}
		// A slot outside the client's 21 and the two "either" values is not
		// something this side can place; dropping it makes that class
		// un-wearable, which is the safe direction.
		if !validSlotValue(slot) {
			continue
		}
		e.slots[classID] = slot
	}
	if len(e.slots) == 0 {
		return nil, fmt.Errorf("the equip slot table parsed to nothing; write it with: " +
			"cd src && python emit_item_equip.py")
	}
	return e, nil
}

// validSlotValue reports whether a value from the file is one this loader can
// act on: a real spot, or one of the two "either" markers.
func validSlotValue(slot int) bool {
	switch {
	case slot == EqpEitherHand, slot == EqpEitherRing:
		return true
	case slot < 0 || slot >= equipSpotLast:
		return false
	}
	return true
}

// Len is how many classes the table knows.
func (e *EquipSlots) Len() int {
	if e == nil {
		return 0
	}
	return len(e.slots)
}

// Allows reports whether the class may be worn at that spot.
//
// A NIL receiver, an unknown class and a spot outside 0..20 all answer false:
// the caller (EquipItem) refuses on false, so "we do not know" and "not allowed"
// take the same safe branch. This is deliberately stricter than the shipped
// server, which moves the item to its own default slot instead - a silent move
// the player did not ask for is harder to see than a refusal, and this zone's
// equip is one transaction with no such branch to test.
func (e *EquipSlots) Allows(classID int64, spot int) bool {
	if e == nil || spot < 0 || spot >= equipSpotLast {
		return false
	}
	slot, known := e.slots[classID]
	if !known {
		return false
	}
	switch slot {
	case EqpEitherHand:
		return spot == spotLeftHand || spot == spotRightHand
	case EqpEitherRing:
		return spot == spotRing1 || spot == spotRing2
	}
	return slot == spot
}
