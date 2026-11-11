package zone

// THE GM COMMAND SURFACE, SLICE 1 - `//hpup`, `//spup`, `//expup`, `//item`.
//
// WHY THIS EXISTS. Every test tonight needed a developer flag and a restart
// (`-spawndummy`, `-respawn`, `-splock`). The owner asked for the levers IN the
// game: "later maybe we can implement the GM commands so that i can use ingame
// command to test instead of doing something with the code" (2026-11-08). This is
// the first four, and every one of them rides a path the zone already had.
//
// THE WIRE PATH IS CHAT, AND THE PREFIX IS THE CLIENT'S OWN. The cheat panel's data
// `ge\server_xml\datatable_cheatlist.xml` (EUC-KR, 28 entries) holds every entry as
// `Scp='Chat("//cmd args")'` - `Chat("//hpup 1000000")`, `Chat("//item
// SteelBulletEA 10000")`. The client sends an ordinary CZ_CHAT (3888); our zone has
// parsed that packet since /fesoshop (zone\zone.go ParseChatText).
//
// THE PERMISSION IS A TABLE, LEVEL 1 IS THE GM TIER. The reference reads
// `GE_DATA.dbo.ACCOUNT_LEVEL(MEMBER_ID, LEVEL)` per family member; our PostgreSQL
// mirror is `account_level` (migration 00014). The level is read ONCE at
// START_GAME and carried on Session.GmLevel - never per chat line. An absent row
// is level 0, and a command from a session that is not exactly level 1 is refused.
//
// A `//` LINE IS NEVER BROADCAST. Chat is not relayed in this zone at all, so the
// property is structural: the branch runs BEFORE the Feso Shop and the dev-message
// branches, and the only packets it can return go to the asking session.
//
// WHAT EACH COMMAND REUSES, AND WHAT IT DOES NOT INVENT:
//
//	//hpup <n>   CFieldChar::AddHP's own three steps, as zone\bag_verbs.go already
//	            carries them for a healing potion: clamp, write Character.HP, send
//	            the 23-byte ZC_ADD_HP (3557) with the delta at +14. The upper bound
//	            is the character's MaxHP from the status script; the lower bound is
//	            0 - see gmHpUp.
//	//spup <n>   ZC_UPDATE_SP (3654), the same setter the skill cast sends with the
//	            caster's SP after SpendSP (zone\skill_area.go). 3654 carries the NEW
//	            value, not a delta, so the command writes the clamp and sends it.
//	//expup <n>  the kill award's own pair: creditExp (the per-handle total ledger,
//	            zone\exp_award.go) and the 23-byte ZC_EXP_UP (3629) whose +3 field
//	            is the character's TOTAL, not the gain.
//	//item <Name> <Count>  the item catalogue main.go already loads (zone\drop_tables.go
//	            ItemTable), resolved by ClassName; the ordinary bag write is
//	            h.Commit -> item.Op (the same door a pickup and a buy pay through).
//
// NO NUMBER IS INVENTED HERE. The command names are the cheat list's; the level
// is the reference table's; the item's stack rule is its own row (`MaxStack`,
// `Unstack`); the packets are the ones the existing paths build.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"geserver/item"
)

const (
	// gmCommandPrefix is the DOUBLE slash the cheat list sends. A single `/` is
	// the client's own local command surface (datatable_command.xml) and must keep
	// falling through untouched.
	gmCommandPrefix = "//"

	// gmLevelOperator is the GM tier. The reference's ACCOUNT_LEVEL says 1; a
	// session must be exactly this to dispatch. Anything else - including a level
	// above 1 this build does not define - is refused rather than guessed at.
	gmLevelOperator = 1
)

// isGmCommand reports whether a chat line is a command. The text is trimmed
// first: a leading space must not smuggle a `//` line past the branch into chat.
func isGmCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), gmCommandPrefix)
}

// gmRefusal is the existing one-sender refusal packet: ZC_MESSAGE with type 0 and
// level 0, the cyan system line the warp refusals already use (zone\commwarp.go
// warpTravelRefusedText, zone\gate.go). It goes ONLY to the session that asked.
func gmRefusal(text string) []byte {
	return BuildMessage(text, MessageTypeGeneral, MessageLevelGeneral)
}

