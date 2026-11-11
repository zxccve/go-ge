// THE HATE TABLE THE GAME'S OWN BATTLE SCRIPT READS.
//
// WHY THIS FILE EXISTS, in the owner's words (2026-11-07): "whatever the scp is doing
// it should do by itself. nothing gets coded in native Go." `ge\script\monster\
// mon_ts_battle.scp` IS the monster battle AI - one function, 277 lines - and it asks
// the engine seven questions about one body:
//
//	target = GetTopHatePointChar(self)      who should I hate most   (:5, :100, :240)
//	lastTarget = GetTacticsTarget(self)     who was I already on     (:101, :241)
//	SetTacticsTarget(self, target)          remember this one        (:164, :173)
//	SetTacticsTarget(self)                  forget it                 (:157, :189)
//	if 0 == IsSameTarget(target, lastTarget)   did the target change  (:167)
//	InsertHate(self, target, 20)            +20 points for this one   (:177)
//	RemoveHate(self, lastTarget)            drop this one entirely    (:149, :188)
//	if 'NO' == IsAttacking(self)            am I already swinging     (:67, :162)
//
// This file stores the answer to all seven and decides NONE of them. There is no
// decay, no scoring heuristic and no "if the player hits harder" rule here, because
// the reference has none either - and a rule written here would be a second AI
// arguing with the script's.
//
// THE RULES, QUOTED FROM THE REFERENCE (each one read before it was used):
//
//   - ADD, NOT SET. `CHateList::Update` adds: `info->m_point += point;`
//     (source2\geServer\ZServerLib\HateList.cpp:42). Nothing decays a point total
//     anywhere in the class - `GarbageCollect` (:96-124) only drops targets that are
//     NULL, a zombie or BS_DEAD, `Remove` (:129-146) deletes one target outright, and
//     `RemoveAll` (:148-172) clears the list. `RemoveHate` is the DELETE
//     (`CHateList::Remove`), not a subtraction, which is why the script calls it on a
//     target it has given up on.
//   - NO `force`. The reference's binding is `InsertHate(to, obj, point)` - three
//     arguments (MonsterNPCFunc.cpp:81-90) - and `CMonsterNPC::OnInsertHate` hands
//     those two numbers straight to Update (MonsterNPC.cpp:136-140). The `force` word
//     the card's census mentions belongs to `SCR_MON_AI_LIB_HATE(self, range, force,
//     hateamnt, target)` -> `BroadMsg(self, range, force, 'OM_HATE', ...)`
//     (mon_ai_lib.scp:48-59): it is the BROADCAST SCOPE ('Friend'), not a priority.
//   - THE TOP IS THE MAX POINT, and the reference's tie-break is the HATE_INFO
//     POINTER (`CLessPoint`: `a < b` when the points are equal, HateList.h:14-22,
//     read out with `LastInorder()` at :57). That is an allocation order, not a game
//     rule, so the deterministic stand-in here is the list's own order: a tie keeps
//     the target that was hated FIRST. Same answer every run, and it is this card's
//     pass condition.
//   - A DEAD OR GONE TARGET IS NO TARGET. The reference collects them out of the map
//     and the sorted index in `GetTopPointChar` (`obj->IsZombie() || BS_DEAD ==
//     obj->GetBodyState()`, HateList.cpp:69-71), and then still answers NULL if the
//     survivor is invincible. This zone has no zombies and no invincibility, so the
//     honest half is the one that matters: an entry whose target has left the zone or
//     reached 0 HP is dropped before the max is taken (`topHateLocked`), which is the
//     same thing GarbageCollect does.
//   - HATE IS CREDITED TO THE DONOR, not to the body that swung: `CFieldChar*
//     hatePointDonor = obj->GetHatePointDonor();` (HateList.cpp:31). That matters for
//     pets and summons - a pet's hate belongs to its master. THIS SERVER HAS NO PET
//     OWNERSHIP (`GetOwner` answers nil, script\status.go:223-228), so the handle the
//     script hands us is the handle that keeps the points. When ownership arrives, the
//     translation belongs HERE and nowhere else.
//
// THE TABLE DIES WITH THE BODY, without editing the despawn path: the state is keyed
// by handle and PRUNED against the roster on every access (`pruneHateLocked`), and the
// corpse's leave deletes the roster row and the HP entry in one locked step
// (respawn.go:352-354). A handle is never reused - `monsterSeq` only grows - so a
// stale table can never be inherited by the body that respawns at the same point.
//
// THE LOCK IS `spawnMu`, the short lock the damage path and the swing already take
// (create.go `takeHP`, monster_swing.go `monsterSwingArmed`): the roster, the HP
// ledger and the hate table are one world, and splitting them across two locks is how
// a body comes back with the hate of the one that died. No second mutex, and no second
// list of bodies - the tables hang off the same handle key as `monsterHP`.
package zone

