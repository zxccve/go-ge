// Package zone is the Go zone server, in its first slice.
//
// WHAT IT DOES TODAY: accepts a client, decrypts CZ_START_GAME (3590), answers
// ZC_START_GAMEOK (3591) with one START_INFO per character, and on CZ_GAME_READY
// (3592) sends ZC_ENTER_LEGACY_OBJECTS (3149) - the packet without which the
// client stays on the "moving to <map>" screen forever.
//
// WHAT IT DOES NOT DO YET: movement echo, NPC spawns, inventory/quest panels,
// scripts. Those are logged as skipped rather than faked, because a packet that
// looks right but carries wrong data is worse than a missing one - it hides a bug
// behind a working client.
package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"geserver/item"
	"geserver/protocol"
	"geserver/text"
	"geserver/world"
)

// Command ids. The zone's own table lives in data\cmd_table_live_z.txt; these
// are the only ones this slice touches, cross-checked against
// src\zone_server.py (CMD_* constants) and that table.
const (
	CmdCZStartGame       = 3590 // client -> server: enter the zone
	CmdZCStartGameOK     = 3591 // server -> client: the team, one START_INFO each
	CmdCZGameReady       = 3592 // client -> server: world loaded
	CmdZCEnterLegacyObjs = 3149 // server -> client: leaves the loading screen

	// The shop transactions. Their layouts do NOT follow the plain "header then
	// records" shape (see shop_trade.go): one unexplained FLAG BYTE sits between
	// the count and the first record, and reading 13-byte records instead of
	// 12-byte ones parses a one-item cart correctly and everything after the first
	// item wrongly (DECISIONS.md, 2026-10-17: six items, len 93 = 20+1+6*12).
	CmdCZBuyItem  = 3672 // client -> server: a normal-shop cart
	CmdCZSellItem = 3673 // client -> server: the rows the player is selling

	// CmdCZChat is a chat line, and it is ALSO how the client's toolbar "Feso
	// Shop" button asks for its window: the button sends no shop packet at all,
	// it calls Chat("/fesoshop"). See Handler.chat.
	CmdCZChat = 3888 // client -> server: BalloonColor, ChatType, then the text
)

// CZ_CHAT's text starts here: cmd(2) seq(4) checksum(4) len@+10(2)
// BalloonColor(4) ChatType(1), so the first character sits at +17. Both a retail
// capture (data\captures\live-20261021, chunk 2162) and our own zone log agree on
// a 27-byte packet ending in "/fesoshop\0".
const chatTextOffset = 17

// What the Feso Shop button asks for, and the shop group it opens. The group is
// a plain name: the client resolves the stock from its own table.
const (
	fesoShopChat  = "/fesoshop"
	fesoShopGroup = "FESO_ITEMSHOP"
)

// Character is what the zone needs to describe one team member. It is
// deliberately a plain struct: the builder is testable without a database.
type Character struct {
	Handle   uint32 // the 32-bit handle the client uses for this character
	Name     string
	Gender   uint8
	Job      uint32
	HeadType uint8
	Level    uint8

	// Promotion is the character's promotion level, already capped by the game's
	// own band for its level - min(PromotionStep, ge_tables.promotion_band), the
	// rule barrack.PromotionBand applies and the barrack packet already carries.
	// It is an INPUT to the status script, not decoration: SCR_Get_MSP adds
	// promotionLv*10 and SCR_Get_RHP / SCR_Get_RSP read it too, so a zero must
	// mean "this character is not promoted", never "this server has no promotions".
	Promotion uint8
	// PromotionStep is the RAW `character_property` step, before the level band
	// caps it. The client is told BOTH: ZC_PROMOTION_INFO (3093) carries the
	// effective level and this raw step as its `maxStep`, exactly as the reference
	// fills level=GetPromotionLv() and maxStep=GetPromotionStep().
	PromotionStep uint8
	// CashPromotion is the raw CASH promotion step (uncapped), read by the script
	// through GetCashPromotionLevel. Both values come from `character_property`
	// (see PGStore.loadPromotion).
	CashPromotion uint8

	HP    float64
	MaxHP float64
	SP    uint16
	MaxSP uint16
	Exp   int64
	// Position is WORLDPOS: x, y, and the navmesh CELL - not a height.
	Position [3]int32

	// Equip is the item class worn in each of the 21 slots, filled by
	// PGStore.TeamOf from `wear_item` (zone\store.go loadWorn); zero means "nothing
	// worn at this spot" and is replaced by EquipNone when the record is built.
	Equip [equipSpotLast]uint32

	// Stances and Skills are the packed blocks that follow the 485-byte record.
	// They are NOT optional: without them the client crashes a millisecond after
	// the packet (measured 2026-10-25, age_dll_crash.log), because it picks its
	// default stance from this list. Built by StanceTable from the game's own XML.
	Stances []byte // STANCE_INFO records, 14 bytes each
	Skills  []byte // SKILL_INFO records, 10 bytes each

	// EquipOptions is the option records the spawn (3520) carries after its fixed
	// part: one per option on a WORN item, in ascending Spot order. The live
	// server's spawn is 1017 or 1101 bytes for exactly this reason (26 and 33
	// options), not because of any stance or skill list - see
	// BuildSpawnStandingPC.
	//
	// NOTHING FILLS THIS YET, honestly: the item layer has no option rows. The
	// PostgreSQL ge_data database holds `wear_item` and `item_reinforce` and no
	// option table at all, so in production the count stays 0 and the packet stays
	// 705 bytes. The field exists so a caller that DOES have options puts them on
	// the wire, and so the tail is not re-guessed the next time the live sizes are
	// compared with ours.
	EquipOptions []EquipOption

	// Stance is the id of the stance the CLIENT itself stands in, resolved once at
	// START_GAME from the game's own tables - see StanceTable.PickStance, which
	// matches the stance condition to what this character is HOLDING. It is what a
	// remote player's spawn carries at +30: without it the other client has no stance
	// and animates a bare-handed character as if it were running. 0 means "not known"
	// (no stance tables, or a job with no usable condition).
	Stance uint32

	// AGIBonus is the item-derived AGI - the `GetAGI_BM + GetAGI_WP` pair the live
	// 3684 carries in its BUFF_MODIFIER block (+71). `data\item_stats.json` has no
	// AGI columns yet, so every character loads it as 0 and the sheet reads base
	// AGI alone. zone\status.go's CSPD rule (sheetCSPD) is the only reader.
	AGIBonus float64
}

// EquipOption is one worn-item option as the spawn carries it: EQUIP_ITEM_OPTION
// (source2\geShared\include\geItemTypes.h), 12 bytes on the wire:
//
//	+0 type(4)  +4 value(4)  +8 spot(2)  +10 two zero bytes
//
// The struct is PACKED, so the two padding bytes come after spot and are written
// as zero rather than left to a Go zero value of a larger field. spot is the
// EQUIP_SPOT_* slot the option belongs to (1 BODY, 3 NECK, 5 RHAND, 8 GLOVE,
// 9 BOOTS, 12 EAR, 13 BELT are the ones the 2026-10-28 live capture shows).
type EquipOption struct {
	Type  uint32
	Value uint32
	Spot  uint16
}

// Session is one connection's state.
type Session struct {
	// CloseAfterAnswer makes the read loop END this session once the answer to the current
	// frame has been written. Only "To Barrack" sets it (zone\sysmenu.go): the client's own
	// CCmdGoBarrack has already begun its 5 s countdown and SetSwitchTarget(MST_SWITCH_BARRACK),
	// and the reference then destroys the commander (RequestDestroy(true), Commander.cpp:767) -
	// the socket closing is what lets the client finish. Without the close the client sits on
	// a session the server still believes in.
	CloseAfterAnswer bool
	// liveChars are the character handles this session CLAIMED at entry (zone\live.go). They
	// are what a second login displaces, and what forgetLiveSession releases on close.
	liveChars []uint32

	// Conn is the socket this session's client is on. It is written to by ONE
	// goroutine only - this session's own writer (zone\writer.go) - whatever
	// goroutine supplied the packet. Fan-out from another player's session is an
	// enqueue, never a write.
	Conn net.Conn

	// wmu is kept for the one path that has no writer: a Session built by hand
	// (a test) writes inline under this mutex, exactly as every write did before
	// zone\writer.go existed. With a writer running nothing takes it.
	wmu sync.Mutex

	// THE WRITER. outq is the queue of batches waiting to go out, outStop closes to
	// end the goroutine that drains it, and outBytes/outPkts are the same queue
	// measured two ways so the bound can be checked without asking the channel.
	// outOverflowed records that this session was closed for not reading, so the
	// close, the count and the line happen once. See zone\writer.go for the whole
	// mechanism, including why the order on the wire is still the order in the queue.
	//
	// simLatency belongs to `-simlatency`, a DEVELOPMENT FLAG: when it is above 0
	// the session's writer holds each packet back until `sentAt + simLatency`. It is
	// a delay INSIDE this writer - there is no second queue. At 0 (the only normal
	// value) the writer writes as soon as it has something.
	outq          chan *outBatch
	outStop       chan struct{}
	outStopOnce   sync.Once
	outBytes      atomic.Int64
	outPkts       atomic.Int64
	outLog        *log.Logger
	outLimited    func(string)
	outOverflowed atomic.Bool
	outDead       atomic.Bool
	simLatency    time.Duration

	AccountID uint32
	SessionID uint32
	LoginID   string
	Team      []Character

	// Control is the team slot of the character the PLAYER DRIVES right now, not
	// "the first character of the team" (zone\control.go). 0 until the client says
	// otherwise, which is also the answer for a session that never speaks: before
	// the client sent CZ_SELECTCHANGE_PC (3652) the server assumed slot 0, and that
	// assumption stays the default.
	//
	// WHICH SENSE IS WHICH. A bare `idx == 0` that means "slot 0, the team's first
	// character" - array indexing, a roster position - is still 0 and must stay
	// 0. A test that means "the character the player is driving" asks this field.
	//
	// Why it exists: the owner clicked his SECOND character as leader and the FIRST
	// one moved (2026-11-01). Slots 1..n are followers the client drags along in
	// formation, so their move reports are not the player's intent - and the
	// session's one walk segment (moveFrom/moveAt/Pos), the speed sample, LastMoveAt
	// and the chase all belong to whichever character that is.
	Control int

	// CommanderID is the account's commander, resolved once at START_GAME. A
	// script transaction writes to a COMMANDER, so the click needs it; 0 means
	// "not resolved", and a transaction then refuses to write instead of writing
	// to nobody.
	CommanderID int32

	// GmLevel is the member's operator tier, read ONCE at START_GAME from
	// `account_level` by the LoginID the client sent. 1 is the GM tier the shipped
	// server reads; 0 - the zero value, and the answer for an absent row - means
	// the session may not dispatch a command. It is deliberately NOT read per chat
	// line (zone\gm.go).
	GmLevel int

	// LastPos and LastMover are the most recent movement report, kept so a later
	// slice can save the position (CONTINUEGAME) and tell other players to stop
	// walking when this one arrives.
	LastPos   [3]int32
	LastMover uint32

	// Pos is where the LEADER stands, as x, y, cell. It starts at the spawn and
	// follows the move reports. The click range is measured from here: only the
	// leader clicks, and the client reports no position while it walks a move the
	// server sent, so a stale value is the best this side ever has.
	Pos [3]int32

	// moveFrom / moveAt / Pos describe the leg the CONTROLLED character's own last move
	// report opened: where the body stood when the report arrived, when it arrived, and
	// the destination it named. CZ_MOVE_PC carries the DESTINATION (geZCPacket.h:1133,
	// PKS_CZ_MOVE_PC), so the report says "walk to here": moveFrom is the model's own live
	// point at that instant (never a point inferred from the report) and moveAt is when it
	// arrived, which is what lets walkedPos (gate.go) answer "where is the character NOW" -
	// the gate must fire on the arrival, not on the click.
	moveFrom [3]int32
	moveAt   time.Time

	// moveRoute is the planned polyline that leg walks: first point moveFrom, last point
	// Pos (move.go, Handler.clientRoute). Empty means the leg walks the straight chord -
	// no path engine, no route, or a route that does not describe this leg. It is planned
	// ONCE, where the leg opens, and never per tick.
	moveRoute [][3]int32

	// walks is the walk each character's OWN move stream opened, parallel to Team
	// and written only by movePC. It is separate from moveFrom/moveAt above, which
	// describe the LEADER for the gate and the chase: the other two characters walk
	// too, and the far client animates each of them from its own ZC_MOVE_PC.
	//
	// It exists for ONE reason: the other players' clients only leave the move
	// state when something stops the actor (gemove::ActorStop does `Stop();
	// SetPosition(pos)` - geClientShared\source\GeMove.cpp:8-15, reached from the
	// ZC_STOP_PC handler), and this side never sent that for a walk the PLAYER
	// drove. Measured live 2026-10-28: the other player ran in place for ever after
	// stopping. See endWalks in move.go.
	walks []charWalk

	// walksSent is the last SERVER-driven walk this zone put on the wire for each
	// character, parallel to Team and written only by legMoveFor (zone\walk_once.go).
	// It is what stops the same walk being sent twice: every ZC_MOVE_PC restarts the
	// client's interpolation of that actor, so a duplicate re-anchors the body
	// mid-stride. A zero entry means that character has not been walked by us yet.
	walksSent []walkSent

	// keptLegs is the leg each character's body WALKED OUT on, remembered after the walk
	// record is dropped from Walk (zone\attack_position.go, R111). It is what gives the
	// swing after an approach the walk's own clock instead of the point an old move
	// report declared - the 841 ms regression. Parallel to Team, grown on demand.
	keptLegs []walkedLeg

	// swingMarks is the last body point each character's swing reported with the leg it
	// was read from - the once-per-leg alarm that says a reading ran BACKWARDS along a
	// leg (zone\attack_position.go, legDoesNotRewind). Parallel to Team.
	swingMarks []swingMark

	// attackAt is when each character last landed a normal attack, parallel to Team
	// and read/written only by the attack path (zone\attack.go). It is the one
	// piece of state the cadence needs: the reference's own FSM keeps exactly this
	// (`m_nextAttackTime`, FieldChar_FSM.cpp:135) so that a second CZ_ATTACK inside
	// the character's own ASPD cannot become a second hit.
	//
	// A zero time means "has not attacked yet", and a missing index is the same
	// thing - see lastAttack/noteAttack, which grow it on demand rather than
	// assuming the team was spawned before the first click.
	attackAt []time.Time

	// swings is the running normal-attack repeat of each character, one slot per team
	// slot and never a queue (zone\attack_swing.go, R94). The client sends ONE 3666 per
	// target lock - its tactics state attacks on ENTRY only - and the reference server
	// keeps swinging at the character's own ASPD from that one request
	// (FieldChar_FSM.cpp:121-173). A slot is replaced on every hit, so a client that
	// re-sends 3666 for the target it already holds can neither start a second rhythm
	// nor double-hit.
	//
	// Written and read only by the connection's own goroutine, like casts: the press
	// arms it, the tick in chaseTick advances it, and a move / stance change / cast /
	// disconnect ends it.
	swings []*swingLoop

	// presses is the HELD PRESS of each character, one slot per team slot and never a
	// queue (zone\attack.go, holdPress). It is what a CZ_ATTACK that arrived inside the
	// character's own attack delay leaves behind: the reference does not throw such a
	// press away - DispatchAttack returns BEFORE `m_lastRequest->type = REQ_NONE` while
	// the delay runs (FieldChar.cpp:955-982), ProcessRequest re-runs the stored request
	// every tick (PlayerChar.cpp:265), and the swing therefore fires the moment the
	// delay ends. This zone used to DROP it, which is why a slow stance (the warlock's
	// 2.244 s) lost almost every press.
	//
	// ONE SLOT PER CHARACTER, NEWEST WINS: any newer decision for that character - a
	// press, a move order, a click, a stance change, a cast - replaces or clears it
	// (clearPress), and it never survives the engagement ending.
	//
	// Written and read only by the connection's own goroutine, like swings and casts.
	presses []*heldPress

	// Gate is the map change this session has started, or nil. It is set when
	// ZC_MOVE_ZONE (3593) goes out and cleared when its answer (3594) has been
	// sent - the two halves of one handshake, so a second gate cannot start while
	// the first is running (see gate.go).
	Gate *gateMove

	// gateInside is the gate the leader was last standing in, as index+1 (0 = none).
	// THE reason a gate fires once per entry: the check runs on every tick, and
	// without this the same gate would fire again on the next one. It is cleared
	// only when the leader walks out of that gate's range, so leaving and coming
	// back fires again.
	gateInside int

	// warpInside is the warp point this client has been TOLD it stands in (the same key
	// space as gateInside, 0 = none): set when ZC_ENTER_WARPPOINT (3541) goes out, cleared
	// when ZC_LEAVE_WARPPOINT (3542) goes out. It is a separate record from gateInside on
	// purpose - the zone arms a gate on arrival WITHOUT sending 3541 (a player who lands
	// inside a doorway must not be told they walked into it), and then there is no enter
	// notice for the leave notice to pair with.
	warpInside int

	// DialogHandle is the NPC whose dialog box is open on this client, or 0 when
	// none is. The client is MODAL while a box is up, so this is what tells an
	// acknowledgement apart from a stray packet - and it must be cleared the
	// moment the close is sent.
	DialogHandle uint32

	// Suspended is a click whose script yielded at ShowSelDlg and is waiting for
	// the player to pick an option. While set, CZ_DIALOG_SELECT / CZ_ACK_DIALOG
	// resume the script instead of just closing the box.
	Suspended *suspendedClick

	// Walk is the chase in progress, or nil. The client reports nothing while it
	// walks a move we sent, so this is the ONLY model of where the player is
	// between the click and the dialog - see chase.go.
	//
	// ChaseFrom / ChaseFromSet carry the body's DRAWN position into the next chase when a
	// walk is replaced mid-stride (redirect, or a player move that overtook it). During a
	// server walk the client reports nothing, so the drawn point is the only honest anchor;
	// startChase consumes the pair once and clears the flag.
	ChaseFrom    [3]int32
	ChaseFromSet bool

	// ClickLockArmed is the reference's CLICK_LOCK (history\zone-npc-click.md, 2026-10-16 -
	// "A click LOCKS on to its NPC"). While a click's walk is in flight, a click on a
	// DIFFERENT NPC does not redirect the actor: the first one only ARMS that NPC and drops
	// the box the current walk would have opened, and the actor keeps walking to the NPC it
	// was locked on to. Only a SECOND click on that same other NPC stops the walk where the
	// body is and redirects it ("click NPC A and then click NPC B twice, it should
	// immediately target NPC B"). The handle is all that is needed; it is cleared when a
	// chase starts, when the click is cancelled, and by the redirect itself.
	ClickLockArmed uint32

	// TEAM WALKS LIVE IN teamWalks NOW (R131). Walk is the CLICK chase: one click, made
	// by the character the player drives, and the client drags the followers along. An
	// ATTACK or CAST approach belongs to the character that pressed it, and each of those
	// keeps its own walk in teamWalks - pressing on character 2 must not take character 0's
	// walk away, and a leader switch must not end anybody's approach (the owner's rule:
	// "each of the character to be independent"). See approachOf/addApproach.
	Walk *walk

	// teamWalks holds one walk per character for the approaches this server sent (attack
	// and cast). Slack: a walk is cleared at its arrival, its give-up, its cancellation or
	// a replacement by the same character, so an empty slot is the normal state.
	teamWalks []*walk

	// casts is the pending cast of each character, parallel to Team: the 3571 has
	// gone out and the result is owed when this entry's clock runs out
	// (zone\skill_cast_clock.go). It is a SEPARATE clock from Walk on purpose - the
	// reference sends its 3571 and then keeps echoing the player's moves for the
	// whole wind-up, so a cast is not a walk and must not be one.
	//
	// One per character, never a queue: a second press replaces it.
	casts []*castClock

	// buffs is each character's own buff list, parallel to Team and grown on demand
	// (zone\buff.go, R-buff slice 1). Written and read ONLY by this connection's own
	// goroutine, exactly like casts/swings/presses above: the item use, the click, the
	// script the zone runs and the tick all run there, so there is no mutex - a mutex
	// would suggest a second writer and there is none (cross-session grants are not in
	// this slice).
	buffs []*buffList

	// buffAt is the moment the buff lists were last advanced, so the tick ages them by
	// the MEASURED wall time and not by the poll interval - the reference's own
	// `timeGetTime() - m_prevTime` (BuffList.cpp:267-269). Advanced only when the tick
	// really runs, like lastTick.
	buffAt time.Time

	// spAt is the moment the team's SP was last regenerated (zone\sp_regen.go). It is
	// ONE clock for the session because every character ticks together, and it is
	// advanced only when a tick really fires: a session that was asleep, or stood in a
	// stance with no regeneration lane, must not bank ticks and pay them all at once.
	// Written and read only by this connection's own goroutine, like buffAt.
	spAt time.Time

	// lastTick is the moment this session's own clock last ran - the walk, the cast and
	// the attack repeat, all three advanced by Handler.tickIfDue (zone\chase.go). It is
	// advanced ONLY when the tick really runs, which is what keeps the cadence exactly
	// pollInterval: a loop that reset the stamp on every pass would let a busy socket
	// stop the clock altogether (R95 item 4).
	lastTick time.Time

	// lastDistLog is the moment this session last wrote its -diag-distance lines
	// (zone\diag_distance.go). It is the line's own clock, advanced only when lines
	// are really written, for the same reason as lastTick: tickIfDue runs at
	// pollInterval (10 times a second), the measurement was asked for once a second,
	// and a rate taken from the tick itself would be read off the tick's value.
	// Written and read only by this session's own goroutine.
	lastDistLog time.Time

	// Inv mirrors the client's bag KEY SPACE (position -> item id, with holes), so
	// an item packet addresses the row the client actually has - see inventory.go.
	Inv *InvMirror

	// LastShop is the shop window this session was last sent - normal or custom:
	// the group, and either its item ids or its custom entries, in the client's
	// order. A buy record carries an INDEX into that order, so without it a buy
	// cannot be resolved.
	LastShop *ShopState

	// LastMoveAt is when the client last reported a move. It tells a chase whether
	// the player is still finishing their own walk: a report inside ChaseGrace of our
	// own move is that tail, not a new instruction.
	LastMoveAt time.Time

	// MoveSpeed is the client's MEASURED walking speed, in units/second, taken from
	// its own consecutive CZ_MOVE_PC reports. The engine default (415) is a guess
	// about a stance, and a wrong guess is what makes a chase clock say "arrived"
	// while the character is still halfway - so the number is measured, not assumed.
	MoveSpeed float64

	lastMoveAt  time.Time
	lastMovePos [3]int32
	lastMoveIdx int
	moveSamples int

	// skipped counts the packets this session sent that Reply does not answer
	// yet, so the gap is visible in the log instead of looking like a client bug.
	//
	// It lives on the SESSION, not on the Handler, and that is the whole point:
	// the Handler is shared by every connection goroutine, and a map written from
	// two of them is `fatal error: concurrent map writes` - a runtime error that
	// recover() cannot catch and that kills the zone process. A per-connection
	// map has one writer by construction.
	skipped map[uint16]int

	// chatSeen is the same idea for chat text: a player typing normally must not
	// fill the log with one line per message, so each distinct text is logged
	// ONCE. Per session for the same reason as `skipped`.
	chatSeen map[string]bool

	// lastPointTried is "this session has already written its Last Point", set by
	// Handler.noteLastPoint (zone\last_point.go). ONE save per session: a log-out is
	// followed a moment later by the socket's own EOF, and a map change closes this
	// session too - without the flag each of those would write a second row and print
	// a second line for the same departure.
	lastPointTried bool

	// Handoff is the PAID map change whose handoff this zone has written and whose
	// deadline has not been decided yet, or nil (zone\gate.go, handoffWait). The
	// session's own tick checks it (gateTick), so the refund and its one message go out
	// on this very socket while the player is still here; a goroutine checks the same
	// jump after the deadline for the case where the socket is gone.
	Handoff *handoffWait

	// stancePushed is the set of THIS team's character handles whose
	// ZC_CHANGE_STANCE (3554) has already gone out to the other players - see
	// claimStance and pushOwedStance (players.go).
	//
	// It exists because the client REFUSES a 3554 while a move animation is playing
	// (CPC::SetStance only starts the idle when !IsMoveAnimPlaying(), PC.cpp:81-92),
	// so the one moment a character whose 3554 never went out CAN be given one is
	// the stop that ends its walk. Written when a join announces the team (another
	// player's goroutine) and read when a stop goes out (this session's own), which
	// is why it has its own mutex.
	stancePushed map[uint32]bool
	stanceMu     sync.Mutex
}

