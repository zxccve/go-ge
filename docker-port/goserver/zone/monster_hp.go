// THE MONSTER'S MAXIMUM HP, READ FROM THE GAME'S OWN SCRIPT.
//
// WHY THIS FILE EXISTS. A monster's max HP is NOT a column of the Monster table:
// `cache/datatable_monster.xml` carries `HP`, `RHP`, `MHPFix`, `MHP_BM`, `CP_MON_MHP`
// and 248 more columns, but never `MHP` - so `MonsterRow.MHP` reads 0 for every one
// of the 7,725 rows, and a spawn that passes no MHP drew a body with HP 0. The
// client then has no max HP for it (no target frame, no bar) and this zone has no
// ledger entry, so the body could not be hit and never swung (zone\monster_swing.go
// guards on `monsterHP > 0`).
//
// THE RULE IS THE GAME'S OWN ENTRY POINT, not a re-port:
//
//	SCR_Get_MON_MHP(self)      monster\get_status_monster.lua:13-64
//
//	value = (150 + Lv*15) * (CON/5) * ((Lv/2 + 50)/100) * 0.8      (the ordinary body)
//	value = value * (100 + MHP_BM)/100                             (then the row's percents)
//	value = value * (MHPFix*0.01 + 1)
//	value = value + ADDMHP_BM
//	return math.floor(value)
//
// with three fixed-class branches ahead of it (RaceType 'Home'/'Colony', class
// 15041, and the summon classes 16664/16665/12973/12974/40345..40359). It is the
// SAME host, the SAME `self` and the SAME one-call-per-run discipline as
// `SCR_Get_MON_DEF` in zone\monster_def.go - the two are siblings in one corpus
// file, and one of them already runs.
//
// WHAT THE SCRIPT NEEDS FROM `self` (monsterStatusSelf in zone\monster_def.go):
// Lv, CON, ClassID, RaceType, MHPFix, MHP_BM, ADDMHP_BM - every one a column of the
// row - plus `GetOwner(self)` (answers nil: no ownership in this build) and
// `IsWorldPVPServer()` (answers 0: a PVE zone). Both are already bound by
// script\status.go for the DEF script.
//
// AN UNANSWERED SCRIPT IS NOT A NUMBER. A nil source, a raise or a non-number leaves
// hp 0 and the caller logs ONCE why - a made-up maximum is how a body that cannot be
// hit starts to look healthy.
package zone

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"geserver/script"
)

// MonsterHPEntry is the script entry point that answers a monster's maximum HP,
// `SCR_Get_MON_MHP`, named by every monster row's own `CP_MON_MHP` column.
//
// TWO ROWS OF THE 7,725 NAME `SCR_Get_SUMMON_MHP` INSTEAD (56391/56392,
// Summon_Thorn_Effect / Summon_Thorn_Debuff), and the corpus does not define that
// function: they are summon EFFECTS, not bodies a `CreateTmonster` makes in this
// build, so this source runs `SCR_Get_MON_MHP` for every spawn. If a summons lane
// ever spawns them it must read the row's own `CP_MON_MHP` and bind that entry.
const MonsterHPEntry = "SCR_Get_MON_MHP"

// MonsterHPScript is ONE source: the game's own `SCR_Get_MON_MHP`, run for a
// monster. NIL MEANS OFF, and a spawn that passed no MHP then keeps 0 and says why.
//
// It is the same host the monster's DEF, the damage script and the Character Info
// source run on, so its `self` is installed for exactly the length of one call
// (`script.Host.RunStatus` -> `withStatus`), and a raise is RETURNED, never raised
// through the zone's stack.
type MonsterHPScript struct {
	Host *script.Host
	// File is the corpus file the entry point was loaded from, for the startup line -
	// asked of the index, never assembled here.
	File string
	Log  func(format string, args ...any)

	// runMu serialises the run on the shared host, the same rule MonsterDefScript
	// follows: a second spawn must never see another monster's `self`.
	runMu sync.Mutex
}

// NewMonsterHPScript loads the entry point ONCE, at zone startup. An error means the
// zone keeps 0 for a spawn that passed no MHP and says so.
func NewMonsterHPScript(host *script.Host, logf func(string, ...any)) (*MonsterHPScript, error) {
	if host == nil {
		return nil, errors.New("no script host (this zone was started without one)")
	}
	if _, err := host.EnsureScript(MonsterHPEntry); err != nil {
		return nil, err
	}
	return &MonsterHPScript{Host: host, File: host.ScriptFile(MonsterHPEntry), Log: logf}, nil
}

