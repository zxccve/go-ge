package zone

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestEnterLegacyObjectsIsTwoBytes pins the packet that ends the loading screen.
// It has NO payload, and that is the whole reason the client leaves the
// "moving to <map>" screen - a length word added here would break it.
func TestEnterLegacyObjectsIsTwoBytes(t *testing.T) {
	got := BuildEnterLegacyObjects()
	if len(got) != 2 {
		t.Fatalf("packet is %d bytes, want 2 (command only)", len(got))
	}
	if cmd := binary.LittleEndian.Uint16(got); cmd != CmdZCEnterLegacyObjs {
		t.Fatalf("command = %d, want %d", cmd, CmdZCEnterLegacyObjs)
	}
}

// TestStartInfoSizeAndFields pins one 485-byte record. Offsets come from the
// Python builder, which measured them against the client; this test exists so a
// later "tidy up" cannot silently move a field.
func TestStartInfoSizeAndFields(t *testing.T) {
	c := Character{
		Handle:   0x11223344,
		Name:     "Arcis",
		Gender:   1,
		Job:      2,
		HeadType: 3,
		Level:    57,
		HP:       1234.5,
		MaxHP:    2000,
		SP:       100,
		MaxSP:    200,
		Exp:      987654321,
		Position: [3]int32{3317, -1515, 250},
	}
	b := BuildStartInfo(c)

	if len(b) != startInfoSize {
		t.Fatalf("record is %d bytes, want %d", len(b), startInfoSize)
	}
	if got := binary.LittleEndian.Uint32(b[siHandle:]); got != c.Handle {
		t.Fatalf("handle = %#x, want %#x", got, c.Handle)
	}
	if got := trimNul(b[siName : siName+nameLen]); got != c.Name {
		t.Fatalf("name = %q, want %q", got, c.Name)
	}
	if got := trimNul(b[siAppearance+apName : siAppearance+apName+nameLen]); got != c.Name {
		t.Fatalf("appearance name = %q, want %q", got, c.Name)
	}
	if b[siAppearance+apGender] != c.Gender {
		t.Fatalf("gender = %d, want %d", b[siAppearance+apGender], c.Gender)
	}
	if got := binary.LittleEndian.Uint32(b[siAppearance+apJob:]); got != c.Job&0xFF {
		t.Fatalf("job = %d, want %d", got, c.Job&0xFF)
	}
	// Job is a DWORD with three zero bytes after it; a 4-byte write of anything
	// larger would land in headType.
	if b[siAppearance+apJob+3] != 0 {
		t.Fatal("the byte after the job id is not zero - the client reads headType there")
	}
	if b[siAppearance+apHeadType] != c.HeadType {
		t.Fatalf("headType = %d, want %d", b[siAppearance+apHeadType], c.HeadType)
	}
	for i, want := range c.Position {
		off := siInitPos + i*4
		if got := int32(binary.LittleEndian.Uint32(b[off:])); got != want {
			t.Fatalf("position[%d] = %d, want %d (int32, and [2] is the navmesh CELL)", i, got, want)
		}
	}
	bs := siBasicStat
	if b[bs+bsLevel] != c.Level {
		t.Fatalf("level = %d, want %d", b[bs+bsLevel], c.Level)
	}
	if got := mathFloat64(b[bs+bsHP:]); got != c.HP {
		t.Fatalf("hp = %v, want %v (float64)", got, c.HP)
	}
	if got := mathFloat64(b[bs+bsMaxHP:]); got != c.MaxHP {
		t.Fatalf("maxhp = %v, want %v", got, c.MaxHP)
	}
	if got := binary.LittleEndian.Uint16(b[bs+bsSP:]); got != c.SP {
		t.Fatalf("sp = %d, want %d", got, c.SP)
	}
	if got := int64(binary.LittleEndian.Uint64(b[bs+bsExp:])); got != c.Exp {
		t.Fatalf("exp = %d, want %d", got, c.Exp)
	}
	// Counts must be zero while the stance/skill tables are not read on this side.
	if got := binary.LittleEndian.Uint16(b[siStanceCount:]); got != 0 {
		t.Fatalf("stance count = %d, want 0 for the first slice", got)
	}
	if got := binary.LittleEndian.Uint16(b[siSkillCount:]); got != 0 {
		t.Fatalf("skill count = %d, want 0 for the first slice", got)
	}
}

// TestStartGameOKHeader pins the wrapper: the length word counts the WHOLE
// packet, and the character records start at byte 36.
func TestStartGameOKHeader(t *testing.T) {
	one := BuildStartInfo(Character{Name: "A"})
	two := BuildStartInfo(Character{Name: "B"})

	for _, tc := range []struct {
		name    string
		records [][]byte
		want    int
		chars   byte
	}{
		{"empty", nil, headerSize, 0},
		{"one", [][]byte{one}, headerSize + startInfoSize, 1},
		{"two", [][]byte{one, two}, headerSize + 2*startInfoSize, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildStartGameOK(tc.records)
			if len(got) != tc.want {
				t.Fatalf("packet is %d bytes, want %d", len(got), tc.want)
			}
			if cmd := binary.LittleEndian.Uint16(got); cmd != CmdZCStartGameOK {
				t.Fatalf("command = %d, want %d", cmd, CmdZCStartGameOK)
			}
			if size := binary.LittleEndian.Uint16(got[2:]); int(size) != len(got) {
				t.Fatalf("length word = %d, want %d (it counts the whole packet)", size, len(got))
			}
			// Count sits at 27: cmd(2) total(2) tick(4) perm(1) connPos(1)
			// party(17) = 27, so the count byte is the next one.
			if n := got[27]; n != tc.chars {
				t.Fatalf("character count = %d, want %d", n, tc.chars)
			}
			// Records must start where the header ends.
			if tc.chars > 0 {
				if name := trimNul(got[headerSize+siName : headerSize+siName+nameLen]); name != "A" {
					t.Fatalf("first record name = %q, want A", name)
				}
			}
		})
	}
}

// TestParseStartGame reads the fields CZ_START_GAME carries after its security
// header. Offset 10 is where session and account begin.
func TestParseStartGame(t *testing.T) {
	pt := make([]byte, 40)
	binary.LittleEndian.PutUint16(pt, CmdCZStartGame)
	binary.LittleEndian.PutUint32(pt[10:], 0xAABBCCDD) // session
	binary.LittleEndian.PutUint32(pt[14:], 4242)       // account
	copy(pt[18:], "archi")

	sessionID, accountID, loginID := ParseStartGame(pt)
	if sessionID != 0xAABBCCDD {
		t.Fatalf("session = %#x, want 0xAABBCCDD", sessionID)
	}
	if accountID != 4242 {
		t.Fatalf("account = %d, want 4242", accountID)
	}
	if loginID != "archi" {
		t.Fatalf("login id = %q, want archi", loginID)
	}

	// A short packet must not panic: it is a malformed client, not a crash.
	if s, a, l := ParseStartGame([]byte{1, 2, 3}); s != 0 || a != 0 || l != "" {
		t.Fatalf("short packet parsed to %d %d %q, want zeros", s, a, l)
	}
}

// mathFloat64 reads a little-endian float64 without importing math in the test.
func mathFloat64(b []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}