// suspendedClick is the state of one click whose script yielded at ShowSelDlg.
// The zone stores it on the Session so CZ_DIALOG_SELECT / CZ_ACK_DIALOG can
// resume the script down the chosen branch.
type suspendedClick struct {
	entry  string // SCR_CLICK_<name>, for logging
	handle uint32 // the NPC whose dialog is open
}

// Handler answers zone packets. It owns no connection state: that lives in
// Session, and the socket loop lives in Serve.
// cellKey is one point on a map's mesh, as an int32 triple: the spawner asks the engine for the
// same points many times over, so the answer is remembered against this key.
type cellKey struct{ x, y, z int32 }

// cellHit is one cached answer, error included: an off-mesh point is off the mesh for every body.
type cellHit struct {
	pos    [3]int32
	height int32
	err    error
}

type Handler struct {
	Table  *protocol.Spec
	Cipher *protocol.Cipher
	Log    *log.Logger

	// RawLog dumps every decrypted client chunk, BEFORE it is split, together
	// with the frames the splitter managed to cut from it. It is a diagnosis
	// switch, off in normal runs (a busy town would flood the log): the splitter
	// silently swallows a frame it cannot size, so when a known packet seems
	// never to arrive, this is the only way to see the bytes that actually came.
	RawLog bool

	// DiagStopOffset is a DIAGNOSTIC, not a feature: when non-zero, every
	// ZC_STOP_PC this zone broadcasts to the OTHER players has its x field moved
	// east by this many units. Off (0) is the only normal value, and the barrack
	// never sees it - it lives in main and only the zone role passes it.
	//
	// WHAT AN ANSWER MEANS. The far model JUMPS SIDEWAYS: the client's 3549
	// handler FOUND that handle and the packet is fine, so the fault is the
	// animation. The far model does NOT move: the handler never found the
	// handle, so the handle value is the problem - not the stop, and not the
	// animation. Either answer is worth more than another guess, which is why
	// this exists at all. Restart without the flag before believing anything
	// else the run says.
	DiagStopOffset int32

	// DiagHex is a DIAGNOSTIC, not a feature: when true, every character's 3520 spawn
	// is dumped as one DIAGHEX line of raw bytes, and its 3554 stance as another, at
	// the moment the character is announced. ONE-SHOT: a character is announced once
	// per join, so a log holds one dump per announcement and not a stream.
	//
	// The dumped bytes are the SAME bytes every other player receives - the announce
	// builds the spawn once and hands that slice to each observer - so the dump can be
	// diffed against a packet capture, and two characters' lines can be diffed against
	// each other off-line, with no movement and no combat map.
	//
	// WHAT AN ANSWER MEANS. If a leader and a follower differ ONLY in handle, job,
	// stance and position, the difference is on the client side (it draws those two
	// actors with the same player data). If some OTHER field differs, our own spawn is
	// wrong, and the dump names the offset: the two lines are aligned, so the first
	// differing hex character is the byte.
	DiagHex bool

	// DiagEvents is a DIAGNOSTIC, not a feature: `-diag-events`, default OFF. ON
	// restores the three per-event log lines byte for byte - MOVE_PC for every echoed
	// move, STOP_PC for every stop, and "SENT walk char ..." for every walk leg.
	//
	// OFF is the only normal value, and it is off because those three sit on the
	// PER-PACKET path: a line each is a format plus a write, and at 50 bots that is
	// ~100 lines/s while at 300 players in one town it is ~1000. A log that busy
	// buries the one line a reading is actually looking for. OFF they are not written
	// at all - and the information is NOT lost, because the METRICS line counts the
	// same three events (moves=, stops=, walks=). Turn this on when the QUESTION is
	// one event's line (which handle, which destination, which reason), not the count.
	DiagEvents bool

	// SightOff is a DEVELOPMENT FLAG, not a feature: `-sightoff`, default OFF. ON the
	// sight ring (zone\sight.go, R135) is INERT for the whole run: sightArmed answers
	// false and sightTick answers nil at its first line, so nobody is ever entered or
	// left by range - every session is told about everyone, exactly as before R135.
	//
	// IT EXISTS TO MEASURE, not to run. The owner saw a character pop out at the
	// map's ViewRange while his camera could still draw it; the distance at which
	// OUR client stops drawing a character on its own has never been measured, and
	// it cannot be measured while our own ring removes the actor first. With this ON
	// the removal is the CLIENT's doing, and `-diag-distance` records the numbers.
	// OFF is the only normal value, and OFF changes nothing at all.
	SightOff bool

	// DiagDistance is a MEASUREMENT SWITCH, not a feature: `-diag-distance`, default
	// OFF. ON, once a second, the zone logs one line per pair of sessions naming both
	// LEADERS' positions and their X/Y distance, and every STOP the zone sends gains
	// `| DIST to <handle> = <N> units`. See zone\diag_distance.go.
	//
	// LOG ONLY: it changes no packet, no state and no rule - the same bytes go on the
	// wire with it on and off (a test pins that). It exists for exactly one reading:
	// at what distance the client stops drawing an actor, taken beside a client-side
	// observation. OFF writes no line, and it is off in every normal run because a
	// pair line per session per second is a measurement, not something to live with.
	DiagDistance bool

	// QuietSpawn is the `-quietspawn` DEVELOPMENT FLAG (default OFF): ON, the STARTUP
	// spawner does not write its two per-body lines - `zone: MONSTER AI class ...` and
	// `zone: CREATE_TMONSTER ...` - and prints ONE line per distinct tactics decision
	// when the burst ends instead. See zone\spawn_quiet.go for what is kept, and for
	// the measurement that makes this a log switch rather than a speed switch: 858
	// lines cost 3-8 ms of a ~178 ms spawner on map 4063 (429 bodies), while they are
	// 84% of the lines the start writes.
	//
	// IT DOES NOT SILENCE A DIAGNOSTIC: a body whose tactics row does not resolve
	// still prints both lines, and so does every body created outside the startup
	// burst (a script's CreateTmonster, a probe, a respawn). OFF is the only normal
	// value and OFF is byte for byte what the zone wrote before the flag existed.
	QuietSpawn bool

	// spawnerRunning is true only inside StartSpawner's burst: it scopes QuietSpawn to
	// the one place the line volume exists. Atomic because a body can be created from
	// a script thread at the same moment (the burst runs before the listener opens, so
	// in practice it cannot - but a flag that is only safe by ordering is a bug
	// waiting for the next caller).
	spawnerRunning atomic.Bool

	// spawnQuiet is what the silenced burst counted, printed when the burst ends.
	spawnQuiet spawnQuietLog

	// MonsterAIScript is the `-monai script` DEV SWITCH (default OFF, i.e. `go`): ON,
	// a living monster's think turn is the GAME'S OWN tactics script
	// (`SCR_<Tactics>_TS_<state>` -> `SCR_MON_AI_LIB_TS_BATTLE`) run as a thread
	// (zone\monster_ai.go, script\monster_ai.go); OFF, today's Go chase and swing are
	// the whole of the monster's behaviour and NOT ONE BYTE changes.
	//
	// IT IS A SWITCH, NOT A REPLACEMENT, on purpose: the Go chase stays in the tree as
	// the fallback until the owner has compared the two in game, and OFF is the value
	// every existing test runs on.
	MonsterAIScript bool

	// MonsterBrain is the Lua host behind the think turn (script\Host). NIL means "no
	// scripts" (-no-scripts, or a unit test): the think turn then does nothing and
	// says so once, instead of half-running a body.
	MonsterBrain MonsterBrain

	// MonsterNow is the clock the script's ORDER and its MOVE STATE share: the instant
	// `orderMonsterMove` stamps as a leg's `began` and the instant `EnableMove` measures
	// that leg against. NIL means time.Now(), which is every production run.
	//
	// IT EXISTS FOR THE TESTS, and it is the only honest way to pin a walking body: a
	// test drives `monsterAITick` on a simulated clock (the runner's own `sleep`s are
	// simulated seconds), so the leg's clock must be the same one or a 40-second walk is
	// 40 real seconds long. The alternative - letting the order stamp `time.Now()` while
	// the tick is handed a simulated instant - makes every leg appear to begin in the
	// FUTURE, which `posAt` correctly reads as "not walked yet": the body freezes and the
	// test measures the harness.
	MonsterNow func() time.Time

	// fwdFail is sendOthers' own rate limiter for the "could not forward" line: one
	// dead client in a crowded town fails once per recipient per packet, so the first
	// few lines print in full and the rest collapse to one line per window. The mutex
	// is inside it and is taken only on the error path. See zone\players.go
	// logForwardFailure.
	fwdFail forwardFailLog

	// queueFail is the same limiter for the other crowd-scale line: a session closed
	// because its client stopped reading (zone\writer.go, overflow). A town where
	// many clients drop at once must produce one line per window, not one per client.
	queueFail forwardFailLog

	// writeFail limits the third one: a session whose socket failed mid-write, which
	// happens once per socket whenever a crowded zone restarts or a crowd leaves.
	writeFail forwardFailLog

	// SpawnMonster is slice S1's DEVELOPMENT FLAG (`-spawnmonster`, default OFF):
	// when true, ONE ZC_ENTER_STANDINGMONSTER (3527) introduces a single monster at
	// a fixed spot on the first GAME_READY of a session (`zone\monster.go`).
	//
	// OFF is the only normal value, and it is off so that every other map's wire is
	// byte for byte what it was before S1 existed. ON, the fixed cell is asked of
	// the map's mesh before the packet is sent: a cell that is not ground draws
	// nothing and looks like a server fault, so a zone that cannot prove it sends
	// nothing and says why in one line.
	SpawnMonster bool

	// StrictRange is R84's DEVELOPMENT FLAG (`-strictrange`, default OFF): when true
	// a cast whose target or point is outside the skill's own MinR..MaxR is REFUSED
	// and nothing is sent, which is what this server did unconditionally before R84.
	//
	// OFF is the normal value, and it is off because the REFERENCE'S OWN RULE for an
	// out-of-range press is not measured yet: the one capture we have
	// (captures\skillcast20261031, Rapid Shot at 919 of MaxR 1200) is a cast that was
	// INSIDE the range, so it cannot say what the reference does outside it. A refusal
	// needs a reason we can point at, so OFF logs the distance and the range and then
	// APPLIES the cast anyway. ON restores the refusal for the day that rule is
	// measured - the switch is already here so the measurement is one flag away.
	//
	// A refusal with no measured rule is not caution: it refused the owner's Fighter
	// at 443 units on melee skills whose MaxR is 100-200, and the owner read that,
	// correctly, as "fighter cant cast skill". See zone\skill_area.go
	// (skillRangeAllowed) for the one place the arithmetic lives.
	StrictRange bool

	// SPLock is the `-splock` DEVELOPMENT FLAG (default OFF): the caster's SP is CUT
	// by the skill's own SpendSP exactly as the reference cuts it, and then put back,
	// so the 3654 carries a full bar and the CLIENT never refuses the next press for
	// lack of SP.
	//
	// WHY IT EXISTS. A cast spends SP once, at the press (zone\skill_area.go), and
	// nothing in this server ever gives SP back - there is no regeneration lane, and
	// the value is never saved between sessions, so the bar only refills when the zone
	// reads the character in again. A formula test on a stance whose skills cost
	// hundreds of SP (Disable ring's five are 100/300/300/650/750 from a 1390 bar)
	// therefore runs dry after a few presses and the CLIENT - not this server, whose
	// own cut clamps at zero and would keep answering - is what stops. That is a
	// testing wall, not a game rule, so it gets a switch instead of a half-built
	// regeneration rule whose rate nobody measured.
	//
	// OFF is the only normal value and it is the reference's behaviour: the cut is
	// made, the 3654 carries the reduced value, and the two-dummy test still proves
	// 500 - 60 = 440. ON, that same test would read a full bar, which is why no test
	// sets it except the one written for this switch.
	SPLock bool

	// SimLatency is the `-simlatency <ms>` DEVELOPMENT FLAG (default 0 = OFF): how
	// long, in milliseconds, every packet this zone writes to a client is held back
	// on the way out, so the zone can be played at something like a real network's
	// latency instead of the 0 ms of 127.0.0.1. Zero is the only normal value, and
	// at zero the write path is exactly what it always was - same function, same
	// mutex, no queue, no goroutine.
	//
	// NO GAME RULE CHANGES. It delays bytes leaving; it does not delay the zone's
	// own clock, its swing cadence, its reach, its walk handling or its builds. A
	// reply that arrives late is the only difference a client sees, which is the
	// point: a rule that only looks right because the answer was instant will show
	// up here. See zone\writer.go for the one writer goroutine, its queue and the
	// one log line it writes.
	SimLatency time.Duration

	// TeamOf supplies the characters to spawn for an account. Nil means "no
	// characters", which is a legitimate test state and logs loudly.
	TeamOf func(ctx context.Context, accountID uint32) ([]Character, error)

	// ConsumeEntry takes the barrack's ONE-SHOT handoff for this account and marks it
	// used (zone\entry_consume.go, card t_0cda12ac). It answers an *EntryRefused when
	// the row was already used or is older than the 60 s window - the entry must then
	// NOT happen. NIL means "no store wired" (a unit test): every entry is allowed,
	// which is the behaviour before the handoff was a token.
	ConsumeEntry func(ctx context.Context, accountID uint32) error
	// DropEntry deletes the handoff once a session is over (a logout), so nothing can
	// be replayed from it later. NIL is quiet.
	DropEntry func(ctx context.Context, accountID uint32) error

	// noCadenceLogged is set by the first normal attack whose sheet gave no attack
	// delay (zone\attack.go logNoCadenceOnce), so the "not throttled" line appears
	// once per run instead of once per click. It is about this build's data, not
	// about a click.
	noCadenceLogged atomic.Uint32

	// noReachLogged is the same idea for the REACH (R90): set by the first normal
	// attack whose stance row states no MaxR, so "reach not checked" is said once
	// (zone\attack.go logNoReachOnce) rather than on every click. Same reason: it is
	// a fact about the data this build loaded, not about the press.
	noReachLogged atomic.Uint32

	// noMonsterSizeLogged is the same idea for the TARGET's half of the reach (R91 round
	// 5): set by the first attack whose monster row names no body size this build's shape
	// table knows (zonettack_approach.go, logNoMonsterSizeOnce), so "the reach is one
	// body short" is said once per run. A fact about the data loaded, not about the click.
	noMonsterSizeLogged atomic.Uint32

	// swingNoAtkTypeLogged is the same idea for the REPEAT (R95 item 2): set by the first
	// swing whose stance row states no AtkType at all, so "the repeat keeps today's
	// behaviour" is said once per run rather than on every press (zone\attack_swing.go,
	// armSwing). A fact about the data this build loaded, not about the click.
	swingNoAtkTypeLogged atomic.Uint32

	// swingUnsupportedAtkTypeLogged is its other half: set by the first swing whose stance
	// row names an AtkType this build cannot repeat (Double, ...), so
	// "no repeat is armed for that attack type" is said once per run. Same reason: the
	// stance column is loaded once, not once per press.
	swingUnsupportedAtkTypeLogged atomic.Uint32

	// splashCappedLogged is the same idea for the SPLASH's own area
	// (zone\attack_splash.go): set by the first splash hit whose circle held more bodies
	// than this build will put in one 3566 - maxAreaTargets, the area SCAN's cap of 224,
	// which is BELOW the builder's own count-byte ceiling of 255 (maxSplashVictims) - so
	// "the list was capped" is said once per run rather than once per swing. A fact about
	// this build's area cap, not about the click.
	splashCappedLogged atomic.Uint32

	// splashNoRangeLogged is its other half: set by the first hit in a Splash stance whose
	// row states SplRange 0 (or this zone has no stance table), so "the row names no area,
	// so the circle holds nobody" is said once per run. 0 is the table's own "no area" and
	// not an error - the main target is still hit.
	splashNoRangeLogged atomic.Uint32

	// Properties supplies the commander's Lock_* values, which the client CACHES.
	// Without them it draws its own table defaults (248 bag slots). Nil or empty
	// is allowed and means "no properties", which is not an error.
	Properties func(ctx context.Context, accountID uint32) ([]Property, error)

	// CommanderOf maps an account to its commander, once per login. The click
	// path needs it because a script transaction writes to a COMMANDER, and the
	// account id alone does not name one. NIL means "not wired": a session then
	// has no commander, and a script's transaction refuses to commit (it does
	// not write to account 0).
	CommanderOf func(ctx context.Context, accountID uint32) (int32, error)

	// GmLevelOf answers a family member's operator tier: 1 is the GM tier, and an
	// absent row is 0. It is read ONCE at START_GAME, beside CommanderOf, and the
	// answer lives on Session.GmLevel. NIL means "no level table wired": every
	// session is level 0 and no command is dispatched (zone\gm.go).
	GmLevelOf func(ctx context.Context, memberID string) (int, error)

	// Bag supplies the commander's inventory rows, oldest first. The entry burst
	// draws the bag from it, and so does a re-sent list after a script moved
	// items. NIL means "no bag": the client is sent an empty inventory, which is
	// what it got before this existed.
	Bag func(ctx context.Context, commanderID int32) ([]item.Stack, error)

	// Stances supplies the stance and skill blocks each character record carries.
	// NIL means "send none", and that CRASHES the client a millisecond after the
	// entry packet - so a server started without it says so before the client
	// connects, instead of after.
	Stances *StanceTable

	// Status is the Character Info SHEET's source: the game's own status script
	// (zone\status_script.go). It is not an instrument any more - every 3684, the
	// spawn's HP/SP maxima and the combat cadence read its answers. Nil means the
	// zone has no sheet, which it says once at startup and then shows as zeros.
	Status *StatusScript

	// Buffs is the game's own Buff table (`ge\xml\datatable_buff.xml`, 1,962 rows,
	// zone\buff_table.go). It is what makes a buff NAME a row: its UpdateType says
	// whether it expires, its GroupIndex/BuffRank say what it replaces, and its
	// ApplyTime is the seed the ENTER script overwrites.
	//
	// NIL means "no table": every buff application is REFUSED with a line naming the
	// file, because a buff with an invented row is a mask bit with no state behind it.
	Buffs *BuffTable

	// BuffScript runs a buff's own `SCR_BUFF_ENTER_<ClassName>` (zone\buff_script.go)
	// - the only place a duration exists, since 1,954 of the 1,962 rows carry
	// ApplyTime 0. NIL means "no corpus": every buff is scriptless and every NORMAL
	// one is refused by name instead of being granted with 0.
	BuffScript *BuffScript

	// Damage is the game's OWN damage formula (zone\skill_script.go) and it DECIDES
	// the number a skill cast puts on the wire: the script's own factor, multiplied
	// by this character's ATK. Nil means "not armed" - a checkout with no corpus -
	// and the cast then carries the placeholder exactly as it did before R84. A script
	// that raises also leaves the placeholder in place.
	Damage *DamageScript

	// Defence is the game's own `SCR_Get_MON_DEF` (zone\monster_def.go): the target's
	// DEF, run through the same script host from the monster row's own Arm/DEF_BM/
	// DEFPER_BM/RaceType columns. It is the input the damage rule divides by, and it
	// is read PER CAST - nothing is cached, because a spawned body's row is what the
	// game itself would read at that moment. Nil means "not armed": the cast then
	// carries the PLACEHOLDER and the log line says the DEF could not be read. 0 is
	// never substituted - 0 is a real DEF, the Scarecrow's own.
	Defence *MonsterDefScript

	// MaxHP is the game's own `SCR_Get_MON_MHP` (zone\monster_hp.go): a spawned
	// monster's maximum HP, run through the same script host from the row's own
	// Lv/CON/MHPFix/MHP_BM/ADDMHP_BM columns and its RaceType/ClassID. It is read
	// at SPAWN, once per body: nothing in the row changes afterwards, so a second
	// run per cast would pay for the same number.
	//
	// Nil means "not armed": a spawn that passed no MHP then keeps 0 and logs ONCE
	// why (zone\create.go) - never a made-up maximum, because a body with an invented
	// HP bar looks exactly like a working one.
	MaxHP *MonsterHPScript

	// Skills is the game's own Skill table (`datatable_skill.xml`,
	// zone\skill_table.go): the NAME of a skill id and its SklATK factor. The cast
	// (`zone\skill_cast.go`) needs both - one to name the skill in the log line, the
	// other to turn the character's ATK into a damage number.
	//
	// NIL means "no table", and a cast is then REFUSED with a log line naming the
	// skill: without it this zone could fire a hit it cannot name or price, and the
	// damage would be a guess.
	Skills *SkillTable

	// Spawn is where the team is put on entry, as WORLDPOS{x, y, CELL}.
	//
	// The CELL is a navmesh triangle index, NOT a height. Sending the stored z
	// value here puts the character on an invalid triangle, and the client crashes
	// inside PathEngine.DLL the moment a walk starts (measured 2026-10-25,
	// Application Error: faulting module PathEngine.DLL, 0xc0000005).
	//
	// The zero value means "use the position stored on the character", which is
	// only correct when that position is known to be a cell - the CONTINUEGAME
	// case, once it is read from the database.
	Spawn [3]int32

	// SpawnFormation is where each member of the entering team stands, slot 0 = the
	// leader (= Spawn), slots 1.. the followers, from the same file's `formation`.
	//
	// Empty means "everybody on Spawn": that is what a version-1 file says, and it
	// is what this zone did before the formation existed - which stacked the team on one
	// point in game.
	SpawnFormation []FormationSlot

	// NPCs are the town NPCs to spawn on world-ready, from the emitted data file.
	// Empty is allowed: the town is then empty, which is not an error.
	NPCs  []NPC
	MapID int

	// MapName is this map's CLASS name ("coim_basic"), the name the client's own tables
	// use. A warp NPC's arrow has no Warp row of its own, so the row is found by
	// (this map's class, the arrow's destination class) - see WarpTypeTable.TypeForArrow.
	MapName string

	// Warps is the Warp table's ClassIDs, which are the `type` a warp point packet
	// carries. NIL means "not loaded": the packets still go out, with type 0, and each
	// spawn line says so - a guess that is visible beats a silence (see LoadWarpTypes).
	Warps *WarpTypeTable

	// Points are the map's TOWN ARRIVAL points, button 1..4, from the same data
	// file's `points` list. Empty is the normal state for a field map and means the
	// quick-move panel has nowhere to go - a button press is then refused with a log
	// line rather than answered with a guess.
	Points []TownPoint

	// Gates are the map's doorways OUT, from the same data file's `gates` list. The
	// zone watches the leader against them on every tick (gate.go): standing within a
	// gate's own `range` starts a map change. Empty is the normal state for a map with
	// no exits in the table.
	Gates []MapGate

	// ZonePoints is the spawn table the map rows above came from, kept whole because a
	// warp NPC needs what the flattened fields cannot answer: its SCRIPT is looked up in
	// `by_name`, and the destination's spawn and camera come from THAT map's rows
	// (gate.go, warpGate). NIL means "no table": a warp NPC is then refused with a log
	// line, and a gate is unaffected.
	// SaveWarpIDOf reads a commander's SAVED START POINT - the WARP ID in `commander.point`,
	// the same number the barrack shows and the client's 3467 answer carries. It is the
	// "To the Save Point" button's input (zone\sysmenu.go):
	// CCommander::TransferRandomLobbyZone reads RestartWarpID (Commander.cpp:4022-4050).
	SaveWarpIDOf func(ctx context.Context, commanderID int32) (int, bool, error)

	ZonePoints *ZonePoints

	// AskForZones makes a missing map a REQUEST to the zone manager (cmd/zoneman) instead
	// of a refusal: the door asks, waits ZoneStartWait, and opens when the zone announces.
	// main.go sets it for a real zone; tests leave it false and keep the plain refusal.
	AskForZones bool

	// Zones is the zone registry, read for the ip and port the destination map is
	// served on. NIL means "no registry": a gate is then refused with a log line
	// naming the map, because 3594 cannot be built without an address - and a 3593
	// with no 3594 behind it strands the player in a handshake.
	Zones *world.Registry

	// StartPoint reads the commander's SAVED entry point - the town whose Save button
	// was pressed last (zone\start_point_db.go, `commander.point`, a RESTART WARP ID).
	// It is read once, on entry, and only when no handoff file already placed the team.
	// It is the first choice for a NEW GAME (cmd 22) and for the mission button (24);
	// CONTINUE GAME (23) asks for the Last Point first - see Handler.placeTeam.
	//
	// NIL means "no store": the entry then falls through to the Last Point. found is
	// false for a commander that has never saved, and that is NOT an error - it is
	// the normal first login.
	StartPoint func(ctx context.Context, commanderID int32) (StartPoint, bool, error)

	// saveStartPoint WRITES the start point; it is unexported and set only by
	// UseStartPoint, because a handler with the read hook and no write hook would
	// show a saved town and refuse the button that changes it.
	saveStartPoint func(ctx context.Context, commanderID int32, warpID int) (StartPoint, error)

	// EntryKindOf says which barrack button this commander pressed - EntryNew (cmd 22)
	// or EntryContinue (23) - because the entry order depends on it
	// (zone\entry_kind.go). NIL means "no store": the entry behaves as EntryNew, which
	// is what every commander got before this hook existed.
	EntryKindOf func(ctx context.Context, commanderID int32) (EntryKind, error)

	// LastPointOf reads where the leader stood when this commander last left the zone
	// (zone\last_point_db.go), together with whether the barrack's Continue button was
	// the one pressed. It is read once per entry, and only when no handoff file
	// already placed the team.
	//
	// NIL means "no store": the team then enters at the map's own spawn.
	LastPointOf func(ctx context.Context, commanderID int32) (LastPoint, bool, error)

	// saveLastPoint WRITES the last point for the team's character ids (one
	// `character_position` row each); unexported and set only by UseLastPoint, for the
	// same reason as saveStartPoint: the pair is one feature.
	saveLastPoint func(ctx context.Context, commanderID int32, characterIDs []uint32, lp LastPoint) error

	// Transfers is the folder the map-change handoff files live in: the OLD zone
	// writes transfer_<account>.json there, the NEW one consumes it once. Empty means
	// world.DefaultDir, which is the same folder the registry uses - both zones must
	// agree on it without a second flag.
	Transfers string

	// Click runs one NPC's click script and reports what it asked the client to
	// show. NIL means "no script engine here": a click is then logged and dropped
	// rather than answered with an invented line, because a wrong dialog is worse
	// than none - it hides which NPC has no script.
	//
	// It is a function field, not a package import, so the zone can be tested
	// without a Lua corpus and the dependency stays one-way (main wires it).
	Click func(entry string, self, pc ClickObject) (ClickReply, error)

	// Text is the client's dictionary. Two uses, both of them the SERVER's own
	// words: the click log lines (a log names the NPC's line in words instead of
	// a token) and the caption a town NPC's spawn frame carries (zone\npcs.go,
	// resolveNPCName - the reference fills that field from the dictionary too).
	// The DIALOG packets carry the script's own text untouched - the client
	// resolves its own `<$>N</>` - so a Resolve here must never reach a dialog.
	// NIL passes the token through into the log only, and every caption empty.
	Text *text.Book

	// ShopGroups is what a normal shop sells, group name -> class ids in the
	// client's order, from data\shop_groups.json. A buy names its item by the
	// INDEX in that list, so this is what resolves it. NIL means "not loaded":
	// the shop window still opens, and a buy then has nothing to resolve.
	ShopGroups ShopGroups

	// CustomShops is what a CUSTOM (barter) shop offers, group name -> its rows,
	// from data\custom_shops.json. Unlike a normal shop the window packet CARRIES
	// the items and their prices, so this is what draws the window at all - and
	// what a barter buy resolves its index against. NIL means "not loaded": a
	// custom window is then answered with ZC_DIALOG_CLOSE only, so the client is
	// never left modal with nothing to show.
	CustomShops CustomShops

	// Prices is what one of each item costs and sells for, from
	// data\item_prices.json. NIL means "not loaded", which makes every price 0 -
	// so a caller that would charge money must treat "no price" as a refusal, not
	// as free.
	Prices *item.Prices

	// FesoPrices is what a FESO shop charges and pays per class id, from
	// data\item_feso_prices.json. It is a SEPARATE table from Prices on purpose: a
	// Feso shop is the same window with a different coin, and a class can be sold
	// for one coin and not the other. NIL means "not loaded": a Feso buy is then
	// refused outright, because a missing price must never read as free.
	FesoPrices *item.FesoPrices

	// Commit applies item ops in ONE database transaction and reports what the
	// client has to be told. It is the same method a script transaction uses
	// (`PGStore.CommitTx`), wired in by main.go, and it is a function field for the
	// same reason the others are: a buy and a sale must be testable without a
	// database. NIL means "no write path here", and a shop then REFUSES - a shop
	// that says "done" without writing is worse than one that stays open.
	Commit func(ctx context.Context, commanderID int32, props map[string]int64, ops []item.Op) ([]item.Change, error)

	// Equip moves one bag row into one wear slot and reports what the client has
	// to be told: which item left the bag, which item (if any) came back, and the
	// character's whole worn set afterwards. It is `PGStore.EquipItem`, wired in
	// by main.go, and a function field for the same reason Commit is: the equip
	// answer must be testable without a database.
	//
	// NIL means "no write path": an equip is then REFUSED with a log line, the
	// same rule Commit follows - an equip that says "done" without writing is
	// worse than one that does nothing.
	Equip func(ctx context.Context, commanderID, characterID int32, itemID int64, spot int) (EquipResult, error)

	// Unequip is the other half: one worn item back into the bag, with the SAME
	// item id and count (the row moves, it is not minted again). NIL means "no
	// write path": an unequip is then REFUSED.
	Unequip func(ctx context.Context, commanderID, characterID int32, spot int) (EquipResult, error)

	// teleports is the saved-point store behind the `teleportation` window: read the
	// list, write a point the player just saved, delete one. It is `PGStore`'s three
	// saved_teleport methods, wired in by main.go, and a struct of function fields
	// for the same reason Commit and Equip are: the save/list/remove/travel answers
	// must be testable without a database.
	//
	// ANY NIL FIELD means "no write path": the request that needs it is REFUSED with
	// a log line and nothing is sent. See saved_teleport.go.
	teleports teleportSettings

	// Plan answers the live server's own decision: where to stand, how long the walk
	// should take, and the ROUTE it walks. NIL means "no path engine here": the chase
	// then sends one straight move to the NPC's own spot, capped at MaxChase.
	//
	// (from, npc, standoffDistance) -> (goal, route, routeLength, overlap, routable,
	// error). routable=false means the player is not on valid ground and NOTHING may be
	// sent. route is the polyline the walk will be walked ALONG (move.go,
	// walkedRouteAt); nil means "no route": the body then walks the straight chord,
	// which is what every caller did before R110.
	Plan func(from, npc [3]int32, standoff float64) (goal [3]int32, route [][3]int32,
		routeLen float64, overlap bool, ok bool, err error)

	// Route plans the polyline a CLIENT's own reported leg walks: from the point this
	// zone has drawn the body on now, to the destination that CZ_MOVE_PC named. It is
	// the same planner as Plan without the stand-off question - the map's own mesh, the
	// game's own path engine (`PathEngine.Route`).
	//
	// NIL means "no path engine here": a client's leg then walks the straight chord,
	// which is what every one of them did before. The walk graph cannot answer this:
	// it stores walk LENGTHS between sampled nodes, not a polyline, so a route from it
	// would have corners the map does not have.
	//
	// It is called ONCE per leg, where the leg opens - never per tick.
	Route func(from, to [3]int32) (route [][3]int32, ok bool)

	// NavHeight answers the navmesh HEIGHT of a WORLDPOS - the game's own
	// `CWorld::GetHeight(pos)`. It is how the Last Point gets a number to put in
	// `character_position.ZPOS`, which is a HEIGHT and never a cell.
	//
	// NIL means "no path engine on this machine": the Last Point is then NOT written,
	// with a log line, because the only other number this side has is the cell - and a
	// cell in that column is exactly the bug R73 fixes. The row already on file is left
	// alone; the entry refuses it if it cannot be trusted.
	NavHeight func(pos [3]int32) (int32, bool)

	// NavNear answers the navmesh POSITION nearest a 3D point - the game's own
	// `Get3DPointToPosLocaly(shape, point, out, 1000, 300)`. It turns a stored HEIGHT
	// into the CELL the client needs on entry, and it is the only thing that can tell a
	// stored height apart from a stored cell: the mesh's own height at that spot is a
	// different number from a cell index.
	//
	// NIL means the same as NavHeight's NIL: a stored point cannot be trusted, so the
	// entry falls through to the map's own spawn and says so.
	NavNear func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool)

	// cellCache remembers what the mesh said about a point (meshCell). A spawner puts a whole
	// division's quota on a handful of points and the engine round-trip is ~60 ms, so the first
	// Katovic spawn asked the same dozen questions 429 times (measured 2026-11-10).
	cellMu    sync.Mutex
	cellCache map[cellKey]cellHit

	// ResumeClick resumes a suspended click script with the player's chosen
	// option index. NIL means "no resume path": dialogs can be closed but not
	// branched - the same as before this feature existed. Wired by main.go from
	// the script Host's ResumeClick.
	ResumeClick func(chosenIndex int) (ClickReply, bool, error)

	// ReleaseClick drops a click script left suspended when this connection ends.
	// NIL means "nothing to release".
	//
	// A suspended click is ONE global slot in the script host, so a player who
	// walks away from a menu - or a client that just disconnects - would leave it
	// set and every other click on the server would be refused until a restart
	// (live log, 2026-10-26). Serve calls this on the way out for that reason.
	ReleaseClick func()

	// online counts the sessions this zone is serving right now. The presence file
	// carries it so the manager can stop a map nobody is standing in.
	online atomic.Int64

	// Players is who else is in the zone, so the players can SEE each other: spawn
	// on join, move on every step, stop when a walk ends, leave on disconnect.
	// The ZERO VALUE is ready to use - a Handler built in a test starts with an
	// empty roster and needs no wiring.
	Players Players

	// NPCTypes resolves an NPC table ClassName ('npc_tbk_contbox_off') to the
	// ClassID a spawn packet carries (zone\create_tables.go). It exists for
	// `CreateTNPC`, whose third argument is a NAME and whose packet needs a NUMBER.
	// NIL means "no table": such a call is then REFUSED with a log line naming the
	// type, never sent with type 0.
	NPCTypes *NPCTypeTable

	// Monsters is the game's own Monster table (zone\create_tables.go). It is used
	// to REFUSE an unknown id before anything is sent, and to name the row - level,
	// EXP, tactics - in the log. NIL means "no table", and a CreateTmonster is then
	// refused.
	Monsters *MonsterTable

	// Items, MapDrops and DropPenalty are the three tables a drop roll reads
	// (R98 slice 3, zone\drop_tables.go). NIL means "not loaded yet": the FIRST
	// kill loads whatever is still nil, once, because the Item idspace is 25,353
	// rows across 26 client files and a zone that never kills must not pay for it
	// at start-up. A test sets all three and no file is read at all.
	Items       *ItemTable
	MapDrops    *MapDropTable
	DropPenalty *DropPenaltyTable

	// DropRand is the ONE source of drop randomness (zone\drop.go). NIL means the
	// standard library; a test installs a scripted sequence here so an item and a
	// quantity are reproducible without touching a package-level seed.
	DropRand DropRand

	// dropOnce is the lazy-load gate above. dropMu guards the kill guard
	// (dropDone, the reference's m_isItemDropped), the once-per-session log flags
	// (dropSaid) and itemSeq the same way spawnMu guards the monster counter.
	dropOnce sync.Once
	dropMu   sync.Mutex
	dropDone map[uint32]bool
	dropSaid map[string]bool
	itemSeq  int

	// groundItems is EVERY field item this zone has created on the floor and nobody
	// has taken yet, keyed by the handle its 3536 carried (R98 slice 4,
	// zone\pickup.go). It shares spawnMu with itemSeq and the monster roster: a kill
	// arms a drop on one session's goroutine while another session can send
	// CZ_PICKITEM, so the handle allocation, the insert and the claim are one lock.
	// Nothing here is persisted - a ground item lives as long as this process, the
	// same way the reference's field objects are world state and not database rows.
	groundItems map[uint32]groundItem

	// MonsterExp is the experience a kill pays per level: the `Xp` idspace's
	// `MonXp` column, the game's own table (`zone\monster_exp.go`, emitted by
	// src\export_exp_table.py). NIL means "no table", and a kill then awards 0 and
	// says so ONCE - it never invents a number and never crashes the zone.
	MonsterExp *MonsterExpTable

	// Tactics runs ONE tactics entry: `SCR_<script>_<STATE>`, the thread the
	// original starts when a trigger object enters the world
	// (`TriggerObject.cpp:49-57`). Handler.StartupTactics uses it to run every
	// scripted town NPC's TS_NONE entry once at zone start, which is the ONLY thing
	// that calls `CreateTNPC` for the Scarecrow's four dummies.
	//
	// NIL means "no scripts" (a zone started with -no-scripts, or a unit test):
	// the entries are then not run and the log says so, instead of a silent
	// nothing-on-the-field. `found` is false for an NPC with no tactics script,
	// which is the common case and not an error.
	Tactics func(entry string, self ClickObject) (found bool, err error)

	// THE LIVE CLAIM (zone\live.go, card t_487ea31c): which session holds an account and which
	// holds a character handle. A second login on one account displaces the first.
	liveAccounts map[uint32]*Session
	liveChars    map[uint32]*Session

	// Prewarm asks the zone manager for every map this one has a gate to, the moment a
	// player enters (zone\prewarm.go, card t_2950bb0a). OFF by default: a warm map costs
	// ~142 MB, and the manager's idle stop is what reaps one nobody enters.
	Prewarm bool
	// RequestZone is the manager's own request path (world.Registry.Request). NIL means
	// "no manager": a pre-warm then does nothing at all.
	RequestZone func(mapID int) error

	// TacticTick wakes the trigger threads whose `sleep` has expired (R141): the
	// tactics entry an NPC started is a THREAD now, not a one-shot call, so it needs
	// a clock of the zone's. TacticArmed answers whether any thread is still alive,
	// so a zone with none keeps its tick off. Both NIL mean "no scripts wired" - the
	// same as Tactics.
	TacticTick  func(now time.Time)
	TacticArmed func() bool

	// spawnMu guards the two runtime lists below. They are the ONLY world state a
	// SCRIPT mutates while the zone serves, and two players can click at the same
	// moment: a slice appended by one goroutine while another ranges it is a crash,
	// not a glitch.
	spawnMu        sync.Mutex
	spawnedNPC     []NPC
	spawnedMonster []Monster
	monsterSeq     int

	// monsterAIMu serialises the THINK TURN across sessions (zone\monster_ai.go). The
	// Lua host is NOT safe for concurrent use, and the tick is reached by every
	// session's loop, so two sessions ticking in the same instant would otherwise
	// resume the same coroutine twice. Taken BEFORE spawnMu (a script call takes
	// spawnMu, never this).
	monsterAIMu sync.Mutex

	// monsterAIBodies is the set of handles the last think turn saw, so a body that
	// left the field has its thread and its "this entry is spent" mark dropped
	// (script\Host.ForgetMonster). Guarded by monsterAIMu.
	monsterAIBodies map[uint32]bool

	// monsterHP is the LIVE HP of each monster this zone created, keyed by handle
	// and guarded by spawnMu. It exists because Monster.HP is an int32 that cannot
	// hold a Scarecrow's own HP: the reference's dummy sat at 999,350,000,000
	// (measured, `history\combat-attack-heal.md` §2.3), and a float64 holds it
	// exactly. It is written in ONE place - SpawnMonsterObject, with the number the
	// script gave the monster - and moved in ONE place, damageMonster (both in
	// zone\create.go). A monster with no entry has NO HP this zone can name, and the
	// plain attack then refuses to invent one.
	monsterHP map[uint32]float64

	// monsterBuffs is each MONSTER's buff list, keyed by the body's handle - the
	// reference's `CMonsterNPC` is a CFieldChar and owns a CBuffList like a player's
	// (`CMonsterNPC::OnProcess` ticks it, MonsterNPC.cpp:343). It exists because a
	// skill's own buff rule can land on a monster (`AddBuff(self, ...)` with a monster
	// `self`, common_pc.scp:6903) and `IsBuffApplied` must then answer the truth.
	//
	// Protected by monsterBuffMu, which is taken only to fetch or create a list; the
	// lists themselves are written from inside a script run, and every path that
	// reaches one holds the buff runner's own lock (BuffScript.runMu) - so two writes
	// cannot overlap. A handle is never reused (monster.go), and the list is dropped
	// when the body leaves (respawn.go).
	monsterBuffs  map[uint32]*buffList
	monsterBuffMu sync.Mutex

	// monsterBuffAgingSaid is the ONCE flag behind noteMonsterBuffNotAged: the first
	// monster buff says that this slice does not age a monster's list.
	monsterBuffAgingSaid bool

	// monsterHate is the BATTLE SCRIPT's own working set, keyed by the MONSTER's
	// handle: the hate list (who stands at how many points), the tactics target
	// SetTacticsTarget stored, and the attack state IsAttacking reads. It is what
	// `ge\script\monster\mon_ts_battle.scp` asks about on every loop, and this zone
	// decides NOTHING about it - the script's own calls move the numbers
	// (zone\monster_hate.go, and the owner's ruling in that file's header).
	//
	// Guarded by spawnMu, the same short lock the roster and the HP ledger use, and
	// pruned against that roster on every access: a handle that is no longer a body
	// on this field has no table, so a body's hate dies with it. It is NOT a second
	// list of monsters - the tables hang off the same handle key as monsterHP and the
	// roster is the one list.
	monsterHate map[uint32]*hateState

	// monsterEngines is the READ-ONLY half of the same working set
	// (zone\monster_engine.go): each body's TACTICS STATE (what `ChangeTactics`
	// stores) and the point it spawned at. `GetMonAiOption` reads the AI row through
	// `Monster.Tactics`, which is a different string and is not here.
	//
	// Guarded by spawnMu and pruned against the roster on every access, like
	// monsterHate - a body's state dies with the body.
	monsterEngines map[uint32]*monsterEngineState

	// monsterAi is `datatable_monsterai.xml` with EVERY column, loaded once on the
	// first `GetMonAiOption` (the columns the chase acts on live on MonsterRow
	// instead). NIL means "not loaded" and then the option reads answer NULL.
	// Guarded by spawnMu.
	monsterAi *monsterAiTable

	// engineSaid is the ONCE-per-key log gate of zone\monster_engine.go (a refused
	// tactics name, a missing AI row, an unknown home height). Guarded by spawnMu.
	engineSaid map[string]bool

	// hateRefused is the ONCE flag behind logHateRefused: the first script call that
	// names a handle this zone does not have says so, later ones are silent.
	// Guarded by spawnMu.
	hateRefused bool

	// hpSourceWarned is the ONCE flag behind logMaxHPUnavailable (zone/monster_hp.go):
	// the first spawn that could not read a maximum HP says why, later ones are
	// silent. Guarded by spawnMu.
	hpSourceWarned bool

	// corpseShapeWarned is the ONCE-per-class flag behind logCorpseShapeOnce
	// (zone/respawn.go): the first kill of a class whose monster row names no shape
	// row the shape table answers says why the corpse uses the floor, later kills of
	// that class are silent. Guarded by spawnMu.
	corpseShapeWarned map[uint32]bool

	// noRespawnWarned is the ONCE-per-class flag behind logNoRespawnLocked
	// (zone/respawn.go): the first kill of a class whose body has NO spawner row and
	// no `-respawn` flag says that it will not come back, later kills of that class
	// are silent. Guarded by spawnMu.
	noRespawnWarned map[uint32]bool

	// corpses is every body this zone killed that still owes the world something: the
	// 3531 that takes the corpse off the field once its shape's death animation has had
	// its time (max(DyingTime, CorpseLeaveFloor) after the ZC_DEAD), and - when the body
	// has a spawner row - the NEW body that comes back at its point on that row's
	// `RespawnTime` (zone/respawn.go). Armed by the same kill that sends the ZC_DEAD and
	// emptied by corpseTick, so a dead handle leaves once and a body returns once per
	// death. Guarded by spawnMu, which it shares with the roster it edits.
	corpses []corpse

	// monsterSwingAt is the LAST swing instant of each monster, by handle - the clock
	// the monster's own cadence is measured against (zone\monster_swing.go). The key is
	// absent until a body's first swing, which is due at once, exactly as the reference
	// swings on entering its attack state.
	monsterSwingAt map[uint32]time.Time

	// swingMu guards monsterSwingAt AND the victim's HP read-modify-write inside one
	// swing. They are ONE lock on purpose: the claim (may this body swing now?) and the
	// HP move it justifies must not be split by a second session's tick, or two
	// monsters could both stamp the same body in the same instant.
	swingMu sync.Mutex

	// THE MONSTER CHASE (zone\monster_chase.go). chaseMu is the STEP's lock: a body's
	// position, its target choice and its move order move together, or a second
	// session's tick could re-plan from a point the first one has already left.
	// monsterChaseRan is the ZONE's clock - one step per pollInterval, not one per
	// session - and monsterChases is the engagement of each body by handle. The map
	// holds only bodies with a live target or a walk home in flight; an idle body has
	// no entry and costs nothing.
	//
	// chaseNoPlanLogged keeps the "this zone has no path engine" line to one per run:
	// the condition is a property of the build, not of each body.
	chaseMu           sync.Mutex
	monsterChaseRan   time.Time
	monsterChases     map[uint32]*monsterChaseState
	chaseNoPlanLogged atomic.Uint32

	// expMu guards the two experience ledgers below (R98 slice 2). They are the
	// zone's answer to "how much has this character been paid" and "which one-time
	// lines have already been printed": a kill can arrive on any session's
	// goroutine, so both are read and written under this lock and nothing else.
	// The ZERO VALUE is ready to use, and the maps are made on first write.
	expMu     sync.Mutex
	expTotals map[uint32]int64 // character handle -> total exp credited this session
	expSaid   map[string]bool  // one-time log lines already printed

	// spMu guards spSaid, the SP regeneration lane's one-time lines (zone\sp_regen.go):
	// a tick runs on every session's goroutine, so the "already said it" map needs the
	// same lock expSaid has. The zero value is ready; the map is made on first write.
	spMu   sync.Mutex
	spSaid map[string]bool
}

