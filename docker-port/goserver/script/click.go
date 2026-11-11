package script

import (
	"context"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"

	"geserver/item"
)

// The CLICK entry: what happens when a player clicks an NPC.
//
// A click script gets two arguments - the NPC it was clicked on and the player -
// and answers by asking for something: a line of text, a choice, a shop, a lobby.
// It does NOT send packets; the zone owns the socket and sends what the script
// asked for after the script has stopped. That split is deliberate and matches
// src\script_host.py: a menu NPC's choice decides WHICH window opens, so opening
// it from inside Lua would open it before the answer, or the wrong one.
//
// Dialog kinds, as the zone sees them.
const (
	DialogNone   = ""       // the script asked for nothing
	DialogOK     = "ok"     // one line of text: ZC_DIALOG_OK
	DialogSelect = "select" // a prompt plus choices: ZC_DIALOG_SELECT
)

// Fade types. `ShowOkDlg` opens with a fade-in and `MsgBox` does not
// (PlayerCharFunc.cpp:1073); the client draws the box either way.
const (
	FadeNone   uint8 = 0
	FadeFadeIn uint8 = 1
)

// Window kinds, for the request a shop or lobby NPC makes from its click script.
const (
	WindowNone       = ""
	WindowShop       = "normal"    // ShowNPCTradeDlg
	WindowShopCustom = "custom"    // ShowCustomNPCTradeDlg
	WindowWarehouse  = "warehouse" // ShowWarehouseDlg
	WindowLobby      = "lobby"     // ShowLobby
	// WindowWaypoint is the town-teleport grid: ShowWayPointList(self, pc), which
	// the waypoint NPC's own click script calls
	// (out\lua_corpus\waypoint\waypoint_auch_basic.lua:16). The live server answers
	// it with ZC_WAYPOINT_LIST (3742) and ZC_UI_OPEN (3753, frame "waypointlist");
	// the zone does that, from this kind and the game's own 15 rows.
	WindowWaypoint = "waypoint" // ShowWayPointList
)

// Coin names, normalized from the 4th argument of ShowNPCTradeDlg.
//
// The client's own scripts spell the coin in that argument: 'FesoItem' is a Feso
// shop, 'NormalItem' / 'NormalItem_Tax' / 'CashItem' and an ABSENT argument are
// vis. Only 'FesoItem' is carried through - the client's 3680 coin dword has two
// measured values (SHOP_VIS = 0, SHOP_FESO = 1), and there is no measured value for
// a cash item shop, so 'CashItem' opens a vis window rather than an invented one.
const (
	CoinNone = ""
	CoinFeso = "feso" // the script said 'FesoItem': the shop charges class 30001
)

// Dialog is what a click script asked the client to show.
type Dialog struct {
	Kind    string   // DialogNone, DialogOK or DialogSelect
	Text    string   // DialogOK: the line
	Prompt  string   // DialogSelect: the question
	Options []string // DialogSelect: the choices, in the script's order
	Fade    uint8
}

// Window is a request to open a panel rather than to speak.
type Window struct {
	Kind  string // WindowNone, WindowShop, ...
	Group string // the trade group name, when there is one
	// Coin is CoinNone (vis) or CoinFeso, from ShowNPCTradeDlg's 4th argument.
	// Only a NORMAL shop has one; a custom window carries its prices instead.
	Coin string
}

// Outcome is everything one click script asked for.
type Outcome struct {
	Dialog Dialog
	Window Window

	// Superseded counts the dialog requests that arrived AFTER the one being
	// answered. Non-zero means the script went on past a choice it could not
	// really make yet, and is how the resume work will know it is needed.
	Superseded int

	// BadText counts dialog requests whose text was missing or purely numeric -
	// what a script asks for when a helper it calls returned 0. Showing those
	// would put a box reading "0" in front of the player and hide the real gap.
	BadText int

	// Properties are the commander properties a COMMITTED transaction moved.
	// The zone pushes the Lock_* ones back to the client, whose cache only
	// moves when the packet arrives.
	Properties []PropValue

	// Items are the bag changes a COMMITTED transaction made, with the NEW total
	// per stack - which is what the client is told.
	Items []item.Change
}

