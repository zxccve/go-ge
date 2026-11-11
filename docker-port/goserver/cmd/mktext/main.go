// mktext - write the SHARED dialog-text image (out/text_image.bin).
//
// The zone memory measurement of 2026-11-09 (history\world-layer.md) showed the three text
// tables are 73 MB of LIVE heap in every zone - the same bytes in every process. As one
// memory-mapped file the operating system keeps ONE physical copy for the whole machine and
// every zone's private memory loses the whole 73 MB.
//
// Run it once after the dictionary / id map change, then start the zones (they map the file
// automatically). The image is versioned by its magic: an old image is refused with a log
// line and the zone falls back to loading the tables, so a stale file can never be misread.
//
//	cd goserver && go run ./cmd/mktext -out ../out/text_image.bin
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"geserver/text"
)

func main() {
	idMap := flag.String("idmap", text.DefaultIDMap, "old-id -> client-id map (src\\make_locale_map.py)")
	dict := flag.String("dict", text.DefaultDict, "the client's dictionary_local.xml")
	pack := flag.String("serverpack", text.DefaultServerPack, "the server pack's dictionary")
	out := flag.String("out", text.DefaultImage, "where to write the image")
	flag.Parse()

	size, err := text.SaveImage(*out, *idMap, *dict, *pack)
	if err != nil {
		log.Fatalf("mktext: %v", err)
	}
	if fi, serr := os.Stat(*out); serr == nil {
		fmt.Printf("mktext: wrote %s - %.1f MB\n", *out, float64(fi.Size())/(1<<20))
	} else {
		fmt.Printf("mktext: wrote %s - %d bytes\n", *out, size)
	}

	// A read-back, so a broken write is caught HERE and not by a zone at startup.
	book, err := text.LoadImage(*out)
	if err != nil {
		log.Fatalf("mktext: the image just written does not load: %v", err)
	}
	defer book.Close()
	fmt.Printf("mktext: dictionary %d entries, server pack %d, id map %d\n",
		book.Dict.Len(), book.ServerPack.Len(), book.IDMap.Len())
}
