package zone

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WHICH BARRACK BUTTON THE PLAYER PRESSED, and therefore which point wins on entry.
//
// Two buttons are handled, and they send two different commands. Proven from the
// shipped client source (D:\ge_server\source2\geClient\Source\), not guessed:
//
//	GEBarrackUIHandler.cpp:73  APPM_BAR_NEW  -> OnMsgNewGame (:195)
//	                         -> packet::CB_StartGame (:197)        = cmd 22
//	GEBarrackUIHandler.cpp:61  APPM_BAR_CON  -> OnMsgContuueGame (:189)
//	                         -> packet::CB_ContinueGame (:191)     = cmd 23
//
// and their builder sizes from the client's own table (data\cmd_table_barrack.txt):
// 22 = 22 bytes, 23 = 22 bytes.
//
// The client's THIRD button (APPM_BAR_CON_MISSION, cmd 24) is NOT part of this: missions
// are parked by the owner ("mission is not even being worked on yet ... just leave the
// button for now"), so the barrack does not answer cmd 24 and nothing records its kind.
// Stored value 2 stays reserved and reads back as EntryNew.
//
// `pending_zone_entry.kind` is the stored answer (db\migrations\00010_point_and_position.sql):
// 0 = new / saved start, 1 = continue / last point. `is_continue` is kept and written in
// step with 1, so a row written by an older binary is still readable.
type EntryKind int

const (
	// EntryNew is BAR_NEW / CB_STARTGAME (cmd 22): the player wants the SAVED START
	// POINT - the town they chose with the grid's Save button. Its barrack caption is
	// "새로 시작" (81167) with tooltip 81172, "start the game at the last SAVED point
	// [ %s ]", and the %s is BC_COMMANDER_INFO's newMapID.
	EntryNew EntryKind = iota
	// EntryContinue is BAR_CON / CB_CONTINUEGAME (cmd 23): the player wants the LAST
	// POINT - where the leader stood when they left. Caption 81166, tooltip 81171
	// "start the game at the last EXIT point [ %s ]", and the %s is contMapID.
	EntryContinue
)

// The stored values of `pending_zone_entry.kind`, named so the SQL and the type cannot
// drift apart.
const (
	entryKindValueNew      = 0
	entryKindValueContinue = 1
)

// String names the kind for a log line. An out-of-range value prints as a number with a
// word beside it, because a reader comparing the log with the table needs the number.
func (k EntryKind) String() string {
	switch k {
	case EntryNew:
		return "new game (the saved start point)"
	case EntryContinue:
		return "continue game (the last point)"
	}
	return fmt.Sprintf("kind %d (not one of 0/1)", int(k))
}

// UseEntryKind wires the store that answers which barrack button this commander pressed.
//
// One hook, not two: nothing here writes the kind - the BARRACK writes it when the
// button is pressed (barrack\PGStore.SaveZoneEntry) and the zone only reads it. NIL means
// "no store": the entry then behaves as EntryNew, which is the behaviour every commander
// had before this file existed.
func (h *Handler) UseEntryKind(load func(ctx context.Context, commanderID int32) (EntryKind, error)) {
	h.EntryKindOf = load
}

// EntryKindOf reads which button the barrack recorded for this commander.
//
// No row answers EntryNew and NO ERROR: a session that never went through the barrack's
// button (a test, a hand-made entry, a first login) is a new game, which is the safe
// default - it uses the point the player chose rather than the spot a session ended.
//
// An out-of-range value answers EntryNew too, with no error, for the same reason: an old
// or hand-edited row must never be able to STOP an entry. A refusal here would lock a
// player out of the game over a byte.
func (s *PGStore) EntryKindOf(ctx context.Context, commanderID int32) (EntryKind, error) {
	if commanderID <= 0 {
		return EntryNew, fmt.Errorf("commander id %d is not usable", commanderID)
	}
	var kind int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(kind, CASE WHEN is_continue THEN 1 ELSE 0 END)
		   FROM pending_zone_entry WHERE commander_id = $1`, commanderID).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EntryNew, nil
		}
		return EntryNew, fmt.Errorf("read the entry kind of commander %d: %w",
			commanderID, err)
	}
	switch kind {
	case entryKindValueContinue:
		return EntryContinue, nil
	case entryKindValueNew:
		return EntryNew, nil
	}
	return EntryNew, nil
}
