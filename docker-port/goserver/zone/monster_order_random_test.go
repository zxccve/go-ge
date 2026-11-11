package zone

// THE TWO RANDOM WALKS - the tests for the `RandomMove` / `RandomMoveCreatedLocaly`
// half of script\monster_order.go.
//
// WHY THEY LIVE HERE and not beside the bindings: the card's pass condition reads the
// SPEED out of the 3550 the ZONE builds (`orderMonsterMove` -> SCR_Get_MON_MSPD), and
// the mesh refusal is the zone's `meshCell`. `zone\monster_order_test.go`'s own header
// already says it tests "zone\monster_order.go and script\monster_order.go"; this file
// adds the two names to that suite without editing a line of it.
//
// THE FIXTURE IS THE SAME ONE: `orderZone` is the chase's zone (class 11591, MSPD 143)
// with two joined sessions and a `NavNear` that answers any point it is asked about on
// cell 29 at height 26 - so the goal a test names is the goal the 3550 carries, and a
// refusal is a refusal. The birth point is captured at (0, 0) BEFORE a body is moved,
// which is the live capture's shape: a bear kited far from where it spawned.

import (
	"math"
	"strings"
	"testing"
)

// TestRandomMoveCreatedLocalyWalksTowardTheBirthPoint is pass condition (a): a body
// 4,000 units from its birth point that calls `RandomMoveCreatedLocaly(self, 500)`
// orders a walk whose GOAL is within 500 of the BIRTH point, at the row's MSPD.
//
// The centre is the whole point of the name: the reference's handler draws around
// `m_createdPos` (`GetRandomPosLocaly(shape, randomPos, m_createdPos, float(arg1))`,
// MonsterNPC.cpp:120-133), which is the same point `GetMyHomeDistance` answers from -
// that is why a kited body walks home on live instead of standing where it stopped.
func TestRandomMoveCreatedLocalyWalksTowardTheBirthPoint(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)
	host.Monster = h
	host.Seed(7)

	// The birth point first, at the spot the body spawned on, then the body is dragged
	// 4,000 units away - the kited bear of the 2026-11-08 capture.
	home, ok := h.Home(mon.Handle)
	if !ok {
		t.Fatalf("the fixture's body has no birth point:\n%s", logs.String())
	}
	if home.X != 0 || home.Y != 0 || home.Z != 26 {
		t.Fatalf("the birth point is (%d, %d, z %d), want (0, 0, z 26) - the fixture's "+
			"mesh answers height 26", home.X, home.Y, home.Z)
	}
	h.setMonsterPos(mon.Handle, [3]int32{4000, 0, 29})

	src := `
		local mon = { Handle = ` + luaNum(mon.Handle) + ` }
		RandomMoveCreatedLocaly(mon, 500)
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for one RandomMoveCreatedLocaly, want exactly 1:\n%s",
			len(moves), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != mon.Handle {
		t.Fatalf("the move names handle 0x%08x, want the monster's 0x%08x", handle, mon.Handle)
	}
	// The wire carries INTEGER points (the reference's cPosition is integer too), so a
	// drawn distance lands at most one unit outside the band it was drawn from.
	d := math.Hypot(float64(goal[0]-home.X), float64(goal[1]-home.Y))
	if d > 501 {
		t.Fatalf("the walk's goal (%d, %d) is %.1f units from the birth point (%d, %d), "+
			"want within 500 - the reference draws around m_createdPos "+
			"(MonsterNPC.cpp:120-133)", goal[0], goal[1], d, home.X, home.Y)
	}
	if goal[2] != 29 {
		t.Fatalf("the goal carries cell %d, want the mesh's 29", goal[2])
	}
	if speed != 143 {
		t.Fatalf("the move carries speed %g, want 143 - SCR_Get_MON_MSPD of class %d, "+
			"the same walk path every script order uses (MonsterNPC.cpp:1929-1932)",
			speed, mon.ClassID)
	}

	// The order is STORED and the body's own roster point was not moved: a binding
	// records a walk, it never advances one.
	order, ok := h.MonsterOrderOf(mon.Handle)
	if !ok || !order.move.has {
		t.Fatalf("the walk was not stored as the body's order (order %+v, ok %v)", order, ok)
	}
	if order.move.goal != goal || order.move.speed != 143 {
		t.Fatalf("the stored order is goal %v speed %g, want %v at 143",
			order.move.goal, order.move.speed, goal)
	}
	if body := h.runtimeMonsters()[0]; body.X != 4000 || body.Y != 0 {
		t.Fatalf("the body stands at (%d, %d) after a random walk ORDER - a binding must "+
			"not move it", body.X, body.Y)
	}
}

// TestRandomMoveWalksWithinTheRingOfTheCurrentPoint is pass condition (b): on a body
// 2,000 units from its birth point, `RandomMove(self, 100, 800)` orders a goal 100..800
// from the body's CURRENT point - the reference's `m_pos`
// (FieldChar_Message.cpp:565-577) - never from the birth point.
func TestRandomMoveWalksWithinTheRingOfTheCurrentPoint(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	host := orderHost(t, h)
	host.Monster = h
	host.Seed(11)

	// Capture the birth point at (0, 0), then put the body 2,000 units away: a goal
	// near the origin would prove the wrong centre was used.
	if _, ok := h.Home(mon.Handle); !ok {
		t.Fatalf("the fixture's body has no birth point:\n%s", logs.String())
	}
	h.setMonsterPos(mon.Handle, [3]int32{2000, 0, 29})

	const calls = 12
	var src strings.Builder
	// `self` is the table the zone refreshes on every think turn (X/Y are the body's
	// live point); the binding reads its own centre from it, as the reference reads
	// m_pos.
	src.WriteString("local mon = { Handle = " + luaNum(mon.Handle) + ", X = 2000, Y = 0, Z = 29 }\n")
	for i := 0; i < calls; i++ {
		src.WriteString("RandomMove(mon, 100, 800)\n")
	}
	if err := host.L.DoString(src.String()); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != calls {
		t.Fatalf("%d x 3550 for %d RandomMove calls, want one walk per draw:\n%s",
			len(moves), calls, logs.String())
	}
	seen := map[[2]int32]bool{}
	for i, pkt := range moves {
		_, goal, speed := decodeMonsterMove(t, pkt)
		d := math.Hypot(float64(goal[0]-2000), float64(goal[1]))
		if d < 99 || d > 801 {
			t.Fatalf("draw %d: goal (%d, %d) is %.1f units from the body's current point "+
				"(2000, 0), want 100..800 - the reference draws around m_pos "+
				"(FieldChar_Message.cpp:565-577)", i, goal[0], goal[1], d)
		}
		if speed != 143 {
			t.Fatalf("draw %d carries speed %g, want 143 (SCR_Get_MON_MSPD)", i, speed)
		}
		seen[[2]int32{goal[0], goal[1]}] = true
	}
	if len(seen) < 2 {
		t.Fatalf("all %d draws landed on the same point - the walk is not random", calls)
	}
}

// TestRandomMoveRefusesWhatTheWorldCannotAnswer is pass condition (c), and the honest
// half of the card: a point the world cannot answer leaves the body where it is and is
// said ONCE, never a silent stub and never a made-up 3550.
func TestRandomMoveRefusesWhatTheWorldCannotAnswer(t *testing.T) {
	t.Run("the mesh cannot place the point", func(t *testing.T) {
		h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
		host := orderHost(t, h)
		host.Monster = h
		if _, ok := h.Home(mon.Handle); !ok {
			t.Fatalf("the fixture's body has no birth point:\n%s", logs.String())
		}
		h.setMonsterPos(mon.Handle, [3]int32{4000, 0, 29})
		h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
			return [3]int32{}, 0, false
		}

		src := `
			local mon = { Handle = ` + luaNum(mon.Handle) + ` }
			RandomMoveCreatedLocaly(mon, 500)
			RandomMoveCreatedLocaly(mon, 500)
		`
		if err := host.L.DoString(src); err != nil {
			t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
		}

		if n := len(movesOf(victim.conn)); n != 0 {
			t.Fatalf("%d x 3550 for a point the mesh cannot place, want 0 - the "+
				"reference sends no OM_MOVE when the world answers false "+
				"(FieldCharFunc.cpp:57-58):\n%s", n, logs.String())
		}
		if body := h.runtimeMonsters()[0]; body.X != 4000 || body.Y != 0 {
			t.Fatalf("the body moved to (%d, %d) on a refused point, want it left at "+
				"(4000, 0)", body.X, body.Y)
		}
		// ONCE per body, however many times the script asks.
		if n := strings.Count(logs.String(), "MONSTER MOVE"); n != 1 {
			t.Fatalf("%d refusal line(s) for two calls, want exactly 1:\n%s", n, logs.String())
		}
		if !strings.Contains(logs.String(), "refused") {
			t.Fatalf("the refusal does not say what happened:\n%s", logs.String())
		}
	})

	t.Run("no birth point on this field", func(t *testing.T) {
		h, logs, victim, _, _ := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
		host := orderHost(t, h)
		host.Monster = h

		src := `
			local ghost = { Handle = 536870911 }
			RandomMoveCreatedLocaly(ghost, 500)
			RandomMoveCreatedLocaly(ghost, 500)
		`
		if err := host.L.DoString(src); err != nil {
			t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
		}
		if n := len(movesOf(victim.conn)); n != 0 {
			t.Fatalf("%d x 3550 for a body this field never spawned, want 0:\n%s",
				n, logs.String())
		}
		if n := strings.Count(logs.String(), "no birth point"); n != 1 {
			t.Fatalf("%d birth-point line(s) for two calls, want exactly 1:\n%s",
				n, logs.String())
		}
	})

	t.Run("a range of zero", func(t *testing.T) {
		h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
		host := orderHost(t, h)
		host.Monster = h
		if _, ok := h.Home(mon.Handle); !ok {
			t.Fatalf("the fixture's body has no birth point:\n%s", logs.String())
		}

		src := `
			local mon = { Handle = ` + luaNum(mon.Handle) + ` }
			RandomMoveCreatedLocaly(mon, 0)
			RandomMoveCreatedLocaly(mon, 0)
		`
		if err := host.L.DoString(src); err != nil {
			t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
		}
		if n := len(movesOf(victim.conn)); n != 0 {
			t.Fatalf("%d x 3550 for range 0, want 0 - GetRandomPosLocaly refuses range 0 "+
				"(Ground.cpp:109-110):\n%s", n, logs.String())
		}
		if n := strings.Count(logs.String(), "refuses range 0"); n != 1 {
			t.Fatalf("%d range line(s) for two calls, want exactly 1:\n%s", n, logs.String())
		}
	})

	t.Run("self without a point", func(t *testing.T) {
		h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
		host := orderHost(t, h)
		host.Monster = h
		if _, ok := h.Home(mon.Handle); !ok {
			t.Fatalf("the fixture's body has no birth point:\n%s", logs.String())
		}

		src := `
			local mon = { Handle = ` + luaNum(mon.Handle) + ` }
			RandomMove(mon, 100, 800)
			RandomMove(mon, 100, 800)
		`
		if err := host.L.DoString(src); err != nil {
			t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
		}
		if n := len(movesOf(victim.conn)); n != 0 {
			t.Fatalf("%d x 3550 with no current point on `self`, want 0 - a wander to "+
				"(0, 0) would be a guess:\n%s", n, logs.String())
		}
		if n := strings.Count(logs.String(), "carries no X/Y"); n != 1 {
			t.Fatalf("%d point line(s) for two calls, want exactly 1:\n%s", n, logs.String())
		}
	})

	t.Run("no world wired", func(t *testing.T) {
		h, logs, victim, _, mon := orderZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
		host := orderHost(t, h)
		host.Monster = h // the home answers; the ORDER world is the one missing
		host.Order = nil

		src := `
			local mon = { Handle = ` + luaNum(mon.Handle) + ` }
			RandomMove(mon, 100, 800)
			RandomMoveCreatedLocaly(mon, 500)
		`
		if err := host.L.DoString(src); err != nil {
			t.Fatalf("the Lua snippet failed: %v\nzone log:\n%s", err, logs.String())
		}
		if n := len(movesOf(victim.conn)); n != 0 {
			t.Fatalf("%d x 3550 with no world wired, want 0:\n%s", n, logs.String())
		}
		if n := strings.Count(logs.String(), "no monster world wired"); n != 1 {
			t.Fatalf("%d no-world line(s) for two calls, want exactly 1:\n%s",
				n, logs.String())
		}
	})
}
