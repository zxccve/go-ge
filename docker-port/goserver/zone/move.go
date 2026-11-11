package zone

import (
	"encoding/binary"
	"math"
	"time"
)

// Movement. The zone does NOT drive movement: the client reports each character's
// new position with CZ_MOVE_PC, and the server echoes ZC_MOVE_PC back. That echo
// is what actually moves the actor (CNormalNet::MovePC -> gemove::ActorMove), so
// without it team members 2 and 3 stand still even though they are in the team.
// Measured on the Python server, which tried leader-only echo and watched two of
// three characters refuse to move.
const (
	CmdCZMovePC = 3598 // client -> server: charIndex(1) + WORLDPOS(12)
	CmdZCMovePC = 3548 // server -> client: 30 bytes

	// CmdZCMoveMonster is PKS_ZC_MOVE_MONSTER (3550): a monster this zone drives
	// starts walking. `PKS_ZC_MOVE_MONSTER : public PKS_ZC_MOVE`
	// (source2\geShared\include\geZCPacket.h:1665), so the layout is the SAME 26
	// bytes as the 3548 without the movement-reason word:
	//
	//	command(2) handle(4) tick(4) x(4) y(4) cell(4) speed(4, float)
	//
	// The client's own table agrees: `3550 26 PKS_ZC_MOVE_MONSTER`
	// (data\cmd_table.txt:1026) and `3550 26 ZC_MOVE_MONSTER`
	// (data\cmd_table_live_z.txt:1217). The reference builds it in
	// CMonsterNPC::BroadcastStartMovement (MonsterNPC.cpp:592-601) with exactly
	// handle, tick, pos and spd.
	CmdZCMoveMonster = 3550 // server -> client: 26 bytes

	// The speed field is the character's own MSPD (469 for a bare character), and it
	// is REQUIRED to be right: the client divides it by the stance's RSPD to pick the
	// run animation's playback rate (geclientlib\geScene\PC.cpp:1175-1180). CFieldChar's
	// constructor placeholder 415.0 (FieldChar.cpp:67) was sent here until 2026-10-28
	// and made every walk animate at 0.88 of its stride. See zone\speed.go.

	// ReasonMove is the movement-reason field. 0 = MR_MOVE; the others
	// (attack 1, skill 2, pick 3, dialog 4) are not used by this slice.
	ReasonMove = 0

	// ReasonDialog is MR_DIALOG: the walk that a click on an NPC starts. The live
	// server uses it for the same walk, and it is what tells the client's own code
	// why the actor is moving (it suppresses idle behaviour while a dialog walk
	// runs).
	ReasonDialog = 4

	// ReasonSkill is MR_SKILL: the walk a SERVER starts so a character can cast a
	// skill it was aimed at from too far away (R87). IT IS MEASURED, not chosen:
	// `captures\rangeapproach20261031\20261031-183747` holds five ZC_MOVE_PC with
	// this reason word, every one of them naming a stand point the client never
	// asked for and the caster then cast from - handle 1619551 to (-2326, 1267,
	// cell 50) at +148432, whose 3573 at +156215 says its begin was exactly that
	// point. The reason word on the client's OWN moves in the same recording is 0
	// (MR_MOVE), so the two are distinguishable on the wire.
	ReasonSkill = 2
)

// zoneStart is the moment THIS ZONE PROCESS started. Every tick a move or a stop packet
// carries is a millisecond count from here, NOT a wall clock (2026-11-02).
//
// THE REFERENCE'S OWN TICK IS THIS SHAPE, and it is measured, not chosen:
// FieldChar_FSM.cpp:249-251 sets `m_moveStartTick = Plat_FloatTime()` when a walk starts,
// and PlayerChar.cpp:232-242 copies it into ZC_MOVE_PC's tick field. In
// captures\moveback\20261102-231419 the server's 3548s carry 0x00074f7c = 479100 - a
// server up about eight minutes - and time.Now().UnixMilli() is ~1.79e12, which the
// 4-byte field truncates into a meaningless number. A restart resets this to zero, the
// same way the reference's own clock restarts.
var zoneStart = time.Now()

// nowMillis is the millisecond clock every move/stop packet carries: milliseconds since
// this process started. It is a function so a test can see where it comes from.
func nowMillis() int64 { return time.Since(zoneStart).Milliseconds() }

// millisOf is nowMillis' inverse: the 3548 clock reading of an instant already passed.
// The MOVING re-enter (3524) needs the leg's START tick on that same clock, not the moment
// the packet is built.
func millisOf(t time.Time) uint32 { return uint32(t.Sub(zoneStart).Milliseconds()) }

// BuildMovePC builds ZC_MOVE_PC:
//
//	command(2) handle(4) tick(4) x(4) y(4) cell(4) speed(4, float) reason(4)
//
// 30 bytes, no length word. The tick is a monotonic millisecond counter; the
// client uses it to order moves, not to place the actor.
func BuildMovePC(handle uint32, pos [3]int32, tick uint32, speed float32) []byte {
	return BuildMovePCReason(handle, pos, tick, speed, ReasonMove)
}

