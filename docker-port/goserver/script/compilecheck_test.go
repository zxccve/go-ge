package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp puts `body` in a new file called name under a temp dir and returns
// its path. Kept tiny on purpose: these tests are about ONE method.
func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// A file the engine can parse must produce NO error, and a file it cannot parse
// must produce exactly ONE error naming that file.
//
// The bad case is the proof the test can fail: if CompileCheck always returned
// nil, or named the wrong path, this test fails on the second half.
func TestCompileCheckNamesTheFileThatDoesNotParse(t *testing.T) {
	h := New()
	defer h.Close()

	good := writeTemp(t, "good.lua",
		"local x = 1 + 2\nfunction SCR_GOOD() return x end\n")
	bad := writeTemp(t, "bad.lua", "local x = (1 +\n")

	if got := h.CompileCheck([]string{good}); len(got) != 0 {
		t.Fatalf("a valid file reported %d error(s): %v", len(got), got)
	}

	got := h.CompileCheck([]string{bad})
	if len(got) != 1 {
		t.Fatalf("an unparseable file reported %d error(s), want exactly 1: %v", len(got), got)
	}
	if got[0].Path != bad {
		t.Errorf("error names %q, want the file that failed (%q)", got[0].Path, bad)
	}
	if got[0].Err == nil {
		t.Fatal("error is nil; the gopher-lua message must be kept")
	}
	// The engine's own message is the evidence: it must mention the file too, so
	// a caller logging only Err still names it.
	if !strings.Contains(got[0].Err.Error(), bad) {
		t.Errorf("engine message does not name the file: %q", got[0].Err.Error())
	}
}

// CompileCheck must NOT run the file. A chunk that raises at the top level is
// perfectly valid Lua, so it must come back clean - LoadFile would return an
// error here instead. This is the difference between "compiles" and "loads".
func TestCompileCheckDoesNotRunTheFile(t *testing.T) {
	h := New()
	defer h.Close()

	boom := writeTemp(t, "boom.lua", "error('this must not run at compile time')\n")

	if got := h.CompileCheck([]string{boom}); len(got) != 0 {
		t.Fatalf("CompileCheck ran the file: %d error(s): %v", len(got), got)
	}
}