// gmCommand dispatches one `//` line, or refuses it. It is reached from chat and
// returns packets for that session alone.
func (h *Handler) gmCommand(ctx context.Context, sess *Session, text string) [][]byte {
	line := strings.TrimSpace(text)
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	name := strings.TrimPrefix(fields[0], gmCommandPrefix)
	args := fields[1:]

	// THE PERMISSION CHECK COMES FIRST, before parsing or any side effect. One log
	// line per distinct command (Session.noteChat), naming the command and the
	// level; the sender alone is told. The line is rate-limited because a level-0
	// client can type `//` as fast as chat, and this server's log is the record.
	if sess == nil || sess.GmLevel != gmLevelOperator {
		level := 0
		if sess != nil {
			level = sess.GmLevel
		}
		if sess == nil || sess.noteChat("gm-refused:"+line) {
			h.Log.Printf("zone: GM command %q refused: this session is level %d, "+
				"not %d - nothing was broadcast", line, level, gmLevelOperator)
		}
		if sess == nil {
			return nil
		}
		return [][]byte{gmRefusal(fmt.Sprintf(
			"%s: GM command refused - this account is level %d, not %d",
			fields[0], level, gmLevelOperator))}
	}

	switch strings.ToLower(name) {
	case "hpup":
		return h.gmHpUp(sess, line, args)
	case "spup":
		return h.gmSpUp(sess, line, args)
	case "expup":
		return h.gmExpUp(sess, line, args)
	case "item":
		return h.gmItem(ctx, sess, line, args)
	default:
		// A TYPO IS NEVER SWALLOWED. It gets its own line (once per distinct text,
		// like chat) and one answer to the sender; it changes no state.
		if sess.noteChat("gm-unknown:" + line) {
			h.Log.Printf("zone: GM command %q - unknown command - nothing was dispatched",
				line)
		}
		return [][]byte{gmRefusal("unknown command: " + line)}
	}
}

// gmUsage is one malformed command: one log line with the reason and one answer to
// the sender. It changes nothing.
func (h *Handler) gmUsage(sess *Session, line, why, usage string) [][]byte {
	h.Log.Printf("zone: GM command %q: %s - nothing dispatched (usage: %s)",
		line, why, usage)
	return [][]byte{gmRefusal(usage)}
}

// gmTarget answers the character a command acts on: the one the PLAYER DRIVES
// (Session.Control), which is the same sense every other player action uses. A
// session with no such slot is refused rather than defaulted to slot 0, because a
// command that hits the wrong body is worse than one that does nothing.
func (h *Handler) gmTarget(sess *Session, line, verb string) (*Character, int, bool) {
	idx := sess.Control
	if idx < 0 || idx >= len(sess.Team) {
		h.Log.Printf("zone: GM %s %q refused: the session drives slot %d but has %d "+
			"character(s) - nothing sent", verb, line, idx, len(sess.Team))
		return nil, 0, false
	}
	return &sess.Team[idx], idx, true
}

// gmHpUp is `//hpup <n>` - the same clamped AddHP the potion path runs
// (zone\bag_verbs.go, THE HP HALF), with the number typed instead of read from an
// item's arg1.
//
// WHICH BOUNDS, AND WHY. Upper: the character's own MaxHP from the status script,
// because that is the bar's own end and the reference's AddHP clamps exactly there
// (`if (mhp <= expectHP) expectHP = mhp;`, FieldChar.cpp:132-165). Lower: 0,
// because HP is a bar and the client has no negative-HP state to draw; a GM using
// the cheat list's own `//hpup -1000000` sees the bar empty, and killing a body is
// the death path's job, not a bar command's. A negative n is ACCEPTED on purpose -
// the client's own list carries one, and lowering HP is how a damage test starts.
//
// NO CHANGE, NO PACKET: the reference's AddHP returns 0 when hp == expectHP
// (:156-157), so a `//hpup 0` or a heal at full health writes a line and sends
// nothing.
func (h *Handler) gmHpUp(sess *Session, line string, args []string) [][]byte {
	const usage = "//hpup <n>   (n may be negative; HP is clamped to 0..max)"
	if len(args) != 1 {
		return h.gmUsage(sess, line, fmt.Sprintf("%d argument(s), want 1", len(args)), usage)
	}
	n, err := strconv.ParseFloat(args[0], 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return h.gmUsage(sess, line, fmt.Sprintf("%q is not a finite number", args[0]), usage)
	}
	char, idx, ok := h.gmTarget(sess, line, "hpup")
	if !ok {
		return nil
	}
	if char.MaxHP <= 0 {
		h.Log.Printf("zone: GM hpup refused: commander %d char %d (%s) has max HP 0 - "+
			"the status script has not answered, so there is no bar to clamp to - "+
			"nothing sent", sess.CommanderID, idx, char.Name)
		return nil
	}

	h.swingMu.Lock()
	before := char.HP
	after := before + n
	if after > char.MaxHP {
		after = char.MaxHP
	}
	if after < 0 {
		after = 0
	}
	char.HP = after
	h.swingMu.Unlock()

	delta := after - before
	if delta == 0 {
		h.Log.Printf("zone: GM hpup commander %d char %d (handle %d) n=%g: HP is "+
			"already %g of max %g, so the command changes NOTHING and no %d is sent "+
			"(AddHP returns 0 when hp == expectHP, FieldChar.cpp:156-157)",
			sess.CommanderID, idx, char.Handle, n, before, char.MaxHP, CmdZCAddHP)
		return nil
	}

	// +6 the new value, +14 the delta, +22 = 1 when the delta is negative - the
	// builder already sets that last byte, and the potion path's header explains
	// why +14 is the delta and not 0.
	pkt := BuildAddHP(char.Handle, after, delta)
	flag := byte(0)
	if delta < 0 {
		flag = 1
	}
	seen := h.fanOut(sess, char.Handle, pkt)
	h.Log.Printf("zone: GM hpup commander %d char %d (handle %d) n=%g: HP %g -> %g "+
		"of max %g (delta %g; clamped to 0..max); cmd %d (%d B), +6 = %g, +14 = %g, "+
		"+22 = %d, to %d other player(s) beside this session",
		sess.CommanderID, idx, char.Handle, n, before, after, char.MaxHP, delta,
		CmdZCAddHP, len(pkt), after, delta, flag, seen)
	return [][]byte{pkt}
}

