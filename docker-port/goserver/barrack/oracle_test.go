package barrack

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"geserver/db"
)

// The Python server's own bytes for commander 24, written by
// src\emit_barrack_oracle.py. The PostgreSQL rows were copied from those MSSQL
// rows, so a correct Go builder must produce the SAME bytes.
const (
	oracleCommander = 24
	oracle10        = "testdata/oracle_10.bin"
	oracle16        = "testdata/oracle_16.bin"
)

// TestBuiltPacketsMatchTheOracle is the test that ends the guessing: it compares
// the Go builder, fed from PostgreSQL, against what the WORKING Python server
// sends for the same commander.
//
// A difference names the field; identical bytes mean the builder is right and any
// remaining problem is elsewhere (a packet we drop rather than build).
func TestBuiltPacketsMatchTheOracle(t *testing.T) {
	want10, err := os.ReadFile(oracle10)
	if err != nil {
		t.Skipf("no oracle: run python emit_barrack_oracle.py (%v)", err)
	}
	want16, err := os.ReadFile(oracle16)
	if err != nil {
		t.Skipf("no oracle for cmd 16: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, db.DSN())
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	defer pool.Close()
	store := NewPGBarrackStore(pool)

	cmdr, ok, err := store.Commander(ctx, oracleCommander)
	if err != nil || !ok {
		t.Fatalf("commander %d: ok=%v err=%v", oracleCommander, ok, err)
	}
	chars, err := store.Characters(ctx, oracleCommander)
	if err != nil {
		t.Fatalf("characters: %v", err)
	}

	// THE RANK IN THIS FIXTURE IS NOT A REFERENCE VALUE. oracle_10.bin was emitted
	// by the PYTHON barrack, which filled rank from the commander's Lock_TeamGroup
	// (src\ge_barrack_server.py:499) - the same substitution that kept the Extend
	// button shut. The builder writes whatever Rank it is handed, so this test keeps
	// comparing the LAYOUT; the VALUE a login sends (the cap, GET_TEAMGROUP_MAXSLOT)
	// is pinned by extend_cap_test.go. Do not use this fixture to argue the rank
	// value back to the count.
	got10 := BuildCommanderInfo(CommanderInfo{
		Name: cmdr.Name, Rank: cmdr.TeamMax, CharSlot: cmdr.CharSlot,
		// Commander 24's `point` is 3090, which no map's restart row carries, so there is
		// no saved start point to name and BOTH maps fall back to the last point's map -
		// the single-map barrack. That is what the fixture holds too (two equal words).
		SaveMapID: cmdr.ZoneID, LastMapID: cmdr.ZoneID,
		BarrackNames: cmdr.BarrackNames,
	})

	// THE TWO MAP WORDS ARE DATA, NOT LAYOUT - same rule as teamPos and the equip class
	// ids below, and checked FIRST for the same reason: masking hides a word, so the
	// value must be proven before it is masked.
	//
	// The fixture was emitted from the LEGACY database, where commander 24 sat in map 10
	// (Cite de Reboldoeux) and both map words are 10. The PostgreSQL row now says
	// zone_id 15 (Coimbra): the row has moved since the export, while the builder is
	// unchanged. So the words differ on purpose. What must NOT move is their meaning -
	// newMapID = the save point, contMapID = the last point, and with no save point both
	// are the last point - which is asserted here, before the mask.
	mapAt := 2 + commanderNameLen + partyNameLen + profileLen + partyTitleLen + 2 + 2 + 4 + 2
	if got := binary.LittleEndian.Uint16(got10[mapAt:]); got != cmdr.ZoneID {
		t.Fatalf("cmd 10 newMapID = %d, want the row's zone_id %d (that is the fallback "+
			"when no saved start point resolves)", got, cmdr.ZoneID)
	}
	if got := binary.LittleEndian.Uint16(got10[mapAt+2:]); got != cmdr.ZoneID {
		t.Fatalf("cmd 10 contMapID = %d, want the row's zone_id %d", got, cmdr.ZoneID)
	}
	got10 = withoutMapIDs(got10)
	want10 = withoutMapIDs(want10)
	got16 := BuildBarrackInfo(chars)

	// `teamPos` is DATA, not layout. The oracle fixture was produced from the
	// LEGACY database, which still holds the old team (nobody in it); our builder
	// now reads PostgreSQL, where SaveTeamPositions records the team the player
	// actually picked. That byte therefore differs on purpose - every OTHER byte
	// must still match, which is what this test is for. Blank it in both copies
	// instead of loosening the comparison.
	got16 = withoutTeamPos(got16)
	want16 = withoutTeamPos(want16)

	// THE ALLOWANCE BELOW MASKS THE CLASS IDS, SO IT IS CHECKED FIRST. Masking also
	// hides WHERE a word sat, and a shifted equip block would then look identical. So
	// the drift is named and bounded BEFORE the mask: the two copies may differ only
	// where the legacy copy holds the spot's "nothing equipped" id and ours holds a real
	// item. A word that moved is neither, and fails here, named.
	checkEquipDrift(t, got16, want16)

	// The EQUIP CLASS IDS are DATA too, by the rule just above and for the same reason:
	// gear is live player state, and the fixture is the legacy import. Commander 24's
	// slot 2 character is id 40 "Fighter", whose equip spot 9 (boots) holds class 19533
	// in PostgreSQL - a pair of boots the player actually wears - while the fixture
	// carries 19500, the client's own "nothing equipped" id for that spot
	// (equipNone[9] in barrack_info.go). The legacy import never carried that boot, so
	// the two numbers disagree by design. Blank the class ids in both copies rather
	// than loosening the comparison.
	got16 = withoutEquipClassIDs(got16)
	want16 = withoutEquipClassIDs(want16)

	// THE +300 TAIL IS ALSO DATA-NOT-LAYOUT, and this one was OURS being wrong: the
	// fixture comes from the legacy PYTHON server, which encoded HP as a uint16 at
	// +300 with MaxHP/SP/MaxSP packed after it. The live reference server's own
	// BC_BARRACK_INFO (captures\barrack201-20261027, decoded in history\barrack.md
	// "PC_INFO tail") carries HP as a FLOAT64 at +300, zeros at +308..+315, SP u32
	// at +316, EXP u64 at +320 and the next level's cumulative total at +328. The Go
	// builder now writes that measured map, so the legacy fixture cannot match it.
	// Mask exactly +300..+335 and keep comparing promotion at +336 and every byte
	// before +300; TestBarrackInfoLayout pins the new offsets.
	got16 = withoutPCTail(got16)
	want16 = withoutPCTail(want16)

	// THE RANK WORD IS THE SAME CLASS AS THE PC TAIL: the fixture is the legacy PYTHON
	// server's own bytes, so its rank can only hold the commander's Lock_TeamGroup at
	// export time (src\ge_barrack_server.py:499), never the cap this build sends. The
	// builder writes whatever Rank it is handed, so this test keeps comparing the
	// LAYOUT; the VALUE a login sends is pinned by extend_cap_test.go
	// (GET_TEAMGROUP_MAXSLOT() = 20 on both builders), which must keep passing untouched.
	//
	// PROVE BEFORE MASK, the rule the map words follow above: the word at rankAt must be
	// the Rank handed in at the build site, so the mask cannot be hiding a MOVED field.
	if got := binary.LittleEndian.Uint16(got10[rankAt:]); got != cmdr.TeamMax {
		t.Fatalf("cmd 10 rank at +%d = %d, want the Rank this test handed the builder (%d) - "+
			"a word here that is not the value passed in means the field has moved",
			rankAt, got, cmdr.TeamMax)
	}
	got10 = withoutRank(got10)
	want10 = withoutRank(want10)

	compare(t, "cmd 10 (commander)", got10, want10)
	compare(t, "cmd 16 (barrack)", got16, want16)
}

// maxPlausibleClassID bounds what a class id can be, so a MOVED word cannot pass itself
// off as gear. Measured from this build's own item table: the largest class id is
// 930,239 and the smallest is 7,201, while a word shifted by one byte multiplies an id
// by 256 (at least 1,843,456). This bound sits between the two. Widen it with a dated
// line if the game ever ships an item above it.
const maxPlausibleClassID = 1 << 20

// plausibleClassID says a word could be an item class id from this game at all.
func plausibleClassID(id uint32) bool { return id > 0 && id <= maxPlausibleClassID }

// checkEquipDrift reports every equip class id that differs between our packet and the
// oracle fixture, and fails on any difference that is NOT the known, allowed kind.
//
// THE ALLOWED KIND, exactly: the legacy fixture holds the client's own "nothing
// equipped" id for that spot (equipNone[spot] in barrack_info.go) and our packet holds a
// real item class. That is gear the player has since picked up - data, not layout.
//
// ANYTHING ELSE FAILS, and that is the point: a builder that shifted the equip block by
// a byte, or wrote a wrong stride, produces words that are neither equal nor the spot's
// sentinel, so it is caught HERE even though the masking above would have hidden it.
func checkEquipDrift(t *testing.T, got, want []byte) {
	t.Helper()
	if len(got) != len(want) || len(got) <= BarrackInfoHeaderLen {
		return // compare() reports a length mismatch; nothing to walk
	}
	count := int(want[4])
	if got[4] != want[4] {
		return // different character counts: compare() says so
	}
	drifted := 0
	for i := 0; i < count; i++ {
		rec := BarrackInfoHeaderLen + i*PCInfoSize
		if rec+pcInfoOffReinforce > len(want) {
			return // short packet: compare() reports it
		}
		for spot := 0; spot < equipSpotLast; spot++ {
			at := rec + pcInfoOffEquip + spot*equipEntryStride
			g := binary.LittleEndian.Uint32(got[at:])
			w := binary.LittleEndian.Uint32(want[at:])
			if g == w {
				continue
			}
			// One side is this spot's "nothing equipped" id and the other is a class id
			// this game could hold. Any other pair means the word moved.
			ok := (w == equipNone[spot] && plausibleClassID(g)) ||
				(g == equipNone[spot] && plausibleClassID(w))
			if !ok {
				t.Fatalf("record %d spot %d: %d worn here, %d in the oracle - neither "+
					"pair is \"one side is this spot's nothing-equipped id %d, the "+
					"other a real class id\", so this is not gear drift: something "+
					"moved the equip block", i, spot, g, w, equipNone[spot])
			}
			drifted++
			t.Logf("record %d spot %d: DATA, not layout - %d worn now, %d "+
				"(nothing equipped) in the legacy fixture", i, spot, g, w)
		}
	}
	t.Logf("equip class ids: %d spot(s) differ on purpose, %d compared",
		drifted, count*equipSpotLast)
}

// withoutEquipClassIDs copies a cmd 16 packet with every equip CLASS ID zeroed, and
// nothing else.
//
// WHY: the class id at a spot is player state (which item the character wears), and the
// oracle fixture was emitted from the LEGACY database, so live gear drift shows up here
// - see the "EQUIP CLASS IDS are DATA" note in TestBuiltPacketsMatchTheOracle. The
// layout around those ids is still compared, which is the point of masking so narrowly:
//
//   - ONLY the 4 class-id bytes at `pcInfoOffEquip + spot*equipEntryStride` are zeroed.
//     The eight pad bytes inside each 12-byte entry, the whole reinforce block
//     (pcInfoOffReinforce), and every other field stay byte-for-byte compared.
//   - A record or an equip block shifted by ONE byte moves those ids off these offsets,
//     so the class ids land on bytes this mask does not touch and the comparison fails at
//     the first moved byte. Masking therefore cannot hide a shifted record.
//   - The spot ORDER and the 12-byte stride are pinned independently by
//     TestEmptyEquipmentUsesClientSentinelIds (barrack_info_equip_test.go), which reads
//     each spot back at this same offset and requires its own "None" id.
//
// Record layout is [slot(2)][teamPos(1)][name(17)][gender][job][equip 21*12][reinforce...]
// after the 11-byte header whose byte 4 is the character count.
func withoutEquipClassIDs(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) <= BarrackInfoHeaderLen {
		return out
	}
	count := int(out[4])
	for i := 0; i < count; i++ {
		rec := BarrackInfoHeaderLen + i*PCInfoSize
		for spot := 0; spot < equipSpotLast; spot++ {
			off := rec + pcInfoOffEquip + spot*equipEntryStride
			if off+4 <= len(out) {
				out[off], out[off+1], out[off+2], out[off+3] = 0, 0, 0, 0
			}
		}
	}
	return out
}

// withoutPCTail copies a cmd 16 packet with each record's +300..+335 window zeroed -
// the range whose meaning changed when the builder moved from the legacy Python
// encoding (HP uint16 at +300) to the live server's measured map (HP float64 at +300).
// Everything else - promotion at +336 and every byte before +300 - stays compared.
func withoutPCTail(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) <= BarrackInfoHeaderLen {
		return out
	}
	count := int(out[4])
	for i := 0; i < count; i++ {
		rec := BarrackInfoHeaderLen + i*PCInfoSize
		for off := rec + pcInfoOffHP; off < rec+pcInfoOffPromotion; off++ {
			if off < len(out) {
				out[off] = 0
			}
		}
	}
	return out
}

