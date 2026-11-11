package zone

import (
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE REPEAT'S FOUR UNANSWERED QUESTIONS (PHASE A - THESE ARE THE TESTS, NOT THE FIX).
//
// The repeat R94 built has one job: keep the ONE 3564 + 3557 pair going at the
// character's own ASPD while the player holds a monster. These four tests ask the four
// things it does NOT do yet, and each one is written so that it FAILS on the code as it
// stands today - that failure is the deliverable, not a mistake:
//
//	1. a press on a NEW target must END the repeat on the OLD one, or the character keeps
//	   hitting the monster the player just clicked away from while it walks to the new
//	   one;
//	2. a SPLASH stance must be given a repeat IT CAN BUILD, and it must not write the
//	   "cannot build it" line at all - while a SINGLE stance is still armed exactly as
//	   before (the negative control, which must PASS). THIS ONE WAS REVERSED BY THE
//	   SPLASH LANE (zone\attack_splash.go):
//	   before zone\attack_splash.go existed, a Splash stance was refused a repeat because
//	   every swing was ONE 3564; the 3566 shape exists now, so the same stance arms a
//	   rhythm that really splashes, and the old expectation ("no repeat") is the bug.
//	3. a press that arrives while a CAST is in progress must send nothing and arm nothing,
//	   because the plain attack would otherwise be a second damage stream under the cast;
//	4. the zone's own tick must run even when the client never stops sending frames -
//	   Serve only reaches the tick on a READ ERROR today, so a client that keeps talking
//	   starves the repeat and the next swing waits for a packet that never comes.
//
// NOTHING SLEEPS EXCEPT IN TEST 4, and that one has to: it is about the real Serve loop
// over a real socket, so its clock is wall time. Tests 1-3 drive the repeat's clock with
// swingDue, the same helper the existing repeat tests use, so they cost no wall time and
// cannot depend on how fast this machine is.

// TestPressedNewTargetEndsTheRepeatOnTheOldOne: ONE repeat per character means the press
// that names a different monster must stop the rhythm the previous monster had, BEFORE
// the character walks to the new one. A repeat left pointing at the old handle is the
// character swinging at a body the player has already clicked away from.
//
// The second monster is put far outside the fixture stance's own reach on purpose: the
// press then sends no hit at all (it walks), so anything that swings afterwards can only
// be the OLD repeat - which is exactly the mistake this test is about.
func TestPressedNewTargetEndsTheRepeatOnTheOldOne(t *testing.T) {
	h, logs, p := attackZone(t)
	const second = 0x20000004
	h.spawnMu.Lock()
	h.spawnedMonster = append(h.spawnedMonster,
		Monster{Handle: second, X: -919, Y: 2711, Cell: 136, ClassID: 11600, Level: 130})
	h.monsterHP[second] = 9000
	h.spawnMu.Unlock()

	// THE FIRST PRESS, on the Scarecrow: one hit and a repeat holding it.
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the first press did not land:\n%s", logs.String())
	}
	if s := p.sess.swingOf(0); s == nil || s.target != capturedTarget {
		t.Fatalf("the first press left no repeat on the clicked target %d (record %v):\n%s",
			capturedTarget, s, logs.String())
	}

	// THE SECOND PRESS, on a monster hundreds of units away: an out-of-reach press walks
	// and sends no hit, so the counts below are not about this press's own damage. The
	// total packet count is deliberately NOT asserted - an approach may send a move.
	pressed := pressAttack(t, h, p.sess, second)
	if got := swingHits(t, pressed); got != 0 {
		t.Fatalf("a press on a target far past the stance's reach hit %d time(s), so the "+
			"fixture is wrong and this test cannot ask about the retarget:\n%s", got,
			logs.String())
	}

	// THE CORE ASSERTION, FIRST, so a pre-fix run fails about the RETARGET and not about
	// a log line: if a repeat is still there, one tick of it can only be a hit on the OLD
	// monster, and that is the bug.
	if s := p.sess.swingOf(0); s != nil {
		swingDue(t, h, p.sess, 0)
		tick := h.swingTick(p.sess)
		t.Fatalf("the press for target %d left the repeat running on target %d: the next "+
			"tick produced %d packet(s) %v - it would keep hitting the OLD monster while "+
			"the character walks to the new one", second, s.target, len(tick), kinds(tick))
	}

	// AND THE LOG NAMES BOTH MONSTERS, so a reader can tell a retarget from a dropped
	// repeat without a debugger: the handle pressed for, the handle it stopped, and how
	// many hits the old rhythm had made.
	var ended string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "ATTACK REPEAT ENDED") &&
			strings.Contains(line, "pressed for target") {
			ended = strings.TrimSpace(line)
			break
		}
	}
	if ended == "" {
		t.Fatalf("the press on target %d did not END the repeat with a line saying it was "+
			"pressed for a new target - the rhythm was dropped silently:\n%s", second,
			logs.String())
	}
	for _, want := range []string{strconv.Itoa(second), strconv.Itoa(int(capturedTarget)),
		"hit(s)"} {
		if !strings.Contains(ended, want) {
			t.Errorf("the ending line does not say %q: %s", want, ended)
		}
	}
	t.Logf("retarget ended the old rhythm: %s", ended)
}

