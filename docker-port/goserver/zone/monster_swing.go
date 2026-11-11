// THE MONSTER'S OWN SWING - SLICE 1, SINGLE TARGET ONLY.
//
// WHAT THIS FILE DOES. A monster this zone is running (a body a script created,
// with a live HP ledger and a row behind it) looks for the NEAREST player character
// inside its OWN attack reach, and swings on its own cadence. One swing is ONE
// `ZC_ATTACK_SINGLE` (3564, 74 bytes):
//
//	the monster's CURRENT handle and position            (+4, +8)
//	its attackSpd                                        (+20)
//	the victim's handle and position                     (+26, +30)
//	the damage as a float64                              (+42)
//	the victim's POST-HIT HP as a float64                (+50)
//	hit type 0 - no knockdown                            (+58)
//
// The victim's HP falls BEFORE the packet is built, so +50 is the new HP. The packet
// goes through `Handler.fanOut(nil, monsterHandle, pkt)` - the SAME per-viewer
// fan-out a player's swing uses (`sendOthers`), so the R135e ViewMiddleRange cull and
// the R93 observers path apply to a monster's swing for free. It is a BROADCAST to
// every session whose view holds the MONSTER, not a unicast to the victim:
// `history\monster-attack.md` measured that on the reference
// (`FieldChar.cpp:419-431` `BroadcastPacket`), and it is why a watcher sees the same
// bytes the victim does.
//
// THE EVIDENCE, verified in the tree before this file was written:
//
//	fields          history\monster-attack.md; geZCPacket.h:1438 (PKS_ZC_ATTACK_SINGLE
//	                = PKS_VARSIZE + PKS_ZC_ATTACK + HIT_INFO), geZCTypes.h:542
//	builder         zone\attack.go:258 BuildAttackSingle (74 bytes, damage/HP float64)
//	our capture     data\captures\liveattack01\s2c_7061_52236.bin - decoded: 37 x 3564,
//	                every one 74 bytes, one victim handle, two monster attackers,
//	                attackSpd 400 in all 37, type 2/3 (the reference rolls knockdowns;
//	                this slice does not - see below)
//
// SCOPE, SAID OUT LOUD SO NOBODY ASSUMES MORE: no splash (3566), no miss / AR-vs-DR
// roll, no knockdown, no respawn change, no drops, no player-death or resurrection
// flow. A victim that reaches 0 HP is logged as `zone: MONSTER KILL ...` and is
// skipped from then on; nothing else is sent for it, because a death screen is a
// different slice. The damage rule is DETERMINISTIC (no random roll) and reuses the
// player's own plain-swing arithmetic - see monsterSwingDamage.
//
// TICKING. It rides the zone's existing clock: `Handler.tickIfDue` (zone\chase.go)
// calls `monsterSwingTick` on every pass, the same clock `deadTick` and the sight ring
// use. There is no goroutine per monster and no timer of its own. `monsterSwingArmed`
// is what keeps a session's loop waking while a body that CAN swing stands in the
// zone, exactly as `deadPending` does for a death.
package zone

import (
	"math"
	"strconv"
	"time"
)

// monsterSwingType is the ATTACKTYPE a monster's swing states at +58: 0, a plain hit
// with no knockdown and no trailing KNOCKDOWN_INFO.
//
// THE CAPTURE'S OWN TYPES ARE 2 AND 3, and that is not a contradiction: the reference
// rolls a knockdown for a monster's hit (`hit.type = ATTACKTYPE_KNOCKDOWN`, or 3 when
// it crits - FieldChar.cpp:380-400) and this slice deliberately does not. 0 is the
// one flavour with nothing extra for the client to consume.
const monsterSwingType int32 = 0