import (
	"fmt"
	"time"

	"geserver/script"
)

// The zone IS the world behind the seven builtins (script\hate.go). This line fails the
// build, not a script run, if a signature ever drifts.
var _ script.HateWorld = (*Handler)(nil)

// hateEntry is ONE character's standing with one body - the reference's HATE_INFO
// (`CFieldCharPtr m_char; int m_point;`, HateList.h:8-12). The total is an `int`
// there and an `int` here; the script's own numbers (`InsertHate(self, target, 20)`,
// the level term in SCR_MSG_MON_HATE) are small integers.
type hateEntry struct {
	target uint32
	points int
}

// hateState is everything the battle script keeps about ONE body: the hate list, the
// tactics target it last announced (`m_tacticsTarget`, MonsterNPC.cpp:2187-2195) and
// the attack state `IsAttacking` reads. One per monster handle, created on first use
// and dropped when the body leaves.
type hateState struct {
	// entries are in INSERTION ORDER, which is the tie rule (see the file header).
	entries []hateEntry

	// tactics is the SetTacticsTarget the script last asked for; 0 is the
	// reference's NULL.
	tactics uint32

	// attacking is the action state: true while the body is in one of the states
	// `IsAttacking` counts as attacking (AS_ATTACK_CHASE, AS_ATTACK, AS_GROUND_ATTACK,
	// AS_SKILL_CHASE, AS_SKILL_PREPROCESS, AS_SKILL_CASTING, AS_SKILL_USE,
	// FieldCharFunc.cpp:316-320). STAGE 2 sets it from StartAttack/StopAttack; nothing
	// in this card writes it except `markMonsterAttacking`, which is what those two
	// builtins will call. A body nobody has started an attack for answers 'NO', which
	// is the truth and not a placeholder.
	attacking bool
}

// bodyOnFieldLocked answers whether this handle is a body this zone created and has
// not yet taken off the field. Caller holds spawnMu.
func (h *Handler) bodyOnFieldLocked(handle uint32) bool {
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == handle {
			return true
		}
	}
	return false
}

// pruneHateLocked forgets the tables of bodies that are no longer on this field - the
// "dies with the body" half of this file. Caller holds spawnMu.
//
// It is O(bodies with hate x roster) and both are tiny (a zone holds a handful of
// script-made bodies), and it runs once per script call - which is at most once a
// second per body. Cheap enough to be the correctness rule rather than a tick.
func (h *Handler) pruneHateLocked() {
	for handle := range h.monsterHate {
		if !h.bodyOnFieldLocked(handle) {
			delete(h.monsterHate, handle)
		}
	}
}

// hateLocked answers one body's table, creating it when `create` is true. It answers
// nil for a handle that is not a body on this field, so no call can invent a table
// for a body that does not exist. Caller holds spawnMu.
func (h *Handler) hateLocked(handle uint32, create bool) *hateState {
	if !h.bodyOnFieldLocked(handle) {
		return nil
	}
	st := h.monsterHate[handle]
	if st == nil && create {
		if h.monsterHate == nil {
			h.monsterHate = make(map[uint32]*hateState, 4)
		}
		st = &hateState{}
		h.monsterHate[handle] = st
	}
	return st
}

