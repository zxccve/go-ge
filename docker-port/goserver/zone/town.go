package zone

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"geserver/protocol"
)

// The town quick-move ("Warp (Alt+F3)").
//
// The client owns the PANEL and the server owns the DESTINATIONS. The panel's four
// buttons are drawn from `datatable_town.ies` (PointName1..4) by the client itself,
// which never sends a coordinate: it sends the BUTTON NUMBER. Where that button leads
// is a server-side row, and today the row is the map's own `points` list in
// data\zone_points.json (written by src\emit_zone_points.py from the same table).
//
// The answer is ONE ZC_STOP_PC (3549, 22 B) per character and NOTHING ELSE - no
// ZC_MOVE_PC, no ZC_ROTATE - in REVERSE team order (last member first, the LEADER LAST),
// all sharing ONE tick. A live capture of the real server settled this; the three attempts
// below are kept because they explain why each wrong shape looked plausible, and so nobody
// re-litigates it.
//
// THE EVIDENCE: capture 2026-10-27, warps to points 2/3/4/1 of reb_basic from pktmon
// traffic to the live server (four warps plus a gate crossing). Every warp was three
// 3549s and nothing else. Point 2, verbatim:
//
//	handle 972620 (-5909, 1269, cell 218) = leader + (-75, -44)  <- sent FIRST (slot 2)
//	handle 972619 (-5909, 1356, cell 218) = leader + (-75, +43)  <- slot 1
//	handle 972618 (-5834, 1313, cell 218) = the point itself     <- LEADER, sent LAST
//
// All three carried the SAME tick (528554986), and all three the leader's cell. The four
// clusters landed exactly on those four points, so the geometry is confirmed too.
//
// Attempt 1 - move + stop per character. Landed the team correctly in game (the V was
// right), but 3548 starts a PATH and the client turns the actor to face it, so the third
// character's facing was arbitrary and re-created on every warp.
//
// Attempt 2 - move + stop, LEADER ONLY. Wrong: the other two never moved. The button is
// the commander's but the warp is a team move, and this client keys its actors by handle,
// so every member needs its own packet. Leader-only is never the answer.
//
// Attempt 3 - stop only per character, in team order with one tick per packet. Did not
// work in game: one character missing and the others stacked. The capture shows the shape
// was RIGHT and the ORDER and TICK were wrong - the client anchors the team on the leader,
// so stops that name the leader first make the follower stops stale. Hence: leaders last,
// one tick.
const (
	// CmdCZReqTownMove is the panel button, FIXED 14 bytes: the 10-byte security
	// header, then one int32.
	CmdCZReqTownMove = 3277

	// townMoveSize is the WHOLE packet. There is no length word to read: the size
	// belongs to the command in the client's own table.
	townMoveSize = 14

	// townMoveButton is where the int32 sits, and how many buttons the client has.
	townMoveButton  = 10
	townMoveButtons = 4
)

// ParseTownMove reads the button number from CZ_REQ_TOWN_MOVE.
//
// ok is false for a packet too short to hold the int, which is a malformed client and
// not a crash. The value is NOT range-checked here: the refusal belongs where the
// button is resolved, so the log line can say what the number was.
func ParseTownMove(pt []byte) (button int, ok bool) {
	if len(pt) < townMoveSize {
		return 0, false
	}
	button = int(int32(binary.LittleEndian.Uint32(pt[townMoveButton:])))
	return button, true
}

// townMove answers one quick-move button: put the WHOLE TEAM on the point's formation,
// with ONE ZC_STOP_PC (3549, 22 B) per character and nothing else - in REVERSE team order,
// last member first and the LEADER LAST, every packet on the SAME tick. That is the shape
// the live server was captured sending (see the file comment above).
//
// It always answers nil packets on a refusal, so a bad button, a field map or a point
// with no cell moves NOBODY and says why in one line. Nothing here guesses: a
// coordinate that is not in the file is somebody else's map, and a HEIGHT in the cell
// slot crashes the client inside PathEngine on the next walk.
//
// Every character gets its OWN stop: the client keys the actor by handle, so the leader's
// handle on a follower's packet moves the leader twice and the follower never. See the
// loop below for why the whole team and not just the commander.
//
// The order is not cosmetic. The client anchors the team on the leader, so a stop naming
// the leader FIRST makes the follower stops that follow it stale - that was attempt 3,
// which landed one character missing and the rest stacked. One tick for the whole warp is
// part of the same rule: the real server's three stops share it.
func (h *Handler) townMove(sess *Session, f protocol.Frame) ([][]byte, error) {
	button, ok := ParseTownMove(f.Data)
	if !ok {
		h.Log.Printf("zone: REQ_TOWN_MOVE carries %d bytes, want %d - no button to read, "+
			"nobody moved", len(f.Data), townMoveSize)
		return nil, nil
	}
	point, err := h.townPoint(button)
	if err != nil {
		h.Log.Printf("zone: town move button refused - nobody moved: %v", err)
		return nil, nil
	}
	if len(sess.Team) == 0 {
		h.Log.Printf("zone: town move button %d before the team exists - nobody to move",
			button)
		return nil, nil
	}
	// THE PLACEMENT ITSELF is placeTeamAt, so a Warp List jump to a point on this
	// map moves the team with THESE bytes and not with a second copy of them (see
	// the function, and commwarp.go's commWarpSelect).
	out, tick, placed := h.placeTeamAt(sess, point.Slot(0), point.Slot)

	// One line that PROVES what went out, in the order it went out, and in the same terms
	// as the capture so the two can be compared by eye. "The team stacked" is
	// unanswerable without it.
	h.Log.Printf("zone: TOWN_MOVE button %d: %d character(s), stop only, reverse order, "+
		"one tick %d - cmd %d (%d B each), written: %s",
		button, len(sess.Team), tick, CmdZCStopPC, len(out[0]), strings.Join(placed, ", "))
	return out, nil
}

