// THE SKILL'S BUFF LANDS THE WAY THE GAME'S OWN SCRIPTS SAY IT DOES.
//
// THE CHAIN, AND WHO OWNS WHICH STEP (all quoted in research\item-buff-skill.md §3):
//
//	1. the row's `Script` column names SCR_SKILL_BUFF
//	       ge\script\skill\buff.scp:2
//	2. that script fills a MAGICAL argument bag with Buff/BuffType/BuffFreq/BuffID
//	       buff.scp:36-40, and sends it:
//	       SendMsg(self, target, 'OM_SKILL_MAGICAL_ASSIST', magicalskillarg, ret)  :143
//	3. the TARGET's own message script runs (script\skillmsg.go, Object.cpp:37-43)
//	       SCR_MSG_PC_SKILL_MAGICAL_ASSIST   pc\common_pc.scp:5794
//	       SCR_MSG_MON_SKILL_MAGICAL_ASSIST  monster\common_monster.scp:8476
//	4. SCR_RESULT_BATTLEBUFF_SKILL decides (rank, resist, BuffType)  common_pc.scp:6727
//	5. AddBuff(self, arg.Buff, SklLv, rankdiff, 0)                  common_pc.scp:6903
//	       -> the zone's buff list (zone\buff.go)
//
// NOT ONE OF THOSE DECISIONS IS TAKEN HERE. This file resolves the LIVE TARGET (the
// handle the script named is an actor on this field, not a number), hands the script
// the caster's real numbers, and reports what the script decided - nothing else. A
// rank, a resist or a chance computed in Go would be a second implementation of the
// rule the corpus already states.
package zone

import (
	"fmt"

	"geserver/script"
)

// assistObjectType is the reference's `GetObjTypeName()` for the two kinds of actor a
// buff message can land on - the word that selects the message script
// (PlayerChar.h:262 `"PC"`, MonsterNPC.h:157 `"MON"`).
const (
	assistTypePC      = "PC"
	assistTypeMonster = "MON"
)

// SkillBuffEntry is the `Script` column value this lane runs: 714 of the 5,626 rows
// in datatable_skill.xml name it (research\item-buff-skill.md §3). A row that names it
// runs THIS entry point instead of the damage formula, because that is what the
// reference's ProcessSkill does with the column (FieldChar_Skill.cpp:65-67).
const SkillBuffEntry = "SCR_SKILL_BUFF"

// assistWorld implements script.AssistWorld for this zone: it turns the handle a
// script named into the actor the reference would push at the message script.
//
// IT IS INSTALLED FOR ONE RUN (`script.SkillBuffCall.Assist`), because a message may
// only be delivered while a cast is being processed.
type assistWorld struct{ h *Handler }

// AssistActor resolves one handle: a monster body on this field, or a player
// character in any session of the zone. Anything else is not an actor, and the
// script package says so once rather than applying a buff to nobody.
func (a *assistWorld) AssistActor(handle uint32) (script.AssistActor, bool) {
	if a == nil || a.h == nil {
		return script.AssistActor{}, false
	}
	if m, ok := a.h.monsterByHandle(handle); ok {
		self := monsterStatusSelf(m)
		// THE HANDLE IS WHAT EVERY BUFF BUILTIN TRUSTS: `AddBuff(self, ...)` gets its
		// target from this field (`__status_id`, script\status.go), so a `self` without
		// it would apply the buff to handle 0 - nobody - and the rule would look like
		// it refused.
		self.Handle = m.Handle
		return script.AssistActor{
			Type: assistTypeMonster,
			Self: self,
			// A monster has no Character Info source: the message script that runs for
			// one (`SCR_MSG_MON_*`) reads its numbers off `self`, which is the row.
			Source: nil,
		}, true
	}
	if actor, ok := a.h.playerAssistActor(handle); ok {
		return actor, true
	}
	return script.AssistActor{}, false
}