// MaxHP runs the game's own `SCR_Get_MON_MHP` for ONE monster and answers the HP it
// computed.
//
// ok=false plus an error means the script did not answer - a raise, a missing entry
// point, a non-number - and the caller keeps 0 and says so. NOTHING HERE INVENTS A
// VALUE: 0 is not a "small" max HP, it is "not answered", and the caller treats it
// as such.
func (d *MonsterHPScript) MaxHP(m Monster) (hp float64, ok bool, err error) {
	if d == nil || d.Host == nil {
		return 0, false, errors.New("no monster HP script is armed")
	}

	var raised any
	d.runMu.Lock()
	func() {
		// A panic in the Lua binding must not take the spawn down with it.
		defer func() { raised = recover() }()
		results := d.Host.RunStatus(nil, monsterStatusSelf(m),
			[]script.StatusEntrySpec{{Label: "MHP", Script: MonsterHPEntry}})
		if len(results) != 1 {
			err = fmt.Errorf("%s answered %d result(s), want 1", MonsterHPEntry, len(results))
			return
		}
		if results[0].Err != nil {
			err = results[0].Err
			return
		}
		hp = results[0].Value
	}()
	d.runMu.Unlock()

	if raised != nil {
		return 0, false, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	if err != nil {
		return 0, false, err
	}
	return hp, true, nil
}

// monsterMaxHP reads a spawn's maximum HP through the game's own script, as armed at
// zone startup (Handler.MaxHP).
//
// AN UNARMED SOURCE IS AN ERROR, NOT A ZERO. 0 is a real "no HP this zone can name"
// answer, and the caller must log the reason instead of drawing a body that cannot
// be hit.
func (h *Handler) monsterMaxHP(m Monster) (float64, bool, error) {
	if h == nil || h.MaxHP == nil {
		return 0, false, errors.New("no monster HP source is armed (Handler.MaxHP is nil)")
	}
	return h.MaxHP.MaxHP(m)
}

// MonsterHPByHandle answers the HP this zone's LEDGER holds for a monster, without
// running a script or changing anything. It exists for the -spawndummy probe: the
// line must name the number the client was actually told, so the next reader can
// compare the bar on screen with the ledger without a query.
func (h *Handler) MonsterHPByHandle(handle uint32) (float64, bool) {
	return h.monsterLiveHP(handle)
}

// int32HP is the LEGACY int32 copy of an HP (Monster.HP), clamped to the int32
// range. It is a display copy only: the 3527 and the ledger both use the float64.
//
// A VALUE TOO LARGE FOR INT32 KEEPS MaxInt32 RATHER THAN WRAPPING. The reference's
// Scarecrow sits near 1.0e12, and a naive int32() would turn that into a negative
// number - which is exactly how a wrapped HP becomes a full bar over a dead body. The
// packet never sees this value (zone\monster.go emHP is a float64), but the field is
// kept honest for every older reader.
func int32HP(hp float64) int32 {
	if hp <= 0 || math.IsNaN(hp) {
		return 0
	}
	if hp > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(hp)
}

// logMaxHPUnavailable says ONCE per zone why a spawn that passed no MHP is being
// drawn with 0. The brief's rule: keep 0, never invent a number, and let a reader
// find the reason - a line per body would bury the first one in a spawn storm.
func (h *Handler) logMaxHPUnavailable(class uint32, name string, err error) {
	if h == nil {
		return
	}
	h.spawnMu.Lock()
	warned := h.hpSourceWarned
	h.hpSourceWarned = true
	h.spawnMu.Unlock()
	if warned {
		return
	}
	if h.Log != nil {
		h.Log.Printf("zone: CREATE_TMONSTER class %d (%q) has NO max HP: the spawn "+
			"passed none and %s did not answer (%v) - the body is drawn with hp 0, "+
			"cannot be hit and will not swing. This is the ONE line about it for this "+
			"zone; an explicit MHP on the spawn, or an armed HP script, is the fix",
			class, name, MonsterHPEntry, err)
	}
}
