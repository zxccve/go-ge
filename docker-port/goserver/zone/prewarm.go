// PRE-WARM THE MAPS THIS ONE CONNECTS TO (card t_2950bb0a, the owner's own refinement:
// "the pre warm on gate should just warm the nearby map/connected map. even if the player
// did not cross the gate we can just load the map to be ready").
//
// WHY IT EXISTS. A gate starts the destination only when the player actually walks out
// (zone\gate.go asks the manager at the door), so the player then waits the whole cold
// start - 2.6 s for a field on this build, seconds more on a monster-dense map. The maps a
// player can reach from the one they are standing in are KNOWN (the map's own gate table),
// so the ask can happen when they ENTER, not when they leave: by the time anybody walks
// through a door, the zone behind it is already up.
//
// WHAT IT IS NOT. It starts nothing itself: `Handler.RequestZone` is the zone manager's own
// request path (`world.Registry.Request` -> `out\zones\requests\request_<map>.json`, which
// `cmd/zoneman` answers), the same one the barrack's START uses. The manager's idle stop
// (`-idle 10m`, 0 players) is the ONE reaper for a map nobody entered - this file adds no
// second unload path and no timer of its own.
//
// THE COOLDOWN IS THE ONLY STATE: one request per map per prewarmCooldown, so a zone with
// many players entering does not write a request file per session, and a map that a player
// bounces in and out of is asked about once a minute rather than once a click. A refusal is
// NOT remembered, so the next entry can try again (the manager may be restarting).
package zone

import (
	"sync"
	"time"
)

// prewarmCooldown is how long one map's request is trusted. One minute is far longer than a
// start takes and far shorter than the manager's ten-minute idle stop, so a pre-warmed map
// that nobody enters is reaped by the rule that already exists.
const prewarmCooldown = 60 * time.Second

var (
	prewarmMu   sync.Mutex
	prewarmLast = map[int]time.Time{}
)

// PrewarmGates asks the zone manager for every map this map has a usable gate to. It is a
// no-op unless the switch is on and a requester is wired.
func (h *Handler) PrewarmGates() {
	if h == nil || !h.Prewarm || h.RequestZone == nil {
		return
	}
	now := time.Now()
	for _, g := range h.Gates {
		if g.ToID == 0 || g.ToID == h.MapID {
			// An unknown destination is a data gap the gate itself already refuses;
			// this map is not a destination at all.
			continue
		}
		if !prewarmClaim(g.ToID, now) {
			continue
		}
		if err := h.RequestZone(g.ToID); err != nil {
			h.Log.Printf("zone: PREWARM map %d (%s): the manager refused the request "+
				"(%v) - the gate still works, it just waits for the start at the door",
				g.ToID, g.Name, err)
			prewarmForget(g.ToID)
			continue
		}
		h.Log.Printf("zone: PREWARM map %d (%s) - asked the manager when a player entered "+
			"map %d, so the door does not wait for a start", g.ToID, g.Name, h.MapID)
	}
}

// prewarmClaim reserves one map's request, and answers false while its cooldown is running.
func prewarmClaim(mapID int, now time.Time) bool {
	prewarmMu.Lock()
	defer prewarmMu.Unlock()
	if at, was := prewarmLast[mapID]; was && now.Sub(at) < prewarmCooldown {
		return false
	}
	prewarmLast[mapID] = now
	return true
}

// prewarmForget drops a map's mark after a refusal, so the next entry asks again.
func prewarmForget(mapID int) {
	prewarmMu.Lock()
	delete(prewarmLast, mapID)
	prewarmMu.Unlock()
}

// PrewarmState answers when a map was last asked about - a test and a diagnosis line see
// the same number.
func PrewarmState(mapID int) (time.Time, bool) {
	prewarmMu.Lock()
	defer prewarmMu.Unlock()
	at, ok := prewarmLast[mapID]
	return at, ok
}

// forgetAllPrewarm clears the cooldown table: ONLY a test uses it, so one test's maps do
// not silence another's.
func forgetAllPrewarm() {
	prewarmMu.Lock()
	prewarmLast = map[int]time.Time{}
	prewarmMu.Unlock()
}
