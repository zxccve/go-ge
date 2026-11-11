package main

import (
	"encoding/xml"
	"io"
	"os"
	"strconv"
	"strings"
)

// THE PRELOAD GRAPH: which maps a map's own DOORS lead to.
//
// The client's map table gives every map a `MovableZone` attribute - the comma-separated CLASS
// names its doors and gates open onto (`reb_basic` -> office_reb, btl_reb01, fld_reb, party_hq,
// btl_ktv03, gig_basic, ...). That is exactly the set a player standing on the map might walk into
// next, so it is the set worth loading BEFORE they click the gate.
//
// It is loaded from the same table the console reads, so a client patch updates both.
type zoneGraph struct {
	byClass map[string]int // class name -> map id
	doors   map[int][]int  // map id -> the map ids its doors lead to
}

// loadZoneGraph reads `ge/xml/datatable_map.xml` and builds the door graph.
func loadZoneGraph(path string) (*zoneGraph, error) {
	g := &zoneGraph{byClass: map[string]int{}, doors: map[int][]int{}}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	type row struct {
		id    int
		class string
		move  string
	}
	var rows []row

	dec := xml.NewDecoder(fh)
	dec.Strict = false // the client's files carry raw '&' (dictionary_local.xml:16666)
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		st, ok := tok.(xml.StartElement)
		if !ok || st.Name.Local != "Class" {
			continue
		}
		var r row
		for _, a := range st.Attr {
			switch a.Name.Local {
			case "ClassID":
				r.id, _ = strconv.Atoi(a.Value)
			case "ClassName":
				r.class = a.Value
			case "MovableZone":
				r.move = a.Value
			}
		}
		if r.id > 0 && r.class != "" {
			rows = append(rows, r)
		}
	}

	for _, r := range rows {
		g.byClass[r.class] = r.id
	}
	for _, r := range rows {
		seen := map[int]bool{}
		for _, name := range strings.Split(r.move, ",") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			if id, ok := g.byClass[name]; ok && id != r.id && !seen[id] {
				seen[id] = true
				g.doors[r.id] = append(g.doors[r.id], id)
			}
		}
	}
	return g, nil
}

// neighbours returns the maps this one's doors lead to, or nil.
func (g *zoneGraph) neighbours(mapID int) []int {
	if g == nil {
		return nil
	}
	return g.doors[mapID]
}
