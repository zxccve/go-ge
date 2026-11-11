// ONE LIVE SESSION PER ACCOUNT AND PER CHARACTER (card t_487ea31c).
package zone

import (
	"io"
	"log"
	"net"
	"testing"
	"time"
)

func liveHandler() *Handler {
	return &Handler{Log: log.New(io.Discard, "", 0)}
}

func sessionWith(account uint32, handles ...uint32) *Session {
	s := &Session{AccountID: account}
	for _, h := range handles {
		s.Team = append(s.Team, Character{Handle: h})
	}
	return s
}

// TestASecondLoginClosesTheOlderSession is the card's pass condition: two sessions on one account,
// the second entry displaces the first and CLOSES ITS SOCKET, and the zone keeps one claim per
// character. RED without the account map: the second register answers nothing and both sessions
// keep their handles, which is the bug (two bodies, one character).
func TestASecondLoginClosesTheOlderSession(t *testing.T) {
	h := liveHandler()
	first := sessionWith(24, 0x40000001, 0x40000002)
	second := sessionWith(24, 0x40000001, 0x40000002)

	if d := h.registerLiveSession(first); len(d) != 0 {
		t.Fatalf("the first entry displaced %d session(s), want 0", len(d))
	}

	// A real socket pair, so the close can be SEEN by the other end.
	oldClient, oldServer := net.Pipe()
	defer oldClient.Close()
	first.Conn = oldServer

	displaced := h.registerLiveSession(second)
	if len(displaced) != 1 || displaced[0] != first {
		t.Fatalf("the second entry displaced %v, want the first session", displaced)
	}
	h.endDisplaced(second, displaced)

	if s, ok := h.LiveAccountOf(24); !ok || s != second {
		t.Fatal("the account is not claimed by the newer session")
	}
	if s, ok := h.LiveSessionOf(0x40000001); !ok || s != second {
		t.Fatal("handle 0x40000001 is not claimed by the newer session")
	}
	if s, ok := h.LiveSessionOf(0x40000002); !ok || s != second {
		t.Fatal("handle 0x40000002 is not claimed by the newer session")
	}

	// THE OLD SOCKET IS CLOSED, which is what ends the old Serve loop.
	_ = oldClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := oldClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("the older session's socket is still open after the takeover")
	}

	// AND THE OLDER SESSION'S OWN CLEANUP MUST NOT CLEAR THE NEWER CLAIM: its Serve loop runs
	// forgetLiveSession on the way out, moments after the takeover.
	h.forgetLiveSession(first)
	if s, ok := h.LiveAccountOf(24); !ok || s != second {
		t.Fatal("the older session's close cleared the newer session's claim")
	}
	if s, ok := h.LiveSessionOf(0x40000001); !ok || s != second {
		t.Fatal("the older session's close cleared a handle the newer session holds")
	}
}

func TestDifferentAccountsDoNotDisplaceEachOther(t *testing.T) {
	h := liveHandler()
	a := sessionWith(24, 0x40000001)
	b := sessionWith(25, 0x40000002)
	h.registerLiveSession(a)
	if d := h.registerLiveSession(b); len(d) != 0 {
		t.Fatalf("a different account displaced %d session(s), want 0", len(d))
	}
}

func TestASessionThatReentersDoesNotCloseItself(t *testing.T) {
	h := liveHandler()
	s := sessionWith(24, 0x40000001)
	h.registerLiveSession(s)
	if d := h.registerLiveSession(s); len(d) != 0 {
		t.Fatalf("a re-entry on the same session displaced %d session(s), want 0", len(d))
	}
}

func TestForgettingTheLastSessionLeavesNothing(t *testing.T) {
	h := liveHandler()
	s := sessionWith(24, 0x40000001)
	h.registerLiveSession(s)
	h.forgetLiveSession(s)
	if _, ok := h.LiveAccountOf(24); ok {
		t.Fatal("the account is still claimed after the only session left")
	}
	if _, ok := h.LiveSessionOf(0x40000001); ok {
		t.Fatal("the handle is still claimed after the only session left")
	}
}
