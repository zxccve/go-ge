package zone

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geserver/barrack"
	"geserver/item"
)

// PGStore reads the zone's share of PostgreSQL: which characters a logged-in
// account owns, and which of them the barrack asked to put on the field.
//
// It reads TWO tables only - `account_info` (account -> commander) and
// `pending_zone_entry` (the slots the player picked in the barrack) - and then
// the `character` rows those slots name. The same shape the Python zone reads
// from its handoff, but from the database instead of a file, which is the rule
// in DECISIONS.md.
type PGStore struct {
	pool *pgxpool.Pool

	// idBase and idCounter mint item ids the way the shipped server does:
	// (base << 32) + counter. The base is claimed once per process, so the mutex
	// has to be here, not a package global - two stores must not share a counter
	// they would then issue from different bases.
	idMu      sync.Mutex
	idBase    int64
	idCounter int64

	// Wearable is which slot(s) each item class may be worn in
	// (item.EquipSlots), loaded by main.go from data\item_equip.json. EquipItem
	// REFUSES every equip while it is nil: a missing table must never read as
	// "this class fits anywhere", which would let a player wear a sword as boots.
	Wearable *item.EquipSlots
}

// NewPGStore wraps the shared pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Property is one commander property as the client caches it.
type Property struct {
	Name  string // e.g. Lock_InventoryCount
	Value uint32
}

