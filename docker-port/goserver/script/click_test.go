package script

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"geserver/item"
	"geserver/text"
)

// newTestHost is a runtime with the dialog builtins installed and no corpus: the
// functions under test are defined inline, so the test says exactly what the
// script does without a translation step in between.
func newTestHost(t *testing.T, body string) *Host {
	t.Helper()
	h := New()
	t.Cleanup(h.Close)
	if body != "" {
		if err := h.L.DoString(body); err != nil {
			t.Fatalf("loading the test script: %v", err)
		}
	}
	return h
}

func TestStartClickCapturesLine(t *testing.T) {
	h := newTestHost(t, `function SCR_CLICK_T(self, pc) ShowOkDlg(self, "<$>14047</>") end`)

	out, err := h.StartClick("SCR_CLICK_T", Obj{Handle: 1}, Obj{Name: "Tester"}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if out.Dialog.Kind != DialogOK {
		t.Fatalf("kind = %q, want %q", out.Dialog.Kind, DialogOK)
	}
	// The text is passed through UNRESOLVED on purpose: the script engine speaks
	// tokens and the zone owns the dictionary.
	if out.Dialog.Text != "<$>14047</>" {
		t.Fatalf("text = %q, want the raw token", out.Dialog.Text)
	}
	if out.Dialog.Fade != FadeFadeIn {
		t.Fatalf("fade = %d, want %d (ShowOkDlg fades in)", out.Dialog.Fade, FadeFadeIn)
	}
}

func TestStartClickArgs(t *testing.T) {
	// A helper that reads a POSITION off pc and a NAME off self is the shape most
	// of the corpus uses; GetPos pushes three values, so a one-value stub would
	// put nil in y and z and raise on the first arithmetic.
	h := newTestHost(t, `
function SCR_CLICK_ARGS(self, pc)
  local x, y, z = GetPos(pc)
  seen = GetName(self) .. "|" .. tostring(x) .. "|" .. tostring(y) .. "|" .. tostring(z)
    .. "|" .. tostring(GetName(GetCommander(pc)))
end`)
	out, err := h.StartClick("SCR_CLICK_ARGS",
		Obj{Name: "Taxman"}, Obj{Name: "Tester", X: 1000, Y: 2000, Z: 33}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if out.Dialog.Kind != DialogNone {
		t.Fatalf("kind = %q, want none", out.Dialog.Kind)
	}
	if got, want := h.L.GetGlobal("seen").String(), "Taxman|1000|2000|33|Tester"; got != want {
		t.Fatalf("script saw %q, want %q", got, want)
	}
}

func TestStartClickFirstDialogWins(t *testing.T) {
	// The second box is what a script asks for after a choice it could not really
	// make (ShowSelDlg answers 0 until the resume exists). Keeping the first is
	// what makes the player see the menu they were meant to see.
	h := newTestHost(t, `function SCR_CLICK_TWO(self, pc)
  ShowOkDlg(self, "first")
  ShowOkDlg(self, "second")
end`)
	out, err := h.StartClick("SCR_CLICK_TWO", Obj{}, Obj{}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if out.Dialog.Text != "first" {
		t.Fatalf("text = %q, want the FIRST line", out.Dialog.Text)
	}
	if out.Superseded != 1 {
		t.Fatalf("Superseded = %d, want 1", out.Superseded)
	}
}

func TestStartClickSelect(t *testing.T) {
	h := newTestHost(t, `function SCR_CLICK_SEL(self, pc)
  local pick = ShowSelDlg(self, "Which one?", "Buy", "Sell", "Leave")
  after = tostring(pick)
end`)
	out, err := h.StartClick("SCR_CLICK_SEL", Obj{}, Obj{}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if out.Dialog.Kind != DialogSelect {
		t.Fatalf("kind = %q, want select", out.Dialog.Kind)
	}
	if out.Dialog.Prompt != "Which one?" {
		t.Fatalf("prompt = %q", out.Dialog.Prompt)
	}
	if want := []string{"Buy", "Sell", "Leave"}; !equal(out.Dialog.Options, want) {
		t.Fatalf("options = %v, want %v", out.Dialog.Options, want)
	}
	// 0, not 1: nothing has been chosen. The value is visible to the script, so a
	// future resume can change it and nobody has to guess what it used to be.
	if after := h.L.GetGlobal("after").String(); after != "0" {
		t.Fatalf("ShowSelDlg returned %q, want 0 until the player picks", after)
	}
}

// TestStartClickWayPointWindow is the call the waypoint NPC's own script makes.
//
// `ShowWayPointList(pc)` takes no group and no coin, and before it was a real
// builtin it fell through to the recording stub: the stub answers 0, the script's
// `return` is next, and the click produced nothing at all - which is exactly what
// "clicking the Waypoint structure does nothing" looked like in game.
//
// The real script call is `ShowWayPointList(pc)` (out\lua_corpus\waypoint\
// waypoint_auch_basic.lua:16), one argument; the two-argument form is here to pin
// that the builtin ignores extra arguments instead of raising, because a builtin
// that raises aborts the rest of the click.
func TestStartClickWayPointWindow(t *testing.T) {
	for _, call := range []string{"ShowWayPointList(pc)", "ShowWayPointList(self, pc)"} {
		h := newTestHost(t, `function SCR_CLICK_WAYPOINT_AUCH_BASIC(self, pc)
  ShowWayPointList(pc)
  return
end`)
		out, err := h.StartClick("SCR_CLICK_WAYPOINT_AUCH_BASIC", Obj{}, Obj{}, time.Second)
		if err != nil {
			t.Fatalf("%s: StartClick: %v", call, err)
		}
		if out.Window.Kind != WindowWaypoint {
			t.Fatalf("%s: window = %+v, want the waypoint kind %q", call, out.Window, WindowWaypoint)
		}
		if out.Dialog.Kind != DialogNone {
			t.Fatalf("%s: dialog kind = %q, want none (the grid is a window, not a box)",
				call, out.Dialog.Kind)
		}
		// A recorder that reported a missing builtin would mean the script called
		// something else and this test proved nothing.
		if missing := h.Missing(); len(missing) != 0 {
			t.Fatalf("%s: the script reached the stub for %v - ShowWayPointList is not "+
				"registered", call, missing)
		}
	}
}

func TestStartClickWindow(t *testing.T) {
	h := newTestHost(t, `function SCR_CLICK_SHOP(self, pc)
  ShowNPCTradeDlg(self, pc, "WEAPON_SHOP")
end`)
	out, err := h.StartClick("SCR_CLICK_SHOP", Obj{}, Obj{}, time.Second)
	if err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if out.Window.Kind != WindowShop || out.Window.Group != "WEAPON_SHOP" {
		t.Fatalf("window = %+v, want a normal shop on WEAPON_SHOP", out.Window)
	}
	if out.Dialog.Kind != DialogNone {
		t.Fatalf("kind = %q, want none (the window is not a dialog)", out.Dialog.Kind)
	}
}

// TestStartClickWindowCoin is the 4th argument of ShowNPCTradeDlg, the coin the
// client's own scripts name: 'FesoItem' is Feso, and every other value the corpus
// passes ('NormalItem', 'NormalItem_Tax', 'CashItem', a variable, or no argument)
// is vis. A wrong guess here takes the wrong item out of the player's bag.
func TestStartClickWindowCoin(t *testing.T) {
	cases := []struct {
		name string
		call string
		want string
	}{
		{"Feso", `ShowNPCTradeDlg(self, pc, "FESO_ITEMSHOP_POT", "FesoItem")`, CoinFeso},
		{"NormalItem", `ShowNPCTradeDlg(self, pc, "ETC_LEONARDO", "NormalItem")`, CoinNone},
		{"NormalItem_Tax", `ShowNPCTradeDlg(self, pc, "UST_FARM_SHOP", "NormalItem_Tax")`, CoinNone},
		{"CashItem", `ShowNPCTradeDlg(self, pc, "ETC_LEONARDO", "CashItem")`, CoinNone},
		{"a variable", `local t = 0; ShowNPCTradeDlg(self, pc, "ETC_LEONARDO", t)`, CoinNone},
		{"no argument", `ShowNPCTradeDlg(self, pc, "ETC_LEONARDO")`, CoinNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHost(t, `function SCR_CLICK_SHOP(self, pc) `+c.call+`
end`)
			out, err := h.StartClick("SCR_CLICK_SHOP", Obj{}, Obj{}, time.Second)
			if err != nil {
				t.Fatalf("StartClick: %v", err)
			}
			if out.Window.Kind != WindowShop {
				t.Fatalf("window = %+v, want a normal shop", out.Window)
			}
			if out.Window.Coin != c.want {
				t.Fatalf("coin = %q, want %q for %s", out.Window.Coin, c.want, c.call)
			}
		})
	}
}

func TestStartClickRaisedScriptKeepsWhatItAsked(t *testing.T) {
	h := newTestHost(t, `function SCR_CLICK_BAD(self) ShowOkDlg(self, "hello") error("mid-script") end`)
	out, err := h.StartClick("SCR_CLICK_BAD", Obj{}, Obj{}, time.Second)
	if err == nil {
		t.Fatal("a script that raised reported no error")
	}
	if out.Dialog.Text != "hello" {
		t.Fatalf("text = %q, want what it asked for before raising", out.Dialog.Text)
	}
}

// TestMissingPropertyIsZero is the crash class that three map-10 NPCs died on:
// the corpus reads a game property straight off the commander
// (`commander.NPC_Master`), and a bare Lua table answers nil, so `nil >= 11`
// raises "attempt to compare number with nil" and the click is lost.
func TestMissingPropertyIsZero(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_PROP(self, pc)
  local c = GetCommander(pc)
  if (c.NPC_Master >= 11 and c.NPC_Master <= 14) then
    ShowOkDlg(self, "mastery branch")
  else
    ShowSelDlg(self, "which one?", "a", "b")
  end
  local m = GetMasteryObject(c)
  seen = tostring(m.CM_AttackUp1)
end`)

	out, err := h.StartClick("SCR_CLICK_PROP", Obj{}, Obj{Name: "Tester"}, time.Second)
	if err != nil {
		t.Fatalf("a missing property crashed the script: %v", err)
	}
	// 0 is not between 11 and 14, so the else arm runs - the same side the Python
	// oracle's PropBag takes.
	if out.Dialog.Kind != DialogSelect {
		t.Fatalf("kind = %q, want the else arm's menu", out.Dialog.Kind)
	}
	if got := h.L.GetGlobal("seen").String(); got != "0" {
		t.Fatalf("mastery field = %q, want \"0\"", got)
	}
}

// TestBagFieldsDoNotOverwriteKept verifies the real fields still read correctly
// once the fallback metatable is on: Handle and Name are set, not zero.
func TestBagFieldsDoNotOverwriteKept(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_BAG(self, pc)
  seen = tostring(self.Handle) .. "|" .. GetName(self) .. "|" .. tostring(pc.Level)
    .. "|" .. tostring(pc.NotAField)
end`)
	if _, err := h.StartClick("SCR_CLICK_BAG",
		Obj{Handle: 7, Name: "Taxman"}, Obj{Level: 42}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got, want := h.L.GetGlobal("seen").String(), "7|Taxman|42|0"; got != want {
		t.Fatalf("bag reads = %q, want %q", got, want)
	}
}

// TestMultiValueBuiltin covers the arity half of the same class: a stub answers
// ONE value, so `local a, b, c = F()` leaves b and c nil and `b >= 1` raises.
func TestMultiValueBuiltin(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_MCC(self, pc)
  local a, b, c = GetMccPromotionLevel(pc)
  if (b >= 1 or c >= 1) then
    ShowOkDlg(self, "someone is promoted")
  else
    ShowOkDlg(self, "nobody is promoted")
  end
end`)
	out, err := h.StartClick("SCR_CLICK_MCC", Obj{}, Obj{}, time.Second)
	if err != nil {
		t.Fatalf("a multi-value builtin crashed the script: %v", err)
	}
	if out.Dialog.Text != "nobody is promoted" {
		t.Fatalf("text = %q, want the not-promoted arm", out.Dialog.Text)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMap10ClickScripts runs the REAL map 10 click scripts through this engine.
//
// It is the honest measure of the feature: "the click is wired" means nothing if
// the scripts a player can reach raise. By default it checks a handful (the suite
// stays fast); set GE_CLICK_ALL=1 to sweep every NPC and print the tally.
//
// It skips, loudly, when the corpus or the NPC data is not built - a missing
// build input is not a failing test.
func TestMap10ClickScripts(t *testing.T) {
	corpus := filepath.Join("..", "..", "out", "lua_corpus")
	npcsPath := filepath.Join("..", "..", "data", "zone_npcs", "map10.json")
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("no corpus at %s (run: cd src && python scp_to_lua.py --dump ../out/lua_corpus)", corpus)
	}
	blob, err := os.ReadFile(npcsPath)
	if err != nil {
		t.Skipf("no NPC data at %s", npcsPath)
	}
	var doc struct {
		NPCs []struct {
			ClassType string `json:"class_type"`
			Script    string `json:"script"`
		} `json:"npcs"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("%s: %v", npcsPath, err)
	}

	all := os.Getenv("GE_CLICK_ALL") != ""
	limit := 5
	if all {
		limit = len(doc.NPCs)
	}

	h := New()
	defer h.Close()
	// The transaction layer, in memory: a click that commits a counter or a quest
	// flag must SUCCEED here, or the sweep measures "no database" instead of the
	// script. A store that answers 0 for everything is honest - the property rows
	// live in the database this test does not touch.
	store := &fakeStore{}
	h.Tx = store
	h.Log = t.Logf
	// The bag and the item table, so `TxGiveItem`/`TxTakeItem` are measured with a
	// real item name -> class id: an empty bag means every give can succeed and
	// every take is refused for "not enough held", which is what a fresh character
	// looks like.
	h.Items = &fakeBag{counts: map[int64]int64{}}
	if catalog, err := item.Load(filepath.Join("..", "..", "data", "item_catalog.json")); err == nil {
		h.Catalog = catalog
	} else {
		t.Logf("no item catalog (%v) - item ops will be refused", err)
	}
	if _, err := h.Index(corpus); err != nil {
		t.Fatalf("index %s: %v", corpus, err)
	}
	if err := h.LoadLibraries(); err != nil {
		t.Fatalf("libraries: %v", err)
	}
	// The word tables, when the client data is there: `IMCFormat` needs them, and
	// without them every formatted line is a raw token. Skips quietly otherwise.
	if book, err := text.Load(
		filepath.Join("..", "..", "data", "locale_id_map.json"),
		filepath.Join("..", "..", "..", "ge", "dictionary", "dictionary_local.xml"),
	); err == nil {
		h.Text = book
	} else {
		t.Logf("no dictionary (%v) - formatted lines will be tokens", err)
	}

	kinds := map[string]int{}
	failed := []string{}
	checked := 0
	// What the transaction layer actually did: a click that produced a dialog
	// can still have changed nothing, and those are different results.
	committing := 0
	committed := map[string]int{}
	for _, npc := range doc.NPCs {
		if npc.Script == "" || checked == limit {
			continue
		}
		entry := "SCR_CLICK_" + npc.Script
		if _, err := h.EnsureScript(entry); err != nil {
			failed = append(failed, entry+" (not in the corpus)")
			continue
		}
		checked++
		out, err := h.StartClick(entry,
			Obj{Handle: 0x40000000, Script: npc.Script, Name: "<$>1</>"},
			Obj{Name: "Tester", Level: 1, X: 5779, Y: 303, Z: 149, Commander: 24},
			2*time.Second)
		if err != nil {
			failed = append(failed, entry+": "+err.Error())
			continue
		}
		kind := out.Dialog.Kind
		if kind == DialogNone && out.Window.Kind != "" {
			kind = "window:" + out.Window.Kind
		}
		if kind == DialogNone {
			kind = "none"
		}
		kinds[kind]++
		if len(out.Properties) > 0 {
			committing++
			for _, p := range out.Properties {
				committed[p.Name]++
			}
		}
	}

	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t.Logf("%-16s %d", k, kinds[k])
	}
	// The Tx layer's own number: a dialog is not a world change.
	t.Logf("%d of %d script(s) COMMITTED at least one property", committing, checked)
	t.Logf("%d give(s) and %d take(s) reached the store", len(store.gave), len(store.took))
	props := make([]string, 0, len(committed))
	for name := range committed {
		props = append(props, name)
	}
	sort.Strings(props)
	for _, name := range props {
		t.Logf("  committed %-32s in %d click(s)", name, committed[name])
	}
	t.Logf("checked %d script(s); %d failed", checked, len(failed))
	for _, f := range failed {
		t.Logf("  FAILED %s", f)
	}
	if checked == 0 {
		t.Fatal("no click script was checked at all")
	}
}

// TestGMManualScriptWinsTheIndex guards the ONE deliberate shadow in the corpus.
//
// `AGE_REBECCA` is the "[GM] Server Rules and Updates" NPC, and `zz_gm_test.scp`
// replaces its click with the Tx test console. The index keeps the LAST file that
// defines a name, so the shadow only works while that file sorts after every
// `npc\...` script - a rename or a re-dump that drops it would quietly restore the
// shipped lobby script, and the GM console would look "deleted".
func TestGMManualScriptWinsTheIndex(t *testing.T) {
	corpus := filepath.Join("..", "..", "out", "lua_corpus")
	if _, err := os.Stat(corpus); err != nil {
		t.Skipf("no corpus at %s", corpus)
	}
	h := New()
	defer h.Close()
	if _, err := h.Index(corpus); err != nil {
		t.Fatalf("index: %v", err)
	}
	path, ok := h.index.Lookup("SCR_CLICK_AGE_REBECCA")
	if !ok {
		t.Fatal("AGE_REBECCA is not in the index at all")
	}
	if filepath.Base(path) != "zz_gm_test.lua" {
		t.Fatalf("SCR_CLICK_AGE_REBECCA resolves to %s, want the GM console (zz_gm_test.lua)",
			path)
	}
}

// TestResumeClickWithoutASuspendedClick: a resume that arrives with nothing to
// resume is an ERROR, never a panic. The zone process serves every connection in
// one goroutine per player, and a panic here used to take the whole zone down -
// the client disconnected and the log stopped with no zone line at all.
func TestResumeClickWithoutASuspendedClick(t *testing.T) {
	h := newTestHost(t, "")

	out, again, err := h.ResumeClick(1, time.Second)
	if err == nil {
		t.Fatal("ResumeClick with no suspended click returned no error")
	}
	if again {
		t.Fatal("ResumeClick with no suspended click claimed it suspended again")
	}
	if out.Dialog.Kind != DialogNone || out.Window.Kind != WindowNone {
		t.Fatalf("outcome = %+v, want nothing asked for", out)
	}
}

// TestResumeClickWithoutClickState: the coroutine and the click state are two
// fields, and a half-cleared host has the coroutine with no state behind it.
// That case must answer an error and clear the coroutine, not dereference nil.
func TestResumeClickWithoutClickState(t *testing.T) {
	h := newTestHost(t, "")
	cancelled := false
	h.clickCoroutine = &clickCoroutineState{cancel: func() { cancelled = true }}

	if _, _, err := h.ResumeClick(1, time.Second); err == nil {
		t.Fatal("ResumeClick with no click state returned no error")
	}
	if !cancelled {
		t.Fatal("the coroutine was not cancelled")
	}
	if h.clickCoroutine != nil {
		t.Fatal("the coroutine was left set: the next click would be refused as 'already suspended'")
	}
}

// TestStartClickReplacesASuspendedClick is the server lockout this file was fixed
// for. One unanswered menu used to make every NPC on the server unclickable until
// the zone restarted, because the suspended click is ONE slot for the whole
// runtime and a new click was refused while it was set (live log, 2026-10-26).
//
// A player who clicks again has abandoned the old menu, so the new click must WIN:
// it returns its own outcome and reports itself suspended, and the old coroutine's
// cancel is called.
func TestStartClickReplacesASuspendedClick(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_FIRST(self, pc)
  ShowSelDlg(self, "first menu", "one", "two")
  ShowOkDlg(self, "first choice made")
end
function SCR_CLICK_SECOND(self, pc)
  ShowSelDlg(self, "second menu", "alpha", "beta")
  ShowOkDlg(self, "second choice made")
end`)

	if _, suspended, err := h.StartClickCoroutine("SCR_CLICK_FIRST", Obj{Handle: 1}, Obj{Name: "Tester"}, time.Second); err != nil {
		t.Fatalf("first StartClickCoroutine: %v", err)
	} else if !suspended {
		t.Fatal("the first script did not suspend at ShowSelDlg")
	}
	first := h.clickCoroutine

	out, suspended, err := h.StartClickCoroutine("SCR_CLICK_SECOND", Obj{Handle: 2}, Obj{Name: "Tester"}, time.Second)
	if err != nil {
		t.Fatalf("the second click was refused (%v): one unanswered menu must not "+
			"lock every NPC on the server", err)
	}
	if !suspended {
		t.Fatal("the second script did not suspend at ShowSelDlg")
	}
	if out.Dialog.Prompt != "second menu" {
		t.Fatalf("prompt = %q, want %q: the outcome of the OLD menu was returned",
			out.Dialog.Prompt, "second menu")
	}
	if h.clickCoroutine == nil || h.clickCoroutine == first {
		t.Fatal("the host still holds the first coroutine, not the second")
	}
	// The replaced click must be dead, not resumed later down the wrong branch.
	if _, _, err := h.ResumeClick(1, time.Second); err != nil {
		t.Fatalf("resuming the SECOND click failed: %v", err)
	}
}

// TestCancelSuspended pins the release a closing connection needs: true when a
// click was waiting, false the second time, and the host left clean either way.
func TestCancelSuspended(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_WAIT(self, pc)
  local choice = ShowSelDlg(self, "waiting", "yes", "no")
  ShowOkDlg(self, "chosen " .. choice)
end`)

	if h.CancelSuspended() {
		t.Fatal("CancelSuspended reported a suspended click on a clean host")
	}

	if _, suspended, err := h.StartClickCoroutine("SCR_CLICK_WAIT", Obj{Handle: 1}, Obj{Name: "Tester"}, time.Second); err != nil {
		t.Fatalf("StartClickCoroutine: %v", err)
	} else if !suspended {
		t.Fatal("the script did not suspend at ShowSelDlg")
	}

	if !h.CancelSuspended() {
		t.Fatal("CancelSuspended did not report the suspended click")
	}
	if h.clickCoroutine != nil || h.click != nil {
		t.Fatal("CancelSuspended left state behind: the next click would be refused")
	}
	if h.CancelSuspended() {
		t.Fatal("CancelSuspended reported a suspended click after clearing it")
	}
	if _, _, err := h.ResumeClick(1, time.Second); err == nil {
		t.Fatal("the cancelled click could still be resumed")
	}
}

