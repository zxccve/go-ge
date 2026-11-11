// THE BUFF LIST - one per character, and the rules that decide what lands on it.
//
// WHAT THIS IS, AND WHAT IT IS NOT. It is the buff TABLE (zone\buff_table.go), the
// per-character LIST and the ENTER script (zone\buff_script.go) - the first slice
// named in `research\item-buff-skill.md` §5. It is not "the buff system": the buff's
// STAT terms are carried and not applied (the panel is the shadow's, zone\status.go),
// a SCRIPT-type buff's UPDATE entry is refused rather than half-run, and a `Save=TRUE`
// buff is not written to the database. Each of those is refused or carried BY NAME
// below, so a reader can tell a missing feature from a silent zero.
//
// MODELLED ON `CBuffList` (source2\geServer\ZServerLib\BuffList.cpp):
//
//	Add      BuffList.cpp:103-136   look the row up, refuse a lower rank, remove the
//	                                same-group buff, run OnEnter, set the mask bit
//	Update   BuffList.cpp:265-293   elapsed += dt; INFINITY stays; NORMAL goes when
//	                                elapsed > ApplyTime; SCRIPT asks its own script
//	Remove   BuffList.cpp:224       drop the entry, clear its bit
//	IsBuffApplied / GetAppliedBuffLevel   BuffList.cpp:502 / :527  by NAME; -1 when
//	                                the buff is not there
//
// THE DURATION IS NEVER INVENTED. It is the row's `ApplyTime` (0 on 1,954 of 1,962
// rows) OVERWRITTEN by what the buff's own ENTER script left in `buff.ApplyTime`.
// With no ENTER and a table value of 0 there is NO duration, and a NORMAL buff is
// then REFUSED - granting it would mean a buff that dies on the first tick, which is
// the shape the rail forbids. 546 of the 1,962 rows are scriptless and 325 of those
// are NORMAL with ApplyTime 0 (measured against `ge\script\**`; `Poison` 1,
// `PotionHPUP`, `PSoul`, `IncAR` are examples).
//
// ONE GOROUTINE OWNS ONE LIST. A session's buffs are written and read only on that
// connection's own goroutine - the item use, the click, the script the zone runs and
// the tick all run there - exactly like `casts`, `swings` and `presses` (zone\zone.go).
// There is no mutex here on purpose: a second one would suggest a second writer, and
// cross-session grants (AddBuffToAllPC, AddBuffToMonster) are NOT in this slice. A
// handle that is not this session's own team is refused with a line.
package zone

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"geserver/script"
)

// buff is ONE applied buff: a row, the numbers it was applied with, what its ENTER
// script computed, and how long it has been up.
type buff struct {
	Row        BuffRow
	Name       string
	ClassID    uint32
	UpdateType string
	// ApplyTime is the duration the list counts against, in MILLISECONDS: the row's
	// own value overwritten by the ENTER script's (Buff.cpp:79-90 - `m_buff.time =
	// GetApplyTime()` AFTER the script ran). 0 is possible only for INFINITY rows,
	// because a NORMAL one with no duration is refused.
	ApplyTime float64
	// elapsed is how long it has been up, in ms, advanced by Update.
	elapsed float64
	// Arg1/Arg2/Arg3 are what it was applied with - the reference's `m_buff.arg1/2`
	// plus the third argument the ENTER script receives. Arg1 is the buff's LEVEL:
	// `CBuff::GetSkiilLevel()` answers `m_buff.arg1` (Buff.cpp:583-586) and 3125's
	// `level` byte is that number.
	Arg1, Arg2, Arg3 float64
	// BM is every `*_BM` term the ENTER script set, carried and not applied.
	BM map[string]float64
	// Enter is the entry point that ran, "" when the row has no ENTER script.
	Enter string
}

// remainingMS is what is left of the buff, in milliseconds, floored at 0 - the
// reference's `SendBuffRemainTime` clamp (Buff.cpp:563-567).
func (b *buff) remainingMS() uint32 {
	if b.ApplyTime <= 0 {
		return 0
	}
	left := b.ApplyTime - b.elapsed
	if left <= 0 {
		return 0
	}
	return uint32(left)
}

// level is the buff's applied level as 3125 carries it: arg1 in ONE BYTE. A negative
// or an over-range value cannot be carried, and clamping is the only honest answer
// to a number the wire cannot hold - 0 and 255 are its own ends.
func (b *buff) level() byte {
	switch {
	case b.Arg1 <= 0:
		return 0
	case b.Arg1 >= 255:
		return 255
	}
	return byte(b.Arg1)
}

