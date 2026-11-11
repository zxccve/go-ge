// Package script runs the game's own .scp scripts, translated to Lua.
//
// The TRANSLATOR stays in Python: `scp_to_lua.py --dump DIR` writes the corpus
// as plain .lua and this package runs that output. Rewriting the translator here
// would be a second implementation of a step already proved by a compile check
// (see DECISIONS.md, 2026-10-25).
//
// The ENGINE is gopher-lua, Lua 5.1. The game's own binaries embed Lua 5.0 and
// the corpus was measured to need nothing newer than 5.1, so this is closer to
// the original than the Python oracle's Lua 5.5.
//
// The SHAPE mirrors the Python host that already works (src\script_host.py), on
// purpose: ONE runtime per server, every script loaded into it at startup, a call
// by script name, and any builtin we have not written yet is RECORDED and answers
// 0 instead of raising. A missing feature is then a list of names in the log, not
// a crash in the middle of a dialog - which is how the Python host ranks its own
// remaining work.
package script

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"

	"geserver/text"
)

// DefaultCorpus is where `scp_to_lua.py --dump` writes the translated scripts,
// relative to goserver\ (the same way the command table is reached as ../data).
const DefaultCorpus = "../out/lua_corpus"

// Libraries are the shared `func` collections that every script may call. They
// are loaded EAGERLY, because a click script calls them without defining them;
// everything else is loaded on demand (see Index and EnsureScript).
//
// Same three the Python host preloads (script_host.py, LIBRARIES).
var Libraries = []string{
	"npc/utility/func_define.lua",
	"npc/utility/util_define.lua",
	"npc/utility/conv_define.lua",
}

// DefaultTickBudget is the runaway-loop limit. The Python host uses the same
// number, and the original server has the same protection ("Too long script
// execution time"); without it one bad `while(1)` stops the whole zone.
const DefaultTickBudget = 5_000_000

// prelude is Lua 5.0 compatibility, byte for byte the Python host's text:
//   - the 5.0 names 5.1 dropped (table.getn, table.foreach, string.gfind,
//     math.mod) are restored, because the corpus was written against 5.0;
//   - an unknown global becomes a recording stub instead of nil.
//
// ONE DEPARTURE FROM THE PYTHON HOST, added with the monster think turn: before the
// stub is created, `__resolve` is asked whether the CORPUS defines the name. The
// tactics scripts call each other across files (`SCR_MON_AGGRESSIVE_TS_NONE` ->
// `SCR_MON_AI_LIB_TS_NONE` -> `SCR_MON_AI_LIB_SKILL`), and this host loads a file on
// first use, so without the resolve the inner call would get the stub's 0 and the body
// would sit still. `__resolve` answers nil outside a monster thread, which keeps every
// existing run (clicks included) on exactly the old text.
const prelude = `
table.getn = function(t) return #t end
if not table.foreach then table.foreach = function(t, f)
  for k, v in pairs(t) do local r = f(k, v) if r ~= nil then return r end end
end end
if not unpack then unpack = table.unpack end
if not string.gfind then string.gfind = string.gmatch end
if not math.mod then math.mod = math.fmod end
-- unknown globals become recording stubs instead of nil, unless the corpus defines them
setmetatable(_G, {__index = function(t, k)
  local real = __resolve(k)
  if real ~= nil then rawset(t, k, real) return real end
  local f = function(...) return __on_missing(k, ...) end
  rawset(t, k, f)
  return f
end})
`

