package barrack

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BarrackStore reads what the barrack screen needs for one commander.
//
// Kept separate from Store (the login lookup) so each can be faked in tests
// without dragging the other along.
type BarrackStore interface {
	Commander(ctx context.Context, commanderID uint32) (CommanderRow, bool, error)
	Characters(ctx context.Context, commanderID uint32) ([]CharInfo, error)
	TeamGroups(ctx context.Context, commanderID uint32) ([]TeamGroup, error)

	// SaveZoneEntry records the team the player picked, for the ZONE process to
	// read. It replaces the out\zone_team_<id>.json handoff file: a row keyed by
	// commander is the same information without a file, and it is safe for several
	// players entering at once.
	SaveZoneEntry(ctx context.Context, entry ZoneEntry) error

	// SaveTeamPositions writes the picked team back onto the character rows
	// (`character.teampos`), which is what makes the team PERSIST: the barrack
	// packet (cmd 16) sends teampos for every character, the client turns a
	// non-zero one into its "previous team", and Last Point / Continue Game only
	// exists while that team is there.
	SaveTeamPositions(ctx context.Context, commanderID uint32, slots [3]uint16) error

	// ExtendTeamGroup is the Team List's Extend button: it charges the Moon
	// Stones and raises Lock_TeamGroup by ONE, in a single transaction. The
	// outcome says whether it happened; a refusal writes nothing at all.
	ExtendTeamGroup(ctx context.Context, commanderID uint32, classID uint32) (ExtendResult, error)
}

// ExtendOutcome is what ExtendTeamGroup did.
type ExtendOutcome int

const (
	// ExtendDone: the stones were taken and Lock_TeamGroup went up by one.
	ExtendDone ExtendOutcome = iota
	// ExtendAtCap: Lock_TeamGroup is already GET_TEAMGROUP_MAXSLOT(); nothing
	// was read from the bag and nothing was written.
	ExtendAtCap
	// ExtendNoStones: the bag does not hold the price; nothing was written.
	ExtendNoStones
)

// ExtendResult is what ExtendTeamGroup saw and did, for the handler's log line.
//
// Slot is the value the call READ (not a value the caller passed in), because
// the price and the cap are decided inside the transaction - two clicks must not
// both pay the old price.
type ExtendResult struct {
	Outcome ExtendOutcome
	Slot    uint16 // Lock_TeamGroup when the call read it
	NewSlot uint16 // Slot + 1 when Outcome is ExtendDone, else Slot
	Need    int    // TeamGroupPrice(Slot), the stones the script asks
	Held    int    // stones in the bag when the call read it
	Taken   int    // stones actually taken (0 unless ExtendDone)
}

// ZoneEntry is the team a player picked in the barrack, waiting for the zone.
//
// It is a MESSAGE, not history: one row per commander, overwritten on every New
// Game, and only the newest pick means anything. Slots keep their EMPTY_CHARSLOT
// (255) entries so the row is a faithful copy of what the client sent.
type ZoneEntry struct {
	CommanderID uint32
	Channel     uint16
	Slots       [3]uint16
	Continue    bool
}

// Kind is the `pending_zone_entry.kind` value this entry records.
//
// R71 KEPT THIS FROM THE NEWER TREE. It is not a map field and it is not part of the
// channel work: the zone (zone\entry_kind.go, PGStore.EntryKindOf) reads this column to
// decide whether cmd 21 uses the SAVED START POINT or the LAST POINT, and
// zone\r70_test.go asserts these two numbers. Dropping it would make every entry read as
// row default 0 (new game), so the Continue button would enter at the saved town instead
// of where the leader logged out.
func (e ZoneEntry) Kind() int16 {
	if e.Continue {
		return 1
	}
	return 0
}

