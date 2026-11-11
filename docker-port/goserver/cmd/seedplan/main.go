// Command seedplan makes the zone plan cover EVERY map the client knows.
//
// The owner's call (2026-11-09): "maybe we should configure all map? only City should be warm, and
// i will decide which to drop, the rest is on demand and mission is exclusive (since this can be
// destroy/have time limit)."
//
// So: one row per map in `ge/xml/datatable_map.xml`, `warm` only for Type="CITY", everything else
// on demand, and a note that carries the map's TYPE so the operator sees at a glance which rows are
// mission instances (the ones a mission allocates, uses and destroys, rather than places somebody
// lives).
//
// It NEVER touches a row that already exists: the operator's warm flags, ports and notes survive
// every run. Run it again after a client patch and only the new maps are added.
//
//	go run ./cmd/seedplan -maps ../../ge/xml/datatable_map.xml
package main

import (
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"geserver/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

type mapRow struct {
	ID    int
	Class string
	Type  string
}

func main() {
	path := flag.String("maps", "../../ge/xml/datatable_map.xml", "the client's map table")
	dry := flag.Bool("dry", false, "count what would be inserted, write nothing")
	flag.Parse()

	maps, err := readMapTable(*path)
	if err != nil {
		log.Fatalf("seedplan: %v", err)
	}
	log.Printf("seedplan: %d map(s) in %s", len(maps), *path)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, db.DSN())
	if err != nil {
		log.Fatalf("seedplan: %v", err)
	}
	defer pool.Close()

	have := map[int]bool{}
	rows, err := pool.Query(ctx, `SELECT map_id FROM zone_daemon`)
	if err != nil {
		log.Fatalf("seedplan: read zone_daemon: %v", err)
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			log.Fatalf("seedplan: %v", err)
		}
		have[id] = true
	}
	rows.Close()

	// daemon_id continues from the highest one already in the table.
	var nextID int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(daemon_id), 0) FROM zone_daemon`).Scan(&nextID); err != nil {
		log.Fatalf("seedplan: %v", err)
	}

	inserted, warm, skipped := 0, 0, 0
	for _, m := range maps {
		if have[m.ID] {
			skipped++
			continue
		}
		warmNow := m.Type == "CITY"
		note := fmt.Sprintf("%s (every map, 2026-11-09: city warm, the rest on demand)", m.Type)
		if m.Type == "CITY" {
			warm++
		}
		inserted++
		if *dry {
			continue
		}
		nextID++
		if _, err := pool.Exec(ctx, `INSERT INTO zone_daemon
			(daemon_id, map_id, channel, name, port, kind, management_on, warm, note)
			VALUES ($1, $2, 0, $3, 0, 'Z', true, $4, $5)`,
			nextID, m.ID, m.Class, warmNow, note); err != nil {
			log.Fatalf("seedplan: insert map %d (%s): %v", m.ID, m.Class, err)
		}
	}
	log.Printf("seedplan: %d inserted (%d of them CITY, warm), %d already configured, dry=%v",
		inserted, warm, skipped, *dry)

	var total, warmTotal int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE warm) FROM zone_daemon`).
		Scan(&total, &warmTotal); err != nil {
		log.Fatalf("seedplan: %v", err)
	}
	log.Printf("seedplan: zone_daemon now holds %d row(s), %d warm", total, warmTotal)
}

// readMapTable reads id, class name and type for every map.
func readMapTable(path string) ([]mapRow, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	dec := xml.NewDecoder(fh)
	dec.Strict = false // the client's files carry raw '&' (measured in dictionary_local.xml:16666)
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var out []mapRow
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
		var m mapRow
		for _, a := range st.Attr {
			switch a.Name.Local {
			case "ClassID":
				m.ID, _ = strconv.Atoi(a.Value)
			case "ClassName":
				m.Class = a.Value
			case "Type":
				m.Type = a.Value
			}
		}
		if m.ID > 0 && m.Class != "" {
			out = append(out, m)
		}
	}
	return out, nil
}
