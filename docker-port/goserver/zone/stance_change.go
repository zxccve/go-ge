package zone

import (
	"encoding/binary"
	"strconv"

	"geserver/protocol"
)

// The stance key: CZ_CHANGE_STANCE in, ZC_CHANGE_STANCE out.
//
// WHY THIS EXISTS. The client sends 3625 when the player presses the stance key
// (CNormalObject::ReqChangeStance, source2\geClient\Source\GENormalObject.cpp:2231).
// Until this file the zone answered nothing at all, so the request fell into the
// "not handled yet" branch and the character kept the stance it was spawned with.
//
// THERE IS NO MAP RULE. A stance change in a TOWN is ALLOWED - the shipped client
// does not refuse it, and only the displayed skill changes there, because a town has
// no combat (the owner's correction, 2026-10-31). The old
// `World()->IsVillageMap()` branch in source2\geClient\Source\GENormalObject.cpp:2231
// is a pre-release reference and is NOT what the shipped client does, so nothing here
// reads a map type and nothing refuses a request because of the map it arrived on.
//
// WHAT *IS* CHECKED is the stance the client names, and it is the client's own rule:
// CSession::GetStanceByCondition (source2\geClientShared\source\GeSession.cpp:487-520)
// resolves the id from the weapons in the two hands. So a request naming a stance this
// character's job cannot reach with what it holds is a probe or a client bug - it is
// refused here with ONE log line and NO answer, rather than applied and broadcast.
const (
	// CmdCZChangeStance is CZ_CHANGE_STANCE, client -> server, 15 bytes FIXED:
	// cmd(2) seq(4) checksum(4) charIndex(1) stanceType(4).
	// PKS_CZ_CHANGE_STANCE : public PKS_SECURITY_DEFAULT
	// (source2\geShared\include\geZCPacket.h:1788), and PKS_SECURITY_DEFAULT is
	// `WORD command; DWORD seq; int checksum;` (gePacketBase.h:52-58, packed at :38)
	// = the 10-byte header every CZ frame carries. The size is the one the command
	// table registers (data\cmd_table_live.txt:1292 "3625 15 CZ_CHANGE_STANCE").
	CmdCZChangeStance = 3625

	// czChangeStanceSize is that fixed 15 bytes: 10 + 1 + 4. A shorter frame is a
	// broken client and is refused rather than read past (see ParseChangeStance).
	czChangeStanceSize = 15
)

// ChangeStanceRequest is one decoded CZ_CHANGE_STANCE.
type ChangeStanceRequest struct {
	// CharIndex is the client's "multi character index": the position in THIS
	// connection's own team list, in the order this server spawned it - the same
	// order as Session.Team (the same field CZ_EQUIP carries, zone\equip.go).
	CharIndex int
	// Stance is the stance CLASS ID the client wants (10010 Bareknuckle, 10210
	// EpeeGarde, ...). It is never trusted: Handler.changeStance checks it against
	// the character's job and the weapons it holds before anything is applied.
	Stance uint32
}

// ParseChangeStance reads CZ_CHANGE_STANCE (3625):
//
//	+0  cmd(2)=3625  +2 seq(4)  +6 checksum(4)  +10 charIndex(1)  +11 stanceType(4)
//
// ok is false when the frame is shorter than the client's fixed 15 bytes. The
// caller answers NOTHING in that case: a truncated frame is a broken client, and
// reading past it is worse than refusing.
func ParseChangeStance(pt []byte) (ChangeStanceRequest, bool) {
	var req ChangeStanceRequest
	if len(pt) < czChangeStanceSize {
		return req, false
	}
	req.CharIndex = int(pt[10])
	req.Stance = binary.LittleEndian.Uint32(pt[11:])
	return req, true
}

