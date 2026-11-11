// Command scriptcensus measures how much of the game's own script corpus the Go
// script host can actually answer.
//
// It is the Go-side answer to offlinespada's `ge script census`: for every
// `SCR_CLICK_*` handler the corpus defines, run it against the host with no
// world wired, and count what happened. Three outcomes, and they mean different
// things:
//
//   - clean   the handler ran without ever reaching a name the host does not
//     bind. Its work is answered by real code.
//   - stubbed the handler ran, but called at least one name that lands in
//     `onMissing` - that call returned 0. The handler did not crash; it took a
//     branch on a lie. This is the number that matters: every one of these is a
//     feature that silently does nothing today.
//   - error   the handler raised. Named by the error text.
//
// The engine-call census (which NAMES the corpus calls, and how often) already
// exists and is a different measurement: `src\count_script_builtins.py` over the
// .scp source. This tool answers "how many handlers does the RUNNING host
// serve", not "how many call sites name some name".
//
// Usage, from docker-port\goserver:
//
//	go run ./cmd/scriptcensus -corpus ../out/lua_corpus
//	go run ./cmd/scriptcensus -corpus ../out/lua_corpus -limit 50 -v
//	go run ./cmd/scriptcensus -corpus ../out/lua_corpus -json ../out/script_census.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"geserver/script"
)

var clickRe = regexp.MustCompile(`\bfunction\s+(SCR_CLICK_[A-Za-z0-9_]+)`)

type handlerResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"` // clean | stubbed | error
	Missing []string `json:"missing,omitempty"`
	Error   string `json:"error,omitempty"`
}

type report struct {
	Corpus        string         `json:"corpus"`
	FilesIndexed  int            `json:"files_indexed"`
	FilesLoaded   int            `json:"files_loaded"`
	LoadErrors    []string       `json:"load_errors,omitempty"`
	Handlers      int            `json:"handlers"`
	Clean         int            `json:"clean"`
	Stubbed       int            `json:"stubbed"`
	Errored       int            `json:"errored"`
	Missing       []script.Missing `json:"missing"`
	MissingTotal  int            `json:"missing_calls_total"`
	TopErrors     map[string]int `json:"top_errors"`
	Results       []handlerResult `json:"results,omitempty"`
}