// withContext installs `ctx` as the deadline of the run about to start, and answers
// the function that takes it back out - ONLY IF the host is not already inside a run.
//
// WHY THE GUARD IS NOT OPTIONAL. `RemoveContext()` does not restore a previous
// context: it sets `L.ctx = nil` and puts the plain `mainLoop` back
// (gopher-lua state.go:2271-2277). A nested run - a buff ENTER started from inside an
// item's own script, which is exactly how `SCP_USE_ITEM_AddBuff` works - would
// therefore leave the OUTER VM loop's `select { case <-L.ctx.Done(): }` dereferencing
// a nil context on its next instruction: "invalid memory address or nil pointer
// dereference", raised from inside `mainLoopWithContext`, with no Go frame of ours in
// the traceback to say why. Measured here, not guessed: that is the crash this guard
// fixes.
//
// The nested run keeps the OUTER deadline, which is the right bound anyway - it is
// the same goroutine and the same unit of work.
func (h *Host) withContext(ctx context.Context) func() {
	if h == nil || h.L == nil {
		return func() {}
	}
	if h.L.Context() != nil {
		return func() {}
	}
	h.L.SetContext(ctx)
	return func() { h.L.RemoveContext() }
}

// Missing is one builtin the running scripts asked for and we have not written.
type Missing struct {
	Name  string
	Calls int
}

