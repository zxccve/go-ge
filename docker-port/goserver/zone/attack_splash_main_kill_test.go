package zone

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// A SPLASH'S MAIN TARGET MUST FALL LIKE ANY OTHER BODY THE SAME HIT KILLS.
//
// f56bb353 gave every EXTRA victim this hit kills the fatal form - type 6 (the type the
// client reads a KNOCKDOWN_INFO on) plus the 37-byte block the single-target killing blow
// carries. The body the player actually CLICKED kept the plain 74-byte main block, so a
// splash kill of the main target still blinked out: `CNormalNet::AttackSplash`
// (source2\geClient\Source\GENormalNet_Action.cpp:270-345) handles the MAIN block through
// the same switch a victim's record goes through, and with type 0 it consumes no block and
// never calls SetKnockDownInfo.
//
// So when the main hit is lethal (`before > 0 && after == 0`, the condition takeHP arms
// the ZC_DEAD on) the packet is BuildAttackSplashMainKnockdown: the main 111 bytes are
// BuildAttackSingleKnockdown's own - type 6 and the block - and the count and the records
// move to +111/+112 with it. A main hit that SURVIVES is BuildAttackSplash, byte for byte
// as before.
//
// THE BLOCK'S VALUES ARE NOT RE-DERIVED HERE: every test below compares the bytes with
// `BuildAttackSingleKnockdown`'s own, the same way attack_splash_kill_test.go compares a
// victim's block, so the two fatal forms and their fall geometry cannot drift apart.

// splashMainWalk is one 3566 read the way CNormalNet::AttackSplash reads it: the MAIN
// HIT_INFO first, then - when the main type is a knockdown flavour - its 37-byte
// KNOCKDOWN_INFO, and only THEN the count and the victim records.
//
// THE ORDER IS THE POINT OF THE WHOLE CHANGE: the client advances the main block before
// it looks at +74, so a killing main hit that left the count at +74 would have its count
// read out of the block's own startPos bytes. This walk takes the count where the client
// takes it, and fails when the packet leaves any remainder but the one unused record.
type splashMainWalk struct {
	mainType int32
	mainKD   []byte // the block after the main hit, nil when the type carries none
	countAt  int    // where the count byte was actually read
	count    int
	recs     []splashRecord
	end      int // first byte after the last record the client reads
}

func parseSplashMainLikeClient(t *testing.T, pkt []byte) splashMainWalk {
	t.Helper()
	if len(pkt) < AttackSplashBase {
		t.Fatalf("the 3566 is %d bytes, shorter than the %d-byte main block",
			len(pkt), AttackSplashBase)
	}
	w := splashMainWalk{mainType: int32(binary.LittleEndian.Uint32(pkt[asAttackType:]))}
	// THE CLIENT'S ORDER, not the builder's: the victim count is the 3566's own last
	// struct member at +74 and is read FIRST (`FUN_004e2302` loops on
	// `*(byte *)(packet + 0x4a)`), and only then does a knockdown main type place a
	// 37-byte KNOCKDOWN_INFO at +75 (`FUN_004e11bc` = packet + 0x4b). Walking it the
	// other way round - block at +74, count after it - is the mistake this test exists
	// to catch: the packet is the same LENGTH either way and every record still lands
	// on +112.
	off := AttackSplashCount
	if off >= len(pkt) {
		t.Fatalf("the 3566 is %d bytes, shorter than the %d-byte main block plus its "+
			"count byte", len(pkt), AttackSplashCount)
	}
	w.countAt = off
	w.count = int(pkt[off])
	off++
	if splashTypeReadsKnockdown(w.mainType) {
		if off+KnockDownInfoSize > len(pkt) {
			t.Fatalf("the main type is %d, so the client reads a %d-byte "+
				"KNOCKDOWN_INFO at +%d, past the %d-byte packet", w.mainType,
				KnockDownInfoSize, off, len(pkt))
		}
		w.mainKD = append([]byte(nil), pkt[off:off+KnockDownInfoSize]...)
		off += KnockDownInfoSize
	}
	for i := 0; i < w.count; i++ {
		if off+SplashVictimSize > len(pkt) {
			t.Fatalf("record %d of %d starts at +%d, past the %d-byte packet - the "+
				"count and the bytes disagree", i, w.count, off, len(pkt))
		}
		rec := splashRecord{
			off:      off,
			size:     SplashVictimSize,
			handle:   binary.LittleEndian.Uint32(pkt[off+svHandle:]),
			pos:      worldPos(pkt[off+svPos:]),
			damage:   math.Float64frombits(binary.LittleEndian.Uint64(pkt[off+svDamage:])),
			hpBefore: math.Float64frombits(binary.LittleEndian.Uint64(pkt[off+svHPBefore:])),
			typ:      int32(binary.LittleEndian.Uint32(pkt[off+svAttackType:])),
			knockPos: worldPos(pkt[off+svKnockPos:]),
		}
		off += SplashVictimSize
		if splashTypeReadsKnockdown(rec.typ) {
			if off+KnockDownInfoSize > len(pkt) {
				t.Fatalf("record %d (handle %d) states type %d, so the client reads a "+
					"%d-byte KNOCKDOWN_INFO at +%d, past the %d-byte packet",
					i, rec.handle, rec.typ, KnockDownInfoSize, off, len(pkt))
			}
			rec.kd = append([]byte(nil), pkt[off:off+KnockDownInfoSize]...)
			rec.size = SplashKillingVictimSize
			off += KnockDownInfoSize
		}
		w.recs = append(w.recs, rec)
	}
	w.end = off
	if rest := len(pkt) - off; rest != SplashVictimSize {
		t.Fatalf("the client's walk of the %d-byte 3566 (main type %d, count %d) "+
			"stopped at +%d and left %d byte(s); the only bytes after the last record "+
			"may be the one %d-byte unused record", len(pkt), w.mainType, w.count, off,
			rest, SplashVictimSize)
	}
	for i, b := range pkt[off:] {
		if b != 0 {
			t.Fatalf("the unused record's byte +%d is 0x%02x, want 0 (the one record "+
				"after the last real one is all zero in 16 of 16 measured splashes)",
				off+i, b)
		}
	}
	return w
}