// Obj describes one actor to a script: `self` (the NPC) or `pc` (the player).
//
// The fields are the ones the corpus actually reads off these bags -
// `GetPos(self)` among them, which is why X/Y/Z travel and not just a handle.
type Obj struct {
	Handle uint32
	Name   string
	Script string
	Level  int
	// Commander is whose properties `pc.<Name>` reads and whose transaction a
	// `TxEnd` writes. 0 means "not known", and a transaction then refuses to
	// write rather than writing to nobody.
	Commander int32
	X         int32
	Y         int32
	Z         int32
	// HP is the character's MAXIMUM HP. It is here for the Scarecrow, whose own
	// script sizes the dummy off the player: `mhp = stat1.HP * 100 - 100`. 0 means
	// "not known", and the Scarecrow's stand-in then says so in its log line.
	HP float64
}

// clickState is one StartClick call's scratch: the builtins write here and the
// zone reads it back. A Host runs ONE call at a time (it is not safe for
// concurrent use), so a single field is enough - no map, no per-player key.
type clickState struct {
	outcome Outcome

	// commander is whose properties this click may read and write; 0 when the
	// caller did not say, and a transaction then refuses to commit.
	commander int32

	// props is the commander's named properties, loaded once at the start of
	// the click. `pc.Quest_00001` is a real read, not always 0: a quest guards
	// itself with the state it wrote last time, and a bag that answers 0 for
	// every name makes every guard pass and the quest pay out on every click.
	props map[string]int64

	// refused counts item ops this click asked for that cannot be served yet
	// (no item layer). The first one is worth a log line; the rest are not.
	refused int

	// bag is the player's bag, read ONCE per click: class id -> total count. The
	// money question is a count in here too (the vis item), and slots is how full
	// the bag is - what an inventory-full check compares with capacity.
	//
	// first is the OLDEST stack's size per class, which is the only stack a give
	// may join: the shipped server never opens a second stack, it refuses a give
	// that does not fit.
	bag       map[int64]int64
	first     map[int64]int64
	slots     int
	bagLoaded bool

	// first-wins. ShowSelDlg answers 0 until the COROUTINE that resumes a
	// selected branch exists (src\script_host.py:327 yields instead), so a script
	// that branches on the answer takes the `else` arm and may ask for a SECOND
	// dialog. Keeping the first one is what makes the menu the player sees the
	// one they were meant to see; the rest are counted for the log.
	asked    int
	overrode int
}

// StartClick runs one NPC's click script and reports what it asked for.
//
// entry is the corpus name (`SCR_CLICK_<Script>`); it must already be loaded -
// the caller uses EnsureScript for that, because loading is what costs memory.
//
// A script that runs too long raises instead of hanging, the same budget every
// other call here uses.
func (h *Host) StartClick(entry string, self, pc Obj, timeout time.Duration) (Outcome, error) {
	fn, ok := h.L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		return Outcome{}, fmt.Errorf("%s is not a function (was the file loaded?)", entry)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The player's own properties, ONCE per click. A script that sets a quest
	// flag reads it back on the next click, so these must be the real rows;
	// a bag answering 0 for everything makes every guard pass.
	props := map[string]int64{}
	if h.Tx != nil && pc.Commander != 0 {
		loaded, err := h.Tx.CommanderProps(ctx, pc.Commander)
		if err != nil {
			// Not fatal: the click still runs, on zeros - which is what it did
			// before this layer existed. Saying so is what stops "the quest pays
			// out every time" from looking like the quest's own bug.
			h.logf("script: could not load commander %d's properties (%v) - every "+
				"property read this click answers 0", pc.Commander, err)
		} else if loaded != nil {
			props = loaded
		}
	}

	h.click = &clickState{commander: pc.Commander, props: props}
	defer func() { h.click = nil }()
	h.ticks = 0

	h.callCtx = ctx
	defer func() { h.callCtx = nil }()
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	err := h.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true},
		selfTable(h.L, self), h.pcTable(h.L, pc))
	out := h.click.outcome
	if err != nil {
		// A script that raised still asked for something before it raised, and a
		// caller can show that: it is the difference between "this NPC says
		// nothing" and "this NPC's script broke", and only one is worth chasing.
		return out, fmt.Errorf("%s: %w", entry, err)
	}
	return out, nil
}