// Host is one Lua runtime with the corpus loaded into it.
//
// A Host is NOT safe for concurrent use: the same is true of the Python host,
// and the zone serialises script runs. Nothing here hides that.
type Host struct {
	L *lua.LState

	missing    map[string]int
	loaded     int
	failed     []string
	ticks      int
	TickBudget int
	dialogs    int

	// root is the corpus folder; index maps a script name to the file that
	// defines it, and loadedFiles stops a file being executed twice. The index is the
	// COMPACT form (script\index.go): 41,573 names as a plain map cost ~58 MB in every
	// zone, which is half a zone's tables (history\world-layer.md, 2026-11-09).
	root        string
	index       *packedIndex
	loadedFiles map[string]bool

	// click is the scratch of the ONE StartClick in progress (nil otherwise).
	// A Host is not safe for concurrent use, so a single field is enough - see
	// clickState in click.go.
	click *clickState

	// Text resolves locale tokens for the builtins that need the words, today
	// IMCFormat. NIL is allowed: a format then fills placeholders into the raw
	// token, which is wrong but visible, and the zone says why at startup.
	Text *text.Book

	// Tx is the database behind a script transaction (`TxBegin` ->
	// `TxSetCommProp` -> `TxEnd`). NIL means "no store wired": a transaction
	// that queued a write then FAILS (TxEnd answers 0) instead of pretending it
	// landed, because a script that believes a write succeeded takes the wrong
	// branch for the rest of the session.
	Tx TxStore

	// Log is where a refused or failed transaction is reported. NIL is quiet,
	// which is what a unit test wants - the zone sets it.
	Log func(format string, args ...any)

	// Items is the player's bag, read once per click. NIL means "no store": every
	// item count answers 0, which is the same answer an empty bag gives.
	Items ItemStore

	// Spawner is the world behind `CreateTNPC` and `CreateTmonster` - the two
	// builtins the game's own scripts use to put an NPC or a monster on the field
	// while the server runs (see create.go). NIL means "no world wired": both
	// names then REFUSE with a log line and create nothing, instead of answering
	// the 0 the missing-builtin stub answers - 0 is not nil, so the corpus' own
	// `if(Dummy1 ~= nil)` guard would pass and its next line would index a number.
	Spawner Spawner

	// Catalog answers what an item NAME is (class id, stack rule). NIL means "no
	// item table loaded": a name resolves to nothing and the count is 0.
	Catalog ItemCatalog

	// Near is the live world behind the radius scan and `SendMsg` - the half of the
	// Scarecrow's click that decides "create one" or "remove the one already here"
	// (see near.go). NIL means "no world wired": the scan answers 0 and says so.
	Near NearWorld

	// Hate is the world behind the battle script's seven hate builtins
	// (script\hate.go): the per-monster hate list, its tactics target and its attack
	// state, which `ge\script\monster\mon_ts_battle.scp` reads on every loop. NIL
	// means "no world wired": each of the seven answers its EMPTY value (nil /
	// nothing / 'NO') and says so once, because 0 here is a real answer ("nobody to
	// hate", "not attacking") and a silent stub would make the AI believe it.
	Hate HateWorld

	// Monster is the world behind the battle script's five READ-ONLY and TACTIC
	// builtins (script\monster_engine.go): the body's tactics state, the AI row its
	// `Tactics` names, its spawn point and the edge-to-edge distance between two
	// bodies. NIL means "no world wired": each of the five answers its EMPTY value
	// (nothing / nil / 0) and says so once. These are reads and one store - nothing
	// in that file starts, stops or steers anything.
	Monster MonsterWorld

	// Order is the world behind the battle script's seven MOVEMENT and ATTACK
	// builtins (script\monster_order.go): the walk a body is ordered to make - the
	// one it names, the one near its owner, and the two RANDOM ones that wander it
	// near its current point or its birth point - and the attack request it is
	// given. Those are the calls that put bytes on the wire - a 3550 for the walk -
	// so NIL means "no world wired" and every one of them then does NOTHING and
	// says so once, never sending a made-up packet.
	Order OrderWorld

	// Seek is the world behind the two names the THINK TURN adds to the battle
	// script's vocabulary (script\monster_ai.go): `GetObjectLocaly`, the circle the
	// NONE state acquires a target with, and `ResetHate`, the list reset it starts
	// with. NIL means "no world wired": both answer their empty value and say so
	// once. They are a separate field from Hate/Monster/Order because they are a
	// separate question - "who is near me" - and stage 1/2's interfaces stay as
	// they were.
	Seek MonsterSeek

	// monsterThreads are the RUNNING tactics scripts, one per body - the
	// reference's m_tacticsStateThread (script\monster_ai.go). monsterStops is
	// where a body whose thread ENDED (its own return, or an error) is remembered,
	// so it is not restarted until its tactics state changes. Both are keyed by the
	// body's handle; a handle is never reused.
	monsterThreads map[uint32]*monsterThread
	monsterStops   map[uint32]monsterStop

	// think is the monster thread being resumed RIGHT NOW, or nil. `sleep` yields
	// only while it is set, which is what keeps a click script's `sleep(100)` from
	// suspending a click.
	think *monsterThread

	// triggerThreads are the NPCs' RUNNING tactics scripts, one per object - the
	// reference's m_tacticsStateThread on a trigger object (script	rigger.go).
	// triggerStops remembers an object whose thread ended for good, so a broken
	// entry is reported once; trigger is the thread being resumed right now, so
	// `sleep` parks it instead of a click.
	triggers     map[uint32]*triggerThread
	triggerStops map[uint32]monsterStop
	trigger      *triggerThread

	// TriggerLog writes one line per thread life (start / end / failed). NIL is
	// quiet, which is what a unit test wants; the zone sets it to its own logger.
	TriggerLog func(handle uint32, entry, what, why string)

	// nearList is the snapshot the last `GetNearObjectCount` left for
	// `GetNearObject(index)` to index, which is the original's shared
	// `g_AllObjectChecker`. A Host runs one call at a time, so one field is enough.
	nearList []NearObject

	// reported is the set of problems already written to the log (an unserved
	// `SendMsg` name, a refused kill handle), so a script that repeats one writes one
	// line and not a thousand. reportMu guards it: a script run is serialised per
	// runner, but there is more than one runner on the shared host (a click, a buff
	// ENTER, a skill message), so two goroutines can reach this map at once.
	reported map[string]bool
	reportMu sync.Mutex

	// txSeq, txs and txOrder are the OPEN transactions. One runtime means one
	// set, keyed by the number TxBegin handed out; txOrder keeps them in the
	// order they began so the LATEST queued value wins a read-back.
	txSeq   int64
	txs     map[int64]*tx
	txOrder []int64

	// callCtx is the deadline of the call in progress, for a transaction's
	// database write. NIL outside a call.
	callCtx context.Context

	// clickCoroutine is a click script that yielded at ShowSelDlg and is waiting
	// for the player to pick an option. NIL when no dialog is suspended. While
	// set, h.click is still alive - its props and outcome are the suspended
	// script's scratch, and ResumeClick continues it.
	clickCoroutine *clickCoroutineState

	// status and statusSelf are the character ONE status-script run is about
	// (script\status.go). They are set for the length of RunStatus and cleared
	// after it, the same scratch discipline as h.click, and for the same reason:
	// one runtime, one call at a time. NIL status means "no source": every status
	// builtin then answers its empty value and says so once.
	status     StatusSource
	statusSelf StatusSelf

	// statusFrom is the SECOND character a two-actor run is about - the SENDER of a
	// script message, while `status` is its TARGET. The message script reads both
	// (`SCR_RESULT_BATTLEBUFF_SKILL` asks `GetCurrentStance(from)` for the caster's
	// stance, pc\common_pc.scp:6744), and the builtins resolve the caller's `self`
	// table through statusSourceFor. Installed only for the length of one message run
	// (script\skillmsg.go), so nothing else can see it.
	statusFrom StatusSource

	// buffs is the zone behind the nine buff builtins (script\buff.go), installed
	// for the length of ONE run by withBuffWorld. NIL means "no buff world": every
	// one of the nine then does nothing and answers the empty value ('NO' / -1), with
	// one line naming the name. It is save-and-restore rather than set-and-clear
	// because a buff's ENTER script may add another buff - the reference's
	// CBuffList::Add runs OnEnter with the list already in place (BuffList.cpp:127-130).
	buffs BuffWorld

	// physArg is the argument bag the LAST `GetPhysicalSkillArg()` handed out
	// (script\skill.go). It is the script's own working memory for ONE cast - the
	// same scratch discipline as h.click - and the caller reads it after the run
	// to see what the formula computed.
	physArg *lua.LTable

	// magArg is the same bag for the MAGICAL family: `GetMagicalSkillArg()`, the
	// table `SCR_SKILL_BUFF` writes `Buff` / `BuffType` / `BuffFreq` / `BuffID`
	// into before it hands the bag to the target with
	// `SendMsg(self, target, 'OM_SKILL_MAGICAL_ASSIST', magicalskillarg, ret)`
	// (ge\script\skill\buff.scp:36-40, :143). One cast at a time, like physArg.
	magArg *lua.LTable

	// assist is the live world behind `SendMsg(..., 'OM_SKILL_MAGICAL_ASSIST', ...)`
	// (script\skillmsg.go): it resolves the handle the script names as the message's
	// target to a real actor - its object type name and the `self` table its message
	// script reads. It is installed for the length of ONE skill-buff run
	// (`RunSkillBuff`), because a message may only land while the zone is behind it.
	// NIL means "no world": the message is then refused with one line, never a
	// silent no-op.
	assist AssistWorld

	// rng is the generator behind `IMCRandom` and `math.random` (script\random.go).
	// Seeded once in New from the nanosecond clock, and reseedable with Seed so a
	// test is reproducible. Owned by the host on purpose: the global source would
	// make two hosts (and the whole process) share one sequence.
	rng *rand.Rand
}