// TestEquipClassIDMaskStaysNarrow guards the allowance above. A mask that zeroed whole
// 12-byte entries, or a stretch of the record, would let a SHIFTED equip block pass the
// oracle comparison - the opposite of what the allowance is for. So assert the mask's
// own shape: exactly equipSpotLast*4 bytes per record, at the builder's offsets, and no
// byte of the reinforce block.
func TestEquipClassIDMaskStaysNarrow(t *testing.T) {
	// One record of 0x01 bytes, so a zeroed byte is unmistakable, behind a header whose
	// byte 4 is the character count.
	packet := bytes.Repeat([]byte{1}, BarrackInfoHeaderLen+PCInfoSize)
	packet[4] = 1

	got := withoutEquipClassIDs(packet)[BarrackInfoHeaderLen:]
	zeroed := 0
	for i, v := range got {
		if v != 0 {
			continue
		}
		zeroed++
		// Every zeroed byte must be a class-id byte of some spot, and the reinforce
		// block (which starts at pcInfoOffReinforce) must be untouched.
		isClass := false
		for spot := 0; spot < equipSpotLast && !isClass; spot++ {
			at := pcInfoOffEquip + spot*equipEntryStride
			isClass = i >= at && i < at+4
		}
		if !isClass {
			t.Fatalf("byte %d of the record was masked, and it is not a class id - "+
				"the allowance must touch the class ids only", i)
		}
	}
	if want := equipSpotLast * 4; zeroed != want {
		t.Fatalf("masked %d bytes per record, want %d (21 spots x 4 bytes)", zeroed, want)
	}
}

