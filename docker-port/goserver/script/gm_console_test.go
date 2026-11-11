package script

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"geserver/item"
)

// A world where the STORE and the BAG are the same object.
//
// The unit tests elsewhere keep them apart on purpose (each proves one call). The GM
// console reads a count AFTER a commit, so its test needs the two to agree - the
// store applies an op to the same stacks the bag read returns, which is what the
// real CommitTx + Stacks pair does.
type gmWorld struct {
	props  map[string]int64
	stacks []item.Stack
	nextID int64
}

func (w *gmWorld) CommanderProps(context.Context, int32) (map[string]int64, error) {
	out := make(map[string]int64, len(w.props))
	for k, v := range w.props {
		out[k] = v
	}
	return out, nil
}

func (w *gmWorld) Stacks(context.Context, int32) ([]item.Stack, error) {
	out := make([]item.Stack, len(w.stacks))
	copy(out, w.stacks)
	return out, nil
}

func (w *gmWorld) CommitTx(_ context.Context, _ int32, props map[string]int64, ops []item.Op) ([]item.Change, error) {
	for k, v := range props {
		w.props[k] = v
	}
	var changes []item.Change
	for _, op := range ops {
		idx := -1
		for i, st := range w.stacks {
			if st.ClassID == op.ClassID {
				idx = i
				break
			}
		}
		if op.Give {
			if idx >= 0 {
				w.stacks[idx].Count += op.Count
				changes = append(changes, item.Change{
					Give: true, ClassID: op.ClassID,
					ItemID: w.stacks[idx].ItemID, Total: w.stacks[idx].Count,
				})
				continue
			}
			w.nextID++
			w.stacks = append(w.stacks, item.Stack{
				ItemID: 9000 + w.nextID, ClassID: op.ClassID, Count: op.Count,
			})
			changes = append(changes, item.Change{
				Give: true, ClassID: op.ClassID, ItemID: 9000 + w.nextID, Total: op.Count,
			})
			continue
		}
		left := op.Count
		for i := 0; i < len(w.stacks) && left > 0; i++ {
			if w.stacks[i].ClassID != op.ClassID {
				continue
			}
			spend := left
			if spend > w.stacks[i].Count {
				spend = w.stacks[i].Count
			}
			w.stacks[i].Count -= spend
			left -= spend
			changes = append(changes, item.Change{
				Give: false, ClassID: op.ClassID,
				ItemID: w.stacks[i].ItemID, Total: w.stacks[i].Count,
			})
		}
	}
	return changes, nil
}

// TestGMConsoleReportsEveryLine runs the SHIPPED test console - the translated file
// the zone loads for AGE_REBECCA - and checks the line it prints.
//
// This is the pre-flight for a click nobody should have to debug in game: it proves
// the script's own arithmetic (count 0->1, Vis 0->500, Stone 0->5) and that the two
// refusal cases report 0/0 from a clean bag.
func TestGMConsoleReportsEveryLine(t *testing.T) {
	corpus := filepath.Join("..", "..", "out", "lua_corpus")
	path := filepath.Join(corpus, "zz_gm_test.lua")

	h := New()
	defer h.Close()
	if _, err := h.Index(corpus); err != nil {
		t.Skipf("no corpus at %s (%v)", corpus, err)
	}
	if err := h.LoadFile(path); err != nil {
		t.Fatalf("load the GM console: %v", err)
	}

	world := &gmWorld{props: map[string]int64{}}
	h.Tx = world
	h.Items = world
	if catalog, err := item.Load(filepath.Join("..", "..", "data", "item_catalog.json")); err == nil {
		h.Catalog = catalog
	} else {
		t.Skipf("no item catalog (%v)", err)
	}

	out, err := h.StartClick("SCR_CLICK_AGE_REBECCA", Obj{Name: "GM"},
		Obj{Name: "Tester", Commander: 24}, 2*time.Second)
	if err != nil {
		t.Fatalf("the GM console raised: %v", err)
	}
	if out.Dialog.Kind != DialogOK {
		t.Fatalf("dialog kind = %q, want a line", out.Dialog.Kind)
	}

	// One line, every expectation in it. Checked by PIECE so a failure names the
	// half that broke instead of dumping the whole string.
	for _, want := range []string{
		"count 0->1",    // the property write landed and read back
		"Vis 0->500",    // +1000 then -500, inside ONE transaction
		"Stone 0->5",    // a real item
		"over-take 0/0", // too much: refused at call time AND at TxEnd
		"bad name 0/0",  // unknown item: refused the same way
	} {
		if !strings.Contains(out.Dialog.Text, want) {
			t.Errorf("the GM line does not say %q:\n%s", want, out.Dialog.Text)
		}
	}
}