// bmNote renders the carried stat terms for the log line, in name order so two runs
// of the same buff read the same.
func (b *buff) bmNote() string {
	if len(b.BM) == 0 {
		return "no *_BM term"
	}
	names := make([]string, 0, len(b.BM))
	for k := range b.BM {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s %g", n, b.BM[n]))
	}
	return strings.Join(parts, ", ")
}

// buffList is ONE character's buffs, in the order they were applied.
type buffList struct {
	buffs []*buff
}

// Len answers how many buffs are up.
func (l *buffList) Len() int {
	if l == nil {
		return 0
	}
	return len(l.buffs)
}

// Add appends ONE already-resolved buff, after the same-group rule has removed what
// it replaces, and answers the names it removed.
//
// THE ORDER IS THE REFERENCE'S: `RemoveSameGroupBuff` runs BEFORE the new buff is
// appended and before OnEnter (BuffList.cpp:118-127), so a script that asks
// `IsBuffApplied` about its own name during OnEnter sees it already there - which is
// why the zone appends first and runs the ENTER second, and why an ENTER that raises
// takes the entry back out (Handler.addBuff).
func (l *buffList) Add(b *buff) (removed []string) {
	removed = l.removeSameGroup(b.Row)
	l.buffs = append(l.buffs, b)
	return removed
}

// Remove drops one buff by name and answers whether it was there.
func (l *buffList) Remove(name string) bool {
	if l == nil {
		return false
	}
	for i, b := range l.buffs {
		if b.Name == name {
			l.buffs = append(l.buffs[:i], l.buffs[i+1:]...)
			return true
		}
	}
	return false
}

// Update advances every buff by `elapsed` milliseconds and answers the ones that
// expired, removing them.
//
// THE THREE TYPES, exactly as `CBuff::OnUpdate` (Buff.cpp:137-186):
//
//	INFINITY  never expires - it returns 1 before looking at any time.
//	NORMAL    stays while `elapsed <= ApplyTime`, expires when it is GREATER
//	          (`if (m_elapsedTime <= GetApplyTime()) return 1; else return 2;`).
//	SCRIPT    its own UPDATE script decides. THIS SLICE DOES NOT RUN ONE, so a SCRIPT
//	          buff is refused at Add and can never be in this list - if one is ever
//	          found here it is expired on the spot with a line, because a buff that
//	          cannot be advanced must not be drawn as if it were.
func (l *buffList) Update(elapsedMS float64) (expired []*buff) {
	if l == nil || len(l.buffs) == 0 {
		return nil
	}
	keep := l.buffs[:0]
	for _, b := range l.buffs {
		b.elapsed += elapsedMS
		switch b.UpdateType {
		case BuffUpdateInfinity:
			keep = append(keep, b)
		case BuffUpdateNormal:
			if b.elapsed > b.ApplyTime {
				expired = append(expired, b)
				continue
			}
			keep = append(keep, b)
		default: // BuffUpdateScript (refused at Add) and anything unreadable
			expired = append(expired, b)
		}
	}
	// Zero the tail so a dropped *buff is not kept alive by the slice's capacity.
	for i := len(keep); i < len(l.buffs); i++ {
		l.buffs[i] = nil
	}
	l.buffs = keep
	return expired
}

// IsBuffApplied answers whether the named buff is up - the rule the corpus compares
// against 'YES'.
func (l *buffList) IsBuffApplied(name string) bool {
	if l == nil {
		return false
	}
	for _, b := range l.buffs {
		if b.Name == name {
			return true
		}
	}
	return false
}

// GetAppliedBuffLevel answers the named buff's level, or -1 when it is not applied.
// -1 is the reference's own answer (`CBuffList::GetAppliedBuffLevel`, BuffList.cpp:527).
func (l *buffList) GetAppliedBuffLevel(name string) float64 {
	if l == nil {
		return -1
	}
	for _, b := range l.buffs {
		if b.Name == name {
			return b.Arg1
		}
	}
	return -1
}

// Mask is the 256-byte bitmap 3556 carries: one bit per up buff, at its ClassID.
func (l *buffList) Mask() [BuffMaskSize]byte {
	var mask [BuffMaskSize]byte
	if l == nil {
		return mask
	}
	for _, b := range l.buffs {
		buffMaskBit(&mask, b.ClassID)
	}
	return mask
}

// Names lists the applied buffs in application order - for a log line or a test.
func (l *buffList) Names() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.buffs))
	for _, b := range l.buffs {
		out = append(out, b.Name)
	}
	return out
}

