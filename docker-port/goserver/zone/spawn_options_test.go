package zone

import (
	"encoding/binary"
	"testing"
)

// THE VARIABLE-LENGTH SPAWN (3520): the fixed 705 bytes we already build, then one
// 12-byte EQUIP_ITEM_OPTION per option on a worn item, and a length word that covers
// the WHOLE packet.
//
// The two option counts below are not invented: they are the LIVE server's own
// numbers from the 2026-10-28 capture. 26 options -> 705 + 26*12 = 1017 bytes, and
// 33 options -> 705 + 33*12 = 1101, which are exactly the two sizes the official
// server put on the wire. The spot layout is the capture's too (1 BODY, 3 NECK,
// 5 RHAND, 8 GLOVE, 9 BOOTS, 12 EAR, 13 BELT), plus the empty slots 4/6/7 in the
// 33-option run.

// spotLayout is one spot and how many options the item in that spot carries, in the
// order the layout is written down here - NOT in spot order on purpose, so the
// builder's own sort is exercised rather than handed a sorted input.
type spotLayout struct {
	spot  uint16
	count int
}

// optionsFrom turns a spot layout into options with a distinct type and value per
// record, so a test can identify every single one of them after decoding.
func optionsFrom(layout []spotLayout) []EquipOption {
	var out []EquipOption
	for _, s := range layout {
		for n := 0; n < s.count; n++ {
			out = append(out, EquipOption{
				Type:  uint32(1000 + s.spot),
				Value: uint32(100 + int(s.spot)*10 + n),
				Spot:  s.spot,
			})
		}
	}
	return out
}

// TestSpawnLengthWordCoversTheWholePacket is the pin the whole change exists for: the
// length word at +2 is the WHOLE packet, option records included, for two characters
// with DIFFERENT option counts.
//
// Two different counts matter. With one count a length word could be a constant by
// accident; with 26 and 33 it has to be computed, and the packet has to be LONGER for
// the second one - which is the property a fixed-size buffer silently loses.
func TestSpawnLengthWordCoversTheWholePacket(t *testing.T) {
	twentySix := []spotLayout{{1, 3}, {3, 3}, {5, 6}, {8, 5}, {9, 3}, {12, 3}, {13, 3}}
	thirtyThree := []spotLayout{{1, 4}, {3, 3}, {4, 3}, {5, 4}, {6, 4}, {7, 4}, {8, 5}, {9, 3}, {13, 3}}

	cases := []struct {
		name   string
		layout []spotLayout
		want   int
	}{
		{"26 options, the live 1017-byte spawn", twentySix, 1017},
		{"33 options, the live 1101-byte spawn", thirtyThree, 1101},
	}
	var sizes []int
	for _, tc := range cases {
		opts := optionsFrom(tc.layout)
		if len(opts) != countOf(tc.layout) {
			t.Fatalf("%s: the layout produced %d option(s), want %d",
				tc.name, len(opts), countOf(tc.layout))
		}
		c := Character{Handle: 0x40000031, Name: "Jin", Job: 110, Level: 9,
			HP: 120, MaxHP: 200, Stance: 30110, EquipOptions: opts}
		p := BuildSpawnStandingPC(c.Handle, c, [3]int32{111, 222, 3}, DefaultPosture)

		if len(p) != tc.want {
			t.Fatalf("%s: packet is %d bytes, want %d (705 + %d*12)",
				tc.name, len(p), tc.want, len(opts))
		}
		lw := int(binary.LittleEndian.Uint16(p[2:]))
		if lw != len(p) {
			t.Fatalf("%s: length word = %d but the packet is %d bytes - they MUST "+
				"agree, or the client reads the rest of the stream as option records",
				tc.name, lw, len(p))
		}
		if got := int(binary.LittleEndian.Uint32(p[spOptions:])); got != len(opts) {
			t.Fatalf("%s: optionCount at +%d = %d, want %d", tc.name, spOptions, got, len(opts))
		}
		sizes = append(sizes, len(p))
	}
	if sizes[1] <= sizes[0] {
		t.Fatalf("the 33-option spawn is %d bytes and the 26-option one %d: the "+
			"packet must GROW with the option count", sizes[1], sizes[0])
	}
}

