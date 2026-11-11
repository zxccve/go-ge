package zone

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
)

// The six base stats the client's basic-stat block carries, and the order it
// reads them in.
//
// WHERE THIS ORDER COMES FROM - three sources, none of them our own Python
// server, and they agree:
//
//  1. The shipped client (ge.exe, sha256 8bef2a66...). FUN_005e645a memcpys
//     0x1e5 (485) bytes per record and the parser's next read is wire +477
//     (stanceCount), so the stat block is wire +465..+476 = 12 bytes = 6 WORDs.
//     The slot -> label map is FUN_005e63fe (0x5e63fe), which copies six words
//     from the barrack object into +0xa89/+0xa8b/+0xa8d/+0xa8f/+0xa91/+0xa93
//     (record +465..+475), and FUN_00774635 (0x774635), which labels those same
//     object fields Stat_STR, Stat_AGI, Stat_CON, Stat_DEX, Stat_INT, Stat_CHA.
//     Joined up: +465 STR, +467 CON, +469 DEX, +471 INT, +473 AGI, +475 CHA.
//  2. The reference struct: source2\geShared\include\geZCTypes.h:207 BASIC_STAT
//     is LV(1) HP(8) maxHP(8) SP(2) maxSP(2) EXP(8) nextEXP(8) = 37 bytes, then
//     WORD STR, CON, DEX, INTE, AGI, CHA - the same 37 and the same order.
//  3. The panel in the shipped client, live: with the Python server writing this
//     order, Grandies read 80/60/90/50/50/30, Lv.60 (history\gameplay.md:443).
//
// One trap: the client's own UI/barrack enum is STR, AGI, CON, DEX, INT, CHA
// (FUN_007762e9, 1..6) - a DIFFERENT order from the wire. Reading the wire with
// the UI order swaps AGI and CON and still looks like a valid character sheet,
// which is why this comment names the offsets instead of the enum.
//
// The block is six WORDs; nothing bigger, because the byte after the sixth WORD
// is the stance count.
const statCount = 6

// statNames is the six base stats, in the order the client reads them at
// siBasicStat+bsStats (block offset 37). The names are the game's own: the .ies
// column is INT and the C struct field is INTE - the same number, two labels.
var statNames = [statCount]string{"STR", "CON", "DEX", "INT", "AGI", "CHA"}

// JobStat is ONE job's class base stats - the numbers the client's own
// datatable_job.ies carries per class (STR, CON, DEX, INT, AGI, CHA columns),
// copied to data\job_stats.json by the extractor named in
// history\gameplay.md.
//
// These are BASE stats only: the game's scripts add equipment, mastery,
// promotion and buffs on top (ge\script\pc\get_status_pc.scp), so the sheet's
// ATK/DEF/A.R. are NOT derived from this table alone - and the client does not
// derive them either. They arrive in ZC_UPDATE_ALL_STATUS 3684, which this
// server still does not send (R61b; the proof is in history\zone-entry.md).
type JobStat struct {
	STR int `json:"STR"`
	CON int `json:"CON"`
	DEX int `json:"DEX"`
	INT int `json:"INT"`
	AGI int `json:"AGI"`
	CHA int `json:"CHA"`
}

// Words packs the six stats in the client's order, masked to a WORD exactly as
// the record write does. A negative or oversized value in the file must not
// carry into the neighbouring field.
func (s JobStat) Words() [statCount]uint16 {
	return [statCount]uint16{
		uint16(s.STR & 0xFFFF),
		uint16(s.CON & 0xFFFF),
		uint16(s.DEX & 0xFFFF),
		uint16(s.INT & 0xFFFF),
		uint16(s.AGI & 0xFFFF),
		uint16(s.CHA & 0xFFFF),
	}
}

// JobStats is the class base stats, one entry per job id: data\job_stats.json.
type JobStats struct {
	jobs map[int]JobStat

	// mu guards reported, the ids already named in the log. A team of three on
	// the same unknown job says so once, not once per record and per packet.
	mu       sync.Mutex
	reported map[int]bool
}

