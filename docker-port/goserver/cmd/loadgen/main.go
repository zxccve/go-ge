// Command loadgen drives many bots into one zone, for a load test.
//
// WHAT IT IS (S1, and S2 built on top of it)
// One bot is one TCP connection that does exactly what the real client does on
// the way into a map: CZ_START_GAME, read ZC_START_GAMEOK, CZ_GAME_READY, read
// ZC_ENTER_LEGACY_OBJECTS, then run a pattern for a fixed time. S1 only sat
// still (the `idle` pattern); S2 adds walk, crowd and attack, and measures what
// each bot actually saw on its own socket.
//
// WHY THE PACKETS LOOK THE WAY THEY DO
// Every shape below is copied from what the SERVER parses or writes, never
// invented, and each constant names the line it came from:
//
//	CZ_START_GAME (3590)  zone\zone.go, ParseStartGame: 40 bytes, session id at
//	                      +10, account id at +14, the 17-byte login at +18.
//	CZ_GAME_READY (3592)  13 bytes; the zone reads only the command.
//	CZ_MOVE_PC (3598)     23 bytes, built exactly as zone\walkend_test.go:29-37
//	                      (moveTo) builds it, security header left ZERO.
//	CZ_ATTACK (3666)      15 bytes, zone\attack_test.go:59-67 (attackFrame),
//	                      with the client's own checksum at +6.
//
// The ids themselves are the zone package's constants, so they cannot drift
// from what the server switches on. The cipher and BOTH framing layers are
// protocol's - this program does not own a second copy of either.
//
// Server->client data arrives PLAINTEXT, one packet after another, with no
// transport chunk (zone\zone.go, writePackets). So the reply side is cut with
// the same command table, which is what every size on it is for.
//
// Usage, from goserver\:
//
//	go run ./cmd/loadgen -zone 127.0.0.1:7102 -bots 10 -pattern walk
//	go run ./cmd/loadgen -zone 127.0.0.1:7102 -bots 50 -ramp 20 -pattern crowd -duration 30s
//	go run ./cmd/loadgen -zone 127.0.0.1:7102 -bots 4 -pattern attack
package main

import (
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"geserver/protocol"
	"geserver/zone"
)

// Packet shapes, from data\cmd_table_live_z.txt and the zone's parsers.
const (
	startGameSize = 40 // CZ_START_GAME: a FIXED-size packet in the table
	gameReadySize = 13 // CZ_GAME_READY: fixed too

	startSessionAt = 10 // session id, zone.ParseStartGame
	startAccountAt = 14 // account id, zone.ParseStartGame
	startLoginAt   = 18 // login id, 17 bytes, NUL padded (zone\entry.go seqNameLen)
	loginLen       = 17
)

// CZ_MOVE_PC (3598), 23 bytes. The builder is COPIED from the zone's own test,
// zone\walkend_test.go:29-37 (moveTo): command at +0, charIndex at +10, then
// three int32 WORLDPOS fields. Bytes +2..+9 (the security header's seq and
// checksum) are left ZERO, exactly as that oracle builder leaves them - the
// zone's movePC does not read either (zone\zone.go:1506).
const (
	moveSize        = 23
	moveCharIndexAt = 10
	moveXAt         = 11
	moveYAt         = 15
	moveCellAt      = 19
)

// CZ_ATTACK (3666), 15 bytes: zone\attack_test.go:59-67 (attackFrame) builds it,
// zone\attack.go:86-88 (caChecksum, caCharIndex, caTarget) names the fields the
// zone's own parser reads back.
const (
	attackSize        = 15
	attackSeqAt       = 2  // u32, any value; a per-bot counter here
	attackChecksumAt  = 6  // u32, czChecksum over the whole packet
	attackCharIndexAt = 10 // u8, the acting slot in this session's team
	attackTargetAt    = 11 // u32, the target's handle
)

// Server->client frames this program reads. Every offset is the ZONE's own
// writer, so a change there shows up here as a wrong number, not a guess.
const (
	// ZC_MOVE_PC (3548), 30 bytes: zone\move.go:62-86 (BuildMovePC).
	echoHandleAt = 2
	echoXAt      = 10
	echoYAt      = 14
	echoCellAt   = 18

	// ZC_SPAWN_STANDINGPC (3520): VARIABLE size, length word at +2. Field offsets
	// are zone\players.go:250-263; the nameplate is the 17-byte string at +341,
	// which the comment at zone\players.go:675 states outright (306 bytes of
	// appearance, then apNameplate at block offset 35).
	spawnHandleAt = 4
	spawnPosAt    = 17 // x, y, cell: three int32
	spawnNameAt   = 341
	spawnNameLen  = 17

	// ZC_ENTER_STANDINGMONSTER (3527), 360 bytes fixed: zone\monster.go:145-149
	// (emHandle, emPos).
	monsterHandleAt = 2
	monsterPosAt    = 15 // x, y, cell: three int32
)

// Timing and geometry. Every number here that the zone did not give us is
// marked, so the next reader knows which ones are measured and which are a
// deliberate choice.
const (
	// enterTimeout is how long one bot waits for a reply it is expecting during
	// entry. A zone that cannot answer entry in this long is a failure worth
	// naming, not a hang.
	enterTimeout = 20 * time.Second

	movePeriod  = 300 * time.Millisecond // CHOSEN: a client walks in a stream; 300 ms is a slow, cheap stream
	walkRadius  = 60                     // CHOSEN: the square's half-side for `walk`
	crowdRadius = 20                     // CHOSEN: the square's half-side for `crowd`
	crowdSpread = 6                      // CHOSEN: how far each crowd bot's centre sits from the common spawn

	// spawnWait is how long a bot waits for a 3520 (its centre) or a 3527 (the
	// monster) after it is in the map. The zone announces both DURING entry, so
	// this window is only ever reached when the zone sent neither.
	spawnWait = 5 * time.Second

	attackApproach  = 25                      // units west of the monster, as the brief fixes it
	attackWalkWait  = 1200 * time.Millisecond // the walk sim's own arrival rule: 25 / 469 s plus slack
	attackGap       = 800 * time.Millisecond  // CHOSEN: the gap between one CZ_ATTACK and the next
	attackReplyWait = 3 * time.Second         // CHOSEN: a reply later than this is a failed exchange

	pollTick = 20 * time.Millisecond // how often a waiting bot looks at the shared state
)

