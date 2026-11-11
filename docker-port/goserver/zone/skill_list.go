package zone

import (
	"encoding/binary"
)

// ZC_SKILLLIST (3621) - the character's own skill bar, one packet per character.
//
// WHY THIS EXISTS. The START_INFO record a character arrives with carries a skill
// block, and that block is what CREATES the entries in the client's skill list. It
// does not unlock them: the client's 3621 handler only clears `isLocked` on entries
// that are already there, so a 3621 that arrives BEFORE that record sets the flag on
// nothing. Until this file the zone never sent 3621 at all, which is what leaves a
// fresh character's skill bar drawn but locked.
//
// THE LAYOUT IS MEASURED, not derived. 27 ZC_SKILLLIST packets were cut out of the
// live captures (13 capture folders) and every one of them closes exactly as the
// header says:
//
//	+0  WORD  cmd = 3621
//	+2  WORD  len  = the WHOLE packet, header included
//	+4  OBJHANDLE handle - the character the list belongs to (4 bytes)
//	+8  WORD  count   = number of SKILL_INFO records
//	+10 WORD  bmCount = number of BUFF_MODIFIER_SKILL_INFO records
//	+12 count   x { type uint32, level uint16, exp uint32 }  (10 bytes each)
//	    bmCount x { type uint32, level uint16 }                (6 bytes each)
//
// so total = 12 + 10*count + 6*bmCount. In all 27 live samples bmCount == count, and
// the buff block repeats the SAME skill ids in the SAME order with level 0 - it is
// not a separate set of modifiers this zone could compute, so it is filled from the
// ids the caller already gave.
const (
	CmdZCSkillList    = 3621
	skillListHeader   = 12 // cmd(2) len(2) handle(4) count(2) bmCount(2)
	skillListInfoSize = 10 // SKILL_INFO: type(4) level(2) exp(4)
	skillListBuffSize = 6  // BUFF_MODIFIER_SKILL_INFO: type(4) level(2)
)

// skillListLevel and skillListExp are the values this zone sends for EVERY skill it
// lists. They are a PLACEHOLDER with a measurement behind it: 118 of the 119 records
// in the live fixture are level 1 / exp 1, so level 1 / exp 1 is what the live server
// sends for a skill a character has not levelled. The real values are the character's
// own skill rows in the legacy database, which this zone does not read yet
// (zone\stance.go BuildSkillRecords has the same gap for the START_INFO block).
const (
	skillListLevel = 1
	skillListExp   = 1
)

// SkillListEntry is one SKILL_INFO record: the skill, its level, and its exp.
type SkillListEntry struct {
	ID    uint32
	Level uint16
	Exp   uint32
}

// BuildSkillList packs one ZC_SKILLLIST for `handle`.
//
// The length word is written from the FINISHED buffer, never computed a second time:
// a header that disagrees with the bytes is the one fault that makes a variable-size
// packet unreadable, and the client sizes its read from that word.
//
// An EMPTY list returns nil, not a 12-byte header with count 0. The live server has
// never been seen to send one, and the caller (skillListBurst, and the stance change)
// is the side that knows what to do about it: a stance that grants no skills sends
// NOTHING rather than a bar this server guessed at.
func BuildSkillList(handle uint32, skills []SkillListEntry) []byte {
	if len(skills) == 0 {
		return nil
	}
	out := make([]byte, skillListHeader,
		skillListHeader+len(skills)*(skillListInfoSize+skillListBuffSize))
	binary.LittleEndian.PutUint16(out[0:], CmdZCSkillList)
	// +2 (len) is written last, from len(out).
	binary.LittleEndian.PutUint32(out[4:], handle)
	binary.LittleEndian.PutUint16(out[8:], uint16(len(skills)))  // count
	binary.LittleEndian.PutUint16(out[10:], uint16(len(skills))) // bmCount, see above

	for _, s := range skills {
		rec := make([]byte, skillListInfoSize)
		binary.LittleEndian.PutUint32(rec[0:], s.ID)
		binary.LittleEndian.PutUint16(rec[4:], s.Level)
		binary.LittleEndian.PutUint32(rec[6:], s.Exp)
		out = append(out, rec...)
	}
	for _, s := range skills {
		rec := make([]byte, skillListBuffSize)
		binary.LittleEndian.PutUint32(rec[0:], s.ID)
		binary.LittleEndian.PutUint16(rec[4:], 0) // the buff block's level, 0 live
		out = append(out, rec...)
	}

	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// JobSkillList is what the ENTRY BURST sends: every skill the job's whole stance
// set grants, plus the job's own skill - the live server's set, to the id.
//
// WHY SO WIDE. The live 3621 is not "the skills of the stance I am standing in": it
// is the character's whole set, and the measurement is exact for all three captured
// characters (captures\skillcast20261031, see history\stance-skill-lists.md §2):
//
//	Scout   job 3: 54 skills = the distinct SkillID1..5 over the job's 12 stances
//	               (its own job skill, 30113, is one of them, so nothing is added)
//	Fighter job 1: 119 = 118 from its 30 stances + 10415 HighlanderSoul
//	Warlock job 5: 56  = 55  from its 15 stances + 1005 ManaActivation
//
// The extra id is the job's own skill, and the shipped server says where it comes
// from (source2\geServer\ZServerLib\PlayerChar.cpp:2178-2190, GetUseSkillID): the
// job row's `jobSkill` first, then every stance's SkillID1..5. It is read here from
// the SAME datatable_job.xml row this zone already loads for EqpWeaponSet and STD
// (StanceTable.jobSkill), not from a list written by hand.
//
// It is appended at the END because that is where the live packet puts it (index
// 118 of the Fighter's 119, index 55 of the Warlock's 56). THE ORDER IS NOT A
// CONTRACT and must never be tested: the live order is a hash-map walk - stance
// 30110's four skills land split across both ends of the Scout packet - and the
// client matches by `type` only (GeSession.cpp:418-424). Only the SET matters.
//
// Why the WIDE list and not the current stance's: the client creates one row per
// skill in START_INFO's skill block, which this zone already fills with
// BuildSkillRecords(JobSkills(job)) - the whole set - and then marks every row
// LOCKED. A 3621 that names only the current stance's four skills unlocks four
// rows and leaves the rest grey. The entry burst is the moment to unlock them all.
func JobSkillList(st *StanceTable, job int) []SkillListEntry {
	if st == nil {
		return nil
	}
	ids := st.JobSkills(job)
	if own := st.JobSkill(job); own != 0 && !intIn(ids, own) {
		ids = append(ids, own)
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]SkillListEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, SkillListEntry{
			ID:    uint32(id),
			Level: skillListLevel,
			Exp:   skillListExp,
		})
	}
	return out
}