// TestResumeClickKeepsWhatARaisedBranchAsked is the crash this file was fixed for.
//
// A menu branch that raises is common in this corpus ("attempt to index a nil
// value"). Before the fix, the error path cleared h.click and THEN read
// h.click.outcome to return it - a nil dereference that panicked the zone. The
// line the branch asked for before it raised is still worth showing, so the test
// pins both halves: no panic, and the outcome survives.
func TestResumeClickKeepsWhatARaisedBranchAsked(t *testing.T) {
	h := newTestHost(t, `
function SCR_CLICK_RAISE(self, pc)
  local choice = ShowSelDlg(self, "<$>14047</>", "first", "second")
  ShowOkDlg(self, "chosen " .. choice)
  local broken = nil
  broken.Field = 1
end`)

	_, suspended, err := h.StartClickCoroutine("SCR_CLICK_RAISE", Obj{Handle: 1}, Obj{Name: "Tester"}, time.Second)
	if err != nil {
		t.Fatalf("StartClickCoroutine: %v", err)
	}
	if !suspended {
		t.Fatal("the script did not suspend at ShowSelDlg")
	}

	out, again, err := h.ResumeClick(1, time.Second)
	if err == nil {
		t.Fatal("the raising branch returned no error")
	}
	if again {
		t.Fatal("a raising branch claimed it suspended")
	}
	if out.Dialog.Kind != DialogOK {
		t.Fatalf("kind = %q, want %q: the line asked for before the raise was lost",
			out.Dialog.Kind, DialogOK)
	}
	if out.Dialog.Text != "chosen 1" {
		t.Fatalf("text = %q, want %q (the choice the player made)", out.Dialog.Text, "chosen 1")
	}
	if h.click != nil || h.clickCoroutine != nil {
		t.Fatal("the click state was not cleared after the error")
	}
}
