package zone

// THE `-respawn` DEVELOPMENT FLAG - one test, both ends of it.
//
// There is no zone-wide fallback any more (zone/respawn.go): a body with no spawner row
// and no flag DOES NOT COME BACK, and the 23.568 s that used to stand in for one is
// deleted. What is left to pin here is the flag: main.go's `-respawn <duration>` replaces
// the return interval for EVERY body in the run, so a probe can be followed through its
// corpse and its return without waiting on a map's own `RespawnTime` (5 s here, and up to
// 1,800,000 ms in the shipped tables). The two ends are 2 s under the flag and the
// fixture's own spawner row without it, and the return fires at the armed instant and not
// a millisecond before.
//
// THE TEST SETS THE PACKAGE GLOBAL ITSELF (SetRespawnDelay), which is the same call
// main.go makes at startup, and puts it back to 0 in a cleanup - the flag's own contract
// is that 0 means "no override".

import (
	"strings"
	"testing"
	"time"
)

// TestTheRespawnOverrideReplacesTheSpawnerRowsDelay arms a kill with and without the
// override and reads the corpse's own clock: `backAt` is the instant the return is owed,
// so no test sleeps and no wall clock is involved.
func TestTheRespawnOverrideReplacesTheSpawnerRowsDelay(t *testing.T) {
	cases := []struct {
		name     string
		override time.Duration
		want     time.Duration
		wantLog  string
	}{
		{
			name:     "-respawn 2s: the body is back two seconds after the kill",
			override: 2 * time.Second,
			want:     2 * time.Second,
			wantLog: "(the -respawn DEV FLAG's number, " +
				"NOT this body's spawner row)",
		},
		{
			name:     "no override: the fixture's spawner row, 5s",
			override: 0,
			want:     fixtureSpawnerDelay,
			wantLog:  "this body's SPAWNER row (mongentype RespawnTime 5s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetRespawnDelay(tc.override)
			t.Cleanup(func() { SetRespawnDelay(0) })

			w, p, mon := spawnedZone(t)
			at := killAndDead(t, w.Handler, mon, p.sess.Team[0].Handle)
			c := corpseFor(t, w.Handler, mon.Handle)
			if got := c.backAt.Sub(at); got != tc.want {
				t.Fatalf("the body's return is owed %s after the kill at %s, want %s "+
					"(override %s, the fixture's spawner row %s)", got, at, tc.want,
					tc.override, fixtureSpawnerDelay)
			}

			// NOT ONE MILLISECOND EARLY, and exactly at the armed instant: the tick
			// takes `now` as an argument, so both instants are stood on for free.
			p.conn.writes = nil
			w.corpseTick(c.backAt.Add(-time.Millisecond))
			if n := len(enterFrames(p.conn.sent())); n != 0 {
				t.Fatalf("%d 3527(s) went out one millisecond before the return was "+
					"due, want 0:\n%s", n, w.logText())
			}
			// THE RETURN CANNOT PRECEDE THE LEAVE (takeDueRespawns refuses a corpse that
			// has not left), so one tick at the LATER of the two instants runs the leave
			// and the return in order - and an override at or below the 2 s attack
			// delay comes back on the leave's own pass.
			due := c.backAt
			if c.leaveAt.After(due) {
				due = c.leaveAt
			}
			w.corpseTick(due)
			if n := len(enterFrames(p.conn.sent())); n != 1 {
				t.Fatalf("the socket has %d 3527(s) at the return, want exactly 1:\n%s",
					n, w.logText())
			}

			// THE LEAVE LINE NAMES THE INTERVAL IN FORCE. Without that a log reader
			// cannot tell a probe from a timer that came back far too early.
			//
			// THE SOURCE IS WHAT IS PINNED HERE, NOT THE LEADING NUMBER: which clock the
			// line names is the claim, and the interval itself is already pinned above by
			// `backAt.Sub(kill)`. (The "the body comes back in X" prefix is measured from
			// the corpse's ZC_DEAD instant today - respawn.go respawnReturnTextFor(c,
			// c.deadAt) - so it reads short by DeathPacketLead. That is a production log
			// arithmetic defect reported with this change; a test must not freeze it.)
			if text := w.logText(); !strings.Contains(text, tc.wantLog) {
				t.Fatalf("the CORPSE LEAVE line does not say %q:\n%s", tc.wantLog, text)
			}

			// AND THE BODY THAT CAME BACK TAKES THE SAME INTERVAL: the flag is the
			// zone's number for the run, not a one-shot discount on the first kill.
			made := w.runtimeMonsters()
			if len(made) != 1 {
				t.Fatalf("%d monster(s) in the roster after the return, want 1", len(made))
			}
			at2 := killAndDead(t, w.Handler, made[0], p.sess.Team[0].Handle)
			if got := corpseFor(t, w.Handler, made[0].Handle).backAt.Sub(at2); got != tc.want {
				t.Fatalf("the returned body's own return is owed %s after ITS kill, "+
					"want %s - the flag must not stop at the first death", got, tc.want)
			}
		})
	}
}