// BuildMovePCReason is BuildMovePC with an explicit reason word (MR_MOVE,
// MR_DIALOG, ...). The reason is not cosmetic: the client stores it and its own
// state machine reads it.
func BuildMovePCReason(handle uint32, pos [3]int32, tick uint32, speed float32, reason uint32) []byte {
	out := make([]byte, 30)
	binary.LittleEndian.PutUint16(out[0:], CmdZCMovePC)
	putMoveFields(out, handle, pos, tick, speed)
	binary.LittleEndian.PutUint32(out[26:], reason)
	return out
}

// BuildMoveMonster is ZC_MOVE_MONSTER (3550), the 26-byte start of a monster's
// walk. It is the same struct as ZC_MOVE_PC minus the reason word, so it shares
// `putMoveFields` with BuildMovePCReason rather than restating the offsets: a
// second copy of eight Put* calls is a second place for one of them to drift.
//
// THE SPEED IS THE MONSTER'S OWN MSPD, not WSPD and not RSPD: the reference's
// CMonsterNPC::GetMoveSPD answers `GetMON_MSPD()` (MonsterNPC.cpp:1929-1932), the
// move FSM stamps `m_moveSpeed = GetMoveSPD()` before it broadcasts the start
// (FieldChar_FSM.cpp:599-603), and SCR_Get_MON_MSPD is `MSPD * (MSPDFix*0.01+1)
// * (100+MSPD_BM)/100` floored (monster\get_status_monster.lua:535-547). For
// class 11591 (Monster_Bigfoot) that is 143.
func BuildMoveMonster(handle uint32, pos [3]int32, tick uint32, speed float32) []byte {
	out := make([]byte, 26)
	binary.LittleEndian.PutUint16(out[0:], CmdZCMoveMonster)
	putMoveFields(out, handle, pos, tick, speed)
	return out
}

