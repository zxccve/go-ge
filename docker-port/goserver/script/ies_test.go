package script

import (
	"testing"
	"time"
)

// TestKillCountIsAPropertyRead pins the builtin that 479 click scripts branch on.
//
// `GetKillCount(obj, name)` is NOT a quest-stage counter: `CommanderFunc.cpp:339`
// reads the commander's IES property and pushes it. Answering 0 for every name
// made every "is this objective done?" guard take the false side, which looks like
// an NPC that refuses to acknowledge progress.
func TestKillCountIsAPropertyRead(t *testing.T) {
	store := &fakeStore{props: map[string]int64{"Kill_Goblin": 7}}
	h := newTestHost(t, `
function SCR_CLICK_KILL(self, pc)
  kills = GetKillCount(GetCommander(pc), "Kill_Goblin")
  missing = GetKillCount(GetCommander(pc), "Kill_Nobody")
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_KILL", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	if got := h.L.GetGlobal("kills").String(); got != "7" {
		t.Fatalf("GetKillCount = %q, want the stored 7", got)
	}
	// A property with no row is 0, not nil: `nil >= 11` raises in Lua.
	if got := h.L.GetGlobal("missing").String(); got != "0" {
		t.Fatalf("absent kill count = %q, want 0", got)
	}
}

// TestIESPropReadsAndWritesTheObject covers the other two: a read goes through the
// object's own `__index`, and a write lands on the object (memory only - only a
// transaction reaches the database).
func TestIESPropReadsAndWritesTheObject(t *testing.T) {
	store := &fakeStore{props: map[string]int64{"Zone_Stone": 3}}
	h := newTestHost(t, `
function SCR_CLICK_IES(self, pc)
  local o = GetZoneObject(GetCommander(pc))
  before = GetIESProp(o, "Zone_Stone")
  SetIESProp(o, "Zone_Stone", 9)
  after = GetIESProp(o, "Zone_Stone")
  -- a write must NOT reach the database: there was no transaction
  SetIESProp(GetCommander(pc), "Cwar_Mining", 5)
end`)
	h.Tx = store

	if _, err := h.StartClick("SCR_CLICK_IES", Obj{},
		Obj{Commander: 24}, time.Second); err != nil {
		t.Fatalf("StartClick: %v", err)
	}
	// GetZoneObject hands back an empty bag (this side tracks no zone counters),
	// so a read there is 0 and the write is remembered on THAT bag.
	if got := h.L.GetGlobal("before").String(); got != "0" {
		t.Fatalf("zone object read = %q, want 0", got)
	}
	if got := h.L.GetGlobal("after").String(); got != "9" {
		t.Fatalf("read-back after SetIESProp = %q, want 9", got)
	}
	if store.commits != 0 {
		t.Fatalf("SetIESProp wrote %v to the database - only a transaction may", store.committed)
	}
}
