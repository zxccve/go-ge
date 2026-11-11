package barrack

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestBarrackInfoLayout pins the header and every record offset. A wrong header
// size is the historical crash: the client reads 11 bytes, then 343 per record.
func TestBarrackInfoLayout(t *testing.T) {
	var equip [equipSpotLast]uint32
	equip[0] = 0xAABBCCDD
	var reinforce [equipSpotLast]byte
	reinforce[0] = 5

	chars := []CharInfo{{
		Slot:      3,
		TeamPos:   2,
		Name:      "Arcanine",
		Gender:    true,
		Job:       7,
		Equip:     equip,
		Reinforce: reinforce,
		HeadType:  4,
		Level:     89,
		HP:        1234,
		SP:        567,
		Exp:       100000,
		NextExp:   200000,
		Promotion: 10,
	}}

	b := BuildBarrackInfo(chars)
	want := BarrackInfoHeaderLen + PCInfoSize
	if len(b) != want {
		t.Fatalf("packet = %d bytes, want %d", len(b), want)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != 16 {
		t.Fatalf("command = %d, want 16 (BC_BARRACK_INFO)", cmd)
	}
	if got := binary.LittleEndian.Uint16(b[2:]); int(got) != want {
		t.Fatalf("length word = %d, want %d (the WHOLE packet)", got, want)
	}
	if b[4] != 1 {
		t.Fatalf("characterCount = %d, want 1", b[4])
	}

	rec := b[BarrackInfoHeaderLen:]
	if got := binary.LittleEndian.Uint16(rec); got != 3 {
		t.Fatalf("slot = %d, want 3", got)
	}
	if rec[2] != 2 {
		t.Fatalf("teamPos = %d, want 2", rec[2])
	}
	if got := string(rec[3:3+pcInfoNameLen]); got[:8] != "Arcanine" {
		t.Fatalf("name = %q, want Arcanine", got)
	}
	if rec[pcInfoOffGender] != 1 {
		t.Fatal("gender flag not set")
	}
	if rec[pcInfoOffJob] != 7 {
		t.Fatalf("job = %d, want 7 - the client looks this up in its job table", rec[pcInfoOffJob])
	}
	if got := binary.LittleEndian.Uint32(rec[pcInfoOffEquip:]); got != 0xAABBCCDD {
		t.Fatalf("equip[0] = %#x, want 0xAABBCCDD", got)
	}
	if rec[pcInfoOffReinforce] != 5 {
		t.Fatalf("reinforce[0] = %d, want 5", rec[pcInfoOffReinforce])
	}
	if rec[pcInfoOffHeadType] != 4 || rec[pcInfoOffLv] != 89 {
		t.Fatalf("headType/lv = %d/%d, want 4/89", rec[pcInfoOffHeadType], rec[pcInfoOffLv])
	}
	if got := math.Float64frombits(binary.LittleEndian.Uint64(rec[pcInfoOffHP:])); got != 1234 {
		t.Fatalf("hp = %v, want 1234.0 (a float64 at +300, see history\\barrack.md)", got)
	}
	if got := binary.LittleEndian.Uint32(rec[pcInfoOffSP:]); got != 567 {
		t.Fatalf("sp = %d, want 567 (a u32 at +316)", got)
	}
	if got := int64(binary.LittleEndian.Uint64(rec[pcInfoOffExp:])); got != 100000 {
		t.Fatalf("exp = %d, want 100000", got)
	}
	if got := int64(binary.LittleEndian.Uint64(rec[pcInfoOffNextExp:])); got != 200000 {
		t.Fatalf("nextExp = %d, want 200000 (a cumulative total, not a delta)", got)
	}
	if got := binary.LittleEndian.Uint16(rec[pcInfoOffPromotion:]); got != 10 {
		t.Fatalf("promotion = %d, want 10", got)
	}
}

// TestBarrackInfoCountsRecords proves the packet grows by exactly one record per
// character, which is what the client's loop relies on.
func TestBarrackInfoCountsRecords(t *testing.T) {
	for n := 0; n <= 3; n++ {
		chars := make([]CharInfo, n)
		b := BuildBarrackInfo(chars)
		if len(b) != BarrackInfoHeaderLen+n*PCInfoSize {
			t.Fatalf("%d characters produced %d bytes", n, len(b))
		}
		if int(b[4]) != n {
			t.Fatalf("count byte = %d, want %d", b[4], n)
		}
		if got := int(binary.LittleEndian.Uint16(b[2:])); got != len(b) {
			t.Fatalf("length word = %d, want %d", got, len(b))
		}
	}
}