// putMoveFields is the seven fields PKS_ZC_MOVE and PKS_ZC_MOVE_MONSTER share, in
// the header's order (geZCPacket.h:1046-1052): handle, tick, x, y, cell, speed.
// It writes from +2 because the caller puts the command word at +0.
func putMoveFields(out []byte, handle uint32, pos [3]int32, tick uint32, speed float32) {
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint32(out[6:], tick)
	binary.LittleEndian.PutUint32(out[10:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(out[14:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(out[18:], uint32(pos[2]))
	binary.LittleEndian.PutUint32(out[22:], math.Float32bits(speed))
}

// ---- ending a walk the PLAYER drove ---------------------------------------

// THE ARRIVAL RULE FOR A LEG THE CLIENT ITSELF OPENED. A ZC_STOP_PC is only ever sent
// when the walk is PROVABLY over, and these two numbers are the whole of the clock that
// decides it:
//
//	over  =  time since the mover's report  >=  segment length / animation speed * StopSafety + StopSlack
//
// where the animation speed is the `spd` we put in the very move both clients got
// (Handler.packetSpeed), NOT the measured speed.
//
// WHY THIS ONE STILL KEEPS ITS PADS (piece 2d). This leg has no body model: the client
// reported a destination and nothing else, and this side has no idea where along that
// leg the actor really is - it can only say "it has had time". A stop names a position
// the OTHER client jumps its actor to (GeMove.cpp:13), so an early one is a visible
// yank, and a late one re-affirms where the actor already idles. The pads therefore buy
// exactly one thing - the stop may only ever fire LATE - and that is still worth 40% plus
// a quarter second here.
//
// THE CHASE NO LONGER USES THEM, and that is the point of piece 2d: a walk this side
// STARTED has a body - walkedRouteAt draws it along the route at the speed the move
// named - so its arrival is the body's own DISTANCE, re-tested every tick (chase.go,
// walk.arrived), and its stop names a point the body provably reached. Nothing on the
// chase path waits for a clock any more.
//
// WHY THAT CANNOT RUN AHEAD OF THE ANIMATION: the client moves the actor at exactly
// `spd` units per second (speed.go, and PC.cpp:1175-1180 only rescales the ANIMATION
// against the same number), and any path it takes around an obstacle is LONGER than
// the straight line we measure. So `length/spd` is the earliest moment the body can
// be there, and the 1.4 factor plus 250 ms only ever push the stop LATER. A late stop
// is harmless - the far actor has already idled, and the stop re-affirms the place it
// is standing in. An early stop is the owner's jump: it yanks a walking actor to a
// point it has not reached.
//
// The speed here used to be Session.walkSpeed(), which is a MEASUREMENT of how fast
// the player re-clicks destinations, not of how fast the body walks; it can read 800
// while the actor animates at 469, and a 469-unit walk timed at 800 fires at 59% of
// the distance. That is the "sometimes it skips straight to the destination" the
// owner reported (R63).
const (
	StopSafety = 1.4
	StopSlack  = 250 * time.Millisecond
)

// charWalk is one character's walk as its own client reported it: where the
// character stood when the report arrived, when that was, the destination the report
// named, and the speed the CLIENT animates that walk at. CZ_MOVE_PC carries the
// DESTINATION, never "where I am now" - the same reason walkedPos exists for the
// gate.
type charWalk struct {
	from [3]int32
	at   time.Time
	dest [3]int32
	// speed is the `spd` of the move we broadcast for this segment: the speed the
	// actor on the other screen is animating at, and therefore the clock's own speed.
	speed float64
	// route is the planned polyline the body walks, FIRST point = from and LAST = dest.
	// Empty means "walk the straight line", which is what every leg was before R110.
	// It is planned ONCE, where the leg opens (Handler.clientRoute), never per tick:
	// the position each tick is arithmetic on this answer (walkedRouteAt).
	route [][3]int32
}

// openWalk records that character idx has just reported a walk to dest, at the
// animation speed of the move that will be broadcast for it.
//
// TWO cases deliberately open NO walk, and both matter:
//
//   - from == dest: a report that names the place the character already stands is
//     not a walk. The client sends one as its last packet sometimes, and sending a
//     stop for it would be the SECOND stop of one stop - which is exactly what this
//     packet pair must not do.
//   - the same destination again while that segment is still running: the client
//     repeats its destination per path step, and restarting the clock on every
//     repeat would postpone the end of the walk for as long as the player walks.
//
// A DIFFERENT destination meanwhile REPLACES the segment, and the replaced one sends
// no stop at all (see endWalks): the new move already re-aims the far actor, and the
// only position the dropped segment could name is one this side never proved the
// character reached.
func (s *Session) openWalk(idx int, from, dest [3]int32, now time.Time, speed float64) {
	s.openWalkTo(idx, from, dest, nil, now, speed)
}

// openWalkTo is openWalk with the leg's ROUTE: the planned polyline the body walks from
// `from` to `dest` (Handler.clientRoute). A nil route is openWalk itself - the straight
// chord - so a caller with nothing to plan says nothing and gets the old geometry.
func (s *Session) openWalkTo(idx int, from, dest [3]int32, route [][3]int32, now time.Time,
	speed float64) {
	if idx < 0 {
		return
	}
	if len(s.walks) <= idx {
		grown := make([]charWalk, len(s.Team))
		copy(grown, s.walks)
		s.walks = grown
		if idx >= len(s.walks) {
			return // a report for a character this team does not have
		}
	}
	if from == dest {
		// A REPORT THAT NAMES WHERE THE CHARACTER ALREADY STANDS STILL NAMES A POINT
		// (R91 round 7): the segment goes, and with it went the only place the model
		// kept a FOLLOWER's live position - drawnPos then fell back to Team[idx].Position,
		// which a walk only writes when it ENDS (endWalks), so the model stood on the
		// point an EARLIER walk last wrote. On the owner's own log 6 of 7 swings CARRIED
		// one of those stale points, 50 to 479 units from the place the client had just
		// reported, and the segment line was zero-length - `from (X, Y) -> (X, Y)` -
		// because both ends came from the same stale record (logs\go_zone1331.log,
		// 2026-11-01 14:43, every one of them char 1, never the leader). The swing's 3564
		// is obeyed by the client, which hard-sets its own actor to it
		// (GeActor.cpp:309-311), so that gap is a teleport on screen: the owner's
		// "twitchy/jittery/snapping". The point this report names IS where the body is,
		// so it is recorded before the segment is dropped.
		//
		// idx 0 IS UNCHANGED ON PURPOSE: movePC already wrote s.moveFrom / s.moveAt /
		// s.Pos for the leader, and drawnPos reads THOSE rather than Team[0].Position,
		// so the leader's answer cannot come from here either way.
		if idx > 0 && idx < len(s.Team) {
			s.Team[idx].Position = dest
		}
		s.walks[idx] = charWalk{}
		return
	}
	if w := s.walks[idx]; !w.at.IsZero() && w.dest == dest {
		return
	}
	s.walks[idx] = charWalk{from: from, at: now, dest: dest, speed: speed, route: route}
}

// walkedOut says whether one segment has been totally walked by now, under the
// arrival rule above.
//
// There is no timeout here and there must not be one - a timeout would also end a
// walk that is still going. This answers "the body has had time to cover the whole
// segment at the speed its own animation runs at".
//
// THE SEGMENT IS THE LEG'S OWN LENGTH (2026-11-02). A leg that carries a planned route
// is DRAWN along that polyline (walkedRouteAt), so its length is the route's, not the
// chord's: timing a bent walk on the chord would fire the stop before the body has
// walked the bend, and the far client does SetPosition(pos) with whatever the stop names
// - a jump. The rule itself is untouched: route length / spd is still the EARLIEST
// moment the body can be there, and StopSafety/StopSlack still only push the stop later.
//
// THIS IS A PLAYER-DRIVEN LEG ONLY (piece 2d). A walk this side STARTED has a body and
// arrives on its distance (chase.go, walk.arrived); these pads are not part of that
// answer and must not be reused for it.
func (w charWalk) walkedOut(now time.Time) bool {
	if w.at.IsZero() {
		return false
	}
	seg := legLength(w.from, w.dest, w.route)
	if seg == 0 {
		return true
	}
	spd := w.speed
	if spd <= 0 {
		spd = MoveBaseSpeed
	}
	need := time.Duration(seg/spd*StopSafety*float64(time.Second)) + StopSlack
	return !now.Before(w.at.Add(need))
}

// walkEnded says whether character idx's walk has been walked out by now.
func (s *Session) walkEnded(idx int, now time.Time) bool {
	if idx < 0 || idx >= len(s.walks) {
		return false
	}
	return s.walks[idx].walkedOut(now)
}

// hasOpenWalk reports whether ANY character of this team has a reported walk that
// has not been closed yet.
//
// THIS IS WHAT MAKES endWalks REACHABLE, and it was the whole bug on 2026-10-28:
// Serve sets a read deadline ONLY while a chase or a pending click is running, and
// the zone's tick (chaseTick, which calls endWalks) only runs when that deadline
// fires. A player who clicks the GROUND starts no chase, so the loop sat in a
// blocking read for ever, endWalks never ran, and the other client ran in place -
// with the 3549 rule present, tested, and never executed. Measured in
// logs\zone10_live.log after the 20:53:52 restart: 12 MOVE_PC reports, 0 stops.
func (s *Session) hasOpenWalk() bool {
	for i := range s.walks {
		if !s.walks[i].at.IsZero() {
			return true
		}
	}
	return false
}

// wantsTick says whether Serve must wake up on a timer for this session.
//
// Five things need a clock: a chase we started (the arrival opens the dialog), a walk
// the PLAYER drove (its arrival is owed one ZC_STOP_PC - see endWalks), a cast waiting
// on its own wind-up (its result is owed a 3573/3581 - see zone\skill_cast_clock.go), a
// normal attack's repeat (its next swing is owed a 3564 + 3557 at the character's own
// ASPD - see zone\attack_swing.go), and a map handoff waiting for its claim.
//
// A SIXTH: a press the cadence gate KEPT (zone\attack.go, holdPress). It is owed a hit
// the moment the character's own delay runs out, and it has no walk of its own to carry
// it - the reference re-runs the stored request on every tick (PlayerChar.cpp:265), so
// without this clock the kept press would wait for the client's next packet, which
// never comes.
//
// An out-of-reach press needs no clock of its own (R91 round 6): its walk is in `Walk` (or,
// since R131, in that character's own slot of `teamWalks`) from the moment it is pressed, and the hit it owes fires when the body's distance says
// so. A CLICK needs no waiting-room clock any more either (piece 2d): it starts its walk
// the instant it is clicked, so there is nothing queued to wake up for. Nothing else
// does: a session with none of these must block on the socket, or every idle player
// would be woken ten times a second for nothing.
func (s *Session) wantsTick() bool {
	// A TEAM WALK IS A REASON TO TICK ON ITS OWN (R131): an approach can live in teamWalks
	// with Walk nil - character 2's walk while character 0 holds the click record - and a
	// session that stopped ticking would never advance it, fire it, or give it up.
	return s.Walk != nil || len(s.teamApproaches()) > 0 || s.hasOpenWalk() ||
		s.Handoff != nil || s.hasCast() || s.hasSwing() || s.hasPress()
}

// rememberWalk records where character idx's OWN reported segment says the body is at
// `now`, for a caller that is about to DROP that segment without writing the position
// itself (R91 round 7).
//
// WHY A DROPPED SEGMENT NEEDS THIS. The segment is the only place the model keeps a
// follower's live point. With it gone, drawnPos and charPos fall back to
// s.Team[idx].Position - and a walk only writes that when it ENDS (endWalks) or is
// PLACED (placeTeamAt, placeWalker). So dropping a segment mid-walk put the model
// back on the point an EARLIER walk last wrote, which is the 50..479 unit gap the
// swing then handed to the client (see the from == dest branch of openWalk, and
// logs\go_zone1331.log, 2026-11-01 14:43).
//
// idx 0 IS DELIBERATELY SKIPPED: the leader's live point is s.moveFrom -> s.Pos with
// s.moveAt, written by movePC, and both accessors read that - never Team[0].Position -
// so writing here would be a second source of truth for slot 0.
func (s *Session) rememberWalk(idx int, now time.Time) {
	if idx <= 0 || idx >= len(s.Team) || idx >= len(s.walks) {
		return
	}
	w := s.walks[idx]
	if w.at.IsZero() {
		return // no segment open: nothing newer than the stored position to record
	}
	// ALONG THE LEG'S ROUTE when it has one (2026-11-02): a bent leg is DRAWN on its
	// polyline, so the point it is dropped on must be read from that polyline too. The
	// chord across a bend is the same snap this lane removed from every other reader.
	s.Team[idx].Position = walkedRouteAt(w.route, w.from, w.dest, w.at, now, w.speed)
}

// clearWalks drops every open segment: the team was PLACED (a warp, a spawn), not
// walked, and a segment left open would fire a stop at a place the character never
// stopped at.
//
// IT NEEDS NO rememberWalk (checked R91 round 7): both callers - placeTeamAt and the
// entry path - are placements that write EVERY member's own position themselves
// (town.go's member loop, and the spawn slot the entry path laid out), so a fallback
// here can only ever read the point the team was just put on.
func (s *Session) clearWalks() {
	for i := range s.walks {
		s.walks[i] = charWalk{}
	}
	// AND THE SENT RECORDS DIE WITH THEM (zone\walk_once.go): a placement ends every
	// walk, so nothing is "still in flight" afterwards and a click to a point a
	// pre-warp walk named must be sent, not refused as a duplicate.
	for i := range s.walksSent {
		s.walksSent[i] = walkSent{}
	}
	// AND NO PLANNED ROUTE SURVIVES A PLACEMENT: a warp or a spawn puts the body
	// somewhere new, so the polyline the driven character last walked describes ground
	// it is no longer on. (walkedRouteAt also refuses a route whose ends are not the
	// leg's own, so a stale one could not be misread - this drops it rather than
	// relying on that.)
	s.moveRoute = nil
	// AND NO WALKED-OUT LEG SURVIVES A PLACEMENT (R111): the body was put somewhere, so
	// the leg it last walked describes ground it is no longer on.
	for i := range s.keptLegs {
		s.keptLegs[i] = walkedLeg{}
	}
}

// closeWalk drops ONE character's open segment, used where the caller sends its own
// stop and a second one would repeat it.
//
// A CALLER THAT DROPS A FOLLOWER'S SEGMENT MID-WALK MUST CALL rememberWalk FIRST
// (R91 round 7) - otherwise the body's live point is lost and the next reader falls
// back to an older stored one. The sites that need it are marked at their calls; the
// ones that do not are: endWalks (it writes w.dest), placeWalker and stopWalker (the
// stop's goal is written before the drop), and every leader-only site (idx 0 keeps
// its own s.Pos, which rememberWalk returns from untouched).
func (s *Session) closeWalk(idx int) {
	if idx >= 0 && idx < len(s.walks) {
		s.walks[idx] = charWalk{}
	}
}

// endWalks sends ONE ZC_STOP_PC to every other player for each character whose own
// walk has finished, and nothing for a walk that is still running.
//
// WHY THIS EXISTS AT ALL, and why the original does not need it: the shipped server
// sends ZC_STOP_PC only when it CUTS a walk short or PLACES a character - the only
// builder is CPlayerChar::BroadcastStopMovement (ZServerLib\PlayerChar.cpp:296-304),
// called from CFieldChar::Stop (FieldChar.cpp:285), OnWarp (FieldChar_Message.cpp:605)
// and the team warp (Commander.cpp:4374). A walk that simply ARRIVES sends nothing:
// the far client's own actor notices the destination is reached
// (GeAiActor_FSM.cpp:216-220, IsNearFromMoveDestPosition(100)) and drops to idle.
//
// The 15 ZC_STOP_PC in captures\warp_ref\20261027-200136\ agree: they are five
// warp triples, all with the SAME tick, and not one of them is preceded by a
// ZC_MOVE_PC. So this is not a port of an arrival packet - it is the close of OUR
// own hole: the client reports a walk as a series of path steps, every step restarts
// the far actor's interpolation from a logical position that only moves on
// COMPLETION, and without a stop the actor is left animating at the spot it stopped
// at (measured live 2026-10-28, two clients, map 10).
//
// A SECOND CAPTURE, COUNTED (captures\animref\20261028-221827, town, 1,746 packets):
// 58 ZC_MOVE_PC and 3 ZC_STOP_PC - and all three stops are the team being PLACED on
// entry, before any move of those handles. None of the 8 walks in it ends with a stop,
// and the player-driven ones carry 4-12 moves each, one per path step. So the far
// client truly does idle on its own; our stop exists only because the port measured it
// otherwise, and what R63 fixed is WHEN it may be sent (the arrival rule above).
//
// ONE stop per stop: the segment is cleared as it is closed, so the next call in the
// same arrival sends nothing.
//
// A segment that a LATER report replaced is already gone from sess.walks and can
// therefore never stop the far actor at a place the character never stood - that is
// the chained-click case, and it is handled by openWalk, not here.
//
// THE LEADER IS SKIPPED WHILE A CHASE RUNS: the client reports path steps of its own
// during a server-driven walk too, and one of those segments ending would stop the
// far actor half way to the NPC, in the middle of the walk the chase announced. The
// chase's own arrival closes the leader's stop (chase.go, stopOthers).
func (h *Handler) endWalks(sess *Session, now time.Time) {
	for idx := range sess.walks {
		// The character a walk is running for is SKIPPED, whatever slot it is in: the
		// client reports path steps during a server-driven walk too, and one of those
		// segments ending would stop the far actor half way there, in the middle of
		// the walk the chase announced. The chase's own arrival closes that stop
		// (chase.go, stopOthers / stopWalker). For a click the walking slot is 0, so
		// this is the rule that was always here - R87 only made the slot a field.
		if sess.walkOwnedBy(idx) != nil {
			continue
		}
		if !sess.walkEnded(idx, now) {
			continue
		}
		w := sess.walks[idx]
		sess.walks[idx] = charWalk{}
		if idx >= len(sess.Team) {
			continue
		}
		handle := sess.Team[idx].Handle
		// The stop names the DESTINATION that was walked out - that is where the
		// character stands now, and the far client does SetPosition(pos) with it.
		pos := w.dest
		// The stored position of this character must follow the stop, or its next
		// walk segment would start from where it used to stand.
		sess.Team[idx].Position = pos
		h.broadcastStop(sess, idx, handle, pos, BuildStopPC(handle, pos))
	}
}

// walkedToAt is the walked-position rule with the speed given EXPLICITLY: the body is
// walked from `from` toward `dest` at `speed` units per second, and it never overshoots.
//
// gate.go's Session.walkedTo is this with the MEASURED speed, which is what the gate
// wants. The stop clock passes the speed the client ANIMATES at instead - a body can
// never be further along than that, and a measured 800 u/s would place a body that
// animates at 469 well past where it can possibly be (R63).
func walkedToAt(from, dest [3]int32, at, now time.Time, speed float64) [3]int32 {
	elapsed := now.Sub(at)
	if elapsed < 0 {
		elapsed = 0 // a caller with an earlier clock must not walk backwards
	}
	dx := float64(dest[0]) - float64(from[0])
	dy := float64(dest[1]) - float64(from[1])
	seg := math.Hypot(dx, dy)
	if seg == 0 {
		return dest // standing still: no direction, and dividing by 0 is NaN
	}
	travelled := speed * elapsed.Seconds()
	if travelled >= seg {
		return dest
	}
	f := travelled / seg
	return [3]int32{
		int32(float64(from[0]) + dx*f),
		int32(float64(from[1]) + dy*f),
		dest[2],
	}
}

// walkedRouteAt is walkedToAt for a walk that carries a PLANNED ROUTE: the body travels
// `speed` x elapsed units ALONG the polyline, corner by corner, and the point it has
// reached sits on the segment it is on.
//
// WHY (R110). The walk's clock already counts the route's own length (nav.go, Plan), so
// a bent walk was TIMED on the polyline and then DRAWN on the straight chord between the
// two ends. The two disagree by exactly how much the route bends, and the client obeys
// the point a swing names (GeActor.cpp:300-309 hard-sets its actor to srcPos), so the
// disagreement is a snap on screen. The reference has no such gap: it advances its own
// path-engine agent and takes THAT position (FieldChar.cpp:240-260, FieldChar_FSM.cpp:
// 246-270: `dist = m_moveSpeed * (tick - m_moveCurrentTick)`, then Advance(dist)).
//
// `route` is the engine's polyline, whose first point is the walk's start and whose last
// is its goal (nav.go, Plan). The CELL comes from the segment's end, which is the ground
// this side has a real navmesh triangle for.
//
// A walk with NO route - no path engine, the walk graph, a retreat, anything that never
// asked the engine - must stay exactly what it was, so an empty route, a one-point route
// or a non-positive speed all fall back to walkedToAt. That fallback is what keeps a
// straight walk bit-identical to the old geometry instead of merely close.
//
// A ROUTE THAT DOES NOT DESCRIBE THIS LEG IS NOT THIS LEG'S ROUTE (2026-11-02). The
// first point must be `from` and the last must be `dest`; anything else falls back to the
// chord. That is what lets a leg record be OVERWRITTEN by a placement (a warp writes
// moveFrom/moveAt and nothing else) without a polyline from the previous map being drawn
// under the new body: the ends no longer match, so the stale answer is never used.
func walkedRouteAt(route [][3]int32, from, dest [3]int32, at, now time.Time, speed float64) [3]int32 {
	if speed <= 0 || !legRouteMatches(route, from, dest) {
		return walkedToAt(from, dest, at, now, speed)
	}
	elapsed := now.Sub(at)
	if elapsed < 0 {
		elapsed = 0 // a caller with an earlier clock must not walk backwards
	}
	left := speed * elapsed.Seconds()
	for i := 1; i < len(route); i++ {
		ax, ay := float64(route[i-1][0]), float64(route[i-1][1])
		bx, by := float64(route[i][0]), float64(route[i][1])
		seg := math.Hypot(bx-ax, by-ay)
		if left >= seg {
			// This leg is behind the body - including a ZERO-length corner, which is
			// walked in no distance at all.
			left -= seg
			continue
		}
		f := left / seg
		return [3]int32{
			int32(ax + (bx-ax)*f),
			int32(ay + (by-ay)*f),
			route[i][2],
		}
	}
	// The route is walked out: the last point is where the walk ends, which is the goal.
	return route[len(route)-1]
}

// flatDist is the ground-plane distance between two points, for the move log line.
// The cell is ignored: a walk happens on one map layer.
func flatDist(a, b [3]int32) float64 {
	return math.Hypot(float64(a[0])-float64(b[0]), float64(a[1])-float64(b[1]))
}

// legRouteMatches says the polyline really is a route for THIS leg: at least two points,
// the first the leg's own start and the last its own destination. Everything that reads
// a route asks this first, so a route left behind by an earlier leg (a placement writes
// moveFrom/moveAt and nothing else) can never be drawn under the new body.
func legRouteMatches(route [][3]int32, from, dest [3]int32) bool {
	return len(route) >= 2 && route[0] == from && route[len(route)-1] == dest
}

// legLength is how far the body really walks a leg: along its planned polyline when it
// has one that matches (legRouteMatches), and along the straight line otherwise.
//
// ONE answer for the two things that must agree about a leg's length: the drawn position
// (walkedRouteAt) and the stop's clock (charWalk.walkedOut). A bent leg timed on its
// chord would stop the far actor before the body had walked the bend.
func legLength(from, dest [3]int32, route [][3]int32) float64 {
	if legRouteMatches(route, from, dest) {
		return polylineLength(route)
	}
	return flatDist(from, dest)
}

// clientRoute plans the polyline a CLIENT's reported leg walks: the body's own live point
// -> the destination that report named.
//
// WHY (R110, extended to the client's own legs 2026-11-02). A leg is DRAWN along its
// route and TIMED by it; a client's leg had no route at all, so a walk the client bent
// around a wall was drawn on the straight chord across it. The swing's 3564 hard-sets
// the client's actor to the position this side names (GeActor.cpp:300-311), so the gap
// between the two is a jump on screen. The planner is the SAME one the chase uses (nav.go,
// PathEngine.Plan against the map's own mesh), so the two cannot disagree about a map.
//
// IT RUNS ONCE PER LEG, where the leg opens, and never per tick: a plan is a process
// against the navmesh, and a tick only does arithmetic on the answer (walkedRouteAt).
//
// nil is a legitimate answer and means "walk the straight line": no path engine on this
// machine, no route between the two points, or a Handler a test built without a Route.
// Every reader then falls back to the chord, which is what every client leg was before.
func (h *Handler) clientRoute(from, to [3]int32) [][3]int32 {
	if from == to {
		// A report that names the place the body stands opens no leg at all (move.go,
		// openWalk), so there is nothing to plan - and a plan is a process against the
		// navmesh, so one made for a leg that will not exist is pure cost.
		return nil
	}
	if h.Route == nil {
		// NO PLANNER: a chord that cannot be refused (the client already moved), so it is
		// counted and named once per (map, cell pair) - never silent (2026-11-09).
		h.noteClientChord(from, to)
		return nil
	}
	route, ok := h.Route(from, to)
	if !ok || len(route) < 2 {
		// A chord here cannot be refused - the client has already moved - so it is
		// COUNTED and named once per (map, cell pair): never silent (2026-11-09).
		h.noteClientChord(from, to)
		return nil
	}
	// THE TWO ENDS ARE FACTS, NOT THE MESH'S OPINION OF THEM. The body's live point and
	// the destination the client named are both exact; the engine snaps a corner onto its
	// own sampling. The leg must start where the body is and end where the client
	// pointed, or the drawn body would step sideways the instant the leg opened.
	if route[0] != from {
		route = append([][3]int32{from}, route...)
	}
	if route[len(route)-1] != to {
		route = append(route, to)
	}
	return route
}

// closeLegForControlSwitch re-seats the session's ONE walk segment on the character the
// player has just taken over (CZ_SELECTCHANGE_PC, zone\control.go).
//
// WHY IT MUST EXIST. The driven character's leg is read from moveFrom / Pos / moveAt
// (move.go, clientLegOf) - ONE record for the whole session, however many characters are
// on the team - and a switch used to leave it pointing at the leg the PREVIOUS character
// was walking. The new character then read that leg as its own and its swing position was
// interpolated along it (measured 2026-11-01 23:31:53: char 2's swing claimed the
// segment (2560,-1207) -> (2119,-797), and (2560,-1207) was char 1's point). The client
// hard-sets its actor to that position, which is the teleport.
//
// It closes the old leg rather than converting it: a leg belongs to the body that walked
// it, and the new body never stood on its start. `Pos` moves to the new character's own
// last point, so the next CZ_MOVE_PC opens a leg from the right place - and the new
// character's own walk record is dropped for the same reason, while an approach walk
// belonging to somebody else is released so the tick cannot chase with it.
func (s *Session) closeLegForControlSwitch(idx int) {
	s.moveAt = time.Time{}
	s.moveFrom = [3]int32{}
	s.moveRoute = nil
	if idx >= 0 && idx < len(s.Team) {
		s.Pos = s.Team[idx].Position
	}
	if idx >= 0 && idx < len(s.walks) {
		s.walks[idx] = charWalk{}
	}
	// NOTHING ELSE IS TOUCHED, and that is the rule now (2026-11-04, R131). A character's
	// approach belongs to THAT character: keep mode is pressed per character and runs until
	// that character is told otherwise, so a switch changes which body the player's input
	// goes to and nothing more. The owner's words: "you want each of the character to be
	// independent unless u use the group mode ... changing leader should not affect
	// anything"; the reference does exactly this - CCommander::OnSelectChangePC sets
	// m_pcLeaderIndex and returns (Commander_Packet.cpp:1284-1302), while every CPlayerChar
	// keeps its own request, its own move target and its own FSM.
	//
	// WHAT THIS REPLACED, and why it was wrong: an approach was DROPPED here - no packet,
	// no log, and the body's stored point left at the START of the leg. Measured 2026-11-04
	// 17:12:15: a 419-unit approach sent for char 0, a switch 48 ms later, then the next
	// press re-planned the SAME leg with the distance still 1375.6490097580793 to thirteen
	// decimals - two seconds standing still, and the press that was 0.9 s from landing took
	// 2.65 s. Thirteen of that session's 38 switches came within 3 s of a walk this server
	// had just sent, so it is not an edge case.
}

// reportRepeats says this report names the destination the character's open leg already
// ends at.
//
// A REPEAT OPENS NO LEG, and the reason is the clock, not the geometry: the client
// repeats its destination per path step, so restarting the leg on every repeat would
// postpone its end for as long as the player keeps walking. It reads the same leg every
// other reader does (clientLegOf), so "repeated" cannot mean one thing here and another
// in the position a swing carries. The reference refuses it at the same place:
// CFieldChar::DispatchMove returns early when the destination equals m_moveTo
// (FieldChar.cpp:724-745).
func (s *Session) reportRepeats(idx int, pos [3]int32) bool {
	leg := s.clientLegOf(idx, 0)
	return !leg.placed && leg.dest == pos
}

// swingSegment names the inputs a swing's position came from: the segment the
// character is walking (`from` -> `dest`) and how long it has been walking it. It
// reads the SAME record attackBodyPoint reads, through the same resolver, so the log
// line and the number in the packet can never describe different segments.
// IT ASKS THE SAME FUNCTION attackBodyPoint ASKS (R111). It used to walk the two records
// itself, which is how one `SWING POS` line came to name the client's leg while the point
// beside it had been read from the WALK - two readers deciding one leg is the second clock
// this round removes. swingLegOf is the one decision, and legElapsed is the one age (R112):
// the line's elapsed stops where the leg's own walk time does, exactly as the point stops
// on the leg's far end, so this line and the packet cannot drift.
func (s *Session) swingSegment(idx int, now time.Time) (from, dest [3]int32, elapsed time.Duration) {
	leg := s.swingLegOf(idx, 0)
	if leg.placed {
		return leg.from, leg.dest, 0 // placed, not walking: the point is the place
	}
	return leg.from, leg.dest, legElapsed(leg, now)
}

// charPos is where character idx stands NOW by the walked-position rule: the
// leader has the session's own segment (walkedPos), a follower has the segment its
// own move reports opened, and a character with neither was PLACED - its stored
// position is then the truth.
//
// A FOLLOWER'S LEG IS A ROUTE TOO (2026-11-02): its own report opens the leg on the
// planned polyline (openWalkTo), so the answer is where the body is ALONG that polyline.
// The speed is the measured one, which is what this function has always used - the
// position a SWING carries is drawnPos, at the speed the client was told.
func (s *Session) charPos(idx int, now time.Time) [3]int32 {
	if idx == s.Control {
		// SENSE: the controlled character has the session's own segment (walkedPos).
		// Not "slot 0" - the whole point of zone\control.go.
		return s.walkedPos(now)
	}
	if idx < 0 || idx >= len(s.Team) {
		return [3]int32{}
	}
	if idx < len(s.walks) {
		if w := s.walks[idx]; !w.at.IsZero() {
			return walkedRouteAt(w.route, w.from, w.dest, w.at, now, s.walkSpeed())
		}
	}
	return s.Team[idx].Position
}

// ParseMovePC reads CZ_MOVE_PC:
//
//	security header(10) charIndex(1) x(4) y(4) cell(4)
//
// charIndex is the position in the TEAM (0 = leader), not a character id; the
// client sends one of these per character it moves, including the followers.
// ok is false for a short packet, which is a malformed client and not a crash.
func ParseMovePC(pt []byte) (charIndex int, pos [3]int32, ok bool) {
	if len(pt) < 23 {
		return 0, [3]int32{}, false
	}
	charIndex = int(pt[10])
	pos[0] = int32(binary.LittleEndian.Uint32(pt[11:]))
	pos[1] = int32(binary.LittleEndian.Uint32(pt[15:]))
	pos[2] = int32(binary.LittleEndian.Uint32(pt[19:]))
	return charIndex, pos, true
}
