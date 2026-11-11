package zone

import (
	"log"
	"os"
	"testing"
)

// A RESPAWN COMES BACK AT ITS BIRTH POINT, NOT AT THE CORPSE (2026-11-09).
//
// The owner: "they spawn near at the death spot? i thought they spawn at their own spawn spot?"
// The roster entry is LIVE - every walk writes X/Y/Cell back into it (zone\monster_chase.go), and
// the corpse keeps that entry as its template. So the template's X/Y is where the body died, and
// respawning from it re-created the species at the corpse. The birth point is now carried in
// SpawnX/Y/Cell, set once at creation and restored by bringBack.
func TestARespawnedBodyComesBackAtItsSpawnPointNotAtTheCorpse(t *testing.T) {
	h := &Handler{Log: log.New(os.Stderr, "", 0)}

	// The template as the corpse keeps it: born at (1000, 2000), walked to (5000, 6000) and
	// killed there - which is exactly the state a chase leaves behind.
	body := Monster{
		Handle: 0x24530001, X: 5000, Y: 6000, Cell: 44,
		SpawnX: 1000, SpawnY: 2000, SpawnCell: 9,
		ClassID: 12981, Level: 65, MaxHP: 2540,
	}
	c := corpse{handle: body.Handle, body: body, delay: CorpseLeaveAttack}

	// The dead body has already left the roster (takeDueLeaves removes it before the return).
	got, ok := h.bringBack(c)
	if !ok {
		t.Fatalf("bringBack refused")
	}
	if got.X != 1000 || got.Y != 2000 || got.Cell != 9 {
		t.Fatalf("the new body stands at (%d, %d, cell %d), want its birth point (1000, 2000, cell 9) - "+
			"(%d, %d, cell %d) is where the old one died", got.X, got.Y, got.Cell, 5000, 6000, 44)
	}
	if got.Handle == body.Handle {
		t.Fatalf("the new body re-used the dead handle %d", got.Handle)
	}
}

// TestABodySpawnedAtTheOriginKeepsTheOrigin: a body whose birth point is legitimately (0, 0) must
// not be moved by the "no spawn point recorded" guard, which tests both coordinates.
func TestABodySpawnedAtTheOriginKeepsItsPoint(t *testing.T) {
	h := &Handler{Log: log.New(os.Stderr, "", 0)}
	body := Monster{Handle: 0x24530002, X: 700, Y: 800, Cell: 12, SpawnX: 0, SpawnY: 0, SpawnCell: 0, MaxHP: 10}
	got, ok := h.bringBack(corpse{handle: body.Handle, body: body})
	if !ok {
		t.Fatalf("bringBack refused")
	}
	if got.X != 700 || got.Y != 800 {
		t.Fatalf("a body with no recorded spawn point was moved to (%d, %d), want its template (700, 800)",
			got.X, got.Y)
	}
}