// splashMainKillZone is the measured splash fixture with the CLICKED body at 1 HP, so
// this press IS its kill, and two extras inside the 220-unit circle: one at 1 HP that
// dies with it and one at 1e9 that stands. The main kill and a per-victim kill are in
// the SAME packet, which is what keeps the two forms from being proved apart.
func splashMainKillZone(t *testing.T) (h *Handler, logs *stringBuf, p *player, killed, survivor uint32) {
	t.Helper()
	h, logs, p = splashZone(t)
	h.monsterHP[capturedTarget] = 1
	killed = splashExtra(t, h, 0x20004011, scarecrow().X+100, scarecrow().Y, 1)
	survivor = splashExtra(t, h, 0x20004012, scarecrow().X, scarecrow().Y+120, 1e9)
	return h, logs, p, killed, survivor
}

// splashMainFields reads the main block's own numbers back out of a built 3566, so the
// control packet is built from what the handler actually sent.
func splashMainFields(hit []byte) (attacker uint32, atkPos, mainPos [3]int32,
	damage, mainHP float64, spd uint16) {

	return binary.LittleEndian.Uint32(hit[asAttacker:]), worldPos(hit[asAtkPos:]),
		worldPos(hit[asTargetPos:]),
		math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:])),
		math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:])),
		binary.LittleEndian.Uint16(hit[asAtkSpd:])
}

// splashVictimsOf rebuilds the victim list from the records the packet carries, which is
// what lets a test ask "what would TODAY'S builder have sent for these same bodies".
func splashVictimsOf(recs []splashRecord) []SplashVictim {
	out := make([]SplashVictim, 0, len(recs))
	for _, rec := range recs {
		out = append(out, SplashVictim{
			Handle:     rec.handle,
			Pos:        rec.pos,
			Damage:     rec.damage,
			HPBefore:   rec.hpBefore,
			AttackType: rec.typ,
			Killing:    rec.typ == knockdownAttackType,
		})
	}
	return out
}

