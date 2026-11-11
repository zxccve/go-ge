package zone

// THE MONSTER'S OWN SWING, SLICE 1 - the tests the card names.
//
// The rule under test (zone\monster_swing.go): a monster with a player inside its
// row's own MinR..MaxR acquires the NEAREST one and sends ONE ZC_ATTACK_SINGLE (3564,
// 74 bytes) per cadence, through the ordinary per-viewer fan-out (`fanOut` ->
// `sendOthers`), with the victim's post-hit HP in +50.
//
// THE FIXTURE IS THE REAL PATH WHERE IT MATTERS. The monster's row lives in a real
// `MonsterTable` (newMonsters), its body radius comes through `monsterBodyRadius`, the
// victim's DEF comes from `Handler.sheetFor` (the game's own status script via
// `requireStatus`), and the packets go through `recordConn` sockets. The one thing
// written by hand is the monster's attack copy (ATK/MinR/MaxR/ASPD), because that is
// what `SpawnMonsterObject` copies from a row and the spawn needs a path engine.
//
// THE CADENCE IS THE ROW'S ASPD (400 ms here) - see MonsterSwingCadenceLine for why the
// stored capture cannot answer it (the .bin has no timestamps) and why the row can.

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

const (
	swingMapID      = 1331 // map_table 1331: ViewRange 6000 / ViewMiddleRange 4000
	swingClassID    = uint32(19763)
	swingMonsterHP  = 1e7
	swingVictimHP   = 1e12
	swingMonsterATK = 52427.0 // SCR_Get_MON_DAM of row 19763, pinned by Test...Row
	swingCadenceMS  = 400.0   // SCR_Get_MON_ASPD floor; the capture's own +20 too
)

// swingRow is the row the fixture stands on: the live monster the capture's own
// `emCaptureClass` copies (mon_flealouse1, Lv 140, Size S, Wpn 82, STR 222, MinR 1,
// MaxR 100, ASPD 2460, Hits 1). The numbers are the real XML's (see
// TestMonsterRowCarriesTheAttackColumns, which reads them from disk).
func swingRow() MonsterRow {
	return MonsterRow{
		ID: swingClassID, Name: "mon_flealouse1", Level: 140, Size: "S", Radius: 50,
		RaceType: "None", SizeType: "SMALL",
		Wpn: 82, STR: 222, ATKBM: 0, MinR: 1, MaxR: 100, ASPD: 2460, ASPDBM: 0, Hits: 1,
	}
}

// swingZone is one monster and the players a case needs, all in the roster and with
// their sockets cleared of the join burst. `watcherAt` is where the second player's
// BODY is placed; the body (not the roster's Pos) is what the ring and the reach
// measure, which is why cullPlace is used rather than a bare assignment.
func swingZone(t *testing.T, victimAt, watcherAt [3]int32) (*Handler, *bytes.Buffer, *player, *player, Monster) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Monsters = newMonsters(map[uint32]MonsterRow{swingClassID: swingRow()})

	victim := newPlayer(0x40000101, "Victim", victimAt)
	victim.sess.Team[0].HP = swingVictimHP
	victim.sess.Team[0].MaxHP = swingVictimHP
	victim.sess.Team[0].Stance = 10560 // Peltast: a real stance row for sheetFor
	watcher := newPlayer(0x40000202, "Watcher", watcherAt)
	watcher.sess.Team[0].Stance = 10560

	h.joinZone(victim.sess)
	h.joinZone(watcher.sess)
	cullPlace(h, victim, victimAt)
	cullPlace(h, watcher, watcherAt)
	victim.conn.writes, watcher.conn.writes = nil, nil

	mon := Monster{
		Handle:  MonsterHandle(7),
		X:       0, Y: 0, Cell: 29,
		ClassID: swingClassID, Level: 140,
		ATK: swingMonsterATK, MinR: 1, MaxR: 100, ASPD: swingCadenceMS,

		// A MAP SPAWNER'S BODY. The fixture is hand-built, and a body with no spawner
		// row is never brought back (zone/respawn.go) - so it states the row it stands
		// for, which is the clock the respawn tests below run on.
		HasSpawner:   true,
		SpawnerDelay: fixtureSpawnerDelay,
	}
	h.spawnedMonster = []Monster{mon}
	h.monsterHP = map[uint32]float64{mon.Handle: swingMonsterHP}
	return h, logs, victim, watcher, mon
}