// rawHex renders at most n bytes of b as hex, for the RawLog dump. A chunk the
// splitter cannot finish is the case this exists for, so the head is what
// matters and the rest is only counted.
func rawHex(b []byte, n int) string {
	if len(b) > n {
		return hex.EncodeToString(b[:n]) + "...(+" + fmt.Sprint(len(b)-n) + ")"
	}
	return hex.EncodeToString(b)
}

// Serve runs one connection until it closes.
func (h *Handler) Serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	h.online.Add(1)
	defer h.online.Add(-1)
	if h.Log == nil {
		h.Log = log.Default()
	}

	// A panic in ONE connection must not kill the zone. Without this the whole
	// process dies (main calls Serve in a goroutine, and an unrecovered panic in
	// any goroutine ends the program), the client just disconnects, and the log
	// stops mid-sentence. The stack goes to h.Log so it lands in the -log FILE:
	// a plain panic writes to stderr, which is not captured there.
	defer func() {
		if r := recover(); r != nil {
			h.Log.Printf("zone: PANIC while serving %s: %v\n%s",
				conn.RemoteAddr(), r, debug.Stack())
		}
	}()

	// The script host holds ONE suspended click for the whole server, so this
	// session's menu must be released when the session ends - a client that
	// disconnects while a dialog is open would otherwise lock every NPC until the
	// zone restarted (live log, 2026-10-26). Deferred AFTER the recover so a panic
	// on the way out still reaches it.
	if h.ReleaseClick != nil {
		defer h.ReleaseClick()
	}

	h.Log.Printf("zone: CONNECT from %s", conn.RemoteAddr())

	sess := &Session{skipped: map[uint16]int{}, Conn: conn}

	// THIS SESSION'S WRITER (zone\writer.go). One goroutine per session, one queue,
	// started BEFORE anything can be sent to it and stopped on the way out - so no
	// goroutine outlives its session and a closed socket never gets a late packet.
	// `-simlatency` is not a second path: it is a delay applied inside this writer.
	if h.startWriter(sess) {
		defer sess.stopWriter()
	}

	// The player is leaving: take them out of the roster and tell the others, so
	// their client removes the character (Python zone_server.py:4987-4995). This
	// runs for a broken connection too, which is the common case.
	defer h.leaveZone(sess)
	defer h.forgetLiveSession(sess)

	// WHERE THE LEADER STOOD WHEN THEY LEFT (zone\last_point.go). Deferred AFTER
	// leaveZone, so it runs BEFORE it - LIFO - while the session still has its team and
	// its last walk segment. A broken connection is the COMMON way out of a zone, and
	// this defer is the only place it is caught: CZ_SYSMENU never arrives when the
	// client is killed, and the map change has its own call (Handler.acceptMove).
	defer h.noteLastPoint(sess, lastPointDisconnect)

	var buf []byte
	read := make([]byte, 64*1024)

	for {
		// THE ZONE'S OWN CLOCK RUNS HERE, AT THE TOP OF EVERY PASS (R95 item 4), before
		// the read and whether or not the read is about to time out.
		//
		// THE MEASURED REASON IT MOVED: a client that sends frames faster than the 100 ms
		// poll resets the read deadline on every iteration, so the read never times out -
		// and the old tick, reached only on a read ERROR, never ran at all. The swing
		// repeat, a cast's wind-up and a walk's stop then all waited for a packet that,
		// per attack_swing.go's header, never comes until the player clicks again.
		//
		// THE CLOCK IS NEVER FASTER THAN pollInterval: tickIfDue answers false unless a
		// full pollInterval has passed since Session.lastTick, and lastTick is advanced
		// only when the tick really runs.
		if out, ran := h.tickIfDue(sess); ran {
			if werr := h.writeSession(sess, out); werr != nil {
				h.Log.Printf("zone: chase write failed: %v", werr)
				return
			}
		}

		// While a chase is running, a click is waiting, or a walk the PLAYER drove is
		// still in flight, the read must WAKE UP: the client sends nothing while it
		// walks a move we sent, so a blocking read would never notice the walk is over
		// and the dialog would never open. A read deadline does this without a second
		// goroutine writing to the same socket. A pending map change is NOT a reason to
		// wake up: 3593 is answered by the client's own ack (3686), which arrives on
		// this very socket, and there is no timeout beside it.
		//
		// THE SIGHT RING IS A REASON TOO (zone\sight.go): it is evaluated on this tick,
		// and another player walking into or out of view sends THIS client nothing that
		// would wake its read. Without this an idle session would never see anyone
		// arrive or leave - its tick would never run at all.
		//
		// THE DISTANCE MEASUREMENT IS A REASON FOR THE SAME PURPOSE: -diag-distance
		// writes its pair lines from this tick, so a player standing still with
		// nothing to walk or cast must still wake up or the record has a hole in it.
		if h.sessionClockAwake(sess) {
			_ = conn.SetReadDeadline(time.Now().Add(pollInterval))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}

		n, err := conn.Read(read)
		if n > 0 {
			buf = append(buf, read[:n]...)
			chunks, used := protocol.SplitWire(buf)
			buf = buf[used:]

			for _, ch := range chunks {
				plain, derr := h.Cipher.DecryptChunk(ch.Data)
				if derr != nil {
					h.Log.Printf("zone: chunk cannot be decrypted: %v", derr)
					continue
				}
				if h.RawLog {
					h.Log.Printf("zone: RAW chunk %d bytes: %s", len(plain), rawHex(plain, 64))
				}
				frames, _ := h.Table.Split(plain)
				if h.RawLog {
					cmds := make([]string, 0, len(frames))
					for _, f := range frames {
						cmds = append(cmds, fmt.Sprintf("%d/%dB", f.Cmd, f.Size))
					}
					h.Log.Printf("zone: RAW frames: %v", cmds)
				}
				for _, f := range frames {
					// S3 counters: one frame read, its whole length. Here and
					// nowhere else, so a packet is never counted twice on the way in.
					metrics.packetIn(f.Size)
					out, rerr := h.Reply(ctx, sess, f)
					if rerr != nil {
						h.Log.Printf("zone: cmd %d: %v", f.Cmd, rerr)
						continue
					}
					if werr := h.writeSession(sess, out); werr != nil {
						h.Log.Printf("zone: cmd %d: write failed: %v", f.Cmd, werr)
						return
					}
					if sess.CloseAfterAnswer {
						h.Log.Printf("zone: closing this session after cmd %d - the client asked "+
							"to go to the barrack and the switch finishes when the socket ends", f.Cmd)
						return
					}
					// The gate check runs after every packet, because the packet that
					// matters is the leader's own move report: it is what carries the
					// player into a gate. Put here rather than in movePC so a position
					// that arrives any other way is checked too.
					if werr := h.writeSession(sess, h.gateTick(sess)); werr != nil {
						h.Log.Printf("zone: gate write failed: %v", werr)
						return
					}
				}
			}
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// THE POLL FIRED, and there is nothing left to do here (R95 item 4): the
				// tick at the TOP of this pass is what advances the walk, the cast, the
				// repeat and the gates now. A read timeout is itself the proof that a
				// full pollInterval has passed, which is exactly what tickIfDue checks -
				// so going round again runs it.
				continue
			}
			h.Log.Printf("zone: disconnect from %s: %v", conn.RemoteAddr(), err)
			return
		}
	}
}