// CommanderRow is the commander's own data for BC_COMMANDER_INFO.
type CommanderRow struct {
	ID        uint32
	Name      string
	ZoneID    uint16
	// SaveWarpID is the SAVED START POINT from `commander.point`: a RESTART WARP
	// id (203 Reboldoeux, 213 Coimbra), NOT a map id.
	//
	// R71 KEPT THIS FIELD, NOT ITS USE. Nothing in this rolled-back barrack turns it
	// into the cmd 10 newMapID - that rename is what regressed, so this build labels
	// both buttons with ZoneID again. It stays because the ZONE's storage split
	// (commander.point = saved warp, commander.zone_id = last point) is still the
	// truth, and zone\start_point_test.go asserts the barrack can read it back.
	SaveWarpID uint16
	ChannelID  byte // which channel of that zone the player is on; cmd 35 reports it
	CharSlot   uint16 // Lock_CharSlot
	TeamMax    uint16 // Lock_TeamGroup -> the packet's "rank"

	// BarrackNames are the player's own captions for the 24 barrack buttons.
	// An empty slot makes the client keep its own localised caption.
	BarrackNames [maxBarrackChannel]string
}

// The reference server's barrack lock DEFAULTS, taken from `load_lock_props()`
// in the working Python server and confirmed against its actual packet by
// oracle_test.go. They apply ONLY when the commander has no property row: a
// real row (a player who extended their Team List) is what both the packet and
// the Extend button read - see Commander and ExtendTeamGroup, which use the
// same (commander_id, 'Lock_TeamGroup', section 0) row and the same fallback.
const (
	defaultCharSlot uint16 = 10 // Lock_CharSlot
	defaultTeamMax  uint16 = 5  // Lock_TeamGroup -> the packet's "rank"
	// newFamilyStartMap is Startonia (`city_tuto`, ClassID 18 in data\map_table.xml): the
	// map a BRAND-NEW family starts at (owner, 2026-11-11). cmd 10's newMapID must be a real
	// map - the client dereferences geMapTable::Get(newMapID) with no null check.
	newFamilyStartMap uint16 = 18
)

// PGBarrackStore is the PostgreSQL implementation.
type PGBarrackStore struct{ pool *pgxpool.Pool }

// NewPGBarrackStore wraps the pool.
func NewPGBarrackStore(pool *pgxpool.Pool) *PGBarrackStore { return &PGBarrackStore{pool: pool} }

