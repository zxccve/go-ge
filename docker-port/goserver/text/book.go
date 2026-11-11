// Package text turns the game's locale tokens into the words the client draws.
//
// The scripts do not carry English: they carry the client's own TOKENS
// (`<$>14047</>`). A caption resolves itself in the client, but dialog text does
// not - a box filled with `<$>14047</>` is unreadable - so the server resolves the
// token before sending it (src\zone_server.py, `resolve_for_client`).
//
// Three tables, all the game's own:
//   - the dictionary: `<Text ClassID="10114047" Text="..."/>`, 20 MB, the ids the
//     client can actually draw;
//   - the server pack's dictionary: the same element, 188k entries, the ids the
//     SCRIPTS were written against. It is the OLDER generation's numbering (its ids
//     stop below 10,000,000), so it may only FILL a gap in the modern dictionary
//     and never overwrite one of its ids - see Book.ServerPack;
//   - the id map: our OLD id -> the client's id, 158k rows, kept in git
//     (data\locale_id_map.json). A shipped script says 14047, the dictionary knows
//     10146189, and only the map links the two.
//
// Both the ZONE (dialog text) and the SCRIPT ENGINE (`IMCFormat`) need this, which
// is why it is a package and not a file inside the zone.
package text

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DefaultDict, DefaultServerPack and DefaultIDMap are where the tables live when
// the server is started from goserver\ and the repo layout is intact. All three
// are relative on purpose: a hard-coded drive letter is what breaks first on the
// Linux VPS this server targets.
const (
	DefaultDict       = "../../ge/dictionary/dictionary_local.xml"
	DefaultServerPack = "../../ge/dictionary/dictionary_local_serverpack.xml"
	DefaultIDMap      = "../data/locale_id_map.json"

	// DefaultImage is the shared text image cmd/mktext writes. Independent of the working
	// directory being goserver\ or out\: both reach docker-port\out	ext_image.bin.
	DefaultImage = "../out/text_image.bin"
)

// TokenRe is the client's own token form. Only digits: a token with arguments
// (`<$>%d</>`) is a format, not a dictionary lookup, and is left alone.
var TokenRe = regexp.MustCompile(`<\$>(\d+)</>`)

// placeholderRe matches a text the CLIENT would have to fill in. Sending one is
// worse than sending the token: the box shows a literal "%s".
var placeholderRe = regexp.MustCompile(`%[0-9]*[dsf]`)

// Book holds the game's dictionaries and the id map.
//
// Each field is a *Table (text	able.go): the JSON/XML path fills it with a map, and the
// shared image path fills it with views into one mapped file - the difference is invisible
// to callers, which use Get/Len.
type Book struct {
	Dict  *Table // the modern client id -> text
	IDMap *Table // shipped script id -> client id

	// ServerPack is the server pack's dictionary: the ids the SCRIPTS were
	// written against, in the OLDER generation's numbering. It answers for ids
	// Dict has no entry for and is never asked about one it has - the two files
	// overlap on 136 ids whose meanings differ between generations, and the
	// modern one must win (see lookup).
	ServerPack *Table

	// Missing counts tokens with no entry, so the log can report how much of the
	// data these dictionaries actually cover.
	Missing int

	// unmapped releases the shared image when there is one. Nil for the map path.
	unmapped func()
}

// Close releases a mapped image. It is a no-op for the JSON/XML path, so a caller that
// does not care can ignore it; tests and a clean shutdown call it.
func (b *Book) Close() {
	if b != nil && b.unmapped != nil {
		b.unmapped()
		b.unmapped = nil
	}
}

