package zone

import "encoding/binary"

// ZC_STANCELIST (3626) - the stances a character has UNLOCKED, one packet per
// character, sent in the entry burst immediately before that character's
// ZC_SKILLLIST (3621).
//
// WHY THIS EXISTS. The START_INFO record a character arrives with carries a
// stance block, and that block CREATES the stance rows in the client's
// stance/skill window - every one of them flagged `isLocked = true`
// (geClientShared\source\GeSession.cpp:288-294). Only this packet clears that
// flag, and it only clears it on a type START_INFO already made
// (GeSession.cpp:440-455):
//
//	for (j...) if (pStanceInfo->type == stanceList[j]->type) {
//	    memcpy(stanceList[j], pStanceInfo, sizeof(STANCE_INFO));
//	    stanceList[j]->isLocked = false;
//
// so a stance this packet does not name is drawn with the grey icon and the grey
// font (geUI\source\UIStanceListBox.cpp:170-186) - the owner's "some of my stance
// are locked". Until this file the zone never sent 3626 at all, so EVERY stance
// row was grey.
//
// THE LAYOUT IS MEASURED, not derived: the three 3626s in the owner's own capture
// (captures\skillcast20261031\20261031-153900\s2c_7061_55960.bin, a LIVE server)
// close on the byte, on their own length word, at three different sizes:
//
//	+0   WORD  cmd = 3626
//	+2   WORD  len = the WHOLE packet, header included
//	+4   OBJHANDLE handle (4) - the character the list belongs to
//	+8   WORD  count = number of STANCE_INFO records
//	+10  count x STANCE_INFO                               (14 bytes each)
//
//	offset 69172: len 136 = 10 + 9*14   (Scout,  job 3, handle 1560907)
//	offset 70306: len 374 = 10 + 26*14  (Fighter, job 1, handle 1560908)
//	offset 72780: len 192 = 10 + 13*14  (Warlock, job 5, handle 1560909)
//
// 14 is the only stride that divides all three remainders (126/9, 364/26 and
// 182/13 all equal 14 exactly), so it is arithmetic and not an eyeball. The
// client agrees: it computes the count from the length word and IGNORES the count
// field (source2\geClient\Source\GEModeNet.cpp:227-234)
//
//	int stanceCount = ((packet->len - sizeof(PKS_ZC_STANCELIST)) / sizeof(STANCE_INFO));
//
// which is why `len` is written here from the FINISHED buffer and the count is
// written only as a courtesy: a wrong length is fatal, a wrong count is cosmetic.
//
// There is NO locked/unlocked flag and no "learned" byte anywhere in the 14 bytes
// (all 48 records of the three packets measured). A stance is unlocked because it
// is IN this packet, and locked because the server left it out.
const (
	CmdZCStanceList    = 3626
	stanceListHeader   = 10 // cmd(2) len(2) handle(4) count(2)
	stanceListInfoSize = 14 // STANCE_INFO: type(4) level(2) exp(4) skillPoint(4)
)

// stanceEntryLevel, stanceEntryExp and stanceEntrySkillPoint are what this zone
// sends for EVERY stance it names. They are a PLACEHOLDER, and the live sample is
// what says the three fields are real: 46 of its 48 records are level 1 / exp 0 /
// sp 0, and the two that are not carry a trained stance's progress (30110 FirstAid
// and 10110 BackGuard, both level 25, exp 315303, sp 79).
//
// OWED: a character's own stance level, exp and skill point live in its rows in
// the legacy database and are NOT read here yet. Until they are, every stance is
// level 1, so the client's skill-icon test `pStanceInfo->level >= reqStnLv`
// (geUI\source\UISkillListBox.cpp:170) keeps any skill whose `Skill%dReq` is above
// 1 greyed. That is a SKILL question and not this packet's: for the Fighter's 26
// live stances the requirement is 1 everywhere. The same gap is in
// zone\stance.go BuildStanceRecords (the START_INFO block), which is why the two
// packets agree instead of disagreeing.
const (
	stanceEntryLevel      = 1
	stanceEntryExp        = 0
	stanceEntrySkillPoint = 0
)

// StanceEntry is one STANCE_INFO record: the stance, its level, its exp and its
// skill point. The last three are placeholders today - see above.
type StanceEntry struct {
	ID         uint32
	Level      uint16
	Exp        uint32
	SkillPoint uint32
}