// StanceSkillList is what one stance grants as a skill list: one entry per
// SkillID1..SkillID5 of that stance's row in datatable_stance.xml, in row order.
//
// It is now only the STANCE CHANGE's list (zone\stance_change.go). The entry burst
// uses JobSkillList above, because the live server sends the whole job set there;
// the re-send after a stance change stays this narrow one on purpose - it can only
// clear `isLocked` on rows the burst already unlocked, so it cannot grey anything,
// and it keeps the packet tied to the stance the client has just been told about.
func StanceSkillList(st *StanceTable, stance uint32) []SkillListEntry {
	ids := st.StanceSkills(stance)
	if len(ids) == 0 {
		return nil
	}
	out := make([]SkillListEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, SkillListEntry{
			ID:    uint32(id),
			Level: skillListLevel,
			Exp:   skillListExp,
		})
	}
	return out
}

// skillListBurst is the entry burst's ZC_SKILLLIST packets: ONE per team member, in
// team order, each naming its own handle. The live server does exactly this - a
// 3-character team gets three 3621s with consecutive handles.
//
// The content is JobSkillList: the character's WHOLE set for its job, not the skills
// of the stance it happens to stand in. The live packets are 54 / 119 / 56 records
// for the Scout / Fighter / Warlock, and this zone's tables give exactly those sets
// (TestJobSkillListIsTheLiveSetForTheThreeCapturedJobs). A narrow list would unlock
// only the current stance's four or five rows and leave the rest of the bar grey.
//
// THE STANCE NO LONGER DECIDES THE LIST. It used to, and that is the defect this
// widening removes; the character's job is what the packets agree on now. The stance
// still matters to the CLIENT - it draws the bar for the current stance - which is
// why the 3626 (the stance list) goes out FIRST, immediately before this.
//
// A CHARACTER WITH NOTHING TO LIST SENDS NOTHING, with one line saying which
// commander, which character and which job: a job the stance tables know no stances
// for has no skills either, and inventing a list for it would be a bar this server
// guessed at.
func (h *Handler) skillListBurst(sess *Session) [][]byte {
	if h.Stances == nil {
		h.Log.Printf("zone: SKILLLIST not sent: this zone has no stance table " +
			"(Handler.Stances is nil), so it cannot say which skills any stance grants")
		return nil
	}
	var out [][]byte
	for i := range sess.Team {
		c := &sess.Team[i]
		skills := JobSkillList(h.Stances, int(c.Job))
		if len(skills) == 0 {
			h.Log.Printf("zone: SKILLLIST not sent: commander %d char %d (%d) job %d "+
				"grants no skills in datatable_stance.xml / datatable_job.xml",
				sess.CommanderID, i, c.Handle, c.Job)
			continue
		}
		pkt := BuildSkillList(c.Handle, skills)
		h.Log.Printf("zone: SKILLLIST commander %d char %d (%d) job %d (stance %d %q): "+
			"%d skill(s), %d bytes",
			sess.CommanderID, i, c.Handle, c.Job, c.Stance,
			h.Stances.StanceName(c.Stance), len(skills), len(pkt))
		out = append(out, pkt)
	}
	return out
}
