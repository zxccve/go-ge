package zone

// THE MONSTER'S OWN CHASE - the tests the card names (zone\monster_chase.go).
//
// The rule under test: an AGGRESSIVE-family monster acquires the nearest player
// character within its OWN tactics row's SeekRange (datatable_monsterai.xml:
// MON_AGGRESSIVE says 2500), walks toward it through `Handler.Plan` while the edge
// gap is beyond its own MaxR, sends ONE ZC_MOVE_MONSTER (3550, 26 bytes) per CHANGED
// stand point, stops inside reach (the existing swing owns the cadence and the
// damage), and gives up - walking home - when the target dies, leaves, or gets
// beyond KeepReturnRange (2000) from where the engagement started.
//
// THE FIXTURE IS THE REAL PATH WHERE IT MATTERS: the monster's row lives in a real
// `MonsterTable` (newMonsters), its body radius comes through `monsterBodyRadius`
// (the row's Size), the packets go through the ordinary per-viewer fan-out, and the
// target's body is where the sight ring draws it. The one thing written by hand is
// the monster's attack/chase copy (ATK/MinR/MaxR/ASPD/AssaultRange/ChaseSpeed),
// because that is what SpawnMonsterObject copies from a row - and the XML test below
// pins the REAL row's numbers against the same functions.

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// chaseClassID is Monster_Bigfoot, the row the owner's report is about and the row
// whose MSPD=143 the brief names.
const chaseClassID = uint32(11591)

// chaseRow is the real Bigfoot row's columns, copied from
// ge\xml\datatable_monster.xml:708 (the XML test below reads them from disk). The
// last four are its `Tactics="MON_AGGRESSIVE"` row, which datatable_monsterai.xml
// resolves: AIType AGGRESSIVE, Attack TRUE, SeekRange 2500.
func chaseRow() MonsterRow {
	return MonsterRow{
		ID: chaseClassID, Name: "Monster_Bigfoot", Level: 111, Size: "S", Radius: 50,
		RaceType: "Creature", SizeType: "LARGE",
		Wpn: 75, STR: 19.5, ATKBM: 0, MinR: 2, MaxR: 306, ASPD: 2467, ASPDBM: 0,
		Hits: 1,
		// MSPD 143, MSPDFix 0, MSPD_BM 0 -> SCR_Get_MON_MSPD = 143.
		MSPD: 143, MSPDFix: 0, MSPDBM: 0,
		AIType: "AGGRESSIVE", AIKnown: true, AIAttack: true, SeekRange: 2500,
	}
}

// chaseZone is one Bigfoot and the players a case needs, all in the roster, with
// their sockets cleared of the join burst. The monster starts at (0, 0, cell 29)
// and the row's own reach applies; the path engine is `directPlan`, the same one
// the attack-approach tests use, so the stand point is the reach ring.
func chaseZone(t *testing.T, victimAt, watcherAt [3]int32) (*Handler, *bytes.Buffer, *player, *player, Monster) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Plan = directPlan
	row := chaseRow()
	h.Monsters = newMonsters(map[uint32]MonsterRow{chaseClassID: row})

	victim := newPlayer(0x40000101, "Victim", victimAt)
	victim.sess.Team[0].HP = swingVictimHP
	victim.sess.Team[0].MaxHP = swingVictimHP
	victim.sess.Team[0].Stance = 10560
	watcher := newPlayer(0x40000202, "Watcher", watcherAt)
	watcher.sess.Team[0].Stance = 10560

	h.joinZone(victim.sess)
	h.joinZone(watcher.sess)
	cullPlace(h, victim, victimAt)
	cullPlace(h, watcher, watcherAt)
	victim.conn.writes, watcher.conn.writes = nil, nil

	mon := Monster{
		Handle: MonsterHandle(9),
		X:      0, Y: 0, Cell: 29,
		ClassID: chaseClassID, Level: 111,
		ATK: monsterATK(row), MinR: row.MinR, MaxR: row.MaxR, ASPD: monsterASPD(row),
		AssaultRange:    monsterAssaultRange(row.AI()),
		KeepReturnRange: monsterKeepReturnRange(row.AIType),
		ChaseSpeed:      monsterChaseSpeed(row.AIType, row),
	}
	h.spawnedMonster = []Monster{mon}
	h.monsterHP = map[uint32]float64{mon.Handle: swingMonsterHP}
	return h, logs, victim, watcher, mon
}

