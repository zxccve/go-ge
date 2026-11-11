// RESPAWN - SLICE 1: A BODY THIS ZONE CREATED DIES PROPERLY, AND COMES BACK.
//
// WHAT WAS WRONG. takeHP armed one ZC_DEAD (3586) and the zone stopped there: the client
// drew a corpse for ever, the body stayed in the roster at 0 HP, and a field emptied over
// a session because nothing ever made another one.
//
// THE REFERENCE, from `history/monster-death.md` §5 (given for this job, re-read rather
// than re-derived):
//
//   - the corpse LEAVES after `ZC_DEAD`, with `ZC_LEAVE_MONSTER` (3531, 8 bytes)
//     and REASON 0. Reason 0 is `OBJ_LEAVE_LOGOUT`, "the object is gone": only reason 3
//     (`OBJ_OUTOFSIGHT`) makes the client FADE an object, and a dead monster must be
//     dropped at once - the same two constants `KillMonsterObject` already uses
//     (zone/near.go, zone/monster.go CmdZCLeaveMonster);
//   - the species comes back at the SAME spawn point, with a NEW entity id - a fresh
//     body, not a revival of the old handle.
//
// HOW LONG THE CORPSE STAYS is the SHAPE's own `DyingTime`, with a floor: the client
// plays the death animation ITSELF, from the shape data (`DIE1` / `DN` / `DeadEffect`
// in `cache/datatable_monster_shape.xml`), and it needs the body to still exist while
// the animation runs.
//
// THE FLOOR IS 600 ms, AND IT IS THE REFERENCE'S NUMBER (2026-11-07). `history/
// monster-death.md` §5 records the corpse leaving 0.60 s after `ZC_DEAD` - ONE
// recording, and that caveat is still true - and `GetDyingTime()`'s body is not in our
// trees, so a shape that states 0 (the Bigfoot, `Monster_Bigfoot` -> `DyingTime="0"`)
// gets the floor. The 3000 ms floor this file used to carry was OURS: a reading of a
// comment, never a measurement. It is gone because it was wrong at the other end - the
// owner watched the body lie on the ground long after the fall had finished ("death
// fall too long before body gone"). A shape that states its own longer number (2,033 /
// 3,500 ms in the shipped table) keeps it, so nothing that HAS a measured animation
// time is shortened by this change.
//
// THE DELAY IS THE BODY'S OWN SPAWNER ROW, AND NOTHING ELSE (2026-11-08). The map's
// `mongentype_<map>` table carries a `RespawnTime` per spawner, and a body the map
// spawner placed carries that number from its spawn (zone/spawner.go, Monster.
// SpawnerDelay) through its corpse to the body that comes back. The reference reads
// the same column (`CMonsterGen::IsAllowCreateMonster`, MonsterGen.cpp:158-215), and
// the shipped rows prove the shape: 5,000 ms is the most common value, with 900,000
// and 1,800,000 present for elite and boss spawners.
//
// A BODY WITH NO SPAWNER ROW DOES NOT COME BACK. A probe (`-spawnmonster`,
// `-spawndummy`), a script's `CreateTmonster`, and every body on a map whose spawner
// type table is absent (map 1331 fld_auch is one: 54 flag rows, no
// `mongentype_fld_auch`) has no row to read a number from, so NO number is invented:
// the corpse still leaves the field on its shape's clock, and the zone says ONCE per
// class that this body will not come back. The 23.568 s that used to stand in for a
// missing row is DELETED - it was one recording's interval, it was never any map's
// rule, and a number that stands in for "unknown" is exactly what the owner ruled
// out: "i dont want fallback, it can confuse or mislead us".
//
// The `-respawn` DEVELOPMENT flag still wins over the row, because its whole purpose
// is to shorten a probe's wait: precedence is override > spawner row > NOTHING.
//
// SCOPE: only bodies the zone created at runtime (`Handler.SpawnMonsterObject` - the
// probes and a script's `CreateTmonster`). The criterion is the roster: `spawnedMonster`
// is exactly what that function made. The map's static NPCs/objects are not touched, the
// fixed S1 body is not in that list and has no HP ledger, so it cannot reach here.
//
// OM_KILL IS WIRED TO A SCRIPT NOW (2026-11-08), and it uses THIS chain rather than its
// own: `KillMonsterObject` (`zone\near.go`) does not remove the body - it makes it a
// zombie and arms the same corpse a killing hit arms, so the leave is this file's
// reason-0 3531 after max(DyingTime, the 600 ms floor) and the return is this file's
// `respawnDelayInForce()`. A body whose corpse is ALREADY owed (a killing hit a moment
// earlier) is not armed a second time: two corpses for one handle would be the
// double-leave this note used to warn about.
package zone

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

