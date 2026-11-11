package barrack

// THE FAMILY NAME - the one thing a brand-new account needs before it can do anything.
//
// The client asks for it itself: on the barrack screen an account with no commander sends
// CB_CREATE_COMMANDER (11) carrying the name the player typed (protocol\barrack_commander.go
// has the layout, taken from the original's PKS_CB_CREATE_COMMANDER). The original answers
// BC_CREATE_COMMANDEROK (12) once the DB insert finishes; the client then re-fetches the
// barrack and the family is there.
//
// WHY THE WRITE IS NOT IN THE HANDLER (the project's own rule): every SQL statement lives in
// a store method, so a handler can be tested without a database and this row can only be
// written one way.
//
// WHAT IT DOES NOT DO: no character (that is cmd 18, its own card) and no barrack captions
// or lock properties - a fresh commander has none, and the readers already fall back to the
// game data's defaults (Lock_CharSlot 10, Lock_TeamGroup 5).

import (
	"context"
	"errors"
	"fmt"

	"geserver/protocol"

	"github.com/jackc/pgx/v5"
)

// CommanderCreator is the write half of the commander row, kept as its own interface so
// adding it does not change BarrackStore and break every test double that implements it
// (the same shape LoginSeqStore uses).
type CommanderCreator interface {
	// CreateCommander creates the family for an account that has none and points
	// account_info.commander_id at it. created is false when the account ALREADY had a
	// commander - then nothing is written and the existing row is returned, which is what
	// makes a double-clicked button harmless.
	CreateCommander(ctx context.Context, accountID uint32, memberID, name string) (row CommanderRow, created bool, err error)
}