// clickCoroutineState holds one suspended click script: the gopher-lua coroutine,
// the function being resumed, and the cancel function for the coroutine's context.
type clickCoroutineState struct {
	co     *lua.LState
	fn     *lua.LFunction
	cancel context.CancelFunc
}

// New builds the runtime and the compatibility prelude.
func New() *Host {
	h := &Host{
		L:           lua.NewState(),
		missing:     map[string]int{},
		loadedFiles: map[string]bool{},
		TickBudget:  DefaultTickBudget,
		txs:         map[int64]*tx{},
	}
	h.L.SetGlobal("__on_missing", h.L.NewFunction(h.onMissing))
	h.L.SetGlobal("__resolve", h.L.NewFunction(h.resolveGlobal))
	h.L.SetGlobal("__record_dialog", h.L.NewFunction(h.onDialog))
	h.L.SetGlobal("_vtick", h.L.NewFunction(h.tick))

	// The dialog, window and object builtins a click script calls. They are
	// installed here, not left to the __on_missing stub: a stub answers 0, and 0
	// for `ShowOkDlg` means the NPC's line is discarded and the click looks dead.
	h.installClick()

	// The transaction builtins: a script's only way to change the world.
	h.installTx()

	// The bag and item reads: what the player holds (money included - it is the
	// vis item, not a property).
	h.installItems()

	// The world-creation builtins: CreateTNPC and CreateTmonster, which is how the
	// game's own scripts put a Scarecrow and its dummies on the field.
	h.installCreate()

	// The radius scan and `SendMsg`: how the Scarecrow's click asks "is one already
	// here?" and takes one off the field.
	h.installNear()

	// The battle script's hate builtins: the list, the tactics target and the attack
	// state `mon_ts_battle.scp` reads on every loop (script\hate.go). The world they
	// read is the zone's, so they are installed EMPTY here and answer nil/'NO' until
	// a zone is wired - never a made-up score.
	h.installHate()

	// The battle script's READ-ONLY and TACTIC calls: the body's tactics state, the
	// AI row its `Tactics` names, its spawn point and the edge-to-edge distance
	// (script\monster_engine.go). Installed EMPTY here like the hate builtins - the
	// world they read is the zone's, and nothing starts driving until the script
	// itself is run.
	h.installMonsterEngine()

	// The battle script's MOVEMENT and ATTACK calls: the walk (`Move`,
	// `MoveToOwner`) and the three attack requests (`script\monster_order.go`).
	// Installed EMPTY here like the two groups above - the world they steer is the
	// zone's, and until stage 3 runs the script nothing calls them.
	h.installMonsterOrder()

	// The THINK TURN's own three names: `sleep` (the IES thread's SleepThread, mine
	// to bind because the script's period is the script's own), `ResetHate` and
	// `GetObjectLocaly` (script\monster_ai.go). `sleep` is the one builtin whose
	// behaviour depends on WHICH run is asking - see monsterSleep.
	h.installMonsterAI()

	// The Character Info builtins: GetEquipItem, GetCurrentStance, the data-table
	// reads and the empty-but-honest answers (script\status.go). They are what lets
	// the game's own get_status_pc.scp run at all - its formula reads an item table
	// on line one, and the missing recorder would hand it 0.
	h.installStatus()

	// The buff builtins: the nine names the corpus applies, removes and reads a buff
	// with (script\buff.go). They are installed EMPTY like the hate builtins - the
	// list they act on is the zone's, and it is handed over for one run.
	h.installBuffs()

	// The combat-script builtins: the two argument bags the damage formula and its
	// hate message are written into (script\skill.go, R84). Two names, and without
	// them the formula raises on its first assignment.
	h.installSkill()

	// The RNG: `IMCRandom` and `math.random`/`math.randomseed` over one generator
	// owned by this host (script\random.go). Without it every chance roll takes
	// its low branch, and `math.random` is gopher-lua's unseeded global stream.
	h.installRandom()

	// The prelude is ours, so a failure here is a bug, not game data.
	if err := h.L.DoString(prelude); err != nil {
		panic("script: prelude failed: " + err.Error())
	}
	return h
}