// LoadJobStats reads the emitted class base stats.
//
// A failure is returned, never swallowed here: the CALLER decides whether a
// missing file is fatal (in the zone it is not - see main.go loadJobStats).
func LoadJobStats(path string) (*JobStats, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]JobStat
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t := &JobStats{jobs: make(map[int]JobStat, len(raw)), reported: map[int]bool{}}
	for key, stat := range raw {
		id, aerr := strconv.Atoi(key)
		if aerr != nil {
			return nil, fmt.Errorf("%s: job key %q is not a number - the file came "+
				"from another emitter", path, key)
		}
		t.jobs[id] = stat
	}
	if len(t.jobs) == 0 {
		return nil, fmt.Errorf("%s: no job at all", path)
	}
	return t, nil
}

// Len is how many jobs the file holds, for the startup log line.
func (t *JobStats) Len() int {
	if t == nil {
		return 0
	}
	return len(t.jobs)
}

// For answers one job's class base stats.
//
// A job that is not in the file is NOT an error and must never panic: it answers
// six zeros (what the client showed before this table existed) and the id is
// named in the log once, so a missing class is visible instead of silent. A nil
// table - the file was missing at startup - answers zeros too, and stays quiet:
// main.go has already said the whole table is absent.
func (t *JobStats) For(job uint32) (JobStat, bool) {
	if t == nil {
		return JobStat{}, false
	}
	if stat, ok := t.jobs[int(job)]; ok {
		return stat, true
	}
	t.noteMissing(int(job))
	return JobStat{}, false
}

// noteMissing logs an unknown job id exactly once.
func (t *JobStats) noteMissing(job int) {
	t.mu.Lock()
	seen := t.reported[job]
	t.reported[job] = true
	t.mu.Unlock()
	if !seen {
		log.Printf("zone: job %d has no entry in job_stats.json - its six base "+
			"stats are sent as 0 and Character Info will show 0 for them", job)
	}
}

// jobStats is the table BuildStartInfo reads.
//
// It is package state, not a field of Handler, because the record builder is a
// plain function (zone.go calls BuildStartInfo(*c), and appearanceBlock calls it
// for the 3546 redraw) - and because there must be exactly ONE copy: a second
// table would be a second answer for the same character.
var jobStats *JobStats

// SetJobStats installs the class base stats. Called once at startup, before any
// connection; nil is legal and means "no table" (every job answers zeros).
func SetJobStats(t *JobStats) { jobStats = t }

// Sheet answers what the client cannot work out for itself: the six base stats,
// the maximum HP the HP bar is drawn against, and whether the job was found.
//
// maxHP is the game's own rule, not a guess - ge\script\pc\get_status_pc.scp
// SCR_Get_MHP:
//
//	CON*10 + Lv*CON   (+ promotion*1500, + a CON-over-100 bonus*200,
//	                   + equipment / set / mastery - NOT sourced yet)
//
// Only the first two terms are computed here: they are the ones the old save's
// data supports, and for Grandies (job 72, CON 90, level 60) they give 6300, the
// number the game's own scripts produce for that class and level. The missing
// terms are named in history\gameplay.md - a character wearing +CON/+HP gear will
// read LOW until item properties are emitted, and that is the next lane, not a
// silent fudge.
//
// Level comes from c.Level, which PGStore fills with barrack.LevelOf(exp) - the
// same exp table the barrack uses, NOT the DB's LIFE column (Grandies has LIFE 99
// and is level 60).
//
// known is false for a job that is not in the table. That case must NOT read as
// "maximum HP is zero": the caller keeps the stored HP instead, because a zero
// maximum draws an EMPTY bar for a character who is alive. (The Python server
// clamps to 0 there. That is the one place this port deliberately does not copy
// it - the stats themselves still fall back to zeros, which is what the sheet
// shows and what the log line names.)
func BaseStats(c Character) [statCount]uint16 {
	stat, _ := jobStats.For(c.Job)
	return stat.Words()
}