// placeTeamAt puts the WHOLE TEAM on a set of positions, with ONE ZC_STOP_PC (3549, 22 B)
// per character and NOTHING ELSE, and is THE in-zone teleport of this server.
//
// It is ONE function on purpose: the town quick-move (townMove above) and a Warp List
// jump to a saved point on the map the player already stands on (commWarpSelect,
// commwarp.go) are the same movement. The rules below took three attempts to get right
// in game, so the second caller reuses them instead of holding a copy that could drift.
//
// `anchor` is where the LEADER lands, and it is also what the session's own position
// becomes. `slotAt` answers each member's place. A caller whose point carries no formation
// of its own passes `formationSlot` over a shape it built - the Warp List's jump does
// exactly that (commwarp.go: this map's own arrival formation, laid around the saved
// point) - and only a caller with nothing to spread passes a function that returns the
// anchor for every index, which stands the whole team on one spot.
//
// ORDER: ONE stop per character, the LEADER LAST, so the loop runs backwards. A town
// button is the commander's, but a warp is a TEAM move - CCommander::OnReqTownMove ->
// SetPos -> OM_WARP per PC -> CFieldChar::OnWarp re-positions the whole team
// (Commander.cpp:1270-1286) - and this client keys its actors by handle, so every member
// needs its own packet. Leader-only left the other two standing in game.
//
// ONE TICK for all of them, computed here and not per packet: the capture's three stops
// share tick 528554986. And each member's STORED position is updated, or that
// character's next move report starts from where it used to be and contradicts the jump.
func (h *Handler) placeTeamAt(sess *Session, anchor [3]int32, slotAt func(int) [3]int32) (out [][]byte, tick uint32, placed []string) {
	if len(sess.Team) == 0 || slotAt == nil {
		return nil, 0, nil
	}
	handle := sess.Team[0].Handle

	// A warp is not a walk: drop a chase first, or it keeps walking the character
	// back to the NPC it was heading for.
	h.cancelChase(sess)
	sess.Pos = anchor
	sess.moveFrom, sess.moveAt = anchor, time.Now() // a warp, not a walk
	sess.LastPos = anchor
	sess.LastMover = handle
	// No reported walk survives a placement: a segment left open would fire a stop
	// at a spot the character never stopped at (see endWalks).
	sess.clearWalks()
	// And no pending cast survives one either: its result would land on a screen that
	// has just been placed on ANOTHER map, naming a point on the old one
	// (zone\skill_cast_clock.go).
	h.dropCasts(sess, "the team was placed by a warp or a map change")

	tick = uint32(nowMillis())
	out = make([][]byte, 0, len(sess.Team))
	placed = make([]string, 0, len(sess.Team))
	for i := len(sess.Team) - 1; i >= 0; i-- {
		c := sess.Team[i]
		memberPos := slotAt(i)
		memberStop := BuildStopPCAt(c.Handle, memberPos, tick)
		out = append(out, memberStop)
		sess.Team[i].Position = memberPos
		h.broadcastStop(sess, i, c.Handle, memberPos, memberStop)
		placed = append(placed, fmt.Sprintf("slot %d handle %d -> (%d, %d, cell %d)",
			i, c.Handle, memberPos[0], memberPos[1], memberPos[2]))
	}
	return out, tick, placed
}

// townPoint resolves one button to its point (with its formation), or explains why it
// cannot.
//
// The list is searched by its own `n`, not by position, so a file whose points are
// out of order or missing a number refuses the button it cannot answer instead of
// silently serving a different place.
func (h *Handler) townPoint(button int) (TownPoint, error) {
	if button < 1 || button > townMoveButtons {
		return TownPoint{}, fmt.Errorf("movetype %d is not a button: the client sends 1..%d",
			button, townMoveButtons)
	}
	if len(h.Points) == 0 {
		return TownPoint{}, fmt.Errorf("map %d has no town points, so it has no quick-move "+
			"panel (a field map) - build them with: cd src && python emit_zone_points.py",
			h.MapID)
	}
	for _, p := range h.Points {
		if p.N != button {
			continue
		}
		if p.Cell == nil {
			// The emitter's path engine found no ground near the point. That is
			// "nowhere to land", not a number to fill in.
			return TownPoint{}, fmt.Errorf("map %d point %d (%d, %d) has NO resolved cell - "+
				"the path engine found no ground there, so there is nowhere to land; "+
				"rebuild the table with: cd src && python emit_zone_points.py",
				h.MapID, p.N, p.X, p.Y)
		}
		if !PlausibleCell(*p.Cell) {
			return TownPoint{}, fmt.Errorf("map %d point %d: %w", h.MapID, p.N, badCellError(*p.Cell))
		}
		return p, nil
	}
	return TownPoint{}, fmt.Errorf("map %d has no point %d in the table (it carries %d point(s))",
		h.MapID, button, len(h.Points))
}