// hateTargetLocked answers the OBJECT the script sees for one handle, and whether it
// is a live body at all.
//
// A PLAYER answers ClassName 'PC' - the own name the corpus tests with `self.ClassName
// == 'PC'` - its own level and its live position. A MONSTER answers its row's ClassID
// and level and carries no ClassName: this server has no ClassName column for a body,
// and the property fallback in the binding turns that into 0, which is not
// 'Structure_PartyColony' - the one comparison the battle script makes (:148) - so a
// structure target degrades to "not a structure", which is what the branch would do
// with any body it does not know. Stage 4 (minions, boxes) is where that name is
// earned.
//
// A target at 0 HP is NOT live: the reference collects `BS_DEAD` bodies out of the
// list (HateList.cpp:69-71), and a corpse must not stay the top hate target.
func (h *Handler) hateTargetLocked(handle uint32) (script.HateTarget, bool) {
	now := time.Now()
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			c := &mem.Sess.Team[j]
			if c.Handle != handle {
				continue
			}
			if c.HP <= 0 {
				return script.HateTarget{}, false
			}
			pos := h.sessionBodyAt(mem.Sess, j, now)
			return script.HateTarget{
				Handle:    c.Handle,
				ClassName: "PC",
				Lv:        int(c.Level),
				X:         pos[0],
				Y:         pos[1],
				Cell:      pos[2],
			}, true
		}
	}
	for i := range h.spawnedMonster {
		m := &h.spawnedMonster[i]
		if m.Handle != handle {
			continue
		}
		if hp, ok := h.monsterHP[handle]; ok && hp <= 0 {
			return script.HateTarget{}, false
		}
		return script.HateTarget{
			Handle:  m.Handle,
			ClassID: m.ClassID,
			Lv:      m.Level,
			X:       m.X,
			Y:       m.Y,
			Cell:    m.Cell,
		}, true
	}
	return script.HateTarget{}, false
}

// hateTop is the answer `topHateLocked` gives, and the only thing the `zone: HATE`
// line compares: which handle, how many points, and whether there was one at all.
type hateTop struct {
	handle uint32
	points int
	ok     bool
}

// topHateLocked answers the highest-standing LIVE target of one body.
//
// DEAD TARGETS ARE DROPPED FIRST, in place, exactly as `CHateList::GarbageCollect`
// does before it reads the max (HateList.cpp:54-56 -> :96-124). Then the max is taken
// in LIST ORDER with a strict `>`, so a tie keeps the first target hated - see the
// file header for why that is the tie rule and not the reference's pointer order.
// Caller holds spawnMu.
func (h *Handler) topHateLocked(st *hateState) hateTop {
	if st == nil || len(st.entries) == 0 {
		return hateTop{}
	}
	kept := st.entries[:0]
	for _, e := range st.entries {
		if _, live := h.hateTargetLocked(e.target); live {
			kept = append(kept, e)
		}
	}
	st.entries = kept

	var best hateTop
	for _, e := range st.entries {
		if !best.ok || e.points > best.points {
			best = hateTop{handle: e.target, points: e.points, ok: true}
		}
	}
	return best
}

// InsertHate is `InsertHate(mon, target, points)`: ADD the points to `target`'s
// standing with the body `mon` (HateList.cpp:42), creating the entry at 0 first when
// this is the first time the script names this target.
//
// `points` may be negative and is NOT clamped: the reference adds whatever it is
// given and the script is the only thing that decides what a hit is worth.
func (h *Handler) InsertHate(mon, target uint32, points int) {
	h.spawnMu.Lock()
	h.pruneHateLocked()
	st := h.hateLocked(mon, true)
	if st == nil {
		h.spawnMu.Unlock()
		h.logHateRefused("InsertHate", mon)
		return
	}
	before := h.topHateLocked(st)
	added := false
	for i := range st.entries {
		if st.entries[i].target == target {
			st.entries[i].points += points
			added = true
			break
		}
	}
	if !added {
		st.entries = append(st.entries, hateEntry{target: target, points: points})
	}
	after := h.topHateLocked(st)
	h.spawnMu.Unlock()

	h.logHateTop(mon, before, after)
}

// RemoveHate is `RemoveHate(mon, target)`: delete that target's entry outright
// (`CHateList::Remove`, HateList.cpp:129-146) - the points are not merely lowered,
// which is why the script's 3000-unit branch can never see the same body again.
func (h *Handler) RemoveHate(mon, target uint32) {
	h.spawnMu.Lock()
	h.pruneHateLocked()
	st := h.hateLocked(mon, false)
	if st == nil {
		h.spawnMu.Unlock()
		return
	}
	before := h.topHateLocked(st)
	kept := st.entries[:0]
	for _, e := range st.entries {
		if e.target != target {
			kept = append(kept, e)
		}
	}
	st.entries = kept
	after := h.topHateLocked(st)
	h.spawnMu.Unlock()

	h.logHateTop(mon, before, after)
}

// TopHate answers the object behind `GetTopHatePointChar(mon)`, with the points it
// was chosen by. false is the reference's NULL.
func (h *Handler) TopHate(mon uint32) (script.HateTarget, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneHateLocked()
	top := h.topHateLocked(h.hateLocked(mon, false))
	if !top.ok {
		return script.HateTarget{}, false
	}
	obj, live := h.hateTargetLocked(top.handle)
	if !live {
		return script.HateTarget{}, false
	}
	obj.Points = top.points
	return obj, true
}