// TestSplashMainKillCarriesTheFatalForm is item 1: the main block of a splash whose MAIN
// target dies is type 6 and carries the 37-byte KNOCKDOWN_INFO the single-target killing
// blow sends, and the packet is today's plus exactly that block - the main 74 bytes, the
// count and every record otherwise unmoved.
func TestSplashMainKillCarriesTheFatalForm(t *testing.T) {
	h, logs, p, _, _ := splashMainKillZone(t)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)

	attacker, atkPos, mainPos, damage, mainHP, spd := splashMainFields(hit)

	// THE CLIENT'S OWN DEADLY CONDITION on the main block: hp is BEFORE the hit and must
	// not exceed the damage, or SetDeadlyHit never runs (GeAttack.cpp:42) and no fall can
	// follow. This fixture is meant to KILL the main target, so a hit that does not is a
	// broken fixture, not a passing test.
	if !(mainHP > 0 && mainHP <= damage) {
		t.Fatalf("the main block says HP-before %g, damage %g - the client only calls "+
			"SetDeadlyHit for hp <= damage, so this fixture is not the kill it claims "+
			"to be:\n%s", mainHP, damage, logs.String())
	}

	// TYPE 6: the type that carries the block.
	if got := int32(binary.LittleEndian.Uint32(hit[asAttackType:])); got != knockdownAttackType {
		t.Fatalf("the main block's type is %d, want %d (the type the client reads a "+
			"KNOCKDOWN_INFO on) - a dead main target still blinks out with %d:\n%s",
			got, knockdownAttackType, splashMainAttackType, logs.String())
	}

	// THE MAIN BLOCK IS THE SINGLE-TARGET KILLING BLOW'S, split around the count byte:
	// +4..+73 is the block the two packets share, +74 is the count, and +75..+111 is
	// that builder's 37-byte KNOCKDOWN_INFO. Compared with the builder's bytes rather
	// than a transcription, so type, HP, damage and the fall move together.
	if len(hit) < AttackSplashKDFirst {
		t.Fatalf("the 3566 is %d bytes, shorter than the %d-byte killing main form",
			len(hit), AttackSplashKDFirst)
	}
	single := BuildAttackSingleKnockdown(attacker, atkPos, spd, capturedTarget, mainPos,
		damage, mainHP, knockdownAttackType)
	if !bytes.Equal(hit[4:AttackSplashCount], single[4:AttackSingleSize]) {
		t.Fatalf("the main %d bytes (+4..+%d) are not the single-target killing "+
			"blow's:\n got %x\nwant %x", AttackSplashCount-4, AttackSplashCount-1,
			hit[4:AttackSplashCount], single[4:AttackSingleSize])
	}
	if !bytes.Equal(hit[AttackSplashMainKD:AttackSplashKDFirst], single[AttackSingleSize:]) {
		t.Fatalf("the main KNOCKDOWN_INFO at +%d is not the single-target killing "+
			"blow's block (which sits at +%d there):\n got %x\nwant %x",
			AttackSplashMainKD, AttackSingleSize,
			hit[AttackSplashMainKD:AttackSplashKDFirst], single[AttackSingleSize:])
	}

	// THE LENGTH IS TODAY'S + EXACTLY ONE BLOCK, and "today's" is the current builder fed
	// the victim records this packet carries - so the 37 is the only difference left.
	walk := parseSplashMainLikeClient(t, hit)
	if walk.mainKD == nil {
		t.Fatalf("the killing main block carries no KNOCKDOWN_INFO - the client would "+
			"never call SetKnockDownInfo:\n%s", logs.String())
	}
	today := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos, damage,
		mainHP, splashMainAttackType, splashVictimsOf(walk.recs))
	if got, want := len(hit), len(today)+KnockDownInfoSize; got != want {
		t.Fatalf("the killing-main splash is %d bytes, want today's %d + one %d-byte "+
			"KNOCKDOWN_INFO = %d:\n%s", got, len(today), KnockDownInfoSize, want,
			logs.String())
	}
	if got := int(binary.LittleEndian.Uint16(hit[2:])); got != len(hit) {
		t.Fatalf("the length word at +2 is %d, the frame is %d bytes - the +2 word "+
			"must carry the block too", got, len(hit))
	}
	// NOTHING BUT THE TYPE AND THE BLOCK MOVED: everything from +4 to the type, and the
	// knock-back position after it, is today's main block; +74 is today's count and +112
	// onwards is today's records, byte for byte. Only +2 (the length), the type and the
	// block's arrival between them differ besides.
	if !bytes.Equal(hit[4:asAttackType], today[4:asAttackType]) {
		t.Fatalf("the main block's fields before the type changed:\n got %x\nwant %x",
			hit[4:asAttackType], today[4:asAttackType])
	}
	if !bytes.Equal(hit[asKnockPos:AttackSplashBase], today[asKnockPos:AttackSplashBase]) {
		t.Fatalf("the main block's knock-back position changed:\n got %x\nwant %x",
			hit[asKnockPos:AttackSplashBase], today[asKnockPos:AttackSplashBase])
	}
	if hit[AttackSplashCount] != today[AttackSplashCount] {
		t.Fatalf("the count moved: +%d is %d in the killing form and %d in the "+
			"ordinary one - it is the same byte in both (FUN_004e2302 reads it at +0x4a)",
			AttackSplashCount, hit[AttackSplashCount], today[AttackSplashCount])
	}
	if !bytes.Equal(hit[AttackSplashKDFirst:], today[AttackSplashFirst:]) {
		t.Fatalf("the records are not today's bytes 37 along:\n got %x\nwant %x",
			hit[AttackSplashKDFirst:], today[AttackSplashFirst:])
	}
	t.Logf("%d B = today's %d + %d, main type %d, count at +%d, %d record(s)",
		len(hit), len(today), KnockDownInfoSize, walk.mainType, walk.countAt,
		len(walk.recs))
}

