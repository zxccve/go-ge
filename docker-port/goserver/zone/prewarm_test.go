// PRE-WARM: entering a map asks the manager for every map its gates lead to (card t_2950bb0a).
package zone

import (
	"errors"
	"io"
	"log"
	"sort"
	"testing"
)

func prewarmHandler(t *testing.T, gates []MapGate, req func(mapID int) error) *Handler {
	t.Helper()
	forgetAllPrewarm()
	return &Handler{
		MapID:       204,
		Prewarm:     true,
		Log:         log.New(io.Discard, "", 0),
		Gates:       gates,
		RequestZone: req,
	}
}

func TestEnteringAMapAsksForEveryGateDestination(t *testing.T) {
	var asked []int
	h := prewarmHandler(t, []MapGate{
		{Name: "to coimbra", ToID: 10},
		{Name: "to reboldoeux", ToID: 15},
		{Name: "unknown destination", ToID: 0}, // a data gap: the gate refuses it itself
		{Name: "back here", ToID: 204},         // this map is not a destination
	}, func(mapID int) error { asked = append(asked, mapID); return nil })

	h.PrewarmGates()
	sort.Ints(asked)
	if len(asked) != 2 || asked[0] != 10 || asked[1] != 15 {
		t.Fatalf("asked for %v, want [10 15]", asked)
	}
}

func TestTheSameMapIsAskedOnceCooldown(t *testing.T) {
	calls := 0
	h := prewarmHandler(t, []MapGate{{Name: "to coimbra", ToID: 10}},
		func(mapID int) error { calls++; return nil })
	h.PrewarmGates()
	h.PrewarmGates()
	h.PrewarmGates()
	if calls != 1 {
		t.Fatalf("the manager was asked %d time(s) inside the cooldown, want 1", calls)
	}
	if _, ok := PrewarmState(10); !ok {
		t.Fatal("the cooldown holds no mark for map 10")
	}
}

func TestARefusedRequestIsAskedAgain(t *testing.T) {
	calls := 0
	h := prewarmHandler(t, []MapGate{{Name: "to coimbra", ToID: 10}}, func(mapID int) error {
		calls++
		if calls == 1 {
			return errors.New("the manager is restarting")
		}
		return nil
	})
	h.PrewarmGates() // refused
	if _, ok := PrewarmState(10); ok {
		t.Fatal("a refused request kept its cooldown mark - the next entry would skip it")
	}
	h.PrewarmGates() // asked again
	if calls != 2 {
		t.Fatalf("the manager was asked %d time(s), want 2", calls)
	}
}

func TestNoPrewarmWithoutTheSwitchOrTheManager(t *testing.T) {
	forgetAllPrewarm()
	asked := 0
	off := &Handler{MapID: 204, Gates: []MapGate{{Name: "x", ToID: 10}},
		Log: log.New(io.Discard, "", 0), RequestZone: func(int) error { asked++; return nil }}
	off.PrewarmGates()
	if asked != 0 {
		t.Fatal("a zone without -prewarm asked the manager")
	}
	on := &Handler{MapID: 204, Prewarm: true, Log: log.New(io.Discard, "", 0),
		Gates: []MapGate{{Name: "x", ToID: 10}}}
	on.PrewarmGates() // no RequestZone wired: must not panic
	if asked != 0 {
		t.Fatal("a zone with no manager asked something")
	}
}
