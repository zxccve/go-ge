package zone

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// WHO ELSE IS IN THE ZONE.
//
// The Python zone kept a LIST of the players in the zone (src\zone_server.py:964-985
// ZONE_MEMBERS with add_member/remove_member/members, :1046-1068 announce_players)
// and every client learnt about every other one through ZC_SPAWN_STANDINGPC. The
// Go zone had none of that: each session answered only its own frames, so two
// players stood in the same map and never saw each other.
//
// This file is that missing half: the roster, the three packets that announce a
// player (spawn, stop, leave), and the fan-out that writes to OTHER sessions'
// connections. Movement echo itself stays in move.go.

// errNoSessionConn is what a fan-out write answers for a session whose socket is
// already gone. It is an ordinary error on purpose: one dead player must never
// stop the packets going to the others.
var errNoSessionConn = errors.New("session has no connection")

// Players is the zone's live roster, keyed by the session that owns a player.
//
// The ZERO VALUE is ready to use: a Handler built in a test without touching this
// field starts with an empty roster, and no caller has to nil-check it.
type Players struct {
	mu    sync.RWMutex
	byKey map[*Session]*Member
	// order is the JOIN order, which is the order the reference announced in
	// (zone_server.py:1049 iterates its list). A map alone would announce in a
	// different order on every join, and the order is visible: it decides which
	// spawn a client draws first.
	order []*Member
}

// Member is one session in the zone: its WHOLE TEAM, in team order, with each
// character's own position.
//
// A team is not one character. The client draws every member of a party, and a
// follower whose handle was never announced cannot be moved afterwards - the far
// client was being sent moves for handles it had never been introduced to, which
// is why only the leader was ever visible.
type Member struct {
	Sess  *Session
	Chars []Character
	// Pos is parallel to Chars: where each character stands. The leader's entry is
	// the session's own Pos (the arrival point and every leader move report), while
	// a follower's follows that follower's CZ_MOVE_PC.
	Pos [][3]int32
}

// Add puts a session in the roster and returns its record (Python add_member,
// zone_server.py:968-972), with every member of its team.
//
// A session that is already listed is REPLACED where it stands, not appended
// again: a client that sends GAME_READY twice must not appear twice on the other
// players' screens.
func (p *Players) Add(sess *Session) *Member {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byKey == nil {
		p.byKey = make(map[*Session]*Member)
	}
	chars, pos := copiesOf(sess)
	if old, ok := p.byKey[sess]; ok {
		old.Chars, old.Pos = chars, pos
		return old
	}
	m := &Member{Sess: sess, Chars: chars, Pos: pos}
	p.byKey[sess] = m
	p.order = append(p.order, m)
	return m
}

// copiesOf is the session's team and where each character stands, COPIED.
//
// The copy matters: the roster outlives the call, and a later move report writes
// the stored position without touching the session's own state.
func copiesOf(sess *Session) ([]Character, [][3]int32) {
	chars := make([]Character, len(sess.Team))
	pos := make([][3]int32, len(sess.Team))
	for i, c := range sess.Team {
		chars[i] = c
		pos[i] = c.Position
	}
	if len(pos) > 0 {
		// The leader's own position is the session's: it is where the client actually
		// put them, spawn override included, not the value the database held.
		pos[0] = sess.Pos
	}
	return chars, pos
}

// Remove takes a session out and answers its record, or nil when it was never in
// the roster (Python remove_member, zone_server.py:975-980). The join order closes
// up, so a later joiner takes the freed place in the list.
func (p *Players) Remove(sess *Session) *Member {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.byKey[sess]
	if !ok {
		return nil
	}
	delete(p.byKey, sess)
	for i, other := range p.order {
		if other == m {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	return m
}

// Snapshot is everyone in the zone, in JOIN ORDER.
//
// It answers COPIES so a fan-out can write to sockets after the lock is released:
// holding the roster while blocking on one player's socket would stall every other
// join and leave behind it.
func (p *Players) Snapshot() []Member {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Member, 0, len(p.order))
	for _, m := range p.order {
		out = append(out, *m)
	}
	return out
}

// SetPos remembers where ONE character of a session now stands: index is its
// position in the session's team, which is what a CZ_MOVE_PC's charIndex means.
//
// The leader's entry and the session's own Pos are kept in step by movePC; this is
// how a FOLLOWER's position reaches the roster, so the next newcomer is spawned
// where that follower actually is.
func (p *Players) SetPos(sess *Session, index int, pos [3]int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.byKey[sess]
	if !ok || index < 0 || index >= len(m.Pos) {
		return
	}
	m.Pos[index] = pos
}

// SetEquip remembers the worn set of ONE character of a session, so a player who
// joins LATER is spawned in the armour that character is wearing NOW.
//
// The roster keeps COPIES of each team (copiesOf), made at join time. An equip
// changes the session's own copy; without this the roster would still describe the
// set worn at zone entry, and the next joiner would see the old body.
func (p *Players) SetEquip(sess *Session, index int, equip [equipSpotLast]uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.byKey[sess]
	if !ok || index < 0 || index >= len(m.Chars) {
		return
	}
	m.Chars[index].Equip = equip
}

// Len is how many players the zone holds.
func (p *Players) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.order)
}

// send writes ONE packet to another session's connection.
//
// It ENQUEUES and returns. The packet is written by that session's own writer
// goroutine (zone\writer.go), so a fan-out from THIS player's goroutine costs one
// channel send per recipient instead of one Conn.Write per recipient - which is the
// measured wait this change removes (49 sequential writes per move report at 50
// players). Order per recipient is kept by the writer's single queue: see the three
// rules at the top of zone\writer.go.
func (s *Session) send(pkt []byte) error {
	return s.enqueue([][]byte{pkt})
}

// writeSession hands this session's OWN packets to its writer as ONE batch.
//
// ONE BATCH, not one packet at a time, and that is the ordering guarantee: before
// the writer existed this held wmu for the whole run, so a fan-out packet from
// another goroutine could not land between two of them. Enqueuing the run as one
// item keeps exactly that, and the writer still concatenates it into one write.
func (h *Handler) writeSession(sess *Session, packets [][]byte) error {
	if sess == nil || sess.Conn == nil {
		return errNoSessionConn
	}
	if len(packets) == 0 {
		return nil
	}
	return sess.enqueue(packets)
}