// monsterATK is `SCR_Get_MON_DAM(self)` for a monster with no owner - the game's own
// attack-power formula, quoted from get_status_monster.lua:398-471:
//
//	statfix  = self.STR / 5
//	levelfix = self.Lv * 0.05 + 1
//	atk      = math.floor(self.Wpn * statfix * levelfix / self.Hits * 1.8)
//	atk      = math.floor(atk * (100 + self.ATK_BM) / 100)
//	if atk < 0 then atk = 0
//
// The owner branches (a summon's artifact/weapon/buff terms) do not apply: a monster
// this zone runs has no owner, which is the `GetOwner(self) == nil` path.
//
// `Hits` is the SHAPE row's column (datatable_monster_shape.xml), joined at load time -
// the Monster table does not carry it. Hits <= 0 means this body's shape is unknown and
// the divisor does not exist, so the ATK is 0 rather than a guess: a monster with no
// ATK does not swing (monsterSwingTick).
func monsterATK(row MonsterRow) float64 {
	if row.Hits <= 0 {
		return 0
	}
	atk := math.Floor(row.Wpn * (row.STR / 5) * (float64(row.Level)*0.05+1) / row.Hits * 1.8)
	atk = math.Floor(atk * (100 + row.ATKBM) / 100)
	if atk < 0 {
		return 0
	}
	return atk
}

// monsterASPD is `SCR_Get_MON_ASPD(self)` for the same no-owner path
// (get_status_monster.lua:496-521):
//
//	value = self.ASPD * (100 - self.ASPD_BM) / 100
//	if value < 400 then value = 400 elseif value > 10000 then value = 10000
//	return math.floor(value)
//
// It is a DELAY in milliseconds, and it is used twice by the reference: the FSM
// re-arms with it (`m_nextAttackTime = timeGetTime() + GetAttackSPD()`,
// FieldChar_FSM.cpp:135) and the 3564 carries the same number at +20. This zone does
// the same thing with one value, so the packet cannot advertise a rhythm the tick does
// not keep.
func monsterASPD(row MonsterRow) float64 {
	value := row.ASPD * (100 - row.ASPDBM) / 100
	if value < float64(monsterMinCadence/time.Millisecond) {
		value = float64(monsterMinCadence / time.Millisecond)
	}
	if value > 10000 {
		value = 10000
	}
	return math.Floor(value)
}

// monsterSwingDamage is the damage of ONE monster swing, and it is built from the two
// pieces of arithmetic the PLAYER's plain swing already uses - no second formula:
//
//	deal   = castDamage(monATK, plainAttackPercent, 0)    zone\attack.go:989 (the
//	                                                      plain swing's own base)
//	def    = (victimDEF - 0) / 2 - 0 / 2                  common_pc.lua:2606, the
//	                                                      `arg.Attacker == 'monster'`
//	                                                      line for a PC target
//	damage = applyDefence(deal, def)                      zone\monster_def.go:293
//
// THE TWO ZEROES ARE SAID, NOT HIDDEN. `immune` is 0 because a monster's plain swing
// carries no IgnoreDEF/DefIP; the attacker's `AR` is 0 because no AR column is modelled
// for a monster yet (history\monster-attack.md "Still open" #2) and inventing one would
// change the defence by half of a number nobody measured. There is NO miss roll in this
// slice for the same reason: the swing always lands.
func monsterSwingDamage(monATK, victimDEF float64) float64 {
	deal := castDamage(monATK, plainAttackPercent, 0)
	return applyDefence(deal, victimDEF/2)
}

// swingVictim is one player character a monster swing can land on: the session it
// belongs to (for the live HP ledger), its index in that team, its handle and where its
// BODY stands right now.
type swingVictim struct {
	sess   *Session
	idx    int
	handle uint32
	pos    [3]int32
}