const (
	// CorpseLeaveFloor is the MINIMUM time between the ZC_DEAD and the corpse's
	// leave: 600 ms. The actual interval is max(the shape's `DyingTime`,
	// CorpseLeaveFloor) - see corpseLeaveDelay.
	//
	// WHY 600 ms. It is the reference's own measured number: `history/
	// monster-death.md` §5, a corpse that left 0.60 s after `ZC_DEAD` with 3531
	// reason 0. It is ONE recording (the caveat stands), but it is a measurement,
	// where the 3000 ms this constant used to hold was ours - a reading of a comment,
	// and one the owner felt on the field as a body that lay on the ground far too
	// long after the fall ("death fall too long before body gone"). A shape that
	// states its own longer `DyingTime` (2,033 / 3,500 ms in the shipped table) keeps
	// that number, so the floor only decides the shapes that state 0 or nothing.
	//
	// It is measured from the instant the ZC_DEAD LEAVES, which since 2026-11-07 is
	// the killing hit itself (zone/create.go damageMonsterBy), not a tick.
	CorpseLeaveFloor = 600 * time.Millisecond

	// THE REFERENCE'S TWO DEATH CLOCKS (source2 ZServerLib\MonsterNPC.cpp:2588-2602,
	// 2653-2655). `leaveDelay` is set when the body enters BS_DEAD; the leave follows at
	// kill+leaveDelay and the ZC_DEAD at leaveAt-DeathPacketLead, so the client draws the
	// fall 500 ms before the body is taken away.
	DeathPacketLead         = 500 * time.Millisecond
	CorpseLeaveAttack       = 2000 * time.Millisecond // killed during an attack animation
	CorpseLeaveIdle         = 5000 * time.Millisecond // a death that is not an attack (script OM_KILL)
	CorpseLeaveDeadAniFloor = 3000 * time.Millisecond // 1000 + the shape's DyingTime, floored

	// THE 23.568 s FALLBACK IS DELETED (2026-11-08) and this comment is the only
	// place left that names it. It was ONE recording's interval (history\
	// monster-death.md §5) and it was answered for every body with no spawner row
	// behind it - a probe, a script's `CreateTmonster`, a map with no type table -
	// so a missing row read as a rule. The owner: "i dont want fallback, it can
	// confuse or mislead us". There is no constant here any more: respawnDelayFor
	// answers the row or nothing, and a body with neither the row nor the flag is
	// not brought back at all.
)

// respawnOverride is the `-respawn` DEVELOPMENT flag's interval, in NANOSECONDS, and 0
// means "no override": the spawner row's own number is then the only clock. main.go
// declares the flag and SetRespawnDelay below is its only writer.
//
// WHY IT EXISTS. 23.568 s of waiting (the deleted fallback) was one recording, and a
// map's own `RespawnTime` can be 900 s; either is a long time to stand in a field for
// one kill to run its whole chain - corpse, leave, return - so a probe of the death or
// of the return would spend most of its run waiting. The flag shortens the interval for
// the WHOLE zone, which is the honest scope: the corpse timing it must not touch is
// untouched, and a run without the flag uses the row's number.
//
// IT IS AN ATOMIC RATHER THAN A PLAIN VAR because the kill path is not one goroutine
// (zone\create.go damageMonsterBy runs on whichever session's reader took the hit), and a
// race-detector run must not find this one. The value is set once at startup and never
// changes after it, so the load is uncontended.
var respawnOverride atomic.Int64