// Reply turns one client frame into the packets to send back.
func (h *Handler) Reply(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	// S3 counters: the handler time, in a defer, so a reply that returns an error
	// is measured too. time.Now is the only cost and it allocates nothing.
	replyStart := time.Now()
	defer func() {
		metrics.handlerMicros(uint64(time.Since(replyStart).Microseconds()))
	}()
	switch f.Cmd {
	case CmdCZStartGame:
		return h.startGame(ctx, sess, f)
	case CmdCZGameReady:
		h.Log.Printf("zone: GAME_READY - world entry handshake (%d team member(s))",
			len(sess.Team))
		// The handshake that ends the loading screen, then the town: the client
		// only draws NPCs after it has the world, so they go out here and not with
		// the entry burst.
		out := [][]byte{BuildEnterLegacyObjects()}
		// The 3684 status burst rides HERE, and not with CZ_START_GAME where it
		// used to be. The client's 3684 handler exits silently on a record-count
		// gate that is still 0 during the START_GAME reply, so every packet sent
		// there was dropped with no log line: the six BASE stats (which travel in
		// START_INFO) rendered and every DERIVED field read 0 while the zone's own
		// log printed real numbers. By GAME_READY the record exists, and the client
		// keeps the sheet for the whole session. See history\zone-entry.md.
		out = append(out, h.statusBurst(sess)...)
		if len(h.NPCs) > 0 {
			out = append(out, h.npcSpawnPackets(h.NPCs)...)
			h.Log.Printf("zone: spawned %d town NPC(s) for map %d",
				len(h.NPCs), h.MapID)
		} else {
			h.Log.Printf("zone: no town NPCs loaded - the town will be empty")
		}
		// The NPCs and monsters a SCRIPT made before this player arrived
		// (zone\create.go). They are not in the map's data file - the Scarecrow's
		// four dummies come from `SCR_NPC_HITDUMMYMANAGER_TS_NONE`, which runs once
		// at zone start - so a player who enters afterwards has to be told about
		// them here or walks past objects that are not there for him.
		//
		// THE MONSTERS AMONG THEM OBEY THE SIGHT RING NOW, so they are announced by
		// joinZone (zone\sight.go) with the players: a body further away than the
		// map's ViewRange is not sent, and the ring remembers what it did send. The
		// NPCs stay unfiltered - the item/trigger/NPC burst is a later slice.
		if made := h.runtimeNPCs(); len(made) > 0 {
			out = append(out, h.npcSpawnPackets(made)...)
			h.Log.Printf("zone: %d NPC(s) a script created are sent to this player too",
				len(made))
		}
		// The doors' own arrows, AFTER the NPCs (they are objects too, and for the same
		// reason: the client draws nothing it was not told about). Without 3540 the
		// client has no warp point at the exit at all, and the 3541 that follows a step
		// into one would name a handle it never saw.
		out = append(out, h.spawnWarpPoints()...)
		// SLICE S1: ONE standing monster, at a fixed spot, using the client's own
		// 360-byte ZC_ENTER_STANDINGMONSTER (3527). It rides HERE and not the entry
		// burst for the reason the NPCs do - the client draws no actor before it has
		// the world - and it is sent once per session, never on a move or a tick: in
		// S1 the monster stands still. `-spawnmonster` is OFF by default, so this call
		// returns nil and the wire is unchanged for every other map (zone\monster.go).
		out = append(out, h.spawnDevMonster()...)
		// The STANCE column of that window, ONE ZC_STANCELIST (3626) per character,
		// and it goes out FIRST - the entry burst's order is measured: in the live
		// capture every 3626 is immediately followed by that same handle's 3621
		// (69172 -> 69308, 70306 -> 70680, 72780 -> 72972). It has to come first
		// because the client's skill-icon test needs the stance row unlocked as well
		// as the skill row (UISkillListBox.cpp:170), and only this packet clears the
		// stance row's flag (GeSession.cpp:440-455). Until 2026-10-31 the zone never
		// sent it at all, which greyed EVERY stance in the window - the owner's
		// "some of my stance are locked".
		out = append(out, h.stanceListBurst(sess)...)
		// The skill bar, ONE ZC_SKILLLIST (3621) per character. It rides HERE, at the
		// head of the world pass and immediately before h.joinZone, because that is the
		// live order: in four live entry captures every 3621 lands in the entry burst
		// BEFORE the first 3520 spawn and before its 3572. In
		// captures\pcdialogs\20261022-213904\s2c_7054_55316.bin the 3621 is packet 63
		// of 1502 at stream offset 56636 (handle 188005), while that same handle's 3520
		// is packet 83 at offset 58710 and its 3572 packet 84 at 59415.
		//
		// GAME_READY is also LATE ENOUGH for the packet to do anything: the client's
		// 3621 handler only clears `isLocked` on skill entries that ALREADY exist, and
		// those were created by the START_INFO skill block at CZ_START_GAME. Sending it
		// earlier would set a flag on nothing.
		//
		// WHAT THIS ANSWER ACTUALLY CONTAINS, since it is easy to misread: the spawns in
		// h.joinZone's part are the OTHER players' characters - this session's own team
		// went to it with START_INFO - so this 3621 precedes the spawns that arrive in
		// the same answer and follows the entries it unlocks.
		out = append(out, h.skillListBurst(sess)...)
		// Now that the world is up, tell everybody about everybody, so the players
		// can SEE each other (Python zone_server.py:4400-4408: add_member, then
		// announce_players). AFTER the NPCs, because the client only draws actors
		// once it has the world.
		out = append(out, h.joinZone(sess)...)
		return out, nil
	case CmdCZMovePC:
		return h.movePC(sess, f)
	// WHICH CHARACTER THE PLAYER DRIVES (3652). It answers nothing and spawns
	// nothing: it moves every "the player's character" decision from slot 0 to the
	// slot the client names. Before this case existed the frame fell into the
	// default branch and was logged `cmd 3652 not handled yet (first seen)`
	// (docker-port\logs\go_zone1331.log, 2026-11-01 10:28:02).
	case CmdCZSelectChangePC:
		return h.selectChangePC(sess, f)
	case CmdCZMoveZoneOK:
		return h.gateReady(ctx, sess, f)
	case CmdCZReqTownMove:
		return h.townMove(sess, f)
	case CmdCZClickTrigger:
		return h.clickTrigger(sess, f)
	case CmdCZDialogSelect, CmdCZAckDialog:
		return h.endDialog(sess, f)
	case CmdCZBuyItem:
		return h.buyItem(ctx, sess, f)
	case CmdCZSellItem:
		return h.sellItem(ctx, sess, f)
	case CmdCZEquip:
		return h.equip(ctx, sess, f)
	case CmdCZUnequip:
		return h.unequip(ctx, sess, f)
	// The stance key. The client sends 3625 when the player presses it, the zone
	// checks the id against the character's job AND the weapons it holds (the rule
	// the client applies itself, GeSession.cpp:487-520), and answers 3554 to the
	// owner and to every other player in the zone - see zone\stance_change.go.
	case CmdCZChangeStance:
		return h.changeStance(sess, f)
	// The skill cast: 3646 (aimed at a monster) and 3645 (a cast with no target).
	// The zone checks the character, the stance it stands in and the target, then
	// answers ZC_SKILL_CASTING (3571) and ZC_SKILL_MELEE (3573) - the wind-up and the
	// hit, in that order, which is the one the live server sends
	// (history\combat.md §9.2). See zone\skill_cast.go.
	case CmdCZSkillTarget, CmdCZSkill:
		return h.castSkill(sess, f)
	case CmdCZSkillGround:
		return h.castSkillGround(sess, f)
	// The player cancelled the cast that is winding up. The client's own table
	// registers 3650 CZ_SKILL_CANCEL at 16 bytes (`data\cmd_table_live_z.txt:1317`),
	// though it sent none in the 77 s recording this lane was measured on. The pending
	// cast is dropped and NOTHING is sent - the one cancel the protocol really has,
	// beside the moves that do not cancel anything (zone\skill_cast_clock.go).
	case CmdCZSkillCancel:
		return h.skillCancel(sess, f)
	// The PLAIN ATTACK: the player left-clicked a monster. 3666 carries the acting
	// character and the target handle; the answer is 3564 ZC_ATTACK_SINGLE (the hit,
	// with the damage as a float64 and the target's HP BEFORE it) followed by 3557
	// ZC_ADD_HP (the same handle, the HP AFTER, the same double as the delta), and a
	// refused attack is answered with 3568 ZC_ATTACK_DISABLE so the client switches
	// the attack off instead of waiting. Before this case the click fell into the
	// default branch and logged `cmd 3666 not handled yet`. See zone\attack.go.
	case CmdCZAttack:
		return h.attack(sess, f)
	// THE PICKUP: 3611 CZ_PICKITEM names a character and a ground handle; the zone
	// resolves the handle, checks the reach and the claim, then answers the picker
	// (3555 + the 3607 bag row) and every viewer inside the middle radius (3539).
	// See zone\pickup.go.
	case CmdCZPickItem:
		return h.pickItem(ctx, sess, f)
	// THE OTHER TWO BAG VERBS. 3612 CZ_DROP takes N out of one named row and puts ONE
	// ground item of N on the floor (a count below the stack IS the client's split - the
	// registry has no move/swap message at all); 3651 CZ_USEITEM_TARGET takes exactly ONE
	// and announces the class with 3613. Both write through the SAME `h.Commit` a pickup
	// uses and both refuse with one log line and NO packet. Before these cases existed the
	// two frames fell into the default branch and the inventory window's own buttons did
	// nothing. See zone\bag_verbs.go.
	case CmdCZDrop:
		return h.dropItem(ctx, sess, f)
	case CmdCZUseItemTarget:
		return h.useItemTarget(ctx, sess, f)
	case CmdCZChat:
		return h.chat(ctx, sess, f)
	case CmdCZWaypointSelect:
		return h.waypointSelect(sess, f)
	// The system menu (3595): go to barrack, log out, go to the lobby zone. It is the
	// log-out REQUEST, so it is also the cleanest way to know a player is leaving the
	// map - the last point is written for every selection, and only LOGOUT is answered
	// (ZC_LOGOUT_OK 3784, after which the client closes its own socket). Before this
	// case existed, 3595 fell into the default branch and was logged as "not handled
	// yet (first seen)" (docker-port\logs\zone10_err.txt:513-514).
	case CmdCZSysMenu:
		return h.sysMenu(ctx, sess, f)
	// The grid's SAVE button, "Save as Start Point": it writes the town the player
	// stands in as the entry point the barrack will name next time, and answers with
	// 3467 so the window shows it (start_point.go).
	case CmdCZSaveRestartWarpID:
		return h.saveRestartWarp(ctx, sess, f)
	// The player's own saved points - the `teleportation` window's five ids. All
	// four requests are answered in saved_teleport.go; the store they read and write
	// is `saved_teleport` in PostgreSQL.
	case CmdCZTeleportList:
		return h.teleportRequest(ctx, sess, f)
	case CmdCZTeleportSave:
		return h.teleportSave(ctx, sess, f)
	case CmdCZTeleportRemove:
		return h.teleportRemove(ctx, sess, f)
	case CmdCZTeleport:
		return h.teleportTravel(ctx, sess, f)
	// The rest of the grid's requests, LOGGED AND NOT ANSWERED - they are not this
	// slice. Each one names its bytes in the log so a later slice starts from what the
	// client really sent instead of from the struct.
	//
	// WHICH IDS ARRIVE (client source, not a guess):
	//   3743 CZ_WAYPOINT_SELECT       - a click on a town thumbnail. ANSWERED now, see
	//        Handler.waypointSelect (waypoint_travel.go).
	//   3468 CZ_SAVE_RESTART_WARPID   - the SAVE button, "Save as Start Point":
	//        game.scp:381 calls `SaveRestartWarpID()`, not CZ_COMMWARP_SAVE. ANSWERED
	//        now, see Handler.saveRestartWarp (start_point.go) - it is NOT in the
	//        list below any more.
	//   3637 is the SAVED-TELEPORT window's GO button (travel): ANSWERED now, see
	//        Handler.commWarpSelect (commwarp.go) - it resolves the row, charges ONE
	//        Warp Scroll (21006, or 21446 when the client says it is spending the event
	//        one) and moves the player. 3635 SAVE, 3636 REMOVE, 3638 RENAME and the
	//        Up/Down SWAP (3639) are answered there too and are no longer here.
	//        3639 DOES arrive: an earlier note in this file said this client had no
	//        up/down control, and the owner's clicks disproved it on 2026-10-30
	//        (logs\zone15_live.log 15:07:46, 19 bytes, rows 4 -> 3).
	case CmdCZCommWarpSelect:
		return h.commWarpSelect(ctx, sess, f)
	// The Warp List's four writes. The client has no receive handler for any of
	// them, so the answer is not a reply packet: the row is written (or moved) and
	// the four 3634 lists go back out, which is what redraws the window (commwarp.go).
	case CmdCZCommWarpSave:
		return h.commWarpSave(ctx, sess, f)
	case CmdCZCommWarpRemove:
		return h.commWarpRemove(ctx, sess, f)
	case CmdCZCommWarpRename:
		return h.commWarpRename(ctx, sess, f)
	case CmdCZCommWarpSwap:
		return h.commWarpSwap(ctx, sess, f)
	default:
		// countSkipped initialises the map on demand: a test builds a bare
		// Session, and a write to a nil map panics just as hard as the shared one
		// this replaced.
		count := sess.countSkipped(f.Cmd)
		if count == 1 {
			// Log the bytes the FIRST time a command appears. A command id the
			// table does not know makes the splitter swallow the rest of its chunk
			// (protocol\split.go), so the only way to fix the table is to see what
			// arrived. Cheap, and it turns a mystery into one line of hex.
			h.Log.Printf("zone: cmd %d not handled yet (first seen), %d bytes: %s",
				f.Cmd, f.Size, hexHead(f.Data, 32))
		} else {
			h.Log.Printf("zone: cmd %d not handled yet (skipped %d time(s))",
				f.Cmd, count)
		}
		return nil, nil
	}
}