var (
	errNoSpawn   = errors.New("no spawn seen: the zone sent no ZC_SPAWN_STANDING_PC and -center was empty")
	errNoMonster = errors.New("no monster: start the zone with -spawnmonster on map 1331")
)

// bot is one row of the accounts CSV.
type bot struct {
	account uint32
	login   string
}

// botState is one bot's live state. EVERY field except t0 and stopping is
// guarded by hub.mu: the reader goroutine writes it while the pattern goroutine
// reads it, and a bare map written from two goroutines is a race.
type botState struct {
	login    string
	t0       time.Time   // just before the first byte goes out
	stopping atomic.Bool // set before we close the socket, so the reader stops treating EOF as a failure

	packetsOut    int64
	packetsIn     int64
	fanoutRecv    int
	rttN          int
	rttFailed     int
	attackSeq     uint32
	attackSentAt  time.Time
	attackPending bool
	readErr       error

	// rttCh carries ONE completed round trip to the pattern goroutine. Capacity
	// 1 plus the one-outstanding rule below means it can never hold a stale
	// sample: every send is preceded by a drained channel.
	rttCh chan time.Duration
}

type pairKey struct{ from, to string }

// hub is everything the bots share: the roster learned from 3520s, the common
// spawn point, the monster, and every latency sample. ONE mutex guards it all.
// The timestamps that feed the samples are taken OUTSIDE the lock, so the lock
// can only ever add scheduling time to the comparison, never to the reading.
type hub struct {
	mu sync.Mutex

	roster map[uint32]string

	centerSet bool
	center    [3]int32

	monsterSet bool
	monster    uint32
	monsterPos [3]int32

	// lastSend is THE KEY TO THE FAN-OUT SAMPLE: sender login -> the moment that
	// bot last wrote a move. It is keyed by the SENDER, not by anything in the
	// frame, because the echo (3548) carries the mover's HANDLE at +2 and the
	// roster turns that handle into the login. CZ_MOVE_PC has no sequence of its
	// own (bytes +2..+9 stay zero, zone\walkend_test.go:29-37), so the handle is
	// the one exact name for "who moved"; a position would collide the moment two
	// bots walked through the same point, which `crowd` makes likely. One move per
	// 300 ms per sender means the last send IS the send this echo belongs to -
	// unless fan-out is slower than the whole cadence, and then the sample is
	// short, never invented: a number that late is already the finding.
	lastSend map[string]time.Time

	fanoutAll    []float64 // milliseconds, every pair together
	pairs        map[pairKey][]float64
	unattributed int

	rttAll []float64 // milliseconds, CZ_ATTACK -> 3564/3568
}

func main() {
	zoneAddr := flag.String("zone", "127.0.0.1:7002", "zone address to dial")
	accountsPath := flag.String("accounts", "../out/loadtest/accounts.csv",
		"accounts CSV from src\\loadtest_seed.py")
	bots := flag.Int("bots", 0, "how many bots to run; 0 = every row in the CSV")
	ramp := flag.Int("ramp", 10, "connections per second; 0 = all at once")
	wait := flag.Duration("wait", 20*time.Second,
		"S1's idle hold, kept so an old command line still runs. It is used ONLY when "+
			"-duration is left at its default AND -wait is non-zero; otherwise -duration wins")
	duration := flag.Duration("duration", 20*time.Second,
		"how long each bot runs its pattern after entering. Supersedes -wait, except that "+
			"-wait is used when this flag is left at its default and -wait is non-zero")
	pattern := flag.String("pattern", "idle", "idle | walk | crowd | attack")
	spread := flag.Int("spread", 0,
		"walk only, default 0 = today's 3x3 cluster of 60-unit squares: place bot i in its own "+
			"cell of a near-square grid whose cells are this many units apart, so the SIGHT RING "+
			"has real distance to cut. 0 changes nothing. See history\\vps-capacity.md")
	outDir := flag.String("out", "out/loadtest",
		"directory for the JSON report; ../out/loadtest is used when that one exists and this one does not")
	center := flag.String("center", "",
		`manual "x,y,cell" override for a 1-bot run; normally learned from another bot's spawn`)
	tablePath := flag.String("table", "../data/cmd_table_live_z.txt",
		"the zone's own command id/size table, used to cut the replies")
	flag.Parse()

	switch *pattern {
	case "idle", "walk", "crowd", "attack":
	default:
		fmt.Fprintf(os.Stderr, "loadgen: unknown -pattern %q: must be idle, walk, crowd or attack\n", *pattern)
		os.Exit(2)
	}
	if *spread < 0 {
		fmt.Fprintf(os.Stderr, "loadgen: -spread %d: must be 0 or more\n", *spread)
		os.Exit(2)
	}

	// THE -wait RULE, in one place. S1 had no -duration, so an old command line
	// must still mean what it meant: hold the session for -wait. As soon as the
	// caller names -duration, that is the answer and -wait is ignored.
	durationSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "duration" {
			durationSet = true
		}
	})
	if !durationSet && *wait > 0 {
		*duration = *wait
	}
	if *duration < 0 {
		fmt.Fprintf(os.Stderr, "loadgen: -duration cannot be negative\n")
		os.Exit(2)
	}

	var fixedCenter *[3]int32
	if *center != "" {
		p, err := parseCenter(*center)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: -center %q: %v\n", *center, err)
			os.Exit(2)
		}
		fixedCenter = &p
	}

	table, err := protocol.LoadTable(*tablePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}

	all, err := readAccounts(*accountsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}
	if *bots < 0 {
		fmt.Fprintf(os.Stderr, "loadgen: -bots cannot be negative\n")
		os.Exit(2)
	}
	if *bots == 0 {
		*bots = len(all)
	}
	if *bots > len(all) {
		fmt.Fprintf(os.Stderr, "loadgen: asked for %d bots, %s has %d row(s)\n",
			*bots, *accountsPath, len(all))
		os.Exit(2)
	}

	dir, err := resolveOutDir(*outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}

	h := &hub{
		roster:   map[uint32]string{},
		lastSend: map[string]time.Time{},
		pairs:    map[pairKey][]float64{},
	}
	if fixedCenter != nil {
		h.centerSet = true
		h.center = *fixedCenter
	}

	started := time.Now()
	fmt.Printf("loadgen: pattern %s, %d bot(s), %s, zone %s\n", *pattern, *bots, *duration, *zoneAddr)

	// One goroutine per bot, a ticker between starts so the zone sees a ramp
	// rather than a thundering herd. results is buffered for every bot, so no
	// goroutine can block on reporting.
	results := make(chan botResult, *bots)
	var interval time.Duration
	if *ramp > 0 {
		interval = time.Second / time.Duration(*ramp)
	}
	cfg := runConfig{addr: *zoneAddr, table: table, pattern: *pattern, duration: *duration, nBots: *bots, spread: *spread}
	for i := 0; i < *bots; i++ {
		if i > 0 && interval > 0 {
			time.Sleep(interval)
		}
		b := all[i]
		go func(i int, b bot) { results <- runBot(cfg, h, b, i) }(i, b)
	}
	fmt.Fprintf(os.Stderr, "loadgen: %d bot(s) started in %.1fs\n", *bots, time.Since(started).Seconds())

	botResults := make([]botResult, 0, *bots)
	failures := make([]failureJSON, 0)
	entered, failed := 0, 0
	for i := 0; i < *bots; i++ {
		r := <-results
		botResults = append(botResults, r)
		if r.err != nil {
			failed++
			failures = append(failures, failureJSON{Login: r.login, Reason: r.err.Error()})
			fmt.Printf("FAIL %s: %v\n", r.login, r.err)
			continue
		}
		entered++
	}

	sort.Slice(botResults, func(i, j int) bool { return botResults[i].login < botResults[j].login })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Login < failures[j].Login })

	printRoster(h)
	printBots(botResults)

	fanout := summarize(h.fanoutAll)
	h.mu.Lock()
	unattributed := h.unattributed
	h.mu.Unlock()
	fmt.Printf("fan-out latency (3598 move -> 3548 echo): n=%d p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms unattributed=%d\n",
		fanout.n, fanout.p50, fanout.p95, fanout.p99, fanout.max, unattributed)

	if *pattern == "attack" {
		rtt := summarize(h.rttAll)
		fmt.Printf("self RTT (3666 -> 3564/3568): n=%d p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms\n",
			rtt.n, rtt.p50, rtt.p95, rtt.p99, rtt.max)
	}

	fmt.Printf("entered %d, failed %d\n", entered, failed)

	rep := buildReport(started, *zoneAddr, *pattern, *bots, *ramp, *duration, entered, failed, botResults, h, failures, *spread)
	path, err := writeReport(dir, rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("wrote %s\n", path)

	if failed > 0 {
		os.Exit(1)
	}
}