// chaseStep drives the zone's own chase clock for the given span at 100 ms a step -
// the same cadence `tickIfDue` gives it in the running server.
func chaseStep(h *Handler, from time.Time, span time.Duration) {
	for i := int64(1); i*int64(pollInterval) <= int64(span); i++ {
		h.monsterChaseTick(from.Add(time.Duration(i) * pollInterval))
	}
}

// decodeMonsterMove reads the fields a case checks out of one 26-byte 3550, using
// the builder's own layout (PKS_ZC_MOVE).
func decodeMonsterMove(t *testing.T, pkt []byte) (handle uint32, pos [3]int32, speed float32) {
	t.Helper()
	if len(pkt) != 26 {
		t.Fatalf("a monster move is %d bytes, the client's table says 26", len(pkt))
	}
	handle = binary.LittleEndian.Uint32(pkt[2:])
	pos = [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[10:])),
		int32(binary.LittleEndian.Uint32(pkt[14:])),
		int32(binary.LittleEndian.Uint32(pkt[18:])),
	}
	speed = math.Float32frombits(binary.LittleEndian.Uint32(pkt[22:]))
	return handle, pos, speed
}

// movesOf answers every 3550 on a connection.
func movesOf(conn *recordConn) [][]byte {
	var out [][]byte
	for _, pkt := range conn.sent() {
		if len(pkt) >= 2 && cmdOf(pkt) == CmdZCMoveMonster {
			out = append(out, pkt)
		}
	}
	return out
}

// swingsOf answers every 3564 on a connection.
func swingsOf(conn *recordConn) [][]byte {
	var out [][]byte
	for _, pkt := range conn.sent() {
		if len(pkt) >= 2 && cmdOf(pkt) == CmdZCAttackSingle {
			out = append(out, pkt)
		}
	}
	return out
}