// Command ids and layout of the three packets that announce a player. Measured
// from the client by the Python port (src\zone_server.py:254-282, :874-878,
// :1519-1529); the layouts are repeated in each builder below.
const (
	CmdZCSpawnStandingPC = 3520 // server -> client: another player appears
	CmdZCLeavePC         = 3525 // server -> client: another player is gone
	CmdZCStopPC          = 3549 // server -> client: another player stopped walking

	// CmdZCChangeStance is ZC_CHANGE_STANCE: "this character now stands in stance N".
	// The original sends it as its OWN packet (CPlayerChar::SendCurStance,
	// ZServerLib\PlayerChar.cpp:1033-1040) and it is the only packet whose handler
	// necessarily reaches the idle animation: CNormalNet::ChangeStance
	// (geClient\Source\GENormalNet_Action.cpp:969) -> CNormalObject::SetStance
	// (GENormalObject.cpp:2453) -> CBaseObject::SetStance (GeBaseObject.cpp:736) ->
	// CPC::SetStance (geclientlib\geScene\PC.cpp:81) -> PlayASTDAnim (PC.cpp:1219),
	// which plays the "ASTD" motion named by that stance's own row.
	CmdZCChangeStance = 3554
	// ChangeStanceSize is the registered size (data\cmd_table.txt: "3554  10").
	ChangeStanceSize = 10

	// SpawnStandingPCSize is the FIXED part of the spawn: 705 bytes, no option
	// records. SpawnStandingPCSize + EquipOptionSize*optionCount is the WHOLE
	// packet, and the length word at +2 carries the whole one.
	SpawnStandingPCSize = 705
	// EquipOptionSize is one EQUIP_ITEM_OPTION on the wire (type 4 + value 4 +
	// spot 2 + two zero bytes) - see EquipOption.
	EquipOptionSize = 12

	// CmdZCSkillDisable is ZC_SKILL_DISABLE, and SkillDisableSize is its whole
	// length: cmd(2) handle(4) type(1) value(4). See BuildSkillDisable.
	CmdZCSkillDisable = 3572
	SkillDisableSize  = 11

	spHandle        = 4
	spDir           = 8
	spClass         = 9  // WORD: the character's own class id (its job)
	spWeapon        = 11 // WORD: "what is in the hand" - ZERO always, see BuildSpawnStandingPC
	spPosture       = 13 // DWORD: the animation state, see DefaultPosture
	spPos           = 17 // x(4) y(4) cell(4)
	spByte29        = 29 // one byte the reference leaves 0
	spStance        = 30 // DWORD: the active stance
	spHP            = 34 // double
	spMaxHP         = 42 // double
	spBuffMask      = 50 // 256 bytes, and they must stay ZERO
	spAppearance    = 306
	spAppearanceLen = AppearanceLen
	// spAppearanceName is the SECOND name string, inside the appearance block
	// (block offset apNameplate): the one the client draws over the remote player.
	// Empty here means NO NAMEPLATE. It is filled by appearanceBlock, once, so the
	// spawn and the appearance update (3546) carry the same string.
	spAppearanceName    = apNameplate
	spAppearanceNameLen = apNameplateLen
	spOptions           = 701 // DWORD: how many equip-option records follow
)

// DefaultPosture is the posture dword at +13 of the spawn.
//
// IT IS NOT THE ANIMATION SELECTOR, and looking for "which number stands up" here
// was a dead end. The client's own 3520 macro HARDCODES the pose and never reads
// this field: `PCParam.pose = 0;` (geClientShared\include\GePacketUtil.h:153 - the
// macro at :142 is a DIFFERENT packet, ZC_ENTER_STANDINGPC, which does carry a
// posture). The live server sends 0 here in 58 of 58 captured spawns, so 0 is the
// measured value; the old 1 was a guess (and POSTURE_SITTING is what the client's
// enum calls 1, geZCTypes.h:423-424).
//
// WHAT ACTUALLY SELECTS THE IDLE is the STANCE at +30 (see spStance), and it must
// never be 0: the client resolves the standing animation by name out of that
// stance's own row (PC.cpp:503 -> GetPropertyString("Stance", m_stance, "ASTD")).
const DefaultPosture uint32 = 0

// LeavePCReason is the reason word ZC_LEAVE_PC carries, and it is NOT cosmetic:
// the client branches on it.
//
// source2\geClient\Source\GENormalNet_World.cpp:188-193 (CNormalNet::LeavePC):
//
//	if (packet->code == OBJ_OUTOFSIGHT) geactor::Leave(packet->handle, true);
//	else                                geactor::Leave(packet->handle, false);
//
// and geactor::Leave hands that bool to CWorld::LeaveWorld
// (source2\geClientShared\source\GeWorld.cpp:319-338), where true removes the model
// at once - OnExitScene(0.0f, true) - and false runs OnExitScene(fadeoutTime,
// false) with the DEFAULT fadeoutTime of 2.0 s. So the reason selects
// "pop out" versus "fade out over two seconds".
//
// The enum (source\geShared\include\geZCPacket.h) is
// OBJ_LEAVE_LOGOUT=0, OBJ_DEAD=1, OBJ_TELEPORT=2, OBJ_OUTOFSIGHT=3, and the
// reference SERVER sends OBJ_LEAVE_LOGOUT when a character has left the world
// (source2\geServer\ZServerLib\View.cpp:195, the IsZombie path) and OBJ_OUTOFSIGHT
// only for range culling (same file, :221). We keep no range culling, so every
// leave we send is a real leave and must be 0.
//
// THIS USED TO BE 3, and the owner saw the result on 2026-11-05: a character that
// crossed a gate VANISHED instantly for the player left behind, because 3 is the
// culling code and takes the instant branch. Python had sent 3 since
// src\zone_server.py:293 with the note that the client "probably ignores" the word;
// it does not.
const LeavePCReason uint16 = 0

// BuildLeavePC builds ZC_LEAVE_PC - another player left the zone - in 8 bytes:
//
//	cmd(2) handle(4) reason(2)
//
// The client's handler removes the actor, which is the only way a character stops
// standing in the world after its connection is gone.
func BuildLeavePC(handle uint32, reason uint16) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint16(out[0:], CmdZCLeavePC)
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint16(out[6:], reason)
	return out
}

// BuildChangeStance builds ZC_CHANGE_STANCE - "this character stands in stance N" -
// in 10 bytes:
//
//	cmd(2) handle(4) stanceType(4)
//
// There is NO length word: the packet is sized by its command (data\cmd_table.txt:
// "3554   10    PKS_ZC_CHANGE_STANCE"), so bytes 2..5 are the whole handle.
//
// WHY THE OBSERVER GETS IT EVEN THOUGH THE SPAWN ALREADY CARRIES IT. The shipped
// client's 3520 handler DOES read the stance at +30 and DOES apply it: measured in
// ge.exe (FUN_004f3fbb at 0x4f402e reads [EBX+0x1e], stores it, then calls the
// actor's stance slot +0x24 with sound=0 - the SAME slot this packet calls with
// sound=1, 0x4e2984). So the spawn value is not ignored, and this packet is not a
// repair of a missing mechanism.
//
// It is what the OFFICIAL server does on top of the spawn, and that is why we send
// it: its own capture (data\captures\live-20261021\s2c_7054_*.bin) carries a 3554 per
// character with the SAME stance id its 3520 carried - handle 0x2037/30250,
// 0x2038/20660, 0x2039/40915 - each followed by ZC_UPDATE_MHP (3803) with the same
// hp/maxhp doubles. That pair is CPlayerChar::SendCurStance()
// (ZServerLib\PlayerChar.cpp:1033-1040): BroadcastPacket(PACKET_ZC_CHANGE_STANCE)
// then SendAllStatus(). A remote player's stance is therefore set TWICE by the real
// server - once silently by the spawn, once audibly by this packet - and a rebuild
// that sets it once is not doing what the client's own server does.
//
// The client chain for this packet: CNormalNet::ChangeStance
// (geClient\Source\GENormalNet_Action.cpp:969) -> CNormalObject::SetStance(stanceType,
// true) (GENormalObject.cpp:2453) -> CBaseObject::SetStance (GeBaseObject.cpp:736) ->
// CPC::SetStance (geclientlib\geScene\PC.cpp:81) -> PlayASTDAnim (PC.cpp:1219), which
// plays the motion named by that stance's own ASTD row.
//
// Sent once per character, right AFTER the 3520 that introduced it: a stance can only
// be applied to an actor the client already has.
func BuildChangeStance(handle uint32, stance uint32) []byte {
	out := make([]byte, ChangeStanceSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCChangeStance)
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint32(out[6:], stance)
	return out
}

