package zone

// THE SKILL TABLE CACHE'S TESTS (`zone\skill_table.go`).
//
// The card's pass condition: a cached answer must be the UNCAHED answer - same
// row count, same values - and every return value must be a COPY, because the
// zone suite mutates `h.Skills.byID` (`zone\skill_area_test.go:930`) and that
// mutation must not reach the next test that builds a zone. The third test pins
// the other half of the promise: the file is parsed ONCE per process.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCachedSkillTableMatchesAnUncachedRead(t *testing.T) {
	dir := stanceDir()
	want, err := LoadSkillsUncached(dir)
	if err != nil {
		t.Skipf("the client's skill table is not readable (%v)", err)
	}
	got, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("LoadSkills(%q): %v", dir, err)
	}
	if got.Len() != want.Len() {
		t.Fatalf("the cached table holds %d rows, an uncached read %d", got.Len(), want.Len())
	}

	// THREE KNOWN SKILLS, with the values the client's own file carries (measured
	// 2026-11-08): a melee hit, an instant melee hit, and the area skill the
	// ground-cast lane builds.
	known := []struct {
		id         uint32
		name, code string
		sklATK     float64
		maxR       float64
		splash     float64
		splLimit   int
		castTime   int
		useType    string
		valueType  string
	}{
		{10561, "Rapid Shot", "RapidShot", 7, 1200, 0, 0, 600, "MELEE", "Attack"},
		{10562, "Implode Shock", "ImplodeShock", 12, 125, 0, 0, 0, "MELEE", "Attack"},
		{20211, "Fireball", "Fireball", 5, 800, 300, 5, 900, "AREA", "Attack"},
	}
	for _, k := range known {
		cached, cok := got.Row(k.id)
		plain, pok := want.Row(k.id)
		if !cok || !pok {
			t.Errorf("%d: cached ok=%v, uncached ok=%v - the cache lost a row", k.id, cok, pok)
			continue
		}
		if cached != plain {
			t.Errorf("%d: the cached row is %+v, the uncached read says %+v - a cache must not change a value", k.id, cached, plain)
		}
		if cached.Name != k.name || cached.Code != k.code || cached.SklATK != k.sklATK ||
			cached.MaxR != k.maxR || cached.SplashRange != k.splash ||
			cached.SplLimit != k.splLimit || cached.CastTime != k.castTime ||
			cached.UseType != k.useType || cached.ValueType != k.valueType {
			t.Errorf("%d (%s): the cached row is %+v, the file says name %q code %q SklATK %g MaxR %g SplashRange %g SplLimit %d CastTime %d UseType %q ValueType %q",
				k.id, k.name, cached, k.name, k.code, k.sklATK, k.maxR, k.splash, k.splLimit, k.castTime, k.useType, k.valueType)
		}
	}

	// AND EVERY OTHER ROW: a three-row sample could pass while the rest of the
	// table was lost or renumbered.
	for id, plain := range want.byID {
		cached, ok := got.byID[id]
		if !ok || cached != plain {
			t.Fatalf("row %d: the cached table has it as %+v (ok=%v), the uncached read says %+v", id, cached, ok, plain)
		}
	}
}

func TestASkillTableCopyCannotLeakIntoTheNextCall(t *testing.T) {
	dir := stanceDir()
	first, err := LoadSkills(dir)
	if err != nil {
		t.Skipf("the client's skill table is not readable (%v)", err)
	}
	original, ok := first.Row(20211)
	if !ok {
		t.Fatalf("20211 (Fireball) is missing from %s", skillTableFile)
	}

	// The mutation `zone\skill_area_test.go:930` makes: replace a row, and drop one.
	first.byID[20211] = SkillRow{ID: 20211, Name: "MUTATED BY A TEST"}
	delete(first.byID, 10561)

	second, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("the second LoadSkills(%q): %v", dir, err)
	}
	if got, ok := second.Row(20211); !ok || got != original {
		t.Errorf("the second call sees 20211 = %+v (ok=%v), want the file's own row %+v: a returned table is not the cache", got, ok, original)
	}
	if _, ok := second.Row(10561); !ok {
		t.Errorf("10561 (Rapid Shot) is gone from the second call: the two tables share one map")
	}
	if first.Len() != second.Len()-1 {
		t.Errorf("the first table holds %d rows after the delete, the second %d: want exactly one fewer", first.Len(), second.Len())
	}
}

func TestTheSkillTableIsParsedOncePerProcess(t *testing.T) {
	real := stanceDir()
	original, err := os.ReadFile(filepath.Join(real, skillTableFile))
	if err != nil {
		t.Skipf("the client's skill table is not readable (%v)", err)
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, skillTableFile)
	if err := os.WriteFile(dst, original, 0o644); err != nil {
		t.Fatalf("writing the copy: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat the copy: %v", err)
	}

	// A DIFFERENT PATH is a different cache entry: this call parses the copy.
	first, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("first load of the copy: %v", err)
	}
	if first.Len() == 0 {
		t.Fatalf("the copy answered 0 rows")
	}

	// BREAK THE FILE WITHOUT MOVING THE KEY: same byte count, same mtime. A call
	// that re-reads finds no XML and fails; a call that takes the cache answers.
	if err := os.WriteFile(dst, bytes.Repeat([]byte{'x'}, len(original)), 0o644); err != nil {
		t.Fatalf("rewriting the copy: %v", err)
	}
	if err := os.Chtimes(dst, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("restoring the copy's mtime: %v", err)
	}
	after, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat the rewritten copy: %v", err)
	}
	if after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime()) {
		t.Fatalf("the rewritten copy's key moved (size %d -> %d, mtime %v -> %v); this test needs the same key",
			fi.Size(), after.Size(), fi.ModTime(), after.ModTime())
	}

	second, err := LoadSkills(dir)
	if err != nil {
		t.Fatalf("the second call RE-READ the file (%v): the cache missed a key it had just stored", err)
	}
	if second.Len() != first.Len() {
		t.Fatalf("the cached answer holds %d rows, the first read %d", second.Len(), first.Len())
	}

	// And the bytes truly are not XML: the uncached read fails, so the answer above
	// cannot have come from the file.
	if _, uerr := LoadSkillsUncached(dir); uerr == nil {
		t.Fatalf("the rewritten copy parsed as XML, so this test proves nothing")
	}
}

func TestAMissingSkillTableIsReportedExactlyAsBefore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-xml-dir")
	_, plainErr := LoadSkillsUncached(dir)
	_, cachedErr := LoadSkills(dir)
	if plainErr == nil || cachedErr == nil {
		t.Fatalf("a missing %s: uncached err %v, cached err %v - want an error from both", skillTableFile, plainErr, cachedErr)
	}
	if cachedErr.Error() != plainErr.Error() {
		t.Errorf("the cached call reports %q, the uncached read %q - a miss must report the reader's own words", cachedErr, plainErr)
	}
}