// CreateCommander is one transaction: refuse if the account already has a family, insert
// the commander row, point the account at it.
func (s *PGBarrackStore) CreateCommander(ctx context.Context, accountID uint32, memberID, name string) (CommanderRow, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CommanderRow{}, false, err
	}
	defer tx.Rollback(ctx)

	// commander_id is MAX+1 (the legacy table has no sequence), so two families created at
	// the same instant must not pick the same id. The row lock on the ACCOUNT is the second
	// half: two clicks for one account serialise here, and the second sees existing != 0.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(71002)"); err != nil {
		return CommanderRow{}, false, fmt.Errorf("lock: %w", err)
	}
	var existing uint32
	err = tx.QueryRow(ctx,
		"SELECT commander_id FROM account_info WHERE account_id = $1 FOR UPDATE", accountID,
	).Scan(&existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return CommanderRow{}, false, fmt.Errorf("no account %d", accountID)
	case err != nil:
		return CommanderRow{}, false, fmt.Errorf("read account %d: %w", accountID, err)
	case existing != 0:
		row, found, rerr := s.Commander(ctx, existing)
		if rerr != nil {
			return CommanderRow{}, false, rerr
		}
		if !found {
			return CommanderRow{}, false, fmt.Errorf("account %d points at commander %d, which does not exist", accountID, existing)
		}
		return row, false, nil
	}

	var id uint32
	if err := tx.QueryRow(ctx,
		"INSERT INTO commander (commander_id, account_id, member_id, name, zone_id) "+
			"VALUES ((SELECT COALESCE(MAX(commander_id),0)+1 FROM commander), $1, $2, $3, 0) "+
			"RETURNING commander_id", accountID, memberID, name,
	).Scan(&id); err != nil {
		return CommanderRow{}, false, fmt.Errorf("insert commander %q: %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE account_info SET commander_id = $1 WHERE account_id = $2", id, accountID,
	); err != nil {
		return CommanderRow{}, false, fmt.Errorf("point account %d at commander %d: %w", accountID, id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CommanderRow{}, false, fmt.Errorf("commit: %w", err)
	}

	// Read it back through the SAME reader the barrack uses, so what the handler reports is
	// what the next login will see (name, locks, captions).
	row, found, err := s.Commander(ctx, id)
	if err != nil {
		return CommanderRow{}, false, err
	}
	if !found {
		return CommanderRow{}, false, fmt.Errorf("commander %d vanished after insert", id)
	}
	return row, true, nil
}

// createCommander answers CB_CREATE_COMMANDER. A refusal is LOGGED and left unanswered,
// which is what the original does: CClient::OnCreateCommander returns false without sending
// anything, so the client stays on its own screen instead of interpreting a reply it was
// not designed for.
func (h *Handler) createCommander(ctx context.Context, sess *Session, frame []byte) ([][]byte, error) {
	req, err := protocol.ParseCreateCommander(frame)
	if err != nil {
		h.Log.Printf("CREATE_COMMANDER refused: %v", err)
		return nil, nil
	}
	if h.Barrack == nil {
		h.Log.Printf("CREATE_COMMANDER %q: no barrack store wired - nothing created", req.Name)
		return nil, nil
	}
	cc, ok := h.Barrack.(CommanderCreator)
	if !ok {
		h.Log.Printf("CREATE_COMMANDER %q: the barrack store cannot create commanders - nothing created", req.Name)
		return nil, nil
	}
	row, created, err := cc.CreateCommander(ctx, sess.AccountID, sess.MemberID, req.Name)
	if err != nil {
		h.Log.Printf("CREATE_COMMANDER %q (account %d): %v", req.Name, sess.AccountID, err)
		return nil, nil
	}
	// The session now knows the family, so the next CB_REQ_BARRACK_INFO (15) builds a real
	// barrack instead of an empty one - no re-login needed.
	sess.CommanderID = row.ID
	if created {
		h.Log.Printf("CREATE_COMMANDER account %d -> commander %d %q (charSlot %d, teamMax %d), 12-byte OK",
			sess.AccountID, row.ID, row.Name, row.CharSlot, row.TeamMax)
	} else {
		h.Log.Printf("CREATE_COMMANDER account %d already had commander %d %q - nothing written, 12-byte OK",
			sess.AccountID, row.ID, row.Name)
	}
	// The five values are the new family's citizen point, rank, duty, fame and karma. A
	// brand-new commander has 0 in all of them (the columns' own defaults), which is what
	// account creation wrote.
	out := make([][]byte, 0, 3)
	// THE REFERENCE'S OWN ORDER (source2\geServer\BarrackServer\ClientFSM.cpp:730-735,
	// AS_CREATE_COMMANDER): OK -> SendData() (cmd 10) -> SendLockInfo() (cmd 32). The client
	// then asks for its barrack (cmd 15) by itself - measured, it asks twice - and THAT answer
	// is where the screen is decided: for a family with zero characters it is cmd 16 alone,
	// which is the packet that opens character creation.
	out = append(out, protocol.BuildCreateCommanderOK(0, 0, 0, 0, 0)) // 12
	out = append(out, h.commanderInfoPacket(row))                    // 10: the card, now real
	out = append(out, LockTableFor(row.TeamMax))                     // 32: the lock table
	h.Log.Printf("CREATE_COMMANDER: %q created - pushed cmd 12 + cmd 10 + cmd 32; the barrack "+
		"follows on the client's own request", row.Name)
	return out, nil
}

// commanderInfoPacket is cmd 10 for a family that exists but has no characters - the state a
// brand-new family is in the moment it is created.
//
// THE START MAP MUST BE REAL. A fresh commander row has zone_id 0 and no saved point, so
// barrackMaps() returns 0/0 - and the client dereferences geMapTable::Get(newMapID) with NO
// null check (source2\geClient\Source\GEBarrackNet.cpp:230). Measured 2026-11-11: with
// newMapID 0 the dialog stayed open and no later packet moved it (log barrack_family_test18:
// 12+10+32+barrack delivered, client silent). A new family starts at STARTONIA - map 18,
// `city_tuto` (owner's rule, 2026-11-11) - so that is what the card carries.
func (h *Handler) commanderInfoPacket(row CommanderRow) []byte {
	saveMap, lastMap := h.barrackMaps(row)
	ci := CommanderInfo{
		Name: row.Name, Rank: TeamGroupMaxSlot, CharSlot: slotCount(row.CharSlot, 0),
		SaveMapID: saveMap, LastMapID: lastMap, BarrackNames: row.BarrackNames,
	}
	if h.JP {
		return BuildCommanderInfoChannels(ci, 32)
	}
	return BuildCommanderInfo(ci)
}