// SetRespawnDelay sets the zone-wide return interval: the `-respawn` development flag
// (main.go) calls it once at startup, and 0 removes the override, leaving each body's
// spawner row as its only clock (a body with no row then does not come back at all). A
// negative value is stored as 0, because it would only make a body's return due before its
// corpse has left - a probe would read that as a broken timer, not as a flag.
func SetRespawnDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	respawnOverride.Store(int64(d))
}

// respawnDelayFor answers the interval ONE body's return is armed with, and whether
// there IS a return at all, in this order:
//
//  1. the `-respawn` DEVELOPMENT override, when one is set - a probe flag whose
//     whole purpose is to shorten the wait, so it wins over every row;
//  2. the body's own spawner `RespawnTime` (Monster.SpawnerDelay, set by
//     zone/spawner.go from the map's `mongentype` row) - the reference's rule;
//  3. nothing, (0, false): the body has no spawner row and no flag was passed, so it
//     does not come back. THERE IS NO THIRD NUMBER - the 23.568 s this used to answer
//     is deleted (the const block above says why), and a test greps the package for
//     it so it cannot come back as a default.
//
// A ROW THAT SAYS 0 IS A ROW. (0, true) is a real answer: the map's spawner says the
// body returns at once (its corpse still leaves first - takeDueRespawns never takes a
// corpse that has not left). `HasSpawner` is what separates that from a missing row;
// `SpawnerDelay > 0` would silently read a stated 0 as absent, which is the mistake
// this pair of fields exists to make impossible.
//
// This is the ONE reader the return time goes through (armCorpse below), so the flag
// and the row cannot drift apart.
func respawnDelayFor(m Monster) (time.Duration, bool) {
	if d := time.Duration(respawnOverride.Load()); d > 0 {
		return d, true
	}
	if m.HasSpawner {
		return m.SpawnerDelay, true
	}
	return 0, false
}

// respawnDelaySourceFor names the interval's origin for the CORPSE LEAVE line and the
// OM_KILL line, so a reader of the log can tell a spawner row from a probe's override
// without having the command line in front of them. It is only read when
// respawnDelayFor answered true; the no-row case has its own sentence
// (respawnReturnTextFor).
func respawnDelaySourceFor(m Monster) string {
	if respawnOverride.Load() > 0 {
		return "the -respawn DEV FLAG's number, NOT this body's spawner row"
	}
	return "this body's SPAWNER row (mongentype RespawnTime " + m.SpawnerDelay.String() + ")"
}

// respawnReturnTextFor is what one corpse's return is, in the log's own words: the
// interval and where it came from, measured from `from`, or the plain sentence that
// there is no return. `from` is the death for the CORPSE LEAVE line (the instant the
// return was armed) and the current instant for an OM_KILL that arrives later.
func respawnReturnTextFor(c corpse, from time.Time) string {
	if c.noReturn {
		return "the body does NOT come back (no spawner row and no -respawn flag)"
	}
	d := c.backAt.Sub(from)
	if d < 0 {
		d = 0
	}
	return "the body comes back in " + d.String() + " (" + respawnDelaySourceFor(c.body) + ")"
}

// corpseLeaveDelay answers how long after the KILL this body's corpse leaves - the
// reference's own rule, read out of source2 ZServerLib\MonsterNPC.cpp (BS_DEAD ON_ENTER):
//
//	if (IsOriginalDeadAni())            leaveDelay = 1000 + GetDyingTime(), floored at 3000
//	else if (state is AS_SKILL_HIT | AS_KNOCKDOWN | AS_DOWN | AS_ATTACK | AS_GROUND_ATTACK)
//	                                    leaveDelay = 2000
//	else                                leaveDelay = 5000
//
// and ON_UPDATE sets `m_leaveTime = now + leaveDelay; m_deadTime = m_leaveTime - 500`.
// So the ZC_DEAD is drawn 500 ms before the body leaves - 1.5 s after a killing blow -
// which is exactly the ordering the owner was missing when the death packet went out
// ahead of the hit (DEAD_PACKET_LEAD below).
//
// A body with a shape that states a `DyingTime` takes the first branch (its own death
// animation); one with no shape row and no statement falls to the attack branch, which
// is this lane's case because every kill here comes from an attack. The 5 s branch is
// the reference's for a death that is NOT an attack (a script's OM_KILL); it is used by
// armCorpseAt with byAttack false.
func corpseLeaveDelay(m Monster, byAttack bool) time.Duration {
	if m.ShapeKnown && m.DyingTime > 0 {
		d := time.Second + m.DyingTime
		if d < CorpseLeaveDeadAniFloor {
			d = CorpseLeaveDeadAniFloor
		}
		return d
	}
	if byAttack {
		return CorpseLeaveAttack
	}
	return CorpseLeaveIdle
}