// TestMonsterChaseWalksIntoReachAndSwings is pass condition 1: a target at 900
// (inside AssaultRange 1000, outside MaxR 306) makes the monster order ONE walk,
// and once the body has walked inside reach a later tick swings - with the player
// never moving.
func TestMonsterChaseWalksIntoReachAndSwings(t *testing.T) {
	victimAt := [3]int32{900, 0, 29}
	h, logs, victim, _, mon := chaseZone(t, victimAt, [3]int32{0, 3000, 29})

	before := victim.sess.Team[0].Position
	start := time.Now()
	chaseStep(h, start, 6*time.Second)

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d monster moves, want exactly 1 for one stand point - log:\n%s",
			len(moves), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != mon.Handle {
		t.Fatalf("the move names handle 0x%08x, want the monster's 0x%08x", handle, mon.Handle)
	}
	if speed != 143 {
		t.Fatalf("the move walks at speed %g, want 143 (SCR_Get_MON_MSPD of the "+
			"Bigfoot row: MSPD 143, no fix and no BM)", speed)
	}
	// The stand point must be ON the reach ring: the edge gap at the goal is inside
	// MaxR, which is what makes the swing land when the body gets there.
	gap := flatDist(goal, victimAt) - (h.monsterBodyRadius(mon) + PCBodyRadius)
	if gap > mon.MaxR {
		t.Fatalf("the stand point (%d, %d) leaves an edge gap of %g, outside MaxR %g - "+
			"the body would walk past its own reach", goal[0], goal[1], gap, mon.MaxR)
	}

	// The walk drew the body into its own reach: the stand point is on the ring, and
	// the body stops the moment it is inside MaxR rather than walking the order out.
	bodies := h.runtimeMonsters()
	if len(bodies) != 1 {
		t.Fatalf("the roster holds %d monster(s), want 1", len(bodies))
	}
	bodyAt := [3]int32{bodies[0].X, bodies[0].Y, bodies[0].Cell}
	if gap := flatDist(bodyAt, victimAt) - (h.monsterBodyRadius(mon) + PCBodyRadius); gap > mon.MaxR {
		t.Fatalf("the body stopped at (%d, %d), edge gap %g - still outside MaxR %g, "+
			"so the swing below would never land", bodyAt[0], bodyAt[1], gap, mon.MaxR)
	}
	if bodyAt == victimAt {
		t.Fatalf("the body stands on the player - a walk into the target is not a stop")
	}

	// THE SWING ARRIVES WITHOUT THE PLAYER MOVING. It is the ordinary swing tick,
	// on a later instant: the chase has already moved the body into reach.
	h.monsterSwingTick(start.Add(6 * time.Second))
	swings := swingsOf(victim.conn)
	if len(swings) != 1 {
		t.Fatalf("%d x 3564 after the walk, want 1 - the chase must hand the body "+
			"to the swing - log:\n%s", len(swings), logs.String())
	}
	if got := victim.sess.Team[0].Position; got != before {
		t.Fatalf("the victim moved from %v to %v - this test is about the MONSTER "+
			"walking, and a moving player would make its reach a different question",
			before, got)
	}
	if n := cmdCount(victim.conn.sent(), CmdZCMovePC); n != 0 {
		t.Fatalf("%d x 3548 on the victim's socket, want 0 - nothing should have "+
			"moved the player", n)
	}
}

// TestMonsterChaseDoesNotAcquireOutsideSeekRange is pass condition 2: a target at
// 2600 is outside the Bigfoot's tactics row SeekRange 2500, so nothing is acquired
// and no 3550 is sent.
//
// THE DISTANCE MOVED WITH THE RULE: this case used to stand at 1200, outside the
// ATTACKER profile's AssaultRange 1000 the chase first used. The acquisition radius
// is now the monster-AI row's own SeekRange (2500), so 1200 is INSIDE it and the
// old number no longer tests anything - 2600 is the same test one radius over.
func TestMonsterChaseDoesNotAcquireOutsideSeekRange(t *testing.T) {
	h, logs, victim, _, _ := chaseZone(t, [3]int32{2600, 0, 29}, [3]int32{0, 3000, 29})

	start := time.Now()
	chaseStep(h, start, 3*time.Second)
	h.monsterSwingTick(start.Add(3 * time.Second))

	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 for a target 2600 away, want 0: 2600 is outside the "+
			"tactics row's SeekRange 2500 - log:\n%s", n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 for a target 2600 away, want 0: it is also outside MaxR 306",
			n)
	}
	if len(h.monsterChases) != 0 {
		t.Fatalf("the chase holds %d engagement(s) for a target it may not acquire",
			len(h.monsterChases))
	}
}

