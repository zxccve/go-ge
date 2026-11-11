package barrack

// THE CHARACTER - what the create-character screen finally asks for.
//
// The client sends CB_CREATE_CHAR (18, 63 bytes) when the player presses OK on that screen,
// and waits for BC_CREATE_CHAROK (19, 277 bytes) before it shows the finished character in
// the barrack. Until this handler existed the request was logged as "no handler yet" and the
// screen sat there (owner, 2026-11-11: "now create character is stuck").
//
// The write is a store method for the same reason every other write is: a handler test needs
// no database, and the row can only be written one way.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"geserver/protocol"

	"github.com/jackc/pgx/v5"
)

// CharacterCreator is the write half of a character row.
type CharacterCreator interface {
	// CreateCharacter inserts the character for a family that has none in that slot.
	// created is false when the slot is already used - then nothing is written and the
	// existing row is returned, so a double-clicked button changes nothing.
	CreateCharacter(ctx context.Context, commanderID uint32, in protocol.CreateChar) (CharInfo, bool, error)
}

// CreateCharacter is one transaction: refuse an empty name, a job id of 0 (the client sends
// the class it chose; 0 means the packet was misread), a slot outside the barrack, or a name
// this family already uses - then insert.
func (s *PGBarrackStore) CreateCharacter(ctx context.Context, commanderID uint32, in protocol.CreateChar) (CharInfo, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CharInfo{}, false, err
	}
	defer tx.Rollback(ctx)

	// character_id is MAX+1 (the legacy table has no sequence); the advisory lock serialises
	// two creations so they cannot pick the same id.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(71004)"); err != nil {
		return CharInfo{}, false, fmt.Errorf("lock: %w", err)
	}

	var existingSlot uint16
	err = tx.QueryRow(ctx,
		"SELECT slot FROM character WHERE commander_id = $1 AND slot = $2", commanderID, in.Slot,
	).Scan(&existingSlot)
	switch {
	case err == nil:
		return CharInfo{}, false, fmt.Errorf("slot %d is already used", in.Slot)
	case !errors.Is(err, pgx.ErrNoRows):
		return CharInfo{}, false, fmt.Errorf("look up slot %d: %w", in.Slot, err)
	}

	var sameName int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM character WHERE commander_id = $1 AND lower(name) = lower($2)",
		commanderID, in.Name,
	).Scan(&sameName); err != nil {
		return CharInfo{}, false, fmt.Errorf("look up name %q: %w", in.Name, err)
	}
	if sameName != 0 {
		return CharInfo{}, false, fmt.Errorf("this family already has a character called %q", in.Name)
	}

	var id uint32
	if err := tx.QueryRow(ctx,
		"INSERT INTO character (character_id, commander_id, slot, name, teampos, gender, job, headtype) "+
			"VALUES ((SELECT COALESCE(MAX(character_id),0)+1 FROM character), $1, $2, $3, 0, $4, $5, $6) "+
			"RETURNING character_id",
		commanderID, in.Slot, in.Name, in.Gender != 0, in.Job, in.HeadType,
	).Scan(&id); err != nil {
		return CharInfo{}, false, fmt.Errorf("insert character %q: %w", in.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CharInfo{}, false, fmt.Errorf("commit: %w", err)
	}

	return CharInfo{
		Slot: in.Slot, Name: in.Name, Gender: in.Gender != 0, Job: byte(in.Job),
	}, true, nil
}

// createChar answers CB_CREATE_CHAR. A refusal is logged and left unanswered, the way the
// original leaves the client on its own screen instead of interpreting a reply it was not
// designed for.
func (h *Handler) createChar(ctx context.Context, sess *Session, frame []byte) ([][]byte, error) {
	req, err := protocol.ParseCreateChar(frame)
	if err != nil {
		h.Log.Printf("CREATE_CHAR refused: %v", err)
		return nil, nil
	}
	if req.Name == "" {
		h.Log.Printf("CREATE_CHAR refused: the name is empty")
		return nil, nil
	}
	if req.Job == 0 || req.Job > 999999 {
		h.Log.Printf("CREATE_CHAR %q refused: job %d is not a class id", req.Name, req.Job)
		return nil, nil
	}
	if int(req.Slot) >= protocol.MaxCharSlots {
		h.Log.Printf("CREATE_CHAR %q refused: slot %d is outside the barrack", req.Name, req.Slot)
		return nil, nil
	}
	if h.Barrack == nil || sess.CommanderID == 0 {
		h.Log.Printf("CREATE_CHAR %q refused: no family on this session", req.Name)
		return nil, nil
	}
	cc, ok := h.Barrack.(CharacterCreator)
	if !ok {
		h.Log.Printf("CREATE_CHAR %q refused: the barrack store cannot create characters", req.Name)
		return nil, nil
	}
	row, created, err := cc.CreateCharacter(ctx, sess.CommanderID, *req)
	if err != nil {
		h.Log.Printf("CREATE_CHAR %q (commander %d): %v", req.Name, sess.CommanderID, err)
		return nil, nil
	}
	if created {
		h.Log.Printf("CREATE_CHAR commander %d -> %q (slot %d, job %d, gender %d), 277-byte OK",
			sess.CommanderID, row.Name, row.Slot, req.Job, req.Gender)
	} else {
		h.Log.Printf("CREATE_CHAR %q: slot %d already used - nothing written", req.Name, row.Slot)
	}
	okPkt := BuildCreateCharOK(1)
	// AND NOTHING ELSE. The OK alone is what the client acts on: its own handler
	// (CBarrackNet::CreateCharOK) reads the equipment out of this packet, adds the character
	// to the barrack world itself and shows its "[class] x has been created" notice. Sending
	// cmd 16 behind it - which this handler used to do - raises the BARRACK window behind
	// that notice (owner, 2026-11-11: "it should not show the barrack page/window at all when
	// the dialog appear, should just stay in the character creation window").
	out := [][]byte{okPkt}
	h.Log.Printf("CREATE_CHAR: cmd 19 alone - the client adds the character and keeps the " +
		"creation screen")
	return out, nil
}

// BuildCreateCharOK is BC_CREATE_CHAROK: the command, then the character's equipment block
// (21 spots at a 12-byte stride, an empty spot carrying the client's own "None" class), then
// the level byte at 275 and one pad byte - 277 in total, the size in the client's table.
//
// THE LEVEL IS NOT AT 276. The old Python server wrote it there and the barrack showed the
// new character at level 0 (src\ge_barrack_server.py, build_create_charok).
func BuildCreateCharOK(level byte) []byte {
	b := make([]byte, protocol.CreateCharOKSize)
	binary.LittleEndian.PutUint16(b, protocol.CreateCharOKCmd)
	for spot := 0; spot < equipSpotLast; spot++ {
		binary.LittleEndian.PutUint32(b[2+spot*equipEntryStride:], equipNone[spot])
	}
	b[275] = level
	return b
}
