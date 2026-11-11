package zone

import (
	"fmt"
	"time"
)

// THE SCAN AND THE KILL - the zone's half of the Scarecrow Creator's second
// question, `GetNearObjectCount` / `GetNearObject` / `SendMsg(..., 'OM_KILL', ...)`.
//
// The script half is `script\near.go`; this file is the world it asks. The split is
// the same one `zone\create.go` uses for `CreateTNPC`/`CreateTmonster`: the script
// package must not import the zone, so main bridges the two, and both sides name
// their own types.
//
// WHAT COUNTS AS "NEAR", copied from the shipped server and not chosen here
// (`SystemFunc.cpp:776-826` + `geServer\ZServerLib\geCircleChecker.cpp`):
//
//	pos1 = the object the scan is called on - here the NPC itself
//	dSq  = (x1-x2)^2 + (y1-y2)^2        <- X AND Y ONLY, no height (Common.cpp:47-53)
//	near = dSq < range*range            <- STRICTLY less, not "<="
//
// and the type word picks what is visited: `"Monster"` -> the monster objects
// (`SystemFunc.cpp:791-795`).
//
// THE UNIT, because this is the trap: those are not metres and not cells. A stored
// X/Y is the client's WORLDPOS coordinate and the map is in CENTIMETRES - the
// server's own move speed is 469 cm/s (`zone\speed.go`, `MoveBaseSpeed`, read off
// `get_status_pc.scp:1808`), so a character crosses 469 of these units in one
// second. The script's 800 is therefore 8 metres: about 1.7 s of bare running, and
// roughly three character lengths wide on the screen.

// NearObject is one live actor a scan found. It mirrors `script.NearObject` on
// purpose, the same way MonsterSpec mirrors script.MonsterSpawn.
type NearObject struct {
	Handle  uint32
	ClassID uint32
	X       int32
	Y       int32
}

// The one type word this zone can serve, spelled exactly as the game's own script
// spells it: `GetNearObjectCount(self, 800, "Monster", 0)`.
const NearKindMonster = "Monster"

// NearObjects returns every monster this zone is running that stands within
// `radius` of (x, y), in the order the monsters were created.
//
// AN EMPTY ANSWER IS NOT AN ERROR: it is `nil, nil`, the ordinary "there is nothing
// there" that ends in the script also counting 0. An ERROR is for a scan this zone
// cannot answer at all - today only a type word it does not serve - and the script
// host turns that into a 0 plus one line in the log.
//
// WHAT IS *NOT* IN THE LIST, and why: the fixed S1 debug monster
// (`DevStandingMonster`, `-spawnmonster`) is not in the registry, cannot be removed,
// and is drawn again for every session that enters - so offering it to a script that
// follows up with `OM_KILL` would be an object the script could see and never
// delete. It also stands 2,400+ units from the nearest Scarecrow Creator on this
// map, so no scan reaches it either way.
func (h *Handler) NearObjects(x, y int32, radius float64, kind string) ([]NearObject, error) {
	if kind != NearKindMonster {
		return nil, fmt.Errorf("this zone only serves the %q type word, and the scan "+
			"asked for %q: nothing else is modelled well enough to be counted "+
			"(%q is the Scarecrow script's own word)", NearKindMonster, kind, NearKindMonster)
	}
	limit := radius * radius

	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	var out []NearObject
	for _, m := range h.spawnedMonster {
		if distSq(m.X, m.Y, x, y) >= limit {
			continue
		}
		out = append(out, NearObject{Handle: m.Handle, ClassID: m.ClassID, X: m.X, Y: m.Y})
	}
	return out, nil
}