// splashStanceID answers the FIRST stance in this zone's own table that is a Splash
// stance a repeat can be armed in: AtkType Splash, a real ASPD, a stated MaxR, and a
// MinR..MaxR that contains the fixture's stand point.
//
// THE ID IS NEVER WRITTEN HERE. It is read off the same table the handler reads, so the
// day the rows move, this test moves with them; a table with no such row is a FAILURE of
// the premise, said out loud instead of asserted against nothing.
func splashStanceID(t *testing.T, stances *StanceTable) uint32 {
	t.Helper()
	if stances == nil {
		t.Fatal("this zone has no stance table, so no Splash stance can be chosen")
	}
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	for id := 1; id <= 40000; id++ {
		row := stances.Combat(uint32(id))
		if row.AtkType != "Splash" || row.ASPD <= 0 || row.MaxR <= 0 {
			continue
		}
		// The distance is checked with the handler's OWN reach rule, so this test and
		// the press cannot disagree about whether the fixture is in reach.
		if _, in := attackRangeAllowed(attackReachStand, target, row.MinR, row.MaxR,
			PCBodyRadius, 0); !in {
			continue
		}
		return uint32(id)
	}
	t.Fatalf("no stance id in 1..40000 is AtkType \"Splash\" with an ASPD, a MaxR and a " +
		"band that contains the fixture's stand point - the row this test is written " +
		"against is missing from datatable_stance.xml")
	return 0
}