// countSkipped adds one to this session's count for a command and returns the
// NEW count, so the caller can tell "first seen" (1) from "seen again".
//
// The map is created on first use because a bare &Session{} is a legitimate
// state in tests, and a write to a nil map panics.
func (s *Session) countSkipped(cmd uint16) int {
	if s.skipped == nil {
		s.skipped = map[uint16]int{}
	}
	s.skipped[cmd]++
	return s.skipped[cmd]
}

// noteChat records that this session has now shown a chat text, and reports
// whether it is the FIRST time. Per session for the same reason `skipped` is: the
// Handler is shared by every connection goroutine, so a map on it would be a
// concurrent map write.
//
// The key is lowercased and trimmed, so "Hello" and " hello" count as the same
// line. That is deliberate - the point is to keep the log readable, not to be an
// exact record of what was typed.
func (s *Session) noteChat(text string) bool {
	if s.chatSeen == nil {
		s.chatSeen = map[string]bool{}
	}
	key := strings.ToLower(strings.TrimSpace(text))
	if s.chatSeen[key] {
		return false
	}
	s.chatSeen[key] = true
	return true
}

// ParseChatText reads the text of CZ_CHAT (3888):
//
//	cmd(2) seq(4) checksum(4) len@+10(2) BalloonColor(4) ChatType(1) text(NUL) from +17
//
// The NUL ends the text. A packet with NO NUL is tolerated - the text is then
// everything to the end of the frame - because a truncated chat line must never be
// able to read past the buffer. Too short to hold a text reads as "".
func ParseChatText(data []byte) string {
	if len(data) <= chatTextOffset {
		return ""
	}
	body := data[chatTextOffset:]
	if i := bytes.IndexByte(body, 0); i >= 0 {
		body = body[:i]
	}
	return string(body)
}

// chat answers CZ_CHAT (3888).
//
// WHY THIS IS NOT A CHAT FEATURE: the client's toolbar "Feso Shop" button sends no
// shop packet. Its UI script (uixml\cashshopsub.xml -> uiscp\game.scp
// `UI_OPEN_FESO_SHOP`) closes the menu and calls Chat("/fesoshop"), which arrives
// as an ordinary chat line - the window exists only because the SERVER answers it.
// Before this, the zone logged "cmd 3888 not handled yet" and the button did
// nothing.
//
// The answer is ONE 40-byte ZC_DIALOG_NPC_TRADE (3680) with the FESO coin, exactly
// as the retail server sent it: nothing before it, and the player closes it with
// the ordinary CZ_ACK_DIALOG -> ZC_DIALOG_CLOSE we already do.
//
// EVERY OTHER TEXT IS LEFT ALONE: no echo, no relay, no invented reply. It gets
// one log line per distinct text (Session.noteChat) and nothing else.
//
// A TEXT WHOSE FIRST TWO NON-SPACE BYTES ARE `//` IS A COMMAND (zone\gm.go): it is
// dispatched before any chat branch and is NEVER broadcast. The `//` prefix is the
// client's own: `datatable_cheatlist.xml` (EUC-KR) holds every entry as
// `Chat("//cmd args")`, e.g. `Chat("//hpup 1000000")`.
func (h *Handler) chat(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	text := ParseChatText(f.Data)
	if text == "" {
		// No readable text at all: an empty line is not chat either, and it is
		// rate-limited the same way so a malformed client cannot flood the log.
		if sess.noteChat("") {
			h.Log.Printf("zone: CHAT is %d bytes with no readable text - ignored",
				len(f.Data))
		}
		return nil, nil
	}
	if isGmCommand(text) {
		// A command is NEVER chat: it is dispatched (or refused) here, before the
		// Feso Shop and the dev-message branches, so no `//` line can ever fall
		// through to "chat is not relayed yet" and be silently swallowed.
		return h.gmCommand(ctx, sess, text), nil
	}
	if args, isDev := devMessageArgs(text); isDev {
		// DEV ONLY (zone\dev_message.go): one ZC_MESSAGE carrying the type and level
		// a person typed, so the client's own sentence can name an unmeasured type.
		// It is answered BEFORE the Feso Shop line so the two can never shadow each
		// other, and it touches no state at all.
		return h.devMessage(args), nil
	}
	if !strings.EqualFold(strings.TrimSpace(text), fesoShopChat) {
		if sess.noteChat(text) {
			h.Log.Printf("zone: CHAT %q ignored - chat is not relayed yet", text)
		}
		return nil, nil
	}

	// The stock is the client's own table, but the INDEX a buy names is a position
	// in OUR list, so the list has to be remembered with the window. Without rows
	// nothing is sent: an empty window would open and then refuse every buy.
	items := h.ShopGroups.Items(fesoShopGroup)
	if len(items) == 0 {
		h.Log.Printf("zone: %s asked for the %q shop but no rows are loaded for it "+
			"(data\\shop_groups.json, built by src\\emit_shop_data.py) - sending "+
			"nothing rather than an empty window", fesoShopChat, fesoShopGroup)
		return nil, nil
	}
	sess.LastShop = &ShopState{Group: fesoShopGroup, Kind: WindowShop, Coin: CoinFeso,
		Items: items}
	// handle 0 on purpose: the window belongs to no NPC (the player pressed a
	// toolbar button) and the wire format carries no handle anyway.
	window := h.tradeDialog(0, fesoShopGroup, CoinFeso, "chat "+fesoShopChat)
	h.Log.Printf("zone: CHAT %s -> TRADE window, group %q, coin %s, %d item(s) "+
		"(ZC_DIALOG_NPC_TRADE, 3680, %d bytes)", fesoShopChat, fesoShopGroup, CoinFeso,
		len(items), len(window))
	return [][]byte{window}, nil
}

