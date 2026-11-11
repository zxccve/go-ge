package barrack

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"geserver/address"
	"geserver/protocol"
	"geserver/world"
)

// Session is what the server learns about one connection.
//
// It lives as long as the connection, so two players no longer share state -
// the Python server kept one global for both.
type Session struct {
	// CommanderID is learned at login and decides whose barrack is built.
	CommanderID uint32
	// AccountID is what the login answer carried.
	AccountID uint32
	// LoginSeq is this connection's place in the account's login order, stamped at
	// CB_LOGIN by `account_info.login_seq` (login_seq.go). It is what makes a Start
	// Game from a SUPERSEDED connection refusable: 0 means "not stamped" (no sequence
	// source, or the write failed), and an unstamped connection is never refused.
	LoginSeq int64
	// MemberID and RemoteIP are what GE_LOG.ACCOUNT_LOGIN keeps: the name the player
	// typed and the address the barrack connection came from. Recorded once at login and
	// once at logout (the original's two tables, db/migrations/00018_usage_log.sql).
	MemberID string
	RemoteIP string
	// BarrackInfo is the BC_BARRACK_INFO we built for this session. It answers
	// the client's later CB_REQ_BARRACK_INFO; without it the captured packet
	// (another account's 159 characters) was sent on top of ours.
	BarrackInfo []byte

	// JPBarrackSet is the JP barrack set DEFERRED until CB_REQ_BARRACK_INFO.
	// The JP client (like the later v42 client) wants only BC_COMMANDER_INFO
	// after CB_STARTBARRACK; the rest of the set arrives in answer to op 15.
	// Sending it twice (once in the burst, once here) is what it crashed on.
	JPBarrackSet [][]byte
}

// Handler turns frames into replies. It owns no connection state: that lives in
// Session, and the socket loop lives in Serve.
type Handler struct {
	Store   Store
	Barrack BarrackStore
	Table   *protocol.Spec
	Cipher  *protocol.Cipher
	Log     *log.Logger

	// AfterLogin are the captured packets the live barrack sends right after
	// LOGINOK (the script-binding table, cmd 56). Without it the client
	// connects, then waits and sends nothing.
	AfterLogin [][]byte

	// AfterStartBarrack are the captured packets that follow when the client
	// asks to enter the barrack (cmd 9).
	//
	// THE ORDER IS NOT OPTIONAL. Measured by the Python server on 2026-10-21:
	// the job lists (cmd 66) must arrive BEFORE BC_COMMANDER_INFO and must
	// arrive NINE times, or the client crashes (0xc0000409). One list before,
	// or nine after, both crash. The replay keeps the live capture's order, so
	// do not sort or filter these.
	AfterStartBarrack [][]byte

	// RecordLogin / RecordLogout are the usage-log hooks (db.RecordLogin, wired in main.go).
	// NIL IS FINE and is the default in tests: a barrack with no database writes no record
	// and still lets the player in - a record is never a reason to refuse a login.
	RecordLogin  func(ctx context.Context, memberID, ip string)
	RecordLogout func(ctx context.Context, memberID, ip string)

	// LoginSeqs is where an account's login sequence lives (login_seq.go): it is
	// stamped at CB_LOGIN and read back at CB_STARTGAME, so a Start Game from a
	// connection that a newer login has SUPERSEDED is refused.
	//
	// NIL IS THE NORMAL CASE IN PRODUCTION, and it does not disable the rule: a nil
	// field means "use Store when it can do the job", and PGStore - the login store,
	// which already owns account_info - implements LoginSeqStore. The field is here
	// for a test and for a backend with another source.
	LoginSeqs LoginSeqStore

	// AfterBarrackInfo answers CB_REQ_BARRACK_INFO (15) with BC_BARRACK_INFO.
	AfterBarrackInfo [][]byte

	// JP sends the built barrack-info set (16/17/26/35/41/57/65) IN the
	// STARTBARRACK burst instead of waiting for CB_REQ_BARRACK_INFO (15).
	// Measured 2026-11-06: the JP 375639 client logs in (LOGINOK), takes the
	// 2020 cmd 56, sends CB_STARTBARRACK, then sits on the loading screen and
	// NEVER sends op 15 - so without this flag it never receives cmd 16.
	JP bool

	// JPList16 is a diagnostic: answer CB_REQ_BARRACK_INFO with cmd 16 only,
	// dropping 17 and 35. Use it to tell WHICH of the three crashes the JP
	// client; it is not a shipping mode.
	JPList16 bool

	// JPSubset, when non-empty, is a diagnostic filter for the deferred JP set:
	// only these command ids are sent on CB_REQ_BARRACK_INFO. It lets us test
	// 16+17+35 (the 2020 minimum) without the extra set packets.
	JPSubset map[uint16]bool

	// LoginOKCapture, when set, is sent INSTEAD of the built login answer.
	//
	// PROBE ONLY. The captured barrack packets belong to the capture's own
	// account, while a normal login answers with the database's account. Mixing
	// the two makes the client drop the session a second after the barrack
	// appears, because the ids it was told do not match the data it received.
	LoginOKCapture []byte

	// ZoneAddr is the host:port the client is told to enter, in BC_STARTGAMEOK
	// (cmd 25). Empty means "compose it from AdvertisedIP and DefaultZonePort" -
	// see fallbackAddr - so the address COMES FROM THE DATABASE by default and a
	// -zone flag is only a one-off override.
	ZoneAddr string

	// AdvertisedIP is the ip the client is told to dial when NO live zone
	// announces the map: the machine row's ip (or -advertise), resolved ONCE at
	// startup by address.Resolve, which also logs where it came from.
	//
	// Empty is allowed and means the documented default (address.DefaultIP), so a
	// Handler built without one - a test, or a caller from before the address book
	// - answers exactly what this server always answered.
	AdvertisedIP string

	// AskForZones makes a missing map a REQUEST to the zone manager (cmd/zoneman)
	// instead of a refusal. main.go sets it; tests leave it false.
	AskForZones bool

	// Zones is the world layer's zone registry: it answers "which live zone
	// serves the map this player is saved on, and what is that map's class
	// name?". When it has an answer it BEATS ZoneAddr and the map table, because
	// a live zone's own announcement is the only thing that knows which world it
	// is really serving.
	//
	// Nil is allowed and means "no registry": the flag and the table are used
	// instead, which is exactly what the server did before the registry existed.
	Zones *world.Registry

	// MapNames answers a map id's CLASS name from the shared spawn table
	// (data\zone_points.json, written by src\emit_zone_points.py), which carries
	// every map the client has. It sits BETWEEN the registry and the built-in
	// table: a live zone knows best, the table knows more than the two ids this
	// build was compiled with.
	//
	// Nil is allowed and means "no table": the built-in table and then the
	// reb_basic constant answer, as before.
	MapNames func(mapID uint16) (string, bool)

	// WarpMaps turns a RESTART WARP ID into the map that owns it, from the same
	// spawn table (zone\ZonePoints.RestartWarpMap). It is needed because the SAVED
	// START POINT is stored as a warp id alone (`commander.point`: 203 Reboldoeux,
	// 213 Coimbra) while cmd 10's newMapID needs the MAP.
	//
	// Nil is allowed and means "no table": a saved start point then cannot be
	// named, and the barrack falls back to the last point's map, exactly as it did
	// before the save point existed. A missing table must never stop a login.
	WarpMaps func(warpID uint16) (uint16, bool)
}

