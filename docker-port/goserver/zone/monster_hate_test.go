// THE HATE TABLE'S OWN TESTS - the rules the zone\monster_hate.go header quotes, run
// against the reference's own answers (`source2\geServer\ZServerLib\HateList.cpp`).
//
// One test per rule, named for the rule: the top is the max, a tie keeps the first,
// a remove drops to the second, the points ADD (`info->m_point += point`, :42), the
// table is per body and dies with it, and the line is written once per CHANGE. The
// last test is the one the card asks for in Lua: the seven names, called with the
// arity `ge\script\monster\mon_ts_battle.scp` calls them with.
package zone

import (
	"bytes"
	"log"
	"strconv"
	"strings"
	"testing"

	"geserver/script"
)

// hateZone is the smallest world the hate table answers from: a logger and nothing
// else. Bodies and players are added by the helpers below, so each test names exactly
// what it needs.
func hateZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	return &Handler{Log: log.New(logs, "", 0)}, logs
}

// hateBody puts one monster on the field the way create.go does: a roster row plus a
// live HP. Nothing else about it matters to the hate table.
func hateBody(h *Handler, handle uint32) Monster {
	m := Monster{Handle: handle, ClassID: 11591, Level: 111, X: 10, Y: 20, Cell: 3}
	h.spawnedMonster = append(h.spawnedMonster, m)
	if h.monsterHP == nil {
		h.monsterHP = map[uint32]float64{}
	}
	h.monsterHP[handle] = 100
	return m
}

// hatePlayer joins one player character the way the zone's roster does, and answers
// its handle.
func hatePlayer(h *Handler, handle uint32, name string) uint32 {
	p := newPlayer(handle, name, [3]int32{100, 100, 1})
	h.Players.Add(p.sess)
	return p.sess.Team[0].Handle
}

// TestHateTopIsTheHigherScore is the first half of the pass condition: two attackers,
// and the one with more points is the target.
func TestHateTopIsTheHigherScore(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(7))
	ann := hatePlayer(h, 0x40000101, "Ann")
	bob := hatePlayer(h, 0x40000102, "Bob")

	h.InsertHate(mon.Handle, ann, 20)
	h.InsertHate(mon.Handle, bob, 50)

	top, ok := h.TopHate(mon.Handle)
	if !ok {
		t.Fatalf("body 0x%08x hates two characters and TopHate answered nobody", mon.Handle)
	}
	if top.Handle != bob {
		t.Fatalf("top hate is 0x%08x with %d points, want 0x%08x (50 > 20)",
			top.Handle, top.Points, bob)
	}
	if top.Points != 50 {
		t.Fatalf("the top's points read %d, want 50 - the total the answer was chosen by", top.Points)
	}
	if top.ClassName != "PC" {
		t.Fatalf("a player target's ClassName reads %q, want 'PC' - mon_ts_battle.scp:148 "+
			"compares it, and the property fallback would answer 0 for any other name",
			top.ClassName)
	}
}

// TestHateTieKeepsTheFirst pins the tie rule this card asks for: equal points keep the
// target that was hated FIRST, and it stays the answer on every re-read. The
// reference's own tie-break is the HATE_INFO pointer (`CLessPoint`, HateList.h:14-22),
// which is an allocation order and not a rule a port can copy - so the list's own order
// is the stand-in, and it must not drift.
func TestHateTieKeepsTheFirst(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(8))
	first := hatePlayer(h, 0x40000201, "First")
	second := hatePlayer(h, 0x40000202, "Second")

	h.InsertHate(mon.Handle, first, 30)
	h.InsertHate(mon.Handle, second, 30)

	for i := 0; i < 3; i++ {
		top, ok := h.TopHate(mon.Handle)
		if !ok {
			t.Fatalf("read %d: two targets at 30 points and TopHate answered nobody", i+1)
		}
		if top.Handle != first {
			t.Fatalf("read %d: the tie answered 0x%08x, want 0x%08x - the FIRST target "+
				"hated keeps the tie", i+1, top.Handle, first)
		}
		if top.Points != 30 {
			t.Fatalf("read %d: the tie's points read %d, want 30", i+1, top.Points)
		}
	}

	// A later point on the second target breaks the tie honestly rather than by order.
	h.InsertHate(mon.Handle, second, 1)
	top, _ := h.TopHate(mon.Handle)
	if top.Handle != second || top.Points != 31 {
		t.Fatalf("after +1 the top is 0x%08x at %d points, want 0x%08x at 31",
			top.Handle, top.Points, second)
	}
}

