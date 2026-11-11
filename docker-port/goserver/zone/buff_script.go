// THE SCRIPT HALF OF A BUFF: `SCR_BUFF_ENTER_<ClassName>`, run for real.
//
// WHY THE DURATION IS NOT IN THE TABLE. `datatable_buff.xml` says `ApplyTime="0"` on
// 1,954 of its 1,962 rows; the buff's own ENTER script writes the real number
// (`buff.ApplyTime = 30000`, ge\script\buff\buff.scp:24; `= skilllv * 20000`, :44;
// `= 60000`, :33781). So a buff list that reads only the table has no durations, and
// a NORMAL buff would expire on the first tick. This file is what makes the number
// exist: it runs the entry point and reads `buff.ApplyTime` back off the same table
// the script wrote it into.
//
// THE CALL IS THE REFERENCE'S OWN (`CBuff::OnEnter`, Buff.cpp:66-90):
//
//	context->PushObject(owner's IES object)   -> `self`
//	context->PushObject(buff's IES object)    -> `buff`
//	context->PushFloat(arg1, arg2, arg3)      -> `skilllv`, `arg2`, `AddAppTime`
//	context->Run(enterFuncName)
//	ApplyBMProperties()                       -> the *_BM terms, carried here
//	if (additionalTime) SetApplyTime(... + additionalTime)
//
// The corpus signature every ENTER uses is
// `func SCR_BUFF_ENTER_<Name>(self, buff, skilllv, arg2, AddAppTime)`
// (ge\script\buff\buff.scp:22-26 is a whole one: two assignments and nothing else).
//
// THE MISSING-BUILTIN LIST IS PART OF THE DELIVERABLE, the same way it is for the
// damage formula (zone\skill_script.go): the research measured 17 of 1,305 ENTER
// definitions that need a builtin this build does not bind (`GetMccJob` 8,
// `SetBlind` 2, `SetAttackAnyOne` 2, `AddHP` 2, `ShowDigit` 2, five singletons), and
// the only way to know WHICH buffs those are is to let the recorder name them on the
// first run and print the diff once.
package zone

import (
	"errors"
	"fmt"

	"strings"
	"sync"

	"geserver/script"
)

// buffMissingReportCap is how many missing names one line prints - the same shape
// and the same reason as the damage script's cap (a list a reader can read; the
// count at the end keeps it complete).
const buffMissingReportCap = 40

// BuffScript is the buff corpus, run on the zone's one script host.
//
// NIL MEANS "NO CORPUS": every buff is then scriptless, which is a legitimate
// checkout state and is exactly what the refusal rule is for - a NORMAL buff with no
// duration is refused by name, never granted with 0.
type BuffScript struct {
	Host *script.Host
	// Sheet is the zone's Character Info source, handed to every ENTER run so the
	// builtins the buff scripts call (`GetEquipItem`, `GetCurrentStance`, the
	// GetDataTable* reads, GetPromotionLevel) answer the same numbers they answer on
	// the panel. NIL is legal and means "nothing known": each of those then answers
	// its empty value and says so once.
	Sheet *StatusScript
	Log   func(format string, args ...any)

	// runMu serialises runs: the host is not safe for concurrent use and two
	// sessions can use an item in the same instant.
	runMu sync.Mutex
	// missingOnce prints the worklist after the FIRST ENTER that ran.
	missingOnce sync.Once
	// skillMissingOnce prints the worklist after the FIRST skill-buff run. It is a
	// SECOND gate on purpose: the ENTER worklist is printed once per process and a
	// skill's names would otherwise ride into whichever list printed first.
	skillMissingOnce sync.Once
}

// NewBuffScript arms the buff corpus. It does not load any buff file up front: the
// entry points are named per buff and the host loads a file on first use (that is
// what `EnsureScript` is for), so the first buff of a kind pays for its file and
// every later one is a map lookup.
func NewBuffScript(host *script.Host, logf func(string, ...any)) (*BuffScript, error) {
	if host == nil {
		return nil, errors.New("no script host (this zone was started without one)")
	}
	return &BuffScript{Host: host, Log: logf}, nil
}

// Has answers whether the corpus defines this entry point. It is the INDEX's answer
// (`Host.HasScript`), not a count kept in a comment.
func (b *BuffScript) Has(entry string) bool {
	if b == nil || b.Host == nil || entry == "" {
		return false
	}
	return b.Host.HasScript(entry)
}