// DefaultZonePort is the port cmd 25 names when NO live zone announces the map
// and -zone was not given. It is the port this project's own zone listens on.
//
// THE IP IS NOT HERE ANY MORE, and that is the point: it comes from the address
// book (`machine` in PostgreSQL, package address) so a player on a SECOND PC can
// be pointed at this machine's LAN address by changing one row, not by rebuilding.
// A port is a different thing from an address: a town announces the port its own
// listener BOUND, and this constant is only the fallback for a map nothing
// announced at all.
const DefaultZonePort = 7002

// FallbackAddr is the address cmd 25 carries when no live zone names a map: the
// advertised ip and DefaultZonePort. A blank ip means the documented default
// (address.Fill), so an unconfigured barrack keeps the old answer.
func FallbackAddr(ip string) string {
	return net.JoinHostPort(address.Fill(ip), strconv.Itoa(DefaultZonePort))
}

// fallbackAddr is what THIS barrack answers when no live zone names the map. The
// -zone flag wins when it was given (a one-off run against another machine);
// otherwise the address is the ADVERTISED ip plus DefaultZonePort, so the address
// book is the single place this machine's address is decided.
func (h *Handler) fallbackAddr() string {
	if h.ZoneAddr != "" {
		return h.ZoneAddr
	}
	return FallbackAddr(h.AdvertisedIP)
}

// isNoRows keeps the pgx dependency out of callers.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// barrackMaps is the pair of maps cmd 10 names and cmd 35 must carry an entry for:
// the map a NEW GAME enters at (the SAVED start point, resolved from `commander.point`
// through the warp table) and the map CONTINUE resumes (the LAST point).
//
// THE FALLBACKS ARE PART OF THE CONTRACT:
//   - no start point saved (point 0), no warp table wired, or a warp id that no map
//     carries -> the new-game map becomes the last point's map, which is the single-map
//     barrack this build shipped before the save point existed;
//   - never 0 for the new-game map: the client dereferences that map's name with no
//     null check (GEBarrackNet.cpp:234-235) and 0 is not a map.
//
// Resolved ONCE per barrack so cmd 10 and cmd 35 cannot disagree - a channel list that
// names a map cmd 10 does not is the crash from 2026-10-29 21:31:20.
func (h *Handler) barrackMaps(cmdr CommanderRow) (saveMap, lastMap uint16) {
	lastMap = cmdr.ZoneID
	saveMap = h.saveMapOf(cmdr)
	if saveMap == 0 {
		saveMap = lastMap
	}
	// A FAMILY THAT HAS NEVER PLAYED HAS NO MAP AT ALL, and neither field may be 0: the
	// client dereferences geMapTable::Get(newMapID) with no null check
	// (source2\geClient\Source\GEBarrackNet.cpp:230). Resolved HERE, once, so cmd 10 and
	// cmd 35 cannot disagree - a new family starts at STARTONIA, map 18 `city_tuto`
	// (owner's rule, 2026-11-11). contMapID stays whatever it was: that field IS
	// null-checked and "no last point" is exactly true for a family that never played.
	if saveMap == 0 {
		saveMap = newFamilyStartMap
	}
	return saveMap, lastMap
}

// saveMapOf resolves a commander's SAVED START POINT to the map owning its restart
// warp id, or 0 when there is nothing saved or the warp id does not resolve.
//
// A warp id and not a map, because `commander.point` holds what the original server
// stored (Commander.cpp:589 GetRestartWarpID): 203 Reboldoeux, 213 Coimbra. The map
// comes from whichever map's `restart` row in data\zone_points.json names that warp id
// (zone\ZonePoints.RestartWarpMap; zone\r70_test.go pins 213 -> 15 and 203 -> 10).
//
// 0 is not an error when nothing is saved: that is the normal state before the player
// ever presses Save, and it is not logged. A warp id that DOES not resolve is logged,
// naming it, because that is a data error worth finding - and the caller then falls
// back to the last point's map rather than inventing a world.
func (h *Handler) saveMapOf(cmdr CommanderRow) uint16 {
	if cmdr.SaveWarpID == 0 {
		return 0
	}
	if h.WarpMaps == nil {
		h.Log.Printf("BARRACK: commander %d has saved start point warp %d but no map "+
			"table is wired in (Handler.WarpMaps is nil) - using the last point's map %d",
			cmdr.ID, cmdr.SaveWarpID, cmdr.ZoneID)
		return 0
	}
	mapID, ok := h.WarpMaps(cmdr.SaveWarpID)
	if !ok || mapID == 0 {
		h.Log.Printf("BARRACK: commander %d has saved start point warp %d and no map's "+
			"restart row carries that warp id - using the last point's map %d",
			cmdr.ID, cmdr.SaveWarpID, cmdr.ZoneID)
		return 0
	}
	return mapID
}