// runConfig is what every bot needs and none of them may change.
type runConfig struct {
	addr     string
	table    protocol.Table
	pattern  string
	duration time.Duration
	nBots    int
	spread   int
}

// botResult is one bot's row for the table and the JSON.
type botResult struct {
	login      string
	bytesOut   int64
	bytesIn    int64
	packetsOut int64
	packetsIn  int64
	fanoutRecv int
	rttN       int
	rttFailed  int
	elapsed    time.Duration
	err        error
}

// resolveOutDir finds the report directory the way readAccounts finds the CSV:
// the given path first, then "../" + path (so it works from goserver\ and from
// docker-port\), and creates the given one when neither exists.
func resolveOutDir(dir string) (string, error) {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, nil
	}
	if !strings.HasPrefix(dir, "../") && !strings.HasPrefix(dir, `..\`) {
		alt := "../" + dir
		if fi, err := os.Stat(alt); err == nil && fi.IsDir() {
			return alt, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create output directory %s: %w", dir, err)
	}
	return dir, nil
}

// parseCenter turns "x,y,cell" into the three int32 a WORLDPOS is.
func parseCenter(s string) ([3]int32, error) {
	var out [3]int32
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return out, fmt.Errorf("want x,y,cell (three numbers)")
	}
	for i, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 32)
		if err != nil {
			return out, fmt.Errorf("field %d %q: %w", i+1, p, err)
		}
		out[i] = int32(v)
	}
	return out, nil
}

// runBot takes one bot through the entry handshake, runs its pattern, and
// returns its counters. A nil error means the pattern ran to the end of the
// duration on a connection that never dropped.
func runBot(cfg runConfig, h *hub, b bot, idx int) botResult {
	res := botResult{login: b.login}
	if len(b.login) > loginLen-1 {
		res.err = fmt.Errorf("login %q is longer than %d bytes", b.login, loginLen-1)
		return res
	}

	conn, err := net.DialTimeout("tcp", cfg.addr, 10*time.Second)
	if err != nil {
		res.err = fmt.Errorf("dial %s: %w", cfg.addr, err)
		return res
	}
	cc := &countingConn{Conn: conn}
	st := &botState{login: b.login, rttCh: make(chan time.Duration, 1)}

	// The reader is started BEFORE the first write and runs for the whole life
	// of the connection. That is deliberate, not tidiness: joinZone sends the
	// other players' spawns BEFORE ZC_ENTER_LEGACY_OBJECTS, so a reader started
	// only after entry would throw away the very roster and centre it needs.
	// And it is the reader, not the pattern loop, that timestamps a 3548 the
	// moment it has a whole frame - a loop that slept between reads would add
	// up to one poll interval of noise to every latency sample.
	rd := &botReader{
		conn: cc, table: cfg.table, h: h, st: st,
		startOK: make(chan struct{}, 1),
		entered: make(chan struct{}, 1),
		monster: make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() { rd.run(); close(done) }()

	// finish closes the socket, WAITS for the reader, and only then takes the
	// counters. Closing first is what unblocks a reader parked in Read; waiting
	// for it is what makes the counters final before the report reads them.
	finish := func(patternErr error) botResult {
		st.stopping.Store(true)
		cc.Close()
		<-done
		if patternErr == nil {
			patternErr = h.readErrOf(st)
		}
		res.err = patternErr
		res.bytesOut = atomic.LoadInt64(&cc.bytesOut)
		res.bytesIn = atomic.LoadInt64(&cc.bytesIn)
		h.mu.Lock()
		res.packetsOut = st.packetsOut
		res.packetsIn = st.packetsIn
		res.fanoutRecv = st.fanoutRecv
		res.rttN = st.rttN
		res.rttFailed = st.rttFailed
		h.mu.Unlock()
		res.elapsed = time.Since(st.t0)
		return res
	}

	st.t0 = time.Now()

	// -- CZ_START_GAME: 40 bytes, accounted for field by field -------------
	start := make([]byte, startGameSize)
	binary.LittleEndian.PutUint16(start, zone.CmdCZStartGame)
	// The session id is the client's own; the zone stores it and never checks
	// it against anything, so reusing the account id keeps two bots distinct
	// without inventing a random field.
	binary.LittleEndian.PutUint32(start[startSessionAt:], b.account)
	binary.LittleEndian.PutUint32(start[startAccountAt:], b.account)
	copy(start[startLoginAt:startLoginAt+loginLen], b.login)
	if err := st.send(h, cc, start); err != nil {
		return finish(fmt.Errorf("send CZ_START_GAME: %w", err))
	}
	if err := waitSignal(rd.startOK, done, st, h,
		fmt.Sprintf("no ZC_START_GAMEOK (%d)", zone.CmdZCStartGameOK)); err != nil {
		return finish(err)
	}

	// -- CZ_GAME_READY: 13 bytes, the world is loaded ----------------------
	ready := make([]byte, gameReadySize)
	binary.LittleEndian.PutUint16(ready, zone.CmdCZGameReady)
	if err := st.send(h, cc, ready); err != nil {
		return finish(fmt.Errorf("send CZ_GAME_READY: %w", err))
	}

	// -- ZC_ENTER_LEGACY_OBJECTS: the packet that ends the loading screen ---
	if err := waitSignal(rd.entered, done, st, h,
		fmt.Sprintf("no ZC_ENTER_LEGACY_OBJECTS (%d)", zone.CmdZCEnterLegacyObjs)); err != nil {
		return finish(err)
	}

	// -- the pattern, for the duration, starting from "I am in the map" -----
	var patternErr error
	switch cfg.pattern {
	case "idle":
		patternErr = runIdle(h, st, done, cfg.duration)
	case "walk", "crowd":
		if err := waitCenter(h, st, done, spawnWait); err != nil {
			patternErr = err
			break
		}
		patternErr = runWalkLoop(h, st, cc, done, idx, cfg.duration, cfg.pattern == "crowd", cfg.nBots, cfg.spread)
	case "attack":
		if err := waitMonster(h, st, done, spawnWait, rd.monster); err != nil {
			patternErr = err
			break
		}
		patternErr = runAttack(h, st, cc, done, cfg.duration)
	}
	return finish(patternErr)
}

// send encrypts one plaintext packet with the client's own cipher, counts it,
// and writes it. protocol owns the cipher; a second copy of it here is the bug
// this avoids.
func (st *botState) send(h *hub, cc net.Conn, plain []byte) error {
	h.mu.Lock()
	st.packetsOut++
	h.mu.Unlock()
	return writeCipher(cc, plain)
}

// sendMove is one CZ_MOVE_PC. The send time is recorded BEFORE the write, so a
// fast echo can never be compared against a timestamp that did not exist yet.
func (st *botState) sendMove(h *hub, cc net.Conn, pos [3]int32) error {
	pkt := make([]byte, moveSize)
	binary.LittleEndian.PutUint16(pkt[0:], zone.CmdCZMovePC)
	pkt[moveCharIndexAt] = 0 // slot 0: the leader of this session's team
	binary.LittleEndian.PutUint32(pkt[moveXAt:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(pkt[moveYAt:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(pkt[moveCellAt:], uint32(pos[2]))
	// pkt[2:10] stays ZERO - the oracle builder leaves seq and checksum zero
	// and the zone reads neither (zone\walkend_test.go:29-37).

	now := time.Now()
	h.mu.Lock()
	h.lastSend[st.login] = now
	st.packetsOut++
	h.mu.Unlock()
	return writeCipher(cc, pkt)
}

// sendAttack is one CZ_ATTACK at target. The round trip starts at the send
// time recorded here, which is taken immediately before the write.
func (st *botState) sendAttack(h *hub, cc net.Conn, target uint32) error {
	pkt := make([]byte, attackSize)
	binary.LittleEndian.PutUint16(pkt[0:], zone.CmdCZAttack)
	pkt[attackCharIndexAt] = 0
	binary.LittleEndian.PutUint32(pkt[attackTargetAt:], target)

	h.mu.Lock()
	st.attackSeq++
	binary.LittleEndian.PutUint32(pkt[attackSeqAt:], st.attackSeq)
	st.attackSentAt = time.Now()
	st.attackPending = true
	st.packetsOut++
	h.mu.Unlock()

	binary.LittleEndian.PutUint32(pkt[attackChecksumAt:], czChecksum(pkt))
	return writeCipher(cc, pkt)
}

// writeCipher sends one packet the way the real client does.
func writeCipher(conn net.Conn, plain []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write(protocol.DefaultCipher.EncryptChunk(plain))
	return err
}

// czChecksum is the client's own GetChecksum, COPIED from zone\attack.go:224-237
// because it is unexported and this program may not edit the zone package. It
// must stay byte-identical. The zone recomputes it and only LOGS a mismatch
// (zone\attack.go:690-694 - it accepts the packet anyway, because no sample
// shows the reference refusing one), so a copy that drifted would still "work"
// and the log line would be the only sign.
func czChecksum(pt []byte) uint32 {
	var acc uint32
	for i, b := range pt {
		if i >= attackChecksumAt && i < attackChecksumAt+4 {
			continue // the field itself is zero while it is computed
		}
		if i%2 == 0 {
			acc ^= uint32(b)
			continue
		}
		acc += uint32(b)
	}
	return acc
}

// waitSignal waits for one entry event, the connection ending, or the timeout.
func waitSignal(sig <-chan struct{}, done <-chan struct{}, st *botState, h *hub, what string) error {
	timer := time.NewTimer(enterTimeout)
	defer timer.Stop()
	select {
	case <-sig:
		return nil
	case <-done:
		return fmt.Errorf("%s: connection closed: %w", what, readErrOrEOF(h, st))
	case <-timer.C:
		return fmt.Errorf("%s: timed out after %s", what, enterTimeout)
	}
}

// waitCenter waits for the common spawn point. A bot is never told its OWN
// spawn (joinZone skips the sender, zone\players.go:767-770), but every bot IS
// told every OTHER bot's spawn, and all bots on a map share one team spawn
// point - so one 3520 is enough. With -bots >= 2 this wait always succeeds,
// because joinZone announces each newcomer to everyone already there and every
// newcomer is told about everyone already there (zone\players.go:766-785): the
// only bot that can still be waiting is one that arrived alone, and the next
// bot's join fills it in.
func waitCenter(h *hub, st *botState, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h.mu.Lock()
		ok := h.centerSet
		h.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-done:
			return readErrOrEOF(h, st)
		default:
		}
		if time.Now().After(deadline) {
			return errNoSpawn
		}
		time.Sleep(pollTick)
	}
}

// waitMonster waits for the first 3527. Its signal is a channel, so a monster
// that arrived during entry is not missed.
func waitMonster(h *hub, st *botState, done <-chan struct{}, timeout time.Duration, sig <-chan struct{}) error {
	h.mu.Lock()
	ok := h.monsterSet
	h.mu.Unlock()
	if ok {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-sig:
		return nil
	case <-done:
		return readErrOrEOF(h, st)
	case <-timer.C:
		return errNoMonster
	}
}

// runIdle is S1's behaviour unchanged: hold the session open and let the reader
// drain. A dropped connection is noticed by checking the reader's error, not by
// a write, because an idle bot writes nothing.
func runIdle(h *hub, st *botState, done <-chan struct{}, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-done:
			return readErrOrEOF(h, st)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runWalkLoop is the square loop `walk` and `crowd` share. `walk` gives each bot
// its own centre, offset by a whole step in a 3x3 grid, so two bots never walk on
// top of each other and the zone sees ten separate bodies. `crowd`
// instead packs every centre within ~30 units of the COMMON spawn, at
// crowdSpread units on its own angle, because the point of that pattern is that
// every bot is inside every other bot's view and every move fans out to all.
//
// The cell is the one the bot LEARNED, sent as-is: the zone ignores a move's
// cell outright ("The cell is ignored: a walk happens on one map layer",
// zone\move.go:535), so this program must not pretend to know a better one.
func runWalkLoop(h *hub, st *botState, cc net.Conn, done <-chan struct{}, idx int, d time.Duration, crowd bool, nBots int, spread int) error {
	h.mu.Lock()
	center := h.center
	h.mu.Unlock()

	r := int32(walkRadius)
	cx := center[0] + int32(((idx%3)-1)*walkRadius)
	cy := center[1] + int32(((idx/3)-1)*walkRadius)
	if crowd {
		r = crowdRadius
		ang := 2 * math.Pi * float64(idx) / float64(nBots)
		cx = center[0] + int32(math.Round(crowdSpread*math.Cos(ang)))
		cy = center[1] + int32(math.Round(crowdSpread*math.Sin(ang)))
	}
	if spread > 0 && !crowd {
		// SPREAD (the sight-ring run, 2026-11-10). A `walk` bot normally circles
		// 60 units from a 3x3 cluster, so every bot sits well inside every other
		// bot's ring and a measurement can only ever read "everyone was told".
		// Here bot i gets its own cell of a near-square grid whose cells are
		// `spread` units apart, so DISTANCE - and only distance - decides who is
		// told about whom. 0 keeps today's cluster byte for byte.
		cols := int(math.Ceil(math.Sqrt(float64(nBots))))
		if cols < 1 {
			cols = 1
		}
		rows := (nBots + cols - 1) / cols
		col, row := idx%cols, idx/cols
		cx = center[0] + int32(math.Round((float64(col)-float64(cols-1)/2)*float64(spread)))
		cy = center[1] + int32(math.Round((float64(row)-float64(rows-1)/2)*float64(spread)))
	}
	cell := center[2]
	square := [4][2]int32{
		{cx + r, cy + r},
		{cx + r, cy - r},
		{cx - r, cy - r},
		{cx - r, cy + r},
	}

	deadline := time.Now().Add(d)
	for i := 0; ; i++ {
		if !time.Now().Before(deadline) {
			return nil
		}
		wp := square[i%len(square)]
		if err := st.sendMove(h, cc, [3]int32{wp[0], wp[1], cell}); err != nil {
			return fmt.Errorf("send CZ_MOVE_PC: %w", err)
		}
		if !time.Now().Add(movePeriod).Before(deadline) {
			return nil
		}
		if err := waitPeriod(st, h, done, movePeriod); err != nil {
			return err
		}
	}
}

// runAttack approaches the monster once, lets the zone's own walk simulation
// arrive, then keeps ONE attack outstanding at a time.
//
// THE APPROACH WAIT IS THE ARRIVAL RULE, not a guess: a walk is simulated at
// MoveBaseSpeed = 469 units/s (zone\walkend_test.go:17-26), so 25 units is 53 ms
// and 1200 ms is far past the arrival rule plus its slack. Sending the attack
// before the server thinks we are in range would measure a refusal, not a hit.
func runAttack(h *hub, st *botState, cc net.Conn, done <-chan struct{}, d time.Duration) error {
	h.mu.Lock()
	target := h.monster
	mpos := h.monsterPos
	h.mu.Unlock()

	approach := [3]int32{mpos[0] - attackApproach, mpos[1], mpos[2]}
	if err := st.sendMove(h, cc, approach); err != nil {
		return fmt.Errorf("send approach CZ_MOVE_PC: %w", err)
	}
	if err := waitPeriod(st, h, done, attackWalkWait); err != nil {
		return err
	}

	deadline := time.Now().Add(d)
	for {
		if !time.Now().Before(deadline) {
			return nil
		}
		sent := time.Now()
		if err := st.sendAttack(h, cc, target); err != nil {
			return fmt.Errorf("send CZ_ATTACK: %w", err)
		}

		timer := time.NewTimer(attackReplyWait)
		select {
		case <-st.rttCh:
			timer.Stop()
		case <-timer.C:
			// A failed exchange is COUNTED, not fatal: a busy zone dropping one
			// reply says something about the load, not about this bot. A reply
			// that turns up after the give-up is ignored only until the next
			// attack is sent, which is the one window this rule cannot close.
			h.mu.Lock()
			st.rttFailed++
			st.attackPending = false
			h.mu.Unlock()
		case <-done:
			timer.Stop()
			return readErrOrEOF(h, st)
		}

		if rest := attackGap - time.Since(sent); rest > 0 {
			if err := waitPeriod(st, h, done, rest); err != nil {
				return err
			}
		}
	}
}

// waitPeriod sleeps for d, but gives up early when the connection ends, so a
// bot whose zone died does not sit out the rest of its duration first.
func waitPeriod(st *botState, h *hub, done <-chan struct{}, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-done:
		return readErrOrEOF(h, st)
	}
}

// readErrOrEOF returns the reader's error, or a bare EOF when it has none (a
// reader that stopped for any other reason still means the connection is gone).
func readErrOrEOF(h *hub, st *botState) error {
	if err := h.readErrOf(st); err != nil {
		return err
	}
	return io.EOF
}

func (h *hub) readErrOf(st *botState) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return st.readErr
}

// addSpawn records one 3520: the handle -> nameplate map, and - the first time
// only - the loop centre. The centre is the SENDER's position, which every bot
// on the map shares; -center overrides it by setting centerSet up front.
func (h *hub) addSpawn(handle uint32, pos [3]int32, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.roster[handle]; !ok {
		h.roster[handle] = name
	}
	if !h.centerSet {
		h.centerSet = true
		h.center = pos
	}
}

// setMonster records the first 3527 only: a later one on the same connection
// would be another monster, and the attack pattern follows one target.
func (h *hub) setMonster(handle uint32, pos [3]int32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.monsterSet {
		return
	}
	h.monsterSet = true
	h.monster = handle
	h.monsterPos = pos
}

// recordEcho is one 3548 seen by this bot: a move fanned out to it, from another
// bot OR from itself (the zone echoes the mover's own report back, zone\zone.go:1501),
// so a from == to pair is real and its number is the bot's own move RTT. The clock
// is shared - one process, one machine - so there is no skew correction to do.
//
// WHO SENT IT comes from the handle at +2 through the roster, which is why the
// roster is learned from every 3520 before a sample is trusted. The moment it was
// sent comes from that sender's own lastSend entry.
//
// An echo whose handle is not in the roster yet, or whose sender has no move on
// record, is COUNTED in unattributed, never silently dropped: those are the
// samples a crowded town would otherwise hide.
func (h *hub) recordEcho(recvAt time.Time, handle uint32, me *botState) {
	h.mu.Lock()
	defer h.mu.Unlock()

	me.fanoutRecv++
	from, known := h.roster[handle]
	if !known {
		h.unattributed++
		return
	}
	at, ok := h.lastSend[from]
	if !ok {
		h.unattributed++
		return
	}
	ms := float64(recvAt.Sub(at)) / float64(time.Millisecond)
	if ms < 0 {
		ms = 0 // one clock, so a negative means the echo preceded the write: clamp, do not lie
	}
	h.fanoutAll = append(h.fanoutAll, ms)
	k := pairKey{from: from, to: me.login}
	h.pairs[k] = append(h.pairs[k], ms)
}

// countingConn counts the bytes that cross the socket, in both directions. It
// counts what the WIRE carries: a Write receives what EncryptChunk produced, so
// the cipher header and the block padding are included - that is the cost the
// zone actually pays, which is the number a load test needs.
type countingConn struct {
	net.Conn
	bytesOut int64
	bytesIn  int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	atomic.AddInt64(&c.bytesOut, int64(n))
	return n, err
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	atomic.AddInt64(&c.bytesIn, int64(n))
	return n, err
}

// botReader is one bot's read side. It runs for the whole life of the
// connection and does three things: cut the plaintext stream with the zone's
// own command table, timestamp each frame AS IT IS CUT, and hand the interesting
// ones to the shared state.
type botReader struct {
	conn  net.Conn
	table protocol.Table
	h     *hub
	st    *botState
	buf   []byte

	startOK chan struct{}
	entered chan struct{}
	monster chan struct{}
}

func (r *botReader) run() {
	for {
		// Everything the splitter cut is KEPT, not just the first frame: one
		// read often carries several packets, and dropping the rest would lose
		// the very packet being waited for.
		if frames, used := r.table.Split(r.buf); len(frames) > 0 {
			r.buf = append(r.buf[:0], r.buf[used:]...)
			for _, f := range frames {
				r.dispatch(f, time.Now())
			}
			continue
		}
		chunk := make([]byte, 32*1024)
		n, err := r.conn.Read(chunk)
		if n > 0 {
			r.buf = append(r.buf, chunk[:n]...)
		}
		if err != nil {
			r.stop(err)
			return
		}
		if n == 0 {
			r.stop(io.EOF)
			return
		}
	}
}

// stop records the FIRST read error. Errors caused by our own Close are not
// failures, which is what stopping is for: it is set before the socket closes.
func (r *botReader) stop(err error) {
	if r.st.stopping.Load() {
		return
	}
	r.h.mu.Lock()
	if r.st.readErr == nil {
		r.st.readErr = err
	}
	r.h.mu.Unlock()
}

func (r *botReader) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *botReader) dispatch(f protocol.Frame, recvAt time.Time) {
	// A frame is a packet whatever it is: count it here, where Split handed it
	// over, not at the interesting cases below.
	r.h.mu.Lock()
	r.st.packetsIn++
	r.h.mu.Unlock()

	switch f.Cmd {
	case zone.CmdZCStartGameOK:
		r.signal(r.startOK)
	case zone.CmdZCEnterLegacyObjs:
		r.signal(r.entered)
	case zone.CmdZCSpawnStandingPC:
		r.spawn(f)
	case zone.CmdZCEnterStandingMonster:
		r.monsterSpawn(f)
	case zone.CmdZCMovePC:
		r.moveEcho(f, recvAt)
	case zone.CmdZCAttackSingle, zone.CmdZCAttackDisable:
		r.attackReply(recvAt)
	}
}

// spawn is one ZC_SPAWN_STANDINGPC. The size is variable and Split already used
// the length word at +2, so f.Data is the whole packet.
func (r *botReader) spawn(f protocol.Frame) {
	if len(f.Data) < spawnNameAt+spawnNameLen {
		return
	}
	handle := binary.LittleEndian.Uint32(f.Data[spawnHandleAt:])
	pos := [3]int32{
		int32(binary.LittleEndian.Uint32(f.Data[spawnPosAt:])),
		int32(binary.LittleEndian.Uint32(f.Data[spawnPosAt+4:])),
		int32(binary.LittleEndian.Uint32(f.Data[spawnPosAt+8:])),
	}
	name := cstr(f.Data[spawnNameAt : spawnNameAt+spawnNameLen])
	r.h.addSpawn(handle, pos, name)
}

// monsterSpawn is one ZC_ENTER_STANDINGMONSTER, 360 bytes, fixed.
func (r *botReader) monsterSpawn(f protocol.Frame) {
	if len(f.Data) < monsterPosAt+12 {
		return
	}
	handle := binary.LittleEndian.Uint32(f.Data[monsterHandleAt:])
	pos := [3]int32{
		int32(binary.LittleEndian.Uint32(f.Data[monsterPosAt:])),
		int32(binary.LittleEndian.Uint32(f.Data[monsterPosAt+4:])),
		int32(binary.LittleEndian.Uint32(f.Data[monsterPosAt+8:])),
	}
	r.h.setMonster(handle, pos)
	r.signal(r.monster)
}

// moveEcho is one ZC_MOVE_PC: a move fanned out to this bot, which is either
// someone else's (sendOthers) or this bot's OWN report echoed back to it - the
// zone answers a CZ_MOVE_PC by echoing it to the mover too (zone\zone.go:1501).
// The sender is named by the handle at +2, which the roster turns into the login
// whose send time this sample is measured against, so a from == to sample is the
// bot's own move RTT.
func (r *botReader) moveEcho(f protocol.Frame, recvAt time.Time) {
	if len(f.Data) < echoHandleAt+4 {
		return
	}
	handle := binary.LittleEndian.Uint32(f.Data[echoHandleAt:])
	r.h.recordEcho(recvAt, handle, r.st)
}

// attackReply is the end of THIS bot's own attack round trip: request -> RESULT.
// A move's self echo is transport only (it carries no outcome), which is why the
// attack is the pattern that measures an application-level RTT.
func (r *botReader) attackReply(recvAt time.Time) {
	r.h.mu.Lock()
	if !r.st.attackPending {
		r.h.mu.Unlock()
		return
	}
	d := recvAt.Sub(r.st.attackSentAt)
	r.st.attackPending = false
	r.st.rttN++
	r.h.rttAll = append(r.h.rttAll, float64(d)/float64(time.Millisecond))
	r.h.mu.Unlock()

	select {
	case r.st.rttCh <- d:
	default:
	}
}

// cstr reads a NUL-padded fixed-size string, which is how the nameplate is
// written (zone\entry.go, putCStr).
func cstr(b []byte) string {
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	return string(b[:n])
}

// latency is a nearest-rank summary in milliseconds. n == 0 means "no samples",
// and every percentile is 0 - never NaN.
type latency struct {
	n             int
	p50, p95, p99 float64
	max           float64
}

func summarize(samples []float64) latency {
	n := len(samples)
	if n == 0 {
		return latency{}
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	return latency{
		n:   n,
		p50: nearestRank(sorted, 0.50),
		p95: nearestRank(sorted, 0.95),
		p99: nearestRank(sorted, 0.99),
		max: sorted[n-1],
	}
}

// nearestRank is the nearest-rank percentile: rank = ceil(p*n), 1-based.
func nearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	rank := int(math.Ceil(p*float64(n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}

// ---- report ---------------------------------------------------------------

type reportJSON struct {
	StartedUTC    string        `json:"started_utc"`
	Zone          string        `json:"zone"`
	Pattern       string        `json:"pattern"`
	Spread        int           `json:"spread"`
	Bots          int           `json:"bots"`
	RampPerS      int           `json:"ramp_per_s"`
	DurationS     float64       `json:"duration_s"`
	Entered       int           `json:"entered"`
	Failed        int           `json:"failed"`
	PerBot        []botJSON     `json:"per_bot"`
	FanoutLatency fanoutJSON    `json:"fanout_latency_ms"`
	FanoutByPair  []pairJSON    `json:"fanout_by_pair"`
	SelfRTT       rttJSON       `json:"self_rtt_ms"`
	Failures      []failureJSON `json:"failures"`
}

type botJSON struct {
	Login          string  `json:"login"`
	BytesIn        int64   `json:"bytes_in"`
	BytesOut       int64   `json:"bytes_out"`
	PacketsIn      int64   `json:"packets_in"`
	PacketsOut     int64   `json:"packets_out"`
	BytesInPerS    float64 `json:"bytes_in_per_s"`
	BytesOutPerS   float64 `json:"bytes_out_per_s"`
	PacketsInPerS  float64 `json:"packets_in_per_s"`
	PacketsOutPerS float64 `json:"packets_out_per_s"`
	FanoutReceived int     `json:"fanout_received"`
	SelfRTTCount   int     `json:"self_rtt_count"`
	Err            string  `json:"err"`
}

type fanoutJSON struct {
	Count        int     `json:"count"`
	P50          float64 `json:"p50"`
	P95          float64 `json:"p95"`
	P99          float64 `json:"p99"`
	Max          float64 `json:"max"`
	Unattributed int     `json:"unattributed"`
}

type pairJSON struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

type rttJSON struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

type failureJSON struct {
	Login  string `json:"login"`
	Reason string `json:"reason"`
}

// printRoster writes the handle -> nameplate map every bot built, sorted by
// handle so two runs of the same size diff cleanly.
func printRoster(h *hub) {
	h.mu.Lock()
	handles := make([]uint32, 0, len(h.roster))
	for handle := range h.roster {
		handles = append(handles, handle)
	}
	sort.Slice(handles, func(i, j int) bool { return handles[i] < handles[j] })
	for _, handle := range handles {
		fmt.Printf("roster: handle %d = %s\n", handle, h.roster[handle])
	}
	h.mu.Unlock()
}

// printBots writes the per-bot table. The err column is flattened to one line
// per bot on purpose: a tab or a newline inside an error would break the table.
func printBots(results []botResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "login\tbytes_out\tpkt_out\tB/s(IO)\tbytes_in\tpkt_in\tB/s(IO)\tfanout_recv\trtt_n\terr")
	for _, r := range results {
		secs := r.elapsed.Seconds()
		errText := ""
		if r.err != nil {
			errText = strings.NewReplacer("\t", " ", "\n", " ").Replace(r.err.Error())
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%.1f\t%d\t%d\t%.1f\t%d\t%d\t%s\n",
			r.login, r.bytesOut, r.packetsOut, perSecond(r.bytesOut, secs),
			r.bytesIn, r.packetsIn, perSecond(r.bytesIn, secs),
			r.fanoutRecv, r.rttN, errText)
	}
	w.Flush()
}

// perSecond is one counter over the bot's own measured elapsed time. The
// denominator is zero for a bot that never got a connection, and a rate over
// no time is 0, not an infinity.
func perSecond(n int64, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return float64(n) / secs
}

func buildReport(started time.Time, zoneAddr, pattern string, bots, ramp int, duration time.Duration,
	entered, failed int, results []botResult, h *hub, failures []failureJSON, spread int) reportJSON {

	rep := reportJSON{
		StartedUTC:   started.UTC().Format(time.RFC3339),
		Zone:         zoneAddr,
		Pattern:      pattern,
		Spread:       spread,
		Bots:         bots,
		RampPerS:     ramp,
		DurationS:    duration.Seconds(),
		Entered:      entered,
		Failed:       failed,
		PerBot:       make([]botJSON, 0, len(results)),
		FanoutByPair: make([]pairJSON, 0),
		Failures:     failures,
	}

	for _, r := range results {
		secs := r.elapsed.Seconds()
		errText := ""
		if r.err != nil {
			errText = r.err.Error()
		}
		rep.PerBot = append(rep.PerBot, botJSON{
			Login:          r.login,
			BytesIn:        r.bytesIn,
			BytesOut:       r.bytesOut,
			PacketsIn:      r.packetsIn,
			PacketsOut:     r.packetsOut,
			BytesInPerS:    perSecond(r.bytesIn, secs),
			BytesOutPerS:   perSecond(r.bytesOut, secs),
			PacketsInPerS:  perSecond(r.packetsIn, secs),
			PacketsOutPerS: perSecond(r.packetsOut, secs),
			FanoutReceived: r.fanoutRecv,
			SelfRTTCount:   r.rttN,
			Err:            errText,
		})
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	all := summarize(h.fanoutAll)
	rep.FanoutLatency = fanoutJSON{
		Count: all.n, P50: all.p50, P95: all.p95, P99: all.p99, Max: all.max,
		Unattributed: h.unattributed,
	}

	keys := make([]pairKey, 0, len(h.pairs))
	for k := range h.pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		return keys[i].to < keys[j].to
	})
	for _, k := range keys {
		s := summarize(h.pairs[k])
		rep.FanoutByPair = append(rep.FanoutByPair, pairJSON{
			From: k.from, To: k.to, Count: s.n,
			P50: s.p50, P95: s.p95, P99: s.p99, Max: s.max,
		})
	}

	rtt := summarize(h.rttAll)
	rep.SelfRTT = rttJSON{Count: rtt.n, P50: rtt.p50, P95: rtt.p95, P99: rtt.p99, Max: rtt.max}
	return rep
}

// writeReport writes out/<UTC stamp>.json, 2-space indented.
func writeReport(dir string, rep reportJSON) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(dir, stamp+".json")
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode report: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// readAccounts reads account_id,commander_id,login and keeps the two it uses.
func readAccounts(path string) ([]bot, error) {
	// Run from goserver\, the CSV sits one level up; running from docker-port\,
	// the same relative path is underfoot. Try both rather than make the caller
	// remember which directory this program is started from.
	tryPaths := []string{path}
	if !strings.HasPrefix(path, "../") && !strings.HasPrefix(path, `..\`) {
		tryPaths = append(tryPaths, "../"+path)
	}

	var lastErr error
	for _, p := range tryPaths {
		f, err := os.Open(p)
		if err != nil {
			lastErr = err
			continue
		}
		defer f.Close()
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(rows) < 2 {
			return nil, fmt.Errorf("%s: no account rows (header + at least one bot)", p)
		}
		out := make([]bot, 0, len(rows)-1)
		for n, row := range rows[1:] {
			if len(row) < 3 {
				return nil, fmt.Errorf("%s line %d: want account_id,commander_id,login", p, n+2)
			}
			id, err := strconv.ParseUint(strings.TrimSpace(row[0]), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%s line %d: account id %q: %w", p, n+2, row[0], err)
			}
			out = append(out, bot{account: uint32(id), login: strings.TrimSpace(row[2])})
		}
		return out, nil
	}
	return nil, fmt.Errorf("cannot open accounts CSV: %v", lastErr)
}
