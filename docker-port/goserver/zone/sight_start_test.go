package zone

// THE MAP TABLE IS READ AT THE ZONE'S START, NOT BY THE FIRST BROADCAST (card t_a593b9a2).
//
// WHY THIS FILE EXISTS. `sightRadii` used to read data\map_table.xml the first time any
// caller needed a radius, and that caller is the first body's broadcast. On map 4063 the
// read is a measured 81-87 ms of XML parse, ONE call, and it landed inside the spawn burst:
// `MEMSTAGE spawner` (a delta of 176-185 ms over `MEMSTAGE startup tactics`) carried the
// parse plus the broadcast lines, so two cards read the spawner as the cost. The zone now
// calls Handler.PrimeSight at start-up, in its own `MEMSTAGE sight ring` stage, and
// `main.go` is what wires it.
//
// WHAT A UNIT TEST CAN PIN, and what it cannot: it cannot see main.go's stage line (that is
// the live MEMSTAGE measurement in lanes\build.md). It CAN pin the rule that makes the stage
// worth anything - that the START call, on its own, is what reads the table - because the
// table's own flag says whether the read happened and a radius cannot: 6000 is the answer
// whether the table was read at start or inside the first broadcast.
//
// RED when the lazy path is restored: with `PrimeSight` answering without calling
// sightRadii (i.e. today's first-broadcast load), the first test fails at "the start-up
// call left data\map_table.xml UNLOADED".

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// sightStartFresh clears BOTH caches - the process's map table and this zone's resolved
// radii - for one test, and puts them back afterwards.
//
// BOTH, because PrimeSight is sightRadii: a radius already resolved by an earlier test in
// this package would answer from sightRadiusDone and never reach the loader, so a test that
// only cleared the table would pass without proving anything.
func sightStartFresh(t *testing.T) {
	t.Helper()
	sightTableMu.Lock()
	oldDone, oldData, oldErr := sightTableDone, sightTableData, sightTableErr
	sightTableDone, sightTableData, sightTableErr = false, nil, nil
	sightTableMu.Unlock()

	sightRadiusMu.Lock()
	oldRadiusDone := sightRadiusDone
	oldView, oldMiddle, oldOK := sightRadiusValue, sightRadiusMiddle, sightRadiusOK
	oldLogged := sightRadiusLogged
	sightRadiusDone = false
	sightRadiusValue, sightRadiusMiddle, sightRadiusOK = 0, 0, false
	sightRadiusLogged = map[int]bool{}
	sightRadiusMu.Unlock()

	t.Cleanup(func() {
		sightTableMu.Lock()
		sightTableDone, sightTableData, sightTableErr = oldDone, oldData, oldErr
		sightTableMu.Unlock()
		sightRadiusMu.Lock()
		sightRadiusDone = oldRadiusDone
		sightRadiusValue, sightRadiusMiddle, sightRadiusOK = oldView, oldMiddle, oldOK
		sightRadiusLogged = oldLogged
		sightRadiusMu.Unlock()
	})
}

// THE PIN: the start-up call is what reads the table, and it answers this map's row.
//
// Map 4063 btl_ktv03 is the card's map: the shipped row is ViewRange 6000,
// ViewMiddleRange 4000, PcViewDist 3000, MonViewDist 3200, so the ring is 6000/4000 - the
// numbers the live run logs at start.
func TestTheSightTableIsLoadedAtStartNotByTheFirstBroadcast(t *testing.T) {
	sightStartFresh(t)
	logs := &bytes.Buffer{}
	h := &Handler{MapID: 4063, MapName: "btl_ktv03", Log: log.New(logs, "", 0)}

	if sightTableLoaded() {
		t.Fatal("data\\map_table.xml is already loaded - this test cannot say WHEN it loads")
	}

	// THE ONE CALL main.go makes at start-up, in its own MEMSTAGE stage. No session, no
	// body, no broadcast exists here - exactly the state a zone is in before the spawner.
	view, middle, armed := h.PrimeSight()

	if !sightTableLoaded() {
		t.Fatalf("the start-up call left data\\map_table.xml UNLOADED - the read then happens " +
			"inside the first body's broadcast, which is the 81-87 ms `MEMSTAGE sight ring` " +
			"exists to keep out of `MEMSTAGE spawner`")
	}
	if !armed || view != 6000 || middle != 4000 {
		t.Fatalf("map 4063: leave/update = %d/%d (armed %v), want 6000/4000 from the shipped row",
			view, middle, armed)
	}
	got := logs.String()
	if !strings.Contains(got, "zone: sight: map 4063 btl_ktv03 leave 6000 update 4000") ||
		!strings.Contains(got, "the ring is ON") {
		t.Fatalf("the start-up ring line is missing (pass condition: the line still prints):\n%s", got)
	}

	// AND THE BROADCAST PATH NOW ONLY READS THE ANSWER: the ring's own reader must answer
	// the start-up's numbers and write nothing, or the "moved" cost would just be paid again.
	before := logs.Len()
	v2, m2, armed2 := h.sightRadii()
	if !armed2 || v2 != view || m2 != middle {
		t.Fatalf("after start the ring answers %d/%d (armed %v), want the start-up's %d/%d",
			v2, m2, armed2, view, middle)
	}
	if logs.Len() != before {
		t.Fatalf("reading the radius again logged a second time:\n%s", logs.String()[before:])
	}
}

// -sightoff STILL SKIPS EVERYTHING: no table read, no radius, no line.
//
// This is the control the card's measurement is made with - the same run without the
// flag reads the table at start and the run with it must not read it at all, so a
// `-sightoff` A/B stays an honest A/B of the ring.
func TestASightOffStartReadsNoTableAndLogsNothing(t *testing.T) {
	sightStartFresh(t)
	logs := &bytes.Buffer{}
	h := &Handler{MapID: 4063, MapName: "btl_ktv03", SightOff: true, Log: log.New(logs, "", 0)}

	if view, middle, armed := h.PrimeSight(); armed || view != 0 || middle != 0 {
		t.Fatalf("-sightoff answered leave/update %d/%d (armed %v), want 0/0 and not armed",
			view, middle, armed)
	}
	if sightTableLoaded() {
		t.Fatal("a -sightoff zone READ data\\map_table.xml - the flag skips the whole ring, its table included")
	}
	if logs.Len() != 0 {
		t.Fatalf("-sightoff wrote %d log byte(s), want none:\n%s", logs.Len(), logs.String())
	}
}

// A MAP WITH NO ROW IS STILL ANSWERED AT START, ONCE - the unarmed fallback is a fact the
// start stage reports, not one the first broadcast discovers.
func TestAStartOnAMapWithNoRowSaysSoOnceAndStaysUnarmed(t *testing.T) {
	sightStartFresh(t)
	logs := &bytes.Buffer{}
	h := &Handler{MapID: 999999, MapName: "no_such_map", Log: log.New(logs, "", 0)}

	if _, _, armed := h.PrimeSight(); armed {
		t.Fatal("a map with no map_table row armed the ring - it must send to everyone")
	}
	if !sightTableLoaded() {
		t.Fatal("the table was not read at start for a map with no row - the load must happen at start whatever the row answers")
	}
	const want = "zone: sight: no map_table row for map 999999"
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("%d line(s) naming the missing row, want exactly 1:\n%s", n, logs.String())
	}
	h.PrimeSight() // the second start call (and every broadcast) is silent on the same map
	if n := strings.Count(logs.String(), want); n != 1 {
		t.Fatalf("%d line(s) after a second call, want the first one only - the report is once per map id",
			n)
	}
}