// playerAssistActor finds a player character by handle across the zone's sessions and
// builds the `self` table its message script reads.
//
// THE NUMBERS ARE THE PANEL'S, from the game's own Character Info script
// (`h.sheetFor`): `self.DR`, `self.RSTAT`, `self.DEF` and `self.AR` are four of the
// terms `SCR_RESULT_BATTLEBUFF_SKILL` decides on (common_pc.scp:6752-6768), and they
// must be the same numbers the owner's screen shows - not a second derivation.
func (h *Handler) playerAssistActor(handle uint32) (script.AssistActor, bool) {
	for _, m := range h.Players.Snapshot() {
		for i := range m.Chars {
			c := m.Chars[i]
			if c.Handle != handle {
				continue
			}
			self := statusSelfFor(c)
			sheet := h.sheetFor(c)
			self.Props["AR"] = sheet.AR
			self.Props["DR"] = sheet.DR
			self.Props["DEF"] = sheet.DEF
			self.Props["RSTAT"] = sheet.RSTAT
			self.Props["MHP"] = sheet.MHP
			self.Props["MSP"] = sheet.MSP
			var src script.StatusSource
			if h.Status != nil {
				src = &statusRun{script: h.Status, c: c}
			}
			return script.AssistActor{Type: assistTypePC, Self: self, Source: src}, true
		}
	}
	return script.AssistActor{}, false
}

// messageSelfFor is the CASTER's `self` for a buff script run: the Character Info
// inputs plus the four derived numbers the script copies into the bag
// (`magicalskillarg.AR = self.AR`, buff.scp:37) and the message rule reads back.
//
// THE `_BM` TERMS ARE 0 BECAUSE THIS BUILD HOLDS NONE, which is the same answer
// statusSelfFor gives and the same one the reference gives for an unset property.
func (h *Handler) messageSelfFor(c Character) script.StatusSelf {
	self := statusSelfFor(c)
	sheet := h.sheetFor(c)
	self.Props["AR"] = sheet.AR
	self.Props["DR"] = sheet.DR
	self.Props["DEF"] = sheet.DEF
	self.Props["RSTAT"] = sheet.RSTAT
	self.Props["MHP"] = sheet.MHP
	self.Props["MSP"] = sheet.MSP
	return self
}

// skillBuffTarget is the BODY a buff cast is aimed at, as the packet path knows it:
// the handle the script will name in `SendMsg`, the table the skill script reads as
// its `target` argument, and the words the log uses.
type skillBuffTarget struct {
	handle uint32
	table  map[string]any
	words  string
}

// monsterBuffTarget is the ordinary cast: a monster body the client named.
func monsterBuffTarget(h *Handler, m *Monster) skillBuffTarget {
	if m == nil {
		return skillBuffTarget{table: map[string]any{}, words: "nobody"}
	}
	return skillBuffTarget{
		handle: m.Handle,
		table:  skillScriptTarget(m),
		words:  fmt.Sprintf("monster %d (%q)", m.Handle, h.MonsterName(*m)),
	}
}

// characterBuffTarget is a cast aimed at a CHARACTER - the caster itself for the
// SELF family (`SendMsg(self, self, ...)`, ge\script\skill\extraskill.scp:784). The
// packet lane does not build a SELF cast yet, so this is reached by the buff script's
// own target table today; it is here so the two kinds of target are one call.
func characterBuffTarget(c Character) skillBuffTarget {
	return skillBuffTarget{
		handle: c.Handle,
		table: map[string]any{
			"Handle":    float64(c.Handle),
			"ClassID":   float64(c.Job),
			"ClassName": "PC",
			"Lv":        float64(c.Level),
			"HP":        float64(c.HP),
			"MHP":       float64(c.MaxHP),
		},
		words: fmt.Sprintf("char %d (%q)", c.Handle, c.Name),
	}
}