// movePC answers one CZ_MOVE_PC by echoing the same move back.
//
// The echo is sent for whichever character moved, not just the leader: the client
// moves a follower because it RECEIVES ZC_MOVE_PC for that handle, so a
// leader-only echo leaves members 2 and 3 standing still.
func (h *Handler) movePC(sess *Session, f protocol.Frame) ([][]byte, error) {
	idx, pos, ok := ParseMovePC(f.Data)
	if !ok {
		h.Log.Printf("zone: MOVE_PC is %d bytes, too short to carry a position - ignored",
			len(f.Data))
		return nil, nil
	}

	// One clock for the whole report: the walk segment written below and the speed
	// sample must agree, or the segment starts at a point the sample never saw.
	now := time.Now()

	// The LEADER's report is what says anything about the player's intent: the followers
	// stream move reports of their own the whole time (they are dragged along), and
	// treating those as "the player took control" cancelled every walk 2-3 seconds in -
	// measured 2026-10-25, the report that did it was `char 2`.
	//
	// SENSE: "the character the player drives", so the slot comes from
	// CZ_SELECTCHANGE_PC (zone\control.go). It is 0 until the client says otherwise,
	// which is exactly what this line did before that frame was handled.
	if idx == sess.Control {
		sess.LastMoveAt = now
	}
	if idx < 0 || idx >= len(sess.Team) {
		// A move for a character this session never spawned: echoing a guess would
		// teleport somebody else's actor, so it is dropped and counted.
		h.Log.Printf("zone: MOVE_PC char %d is not in this team (%d member(s)) - not echoed",
			idx, len(sess.Team))
		return nil, nil
	}

	handle := sess.Team[idx].Handle

	// THE CLIENT'S CELL IS NOT THE MESH'S CELL (2026-11-09). A report's third word is
	// the client's own cell; the path engine only accepts the mesh's triangle for that
	// (x, y), and a foreign one makes it answer "no route" - which was the chase's
	// hit-and-miss. Adopt the mesh's cell here, once, before any bookkeeping. A point the
	// mesh does not have is left as sent (a player's own move cannot be refused; R130's
	// line below names it).
	if h.NavNear != nil {
		if at, _, nearOK := h.NavNear(pos, NearRangeXY, NearRangeZ); nearOK &&
			at[2] != pos[2] && flatDist(at, pos) <= 50 {
			h.noteCellFix(pos[2], at[2])
			pos[2] = at[2]
		}
	}

	// THE CASTER'S OWN MOVE, WHILE ITS CAST OR ITS HOLD IS RUNNING: QUEUED, not
	// echoed, not a cancel, and no stop.
	//
	// This is the correction `history\cancel-and-animation.md` §6.4 made to §3. The
	// reference's `DispatchMove()` returns early on `IsHoldAction()` without clearing
	// the request (`PlayerChar_Request.cpp:343-348`), and the capture shows the same
	// thing byte for byte: between the `3571` and its `3581` the server echoed the
	// TWO TEAMMATES only, never the caster's handle, and the first caster echo
	// afterwards carried the LAST of the three clicks it had sent. A teammate keeps
	// the ordinary echo below - it is not the one holding.
	//
	// Placed BEFORE any position bookkeeping, so a queued move does not open a walk
	// segment or move the stored position: the body stands still until the hold ends.
	if c := h.castHolding(sess, idx, now); c != nil {
		h.queueCastMove(c, f, pos)
		h.Log.Printf("zone: MOVE_PC char %d handle %d -> (%d, %d, cell %d) arrived while "+
			"its own skill %d is holding (result %s, hold ends in %s) - QUEUED, nothing "+
			"echoed, nothing cancelled: the reference queues it too "+
			"(PlayerChar_Request.cpp:343-348, IsHoldAction) - move queued until the hold ends",
			idx, handle, pos[0], pos[1], pos[2], c.skill,
			time.Until(c.due).Round(time.Millisecond),
			time.Until(c.holdUntil).Round(time.Millisecond))
		return nil, nil
	}

	// THE MOVE GUARD (zone\move_guard.go) IS AN ALARM, NOT A CLAMP: a destination more
	// than MoveGuardAlarmDistance units from where the model last had this body writes
	// one log line, and the point is used EXACTLY as the client sent it. A
	// CZ_MOVE_PC carries a DESTINATION chosen by the client's own pathfinding, not a
	// position, so bounding it by distance over time rewrote points the client never
	// reported - it fired 50 times in 100 seconds of ordinary play
	// (logs\go_zone1331.log, 2026-11-01 15:23). Proving where a body may be is the
	// parked server-authoritative-movement item, not this path's job.
	//
	// It sits here, before the bookkeeping below, because the alarm compares the report
	// against the body the server is really drawing.
	spd := h.packetSpeed(&sess.Team[idx])
	stand := sess.drawnPos(idx, now, spd)
	pos = h.guardMoveReport(idx, pos, stand)

	// A POSITION UPDATE IS A REACH UPDATE FOR A HELD ATTACK PRESS (R100), and it is asked
	// BEFORE the cancel calls below on purpose: this report may be the client saying the
	// body is now inside the stance's reach, and the answer to that is the hit - not a
	// cancellation of the very press it just satisfied. A report that does NOT put the body
	// in reach is still what it always was (the player took the character somewhere else),
	// and the cancels below run exactly as they did.
	//
	// THE POINT IS NOT `pos` (R104). CZ_MOVE_PC names a DESTINATION (move.go, openWalk),
	// so measuring the reach at it measured a point the body had not walked to - and the
	// swing then hard-set the client's actor to it: `pos (2392, -770) from segment (2605,
	// -1299) -> (2997, -1170)` in logs\go_zone1331.log at 19:13:21.181 is that report's
	// destination, ~600 units from the body. The fire reads the DRAWN body itself.
	held := h.fireHeldAttackOnArrival(sess, idx, now)

	// MID FLIGHT: STILL WALKING TO ITS CAST, SO THE CLICK CANCELS IT (R89).
	//
	// The hold above was asked FIRST on purpose: a cast that has already sent its 3571
	// is animating by definition, and the owner's rule is that the animation is never
	// interrupted. This branch is the OTHER half - the approach walk - and it drops the
	// whole cast, sends nothing for it, and then lets this function carry on with the
	// ordinary move below, which is the same one-continuous-move path every click to
	// walk uses (the echo, the broadcast, the segment, and the stop at its end).
	// The cancel logs its own line; the move is handled below exactly as if the press
	// had never happened, which is what "the character walks to the clicked point" means.
	h.cancelApproachCast(sess, idx)
	// AND THE SAME CLICK CANCELS A NORMAL ATTACK'S APPROACH (R90 item 4), with the same
	// rule and the same consequence: nothing is sent for the attack and the click walks
	// the character where the player pointed. Only one of the two can be pending, so
	// the two calls cannot fight.
	h.cancelApproachAttack(sess, idx)
	// AND A PICKUP CHASE IS CANCELLED BY THE SAME CLICK (2026-11-08). An out-of-reach
	// CZ_PICKITEM now walks the character to the item (zone\pickup.go), and the player's
	// own move order is exactly what ends an attack's approach - one rule, one click. It
	// logs its own line and sends nothing; the move below carries on as an ordinary walk.
	h.cancelApproachPick(sess, idx)

	// A MOVE ALONE DOES NOT END THE CHARACTER'S SWING REPEAT - AN ORDER DOES (R105, R108).
	// The two are told apart further down by WHERE the report points, not by how far it
	// goes: a walk this zone sent, coming back, keeps the engagement; any other report
	// from that character is the player's own order for it and ends it after the swing
	// already due has landed (Session.walkWasOurs). EVERY slot reports its own moves, so
	// this is asked of the slot the report names and not only of the driven one. The
	// reference keeps the attack as an
	// ACTION with a duration and re-decides after it; nothing in FieldChar_FSM.cpp /
	// PlayerChar.cpp aborts the attack action mid-swing on a move.
	//
	// WHAT STILL ENDS THE REPEAT is listed in zone\attack_swing.go's header, and every
	// one of those checks lives in swingTick (the target dying or leaving the zone, the
	// stance changing, the sheet losing its cadence, a player move order) or in
	// attackAt's reach check - a body that walks out of reach has its NEXT swing refused
	// by the reach, with its own line, instead of being cancelled mid-swing. Nothing
	// about the move above changed: the echo, the broadcast, the segment and the stop are
	// all still sent below.

	sess.LastPos = pos
	sess.LastMover = handle
	if idx == sess.Control {
		// SENSE: the speed sample is the CONTROLLED character's - it is the one the
		// player walks, so it is the only stream that measures the player's intent.
		// Only the leader tells this side anything about its own walking: the followers
		// are dragged along and their reports say nothing about the player's intent.
		// BEFORE the segment below, because a fresh speed sample changes where walkedPos
		// says the character is.
		sess.observeSpeed(idx, pos, now)
	}
	// THE LEG STARTS WHERE OUR OWN BODY IS, AND IT IS A ROUTE (2026-11-02).
	//
	// The reference does exactly this: MS_MOVE's ON_ENTER sets m_moveStartPos = m_pos
	// (FieldChar_FSM.cpp:246-270) and the per-tick update advances its own path agent by
	// m_moveSpeed x dt (FieldChar.cpp:240-260) - the server never learns where the
	// client's actor is, because the move packet carries a DESTINATION, a start tick and
	// a speed (PlayerChar.cpp:232-242) and the client follows the body it is told about.
	// So there is nothing to infer: `stand` is what this zone has ALREADY DRAWN this
	// character on, read above before anything was written, and the leg begins there.
	//
	// THE OLD GUESS IS GONE WITH IT. Until today the start was a point chosen between the
	// previous leg's two ends (the rule movement piece 2a shipped), and the gap back to the
	// drawn body was paid off as a residual in a file of its own. Measured on the owner's
	// own session, 2026-11-02 18:22:04.412745: `-> (1964, -746) | model was at (2621, -816)
	// 963 u off` - the guess was 963 units from the body, and that number WAS the snap. A
	// leg that starts on the body cannot be 963 units from it.
	//
	// THE LEG IS PLANNED ONCE, HERE, AND NEVER PER TICK: the polyline from `stand` to the
	// destination the client named, by the same planner the chase uses (clientRoute ->
	// Handler.Route -> the map's own mesh). Every later reading is arithmetic along that
	// polyline (walkedRouteAt), so a bent walk is drawn on the ground it was timed on.
	legFrom, legDest := stand, pos
	var route [][3]int32
	opening := !sess.reportRepeats(idx, pos)
	if opening {
		// ONCE PER LEG, HERE AND NOWHERE ELSE: the polyline from the body's own point to
		// the destination the report named, by the map's own planner.
		route = h.clientRoute(legFrom, legDest)
	} else if leg := sess.clientLegOf(idx, spd); !leg.placed {
		// A REPEATED DESTINATION OPENS NO LEG (move.go, reportRepeats): the client repeats
		// its destination per path step, and restarting the leg on every repeat would push
		// its end back for as long as the player walks. Nothing is planned and nothing is
		// written; the line below then describes the leg the report NAMES - the one still
		// open - so its numbers are never about a leg that does not exist.
		legFrom, legDest, route = leg.from, leg.dest, leg.route
	}
	legLen := 0.0
	if h.DiagEvents {
		legLen = legLength(legFrom, legDest, route)
	}

	tick := uint32(nowMillis())
	out := BuildMovePC(handle, pos, tick, float32(spd))

	// ONE LINE PER REPORT, cut down to what is now CHECKABLE: where the body was when the
	// report arrived, the length of the leg it walks (its route, and the straight
	// distance for comparison), and the speed. `u off` is gone on purpose - the leg starts
	// on the body, so the only gap it could print is zero; a second number there would be
	// a measurement of nothing.
	//
	// OFF BY DEFAULT (`-diag-events`), like the fan-out summary in zone\players.go: this is
	// ONE line per move report, and at 300 players in one town that is ~1000 lines/s on the
	// packet path. The event is still counted - METRICS `moves=` - and legLen above is
	// computed only when the line is written, because nothing else reads it. `-diag-events`
	// restores this line byte for byte. (This gate was lost once to a concurrent write on
	// 2026-11-05 and restored the same night; if the line is back without the flag, that is
	// what happened again - check `git log -p -- zone\zone.go`.)
	if h.DiagEvents {
		h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d) cell %d | body was at (%d, %d), "+
			"leg %.0f u (straight %.0f u), speed %.0f",
			idx, pos[0], pos[1], pos[2], stand[0], stand[1], legLen,
			flatDist(legFrom, legDest), spd)
	}

	// A DESTINATION THE MESH DOES NOT KNOW IS NOT GROUND (R130), said out loud and NOT
	// clamped: the client owns its own body's pathing and its own collision, and rewriting
	// its destination would echo back a point it never asked for. The reference REFUSES such
	// a move outright (CFieldChar::DispatchMove: `m_world->IsCollision(m_shape, moveTo)` ->
	// return), and this zone keeps the report; the line is what makes a stuck body
	// diagnosable instead of mysterious.
	if h.NavHeight != nil && opening {
		if _, ok := h.NavHeight(pos); !ok {
			h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d, cell %d) is NOT WALKABLE GROUND on "+
				"this map's mesh - used as sent (the reference refuses such a move: "+
				"CFieldChar::DispatchMove, m_world->IsCollision); if a body sticks here, this is "+
				"the line (R130)", idx, pos[0], pos[1], pos[2])
		}
	}

	// A MOVE FROM THE DRIVEN CHARACTER ENDS THE KEEP-MODE ENGAGEMENT - ANY MOVE, HOWEVER
	// SMALL (R108; the owner's rule, 2026-11-01 22:44).
	//
	// THE 150-UNIT LINE IS GONE, and the measurement that put it there was the wrong
	// reading of the same evidence. That line said "a background correction is a short
	// leg, so only a long one is a move order" - and the owner then clicked away for 11
	// more seconds while the server kept fighting: his legs were 213, 271 and 357 units,
	// and one 216 u leg was refused for pointing "toward the target". Both refusals were
	// invented here. Inside KeepReturnRange (2000 u, every profile in
	// ge\xml\datatable_ai.xml) the client's AI never moves the driven character by
	// itself (ge\ai\lib.scp:106), so a move report there is the player, and a move is a
	// cancel no matter how small.
	//
	// SO THE ONE THING THAT EXCUSES A REPORT IS PROVENANCE: if the report names a point on
	// a leg THIS ZONE opened - its goal, its own start, or a point along it - the report is
	// our own walk coming back and the engagement is untouched (Session.walkOurs,
	// zone\walk_once.go). "Along it" is the part that matters: the client reports one PATH
	// STEP at a time and its steps are not the point we named (R110). Everything else is a
	// move order, and the log says which of the two it decided, with the goal and the
	// distance it decided it on.
	//
	// THE REPORT ENDS THE ENGAGEMENT OF THE CHARACTER IT NAMES - EVERY SLOT, NOT ONLY THE
	// DRIVEN ONE (2026-11-02). "Only the controlled character speaks for the player" was
	// wrong, and the owner's own session says so: at 22:02:27 the client armed two repeats
	// at once, he drove slot 2 and moved, and only the character he was DRIVING stopped
	// swinging while the other one kept at it. The wire agrees with him: a team move comes
	// back as ONE MOVE_PC PER CHARACTER, not one for the leader - logs\go_zone1331.log on
	// 2026-11-02 has `MOVE_PC char 0`, `char 1` and `char 2` at 22:02, 22:05 and 22:10.
	// The reference keeps the same per-character sense: each CPlayerChar owns its own
	// action state and its own move dispatch (Commander_Packet.cpp `OnMove` ->
	// `pc->RequestMove`), and a move sets AS_NONE for THAT character only
	// (FieldChar.cpp `DispatchMove`). So a report is that slot's own order and ends that
	// slot's engagement, whether the slot is the leader or a follower.
	//
	// THE EXEMPTION DOES NOT CARE ABOUT THE SLOT EITHER: a report naming a point on a leg
	// THIS ZONE opened for that slot is our own walk coming back (walkOurs is per slot, so
	// a follower's own walk is excused by a follower's own leg and by nothing else).
	//
	// AND PROVENANCE ALONE IS NOT ENOUGH - THE LEG MUST STILL BE WALKING (2026-11-03). A
	// walk that has ARRIVED covers its own line, and above all its own START, for ever, so
	// a report on it used to be excused however little was left to walk. The owner's own
	// session is the proof: at 18:29:33.056616 char 1 reported (2198, -819) while the body
	// was at (2411, -738) - the goal of the attack approach this zone had sent - and that
	// point was the leg's own start, 228 u back. Nothing was left to advance, so the report
	// was the player clicking away; it was swallowed as "a path step", the fight was not
	// cancelled, and 200 ms later the server walked the character back. walkOurs now also
	// requires the body to be more than walkRoundSlack short of that goal and the report to
	// be CLOSER to the goal than the body already is (zone\walk_once.go, walkAdvances), and
	// the two lines below say which part refused it.
	//
	// THE ORDER OF THE TWO ANSWERS IS THE OWNER'S, AND IT IS THE REFERENCE'S (2026-11-03):
	// "can the damage go through and then initiate the walk? ... it will 9/10 let the damage
	// go through and then the character walk. essentially nothing is cut/abrupt". Measured
	// on the live server as durable fact #236: of 8 attack+move windows, 7 produced damage
	// and 5 of those put the damage BEFORE the move echo
	// (captures\longwalk\20261103-173112: move report -> damage +165/+233 ms, move echo
	// +251/+371 ms).
	//
	// SO THE SWING THAT IS DUE IS BUILT HERE, ON THE BODY AS IT WAS BEFORE THIS REPORT, AND
	// IT GOES OUT IN THIS REPLY BEFORE THE ECHO. Handler.noteMoveOrder marks the record and
	// Handler.drainMoveOrderSwing builds that one swing - which is why the leg this report
	// opens is written DOWN THE FUNCTION, AFTER this block: attackBodyPoint then still reads
	// the leg the character was walking, and the 3564's +8 is the point it was standing on
	// (the old code built it 100 ms later, on the NEW leg - 49 u the client had never
	// walked - and that is the owner's "stuck now": the handler hard-sets the actor to +8
	// and calls CMS_STOP, durable fact #219).
	//
	// A SWING THAT HAS NOT COME DUE IS NEVER BUILT AFTERWARDS: the tick only ends the
	// record (attack_swing.go, swingTick's cancelled branch). Nothing is deleted here, so a
	// hit already on the wire is neither re-sent nor taken back.
	leg := flatDist(legDest, legFrom)
	var dueSwing [][]byte
	verdict := sess.walkOurs(idx, pos, time.Now())
	if verdict.ours {
		// THE LINE NAMES THE LEG AND THE NUMBERS THE DECISION USED (R110). Tonight's
		// log had ZERO exemption lines, so a reader could not tell an exemption from a
		// rule that never fired - that is how a dead rule survived a whole session.
		h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d): a path step of the walk this "+
			"server sent to (%d, %d) (%.0f u), not a move order", idx, pos[0], pos[1],
			verdict.goal[0], verdict.goal[1], verdict.dist)
	} else {
		// A WALK OF OURS THAT CANNOT EXCUSE THIS REPORT SAYS SO (2026-11-03). Both lines
		// below describe a report the OLD rule swallowed, and without them the difference
		// between "the exemption fired" and "a finished walk kept the fight alive" is
		// invisible in the log - which is exactly how 18:29:33 in logs\zone_go_live.log
		// went unnoticed.
		switch {
		case verdict.arrived:
			// THE OWNER'S OWN CASE: the walk was still on the record, but its body was
			// already at the goal, so the point it named was a click back down a finished
			// leg.
			h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d): that walk to (%d, %d) has "+
				"ARRIVED (body within %.0f u), so this is the player's order",
				idx, pos[0], pos[1], verdict.goal[0], verdict.goal[1], verdict.left)
		case verdict.covered:
			// Still walking, but the report names a point the body has already passed:
			// no step of that walk goes backwards, so this is the player too.
			h.Log.Printf("zone: MOVE_PC char %d -> (%d, %d): that walk to (%d, %d) still "+
				"has %.0f u to go and this report is %.0f u from it, no closer, so this "+
				"is the player's order", idx, pos[0], pos[1], verdict.goal[0],
				verdict.goal[1], verdict.left, verdict.dist)
		}
		// THE SWING THAT IS DUE GOES OUT BEFORE THE MOVE IS EVEN WRITTEN DOWN. It is
		// built inside this branch - the branch that has just decided the report is the
		// player's own order - so it reaches the wire before the echo below, and it is
		// measured on the leg the body is on RIGHT NOW, because the leg this report opens
		// is not applied until after this call returns.
		if h.noteMoveOrder(sess, idx, leg) {
			dueSwing = h.drainMoveOrderSwing(sess, idx, now)
		}
		// A MOVE ORDER MARKS THE CLICK WALK AS OVERTAKEN (2026-11-09). It is NOT cancelled
		// here: a report inside ChaseGrace can still be the tail of the player's earlier
		// walk (TestChaseCancelIgnoredInsideGrace), and the walk's own arrival/away rules
		// already end it when it really is abandoned. What the mark buys is the NEXT
		// click: it replaces this walk instead of being absorbed - the owner's own
		// sequence, click 1 walks and he cancels, click 2 did NOTHING, click 3 worked only
		// after the stale clock ran out (logs\zone_go_10.log, 15:50-15:56).
		//
		// A REPORT NAMING THE POINT THE BODY ALREADY STANDS ON IS NOT AN ORDER: it is the
		// tail or the echo of an earlier walk, and 50 units of it is tolerated.
		if w := sess.Walk; w != nil && w.caster == idx && w.cast == nil && w.atk == nil && w.pick == nil {
			if flatDist(pos, sess.walkedPos(now)) > WalkOrderNoise {
				w.takenOver = true
			}
		}
	}

	// THE MOVE IS APPLIED HERE, AND ONLY HERE (2026-11-03). Nothing above this line has
	// written the leg this report names, so the swing built just above could only be read
	// on the body the character was walking BEFORE the report - its point, its speed and
	// its segment. Moving this block above the call is what put the 3564's +8 on the new
	// leg and stopped the owner's walk (fact #219).
	if opening {
		if idx == sess.Control {
			// SENSE: this is the PLAYER'S OWN CLICK. The session's segment (moveFrom /
			// moveRoute / Pos / moveAt) belongs to the character the player drives, and
			// the gate and the click range read it.
			sess.moveFrom = legFrom
			sess.moveAt = now
			sess.moveRoute = route
			sess.Pos = legDest
		}
		// The same leg, kept for the STOP the other players are owed when it ends
		// (endWalks): the leader's copy above exists for the gate, and a follower has no
		// copy there at all.
		sess.openWalkTo(idx, legFrom, legDest, route, now, spd)
	}

	// The other players see the SAME bytes, so their clients animate the same walk
	// (Python zone_server.py:4615-4627). And the roster remembers where THIS
	// character is - idx is its place in the team, so a follower's move moves the
	// follower and leaves the leader's stored position alone.
	h.broadcastMove(sess, idx, handle, pos, out)

	packets := make([][]byte, 0, len(dueSwing)+len(held)+2)
	// THE SWING THAT WAS DUE COMES FIRST - before the held press's own hit and before
	// the move echo - because that is the reference's own order on the wire (durable fact
	// #236: the damage precedes the move echo in 5 of the 7 windows that landed one).
	packets = append(packets, dueSwing...)
	packets = append(packets, held...)
	packets = append(packets, out)
	if sess.walkOwnedBy(idx) != nil {

		// The report from the character the walk belongs to is the ARRIVAL signal: it
		// can open the dialog, or cancel the walk because the player went elsewhere.
		//
		// A SKILL APPROACH NEVER CANCELS ITS CAST HERE, and that is measured, not
		// inferred: the live server answers the click 130 ms after its own 3571,
		// echoes it at ordinary latency, and lands the cast 941 ms later anyway
		// (captures\cancel20261031\20261031-191902, history\cancel-and-animation.md
		// §2). observeWalk returns nil for that walk, so a move during it sends the
		// echo above and NOTHING else - no stop, no cancel, no extra packet.
		packets = append(packets, h.observeWalk(sess, idx, pos)...)
	}
	return packets, nil
}

