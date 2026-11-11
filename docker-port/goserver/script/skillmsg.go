// THE SCRIPT MESSAGE AN ARGUMENT BAG TRAVELS IN - `OM_SKILL_MAGICAL_ASSIST`.
//
// WHY THIS FILE EXISTS. A skill does not apply its buff. `SCR_SKILL_BUFF` fills a
// magical-argument bag and hands it to the TARGET:
//
//	SendMsg(self, target, 'OM_SKILL_MAGICAL_ASSIST', magicalskillarg, ret)
//	    ge\script\skill\buff.scp:143  (the bag: :36-40 Buff/BuffType/BuffFreq/BuffID)
//
// The reference's `CFieldChar::OnSkillMagicalAssist`
// (source2\geServer\ZServerLib\FieldChar_Message.cpp:489) then runs the TARGET's own
// script message handler:
//
//	CObject::ProcessScriptMsg(from, "SKILL_MAGICAL_ASSIST", arg1, arg2, ...)
//	    -> imc::StrFormat(funcName, "SCR_MSG_%s_%s", GetObjTypeName(), typeName)
//	    source2\geServer\ZServerLib\Object.cpp:36-43
//
// so a player character's handler is `SCR_MSG_PC_SKILL_MAGICAL_ASSIST`
// (ge\script\pc\common_pc.scp:5794) and a monster's is
// `SCR_MSG_MON_SKILL_MAGICAL_ASSIST` (ge\script\monster\common_monster.scp:8476).
// `GetObjTypeName()` is "PC" for a player character (PlayerChar.h:262) and "MON" for
// a monster (MonsterNPC.h:157) - the same two words this file builds the name from.
//
// THE NAME IS BUILT, NEVER CHOSEN. Nothing here decides which script runs, whether a
// buff lands or what a rank is: `SCR_RESULT_BATTLEBUFF_SKILL` (common_pc.scp:6727)
// decides that and calls `AddBuff(self, arg.Buff, SklLv, rankdiff, 0)` (:6903),
// which is the zone's list, reached through the buff builtins in script\buff.go.
//
// THE TABLES ARE THE CALLER'S OWN. `from` is the `self` table of the script that
// sent the message and `ret` is the table it passed, so a write the message script
// makes (`ret.BuffSucc = 1`, common_pc.scp:5797) is visible to the sender after
// `SendMsg` returns - which is exactly how the reference behaves, because both are
// the same IES objects.
package script