// skillScriptBuff runs ONE landed cast's own buff script and answers the packets the
// buff rules produced (3556 / 3125, and anything a nested ENTER added).
//
// IT IS CALLED WHERE THE REFERENCE RUNS ProcessSkill (FieldChar_Skill.cpp:49-75): at
// the result of the cast, with the target the client named. A cast whose row names no
// buff script never reaches this function.
func (h *Handler) skillScriptBuff(sess *Session, c Character, row SkillRow,
	target skillBuffTarget, splash bool, targetCount int) (out [][]byte) {

	if h == nil || h.BuffScript == nil {
		h.Log.Printf("zone: SKILL BUFF refused: skill %d (%q) is dispatched to %s by its "+
			"own row, and this zone has no buff script source armed - nothing landed",
			row.ID, h.SkillName(row.ID), row.Script)
		return nil
	}

	// THE WORLD SAYS "A SCRIPT IS RUNNING", because every buff it grants is granted FROM
	// ONE: the message script calls `AddBuff` while RunSkill holds the runner's lock, so
	// a nested ENTER must not try to take that lock again on this goroutine.
	world := &buffWorld{h: h, sess: sess, inScript: true}
	targetHandle := target.handle
	targetWords := target.words
	// The state BEFORE the rule runs, so "it was already applied" can be told from
	// "the rule refused it" - both are refusals, and they are different answers.
	alreadyApplied := targetHandle != 0 && row.Buff != "" &&
		world.IsBuffApplied(targetHandle, row.Buff)

	call := script.SkillBuffCall{
		Entry:       row.Script,
		Source:      h.BuffScript.source(c),
		Self:        h.messageSelfFor(c),
		Skill:       skillScriptRow(row),
		Target:      target.table,
		Splash:      splash,
		TargetCount: targetCount,
		World:       world,
		Assist:      &assistWorld{h: h},
	}
	res, err := h.BuffScript.RunSkill(call)
	out = append(out, world.out...)

	// WHAT THE SCRIPT DECIDED, read off the bag it filled - never off the row: the
	// rule rewrites `arg.Buff` on several branches (`MindDrain` -> 'Fear',
	// common_pc.scp:6766-6774), so the row's column is not the answer.
	buff := argStringField(res.Arg, "Buff")
	buffType := argStringField(res.Arg, "BuffType")
	buffFreq := argNumberField(res.Arg, "BuffFreq")
	buffID := argNumberField(res.Arg, "BuffID")

	switch {
	case err != nil:
		h.Log.Printf("zone: SKILL BUFF refused: skill %d (%q) char %d (%q) -> %s - its "+
			"own script RAISED: %v (the rule is the script's, this server computes no "+
			"rank, resist or chance)", row.ID, h.SkillName(row.ID), c.Handle, c.Name,
			targetWords, err)
		return out
	case !res.Ran:
		h.Log.Printf("zone: SKILL BUFF refused: skill %d (%q) char %d (%q) -> %s - %s "+
			"did not run (the corpus defines no such entry point), so no buff could land",
			row.ID, h.SkillName(row.ID), c.Handle, c.Name, targetWords, row.Script)
		return out
	case res.Arg == nil:
		h.Log.Printf("zone: SKILL BUFF refused: skill %d (%q) char %d (%q) -> %s - the "+
			"script ran and never asked for its argument bag (GetMagicalSkillArg), so it "+
			"composed no buff to send", row.ID, h.SkillName(row.ID), c.Handle, c.Name,
			targetWords)
		return out
	case buff == "":
		h.Log.Printf("zone: SKILL BUFF: skill %d (%q) char %d (%q) -> %s - the bag names "+
			"no Buff (the row's own Buff column is %q), so there was nothing to land",
			row.ID, h.SkillName(row.ID), c.Handle, c.Name, targetWords, row.Buff)
		return out
	}

	if targetHandle != 0 && world.IsBuffApplied(targetHandle, buff) {
		h.Log.Printf("zone: SKILL BUFF skill %d (%q) char %d (%q) -> %s: the bag says "+
			"Buff %q BuffType %q BuffFreq %g BuffID %g; %q is APPLIED on the target",
			row.ID, h.SkillName(row.ID), c.Handle, c.Name, targetWords,
			buff, buffType, buffFreq, buffID, buff)
		return out
	}

	reason := "the message script ran and its own rule did not add it"
	if alreadyApplied {
		reason = "the buff was ALREADY applied and BuffType is not FORCE " +
			"(common_pc.scp:6895)"
	} else if targetHandle == 0 {
		reason = "the cast recorded no target, so there was no defender to run the rule on"
	}
	h.Log.Printf("zone: SKILL BUFF refused: skill %d (%q) char %d (%q) -> %s, buff %q "+
		"(BuffType %q, BuffFreq %g, BuffID %g) - %s; the rule is the target's own "+
		"message script's and this server computes no rank, resist or chance",
		row.ID, h.SkillName(row.ID), c.Handle, c.Name, targetWords, buff,
		buffType, buffFreq, buffID, reason)
	return out
}

// argStringField reads one string field out of a script bag, "" when absent.
func argStringField(arg map[string]any, name string) string {
	if arg == nil {
		return ""
	}
	if s, ok := arg[name].(string); ok {
		return s
	}
	return ""
}

// argNumberField reads one number field out of a script bag, 0 when absent.
func argNumberField(arg map[string]any, name string) float64 {
	if arg == nil {
		return 0
	}
	if n, ok := arg[name].(float64); ok {
		return n
	}
	return 0
}