// Close releases the runtime.
func (h *Host) Close() { h.L.Close() }

// onMissing answers an unimplemented builtin: record it, answer 0 - the same
// contract as the Python host's `_missing`.
func (h *Host) onMissing(L *lua.LState) int {
	name := L.OptString(1, "?")
	h.missing[name]++
	L.Push(lua.LNumber(0))
	return 1
}

// resolveGlobal answers the corpus's OWN function for a name the running script read as
// a global and did not find. It is the lazy half of "the reference has every script
// loaded": the tactics scripts call each other across files by design
// (`SCR_MON_AGGRESSIVE_TS_NONE` -> `SCR_MON_AI_LIB_TS_NONE`, mon_tactics_normal.scp:25 ->
// mon_ts_none.scp:1), and this host loads a file on first use.
//
// IT ANSWERS NIL OUTSIDE A MONSTER THREAD, which is what keeps this from changing a
// click: `h.think` is set for exactly the length of one resume of one body's tactics
// script, and a click that reads an SCR_ name keeps the stub it had before.
//
// The file is loaded RIGHT HERE, inside the metamethod and before the call, so the real
// function is what the script calls - never a nested Go->Lua call, which gopher-lua
// cannot yield inside (vm.go:177-180: a Go function that calls back into Lua and then
// hits `sleep` raises "can not yield from outside of a coroutine").
func (h *Host) resolveGlobal(L *lua.LState) int {
	name := L.OptString(1, "")
	if h.think == nil || name == "" || !strings.HasPrefix(name, "SCR_") {
		L.Push(lua.LNil)
		return 1
	}
	path, ok := h.index.Lookup(name)
	if !ok {
		L.Push(lua.LNil) // genuinely not in the corpus: the stub stands
		return 1
	}
	if !h.loadedFiles[path] {
		if err := h.LoadFile(path); err != nil {
			h.reportOnce("monai:load-failed:"+name,
				"script: %s is defined by %s but that file did not load (%v) - the call "+
					"gets the stub's 0", name, path, err)
			L.Push(lua.LNil)
			return 1
		}
		h.reportOnce("monai:loaded:"+name,
			"script: loaded %s because a tactics thread read %s - the corpus defines it "+
				"and only its file was not loaded (further loads are not logged)", path, name)
	}
	if fn, isFn := h.L.GetGlobal(name).(*lua.LFunction); isFn {
		h.L.SetGlobal(name, fn)
		L.Push(fn)
		return 1
	}
	L.Push(lua.LNil)
	return 1
}