// withoutMapIDs copies a cmd 10 packet with the two map words zeroed, and nothing else:
// newMapID at offset 306 and contMapID right after it.
//
// WHY: which map a commander is in is player state, and the oracle fixture came from the
// LEGACY database, so a commander who has since moved shows different map ids here - the
// same allowance as teamPos and the equip class ids. The layout AROUND the two words is
// still compared byte for byte (rank, charSlot, commanderPoint, ladderPoint before them;
// continueMissionMapID and every field after them), and the values themselves are
// asserted against the row BEFORE the mask - see TestBuiltPacketsMatchTheOracle.
//
// A builder that moved the map words by a byte lands them off these offsets, so the
// comparison fails at the first moved byte: masking cannot hide a shifted field.
func withoutMapIDs(b []byte) []byte {
	out := append([]byte(nil), b...)
	const at = 2 + commanderNameLen + partyNameLen + profileLen + partyTitleLen + 2 + 2 + 4 + 2
	if len(out) >= at+4 {
		out[at], out[at+1], out[at+2], out[at+3] = 0, 0, 0, 0
	}
	return out
}

// TestMapIDMaskStaysNarrow guards the allowance above: exactly four bytes, at the two
// map words, and nothing else in the packet.
func TestMapIDMaskStaysNarrow(t *testing.T) {
	packet := bytes.Repeat([]byte{1}, CommanderInfoSize)
	got := withoutMapIDs(packet)
	const at = 2 + commanderNameLen + partyNameLen + profileLen + partyTitleLen + 2 + 2 + 4 + 2
	for i, v := range got {
		wantZero := i >= at && i < at+4
		if wantZero && v != 0 {
			t.Fatalf("byte %d is the map words' and was not masked", i)
		}
		if !wantZero && v != 1 {
			t.Fatalf("byte %d was masked, and it is not one of the two map words", i)
		}
	}
}