// logf writes one line, or nothing when no logger is wired (a unit test).
func (b *BuffScript) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log(format, args...)
	}
}

// Enter runs ONE buff's ENTER entry, taking the runner's lock - the OUTERMOST entry
// (an item script, the tick, a direct call).
//
// THE LOCK IS NOT RE-ENTRANT, AND AN ENTER MAY NEST (a buff script that calls
// `AddBuff` on another buff, which is legal in the reference: `CBuffList::Add` runs
// `OnEnter` with the list in place). So a run started from INSIDE a script calls
// enterLocked instead - `Handler.addBuff` decides, and the world it hands a nested add
// says so (`buffWorld.inScript`). Without that split the item path deadlocks on
// itself: the item's script holds the lock and its `AddBuffImm` asks for it again.
func (b *BuffScript) Enter(entry string, c Character, row map[string]any,
	arg1, arg2, arg3 float64, w script.BuffWorld) (script.BuffEnterResult, error) {

	if b == nil {
		return script.BuffEnterResult{}, errors.New("no buff script source")
	}
	b.runMu.Lock()
	defer b.runMu.Unlock()
	return b.enterActorLocked(entry, statusSelfFor(c), b.source(c), row, arg1, arg2, arg3, w)
}

// EnterActor is Enter for an owner that is not a window into a Session - a MONSTER
// body. The reference's monster is a CFieldChar and runs the same ENTER entry
// (`CBuff::OnEnter` pushes the owner's IES object, Buff.cpp:71), so the only thing
// that changes is where `self` comes from: `monsterStatusSelf` for a body.
func (b *BuffScript) EnterActor(entry string, self script.StatusSelf, src script.StatusSource,
	row map[string]any, arg1, arg2, arg3 float64, w script.BuffWorld) (script.BuffEnterResult, error) {

	if b == nil {
		return script.BuffEnterResult{}, errors.New("no buff script source")
	}
	b.runMu.Lock()
	defer b.runMu.Unlock()
	return b.enterActorLocked(entry, self, src, row, arg1, arg2, arg3, w)
}

// enterLocked is the body of Enter, WITHOUT the lock: it is what a nested run calls,
// because the outermost run on this goroutine already holds it.
func (b *BuffScript) enterLocked(entry string, c Character, row map[string]any,
	arg1, arg2, arg3 float64, w script.BuffWorld) (script.BuffEnterResult, error) {

	return b.enterActorLocked(entry, statusSelfFor(c), b.source(c), row, arg1, arg2, arg3, w)
}