// placeTeam decides WHERE this team enters, and says so in the log. It is the ONE
// place the entry order lives, extracted from startGame so it can be tested without a
// socket or a database.
//
// THE HANDOFF FILE ALWAYS WINS. A player who walked through a gate on ANOTHER map
// arrives at that gate's exit, which the old zone left in transfer_<account>.json. It
// is consumed ONCE, and only when it names this account, this map, and is younger than
// TransferMaxAge; every refusal is a log line saying which of those four conditions
// failed (see ReadTransfer). Nothing below overrides a handoff.
//
// AFTER THAT, THE ORDER DEPENDS ON WHICH BARRACK BUTTON THE PLAYER PRESSED
// (`pending_zone_entry.kind`, zone\entry_kind.go):
//
//	EntryNew       (22 CB_STARTGAME)       SAVED START POINT, then LAST POINT
//	EntryContinue  (23 CB_CONTINUEGAME)    LAST POINT, then SAVED START POINT
//
// THE TWO POINTS ARE DIFFERENT FACTS AND THE PLAYER ALREADY CHOSE BETWEEN THEM. The
// saved start point is the town chosen with the waypoint grid's Save button
// (`commander.point`, a restart warp id); the last point is where the leader stood when
// the session ended (`commander.zone_id` + `character_position`). Neither can overwrite
// the other in the database, and the ORDER is the player's choice, not this server's:
// pressing "Continue Game" means "put me back", pressing "New Game" means "take me to
// the town I saved". Before R70 the order was fixed and the two buttons ranked the same.
//
// THE CLIENT'S MISSION BUTTON IS NOT HANDLED: missions are parked by the owner, so cmd 24
// is not answered and no kind is recorded for it (zone\entry_kind.go).
//
// Every step writes its own reason when it refuses, and the last line of this function
// writes the DECISION, so "which point was used" is always answerable from the log.
func (h *Handler) placeTeam(ctx context.Context, sess *Session) ([3]int32,
	[]FormationSlot, string) {
	base, slots := h.Spawn, h.SpawnFormation
	placed := ""
	if sess.AccountID != 0 {
		path := TransferPath(h.transferDir(), sess.AccountID)
		// CLAIMED, not merely read: the file is taken off its name as it is used, by the
		// same rename the handover deadline uses to refund (zone\gate.go,
		// Handler.expireHandoff). One rename wins, so an arrival can never use a point the
		// deadline has already refunded.
		t, err := ClaimTransfer(path, sess.AccountID, h.MapID, time.Now())
		if err != nil {
			// The line names the HANDOFF, not where the team ends up: a Last Point or a
			// saved start point below may still move it, and a line claiming the spawn
			// would then be wrong.
			h.Log.Printf("zone: no map-change handoff for account %d on map %d - %v",
				sess.AccountID, h.MapID, err)
		} else {
			arrival := [3]int32{t.X, t.Y, t.Cell}
			base = arrival
			slots = arrivalFormation(h.SpawnFormation, h.Spawn, arrival)
			placed = fmt.Sprintf("the handoff file %s", path)
			h.Log.Printf("zone: HANDOFF read from %s: account %d ARRIVES on map %d at "+
				"(%d, %d, cell %d), dir byte %d, written %.1fs ago - not the map's own "+
				"spawn", path, t.AccountID, t.MapID, t.X, t.Y, t.Cell, t.Dir,
				float64(time.Now().UnixNano())/1e9-t.At)
			// The file is already gone: ClaimTransfer took it off its name. It is
			// therefore used ONCE, and a handover the deadline refunded can never be
			// applied afterwards.
		}
	}

	// WHICH BUTTON. Read ONCE, before either step, so the two cannot be ordered by
	// different answers. A read error is logged and treated as EntryNew: the safe
	// default is the point the player chose, and a broken read must never stop an
	// entry (the same rule PGStore.EntryKindOf follows for an unknown value).
	kind := EntryNew
	if sess.CommanderID > 0 {
		if h.EntryKindOf == nil {
			h.Log.Printf("zone: no entry-kind store is wired in, so commander %d's "+
				"entry is treated as a new game (the saved start point first)",
				sess.CommanderID)
		} else {
			k, kerr := h.EntryKindOf(ctx, sess.CommanderID)
			if kerr != nil {
				h.Log.Printf("zone: could not read which barrack button commander %d "+
					"pressed (%v) - treated as a new game (the saved start point first)",
					sess.CommanderID, kerr)
			} else {
				kind = k
			}
		}
	}
	h.Log.Printf("zone: ENTRY KIND for commander %d on map %d: %s", sess.CommanderID,
		h.MapID, kind)

	// The two steps, in the order this button asks for. Each one only runs while
	// nothing has placed the team yet, so the second is a FALLBACK and never an
	// override - that is what "the save point wins" means for a New Game.
	savePointStep := func() {
		if placed == "" {
			// Its own function says why when it refuses.
			base, slots, placed = h.savedStartPoint(ctx, sess, base, slots)
		}
	}
	lastPointStep := func() {
		if placed != "" {
			return
		}
		// Only where the leader stood when they left, and only when the barrack's
		// Continue button was the one pressed. It moves the formation with the point,
		// by the same rule a gate's exit uses, so the followers cannot end up at the
		// map's spawn while the leader stands where they logged out.
		//
		// at is a WORLDPOS (x, y, CELL) RESOLVED from the row's stored height by the
		// map's own mesh (Handler.continuePoint); lp.Pos is the row as stored, which is
		// what the log line names so a reader can compare it with the database.
		if at, lp, ok := h.continuePoint(ctx, sess); ok {
			base = at
			slots = arrivalFormation(h.SpawnFormation, h.Spawn, at)
			placed = fmt.Sprintf("the Last Point of commander %d (map %d, stored z %d, "+
				"cell %d)", sess.CommanderID, lp.MapID, lp.Pos[2], at[2])
		}
	}
	switch kind {
	case EntryContinue:
		lastPointStep()
		savePointStep()
	default: // EntryNew
		savePointStep()
		lastPointStep()
	}
	// THE ONE LINE THAT ANSWERS "WHERE DID THIS TEAM ACTUALLY ENTER, AND WHY". Every
	// refusal above wrote its own reason; this writes the decision.
	if placed == "" {
		h.Log.Printf("zone: ENTRY POINT for commander %d on map %d (%s): the map's OWN "+
			"SPAWN (%d, %d, cell %d) - there was no save point to use and no Last "+
			"Point to use", sess.CommanderID, h.MapID, h.MapName, base[0], base[1],
			base[2])
	} else {
		h.Log.Printf("zone: ENTRY POINT for commander %d on map %d (%s): %s",
			sess.CommanderID, h.MapID, h.MapName, placed)
	}
	return base, slots, placed
}

// startGame answers CZ_START_GAME with the team.
func (h *Handler) startGame(ctx context.Context, sess *Session, f protocol.Frame) ([][]byte, error) {
	// A FRESH ENTRY DRIVES SLOT 0 until the client says otherwise (zone\control.go).
	// Set BEFORE the team is read, so even the team-less early return below leaves the
	// default standing: an entry must never inherit the previous character's slot.
	sess.resetControl()
	sess.SessionID, sess.AccountID, sess.LoginID = ParseStartGame(f.Data)

	// PRE-WARM WHAT THIS MAP CONNECTS TO (card t_2950bb0a): the first moment a player is
	// known to be coming here is this packet, so every door out gets its destination asked
	// for NOW, not when they walk through it. It runs before the team is read on purpose -
	// a hint does not need the team, and a refused entry costs nothing but a request the
	// manager's idle stop reaps. A no-op unless -prewarm is on; the cooldown is in
	// zone\prewarm.go.
	h.PrewarmGates()

	// THE OPERATOR TIER, READ ONCE AND NOT PER CHAT LINE (zone\gm.go). A fresh
	// entry must not inherit the previous grant, so the field is cleared first; an
	// absent row and a failed read both leave it at 0, which is the closed answer.
	// The level is keyed by the LOGIN ID (account_info.member_id), the column the
	// reference's ACCOUNT_LEVEL and our account_info share.
	sess.GmLevel = 0
	if h.GmLevelOf != nil && sess.LoginID != "" {
		level, err := h.GmLevelOf(ctx, sess.LoginID)
		if err != nil {
			h.Log.Printf("zone: could not read the account level of %q: %v - this "+
				"session is level 0 and dispatches no GM command", sess.LoginID, err)
		} else {
			sess.GmLevel = level
			if level != 0 {
				h.Log.Printf("zone: GM level %d for %q - // commands from this session "+
					"are dispatched", level, sess.LoginID)
			}
		}
	}

	if h.ConsumeEntry != nil && sess.AccountID != 0 {
		// THE HANDOFF IS A ONE-SHOT TOKEN: taken here, once, before the team is read,
		// so the team comes from exactly the row this call consumed. A refusal is a
		// refusal - the entry must not fall through to "first character", or a replay
		// would still get in. See zone\entry_consume.go.
		if err := h.ConsumeEntry(ctx, sess.AccountID); err != nil {
			var refused *EntryRefused
			if errors.As(err, &refused) {
				h.Log.Printf("zone: ENTRY REFUSED on map %d - %v", h.MapID, refused)
			}
			return nil, fmt.Errorf("entry for account %d: %w", sess.AccountID, err)
		}
	}

	if h.TeamOf != nil && sess.AccountID != 0 {
		team, err := h.TeamOf(ctx, sess.AccountID)
		if err != nil {
			return nil, fmt.Errorf("team for account %d: %w", sess.AccountID, err)
		}
		sess.Team = team
		// A NEW TEAM HAS NO BUFFS: the lists are parallel to Team (zone\buff.go), so a
		// list that survived a team change would be indexed by a slot that now holds
		// another character - and `hasBuffs` would keep the session's clock awake for a
		// buff nobody holds. Nothing persists a buff across an entry in this slice, so
		// starting empty is the truth (a `Save=TRUE` buff is a named GAP).
		sess.buffs = nil
		sess.buffAt = time.Time{}
	}

	// ONE LIVE CONNECTION PER ACCOUNT AND PER CHARACTER (card t_487ea31c): this entry claims the
	// account and every handle the team holds; whoever held them before is closed with one line.
	if displaced := h.registerLiveSession(sess); len(displaced) > 0 {
		h.endDisplaced(sess, displaced)
	}

	// THE MAXIMA COME FROM THE GAME'S OWN FORMULA, HERE, BEFORE anything draws a
	// bar: SCR_Get_MHP and SCR_Get_MSP (zone\status_script.go). The store gives a
	// character its STORED hp/sp, which is a current value, not a maximum - Grace
	// was stored at 5640 while the formula says 9140, and a panel drawn against the
	// stored number shows a full bar at half her health.
	for i := range sess.Team {
		h.refreshSheet(&sess.Team[i])
	}
	// Whose script transactions this session writes to. Resolved once, here: a
	// click must not have to guess, and a transaction with no commander refuses
	// to commit rather than writing to nobody.
	if h.CommanderOf != nil && sess.AccountID != 0 {
		commanderID, err := h.CommanderOf(ctx, sess.AccountID)
		if err != nil {
			// Not fatal: the player can still walk the map and talk to NPCs that
			// change nothing. The click path says why when a script tries to write.
			h.Log.Printf("zone: could not resolve the commander of account %d: %v",
				sess.AccountID, err)
		} else {
			sess.CommanderID = commanderID
		}
	}
	if len(sess.Team) == 0 {
		h.Log.Printf("zone: START_GAME for account %d (%q) but NO characters to spawn - "+
			"the client will sit on the loading screen",
			sess.AccountID, sess.LoginID)
	}

	// Where this team enters - the whole order lives in placeTeam, so it can be
	// tested without a session, a socket or a database.
	base, slots, _ := h.placeTeam(ctx, sess)

	records := make([][]byte, 0, len(sess.Team))
	for i := range sess.Team {
		// By POINTER: the stance and the spawn position have to land on the SESSION's
		// team, not on a copy - the multiplayer spawn sends this same team to the
		// other clients later (players.go).
		c := &sess.Team[i]
		if h.Stances != nil {
			if len(c.Stances) == 0 {
				c.Stances = h.Stances.BuildStanceRecords(int(c.Job))
				c.Skills = h.Stances.BuildSkillRecords(int(c.Job))
			}
			// The stance the CLIENT will stand in, so another player's spawn can name
			// it: the client picks its default from the stance list, and a spawn with
			// 0 leaves the remote actor with no stance at all - it then animates a run.
			//
			// WHAT CHANGED 2026-10-28: the choice now looks at what the character
			// HOLDS (PickStance). The old rule ignored Character.Equip and gave every
			// character the bare-hand stance, so a Fighter with a rapier and a shield
			// stood in "Bareknuckle" on the other player's screen.
			hands := h.Stances.Hands(*c)
			if pick, ok := h.Stances.PickStance(int(c.Job), hands); ok {
				c.Stance = pick.Stance
				// ONE line per character, and it is the line the owner can be asked
				// to read back: the TWO names the client resolves by string, each
				// marked usable or NOT. A "NOT usable" here is a T-pose with no other
				// symptom, so both halves are printed - the job's STD (the standing
				// motion) and the stance's ASTD (the stance's idle).
				h.Log.Print(pick.SpawnLine(c.Handle, c.Name, int(c.Job),
					h.Stances.JobSTD(int(c.Job)), hands))
			} else {
				h.Log.Printf("zone: %q (job %d, holding %s/%s) has NO stance whose idle "+
					"motion resolves - other players will see it without a stance",
					c.Name, c.Job, hands[0], hands[1])
			}
		}
		// Each member stands in its own place in the arrival FORMATION, not all on one
		// spot: the client draws what it is given, and a team on one point stacks three
		// characters on top of each other. Slot 0 is the leader and IS the point; a
		// version-1 file carries no formation and everybody stands on the point, as
		// before.
		//
		// base and slots are the map's own spawn, or the gate's exit and its formation
		// when this entry came through a handoff, or the commander's LAST POINT (where
		// the leader stood when they left, when the barrack's Last Point button was the
		// one pressed), or the SAVED START POINT (the town whose waypoint grid's Save
		// button was pressed last) - see the entry order above.
		//
		// A stored third number is a CELL only when it came from this server: the
		// Last Point is refused here if it is not plausible (Handler.continuePoint),
		// because an invalid cell crashes the client inside PathEngine on the first walk.
		if base != [3]int32{} {
			c.Position = formationSlot(slots, base, i)
		}
		// ONE line per character, so a bad equip fill is visible in the log without
		// a second client: how many of the 21 appearance slots carry a worn item,
		// and which item sits in the hands. `worn` counts the FILLED slots only - the
		// "nothing worn" ids are substituted later, when the packet is built. The
		// spawn's WORD at +11 is ALWAYS 0 now (the live server's own value, armed or
		// not - see BuildSpawnStandingPC), so this line is the only place the held
		// item is named for a human.
		worn := 0
		for _, cls := range c.Equip {
			if cls != 0 {
				worn++
			}
		}
		weapon := weaponClassOf(*c)
		h.Log.Printf("zone: equip char %d %q: %d slot(s) worn, held class %d "+
			"(the spawn's +11 stays 0 - the hands are drawn from the appearance block)",
			c.Handle, c.Name, worn, weapon)
		records = append(records, BuildStartInfo(*c))
	}
	if h.Stances == nil {
		h.Log.Printf("zone: NO stance tables loaded - the records carry no stances " +
			"and the client WILL crash on entry (this is not a client bug)")
	}
	// The click range is measured from the leader, so the session needs a position
	// BEFORE the first move report: the spawn cell is where the client puts them.
	if len(sess.Team) > 0 {
		sess.Pos = sess.Team[0].Position
		if base != [3]int32{} {
			sess.Pos = base
		}
		// Placed, not walked: no segment may drag the character from its old spot.
		sess.moveFrom, sess.moveAt = sess.Pos, time.Now()
		// A fresh entry owns no walk at all, and a stale one would stop the other
		// players at a place this character never stood (see endWalks).
		sess.clearWalks()
		// And no cast survives a placement either: a fresh session has none, but this
		// is the one place a team is put down, and a result owed from before the move
		// would name a point on the map the player just left
		// (zone\skill_cast_clock.go).
		h.dropCasts(sess, "the team was placed on entry")
	}
	// A team can land INSIDE a trigger - and a door's exit is often exactly where the
	// door BACK looks like from here, so a plain arrival is the classic way to bounce a
	// player straight back to the map they came from. office_coim's gate out lands on
	// (-8771, 5529), which is the pixel the WARP_OFFICE_COIM arrow stands on, so this is
	// not hypothetical: without the guard the player would ping-pong forever. Marking
	// that trigger as already fired means the player has to walk out of it first, which
	// is also what a player entering normally at a door-side spawn needs.
	//
	// Both kinds are checked through triggerAt, so the one key space from gateTick is
	// what this uses too - the arrival guard cannot know about gates and not arrows.
	if key, gate, npc, dist, isWarp, inside := h.triggerAt(sess.Pos); inside {
		sess.gateInside = key
		switch {
		case isWarp:
			h.Log.Printf("zone: the team lands %.0f units from warp NPC %s (%s) at "+
				"(%d, %d), inside the %d unit radius - that warp stays shut until the "+
				"player leaves it, so the arrival cannot bounce straight back",
				dist, npc.Script, npc.ClassType, npc.X, npc.Y, WarpNPCRange)
		default:
			h.Log.Printf("zone: the team lands %.0f units from gate %s (range %d) - that "+
				"gate stays shut until the player leaves it, so the arrival cannot bounce "+
				"straight back", dist, gate.Name, gate.Range)
			// Only ONE trigger can be armed (Session.gateInside is one key), so a second
			// circle the team landed in is named here rather than left silent: it WILL
			// fire on the first tick, and a reader of the log deserves to know why.
			if _, n, wDist, win := h.warpAt(sess.Pos); win {
				h.Log.Printf("zone: the team also lands %.0f units from warp NPC %s "+
					"(%s), which is NOT armed - only the nearest trigger is",
					wDist, n.Script, n.ClassType)
			}
		}
	}
	out := BuildStartGameOK(records)
	h.Log.Printf("zone: START_GAME account %d (%q) -> %d character(s), %d bytes",
		sess.AccountID, sess.LoginID, len(records), len(out))

	// The DERIVED half of the Character Info sheet does NOT go here: it is the
	// 3684 burst, and it rides the CZ_GAME_READY reply (Handler.Reply, CmdCZGameReady
	// -> statusBurst). Sent here it was invisible - the client's 3684 handler drops
	// every record while its record-count gate is still 0, which is the state during
	// this reply - and the panel showed the six base stats from START_INFO next to 0
	// for ATK, DEF, A.R., D.R., HR, CSPD and the resists. The numbers were always
	// right (zone\status.go applies the game's own rules, ge\script\pc\get_status_pc.scp);
	// only the packet's place in the handshake was wrong.

	// The entry burst follows the entry packet, in the reference server's order.
	// The data-dependent packets are named in the log instead of being silently
	// missing, so "the panel is empty" has a reason a reader can find.
	var props []Property
	if h.Properties != nil && sess.AccountID != 0 {
		var perr error
		props, perr = h.Properties(ctx, sess.AccountID)
		if perr != nil {
			// Not fatal: a character with no property rows still has to enter the
			// world, and the client then keeps its own defaults.
			h.Log.Printf("zone: could not read the commander properties: %v", perr)
			props = nil
		}
	}
	// The Warp List's cap is the server's own number, and the client is TOLD it here
	// rather than left on its XML default of 5 - otherwise the window promises saves
	// this server refuses. A stale `commander_property` row does not win; the
	// disagreement is logged ONCE, naming both numbers, so the row can be cleaned up.
	props, staleWarp := PropsWithWarpCap(props)
	if staleWarp != nil {
		h.Log.Print(WarpCapWarning(*staleWarp))
	}

	// The bag, drawn at entry: the client's inventory list and the INDEX every
	// later item packet refers to both come from this order, so it is read once
	// here rather than guessed.
	var items []byte
	if h.Bag != nil && sess.CommanderID != 0 {
		stacks, berr := h.Bag(ctx, sess.CommanderID)
		if berr != nil {
			// Not fatal: an empty bag is a state the client draws correctly, and a
			// wrong list is not worth refusing entry over.
			h.Log.Printf("zone: could not read commander %d's bag: %v", sess.CommanderID, berr)
		} else {
			items = BuildInventoryRecords(stacks)
			// The mirror of the client's KEY SPACE. Positions come from this, never
			// from the database, because a take deletes a row and the client keeps
			// its position as a hole (see inventory.go).
			sess.Inv = NewInvMirror(stacks)
			if len(stacks) > 0 {
				h.Log.Printf("zone: %d item stack(s) in the bag, sent as the inventory list",
					len(stacks))
			}
		}
	}
	// The Warp List (`warplist`), read before the burst is built: the four 3634 lists
	// carry the player's own saved points - one list per panel of the window - and the
	// client draws nothing it was not sent. An empty list is the correct answer for a
	// player with no points yet, so a failed read is logged and the window goes out
	// empty - never refused, because entry must not depend on a table that may be empty.
	var savedWarps []SavedPoint
	switch {
	case h.teleports.Load == nil:
		h.Log.Printf("zone: the Warp List (3634) goes out empty - no saved-point store is " +
			"wired in")
	case sess.CommanderID != 0:
		rows, serr := h.teleports.Load(ctx, sess.CommanderID)
		if serr != nil {
			h.Log.Printf("zone: could not read commander %d's saved teleport points: "+
				"%v - the Warp List goes out empty", sess.CommanderID, serr)
		} else {
			savedWarps = rows
			h.Log.Printf("zone: Warp List for commander %d: %d saved point(s) across %d "+
				"list(s); 3634 sent %d time(s) - %s", sess.CommanderID, len(rows),
				commWarpTypes, commWarpTypes, CommWarpSummary(rows))
		}
	}
	burst, skipped := EntryBurstWithSavedTeleports(props, items, savedWarps)
	for _, name := range skipped {
		h.Log.Printf("zone: burst SKIPS %s", name)
	}
	if len(props) > 0 {
		h.Log.Printf("zone: pushing %d Lock_* property value(s) into the client's cache",
			len(props))
	}
	h.Log.Printf("zone: entry burst: %d packet(s) after the entry packet", len(burst))

	packets := make([][]byte, 0, 2+len(burst))
	packets = append(packets, out)
	packets = append(packets, burst...)

	// THE SAVE POINT, TOLD AT ENTRY (2026-11-09). The System Option's "To the Save Point"
	// is the CLIENT's own button: it only does anything when its cache holds a restart warp
	// id, and that id lives in the database (`commander.point`) - so the server has to say
	// it. ZC_UPDATE_RESTART_WARP (3467, cmd + one DWORD) is the packet a 3468 save is
	// answered with (zone\start_point.go), and the entry is the other moment it is owed.
	// Without it the owner pressed the button and the client sent NOTHING AT ALL - there
	// was no packet in the zone log to refuse.
	if h.SaveWarpIDOf != nil && sess.CommanderID > 0 {
		if warpID, found, err := h.SaveWarpIDOf(ctx, sess.CommanderID); err != nil {
			h.Log.Printf("zone: could not read the save point of commander %d at entry: %v",
				sess.CommanderID, err)
		} else if !found || warpID <= 0 {
			h.Log.Printf("zone: no save point for commander %d at entry (commander.point is 0) - "+
				"the client keeps no restart warp, so its To-the-Save-Point button will do nothing",
				sess.CommanderID)
		} else {
			packets = append(packets, BuildUpdateRestartWarp(warpID))
			h.Log.Printf("zone: save point sent at entry: ZC_UPDATE_RESTART_WARP (cmd %d) "+
				"with warp %d, commander %d", CmdZCUpdateRestartWarp, warpID, sess.CommanderID)
		}
	}
	return packets, nil
}