// TestMonsterChaseGivesUpBeyondKeepReturnAndWalksHome is pass condition 3: a target
// that runs beyond KeepReturnRange (2000 from where the engagement started) ends the
// chase and the monster walks home.
func TestMonsterChaseGivesUpBeyondKeepReturnAndWalksHome(t *testing.T) {
	h, logs, victim, _, mon := chaseZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})

	start := time.Now()
	h.monsterChaseTick(start)
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 after the first step, want the walk to the target - log:\n%s",
			n, logs.String())
	}
	home := [3]int32{0, 0, 29}

	// The target runs to 2100 from the encounter's start point: 100 beyond the
	// profile's KeepReturnRange.
	cullPlace(h, victim, [3]int32{2100, 0, 29})
	h.monsterChaseTick(start.Add(pollInterval))

	moves := movesOf(victim.conn)
	if len(moves) != 2 {
		t.Fatalf("%d x 3550 after the target broke the leash, want 2 (the chase and "+
			"the walk home) - log:\n%s", len(moves), logs.String())
	}
	_, goal, _ := decodeMonsterMove(t, moves[1])
	if goal != home {
		t.Fatalf("the give-up order walks to (%d, %d, cell %d), want home (%d, %d, "+
			"cell %d)", goal[0], goal[1], goal[2], home[0], home[1], home[2])
	}
	if st := h.monsterChases[mon.Handle]; st == nil || !st.returning {
		t.Fatalf("the chase state is %+v, want a returning body at home", st)
	}

	// Long enough to walk the first leg home at 143 u/s.
	chaseStep(h, start.Add(pollInterval), 6*time.Second)
	if st := h.monsterChases[mon.Handle]; st != nil {
		t.Fatalf("the body is still chasing after walking home: %+v", st)
	}
	if n := len(movesOf(victim.conn)); n != 2 {
		t.Fatalf("%d x 3550 total, want 2 - the body must idle at home, not re-order "+
			"the same point", n)
	}
	bodies := h.runtimeMonsters()
	if len(bodies) != 1 || bodies[0].X != home[0] || bodies[0].Y != home[1] {
		t.Fatalf("the body is %+v, want it back at home %v", bodies, home)
	}
}

// TestMonsterChaseOrdersOncePerChangedStandPoint is pass condition 4: ten ticks with
// the same stand point send ONE order, and a second viewer receives the SAME bytes.
func TestMonsterChaseOrdersOncePerChangedStandPoint(t *testing.T) {
	h, logs, victim, watcher, _ := chaseZone(t,
		[3]int32{900, 0, 29}, [3]int32{0, 3000, 29})

	start := time.Now()
	for i := 1; i <= 10; i++ {
		h.monsterChaseTick(start.Add(time.Duration(i) * pollInterval))
	}

	mine := movesOf(victim.conn)
	theirs := movesOf(watcher.conn)
	if len(mine) != 1 {
		t.Fatalf("%d x 3550 over ten ticks at one stand point, want 1 - a repeat "+
			"would restart the client's interpolation mid-stride - log:\n%s",
			len(mine), logs.String())
	}
	if len(theirs) != 1 {
		t.Fatalf("the second viewer got %d x 3550, want 1", len(theirs))
	}
	if !bytes.Equal(mine[0], theirs[0]) {
		t.Fatalf("the two viewers got different bytes:\n  %x\n  %x", mine[0], theirs[0])
	}
}

// TestMonsterChaseNeverWalksADeadBody is pass condition 5: a dead monster sends no
// 3550 (it is leave-pending, and the swing tick already skips it).
func TestMonsterChaseNeverWalksADeadBody(t *testing.T) {
	h, logs, victim, _, mon := chaseZone(t, [3]int32{900, 0, 29}, [3]int32{0, 3000, 29})
	h.monsterHP[mon.Handle] = 0

	start := time.Now()
	chaseStep(h, start, 3*time.Second)
	h.monsterSwingTick(start.Add(3 * time.Second))

	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 for a dead body, want 0 - log:\n%s", n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 from a dead body, want 0", n)
	}
	if len(h.monsterChases) != 0 {
		t.Fatalf("the chase holds %d state(s) for a dead body", len(h.monsterChases))
	}
}