// TestSplashStancePressArmsTheSplashRepeat: a press in a Splash stance answers the SPLASH
// shape and arms a repeat, because the shape the repeat replays now exists
// (zone\attack_splash.go). It used to be named ...ArmsNoRepeat and assert the
// opposite: every swing was ONE 3564, so a rhythm in that stance was a stream of single
// hits wearing a splash name. The 3566 is built now, the swing goes through the same
// Handler.attackAt the press does, and the circle is recomputed at swing time - so the
// refusal is gone and the line that used to say why is gone with it.
//
// THE ID IS READ FROM THE TABLE (splashStanceID), never written here, and the fixture has
// ONE monster - the main target - so the count byte is 0 and the reply is two packets.
func TestSplashStancePressArmsTheSplashRepeat(t *testing.T) {
	stances, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	stance := splashStanceID(t, stances)
	h, logs, p := attackZoneAt(t, stance, attackReachStand)

	const notBuilt = "ATTACK REPEAT NOT BUILT"

	// 1. THE PRESS ANSWERS THE SPLASH SHAPE: 3566 then the main target's 3557, with no
	// extra victim (this fixture runs one monster and it is the one clicked).
	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 {
		t.Fatalf("one press in Splash stance %d produced %d packet(s) %v, want 2 (3566 "+
			"then 3557):\n%s", stance, len(out), kinds(out), logs.String())
	}
	if got := cmdOf(out[0]); got != CmdZCAttackSplash {
		t.Fatalf("the press in Splash stance %d answered cmd %d, want %d "+
			"ZC_ATTACK_SPLASH:\n%s", stance, got, CmdZCAttackSplash, logs.String())
	}
	if got := out[0][AttackSplashCount]; got != 0 {
		t.Errorf("the count byte is %d with one monster in the zone (the main target), "+
			"want 0", got)
	}
	if got := cmdOf(out[1]); got != CmdZCAddHP {
		t.Fatalf("the second packet is cmd %d, want %d", got, CmdZCAddHP)
	}

	// 2. AND THE REPEAT IS ARMED, on the body the player clicked.
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("stance %d is AtkType %q and this build builds that shape, yet the press "+
			"armed no repeat:\n%s", stance, h.Stances.Combat(stance).AtkType,
			logs.String())
	}
	if s.target != capturedTarget {
		t.Fatalf("the repeat follows target %d, want the clicked %d", s.target,
			capturedTarget)
	}

	// 3. NO REFUSAL LINE, and the rhythm's own line is there instead.
	if n := strings.Count(logs.String(), notBuilt); n != 0 {
		t.Fatalf("%d %q line(s) after a splash press, want 0 - the shape exists now:\n%s",
			n, notBuilt, logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT: commander"); line == "" {
		t.Fatalf("the splash press armed a repeat but wrote no rhythm line:\n%s",
			logs.String())
	}

	// 4. ONE SLOT, still. The cadence is not this test's question, so its clock is put far
	// enough in the past for a second press to land - the same trick
	// TestTheRepeatFollowsANewTarget uses.
	p.sess.noteAttack(0, time.Now().Add(-time.Hour))
	if out := pressAttack(t, h, p.sess, capturedTarget); len(out) != 2 {
		t.Fatalf("the second press in Splash stance %d produced %v, want 2:\n%s",
			stance, kinds(out), logs.String())
	}
	if n := len(p.sess.swings); n != 1 {
		t.Fatalf("%d repeat slot(s) after two presses, want 1 (one per character)", n)
	}
	if len(p.sess.swings) == 1 && p.sess.swingOf(0) == nil {
		t.Fatal("the second press dropped the repeat instead of re-arming it")
	}
	t.Logf("splash stance %d: %s", stance,
		lineWith(logs.String(), "ATTACK SPLASH"))
}

// TestSingleStancePressStillArmsTheRepeat is the negative control for the Splash rule: a
// stance whose AtkType IS Single, which is the shape every swing in this build has, must
// still be armed exactly as before. Without this, "no repeat" could be implemented by
// refusing every stance and the Splash test above would pass on a zone that never swings.
func TestSingleStancePressStillArmsTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)

	stance := p.sess.Team[0].Stance
	if atkType := h.Stances.Combat(stance).AtkType; atkType != "Single" {
		t.Fatalf("the fixture stands in stance %d whose AtkType is %q, want Single - this "+
			"control is written against a Single stance", stance, atkType)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("one press in Single stance %d did not land one hit:\n%s", stance,
			logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("a Single stance press armed no repeat - every swing this build makes is "+
			"the Single shape, so this is the case that MUST keep its rhythm:\n%s",
			logs.String())
	}
	if s.target != capturedTarget {
		t.Fatalf("the repeat follows target %d, want the clicked %d", s.target,
			capturedTarget)
	}
	if strings.Contains(logs.String(), "ATTACK REPEAT NOT BUILT") {
		t.Fatalf("a Single stance press wrote the Splash refusal:\n%s", logs.String())
	}
	t.Logf("single stance %d still armed: %s", stance,
		lineWith(logs.String(), "ATTACK REPEAT: commander"))
}