// nearestPlayerInReach answers the NEAREST player character whose body is inside this
// monster's own reach, or false when no player is.
//
// THE REACH IS THE REFERENCE'S: `CMonsterNPC::GetAttackRange` is the row's own
// MinR..MaxR (MonsterNPC.cpp:1035-1046) and it is measured EDGE TO EDGE - the same
// `attackRangeAllowed` (zone\attack_approach.go:486) the player's own attack uses, with
// the monster's body radius from its row and the player's fixed 50. No second reach
// rule is written here.
//
// THE BODY, NOT THE DESTINATION: the candidate point comes from `sessionBodyAt`, the
// same reader the sight ring uses, so a victim who is mid-walk is measured where it is
// DRAWN (R135d's rule), not where it clicked to.
//
// A VICTIM AT 0 HP IS NOT A TARGET. The brief's own rule: once a body is emptied the
// monster stops swinging at it - and skipping it here is also what lets the monster
// pick somebody else instead of standing over the corpse.
func (h *Handler) nearestPlayerInReach(m Monster, now time.Time) (swingVictim, bool) {
	monPos := [3]int32{m.X, m.Y, m.Cell}
	monR := h.monsterBodyRadius(m)

	var best swingVictim
	bestD2 := -1.0
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			c := &mem.Sess.Team[j]
			if c.HP <= 0 {
				continue // an emptied body is not swung at again
			}
			pos := h.sessionBodyAt(mem.Sess, j, now)
			if _, in := attackRangeAllowed(monPos, pos, m.MinR, m.MaxR, monR, PCBodyRadius); !in {
				continue
			}
			d2 := distSq(monPos[0], monPos[1], pos[0], pos[1])
			if bestD2 < 0 || d2 < bestD2 {
				best = swingVictim{sess: mem.Sess, idx: j, handle: c.Handle, pos: pos}
				bestD2 = d2
			}
		}
	}
	return best, bestD2 >= 0
}

// claimMonsterSwing answers whether this monster may swing NOW, and stamps the swing
// when it may. The check and the stamp are ONE locked step, so two sessions ticking at
// the same instant cannot both swing the same monster: the loser sees the winner's
// stamp and answers false.
//
// The clock is per handle because the cadence is per row (SCR_Get_MON_ASPD). A monster
// that has never swung is due at once - the reference swings on entering AS_ATTACK, not
// one cadence later.
func (h *Handler) claimMonsterSwing(handle uint32, now time.Time, cadence time.Duration) bool {
	h.swingMu.Lock()
	defer h.swingMu.Unlock()
	if h.monsterSwingAt == nil {
		h.monsterSwingAt = make(map[uint32]time.Time, 4)
	}
	if last, ok := h.monsterSwingAt[handle]; ok && now.Sub(last) < cadence {
		return false
	}
	h.monsterSwingAt[handle] = now
	return true
}

// monsterSwingArmed reports whether ANY monster in this zone could swing, which is what
// tells a session's loop to keep ticking (zone\chase.go tickIfDue) - the sibling of
// deadPending. It is a cheap read: the roster is a short slice and the run only asks
// whether one row names an attack at all.
func (h *Handler) monsterSwingArmed() bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for _, m := range h.spawnedMonster {
		if m.ATK <= 0 || m.MaxR <= 0 || m.ASPD <= 0 || m.AINoAttack {
			continue
		}
		if hp, ok := h.monsterHP[m.Handle]; ok && hp > 0 {
			return true
		}
	}
	return false
}

// monsterSwingTick is the zone's ONE monster clock. It rides `tickIfDue`'s pass: no
// goroutine, no timer, and it writes its own packets because a monster has no owning
// session to return them to (deadTick's broadcast has the same shape).
//
// For each body a script created: skip a dead or HP-less one, skip a row that names no
// attack OR whose tactics row says `Attack="FALSE"`, find the nearest player in reach,
// claim the body's cadence, then swing. Every step is guarded so a body that cannot
// act costs nothing and never logs per tick.
func (h *Handler) monsterSwingTick(now time.Time) {
	for _, m := range h.runtimeMonsters() {
		if m.ATK <= 0 || m.MaxR <= 0 || m.ASPD <= 0 {
			continue // this row names no attack: no reach, no power or no shape Hits
		}
		if m.AINoAttack {
			continue // the tactics row says Attack="FALSE": this body never swings
		}
		if hp, ok := h.monsterLiveHP(m.Handle); !ok || hp <= 0 {
			continue // dead or not a body this zone keeps HP for
		}
		// THE SCRIPT'S STOP IS THE ONLY THING THAT PARKS THIS BODY: `monsterMaySwing`
		// answers TRUE unless an explicit `StopAttack` recorded a stop
		// (zone\monster_order.go), so a zone nobody runs a script on swings exactly as
		// it did before the movement/attack calls were bound.
		if !h.monsterMaySwing(m.Handle) {
			continue
		}
		v, ok := h.nearestPlayerInReach(m, now)
		if !ok {
			continue
		}
		cadence := time.Duration(m.ASPD) * time.Millisecond
		if !h.claimMonsterSwing(m.Handle, now, cadence) {
			continue
		}
		h.monsterSwing(m, v, now)
	}
}