// TestRemoveHateDropsToTheSecond is the second half: a remove is a DELETE
// (`CHateList::Remove`, HateList.cpp:129-146), so the next-highest becomes the target
// instead of the removed one lingering at 0.
func TestRemoveHateDropsToTheSecond(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(9))
	ann := hatePlayer(h, 0x40000301, "Ann")
	bob := hatePlayer(h, 0x40000302, "Bob")

	h.InsertHate(mon.Handle, ann, 50)
	h.InsertHate(mon.Handle, bob, 20)

	h.RemoveHate(mon.Handle, ann)
	top, ok := h.TopHate(mon.Handle)
	if !ok || top.Handle != bob {
		t.Fatalf("after RemoveHate(top) the target is 0x%08x (ok=%v), want 0x%08x", top.Handle, ok, bob)
	}
	if top.Points != 20 {
		t.Fatalf("the second target's points read %d, want 20 - a remove must not move them", top.Points)
	}

	// And the removed one is GONE, not reset: a fresh +1 puts it back below, at 1.
	h.InsertHate(mon.Handle, ann, 1)
	top, _ = h.TopHate(mon.Handle)
	if top.Handle != bob {
		t.Fatalf("a re-inserted attacker at 1 point became the top - the remove did not "+
			"delete the entry (top 0x%08x)", top.Handle)
	}
}

// TestInsertHateAddsNotSets pins the reference's own arithmetic: `info->m_point +=
// point` (HateList.cpp:42). Two 20s are 40, and a NEGATIVE point subtracts because
// nothing in the reference clamps - only the script decides what a hit is worth.
func TestInsertHateAddsNotSets(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(10))
	ann := hatePlayer(h, 0x40000401, "Ann")

	h.InsertHate(mon.Handle, ann, 20)
	h.InsertHate(mon.Handle, ann, 20)
	top, _ := h.TopHate(mon.Handle)
	if top.Points != 40 {
		t.Fatalf("two inserts of 20 left %d points, want 40 - Update ADDS "+
			"(HateList.cpp:42), it does not set", top.Points)
	}

	h.InsertHate(mon.Handle, ann, -15)
	top, _ = h.TopHate(mon.Handle)
	if top.Points != 25 {
		t.Fatalf("-15 on 40 left %d points, want 25 - the reference clamps nothing", top.Points)
	}
}

// TestIsSameTargetFollowsSetTacticsTarget is the card's third line: the script stores
// the target it is fighting and compares the one it just chose against it
// (mon_ts_battle.scp:167). It is a comparison of TWO OBJECTS, so it needs no body and
// no world - the reference compares two CFieldChar pointers (MonsterNPCFunc.cpp:321-347).
func TestIsSameTargetFollowsSetTacticsTarget(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(11))
	ann := hatePlayer(h, 0x40000501, "Ann")
	bob := hatePlayer(h, 0x40000502, "Bob")

	h.InsertHate(mon.Handle, bob, 50)
	h.InsertHate(mon.Handle, ann, 20)

	top, ok := h.TopHate(mon.Handle)
	if !ok || top.Handle != bob {
		t.Fatalf("the top is 0x%08x (ok=%v), want 0x%08x", top.Handle, ok, bob)
	}
	// The script's own sequence: StartAttack + SetTacticsTarget(self, target).
	h.SetTacticsTarget(mon.Handle, top.Handle)
	last, ok := h.TacticsTarget(mon.Handle)
	if !ok || last.Handle != bob {
		t.Fatalf("GetTacticsTarget answered 0x%08x (ok=%v) after SetTacticsTarget(0x%08x)",
			last.Handle, ok, bob)
	}
	// The same target, twice: SetTacticsTarget is a no-op and the answer is stable.
	h.SetTacticsTarget(mon.Handle, bob)
	if again, _ := h.TacticsTarget(mon.Handle); again.Handle != bob {
		t.Fatalf("a repeated SetTacticsTarget changed the stored target to 0x%08x", again.Handle)
	}
	// The CLEAR form the script uses on `target == nil` (:157) takes the target away.
	h.SetTacticsTarget(mon.Handle, 0)
	if _, ok := h.TacticsTarget(mon.Handle); ok {
		t.Fatalf("SetTacticsTarget(self) with one argument did not clear the target")
	}
	// A dead character is refused as a tactics target, the reference's IsZombie guard
	// (MonsterNPCFunc.cpp:268-269).
	h.SetTacticsTarget(mon.Handle, ann)
	killPlayerForHate(h, ann)
	h.SetTacticsTarget(mon.Handle, ann)
	if _, ok := h.TacticsTarget(mon.Handle); ok {
		t.Fatalf("a body at 0 HP became the tactics target - the reference refuses it")
	}
}