// doubleStanceID answers the FIRST stance in this zone's own table that is a Double
// stance a press could actually swing in: AtkType "Double", a real ASPD, a stated MaxR,
// and a MinR..MaxR that contains the fixture's stand point. It answers false when the
// table holds no such row, which is what lets the caller fall back to a synthetic row.
//
// THE ID IS NEVER WRITTEN HERE, for the reason splashStanceID gives: the day the rows
// move, this test moves with them. The scan reaches past 40000 (splashStanceID's bound)
// because the shipped Double row is 120081 "Smasher" - a bound of 40000 would miss it and
// quietly fall back to the synthetic row, which says less.
func doubleStanceID(stances *StanceTable) (uint32, bool) {
	if stances == nil {
		return 0, false
	}
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	for id := 1; id <= 200000; id++ {
		row := stances.Combat(uint32(id))
		if row.AtkType != "Double" || row.ASPD <= 0 || row.MaxR <= 0 {
			continue
		}
		if _, in := attackRangeAllowed(attackReachStand, target, row.MinR, row.MaxR,
			PCBodyRadius, 0); !in {
			continue
		}
		return uint32(id), true
	}
	return 0, false
}

// TestDoubleStancePressLandsOneHitAndArmsNoRepeat is the UNSUPPORTED AtkType guard, which
// lost its test when the splash lane reversed the old "a Splash stance arms no repeat"
// expectation (attack_swing.go's header). The guard itself did not move: armSwing's
// `switch atkType` has a `default` that REFUSES the repeat for any type this build cannot
// build, and Double is the one such type in the shipped table. Nothing else exercises that
// branch any more, so this test does.
//
// WHAT IT PINS, in the order the code answers:
//
//	1. the press itself is UNTOUCHED - one 3564 and its 3557, a normal hit;
//	2. NO repeat is armed (`swingOf(0) == nil`), because a stream of SINGLE swings behind a
//	   name that promises two hits would be the player seeing one body of a pair take
//	   damage for ever;
//	3. the refusal is SAID ONCE, naming the stance and its AtkType, so a reader can tell it
//	   from the other "no AtkType at all" line which shares the same prefix;
//	4. and a tick straight afterwards really produces nothing, which is the observable
//	   half of (2).
//
// THE ROW IS READ FROM datatable_stance.xml WHEN THE TREE HAS ONE (stance 120081 "Smasher"
// is the shipped Double row), and only otherwise is a synthetic row put on the fixture's
// own stance id. NO STANCE ID IS WRITTEN INTO PRODUCTION CODE, and none is invented here
// either when the table can answer.
func TestDoubleStancePressLandsOneHitAndArmsNoRepeat(t *testing.T) {
	h, logs, p := attackZone(t)

	stance := p.sess.Team[0].Stance
	if id, ok := doubleStanceID(h.Stances); ok {
		stance = id
		p.sess.Team[0].Stance = id
		t.Logf("using the shipped Double stance %d (%q)", id, h.stanceName(id))
	} else {
		// A synthetic row on the fixture's OWN stance id: the builder needs a row that
		// says Double and a reach that contains the stand point, and nothing else.
		h.Stances.combat[int(stance)] = StanceCombat{
			AtkType: "Double", ASPD: 1000, MinR: 0, MaxR: 500,
		}
		t.Logf("no reachable Double row in this tree's stance table - synthetic row on "+
			"stance %d", stance)
	}
	if atkType := h.Stances.Combat(stance).AtkType; atkType != "Double" {
		t.Fatalf("stance %d reads AtkType %q, want Double - the guard this test is about "+
			"is the `default` branch, and it only runs for a type the build cannot build",
			stance, atkType)
	}

	// 1. THE PRESS LANDS ITS ONE HIT. swingHits also holds the 3564/3557 pairing.
	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 1 {
		t.Fatalf("one press in the Double stance %d landed %d hit(s), want 1 - the press "+
			"is untouched by the repeat's refusal:\n%s", stance, got, logs.String())
	}
	if got := cmdOf(out[0]); got != CmdZCAttackSingle {
		t.Fatalf("the press in Double stance %d answered cmd %d, want %d "+
			"ZC_ATTACK_SINGLE (a Double stance has no measured second hit, so the press "+
			"keeps the Single shape):\n%s", stance, got, CmdZCAttackSingle, logs.String())
	}

	// 2. AND NO REPEAT IS ARMED.
	if s := p.sess.swingOf(0); s != nil {
		t.Fatalf("the press in Double stance %d armed a repeat on target %d - the build "+
			"cannot make a Double's second hit, so a rhythm here would stream SINGLE hits "+
			"under a name that promises two:\n%s", stance, s.target, logs.String())
	}

	// 3. THE REFUSAL IS SAID ONCE, and it names the stance and the type.
	const notBuilt = "ATTACK REPEAT NOT BUILT"
	if n := strings.Count(logs.String(), notBuilt); n != 1 {
		t.Fatalf("%d %q line(s) for one press in Double stance %d, want exactly 1 (once "+
			"per run, not once per click):\n%s", n, notBuilt, stance, logs.String())
	}
	line := lineWith(logs.String(), notBuilt)
	for _, want := range []string{strconv.Itoa(int(stance)), `"Double"`} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal line does not say %s: %s", want, line)
		}
	}

	// 4. AND THE TICK REALLY HAS NOTHING TO FIRE (there is no record to advance, so this
	// asks the tick itself rather than swingDue, which fails when there is none).
	if tick := h.swingTick(p.sess); len(tick) != 0 {
		t.Fatalf("a tick after the refused Double press produced %v, want nothing - no "+
			"repeat should exist to fire", kinds(tick))
	}
	t.Logf("double stance %d: one hit, no repeat: %s", stance, line)
}