// TestBuildMoveMonsterIsThe26ByteHeader pins the wire layout against
// `PKS_ZC_MOVE_MONSTER : public PKS_ZC_MOVE` (geZCPacket.h:1046-1052, :1665).
func TestBuildMoveMonsterIsThe26ByteHeader(t *testing.T) {
	pkt := BuildMoveMonster(0x20000009, [3]int32{-169, 870, 29}, 0x1234, 143)
	if len(pkt) != 26 {
		t.Fatalf("%d bytes, want 26 (data\\cmd_table.txt: 3550 26 PKS_ZC_MOVE_MONSTER)", len(pkt))
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCMoveMonster {
		t.Fatalf("command %d, want %d", cmd, CmdZCMoveMonster)
	}
	if h := binary.LittleEndian.Uint32(pkt[2:]); h != 0x20000009 {
		t.Fatalf("handle 0x%08x", h)
	}
	if tick := binary.LittleEndian.Uint32(pkt[6:]); tick != 0x1234 {
		t.Fatalf("tick %d, want 0x1234", tick)
	}
	if x := int32(binary.LittleEndian.Uint32(pkt[10:])); x != -169 {
		t.Fatalf("x %d, want -169", x)
	}
	if y := int32(binary.LittleEndian.Uint32(pkt[14:])); y != 870 {
		t.Fatalf("y %d, want 870", y)
	}
	if c := int32(binary.LittleEndian.Uint32(pkt[18:])); c != 29 {
		t.Fatalf("cell %d, want 29", c)
	}
	if spd := math.Float32frombits(binary.LittleEndian.Uint32(pkt[22:])); spd != 143 {
		t.Fatalf("speed %g, want 143", spd)
	}
}

// TestBigfootRowCarriesTheChaseColumns reads the REAL row the owner reported and
// pins the numbers the chase uses: the tactics family and its Attack/SeekRange
// resolve through datatable_monsterai.xml, the ACQUISITION radius is that row's
// SeekRange (2500 - replaced the ATTACKER profile's AssaultRange 1000, which is a
// player-side number and was the bug), the leash answers the ATTACKER profile, and
// the chase speed is SCR_Get_MON_MSPD - 143 - not WSPD 130 and not RSPD 856.
func TestBigfootRowCarriesTheChaseColumns(t *testing.T) {
	table, err := LoadMonsters(XMLDir())
	if err != nil {
		t.Skipf("monster table not readable (%v)", err)
	}
	row, ok := table.Row(chaseClassID)
	if !ok {
		t.Skipf("class %d is not in this client's table", chaseClassID)
	}
	if row.Tactics != "MON_AGGRESSIVE" {
		t.Fatalf("class %d names tactics %q, want MON_AGGRESSIVE",
			chaseClassID, row.Tactics)
	}
	if row.AIType != "AGGRESSIVE" {
		t.Fatalf("class %d Tactics %q resolved AIType %q, want AGGRESSIVE",
			chaseClassID, row.Tactics, row.AIType)
	}
	if !row.AIKnown || !row.AIAttack {
		t.Fatalf("class %d's tactics row did not resolve, or says Attack=false "+
			"(AIKnown %v, AIAttack %v) - MON_AGGRESSIVE says Attack=\"TRUE\"",
			chaseClassID, row.AIKnown, row.AIAttack)
	}
	if row.SeekRange != 2500 {
		t.Fatalf("class %d's tactics row SeekRange = %g, want 2500 "+
			"(datatable_monsterai.xml MON_AGGRESSIVE)", chaseClassID, row.SeekRange)
	}
	if got := monsterMSPD(row); got != 143 {
		t.Fatalf("SCR_Get_MON_MSPD = %g, want 143 (MSPD %g, MSPDFix %g, MSPD_BM %g)",
			got, row.MSPD, row.MSPDFix, row.MSPDBM)
	}
	if got := monsterAssaultRange(row.AI()); got != 2500 {
		t.Fatalf("engage range %g, want the tactics row's SeekRange 2500 - the "+
			"ATTACKER profile's AssaultRange 1000 was this chase's first, wrong answer",
			got)
	}
	if got := monsterKeepReturnRange(row.AIType); got != 2000 {
		t.Fatalf("keep-return range %g, want 2000", got)
	}
	if got := monsterChaseSpeed(row.AIType, row); got != 143 {
		t.Fatalf("chase speed %g, want 143", got)
	}
	if row.MaxR != 306 || row.MSPD != 143 {
		t.Fatalf("row %d: MaxR %g, MSPD %g - the brief's numbers", chaseClassID,
			row.MaxR, row.MSPD)
	}
}
