package main

// THE WORLD'S IGNITION: one pass over everything that is translated, run ONCE when the manager
// starts, so every zone that follows starts fast.
//
// The owner's shape for it (2026-11-09): "preferably during world/main startup. like a start button
// on a car engine." So this is the key turn, and the zones are the engine:
//
//  1. the SCP corpus is refreshed (prepare.py) - the expensive translation of the game's own
//     scripts, done once instead of once per zone;
//  2. every per-table IMAGE is rebuilt only when its source is newer than the image (the owner's
//     rule: one image per table, so editing items never rebuilds buffs);
//  3. the zones are told the corpus is ready (GE_CORPUS_READY), so they skip their own refresh.
//
// A first start may take a while - that is the accepted price. A second start finds everything
// fresh and costs milliseconds.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"geserver/script"
	"geserver/text"
	"geserver/zone"
)

// preparePaths is everything the ignition needs to know where things live.
type preparePaths struct {
	preparePy  string // ../src/prepare.py
	prepareDir string // ../src (its working directory)
	corpus     string // ../out/lua_corpus
	xmlDir     string // ../../ge/xml
	textImage  string // ../out/text_image.bin
	buffImage  string // ../out/buff_image.bin
	timeout    time.Duration
}

// ignition runs the start button. It never fails hard: a step that cannot run is logged and the
// world starts anyway, exactly like a car that starts with the radio broken.
func ignition(p preparePaths) {
	started := time.Now()

	// 1. THE CORPUS: the game's own SCP, translated once for every zone.
	if err := refreshCorpus(p); err != nil {
		log.Printf("ignition: script corpus NOT refreshed (%v) - zones will check it themselves", err)
	} else {
		log.Printf("ignition: script corpus ready")
	}

	// 2. THE IMAGES, each with its own staleness.
	built, skipped := 0, 0
	if stale, why := text.ImageStale(p.textImage, text.DefaultIDMap, text.DefaultDict, text.DefaultServerPack); stale {
		if n, err := text.SaveImage(p.textImage, text.DefaultIDMap, text.DefaultDict, text.DefaultServerPack); err != nil {
			log.Printf("ignition: the dialog text image was not rebuilt (%v) - zones will parse the XML", err)
		} else {
			log.Printf("ignition: dialog text image rebuilt (%d bytes) - %s", n, why)
			built++
		}
	} else {
		skipped++
	}

	if didBuild, err := buildScriptImage(p); err != nil {
		log.Printf("ignition: the script index image was not rebuilt (%v) - zones will walk the corpus", err)
	} else if didBuild {
		built++
	} else {
		skipped++
	}

	if didBuild, err := buildBuffImage(p); err != nil {
		log.Printf("ignition: the buff image was not rebuilt (%v) - zones will parse the XML", err)
	} else if didBuild {
		built++
	} else {
		skipped++
	}

	log.Printf("ignition: %d image(s) rebuilt, %d already fresh, %.1fs",
		built, skipped, time.Since(started).Seconds())
}

// refreshCorpus runs prepare.py with the same deadline the zones used, so a broken translator
// cannot hold the world hostage.
func refreshCorpus(p preparePaths) error {
	py, err := exec.LookPath("python")
	if err != nil {
		return fmt.Errorf("python not found: %w", err)
	}
	script, dir := p.preparePy, p.prepareDir
	if abs, aerr := filepath.Abs(script); aerr == nil {
		script, dir = abs, filepath.Dir(abs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("prepare.py did not finish in %s", p.timeout)
	}
	if err != nil {
		return fmt.Errorf("prepare.py: %v: %s", err, firstLineOf(string(out)))
	}
	log.Printf("ignition: prepare.py says \"%s\"", firstLineOf(string(out)))
	return nil
}

// scriptImageStale answers whether the index image is missing or older than the corpus.
func scriptImageStale(p preparePaths) bool {
	img, err := os.Stat(script.IndexImagePath(p.corpus))
	if err != nil {
		return true
	}
	newest := time.Time{}
	_ = filepath.Walk(p.corpus, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || filepath.Ext(info.Name()) != ".lua" {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest.After(img.ModTime())
}

// buildScriptImage walks the corpus and writes the index image when it is missing or stale.
// The walk is the expensive half and happens only here, once for the whole world.
func buildScriptImage(p preparePaths) (bool, error) {
	if !scriptImageStale(p) {
		return false, nil
	}
	host := script.New()
	n, err := host.Index(p.corpus)
	if err != nil {
		return false, err
	}
	if err := script.SaveIndexImage(script.IndexImagePath(p.corpus), host.IndexValue()); err != nil {
		return false, err
	}
	log.Printf("ignition: script index image rebuilt - %d names", n)
	return true, nil
}

// buffImageStale compares the buff image against the client's buff XML.
func buffImageStale(p preparePaths) bool {
	return zone.BuffImageStale(p.buffImage, filepath.Join(p.xmlDir, zone.BuffTableFile))
}

// buildBuffImage parses datatable_buff.xml once and writes its image.
func buildBuffImage(p preparePaths) (bool, error) {
	if !buffImageStale(p) {
		return false, nil
	}
	t, err := zone.LoadBuffs(p.xmlDir)
	if err != nil {
		return false, err
	}
	if err := zone.SaveBuffsImage(p.buffImage, t); err != nil {
		return false, err
	}
	log.Printf("ignition: buff image rebuilt - %d rows", t.Len())
	return true, nil
}

// firstLineOf is the one line worth logging from a tool's output.
func firstLineOf(s string) string {
	line := s
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			line = s[:i]
			break
		}
	}
	if len(line) > 200 {
		line = line[:200]
	}
	if line == "" {
		return "(no output)"
	}
	return line
}