// TestHateIsPerBodyAndDiesWithTheBody pins the lifetime: each handle has its OWN list,
// and the corpse's leave - which deletes the roster row (respawn.go:352-354) - takes
// the table with it. Nothing may inherit another body's hate.
func TestHateIsPerBodyAndDiesWithTheBody(t *testing.T) {
	h, logs := hateZone(t)
	first := hateBody(h, MonsterHandle(12))
	second := hateBody(h, MonsterHandle(13))
	ann := hatePlayer(h, 0x40000601, "Ann")
	bob := hatePlayer(h, 0x40000602, "Bob")

	h.InsertHate(first.Handle, ann, 50)
	h.InsertHate(second.Handle, bob, 50)

	if top, _ := h.TopHate(first.Handle); top.Handle != ann {
		t.Fatalf("body 0x%08x hates 0x%08x, want 0x%08x - the tables are not per body",
			first.Handle, top.Handle, ann)
	}
	if top, _ := h.TopHate(second.Handle); top.Handle != bob {
		t.Fatalf("body 0x%08x hates 0x%08x, want 0x%08x - the tables are not per body",
			second.Handle, top.Handle, bob)
	}

	// THE LEAVE, exactly as takeDueLeaves does it: the roster row and the HP entry go
	// in one locked step, and the hate table must be gone with them.
	h.spawnMu.Lock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == first.Handle {
			h.spawnedMonster = append(h.spawnedMonster[:i], h.spawnedMonster[i+1:]...)
			break
		}
	}
	delete(h.monsterHP, first.Handle)
	h.spawnMu.Unlock()

	if _, ok := h.TopHate(first.Handle); ok {
		t.Fatalf("a body that left the field still answers a top-hate target - its table "+
			"outlived it (logs: %s)", logs.String())
	}
	if _, ok := h.TopHate(second.Handle); !ok {
		t.Fatalf("the OTHER body's table died with its neighbour - the tables are not " +
			"keyed per handle")
	}
	if n := len(h.monsterHate); n != 1 {
		t.Fatalf("%d hate tables survive a one-body leave, want 1 (a table is pruned with "+
			"the roster row it belongs to)", n)
	}
}

// TestHateRefusesAHandleThatIsNotABody pins the other side of the lifetime: a script
// that asks about a handle this zone never made gets NO table (nothing to clean up
// later), and the refusal is said ONCE in the log rather than per call.
func TestHateRefusesAHandleThatIsNotABody(t *testing.T) {
	h, logs := hateZone(t)
	ann := hatePlayer(h, 0x40000701, "Ann")
	stranger := MonsterHandle(99)

	h.InsertHate(stranger, ann, 50)
	if _, ok := h.TopHate(stranger); ok {
		t.Fatalf("a handle that is not a body on this field answered a top-hate target")
	}
	if len(h.monsterHate) != 0 {
		t.Fatalf("%d hate tables were created for a body that does not exist", len(h.monsterHate))
	}
	h.InsertHate(stranger, ann, 50)
	if n := strings.Count(logs.String(), "zone: HATE InsertHate(0x"); n != 1 {
		t.Fatalf("the refusal was written %d times, want exactly 1 (once per zone)\n%s",
			n, logs.String())
	}
}