// TestSplashMainSurvivorKeepsTodaysBytes is item 2: the same packet shape with a main
// target that LIVES is byte-identical to the builder's current output - no block, the
// count at +74, the records at +75 - so the fatal form cannot leak into an ordinary hit.
func TestSplashMainSurvivorKeepsTodaysBytes(t *testing.T) {
	h, logs, p, _, _ := splashKillZone(t) // the Scarecrow has 1e9 HP here: it survives

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)

	attacker, atkPos, mainPos, damage, mainHP, spd := splashMainFields(hit)
	if mainHP <= damage {
		t.Fatalf("the fixture's main hit is lethal (HP %g <= damage %g), so it cannot "+
			"test the surviving form:\n%s", mainHP, damage, logs.String())
	}

	walk := parseSplashMainLikeClient(t, hit)
	if walk.mainKD != nil {
		t.Fatalf("the main target SURVIVED but its block carries a %d-byte "+
			"KNOCKDOWN_INFO - only a killing main hit may:\n%s", KnockDownInfoSize,
			logs.String())
	}
	if walk.countAt != AttackSplashCount {
		t.Fatalf("the count byte sits at +%d on a SURVIVING main hit, want today's "+
			"+%d - nothing may move without a kill", walk.countAt, AttackSplashCount)
	}
	if walk.mainType != splashMainAttackType {
		t.Fatalf("the surviving main block states type %d, want %d "+
			"(splashMainAttackType):\n%s", walk.mainType, splashMainAttackType,
			logs.String())
	}

	today := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos, damage,
		mainHP, splashMainAttackType, splashVictimsOf(walk.recs))
	if !bytes.Equal(hit, today) {
		t.Fatalf("a splash whose main target survives is not byte-identical to today's "+
			"builder:\n got %x\nwant %x\n%s", hit, today, logs.String())
	}
	t.Logf("%d B, count %d, main type %d, %d record(s) - today's bytes exactly",
		len(hit), walk.count, walk.mainType, len(walk.recs))
}