// BuildStanceList packs one ZC_STANCELIST for `handle`.
//
// The length word is written from the FINISHED buffer, never computed a second
// time: the client sizes its read from that word, so a header that disagrees with
// the bytes makes the packet unreadable rather than merely wrong.
//
// An EMPTY list returns nil, not a 10-byte header with count 0 - the same rule as
// BuildSkillList. A character with nothing to unlock must send NOTHING: a count-0
// packet would still be a claim about the character, and the caller is the side that
// knows what to do about it (see stanceListBurst).
func BuildStanceList(handle uint32, stances []StanceEntry) []byte {
	if len(stances) == 0 {
		return nil
	}
	out := make([]byte, stanceListHeader, stanceListHeader+len(stances)*stanceListInfoSize)
	binary.LittleEndian.PutUint16(out[0:], CmdZCStanceList)
	// +2 (len) is written last, from len(out).
	binary.LittleEndian.PutUint32(out[4:], handle)
	binary.LittleEndian.PutUint16(out[8:], uint16(len(stances))) // count, cosmetic

	for _, s := range stances {
		rec := make([]byte, stanceListInfoSize)
		binary.LittleEndian.PutUint32(rec[0:], s.ID)
		binary.LittleEndian.PutUint16(rec[4:], s.Level)
		binary.LittleEndian.PutUint32(rec[6:], s.Exp)
		binary.LittleEndian.PutUint32(rec[10:], s.SkillPoint)
		out = append(out, rec...)
	}

	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)))
	return out
}

// stanceEntries is the ONE list of stances this zone will ever name for a job:
// STANCE_INFO entries for the ids t.JobStances(job) gives - the very same call
// zone\stance.go BuildStanceRecords packs into START_INFO, so the rows the client
// creates and the rows this packet unlocks cannot drift apart.
//
// The set is the JOB's, not the hands': the live 3626 for the Fighter names 26
// stances while that character holds a sword and a shield and can stand in a
// handful of them. Filtering by hands here would leave the rest of the column
// grey - which is the complaint this file exists to fix.
//
// TWO KINDS OF ID ARE WITHHELD, and neither becomes a zero entry:
//
//   - an id with no row in datatable_stance.xml (a stale id in a stance-condition
//     row). It has no name and no icon, so the client could not draw it; it is
//     returned in `unresolved` so the caller can say which ones and why.
//   - nothing else. A duel between "everything the job may use" and "only what the
//     character has trained" is settled in the live capture: 25 of the Fighter's 26
//     entries are level 1 / exp 0 (never used) and are still sent, so the live
//     server sends the job's starter set, not the trained set.
func (t *StanceTable) stanceEntries(job int) (entries []StanceEntry, unresolved []int) {
	if t == nil {
		return nil, nil
	}
	ids := t.JobStances(job)
	entries = make([]StanceEntry, 0, len(ids))
	for _, sid := range ids {
		if _, ok := t.rows[sid]; !ok {
			unresolved = append(unresolved, sid)
			continue
		}
		entries = append(entries, StanceEntry{
			ID:         uint32(sid),
			Level:      stanceEntryLevel,
			Exp:        stanceEntryExp,
			SkillPoint: stanceEntrySkillPoint,
		})
	}
	return entries, unresolved
}

// stanceListBurst is the entry burst's ZC_STANCELIST packets: ONE per team member,
// in team order, each naming its own handle - what the live server does for a
// three-character team (three 3626s, 136/374/192 bytes, each immediately followed
// by that character's 3621).
//
// It MUST go out before the skill list: the client's skill-icon test needs the
// stance row unlocked as well as the skill row
// (UISkillListBox.cpp:170 `pStanceInfo->isLocked == false && ...`), so the 3621
// cannot do its job until this has landed.
//
// A CHARACTER WITH NOTHING TO UNLOCK SENDS NOTHING AND SAYS SO, in one line naming
// the commander, the character and the job: a zero-count packet would be a claim
// about a character this zone cannot describe.
func (h *Handler) stanceListBurst(sess *Session) [][]byte {
	if h.Stances == nil {
		h.Log.Printf("zone: STANCELIST not sent: this zone has no stance table " +
			"(Handler.Stances is nil), so it cannot say which stances any job has")
		return nil
	}
	var out [][]byte
	for i := range sess.Team {
		c := &sess.Team[i]
		entries, unresolved := h.Stances.stanceEntries(int(c.Job))
		if len(unresolved) > 0 {
			// Logged even when the rest goes out: an id the tables name and this
			// zone cannot describe is a data fault, and silence would make it look
			// like a stance the job simply does not have.
			h.Log.Printf("zone: STANCELIST commander %d char %d (%d) job %d: %d stance(s) "+
				"of the job's conditions have no row in datatable_stance.xml and are "+
				"NOT sent: %v",
				sess.CommanderID, i, c.Handle, c.Job, len(unresolved), unresolved)
		}
		if len(entries) == 0 {
			h.Log.Printf("zone: STANCELIST not sent: commander %d char %d (%d) job %d has "+
				"no stance this zone can describe, so it sends no (empty) list",
				sess.CommanderID, i, c.Handle, c.Job)
			continue
		}
		pkt := BuildStanceList(c.Handle, entries)
		h.Log.Printf("zone: STANCELIST commander %d char %d (%d) job %d: %d stance(s), "+
			"%d bytes",
			sess.CommanderID, i, c.Handle, c.Job, len(entries), len(pkt))
		out = append(out, pkt)
	}
	return out
}
