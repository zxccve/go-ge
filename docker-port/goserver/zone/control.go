package zone

import (
	"geserver/protocol"
)

// CmdCZSelectChangePC is CZ_SELECTCHANGE_PC (3652), client -> server: the player
// changed WHICH character of the team they drive. The client switches silently -
// it sends no other packet about it, and the server cannot see the click - so this
// frame is the ONLY signal that "the player's character" is no longer slot 0.
//
// It is in the client's own command table (docker-port\data\cmd_table.txt:1128,
// `3652   11    PKS_CZ_SELECTCHANGE_PC`, 11 bytes fixed) and its struct is at
// source2\geShared\include\geZCPacket.h:1890.
//
// FIRST ONE EVER SEEN, unhandled, in docker-port\logs\go_zone1331.log at 10:28:02:
//
//	cmd 3652 not handled yet (first seen), 11 bytes: 44 0e 18 00 00 00 4b 00 00 00 01
//
// 0x0e44 = 3652, then the 10-byte security header, then ONE payload byte = 01: the
// team index the player switched to. It is the same shape as CZ_MOVE_PC's own
// charIndex at +10 - the client names a team slot, not a character id.
const CmdCZSelectChangePC = 3652

// selectChangePCWidth is the whole frame, header included: cmd(2) seq(4)
// checksum(4) + index(1). The client builds it from a fixed struct, so 11 is the
// number this build sends - and a different number means a different build, not a
// frame to be reinterpreted.
const selectChangePCWidth = 11

// ParseSelectChangePC reads CZ_SELECTCHANGE_PC:
//
//	security header(10) charIndex(1)
//
// ok is false for ANY length but 11. That is deliberately stricter than "at least
// 11": a longer frame from another build would put the index somewhere this
// reader has not verified, and guessing there would silently hand control to a
// character the player is not driving - the exact bug this file exists to fix.
func ParseSelectChangePC(pt []byte) (charIndex int, ok bool) {
	if len(pt) != selectChangePCWidth {
		return 0, false
	}
	return int(pt[10]), true
}

// selectChangePC answers one CZ_SELECTCHANGE_PC: it records which team slot the
// player now drives, one line in the log, nothing on the wire.
//
// The client expects no reply - it has already switched its own camera and input
// before it tells us - so returning no packets is correct, not an omission.
func (h *Handler) selectChangePC(sess *Session, f protocol.Frame) ([][]byte, error) {
	idx, ok := ParseSelectChangePC(f.Data)
	if !ok {
		// THE BYTES ARE LOGGED, the control slot is NOT changed: this is the one
		// frame that decides who the player is, so an unreadable one must leave the
		// old answer standing rather than pick a new one.
		h.Log.Printf("zone: SELECTCHANGE_PC (cmd %d) is %d bytes, not the %d this build "+
			"sends (10-byte header + 1 index byte) - REFUSED, the player still drives "+
			"slot %d; bytes: %s",
			f.Cmd, len(f.Data), selectChangePCWidth, sess.Control, hexHead(f.Data, 32))
		return nil, nil
	}
	if idx < 0 || idx >= len(sess.Team) {
		// Outside the team is the client disagreeing with the roster we spawned - a
		// bad packet or a desynced barrack. Refusing keeps every character's copy
		// where it is; obeying would point the whole "the player's character" set at
		// a slot that has no character, no handle and no position.
		h.Log.Printf("zone: SELECTCHANGE_PC names team slot %d but this session has %d "+
			"character(s) - REFUSED, the player still drives slot %d",
			idx, len(sess.Team), sess.Control)
		return nil, nil
	}
	sess.Control = idx
	// A SWITCH CANCELS A CLICK THAT IS STILL WALKING (the live server, 2026-11-03).
	//
	// The owner clicked an NPC from out of reach, then changed leader while the walk was
	// in flight. The character finished the walk - a 3548 echo at 23.57 s - and NO box
	// ever opened: after the CZ_SELECTCHANGE_PC there is no ZC_DIALOG_SELECT (3663)
	// anywhere in captures\leaderdlg\20261103-200921. That is what `cancelWalkDialog`
	// reproduces: the dialog REQUEST is dropped, the walk and its stop are not.
	//
	// It runs for ANY caster slot, because a click walks whichever character the player
	// drove when the click was made. An attack's or a cast's approach is deliberately
	// left alone: their answer is a hit, not a box.
	h.cancelWalkDialog(sess)
	// THE SESSION'S SEGMENT BELONGS TO THE CHARACTER IT WAS OPENED FOR (R109).
	//
	// The driven character reads ONE record for the whole session - moveFrom / moveRoute /
	// Pos / moveAt (move.go, clientLegOf) - and that record was left pointing at the leg
	// the PREVIOUS character was still walking. The newly driven character then read
	// that leg as its own: its swing position was interpolated along it and the client
	// hard-set the actor on the result, which is the teleport. Measured 2026-11-01
	// 23:31:53, right after switching to the warlock:
	//   `SWING POS char 2 (1793) pos (2119, -797) from segment (2560, -1207) ->
	//    (2119, -797) elapsed 1754ms`
	// and (2560, -1207) was char 1's position five seconds earlier.
	//
	// So the switch CLOSES the old leg and re-seats the record on the new character's
	// OWN last point; the next CZ_MOVE_PC then opens a leg from the right place.
	sess.closeLegForControlSwitch(idx)
	h.Log.Printf("zone: CONTROL slot %d - the player now drives this character (cmd %d)",
		idx, f.Cmd)
	return nil, nil
}

// resetControl puts the session back to "the player drives slot 0", which is the
// state before any CZ_SELECTCHANGE_PC arrives.
//
// It is called where the team is (re)read - CZ_START_GAME - so a fresh entry never
// inherits the previous character's slot, and where the session leaves the zone.
// A session that never says otherwise keeps slot 0, so the behaviour before this
// lane existed is unchanged.
func (s *Session) resetControl() {
	s.Control = 0
}