// countOf is the total number of options a layout asks for.
func countOf(layout []spotLayout) int {
	n := 0
	for _, s := range layout {
		n += s.count
	}
	return n
}

// TestSpawnOptionRecordsArePacked reads the tail back out of the packet byte by byte:
// the count at +701, then 12 bytes per record (+0 type, +4 value, +8 spot, +10 and
// +11 zero), and the packet ends EXACTLY after the last record.
//
// It also hands the builder a layout whose spots are out of order, because the
// official server walks the spots ascending (CEquipSlot::GetItemOptions): the wire
// order must be the server's, not the caller's.
func TestSpawnOptionRecordsArePacked(t *testing.T) {
	// Deliberately NOT sorted: spot 9 before spot 1.
	layout := []spotLayout{{9, 2}, {1, 1}, {13, 2}, {5, 1}}
	want := []EquipOption{
		{Type: 1001, Value: 110, Spot: 1},
		{Type: 1005, Value: 150, Spot: 5},
		{Type: 1009, Value: 190, Spot: 9},
		{Type: 1009, Value: 191, Spot: 9},
		{Type: 1013, Value: 230, Spot: 13},
		{Type: 1013, Value: 231, Spot: 13},
	}
	c := Character{Handle: 0x40000031, Name: "Jin", Job: 110, Level: 9,
		HP: 120, MaxHP: 200, Stance: 30110, EquipOptions: optionsFrom(layout)}
	p := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	if EquipOptionSize != 12 {
		t.Fatalf("EquipOptionSize = %d, want the 12 bytes EQUIP_ITEM_OPTION takes", EquipOptionSize)
	}
	if got, wantLen := len(p), SpawnStandingPCSize+EquipOptionSize*len(want); got != wantLen {
		t.Fatalf("packet is %d bytes, want %d (705 + %d*12)", got, wantLen, len(want))
	}
	n := int(binary.LittleEndian.Uint32(p[spOptions:]))
	if n != len(want) {
		t.Fatalf("optionCount = %d, want %d", n, len(want))
	}
	for i, w := range want {
		off := SpawnStandingPCSize + i*EquipOptionSize
		rec := p[off : off+EquipOptionSize]
		if got := binary.LittleEndian.Uint32(rec[0:]); got != w.Type {
			t.Fatalf("record %d type = %d, want %d", i, got, w.Type)
		}
		if got := binary.LittleEndian.Uint32(rec[4:]); got != w.Value {
			t.Fatalf("record %d value = %d, want %d", i, got, w.Value)
		}
		if got := binary.LittleEndian.Uint16(rec[8:]); got != w.Spot {
			t.Fatalf("record %d spot = %d, want %d - the records go out in ASCENDING "+
				"spot order, the way CEquipSlot::GetItemOptions walks them", i, got, w.Spot)
		}
		if rec[10] != 0 || rec[11] != 0 {
			t.Fatalf("record %d pad bytes are %#x %#x, want 0 0", i, rec[10], rec[11])
		}
	}
	// The packet ENDS after the last record: one byte more or less is a client that
	// reads the next packet's bytes as an option.
	if wantLen := SpawnStandingPCSize + EquipOptionSize*len(want); len(p) != wantLen {
		t.Fatalf("packet is %d bytes, want it to end exactly after record %d at %d",
			len(p), len(want)-1, wantLen)
	}
	if lw := int(binary.LittleEndian.Uint16(p[2:])); lw != len(p) {
		t.Fatalf("length word = %d, packet = %d bytes", lw, len(p))
	}
}