// Commander loads the commander row plus the two lock properties that the
// packet derives its fields from.
//
// TeamMax is the commander's CURRENT team-group count: `commander_property`
// 'Lock_TeamGroup' section 0, or the game data's default (defaultTeamMax, 5) when
// the row is absent. It is what cmd 32's Lock table carries, what
// GET_TEAMGROUP_PRICE charges from, and what ExtendTeamGroup raises by one - ONE
// value, one source, because a commander who extended to 6 must not be told 5 on
// the next login.
//
// IT IS NOT cmd 10's `rank`. Rank is the CAP, GET_TEAMGROUP_MAXSLOT() (20), and
// the handler sends that constant on both build paths: the client's Extend guard
// is `Lock_TeamGroup < rank` (client 0x005426c1, history\barrack.md "2026-11-08 -
// the Extend gate"), so filling rank with this same number makes the guard
// `n < n` and the button sends nothing. Read the two as a pair: this value is the
// count, rank is the ceiling, and only their DIFFERENCE is what the Extend button
// is for.
//
// The values are not in the database for a commander who never extended: the
// working Python server takes them from `load_lock_props()` (Lock_CharSlot=10,
// Lock_TeamGroup=5). The 4/3 that used to be here were the Python builder's
// FALLBACKS for a missing table, and using them made every barrack differ from the
// reference (charSlot 4 vs 10). Section 0 is the row the Extend handler reads, so
// it is the only one this join may pick.
func (s *PGBarrackStore) Commander(ctx context.Context, commanderID uint32) (CommanderRow, bool, error) {
	row := CommanderRow{ID: commanderID, CharSlot: defaultCharSlot, TeamMax: defaultTeamMax}
	err := s.pool.QueryRow(ctx,
		"SELECT c.name, c.zone_id, c.point, c.channel_id, "+
			"COALESCE(p1.value::smallint, $2), COALESCE(p2.value::smallint, $3) "+
			"FROM commander c "+
			"LEFT JOIN commander_property p1 ON p1.commander_id = c.commander_id "+
			"  AND p1.property = 'Lock_CharSlot' AND p1.section = 0 "+
			"LEFT JOIN commander_property p2 ON p2.commander_id = c.commander_id "+
			"  AND p2.property = 'Lock_TeamGroup' AND p2.section = 0 "+
			"WHERE c.commander_id = $1", commanderID, defaultCharSlot, defaultTeamMax,
	).Scan(&row.Name, &row.ZoneID, &row.SaveWarpID, &row.ChannelID, &row.CharSlot,
		&row.TeamMax)
	if err != nil {
		if isNoRows(err) {
			return CommanderRow{}, false, nil
		}
		return CommanderRow{}, false, fmt.Errorf("load commander %d: %w", commanderID, err)
	}

	// The player's own barrack captions. A failure here must not lose the login,
	// so it is logged through the returned error only when the table is missing.
	names, err := s.pool.Query(ctx,
		"SELECT channel, name FROM commander_barrack_name WHERE commander_id = $1", commanderID)
	if err == nil {
		defer names.Close()
		for names.Next() {
			var ch int
			var name string
			if names.Scan(&ch, &name) == nil && ch >= 0 && ch < maxBarrackChannel {
				row.BarrackNames[ch] = name
			}
		}
	}
	return row, true, nil
}

// SaveZoneEntry records the picked team for the zone process.
//
// An UPSERT, because one commander has at most one pending entry and the newest
// pick wins. The row is small and short-lived; nothing reads its history.
//
// A NEW PICK IS A NEW TOKEN (2026-11-10, card t_0cda12ac): the write bumps `version`
// and clears `consumed_at`, so the row the zone consumed for the previous entry can
// never be taken twice - the zone's consume is a compare-and-swap on
// `consumed_at IS NULL` (zone\store.go ConsumeZoneEntry).
func (s *PGBarrackStore) SaveZoneEntry(ctx context.Context, e ZoneEntry) error {
	_, err := s.pool.Exec(ctx,
		"INSERT INTO pending_zone_entry "+
			"(commander_id, channel, slot0, slot1, slot2, is_continue, kind, picked_at) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, now()) "+
			"ON CONFLICT (commander_id) DO UPDATE SET "+
			"channel = EXCLUDED.channel, slot0 = EXCLUDED.slot0, slot1 = EXCLUDED.slot1, "+
			"slot2 = EXCLUDED.slot2, is_continue = EXCLUDED.is_continue, "+
			// `kind` is written IN STEP with is_continue on purpose (migration
			// 00010 comment): the zone reads kind, older readers read is_continue,
			// and the two must never say different things.
			"kind = EXCLUDED.kind, picked_at = now(), "+
			// A fresh token: version rises, the consume mark clears.
			"consumed_at = NULL, version = pending_zone_entry.version + 1",
		e.CommanderID, int16(e.Channel),
		int16(e.Slots[0]), int16(e.Slots[1]), int16(e.Slots[2]), e.Continue, e.Kind())
	if err != nil {
		return fmt.Errorf("save zone entry for commander %d: %w", e.CommanderID, err)
	}
	return nil
}