// LockProperties reads the commander's Lock_* values for an account.
//
// They live in `commander_property`, whose `property` column is NULL for the
// numeric quest rows (SECTION 1) and carries the NAME for the named ones. A
// missing row is NOT an error: the client then keeps its own table default, which
// is what the Python server does too (measured 2026-10-25: there is currently no
// Lock_* row for any commander, so both servers show the default 248 slots).
func (s *PGStore) LockProperties(ctx context.Context, accountID uint32) ([]Property, error) {
	commanderID, err := s.CommanderOf(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT property, value FROM commander_property
		  WHERE commander_id = $1 AND property LIKE 'Lock\_%' ESCAPE '\'
		  ORDER BY property`, commanderID)
	if err != nil {
		return nil, fmt.Errorf("properties of commander %d: %w", commanderID, err)
	}
	defer rows.Close()

	var out []Property
	for rows.Next() {
		var (
			name  string
			value string
		)
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("scan property: %w", err)
		}
		var n uint32
		// The value column is text; a row that is not a number is skipped rather
		// than sent as 0, which would silently wipe the client's value.
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
			continue
		}
		out = append(out, Property{Name: name, Value: n})
	}
	return out, rows.Err()
}

// CommanderOf maps an account to its commander. Exported because the zone needs
// it once per login: a script transaction writes to a COMMANDER, not an account.
func (s *PGStore) CommanderOf(ctx context.Context, accountID uint32) (int32, error) {
	var commanderID int32
	err := s.pool.QueryRow(ctx,
		`SELECT commander_id FROM account_info WHERE account_id = $1`,
		int32(accountID)).Scan(&commanderID)
	if err != nil {
		return 0, fmt.Errorf("account %d: %w", accountID, err)
	}
	return commanderID, nil
}

// GmLevelOf answers the operator tier of a family member: 1 is the GM tier the
// shipped server reads from ACCOUNT_LEVEL, and an ABSENT ROW is level 0, not an
// error (`account_level` only carries grants; see the migration's comment).
//
// THE MATCH IS CASE-INSENSITIVE, ON PURPOSE. The client sends the login id as the
// player typed it (`START_GAME account 35 ("Arcanine")`) while this database may
// store it as `arcanine`, and PostgreSQL's `=` on varchar is case-sensitive - so
// an equality match silently read a real grant as level 0 and refused `//hpup`
// (live failure 2026-11-08 23:09). A permission decision must not depend on how
// the player capitalised their own name. `lower()` is used rather than a citext
// column because the rail forbids a schema change and `account_level` holds a
// handful of grant rows, so the scan this costs is nothing.
//
// TWO ROWS DIFFERING ONLY IN CASE (the workaround this database carries today)
// ANSWER ONE DETERMINISTIC VALUE: the HIGHEST level wins, and equal levels break
// on the member_id BYTES (`COLLATE "C"`), never on plan order. The highest grant
// wins because a grant is the exception being looked for - a stale lower-case row
// must not cancel a real grant under the spelling the client sends. Once only one
// spelling remains, the duplicate row can be deleted.
//
// It is called ONCE per zone entry, from startGame, so the chat path never asks
// the database per line. A real query error is returned: the caller logs it and
// treats the session as level 0, which is the closed answer.
func (s *PGStore) GmLevelOf(ctx context.Context, memberID string) (int, error) {
	if memberID == "" {
		return 0, nil
	}
	var level int16
	// One answer, whatever the plan: highest level, then the member_id bytes.
	err := s.pool.QueryRow(ctx,
		`SELECT level FROM account_level
		  WHERE lower(member_id) = lower($1)
		  ORDER BY level DESC, member_id COLLATE "C" ASC
		  LIMIT 1`, memberID).Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("level of member %q: %w", memberID, err)
	}
	return int(level), nil
}

// characterRow is one `character` row, before the game's formulas touch it.
type characterRow struct {
	ID     uint32
	Slot   int
	Name   string
	Gender bool
	Job    int
	Head   int
	Exp    int64
	HP     int64
	SP     int64
	PosX   int32
	PosY   int32
	PosZ   int32
	// Equip is the item class worn at each of the 21 appearance slots, copied out
	// of `wear_item` -> `item.class_id` (see loadWorn). Zero means "nothing worn at
	// this spot" and is replaced by EquipNone when the packet is built.
	Equip [equipSpotLast]uint32
	// Promotion and CashPromotion are `character_property` rows (see
	// loadPromotion). Promotion is ALREADY capped by the level band; PromotionStep
	// is the raw step and CashPromotion the raw cash step.
	Promotion     int
	PromotionStep int
	CashPromotion int
}

// TeamOf answers the characters to spawn for an account.
//
// Team choice mirrors the Python zone exactly (`load_players`): the slots the
// barrack recorded are used IN ORDER, and if none of them can be found the
// player's first character is used instead - a player must never enter an empty
// world.
func (s *PGStore) TeamOf(ctx context.Context, accountID uint32) ([]Character, error) {
	var commanderID int32
	err := s.pool.QueryRow(ctx,
		`SELECT commander_id FROM account_info WHERE account_id = $1`,
		int32(accountID)).Scan(&commanderID)
	if err != nil {
		return nil, fmt.Errorf("account %d: %w", accountID, err)
	}

	rows, err := s.loadCharacters(ctx, commanderID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	picked := s.pickedSlots(ctx, commanderID)
	bySlot := make(map[int]characterRow, len(rows))
	for _, r := range rows {
		bySlot[r.Slot] = r
	}

	chosen := make([]characterRow, 0, len(picked))
	for _, slot := range picked {
		if r, ok := bySlot[slot]; ok {
			chosen = append(chosen, r)
		}
	}
	if len(chosen) == 0 {
		chosen = rows[:1]
	}

	out := make([]Character, 0, len(chosen))
	for _, r := range chosen {
		lvl := barrack.LevelOf(r.Exp)
		out = append(out, Character{
			// The handle IS the character id: the Python zone passes
			// handle=c["character_id"], and the client keys everything on it.
			Handle:   r.ID,
			Name:     r.Name,
			Gender:   boolByte(r.Gender),
			Job:      uint32(r.Job),
			HeadType: uint8(r.Head),
			Level:    uint8(lvl & 0xFF),
			// The promotion level the STATUS SCRIPT reads (SCR_Get_MSP's
			// promotionLv*10). Already banded by loadPromotion, which is the same
			// min(step, band) the barrack packet uses.
			Promotion:     uint8(r.Promotion),
			PromotionStep: uint8(r.PromotionStep),
			CashPromotion: uint8(r.CashPromotion),
			HP:            float64(r.HP),
			// The real maximum comes from the game's own formula
			// (get_status_pc.scp SCR_Get_MHP), which is NOT ported yet. Sending
			// the current hp as the max keeps the bar full; sending 0 draws an
			// empty one. This is a known gap, not a measurement.
			MaxHP: float64(r.HP),
			SP:    uint16(clamp16(r.SP)),
			MaxSP: uint16(clamp16(r.SP)),
			Exp:   r.Exp,
			// WORLDPOS: x, y, cell. The stored zpos is a HEIGHT, not a navmesh
			// cell, so a fresh entry may land awkwardly - the same last-resort
			// the Python zone falls back to when it has no spawn point.
			Position: [3]int32{r.PosX, r.PosY, r.PosZ},
			// What this character wears. Without it every appearance slot is
			// EquipNone and the other player's client draws the character with no
			// weapon and no armour (R32's leftover).
			Equip: r.Equip,
		})
	}
	return out, nil
}

func (s *PGStore) loadCharacters(ctx context.Context, commanderID int32) ([]characterRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT character_id, slot, name, gender, job, headtype, exp, hp, sp,
		        pos_x, pos_y, pos_z
		   FROM character WHERE commander_id = $1 AND iscard = 0
		  ORDER BY slot`, commanderID)
	if err != nil {
		return nil, fmt.Errorf("characters of commander %d: %w", commanderID, err)
	}
	defer rows.Close()

	var out []characterRow
	for rows.Next() {
		var r characterRow
		if err := rows.Scan(&r.ID, &r.Slot, &r.Name, &r.Gender, &r.Job,
			&r.Head, &r.Exp, &r.HP, &r.SP, &r.PosX, &r.PosY, &r.PosZ); err != nil {
			return nil, fmt.Errorf("scan character: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	if err := s.loadWorn(ctx, out); err != nil {
		return nil, err
	}
	if err := s.loadPromotion(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadWorn fills each character's Equip from the WORN items: `wear_item` names an
// item per (character, spot), and `item.class_id` is the class the client keys its
// model and its item properties on.
//
// `spot` is the SAME number as the appearance block's entry index - 0..20, stride
// 12 at the appearance's +68 - so it is used as the index directly, never
// remapped. Spot 0 is the HEAD, 4 and 5 are the two hands, 11 is the COSTUME.
// (The numbering is not this file's invention: the Python oracle reads the same
// column into the same index, src\ge_db.py:269-276, and src\zone_server.py:638-642
// spells it out. The rows agree: char 1066 has 17740 = LafiteHair at spot 0 and
// char 40 has 7215 = CrecementoShield at spot 4.)
//
// A character with no worn rows is NOT an error and gets no entries: Equip stays
// zero, and the packet builder turns a zero into the client's own "nothing worn"
// class id. Sending nothing is right; inventing an id is the crash this project
// already paid for twice.
func (s *PGStore) loadWorn(ctx context.Context, out []characterRow) error {
	ids := make([]int32, len(out))
	index := make(map[int32]int, len(out))
	for i := range out {
		ids[i] = int32(out[i].ID)
		index[int32(out[i].ID)] = i
	}
	rows, err := s.pool.Query(ctx,
		`SELECT w.character_id, w.spot, i.class_id
		   FROM wear_item w JOIN item i ON i.item_id = w.item_id
		  WHERE w.character_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("worn items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			charID int32
			spot   int
			class  int64
		)
		if err := rows.Scan(&charID, &spot, &class); err != nil {
			return fmt.Errorf("scan worn item: %w", err)
		}
		i, ok := index[charID]
		// A spot outside the appearance block is not ours to place: skipping it
		// keeps a bad row from writing past the end of Equip.
		if !ok || spot < 0 || spot >= equipSpotLast {
			continue
		}
		out[i].Equip[spot] = uint32(class)
	}
	return rows.Err()
}

// loadPromotion fills each character's promotion steps from `character_property`.
//
// WHY THIS EXISTS: the status script reads GetPromotionLevel(self), and
// SCR_Get_MSP is `195 + Lv*5 + promotionLv*10` before gear. With that binding
// answering 0 the sheet's max SP could only ever be the unpromoted rule (695 at
// level 100), while the live server's own sheet for a promoted character reads
// 995 at level 150 - exactly promotion 5. The row exists in this very database
// (character 304 "Amy" carries PromotionStep 4), so answering 0 was a gap in the
// zone's loader, not missing data.
//
// THE CLAMP IS THE GAME'S, NOT THIS FILE'S: promotionLv is
// min(PromotionStep, ge_tables.promotion_band for the level) - barrack.PromotionBand,
// the same rule the barrack packet already applies, so the zone and the barrack
// can never disagree about how promoted a character is.
//
// A missing row means "not promoted" (0), which is a real answer: 10 of the 14
// characters here have no such row and are below the level where a band starts.
func (s *PGStore) loadPromotion(ctx context.Context, out []characterRow) error {
	ids := make([]int32, len(out))
	index := make(map[int32]int, len(out))
	for i := range out {
		ids[i] = int32(out[i].ID)
		index[int32(out[i].ID)] = i
	}
	rows, err := s.pool.Query(ctx,
		`SELECT character_id, property, value FROM character_property
		  WHERE character_id = ANY($1)
		    AND property IN ('PromotionStep', 'CashPromotionStep')`, ids)
	if err != nil {
		return fmt.Errorf("promotion steps: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			charID int32
			prop   string
			val    string
		)
		if err := rows.Scan(&charID, &prop, &val); err != nil {
			return fmt.Errorf("scan promotion step: %w", err)
		}
		i, ok := index[charID]
		if !ok {
			continue
		}
		n, convErr := strconv.Atoi(val)
		if convErr != nil || n < 0 {
			// A row this build cannot read must not promote anyone and must not
			// fail the login: the character still spawns, unpromoted.
			continue
		}
		switch prop {
		case "PromotionStep":
			// The RAW step is kept as well as the banded one: ZC_PROMOTION_INFO's
			// `maxStep` is the raw property, `level` the banded value.
			out[i].PromotionStep = n
			if band := barrack.PromotionBand(barrack.LevelOf(out[i].Exp)); n > band {
				n = band
			}
			if n > 255 {
				n = 255
			}
			out[i].Promotion = n
		case "CashPromotionStep":
			if n > 255 {
				n = 255
			}
			out[i].CashPromotion = n
		}
	}
	return rows.Err()
}

// pickedSlots reads the barrack's choice. A missing row is NOT an error: it means
// this account did not come through the barrack (a direct zone login), and the
// first character is used instead.
func (s *PGStore) pickedSlots(ctx context.Context, commanderID int32) []int {
	var s0, s1, s2 int16
	err := s.pool.QueryRow(ctx,
		`SELECT slot0, slot1, slot2 FROM pending_zone_entry WHERE commander_id = $1`,
		commanderID).Scan(&s0, &s1, &s2)
	if err != nil {
		return nil
	}
	return []int{int(s0), int(s1), int(s2)}
}

func boolByte(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func clamp16(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return v
}
