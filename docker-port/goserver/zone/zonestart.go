package zone

import (
	"time"

	"geserver/world"
)

// ZoneStartWait is how long a gate, warp NPC or waypoint waits for the zone manager to
// bring a missing map up before refusing. A zone announces itself ~4 s after it starts on
// this host, so six seconds leaves room for a cold start and no more.
//
// THE WAIT BLOCKS THE ZONE'S TICK. That is fine on this server - one player, one zone at a
// time - and it is the honest trade: the alternative is refusing the door and asking the
// player to click it again, which is exactly the "ask the operator to start the map" this
// removes. When more than one player is online, this belongs in a goroutine that answers
// the gate on a later tick.
const ZoneStartWait = 6 * time.Second

// ensureZone answers the zone that serves mapID, asking the manager (cmd/zoneman) to start
// one when none is live yet. A missing registry or a failed request is "not live", which
// leaves the caller's refusal in place - no door ever names an address that does not exist.
func (h *Handler) ensureZone(mapID int) (world.Record, bool) {
	if h.Zones == nil {
		return world.Record{}, false
	}
	if rec, ok := h.Zones.Address(mapID, world.AnyChannel); ok {
		return rec, true
	}
	if !h.AskForZones {
		// No manager wired into this handler (tests, or a server built without one):
		// the caller's refusal stands, exactly as before the manager existed.
		return world.Record{}, false
	}
	if err := h.Zones.Request(mapID); err != nil {
		h.Log.Printf("zone: map %d has no live zone and the start request failed (%v)",
			mapID, err)
		return world.Record{}, false
	}
	h.Log.Printf("zone: map %d has no live zone - asked the manager to start it (waiting "+
		"up to %s)", mapID, ZoneStartWait)
	rec, ok := h.Zones.WaitFor(mapID, ZoneStartWait)
	if !ok {
		h.Log.Printf("zone: map %d did not come up within %s", mapID, ZoneStartWait)
		return world.Record{}, false
	}
	h.Log.Printf("zone: the manager answered map %d at %s:%d - the door can open",
		mapID, rec.IP, rec.Port)
	return rec, true
}