import (
	"context"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// AssistActor is one live actor a script message can land on.
//
// Type is the reference's `GetObjTypeName()` - "PC" or "MON" - and it is the word
// that selects the message script. Self is the `self` table that script reads, built
// by the ZONE from the actor's own data (`monsterStatusSelf`, `statusSelfFor`), and
// Source is the status source its builtins answer from. NIL Source is legal and means
// "no source wired": every data-table read then answers its empty value and says so
// once, which is the same contract every other script run has.
type AssistActor struct {
	Type   string
	Self   StatusSelf
	Source StatusSource
}

// AssistWorld is the live world behind `OM_SKILL_MAGICAL_ASSIST`: the zone's answer
// to "which actor is this handle, and what does its own `self` look like".
//
// It is installed for the length of ONE skill-buff run (`RunSkillBuff`) because a
// message may only be delivered while a cast is being processed. NIL is legal: the
// message is then refused with one line and nothing is applied.
type AssistWorld interface {
	AssistActor(handle uint32) (AssistActor, bool)
}

// assistMsgType is the message type `SendMsg` is served with, spelled as the corpus
// spells it (`'OM_SKILL_MAGICAL_ASSIST'`, buff.scp:143). The reference strips the
// `OM_` prefix before it builds the script name (Object.cpp:38-40), so the entry is
// `SCR_MSG_<TYPE>_SKILL_MAGICAL_ASSIST` - see assistEntry.
const assistMsgType = "SKILL_MAGICAL_ASSIST"

// scriptMsgHelpers are the corpus entry points a message script CALLS but that no
// eagerly loaded library defines. They are loaded by NAME through the index, never by
// a path, exactly like the damage formula's own helpers (zone\skill_script.go).
//
//	SCR_RESULT_BATTLEBUFF_SKILL   pc\common_pc.lua:6727 - the rule that decides
//	                              whether a buff lands. BOTH message handlers call
//	                              it (common_pc.scp:5837, common_monster.scp:8500),
//	                              and the monster handler's file does not define it.
//	                              Its own sibling SCR_RESULT_BATTLEBUFF_SKILL_CUSTOM
//	                              lives in the same file, so loading either loads both.
//	SCR_GET_BATTLE_POSITIVENUM    pc\common_pc.lua:5983 - the clamp both callers
//	                              apply to a level difference.
var scriptMsgHelpers = []string{"SCR_RESULT_BATTLEBUFF_SKILL", "SCR_GET_BATTLE_POSITIVENUM"}

// assistEntry builds the entry point name the reference builds:
// `SCR_MSG_<GetObjTypeName()>_<type>` (Object.cpp:37-40).
func assistEntry(objType, msgType string) string {
	return "SCR_MSG_" + objType + "_" + msgType
}

// withStatusFrom installs the SENDER's status source for one message run and puts
// back whatever was there. It is separate from withStatus because a message has TWO
// actors: `status` is the receiver (`self`), `statusFrom` is the sender (`from`).
func (h *Host) withStatusFrom(src StatusSource) func() {
	prev := h.statusFrom
	h.statusFrom = src
	return func() { h.statusFrom = prev }
}

// skillMagicalAssist delivers one `OM_SKILL_MAGICAL_ASSIST`.
//
// The arguments are the reference's own: `SendMsg(from, to, msg, arg1, arg2)`, where
// arg1 is the magical-argument bag and arg2 the `ret` table. `from` (argument 1) is
// reused as the message script's `from` - the sender's own `self` table, the same
// object the outer script was handed - and the receiver's `self` is built from the
// live actor, because that is the object the reference pushes
// (`RunMainScript`: `PushObject(from->GetIESObject()); PushObject(this->...)`).
//
// EVERY REFUSAL IS ONE LINE AND NOTHING SENT. A message that cannot be delivered -
// no world, a handle no actor owns, a type the corpus has no handler for, a script
// that raised - says so once and changes nothing. A message this host never learned
// still falls to reportUnhandledMessage, so the next slice is discovered from the log.
func (h *Host) skillMagicalAssist(L *lua.LState, handle uint32) {
	from, ok := L.Get(1).(*lua.LTable)
	if !ok {
		h.reportOnce("assist:nofrom", "script: SendMsg(%q) needs the SENDER's self table "+
			"as argument 1, got %s - nothing was sent", assistMsgType, luaTypeName(L.Get(1)))
		return
	}
	arg, ok := L.Get(4).(*lua.LTable)
	if !ok {
		h.reportOnce("assist:noarg", "script: SendMsg(%q) needs the magical-argument bag "+
			"as argument 4, got %s - the message was not sent, so no buff can land",
			assistMsgType, luaTypeName(L.Get(4)))
		return
	}
	ret, ok := L.Get(5).(*lua.LTable)
	if !ok {
		// The reference always passes its CSkillResult. A script that passes something
		// else still gets a handler run (the buff is the point), but the sender will
		// not see the writes - say so once rather than pretending they stick.
		h.reportOnce("assist:noret", "script: SendMsg(%q) was given %s as argument 5, "+
			"where the reference passes its result table - the message runs against a "+
			"fresh table and the sender will not see its writes",
			assistMsgType, luaTypeName(L.Get(5)))
		ret = bag(L)
	}

	if h.assist == nil {
		h.reportOnce("assist:noworld", "script: SendMsg(%q) to handle 0x%08x - no live "+
			"world is wired for this run, so the target's message script cannot run and "+
			"no buff lands", assistMsgType, handle)
		return
	}
	actor, ok := h.assist.AssistActor(handle)
	if !ok {
		h.reportOnce(fmt.Sprintf("assist:unknown:0x%08x", handle),
			"script: SendMsg(%q) to handle 0x%08x - that handle is not an actor this "+
				"zone is running, so there is no message script to run and nothing landed",
			assistMsgType, handle)
		return
	}
	if actor.Type == "" {
		h.reportOnce("assist:notype", "script: SendMsg(%q) to handle 0x%08x - the actor "+
			"did not name its object type, so the message script's name cannot be built "+
			"(SCR_MSG_<TYPE>_%s) and nothing landed", assistMsgType, handle, assistMsgType)
		return
	}

	entry := assistEntry(actor.Type, assistMsgType)
	if !h.HasScript(entry) {
		h.reportOnce("assist:noentry:"+entry, "script: SendMsg(%q) to handle 0x%08x runs "+
			"%s (the reference's SCR_MSG_<GetObjTypeName()>_<type>, Object.cpp:37-43), and "+
			"the corpus defines no such function - nothing landed",
			assistMsgType, handle, entry)
		return
	}
	if _, err := h.EnsureScript(entry); err != nil {
		h.reportOnce("assist:loadfailed:"+entry, "script: SendMsg(%q) to handle 0x%08x: %s "+
			"did not load (%v) - nothing landed", assistMsgType, handle, entry, err)
		return
	}
	for _, helper := range scriptMsgHelpers {
		if _, err := h.EnsureScript(helper); err != nil {
			h.reportOnce("assist:helper:"+helper, "script: %s is not loadable (%v) - the "+
				"message script calls it, and without it the rule that decides whether the "+
				"buff lands cannot answer", helper, err)
		}
	}

	// The receiver's own `self` and the builtins it reads. `withStatus` is
	// save-and-restore, so the outer skill run's caster is put back on the way out.
	defer h.withStatus(actor.Source, actor.Self)()
	defer h.withStatusFrom(h.status)()

	self := h.statusSelfTable(actor.Self)
	fn, ok := h.L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		h.reportOnce("assist:notafunction:"+entry, "script: %s is not a function after its "+
			"file loaded - nothing landed", entry)
		return
	}

	h.ticks = 0
	ctx, cancel := context.WithTimeout(context.Background(), SkillTimeout)
	defer cancel()
	// THE GUARD, because this run happens INSIDE the skill script's own Lua call: a
	// raw RemoveContext here would take the OUTER run's deadline away (the crash
	// withContext's own comment records).
	defer h.withContext(ctx)()
	if err := h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		from, self, arg, ret); err != nil {
		h.reportOnce("assist:failed:"+entry, "script: SendMsg(%q) to handle 0x%08x ran %s "+
			"and it RAISED: %v", assistMsgType, handle, entry, err)
	}
}