// Reply answers one frame, returning the PLAINTEXT packets to send, in order.
// An empty result with a nil error means "no answer for this command".
//
// Encryption happens in Serve, not here: the transport is not the handler's
// business, and keeping it out means the reply can be tested as plain bytes.
func (h *Handler) Reply(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	switch f.Cmd {
	case 4: // CB_LOGIN
		return h.login(ctx, sess, f.Data)
	case 9: // CB_STARTBARRACK - "show me the barrack"
		return h.startBarrack(ctx, sess)
	case 11: // CB_CREATE_COMMANDER - the family name, from the client's own screen
		return h.createCommander(ctx, sess, f.Data)
	case 18: // CB_CREATE_CHAR - the character from the client's own create screen
		return h.createChar(ctx, sess, f.Data)
	case 15: // CB_REQ_BARRACK_INFO - the answer is BC_BARRACK_INFO (cmd 16)
		// A SESSION WITH NO FAMILY still gets cmd 16 - the client waits for it and hangs
		// otherwise - but FLAGGED (byte 10 = 1) so its handler does not re-evaluate the
		// screen into CharacterCreate (ge.exe FUN_00521a49), and followed by cmd 14, which
		// is what opens the create-family screen.
		if sess.CommanderID == 0 {
			// Same answer as the start burst: the no-commander message, because this client
			// asks for the barrack again and there is no barrack to describe.
			h.Log.Printf("REQ_BARRACK_INFO: no family on this session - no-commander BC_MESSAGE")
			return [][]byte{BuildNoCommanderMessage(sess.MemberID)}, nil
		}
		if h.JP && len(sess.JPBarrackSet) > 0 {
			out := sess.JPBarrackSet
			if len(h.JPSubset) > 0 {
				out = make([][]byte, 0, len(sess.JPBarrackSet))
				for _, p := range sess.JPBarrackSet {
					if len(p) >= 2 && h.JPSubset[binary.LittleEndian.Uint16(p)] {
						out = append(out, p)
					}
				}
			}
			h.Log.Printf("REQ_BARRACK_INFO: JP sends the deferred set (%d of %d packets, subset=%v)",
				len(out), len(sess.JPBarrackSet), len(h.JPSubset) > 0)
			return out, nil
		}
		// Rebuild it from the database when we can. The client asks for the barrack
		// again when it QUITS THE ZONE back to the barrack, and by then the team may
		// have changed (SaveTeamPositions writes it on every start). Serving the copy
		// built at login meant the client was handed a barrack from BEFORE the team
		// was picked: no previous team, so "Last Point" stayed grey despite the row
		// being correct in the database.
		if h.Barrack != nil && sess.CommanderID != 0 {
			chars, err := h.Barrack.Characters(ctx, sess.CommanderID)
			if err == nil {
				if h.JP {
					sess.BarrackInfo = BuildBarrackInfoJP(chars)
				} else {
					sess.BarrackInfo = BuildBarrackInfo(chars)
				}
				// A FAMILY WITH NO CHARACTERS GETS THE CHARACTER LIST AND NOTHING ELSE.
				// cmd 16 is what switches the client to the character screen when the count is
				// zero, but the packets that normally follow it (17 equip extra, 35 channel
				// list, 41/57/65) re-render the BARRACK: measured 2026-11-11 on the owner's
				// client, after creating a family the answer landed on the barrack with a blank
				// 3D view (screenshot) while a re-login - which takes the same set - showed
				// character creation. With nothing to equip or team up, the rest is noise that
				// can only win the screen back.
				if len(chars) == 0 {
					// THE CHANNEL LIST STILL GOES OUT. cmd 16 is what opens the character
					// screen, but cmd 35 is what fills the New Game drop-down - without it the
					// client has no map to start on and the entry list is EMPTY (owner,
					// 2026-11-11: "now zone dont appear?", after a family made in-session had
					// only ever been answered with cmd 16 alone). Order matters: 35 first, 16
					// LAST, so the screen still ends on character creation.
					out := make([][]byte, 0, 2)
					if cmdr, found, err := h.Barrack.Commander(ctx, sess.CommanderID); err == nil && found {
						saveMap, lastMap := h.barrackMaps(cmdr)
						out = append(out, BuildChannelSet(ChannelsForEntry(saveMap, lastMap, cmdr.ChannelID)))
					}
					out = append(out, sess.BarrackInfo)
					h.Log.Printf("REQ_BARRACK_INFO: 0 characters - channel list then cmd 16 (%d packet(s))",
						len(out))
					return out, nil
				}
				// Answer with 16, then 17, then 35 - the working Python
				// barrack's set, in its order:
				//     conn.sendall(build_barrack_info(chars))    # 16
				//     conn.sendall(build_equip_extra(chars))     # 17
				//     conn.sendall(build_channel_list(...))      # 35
				// 16 rebuilds the character list and 35 is the refresh trigger
				// (CBarrackNet::ChannelList ends with BarrackMode()->UpdateTeamButton()),
				// so the channel list must come AFTER the rebuild: sending 16 in
				// the burst as well made the barrack rebuild AFTER the refresh and
				// "Last Point" kept the state computed before the team existed.
				//
				// 41/57/65 come AFTER 35 and are appended below, never in the
				// burst: the reference server does not send them, so burstSkip
				// drops their captured copies, and for a long time nothing
				// replaced them - the team-group and preset panels showed
				// whatever the client already had (ROADMAP R41). They ride here
				// because this is the answer to the client's own request, i.e.
				// the barrack already exists; nothing below rebuilds it.
				out := [][]byte{sess.BarrackInfo}
				if h.JPList16 {
					h.Log.Printf("REQ_BARRACK_INFO: JP isolation - sending cmd 16 only")
					return out, nil
				}
				out = append(out, BuildEquipExtra())
				haveChannels := false
				if cmdr, found, err := h.Barrack.Commander(ctx, sess.CommanderID); err == nil && found {
					// ONE ENTRY PER MAP the barrack names. The client looks each entry up
					// by (mapID, channel), so a list that carries only the last point's
					// map leaves the New Game drop-down with nothing in it - and the
					// button then does nothing at all (see ChannelSet).
					saveMap, lastMap := h.barrackMaps(cmdr)
					out = append(out, BuildChannelSet(
						ChannelsForEntry(saveMap, lastMap, cmdr.ChannelID)))
					haveChannels = true
				}
				// 41/57/65 go out only behind a REAL channel list: after 35
				// means after it, so an answer whose 35 could not be built
				// stays the three-packet set it always was rather than gaining
				// a tail with no refresh in front of it.
				if haveChannels {
					out = append(out, h.barrackInfoLists(ctx, sess)...)
				}
				// ONE line per answer, built from the packets themselves: a
				// grey "Last Point" is then diagnosable from the log alone -
				// the ids say whether 35 and the three lists really went out,
				// and the byte count says how much body went with them (57 and
				// 65 keep their size even when every value in them is zero, so
				// the size is not an emptiness signal - the ids are).
				h.Log.Printf("barrack: info tail %s", tailOf(out))
				h.Log.Printf("REQ_BARRACK_INFO: rebuilt from the database (%d characters, %d bytes) + channel list",
					len(chars), len(sess.BarrackInfo))
				return out, nil
			}
			h.Log.Printf("REQ_BARRACK_INFO: could not rebuild (%v) - sending the cached copy", err)
		}
		// Answer with OUR barrack when we built one. The captured packet belongs
		// to another account, and sending it after our own made the client
		// crash (two different character lists for one session).
		if sess.BarrackInfo != nil {
			out := [][]byte{sess.BarrackInfo}
			// NO FAMILY YET: cmd 16 IS the packet that tells the client "you have a
			// barrack, make a character" (ge.exe FUN_00521a49 -> SwitchSubMode(2)),
			// and the client asks for it again right after login. The family trigger
			// therefore has to come LAST here too: it did in the start burst and was
			// overridden 4 ms later by this very answer (logs\barrack_family_test6.log,
			// cmd 14 at .970766 then cmd 16 at .974156), which is why the client still
			// landed on character creation.
			if sess.CommanderID == 0 {
				out = append(out, []byte{0x0E, 0x00}) // BC_DESTROY_COMMANDEROK -> BARRACKMODE_CREATE_COMMANDER
				h.Log.Printf("REQ_BARRACK_INFO: no family on this session - cmd 14 sent after the barrack info")
			}
			// The degraded answer says so in the same shape as the full one: with
			// no database there is no 35 and no 41/57/65, and that is exactly what
			// a reader of a grey-"Last Point" report needs to see.
			h.Log.Printf("barrack: info tail %s", tailOf(out))
			h.Log.Printf("REQ_BARRACK_INFO: sending our built barrack (%d bytes)", len(sess.BarrackInfo))
			return out, nil
		}
		h.Log.Printf("REQ_BARRACK_INFO: sending %d captured packet(s)", len(h.AfterBarrackInfo))
		return h.AfterBarrackInfo, nil
	case 22, 23: // CB_STARTGAME / CB_CONTINUEGAME - "send me to the zone"
		// The answer is BC_STARTGAMEOK (25), and without it the client sits at
		// the barrack forever: this is the packet that lets a player LEAVE.
		return h.startGame(ctx, sess, f)
	case 52: // CB_REQ_EXTEND_GROUP - the Team List's Extend button
		// 0 asks the price (answered 53), 1 buys one slot (53 -> 32 + 54). The
		// whole rule, and why a refusal sends NOTHING, is in extend_group.go.
		return h.extendTeamGroup(ctx, sess, f.Data)
	case 64: // CB_EQUIP_ITEM_PRESET_INIT - the client picking a character asks
		// "what presets does this one have", and the answer is the preset list
		// (cmd 57). It is sent once at barrack entry too, but the client asks
		// AGAIN on every character selection and retries until answered, so
		// without this case the log fills with `cmd 64: no handler yet` and the
		// panel of the character just picked stays empty.
		h.Log.Printf("EQUIP_ITEM_PRESET_INIT: answering with the preset list (%d bytes)",
			EquipPresetInfoSize)
		return [][]byte{BuildEquipPresetInfo()}, nil
	default:
		h.Log.Printf("cmd %d (%d bytes): no handler yet", f.Cmd, f.Size)
		return nil, nil
	}
}