// statusBurst builds the per-character half of the Character Info sheet: one
// 125-byte ZC_UPDATE_ALL_STATUS (3684) per team member, each with its own log
// line so a reader can compare the panel against the numbers the zone derived.
//
// It answers CZ_GAME_READY, inside Handler.Reply. It used to answer CZ_START_GAME
// and the client threw every packet away: its 3684 handler returns silently while
// the record-count gate it tests is still 0, and that is the state during the
// START_GAME reply. The trap is worth remembering - a dropped 3684 logs nothing
// at either end, so "the panel reads 0" looked like a wrong FORMULA for a while
// when the numbers were right all along.
//
// ONE DERIVATION PATH: every 3684 this zone sends comes from StatusPacketFor, here
// and in the equip re-send (zone\equip.go), so an equipment change and an entry
// cannot disagree about a field.
func (h *Handler) statusBurst(sess *Session) [][]byte {
	out := make([][]byte, 0, len(sess.Team))
	for i := range sess.Team {
		c := &sess.Team[i]
		pkt, sheet := StatusPacketFor(*c, h.Status)
		out = append(out, pkt)
		// THE RANK LINE RIDES BESIDE THE SHEET. ZC_PROMOTION_INFO (3093) is what
		// turns the blank line under a party card's name into "Master" / "High
		// Master": the 3684 has no promotion field at all, and the reference sends
		// this packet per character on the way into the world (PlayerChar.cpp:1472,
		// called from Commander_FSM.cpp:251). Same moment as the 3684, same
		// condition - the client drops entry packets before its record exists.
		out = append(out, BuildPromotionInfo(*c))
		h.Log.Printf("zone: 3093 char %d %q: promotion level %d (raw step %d, cash %d)",
			c.Handle, c.Name, c.Promotion, c.PromotionStep, c.CashPromotion)
		h.Log.Printf("zone: 3684 char %d %q: ATK %d DEF %d AR %d DR %d HR %d CSPD %d "+
			"RFIRE %d RICE %d RLGHT %d RSTAT %d RPSY %d (%d bytes)",
			c.Handle, c.Name, int(sheet.ATK), int(sheet.DEF), int(sheet.AR),
			int(sheet.DR), int(sheet.HR), int(sheet.CSPD), int(sheet.RFIRE),
			int(sheet.RICE), int(sheet.RLGHT), int(sheet.RSTAT), int(sheet.RPSY),
			UpdateAllStatusSize)
	}
	return out
}

// defaultStatusForTests is NIL IN PRODUCTION. It exists so the test suite can give
// the hundreds of handlers it builds inline a real sheet source: the sheet is the
// game's script now, so a handler with no Status would answer zeros and every
// cadence and cast assertion would compare nothing. zone\main_test.go TestMain
// installs it once per process; production never sets it, and a production handler
// with no Status still reads zeros and says so at startup.
var defaultStatusForTests *StatusScript

// sheetFor is the zone's ONE way to ask for a character's fifteen Character Info
// numbers. They come from the game's own status script (zone\status_script.go),
// cached per character-and-loadout, so a swing, a cast and the panel all read the
// same answer.
//
// An unwired or failing script answers a ZERO sheet. That is deliberately visible
// rather than plausible: the script logs which character and which field failed,
// and the sheet's own numbers on screen then read 0 - a number nobody can mistake
// for a right one. (attackInterval and the mainline already treat ASPD <= 0 as
// "use the stance row", so a missing script does not spin the attack loop.)
func (h *Handler) sheetFor(c Character) StatusSheet {
	src := h.Status
	if src == nil {
		// TEST-ONLY default: the suite installs one script for the package so a
		// handler built without an explicit source still answers the real formula.
		// nil in production, where a missing source must read as zeros.
		src = defaultStatusForTests
	}
	if src == nil {
		return StatusSheet{}
	}
	sheet, _ := src.Sheet(c)
	return sheet
}

// refreshSheet moves a character's MAXIMA onto what the game's formula says.
//
// WHY IT IS SEPARATE FROM THE PACKET: max HP and max SP do not travel in the
// 3684's BATTLE_STAT - they are BASIC_STAT, the block the SPAWN (3520) and the
// START_INFO record carry, and the same block the 3684 copies over the client's
// record. So a character's maxima have to be ON the character before any packet
// that draws a bar is built, which is why every path that changes what the
// formulas read (entry, equip, unequip, stance change) calls this FIRST.
//
// A zero maximum is not written: the script clamps SCR_Get_MHP at 1, so a zero
// fillMissingGauges gives a character who has never had hit points stored their full bars.
// The create screen sends no values, so the row is written with 0 and the panel draws an
// empty gauge for a character who is alive; the maxima are only known after the status
// script runs, which is why the fill lives next to the clamps in refreshSheet.
func fillMissingGauges(c *Character) {
	if c == nil {
		return
	}
	if c.HP <= 0 && c.MaxHP > 0 {
		c.HP = c.MaxHP
	}
	if c.SP <= 0 && c.MaxSP > 0 {
		c.SP = c.MaxSP
	}
}

func (h *Handler) refreshSheet(c *Character) {
	if c == nil || h.Status == nil {
		return
	}
	sheet, ok := h.Status.Sheet(*c)
	if !ok {
		return
	}
	if sheet.MHP > 0 {
		c.MaxHP = sheet.MHP
	}
	if sheet.MSP > 0 {
		c.MaxSP = clampWord(sheet.MSP)
	}
	// A stored HP/SP above the new maximum would draw a bar past its own end - the
	// same clamp the reference applies when it reads a character in.
	if c.HP > c.MaxHP {
		c.HP = c.MaxHP
	}
	if c.SP > c.MaxSP {
		c.SP = c.MaxSP
	}
	// A CHARACTER IS BORN AT FULL HEALTH. `character.hp` is written as 0 for a new
	// character (the create screen sends no hit points), and the panel then draws 0/680 -
	// alive, but with an empty gauge (owner, 2026-11-11: "but hp/sp? im dead?"). Nothing
	// else fills it: the clamps above only ever lower a value. So a stored zero - and only
	// a zero, never a character who was actually beaten down to it - starts at the maximum
	// the formula just gave.
	fillMissingGauges(c)
	// WITH THE SP LOCK ON, a bar that came in short is filled here too: this is the one
	// place the maxima are re-read (entry, equip, unequip, stance change), so it is the
	// one place that can promise the next packet draws a full gauge. Off - the normal
	// run - this block does not exist. See Handler.SPLock.
	if h.SPLock && c.MaxSP > 0 && c.SP < c.MaxSP {
		c.SP = c.MaxSP
	}
}

// observeSpeed keeps a rolling estimate of how fast the CLIENT actually walks, from
// its own consecutive position reports.
//
// Why measure at all: the packet's speed is now the character's own MSPD, but the
// clock also has to survive a stance this side guessed wrong, a buff it cannot see,
// and the client's own collision stops. Measuring costs nothing - the client already
// streams positions while the player walks - and a clock that believes a speed 30%
// too high opens the dialog mid-street.
//
// Samples too close in time or space are dropped (a corner, a duplicate report), and
// the estimate is smoothed so one bad pair cannot swing it.
func (s *Session) observeSpeed(idx int, pos [3]int32, now time.Time) {
	if s.lastMoveAt.IsZero() || idx != s.lastMoveIdx {
		s.lastMoveAt, s.lastMovePos, s.lastMoveIdx = now, pos, idx
		return
	}
	dt := now.Sub(s.lastMoveAt).Seconds()
	dist := math.Hypot(float64(pos[0]-s.lastMovePos[0]), float64(pos[1]-s.lastMovePos[1]))
	s.lastMoveAt, s.lastMovePos = now, pos

	if dt < 0.05 || dist < 20 {
		return // too short to mean anything: a turn, a stop, a duplicate
	}
	speed := dist / dt
	if speed < 40 || speed > 1200 {
		return // a teleport or a rounding artefact, not a walk
	}
	if s.MoveSpeed == 0 {
		s.MoveSpeed = speed
	} else {
		s.MoveSpeed = s.MoveSpeed*0.7 + speed*0.3
	}
	s.moveSamples++
}

// walkSpeed is the speed the CLOCK uses. The measured value wins only when it is
// trustworthy: at least 4 clean samples, and inside the range a stance run speed can
// actually have. Two reports taken a moment apart once produced "48 u/s" and made the
// character crawl (2026-10-25) - a sparse sample is not a speed.
func (s *Session) walkSpeed() float64 {
	if s.moveSamples >= 4 && s.MoveSpeed >= minPlausibleSpeed && s.MoveSpeed <= maxPlausibleSpeed {
		return s.MoveSpeed
	}
	// The fallback is the move-speed formula's own base (469), not CFieldChar's 415
	// placeholder: with no gear and a StnMSPD of 1 that IS the speed the character
	// walks at, so the estimate is right before a single sample has arrived.
	return MoveBaseSpeed
}

// The band a stance run speed can plausibly sit in. Below it is a measurement artefact
// (a turn, a corner, two reports inside one stride); above it is a teleport.
const (
	minPlausibleSpeed = 250.0
	maxPlausibleSpeed = 800.0
)

// packetSpeed is the speed put IN the move packet, which is the speed the client
// MOVES the actor at. It is the character's own MSPD, never the engine default:
// `packet.spd = m_moveSpeed` and m_moveSpeed = GetMoveSPD() (PlayerChar.cpp:408,
// FieldChar_FSM.cpp:599), and GetMoveSPD() reads the scripted MSPD property - see
// zone\speed.go for the whole chain, and for why 415 was wrong.
//
// A nil character (no team yet) or one with no stance answers the base 469: the
// formula's constant does not depend on the stance, only its multiplier does.
func (h *Handler) packetSpeed(c *Character) float64 {
	if c == nil {
		return MoveBaseSpeed
	}
	// The worn items count: MSPD is a multiplier per category, and the swing's
	// position is interpolated at this speed - see zone\speed.go. The TERMS line
	// is printed HERE, once per change of value and not once per packet.
	value, stanceMSPD, itemMSPD, limit := h.Stances.moveSpeedTerms(
		c.Stance, c.Job, gearMSPD(wornGear(*c)))
	mspdLogOnce(h.Log, c.Handle, stanceMSPD, itemMSPD, value, limit)
	return value
}

// hexHead renders the first n bytes as hex for the log. Used when an unknown
// command arrives: the bytes are the only evidence of what it really was.
func hexHead(b []byte, n int) string {
	if n <= 0 || len(b) == 0 {
		// An empty packet is a real thing to see, and `out[:len(out)-1]` on an
		// empty slice panics - the log line for it must not be able to crash.
		return ""
	}
	if len(b) < n {
		n = len(b)
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		out = append(out, digits[b[i]>>4], digits[b[i]&0x0F], ' ')
	}
	return string(out[:len(out)-1])
}

// ParseStartGame reads the three fields CZ_START_GAME carries after the
// security header: PKS_SECURITY_DEFAULT is command(2) + seq(4) + checksum(4),
// so session and account start at offset 10, and the login id is 17 bytes at 18.
func ParseStartGame(pt []byte) (sessionID, accountID uint32, loginID string) {
	if len(pt) < 18 {
		return 0, 0, ""
	}
	sessionID = binary.LittleEndian.Uint32(pt[10:])
	accountID = binary.LittleEndian.Uint32(pt[14:])
	if len(pt) > 18 {
		end := 18 + seqNameLen
		if end > len(pt) {
			end = len(pt)
		}
		loginID = trimNul(pt[18:end])
	}
	return sessionID, accountID, loginID
}

// Online answers how many sessions this zone is serving right now. The manager reads the
// same number from the presence file (world.Record.Players) to decide whether a map can
// be stopped.
func (h *Handler) Online() int { return int(h.online.Load()) }