// removeSameGroup applies `CBuffList::RemoveSameGroupBuff` (BuffList.cpp:171-...):
// the buffs whose group overlaps the new row's go, because two buffs of one group
// are the same effect and the newer one wins.
func (l *buffList) removeSameGroup(row BuffRow) (removed []string) {
	if l == nil || !buffHasGroup(row) {
		return nil
	}
	keep := l.buffs[:0]
	for _, b := range l.buffs {
		if buffHasGroup(b.Row) && buffGroupsOverlap(row, b.Row) {
			removed = append(removed, b.Name)
			continue
		}
		keep = append(keep, b)
	}
	for i := len(keep); i < len(l.buffs); i++ {
		l.buffs[i] = nil
	}
	l.buffs = keep
	return removed
}

// higherRankApplied answers the buff that makes `row`'s application REFUSED under
// `CBuffList::ExistHigherRank` (BuffList.cpp:62-99): a buff of an overlapping group
// with a STRICTLY HIGHER `BuffRank`. The rule is rank-first and group-second, so a
// higher-rank buff of the same group cannot be pushed down by a weaker one.
func (l *buffList) higherRankApplied(row BuffRow) *buff {
	if l == nil || !buffHasGroup(row) {
		return nil
	}
	for _, b := range l.buffs {
		if b.Row.ID == row.ID {
			continue // itself is not a competing higher rank
		}
		if !buffHasGroup(b.Row) || !buffGroupsOverlap(row, b.Row) {
			continue
		}
		if row.BuffRank < b.Row.BuffRank {
			return b
		}
	}
	return nil
}

// buffHasGroup answers whether a row names a real group. The table spells "no group"
// as the word None, and an empty attribute is the same thing - never a group "0".
func buffHasGroup(r BuffRow) bool {
	g := strings.TrimSpace(r.GroupIndex)
	return g != "" && !strings.EqualFold(g, "None")
}

// buffGroupsOverlap is the reference's three-clause test
// (`pNewBuffGroupIndexList->HasElement(old.GroupIndex)` OR the reverse OR the plain
// equality, BuffList.cpp:88-96) reduced to the single group each shipped row names:
// same GroupIndex, or either row's ExGroupIndex naming the other's group.
func buffGroupsOverlap(a, b BuffRow) bool {
	ga, gb := strings.TrimSpace(a.GroupIndex), strings.TrimSpace(b.GroupIndex)
	if ga != "" && strings.EqualFold(ga, gb) {
		return true
	}
	if ex := strings.TrimSpace(a.ExGroupIndex); ex != "" && strings.EqualFold(ex, gb) {
		return true
	}
	if ex := strings.TrimSpace(b.ExGroupIndex); ex != "" && strings.EqualFold(ex, ga) {
		return true
	}
	return false
}

// ---- the SESSION side: who owns which list -------------------------------------

// buffsOf answers character `idx`'s list, creating it on first use. It is called
// only from this session's own goroutine.
func (s *Session) buffsOf(idx int) *buffList {
	for len(s.buffs) <= idx {
		s.buffs = append(s.buffs, nil)
	}
	if s.buffs[idx] == nil {
		s.buffs[idx] = &buffList{}
	}
	return s.buffs[idx]
}

// liveBuffs answers character `idx`'s list WITHOUT creating one - the tick's reader,
// so an idle session of three characters does not grow three empty lists.
func (s *Session) liveBuffs(idx int) *buffList {
	if idx < 0 || idx >= len(s.buffs) {
		return nil
	}
	return s.buffs[idx]
}

// hasBuffs answers whether ANY character in this session has a buff up. It is the
// session clock's new reason to wake (zone\chase.go, sessionClockAwake): a buff that
// expires in 30 s must expire while the player stands still, and the zone only ever
// wakes for work somebody named.
func (s *Session) hasBuffs() bool {
	for _, l := range s.buffs {
		if l != nil && len(l.buffs) > 0 {
			return true
		}
	}
	return false
}

// teamHandles is every character handle in this session, in team order.
func (s *Session) teamHandles() []uint32 {
	out := make([]uint32, 0, len(s.Team))
	for i := range s.Team {
		out = append(out, s.Team[i].Handle)
	}
	return out
}

// teamIndex answers the slot of `handle` in this session, or -1.
func (s *Session) teamIndex(handle uint32) int {
	for i := range s.Team {
		if s.Team[i].Handle == handle {
			return i
		}
	}
	return -1
}

// ---- the MONSTER side: one list per body ---------------------------------------