// TestSplashMainKillClientWalk is item 3: walk the built packet exactly as the client
// does - main hit, its block when the type reads one, THEN the count, then each victim's
// hit and its block - and land on the last record with only the one 48-byte unused record
// left. No remainder of any other length, no read past the end.
func TestSplashMainKillClientWalk(t *testing.T) {
	h, logs, p, killed, survivor := splashMainKillZone(t)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)

	walk := parseSplashMainLikeClient(t, hit) // fails on any remainder but the unused record
	if walk.mainType != knockdownAttackType || walk.mainKD == nil {
		t.Fatalf("the main block is type %d, block present %v - want type %d and a "+
			"%d-byte KNOCKDOWN_INFO:\n%s", walk.mainType, walk.mainKD != nil,
			knockdownAttackType, KnockDownInfoSize, logs.String())
	}
	if walk.countAt != AttackSplashKDCount {
		t.Fatalf("the client read the count at +%d, want +%d - the count does not move "+
			"in the killing form; the %d-byte KNOCKDOWN_INFO goes after it, at +%d:\n%s",
			walk.countAt, AttackSplashKDCount, KnockDownInfoSize, AttackSplashMainKD,
			logs.String())
	}
	if walk.count != 2 {
		t.Fatalf("the client reads %d victim record(s), want 2:\n%s", walk.count,
			logs.String())
	}
	if walk.end != len(hit)-SplashVictimSize {
		t.Fatalf("the client's walk ended at +%d in a %d-byte packet; want +%d - the "+
			"last %d bytes are the one unused record", walk.end, len(hit),
			len(hit)-SplashVictimSize, SplashVictimSize)
	}

	// THE ARITHMETIC FROM THE CLIENT'S SIDE, so the walk is not its own oracle:
	// 74 (main) + 37 (block) + 1 (count) + the records + 48 (unused) = the packet.
	total := AttackSplashBase + KnockDownInfoSize + 1
	for _, rec := range walk.recs {
		total += rec.size
	}
	total += SplashVictimSize
	if total != len(hit) {
		t.Fatalf("the client's fields account for %d of the packet's %d bytes",
			total, len(hit))
	}

	// EACH RECORD IS WHERE THIS HIT PUT THE BODY: the extra at 1 HP dies and carries its
	// own block, the 1e9 extra survives with the plain 48 bytes - both read AFTER the
	// main block, which is what proves the cursor was where the client expects it.
	byHandle := map[uint32]splashRecord{}
	for _, rec := range walk.recs {
		byHandle[rec.handle] = rec
	}
	dead, ok := byHandle[killed]
	if !ok || dead.kd == nil || dead.size != SplashKillingVictimSize {
		t.Fatalf("the extra %d is not a %d-byte killing record (present %v, block %v): "+
			"%v", killed, SplashKillingVictimSize, ok, ok && dead.kd != nil,
			recHandles(walk.recs))
	}
	alive, ok := byHandle[survivor]
	if !ok || alive.kd != nil || alive.size != SplashVictimSize {
		t.Fatalf("the extra %d is not a %d-byte surviving record (present %v, block %v)",
			survivor, SplashVictimSize, ok, ok && alive.kd != nil)
	}
	t.Logf("%d B walked: main %d + block %d + count 1 + %d + %d + unused %d = %d",
		len(hit), AttackSplashBase, KnockDownInfoSize, dead.size, alive.size,
		SplashVictimSize, total)
}