// StartClickCoroutine runs one NPC's click script in a coroutine, so a
// ShowSelDlg call can yield and wait for the player's choice instead of
// answering 0.
//
// Returns the outcome and whether the script suspended (true when ShowSelDlg
// yielded - the zone must send ZC_DIALOG_SELECT and later call ResumeClick).
//
// A Host runs ONE call at a time, so ONE click can be suspended. A click that
// arrives while another is still suspended REPLACES it: the player who clicked
// again has walked away from the old menu, and refusing the click was worse than
// dropping the old one - a single unanswered menu locked every NPC on the server
// until the zone restarted (live log, 2026-10-26).
func (h *Host) StartClickCoroutine(entry string, self, pc Obj, timeout time.Duration) (Outcome, bool, error) {
	fn, ok := h.L.GetGlobal(entry).(*lua.LFunction)
	if !ok {
		return Outcome{}, false, fmt.Errorf("%s is not a function (was the file loaded?)", entry)
	}

	// A new click abandons whatever was waiting for an answer. Dropping it is
	// correct even when the client still shows the old box: the zone closes the
	// box on the player's next answer, and a menu that outlives the player's
	// interest must never stop the server answering anybody else.
	if h.clickCoroutine != nil {
		h.logf("script: %s replaces a suspended click - the player clicked away "+
			"from the dialog that was waiting for an answer", entry)
		h.clickCoroutine.cancel()
		h.clickCoroutine = nil
		h.click = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	// cancel is NOT deferred: it lives in clickCoroutineState while suspended,
	// and is called when the script finishes or errors.

	props := map[string]int64{}
	if h.Tx != nil && pc.Commander != 0 {
		loaded, err := h.Tx.CommanderProps(ctx, pc.Commander)
		if err != nil {
			h.logf("script: could not load commander %d's properties (%v) - every property read this click answers 0", pc.Commander, err)
		} else if loaded != nil {
			props = loaded
		}
	}

	h.click = &clickState{commander: pc.Commander, props: props}
	h.ticks = 0
	h.callCtx = ctx
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()

	co, coCancel := h.L.NewThread()

	state, err, _ := h.L.Resume(co, fn, selfTable(h.L, self), h.pcTable(h.L, pc))
	if err != nil {
		coCancel()
		cancel()
		h.click = nil
		h.clickCoroutine = nil
		return Outcome{}, false, fmt.Errorf("%s: %w", entry, err)
	}

	if state == lua.ResumeYield {
		// ShowSelDlg yielded: keep clickState alive, store the coroutine.
		// cancel() is not needed here: ResumeClick creates its own context.
		h.clickCoroutine = &clickCoroutineState{co: co, fn: fn, cancel: coCancel}
		cancel()
		return h.click.outcome, true, nil
	}

	// Finished normally - clean up.
	coCancel()
	cancel()
	h.clickCoroutine = nil
	defer func() { h.click = nil }()
	return h.click.outcome, false, nil
}

// CancelSuspended drops a click script that is waiting for the player's choice
// and reports whether anything was actually waiting.
//
// It exists because the suspension outlives the SESSION, not just the menu: a
// client that disconnects (or a player who never answers) leaves h.clickCoroutine
// set, and that ONE global slot used to refuse every later click on the whole
// server. The zone calls this when a connection closes.
func (h *Host) CancelSuspended() bool {
	if h.clickCoroutine == nil {
		return false
	}
	// cancel() releases the coroutine's context. The gopher-lua thread itself is
	// garbage once nothing refers to it; there is nothing else to free.
	h.clickCoroutine.cancel()
	h.clickCoroutine = nil
	h.click = nil
	return true
}

// ResumeClick continues a suspended click script with the player's chosen
// option index. Returns the outcome and whether it suspended again.
//
// chosenIndex is 0 for "no choice / closed" (CZ_ACK_DIALOG) or the 1-based
// index of the option the player picked (CZ_DIALOG_SELECT).
func (h *Host) ResumeClick(chosenIndex int, timeout time.Duration) (Outcome, bool, error) {
	cc := h.clickCoroutine
	if cc == nil {
		return Outcome{}, false, fmt.Errorf("no suspended click to resume")
	}
	// The coroutine and the click state are cleared together everywhere, but they
	// are two fields: a resume that raced a failed start (or a panic in the script)
	// leaves the coroutine behind with nothing to write the outcome to. Answering
	// with an error is right; dereferencing h.click below would be a panic.
	if h.click == nil {
		cc.cancel()
		h.clickCoroutine = nil
		return Outcome{}, false, fmt.Errorf("resume: click state is gone")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	h.callCtx = ctx
	h.L.SetContext(ctx)
	defer h.L.RemoveContext()
	// The coroutine carries its OWN context, taken from the state when NewThread
	// made it - and StartClickCoroutine cancels that one the moment the script
	// yields. Resuming therefore aborted instantly with "context canceled", which
	// is what sent every menu choice down the error path above. The resume's own
	// deadline has to be set on the THREAD, not just on the main state.
	cc.co.SetContext(ctx)

	// The dialog already shown must not block the resumed script: clear it so
	// ShowOkDlg etc. can record a new dialog after the choice.
	h.click.outcome.Dialog = Dialog{Kind: DialogNone}

	state, err, _ := h.L.Resume(cc.co, cc.fn, lua.LNumber(chosenIndex))
	if err != nil {
		// READ THE OUTCOME BEFORE CLEARING h.click. The script may have raised
		// after asking for something (a bad menu branch raising "attempt to index
		// a nil value" is common in this corpus), and that request is what the
		// caller can still show. Reading it after `h.click = nil` is a nil deref,
		// and that panic killed the whole zone process - the client just
		// disconnected and the log stopped, with no zone error at all.
		out := h.click.outcome
		cc.cancel()
		h.click = nil
		h.clickCoroutine = nil
		return out, false, fmt.Errorf("resume: %w", err)
	}

	if state == lua.ResumeYield {
		return h.click.outcome, true, nil
	}

	// Finished - clean up. Read the outcome first for the same reason as above:
	// the deferred `h.click = nil` runs after the return value is evaluated, but
	// saying it in one place keeps the order obvious.
	out := h.click.outcome
	cc.cancel()
	h.clickCoroutine = nil
	h.click = nil
	return out, false, nil
}

// selfTable and pcTable build the two argument bags. Same field names the Python
// host exposes (src\script_host.py `_self_bag` / `_pc_bag`).
func selfTable(L *lua.LState, o Obj) *lua.LTable {
	t := L.NewTable()
	t.RawSetString("Handle", lua.LNumber(o.Handle))
	t.RawSetString("Script", lua.LString(o.Script))
	t.RawSetString("Name", lua.LString(o.Name))
	t.RawSetString("X", lua.LNumber(o.X))
	t.RawSetString("Y", lua.LNumber(o.Y))
	t.RawSetString("Z", lua.LNumber(o.Z))
	propertyFallback(L, t)
	return t
}

// pcTable is the PLAYER's bag: the same fields as `self`, plus the commander's
// properties, which are real reads.
//
// The stored fields are raw-set, so they win over the metatable; every other
// name is looked up as a property. A property the running transaction has
// queued wins over the stored one, so a script that sets then reads agrees with
// itself - the same order the Python oracle's CommanderBag uses.
func (h *Host) pcTable(L *lua.LState, o Obj) *lua.LTable {
	t := selfTable(L, o)
	t.RawSetString("Level", lua.LNumber(o.Level))
	// HP is the character's MAXIMUM HP, and only the PLAYER's bag carries it: the
	// Scarecrow's own script sizes its dummy off it (`mhp = stat1.HP * 100 - 100`,
	// the game's damage meter). Set only when it is known, so a caller that does not
	// know leaves the property fallback answering 0 - the old behaviour.
	if o.HP > 0 {
		t.RawSetString("HP", lua.LNumber(o.HP))
	}

	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(h.propValue(o.Commander, L.CheckString(2))))
		return 1
	}))
	t.Metatable = mt
	return t
}