// onDialog records that a script opened a message box. The text matters to the
// caller, so it is kept only as a count for now; the zone will need the text.
func (h *Host) onDialog(L *lua.LState) int {
	h.dialogs++
	return 0
}

// tick is the loop guard every `while(1)` body calls.
func (h *Host) tick(L *lua.LState) int {
	h.ticks++
	if h.TickBudget > 0 && h.ticks > h.TickBudget {
		L.RaiseError("script ran too long (%d ticks)", h.ticks)
	}
	return 0
}

// scriptFuncRe finds the entry points a translated file defines. It is a TEXT scan
// on purpose: building the index must not cost a Lua parse of 4,238 files, and the
// translator emits `function SCR_<name>(` once per entry.
//
// EVERY SCR_ NAME IS INDEXED, not only the click ones. A click is not the only way
// into a script: a trigger object also runs a tactics entry, `SCR_<script>_TS_NONE`
// (`TriggerObject.cpp:49-57`), and that is the one that creates the Scarecrow's
// dummies. Indexing only `SCR_CLICK_` made the tactics lookup fail with "not
// defined by any file" even though the function was there in the corpus.
//
// `SCP_` NAMES ARE INDEXED TOO, for the same reason one letter lower: an ITEM's use
// script is named `SCP_USE_ITEM_<effect>` (ge\script\item\item.scp:1529 onwards, 1,504
// distinct definitions), and the reference runs it by NAME the way it runs a click -
// `context->Run(itemProperty->scpType)` (PlayerChar.cpp:2250). Without them in the
// index the buff path's `HasScript("SCP_USE_ITEM_AddBuff")` answered false for a
// function the corpus plainly defines, and the item's own effect would never run.
var scriptFuncRe = regexp.MustCompile(`\bfunction\s+((?:SCR|SCP)_[A-Za-z0-9_]+)`)

// Index scans root and remembers which file defines which `SCR_*` name.
//
// It is the cheap half of loading on demand: a file is read as text, never
// parsed, so indexing the whole corpus costs well under a second. A name defined
// twice maps to the LAST file in sorted order, which is what one runtime would
// do anyway.
func (h *Host) Index(root string) (int, error) {
	if h.loadedFiles == nil {
		h.loadedFiles = map[string]bool{}
	}
	h.root = root

	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(p, ".lua") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk corpus %s: %w", root, err)
	}
	sort.Strings(files)

	// A file id per distinct path, interned once: 41,573 names share a few thousand
	// paths, and storing a string per name is exactly the waste this replaces.
	byName := make(map[string]uint32, 50000)
	fileID := make(map[string]uint32, 4096)
	paths := make([]string, 0, 4096)
	for _, path := range files {
		blob, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		id, seen := fileID[path]
		if !seen {
			id = uint32(len(paths))
			fileID[path] = id
			paths = append(paths, path)
		}
		for _, match := range scriptFuncRe.FindAllSubmatch(blob, -1) {
			byName[string(match[1])] = id
		}
	}
	h.index = packIndex(byName, paths)
	return h.index.Len(), nil
}

