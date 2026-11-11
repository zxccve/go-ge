package barrack

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestLockTableMatchesThePythonServer is the reason this packet can be trusted:
// the bytes were produced by the WORKING Python server (which is the server under
// which "Last Point" is clickable), so ours must be identical - including the
// odd trailing space before `/>` and the "\n" before the NUL.
func TestLockTableMatchesThePythonServer(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(replayLiveDir, "..", "lock_table_cmd32.bin"))
	if err != nil {
		t.Skipf("captured cmd 32 not available: %v", err)
	}

	got := LockTable()
	if len(got) != len(want) {
		t.Fatalf("packet is %d bytes, the Python server's is %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("first difference at byte %d: ours 0x%02x (%q), python 0x%02x (%q)",
					i, got[i], snippet(got, i), want[i], snippet(want, i))
			}
		}
	}

	// And the shape must be the documented one.
	if cmd := binary.LittleEndian.Uint16(got); cmd != 32 {
		t.Fatalf("command = %d, want 32", cmd)
	}
	if total := binary.LittleEndian.Uint16(got[2:]); int(total) != len(got) {
		t.Fatalf("length word = %d, want %d (the whole packet)", total, len(got))
	}
	if id := trimNul(string(got[4:updateTableHeader])); id != "Lock" {
		t.Fatalf("id-space = %q, want Lock", id)
	}
	if got[len(got)-1] != 0 {
		t.Fatal("the XML must be NUL terminated")
	}
	t.Logf("%d bytes, %d Lock properties", len(got), len(LockProps()))
}

// snippet shows a few printable bytes around i, to make a diff readable.
func snippet(b []byte, i int) string {
	lo := i - 12
	if lo < 0 {
		lo = 0
	}
	hi := i + 12
	if hi > len(b) {
		hi = len(b)
	}
	out := make([]byte, 0, hi-lo)
	for _, c := range b[lo:hi] {
		if c >= 32 && c < 127 {
			out = append(out, c)
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

// TestLockPropsCarryTheBarrackValues pins the properties the barrack buttons read.
// If one of these ever disappears from the table, the buttons that depend on it go
// dead - which is exactly the bug this packet exists to fix.
func TestLockPropsCarryTheBarrackValues(t *testing.T) {
	want := map[string]bool{
		"Lock_BarrackLv":  true,
		"Lock_MCCLv":      true,
		"Lock_CharSlot":   true,
		"Lock_TeamGroup":  true,
		"Lock_InventoryCount": true,
	}
	got := map[string]string{}
	for _, kv := range LockProps() {
		got[kv[0]] = kv[1]
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("the Lock table is missing %s", name)
		}
	}
	if got["Lock_MCCLv"] != "3" {
		t.Fatalf("Lock_MCCLv = %q, want 3 (three characters may be controlled)", got["Lock_MCCLv"])
	}
}
