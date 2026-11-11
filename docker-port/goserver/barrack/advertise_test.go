package barrack

// WHY THIS TEST EXISTS. The barrack's answer to a map it cannot find used to be
// `const DefaultZoneAddr = "127.0.0.1:7002"`, so a player on a second PC was sent
// to his own loopback and could not connect - silently. The ip now comes from the
// address book (Handler.AdvertisedIP, resolved once at startup by
// address.Resolve), and this test holds cmd 25 to it through the real handler.
//
// The PORT half of that answer is deliberately NOT configurable: a town reports the
// port its listener bound, and DefaultZonePort is only the fallback for a map
// nothing announced.

import (
	"context"
	"encoding/binary"
	"testing"

	"geserver/protocol"
	"geserver/world"
)

// startGameAnswer runs one CB_START_GAME for commander 24 and cuts the ip and the
// port out of the answer the client would receive.
func startGameAnswer(t *testing.T, h *Handler) (string, uint32) {
	t.Helper()
	frame := startGameFrame(22, 1, [3]uint16{0, 3, EmptyCharSlot})
	out, err := h.Reply(context.Background(), &Session{CommanderID: 24},
		protocol.Frame{Cmd: 22, Size: len(frame), Data: frame})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sent %d packet(s), want 1", len(out))
	}
	return trimNul(string(out[0][6:22])), binary.LittleEndian.Uint32(out[0][22:])
}

func TestBarrackFallbackAddress(t *testing.T) {
	// A barrack with a readable registry that holds no zone, so the fallback is
	// the branch under test.
	barrackFor := func(t *testing.T, advertised, zoneAddr string) *Handler {
		t.Helper()
		h := quietHandler(t, fakeStore{ok: true})
		h.Zones = &world.Registry{Dir: t.TempDir()} // readable, but no zone inside
		h.AdvertisedIP = advertised
		h.ZoneAddr = zoneAddr
		h.Barrack = fakeBarrack{found: true,
			cmdr: CommanderRow{ID: 24, ZoneID: 10, ChannelID: 0}}
		return h
	}

	t.Run("no live zone: the advertised ip is what the client is told", func(t *testing.T) {
		ip, port := startGameAnswer(t, barrackFor(t, "192.0.2.7", ""))
		if ip != "192.0.2.7" {
			t.Fatalf("zoneIP = %q, want the advertised 192.0.2.7", ip)
		}
		if port != DefaultZonePort {
			t.Fatalf("zonePort = %d, want the fallback port %d", port, DefaultZonePort)
		}
	})

	t.Run("-zone still overrides the whole address", func(t *testing.T) {
		ip, port := startGameAnswer(t, barrackFor(t, "192.0.2.7", "10.0.0.5:7003"))
		if ip != "10.0.0.5" || port != 7003 {
			t.Fatalf("answered %s:%d, want the -zone value 10.0.0.5:7003", ip, port)
		}
	})

	t.Run("nothing configured keeps the documented default", func(t *testing.T) {
		ip, port := startGameAnswer(t, barrackFor(t, "", ""))
		if ip != "127.0.0.1" || port != DefaultZonePort {
			t.Fatalf("answered %s:%d, want the default 127.0.0.1:%d",
				ip, port, DefaultZonePort)
		}
	})

	t.Run("a live zone's own announcement beats all of it", func(t *testing.T) {
		h := quietHandler(t, fakeStore{ok: true})
		dir := t.TempDir()
		announceZone(t, dir, 10, 7009, "203.0.113.4", "reb_basic")
		h.Zones = &world.Registry{Dir: dir, FallbackIP: "192.0.2.7"}
		h.AdvertisedIP = "192.0.2.7"
		h.Barrack = fakeBarrack{found: true,
			cmdr: CommanderRow{ID: 24, ZoneID: 10, ChannelID: 0}}

		ip, port := startGameAnswer(t, h)
		if ip != "203.0.113.4" || port != 7009 {
			t.Fatalf("answered %s:%d, want the announced 203.0.113.4:7009", ip, port)
		}
	})
}