// swingFrame decodes the fields a case checks out of one 74-byte 3564, using the
// builder's own offsets (zone\attack.go) rather than literals.
type swingFrame struct {
	cmd      uint16
	size     int
	attacker uint32
	atkPos   [3]int32
	atkSpd   uint16
	victim   uint32
	victimAt [3]int32
	damage   float64
	hp       float64
	typ      int32
}

func decodeSwing(t *testing.T, pkt []byte) swingFrame {
	t.Helper()
	var f swingFrame
	if len(pkt) < AttackSingleSize {
		t.Fatalf("a swing is %d bytes, shorter than the client's own %d", len(pkt), AttackSingleSize)
	}
	f.cmd = binary.LittleEndian.Uint16(pkt)
	f.size = len(pkt)
	f.attacker = binary.LittleEndian.Uint32(pkt[asAttacker:])
	f.atkPos = [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[asAtkPos:])),
		int32(binary.LittleEndian.Uint32(pkt[asAtkPos+4:])),
		int32(binary.LittleEndian.Uint32(pkt[asAtkPos+8:])),
	}
	f.atkSpd = binary.LittleEndian.Uint16(pkt[asAtkSpd:])
	f.victim = binary.LittleEndian.Uint32(pkt[asTarget:])
	f.victimAt = [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[asTargetPos:])),
		int32(binary.LittleEndian.Uint32(pkt[asTargetPos+4:])),
		int32(binary.LittleEndian.Uint32(pkt[asTargetPos+8:])),
	}
	f.damage = math.Float64frombits(binary.LittleEndian.Uint64(pkt[asDamage:]))
	f.hp = math.Float64frombits(binary.LittleEndian.Uint64(pkt[asTargetHP:]))
	f.typ = int32(binary.LittleEndian.Uint32(pkt[asAttackType:]))
	return f
}

// TestMonsterSwingSendsOne3564PerCadence is pass condition 1: exactly ONE 3564 per
// cadence, the +50 is the victim's STORED HP after the hit, and the damage is the
// reused rule's own answer - `applyDefence(castDamage(ATK, plainAttackPercent, 0),
// DEF/2)`, the player's plain-swing arithmetic with the PC target's defence line.
func TestMonsterSwingSendsOne3564PerCadence(t *testing.T) {
	h, logs, victim, _, mon := swingZone(t,
		[3]int32{0, 150, 29}, // centre 150: 50 of clear ground inside MaxR 100
		[3]int32{0, 3000, 29})

	c := &victim.sess.Team[0]
	sheet := h.sheetFor(*c)
	want := applyDefence(castDamage(mon.ATK, plainAttackPercent, 0), sheet.DEF/2)
	if want < 1 {
		t.Fatalf("the fixture's damage is %g - this test cannot tell a hit from the floor", want)
	}

	t0 := time.Unix(1_700_000_000, 0)
	h.monsterSwingTick(t0)
	sent := victim.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("one cadence produced %d packet(s) on the victim's socket, want exactly 1:\n%s",
			len(sent), logs.String())
	}
	f := decodeSwing(t, sent[0])
	if f.cmd != CmdZCAttackSingle || f.size != AttackSingleSize {
		t.Fatalf("the swing is cmd %d (%d B), want %d (%d B)", f.cmd, f.size,
			CmdZCAttackSingle, AttackSingleSize)
	}
	if f.attacker != mon.Handle || f.atkPos != [3]int32{mon.X, mon.Y, mon.Cell} {
		t.Errorf("attacker is %d at %v, want the monster %d at (%d, %d, %d)",
			f.attacker, f.atkPos, mon.Handle, mon.X, mon.Y, mon.Cell)
	}
	if float64(f.atkSpd) != mon.ASPD {
		t.Errorf("+20 attackSpd = %d, want the row's %g", f.atkSpd, mon.ASPD)
	}
	if f.victim != c.Handle || f.victimAt != [3]int32{0, 150, 29} {
		t.Errorf("victim is %d at %v, want %d at (0, 150, 29)", f.victim, f.victimAt, c.Handle)
	}
	if f.typ != monsterSwingType {
		t.Errorf("+58 type = %d, want %d (a plain hit, no knockdown)", f.typ, monsterSwingType)
	}
	if math.Abs(f.damage-want) > 1e-9 {
		t.Errorf("+42 damage = %g, want %g (applyDefence(castDamage(ATK %g, %d, 0), DEF %g / 2))",
			f.damage, want, mon.ATK, plainAttackPercent, sheet.DEF)
	}
	if c.HP != swingVictimHP-want {
		t.Errorf("the victim's ledger is %g, want %g (its HP fell BEFORE the packet)",
			c.HP, swingVictimHP-want)
	}
	if f.hp != c.HP {
		t.Errorf("+50 HP = %g, want the STORED post-hit HP %g", f.hp, c.HP)
	}

	// A second pass inside the cadence sends nothing; the pass at the cadence sends
	// exactly one more. 399/400 pins the comparison to the interval itself.
	h.monsterSwingTick(t0.Add(399 * time.Millisecond))
	if n := len(victim.conn.sent()); n != 1 {
		t.Fatalf("399 ms after the swing the victim has %d packet(s), want still 1 - "+
			"the cadence is %g ms:\n%s", n, mon.ASPD, logs.String())
	}
	h.monsterSwingTick(t0.Add(400 * time.Millisecond))
	if n := len(victim.conn.sent()); n != 2 {
		t.Fatalf("at the cadence the victim has %d packet(s), want 2 (one per cadence):\n%s",
			n, logs.String())
	}
	if last := decodeSwing(t, victim.conn.sent()[1]); last.hp != c.HP {
		t.Errorf("the second swing's +50 = %g, want the ledger %g", last.hp, c.HP)
	}
	t.Logf("one 3564 per %g ms, damage %g, HP %g -> %g", mon.ASPD, want,
		swingVictimHP, c.HP)
}