// monsterBuffList answers monster `handle`'s buff list, creating it on first use.
// The map access takes monsterBuffMu; the list itself is only written from inside a
// script run (see Handler.monsterBuffs).
func (h *Handler) monsterBuffList(handle uint32) *buffList {
	h.monsterBuffMu.Lock()
	defer h.monsterBuffMu.Unlock()
	if h.monsterBuffs == nil {
		h.monsterBuffs = make(map[uint32]*buffList, 8)
	}
	l := h.monsterBuffs[handle]
	if l == nil {
		l = &buffList{}
		h.monsterBuffs[handle] = l
	}
	return l
}

// liveMonsterBuffs answers the list WITHOUT creating one, for a reader that must not
// grow the map (the `IsBuffApplied` builtin runs on every message script).
func (h *Handler) liveMonsterBuffs(handle uint32) *buffList {
	h.monsterBuffMu.Lock()
	defer h.monsterBuffMu.Unlock()
	return h.monsterBuffs[handle]
}

// forgetMonsterBuffs drops a body's list when the body leaves the field.
func (h *Handler) forgetMonsterBuffs(handle uint32) {
	h.monsterBuffMu.Lock()
	delete(h.monsterBuffs, handle)
	h.monsterBuffMu.Unlock()
}

// ---- the HANDLER side: the rules, the packets and the lines -------------------

// buffRecipient is WHO a buff lands on, reduced to the four things the rules need.
//
// A player character and a monster are the SAME thing in the reference - both are
// CFieldChar and both own a CBuffList (`CFieldChar::AddBuff`, FieldChar.cpp:665) -
// so the rule body is shared and only the owner's identity differs. `player` is the
// one place they differ on the wire: the 3125 timer is sent to a player character
// only (`CBuff::OnEnter` sends it when `OT_PLAYERCHAR == owner`'s type,
// Buff.cpp:104-105), and a monster's 3556 is broadcast instead of fanned out to one
// session's peers.
type buffRecipient struct {
	handle uint32
	who    string // the log's words: `char 455 ("Grace")` / `monster 189764 ("Scarecrow")`
	list   *buffList
	self   script.StatusSelf
	src    script.StatusSource
	player bool
}

// AddBuff applies one buff BY NAME to one character of this session and answers the
// packets it owes the owner, whether it landed, and why it did not.
//
// THE RULES, in the reference's order (`CBuffList::Add`, BuffList.cpp:103-136):
//
//  1. the name must be a ROW - an unknown name is refused, never turned into a zero row;
//  2. a strictly higher rank of an overlapping group refuses it (`ExistHigherRank`);
//  3. the same-group buffs are removed (the new one wins);
//  4. the buff's ENTER script runs with (self, buff, arg1, arg2, arg3) and the
//     duration is what it left in `buff.ApplyTime`;
//  5. a NORMAL buff with no duration is REFUSED - never granted with 0;
//  6. a SCRIPT buff is refused: its expiry is its own UPDATE script's answer and
//     this slice does not run one (`Buff.cpp:163-186`);
//  7. on success: one 3556 broadcast (the owner gets it in the return value, every
//     other player through the fan-out) and one 3125 to the owner for the bar's timer.
func (h *Handler) AddBuff(sess *Session, idx int, name string, arg1, arg2, arg3, addTime float64) (out [][]byte, added bool, why string) {
	// The OUTERMOST entry: it takes the runner's lock (see BuffScript.Enter). The
	// adapter in this package (`buffWorld`) calls addBuff directly, because a script is
	// already inside that lock when it asks for a buff.
	return h.addBuff(sess, idx, name, arg1, arg2, arg3, addTime, false)
}

// addBuff is AddBuff with the "a script already holds the lock" bit threaded in.
func (h *Handler) addBuff(sess *Session, idx int, name string, arg1, arg2, arg3, addTime float64, inScript bool) (out [][]byte, added bool, why string) {
	if sess == nil || idx < 0 || idx >= len(sess.Team) {
		return nil, false, "no such character in this session"
	}
	c := sess.Team[idx]
	r := buffRecipient{
		handle: c.Handle,
		who:    fmt.Sprintf("char %d (%q)", c.Handle, c.Name),
		list:   sess.buffsOf(idx),
		self:   statusSelfFor(c),
		player: true,
	}
	if h.BuffScript != nil {
		r.src = h.BuffScript.source(c)
	}
	out, change, added, why := h.grantBuff(r, name, arg1, arg2, arg3, addTime, inScript, sess)
	if change != nil {
		h.fanOut(sess, c.Handle, change)
	}
	return out, added, why
}