// barrackInfoLists builds the three packets that ride AFTER the channel list in a
// barrack-info answer: the saved team groups (41), the equipment presets (57) and
// the preset-extend counts (65).
//
// WHY HERE AND NOT IN THE BURST. burstSkip lists every id the reference server does
// not send, and that includes these three. Dropping them entirely was the state this
// fixes: we had the builders and never sent them, so the team-group and preset panels
// showed whatever the client already had (ROADMAP R41). Answering op 15 is the one
// moment the client asks for this data by name, and 35 - the refresh - has already
// gone out ahead of them, so nothing here can leave "Last Point" on a state computed
// before the team existed.
//
// JP IS NOT INCLUDED. Its build shifted these ids (58/66, see jpAfterStartID), it
// gets its own copies inside the burst, and it never sends op 15; appending the 2020
// ids to a JP answer would hand it two packets it cannot place. A JP session that
// somehow reaches this path sends exactly what it sent before: 16, 17 and 35.
//
// A team-group read failure is not fatal: the list goes out EMPTY and the reason is
// logged, because an empty list and a broken table look identical to the client and
// only one of them is true.
func (h *Handler) barrackInfoLists(ctx context.Context, sess *Session) [][]byte {
	if h.JP {
		return nil
	}
	groups, err := h.Barrack.TeamGroups(ctx, sess.CommanderID)
	if err != nil {
		h.Log.Printf("REQ_BARRACK_INFO: team groups could not be read (%v) - sending an empty list", err)
		groups = nil
	}
	return [][]byte{
		BuildTeamGroupList(groups),
		BuildEquipPresetInfo(),
		BuildPresetRemainCount(PresetExtendItems()),
	}
}

// tailOf renders the command ids and the total bytes of a packet answer for the log:
// "16 17 35 41 57 65 (1234 bytes)". It reads the packets it is given, so the line
// cannot drift from what was really sent - the ids and the size ARE the evidence.
func tailOf(pkts [][]byte) string {
	var ids strings.Builder
	total := 0
	for i, p := range pkts {
		if i > 0 {
			ids.WriteByte(' ')
		}
		if len(p) < 2 {
			ids.WriteString("??")
		} else {
			fmt.Fprintf(&ids, "%d", binary.LittleEndian.Uint16(p))
		}
		total += len(p)
	}
	return fmt.Sprintf("%s (%d bytes)", ids.String(), total)
}