// propertyFallback makes a bag answer ANY property name with 0.
//
// This is the difference between a working NPC and a crashing one. The corpus
// reads game properties straight off the object - `commander.NPC_Master`,
// `commander.XQ_Yeganeh`, `commander.Lock_InventoryCount` - and a Lua table that
// does not carry the name answers NIL, so `nil >= 11` raises
// "attempt to compare number with nil" and the whole click dies. Measured on three
// map-10 NPCs (REB_BOARD, SHOP_PROMOTION, LEO_SERVICE, 2026-10-25).
//
// 0 is what the IES layer itself answers for an unset integer, and it is what the
// Python oracle's PropBag does (script_host.py, `_d.get(key, 0)`), so a branch that
// tests a property we do not know takes the same side on both sides.
func propertyFallback(L *lua.LState, t *lua.LTable) {
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(0))
		return 1
	}))
	t.Metatable = mt
}

// bag is a fresh object with the same fallback, for a builtin whose result the
// script indexes (`GetMasteryObject(pc).CM_AttackUp1`). Without it the missing
// builtin's 0 is a NUMBER, and indexing a number raises instead of answering 0.
func bag(L *lua.LState) *lua.LTable {
	t := L.NewTable()
	propertyFallback(L, t)
	return t
}

// -- the builtins a click script calls ------------------------------------
//
// installClick registers them once per runtime. They are NOT left to the
// __on_missing stub: a stub answers 0, and 0 for `ShowOkDlg` means the NPC's
// line is thrown away - the click then looks like it did nothing, which is
// exactly the bug this file exists to fix.