// logCorpseShapeOnce says ONCE PER CLASS why a corpse uses the floor: its monster row
// named no shape that `datatable_monster_shape.xml` answers, so there is no DyingTime
// to wait for. It is a missing INPUT (and not a shape that states 0), so it is
// reported once per class the way logMaxHPUnavailable reports a missing max HP - a
// line per body would bury the first one in a field of kills.
func (h *Handler) logCorpseShapeOnce(m Monster) {
	if h == nil || h.Log == nil {
		return
	}
	h.spawnMu.Lock()
	if h.corpseShapeWarned == nil {
		h.corpseShapeWarned = make(map[uint32]bool, 4)
	}
	warned := h.corpseShapeWarned[m.ClassID]
	h.corpseShapeWarned[m.ClassID] = true
	h.spawnMu.Unlock()
	if warned {
		return
	}
	h.Log.Printf("zone: CORPSE class %d has NO shape row: there is no DyingTime to wait "+
		"for, so the body stays for the %s floor. The client plays its death animation "+
		"from the shape data and the body must still exist while it runs. ONE line per "+
		"class - a monster row whose Shape names a row of %s is the fix",
		m.ClassID, CorpseLeaveFloor, monsterShapeFile)
}

// logNoRespawnLocked says ONCE PER CLASS that a body will not come back: it has no
// spawner row behind it and the run passed no `-respawn` flag, so there is no interval
// to arm a return with. It names the missing INPUT and never a number - a default here
// is what the owner ruled out ("i dont want fallback, it can confuse or mislead us").
//
// ONE LINE PER CLASS, like logCorpseShapeOnce: a probe field of one species is many
// kills, and a line per kill would bury the first. CALLERS HOLD spawnMu (armCorpse's
// own contract), which is why this is the locked variant and takes no lock - taking it
// again would deadlock the death path.
func (h *Handler) logNoRespawnLocked(m Monster) {
	if h == nil || h.Log == nil {
		return
	}
	if h.noRespawnWarned == nil {
		h.noRespawnWarned = make(map[uint32]bool, 4)
	}
	if h.noRespawnWarned[m.ClassID] {
		return
	}
	h.noRespawnWarned[m.ClassID] = true
	h.Log.Printf("zone: respawn: class %d (%s) has no spawner row and no -respawn flag - "+
		"it will not come back", m.ClassID, h.MonsterName(m))
}

// corpse is one body this zone created that has died and not yet come back: the handle the
// client is holding, the instant its leave is owed, the instant its successor is owed, and
// the TEMPLATE the successor is made from.
//
// THE TEMPLATE IS THE BODY ITSELF, copied at the kill: class, point, direction, level,
// tactics, the defence and attack columns `SpawnMonsterObject` copied from the row, and
// `MaxHP` - which is the full HP the new body starts at. Copying it (rather than re-running
// the row lookup and the Lua maximum-HP script) is what makes a respawn the SAME body in
// every field the row answers, including a script's own `mhpFix` override: a new body must
// not be able to come back with a different maximum than the one it had.
type corpse struct {
	handle   uint32        // the dead body's handle; never handed out again
	name     string        // the row's name, kept for the log
	deadAt   time.Time     // the instant its ZC_DEAD left - the killing hit itself
	leaveAt  time.Time     // ZC_DEAD + delay
	delay    time.Duration // the interval itself: max(shape DyingTime, CorpseLeaveFloor)
	backAt   time.Time     // the kill + the return interval; ZERO when noReturn
	body     Monster       // the template
	left     bool          // the 3531 has gone out and the roster entry is gone
	deadSent bool          // the 3586 has gone out (DeathPacketLead before `leaveAt`)

	// WHO KILLED IT, kept until the ZC_DEAD instant: the drop roll needs the killer's
	// level (`MonsterNPC.cpp:764-766` passes `m_killer->GetLv()`) and the award needs
	// the killer's handle, and both now run on the death-packet pass rather than inside
	// the hit (zone/respawn.go deathPacketTick).
	killer   uint32
	killerLv int

	// noReturn is "this body has no return at all": no spawner row behind it and no
	// `-respawn` flag, so no interval exists to arm `backAt` with (respawnDelayFor
	// answered false). The corpse still owes its LEAVE - the dead body must be taken
	// off the client's world - and takeDueRespawns drops the entry once that leave has
	// gone out. It is a field of its own rather than `backAt.IsZero()` so a caller can
	// never read a missing return as "due at the zero instant".
	noReturn bool
}