// TestPressDuringACastSendsNothingAndArmsNothing: while a cast is in progress the
// character is not in the plain-attack state (PlayerChar.cpp:1987-2002), so a 3666 that
// arrives then must send NOTHING - no 3564, no 3557, and no 3568 either: this is one
// press the character cannot act on, not a refusal the player is waiting to hear about -
// and it must not arm a rhythm underneath the cast.
//
// THE LINE IS ONCE per press, and it names the skill that is casting, so a reader can
// tell "the guard fired for the right cast" from "something else said so".
func TestPressDuringACastSendsNothingAndArmsNothing(t *testing.T) {
	h, logs, p := attackZone(t)

	skill, row := firstCastableSkill(t, h, p.sess.Team[0].Stance)
	// The cast has to be ACCEPTED for this test to mean anything; a refusal here is a
	// different question (a reach, a stance row) and is not this test's.
	if out := h.startCast(p.sess, 0, protocol.Frame{Cmd: CmdCZSkillTarget},
		false, skill, capturedTarget, row); out == nil {
		t.Skipf("the zone refused to start skill %d on char 0 - this test cannot ask "+
			"about a press during the cast:\n%s", skill, logs.String())
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("a press during a cast produced %d hit(s), want 0 - the plain attack would "+
			"be a second damage stream running under the cast:\n%s", got, logs.String())
	}
	if len(out) != 0 {
		t.Fatalf("a press during a cast sent %v, want NOTHING at all (no %d, no %d and no "+
			"%d either):\n%s", kinds(out), CmdZCAttackSingle, CmdZCAddHP,
			CmdZCAttackDisable, logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatalf("a press during a cast armed a plain-attack repeat - the two damage "+
			"streams would then both be live:\n%s", logs.String())
	}

	const inProgress = "a cast is in progress"
	line := lineWith(logs.String(), inProgress)
	if line == "" {
		t.Fatalf("the press during a cast wrote no line saying why it was ignored:\n%s",
			logs.String())
	}
	if !strings.Contains(line, strconv.Itoa(int(skill))) {
		t.Errorf("the line does not name the skill that is casting (%d): %s", skill, line)
	}
	if n := strings.Count(logs.String(), inProgress); n != 1 {
		t.Fatalf("%d %q line(s) for one press, want exactly 1 - the guard must not say it "+
			"twice for one frame:\n%s", n, inProgress, logs.String())
	}
	t.Logf("ignored during the cast: %s", line)
}

// packetsOfCmd counts one command id in a packet stream. It reads the id the same way
// cmdOf does, so it counts what the client would dispatch on and nothing else.
func packetsOfCmd(pkts [][]byte, cmd uint16) int {
	n := 0
	for _, p := range pkts {
		if len(p) >= 2 && cmdOf(p) == cmd {
			n++
		}
	}
	return n
}

// attackServeServer builds a WHOLE zone - command table, stance table, one character, one
// monster already in the world and one observer already in the zone - and only THEN
// starts Serve on a loopback socket.
//
// WHY A NEW HELPER AND NOT tickWalkServer (zone\serve_walk_test.go): that one leaves
// h.Stances nil and spawns no monster, so a press sent through it could not be answered
// with a hit at all. And EVERY field must be set BEFORE `go h.Serve(...)`: the serve
// goroutine reads them on the first packet, so a field filled in afterwards would be a
// race between the test and the loop rather than a fixture.
//
// The TCP socket, not net.Pipe, for the reason serve_walk_test.go gives: net.Pipe is
// unbuffered, so Serve's write would block until this test read it, and the test would
// have to know every reply's exact size.
func attackServeServer(t *testing.T, spawn [3]int32, team []Character) (*Handler, *player,
	net.Conn, *protocol.Spec) {
	t.Helper()

	table, err := protocol.LoadSpec("../../data/cmd_table_live_z.txt")
	if err != nil {
		t.Skipf("zone command table not readable (%v)", err)
	}
	stances, err := LoadStances(stanceDir())
	if err != nil {
		t.Skipf("stance tables not readable (%v)", err)
	}
	chars := team
	h := &Handler{
		Table:   table,
		Cipher:  protocol.DefaultCipher,
		Log:     log.New(io.Discard, "", 0),
		Spawn:   spawn,
		Stances: stances,
		TeamOf:  func(_ context.Context, _ uint32) ([]Character, error) { return chars, nil },
	}

	// THE WORLD, BEFORE THE LOOP EXISTS. The monster is 100 units from the spawn along
	// y, inside a sword stance's own MinR..MaxR, and it has HP in the ledger - without an
	// entry there the plain attack refuses to send anything at all, because both packets
	// carry an HP this zone could not name.
	h.spawnedMonster = []Monster{{Handle: capturedTarget, X: 1000, Y: 1100, Cell: 1,
		ClassID: 11600, Level: 1, HP: 1000000}}
	h.monsterHP = map[uint32]float64{capturedTarget: 5000}

	// The observer joins FIRST: when the player's team joins afterwards, its characters
	// are announced to the observer through a recordConn, which never blocks. What it was
	// told before Serve started is cleared, so every packet it holds later came from the
	// loop under test.
	obs := newPlayer(0x40000041, "Obs", [3]int32{9000, 9000, 1})
	h.joinZone(obs.sess)
	obs.conn.writes = nil

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	served := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			close(served)
			return
		}
		h.Serve(context.Background(), conn)
		close(served)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return h, obs, client, table
}