// startGame answers CB_STARTGAME (22) and CB_CONTINUEGAME (23) with
// BC_STARTGAMEOK (25): which zone process to connect to, and which map to load.
//
// The captured cmd 25 is never replayed: its zoneIP is the ORIGINAL server's
// public address (85.11.190.15:7054). It stays in `unpbuildable` so the burst's
// copy is dropped, and this handler is the only thing that sends cmd 25.
func (h *Handler) startGame(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	// A SUPERSEDED CONNECTION CANNOT HAND OFF (login_seq.go). This is FIRST, before the
	// team is recorded and before the character rows are touched, because the card's
	// refusal is that `pending_zone_entry` is left exactly as the newer login finds it -
	// a refusal that had already written the row would be a handoff with no answer.
	if h.supersededStartGame(ctx, sess) {
		return nil, nil
	}

	// Which team: the zone does not know, so record it where the zone reads.
	if cmd, slots, ok := ParseStartGame(f.Data); ok {
		entry := ZoneEntry{
			CommanderID: sess.CommanderID,
			Channel:     cmd,
			Slots:       slots,
			Continue:    f.Cmd == 23,
		}
		if h.Barrack == nil {
			h.Log.Printf("STARTGAME: no barrack store, so the picked team is NOT "+
				"recorded - the zone will spawn the first character only (slots %v)", slots)
		} else {
			if err := h.Barrack.SaveZoneEntry(ctx, entry); err != nil {
				// Not fatal: the zone falls back to the player's first character, so
				// a failed write costs a team, not the login. It must still be said.
				h.Log.Printf("STARTGAME: could not record the picked team (%v) - the zone "+
					"will spawn the first character only", err)
			} else {
				h.Log.Printf("STARTGAME: pending zone entry channel=%d slots=%v continue=%v",
					cmd, slots, entry.Continue)
			}
			// And PERSIST the team onto the character rows, so the next login's
			// barrack carries it back. Without this the client has no "previous
			// team", Last Point never becomes a Continue Game, and the saved
			// position is never used - which looks exactly like "nothing is saved".
			if err := h.Barrack.SaveTeamPositions(ctx, sess.CommanderID, slots); err != nil {
				h.Log.Printf("STARTGAME: could not save the team positions (%v) - the "+
					"team will not reappear at the next login", err)
			} else {
				h.Log.Printf("STARTGAME: team positions saved: %v", slots)
			}
		}
	} else {
		h.Log.Printf("STARTGAME: request is %d bytes, too short to carry the team "+
			"- the zone will spawn the first character only", f.Size)
	}

	// WHICH MAP: the player's two buttons are two different facts, and only one of
	// them is the Last Point's map.
	//
	//	CB_STARTGAME     22  "New Game"      -> the SAVED START POINT's map
	//	CB_CONTINUEGAME  23  "Last Point"    -> commander.zone_id (where they left)
	//
	// `commander.zone_id` is written on EVERY way out of a zone (zone\last_point_db.go),
	// so a log-out in Reboldoeux overwrites it whatever the player saved. Routing a New
	// Game by it is the 2026-10-29 22:31 bug: a Coimbra save point (commander.point =
	// 213, a restart warp on map 15) was sent to the map-10 zone, which cannot honour a
	// map-15 point and dropped the team on Reboldoeux's own spawn.
	//
	// saveMap is 0 when nothing is saved or the warp id resolves to no map; that is the
	// pre-save-point state and keeps the old single-map answer (saveMapOf says why).
	var zoneID uint16
	var channel byte
	newGame := f.Cmd == 22
	var saveMap, saveWarp, lastMap uint16
	if h.Barrack != nil && sess.CommanderID != 0 {
		cmdr, found, err := h.Barrack.Commander(ctx, sess.CommanderID)
		if err != nil {
			return nil, err
		}
		if found {
			zoneID, channel, lastMap = cmdr.ZoneID, cmdr.ChannelID, cmdr.ZoneID
			if newGame {
				saveWarp = cmdr.SaveWarpID
				saveMap = h.saveMapOf(cmdr)
				if saveMap != 0 {
					zoneID = saveMap
				}
			}
			// A FAMILY THAT HAS NEVER PLAYED HAS NO MAP AT ALL. zone_id 0 is not a map id,
			// so every lookup below misses and the answer used to fall through to the
			// reb_basic constant - measured 2026-11-11 on the first Start Game of a brand-new
			// family (log barrack_family_test29: "no map class name for zone 0 ... sending
			// reb_basic"). A new family starts at STARTONIA - map 18, `city_tuto` (owner's
			// rule, 2026-11-11), the same map the family card is given for the same reason.
			if zoneID == 0 {
				zoneID = newFamilyStartMap
				lastMap = newFamilyStartMap
				h.Log.Printf("STARTGAME: this family has no map yet - entry goes to STARTONIA "+
					"(map %d, city_tuto)", newFamilyStartMap)
			}
		}
	}

	// Which zone serves the map the player is saved on? A live zone's own
	// announcement beats every constant; with no announcement the old
	// flag-plus-table pair answers exactly as it did before the registry
	// existed. That fallback IS the design rule (history\world-layer.md): a
	// broken registry must degrade to "one zone on one port", never to "no
	// game".
	rec, haveZone := world.Record{}, false
	if h.Zones != nil {
		rec, haveZone = h.Zones.Address(int(zoneID), int(channel))
	}

	// ASK THE MANAGER FOR THE MAP WHOEVER THE PLAYER IS. The request path below only ran
	// for a SAVED start point, so the FIRST Start Game of a brand-new family - which has no
	// saved point and enters at Startonia, a map that is not in the warm set (the warm set
	// is the cities) - was answered with an address nothing listens on: measured 2026-11-11,
	// log barrack_family_test29 "zone 0 map reb_basic ... 127.0.0.1:7002" with no zone up.
	// cmd/zoneman starts any map on request (~1.5 s), so ask and wait here, once, before the
	// answer is built.
	if !haveZone && h.AskForZones && h.Zones != nil && zoneID != 0 {
		if err := h.Zones.Request(int(zoneID)); err == nil {
			if started, live := h.Zones.WaitFor(int(zoneID), 10*time.Second); live {
				rec, haveZone = started, true
				h.Log.Printf("STARTGAME: map %d had no live zone - the manager started it at "+
					"%s:%d", zoneID, rec.IP, rec.Port)
			} else {
				h.Log.Printf("STARTGAME: asked the manager for map %d and it did not come up in 10s", zoneID)
			}
		} else {
			h.Log.Printf("STARTGAME: map %d has no live zone and the start request failed (%v)", zoneID, err)
		}
	}

	// A NEW GAME GOES TO THE SAVE POINT'S MAP OR NOWHERE. Sending the client to a zone
	// that serves another map is not a small error: that zone refuses the point and
	// drops the team on ITS OWN spawn, which is what "my save point is in Coimbra but it
	// sends me to Rebo" was. So when a saved point names a map and no live zone serves
	// it, the answer is a REFUSAL and a log line naming the map - never the last point's
	// map. (With no registry at all there is nothing to ask; the flag below then applies,
	// and the line says so, because a missing registry must not stop every login.)
	if newGame && saveMap != 0 {
		switch {
		case h.Zones == nil:
			h.Log.Printf("STARTGAME: commander %d's SAVED START POINT is map %d (warp %d) "+
				"but no zone registry is wired in, so this build cannot tell whether a "+
				"live zone serves it - using the zone address %q instead",
				sess.CommanderID, saveMap, saveWarp, h.fallbackAddr())
		case !haveZone:
			// ASK THE MANAGER BEFORE REFUSING (2026-11-09). A map with no live zone used
			// to mean "ask the operator to start it" - the owner's "hard to test each
			// time asking u to start certain map". cmd/zoneman starts it on request, so
			// the login waits a few seconds and then goes through.
			if !h.AskForZones {
				// No manager wired in (tests, or a barrack built without one): keep the
				// old refusal path untouched.
				h.Log.Printf("STARTGAME: REFUSED - commander %d's SAVED START POINT is map %d "+
					"(warp %d) and NO LIVE ZONE serves that map, so the client is sent "+
					"nowhere. It is NOT sent to map %d (the Last Point's map): that zone "+
					"would refuse the point and put the team on its own spawn",
					sess.CommanderID, saveMap, saveWarp, lastMap)
				return nil, nil
			}
			if err := h.Zones.Request(int(saveMap)); err == nil {
				if started, live := h.Zones.WaitFor(int(saveMap), 10*time.Second); live {
					rec, haveZone = started, true
					h.Log.Printf("STARTGAME: map %d had no live zone - the manager started "+
						"it at %s:%d for commander %d", saveMap, rec.IP, rec.Port,
						sess.CommanderID)
					break
				}
				h.Log.Printf("STARTGAME: asked the manager for map %d and it did not come "+
					"up in 10s", saveMap)
			} else {
				h.Log.Printf("STARTGAME: map %d has no live zone and the start request "+
					"failed (%v)", saveMap, err)
			}
			h.Log.Printf("STARTGAME: REFUSED - commander %d's SAVED START POINT is map %d "+
				"(warp %d) and NO LIVE ZONE serves that map, so the client is sent "+
				"nowhere. It is NOT sent to map %d (the Last Point's map): that zone "+
				"would refuse the point and put the team on its own spawn",
				sess.CommanderID, saveMap, saveWarp, lastMap)
			return nil, nil
		}
	}

	var ip string
	var port uint32
	from := "the address book"
	if h.ZoneAddr != "" {
		from = "the -zone flag"
	}
	if haveZone {
		ip, port, from = rec.IP, uint32(rec.Port), "the zone registry"
	} else {
		addr := h.fallbackAddr()
		var ok bool
		ip, port, ok = ParseZoneAddr(addr)
		if !ok {
			h.Log.Printf("STARTGAME: zone address %q is malformed - falling back to %s",
				addr, FallbackAddr(h.AdvertisedIP))
			ip, port, _ = ParseZoneAddr(FallbackAddr(h.AdvertisedIP))
		}
	}

	// The map's CLASS name, three steps deep, best first: the zone that announced
	// the map knows which world it is really serving; then the shared spawn table,
	// which knows every map; then what this build was compiled with. Only the LAST
	// step is loud, because only it is a guess.
	name, known := "", false
	nameFrom := ""
	if haveZone && rec.MapName != "" {
		name, known, nameFrom = rec.MapName, true, "the zone registry"
	}
	if !known && h.MapNames != nil {
		// One name per map id, so map 15 answers "coim_basic" without a live zone
		// and without the full map table decoded.
		if n, ok := h.MapNames(zoneID); ok {
			name, known, nameFrom = n, true, "the zone points table"
		}
	}
	if !known {
		name, known = ZoneMapName(zoneID)
		nameFrom = "the built-in map table"
	}
	if !known {
		// Loud on purpose: the client loads the world NAMED here, so a wrong name
		// is a wrong map, not a cosmetic glitch.
		h.Log.Printf("STARTGAME: no map class name for zone %d in the zone points "+
			"table or the built-in map table (data\\map_table.xml has the full "+
			"table) - sending reb_basic", zoneID)
		name, nameFrom = "reb_basic", "the reb_basic constant (UNKNOWN MAP)"
	}

	// WHERE THE MAP CAME FROM, in words: the definition-of-done line for R73 is this one,
	// because it is the only place a reader can see that a New Game went to the save
	// point's map and not to the last point's.
	routedBy := "the LAST POINT's map"
	switch {
	case newGame && saveMap != 0:
		routedBy = fmt.Sprintf("the SAVED START POINT (warp %d, map %d)", saveWarp, saveMap)
	case newGame:
		routedBy = "the LAST POINT's map (no start point is saved)"
	}

	out := BuildStartGameOK(StartGameOK{
		ZoneInstID: StartGameZoneInstID,
		IP:         ip,
		Port:       port,
		MapName:    name,
		Channel:    channel,
	})
	h.Log.Printf("STARTGAME: zone %d map %q channel %d -> %s:%d from %s, map name "+
		"from %s, routed by %s (%d bytes)",
		zoneID, name, channel, ip, port, from, nameFrom, routedBy, len(out))
	return [][]byte{out}, nil
}