// IndexFromImage installs an index read from an image instead of walking the corpus. The host
// still remembers its root, because a file that is missing from the image (a script written after
// the image was built) must still be openable by name.
//
// The image's interned paths were produced by whichever build ran the WALK, so they are resolved
// against THIS root here (resolveIndexPath, index_image.go): that is what makes one image built on
// Windows open its files on Linux, where the stored `..\out\lua_corpus\...` prefix is not a path
// at all. Only the interned path table is rewritten (a few thousand entries, once per start).
func (h *Host) IndexFromImage(root string, idx *packedIndex) int {
	if h.loadedFiles == nil {
		h.loadedFiles = map[string]bool{}
	}
	h.root = root
	for i, stored := range idx.paths {
		idx.paths[i] = resolveIndexPath(root, stored)
	}
	h.index = idx
	return idx.Len()
}

// IndexValue hands the built index to the writer, so a first boot can save what it walked.
func (h *Host) IndexValue() *packedIndex { return h.index }

// Scripts lists the names the index knows, sorted.
func (h *Host) Scripts() []string {
	return h.index.Names()
}

// LoadLibraries loads the shared helper collections, once each.
func (h *Host) LoadLibraries() error {
	if h.root == "" {
		return fmt.Errorf("script: call Index before LoadLibraries")
	}
	for _, rel := range Libraries {
		path := filepath.Join(h.root, filepath.FromSlash(rel))
		if h.loadedFiles[path] {
			continue
		}
		if err := h.LoadFile(path); err != nil {
			return err
		}
	}
	return nil
}

// HasScript reports whether the corpus defines `name` at all.
//
// It exists for a TACTICS entry, and the difference matters: an NPC whose
// `SCR_<script>_TS_NONE` was never written is the NORMAL case (most NPCs have no
// tactics script), while a file that fails to load is a real fault. Without this a
// zone reports every script-less NPC as "FAILED" and buries the ones that are.
func (h *Host) HasScript(name string) bool {
	_, ok := h.index.Lookup(name)
	return ok
}

// ScriptFile is the corpus file the index says DEFINES name, or "" when no file
// does. It exists so a startup line can name the real path a source was built
// from instead of a path assembled by the caller - the index is the only thing
// that knows, and a second guess at it is a second place to be wrong.
func (h *Host) ScriptFile(name string) string {
	path, _ := h.index.Lookup(name)
	return path
}

// EnsureScript loads the file that defines name, unless it is already in.
//
// Returns loaded=true when work was done, so a caller can log the first click on
// an NPC and nothing after it. One runtime, many players: the definitions are
// shared, which is the point - memory is paid once per SCRIPT, not per player.
func (h *Host) EnsureScript(name string) (bool, error) {
	path, ok := h.index.Lookup(name)
	if !ok {
		return false, fmt.Errorf("%s is not defined by any file under %s", name, h.root)
	}
	if h.loadedFiles[path] {
		return false, nil
	}
	if err := h.LoadFile(path); err != nil {
		return false, err
	}
	return true, nil
}