// BuildSkillDisable builds ZC_SKILL_DISABLE - "this character's skill state is
// (re)set" - in SkillDisableSize (11) bytes:
//
//	+0 cmd(2) = 3572   +2 handle(4)   +6 type(1)   +7 value(4)
//
// The struct is PKS_ZC_SKILL_DISABLE { OBJHANDLE handle; BYTE type; DWORD value; }
// and it is PACKED - there is no padding between the type byte and the value, so
// the value starts at +7 and the packet is 11 bytes, not 12 or 14. Reading the
// value at +8 is the trap here: the last five bytes of the live packet are
// `00 00 00 00 00`, so a one-byte shift parses as 0 either way and only a
// non-zero value would ever show it.
//
// THE LIVE CAPTURE'S OWN BYTES (2026-10-28), one per spawn: f4 0d | 34 6f 01 00 |
// 00 00 00 00 00 - i.e. cmd 3572, handle 0x00016f34, type 0, value 0. Every 3520
// spawn was followed by exactly this (3 of 3), and every one of the 36 3521
// player-enter packets likewise, always with the SAME handle as the spawn before
// it.
//
// THE ORDER IS NOT OPTIONAL, and not merely tidy: the client's handler
// CNormalNet::SkillDisable (source2\geClient\Source\GENormalNet_Action.cpp:1242)
// looks the object up with NormalWorld()->GetNormalObject(packet->handle) and calls
// a method on the result with NO NULL CHECK. A 3572 for a handle the client has not
// been introduced to is therefore not "ignored" - it is a null dereference. It may
// only ever follow the spawn that created that actor.
func BuildSkillDisable(handle uint32) []byte {
	out := make([]byte, SkillDisableSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCSkillDisable)
	binary.LittleEndian.PutUint32(out[2:], handle)
	out[6] = 0 // type
	binary.LittleEndian.PutUint32(out[7:], 0)
	return out
}

// diagHexCap is the most hex characters one DIAGHEX field may hold - 4000, which is
// twice the largest packet this zone announces (a 3520 spawn with 26 equip options,
// 1017 bytes = 2034 characters; the fixed-only spawn is 705 = 1410) - and well clear
// of it even at the live 1101-byte, 33-option spawn.
// The headroom is deliberate: the cap is not there to trim today's
// packets, it is there so a FUTURE, bigger packet cannot flood a log a human is meant
// to read line by line. Past it the field is cut and says so, because a silently short
// hex dump is worse than no dump - it would diff as "a shorter packet".
const diagHexCap = 4000

// diagHexBytes is the `hex=` field of a DIAGHEX line: the whole packet, lowercase, no
// separators, cut at diagHexCap characters with a note naming how much was kept.
//
// The note lives INSIDE the same field (spaces and all) so the line's key=value shape
// does not change: a reader that splits on spaces can still find `hex=`, and a human
// sees immediately that this line is incomplete.
func diagHexBytes(b []byte) string {
	s := fmt.Sprintf("%x", b)
	if len(s) <= diagHexCap {
		return s
	}
	return s[:diagHexCap] +
		fmt.Sprintf("...truncated after %d of %d bytes", diagHexCap/2, len(b))
}

// announceChars is ONE player's characters as the other client has to receive them:
// the spawn that creates each actor, the skill-disable that follows it, then the
// stance that gives it an idle.
//
// THE ORDER IS SPAWN, 3572, THEN (3554). The live server sends, per character, the
// 3520 and then exactly one ZC_SKILL_DISABLE (3572) naming the SAME handle - 3 of 3
// spawns in the 2026-10-28 capture, and the same for all 36 3521 player-enter
// packets. Our 3554 after that is an ADDITION, not a port: the capture has NO 3554
// after a spawn at all (its three 3554s sit inside one combat burst). It is kept
// because it is the only packet whose client path starts the stance idle
// (updateMotion = 1 in CPC::SetStance), and it is safe once the actor exists.
//
// THE PACKETS ARE BUILT TOGETHER so no caller can send a spawn without the packets
// that belong to it - a stance omission is exactly the T-pose. It also RECORDS every
// 3554 it built on the owning session (markStancePushed), so the stop path knows
// which characters have never had one - see pushOwedStance.
//
// THE HANDLES THE 3572 AND THE 3554 NAME ARE READ BACK OUT OF THE SPAWN BYTES, not
// taken from c.Handle a second time. All three packets must point at the same actor,
// and the bytes the client receives are the only authority on which handle it will
// key that actor by: a spawn built from one value and a follower packet from another
// is silent (it lands on nothing) and looks exactly like "the client ignores the
// packet". That is worse than silent for the 3572 - its handler dereferences the
// looked-up object with NO NULL CHECK (BuildSkillDisable), so a 3572 for an actor the
// client does not have is a crash, not a dropped packet. The one line per character,
// in BUILD order (= send order), is what makes any mismatch visible.
//
// Handler.DiagHex adds THREE more lines per character - this spawn's bytes, this
// 3572's bytes and this stance's bytes - for the standing case, where nothing else can
// be measured. They are one line per PACKET on purpose: two characters' spawns then
// sit on their own lines and diff cleanly.
func (h *Handler) announceChars(owner *Session, chars []Character, pos [][3]int32) [][]byte {
	out := make([][]byte, 0, 3*len(chars))
	for i, c := range chars {
		spawn := BuildSpawnStandingPC(c.Handle, c, pos[i], DefaultPosture)
		handle := binary.LittleEndian.Uint32(spawn[spHandle:])
		disable := BuildSkillDisable(handle)
		if got := binary.LittleEndian.Uint32(disable[2:]); got != handle {
			h.Log.Printf("zone: WIRING BUG: the spawn of slot %d names handle 0x%08x "+
				"but its ZC_SKILL_DISABLE names 0x%08x - the client looks that "+
				"handle up with no null check and would dereference nothing", i, handle, got)
		}
		var stance []byte
		if c.Stance != 0 {
			stance = BuildChangeStance(handle, c.Stance)
			if got := binary.LittleEndian.Uint32(stance[2:]); got != handle {
				h.Log.Printf("zone: WIRING BUG: the spawn of slot %d names handle 0x%08x "+
					"but its ZC_CHANGE_STANCE names 0x%08x - the stance will be applied to "+
					"an actor the client does not have", i, handle, got)
			}
			owner.markStancePushed(handle)
		}
		h.Log.Printf("zone: announced handle 0x%08x (slot %d of team %d) -> spawn %d bytes, "+
			"skilldisable %d bytes, stance %d bytes",
			handle, i, len(chars), len(spawn), len(disable), len(stance))
		if h.DiagHex {
			// No "zone: " prefix: these lines are meant to be GREPPED (DIAGHEX) and
			// diffed, so the marker is the first thing on the line.
			h.Log.Printf("DIAGHEX spawn handle=0x%08x slot=%d team=%d cmd=3520 bytes=%d hex=%s",
				handle, i, len(chars), len(spawn), diagHexBytes(spawn))
			h.Log.Printf("DIAGHEX skilldisable handle=0x%08x slot=%d team=%d cmd=3572 bytes=%d hex=%s",
				handle, i, len(chars), len(disable), diagHexBytes(disable))
			if stance != nil {
				h.Log.Printf("DIAGHEX stance handle=0x%08x slot=%d team=%d cmd=3554 stance=%d bytes=%d hex=%s",
					handle, i, len(chars), c.Stance, len(stance), diagHexBytes(stance))
			} else {
				h.Log.Printf("DIAGHEX stance handle=0x%08x slot=%d team=%d cmd=3554 none=1 (no stance: no 3554 is sent)",
					handle, i, len(chars))
			}
		}
		out = append(out, spawn)
		out = append(out, disable)
		if stance != nil {
			out = append(out, stance)
		}
	}
	return out
}