// TestMonsterSwingOutsideReachSendsNothing is pass condition 2: a player outside the
// row's MaxR receives nothing and the monster does not move.
func TestMonsterSwingOutsideReachSendsNothing(t *testing.T) {
	h, logs, victim, watcher, mon := swingZone(t,
		[3]int32{0, 5000, 29}, // far outside MinR 1 .. MaxR 100 (edge to edge)
		[3]int32{0, 6000, 29})
	before := victim.sess.Team[0].HP

	h.monsterSwingTick(time.Unix(1_700_000_000, 0))

	if n := len(victim.conn.sent()); n != 0 {
		t.Fatalf("a player %d units away received %d packet(s) - MaxR is %g:\n%s",
			5000, n, mon.MaxR, logs.String())
	}
	if n := len(watcher.conn.sent()); n != 0 {
		t.Fatalf("the second far player received %d packet(s)", n)
	}
	if victim.sess.Team[0].HP != before {
		t.Errorf("an out-of-reach swing moved the victim's HP %g -> %g", before,
			victim.sess.Team[0].HP)
	}
	stored := h.runtimeMonsters()[0]
	if stored.X != mon.X || stored.Y != mon.Y || stored.Cell != mon.Cell {
		t.Errorf("the monster moved: (%d, %d, cell %d) -> (%d, %d, cell %d)",
			mon.X, mon.Y, mon.Cell, stored.X, stored.Y, stored.Cell)
	}
}

// TestMonsterSwingReachesEveryViewerByteIdentical is pass condition 3: the swing is a
// BROADCAST. A watcher inside the view receives the SAME BYTES as the victim, because
// `fanOut` hands the actor's one slice to every recipient unchanged.
func TestMonsterSwingReachesEveryViewerByteIdentical(t *testing.T) {
	h, logs, victim, watcher, mon := swingZone(t,
		[3]int32{0, 150, 29},  // the victim, in reach
		[3]int32{0, 900, 29}) // a watcher inside 6,000 / 4,000, outside the reach

	h.monsterSwingTick(time.Unix(1_700_000_000, 0))

	vs, ws := victim.conn.sent(), watcher.conn.sent()
	if len(vs) != 1 || len(ws) != 1 {
		t.Fatalf("victim got %d packet(s), watcher got %d, want 1 each:\n%s",
			len(vs), len(ws), logs.String())
	}
	if !bytes.Equal(vs[0], ws[0]) {
		t.Fatalf("the watcher's 3564 differs from the victim's - the broadcast must hand "+
			"over the same bytes:\n got % x\nwant % x", ws[0], vs[0])
	}
	f := decodeSwing(t, ws[0])
	if f.attacker != mon.Handle || f.victim != victim.sess.Team[0].Handle {
		t.Errorf("the watcher's copy names attacker %d victim %d, want %d / %d",
			f.attacker, f.victim, mon.Handle, victim.sess.Team[0].Handle)
	}
}