func (h *Host) installClick() {
	L := h.L

	// Dialogs with a line of text. `pc` is arg 1, the text arg 2.
	ok := func(fade uint8) lua.LGFunction {
		return func(L *lua.LState) int {
			h.ask(DialogOK, argString(L, 2), nil, fade)
			return 0
		}
	}
	L.SetGlobal("ShowOkDlg", L.NewFunction(ok(FadeFadeIn)))
	L.SetGlobal("ShowMsgDlg", L.NewFunction(ok(FadeFadeIn)))
	L.SetGlobal("ShowScnDlg", L.NewFunction(ok(FadeFadeIn)))
	L.SetGlobal("ShowCommonDlg", L.NewFunction(ok(FadeFadeIn)))
	L.SetGlobal("MsgBox", L.NewFunction(ok(FadeNone)))

	// Dialogs that ASK. The prompt is arg 2, the options are the rest.
	selectDlg := func(fade uint8) lua.LGFunction {
		return func(L *lua.LState) int {
			options := make([]string, 0, L.GetTop()-2)
			for i := 3; i <= L.GetTop(); i++ {
				options = append(options, argString(L, i))
			}
			h.ask(DialogSelect, argString(L, 2), options, fade)
			// Yield only when running inside a coroutine (StartClickCoroutine).
			// On the main thread (StartClick, tests) keep the old answer of 0
			// so existing callers are not broken. The zone uses StartClickCoroutine,
			// so its scripts yield and wait for the player's choice.
			if L != h.L {
				return L.Yield()
			}
			L.Push(lua.LNumber(0))
			return 1
		}
	}
	L.SetGlobal("ShowSelDlg", L.NewFunction(selectDlg(FadeFadeIn)))
	L.SetGlobal("ShowScnSelDlg", L.NewFunction(selectDlg(FadeFadeIn)))
	L.SetGlobal("MsgSelBox", L.NewFunction(selectDlg(FadeNone)))

	// Windows. Recorded, opened by the zone after the script's branch has run.
	window := func(kind string, groupArg, coinArg int) lua.LGFunction {
		return func(L *lua.LState) int {
			group := ""
			if groupArg > 0 {
				group = argString(L, groupArg)
			}
			coin := CoinNone
			if coinArg > 0 {
				coin = coinName(argString(L, coinArg))
			}
			if h.click != nil && h.click.outcome.Window.Kind == WindowNone {
				h.click.outcome.Window = Window{Kind: kind, Group: group, Coin: coin}
			}
			return 0
		}
	}
	// ShowNPCTradeDlg(self, pc, '<GROUP>', '<COIN>'): the 4th argument picks the
	// coin, and a menu NPC that opens a Feso shop on an option needs it just as
	// much as one that opens it on the first click.
	L.SetGlobal("ShowNPCTradeDlg", L.NewFunction(window(WindowShop, 3, 4)))
	L.SetGlobal("ShowCustomNPCTradeDlg", L.NewFunction(window(WindowShopCustom, 3, 0)))
	L.SetGlobal("ShowWarehouseDlg", L.NewFunction(window(WindowWarehouse, 0, 0)))
	L.SetGlobal("ShowLobby", L.NewFunction(window(WindowLobby, 0, 0)))
	// ShowWayPointList(self, pc): the town-teleport grid. It takes no group and no
	// coin - the rows come from the game's own waypoint table, not from the script -
	// so it is the same recorder with no arguments. Before this existed the call
	// fell through to the recording stub, which answers 0 and sends nothing, and
	// that is exactly what "clicking the Waypoint does nothing" looked like.
	L.SetGlobal("ShowWayPointList", L.NewFunction(window(WindowWaypoint, 0, 0)))

	// Object access. A helper library calls these on the bags above, and a 0
	// answer makes `GetPos(self)[1]` an index of a number - a Lua ERROR, not a
	// missing feature, and it would abort the whole click.
	L.SetGlobal("GetCommander", L.NewFunction(identity))
	L.SetGlobal("GetPC", L.NewFunction(identity))
	L.SetGlobal("GetLeader", L.NewFunction(identity))
	L.SetGlobal("GetPos", L.NewFunction(pushXYZ))

	// Builtins that answer with SEVERAL values. The stub answers ONE, so the rest
	// are nil in `local a, b, c = ...` and the first comparison raises - the same
	// shape as GetPos. Three zeros here is the honest answer (this side tracks no
	// promotion levels), and it makes the script take its "nobody is promoted"
	// arm instead of dying.
	L.SetGlobal("GetMccPromotionLevel", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		return 3
	}))

	// Builtins whose RESULT a script indexes. Answering 0 there is not "unset",
	// it is a number, and `GetMasteryObject(pc).CM_AttackUp1` raises on it - so
	// they hand back an empty bag that reads 0 for every field.
	L.SetGlobal("GetMasteryObject", L.NewFunction(func(L *lua.LState) int {
		L.Push(bag(L))
		return 1
	}))
	L.SetGlobal("GetZoneObject", L.NewFunction(func(L *lua.LState) int {
		L.Push(bag(L))
		return 1
	}))
	L.SetGlobal("GetName", L.NewFunction(func(L *lua.LState) int {
		if t, ok := L.Get(1).(*lua.LTable); ok {
			L.Push(t.RawGetString("Name"))
		} else {
			L.Push(lua.LString(""))
		}
		return 1
	}))

	// IMCFormat(token, args...) fills a text with numbers. It is a HOST builtin,
	// not a script one, and leaving it to the stub is what made 24 of map 10's
	// click scripts fail with "bad argument #2 to ShowOkDlg (string expected,
	// got number)": the stub answered 0, and 0 was handed to ShowOkDlg as text.
	L.SetGlobal("IMCFormat", L.NewFunction(func(L *lua.LState) int {
		args := make([]any, 0, L.GetTop()-1)
		for i := 2; i <= L.GetTop(); i++ {
			switch v := L.Get(i).(type) {
			case lua.LNumber:
				args = append(args, float64(v))
			case lua.LString:
				args = append(args, string(v))
			default:
				args = append(args, L.Get(i).String())
			}
		}
		L.Push(lua.LString(h.Text.Format(argString(L, 1), args)))
		return 1
	}))
	// Property reads and writes on ANY object the script holds (an NPC, the
	// player, the zone). The read goes through the object's own `__index`, which
	// is why the commander's bag answers real rows and a bare bag answers 0.
	//
	// `GetKillCount(obj, name)` is the SAME read, not a quest-stage counter -
	// `CommanderFunc.cpp:339` reads the commander's IES property and pushes it.
	// It was answering 0 for every name, and 479 click scripts branch on it, so
	// every "is this objective done?" guard took the false side.
	//
	// A WRITE is memory only: the original's IES object is memory too, and only a
	// transaction (`TxSetCommProp`) reaches the database. Forgetting a write is
	// the honest behaviour; persisting it silently would change the world without
	// a commit.
	readProp := func(L *lua.LState) int {
		name := argString(L, 2)
		if name == "" {
			L.Push(lua.LNumber(0))
			return 1
		}
		value := L.GetTable(L.Get(1), lua.LString(name))
		if value == lua.LNil {
			value = lua.LNumber(0)
		}
		L.Push(value)
		return 1
	}
	L.SetGlobal("GetIESProp", L.NewFunction(readProp))
	L.SetGlobal("GetKillCount", L.NewFunction(readProp))
	L.SetGlobal("SetIESProp", L.NewFunction(func(L *lua.LState) int {
		if t, ok := L.Get(1).(*lua.LTable); ok {
			if name := argString(L, 2); name != "" {
				L.SetTable(t, lua.LString(name), L.Get(3))
			}
		}
		L.Push(lua.LNumber(0))
		return 1
	}))
}