// TestHateTopChangeWritesOneLine is the card's fourth deliverable: a body's top-hate
// target changing is ONE log line naming both ends and the points, and a change that
// does not move the top writes nothing.
func TestHateTopChangeWritesOneLine(t *testing.T) {
	h, logs := hateZone(t)
	mon := hateBody(h, MonsterHandle(14))
	ann := hatePlayer(h, 0x40000801, "Ann")
	bob := hatePlayer(h, 0x40000802, "Bob")

	h.InsertHate(mon.Handle, ann, 20) // top: nobody -> Ann
	want := "top -> 0x40000801 \"Ann\" (20 points)"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the first hate insert did not write a top change\nwant: zone: HATE 0x... top -> %s\ngot:\n%s",
			want, logs.String())
	}
	lines := strings.Count(logs.String(), "zone: HATE ")

	h.InsertHate(mon.Handle, bob, 5) // top does NOT change
	if got := strings.Count(logs.String(), "zone: HATE "); got != lines {
		t.Fatalf("a smaller insert wrote %d extra HATE line(s); only a CHANGE may be written\n%s",
			got-lines, logs.String())
	}

	h.InsertHate(mon.Handle, bob, 40) // top: Ann -> Bob
	if want := "top -> 0x40000802 \"Bob\" (45 points)"; !strings.Contains(logs.String(), want) {
		t.Fatalf("the takeover was not written\nwant: %s\ngot:\n%s", want, logs.String())
	}

	h.RemoveHate(mon.Handle, bob) // top: Bob -> Ann
	if want := "top -> 0x40000801 \"Ann\" (20 points)"; !strings.Contains(logs.String(), want) {
		t.Fatalf("the fall-back after a remove was not written\nwant: %s\ngot:\n%s", want, logs.String())
	}
}

// TestDeadTargetIsNotTheTop pins the GarbageCollect half (HateList.cpp:69-71): an
// entry whose character reached 0 HP is dropped and the next one answers - and it does
// not come back when that character is healed, because the entry was collected.
func TestDeadTargetIsNotTheTop(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(15))
	ann := hatePlayer(h, 0x40000901, "Ann")
	bob := hatePlayer(h, 0x40000902, "Bob")

	h.InsertHate(mon.Handle, ann, 50)
	h.InsertHate(mon.Handle, bob, 20)

	killPlayerForHate(h, ann)
	top, ok := h.TopHate(mon.Handle)
	if !ok || top.Handle != bob {
		t.Fatalf("the top target is 0x%08x (ok=%v) with 0x%08x at 0 HP - a dead character "+
			"must not stay the target", top.Handle, ok, ann)
	}

	healPlayerForHate(h, ann, 120)
	top, _ = h.TopHate(mon.Handle)
	if top.Handle != bob {
		t.Fatalf("the collected target came back at 0x%08x after being healed - the entry "+
			"was not dropped", top.Handle)
	}
}

// TestIsAttackingFollowsTheActionState pins the gate the script reads before StartAttack
// (mon_ts_battle.scp:162): the answer is the body's OWN state, not "does it have a
// target". `markMonsterAttacking` is the writer stage 2's StartAttack/StopAttack call.
func TestIsAttackingFollowsTheActionState(t *testing.T) {
	h, _ := hateZone(t)
	mon := hateBody(h, MonsterHandle(16))
	ann := hatePlayer(h, 0x40000A01, "Ann")

	h.InsertHate(mon.Handle, ann, 50)
	if h.IsAttacking(mon.Handle) {
		t.Fatalf("a body that has never started an attack answered attacking")
	}
	h.markMonsterAttacking(mon.Handle, true)
	if !h.IsAttacking(mon.Handle) {
		t.Fatalf("markMonsterAttacking(true) did not reach IsAttacking")
	}
	h.markMonsterAttacking(mon.Handle, false)
	if h.IsAttacking(mon.Handle) {
		t.Fatalf("markMonsterAttacking(false) did not reach IsAttacking")
	}
}