func main() {
	corpus := flag.String("corpus", "../out/lua_corpus", "directory of translated .lua scripts")
	limit := flag.Int("limit", 0, "run only the first N handlers (0 = all)")
	timeout := flag.Duration("timeout", 100*time.Millisecond, "per-handler timeout")
	jsonPath := flag.String("json", "", "write the full report as JSON here")
	verbose := flag.Bool("v", false, "print one line per handler")
	flag.Parse()

	h := script.New()
	defer h.Close()

	n, err := h.Index(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index %s: %v\n", *corpus, err)
		os.Exit(1)
	}

	// Walk the same tree and (a) collect the click handlers it defines, (b) load
	// every file, because a click reaches the tree's OWN functions across files -
	// a census that loaded one file at a time would report those as missing.
	var files []string
	names := map[string]struct{}{}
	walkErr := filepath.Walk(*corpus, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(p, ".lua") {
			return nil
		}
		files = append(files, p)
		blob, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		for _, m := range clickRe.FindAllSubmatch(blob, -1) {
			names[string(m[1])] = struct{}{}
		}
		return nil
	})
	if walkErr != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", *corpus, walkErr)
		os.Exit(1)
	}
	sort.Strings(files)

	rep := report{Corpus: *corpus, FilesIndexed: n, TopErrors: map[string]int{}}
	// NOTE: Index returns the number of SCR_/SCP_ NAMES it recorded, not files.
	// The field keeps its name for the JSON, the report prints it as names.
	loaded := 0
	for _, p := range files {
		if err := h.LoadFile(p); err != nil {
			rep.LoadErrors = append(rep.LoadErrors, err.Error())
			continue
		}
		loaded++
	}
	rep.FilesLoaded = loaded

	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	sort.Strings(list)
	if *limit > 0 && len(list) > *limit {
		list = list[:*limit]
	}
	rep.Handlers = len(list)

	// snapshot of the host's missing map, so each handler's stubs can be
	// attributed to it rather than to the run as a whole
	prev := map[string]int{}
	for _, m := range h.Missing() {
		prev[m.Name] = m.Calls
	}

	self := script.Obj{Handle: 1, Name: "CensusNPC", Script: "CENSUS"}
	pc := script.Obj{Handle: 2, Name: "CensusPC", Commander: 1}

	for _, name := range list {
		out, err := h.StartClick(name, self, pc, *timeout)
		_ = out
		res := handlerResult{Name: name}
		switch {
		case err != nil:
			res.Outcome = "error"
			res.Error = err.Error()
			rep.Errored++
			rep.TopErrors[shortError(err.Error())]++
		default:
			// which names landed in onMissing DURING this handler
			for _, m := range h.Missing() {
				if m.Calls > prev[m.Name] {
					res.Missing = append(res.Missing, m.Name)
					prev[m.Name] = m.Calls
				}
			}
			if len(res.Missing) > 0 {
				res.Outcome = "stubbed"
				rep.Stubbed++
			} else {
				res.Outcome = "clean"
				rep.Clean++
			}
		}
		if *verbose {
			fmt.Printf("%-10s %-40s %s\n", res.Outcome, name, res.Error)
		}
		rep.Results = append(rep.Results, res)
	}

	rep.Missing = h.Missing()
	sort.Slice(rep.Missing, func(i, j int) bool {
		if rep.Missing[i].Calls != rep.Missing[j].Calls {
			return rep.Missing[i].Calls > rep.Missing[j].Calls
		}
		return rep.Missing[i].Name < rep.Missing[j].Name
	})
	for _, m := range rep.Missing {
		rep.MissingTotal += m.Calls
	}

	// summary
	fmt.Printf("corpus         : %s\n", rep.Corpus)
	fmt.Printf("names indexed  : %d\n", rep.FilesIndexed)
	fmt.Printf("files loaded   : %d", rep.FilesLoaded)
	if len(rep.LoadErrors) > 0 {
		fmt.Printf(" (%d did NOT load)", len(rep.LoadErrors))
	}
	fmt.Printf("\n")
	fmt.Printf("click handlers : %d\n", rep.Handlers)
	pct := func(x int) float64 {
		if rep.Handlers == 0 {
			return 0
		}
		return 100 * float64(x) / float64(rep.Handlers)
	}
	fmt.Printf("  clean        : %d (%.1f%%)\n", rep.Clean, pct(rep.Clean))
	fmt.Printf("  stubbed      : %d (%.1f%%)  <- ran, but a name answered 0\n", rep.Stubbed, pct(rep.Stubbed))
	fmt.Printf("  errored      : %d (%.1f%%)\n", rep.Errored, pct(rep.Errored))
	fmt.Printf("distinct unbound names called : %d (calls %d)\n", len(rep.Missing), rep.MissingTotal)
	for i, m := range rep.Missing {
		if i == 20 {
			break
		}
		fmt.Printf("    %8d  %s\n", m.Calls, m.Name)
	}
	if len(rep.TopErrors) > 0 {
		fmt.Printf("top errors:\n")
		type kv struct {
			K string
			V int
		}
		var errs []kv
		for k, v := range rep.TopErrors {
			errs = append(errs, kv{k, v})
		}
		sort.Slice(errs, func(i, j int) bool { return errs[i].V > errs[j].V })
		for i, e := range errs {
			if i == 8 {
				break
			}
			fmt.Printf("    %5d  %s\n", e.V, e.K)
		}
	}
	for _, e := range rep.LoadErrors {
		fmt.Printf("load error: %s\n", e)
	}

	if *jsonPath != "" {
		out := rep
		out.Results = rep.Results
		blob, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(*jsonPath, blob, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *jsonPath, err)
			os.Exit(1)
		}
		fmt.Printf("json           : %s\n", *jsonPath)
	}
}

// shortError keeps the error's first line and strips the corpus path, so the
// error ranking groups handlers that failed the same way.
func shortError(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "../out/lua_corpus/"); i >= 0 {
		s = s[i+len("../out/lua_corpus/"):]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