// TestSplashMainKillLeavesTheVictimsAndTheirHPAlone is item 4: the main block gaining its
// block must not take anything from the bodies beside it - the per-victim records stay the
// ones f56bb353 writes, one 3557 per body still goes out with the same delta, and the
// ledger owes exactly the two deaths this hit made: the main target and the 1 HP extra.
func TestSplashMainKillLeavesTheVictimsAndTheirHPAlone(t *testing.T) {
	h, logs, p, killed, survivor := splashMainKillZone(t)

	out := pressAttack(t, h, p.sess, capturedTarget)
	hit := splashHitOf(t, out)
	walk := parseSplashMainLikeClient(t, hit)
	attacker, atkPos, mainPos, damage, mainHP, spd := splashMainFields(hit)

	// THE RECORDS ARE TODAY'S, compared against the builder with the same victim inputs.
	today := BuildAttackSplash(attacker, atkPos, spd, capturedTarget, mainPos, damage,
		mainHP, splashMainAttackType, splashVictimsOf(walk.recs))
	if !bytes.Equal(hit[AttackSplashKDFirst:], today[AttackSplashFirst:]) {
		t.Fatalf("the count and the victim records are not the ones the current builder "+
			"writes for the same bodies:\n got %x\nwant %x",
			hit[AttackSplashKDFirst:], today[AttackSplashFirst:])
	}

	// ONE 3557 PER BODY: the main target and both extras, each carrying the hit's own
	// delta, and the plates of the two dead bodies at 0.
	var hps [][]byte
	for _, pkt := range out {
		if cmdOf(pkt) == CmdZCAddHP {
			hps = append(hps, pkt)
		}
	}
	if len(hps) != 3 {
		t.Fatalf("%d 3557(s), want 3 (the main target and both extras):\n%s",
			len(hps), logs.String())
	}
	seen := map[uint32]float64{}
	for _, pkt := range hps {
		if got := math.Float64frombits(binary.LittleEndian.Uint64(pkt[ahAddHP:])); got != damage {
			t.Errorf("a 3557 states a delta of %g, want the hit's own %g", got, damage)
		}
		seen[binary.LittleEndian.Uint32(pkt[ahHandle:])] = math.Float64frombits(
			binary.LittleEndian.Uint64(pkt[ahNowHP:]))
	}
	if got, ok := seen[capturedTarget]; !ok || got != 0 {
		t.Errorf("the main target's 3557 carries %v (present %v), want 0 - it was the "+
			"kill the main block's type 6 is for", got, ok)
	}
	if got, ok := seen[killed]; !ok || got != 0 {
		t.Errorf("the killed extra's 3557 carries %v (present %v), want 0", got, ok)
	}
	if got, ok := seen[survivor]; !ok || got <= 0 {
		t.Errorf("the survivor's 3557 carries %v (present %v), want the HP it has left",
			got, ok)
	}

	// THE PACKETS AGREE WITH THE LEDGER: two deaths, the main target and the 1 HP extra -
	// not the survivor, and not the main target twice. They leave on each corpse's own
	// clock (2026-11-09, zone/respawn.go deathPacketTick); this fixture's roster holds no
	// player, so the kill lines are what name the bodies they were built for.
	deadTick(t, h, capturedTarget)
	deadTick(t, h, killed)
	deaths := deadLines(logs.String())
	if len(deaths) != 2 {
		t.Fatalf("the press sent %d death(s), want 2 (the main target %d and the extra "+
			"%d):\n%s", len(deaths), capturedTarget, killed, logs.String())
	}
	if !deaths[capturedTarget] || !deaths[killed] || deaths[survivor] {
		t.Fatalf("the deaths are %v, want the main target %d and the extra %d and no "+
			"other", deaths, capturedTarget, killed)
	}
	t.Logf("%d B: main type %d + block, %d victim record(s) unchanged, 3 3557s, "+
		"deaths owed for %d and %d", len(hit), walk.mainType, len(walk.recs),
		capturedTarget, killed)
}