// armCorpse records a dying body's leaving and its return. Called from takeHP (zone\
// create.go) in the SAME locked step that moves the HP to zero, on the same clock, so one
// kill produces one death, one leave and - when the body has a spawner row - one return,
// with nothing to keep in step afterwards. A body with NO spawner row and no `-respawn`
// flag arms the leave only: there is no interval, so no return is scheduled and the zone
// says so once per class (logNoRespawnLocked).
//
// `killed` is the instant of the death AND the instant the ZC_DEAD leaves (since
// 2026-11-07 the packet goes out with the killing hit, zone\create.go damageMonsterBy),
// which is why the leave is measured from it. The return is measured from the same
// instant because the reference counts the spawner's `RespawnTime` from the death.
//
// A BODY WITH NO ROSTER ENTRY GETS NO CORPSE: it is not something this zone created, and a
// respawn needs a template. Callers hold spawnMu (logNoRespawnLocked relies on it).
func (h *Handler) armCorpseAt(m Monster, killed time.Time, killer uint32, killerLv int, byAttack bool) bool {
	idx := h.monsterIndexLocked(m.Handle)
	if idx < 0 {
		return false
	}
	delay := corpseLeaveDelay(m, byAttack)
	back, returns := respawnDelayFor(m)
	c := corpse{
		handle: m.Handle,
		name:   h.MonsterName(m),
		// THE PACKET IS NOT AT THE KILL: it is DeathPacketLead before the leave, which is
		// the reference's `m_deadTime = m_leaveTime - 500`.
		deadAt:   killed.Add(delay - DeathPacketLead),
		leaveAt:  killed.Add(delay),
		delay:    delay,
		noReturn: !returns,
		body:     h.spawnedMonster[idx],
		killer:   killer,
		killerLv: killerLv,
	}
	if returns {
		c.backAt = killed.Add(back)
	}
	h.corpses = append(h.corpses, c)
	if !returns {
		h.logNoRespawnLocked(m)
	}
	return true
}

// monsterIndexLocked answers a monster's position in the roster, or -1. Callers hold
// spawnMu.
func (h *Handler) monsterIndexLocked(handle uint32) int {
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == handle {
			return i
		}
	}
	return -1
}

// corpseOwedLocked answers the corpse this zone already owes for a handle, and whether
// there is one. Callers hold spawnMu.
//
// IT EXISTS FOR THE SECOND KILL OF ONE BODY. A script's OM_KILL can land on a body the
// killing hit made dead a moment earlier (`KillMonsterObject`, zone\near.go); arming a
// second corpse would send a second 3531 for a handle the client has already dropped and
// schedule a second return for one body. A handle is never reused, so a match is this
// body's own and never another's.
func (h *Handler) corpseOwedLocked(handle uint32) (corpse, bool) {
	for i := range h.corpses {
		if h.corpses[i].handle == handle {
			return h.corpses[i], true
		}
	}
	return corpse{}, false
}

// corpsePending answers whether this zone owes ANY corpse work, which is what keeps a
// session's loop ticking after the ZC_DEAD has gone out (zone\chase.go tickIfDue,
// Handler.deadPending, zone\zone.go Serve). Two lists, one debt: a death packet, a leave
// and a return are all the same chain and none of them may depend on traffic that will
// never come.
func (h *Handler) corpsePending() bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	return len(h.corpses) > 0
}