// TestMonsterSwingIsCulledPastTheMiddleRange is pass condition 4 - the reference's own
// trap. A viewer whose body stands past the map's ViewMiddleRange (but inside
// ViewRange) stays in the world and receives NO update. The monster has no owning
// session, so this is what proves `cullActorAt` can measure a monster's body; without
// it the same fan-out would treat every monster packet as "no cull".
func TestMonsterSwingIsCulledPastTheMiddleRange(t *testing.T) {
	h, logs, victim, watcher, _ := swingZone(t,
		[3]int32{0, 150, 29},  // in reach
		[3]int32{0, 4500, 29}) // inside 6,000, past the 4,000 middle

	if _, _, armed := h.sightRadii(); !armed {
		t.Fatal("the sight ring is not armed, so the cull is not being tested")
	}
	h.monsterSwingTick(time.Unix(1_700_000_000, 0))

	if n := len(victim.conn.sent()); n != 1 {
		t.Fatalf("the victim received %d packet(s), want 1 - the swing must still happen:\n%s",
			n, logs.String())
	}
	if n := len(watcher.conn.sent()); n != 0 {
		t.Fatalf("a watcher %d units from the monster (middle range 4,000) received %d "+
			"packet(s) %v - the cull is missing for a monster's swing",
			4500, n, watcher.conn.sent())
	}
	// THE MONSTER IS FROZEN, NOT GONE: no 3525 either.
	for _, pkt := range watcher.conn.sent() {
		if binary.LittleEndian.Uint16(pkt) == CmdZCLeaveMonster {
			t.Errorf("the culled watcher was told the monster left - it is frozen, not gone")
		}
	}
}

// TestMonsterSwingStopsOnADeadBody is pass condition 5: a dead monster never swings; a
// monster whose target is at 0 HP or has logged out acquires nobody and idles.
func TestMonsterSwingStopsOnADeadBody(t *testing.T) {
	t.Run("dead monster", func(t *testing.T) {
		h, logs, victim, _, mon := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
		h.monsterHP[mon.Handle] = 0 // the ledger says the body is empty

		h.monsterSwingTick(time.Unix(1_700_000_000, 0))
		if n := len(victim.conn.sent()); n != 0 {
			t.Fatalf("a monster at 0 HP swung %d time(s):\n%s", n, logs.String())
		}
	})

	t.Run("target at 0 HP", func(t *testing.T) {
		h, logs, victim, _, _ := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
		victim.sess.Team[0].HP = 0

		h.monsterSwingTick(time.Unix(1_700_000_000, 0))
		if n := len(victim.conn.sent()); n != 0 {
			t.Fatalf("a monster swung at an emptied body %d time(s):\n%s", n, logs.String())
		}
		if bytes.Contains(logs.Bytes(), []byte("MONSTER KILL")) {
			t.Errorf("a swing was logged against an emptied body:\n%s", logs.String())
		}
	})

	t.Run("target logged out", func(t *testing.T) {
		h, logs, victim, _, _ := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
		h.Players.Remove(victim.sess)

		h.monsterSwingTick(time.Unix(1_700_000_000, 0))
		if n := len(victim.conn.sent()); n != 0 {
			t.Fatalf("a monster swung at a session that left the zone %d time(s):\n%s",
				n, logs.String())
		}
	})

	t.Run("the kill is logged and the body is skipped", func(t *testing.T) {
		h, logs, victim, _, _ := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
		victim.sess.Team[0].HP = 1 // one hit empties it

		h.monsterSwingTick(time.Unix(1_700_000_000, 0))
		if victim.sess.Team[0].HP != 0 {
			t.Fatalf("the victim's HP is %g after a lethal swing, want 0", victim.sess.Team[0].HP)
		}
		if !bytes.Contains(logs.Bytes(), []byte("MONSTER KILL")) {
			t.Errorf("an emptied body did not produce a MONSTER KILL line:\n%s", logs.String())
		}
		// The next cadence must not swing at the emptied body again.
		h.monsterSwingTick(time.Unix(1_700_000_000, 0).Add(time.Second))
		if n := len(victim.conn.sent()); n != 1 {
			t.Fatalf("after the kill the victim received %d swing(s), want exactly the one "+
				"that killed it:\n%s", n, logs.String())
		}
	})
}