// withoutRank copies a cmd 10 packet with the rank word (+296) zeroed.
//
// The same class of allowance as withoutPCTail: the oracle fixture is the legacy
// PYTHON server's bytes, and that server filled rank from the commander's
// Lock_TeamGroup, so the fixture can never carry the cap this build sends. Two bytes
// only - charSlot, the point and every other field stay compared, and the VALUE the
// wire carries is pinned by extend_cap_test.go.
func withoutRank(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) >= rankAt+2 {
		out[rankAt], out[rankAt+1] = 0, 0
	}
	return out
}

// TestRankMaskStaysNarrow guards the allowance above: exactly the two bytes of the
// rank word, and nothing else in the packet. A wider mask would hide charSlot (the
// very next field) and let a shifted cmd 10 pass.
func TestRankMaskStaysNarrow(t *testing.T) {
	got := withoutRank(bytes.Repeat([]byte{1}, CommanderInfoSize))
	for i, v := range got {
		wantZero := i >= rankAt && i < rankAt+2
		if wantZero && v != 0 {
			t.Fatalf("byte %d is the rank word and was not masked", i)
		}
		if !wantZero && v != 1 {
			t.Fatalf("byte %d was masked, and it is not the rank word (+%d)", i, rankAt)
		}
	}
}

// withoutTeamPos copies a cmd 16 packet with every record's teamPos byte zeroed.
//
// Record layout is [slot][teamPos][name...] (`b[2] = c.TeamPos` in the builder),
// after the 11-byte header whose byte 4 is the character count.
func withoutTeamPos(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) <= BarrackInfoHeaderLen {
		return out
	}
	count := int(out[4])
	for i := 0; i < count; i++ {
		off := BarrackInfoHeaderLen + i*PCInfoSize + 2
		if off < len(out) {
			out[off] = 0
		}
	}
	return out
}