// THE KILLING SPLASH, READ BACK AT THE TWO OFFSETS THE CLIENT ITSELF USES.
//
// This is the packet-level proof for the fatal 3566, independent of the handler: the
// 2020 client's 3566 handler `FUN_004e2302` (`docker-port\out\ge_decompiled.txt:162755`)
// takes its record cursor from `FUN_004e11bc` = `packet + 0x4b` (+75, `:161966-161972`),
// keeps that as the KNOCKDOWN_INFO pointer when the main type is 4..7, and loops on
// `*(byte *)(packet + 0x4a)` - the count at +74 (`:162854`, `:162973`). `FUN_004e5fd3`
// (`:166615`) then reads the block at startPos +0, endPos +0xc, delay +0x18, power
// +0x1a, vAngle +0x1c, hAngle +0x20 and type +0x24. If the block sits at +74 instead,
// every one of those lands one byte early and the count is read out of startPos.x - the
// packet is still the same LENGTH, which is why this needs its own test.
func TestSplashMainKillBlockIsReadAtTheClientsOffsets(t *testing.T) {
	const (
		attacker = uint32(1794)
		target   = uint32(536870925)
		spd      = uint16(400)
		extra    = uint32(777)
	)
	atkPos := [3]int32{1846, -1431, 114}
	mainPos := [3]int32{1596, -1275, 34}
	extraPos := [3]int32{1500, -1200, 34}
	const damage, mainHP = 5094.0, 50.0

	pkt := BuildAttackSplashMainKnockdown(attacker, atkPos, spd, target, mainPos,
		damage, mainHP, []SplashVictim{{
			Handle: extra, Pos: extraPos, Damage: damage, HPBefore: 900.0,
			AttackType: splashVictimAttackType,
		}})

	// SIZE: the 74-byte main block + one count byte + the 37-byte block + one 48-byte
	// record + the unused 48-byte record. 74 + 1 + 37 + 48 + 48 = 208.
	if want := AttackSplashKDFirst + 2*SplashVictimSize; len(pkt) != want {
		t.Fatalf("the killing splash is %d bytes, want %d (%d + 1 + %d + 2 x %d)",
			len(pkt), want, AttackSplashCount, KnockDownInfoSize, SplashVictimSize)
	}
	if got := int(binary.LittleEndian.Uint16(pkt[2:])); got != len(pkt) {
		t.Fatalf("the length word at +2 is %d, the frame is %d bytes", got, len(pkt))
	}
	if got := binary.LittleEndian.Uint16(pkt[0:]); got != CmdZCAttackSplash {
		t.Fatalf("command is %d, want %d", got, CmdZCAttackSplash)
	}

	// THE CLIENT'S CONDITION, on the main block: hp is BEFORE the hit and must not
	// exceed the damage, or SetDeadlyHit never runs and no fall can follow.
	hp := math.Float64frombits(binary.LittleEndian.Uint64(pkt[asTargetHP:]))
	dmg := math.Float64frombits(binary.LittleEndian.Uint64(pkt[asDamage:]))
	if !(hp <= dmg) {
		t.Fatalf("the main block says HP-before %g > damage %g: the client will not "+
			"call SetDeadlyHit (FUN_0061379f, ge_decompiled.txt:519694)", hp, dmg)
	}
	if got := int32(binary.LittleEndian.Uint32(pkt[asAttackType:])); got != knockdownAttackType {
		t.Fatalf("the main type is %d, want %d - the type the client reads a block on",
			got, knockdownAttackType)
	}
	if got := binary.LittleEndian.Uint32(pkt[asAttacker:]); got != attacker {
		t.Fatalf("the killer at +4 is %d, want %d - that handle becomes the actor the "+
			"client compares against GetKillMeActor()", got, attacker)
	}

	// THE COUNT IS AT +74, IN THE KILLING FORM TOO. One extra victim here.
	if got := pkt[AttackSplashCount]; got != 1 {
		t.Fatalf("the count byte at +%d is %d, want 1 - the client reads it at +0x4a "+
			"before it ever looks at the block", AttackSplashCount, got)
	}

	// AND THE BLOCK IS AT +75, read the way FUN_004e5fd3 reads it.
	kd := pkt[AttackSplashMainKD:AttackSplashKDFirst]
	if len(kd) != KnockDownInfoSize {
		t.Fatalf("the block is %d bytes, want %d", len(kd), KnockDownInfoSize)
	}
	if got := worldPos(kd); got != mainPos {
		t.Fatalf("the block's startPos is %v, want the main body's own position %v",
			got, mainPos)
	}
	end := worldPos(kd[12:])
	dx := float64(mainPos[0] - atkPos[0])
	dy := float64(mainPos[1] - atkPos[1])
	if got := math.Hypot(float64(end[0]-mainPos[0]), float64(end[1]-mainPos[1])); math.Abs(got-kdFallDistance) > 1.0 {
		t.Fatalf("the block's endPos is %v, %g units from the body - the reference's "+
			"own fall is %g", end, got, kdFallDistance)
	}
	if want := float32(math.Atan2(dy, dx) * 180 / math.Pi); math.Float32frombits(binary.LittleEndian.Uint32(kd[32:])) != want {
		t.Fatalf("the block's hAngle is %g, want the bearing %g",
			math.Float32frombits(binary.LittleEndian.Uint32(kd[32:])), want)
	}
	if got := binary.LittleEndian.Uint16(kd[24:]); got != kdDelayMs {
		t.Fatalf("the block's delay is %d, want %d - read from +0x18 of the block, "+
			"i.e. +%d of the packet", got, kdDelayMs, AttackSplashMainKD+24)
	}
	if got := binary.LittleEndian.Uint16(kd[26:]); got != kdPower {
		t.Fatalf("the block's power is %d, want %d", got, kdPower)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(kd[28:])); got != kdVAngleDeg {
		t.Fatalf("the block's vAngle is %g, want %g", got, kdVAngleDeg)
	}
	if got := kd[36]; got != kdTypeNormal {
		t.Fatalf("the block's KD type is %d, want %d", got, kdTypeNormal)
	}

	// THE RECORDS START AT +112, and the first one is the extra body.
	if got := binary.LittleEndian.Uint32(pkt[AttackSplashKDFirst:]); got != extra {
		t.Fatalf("the first record at +%d names handle %d, want the extra body %d",
			AttackSplashKDFirst, got, extra)
	}
}