// SaveTeamPositions writes the picked team onto the character rows.
//
// `teampos` is the client's TEAM_NONE = 0, TEAM_LEADER = 1, TEAM_SECOND = 2,
// TEAM_THIRD = 3 (geShared.h:629). Only NON-zero means "in a team", which is why
// a server that leaves every row at 0 makes the client believe nobody is in a
// team: New Game and Last Point both key off it (GEBarrackMode.cpp:462
// IsThereMember, and ExistPrevTeam() at GEBarrackActor.cpp:241).
//
// ONE statement, so there is no moment where the old and the new team are both
// half-written. An empty slot (255) matches no character and simply falls to 0.
func (s *PGBarrackStore) SaveTeamPositions(ctx context.Context, commanderID uint32, slots [3]uint16) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE character SET teampos = "+
			"CASE slot WHEN $2 THEN 1 WHEN $3 THEN 2 WHEN $4 THEN 3 ELSE 0 END "+
			"WHERE commander_id = $1",
		commanderID, int16(slots[0]), int16(slots[1]), int16(slots[2]))
	if err != nil {
		return fmt.Errorf("save team positions for commander %d: %w", commanderID, err)
	}
	return nil
}

// ExtendTeamGroup charges the Moon Stones and raises Lock_TeamGroup by one.
//
// ONE TRANSACTION, because the reference does it as one DB command
// (CTeamGroupExtender.AppendProperty("Lock_TeamGroup", maxCount+1) +
// AppendRemoveItem, BarrackServer/Client.cpp:953-967): a half-done extension is
// either a free slot or stones taken for nothing, and neither is recoverable by
// looking at the wire.
//
// The ORDER is the safety: the commander row is locked FIRST, so two Extend
// clicks (or two clients on one commander) are serialised and the second reads
// the FIRST one's slot - it pays the price for the slot it is actually buying.
//
// Every refusal RETURNS BEFORE ITS FIRST WRITE, which is the card's "no write"
// half: at the cap (GET_TEAMGROUP_MAXSLOT) and with a short bag the transaction
// is rolled back and both the property and the bag are untouched.
func (s *PGBarrackStore) ExtendTeamGroup(ctx context.Context, commanderID uint32, classID uint32) (ExtendResult, error) {
	var res ExtendResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("extend team group for commander %d: begin: %w", commanderID, err)
	}
	// A committed transaction answers ErrTxClosed here; ignoring that error is
	// the pgx idiom for "roll back if we never reached Commit".
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int32
	if err := tx.QueryRow(ctx,
		`SELECT commander_id FROM commander WHERE commander_id = $1 FOR UPDATE`,
		commanderID).Scan(&locked); err != nil {
		if isNoRows(err) {
			return res, fmt.Errorf("extend team group: commander %d is not in the database", commanderID)
		}
		return res, fmt.Errorf("extend team group for commander %d: lock: %w", commanderID, err)
	}

	// The CURRENT slot, read inside the lock. A missing row is the server-side
	// default the login path also uses (defaultTeamMax) - never 0, which is not a
	// barrack state but the absence of one.
	var slot int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE((SELECT value::smallint FROM commander_property
		                    WHERE commander_id = $1 AND property = 'Lock_TeamGroup'
		                      AND section = 0), $2)`,
		commanderID, defaultTeamMax).Scan(&slot); err != nil {
		return res, fmt.Errorf("extend team group for commander %d: read Lock_TeamGroup: %w", commanderID, err)
	}
	if slot < 0 {
		slot = 0
	}
	res.Slot = uint16(slot)
	res.NewSlot = res.Slot
	res.Need = TeamGroupPrice(res.Slot)

	if res.Slot >= TeamGroupMaxSlot {
		res.Outcome = ExtendAtCap
		return res, nil
	}

	// The bag, oldest stack first - the same order, filter and row locks the
	// zone's take uses (zone/items.go takeItem), so one stack is never spent
	// twice by two writers.
	rows, err := tx.Query(ctx,
		`SELECT s.item_id, i.count
		   FROM storage s JOIN item i ON i.item_id = s.item_id
		  WHERE s.commander_id = $1 AND s.type = 0 AND s.trash = 0
		    AND i.class_id = $2
		  ORDER BY s.insert_date, s.item_id
		  FOR UPDATE OF i`, commanderID, int32(classID))
	if err != nil {
		return res, fmt.Errorf("extend team group for commander %d: read stacks of %d: %w",
			commanderID, classID, err)
	}
	type stack struct {
		id   int64
		have int
	}
	var stacks []stack
	for rows.Next() {
		var st stack
		if err := rows.Scan(&st.id, &st.have); err != nil {
			rows.Close()
			return res, fmt.Errorf("extend team group for commander %d: scan stack: %w", commanderID, err)
		}
		stacks = append(stacks, st)
		res.Held += st.have
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("extend team group for commander %d: read stacks of %d: %w",
			commanderID, classID, err)
	}

	if res.Held < res.Need {
		res.Outcome = ExtendNoStones
		return res, nil
	}

	// Take the price, oldest first, deleting a stack that reaches zero (a
	// zero-count row is a bag slot the client still draws; the legacy proc
	// leaned on a cascade, this deletes both halves).
	left := res.Need
	for _, st := range stacks {
		if left <= 0 {
			break
		}
		spend := left
		if spend > st.have {
			spend = st.have
		}
		rest := st.have - spend
		left -= spend
		if rest == 0 {
			if _, err := tx.Exec(ctx,
				`DELETE FROM storage WHERE item_id = $1 AND commander_id = $2`,
				st.id, commanderID); err != nil {
				return res, fmt.Errorf("extend team group for commander %d: remove stack %d: %w",
					commanderID, st.id, err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM item WHERE item_id = $1`, st.id); err != nil {
				return res, fmt.Errorf("extend team group for commander %d: remove item %d: %w",
					commanderID, st.id, err)
			}
		} else if _, err := tx.Exec(ctx,
			`UPDATE item SET count = $2 WHERE item_id = $1`, st.id, rest); err != nil {
			return res, fmt.Errorf("extend team group for commander %d: shrink stack %d: %w",
				commanderID, st.id, err)
		}
	}
	if left != 0 {
		// Unreachable after the Held check above unless another writer moved a
		// stack between the two statements - and then the whole transaction goes.
		return res, fmt.Errorf("extend team group for commander %d: %d stone(s) short after the take",
			commanderID, left)
	}
	res.Taken = res.Need

	// The property is a ROW, not a column (barrack_store.go Commander reads it
	// the same way; the legacy COMMANDER table has no team/group/slot column
	// either). An UPSERT, because a commander who never had the row is at the
	// default and must get a real one now.
	if _, err := tx.Exec(ctx,
		`INSERT INTO commander_property (commander_id, property, section, value)
		 VALUES ($1, 'Lock_TeamGroup', 0, $2)
		 ON CONFLICT (commander_id, property, section)
		 DO UPDATE SET value = EXCLUDED.value`,
		commanderID, strconv.Itoa(int(res.Slot)+1)); err != nil {
		return res, fmt.Errorf("extend team group for commander %d: write Lock_TeamGroup: %w",
			commanderID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("extend team group for commander %d: commit: %w", commanderID, err)
	}
	res.Outcome = ExtendDone
	res.NewSlot = res.Slot + 1
	return res, nil
}