// Load reads the id map, the client's dictionary and the server pack's
// dictionary. Either missing file is an error the caller can ignore (it then
// sends tokens, which is visibly wrong), so this returns the reason rather than
// exiting.
//
// serverPackPath is optional and defaults to DefaultServerPack, which sits next
// to DefaultDict: a server started from goserver\ finds both with no flag. A
// caller that knows better passes the path explicitly, the way -dict overrides
// DefaultDict.
//
// The server pack is a REQUIRED read, not a best effort: it carries the 9.8% of
// the scripts' tokens the modern dictionary has no id for, and a silently empty
// table would look exactly like a script bug.
func Load(idMapPath, dictPath string, serverPackPath ...string) (*Book, error) {
	packPath := DefaultServerPack
	if len(serverPackPath) > 1 {
		return nil, fmt.Errorf("server pack: want one path, got %d", len(serverPackPath))
	}
	if len(serverPackPath) == 1 {
		packPath = serverPackPath[0]
	}

	blob, err := os.ReadFile(idMapPath)
	if err != nil {
		return nil, fmt.Errorf("id map: %w", err)
	}
	var idMap map[string]string
	if err := json.Unmarshal(blob, &idMap); err != nil {
		return nil, fmt.Errorf("%s: %w", idMapPath, err)
	}

	dict, err := readDictionary(dictPath)
	if err != nil {
		return nil, fmt.Errorf("dictionary: %w", err)
	}
	pack, err := readDictionary(packPath)
	if err != nil {
		return nil, fmt.Errorf("server pack dictionary: %w", err)
	}
	return &Book{Dict: fromMap(dict), IDMap: fromMap(idMap), ServerPack: fromMap(pack)}, nil
}

// readDictionary is one file: read it, then parse it into id -> text. The wrap
// is left to the caller so the error names WHICH dictionary failed.
func readDictionary(path string) (map[string]string, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dict, err := parseDictionary(blob)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return dict, nil
}

// parseDictionary turns `<Text ClassID="id" Text="..."/>` entries into a map.
//
// It scans the bytes rather than unmarshalling XML: the file is 20 MB of one flat
// element per line, and a byte scan costs a fraction of a document tree while
// keeping every ENTRY (encoding/xml into a map would need a decoder loop anyway).
func parseDictionary(blob []byte) (map[string]string, error) {
	out := make(map[string]string, 200000)
	const open = "<Text "
	for i := 0; ; {
		j := bytes.Index(blob[i:], []byte(open))
		if j < 0 {
			break
		}
		start := i + j + len(open)
		end := bytes.IndexByte(blob[start:], '>')
		if end < 0 {
			break
		}
		tag := blob[start : start+end]
		i = start + end

		id, ok := attr(tag, "ClassID")
		if !ok {
			continue
		}
		value, ok := attr(tag, "Text")
		if !ok {
			continue
		}
		out[id] = unescapeXML(value)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no <Text ClassID=... Text=.../> entries found - not a dictionary file")
	}
	return out, nil
}

// attr reads one attribute value out of a tag body.
func attr(tag []byte, name string) (string, bool) {
	key := []byte(name + `="`)
	k := bytes.Index(tag, key)
	if k < 0 {
		return "", false
	}
	rest := tag[k+len(key):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return string(rest[:end]), true
}

var xmlEntities = strings.NewReplacer(
	"&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&",
)

func unescapeXML(s string) string { return xmlEntities.Replace(s) }

// Resolve turns every plain token in s into the text the client can draw.
//
// An id the dictionary does not know is LEFT AS THE TOKEN and counted: a box
// reading `<$>999</>` points straight at the bad id, while an empty box looks
// like a script bug. Text that still holds a placeholder is left alone too - the
// client cannot fill one, so the player would read "%s".
func (b *Book) Resolve(s string) string {
	if b == nil || !strings.Contains(s, "<$>") {
		return s
	}
	return TokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		value, ok := b.lookup(tok)
		if !ok || placeholderRe.MatchString(value) {
			b.Missing++
			return tok
		}
		return value
	})
}

