package zone

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The town NPC hide list: docker-port\config\zone_npc_hide.txt.
//
// The owner does not want every NPC the client's data carries. This is the list of
// ones to leave out, and it is a CONFIG file rather than code so an NPC can be removed
// from a town without a rebuild.
//
// The key is the NPC's SCRIPT name - the `script` field of data\zone_npcs\map<id>.json
// - not the caption the player reads: a caption is a locale token the client resolves,
// so it changes with the client's language, while the script name is what the server
// itself acts on.
//
// Two rule shapes, one per line:
//
//	SCRIPTNAME          hide that script on EVERY map
//	map<id>:SCRIPTNAME  hide it on one map only
//
// `#` starts a comment, blank lines are ignored, and matching ignores case. A hidden
// NPC is simply not spawned: no packet, no handle, nothing to click - which is what
// makes the client draw the town without it.

// hideRule is one line of the file, parsed.
type hideRule struct {
	mapID  int    // 0 = every map
	script string // upper case, trimmed: the comparison is case-insensitive
}

// NPCHide is the parsed list. A NIL *NPCHide is usable on purpose: it hides nothing,
// the same as a missing or empty file, so a caller never has to nil-check.
type NPCHide struct {
	rules []hideRule

	// Bad holds one line per rule that could not be read ("line 12: ..."), for the
	// caller to log. A bad line hides NOTHING: guessing what it meant is how the wrong
	// NPC disappears from a town.
	Bad []string
}

// LoadNPCHide reads the hide list.
//
// A MISSING file is not an error: it means "hide nothing", which is the state this
// server was in before the list existed. Only a file that exists and cannot be READ
// (a directory in its place, a permission problem) is reported, because then the
// operator believes a rule is in force and it is not.
func LoadNPCHide(path string) (*NPCHide, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &NPCHide{}, nil
		}
		return nil, err
	}
	return ParseNPCHide(string(blob)), nil
}

// ParseNPCHide reads the rules out of the file's TEXT. It exists so the parser can be
// tested without a file, and so a caller that already has the text (a future config
// watcher) can reuse it.
func ParseNPCHide(text string) *NPCHide {
	hide := &NPCHide{}
	for i, line := range strings.Split(text, "\n") {
		// A comment starts ANYWHERE on the line: the file's own header explains each
		// rule after the rule itself.
		if at := strings.IndexByte(line, '#'); at >= 0 {
			line = line[:at]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rule, err := parseHideRule(line)
		if err != nil {
			hide.Bad = append(hide.Bad, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		hide.rules = append(hide.rules, rule)
	}
	return hide
}

// parseHideRule reads one trimmed, comment-free line.
func parseHideRule(line string) (hideRule, error) {
	mapID := 0
	script := line
	if at := strings.IndexByte(line, ':'); at >= 0 {
		scope := strings.TrimSpace(line[:at])
		script = strings.TrimSpace(line[at+1:])
		if !strings.HasPrefix(strings.ToLower(scope), "map") {
			return hideRule{}, fmt.Errorf("%q is not a map scope - write SCRIPTNAME, or map<id>:SCRIPTNAME", scope)
		}
		id, err := strconv.Atoi(strings.TrimSpace(scope[3:]))
		if err != nil || id <= 0 {
			return hideRule{}, fmt.Errorf("%q does not name a map id - write map<id>:SCRIPTNAME", scope)
		}
		mapID = id
	}
	if script == "" {
		return hideRule{}, fmt.Errorf("no script name after the %q scope", "map<id>:")
	}
	return hideRule{mapID: mapID, script: strings.ToUpper(script)}, nil
}

// Rules is how many usable rules the list holds, for the startup summary. A "0" here
// is what makes an empty list visibly empty instead of a silent no-op.
func (h *NPCHide) Rules() int {
	if h == nil {
		return 0
	}
	return len(h.rules)
}

// Hides reports whether one NPC's script is on the list for this map.
func (h *NPCHide) Hides(mapID int, script string) bool {
	if h == nil || len(h.rules) == 0 {
		return false
	}
	script = strings.ToUpper(strings.TrimSpace(script))
	if script == "" {
		// An NPC with no click script cannot be named by a rule, and hiding every
		// script-less NPC because a rule said "" would empty half the town.
		return false
	}
	for _, r := range h.rules {
		if r.script != script {
			continue
		}
		if r.mapID == 0 || r.mapID == mapID {
			return true
		}
	}
	return false
}

// Apply drops the NPCs this list hides on mapID, answers the ones to spawn and how many
// were dropped. Order is kept, and so are the handles: a hidden NPC leaves a GAP in the
// numbering, and the client uses the handle, not the index.
func (h *NPCHide) Apply(mapID int, npcs []NPC) (kept []NPC, hidden int) {
	if h == nil || len(h.rules) == 0 || len(npcs) == 0 {
		return npcs, 0
	}
	kept = make([]NPC, 0, len(npcs))
	for _, npc := range npcs {
		if h.Hides(mapID, npc.Script) {
			hidden++
			continue
		}
		kept = append(kept, npc)
	}
	return kept, hidden
}