// login answers a CB_LOGIN with a BC_LOGINOK and remembers the commander.
//
// The password is NOT verified: the client sends a different 16-byte digest on
// every login while the database holds one static MD5. That is logged loudly on
// every login rather than hidden, because an unverified login is a security
// property, not a detail.
func (h *Handler) login(ctx context.Context, sess *Session, frame []byte) ([][]byte, error) {
	l, err := protocol.ParseLogin(frame)
	if err != nil {
		return nil, err
	}

	acc, ok, err := h.Store.Account(ctx, l.AccountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		h.Log.Printf("LOGIN id=%q nation=%q: no such account - no reply sent", l.AccountID, l.Nation)
		return nil, nil
	}

	sess.AccountID = acc.AccountID
	sess.CommanderID = acc.CommanderID
	// THIS login takes the account's next sequence (login_seq.go). Done here and not in
	// Serve, because this is the moment the account is known and the only place a login
	// is accepted - the sequence is the socket's identity, so it is stamped by the same
	// reply that identifies the player.
	h.stampLoginSeq(ctx, sess)

	reply := protocol.BuildLoginOK(acc.AccountID, newSessionID(), l.AccountID)
	if h.JP {
		// The JP 375639 client's table says BC_LOGINOK is 61 bytes, not 57;
		// a short packet leaves four bytes of cmd 57 to be misread.
		reply = protocol.BuildLoginOKJP(acc.AccountID, newSessionID(), l.AccountID)
	}
	if h.LoginOKCapture != nil {
		reply = h.LoginOKCapture
		h.Log.Printf("=== PROBE: sending the CAPTURE's login answer (ids will not match " +
			"data built from the database) ===")
	}
	sess.MemberID = l.AccountID
	if h.RecordLogin != nil {
		h.RecordLogin(ctx, sess.MemberID, sess.RemoteIP)
	}
	h.Log.Printf("LOGIN id=%q -> account %d commander %d, %d-byte LOGINOK "+
		"(PASSWORD NOT VERIFIED: the client's digest cannot be checked)",
		l.AccountID, acc.AccountID, acc.CommanderID, len(reply))

	out := make([][]byte, 0, 1+len(h.AfterLogin))
	out = append(out, reply)
	out = append(out, h.AfterLogin...)
	return out, nil
}