// Characters loads the character rows with their worn items, and derives the
// level and nextExp from exp through the exported table.
func (s *PGBarrackStore) Characters(ctx context.Context, commanderID uint32) ([]CharInfo, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT character_id, slot, name, teampos, gender, job, headtype, exp, hp, sp "+
			"FROM character WHERE commander_id = $1 ORDER BY slot", commanderID)
	if err != nil {
		return nil, fmt.Errorf("load characters: %w", err)
	}
	defer rows.Close()

	var out []CharInfo
	ids := []int32{}
	for rows.Next() {
		var (
			id  int32
			c   CharInfo
			exp int64
			hp  int64
			sp  int32
		)
		if err := rows.Scan(&id, &c.Slot, &c.Name, &c.TeamPos, &c.Gender,
			&c.Job, &c.HeadType, &exp, &hp, &sp); err != nil {
			return nil, fmt.Errorf("scan character: %w", err)
		}
		// level and nextExp come from the exp table, never from LIFE.
		c.Exp = exp
		c.Level = byte(LevelOf(exp))
		c.NextExp = ExpOf(int(c.Level) + 1)
		c.HP = hp
		c.SP = int64(sp)
		out = append(out, c)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Worn items: spot -> class id, and the "+N" reinforcement.
	wear, err := s.pool.Query(ctx,
		"SELECT w.character_id, w.spot, i.class_id, COALESCE(r.hit_count, 0) "+
			"FROM wear_item w JOIN item i ON i.item_id = w.item_id "+
			"LEFT JOIN item_reinforce r ON r.item_id = w.item_id "+
			"WHERE w.character_id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("load worn items: %w", err)
	}
	defer wear.Close()

	index := map[int32]int{}
	for i, id := range ids {
		index[id] = i
	}
	for wear.Next() {
		var (
			charID int32
			spot   int
			class  int64
			hit    int
		)
		if err := wear.Scan(&charID, &spot, &class, &hit); err != nil {
			return nil, fmt.Errorf("scan worn item: %w", err)
		}
		i, ok := index[charID]
		if !ok || spot < 0 || spot >= equipSpotLast {
			continue
		}
		out[i].Equip[spot] = uint32(class)
		if hit < 0 {
			hit = 0
		}
		if hit > 255 {
			hit = 255
		}
		out[i].Reinforce[spot] = byte(hit)
	}
	// Promotion steps, folded in the way the original does it: promotionLv is
	// min(band(level), PromotionStep), and cashPromotionLv is the raw step.
	steps, err := s.pool.Query(ctx,
		"SELECT character_id, property, value FROM character_property WHERE character_id = ANY($1)", ids)
	if err == nil {
		defer steps.Close()
		for steps.Next() {
			var charID int32
			var prop, val string
			if steps.Scan(&charID, &prop, &val) != nil {
				continue
			}
			i, ok := index[charID]
			if !ok {
				continue
			}
			n, convErr := strconv.Atoi(val)
			if convErr != nil {
				continue
			}
			switch prop {
			case "PromotionStep":
				if band := PromotionBand(int(out[i].Level)); n > band {
					n = band
				}
				out[i].Promotion = uint16(n)
			case "CashPromotionStep":
				out[i].CashPromotion = uint16(n)
			}
		}
	}
	return out, wear.Err()
}