// KillMonsterObject turns one monster into a ZOMBIE. It is the zone's half of
// `SendMsg(self, self, 'OM_KILL', 0, 0)` and it is the reference's
// `CMonsterNPC::OnKill` (`MonsterNPC.cpp:149-171`), NOT a removal.
//
// WHAT THE REFERENCE DOES: it sends NO packet and takes nothing off the field. It
// tells the generator the body died (`NotifyDeadToGenerator`), ends the attack it
// had on its target, drops the boss/minion relation, `SetMonsterGen(NULL)`,
// `SetZombie()`, `DestroyVisitor()` and `ReleaseTacticsStateThread()` (:161-171).
// A zombie is a body whose script threads are never resumed again - the thread
// callback's `THREAD_PRE_RESUME` answers false on `IsZombie()` (`FieldObject.cpp:
// 29-34`) - and it STAYS where it stands, inert and still drawn. The leave reaches
// a viewer only when THAT viewer's own view update finds the object a zombie
// (`View.cpp:193-196`, reason `OBJ_LEAVE_LOGOUT`), and the map's generator is what
// brings the species back later (`:159`).
//
// SO THIS FUNCTION: the body stays in the roster; the HP ledger goes to 0, which is
// what "dead" means here and is the number the swing, think and chase ticks already
// refuse (`hp <= 0`: `zone\monster_swing.go:244`, `zone\monster_ai.go:110`,
// `zone\monster_chase.go:257`); its hate list, tactics target, attack state and
// order are cleared, and its swing claim and Go-chase engagement are dropped.
// NOTHING is sent - so a body kited past every player's ring does not blink out of
// a screen that was never told it existed (the owner's report: "it vanish? the live
// server does not do that").
//
// THE TACTICS THREAD IS NOT RESUMED AGAIN, and it is not destroyed HERE on purpose.
// This function runs INSIDE that very coroutine's resume - the script called
// `SendMsg` - and cancelling the Lua context from within it would kill the running
// state and report a FAILED script. The think turn skips the body at 0 HP before it
// reaches the runner, and the runner releases the thread (`ForgetMonster`) when the
// corpse's leave takes the body off the roster.
//
// THE LEAVE IS THE CORPSE'S, through the SAME handshake the death path uses
// (`armCorpse`, `zone\respawn.go`): max(the shape's `DyingTime`, `CorpseLeaveFloor`
// = 600 ms) later the corpse's own 3531 goes out with reason 0 - never the ring's
// reason 3, which makes the client FADE the body - and the body comes back after its
// spawner row's `RespawnTime` (or the `-respawn` flag) as a NEW handle at the point it
// was killed. A body with NO spawner row and no flag is not brought back at all, and
// the zone says so once per class (zone/respawn.go respawnDelayFor).
//
// A BODY THAT ALREADY HAS A CORPSE OWED IS NOT ARMED TWICE. `takeHP` may have made
// it dead a moment earlier; arming again would send a second 3531 for a handle the
// client has already dropped (the double-leave the respawn.go header warns about)
// and schedule two returns for one body.
//
// A handle that is not a monster THIS ZONE is running is REFUSED: a forged or stale
// handle must not be able to silence another object, and the caller gets the reason
// rather than an idle success.
func (h *Handler) KillMonsterObject(handle uint32) error {
	// THE HOUSE LOCK ORDER: chaseMu before spawnMu (`EnableMove` and
	// `endApproachesTo` take them that way), and swingMu only after both are
	// released (`takeDueLeaves` does the same) - the roster is never held while a
	// swing is claimed.
	h.chaseMu.Lock()
	h.spawnMu.Lock()

	idx := h.monsterIndexLocked(handle)
	if idx < 0 {
		h.spawnMu.Unlock()
		h.chaseMu.Unlock()
		return fmt.Errorf("handle 0x%08x is not a monster this zone is running, so "+
			"there is nothing to kill", handle)
	}
	m := h.spawnedMonster[idx]

	// 1. THE MARK. HP 0 is "dead" in this zone - the same number the killing hit
	// writes - and every clock that could make the body act reads it. A body with no
	// ledger entry at all was already inert (the same three ticks refuse it for the
	// missing entry), so nothing is invented for it.
	if hp, known := h.monsterHP[handle]; known && hp > 0 {
		h.monsterHP[handle] = 0
	}

	// 2. THE HATE AND THE ORDER. The reference ends the attack on its target
	// (`OM_NOTIFY_END_ATTACK`) and destroys the visitor that holds the target. In our
	// terms that is the hate list, the tactics target, the attack state, the order the
	// script last gave and the Go chase's engagement. Read directly rather than through
	// `hateLocked`/`engineLocked`: those CREATE state, and a body that never had one has
	// nothing to clear (and `engineLocked` would ask the mesh for a home point from
	// inside a Lua resume).
	if st := h.monsterHate[handle]; st != nil {
		st.entries = nil
		st.tactics = 0
		st.attacking = false
	}
	if st := h.monsterEngines[handle]; st != nil {
		st.order.move = monsterMoveOrder{}
		st.order.attack = monsterAttackOrder{}
		st.order.approach = monsterApproach{}
	}
	delete(h.monsterChases, handle)

	// 3. THE CORPSE - the one existing chain: the leave after the shape's DyingTime
	// (floor 600 ms) and the return respawnDelayFor(m) answers, WHEN it answers one
	// (the body's own spawner RespawnTime, or the `-respawn` flag). A body with
	// neither gets the leave and no return. It cannot answer false
	// here: the roster entry it looks for is the one read above, under this same lock.
	owed, hadCorpse := h.corpseOwedLocked(handle)
	if !hadCorpse {
		// The corpse is armed here and read back in the SAME lock, so the log line
		// below describes the corpse that was just made rather than a second lookup
		// that could race another kill. armCorpse cannot answer false for a handle
		// that has a roster entry - and the entry was read above, under this lock -
		// but a zero corpse must never be printed as a return, so it is checked.
		if !h.armCorpse(m, time.Now()) {
			h.spawnMu.Unlock()
			h.chaseMu.Unlock()
			return fmt.Errorf("handle 0x%08x has no roster entry to arm a corpse for", handle)
		}
		owed, _ = h.corpseOwedLocked(handle)
	}
	h.spawnMu.Unlock()
	h.chaseMu.Unlock()

	// The swing's own claim dies with the body (takeDueLeaves clears it at the leave
	// too; here it is cleared while the body is still a roster entry).
	h.swingMu.Lock()
	delete(h.monsterSwingAt, handle)
	h.swingMu.Unlock()

	if hadCorpse {
		// One line per OM_KILL, and it says what will actually happen - including that
		// this call armed nothing, because the killing hit already did.
		h.Log.Printf("zone: OM_KILL 0x%08x (class %d) was already dead at (%d, %d, cell %d) - "+
			"no second corpse and no leave now; the corpse armed by its death leaves in %s "+
			"(reason %d, cmd %d), and %s",
			m.Handle, m.ClassID, m.X, m.Y, m.Cell, timeUntil(owed.leaveAt), LeaveCodeLogout,
			CmdZCLeaveMonster, respawnReturnTextFor(owed, time.Now()))
		return nil
	}
	h.Log.Printf("zone: OM_KILL 0x%08x (class %d) is now a zombie at (%d, %d, cell %d) - "+
		"no leave now; the body stays in the roster, the corpse leaves in %s (reason %d, "+
		"cmd %d) and %s",
		m.Handle, m.ClassID, m.X, m.Y, m.Cell,
		corpseLeaveDelay(m, false), LeaveCodeLogout, CmdZCLeaveMonster,
		respawnReturnTextFor(owed, owed.deadAt))
	return nil
}