// gmSpUp is `//spup <n>` - the SP twin. The zone HAS an SP path, but it is a
// SETTER, not an add: ZC_UPDATE_SP (3654) carries the new value and the skill cast
// already uses it after SpendSP (`c.SP = uint16(sp); BuildUpdateSP(...)`,
// zone\skill_area.go). So the command adds n, clamps to 0..MaxSP, writes the
// ledger and sends that same packet. Nothing here invents an SP delta packet: the
// client's table has none (data\cmd_table.txt has 3654 and no 3558-style twin).
func (h *Handler) gmSpUp(sess *Session, line string, args []string) [][]byte {
	const usage = "//spup <n>   (n may be negative; SP is clamped to 0..max)"
	if len(args) != 1 {
		return h.gmUsage(sess, line, fmt.Sprintf("%d argument(s), want 1", len(args)), usage)
	}
	n, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return h.gmUsage(sess, line, fmt.Sprintf("%q is not a whole number", args[0]), usage)
	}
	char, idx, ok := h.gmTarget(sess, line, "spup")
	if !ok {
		return nil
	}
	if char.MaxSP == 0 {
		h.Log.Printf("zone: GM spup refused: commander %d char %d (%s) has max SP 0 - "+
			"the status script has not answered, so there is no gauge to clamp to - "+
			"nothing sent", sess.CommanderID, idx, char.Name)
		return nil
	}

	before := char.SP
	sp := int64(before) + n
	if sp > int64(char.MaxSP) {
		sp = int64(char.MaxSP)
	}
	if sp < 0 {
		sp = 0
	}
	char.SP = uint16(sp)
	pkt := BuildUpdateSP(char.Handle, uint32(sp))
	h.Log.Printf("zone: GM spup commander %d char %d (handle %d) n=%d: SP %d -> %d "+
		"of max %d (clamped to 0..max); cmd %d (%d B), the same setter a skill cast "+
		"sends after SpendSP",
		sess.CommanderID, idx, char.Handle, n, before, char.SP, char.MaxSP,
		CmdZCUpdateSP, len(pkt))
	return [][]byte{pkt}
}

// gmExpUp is `//expup <n>` - the character's own exp path. The zone awards exp on
// a kill already: creditExp keeps the per-handle running total (starting from the
// experience the character loaded with) and BuildExpUp is the 23-byte ZC_EXP_UP
// whose +3 field is the TOTAL, not the gain (zone\exp_award.go). This command uses
// that exact pair, so the client's exp bar reads the same number a kill would give
// it.
//
// n must be positive: an "exp up" of zero or a subtraction is not an award, and
// the kill path never subtracts. The total is never allowed below 0 by a positive
// n, so no extra clamp is needed.
func (h *Handler) gmExpUp(sess *Session, line string, args []string) [][]byte {
	const usage = "//expup <n>   (n > 0)"
	if len(args) != 1 {
		return h.gmUsage(sess, line, fmt.Sprintf("%d argument(s), want 1", len(args)), usage)
	}
	n, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || n <= 0 {
		return h.gmUsage(sess, line, fmt.Sprintf("%q is not a positive whole number", args[0]), usage)
	}
	char, idx, ok := h.gmTarget(sess, line, "expup")
	if !ok {
		return nil
	}

	total := h.creditExp(char.Handle, char.Exp, n)
	// stanceExp 0 and autoRule 0 for the same reasons the kill award gives: this
	// build's stance records all say level 1 / 0 exp, and no auto-inspect rule is
	// carried.
	pkt := BuildExpUp(uint8(idx), total, char.Stance, 0, 0)
	h.Log.Printf("zone: GM expup commander %d char %d (handle %d) +%d -> total %d; "+
		"cmd %d (%d B) - the same ZC_EXP_UP the kill award sends",
		sess.CommanderID, idx, char.Handle, n, total, CmdZCExpUp, len(pkt))
	return [][]byte{pkt}
}