// changeStance answers CZ_CHANGE_STANCE (3625): "this character now stands in
// stance N".
//
// THE THREE REFUSALS ARE IN THIS ORDER ON PURPOSE: a broken frame, then an index
// that is not this team, then a stance the character cannot use. There is no fourth,
// map-shaped refusal: a town map accepts a stance change like any other (the owner's
// correction, 2026-10-31 - see this file's opening comment).
//
// EVERY refusal logs ONE line naming its numbers and answers NOTHING: the client
// already shows the old stance, and a 3554 for a stance this server never validated
// would be a story neither side holds.
func (h *Handler) changeStance(sess *Session, f protocol.Frame) ([][]byte, error) {
	req, ok := ParseChangeStance(f.Data)
	if !ok {
		h.Log.Printf("zone: CHANGE_STANCE refused: %d-byte frame, CZ_CHANGE_STANCE is a fixed %d bytes",
			len(f.Data), czChangeStanceSize)
		return nil, nil
	}
	if req.CharIndex < 0 || req.CharIndex >= len(sess.Team) {
		h.Log.Printf("zone: CHANGE_STANCE refused: character index %d is not in this team "+
			"(%d member(s) spawned)", req.CharIndex, len(sess.Team))
		return nil, nil
	}
	c := &sess.Team[req.CharIndex]
	// A zone with no stance table cannot say what the job may use, and it must not
	// guess: without the table there is no way to tell a real stance from a forged
	// id, so the request is refused instead of applied.
	if h.Stances == nil {
		h.Log.Printf("zone: CHANGE_STANCE refused: this zone has no stance table "+
			"(Handler.Stances is nil), so it cannot say which stance job %d may use",
			c.Job)
		return nil, nil
	}

	// WHAT THE CHARACTER MAY STAND IN, from the game's own tables: the job's stance
	// conditions whose two hands are exactly what this character holds - the same
	// rule CSession::GetStanceByCondition applies on the client
	// (GeSession.cpp:487-520), including its two-handed override (see
	// StanceTable.StanceHands). The set is authoritative: an id outside it is not a
	// stance this character could ever have chosen itself.
	hands := h.Stances.StanceHands(*c)
	allowed := h.Stances.StancesForHands(int(c.Job), hands)
	if !intIn(allowed, int(req.Stance)) {
		// THREE FACTS, because they read differently: a stance the JOB never learns
		// (a wrong-job id - a client bug or a forged packet), a stance the job knows
		// but cannot reach with THESE weapons (a stale request sent while a weapon
		// changed), and a stance id that is not in the table at all (an empty name).
		h.Log.Printf("zone: CHANGE_STANCE refused: stance %d (%q) is not one job %d may "+
			"use with hands %s/%s; that job+hands allows %v (the job knows this stance: %v)",
			req.Stance, h.Stances.StanceName(req.Stance), c.Job,
			normHand(hands[0]), normHand(hands[1]), allowed,
			intIn(h.Stances.JobStances(int(c.Job)), int(req.Stance)))
		return nil, nil
	}

	// ACCEPTED. The stance is stored on the session's copy so a later spawn from the
	// same session carries it, and so the status sheet below answers for it.
	old := c.Stance
	c.Stance = req.Stance

	// A STANCE CHANGE REPLACES A KEPT PRESS (zone\attack.go, holdPress): the press asked
	// for a swing in the OLD stance - its reach, its cadence and its attack type were all
	// that stance's - and the reference's single request slot is overwritten by this
	// change (FieldChar_Request.cpp:1364-1368 writes the stance and re-decides the action
	// state). So the kept press is cleared and nothing is sent for it.
	h.clearPress(sess, req.CharIndex, "the character changed stance to "+
		strconv.FormatUint(uint64(req.Stance), 10)+" ("+
		h.Stances.StanceName(req.Stance)+"), so a press kept in the old stance "+
		strconv.FormatUint(uint64(old), 10)+" no longer describes what the body would do")

	// A STANCE CHANGE ENDS THE SWING REPEAT ONLY WHEN THE NEW STANCE IS NOT ATTACKER
	// AI. That qualifier is the whole point, and the reference states it in one line:
	//
	//	IMC_DECLARE_STRINGID(ATTACKER);
	//	if (AS_ATTACK == actionState && ATTACKER != m_stance->GetAI())
	//		SetActionState(AS_NONE);      // PlayerChar_Request.cpp:1364-1368
	//
	// - the attack state is dropped for a stance whose AI is not the attacker's. An
	// ATTACKER stance is a different rhythm of the same fight, so it keeps the
	// engagement. Ending it there was the owner's "changing stance sometimes drops the
	// attack": the client does ask again, once, about 80 ms later, and that one request
	// usually lands inside the cadence and is held, so nothing resumes.
	//
	// NOTHING ELSE HAS TO BE RE-ARMED WHEN THE REPEAT SURVIVES: the tick reads the
	// stance's own row every pass (attackInterval, stanceRange, AtkType - the ASPD
	// TERMS line), so the new cadence, the new reach and the new attack type all take
	// effect on the next swing. It only has to learn WHICH stance is current, which is
	// the one line in swingTick's stance check.
	//
	// WHICH BRANCH IT TOOK GOES ON THE CHANGE'S OWN LINE below, so a reader of the log
	// never has to guess whether the fight continued.
	repeatNote := "the swing repeat continues (attacker AI)"
	if !h.Stances.IsAttackerAI(int(req.Stance)) {
		h.endSwing(sess, req.CharIndex, "the stance changed from "+strconv.Itoa(int(old))+" ("+
			h.Stances.StanceName(old)+") to "+strconv.Itoa(int(req.Stance))+" ("+
			h.Stances.StanceName(req.Stance)+"), and the new stance's AI is not the attacker "+
			"AI (AI "+strconv.Itoa(h.Stances.AI(int(req.Stance)))+", the attacker profile is "+
			strconv.Itoa(AttackerAI)+"), so the attack state ends "+
			"(PlayerChar_Request.cpp:1364-1368)")
		repeatNote = "the swing repeat is ended (the new stance is not attacker AI)"
	}

	// ONE packet, sent TWICE: to the owner through the return value and to every
	// other player through sendOthers. The slice is built once and shared, so the
	// two cannot disagree; sendOthers skips the sender, which is why the owner's copy
	// has to be in the returned slice.
	pkt := BuildChangeStance(c.Handle, req.Stance)
	others := h.sendOthers(sess, c.Handle, pkt)
	// The stance has now been announced for this handle, so the stop path
	// (pushOwedStance, players.go) must not send a SECOND, OLDER 3554 when this
	// character's walk ends - that one would carry the stance it stood in before
	// this request and undo it on every other screen.
	sess.markStancePushed(c.Handle)

	// The sheet follows the stance: the 3684 this server builds reads the stance's
	// own StnATK/StnHR/IncHR/ASPD/IncASPD/StnMSPD through the game's status script
	// (zone\status_script.go), so ATK, HR, CSPD and MSPD all move with it and the
	// client recomputes none of them. The owner gets it; the other players see a
	// 3554 only, because their panel is not this character's.
	//
	// The MAXIMA are refreshed first: a stance can change max HP/SP too (IncMHP /
	// IncMSP terms), and the bars are drawn against those.
	h.refreshSheet(c)
	statPkt, _ := StatusPacketFor(*c, h.Status)

	// The BUFF BAR follows the stance too. The 3621 is the skill list of the stance
	// the character now stands in, built from the NEW stance (req.Stance) and not the
	// one it just left - the client has already moved the stance, so a list of the old
	// stance's skills would clear `isLocked` on exactly the wrong entries.
	//
	// NO ZC_STANCELIST (3626) IS RE-SENT HERE, and that is a measurement, not an
	// omission: the set 3626 names is the JOB's (datatable_job.EqpWeaponSet ->
	// stancecondition.Stance1..6, zone\stance_list.go), and a stance change never
	// changes the job, so the unlocked set cannot move. The live server does not
	// re-send it either - the last ZC_CHANGE_STANCE in the capture is at 150431 and
	// no 3626 follows it (history\stance-skill-lists.md). The one packet that must
	// follow a change is the 3554, and it does.
	//
	// It goes to the OWNER ONLY, through the returned slice, and never through
	// sendOthers: another player's screen does not show this character's skill bar, and
	// a 3621 for a handle they do not own would at best be ignored. A stance that
	// grants no skills (10010 Bareknuckle, whose SkillID1..5 are all 0) sends NOTHING -
	// no packet and no guess.
	skills := StanceSkillList(h.Stances, req.Stance)
	var listPkt []byte
	if len(skills) > 0 {
		listPkt = BuildSkillList(c.Handle, skills)
		h.Log.Printf("zone: SKILLLIST commander %d char %d (%d) stance %d (%q): "+
			"%d skill(s), %d bytes - pushed to the owner on the stance change",
			sess.CommanderID, req.CharIndex, c.Handle, req.Stance,
			h.Stances.StanceName(req.Stance), len(skills), len(listPkt))
	} else {
		h.Log.Printf("zone: SKILLLIST not sent for commander %d char %d (%d): stance %d "+
			"(%q) grants no skills in datatable_stance.xml (SkillID1..5 are all 0)",
			sess.CommanderID, req.CharIndex, c.Handle, req.Stance,
			h.Stances.StanceName(req.Stance))
	}

	h.Log.Printf("zone: CHANGE_STANCE commander %d char %d (%d) job %d: stance %d (%q) -> %d, "+
		"hands %s/%s, map %d (%s) - sent to the owner and %d other player(s), 3684 re-sent; "+
		"%s",
		sess.CommanderID, req.CharIndex, c.Handle, c.Job, old,
		h.Stances.StanceName(req.Stance), req.Stance,
		normHand(hands[0]), normHand(hands[1]), h.MapID, h.MapName, others, repeatNote)
	if listPkt != nil {
		return [][]byte{pkt, statPkt, listPkt}, nil
	}
	return [][]byte{pkt, statPkt}, nil
}

// intIn answers whether id is one of ids. It is here rather than in stance.go
// because the stance tables have no membership helper of their own, and three
// callers in this file would otherwise each grow a loop.
func intIn(ids []int, id int) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