// compare reports the FIRST difference with the field it falls in, which is what
// turns a mismatch into a fix.
func compare(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d bytes, oracle has %d", what, len(got), len(want))
		return
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < len(got) && got[i] == want[i] {
			i++
		}
		lo := i - 8
		if lo < 0 {
			lo = 0
		}
		hi := i + 16
		if hi > len(got) {
			hi = len(got)
		}
		t.Errorf("%s: first difference at byte %d (%s)\n got % x\nwant % x",
			what, i, fieldAt(what, i), got[lo:hi], want[lo:hi])
	}
	t.Logf("%s: identical to the oracle (%d bytes)", what, len(got))
}

// fieldAt names the field a cmd 10 offset belongs to, so the failure message is
// readable instead of "byte 299 differs".
func fieldAt(what string, off int) string {
	if what == "cmd 10 (commander)" {
		switch {
		case off < 2:
			return "command"
		case off < 19:
			return "name"
		case off < 36:
			return "party name"
		case off < 287:
			return "profile"
		case off < 296:
			return "party title"
		case off < 298:
			return "rank (max team groups)"
		case off < 300:
			return "charSlot (Lock_CharSlot)"
		case off < 306:
			return "commander/ladder point"
		case off < 310:
			return "newMapID/contMapID"
		case off < 319:
			return "commLv block"
		case off < 327:
			return "mcc times"
		case off < 735:
			return "barrack names"
		}
		return "tail"
	}
	// cmd 16: header, then 343-byte records.
	if off < BarrackInfoHeaderLen {
		return "header"
	}
	rec := (off - BarrackInfoHeaderLen) / PCInfoSize
	in := (off - BarrackInfoHeaderLen) % PCInfoSize
	switch {
	case in < 2:
		return "record " + itoa(rec) + " slot"
	case in < 3:
		return "record " + itoa(rec) + " teamPos"
	case in < 20:
		return "record " + itoa(rec) + " name"
	case in == 20:
		return "record " + itoa(rec) + " gender"
	case in == 21:
		return "record " + itoa(rec) + " job"
	case in < 277:
		return "record " + itoa(rec) + " equip"
	case in < 298:
		return "record " + itoa(rec) + " reinforce"
	case in < 300:
		return "record " + itoa(rec) + " headType/level"
	case in < 308:
		return "record " + itoa(rec) + " hp/sp"
	case in < 316:
		return "record " + itoa(rec) + " exp"
	case in < 336:
		return "record " + itoa(rec) + " nextExp"
	}
	return "record " + itoa(rec) + " promotion"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