// TestHateBuiltinsRunFromLuaWithTheScriptsArity is the pass condition's proof that the
// names are BOUND, not stubbed: a real gopher-lua runtime, a Handler behind it, and a
// snippet that calls each name with the arity `mon_ts_battle.scp` uses - including the
// 4th argument the census invented, which must do nothing.
func TestHateBuiltinsRunFromLuaWithTheScriptsArity(t *testing.T) {
	h, logs := hateZone(t)
	mon := hateBody(h, MonsterHandle(17))
	ann := hatePlayer(h, 0x40000B01, "Ann")
	bob := hatePlayer(h, 0x40000B02, "Bob")

	host := script.New()
	defer host.Close()
	host.Hate = h

	// The script's OWN calls, in its own order (mon_ts_battle.scp:5, :100-:177), with
	// handles written the way the zone's tables carry them.
	src := `
		local mon = { Handle = ` + luaNum(mon.Handle) + ` }
		local ann = { Handle = ` + luaNum(ann) + `, ClassName = 'PC', Lv = 9 }
		local bob = { Handle = ` + luaNum(bob) + `, ClassName = 'PC', Lv = 9 }

		-- :177 InsertHate(self, target, 20), and the census' 4th argument: no force.
		InsertHate(mon, ann, 20)
		InsertHate(mon, ann, 20, 100000)

		-- :5 / :150 GetTopHatePointChar(self)
		local target = GetTopHatePointChar(mon)
		assert(target ~= nil, 'GetTopHatePointChar answered nil')
		assert(target.Handle == ann.Handle, 'GetTopHatePointChar answered the wrong target')
		assert(target.ClassName == 'PC', 'the object lost its ClassName')
		assert(target.Points == 0, 'the object grew a Points field: ' .. tostring(target.Points))

		-- :164 SetTacticsTarget(self, target) then :101 GetTacticsTarget(self)
		SetTacticsTarget(mon, target)
		local lastTarget = GetTacticsTarget(mon)
		assert(lastTarget ~= nil, 'GetTacticsTarget answered nil after SetTacticsTarget')
		assert(lastTarget.Handle == ann.Handle, 'GetTacticsTarget answered another target')

		-- :167 IsSameTarget(target, lastTarget) - 1 is the same, 0 is not.
		assert(1 == IsSameTarget(target, lastTarget), 'IsSameTarget said a target is not itself')
		InsertHate(mon, bob, 50)
		local other = GetTopHatePointChar(mon)
		assert(other.Handle == bob.Handle, 'the higher score did not become the top')
		assert(0 == IsSameTarget(other, lastTarget), 'IsSameTarget confused two characters')

		-- :67 / :162 IsAttacking(self) - a StrID, compared as the string 'NO'.
		assert('NO' == IsAttacking(mon), 'IsAttacking answered ' .. tostring(IsAttacking(mon)))

		-- :149 / :188 RemoveHate(self, target), then the top falls to the second.
		RemoveHate(mon, other)
		local fallback = GetTopHatePointChar(mon)
		assert(fallback ~= nil, 'after RemoveHate nobody is hated, but one target is left')
		assert(fallback.Handle == ann.Handle, 'after RemoveHate the top is not the remaining target')

		-- :157 SetTacticsTarget(self) with ONE argument clears it.
		SetTacticsTarget(mon)
		assert(nil == GetTacticsTarget(mon), 'SetTacticsTarget(self) did not clear')
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nhate log:\n%s", err, logs.String())
	}

	// No name may have fallen through to the __on_missing stub: that would have read as
	// a 0 answer and the snippet's assertions would still have passed for a name the
	// runtime never found.
	bound := map[string]bool{"InsertHate": true, "RemoveHate": true,
		"GetTopHatePointChar": true, "SetTacticsTarget": true, "GetTacticsTarget": true,
		"IsSameTarget": true, "IsAttacking": true}
	for _, miss := range host.Missing() {
		if bound[miss.Name] {
			t.Fatalf("%s fell through to the missing-builtin stub %d time(s) - it is not "+
				"bound on the host", miss.Name, miss.Calls)
		}
	}

	// The zone saw the script's own numbers: ann at 40 (two 20s - the 4th argument was
	// ignored), bob at 50 then removed.
	top, ok := h.TopHate(mon.Handle)
	if !ok || top.Handle != ann || top.Points != 40 {
		t.Fatalf("after the snippet the top is 0x%08x at %d points (ok=%v), want 0x%08x at 40",
			top.Handle, top.Points, ok, ann)
	}
}

// luaNum writes a handle as a Lua number literal. Decimals only: the handles sit above
// 2^30 and both gopher-lua and the client's own scripts carry them as numbers, so a
// decimal literal is the least surprising thing to hand a runtime.
func luaNum(v uint32) string {
	return strconv.FormatUint(uint64(v), 10)
}

// killPlayerForHate and healPlayerForHate move one character's HP inside the roster,
// which is the whole of "this character is dead" to the hate table (the reference's
// BS_DEAD, HateList.cpp:69-71). The swing's own death path is not involved.
func killPlayerForHate(h *Handler, handle uint32) {
	setPlayerHPForHate(h, handle, 0)
}

func healPlayerForHate(h *Handler, handle uint32, hp float64) {
	setPlayerHPForHate(h, handle, hp)
}

func setPlayerHPForHate(h *Handler, handle uint32, hp float64) {
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			if mem.Sess.Team[j].Handle == handle {
				mem.Sess.Team[j].HP = hp
			}
		}
	}
}