// LoadFile executes one translated file, which defines its functions.
//
// THE CHUNK NAME IS THE FILE'S PATH, not the source text: `Load` with the path as the
// name is what makes a RUNTIME error say `<corpus path>:<line>: ...` instead of
// embedding the whole file in the message. The battle AI reports an error with the
// script's own line (script\monster_ai.go, monsterGiveUp), and the translator is
// line-for-line, so that line is the `.scp` line too.
func (h *Host) LoadFile(path string) error {
	blob, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	chunk, err := h.L.Load(bytes.NewReader(blob), path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// The chunk only DEFINES functions; it has to run before any of them can be
	// called.
	if err := h.L.CallByParam(lua.P{Fn: chunk, NRet: 0, Protect: true}); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if h.loadedFiles == nil {
		h.loadedFiles = map[string]bool{}
	}
	h.loadedFiles[path] = true
	h.loaded++
	return nil
}

// ScriptError is one file this engine cannot even PARSE. Path is the file as the
// caller named it; Err is gopher-lua's own message, which repeats the path
// because the path is the chunk name - a caller that logs both can trim it.
type ScriptError struct {
	Path string
	Err  error
}

// CompileCheck parses each file with the runtime's OWN Lua and does NOT run it:
// no Call, no global defined, no side effect. It answers the one question
// LoadFile cannot answer cheaply - "would this file parse at all?".
//
// It exists for the startup refresh (main.go, refreshScriptCorpus). The
// translator writes Lua as TEXT and never parses it, so a .scp with a real
// syntax error is reported as a successful translation, the zone starts
// normally, and the fault only appears when somebody clicks that NPC - the
// corpus index is lazy. This is the one place that difference is made visible,
// at boot, with the file named, using the engine that will actually run it and
// not the Python oracle's Lua.
//
// One state, one runtime: compiling does not execute anything, and a chunk that
// fails to compile leaves nothing behind (gopher-lua returns a throwaway
// function object), so a check cannot poison a script that runs later.
//
// A file that cannot be READ is reported too: the refresh just wrote it, so a
// missing one is a real fault and its name matters.
func (h *Host) CompileCheck(paths []string) []ScriptError {
	if h == nil || h.L == nil {
		return nil
	}
	var bad []ScriptError
	for _, path := range paths {
		fh, err := os.Open(path)
		if err != nil {
			bad = append(bad, ScriptError{Path: path, Err: err})
			continue
		}
		// Load = parse + compile to bytecode. It does NOT call the chunk, which
		// is the whole difference from LoadFile.
		_, parseErr := h.L.Load(fh, path)
		fh.Close()
		if parseErr != nil {
			bad = append(bad, ScriptError{Path: path, Err: parseErr})
		}
	}
	return bad
}

// LoadCorpus executes every .lua file under dir, which DEFINES the script
// functions. Files are walked in sorted order so two runs behave the same.
//
// This is the BAR TRACK load: the whole corpus at once, ~828 MB measured. A zone
// must not use it - see Index + LoadLibraries + EnsureScript, which pay for the
// scripts a map actually uses.
func (h *Host) LoadCorpus(dir string) (int, error) {
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(p, ".lua") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk corpus %s: %w", dir, err)
	}
	sort.Strings(files)

	h.root = dir
	for _, path := range files {
		if loadErr := h.LoadFile(path); loadErr != nil {
			h.failed = append(h.failed, loadErr.Error())
		}
	}
	return h.loaded, nil
}

// Names lists the globals a prefix introduced, sorted (`SCR_CLICK_` is the one
// the zone cares about: one per clickable NPC or object).
func (h *Host) Names(prefix string) []string {
	var names []string
	h.L.G.Global.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok && strings.HasPrefix(string(s), prefix) {
			names = append(names, string(s))
		}
	})
	sort.Strings(names)
	return names
}

// Call runs a script function with no arguments and a deadline, and resets the
// tick counter first so one runaway script cannot spend another one's budget.
func (h *Host) Call(name string, timeout time.Duration) error {
	fn, ok := h.L.GetGlobal(name).(*lua.LFunction)
	if !ok {
		return fmt.Errorf("%s is not a function (was the file loaded?)", name)
	}
	h.ticks = 0

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	return h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true})
}

// Missing lists the unimplemented builtins, most-called first - the worklist,
// in the order the corpus actually asks for them.
func (h *Host) Missing() []Missing {
	out := make([]Missing, 0, len(h.missing))
	for name, calls := range h.missing {
		out = append(out, Missing{Name: name, Calls: calls})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Stats reports what the corpus load did. Failed files are kept, not summarised:
// a script that does not load is a missing NPC, and the name matters.
func (h *Host) Stats() (loaded int, failed []string, dialogs int) {
	return h.loaded, append([]string(nil), h.failed...), h.dialogs
}