// corpseTick sends every leave that is owed and brings back every body whose delay is up.
//
// TWO PHASES, IN THIS ORDER, ON THE CALLER'S CLOCK: the leave of a body that has died, then
// the return of a body that has already left. A tick that skipped the whole window (no
// player in the zone for a minute) runs both phases in one pass, so a corpse can never be
// left on a client and a body can never be lost. `now` is an argument so a test can stand
// at 599 ms and at 600 ms without sleeping - the same trick the ledger's own clock uses.
func (h *Handler) corpseTick(now time.Time) {
	// THE DEATHS FIRST: a ZC_DEAD goes out on the pass AFTER the hit that caused it
	// (2026-11-09, zone\create.go queueDeath), so this drain must run before the
	// leave/respawn work that follows a death - a body queued this pass is announced
	// here, and its corpse clock starts from the same `now`.
	h.deathPacketTick(now)
	for _, c := range h.takeDueLeaves(now) {
		pkt := BuildLeaveMonster(c.handle, LeaveCodeLogout)
		// The kill's leave is the only one this handle gets: mark it departed BEFORE the
		// send, so a ring pass that still lists the body forgets it in silence instead of
		// sending a second 3531 with reason 3. It is the ONE leave a script's OM_KILL
		// produces too - that call arms this corpse and sends nothing itself
		// (`KillMonsterObject`, zone\near.go).
		sightDeparted(c.handle)
		// THE OUTER RING IS ENOUGH HERE, and it is the right radius: a viewer past
		// ViewMiddleRange is FROZEN, not gone - it still holds the corpse in its world, so
		// it must be told the corpse is gone. (broadcastRespawnAt below needs both radii
		// for the opposite reason: an ADD is suppressed in the frozen band.)
		sent := h.broadcastWorldAt(pkt, c.body.X, c.body.Y, 0, 0)
		h.Log.Printf("zone: CORPSE LEAVE 0x%08x %q at (%d, %d, cell %d) - cmd %d (%d B) "+
			"reason %d to %d player(s), %dms after the ZC_DEAD (shape DyingTime %dms, "+
			"floor %dms); %s",
			c.handle, c.name, c.body.X, c.body.Y, c.body.Cell,
			binary.LittleEndian.Uint16(pkt), len(pkt), LeaveCodeLogout, sent,
			c.delay.Milliseconds(), c.body.DyingTime.Milliseconds(),
			CorpseLeaveFloor.Milliseconds(), respawnReturnTextFor(c, c.deadAt))
	}

	for _, c := range h.takeDueRespawns(now) {
		m, ok := h.bringBack(c)
		if !ok {
			h.Log.Printf("zone: RESPAWN of %q REFUSED: 0x%08x is still in the roster, so "+
				"there is nothing to bring back", c.name, c.handle)
			continue
		}
		pkt := BuildEnterStandingMonster(m)
		sent := h.broadcastRespawnAt(pkt, m.X, m.Y, m.Handle, now)
		h.Log.Printf("zone: RESPAWN %q (class %d, Lv %d) at (%d, %d, cell %d), hp %g -> "+
			"handle 0x%08x (the dead 0x%08x is gone), cmd %d (%d B) to %d player(s): a NEW "+
			"body at the same point, %s after the death",
			c.name, m.ClassID, m.Level, m.X, m.Y, m.Cell, m.MaxHP, m.Handle, c.handle,
			binary.LittleEndian.Uint16(pkt), len(pkt), sent, c.backAt.Sub(c.deadAt))
	}
}

// armCorpse is the script path's entry: a death that is NOT an attack (a script's
// OM_KILL), which the reference gives the longer 5 s leave to.
func (h *Handler) armCorpse(m Monster, killed time.Time) bool {
	return h.armCorpseAt(m, killed, 0, 0, false)
}