// addMonsterBuff applies one buff to a MONSTER body - the end the skill lane needed:
// `SCR_RESULT_BATTLEBUFF_SKILL` calls `AddBuff(self, arg.Buff, ...)` with a monster
// as `self` (common_pc.scp:6903), and the reference's monster is a CFieldChar with a
// buff list of its own (`CMonsterNPC::OnProcess` ticks it, MonsterNPC.cpp:343).
//
// THE 3556 IS BROADCAST: a monster has no owning session, and every player who can
// see the body must see its buff bar. There is NO 3125 - the reference sends that
// timer to a player character only - and this slice does not age a monster's buff
// yet (a NAMED gap: `monsterBuffTick` does not exist, so a monster's buff stays up
// until it is removed; the PC path's tick is unaffected).
func (h *Handler) addMonsterBuff(handle uint32, name string, arg1, arg2, arg3, addTime float64, inScript bool, sess *Session) (out [][]byte, added bool, why string) {
	m, ok := h.monsterByHandle(handle)
	if !ok {
		return nil, false, fmt.Sprintf("handle 0x%08x is not a monster on this field", handle)
	}
	r := buffRecipient{
		handle: m.Handle,
		who:    fmt.Sprintf("monster %d (%q)", m.Handle, h.MonsterName(m)),
		list:   h.monsterBuffList(m.Handle),
		self:   monsterStatusSelf(m),
		player: false,
	}
	out, change, added, why := h.grantBuff(r, name, arg1, arg2, arg3, addTime, inScript, sess)
	if added {
		// THE CLOCK THIS SLICE DOES NOT HAVE, said once, when it first matters.
		h.noteMonsterBuffNotAged()
	}
	if change != nil {
		// THE WHOLE ZONE, ONCE. `broadcastWorld` writes to every session including the
		// caster's, so the copy `grantBuff` put in `out` would reach that client twice -
		// take it out before the batch is returned as the reply.
		h.broadcastWorld(change)
		out = withoutPacket(out, change)
	}
	return out, added, why
}