// BuildStopPC builds ZC_STOP_PC - another player's walk is over - in 22 bytes:
//
//	cmd(2) handle(4) tick(4) x(4) y(4) cell(4)
//
// THIS PACKET IS NOT OPTIONAL. A remote actor's animation is driven entirely by
// ZC_MOVE_PC, and the client only stops it when this arrives: its handler does
// `Stop(); SetPosition(pos)`. Without it the other player animates (runs) forever
// and never arrives (Python zone_server.py:1071-1102 stop_others_if_walk_over).
//
// The tick is this machine's clock. A caller that sends SEVERAL stops as one event -
// a warp is exactly that - must use BuildStopPCAt with one tick for all of them: the
// real server stamps one tick across the whole warp (capture 2026-10-27), and the
// client uses it to order stops for the same team.
func BuildStopPC(handle uint32, pos [3]int32) []byte {
	return BuildStopPCAt(handle, pos, uint32(nowMillis()))
}

// BuildStopPCAt is BuildStopPC with the tick supplied by the caller, so a group of
// packets that belong to ONE event can share it.
func BuildStopPCAt(handle uint32, pos [3]int32, tick uint32) []byte {
	out := make([]byte, 22)
	binary.LittleEndian.PutUint16(out[0:], CmdZCStopPC)
	binary.LittleEndian.PutUint32(out[2:], handle)
	binary.LittleEndian.PutUint32(out[6:], tick)
	binary.LittleEndian.PutUint32(out[10:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(out[14:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(out[18:], uint32(pos[2]))
	return out
}

// stopPCForOthers returns the ZC_STOP_PC bytes to put on the wire for the OTHER
// players, with Handler.DiagStopOffset applied. It COPIES, never edits in place:
// a warp hands the same slice to its own reply, and the mover's own team must not
// jump. Only the x DWORD (+10) moves - y (+14) and the navmesh CELL (+18) are
// left alone on purpose: a cell is a walk-mesh triangle index, so a shifted cell
// puts the client on an invalid triangle and crashes it in PathEngine.DLL, which
// would look like "the packet is broken" instead of answering the question.
//
// With the switch at its normal 0 it returns pkt ITSELF, unchanged: same slice,
// same bytes, so a normal run is not merely equivalent to the old one - it is the
// same buffer.
//
// WHAT AN ANSWER MEANS. The far model JUMPS SIDEWAYS: the client found that handle,
// so the packet is fine and the fault is the animation. It does NOT move: the
// handler never found the handle, so the handle value is the problem.
func (h *Handler) stopPCForOthers(handle uint32, pkt []byte) []byte {
	if h.DiagStopOffset == 0 || len(pkt) != 22 ||
		binary.LittleEndian.Uint16(pkt[0:]) != CmdZCStopPC {
		return pkt // off, or not the packet this switch is about
	}
	out := append([]byte(nil), pkt...)
	oldX := int32(binary.LittleEndian.Uint32(out[10:]))
	newX := oldX + h.DiagStopOffset
	binary.LittleEndian.PutUint32(out[10:], uint32(newX))
	h.Log.Printf("zone: DIAG -diag-stop-offset=%d: ZC_STOP_PC handle 0x%08x x %d -> %d (y %d and cell %d unchanged)",
		h.DiagStopOffset, handle, oldX, newX,
		int32(binary.LittleEndian.Uint32(out[14:])), int32(binary.LittleEndian.Uint32(out[18:])))
	return out
}

// The two weapon hands, as appearance slots. The numbering is the client's own:
// spot 4 is the LEFT hand and spot 5 the RIGHT one, which is why EQUIP_NONE[4] and
// EQUIP_NONE[5] are the same class id (10000, the bare hand). The same pair is what
// the Python oracle calls EQUIP_WEAPON_SPOTS (src\zone_server.py:638-642), and the
// stored rows agree: character 40 wears 7215 = CrecementoShield at 4 and
// 7201 = CrecementoSword at 5.
var equipWeaponSpots = [2]int{5, 4}

// weaponClassOf is the class id of the item this character holds in a hand, or 0
// when both hands are empty. IT IS A LOG AND REPORT VALUE ONLY - nothing on the
// wire is built from it any more.
//
// Right hand first. The left hand is where a SHIELD goes and a shield is not what
// the character attacks with, and the live capture's own Fighter says the same:
// shield 7215 in slot 4, sword 7201 in slot 5 (captures\pcref2\20261028-232622).
// The Python oracle walked the pair left-first (src\zone_server.py:1024-1029), but
// that order was a guard against picking up a HEAD item, not a measurement.
//
// WHY IT NO LONGER FEEDS +11. It used to be written into the spawn's WORD at +11,
// which broke armed characters in game - see BuildSpawnStandingPC. The hands are
// drawn from the 21-slot appearance block (EquipNone/Character.Equip), which is
// where the client reads them, so this function now answers one question only:
// "which item would we say is held", for the zone's equip log line.
func weaponClassOf(c Character) uint32 {
	for _, spot := range equipWeaponSpots {
		if cls := c.Equip[spot]; cls != 0 {
			return cls
		}
	}
	return 0
}

// BuildSpawnStandingPC builds ZC_SPAWN_STANDINGPC - tell one client about another
// player. The FIXED part is SpawnStandingPCSize (705) bytes; after it come
// c.EquipOptions as 12-byte EQUIP_ITEM_OPTION records, so the packet is
// 705 + 12*optionCount bytes long:
//
//	+0   cmd(2) = 3520
//	+2   len(2) = the WHOLE packet length, option records included
//	+4   handle(4) = the leader's character handle
//	+8   direction(1) = 0
//	+9   classID(2) = the character's job/class id
//	+11  weaponClassID(2) = ZERO, ALWAYS. See "THE WORD AT +11" below.
//	     Both of these are WORDs in the SHARED spawn base struct - the pet spawn
//	     handler FUN_004f414d reads the same pair two bytes earlier, because a pet
//	     has no hands. So +11 is one "what it is holding" id, NOT a hand slot.
//	+13  posture(4) = not read by the client for this packet; 0, the live value (see
//	     DefaultPosture). The idle is chosen by the stance at +30.
//	+17  x(4) +21 y(4) +25 cell(4)
//	+29  one byte, 0
//	+30  stance(4) = the active stance, from the game's own stance tables - NEVER 0:
//	     it is the row the client reads the standing animation out of.
//	+34  hp(double) +42 maxhp(double)
//	+50  256 bytes of BUFF MASK - ALL ZERO. Any other value makes the client draw
//	     the other player standing in a glowing buff circle.
//	+306 395 bytes of APPEARANCE_PC, copied from the START_INFO record (see below)
//	+701 optionCount(4)
//	+705 { type(4) value(4) spot(2) pad(2) } * optionCount
//
// THE WORD AT +11 IS SENT AS ZERO, ALWAYS - AND THAT IS A MEASURED VALUE, NOT A
// DEFAULT. The live server (captures\pcref2\20261028-232622\s2c_7054_62474.bin) sends
// 0 at +11 in all SIX of its spawns, and in all 30+ spawns of the earlier live
// captures - INCLUDING characters that are clearly armed in the same packet: a
// Fighter with shield 7215 in appearance slot 4 and sword 7201 in slot 5, an
// Elementalist with 7222 in slot 5. The weapon the model shows comes from the
// 21-slot appearance block; this WORD is something else ("what is in the hand",
// stored once on the actor at actor+0x55c by FUN_004f9546 - see weaponClassOf).
//
// WHY THAT MATTERS IN GAME (owner report 2026-10-29, in a TOWN): a remote character
// carrying a weapon T-posed and kept its walk animation after stopping, while
// bare-handed characters looked right; removing every item fixed it. We used to put
// the weapon's class id HERE, so exactly the armed characters got a non-zero value
// the live server never sends - one variable, and it is the prime suspect. A weapon
// in hand puts the client on the battle-ready path; in a town (combat disabled)
// there is no battle stance to hold, so what is left is the bind pose.
//
// ONE VARIABLE AT A TIME: this change is +11 ONLY. The 21-slot array was re-measured
// against the same capture and is ALREADY in the right place (appearance +68 =
// packet +374, 12-byte slots) - see TestSpawnMatchesLiveCapture. It is not touched.
//
// +9 still carries the job, where the live server sends 0. Nothing here has shown
// the client needs it, and nothing has shown it is harmless: it is the NEXT
// candidate, kept as it is on purpose so the next in-game test has one variable in
// it, not two.
//
// The field is a WORD - measured in the client, not assumed: FUN_004f3fbb reads it
// with `MOV AX, word ptr [EBX + 0xb]` (0x4f4088), and the class id beside it at +9
// with `MOV AX, [EBX + 0x9]` (0x4f407d).
//
// THE TAIL IS THE OPTION RECORDS, and that is what makes the live packet bigger
// than ours. Three sources agree:
//   - the struct: PKS_ZC_SPAWN_STANDINGPC ends with `int optionCount; // EQUIP_ITEM_OPTION *
//     optionCount;` (source2\geShared\include\geZCPacket.h), and EQUIP_ITEM_OPTION is
//     { DWORD type; DWORD value; WORD spot; WORD pad; } (geShared\include\geItemTypes.h);
//   - the official builder: CPlayerChar::SendSpawned writes optionCount from
//     GetEquipItemOption and then sets the length to lastPos - curPos, i.e. the WHOLE
//     packet (source2\geServer\ZServerLib\PlayerChar.cpp:432-446), and
//     CEquipSlot::GetItemOptions emits one per option, walking the spots in ascending
//     order (EquipSlot.cpp:462-476);
//   - the capture: the live server's spawn is 1017 bytes with 26 options and 1101
//     bytes with 33 (705 + 26*12 and 705 + 33*12), and the spots it carries decode to
//     real slots (1 BODY, 3 NECK, 5 RHAND, 8 GLOVE, 9 BOOTS, 12 EAR, 13 BELT).
//
// The extra bytes are THESE records - not a stance list and not a skill list.
//
// THE APPARANCE BLOCK IS COPIED, NEVER HAND-BUILT: it is the same [21:416] slice of
// BuildStartInfo, so the remote model wears what the player wears and gets the
// EquipNone fallback that stops the client dereferencing a null item property on
// class id 0. The NAMEPLATE is a SECOND string inside it, at +341 (17 bytes), and it
// is filled here - left empty there is no name over the other player.
//
// HARD DISCIPLINE: the buffer IS the packet, and the length word is written LAST
// from the buffer's own size, so the two cannot disagree. A length word that claims
// more (or fewer) bytes than follow makes the client read the rest of the stream as
// option records and the connection never re-aligns - which looks exactly like "the
// client is broken". This project has been burned twice by a lying length word.
//
// WHAT IS DELIBERATELY MISSING: the per-slot reinforcement ("+N") bytes, which this
// packet does not carry at all. EquipOptions is empty in production today because the
// item layer has no option rows (see Character.EquipOptions), so the packet stays
// 705 bytes there - which the client accepts, since a ZERO count promises no records.
func BuildSpawnStandingPC(handle uint32, c Character, pos [3]int32, posture uint32) []byte {
	b := make([]byte, SpawnStandingPCSize+EquipOptionSize*len(c.EquipOptions))
	binary.LittleEndian.PutUint16(b[0:], CmdZCSpawnStandingPC)

	binary.LittleEndian.PutUint32(b[spHandle:], handle)
	b[spDir] = 0
	binary.LittleEndian.PutUint16(b[spClass:], uint16(c.Job))
	// +11 stays ZERO - not "for now", but as the measured live value for a real PC,
	// armed or not. It used to carry the weapon's class id, and that is the change
	// that made armed remote characters T-pose in a town: see "THE WORD AT +11" in
	// the doc comment above. The weapon is still on the model - the appearance block
	// below carries both hands as full DWORDs (Character.Equip -> EquipNone).
	binary.LittleEndian.PutUint32(b[spPosture:], posture)
	binary.LittleEndian.PutUint32(b[spPos:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(b[spPos+4:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(b[spPos+8:], uint32(pos[2]))
	b[spByte29] = 0
	// The ACTIVE stance, from Character.Stance - read out of the game's own
	// datatable_stancecondition.xml by StanceTable.PickStance, which matches the
	// condition to what this character is HOLDING. A remote client with 0 here has no
	// stance for the actor and falls back to the run animation, which is exactly the
	// bug this field fixes. 0 is sent only when no stance resolves.
	binary.LittleEndian.PutUint32(b[spStance:], c.Stance)
	putFloat64(b[spHP:], c.HP)
	putFloat64(b[spMaxHP:], c.MaxHP)
	// b[spBuffMask:spAppearance] stays zero. Not scored: see the doc comment.

	// The appearance: the SAME bytes START_INFO carries, so the remote player looks
	// exactly like they do on their own screen. appearanceBlock also writes the
	// nameplate, so this packet and the appearance update (3546) cannot drift.
	copy(b[spAppearance:spAppearance+spAppearanceLen], appearanceBlock(c))

	// The equip options, one 12-byte EQUIP_ITEM_OPTION per option on a worn item.
	// They are SORTED BY SPOT into a STABLE copy: the official server walks the
	// spots in ascending order (CEquipSlot::GetItemOptions), so a caller that fills
	// EquipOptions in any other order still puts the records on the wire in the
	// order the live packet has. Sorting the caller's own slice would be a side
	// effect on another owner's data, so it is copied first.
	opts := make([]EquipOption, len(c.EquipOptions))
	copy(opts, c.EquipOptions)
	sort.SliceStable(opts, func(i, j int) bool { return opts[i].Spot < opts[j].Spot })
	// The count is what the records below actually are, never a second opinion
	// about how many there should have been.
	binary.LittleEndian.PutUint32(b[spOptions:], uint32(len(opts)))
	for i, o := range opts {
		rec := b[SpawnStandingPCSize+i*EquipOptionSize:]
		binary.LittleEndian.PutUint32(rec[0:], o.Type)
		binary.LittleEndian.PutUint32(rec[4:], o.Value)
		binary.LittleEndian.PutUint16(rec[8:], o.Spot)
		rec[10], rec[11] = 0, 0 // EQUIP_ITEM_OPTION's WORD pad, written not implied
	}

	// LAST, from the buffer itself: this is the field that must never disagree with
	// the bytes.
	binary.LittleEndian.PutUint16(b[2:], uint16(len(b)))
	return b
}

// joinZone adds this session to the roster and tells THE CIRCLE ONLY about THE
// CIRCLE's contents: the newcomer hears about the players and monsters inside ITS
// map's ViewRange, and only the players whose own circle holds the newcomer hear
// about it (zone\sight.go, View.cpp:139).
//
// The radius is the MAP's, so a field map (ViewRange 6000) announces less than the
// same crowd would in a town (4000) - and a map with no table row announces
// everybody, exactly as every map did before this lane.
//
// The packets for THIS connection are RETURNED - Reply only answers the frame it
// was given - while the ones for the other players are written here, through their
// own write mutex.
func (h *Handler) joinZone(sess *Session) [][]byte {
	if len(sess.Team) == 0 {
		h.Log.Printf("zone: GAME_READY with no team member - this player is not announced")
		return nil
	}
	me := h.Players.Add(sess)
	radius, armed := h.sightRadius()
	// The newcomer's own spawns, one per character, built ONCE: every other player
	// is told the same bytes. Each character goes out as a SPAWN, the SKILL-DISABLE
	// the live server sends after it, and then its STANCE (announceChars): the spawn
	// creates the actor, the other two act on it.
	mine := h.announceChars(me.Sess, me.Chars, me.Pos)

	// THE MONSTERS A SCRIPT MADE, if they stand inside the newcomer's circle. They
	// used to be sent unconditionally from the GAME_READY branch; the ring owns them
	// now, and it records each one as announced so the next tick does not draw it a
	// second time.
	var out [][]byte
	sentMonsters := 0
	made := h.runtimeMonsters()
	for _, m := range made {
		if armed && !inRing(sess.Pos, m.X, m.Y, radius) {
			continue
		}
		out = append(out, BuildEnterStandingMonster(m))
		sightMarkSeen(sess, m.Handle, sightKindMonster)
		metrics.sightEnter()
		sentMonsters++
	}
	if len(made) > 0 {
		h.Log.Printf("zone: %d of %d monster(s) a script created stand inside this "+
			"player's view ring and are sent to him", sentMonsters, len(made))
	}

	for _, m := range h.Players.Snapshot() {
		if m.Sess == sess {
			continue
		}
		// The newcomer sees only the characters of the player already there that
		// stand inside ITS circle...
		for j := range m.Chars {
			if armed && !inRing(sess.Pos, m.Pos[j][0], m.Pos[j][1], radius) {
				continue
			}
			pkts := h.announceChars(m.Sess, m.Chars[j:j+1], m.Pos[j:j+1])
			out = append(out, pkts...)
			for range pkts {
				metrics.sightEnter()
			}
			sightMarkSeen(sess, m.Chars[j].Handle, sightKindPC)
		}
		// ...and that player hears about the newcomer only if the newcomer stands
		// inside THAT PLAYER's circle - measured from the leader, the same centre the
		// ring uses.
		if armed && !inRing(m.Pos[0], sess.Pos[0], sess.Pos[1], radius) {
			continue
		}
		sent := 0
		ok := true
		for _, pkt := range mine {
			if err := m.Sess.send(pkt); err != nil {
				h.Log.Printf("zone: could not announce a character of handle %d to "+
					"handle %d: %v", me.Chars[0].Handle, m.Chars[0].Handle, err)
				ok = false
				break
			}
			sent++
		}
		if ok {
			for _, c := range me.Chars {
				sightMarkSeen(m.Sess, c.Handle, sightKindPC)
			}
			for range mine {
				metrics.sightEnter()
			}
		}
		h.Log.Printf("zone: announced %d character(s) of handle %d to handle %d (%d sent)",
			len(me.Chars), me.Chars[0].Handle, m.Chars[0].Handle, sent)
	}
	h.Log.Printf("zone: %d player(s) in the zone after handle %d joined (%d character "+
		"spawn(s) out, %d monster(s) in his ring)", h.Players.Len(), me.Chars[0].Handle,
		len(out), sentMonsters)
	return out
}

// leaveZone takes this session out of the roster and tells every OTHER player, so
// their client removes the character (Python zone_server.py:4987-4995).
//
// ONE ZC_LEAVE_PC PER CHARACTER: the far client drew every member of the team, so
// leaving only the leader's handle behind would leave the followers standing in the
// world with no connection behind them.
//
// It runs from Serve's defer, so a disconnecting player is announced even when the
// connection broke mid-packet. The REMOVAL comes first: a leave must never be sent
// to the player who is leaving.
func (h *Handler) leaveZone(sess *Session) {
	// THE PLAYER IS LEAVING, so the session goes back to the default: slot 0 drives
	// until the client says otherwise (zone\control.go). FIRST, before any early
	// return below, so it holds for a session that never joined the roster too.
	sess.resetControl()
	// A cast still winding up when the player leaves is dropped, and it must be
	// dropped BEFORE anything else here: its result would be owed to a session that is
	// no longer in this zone (zone\skill_cast_clock.go).
	h.dropCasts(sess, "the connection ended")
	// AND SO IS EVERY RUNNING SWING REPEAT (R94), for the same reason and BEFORE the
	// early return below: a session that never joined the roster still has its own
	// records, and a repeat left behind would be packets built for nobody on the next
	// tick. Nothing else on this path can carry one - the record lives on the Session.
	h.dropSwings(sess, "the connection ended")
	// AND EVERY KEPT PRESS (zone\attack.go, holdPress), for the same reason and in the
	// same place: a press waiting for its delay would fire on the next tick for a session
	// that is no longer in this zone.
	h.dropPresses(sess, "the connection ended")
	m := h.Players.Remove(sess)
	if m == nil {
		return // never joined: nothing to announce
	}
	// A SCRIPT-ORDERED APPROACH ENDS WITH ITS TARGET (zone\monster_order.go,
	// `endApproachesTo`). THIS IS THE ONE MOMENT THE ZONE'S CLOCK CANNOT DO IT: the
	// approach ends on the tick that finds the target gone, and with the LAST session
	// leaving there is no tick left to run - the measured run kept four bodies engaged
	// from 08:19:46 until the player RECONNECTED at 08:22:32 as a new handle
	// (logs\zone_go_script.log). One line per ended approach, written now.
	gone := make([]uint32, 0, len(m.Chars))
	for _, c := range m.Chars {
		gone = append(gone, c.Handle)
	}
	h.endApproachesTo(gone)
	// THE RING IS DROPPED WITH THE SESSION, and every OTHER session is made to forget
	// this team's handles at the same instant - so no ring pass can send a SECOND
	// leave for a handle whose reason-0 leave is going out below.
	//
	// THE DOOR THAT SHUTS: each state is cleared UNDER ITS OWN MUTEX and the handle is
	// marked departed in the same critical section, and sightTick's leave loop reads
	// both under that same mutex. So a tick either runs before (its leave is already
	// queued and the client drops the actor) or after (it sees the handle departed and
	// says nothing). There is no window where both go out.
	dropSight(sess)
	for _, other := range h.Players.Snapshot() {
		st := sightStateOf(other.Sess)
		st.mu.Lock()
		for _, c := range m.Chars {
			delete(st.seen, c.Handle)
			delete(st.gone, c.Handle)
		}
		st.mu.Unlock()
	}
	for _, c := range m.Chars {
		sightDeparted(c.Handle)
	}
	pkts := make([][]byte, 0, len(m.Chars))
	for _, c := range m.Chars {
		pkts = append(pkts, BuildLeavePC(c.Handle, LeavePCReason))
	}
	sent := 0
	for _, other := range h.Players.Snapshot() {
		ok := true
		for _, pkt := range pkts {
			if err := other.Sess.send(pkt); err != nil {
				h.Log.Printf("zone: could not tell handle %d that handle %d left: %v",
					other.Chars[0].Handle, m.Chars[0].Handle, err)
				ok = false
				break
			}
		}
		if ok {
			sent++
		}
	}
	h.Log.Printf("zone: LEAVE_PC %d character(s) of handle %d (reason %d) -> %d player(s), "+
		"%d left", len(m.Chars), m.Chars[0].Handle, LeavePCReason, sent, h.Players.Len())
}

// THE FORWARD-FAILURE LINE IS RATE LIMITED, AND WHY IT HAS TO BE. sendOthers logs
// once per RECIPIENT whose send failed, so ONE dead client in a 300-player town
// writes 300 lines for every packet the zone forwards - the whole log becomes one
// error repeated, and the packet that actually mattered is not in it any more.
//
// The first forwardFailLines lines print IN FULL, with the same wording as before,
// so a short run (and every test that reads this line) sees exactly the log it
// always saw. After that the line is suppressed and counted, and ONE line per
// forwardFailWindow carries the CURRENT error plus how many were held back - so a
// long run still answers "is it still failing, and with what".
//
// The two numbers are package variables, not constants, so a test can shrink the
// window and exercise the second line in milliseconds instead of ten seconds.
var (
	forwardFailLines  = 3                // printed in full, per run
	forwardFailWindow = 10 * time.Second // after that, ONE line per this
)

// forwardFailLog is the limiter's state, on the Handler because the Handler is what
// sendOthers is called on. The mutex is here because sendOthers runs on SEVERAL
// sessions' goroutines at once, and it is taken ONLY on the error path: a forward
// that succeeds must not wait on anything. TWO lines share it, under two fields: the
// forward failure above (Handler.fwdFail) and "this client stopped reading, closing
// it" (Handler.queueFail, zone\writer.go overflow) - both are one-per-recipient at
// crowd scale and both would bury the log without a limit.
type forwardFailLog struct {
	mu         sync.Mutex
	lines      int       // how many printed in full so far, per run
	suppressed int       // held back since the last line that was printed
	last       time.Time // when the last line was printed
}

// logLimited prints ONE rate-limited line: the first forwardFailLines of its kind
// print IN FULL, then one per forwardFailWindow carries the CURRENT error plus how
// many were held back. kind names the line in the caught-up sentence
// ("forward-failure", "queue-overflow") so a reader knows which one this is.
//
// It is shared by the two crowd-scale lines - the forward failure and the session
// closed for not reading - so neither can drift into a different policy, and so
// there is ONE place to read when the question is "why is the log quiet".
func (h *Handler) logLimited(l *forwardFailLog, kind, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lines < forwardFailLines {
		l.lines++
		// The FULL lines are unchanged, format string and all: a run that fails
		// fewer than forwardFailLines times must read as it always did.
		if l.lines == forwardFailLines {
			// The window starts at the last full line, so the next one cannot
			// print until a whole window has passed - otherwise a burst of ten
			// failures would print three full lines and then a fourth one at
			// once, which is the flood this exists to stop.
			l.last = time.Now()
		}
		h.Log.Printf("%s", msg)
		return
	}

	since := time.Since(l.last)
	if since < forwardFailWindow {
		l.suppressed++
		return
	}
	n := l.suppressed
	l.suppressed = 0
	l.last = time.Now()
	h.Log.Printf("%s (%d further %s line(s) suppressed in the %s before this one; "+
		"after the first %d, one is printed per %s per run)",
		msg, n, kind, forwardFailWindow, forwardFailLines, forwardFailWindow)
}

// logForwardFailure writes the "could not forward" line under the limiter above.
// handle is whose packet failed, to is the recipient it failed for, and err is the
// real error - carried verbatim, because "why" is the whole point of the line.
func (h *Handler) logForwardFailure(handle, to uint32, err error) {
	h.logLimited(&h.fwdFail, "forward-failure", fmt.Sprintf(
		"zone: could not forward handle %d's packet to handle %d: %v", handle, to, err))
}

// sendOthers forwards ONE packet about a character to every OTHER session in the
// zone, and answers how many accepted it.
//
// This is THE fan-out for movement: a walk, a stop and a warp all send through here,
// so they cannot drift apart in who they tell or in what a failure says. The action
// family (3564/3566 and their 3557s, zone\attack.go's fanOut) and the stance/equip
// updates go through it too, which is what makes ONE gate enough for "updates".
//
// THE UPDATE CULL LIVES HERE (zone\sight.go, sightCulls): a viewer whose DRIVEN BODY is
// past the map's ViewMiddleRange - and still inside ViewRange - keeps the actor in its
// world and stops being sent that actor's updates. Nothing is left (no 3525), and the
// actor stays announced; when the bodies come back inside the middle range the updates
// resume by themselves. Both points are read at ONE clock per packet, and a case that
// cannot be measured (no band, no middle column, -sightoff, a handle this session does
// not own) culls nobody, so it behaves exactly as it did before this gate existed.
func (h *Handler) sendOthers(sess *Session, handle uint32, pkt []byte) int {
	sent := 0
	view, middle, armed := h.sightRadii()
	cull := armed && middle > 0 && middle < view
	now := time.Now()
	var actor [3]int32
	if cull {
		var ok bool
		if actor, ok = h.cullActorAt(sess, handle, now); !ok {
			cull = false
		}
	}
	for _, m := range h.Players.Snapshot() {
		if m.Sess == sess {
			continue
		}
		// THE FROZEN VIEWER: the actor stays, its updates do not. Counted per
		// (viewer, packet) pair, because that is what the log has to make visible.
		if cull && m.Sess != nil && len(m.Sess.Team) > 0 &&
			sightCulls(h.sightCentre(m.Sess, now), actor[0], actor[1], middle, view) {
			metrics.sightCull()
			continue
		}
		if err := m.Sess.send(pkt); err != nil {
			// A session closed for a full queue has ALREADY said so in its own line,
			// once, and the session is gone; reporting it once per packet here would
			// be the flood the limiter below exists to stop, about a cause already
			// named. So that one error is not a forward failure.
			if errors.Is(err, errQueueFull) {
				continue
			}
			h.logForwardFailure(handle, m.Chars[0].Handle, err)
			continue
		}
		sent++
	}
	// S3 counters: ONE record per broadcast, holding how many sessions took it.
	// This is the number that predicts the crowded-town wall.
	metrics.broadcast(sent)
	return sent
}

// fanOut hands ONE action's finished packets to every OTHER session in the zone and
// answers how many sessions took ALL of them (the smallest count of any one packet).
//
// It is sendOthers for a GROUP of packets, and it exists because of R93: a cast's
// result is a set - the 3573, or the 3581 and one 3557 per victim - and a watcher
// that got half of it would see a hit with no number, or a number with no hit. The
// packets are the actor's own slices, handed over UNCHANGED: nothing here copies,
// rebuilds or adjusts per recipient. §4 of history\combat-attack-heal.md says why
// that is not laziness - 3564 carries no "for whom" field, so the same bytes serve
// the attacker and every watcher, and a per-recipient rebuild is where a hit turns
// into a different hit on somebody else's screen.
//
// A failure is logged once per session by sendOthers, naming the handle; this
// function adds nothing of its own.
func (h *Handler) fanOut(sess *Session, handle uint32, pkts ...[]byte) int {
	sent := -1
	for _, pkt := range pkts {
		n := h.sendOthers(sess, handle, pkt)
		if sent < 0 || n < sent {
			sent = n
		}
	}
	if sent < 0 {
		return 0
	}
	return sent
}

// stopOthers tells every OTHER player that this one stopped walking, at pos.
//
// It is called where a walk ENDS (chase.go): the remote clients need a ZC_STOP_PC
// or the actor animates forever - see BuildStopPC. The walker itself is skipped:
// its own client drove the walk and already stands where it stands.
//
// Only the leader ever walks on this side (clicks and chases are the player's), so
// the stop names Team[0]. A WARP moves the WHOLE team and calls broadcastStop once
// per character itself; it must not come through here.
func (h *Handler) stopOthers(sess *Session, pos [3]int32) {
	if len(sess.Team) == 0 {
		return
	}
	handle := sess.Team[0].Handle
	h.broadcastStop(sess, 0, handle, pos, BuildStopPC(handle, pos))
	// The leader's reported walk ends HERE for the others, so its segment is closed:
	// leaving it open would send a SECOND stop for the same stop on the next tick.
	//
	// NO rememberWalk (R91 round 7): slot 0's live point is s.Pos / moveFrom / moveAt,
	// which movePC wrote, and this stop names the point it was given.
	sess.closeWalk(0)
}

// markStancePushed records that a ZC_CHANGE_STANCE for this handle has been BUILT
// for the other players - see announceChars.
//
// It records the attempt, not a successful write: a socket that refused the packet is
// gone, and a player who is gone is not owed a second one. The point is the ordinary
// case - a character whose 3554 went out at join must not get a second one on its
// first stop.
func (s *Session) markStancePushed(handle uint32) {
	if s == nil {
		return
	}
	s.stanceMu.Lock()
	defer s.stanceMu.Unlock()
	if s.stancePushed == nil {
		s.stancePushed = map[uint32]bool{}
	}
	s.stancePushed[handle] = true
}

// claimStance answers whether a ZC_CHANGE_STANCE is STILL OWED for this handle, and
// marks it as sent in the same step - so two callers cannot both get "owed" and send
// it twice.
func (s *Session) claimStance(handle uint32) bool {
	if s == nil {
		return false
	}
	s.stanceMu.Lock()
	defer s.stanceMu.Unlock()
	if s.stancePushed[handle] {
		return false
	}
	if s.stancePushed == nil {
		s.stancePushed = map[uint32]bool{}
	}
	s.stancePushed[handle] = true
	return true
}

// pushOwedStance sends the ONE ZC_CHANGE_STANCE still owed for this character, or
// nothing when it has already had one.
//
// WHY IT HOOKS ONTO THE STOP, and why it goes out AFTER it. The client refuses a
// stance change while a move animation is playing: CPC::SetStance does
// `if (updateMotion && !IsMoveAnimPlaying()) PlayASTDAnim()`, so a 3554 that lands
// mid-walk stores the stance and starts nothing. The stop (3549) is exactly the
// moment the move ENDS - its handler runs Stop() (CAS_STAND + CMS_STOP) and the
// arrival animation - so the stance push is put on the wire immediately after it.
// Sending it BEFORE the stop would be refused for the same reason the walk existed.
func (h *Handler) pushOwedStance(sess *Session, index int, handle uint32) {
	if index < 0 || index >= len(sess.Team) {
		return
	}
	stance := sess.Team[index].Stance
	if stance == 0 {
		return // nothing to name, and stance 0 is not a stance
	}
	if !sess.claimStance(handle) {
		return
	}
	sent := h.sendOthers(sess, handle, BuildChangeStance(handle, stance))
	if sent == 0 {
		return
	}
	h.Log.Printf("zone: CHANGE_STANCE handle %d (char %d) stance %d sent after its STOP - "+
		"the actor can only take a stance once the move animation has ended "+
		"(-> %d other player(s))", handle, index, stance, sent)
}

// broadcastStop forwards ONE character's ZC_STOP_PC to every other player, and keeps
// that character's stored position current.
//
// It is the counterpart of broadcastMove, and it is needed for the same reason: the
// other clients must see where the character went. A WARP sends it once per character,
// leader LAST, all sharing one tick (see zone\town.go) - that is the whole warp, as the
// live capture shows.
func (h *Handler) broadcastStop(sess *Session, index int, handle uint32, pos [3]int32, pkt []byte) {
	// FIRST, and wire-only: the diagnostic shift (see stopPCForOthers) must not reach
	// the stored position below, and must not reach the mover's own reply - which is
	// the same slice another caller is holding - so it copies and returns early.
	pkt = h.stopPCForOthers(handle, pkt)
	sent := h.sendOthers(sess, handle, pkt)
	h.Players.SetPos(sess, index, pos)
	// The stop ends the move animation, which is the only moment a stance change is
	// ACCEPTED for a character that never had one - see pushOwedStance. It goes out
	// after the stop on purpose.
	h.pushOwedStance(sess, index, handle)
	// THE COUNTER COUNTS THE EVENT; THE LINE IS A DIAGNOSTIC NOW (`-diag-events`).
	// The stop happened whether or not another player was there to hear it, so
	// metrics.stop() is unconditional - while the line below was only ever written
	// for a fan-out that REACHED somebody (sent > 0), and that is still true. The
	// line is off by default because it sits on the per-packet path: at 50 bots the
	// three movement lines are ~100 lines/s, at 300 players in one town ~1000, which
	// buries every other line. OFF, the METRICS line's `stops=` counts the same event,
	// so the words are gone and the information is not. `-diag-events` restores this
	// line byte for byte.
	metrics.stop()
	// THE LINE IS WRITTEN WHEN THE DISTANCE MEASUREMENT NEEDS IT TOO: with
	// -diag-distance on, the stop line is the one place a number survives at the
	// moment an actor leaves the screen (zone\diag_distance.go), so the flag brings
	// the line back even with -diag-events off. With both off, neither the line nor
	// the suffix exists - byte for byte today's log.
	if sent > 0 && (h.DiagEvents || h.DiagDistance) {
		h.Log.Printf("zone: STOP_PC handle %d (char %d) -> (%d, %d, cell %d) -> %d other player(s)%s",
			handle, index, pos[0], pos[1], pos[2], sent, h.stopDistSuffix(sess, pos))
	}
}

// broadcastMove forwards ONE character's move to every other player, and keeps that
// character's stored position current.
//
// The packet is forwarded for whichever handle moved, followers included: the
// client reports a CZ_MOVE_PC per team member, and the other players must see all
// of them (Python zone_server.py:4612-4627). `index` is that character's place in
// the session's team - exactly what ParseMovePC's charIndex is - so a follower's
// position lands on the follower and not on the leader.
func (h *Handler) broadcastMove(sess *Session, index int, handle uint32, pos [3]int32, pkt []byte) {
	sent := h.sendOthers(sess, handle, pkt)
	h.Players.SetPos(sess, index, pos)
	// THE COUNTER COUNTS THE EVENT; THE LINE IS A DIAGNOSTIC NOW (`-diag-events`).
	// metrics.move() is incremented even when the fan-out reached nobody - the move
	// happened and the server did the work - while the line below only ever covered
	// a fan-out that reached at least one other player (sent > 0), which is what the
	// line has always said. The line is off by default because it sits on the
	// per-packet path: at 50 bots these three movement lines are ~100 lines/s, at 300
	// players in one town ~1000, and that buries every other line in the log. OFF, the
	// METRICS line's `moves=` counts the same event, so the words are gone and the
	// information is not. `-diag-events` restores this line byte for byte.
	metrics.move()
	if h.DiagEvents && sent > 0 {
		h.Log.Printf("zone: MOVE_PC handle %d (char %d) -> (%d, %d, cell %d) -> %d other player(s)",
			handle, index, pos[0], pos[1], pos[2], sent)
	}
}