// resolveFormatting is Resolve WITHOUT the placeholder filter: `IMCFormat` is
// about to fill those placeholders, so the text it needs is exactly the text
// Resolve refuses.
func (b *Book) resolveFormatting(s string) string {
	if b == nil || !strings.Contains(s, "<$>") {
		return s
	}
	return TokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		if value, ok := b.lookup(tok); ok {
			return value
		}
		return tok
	})
}

// lookup answers the text behind one token: the id map first (our old id -> the
// client's id), then the modern dictionary, then the server pack. The pack comes
// LAST on purpose - it is an older generation's numbering, so its text may fill a
// gap and never overwrite a modern id.
func (b *Book) lookup(tok string) (string, bool) {
	id := TokenRe.FindStringSubmatch(tok)[1]
	if clientID, ok := b.IDMap.Get(id); ok {
		id = clientID
	}
	if value, ok := b.Dict.Get(id); ok {
		return value, true
	}
	value, ok := b.ServerPack.Get(id)
	return value, ok
}

// Format is the script builtin `IMCFormat(token, args...)`: resolve the token
// FIRST, then fill its placeholders.
//
// The order matters and was measured live: formatting the token itself leaves
// every argument unplaced and the player reads a literal `%d`
// (src\script_host.py `_imc_format`, 2026-10-23). The C length prefixes the
// corpus uses (`%I64d`, `%ld`) are accepted and ignored, because Go needs no
// width hint for a 64-bit value.
func (b *Book) Format(token string, args []any) string {
	out := b.resolveFormatting(token)
	if len(args) == 0 || !strings.Contains(out, "%") {
		return out
	}
	return fill(out, args)
}

// fill substitutes placeholders in order. A placeholder with no argument left is
// copied through untouched, the way the Python host leaves a failed format alone
// rather than raising in the middle of a dialog.
func fill(format string, args []any) string {
	var out strings.Builder
	out.Grow(len(format) + 16)
	arg := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			out.WriteByte(format[i])
			continue
		}
		j := i + 1
		if format[j] == '%' {
			out.WriteByte('%')
			i = j
			continue
		}
		// flags, width and precision
		prec := -1
		for j < len(format) && strings.IndexByte("-+ 0#0123456789.", format[j]) >= 0 {
			if format[j] == '.' {
				prec = 0
			} else if prec >= 0 && format[j] >= '0' && format[j] <= '9' {
				prec = prec*10 + int(format[j]-'0')
			}
			j++
		}
		// C length prefixes: %I64d, %ld, %hd
		for j < len(format) && strings.IndexByte("I3264lh", format[j]) >= 0 {
			j++
		}
		if j >= len(format) {
			out.WriteString(format[i:])
			break
		}
		verb := format[j]
		if arg >= len(args) {
			out.WriteString(format[i : j+1])
			i = j
			continue
		}
		text, ok := subst(verb, prec, args[arg])
		if !ok {
			// The value does not fit the verb: keep the placeholder. Printing
			// nothing (or a Go type error) into a dialog box is worse.
			text = format[i : j+1]
		}
		out.WriteString(text)
		arg++
		i = j
	}
	return out.String()
}

// subst renders one value for one verb. ok=false means "this verb cannot take
// this value", and the caller then leaves the placeholder in place.
func subst(verb byte, prec int, v any) (string, bool) {
	num, isNum := v.(float64)
	switch verb {
	case 'd', 'i', 'u', 'x', 'X', 'o':
		if isNum {
			n := int64(num)
			switch verb {
			case 'x':
				return fmt.Sprintf("%x", n), true
			case 'X':
				return fmt.Sprintf("%X", n), true
			case 'o':
				return fmt.Sprintf("%o", n), true
			}
			return fmt.Sprintf("%d", n), true
		}
	case 'f', 'e', 'E', 'g', 'G':
		if isNum {
			p := prec
			if p < 0 {
				p = 6 // C's default for %f
			}
			return fmt.Sprintf("%.*"+string(verb), p, num), true
		}
	case 's', 'c':
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}