// enterActorLocked is the body of EnterActor, WITHOUT the lock: a nested run (a buff
// script that adds another buff) already holds it on this goroutine.
func (b *BuffScript) enterActorLocked(entry string, self script.StatusSelf, src script.StatusSource,
	row map[string]any, arg1, arg2, arg3 float64, w script.BuffWorld) (script.BuffEnterResult, error) {

	var res script.BuffEnterResult
	if b == nil || b.Host == nil {
		return res, errors.New("no script host")
	}
	if !b.Has(entry) {
		return res, nil
	}

	call := script.BuffEnterCall{
		Entry:  entry,
		Source: src,
		Self:   self,
		Row:    row,
		Arg1:   arg1, Arg2: arg2, Arg3: arg3,
		World: w,
	}
	before := b.Host.Missing()

	var raised any
	var err error
	func() {
		// A panic in the Lua binding must not take the zone's reply down with it.
		defer func() { raised = recover() }()
		res, err = b.Host.RunBuffEnter(call)
	}()

	b.reportMissingOnce(before, b.Host.Missing())

	if raised != nil {
		return script.BuffEnterResult{}, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	return res, err
}

// RunSkill runs a SKILL's own buff script - `SCR_SKILL_BUFF` - with the live target
// behind it, and answers what the script left in the argument bag.
//
// IT TAKES THE RUNNER'S LOCK, like Enter and RunItem: the host is not safe for
// concurrent use, and a cast can arrive on any session's goroutine. The lock is what
// also serialises every monster buff list mutation (see Handler.monsterBuffs).
func (b *BuffScript) RunSkill(c script.SkillBuffCall) (script.SkillBuffResult, error) {
	var res script.SkillBuffResult
	if b == nil || b.Host == nil {
		return res, errors.New("no script host")
	}
	before := b.Host.Missing()
	var raised any
	var err error
	b.runMu.Lock()
	func() {
		defer func() { raised = recover() }()
		res, err = b.Host.RunSkillBuff(c)
	}()
	b.runMu.Unlock()

	b.reportSkillMissingOnce(before, b.Host.Missing())
	if raised != nil {
		return script.SkillBuffResult{}, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	return res, err
}

// RunItem runs an ITEM's own `ScpType` (the reference's
// `context->Run(itemProperty->scpType)`, PlayerChar.cpp:2250) with the four
// arguments it pushes for an item use: self, the row's BuffName, arg1 and arg2.
//
// returned=false means this corpus does not define that script, so the caller keeps
// the behaviour it had. It is recorded for the item's 3556/3125 packets by the
// world the script's `AddBuff` calls land in.
func (b *BuffScript) RunItem(entry string, c Character, buffName string,
	arg1, arg2 float64, w script.BuffWorld) (bool, error) {

	if b == nil || b.Host == nil {
		return false, errors.New("no script host")
	}
	if !b.Has(entry) {
		return false, nil
	}
	before := b.Host.Missing()
	var raised any
	var ran bool
	var err error
	b.runMu.Lock()
	func() {
		defer func() { raised = recover() }()
		ran, err = b.Host.RunItemBuffScript(entry, b.source(c), statusSelfFor(c),
			buffName, arg1, arg2, w)
	}()
	b.runMu.Unlock()

	b.reportMissingOnce(before, b.Host.Missing())
	if raised != nil {
		return ran, fmt.Errorf("the Lua binding panicked: %v", raised)
	}
	return ran, err
}

// source is the status source the ENTER's builtins read, for THIS character. A nil
// Sheet answers nil, which the status builtins treat as "no source wired".
func (b *BuffScript) source(c Character) script.StatusSource {
	if b.Sheet == nil {
		return nil
	}
	return &statusRun{script: b.Sheet, c: c}
}

// reportMissingOnce prints, ONCE per process, the builtins a buff script asked for
// and nothing answered - the worklist the next slice is chosen from.
func (b *BuffScript) reportMissingOnce(before, after []script.Missing) {
	b.missingOnce.Do(func() {
		asked := missingSince(before, after)
		if len(asked) == 0 {
			b.logf("zone: BUFF SCRIPT host: 0 missing builtin(s) - every name the ENTER " +
				"entry asked for had a binding")
			return
		}
		shown := asked
		left := 0
		if len(shown) > buffMissingReportCap {
			left = len(shown) - buffMissingReportCap
			shown = shown[:buffMissingReportCap]
		}
		parts := make([]string, 0, len(shown))
		for _, m := range shown {
			parts = append(parts, fmt.Sprintf("%s x%d", m.Name, m.Calls))
		}
		tail := ""
		if left > 0 {
			tail = fmt.Sprintf(", ... and %d more (not printed)", left)
		}
		b.logf("zone: BUFF SCRIPT host: %d missing builtin(s) that a buff ENTER asked for: %s%s",
			len(asked), strings.Join(parts, ", "), tail)
	})
}

// reportSkillMissingOnce prints, ONCE per process, the builtins ONE skill-buff cast
// asked for and nothing answered - the same worklist, kept separate so the skill
// lane's names are not lost to whichever run printed first.
func (b *BuffScript) reportSkillMissingOnce(before, after []script.Missing) {
	b.skillMissingOnce.Do(func() {
		asked := missingSince(before, after)
		if len(asked) == 0 {
			b.logf("zone: SKILL BUFF host: 0 missing builtin(s) - every name THIS cast " +
				"asked for had a binding")
			return
		}
		shown := asked
		left := 0
		if len(shown) > buffMissingReportCap {
			left = len(shown) - buffMissingReportCap
			shown = shown[:buffMissingReportCap]
		}
		parts := make([]string, 0, len(shown))
		for _, m := range shown {
			parts = append(parts, fmt.Sprintf("%s x%d", m.Name, m.Calls))
		}
		tail := ""
		if left > 0 {
			tail = fmt.Sprintf(", ... and %d more (not printed)", left)
		}
		b.logf("zone: SKILL BUFF host: %d missing builtin(s) that THIS cast asked for: %s%s",
			len(asked), strings.Join(parts, ", "), tail)
	})
}