// TestServeKeepsTickingWhileTheClientKeepsSendingFrames is the TICK STARVATION test, and
// it needs the real loop: Serve sets a read deadline only while a tick is wanted, and
// reaches its tick (chaseTick, which is what fires a repeat's swing) ONLY on a read
// error. A client that keeps the socket busy never gives it that error, so the read
// returns data every time and the tick is never reached - the repeat then waits for a
// packet that, per attack_swing.go's own header, never comes until the player clicks.
//
// So: press once, then keep the socket busy with a 3666 every 5 ms (far faster than
// pollInterval) for two attack delays. The zone's own tick must still advance at least
// once in that window and put a swing on the wire. The threshold is "the press's own hit
// plus at least one more", because a second hit inside this window can only be the
// tick's - see step 3 for why the busy presses name an unknown handle.
func TestServeKeepsTickingWhileTheClientKeepsSendingFrames(t *testing.T) {
	spawn := [3]int32{1000, 1000, 1}
	team := []Character{
		{Handle: 0x40000031, Name: "Jin", Job: 1, Level: 100, HP: 500, MaxHP: 900,
			Stance: 10560, Position: spawn},
	}
	team[0].Equip[5] = 7201 // a real sword row, so ATK is the sheet's and not 0

	h, obs, client, table := attackServeServer(t, spawn, team)
	logInAs(t, client, table)

	// 1. ONE PRESS OVER THE WIRE, the way the client sends it.
	sendWire(t, client, attackFrame(0, capturedTarget), table)

	// The first hit proves three things at once: the login finished, the character can
	// reach the monster, and the fan-out works. If it never arrives the fixture is wrong
	// and no conclusion about the tick can be drawn from anything below.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && packetsOfCmd(obs.conn.sent(), CmdZCAttackSingle) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if packetsOfCmd(obs.conn.sent(), CmdZCAttackSingle) == 0 {
		t.Fatalf("the press never reached the observer - the fixture is wrong, not the "+
			"tick (the character stands in stance %d, %v, and this zone has no path "+
			"engine, so an out-of-reach press sends nothing)", team[0].Stance,
			team[0].Position)
	}

	// 2. THE CADENCE, read with the same call the handler makes. team[0] is the very
	// element the session's own team slice holds (TeamOf hands the same backing array
	// over), so this is the stance startGame resolved, not the one guessed above.
	interval := attackInterval(h.sheetFor(team[0]))
	if interval <= 0 {
		t.Fatalf("the sheet gives this character no attack delay (stance %d), so there is "+
			"no cadence to be starved", team[0].Stance)
	}

	// 3. STEADY INBOUND FRAMES for two attack delays: far faster than pollInterval, so
	// the read NEVER returns an error and the old loop never reached its tick.
	//
	// WHY THESE PRESSES NAME A CHARACTER THIS TEAM DOES NOT HAVE, and not capturedTarget.
	// A second press for the target already held is held only WHILE THE CADENCE IS
	// UNSPENT: an attack delay later it does land, and it lands a hit of its own
	// (`ATTACK held` is a gate, not a lock). Measured on the code as it stands: three
	// same-target presses in a 2 x ASPD window produced THREE 3564s at the hands of the
	// presses alone, so a count of hits could not tell a tick swing from a press's own
	// hit and the test would pass on a zone whose tick never ran.
	//
	// IT CANNOT BE AN UNKNOWN TARGET HANDLE EITHER: a press naming a monster this zone
	// does not run is a RETARGET as far as the repeat is concerned, and R95 ends the
	// repeat for it - correctly - so the stream would stop the rhythm inside the window
	// and this test would fail on the FIXED code for the wrong reason. A character index
	// outside the team is refused FIRST, before the cast guard, before the retarget and
	// before the target: it sends its 3568 to the actor's own socket and leaves the
	// repeat exactly as it was. So every hit the OBSERVER receives after the first one
	// can only have come from the tick.
	const absentChar = 9 // this team has one character, so index 9 is never one of them
	window := 2*interval + 400*time.Millisecond
	frames := 0
	for end := time.Now().Add(window); time.Now().Before(end); frames++ {
		sendWire(t, client, attackFrame(absentChar, capturedTarget), table)
		time.Sleep(5 * time.Millisecond)
	}

	// 4. WHAT THE OBSERVER REALLY GOT: the press's own hit, plus at least one more -
	// and that one only the tick could have produced.
	pkts := obs.conn.sent()
	hits := packetsOfCmd(pkts, CmdZCAttackSingle)
	if hits < 2 {
		t.Fatalf("%d hit(s) in %d ms of steady inbound frames (%d frame(s) sent) - the "+
			"zone's own tick never ran, so the repeat was starved; Serve must advance the "+
			"tick at least once per pollInterval whatever the read returned", hits,
			int(window/time.Millisecond), frames)
	}

	// 5. AND EVERY HIT ARRIVED WITH ITS NUMBER: a swing must never reach another client
	// without the 3557 that moves the health plate.
	if hps := packetsOfCmd(pkts, CmdZCAddHP); hps != hits {
		t.Fatalf("%d hit(s) and %d HP update(s) reached the observer - a swing must never "+
			"arrive without its HP number", hits, hps)
	}
	t.Logf("%d hit(s) in %d ms while the client sent %d frame(s); cadence %s", hits,
		int(window/time.Millisecond), frames, interval)
}
