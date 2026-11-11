package main

import "testing"

// THE DOOR GRAPH COMES FROM THE CLIENT'S OWN TABLE. Reboldoeux opens onto the queen's gate, the
// port of Coimbra and the Armonia dungeons; a preload is only worth anything if those ids are the
// real ones.
func TestTheDoorGraphNamesTheMapsTheDoorsOpenOnto(t *testing.T) {
	g, err := loadZoneGraph("../../../../ge/xml/datatable_map.xml")
	if err != nil {
		t.Skipf("no map table: %v", err)
	}
	if len(g.doors) < 100 {
		t.Fatalf("door graph has only %d map(s) - the table did not load", len(g.doors))
	}
	reb, ok := g.byClass["reb_basic"]
	if !ok || reb != 10 {
		t.Fatalf("reb_basic resolved to %d (found=%v), want map 10", reb, ok)
	}
	// The table's own MovableZone for reb_basic: coim_basic, fld_reb, auch_basic, fld_ptv,
	// fld_ttr, fld_alq, office_reb, party_hq and the battlegrounds.
	want := map[int]bool{
		15:   true, // coim_basic, the Port of Coimbra
		200:  true, // fld_reb, Reboldoeux Queen's Gate
		1301: true, // auch_basic, City of Auch
		204:  true, // fld_ptv, Porto Bello
	}
	got := map[int]bool{}
	for _, id := range g.neighbours(10) {
		got[id] = true
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("map 10's doors do not lead to map %d; got %v", id, g.neighbours(10))
		}
	}
}