// TestSpawnWithNoOptionsIsStillValid is the PRODUCTION case: no option rows exist in
// the item layer, so the count is 0, the packet is the 705 bytes it always was, and
// its length word says 705 - a zero count promises no records, and the client accepts
// that. A nil slice must not panic.
func TestSpawnWithNoOptionsIsStillValid(t *testing.T) {
	c := Character{Handle: 0x40000031, Name: "Jin", Job: 110, Level: 9,
		HP: 120, MaxHP: 200, Stance: 30110} // EquipOptions nil
	p := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	if len(p) != SpawnStandingPCSize {
		t.Fatalf("packet is %d bytes, want the fixed %d with no options",
			len(p), SpawnStandingPCSize)
	}
	if SpawnStandingPCSize != 705 {
		t.Fatalf("SpawnStandingPCSize = %d, want the fixed part's 705", SpawnStandingPCSize)
	}
	if lw := int(binary.LittleEndian.Uint16(p[2:])); lw != 705 {
		t.Fatalf("length word = %d, want 705", lw)
	}
	if n := binary.LittleEndian.Uint32(p[spOptions:]); n != 0 {
		t.Fatalf("optionCount = %d, want 0", n)
	}
	// An EMPTY, non-nil slice must behave the same: the count comes from what was
	// written, not from whether the caller allocated.
	c.EquipOptions = []EquipOption{}
	if p := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture); len(p) != 705 {
		t.Fatalf("an empty option slice changed the packet to %d bytes, want 705", len(p))
	}
}

// TestSkillDisableIsElevenBytes pins the exact bytes of ZC_SKILL_DISABLE: cmd(2)
// handle(4) type(1) value(4), packed - the value starts at +7, not +8.
//
// The second case is the live capture's own bytes (handle 0x00016f34), because a
// layout mistake that only shows on a non-zero value would still pass on the zero one
// it is easiest to write down.
func TestSkillDisableIsElevenBytes(t *testing.T) {
	if SkillDisableSize != 11 {
		t.Fatalf("SkillDisableSize = %d, want 11", SkillDisableSize)
	}
	cases := []struct {
		handle uint32
		want   []byte
	}{
		{0x40000031, []byte{0xf4, 0x0d, 0x31, 0x00, 0x00, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{0x00016f34, []byte{0xf4, 0x0d, 0x34, 0x6f, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
	}
	for _, tc := range cases {
		got := BuildSkillDisable(tc.handle)
		if len(got) != SkillDisableSize {
			t.Fatalf("handle %#x: %d bytes, want 11", tc.handle, len(got))
		}
		if string(got) != string(tc.want) {
			t.Fatalf("handle %#x: bytes % x, want % x", tc.handle, got, tc.want)
		}
		if cmd := binary.LittleEndian.Uint16(got[0:]); cmd != CmdZCSkillDisable {
			t.Fatalf("cmd = %d, want %d", cmd, CmdZCSkillDisable)
		}
		if h := binary.LittleEndian.Uint32(got[2:]); h != tc.handle {
			t.Fatalf("handle at +2 = %#x, want %#x", h, tc.handle)
		}
		if got[6] != 0 {
			t.Fatalf("type at +6 = %d, want 0", got[6])
		}
		if v := binary.LittleEndian.Uint32(got[7:]); v != 0 {
			t.Fatalf("value at +7 = %d, want 0", v)
		}
	}
}

// TestSkillDisableCarriesTheSpawnsHandle is the ORDER contract in test form: the 3572
// announces state for the ACTOR the 3520 created, so the two must name the same four
// bytes - and the 3572's handle is read out of the spawn, never from a second copy.
//
// It matters more than "ignored": the client's handler dereferences the object it
// looks up by this handle with no null check.
func TestSkillDisableCarriesTheSpawnsHandle(t *testing.T) {
	c := Character{Handle: 0x40000031, Name: "Jin", Job: 110, Stance: 30110,
		EquipOptions: []EquipOption{{Type: 1, Value: 2, Spot: 3}}}
	spawn := BuildSpawnStandingPC(c.Handle, c, [3]int32{1, 2, 3}, DefaultPosture)

	spawnHandle := binary.LittleEndian.Uint32(spawn[4:])
	disable := BuildSkillDisable(spawnHandle)

	if got := binary.LittleEndian.Uint32(disable[2:]); got != spawnHandle {
		t.Fatalf("the 3572 names %#x, the spawn named %#x", got, spawnHandle)
	}
	for i := 0; i < 4; i++ {
		if disable[2+i] != spawn[4+i] {
			t.Fatalf("handle byte %d differs: 3572 %#x vs spawn %#x - the client keys "+
				"the actor by the spawn's bytes, so a second copy of the number is a "+
				"different actor", i, disable[2+i], spawn[4+i])
		}
	}
}