// gmItem is `//item <Name> <Count>` - give an item by the game's OWN ClassName.
//
// THE NAME IS RESOLVED THROUGH THE TABLE THE ZONE ALREADY LOADS. main.go arms
// ItemTable from the client's `datatable_item*.xml` at start (zone\drop_tables.go),
// and IDByName is the same lookup the crystal drop uses. An unknown name is
// refused with ONE line naming it - never silently ignored, and never passed to
// the bag as id 0.
//
// THE BAG WRITE IS THE ORDINARY DOOR: h.Commit -> item.Op, the transaction a
// pickup, a buy and a script all use (zone\items.go). Merge and MaxStack come from
// the item's own row: `Unstack` false means the stack may grow, and `MaxStack` is
// the client's per-slot cap. This command does NOT split an unstackable item into
// several slots; a count above its cap is refused by the write path with the cap
// named, which is honest and keeps one write rule.
func (h *Handler) gmItem(ctx context.Context, sess *Session, line string, args []string) [][]byte {
	const usage = "//item <Name> <Count>   (Name is the game's ClassName, e.g. SteelBulletEA)"
	if len(args) != 2 {
		return h.gmUsage(sess, line, fmt.Sprintf("%d argument(s), want 2", len(args)), usage)
	}
	name := args[0]
	count, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || count <= 0 {
		return h.gmUsage(sess, line, fmt.Sprintf("count %q is not a positive whole number", args[1]), usage)
	}
	if h.Items == nil {
		h.Log.Printf("zone: GM item %q refused: no Item table is loaded "+
			"(datatable_item*.xml), so %q cannot be resolved - nothing written",
			line, name)
		return nil
	}
	id, known := h.Items.IDByName(name)
	if !known {
		h.Log.Printf("zone: GM item %q refused: the Item table has no ClassName %q "+
			"(%d row(s) loaded) - nothing written", line, name, h.Items.Len())
		return [][]byte{gmRefusal(fmt.Sprintf("//item: unknown item %q", name))}
	}
	row, known := h.Items.Row(id)
	if !known {
		h.Log.Printf("zone: GM item %q refused: ClassName %q resolves to class %d "+
			"but the Item table has no row for it - nothing written", line, name, id)
		return [][]byte{gmRefusal(fmt.Sprintf("//item: class %d has no row", id))}
	}
	if h.Commit == nil || sess.CommanderID == 0 || sess.Inv == nil {
		h.Log.Printf("zone: GM item %q refused: %s - nothing written", line,
			gmItemMissingWiring(h.Commit, sess))
		return nil
	}

	op := item.Op{
		Give:     true,
		ClassID:  int64(id),
		Count:    count,
		Merge:    !row.Unstack,
		MaxStack: row.MaxStack,
		Name:     row.Name,
	}
	changes, err := h.Commit(ctx, sess.CommanderID, nil, []item.Op{op})
	if err != nil {
		h.Log.Printf("zone: GM item %q refused: commander %d's bag refused %s x%d "+
			"(%v) - nothing written", line, sess.CommanderID, row.Name, count, err)
		return [][]byte{gmRefusal(fmt.Sprintf("//item %s: refused - %v", name, err))}
	}

	var out [][]byte
	for _, c := range changes {
		if !c.Give {
			continue
		}
		if p := sess.Inv.PacketFor(c, AddItemPlain, RemoveItemTake); p != nil {
			out = append(out, p)
		}
	}
	h.Log.Printf("zone: GM item %q: commander %d +%d x class %d (%s), merge %v, "+
		"max stack %d; %d change(s), %d packet(s) (cmd %d ZC_ADD_ITEM)",
		line, sess.CommanderID, count, id, row.Name, op.Merge, row.MaxStack,
		len(changes), len(out), CmdZCAddItem)
	return out
}

// gmItemMissingWiring names the one missing piece, so the refusal line says what
// to fix rather than "refused".
func gmItemMissingWiring(commit func(context.Context, int32, map[string]int64, []item.Op) ([]item.Change, error), sess *Session) string {
	switch {
	case commit == nil:
		return "this zone has no bag write path wired (Handler.Commit is nil)"
	case sess.CommanderID == 0:
		return "the session's commander is not resolved (0)"
	default:
		return "the session has no inventory mirror (Inv is nil)"
	}
}