// startBarrack answers CB_STARTBARRACK.
//
// The captured burst is the reference for ORDER, but four of its packets
// describe the capture's own character. When a barrack store is configured those
// four are BUILT for the commander that actually logged in and substituted in
// place, so the sequence keeps its shape while the data becomes ours.
func (h *Handler) startBarrack(ctx context.Context, sess *Session) ([][]byte, error) {
	// THE VALUES THE BURST IS BUILT FROM. For an account with NO family they are the empty
	// ones, and the burst still goes out in full: the client waits for the WHOLE
	// start-barrack sequence - and for the capture-derived ordering (the hotkey table, the
	// job list nine times) that our packet set is merged into. Sending only cmd 10 hung the
	// client exactly like sending nothing did (2026-11-11, measured on the owner's Andro
	// client).
	var (
		cmdr             CommanderRow
		chars            []CharInfo
		groups           []TeamGroup
		saveMap, lastMap uint16
	)
	if h.Barrack == nil || sess.CommanderID == 0 {
		// NO FAMILY: the live server's own answer, replicated. Measured 2026-11-11 on the
		// owner's client against the live server (captures\v42-nofamily\20261111-143143):
		// for an account with no commander the server sends NO commander info and NO barrack
		// info at all - it sends a system message saying the commander failed to load, and
		// that message is what puts the client on an EMPTY BARRACK with the create-family
		// dialog open (owner's observation of the same session).
		//
		// Everything this server tried before that came from guesswork - an empty cmd 10, a
		// bare cmd 14, the whole burst - and each one either sent the client to the character
		// wizard or hung it. cmd 3 is the packet the real server uses, so it is the one sent.
		h.Log.Printf("STARTBARRACK: account %d has no family - sending the no-commander "+
			"BC_MESSAGE (cmd 3), as the live server does", sess.AccountID)
		return [][]byte{BuildNoCommanderMessage(sess.MemberID)}, nil
	} else {
		var ok bool
		var err error
		cmdr, ok, err = h.Barrack.Commander(ctx, sess.CommanderID)
		if err != nil {
			return nil, err
		}
		if !ok {
			h.Log.Printf("STARTBARRACK: commander %d is not in the database - replaying the capture",
				sess.CommanderID)
			return h.AfterStartBarrack, nil
		}
		chars, err = h.Barrack.Characters(ctx, sess.CommanderID)
		if err != nil {
			return nil, err
		}
		// The saved team groups. A failure here must not cost the player the
		// barrack, but it must be SAID: an empty list and a broken table look
		// identical to the client, and only one of them is true.
		groups, err = h.Barrack.TeamGroups(ctx, sess.CommanderID)
		if err != nil {
			h.Log.Printf("STARTBARRACK: team groups could not be read (%v) - sending an empty list", err)
			groups = nil
		}
		// The two maps this barrack names, resolved ONCE so cmd 10 and cmd 35 cannot
		// disagree. saveMap is the SAVED start point's map (cmd 10's newMapID, what New
		// Game enters at); lastMap is the LAST point's map (cmd 10's contMapID, what
		// Continue resumes). They are the same number when nothing is saved.
		saveMap, lastMap = h.barrackMaps(cmdr)
	}

	built := map[uint16][]byte{
		// RANK IS THE CAP, NOT THE COUNT. The client's Extend guard reads cmd 10's
		// rank at +296 and sends cmd 52 only when `IES("Lock").Lock_TeamGroup <
		// rank` (PROVEN, client 0x005426c1; history\barrack.md "2026-11-08 - the
		// Extend gate"). Filling rank with the commander's own count made that test
		// `n < n` - false for every n - so the button never sent a packet, at 20 and
		// at 5 alike. The LIVE server's rank is 20 for a commander whose Lock table
		// says 13 (captures\barrack201-20261027) and 7 (captures\live-20261021), so
		// rank is GET_TEAMGROUP_MAXSLOT(); the reference sets it the same way
		// (`bcPacket->rank = GetBarrackServer()->GetMaxTeamGroupCount()`,
		// src\ge_barrack_server.py:494). cmd 32 below keeps the CURRENT count -
		// that is the number this one is compared against, and the only number the
		// client's AddBarrackTeamGroup enforces.
		10: func() []byte {
			ci := CommanderInfo{
				Name: cmdr.Name, Rank: TeamGroupMaxSlot, CharSlot: slotCount(cmdr.CharSlot, len(chars)),
				SaveMapID: saveMap, LastMapID: lastMap,
				BarrackNames: cmdr.BarrackNames,
			}
			if h.JP {
				return BuildCommanderInfoChannels(ci, 32)
			}
			return BuildCommanderInfo(ci)
		}(),
		16: func() []byte {
			if h.JP {
				return BuildBarrackInfoJP(chars)
			}
			return BuildBarrackInfo(chars)
		}(),
		17: BuildEquipExtra(),
		26: BuildEmptyItemList(),
		// The family level is COMPUTED from the characters' levels
		// (CCommander::ComputeCommanderLv), never read from a column. Stored
		// level 0 here means "no stored level yet", so the computed one is used.
		37: BuildCommanderLvInfo(chars, 0),
		// The saved team groups. Empty for a player who has none - which is the
		// truth and also clears any list the client is still showing.
		41: BuildTeamGroupList(groups),
		// The channel list. NOT cosmetic: an empty one greys out New Game and
		// Continue Game (GEBarrackMode.cpp:465), so it must report the channel we
		// actually run, on BOTH map ids cmd 10 carries - one entry per map. An entry
		// missing for one of the two maps leaves that drop-down empty and its button
		// dead (ChannelSet explains why), and it is the shape that crashed the client
		// on 2026-10-29 21:31:20.
		35: BuildChannelSet(ChannelsForEntry(saveMap, lastMap, cmdr.ChannelID)),
		// The Lock property table. This was dropped for the whole port and it is the
		// packet that carries Lock_BarrackLv / Lock_MCCLv / Lock_CharSlot - the values
		// the barrack UI reads through imcIES. Measured: with the Python barrack
		// (which sends it) "Last Point" is clickable; with ours (which did not) it is
		// disabled, every other packet being equal.
		//
		// Lock_TeamGroup is replaced with THIS commander's CURRENT count: the
		// embedded table carries the server-side default (5), and a commander who
		// extended to 6+ would be told 5 on the next login - the client would then
		// refuse the 6th saved team itself (script.cpp:4798).
		//
		// THE SOURCE IS `commander_property.Lock_TeamGroup` (cmdr.TeamMax), NOT the
		// number of `team_group` rows and NOT cmd 10's rank. The three are different
		// facts: rank is the CAP (see the 10: arm above), this is the count of groups
		// the commander is currently allowed, and the `team_group` rows are only the
		// teams he has SAVED - this database holds Lock_TeamGroup 5 with 0 saved
		// teams, so a row count here would advertise 0 and close the list entirely.
		// The client compares this value against rank (script.cpp:4776) and enforces
		// it when adding a team (:4798); the Extend handler raises exactly this row
		// (barrack_store.go, ExtendTeamGroup). The live captures carry 13 and 7 here
		// for a commander whose rank was 20, which is the shape this keeps.
		32: LockTableFor(cmdr.TeamMax),
	}
	if h.JP {
		// The JP build shifted these two: preset info is 58 (1,154 bytes for 32
		// channels) and preset remain is 66 (variable). See data\cmd_table_barrack_jp.txt.
		built[58] = BuildEquipPresetInfoJP()
		built[66] = BuildPresetRemainCountJP(PresetExtendItems())
	} else {
		built[57] = BuildEquipPresetInfo()
		built[65] = BuildPresetRemainCount(PresetExtendItems())
	}
	// The client asks for the barrack info AGAIN after the burst. Answer with
	// the packet we built, not the captured one: two different character lists
	// in one session is a crash, and the captured list is another account's.
	sess.BarrackInfo = built[16]

	// THE JP BUILD SHIFTED THE BARRACK IDS AFTER 55 BY +1. Its table
	// (data\cmd_table_barrack_jp.txt, dumped from the running client) says the
	// rotation list is 67, preset info is 58, preset remain is 66 and end second
	// password is 73; 56 is CB_COMMANDER_TRANSFER (client -> server) and the
	// 7,571-byte hotkey table is 57. Remap the captured burst for the JP client
	// and leave every other build's bytes alone.
	src := h.AfterStartBarrack
	if h.JP {
		src = make([][]byte, len(h.AfterStartBarrack))
		for i, p := range h.AfterStartBarrack {
			src[i] = remapJPPacket(p)
		}
		h.Log.Printf("STARTBARRACK: JP 375639 remap (rotation 67, preset 58/66, " +
			"end-second-password 73, 880-byte cmd 10)")
	}

	out := make([][]byte, 0, len(src)+len(built))
	replaced, dropped := 0, 0
	seenRotation := 0
	rotationCmd := uint16(66)
	if h.JP {
		rotationCmd = 67
	}
	for _, p := range src {
		cmd := binary.LittleEndian.Uint16(p)
		// The reference server sends the job list NINE times, byte-identical
		// (JOB_LIST_REPEATS in src\ge_barrack_server.py) and BEFORE the commander
		// info; its notes say the count and the position are load-bearing (one copy
		// crashes the client). Our capture has ten, so the extra is dropped.
		if cmd == rotationCmd {
			seenRotation++
			if seenRotation > 9 {
				dropped++
				continue
			}
		}
		// Checked BEFORE the substitution map: a packet we CAN build would otherwise
		// be replaced instead of skipped, and the packet set would not match the
		// reference server.
		if burstSkip[cmd] {
			// THE JP CLIENT DOES NOT ASK. It takes the barrack-info set in the
			// burst; op 15 never arrives, so skipping these leaves it on the
			// loading screen (measured 2026-11-06). Send the built ones here.
			if h.JP {
				if b, ok := built[cmd]; ok {
					out = append(out, b)
					replaced++
					continue
				}
			}
			dropped++
			continue
		}
		if b, ok := built[cmd]; ok {
			out = append(out, b)
			replaced++
			continue
		}
		// These carry ANOTHER ACCOUNT's values and we cannot build them yet, so
		// they are dropped rather than replayed. Everything left here needs a
		// builder of its own.
		if unpbuildable[cmd] {
			dropped++
			continue
		}
		out = append(out, p)
	}
	h.Log.Printf("STARTBARRACK: built cmd 10/16/17/26/35/37/41/57/65 for commander %d %q (%d characters, "+
		"level and family level from exp, %d team group(s)); %d captured kept, "+
		"%d per-player packet(s) dropped",
		cmdr.ID, cmdr.Name, len(chars), len(groups), len(out)-replaced, dropped)

	// THE JP CLIENT WANTS ONLY THE COMMANDER INFO NOW. Everything else waits
	// for its CB_REQ_BARRACK_INFO, exactly like the later v42 client. The full
	// set in the burst AND again on op 15 is what crashed it (2026-11-06).
	if h.JP {
		sess.JPBarrackSet = make([][]byte, 0, len(out))
		for _, p := range out {
			if len(p) >= 2 && binary.LittleEndian.Uint16(p) == 10 {
				continue
			}
			sess.JPBarrackSet = append(sess.JPBarrackSet, p)
		}
		h.Log.Printf("STARTBARRACK: JP sends cmd 10 only now; %d packet(s) deferred to CB_REQ_BARRACK_INFO",
			len(sess.JPBarrackSet))
		return [][]byte{built[10]}, nil
	}

	// (a family-less session returns the no-commander message from the top of this function)

	return out, nil
}

