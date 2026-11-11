// ONE LIVE SESSION PER ACCOUNT AND PER CHARACTER (card t_487ea31c).
//
// WHY. The zone's roster is keyed by *Session (zone\players.go), so two logins on one account each
// joined and spawned the SAME character handles: two bodies for one character, and when the first
// socket closed it sent ZC_LEAVE_PC for bodies the second still owned - the survivor's own client
// then drew a character that the server no longer told anybody about. The reference keeps ONE live
// connection per account AND per character and closes the older one (offlinespada
// docs\kb\ge-server.md:63-65, accounts-security.md:32; PROVEN on the stock binaries).
//
// THE CLAIM IS THE TRUTH, NOT A POLICY: entering takes the account and every handle the team holds;
// whoever held them before is DISPLACED, and the caller closes that socket. A displaced session is
// the one thing this file reports, so the log line (what happened, to whom) is written where the
// close happens.
//
// WHAT IT IS NOT: it does not kick a player for a bad password, does not touch the barrack's handoff
// (that is zone\entry_consume.go), and does not persist anything - this is the RUNNING zone's own
// knowledge, and it dies with the process.
package zone

import "sync"

// liveSessionMu guards the two maps below.
var liveSessionMu sync.Mutex

// liveSession is the claim one session holds: its account and its characters.
type liveSession struct {
	account uint32
	chars   []uint32
}

// registerLiveSession claims this session's account and handles, and answers the sessions that held
// them before - each of them must be CLOSED by the caller (their socket is what ends them; the zone
// holds no "kick" packet for this and the reference does not send one either).
//
// A session that displaces ITSELF (a re-entry on the same object) is not answered: the claim is
// replaced in place.
func (h *Handler) registerLiveSession(sess *Session) []*Session {
	if h == nil || sess == nil || sess.AccountID == 0 {
		return nil
	}
	chars := make([]uint32, 0, len(sess.Team))
	for _, c := range sess.Team {
		if c.Handle != 0 {
			chars = append(chars, c.Handle)
		}
	}

	liveSessionMu.Lock()
	defer liveSessionMu.Unlock()
	if h.liveAccounts == nil {
		h.liveAccounts = make(map[uint32]*Session, 8)
	}
	if h.liveChars == nil {
		h.liveChars = make(map[uint32]*Session, 32)
	}

	seen := make(map[*Session]bool, 4)
	var displaced []*Session
	take := func(old *Session) {
		if old == nil || old == sess || seen[old] {
			return
		}
		seen[old] = true
		displaced = append(displaced, old)
	}
	take(h.liveAccounts[sess.AccountID])
	for _, handle := range chars {
		take(h.liveChars[handle])
	}

	h.liveAccounts[sess.AccountID] = sess
	for _, handle := range chars {
		h.liveChars[handle] = sess
	}
	sess.liveChars = chars
	return displaced
}

// forgetLiveSession drops this session's claim - called when its connection ends. It removes only
// what still POINTS AT THIS SESSION, so a newer login's claim is never cleared by the older session's
// socket finally closing (which is exactly what happens after a takeover).
func (h *Handler) forgetLiveSession(sess *Session) {
	if h == nil || sess == nil || sess.AccountID == 0 {
		return
	}
	liveSessionMu.Lock()
	defer liveSessionMu.Unlock()
	if h.liveAccounts[sess.AccountID] == sess {
		delete(h.liveAccounts, sess.AccountID)
	}
	for _, handle := range sess.liveChars {
		if h.liveChars[handle] == sess {
			delete(h.liveChars, handle)
		}
	}
	sess.liveChars = nil
}

// LiveSessionOf answers which session currently holds one character handle, and false when nobody
// does - a diagnostic (and a test) sees the same claim the zone acts on.
func (h *Handler) LiveSessionOf(handle uint32) (*Session, bool) {
	liveSessionMu.Lock()
	defer liveSessionMu.Unlock()
	s, ok := h.liveChars[handle]
	return s, ok
}

// LiveAccountOf answers which session currently holds an account.
func (h *Handler) LiveAccountOf(accountID uint32) (*Session, bool) {
	liveSessionMu.Lock()
	defer liveSessionMu.Unlock()
	s, ok := h.liveAccounts[accountID]
	return s, ok
}

// endDisplaced closes every session a new entry displaced, with ONE log line naming the account and
// both sets of handles. The close is the socket's Close: the displaced Serve loop notices, runs its
// own cleanup (last point, writer stop, leaveZone) and forgets a claim that is no longer its own.
func (h *Handler) endDisplaced(sess *Session, displaced []*Session) {
	for _, old := range displaced {
		oldHandles := old.liveChars
		if len(oldHandles) == 0 {
			for _, c := range old.Team {
				oldHandles = append(oldHandles, c.Handle)
			}
		}
		h.Log.Printf("zone: SECOND LOGIN - account %d entered on a new connection with "+
			"handle(s) %v; closing the older session that held handle(s) %v (one live "+
			"connection per account and per character)", sess.AccountID, sess.liveChars, oldHandles)
		if old.Conn != nil {
			_ = old.Conn.Close()
		}
	}
}
