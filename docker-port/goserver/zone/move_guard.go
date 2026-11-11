package zone

import "math"

// MOVE GUARD: A LONG JUMP IS ANNOUNCED IN THE LOG AND THE REPORT IS THEN USED EXACTLY
// AS THE CLIENT SENT IT. NOTHING IS CLAMPED, DROPPED OR REWRITTEN.
//
// WHY THE CLAMP IS GONE (removed 2026-11-01). Until this file knew two constants,
// MoveGuardSpeedFactor 1.5 and MoveGuardSlack 50, and it shortened any CZ_MOVE_PC whose
// destination was further from the body than a walk at packetSpeed x1.5 + 50 units could
// cover in the time since that character's previous report. That rule rested on a FALSE
// PREMISE: the value in a CZ_MOVE_PC is a DESTINATION, not a position. The client re-paths
// as it walks and sends a fresh destination for each path step, so two consecutive
// destinations can be any distance apart while the body itself keeps walking smoothly.
// Distance between destinations divided by time is therefore not a body speed, and every
// clamp moved the server's model to a point the CLIENT NEVER REPORTED.
//
// MEASURED, logs\go_zone1331.log 2026-11-01 15:23: the clamp fired 50 times in 100
// seconds of ORDINARY play -
//
//	MOVE_PC char 0 reported 641 units in 409ms (1568 u/s), the allowance is 337 units at
//	469 u/s x1.5 + 50 - CLAMPED to (3171, -1007, cell 55)
//
// - and after the control-slot fix 3 of 8 swings still carried a position 33-208 units
// away from the client's own path: drift the clamp itself created, which the client
// answered by hard-setting its actor to our point (GeActor.cpp:309-311).
//
// SO POSITION HONESTY IS NOT SOMETHING A DESTINATION CLAMP CAN BUY. Proving where a body
// may really be needs the server to own the movement (server-authoritative movement,
// PARKED - see ROADMAP.md), not to second-guess each destination. This file's whole job
// now is to make an impossible jump VISIBLE, and the parked item is where the real
// guarantee will come from.
//
// THE ALARM: a report whose destination is more than MoveGuardAlarmDistance units from
// the character's last known point writes ONE line naming both points and the distance,
// and then the report is used anyway. An honest client never fires it - a path step is a
// few hundred units, not thousands - so a line here means a modified client or a real
// bug, and being seen is the whole point.
const (
	// MoveGuardAlarmDistance is how far a report's destination may sit from the
	// character's last known point before the alarm line is written, in world units. It
	// is a REPORTING threshold, not a rule: the report is used whether it fires or not.
	MoveGuardAlarmDistance = 3000.0
)

// guardMoveReport returns the destination to use - ALWAYS the one the client sent - and
// writes one alarm line when that destination is implausibly far from where the model
// last had the body.
//
// stand is the model's own position for that character, read by the caller BEFORE the
// report is applied: the same number the walk segment below opens from, so the line
// always compares the report against the body the server is really drawing.
func (h *Handler) guardMoveReport(idx int, pos, stand [3]int32) [3]int32 {
	// float64 conversions FIRST: an x and a stand of opposite sign would overflow int32
	// subtraction and report a tiny distance for a huge jump.
	dx := float64(pos[0]) - float64(stand[0])
	dy := float64(pos[1]) - float64(stand[1])
	dist := math.Hypot(dx, dy)
	if dist <= MoveGuardAlarmDistance {
		return pos // THE HOT PATH: used exactly as sent, nothing logged
	}
	h.Log.Printf("zone: MOVE_PC ALARM char %d destination (%d, %d, cell %d) is %.0f units "+
		"from its last known point (%d, %d, cell %d), over the %.0f unit alarm - USED AS "+
		"SENT, nothing clamped; position honesty is the parked server-authoritative "+
		"movement item, not this file's job",
		idx, pos[0], pos[1], pos[2], dist, stand[0], stand[1], stand[2],
		MoveGuardAlarmDistance)
	return pos // AND THE JUMP IS USED ANYWAY: shortening it is what corrupted the model
}
