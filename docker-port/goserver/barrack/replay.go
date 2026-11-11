package barrack

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Replay packets are the ones the LIVE barrack server sent, captured verbatim.
//
// The client will not finish logging in on a LOGINOK alone: the live server
// sends BC_HOTKEY_BIND_INFO (56, 7571 bytes) immediately after it, then ten
// BC_ROTATION_CHAR_LIST (66) packets. Measured on the real client: with only
// LOGINOK it connects, then sits waiting and sends nothing until it gives up.
//
// These packets are STATIC server data - the three live captures are
// byte-identical - so replaying them is honest, not a shortcut. A packet that
// varied per player (names, stats, items) must NOT be replayed; it has to be
// built. See src\ge_barrack_server.py for the same distinction.
type replayManifest struct {
	Capture string `json:"capture"`
	Packets []struct {
		File   string `json:"file"`
		Cmd    uint16 `json:"cmd"`
		Name   string `json:"name"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
	} `json:"packets"`
}

// LoadReplay reads every captured packet in dir, in the order the live server
// sent them (the manifest's offset), and checks each one against its own
// command header so a renamed or truncated file fails loudly here instead of
// confusing the client.
func LoadReplay(dir string) ([][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read replay manifest: %w", err)
	}
	var m replayManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse replay manifest: %w", err)
	}
	if len(m.Packets) == 0 {
		return nil, fmt.Errorf("replay manifest lists no packets")
	}

	order := append([]struct {
		File   string `json:"file"`
		Cmd    uint16 `json:"cmd"`
		Name   string `json:"name"`
		Offset int    `json:"offset"`
		Length int    `json:"length"`
	}(nil), m.Packets...)
	sort.Slice(order, func(i, j int) bool { return order[i].Offset < order[j].Offset })

	out := make([][]byte, 0, len(order))
	for _, p := range order {
		// The login answer is BUILT from the database, never replayed: the
		// captured one carries another session's account id. Skip it so the
		// client does not receive two LOGINOKs.
		if p.Cmd == 8 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, p.File))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p.File, err)
		}
		if len(b) < 2 {
			return nil, fmt.Errorf("%s is too short to be a packet", p.File)
		}
		if cmd := binary.LittleEndian.Uint16(b); cmd != p.Cmd {
			return nil, fmt.Errorf("%s starts with command %d, manifest says %d",
				p.File, cmd, p.Cmd)
		}
		out = append(out, b)
	}
	return out, nil
}