// SetTacticsTarget is `SetTacticsTarget(mon [, target])`: store the target, or clear
// it when `target` is 0.
//
// The reference's two guards are copied: the SAME target is a no-op and a ZOMBIE is
// refused (MonsterNPCFunc.cpp:264-270). Its OM_NOTIFY_BEGIN_ATTACK / END_ATTACK
// messages are not sent - those belong to the attack engine, which this card does not
// touch.
func (h *Handler) SetTacticsTarget(mon, target uint32) {
	h.spawnMu.Lock()
	h.pruneHateLocked()
	st := h.hateLocked(mon, true)
	if st == nil {
		h.spawnMu.Unlock()
		h.logHateRefused("SetTacticsTarget", mon)
		return
	}
	if target == 0 {
		st.tactics = 0
		h.spawnMu.Unlock()
		return
	}
	if st.tactics == target {
		h.spawnMu.Unlock()
		return
	}
	if _, live := h.hateTargetLocked(target); !live {
		h.spawnMu.Unlock()
		return
	}
	st.tactics = target
	h.spawnMu.Unlock()
}

// TacticsTarget answers `GetTacticsTarget(mon)` - the body's stored target, or false
// when it has none (or when the actor behind it has left the zone).
func (h *Handler) TacticsTarget(mon uint32) (script.HateTarget, bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	h.pruneHateLocked()
	st := h.hateLocked(mon, false)
	if st == nil || st.tactics == 0 {
		return script.HateTarget{}, false
	}
	return h.hateTargetLocked(st.tactics)
}

// IsAttacking answers `IsAttacking(mon)`: is this body in an attack action?
//
// It is the body's OWN state, never "does it have a target": the reference reads
// `GetActionState()` (FieldCharFunc.cpp:312) and the script's use of it is to decide
// whether `StartAttack` still has to be called (:162-164).
func (h *Handler) IsAttacking(mon uint32) bool {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if st := h.hateLocked(mon, false); st != nil {
		return st.attacking
	}
	return false
}

// markMonsterAttacking is how the attack engine will report its own action state -
// stage 2 calls it from StartAttack/StopAttack, which are not bound yet.
//
// It exists in this card so the state `IsAttacking` reads has a WRITER (a test drives
// it), instead of the answer being hard-wired to 'NO' and having to be redesigned when
// the attack builtins arrive.
func (h *Handler) markMonsterAttacking(handle uint32, on bool) {
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	if st := h.hateLocked(handle, true); st != nil {
		st.attacking = on
	}
}

// logHateTop writes the ONE line this card promises: when a body's TOP-HATE target
// changes, name both ends and the points that won.
//
// Only a CHANGE is written. The script reads the top every second per body, so a line
// per read would bury the log; a line per change is the event a reader wants (who
// stole the monster, and from whom).
func (h *Handler) logHateTop(mon uint32, before, after hateTop) {
	if before.ok == after.ok && (!after.ok || before.handle == after.handle) {
		return
	}
	who := "none (0 points)"
	if after.ok {
		who = fmt.Sprintf("0x%08x %q (%d points)", after.handle, h.hateName(after.handle), after.points)
	}
	h.Log.Printf("zone: HATE 0x%08x %q top -> %s", mon, h.hateName(mon), who)
}

// logHateRefused says ONCE per zone that a script asked about a handle this zone does
// not have on the field. That is a wiring fault, not game data: stage 2 hands the
// script its own bodies, so the first run either never writes this line or shows the
// one handle that is wrong.
func (h *Handler) logHateRefused(call string, mon uint32) {
	h.spawnMu.Lock()
	said := h.hateRefused
	h.hateRefused = true
	h.spawnMu.Unlock()
	if said {
		return
	}
	h.Log.Printf("zone: HATE %s(0x%08x) refused - that handle is not a body on this "+
		"field, so no table was touched (further refusals are not logged)", call, mon)
}

// hateName answers a handle's display name for the HATE line: the player character's
// name, else the body's row name, else "?". It takes no lock of its own beyond the
// two it needs in sequence (the player roster, then spawnMu), so it must be called
// OUTSIDE spawnMu - which is why every caller writes its line after unlocking.
func (h *Handler) hateName(handle uint32) string {
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			if mem.Sess.Team[j].Handle == handle {
				return mem.Sess.Team[j].Name
			}
		}
	}
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == handle {
			return h.MonsterName(h.spawnedMonster[i])
		}
	}
	return "?"
}