// TestPlayerAttackIsUnaffectedByAMonsterSwing is pass condition 6's own case: with a
// monster swinging on its clock, the player's press still answers 3564 + 3557, exactly
// as before this slice. (The rest of condition 6 is the suite staying green.)
func TestPlayerAttackIsUnaffectedByAMonsterSwing(t *testing.T) {
	h, logs, victim, _, mon := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})

	h.monsterSwingTick(time.Unix(1_700_000_000, 0))
	if n := len(victim.conn.sent()); n != 1 {
		t.Fatalf("the monster did not swing before the press (%d packets):\n%s",
			n, logs.String())
	}
	victim.conn.writes = nil

	out := pressAttack(t, h, victim.sess, mon.Handle)
	if len(out) != 2 {
		t.Fatalf("the player's press answered %v, want the 3564 and the 3557:\n%s",
			kinds(out), logs.String())
	}
	if got := kinds(out); got[0] != CmdZCAttackSingle || got[1] != CmdZCAddHP {
		t.Fatalf("the player's answer is %v, want %d then %d", got,
			CmdZCAttackSingle, CmdZCAddHP)
	}
}

// TestMonsterSwingRidesTheZoneClock is the wiring half of pass condition 1: the swing
// is not run by the test's own call, it is run by `tickIfDue` - the same pass the death
// clock and the sight ring ride - and its packet is written by the zone (a monster has
// no session to return it to), not handed back in the session's batch.
func TestMonsterSwingRidesTheZoneClock(t *testing.T) {
	h, logs, victim, _, _ := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	victim.sess.lastTick = time.Time{}

	out, ran := h.tickIfDue(victim.sess)
	if !ran {
		t.Fatalf("a session standing in front of a monster that can swing did not tick - " +
			"monsterSwingArmed is not a reason to run the clock")
	}
	if len(out) > 0 {
		for _, pkt := range out {
			if binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
				t.Fatalf("the tick returned the monster's 3564 to ONE session - a monster's "+
					"swing must go to every viewer through the fan-out, not into a session's "+
					"own batch")
			}
		}
	}
	if n := len(victim.conn.sent()); n != 1 {
		t.Fatalf("the zone's own tick produced %d swing(s) on the victim's socket, want 1:\n%s",
			n, logs.String())
	}
}

// TestAMonsterIsAReasonToRunTheClock proves `monsterSwingArmed` itself: with the sight
// ring OFF (so sightArmed cannot be the reason) and a session with no work of its own,
// a monster that can swing must still make tickIfDue run. Without it the clock would
// stop on the first quiet moment and the monster would never swing at a standing player.
func TestAMonsterIsAReasonToRunTheClock(t *testing.T) {
	h, _, victim, _, _ := swingZone(t, [3]int32{0, 150, 29}, [3]int32{0, 3000, 29})
	h.SightOff = true
	victim.sess.lastTick = time.Time{}
	if victim.sess.wantsTick() || h.deadPending() || h.sightArmed() {
		t.Fatal("the fixture has another reason to tick - this test cannot isolate the monster")
	}
	if _, ran := h.tickIfDue(victim.sess); !ran {
		t.Fatalf("tickIfDue answered false with a swinging monster in the zone")
	}
	if n := len(victim.conn.sent()); n != 1 {
		t.Fatalf("the victim received %d swing(s), want 1", n)
	}
}

// TestMonsterSwingPicksTheNearestPlayer pins the ACQUISITION: with two characters
// inside the row's reach, the nearer one is the target (the packet names it and only its
// ledger moves); the other still receives the broadcast, which is what "nearest target"
// means rather than "only the target is told".
func TestMonsterSwingPicksTheNearestPlayer(t *testing.T) {
	h, logs, near, far, _ := swingZone(t,
		[3]int32{0, 130, 29}, // 30 of clear ground inside MaxR 100
		[3]int32{0, 190, 29}) // 90 - still inside, but farther
	farHP := far.sess.Team[0].HP

	h.monsterSwingTick(time.Unix(1_700_000_000, 0))

	if n := len(near.conn.sent()); n != 1 {
		t.Fatalf("the nearer player received %d packet(s), want 1:\n%s", n, logs.String())
	}
	f := decodeSwing(t, near.conn.sent()[0])
	if f.victim != near.sess.Team[0].Handle {
		t.Fatalf("the swing names victim %d, want the nearer character %d",
			f.victim, near.sess.Team[0].Handle)
	}
	if near.sess.Team[0].HP >= swingVictimHP {
		t.Errorf("the nearer player's HP did not fall")
	}
	if far.sess.Team[0].HP != farHP {
		t.Errorf("the farther player's HP moved %g -> %g - the NEAREST one is the target",
			farHP, far.sess.Team[0].HP)
	}
	// The broadcast still reaches the farther player: it is a viewer, not the victim.
	if n := len(far.conn.sent()); n != 1 {
		t.Errorf("the farther player received %d packet(s), want the broadcast", n)
	}
}