// ask records one dialog request, keeping the FIRST one (see clickState).
func (h *Host) ask(kind, text string, options []string, fade uint8) {
	c := h.click
	if c == nil {
		return // a dialog outside a click: nothing to answer it
	}
	if c.outcome.Dialog.Kind != DialogNone {
		c.outcome.Superseded++
		return
	}
	if !usableText(text) {
		// A number here is a helper that answered 0 (IMCFormat before it was
		// written did exactly that). Skip it and let a later, real line win.
		c.outcome.BadText++
		return
	}
	if kind == DialogOK {
		c.outcome.Dialog = Dialog{Kind: DialogOK, Text: text, Fade: fade}
		return
	}
	c.outcome.Dialog = Dialog{Kind: DialogSelect, Prompt: text, Options: options, Fade: fade}
}

// identity hands its first argument back: `GetCommander(pc)` and `GetPC(obj)` are
// how a script gets the player bag, and answering 0 there makes every later field
// read fail.
func identity(L *lua.LState) int {
	L.Push(L.Get(1))
	return 1
}

// coinName normalizes ShowNPCTradeDlg's 4th argument.
//
// Only 'FesoItem' selects Feso. Every other value the corpus passes -
// 'NormalItem', 'NormalItem_Tax', 'CashItem', a variable that holds a number, or
// no argument at all - means VIS, which is what the zone did before Feso shops
// existed. Defaulting the other way would take the wrong item out of the bag.
func coinName(s string) string {
	if s == "FesoItem" {
		return CoinFeso
	}
	return CoinNone
}

// argString reads one argument as text.
//
// It does NOT demand a string. A script legitimately passes a NUMBER as text
// (gopher-lua's OptString raises on it, and the raise aborted a quarter of map
// 10's click scripts), and a helper's 0 is a number too - which is why callers
// filter with usableText rather than trusting this.
func argString(L *lua.LState, i int) string {
	switch v := L.Get(i).(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return v.String()
	case lua.LBool:
		if bool(v) {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// usableText reports whether a dialog line is worth showing: present, and not
// just digits. A box reading "0" is what a stub answered, not what an NPC says.
func usableText(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return true
		}
	}
	return false
}

// pushXYZ implements `GetPos(obj)`, which pushes THREE values (x, y, height) -
// FieldCharFunc.cpp:972. Returning one number would make `local x, y, z =
// GetPos(self)` put nil in y and z, and the first arithmetic on them raises.
func pushXYZ(L *lua.LState) int {
	t, ok := L.Get(1).(*lua.LTable)
	if !ok {
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		L.Push(lua.LNumber(0))
		return 3
	}
	for _, k := range []string{"X", "Y", "Z"} {
		L.Push(t.RawGetString(k))
	}
	return 3
}
