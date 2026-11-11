// Package protocol holds the wire format: the client's own command tables and
// the frame splitter that uses them.
//
// No game logic lives here. This package answers one question - "where does
// one packet end and the next begin" - because a wrong answer here misaligns
// the whole stream and the client crashes.
//
// There are TWO command tables, because the game has two id spaces:
//
//	data\cmd_table.txt           the ZONE ids (128..9529)
//	data\cmd_table_barrack.txt   the BARRACK/login ids (3..71), single digits
//
// A fixture must be checked against the table of its own id space.
package protocol

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Where the length word of a variable-size packet sits. It depends on the
// packet's base class, NOT on the command:
//
//	PKS_VARSIZE           command(2) len(2)                -> +2
//	PKS_SECURITY_VARSIZE  command(2) seq(4) chk(4) len(2)  -> +10
const (
	LenAtPlain    = 2
	LenAtSecurity = 10
)

// Table maps a command id to the size of that packet in bytes.
// A size of 0 means variable size: the packet carries its own length.
type Table map[uint16]int

// Size returns the size the table gives a command. ok is false when the
// command is not in the table at all, which is different from a size of 0.
func (t Table) Size(cmd uint16) (size int, ok bool) {
	size, ok = t[cmd]
	return size, ok
}

// Spec is a command table that also knows where each variable-size packet
// keeps its length word.
type Spec struct {
	sizes Table
	lenAt map[uint16]int // only meaningful for commands with size 0
}

// Size is the declared size of a command, 0 meaning variable size.
func (s *Spec) Size(cmd uint16) (size int, ok bool) { return s.sizes.Size(cmd) }

// LenAt is the offset of the length word for a variable-size command.
// ok is false when the shape is not known, in which case the packet cannot be
// cut and the splitter must not guess.
func (s *Spec) LenAt(cmd uint16) (off int, ok bool) {
	off, ok = s.lenAt[cmd]
	return off, ok
}

// LoadTable reads a cmd_table.txt file.
//
// One line per command: `id  size  NAME`. Lines that do not start with two
// numbers are skipped, exactly as src\test_golden.py skip them.
func LoadTable(path string) (Table, error) {
	t := Table{}
	err := eachRow(path, func(id uint16, size int, _ []string) {
		t[id] = size
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// LoadSpec reads a table that may carry the packet shape in a fourth column:
// `id  size  NAME  shape`, where shape is `fixed`, `len@2`, `len@10`, or `?`
// when the shape is unknown.
//
// The shape follows the packet's base class, and the two id spaces differ:
//
//   - a FOUR-column table states the shape on every size-0 row, so the column
//     is used and nothing is assumed (data\cmd_table_barrack.txt does this).
//   - a THREE-column table is a CZ_ / security id space. It is used only to cut
//     client->server data, and every CZ_ packet derives from PKS_SECURITY_*
//     with its length word at +10. So the default is len@10, NOT len@2: at +2
//     sits the sequence number, and reading that as a length silently drops the
//     packet (CZ_BUYITEM 3672 read its seq of 50 as a 50-byte packet and never
//     reached the handler).
func LoadSpec(path string) (*Spec, error) {
	s := &Spec{sizes: Table{}, lenAt: map[uint16]int{}}
	err := eachRow(path, func(id uint16, size int, fields []string) {
		s.sizes[id] = size
		if size != 0 {
			return // fixed: no length word to find
		}
		shape := "len@10" // LenAtSecurity: a three-column table is CZ_ only
		if len(fields) >= 4 {
			shape = fields[3]
		}
		switch {
		case shape == "?":
			// Unknown shape: leave it out, so LenAt reports false and the
			// splitter treats the packet as uncuttable instead of guessing.
		case strings.HasPrefix(shape, "len@"):
			off, err := strconv.Atoi(strings.TrimPrefix(shape, "len@"))
			if err == nil && off > 0 {
				s.lenAt[id] = off
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// eachRow reads `id size ...` lines and hands each one to fn.
func eachRow(path string, fn func(id uint16, size int, fields []string)) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open cmd table: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Fields(line)
		// The Python loader wants at least three fields (`id size NAME`);
		// keep that rule so both languages skip the same lines.
		if len(p) < 3 {
			continue
		}
		id, errID := strconv.ParseUint(p[0], 10, 16)
		size, errSize := strconv.Atoi(p[1])
		if errID != nil || errSize != nil || size < 0 {
			continue
		}
		fn(uint16(id), size, p)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read cmd table: %w", err)
	}
	return nil
}