// TestMonsterSwingDamageReusesThePlainSwingRule pins the damage function itself with a
// NON-ZERO defence: the fixture's bare level-9 victim has DEF 0, so the packet test
// alone could not tell the defence multiply from a missing one.
func TestMonsterSwingDamageReusesThePlainSwingRule(t *testing.T) {
	for _, def := range []float64{0, 50, 100, 200} {
		want := applyDefence(castDamage(swingMonsterATK, plainAttackPercent, 0), def/2)
		if got := monsterSwingDamage(swingMonsterATK, def); got != want {
			t.Errorf("monsterSwingDamage(ATK %g, DEF %g) = %g, want %g "+
				"(applyDefence(castDamage(ATK, %d, 0), DEF/2))",
				swingMonsterATK, def, got, want, plainAttackPercent)
		}
	}
	// A DEF high enough to erase the hit still lands for the floor of 1, not 0: a 0
	// reads as a miss the client animates as a death.
	if got := monsterSwingDamage(swingMonsterATK, 1e6); got != 1 {
		t.Errorf("damage at DEF 1e6 = %g, want the floor 1", got)
	}
}

// TestMonsterRowCarriesTheAttackColumns pins the DATA the swing stands on: the real
// datatable_monster.xml row and the real datatable_monster_shape.xml row, read through
// the same loader the defence slice uses. It would go red if a column name changed or
// the shape join was dropped - the failure mode that would otherwise be a silent
// ATK 0.
func TestMonsterRowCarriesTheAttackColumns(t *testing.T) {
	dir := stanceDir()
	monsters, err := LoadMonsters(dir)
	if err != nil {
		t.Skipf("no client xml folder at %s: %v", dir, err)
	}

	row, ok := monsters.Row(swingClassID)
	if !ok {
		t.Fatalf("monster %d is not in the table", swingClassID)
	}
	want := swingRow()
	if row.Name != want.Name || row.Level != want.Level {
		t.Errorf("row %d is %q Lv %d, want %q Lv %d", swingClassID, row.Name, row.Level,
			want.Name, want.Level)
	}
	if row.Wpn != want.Wpn || row.STR != want.STR || row.ATKBM != want.ATKBM {
		t.Errorf("the ATK columns are Wpn %g STR %g ATK_BM %g, want %g / %g / %g",
			row.Wpn, row.STR, row.ATKBM, want.Wpn, want.STR, want.ATKBM)
	}
	if row.MinR != want.MinR || row.MaxR != want.MaxR {
		t.Errorf("the reach columns are MinR %g MaxR %g, want %g / %g",
			row.MinR, row.MaxR, want.MinR, want.MaxR)
	}
	if row.ASPD != want.ASPD || row.ASPDBM != want.ASPDBM {
		t.Errorf("the speed columns are ASPD %g ASPD_BM %g, want %g / %g",
			row.ASPD, row.ASPDBM, want.ASPD, want.ASPDBM)
	}
	if row.Hits != want.Hits {
		t.Errorf("the shape's Hits = %g, want %g (datatable_monster_shape.xml, joined "+
			"through the row's Shape)", row.Hits, want.Hits)
	}
	if got := monsterATK(row); got != swingMonsterATK {
		t.Errorf("monsterATK = %g, want %g (SCR_Get_MON_DAM of Wpn %g STR %g Lv %d "+
			"Hits %g)", got, swingMonsterATK, row.Wpn, row.STR, row.Level, row.Hits)
	}
	if got := monsterASPD(row); got != 2460 {
		t.Errorf("monsterASPD = %g, want 2460 (ASPD %g, ASPD_BM %g)", got, row.ASPD, row.ASPDBM)
	}

	// THE SCARECROW IS THE COUNTER-CASE: ATK 0 and MaxR 0 in its own row, so it never
	// swings - the hit dummy must not hit back.
	dummy, ok := monsters.Row(11600)
	if !ok {
		t.Fatal("monster 11600 (the Scarecrow) is not in the table")
	}
	if got := monsterATK(dummy); got != 0 || dummy.MaxR != 0 {
		t.Errorf("the Scarecrow reads ATK %g MaxR %g, want 0 / 0", got, dummy.MaxR)
	}
}