// unpbuildable lists captured packets that reference the OTHER account's
// characters, items or slots. They are dropped while we build our own barrack:
// mixing them with our 12 characters is an inconsistency the client can act on.
//
// Only genuinely static packets stay: cmd 66 (the job list, byte-identical in
// every capture) and cmd 70. Everything here needs a builder of its own.
var unpbuildable = map[uint16]bool{
	25: true, // BC_STARTGAMEOK-of-barrack? per-session values (answered by startGame)
}

// burstSkip lists captured packets the barrack does NOT send.
//
// The list is not a leftover debug switch: it is the packet set the WORKING
// reference server (src\ge_barrack_server.py) sends, which is smaller than the
// captured burst. Sending the captures on top of our built packets is what kept
// "Last Point" disabled for a whole evening - see history\zone-entry.md.
//
// Two separate reasons:
//
//   - 16, 17, 35 belong to the ANSWER to the barrack-info request (cmd 15), in that
//     order: 16 rebuilds the character list, 17 the worn equipment, and 35 is the
//     refresh - its handler ends in UpdateTeamButton(), which is what decides whether
//     "Last Point" can be clicked. Sending them in the burst as well produced a
//     duplicate barrack rebuild AFTER the refresh, and the button kept the state
//     computed before the team existed.
//   - 26 and 70 are lists nobody needs to enter the game, and the working server
//     never sends them either.
//   - 41, 57, 65 are ALSO never sent by the working server, so the burst must not
//     carry them (their captured copies are another account's). They have builders
//     and they DO go out now: in the ANSWER to op 15, after 35, once -
//     handler.Reply case 15 -> barrackInfoLists. Keeping them in this map is what
//     makes that "once": the burst drops them, the answer adds them.
//
// A grey "Last Point" is diagnosable from the tail line the answer writes,
// "barrack: info tail 16 17 35 41 57 65 (<n> bytes)": it quotes the ids and the bytes
// that actually went out.
var burstSkip = map[uint16]bool{
	16: true,
	17: true,
	35: true,
	26: true,
	41: true,
	57: true,
	65: true,
	70: true,
}

// jpAfterStartID is the JP 375639 id shift for the captured STARTBARRACK burst.
// The JP build inserted CB_COMMANDER_TRANSFER at 56 and moved every barrack id
// after 55 up by one; the sizes table is data\cmd_table_barrack_jp.txt.
var jpAfterStartID = map[uint16]uint16{
	66: 67, // BC_ROTATION_CHAR_LIST (same 122-byte body)
	57: 58, // BC_ALL_EQUIP_ITEM_PRESET_INFO (JP is 1,154 bytes, rebuilt)
	65: 66, // BC_EQUIP_ITEM_PRESET_ADD_ITEM_REMAIN_COUNT (variable, rebuilt)
	70: 73, // BC_END_SECOND_PASSWORD (same 3-byte body)
}

// remapJPPacket returns a copy of p with the command word shifted for the JP
// client, or p itself when the id did not move.
func remapJPPacket(p []byte) []byte {
	if len(p) < 2 {
		return p
	}
	cmd := binary.LittleEndian.Uint16(p)
	to, ok := jpAfterStartID[cmd]
	if !ok {
		return p
	}
	q := append([]byte(nil), p...)
	binary.LittleEndian.PutUint16(q, to)
	return q
}

// slotCount is the character-slot count sent in BC_COMMANDER_INFO.
//
// The client allocates from this value, so it must NEVER be smaller than the
// number of records we send: 12 characters with a slot count of 4 is an
// out-of-bounds write, and this project has been crashed by exactly that shape
// before (a wrong header made the client read a huge slot id). The database's
// Lock_CharSlot is preferred when it is big enough.
func slotCount(declared uint16, characters int) uint16 {
	if n := uint16(characters); n > declared {
		return n
	}
	return declared
}

// Serve reads until the client closes, logging every chunk and packet, and
// answers the ones that have a handler.
func (h *Handler) Serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// A panic in ONE connection must not kill the barrack. main runs Serve in a
	// goroutine, and an unrecovered panic in any goroutine ends the process: the
	// client disconnects and the log just stops. The stack goes through the
	// logger, because a plain panic writes to stderr and not to the -log FILE.
	defer func() {
		if r := recover(); r != nil {
			lg := h.Log
			if lg == nil {
				lg = log.Default()
			}
			lg.Printf("PANIC while serving %s: %v\n%s", conn.RemoteAddr(), r, debug.Stack())
		}
	}()

	h.Log.Printf("CONNECT from %s", conn.RemoteAddr())

	sess := &Session{RemoteIP: remoteHost(conn.RemoteAddr())}
	// GE_LOG.ACCOUNT_LOGOUT, written once however this connection ends (EOF, a read error,
	// a panic above). A session that never logged in has no MemberID and writes nothing.
	defer func() {
		if sess.MemberID != "" && h.RecordLogout != nil {
			h.RecordLogout(context.Background(), sess.MemberID, sess.RemoteIP)
		}
	}()
	var buf []byte
	read := make([]byte, 64*1024)
	chunkNo := 0

	for {
		n, err := conn.Read(read)
		if n > 0 {
			buf = append(buf, read[:n]...)
			chunks, used := protocol.SplitWire(buf)
			buf = buf[used:]

			for _, ch := range chunks {
				plain, derr := h.Cipher.DecryptChunk(ch.Data)
				if derr != nil {
					h.Log.Printf("chunk %d cipher=%d: cannot decrypt: %v", chunkNo, ch.CipherLen(), derr)
					chunkNo++
					continue
				}
				frames, _ := h.Table.Split(plain)
				h.Log.Printf("chunk %d cipher=%d plain=%d packets=%d",
					chunkNo, ch.CipherLen(), len(plain), len(frames))

				for _, f := range frames {
					out, rerr := h.Reply(ctx, sess, f)
					if rerr != nil {
						h.Log.Printf("  cmd %d: %v", f.Cmd, rerr)
						continue
					}
					for _, packet := range out {
						// SERVER -> CLIENT IS PLAINTEXT. No encryption, no
						// length word: packets are written back to back and the
						// client frames them by the command table.
						//
						// Proven by data\captures\live-20261021\s2c_7001_49167.bin:
						// its first word is command 8 and packet 2 starts at
						// offset 57, so nothing wraps them. Encrypting here made
						// the client ignore every reply.
						if _, werr := conn.Write(packet); werr != nil {
							h.Log.Printf("  cmd %d: write failed: %v", f.Cmd, werr)
							return
						}
						h.Log.Printf("  SENT cmd %d, %d plaintext bytes",
							binary.LittleEndian.Uint16(packet), len(packet))
					}
				}
				chunkNo++
			}
			if len(buf) > 0 {
				h.Log.Printf("held %d byte(s) of an unfinished chunk", len(buf))
			}
		}
		if err != nil {
			h.Log.Printf("CLOSE %v", err)
			return
		}
	}
}

// newSessionID is the value sent in the login reply.
//
// It is FIXED on purpose: the working Python server always sends 0x00003333 and
// the real client accepts it. A random value is untested against the client and
// is the kind of gratuitous difference that produces "the client just sits
// there". Make it configurable only when there is evidence the client cares.
// remoteHost is the bare address of a connection: "1.2.3.4:5678" -> "1.2.3.4". The usage
// record keeps the address the way GE_LOG.ACCOUNT_LOGIN does (CONN_IP), not the port.
func remoteHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func newSessionID() uint32 { return 0x00003333 }