// deathPacketTick writes the ZC_DEAD for every corpse whose packet instant has come, and
// does what a death owes at that instant: the drop roll, the experience, and the
// missing-shape line (source2 MonsterNPC.cpp:2612-2621 - the drop happens in BS_DEAD's
// ON_UPDATE, the same pass that sets the two clocks).
//
// IT RUNS BEFORE THE LEAVE PASS, so a body whose packet and leave are both due in one
// pass still sends 3586 then 3531.
func (h *Handler) deathPacketTick(now time.Time) {
	h.spawnMu.Lock()
	var due []corpse
	for i := range h.corpses {
		c := &h.corpses[i]
		if c.deadSent || c.left || now.Before(c.deadAt) {
			continue
		}
		c.deadSent = true
		due = append(due, *c)
	}
	h.spawnMu.Unlock()
	for _, c := range due {
		pkt := BuildDead(c.handle)
		sent := h.broadcastWorld(pkt)
		h.Log.Printf("zone: DEAD %d %q - ZC_DEAD on the corpse's own clock (%s after the kill), "+
			"cmd %d (%d B) to %d player(s)", c.handle, c.name, c.delay.String(), CmdZCDead, len(pkt), sent)
		h.rollMonsterDrop(c.body, c.killer, c.killerLv)
		h.awardMonsterExp(c.body)
		if !c.body.ShapeKnown {
			h.logCorpseShapeOnce(c.body)
		}
	}
}

// takeDueLeaves answers every corpse whose leave is owed and takes its body off the field.
//
// ONE LOCKED PASS, for two edits that must not be split: the roster entry (and the live HP
// and the swing clock with it) and the `left` flag. A hit that ran between them would land
// on a body the roster no longer holds, and a second tick would leave the same corpse twice.
func (h *Handler) takeDueLeaves(now time.Time) []corpse {
	h.spawnMu.Lock()
	var due []corpse
	for i := range h.corpses {
		c := &h.corpses[i]
		if c.left || now.Before(c.leaveAt) {
			continue
		}
		c.left = true
		h.forgetBodyLocked(c.handle)
		due = append(due, *c)
	}
	h.spawnMu.Unlock()

	// THE SWING CLOCK IS A SECOND LOCK, taken after spawnMu is released: swingMu is the
	// monster's cadence lock and nothing here may hold the roster while it waits for a
	// swing's claim (or the other way round).
	if len(due) > 0 {
		h.swingMu.Lock()
		for _, c := range due {
			delete(h.monsterSwingAt, c.handle)
		}
		h.swingMu.Unlock()
	}
	return due
}

// forgetBodyLocked takes one body off the field: the roster entry (keeping the order of the
// rest, because that list is the world a later login is drawn - see KillMonsterObject) and
// its live HP. Callers hold spawnMu.
func (h *Handler) forgetBodyLocked(handle uint32) {
	if i := h.monsterIndexLocked(handle); i >= 0 {
		h.spawnedMonster = append(h.spawnedMonster[:i], h.spawnedMonster[i+1:]...)
	}
	delete(h.monsterHP, handle)
	// THE BODY'S BUFF LIST DIES WITH THE BODY (zone\buff.go): a handle is never reused,
	// so a list left behind could never be reached again - it would just hold memory.
	h.forgetMonsterBuffs(handle)
}

// takeDueRespawns removes and answers every corpse whose return is due.
//
// A CORPSE THAT HAS NOT LEFT YET IS NOT TAKEN, even if its delay has run out: the leave is
// the contract with the client, and a body that came back without its corpse leaving would
// be two objects for one name. corpseTick runs the leave phase first, so a skipped window
// still ends with both, in order.
//
// A CORPSE THAT NEVER RETURNS IS DROPPED HERE, once its leave has gone out: no spawner row
// and no `-respawn` flag means there is no interval and no successor, and keeping the entry
// would only keep corpsePending true and a session looping for a body that will never be
// made again.
func (h *Handler) takeDueRespawns(now time.Time) []corpse {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if len(h.corpses) == 0 {
		return nil
	}
	var due []corpse
	keep := h.corpses[:0]
	for _, c := range h.corpses {
		if !c.left {
			keep = append(keep, c)
			continue
		}
		if c.noReturn {
			continue
		}
		if !now.Before(c.backAt) {
			due = append(due, c)
			continue
		}
		keep = append(keep, c)
	}
	h.corpses = keep
	return due
}