// monsterSwing lands ONE swing: the ledger move, the packet, the fan-out, the lines.
//
// THE ORDER IS THE CONTRACT. The victim's HP is written FIRST, then the packet is built
// from the NEW number, then it goes out: +50 is the post-hit HP (measured,
// history\combat-attack-heal.md §2.1: +50 equals the 3557's +6 of the same hit), and a
// packet built before the subtraction would tell every client an HP the server no
// longer holds.
//
// THE LEDGER IS Character.HP (float64, zone\zone.go:98). `swingMu` makes the
// read-modify-write one step against another monster swinging at the same body in the
// same tick.
func (h *Handler) monsterSwing(m Monster, v swingVictim, now time.Time) {
	sheet := h.sheetFor(v.sess.Team[v.idx])
	damage := monsterSwingDamage(m.ATK, sheet.DEF)

	h.swingMu.Lock()
	before := v.sess.Team[v.idx].HP
	after := before - damage
	if after < 0 {
		after = 0
	}
	v.sess.Team[v.idx].HP = after
	h.swingMu.Unlock()

	pkt := BuildAttackSingle(m.Handle, [3]int32{m.X, m.Y, m.Cell}, uint16(m.ASPD),
		v.handle, v.pos, damage, after, monsterSwingType)
	sent := h.fanOut(nil, m.Handle, pkt)

	h.Log.Printf("zone: MONSTER SWING %d %q hits %d (%q) for %g: HP %g -> %g, "+
		"attackSpd %g ms from the row, cmd %d (%d B) to %d session(s) - one 3564, "+
		"single target, no splash and no knockdown",
		m.Handle, h.MonsterName(m), v.handle, v.sess.Team[v.idx].Name, damage,
		before, after, m.ASPD, CmdZCAttackSingle, len(pkt), sent)

	if after == 0 {
		// NO DEATH SCREEN IS BUILT IN THIS SLICE. The line is the observable, and the
		// emptied body is skipped by nearestPlayerInReach from now on.
		h.Log.Printf("zone: MONSTER KILL %d %q killed player %d (%q) - HP 0. This slice "+
			"does not build a player-death or resurrection flow; the monster stops "+
			"swinging at that body",
			m.Handle, h.MonsterName(m), v.handle, v.sess.Team[v.idx].Name)
	}
}

// MonsterSwingStartupLine is the ONE startup line about the monster's swing, printed by
// main: where the cadence comes from AND what this slice deliberately does not do.
//
// IT SAYS WHERE THE NUMBER COMES FROM because the two sources could disagree. The live
// capture we own was expected to answer the cadence with per-packet timestamps, but the
// stored stream (data\captures\liveattack01\s2c_7061_52236.bin) is a flattened byte
// stream with NO timestamps - capture.txt has src/seq/len and no clock - so an interval
// cannot be measured from it. What it DOES pin is the packet's own +20: 400 in all 37
// samples, which is the floor of `SCR_Get_MON_ASPD`'s formula. The cadence therefore
// comes from the row's ASPD, which is the same number the reference re-arms its FSM with
// (`FieldChar_FSM.cpp:135`).
func MonsterSwingStartupLine() string {
	return "zone: monster swing slice 1: cadence is each row's own ASPD through " +
		"SCR_Get_MON_ASPD (ASPD*(100-ASPD_BM)/100, floored at " +
		strconv.Itoa(int(monsterMinCadence/time.Millisecond)) + " ms, capped at 10000 ms) - " +
		"the same value the 3564 carries at +20. The stored liveattack01 capture has no " +
		"packet timestamps, so the row is the source; its 37 x 3564 all carry attackSpd " +
		"400, the formula's floor. SCOPE: single target only - no 3566 splash, no miss / " +
		"AR-vs-DR roll (no AR column is modelled, so every swing lands), no respawn change, " +
		"no drops, no player-death or resurrection flow."
}

// monsterMinCadence is SCR_Get_MON_ASPD's floor, named so the startup line and the
// formula cannot drift apart.
const monsterMinCadence = 400 * time.Millisecond