// withoutPacket answers `pkts` without the element that IS `pkt` (same backing bytes),
// and answers `pkts` untouched when it is not there.
func withoutPacket(pkts [][]byte, pkt []byte) [][]byte {
	if len(pkt) == 0 {
		return pkts
	}
	kept := pkts[:0]
	for _, p := range pkts {
		if len(p) == len(pkt) && &p[0] == &pkt[0] {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// noteMonsterBuffNotAged says ONCE that a monster's buff has no clock in this slice.
// A buff with a duration that never counts down is a lie the owner would only find
// out about minutes later, so the first one says it in the log.
func (h *Handler) noteMonsterBuffNotAged() {
	if h == nil || h.Log == nil {
		return
	}
	h.monsterBuffMu.Lock()
	said := h.monsterBuffAgingSaid
	h.monsterBuffAgingSaid = true
	h.monsterBuffMu.Unlock()
	if !said {
		h.Log.Printf("zone: BUFF: a MONSTER's buff is granted and readable, but this slice " +
			"does not age it - no monster process loop runs CBuffList::Update here yet, so " +
			"the icon stays until something removes it")
	}
}

// grantBuff is the RULE BODY, shared by a player character and a monster: look the
// row up, refuse a lower rank, add, run the ENTER, enforce the duration rule, and
// answer the 3556 (through `change`) plus every packet the owner is owed.
func (h *Handler) grantBuff(r buffRecipient, name string, arg1, arg2, arg3, addTime float64, inScript bool, sess *Session) (out [][]byte, change []byte, added bool, why string) {
	if h.Buffs == nil {
		return nil, nil, false, "this build has no Buff table loaded (ge\\xml\\datatable_buff.xml)"
	}
	row, ok := h.Buffs.Row(name)
	if !ok {
		h.Log.Printf("zone: BUFF REFUSED %s %q: the Buff table has no row with "+
			"that ClassName - a name it does not carry is never granted from invented values",
			r.who, name)
		return nil, nil, false, "no row named " + name
	}
	l := r.list
	if higher := l.higherRankApplied(row); higher != nil {
		h.Log.Printf("zone: BUFF REFUSED %s %q (class %d, rank %d, group %q): "+
			"%q (rank %d) of the same group is applied and this row's rank is LOWER "+
			"(ExistHigherRank, BuffList.cpp:62-99)",
			r.who, name, row.ID, row.BuffRank, row.GroupIndex,
			higher.Name, higher.Row.BuffRank)
		return nil, nil, false, fmt.Sprintf("a higher rank of group %q is applied (%s)",
			row.GroupIndex, higher.Name)
	}

	b := &buff{
		Row: row, Name: row.Name, ClassID: row.ID, UpdateType: row.UpdateType,
		ApplyTime: row.ApplyTime, Arg1: arg1, Arg2: arg2, Arg3: arg3,
	}
	removed := l.Add(b)

	// THE ENTER SCRIPT IS WHERE THE DURATION LIVES. With no corpus, or a buff the
	// corpus does not define, nothing runs and the row's own ApplyTime stands - which
	// for a NORMAL buff is 0, and the rule below then refuses it.
	//
	// THE LIST IS HANDED TO THE SCRIPT, the way the reference runs OnEnter with the
	// buff already in its list (BuffList.cpp:127-130): an ENTER that asks
	// `IsBuffApplied(self,'X')` must get the truth, and one that applies a second buff
	// must land it. Its packets ride `w.out` and go out BEFORE this buff's own 3556.
	world := &buffWorld{h: h, sess: sess, inScript: true}
	entry := row.EnterEntry()
	if h.BuffScript != nil {
		var res script.BuffEnterResult
		var err error
		if inScript {
			res, err = h.BuffScript.enterActorLocked(entry, r.self, r.src, row.Lua(), arg1, arg2, arg3, world)
		} else {
			res, err = h.BuffScript.EnterActor(entry, r.self, r.src, row.Lua(), arg1, arg2, arg3, world)
		}
		out = append(out, world.out...)
		if err != nil {
			l.Remove(b.Name) // take the half-applied entry back out (see buffList.Add)
			h.Log.Printf("zone: BUFF REFUSED %s %q (class %d): its ENTER %s "+
				"RAISED: %v - a duration nobody computed is not a duration of 0, so the "+
				"buff is not granted", r.who, name, row.ID, entry, err)
			return nil, nil, false, "the ENTER script raised: " + err.Error()
		}
		if res.Ran {
			b.Enter = entry
			b.ApplyTime = res.ApplyTime
			b.BM = res.BM
		}
	}
	// `AddBuffWithTime`'s additional time is applied AFTER the script, exactly as
	// `CBuff::OnEnter` does it (Buff.cpp:83-86: `SetApplyTime(GetApplyTime() + additionalTime)`).
	if addTime != 0 {
		b.ApplyTime += addTime
	}

	switch b.UpdateType {
	case BuffUpdateNormal:
		if b.ApplyTime <= 0 {
			l.Remove(b.Name)
			reason := "scriptless"
			if b.Enter != "" {
				reason = "its ENTER left ApplyTime 0"
			}
			h.Log.Printf("zone: BUFF REFUSED %s %q (class %d): %s and the row's "+
				"own ApplyTime is %g - a NORMAL buff with no duration would expire on the "+
				"first tick, so it is REFUSED rather than granted with 0",
				r.who, name, row.ID, reason, row.ApplyTime)
			return nil, nil, false, fmt.Sprintf("no duration: %s", reason)
		}
	case BuffUpdateInfinity:
		// Never expires. A 0 here is the normal case (buff 43 Clique, 45 WrongAnswer, ...).
	case BuffUpdateScript:
		l.Remove(b.Name)
		h.Log.Printf("zone: BUFF REFUSED %s %q (class %d): UpdateType SCRIPT - "+
			"its expiry is the answer of SCR_BUFF_UPDATE_%s and this slice runs no UPDATE "+
			"entry, so granting it would draw a buff nothing can ever end",
			r.who, name, row.ID, row.Name)
		return nil, nil, false, "UpdateType SCRIPT needs its UPDATE script, not in this slice"
	default:
		l.Remove(b.Name)
		h.Log.Printf("zone: BUFF REFUSED %s %q (class %d): UpdateType %q is not "+
			"one of NORMAL / SCRIPT / INFINITY", r.who, name, row.ID, row.UpdateType)
		return nil, nil, false, "UpdateType " + row.UpdateType
	}

	// ONE PACKET, SENT TWICE: the owner through the return value, every other player
	// through the fan-out (a monster: every player, through broadcastWorld). The mask
	// is the WHOLE bar (3556's body), so a removal by the same-group rule above is
	// carried by this one packet and needs no second.
	pkt := BuildChangeBuff(r.handle, l.Mask())
	out = append(out, pkt)
	// 3125 is the bar's TIMER, and it goes to the OWNER only (Buff.cpp:104-105 sends it
	// when the object is a player character; it is not broadcast).
	if r.player {
		out = append(out, BuildBuffRemainTime(r.handle, b.ClassID, b.level(), b.remainingMS()))
	}

	replaced := "nothing"
	if len(removed) > 0 {
		replaced = strings.Join(removed, ", ")
	}
	h.Log.Printf("zone: BUFF ADD %s %q class %d %s apply %gms (row %g%s) args "+
		"%g/%g/%g level %d bit %d/%d replace %s; %s; the list is now %v",
		r.who, b.Name, b.ClassID, b.UpdateType, b.ApplyTime, row.ApplyTime,
		enterNote(b), b.Arg1, b.Arg2, b.Arg3, b.level(), b.ClassID>>3, b.ClassID&7,
		replaced, b.bmNote(), l.Names())
	return out, pkt, true, ""
}

// enterNote is the log's own "where the duration came from" clause: the ENTER entry
// when one ran, or the word that says none did.
func enterNote(b *buff) string {
	if b.Enter == "" {
		return ", no ENTER entry - the row's own value"
	}
	return ", " + b.Enter
}

// RemoveBuff drops one buff by name and answers the packets it owes the owner.
func (h *Handler) RemoveBuff(sess *Session, idx int, name string, inDB bool) (out [][]byte, removed bool, why string) {
	if sess == nil || idx < 0 || idx >= len(sess.Team) {
		return nil, false, "no such character in this session"
	}
	c := sess.Team[idx]
	l := sess.liveBuffs(idx)
	if !l.Remove(name) {
		// The reference's RemoveBuff is silent when there is nothing to remove
		// (`CBuffList::RemoveBuff` walks the list and finds no match); one line here,
		// because "the script asked to remove something that is not up" is the kind of
		// thing a reader needs to see at least once.
		h.Log.Printf("zone: BUFF REMOVE char %d (%q) %q was NOT applied - nothing to remove, "+
			"nothing sent", c.Handle, c.Name, name)
		return nil, false, "not applied"
	}
	pkt := BuildChangeBuff(c.Handle, l.Mask())
	h.fanOut(sess, c.Handle, pkt)
	out = append(out, pkt)
	h.Log.Printf("zone: BUFF REMOVE char %d (%q) %q (database flag %v): the bit is cleared "+
		"and the whole bar resent; the list is now %v (this build persists no buff, so the "+
		"flag changes nothing here)", c.Handle, c.Name, name, inDB, l.Names())
	return out, true, ""
}

// buffTick advances every character's buffs by the wall time since the last pass,
// removes what expired and answers the 3556s due. It runs on the session's own clock
// (zone\chase.go, tickIfDue -> chaseTick), which only wakes because
// sessionClockAwake now counts a live buff as work.
//
// THE ELAPSED TIME IS MEASURED, NOT THE POLL INTERVAL. `tickIfDue` runs at most once
// per pollInterval, but a loop that spun through many frames would run it less often
// than the clock says; measuring `now - lastBuffAt` is the reference's own
// `timeGetTime() - m_prevTime` (BuffList.cpp:267-269) and keeps a 30 s buff at 30 s
// whatever the socket did.
func (h *Handler) buffTick(sess *Session) [][]byte {
	if sess == nil || !sess.hasBuffs() {
		return nil
	}
	now := time.Now()
	if sess.buffAt.IsZero() {
		sess.buffAt = now
		return nil
	}
	elapsed := float64(now.Sub(sess.buffAt)) / float64(time.Millisecond)
	if elapsed <= 0 {
		return nil
	}
	sess.buffAt = now

	var out [][]byte
	for i := range sess.Team {
		l := sess.liveBuffs(i)
		if l.Len() == 0 {
			continue
		}
		expired := l.Update(elapsed)
		if len(expired) == 0 {
			continue
		}
		c := sess.Team[i]
		pkt := BuildChangeBuff(c.Handle, l.Mask())
		h.fanOut(sess, c.Handle, pkt)
		out = append(out, pkt)
		for _, b := range expired {
			h.Log.Printf("zone: BUFF EXPIRED char %d (%q) %q class %d after %gms of %g "+
				"(%s): the bit is cleared and the bar resent; the list is now %v",
				c.Handle, c.Name, b.Name, b.ClassID, b.elapsed, b.ApplyTime, b.UpdateType,
				l.Names())
		}
	}
	return out
}

// buffWorld is the zone behind the nine script builtins (script.BuffWorld), for ONE
// run and ONE session. Packets it builds are COLLECTED, not sent: the caller owns the
// reply batch, so a script-driven buff lands in the same order as everything else
// that use produced (the item's 3613/3608 first, then the buff's 3556/3125).
type buffWorld struct {
	h    *Handler
	sess *Session
	out  [][]byte
	// inScript says this world was built by a run that already holds the buff runner's
	// lock (an item's own script, or a buff ENTER). A nested add must then call
	// enterLocked instead of Enter, or the same goroutine would deadlock on itself -
	// the exact shape `SCP_USE_ITEM_AddBuff -> AddBuffImm -> ENTER` has.
	inScript bool
}

// AddBuff implements script.BuffWorld.
//
// TWO KINDS OF OWNER: one of this session's characters, or a MONSTER body (a skill's
// buff rule can land on either - `AddBuff(self, arg.Buff, ...)`, common_pc.scp:6903).
// A handle that is neither is refused with a line, never guessed at.
func (w *buffWorld) AddBuff(handle uint32, name string, a1, a2, a3, addTime float64) (bool, string) {
	if idx := w.sessionIndex(handle); idx >= 0 {
		pkts, added, why := w.h.addBuff(w.sess, idx, name, a1, a2, a3, addTime, w.inScript)
		w.out = append(w.out, pkts...)
		return added, why
	}
	if w.h.monsterOnField(handle) {
		pkts, added, why := w.h.addMonsterBuff(handle, name, a1, a2, a3, addTime, w.inScript, w.sess)
		w.out = append(w.out, pkts...)
		return added, why
	}
	w.h.Log.Printf("zone: BUFF REFUSED handle 0x%08x (%q): it is neither one of this "+
		"session's characters nor a monster on this field - nothing was applied", handle, name)
	return false, "handle is neither this session's character nor a live monster"
}

// RemoveBuff implements script.BuffWorld.
func (w *buffWorld) RemoveBuff(handle uint32, name string, inDB bool) (bool, string) {
	if idx := w.sessionIndex(handle); idx >= 0 {
		pkts, removed, why := w.h.RemoveBuff(w.sess, idx, name, inDB)
		w.out = append(w.out, pkts...)
		return removed, why
	}
	if w.h.monsterOnField(handle) {
		return w.h.removeMonsterBuff(handle, name)
	}
	return false, "handle is neither this session's character nor a live monster"
}

// IsBuffApplied implements script.BuffWorld.
func (w *buffWorld) IsBuffApplied(handle uint32, name string) bool {
	if idx := w.sessionIndex(handle); idx >= 0 {
		return w.sess.liveBuffs(idx).IsBuffApplied(name)
	}
	if l := w.h.liveMonsterBuffs(handle); l != nil {
		return l.IsBuffApplied(name)
	}
	return false
}

// AppliedBuffLevel implements script.BuffWorld.
func (w *buffWorld) AppliedBuffLevel(handle uint32, name string) float64 {
	if idx := w.sessionIndex(handle); idx >= 0 {
		return w.sess.liveBuffs(idx).GetAppliedBuffLevel(name)
	}
	if l := w.h.liveMonsterBuffs(handle); l != nil {
		return l.GetAppliedBuffLevel(name)
	}
	return -1
}

// TeamBuffTargets implements script.BuffWorld: `AddBuffToTeam` walks the commander's
// characters, which for this zone is the session's own team. A monster has no team.
func (w *buffWorld) TeamBuffTargets(handle uint32) []uint32 {
	if w.sess == nil || w.sessionIndex(handle) < 0 {
		return nil
	}
	return w.sess.teamHandles()
}

// sessionIndex answers the slot of `handle` in this session, or -1. A world built
// with no session (a monster's own ENTER while nobody owns the run) answers -1.
func (w *buffWorld) sessionIndex(handle uint32) int {
	if w.sess == nil {
		return -1
	}
	return w.sess.teamIndex(handle)
}

// monsterOnField reports whether `handle` names a monster body this zone is running.
func (h *Handler) monsterOnField(handle uint32) bool {
	_, ok := h.monsterByHandle(handle)
	return ok
}

// removeMonsterBuff drops one buff from a monster's list, broadcasts the whole bar
// and answers whether it was there. There is no database half (this build persists
// no buff) and no 3125 (a monster owns no client).
func (h *Handler) removeMonsterBuff(handle uint32, name string) (bool, string) {
	l := h.liveMonsterBuffs(handle)
	if l == nil || !l.Remove(name) {
		return false, "not applied"
	}
	h.broadcastWorld(BuildChangeBuff(handle, l.Mask()))
	h.Log.Printf("zone: BUFF REMOVE monster %d %q: the bit is cleared and the whole bar "+
		"resent; the list is now %v", handle, name, l.Names())
	return true, ""
}

// script.BuffWorld is satisfied by *buffWorld; the interface assertion lives here so
// a change in the script package breaks this file at compile time, not at run time.
var _ script.BuffWorld = (*buffWorld)(nil)