// TeamGroups loads the player's saved team groups.
//
// A character id is resolved to its SLOT because that is what the wire struct
// carries (geShared.h:388). A member whose character was deleted, or one that
// was never set, becomes EmptyCharSlot - never 0, which is a real character.
func (s *PGBarrackStore) TeamGroups(ctx context.Context, commanderID uint32) ([]TeamGroup, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT g.name, c1.slot, c2.slot, c3.slot "+
			"FROM team_group g "+
			"LEFT JOIN character c1 ON c1.character_id = g.character1 "+
			"LEFT JOIN character c2 ON c2.character_id = g.character2 "+
			"LEFT JOIN character c3 ON c3.character_id = g.character3 "+
			"WHERE g.commander_id = $1 ORDER BY g.idx", commanderID)
	if err != nil {
		return nil, fmt.Errorf("load team groups: %w", err)
	}
	defer rows.Close()

	var out []TeamGroup
	for rows.Next() {
		var (
			g          TeamGroup
			s1, s2, s3 *int16
		)
		if err := rows.Scan(&g.Name, &s1, &s2, &s3); err != nil {
			return nil, fmt.Errorf("scan team group: %w", err)
		}
		for i, sp := range []*int16{s1, s2, s3} {
			g.Slots[i] = EmptyCharSlot
			if sp != nil && *sp >= 0 {
				g.Slots[i] = byte(*sp)
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