// timeUntil is how long from now until `at`, never negative: a corpse whose leave
// was owed a moment ago reads "0s" in a log line, not a negative duration.
func timeUntil(at time.Time) time.Duration {
	if d := time.Until(at); d > 0 {
		return d
	}
	return 0
}

// RadiusUnitsPerMetre is how many stored map units make one metre, and it is the
// answer to "how far is 800?".
//
// IT IS NOT A NEW MEASUREMENT: the server's own move speed is 469 cm/s and it is the
// number our ZC_MOVE_PC carries (`zone\speed.go`, measured against the live stream),
// so one unit is one centimetre. It is a named constant so the next reader does not
// have to re-derive it, and so the 800 in the game's script can be read as 8 metres.
const RadiusUnitsPerMetre = 100

// distSq is the same distance test the original makes, in one place
// (`Common.cpp:47-53`: X and Y only, no height), so a caller that wants to explain a
// radius cannot quietly use a different rule.
//
// The strict "<" lives in the caller on purpose: the shipped checker is
// `distanceSq < m_fSqRange` (`geCircleChecker.cpp:19-23`), so a monster standing
// EXACTLY `range` away is NOT found.
func distSq(x1, y1, x2, y2 int32) float64 {
	dx := float64(x1 - x2)
	dy := float64(y1 - y2)
	return dx*dx + dy*dy
}