// bringBack creates the NEW body: a fresh handle from the monster block, the template's own
// numbers and point, and its FULL HP in the ledger.
//
// A NEW HANDLE, NOT THE OLD ONE, and that is three things at once: the reference gives the
// species a new entity id (history\monster-death.md §5); a client keys every object it holds
// by handle, so re-using the dead number would make the corpse's leave and the new body's
// enter describe one actor; and the drop guard is per handle (zone\drop.go claimDropRoll,
// the reference's `m_isItemDropped`), so a new body rolls its own drop - which is what a new
// body is.
//
// FULL HP is the template's MaxHP, the number the spawn gave the body (the row's own, or a
// script's `mhpFix`). A body whose spawn could not name a maximum (MaxHP 0) comes back with
// no ledger entry, exactly as it was spawned - the attack lane then refuses to invent HP for
// it (zone\create.go damageMonster).
func (h *Handler) bringBack(c corpse) (Monster, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if h.monsterIndexLocked(c.handle) >= 0 {
		return Monster{}, false
	}
	m := c.body
	m.Handle = h.nextMonsterHandleLocked()
	// BACK AT ITS BIRTH POINT, NOT AT THE CORPSE (2026-11-09). The roster entry is live: a
	// walk writes X/Y/Cell back into it, so `c.body` carries the spot the old body died on.
	// The reference re-creates the body from its spawn row, which is the birth point.
	if m.SpawnX != 0 || m.SpawnY != 0 {
		m.X, m.Y, m.Cell = m.SpawnX, m.SpawnY, m.SpawnCell
	}
	h.spawnedMonster = append(h.spawnedMonster, m)
	if m.MaxHP > 0 {
		if h.monsterHP == nil {
			h.monsterHP = make(map[uint32]float64, 4)
		}
		h.monsterHP[m.Handle] = m.MaxHP
	}
	return m, true
}

// broadcastRespawnAt fans ONE 3527 out over the sight ring's TWO radii, and marks the new
// handle announced for every session that received it.
//
// WHY NOT broadcastWorldAt: that one knows the OUTER radius only. A viewer inside ViewRange
// but past ViewMiddleRange is FROZEN, not gone (`int cullDistance = m_viewMiddleRange;`,
// View.cpp:269 -> zone\sight.go sightCulls), so the frame that would ADD this body to its
// world is not sent either - the same rule, and the same two readers, the field-item frame
// already follows (zone\drop.go broadcastDropAt, which is the sibling of this function).
//
// A CULLED VIEWER IS NOT MARKED SEEN, and that is what keeps this from being a silent hole:
// nothing in that session's `seen` set names the body, so the ring's own pass announces it
// (a first-sight 3527) the moment that player walks inside the middle. The centre and the
// actor are both the ring's own readers (sightCentre, never the roster's destination - the
// R135d trap), and `now` is the tick's clock so both are read at one instant.
//
// A RING THAT IS NOT ARMED (no map row, -sightoff) SENDS TO EVERYONE, exactly as
// broadcastWorldAt and broadcastDropAt do.
func (h *Handler) broadcastRespawnAt(pkt []byte, x, y int32, handle uint32, now time.Time) int {
	view, middle, armed := h.sightRadii()
	sent := 0
	for _, m := range h.Players.Snapshot() {
		if armed && len(m.Pos) > 0 {
			if !inRing(m.Pos[0], x, y, view) {
				continue // past ViewRange: this object was never in its world
			}
			if m.Sess != nil && len(m.Sess.Team) > 0 &&
				sightCulls(h.sightCentre(m.Sess, now), x, y, middle, view) {
				continue // R135e: frozen, not gone - the body stays unseen until it walks in
			}
		}
		if m.Sess == nil {
			continue
		}
		if err := m.Sess.send(pkt); err != nil {
			h.Log.Printf("zone: could not tell a player about a respawned monster: %v", err)
			continue
		}
		sightMarkSeen(m.Sess, handle, sightKindMonster)
		sent++
	}
	return sent
}
